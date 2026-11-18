package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// listenRefusal is the refusal text of each descriptor that fails a check of
// --listen-fd (ADR 0008, "Refusals", order 3).
const listenRefusal = "--listen-fd requires an inherited listener at the configured server address"

// notOpen is a valid flag value that names no open descriptor.
const notOpen = "2147483647"

// sentinel stands for private text that a command line carries by mistake.
// No output may contain it (ADR 0008, "The value"; criteria LF4 and D2).
const sentinel = "LF_PRIVATE_TEXT_SENTINEL"

// Go developer test, not acceptance proof (ADR 0008, "Refusals", order 1 and
// 2; criteria D1 to D5 and O-LF1). Defect: pm-server has no --listen-fd flag,
// accepts a second flag or a value that breaks the number rules, accepts the
// same descriptor as --ready-fd, or repeats a raw value in a refusal. Or the
// value checks run before the --config and --secrets-directory checks, or
// after the configuration opens. The configuration path and the secret
// directory do not exist, so a value check after the configuration open gives
// another refusal. The equal descriptors are not open, so that refusal must
// come before the check of each descriptor. Each case runs the command of the
// binary through run and requires the fixed text, no output and no sentinel.
func TestListenDescriptorFlagRefusals(t *testing.T) {
	missing := t.TempDir()
	paths := []string{"--config", filepath.Join(missing, "pm-deployment.json"), "--secrets-directory", filepath.Join(missing, "secrets")}
	const once = "--listen-fd may be supplied exactly once"
	const numberRule = "--listen-fd requires only the digits 0-9, with no leading zero, for a number from 3 to the int maximum"
	type refusal struct {
		name string
		args []string
		want string
	}
	cases := []refusal{
		{"D1 no value", withArguments(paths, "--listen-fd"), "flag needs an argument: --listen-fd"},
		{"D4 a second flag", withArguments(paths, "--listen-fd", "4", "--listen-fd", "5"), once},
		{"D4 a second flag with the sentinel", withArguments(paths, "--listen-fd", "5", "--listen-fd", sentinel), once},
		{"D4 the sentinel and a second flag", withArguments(paths, "--listen-fd", sentinel, "--listen-fd", "5"), once},
		{"D5 the same descriptor as --ready-fd", withArguments(paths, "--ready-fd", "4", "--listen-fd", "4"), "--listen-fd and --ready-fd require different descriptors"},
		{"D5 the same descriptor as --ready-fd, not open", withArguments(paths, "--ready-fd", notOpen, "--listen-fd", notOpen), "--listen-fd and --ready-fd require different descriptors"},
		{"O-LF1 the --config check comes first", []string{"--secrets-directory", filepath.Join(missing, "secrets"), "--listen-fd=" + sentinel}, "--config requires exactly one nonempty path"},
		{"O-LF1 the --secrets-directory check comes first", []string{"--config", filepath.Join(missing, "pm-deployment.json"), "--listen-fd=" + sentinel}, "--secrets-directory requires exactly one nonempty path"},
	}
	for _, value := range []string{sentinel, "", "0", "2", "03", "04", "+3", "+4", "-3", "-4", "x", "3 ", "2147483648"} {
		cases = append(cases, refusal{"D2 and D3 value " + strconv.Quote(value), withArguments(paths, "--listen-fd="+value), numberRule})
	}
	for _, refusal := range cases {
		t.Run(refusal.name, func(t *testing.T) {
			requireRefusal(t, refusal.args, refusal.want)
		})
	}
}

// inheritedCase is one descriptor for the check of --listen-fd. inherited
// tells if the descriptor arrives as an inherited one, which the check owns.
// handles names the socket whose handles the test counts after the refusal.
type inheritedCase struct {
	descriptor string
	listen     string
	inherited  bool
	handles    string
}

