package temporal

import (
	"context"
	"errors"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// NewWorkflowContinuationObserver binds one borrowed Temporal client to the
// configured namespace. The returned observer proves an actual running
// Continue-As-New member from Temporal-owned Describe and history evidence; it
// does not trust workflow payload lineage and does not own or close the client.
func NewWorkflowContinuationObserver(
	backend WorkflowClient,
	namespace string,
	requestTimeout time.Duration,
) (contract.WorkflowContinuationObserver, error) {
	if backend == nil {
		return nil, errors.New("Temporal continuation observer client is required")
	}
	if !validTemporalIdentity(namespace) {
		return nil, errors.New("Temporal continuation observer namespace is invalid")
	}
	if requestTimeout <= 0 || requestTimeout > 5*time.Minute {
		return nil, errors.New("Temporal continuation observer timeout is invalid")
	}

	return func(
		ctx context.Context,
		execution contract.WorkflowExecution,
	) (contract.WorkflowContinuationObservation, error) {
		if ctx == nil {
			return contract.WorkflowContinuationObservation{}, errors.New("Temporal continuation observation context is required")
		}
		if err := execution.Validate(); err != nil || execution.Namespace != namespace {
			return contract.WorkflowContinuationObservation{}, ErrWorkflowBindingMismatch
		}
		observationCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		defer cancel()
		if err := observationCtx.Err(); err != nil {
			return contract.WorkflowContinuationObservation{}, err
		}

		describe := func(
			callCtx context.Context,
			workflowID string,
			runID string,
		) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
			return backend.DescribeWorkflowExecution(callCtx, workflowID, runID)
		}
		historyEvent := func(
			callCtx context.Context,
			workflowID string,
			runID string,
			filter enumspb.HistoryEventFilterType,
		) (*historypb.HistoryEvent, error) {
			return readWorkflowHistoryEvent(callCtx, backend, workflowID, runID, filter)
		}

		current, err := readWorkflowExecution(
			observationCtx,
			execution.WorkflowID,
			execution.RunID,
			false,
			describe,
			historyEvent,
		)
		if err != nil {
			return contract.WorkflowContinuationObservation{}, err
		}
		if current.Status != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING ||
			current.RunID == current.FirstExecutionRunID ||
			!validTemporalIdentity(current.PreviousRunID) ||
			current.PreviousRunID == current.RunID ||
			current.WorkflowType != contract.RunWorkflowType ||
			current.TaskQueue != contract.ControlTaskQueue ||
			current.ExecutionTimeout <= 0 {
			return contract.WorkflowContinuationObservation{}, ErrWorkflowBindingMismatch
		}

		predecessor, err := readWorkflowExecution(
			observationCtx,
			execution.WorkflowID,
			current.PreviousRunID,
			false,
			describe,
			historyEvent,
		)
		if err != nil {
			return contract.WorkflowContinuationObservation{}, err
		}
		if predecessor.Status != enumspb.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW ||
			predecessor.FirstExecutionRunID != current.FirstExecutionRunID ||
			predecessor.TaskQueue != contract.ControlTaskQueue ||
			predecessor.ExecutionTimeout != current.ExecutionTimeout {
			return contract.WorkflowContinuationObservation{}, ErrWorkflowBindingMismatch
		}
		if predecessor.RunID == predecessor.FirstExecutionRunID {
			if predecessor.WorkflowType != contract.RunWorkflowType &&
				predecessor.WorkflowType != contract.ScheduledWorkflowType {
				return contract.WorkflowContinuationObservation{}, ErrWorkflowBindingMismatch
			}
		} else if predecessor.WorkflowType != contract.RunWorkflowType {
			return contract.WorkflowContinuationObservation{}, ErrWorkflowBindingMismatch
		}

		closeEvent, err := historyEvent(
			observationCtx,
			predecessor.WorkflowID,
			predecessor.RunID,
			enumspb.HISTORY_EVENT_FILTER_TYPE_CLOSE_EVENT,
		)
		if err != nil {
			return contract.WorkflowContinuationObservation{}, err
		}
		condition, _, nextRunID, err := classifyCloseEvent(predecessor.Status, closeEvent)
		if err != nil {
			return contract.WorkflowContinuationObservation{}, err
		}
		if condition != contract.OrchestrationProjectionPending ||
			!validTemporalIdentity(nextRunID) || nextRunID != current.RunID {
			return contract.WorkflowContinuationObservation{}, ErrWorkflowBindingMismatch
		}

		observation := contract.WorkflowContinuationObservation{
			Execution: contract.WorkflowExecution{
				Namespace: namespace, WorkflowID: current.WorkflowID, RunID: current.RunID,
			},
			FirstExecutionRunID: current.FirstExecutionRunID,
			PreviousRunID:       current.PreviousRunID,
			WorkflowType:        current.WorkflowType,
			TaskQueue:           current.TaskQueue,
			ExecutionTimeout:    current.ExecutionTimeout,
		}
		if err := observation.Validate(); err != nil {
			return contract.WorkflowContinuationObservation{}, ErrWorkflowBindingMismatch
		}
		return observation, nil
	}, nil
}
