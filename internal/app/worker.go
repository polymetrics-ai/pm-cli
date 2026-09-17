package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

var (
	ErrWorkerInvalidRequest        = errors.New("worker request is invalid")
	ErrWorkerRouteMismatch         = errors.New("worker route does not match durable authority")
	ErrWorkerCapabilityUnavailable = errors.New("worker capability is unavailable")
	ErrWorkerAuthorization         = errors.New("worker authorization is unavailable")
	ErrWorkerConflict              = errors.New("worker durable state conflicts")
)

const (
	workerAttemptReceiptKind    = "worker_attempt"
	workerSettlementReceiptKind = "worker_settlement"
)

// WorkerFailure retains an internal cause for classification without exposing
// it through the safe error text serialized by the Temporal adapter.
type WorkerFailure struct {
	Kind  error
	Cause error
}

func (failure *WorkerFailure) Error() string {
	if failure == nil || failure.Kind == nil {
		return "worker failure"
	}
	return failure.Kind.Error()
}

func (failure *WorkerFailure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.Cause
}

func (failure *WorkerFailure) Is(target error) bool {
	return failure != nil && (target == failure.Kind || errors.Is(failure.Cause, target))
}

func workerFailure(kind, cause error) error {
	return &WorkerFailure{Kind: kind, Cause: cause}
}

type ScheduledRunAdmitter func(
	context.Context,
	contract.WorkflowExecution,
	contract.ScheduledRunInput,
) (contract.StartRunInput, error)

type WorkerServiceConfig struct {
	Pool                 *store.Pool
	DeploymentID         string
	Registry             *HandlerRegistry
	ResolveOperation     OperationResolver
	Authorization        auth.ResourceAuthorization
	ObserveContinuation  contract.WorkflowContinuationObserver
	ActivityTimeouts     contract.ActivityTimeouts
	ScheduledRunAdmitter ScheduledRunAdmitter
}

// WorkerService is the authenticated application boundary behind Temporal
// activities. Workflow payload tenant fields are integrity checks only; the
// database route derives the accepted scope and actor before RLS is installed.
type WorkerService struct {
	pool                *store.Pool
	deploymentID        string
	registry            *HandlerRegistry
	resolveOperation    OperationResolver
	authorization       auth.ResourceAuthorization
	observeContinuation contract.WorkflowContinuationObserver
	timeouts            contract.ActivityTimeouts
	scheduledAdmitter   ScheduledRunAdmitter
}

func NewWorkerService(configuration WorkerServiceConfig) (*WorkerService, error) {
	if configuration.Pool == nil {
		return nil, errors.New("worker store pool is required")
	}
	if !validApplicationID(configuration.DeploymentID) {
		return nil, errors.New("worker deployment identity is invalid")
	}
	if configuration.Registry == nil || configuration.ResolveOperation == nil ||
		configuration.Authorization == nil || configuration.ObserveContinuation == nil {
		return nil, errors.New("worker registry, operation resolver, resource authorization, and continuation observer are required")
	}
	if err := configuration.ActivityTimeouts.Validate(); err != nil {
		return nil, fmt.Errorf("validate worker activity timeouts: %w", err)
	}
	return &WorkerService{
		pool:                configuration.Pool,
		deploymentID:        configuration.DeploymentID,
		registry:            configuration.Registry,
		resolveOperation:    configuration.ResolveOperation,
		authorization:       configuration.Authorization,
		observeContinuation: configuration.ObserveContinuation,
		timeouts:            configuration.ActivityTimeouts,
		scheduledAdmitter:   configuration.ScheduledRunAdmitter,
	}, nil
}

func (service *WorkerService) SupportsScheduledAdmission() bool {
	return service != nil && service.scheduledAdmitter != nil
}

func (service *WorkerService) AdmitScheduledRun(
	ctx context.Context,
	execution contract.WorkflowExecution,
	input contract.ScheduledRunInput,
) (contract.StartRunInput, error) {
	if service == nil || ctx == nil {
		return contract.StartRunInput{}, workerFailure(ErrWorkerInvalidRequest, nil)
	}
	if err := execution.Validate(); err != nil {
		return contract.StartRunInput{}, workerFailure(ErrWorkerInvalidRequest, err)
	}
	if err := input.Validate(); err != nil {
		return contract.StartRunInput{}, workerFailure(ErrWorkerInvalidRequest, err)
	}
	if service.scheduledAdmitter == nil {
		return contract.StartRunInput{}, workerFailure(ErrWorkerCapabilityUnavailable, nil)
	}
	admitted, err := service.scheduledAdmitter(ctx, execution, input)
	if err != nil {
		return contract.StartRunInput{}, err
	}
	if err := admitted.ValidateScheduledStart(); err != nil {
		return contract.StartRunInput{}, workerFailure(ErrWorkerConflict, err)
	}
	if !admitted.Scope.Equal(input.Scope) || admitted.WorkflowID != execution.WorkflowID {
		return contract.StartRunInput{}, workerFailure(ErrWorkerRouteMismatch, nil)
	}
	return admitted, nil
}

type workerRunState struct {
	route      store.WorkerRoute
	intent     store.WorkflowCommandIntent
	record     contract.RunRecord
	input      store.RunInputSnapshot
	budget     store.RunBudgetSnapshot
	grant      store.RunExecutionGrant
	acceptedAt time.Time
	now        time.Time
	deadline   time.Time
}

func (service *WorkerService) LoadWorkflow(
	ctx context.Context,
	execution contract.WorkflowExecution,
	reference contract.WorkflowRunRef,
) (contract.WorkflowRunSnapshot, error) {
	if service == nil || ctx == nil {
		return contract.WorkflowRunSnapshot{}, workerFailure(ErrWorkerInvalidRequest, nil)
	}
	if err := execution.Validate(); err != nil {
		return contract.WorkflowRunSnapshot{}, workerFailure(ErrWorkerInvalidRequest, err)
	}
	if err := reference.Validate(); err != nil {
		return contract.WorkflowRunSnapshot{}, workerFailure(ErrWorkerInvalidRequest, err)
	}

	var snapshot contract.WorkflowRunSnapshot
	err := service.withinWorkerRun(ctx, execution, reference, func(
		ctx context.Context,
		tx *store.Transaction,
		state workerRunState,
	) error {
		timeouts, err := store.SealWorkerActivityTimeouts(
			ctx, tx, reference.RunID, reference.StartCommandID, reference.Generation, service.timeouts,
		)
		if err != nil {
			return err
		}
		tasks, err := store.ListWorkerTasks(ctx, tx, reference.RunID)
		if err != nil {
			return err
		}
		if state.record.Spec.OperationOrPlanRef.Kind == contract.OperationReference {
			tasks, err = service.ensureOperationTask(ctx, tx, state, tasks)
			if err != nil {
				return err
			}
		} else if len(tasks) == 0 {
			return workerFailure(ErrWorkerCapabilityUnavailable, errors.New("accepted plan has no owned durable tasks"))
		}

		commands, err := store.ListWorkerCommands(ctx, tx, reference.RunID)
		if err != nil {
			return err
		}
		units := make([]contract.WorkflowUnitRef, 0, len(tasks))
		allCoordinated := true
		for _, task := range tasks {
			if task.Lane != state.input.Lane {
				return workerFailure(ErrWorkerConflict, errors.New("task lane differs from the immutable admission lane"))
			}
			coordinated, err := workerTaskCoordinationComplete(ctx, tx, state.route.Scope, task)
			if err != nil {
				return err
			}
			if !coordinated {
				allCoordinated = false
				if workerRunDispatchable(state.record.State) &&
					workerRunDispatchable(task.State) && !task.CancelRequested {
					units = append(units, contract.WorkflowUnitRef{TaskID: task.TaskID})
				}
			}
		}
		done := workerTerminalState(state.record.State) || allCoordinated
		if done {
			units = []contract.WorkflowUnitRef{}
		}
		snapshot = contract.WorkflowRunSnapshot{
			Run:             reference,
			Deadline:        state.deadline,
			Timeouts:        timeouts,
			Units:           units,
			Commands:        commands,
			Done:            done,
			CancelRequested: state.record.CancelRequested,
			Fanout:          uint32(state.budget.Policy.WorkflowFanout),
		}
		return snapshot.Validate()
	})
	if err != nil {
		return contract.WorkflowRunSnapshot{}, service.mapWorkerError(err)
	}
	return snapshot, nil
}

