package app_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

// TestHeldLeaseRetryStartsWhenTheLeaseEnds holds the task lease of a planned
// unit for another attempt before a real worker starts the run. The first
// ExecuteUnit attempt then finds the lease held and fails. Its retry must wait
// for the remaining lease time on the database clock, not for Temporal's
// default backoff, so the second attempt claims the task and succeeds.
//
// PostgreSQL and Temporal are the real stack. The held lease is a durable row
// with a known end. The check reads the activity's last failure from the
// workflow history after the run ends, so it does not sleep, poll or retry.
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
	service := newPermitWorkerService(t, fixture, temporal)
	admitted := admitPermitRun(
		t, fixture, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID, "held-lease-retry",
	)
	run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
	held, heldAt := holdPermitTestTaskLease(t, fixture, run, 6*time.Second)

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

// holdPermitTestTaskLease claims the unit's task lease for another attempt with
// the given lifetime. A planning lease that is still live is first moved past
// its end by SQL. It returns the held lease and the database time before the
// claim.
func holdPermitTestTaskLease(
	t *testing.T,
	fixture *testfixture.Fixture,
	run permitWorkerRun,
	ttl time.Duration,
) (store.Lease, time.Time) {
	t.Helper()
	ref := store.TaskRef{RunID: run.reference.RunID, TaskID: run.plan.TaskID}
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE pm.task_leases
			SET lease_expires_at = clock_timestamp() - interval '1 microsecond'
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND task_id = $4
			  AND lease_expires_at > clock_timestamp()
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, ref.RunID, ref.TaskID)
		return err
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var held store.Lease
	var heldAt time.Time
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
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
	}); err != nil {
		t.Fatalf("hold the unit's task lease: %v", err)
	}
	return held, heldAt
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
