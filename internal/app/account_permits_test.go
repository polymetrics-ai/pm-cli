package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	pmtemporal "github.com/polymetrics-ai/pm-cli/internal/temporal"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
	sdkclient "go.temporal.io/sdk/client"
)

var permitTemporalIdentitySequence atomic.Uint64

type permitTemporalHarness struct {
	namespace   string
	backend     sdkclient.Client
	relayClient *pmtemporal.Client
	observer    contract.WorkflowContinuationObserver
	timeouts    contract.ActivityTimeouts
}

type permitWorkerRun struct {
	execution contract.WorkflowExecution
	reference contract.WorkflowRunRef
	plan      testfixture.SealedWorkerPlan
}

type permitExecutionResult struct {
	result contract.WorkflowUnitResult
	err    error
}

func TestInitialGrantRevocation(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	t.Run("E01_C01_cli_session_expires_only", func(t *testing.T) {
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
			t,
			fixture,
			fixture.Client,
			fixture.Scope,
			fixture.Connection.ConnectionID,
			"issue17-session-expiry",
		)
		run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)

		expirePermitTestSessions(t, fixture, fixture.Actor)
		requirePermitTestSessionExpired(t, fixture)

		result, err := executePermitWorkerUnit(t, service, run)
		if err != nil {
			t.Fatalf("accepted run after CLI session expiry: %v", err)
		}
		if result.State != contract.RunStateSucceeded || sends.Load() != 1 {
			t.Fatalf("session-independent execution = state %q sends %d; want succeeded/1", result.State, sends.Load())
		}
	})

	t.Run("E01_C02_connection_principal_or_grant_revoked", func(t *testing.T) {
		for _, authority := range []string{"connection", "principal", "grant"} {
			t.Run(authority, func(t *testing.T) {
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
					t,
					fixture,
					fixture.Client,
					fixture.Scope,
					fixture.Connection.ConnectionID,
					"issue17-revoked-"+authority,
				)
				run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)

				revokePermitTestAuthority(t, fixture, admitted.RunRef, authority)
				_, err := executePermitWorkerUnit(t, service, run)
				if !errors.Is(err, app.ErrWorkerAuthorization) {
					t.Fatalf("worker error after %s revocation = %v; want ErrWorkerAuthorization", authority, err)
				}
				if sends.Load() != 0 {
					t.Fatalf("provider calls after %s revocation = %d; want 0", authority, sends.Load())
				}
				permits, attempts := permitTestDurableCounts(t, fixture, fixture.Scope, admitted.RunRef)
				if permits != 0 || attempts != 0 {
					t.Fatalf("durable physical work after %s revocation = permits %d attempts %d; want 0/0", authority, permits, attempts)
				}
			})
		}
	})

	t.Run("E01_C03_no_registered_handler", func(t *testing.T) {
		fixture := testfixture.New(t)
		response := submitInitialRun(
			t,
			fixture,
			fixture.Client,
			fixture.Scope.WorkspaceID,
			"issue17-unregistered-handler",
		)
		requireInitialProblem(t, response, http.StatusServiceUnavailable, "capability_unavailable")

		var runs, permits, attempts int64
		fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `
				SELECT count(*) FROM pm.runs
				WHERE org_id = $1 AND workspace_id = $2
			`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID).Scan(&runs); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `
				SELECT count(*) FROM pm.account_permits
				WHERE org_id = $1 AND workspace_id = $2
			`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID).Scan(&permits); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `
				SELECT count(*) FROM pm.dispatch_attempts
				WHERE org_id = $1 AND workspace_id = $2
			`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID).Scan(&attempts)
		})
		if runs != 0 || permits != 0 || attempts != 0 {
			t.Fatalf("unregistered handler durable work = runs %d permits %d attempts %d; want 0/0/0", runs, permits, attempts)
		}
	})
}

