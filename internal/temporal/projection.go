package temporal

import (
	"context"
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	sdkclient "go.temporal.io/sdk/client"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

type ProjectionDisposition string

const (
	ProjectionCurrent             ProjectionDisposition = "current"
	ProjectionRetryCompleted      ProjectionDisposition = "retry_completed"
	ProjectionRepairedFromHistory ProjectionDisposition = "repaired_from_history"
	ProjectionNeverStarted        ProjectionDisposition = "never_started"
	ProjectionDeferred            ProjectionDisposition = "deferred"
	ProjectionUncertain           ProjectionDisposition = "uncertain"
)

type ProjectionResult struct {
	Disposition   ProjectionDisposition
	WorkflowID    string
	TemporalRunID string
	Generation    uint64
	Condition     contract.OrchestrationCondition
}

// RepairProjection keeps orchestration observations orthogonal to business and
// effect truth. Open workflows receive only the canonical, idempotent final
// projection update. Closed history can repair status/time but cannot fabricate
// an activity, provider success, or pending-capacity release.
func (relay *Relay) RepairProjection(ctx context.Context, route store.WorkflowRoute) (ProjectionResult, error) {
	if route.Claim.Command.Kind != store.WorkflowCommandRepair && route.Claim.Command.Kind != store.WorkflowCommandStartRun {
		return ProjectionResult{}, errors.New("projection repair requires a workflow route")
	}
	handoff, intent, err := relay.sealRoute(ctx, route)
	if err != nil {
		return ProjectionResult{}, err
	}
	binding := handoff.Record.WorkflowBinding
	base := ProjectionResult{WorkflowID: binding.WorkflowID, Generation: binding.Generation}
	if len(binding.Observations) == 0 {
		execution, lookupErr := relay.lookupWorkflowExecution(ctx, route, binding.WorkflowID, "", binding.Origin != contract.RunOriginSchedule)
		if workflowNotFound(lookupErr) {
			if err := store.WithinScope(ctx, relay.store, route.Claim.Scope, route.Actor, func(ctx context.Context, tx *store.Transaction) error {
				_, err := store.PersistNoDispatchWitness(ctx, tx, route.Claim, intent.Digest)
				return err
			}); err != nil {
				return ProjectionResult{}, err
			}
			base.Disposition = ProjectionNeverStarted
			base.Condition = handoff.Record.OrchestrationCondition
			return base, nil
		}
		if workflowEvidenceMismatch(lookupErr) {
			markErr := relay.markUnknown(ctx, route, intent, store.WorkflowWitnessBindingMismatch, "")
			base.Disposition = ProjectionUncertain
			base.Condition = contract.OrchestrationUnknown
			return base, markErr
		}
		if lookupErr != nil {
			return ProjectionResult{}, lookupErr
		}
		input, inputErr := startRunInput(route.Claim, intent)
		if inputErr != nil {
			return ProjectionResult{}, inputErr
		}
		if verifyErr := verifyStartedExecution(execution, input, intent); verifyErr != nil {
			markErr := relay.markUnknown(ctx, route, intent, store.WorkflowWitnessBindingMismatch, execution.RunID)
			base.Disposition = ProjectionUncertain
			base.TemporalRunID = execution.RunID
			base.Condition = contract.OrchestrationUnknown
			return base, markErr
		}
		markErr := relay.markUnknown(ctx, route, intent, store.WorkflowWitnessBindingMismatch, execution.RunID)
		base.Disposition = ProjectionUncertain
		base.TemporalRunID = execution.RunID
		base.Condition = contract.OrchestrationUnknown
		return base, markErr
	}

	currentRunID := binding.CurrentTemporalRunID()
	execution, err := relay.lookupWorkflowExecution(ctx, route, binding.WorkflowID, currentRunID, false)
	if err != nil {
		if workflowNotFound(err) {
			markErr := relay.markUnknown(ctx, route, intent, store.WorkflowWitnessPriorBoundNotFound, currentRunID)
			base.Disposition = ProjectionUncertain
			base.TemporalRunID = currentRunID
			base.Condition = contract.OrchestrationUnknown
			return base, markErr
		}
		if workflowEvidenceMismatch(err) {
			markErr := relay.markUnknown(ctx, route, intent, store.WorkflowWitnessBindingMismatch, currentRunID)
			base.Disposition = ProjectionUncertain
			base.TemporalRunID = currentRunID
			base.Condition = contract.OrchestrationUnknown
			return base, markErr
		}
		return ProjectionResult{}, err
	}
	if err := verifyPersistedChain(execution, binding); err != nil ||
		execution.WorkflowType != intent.Plan.WorkflowType ||
		execution.TaskQueue != intent.Plan.TaskQueue ||
		execution.ExecutionTimeout != temporalDuration(intent.Plan.ExecutionTimeout) {
		markErr := relay.markUnknown(ctx, route, intent, store.WorkflowWitnessBindingMismatch, execution.RunID)
		base.Disposition = ProjectionUncertain
		base.TemporalRunID = execution.RunID
		base.Condition = contract.OrchestrationUnknown
		return base, markErr
	}

	if execution.Status == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		return relay.retryOpenProjection(ctx, route, intent, execution)
	}
	closeEvent, err := relay.historyEvent(ctx, route, execution.WorkflowID, execution.RunID, enumspb.HISTORY_EVENT_FILTER_TYPE_CLOSE_EVENT)
	if err != nil {
		if workflowEvidenceMismatch(err) {
			markErr := relay.markUnknown(ctx, route, intent, store.WorkflowWitnessBindingMismatch, execution.RunID)
			base.Disposition = ProjectionUncertain
			base.TemporalRunID = execution.RunID
			base.Condition = contract.OrchestrationUnknown
			return base, markErr
		}
		return ProjectionResult{}, err
	}
	condition, closedAt, nextRunID, err := classifyCloseEvent(execution.Status, closeEvent)
	if err != nil {
		if workflowEvidenceMismatch(err) {
			markErr := relay.markUnknown(ctx, route, intent, store.WorkflowWitnessBindingMismatch, execution.RunID)
			base.Disposition = ProjectionUncertain
			base.TemporalRunID = execution.RunID
			base.Condition = contract.OrchestrationUnknown
			return base, markErr
		}
		return ProjectionResult{}, err
	}
	if execution.Status == enumspb.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW {
		return relay.followContinuedExecution(ctx, route, handoff, intent, execution, nextRunID, closedAt)
	}
	if closedAt.Before(handoff.Record.ProjectionObservedAt) {
		closedAt = handoff.Record.ProjectionObservedAt
	}
	if _, err := relay.commitProjectionObservation(ctx, route, intent, execution.RunID,
		execution.FirstExecutionRunID, condition, closedAt, store.WorkflowWitnessClosedHistory, true, false); err != nil {
		return ProjectionResult{}, err
	}
	return ProjectionResult{
		Disposition: ProjectionRepairedFromHistory, WorkflowID: execution.WorkflowID,
		TemporalRunID: execution.RunID, Generation: binding.Generation,
		Condition: condition,
	}, nil
}

