package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	pmruntime "github.com/polymetrics-ai/pm-cli/internal/runtime"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

func workerHTTPRetryPlan(
	plan workerUnitPlan,
	observation ExecutionObservation,
	outcomes []contract.MemberOutcome,
	settlementRef string,
) (*store.WorkerRetryPlan, error) {
	httpObservation := observation.HTTP
	if httpObservation == nil || !httpObservation.Retry.Eligible {
		return nil, nil
	}
	if httpObservation.Retry.RequiresCondition {
		return nil, nil
	}
	if plan.reconcile || plan.handler.Kind != HandlerHTTP || observation.Result != nil ||
		plan.dispatch.State != store.DispatchPossiblySent || len(plan.dispatch.Attempts) != 1 ||
		plan.dispatch.AttemptNumber >= plan.request.HTTPPolicy.MaxAttempts ||
		!httpObservation.PhysicalSend {
		return nil, workerFailure(ErrWorkerConflict, errors.New("HTTP retry lacks one fresh bounded physical attempt"))
	}
	if httpObservation.Cooldown.Present && !httpObservation.Cooldown.Usable {
		return nil, nil
	}
	if httpObservation.ObservedAt.IsZero() || httpObservation.ObservedAt.Location() != time.UTC {
		return nil, workerFailure(ErrWorkerConflict, errors.New("HTTP retry observation time is not canonical UTC"))
	}
	if err := plan.request.HTTPPolicy.ValidateForOperation(plan.request.Operation); err != nil {
		return nil, workerFailure(ErrWorkerConflict, err)
	}
	if err := contract.ValidateSettlement(plan.dispatch.Intent, outcomes); err != nil {
		return nil, workerFailure(ErrWorkerConflict, err)
	}
	latestObservedAt := outcomes[0].ObservedAt
	for _, outcome := range outcomes[1:] {
		if outcome.ObservedAt.After(latestObservedAt) {
			latestObservedAt = outcome.ObservedAt
		}
	}
	if !latestObservedAt.Equal(httpObservation.ObservedAt) {
		return nil, workerFailure(ErrWorkerConflict, errors.New("HTTP retry observation differs from exact member settlement"))
	}

	basis, err := workerHTTPRetryBasis(plan.request.Operation, *httpObservation, outcomes)
	if err != nil {
		return nil, err
	}
	notBefore := httpObservation.ObservedAt
	if httpObservation.Cooldown.Usable && httpObservation.Cooldown.NotBefore.After(notBefore) {
		notBefore = httpObservation.Cooldown.NotBefore
	}
	if notBefore.Location() != time.UTC || !notBefore.Before(plan.state.deadline) ||
		notBefore.Sub(httpObservation.ObservedAt) > plan.request.HTTPPolicy.MaxRetryDelay {
		return nil, nil
	}
	inputDigest, err := store.WorkerRetryInputDigest(plan.state.input)
	if err != nil {
		return nil, workerFailure(ErrWorkerConflict, err)
	}
	nextDispatch, err := nextWorkerRetryDispatch(plan.dispatch, plan.task.TaskID, settlementRef)
	if err != nil {
		return nil, err
	}
	retry := &store.WorkerRetryPlan{
		NextDispatch: nextDispatch,
		Basis:        basis,
		NotBefore:    notBefore,
		InputDigest:  inputDigest,
	}
	if _, err := store.WorkerRetrySettlementSummary(plan.dispatch.Intent, outcomes, *retry); err != nil {
		return nil, workerFailure(ErrWorkerConflict, err)
	}
	return retry, nil
}

func workerHTTPRetryBasis(
	operation contract.Operation,
	observation HTTPAttemptObservation,
	outcomes []contract.MemberOutcome,
) (store.WorkerRetryBasis, error) {
	switch observation.Disposition {
	case pmruntime.DispositionNotSent:
		if observation.Retry.Reason != "definitive_unsent" ||
			observation.Cooldown.Present || !workerOutcomesAll(outcomes, contract.OutcomeUnsent) {
			return "", workerFailure(ErrWorkerConflict, errors.New("definitive-unsent retry lacks exact no-send evidence"))
		}
		return store.WorkerRetryDefinitiveUnsent, nil
	case pmruntime.DispositionUnknown:
		if observation.Retry.Reason != "source_declared_replay_safe" ||
			observation.Cooldown.Present || !workerOutcomesAll(outcomes, contract.OutcomeUnknown) ||
			!workerOperationRetrySafe(operation) {
			return "", workerFailure(ErrWorkerConflict, errors.New("unknown HTTP retry lacks unconditional source-safe authority"))
		}
		return store.WorkerRetrySourceDeclaredSafe, nil
	case pmruntime.DispositionResponded:
		if observation.Retry.Reason != "source_declared_http_status" ||
			observation.StatusCode < 400 || observation.StatusCode > 599 ||
			!workerOutcomesAll(outcomes, contract.OutcomeRejected) ||
			!workerOperationRetrySafe(operation) ||
			!workerRetryAdmitsHTTPStatus(operation, uint16(observation.StatusCode)) {
			return "", workerFailure(ErrWorkerConflict, errors.New("HTTP status retry lacks unconditional source-safe authority"))
		}
		return store.WorkerRetrySourceDeclaredSafe, nil
	default:
		return "", workerFailure(ErrWorkerConflict, errors.New("HTTP retry disposition is unsupported"))
	}
}

