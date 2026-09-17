package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/polymetrics-ai/pm-cli/internal/activities"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/config"
	pmtemporal "github.com/polymetrics-ai/pm-cli/internal/temporal"
	"github.com/polymetrics-ai/pm-cli/internal/workflows"
	"github.com/spf13/cobra"
)

var version = "development"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "pm-worker: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if ctx == nil {
		return errors.New("worker context is required")
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
		Use:                   "pm-worker",
		Short:                 "Run the durable PM workflow and activity worker",
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
			return serve(ctx, configurationPath, secretsDirectory)
		},
	}
	command.SetIn(stdin)
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.SetArgs(args)
	command.SetVersionTemplate("pm-worker {{.Version}}\n")
	command.Flags().Var(configurationFlag, "config", "absolute or operator-selected PM worker configuration file")
	command.Flags().Var(secretsFlag, "secrets-directory", "descriptor-confined operator secret bundle root")
	return command.ExecuteContext(ctx)
}

func serve(ctx context.Context, configurationPath, secretsDirectory string) (result error) {
	file, err := os.Open(configurationPath)
	if err != nil {
		return errors.New("worker configuration is unavailable")
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
	workerConfiguration := configuration.Worker()
	if !workerConfiguration.Configured() {
		return errors.New("worker configuration is required")
	}

	runtime, err := app.OpenWorkerRuntime(ctx, app.WorkerRuntimeConfig{
		Configuration:    configuration,
		SecretsDirectory: secretsDirectory,
	})
	if err != nil {
		return err
	}
	defer func() {
		closeErr := runtime.Close()
		if result == nil {
			result = closeErr
		} else if closeErr != nil && !errors.Is(closeErr, result) {
			result = errors.Join(result, closeErr)
		}
	}()

	if runtime.TemporalTLS == nil || len(runtime.TemporalTLS.Certificates) != 1 {
		return errors.New("Temporal worker identity is unavailable")
	}
	clientCertificate := runtime.TemporalTLS.Certificates[0]
	connectionTLS := runtime.TemporalTLS.Clone()
	connectionTLS.Certificates = nil

	temporalConfiguration := configuration.Temporal()
	client, err := pmtemporal.DialWorkerClient(ctx, sdkclient.Options{
		HostPort:    temporalConfiguration.Endpoint,
		Namespace:   temporalConfiguration.Namespace,
		Identity:    configuration.DeploymentID() + "/pm-worker",
		Credentials: sdkclient.NewMTLSCredentials(clientCertificate),
		ConnectionOptions: sdkclient.ConnectionOptions{
			TLS: connectionTLS,
		},
	}, workerConfiguration.DialTimeout)
	if err != nil {
		return errors.New("Temporal worker dependency is unavailable")
	}
	defer client.Close()

	observer, err := pmtemporal.NewWorkflowContinuationObserver(
		client,
		temporalConfiguration.Namespace,
		workerConfiguration.ActivityTimeouts.Transport,
	)
	if err != nil {
		return errors.New("Temporal continuation observer is unavailable")
	}
	service, err := runtime.NewService(observer)
	if err != nil {
		return err
	}

	activityService, err := activities.New(service)
	if err != nil {
		return err
	}
	coordinator, err := workflows.NewCoordinator(workerConfiguration.ActivityTimeouts)
	if err != nil {
		return err
	}
	sdkWorker := worker.New(client, workerConfiguration.TaskQueue, worker.Options{
		MaxConcurrentActivityExecutionSize:     int(workerConfiguration.MaxConcurrentActivities),
		MaxConcurrentWorkflowTaskExecutionSize: int(workerConfiguration.MaxConcurrentWorkflowTasks),
		WorkerStopTimeout:                      workerConfiguration.DrainTimeout,
	})
	if err := workflows.RegisterWorkflows(sdkWorker, coordinator, service.SupportsScheduledAdmission()); err != nil {
		return err
	}
	if err := activities.RegisterActivities(sdkWorker, activityService); err != nil {
		return err
	}
	interrupt := make(chan interface{}, 1)
	waiterDone := make(chan struct{})
	defer close(waiterDone)
	go func() {
		select {
		case <-ctx.Done():
			interrupt <- struct{}{}
		case <-waiterDone:
		}
	}()
	if err := sdkWorker.Run(interrupt); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return errors.New("Temporal worker stopped after a fatal runtime error")
	}
	return nil
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

func (*singleStringFlag) Type() string { return "path" }

type nilReader struct{}

func (nilReader) Read([]byte) (int, error) { return 0, io.EOF }
