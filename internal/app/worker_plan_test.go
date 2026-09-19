package app_test

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

// plannedWorkerRun is one runnable worker unit that the production load path
// planned from durable rows alone. It deliberately carries no fixture-sealed
// dispatch: every fact below was read back from PostgreSQL after planning.
type plannedWorkerRun struct {
	execution contract.WorkflowExecution
	reference contract.WorkflowRunRef
	unit      contract.WorkflowUnitRef
	dispatch  store.DispatchRecord
}

// plannedDirectReadFacts is the durable authority that a direct-read plan must
// match exactly.
type plannedDirectReadFacts struct {
	run     contract.RunRecord
	input   store.RunInputSnapshot
	grant   store.RunExecutionGrant
	account store.ProviderAccount
	task    contract.Task
}

// planningDurableCounts counts the durable planning rows of one run.
type planningDurableCounts struct {
	dispatches int64
	effects    int64
	members    int64
}

// TestWorkerDispatchPlanning proves an accepted operation-reference direct-read
// run reaches the physical worker path through the production planner alone,
// while no send authority exists until the existing permit and attempt boundary
// runs.
func TestWorkerDispatchPlanning(t *testing.T) {
	temporal := newPermitTemporalHarness(t)

	t.Run("one exact durable plan without send authority", func(t *testing.T) {
		fixture, selection, policy := newPlannableDirectReadFixture(t, "fixture.plan.exact")
		service := newPermitWorkerService(t, fixture, temporal)
		admitted := admitPlanningRun(
			t, fixture, selection, fixture.Client, fixture.Scope,
			fixture.Connection.ConnectionID, contract.LaneDirectRead, "worker-plan-exact",
		)
		run := planWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)

		// Planning must not be send authority: no account permit, no physical
		// attempt and no member outcome may exist yet.
		if permits, attempts := permitTestDurableCounts(t, fixture, fixture.Scope, admitted.RunRef); permits != 0 || attempts != 0 {
			t.Fatalf("planning created send authority: permits %d attempts %d; want 0/0", permits, attempts)
		}
		if counts := countPlanningRows(t, fixture, fixture.Scope, admitted.RunRef); counts != (planningDurableCounts{dispatches: 1, effects: 1, members: 1}) {
			t.Fatalf("durable planning rows = %#v; want exactly one effect, dispatch and member", counts)
		}
		if run.dispatch.State != store.DispatchPrepared || run.dispatch.AttemptNumber != 1 || len(run.dispatch.Attempts) != 0 {
			t.Fatalf("planned dispatch record = %#v; want one prepared attempt-free dispatch", run.dispatch)
		}
		facts := loadPlannedDirectReadFacts(t, fixture, fixture.Scope, fixture.Actor, run.reference.RunID, run.unit.TaskID)
		requestBytes, err := json.Marshal(facts.input.Input)
		if err != nil {
			t.Fatal(err)
		}
		entity, err := contract.NewEntityKey(
			facts.account.AccountWitness, "operation", facts.task.OperationID, facts.task.BehaviorID,
		)
		if err != nil {
			t.Fatal(err)
		}
		effect, err := contract.NewEffectKey(entity, facts.task.TaskID, facts.task.BehaviorID)
		if err != nil {
			t.Fatal(err)
		}
		dispatch := run.dispatch.Intent
		if dispatch.RunID != run.reference.RunID ||
			dispatch.EffectKey != effect ||
			dispatch.PayloadHash != contract.NewPayloadHash(requestBytes) ||
			dispatch.RequestArtifactRef != facts.input.InputRef ||
			dispatch.OperationRevision != facts.run.Spec.OperationOrPlanRef.Revision ||
			dispatch.AccountWitness != facts.account.AccountWitness ||
			dispatch.GrantEpoch != facts.grant.Grant.GrantEpoch ||
			len(dispatch.OrderedMemberIDs) != 1 ||
			dispatch.CapacityReservation.OutcomeSlots != 1 ||
			dispatch.CapacityReservation.ReadbackSlots != 1 ||
			dispatch.CapacityReservation.EncodedBytes != uint64(len(requestBytes))+policy.MaxResponseBytes {
			t.Fatalf("planned dispatch intent = %#v; want the exact admitted direct-read facts", dispatch)
		}
		if facts.task.OperationID != selection.Operation.OperationID || facts.task.Lane != contract.LaneDirectRead {
			t.Fatalf("planned task = %#v; want the admitted direct-read operation", facts.task)
		}
		if run.unit != (contract.WorkflowUnitRef{
			TaskID: facts.task.TaskID, DispatchID: dispatch.DispatchID,
		}) {
			t.Fatalf("planned unit = %#v; want the exact durable dispatch identity", run.unit)
		}
	})

	t.Run("repeated loads stay one durable plan", func(t *testing.T) {
		fixture, selection, _ := newPlannableDirectReadFixture(t, "fixture.plan.repeat")
		service := newPermitWorkerService(t, fixture, temporal)
		admitted := admitPlanningRun(
			t, fixture, selection, fixture.Client, fixture.Scope,
			fixture.Connection.ConnectionID, contract.LaneDirectRead, "worker-plan-repeat",
		)
		first := planWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)

		for attempt := 0; attempt < 3; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			snapshot, err := service.LoadWorkflow(ctx, first.execution, first.reference)
			cancel()
			if err != nil {
				t.Fatalf("repeated load %d: %v", attempt, err)
			}
			if snapshot.Done || len(snapshot.Units) != 1 || snapshot.Units[0] != first.unit {
				t.Fatalf("repeated load %d units = %#v done=%v; want the same single unit", attempt, snapshot.Units, snapshot.Done)
			}
		}
		if counts := countPlanningRows(t, fixture, fixture.Scope, admitted.RunRef); counts != (planningDurableCounts{dispatches: 1, effects: 1, members: 1}) {
			t.Fatalf("repeated loads produced %#v; want one effect, dispatch and member", counts)
		}
	})

	t.Run("concurrent loads produce one effect", func(t *testing.T) {
		fixture, selection, _ := newPlannableDirectReadFixture(t, "fixture.plan.concurrent")
		service := newPermitWorkerService(t, fixture, temporal)
		admitted := admitPlanningRun(
			t, fixture, selection, fixture.Client, fixture.Scope,
			fixture.Connection.ConnectionID, contract.LaneDirectRead, "worker-plan-concurrent",
		)
		bound := bindPermitWorkerRun(t, fixture, temporal, fixture.Scope, fixture.Actor, admitted.RunRef)

		type loadOutcome struct {
			snapshot contract.WorkflowRunSnapshot
			err      error
		}
		outcomes := make([]loadOutcome, 2)
		var wait sync.WaitGroup
		for index := range outcomes {
			wait.Add(1)
			go func(index int) {
				defer wait.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				outcomes[index].snapshot, outcomes[index].err = service.LoadWorkflow(
					ctx, bound.execution, bound.reference,
				)
			}(index)
		}
		wait.Wait()

		planned := map[contract.DispatchID]int{}
		for index, outcome := range outcomes {
			if outcome.err != nil {
				t.Fatalf("concurrent load %d error = %v; want a bounded pending or planned result", index, outcome.err)
			}
			for _, unit := range outcome.snapshot.Units {
				planned[unit.DispatchID]++
			}
		}
		if len(planned) != 1 {
			t.Fatalf("concurrent loads planned %#v; want exactly one dispatch identity", planned)
		}
		if counts := countPlanningRows(t, fixture, fixture.Scope, admitted.RunRef); counts != (planningDurableCounts{dispatches: 1, effects: 1, members: 1}) {
			t.Fatalf("concurrent loads produced %#v; want one effect, dispatch and member", counts)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		snapshot, err := service.LoadWorkflow(ctx, bound.execution, bound.reference)
		if err != nil {
			t.Fatalf("load after concurrent planning: %v", err)
		}
		if snapshot.Done || len(snapshot.Units) != 1 || planned[snapshot.Units[0].DispatchID] == 0 {
			t.Fatalf("post-concurrency units = %#v; want the one durable planned dispatch", snapshot.Units)
		}
	})

	t.Run("cancelled and terminal runs are not planned", func(t *testing.T) {
		fixture, selection, _ := newPlannableDirectReadFixture(t, "fixture.plan.cancel")
		service := newPermitWorkerService(t, fixture, temporal)
		admitted := admitPlanningRun(
			t, fixture, selection, fixture.Client, fixture.Scope,
			fixture.Connection.ConnectionID, contract.LaneDirectRead, "worker-plan-cancelled",
		)
		bound := bindPermitWorkerRun(t, fixture, temporal, fixture.Scope, fixture.Actor, admitted.RunRef)

		before := getRunViewForTest(t, fixture.Client, runPathForTest(fixture, admitted.RunRef))
		response := sendRunCancel(
			t, fixture, fixture.Client, admitted.RunRef, "worker-plan-cancelled", before.Revision, true,
		)
		if response.status != http.StatusAccepted {
			t.Fatalf("cancel status = %d; want 202: %s", response.status, response.document)
		}
		cancelled := decodeRunRoute[api.RunCancelResponse](t, response)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		applied, err := service.ApplyCommand(ctx, bound.execution, contract.WorkflowCommandRequest{
			Run: bound.reference, Command: contract.WorkflowCommandRef{CommandID: cancelled.CommandRef},
		})
		if err != nil || !applied.CancelRequested {
			t.Fatalf("apply durable cancel = %#v error %v; want applied cancellation", applied, err)
		}

		snapshot, err := service.LoadWorkflow(ctx, bound.execution, bound.reference)
		if err != nil {
			t.Fatalf("load cancelled run: %v", err)
		}
		if !snapshot.CancelRequested || len(snapshot.Units) != 0 {
			t.Fatalf("cancelled snapshot = %#v; want cancellation without a planned unit", snapshot)
		}
		if counts := countPlanningRows(t, fixture, fixture.Scope, admitted.RunRef); counts != (planningDurableCounts{}) {
			t.Fatalf("cancelled run planned %#v; want no durable dispatch intent", counts)
		}

		projection, err := service.FinalizeRunProjection(ctx, bound.execution, contract.WorkflowProjectionRequest{
			Run: bound.reference,
		})
		if err != nil || projection.State != contract.RunStateCancelled {
			t.Fatalf("cancel projection = %#v error %v; want terminal cancellation", projection, err)
		}
		snapshot, err = service.LoadWorkflow(ctx, bound.execution, bound.reference)
		if err != nil {
			t.Fatalf("load terminal cancelled run: %v", err)
		}
		if !snapshot.Done || len(snapshot.Units) != 0 {
			t.Fatalf("terminal snapshot = %#v; want done without units", snapshot)
		}
		if counts := countPlanningRows(t, fixture, fixture.Scope, admitted.RunRef); counts != (planningDurableCounts{}) {
			t.Fatalf("terminal run planned %#v; want no durable dispatch intent", counts)
		}
	})

	t.Run("expired accepted lifetime is not planned", func(t *testing.T) {
		admission := httpDirectReadAdmissionPolicy()
		admission.RunLifetime = 250 * time.Millisecond
		fixture, selection, _ := newControlledDirectReadFixture(t, "fixture.plan.expired", admission)
		service := newPermitWorkerService(t, fixture, temporal)
		admitted := admitPlanningRun(
			t, fixture, selection, fixture.Client, fixture.Scope,
			fixture.Connection.ConnectionID, contract.LaneDirectRead, "worker-plan-expired",
		)
		bound := bindPermitWorkerRun(t, fixture, temporal, fixture.Scope, fixture.Actor, admitted.RunRef)
		awaitAcceptedLifetimeExpiry(t, fixture, fixture.Scope, fixture.Actor, admitted.RunRef)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := service.LoadWorkflow(ctx, bound.execution, bound.reference); !errors.Is(err, app.ErrWorkerAuthorization) {
			t.Fatalf("expired lifetime load error = %v; want ErrWorkerAuthorization", err)
		}
		if counts := countPlanningRows(t, fixture, fixture.Scope, admitted.RunRef); counts != (planningDurableCounts{}) {
			t.Fatalf("expired run planned %#v; want no durable dispatch intent", counts)
		}
	})
}

