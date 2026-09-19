package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type WorkerRetryBasis string

const (
	WorkerRetryDefinitiveUnsent   WorkerRetryBasis = "definitive_unsent"
	WorkerRetrySourceDeclaredSafe WorkerRetryBasis = "source_declared_safe"
)

type WorkerRetryPlan struct {
	NextDispatch contract.Dispatch
	Basis        WorkerRetryBasis
	NotBefore    time.Time
	InputDigest  contract.SemanticDigest
}

type WorkerRetry struct {
	ParentDispatchID    contract.DispatchID
	ParentAttempt       contract.Attempt
	SettlementReceiptID string
	Basis               WorkerRetryBasis
	NotBefore           time.Time
	InputDigest         contract.SemanticDigest
}

func (basis WorkerRetryBasis) validate() error {
	switch basis {
	case WorkerRetryDefinitiveUnsent, WorkerRetrySourceDeclaredSafe:
		return nil
	default:
		return errors.New("worker retry basis is unsupported")
	}
}

func (plan WorkerRetryPlan) validate() error {
	if err := plan.NextDispatch.Validate(); err != nil {
		return fmt.Errorf("validate next retry dispatch: %w", err)
	}
	if err := plan.Basis.validate(); err != nil {
		return err
	}
	if err := validateWorkerRetryInstant(plan.NotBefore); err != nil {
		return err
	}
	if err := plan.InputDigest.Validate(); err != nil {
		return fmt.Errorf("validate worker retry input digest: %w", err)
	}
	return nil
}

func (retry WorkerRetry) validate() error {
	if err := retry.ParentDispatchID.Validate(); err != nil {
		return err
	}
	if err := retry.ParentAttempt.Validate(); err != nil {
		return err
	}
	if retry.ParentAttempt.DispatchID != retry.ParentDispatchID {
		return errors.New("worker retry parent attempt does not match its dispatch")
	}
	if !validStoreID(retry.SettlementReceiptID) {
		return errors.New("worker retry settlement receipt is invalid")
	}
	if err := retry.Basis.validate(); err != nil {
		return err
	}
	if err := validateWorkerRetryInstant(retry.NotBefore); err != nil {
		return err
	}
	if err := retry.InputDigest.Validate(); err != nil {
		return fmt.Errorf("validate stored worker retry input digest: %w", err)
	}
	return nil
}

func validateWorkerRetryInstant(value time.Time) error {
	if value.IsZero() || value.Location() != time.UTC {
		return errors.New("worker retry eligibility must be an explicit canonical UTC instant")
	}
	return nil
}

func workerRetryTaskStateTerminal(state contract.RunState) bool {
	switch state {
	case contract.RunStateSucceeded,
		contract.RunStatePartial,
		contract.RunStateFailed,
		contract.RunStateCancelled:
		return true
	default:
		return false
	}
}

// WorkerRetrySettlementSummary keeps the logical retry disposition separate
// from the exact member outcomes retained for the parent physical attempt.
func WorkerRetrySettlementSummary(
	dispatch contract.Dispatch,
	outcomes []contract.MemberOutcome,
	retry WorkerRetryPlan,
) (contract.EffectSummary, error) {
	if err := contract.ValidateSettlement(dispatch, outcomes); err != nil {
		return contract.EffectSummary{}, err
	}
	if err := retry.validate(); err != nil {
		return contract.EffectSummary{}, err
	}
	if !sameRetryDispatchIntent(dispatch, retry.NextDispatch) {
		return contract.EffectSummary{}, errors.New("retry dispatch does not preserve the exact logical effect intent")
	}
	if retry.NextDispatch.DispatchID == dispatch.DispatchID ||
		retry.NextDispatch.AttemptID == dispatch.AttemptID {
		return contract.EffectSummary{}, errors.New("retry dispatch and attempt identities must be new")
	}

	switch retry.Basis {
	case WorkerRetryDefinitiveUnsent:
		for _, outcome := range outcomes {
			if outcome.State != contract.OutcomeUnsent {
				return contract.EffectSummary{}, errors.New("definitive-unsent retry requires exact unsent member outcomes")
			}
		}
		return contract.EffectSummary{Unsent: 1}, nil
	case WorkerRetrySourceDeclaredSafe:
		for _, outcome := range outcomes {
			switch outcome.State {
			case contract.OutcomeRejected, contract.OutcomeUnknown:
			default:
				return contract.EffectSummary{}, errors.New("source-safe retry cannot defer a successful, unchanged, or unsent result")
			}
		}
		return contract.EffectSummary{Unknown: 1}, nil
	default:
		return contract.EffectSummary{}, errors.New("worker retry basis is unsupported")
	}
}

