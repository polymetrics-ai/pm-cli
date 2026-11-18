package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/server"
)

// Go developer test (c), not acceptance proof. Defect: a kept handle without
// FD_CLOEXEC after the check. The test also shows that the check refuses a
// descriptor that has close-on-exec set and a descriptor that is open only
// for reading.
func TestReadyDescriptorHasCloseOnExecAfterTheCheck(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()

	// os.Pipe sets close-on-exec, as Go does for each descriptor of its own.
	if _, err := inheritReadyDescriptor(int(write.Fd())); err == nil {
		t.Fatal("the check accepted a descriptor that has close-on-exec set")
	}
	// dup clears close-on-exec, as an inherited descriptor arrives.
	readOnly, err := syscall.Dup(int(read.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(readOnly)
	if _, err := inheritReadyDescriptor(readOnly); err == nil {
		t.Fatal("the check accepted a descriptor that is open only for reading")
	}
	inherited, err := syscall.Dup(int(write.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if flags := descriptorFlags(t, inherited); flags&syscall.FD_CLOEXEC != 0 {
		_ = syscall.Close(inherited)
		t.Fatalf("test setup: the inherited descriptor has close-on-exec (flags %#x)", flags)
	}
	handle, err := inheritReadyDescriptor(inherited)
	if err != nil {
		_ = syscall.Close(inherited)
		t.Fatalf("the check refused an inherited descriptor that is open for writing: %v", err)
	}
	defer handle.Close()
	if int(handle.Fd()) != inherited {
		t.Fatalf("handle descriptor %d, want the checked descriptor %d and no copy", handle.Fd(), inherited)
	}
	if flags := descriptorFlags(t, inherited); flags&syscall.FD_CLOEXEC == 0 {
		t.Fatalf("the kept handle has no FD_CLOEXEC after the check (flags %#x)", flags)
	}
	// Review finding F2: the kept handle is non-blocking, so that Serve can
	// end a pending ready write when it returns.
	if status := statusFlags(t, inherited); status&syscall.O_NONBLOCK == 0 {
		t.Fatalf("the kept handle is in blocking mode after the check (status flags %#x)", status)
	}
}

func descriptorFlags(t *testing.T, fd int) uintptr {
	t.Helper()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFD), 0)
	if errno != 0 {
		t.Fatalf("F_GETFD on descriptor %d: %v", fd, errno)
	}
	return flags
}

// Go developer test, not acceptance proof. Defect: a value rule that accepts
// a leading zero, a sign, a value below 3 or a value that does not fit in a
// 32-bit signed int, or that refuses the boundary 2147483647.
func TestReadyDescriptorValueRule(t *testing.T) {
	for _, value := range []string{"3", "4", "300", "2147483647"} {
		if _, ok := parseReadyDescriptor(value); !ok {
			t.Errorf("value %q refused, want accepted", value)
		}
	}
	for _, value := range []string{"", "0", "2", "03", "+3", "-3", "x", "3 ", "2147483648", "99999999999999999999"} {
		if _, ok := parseReadyDescriptor(value); ok {
			t.Errorf("value %q accepted, want refused", value)
		}
	}
}

func statusFlags(t *testing.T, fd int) uintptr {
	t.Helper()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFL), 0)
	if errno != 0 {
		t.Fatalf("F_GETFL on descriptor %d: %v", fd, errno)
	}
	return flags
}

// readyTestLimit bounds each single wait of the check below.
const readyTestLimit = 10 * time.Second