func validatePreparedWorkerRetry(
	state workerRunState,
	task contract.Task,
	dispatch store.DispatchRecord,
	selection OperationSelection,
	handler ResolvedHandler,
) error {
	if dispatch.Retry == nil || dispatch.State != store.DispatchPrepared ||
		len(dispatch.Attempts) != 0 || dispatch.AttemptNumber <= 1 ||
		dispatch.Intent.RunID != task.RunID ||
		selection.Operation.OperationID != task.OperationID {
		return workerFailure(ErrWorkerConflict, errors.New("prepared HTTP retry identity is inconsistent"))
	}
	if handler.Kind != HandlerHTTP || state.budget.Policy.HTTP == nil {
		return workerFailure(ErrWorkerCapabilityUnavailable, errors.New("prepared retry lacks retained HTTP execution authority"))
	}
	if err := state.budget.Policy.HTTP.ValidateForOperation(selection.Operation); err != nil {
		return workerFailure(ErrWorkerConflict, err)
	}
	if dispatch.AttemptNumber > state.budget.Policy.HTTP.MaxAttempts ||
		!dispatch.Retry.NotBefore.Before(state.deadline) ||
		!dispatch.Retry.NotBefore.Before(state.grant.Grant.ExpiresAt) {
		return workerFailure(ErrWorkerAuthorization, errors.New("prepared HTTP retry exceeds retained time or attempt authority"))
	}
	switch dispatch.Retry.Basis {
	case store.WorkerRetryDefinitiveUnsent:
	case store.WorkerRetrySourceDeclaredSafe:
		if !workerOperationRetrySafe(selection.Operation) {
			return workerFailure(ErrWorkerConflict, errors.New("prepared source-safe retry no longer has unconditional source authority"))
		}
	default:
		return workerFailure(ErrWorkerConflict, errors.New("prepared HTTP retry basis is unsupported"))
	}
	inputDigest, err := store.WorkerRetryInputDigest(state.input)
	if err != nil {
		return workerFailure(ErrWorkerConflict, err)
	}
	if inputDigest != dispatch.Retry.InputDigest {
		return workerFailure(ErrWorkerConflict, errors.New("prepared HTTP retry input authority changed"))
	}
	return nil
}

func workerOperationRetrySafe(operation contract.Operation) bool {
	return operation.Execution != nil && operation.Execution.Retry != nil &&
		operation.Execution.Retry.Classification == contract.OperationRetrySafe
}

func workerRetryAdmitsHTTPStatus(operation contract.Operation, status uint16) bool {
	if operation.Execution == nil || operation.Execution.Retry == nil {
		return false
	}
	return slices.Contains(operation.Execution.Retry.HTTPStatuses, status)
}

func workerOutcomesAll(outcomes []contract.MemberOutcome, state contract.OutcomeState) bool {
	if len(outcomes) == 0 {
		return false
	}
	for _, outcome := range outcomes {
		if outcome.State != state {
			return false
		}
	}
	return true
}

