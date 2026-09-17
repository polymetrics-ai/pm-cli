package temporal

import (
	"context"
	"errors"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/serviceerror"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/converter"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

var (
	ErrWorkflowBindingMismatch = errors.New("Temporal workflow does not match sealed PM binding")
	ErrWorkflowHistoryMissing  = errors.New("Temporal workflow history is unavailable")
)

type workflowExecution struct {
	WorkflowID          string
	RunID               string
	FirstExecutionRunID string
	PreviousRunID       string
	WorkflowType        string
	TaskQueue           string
	ExecutionTimeout    time.Duration
	Status              enumspb.WorkflowExecutionStatus
	StartedAt           time.Time
	StartInput          contract.StartRunInput
	HasStartInput       bool
}

type describeWorkflowExecutionFunc func(
	context.Context,
	string,
	string,
) (*workflowservice.DescribeWorkflowExecutionResponse, error)

type workflowHistoryEventFunc func(
	context.Context,
	string,
	string,
	enumspb.HistoryEventFilterType,
) (*historypb.HistoryEvent, error)

func (relay *Relay) lookupWorkflowExecution(
	ctx context.Context,
	route store.WorkflowRoute,
	workflowID string,
	runID string,
	decodeStartInput bool,
) (workflowExecution, error) {
	return readWorkflowExecution(
		ctx,
		workflowID,
		runID,
		decodeStartInput,
		func(
			callCtx context.Context,
			requestWorkflowID string,
			requestRunID string,
		) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
			return routeRPC(callCtx, relay, route, func(
				rpcCtx context.Context,
				backend WorkflowClient,
			) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
				return backend.DescribeWorkflowExecution(rpcCtx, requestWorkflowID, requestRunID)
			})
		},
		func(
			callCtx context.Context,
			requestWorkflowID string,
			requestRunID string,
			filter enumspb.HistoryEventFilterType,
		) (*historypb.HistoryEvent, error) {
			return relay.historyEvent(callCtx, route, requestWorkflowID, requestRunID, filter)
		},
	)
}

func readWorkflowExecution(
	ctx context.Context,
	workflowID string,
	runID string,
	decodeStartInput bool,
	describe describeWorkflowExecutionFunc,
	historyEvent workflowHistoryEventFunc,
) (workflowExecution, error) {
	if ctx == nil || describe == nil || historyEvent == nil {
		return workflowExecution{}, errors.New("Temporal execution evidence reader is invalid")
	}
	response, err := describe(ctx, workflowID, runID)
	if err != nil {
		return workflowExecution{}, err
	}
	if response == nil || response.GetWorkflowExecutionInfo() == nil ||
		response.GetWorkflowExecutionInfo().GetExecution() == nil {
		return workflowExecution{}, ErrWorkflowHistoryMissing
	}
	info := response.GetWorkflowExecutionInfo()
	execution := workflowExecution{
		WorkflowID: info.GetExecution().GetWorkflowId(),
		RunID:      info.GetExecution().GetRunId(),
		Status:     info.GetStatus(),
	}
	if execution.WorkflowID != workflowID || !validTemporalIdentity(execution.RunID) ||
		runID != "" && execution.RunID != runID {
		return workflowExecution{}, ErrWorkflowBindingMismatch
	}

	startedEvent, err := historyEvent(
		ctx,
		execution.WorkflowID,
		execution.RunID,
		enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT,
	)
	if err != nil {
		return workflowExecution{}, err
	}
	if startedEvent.GetEventType() != enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED ||
		startedEvent.GetWorkflowExecutionStartedEventAttributes() == nil {
		return workflowExecution{}, ErrWorkflowHistoryMissing
	}
	attributes := startedEvent.GetWorkflowExecutionStartedEventAttributes()
	execution.FirstExecutionRunID = attributes.GetFirstExecutionRunId()
	execution.PreviousRunID = attributes.GetContinuedExecutionRunId()
	execution.WorkflowType = attributes.GetWorkflowType().GetName()
	execution.TaskQueue = attributes.GetTaskQueue().GetName()
	if !validTemporalIdentity(execution.FirstExecutionRunID) ||
		!validTemporalIdentity(execution.WorkflowType) ||
		!validTemporalIdentity(execution.TaskQueue) {
		return workflowExecution{}, ErrWorkflowHistoryMissing
	}
	if timeout := attributes.GetWorkflowExecutionTimeout(); timeout != nil {
		if err := timeout.CheckValid(); err != nil {
			return workflowExecution{}, ErrWorkflowHistoryMissing
		}
		execution.ExecutionTimeout = timeout.AsDuration()
	}
	if startedEvent.GetEventTime() == nil || startedEvent.GetEventTime().CheckValid() != nil {
		return workflowExecution{}, ErrWorkflowHistoryMissing
	}
	execution.StartedAt = startedEvent.GetEventTime().AsTime().UTC()
	if decodeStartInput {
		inputAttributes := attributes
		if execution.RunID != execution.FirstExecutionRunID {
			firstEvent, err := historyEvent(
				ctx,
				execution.WorkflowID,
				execution.FirstExecutionRunID,
				enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT,
			)
			if err != nil {
				return workflowExecution{}, err
			}
			if firstEvent.GetEventType() != enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED ||
				firstEvent.GetWorkflowExecutionStartedEventAttributes() == nil {
				return workflowExecution{}, ErrWorkflowHistoryMissing
			}
			inputAttributes = firstEvent.GetWorkflowExecutionStartedEventAttributes()
			if inputAttributes.GetFirstExecutionRunId() != execution.FirstExecutionRunID ||
				inputAttributes.GetWorkflowType().GetName() != execution.WorkflowType ||
				inputAttributes.GetTaskQueue().GetName() != execution.TaskQueue {
				return workflowExecution{}, ErrWorkflowBindingMismatch
			}
			firstTimeout := inputAttributes.GetWorkflowExecutionTimeout()
			if firstTimeout == nil || firstTimeout.CheckValid() != nil ||
				firstTimeout.AsDuration() != execution.ExecutionTimeout {
				return workflowExecution{}, ErrWorkflowBindingMismatch
			}
		}
		payloads := inputAttributes.GetInput().GetPayloads()
		if len(payloads) != 1 {
			return workflowExecution{}, ErrWorkflowBindingMismatch
		}
		payload := payloads[0]
		if string(payload.GetMetadata()[converter.MetadataEncoding]) != converter.MetadataEncodingJSON {
			return workflowExecution{}, ErrWorkflowBindingMismatch
		}
		var decoded contract.StartRunInput
		if err := contract.DecodeStrictRequiredObject(
			payload.GetData(),
			&decoded,
			"schema", "scope", "run_id", "command_id", "workflow_id",
			"generation", "origin", "intent_digest",
		); err != nil {
			return workflowExecution{}, ErrWorkflowBindingMismatch
		}
		if err := decoded.ValidateRelayStart(); err != nil {
			return workflowExecution{}, ErrWorkflowBindingMismatch
		}
		execution.StartInput = decoded
		execution.HasStartInput = true
	}
	return execution, nil
}

