package app_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

// The checks in this file cover a continuation unit whose account permit is
// still reserved when its settlement does not commit. A continuation unit
// leaves its permit reserved, and only the physical-call boundary changes it
// to dispatched before a send. So a permit that is still reserved proves that
// no send was admitted.
//
// PostgreSQL and Temporal are the real stack. The provider and the object
// store are controlled peers. A scoped trigger, SQL state changes and
// callback hooks force the order. No check sleeps, polls or retries.

const unadmittedRecoveryReason = "unadmitted_attempt_recovered"

// TestUnadmittedContinuationRecoversAfterAFailedSettlement runs a unit that
// completes from the unread tail of a continuation. Its settlement fails, so
// the permit stays reserved after the recorded attempt. The retry must settle
// every member unsent from the permit witness, with no provider call, and
// release the permit once.
func TestUnadmittedContinuationRecoversAfterAFailedSettlement(t *testing.T) {
	check := newUnadmittedCheck(t, "fixture.http.unadmitted-recovery")
	token, _ := check.produceTokens(t, "unadmitted-recovery")
	calls := check.providerCalls.Load()

	run, baseline := check.failLocalTailSettlement(t, "unadmitted-recovery-resume", token)
	unit, err := executePermitWorkerUnit(t, check.service, run)
	check.requireUnsentSettlement(t, run, unit, err, store.AccountPermitReleased, baseline, calls)
	requireSettlementReason(t, check.fixture, run.reference.RunID, unadmittedRecoveryReason, true)

	again, err := executePermitWorkerUnit(t, check.service, run)
	if err != nil || again != unit {
		t.Fatalf("repeated retry = %#v error=%v; want the stored settlement %#v", again, err, unit)
	}
	check.requireUnsentSettlement(t, run, again, err, store.AccountPermitReleased, baseline, calls)

	resumed := check.admitResume(t, "unadmitted-recovery-resubmit", token)
	resumedUnit, err := executePermitWorkerUnit(t, check.service, resumed)
	if err != nil || resumedUnit.State != contract.RunStateSucceeded || check.providerCalls.Load() != calls {
		t.Fatalf("same token after the recovery = %#v error=%v provider_calls=%d; want a local-tail success and %d calls",
			resumedUnit, err, check.providerCalls.Load(), calls)
	}
	assertDurableContinuationIDs(t, loadDurableContinuationResult(t, check.fixture, resumed.reference.RunID), 3)
}

// TestUnadmittedContinuationRecoversWithAnExpiredPermit repeats the failed
// settlement, then moves the reserved permit past its expiry. The retry must
// settle unsent in both permit states and change provider-account capacity at
// most once.
func TestUnadmittedContinuationRecoversWithAnExpiredPermit(t *testing.T) {
	check := newUnadmittedCheck(t, "fixture.http.unadmitted-expiry")
	token, _ := check.produceTokens(t, "unadmitted-expiry")

	t.Run("reserved past expiry", func(t *testing.T) {
		calls := check.providerCalls.Load()
		run, baseline := check.failLocalTailSettlement(t, "unadmitted-expiry-reserved", token)
		movePermitPastExpiry(t, check.fixture, run.reference.RunID)
		unit, err := executePermitWorkerUnit(t, check.service, run)
		check.requireUnsentSettlement(t, run, unit, err, store.AccountPermitReleased, baseline, calls)
	})

	t.Run("expired state", func(t *testing.T) {
		calls := check.providerCalls.Load()
		run, baseline := check.failLocalTailSettlement(t, "unadmitted-expiry-expired", token)
		movePermitPastExpiry(t, check.fixture, run.reference.RunID)
		expireRunPermit(t, check.fixture, run.reference.RunID)
		if got := providerAccountReservedUnits(t, check.fixture); got != baseline {
			t.Fatalf("reserved units after the permit expiry = %d; want %d", got, baseline)
		}
		unit, err := executePermitWorkerUnit(t, check.service, run)
		check.requireUnsentSettlement(t, run, unit, err, store.AccountPermitExpired, baseline, calls)
	})
}

