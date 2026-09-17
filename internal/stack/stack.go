package stack

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/config"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowservicepb "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	postgresVersion          = "16.15"
	temporalServerVersion    = "1.31.2"
	temporalPersistence      = "1.19"
	temporalVisibility       = "1.14"
	maximumSecretBytes       = 64 << 10
	readinessTimeout         = 2 * time.Minute
	deploymentLockRetryDelay = 100 * time.Millisecond
)

var projectNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{2,62}$`)

var processDeploymentFence = make(chan struct{}, 1)

var deploymentPlaceholderPattern = regexp.MustCompile(`\{\{[A-Z][A-Z0-9_]*\}\}`)

type commandOptions struct {
	action     string
	configPath string
	selection  string
}

type dependencySet map[config.Dependency]bool

type runtime struct {
	root              string
	stateDir          string
	dataDir           string
	project           string
	postgresSecretDir string
	temporalSecretDir string
	artifactSecretDir string
	ports             map[string]string
	images            map[string]string
	uid               string
	gid               string
	stdout            io.Writer
	stderr            io.Writer
}

type imageLock struct {
	SchemaVersion int                  `json:"schema_version"`
	Source        string               `json:"source"`
	Images        map[string]imageSpec `json:"images"`
}

type imageSpec struct {
	Repository  string `json:"repository"`
	Tag         string `json:"tag"`
	IndexDigest string `json:"index_digest"`
}

type composeDocument struct {
	Services map[string]composeService `yaml:"services"`
}

type composeService struct {
	NetworkMode string   `yaml:"network_mode"`
	Ports       []string `yaml:"ports"`
	Privileged  bool     `yaml:"privileged"`
	Volumes     []string `yaml:"volumes"`
}

type postgresConnection struct {
	host       string
	port       uint16
	user       string
	password   string
	database   string
	serverName string
}

type renderTemplate struct {
	source       string
	destination  string
	replacements map[string]string
}

// Run is the stable operator entry point. It loads and validates the complete
// requested action before any init, start, stop, or delete operation is run.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}

	options, err := parseCommand(args)
	if err != nil {
		return err
	}

	reader := stdin
	var file *os.File
	if options.configPath != "" && options.configPath != "-" {
		file, err = os.Open(options.configPath)
		if err != nil {
			return fmt.Errorf("open stack configuration: %w", err)
		}
		defer file.Close()
		reader = file
	}
	if reader == nil {
		return fmt.Errorf("stack configuration is required on stdin or with --config")
	}

	document, err := config.Load(reader)
	if err != nil {
		return err
	}
	base, err := config.Validate(document, config.Prelaunch{})
	if err != nil {
		return err
	}
	selection, err := selectDependencies(base, options.selection)
	if err != nil {
		return err
	}
	prelaunch, err := requestedLifecycle(options.action, selection)
	if err != nil {
		return err
	}
	configuration, err := config.Validate(document, prelaunch)
	if err != nil {
		return err
	}

	switch options.action {
	case "check":
		if err := CheckStack(ctx, configuration, stdout, stderr); err != nil {
			return err
		}
	case "up":
		if err := upStack(ctx, configuration, selection, stdout, stderr); err != nil {
			return err
		}
	case "migrate":
		if err := migrateStack(ctx, configuration, selection, stdout, stderr); err != nil {
			return err
		}
	case "drain":
		if err := drainStack(ctx, configuration, selection, stdout, stderr); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported stack action %q", options.action)
	}
	return nil
}

// CheckStack verifies the selected PostgreSQL schemas and roles, the
// authenticated Temporal namespace, and authenticated S3 bucket access.
func CheckStack(ctx context.Context, configuration config.Configuration, stdout, stderr io.Writer) error {
	rt, err := newRuntime(configuration, stdout, stderr)
	if err != nil {
		return err
	}
	if err := checkLive(ctx, configuration, rt); err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, "pm-stack: PostgreSQL, Temporal, and artifact readiness verified")
	return err
}

// UpStack starts only bundled dependencies, applies their real migrations, and
// verifies all bundled or managed dependencies through authenticated clients.
func UpStack(ctx context.Context, configuration config.Configuration, stdout, stderr io.Writer) error {
	return upStack(ctx, configuration, bundledDependencies(configuration), stdout, stderr)
}

// MigrateStack serializes and runs the real PostgreSQL and Temporal schema
// tooling for bundled dependencies.
func MigrateStack(ctx context.Context, configuration config.Configuration, stdout, stderr io.Writer) error {
	return migrateStack(ctx, configuration, bundledDependencies(configuration), stdout, stderr)
}

// DrainStack stops bundled dependency services without deleting data or
// touching managed dependencies.
func DrainStack(ctx context.Context, configuration config.Configuration, stdout, stderr io.Writer) error {
	return drainStack(ctx, configuration, bundledDependencies(configuration), stdout, stderr)
}

func parseCommand(args []string) (commandOptions, error) {
	if len(args) == 0 {
		return commandOptions{}, fmt.Errorf("usage: pm-stack <check|up|migrate|drain> [--config path] [--dependency bundled|all|postgres|temporal|artifacts]")
	}
	options := commandOptions{action: args[0], selection: "bundled"}
	configSeen := false
	dependencySeen := false
	if options.action != "check" && options.action != "up" && options.action != "migrate" && options.action != "drain" {
		return commandOptions{}, fmt.Errorf("unsupported stack action %q", options.action)
	}
	for index := 1; index < len(args); index++ {
		switch args[index] {
		case "--config":
			if configSeen {
				return commandOptions{}, fmt.Errorf("--config requires exactly one path")
			}
			configSeen = true
			index++
			if index >= len(args) {
				return commandOptions{}, fmt.Errorf("--config requires exactly one path")
			}
			options.configPath = args[index]
		case "--dependency":
			if dependencySeen {
				return commandOptions{}, fmt.Errorf("--dependency requires exactly one selection")
			}
			dependencySeen = true
			index++
			if index >= len(args) {
				return commandOptions{}, fmt.Errorf("--dependency requires exactly one selection")
			}
			options.selection = args[index]
		default:
			return commandOptions{}, fmt.Errorf("unknown pm-stack argument %q", args[index])
		}
	}
	if options.action == "check" && dependencySeen {
		return commandOptions{}, fmt.Errorf("check does not accept --dependency")
	}
	return options, nil
}

func selectDependencies(configuration config.Configuration, selection string) (dependencySet, error) {
	all := []config.Dependency{config.DependencyPostgres, config.DependencyTemporal, config.DependencyArtifacts}
	selected := make(dependencySet)
	switch selection {
	case "bundled":
		return bundledDependencies(configuration), nil
	case "all":
		for _, dependency := range all {
			selected[dependency] = true
		}
		return selected, nil
	case string(config.DependencyPostgres):
		selected[config.DependencyPostgres] = true
	case string(config.DependencyTemporal):
		selected[config.DependencyTemporal] = true
	case string(config.DependencyArtifacts):
		selected[config.DependencyArtifacts] = true
	default:
		return nil, fmt.Errorf("unknown dependency selection %q", selection)
	}
	return selected, nil
}

func bundledDependencies(configuration config.Configuration) dependencySet {
	selected := make(dependencySet)
	if configuration.Postgres().Ownership == config.OwnershipBundled {
		selected[config.DependencyPostgres] = true
	}
	if configuration.Temporal().Ownership == config.OwnershipBundled {
		selected[config.DependencyTemporal] = true
	}
	if configuration.Artifacts().Ownership == config.OwnershipBundled {
		selected[config.DependencyArtifacts] = true
	}
	return selected
}

func requestedLifecycle(action string, selected dependencySet) (config.Prelaunch, error) {
	if action == "check" {
		return config.Prelaunch{}, nil
	}
	var lifecycleAction config.LifecycleAction
	switch action {
	case "up", "migrate":
		lifecycleAction = config.LifecycleInit
	case "drain":
		lifecycleAction = config.LifecycleStop
	default:
		return config.Prelaunch{}, fmt.Errorf("unsupported stack action %q", action)
	}
	ordered := []config.Dependency{config.DependencyPostgres, config.DependencyTemporal, config.DependencyArtifacts}
	requests := make([]config.LifecycleRequest, 0, len(selected))
	for _, dependency := range ordered {
		if selected[dependency] {
			requests = append(requests, config.LifecycleRequest{Dependency: dependency, Action: lifecycleAction})
		}
	}
	return config.Prelaunch{Lifecycle: requests}, nil
}

func upStack(ctx context.Context, configuration config.Configuration, selected dependencySet, stdout, stderr io.Writer) error {
	if len(selected) == 0 {
		return CheckStack(ctx, configuration, stdout, stderr)
	}
	rt, err := newRuntime(configuration, stdout, stderr)
	if err != nil {
		return err
	}
	lock, err := acquireDeploymentLock(ctx, rt.stateDir)
	if err != nil {
		return err
	}
	defer releaseDeploymentLock(lock)
	if err := rt.render(configuration); err != nil {
		return err
	}
	if err := rt.validateCompose(ctx, configuration); err != nil {
		return err
	}

	if selected[config.DependencyPostgres] {
		if err := rt.compose(ctx, configuration, "up", "-d", "--wait", "postgres"); err != nil {
			return fmt.Errorf("start PostgreSQL: %w", err)
		}
	}
	if err := migrateUnderLock(ctx, configuration, selected, rt); err != nil {
		return err
	}

	if selected[config.DependencyTemporal] {
		if err := rt.compose(ctx, configuration, "up", "-d", "temporal"); err != nil {
			return fmt.Errorf("start Temporal: %w", err)
		}
		if err := waitFor(ctx, readinessTimeout, func(checkCtx context.Context) error {
			return ensureTemporalNamespace(checkCtx, configuration, rt, true)
		}); err != nil {
			return fmt.Errorf("initialize Temporal namespace: %w", err)
		}
	}

	if selected[config.DependencyArtifacts] {
		for _, services := range [][]string{
			{"up", "-d", "seaweed-master"},
			{"up", "-d", "seaweed-volume"},
			{"up", "-d", "seaweed-filer"},
			{"up", "-d", "seaweed-s3-a", "seaweed-s3-b", "caddy"},
		} {
			if err := rt.compose(ctx, configuration, services...); err != nil {
				return fmt.Errorf("start artifact services: %w", err)
			}
		}
		if err := waitFor(ctx, readinessTimeout, func(checkCtx context.Context) error {
			return ensureArtifactBucket(checkCtx, configuration, rt, true)
		}); err != nil {
			return fmt.Errorf("initialize artifact bucket: %w", err)
		}
	}

	if err := waitFor(ctx, readinessTimeout, func(checkCtx context.Context) error {
		return checkLive(checkCtx, configuration, rt)
	}); err != nil {
		return fmt.Errorf("stack readiness: %w", err)
	}
	_, err = fmt.Fprintln(stdout, "pm-stack: bundled services started and authenticated readiness verified")
	return err
}

func migrateStack(ctx context.Context, configuration config.Configuration, selected dependencySet, stdout, stderr io.Writer) error {
	if len(selected) == 0 {
		return CheckStack(ctx, configuration, stdout, stderr)
	}
	rt, err := newRuntime(configuration, stdout, stderr)
	if err != nil {
		return err
	}
	lock, err := acquireDeploymentLock(ctx, rt.stateDir)
	if err != nil {
		return err
	}
	defer releaseDeploymentLock(lock)
	if err := rt.render(configuration); err != nil {
		return err
	}
	if err := rt.validateCompose(ctx, configuration); err != nil {
		return err
	}
	if err := migrateUnderLock(ctx, configuration, selected, rt); err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, "pm-stack: schema migrations completed under the deployment fence")
	return err
}

func migrateUnderLock(ctx context.Context, configuration config.Configuration, selected dependencySet, rt *runtime) error {
	postgresBundled := configuration.Postgres().Ownership == config.OwnershipBundled
	if postgresBundled && len(selected) > 0 {
		if err := rt.compose(ctx, configuration, "run", "--rm", "--no-deps", "postgres-bootstrap"); err != nil {
			return fmt.Errorf("run PostgreSQL bootstrap: %w", err)
		}
	}
	if selected[config.DependencyTemporal] {
		if postgresBundled {
			if err := rt.compose(ctx, configuration, "run", "--rm", "--no-deps", "temporal-schema"); err != nil {
				return fmt.Errorf("run Temporal schema tooling: %w", err)
			}
		} else if err := checkTemporalSchemas(ctx, configuration, rt); err != nil {
			return fmt.Errorf("managed PostgreSQL requires pre-provisioned Temporal schemas: %w", err)
		}
	}
	return nil
}

func drainStack(ctx context.Context, configuration config.Configuration, selected dependencySet, stdout, stderr io.Writer) error {
	if len(selected) == 0 {
		_, err := fmt.Fprintln(stdout, "pm-stack: no bundled services selected; managed dependencies were left untouched")
		return err
	}
	rt, err := newRuntime(configuration, stdout, stderr)
	if err != nil {
		return err
	}
	lock, err := acquireDeploymentLock(ctx, rt.stateDir)
	if err != nil {
		return err
	}
	defer releaseDeploymentLock(lock)

	services := make([]string, 0, 8)
	if selected[config.DependencyArtifacts] {
		services = append(services, "caddy", "seaweed-s3-a", "seaweed-s3-b", "seaweed-filer", "seaweed-volume", "seaweed-master")
	}
	if selected[config.DependencyTemporal] {
		services = append(services, "temporal")
	}
	if selected[config.DependencyPostgres] {
		services = append(services, "postgres")
	}
	arguments := append([]string{"stop", "--timeout", "45"}, services...)
	if err := rt.compose(ctx, configuration, arguments...); err != nil {
		return fmt.Errorf("drain bundled services: %w", err)
	}
	_, err = fmt.Fprintln(stdout, "pm-stack: bundled services drained; persistent data retained")
	return err
}

func newRuntime(configuration config.Configuration, stdout, stderr io.Writer) (*runtime, error) {
	root, err := repositoryRoot()
	if err != nil {
		return nil, err
	}
	rt := &runtime{root: root, stdout: stdout, stderr: stderr}
	if rt.stdout == nil {
		rt.stdout = io.Discard
	}
	if rt.stderr == nil {
		rt.stderr = io.Discard
	}

	rt.postgresSecretDir, err = resolveSecretDirectory(os.Getenv("PM_STACK_SECRETS_DIR"), configuration.Postgres().EndpointRef)
	if err != nil {
		return nil, fmt.Errorf("resolve PostgreSQL secret bundle: %w", err)
	}
	rt.temporalSecretDir, err = resolveSecretDirectory(os.Getenv("PM_STACK_SECRETS_DIR"), configuration.Temporal().ClientIdentityRef)
	if err != nil {
		return nil, fmt.Errorf("resolve Temporal identity bundle: %w", err)
	}
	rt.artifactSecretDir, err = resolveSecretDirectory(os.Getenv("PM_STACK_SECRETS_DIR"), configuration.Artifacts().CredentialRef)
	if err != nil {
		return nil, fmt.Errorf("resolve artifact credential bundle: %w", err)
	}
	if err := rt.validateSecretBundles(configuration); err != nil {
		return nil, err
	}

	if len(bundledDependencies(configuration)) == 0 {
		return rt, nil
	}
	if host := os.Getenv("PM_STACK_PUBLISH_HOST"); host != "" && host != "127.0.0.1" {
		return nil, fmt.Errorf("published infrastructure ports must bind only to 127.0.0.1")
	}
	rt.project = os.Getenv("PM_STACK_PROJECT")
	if !projectNamePattern.MatchString(rt.project) {
		return nil, fmt.Errorf("PM_STACK_PROJECT must be an explicit lowercase Compose project name")
	}
	stateDir := os.Getenv("PM_STACK_STATE_DIR")
	if stateDir == "" {
		return nil, fmt.Errorf("PM_STACK_STATE_DIR is required for bundled dependencies")
	}
	rt.stateDir, err = filepath.Abs(stateDir)
	if err != nil {
		return nil, fmt.Errorf("resolve stack state directory: %w", err)
	}
	rt.dataDir = filepath.Join(rt.stateDir, "data")
	rt.ports, err = publishedPorts()
	if err != nil {
		return nil, err
	}
	if err := rt.validateBundledEndpoints(configuration); err != nil {
		return nil, err
	}
	rt.images, err = loadImageLock(root)
	if err != nil {
		return nil, err
	}
	if err := validateComposeSource(root); err != nil {
		return nil, err
	}
	rt.uid = strconv.Itoa(os.Getuid())
	rt.gid = strconv.Itoa(os.Getgid())

	return rt, nil
}

func repositoryRoot() (string, error) {
	if configured := os.Getenv("PM_STACK_ROOT"); configured != "" {
		root, err := filepath.Abs(configured)
		if err != nil {
			return "", fmt.Errorf("resolve PM_STACK_ROOT: %w", err)
		}
		if sourceTreeExists(root) {
			return root, nil
		}
		return "", fmt.Errorf("PM_STACK_ROOT does not name a pm-cli source tree")
	}
	current, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve current directory: %w", err)
	}
	for {
		if sourceTreeExists(current) {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return "", fmt.Errorf("pm-cli source root not found; set PM_STACK_ROOT")
}

func sourceTreeExists(root string) bool {
	for _, relative := range []string{"deploy/compose.yaml", "deploy/images.lock.json", "tools/bootstrap.py"} {
		if info, err := os.Stat(filepath.Join(root, relative)); err != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	return true
}

func resolveSecretDirectory(root, reference string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("PM_STACK_SECRETS_DIR is required")
	}
	rootPath, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve secret root: %w", err)
	}
	rootPath, err = filepath.EvalSymlinks(rootPath)
	if err != nil {
		return "", fmt.Errorf("resolve secret root: %w", err)
	}
	parsed, err := url.Parse(reference)
	if err != nil || parsed.Scheme != "secret" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid secret bundle reference")
	}
	relative := filepath.FromSlash(parsed.Host + "/" + strings.TrimPrefix(parsed.EscapedPath(), "/"))
	relative, err = url.PathUnescape(relative)
	if err != nil {
		return "", fmt.Errorf("invalid escaped secret bundle reference")
	}
	candidate := filepath.Join(rootPath, relative)
	candidate, err = filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("secret bundle is unavailable")
	}
	if !pathWithin(rootPath, candidate) {
		return "", fmt.Errorf("secret bundle escapes PM_STACK_SECRETS_DIR")
	}
	info, err := os.Stat(candidate)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("secret bundle is not a directory")
	}
	return candidate, nil
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (rt *runtime) validateSecretBundles(configuration config.Configuration) error {
	postgresFiles := []string{
		"pm-app-endpoint",
		"temporal-endpoint",
		"temporal-visibility-endpoint",
		"objects-endpoint",
	}
	if configuration.Postgres().Ownership == config.OwnershipBundled {
		postgresFiles = append(postgresFiles,
			"superuser-password",
			"pm-app-migrator-password",
			"pm-app-runtime-password",
			"temporal-migrator-password",
			"temporal-runtime-password",
			"temporal-visibility-migrator-password",
			"temporal-visibility-runtime-password",
			"objects-migrator-password",
			"objects-runtime-password",
			"denied-password",
			"ca.pem",
			"server.pem",
			"server-key.pem",
		)
	}
	for _, name := range postgresFiles {
		if _, err := readBundleFile(rt.postgresSecretDir, name); err != nil {
			return fmt.Errorf("PostgreSQL secret bundle member %q: %w", name, err)
		}
	}

	temporalFiles := []string{"ca.pem", "client.pem", "client-key.pem"}
	if configuration.Temporal().Ownership == config.OwnershipBundled {
		temporalFiles = append(temporalFiles, "server.pem", "server-key.pem")
	}
	for _, name := range temporalFiles {
		if _, err := readBundleFile(rt.temporalSecretDir, name); err != nil {
			return fmt.Errorf("Temporal identity bundle member %q: %w", name, err)
		}
	}

	artifactFiles := []string{"access-key", "secret-key", "ca.pem"}
	if configuration.Artifacts().Ownership == config.OwnershipBundled {
		artifactFiles = append(artifactFiles,
			"server.pem",
			"server-key.pem",
			"volume-jwt-write",
			"volume-jwt-read",
			"filer-jwt-write",
			"filer-jwt-read",
			"sse-key",
		)
	}
	for _, name := range artifactFiles {
		if _, err := readBundleFile(rt.artifactSecretDir, name); err != nil {
			return fmt.Errorf("artifact credential bundle member %q: %w", name, err)
		}
	}
	return nil
}

func readBundleFile(directory, name string) ([]byte, error) {
	candidate := filepath.Join(directory, name)
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return nil, fmt.Errorf("secret file is unavailable")
	}
	if !pathWithin(directory, resolved) {
		return nil, fmt.Errorf("secret file escapes its bundle")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("secret file is not regular")
	}
	if info.Size() == 0 || info.Size() > maximumSecretBytes {
		return nil, fmt.Errorf("secret file size is invalid")
	}
	value, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("read secret file: %w", err)
	}
	return value, nil
}

func readBundleText(directory, name string) (string, error) {
	value, err := readBundleFile(directory, name)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(value))
	if text == "" || strings.ContainsAny(text, "\x00\r\n") {
		return "", fmt.Errorf("secret text value is invalid")
	}
	return text, nil
}

func publishedPorts() (map[string]string, error) {
	names := []string{
		"PM_STACK_POSTGRES_PORT",
		"PM_STACK_TEMPORAL_PORT",
		"PM_STACK_S3_PRIMARY_PORT",
		"PM_STACK_S3_SECONDARY_PORT",
		"PM_STACK_PUBLIC_PORT",
	}
	ports := make(map[string]string, len(names))
	seen := make(map[int]string, len(names))
	for _, name := range names {
		value := os.Getenv(name)
		port, err := strconv.Atoi(value)
		if err != nil || port < 1024 || port > 65535 {
			return nil, fmt.Errorf("%s must be an explicit non-privileged TCP port", name)
		}
		if other, exists := seen[port]; exists {
			return nil, fmt.Errorf("%s and %s must not publish the same port", other, name)
		}
		seen[port] = name
		ports[name] = value
	}
	return ports, nil
}

func (rt *runtime) validateBundledEndpoints(configuration config.Configuration) error {
	if configuration.Postgres().Ownership == config.OwnershipBundled {
		for _, endpoint := range []struct {
			name     string
			database string
		}{
			{name: "pm-app-endpoint", database: "pm_app"},
			{name: "temporal-endpoint", database: "temporal"},
			{name: "temporal-visibility-endpoint", database: "temporal_visibility"},
			{name: "objects-endpoint", database: "pm_objects_meta"},
		} {
			connection, err := rt.postgresConnection(endpoint.name, endpoint.database)
			if err != nil {
				return err
			}
			if !isLoopbackHost(connection.host) || strconv.Itoa(int(connection.port)) != rt.ports["PM_STACK_POSTGRES_PORT"] {
				return fmt.Errorf("bundled PostgreSQL endpoint %q must use its selected loopback port", endpoint.name)
			}
		}
	}
	if configuration.Temporal().Ownership == config.OwnershipBundled {
		host, port, err := net.SplitHostPort(configuration.Temporal().Endpoint)
		if err != nil || !isLoopbackHost(host) || port != rt.ports["PM_STACK_TEMPORAL_PORT"] {
			return fmt.Errorf("bundled Temporal endpoint must use its selected loopback port")
		}
	}
	if configuration.Artifacts().Ownership == config.OwnershipBundled {
		endpoint, err := url.Parse(configuration.Artifacts().Endpoint)
		if err != nil || !isLoopbackHost(endpoint.Hostname()) || endpoint.Port() != rt.ports["PM_STACK_PUBLIC_PORT"] {
			return fmt.Errorf("bundled artifact endpoint must use its selected HTTPS loopback port")
		}
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(strings.Trim(host, "[]"))
	return address != nil && address.IsLoopback()
}

func loadImageLock(root string) (map[string]string, error) {
	path := filepath.Join(root, "deploy/images.lock.json")
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open deployment image lock: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var lock imageLock
	if err := decoder.Decode(&lock); err != nil {
		return nil, fmt.Errorf("decode deployment image lock: %w", err)
	}
	if lock.SchemaVersion != 1 || lock.Source != "build/toolchains.lock.json" {
		return nil, fmt.Errorf("unsupported deployment image lock")
	}
	expected := map[string]imageSpec{
		"caddy": {
			Repository: "docker.io/library/caddy", Tag: "2.11.4",
			IndexDigest: "sha256:13ba145cba2f3e28fa801994876e4c086d1b95d5aa2a520a734765ffb6b12017",
		},
		"postgres": {
			Repository: "docker.io/library/postgres", Tag: postgresVersion,
			IndexDigest: "sha256:f1c3376c26f2609ab9f29f71f824103fe2fcd8ee0346485cb6122a4f93df6f94",
		},
		"seaweedfs": {
			Repository: "docker.io/chrislusf/seaweedfs", Tag: "4.46",
			IndexDigest: "sha256:08d516132314207d10c8e37cbffc1f32b147d870169688734cc61c6231625b62",
		},
		"temporal-admin-tools": {
			Repository: "docker.io/temporalio/admin-tools", Tag: "1.31.2",
			IndexDigest: "sha256:dbc5fcd6ee8f0f4d808bf765af9a87dea9d8a283abfdcfbd2fc148496ba66107",
		},
		"temporal-server": {
			Repository: "docker.io/temporalio/server", Tag: "1.31.2",
			IndexDigest: "sha256:b5ecdb8282bededae2a10c36e8d862e27d0bc2d247fc73c5416025997ab4a1da",
		},
	}
	if len(lock.Images) != len(expected) {
		return nil, fmt.Errorf("deployment image lock has an unexpected image set")
	}
	images := make(map[string]string, len(expected))
	for name, want := range expected {
		got, exists := lock.Images[name]
		if !exists || got != want {
			return nil, fmt.Errorf("deployment image %q does not match the authoritative lock", name)
		}
		images[name] = got.Repository + "@" + got.IndexDigest
	}
	return images, nil
}

func validateComposeSource(root string) error {
	contents, err := os.ReadFile(filepath.Join(root, "deploy/compose.yaml"))
	if err != nil {
		return fmt.Errorf("read bundled Compose projection: %w", err)
	}
	var document composeDocument
	if err := yaml.Unmarshal(contents, &document); err != nil {
		return fmt.Errorf("parse bundled Compose projection: %w", err)
	}
	if len(document.Services) == 0 {
		return fmt.Errorf("bundled Compose projection contains no services")
	}
	for name, service := range document.Services {
		if service.Privileged || service.NetworkMode == "host" {
			return fmt.Errorf("service %q requests prohibited host privileges", name)
		}
		for _, binding := range service.Ports {
			if !strings.HasPrefix(binding, "127.0.0.1:") {
				return fmt.Errorf("service %q publishes a non-loopback port", name)
			}
		}
		for _, mount := range service.Volumes {
			if strings.Contains(mount, "/var/run/docker.sock") {
				return fmt.Errorf("service %q mounts the Docker socket", name)
			}
		}
	}
	return nil
}

func (rt *runtime) render(configuration config.Configuration) error {
	postgresRuntime, err := rt.postgresRuntimeConnection()
	if err != nil {
		return err
	}
	visibilityRuntime, err := rt.postgresConnection("temporal-visibility-endpoint", "temporal_visibility")
	if err != nil {
		return err
	}
	objectsRuntime, err := rt.postgresConnection("objects-endpoint", "pm_objects_meta")
	if err != nil {
		return err
	}
	if configuration.Postgres().Ownership == config.OwnershipBundled {
		temporalPassword, err := readBundleText(rt.postgresSecretDir, "temporal-runtime-password")
		if err != nil {
			return fmt.Errorf("read Temporal runtime database credential: %w", err)
		}
		visibilityPassword, err := readBundleText(rt.postgresSecretDir, "temporal-visibility-runtime-password")
		if err != nil {
			return fmt.Errorf("read Temporal visibility database credential: %w", err)
		}
		objectsPassword, err := readBundleText(rt.postgresSecretDir, "objects-runtime-password")
		if err != nil {
			return fmt.Errorf("read object metadata database credential: %w", err)
		}
		postgresRuntime = postgresConnection{
			host: "postgres", port: 5432, user: "temporal_runtime",
			password: temporalPassword, database: "temporal", serverName: "postgres",
		}
		visibilityRuntime = postgresConnection{
			host: "postgres", port: 5432, user: "temporal_visibility_runtime",
			password: visibilityPassword, database: "temporal_visibility", serverName: "postgres",
		}
		objectsRuntime = postgresConnection{
			host: "postgres", port: 5432, user: "pm_objects_runtime",
			password: objectsPassword, database: "pm_objects_meta", serverName: "postgres",
		}
	}

	templates := make([]renderTemplate, 0, 5)
	if configuration.Artifacts().Ownership == config.OwnershipBundled {
		accessKey, err := readBundleText(rt.artifactSecretDir, "access-key")
		if err != nil {
			return fmt.Errorf("read artifact access key: %w", err)
		}
		secretKey, err := readBundleText(rt.artifactSecretDir, "secret-key")
		if err != nil {
			return fmt.Errorf("read artifact secret key: %w", err)
		}
		volumeWrite, err := readBundleText(rt.artifactSecretDir, "volume-jwt-write")
		if err != nil {
			return fmt.Errorf("read volume write identity: %w", err)
		}
		volumeRead, err := readBundleText(rt.artifactSecretDir, "volume-jwt-read")
		if err != nil {
			return fmt.Errorf("read volume read identity: %w", err)
		}
		filerWrite, err := readBundleText(rt.artifactSecretDir, "filer-jwt-write")
		if err != nil {
			return fmt.Errorf("read filer write identity: %w", err)
		}
		filerRead, err := readBundleText(rt.artifactSecretDir, "filer-jwt-read")
		if err != nil {
			return fmt.Errorf("read filer read identity: %w", err)
		}
		sseKey, err := readBundleText(rt.artifactSecretDir, "sse-key")
		if err != nil {
			return fmt.Errorf("read artifact encryption key: %w", err)
		}
		templates = append(templates,
			renderTemplate{
				source: "deploy/seaweed/filer.toml.tmpl", destination: "filer.toml",
				replacements: map[string]string{
					"POSTGRES_HOST":     strconv.Quote(objectsRuntime.host),
					"POSTGRES_PORT":     strconv.Itoa(int(objectsRuntime.port)),
					"POSTGRES_USER":     strconv.Quote(objectsRuntime.user),
					"POSTGRES_PASSWORD": strconv.Quote(objectsRuntime.password),
				},
			},
			renderTemplate{
				source: "deploy/seaweed/security.toml.tmpl", destination: "security.toml",
				replacements: map[string]string{
					"VOLUME_JWT_WRITE": strconv.Quote(volumeWrite),
					"VOLUME_JWT_READ":  strconv.Quote(volumeRead),
					"FILER_JWT_WRITE":  strconv.Quote(filerWrite),
					"FILER_JWT_READ":   strconv.Quote(filerRead),
					"SSE_KEY":          strconv.Quote(sseKey),
				},
			},
			renderTemplate{
				source: "deploy/seaweed/s3.json.tmpl", destination: "s3.json",
				replacements: map[string]string{
					"ACCESS_KEY": strconv.Quote(accessKey),
					"SECRET_KEY": strconv.Quote(secretKey),
				},
			},
		)
	}
	if configuration.Temporal().Ownership == config.OwnershipBundled {
		templates = append(templates, renderTemplate{
			source: "deploy/temporal/config.yaml.tmpl", destination: "temporal.yaml",
			replacements: map[string]string{
				"POSTGRES_ADDRESS":     strconv.Quote(net.JoinHostPort(postgresRuntime.host, strconv.Itoa(int(postgresRuntime.port)))),
				"POSTGRES_SERVER_NAME": strconv.Quote(postgresRuntime.serverName),
				"TEMPORAL_USER":        strconv.Quote(postgresRuntime.user),
				"TEMPORAL_PASSWORD":    strconv.Quote(postgresRuntime.password),
				"VISIBILITY_USER":      strconv.Quote(visibilityRuntime.user),
				"VISIBILITY_PASSWORD":  strconv.Quote(visibilityRuntime.password),
			},
		})
	}
	if configuration.Postgres().Ownership == config.OwnershipBundled {
		templates = append(templates, renderTemplate{
			source: "deploy/postgres/bootstrap.sql", destination: "bootstrap.sql",
			replacements: map[string]string{"ARTIFACT_BUCKET": configuration.Artifacts().Bucket},
		})
	}

	rendered := make(map[string][]byte, len(templates))
	for _, template := range templates {
		source, err := os.ReadFile(filepath.Join(rt.root, template.source))
		if err != nil {
			return fmt.Errorf("read deployment template %q: %w", template.source, err)
		}
		value := string(source)
		for token, replacement := range template.replacements {
			placeholder := "{{" + token + "}}"
			if strings.Count(value, placeholder) == 0 {
				return fmt.Errorf("deployment template %q is missing %s", template.source, placeholder)
			}
			value = strings.ReplaceAll(value, placeholder, replacement)
		}
		if deploymentPlaceholderPattern.MatchString(value) {
			return fmt.Errorf("deployment template %q contains an unresolved placeholder", template.source)
		}
		rendered[template.destination] = []byte(value)
	}

	if err := os.MkdirAll(rt.stateDir, 0o700); err != nil {
		return fmt.Errorf("create stack state directory: %w", err)
	}
	if err := os.Chmod(rt.stateDir, 0o700); err != nil {
		return fmt.Errorf("secure stack state directory: %w", err)
	}
	for _, directory := range []string{
		rt.dataDir,
		filepath.Join(rt.dataDir, "postgres"),
		filepath.Join(rt.dataDir, "seaweed-master"),
		filepath.Join(rt.dataDir, "seaweed-volume"),
		filepath.Join(rt.dataDir, "seaweed-filer"),
		filepath.Join(rt.dataDir, "seaweed-s3-a"),
		filepath.Join(rt.dataDir, "seaweed-s3-b"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("create stack data directory: %w", err)
		}
	}
	for name, value := range rendered {
		if err := writeAtomic(filepath.Join(rt.stateDir, name), value, 0o600); err != nil {
			return fmt.Errorf("write rendered deployment file %q: %w", name, err)
		}
	}
	return nil
}

func (rt *runtime) postgresRuntimeConnection() (postgresConnection, error) {
	connection, err := rt.postgresConnection("temporal-endpoint", "temporal")
	if err != nil {
		return postgresConnection{}, err
	}
	return connection, nil
}

func (rt *runtime) postgresConnection(name, expectedDatabase string) (postgresConnection, error) {
	dsn, err := readBundleText(rt.postgresSecretDir, name)
	if err != nil {
		return postgresConnection{}, fmt.Errorf("read PostgreSQL endpoint %q: %w", name, err)
	}
	parsed, err := pgx.ParseConfig(dsn)
	if err != nil {
		return postgresConnection{}, fmt.Errorf("parse PostgreSQL endpoint %q", name)
	}
	if parsed.Database != expectedDatabase || parsed.Host == "" || parsed.Port == 0 || parsed.User == "" || parsed.Password == "" || parsed.TLSConfig == nil {
		return postgresConnection{}, fmt.Errorf("PostgreSQL endpoint %q does not identify the required TLS database", name)
	}
	serverName := parsed.TLSConfig.ServerName
	if serverName == "" {
		serverName = parsed.Host
	}
	return postgresConnection{
		host: parsed.Host, port: parsed.Port, user: parsed.User, password: parsed.Password,
		database: parsed.Database, serverName: serverName,
	}, nil
}

func writeAtomic(path string, value []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".pm-stack-render-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(value); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

func (rt *runtime) composeEnvironment() []string {
	values := map[string]string{
		"PM_STACK_ROOT":                  rt.root,
		"PM_STACK_STATE_DIR":             rt.stateDir,
		"PM_STACK_DATA_DIR":              rt.dataDir,
		"PM_STACK_POSTGRES_SECRET_DIR":   rt.postgresSecretDir,
		"PM_STACK_TEMPORAL_SECRET_DIR":   rt.temporalSecretDir,
		"PM_STACK_ARTIFACT_SECRET_DIR":   rt.artifactSecretDir,
		"PM_STACK_UID":                   rt.uid,
		"PM_STACK_GID":                   rt.gid,
		"PM_STACK_IMAGE_POSTGRES":        rt.images["postgres"],
		"PM_STACK_IMAGE_TEMPORAL_ADMIN":  rt.images["temporal-admin-tools"],
		"PM_STACK_IMAGE_TEMPORAL_SERVER": rt.images["temporal-server"],
		"PM_STACK_IMAGE_SEAWEEDFS":       rt.images["seaweedfs"],
		"PM_STACK_IMAGE_CADDY":           rt.images["caddy"],
		"COMPOSE_DISABLE_ENV_FILE":       "true",
		"COMPOSE_ENV_FILES":              "",
	}
	for name, value := range rt.ports {
		values[name] = value
	}
	environment := make([]string, 0, len(os.Environ())+len(values))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, replaced := values[name]; !replaced {
			environment = append(environment, entry)
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		environment = append(environment, name+"="+values[name])
	}
	return environment
}

func (rt *runtime) composeArguments(configuration config.Configuration, arguments ...string) []string {
	command := []string{
		filepath.Join(rt.root, "tools/bootstrap.py"),
		"exec", "docker-compose", "--",
		"--project-directory", rt.root,
		"--project-name", rt.project,
		"-f", filepath.Join(rt.root, "deploy/compose.yaml"),
		"-f", filepath.Join(rt.root, "deploy/compose.managed.yaml"),
	}
	if configuration.Postgres().Ownership == config.OwnershipBundled {
		command = append(command, "--profile", "bundled-postgres")
	}
	if configuration.Temporal().Ownership == config.OwnershipBundled {
		command = append(command, "--profile", "bundled-temporal")
	}
	if configuration.Artifacts().Ownership == config.OwnershipBundled {
		command = append(command, "--profile", "bundled-artifacts")
	}
	return append(command, arguments...)
}

func (rt *runtime) compose(ctx context.Context, configuration config.Configuration, arguments ...string) error {
	// Bind-mounted private files must be read as their owner in the selected
	// daemon's user namespace. Rootless Docker maps container root to the
	// unprivileged daemon owner, not to host root.
	if err := rt.resolveContainerIdentity(ctx); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "python3", rt.composeArguments(configuration, arguments...)...)
	command.Dir = rt.root
	command.Env = rt.composeEnvironment()
	command.Stdout = rt.stdout
	command.Stderr = rt.stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("docker compose command failed: %w", err)
	}
	return nil
}

func (rt *runtime) resolveContainerIdentity(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "docker", "info", "--format", "{{json .SecurityOptions}}")
	command.Dir = rt.root
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("inspect Docker user namespace: %w", err)
	}
	uid, gid, err := containerIdentity(output, os.Getuid(), os.Getgid())
	if err != nil {
		return err
	}
	rt.uid, rt.gid = uid, gid
	return nil
}

func containerIdentity(output []byte, uid, gid int) (string, string, error) {
	var options []string
	if err := json.Unmarshal(output, &options); err != nil || options == nil {
		return "", "", fmt.Errorf("Docker security options are invalid")
	}
	rootless, remapped := false, false
	for _, option := range options {
		name, _, _ := strings.Cut(option, ",")
		rootless = rootless || name == "name=rootless"
		remapped = remapped || name == "name=userns"
	}
	if rootless {
		return "0", "0", nil
	}
	if remapped {
		return "", "", fmt.Errorf("rootful Docker userns-remap requires an explicit bind-mount ownership mapping")
	}
	return strconv.Itoa(uid), strconv.Itoa(gid), nil
}

func (rt *runtime) validateCompose(ctx context.Context, configuration config.Configuration) error {
	command := exec.CommandContext(ctx, "python3", rt.composeArguments(configuration, "config", "--quiet")...)
	command.Dir = rt.root
	command.Env = rt.composeEnvironment()
	command.Stdout = io.Discard
	command.Stderr = rt.stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("validate Docker Compose projection: %w", err)
	}
	return nil
}

func acquireDeploymentLock(ctx context.Context, stateDir string) (*os.File, error) {
	if stateDir == "" {
		return nil, fmt.Errorf("stack state directory is unavailable")
	}
	select {
	case processDeploymentFence <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	keepProcessFence := true
	defer func() {
		if keepProcessFence {
			<-processDeploymentFence
		}
	}()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create deployment lock directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(stateDir, "operation.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open deployment lock: %w", err)
	}
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			keepProcessFence = false
			return lock, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			lock.Close()
			return nil, fmt.Errorf("acquire deployment lock: %w", err)
		}
		timer := time.NewTimer(deploymentLockRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			lock.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func releaseDeploymentLock(lock *os.File) {
	if lock == nil {
		return
	}
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
	<-processDeploymentFence
}

func checkLive(ctx context.Context, configuration config.Configuration, rt *runtime) error {
	if err := checkPostgres(ctx, configuration, rt); err != nil {
		return fmt.Errorf("PostgreSQL readiness: %w", err)
	}
	if err := ensureTemporalNamespace(ctx, configuration, rt, false); err != nil {
		return fmt.Errorf("Temporal readiness: %w", err)
	}
	if err := ensureArtifactBucket(ctx, configuration, rt, false); err != nil {
		return fmt.Errorf("artifact readiness: %w", err)
	}
	return nil
}

func checkPostgres(ctx context.Context, configuration config.Configuration, rt *runtime) error {
	pmEndpoint, err := readBundleText(rt.postgresSecretDir, "pm-app-endpoint")
	if err != nil {
		return err
	}
	pm, err := pgx.Connect(ctx, pmEndpoint)
	if err != nil {
		return fmt.Errorf("connect pm_app")
	}
	defer pm.Close(context.Background())
	var database, user, serverVersion, revision string
	if err := pm.QueryRow(ctx, `SELECT current_database(), current_user, current_setting('server_version'), revision FROM pm.stack_schema WHERE singleton`).Scan(&database, &user, &serverVersion, &revision); err != nil {
		return fmt.Errorf("query pm_app schema")
	}
	if database != "pm_app" || user != "pm_app_runtime" || !strings.HasPrefix(serverVersion, postgresVersion) || revision != "pm.stack/v1" {
		return fmt.Errorf("pm_app database, role, version, or schema revision mismatch")
	}
	rows, err := pm.Query(ctx, `SELECT rolname FROM pg_roles WHERE rolname = ANY($1)`, []string{
		"pm_app_migrator", "pm_app_runtime",
		"temporal_migrator", "temporal_runtime",
		"temporal_visibility_migrator", "temporal_visibility_runtime",
		"pm_objects_migrator", "pm_objects_runtime",
	})
	if err != nil {
		return fmt.Errorf("query PostgreSQL roles")
	}
	defer rows.Close()
	roles := make(map[string]bool)
	for rows.Next() {
		var role string
		if err := rows.Scan(&role); err != nil {
			return fmt.Errorf("scan PostgreSQL role")
		}
		roles[role] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read PostgreSQL roles")
	}
	if len(roles) != 8 {
		return fmt.Errorf("required PostgreSQL role set is incomplete")
	}
	return checkTemporalSchemas(ctx, configuration, rt)
}

func checkTemporalSchemas(ctx context.Context, configuration config.Configuration, rt *runtime) error {
	checks := []struct {
		file     string
		database string
		user     string
		version  string
	}{
		{file: "temporal-endpoint", database: "temporal", user: "temporal_runtime", version: temporalPersistence},
		{file: "temporal-visibility-endpoint", database: "temporal_visibility", user: "temporal_visibility_runtime", version: temporalVisibility},
	}
	for _, check := range checks {
		endpoint, err := readBundleText(rt.postgresSecretDir, check.file)
		if err != nil {
			return err
		}
		connection, err := pgx.Connect(ctx, endpoint)
		if err != nil {
			return fmt.Errorf("connect %s", check.database)
		}
		var database, user, version string
		err = connection.QueryRow(ctx, `SELECT current_database(), current_user, curr_version FROM schema_version WHERE version_partition = 0 AND db_name = $1`, check.database).Scan(&database, &user, &version)
		connection.Close(context.Background())
		if err != nil {
			return fmt.Errorf("query %s schema version", check.database)
		}
		if database != check.database || user != check.user || version != check.version {
			return fmt.Errorf("%s database, role, or schema version mismatch", check.database)
		}
	}

	objectsEndpoint, err := readBundleText(rt.postgresSecretDir, "objects-endpoint")
	if err != nil {
		return err
	}
	objects, err := pgx.Connect(ctx, objectsEndpoint)
	if err != nil {
		return fmt.Errorf("connect pm_objects_meta")
	}
	defer objects.Close(context.Background())
	var database, user string
	var tableCount int
	if err := objects.QueryRow(ctx, `SELECT current_database(), current_user, count(*) FROM pg_tables WHERE schemaname = 'pm_objects' AND tablename IN ('filemeta', $1)`, configuration.Artifacts().Bucket).Scan(&database, &user, &tableCount); err != nil {
		return fmt.Errorf("query pm_objects_meta schema")
	}
	if database != "pm_objects_meta" || user != "pm_objects_runtime" || tableCount != 2 {
		return fmt.Errorf("pm_objects_meta database, role, or schema mismatch")
	}
	return nil
}

func temporalOptions(configuration config.Configuration, rt *runtime) (client.Options, error) {
	ca, err := readBundleFile(rt.temporalSecretDir, "ca.pem")
	if err != nil {
		return client.Options{}, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return client.Options{}, fmt.Errorf("Temporal CA bundle is invalid")
	}
	host, _, err := net.SplitHostPort(configuration.Temporal().Endpoint)
	if err != nil {
		return client.Options{}, fmt.Errorf("Temporal endpoint is not host:port")
	}
	certificate, err := tls.LoadX509KeyPair(
		filepath.Join(rt.temporalSecretDir, "client.pem"),
		filepath.Join(rt.temporalSecretDir, "client-key.pem"),
	)
	if err != nil {
		return client.Options{}, fmt.Errorf("load Temporal client identity")
	}
	return client.Options{
		HostPort:    configuration.Temporal().Endpoint,
		Namespace:   configuration.Temporal().Namespace,
		Identity:    "pm-stack-operator",
		Credentials: client.NewMTLSCredentials(certificate),
		ConnectionOptions: client.ConnectionOptions{TLS: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
			ServerName: host,
		}},
	}, nil
}

func ensureTemporalNamespace(ctx context.Context, configuration config.Configuration, rt *runtime, create bool) error {
	options, err := temporalOptions(configuration, rt)
	if err != nil {
		return err
	}
	temporalClient, err := client.Dial(options)
	if err != nil {
		return fmt.Errorf("connect authenticated Temporal client")
	}
	defer temporalClient.Close()
	service := temporalClient.WorkflowService()
	response, err := service.DescribeNamespace(ctx, &workflowservicepb.DescribeNamespaceRequest{
		Namespace: configuration.Temporal().Namespace,
	})
	if err != nil && create {
		var notFound *serviceerror.NamespaceNotFound
		if errors.As(err, &notFound) {
			_, err = service.RegisterNamespace(ctx, &workflowservicepb.RegisterNamespaceRequest{
				Namespace:                        configuration.Temporal().Namespace,
				WorkflowExecutionRetentionPeriod: durationpb.New(configuration.Temporal().Retention),
			})
			if err != nil {
				var exists *serviceerror.NamespaceAlreadyExists
				if !errors.As(err, &exists) {
					return fmt.Errorf("register configured namespace")
				}
			}
			response, err = service.DescribeNamespace(ctx, &workflowservicepb.DescribeNamespaceRequest{
				Namespace: configuration.Temporal().Namespace,
			})
		}
	}
	if err != nil {
		return fmt.Errorf("describe configured namespace")
	}
	if response.GetNamespaceInfo().GetName() != configuration.Temporal().Namespace ||
		response.GetNamespaceInfo().GetState() != enumspb.NAMESPACE_STATE_REGISTERED ||
		response.GetConfig().GetWorkflowExecutionRetentionTtl().AsDuration() != configuration.Temporal().Retention {
		return fmt.Errorf("configured namespace name, state, or retention mismatch")
	}
	systemInfo, err := service.GetSystemInfo(ctx, &workflowservicepb.GetSystemInfoRequest{})
	if err != nil {
		return fmt.Errorf("read Temporal server version")
	}
	if systemInfo.GetServerVersion() != temporalServerVersion {
		return fmt.Errorf("Temporal server version mismatch")
	}
	return nil
}

func artifactClient(configuration config.Configuration, rt *runtime) (*s3.Client, error) {
	endpoint, err := url.Parse(configuration.Artifacts().Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" {
		return nil, fmt.Errorf("artifact endpoint must be HTTPS")
	}
	ca, err := readBundleFile(rt.artifactSecretDir, "ca.pem")
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("artifact CA bundle is invalid")
	}
	accessKey, err := readBundleText(rt.artifactSecretDir, "access-key")
	if err != nil {
		return nil, err
	}
	secretKey, err := readBundleText(rt.artifactSecretDir, "secret-key")
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	return s3.New(s3.Options{
		BaseEndpoint: aws.String(endpoint.String()),
		Region:       "us-east-1",
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey, Source: "pm-stack-secret-bundle"}, nil
		}),
		HTTPClient:   &http.Client{Transport: transport, Timeout: 30 * time.Second},
		UsePathStyle: true,
	}), nil
}

func ensureArtifactBucket(ctx context.Context, configuration config.Configuration, rt *runtime, create bool) error {
	artifactClient, err := artifactClient(configuration, rt)
	if err != nil {
		return err
	}
	_, err = artifactClient.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(configuration.Artifacts().Bucket)})
	if err != nil && create {
		var responseError *smithyhttp.ResponseError
		if !errors.As(err, &responseError) || responseError.HTTPStatusCode() != http.StatusNotFound {
			return fmt.Errorf("authenticate artifact bucket")
		}
		_, err = artifactClient.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(configuration.Artifacts().Bucket)})
		if err == nil {
			_, err = artifactClient.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(configuration.Artifacts().Bucket)})
		}
	}
	if err != nil {
		return fmt.Errorf("authenticate artifact bucket")
	}
	_, err = artifactClient.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(configuration.Artifacts().Bucket), MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return fmt.Errorf("list artifact bucket")
	}
	return nil
}

func waitFor(ctx context.Context, timeout time.Duration, check func(context.Context) error) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
		last = check(attempt)
		cancel()
		if last == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return last
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
