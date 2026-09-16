package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type DispatchState string

const (
	DispatchPrepared     DispatchState = "prepared"
	DispatchPossiblySent DispatchState = "possibly_dispatched"
)

type DispatchRecord struct {
	Intent               contract.Dispatch
	State                DispatchState
	PreparedAt           time.Time
	PossiblyDispatchedAt *time.Time
	Attempts             []contract.Attempt
}

type DispatchAttemptCommit struct {
	Task    TaskRef
	Attempt contract.Attempt
	Receipt Receipt
	Audit   AuditEntry
	Event   EventInput
	Outbox  OutboxCommand
}

func PrepareDispatch(ctx context.Context, tx *Transaction, lease Lease, dispatch contract.Dispatch) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	if err := dispatch.Validate(); err != nil {
		return fmt.Errorf("validate dispatch intent: %w", err)
	}
	if dispatch.RunID != lease.Task.RunID {
		return ErrScopeMismatch
	}
	if err := requireCurrentLease(ctx, tx, lease); err != nil {
		return err
	}
	effect, err := contract.NewEffectRecord(scope.OrgID, scope.WorkspaceID, lease.Task.RunID, lease.Task.TaskID, dispatch.EffectKey)
	if err != nil {
		return fmt.Errorf("construct effect record: %w", err)
	}
	effectDigest, err := digestEffectKey(dispatch.EffectKey)
	if err != nil {
		return err
	}
	effectJSON, err := json.Marshal(effect)
	if err != nil {
		return fmt.Errorf("encode effect record: %w", err)
	}
	dispatchJSON, err := json.Marshal(dispatch)
	if err != nil {
		return fmt.Errorf("encode dispatch intent: %w", err)
	}
	preparedAt, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.effects (
			org_id, workspace_id, run_id, task_id, effect_digest, state, revision, effect_record, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`,
		scope.OrgID,
		scope.WorkspaceID,
		lease.Task.RunID,
		lease.Task.TaskID,
		effectDigest,
		string(effect.State),
		effect.Revision, json.RawMessage(effectJSON), preparedAt); err != nil {
		return fmt.Errorf("insert durable effect intent: %w", err)
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.dispatches (
			org_id, workspace_id, run_id, task_id, dispatch_id, effect_digest,
			state, intent, prepared_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`,
		scope.OrgID,
		scope.WorkspaceID,
		lease.Task.RunID,
		lease.Task.TaskID,
		string(dispatch.DispatchID),
		effectDigest,
		string(DispatchPrepared), json.RawMessage(dispatchJSON), preparedAt); err != nil {
		return fmt.Errorf("insert sealed dispatch intent: %w", err)
	}
	for ordinal, memberID := range dispatch.OrderedMemberIDs {
		if _, err := tx.exec(ctx, `
			INSERT INTO pm.dispatch_members (
				org_id, workspace_id, run_id, task_id, dispatch_id, ordinal, member_id
			) VALUES ($1, $2, $3, $4, $5, $6, $7)
		`,
			scope.OrgID,
			scope.WorkspaceID,
			lease.Task.RunID,
			lease.Task.TaskID,
			string(dispatch.DispatchID),
			ordinal,
			string(memberID),
		); err != nil {
			return fmt.Errorf("insert ordered dispatch member: %w", err)
		}
	}
	return nil
}

