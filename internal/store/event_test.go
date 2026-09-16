package store

import (
	"context"
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
