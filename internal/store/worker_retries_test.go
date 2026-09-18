package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type workerRetryStoreFixture struct {
	credentials testStoreCredentials
	pool        *Pool
	scope       contract.Scope
	request     AdmissionRequest
	task        contract.Task
	lease       Lease
	root        contract.Dispatch
	rootAttempt contract.Attempt
}

func newWorkerRetryStoreFixture(t *testing.T, maxAttempts uint32) *workerRetryStoreFixture {
	t.Helper()
	credentials, pool := prepareTestStore(t, 4)
	scope := newTestScope(t)
	t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })
	request := testAdmissionRequest(t, scope, "worker-retry", "worker-retry")
	request.Grant.ConnectionIDs = []string{request.InputSnapshot.ConnectionID}
	request.Grant.PolicyEpochs.Connection = 1
	policy := request.BudgetSnapshot.Policy
	policy.HTTP = &contract.HTTPExecutionPolicy{
		MaxRequestBytes:  1 << 20,
		MaxResponseBytes: 1 << 20,
		MaxDNSAnswers:    8,
		MaxAttempts:      maxAttempts,
		MaxRetryDelay:    time.Minute,
		SelectedOrigin:   "https://api.example.test/v1",
	}
	setAdmissionPolicyForTest(t, &request, policy)
	admitForTest(t, pool, scope, request)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	task := testTask(request, "retry-task", time.Now().UTC())
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return CreateTask(ctx, tx, task)
	}); err != nil {
		t.Fatalf("create retry task: %v", err)
	}
	var lease Lease
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		lease, err = ClaimTask(ctx, tx, ClaimRequest{
			Task:    TaskRef{RunID: task.RunID, TaskID: task.TaskID},
			OwnerID: "retry-worker",
			TTL:     time.Minute,
		})
		return err
	}); err != nil {
		t.Fatalf("claim retry task: %v", err)
	}
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		task, err = StartWorkerTask(ctx, tx, lease, task, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatalf("start retry task: %v", err)
	}
	root := testDispatch(t, request.Spec.RunID)
	root.OperationRevision = request.Spec.OperationOrPlanRef.Revision
	root.GrantEpoch = request.Grant.GrantEpoch
	if err := root.Validate(); err != nil {
		t.Fatalf("validate retry root dispatch: %v", err)
	}
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return PrepareDispatch(ctx, tx, lease, root)
	}); err != nil {
		t.Fatalf("prepare retry root dispatch: %v", err)
	}
	attemptReceipt := testReceipt("retry-root-attempt-"+request.Spec.RunID, "dispatch_attempt", request.Spec.RunID)
	attempt := contract.Attempt{
		AttemptID:  root.AttemptID,
		DispatchID: root.DispatchID,
		ReceiptRef: attemptReceipt.ReceiptID,
		RecordedAt: time.Now().UTC(),
	}
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := RecordDispatchAttempt(ctx, tx, DispatchAttemptCommit{
			Task:    lease.Task,
			Attempt: attempt,
			Receipt: attemptReceipt,
			Audit:   testAudit("retry-root-attempt-audit-"+request.Spec.RunID, "dispatch_possibly_sent", request.Spec.RunID),
			Event:   testEvent("retry-root-attempt-event-"+request.Spec.RunID, "dispatch_possibly_sent", request.Spec.RunID),
			Outbox:  testOutbox("retry-root-attempt-outbox-"+request.Spec.RunID, "reconcile_dispatch", request.Spec.RunID),
		})
		return err
	}); err != nil {
		t.Fatalf("record retry root attempt: %v", err)
	}
	return &workerRetryStoreFixture{
		credentials: credentials,
		pool:        pool,
		scope:       scope,
		request:     request,
		task:        task,
		lease:       lease,
		root:        root,
		rootAttempt: attempt,
	}
}

