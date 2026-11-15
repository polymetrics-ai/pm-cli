package store_test

import "testing"

// TestCanaryB2FailsInStackRun is a deliberate fault for issue #272. It exists
// only on the throwaway branch agent/272-ci-go-tests-canary and never merges.
// internal/store is a stack package, so the git hooks do not run it. The CI
// stack run must fail and name it, and the Gherkin run must still run and
// report (E4, stack run fails and Gherkin run passes).
func TestCanaryB2FailsInStackRun(t *testing.T) {
	t.Fatal("canary B2 for issue #272: deliberate failure in the CI stack run")
}