// Go developer test, not acceptance proof (review finding F2 of PR #283).
// Defect: Serve returns while its ready handle is still open and its writer
// still blocks in the ready write. The pipe is full and nobody reads it. The
// test hands the write end over in blocking mode, as a starter does
// (File.Fd), as a copy without close-on-exec. The product check
// (inheritReadyDescriptor) makes the handle, and Serve gets it. When Serve
// returns, the handle must be closed and the writer must have ended. The
// shutdown ended that write, so no write-failure line may appear. Each wait
// has one limit. No sleep, poll or retry.
func TestServeEndsABlockedReadyWriteWhenItReturns(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// Closing the read end at the end also ends a writer that still blocks.
	t.Cleanup(func() { _ = read.Close() })
	fillPipe(t, write)
	inherited, err := syscall.Dup(int(write.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	_ = write.Close()
	handle, err := inheritReadyDescriptor(inherited)
	if err != nil {
		_ = syscall.Close(inherited)
		t.Fatalf("the check refused an inherited descriptor that is open for writing: %v", err)
	}
	observed := newObservedHandle(handle)
	t.Cleanup(func() { _ = observed.Close() })

	diagnostics := &lockedText{}
	service := newReadyTestServer(t, diagnostics)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- service.Serve(ctx, listener, http.NewServeMux(), observed) }()

	awaitReady(t, observed.entered, "start of the ready write")
	cancel()
	select {
	case <-served:
	case <-time.After(readyTestLimit):
		t.Fatalf("Serve did not return within %s after the context ended", readyTestLimit)
	}
	if !observed.isClosed() {
		t.Error("Serve returned while the ready handle was still open")
	}
	if !observed.hasEnded() {
		t.Error("Serve returned while the ready writer still blocked in its write")
	}
	if got := diagnostics.String(); got != "" {
		t.Errorf("diagnostics %q, want no write-failure line for a write that the shutdown ended", got)
	}
}

// fillPipe writes to the pipe until it is full. It writes through the raw
// connection of the non-blocking write end and stops at the first EAGAIN.
func fillPipe(t *testing.T, write *os.File) {
	t.Helper()
	raw, err := write.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 4096)
	var writeErr error
	err = raw.Write(func(fd uintptr) bool {
		for {
			if _, err := syscall.Write(int(fd), chunk); err != nil {
				if err != syscall.EAGAIN {
					writeErr = err
				}
				return true
			}
		}
	})
	if err != nil || writeErr != nil {
		t.Fatalf("fill the pipe: %v %v", err, writeErr)
	}
}

func awaitReady(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(readyTestLimit):
		t.Fatalf("no %s within %s", what, readyTestLimit)
	}
}

// observedHandle passes each call to the ready handle. It records the start
// of the first write, the end of the first write and the first Close.
type observedHandle struct {
	file      *os.File
	entered   chan struct{}
	ended     chan struct{}
	closed    chan struct{}
	enterOnce sync.Once
	endOnce   sync.Once
	closeOnce sync.Once
}

func newObservedHandle(file *os.File) *observedHandle {
	return &observedHandle{file: file, entered: make(chan struct{}), ended: make(chan struct{}), closed: make(chan struct{})}
}

func (handle *observedHandle) Write(payload []byte) (int, error) {
	handle.enterOnce.Do(func() { close(handle.entered) })
	written, err := handle.file.Write(payload)
	handle.endOnce.Do(func() { close(handle.ended) })
	return written, err
}

func (handle *observedHandle) Close() error {
	err := handle.file.Close()
	handle.closeOnce.Do(func() { close(handle.closed) })
	return err
}

func (handle *observedHandle) isClosed() bool { return isDone(handle.closed) }

func (handle *observedHandle) hasEnded() bool { return isDone(handle.ended) }

func isDone(signal <-chan struct{}) bool {
	select {
	case <-signal:
		return true
	default:
		return false
	}
}

// lockedText is a diagnostics writer that the test reads after Serve returns.
type lockedText struct {
	mu   sync.Mutex
	text strings.Builder
}

func (buffer *lockedText) Write(payload []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.text.Write(payload)
}

func (buffer *lockedText) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.text.String()
}

// newReadyTestServer makes a lifecycle server with a passing admission probe,
// as the lifecycle tests of internal/server do. The drain bound is 2 s.
func newReadyTestServer(t *testing.T, diagnostics *lockedText) *server.Server {
	t.Helper()
	raw := `{
  "schema_version":"1","deployment_id":"ready-fixture","public_origin":"https://pm.example.invalid",
  "server":{"drain_timeout":"2s"},
  "postgres":{"ownership":"managed","database":"pm_app","endpoint_ref":"secret://fixture/postgres"},
  "temporal":{"ownership":"managed","endpoint":"temporal.example.invalid:7233","namespace":"fixture","client_identity_ref":"secret://fixture/temporal","retention":"720h","task_queues":["pm-control"]},
  "artifacts":{"ownership":"managed","driver":"s3","endpoint":"https://objects.example.invalid","bucket":"fixture-objects","credential_ref":"secret://fixture/objects","attempt_key_policy":"unique_immutable"},
  "identity":{"issuer":"https://identity.example.invalid","client_id":"pm-server","client_secret_ref":"secret://fixture/oidc"},
  "encryption":{"wrapping_key_ref":"secret://fixture/wrapping"},"admission":{"profile":"development"},
  "limits":{"workspace_active_runs":4,"workflow_payload_bytes":65536,"workflow_fanout":32,"run_lifetime":"1h","event_buffer":128,"query_children":1,"query_child_memory_bytes":4294967296,"query_child_deadline":"5m","request_bytes":1048576,"metadata_deadline":"5s"}
}`
	document, err := config.Load(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := config.Validate(document, config.Prelaunch{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := server.New(configuration, func(context.Context) error { return nil }, nil, diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	return service
}