func (fixture *workerRetryStoreFixture) nextDispatch(t *testing.T, label string) contract.Dispatch {
	t.Helper()
	next := fixture.root
	dispatchID, err := contract.NewDispatchID("dispatch-" + label)
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := contract.NewAttemptID("attempt-" + label)
	if err != nil {
		t.Fatal(err)
	}
	next.DispatchID = dispatchID
	next.AttemptID = attemptID
	if err := next.Validate(); err != nil {
		t.Fatalf("validate next retry dispatch: %v", err)
	}
	return next
}

func (fixture *workerRetryStoreFixture) retrySettlement(
	t *testing.T,
	label string,
	state contract.OutcomeState,
	basis WorkerRetryBasis,
	delay time.Duration,
) SettlementCommit {
	t.Helper()
	observedAt := time.Now().UTC()
	assertions := []string{}
	if state == contract.OutcomeUnknown || state == contract.OutcomeUnsent {
		assertions = []string{fixture.rootAttempt.ReceiptRef}
	}
	outcomes := []contract.MemberOutcome{{
		MemberID:      fixture.root.OrderedMemberIDs[0],
		EffectKey:     fixture.root.EffectKey,
		DispatchID:    fixture.root.DispatchID,
		State:         state,
		ObservedAt:    observedAt,
		AssertionRefs: assertions,
	}}
	inputDigest, err := WorkerRetryInputDigest(fixture.request.InputSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	plan := WorkerRetryPlan{
		NextDispatch: fixture.nextDispatch(t, label),
		Basis:        basis,
		NotBefore:    observedAt.Add(delay),
		InputDigest:  inputDigest,
	}
	summary, err := WorkerRetrySettlementSummary(fixture.root, outcomes, plan)
	if err != nil {
		t.Fatalf("summarize retry settlement: %v", err)
	}
	updated, err := fixture.task.ApplyEffectSummary(summary, false, "", observedAt)
	if err != nil {
		t.Fatalf("apply retry settlement summary: %v", err)
	}
	return SettlementCommit{
		Lease:      fixture.lease,
		DispatchID: fixture.root.DispatchID,
		Outcomes:   outcomes,
		Task:       updated,
		Retry:      &plan,
		Receipt:    testReceipt("retry-settlement-"+label+"-"+fixture.request.Spec.RunID, "worker_settlement", fixture.request.Spec.RunID),
		Audit:      testAudit("retry-settlement-audit-"+label+"-"+fixture.request.Spec.RunID, "dispatch_settled", fixture.request.Spec.RunID),
		Event:      testEvent("retry-settlement-event-"+label+"-"+fixture.request.Spec.RunID, "dispatch_settled", fixture.request.Spec.RunID),
		Outbox:     testOutbox("retry-settlement-outbox-"+label+"-"+fixture.request.Spec.RunID, "reconcile_dispatch", fixture.request.Spec.RunID),
	}
}

func (fixture *workerRetryStoreFixture) commitRetry(t *testing.T, commit SettlementCommit) DispatchRecord {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
		_, err := CommitSettlement(ctx, tx, commit)
		return err
	}); err != nil {
		t.Fatalf("commit worker retry: %v", err)
	}
	fixture.task = commit.Task
	var leaf DispatchRecord
	if err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		leaf, err = LoadWorkerDispatch(ctx, tx, fixture.lease.Task)
		return err
	}); err != nil {
		t.Fatalf("load committed retry leaf: %v", err)
	}
	return leaf
}

func (fixture *workerRetryStoreFixture) claimRetryLease(t *testing.T, label string) Lease {
	t.Helper()
	parent := fixture.lease
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var fresh Lease
	if err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		fresh, err = ClaimTask(ctx, tx, ClaimRequest{
			Task:          parent.Task,
			OwnerID:       "retry-worker-" + label,
			ExpectedFence: parent.Fence,
			TTL:           time.Minute,
		})
		return err
	}); err != nil {
		t.Fatalf("claim fresh retry lease: %v", err)
	}
	if fresh.Fence != parent.Fence+1 ||
		fresh.OwnerID == parent.OwnerID ||
		fresh.Task != parent.Task {
		t.Fatalf("fresh retry lease = %#v; parent = %#v", fresh, parent)
	}
	err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
		_, err := RenewLease(ctx, tx, parent, time.Minute)
		return err
	})
	if !errors.Is(err, ErrStaleFence) {
		t.Fatalf("stale parent retry renewal error = %v; want ErrStaleFence", err)
	}
	fixture.lease = fresh
	return parent
}

