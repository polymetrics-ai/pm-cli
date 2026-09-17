package store

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestRunViewsUseAcceptedIdentityAndStoredOutcomeTruth(t *testing.T) {
	credentials, pool := prepareTestStore(t, 4)
	scope := newTestScope(t)
	foreignScope := newTestScope(t)
	t.Cleanup(func() {
		cleanupTestScope(t, credentials, scope)
		cleanupTestScope(t, credentials, foreignScope)
	})

	first := testAdmissionRequest(t, scope, "run-view-first", "run-view-first")
	second := testAdmissionRequest(t, scope, "run-view-second", "run-view-second")
	foreign := testAdmissionRequest(t, foreignScope, "run-view-foreign", "run-view-foreign")
	registerRunViewFixture(t, pool, scope, first)
	registerRunViewFixture(t, pool, scope, second)
	registerRunViewFixture(t, pool, foreignScope, foreign)

	observedAt := time.Date(2026, 9, 15, 11, 12, 13, 456789000, time.UTC)
	taskID := "shared-task"
	firstTask := testTask(first, taskID, observedAt.Add(-time.Minute))
	secondTask := testTask(second, taskID, observedAt.Add(-time.Minute))
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		if err := CreateTask(ctx, tx, firstTask); err != nil {
			return err
		}
		return CreateTask(ctx, tx, secondTask)
	}); err != nil {
		t.Fatal(err)
	}

	var lease Lease
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		lease, err = ClaimTask(ctx, tx, ClaimRequest{
			Task:    TaskRef{RunID: first.Spec.RunID, TaskID: taskID},
			OwnerID: "run-view-worker",
			TTL:     time.Minute,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	running, err := firstTask.ObserveOrchestration(contract.OrchestrationRunning, observedAt.Add(-30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := CommitTaskTransition(ctx, tx, TaskTransitionCommit{
			Lease:   lease,
			Task:    running,
			Receipt: testReceipt("run-view-running-receipt", "task_transition", first.Spec.RunID),
			Audit:   testAudit("run-view-running-audit", "task_running", first.Spec.RunID),
			Event:   testEvent("run-view-running-event", "task_running", first.Spec.RunID),
			Outbox:  testOutbox("run-view-running-command", "project_task", first.Spec.RunID),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	completed, err := running.ObserveOrchestration(contract.OrchestrationCompleted, observedAt.Add(-20*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := CommitTaskTransition(ctx, tx, TaskTransitionCommit{
			Lease:   lease,
			Task:    completed,
			Receipt: testReceipt("run-view-completed-receipt", "task_transition", first.Spec.RunID),
			Audit:   testAudit("run-view-completed-audit", "task_completed", first.Spec.RunID),
			Event:   testEvent("run-view-completed-event", "task_completed", first.Spec.RunID),
			Outbox:  testOutbox("run-view-completed-command", "project_task", first.Spec.RunID),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	dispatch := testDispatchFor(t, first.Spec.RunID, "run-view")
	secondMember, err := contract.NewMemberID("member-run-view-second")
	if err != nil {
		t.Fatal(err)
	}
	dispatch.OrderedMemberIDs = append(dispatch.OrderedMemberIDs, secondMember)
	dispatch.CapacityReservation.OutcomeSlots = 2
	if err := dispatch.Validate(); err != nil {
		t.Fatalf("construct two-member dispatch: %v", err)
	}
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return PrepareDispatch(ctx, tx, lease, dispatch)
	}); err != nil {
		t.Fatal(err)
	}
	outcomes := []contract.MemberOutcome{
		{
			MemberID:      dispatch.OrderedMemberIDs[0],
			EffectKey:     dispatch.EffectKey,
			DispatchID:    dispatch.DispatchID,
			State:         contract.OutcomeUnknown,
			ObservedAt:    observedAt,
			AssertionRefs: []string{"assertion-run-view-first"},
		},
		{
			MemberID:      dispatch.OrderedMemberIDs[1],
			EffectKey:     dispatch.EffectKey,
			DispatchID:    dispatch.DispatchID,
			State:         contract.OutcomeUnknown,
			ObservedAt:    observedAt.Add(time.Microsecond),
			AssertionRefs: []string{"assertion-run-view-second"},
		},
	}
	settledTask, err := completed.ApplyEffectSummary(contract.EffectSummary{Unknown: 1}, false, "", observedAt.Add(2*time.Microsecond))
	if err != nil {
		t.Fatal(err)
	}
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := CommitSettlement(ctx, tx, SettlementCommit{
			Lease:      lease,
			DispatchID: dispatch.DispatchID,
			Outcomes:   outcomes,
			Task:       settledTask,
			Receipt:    testReceipt("run-view-settlement-receipt", "settlement", first.Spec.RunID),
			Audit:      testAudit("run-view-settlement-audit", "dispatch_settled", first.Spec.RunID),
			Event:      testEvent("run-view-settlement-event", "dispatch_settled", first.Spec.RunID),
			Outbox:     testOutbox("run-view-settlement-command", "reconcile_dispatch", first.Spec.RunID),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		view, err := LoadRunView(ctx, tx, first.Spec.RunID)
		if err != nil {
			return err
		}
		if view.Record.Spec.RunID != first.Spec.RunID ||
			!reflect.DeepEqual(view.ConnectionIDs, []string{first.InputSnapshot.ConnectionID}) ||
			len(view.ResultRefs) != 0 {
			t.Fatalf("exact run view = %#v", view)
		}
		state := contract.RunStateAccepted
		allowedConnections := []string{first.InputSnapshot.ConnectionID, second.InputSnapshot.ConnectionID}
		allowedOperations := []string{first.Spec.OperationOrPlanRef.ID, second.Spec.OperationOrPlanRef.ID}
		slices.Sort(allowedConnections)
		slices.Sort(allowedOperations)

		page, err := ListRunViews(
			ctx,
			tx,
			allowedConnections,
			allowedOperations,
			"",
			&state,
			8,
		)
		if err != nil {
			return err
		}
		if len(page) != 2 {
			t.Fatalf("authorized accepted run page length = %d; want 2", len(page))
		}
		filtered, err := ListRunViews(ctx, tx, []string{"connection-not-authorized"}, []string{first.Spec.OperationOrPlanRef.ID}, "", nil, 8)
		if err != nil {
			return err
		}
		if len(filtered) != 0 {
			t.Fatalf("pre-exposure filtered run page = %#v; want empty", filtered)
		}

		firstTaskView, err := LoadTaskView(ctx, tx, TaskRef{RunID: first.Spec.RunID, TaskID: taskID})
		if err != nil {
			return err
		}
		secondTaskView, err := LoadTaskView(ctx, tx, TaskRef{RunID: second.Spec.RunID, TaskID: taskID})
		if err != nil {
			return err
		}
		if len(firstTaskView.Outcomes) != len(outcomes) || len(secondTaskView.Outcomes) != 0 {
			t.Fatalf("exact composite task outcomes first=%#v second=%#v", firstTaskView.Outcomes, secondTaskView.Outcomes)
		}
		for index, outcome := range outcomes {
			stored := firstTaskView.Outcomes[index]
			if !reflect.DeepEqual(stored.Outcome.EffectKey, outcome.EffectKey) ||
				stored.Outcome.MemberID != outcome.MemberID ||
				stored.Outcome.DispatchID != outcome.DispatchID ||
				!stored.Outcome.ObservedAt.Equal(outcome.ObservedAt) ||
				!reflect.DeepEqual(stored.Outcome.AssertionRefs, outcome.AssertionRefs) {
				t.Fatalf("stored outcome[%d] = %#v; want %#v", index, stored.Outcome, outcome)
			}
			var persistedEffectRef string
			if err := tx.queryRow(ctx, `
				SELECT effect_digest
				FROM pm.effects
				WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND task_id = $4
			`, scope.OrgID, scope.WorkspaceID, first.Spec.RunID, taskID).Scan(&persistedEffectRef); err != nil {
				return err
			}
			if stored.EffectRef != persistedEffectRef {
				t.Fatalf("outcome effect ref = %q; want persisted digest %q", stored.EffectRef, persistedEffectRef)
			}
		}
		taskPage, err := ListTaskViews(ctx, tx, first.Spec.RunID, []string{first.Spec.OperationOrPlanRef.ID}, "", 8)
		if err != nil {
			return err
		}
		if len(taskPage) != 1 || taskPage[0].Record.TaskID != taskID || len(taskPage[0].Outcomes) != len(outcomes) {
			t.Fatalf("authorized task page = %#v", taskPage)
		}
		deniedTaskPage, err := ListTaskViews(ctx, tx, first.Spec.RunID, []string{"operation-not-authorized"}, "", 8)
		if err != nil {
			return err
		}
		if len(deniedTaskPage) != 0 {
			t.Fatalf("pre-exposure filtered task page = %#v; want empty", deniedTaskPage)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := WithinScope(ctx, pool, foreignScope, testActor(foreignScope), func(ctx context.Context, tx *Transaction) error {
		if _, err := LoadRunView(ctx, tx, first.Spec.RunID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-scope run view error = %v; want ErrNotFound", err)
		}
		if _, err := LoadTaskView(ctx, tx, TaskRef{RunID: first.Spec.RunID, TaskID: taskID}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-scope task view error = %v; want ErrNotFound", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func registerRunViewFixture(t *testing.T, pool *Pool, scope contract.Scope, request AdmissionRequest) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		if err := RegisterWorkspace(ctx, tx, WorkspaceIdentity{Scope: scope, Name: "run-view-workspace"}); err != nil {
			return err
		}
		accountID := "provider-" + request.Spec.RunID
		if err := RegisterProviderAccount(ctx, tx, ProviderAccount{
			OrgID:          scope.OrgID,
			AccountID:      accountID,
			Provider:       "run-view-provider",
			AccountWitness: "witness-" + request.Spec.RunID,
			Capacity:       64,
			Revision:       1,
		}); err != nil {
			return err
		}
		if err := RegisterConnection(ctx, tx, Connection{
			Scope:             scope,
			ConnectionID:      request.InputSnapshot.ConnectionID,
			ConnectorID:       "run-view-connector",
			ProviderAccountID: accountID,
			Revision:          1,
		}); err != nil {
			return err
		}
		_, err := AdmitRun(ctx, tx, request)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
