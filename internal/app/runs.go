package app

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"math"
	"slices"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

type RunServiceConfig struct {
	Pool             *store.Pool
	DeploymentID     string
	Authorize        auth.AuthorizationCallback
	Random           io.Reader
	ReceiptRetention time.Duration
}

// RunService owns scope-safe run observation and explicit run controls. It has
// no provider execution path and never treats an accepted execution grant as
// current view authority.
type RunService struct {
	pool             *store.Pool
	deploymentID     string
	authorize        auth.AuthorizationCallback
	random           io.Reader
	receiptRetention time.Duration
}

func NewRunService(configuration RunServiceConfig) (*RunService, error) {
	if configuration.Pool == nil {
		return nil, errors.New("run service store pool is required")
	}
	if configuration.Authorize == nil {
		return nil, errors.New("run service current authorization callback is required")
	}
	if !validApplicationID(configuration.DeploymentID) {
		return nil, errors.New("run service deployment identity is invalid")
	}
	if configuration.Random == nil {
		configuration.Random = rand.Reader
	}
	if configuration.ReceiptRetention < minimumMutationReceiptRetention ||
		configuration.ReceiptRetention > 365*24*time.Hour {
		return nil, errors.New("run mutation receipt retention must be between seven and 365 days")
	}
	return &RunService{
		pool:             configuration.Pool,
		deploymentID:     configuration.DeploymentID,
		authorize:        configuration.Authorize,
		random:           configuration.Random,
		receiptRetention: configuration.ReceiptRetention,
	}, nil
}

type RunView struct {
	Record        contract.RunRecord
	ConnectionIDs []string
	ResultRefs    []string
}

type TaskView struct {
	Record   contract.Task
	Outcomes []store.MemberOutcomeView
}

type RunPage struct {
	Items []RunView
	Next  string
}

type TaskPage struct {
	Items []TaskView
	Next  string
}

type RunEventPage struct {
	Run          RunView
	Watermark    uint64
	ReadThrough  uint64
	RetentionGap bool
	Events       []contract.ServerEvent
}

func (service *RunService) GetRun(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	runID string,
) (RunView, error) {
	if service == nil || ctx == nil || !validApplicationID(runID) {
		return RunView{}, invalidRequestFailure()
	}
	var result RunView
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		stored, err := store.LoadRunView(ctx, tx, runID)
		if err != nil {
			return mapRunStoreFailure(err)
		}
		if _, err := service.authorizeRunRecord(ctx, tx, actor, scope, stored, "run.read", stored.Record.Spec.OperationOrPlanRef.ID); err != nil {
			return err
		}
		result = runViewFromStore(stored)
		return nil
	})
	return result, err
}

// AuthorizeRunEvents establishes both authorities needed by a stream before
// cursor-signing material or durable event rows are accessed. Event collections
// are authorized against their parent run; ResourceEvent remains reserved for
// an exact event identity that is already known.
func (service *RunService) AuthorizeRunEvents(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	runID string,
) error {
	if service == nil || ctx == nil || !validApplicationID(runID) {
		return invalidRequestFailure()
	}
	return store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		stored, err := store.LoadRunView(ctx, tx, runID)
		if err != nil {
			return mapRunStoreFailure(err)
		}
		return service.authorizeRunEventsRecord(
			ctx,
			tx,
			actor,
			scope,
			stored,
			stored.Record.Spec.OperationOrPlanRef.ID,
		)
	})
}

// ReadRunEvents authorizes the current actor before loading any event row. The
// store then observes the run projection, durable watermark and bounded replay
// while holding the run row against concurrent sequence allocation.
func (service *RunService) ReadRunEvents(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	runID string,
	after uint64,
	through uint64,
	limit uint32,
) (RunEventPage, error) {
	if service == nil || ctx == nil || !validApplicationID(runID) ||
		after > math.MaxInt64 || through > math.MaxInt64 || through != 0 && after > through ||
		limit == 0 || limit > 1000 {
		return RunEventPage{}, invalidRequestFailure()
	}
	var result RunEventPage
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		candidate, err := store.LoadRunView(ctx, tx, runID)
		if err != nil {
			return mapRunStoreFailure(err)
		}
		if err := service.authorizeRunEventsRecord(
			ctx,
			tx,
			actor,
			scope,
			candidate,
			candidate.Record.Spec.OperationOrPlanRef.ID,
		); err != nil {
			return err
		}
		stored, err := store.LoadRunEventSnapshot(ctx, tx, runID, after, through, int(limit))
		if err != nil {
			return mapRunStoreFailure(err)
		}
		result = RunEventPage{
			Run:          runViewFromStore(stored.Run),
			Watermark:    stored.Watermark,
			ReadThrough:  stored.ReadThrough,
			RetentionGap: stored.RetentionGap,
			Events:       stored.Events,
		}
		return nil
	})
	return result, err
}

func (service *RunService) ListRuns(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	after string,
	state *contract.RunState,
	pageSize uint32,
) (RunPage, error) {
	if service == nil || ctx == nil || after != "" && !validApplicationID(after) ||
		pageSize == 0 || pageSize > 100 || state != nil && !validApplicationRunState(*state) {
		return RunPage{}, invalidRequestFailure()
	}
	result := RunPage{Items: []RunView{}}
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		authorization, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:           scope,
			Actor:           actor,
			Permission:      "run.read",
			ResourceKind:    auth.ResourceWorkspace,
			ResourceID:      scope.WorkspaceID,
			AutomationScope: actor.RequestedActor.Kind == contract.PrincipalServiceAccount,
		})
		if err != nil {
			return mapAuthorizationFailure(err)
		}
		stored, err := store.ListRunViews(
			ctx,
			tx,
			authorization.ConnectionIDs,
			authorization.OperationIDs,
			after,
			state,
			pageSize+1,
		)
		if err != nil {
			return mapRunStoreFailure(err)
		}
		result.Items = make([]RunView, 0, min(len(stored), int(pageSize)))
		for index, item := range stored {
			if index == int(pageSize) {
				result.Next = result.Items[len(result.Items)-1].Record.Spec.RunID
				break
			}
			result.Items = append(result.Items, runViewFromStore(item))
		}
		return nil
	})
	return result, err
}

