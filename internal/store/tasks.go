package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type TaskRef struct {
	RunID  string
	TaskID string
}

func (ref TaskRef) validate() error {
	if !validStoreID(ref.RunID) || !validStoreID(ref.TaskID) {
		return errors.New("task reference is invalid")
	}
	return nil
}

func CreateTask(ctx context.Context, tx *Transaction, task contract.Task) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	if err := task.Validate(); err != nil {
		return fmt.Errorf("validate task: %w", err)
	}
	if task.OrgID != scope.OrgID || task.WorkspaceID != scope.WorkspaceID {
		return ErrScopeMismatch
	}
	encoded, err := json.Marshal(task)
	if err != nil {
		return fmt.Errorf("encode task: %w", err)
	}
	updatedAt, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.tasks (
			org_id, workspace_id, run_id, task_id, state, revision, task, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`,
		scope.OrgID,
		scope.WorkspaceID,
		task.RunID,
		task.TaskID,
		string(task.State),
		task.Revision, json.RawMessage(encoded), updatedAt); err != nil {
		return fmt.Errorf("insert task: %w", err)
	}
	return nil
}

func LoadTask(ctx context.Context, tx *Transaction, ref TaskRef) (contract.Task, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return contract.Task{}, err
	}
	if err := ref.validate(); err != nil {
		return contract.Task{}, err
	}
	var encoded []byte
	if err := tx.queryRow(ctx, `
		SELECT task FROM pm.tasks WHERE run_id = $1 AND task_id = $2
	`, ref.RunID, ref.TaskID).Scan(&encoded); errors.Is(err, pgx.ErrNoRows) {
		return contract.Task{}, ErrNotFound
	} else if err != nil {
		return contract.Task{}, fmt.Errorf("load task: %w", err)
	}
	var task contract.Task
	if err := json.Unmarshal(encoded, &task); err != nil {
		return contract.Task{}, fmt.Errorf("decode stored task: %w", err)
	}
	if task.OrgID != scope.OrgID || task.WorkspaceID != scope.WorkspaceID || task.RunID != ref.RunID || task.TaskID != ref.TaskID {
		return contract.Task{}, ErrScopeMismatch
	}
	return task, nil
}
