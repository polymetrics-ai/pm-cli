package acceptance

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// stackInputs are the job-owned stack inputs that scripts/ci/with-stack-fixture.py
// gives to the suite.
type stackInputs struct {
	document     map[string]json.RawMessage
	secretsDir   string
	postgresRef  string
	temporalRef  string
	artifactsRef string
	postgres     stackPostgres
	temporal     stackTemporal
	identity     stackIdentity
}

type stackPostgres struct {
	endpoint         *url.URL
	migratorPassword string
	adminPassword    string
}

// dsn returns a connection string for the stack PostgreSQL server. It keeps
// the host, port and TLS settings of the runtime endpoint.
func (postgres stackPostgres) dsn(user, password, database string) string {
	endpoint := *postgres.endpoint
	endpoint.User = url.UserPassword(user, password)
	endpoint.Path = "/" + database
	return endpoint.String()
}

type stackTemporal struct {
	endpoint  string
	namespace string
	bundle    string
}

func readStackInputs() (stackInputs, error) {
	environment := map[string]string{}
	for _, name := range []string{"PM_STACK_TEST_CONFIG", "PM_STACK_SECRETS_DIR", "PM_STORE_TEST_SECRETS_DIR"} {
		value := os.Getenv(name)
		if value == "" || !filepath.IsAbs(value) {
			return stackInputs{}, fmt.Errorf("%s must name an absolute path: run the @stack scenarios through scripts/ci/with-stack-fixture.py", name)
		}
		environment[name] = value
	}
	contents, err := readBoundedFile(environment["PM_STACK_TEST_CONFIG"], laneFileLimit)
	if err != nil {
		return stackInputs{}, err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(contents, &document); err != nil || document == nil {
		return stackInputs{}, errors.New("the stack configuration is not a JSON object")
	}
	var sections struct {
		Postgres struct {
			EndpointRef string `json:"endpoint_ref"`
		} `json:"postgres"`
		Temporal struct {
			Endpoint          string `json:"endpoint"`
			Namespace         string `json:"namespace"`
			ClientIdentityRef string `json:"client_identity_ref"`
		} `json:"temporal"`
		Artifacts struct {
			CredentialRef string `json:"credential_ref"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(contents, &sections); err != nil {
		return stackInputs{}, fmt.Errorf("decode the stack configuration: %w", err)
	}
	temporalBundle, err := secretBundlePath(sections.Temporal.ClientIdentityRef)
	if err != nil {
		return stackInputs{}, err
	}
	postgres, err := readStackPostgres(environment["PM_STORE_TEST_SECRETS_DIR"])
	if err != nil {
		return stackInputs{}, err
	}
	if sections.Temporal.Endpoint == "" || sections.Temporal.Namespace == "" {
		return stackInputs{}, errors.New("the stack configuration has no Temporal endpoint or namespace")
	}
	identity, err := readStackIdentity()
	if err != nil {
		return stackInputs{}, err
	}
	return stackInputs{
		document:     document,
		secretsDir:   environment["PM_STACK_SECRETS_DIR"],
		postgresRef:  sections.Postgres.EndpointRef,
		temporalRef:  sections.Temporal.ClientIdentityRef,
		artifactsRef: sections.Artifacts.CredentialRef,
		postgres:     postgres,
		temporal: stackTemporal{
			endpoint:  sections.Temporal.Endpoint,
			namespace: sections.Temporal.Namespace,
			bundle:    filepath.Join(environment["PM_STACK_SECRETS_DIR"], temporalBundle),
		},
		identity: identity,
	}, nil
}

func readStackPostgres(directory string) (stackPostgres, error) {
	read := func(member string) (string, error) {
		contents, err := readBoundedFile(filepath.Join(directory, member), 64<<10)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(contents)), nil
	}
	endpointText, err := read("pm-app-endpoint")
	if err != nil {
		return stackPostgres{}, err
	}
	endpoint, err := url.Parse(endpointText)
	if err != nil || (endpoint.Scheme != "postgres" && endpoint.Scheme != "postgresql") || endpoint.User == nil ||
		endpoint.User.Username() != store.RuntimeRole || strings.TrimPrefix(endpoint.Path, "/") != applicationDatabase {
		return stackPostgres{}, errors.New("the stack PostgreSQL runtime endpoint is not the pm_app runtime role")
	}
	migrator, err := read("pm-app-migrator-password")
	if err != nil {
		return stackPostgres{}, err
	}
	admin, err := read("superuser-password")
	if err != nil {
		return stackPostgres{}, err
	}
	if migrator == "" || admin == "" {
		return stackPostgres{}, errors.New("a stack PostgreSQL password is empty")
	}
	return stackPostgres{endpoint: endpoint, migratorPassword: migrator, adminPassword: admin}, nil
}

// secretBundlePath maps a secret://bundle/path reference to its relative
// directory, the same way the operator secret reader does.
func secretBundlePath(reference string) (string, error) {
	parsed, err := url.Parse(reference)
	if err != nil || parsed.Scheme != "secret" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("secret reference %q is invalid", reference)
	}
	relative := filepath.FromSlash(parsed.Host + "/" + strings.TrimPrefix(parsed.Path, "/"))
	relative = filepath.Clean(relative)
	if !filepath.IsLocal(relative) {
		return "", fmt.Errorf("secret reference %q leaves the secret root", reference)
	}
	return relative, nil
}

func writeSecretMember(root, bundle, member string, contents []byte) error {
	directory := filepath.Join(root, bundle)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create the lane secret bundle %s: %w", bundle, err)
	}
	return writePrivateFile(filepath.Join(directory, member), contents)
}

// writePrivateFile creates a new mode 0600 file. It never replaces a file.
func writePrivateFile(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	return nil
}

// readBoundedFile reads one regular file of at most limit bytes.
func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer file.Close()
	information, err := file.Stat()
	if err != nil || !information.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if int64(len(contents)) > limit || len(contents) == 0 {
		return nil, fmt.Errorf("%s is empty or larger than %d bytes", filepath.Base(path), limit)
	}
	return contents, nil
}

func randomHex(size int) string {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		panic(fmt.Sprintf("read random bytes: %v", err))
	}
	return hex.EncodeToString(value)
}