// TestWorkerDispatchPlanningRefusals proves every unsupported lane and transport
// fails closed as capability unavailable instead of planning a speculative
// dispatch.
func TestWorkerDispatchPlanningRefusals(t *testing.T) {
	temporal := newPermitTemporalHarness(t)

	t.Run("non-direct-read lane", func(t *testing.T) {
		selection := httpDirectReadSelection("fixture.plan.lane", "https://provider.test", http.StatusOK, false)
		eligibility := httpDirectReadLaneEligibility()
		for index := range eligibility {
			if eligibility[index].Lane == contract.LaneDirectWrite {
				eligibility[index].Status = contract.LaneEligible
				eligibility[index].Reason = "fixture-direct-write"
			}
		}
		selection.Operation.LaneEligibility = eligibility
		fixture := newPlanningFixture(
			t, selection,
			func(registry *app.HandlerRegistry) error {
				registration := app.NewHTTPDirectReadRegistration(app.HTTPDirectReadDependencies{
					Resolver: httpDirectReadResolver{}, Dialer: &httpDirectReadDialer{address: "127.0.0.1:1"},
				})
				registration.Lanes = append(registration.Lanes, contract.LaneDirectWrite)
				return registry.RegisterHTTP(registration)
			},
			httpDirectReadPolicy("https://provider.test"), httpDirectReadAdmissionPolicy(),
		)
		service := newPermitWorkerService(t, fixture, temporal)
		admitted := admitPlanningRun(
			t, fixture, selection, fixture.Client, fixture.Scope,
			fixture.Connection.ConnectionID, contract.LaneDirectWrite, "worker-plan-write-lane",
		)
		bound := bindPermitWorkerRun(t, fixture, temporal, fixture.Scope, fixture.Actor, admitted.RunRef)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := service.LoadWorkflow(ctx, bound.execution, bound.reference); !errors.Is(err, app.ErrWorkerCapabilityUnavailable) {
			t.Fatalf("non-direct-read lane error = %v; want ErrWorkerCapabilityUnavailable", err)
		}
		if permits, attempts := permitTestDurableCounts(t, fixture, fixture.Scope, admitted.RunRef); permits != 0 || attempts != 0 {
			t.Fatalf("refused lane created send authority: permits %d attempts %d", permits, attempts)
		}
		if counts := countPlanningRows(t, fixture, fixture.Scope, admitted.RunRef); counts != (planningDurableCounts{}) {
			t.Fatalf("refused lane planned %#v; want no durable dispatch intent", counts)
		}
	})

	t.Run("non-HTTP transport", func(t *testing.T) {
		sqlRegistration := sqlRegistrationForTest(controlledCallbackForRegistryTest("fixture.plan.sql"), nil)
		operation := operationForRegistryTest(
			contract.TransportKindPostgreSQL, contract.LaneDirectRead, sqlRegistration.Capabilities,
		)
		operation.OperationID = "fixture.plan.postgres"
		selection := app.OperationSelection{Operation: operation, SchemaRefs: []string{"fixture-schema-v1"}}
		fixture := newPlanningFixture(
			t, selection,
			func(registry *app.HandlerRegistry) error {
				return registry.RegisterSQL(sqlRegistration)
			},
			httpDirectReadPolicy("https://provider.test"), httpDirectReadAdmissionPolicy(),
		)
		service := newPermitWorkerService(t, fixture, temporal)
		admitted := admitPlanningRun(
			t, fixture, selection, fixture.Client, fixture.Scope,
			fixture.Connection.ConnectionID, contract.LaneDirectRead, "worker-plan-postgres-transport",
		)
		bound := bindPermitWorkerRun(t, fixture, temporal, fixture.Scope, fixture.Actor, admitted.RunRef)

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := service.LoadWorkflow(ctx, bound.execution, bound.reference); !errors.Is(err, app.ErrWorkerCapabilityUnavailable) {
			t.Fatalf("non-HTTP transport error = %v; want ErrWorkerCapabilityUnavailable", err)
		}
		if permits, attempts := permitTestDurableCounts(t, fixture, fixture.Scope, admitted.RunRef); permits != 0 || attempts != 0 {
			t.Fatalf("refused transport created send authority: permits %d attempts %d", permits, attempts)
		}
		if counts := countPlanningRows(t, fixture, fixture.Scope, admitted.RunRef); counts != (planningDurableCounts{}) {
			t.Fatalf("refused transport planned %#v; want no durable dispatch intent", counts)
		}
	})
}

