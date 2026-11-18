package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/server"
)

// The handover check runs this test binary again as a child process, as a
// starter runs pm-server. The environment selects the part of the child.
const (
	handoverPart    = "PM_SERVER_HANDOVER_PART"
	handoverAddress = "PM_SERVER_HANDOVER_ADDRESS"
	handoverConfig  = "PM_SERVER_HANDOVER_CONFIG"
	handoverSecrets = "PM_SERVER_HANDOVER_SECRETS"
)

func TestMain(m *testing.M) {
	if part := os.Getenv(handoverPart); part != "" {
		os.Exit(runHandoverChild(part))
	}
	os.Exit(m.Run())
}

// runHandoverChild is the child process of the handover check. Descriptor 3
// is the inherited listener. In the serve part, descriptor 4 is the inherited
// write end of the ready pipe. A broken rule gives one line on standard error
// and exit code 2.
func runHandoverChild(part string) int {
	address := os.Getenv(handoverAddress)
	name := "inet4 " + address
	broken := func(format string, args ...any) int {
		_, _ = fmt.Fprintf(os.Stderr, "handover child: "+format+"\n", args...)
		return 2
	}
	switch part {
	case "start":
		// The real command path. The secret directory is empty, so the start
		// fails after the checks of the descriptor.
		args := []string{"--config", os.Getenv(handoverConfig), "--secrets-directory", os.Getenv(handoverSecrets), "--listen-fd", "3"}
		err := run(context.Background(), args, nil, io.Discard, io.Discard)
		if err == nil {
			return broken("the start passed with an empty secret directory")
		}
		if count, countErr := countSocketHandles(name); countErr != nil || count != 0 {
			return broken("%d handles of the socket after the failed start (%v), want none", count, countErr)
		}
		_, _ = fmt.Fprintf(os.Stderr, "pm-server: %v\n", err)
		return 1
	case "serve":
		ready, err := inheritReadyDescriptor(4)
		if err != nil {
			return broken("the ready check refused descriptor 4: %v", err)
		}
		listener, err := inheritListenDescriptor(3, address)
		if err != nil {
			return broken("the listen check refused descriptor 3: %v", err)
		}
		if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, 3, uintptr(syscall.F_GETFD), 0); errno != syscall.EBADF {
			return broken("descriptor 3 is still open after the check (errno %v)", errno)
		}
		if count, err := countSocketHandles(name); err != nil || count != 1 {
			return broken("%d handles of the socket after the check (%v), want exactly one", count, err)
		}
		if closeOnExec, err := listenerCloseOnExec(listener); err != nil || !closeOnExec {
			return broken("the listener handle has no close-on-exec (%v)", err)
		}
		service, err := lifecycleFixture(os.Stderr)
		if err != nil {
			return broken("lifecycle fixture: %v", err)
		}
		handler := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(writer, strconv.Itoa(os.Getpid()))
		})
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
		defer stop()
		if err := service.Serve(ctx, listener, handler, ready); err != nil {
			return broken("Serve: %v", err)
		}
		if count, err := countSocketHandles(name); err != nil || count != 0 {
			return broken("%d handles of the socket after serving ended (%v), want none", count, err)
		}
		return 0
	}
	if extra, ok := handoverChildParts[part]; ok {
		return extra()
	}
	return broken("unknown part %q", part)
}

// handoverChildParts are child parts that a test file for one system adds.
var handoverChildParts = map[string]func() int{}

