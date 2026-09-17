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

var ErrWorkerRouteUnknown = fmt.Errorf("worker route is unavailable: %w", ErrNotFound)

// WorkerRouteDescriptor is the sealed workflow identity presented by a real
// Temporal activity. Scope and actor are intentionally absent: PostgreSQL
// derives both from the accepted command before the normal RLS boundary opens.
type WorkerRouteDescriptor struct {
	RunID          string
	StartCommandID string
	Namespace      string
	WorkflowID     string
	TemporalRunID  string
	Generation     uint64
	IntentDigest   contract.SemanticDigest
}

type WorkerRoute struct {
	Scope contract.Scope
	Actor contract.Actor
}

type WorkerScopedTransactionFunc func(context.Context, *Transaction, WorkerRoute) error

func (descriptor WorkerRouteDescriptor) validate() error {
	if !validStoreID(descriptor.RunID) || !validStoreID(descriptor.StartCommandID) ||
		!validStoreID(descriptor.Namespace) || !validStoreID(descriptor.WorkflowID) ||
		!validStoreID(descriptor.TemporalRunID) || descriptor.Generation == 0 || descriptor.Generation > maxStoreFence {
		return errors.New("worker route descriptor is invalid")
	}
	if err := descriptor.IntentDigest.Validate(); err != nil {
		return fmt.Errorf("validate worker route intent digest: %w", err)
	}
	return nil
}

func (route WorkerRoute) validate() error {
	if err := route.Scope.Validate(); err != nil {
		return err
	}
	if err := route.Actor.Validate(); err != nil {
		return err
	}
	if route.Actor.RequestedActor.OrgID != route.Scope.OrgID ||
		route.Actor.EffectiveActor.OrgID != route.Scope.OrgID {
		return ErrScopeMismatch
	}
	return nil
}

// WithinWorkerScope resolves the accepted route and installs its immutable
// scope and actor in one transaction. Caller-supplied tenant fields are never
// used to choose the RLS scope.
func WithinWorkerScope(
	ctx context.Context,
	pool *Pool,
	descriptor WorkerRouteDescriptor,
	fn WorkerScopedTransactionFunc,
) error {
	if err := descriptor.validate(); err != nil {
		return err
	}
	if fn == nil {
		return errors.New("worker scoped transaction callback is required")
	}
	return WithinTransaction(ctx, pool, func(ctx context.Context, tx *Transaction) error {
		route, err := resolveWorkerRoute(ctx, tx, descriptor)
		if err != nil {
			return err
		}
		if err := installScope(ctx, tx, route.Scope, route.Actor); err != nil {
			return err
		}
		return fn(ctx, tx, route)
	})
}