// Go developer test, not acceptance proof (ADR 0008, "The checks of the
// descriptor" and "Ownership of the listener"). Defect: the check accepts a
// descriptor that is not open, has close-on-exec set, is not a stream socket
// of the configured family, is not TCP, does not listen, or listens at
// another address. Or it keeps an inherited descriptor open after a refusal,
// closes a descriptor of the process itself, binds server.listen in place of
// the inherited socket, or prints the address. The configuration is valid,
// so the check of the descriptor is the only cause of the refusal. The test
// keeps no copy of an inherited socket, so after the refusal no handle of it
// may remain.
func TestListenDescriptorRefusals(t *testing.T) {
	cases := []struct {
		name  string
		setUp func(t *testing.T) inheritedCase
	}{
		{"a descriptor that is not open", func(t *testing.T) inheritedCase {
			return inheritedCase{descriptor: notOpen, listen: "127.0.0.1:8080"}
		}},
		{"a listener at the configured address with close-on-exec set", func(t *testing.T) inheritedCase {
			listener := listenLoopback(t)
			t.Cleanup(func() { _ = listener.Close() })
			file, err := listener.(*net.TCPListener).File()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = file.Close() })
			descriptor := int(file.Fd())
			if flags := descriptorFlags(t, descriptor); flags&syscall.FD_CLOEXEC == 0 {
				t.Fatalf("test setup: descriptor %d has no close-on-exec (flags %#x)", descriptor, flags)
			}
			return inheritedCase{descriptor: strconv.Itoa(descriptor), listen: listener.Addr().String()}
		}},
		{"a regular file", func(t *testing.T) inheritedCase {
			file, err := os.Create(filepath.Join(t.TempDir(), "regular"))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			return inheritedCase{descriptor: strconv.Itoa(inheritedCopy(t, int(file.Fd()))), listen: "127.0.0.1:8080", inherited: true}
		}},
		{"a datagram socket at the configured address", func(t *testing.T) inheritedCase {
			descriptor := boundLoopbackSocket(t, syscall.SOCK_DGRAM)
			name := socketName(descriptor)
			return inheritedCase{descriptor: strconv.Itoa(descriptor), listen: strings.TrimPrefix(name, "inet4 "), inherited: true, handles: name}
		}},
		{"an IPv4 listener for a configured IPv6 address", func(t *testing.T) inheritedCase {
			descriptor, name := inheritedListener(t)
			_, port, _ := net.SplitHostPort(strings.TrimPrefix(name, "inet4 "))
			return inheritedCase{descriptor: strconv.Itoa(descriptor), listen: net.JoinHostPort("::1", port), inherited: true, handles: name}
		}},
		{"an IPv4 listener for a configured IPv4-mapped address", func(t *testing.T) inheritedCase {
			descriptor, name := inheritedListener(t)
			_, port, _ := net.SplitHostPort(strings.TrimPrefix(name, "inet4 "))
			return inheritedCase{descriptor: strconv.Itoa(descriptor), listen: net.JoinHostPort("::ffff:127.0.0.1", port), inherited: true, handles: name}
		}},
		{"a Unix stream socket that listens", func(t *testing.T) inheritedCase {
			descriptor := unixListener(t)
			return inheritedCase{descriptor: strconv.Itoa(descriptor), listen: "127.0.0.1:8080", inherited: true, handles: socketName(descriptor)}
		}},
		{"a stream socket at the configured address that does not listen", func(t *testing.T) inheritedCase {
			descriptor := boundLoopbackSocket(t, syscall.SOCK_STREAM)
			name := socketName(descriptor)
			return inheritedCase{descriptor: strconv.Itoa(descriptor), listen: strings.TrimPrefix(name, "inet4 "), inherited: true, handles: name}
		}},
		{"a listener at another port", func(t *testing.T) inheritedCase {
			descriptor, name := inheritedListener(t)
			configured := listenLoopback(t)
			t.Cleanup(func() { _ = configured.Close() })
			return inheritedCase{descriptor: strconv.Itoa(descriptor), listen: configured.Addr().String(), inherited: true, handles: name}
		}},
		{"a listener for another configured loopback IP address at its port", func(t *testing.T) inheritedCase {
			descriptor, name := inheritedListener(t)
			_, port, _ := net.SplitHostPort(strings.TrimPrefix(name, "inet4 "))
			return inheritedCase{descriptor: strconv.Itoa(descriptor), listen: net.JoinHostPort("127.0.0.2", port), inherited: true, handles: name}
		}},
		{"a loopback listener for the configured wildcard at its port", func(t *testing.T) inheritedCase {
			descriptor, name := inheritedListener(t)
			_, port, _ := net.SplitHostPort(strings.TrimPrefix(name, "inet4 "))
			return inheritedCase{descriptor: strconv.Itoa(descriptor), listen: net.JoinHostPort("0.0.0.0", port), inherited: true, handles: name}
		}},
	}
	for _, refusal := range cases {
		t.Run(refusal.name, func(t *testing.T) {
			check := refusal.setUp(t)
			paths := []string{"--config", writeListenConfiguration(t, check.listen), "--secrets-directory", t.TempDir()}
			stdout, stderr := requireRefusal(t, withArguments(paths, "--listen-fd", check.descriptor), listenRefusal)
			if strings.Contains(stdout+stderr, check.listen) {
				t.Errorf("output shows the configured address %q", check.listen)
			}
			descriptor, _ := strconv.Atoi(check.descriptor)
			requireOwnership(t, descriptor, check.inherited, check.handles)
		})
	}
}

