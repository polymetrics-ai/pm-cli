package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/server"
)

// Go developer test, not acceptance proof (ADR 0008, "Ownership of the
// listener" and "The ready signal"). Defect: the check refuses a listener at
// the configured address that a starter hands over, leaves the inherited
// descriptor open, keeps more than one handle of the socket, serves on a
// handle without close-on-exec, does not serve on the inherited socket,
// leaves a handle after serving ends, or does not send the ready line through
// the ready descriptor before the first connection. The test reads the ready line before it sends
// a request, so no connection comes before the line. The test keeps no copy
// of the socket, so it can count the handles of the process. No sleep, poll
// or retry.
func TestInheritedListenerServesOneRequest(t *testing.T) {
	inherited, name := inheritedListener(t)
	address := strings.TrimPrefix(name, "inet4 ")

	listener, err := inheritListenDescriptor(inherited, address)
	if err != nil {
		_ = syscall.Close(inherited)
		t.Fatalf("the check refused an inherited listener at the configured address: %v", err)
	}
	served := false
	defer func() {
		if !served {
			_ = listener.Close()
		}
	}()
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(inherited), uintptr(syscall.F_GETFD), 0); errno != syscall.EBADF {
		t.Errorf("the inherited descriptor %d is still open after the check (errno %v)", inherited, errno)
	}
	if count := socketHandles(t, name); count != 1 {
		t.Errorf("%d handles of the socket after the check, want exactly one", count)
	}
	if got := listener.Addr().String(); got != address {
		t.Errorf("listener address %q, want the configured address %q", got, address)
	}
	if flags := listenerDescriptorFlags(t, listener); flags&syscall.FD_CLOEXEC == 0 {
		t.Errorf("the listener handle has no close-on-exec (flags %#x)", flags)
	}

	readEnd, ready := readyPipe(t)
	diagnostics := &lockedText{}
	service := newReadyTestServer(t, diagnostics)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const answer = "served on the inherited listener\n"
	handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, answer)
	})
	result := make(chan error, 1)
	served = true
	go func() { result <- service.Serve(ctx, listener, handler, ready) }()

	// The ready line comes at the first call of Accept. No connection comes
	// before it (ADR 0005, ADR 0008 v2.1).
	if err := readEnd.SetReadDeadline(time.Now().Add(readyTestLimit)); err != nil {
		t.Fatal(err)
	}
	line, err := io.ReadAll(readEnd)
	if err != nil || string(line) != server.ReadyLine {
		t.Errorf("ready descriptor gave %q (%v), want %q and end of file", line, err, server.ReadyLine)
	}

	client := &http.Client{Timeout: readyTestLimit, Transport: &http.Transport{DisableKeepAlives: true}}
	response, err := client.Get("http://" + address + "/")
	if err != nil {
		t.Fatalf("request to the inherited listener: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(body) != answer {
		t.Errorf("response %d %q (%v), want %d %q", response.StatusCode, body, err, http.StatusOK, answer)
	}

	cancel()
	awaitServe(t, result)
	if count := socketHandles(t, name); count != 0 {
		t.Errorf("%d handles of the socket after serving ended, want none", count)
	}
	if got := diagnostics.String(); got != "" {
		t.Errorf("diagnostics %q, want none", got)
	}
}

// readyPipe gives the read end of a pipe and the ready handle that the
// product check makes from an inherited copy of its write end.
func readyPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = read.Close() })
	inherited, err := syscall.Dup(int(write.Fd()))
	_ = write.Close()
	if err != nil {
		t.Fatal(err)
	}
	handle, err := inheritReadyDescriptor(inherited)
	if err != nil {
		_ = syscall.Close(inherited)
		t.Fatalf("the ready check refused an inherited write end: %v", err)
	}
	return read, handle
}

func listenerDescriptorFlags(t *testing.T, listener net.Listener) uintptr {
	t.Helper()
	raw, err := listener.(*net.TCPListener).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags uintptr
	if err := raw.Control(func(fd uintptr) { flags = descriptorFlags(t, int(fd)) }); err != nil {
		t.Fatal(err)
	}
	return flags
}

func awaitServe(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		if err != nil {
			t.Errorf("Serve returned %v after the context ended, want nil", err)
		}
	case <-time.After(readyTestLimit):
		t.Fatalf("Serve did not return within %s after the context ended", readyTestLimit)
	}
}
