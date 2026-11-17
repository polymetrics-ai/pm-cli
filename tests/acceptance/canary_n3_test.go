package acceptance

import (
	"context"

	"github.com/cucumber/godog"
)

// registerCanaryN3Hook is a deliberate fault for the #286b canary N3 (issue
// #350). It exists only on the throwaway branch agent/286b-canary-n3-3 and never
// merges. A Before hook returns godog.ErrSkip for each scenario with the tag
// @canary-n3-skip, so that scenario is skipped as a whole. The other scenarios
// pass. The reporter must fail the run and name the scenario in the log and in
// the job summary.
func registerCanaryN3Hook(sc *godog.ScenarioContext) {
	sc.Before(func(ctx context.Context, scenario *godog.Scenario) (context.Context, error) {
		for _, tag := range scenario.Tags {
			if tag.Name == "@canary-n3-skip" {
				return ctx, godog.ErrSkip
			}
		}
		return ctx, nil
	})
}
