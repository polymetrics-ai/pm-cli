package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/spf13/cobra"
)

type exitError struct {
	code    int
	message string
	cause   error
}

func (err *exitError) Error() string {
	if err == nil || err.message == "" {
		return "command failed"
	}
	return err.message
}
func (err *exitError) Unwrap() error { return err.cause }

func usageFailure(err error) error {
	if err == nil {
		err = errUsage
	}
	return &exitError{code: 2, message: err.Error(), cause: err}
}

func Execute(ctx context.Context, options Options) int {
	stderr := options.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	if err := preparseSelectors(options.Args); err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 2
	}

	command, runtime := newRoot(options)
	if options.Catalog == nil {
		if dynamicCommandCandidate(command, options.Args) {
			prepared, err := prepareDynamicCatalog(ctx, options)
			if err != nil {
				code, message := classifyCommandError(err)
				fmt.Fprintf(stderr, "error: %s\n", message)
				return code
			}
			runtime.options = prepared
			invoke := prepared.DynamicInvoke
			if invoke == nil {
				invoke = runtime.invokeDynamic
			}
			if err := BuildCommandTree(command, *prepared.Catalog, invoke); err != nil {
				command.PersistentPreRunE = func(*cobra.Command, []string) error { return err }
			}
		} else if offlineDocumentationCandidate(command, options.Args) {
			projectOfflineCatalog(command, options)
		}
	}
	if err := command.ExecuteContext(ctx); err != nil {
		code, message := classifyCommandError(err)
		fmt.Fprintf(stderr, "error: %s\n", message)
		return code
	}
	return 0
}
func noArgs(command *cobra.Command, arguments []string) error {
	if err := cobra.NoArgs(command, arguments); err != nil {
		return usageFailure(err)
	}
	return nil
}

func exactArgs(count int) cobra.PositionalArgs {
	return func(command *cobra.Command, arguments []string) error {
		if err := cobra.ExactArgs(count)(command, arguments); err != nil {
			return usageFailure(err)
		}
		return nil
	}
}

func NewRoot(options Options) *cobra.Command {
	root, _ := newRoot(options)
	return root
}

func newRoot(options Options) (*cobra.Command, *commandRuntime) {
	if options.Stdin == nil {
		options.Stdin = os.Stdin
	}
	if options.Stdout == nil {
		options.Stdout = io.Discard
	}
	if options.Stderr == nil {
		options.Stderr = io.Discard
	}
	flags := &rootFlags{}
	runtime := &commandRuntime{options: options, flags: flags}
	root := &cobra.Command{
		Use:           "pm",
		Short:         "PM API thin client",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return usageFailure(errors.New("a command is required"))
		},
	}
	root.SetArgs(append([]string(nil), options.Args...))
	root.SetIn(options.Stdin)
	root.SetOut(options.Stdout)
	root.SetErr(options.Stderr)
	root.CompletionOptions.DisableDefaultCmd = true
	root.DisableAutoGenTag = true
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageFailure(err) })
	addRootFlags(root, flags)
	root.AddCommand(newVersionCommand(runtime))
	root.AddCommand(newServerCommand(runtime))
	root.AddCommand(newSessionsCommand(runtime))
	root.AddCommand(newWorkspacesCommand(runtime))
	root.AddCommand(newConnectionsCommand(runtime))
	root.AddCommand(newConnectorsCommand(runtime))
	root.AddCommand(newCommandsCommand(runtime))
	root.AddCommand(newTasksCommand(runtime))
	root.AddCommand(NewRunsCommands(runtime))
	root.AddCommand(newArtifactsCommand(runtime))
	root.AddCommand(NewAccessCommands(runtime))
	root.AddCommand(newManualCommand(root))
	root.AddCommand(newCompletionCommand(root))
	if options.Catalog != nil {
		invoke := options.DynamicInvoke
		if invoke == nil {
			invoke = runtime.invokeDynamic
		}
		if err := BuildCommandTree(root, *options.Catalog, invoke); err != nil {
			root.PersistentPreRunE = func(*cobra.Command, []string) error { return err }
		}
	}
	return root, runtime
}

func newVersionCommand(runtime *commandRuntime) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version without reading settings or contacting a server",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			version := runtime.options.Version
			if version == "" {
				version = "devel"
			}
			_, err := fmt.Fprintln(command.OutOrStdout(), version)
			return err
		},
	}
}

func newServerCommand(runtime *commandRuntime) *cobra.Command {
	command := &cobra.Command{Use: "server", Short: "Inspect and configure PM servers", Args: noArgs}
	command.AddCommand(&cobra.Command{
		Use:   "info",
		Short: "Qualify and inspect the selected PM server",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			_, cancel, bundle, err := runtime.requestBundle(command, false, false)
			if err != nil {
				return err
			}
			defer cancel()
			document, err := json.Marshal(bundle.client.ServerInfo().Value)
			if err != nil {
				return err
			}
			return renderDocument(command.OutOrStdout(), bundle.config.output, document)
		},
	})
	command.AddCommand(newProfilesCommand(runtime))
	return command
}

