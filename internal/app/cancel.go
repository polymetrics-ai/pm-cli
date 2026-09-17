package app

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

const statusAccepted = 202

type RunCancelResult struct {
	CommandID string `json:"command_id"`
	RunID     string `json:"run_id"`
	Revision  uint64 `json:"revision"`
	Replayed  bool   `json:"replayed"`
}

func (service *RunService) RequestCancel(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	runID string,
	control MutationControl,
) (RunCancelResult, error) {
	if service == nil || ctx == nil || !validApplicationID(runID) {
		return RunCancelResult{}, invalidRequestFailure()
	}
	var result RunCancelResult
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		if err := store.BindAuthDeployment(ctx, tx, service.deploymentID); err != nil {
			return err
		}
		view, err := store.LoadRunView(ctx, tx, runID)
		if err != nil {
			return mapRunStoreFailure(err)
		}
		if _, err := service.authorizeRunRecord(
			ctx,
			tx,
			actor,
			scope,
			view,
			"run.control",
			view.Record.Spec.OperationOrPlanRef.ID,
		); err != nil {
			return err
		}
		intentRevision, err := validateMutationControl(control)
		if err != nil {
			return err
		}
		digest, err := canonicalIntent(struct {
			Operation      string         `json:"operation"`
			Scope          contract.Scope `json:"scope"`
			RunID          string         `json:"run_id"`
			IntentRevision uint64         `json:"expected_revision"`
		}{
			Operation:      "run.cancel",
			Scope:          scope,
			RunID:          runID,
			IntentRevision: intentRevision,
		})
		if err != nil {
			return invalidRequestFailure()
		}
		key := mutationReceiptKey(scope, actor, "run.cancel", runID, control.IdempotencyKey)
		begin, err := store.BeginIdentityMutation(ctx, tx, key, digest, intentRevision)
		if err != nil {
			return mapRunControlStoreFailure(err)
		}
		if !begin.Fresh {
			if err := decodeReplay(begin.Receipt, &result); err != nil {
				return err
			}
			if result.RunID != runID || !validApplicationID(result.CommandID) || result.Revision == 0 {
				return store.ErrImmutableConflict
			}
			result.Replayed = true
			return nil
		}
		if err := requireFreshMutationPrecondition(control); err != nil {
			return err
		}
		if view.Record.Revision != intentRevision {
			return mapMutationFailure(store.ErrCASConflict)
		}
		updated, err := view.Record.RequestCancel()
		if err != nil || updated.Revision == view.Record.Revision {
			return mapMutationFailure(store.ErrImmutableConflict)
		}
		commandID, err := randomApplicationID(service.random, "cancel-command-", 18)
		if err != nil {
			return err
		}
		receiptID, err := randomApplicationID(service.random, "cancel-receipt-", 18)
		if err != nil {
			return err
		}
		auditID, err := randomApplicationID(service.random, "cancel-audit-", 18)
		if err != nil {
			return err
		}
		eventID, err := randomApplicationID(service.random, "cancel-event-", 18)
		if err != nil {
			return err
		}
		payload := contract.SafePayload{
			References: []contract.SafeReference{
				{Name: "run", Ref: runID},
				{Name: "command", Ref: commandID},
			},
			Counts:      []contract.SafeCount{{Name: "run_revision", Value: updated.Revision}},
			ReasonCodes: []string{"cancel_requested"},
		}
		if _, err := store.CommitRunTransition(ctx, tx, store.RunTransitionCommit{
			Run: updated,
			Receipt: store.Receipt{
				ReceiptID: receiptID,
				Kind:      "run_cancel",
				RunID:     runID,
				Payload:   payload,
			},
			Audit: store.AuditEntry{
				AuditID:   auditID,
				Action:    "run_cancel_requested",
				TargetRef: runID,
				Payload:   payload,
			},
			Event: store.EventInput{
				EventID: eventID,
				Type:    "run_cancel_requested",
				Payload: payload,
			},
			Outbox: store.OutboxCommand{
				CommandID:  commandID,
				Kind:       "cancel_run",
				PayloadRef: runID,
			},
		}); err != nil {
			return mapRunControlStoreFailure(err)
		}
		result = RunCancelResult{CommandID: commandID, RunID: runID, Revision: updated.Revision}
		encoded, err := json.Marshal(result)
		if err != nil {
			return errors.New("encode safe run cancellation result")
		}
		if _, err := store.FinishIdentityMutation(
			ctx,
			tx,
			key,
			runID,
			updated.Revision,
			statusAccepted,
			encoded,
			service.receiptRetention,
		); err != nil {
			return mapRunControlStoreFailure(err)
		}
		return nil
	})
	if err == nil {
		return result, nil
	}
	if errors.Is(err, store.ErrCommitOutcomeUnknown) {
		return RunCancelResult{}, &Failure{
			HTTPStatus:  503,
			Code:        "mutation_outcome_unknown",
			RetryAction: "same_key_reconcile_only",
			cause:       err,
		}
	}
	var failure *Failure
	if errors.As(err, &failure) {
		return RunCancelResult{}, err
	}
	return RunCancelResult{}, mapRunControlStoreFailure(err)
}

func (service *RunService) RequestResume(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	runID string,
	generation string,
	approvalRef *string,
	control MutationControl,
) error {
	if service == nil || ctx == nil || !validApplicationID(runID) || !validApplicationID(generation) ||
		approvalRef != nil && !validApplicationID(*approvalRef) {
		return invalidRequestFailure()
	}
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		if err := store.BindAuthDeployment(ctx, tx, service.deploymentID); err != nil {
			return err
		}
		view, err := store.LoadRunView(ctx, tx, runID)
		if err != nil {
			return mapRunStoreFailure(err)
		}
		if _, err := service.authorizeRunRecord(
			ctx,
			tx,
			actor,
			scope,
			view,
			"run.control",
			view.Record.Spec.OperationOrPlanRef.ID,
		); err != nil {
			return err
		}
		intentRevision, err := validateMutationControl(control)
		if err != nil {
			return err
		}
		if err := requireFreshMutationPrecondition(control); err != nil {
			return err
		}
		if view.Record.Revision != intentRevision {
			return mapMutationFailure(store.ErrCASConflict)
		}
		return &Failure{
			HTTPStatus:  StatusConflict,
			Code:        "resume_unavailable",
			RetryAction: "refresh_run_state",
		}
	})
	if err == nil {
		return nil
	}
	var failure *Failure
	if errors.As(err, &failure) {
		return err
	}
	return mapRunControlStoreFailure(err)
}

func mapRunControlStoreFailure(err error) error {
	var expired *store.MutationReceiptExpiredError
	switch {
	case errors.As(err, &expired),
		errors.Is(err, store.ErrCASConflict),
		errors.Is(err, store.ErrImmutableConflict),
		errors.Is(err, store.ErrScopeMismatch),
		errors.Is(err, store.ErrNotFound),
		errors.Is(err, store.ErrIdempotencyConflict),
		errors.Is(err, store.ErrMutationUnresolved):
		return mapMutationFailure(err)
	default:
		return &Failure{
			HTTPStatus:  503,
			Code:        "control_store_unavailable",
			RetryAction: "same_key_after_dependency_recovery",
			cause:       err,
		}
	}
}
