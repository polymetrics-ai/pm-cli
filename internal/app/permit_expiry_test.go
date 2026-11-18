package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// The checks in this file cover the expiry of a reserved account permit that
// no settlement or retry releases. A reservation that finds the provider
// account short of capacity expires its reserved permits whose expiry has
// passed. An expired permit proves that no send was admitted, so the unit that
// held it settles unsent and never sends. They reuse the fixture of
// unadmitted_recovery_test.go. SQL, a scoped trigger and channels force the
// order. No check sleeps, polls or retries.

// TestLocalResultWithAnExpiredPermitRecoversOnRetry expires the permit of a
// local-tail unit after its callback and before its settlement. The settlement
// must write nothing and fail retryably. The retry then settles every member
// unsent from the expired permit, with no capacity change.
func TestLocalResultWithAnExpiredPermitRecoversOnRetry(t *testing.T) {
	check := newUnadmittedCheck(t, "fixture.http.expired-local-result")
	tailToken, _ := check.produceTokens(t, "expired-local-result")
	baseline := providerAccountReservedUnits(t, check.fixture)
	calls := check.providerCalls.Load()
	run := check.admitResume(t, "expired-local-result-resume", tailToken)
	check.setHook(func(ctx context.Context, request app.ExecutionRequest, execute app.ExecutionCallback) (app.ExecutionObservation, error) {
		observation, err := execute(ctx, request)
		movePermitPastExpiry(t, check.fixture, request.Run.RunID)
		expireRunPermit(t, check.fixture, request.Run.RunID)
		return observation, err
	})
	unit, err := executePermitWorkerUnit(t, check.service, run)
	check.setHook(nil)
	if !errors.Is(err, store.ErrAccountPermitStale) || errors.Is(err, app.ErrWorkerConflict) {
		t.Fatalf("local result with an expired permit = %#v error=%v cause=%v; want a retryable stale permit",
			unit, err, errors.Unwrap(err))
	}
	if outcomes := unadmittedOutcomes(t, check.fixture, run); len(outcomes) != 0 {
		t.Fatalf("settlement with an expired permit wrote member outcomes %#v", outcomes)
	}
	assertLatestRunPermitState(t, check.fixture, run.reference.RunID, store.AccountPermitExpired)

	expireWorkerTaskLease(t, check.fixture, run)
	unit, err = executePermitWorkerUnit(t, check.service, run)
	check.requireUnsentSettlement(t, run, unit, err, store.AccountPermitExpired, baseline, calls)
	requireSettlementReason(t, check.fixture, run.reference.RunID, unadmittedRecoveryReason, true)
}

// TestLeakedPermitCapacityReturnsToTheNextUnit leaves one continuation unit
// with a reserved permit after a failed settlement, on an account with
// capacity one. When the permit has expired, the next unit on the account must
// get the capacity. The leaked unit's retry then settles unsent, and the
// capacity is released once.
func TestLeakedPermitCapacityReturnsToTheNextUnit(t *testing.T) {
	check := newUnadmittedCheck(t, "fixture.http.leaked-permit-capacity")
	tailToken, _ := check.produceTokens(t, "leaked-permit-capacity")
	setPermitTestAccountCapacity(t, check.fixture, 1)
	calls := check.providerCalls.Load()

	leaked, baseline := check.failLocalTailSettlement(t, "leaked-permit-capacity-leaked", tailToken)
	movePermitPastExpiry(t, check.fixture, leaked.reference.RunID)

	next := check.admitResume(t, "leaked-permit-capacity-next", tailToken)
	nextUnit, err := executePermitWorkerUnit(t, check.service, next)
	if err != nil || nextUnit.State != contract.RunStateSucceeded || check.providerCalls.Load() != calls {
		t.Fatalf("next unit on the account = %#v error=%v provider_calls=%d; want a local success",
			nextUnit, err, check.providerCalls.Load())
	}
	assertLatestRunPermitState(t, check.fixture, leaked.reference.RunID, store.AccountPermitExpired)
	if got := providerAccountReservedUnits(t, check.fixture); got != baseline {
		t.Fatalf("reserved units after the next unit = %d; want %d", got, baseline)
	}

	unit, err := executePermitWorkerUnit(t, check.service, leaked)
	check.requireUnsentSettlement(t, leaked, unit, err, store.AccountPermitExpired, baseline, calls)
}

// TestExpiredPermitCannotAdmitABlockedAttempt holds a continuation unit that
// needs a provider send before its admission, on an account with capacity one.
// Its permit expires, and the next unit's reservation takes the capacity. When
// the held unit continues, the physical-call boundary must refuse its send,
// and the unit settles unsent with no provider call.
func TestExpiredPermitCannotAdmitABlockedAttempt(t *testing.T) {
	check := newUnadmittedCheck(t, "fixture.http.expired-blocked-attempt")
	tailToken, sendToken := check.produceTokens(t, "expired-blocked-attempt")
	setPermitTestAccountCapacity(t, check.fixture, 1)
	baseline := providerAccountReservedUnits(t, check.fixture)
	calls := check.providerCalls.Load()

	held := check.admitResume(t, "expired-blocked-attempt-held", sendToken)
	next := check.admitResume(t, "expired-blocked-attempt-next", tailToken)
	entered := make(chan struct{})
	proceed := make(chan struct{})
	var heldAdmission error
	check.setHook(func(ctx context.Context, request app.ExecutionRequest, execute app.ExecutionCallback) (app.ExecutionObservation, error) {
		if request.Run.RunID != held.reference.RunID {
			return execute(ctx, request)
		}
		close(entered)
		<-proceed
		call, admitter, _, err := request.PhysicalCallAuthority()
		if err == nil {
			err = admitter.AdmitPhysicalCall(ctx, call)
		}
		heldAdmission = err
		if err == nil {
			return app.ExecutionObservation{}, errors.New("controlled held unit was admitted")
		}
		return execute(ctx, request)
	})
	type heldResult struct {
		unit contract.WorkflowUnitResult
		err  error
	}
	done := make(chan heldResult, 1)
	go func() {
		unit, err := executePermitWorkerUnit(t, check.service, held)
		done <- heldResult{unit: unit, err: err}
	}()
	released := false
	release := func() heldResult {
		released = true
		close(proceed)
		return <-done
	}
	defer func() {
		if !released {
			release()
		}
		check.setHook(nil)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("held unit did not reach its callback")
	}
	movePermitPastExpiry(t, check.fixture, held.reference.RunID)

	nextUnit, err := executePermitWorkerUnit(t, check.service, next)
	if err != nil || nextUnit.State != contract.RunStateSucceeded {
		t.Fatalf("next unit while the held permit is expired = %#v error=%v; want a local success", nextUnit, err)
	}
	assertLatestRunPermitState(t, check.fixture, held.reference.RunID, store.AccountPermitExpired)

	result := release()
	if !errors.Is(heldAdmission, app.ErrWorkerAuthorization) ||
		errors.Is(heldAdmission, context.Canceled) || errors.Is(heldAdmission, context.DeadlineExceeded) {
		t.Fatalf("admission of the held unit = %v cause=%v; want a refusal of its expired permit",
			heldAdmission, errors.Unwrap(heldAdmission))
	}
	check.requireUnsentSettlement(t, held, result.unit, result.err, store.AccountPermitExpired, baseline, calls)
}