// Go developer test, not acceptance proof (ADR 0008, "There is no fallback";
// criteria D18). Defect: after it refuses a mismatched descriptor, the start
// path binds server.listen, or goes on to open the application. The
// configured address is free: the test bound it and closed it again. After
// the refusal, the process holds no socket at the configured address.
func TestRefusedDescriptorDoesNotFallBackToTheConfiguredAddress(t *testing.T) {
	free := listenLoopback(t)
	configured := free.Addr().String()
	_ = free.Close()
	descriptor, name := inheritedListener(t)
	paths := []string{"--config", writeListenConfiguration(t, configured), "--secrets-directory", t.TempDir()}
	requireRefusal(t, withArguments(paths, "--listen-fd", strconv.Itoa(descriptor)), listenRefusal)
	requireOwnership(t, descriptor, true, name)
	if count := socketHandles(t, "inet4 "+configured); count != 0 {
		t.Errorf("the process holds %d sockets at the configured address after the refusal, want none", count)
	}
}

// Go developer test, not acceptance proof (ADR 0008, "Ownership of the
// listener" and "No fallback"). Defect: a start that fails after the checks
// leaves the inherited socket open, keeps its descriptor, or binds
// server.listen. The descriptor passes each check. The secret directory is
// empty, so the application open fails after the checks.
func TestListenerClosedWhenTheStartFailsAfterTheChecks(t *testing.T) {
	descriptor, name := inheritedListener(t)
	listen := strings.TrimPrefix(name, "inet4 ")
	args := []string{"--config", writeListenConfiguration(t, listen), "--secrets-directory", t.TempDir(), "--listen-fd", strconv.Itoa(descriptor)}
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), args, nil, &stdout, &stderr)
	if err == nil {
		t.Fatal("the start passed with an empty secret directory, want a failure after the checks")
	}
	if text := err.Error(); text == listenRefusal || strings.Contains(text, "--listen-fd") || text == "configured server listener is unavailable" {
		t.Errorf("failure %q, want the failure of the application open after the checks", text)
	}
	requireOwnership(t, descriptor, true, name)
}

// requireRefusal runs the command of the binary once. It requires the refusal
// text, which main prints after "pm-server: " with exit code 1, no other
// output and no sentinel.
func requireRefusal(t *testing.T, args []string, want string) (string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), args, nil, &stdout, &stderr)
	if err == nil {
		t.Fatalf("arguments %q: no refusal, want %q", args, want)
	}
	if err.Error() != want {
		t.Errorf("arguments %q: refusal %q, want %q", args, err.Error(), want)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("arguments %q: output %q and %q, want none", args, stdout.String(), stderr.String())
	}
	if strings.Contains(err.Error()+stdout.String()+stderr.String(), sentinel) {
		t.Errorf("arguments %q: the output repeats the sentinel", args)
	}
	return stdout.String(), stderr.String()
}

// requireOwnership checks the descriptor after the command returned. The
// command must close an inherited descriptor and must leave a descriptor of
// the process itself open. No handle of the inherited socket may remain.
func requireOwnership(t *testing.T, descriptor int, inherited bool, handles string) {
	t.Helper()
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(descriptor), uintptr(syscall.F_GETFD), 0)
	switch {
	case inherited && errno != syscall.EBADF:
		_ = syscall.Close(descriptor)
		t.Errorf("the inherited descriptor %d is still open (errno %v), want it closed", descriptor, errno)
	case !inherited && descriptor != 2147483647 && errno != 0:
		t.Errorf("descriptor %d of the process itself was closed (errno %v), want it open", descriptor, errno)
	}
	if handles != "" {
		if count := socketHandles(t, handles); count != 0 {
			t.Errorf("%d handles of the socket %s remain, want none", count, handles)
		}
	}
}

func withArguments(paths []string, more ...string) []string {
	return append(append([]string(nil), paths...), more...)
}