func (relay *Relay) retryOpenProjection(
	ctx context.Context,
	route store.WorkflowRoute,
	intent store.WorkflowCommandIntent,
	execution workflowExecution,
) (ProjectionResult, error) {
	command, err := projectionRetryCommand(intent)
	if err != nil {
		return ProjectionResult{}, err
	}
	updateID, err := projectionUpdateID(intent)
	if err != nil {
		return ProjectionResult{}, err
	}
	handle, err := routeRPC(ctx, relay, route, func(callCtx context.Context, backend WorkflowClient) (sdkclient.WorkflowUpdateHandle, error) {
		return backend.UpdateWorkflow(callCtx, sdkclient.UpdateWorkflowOptions{
			UpdateID:            updateID,
			WorkflowID:          execution.WorkflowID,
			RunID:               execution.RunID,
			UpdateName:          relay.client.config.ProjectionUpdateName,
			Args:                []any{command},
			WaitForStage:        sdkclient.WorkflowUpdateStageCompleted,
			FirstExecutionRunID: execution.FirstExecutionRunID,
		})
	})
	if err != nil {
		return ProjectionResult{}, err
	}
	if handle == nil || handle.UpdateID() != updateID || handle.WorkflowID() != execution.WorkflowID ||
		handle.RunID() != execution.RunID {
		return ProjectionResult{}, ErrWorkflowBindingMismatch
	}
	if _, err := routeRPC(ctx, relay, route, func(callCtx context.Context, _ WorkflowClient) (struct{}, error) {
		return struct{}{}, handle.Get(callCtx, nil)
	}); err != nil {
		return ProjectionResult{}, err
	}
	if err := relay.deferRoute(ctx, route); err != nil {
		return ProjectionResult{}, err
	}
	return ProjectionResult{
		Disposition: ProjectionRetryCompleted, WorkflowID: execution.WorkflowID,
		TemporalRunID: execution.RunID, Generation: intent.Plan.Generation,
		Condition: contract.OrchestrationRunning,
	}, nil
}

