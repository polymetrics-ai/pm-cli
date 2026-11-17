package acceptance

import "testing"

// TestCanaryN4Skips is a deliberate fault for the #286b canary N4 (issue
// #350). It exists only on the throwaway branch agent/286b-canary-n4-3 and never
// merges. It is a Go test in the Gherkin package that calls t.Skip. The
// Gherkin run has --fail-on-skip, so the run must fail and name this test in
// the log and in the job summary, with valid scenario totals.
func TestCanaryN4Skips(t *testing.T) {
	t.Skip("canary N4 for issue #350: a skipped Go test in the Gherkin package")
}
