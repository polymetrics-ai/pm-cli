package app_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/activity"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

// TestHeldLeaseRetryStartsWhenTheLeaseEnds holds the task lease of a planned
// unit for another attempt while the first ExecuteUnit attempt prepares the
// unit. The first attempt then finds the lease held and fails. Its retry must
// wait for the remaining lease time on the database clock, not for Temporal's
// default backoff, so the second attempt claims the task and succeeds.
//
// PostgreSQL and Temporal are the real stack. The held lease is a durable row
// with a known end. The first attempt itself installs it: its lease decision
// hook, which runs in the prepare transaction after every preparation step
// and just before the stored lease is read, holds the lease once. So the first
// claim sees a live lease however long the worker takes to start and however
// long the preparation takes. The check reads the activity's last failure from
// the workflow history after the run ends, so it does not sleep, poll or
// retry.
func TestHeldLeaseRetryStartsWhenTheLeaseEnds(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	var sends atomic.Int32
	fixture := testfixture.NewExecuting(
		t,
		func(_ context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
			sends.Add(1)
			return successfulPermitObservation(request), nil
		},
		unexpectedPermitReconciliation,
	)
	var held store.Lease
	var heldAt time.Time
	hook := newLeaseDecisionHook(
		func(ctx context.Context) bool {
			return activity.IsActivity(ctx) && activity.GetInfo(ctx).ActivityType.Name == contract.ExecuteActivityName
		},
		nil,
	)
	service := newLeaseDecisionWorkerService(t, fixture, temporal, hook)
	admitted := admitPermitRun(
		t, fixture, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID, "held-lease-retry",
	)
	run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
	ref := store.TaskRef{RunID: run.reference.RunID, TaskID: run.plan.TaskID}
	endLiveTaskLease(t, fixture, ref)
	hook.action = func(context.Context) error {
		var err error
		held, heldAt, err = holdTaskLease(fixture, ref, 6*time.Second)
		return err
	}
	hook.armed.Store(true)

	worker := startDurableHTTPRetryWorker(t, temporal.backend, temporal.timeouts, service)
	defer worker.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var projection contract.WorkflowProjectionResult
	if err := temporal.backend.GetWorkflow(
		ctx, run.execution.WorkflowID, run.execution.RunID,
	).Get(ctx, &projection); err != nil {
		t.Fatalf("await the run with a held task lease: %v", err)
	}
	if err := hook.result(); err != nil {
		t.Fatal(err)
	}
	if projection.State != contract.RunStateSucceeded || sends.Load() != 1 {
		t.Fatalf("run with a held task lease = %#v sends=%d; want succeeded with one send", projection, sends.Load())
	}

	started := executeUnitStartedEvent(t, temporal, run)
	if started.GetAttempt() < 2 {
		t.Fatalf("ExecuteUnit attempt = %d; want a first attempt that found the lease held", started.GetAttempt())
	}
	failure := started.GetLastFailure().GetApplicationFailureInfo()
	maximum := held.ExpiresAt.Sub(heldAt)
	delay := failure.GetNextRetryDelay()
	if delay == nil || delay.AsDuration() <= 0 || delay.AsDuration() > maximum {
		t.Fatalf("claim-lost retry delay = %v (attempt %d, failure type %q); want the remaining lease time, above 0 and at most %s",
			delay, started.GetAttempt(), failure.GetType(), maximum)
	}
	if started.GetAttempt() != 2 {
		t.Fatalf("ExecuteUnit attempt = %d after a retry delay of %s; want the second attempt to claim the ended lease",
			started.GetAttempt(), delay.AsDuration())
	}
}