func nextWorkerRetryDispatch(
	parent store.DispatchRecord,
	taskID string,
	settlementRef string,
) (contract.Dispatch, error) {
	if parent.AttemptNumber == 0 || parent.AttemptNumber == ^uint32(0) {
		return contract.Dispatch{}, workerFailure(ErrWorkerConflict, errors.New("HTTP retry attempt ordinal cannot advance"))
	}
	nextAttemptNumber := parent.AttemptNumber + 1
	dispatchValue, err := workerRecordID("dispatch-retry-v1-", struct {
		RunID               string              `json:"run_id"`
		TaskID              string              `json:"task_id"`
		ParentDispatchID    contract.DispatchID `json:"parent_dispatch_id"`
		SettlementReceiptID string              `json:"settlement_receipt_id"`
		AttemptNumber       uint32              `json:"attempt_number"`
	}{
		RunID:               parent.Intent.RunID,
		TaskID:              taskID,
		ParentDispatchID:    parent.Intent.DispatchID,
		SettlementReceiptID: settlementRef,
		AttemptNumber:       nextAttemptNumber,
	})
	if err != nil {
		return contract.Dispatch{}, err
	}
	dispatchID, err := contract.NewDispatchID(dispatchValue)
	if err != nil {
		return contract.Dispatch{}, workerFailure(ErrWorkerConflict, err)
	}
	attemptValue, err := workerRecordID("attempt-retry-v1-", struct {
		DispatchID    contract.DispatchID `json:"dispatch_id"`
		AttemptNumber uint32              `json:"attempt_number"`
	}{DispatchID: dispatchID, AttemptNumber: nextAttemptNumber})
	if err != nil {
		return contract.Dispatch{}, err
	}
	attemptID, err := contract.NewAttemptID(attemptValue)
	if err != nil {
		return contract.Dispatch{}, workerFailure(ErrWorkerConflict, err)
	}
	next := parent.Intent
	next.DispatchID = dispatchID
	next.AttemptID = attemptID
	next.OrderedMemberIDs = slices.Clone(parent.Intent.OrderedMemberIDs)
	if err := next.Validate(); err != nil {
		return contract.Dispatch{}, workerFailure(ErrWorkerConflict, fmt.Errorf("validate next HTTP retry dispatch: %w", err))
	}
	return next, nil
}

func workerUnitResultForStoredSettlement(
	ctx context.Context,
	tx *store.Transaction,
	task store.TaskRef,
	taskID string,
	settlementRef string,
	dispatch contract.Dispatch,
	outcomes []contract.MemberOutcome,
) (contract.WorkflowUnitResult, error) {
	summary, err := store.WorkerSettlementSummary(dispatch, outcomes)
	if err != nil {
		return contract.WorkflowUnitResult{}, workerFailure(ErrWorkerConflict, err)
	}
	deferred := false
	leaf, err := store.LoadWorkerDispatch(ctx, tx, task)
	if err != nil {
		return contract.WorkflowUnitResult{}, err
	}
	for leaf.Retry != nil {
		if leaf.Retry.ParentDispatchID == dispatch.DispatchID {
			if leaf.Retry.SettlementReceiptID != settlementRef {
				return contract.WorkflowUnitResult{}, workerFailure(
					ErrWorkerConflict,
					errors.New("retry lineage settlement receipt differs from the qualified parent"),
				)
			}
			summary, err = store.WorkerRetrySettlementSummary(dispatch, outcomes, store.WorkerRetryPlan{
				NextDispatch: leaf.Intent,
				Basis:        leaf.Retry.Basis,
				NotBefore:    leaf.Retry.NotBefore,
				InputDigest:  leaf.Retry.InputDigest,
			})
			if err != nil {
				return contract.WorkflowUnitResult{}, workerFailure(ErrWorkerConflict, err)
			}
			deferred = true
			break
		}
		parent, err := store.LoadDispatch(ctx, tx, task, leaf.Retry.ParentDispatchID)
		if err != nil {
			return contract.WorkflowUnitResult{}, err
		}
		if parent.AttemptNumber == ^uint32(0) ||
			leaf.AttemptNumber != parent.AttemptNumber+1 {
			return contract.WorkflowUnitResult{}, workerFailure(
				ErrWorkerConflict,
				errors.New("retry lineage attempt ordinals are not contiguous"),
			)
		}
		leaf = parent
	}
	result := contract.WorkflowUnitResult{TaskID: taskID, ResultRef: settlementRef}
	switch {
	case summary.Unknown != 0:
		result.State = contract.RunStateReconciling
		result.UnknownEffectCount = summary.Unknown
	case summary.Unsent != 0 && deferred:
		result.State = contract.RunStateRunning
	case summary.Unsent != 0:
		result.State = contract.RunStateBlocked
	case summary.Rejected != 0:
		result.State = contract.RunStateFailed
	default:
		result.State = contract.RunStateSucceeded
	}
	if err := result.Validate(); err != nil {
		return contract.WorkflowUnitResult{}, workerFailure(ErrWorkerConflict, err)
	}
	return result, nil
}
