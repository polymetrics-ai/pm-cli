package acceptance

import "os"

// init is a deliberate fault for the #286b canary N1 (issue #350). It exists
// only on the throwaway branch agent/286b-canary-n1-3 and never merges.
// TestFeatures reads PM_ACCEPTANCE_TAGS as its godog tag filter. No scenario
// has this tag, so the Gherkin run selects 0 scenarios. The reporter must fail
// the run and name "0 scenarios ran" in the log and in the job summary.
func init() {
	os.Setenv("PM_ACCEPTANCE_TAGS", "@canary-n1-no-scenario-has-this-tag")
}