func sameRetryDispatchIntent(parent, child contract.Dispatch) bool {
	return parent.RunID == child.RunID &&
		parent.EffectKey == child.EffectKey &&
		slices.Equal(parent.OrderedMemberIDs, child.OrderedMemberIDs) &&
		parent.PayloadHash == child.PayloadHash &&
		parent.RequestArtifactRef == child.RequestArtifactRef &&
		parent.OperationRevision == child.OperationRevision &&
		parent.AccountWitness == child.AccountWitness &&
		parent.GrantEpoch == child.GrantEpoch &&
		parent.CapacityReservation == child.CapacityReservation
}

// WorkerRetryInputDigest binds retry eligibility to the exact immutable input,
// controls and approval presence admitted for the run.
func WorkerRetryInputDigest(snapshot RunInputSnapshot) (contract.SemanticDigest, error) {
	if err := validateRunInputSnapshot(snapshot); err != nil {
		return "", fmt.Errorf("validate admitted worker retry input: %w", err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("encode admitted worker retry input: %w", err)
	}
	digest, err := contract.CanonicalDigest(encoded)
	if err != nil {
		return "", fmt.Errorf("digest admitted worker retry input: %w", err)
	}
	return digest, nil
}

func validateWorkerRetryAuthority(
	run contract.RunRecord,
	input RunInputSnapshot,
	task contract.Task,
	dispatch contract.Dispatch,
	grant RunExecutionGrant,
	observedAt time.Time,
	authorityThrough time.Time,
) error {
	if err := run.Validate(); err != nil {
		return err
	}
	if err := validateRunInputSnapshot(input); err != nil {
		return err
	}
	if err := task.Validate(); err != nil {
		return err
	}
	if err := validateRunGrantBinding(run.Spec, grant.Grant); err != nil {
		return err
	}
	if run.CancelRequested || workerRetryTaskStateTerminal(run.State) ||
		run.State == contract.RunStateBlocked ||
		run.State == contract.RunStateAwaitingApproval ||
		task.CancelRequested || workerRetryTaskStateTerminal(task.State) ||
		(task.State != contract.RunStateRunning &&
			task.State != contract.RunStateReconciling) {
		return errors.New("worker retry authority is cancelled, terminal, or not executing")
	}
	source := run.Spec.OperationOrPlanRef
	if source.Kind != contract.OperationReference ||
		source.ID != task.OperationID ||
		source.Revision != dispatch.OperationRevision {
		return errors.New("worker retry operation revision differs from its sealed source")
	}
	if input.RunID != run.Spec.RunID ||
		input.Scope.OrgID != run.Spec.OrgID ||
		input.Scope.WorkspaceID != run.Spec.WorkspaceID ||
		!slices.Contains(grant.Grant.OperationIDs, source.ID) ||
		!slices.Contains(grant.Grant.ConnectionIDs, input.ConnectionID) {
		return errors.New("worker retry input source is outside its retained grant")
	}
	if approvalRef, ok := input.ApprovalRef.TextValue(); ok &&
		!slices.Contains(grant.Grant.ApprovalRefs, approvalRef) {
		return errors.New("worker retry approval is outside its retained grant")
	}
	if grant.RevokedAt != nil ||
		grant.Grant.GrantEpoch != dispatch.GrantEpoch ||
		!observedAt.Before(grant.Grant.ExpiresAt) ||
		!authorityThrough.Before(grant.Grant.ExpiresAt) {
		return errors.New("worker retry grant authority is unavailable or expires before use")
	}
	return nil
}

func validateWorkerRetryPlan(
	ctx context.Context,
	tx *Transaction,
	task contract.Task,
	parent DispatchRecord,
	outcomes []contract.MemberOutcome,
	settlementReceiptID string,
	plan WorkerRetryPlan,
	committedAt time.Time,
) (WorkerRetry, error) {
	if _, err := WorkerRetrySettlementSummary(parent.Intent, outcomes, plan); err != nil {
		return WorkerRetry{}, err
	}
	if parent.AttemptNumber == 0 || parent.AttemptNumber == ^uint32(0) {
		return WorkerRetry{}, errors.New("worker retry parent attempt ordinal cannot advance")
	}
	if parent.Retry == nil && parent.AttemptNumber != 1 ||
		parent.Retry != nil && parent.AttemptNumber == 1 {
		return WorkerRetry{}, ErrImmutableConflict
	}
	if parent.State != DispatchPossiblySent || len(parent.Attempts) != 1 {
		return WorkerRetry{}, errors.New("worker retry parent lacks one immutable physical attempt")
	}
	parentAttempt := parent.Attempts[0]
	if parentAttempt.AttemptID != parent.Intent.AttemptID ||
		parentAttempt.DispatchID != parent.Intent.DispatchID {
		return WorkerRetry{}, ErrImmutableConflict
	}
	var hasChild bool
	if err := tx.queryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pm.worker_retries
			WHERE run_id = $1 AND task_id = $2 AND parent_dispatch_id = $3
		)
	`, task.RunID, task.TaskID, string(parent.Intent.DispatchID)).Scan(&hasChild); err != nil {
		return WorkerRetry{}, fmt.Errorf("check worker retry parent leaf: %w", err)
	}
	if hasChild {
		return WorkerRetry{}, errors.New("worker retry parent already has a sealed child")
	}
	for _, outcome := range outcomes {
		if outcome.ObservedAt.Before(parentAttempt.RecordedAt) {
			return WorkerRetry{}, errors.New("worker retry outcome predates its parent attempt")
		}
		if outcome.State == contract.OutcomeUnknown &&
			!slices.Contains(outcome.AssertionRefs, parentAttempt.ReceiptRef) {
			return WorkerRetry{}, errors.New("unknown retry outcome lacks its exact parent attempt receipt")
		}
		if plan.Basis == WorkerRetryDefinitiveUnsent &&
			!slices.Contains(outcome.AssertionRefs, parentAttempt.ReceiptRef) {
			return WorkerRetry{}, errors.New("definitive-unsent retry lacks its exact parent attempt receipt")
		}
	}
	if !validStoreID(settlementReceiptID) {
		return WorkerRetry{}, errors.New("worker retry settlement receipt is invalid")
	}

	input, err := LoadRunInputSnapshot(ctx, tx, parent.Intent.RunID)
	if err != nil {
		return WorkerRetry{}, err
	}
	inputDigest, err := WorkerRetryInputDigest(input)
	if err != nil {
		return WorkerRetry{}, err
	}
	if inputDigest != plan.InputDigest {
		return WorkerRetry{}, errors.New("worker retry input digest differs from admitted input authority")
	}
	budget, err := LoadRunBudgetSnapshot(ctx, tx, parent.Intent.RunID)
	if err != nil {
		return WorkerRetry{}, err
	}
	if budget.Policy.HTTP == nil {
		return WorkerRetry{}, errors.New("worker retry requires retained HTTP attempt authority")
	}
	httpPolicy := *budget.Policy.HTTP
	nextAttemptNumber := parent.AttemptNumber + 1
	if nextAttemptNumber > httpPolicy.MaxAttempts {
		return WorkerRetry{}, errors.New("worker retry exhausts its retained attempt bound")
	}

	latestObservedAt := outcomes[0].ObservedAt.UTC()
	for _, outcome := range outcomes[1:] {
		if outcome.ObservedAt.After(latestObservedAt) {
			latestObservedAt = outcome.ObservedAt.UTC()
		}
	}
	if plan.NotBefore.Before(latestObservedAt) ||
		plan.NotBefore.Sub(latestObservedAt) > httpPolicy.MaxRetryDelay {
		return WorkerRetry{}, errors.New("worker retry eligibility exceeds its retained cooldown bound")
	}
	acceptedAt, err := LoadWorkerAcceptedAt(ctx, tx, parent.Intent.RunID)
	if err != nil {
		return WorkerRetry{}, err
	}
	deadline := acceptedAt.Add(budget.Policy.RunLifetime)
	if !deadline.After(acceptedAt) || !committedAt.Before(deadline) || !plan.NotBefore.Before(deadline) {
		return WorkerRetry{}, errors.New("worker retry exceeds the original accepted deadline")
	}
	run, err := LoadRun(ctx, tx, parent.Intent.RunID)
	if err != nil {
		return WorkerRetry{}, err
	}
	grant, err := LoadRunExecutionGrant(ctx, tx, parent.Intent.RunID)
	if err != nil {
		return WorkerRetry{}, err
	}
	if err := validateWorkerRetryAuthority(
		run, input, task, parent.Intent, grant, committedAt, plan.NotBefore,
	); err != nil {
		return WorkerRetry{}, err
	}
	retry := WorkerRetry{
		ParentDispatchID:    parent.Intent.DispatchID,
		ParentAttempt:       parentAttempt,
		SettlementReceiptID: settlementReceiptID,
		Basis:               plan.Basis,
		NotBefore:           plan.NotBefore,
		InputDigest:         plan.InputDigest,
	}
	if err := retry.validate(); err != nil {
		return WorkerRetry{}, err
	}
	return retry, nil
}

func insertWorkerRetryDispatch(
	ctx context.Context,
	tx *Transaction,
	scope contract.Scope,
	task TaskRef,
	effectDigest string,
	attemptNumber uint32,
	plan WorkerRetryPlan,
	retry WorkerRetry,
	preparedAt time.Time,
) error {
	dispatchJSON, err := json.Marshal(plan.NextDispatch)
	if err != nil {
		return fmt.Errorf("encode retry dispatch intent: %w", err)
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.dispatches (
			org_id, workspace_id, run_id, task_id, dispatch_id, effect_digest,
			state, intent, prepared_at, attempt_number
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`,
		scope.OrgID,
		scope.WorkspaceID,
		task.RunID,
		task.TaskID,
		string(plan.NextDispatch.DispatchID),
		effectDigest,
		string(DispatchPrepared),
		json.RawMessage(dispatchJSON),
		preparedAt,
		int64(attemptNumber),
	); err != nil {
		return safeMetadataWriteError("insert retry dispatch", err)
	}
	for ordinal, memberID := range plan.NextDispatch.OrderedMemberIDs {
		if _, err := tx.exec(ctx, `
			INSERT INTO pm.dispatch_members (
				org_id, workspace_id, run_id, task_id, dispatch_id, ordinal, member_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7)
		`,
			scope.OrgID,
			scope.WorkspaceID,
			task.RunID,
			task.TaskID,
			string(plan.NextDispatch.DispatchID),
			ordinal,
			string(memberID),
		); err != nil {
			return safeMetadataWriteError("insert retry dispatch member", err)
		}
	}
	parentAttemptJSON, err := json.Marshal(retry.ParentAttempt)
	if err != nil {
		return fmt.Errorf("encode retry parent attempt: %w", err)
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.worker_retries (
			org_id, workspace_id, run_id, task_id, child_dispatch_id,
			parent_dispatch_id, parent_attempt_id, parent_attempt_receipt_id,
			parent_attempt, settlement_receipt_id, basis, not_before, input_digest
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`,
		scope.OrgID,
		scope.WorkspaceID,
		task.RunID,
		task.TaskID,
		string(plan.NextDispatch.DispatchID),
		string(retry.ParentDispatchID),
		string(retry.ParentAttempt.AttemptID),
		retry.ParentAttempt.ReceiptRef,
		json.RawMessage(parentAttemptJSON),
		retry.SettlementReceiptID,
		string(retry.Basis),
		retry.NotBefore,
		string(retry.InputDigest),
	); err != nil {
		return safeMetadataWriteError("insert immutable worker retry lineage", err)
	}
	return nil
}

func loadWorkerDispatchLineage(
	ctx context.Context,
	tx *Transaction,
	task TaskRef,
	initial bool,
) (DispatchRecord, error) {
	if _, err := tx.requireScope(); err != nil {
		return DispatchRecord{}, err
	}
	if err := task.validate(); err != nil {
		return DispatchRecord{}, err
	}
	var count, roots, leaves, effects, minimum, maximum int64
	var rootID, leafID *string
	var contiguous bool
	// Unique child/parent keys prevent forks; strict adjacent ordinals prevent
	// cycles. Validate the whole chain with one bounded result, not an arbitrary
	// in-memory history cap that can strand an already admitted retry.
	err := tx.queryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE retry.child_dispatch_id IS NULL),
		       count(*) FILTER (WHERE successor.child_dispatch_id IS NULL),
		       count(DISTINCT dispatch.effect_digest),
		       COALESCE(min(dispatch.attempt_number), 0),
		       COALESCE(max(dispatch.attempt_number), 0),
		       min(dispatch.dispatch_id) FILTER (WHERE retry.child_dispatch_id IS NULL),
		       min(dispatch.dispatch_id) FILTER (WHERE successor.child_dispatch_id IS NULL),
		       COALESCE(bool_and(
		           (retry.child_dispatch_id IS NULL AND dispatch.attempt_number = 1)
		           OR
		           (retry.child_dispatch_id IS NOT NULL AND parent.dispatch_id IS NOT NULL
		            AND dispatch.attempt_number = parent.attempt_number + 1
		            AND dispatch.effect_digest = parent.effect_digest)
		       ), false)
		FROM pm.dispatches AS dispatch
		LEFT JOIN pm.worker_retries AS retry
		  ON retry.org_id = dispatch.org_id
		 AND retry.workspace_id = dispatch.workspace_id
		 AND retry.run_id = dispatch.run_id
		 AND retry.task_id = dispatch.task_id
		 AND retry.child_dispatch_id = dispatch.dispatch_id
		LEFT JOIN pm.dispatches AS parent
		  ON parent.org_id = retry.org_id
		 AND parent.workspace_id = retry.workspace_id
		 AND parent.run_id = retry.run_id
		 AND parent.task_id = retry.task_id
		 AND parent.dispatch_id = retry.parent_dispatch_id
		LEFT JOIN pm.worker_retries AS successor
		  ON successor.org_id = dispatch.org_id
		 AND successor.workspace_id = dispatch.workspace_id
		 AND successor.run_id = dispatch.run_id
		 AND successor.task_id = dispatch.task_id
		 AND successor.parent_dispatch_id = dispatch.dispatch_id
		WHERE dispatch.run_id = $1 AND dispatch.task_id = $2
	`, task.RunID, task.TaskID).Scan(
		&count, &roots, &leaves, &effects, &minimum, &maximum,
		&rootID, &leafID, &contiguous,
	)
	if err != nil {
		return DispatchRecord{}, fmt.Errorf("locate worker dispatch lineage: %w", err)
	}
	if count == 0 {
		return DispatchRecord{}, ErrNotFound
	}
	if roots != 1 || leaves != 1 || effects != 1 || minimum != 1 ||
		maximum != count || maximum > int64(^uint32(0)) ||
		rootID == nil || leafID == nil || !contiguous {
		return DispatchRecord{}, ErrImmutableConflict
	}
	selected := *leafID
	if initial {
		selected = *rootID
	}
	dispatchID, err := contract.NewDispatchID(selected)
	if err != nil {
		return DispatchRecord{}, ErrImmutableConflict
	}
	return LoadDispatch(ctx, tx, task, dispatchID)
}

