package testfixture

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// NewWorkerService binds the production worker application boundary to the
// fixture's actual scoped PostgreSQL pool and deployment identity.
func NewWorkerService(
	t testing.TB,
	fixture *Fixture,
	registry *app.HandlerRegistry,
	resolver app.OperationResolver,
	authorization auth.ResourceAuthorization,
	observer contract.WorkflowContinuationObserver,
	timeouts contract.ActivityTimeouts,
) *app.WorkerService {
	t.Helper()
	if fixture == nil || fixture.Pool == nil {
		t.Fatal("authenticated PostgreSQL fixture is required")
	}
	if registry == nil {
		registry = &app.HandlerRegistry{}
	}
	if authorization == nil {
		authorization = auth.NewResourceAuthorizer()
	}
	service, err := app.NewWorkerService(app.WorkerServiceConfig{
		Pool:                fixture.Pool,
		DeploymentID:        fixture.DeploymentID,
		Registry:            registry,
		ResolveOperation:    resolver,
		Authorization:       authorization,
		ObserveContinuation: observer,
		ActivityTimeouts:    timeouts,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// SealedWorkerPlan is the exact fixture-owned provider dispatch supplied by an
// admitted controlled plan. Production worker code may consume but never
// derive this effect/member/request witness.
type SealedWorkerPlan struct {
	TaskID   string
	Dispatch contract.Dispatch
}

// SealWorkerPlan first exercises the production load boundary to materialize
// its deterministic operation task, then supplies the otherwise external
// planner-owned dispatch for controlled physical worker tests.
func SealWorkerPlan(
	t testing.TB,
	fixture *Fixture,
	service *app.WorkerService,
	execution contract.WorkflowExecution,
	reference contract.WorkflowRunRef,
) SealedWorkerPlan {
	t.Helper()
	if fixture == nil || fixture.Pool == nil || service == nil {
		t.Fatal("sealed worker plan requires an open fixture and production worker service")
	}
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	snapshot, err := service.LoadWorkflow(ctx, execution, reference)
	if err != nil {
		t.Fatalf("materialize controlled operation task: %v", err)
	}
	if !snapshot.Done && len(snapshot.Units) == 1 {
		// The production planner already derived this dispatch from the durable
		// admission facts, so the fixture returns exactly that plan instead of
		// sealing a second one.
		var planned SealedWorkerPlan
		unit := snapshot.Units[0]
		err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(
			ctx context.Context,
			tx *store.Transaction,
		) error {
			dispatch, err := store.LoadDispatch(
				ctx, tx, store.TaskRef{RunID: reference.RunID, TaskID: unit.TaskID}, unit.DispatchID,
			)
			if err != nil {
				return err
			}
			planned = SealedWorkerPlan{TaskID: unit.TaskID, Dispatch: dispatch.Intent}
			return nil
		})
		if err != nil {
			t.Fatalf("load the production planned dispatch: %v", err)
		}
		return planned
	}
	if snapshot.Done || len(snapshot.Units) != 0 {
		t.Fatalf("unplanned controlled snapshot = %#v; want pending work without a dispatchable unit", snapshot)
	}

	var plan SealedWorkerPlan
	var fixtureLease store.Lease
	err = store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(
		ctx context.Context,
		tx *store.Transaction,
	) error {
		tasks, err := store.ListWorkerTasks(ctx, tx, reference.RunID)
		if err != nil {
			return err
		}
		if len(tasks) != 1 {
			return fmt.Errorf("materialized %d controlled tasks; want one", len(tasks))
		}
		task := tasks[0]
		taskRef := store.TaskRef{RunID: reference.RunID, TaskID: task.TaskID}
		run, err := store.LoadRun(ctx, tx, reference.RunID)
		if err != nil {
			return err
		}
		input, err := store.LoadRunInputSnapshot(ctx, tx, reference.RunID)
		if err != nil {
			return err
		}
		grant, err := store.LoadRunExecutionGrant(ctx, tx, reference.RunID)
		if err != nil {
			return err
		}
		connection, err := store.LoadConnection(ctx, tx, input.ConnectionID)
		if err != nil {
			return err
		}
		account, err := store.LoadProviderAccount(ctx, tx, connection.ProviderAccountID)
		if err != nil {
			return err
		}
		requestBytes, err := json.Marshal(input.Input)
		if err != nil {
			return fmt.Errorf("encode controlled request witness: %w", err)
		}
		entity, err := contract.NewEntityKey(
			account.AccountWitness,
			"operation",
			task.OperationID,
			task.BehaviorID,
		)
		if err != nil {
			return err
		}
		effect, err := contract.NewEffectKey(entity, task.TaskID, task.BehaviorID)
		if err != nil {
			return err
		}
		dispatchID, err := contract.NewDispatchID("dispatch-" + task.TaskID)
		if err != nil {
			return err
		}
		memberID, err := contract.NewMemberID("member-" + task.TaskID)
		if err != nil {
			return err
		}
		attemptID, err := contract.NewAttemptID("attempt-" + task.TaskID)
		if err != nil {
			return err
		}
		dispatch := contract.Dispatch{
			RunID:              reference.RunID,
			EffectKey:          effect,
			DispatchID:         dispatchID,
			OrderedMemberIDs:   []contract.MemberID{memberID},
			PayloadHash:        contract.NewPayloadHash(requestBytes),
			RequestArtifactRef: input.InputRef,
			OperationRevision:  run.Spec.OperationOrPlanRef.Revision,
			AccountWitness:     account.AccountWitness,
			GrantEpoch:         grant.Grant.GrantEpoch,
			CapacityReservation: contract.CapacityReservation{
				OutcomeSlots:  1,
				ReadbackSlots: 1,
				EncodedBytes:  uint64(len(requestBytes)) + 1024,
			},
			AttemptID: attemptID,
		}
		if err := dispatch.Validate(); err != nil {
			return err
		}
		fixtureLease, err = store.ClaimTask(ctx, tx, store.ClaimRequest{
			Task: taskRef, OwnerID: "fixture-plan-" + task.TaskID, TTL: time.Second,
		})
		if err != nil {
			return err
		}
		if err := store.PrepareDispatch(ctx, tx, fixtureLease, dispatch); err != nil {
			return err
		}
		plan = SealedWorkerPlan{TaskID: task.TaskID, Dispatch: dispatch}
		return nil
	})
	if err != nil {
		t.Fatalf("seal controlled worker plan: %v", err)
	}
	for {
		var now time.Time
		err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(
			ctx context.Context,
			tx *store.Transaction,
		) error {
			var err error
			now, err = store.ReadAuthenticationTime(ctx, tx)
			return err
		})
		if err != nil {
			t.Fatalf("observe controlled planning lease expiry: %v", err)
		}
		if !now.Before(fixtureLease.ExpiresAt) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("controlled planning lease did not expire")
		case <-time.After(time.Millisecond):
		}
	}
	snapshot, err = service.LoadWorkflow(ctx, execution, reference)
	if err != nil {
		t.Fatalf("load sealed controlled worker plan: %v", err)
	}
	wantUnit := contract.WorkflowUnitRef{TaskID: plan.TaskID, DispatchID: plan.Dispatch.DispatchID}
	if snapshot.Done || len(snapshot.Units) != 1 || snapshot.Units[0] != wantUnit {
		t.Fatalf("planned controlled units = %#v; want exact dispatch %#v", snapshot.Units, wantUnit)
	}
	return plan
}