func (service *WorkerService) ensureOperationTask(
	ctx context.Context,
	tx *store.Transaction,
	state workerRunState,
	tasks []contract.Task,
) ([]contract.Task, error) {
	reference := state.record.Spec.OperationOrPlanRef
	behaviorID, err := workerRecordID("behavior-v1-", struct {
		Schema    string        `json:"schema"`
		Operation string        `json:"operation"`
		Revision  string        `json:"revision"`
		Lane      contract.Lane `json:"lane"`
	}{
		Schema: "pm.operation-invocation-behavior/v1", Operation: reference.ID,
		Revision: reference.Revision, Lane: state.input.Lane,
	})
	if err != nil {
		return nil, err
	}
	taskID, err := workerRecordID("task-v1-", struct {
		Schema     string         `json:"schema"`
		Scope      contract.Scope `json:"scope"`
		RunID      string         `json:"run_id"`
		BehaviorID string         `json:"behavior_id"`
	}{
		Schema: "pm.operation-invocation-task/v1", Scope: state.route.Scope,
		RunID: state.record.Spec.RunID, BehaviorID: behaviorID,
	})
	if err != nil {
		return nil, err
	}
	initial := contract.Task{
		TaskID:                 taskID,
		RunID:                  state.record.Spec.RunID,
		OrgID:                  state.route.Scope.OrgID,
		WorkspaceID:            state.route.Scope.WorkspaceID,
		OperationID:            reference.ID,
		Lane:                   state.input.Lane,
		BehaviorID:             behaviorID,
		InputRefs:              slices.Clone(state.record.Spec.InputRefs),
		SchemaRefs:             slices.Clone(state.record.Spec.SchemaRefs),
		State:                  contract.RunStateAccepted,
		EffectSummary:          contract.EffectSummary{},
		UnknownEffectCount:     0,
		CancelRequested:        false,
		OrchestrationCondition: contract.OrchestrationPending,
		ProjectionObservedAt:   state.acceptedAt,
		Revision:               1,
	}
	if err := initial.Validate(); err != nil {
		return nil, err
	}
	if len(tasks) == 0 {
		if _, _, err := service.resolveHandler(ctx, state, reference.ID, reference.Revision, state.input.Lane); err != nil {
			return nil, err
		}
		created, err := store.EnsureWorkerTask(ctx, tx, initial)
		if err != nil {
			return nil, err
		}
		return []contract.Task{created}, nil
	}
	if len(tasks) != 1 || !sameWorkerTaskIdentity(tasks[0], initial) {
		return nil, workerFailure(ErrWorkerConflict, errors.New("operation task identity conflicts with immutable admission"))
	}
	return tasks, nil
}