func loadWorkerRetry(
	ctx context.Context,
	tx *Transaction,
	task TaskRef,
	childDispatchID contract.DispatchID,
) (*WorkerRetry, error) {
	var parentDispatchID string
	var parentAttemptJSON []byte
	var settlementReceiptID, basis, inputDigest string
	var notBefore time.Time
	err := tx.queryRow(ctx, `
		SELECT parent_dispatch_id, parent_attempt, settlement_receipt_id,
		       basis, not_before, input_digest
		FROM pm.worker_retries
		WHERE run_id = $1 AND task_id = $2 AND child_dispatch_id = $3
	`, task.RunID, task.TaskID, string(childDispatchID)).Scan(
		&parentDispatchID,
		&parentAttemptJSON,
		&settlementReceiptID,
		&basis,
		&notBefore,
		&inputDigest,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load worker retry metadata: %w", err)
	}
	parentID, err := contract.NewDispatchID(parentDispatchID)
	if err != nil {
		return nil, ErrImmutableConflict
	}
	var parentAttempt contract.Attempt
	if err := json.Unmarshal(parentAttemptJSON, &parentAttempt); err != nil {
		return nil, fmt.Errorf("decode worker retry parent attempt: %w", err)
	}
	retry := &WorkerRetry{
		ParentDispatchID:    parentID,
		ParentAttempt:       parentAttempt,
		SettlementReceiptID: settlementReceiptID,
		Basis:               WorkerRetryBasis(basis),
		NotBefore:           notBefore.UTC(),
		InputDigest:         contract.SemanticDigest(inputDigest),
	}
	if err := retry.validate(); err != nil {
		return nil, fmt.Errorf("validate stored worker retry metadata: %w", err)
	}
	return retry, nil
}