// TestLeaseDecisionReadsTheDatabaseTimeAgain checks the lease decision of
// ExecuteUnit when preparation outlasts a held lease. ExecuteUnit samples the
// database time when it loads the run. Its lease decision hook runs later, in
// the same prepare transaction, after every preparation step and just before
// the stored lease is read. Here it changes the end of the held lease, so the
// stored end is after the first sample. The lease decision must use a database
// time from after that change, for both the expiry comparison and the
// remaining lease time. No sleep or poll sets the order: the hook call is the
// event.
func TestLeaseDecisionReadsTheDatabaseTimeAgain(t *testing.T) {
	temporal := newPermitTemporalHarness(t)

	t.Run("the lease ends during preparation: the unit claims the task", func(t *testing.T) {
		run, fixture, service, hook, sends := prepareLeaseDecisionRun(t, temporal, "lease-ends-in-preparation")
		ref := store.TaskRef{RunID: run.reference.RunID, TaskID: run.plan.TaskID}
		if _, _, err := holdTaskLease(fixture, ref, time.Minute); err != nil {
			t.Fatalf("hold the unit's task lease: %v", err)
		}
		var endedAt time.Time
		hook.action = func(context.Context) error {
			var err error
			endedAt, err = setTaskLeaseEnd(fixture, ref, 0)
			return err
		}
		hook.armed.Store(true)
		result, err := executePermitWorkerUnit(t, service, run)
		if hookErr := hook.result(); hookErr != nil {
			t.Fatal(hookErr)
		}
		var held *app.WorkerLeaseHeldError
		if errors.As(err, &held) {
			t.Fatalf("ExecuteUnit found the lease held with %s left; the lease ended at %s, during preparation", held.RetryAfter, endedAt)
		}
		if err != nil {
			t.Fatalf("ExecuteUnit after the held lease ended during preparation: %v", err)
		}
		if result.State != contract.RunStateSucceeded || sends.Load() != 1 {
			t.Fatalf("ExecuteUnit after the held lease ended = %#v sends=%d; want a succeeded unit with one send", result, sends.Load())
		}
	})

	t.Run("the lease is live at the decision: the retry delay is from the later time", func(t *testing.T) {
		run, fixture, service, hook, sends := prepareLeaseDecisionRun(t, temporal, "lease-live-at-decision")
		ref := store.TaskRef{RunID: run.reference.RunID, TaskID: run.plan.TaskID}
		if _, _, err := holdTaskLease(fixture, ref, time.Minute); err != nil {
			t.Fatalf("hold the unit's task lease: %v", err)
		}
		const remaining = 2 * time.Second
		var changedAt time.Time
		hook.action = func(context.Context) error {
			var err error
			changedAt, err = setTaskLeaseEnd(fixture, ref, remaining)
			return err
		}
		hook.armed.Store(true)
		_, err := executePermitWorkerUnit(t, service, run)
		if hookErr := hook.result(); hookErr != nil {
			t.Fatal(hookErr)
		}
		var held *app.WorkerLeaseHeldError
		if !errors.As(err, &held) {
			t.Fatalf("ExecuteUnit with a live held lease: %v; want a held-lease error", err)
		}
		// changedAt is a database time after the first sample and before the
		// lease decision. The lease ends exactly remaining after changedAt.
		if held.RetryAfter <= 0 || held.RetryAfter > remaining {
			t.Fatalf("held-lease retry delay = %s; want above 0 and at most %s, the time left when the lease end changed at %s",
				held.RetryAfter, remaining, changedAt)
		}
		if sends.Load() != 0 {
			t.Fatalf("ExecuteUnit with a live held lease sent %d requests; want none", sends.Load())
		}
	})
}

func prepareLeaseDecisionRun(
	t *testing.T,
	temporal *permitTemporalHarness,
	requestKey string,
) (permitWorkerRun, *testfixture.Fixture, *app.WorkerService, *leaseDecisionHook, *atomic.Int32) {
	t.Helper()
	sends := &atomic.Int32{}
	fixture := testfixture.NewExecuting(
		t,
		func(_ context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
			sends.Add(1)
			return successfulPermitObservation(request), nil
		},
		unexpectedPermitReconciliation,
	)
	// ExecuteUnit runs in the test goroutine here, so every lease decision
	// after the hook is armed belongs to it.
	hook := newLeaseDecisionHook(func(context.Context) bool { return true }, nil)
	service := newLeaseDecisionWorkerService(t, fixture, temporal, hook)
	admitted := admitPermitRun(
		t, fixture, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID, requestKey,
	)
	run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
	endLiveTaskLease(t, fixture, store.TaskRef{RunID: run.reference.RunID, TaskID: run.plan.TaskID})
	return run, fixture, service, hook, sends
}

// leaseDecisionHook runs one action in the prepare transaction of
// ExecuteUnit, just before the lease decision reads the stored lease
// (app.SetBeforeLeaseDecision). That is after every other preparation step,
// so no preparation time lies between the action and the decision. The hook
// runs its action only once, and only after the test arms it.
type leaseDecisionHook struct {
	armed  atomic.Bool
	fired  atomic.Bool
	match  func(context.Context) bool
	action func(context.Context) error
	err    error
	done   chan struct{}
}

func newLeaseDecisionHook(
	match func(context.Context) bool,
	action func(context.Context) error,
) *leaseDecisionHook {
	return &leaseDecisionHook{match: match, action: action, done: make(chan struct{})}
}

func (hook *leaseDecisionHook) run(ctx context.Context) error {
	if hook.armed.Load() && hook.match(ctx) && hook.fired.CompareAndSwap(false, true) {
		hook.err = hook.action(ctx)
		close(hook.done)
		return hook.err
	}
	return nil
}

