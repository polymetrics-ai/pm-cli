package main

import (
	"os"
	"syscall"
	"testing"
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