// TestUnsentContinuationReleasesItsReservedPermit runs a continuation unit
// that needs a provider send. It revokes the run grant after the unit is
// prepared, so the physical-call boundary refuses the send. The callback
// reports every member unsent. The settlement must release the reserved permit
// at once instead of keeping it until an expiry.
func TestUnsentContinuationReleasesItsReservedPermit(t *testing.T) {
	check := newUnadmittedCheck(t, "fixture.http.unsent-release")
	_, sendToken := check.produceTokens(t, "unsent-release")
	baseline := providerAccountReservedUnits(t, check.fixture)
	calls := check.providerCalls.Load()
	run := check.admitResume(t, "unsent-release-next-page", sendToken)
	check.setHook(func(ctx context.Context, request app.ExecutionRequest, execute app.ExecutionCallback) (app.ExecutionObservation, error) {
		revokePermitTestAuthority(t, check.fixture, request.Run.RunID, "grant")
		return execute(ctx, request)
	})
	unit, err := executePermitWorkerUnit(t, check.service, run)
	check.setHook(nil)
	check.requireUnsentSettlement(t, run, unit, err, store.AccountPermitReleased, baseline, calls)
	requireSettlementReason(t, check.fixture, run.reference.RunID, unadmittedRecoveryReason, false)
}

// TestWorkerMadeUnknownUsesThePermitWitness covers a callback that returns
// no member outcomes. The worker makes every member unknown. With a reserved
// permit, the permit proves that no send was admitted, so the unit settles
// unsent. With a dispatched permit, the members stay unknown. A callback that
// reports unknown itself with a reserved permit stays a conflict.
func TestWorkerMadeUnknownUsesThePermitWitness(t *testing.T) {
	check := newUnadmittedCheck(t, "fixture.http.worker-made-unknown")
	_, sendToken := check.produceTokens(t, "worker-made-unknown")

	t.Run("reserved permit settles unsent", func(t *testing.T) {
		baseline := providerAccountReservedUnits(t, check.fixture)
		calls := check.providerCalls.Load()
		run := check.admitResume(t, "worker-made-unknown-reserved", sendToken)
		check.setHook(func(context.Context, app.ExecutionRequest, app.ExecutionCallback) (app.ExecutionObservation, error) {
			return app.ExecutionObservation{}, errors.New("controlled callback failure before the HTTP runtime")
		})
		unit, err := executePermitWorkerUnit(t, check.service, run)
		check.setHook(nil)
		check.requireUnsentSettlement(t, run, unit, err, store.AccountPermitReleased, baseline, calls)
		requireSettlementReason(t, check.fixture, run.reference.RunID, unadmittedRecoveryReason, true)
		requireSettlementReason(t, check.fixture, run.reference.RunID, "callback_error_observed", true)
	})

	t.Run("dispatched permit stays unknown", func(t *testing.T) {
		baseline := providerAccountReservedUnits(t, check.fixture)
		calls := check.providerCalls.Load()
		run := check.admitResume(t, "worker-made-unknown-dispatched", sendToken)
		var admitErr error
		check.setHook(func(ctx context.Context, request app.ExecutionRequest, _ app.ExecutionCallback) (app.ExecutionObservation, error) {
			call, admitter, _, err := request.PhysicalCallAuthority()
			if err == nil {
				err = admitter.AdmitPhysicalCall(ctx, call)
			}
			admitErr = err
			return app.ExecutionObservation{}, errors.New("controlled callback failure after the admission")
		})
		unit, err := executePermitWorkerUnit(t, check.service, run)
		check.setHook(nil)
		if admitErr != nil || err != nil || unit.State != contract.RunStateReconciling || unit.UnknownEffectCount != 1 {
			t.Fatalf("admitted unit with no outcomes = %#v error=%v admission=%v; want reconciling with one unknown",
				unit, err, admitErr)
		}
		for _, outcome := range unadmittedOutcomes(t, check.fixture, run) {
			if outcome.State != contract.OutcomeUnknown {
				t.Fatalf("member outcome after a dispatched permit = %#v; want unknown", outcome)
			}
		}
		assertLatestRunPermitState(t, check.fixture, run.reference.RunID, store.AccountPermitUnknown)
		if got := providerAccountReservedUnits(t, check.fixture); got != baseline+1 || check.providerCalls.Load() != calls {
			t.Fatalf("reserved units/provider calls = %d/%d; want %d/%d", got, check.providerCalls.Load(), baseline+1, calls)
		}
		requireSettlementReason(t, check.fixture, run.reference.RunID, unadmittedRecoveryReason, false)
	})

	t.Run("callback-reported unknown stays a conflict", func(t *testing.T) {
		baseline := providerAccountReservedUnits(t, check.fixture)
		run := check.admitResume(t, "callback-reported-unknown", sendToken)
		check.setHook(func(_ context.Context, request app.ExecutionRequest, _ app.ExecutionCallback) (app.ExecutionObservation, error) {
			observedAt := time.Now().UTC().Truncate(time.Microsecond)
			outcomes := make([]contract.MemberOutcome, 0, len(request.Dispatch.OrderedMemberIDs))
			for _, memberID := range request.Dispatch.OrderedMemberIDs {
				outcomes = append(outcomes, contract.MemberOutcome{
					MemberID: memberID, EffectKey: request.Dispatch.EffectKey, DispatchID: request.Dispatch.DispatchID,
					State: contract.OutcomeUnknown, ObservedAt: observedAt,
					AssertionRefs: []string{request.Attempt.ReceiptRef},
				})
			}
			return app.ExecutionObservation{Outcomes: outcomes}, nil
		})
		unit, err := executePermitWorkerUnit(t, check.service, run)
		check.setHook(nil)
		if !errors.Is(err, app.ErrWorkerConflict) {
			t.Fatalf("callback-reported unknown with a reserved permit = %#v error=%v; want a conflict", unit, err)
		}
		if outcomes := unadmittedOutcomes(t, check.fixture, run); len(outcomes) != 0 {
			t.Fatalf("conflicting unit wrote member outcomes %#v", outcomes)
		}
		assertLatestRunPermitState(t, check.fixture, run.reference.RunID, store.AccountPermitReserved)
		if got := providerAccountReservedUnits(t, check.fixture); got != baseline+1 {
			t.Fatalf("reserved units after the conflict = %d; want %d", got, baseline+1)
		}
	})
}

