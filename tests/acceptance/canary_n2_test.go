package acceptance

import "github.com/cucumber/godog"

// registerCanaryN2Steps is a deliberate fault for the #286b canary N2 (issue
// #350). It exists only on the throwaway branch agent/286c-canary-n2-3 and never
// merges. Its one step returns godog.ErrSkip, so the step is skipped inside a
// scenario whose other steps pass. The reporter must fail the run and name the
// scenario and the step in the log and in the job summary.
func registerCanaryN2Steps(sc *godog.ScenarioContext) {
	sc.Step(`^the canary N2 step is skipped$`, func() error {
		return godog.ErrSkip
	})
}