func TestSharedAccountAdmission(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	t.Run("E01_C04_concurrent_alias_reservations", func(t *testing.T) {
		started := make(chan struct{}, 2)
		release := make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		var sends atomic.Int32
		fixture := testfixture.NewExecuting(
			t,
			func(ctx context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
				sends.Add(1)
				started <- struct{}{}
				select {
				case <-release:
					return successfulPermitObservation(request), nil
				case <-ctx.Done():
					return app.ExecutionObservation{}, ctx.Err()
				}
			},
			unexpectedPermitReconciliation,
		)
		setPermitTestAccountCapacity(t, fixture, 1)

		workerAda := newPermitWorkerService(t, fixture, temporal)
		workerBeta := newPermitWorkerService(t, fixture, temporal)
		alphaAdmission := admitPermitRun(
			t,
			fixture,
			fixture.Client,
			fixture.Scope,
			fixture.Connection.ConnectionID,
			"issue17-alpha-active",
		)
		_ = admitPermitRun(
			t,
			fixture,
			fixture.Client,
			fixture.Scope,
			fixture.Connection.ConnectionID,
			"issue17-alpha-budget-final",
		)
		alphaRefusal := submitInitialRun(
			t,
			fixture,
			fixture.Client,
			fixture.Scope.WorkspaceID,
			"issue17-alpha-budget-refused",
		)
		requireInitialProblem(t, alphaRefusal, http.StatusTooManyRequests, "workspace_admission_count_limit")

		foreignClient := fixture.ClientFor(t, fixture.ForeignActor)
		betaAdmission := admitPermitRun(
			t,
			fixture,
			foreignClient,
			fixture.ForeignScope,
			fixture.ForeignConnection.ConnectionID,
			"issue17-beta-active",
		)
		alpha := preparePermitWorkerRun(t, fixture, workerAda, temporal, fixture.Scope, fixture.Actor, alphaAdmission)
		beta := preparePermitWorkerRun(t, fixture, workerBeta, temporal, fixture.ForeignScope, fixture.ForeignActor, betaAdmission)
		betaBudgetBefore := loadPermitTestBudget(t, fixture, fixture.ForeignScope, fixture.ForeignActor, betaAdmission.RunRef)
		if betaBudgetBefore.Policy.Pending.WorkspacePending != 2 {
			t.Fatalf("beta workspace budget = %d; want fixture budget 2", betaBudgetBefore.Policy.Pending.WorkspacePending)
		}

		alphaResult := make(chan permitExecutionResult, 1)
		go func() {
			result, err := executePermitWorkerUnitContext(context.Background(), workerAda, alpha)
			alphaResult <- permitExecutionResult{result: result, err: err}
		}()
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("worker Ada did not reach the committed physical-unit boundary")
		}

		betaResult := make(chan permitExecutionResult, 1)
		go func() {
			result, err := executePermitWorkerUnitContext(context.Background(), workerBeta, beta)
			betaResult <- permitExecutionResult{result: result, err: err}
		}()
		select {
		case <-started:
			releaseOnce.Do(func() { close(release) })
			<-alphaResult
			<-betaResult
			t.Fatal("both aliases entered the physical callback with account capacity 1")
		case outcome := <-betaResult:
			if !errors.Is(outcome.err, store.ErrAccountCapacity) {
				releaseOnce.Do(func() { close(release) })
				<-alphaResult
				t.Fatalf("beta worker contention error = %v; want ErrAccountCapacity", outcome.err)
			}
		case <-time.After(5 * time.Second):
			releaseOnce.Do(func() { close(release) })
			<-alphaResult
			t.Fatal("beta worker did not observe shared account capacity")
		}

		alphaPermits, alphaAttempts := permitTestDurableCounts(t, fixture, fixture.Scope, alphaAdmission.RunRef)
		betaPermits, betaAttempts := permitTestDurableCounts(t, fixture, fixture.ForeignScope, betaAdmission.RunRef)
		if alphaPermits != 1 || alphaAttempts != 1 || betaPermits != 0 || betaAttempts != 0 {
			releaseOnce.Do(func() { close(release) })
			<-alphaResult
			t.Fatalf("capacity-1 durable winners = alpha %d/%d beta %d/%d; want 1/1 and 0/0", alphaPermits, alphaAttempts, betaPermits, betaAttempts)
		}
		betaBudgetAfter := loadPermitTestBudget(t, fixture, fixture.ForeignScope, fixture.ForeignActor, betaAdmission.RunRef)
		if betaBudgetAfter != betaBudgetBefore {
			releaseOnce.Do(func() { close(release) })
			<-alphaResult
			t.Fatal("alpha workload changed beta's immutable workspace budget")
		}

		releaseOnce.Do(func() { close(release) })
		outcome := <-alphaResult
		if outcome.err != nil || outcome.result.State != contract.RunStateSucceeded {
			t.Fatalf("capacity winner settlement = result %#v error %v", outcome.result, outcome.err)
		}
		if sends.Load() != 1 {
			t.Fatalf("capacity-1 physical calls = %d; want 1", sends.Load())
		}
	})

	t.Run("E01_C05_expired_permit_at_next_unit", func(t *testing.T) {
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
			t,
			fixture,
			fixture.Client,
			fixture.Scope,
			fixture.Connection.ConnectionID,
			"issue17-expired-permit",
		)
		run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
		reserveExpiredPermitForWorker(t, fixture, fixture.Scope, fixture.Actor, run)

		result, err := executePermitWorkerUnit(t, service, run)
		_, attempts := permitTestDurableCounts(t, fixture, fixture.Scope, admitted.RunRef)
		if sends.Load() != 0 || attempts != 0 {
			t.Fatalf(
				"expired permit physical work = callbacks %d attempts %d result %#v error %v; want 0/0",
				sends.Load(),
				attempts,
				result,
				err,
			)
		}
	})

	t.Run("E01_C06_healthy_current_grant", func(t *testing.T) {
		var fixture *testfixture.Fixture
		committed := make(chan app.ExecutionRequest, 1)
		fixture = testfixture.NewExecuting(
			t,
			func(ctx context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
				if err := requireCommittedPermitTestUnit(ctx, fixture, fixture.Actor, request); err != nil {
					return app.ExecutionObservation{}, err
				}
				committed <- request
				return successfulPermitObservation(request), nil
			},
			unexpectedPermitReconciliation,
		)
		service := newPermitWorkerService(t, fixture, temporal)
		admitted := admitPermitRun(
			t,
			fixture,
			fixture.Client,
			fixture.Scope,
			fixture.Connection.ConnectionID,
			"issue17-healthy-grant",
		)
		run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)

		result, err := executePermitWorkerUnit(t, service, run)
		if err != nil {
			t.Fatalf("healthy worker unit: %v", err)
		}
		if result.State != contract.RunStateSucceeded {
			t.Fatalf("healthy unit state = %q; want succeeded", result.State)
		}
		var request app.ExecutionRequest
		select {
		case request = <-committed:
		default:
			t.Fatal("healthy callback did not observe its committed physical unit")
		}
		var permit store.AccountPermit
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
			var err error
			permit, err = store.LoadAccountPermit(ctx, tx, request.PermitRef)
			return err
		}); err != nil {
			t.Fatalf("load settled healthy permit: %v", err)
		}
		_, attempts := permitTestDurableCounts(t, fixture, fixture.Scope, admitted.RunRef)
		if permit.State != store.AccountPermitResolved || attempts != 1 {
			t.Fatalf("healthy committed unit = permit %q attempts %d; want resolved/1", permit.State, attempts)
		}
	})
}