// unadmittedHook replaces or wraps the real HTTP direct-read callback.
type unadmittedHook func(context.Context, app.ExecutionRequest, app.ExecutionCallback) (app.ExecutionObservation, error)

type unadmittedCheck struct {
	fixture       *testfixture.Fixture
	temporal      *permitTemporalHarness
	service       *app.WorkerService
	selection     app.OperationSelection
	providerCalls *atomic.Int32
	hook          atomic.Pointer[unadmittedHook]
}

func newUnadmittedCheck(t *testing.T, operationID string) *unadmittedCheck {
	t.Helper()
	check := &unadmittedCheck{temporal: newPermitTemporalHarness(t), providerCalls: &atomic.Int32{}}
	objectStore := newGatedObjectStore(t)
	objectStore.open()
	origin, tlsConfig, dialer := newHTTPDirectReadTLSPeer(t, publicationCheckProvider(check.providerCalls))
	authority := &durableContinuationAuthority{
		selection: durableHTTPContinuationSelection(operationID, origin),
		policy:    httpDirectReadPolicy(origin),
	}
	check.selection = authority.selection
	dependencies := app.HTTPDirectReadDependencies{
		ClientIdentity: testClientIdentity, Resolver: httpDirectReadResolver{}, Dialer: dialer, TLSConfig: tlsConfig,
	}
	check.fixture, _ = newDurableContinuationFixture(
		t, authority, dependencies, objectStore.resources, objectStore.resources, objectStore.limits,
	)
	publisher, err := objects.NewPublisher(check.fixture.Pool, objectStore.resources.Backend, objectStore.limits)
	if err != nil {
		t.Fatal(err)
	}
	continuations, err := app.NewContinuationService(
		check.fixture.Pool, publisher, objectStore.resources.Backend, durableContinuationObjectBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	dependencies.Continuations = continuations
	registration := app.NewHTTPDirectReadRegistration(dependencies)
	execute := registration.Execute
	registration.Execute = func(ctx context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
		if hook := check.hook.Load(); hook != nil {
			return (*hook)(ctx, request, execute)
		}
		return execute(ctx, request)
	}
	registry := &app.HandlerRegistry{}
	if err := registry.RegisterHTTP(registration); err != nil {
		t.Fatal(err)
	}
	check.fixture.WorkerRegistry = registry
	check.service = workerResultService(t, check.fixture, check.temporal, publisher, durableContinuationObjectBytes)
	return check
}

func (check *unadmittedCheck) setHook(hook unadmittedHook) {
	if hook == nil {
		check.hook.Store(nil)
		return
	}
	check.hook.Store(&hook)
}

func (check *unadmittedCheck) admit(
	t *testing.T,
	requestKey string,
	input contract.PresentValue,
	controls contract.ResultControls,
) permitWorkerRun {
	t.Helper()
	admitted := admitDurableContinuationRun(
		t, check.fixture, check.selection, check.fixture.Client, check.fixture.Scope,
		check.fixture.Connection.ConnectionID, requestKey, input, controls,
	)
	return preparePermitWorkerRun(t, check.fixture, check.service, check.temporal, check.fixture.Scope, check.fixture.Actor, admitted)
}

// admitFirstPage admits a first-page unit with a record cap of two. The
// controlled provider returns three records, so a sent page keeps one record
// in its unread tail.
func (check *unadmittedCheck) admitFirstPage(t *testing.T, requestKey string) permitWorkerRun {
	t.Helper()
	recordCap := uint64(2)
	return check.admit(t, requestKey, durableContinuationInput(t, "chain", 1), contract.ResultControls{TotalRecordCap: &recordCap})
}

func (check *unadmittedCheck) admitResume(t *testing.T, requestKey, token string) permitWorkerRun {
	t.Helper()
	return check.admit(t, requestKey, contract.NewObjectValue(map[string]contract.PresentValue{}), continuationControls(token))
}

// produceTokens runs a first-page unit, then a unit that serves the unread
// tail of its continuation. It returns the first token, whose unread tail
// holds one record, and the second token, whose next unit needs a provider
// send. Only a unit with a continuation token leaves its permit reserved.
func (check *unadmittedCheck) produceTokens(t *testing.T, keyPrefix string) (string, string) {
	t.Helper()
	before := check.providerCalls.Load()
	first := check.admitFirstPage(t, keyPrefix+"-first-page")
	unit, err := executePermitWorkerUnit(t, check.service, first)
	if err != nil || unit.State != contract.RunStateSucceeded || check.providerCalls.Load() != before+1 {
		t.Fatalf("first-page unit = %#v error=%v provider_calls=%d; want one succeeded provider read",
			unit, err, check.providerCalls.Load())
	}
	tailToken := requireDurableContinuationToken(t, loadDurableContinuationResult(t, check.fixture, first.reference.RunID))
	tail := check.admitResume(t, keyPrefix+"-unread-tail", tailToken)
	unit, err = executePermitWorkerUnit(t, check.service, tail)
	if err != nil || unit.State != contract.RunStateSucceeded || check.providerCalls.Load() != before+1 {
		t.Fatalf("unread-tail unit = %#v error=%v provider_calls=%d; want a local success with no provider call",
			unit, err, check.providerCalls.Load())
	}
	sendToken := requireDurableContinuationToken(t, loadDurableContinuationResult(t, check.fixture, tail.reference.RunID))
	return tailToken, sendToken
}

// failLocalTailSettlement prepares a unit that resumes from the unread tail of
// token, and makes its first settlement fail with a scoped trigger. It
// returns the unit with its task lease expired, ready for the retry, and the
// provider-account reservation before the unit.
func (check *unadmittedCheck) failLocalTailSettlement(t *testing.T, requestKey, token string) (permitWorkerRun, int64) {
	t.Helper()
	baseline := providerAccountReservedUnits(t, check.fixture)
	calls := check.providerCalls.Load()
	run := check.admitResume(t, requestKey, token)
	remove := installWorkerSettlementFailure(t, check.fixture, run.reference.RunID)
	unit, err := executePermitWorkerUnit(t, check.service, run)
	remove()
	if err == nil || errors.Is(err, app.ErrWorkerConflict) {
		t.Fatalf("local-tail unit whose settlement failed = %#v error=%v; want a retryable settlement failure", unit, err)
	}
	if outcomes := unadmittedOutcomes(t, check.fixture, run); len(outcomes) != 0 {
		t.Fatalf("failed settlement wrote member outcomes %#v", outcomes)
	}
	_, attempts := permitTestDurableCounts(t, check.fixture, check.fixture.Scope, run.reference.RunID)
	assertLatestRunPermitState(t, check.fixture, run.reference.RunID, store.AccountPermitReserved)
	if got := providerAccountReservedUnits(t, check.fixture); attempts != 1 || got != baseline+1 ||
		check.providerCalls.Load() != calls {
		t.Fatalf("after the failed settlement attempts/reserved units/provider calls = %d/%d/%d; want 1/%d/%d",
			attempts, got, check.providerCalls.Load(), baseline+1, calls)
	}
	expireWorkerTaskLease(t, check.fixture, run)
	return run, baseline
}

// requireUnsentSettlement checks one settled unit: the task is blocked, every
// member is unsent with the attempt receipt as its assertion, and the permit,
// the provider-account reservation and the provider calls have the expected
// values.
func (check *unadmittedCheck) requireUnsentSettlement(
	t *testing.T,
	run permitWorkerRun,
	unit contract.WorkflowUnitResult,
	err error,
	permit store.AccountPermitState,
	reservedUnits int64,
	providerCalls int32,
) {
	t.Helper()
	if err != nil || unit.State != contract.RunStateBlocked || unit.UnknownEffectCount != 0 {
		t.Fatalf("unit = %#v error=%v cause=%v; want blocked with every member unsent",
			unit, err, errors.Unwrap(err))
	}
	receipt := attemptReceiptRef(t, check.fixture, run)
	outcomes := unadmittedOutcomes(t, check.fixture, run)
	if len(outcomes) != len(run.plan.Dispatch.OrderedMemberIDs) {
		t.Fatalf("member outcomes = %#v; want one for each of %d members", outcomes, len(run.plan.Dispatch.OrderedMemberIDs))
	}
	for _, outcome := range outcomes {
		if outcome.State != contract.OutcomeUnsent || !slices.Equal(outcome.AssertionRefs, []string{receipt}) {
			t.Fatalf("member outcome = %#v; want unsent with the attempt receipt %q", outcome, receipt)
		}
	}
	assertLatestRunPermitState(t, check.fixture, run.reference.RunID, permit)
	if got := providerAccountReservedUnits(t, check.fixture); got != reservedUnits {
		t.Fatalf("provider-account reserved units = %d; want %d", got, reservedUnits)
	}
	if got := check.providerCalls.Load(); got != providerCalls {
		t.Fatalf("provider calls = %d; want %d", got, providerCalls)
	}
}

func requireSettlementReason(t *testing.T, fixture *testfixture.Fixture, runID, reason string, want bool) {
	t.Helper()
	reasons := publicationCheckSettlementReasons(t, fixture, runID)
	if slices.Contains(reasons, reason) != want {
		t.Fatalf("settlement reason codes = %q; want %s present=%t", reasons, reason, want)
	}
}

func unadmittedOutcomes(t *testing.T, fixture *testfixture.Fixture, run permitWorkerRun) []contract.MemberOutcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var outcomes []contract.MemberOutcome
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		outcomes, err = store.ListMemberOutcomes(ctx, tx, store.TaskRef{
			RunID: run.reference.RunID, TaskID: run.plan.TaskID,
		}, run.plan.Dispatch.DispatchID)
		return err
	}); err != nil {
		t.Fatalf("load member outcomes: %v", err)
	}
	return outcomes
}