// writeListenConfiguration writes a valid server document whose server.listen
// is listen. The lifecycle fixture of internal/server gives the other fields.
func writeListenConfiguration(t *testing.T, listen string) string {
	t.Helper()
	document := fmt.Sprintf(`{
  "schema_version":"1","deployment_id":"listen-fixture","public_origin":"https://pm.example.invalid",
  "server":{"listen":%q,"drain_timeout":"2s"},
  "postgres":{"ownership":"managed","database":"pm_app","endpoint_ref":"secret://fixture/postgres"},
  "temporal":{"ownership":"managed","endpoint":"temporal.example.invalid:7233","namespace":"fixture","client_identity_ref":"secret://fixture/temporal","retention":"720h","task_queues":["pm-control"]},
  "artifacts":{"ownership":"managed","driver":"s3","endpoint":"https://objects.example.invalid","bucket":"fixture-objects","credential_ref":"secret://fixture/objects","attempt_key_policy":"unique_immutable"},
  "identity":{"issuer":"https://identity.example.invalid","client_id":"pm-server","client_secret_ref":"secret://fixture/oidc"},
  "encryption":{"wrapping_key_ref":"secret://fixture/wrapping"},"admission":{"profile":"development"},
  "limits":{"workspace_active_runs":4,"workflow_payload_bytes":65536,"workflow_fanout":32,"run_lifetime":"1h","event_buffer":128,"query_children":1,"query_child_memory_bytes":4294967296,"query_child_deadline":"5m","request_bytes":1048576,"metadata_deadline":"5s"}
}`, listen)
	path := filepath.Join(t.TempDir(), "server.json")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// listenLoopback gives a Go listener at a free loopback port. The caller owns
// it.
func listenLoopback(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

// inheritedListener gives the one handle of a loopback listener, without
// close-on-exec, as an inherited descriptor arrives, and the socket name. The
// test keeps no other copy. The check owns the descriptor.
func inheritedListener(t *testing.T) (int, string) {
	t.Helper()
	listener := listenLoopback(t)
	file, err := listener.(*net.TCPListener).File()
	_ = listener.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	descriptor := inheritedCopy(t, int(file.Fd()))
	return descriptor, socketName(descriptor)
}

// inheritedCopy gives a copy of fd without close-on-exec, as an inherited
// descriptor arrives. The check owns the copy.
func inheritedCopy(t *testing.T, fd int) int {
	t.Helper()
	inherited, err := syscall.Dup(fd)
	if err != nil {
		t.Fatal(err)
	}
	if flags := descriptorFlags(t, inherited); flags&syscall.FD_CLOEXEC != 0 {
		_ = syscall.Close(inherited)
		t.Fatalf("test setup: descriptor %d has close-on-exec (flags %#x)", inherited, flags)
	}
	return inherited
}

// boundLoopbackSocket gives an IPv4 socket of the kind, bound to a free
// loopback port, with no listen call and no close-on-exec. The check owns it.
func boundLoopbackSocket(t *testing.T, kind int) int {
	t.Helper()
	descriptor, err := syscall.Socket(syscall.AF_INET, kind, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Bind(descriptor, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		_ = syscall.Close(descriptor)
		t.Fatal(err)
	}
	if flags := descriptorFlags(t, descriptor); flags&syscall.FD_CLOEXEC != 0 {
		_ = syscall.Close(descriptor)
		t.Fatalf("test setup: descriptor %d has close-on-exec (flags %#x)", descriptor, flags)
	}
	return descriptor
}

// unixListener gives a Unix stream socket that listens at a short path, with
// no close-on-exec. The check owns it.
func unixListener(t *testing.T) int {
	t.Helper()
	directory, err := os.MkdirTemp("", "pmlfd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	descriptor, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Bind(descriptor, &syscall.SockaddrUnix{Name: filepath.Join(directory, "s")}); err != nil {
		_ = syscall.Close(descriptor)
		t.Fatal(err)
	}
	if err := syscall.Listen(descriptor, 1); err != nil {
		_ = syscall.Close(descriptor)
		t.Fatal(err)
	}
	return descriptor
}

// socketName names the local address of a socket descriptor, with its family,
// or gives "" for a descriptor that is not a socket.
func socketName(descriptor int) string {
	local, err := syscall.Getsockname(descriptor)
	if err != nil {
		return ""
	}
	switch address := local.(type) {
	case *syscall.SockaddrInet4:
		return "inet4 " + net.JoinHostPort(net.IP(address.Addr[:]).String(), strconv.Itoa(address.Port))
	case *syscall.SockaddrInet6:
		return "inet6 " + net.JoinHostPort(net.IP(address.Addr[:]).String(), strconv.Itoa(address.Port))
	case *syscall.SockaddrUnix:
		return "unix " + address.Name
	}
	return ""
}

// socketHandles counts the open descriptors of this process whose socket has
// the name.
func socketHandles(t *testing.T, name string) int {
	t.Helper()
	count, err := countSocketHandles(name)
	if err != nil {
		t.Fatal(err)
	}
	return count
}

// countSocketHandles counts the open descriptors of this process whose
// socket has the name. It reads the descriptor list of the process from
// /dev/fd.
func countSocketHandles(name string) (int, error) {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		descriptor, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if socketName(descriptor) == name {
			count++
		}
	}
	return count, nil
}
