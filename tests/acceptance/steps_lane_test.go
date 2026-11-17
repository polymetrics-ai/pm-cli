package acceptance

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/cucumber/godog"
)

const laneProfileName = "lane"

// laneScenario is the state of the lane steps in one scenario. The lane steps
// use the shared scenario state: the scenario HOME, the pm profile and the
// shared result state of the last pm command, which the shared result steps
// read.
type laneScenario struct {
	lane   *lane
	shared *stackScenario
	// baseline holds the observer row counts from just before the last lane
	// command.
	baseline map[string]int64
}

func registerLaneSteps(sc *godog.ScenarioContext, shared *stackScenario, l *lane) {
	scenario := &laneScenario{lane: l, shared: shared}
	sc.Step(`^the lane runs pm-server on the job-owned stack$`, scenario.laneRuns)
	sc.Step(`^a pm server profile for the lane server$`, scenario.addProfile)
	sc.Step(`^I run "([^"]*)" against the lane server$`, scenario.runAgainstLane)
	sc.Step(`^the observer reads the lane server information through its own HTTPS client, and its instance ID equals the instance ID in the lane command stdout$`, scenario.observerReadsLaneInstanceID)
	sc.Step(`^the observer sees no new row in the PM run and audit tables$`, scenario.observerSeesNoNewRow)
}

func (s *laneScenario) laneRuns(ctx context.Context) error {
	return s.lane.ensure(ctx)
}

// addProfile adds the lane server profile with the real pm binary, in a new
// HOME for this scenario, through the shared profile helper. The helper also
// selects a new workspace ID for the scenario. Part a runs no authenticated
// command, so the server never creates that workspace.
func (s *laneScenario) addProfile(ctx context.Context) error {
	if err := s.lane.running(); err != nil {
		return err
	}
	return s.shared.addServerProfile(ctx, laneProfileName, s.lane.deployment)
}

// runAgainstLane runs the command against the lane server through the shared
// helper, which writes the shared result state. The observer counts the run
// and audit rows just before the command runs.
func (s *laneScenario) runAgainstLane(ctx context.Context, line string) error {
	if err := s.lane.running(); err != nil {
		return err
	}
	baseline, err := s.lane.observer.countRows(ctx)
	if err != nil {
		return err
	}
	if err := s.shared.runAgainst(ctx, line, laneProfileName, s.lane.deployment); err != nil {
		return err
	}
	s.baseline = baseline
	return nil
}

// observerReadsLaneInstanceID is step 29. The observer makes one HTTPS
// request for the server information of the lane pm-server through the lane
// edge, with the helper of step 30 (criteria N22). Its client trusts only the
// lane CA and uses no pm profile and no PM session. The step compares the
// instance ID of the answer with the one instance ID that the lane command
// printed.
func (s *laneScenario) observerReadsLaneInstanceID(ctx context.Context) error {
	if err := s.lane.running(); err != nil {
		return err
	}
	if !s.shared.result.ran {
		return errors.New("no lane command ran")
	}
	printed, err := instanceIDInOutput(s.shared.result.stdout)
	if err != nil {
		return err
	}
	observed, err := readServerInstanceID(ctx, s.lane.deployment.origin, s.lane.deployment.authority.pem)
	if err != nil {
		return err
	}
	if observed != printed {
		return fmt.Errorf("the observer read instance ID %q from the lane pm-server, but the lane command printed %q", observed, printed)
	}
	return nil
}

func (s *laneScenario) observerSeesNoNewRow(ctx context.Context) error {
	if s.baseline == nil {
		return errors.New("the observer has no count from before the lane command")
	}
	after, err := s.lane.observer.countRows(ctx)
	if err != nil {
		return err
	}
	var changed []string
	for _, table := range observedTables {
		if after[table] != s.baseline[table] {
			changed = append(changed, fmt.Sprintf("pm.%s %d -> %d", table, s.baseline[table], after[table]))
		}
	}
	slices.Sort(changed)
	if len(changed) > 0 {
		return fmt.Errorf("the observer sees changed row counts: %s", strings.Join(changed, ", "))
	}
	return nil
}