func workerTaskCoordinationComplete(
	ctx context.Context,
	tx *store.Transaction,
	scope contract.Scope,
	task contract.Task,
) (bool, error) {
	taskRef := store.TaskRef{RunID: task.RunID, TaskID: task.TaskID}
	dispatch, err := store.LoadWorkerDispatch(ctx, tx, taskRef)
	if errors.Is(err, store.ErrNotFound) {
		if workerTaskRequiresSettlement(task.State) {
			return false, workerFailure(ErrWorkerConflict, errors.New("completed task lacks its durable dispatch"))
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	outcomes, err := store.ListMemberOutcomes(ctx, tx, taskRef, dispatch.Intent.DispatchID)
	if err != nil {
		return false, err
	}
	if len(outcomes) == 0 {
		if workerTaskRequiresSettlement(task.State) {
			return false, workerFailure(ErrWorkerConflict, errors.New("completed task lacks durable member outcomes"))
		}
		return false, nil
	}
	if err := contract.ValidateSettlement(dispatch.Intent, outcomes); err != nil {
		return false, workerFailure(ErrWorkerConflict, err)
	}
	settlementRef, err := workerSettlementRef(scope, task, dispatch.Intent)
	if err != nil {
		return false, err
	}
	exists, err := store.WorkerReceiptExists(
		ctx, tx, task.RunID, settlementRef, workerSettlementReceiptKind,
	)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, workerFailure(ErrWorkerConflict, errors.New("settled outcomes lack their durable worker receipt"))
	}
	return true, nil
}

func sameWorkerTaskIdentity(stored, expected contract.Task) bool {
	return stored.TaskID == expected.TaskID && stored.RunID == expected.RunID &&
		stored.OrgID == expected.OrgID && stored.WorkspaceID == expected.WorkspaceID &&
		stored.OperationID == expected.OperationID && stored.Lane == expected.Lane &&
		stored.BehaviorID == expected.BehaviorID &&
		slices.Equal(stored.InputRefs, expected.InputRefs) &&
		slices.Equal(stored.SchemaRefs, expected.SchemaRefs)
}

type workerUnitPlan struct {
	state     workerRunState
	timeouts  contract.ActivityTimeouts
	task      contract.Task
	dispatch  store.DispatchRecord
	lease     store.Lease
	permit    store.AccountPermit
	handler   ResolvedHandler
	request   ExecutionRequest
	reconcile bool
	existing  *contract.WorkflowUnitResult
}

func (service *WorkerService) ExecuteUnit(
	ctx context.Context,
	execution contract.WorkflowExecution,
	request contract.WorkflowUnitRequest,
	heartbeat func(contract.WorkflowProgress),
) (contract.WorkflowUnitResult, error) {
	if service == nil || ctx == nil {
		return contract.WorkflowUnitResult{}, workerFailure(ErrWorkerInvalidRequest, nil)
	}
	if err := execution.Validate(); err != nil {
		return contract.WorkflowUnitResult{}, workerFailure(ErrWorkerInvalidRequest, err)
	}
	if err := request.Validate(); err != nil {
		return contract.WorkflowUnitResult{}, workerFailure(ErrWorkerInvalidRequest, err)
	}

	plan, err := service.prepareWorkerUnit(ctx, execution, request)
	if err != nil {
		return contract.WorkflowUnitResult{}, service.mapWorkerError(err)
	}
	if plan.existing != nil {
		return *plan.existing, nil
	}
	progress := contract.WorkflowProgress{
		RunID: request.Run.RunID, TaskID: request.Unit.TaskID, ProgressRef: plan.request.Attempt.ReceiptRef,
	}
	if err := progress.Validate(); err != nil {
		return contract.WorkflowUnitResult{}, workerFailure(ErrWorkerConflict, err)
	}
	stopHeartbeats := startWorkerHeartbeats(ctx, heartbeat, progress, plan.timeouts.Heartbeat)
	defer stopHeartbeats()

	var callbackCtx context.Context
	var cancel context.CancelFunc
	if plan.reconcile {
		callbackCtx, cancel = context.WithTimeout(ctx, plan.timeouts.Transport)
	} else {
		callbackCtx, cancel = workerCallbackContext(
			ctx, plan.state.deadline, plan.timeouts.Transport,
		)
	}
	var observation ExecutionObservation
	var callbackErr error
	if plan.reconcile {
		observation, callbackErr = plan.handler.Reconcile(callbackCtx, plan.request)
	} else {
		observation, callbackErr = plan.handler.Execute(callbackCtx, plan.request)
	}
	cancel()

	settlementDeadline := plan.state.deadline
	if plan.reconcile {
		settlementDeadline = time.Time{}
	}
	settleCtx, settleCancel := workerSettlementContext(
		ctx, settlementDeadline, plan.timeouts.Heartbeat, plan.timeouts.Transport,
	)
	defer settleCancel()
	result, err := service.settleWorkerUnit(settleCtx, execution, request, plan, observation, callbackErr)
	stopHeartbeats()
	if err != nil {
		return contract.WorkflowUnitResult{}, service.mapWorkerError(err)
	}
	if heartbeat != nil {
		settledProgress := contract.WorkflowProgress{
			RunID: request.Run.RunID, TaskID: request.Unit.TaskID, ProgressRef: result.ResultRef,
		}
		if settledProgress.Validate() == nil {
			heartbeat(settledProgress)
		}
	}
	return result, nil
}

func (service *WorkerService) prepareWorkerUnit(
	ctx context.Context,
	execution contract.WorkflowExecution,
	request contract.WorkflowUnitRequest,
) (workerUnitPlan, error) {
	var plan workerUnitPlan
	err := service.withinWorkerRun(ctx, execution, request.Run, func(
		ctx context.Context,
		tx *store.Transaction,
		state workerRunState,
	) error {
		plan.state = state
		timeouts, err := store.LoadWorkerActivityTimeouts(ctx, tx, request.Run.RunID)
		if errors.Is(err, store.ErrNotFound) {
			return workerFailure(ErrWorkerConflict, errors.New("worker activity policy is not sealed"))
		}
		if err != nil {
			return err
		}
		plan.timeouts = timeouts
		taskRef := store.TaskRef{RunID: request.Run.RunID, TaskID: request.Unit.TaskID}
		task, err := store.LoadTask(ctx, tx, taskRef)
		if err != nil {
			return err
		}
		if task.Lane != state.input.Lane {
			return workerFailure(ErrWorkerConflict, errors.New("task lane differs from immutable admission"))
		}
		dispatch, err := store.LoadWorkerDispatch(ctx, tx, taskRef)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return workerFailure(ErrWorkerCapabilityUnavailable, errors.New("sealed execution-plan dispatch is unavailable"))
			}
			return err
		}
		expectedRevision := dispatch.Intent.OperationRevision
		if state.record.Spec.OperationOrPlanRef.Kind == contract.OperationReference {
			if task.OperationID != state.record.Spec.OperationOrPlanRef.ID {
				return workerFailure(ErrWorkerConflict, errors.New("operation task differs from accepted operation"))
			}
			expectedRevision = state.record.Spec.OperationOrPlanRef.Revision
		}
		if dispatch.Intent.OperationRevision != expectedRevision || dispatch.Intent.GrantEpoch != state.grant.Grant.GrantEpoch {
			return workerFailure(ErrWorkerConflict, errors.New("dispatch revision or grant epoch differs from accepted authority"))
		}

		settlementRef, err := workerSettlementRef(state.route.Scope, task, dispatch.Intent)
		if err != nil {
			return err
		}
		outcomes, err := store.ListMemberOutcomes(ctx, tx, taskRef, dispatch.Intent.DispatchID)
		if err != nil {
			return err
		}
		if len(outcomes) != 0 {
			if err := contract.ValidateSettlement(dispatch.Intent, outcomes); err != nil {
				return workerFailure(ErrWorkerConflict, err)
			}
			exists, err := store.WorkerReceiptExists(ctx, tx, task.RunID, settlementRef, workerSettlementReceiptKind)
			if err != nil {
				return err
			}
			if !exists {
				return workerFailure(ErrWorkerConflict, errors.New("settled outcomes lack their durable worker receipt"))
			}
			result := contract.WorkflowUnitResult{
				TaskID: task.TaskID, State: task.State,
				UnknownEffectCount: task.UnknownEffectCount, ResultRef: settlementRef,
			}
			if err := result.Validate(); err != nil {
				return workerFailure(ErrWorkerConflict, err)
			}
			plan.existing = &result
			return nil
		}
		if workerTerminalState(state.record.State) || workerTerminalState(task.State) {
			return workerFailure(ErrWorkerConflict, errors.New("unsettled dispatch belongs to terminal durable state"))
		}
		freshSend := false
		_, handler, err := service.resolveHandler(ctx, state, task.OperationID, expectedRevision, task.Lane)
		if err != nil {
			return err
		}
		connection, err := store.LoadConnection(ctx, tx, state.input.ConnectionID)
		if err != nil {
			return err
		}
		account, err := store.LoadProviderAccount(ctx, tx, connection.ProviderAccountID)
		if err != nil {
			return err
		}
		if dispatch.Intent.AccountWitness != account.AccountWitness {
			return workerFailure(ErrWorkerConflict, errors.New("dispatch provider-account witness is stale"))
		}
		switch {
		case dispatch.State == store.DispatchPrepared && len(dispatch.Attempts) == 0:
			freshSend = true
			if state.record.State == contract.RunStateRunning {
				locked, err := store.LoadWorkerRunForUpdate(ctx, tx, task.RunID)
				if err != nil {
					return err
				}
				state.record = locked
				plan.state = state
			}
			if state.record.CancelRequested || !workerRunDispatchable(state.record.State) ||
				task.CancelRequested || !workerRunDispatchable(task.State) {
				result := contract.WorkflowUnitResult{
					TaskID: task.TaskID, State: task.State,
					UnknownEffectCount: task.UnknownEffectCount, ResultRef: task.TaskID,
				}
				if err := result.Validate(); err != nil {
					return workerFailure(ErrWorkerConflict, err)
				}
				plan.existing = &result
				return nil
			}
			if !state.deadline.After(state.now) {
				return workerFailure(ErrWorkerAuthorization, errors.New("accepted run lifetime has expired"))
			}
			if state.record.State != contract.RunStateRunning {
				started, err := store.StartWorkerRun(ctx, tx, state.record, state.now)
				if err != nil {
					return err
				}
				state.record = started
				plan.state = state
			}
		case dispatch.State == store.DispatchPossiblySent && len(dispatch.Attempts) == 1:
			// A durable attempt authorizes reconciliation-only observation even
			// after dispatch authority is revoked, cancelled, or expired.
		default:
			return workerFailure(ErrWorkerConflict, errors.New("dispatch state and attempt evidence are inconsistent"))
		}

		lease, err := claimWorkerTask(
			ctx, tx, state, execution, taskRef, timeouts.StartToClose, !freshSend,
		)
		if err != nil {
			return err
		}
		if freshSend {
			task, err = store.StartWorkerTask(ctx, tx, lease, task, state.now)
			if err != nil {
				return err
			}
		}
		permitID, err := workerRecordID("permit-v1-", struct {
			Scope      contract.Scope      `json:"scope"`
			RunID      string              `json:"run_id"`
			TaskID     string              `json:"task_id"`
			DispatchID contract.DispatchID `json:"dispatch_id"`
		}{state.route.Scope, task.RunID, task.TaskID, dispatch.Intent.DispatchID})
		if err != nil {
			return err
		}
		permitOwner, err := workerRecordID("permit-owner-v1-", struct {
			RunID      string              `json:"run_id"`
			TaskID     string              `json:"task_id"`
			DispatchID contract.DispatchID `json:"dispatch_id"`
		}{task.RunID, task.TaskID, dispatch.Intent.DispatchID})
		if err != nil {
			return err
		}
		permitTTL := timeouts.StartToClose
		if freshSend {
			permitTTL = minWorkerDuration(permitTTL, state.deadline.Sub(state.now))
			if permitTTL < time.Microsecond {
				return workerFailure(ErrWorkerAuthorization, errors.New("no accepted lifetime remains for a provider permit"))
			}
		}
		var permit store.AccountPermit
		if freshSend {
			permit, err = AcquirePermit(ctx, tx, store.AccountPermitRequest{
				PermitID: permitID, ConnectionID: state.input.ConnectionID,
				OwnerID: permitOwner, Units: 1, TTL: permitTTL,
			}, connection.ProviderAccountID)
			if err != nil {
				return err
			}
		} else {
			permit, err = store.LoadAccountPermit(ctx, tx, permitID)
			if errors.Is(err, store.ErrNotFound) {
				return workerFailure(ErrWorkerConflict, errors.New("possibly dispatched work lacks its account permit"))
			}
			if err != nil {
				return err
			}
			if permit.ConnectionID != state.input.ConnectionID ||
				permit.ProviderAccountID != connection.ProviderAccountID ||
				permit.OwnerID != permitOwner || permit.Units != 1 {
				return workerFailure(ErrWorkerConflict, errors.New("worker account permit identity conflicts"))
			}
			switch permit.State {
			case store.AccountPermitDispatched, store.AccountPermitUnknown:
				// A recorded attempt may only enter reconciliation; permit
				// expiry cannot erase or re-authorize its possible effect.
			case store.AccountPermitReserved:
				return workerFailure(ErrWorkerConflict, errors.New("unused account permit conflicts with dispatch evidence"))
			case store.AccountPermitResolved:
				return workerFailure(ErrWorkerConflict, errors.New("resolved account permit lacks durable settlement"))
			case store.AccountPermitExpired:
				return workerFailure(ErrWorkerConflict, errors.New("expired account permit cannot authorize the sealed dispatch"))
			default:
				return workerFailure(ErrWorkerConflict, errors.New("account permit state is unsupported"))
			}
		}
		if freshSend {
			if err := service.requireCurrentWorkerAuthorization(ctx, tx, state, task.OperationID); err != nil {
				return err
			}
		}

		attemptRef, err := workerAttemptRef(state.route.Scope, task, dispatch.Intent)
		if err != nil {
			return err
		}
		var attempt contract.Attempt
		switch len(dispatch.Attempts) {
		case 0:
			if !freshSend {
				return workerFailure(ErrWorkerConflict, errors.New("reconciliation lacks its durable physical attempt"))
			}
			attempt = contract.Attempt{
				AttemptID: dispatch.Intent.AttemptID, DispatchID: dispatch.Intent.DispatchID,
				ReceiptRef: attemptRef, RecordedAt: permit.ReservedAt,
			}
			if err := attempt.Validate(); err != nil {
				return err
			}
			if _, err := store.RecordDispatchAttempt(ctx, tx, workerAttemptCommit(taskRef, task, dispatch.Intent, permit, attempt)); err != nil {
				return err
			}
		case 1:
			if freshSend {
				return workerFailure(ErrWorkerConflict, errors.New("new dispatch unexpectedly has a durable physical attempt"))
			}
			attempt = dispatch.Attempts[0]
			if attempt.AttemptID != dispatch.Intent.AttemptID || attempt.DispatchID != dispatch.Intent.DispatchID ||
				attempt.ReceiptRef != attemptRef {
				return workerFailure(ErrWorkerConflict, errors.New("durable dispatch attempt conflicts with sealed identity"))
			}
		default:
			return workerFailure(ErrWorkerConflict, errors.New("sealed worker dispatch has multiple physical attempts"))
		}
		plan.task = task
		plan.dispatch = dispatch
		plan.lease = lease
		plan.permit = permit
		plan.handler = handler
		plan.reconcile = !freshSend
		plan.request = ExecutionRequest{
			Scope: state.route.Scope, ConnectionID: state.input.ConnectionID, PermitRef: permit.PermitID,
			ReconciliationOnly: !freshSend,
			Run:                state.record.Spec, Task: task, Dispatch: dispatch.Intent, Attempt: attempt,
			Input: state.input.Input, Controls: state.input.ResultControls, Deadline: state.deadline,
		}
		return nil
	})
	if err != nil {
		return workerUnitPlan{}, err
	}
	return plan, nil
}

