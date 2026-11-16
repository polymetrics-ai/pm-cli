package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"syscall"

	workflowservice "go.temporal.io/api/workflowservice/v1"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/polymetrics-ai/pm-cli/internal/activities"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/config"
	pmruntime "github.com/polymetrics-ai/pm-cli/internal/runtime"
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
	readyFlag := &readyDescriptorFlag{}
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
			var ready *readySignal
			if readyFlag.present {
				handle, err := openReadyDescriptor(readyFlag.descriptor)
				if err != nil {
					return err
				}
				ready = &readySignal{handle: handle, stderr: stderr}
			}
			return serve(ctx, configurationPath, secretsDirectory, ready)
		},
	}
	command.SetIn(stdin)
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.SetArgs(args)
	command.SetVersionTemplate("pm-worker {{.Version}}\n")
	command.Flags().Var(configurationFlag, "config", "absolute or operator-selected PM worker configuration file")
	command.Flags().Var(secretsFlag, "secrets-directory", "descriptor-confined operator secret bundle root")
	command.Flags().Var(readyFlag, "ready-fd", "write one ready line to inherited descriptor fd (3 or more), then close it")
	return command.ExecuteContext(ctx)
}

// serve owns the ready handle from its start. It closes the handle on each
// path where no ready write started.
func serve(ctx context.Context, configurationPath, secretsDirectory string, ready *readySignal) (result error) {
	defer ready.discard()
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
		ClientIdentity:   pmruntime.ProductClientIdentity(version),
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

	// The dial and the own calls of ready step 1 finish within the dial
	// timeout, counted from the start of the dial.
	temporalConfiguration := configuration.Temporal()
	startContext, cancelStart := context.WithTimeout(ctx, workerConfiguration.DialTimeout)
	defer cancelStart()
	client, err := pmtemporal.DialWorkerClient(startContext, sdkclient.Options{
		HostPort:    temporalConfiguration.Endpoint,
		Namespace:   temporalConfiguration.Namespace,
		Identity:    configuration.DeploymentID() + "/pm-worker",
		Credentials: sdkclient.NewMTLSCredentials(clientCertificate),
		ConnectionOptions: sdkclient.ConnectionOptions{
			TLS: connectionTLS,
		},
	}, workerConfiguration.DialTimeout)
	if err != nil {
		return errTemporalDependency
	}
	defer client.Close()
	if err := checkTemporalService(startContext, client.WorkflowService(), temporalConfiguration.Namespace); err != nil {
		return err
	}
	cancelStart()

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
	// The SDK calls OnFatalError at most once and then stops the worker
	// itself. The channel keeps the error for the run path.
	fatal := make(chan error, 1)
	sdkWorker := worker.New(client, workerConfiguration.TaskQueue, worker.Options{
		MaxConcurrentActivityExecutionSize:     int(workerConfiguration.MaxConcurrentActivities),
		MaxConcurrentWorkflowTaskExecutionSize: int(workerConfiguration.MaxConcurrentWorkflowTasks),
		WorkerStopTimeout:                      workerConfiguration.DrainTimeout,
		OnFatalError: func(err error) {
			select {
			case fatal <- err:
			default:
			}
		},
	})
	if err := workflows.RegisterWorkflows(sdkWorker, coordinator, service.SupportsScheduledAdmission()); err != nil {
		return err
	}
	if err := activities.RegisterActivities(sdkWorker, activityService); err != nil {
		return err
	}
	return runWorker(ctx, sdkWorker, fatal, ready)
}

var (
	// errTemporalDependency is the text of a failed dial and of a failed own
	// GetSystemInfo call.
	errTemporalDependency = errors.New("Temporal worker dependency is unavailable")
	// errTemporalNamespace is the text of a failed own DescribeNamespace call,
	// and of an answer that names another namespace.
	errTemporalNamespace = errors.New("Temporal namespace is unavailable")
	errWorkerFatal       = errors.New("Temporal worker stopped after a fatal runtime error")
)

// checkTemporalService makes the own calls of ready step 1 (ADR 0005):
// GetSystemInfo once, then DescribeNamespace once for the configured
// namespace. The calls carry no call options and use the context of the
// caller unchanged, so they carry no retry options. Only a success answer
// passes, and the DescribeNamespace answer must name the namespace. Each
// other result, also "not implemented", stops the start. The failure text
// depends on the call, not on the status code.
func checkTemporalService(ctx context.Context, service workflowservice.WorkflowServiceClient, namespace string) error {
	info, err := service.GetSystemInfo(ctx, &workflowservice.GetSystemInfoRequest{})
	if err != nil || info == nil {
		return errTemporalDependency
	}
	described, err := service.DescribeNamespace(ctx, &workflowservice.DescribeNamespaceRequest{Namespace: namespace})
	if err != nil || described.GetNamespaceInfo().GetName() != namespace {
		return errTemporalNamespace
	}
	return nil
}

// workerLifecycle is the part of the Temporal worker that the run path uses.
type workerLifecycle interface {
	Start() error
	Stop()
}