func newProfilesCommand(runtime *commandRuntime) *cobra.Command {
	profiles := &cobra.Command{Use: "profiles", Short: "Manage non-secret server profiles", Args: noArgs}
	profiles.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List configured server profiles",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			path, err := runtime.profilesPath()
			if err != nil {
				return usageFailure(err)
			}
			values, err := pmapi.ListServerProfiles(path)
			if err != nil {
				return usageFailure(err)
			}
			return renderProfiles(command.OutOrStdout(), values)
		},
	})
	profiles.AddCommand(newProfileAddCommand(runtime))
	profiles.AddCommand(&cobra.Command{
		Use:   "remove NAME",
		Short: "Remove a server profile without touching its OS session",
		Args:  exactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			path, err := runtime.profilesPath()
			if err != nil {
				return usageFailure(err)
			}
			if err := pmapi.RemoveServerProfile(path, arguments[0]); err != nil {
				return usageFailure(err)
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), arguments[0])
			return err
		},
	})
	return profiles
}

func newProfileAddCommand(runtime *commandRuntime) *cobra.Command {
	var serverURL, instanceID, handle, defaultOutput, defaultTimeout singleString
	var defaultWait singleBool
	serverURL.name = "url"
	instanceID.name = "instance-id"
	handle.name = "session-handle"
	defaultOutput.name = "default-output"
	defaultTimeout.name = "default-request-timeout"
	defaultWait.name = "default-wait"
	command := &cobra.Command{
		Use:   "add NAME",
		Short: "Add one strict non-secret server profile",
		Args:  exactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			if !serverURL.set || !instanceID.set {
				return usageFailure(errors.New("--url and --instance-id are required"))
			}
			profile := pmapi.ServerProfile{Name: arguments[0], ServerURL: serverURL.value, InstanceID: instanceID.value}
			if handle.set {
				profile.SessionHandle = handle.value
			} else {
				value, err := pmapi.DefaultSessionHandle(profile.Name, profile.InstanceID)
				if err != nil {
					return usageFailure(err)
				}
				profile.SessionHandle = value
			}
			if defaultOutput.set {
				value, err := pmapi.ParseOutputFormat(defaultOutput.value)
				if err != nil {
					return usageFailure(err)
				}
				profile.Output = &value
			}
			if defaultWait.set {
				value := defaultWait.value
				profile.Wait = &value
			}
			if defaultTimeout.set {
				value, err := parseCanonicalDuration(defaultTimeout.value)
				if err != nil {
					return usageFailure(err)
				}
				profile.RequestTimeout = &value
			}
			path, err := runtime.profilesPath()
			if err != nil {
				return usageFailure(err)
			}
			if err := pmapi.AddServerProfile(path, profile); err != nil {
				return usageFailure(err)
			}
			_, err = fmt.Fprintln(command.OutOrStdout(), profile.Name)
			return err
		},
	}
	command.Flags().Var(&serverURL, serverURL.name, "HTTPS server origin")
	command.Flags().Var(&instanceID, instanceID.name, "Expected immutable server instance ID")
	command.Flags().Var(&handle, handle.name, "Non-secret PM session handle")
	command.Flags().Var(&defaultOutput, defaultOutput.name, "Profile text or json default")
	command.Flags().Var(&defaultWait, defaultWait.name, "Profile wait default")
	command.Flags().Var(&defaultTimeout, defaultTimeout.name, "Profile request timeout default")
	return command
}

func (runtime *commandRuntime) profilesPath() (string, error) {
	if runtime.options.ProfilesPath != "" {
		return runtime.options.ProfilesPath, nil
	}
	return pmapi.DefaultProfilesPath()
}

func classifyCommandError(err error) (int, string) {
	var explicit *exitError
	if errors.As(err, &explicit) {
		return explicit.code, explicit.Error()
	}
	var problem *pmapi.ProblemError
	if errors.As(err, &problem) {
		return problem.ExitCode(), problem.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 8, "view deadline exceeded"
	}
	if errors.Is(err, errUsage) || errors.Is(err, pmapi.ErrProfileNotFound) {
		return 2, err.Error()
	}
	if errors.Is(err, pmapi.ErrSessionRequired) || errors.Is(err, pmapi.ErrSessionNotFound) {
		return 3, "PM session is required"
	}
	if errors.Is(err, pmapi.ErrRecoveryConflict) {
		return 4, "request key already identifies another intent"
	}
	if errors.Is(err, pmapi.ErrClientTransport) || errors.Is(err, pmapi.ErrSessionStoreUnavailable) ||
		errors.Is(err, pmapi.ErrRecoveryUnavailable) {
		return 5, "PM dependency is unavailable"
	}
	if errors.Is(err, pmapi.ErrMutationOutcomeUnknown) {
		return 7, "PM mutation outcome is unknown; reconcile the original request key"
	}
	if errors.Is(err, pmapi.ErrCompatibility) || errors.Is(err, pmapi.ErrInstanceMismatch) {
		return 6, "PM server qualification failed"
	}
	return 6, "PM command failed"
}