// RecordDispatchAttempt intentionally accepts a stale worker's exact sealed
// attempt. Fence loss can prohibit task mutation, but cannot make a possibly
// dispatched provider effect disappear from durable truth.
func RecordDispatchAttempt(ctx context.Context, tx *Transaction, commit DispatchAttemptCommit) (contract.ServerEvent, error) {
	if _, err := tx.requireScope(); err != nil {
		return contract.ServerEvent{}, err
	}
	if err := commit.Task.validate(); err != nil {
		return contract.ServerEvent{}, err
	}
	if err := commit.Attempt.Validate(); err != nil {
		return contract.ServerEvent{}, fmt.Errorf("validate dispatch attempt: %w", err)
	}
	if commit.Attempt.ReceiptRef != commit.Receipt.ReceiptID {
		return contract.ServerEvent{}, errors.New("dispatch attempt does not bind its durable receipt")
	}
	if err := validateCommitSideRecords(commit.Task.RunID, commit.Receipt, commit.Audit, commit.Event, commit.Outbox); err != nil {
		return contract.ServerEvent{}, err
	}
	storedTask, err := loadTaskForUpdate(ctx, tx, commit.Task)
	if err != nil {
		return contract.ServerEvent{}, err
	}
	var effectDigest string
	var effectJSON []byte
	err = tx.queryRow(ctx, `
		SELECT effect.effect_digest, effect.effect_record
		FROM pm.dispatches AS dispatch
		JOIN pm.effects AS effect
		  ON effect.org_id = dispatch.org_id
		 AND effect.workspace_id = dispatch.workspace_id
		 AND effect.run_id = dispatch.run_id
		 AND effect.task_id = dispatch.task_id
		 AND effect.effect_digest = dispatch.effect_digest
		WHERE dispatch.run_id = $1 AND dispatch.task_id = $2 AND dispatch.dispatch_id = $3
		FOR UPDATE OF dispatch, effect
	`, commit.Task.RunID, commit.Task.TaskID, string(commit.Attempt.DispatchID)).Scan(&effectDigest, &effectJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return contract.ServerEvent{}, ErrNotFound
	}
	if err != nil {
		return contract.ServerEvent{}, fmt.Errorf("lock dispatch attempt intent: %w", err)
	}
	var effect contract.EffectRecord
	if err := json.Unmarshal(effectJSON, &effect); err != nil {
		return contract.ServerEvent{}, fmt.Errorf("decode stored effect record: %w", err)
	}
	updatedEffect, err := effect.RecordAttempt(commit.Attempt)
	if err != nil {
		return contract.ServerEvent{}, fmt.Errorf("record effect attempt: %w", err)
	}
	updatedEffectJSON, err := json.Marshal(updatedEffect)
	if err != nil {
		return contract.ServerEvent{}, fmt.Errorf("encode attempted effect record: %w", err)
	}
	committedAt, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return contract.ServerEvent{}, err
	}
	attemptJSON, err := json.Marshal(commit.Attempt)
	if err != nil {
		return contract.ServerEvent{}, fmt.Errorf("encode dispatch attempt: %w", err)
	}
	if err := insertReceipt(ctx, tx, commit.Receipt, committedAt); err != nil {
		return contract.ServerEvent{}, err
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.dispatch_attempts (
			org_id, workspace_id, run_id, task_id, dispatch_id, attempt_id,
			receipt_id, attempt, recorded_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`,
		effect.OrgID,
		effect.WorkspaceID,
		commit.Task.RunID,
		commit.Task.TaskID,
		string(commit.Attempt.DispatchID),
		string(commit.Attempt.AttemptID),
		commit.Attempt.ReceiptRef, json.RawMessage(attemptJSON), commit.Attempt.RecordedAt); err != nil {
		return contract.ServerEvent{}, fmt.Errorf("insert dispatch attempt: %w", err)
	}
	effectTag, err := tx.exec(ctx, `
		UPDATE pm.effects
		SET state = $5, revision = $6, effect_record = $7, updated_at = $8
		WHERE run_id = $1 AND task_id = $2 AND effect_digest = $3 AND revision = $4
	`,
		commit.Task.RunID,
		commit.Task.TaskID,
		effectDigest,
		effect.Revision,
		string(updatedEffect.State),
		updatedEffect.Revision, json.RawMessage(updatedEffectJSON), committedAt)
	if err != nil {
		return contract.ServerEvent{}, fmt.Errorf("update durable effect attempt: %w", err)
	}
	if effectTag.RowsAffected() != 1 {
		return contract.ServerEvent{}, ErrCASConflict
	}
	tag, err := tx.exec(ctx, `
		UPDATE pm.dispatches
		SET state = $4, possibly_dispatched_at = $5
		WHERE run_id = $1 AND task_id = $2 AND dispatch_id = $3 AND state = $6
	`,
		commit.Task.RunID,
		commit.Task.TaskID,
		string(commit.Attempt.DispatchID),
		string(DispatchPossiblySent),
		committedAt,
		string(DispatchPrepared),
	)
	if err != nil {
		return contract.ServerEvent{}, fmt.Errorf("mark dispatch possibly sent: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return contract.ServerEvent{}, ErrCASConflict
	}
	if err := insertAuditEntry(ctx, tx, commit.Audit, committedAt); err != nil {
		return contract.ServerEvent{}, err
	}
	if err := insertOutboxCommand(ctx, tx, commit.Task.RunID, commit.Outbox, committedAt); err != nil {
		return contract.ServerEvent{}, err
	}
	event, err := insertEvent(ctx, tx, commit.Task.RunID, storedTask.Revision, commit.Event, committedAt)
	if err != nil {
		return contract.ServerEvent{}, err
	}
	return event, nil
}

func LoadDispatch(ctx context.Context, tx *Transaction, task TaskRef, dispatchID contract.DispatchID) (DispatchRecord, error) {
	if _, err := tx.requireScope(); err != nil {
		return DispatchRecord{}, err
	}
	if err := task.validate(); err != nil {
		return DispatchRecord{}, err
	}
	if err := dispatchID.Validate(); err != nil {
		return DispatchRecord{}, err
	}
	var record DispatchRecord
	var encoded []byte
	var state string
	if err := tx.queryRow(ctx, `
		SELECT intent, state, prepared_at, possibly_dispatched_at
		FROM pm.dispatches
		WHERE run_id = $1 AND task_id = $2 AND dispatch_id = $3
	`, task.RunID, task.TaskID, string(dispatchID)).Scan(&encoded, &state, &record.PreparedAt, &record.PossiblyDispatchedAt); errors.Is(err, pgx.ErrNoRows) {
		return DispatchRecord{}, ErrNotFound
	} else if err != nil {
		return DispatchRecord{}, fmt.Errorf("load dispatch: %w", err)
	}
	if err := json.Unmarshal(encoded, &record.Intent); err != nil {
		return DispatchRecord{}, fmt.Errorf("decode stored dispatch: %w", err)
	}
	if err := record.Intent.Validate(); err != nil {
		return DispatchRecord{}, fmt.Errorf("validate stored dispatch: %w", err)
	}
	if record.Intent.RunID != task.RunID || record.Intent.DispatchID != dispatchID {
		return DispatchRecord{}, ErrScopeMismatch
	}
	switch DispatchState(state) {
	case DispatchPrepared, DispatchPossiblySent:
	default:
		return DispatchRecord{}, errors.New("stored dispatch state is invalid")
	}
	record.State = DispatchState(state)
	record.PreparedAt = record.PreparedAt.UTC()
	if record.PossiblyDispatchedAt != nil {
		value := record.PossiblyDispatchedAt.UTC()
		record.PossiblyDispatchedAt = &value
	}
	record.Attempts = make([]contract.Attempt, 0)
	rows, err := tx.query(ctx, `
		SELECT attempt
		FROM pm.dispatch_attempts
		WHERE run_id = $1 AND task_id = $2 AND dispatch_id = $3
		ORDER BY recorded_at, attempt_id
	`, task.RunID, task.TaskID, string(dispatchID))
	if err != nil {
		return DispatchRecord{}, fmt.Errorf("list dispatch attempts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var attemptJSON []byte
		if err := rows.Scan(&attemptJSON); err != nil {
			return DispatchRecord{}, fmt.Errorf("scan dispatch attempt: %w", err)
		}
		var attempt contract.Attempt
		if err := json.Unmarshal(attemptJSON, &attempt); err != nil {
			return DispatchRecord{}, fmt.Errorf("decode stored dispatch attempt: %w", err)
		}
		if attempt.DispatchID != dispatchID {
			return DispatchRecord{}, ErrImmutableConflict
		}
		record.Attempts = append(record.Attempts, attempt)
	}
	if err := rows.Err(); err != nil {
		return DispatchRecord{}, fmt.Errorf("iterate dispatch attempts: %w", err)
	}
	return record, nil
}

func digestEffectKey(key contract.EffectKey) (string, error) {
	encoded, err := json.Marshal(key)
	if err != nil {
		return "", fmt.Errorf("encode effect identity: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}
