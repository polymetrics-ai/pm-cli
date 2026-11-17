//go:build unix

package config_test

import (
	"fmt"
	"os"
	"syscall"
	"testing"
)

// TestCanaryB8StopsGoTestInCI is a deliberate fault for issue #272. It exists
// only on the throwaway branch agent/272-canary-b8-5 and never merges.
// Only in CI (GITHUB_ACTIONS is "true"), it writes a marker to the stderr of
// its parent `go test` process, which is the job log, and then stops that
// process with SIGKILL. So the go test command ends before it prints all its
// results, and the unit run must fail and name the packages with no result.
func TestCanaryB8StopsGoTestInCI(t *testing.T) {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return
	}
	parent := os.Getppid()
	if log, err := os.OpenFile(fmt.Sprintf("/proc/%d/fd/2", parent), os.O_WRONLY|os.O_APPEND, 0); err == nil {
		fmt.Fprintf(log, "canary B8 for issue #272: %s stops its parent go test process %d with SIGKILL\n", t.Name(), parent)
		log.Close()
	}
	if err := syscall.Kill(parent, syscall.SIGKILL); err != nil {
		t.Fatalf("canary B8 for issue #272: cannot stop the parent go test process: %v", err)
	}
	os.Exit(3)
}