// result returns the error of the action. It fails when the action did not
// run, so a test cannot pass without the event that sets its order.
func (hook *leaseDecisionHook) result() error {
	select {
	case <-hook.done:
		if hook.err != nil {
			return errors.Join(errors.New("lease decision hook failed"), hook.err)
		}
		return nil
	default:
		return errors.New("ExecuteUnit made no lease decision after the lease decision hook was armed")
	}
}

func newLeaseDecisionWorkerService(
	t *testing.T,
	fixture *testfixture.Fixture,
	temporal *permitTemporalHarness,
	hook *leaseDecisionHook,
) *app.WorkerService {
	t.Helper()
	service := testfixture.NewWorkerService(
		t,
		fixture,
		fixture.WorkerRegistry,
		fixture.ResolveOperation,
		nil,
		temporal.observer,
		temporal.timeouts,
	)
	app.SetBeforeLeaseDecision(service, hook.run)
	return service
}

// endLiveTaskLease moves a planning lease that is still live past its end by
// SQL.
func endLiveTaskLease(t *testing.T, fixture *testfixture.Fixture, ref store.TaskRef) {
	t.Helper()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE pm.task_leases
			SET lease_expires_at = clock_timestamp() - interval '1 microsecond'
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND task_id = $4
			  AND lease_expires_at > clock_timestamp()
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, ref.RunID, ref.TaskID)
		return err
	})
}

// holdTaskLease claims the unit's ended task lease for another attempt with
// the given lifetime. It returns the held lease and the database time before
// the claim. It does not call t, so a worker goroutine can run it.
func holdTaskLease(fixture *testfixture.Fixture, ref store.TaskRef, ttl time.Duration) (store.Lease, time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var held store.Lease
	var heldAt time.Time
	err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		expectedFence := uint64(0)
		current, err := store.LoadLease(ctx, tx, ref)
		if err == nil {
			expectedFence = current.Fence
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		heldAt, err = store.ReadAuthenticationTime(ctx, tx)
		if err != nil {
			return err
		}
		held, err = store.ClaimTask(ctx, tx, store.ClaimRequest{
			Task: ref, OwnerID: "held-by-another-attempt", ExpectedFence: expectedFence, TTL: ttl,
		})
		return err
	})
	if err != nil {
		return store.Lease{}, time.Time{}, errors.Join(errors.New("hold the unit's task lease"), err)
	}
	return held, heldAt, nil
}

// setTaskLeaseEnd sets the end of the held task lease to the database time of
// the statement plus remaining. It returns that database time. It does not
// call t, so the lease decision hook can run it inside ExecuteUnit.
func setTaskLeaseEnd(fixture *testfixture.Fixture, ref store.TaskRef, remaining time.Duration) (time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var changedAt time.Time
	err := fixture.WithinSetupSQL(ctx, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			WITH lease_clock AS (SELECT clock_timestamp() AS changed_at)
			UPDATE pm.task_leases AS lease
			SET lease_expires_at = lease_clock.changed_at + make_interval(secs => $5::double precision / 1000000.0)
			FROM lease_clock
			WHERE lease.org_id = $1 AND lease.workspace_id = $2 AND lease.run_id = $3 AND lease.task_id = $4
			  AND lease.owner_id = 'held-by-another-attempt'
			RETURNING lease_clock.changed_at
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, ref.RunID, ref.TaskID, remaining.Microseconds()).Scan(&changedAt)
	})
	if err != nil {
		return time.Time{}, errors.Join(errors.New("change the end of the held task lease"), err)
	}
	return changedAt.UTC(), nil
}

func executeUnitStartedEvent(
	t *testing.T,
	temporal *permitTemporalHarness,
	run permitWorkerRun,
) *historypb.ActivityTaskStartedEventAttributes {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	iterator := temporal.backend.GetWorkflowHistory(
		ctx, run.execution.WorkflowID, run.execution.RunID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT,
	)
	activityTypes := map[int64]string{}
	var started *historypb.ActivityTaskStartedEventAttributes
	for iterator.HasNext() {
		event, err := iterator.Next()
		if err != nil {
			t.Fatalf("read the run's workflow history: %v", err)
		}
		switch event.GetEventType() {
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED:
			activityTypes[event.GetEventId()] = event.GetActivityTaskScheduledEventAttributes().GetActivityType().GetName()
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_STARTED:
			attributes := event.GetActivityTaskStartedEventAttributes()
			if activityTypes[attributes.GetScheduledEventId()] == contract.ExecuteActivityName {
				if started != nil {
					t.Fatal("the run has more than one ExecuteUnit activity")
				}
				started = attributes
			}
		}
	}
	if started == nil {
		t.Fatal("the run's history has no started ExecuteUnit activity")
	}
	return started
}
