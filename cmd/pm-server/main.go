package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
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
	listenFlag := &listenDescriptorFlag{}
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
			// The --listen-fd value is checked after the flags are read, with
			// fixed texts, before the check of each descriptor (ADR 0008).
			listen, err := listenFlag.descriptor()
			if err != nil {
				return err
			}
			if listen != 0 && readyFlag.present && listen == readyFlag.descriptor {
				return errors.New("--listen-fd and --ready-fd require different descriptors")
			}
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
			return serve(ctx, configurationPath, secretsDirectory, stderr, ready, listen)
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
	command.Flags().Var(listenFlag, "listen-fd", "serve on inherited listening socket fd (3 or more) at the configured server.listen")
	return command.ExecuteContext(ctx)
}

// serve gives a non-nil ready handle to the lifecycle, which writes the ready
// line and closes the handle. serve writes nothing to the handle itself. It
// closes the handle when the start fails before the lifecycle owns it.
//
// A listenDescriptor of 0 means no --listen-fd: serve binds server.listen
// itself. Otherwise serve checks that descriptor after the configuration
// check, which gives the address, and before the application opens, and it
// serves on it (ADR 0008). A failed check stops the start: serve never binds
// server.listen in place of the inherited socket. serve closes the listener
// when the start fails before the lifecycle owns it.
func serve(ctx context.Context, configurationPath, secretsDirectory string, diagnostics io.Writer, ready io.WriteCloser, listenDescriptor int) (result error) {
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
	var listener net.Listener
	defer func() {
		if listener != nil {
			_ = listener.Close()
		}
	}()
	if listenDescriptor != 0 {
		listener, err = inheritListenDescriptor(listenDescriptor, configuration.Server().Listen)
		if err != nil {
			return err
		}
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

	if listenDescriptor == 0 {
		listener, err = net.Listen("tcp", configuration.Server().Listen)
		if err != nil {
			return errors.New("configured server listener is unavailable")
		}
	}
	serving, handle := listener, ready
	listener, ready = nil, nil
	return application.Serve(ctx, serving, handle)
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
	descriptor, ok := parseDescriptor(value)
	if !ok {
		return errors.New("--ready-fd requires only the digits 0-9, with no leading zero, for a number from 3 to the int maximum")
	}
	flag.descriptor, flag.text = descriptor, value
	return nil
}

func (*readyDescriptorFlag) Type() string {
	return "fd"
}

// listenDescriptorFlag is --listen-fd (ADR 0008). Set records the count and
// the first raw value and cannot fail, so the flag parser never repeats a
// value in an error. descriptor checks the value after the flags are read,
// with fixed texts that contain no value. The number rules are the rules of
// --ready-fd.
type listenDescriptorFlag struct {
	count int
	raw   string
}

// String gives no raw value, so that no help or error text can repeat one.
func (*listenDescriptorFlag) String() string {
	return ""
}

func (flag *listenDescriptorFlag) Set(value string) error {
	flag.count++
	if flag.count == 1 {
		flag.raw = value
	}
	return nil
}

func (*listenDescriptorFlag) Type() string {
	return "fd"
}

// descriptor gives the checked descriptor, or 0 without the flag.
func (flag *listenDescriptorFlag) descriptor() (int, error) {
	switch {
	case flag.count == 0:
		return 0, nil
	case flag.count > 1:
		return 0, errors.New("--listen-fd may be supplied exactly once")
	}
	descriptor, ok := parseDescriptor(flag.raw)
	if !ok {
		return 0, errors.New("--listen-fd requires only the digits 0-9, with no leading zero, for a number from 3 to the int maximum")
	}
	return descriptor, nil
}

func parseDescriptor(value string) (int, bool) {
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
// gets close-on-exec, so that no child process holds it, and non-blocking
// mode, so that Serve can end a ready write that still waits when it stops
// serving. The process makes no other copy of the descriptor.
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
	// os.NewFile makes a pollable handle from a non-blocking descriptor, so a
	// Close ends a write that still waits.
	if err := syscall.SetNonblock(fd, true); err != nil {
		return nil, refused
	}
	return os.NewFile(uintptr(fd), "ready-fd"), nil
}

// inheritListenDescriptor checks descriptor fd in the order of ADR 0008 and
// returns the one listener of its socket.
//
//  1. The descriptor is open and has no close-on-exec flag. Go opens each
//     descriptor of its own with close-on-exec, so a descriptor with the flag
//     belongs to this process and is not inherited.
//  2. It is a stream socket in the family of the configured address: AF_INET
//     for an IPv4 address, AF_INET6 for an IPv6 address.
//  3. It uses TCP.
//  4. It listens for connections.
//  5. Its local address equals listen, the configured server.listen: the same
//     family, IP address and port (netip.AddrPort). An IPv4-mapped address is
//     not an IPv4 address, and a wildcard equals only the same wildcard.
//
// A descriptor that fails check 1 is not the inherited descriptor, so the
// check leaves it open. A descriptor that fails a later check is inherited,
// so the check closes it. After the checks the descriptor gets close-on-exec,
// so that no child process holds it. net.FileListener makes the listener from
// a copy of the descriptor that has close-on-exec, and the check closes the
// descriptor itself, so that the listener is the one handle of the socket.
func inheritListenDescriptor(fd int, listen string) (net.Listener, error) {
	refused := errors.New("--listen-fd requires an inherited listener at the configured server address")
	descriptorFlags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFD), 0)
	if errno != 0 || descriptorFlags&syscall.FD_CLOEXEC != 0 {
		return nil, refused
	}
	if !listenerMatches(fd, listen) {
		_ = syscall.Close(fd)
		return nil, refused
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_SETFD), syscall.FD_CLOEXEC); errno != 0 {
		_ = syscall.Close(fd)
		return nil, refused
	}
	file := os.NewFile(uintptr(fd), "listen-fd")
	listener, err := net.FileListener(file)
	_ = file.Close()
	if err != nil {
		return nil, refused
	}
	return listener, nil
}

// listenerMatches runs checks 2 to 5 of inheritListenDescriptor.
func listenerMatches(fd int, listen string) bool {
	configured, err := netip.ParseAddrPort(listen)
	if err != nil {
		return false
	}
	family := syscall.AF_INET6
	if configured.Addr().Is4() {
		family = syscall.AF_INET
	}
	if kind, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE); err != nil || kind != syscall.SOCK_STREAM {
		return false
	}
	local, err := syscall.Getsockname(fd)
	if err != nil {
		return false
	}
	address, localFamily, ok := socketAddress(local)
	if !ok || localFamily != family {
		return false
	}
	if !tcpListens(fd) {
		return false
	}
	return address == configured
}

// socketAddress gives the address and the family of an IPv4 or IPv6 socket.
// A zone never equals a configured address, because server.listen has no
// zone.
func socketAddress(local syscall.Sockaddr) (netip.AddrPort, int, bool) {
	switch address := local.(type) {
	case *syscall.SockaddrInet4:
		return netip.AddrPortFrom(netip.AddrFrom4(address.Addr), uint16(address.Port)), syscall.AF_INET, true
	case *syscall.SockaddrInet6:
		if address.ZoneId != 0 {
			return netip.AddrPort{}, 0, false
		}
		return netip.AddrPortFrom(netip.AddrFrom16(address.Addr), uint16(address.Port)), syscall.AF_INET6, true
	}
	return netip.AddrPort{}, 0, false
}

type nilReader struct{}

func (nilReader) Read([]byte) (int, error) {
	return 0, io.EOF
}