func (fixture *workerRetryStoreFixture) cancelRun(t *testing.T, label string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
		current, err := LoadWorkerRunForUpdate(ctx, tx, fixture.request.Spec.RunID)
		if err != nil {
			return err
		}
		updated, err := current.RequestCancel()
		if err != nil {
			return err
		}
		_, err = CommitRunTransition(ctx, tx, RunTransitionCommit{
			Run:     updated,
			Receipt: testReceipt("retry-cancel-receipt-"+label, "run_cancel", current.Spec.RunID),
			Audit:   testAudit("retry-cancel-audit-"+label, "run_cancel_requested", current.Spec.RunID),
			Event:   testEvent("retry-cancel-event-"+label, "run_cancel_requested", current.Spec.RunID),
			Outbox:  testOutbox("retry-cancel-outbox-"+label, "cancel_run", current.Spec.RunID),
		})
		return err
	}); err != nil {
		t.Fatalf("cancel retry run: %v", err)
	}
}

func TestWorkerRetryLineagePreservesSourceSafeAttemptTruth(t *testing.T) {
	fixture := newWorkerRetryStoreFixture(t, 3)
	firstCommit := fixture.retrySettlement(t, "source-safe", contract.OutcomeRejected, WorkerRetrySourceDeclaredSafe, 0)
	leaf := fixture.commitRetry(t, firstCommit)
	fixture.claimRetryLease(t, "source-safe")
	if leaf.AttemptNumber != 2 || leaf.Retry == nil || leaf.State != DispatchPrepared || len(leaf.Attempts) != 0 {
		t.Fatalf("prepared source-safe leaf = %#v; want exact unattempted ordinal two", leaf)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
		root, err := LoadInitialWorkerDispatch(ctx, tx, fixture.lease.Task)
		if err != nil {
			return err
		}
		if root.Intent.DispatchID != fixture.root.DispatchID || root.AttemptNumber != 1 || root.Retry != nil {
			t.Fatalf("initial retry lineage root = %#v", root)
		}
		outcomes, err := ListMemberOutcomes(ctx, tx, fixture.lease.Task, fixture.root.DispatchID)
		if err != nil {
			return err
		}
		if len(outcomes) != 1 || !reflect.DeepEqual(outcomes[0], firstCommit.Outcomes[0]) {
			t.Fatalf("retained parent outcomes = %#v; want %#v", outcomes, firstCommit.Outcomes)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	duplicate := firstCommit
	duplicate.Lease = fixture.lease
	duplicateTask, err := fixture.task.ApplyEffectSummary(contract.EffectSummary{Unknown: 1}, false, "", firstCommit.Outcomes[0].ObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	duplicate.Task = duplicateTask
	duplicate.Retry = &WorkerRetryPlan{
		NextDispatch: fixture.nextDispatch(t, "source-safe-duplicate"),
		Basis:        firstCommit.Retry.Basis,
		NotBefore:    firstCommit.Retry.NotBefore,
		InputDigest:  firstCommit.Retry.InputDigest,
	}
	duplicate.Receipt = testReceipt("retry-settlement-duplicate-"+fixture.request.Spec.RunID, "worker_settlement", fixture.request.Spec.RunID)
	duplicate.Audit = testAudit("retry-settlement-duplicate-audit-"+fixture.request.Spec.RunID, "dispatch_settled", fixture.request.Spec.RunID)
	duplicate.Event = testEvent("retry-settlement-duplicate-event-"+fixture.request.Spec.RunID, "dispatch_settled", fixture.request.Spec.RunID)
	duplicate.Outbox = testOutbox("retry-settlement-duplicate-outbox-"+fixture.request.Spec.RunID, "reconcile_dispatch", fixture.request.Spec.RunID)
	if err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
		_, err := CommitSettlement(ctx, tx, duplicate)
		return err
	}); err == nil {
		t.Fatal("second child for the same parent unexpectedly committed")
	}

	attemptReceipt := testReceipt("retry-child-attempt-"+fixture.request.Spec.RunID, "dispatch_attempt", fixture.request.Spec.RunID)
	childAttempt := contract.Attempt{
		AttemptID:  leaf.Intent.AttemptID,
		DispatchID: leaf.Intent.DispatchID,
		ReceiptRef: attemptReceipt.ReceiptID,
		RecordedAt: time.Now().UTC(),
	}
	if err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
		_, err := RecordDispatchAttempt(ctx, tx, DispatchAttemptCommit{
			Task:    fixture.lease.Task,
			Attempt: childAttempt,
			Receipt: attemptReceipt,
			Audit:   testAudit("retry-child-attempt-audit-"+fixture.request.Spec.RunID, "dispatch_possibly_sent", fixture.request.Spec.RunID),
			Event:   testEvent("retry-child-attempt-event-"+fixture.request.Spec.RunID, "dispatch_possibly_sent", fixture.request.Spec.RunID),
			Outbox:  testOutbox("retry-child-attempt-outbox-"+fixture.request.Spec.RunID, "reconcile_dispatch", fixture.request.Spec.RunID),
		})
		return err
	}); err != nil {
		t.Fatalf("record source-safe replay attempt: %v", err)
	}
	if err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
		effects, err := ListWorkerRunEffects(ctx, tx, fixture.request.Spec.RunID)
		if err != nil {
			return err
		}
		if len(effects) != 1 || effects[0].State != contract.OutcomeUnknown || len(effects[0].Attempts) != 2 || effects[0].Attempts[1] != childAttempt {
			t.Fatalf("source-safe replay effect lineage = %#v", effects)
		}
		if effects[0].History[len(effects[0].History)-1].Reason != "replay_dispatched" {
			t.Fatalf("source-safe replay history = %#v", effects[0].History)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	observedAt := time.Now().UTC()
	outcomes := []contract.MemberOutcome{{
		MemberID:      leaf.Intent.OrderedMemberIDs[0],
		EffectKey:     leaf.Intent.EffectKey,
		DispatchID:    leaf.Intent.DispatchID,
		State:         contract.OutcomeSucceeded,
		ObservedAt:    observedAt,
		AssertionRefs: []string{},
	}}
	settledTask, err := fixture.task.ApplyEffectSummary(contract.EffectSummary{Succeeded: 1}, false, "", observedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
		_, err := CommitSettlement(ctx, tx, SettlementCommit{
			Lease:      fixture.lease,
			DispatchID: leaf.Intent.DispatchID,
			Outcomes:   outcomes,
			Task:       settledTask,
			Receipt:    testReceipt("retry-child-settlement-"+fixture.request.Spec.RunID, "worker_settlement", fixture.request.Spec.RunID),
			Audit:      testAudit("retry-child-settlement-audit-"+fixture.request.Spec.RunID, "dispatch_settled", fixture.request.Spec.RunID),
			Event:      testEvent("retry-child-settlement-event-"+fixture.request.Spec.RunID, "dispatch_settled", fixture.request.Spec.RunID),
			Outbox:     testOutbox("retry-child-settlement-outbox-"+fixture.request.Spec.RunID, "reconcile_dispatch", fixture.request.Spec.RunID),
		})
		return err
	}); err != nil {
		t.Fatalf("settle source-safe retry child: %v", err)
	}
	if err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
		effects, err := ListWorkerRunEffects(ctx, tx, fixture.request.Spec.RunID)
		if err != nil {
			return err
		}
		if len(effects) != 1 || effects[0].State != contract.OutcomeSucceeded || len(effects[0].Attempts) != 2 {
			t.Fatalf("settled retry effect = %#v", effects)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDefinitiveUnsentRetryUsesStrictAttemptTransition(t *testing.T) {
	fixture := newWorkerRetryStoreFixture(t, 2)
	commit := fixture.retrySettlement(t, "definitive-unsent", contract.OutcomeUnsent, WorkerRetryDefinitiveUnsent, 0)
	leaf := fixture.commitRetry(t, commit)
	fixture.claimRetryLease(t, "definitive-unsent")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
		effects, err := ListWorkerRunEffects(ctx, tx, fixture.request.Spec.RunID)
		if err != nil {
			return err
		}
		if len(effects) != 1 || effects[0].State != contract.OutcomeUnsent || len(effects[0].Attempts) != 1 {
			t.Fatalf("definitive-unsent parent effect = %#v", effects)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	attemptReceipt := testReceipt("definitive-child-attempt-"+fixture.request.Spec.RunID, "dispatch_attempt", fixture.request.Spec.RunID)
	attempt := contract.Attempt{
		AttemptID:  leaf.Intent.AttemptID,
		DispatchID: leaf.Intent.DispatchID,
		ReceiptRef: attemptReceipt.ReceiptID,
		RecordedAt: time.Now().UTC(),
	}
	if err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
		_, err := RecordDispatchAttempt(ctx, tx, DispatchAttemptCommit{
			Task:    fixture.lease.Task,
			Attempt: attempt,
			Receipt: attemptReceipt,
			Audit:   testAudit("definitive-child-audit-"+fixture.request.Spec.RunID, "dispatch_possibly_sent", fixture.request.Spec.RunID),
			Event:   testEvent("definitive-child-event-"+fixture.request.Spec.RunID, "dispatch_possibly_sent", fixture.request.Spec.RunID),
			Outbox:  testOutbox("definitive-child-outbox-"+fixture.request.Spec.RunID, "reconcile_dispatch", fixture.request.Spec.RunID),
		})
		return err
	}); err != nil {
		t.Fatalf("record definitive-unsent retry attempt: %v", err)
	}
	if err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
		effects, err := ListWorkerRunEffects(ctx, tx, fixture.request.Spec.RunID)
		if err != nil {
			return err
		}
		if len(effects) != 1 || effects[0].State != contract.OutcomeUnknown || len(effects[0].Attempts) != 2 || effects[0].History[len(effects[0].History)-1].Reason != "dispatched" {
			t.Fatalf("definitive-unsent retry lineage = %#v", effects)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerRetryRefusesUnboundOrExhaustedPlan(t *testing.T) {
	t.Run("input_digest", func(t *testing.T) {
		fixture := newWorkerRetryStoreFixture(t, 2)
		commit := fixture.retrySettlement(t, "wrong-input", contract.OutcomeRejected, WorkerRetrySourceDeclaredSafe, 0)
		commit.Retry.InputDigest = testDigest("different-input")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
			_, err := CommitSettlement(ctx, tx, commit)
			return err
		})
		if err == nil {
			t.Fatal("retry with a different admitted input digest unexpectedly committed")
		}
	})

	t.Run("attempt_budget", func(t *testing.T) {
		fixture := newWorkerRetryStoreFixture(t, 1)
		commit := fixture.retrySettlement(t, "exhausted", contract.OutcomeRejected, WorkerRetrySourceDeclaredSafe, 0)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
			_, err := CommitSettlement(ctx, tx, commit)
			return err
		})
		if err == nil {
			t.Fatal("retry beyond the sealed HTTP attempt budget unexpectedly committed")
		}
	})

	t.Run("eligibility", func(t *testing.T) {
		fixture := newWorkerRetryStoreFixture(t, 2)
		commit := fixture.retrySettlement(t, "early", contract.OutcomeRejected, WorkerRetrySourceDeclaredSafe, time.Second)
		leaf := fixture.commitRetry(t, commit)
		fixture.claimRetryLease(t, "early")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		attemptReceipt := testReceipt("early-attempt-"+fixture.request.Spec.RunID, "dispatch_attempt", fixture.request.Spec.RunID)
		err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
			_, err := RecordDispatchAttempt(ctx, tx, DispatchAttemptCommit{
				Task: fixture.lease.Task,
				Attempt: contract.Attempt{
					AttemptID:  leaf.Intent.AttemptID,
					DispatchID: leaf.Intent.DispatchID,
					ReceiptRef: attemptReceipt.ReceiptID,
					RecordedAt: commit.Outcomes[0].ObservedAt,
				},
				Receipt: attemptReceipt,
				Audit:   testAudit("early-attempt-audit-"+fixture.request.Spec.RunID, "dispatch_possibly_sent", fixture.request.Spec.RunID),
				Event:   testEvent("early-attempt-event-"+fixture.request.Spec.RunID, "dispatch_possibly_sent", fixture.request.Spec.RunID),
				Outbox:  testOutbox("early-attempt-outbox-"+fixture.request.Spec.RunID, "reconcile_dispatch", fixture.request.Spec.RunID),
			})
			return err
		})
		if err == nil {
			t.Fatal("retry attempt before durable eligibility unexpectedly committed")
		}
	})
}

