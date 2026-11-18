package workflows

import (
	"errors"
	"time"

	"go.temporal.io/sdk/temporal"
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
	// Retained histories may reload after the deadline before observing Done.
	// Preserve those recorded commands while new executions project at expiry.
	deadlineProjection := workflow.GetVersion(ctx, contract.WorkflowRunDeadlineProjectionChangeID, workflow.DefaultVersion, contract.WorkflowRunDeadlineProjectionVersion) != workflow.DefaultVersion
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
		if deadlineProjection && !workflow.Now(ctx).Before(state.originalDeadline) {
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
			failedUnit, err := executeUnits(ctx, state, snapshot.Units, snapshot.Fanout)
			if err != nil {
				endRunAfterUnitFailure(ctx, state, failedUnit, err)
				return contract.WorkflowProjectionResult{}, err
			}
		}
		if err := drainControlSignals(ctx, state); err != nil {
			return contract.WorkflowProjectionResult{}, err
		}
		if deadlineProjection && !workflow.Now(ctx).Before(state.originalDeadline) {
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
		if len(snapshot.Units) == 0 || state.cancelRequested {
			if err := waitForDurableWork(ctx, state, snapshot.RetryNotBefore); err != nil {
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

// executeUnits returns the first error. When that error comes from a unit
// activity that failed, it also returns that unit.
func executeUnits(
	ctx workflow.Context,
	state *coordinatorState,
	units []contract.WorkflowUnitRef,
	fanout uint32,
) (*contract.WorkflowUnitRef, error) {
	if fanout == 0 || fanout > contract.MaxWorkflowFanout {
		return nil, errors.New("workflow fanout is outside the admitted bound")
	}
	activityCtx, err := executionActivityContext(ctx, state.timeouts)
	if err != nil {
		return nil, err
	}
	selector := workflow.NewSelector(ctx)
	next := 0
	active := 0
	var firstErr error
	var failedUnit *contract.WorkflowUnitRef
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
					failedUnit = &unit
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
	return failedUnit, firstErr
}

// endRunAfterUnitFailure ends the durable run after a unit activity failed and
// Temporal will not run that activity again. The end activity reads the effect
// ledger and writes only the run record. It sends no provider request, and an
// unknown effect keeps the run reconciling.
//
// The workflow still fails with the unit error, so the Temporal close status
// does not change. A drain or end failure stays in the workflow history and
// does not replace the unit error.
func endRunAfterUnitFailure(
	ctx workflow.Context,
	state *coordinatorState,
	failedUnit *contract.WorkflowUnitRef,
	unitErr error,
) {
	var activityErr *temporal.ActivityError
	if failedUnit == nil || !errors.As(unitErr, &activityErr) || ctx.Err() != nil {
		return
	}
	// A history from before this step failed the workflow here. It replays
	// without the step.
	if workflow.GetVersion(
		ctx, contract.WorkflowUnitFailureEndChangeID,
		workflow.DefaultVersion, contract.WorkflowUnitFailureEndVersion,
	) == workflow.DefaultVersion {
		return
	}
	// Apply a pending cancellation first, as the other exits do. A failed drain
	// does not stop the end: the end activity decides from the locked run row
	// and the effect ledger, not from workflow memory. The log line gives an
	// operator the drain error, also for a command that fails Validate.
	if err := drainBeforeExit(ctx, state); err != nil {
		workflow.GetLogger(ctx).Warn(
			"control drain failed before the run end after a unit activity failure",
			"RunRef", state.run.RunID, "Error", err,
		)
	}
	if ctx.Err() != nil {
		return
	}
	// The end result does not change the close cause (R4). A result that does
	// not name this run still gets a warning, with no value from the result.
	// An end activity failure stays in the workflow history only.
	err := executeUnitFailureEnd(ctx, contract.WorkflowUnitRequest{Run: state.run, Unit: *failedUnit}, state.timeouts)
	if errors.Is(err, errUnitFailureEndResult) {
		workflow.GetLogger(ctx).Warn(
			"the run end activity returned a result that does not name this run",
			"RunRef", state.run.RunID,
		)
	}
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

func waitForDurableWork(
	ctx workflow.Context,
	state *coordinatorState,
	retryNotBefore *time.Time,
) error {
	now := workflow.Now(ctx)
	wakeAt := now.Add(state.timeouts.Heartbeat)
	if state.originalDeadline.Before(wakeAt) {
		wakeAt = state.originalDeadline
	}
	if retryNotBefore != nil && retryNotBefore.Before(wakeAt) {
		wakeAt = *retryNotBefore
	}
	wait := wakeAt.Sub(now)
	if wait <= 0 {
		return nil
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