// ObserveWorkerContinuation appends one trusted, verified Continue-As-New
// member without accepting tenant authority from workflow payloads. The
// descriptor and observation are first matched by a bounded definer route,
// then every sealed fact and the current predecessor are rechecked under the
// run lock before the binding evidence changes.
func ObserveWorkerContinuation(
	ctx context.Context,
	pool *Pool,
	descriptor WorkerRouteDescriptor,
	observation contract.WorkflowContinuationObservation,
) error {
	if err := descriptor.validate(); err != nil {
		return err
	}
	if err := observation.Validate(); err != nil {
		return err
	}
	if observation.Execution != (contract.WorkflowExecution{
		Namespace: descriptor.Namespace, WorkflowID: descriptor.WorkflowID, RunID: descriptor.TemporalRunID,
	}) {
		return ErrWorkflowBindingConflict
	}
	return WithinTransaction(ctx, pool, func(ctx context.Context, tx *Transaction) error {
		route, err := resolveWorkerContinuationRoute(ctx, tx, descriptor, observation)
		if err != nil {
			return err
		}
		if err := installScope(ctx, tx, route.Scope, route.Actor); err != nil {
			return err
		}
		intent, err := loadWorkflowCommandIntent(ctx, tx, descriptor.RunID, descriptor.StartCommandID)
		if err != nil {
			return err
		}
		if !intent.Scope.Equal(route.Scope) || intent.RunID != descriptor.RunID ||
			intent.CommandID != descriptor.StartCommandID || intent.Digest != descriptor.IntentDigest ||
			(intent.Kind != WorkflowCommandStartRun && intent.Kind != WorkflowCommandEnsureBinding) ||
			intent.Plan.Namespace != descriptor.Namespace || intent.Plan.WorkflowID != descriptor.WorkflowID ||
			intent.Plan.Generation != descriptor.Generation ||
			(intent.Plan.WorkflowType != contract.RunWorkflowType &&
				intent.Plan.WorkflowType != contract.ScheduledWorkflowType) ||
			intent.Plan.TaskQueue != observation.TaskQueue ||
			intent.Plan.ExecutionTimeout != observation.ExecutionTimeout {
			return ErrWorkflowBindingConflict
		}
		stored, err := loadRunForUpdate(ctx, tx, descriptor.RunID)
		if err != nil {
			return err
		}
		if stored.Spec.OrgID != route.Scope.OrgID ||
			stored.Spec.WorkspaceID != route.Scope.WorkspaceID ||
			!stored.Spec.Actor.Equal(route.Actor) ||
			stored.WorkflowBinding.RunID != descriptor.RunID ||
			stored.WorkflowBinding.WorkflowID != descriptor.WorkflowID ||
			stored.WorkflowBinding.Generation != descriptor.Generation ||
			stored.WorkflowBinding.Origin != intent.Plan.Origin ||
			stored.Spec.Budget.Digest != intent.Plan.BudgetDigest {
			return ErrWorkflowBindingConflict
		}
		observations := stored.WorkflowBinding.Observations
		if len(observations) == 0 ||
			observations[0].TemporalRunID != observation.FirstExecutionRunID {
			return ErrWorkflowBindingConflict
		}
		if observations[len(observations)-1].TemporalRunID == descriptor.TemporalRunID {
			if len(observations) < 2 ||
				observations[len(observations)-2].TemporalRunID != observation.PreviousRunID {
				return ErrWorkflowBindingConflict
			}
			return nil
		}
		if observations[len(observations)-1].TemporalRunID != observation.PreviousRunID {
			return ErrWorkflowBindingConflict
		}
		for _, existing := range observations {
			if existing.TemporalRunID == descriptor.TemporalRunID {
				return ErrWorkflowBindingConflict
			}
		}
		observedAt, err := readDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if observedAt.Before(stored.ProjectionObservedAt) {
			observedAt = stored.ProjectionObservedAt
		}
		if observedAt.Before(observations[len(observations)-1].ObservedAt) {
			observedAt = observations[len(observations)-1].ObservedAt
		}
		binding, err := bindWorkflowChain(
			stored.WorkflowBinding,
			observation.FirstExecutionRunID,
			descriptor.TemporalRunID,
			observedAt,
		)
		if err != nil {
			return err
		}
		updated, err := stored.ObserveOrchestration(contract.OrchestrationRunning, observedAt)
		if err != nil {
			return err
		}
		updated.WorkflowBinding = binding
		if err := validateRunProjectionUpdate(stored, updated); err != nil {
			return err
		}
		encoded, err := json.Marshal(updated)
		if err != nil {
			return fmt.Errorf("encode continued worker binding: %w", err)
		}
		tag, err := tx.exec(ctx, `
			UPDATE pm.runs
			SET state = $4, revision = $5, record = $6, updated_at = $7
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND revision = $8
		`, route.Scope.OrgID, route.Scope.WorkspaceID, descriptor.RunID,
			string(updated.State), int64(updated.Revision), json.RawMessage(encoded),
			observedAt, int64(stored.Revision))
		if err != nil {
			return fmt.Errorf("append continued worker binding: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrCASConflict
		}
		reason := "workflow_continuation_observed"
		material := []string{
			route.Scope.OrgID,
			route.Scope.WorkspaceID,
			descriptor.RunID,
			descriptor.StartCommandID,
			fmt.Sprint(descriptor.Generation),
			descriptor.TemporalRunID,
		}
		payload := workflowSafePayload(
			descriptor.RunID, descriptor.WorkflowID, descriptor.TemporalRunID, reason,
		)
		if err := insertReceipt(ctx, tx, Receipt{
			ReceiptID: workflowRecordID("worker-continuation-receipt", material...),
			Kind:      reason,
			RunID:     descriptor.RunID,
			Payload:   payload,
		}, observedAt); err != nil {
			return err
		}
		if err := insertAuditEntry(ctx, tx, AuditEntry{
			AuditID:   workflowRecordID("worker-continuation-audit", material...),
			Action:    reason,
			TargetRef: descriptor.RunID,
			Payload:   payload,
		}, observedAt); err != nil {
			return err
		}
		if _, err := insertEvent(ctx, tx, descriptor.RunID, updated.Revision, EventInput{
			EventID: workflowRecordID("worker-continuation-event", material...),
			Type:    reason,
			Payload: payload,
		}, observedAt); err != nil {
			return err
		}
		return nil
	})
}

func resolveWorkerContinuationRoute(
	ctx context.Context,
	tx *Transaction,
	descriptor WorkerRouteDescriptor,
	observation contract.WorkflowContinuationObservation,
) (WorkerRoute, error) {
	if tx == nil || tx.tx == nil || tx.scoped {
		return WorkerRoute{}, errors.New("unscoped store transaction is required")
	}
	var route WorkerRoute
	var actorJSON []byte
	err := tx.queryRow(ctx, `
		SELECT org_id, workspace_id, actor
		FROM pm.resolve_workflow_continuation(
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
		)
	`,
		descriptor.RunID,
		descriptor.StartCommandID,
		descriptor.Namespace,
		descriptor.WorkflowID,
		int64(descriptor.Generation),
		string(descriptor.IntentDigest),
		descriptor.TemporalRunID,
		observation.FirstExecutionRunID,
		observation.PreviousRunID,
		observation.WorkflowType,
		observation.TaskQueue,
		int64(observation.ExecutionTimeout),
	).Scan(&route.Scope.OrgID, &route.Scope.WorkspaceID, &actorJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkerRoute{}, ErrWorkerRouteUnknown
	}
	if err != nil {
		return WorkerRoute{}, fmt.Errorf("resolve sealed workflow continuation route: %w", err)
	}
	if err := json.Unmarshal(actorJSON, &route.Actor); err != nil {

		return WorkerRoute{}, fmt.Errorf("decode accepted workflow continuation actor: %w", err)
	}
	if err := route.validate(); err != nil {
		return WorkerRoute{}, fmt.Errorf("validate accepted workflow continuation route: %w", err)
	}
	return route, nil
}

func resolveWorkerRoute(ctx context.Context, tx *Transaction, descriptor WorkerRouteDescriptor) (WorkerRoute, error) {
	if tx == nil || tx.tx == nil || tx.scoped {
		return WorkerRoute{}, errors.New("unscoped store transaction is required")
	}
	var route WorkerRoute
	var actorJSON []byte
	err := tx.queryRow(ctx, `
		SELECT org_id, workspace_id, actor
		FROM pm.resolve_workflow_activity($1, $2, $3, $4, $5, $6, $7)
	`,
		descriptor.RunID,
		descriptor.StartCommandID,
		descriptor.Namespace,
		descriptor.WorkflowID,
		int64(descriptor.Generation),
		string(descriptor.IntentDigest),
		descriptor.TemporalRunID,
	).Scan(&route.Scope.OrgID, &route.Scope.WorkspaceID, &actorJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkerRoute{}, ErrWorkerRouteUnknown
	}
	if err != nil {
		return WorkerRoute{}, fmt.Errorf("resolve sealed workflow activity route: %w", err)
	}
	if err := json.Unmarshal(actorJSON, &route.Actor); err != nil {
		return WorkerRoute{}, fmt.Errorf("decode accepted workflow actor: %w", err)
	}
	if err := route.validate(); err != nil {
		return WorkerRoute{}, fmt.Errorf("validate accepted workflow route: %w", err)
	}
	return route, nil
}

func LoadWorkerRunForUpdate(ctx context.Context, tx *Transaction, runID string) (contract.RunRecord, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return contract.RunRecord{}, err
	}
	if !validStoreID(runID) {
		return contract.RunRecord{}, errors.New("worker run identity is invalid")
	}
	record, err := loadRunForUpdate(ctx, tx, runID)
	if err != nil {
		return contract.RunRecord{}, err
	}
	if record.Spec.RunID != runID || record.Spec.OrgID != scope.OrgID ||
		record.Spec.WorkspaceID != scope.WorkspaceID {
		return contract.RunRecord{}, ErrScopeMismatch
	}
	if err := record.Validate(); err != nil {
		return contract.RunRecord{}, fmt.Errorf("validate locked worker run: %w", err)
	}
	return record, nil
}