// Go developer test, not acceptance proof: the local mechanism check F3 of the
// ADR 0008 review. A test starter runs this test binary as a child process
// with os/exec and ExtraFiles, as a starter runs pm-server. The child runs the
// product checks (inheritListenDescriptor, inheritReadyDescriptor, and run
// for the failed start) and the lifecycle server; it cannot run server.Open,
// which needs the stack. Defect: the port is free during the handover, a
// conversion copy remains, a kept handle has no close-on-exec, or a handle
// remains after a failed launch, a failed start or a normal stop. Each wait
// has one limit. No sleep, poll or retry.
func TestListenerHandoverToAChildProcess(t *testing.T) {
	t.Run("serve, then a normal stop", func(t *testing.T) {
		starter, name, address := starterListener(t)
		listenCopy := starterCopy(t, starter)
		readEnd, readyWrite, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = readEnd.Close() })
		command := handoverCommand(os.Args[0], "serve", address, "", "", listenCopy, readyWrite)
		stderr := &lockedText{}
		command.Stderr = stderr
		process := startChild(t, command)
		_ = listenCopy.Close()
		_ = readyWrite.Close()
		requireStarterHandle(t, starter, name)

		// The ready line comes first, with no connection before it.
		if err := readEnd.SetReadDeadline(time.Now().Add(readyTestLimit)); err != nil {
			t.Fatal(err)
		}
		if line, err := io.ReadAll(readEnd); err != nil || string(line) != server.ReadyLine {
			t.Fatalf("ready descriptor gave %q (%v), want %q and end of file; child: %q", line, err, server.ReadyLine, stderr.String())
		}
		requirePortHeld(t, address)

		client := &http.Client{Timeout: readyTestLimit, Transport: &http.Transport{DisableKeepAlives: true}}
		response, err := client.Get("http://" + address + "/")
		if err != nil {
			t.Fatalf("request to the inherited listener: %v", err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if want := strconv.Itoa(command.Process.Pid); err != nil || string(body) != want {
			t.Errorf("response %q (%v), want the child process %s", body, err, want)
		}

		// The starter closes its copy after one served request, then stops the
		// child as a supervisor does.
		_ = starter.Close()
		if err := command.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if code := process.exitCode(t); code != 0 {
			t.Errorf("child exit code %d, want 0; child: %q", code, stderr.String())
		}
		if text := stderr.String(); text != "" {
			t.Errorf("child standard error %q, want none", text)
		}
		requireNoHandle(t, name, address)
	})

	t.Run("a start that fails after the checks", func(t *testing.T) {
		starter, name, address := starterListener(t)
		listenCopy := starterCopy(t, starter)
		command := handoverCommand(os.Args[0], "start", address, writeListenConfiguration(t, address), t.TempDir(), listenCopy)
		stderr := &lockedText{}
		command.Stderr = stderr
		process := startChild(t, command)
		_ = listenCopy.Close()
		requireStarterHandle(t, starter, name)
		if code := process.exitCode(t); code != 1 {
			t.Errorf("child exit code %d, want 1; child: %q", code, stderr.String())
		}
		text := stderr.String()
		if !strings.HasPrefix(text, "pm-server: ") || strings.Count(text, "\n") != 1 || strings.Contains(text, "--listen-fd") || strings.Contains(text, "configured server listener is unavailable") {
			t.Errorf("child standard error %q, want one line of the failed application open", text)
		}
		// The starter's copy still holds the port after the failed start. The
		// starter then closes it.
		requirePortHeld(t, address)
		_ = starter.Close()
		requireNoHandle(t, name, address)
	})

	t.Run("a launch that fails", func(t *testing.T) {
		starter, name, address := starterListener(t)
		listenCopy := starterCopy(t, starter)
		command := handoverCommand(filepath.Join(t.TempDir(), "missing"), "serve", address, "", "", listenCopy)
		if err := command.Start(); err == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatal("the launch of a missing file started a process")
		}
		_ = listenCopy.Close()
		requireStarterHandle(t, starter, name)
		_ = starter.Close()
		requireNoHandle(t, name, address)
	})
}

// starterListener gives the starter's listener at a free loopback port, its
// socket name and its address. The starter owns it.
func starterListener(t *testing.T) (net.Listener, string, string) {
	t.Helper()
	listener := listenLoopback(t)
	t.Cleanup(func() { _ = listener.Close() })
	address := listener.Addr().String()
	return listener, "inet4 " + address, address
}

