package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type EventInput struct {
	EventID string
	Type    string
	Payload contract.SafePayload
}

func (event EventInput) validate() error {
	if !validStoreID(event.EventID) || !validStoreName(event.Type) {
		return errors.New("event identity or type is invalid")
	}
	if err := event.Payload.Validate(); err != nil {
		return fmt.Errorf("validate event payload: %w", err)
	}
	return nil
}

type TaskTransitionCommit struct {
	Lease   Lease
	Task    contract.Task
	Receipt Receipt
	Audit   AuditEntry
	Event   EventInput
	Outbox  OutboxCommand
}

type RunTransitionCommit struct {
	Run     contract.RunRecord
	Receipt Receipt
	Audit   AuditEntry
	Event   EventInput
	Outbox  OutboxCommand
}

func CommitTaskTransition(ctx context.Context, tx *Transaction, commit TaskTransitionCommit) (contract.ServerEvent, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return contract.ServerEvent{}, err
	}
	if err := commit.Task.Validate(); err != nil {
		return contract.ServerEvent{}, fmt.Errorf("validate task transition: %w", err)
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
	stored, err := loadTaskForUpdate(ctx, tx, commit.Lease.Task)
	if err != nil {
		return contract.ServerEvent{}, err
	}
	if commit.Task.Revision != stored.Revision+1 || !sameTaskIdentity(stored, commit.Task) {
		return contract.ServerEvent{}, ErrCASConflict
	}
	committedAt, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return contract.ServerEvent{}, err
	}
	encoded, err := json.Marshal(commit.Task)
	if err != nil {
		return contract.ServerEvent{}, fmt.Errorf("encode task transition: %w", err)
	}
	tag, err := tx.exec(ctx, `
		UPDATE pm.tasks
		SET state = $3, revision = $4, task = $5, updated_at = $6
		WHERE run_id = $1 AND task_id = $2 AND revision = $7
	`,
		commit.Task.RunID,
		commit.Task.TaskID,
		string(commit.Task.State),
		commit.Task.Revision, json.RawMessage(encoded), committedAt,
		stored.Revision)
	if err != nil {
		return contract.ServerEvent{}, fmt.Errorf("update task transition: %w", err)
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

func CommitRunTransition(ctx context.Context, tx *Transaction, commit RunTransitionCommit) (contract.ServerEvent, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return contract.ServerEvent{}, err
	}
	if err := commit.Run.Validate(); err != nil {
		return contract.ServerEvent{}, fmt.Errorf("validate run transition: %w", err)
	}
	if commit.Run.Spec.OrgID != scope.OrgID || commit.Run.Spec.WorkspaceID != scope.WorkspaceID {
		return contract.ServerEvent{}, ErrScopeMismatch
	}
	if err := validateCommitSideRecords(commit.Run.Spec.RunID, commit.Receipt, commit.Audit, commit.Event, commit.Outbox); err != nil {
		return contract.ServerEvent{}, err
	}
	stored, err := loadRunForUpdate(ctx, tx, commit.Run.Spec.RunID)
	if err != nil {
		return contract.ServerEvent{}, err
	}
	if commit.Run.Revision != stored.Revision+1 || !reflect.DeepEqual(commit.Run.Spec, stored.Spec) {
		return contract.ServerEvent{}, ErrCASConflict
	}
	committedAt, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return contract.ServerEvent{}, err
	}
	recordJSON, err := json.Marshal(commit.Run)
	if err != nil {
		return contract.ServerEvent{}, fmt.Errorf("encode run transition: %w", err)
	}
	tag, err := tx.exec(ctx, `
		UPDATE pm.runs
		SET state = $2, revision = $3, record = $4, updated_at = $5
		WHERE run_id = $1 AND revision = $6
	`,
		commit.Run.Spec.RunID,
		string(commit.Run.State),
		commit.Run.Revision, json.RawMessage(recordJSON), committedAt,
		stored.Revision)
	if err != nil {
		return contract.ServerEvent{}, fmt.Errorf("update run transition: %w", err)
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
	if err := insertOutboxCommand(ctx, tx, commit.Run.Spec.RunID, commit.Outbox, committedAt); err != nil {
		return contract.ServerEvent{}, err
	}
	event, err := insertEvent(ctx, tx, commit.Run.Spec.RunID, commit.Run.Revision, commit.Event, committedAt)
	if err != nil {
		return contract.ServerEvent{}, err
	}
	return event, nil
}

func ListEvents(ctx context.Context, tx *Transaction, runID string, after uint64, limit int) ([]contract.ServerEvent, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return nil, err
	}
	if !validStoreID(runID) || limit <= 0 || limit > 1000 {
		return nil, errors.New("event list arguments are invalid")
	}
	rows, err := tx.query(ctx, `
		SELECT sequence, state_revision, event_type, safe_payload, committed_at
		FROM pm.server_events
		WHERE run_id = $1 AND sequence > $2
		ORDER BY sequence
		LIMIT $3
	`, runID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list server events: %w", err)
	}
	defer rows.Close()
	events := make([]contract.ServerEvent, 0)
	for rows.Next() {
		var event contract.ServerEvent
		var payload []byte
		event.OrgID = scope.OrgID
		event.WorkspaceID = scope.WorkspaceID
		event.RunID = runID
		if err := rows.Scan(&event.Sequence, &event.StateRevision, &event.Type, &payload, &event.CommittedAt); err != nil {
			return nil, fmt.Errorf("scan server event: %w", err)
		}
		if err := json.Unmarshal(payload, &event.SafePayload); err != nil {
			return nil, fmt.Errorf("decode stored event payload: %w", err)
		}
		event.CommittedAt = event.CommittedAt.UTC()
		if err := event.Validate(); err != nil {
			return nil, fmt.Errorf("validate stored event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate server events: %w", err)
	}
	return events, nil
}