func TestCloseUnattemptedWorkerRetriesUsesOnlyKnownParentTruth(t *testing.T) {
	t.Run("known_rejection", func(t *testing.T) {
		fixture := newWorkerRetryStoreFixture(t, 2)
		commit := fixture.retrySettlement(t, "close-known", contract.OutcomeRejected, WorkerRetrySourceDeclaredSafe, 0)
		leaf := fixture.commitRetry(t, commit)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
			return CloseUnattemptedWorkerRetries(ctx, tx, fixture.request.Spec.RunID)
		})
		if err == nil {
			t.Fatal("unattempted retry closed before cancellation or original deadline")
		}
		var unrelatedTasks []contract.Task
		if err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
			for index := uint64(0); index < fixture.request.BudgetSnapshot.Policy.WorkflowFanout; index++ {
				task := testTask(fixture.request, fmt.Sprintf("retained-task-%d", index), time.Now().UTC())
				if err := CreateTask(ctx, tx, task); err != nil {
					return err
				}
				unrelatedTasks = append(unrelatedTasks, task)
			}
			return nil
		}); err != nil {
			t.Fatalf("retain unrelated tasks beyond concurrent fanout: %v", err)
		}
		fixture.cancelRun(t, "close-known")
		if err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
			if err := CloseUnattemptedWorkerRetries(ctx, tx, fixture.request.Spec.RunID); err != nil {
				return err
			}
			return CloseUnattemptedWorkerRetries(ctx, tx, fixture.request.Spec.RunID)
		}); err != nil {
			t.Fatalf("close known source-safe retry: %v", err)
		}
		if err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
			effects, err := ListWorkerRunEffects(ctx, tx, fixture.request.Spec.RunID)
			if err != nil {
				return err
			}
			storedTask, err := LoadTask(ctx, tx, fixture.lease.Task)
			if err != nil {
				return err
			}
			storedLeaf, err := LoadWorkerDispatch(ctx, tx, fixture.lease.Task)
			if err != nil {
				return err
			}
			if len(effects) != 1 || effects[0].State != contract.OutcomeRejected || storedTask.EffectSummary != (contract.EffectSummary{Rejected: 1}) || storedTask.State != contract.RunStateFailed {
				t.Fatalf("known retry closure effect/task = %#v / %#v", effects, storedTask)
			}
			if storedLeaf.Intent.DispatchID != leaf.Intent.DispatchID || storedLeaf.State != DispatchPrepared || len(storedLeaf.Attempts) != 0 || storedLeaf.Retry == nil {
				t.Fatalf("known retry closure rewrote physical leaf: %#v", storedLeaf)
			}
			for _, unrelated := range unrelatedTasks {
				retained, err := LoadTask(ctx, tx, TaskRef{RunID: unrelated.RunID, TaskID: unrelated.TaskID})
				if err != nil {
					return err
				}
				if retained.State != unrelated.State || retained.EffectSummary != unrelated.EffectSummary {
					t.Fatalf("retry closure changed unrelated task truth: %#v", retained)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unknown_parent", func(t *testing.T) {
		fixture := newWorkerRetryStoreFixture(t, 2)
		commit := fixture.retrySettlement(t, "close-unknown", contract.OutcomeUnknown, WorkerRetrySourceDeclaredSafe, 0)
		fixture.commitRetry(t, commit)
		fixture.cancelRun(t, "close-unknown")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
			return CloseUnattemptedWorkerRetries(ctx, tx, fixture.request.Spec.RunID)
		}); err != nil {
			t.Fatalf("preserve unknown source-safe retry: %v", err)
		}
		if err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
			effects, err := ListWorkerRunEffects(ctx, tx, fixture.request.Spec.RunID)
			if err != nil {
				return err
			}
			storedTask, err := LoadTask(ctx, tx, fixture.lease.Task)
			if err != nil {
				return err
			}
			if len(effects) != 1 || effects[0].State != contract.OutcomeUnknown || storedTask.EffectSummary != (contract.EffectSummary{Unknown: 1}) || storedTask.State != contract.RunStateReconciling {
				t.Fatalf("unknown retry closure invented known truth: %#v / %#v", effects, storedTask)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestWorkerRetryOperationRevisionRemainsSealed(t *testing.T) {
	fixture := newWorkerRetryStoreFixture(t, 2)
	fixture.root.OperationRevision = "different-operation-revision"
	commit := fixture.retrySettlement(t, "different-source", contract.OutcomeRejected, WorkerRetrySourceDeclaredSafe, 0)
	commit.Retry.NextDispatch.OperationRevision = fixture.root.OperationRevision
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
		_, err := CommitSettlement(ctx, tx, commit)
		return err
	})
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("retry from an unsealed operation revision error = %v", err)
	}
}

