package config_test

import (
	"os"
	"testing"
)

// TestCanaryB3SkipsInCI is a deliberate fault for issue #272. It exists only
// on the throwaway branch agent/272-canary-b3-5 and never merges. It
// calls t.Skip only in CI (GITHUB_ACTIONS is "true"), so the local git hooks
// see a passing test, and the CI unit run must fail and name the skip.
func TestCanaryB3SkipsInCI(t *testing.T) {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return
	}
	t.Skip("canary B3 for issue #272: deliberate skip in the CI unit run")
}