func StartWorkerRun(
	ctx context.Context,
	tx *Transaction,
	current contract.RunRecord,
	observedAt time.Time,
) (contract.RunRecord, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return contract.RunRecord{}, err
	}
	if err := current.Validate(); err != nil {
		return contract.RunRecord{}, fmt.Errorf("validate worker run start: %w", err)
	}
	if current.Spec.OrgID != scope.OrgID || current.Spec.WorkspaceID != scope.WorkspaceID {
		return contract.RunRecord{}, ErrScopeMismatch
	}
	if len(current.WorkflowBinding.Observations) == 0 {
		return contract.RunRecord{}, ErrWorkflowBindingConflict
	}
	if current.CancelRequested ||
		(current.State != contract.RunStateAccepted && current.State != contract.RunStateQueued) {
		return contract.RunRecord{}, ErrImmutableConflict
	}
	if observedAt.Before(current.ProjectionObservedAt) {
		observedAt = current.ProjectionObservedAt
	}
	started, err := current.Transition(
		contract.RunStateRunning,
		"worker_execution_started",
		observedAt,
	)
	if err != nil {
		return contract.RunRecord{}, err
	}
	started.OrchestrationCondition = contract.OrchestrationRunning
	if err := started.Validate(); err != nil {
		return contract.RunRecord{}, err
	}
	revision := fmt.Sprint(started.Revision)
	reason := "worker_execution_started"
	payload := workflowSafePayload(
		current.Spec.RunID,
		current.WorkflowBinding.WorkflowID,
		current.WorkflowBinding.Observations[len(current.WorkflowBinding.Observations)-1].TemporalRunID,
		reason,
	)
	if _, err := CommitRunTransition(ctx, tx, RunTransitionCommit{
		Run: started,
		Receipt: Receipt{
			ReceiptID: workflowRecordID("worker-start-receipt", current.Spec.RunID, revision),
			Kind:      reason,
			RunID:     current.Spec.RunID,
			Payload:   payload,
		},
		Audit: AuditEntry{
			AuditID:   workflowRecordID("worker-start-audit", current.Spec.RunID, revision),
			Action:    reason,
			TargetRef: current.Spec.RunID,
			Payload:   payload,
		},
		Event: EventInput{
			EventID: workflowRecordID("worker-start-event", current.Spec.RunID, revision),
			Type:    reason,
			Payload: payload,
		},
		Outbox: OutboxCommand{
			CommandID:  workflowRecordID("worker-start-outbox", current.Spec.RunID, revision),
			Kind:       reason,
			PayloadRef: current.Spec.RunID,
		},
	}); err != nil {
		return contract.RunRecord{}, err
	}
	return started, nil
}