// newPlannableDirectReadFixture is the real HTTP direct-read fixture: the same
// production registration, resolver, admission service and sealed HTTP policy
// the composed server uses for one GitHub-style bounded direct read.
func newPlannableDirectReadFixture(
	t *testing.T,
	operationID string,
) (*testfixture.Fixture, app.OperationSelection, contract.HTTPExecutionPolicy) {
	t.Helper()
	selection := httpDirectReadSelection(operationID, "https://provider.test", http.StatusOK, false)
	policy := httpDirectReadPolicy("https://provider.test")
	fixture := newHTTPDirectReadFixture(t, selection, policy, app.HTTPDirectReadDependencies{
		Resolver: httpDirectReadResolver{}, Dialer: &httpDirectReadDialer{address: "127.0.0.1:1"},
	}, nil)
	return fixture, selection, policy
}

// newControlledDirectReadFixture is the same direct-read contract under one
// caller-supplied admission policy, so lifetime boundaries are observed rather
// than assumed.
func newControlledDirectReadFixture(
	t *testing.T,
	operationID string,
	admissionPolicy store.RunBudgetPolicy,
) (*testfixture.Fixture, app.OperationSelection, contract.HTTPExecutionPolicy) {
	t.Helper()
	selection := httpDirectReadSelection(operationID, "https://provider.test", http.StatusOK, false)
	policy := httpDirectReadPolicy("https://provider.test")
	fixture := newPlanningFixture(
		t, selection,
		func(registry *app.HandlerRegistry) error {
			return registry.RegisterHTTP(app.NewHTTPDirectReadRegistration(app.HTTPDirectReadDependencies{
				Resolver: httpDirectReadResolver{}, Dialer: &httpDirectReadDialer{address: "127.0.0.1:1"},
			}))
		},
		policy, admissionPolicy,
	)
	return fixture, selection, policy
}