func claimWorkerTask(
	ctx context.Context,
	tx *store.Transaction,
	state workerRunState,
	execution contract.WorkflowExecution,
	ref store.TaskRef,
	maxTTL time.Duration,
	reconciliationOnly bool,
) (store.Lease, error) {
	ownerID, err := workerRecordID("task-owner-v1-", struct {
		RunID         string `json:"run_id"`
		TaskID        string `json:"task_id"`
		TemporalRunID string `json:"temporal_run_id"`
	}{ref.RunID, ref.TaskID, execution.RunID})
	if err != nil {
		return store.Lease{}, err
	}
	expectedFence := uint64(0)
	current, err := store.LoadLease(ctx, tx, ref)
	if err == nil {
		if current.ExpiresAt.After(state.now) {
			return store.Lease{}, store.ErrClaimLost
		}
		expectedFence = current.Fence
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.Lease{}, err
	}
	ttl := maxTTL
	if !reconciliationOnly {
		ttl = minWorkerDuration(state.deadline.Sub(state.now), ttl)
	}
	if ttl <= 0 {
		return store.Lease{}, workerFailure(ErrWorkerAuthorization, errors.New("worker lease authority has elapsed"))
	}
	return store.ClaimTask(ctx, tx, store.ClaimRequest{
		Task: ref, OwnerID: ownerID, ExpectedFence: expectedFence, TTL: ttl,
	})
}

func workerAttemptCommit(
	taskRef store.TaskRef,
	task contract.Task,
	dispatch contract.Dispatch,
	permit store.AccountPermit,
	attempt contract.Attempt,
) store.DispatchAttemptCommit {
	payload := contract.SafePayload{
		References: []contract.SafeReference{
			{Name: "run", Ref: task.RunID},
			{Name: "task", Ref: task.TaskID},
			{Name: "dispatch", Ref: string(dispatch.DispatchID)},
			{Name: "attempt", Ref: string(attempt.AttemptID)},
			{Name: "permit", Ref: permit.PermitID},
		},
		Counts:      []contract.SafeCount{{Name: "task_revision", Value: task.Revision}},
		ReasonCodes: []string{"physical_attempt_authorized"},
	}
	base := strings.TrimPrefix(attempt.ReceiptRef, "worker-attempt-v1-")
	return store.DispatchAttemptCommit{
		Task:    taskRef,
		Attempt: attempt,
		Receipt: store.Receipt{
			ReceiptID: attempt.ReceiptRef, Kind: workerAttemptReceiptKind, RunID: task.RunID, Payload: payload,
		},
		Audit: store.AuditEntry{
			AuditID: "worker-attempt-audit-v1-" + base,
			Action:  "worker_attempt_authorized", TargetRef: task.RunID, Payload: payload,
		},
		Event: store.EventInput{
			EventID: "worker-attempt-event-v1-" + base,
			Type:    "worker_attempt_authorized", Payload: payload,
		},
		Outbox: store.OutboxCommand{
			CommandID: "worker-attempt-outbox-v1-" + base,
			Kind:      "worker_attempt_recorded", PayloadRef: task.RunID,
		},
	}
}