func insertEvent(ctx context.Context, tx *Transaction, runID string, stateRevision uint64, input EventInput, committedAt time.Time) (contract.ServerEvent, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return contract.ServerEvent{}, err
	}
	if !validStoreID(runID) || stateRevision == 0 {
		return contract.ServerEvent{}, errors.New("event run identity or state revision is invalid")
	}
	if err := input.validate(); err != nil {
		return contract.ServerEvent{}, err
	}
	var lockedRun string
	if err := tx.queryRow(ctx, `SELECT run_id FROM pm.runs WHERE run_id = $1 FOR UPDATE`, runID).Scan(&lockedRun); errors.Is(err, pgx.ErrNoRows) {
		return contract.ServerEvent{}, ErrNotFound
	} else if err != nil {
		return contract.ServerEvent{}, fmt.Errorf("lock event sequence: %w", err)
	}
	payload, err := json.Marshal(input.Payload)
	if err != nil {
		return contract.ServerEvent{}, fmt.Errorf("encode event payload: %w", err)
	}
	var sequence uint64
	if err := tx.queryRow(ctx, `
		INSERT INTO pm.server_events (
			org_id, workspace_id, run_id, sequence, event_id, state_revision, event_type, safe_payload, committed_at
		)
		SELECT $1, $2, $3, COALESCE(MAX(sequence), 0) + 1, $4, $5, $6, $7, $8
		FROM pm.server_events
		WHERE run_id = $3
		RETURNING sequence
	`,
		scope.OrgID,
		scope.WorkspaceID,
		runID,
		input.EventID,
		stateRevision,
		input.Type, json.RawMessage(payload), committedAt).Scan(&sequence); err != nil {
		return contract.ServerEvent{}, fmt.Errorf("insert server event: %w", err)
	}
	event := contract.ServerEvent{
		OrgID: scope.OrgID, WorkspaceID: scope.WorkspaceID, RunID: runID,
		Sequence: sequence, StateRevision: stateRevision, Type: input.Type,
		SafePayload: input.Payload, CommittedAt: committedAt.UTC(),
	}
	if err := event.Validate(); err != nil {
		return contract.ServerEvent{}, fmt.Errorf("validate committed server event: %w", err)
	}
	return event, nil
}

func validateCommitSideRecords(runID string, receipt Receipt, audit AuditEntry, event EventInput, outbox OutboxCommand) error {
	if receipt.RunID != runID || audit.TargetRef != runID || outbox.PayloadRef != runID {
		return errors.New("transaction side records do not bind the transitioned RunID")
	}
	if err := receipt.validate(); err != nil {
		return err
	}
	if err := audit.validate(); err != nil {
		return err
	}
	if err := event.validate(); err != nil {
		return err
	}
	return outbox.validate()
}

func loadTaskForUpdate(ctx context.Context, tx *Transaction, ref TaskRef) (contract.Task, error) {
	var encoded []byte
	if err := tx.queryRow(ctx, `SELECT task FROM pm.tasks WHERE run_id = $1 AND task_id = $2 FOR UPDATE`, ref.RunID, ref.TaskID).Scan(&encoded); errors.Is(err, pgx.ErrNoRows) {
		return contract.Task{}, ErrNotFound
	} else if err != nil {
		return contract.Task{}, fmt.Errorf("lock task: %w", err)
	}
	var task contract.Task
	if err := json.Unmarshal(encoded, &task); err != nil {
		return contract.Task{}, fmt.Errorf("decode locked task: %w", err)
	}
	return task, nil
}

func loadRunForUpdate(ctx context.Context, tx *Transaction, runID string) (contract.RunRecord, error) {
	var encoded []byte
	if err := tx.queryRow(ctx, `SELECT record FROM pm.runs WHERE run_id = $1 FOR UPDATE`, runID).Scan(&encoded); errors.Is(err, pgx.ErrNoRows) {
		return contract.RunRecord{}, ErrNotFound
	} else if err != nil {
		return contract.RunRecord{}, fmt.Errorf("lock run: %w", err)
	}
	var run contract.RunRecord
	if err := json.Unmarshal(encoded, &run); err != nil {
		return contract.RunRecord{}, fmt.Errorf("decode locked run: %w", err)
	}
	return run, nil
}

func sameTaskIdentity(left, right contract.Task) bool {
	return left.TaskID == right.TaskID &&
		left.RunID == right.RunID &&
		left.OrgID == right.OrgID &&
		left.WorkspaceID == right.WorkspaceID &&
		left.OperationID == right.OperationID &&
		left.Lane == right.Lane &&
		left.BehaviorID == right.BehaviorID &&
		reflect.DeepEqual(left.InputRefs, right.InputRefs) &&
		reflect.DeepEqual(left.SchemaRefs, right.SchemaRefs)
}

func readDatabaseTime(ctx context.Context, tx *Transaction) (time.Time, error) {
	var value time.Time
	if err := tx.queryRow(ctx, `SELECT clock_timestamp()`).Scan(&value); err != nil {
		return time.Time{}, fmt.Errorf("read database commit time: %w", err)
	}
	return value.UTC(), nil
}