func (service *RunService) GetRunResult(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	runID string,
) (RunView, error) {
	if service == nil || ctx == nil || !validApplicationID(runID) {
		return RunView{}, invalidRequestFailure()
	}
	var result RunView
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		stored, err := store.LoadRunView(ctx, tx, runID)
		if err != nil {
			return mapRunStoreFailure(err)
		}
		if _, err := service.authorizeRunRecord(ctx, tx, actor, scope, stored, "run.read", stored.Record.Spec.OperationOrPlanRef.ID); err != nil {
			return err
		}
		if len(stored.ResultRefs) == 0 {
			return &Failure{
				HTTPStatus:  503,
				Code:        "capability_unavailable",
				RetryAction: "after_capability_change",
			}
		}
		result = runViewFromStore(stored)
		return nil
	})
	return result, err
}

func (service *RunService) ListRunTasks(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	runID string,
	after string,
	pageSize uint32,
) (TaskPage, error) {
	if service == nil || ctx == nil || !validApplicationID(runID) ||
		after != "" && !validApplicationID(after) || pageSize == 0 || pageSize > 100 {
		return TaskPage{}, invalidRequestFailure()
	}
	result := TaskPage{Items: []TaskView{}}
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		run, err := store.LoadRunView(ctx, tx, runID)
		if err != nil {
			return mapRunStoreFailure(err)
		}
		authorization, err := service.authorizeRunRecord(
			ctx,
			tx,
			actor,
			scope,
			run,
			"run.read",
			run.Record.Spec.OperationOrPlanRef.ID,
		)
		if err != nil {
			return err
		}
		stored, err := store.ListTaskViews(ctx, tx, runID, authorization.OperationIDs, after, pageSize+1)
		if err != nil {
			return mapRunStoreFailure(err)
		}
		result.Items = make([]TaskView, 0, min(len(stored), int(pageSize)))
		for index, item := range stored {
			if index == int(pageSize) {
				result.Next = result.Items[len(result.Items)-1].Record.TaskID
				break
			}
			result.Items = append(result.Items, taskViewFromStore(item))
		}
		return nil
	})
	return result, err
}

func (service *RunService) GetTask(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	ref store.TaskRef,
) (TaskView, error) {
	if service == nil || ctx == nil || !validApplicationID(ref.RunID) || !validApplicationID(ref.TaskID) {
		return TaskView{}, invalidRequestFailure()
	}
	var result TaskView
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		stored, err := store.LoadTaskView(ctx, tx, ref)
		if err != nil {
			return mapRunStoreFailure(err)
		}
		run, err := store.LoadRunView(ctx, tx, ref.RunID)
		if err != nil {
			return mapRunStoreFailure(err)
		}
		if _, err := service.authorizeRunRecord(ctx, tx, actor, scope, run, "run.read", stored.Record.OperationID); err != nil {
			return err
		}
		result = taskViewFromStore(stored)
		return nil
	})
	return result, err
}

func (service *RunService) authorizeRunEventsRecord(
	ctx context.Context,
	tx *store.Transaction,
	actor contract.Actor,
	scope contract.Scope,
	run store.RunViewRecord,
	operationID string,
) error {
	if _, err := service.authorizeRunRecord(
		ctx,
		tx,
		actor,
		scope,
		run,
		"run.read",
		operationID,
	); err != nil {
		return err
	}
	_, err := service.authorizeRunRecord(
		ctx,
		tx,
		actor,
		scope,
		run,
		"event.read",
		operationID,
	)
	return err
}

func (service *RunService) authorizeRunRecord(
	ctx context.Context,
	tx *store.Transaction,
	actor contract.Actor,
	scope contract.Scope,
	run store.RunViewRecord,
	permission string,
	operationID string,
) (auth.Authorization, error) {
	if len(run.ConnectionIDs) == 0 || !validApplicationID(operationID) {
		return auth.Authorization{}, resourceNotFoundFailure(store.ErrNotFound)
	}
	var authorization auth.Authorization
	for _, connectionID := range run.ConnectionIDs {
		if !validApplicationID(connectionID) {
			return auth.Authorization{}, resourceNotFoundFailure(store.ErrNotFound)
		}
		current, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:           scope,
			Actor:           actor,
			Permission:      permission,
			ResourceKind:    auth.ResourceRun,
			ResourceID:      run.Record.Spec.RunID,
			ConnectionID:    connectionID,
			OperationID:     operationID,
			AutomationScope: actor.RequestedActor.Kind == contract.PrincipalServiceAccount,
		})
		if err != nil {
			return auth.Authorization{}, mapAuthorizationFailure(err)
		}
		authorization = current
	}
	return authorization, nil
}

func runViewFromStore(value store.RunViewRecord) RunView {
	return RunView{
		Record:        value.Record,
		ConnectionIDs: slices.Clone(value.ConnectionIDs),
		ResultRefs:    slices.Clone(value.ResultRefs),
	}
}

func taskViewFromStore(value store.TaskViewRecord) TaskView {
	return TaskView{
		Record:   value.Record,
		Outcomes: slices.Clone(value.Outcomes),
	}
}

func mapRunStoreFailure(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrScopeMismatch) {
		return resourceNotFoundFailure(err)
	}
	return err
}

func validApplicationRunState(state contract.RunState) bool {
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
