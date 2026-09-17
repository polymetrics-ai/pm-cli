package workflows

import (
	"errors"

	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type Coordinator struct {
	bootstrapTimeouts contract.ActivityTimeouts
}

func NewCoordinator(timeouts contract.ActivityTimeouts) (*Coordinator, error) {
	if err := timeouts.Validate(); err != nil {
		return nil, err
	}
	return &Coordinator{bootstrapTimeouts: timeouts}, nil
}

func RegisterWorkflows(registry worker.WorkflowRegistry, coordinator *Coordinator, supportsScheduledAdmission bool) error {
	if registry == nil || coordinator == nil {
		return errors.New("workflow registry and coordinator are required")
	}
	registry.RegisterWorkflowWithOptions(
		coordinator.PMRunWorkflow,
		workflow.RegisterOptions{Name: contract.RunWorkflowType},
	)
	if supportsScheduledAdmission {
		registry.RegisterWorkflowWithOptions(
			coordinator.PMScheduledRunWorkflow,
			workflow.RegisterOptions{Name: contract.ScheduledWorkflowType},
		)
	}
	return nil
}

func (coordinator *Coordinator) PMRunWorkflow(
	ctx workflow.Context,
	input contract.StartRunInput,
) (contract.WorkflowProjectionResult, error) {
	if coordinator == nil {
		return contract.WorkflowProjectionResult{}, errors.New("workflow coordinator is required")
	}
	info := workflow.GetInfo(ctx)
	switch input.Origin {
	case contract.RunOriginAPI, contract.RunOriginRecovery:
		if err := input.ValidateRelayStart(); err != nil {
			return contract.WorkflowProjectionResult{}, err
		}
	case contract.RunOriginSchedule:
		if err := input.ValidateScheduledStart(); err != nil {
			return contract.WorkflowProjectionResult{}, err
		}
		if info == nil || info.ContinuedExecutionRunID == "" {
			return contract.WorkflowProjectionResult{}, errors.New("schedule-origin run requires an admitted continued execution")
		}
	default:
		return contract.WorkflowProjectionResult{}, errors.New("workflow start origin is unsupported")
	}
	if err := validateExecutionIdentity(ctx, input.WorkflowID, contract.RunWorkflowType); err != nil {
		return contract.WorkflowProjectionResult{}, err
	}
	return coordinator.CoordinateRun(ctx, input)
}

func (coordinator *Coordinator) CoordinateRun(
	ctx workflow.Context,
	input contract.StartRunInput,
) (contract.WorkflowProjectionResult, error) {
	if coordinator == nil {
		return contract.WorkflowProjectionResult{}, errors.New("workflow coordinator is required")
	}
	if err := input.Validate(); err != nil {
		return contract.WorkflowProjectionResult{}, err
	}
	if err := validateExecutionIdentity(ctx, input.WorkflowID, ""); err != nil {
		return contract.WorkflowProjectionResult{}, err
	}

	state := &coordinatorState{}
	if err := coordinator.installHandlers(ctx, input, state); err != nil {
		return contract.WorkflowProjectionResult{}, err
	}
	expectedRun := input.RunRef()
	for {
		loadTimeouts := coordinator.bootstrapTimeouts
		if state.ready {
			loadTimeouts = state.timeouts
		}
		snapshot, err := loadSnapshot(ctx, expectedRun, loadTimeouts)
		if err != nil {
			return contract.WorkflowProjectionResult{}, err
		}
		if err := bindSnapshot(state, expectedRun, snapshot); err != nil {
			return contract.WorkflowProjectionResult{}, err
		}

		if err := applySnapshotCommands(ctx, state, snapshot.Commands); err != nil {
			return contract.WorkflowProjectionResult{}, err
		}
		if err := drainControlSignals(ctx, state); err != nil {
			return contract.WorkflowProjectionResult{}, err
		}

		if snapshot.Done || state.cancelRequested {
			if err := drainBeforeExit(ctx, state); err != nil {
				return contract.WorkflowProjectionResult{}, err
			}
			return executeProjection(ctx, contract.WorkflowProjectionRequest{Run: expectedRun}, state.timeouts)
		}
		if shouldContinue(ctx) {
			if err := continueAtBoundary(ctx, input, state); err != nil {
				return contract.WorkflowProjectionResult{}, err
			}
		}

		if !state.cancelRequested && len(snapshot.Units) != 0 {
			if err := executeUnits(ctx, state, snapshot.Units, snapshot.Fanout); err != nil {
				return contract.WorkflowProjectionResult{}, err
			}
		}
		if err := drainControlSignals(ctx, state); err != nil {
			return contract.WorkflowProjectionResult{}, err
		}
		if shouldContinue(ctx) {
			if err := continueAtBoundary(ctx, input, state); err != nil {
				return contract.WorkflowProjectionResult{}, err
			}
		}
		if len(snapshot.Units) == 0 || state.cancelRequested {
			if err := waitForDurableWork(ctx, state); err != nil {
				return contract.WorkflowProjectionResult{}, err
			}
		}
	}
}

func validateExecutionIdentity(ctx workflow.Context, workflowID, workflowType string) error {
	if ctx == nil {
		return errors.New("workflow context is required")
	}
	info := workflow.GetInfo(ctx)
	if info == nil || info.WorkflowExecution.ID != workflowID || info.WorkflowExecution.RunID == "" || info.Namespace == "" {
		return errors.New("actual Temporal execution identity does not match the admitted workflow")
	}
	if workflowType != "" && info.WorkflowType.Name != workflowType {
		return errors.New("actual Temporal workflow type is invalid")
	}
	return nil
}

func loadSnapshot(
	ctx workflow.Context,
	run contract.WorkflowRunRef,
	timeouts contract.ActivityTimeouts,
) (contract.WorkflowRunSnapshot, error) {
	if err := run.Validate(); err != nil {
		return contract.WorkflowRunSnapshot{}, err
	}
	activityCtx, err := activityContext(ctx, timeouts)
	if err != nil {
		return contract.WorkflowRunSnapshot{}, err
	}
	var snapshot contract.WorkflowRunSnapshot
	if err := workflow.ExecuteActivity(activityCtx, contract.LoadActivityName, run).Get(activityCtx, &snapshot); err != nil {
		return contract.WorkflowRunSnapshot{}, err
	}
	if err := snapshot.Validate(); err != nil {
		return contract.WorkflowRunSnapshot{}, err
	}
	return snapshot, nil
}

func bindSnapshot(
	state *coordinatorState,
	expected contract.WorkflowRunRef,
	snapshot contract.WorkflowRunSnapshot,
) error {
	if snapshot.Run != expected {
		return errors.New("loaded workflow snapshot does not match the admitted run")
	}
	if !state.ready {
		state.run = snapshot.Run
		state.timeouts = snapshot.Timeouts
		state.originalDeadline = snapshot.Deadline
		state.cancelRequested = snapshot.CancelRequested
		state.ready = true
		return nil
	}
	if snapshot.Timeouts != state.timeouts || !snapshot.Deadline.Equal(state.originalDeadline) {
		return errors.New("loaded workflow snapshot rewrote immutable execution controls")
	}
	if snapshot.CancelRequested {
		state.cancelRequested = true
	}
	return nil
}

func applySnapshotCommands(
	ctx workflow.Context,
	state *coordinatorState,
	commands []contract.WorkflowCommandRef,
) error {
	for _, command := range commands {
		result, err := executeCommand(ctx, state.run, command, state.timeouts)
		if err != nil {
			return err
		}
		if result.CancelRequested {
			state.cancelRequested = true
		}
	}
	return nil
}

func executeUnits(
	ctx workflow.Context,
	state *coordinatorState,
	units []contract.WorkflowUnitRef,
	fanout uint32,
) error {
	if fanout == 0 || fanout > contract.MaxWorkflowFanout {
		return errors.New("workflow fanout is outside the admitted bound")
	}
	activityCtx, err := executionActivityContext(ctx, state.timeouts)
	if err != nil {
		return err
	}
	selector := workflow.NewSelector(ctx)
	next := 0
	active := 0
	var firstErr error
	schedule := func(unit contract.WorkflowUnitRef) {
		request := contract.WorkflowUnitRequest{Run: state.run, Unit: unit}
		future := workflow.ExecuteActivity(activityCtx, contract.ExecuteActivityName, request)
		active++
		selector.AddFuture(future, func(completed workflow.Future) {
			defer func() { active-- }()
			var result contract.WorkflowUnitResult
			if err := completed.Get(activityCtx, &result); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			if err := result.Validate(); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			if result.TaskID != unit.TaskID && firstErr == nil {
				firstErr = errors.New("unit activity returned a mismatched task identity")
			}
		})
	}
	capacity := int(fanout)
	for next < len(units) && active < capacity && !state.cancelRequested {
		schedule(units[next])
		next++
	}
	for active != 0 {
		selector.Select(ctx)
		if firstErr == nil {
			if err := drainControlSignals(ctx, state); err != nil {
				firstErr = err
			}
		}
		for firstErr == nil && next < len(units) && active < capacity && !state.cancelRequested {
			schedule(units[next])
			next++
		}
	}
	return firstErr
}

func drainBeforeExit(ctx workflow.Context, state *coordinatorState) error {
	if err := drainControlSignals(ctx, state); err != nil {
		return err
	}
	if err := workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) }); err != nil {
		return err
	}
	return drainControlSignals(ctx, state)
}

