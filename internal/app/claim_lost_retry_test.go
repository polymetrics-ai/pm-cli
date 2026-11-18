package app_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
// hook runs in the prepare transaction after every preparation step, after the
// lease decision samples the database time and just before the stored lease is
// read. The hook holds the lease once and sets its end 6 s after that sample.
// So, within the time limits of the check, the first lease decision sees a
// live lease with exactly 6 s left, however long the worker takes to start,
// the preparation takes and the hook takes. The check also requires that the
// decision's sample is not before a database time that the check reads itself
// during preparation (leaseDecisionHook.result). The check reads the
// activity's last failure from the workflow history after the run ends, so it
// does not sleep, poll or retry.
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
	var heldAt, heldUntil time.Time
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
	hook.action = func(_ context.Context, decidedAt time.Time) error {
		if err := holdTaskLease(fixture, ref, 6*time.Second); err != nil {
			return err
		}
		var err error
		heldAt = decidedAt
		heldUntil, err = setTaskLeaseEnd(fixture, ref, decidedAt, 6*time.Second)
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
	maximum := heldUntil.Sub(heldAt)
	delay := failure.GetNextRetryDelay()
	if delay == nil || delay.AsDuration() <= 0 || delay.AsDuration() > maximum {
		t.Fatalf("claim-lost retry delay = %v (attempt %d, failure type %q); want the remaining lease time, above 0 and at most %s",
			delay, started.GetAttempt(), failure.GetType(), maximum)
	}
	// The lease ends exactly maximum after the database time of the lease
	// decision, so the remaining lease time is exactly maximum.
	if delay.AsDuration() != maximum {
		t.Fatalf("claim-lost retry delay = %s; want exactly %s, the time from the lease decision's database time %s to the lease end",
			delay.AsDuration(), maximum, heldAt)
	}
	if started.GetAttempt() != 2 {
		t.Fatalf("ExecuteUnit attempt = %d after a retry delay of %s; want the second attempt to claim the ended lease",
			started.GetAttempt(), delay.AsDuration())
	}
}

