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

// registerLaneProcessSteps registers the L2 steps (criteria-worker v4). The
// ready-signal step reads what the lane starter recorded. The still-running
// step makes one observation of the process exit after the ready signal.
// Neither step waits.
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

// laneProcessStillRunning makes one observation of the process exit, with
// no wait, after the ready signal of the process. Running does not depend on
// the reaper goroutine, so a process that exited after its ready line fails
// the step. The step reads the starter report only for its message.
func (s *laneScenario) laneProcessStillRunning(name string) error {
	process, err := s.laneProcess(name)
	if err != nil {
		return err
	}
	// R4: no observation of the process before its ready signal.
	if report := process.Snapshot(); report.Outcome != starter.OutcomeReady && report.Outcome != starter.OutcomeReadyClosed {
		return fmt.Errorf("the lane received no %s ready signal, so the step makes no observation of it: %s", name, report)
	}
	if process.Done() || !process.Running() {
		return fmt.Errorf("the lane %s is no longer running: %s", name, process.Snapshot())
	}
	return nil
}