func attemptReceiptRef(t *testing.T, fixture *testfixture.Fixture, run permitWorkerRun) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var receipt string
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		dispatch, err := store.LoadDispatch(ctx, tx, store.TaskRef{
			RunID: run.reference.RunID, TaskID: run.plan.TaskID,
		}, run.plan.Dispatch.DispatchID)
		if err != nil {
			return err
		}
		if len(dispatch.Attempts) != 1 {
			return fmt.Errorf("dispatch has %d attempts; want one", len(dispatch.Attempts))
		}
		receipt = dispatch.Attempts[0].ReceiptRef
		return nil
	}); err != nil {
		t.Fatalf("load the recorded attempt: %v", err)
	}
	return receipt
}

func providerAccountReservedUnits(t *testing.T, fixture *testfixture.Fixture) int64 {
	t.Helper()
	var reserved int64
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT reserved_units FROM pm.provider_accounts
			WHERE org_id = $1 AND provider_account_id = $2
		`, fixture.Scope.OrgID, fixture.Connection.ProviderAccountID).Scan(&reserved)
	})
	return reserved
}

func runPermitID(t *testing.T, fixture *testfixture.Fixture, runID string) string {
	t.Helper()
	var permitID string
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT reference.value->>'ref'
			FROM pm.receipts AS receipt
			CROSS JOIN LATERAL jsonb_array_elements(receipt.payload->'references') AS reference(value)
			WHERE receipt.org_id = $1 AND receipt.workspace_id = $2 AND receipt.run_id = $3
			  AND receipt.kind = 'worker_attempt' AND reference.value->>'name' = 'permit'
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID).Scan(&permitID)
	})
	return permitID
}

// movePermitPastExpiry sets the expiry of the run's reserved permit to one
// microsecond after its reservation, so the database clock has passed it.
func movePermitPastExpiry(t *testing.T, fixture *testfixture.Fixture, runID string) {
	t.Helper()
	permitID := runPermitID(t, fixture, runID)
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE pm.account_permits
			SET expires_at = reserved_at + interval '1 microsecond'
			WHERE org_id = $1 AND workspace_id = $2 AND permit_id = $3 AND state = 'reserved'
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, permitID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("controlled permit expiry changed no reserved permit")
		}
		return nil
	})
}

// expireRunPermit moves the run's expired reserved permit to the expired
// state through the store transition, which returns its capacity.
func expireRunPermit(t *testing.T, fixture *testfixture.Fixture, runID string) {
	t.Helper()
	permitID := runPermitID(t, fixture, runID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		permit, err := store.LoadAccountPermit(ctx, tx, permitID)
		if err != nil {
			return err
		}
		expired, err := store.ExpireAccountPermit(ctx, tx, permit)
		if err != nil {
			return err
		}
		if expired.State != store.AccountPermitExpired {
			return fmt.Errorf("expired permit state = %q", expired.State)
		}
		return nil
	}); err != nil {
		t.Fatalf("expire the reserved permit: %v", err)
	}
}

func expireWorkerTaskLease(t *testing.T, fixture *testfixture.Fixture, run permitWorkerRun) {
	t.Helper()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE pm.task_leases
			SET lease_expires_at = clock_timestamp() - interval '1 microsecond'
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND task_id = $4
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, run.reference.RunID, run.plan.TaskID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("controlled lease expiry did not change exactly the unit's task lease")
		}
		return nil
	})
}

// installWorkerSettlementFailure makes every worker settlement of one run fail
// inside its transaction. The returned function removes the fault.
func installWorkerSettlementFailure(t *testing.T, fixture *testfixture.Fixture, runID string) func() {
	t.Helper()
	suffix := strings.Map(func(character rune) rune {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			return character
		}
		return '_'
	}, strings.ToLower(runID))
	if len(suffix) > 24 {
		suffix = suffix[len(suffix)-24:]
	}
	name := "test_settle_failure_" + suffix
	qualifiedFunction := pgx.Identifier{"pm", name}.Sanitize()
	quotedTrigger := pgx.Identifier{name}.Sanitize()
	literal := func(value string) string {
		return "'" + strings.ReplaceAll(value, "'", "''") + "'"
	}
	remove := func() {
		fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON pm.outbox_commands", quotedTrigger)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "DROP FUNCTION IF EXISTS "+qualifiedFunction+"()")
			return err
		})
	}
	remove()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
			CREATE FUNCTION %s() RETURNS trigger
			LANGUAGE plpgsql SECURITY INVOKER
			AS $body$
			BEGIN
				RAISE EXCEPTION 'injected worker settlement failure';
				RETURN NEW;
			END
			$body$
		`, qualifiedFunction)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, fmt.Sprintf(`
			CREATE TRIGGER %s
			BEFORE INSERT ON pm.outbox_commands
			FOR EACH ROW
			WHEN (
				NEW.org_id = %s
				AND NEW.workspace_id = %s
				AND NEW.run_id = %s
				AND NEW.kind = 'worker_settlement_recorded'
			)
			EXECUTE FUNCTION %s()
		`,
			quotedTrigger,
			literal(fixture.Scope.OrgID),
			literal(fixture.Scope.WorkspaceID),
			literal(runID),
			qualifiedFunction,
		))
		return err
	})
	t.Cleanup(remove)
	return remove
}
