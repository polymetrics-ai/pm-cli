package store_test

import "testing"

// TestCanaryK1FailsInShard1 is a deliberate fault for the #286a canary K1. It
// exists only on the throwaway branch agent/286a-canary-k1-3 and never merges.
// internal/store is a stack package of shard 1, so the git hooks do not run
// it. Only job stack-1 must fail, and the aggregate bdd job must name it.
func TestCanaryK1FailsInShard1(t *testing.T) {
	t.Fatal("canary K1 for issue #286: deliberate failure in shard 1")
}