// TestLeaseDecisionReadsTheDatabaseTimeAgain checks the lease decision of
// ExecuteUnit when preparation outlasts a held lease. ExecuteUnit samples the
// database time when it loads the run. The lease decision samples it again
// later, in the same prepare transaction, after every preparation step. Its
// lease decision hook runs after that second sample and just before the stored
// lease is read, and it gets the sample. Here it sets the end of the held lease
// relative to that sample, so the stored end is after the first sample. The
// lease decision must use the second sample for both the expiry comparison and
// the remaining lease time. Because the lease end follows the decision's own
// sample, the check also reads its own database time during preparation, after
// the first sample. The decision's sample must not be before it
// (leaseDecisionHook.result), so a decision that used the first sample fails.
// No sleep or poll sets the order: the operation resolver call and the hook
// call are the events. Within the time limits of the check, the time that the
// hook and the lease read take does not change the result.
func TestLeaseDecisionReadsTheDatabaseTimeAgain(t *testing.T) {
	temporal := newPermitTemporalHarness(t)

	t.Run("the lease ends during preparation: the unit claims the task", func(t *testing.T) {
		run, fixture, service, hook, sends := prepareLeaseDecisionRun(t, temporal, "lease-ends-in-preparation")
		ref := store.TaskRef{RunID: run.reference.RunID, TaskID: run.plan.TaskID}
		if err := holdTaskLease(fixture, ref, time.Minute); err != nil {
			t.Fatalf("hold the unit's task lease: %v", err)
		}
		var endedAt time.Time
		hook.action = func(_ context.Context, decidedAt time.Time) error {
			var err error
			endedAt, err = setTaskLeaseEnd(fixture, ref, decidedAt, 0)
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
		if err := holdTaskLease(fixture, ref, time.Minute); err != nil {
			t.Fatalf("hold the unit's task lease: %v", err)
		}
		const remaining = 2 * time.Second
		var changedAt time.Time
		hook.action = func(_ context.Context, decidedAt time.Time) error {
			changedAt = decidedAt
			_, err := setTaskLeaseEnd(fixture, ref, decidedAt, remaining)
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
		// changedAt is the lease decision's database time, after the first
		// sample. The lease ends exactly remaining after changedAt.
		if held.RetryAfter <= 0 || held.RetryAfter > remaining {
			t.Fatalf("held-lease retry delay = %s; want above 0 and at most %s, the time left when the lease end changed at %s",
				held.RetryAfter, remaining, changedAt)
		}
		if held.RetryAfter != remaining {
			t.Fatalf("held-lease retry delay = %s; want exactly %s, the time from the lease decision's database time %s to the lease end",
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
// ExecuteUnit, after the lease decision samples the database time and just
// before it reads the stored lease (app.SetBeforeLeaseDecision). That is after
// every other preparation step. The action gets the decision's database time,
// so it can set the lease relative to the time that the decision uses. The
// hook runs its action only once, and only after the test arms it.
//
// The hook also wraps the operation resolver (resolve). ExecuteUnit calls the
// resolver during preparation: after it samples the database time when it
// loads the run, and before the lease decision samples it again. There the
// check reads its own database time (preparedAt). It does not come from the
// worker, so it is an independent lower bound for the decision's sample.
type leaseDecisionHook struct {
	armed  atomic.Bool
	fired  atomic.Bool
	match  func(context.Context) bool
	action func(context.Context, time.Time) error
	err    error
	done   chan struct{}

	mu         sync.Mutex
	preparedAt time.Time
	decidedAt  time.Time
}

func newLeaseDecisionHook(
	match func(context.Context) bool,
	action func(context.Context, time.Time) error,
) *leaseDecisionHook {
	return &leaseDecisionHook{match: match, action: action, done: make(chan struct{})}
}

// resolve returns the fixture's operation resolver with one addition: while
// the hook is armed and has not fired, each matching call first reads the
// database time in its own transaction and keeps it as preparedAt.
func (hook *leaseDecisionHook) resolve(fixture *testfixture.Fixture) app.OperationResolver {
	return func(ctx context.Context, scope contract.Scope, connectionID, operationID string) (app.OperationSelection, error) {
		if hook.armed.Load() && !hook.fired.Load() && hook.match(ctx) {
			preparedAt, err := preparationDatabaseTime(fixture)
			if err != nil {
				return app.OperationSelection{}, err
			}
			hook.mu.Lock()
			hook.preparedAt = preparedAt
			hook.mu.Unlock()
		}
		return fixture.ResolveOperation(ctx, scope, connectionID, operationID)
	}
}

func (hook *leaseDecisionHook) run(ctx context.Context, decidedAt time.Time) error {
	if hook.armed.Load() && hook.match(ctx) && hook.fired.CompareAndSwap(false, true) {
		hook.mu.Lock()
		hook.decidedAt = decidedAt
		hook.mu.Unlock()
		hook.err = hook.action(ctx, decidedAt)
		close(hook.done)
		return hook.err
	}
	return nil
}

// result returns the error of the action. It fails when the action did not
// run, so a test cannot pass without the event that sets its order. It also
// fails when the decision's database time is before preparedAt, the database
// time that the check read during preparation. A lease decision that uses the
// time that ExecuteUnit sampled when it loaded the run fails here, although
// the lease end follows the decision's time.
func (hook *leaseDecisionHook) result() error {
	select {
	case <-hook.done:
	default:
		return errors.New("ExecuteUnit made no lease decision after the lease decision hook was armed")
	}
	if hook.err != nil {
		return errors.Join(errors.New("lease decision hook failed"), hook.err)
	}
	hook.mu.Lock()
	defer hook.mu.Unlock()
	if hook.preparedAt.IsZero() {
		return errors.New("the check read no database time during the preparation of ExecuteUnit")
	}
	if hook.decidedAt.Before(hook.preparedAt) {
		return fmt.Errorf("lease decision database time %s is before %s, a database time that the check read during preparation; "+
			"the lease decision must read the database time again after preparation", hook.decidedAt, hook.preparedAt)
	}
	return nil
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
		hook.resolve(fixture),
		nil,
		temporal.observer,
		temporal.timeouts,
	)
	app.SetBeforeLeaseDecision(service, hook.run)
	return service
}

// preparationDatabaseTime reads the database time in its own transaction. It
// does not call t, so the operation resolver can run it inside ExecuteUnit.
func preparationDatabaseTime(fixture *testfixture.Fixture) (time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var at time.Time
	err := fixture.WithinSetupSQL(ctx, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at)
	})
	if err != nil {
		return time.Time{}, errors.Join(errors.New("read the database time during preparation"), err)
	}
	return at.UTC(), nil
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
// the given lifetime. It does not call t, so a worker goroutine can run it.
func holdTaskLease(fixture *testfixture.Fixture, ref store.TaskRef, ttl time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		expectedFence := uint64(0)
		current, err := store.LoadLease(ctx, tx, ref)
		if err == nil {
			expectedFence = current.Fence
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		_, err = store.ClaimTask(ctx, tx, store.ClaimRequest{
			Task: ref, OwnerID: "held-by-another-attempt", ExpectedFence: expectedFence, TTL: ttl,
		})
		return err
	})
	if err != nil {
		return errors.Join(errors.New("hold the unit's task lease"), err)
	}
	return nil
}

// setTaskLeaseEnd sets the end of the held task lease to from plus remaining,
// where from is the lease decision's database time. It returns the stored end.
// It does not call t, so the lease decision hook can run it inside
// ExecuteUnit.
func setTaskLeaseEnd(
	fixture *testfixture.Fixture,
	ref store.TaskRef,
	from time.Time,
	remaining time.Duration,
) (time.Time, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var endsAt time.Time
	err := fixture.WithinSetupSQL(ctx, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			UPDATE pm.task_leases AS lease
			SET lease_expires_at = $5::timestamptz + make_interval(secs => $6::double precision / 1000000.0)
			WHERE lease.org_id = $1 AND lease.workspace_id = $2 AND lease.run_id = $3 AND lease.task_id = $4
			  AND lease.owner_id = 'held-by-another-attempt'
			RETURNING lease.lease_expires_at
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, ref.RunID, ref.TaskID, from, remaining.Microseconds()).Scan(&endsAt)
	})
	if err != nil {
		return time.Time{}, errors.Join(errors.New("change the end of the held task lease"), err)
	}
	return endsAt.UTC(), nil
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
