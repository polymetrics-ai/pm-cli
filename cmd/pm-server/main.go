package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/polymetrics-ai/pm-cli/internal/config"
	pmruntime "github.com/polymetrics-ai/pm-cli/internal/runtime"
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
	readyFlag := &readyDescriptorFlag{}
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
			// The ready descriptor check comes before the configuration opens
			// and before any code that can start a child process (ADR 0005).
			var ready io.WriteCloser
			if readyFlag.present {
				handle, err := inheritReadyDescriptor(readyFlag.descriptor)
				if err != nil {
					return err
				}
				ready = handle
			}
			return serve(ctx, configurationPath, secretsDirectory, stderr, ready)
		},
	}
	command.SetIn(stdin)
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.SetArgs(args)
	command.SetVersionTemplate("pm-server {{.Version}}\n")
	command.Flags().Var(configurationFlag, "config", "absolute or operator-selected PM Server configuration file")
	command.Flags().Var(secretsFlag, "secrets-directory", "descriptor-confined operator secret bundle root")
	command.Flags().Var(readyFlag, "ready-fd", "write one ready line to inherited descriptor fd (3 or more), then close it")
	return command.ExecuteContext(ctx)
}

// serve gives a non-nil ready handle to the lifecycle, which writes the ready
// line and closes the handle. serve writes nothing to the handle itself. It
// closes the handle when the start fails before the lifecycle owns it.
func serve(ctx context.Context, configurationPath, secretsDirectory string, diagnostics io.Writer, ready io.WriteCloser) (result error) {
	defer func() {
		if ready != nil {
			_ = ready.Close()
		}
	}()
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
	application, err := server.Open(ctx, configuration, secretsDirectory, diagnostics, pmruntime.ProductClientIdentity(version))
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
	handle := ready
	ready = nil
	return application.Serve(ctx, listener, handle)
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

// readyDescriptorFlag is --ready-fd. The value has only ASCII decimal digits,
// has no leading zero, fits in a 32-bit signed int and is 3 or more. The flag
// may appear once. The check of a second flag comes before the check of its
// value. The flag parser adds the "invalid argument" prefix to each error.
type readyDescriptorFlag struct {
	descriptor int
	text       string
	present    bool
}

func (flag *readyDescriptorFlag) String() string {
	if flag == nil {
		return ""
	}
	return flag.text
}

func (flag *readyDescriptorFlag) Set(value string) error {
	if flag.present {
		return errors.New("--ready-fd may be supplied exactly once")
	}
	flag.present = true
	descriptor, ok := parseReadyDescriptor(value)
	if !ok {
		return errors.New("--ready-fd requires only the digits 0-9, with no leading zero, for a number from 3 to the int maximum")
	}
	flag.descriptor, flag.text = descriptor, value
	return nil
}

func (*readyDescriptorFlag) Type() string {
	return "fd"
}

func parseReadyDescriptor(value string) (int, bool) {
	if value == "" || value[0] == '0' {
		return 0, false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return 0, false
		}
	}
	number, err := strconv.ParseInt(value, 10, 32)
	if err != nil || number < 3 {
		return 0, false
	}
	return int(number), true
}

// inheritReadyDescriptor checks descriptor fd and keeps it in one handle. The
// descriptor must be open for writing and must not have close-on-exec set: Go
// opens each descriptor of its own with close-on-exec, so a descriptor with
// the flag belongs to this process and not to the starter. The handle then
// gets close-on-exec, so that no child process holds it. The process makes
// no other copy of the descriptor.
func inheritReadyDescriptor(fd int) (*os.File, error) {
	refused := errors.New("--ready-fd requires an inherited descriptor that is open for writing")
	descriptorFlags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFD), 0)
	if errno != 0 || descriptorFlags&syscall.FD_CLOEXEC != 0 {
		return nil, refused
	}
	statusFlags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFL), 0)
	if errno != 0 {
		return nil, refused
	}
	if mode := statusFlags & syscall.O_ACCMODE; mode != syscall.O_WRONLY && mode != syscall.O_RDWR {
		return nil, refused
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_SETFD), syscall.FD_CLOEXEC); errno != 0 {
		return nil, refused
	}
	return os.NewFile(uintptr(fd), "ready-fd"), nil
}

type nilReader struct{}

func (nilReader) Read([]byte) (int, error) {
	return 0, io.EOF
}