func continueAtBoundary(
	ctx workflow.Context,
	input contract.StartRunInput,
	state *coordinatorState,
) error {
	if err := drainBeforeExit(ctx, state); err != nil {
		return err
	}
	return ContinueSafely(ctx, input)
}

func waitForDurableWork(ctx workflow.Context, state *coordinatorState) error {
	remaining := state.originalDeadline.Sub(workflow.Now(ctx))
	if remaining <= 0 {
		return nil
	}
	wait := state.timeouts.Heartbeat
	if remaining < wait {
		wait = remaining
	}
	timerCtx, cancelTimer := workflow.WithCancel(ctx)
	timer := workflow.NewTimer(timerCtx, wait)
	selector := workflow.NewSelector(ctx)
	var notification *contract.WorkflowCommandRef
	selector.AddReceive(state.controlSignals, func(channel workflow.ReceiveChannel, more bool) {
		if !more {
			return
		}
		var command contract.WorkflowCommandRef
		channel.Receive(ctx, &command)
		notification = &command
	})
	selector.AddFuture(timer, func(workflow.Future) {})
	selector.Select(ctx)
	cancelTimer()
	if notification == nil {
		return nil
	}
	if err := notification.Validate(); err != nil {
		return err
	}
	result, err := executeCommand(ctx, state.run, *notification, state.timeouts)
	if err != nil {
		return err
	}
	if result.CancelRequested {
		state.cancelRequested = true
	}
	return nil
}