func StartWorkerTask(
	ctx context.Context,
	tx *Transaction,
	lease Lease,
	current contract.Task,
	observedAt time.Time,
) (contract.Task, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return contract.Task{}, err
	}
	if err := current.Validate(); err != nil {
		return contract.Task{}, fmt.Errorf("validate worker task start: %w", err)
	}
	if current.OrgID != scope.OrgID || current.WorkspaceID != scope.WorkspaceID ||
		current.RunID != lease.Task.RunID || current.TaskID != lease.Task.TaskID {
		return contract.Task{}, ErrScopeMismatch
	}
	if current.State == contract.RunStateRunning {
		if err := requireCurrentLease(ctx, tx, lease); err != nil {
			return contract.Task{}, err
		}
		locked, err := loadTaskForUpdate(ctx, tx, lease.Task)
		if err != nil {
			return contract.Task{}, err
		}
		if locked.CancelRequested || locked.State != contract.RunStateRunning {
			return contract.Task{}, ErrCASConflict
		}
		return locked, nil
	}
	if current.CancelRequested ||
		(current.State != contract.RunStateAccepted && current.State != contract.RunStateQueued) {
		return contract.Task{}, ErrImmutableConflict
	}
	if observedAt.Before(current.ProjectionObservedAt) {
		observedAt = current.ProjectionObservedAt
	}
	started := current
	started.State = contract.RunStateRunning
	started.ProjectionObservedAt = observedAt
	started.OrchestrationCondition = contract.OrchestrationRunning
	started.Revision++
	if err := started.Validate(); err != nil {
		return contract.Task{}, err
	}
	revision := fmt.Sprint(started.Revision)
	reason := "worker_execution_started"
	payload := contract.SafePayload{
		References: []contract.SafeReference{
			{Name: "run", Ref: current.RunID},
			{Name: "task", Ref: current.TaskID},
		},
		Counts:      []contract.SafeCount{{Name: "task_revision", Value: started.Revision}},
		ReasonCodes: []string{reason},
	}
	if _, err := CommitTaskTransition(ctx, tx, TaskTransitionCommit{
		Lease: lease,
		Task:  started,
		Receipt: Receipt{
			ReceiptID: workflowRecordID("worker-task-start-receipt", current.RunID, current.TaskID, revision),
			Kind:      reason,
			RunID:     current.RunID,
			Payload:   payload,
		},
		Audit: AuditEntry{
			AuditID:   workflowRecordID("worker-task-start-audit", current.RunID, current.TaskID, revision),
			Action:    reason,
			TargetRef: current.RunID,
			Payload:   payload,
		},
		Event: EventInput{
			EventID: workflowRecordID("worker-task-start-event", current.RunID, current.TaskID, revision),
			Type:    reason,
			Payload: payload,
		},
		Outbox: OutboxCommand{
			CommandID:  workflowRecordID("worker-task-start-outbox", current.RunID, current.TaskID, revision),
			Kind:       reason,
			PayloadRef: current.RunID,
		},
	}); err != nil {
		return contract.Task{}, err
	}
	return started, nil
}

func LoadWorkerAcceptedAt(ctx context.Context, tx *Transaction, runID string) (time.Time, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return time.Time{}, err
	}
	if !validStoreID(runID) {
		return time.Time{}, errors.New("worker run identity is invalid")
	}
	var acceptedAt time.Time
	if err := tx.queryRow(ctx, `
		SELECT accepted_at
		FROM pm.run_specs
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
	`, scope.OrgID, scope.WorkspaceID, runID).Scan(&acceptedAt); errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrNotFound
	} else if err != nil {
		return time.Time{}, fmt.Errorf("load accepted worker run time: %w", err)
	}
	return acceptedAt.UTC(), nil
}

