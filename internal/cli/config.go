package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/spf13/cobra"
)

const (
	defaultRequestTimeout = 30 * time.Second
	maximumInputLength    = 4096
)

type LookupEnv func(string) (string, bool)
type SessionStoreFactory func(pmapi.SessionStoreMode) (pmapi.SessionStore, error)

type Options struct {
	Args          []string
	Stdin         io.Reader
	Stdout        io.Writer
	Stderr        io.Writer
	LookupEnv     LookupEnv
	ProfilesPath  string
	HTTPClient    pmapi.HttpRequestDoer
	SessionStore  SessionStoreFactory
	RecoveryStore pmapi.RecoveryStore
	Version       string
}

type rootFlags struct {
	serverProfile  singleString
	workspace      singleString
	connection     singleString
	output         singleString
	json           singleBool
	wait           singleBool
	requestTimeout singleString
	sessionStore   singleString
}

type ClientControls struct {
	Output         *string
	JSON           *bool
	Wait           *bool
	RequestTimeout *string
	SessionStore   *string
	Workspace      *string
	Connection     *string
}

type ClientConfig struct {
	Profile        pmapi.ServerProfile
	Workspace      string
	Connection     string
	Output         pmapi.OutputFormat
	Wait           bool
	RequestTimeout time.Duration
	SessionMode    pmapi.SessionStoreMode
}

// LoadClientConfig applies the only supported precedence order: explicit
// control presence, then present environment, then non-secret profile, then
// built-in defaults. False and empty presence are never inferred from values.
func LoadClientConfig(profile pmapi.ServerProfile, environment map[string]string, controls ClientControls) (ClientConfig, error) {
	for name := range environment {
		switch name {
		case "PM_OUTPUT", "PM_JSON", "PM_WAIT", "PM_REQUEST_TIMEOUT":
		default:
			return ClientConfig{}, fmt.Errorf("%w: unknown environment control %s", errUsage, name)
		}
	}
	config := ClientConfig{Profile: profile, Output: pmapi.OutputText, RequestTimeout: defaultRequestTimeout, SessionMode: pmapi.SessionStoreOS}
	if profile.Output != nil {
		config.Output = *profile.Output
	}
	if profile.Wait != nil {
		config.Wait = *profile.Wait
	}
	if profile.RequestTimeout != nil {
		config.RequestTimeout = *profile.RequestTimeout
	}
	outputRaw, outputPresent := environment["PM_OUTPUT"]
	jsonRaw, jsonPresent := environment["PM_JSON"]
	if outputPresent {
		output, err := pmapi.ParseOutputFormat(outputRaw)
		if err != nil {
			return ClientConfig{}, fmt.Errorf("%w: PM_OUTPUT must be text or json", errUsage)
		}
		config.Output = output
	}
	if jsonPresent {
		jsonOutput, err := parseStrictBool(jsonRaw)
		if err != nil {
			return ClientConfig{}, fmt.Errorf("%w: PM_JSON must be true or false", errUsage)
		}
		selected := pmapi.OutputText
		if jsonOutput {
			selected = pmapi.OutputJSON
		}
		if outputPresent && config.Output != selected {
			return ClientConfig{}, fmt.Errorf("%w: PM_OUTPUT and PM_JSON conflict", errUsage)
		}
		config.Output = selected
	}
	if raw, present := environment["PM_WAIT"]; present {
		parsed, err := parseStrictBool(raw)
		if err != nil {
			return ClientConfig{}, fmt.Errorf("%w: PM_WAIT must be true or false", errUsage)
		}
		config.Wait = parsed
	}
	if raw, present := environment["PM_REQUEST_TIMEOUT"]; present {
		duration, err := parseCanonicalDuration(raw)
		if err != nil {
			return ClientConfig{}, fmt.Errorf("%w: PM_REQUEST_TIMEOUT is invalid", errUsage)
		}
		config.RequestTimeout = duration
	}
	if controls.Output != nil {
		output, err := pmapi.ParseOutputFormat(*controls.Output)
		if err != nil {
			return ClientConfig{}, fmt.Errorf("%w: --output must be text or json", errUsage)
		}
		config.Output = output
	}
	if controls.JSON != nil {
		selected := pmapi.OutputText
		if *controls.JSON {
			selected = pmapi.OutputJSON
		}
		if controls.Output != nil && config.Output != selected {
			return ClientConfig{}, fmt.Errorf("%w: --output and --json conflict", errUsage)
		}
		config.Output = selected
	}
	if controls.Wait != nil {
		config.Wait = *controls.Wait
	}
	if controls.RequestTimeout != nil {
		duration, err := parseCanonicalDuration(*controls.RequestTimeout)
		if err != nil {
			return ClientConfig{}, fmt.Errorf("%w: --request-timeout is invalid", errUsage)
		}
		config.RequestTimeout = duration
	}
	if controls.SessionStore != nil {
		switch *controls.SessionStore {
		case string(pmapi.SessionStoreOS):
			config.SessionMode = pmapi.SessionStoreOS
		case string(pmapi.SessionStoreMemory):
			config.SessionMode = pmapi.SessionStoreMemory
		default:
			return ClientConfig{}, fmt.Errorf("%w: --session-store must be os or memory", errUsage)
		}
	}
	if controls.Workspace != nil {
		config.Workspace = *controls.Workspace
	}
	if controls.Connection != nil {
		config.Connection = *controls.Connection
	}
	return config, nil
}