func validateWorkerRetryAttempt(
	ctx context.Context,
	tx *Transaction,
	task contract.Task,
	child DispatchRecord,
	committedAt time.Time,
) error {
	taskRef := TaskRef{RunID: task.RunID, TaskID: task.TaskID}
	if child.Retry == nil {
		if child.AttemptNumber != 1 {
			return ErrImmutableConflict
		}
		return nil
	}
	if child.AttemptNumber <= 1 {
		return ErrImmutableConflict
	}
	retry := *child.Retry
	if committedAt.Before(retry.NotBefore) {
		return errors.New("worker retry is not yet eligible")
	}
	var hasChild bool
	err := tx.queryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pm.worker_retries
			WHERE run_id = $1 AND task_id = $2 AND parent_dispatch_id = $3
		)
	`, task.RunID, task.TaskID, string(child.Intent.DispatchID)).Scan(&hasChild)
	if err != nil {
		return fmt.Errorf("check worker retry leaf: %w", err)
	}
	if hasChild {
		return errors.New("worker retry dispatch is no longer the current lineage leaf")
	}
	parent, err := LoadDispatch(ctx, tx, taskRef, retry.ParentDispatchID)
	if err != nil {
		return err
	}
	if parent.AttemptNumber == ^uint32(0) ||
		child.AttemptNumber != parent.AttemptNumber+1 ||
		!sameRetryDispatchIntent(parent.Intent, child.Intent) ||
		parent.State != DispatchPossiblySent ||
		len(parent.Attempts) != 1 ||
		parent.Attempts[0] != retry.ParentAttempt {
		return ErrImmutableConflict
	}
	parentOutcomes, err := ListMemberOutcomes(ctx, tx, taskRef, parent.Intent.DispatchID)
	if err != nil {
		return err
	}
	if err := contract.ValidateSettlement(parent.Intent, parentOutcomes); err != nil {
		return fmt.Errorf("validate worker retry parent settlement: %w", err)
	}
	receiptExists, err := WorkerReceiptExists(
		ctx,
		tx,
		task.RunID,
		retry.SettlementReceiptID,
		"worker_settlement",
	)
	if err != nil {
		return err
	}
	if !receiptExists {
		return ErrImmutableConflict
	}
	input, err := LoadRunInputSnapshot(ctx, tx, task.RunID)
	if err != nil {
		return err
	}
	inputDigest, err := WorkerRetryInputDigest(input)
	if err != nil {
		return err
	}
	if inputDigest != retry.InputDigest {
		return errors.New("worker retry input authority has changed")
	}
	budget, err := LoadRunBudgetSnapshot(ctx, tx, task.RunID)
	if err != nil {
		return err
	}
	if budget.Policy.HTTP == nil || child.AttemptNumber > budget.Policy.HTTP.MaxAttempts {
		return errors.New("worker retry attempt is outside its retained HTTP budget")
	}
	acceptedAt, err := LoadWorkerAcceptedAt(ctx, tx, task.RunID)
	if err != nil {
		return err
	}
	deadline := acceptedAt.Add(budget.Policy.RunLifetime)
	if !deadline.After(acceptedAt) || !committedAt.Before(deadline) {
		return errors.New("worker retry exceeds the original accepted deadline")
	}
	run, err := LoadRun(ctx, tx, task.RunID)
	if err != nil {
		return err
	}
	grant, err := LoadRunExecutionGrant(ctx, tx, task.RunID)
	if err != nil {
		return err
	}
	if err := validateWorkerRetryAuthority(
		run, input, task, child.Intent, grant, committedAt, committedAt,
	); err != nil {
		return err
	}
	return nil
}

// CloseUnattemptedWorkerRetries resolves only source-safe retry leaves whose
// parent outcomes already prove the logical effect was rejected. Cancellation
// or the original accepted deadline authorizes closure; neither condition is
// treated as evidence about an outcome that remains unknown.
func CloseUnattemptedWorkerRetries(
	ctx context.Context,
	tx *Transaction,
	runID string,
) error {
	if ctx == nil {
		return errors.New("worker retry closure context is required")
	}
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	if !validStoreID(runID) {
		return errors.New("worker retry closure run identity is invalid")
	}
	run, err := LoadWorkerRunForUpdate(ctx, tx, runID)
	if err != nil {
		return err
	}
	if workerRetryTaskStateTerminal(run.State) {
		return ErrImmutableConflict
	}
	budget, err := LoadRunBudgetSnapshot(ctx, tx, runID)
	if err != nil {
		return err
	}
	acceptedAt, err := LoadWorkerAcceptedAt(ctx, tx, runID)
	if err != nil {
		return err
	}
	now, err := ReadAuthenticationTime(ctx, tx)
	if err != nil {
		return err
	}
	deadline := acceptedAt.Add(budget.Policy.RunLifetime)
	if !deadline.After(acceptedAt) {
		return errors.New("worker retry closure deadline is invalid")
	}
	authorized := run.CancelRequested || !now.Before(deadline)
	afterTaskID := ""
	for {
		tasks, err := listWorkerRetryClosureTasks(ctx, tx, scope, runID, afterTaskID)
		if err != nil {
			return err
		}
		for _, task := range tasks {
			if err := closeUnattemptedWorkerRetry(ctx, tx, scope, task, now, authorized); err != nil {
				return err
			}
		}
		if len(tasks) < int(contract.MaxWorkflowFanout) {
			return nil
		}
		afterTaskID = tasks[len(tasks)-1].TaskID
	}
}

func listWorkerRetryClosureTasks(
	ctx context.Context,
	tx *Transaction,
	scope contract.Scope,
	runID string,
	afterTaskID string,
) ([]contract.Task, error) {
	rows, err := tx.query(ctx, `
		SELECT task.task
		FROM pm.tasks AS task
		WHERE task.org_id = $1 AND task.workspace_id = $2 AND task.run_id = $3
		  AND ($4 = '' OR task.task_id > $4)
		  AND EXISTS (
		      SELECT 1
		      FROM pm.worker_retries AS retry
		      JOIN pm.dispatches AS child
		        ON child.org_id = retry.org_id
		       AND child.workspace_id = retry.workspace_id
		       AND child.run_id = retry.run_id
		       AND child.task_id = retry.task_id
		       AND child.dispatch_id = retry.child_dispatch_id
		      WHERE retry.org_id = task.org_id
		        AND retry.workspace_id = task.workspace_id
		        AND retry.run_id = task.run_id
		        AND retry.task_id = task.task_id
		        AND retry.basis = $5 AND child.state = 'prepared'
		        AND NOT EXISTS (
		            SELECT 1 FROM pm.dispatch_attempts AS attempt
		            WHERE attempt.org_id = child.org_id
		              AND attempt.workspace_id = child.workspace_id
		              AND attempt.run_id = child.run_id
		              AND attempt.task_id = child.task_id
		              AND attempt.dispatch_id = child.dispatch_id
		        )
		        AND NOT EXISTS (
		            SELECT 1 FROM pm.worker_retries AS successor
		            WHERE successor.org_id = child.org_id
		              AND successor.workspace_id = child.workspace_id
		              AND successor.run_id = child.run_id
		              AND successor.task_id = child.task_id
		              AND successor.parent_dispatch_id = child.dispatch_id
		        )
		  )
		ORDER BY task.task_id
		LIMIT $6
	`, scope.OrgID, scope.WorkspaceID, runID, afterTaskID,
		string(WorkerRetrySourceDeclaredSafe), contract.MaxWorkflowFanout)
	if err != nil {
		return nil, fmt.Errorf("list bounded worker retry closure tasks: %w", err)
	}
	defer rows.Close()
	tasks := make([]contract.Task, 0, contract.MaxWorkflowFanout)
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			return nil, fmt.Errorf("scan worker retry closure task: %w", err)
		}
		var task contract.Task
		if err := json.Unmarshal(encoded, &task); err != nil {
			return nil, fmt.Errorf("decode worker retry closure task: %w", err)
		}
		if err := task.Validate(); err != nil {
			return nil, fmt.Errorf("validate worker retry closure task: %w", err)
		}
		if task.OrgID != scope.OrgID || task.WorkspaceID != scope.WorkspaceID ||
			task.RunID != runID {
			return nil, ErrScopeMismatch
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate worker retry closure tasks: %w", err)
	}
	return tasks, nil
}

func closeUnattemptedWorkerRetry(
	ctx context.Context,
	tx *Transaction,
	scope contract.Scope,
	task contract.Task,
	now time.Time,
	authorized bool,
) error {
	taskRef := TaskRef{RunID: task.RunID, TaskID: task.TaskID}
	storedTask, err := loadTaskForUpdate(ctx, tx, taskRef)
	if err != nil {
		return err
	}
	if storedTask.OrgID != scope.OrgID ||
		storedTask.WorkspaceID != scope.WorkspaceID ||
		storedTask.RunID != task.RunID ||
		storedTask.TaskID != task.TaskID {
		return ErrScopeMismatch
	}
	leaf, err := LoadWorkerDispatch(ctx, tx, taskRef)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if leaf.Retry == nil || leaf.Retry.Basis != WorkerRetrySourceDeclaredSafe {
		return nil
	}
	if leaf.State != DispatchPrepared || len(leaf.Attempts) != 0 {
		return nil
	}
	if !authorized {
		return errors.New("worker retry closure requires durable cancellation or the original accepted deadline")
	}
	retry := *leaf.Retry
	parent, err := LoadDispatch(ctx, tx, taskRef, retry.ParentDispatchID)
	if err != nil {
		return err
	}
	if parent.State != DispatchPossiblySent || len(parent.Attempts) != 1 ||
		parent.Attempts[0] != retry.ParentAttempt ||
		leaf.AttemptNumber != parent.AttemptNumber+1 ||
		!sameRetryDispatchIntent(parent.Intent, leaf.Intent) {
		return ErrImmutableConflict
	}
	input, err := LoadRunInputSnapshot(ctx, tx, task.RunID)
	if err != nil {
		return err
	}
	inputDigest, err := WorkerRetryInputDigest(input)
	if err != nil {
		return err
	}
	if inputDigest != retry.InputDigest {
		return ErrImmutableConflict
	}
	outcomes, err := ListMemberOutcomes(ctx, tx, taskRef, parent.Intent.DispatchID)
	if err != nil {
		return err
	}
	if err := contract.ValidateSettlement(parent.Intent, outcomes); err != nil {
		return err
	}
	exists, err := WorkerReceiptExists(
		ctx, tx, task.RunID, retry.SettlementReceiptID, "worker_settlement",
	)
	if err != nil {
		return err
	}
	if !exists {
		return ErrImmutableConflict
	}
	if _, err := WorkerRetrySettlementSummary(parent.Intent, outcomes, WorkerRetryPlan{
		NextDispatch: leaf.Intent,
		Basis:        retry.Basis,
		NotBefore:    retry.NotBefore,
		InputDigest:  retry.InputDigest,
	}); err != nil {
		return err
	}
	for _, outcome := range outcomes {
		switch outcome.State {
		case contract.OutcomeRejected:
		case contract.OutcomeUnknown:
			return nil
		default:
			return ErrImmutableConflict
		}
	}
	effectDigest, effect, err := loadWorkerRetryEffectForUpdate(
		ctx, tx, scope, taskRef, parent.Intent,
	)
	if err != nil {
		return err
	}
	if len(effect.Attempts) == 0 ||
		effect.Attempts[len(effect.Attempts)-1] != retry.ParentAttempt {
		return ErrImmutableConflict
	}
	if effect.State == contract.OutcomeUnknown {
		updated, changed, err := settleEffectRecord(effect, outcomes)
		if err != nil {
			return err
		}
		if !changed || updated.State != contract.OutcomeRejected {
			return ErrImmutableConflict
		}
		encoded, err := json.Marshal(updated)
		if err != nil {
			return err
		}
		tag, err := tx.exec(ctx, `
			UPDATE pm.effects
			SET state = $4, revision = $5, effect_record = $6, updated_at = $7
			WHERE run_id = $1 AND task_id = $2
			  AND effect_digest = $3 AND revision = $8
		`,
			taskRef.RunID, taskRef.TaskID, effectDigest, updated.State,
			updated.Revision, json.RawMessage(encoded), now, effect.Revision,
		)
		if err != nil {
			return fmt.Errorf("close source-safe worker retry effect: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrCASConflict
		}
	} else if effect.State != contract.OutcomeRejected {
		return ErrImmutableConflict
	}
	return refreshWorkerRetryClosureTask(ctx, tx, taskRef, storedTask, now)
}

func loadWorkerRetryEffectForUpdate(
	ctx context.Context,
	tx *Transaction,
	scope contract.Scope,
	task TaskRef,
	dispatch contract.Dispatch,
) (string, contract.EffectRecord, error) {
	expectedDigest, err := digestEffectKey(dispatch.EffectKey)
	if err != nil {
		return "", contract.EffectRecord{}, err
	}
	var effectDigest string
	var encoded []byte
	if err := tx.queryRow(ctx, `
		SELECT dispatch.effect_digest, effect.effect_record
		FROM pm.dispatches AS dispatch
		JOIN pm.effects AS effect
		  ON effect.org_id = dispatch.org_id
		 AND effect.workspace_id = dispatch.workspace_id
		 AND effect.run_id = dispatch.run_id
		 AND effect.task_id = dispatch.task_id
		 AND effect.effect_digest = dispatch.effect_digest
		WHERE dispatch.org_id = $1
		  AND dispatch.workspace_id = $2
		  AND dispatch.run_id = $3
		  AND dispatch.task_id = $4
		  AND dispatch.dispatch_id = $5
		FOR UPDATE OF effect
	`, scope.OrgID, scope.WorkspaceID, task.RunID, task.TaskID, dispatch.DispatchID).Scan(
		&effectDigest, &encoded,
	); errors.Is(err, pgx.ErrNoRows) {
		return "", contract.EffectRecord{}, ErrNotFound
	} else if err != nil {
		return "", contract.EffectRecord{}, fmt.Errorf("lock source-safe worker retry effect: %w", err)
	}
	if effectDigest != expectedDigest {
		return "", contract.EffectRecord{}, ErrImmutableConflict
	}
	var effect contract.EffectRecord
	if err := json.Unmarshal(encoded, &effect); err != nil {
		return "", contract.EffectRecord{}, fmt.Errorf("decode source-safe worker retry effect: %w", err)
	}
	if err := effect.Validate(); err != nil {
		return "", contract.EffectRecord{}, fmt.Errorf("validate source-safe worker retry effect: %w", err)
	}
	if effect.OrgID != scope.OrgID || effect.WorkspaceID != scope.WorkspaceID ||
		effect.RunID != task.RunID || effect.TaskID != task.TaskID ||
		effect.EffectKey != dispatch.EffectKey {
		return "", contract.EffectRecord{}, ErrScopeMismatch
	}
	return effectDigest, effect, nil
}

func refreshWorkerRetryClosureTask(
	ctx context.Context,
	tx *Transaction,
	taskRef TaskRef,
	stored contract.Task,
	now time.Time,
) error {
	if stored.RunID != taskRef.RunID || stored.TaskID != taskRef.TaskID {
		return ErrScopeMismatch
	}
	summary, err := summarizeDurableTaskEffects(ctx, tx, taskRef)
	if err != nil {
		return err
	}
	if stored.EffectSummary == summary {
		return nil
	}
	if workerRetryTaskStateTerminal(stored.State) {
		return ErrImmutableConflict
	}
	observedAt := now
	if observedAt.Before(stored.ProjectionObservedAt) {
		observedAt = stored.ProjectionObservedAt
	}
	updated, err := stored.ApplyEffectSummary(summary, false, "", observedAt)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(updated)
	if err != nil {
		return err
	}
	tag, err := tx.exec(ctx, `
		UPDATE pm.tasks
		SET state = $3, revision = $4, task = $5, updated_at = $6
		WHERE run_id = $1 AND task_id = $2 AND revision = $7
	`,
		taskRef.RunID, taskRef.TaskID, updated.State, updated.Revision,
		json.RawMessage(encoded), now, stored.Revision,
	)
	if err != nil {
		return fmt.Errorf("refresh source-safe worker retry task summary: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrCASConflict
	}
	return nil
}