func (service *WorkerService) settleWorkerUnit(
	ctx context.Context,
	execution contract.WorkflowExecution,
	request contract.WorkflowUnitRequest,
	plan workerUnitPlan,
	observation ExecutionObservation,
	callbackErr error,
) (contract.WorkflowUnitResult, error) {
	var result contract.WorkflowUnitResult
	err := service.withinWorkerRun(ctx, execution, request.Run, func(
		ctx context.Context,
		tx *store.Transaction,
		state workerRunState,
	) error {
		taskRef := store.TaskRef{RunID: request.Run.RunID, TaskID: request.Unit.TaskID}
		task, err := store.LoadTask(ctx, tx, taskRef)
		if err != nil {
			return err
		}
		dispatch, err := store.LoadWorkerDispatch(ctx, tx, taskRef)
		if err != nil {
			return err
		}
		settlementRef, err := workerSettlementRef(state.route.Scope, task, dispatch.Intent)
		if err != nil {
			return err
		}
		storedOutcomes, err := store.ListMemberOutcomes(ctx, tx, taskRef, dispatch.Intent.DispatchID)
		if err != nil {
			return err
		}
		if len(storedOutcomes) != 0 {
			if err := contract.ValidateSettlement(dispatch.Intent, storedOutcomes); err != nil {
				return workerFailure(ErrWorkerConflict, err)
			}
			exists, err := store.WorkerReceiptExists(ctx, tx, task.RunID, settlementRef, workerSettlementReceiptKind)
			if err != nil {
				return err
			}
			if !exists {
				return workerFailure(ErrWorkerConflict, errors.New("settled outcomes lack their durable worker receipt"))
			}
			result = contract.WorkflowUnitResult{
				TaskID: task.TaskID, State: task.State,
				UnknownEffectCount: task.UnknownEffectCount, ResultRef: settlementRef,
			}
			return result.Validate()
		}
		if dispatch.State != store.DispatchPossiblySent || len(dispatch.Attempts) != 1 ||
			dispatch.Attempts[0].ReceiptRef != plan.request.Attempt.ReceiptRef {
			return workerFailure(ErrWorkerConflict, errors.New("durable attempt witness is incomplete"))
		}
		now, err := store.ReadAuthenticationTime(ctx, tx)
		if err != nil {
			return err
		}
		// Result publication, callback completion, and provider-effect truth are
		// separate durable concerns. A complete valid member settlement remains
		// authoritative even when result publication is unavailable or the
		// callback also reports an error. Only incomplete/invalid member truth
		// degrades the effect to unknown.
		outcomes := observation.Outcomes
		if contract.ValidateSettlement(dispatch.Intent, outcomes) != nil {
			outcomes = unknownWorkerOutcomes(dispatch.Intent, now, plan.request.Attempt.ReceiptRef)
		}
		summary, err := store.WorkerSettlementSummary(dispatch.Intent, outcomes)
		if err != nil {
			return workerFailure(ErrWorkerConflict, err)
		}
		projectedAt := now
		for _, outcome := range outcomes {
			if outcome.ObservedAt.After(projectedAt) {
				projectedAt = outcome.ObservedAt.UTC()
			}
		}
		updatedTask, err := task.ApplyEffectSummary(summary, false, "", projectedAt)
		if err != nil {
			return workerFailure(ErrWorkerConflict, err)
		}
		permit, err := store.LoadAccountPermit(ctx, tx, plan.permit.PermitID)
		if err != nil {
			return err
		}
		if updatedTask.State == contract.RunStateReconciling {
			updatedTask.OrchestrationCondition = contract.OrchestrationUnknown
		} else {
			updatedTask.OrchestrationCondition = contract.OrchestrationCompleted
		}
		if err := updatedTask.Validate(); err != nil {
			return workerFailure(ErrWorkerConflict, err)
		}
		if summary.Unknown != 0 {
			if permit.State != store.AccountPermitDispatched && permit.State != store.AccountPermitUnknown {
				return workerFailure(ErrWorkerConflict, errors.New("unknown settlement has incompatible account permit state"))
			}
		} else if permit.State != store.AccountPermitDispatched && permit.State != store.AccountPermitUnknown {
			return workerFailure(ErrWorkerConflict, errors.New("known settlement has incompatible account permit state"))
		}
		commit := workerSettlementCommit(
			plan.lease,
			updatedTask,
			dispatch.Intent,
			outcomes,
			settlementRef,
			observation.Result != nil,
			callbackErr != nil,
		)
		if _, err := store.CommitSettlement(ctx, tx, commit); err != nil {
			return err
		}
		if summary.Unknown != 0 {
			if permit.State == store.AccountPermitDispatched {
				if _, err := store.MarkAccountPermitUnknown(ctx, tx, permit); err != nil {
					return err
				}
			}
		} else {
			if _, err := store.ResolveAccountPermit(ctx, tx, permit); err != nil {
				return err
			}
		}
		result = contract.WorkflowUnitResult{
			TaskID: updatedTask.TaskID, State: updatedTask.State,
			UnknownEffectCount: updatedTask.UnknownEffectCount, ResultRef: settlementRef,
		}
		return result.Validate()
	})
	if err != nil {
		return contract.WorkflowUnitResult{}, err
	}
	return result, nil
}

func workerSettlementCommit(
	lease store.Lease,
	task contract.Task,
	dispatch contract.Dispatch,
	outcomes []contract.MemberOutcome,
	settlementRef string,
	resultPublicationUnavailable bool,
	callbackFailed bool,
) store.SettlementCommit {
	reasonCodes := []string{"member_settlement"}
	if resultPublicationUnavailable {
		reasonCodes = append(reasonCodes, "result_publication_unavailable")
	}
	if callbackFailed {
		reasonCodes = append(reasonCodes, "callback_error_observed")
	}
	payload := contract.SafePayload{
		References: []contract.SafeReference{
			{Name: "run", Ref: task.RunID},
			{Name: "task", Ref: task.TaskID},
			{Name: "dispatch", Ref: string(dispatch.DispatchID)},
			{Name: "settlement", Ref: settlementRef},
		},
		Counts: []contract.SafeCount{
			{Name: "members", Value: uint64(len(outcomes))},
			{Name: "unknown", Value: task.UnknownEffectCount},
		},
		ReasonCodes: reasonCodes,
	}
	base := strings.TrimPrefix(settlementRef, "worker-settlement-v1-")
	return store.SettlementCommit{
		Lease: lease, DispatchID: dispatch.DispatchID, Outcomes: outcomes, Task: task,
		Receipt: store.Receipt{
			ReceiptID: settlementRef, Kind: workerSettlementReceiptKind, RunID: task.RunID, Payload: payload,
		},
		Audit: store.AuditEntry{
			AuditID: "worker-settlement-audit-v1-" + base,
			Action:  "worker_member_settlement", TargetRef: task.RunID, Payload: payload,
		},
		Event: store.EventInput{
			EventID: "worker-settlement-event-v1-" + base,
			Type:    "worker_member_settlement", Payload: payload,
		},
		Outbox: store.OutboxCommand{
			CommandID: "worker-settlement-outbox-v1-" + base,
			Kind:      "worker_settlement_recorded", PayloadRef: task.RunID,
		},
	}
}

