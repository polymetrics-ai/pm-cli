package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maximumServerExampleBytes  = 1 << 20
	maximumDynamicConfigBytes  = 1 << 20
	rolloverNamespaceSuffix    = "-rollover"
	rolloverHistoryEventTarget = 64
)

var projectNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{2,62}$`)

var certificateDNSNames = []string{
	"localhost",
	"postgres",
	"temporal",
	"temporal.pm-stack.internal",
	"seaweed-master",
	"seaweed-volume",
	"seaweed-filer",
	"seaweed-s3-a",
	"seaweed-s3-b",
	"seaweed-s3.pm-stack.internal",
}

var portNames = []string{
	"POSTGRES",
	"TEMPORAL",
	"S3_PRIMARY",
	"S3_SECONDARY",
	"PUBLIC",
}

type fixtureOptions struct {
	root           string
	project        string
	repositoryRoot string
}

type portReservation struct {
	listeners []net.Listener
	ports     map[string]string
}

type certificateBundle map[string][]byte

type privateFile struct {
	relativePath string
	contents     []byte
}

type fixtureMaterial struct {
	files       []privateFile
	environment []byte
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("pm-stack-fixture", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: pm-stack-fixture --root ABSENT_ABSOLUTE_PATH --project UNIQUE_SAFE_PROJECT --repo-root ABSOLUTE_CHECKOUT")
		fmt.Fprintln(stderr, "Create one private, entirely synthetic PostgreSQL, Temporal, and artifact fixture for the selected stack checks.")
		flags.PrintDefaults()
	}
	root := flags.String("root", "", "new private fixture root; the path must not exist or traverse symlinks")
	project := flags.String("project", "", "unique lowercase Compose project identity")
	repositoryRoot := flags.String("repo-root", "", "absolute pm-cli checkout containing deploy/server.example.json")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "pm-stack-fixture: unexpected positional arguments")
		return 2
	}
	if *root == "" || *project == "" || *repositoryRoot == "" {
		fmt.Fprintln(stderr, "pm-stack-fixture: --root, --project, and --repo-root are required")
		flags.Usage()
		return 2
	}

	options := fixtureOptions{
		root:           *root,
		project:        *project,
		repositoryRoot: *repositoryRoot,
	}
	if err := options.validate(); err != nil {
		fmt.Fprintf(stderr, "pm-stack-fixture: %v\n", err)
		return 2
	}
	if err := generateFixture(options, stdout); err != nil {
		fmt.Fprintf(stderr, "pm-stack-fixture: %v\n", err)
		return 1
	}
	return 0
}

func (options fixtureOptions) validate() error {
	if !filepath.IsAbs(options.root) {
		return errors.New("--root must be absolute")
	}
	if filepath.Clean(options.root) != options.root {
		return errors.New("--root must be lexically clean")
	}
	if !filepath.IsAbs(options.repositoryRoot) {
		return errors.New("--repo-root must be absolute")
	}
	if filepath.Clean(options.repositoryRoot) != options.repositoryRoot {
		return errors.New("--repo-root must be lexically clean")
	}
	if !projectNamePattern.MatchString(options.project) {
		return errors.New("--project must be a lowercase Compose identity of 3 to 63 safe characters")
	}
	if err := validateAbsentRoot(options.root); err != nil {
		return err
	}
	repositoryInfo, err := os.Lstat(options.repositoryRoot)
	if err != nil {
		return fmt.Errorf("inspect --repo-root: %w", err)
	}
	if repositoryInfo.Mode()&os.ModeSymlink != 0 || !repositoryInfo.IsDir() {
		return errors.New("--repo-root must be a non-symlinked directory")
	}
	return nil
}

func validateAbsentRoot(root string) error {
	if err := validateDirectoryPathWithoutSymlinks(filepath.Dir(root)); err != nil {
		return fmt.Errorf("validate --root parent: %w", err)
	}
	if _, err := os.Lstat(root); err == nil {
		return errors.New("--root must not already exist")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect --root: %w", err)
	}
	return nil
}

func validateDirectoryPathWithoutSymlinks(path string) error {
	volume := filepath.VolumeName(path)
	remainder := strings.TrimPrefix(path, volume)
	separator := string(filepath.Separator)
	if !strings.HasPrefix(remainder, separator) {
		return errors.New("path is not absolute")
	}
	current := volume + separator
	remainder = strings.TrimPrefix(remainder, separator)
	if remainder == "" {
		return nil
	}
	for _, component := range strings.Split(remainder, separator) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect path component: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("path traverses a symbolic link")
		}
		if !info.IsDir() {
			return errors.New("path component is not a directory")
		}
	}
	return nil
}

func generateFixture(options fixtureOptions, stdout io.Writer) (err error) {
	example, err := readRepositoryRegularFile(
		options.repositoryRoot,
		filepath.Join("deploy", "server.example.json"),
		maximumServerExampleBytes,
	)
	if err != nil {
		return fmt.Errorf("read deploy/server.example.json: %w", err)
	}
	dynamicConfig, err := readRepositoryRegularFile(
		options.repositoryRoot,
		filepath.Join("deploy", "temporal", "dynamicconfig.yaml"),
		maximumDynamicConfigBytes,
	)
	if err != nil {
		return fmt.Errorf("read deploy/temporal/dynamicconfig.yaml: %w", err)
	}
	reservation, err := reservePorts()
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, reservation.Close())
	}()

	material, err := generateFixtureMaterial(options, example, dynamicConfig, reservation.ports, time.Now().UTC())
	if err != nil {
		return err
	}
	if err := persistFixture(options.root, material); err != nil {
		return err
	}
	if _, err := io.Copy(stdout, bytes.NewReader(material.environment)); err != nil {
		return fmt.Errorf("write safe fixture environment: %w", err)
	}
	return nil
}

func readRepositoryRegularFile(repositoryRoot, relativePath string, limit int64) (contents []byte, err error) {
	if !filepath.IsLocal(relativePath) || limit <= 0 {
		return nil, errors.New("repository file selection is invalid")
	}
	rootFD, err := unix.Open(
		repositoryRoot,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open repository root without following links: %w", err)
	}
	defer func() {
		err = errors.Join(err, unix.Close(rootFD))
	}()

	components := strings.Split(filepath.ToSlash(filepath.Clean(relativePath)), "/")
	parentFD := rootFD
	for _, component := range components[:len(components)-1] {
		nextFD, openErr := unix.Openat(
			parentFD,
			component,
			unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW,
			0,
		)
		if openErr != nil {
			if parentFD != rootFD {
				openErr = errors.Join(openErr, unix.Close(parentFD))
			}
			return nil, fmt.Errorf("walk repository path without following links: %w", openErr)
		}
		if parentFD != rootFD {
			if closeErr := unix.Close(parentFD); closeErr != nil {
				_ = unix.Close(nextFD)
				return nil, fmt.Errorf("close traversed repository directory: %w", closeErr)
			}
		}
		parentFD = nextFD
	}
	if parentFD != rootFD {
		defer func() {
			err = errors.Join(err, unix.Close(parentFD))
		}()
	}
	fileFD, err := unix.Openat(
		parentFD,
		components[len(components)-1],
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open regular repository file without following links: %w", err)
	}
	file := os.NewFile(uintptr(fileFD), relativePath)
	if file == nil {
		_ = unix.Close(fileFD)
		return nil, errors.New("adopt opened repository file descriptor")
	}
	defer func() {
		err = errors.Join(err, file.Close())
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened repository file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("repository input must be a regular file")
	}
	contents, err = io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read repository file: %w", err)
	}
	if int64(len(contents)) > limit {
		return nil, errors.New("repository input exceeds the fixture bound")
	}
	return contents, nil
}

func reservePorts() (*portReservation, error) {
	reservation := &portReservation{
		listeners: make([]net.Listener, 0, len(portNames)),
		ports:     make(map[string]string, len(portNames)),
	}
	seen := make(map[int]struct{}, len(portNames))
	for _, name := range portNames {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return nil, errors.Join(fmt.Errorf("reserve loopback fixture port: %w", err), reservation.Close())
		}
		reservation.listeners = append(reservation.listeners, listener)
		address, ok := listener.Addr().(*net.TCPAddr)
		if !ok || address.Port < 1024 || address.Port > 65535 {
			return nil, errors.Join(errors.New("system selected an invalid fixture port"), reservation.Close())
		}
		if _, duplicate := seen[address.Port]; duplicate {
			return nil, errors.Join(errors.New("system selected a duplicate fixture port"), reservation.Close())
		}
		seen[address.Port] = struct{}{}
		reservation.ports[name] = strconv.Itoa(address.Port)
	}
	return reservation, nil
}

func (reservation *portReservation) Close() error {
	var result error
	for _, listener := range reservation.listeners {
		result = errors.Join(result, listener.Close())
	}
	reservation.listeners = nil
	return result
}

func generateFixtureMaterial(
	options fixtureOptions,
	example []byte,
	baseDynamicConfig []byte,
	ports map[string]string,
	now time.Time,
) (fixtureMaterial, error) {
	postgresDirectory := filepath.Join(options.root, "secrets", "operator", "postgres")

	postgresCertificates, err := generateCertificateBundle("postgres", options.project, false, now)
	if err != nil {
		return fixtureMaterial{}, fmt.Errorf("generate PostgreSQL TLS material: %w", err)
	}
	temporalCertificates, err := generateCertificateBundle("temporal", options.project, true, now)
	if err != nil {
		return fixtureMaterial{}, fmt.Errorf("generate Temporal TLS material: %w", err)
	}
	artifactCertificates, err := generateCertificateBundle("seaweed-s3", options.project, false, now)
	if err != nil {
		return fixtureMaterial{}, fmt.Errorf("generate artifact TLS material: %w", err)
	}

	files := make([]privateFile, 0, 36)
	files = appendCertificateFiles(files, filepath.Join("secrets", "operator", "postgres"), postgresCertificates, false)
	files = appendCertificateFiles(files, filepath.Join("secrets", "operator", "temporal"), temporalCertificates, true)
	files = appendCertificateFiles(files, filepath.Join("secrets", "operator", "artifacts"), artifactCertificates, false)

	passwordNames := []string{
		"superuser",
		"pm-app-migrator",
		"pm-app-runtime",
		"temporal-migrator",
		"temporal-runtime",
		"temporal-visibility-migrator",
		"temporal-visibility-runtime",
		"objects-migrator",
		"objects-runtime",
		"denied",
	}
	passwords := make(map[string]string, len(passwordNames))
	for _, name := range passwordNames {
		value, err := randomHex(24)
		if err != nil {
			return fixtureMaterial{}, fmt.Errorf("generate synthetic PostgreSQL credential: %w", err)
		}
		passwords[name] = value
		files = append(files, privateFile{
			relativePath: filepath.Join("secrets", "operator", "postgres", name+"-password"),
			contents:     []byte(value),
		})
	}

	endpointSpecifications := []struct {
		file         string
		user         string
		passwordName string
		database     string
	}{
		{file: "pm-app-endpoint", user: "pm_app_runtime", passwordName: "pm-app-runtime", database: "pm_app"},
		{file: "temporal-endpoint", user: "temporal_runtime", passwordName: "temporal-runtime", database: "temporal"},
		{file: "temporal-visibility-endpoint", user: "temporal_visibility_runtime", passwordName: "temporal-visibility-runtime", database: "temporal_visibility"},
		{file: "objects-endpoint", user: "pm_objects_runtime", passwordName: "objects-runtime", database: "pm_objects_meta"},
		{file: "denied-endpoint", user: "pm_denied", passwordName: "denied", database: "pm_app"},
	}
	for _, specification := range endpointSpecifications {
		endpoint := url.URL{
			Scheme: "postgres",
			User:   url.UserPassword(specification.user, passwords[specification.passwordName]),
			Host:   net.JoinHostPort("127.0.0.1", ports["POSTGRES"]),
			Path:   "/" + specification.database,
		}
		query := url.Values{}
		query.Set("sslmode", "verify-full")
		query.Set("sslrootcert", filepath.Join(postgresDirectory, "ca.pem"))
		endpoint.RawQuery = query.Encode()
		files = append(files, privateFile{
			relativePath: filepath.Join("secrets", "operator", "postgres", specification.file),
			contents:     []byte(endpoint.String()),
		})
	}

	for _, name := range []string{
		"access-key",
		"secret-key",
		"volume-jwt-write",
		"volume-jwt-read",
		"filer-jwt-write",
		"filer-jwt-read",
	} {
		value, err := randomHex(24)
		if err != nil {
			return fixtureMaterial{}, fmt.Errorf("generate synthetic artifact credential: %w", err)
		}
		files = append(files, privateFile{
			relativePath: filepath.Join("secrets", "operator", "artifacts", name),
			contents:     []byte(value),
		})
	}
	sseBytes, err := randomBytes(32)
	if err != nil {
		return fixtureMaterial{}, fmt.Errorf("generate synthetic artifact encryption key: %w", err)
	}
	files = append(files, privateFile{
		relativePath: filepath.Join("secrets", "operator", "artifacts", "sse-key"),
		contents:     []byte(base64.StdEncoding.EncodeToString(sseBytes)),
	})

	configuration, err := selectConfiguration(example, options.project, ports)
	if err != nil {
		return fixtureMaterial{}, err
	}
	dynamicConfig, rolloverNamespace, err := selectTemporalDynamicConfig(baseDynamicConfig, options.project)
	if err != nil {
		return fixtureMaterial{}, err
	}
	configurationPath := filepath.Join(options.root, "server.json")
	dynamicConfigPath := filepath.Join(options.root, "temporal-dynamicconfig.yaml")
	environment := map[string]string{
		"PM_STACK_ROOT":                       options.repositoryRoot,
		"PM_STACK_SECRETS_DIR":                filepath.Join(options.root, "secrets"),
		"PM_STACK_STATE_DIR":                  filepath.Join(options.root, "state"),
		"PM_STACK_PROJECT":                    options.project,
		"PM_STACK_TEMPORAL_DYNAMIC_CONFIG":    dynamicConfigPath,
		"PM_STACK_TEST_CONFIG":                configurationPath,
		"PM_STACK_TEST_ROLLOVER_NAMESPACE":    rolloverNamespace,
		"PM_STACK_TEST_S3_PRIMARY_ENDPOINT":   loopbackHTTPS(ports["S3_PRIMARY"]),
		"PM_STACK_TEST_S3_SECONDARY_ENDPOINT": loopbackHTTPS(ports["S3_SECONDARY"]),
		"PM_STORE_TEST_SECRETS_DIR":           postgresDirectory,
	}
	for _, name := range portNames {
		environment["PM_STACK_"+name+"_PORT"] = ports[name]
	}
	environmentJSON, err := json.MarshalIndent(environment, "", "  ")
	if err != nil {
		return fixtureMaterial{}, errors.New("encode safe fixture environment")
	}
	environmentJSON = append(environmentJSON, '\n')
	files = append(files,
		privateFile{relativePath: "server.json", contents: configuration},
		privateFile{relativePath: "temporal-dynamicconfig.yaml", contents: dynamicConfig},
		privateFile{relativePath: "environment.json", contents: environmentJSON},
	)
	return fixtureMaterial{files: files, environment: environmentJSON}, nil
}

func selectTemporalDynamicConfig(base []byte, project string) ([]byte, string, error) {
	if !projectNamePattern.MatchString(project) {
		return nil, "", errors.New("Temporal rollover profile project identity is invalid")
	}
	if len(base) == 0 || len(base) > maximumDynamicConfigBytes || !utf8.Valid(base) || bytes.IndexByte(base, 0) >= 0 {
		return nil, "", errors.New("deploy/temporal/dynamicconfig.yaml is invalid or exceeds the fixture bound")
	}
	if bytes.Contains(base, []byte("limit.historyCount.suggestContinueAsNew:")) {
		return nil, "", errors.New("deploy/temporal/dynamicconfig.yaml already owns the rollover suggestion key")
	}
	namespace := project + rolloverNamespaceSuffix
	profile := bytes.Clone(base)
	if profile[len(profile)-1] != '\n' {
		profile = append(profile, '\n')
	}
	profile = append(profile, []byte(fmt.Sprintf(
		"limit.historyCount.suggestContinueAsNew:\n  - value: %d\n    constraints:\n      namespace: %s\n",
		rolloverHistoryEventTarget,
		strconv.Quote(namespace),
	))...)
	if len(profile) > maximumDynamicConfigBytes {
		return nil, "", errors.New("selected Temporal dynamic configuration exceeds the fixture bound")
	}
	return profile, namespace, nil
}

func appendCertificateFiles(
	files []privateFile,
	directory string,
	bundle certificateBundle,
	includeClient bool,
) []privateFile {
	names := []string{"ca.pem", "server.pem", "server-key.pem"}
	if includeClient {
		names = append(names, "client.pem", "client-key.pem")
	}
	for _, name := range names {
		files = append(files, privateFile{
			relativePath: filepath.Join(directory, name),
			contents:     bundle[name],
		})
	}
	return files
}

func generateCertificateBundle(realm, project string, includeClient bool, now time.Time) (certificateBundle, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	caSerial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now = now.UTC()
	caTemplate := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: "PMCLI disposable " + project + " " + realm + " CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(72 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	bundle := certificateBundle{
		"ca.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
	}

	issue := func(name string, usages []x509.ExtKeyUsage) error {
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		serial, err := randomSerial()
		if err != nil {
			return err
		}
		leafTemplate := &x509.Certificate{
			SerialNumber:          serial,
			Subject:               pkix.Name{CommonName: realm + ".pm-stack.internal"},
			DNSNames:              append([]string(nil), certificateDNSNames...),
			IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
			NotBefore:             now.Add(-time.Hour),
			NotAfter:              now.Add(48 * time.Hour),
			KeyUsage:              x509.KeyUsageDigitalSignature,
			ExtKeyUsage:           append([]x509.ExtKeyUsage(nil), usages...),
			BasicConstraintsValid: true,
		}
		certificateDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caTemplate, &leafKey.PublicKey, caKey)
		if err != nil {
			return err
		}
		privateKeyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
		if err != nil {
			return err
		}
		bundle[name+".pem"] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})
		bundle[name+"-key.pem"] = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKeyDER})
		return nil
	}
	if err := issue("server", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}); err != nil {
		return nil, err
	}
	if includeClient {
		if err := issue("client", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}); err != nil {
			return nil, err
		}
	}
	return bundle, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	for {
		serial, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return nil, err
		}
		if serial.Sign() > 0 {
			return serial, nil
		}
	}
}

func randomBytes(length int) ([]byte, error) {
	value := make([]byte, length)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return nil, err
	}
	return value, nil
}

func randomHex(length int) (string, error) {
	value, err := randomBytes(length)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func selectConfiguration(example []byte, project string, ports map[string]string) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(example, &document); err != nil {
		return nil, errors.New("decode deploy/server.example.json")
	}
	if document == nil {
		return nil, errors.New("deploy/server.example.json must contain an object")
	}
	if err := replaceExistingString(document, "deployment_id", project); err != nil {
		return nil, err
	}
	if err := replaceExistingString(document, "public_origin", loopbackHTTPS(ports["PUBLIC"])); err != nil {
		return nil, err
	}
	if err := updateBundledSection(document, "postgres", map[string]string{
		"endpoint_ref": "secret://operator/postgres",
	}); err != nil {
		return nil, err
	}
	if err := updateBundledSection(document, "temporal", map[string]string{
		"endpoint":            net.JoinHostPort("127.0.0.1", ports["TEMPORAL"]),
		"namespace":           project,
		"client_identity_ref": "secret://operator/temporal",
	}); err != nil {
		return nil, err
	}
	if err := updateBundledSection(document, "artifacts", map[string]string{
		"endpoint":       loopbackHTTPS(ports["PUBLIC"]),
		"bucket":         project + "-artifacts",
		"credential_ref": "secret://operator/artifacts",
	}); err != nil {
		return nil, err
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, errors.New("encode selected stack configuration")
	}
	return append(encoded, '\n'), nil
}

func replaceExistingString(document map[string]json.RawMessage, key, value string) error {
	raw, exists := document[key]
	if !exists {
		return fmt.Errorf("deploy/server.example.json is missing %s", key)
	}
	var current string
	if err := json.Unmarshal(raw, &current); err != nil || current == "" {
		return fmt.Errorf("deploy/server.example.json has an invalid %s", key)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode selected %s", key)
	}
	document[key] = encoded
	return nil
}

func updateBundledSection(document map[string]json.RawMessage, name string, updates map[string]string) error {
	raw, exists := document[name]
	if !exists {
		return fmt.Errorf("deploy/server.example.json is missing %s", name)
	}
	var section map[string]json.RawMessage
	if err := json.Unmarshal(raw, &section); err != nil || section == nil {
		return fmt.Errorf("deploy/server.example.json has an invalid %s section", name)
	}
	var ownership string
	if err := json.Unmarshal(section["ownership"], &ownership); err != nil || ownership != "bundled" {
		return fmt.Errorf("deploy/server.example.json %s ownership must remain bundled", name)
	}
	for key, value := range updates {
		if err := replaceExistingString(section, key, value); err != nil {
			return fmt.Errorf("deploy/server.example.json %s section: %w", name, err)
		}
	}
	encoded, err := json.Marshal(section)
	if err != nil {
		return fmt.Errorf("encode selected %s section", name)
	}
	document[name] = encoded
	return nil
}

func loopbackHTTPS(port string) string {
	return "https://" + net.JoinHostPort("127.0.0.1", port)
}

func persistFixture(root string, material fixtureMaterial) error {
	if err := validateAbsentRoot(root); err != nil {
		return err
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		return fmt.Errorf("create private fixture root: %w", err)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return fmt.Errorf("secure private fixture root: %w", err)
	}
	for _, directory := range []string{
		"secrets",
		filepath.Join("secrets", "operator"),
		filepath.Join("secrets", "operator", "postgres"),
		filepath.Join("secrets", "operator", "temporal"),
		filepath.Join("secrets", "operator", "artifacts"),
	} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o700); err != nil {
			return fmt.Errorf("create private fixture directory: %w", err)
		}
	}
	for _, file := range material.files {
		if !filepath.IsLocal(file.relativePath) {
			return errors.New("fixture file path is not local")
		}
		if err := writePrivateFile(filepath.Join(root, file.relativePath), file.contents); err != nil {
			return fmt.Errorf("write private fixture member %q: %w", file.relativePath, err)
		}
	}
	return nil
}

func writePrivateFile(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	written, writeErr := io.Copy(file, bytes.NewReader(contents))
	if writeErr == nil && written != int64(len(contents)) {
		writeErr = io.ErrShortWrite
	}
	modeErr := file.Chmod(0o600)
	closeErr := file.Close()
	return errors.Join(writeErr, modeErr, closeErr)
}
