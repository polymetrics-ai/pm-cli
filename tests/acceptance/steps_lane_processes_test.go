package acceptance

import (
	"errors"
	"fmt"

	"github.com/cucumber/godog"

	"github.com/polymetrics-ai/pm-cli/tests/starter"
)

// laneWorkerProcess is the method of a lane that starts pm-worker. It returns
// the pm-worker process that the lane started through the shared starter.
// The L2 steps find the lane pm-worker only through this method. A lane that
// has no such method started no pm-worker.
type laneWorkerProcess interface {
	workerProcess() *starter.Process
}

// registerLaneProcessSteps registers the L2 steps (criteria-worker v4). They
// read what the lane starter recorded for each process. They make no new
// observation of a process and do not wait.
func registerLaneProcessSteps(sc *godog.ScenarioContext, scenario *laneScenario) {
	sc.Step(`^the lane received the (pm-server|pm-worker) ready signal$`, scenario.laneReceivedReadySignal)
	sc.Step(`^the lane (pm-server|pm-worker) is still running$`, scenario.laneProcessStillRunning)
}

// laneProcess returns the process that the lane started for the binary name.
func (s *laneScenario) laneProcess(name string) (*starter.Process, error) {
	if err := s.lane.running(); err != nil {
		return nil, err
	}
	var process *starter.Process
	switch name {
	case "pm-server":
		process = s.lane.server
	case "pm-worker":
		worker, ok := any(s.lane).(laneWorkerProcess)
		if !ok {
			return nil, errors.New("the lane started no pm-worker: the lane has no pm-worker process")
		}
		process = worker.workerProcess()
	default:
		return nil, fmt.Errorf("the lane has no process named %q", name)
	}
	if process == nil {
		return nil, fmt.Errorf("the lane started no %s", name)
	}
	return process, nil
}

// laneReceivedReadySignal reads the start outcome that the starter recorded.
// Only the ready line, with or without end of file after it, passes.
func (s *laneScenario) laneReceivedReadySignal(name string) error {
	process, err := s.laneProcess(name)
	if err != nil {
		return err
	}
	report := process.Snapshot()
	if report.Outcome != starter.OutcomeReady && report.Outcome != starter.OutcomeReadyClosed {
		return fmt.Errorf("the lane received no %s ready signal: %s", name, report)
	}
	return nil
}

// laneProcessStillRunning reads the exit observation that the starter
// recorded. It makes no new observation of the process. A process that the
// starter finished with, or whose exit the starter recorded, fails the step.
func (s *laneScenario) laneProcessStillRunning(name string) error {
	process, err := s.laneProcess(name)
	if err != nil {
		return err
	}
	if process.Done() {
		return fmt.Errorf("the starter finished with the lane %s: %s", name, process.Snapshot())
	}
	if report := process.Snapshot(); report.Exited {
		return fmt.Errorf("the lane %s is no longer running: %s", name, report)
	}
	return nil
}