func unknownWorkerOutcomes(
	dispatch contract.Dispatch,
	observedAt time.Time,
	attemptRef string,
) []contract.MemberOutcome {
	outcomes := make([]contract.MemberOutcome, 0, len(dispatch.OrderedMemberIDs))
	for _, memberID := range dispatch.OrderedMemberIDs {
		outcomes = append(outcomes, contract.MemberOutcome{
			MemberID: memberID, EffectKey: dispatch.EffectKey, DispatchID: dispatch.DispatchID,
			State: contract.OutcomeUnknown, ObservedAt: observedAt.UTC(),
			AssertionRefs: []string{attemptRef},
		})
	}
	return outcomes
}

func (service *WorkerService) ApplyCommand(
	ctx context.Context,
	execution contract.WorkflowExecution,
	request contract.WorkflowCommandRequest,
) (contract.WorkflowCommandResult, error) {
	if service == nil || ctx == nil {
		return contract.WorkflowCommandResult{}, workerFailure(ErrWorkerInvalidRequest, nil)
	}
	if err := execution.Validate(); err != nil {
		return contract.WorkflowCommandResult{}, workerFailure(ErrWorkerInvalidRequest, err)
	}
	if err := request.Validate(); err != nil {
		return contract.WorkflowCommandResult{}, workerFailure(ErrWorkerInvalidRequest, err)
	}
	var result contract.WorkflowCommandResult
	err := service.withinWorkerRun(ctx, execution, request.Run, func(
		ctx context.Context,
		tx *store.Transaction,
		state workerRunState,
	) error {
		receipt, err := store.ApplyWorkerCommand(
			ctx, tx, request.Run.RunID, request.Run.Generation, request.Command.CommandID,
		)
		if err != nil {
			return err
		}
		result = receipt.Result
		return result.Validate()
	})
	if err != nil {
		return contract.WorkflowCommandResult{}, service.mapWorkerError(err)
	}
	return result, nil
}

func (service *WorkerService) FinalizeRunProjection(
	ctx context.Context,
	execution contract.WorkflowExecution,
	request contract.WorkflowProjectionRequest,
) (contract.WorkflowProjectionResult, error) {
	if service == nil || ctx == nil {
		return contract.WorkflowProjectionResult{}, workerFailure(ErrWorkerInvalidRequest, nil)
	}
	if err := execution.Validate(); err != nil {
		return contract.WorkflowProjectionResult{}, workerFailure(ErrWorkerInvalidRequest, err)
	}
	if err := request.Validate(); err != nil {
		return contract.WorkflowProjectionResult{}, workerFailure(ErrWorkerInvalidRequest, err)
	}
	projectionKey := workerProjectionKey(request)
	var result contract.WorkflowProjectionResult
	err := service.withinWorkerRun(ctx, execution, request.Run, func(
		ctx context.Context,
		tx *store.Transaction,
		state workerRunState,
	) error {
		current, err := store.LoadWorkerRunForUpdate(ctx, tx, request.Run.RunID)
		if err != nil {
			return err
		}
		projectedAt, err := store.ReadAuthenticationTime(ctx, tx)
		if err != nil {
			return err
		}
		if projectedAt.Before(current.ProjectionObservedAt) {
			projectedAt = current.ProjectionObservedAt
		}
		if request.RepairIntentDigest != "" {
			repairIntent, err := store.LoadWorkerProjectionIntent(
				ctx, tx, request.Run.RunID, request.Run.Generation, request.RepairIntentDigest,
			)
			if errors.Is(err, store.ErrNotFound) {
				return workerFailure(ErrWorkerAuthorization, err)
			}
			if err != nil {
				return err
			}
			if !repairIntent.Scope.Equal(state.route.Scope) ||
				repairIntent.RunID != request.Run.RunID ||
				repairIntent.Plan.Namespace != execution.Namespace ||
				repairIntent.Plan.TaskQueue != contract.ControlTaskQueue ||
				(repairIntent.Plan.WorkflowType != contract.RunWorkflowType &&
					repairIntent.Plan.WorkflowType != contract.ScheduledWorkflowType) ||
				repairIntent.Plan.WorkflowID != execution.WorkflowID ||
				repairIntent.Plan.Generation != request.Run.Generation ||
				repairIntent.Plan.Origin != current.WorkflowBinding.Origin ||
				repairIntent.Plan.BudgetDigest != state.budget.Digest {
				return workerFailure(ErrWorkerConflict, errors.New("repair projection intent conflicts with durable run authority"))
			}
		}
		if receipt, found, err := store.LoadWorkerProjectionReceipt(
			ctx, tx, request.Run.RunID, request.Run.Generation, projectionKey,
		); err != nil {
			return err
		} else if found {
			result = receipt.Result
			return nil
		}
		if workerTerminalState(current.State) {
			result = contract.WorkflowProjectionResult{
				RunID: current.Spec.RunID, Revision: current.Revision, State: current.State,
			}
			_, err := store.InsertWorkerProjectionReceipt(
				ctx, tx, current.Spec.RunID, request.Run.Generation, projectionKey, result,
			)
			return err
		}
		effects, err := store.ListWorkerRunEffects(ctx, tx, current.Spec.RunID)
		if err != nil {
			return err
		}
		var updated contract.RunRecord
		switch {
		case current.CancelRequested && (len(effects) == 0 || workerEffectsKnownUnsent(effects)):
			updated, err = projectKnownUnsentWorkerCancellation(current, effects, projectedAt)
		case len(effects) == 0:
			return workerFailure(ErrWorkerCapabilityUnavailable, errors.New("run has no durable effect ledger to project"))
		default:
			updated, err = current.ReconcileEffects(effects, false, "", projectedAt)
			if err == nil && current.CancelRequested &&
				!workerTerminalState(updated.State) && updated.State != contract.RunStateReconciling {
				return workerFailure(ErrWorkerConflict, errors.New("cancellation cannot hide unsettled effects"))
			}
		}
		if err != nil {
			return workerFailure(ErrWorkerConflict, err)
		}
		commit, err := workerProjectionCommit(updated, projectionKey)
		if err != nil {
			return err
		}
		if _, err := store.CommitRunTransition(ctx, tx, commit); err != nil {
			return err
		}
		result = contract.WorkflowProjectionResult{
			RunID: updated.Spec.RunID, Revision: updated.Revision, State: updated.State,
		}
		if err := result.Validate(); err != nil {
			return err
		}
		_, err = store.InsertWorkerProjectionReceipt(
			ctx, tx, updated.Spec.RunID, request.Run.Generation, projectionKey, result,
		)
		return err
	})
	if err != nil {
		return contract.WorkflowProjectionResult{}, service.mapWorkerError(err)
	}
	return result, nil
}

func workerProjectionCommit(run contract.RunRecord, projectionKey string) (store.RunTransitionCommit, error) {
	base := strings.TrimPrefix(projectionKey, "final-")
	base = strings.TrimPrefix(base, "repair-")
	payload := contract.SafePayload{
		References: []contract.SafeReference{
			{Name: "run", Ref: run.Spec.RunID},
			{Name: "projection", Ref: projectionKey},
		},
		Counts: []contract.SafeCount{
			{Name: "run_revision", Value: run.Revision},
			{Name: "unknown", Value: run.UnknownEffectCount},
		},
		ReasonCodes: []string{"worker_projection"},
	}
	return store.RunTransitionCommit{
		Run: run,
		Receipt: store.Receipt{
			ReceiptID: "worker-projection-receipt-v1-" + base,
			Kind:      "worker_projection", RunID: run.Spec.RunID, Payload: payload,
		},
		Audit: store.AuditEntry{
			AuditID: "worker-projection-audit-v1-" + base,
			Action:  "worker_run_projected", TargetRef: run.Spec.RunID, Payload: payload,
		},
		Event: store.EventInput{
			EventID: "worker-projection-event-v1-" + base,
			Type:    "worker_run_projected", Payload: payload,
		},
		Outbox: store.OutboxCommand{
			CommandID: "worker-projection-outbox-v1-" + base,
			Kind:      "worker_projection_recorded", PayloadRef: run.Spec.RunID,
		},
	}, nil
}