// starterCopy gives the copy of the starter's socket that os/exec moves to
// descriptor 3 of the child. Go gives the copy close-on-exec in the starter.
func starterCopy(t *testing.T, listener net.Listener) *os.File {
	t.Helper()
	file, err := listener.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func handoverCommand(path, part, address, configuration, secrets string, files ...*os.File) *exec.Cmd {
	command := exec.Command(path)
	command.Env = append(os.Environ(), handoverPart+"="+part, handoverAddress+"="+address, handoverConfig+"="+configuration, handoverSecrets+"="+secrets)
	command.ExtraFiles = files
	return command
}

// child is a started child process and its one Wait.
type child struct {
	command *exec.Cmd
	done    chan struct{}
}

func startChild(t *testing.T, command *exec.Cmd) *child {
	t.Helper()
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	process := &child{command: command, done: make(chan struct{})}
	go func() {
		_ = command.Wait()
		close(process.done)
	}()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		// Wait also waits for the copy of the child output, so a process
		// that keeps that pipe open can delay it. One limit bounds it.
		select {
		case <-process.done:
		case <-time.After(readyTestLimit):
			t.Errorf("the child did not end within %s after SIGKILL", readyTestLimit)
		}
	})
	return process
}

// exitCode waits once for the exit of the child, within one limit.
func (process *child) exitCode(t *testing.T) int {
	t.Helper()
	select {
	case <-process.done:
	case <-time.After(readyTestLimit):
		t.Fatalf("the child did not exit within %s", readyTestLimit)
	}
	return process.command.ProcessState.ExitCode()
}

// requireStarterHandle requires that the starter holds exactly one handle of
// the socket, its listener, with close-on-exec.
func requireStarterHandle(t *testing.T, listener net.Listener, name string) {
	t.Helper()
	if count := socketHandles(t, name); count != 1 {
		t.Errorf("the starter holds %d handles of the socket, want exactly one", count)
	}
	if closeOnExec, err := listenerCloseOnExec(listener); err != nil || !closeOnExec {
		t.Errorf("the handle that the starter keeps has no close-on-exec (%v)", err)
	}
}

// requirePortHeld requires that a second bind of the address fails with
// EADDRINUSE.
func requirePortHeld(t *testing.T, address string) {
	t.Helper()
	second, err := net.Listen("tcp", address)
	if err == nil {
		_ = second.Close()
		t.Fatalf("a second bind of %s passed, want EADDRINUSE while the socket is held", address)
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("a second bind of %s failed with %v, want EADDRINUSE", address, err)
	}
}

// requireNoHandle requires that this process holds no handle of the socket
// and that no process accepts a connection at the address.
func requireNoHandle(t *testing.T, name, address string) {
	t.Helper()
	if count := socketHandles(t, name); count != 0 {
		t.Errorf("the starter holds %d handles of the socket, want none", count)
	}
	connection, err := net.DialTimeout("tcp", address, readyTestLimit)
	if err == nil {
		_ = connection.Close()
		t.Fatalf("a connection to %s passed, want ECONNREFUSED after each handle closed", address)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("a connection to %s failed with %v, want ECONNREFUSED", address, err)
	}
}

// listenerCloseOnExec tells if the handle of a TCP listener has close-on-exec.
func listenerCloseOnExec(listener net.Listener) (bool, error) {
	raw, err := listener.(*net.TCPListener).SyscallConn()
	if err != nil {
		return false, err
	}
	var flags uintptr
	var errno syscall.Errno
	if err := raw.Control(func(fd uintptr) {
		flags, _, errno = syscall.Syscall(syscall.SYS_FCNTL, fd, uintptr(syscall.F_GETFD), 0)
	}); err != nil {
		return false, err
	}
	if errno != 0 {
		return false, errno
	}
	return flags&syscall.FD_CLOEXEC != 0, nil
}
