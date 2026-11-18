package workflows_test

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"testing"
)

// TestCanaryK5WaitsForCancellation is a deliberate fault for the #286a canary
// K5. It exists only on the throwaway branch agent/286a-canary-k5-3 and never
// merges. Only in CI (GITHUB_ACTIONS is "true"), it waits until the test binary
// gets SIGTERM or SIGINT. It has no timer and never ends by itself, so the
// shard is still in its stack run when a second push cancels the run. The
// +3600 s stop of the stack run also ends it.
func TestCanaryK5WaitsForCancellation(t *testing.T) {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	<-ctx.Done()
	t.Log("canary K5 for issue #286: the test binary got SIGTERM or SIGINT")
}