func (service *WorkerService) withinWorkerRun(
	ctx context.Context,
	execution contract.WorkflowExecution,
	reference contract.WorkflowRunRef,
	fn func(context.Context, *store.Transaction, workerRunState) error,
) error {
	descriptor := store.WorkerRouteDescriptor{
		RunID: reference.RunID, StartCommandID: reference.StartCommandID,
		Namespace: execution.Namespace, WorkflowID: execution.WorkflowID,
		TemporalRunID: execution.RunID, Generation: reference.Generation,
		IntentDigest: reference.IntentDigest,
	}
	runScoped := func() error {
		return store.WithinWorkerScope(ctx, service.pool, descriptor, func(
			ctx context.Context,
			tx *store.Transaction,
			route store.WorkerRoute,
		) error {
			if !route.Scope.Equal(reference.Scope) {
				return workerFailure(ErrWorkerRouteMismatch, store.ErrScopeMismatch)
			}
			if err := store.BindAuthDeployment(ctx, tx, service.deploymentID); err != nil {
				return err
			}
			state, err := loadWorkerRunState(ctx, tx, route, execution, reference)
			if err != nil {
				return err
			}
			return fn(ctx, tx, state)
		})
	}
	err := runScoped()
	if !errors.Is(err, store.ErrWorkerRouteUnknown) {
		return err
	}

	// The observer performs network I/O and MUST remain outside the PostgreSQL
	// transaction. Only its verified private evidence can request a bounded
	// append; tenant payload fields never select the route.
	observation, observeErr := service.observeContinuation(ctx, execution)
	if observeErr != nil {
		return fmt.Errorf("observe workflow continuation: %w", observeErr)
	}
	if err := store.ObserveWorkerContinuation(ctx, service.pool, descriptor, observation); err != nil {
		return err
	}
	return runScoped()
}

func loadWorkerRunState(
	ctx context.Context,
	tx *store.Transaction,
	route store.WorkerRoute,
	execution contract.WorkflowExecution,
	reference contract.WorkflowRunRef,
) (workerRunState, error) {
	intent, err := store.LoadWorkerCommandIntent(ctx, tx, reference.RunID, reference.StartCommandID)
	if err != nil {
		return workerRunState{}, err
	}
	if !intent.Scope.Equal(route.Scope) || intent.RunID != reference.RunID ||
		intent.CommandID != reference.StartCommandID || intent.Digest != reference.IntentDigest ||
		(intent.Kind != store.WorkflowCommandStartRun && intent.Kind != store.WorkflowCommandEnsureBinding) ||
		intent.Plan.Namespace != execution.Namespace || intent.Plan.WorkflowID != execution.WorkflowID ||
		intent.Plan.WorkflowID != reference.WorkflowID || intent.Plan.Generation != reference.Generation ||
		(intent.Plan.WorkflowType != contract.RunWorkflowType &&
			intent.Plan.WorkflowType != contract.ScheduledWorkflowType) ||
		intent.Plan.TaskQueue != contract.ControlTaskQueue {
		return workerRunState{}, workerFailure(ErrWorkerRouteMismatch, store.ErrImmutableConflict)
	}
	record, err := store.LoadRun(ctx, tx, reference.RunID)
	if err != nil {
		return workerRunState{}, err
	}
	binding := record.WorkflowBinding
	if record.Spec.RunID != reference.RunID || record.Spec.OrgID != route.Scope.OrgID ||
		record.Spec.WorkspaceID != route.Scope.WorkspaceID || !record.Spec.Actor.Equal(route.Actor) ||
		binding.RunID != reference.RunID || binding.WorkflowID != reference.WorkflowID ||
		binding.Generation != reference.Generation || binding.Origin != intent.Plan.Origin {
		return workerRunState{}, workerFailure(ErrWorkerRouteMismatch, store.ErrImmutableConflict)
	}
	input, err := store.LoadRunInputSnapshot(ctx, tx, reference.RunID)
	if err != nil {
		return workerRunState{}, err
	}
	budget, err := store.LoadRunBudgetSnapshot(ctx, tx, reference.RunID)
	if err != nil {
		return workerRunState{}, err
	}
	grant, err := store.LoadRunExecutionGrant(ctx, tx, reference.RunID)
	if err != nil {
		return workerRunState{}, err
	}
	acceptedAt, err := store.LoadWorkerAcceptedAt(ctx, tx, reference.RunID)
	if err != nil {
		return workerRunState{}, err
	}
	now, err := store.ReadAuthenticationTime(ctx, tx)
	if err != nil {
		return workerRunState{}, err
	}
	if budget.Binding() != record.Spec.Budget || budget.Digest != intent.Plan.BudgetDigest ||
		intent.Plan.ExecutionTimeout != budget.Policy.RunLifetime ||
		grant.Grant.GrantRef != record.Spec.ExecutionGrantRef || !grant.Grant.Scope.Equal(route.Scope) ||
		!grant.Grant.Actor.Equal(route.Actor) || input.RunID != reference.RunID || !input.Scope.Equal(route.Scope) {
		return workerRunState{}, workerFailure(ErrWorkerConflict, store.ErrImmutableConflict)
	}
	deadline := acceptedAt.Add(budget.Policy.RunLifetime)
	if !deadline.After(acceptedAt) {
		return workerRunState{}, workerFailure(ErrWorkerConflict, errors.New("accepted run lifetime overflows its durable deadline"))
	}
	return workerRunState{
		route: route, intent: intent, record: record, input: input, budget: budget,
		grant: grant, acceptedAt: acceptedAt, now: now.UTC(), deadline: deadline.UTC(),
	}, nil
}

func (service *WorkerService) resolveHandler(
	ctx context.Context,
	state workerRunState,
	operationID, expectedRevision string,
	lane contract.Lane,
) (OperationSelection, ResolvedHandler, error) {
	selection, err := service.resolveOperation(ctx, state.route.Scope, state.input.ConnectionID, operationID)
	if err != nil {
		return OperationSelection{}, ResolvedHandler{}, workerFailure(ErrWorkerCapabilityUnavailable, err)
	}
	selection, err = normalizeOperationSelection(selection)
	if err != nil || selection.Operation.OperationID != operationID {
		return OperationSelection{}, ResolvedHandler{}, workerFailure(ErrWorkerCapabilityUnavailable, err)
	}
	revision, err := OperationSelectionRevision(selection)
	if err != nil {
		return OperationSelection{}, ResolvedHandler{}, workerFailure(ErrWorkerCapabilityUnavailable, err)
	}
	if string(revision) != expectedRevision {
		return OperationSelection{}, ResolvedHandler{}, workerFailure(ErrWorkerConflict, errors.New("operation selection revision differs from durable execution authority"))
	}
	handler, err := service.registry.Resolve(selection.Operation, lane)
	if err != nil {
		return OperationSelection{}, ResolvedHandler{}, workerFailure(ErrWorkerCapabilityUnavailable, err)
	}
	return selection, handler, nil
}

