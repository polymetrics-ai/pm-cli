package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const maximumRunViewPageSize = 101

// RunViewRecord is the complete store-owned input to an authorized run view.
// ConnectionIDs records the immutable accepted connection context. ResultRefs
// contains only actual persisted result references; it is explicitly empty when
// no result publication exists.
type RunViewRecord struct {
	Record        contract.RunRecord
	ConnectionIDs []string
	ResultRefs    []string
}

// MemberOutcomeView retains the typed member truth and the exact persisted
// effect digest used as its public opaque reference. EffectRef must never be
// recomputed from the typed EffectKey at this boundary.
type MemberOutcomeView struct {
	Outcome   contract.MemberOutcome
	EffectRef string
}

// TaskViewRecord is the complete store-owned input to an authorized task view.
type TaskViewRecord struct {
	Record   contract.Task
	Outcomes []MemberOutcomeView
}

func LoadRunView(ctx context.Context, tx *Transaction, runID string) (RunViewRecord, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return RunViewRecord{}, err
	}
	if !validStoreID(runID) {
		return RunViewRecord{}, errors.New("run view identity is invalid")
	}
	var recordJSON, snapshotJSON []byte
	if err := tx.queryRow(ctx, `
		SELECT run.record, input.snapshot
		FROM pm.runs AS run
		JOIN pm.run_input_snapshots AS input
		  ON input.org_id = run.org_id
		 AND input.workspace_id = run.workspace_id
		 AND input.run_id = run.run_id
		WHERE run.org_id = $1 AND run.workspace_id = $2 AND run.run_id = $3
	`, scope.OrgID, scope.WorkspaceID, runID).Scan(&recordJSON, &snapshotJSON); errors.Is(err, pgx.ErrNoRows) {
		return RunViewRecord{}, ErrNotFound
	} else if err != nil {
		return RunViewRecord{}, fmt.Errorf("load run view: %w", err)
	}
	return decodeRunViewRecord(scope, runID, recordJSON, snapshotJSON)
}

// ListRunViews applies the caller's current connection and operation authority
// in PostgreSQL before any record is returned to the application layer. The
// cursor is the last exposed run identity; pages are intentionally not frozen.
func ListRunViews(
	ctx context.Context,
	tx *Transaction,
	allowedConnectionIDs []string,
	allowedOperationIDs []string,
	after string,
	state *contract.RunState,
	limit uint32,
) ([]RunViewRecord, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return nil, err
	}
	if after != "" && !validStoreID(after) ||
		!validStoreIDList(allowedConnectionIDs) ||
		!validStoreOperationList(allowedOperationIDs) ||
		limit == 0 || limit > maximumRunViewPageSize ||
		state != nil && !validRunViewState(*state) {
		return nil, errors.New("run view list bound is invalid")
	}
	stateValue := ""
	if state != nil {
		stateValue = string(*state)
	}
	rows, err := tx.query(ctx, `
		SELECT run.run_id, run.record, input.snapshot
		FROM pm.runs AS run
		JOIN pm.run_input_snapshots AS input
		  ON input.org_id = run.org_id
		 AND input.workspace_id = run.workspace_id
		 AND input.run_id = run.run_id
		JOIN pm.connections AS connection
		  ON connection.org_id = input.org_id
		 AND connection.workspace_id = input.workspace_id
		 AND connection.connection_id = input.connection_id
		WHERE run.org_id = $1 AND run.workspace_id = $2
		  AND run.run_id > $3
		  AND ($4 = '' OR run.state = $4)
		  AND input.connection_id = ANY($5::text[])
		  AND run.record->'spec'->'operation_or_plan_ref'->>'id' = ANY($6::text[])
		  AND connection.state = 'active' AND connection.revoked_at IS NULL
		ORDER BY run.run_id
		LIMIT $7
	`, scope.OrgID, scope.WorkspaceID, after, stateValue, allowedConnectionIDs, allowedOperationIDs, int64(limit))
	if err != nil {
		return nil, fmt.Errorf("list authorized run views: %w", err)
	}
	defer rows.Close()
	views := make([]RunViewRecord, 0, limit)
	for rows.Next() {
		var runID string
		var recordJSON, snapshotJSON []byte
		if err := rows.Scan(&runID, &recordJSON, &snapshotJSON); err != nil {
			return nil, fmt.Errorf("scan authorized run view: %w", err)
		}
		view, err := decodeRunViewRecord(scope, runID, recordJSON, snapshotJSON)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate authorized run views: %w", err)
	}
	return views, nil
}

