package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestAtomicStateReceiptOutbox(t *testing.T) {
	credentials, pool := prepareTestStore(t, 4)
	scope := newTestScope(t)
	t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })
	request := testAdmissionRequest(t, scope, "event-key", "event")
	admitForTest(t, pool, scope, request)
	task := testTask(request, "task-1", time.Now().UTC())
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return CreateTask(ctx, tx, task)
	}); err != nil {
		t.Fatal(err)
	}
	var lease Lease
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		lease, err = ClaimTask(ctx, tx, ClaimRequest{
			Task: TaskRef{RunID: request.Spec.RunID, TaskID: task.TaskID}, OwnerID: "worker-1", TTL: time.Minute,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	running, err := task.ObserveOrchestration(contract.OrchestrationRunning, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	firstCommit := TaskTransitionCommit{
		Lease:   lease,
		Task:    running,
		Receipt: testReceipt("receipt-transition-1", "task_transition", request.Spec.RunID),
		Audit:   testAudit("audit-transition-1", "task_running", request.Spec.RunID),
		Event:   testEvent("event-transition-1", "task_running", request.Spec.RunID),
		Outbox:  testOutbox("outbox-transition-1", "project_task", request.Spec.RunID),
	}
	var firstEvent contract.ServerEvent
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		firstEvent, err = CommitTaskTransition(ctx, tx, firstCommit)
		return err
	}); err != nil {
		t.Fatalf("commit complete transition bundle: %v", err)
	}
	if firstEvent.Sequence != 2 || firstEvent.StateRevision != running.Revision {
		t.Fatalf("first task event = %#v; want sequence 2 and task revision %d", firstEvent, running.Revision)
	}

	completed, err := running.ObserveOrchestration(contract.OrchestrationCompleted, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	failedBundle := TaskTransitionCommit{
		Lease:   lease,
		Task:    completed,
		Receipt: testReceipt("receipt-rolled-back", "task_transition", request.Spec.RunID),
		Audit:   testAudit("audit-rolled-back", "task_completed", request.Spec.RunID),
		Event:   testEvent(firstCommit.Event.EventID, "task_completed", request.Spec.RunID),
		Outbox:  testOutbox("outbox-rolled-back", "project_task", request.Spec.RunID),
	}
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := CommitTaskTransition(ctx, tx, failedBundle)
		return err
	}); err == nil {
		t.Fatal("duplicate event fault unexpectedly committed state")
	}

	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		stored, err := LoadTask(ctx, tx, lease.Task)
		if err != nil {
			return err
		}
		if stored.Revision != running.Revision || stored.OrchestrationCondition != contract.OrchestrationRunning {
			t.Fatalf("state committed without event: %#v", stored)
		}
		for table, id := range map[string]string{
			"receipts":        failedBundle.Receipt.ReceiptID,
			"audit_entries":   failedBundle.Audit.AuditID,
			"outbox_commands": failedBundle.Outbox.CommandID,
		} {
			var count int
			query := `SELECT count(*) FROM pm.` + table + ` WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND `
			switch table {
			case "receipts":
				query += `receipt_id = $4`
			case "audit_entries":
				query += `audit_id = $4`
			default:
				query += `command_id = $4`
			}
			if err := tx.queryRow(ctx, query, scope.OrgID, scope.WorkspaceID, request.Spec.RunID, id).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				t.Fatalf("%s row committed despite event failure", table)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	recoveredBundle := failedBundle
	recoveredBundle.Receipt = testReceipt("receipt-transition-2", "task_transition", request.Spec.RunID)
	recoveredBundle.Audit = testAudit("audit-transition-2", "task_completed", request.Spec.RunID)
	recoveredBundle.Event = testEvent("event-transition-2", "task_completed", request.Spec.RunID)
	recoveredBundle.Outbox = testOutbox("outbox-transition-2", "project_task", request.Spec.RunID)
	var recoveredEvent contract.ServerEvent
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		recoveredEvent, err = CommitTaskTransition(ctx, tx, recoveredBundle)
		return err
	}); err != nil {
		t.Fatalf("commit recovered transition: %v", err)
	}
	if recoveredEvent.Sequence != 3 {
		t.Fatalf("event sequence after rolled-back write = %d; want 3 with no gap", recoveredEvent.Sequence)
	}

	t.Run("settlement_keeps_independently_attributable_member_outcomes", func(t *testing.T) {
		dispatch := testDispatch(t, request.Spec.RunID)
		if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			return PrepareDispatch(ctx, tx, lease, dispatch)
		}); err != nil {
			t.Fatal(err)
		}
		outcomes := []contract.MemberOutcome{{
			MemberID:      dispatch.OrderedMemberIDs[0],
			EffectKey:     dispatch.EffectKey,
			DispatchID:    dispatch.DispatchID,
			State:         contract.OutcomeUnknown,
			ObservedAt:    time.Now().UTC(),
			AssertionRefs: []string{},
		}}
		settledTask, err := completed.ApplyEffectSummary(contract.EffectSummary{Unknown: 1}, false, "", time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		settlement := SettlementCommit{
			Lease:      lease,
			DispatchID: dispatch.DispatchID,
			Outcomes:   outcomes,
			Task:       settledTask,
			Receipt:    testReceipt("receipt-settlement", "settlement", request.Spec.RunID),
			Audit:      testAudit("audit-settlement", "dispatch_settled", request.Spec.RunID),
			Event:      testEvent("event-settlement", "dispatch_settled", request.Spec.RunID),
			Outbox:     testOutbox("outbox-settlement", "reconcile_dispatch", request.Spec.RunID),
		}
		if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			_, err := CommitSettlement(ctx, tx, settlement)
			return err
		}); err != nil {
			t.Fatalf("commit exact member settlement: %v", err)
		}
		if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			stored, err := ListMemberOutcomes(ctx, tx, lease.Task, dispatch.DispatchID)
			if err != nil {
				return err
			}
			if len(stored) != 1 || stored[0].MemberID != outcomes[0].MemberID || stored[0].State != contract.OutcomeUnknown {
				t.Fatalf("stored member outcomes = %#v; want exact unknown member", stored)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		events, err := ListEvents(ctx, tx, request.Spec.RunID, 0, 16)
		if err != nil {
			return err
		}
		for index, event := range events {
			want := uint64(index + 1)
			if event.Sequence != want {
				t.Fatalf("ordered history sequence[%d] = %d; want %d", index, event.Sequence, want)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSettlementProjectionMatchesDurableEffects(t *testing.T) {
	credentials, pool := prepareTestStore(t, 4)
	scope := newTestScope(t)
	t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })
	request := testAdmissionRequest(t, scope, "settlement-projection-key", "settlement-projection")
	admitForTest(t, pool, scope, request)
	task := testTask(request, "task-settlement-projection", time.Now().UTC())
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return CreateTask(ctx, tx, task)
	}); err != nil {
		t.Fatal(err)
	}
	var lease Lease
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		lease, err = ClaimTask(ctx, tx, ClaimRequest{
			Task: TaskRef{RunID: task.RunID, TaskID: task.TaskID}, OwnerID: "worker-settlement", TTL: time.Minute,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	uncertainDispatch := testDispatchFor(t, task.RunID, "uncertain")
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return PrepareDispatch(ctx, tx, lease, uncertainDispatch)
	}); err != nil {
		t.Fatal(err)
	}
	uncertainOutcome := contract.MemberOutcome{
		MemberID: uncertainDispatch.OrderedMemberIDs[0], EffectKey: uncertainDispatch.EffectKey,
		DispatchID: uncertainDispatch.DispatchID, State: contract.OutcomeUnknown,
		ObservedAt: time.Now().UTC(), AssertionRefs: []string{},
	}
	uncertainTask, err := task.ApplyEffectSummary(
		contract.EffectSummary{Unknown: 1}, false, "", uncertainOutcome.ObservedAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	settlement := func(dispatch contract.Dispatch, outcome contract.MemberOutcome, projection contract.Task, suffix string) SettlementCommit {
		return SettlementCommit{
			Lease: lease, DispatchID: dispatch.DispatchID, Outcomes: []contract.MemberOutcome{outcome}, Task: projection,
			Receipt: testReceipt("receipt-settlement-"+suffix, "settlement", task.RunID),
			Audit:   testAudit("audit-settlement-"+suffix, "dispatch_settled", task.RunID),
			Event:   testEvent("event-settlement-"+suffix, "dispatch_settled", task.RunID),
			Outbox:  testOutbox("outbox-settlement-"+suffix, "reconcile_dispatch", task.RunID),
		}
	}
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := CommitSettlement(ctx, tx, settlement(uncertainDispatch, uncertainOutcome, uncertainTask, "uncertain"))
		return err
	}); err != nil {
		t.Fatalf("commit independently uncertain effect: %v", err)
	}

	succeededDispatch := testDispatchFor(t, task.RunID, "succeeded")
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return PrepareDispatch(ctx, tx, lease, succeededDispatch)
	}); err != nil {
		t.Fatal(err)
	}
	succeededOutcome := contract.MemberOutcome{
		MemberID: succeededDispatch.OrderedMemberIDs[0], EffectKey: succeededDispatch.EffectKey,
		DispatchID: succeededDispatch.DispatchID, State: contract.OutcomeSucceeded,
		ObservedAt: time.Now().UTC(), AssertionRefs: []string{},
	}
	assertUnchanged := func(t *testing.T) {
		t.Helper()
		if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			storedTask, err := LoadTask(ctx, tx, lease.Task)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(storedTask, uncertainTask) {
				t.Fatalf("contradictory settlement changed task projection: %#v", storedTask)
			}
			outcomes, err := ListMemberOutcomes(ctx, tx, lease.Task, succeededDispatch.DispatchID)
			if err != nil {
				return err
			}
			if len(outcomes) != 0 {
				t.Fatalf("contradictory settlement retained member outcomes: %#v", outcomes)
			}
			var state string
			if err := tx.queryRow(ctx, `
				SELECT effect.state
				FROM pm.dispatches AS dispatch
				JOIN pm.effects AS effect
				  ON effect.org_id = dispatch.org_id
				 AND effect.workspace_id = dispatch.workspace_id
				 AND effect.run_id = dispatch.run_id
				 AND effect.task_id = dispatch.task_id
				 AND effect.effect_digest = dispatch.effect_digest
				WHERE dispatch.run_id = $1 AND dispatch.task_id = $2 AND dispatch.dispatch_id = $3
			`, task.RunID, task.TaskID, string(succeededDispatch.DispatchID)).Scan(&state); err != nil {
				return err
			}
			if state != string(contract.OutcomeUnsent) {
				t.Fatalf("contradictory settlement changed durable effect to %q", state)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	unexpectedSettlement := errors.New("contradictory settlement unexpectedly accepted")
	for _, terminal := range []contract.RunState{
		contract.RunStateSucceeded,
		contract.RunStateFailed,
		contract.RunStateCancelled,
	} {
		t.Run("reject_clean_"+string(terminal)+"_while_another_effect_is_unknown", func(t *testing.T) {
			candidate := uncertainTask
			candidate.State = terminal
			candidate.EffectSummary = contract.EffectSummary{Succeeded: 1}
			candidate.UnknownEffectCount = 0
			candidate.BlockedReason = ""
			candidate.ProjectionObservedAt = succeededOutcome.ObservedAt
			candidate.Revision++
			if err := candidate.Validate(); err != nil {
				t.Fatalf("construct standalone-valid contradictory task: %v", err)
			}
			err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
				if _, err := CommitSettlement(ctx, tx, settlement(succeededDispatch, succeededOutcome, candidate, string(terminal))); err != nil {
					return err
				}
				return unexpectedSettlement
			})
			if errors.Is(err, unexpectedSettlement) {
				t.Fatalf("contradictory %s settlement reached commit", terminal)
			}
			if !errors.Is(err, ErrCASConflict) {
				t.Fatalf("contradictory %s settlement = %v; want ErrCASConflict", terminal, err)
			}
			assertUnchanged(t)
		})
	}

	combinedTask, err := uncertainTask.ApplyEffectSummary(
		contract.EffectSummary{Succeeded: 1, Unknown: 1}, false, "", succeededOutcome.ObservedAt,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := CommitSettlement(ctx, tx, settlement(succeededDispatch, succeededOutcome, combinedTask, "combined"))
		return err
	}); err != nil {
		t.Fatalf("commit exact multi-effect projection: %v", err)
	}
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		storedTask, err := LoadTask(ctx, tx, lease.Task)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(storedTask, combinedTask) {
			t.Fatalf("stored combined task projection = %#v; want %#v", storedTask, combinedTask)
		}
		outcomes, err := ListMemberOutcomes(ctx, tx, lease.Task, succeededDispatch.DispatchID)
		if err != nil {
			return err
		}
		if len(outcomes) != 1 || outcomes[0].State != contract.OutcomeSucceeded {
			t.Fatalf("stored independent successful outcome = %#v", outcomes)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRunProjectionPreservesHistoryAndTerminalState(t *testing.T) {
	credentials, pool := prepareTestStore(t, 3)
	scope := newTestScope(t)
	t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })
	request := testAdmissionRequest(t, scope, "run-projection-key", "run-projection")
	admitForTest(t, pool, scope, request)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var accepted contract.RunRecord
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		accepted, err = LoadRun(ctx, tx, request.Spec.RunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	commit := func(projection contract.RunRecord, suffix string, rollbackOnSuccess error) error {
		return WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			_, err := CommitRunTransition(ctx, tx, RunTransitionCommit{
				Run:     projection,
				Receipt: testReceipt("receipt-run-"+suffix, "run_transition", request.Spec.RunID),
				Audit:   testAudit("audit-run-"+suffix, "run_transition", request.Spec.RunID),
				Event:   testEvent("event-run-"+suffix, "run_transition", request.Spec.RunID),
				Outbox:  testOutbox("outbox-run-"+suffix, "project_run", request.Spec.RunID),
			})
			if err != nil {
				return err
			}
			return rollbackOnSuccess
		})
	}
	assertStored := func(t *testing.T, expected contract.RunRecord, eventCount int) {
		t.Helper()
		if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			stored, err := LoadRun(ctx, tx, request.Spec.RunID)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(stored, expected) {
				t.Fatalf("stored run projection = %#v; want %#v", stored, expected)
			}
			events, err := ListEvents(ctx, tx, request.Spec.RunID, 0, 16)
			if err != nil {
				return err
			}
			if len(events) != eventCount {
				t.Fatalf("durable event count = %d; want %d", len(events), eventCount)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	unexpectedProjection := errors.New("invalid run projection unexpectedly accepted")
	running, err := accepted.Transition(contract.RunStateRunning, "started", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := commit(running, "running", nil); err != nil {
		t.Fatalf("commit legitimate appended transition: %v", err)
	}
	assertStored(t, running, 2)

	reset, err := contract.NewRunRecord(running.Spec, running.WorkflowBinding, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	reset.Revision = running.Revision + 1
	if err := commit(reset, "history-reset", unexpectedProjection); errors.Is(err, unexpectedProjection) {
		t.Errorf("history reset reached commit")
	} else if !errors.Is(err, ErrCASConflict) {
		t.Fatalf("history reset = %v; want ErrCASConflict", err)
	}
	assertStored(t, running, 2)

	terminal, err := running.Transition(contract.RunStateSucceeded, "effects_settled", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := commit(terminal, "succeeded", nil); err != nil {
		t.Fatalf("commit legitimate terminal transition: %v", err)
	}
	assertStored(t, terminal, 3)

	rollback := running
	rollback.Revision = terminal.Revision + 1
	if err := commit(rollback, "terminal-rollback", unexpectedProjection); errors.Is(err, unexpectedProjection) {
		t.Errorf("terminal/history rollback reached commit")
	} else if !errors.Is(err, ErrCASConflict) {
		t.Fatalf("terminal/history rollback = %v; want ErrCASConflict", err)
	}
	assertStored(t, terminal, 3)

	observed, err := terminal.ObserveOrchestration(contract.OrchestrationCompleted, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := commit(observed, "terminal-observation", nil); err != nil {
		t.Fatalf("commit legitimate terminal observation: %v", err)
	}
	assertStored(t, observed, 4)
}

func TestRunTransitionReleasesPendingReservation(t *testing.T) {
	credentials, pool := prepareTestStore(t, 4)
	limits := AdmissionLimits{
		GlobalPending:          3,
		WorkspacePending:       2,
		WorkspaceMaxPendingAge: 30 * time.Second,
	}

	type transitionFixture struct {
		scope    contract.Scope
		request  AdmissionRequest
		accepted contract.RunRecord
	}
	newFixture := func(t *testing.T, seed string) transitionFixture {
		t.Helper()
		scope := newTestScope(t)
		t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })
		request := testAdmissionRequest(t, scope, "release-"+seed, "release-"+seed)
		setAdmissionLimitsForTest(t, &request, limits)
		admitForTest(t, pool, scope, request)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var accepted contract.RunRecord
		if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			var err error
			accepted, err = LoadRun(ctx, tx, request.Spec.RunID)
			return err
		}); err != nil {
			t.Fatalf("load accepted run: %v", err)
		}
		return transitionFixture{scope: scope, request: request, accepted: accepted}
	}
	commitProjection := func(
		ctx context.Context,
		fixture transitionFixture,
		projection contract.RunRecord,
		suffix string,
		eventID string,
	) error {
		if eventID == "" {
			eventID = "event-release-" + suffix
		}
		return WithinScope(ctx, pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
			_, err := CommitRunTransition(ctx, tx, RunTransitionCommit{
				Run:     projection,
				Receipt: testReceipt("receipt-release-"+suffix, "run_transition", fixture.request.Spec.RunID),
				Audit:   testAudit("audit-release-"+suffix, "run_transition", fixture.request.Spec.RunID),
				Event:   testEvent(eventID, "run_transition", fixture.request.Spec.RunID),
				Outbox:  testOutbox("outbox-release-"+suffix, "project_run", fixture.request.Spec.RunID),
			})
			return err
		})
	}
	reservationState := func(t *testing.T, fixture transitionFixture) (
		count int64,
		releasedAt *time.Time,
		reason *string,
		globalPending int64,
	) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := WithinScope(ctx, pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
			return tx.queryRow(ctx, `
				SELECT
					(SELECT count(*) FROM pm.pending_reservations WHERE run_id = $1),
					(SELECT released_at FROM pm.pending_reservations WHERE run_id = $1),
					(SELECT release_reason FROM pm.pending_reservations WHERE run_id = $1),
					(SELECT pending_count FROM pm.pending_admission_state WHERE singleton)
			`, fixture.request.Spec.RunID).Scan(&count, &releasedAt, &reason, &globalPending)
		}); err != nil {
			t.Fatalf("load pending reservation state: %v", err)
		}
		return count, releasedAt, reason, globalPending
	}
	assertOutstanding := func(t *testing.T, fixture transitionFixture) {
		t.Helper()
		count, releasedAt, reason, globalPending := reservationState(t, fixture)
		if count != 1 || releasedAt != nil || reason != nil || globalPending != 1 {
			t.Fatalf(
				"pending reservation = count %d, released_at %v, reason %v, global %d; want one outstanding reservation",
				count, releasedAt, reason, globalPending,
			)
		}
	}
	assertReleased := func(t *testing.T, fixture transitionFixture, wantReason string) time.Time {
		t.Helper()
		count, releasedAt, reason, globalPending := reservationState(t, fixture)
		if count != 1 || releasedAt == nil || reason == nil || *reason != wantReason || globalPending != 0 {
			t.Fatalf(
				"released reservation = count %d, released_at %v, reason %v, global %d; want reason %q and no pending charge",
				count, releasedAt, reason, globalPending, wantReason,
			)
		}
		return releasedAt.UTC()
	}
	assertStored := func(t *testing.T, fixture transitionFixture, want contract.RunRecord) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := WithinScope(ctx, pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
			stored, err := LoadRun(ctx, tx, fixture.request.Spec.RunID)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(stored, want) {
				t.Fatalf("stored run = %#v; want %#v", stored, want)
			}
			return nil
		}); err != nil {
			t.Fatalf("load stored run: %v", err)
		}
	}

	t.Run("prestart_states_and_orchestration_observation_hold_capacity", func(t *testing.T) {
		fixture := newFixture(t, "prestart")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		assertOutstanding(t, fixture)

		current := fixture.accepted
		observedAt := current.ProjectionObservedAt
		advance := func(next contract.RunState, reason, suffix string) {
			t.Helper()
			observedAt = observedAt.Add(time.Second)
			projection, err := current.Transition(next, reason, observedAt)
			if err != nil {
				t.Fatalf("construct %s projection: %v", next, err)
			}
			if err := commitProjection(ctx, fixture, projection, "prestart-"+suffix, ""); err != nil {
				t.Fatalf("commit %s projection: %v", next, err)
			}
			current = projection
			assertOutstanding(t, fixture)
		}

		advance(contract.RunStateQueued, "queued_for_worker", "queued")
		observedAt = observedAt.Add(time.Second)
		orchestrationObserved, err := current.ObserveOrchestration(contract.OrchestrationRunning, observedAt)
		if err != nil {
			t.Fatalf("construct orchestration observation: %v", err)
		}
		if err := commitProjection(ctx, fixture, orchestrationObserved, "prestart-orchestration", ""); err != nil {
			t.Fatalf("commit orchestration observation: %v", err)
		}
		current = orchestrationObserved
		assertOutstanding(t, fixture)

		advance(contract.RunStateAwaitingApproval, "approval_required", "awaiting-approval")
		advance(contract.RunStateBlocked, "policy_blocked", "blocked")
		advance(contract.RunStateReconciling, "reconciliation_pending", "reconciling")
	})

	t.Run("execution_start_releases_once_and_later_waiting_does_not_recharge", func(t *testing.T) {
		fixture := newFixture(t, "execution-started")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		running, err := fixture.accepted.Transition(
			contract.RunStateRunning,
			"worker_execution_started",
			fixture.accepted.ProjectionObservedAt.Add(time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := commitProjection(ctx, fixture, running, "execution-started", ""); err != nil {
			t.Fatalf("commit execution start: %v", err)
		}
		firstRelease := assertReleased(t, fixture, "execution_started")

		waiting, err := running.Transition(
			contract.RunStateQueued,
			"worker_waiting",
			fixture.accepted.ProjectionObservedAt.Add(2*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := commitProjection(ctx, fixture, waiting, "execution-waiting", ""); err != nil {
			t.Fatalf("commit post-start waiting state: %v", err)
		}
		if waitingRelease := assertReleased(t, fixture, "execution_started"); !waitingRelease.Equal(firstRelease) {
			t.Fatalf("waiting state retimed release from %v to %v", firstRelease, waitingRelease)
		}

		runningAgain, err := waiting.Transition(
			contract.RunStateRunning,
			"worker_execution_resumed",
			fixture.accepted.ProjectionObservedAt.Add(3*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := commitProjection(ctx, fixture, runningAgain, "execution-resumed", ""); err != nil {
			t.Fatalf("commit resumed execution: %v", err)
		}
		if resumedRelease := assertReleased(t, fixture, "execution_started"); !resumedRelease.Equal(firstRelease) {
			t.Fatalf("resumed execution retimed release from %v to %v", firstRelease, resumedRelease)
		}
		assertStored(t, fixture, runningAgain)
	})

	for _, terminal := range []struct {
		name   string
		state  contract.RunState
		reason string
	}{
		{name: "failed", state: contract.RunStateFailed, reason: "startup_failed"},
		{name: "cancelled", state: contract.RunStateCancelled, reason: "cancelled_before_start"},
	} {
		terminal := terminal
		t.Run("terminal_before_start_"+terminal.name, func(t *testing.T) {
			fixture := newFixture(t, "terminal-"+terminal.name)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			projection, err := fixture.accepted.Transition(
				terminal.state,
				terminal.reason,
				fixture.accepted.ProjectionObservedAt.Add(time.Second),
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := commitProjection(ctx, fixture, projection, "terminal-"+terminal.name, ""); err != nil {
				t.Fatalf("commit terminal pre-start state: %v", err)
			}
			assertReleased(t, fixture, "terminal_before_start")
			assertStored(t, fixture, projection)
		})
	}

	t.Run("stale_start_projection_cannot_release", func(t *testing.T) {
		fixture := newFixture(t, "stale")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		queued, err := fixture.accepted.Transition(
			contract.RunStateQueued,
			"queued_for_worker",
			fixture.accepted.ProjectionObservedAt.Add(time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := commitProjection(ctx, fixture, queued, "stale-queued", ""); err != nil {
			t.Fatalf("commit current queued projection: %v", err)
		}
		staleRunning, err := fixture.accepted.Transition(
			contract.RunStateRunning,
			"worker_execution_started",
			fixture.accepted.ProjectionObservedAt.Add(2*time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := commitProjection(ctx, fixture, staleRunning, "stale-running", ""); !errors.Is(err, ErrCASConflict) {
			t.Fatalf("stale running projection = %v; want ErrCASConflict", err)
		}
		assertOutstanding(t, fixture)
		assertStored(t, fixture, queued)
	})

	t.Run("later_event_failure_rolls_back_release", func(t *testing.T) {
		fixture := newFixture(t, "late-event-failure")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		running, err := fixture.accepted.Transition(
			contract.RunStateRunning,
			"worker_execution_started",
			fixture.accepted.ProjectionObservedAt.Add(time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := commitProjection(
			ctx,
			fixture,
			running,
			"late-event-failure",
			fixture.request.Event.EventID,
		); err == nil {
			t.Fatal("duplicate late event unexpectedly committed transition")
		}
		assertOutstanding(t, fixture)
		assertStored(t, fixture, fixture.accepted)
	})

	t.Run("missing_reservation_evidence_refuses_start", func(t *testing.T) {
		fixture := newFixture(t, "missing-evidence")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		migrator, err := pgx.Connect(ctx, credentials.migratorDSN)
		if err != nil {
			t.Fatalf("open owned missing-evidence fixture: %T", err)
		}
		defer func() {
			cleanupCtx, cleanupCancel := detachedCleanupContext()
			defer cleanupCancel()
			if err := migrator.Close(cleanupCtx); err != nil {
				t.Errorf("close owned missing-evidence fixture: %T", err)
			}
		}()
		if err := pgx.BeginFunc(ctx, migrator, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `
				SELECT set_config('pm.org_id', $1, true), set_config('pm.workspace_id', $2, true)
			`, fixture.scope.OrgID, fixture.scope.WorkspaceID); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `
				DELETE FROM pm.pending_reservations
				WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
			`, fixture.scope.OrgID, fixture.scope.WorkspaceID, fixture.request.Spec.RunID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errors.New("owned pending reservation evidence was not removed")
			}
			return nil
		}); err != nil {
			t.Fatalf("prepare missing reservation evidence: %v", err)
		}

		running, err := fixture.accepted.Transition(
			contract.RunStateRunning,
			"worker_execution_started",
			fixture.accepted.ProjectionObservedAt.Add(time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := commitProjection(ctx, fixture, running, "missing-evidence", ""); !errors.Is(err, ErrImmutableConflict) {
			t.Fatalf("start without reservation evidence = %v; want ErrImmutableConflict", err)
		}
		count, releasedAt, reason, globalPending := reservationState(t, fixture)
		if count != 0 || releasedAt != nil || reason != nil || globalPending != 0 {
			t.Fatalf(
				"missing reservation state = count %d, released_at %v, reason %v, global %d; want absent and uncharged",
				count, releasedAt, reason, globalPending,
			)
		}
		assertStored(t, fixture, fixture.accepted)
	})

	t.Run("contradictory_release_evidence_refuses_start", func(t *testing.T) {
		fixture := newFixture(t, "contradictory-evidence")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := WithinScope(ctx, pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
			tag, err := tx.exec(ctx, `
				UPDATE pm.pending_reservations
				SET released_at = clock_timestamp(), release_reason = 'terminal_before_start'
				WHERE run_id = $1 AND released_at IS NULL
			`, fixture.request.Spec.RunID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errors.New("pending reservation evidence was not contradicted")
			}
			return nil
		}); err != nil {
			t.Fatalf("contradict pending reservation evidence: %v", err)
		}
		contradictoryRelease := assertReleased(t, fixture, "terminal_before_start")

		running, err := fixture.accepted.Transition(
			contract.RunStateRunning,
			"worker_execution_started",
			fixture.accepted.ProjectionObservedAt.Add(time.Second),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := commitProjection(ctx, fixture, running, "contradictory-evidence", ""); !errors.Is(err, ErrImmutableConflict) {
			t.Fatalf("start with contradictory reservation evidence = %v; want ErrImmutableConflict", err)
		}
		if retainedRelease := assertReleased(t, fixture, "terminal_before_start"); !retainedRelease.Equal(contradictoryRelease) {
			t.Fatalf("refused start retimed contradictory evidence from %v to %v", contradictoryRelease, retainedRelease)
		}
		assertStored(t, fixture, fixture.accepted)
	})
}