// newPlanningFixture binds one controlled operation contract to the real
// authenticated fixture: the same registration and resolver serve admission and
// the production worker, exactly as the composed server does.
func newPlanningFixture(
	t *testing.T,
	selection app.OperationSelection,
	register func(*app.HandlerRegistry) error,
	httpPolicy contract.HTTPExecutionPolicy,
	admissionPolicy store.RunBudgetPolicy,
) *testfixture.Fixture {
	t.Helper()
	registry := &app.HandlerRegistry{}
	if err := register(registry); err != nil {
		t.Fatalf("register controlled planning operation: %v", err)
	}
	var resolver app.OperationResolver
	fixture := testfixture.NewOperation(t, selection.Operation.OperationID, func(
		dependencies testfixture.ServiceDependencies,
		configuration *httpapi.ServiceConfig,
	) error {
		resolver = func(
			ctx context.Context,
			scope contract.Scope,
			connectionID string,
			operationID string,
		) (app.OperationSelection, error) {
			if ctx == nil || ctx.Err() != nil {
				return app.OperationSelection{}, context.Canceled
			}
			primary := scope.Equal(dependencies.Scope) && connectionID == dependencies.Connection.ConnectionID
			foreign := scope.Equal(dependencies.ForeignScope) && connectionID == dependencies.ForeignConnection.ConnectionID
			if operationID != selection.Operation.OperationID || !primary && !foreign {
				return app.OperationSelection{}, errors.New("controlled planning operation is unavailable")
			}
			return selection, nil
		}
		admission, err := app.NewAdmissionService(app.AdmissionServiceConfig{
			Pool:              dependencies.Pool,
			DeploymentID:      dependencies.DeploymentID,
			Authorize:         configuration.Auth.AuthorizeResource,
			Registry:          registry,
			ResolveOperation:  resolver,
			ResolveHTTPPolicy: controlledPlanningHTTPPolicy(dependencies, httpPolicy),
			Policy:            admissionPolicy,
			Random:            cryptorand.Reader,
		})
		if err != nil {
			return err
		}
		configuration.Admission = admission
		return nil
	})
	fixture.WorkerRegistry = registry
	fixture.ResolveOperation = resolver
	return fixture
}