// runWorker starts the worker. After Start returned, and only while the
// context is not done, it starts the one ready write apart from the run path.
// Then it waits for the context or for a fatal worker error. A ready write
// that cannot complete blocks neither the work nor the stop.
//
// When the context ends, runWorker calls Stop, so WorkerStopTimeout applies.
// After a fatal error, the SDK calls Stop itself right after OnFatalError
// returns, and Stop has no lock. So runWorker makes no second Stop call on
// that path, as the SDK Run call does not.
func runWorker(ctx context.Context, sdkWorker workerLifecycle, fatal <-chan error, ready *readySignal) error {
	defer ready.discard()
	if err := sdkWorker.Start(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return errWorkerFatal
	}
	if ctx.Err() == nil {
		ready.send()
	}
	select {
	case <-ctx.Done():
		sdkWorker.Stop()
		return nil
	case <-fatal:
		if ctx.Err() != nil {
			return nil
		}
		return errWorkerFatal
	}
}

const (
	// readyLine is the ready signal of pm-worker.
	readyLine = "pm-worker ready\n"
	// readyWriteFailure is the one stderr line of a failed ready write.
	readyWriteFailure = "pm-worker: the ready signal write failed. The process continues.\n"
)

// readySignal owns the one handle of --ready-fd. The process uses only this
// handle and closes it on every path.
type readySignal struct {
	handle io.WriteCloser
	stderr io.Writer
}

// send starts the one write of the ready line in its own goroutine. From
// here the goroutine owns the handle. The write has no deadline, because the
// signal waits until the starter reads (ADR 0005).
func (ready *readySignal) send() {
	if ready == nil || ready.handle == nil {
		return
	}
	handle := ready.handle
	ready.handle = nil
	go writeReadyLine(handle, ready.stderr)
}

// discard closes the handle when no ready write started.
func (ready *readySignal) discard() {
	if ready == nil || ready.handle == nil {
		return
	}
	handle := ready.handle
	ready.handle = nil
	_ = handle.Close()
}

// writeReadyLine writes the ready line in one write and closes the handle.
// A failed write prints its line on stderr once, and the process continues.
func writeReadyLine(handle io.WriteCloser, stderr io.Writer) {
	if _, err := io.WriteString(handle, readyLine); err != nil {
		_, _ = io.WriteString(stderr, readyWriteFailure)
	}
	_ = handle.Close()
}

// errReadyDescriptor refuses a --ready-fd descriptor that the process cannot
// use for the ready signal.
var errReadyDescriptor = errors.New("--ready-fd requires an inherited descriptor that is open for writing")

// openReadyDescriptor checks the descriptor of --ready-fd before the process
// opens its configuration. Go opens each descriptor of its own with
// close-on-exec, and a descriptor that a starter passes arrives without it. So
// a descriptor with close-on-exec at the check belongs to this process, and
// the check refuses it. The check also refuses a descriptor that is not open
// for writing. Then it marks the descriptor close-on-exec, so that a child
// process does not hold it, and puts it in the one file handle.
func openReadyDescriptor(descriptor int) (*os.File, error) {
	descriptorFlags, err := fcntl(descriptor, syscall.F_GETFD, 0)
	if err != nil || descriptorFlags&syscall.FD_CLOEXEC != 0 {
		return nil, errReadyDescriptor
	}
	statusFlags, err := fcntl(descriptor, syscall.F_GETFL, 0)
	if err != nil {
		return nil, errReadyDescriptor
	}
	switch statusFlags & syscall.O_ACCMODE {
	case syscall.O_WRONLY, syscall.O_RDWR:
	default:
		return nil, errReadyDescriptor
	}
	if _, err := fcntl(descriptor, syscall.F_SETFD, descriptorFlags|syscall.FD_CLOEXEC); err != nil {
		return nil, errReadyDescriptor
	}
	return os.NewFile(uintptr(descriptor), "ready-fd"), nil
}

func fcntl(descriptor, command, argument int) (int, error) {
	value, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(descriptor), uintptr(command), uintptr(argument))
	if errno != 0 {
		return 0, errno
	}
	return int(value), nil
}

// readyDescriptorFlag is --ready-fd. The value has only ASCII digits, has no
// leading zero, fits in a 32-bit signed int and is 3 or more. The flag may
// appear once, and the check of a second flag comes before the check of its
// value. The flag parser adds its "invalid argument" prefix to each error.
type readyDescriptorFlag struct {
	descriptor int
	present    bool
}

const readyDescriptorRule = "--ready-fd requires only the digits 0-9, with no leading zero, for a number from 3 to the int maximum"

func (flag *readyDescriptorFlag) String() string {
	if flag == nil || !flag.present {
		return ""
	}
	return fmt.Sprint(flag.descriptor)
}

func (flag *readyDescriptorFlag) Set(value string) error {
	if flag.present {
		return errors.New("--ready-fd may be supplied exactly once")
	}
	flag.present = true
	descriptor, ok := parseReadyDescriptor(value)
	if !ok {
		return errors.New(readyDescriptorRule)
	}
	flag.descriptor = descriptor
	return nil
}

func (*readyDescriptorFlag) Type() string { return "fd" }

// parseReadyDescriptor applies the 32-bit rule itself, because Go int has 64
// bits on the runners.
func parseReadyDescriptor(value string) (int, bool) {
	if value == "" || len(value) > 10 || value[0] == '0' {
		return 0, false
	}
	var number int64
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character < '0' || character > '9' {
			return 0, false
		}
		number = number*10 + int64(character-'0')
	}
	if number < 3 || number > math.MaxInt32 {
		return 0, false
	}
	return int(number), true
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
