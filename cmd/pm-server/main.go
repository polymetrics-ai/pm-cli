package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/server"
	"github.com/spf13/cobra"
)

var version = "development"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "pm-server: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if ctx == nil {
		return errors.New("server context is required")
	}
	if stdin == nil {
		stdin = nilReader{}
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}

	var configurationPath string
	var secretsDirectory string
	configurationFlag := &singleStringFlag{name: "config", destination: &configurationPath}
	secretsFlag := &singleStringFlag{name: "secrets-directory", destination: &secretsDirectory}
	command := &cobra.Command{
		Use:                   "pm-server",
		Short:                 "Serve the authenticated PM API",
		Args:                  cobra.NoArgs,
		SilenceErrors:         true,
		SilenceUsage:          true,
		DisableSuggestions:    true,
		DisableFlagsInUseLine: true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
		Version: version,
		RunE: func(*cobra.Command, []string) error {
			if !configurationFlag.present || configurationPath == "" {
				return errors.New("--config requires exactly one nonempty path")
			}
			if !secretsFlag.present || secretsDirectory == "" {
				return errors.New("--secrets-directory requires exactly one nonempty path")
			}
			return serve(ctx, configurationPath, secretsDirectory, stderr)
		},
	}
	command.SetIn(stdin)
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.SetArgs(args)
	command.SetVersionTemplate("pm-server {{.Version}}\n")
	command.Flags().Var(configurationFlag, "config", "absolute or operator-selected PM Server configuration file")
	command.Flags().Var(secretsFlag, "secrets-directory", "descriptor-confined operator secret bundle root")
	return command.ExecuteContext(ctx)
}

func serve(ctx context.Context, configurationPath, secretsDirectory string, diagnostics io.Writer) (result error) {
	file, err := os.Open(configurationPath)
	if err != nil {
		return errors.New("server configuration is unavailable")
	}
	defer file.Close()

	document, err := config.Load(file)
	if err != nil {
		return err
	}
	configuration, err := config.Validate(document, config.Prelaunch{})
	if err != nil {
		return err
	}
	application, err := server.Open(ctx, configuration, secretsDirectory, diagnostics)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := application.Close()
		if result == nil {
			result = closeErr
		} else if closeErr != nil && !errors.Is(closeErr, result) {
			result = errors.Join(result, closeErr)
		}
	}()

	listener, err := net.Listen("tcp", configuration.Server().Listen)
	if err != nil {
		return errors.New("configured server listener is unavailable")
	}
	return application.Serve(ctx, listener)
}

type singleStringFlag struct {
	name        string
	destination *string
	present     bool
}

func (flag *singleStringFlag) String() string {
	if flag == nil || flag.destination == nil {
		return ""
	}
	return *flag.destination
}

func (flag *singleStringFlag) Set(value string) error {
	if flag.present {
		return fmt.Errorf("--%s may be supplied exactly once", flag.name)
	}
	flag.present = true
	*flag.destination = value
	return nil
}

func (*singleStringFlag) Type() string {
	return "path"
}

type nilReader struct{}

func (nilReader) Read([]byte) (int, error) {
	return 0, io.EOF
}