func controlledPlanningHTTPPolicy(
	dependencies testfixture.ServiceDependencies,
	policy contract.HTTPExecutionPolicy,
) func(contract.Scope, string) (contract.HTTPExecutionPolicy, bool) {
	return func(scope contract.Scope, connectionID string) (contract.HTTPExecutionPolicy, bool) {
		primary := scope.Equal(dependencies.Scope) && connectionID == dependencies.Connection.ConnectionID
		foreign := scope.Equal(dependencies.ForeignScope) && connectionID == dependencies.ForeignConnection.ConnectionID
		if !primary && !foreign {
			return contract.HTTPExecutionPolicy{}, false
		}
		return policy, true
	}
}

// admitPlanningRun admits the controlled run under one exact lane. It mirrors the
// admitted request contract; the server recomputes the fingerprint from the
// typed request, so the claimed intent digest is deliberately inert.
func admitPlanningRun(
	t *testing.T,
	fixture *testfixture.Fixture,
	selection app.OperationSelection,
	client *http.Client,
	scope contract.Scope,
	connectionID string,
	lane contract.Lane,
	requestKey string,
) api.AdmissionResponse {
	t.Helper()
	revision, err := app.OperationSelectionRevision(selection)
	if err != nil {
		t.Fatalf("resolve controlled operation revision: %v", err)
	}
	invocation := api.InvocationRequest{
		ConnectionRef:  connectionID,
		OperationRef:   selection.Operation.OperationID,
		Lane:           api.InvocationRequestLane(lane),
		Input:          contract.NewObjectValue(map[string]contract.PresentValue{}),
		ResultControls: contract.ResultControls{},
		Protocol: api.ProtocolMetadata{
			WireVersion:      api.ProtocolMetadataWireVersion(api.WireVersion),
			RequiredFeatures: []string{},
			RequestKey:       requestKey,
			IntentDigest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			ExpectedRevision: revision,
		},
	}
	return requireFreshInitialAdmission(
		t, submitInitialInvocation(t, fixture, client, scope.WorkspaceID, invocation), requestKey,
	)
}

