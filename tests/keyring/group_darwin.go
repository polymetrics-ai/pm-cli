//go:build darwin

package keyring

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// darwinZombie is SZOMB of <sys/proc.h>, the state of a zombie process.
const darwinZombie = 5

// liveGroupMembers counts the live processes of the process group pgid other
// than the process pgid itself. It reads the kern.proc.pgrp sysctl once. A
// zombie does not count. A read error is an error, never "none left".
func liveGroupMembers(pgid int) (int, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return 0, fmt.Errorf("read the kern.proc.pgrp sysctl: %w", err)
	}
	count := 0
	for _, process := range processes {
		if int(process.Proc.P_pid) == pgid || process.Proc.P_stat == darwinZombie {
			continue
		}
		count++
	}
	return count, nil
}
