package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type SettlementCommit struct {
	Lease      Lease
	DispatchID contract.DispatchID
	Outcomes   []contract.MemberOutcome
	Task       contract.Task
	Receipt    Receipt
	Audit      AuditEntry
	Event      EventInput
	Outbox     OutboxCommand
}

func CommitSettlement(ctx context.Context, tx *Transaction, commit SettlementCommit) (contract.ServerEvent, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return contract.ServerEvent{}, err
	}
	if err := commit.DispatchID.Validate(); err != nil {
		return contract.ServerEvent{}, err
	}
	if err := commit.Task.Validate(); err != nil {
		return contract.ServerEvent{}, fmt.Errorf("validate settled task: %w", err)
	}
	if commit.Task.OrgID != scope.OrgID || commit.Task.WorkspaceID != scope.WorkspaceID || commit.Task.RunID != commit.Lease.Task.RunID || commit.Task.TaskID != commit.Lease.Task.TaskID {
		return contract.ServerEvent{}, ErrScopeMismatch
	}
	if err := validateCommitSideRecords(commit.Task.RunID, commit.Receipt, commit.Audit, commit.Event, commit.Outbox); err != nil {
		return contract.ServerEvent{}, err
	}
	if err := requireCurrentLease(ctx, tx, commit.Lease); err != nil {
		return contract.ServerEvent{}, err
	}
	storedTask, err := loadTaskForUpdate(ctx, tx, commit.Lease.Task)
	if err != nil {
		return contract.ServerEvent{}, err
	}
	if commit.Task.Revision != storedTask.Revision+1 || !sameTaskIdentity(storedTask, commit.Task) {
		return contract.ServerEvent{}, ErrCASConflict
	}
	if err := validateWorkerResultArtifactReference(ctx, tx, commit.Receipt); err != nil {
		return contract.ServerEvent{}, err
	}

	var dispatchJSON, effectJSON []byte
	var effectDigest string
	err = tx.queryRow(ctx, `
		SELECT dispatch.intent, effect.effect_digest, effect.effect_record
		FROM pm.dispatches AS dispatch
		JOIN pm.effects AS effect
		  ON effect.org_id = dispatch.org_id
		 AND effect.workspace_id = dispatch.workspace_id
		 AND effect.run_id = dispatch.run_id
		 AND effect.task_id = dispatch.task_id
		 AND effect.effect_digest = dispatch.effect_digest
		WHERE dispatch.run_id = $1 AND dispatch.task_id = $2 AND dispatch.dispatch_id = $3
		FOR UPDATE OF dispatch, effect
	`, commit.Task.RunID, commit.Task.TaskID, string(commit.DispatchID)).Scan(&dispatchJSON, &effectDigest, &effectJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return contract.ServerEvent{}, ErrNotFound
	}
	if err != nil {
		return contract.ServerEvent{}, fmt.Errorf("lock settlement intent: %w", err)
	}
	var dispatch contract.Dispatch
	if err := json.Unmarshal(dispatchJSON, &dispatch); err != nil {
		return contract.ServerEvent{}, fmt.Errorf("decode settlement dispatch: %w", err)
	}
	if err := contract.ValidateSettlement(dispatch, commit.Outcomes); err != nil {
		return contract.ServerEvent{}, fmt.Errorf("validate exact member settlement: %w", err)
	}
	committedAt, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return contract.ServerEvent{}, err
	}
	for _, outcome := range commit.Outcomes {
		encoded, err := json.Marshal(outcome)
		if err != nil {
			return contract.ServerEvent{}, fmt.Errorf("encode member outcome: %w", err)
		}
		if _, err := tx.exec(ctx, `
			INSERT INTO pm.member_outcomes (
				org_id, workspace_id, run_id, task_id, dispatch_id, member_id,
				state, outcome, observed_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		`,
			scope.OrgID,
			scope.WorkspaceID,
			commit.Task.RunID,
			commit.Task.TaskID,
			string(commit.DispatchID),
			string(outcome.MemberID),
			string(outcome.State), json.RawMessage(encoded), outcome.ObservedAt); err != nil {
			return contract.ServerEvent{}, fmt.Errorf("insert exact member outcome: %w", err)
		}
	}
	var effect contract.EffectRecord
	if err := json.Unmarshal(effectJSON, &effect); err != nil {
		return contract.ServerEvent{}, fmt.Errorf("decode settlement effect: %w", err)
	}
	updatedEffect, changed, err := settleEffectRecord(effect, commit.Outcomes)
	if err != nil {
		return contract.ServerEvent{}, err
	}
	if changed {
		encoded, err := json.Marshal(updatedEffect)
		if err != nil {
			return contract.ServerEvent{}, fmt.Errorf("encode settled effect: %w", err)
		}
		tag, err := tx.exec(ctx, `
			UPDATE pm.effects
			SET state = $5, revision = $6, effect_record = $7, updated_at = $8
			WHERE run_id = $1 AND task_id = $2 AND effect_digest = $3 AND revision = $4
		`,
			commit.Task.RunID,
			commit.Task.TaskID,
			effectDigest,
			effect.Revision,
			string(updatedEffect.State),
			updatedEffect.Revision, json.RawMessage(encoded), committedAt)
		if err != nil {
			return contract.ServerEvent{}, fmt.Errorf("update settled effect: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return contract.ServerEvent{}, ErrCASConflict
		}
	}
	summary, err := summarizeDurableTaskEffects(ctx, tx, commit.Lease.Task)
	if err != nil {
		return contract.ServerEvent{}, err
	}
	if commit.Task.EffectSummary != summary {
		return contract.ServerEvent{}, fmt.Errorf("%w: task effect summary does not match durable effects", ErrCASConflict)
	}
	taskJSON, err := json.Marshal(commit.Task)
	if err != nil {
		return contract.ServerEvent{}, fmt.Errorf("encode settled task: %w", err)
	}
	tag, err := tx.exec(ctx, `
		UPDATE pm.tasks
		SET state = $3, revision = $4, task = $5, updated_at = $6
		WHERE run_id = $1 AND task_id = $2 AND revision = $7
	`,
		commit.Task.RunID,
		commit.Task.TaskID,
		string(commit.Task.State),
		commit.Task.Revision, json.RawMessage(taskJSON), committedAt,
		storedTask.Revision)
	if err != nil {
		return contract.ServerEvent{}, fmt.Errorf("update settled task: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return contract.ServerEvent{}, ErrCASConflict
	}
	if err := insertReceipt(ctx, tx, commit.Receipt, committedAt); err != nil {
		return contract.ServerEvent{}, err
	}
	if err := insertAuditEntry(ctx, tx, commit.Audit, committedAt); err != nil {
		return contract.ServerEvent{}, err
	}
	if err := insertOutboxCommand(ctx, tx, commit.Task.RunID, commit.Outbox, committedAt); err != nil {
		return contract.ServerEvent{}, err
	}
	event, err := insertEvent(ctx, tx, commit.Task.RunID, commit.Task.Revision, commit.Event, committedAt)
	if err != nil {
		return contract.ServerEvent{}, err
	}
	return event, nil
}

func ListMemberOutcomes(ctx context.Context, tx *Transaction, task TaskRef, dispatchID contract.DispatchID) ([]contract.MemberOutcome, error) {
	if _, err := tx.requireScope(); err != nil {
		return nil, err
	}
	if err := task.validate(); err != nil {
		return nil, err
	}
	if err := dispatchID.Validate(); err != nil {
		return nil, err
	}
	rows, err := tx.query(ctx, `
		SELECT outcome.outcome
		FROM pm.member_outcomes AS outcome
		JOIN pm.dispatch_members AS member
		  ON member.org_id = outcome.org_id
		 AND member.workspace_id = outcome.workspace_id
		 AND member.run_id = outcome.run_id
		 AND member.task_id = outcome.task_id
		 AND member.dispatch_id = outcome.dispatch_id
		 AND member.member_id = outcome.member_id
		WHERE outcome.run_id = $1 AND outcome.task_id = $2 AND outcome.dispatch_id = $3
		ORDER BY member.ordinal
	`, task.RunID, task.TaskID, string(dispatchID))
	if err != nil {
		return nil, fmt.Errorf("list member outcomes: %w", err)
	}
	defer rows.Close()
	outcomes := make([]contract.MemberOutcome, 0)
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			return nil, fmt.Errorf("scan member outcome: %w", err)
		}
		var outcome contract.MemberOutcome
		if err := json.Unmarshal(encoded, &outcome); err != nil {
			return nil, fmt.Errorf("decode stored member outcome: %w", err)
		}
		if err := outcome.Validate(); err != nil {
			return nil, fmt.Errorf("validate stored member outcome: %w", err)
		}
		outcomes = append(outcomes, outcome)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate member outcomes: %w", err)
	}
	return outcomes, nil
}

