//go:build unix

package temporal

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
)

// TestCanaryF3SignalsGoTestInStackRun is a deliberate fault for issue #272,
// review F3. It exists only on the throwaway branch agent/272-canary-f3-5 and
// never merges. internal/temporal is a stack package, so neither the git hooks
// nor the unit run of the checks job run it. Only in CI (GITHUB_ACTIONS is
// "true"), it sends SIGTERM to its parent `go test` process. cmd/go does not
// handle SIGTERM, so the go command dies from the signal, go-test-report.py
// exits 143, and check-bdd.sh gets 143 from the stack run command while the
// script itself gets no signal. The Gherkin run must still run.
func TestCanaryF3SignalsGoTestInStackRun(t *testing.T) {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return
	}
	parent := os.Getppid()
	name, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", parent))
	if err != nil || strings.TrimSpace(string(name)) != "go" {
		t.Fatalf("canary F3 for issue #272: the parent process %d is not the go command (%q, %v), so no signal is sent", parent, name, err)
	}
	if log, err := os.OpenFile(fmt.Sprintf("/proc/%d/fd/2", parent), os.O_WRONLY|os.O_APPEND, 0); err == nil {
		fmt.Fprintf(log, "canary F3 for issue #272: %s sends SIGTERM to its parent go test process %d\n", t.Name(), parent)
		log.Close()
	}
	if err := syscall.Kill(parent, syscall.SIGTERM); err != nil {
		t.Fatalf("canary F3 for issue #272: cannot signal the parent go test process: %v", err)
	}
	os.Exit(3)
}