func admitPermitRun(
	t *testing.T,
	fixture *testfixture.Fixture,
	client *http.Client,
	scope contract.Scope,
	connectionID string,
	requestKey string,
) api.AdmissionResponse {
	t.Helper()
	invocation := testfixture.Invocation(t, fixture, requestKey)
	invocation.ConnectionRef = connectionID
	response := submitInitialInvocation(t, fixture, client, scope.WorkspaceID, invocation)
	return requireFreshInitialAdmission(t, response, requestKey)
}

func newPermitTemporalHarness(t *testing.T) *permitTemporalHarness {
	t.Helper()
	artifact := testfixture.NewWorkerConfiguration(t)
	configuration := artifact.Configuration
	temporalConfiguration := configuration.Temporal()
	workerConfiguration := configuration.Worker()

	backend := testfixture.OpenTemporal(
		t,
		configuration,
		temporalConfiguration.Namespace,
		nextPermitTemporalIdentity("observer"),
	)
	observer, err := pmtemporal.NewWorkflowContinuationObserver(
		backend,
		temporalConfiguration.Namespace,
		workerConfiguration.ActivityTimeouts.Transport,
	)
	if err != nil {
		t.Fatalf("construct trusted issue17 continuation observer: %v", err)
	}
	relayOptions := testfixture.TemporalOptions(
		t,
		configuration,
		temporalConfiguration.Namespace,
		nextPermitTemporalIdentity("relay"),
	)
	relayClient, err := pmtemporal.NewClient(pmtemporal.ClientConfig{
		Namespace:            temporalConfiguration.Namespace,
		ControlTaskQueue:     contract.ControlTaskQueue,
		WorkflowType:         contract.RunWorkflowType,
		ProjectionUpdateName: contract.ProjectionUpdateName,
		DialTimeout:          workerConfiguration.DialTimeout,
		RequestTimeout:       workerConfiguration.ActivityTimeouts.Transport,
		Dial: func(ctx context.Context) (pmtemporal.WorkflowClient, error) {
			return pmtemporal.DialWorkerClient(ctx, relayOptions, workerConfiguration.DialTimeout)
		},
	})
	if err != nil {
		t.Fatalf("construct production issue17 Temporal client: %v", err)
	}
	t.Cleanup(relayClient.Close)
	return &permitTemporalHarness{
		namespace: temporalConfiguration.Namespace, backend: backend, relayClient: relayClient,
		observer: observer, timeouts: workerConfiguration.ActivityTimeouts,
	}
}