// SealWorkerActivityTimeouts records the first real worker bootstrap policy.
// Later orchestration generations load that immutable policy even if deployment
// defaults change; accepted run budgets are never rewritten.
func SealWorkerActivityTimeouts(
	ctx context.Context,
	tx *Transaction,
	runID, commandID string,
	generation uint64,
	timeouts contract.ActivityTimeouts,
) (contract.ActivityTimeouts, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return contract.ActivityTimeouts{}, err
	}
	if !validStoreID(runID) || !validStoreID(commandID) || generation == 0 || generation > maxStoreFence {
		return contract.ActivityTimeouts{}, errors.New("worker activity policy identity is invalid")
	}
	if err := timeouts.Validate(); err != nil {
		return contract.ActivityTimeouts{}, err
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.workflow_run_policies (
			org_id, workspace_id, run_id, sealed_by_command_id, sealed_generation,
			schedule_to_close_nanoseconds, start_to_close_nanoseconds,
			heartbeat_nanoseconds, transport_nanoseconds, sealed_at
		)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, clock_timestamp()
		FROM pm.run_specs
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
		ON CONFLICT (org_id, workspace_id, run_id) DO NOTHING
	`,
		scope.OrgID, scope.WorkspaceID, runID, commandID, int64(generation),
		int64(timeouts.ScheduleToClose), int64(timeouts.StartToClose),
		int64(timeouts.Heartbeat), int64(timeouts.Transport),
	); err != nil {
		return contract.ActivityTimeouts{}, safeMetadataWriteError("seal worker activity policy", err)
	}
	return LoadWorkerActivityTimeouts(ctx, tx, runID)
}

func LoadWorkerActivityTimeouts(
	ctx context.Context,
	tx *Transaction,
	runID string,
) (contract.ActivityTimeouts, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return contract.ActivityTimeouts{}, err
	}
	if !validStoreID(runID) {
		return contract.ActivityTimeouts{}, errors.New("worker activity policy run identity is invalid")
	}
	var scheduleToClose, startToClose, heartbeat, transport int64
	if err := tx.queryRow(ctx, `
		SELECT schedule_to_close_nanoseconds, start_to_close_nanoseconds,
		       heartbeat_nanoseconds, transport_nanoseconds
		FROM pm.workflow_run_policies
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
	`, scope.OrgID, scope.WorkspaceID, runID).Scan(
		&scheduleToClose, &startToClose, &heartbeat, &transport,
	); errors.Is(err, pgx.ErrNoRows) {
		return contract.ActivityTimeouts{}, ErrNotFound
	} else if err != nil {
		return contract.ActivityTimeouts{}, fmt.Errorf("load sealed worker activity policy: %w", err)
	}
	sealed := contract.ActivityTimeouts{
		ScheduleToClose: time.Duration(scheduleToClose),
		StartToClose:    time.Duration(startToClose),
		Heartbeat:       time.Duration(heartbeat),
		Transport:       time.Duration(transport),
	}
	if err := sealed.Validate(); err != nil {
		return contract.ActivityTimeouts{}, fmt.Errorf("validate sealed worker activity policy: %w", err)
	}
	return sealed, nil
}

// EnsureWorkerTask makes deterministic application-owned task materialization
// idempotent without weakening CreateTask's duplicate refusal.
func EnsureWorkerTask(ctx context.Context, tx *Transaction, task contract.Task) (contract.Task, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return contract.Task{}, err
	}
	if err := task.Validate(); err != nil {
		return contract.Task{}, fmt.Errorf("validate worker task: %w", err)
	}
	if task.OrgID != scope.OrgID || task.WorkspaceID != scope.WorkspaceID {
		return contract.Task{}, ErrScopeMismatch
	}
	encoded, err := json.Marshal(task)
	if err != nil {
		return contract.Task{}, fmt.Errorf("encode worker task: %w", err)
	}
	tag, err := tx.exec(ctx, `
		INSERT INTO pm.tasks (
			org_id, workspace_id, run_id, task_id, state, revision, task, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, clock_timestamp())
		ON CONFLICT (org_id, workspace_id, run_id, task_id) DO NOTHING
	`, scope.OrgID, scope.WorkspaceID, task.RunID, task.TaskID,
		string(task.State), int64(task.Revision), json.RawMessage(encoded))
	if err != nil {
		return contract.Task{}, safeMetadataWriteError("ensure worker task", err)
	}
	if tag.RowsAffected() == 1 {
		return task, nil
	}
	stored, err := LoadTask(ctx, tx, TaskRef{RunID: task.RunID, TaskID: task.TaskID})
	if err != nil {
		return contract.Task{}, err
	}
	if !reflect.DeepEqual(stored, task) {
		return contract.Task{}, ErrImmutableConflict
	}
	return stored, nil
}

func ListWorkerTasks(ctx context.Context, tx *Transaction, runID string) ([]contract.Task, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return nil, err
	}
	if !validStoreID(runID) {
		return nil, errors.New("worker run identity is invalid")
	}
	rows, err := tx.query(ctx, `
		SELECT task
		FROM pm.tasks
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
		ORDER BY task_id
	`, scope.OrgID, scope.WorkspaceID, runID)
	if err != nil {
		return nil, fmt.Errorf("list worker tasks: %w", err)
	}
	defer rows.Close()
	tasks := make([]contract.Task, 0)
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			return nil, fmt.Errorf("scan worker task: %w", err)
		}
		var task contract.Task
		if err := json.Unmarshal(encoded, &task); err != nil {
			return nil, fmt.Errorf("decode worker task: %w", err)
		}
		if err := task.Validate(); err != nil {
			return nil, fmt.Errorf("validate stored worker task: %w", err)
		}
		if task.OrgID != scope.OrgID || task.WorkspaceID != scope.WorkspaceID || task.RunID != runID {
			return nil, ErrScopeMismatch
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate worker tasks: %w", err)
	}
	return tasks, nil
}

// LoadWorkerDispatch requires the actual sealed execution-plan ledger entry.
// It never derives an effect, member, payload, or request witness from a task.
func LoadWorkerDispatch(ctx context.Context, tx *Transaction, task TaskRef) (DispatchRecord, error) {
	if _, err := tx.requireScope(); err != nil {
		return DispatchRecord{}, err
	}
	if err := task.validate(); err != nil {
		return DispatchRecord{}, err
	}
	rows, err := tx.query(ctx, `
		SELECT dispatch_id
		FROM pm.dispatches
		WHERE run_id = $1 AND task_id = $2
		ORDER BY dispatch_id
		LIMIT 2
	`, task.RunID, task.TaskID)
	if err != nil {
		return DispatchRecord{}, fmt.Errorf("locate worker dispatch: %w", err)
	}
	defer rows.Close()
	ids := make([]contract.DispatchID, 0, 2)
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return DispatchRecord{}, fmt.Errorf("scan worker dispatch identity: %w", err)
		}
		id, err := contract.NewDispatchID(encoded)
		if err != nil {
			return DispatchRecord{}, fmt.Errorf("validate worker dispatch identity: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return DispatchRecord{}, fmt.Errorf("iterate worker dispatch identities: %w", err)
	}
	if len(ids) == 0 {
		return DispatchRecord{}, ErrNotFound
	}
	if len(ids) != 1 {
		return DispatchRecord{}, ErrImmutableConflict
	}
	return LoadDispatch(ctx, tx, task, ids[0])
}

func ListWorkerRunEffects(ctx context.Context, tx *Transaction, runID string) ([]contract.EffectRecord, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return nil, err
	}
	if !validStoreID(runID) {
		return nil, errors.New("worker run identity is invalid")
	}
	rows, err := tx.query(ctx, `
		SELECT effect_record
		FROM pm.effects
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
		ORDER BY task_id, effect_digest
	`, scope.OrgID, scope.WorkspaceID, runID)
	if err != nil {
		return nil, fmt.Errorf("list worker run effects: %w", err)
	}
	defer rows.Close()
	effects := make([]contract.EffectRecord, 0)
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			return nil, fmt.Errorf("scan worker effect: %w", err)
		}
		var effect contract.EffectRecord
		if err := json.Unmarshal(encoded, &effect); err != nil {
			return nil, fmt.Errorf("decode worker effect: %w", err)
		}
		if err := effect.Validate(); err != nil {
			return nil, fmt.Errorf("validate stored worker effect: %w", err)
		}
		if effect.OrgID != scope.OrgID || effect.WorkspaceID != scope.WorkspaceID || effect.RunID != runID {
			return nil, ErrScopeMismatch
		}
		effects = append(effects, effect)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate worker effects: %w", err)
	}
	return effects, nil
}

func ListWorkerCommands(ctx context.Context, tx *Transaction, runID string) ([]contract.WorkflowCommandRef, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return nil, err
	}
	if !validStoreID(runID) {
		return nil, errors.New("worker run identity is invalid")
	}
	rows, err := tx.query(ctx, `
		SELECT command.command_id
		FROM pm.outbox_commands AS command
		LEFT JOIN pm.workflow_command_receipts AS receipt
		  ON receipt.org_id = command.org_id
		 AND receipt.workspace_id = command.workspace_id
		 AND receipt.run_id = command.run_id
		 AND receipt.command_id = command.command_id
		WHERE command.org_id = $1 AND command.workspace_id = $2 AND command.run_id = $3
		  AND command.kind = 'cancel_run'
		  AND command.acknowledged_at IS NULL
		  AND receipt.command_id IS NULL
		ORDER BY command.created_at, command.command_id
	`, scope.OrgID, scope.WorkspaceID, runID)
	if err != nil {
		return nil, fmt.Errorf("list worker commands: %w", err)
	}
	defer rows.Close()
	commands := make([]contract.WorkflowCommandRef, 0)
	for rows.Next() {
		var command contract.WorkflowCommandRef
		if err := rows.Scan(&command.CommandID); err != nil {
			return nil, fmt.Errorf("scan worker command: %w", err)
		}
		if err := command.Validate(); err != nil {
			return nil, err
		}
		commands = append(commands, command)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate worker commands: %w", err)
	}
	return commands, nil
}

func LoadWorkerCommandIntent(ctx context.Context, tx *Transaction, runID, commandID string) (WorkflowCommandIntent, error) {
	return loadWorkflowCommandIntent(ctx, tx, runID, commandID)
}

type WorkerCommandReceipt struct {
	Result      contract.WorkflowCommandResult
	RunRevision uint64
	AppliedAt   time.Time
}

func ApplyWorkerCommand(
	ctx context.Context,
	tx *Transaction,
	runID string,
	generation uint64,
	commandID string,
) (WorkerCommandReceipt, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return WorkerCommandReceipt{}, err
	}
	if !validStoreID(runID) || !validStoreID(commandID) || generation == 0 || generation > maxStoreFence {
		return WorkerCommandReceipt{}, errors.New("worker command identity is invalid")
	}
	if receipt, found, err := loadWorkerCommandReceipt(ctx, tx, runID, generation, commandID); err != nil {
		return WorkerCommandReceipt{}, err
	} else if found {
		return receipt, nil
	}

	var kind, payloadRef, state string
	var acknowledgedAt *time.Time
	var runJSON []byte
	err = tx.queryRow(ctx, `
		SELECT command.kind, command.payload_ref, command.state, command.acknowledged_at, run.record
		FROM pm.outbox_commands AS command
		JOIN pm.runs AS run
		  ON run.org_id = command.org_id
		 AND run.workspace_id = command.workspace_id
		 AND run.run_id = command.run_id
		WHERE command.org_id = $1 AND command.workspace_id = $2
		  AND command.run_id = $3 AND command.command_id = $4
		FOR UPDATE OF command, run
	`, scope.OrgID, scope.WorkspaceID, runID, commandID).Scan(
		&kind, &payloadRef, &state, &acknowledgedAt, &runJSON,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkerCommandReceipt{}, ErrNotFound
	}
	if err != nil {
		return WorkerCommandReceipt{}, fmt.Errorf("lock worker command: %w", err)
	}
	if kind != "cancel_run" || payloadRef != runID {
		return WorkerCommandReceipt{}, ErrImmutableConflict
	}
	if state == "acknowledged" && acknowledgedAt != nil {
		if receipt, found, err := loadWorkerCommandReceipt(
			ctx, tx, runID, generation, commandID,
		); err != nil {
			return WorkerCommandReceipt{}, err
		} else if found {
			return receipt, nil
		}
		return WorkerCommandReceipt{}, ErrImmutableConflict
	}
	if state != "pending" || acknowledgedAt != nil {
		return WorkerCommandReceipt{}, ErrImmutableConflict
	}
	var run contract.RunRecord
	if err := json.Unmarshal(runJSON, &run); err != nil {
		return WorkerCommandReceipt{}, fmt.Errorf("decode worker command run: %w", err)
	}
	if err := run.Validate(); err != nil {
		return WorkerCommandReceipt{}, fmt.Errorf("validate worker command run: %w", err)
	}
	if run.Spec.RunID != runID || run.Spec.OrgID != scope.OrgID || run.Spec.WorkspaceID != scope.WorkspaceID || !run.CancelRequested {
		return WorkerCommandReceipt{}, ErrImmutableConflict
	}

	var appliedAt time.Time
	if err := tx.queryRow(ctx, `
		UPDATE pm.outbox_commands
		SET state = 'acknowledged', acknowledged_at = clock_timestamp(),
		    claim_owner = NULL, claim_expires_at = NULL
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND command_id = $4
		  AND kind = 'cancel_run' AND payload_ref = $3
		  AND state = 'pending' AND acknowledged_at IS NULL
		RETURNING acknowledged_at
	`, scope.OrgID, scope.WorkspaceID, runID, commandID).Scan(&appliedAt); errors.Is(err, pgx.ErrNoRows) {
		return WorkerCommandReceipt{}, ErrCASConflict
	} else if err != nil {
		return WorkerCommandReceipt{}, fmt.Errorf("acknowledge worker command: %w", err)
	}
	result := contract.WorkflowCommandResult{CommandID: commandID, Applied: true, CancelRequested: true}
	if err := result.Validate(); err != nil {
		return WorkerCommandReceipt{}, err
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.workflow_command_receipts (
			org_id, workspace_id, run_id, generation, command_id,
			applied, cancel_requested, run_revision, applied_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, scope.OrgID, scope.WorkspaceID, runID, int64(generation), commandID,
		result.Applied, result.CancelRequested, int64(run.Revision), appliedAt); err != nil {
		return WorkerCommandReceipt{}, safeMetadataWriteError("insert worker command receipt", err)
	}
	return WorkerCommandReceipt{Result: result, RunRevision: run.Revision, AppliedAt: appliedAt.UTC()}, nil
}

func loadWorkerCommandReceipt(
	ctx context.Context,
	tx *Transaction,
	runID string,
	generation uint64,
	commandID string,
) (WorkerCommandReceipt, bool, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return WorkerCommandReceipt{}, false, err
	}
	var receipt WorkerCommandReceipt
	var runRevision int64
	err = tx.queryRow(ctx, `
		SELECT applied, cancel_requested, run_revision, applied_at
		FROM pm.workflow_command_receipts
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
		  AND generation = $4 AND command_id = $5
	`, scope.OrgID, scope.WorkspaceID, runID, int64(generation), commandID).Scan(
		&receipt.Result.Applied,
		&receipt.Result.CancelRequested,
		&runRevision,
		&receipt.AppliedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkerCommandReceipt{}, false, nil
	}
	if err != nil {
		return WorkerCommandReceipt{}, false, fmt.Errorf("load worker command receipt: %w", err)
	}
	if runRevision <= 0 {
		return WorkerCommandReceipt{}, false, ErrImmutableConflict
	}
	receipt.Result.CommandID = commandID
	receipt.RunRevision = uint64(runRevision)
	receipt.AppliedAt = receipt.AppliedAt.UTC()
	if err := receipt.Result.Validate(); err != nil {
		return WorkerCommandReceipt{}, false, fmt.Errorf("validate worker command receipt: %w", err)
	}
	return receipt, true, nil
}

// LoadWorkerProjectionIntent proves that a repair projection request was
// authorized by one exact, durably sealed repair command. The workflow update
// payload's digest is only a selector; it never confers repair authority.
func LoadWorkerProjectionIntent(
	ctx context.Context,
	tx *Transaction,
	runID string,
	generation uint64,
	intentDigest contract.SemanticDigest,
) (WorkflowCommandIntent, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return WorkflowCommandIntent{}, err
	}
	if !validStoreID(runID) || generation == 0 || generation > maxStoreFence {
		return WorkflowCommandIntent{}, errors.New("worker projection intent identity is invalid")
	}
	if err := intentDigest.Validate(); err != nil {
		return WorkflowCommandIntent{}, err
	}
	rows, err := tx.query(ctx, `
		SELECT intent.command_id
		FROM pm.workflow_command_intents AS intent
		JOIN pm.outbox_commands AS command
		  ON command.org_id = intent.org_id
		 AND command.workspace_id = intent.workspace_id
		 AND command.run_id = intent.run_id
		 AND command.command_id = intent.command_id
		WHERE intent.org_id = $1 AND intent.workspace_id = $2 AND intent.run_id = $3
		  AND intent.kind = $4 AND intent.generation = $5
		  AND intent.command_digest = $6
		  AND command.kind = $4 AND command.payload_ref = $3
		ORDER BY intent.command_id
		LIMIT 2
	`, scope.OrgID, scope.WorkspaceID, runID, WorkflowCommandRepair,
		int64(generation), string(intentDigest))
	if err != nil {
		return WorkflowCommandIntent{}, fmt.Errorf("locate worker projection intent: %w", err)
	}
	defer rows.Close()
	commandIDs := make([]string, 0, 2)
	for rows.Next() {
		var commandID string
		if err := rows.Scan(&commandID); err != nil {
			return WorkflowCommandIntent{}, fmt.Errorf("scan worker projection intent: %w", err)
		}
		commandIDs = append(commandIDs, commandID)
	}
	if err := rows.Err(); err != nil {
		return WorkflowCommandIntent{}, fmt.Errorf("iterate worker projection intents: %w", err)
	}
	if len(commandIDs) == 0 {
		return WorkflowCommandIntent{}, ErrNotFound
	}
	if len(commandIDs) != 1 {
		return WorkflowCommandIntent{}, ErrImmutableConflict
	}
	intent, err := loadWorkflowCommandIntent(ctx, tx, runID, commandIDs[0])
	if err != nil {
		return WorkflowCommandIntent{}, err
	}
	if intent.Kind != WorkflowCommandRepair || intent.Plan.Generation != generation ||
		intent.Digest != intentDigest {
		return WorkflowCommandIntent{}, ErrImmutableConflict
	}
	return intent, nil
}

type WorkerProjectionReceipt struct {
	ProjectionKey string
	Result        contract.WorkflowProjectionResult
	ProjectedAt   time.Time
}

func LoadWorkerProjectionReceipt(
	ctx context.Context,
	tx *Transaction,
	runID string,
	generation uint64,
	projectionKey string,
) (WorkerProjectionReceipt, bool, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return WorkerProjectionReceipt{}, false, err
	}
	if !validStoreID(runID) || !validStoreID(projectionKey) || generation == 0 || generation > maxStoreFence {
		return WorkerProjectionReceipt{}, false, errors.New("worker projection receipt identity is invalid")
	}
	var receipt WorkerProjectionReceipt
	var revision int64
	var state string
	err = tx.queryRow(ctx, `
		SELECT run_revision, run_state, projected_at
		FROM pm.workflow_projection_receipts
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
		  AND generation = $4 AND projection_key = $5
	`, scope.OrgID, scope.WorkspaceID, runID, int64(generation), projectionKey).Scan(
		&revision, &state, &receipt.ProjectedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkerProjectionReceipt{}, false, nil
	}
	if err != nil {
		return WorkerProjectionReceipt{}, false, fmt.Errorf("load worker projection receipt: %w", err)
	}
	if revision <= 0 {
		return WorkerProjectionReceipt{}, false, ErrImmutableConflict
	}
	receipt.ProjectionKey = projectionKey
	receipt.Result = contract.WorkflowProjectionResult{
		RunID: runID, Revision: uint64(revision), State: contract.RunState(state),
	}
	receipt.ProjectedAt = receipt.ProjectedAt.UTC()
	if err := receipt.Result.Validate(); err != nil {
		return WorkerProjectionReceipt{}, false, fmt.Errorf("validate worker projection receipt: %w", err)
	}
	return receipt, true, nil
}

func InsertWorkerProjectionReceipt(
	ctx context.Context,
	tx *Transaction,
	runID string,
	generation uint64,
	projectionKey string,
	result contract.WorkflowProjectionResult,
) (WorkerProjectionReceipt, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return WorkerProjectionReceipt{}, err
	}
	if !validStoreID(runID) || !validStoreID(projectionKey) || generation == 0 || generation > maxStoreFence {
		return WorkerProjectionReceipt{}, errors.New("worker projection receipt identity is invalid")
	}
	if err := result.Validate(); err != nil {
		return WorkerProjectionReceipt{}, err
	}
	if result.RunID != runID {
		return WorkerProjectionReceipt{}, ErrScopeMismatch
	}
	var projectedAt time.Time
	if err := tx.queryRow(ctx, `
		INSERT INTO pm.workflow_projection_receipts (
			org_id, workspace_id, run_id, generation, projection_key,
			run_revision, run_state, projected_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, clock_timestamp())
		RETURNING projected_at
	`, scope.OrgID, scope.WorkspaceID, runID, int64(generation), projectionKey,
		int64(result.Revision), string(result.State)).Scan(&projectedAt); err != nil {
		return WorkerProjectionReceipt{}, safeMetadataWriteError("insert worker projection receipt", err)
	}
	return WorkerProjectionReceipt{ProjectionKey: projectionKey, Result: result, ProjectedAt: projectedAt.UTC()}, nil
}

func WorkerReceiptExists(ctx context.Context, tx *Transaction, runID, receiptID, kind string) (bool, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return false, err
	}
	if !validStoreID(runID) || !validStoreID(receiptID) || !validStoreName(kind) {
		return false, errors.New("worker receipt identity is invalid")
	}
	var exists bool
	if err := tx.queryRow(ctx, `
		SELECT true
		FROM pm.receipts
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
		  AND receipt_id = $4 AND kind = $5
	`, scope.OrgID, scope.WorkspaceID, runID, receiptID, kind).Scan(&exists); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("load worker receipt: %w", err)
	}
	return exists, nil
}

// WorkerSettlementSummary projects the canonical single-dispatch effect state
// used by CommitSettlement without reimplementing its outcome precedence in the
// application layer.
func WorkerSettlementSummary(
	dispatch contract.Dispatch,
	outcomes []contract.MemberOutcome,
) (contract.EffectSummary, error) {
	if err := contract.ValidateSettlement(dispatch, outcomes); err != nil {
		return contract.EffectSummary{}, err
	}
	var summary contract.EffectSummary
	switch aggregateOutcomeState(outcomes) {
	case contract.OutcomeUnsent:
		summary.Unsent = 1
	case contract.OutcomeSucceeded:
		summary.Succeeded = 1
	case contract.OutcomeUnchanged:
		summary.Unchanged = 1
	case contract.OutcomeRejected:
		summary.Rejected = 1
	case contract.OutcomeUnknown:
		summary.Unknown = 1
	default:
		return contract.EffectSummary{}, ErrImmutableConflict
	}
	return summary, nil
}