// planWorkerRun binds the real workflow execution and asks the production load
// path for a runnable unit. It is the whole production handoff: no fixture-owned
// dispatch is supplied.
func planWorkerRun(
	t *testing.T,
	fixture *testfixture.Fixture,
	service *app.WorkerService,
	temporal *permitTemporalHarness,
	scope contract.Scope,
	actor contract.Actor,
	admitted api.AdmissionResponse,
) plannedWorkerRun {
	t.Helper()
	bound := bindPermitWorkerRun(t, fixture, temporal, scope, actor, admitted.RunRef)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	snapshot, err := service.LoadWorkflow(ctx, bound.execution, bound.reference)
	if err != nil {
		t.Fatalf("plan accepted worker run: %v", err)
	}
	if snapshot.Done || len(snapshot.Units) != 1 {
		t.Fatalf("planned snapshot = %#v; want exactly one runnable unit", snapshot)
	}
	run := plannedWorkerRun{
		execution: bound.execution, reference: bound.reference, unit: snapshot.Units[0],
	}
	err = store.WithinScope(ctx, fixture.Pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		run.dispatch, err = store.LoadDispatch(ctx, tx, store.TaskRef{
			RunID: run.reference.RunID, TaskID: run.unit.TaskID,
		}, run.unit.DispatchID)
		return err
	})
	if err != nil {
		t.Fatalf("load production-planned dispatch: %v", err)
	}
	return run
}