func nextPermitTemporalIdentity(role string) string {
	return fmt.Sprintf("issue17-%s-%d", role, permitTemporalIdentitySequence.Add(1))
}

func newPermitWorkerService(
	t *testing.T,
	fixture *testfixture.Fixture,
	temporal *permitTemporalHarness,
) *app.WorkerService {
	t.Helper()
	if temporal == nil || temporal.observer == nil {
		t.Fatal("issue17 worker requires its real Temporal fixture observer")
	}
	return testfixture.NewWorkerService(
		t,
		fixture,
		fixture.WorkerRegistry,
		fixture.ResolveOperation,
		nil,
		temporal.observer,
		temporal.timeouts,
	)
}

func preparePermitWorkerRun(
	t *testing.T,
	fixture *testfixture.Fixture,
	service *app.WorkerService,
	temporal *permitTemporalHarness,
	scope contract.Scope,
	actor contract.Actor,
	admitted api.AdmissionResponse,
) permitWorkerRun {
	t.Helper()
	bound := bindPermitWorkerRun(t, fixture, temporal, scope, actor, admitted.RunRef)
	bound.plan = sealPermitWorkerPlan(t, fixture, service, scope, actor, bound.execution, bound.reference)
	return bound
}

func bindPermitWorkerRun(
	t *testing.T,
	fixture *testfixture.Fixture,
	temporal *permitTemporalHarness,
	scope contract.Scope,
	actor contract.Actor,
	runID string,
) permitWorkerRun {
	t.Helper()
	var commandID string
	fixture.SetupSQL(t, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT command_id
			FROM pm.outbox_commands
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
			  AND kind = 'start_run' AND state = 'pending'
		`, scope.OrgID, scope.WorkspaceID, runID).Scan(&commandID)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var route store.WorkflowRoute
	if err := store.WithinScope(ctx, fixture.Pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		claim, err := store.ClaimOutbox(ctx, tx, store.OutboxClaimRequest{
			RunID: runID, CommandID: commandID, OwnerID: "issue17-relay", TTL: 30 * time.Second,
		})
		if err != nil {
			return err
		}
		route = store.WorkflowRoute{Claim: claim, Actor: actor}
		return nil
	}); err != nil {
		t.Fatalf("claim accepted issue17 start route: %v", err)
	}
	relay, err := pmtemporal.NewRelay(pmtemporal.RelayConfig{
		Store: fixture.Pool, Client: temporal.relayClient, OwnerID: "issue17-relay",
		ClaimLimit: 1, ClaimTTL: 30 * time.Second,
		IdleDelay: 10 * time.Millisecond, FailureDelay: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("construct production issue17 relay: %v", err)
	}
	handoff, err := relay.StartOrReconcile(ctx, route)
	if err != nil {
		t.Fatalf("start accepted issue17 workflow through production relay: %v", err)
	}
	if handoff.Disposition != pmtemporal.HandoffStarted || handoff.WorkflowID == "" ||
		handoff.TemporalRunID == "" || handoff.Generation == 0 {
		t.Fatalf("issue17 workflow handoff = %#v; want a newly started actual execution", handoff)
	}

	var bound permitWorkerRun
	var firstExecutionRunID string
	if err := store.WithinScope(ctx, fixture.Pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		record, err := store.LoadRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		intent, err := store.LoadWorkerCommandIntent(ctx, tx, runID, commandID)
		if err != nil {
			return err
		}
		if len(record.WorkflowBinding.Observations) == 0 ||
			record.WorkflowBinding.CurrentTemporalRunID() != handoff.TemporalRunID ||
			record.WorkflowBinding.WorkflowID != handoff.WorkflowID ||
			intent.Plan.Namespace != temporal.namespace ||
			intent.Plan.WorkflowID != handoff.WorkflowID ||
			intent.Plan.Generation != handoff.Generation {
			return errors.New("production relay did not persist its exact observed Temporal execution")
		}
		firstExecutionRunID = record.WorkflowBinding.Observations[0].TemporalRunID
		bound.execution = contract.WorkflowExecution{
			Namespace: temporal.namespace, WorkflowID: handoff.WorkflowID, RunID: handoff.TemporalRunID,
		}
		bound.reference = contract.WorkflowRunRef{
			Scope: scope, RunID: runID, WorkflowID: handoff.WorkflowID,
			StartCommandID: commandID, Generation: handoff.Generation, IntentDigest: intent.Digest,
		}
		if err := bound.execution.Validate(); err != nil {
			return err
		}
		return bound.reference.Validate()
	}); err != nil {
		t.Fatalf("load actual issue17 workflow binding: %v", err)
	}
	testfixture.CleanupWorkflowChain(
		t,
		temporal.backend,
		temporal.namespace,
		handoff.WorkflowID,
		firstExecutionRunID,
	)
	return bound
}

func sealPermitWorkerPlan(
	t *testing.T,
	fixture *testfixture.Fixture,
	service *app.WorkerService,
	scope contract.Scope,
	actor contract.Actor,
	execution contract.WorkflowExecution,
	reference contract.WorkflowRunRef,
) testfixture.SealedWorkerPlan {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snapshot, err := service.LoadWorkflow(ctx, execution, reference)
	if err != nil {
		t.Fatalf("materialize issue17 operation task: %v", err)
	}
	if snapshot.Done || len(snapshot.Units) != 1 || snapshot.Units[0].TaskID == "" {
		t.Fatalf("issue17 materialized units = %#v; want one dispatchable unit", snapshot.Units)
	}

	taskRef := store.TaskRef{RunID: reference.RunID, TaskID: snapshot.Units[0].TaskID}
	var plan testfixture.SealedWorkerPlan
	var planningLease store.Lease
	err = store.WithinScope(ctx, fixture.Pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		task, err := store.LoadTask(ctx, tx, taskRef)
		if err != nil {
			return err
		}
		run, err := store.LoadRun(ctx, tx, reference.RunID)
		if err != nil {
			return err
		}
		input, err := store.LoadRunInputSnapshot(ctx, tx, reference.RunID)
		if err != nil {
			return err
		}
		grant, err := store.LoadRunExecutionGrant(ctx, tx, reference.RunID)
		if err != nil {
			return err
		}
		connection, err := store.LoadConnection(ctx, tx, input.ConnectionID)
		if err != nil {
			return err
		}
		account, err := store.LoadProviderAccount(ctx, tx, connection.ProviderAccountID)
		if err != nil {
			return err
		}
		requestBytes, err := json.Marshal(input.Input)
		if err != nil {
			return err
		}
		entity, err := contract.NewEntityKey(account.AccountWitness, "operation", task.OperationID, task.BehaviorID)
		if err != nil {
			return err
		}
		effect, err := contract.NewEffectKey(entity, task.TaskID, task.BehaviorID)
		if err != nil {
			return err
		}
		dispatchID, err := contract.NewDispatchID("dispatch-" + task.TaskID)
		if err != nil {
			return err
		}
		memberID, err := contract.NewMemberID("member-" + task.TaskID)
		if err != nil {
			return err
		}
		attemptID, err := contract.NewAttemptID("attempt-" + task.TaskID)
		if err != nil {
			return err
		}
		dispatch := contract.Dispatch{
			RunID:              reference.RunID,
			EffectKey:          effect,
			DispatchID:         dispatchID,
			OrderedMemberIDs:   []contract.MemberID{memberID},
			PayloadHash:        contract.NewPayloadHash(requestBytes),
			RequestArtifactRef: input.InputRef,
			OperationRevision:  run.Spec.OperationOrPlanRef.Revision,
			AccountWitness:     account.AccountWitness,
			GrantEpoch:         grant.Grant.GrantEpoch,
			CapacityReservation: contract.CapacityReservation{
				OutcomeSlots: 1, ReadbackSlots: 1, EncodedBytes: uint64(len(requestBytes)) + 1024,
			},
			AttemptID: attemptID,
		}
		if err := dispatch.Validate(); err != nil {
			return err
		}
		planningLease, err = store.ClaimTask(ctx, tx, store.ClaimRequest{
			Task: taskRef, OwnerID: "issue17-plan-" + task.TaskID, TTL: 50 * time.Millisecond,
		})
		if err != nil {
			return err
		}
		if err := store.PrepareDispatch(ctx, tx, planningLease, dispatch); err != nil {
			return err
		}
		plan = testfixture.SealedWorkerPlan{TaskID: task.TaskID, Dispatch: dispatch}
		return nil
	})
	if err != nil {
		t.Fatalf("seal issue17 worker plan: %v", err)
	}
	for {
		var now time.Time
		if err := store.WithinScope(ctx, fixture.Pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
			var err error
			now, err = store.ReadAuthenticationTime(ctx, tx)
			return err
		}); err != nil {
			t.Fatalf("observe issue17 planning lease: %v", err)
		}
		if !now.Before(planningLease.ExpiresAt) {
			return plan
		}
		select {
		case <-ctx.Done():
			t.Fatal("issue17 planning lease did not expire")
		case <-time.After(time.Millisecond):
		}
	}
}

func executePermitWorkerUnit(
	t *testing.T,
	service *app.WorkerService,
	run permitWorkerRun,
) (contract.WorkflowUnitResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return executePermitWorkerUnitContext(ctx, service, run)
}

func executePermitWorkerUnitContext(
	ctx context.Context,
	service *app.WorkerService,
	run permitWorkerRun,
) (contract.WorkflowUnitResult, error) {
	return service.ExecuteUnit(ctx, run.execution, contract.WorkflowUnitRequest{
		Run: run.reference, Unit: contract.WorkflowUnitRef{TaskID: run.plan.TaskID},
	}, nil)
}

func successfulPermitObservation(request app.ExecutionRequest) app.ExecutionObservation {
	observedAt := time.Now().UTC().Truncate(time.Microsecond)
	outcomes := make([]contract.MemberOutcome, 0, len(request.Dispatch.OrderedMemberIDs))
	for _, memberID := range request.Dispatch.OrderedMemberIDs {
		outcomes = append(outcomes, contract.MemberOutcome{
			MemberID: memberID, EffectKey: request.Dispatch.EffectKey, DispatchID: request.Dispatch.DispatchID,
			State: contract.OutcomeSucceeded, ObservedAt: observedAt, AssertionRefs: []string{},
		})
	}
	return app.ExecutionObservation{Outcomes: outcomes}
}

func unexpectedPermitReconciliation(context.Context, app.ExecutionRequest) (app.ExecutionObservation, error) {
	return app.ExecutionObservation{}, errors.New("issue17 fresh controlled unit unexpectedly reached reconciliation")
}

func expirePermitTestSessions(t *testing.T, fixture *testfixture.Fixture, actor contract.Actor) {
	t.Helper()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE pm.pm_sessions
			SET issued_at = clock_timestamp() - interval '2 hours',
			    expires_at = clock_timestamp() - interval '1 hour'
			WHERE deployment_id = $1 AND org_id = $2 AND principal_id = $3
			  AND revoked_at IS NULL
		`, fixture.DeploymentID, fixture.Scope.OrgID, actor.RequestedActor.PrincipalID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errors.New("fixture session expiry matched no active session")
		}
		return nil
	})
}