func TestWorkerRetryMembershipSurvivesScopeClear(t *testing.T) {
	fixture := newWorkerRetryStoreFixture(t, 2)
	orphan := fixture.nextDispatch(t, "unlinked-child")
	encoded, err := json.Marshal(orphan)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
		_, err := tx.exec(ctx, `
			INSERT INTO pm.dispatches (
				org_id, workspace_id, run_id, task_id, dispatch_id, effect_digest,
				state, intent, prepared_at, attempt_number
			)
			SELECT org_id, workspace_id, run_id, task_id, $3, effect_digest,
			       'prepared', $4, clock_timestamp(), 2
			FROM pm.dispatches
			WHERE run_id = $1 AND task_id = $2 AND dispatch_id = $5
		`, fixture.task.RunID, fixture.task.TaskID, string(orphan.DispatchID),
			json.RawMessage(encoded), string(fixture.root.DispatchID))
		return err
	})
	if err == nil || errors.Is(err, ErrCommitOutcomeUnknown) {
		t.Fatalf("unlinked retry commit = %v; want a definitive refusal after scope clear", err)
	}
	if err := WithinScope(ctx, fixture.pool, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
		if _, err := LoadDispatch(ctx, tx, fixture.lease.Task, orphan.DispatchID); !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("unlinked child survived rollback: %w", err)
		}
		root, err := LoadWorkerDispatch(ctx, tx, fixture.lease.Task)
		if err != nil {
			return err
		}
		if root.Intent.DispatchID != fixture.root.DispatchID || len(root.Attempts) != 1 || root.Attempts[0] != fixture.rootAttempt {
			return errors.New("refused child changed the existing physical attempt")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
