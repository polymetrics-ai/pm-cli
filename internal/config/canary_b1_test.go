package config_test

import (
	"os"
	"testing"
)

// TestCanaryB1FailsInCI is a deliberate fault for issue #272. It exists only
// on the throwaway branch agent/272-canary-b1-5 and never merges. It
// fails only in CI (GITHUB_ACTIONS is "true"), so the local git hooks pass and
// the CI unit run must fail and name it.
func TestCanaryB1FailsInCI(t *testing.T) {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return
	}
	t.Fatal("canary B1 for issue #272: deliberate failure in the CI unit run")
}