func requirePermitTestSessionExpired(t *testing.T, fixture *testfixture.Fixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fixture.BaseURL+"/v1/sessions/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := fixture.Client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired CLI session status = %d; want 401", response.StatusCode)
	}
}

func revokePermitTestAuthority(t *testing.T, fixture *testfixture.Fixture, runID, authority string) {
	t.Helper()
	switch authority {
	case "connection":
		fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `
				UPDATE pm.connections
				SET state = 'revoked', revoked_at = clock_timestamp(),
				    revision = revision + 1, updated_at = clock_timestamp()
				WHERE org_id = $1 AND workspace_id = $2 AND connection_id = $3
				  AND state = 'active' AND revoked_at IS NULL
			`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, fixture.Connection.ConnectionID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errors.New("fixture connection revocation did not change one authority")
			}
			return nil
		})
	case "principal":
		fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `
				UPDATE pm.memberships
				SET revoked_at = clock_timestamp(), policy_epoch = policy_epoch + 1,
				    control_revision = control_revision + 1
				WHERE org_id = $1 AND workspace_id = $2 AND identity_id = $3
				  AND revoked_at IS NULL
			`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, fixture.Actor.RequestedActor.PrincipalID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errors.New("fixture principal revocation did not change one authority")
			}
			return nil
		})
	case "grant":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
			grant, err := store.LoadRunExecutionGrant(ctx, tx, runID)
			if err != nil {
				return err
			}
			revoked, changed, err := app.RevokeFutureEffects(ctx, tx, runID, grant.Grant.GrantEpoch)
			if err == nil && (!changed || revoked.RevokedAt == nil || !revoked.Grant.Equal(grant.Grant)) {
				return errors.New("durable run grant revocation did not preserve immutable accepted authority")
			}
			return err
		}); err != nil {
			t.Fatalf("revoke fixture run grant: %v", err)
		}
	default:
		t.Fatalf("unknown issue17 authority %q", authority)
	}
}