func (relay *Relay) followContinuedExecution(
	ctx context.Context,
	route store.WorkflowRoute,
	handoff store.WorkflowHandoff,
	intent store.WorkflowCommandIntent,
	prior workflowExecution,
	nextRunID string,
	continuedAt time.Time,
) (ProjectionResult, error) {
	if !validTemporalIdentity(nextRunID) {
		markErr := relay.markUnknown(ctx, route, intent, store.WorkflowWitnessBindingMismatch, prior.RunID)
		return ProjectionResult{
			Disposition: ProjectionUncertain, WorkflowID: prior.WorkflowID,
			TemporalRunID: prior.RunID, Generation: intent.Plan.Generation,
			Condition: contract.OrchestrationUnknown,
		}, markErr
	}
	next, err := relay.lookupWorkflowExecution(ctx, route, prior.WorkflowID, nextRunID, false)
	if err != nil {
		if workflowNotFound(err) || workflowEvidenceMismatch(err) {
			markErr := relay.markUnknown(ctx, route, intent, store.WorkflowWitnessBindingMismatch, nextRunID)
			return ProjectionResult{
				Disposition: ProjectionUncertain, WorkflowID: prior.WorkflowID,
				TemporalRunID: nextRunID, Generation: intent.Plan.Generation,
				Condition: contract.OrchestrationUnknown,
			}, markErr
		}
		return ProjectionResult{}, err
	}
	if next.FirstExecutionRunID != prior.FirstExecutionRunID ||
		next.WorkflowType != intent.Plan.WorkflowType ||
		next.TaskQueue != intent.Plan.TaskQueue ||
		next.ExecutionTimeout != temporalDuration(intent.Plan.ExecutionTimeout) {
		markErr := relay.markUnknown(ctx, route, intent, store.WorkflowWitnessBindingMismatch, next.RunID)
		return ProjectionResult{
			Disposition: ProjectionUncertain, WorkflowID: next.WorkflowID,
			TemporalRunID: next.RunID, Generation: intent.Plan.Generation,
			Condition: contract.OrchestrationUnknown,
		}, markErr
	}
	observedAt := next.StartedAt
	if observedAt.Before(continuedAt) {
		observedAt = continuedAt
	}
	if observedAt.Before(handoff.Record.ProjectionObservedAt) {
		observedAt = handoff.Record.ProjectionObservedAt
	}
	condition := orchestrationCondition(next.Status)
	if _, err := relay.commitProjectionObservation(ctx, route, intent, next.RunID,
		next.FirstExecutionRunID, condition, observedAt, "", false, true); err != nil {
		return ProjectionResult{}, err
	}
	return ProjectionResult{
		Disposition: ProjectionDeferred, WorkflowID: next.WorkflowID,
		TemporalRunID: next.RunID, Generation: intent.Plan.Generation,
		Condition: condition,
	}, nil
}

func (relay *Relay) commitProjectionObservation(
	ctx context.Context,
	route store.WorkflowRoute,
	intent store.WorkflowCommandIntent,
	temporalRunID string,
	firstExecutionRunID string,
	condition contract.OrchestrationCondition,
	observedAt time.Time,
	witnessKind store.WorkflowWitnessKind,
	acknowledge bool,
	deferCommand bool,
) (store.OrchestrationObservationCommitResult, error) {
	var result store.OrchestrationObservationCommitResult
	err := store.WithinScope(ctx, relay.store, route.Claim.Scope, route.Actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		result, err = store.CommitOrchestrationObservation(ctx, tx, store.OrchestrationObservationCommit{
			Claim: route.Claim, IntentDigest: intent.Digest,
			TemporalRunID: temporalRunID, FirstExecutionRunID: firstExecutionRunID,
			Condition: condition, ObservedAt: observedAt,
			WitnessKind: witnessKind, Acknowledge: acknowledge,
		})
		if err != nil {
			return err
		}
		if deferCommand {
			return store.DeferWorkflowCommand(ctx, tx, route.Claim, time.Now().UTC().Add(relay.failureDelay))
		}
		return nil
	})
	return result, err
}

func classifyCloseEvent(
	status enumspb.WorkflowExecutionStatus,
	event *historypb.HistoryEvent,
) (contract.OrchestrationCondition, time.Time, string, error) {
	if event == nil || event.GetEventTime() == nil || event.GetEventTime().CheckValid() != nil {
		return "", time.Time{}, "", ErrWorkflowHistoryMissing
	}
	closedAt := event.GetEventTime().AsTime().UTC()
	switch status {
	case enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED:
		if event.GetEventType() != enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_COMPLETED {
			return "", time.Time{}, "", ErrWorkflowBindingMismatch
		}
		return contract.OrchestrationCompleted, closedAt, "", nil
	case enumspb.WORKFLOW_EXECUTION_STATUS_FAILED:
		if event.GetEventType() != enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_FAILED {
			return "", time.Time{}, "", ErrWorkflowBindingMismatch
		}
		return contract.OrchestrationFailed, closedAt, "", nil
	case enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED:
		if event.GetEventType() != enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_CANCELED {
			return "", time.Time{}, "", ErrWorkflowBindingMismatch
		}
		return contract.OrchestrationCancelled, closedAt, "", nil
	case enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED:
		if event.GetEventType() != enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_TERMINATED {
			return "", time.Time{}, "", ErrWorkflowBindingMismatch
		}
		return contract.OrchestrationTerminated, closedAt, "", nil
	case enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT:
		if event.GetEventType() != enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_TIMED_OUT {
			return "", time.Time{}, "", ErrWorkflowBindingMismatch
		}
		return contract.OrchestrationTimedOut, closedAt, "", nil
	case enumspb.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW:
		attributes := event.GetWorkflowExecutionContinuedAsNewEventAttributes()
		if event.GetEventType() != enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_CONTINUED_AS_NEW || attributes == nil {
			return "", time.Time{}, "", ErrWorkflowBindingMismatch
		}
		return contract.OrchestrationProjectionPending, closedAt, attributes.GetNewExecutionRunId(), nil
	default:
		return "", time.Time{}, "", fmt.Errorf("unsupported Temporal close status: %s", status.String())
	}
}