type resolvedConfig struct {
	profile        pmapi.ServerProfile
	workspace      string
	connection     string
	output         pmapi.OutputFormat
	wait           bool
	requestTimeout time.Duration
	sessionMode    pmapi.SessionStoreMode
}

type commandRuntime struct {
	options Options
	flags   *rootFlags
}

type clientBundle struct {
	config resolvedConfig
	client *pmapi.QualifiedClient
	store  pmapi.SessionStore
}

var errUsage = errors.New("invalid command usage")

type singleString struct {
	value string
	set   bool
	name  string
}

func (value *singleString) Set(raw string) error {
	if value.set {
		return fmt.Errorf("%w: --%s may be specified only once", errUsage, value.name)
	}
	if raw == "" || len(raw) > maximumInputLength {
		return fmt.Errorf("%w: --%s requires one bounded non-empty value", errUsage, value.name)
	}
	value.value = raw
	value.set = true
	return nil
}
func (value *singleString) String() string { return value.value }
func (*singleString) Type() string         { return "string" }

type singleBool struct {
	value bool
	set   bool
	name  string
}

func (value *singleBool) Set(raw string) error {
	if value.set {
		return fmt.Errorf("%w: --%s may be specified only once", errUsage, value.name)
	}
	parsed, err := parseStrictBool(raw)
	if err != nil {
		return fmt.Errorf("%w: --%s requires true or false", errUsage, value.name)
	}
	value.value = parsed
	value.set = true
	return nil
}
func (value *singleBool) String() string { return strconv.FormatBool(value.value) }
func (*singleBool) Type() string         { return "bool" }
func (*singleBool) IsBoolFlag() bool     { return true }

func (runtime *commandRuntime) resolveConfig(requireWorkspace bool) (resolvedConfig, error) {
	profileName, err := runtime.profileName()
	if err != nil {
		return resolvedConfig{}, err
	}
	path := runtime.options.ProfilesPath
	if path == "" {
		path, err = pmapi.DefaultProfilesPath()
		if err != nil {
			return resolvedConfig{}, fmt.Errorf("%w: server profile path is unavailable", errUsage)
		}
	}
	profile, err := pmapi.LoadServerProfile(path, profileName)
	if err != nil {
		return resolvedConfig{}, fmt.Errorf("%w: selected server profile is unavailable", errUsage)
	}
	environment := make(map[string]string, 4)
	for _, name := range []string{"PM_OUTPUT", "PM_JSON", "PM_WAIT", "PM_REQUEST_TIMEOUT"} {
		if value, present := runtime.lookup(name); present {
			environment[name] = value
		}
	}
	var controls ClientControls
	if runtime.flags.output.set {
		controls.Output = &runtime.flags.output.value
	}
	if runtime.flags.json.set {
		controls.JSON = &runtime.flags.json.value
	}
	if runtime.flags.wait.set {
		controls.Wait = &runtime.flags.wait.value
	}
	if runtime.flags.requestTimeout.set {
		controls.RequestTimeout = &runtime.flags.requestTimeout.value
	}
	if runtime.flags.sessionStore.set {
		controls.SessionStore = &runtime.flags.sessionStore.value
	}
	if runtime.flags.workspace.set {
		controls.Workspace = &runtime.flags.workspace.value
	}
	if runtime.flags.connection.set {
		controls.Connection = &runtime.flags.connection.value
	}
	loaded, err := LoadClientConfig(profile, environment, controls)
	if err != nil {
		return resolvedConfig{}, err
	}
	if requireWorkspace && loaded.Workspace == "" {
		return resolvedConfig{}, fmt.Errorf("%w: --workspace is required for this command", errUsage)
	}
	return resolvedConfig{
		profile:        loaded.Profile,
		workspace:      loaded.Workspace,
		connection:     loaded.Connection,
		output:         loaded.Output,
		wait:           loaded.Wait,
		requestTimeout: loaded.RequestTimeout,
		sessionMode:    loaded.SessionMode,
	}, nil
}

