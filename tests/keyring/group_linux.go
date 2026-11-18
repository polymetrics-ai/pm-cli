//go:build linux

package keyring

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// liveGroupMembers counts the live processes of the process group pgid other
// than the process pgid itself. It reads /proc once: for each process, the
// state and the process group from /proc/<pid>/stat, after the last ")". A
// zombie (state Z) does not count. A process that exits during the read is
// not counted. Each other read error is an error, never "none left".
func liveGroupMembers(pgid int) (int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, fmt.Errorf("read /proc: %w", err)
	}
	count := 0
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == pgid {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("read the state of process %d: %w", pid, err)
		}
		end := bytes.LastIndexByte(stat, ')')
		if end < 0 {
			return 0, fmt.Errorf("the state of process %d has no command name", pid)
		}
		// After the command name: state, parent ID, process group ID.
		fields := bytes.Fields(stat[end+1:])
		if len(fields) < 3 {
			return 0, fmt.Errorf("the state of process %d is too short", pid)
		}
		group, err := strconv.Atoi(string(fields[2]))
		if err != nil {
			return 0, fmt.Errorf("the process group of process %d is not a number", pid)
		}
		if group == pgid && string(fields[0]) != "Z" {
			count++
		}
	}
	return count, nil
}