func setPermitTestAccountCapacity(t *testing.T, fixture *testfixture.Fixture, capacity int64) {
	t.Helper()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE pm.provider_accounts
			SET capacity = $3
			WHERE org_id = $1 AND provider_account_id = $2 AND reserved_units = 0
		`, fixture.Scope.OrgID, fixture.Connection.ProviderAccountID, capacity)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("fixture provider-account capacity was not changed")
		}
		return nil
	})
}

func loadPermitTestBudget(
	t *testing.T,
	fixture *testfixture.Fixture,
	scope contract.Scope,
	actor contract.Actor,
	runID string,
) store.RunBudgetSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var budget store.RunBudgetSnapshot
	if err := store.WithinScope(ctx, fixture.Pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		budget, err = store.LoadRunBudgetSnapshot(ctx, tx, runID)
		return err
	}); err != nil {
		t.Fatalf("load issue17 run budget: %v", err)
	}
	return budget
}

func permitTestDurableCounts(
	t *testing.T,
	fixture *testfixture.Fixture,
	scope contract.Scope,
	runID string,
) (int64, int64) {
	t.Helper()
	var permits, attempts int64
	fixture.SetupSQL(t, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM pm.account_permits
			WHERE org_id = $1 AND workspace_id = $2
		`, scope.OrgID, scope.WorkspaceID).Scan(&permits); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM pm.dispatch_attempts
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
		`, scope.OrgID, scope.WorkspaceID, runID).Scan(&attempts)
	})
	return permits, attempts
}

func reserveExpiredPermitForWorker(
	t *testing.T,
	fixture *testfixture.Fixture,
	scope contract.Scope,
	actor contract.Actor,
	run permitWorkerRun,
) {
	t.Helper()
	permitID := permitTestRecordID(t, "permit-v1-", struct {
		Scope      contract.Scope      `json:"scope"`
		RunID      string              `json:"run_id"`
		TaskID     string              `json:"task_id"`
		DispatchID contract.DispatchID `json:"dispatch_id"`
	}{scope, run.reference.RunID, run.plan.TaskID, run.plan.Dispatch.DispatchID})
	ownerID := permitTestRecordID(t, "permit-owner-v1-", struct {
		RunID      string              `json:"run_id"`
		TaskID     string              `json:"task_id"`
		DispatchID contract.DispatchID `json:"dispatch_id"`
	}{run.reference.RunID, run.plan.TaskID, run.plan.Dispatch.DispatchID})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var permit store.AccountPermit
	if err := store.WithinScope(ctx, fixture.Pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		permit, err = store.ReserveAccountPermit(ctx, tx, store.AccountPermitRequest{
			PermitID: permitID, ConnectionID: fixture.Connection.ConnectionID,
			OwnerID: ownerID, Units: 1, TTL: 5 * time.Millisecond,
		})
		return err
	}); err != nil {
		t.Fatalf("reserve issue17 expiring permit: %v", err)
	}
	for {
		var now time.Time
		if err := store.WithinScope(ctx, fixture.Pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
			var err error
			now, err = store.ReadAuthenticationTime(ctx, tx)
			return err
		}); err != nil {
			t.Fatalf("read permit expiry clock: %v", err)
		}
		if !now.Before(permit.ExpiresAt) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("controlled account permit did not expire")
		case <-time.After(time.Millisecond):
		}
	}
}

func permitTestRecordID(t *testing.T, prefix string, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := contract.CanonicalDigest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return prefix + strings.TrimPrefix(string(digest), "sha256:")
}

func requireCommittedPermitTestUnit(
	ctx context.Context,
	fixture *testfixture.Fixture,
	actor contract.Actor,
	request app.ExecutionRequest,
) error {
	return store.WithinScope(ctx, fixture.Pool, request.Scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		permit, err := store.LoadAccountPermit(ctx, tx, request.PermitRef)
		if err != nil {
			return err
		}
		if permit.State != store.AccountPermitDispatched || permit.ConnectionID != request.ConnectionID {
			return fmt.Errorf("callback permit = state %q connection %q", permit.State, permit.ConnectionID)
		}
		dispatch, err := store.LoadWorkerDispatch(ctx, tx, store.TaskRef{
			RunID: request.Run.RunID, TaskID: request.Task.TaskID,
		})
		if err != nil {
			return err
		}
		if dispatch.State != store.DispatchPossiblySent || len(dispatch.Attempts) != 1 ||
			dispatch.Attempts[0].AttemptID != request.Attempt.AttemptID {
			return fmt.Errorf("callback dispatch was not one committed physical unit: %#v", dispatch)
		}
		return nil
	})
}
