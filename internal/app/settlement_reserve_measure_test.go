package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

// TestSettlementCommitTimeAgainstTheReserve measures how long the settlement of
// one unit takes to commit after a provider callback that uses its whole bound,
// against the settlement reserve min(heartbeat/2, transport/2) (issue #281). It
// is a developer measurement on the real stack, not acceptance proof.
//
// ExecuteUnit runs directly, with a context deadline that stands for the
// activity attempt deadline. That deadline is close enough that the settlement
// cutoff, not the transport limit, ends the callback. The callback waits until
// its context ends. The check logs the records and fails only on an error, a
// unit that is not succeeded, or records out of order. A settlement that misses
// its deadline makes ExecuteUnit return an error.
//
// Instants come from two clocks: Go for the deadlines, PostgreSQL for the
// settlement. The check compares durations only, so it needs no clock offset.
func TestSettlementCommitTimeAgainstTheReserve(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	reserve := min(temporal.timeouts.Heartbeat/2, temporal.timeouts.Transport/2)
	type callbackRecord struct {
		deadline   time.Time
		returnedGo time.Time
		returnedDB time.Time
		err        error
	}
	var fixture *testfixture.Fixture
	var callback callbackRecord
	fixture = testfixture.NewExecuting(
		t,
		func(ctx context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
			startedDB, err := settlementMeasureDatabaseTime(fixture)
			if err != nil {
				callback.err = err
				return app.ExecutionObservation{}, err
			}
			callback.deadline, _ = ctx.Deadline()
			<-ctx.Done()
			callback.returnedGo = time.Now()
			callback.returnedDB, callback.err = settlementMeasureDatabaseTime(fixture)
			observation := successfulPermitObservation(request)
			// An observation time before the settlement makes the settled task
			// record the settlement's own database time sample.
			for index := range observation.Outcomes {
				observation.Outcomes[index].ObservedAt = startedDB
			}
			return observation, callback.err
		},
		unexpectedPermitReconciliation,
	)
	service := newPermitWorkerService(t, fixture, temporal)
	admitted := admitPermitRun(
		t, fixture, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID, "settlement-reserve-measure",
	)
	run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)

	attemptDeadline := time.Now().Add(2*reserve + 2*time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), attemptDeadline)
	defer cancel()
	result, err := executePermitWorkerUnitContext(ctx, service, run)
	completedDB, completedErr := settlementMeasureDatabaseTime(fixture)
	if err != nil || callback.err != nil || completedErr != nil {
		t.Fatalf("ExecuteUnit: %v; callback: %v; database time after ExecuteUnit: %v", err, callback.err, completedErr)
	}
	if result.State != contract.RunStateSucceeded {
		t.Fatalf("ExecuteUnit result = %#v; want a succeeded unit", result)
	}
	// The settlement cutoff is one reserve before the attempt deadline. The
	// callback bound is one reserve before the cutoff.
	settlementDeadline := attemptDeadline.Add(-reserve)
	if !callback.deadline.Equal(settlementDeadline.Add(-reserve)) {
		t.Fatalf("callback deadline = %s; want %s, one reserve before the settlement cutoff %s",
			callback.deadline, settlementDeadline.Add(-reserve), settlementDeadline)
	}

	var settlementStart, committed time.Time
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT (task->>'projection_observed_at')::timestamptz, updated_at
			FROM pm.tasks
			WHERE run_id = $1 AND task_id = $2
		`, run.reference.RunID, run.plan.TaskID).Scan(&settlementStart, &committed)
	})
	if settlementStart.Before(callback.returnedDB) || committed.Before(settlementStart) || completedDB.Before(committed) {
		t.Fatalf("settlement records out of order: callback return %s, settlement start %s, commit %s, completion %s",
			callback.returnedDB, settlementStart, committed, completedDB)
	}
	budget := settlementDeadline.Sub(callback.returnedGo)
	completion := completedDB.Sub(callback.returnedDB)
	t.Logf("settlement reserve %s; after the callback return: settlement start %s, commit %s, completion at most %s; "+
		"settlement budget %s, remaining at completion at least %s (%.1f%% of the reserve used)",
		reserve, settlementStart.Sub(callback.returnedDB), committed.Sub(callback.returnedDB), completion,
		budget, budget-completion, 100*float64(completion)/float64(reserve))
}

// settlementMeasureDatabaseTime reads the PostgreSQL clock with its own bound. It
// does not call t, so the provider callback can run it.
func settlementMeasureDatabaseTime(fixture *testfixture.Fixture) (time.Time, error) {
	if fixture == nil {
		return time.Time{}, errors.New("settlement measurement fixture is not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var now time.Time
	err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		now, err = store.ReadAuthenticationTime(ctx, tx)
		return err
	})
	return now, err
}