func summarizeDurableTaskEffects(ctx context.Context, tx *Transaction, task TaskRef) (contract.EffectSummary, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return contract.EffectSummary{}, err
	}
	if err := task.validate(); err != nil {
		return contract.EffectSummary{}, err
	}
	var unsent, succeeded, unchanged, rejected, unknown int64
	err = tx.queryRow(ctx, `
		WITH locked_effects AS MATERIALIZED (
			SELECT state
			FROM pm.effects
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND task_id = $4
			FOR UPDATE
		)
		SELECT
			count(*) FILTER (WHERE state = 'unsent'),
			count(*) FILTER (WHERE state = 'succeeded'),
			count(*) FILTER (WHERE state = 'unchanged'),
			count(*) FILTER (WHERE state = 'rejected'),
			count(*) FILTER (WHERE state = 'unknown')
		FROM locked_effects
	`, scope.OrgID, scope.WorkspaceID, task.RunID, task.TaskID).Scan(
		&unsent, &succeeded, &unchanged, &rejected, &unknown,
	)
	if err != nil {
		return contract.EffectSummary{}, fmt.Errorf("summarize durable task effects: %w", err)
	}
	return contract.EffectSummary{
		Unsent:    uint64(unsent),
		Succeeded: uint64(succeeded),
		Unchanged: uint64(unchanged),
		Rejected:  uint64(rejected),
		Unknown:   uint64(unknown),
	}, nil
}

