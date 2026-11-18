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
// The elapsed time and the remaining budget come only from paired Go
// monotonic samples: the callback return and the ExecuteUnit return. The
// settlement start and the commit are PostgreSQL times. The check reports
// them as durations after a PostgreSQL clock read that the callback makes
// just after it returns, and it names how long that read took. It never
// subtracts a Go duration from a PostgreSQL duration.
func TestSettlementCommitTimeAgainstTheReserve(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	reserve := min(temporal.timeouts.Heartbeat/2, temporal.timeouts.Transport/2)
	type callbackRecord struct {
		deadline   time.Time
		returnedGo time.Time
		returnedDB time.Time
		readEndGo  time.Time
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
			callback.readEndGo = time.Now()
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
	completedGo := time.Now()
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
	measure := settlementMeasure{
		settlementDeadline: settlementDeadline, callbackReturnedGo: callback.returnedGo, unitReturnedGo: completedGo,
		callbackReturnedDB: callback.returnedDB, unitReturnedDB: completedDB,
	}
	t.Logf("settlement reserve %s. Go clock: settlement budget %s at the callback return, ExecuteUnit returned %s after it, "+
		"%s left at that return (%.1f%% of the reserve used). PostgreSQL clock, after the read that ended %s after the callback return: "+
		"settlement start %s, commit %s",
		reserve, measure.budget(), measure.elapsed(), measure.remaining(), 100*float64(measure.elapsed())/float64(reserve),
		callback.readEndGo.Sub(callback.returnedGo), settlementStart.Sub(callback.returnedDB), committed.Sub(callback.returnedDB))
}

// settlementMeasure holds the samples of one settlement measurement. The Go
// samples are monotonic. The PostgreSQL samples come from reads that follow
// the Go samples, so they are later by the time of the read.
type settlementMeasure struct {
	settlementDeadline time.Time // Go clock
	callbackReturnedGo time.Time
	unitReturnedGo     time.Time
	callbackReturnedDB time.Time // PostgreSQL clock, read after callbackReturnedGo
	unitReturnedDB     time.Time // PostgreSQL clock, read after unitReturnedGo
}

// budget is the settlement time left when the callback returned.
func (m settlementMeasure) budget() time.Duration {
	return m.settlementDeadline.Sub(m.callbackReturnedGo)
}

// elapsed is the time from the callback return to the ExecuteUnit return: the
// publication step, the settlement and its commit, and the return.
func (m settlementMeasure) elapsed() time.Duration {
	return m.unitReturnedGo.Sub(m.callbackReturnedGo)
}

// remaining is the settlement time left when ExecuteUnit returned. It uses
// only Go samples. A PostgreSQL duration starts at another instant, so it
// cannot stand in for one of them.
func (m settlementMeasure) remaining() time.Duration {
	return m.settlementDeadline.Sub(m.unitReturnedGo)
}

// The remaining budget must come from aligned samples. brahmagupta's example:
// 1s of budget at the callback return, a PostgreSQL read that ends 300ms later
// and a completion 100ms after that read. Only 600ms remain at the completion.
// A sum that subtracts the 100ms PostgreSQL duration from the 1s Go budget
// reports 900ms. The check needs no stack.
func TestSettlementMeasureUsesAlignedSamples(t *testing.T) {
	start := time.Now()
	database := time.Date(2026, 11, 18, 2, 0, 0, 0, time.UTC)
	measure := settlementMeasure{
		settlementDeadline: start.Add(time.Second),
		callbackReturnedGo: start,
		callbackReturnedDB: database,
		unitReturnedGo:     start.Add(400 * time.Millisecond),
		unitReturnedDB:     database.Add(100 * time.Millisecond),
	}
	if got := measure.remaining(); got != 600*time.Millisecond {
		t.Errorf("remaining at the completion = %s; want 600ms", got)
	}
	if got := measure.elapsed(); got != 400*time.Millisecond {
		t.Errorf("elapsed from the callback return = %s; want 400ms", got)
	}
	if got := measure.budget(); got != time.Second {
		t.Errorf("budget at the callback return = %s; want 1s", got)
	}
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
