package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

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