func (runtime *commandRuntime) profileName() (string, error) {
	if runtime.flags.serverProfile.set {
		return runtime.flags.serverProfile.value, nil
	}
	if raw, present := runtime.lookup("PM_SERVER_PROFILE"); present {
		if raw == "" || len(raw) > maximumInputLength {
			return "", fmt.Errorf("%w: PM_SERVER_PROFILE must be non-empty", errUsage)
		}
		return raw, nil
	}
	return "", fmt.Errorf("%w: --server-profile or PM_SERVER_PROFILE is required", errUsage)
}

func (runtime *commandRuntime) requestBundle(command *cobra.Command, requireWorkspace, authenticated bool) (context.Context, context.CancelFunc, clientBundle, error) {
	config, err := runtime.resolveConfig(requireWorkspace)
	if err != nil {
		return nil, nil, clientBundle{}, err
	}
	ctx, cancel := context.WithTimeout(command.Context(), config.requestTimeout)
	bundle, err := runtime.clientForConfig(ctx, config, authenticated)
	if err != nil {
		cancel()
		return nil, nil, clientBundle{}, err
	}
	return ctx, cancel, bundle, nil
}

func (runtime *commandRuntime) clientForConfig(ctx context.Context, config resolvedConfig, authenticated bool) (clientBundle, error) {
	client, err := pmapi.NewQualifiedClient(ctx, config.profile, pmapi.QualifiedClientOptions{
		HTTPClient:     runtime.options.HTTPClient,
		RequestTimeout: config.requestTimeout,
	})
	if err != nil {
		return clientBundle{}, err
	}
	bundle := clientBundle{config: config, client: client}
	if !authenticated {
		return bundle, nil
	}
	factory := runtime.options.SessionStore
	if factory == nil {
		factory = pmapi.NewSessionStore
	}
	store, err := factory(config.sessionMode)
	if err != nil {
		return clientBundle{}, pmapi.ErrSessionStoreUnavailable
	}
	authenticatedClient, err := client.WithSession(ctx, store)
	if err != nil {
		return clientBundle{}, err
	}
	bundle.client = authenticatedClient
	bundle.store = store
	return bundle, nil
}

func (runtime *commandRuntime) recoveryStore() (pmapi.RecoveryStore, error) {
	if runtime.options.RecoveryStore != nil {
		return runtime.options.RecoveryStore, nil
	}
	path := runtime.options.ProfilesPath
	var err error
	if path == "" {
		path, err = pmapi.DefaultProfilesPath()
		if err != nil {
			return nil, pmapi.ErrRecoveryUnavailable
		}
	}
	directory, err := pmapi.DefaultRecoveryDirectory(path)
	if err != nil {
		return nil, err
	}
	return pmapi.NewFileRecoveryStore(directory)
}

func (runtime *commandRuntime) lookup(name string) (string, bool) {
	if runtime.options.LookupEnv == nil {
		return "", false
	}
	return runtime.options.LookupEnv(name)
}

func requestError(ctx context.Context, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if errors.Is(err, pmapi.ErrCompatibility) || errors.Is(err, pmapi.ErrInstanceMismatch) ||
		errors.Is(err, pmapi.ErrUnsafeClientRequest) || errors.Is(err, pmapi.ErrMalformedEnvelope) {
		return err
	}
	return pmapi.ErrClientTransport
}

func parseStrictBool(raw string) (bool, error) {
	switch raw {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, errUsage
	}
}

func parseCanonicalDuration(raw string) (time.Duration, error) {
	if raw == "" || len(raw) > 64 {
		return 0, errUsage
	}
	duration, err := time.ParseDuration(raw)
	if err != nil || duration.String() != raw || duration < time.Second || duration > 5*time.Minute {
		return 0, errUsage
	}
	return duration, nil
}

func addRootFlags(command *cobra.Command, flags *rootFlags) {
	flags.serverProfile.name = "server-profile"
	flags.workspace.name = "workspace"
	flags.connection.name = "connection"
	flags.output.name = "output"
	flags.json.name = "json"
	flags.wait.name = "wait"
	flags.requestTimeout.name = "request-timeout"
	flags.sessionStore.name = "session-store"
	command.PersistentFlags().Var(&flags.serverProfile, flags.serverProfile.name, "Select a named non-secret server profile")
	command.PersistentFlags().Var(&flags.workspace, flags.workspace.name, "Select an explicit workspace for this invocation")
	command.PersistentFlags().Var(&flags.connection, flags.connection.name, "Select an explicit connection for this invocation")
	command.PersistentFlags().Var(&flags.output, flags.output.name, "Render text or json output")
	command.PersistentFlags().Var(&flags.json, flags.json.name, "Select JSON output; --json=false explicitly selects text")
	command.PersistentFlags().Var(&flags.wait, flags.wait.name, "Wait for a terminal projection where the command supports it")
	command.PersistentFlags().Var(&flags.requestTimeout, flags.requestTimeout.name, "Bound each API view request")
	command.PersistentFlags().Var(&flags.sessionStore, flags.sessionStore.name, "Select os or explicit process-local memory PM session storage")
}