func loadPlannedDirectReadFacts(
	t *testing.T,
	fixture *testfixture.Fixture,
	scope contract.Scope,
	actor contract.Actor,
	runID, taskID string,
) plannedDirectReadFacts {
	t.Helper()
	var facts plannedDirectReadFacts
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.WithinScope(ctx, fixture.Pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		if facts.run, err = store.LoadRun(ctx, tx, runID); err != nil {
			return err
		}
		if facts.task, err = store.LoadTask(ctx, tx, store.TaskRef{RunID: runID, TaskID: taskID}); err != nil {
			return err
		}
		if facts.input, err = store.LoadRunInputSnapshot(ctx, tx, runID); err != nil {
			return err
		}
		if facts.grant, err = store.LoadRunExecutionGrant(ctx, tx, runID); err != nil {
			return err
		}
		connection, err := store.LoadConnection(ctx, tx, facts.input.ConnectionID)
		if err != nil {
			return err
		}
		facts.account, err = store.LoadProviderAccount(ctx, tx, connection.ProviderAccountID)
		return err
	}); err != nil {
		t.Fatalf("load planned direct-read facts: %v", err)
	}
	return facts
}

func countPlanningRows(
	t *testing.T,
	fixture *testfixture.Fixture,
	scope contract.Scope,
	runID string,
) planningDurableCounts {
	t.Helper()
	var counts planningDurableCounts
	fixture.SetupSQL(t, scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM pm.effects
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
		`, scope.OrgID, scope.WorkspaceID, runID).Scan(&counts.effects); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM pm.dispatches
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
		`, scope.OrgID, scope.WorkspaceID, runID).Scan(&counts.dispatches); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM pm.dispatch_members
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
		`, scope.OrgID, scope.WorkspaceID, runID).Scan(&counts.members)
	})
	return counts
}

// awaitAcceptedLifetimeExpiry waits until the durable accepted lifetime of the
// run has elapsed, so the expiry case is observed rather than assumed.
func awaitAcceptedLifetimeExpiry(
	t *testing.T,
	fixture *testfixture.Fixture,
	scope contract.Scope,
	actor contract.Actor,
	runID string,
) {
	t.Helper()
	bound := time.Now().Add(30 * time.Second)
	for {
		expired := false
		var observed time.Time
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := store.WithinScope(ctx, fixture.Pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
			acceptedAt, err := store.LoadWorkerAcceptedAt(ctx, tx, runID)
			if err != nil {
				return err
			}
			budget, err := store.LoadRunBudgetSnapshot(ctx, tx, runID)
			if err != nil {
				return err
			}
			current, err := store.ReadAuthenticationTime(ctx, tx)
			if err != nil {
				return err
			}
			observed = current
			expired = !current.Before(acceptedAt.Add(budget.Policy.RunLifetime))
			return nil
		})
		cancel()
		if err != nil {
			t.Fatalf("observe accepted run lifetime: %v", err)
		}
		if expired {
			return
		}
		if time.Now().After(bound) {
			t.Fatalf("accepted run lifetime did not expire by %s", observed)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
