package stack

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
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

	// The job identity provider is a test-only Keycloak in its own Compose
	// overlay. Only this service may publish on the second loopback address.
	identityProviderService          = "keycloak"
	identityProviderNetwork          = "identity-private"
	identityProviderComposeFile      = "deploy/compose.identity.yaml"
	identityProviderBinding          = "${PM_STACK_IDENTITY_PROVIDER_HOST:?}:${PM_STACK_IDENTITY_PROVIDER_PORT:?}:8443"
	identityProviderUserVariable     = "${PM_STACK_IDENTITY_PROVIDER_USER:?}"
	identityProviderImageUser        = "1000:0"
	identityProviderBuildOutput      = "/opt/keycloak/lib/quarkus"
	identityProviderReadinessTimeout = 4 * time.Minute
	// identityProviderStartTimeout bounds the Compose start of the provider.
	// The start can pull the pinned image, so it is longer than the readiness
	// wait (review F7).
	identityProviderStartTimeout = 10 * time.Minute
	// The resource limits that D1-7 admits for the provider (review F5).
	identityProviderMaximumCPUs        = 2.0
	identityProviderMaximumMemoryBytes = 1536 << 20
	identityProviderMaximumPids        = 512
)

var projectNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{2,62}$`)

// identityProviderMounts are the only mounts that the provider may have, as
// Compose volume strings: each one read-only (review F3). The realm import is
// a directory mount, so a removed import file also leaves the container view
// (#316 review F2).
var identityProviderMounts = []string{
	"${PM_STACK_IDENTITY_PROVIDER_DIR:?}/server.pem:/run/secrets/identity-provider-server.pem:ro",
	"${PM_STACK_IDENTITY_PROVIDER_DIR:?}/server-key.pem:/run/secrets/identity-provider-server-key.pem:ro",
	"${PM_STACK_IDENTITY_PROVIDER_DIR:?}/import:/opt/keycloak/data/import:ro",
}

// identityProviderTmpfs are the only tmpfs mounts that the provider may have.
// The build output is writable and starts with a copy of the image's build
// output (review F3, F6).
var identityProviderTmpfs = []string{
	"/tmp:size=64m,mode=1777",
	"/opt/keycloak/data:size=128m,mode=1777",
	identityProviderBuildOutput + ":size=64m,mode=0770,tmpcopyup",
}

var identityProviderRealmPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

var identityProviderVariables = []string{
	"PM_STACK_IDENTITY_PROVIDER_HOST",
	"PM_STACK_IDENTITY_PROVIDER_PORT",
	"PM_STACK_IDENTITY_PROVIDER_ISSUER",
	"PM_STACK_IDENTITY_PROVIDER_DIR",
}

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
	identityProvider  *identityProvider
	identityUser      string
	stdout            io.Writer
	stderr            io.Writer
}

// identityProvider is the job-owned test identity provider that the job
// environment selects. It is nil for every other stack.
type identityProvider struct {
	host   string
	port   string
	issuer string
	dir    string
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
	User        string   `yaml:"user"`
	ReadOnly    bool     `yaml:"read_only"`
	CapDrop     []string `yaml:"cap_drop"`
	SecurityOpt []string `yaml:"security_opt"`
	Networks    []string `yaml:"networks"`
	Command     []string `yaml:"command"`
	Tmpfs       []string `yaml:"tmpfs"`
}

// identityOverlay is the identity provider overlay. pm-stack decodes it with
// known fields only, so a key that this type does not name, for example
// devices or an external network, refuses the overlay (D1-7, D1-8).
type identityOverlay struct {
	Services map[string]identityOverlayService `yaml:"services"`
	Networks map[string]identityOverlayNetwork `yaml:"networks"`
}

type identityOverlayService struct {
	Image           string   `yaml:"image"`
	Restart         string   `yaml:"restart"`
	Init            bool     `yaml:"init"`
	User            string   `yaml:"user"`
	ReadOnly        bool     `yaml:"read_only"`
	CapDrop         []string `yaml:"cap_drop"`
	CapAdd          []string `yaml:"cap_add"`
	SecurityOpt     []string `yaml:"security_opt"`
	Command         []string `yaml:"command"`
	Ports           []string `yaml:"ports"`
	Volumes         []string `yaml:"volumes"`
	Tmpfs           []string `yaml:"tmpfs"`
	Networks        []string `yaml:"networks"`
	NetworkMode     string   `yaml:"network_mode"`
	Privileged      bool     `yaml:"privileged"`
	CPUs            *float64 `yaml:"cpus"`
	MemLimit        *string  `yaml:"mem_limit"`
	PidsLimit       *int64   `yaml:"pids_limit"`
	StopGracePeriod string   `yaml:"stop_grace_period"`
}

type identityOverlayNetwork struct {
	Driver string            `yaml:"driver"`
	Labels map[string]string `yaml:"labels"`
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
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (err error) {
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
	if options.action == "up" {
		// Every failure of pm-stack up has a fixed step code, also before
		// upStack runs: opening, loading and validating the configuration
		// (review F9).
		defer func() {
			var step *stepError
			if err != nil && !errors.As(err, &step) {
				err = &stepError{step: "configuration", err: err}
			}
		}()
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
// authenticated Temporal namespace, authenticated S3 bucket access, and, for
// bundled artifacts, a writable volume for the bucket and the default
// collection.
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

// stepError names the failed step of pm-stack up, and causeError the cause of
// a failed provider check, with fixed codes. The job launcher prints only the
// codes, never the error text, which can name a path or an address.
type stepError struct {
	step string
	err  error
}

func (e *stepError) Error() string { return e.err.Error() }
func (e *stepError) Unwrap() error { return e.err }

// identityProviderCAError keeps the cause of an unreadable identity provider
// CA. Its text is fixed, because the cause can name a private path (review
// F4). errors.Is and errors.As still reach the cause.
type identityProviderCAError struct{ cause error }

func (*identityProviderCAError) Error() string   { return "identity provider CA is unreadable" }
func (e *identityProviderCAError) Unwrap() error { return e.cause }

type causeError struct{ code, text string }

func (e causeError) Error() string { return e.text }

// FailedStep returns "<step>" or "<step>/<cause>" for err, or "".
func FailedStep(err error) string {
	var step *stepError
	var cause causeError
	if !errors.As(err, &step) {
		return ""
	} else if errors.As(step.err, &cause) {
		return step.step + "/" + cause.code
	}
	return step.step
}

func upStack(ctx context.Context, configuration config.Configuration, selected dependencySet, stdout, stderr io.Writer) (err error) {
	if len(selected) == 0 {
		if err := CheckStack(ctx, configuration, stdout, stderr); err != nil {
			return &stepError{step: "stack-ready", err: err}
		}
		return nil
	}
	step := "configuration"
	defer func() {
		if err != nil {
			err = &stepError{step: step, err: err}
		}
	}()
	rt, err := newRuntime(configuration, stdout, stderr)
	if err != nil {
		return err
	}
	step = "deployment-lock"
	lock, err := acquireDeploymentLock(ctx, rt.stateDir)
	if err != nil {
		return err
	}
	defer releaseDeploymentLock(lock)
	step = "render"
	if err := rt.render(configuration); err != nil {
		return err
	}
	step = "compose-validate"
	if err := rt.validateCompose(ctx, configuration); err != nil {
		return err
	}

	step = "postgres-start"
	if selected[config.DependencyPostgres] {
		if err := rt.startServices(ctx, configuration, true, "postgres"); err != nil {
			return fmt.Errorf("start PostgreSQL: %w", err)
		}
	}
	step = "migrate"
	if err := migrateUnderLock(ctx, configuration, selected, rt); err != nil {
		return err
	}

	if selected[config.DependencyTemporal] {
		step = "temporal-start"
		if err := rt.startServices(ctx, configuration, false, "temporal"); err != nil {
			return fmt.Errorf("start Temporal: %w", err)
		}
		step = "temporal-namespace"
		if err := waitFor(ctx, readinessTimeout, func(checkCtx context.Context) error {
			return ensureTemporalNamespace(checkCtx, configuration, rt, true)
		}); err != nil {
			return fmt.Errorf("initialize Temporal namespace: %w", err)
		}
	}

	if selected[config.DependencyArtifacts] {
		step = "artifacts-start"
		for _, services := range [][]string{
			{"seaweed-master"},
			{"seaweed-volume"},
			{"seaweed-filer"},
			{"seaweed-s3-a", "seaweed-s3-b", "caddy"},
		} {
			if err := rt.startServices(ctx, configuration, false, services...); err != nil {
				return fmt.Errorf("start artifact services: %w", err)
			}
		}
		step = "artifact-bucket"
		// The start-up wait repeats only the bucket step and the read-only
		// capacity fact. The volume grow runs after the wait, once.
		if err := waitFor(ctx, readinessTimeout, func(checkCtx context.Context) error {
			if err := ensureArtifactBucket(checkCtx, configuration, rt, true); err != nil {
				return err
			}
			return checkArtifactVolumeCapacity(checkCtx, configuration, rt)
		}); err != nil {
			return fmt.Errorf("initialize artifact bucket: %w", err)
		}
		if err := ensureArtifactVolumes(ctx, configuration, rt, true); err != nil {
			return fmt.Errorf("initialize artifact volumes: %w", err)
		}
	}

	if rt.identityProvider != nil {
		// --no-recreate keeps a running provider: the job removes its realm
		// import file after the first ready answer.
		step = "identity-provider-start"
		if err := startWithin(ctx, identityProviderStartTimeout, func(startCtx context.Context) error {
			return rt.compose(startCtx, configuration, "up", "-d", "--no-recreate", identityProviderService)
		}); err != nil {
			return fmt.Errorf("start the job identity provider: %w", err)
		}
		step = "identity-provider-ready"
		if err := waitFor(ctx, identityProviderReadinessTimeout, func(checkCtx context.Context) error {
			return checkIdentityProvider(checkCtx, rt.identityProvider)
		}); err != nil {
			return fmt.Errorf("job identity provider readiness: %w", err)
		}
	}

	step = "stack-ready"
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

	services := make([]string, 0, 9)
	if rt.identityProvider != nil {
		services = append(services, identityProviderService)
	}
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
	// PM's own services publish only on 127.0.0.1. The job identity provider
	// has its own variables and is checked after the port selection below.
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
	rt.identityProvider, err = identityProviderFromEnvironment(os.Getenv, rt.ports)
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
	// Compose validation interpolates the overlay before the engine is known.
	// rt.compose replaces this value with the engine-specific user.
	rt.identityUser = rt.uid + ":" + rt.gid

	return rt, nil
}

// identityProviderFromEnvironment returns the job identity provider that the
// environment selects, or nil when it selects none. Its address must be a
// second IPv4 loopback address, so that PM cookies for 127.0.0.1 never reach it.
func identityProviderFromEnvironment(lookup func(string) string, ports map[string]string) (*identityProvider, error) {
	values := make(map[string]string, len(identityProviderVariables))
	for _, name := range identityProviderVariables {
		if value := lookup(name); value != "" {
			values[name] = value
		}
	}
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) != len(identityProviderVariables) {
		return nil, fmt.Errorf("the job identity provider requires %s together", strings.Join(identityProviderVariables, ", "))
	}
	host := values["PM_STACK_IDENTITY_PROVIDER_HOST"]
	if err := validateIdentityProviderHost(host); err != nil {
		return nil, err
	}
	port := values["PM_STACK_IDENTITY_PROVIDER_PORT"]
	if number, err := strconv.Atoi(port); err != nil || number < 1024 || number > 65535 || strconv.Itoa(number) != port {
		return nil, fmt.Errorf("PM_STACK_IDENTITY_PROVIDER_PORT must be an explicit non-privileged TCP port")
	}
	for name, value := range ports {
		if value == port {
			return nil, fmt.Errorf("PM_STACK_IDENTITY_PROVIDER_PORT and %s must not publish the same port", name)
		}
	}
	issuer := values["PM_STACK_IDENTITY_PROVIDER_ISSUER"]
	realm, found := strings.CutPrefix(issuer, "https://"+net.JoinHostPort(host, port)+"/realms/")
	if !found || !identityProviderRealmPattern.MatchString(realm) {
		return nil, fmt.Errorf("PM_STACK_IDENTITY_PROVIDER_ISSUER must be the exact HTTPS realm issuer at the identity provider address")
	}
	directory := values["PM_STACK_IDENTITY_PROVIDER_DIR"]
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, fmt.Errorf("PM_STACK_IDENTITY_PROVIDER_DIR must be an absolute clean path")
	}
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, fmt.Errorf("identity provider directory is unavailable")
	}
	if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("identity provider directory is not a directory")
	}
	if _, err := readBundleFile(resolved, "ca.pem"); err != nil {
		return nil, &identityProviderCAError{cause: err}
	}
	return &identityProvider{host: host, port: port, issuer: issuer, dir: resolved}, nil
}

// validateIdentityProviderHost admits only a canonical IPv4 loopback address
// other than 127.0.0.1. It refuses wildcard and non-loopback addresses.
func validateIdentityProviderHost(host string) error {
	address, err := netip.ParseAddr(host)
	if err == nil && address.IsUnspecified() {
		return fmt.Errorf("PM_STACK_IDENTITY_PROVIDER_HOST must not be a wildcard address")
	}
	if err != nil || !address.Is4() || address.String() != host {
		return fmt.Errorf("PM_STACK_IDENTITY_PROVIDER_HOST must be a canonical IPv4 address")
	}
	if !address.IsLoopback() {
		return fmt.Errorf("PM_STACK_IDENTITY_PROVIDER_HOST must not be a non-loopback address")
	}
	if address == netip.AddrFrom4([4]byte{127, 0, 0, 1}) {
		return fmt.Errorf("PM_STACK_IDENTITY_PROVIDER_HOST must differ from the first loopback address that PM uses")
	}
	return nil
}

// identityProviderUser selects a non-root container user that can read the
// provider's group-readable mounts. A rootless engine maps container group 0
// to the job account's group, so the image's own user 1000:0 reads them.
func identityProviderUser(rootless bool, uid, gid int) (string, error) {
	if rootless {
		return identityProviderImageUser, nil
	}
	if uid == 0 || gid == 0 {
		return "", fmt.Errorf("the job identity provider must not run as the root account or group")
	}
	return strconv.Itoa(uid) + ":" + strconv.Itoa(gid), nil
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
		"keycloak": {
			Repository: "docker.io/keycloak/keycloak", Tag: "26.8.1",
			IndexDigest: "sha256:3f3c5f8e03c614774b4bd9173b88391fefa4153bfc010d8012d59ad825377332",
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

// validateComposeSource checks the bundled projection and the identity
// provider overlay. Every service publishes only on 127.0.0.1, except the
// identity provider, which publishes exactly its second loopback binding.
func validateComposeSource(root string) error {
	bundled, err := readComposeDocument(root, "deploy/compose.yaml", "bundled Compose projection")
	if err != nil {
		return err
	}
	if len(bundled.Services) == 0 {
		return fmt.Errorf("bundled Compose projection contains no services")
	}
	if _, exists := bundled.Services[identityProviderService]; exists {
		return fmt.Errorf("service %q must stay outside the bundled Compose projection", identityProviderService)
	}
	identity, err := readComposeDocument(root, identityProviderComposeFile, "identity provider Compose overlay")
	if err != nil {
		return err
	}
	for name := range identity.Services {
		if name != identityProviderService {
			return fmt.Errorf("service %q must not join the identity provider overlay", name)
		}
	}
	if _, exists := identity.Services[identityProviderService]; !exists {
		return fmt.Errorf("identity provider overlay does not define service %q", identityProviderService)
	}
	for _, document := range []composeDocument{bundled, identity} {
		for name, service := range document.Services {
			if service.Privileged || service.NetworkMode == "host" {
				return fmt.Errorf("service %q requests prohibited host privileges", name)
			}
			for _, binding := range service.Ports {
				if name == identityProviderService {
					if binding != identityProviderBinding {
						return fmt.Errorf("service %q publishes a port outside its second loopback binding", name)
					}
				} else if !strings.HasPrefix(binding, "127.0.0.1:") {
					return fmt.Errorf("service %q publishes a non-loopback port", name)
				}
			}
			for _, mount := range service.Volumes {
				if strings.Contains(mount, "/var/run/docker.sock") {
					return fmt.Errorf("service %q mounts the Docker socket", name)
				}
			}
		}
	}
	overlay, err := readIdentityOverlay(root)
	if err != nil {
		return err
	}
	return validateIdentityOverlay(overlay)
}

// readIdentityOverlay decodes the identity provider overlay with known
// fields only.
func readIdentityOverlay(root string) (identityOverlay, error) {
	contents, err := os.ReadFile(filepath.Join(root, identityProviderComposeFile))
	if err != nil {
		return identityOverlay{}, errors.New("read identity provider Compose overlay: the file is unavailable")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)
	var overlay identityOverlay
	if err := decoder.Decode(&overlay); err != nil {
		return identityOverlay{}, fmt.Errorf("parse identity provider Compose overlay: %w", err)
	}
	return overlay, nil
}

// validateIdentityOverlay checks the D1-7 and D1-8 conditions of the overlay:
// the one service, its own network definition, no capability, no new
// privileges, the exact mount and tmpfs sets, and the resource limits.
func validateIdentityOverlay(overlay identityOverlay) error {
	provider, exists := overlay.Services[identityProviderService]
	if !exists || len(overlay.Services) != 1 {
		return fmt.Errorf("identity provider overlay must define only service %q", identityProviderService)
	}
	network, exists := overlay.Networks[identityProviderNetwork]
	if !exists || len(overlay.Networks) != 1 || network.Driver != "bridge" ||
		len(network.Labels) != 1 || network.Labels["ai.polymetrics.pm-stack"] != "true" {
		return fmt.Errorf("service %q must use its own bridge network %q, defined only in its overlay with the stack label", identityProviderService, identityProviderNetwork)
	}
	if provider.User != identityProviderUserVariable || !provider.ReadOnly ||
		!slices.Equal(provider.CapDrop, []string{"ALL"}) || len(provider.CapAdd) != 0 ||
		!slices.Equal(provider.SecurityOpt, []string{"no-new-privileges:true"}) ||
		!slices.Equal(provider.Networks, []string{identityProviderNetwork}) ||
		provider.NetworkMode != "" || provider.Privileged || provider.Restart != "no" ||
		!slices.Equal(provider.Ports, []string{identityProviderBinding}) {
		return fmt.Errorf("service %q must run as the selected non-root user with no capabilities, no new privileges, a read-only root file system, restart no, its own network and one published port", identityProviderService)
	}
	// The pinned stock image has no persisted Keycloak build, so Keycloak
	// refuses start --optimized. start builds once into a writable tmpfs that
	// starts with a copy of the image's build output.
	buildOutput := identityProviderTmpfs[len(identityProviderTmpfs)-1]
	if len(provider.Command) == 0 || provider.Command[0] != "start" || slices.Contains(provider.Command, "--optimized") ||
		!slices.Contains(provider.Tmpfs, buildOutput) {
		return fmt.Errorf("service %q must run start without --optimized and with the writable tmpfs %q, because the pinned stock image has no persisted build", identityProviderService, buildOutput)
	}
	if !sameStrings(provider.Volumes, identityProviderMounts) {
		return fmt.Errorf("service %q must have exactly the admitted read-only mounts", identityProviderService)
	}
	if !sameStrings(provider.Tmpfs, identityProviderTmpfs) {
		return fmt.Errorf("service %q must have exactly the admitted tmpfs mounts", identityProviderService)
	}
	memory, memoryOK := composeMemoryBytes(provider.MemLimit)
	if provider.CPUs == nil || *provider.CPUs <= 0 || *provider.CPUs > identityProviderMaximumCPUs ||
		!memoryOK || memory > identityProviderMaximumMemoryBytes ||
		provider.PidsLimit == nil || *provider.PidsLimit <= 0 || *provider.PidsLimit > identityProviderMaximumPids {
		return fmt.Errorf("service %q must set cpus, mem_limit and pids_limit within the admitted limits", identityProviderService)
	}
	return nil
}

// sameStrings reports whether the two lists hold the same strings, each once.
func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	sortedGot, sortedWant := slices.Clone(got), slices.Clone(want)
	slices.Sort(sortedGot)
	slices.Sort(sortedWant)
	return slices.Equal(sortedGot, sortedWant) && len(slices.Compact(sortedGot)) == len(sortedWant)
}

// composeMemoryBytes reads a Compose memory size: a positive integer with an
// optional b, k, m or g suffix. It refuses any other form.
func composeMemoryBytes(value *string) (int64, bool) {
	if value == nil || *value == "" {
		return 0, false
	}
	text := strings.ToLower(*value)
	multiplier := int64(1)
	switch text[len(text)-1] {
	case 'b':
		text = text[:len(text)-1]
	case 'k':
		multiplier, text = 1<<10, text[:len(text)-1]
	case 'm':
		multiplier, text = 1<<20, text[:len(text)-1]
	case 'g':
		multiplier, text = 1<<30, text[:len(text)-1]
	}
	number, err := strconv.ParseInt(text, 10, 64)
	if err != nil || number <= 0 || number > (1<<62)/multiplier {
		return 0, false
	}
	return number * multiplier, true
}

func readComposeDocument(root, relative, description string) (composeDocument, error) {
	contents, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil {
		return composeDocument{}, fmt.Errorf("read %s: %w", description, err)
	}
	var document composeDocument
	if err := yaml.Unmarshal(contents, &document); err != nil {
		return composeDocument{}, fmt.Errorf("parse %s: %w", description, err)
	}
	return document, nil
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
		"PM_STACK_IMAGE_KEYCLOAK":        rt.images["keycloak"],
		"COMPOSE_DISABLE_ENV_FILE":       "true",
		"COMPOSE_ENV_FILES":              "",
	}
	for name, value := range rt.ports {
		values[name] = value
	}
	if provider := rt.identityProvider; provider != nil {
		values["PM_STACK_IDENTITY_PROVIDER_HOST"] = provider.host
		values["PM_STACK_IDENTITY_PROVIDER_PORT"] = provider.port
		values["PM_STACK_IDENTITY_PROVIDER_DIR"] = provider.dir
		values["PM_STACK_IDENTITY_PROVIDER_USER"] = rt.identityUser
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
	if rt.identityProvider != nil {
		command = append(command, "-f", filepath.Join(rt.root, identityProviderComposeFile))
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

// serviceContainer is the state of one Compose service container that
// decides whether pm-stack up keeps it.
type serviceContainer struct {
	running    bool
	configHash string
}

// settledService reports whether a service keeps its container: the service
// has exactly one container, the container runs, and its Compose config hash
// label equals the hash that Compose expects for the current configuration.
// A changed configuration, a missing container, a stopped container or an
// unknown hash goes through the normal up.
func settledService(expectedHash string, containers []serviceContainer) bool {
	return expectedHash != "" &&
		len(containers) == 1 &&
		containers[0].running &&
		containers[0].configHash == expectedHash
}

// startServices runs compose up for the named services. A settled service
// (settledService) goes through up --no-recreate, so its running container
// stays. Compose otherwise recreates a running container when the engine
// reports a network ID that Compose cannot match, also when the configuration
// is unchanged (rootless Podman). Every other service goes through the normal
// up, which creates, starts or recreates it.
func (rt *runtime) startServices(ctx context.Context, configuration config.Configuration, wait bool, services ...string) error {
	if err := rt.resolveContainerIdentity(ctx); err != nil {
		return err
	}
	expected, err := rt.expectedServiceHashes(ctx, configuration, services)
	if err != nil {
		return err
	}
	var recreate, keep []string
	for _, service := range services {
		containers, err := rt.serviceContainers(ctx, service)
		if err != nil {
			return err
		}
		if settledService(expected[service], containers) {
			keep = append(keep, service)
		} else {
			recreate = append(recreate, service)
		}
	}
	arguments := []string{"up", "-d"}
	if wait {
		arguments = append(arguments, "--wait")
	}
	if len(recreate) > 0 {
		if err := rt.compose(ctx, configuration, append(slices.Clone(arguments), recreate...)...); err != nil {
			return err
		}
	}
	if len(keep) > 0 {
		keepArguments := append(slices.Clone(arguments), "--no-recreate")
		if err := rt.compose(ctx, configuration, append(keepArguments, keep...)...); err != nil {
			return err
		}
	}
	return nil
}

// expectedServiceHashes returns the config hash that Compose computes for
// each named service from the current configuration.
func (rt *runtime) expectedServiceHashes(ctx context.Context, configuration config.Configuration, services []string) (map[string]string, error) {
	command := exec.CommandContext(ctx, "python3",
		rt.composeArguments(configuration, "config", "--hash", strings.Join(services, ","))...)
	command.Dir = rt.root
	command.Env = rt.composeEnvironment()
	command.Stderr = rt.stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("read Compose service configuration hashes: %w", err)
	}
	hashes := make(map[string]string, len(services))
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("Compose service configuration hash output is malformed")
		}
		hashes[fields[0]] = fields[1]
	}
	return hashes, nil
}

// serviceContainers returns the state of the containers of one service of
// this Compose project. One-off containers (compose run) are not included.
func (rt *runtime) serviceContainers(ctx context.Context, service string) ([]serviceContainer, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	list := exec.CommandContext(ctx, "docker", "ps", "--all", "--quiet", "--no-trunc",
		"--filter", "label=com.docker.compose.project="+rt.project,
		"--filter", "label=com.docker.compose.service="+service,
		"--filter", "label=com.docker.compose.oneoff=False")
	list.Dir = rt.root
	output, err := list.Output()
	if err != nil {
		return nil, fmt.Errorf("list the %s service containers: %w", service, err)
	}
	ids := strings.Fields(string(output))
	containers := make([]serviceContainer, 0, len(ids))
	for _, id := range ids {
		inspect := exec.CommandContext(ctx, "docker", "inspect", "--format",
			`{{.State.Running}} {{index .Config.Labels "com.docker.compose.config-hash"}}`, id)
		inspect.Dir = rt.root
		state, err := inspect.Output()
		if err != nil {
			return nil, fmt.Errorf("inspect a %s service container: %w", service, err)
		}
		fields := strings.Fields(string(state))
		if len(fields) == 0 {
			return nil, fmt.Errorf("inspect a %s service container: empty state", service)
		}
		container := serviceContainer{running: fields[0] == "true"}
		if len(fields) == 2 {
			container.configHash = fields[1]
		}
		containers = append(containers, container)
	}
	return containers, nil
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
	if rt.identityProvider != nil {
		rootless, err := rootlessDaemon(output)
		if err != nil {
			return err
		}
		rt.identityUser, err = identityProviderUser(rootless, os.Getuid(), os.Getgid())
		if err != nil {
			return err
		}
	}
	return nil
}

func containerIdentity(output []byte, uid, gid int) (string, string, error) {
	rootless, err := rootlessDaemon(output)
	if err != nil {
		return "", "", err
	}
	if rootless {
		return "0", "0", nil
	}
	return strconv.Itoa(uid), strconv.Itoa(gid), nil
}

func rootlessDaemon(output []byte) (bool, error) {
	var options []string
	if err := json.Unmarshal(output, &options); err != nil || options == nil {
		return false, fmt.Errorf("Docker security options are invalid")
	}
	rootless, remapped := false, false
	for _, option := range options {
		name, _, _ := strings.Cut(option, ",")
		rootless = rootless || name == "name=rootless"
		remapped = remapped || name == "name=userns"
	}
	if !rootless && remapped {
		return false, fmt.Errorf("rootful Docker userns-remap requires an explicit bind-mount ownership mapping")
	}
	return rootless, nil
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
	if configuration.Artifacts().Ownership == config.OwnershipBundled {
		if err := ensureArtifactVolumes(ctx, configuration, rt, false); err != nil {
			return fmt.Errorf("artifact readiness: %w", err)
		}
	}
	if rt.identityProvider != nil {
		if err := checkIdentityProvider(ctx, rt.identityProvider); err != nil {
			return fmt.Errorf("job identity provider readiness: %w", err)
		}
	}
	return nil
}

// transportFailure returns the fixed class of a failed discovery request.
func transportFailure(err error) string {
	var certificate *tls.CertificateVerificationError
	var network net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection-refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection-closed"
	case errors.As(err, &certificate):
		return "tls-certificate"
	case errors.As(err, &network) && network.Timeout():
		return "timeout"
	}
	return "unavailable"
}

// checkIdentityProvider reads the realm discovery document over TLS that the
// identity provider CA and the exact IP address verify. The provider is ready
// only when it names the exact configured issuer, whatever the request host.
// Errors omit the address and the response.
func checkIdentityProvider(ctx context.Context, provider *identityProvider) error {
	ca, err := readBundleFile(provider.dir, "ca.pem")
	if err != nil {
		return &identityProviderCAError{cause: err}
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return fmt.Errorf("identity provider CA bundle is invalid")
	}
	transport := &http.Transport{
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: provider.host},
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{
		Transport:     transport,
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, provider.issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return fmt.Errorf("build identity provider discovery request")
	}
	response, err := httpClient.Do(request)
	if err != nil {
		code := transportFailure(err)
		return causeError{code, "identity provider discovery is unavailable: " + code}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		code, text := fmt.Sprintf("status-%d", response.StatusCode), "identity provider discovery returned status"
		return causeError{code, fmt.Sprintf("%s %d", text, response.StatusCode)}
	}
	var discovery struct {
		Issuer string `json:"issuer"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&discovery); err != nil {
		return causeError{"not-json", "identity provider discovery is not JSON"}
	}
	if discovery.Issuer != provider.issuer {
		return causeError{"issuer-mismatch", "identity provider discovery does not name the exact configured issuer"}
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
	temporalClient, err := client.DialContext(ctx, options)
	if err != nil {
		return temporalConnectError{cause: err}
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

// temporalConnectError keeps the cause of a failed Temporal connection for
// errors.As. Its text names only the class of the cause. The SDK text can
// name the endpoint or its address, so the text does not include it.
type temporalConnectError struct {
	cause error
}

func (e temporalConnectError) Error() string {
	var deadline *serviceerror.DeadlineExceeded
	var canceled *serviceerror.Canceled
	var unavailable *serviceerror.Unavailable
	switch {
	case errors.As(e.cause, &deadline):
		return "connect authenticated Temporal client: the server did not answer within the connection limit"
	case errors.As(e.cause, &canceled):
		return "connect authenticated Temporal client: the check was cancelled"
	case errors.As(e.cause, &unavailable):
		return "connect authenticated Temporal client: the server refused or closed the connection"
	default:
		return "connect authenticated Temporal client: the connection failed"
	}
}

func (e temporalConnectError) Unwrap() error {
	return e.cause
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

// seaweedMasterOrigin is the HTTP origin of the SeaweedFS master on the
// private stack network. pm-stack calls it inside the master container, so
// the admin API is not published outside that network.
const seaweedMasterOrigin = "http://seaweed-master:9333"

// seaweedTopology is the part of the master's /dir/status document that
// pm-stack reads: the volume slots and the writable volumes of each layout.
type seaweedTopology struct {
	Max     *int64 `json:"Max"`
	Free    *int64 `json:"Free"`
	Layouts []struct {
		Collection string   `json:"collection"`
		Writables  []uint32 `json:"writables"`
	} `json:"Layouts"`
}

// writable returns the number of writable volumes of the layouts whose
// collection is exactly the given collection. The default collection is "".
func (topology seaweedTopology) writable(collection string) int {
	count := 0
	for _, layout := range topology.Layouts {
		if layout.Collection == collection {
			count += len(layout.Writables)
		}
	}
	return count
}

// seaweedMaster calls one master endpoint with curl inside the master
// container and returns the response body. A non-2xx response fails. The
// Compose and curl diagnostics can name the private service address and the
// bucket, so pm-stack does not show them.
func (rt *runtime) seaweedMaster(ctx context.Context, configuration config.Configuration, path string, query url.Values) ([]byte, error) {
	if err := rt.resolveContainerIdentity(ctx); err != nil {
		return nil, err
	}
	target := seaweedMasterOrigin + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	command := exec.CommandContext(ctx, "python3", rt.composeArguments(configuration,
		"exec", "-T", "seaweed-master", "curl", "-fsS", "-m", "10", target)...)
	command.Dir = rt.root
	command.Env = rt.composeEnvironment()
	return command.Output()
}

func (rt *runtime) artifactVolumeStatus(ctx context.Context, configuration config.Configuration) (seaweedTopology, error) {
	output, err := rt.seaweedMaster(ctx, configuration, "/dir/status", nil)
	if err != nil {
		return seaweedTopology{}, fmt.Errorf("read the artifact volume status")
	}
	var status struct {
		Topology *seaweedTopology `json:"Topology"`
	}
	if err := json.Unmarshal(output, &status); err != nil || status.Topology == nil ||
		status.Topology.Max == nil || status.Topology.Free == nil {
		return seaweedTopology{}, fmt.Errorf("read the artifact volume status")
	}
	return *status.Topology, nil
}

// checkArtifactVolumeCapacity is a read-only readiness fact: the master
// reports volume slots, so the volume server has joined the master.
func checkArtifactVolumeCapacity(ctx context.Context, configuration config.Configuration, rt *runtime) error {
	topology, err := rt.artifactVolumeStatus(ctx, configuration)
	if err != nil {
		return err
	}
	if *topology.Max < 1 {
		return fmt.Errorf("artifact volume server has not joined the master")
	}
	return nil
}

// ensureArtifactVolumes requires a writable volume for the artifact bucket
// and for the default collection. Without one, the first object write of a
// run waits for SeaweedFS to grow the volume. With grow, it reads the status,
// asks the master once to grow one volume for each layout that has no
// writable volume, and reads the status again. It never repeats a grow. Each
// pm-stack up holds the deployment lock, and a second up finds the volumes
// and grows nothing. Without grow, it only reads.
func ensureArtifactVolumes(ctx context.Context, configuration config.Configuration, rt *runtime, grow bool) error {
	layouts := []struct {
		collection string
		missing    string
	}{
		{configuration.Artifacts().Bucket, "artifact bucket has no writable volume"},
		{"", "default collection has no writable volume"},
	}
	topology, err := rt.artifactVolumeStatus(ctx, configuration)
	if err != nil {
		return err
	}
	if grow {
		grown := false
		for _, layout := range layouts {
			if topology.writable(layout.collection) > 0 {
				continue
			}
			query := url.Values{"count": {"1"}}
			if layout.collection != "" {
				query.Set("collection", layout.collection)
			}
			if _, err := rt.seaweedMaster(ctx, configuration, "/vol/grow", query); err != nil {
				return fmt.Errorf("%s: the master did not grow a volume", layout.missing)
			}
			grown = true
		}
		if grown {
			if topology, err = rt.artifactVolumeStatus(ctx, configuration); err != nil {
				return err
			}
		}
	}
	for _, layout := range layouts {
		if topology.writable(layout.collection) < 1 {
			return errors.New(layout.missing)
		}
	}
	return nil
}

// startWithin runs start with its own deadline (review F7). rt.compose runs
// the process with exec.CommandContext, which stops the process when the
// deadline passes and returns after its exit. A start that passes the
// deadline keeps the step code of its caller, with the cause timeout.
func startWithin(ctx context.Context, limit time.Duration, start func(context.Context) error) error {
	startCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	err := start(startCtx)
	if err != nil && ctx.Err() == nil && errors.Is(startCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("the start did not end within %s: %w", limit,
			causeError{code: "timeout", text: "the start passed its deadline"})
	}
	return err
}

// waitFor repeats check until it passes, with one outer deadline for the
// whole wait (review F8). Each attempt derives from that deadline, and an
// attempt that passes after the deadline does not count.
func waitFor(ctx context.Context, timeout time.Duration, check func(context.Context) error) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last error
	for {
		attempt, stop := context.WithTimeout(waitCtx, 10*time.Second)
		last = check(attempt)
		stop()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if waitCtx.Err() != nil {
			if last == nil {
				last = causeError{code: "timeout", text: "the check passed only after the wait limit"}
			}
			return fmt.Errorf("the wait passed its limit of %s: %w", timeout, last)
		}
		if last == nil {
			return nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("the wait passed its limit of %s: %w", timeout, last)
		case <-timer.C:
		}
	}
}