func LoadTaskView(ctx context.Context, tx *Transaction, ref TaskRef) (TaskViewRecord, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return TaskViewRecord{}, err
	}
	if err := ref.validate(); err != nil {
		return TaskViewRecord{}, err
	}
	var encoded []byte
	if err := tx.queryRow(ctx, `
		SELECT task
		FROM pm.tasks
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND task_id = $4
	`, scope.OrgID, scope.WorkspaceID, ref.RunID, ref.TaskID).Scan(&encoded); errors.Is(err, pgx.ErrNoRows) {
		return TaskViewRecord{}, ErrNotFound
	} else if err != nil {
		return TaskViewRecord{}, fmt.Errorf("load task view: %w", err)
	}
	record, err := decodeTaskViewRecord(scope, ref, encoded)
	if err != nil {
		return TaskViewRecord{}, err
	}
	outcomes, err := loadMemberOutcomeViews(ctx, tx, ref.RunID, []string{ref.TaskID})
	if err != nil {
		return TaskViewRecord{}, err
	}
	return TaskViewRecord{Record: record, Outcomes: outcomes[ref.TaskID]}, nil
}

// ListTaskViews returns a bounded task page while preserving every exact member
// outcome belonging to each returned task. Outcome truth is never truncated.
func ListTaskViews(
	ctx context.Context,
	tx *Transaction,
	runID string,
	allowedOperationIDs []string,
	after string,
	limit uint32,
) ([]TaskViewRecord, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return nil, err
	}
	if !validStoreID(runID) || after != "" && !validStoreID(after) ||
		!validStoreOperationList(allowedOperationIDs) || limit == 0 || limit > maximumRunViewPageSize {
		return nil, errors.New("task view list bound is invalid")
	}
	rows, err := tx.query(ctx, `
		SELECT task_id, task
		FROM pm.tasks
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
		  AND task_id > $4
		  AND task->>'operation_id' = ANY($5::text[])
		ORDER BY task_id
		LIMIT $6
	`, scope.OrgID, scope.WorkspaceID, runID, after, allowedOperationIDs, int64(limit))
	if err != nil {
		return nil, fmt.Errorf("list authorized task views: %w", err)
	}
	defer rows.Close()
	views := make([]TaskViewRecord, 0, limit)
	taskIDs := make([]string, 0, limit)
	for rows.Next() {
		var taskID string
		var encoded []byte
		if err := rows.Scan(&taskID, &encoded); err != nil {
			return nil, fmt.Errorf("scan authorized task view: %w", err)
		}
		record, err := decodeTaskViewRecord(scope, TaskRef{RunID: runID, TaskID: taskID}, encoded)
		if err != nil {
			return nil, err
		}
		views = append(views, TaskViewRecord{Record: record, Outcomes: []MemberOutcomeView{}})
		taskIDs = append(taskIDs, taskID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate authorized task views: %w", err)
	}
	if len(taskIDs) == 0 {
		return views, nil
	}
	outcomes, err := loadMemberOutcomeViews(ctx, tx, runID, taskIDs)
	if err != nil {
		return nil, err
	}
	for index := range views {
		views[index].Outcomes = outcomes[views[index].Record.TaskID]
	}
	return views, nil
}

func decodeRunViewRecord(scope contract.Scope, runID string, recordJSON, snapshotJSON []byte) (RunViewRecord, error) {
	var record contract.RunRecord
	if err := json.Unmarshal(recordJSON, &record); err != nil {
		return RunViewRecord{}, fmt.Errorf("decode stored run view: %w", err)
	}
	var snapshot RunInputSnapshot
	if err := json.Unmarshal(snapshotJSON, &snapshot); err != nil {
		return RunViewRecord{}, fmt.Errorf("decode stored run view input binding: %w", err)
	}
	if err := validateRunInputSnapshot(snapshot); err != nil {
		return RunViewRecord{}, fmt.Errorf("validate stored run view input binding: %w", err)
	}
	if err := record.Validate(); err != nil {
		return RunViewRecord{}, fmt.Errorf("validate stored run view: %w", err)
	}
	if record.Spec.RunID != runID || record.Spec.OrgID != scope.OrgID || record.Spec.WorkspaceID != scope.WorkspaceID ||
		!snapshot.Scope.Equal(scope) || snapshot.RunID != runID || !runSpecContainsInputRef(record.Spec, snapshot.InputRef) {
		return RunViewRecord{}, ErrImmutableConflict
	}
	return RunViewRecord{
		Record:        record,
		ConnectionIDs: []string{snapshot.ConnectionID},
		ResultRefs:    []string{},
	}, nil
}

