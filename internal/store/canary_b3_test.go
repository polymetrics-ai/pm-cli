package store_test

import "testing"

// TestCanaryB3SkipsInStackRun is a deliberate fault for issue #272. It exists
// only on the throwaway branch agent/272-canary-b3-5 and never merges.
// The CI stack run must fail and name the skip.
func TestCanaryB3SkipsInStackRun(t *testing.T) {
	t.Skip("canary B3 for issue #272: deliberate skip in the CI stack run")
}