func (service *WorkerService) requireCurrentWorkerAuthorization(
	ctx context.Context,
	tx *store.Transaction,
	state workerRunState,
	operationID string,
) error {
	grant, err := RecheckRunGrant(ctx, tx, service.authorization, RunGrantCheck{
		RunID:        state.record.Spec.RunID,
		Permission:   "operation.invoke",
		ResourceKind: auth.ResourceRun,
		ResourceID:   state.record.Spec.RunID,
		ConnectionID: state.input.ConnectionID,
		OperationID:  operationID,
	})
	if err != nil {
		return workerFailure(ErrWorkerAuthorization, err)
	}
	if !grant.Scope.Equal(state.route.Scope) || !grant.Actor.Equal(state.route.Actor) {
		return workerFailure(ErrWorkerAuthorization, auth.ErrGrantUnavailable)
	}
	return nil
}

func (service *WorkerService) mapWorkerError(err error) error {
	if err == nil {
		return nil
	}
	var failure *WorkerFailure
	if errors.As(err, &failure) {
		return failure
	}
	switch {
	case errors.Is(err, store.ErrWorkflowBindingConflict):
		return workerFailure(ErrWorkerRouteMismatch, err)
	case errors.Is(err, store.ErrNotFound):
		return workerFailure(ErrWorkerRouteMismatch, err)
	case errors.Is(err, store.ErrImmutableConflict):
		return workerFailure(ErrWorkerConflict, err)
	case errors.Is(err, store.ErrCASConflict), errors.Is(err, store.ErrClaimLost),
		errors.Is(err, store.ErrStaleFence), errors.Is(err, store.ErrAccountCapacity),
		errors.Is(err, store.ErrAccountPermitStale),
		errors.Is(err, store.ErrCommitOutcomeUnknown):
		return err
	default:
		return err
	}
}

func workerAttemptRef(scope contract.Scope, task contract.Task, dispatch contract.Dispatch) (string, error) {
	return workerRecordID("worker-attempt-v1-", struct {
		Scope      contract.Scope      `json:"scope"`
		RunID      string              `json:"run_id"`
		TaskID     string              `json:"task_id"`
		DispatchID contract.DispatchID `json:"dispatch_id"`
		AttemptID  contract.AttemptID  `json:"attempt_id"`
	}{scope, task.RunID, task.TaskID, dispatch.DispatchID, dispatch.AttemptID})
}

func workerSettlementRef(scope contract.Scope, task contract.Task, dispatch contract.Dispatch) (string, error) {
	return workerRecordID("worker-settlement-v1-", struct {
		Scope      contract.Scope      `json:"scope"`
		RunID      string              `json:"run_id"`
		TaskID     string              `json:"task_id"`
		DispatchID contract.DispatchID `json:"dispatch_id"`
	}{scope, task.RunID, task.TaskID, dispatch.DispatchID})
}

func workerRecordID(prefix string, value any) (string, error) {
	digest, err := canonicalIntent(value)
	if err != nil {
		return "", err
	}
	return prefix + strings.TrimPrefix(string(digest), "sha256:"), nil
}

func workerProjectionKey(request contract.WorkflowProjectionRequest) string {
	if request.RepairIntentDigest != "" {
		return "repair-" + strings.TrimPrefix(string(request.RepairIntentDigest), "sha256:")
	}
	return "final-" + strings.TrimPrefix(string(request.Run.IntentDigest), "sha256:")
}

func startWorkerHeartbeats(
	ctx context.Context,
	heartbeat func(contract.WorkflowProgress),
	progress contract.WorkflowProgress,
	heartbeatTimeout time.Duration,
) func() {
	if heartbeat == nil {
		return func() {}
	}
	heartbeat(progress)
	interval := heartbeatTimeout / 2
	if interval <= 0 {
		interval = time.Nanosecond
	}
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				heartbeat(progress)
			case <-ctx.Done():
				return
			case <-stop:
				return
			}
		}

	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			<-stopped
		})
	}
}

func workerEffectsKnownUnsent(effects []contract.EffectRecord) bool {
	if len(effects) == 0 {
		return false
	}
	for _, effect := range effects {
		if effect.State != contract.OutcomeUnsent || len(effect.Attempts) != 0 {
			return false
		}
	}
	return true
}

func projectKnownUnsentWorkerCancellation(
	current contract.RunRecord,
	effects []contract.EffectRecord,
	observedAt time.Time,
) (contract.RunRecord, error) {
	if !current.CancelRequested ||
		len(effects) != 0 && !workerEffectsKnownUnsent(effects) ||
		len(effects) == 0 && current.EffectSummary.Total() != 0 {
		return contract.RunRecord{}, errors.New("run is not eligible for known-unsent cancellation")
	}
	aggregate, err := current.ReconcileEffects(effects, false, "", observedAt)
	if err != nil {
		return contract.RunRecord{}, err
	}
	if aggregate.EffectSummary.Unsent != uint64(len(effects)) ||
		aggregate.EffectSummary.Total() != aggregate.EffectSummary.Unsent {
		return contract.RunRecord{}, errors.New("cancelled run contains an effect that is not known unsent")
	}
	cancelled, err := current.Transition(contract.RunStateCancelled, "cancel_requested", observedAt)
	if err != nil {
		return contract.RunRecord{}, err
	}
	cancelled.EffectSummary = aggregate.EffectSummary
	cancelled.UnknownEffectCount = 0
	cancelled.OrchestrationCondition = contract.OrchestrationCancelled
	cancelled.ProjectionObservedAt = observedAt
	if err := cancelled.Validate(); err != nil {
		return contract.RunRecord{}, err
	}
	return cancelled, nil
}

func workerTaskRequiresSettlement(state contract.RunState) bool {
	return state == contract.RunStateReconciling || workerTerminalState(state)
}

func workerRunDispatchable(state contract.RunState) bool {
	switch state {
	case contract.RunStateAccepted, contract.RunStateQueued, contract.RunStateRunning:
		return true
	default:
		return false
	}
}

func workerTerminalState(state contract.RunState) bool {
	switch state {
	case contract.RunStateSucceeded, contract.RunStatePartial, contract.RunStateFailed, contract.RunStateCancelled:
		return true
	default:
		return false
	}
}

func minWorkerDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

func workerCallbackContext(
	ctx context.Context,
	acceptedDeadline time.Time,
	transport time.Duration,
) (context.Context, context.CancelFunc) {
	deadline := time.Now().Add(transport)
	if acceptedDeadline.Before(deadline) {
		deadline = acceptedDeadline
	}
	return context.WithDeadline(ctx, deadline)
}

func workerSettlementContext(
	ctx context.Context,
	acceptedDeadline time.Time,
	heartbeatTimeout time.Duration,
	transportTimeout time.Duration,
) (context.Context, context.CancelFunc) {
	now := time.Now()
	deadline := now.Add(transportTimeout)
	if !acceptedDeadline.IsZero() && acceptedDeadline.Before(deadline) {
		deadline = acceptedDeadline
	}
	if activityDeadline, ok := ctx.Deadline(); ok {
		reserve := minWorkerDuration(heartbeatTimeout/2, transportTimeout/2)
		if reserve <= 0 {
			reserve = time.Nanosecond
		}
		activitySettlementDeadline := activityDeadline.Add(-reserve)
		if activitySettlementDeadline.Before(deadline) {
			deadline = activitySettlementDeadline
		}
	}
	return context.WithDeadline(context.WithoutCancel(ctx), deadline)
}