func (relay *Relay) historyEvent(
	ctx context.Context,
	route store.WorkflowRoute,
	workflowID string,
	runID string,
	filter enumspb.HistoryEventFilterType,
) (*historypb.HistoryEvent, error) {
	return routeRPC(ctx, relay, route, func(
		callCtx context.Context,
		backend WorkflowClient,
	) (*historypb.HistoryEvent, error) {
		return readWorkflowHistoryEvent(callCtx, backend, workflowID, runID, filter)
	})
}

func readWorkflowHistoryEvent(
	ctx context.Context,
	backend WorkflowClient,
	workflowID string,
	runID string,
	filter enumspb.HistoryEventFilterType,
) (*historypb.HistoryEvent, error) {
	if ctx == nil || backend == nil {
		return nil, errors.New("Temporal history reader is invalid")
	}
	iterator := backend.GetWorkflowHistory(ctx, workflowID, runID, false, filter)
	if iterator == nil || !iterator.HasNext() {
		return nil, ErrWorkflowHistoryMissing
	}
	event, err := iterator.Next()
	if err != nil {
		if workflowNotFound(err) {
			return nil, ErrWorkflowHistoryMissing
		}
		return nil, err
	}
	if event == nil {
		return nil, ErrWorkflowHistoryMissing
	}
	return event, nil
}

func verifyStartedExecution(
	execution workflowExecution,
	input contract.StartRunInput,
	intent store.WorkflowCommandIntent,
) error {
	if !execution.HasStartInput || execution.StartInput != input ||
		execution.WorkflowID != intent.Plan.WorkflowID ||
		execution.WorkflowType != intent.Plan.WorkflowType ||
		execution.TaskQueue != intent.Plan.TaskQueue ||
		execution.ExecutionTimeout != temporalDuration(intent.Plan.ExecutionTimeout) ||
		!validTemporalIdentity(execution.RunID) || !validTemporalIdentity(execution.FirstExecutionRunID) {
		return ErrWorkflowBindingMismatch
	}
	return nil
}

func verifyPersistedChain(execution workflowExecution, binding contract.WorkflowBinding) error {
	if execution.WorkflowID != binding.WorkflowID || len(binding.Observations) == 0 ||
		execution.FirstExecutionRunID != binding.Observations[0].TemporalRunID {
		return ErrWorkflowBindingMismatch
	}
	current := binding.CurrentTemporalRunID()
	if execution.RunID != current {
		return ErrWorkflowBindingMismatch
	}
	return nil
}

func workflowNotFound(err error) bool {
	var missing *serviceerror.NotFound
	return errors.As(err, &missing)
}

func workflowEvidenceMismatch(err error) bool {
	return errors.Is(err, ErrWorkflowBindingMismatch) || errors.Is(err, ErrWorkflowHistoryMissing)
}