func settleEffectRecord(effect contract.EffectRecord, outcomes []contract.MemberOutcome) (contract.EffectRecord, bool, error) {
	if len(outcomes) == 0 {
		return effect, false, errors.New("member settlement is empty")
	}
	state := aggregateOutcomeState(outcomes)
	if state == effect.State || state == contract.OutcomeUnsent {
		return effect, false, nil
	}
	observedAt := outcomes[0].ObservedAt.UTC()
	for _, outcome := range outcomes[1:] {
		if outcome.ObservedAt.After(observedAt) {
			observedAt = outcome.ObservedAt.UTC()
		}
	}
	updated, err := effect.Transition(state, "member_settlement", observedAt)
	if err != nil {
		return effect, false, fmt.Errorf("transition settled effect: %w", err)
	}
	return updated, true, nil
}

func aggregateOutcomeState(outcomes []contract.MemberOutcome) contract.OutcomeState {
	allUnchanged := true
	allUnsent := true
	anySucceeded := false
	anyRejected := false
	for _, outcome := range outcomes {
		switch outcome.State {
		case contract.OutcomeUnknown:
			return contract.OutcomeUnknown
		case contract.OutcomeSucceeded:
			anySucceeded = true
			allUnchanged = false
			allUnsent = false
		case contract.OutcomeRejected:
			anyRejected = true
			allUnchanged = false
			allUnsent = false
		case contract.OutcomeUnchanged:
			allUnsent = false
		case contract.OutcomeUnsent:
			allUnchanged = false
		}
	}
	if anyRejected {
		return contract.OutcomeRejected
	}
	if anySucceeded {
		return contract.OutcomeSucceeded
	}
	if allUnchanged {
		return contract.OutcomeUnchanged
	}
	if allUnsent {
		return contract.OutcomeUnsent
	}
	return contract.OutcomeUnknown
}
