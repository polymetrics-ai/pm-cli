//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"unsafe"
)

func init() { handoverChildParts["refuse-without-bind"] = runRefuseWithoutBind }

// Go developer test, not acceptance proof (ADR 0008, "There is no fallback";
// criteria D18; #405 review R4). Defect: after it refuses a mismatched
// descriptor, the start path binds server.listen or tries to, even if it
// closes the socket again at once. The child runs the real command path under
// a seccomp filter that ends the process with SIGSYS at the first bind call.
// So any bind after the refusal ends the child by a signal, not with the
// refusal. One limit bounds the wait. No sleep, poll or retry.
func TestRefusedDescriptorMakesNoBind(t *testing.T) {
	free := listenLoopback(t)
	configured := free.Addr().String()
	starter, name, _ := starterListener(t)
	_ = free.Close()
	if name == "inet4 "+configured {
		t.Fatalf("test setup: the inherited listener is at the configured address %s", configured)
	}
	listenCopy := starterCopy(t, starter)
	command := handoverCommand(os.Args[0], "refuse-without-bind", configured, writeListenConfiguration(t, configured), t.TempDir(), listenCopy)
	stderr := &lockedText{}
	command.Stderr = stderr
	process := startChild(t, command)
	_ = listenCopy.Close()
	code := process.exitCode(t)
	if want := "pm-server: " + listenRefusal + "\n"; code != 1 || stderr.String() != want {
		t.Fatalf("child ended with %s and stderr %q; want exit code 1 and %q (SIGSYS: a bind after the refusal)",
			process.command.ProcessState, stderr.String(), want)
	}
}

// runRefuseWithoutBind is the child part of TestRefusedDescriptorMakesNoBind.
// Descriptor 3 is the inherited listener at another address.
func runRefuseWithoutBind() int {
	if err := endProcessOnBind(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "handover child: %v\n", err)
		return 2
	}
	args := []string{"--config", os.Getenv(handoverConfig), "--secrets-directory", os.Getenv(handoverSecrets), "--listen-fd", strconv.Itoa(3)}
	err := run(context.Background(), args, nil, io.Discard, io.Discard)
	if err == nil {
		err = errors.New("no refusal")
	}
	_, _ = fmt.Fprintf(os.Stderr, "pm-server: %v\n", err)
	return 1
}

// endProcessOnBind installs a seccomp filter on every thread of the process:
// the bind system call ends the process (SECCOMP_RET_KILL_PROCESS). Every
// other call is allowed. <linux/seccomp.h> and <linux/filter.h> give the
// values.
func endProcessOnBind() error {
	seccompCall, ok := map[string]uintptr{"amd64": 317, "arm64": 277}[runtime.GOARCH]
	if !ok {
		return fmt.Errorf("no seccomp system call number for %s", runtime.GOARCH)
	}
	runtime.LockOSThread()
	filter := []syscall.SockFilter{
		{Code: 0x20, K: 0}, // BPF_LD|BPF_W|BPF_ABS: the system call number
		{Code: 0x15, Jt: 0, Jf: 1, K: syscall.SYS_BIND}, // BPF_JMP|BPF_JEQ|BPF_K
		{Code: 0x06, K: 0x80000000},                     // BPF_RET: SECCOMP_RET_KILL_PROCESS
		{Code: 0x06, K: 0x7fff0000},                     // BPF_RET: SECCOMP_RET_ALLOW
	}
	program := syscall.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, 38, 1, 0, 0, 0, 0); errno != 0 { // PR_SET_NO_NEW_PRIVS
		return fmt.Errorf("prctl PR_SET_NO_NEW_PRIVS: %v", errno)
	}
	// SECCOMP_SET_MODE_FILTER with SECCOMP_FILTER_FLAG_TSYNC.
	if _, _, errno := syscall.RawSyscall(seccompCall, 1, 1, uintptr(unsafe.Pointer(&program))); errno != 0 {
		return fmt.Errorf("seccomp SECCOMP_SET_MODE_FILTER: %v", errno)
	}
	return nil
}
