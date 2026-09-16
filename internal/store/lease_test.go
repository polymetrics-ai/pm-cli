package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestLeaseFenceAndClaimCAS(t *testing.T) {
	credentials, pool := prepareTestStore(t, 6)
	scope := newTestScope(t)
	t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })
	request := testAdmissionRequest(t, scope, "lease-key", "lease")
	admitForTest(t, pool, scope, request)
	task := testTask(request, "task-1", time.Now().UTC())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return CreateTask(ctx, tx, task)
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	type claimResult struct {
		lease Lease
		err   error
	}
	start := make(chan struct{})
	results := make(chan claimResult, 2)
	for _, owner := range []string{"worker-a", "worker-b"} {
		owner := owner
		go func() {
			<-start
			var lease Lease
			err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
				var err error
				lease, err = ClaimTask(ctx, tx, ClaimRequest{
					Task:          TaskRef{RunID: request.Spec.RunID, TaskID: task.TaskID},
					OwnerID:       owner,
					ExpectedFence: 0,
					TTL:           2 * time.Second,
				})
				return err
			})
			results <- claimResult{lease: lease, err: err}
		}()
	}
	close(start)
	first := <-results
	second := <-results
	claims := []claimResult{first, second}
	var winner Lease
	wins := 0
	losses := 0
	for _, result := range claims {
		switch {
		case result.err == nil:
			wins++
			winner = result.lease
		case errors.Is(result.err, ErrClaimLost):
			losses++
		default:
			t.Fatalf("concurrent claim error = %v; want nil or ErrClaimLost", result.err)
		}
	}
	if wins != 1 || losses != 1 || winner.Fence != 1 {
		t.Fatalf("claim results wins=%d losses=%d winner=%#v; want one owner at fence 1", wins, losses, winner)
	}

	dispatch := testDispatch(t, request.Spec.RunID)
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return PrepareDispatch(ctx, tx, winner, dispatch)
	}); err != nil {
		t.Fatalf("persist pre-send dispatch intent: %v", err)
	}

	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := tx.exec(ctx, `
			UPDATE pm.task_leases
			SET claimed_at = clock_timestamp() - interval '2 seconds',
			    lease_expires_at = clock_timestamp() - interval '1 second'
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND task_id = $4 AND fence = $5
		`, scope.OrgID, scope.WorkspaceID, winner.Task.RunID, winner.Task.TaskID, winner.Fence)
		return err
	}); err != nil {
		t.Fatalf("expire controlled lease: %v", err)
	}

	var replacement Lease
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		replacement, err = ClaimTask(ctx, tx, ClaimRequest{
			Task:          winner.Task,
			OwnerID:       "worker-replacement",
			ExpectedFence: winner.Fence,
			TTL:           2 * time.Second,
		})
		return err
	}); err != nil {
		t.Fatalf("claim expired lease: %v", err)
	}
	if replacement.Fence != winner.Fence+1 || replacement.OwnerID == winner.OwnerID {
		t.Fatalf("replacement lease = %#v; want new owner and next fence", replacement)
	}

	updatedTask, err := task.ObserveOrchestration(contract.OrchestrationRunning, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	staleCommit := TaskTransitionCommit{
		Lease:   winner,
		Task:    updatedTask,
		Receipt: testReceipt("receipt-stale", "task_transition", request.Spec.RunID),
		Audit:   testAudit("audit-stale", "task_transition", request.Spec.RunID),
		Event:   testEvent("event-stale", "task_transition", request.Spec.RunID),
		Outbox:  testOutbox("outbox-stale", "project_task", request.Spec.RunID),
	}
	err = WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := CommitTaskTransition(ctx, tx, staleCommit)
		return err
	})
	if !errors.Is(err, ErrStaleFence) {
		t.Fatalf("stale task commit error = %v; want ErrStaleFence", err)
	}

	attemptReceipt := testReceipt("receipt-attempt", "dispatch_attempt", request.Spec.RunID)
	attempt := contract.Attempt{
		AttemptID:  dispatch.AttemptID,
		DispatchID: dispatch.DispatchID,
		ReceiptRef: attemptReceipt.ReceiptID,
		RecordedAt: time.Now().UTC(),
	}
	attemptCommit := DispatchAttemptCommit{
		Task:    winner.Task,
		Attempt: attempt,
		Receipt: attemptReceipt,
		Audit:   testAudit("audit-attempt", "dispatch_possibly_sent", request.Spec.RunID),
		Event:   testEvent("event-attempt", "dispatch_possibly_sent", request.Spec.RunID),
		Outbox:  testOutbox("outbox-attempt", "reconcile_dispatch", request.Spec.RunID),
	}
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := RecordDispatchAttempt(ctx, tx, attemptCommit)
		return err
	}); err != nil {
		t.Fatalf("record possibly dispatched attempt after fence loss: %v", err)
	}

	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		storedTask, err := LoadTask(ctx, tx, winner.Task)
		if err != nil {
			return err
		}
		if storedTask.Revision != task.Revision || storedTask.OrchestrationCondition != task.OrchestrationCondition {
			t.Fatalf("stale worker changed task: %#v", storedTask)
		}
		storedDispatch, err := LoadDispatch(ctx, tx, winner.Task, dispatch.DispatchID)
		if err != nil {
			return err
		}
		if storedDispatch.State != DispatchPossiblySent || len(storedDispatch.Attempts) != 1 || storedDispatch.Intent.DispatchID != dispatch.DispatchID {
			t.Fatalf("possibly dispatched effect was erased or collapsed: %#v", storedDispatch)
		}
		lease, err := LoadLease(ctx, tx, winner.Task)
		if err != nil {
			return err
		}
		if lease.OwnerID != replacement.OwnerID || lease.Fence != replacement.Fence {
			t.Fatalf("current owner = %#v; want %#v", lease, replacement)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func testDispatch(t *testing.T, runID string) contract.Dispatch {
	t.Helper()
	entity, err := contract.NewEntityKey("account-witness-1", "issue", "42", "generation-1")
	if err != nil {
		t.Fatal(err)
	}
	effect, err := contract.NewEffectKey(entity, "occurrence-1", "effect-revision-1")
	if err != nil {
		t.Fatal(err)
	}
	dispatchID, err := contract.NewDispatchID("dispatch-1")
	if err != nil {
		t.Fatal(err)
	}
	memberID, err := contract.NewMemberID("member-1")
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := contract.NewAttemptID("attempt-1")
	if err != nil {
		t.Fatal(err)
	}
	dispatch := contract.Dispatch{
		RunID:              runID,
		EffectKey:          effect,
		DispatchID:         dispatchID,
		OrderedMemberIDs:   []contract.MemberID{memberID},
		PayloadHash:        contract.NewPayloadHash([]byte("payload")),
		RequestArtifactRef: "artifact-request-1",
		OperationRevision:  "operation-revision-1",
		AccountWitness:     entity.AccountWitness,
		GrantEpoch:         1,
		CapacityReservation: contract.CapacityReservation{
			OutcomeSlots:  1,
			ReadbackSlots: 1,
			EncodedBytes:  1024,
		},
		AttemptID: attemptID,
	}
	if err := dispatch.Validate(); err != nil {
		t.Fatalf("construct dispatch: %v", err)
	}
	return dispatch
}