func decodeTaskViewRecord(scope contract.Scope, ref TaskRef, encoded []byte) (contract.Task, error) {
	var record contract.Task
	if err := json.Unmarshal(encoded, &record); err != nil {
		return contract.Task{}, fmt.Errorf("decode stored task view: %w", err)
	}
	if err := record.Validate(); err != nil {
		return contract.Task{}, fmt.Errorf("validate stored task view: %w", err)
	}
	if record.OrgID != scope.OrgID || record.WorkspaceID != scope.WorkspaceID ||
		record.RunID != ref.RunID || record.TaskID != ref.TaskID {
		return contract.Task{}, ErrImmutableConflict
	}
	return record, nil
}

func loadMemberOutcomeViews(
	ctx context.Context,
	tx *Transaction,
	runID string,
	taskIDs []string,
) (map[string][]MemberOutcomeView, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return nil, err
	}
	result := make(map[string][]MemberOutcomeView, len(taskIDs))
	for _, taskID := range taskIDs {
		result[taskID] = []MemberOutcomeView{}
	}
	rows, err := tx.query(ctx, `
		SELECT outcome.task_id, outcome.dispatch_id, outcome.member_id, outcome.outcome, effect.effect_digest
		FROM pm.member_outcomes AS outcome
		JOIN pm.dispatch_members AS member
		  ON member.org_id = outcome.org_id
		 AND member.workspace_id = outcome.workspace_id
		 AND member.run_id = outcome.run_id
		 AND member.task_id = outcome.task_id
		 AND member.dispatch_id = outcome.dispatch_id
		 AND member.member_id = outcome.member_id
		JOIN pm.dispatches AS dispatch
		  ON dispatch.org_id = outcome.org_id
		 AND dispatch.workspace_id = outcome.workspace_id
		 AND dispatch.run_id = outcome.run_id
		 AND dispatch.task_id = outcome.task_id
		 AND dispatch.dispatch_id = outcome.dispatch_id
		JOIN pm.effects AS effect
		  ON effect.org_id = dispatch.org_id
		 AND effect.workspace_id = dispatch.workspace_id
		 AND effect.run_id = dispatch.run_id
		 AND effect.task_id = dispatch.task_id
		 AND effect.effect_digest = dispatch.effect_digest
		WHERE outcome.org_id = $1 AND outcome.workspace_id = $2 AND outcome.run_id = $3
		  AND outcome.task_id = ANY($4::text[])
		ORDER BY outcome.task_id, dispatch.prepared_at, outcome.dispatch_id, member.ordinal
	`, scope.OrgID, scope.WorkspaceID, runID, taskIDs)
	if err != nil {
		return nil, fmt.Errorf("list exact member outcome views: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var taskID, dispatchID, memberID, effectRef string
		var encoded []byte
		if err := rows.Scan(&taskID, &dispatchID, &memberID, &encoded, &effectRef); err != nil {
			return nil, fmt.Errorf("scan exact member outcome view: %w", err)
		}
		if _, ok := result[taskID]; !ok || !validStoreID(effectRef) {
			return nil, ErrImmutableConflict
		}
		var outcome contract.MemberOutcome
		if err := json.Unmarshal(encoded, &outcome); err != nil {
			return nil, fmt.Errorf("decode exact member outcome view: %w", err)
		}
		if err := outcome.Validate(); err != nil {
			return nil, fmt.Errorf("validate exact member outcome view: %w", err)
		}
		if string(outcome.DispatchID) != dispatchID || string(outcome.MemberID) != memberID {
			return nil, ErrImmutableConflict
		}
		result[taskID] = append(result[taskID], MemberOutcomeView{Outcome: outcome, EffectRef: effectRef})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate exact member outcome views: %w", err)
	}
	return result, nil
}

func validRunViewState(state contract.RunState) bool {
	switch state {
	case contract.RunStateAccepted,
		contract.RunStateQueued,
		contract.RunStateRunning,
		contract.RunStateAwaitingApproval,
		contract.RunStateBlocked,
		contract.RunStateReconciling,
		contract.RunStateSucceeded,
		contract.RunStatePartial,
		contract.RunStateFailed,
		contract.RunStateCancelled:
		return true
	default:
		return false
	}
}
