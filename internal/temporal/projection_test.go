package temporal

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	sdkclient "go.temporal.io/sdk/client"
	sdktemporal "go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

const controlledProjectionActivityName = "pm.test.retry-final-projection.v1"

type controlledProjectionActivity struct {
	attempts atomic.Int32

	mu                  sync.RWMutex
	pool                *store.Pool
	route               store.WorkflowRoute
	intent              store.WorkflowCommandIntent
	temporalRunID       string
	firstExecutionRunID string
	projected           chan time.Time
}

func (activityFixture *controlledProjectionActivity) configure(
	pool *store.Pool,
	route store.WorkflowRoute,
	intent store.WorkflowCommandIntent,
	temporalRunID string,
	firstExecutionRunID string,
) {
	activityFixture.mu.Lock()
	defer activityFixture.mu.Unlock()
	activityFixture.pool = pool
	activityFixture.route = route
	activityFixture.intent = intent
	activityFixture.temporalRunID = temporalRunID
	activityFixture.firstExecutionRunID = firstExecutionRunID
}

func (activityFixture *controlledProjectionActivity) execute(ctx context.Context, command ProjectionRetryCommand) error {
	if activityFixture.attempts.Add(1) == 1 {
		return sdktemporal.NewApplicationError("injected transient projection write", "PhysicalProjectionTransient")
	}
	activityFixture.mu.RLock()
	pool := activityFixture.pool
	route := activityFixture.route
	intent := activityFixture.intent
	temporalRunID := activityFixture.temporalRunID
	firstExecutionRunID := activityFixture.firstExecutionRunID
	activityFixture.mu.RUnlock()
	if pool == nil {
		return errors.New("controlled projection activity was not configured")
	}
	expected, err := projectionRetryCommand(intent)
	if err != nil {
		return err
	}
	if command != expected {
		return ErrWorkflowBindingMismatch
	}
	observedAt := time.Now().UTC()
	err = store.WithinScope(ctx, pool, route.Claim.Scope, route.Actor, func(ctx context.Context, tx *store.Transaction) error {
		_, err := store.CommitOrchestrationObservation(ctx, tx, store.OrchestrationObservationCommit{
			Claim:               route.Claim,
			IntentDigest:        intent.Digest,
			TemporalRunID:       temporalRunID,
			FirstExecutionRunID: firstExecutionRunID,
			Condition:           contract.OrchestrationRunning,
			ObservedAt:          observedAt,
			Acknowledge:         false,
		})
		return err
	})
	if err != nil {
		return err
	}
	select {
	case activityFixture.projected <- observedAt:
	default:
	}
	return nil
}

type capturedUpdate struct {
	updateID            string
	workflowID          string
	runID               string
	updateName          string
	firstExecutionRunID string
}

type recordingUpdateClient struct {
	WorkflowClient
	loseFirst atomic.Bool

	mu      sync.Mutex
	updates []capturedUpdate
}

func (client *recordingUpdateClient) UpdateWorkflow(
	ctx context.Context,
	options sdkclient.UpdateWorkflowOptions,
) (sdkclient.WorkflowUpdateHandle, error) {
	client.mu.Lock()
	client.updates = append(client.updates, capturedUpdate{
		updateID: options.UpdateID, workflowID: options.WorkflowID,
		runID: options.RunID, updateName: options.UpdateName,
		firstExecutionRunID: options.FirstExecutionRunID,
	})
	client.mu.Unlock()
	handle, err := client.WorkflowClient.UpdateWorkflow(ctx, options)
	if err == nil && client.loseFirst.CompareAndSwap(true, false) {
		return nil, errors.New("injected lost update response after Temporal completion")
	}
	return handle, err
}

func (client *recordingUpdateClient) captured() []capturedUpdate {
	client.mu.Lock()
	defer client.mu.Unlock()
	return append([]capturedUpdate(nil), client.updates...)
}

func TestClosedAbsentProjectionRepair(t *testing.T) {
	t.Run("R10_E02_C01_open_update_activity_retry", func(t *testing.T) {
		fixture := testfixture.NewAdmitting(t)
		workerClient := openPhysicalTemporal(t, randomPhysicalID(t, "r10-c01-worker-"))
		activityFixture := &controlledProjectionActivity{projected: make(chan time.Time, 1)}
		controlledWorker := worker.New(workerClient, ControlTaskQueue, worker.Options{})
		controlledWorker.RegisterWorkflowWithOptions(controlledOpenProjectionWorkflow, workflow.RegisterOptions{Name: RunWorkflowType})
		controlledWorker.RegisterActivityWithOptions(activityFixture.execute, activity.RegisterOptions{Name: controlledProjectionActivityName})
		if err := controlledWorker.Start(); err != nil {
			t.Fatalf("start controlled projection worker: %v", err)
		}
		t.Cleanup(controlledWorker.Stop)

		starterBackend := openPhysicalTemporal(t, randomPhysicalID(t, "r10-c01-starter-"))
		admitted, startResult, repairRoute := seedBoundPhysicalRun(t, fixture, "r10-c01-open-update", starterBackend)
		terminatePhysicalWorkflow(t, workerClient, startResult.WorkflowID, startResult.TemporalRunID)

		repairBackend := openPhysicalTemporal(t, randomPhysicalID(t, "r10-c01-repair-"))
		recorder := &recordingUpdateClient{WorkflowClient: repairBackend}
		recorder.loseFirst.Store(true)
		relay, _ := newPhysicalRelay(t, fixture, recorder, "r10-c01-repair-relay", physicalLongRouteTTL)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, intent, err := relay.sealRoute(ctx, repairRoute)
		if err != nil {
			t.Fatalf("seal controlled repair command: %v", err)
		}
		activityFixture.configure(
			fixture.Pool,
			repairRoute,
			intent,
			startResult.TemporalRunID,
			startResult.TemporalRunID,
		)

		if _, err := relay.RepairProjection(ctx, repairRoute); err == nil {
			t.Fatal("lost completed update response was reported as success")
		}
		var activityObservedAt time.Time
		select {
		case activityObservedAt = <-activityFixture.projected:
		case <-time.After(10 * time.Second):
			t.Fatal("controlled final-projection activity did not retry and commit")
		}
		if activityFixture.attempts.Load() != 2 {
			t.Fatalf("controlled projection activity attempts = %d; want one failure plus one retry", activityFixture.attempts.Load())
		}
		projected := loadPhysicalRun(t, fixture, admitted.RunRef)
		if projected.OrchestrationCondition != contract.OrchestrationRunning || !projected.ProjectionObservedAt.Equal(activityObservedAt) {
			t.Fatalf("activity PostgreSQL projection = condition:%q observed:%s; want exact activity commit %s", projected.OrchestrationCondition, projected.ProjectionObservedAt, activityObservedAt)
		}
		_, _, dispatches, effects, pending := workflowRouteRows(t, fixture, admitted.RunRef)
		if dispatches != 0 || effects != 0 || pending != 1 {
			t.Fatalf("open projection rows = dispatches:%d effects:%d pending:%d", dispatches, effects, pending)
		}

		result, err := relay.RepairProjection(ctx, repairRoute)
		if err != nil {
			t.Fatalf("retry same completed workflow update: %v", err)
		}
		if result.Disposition != ProjectionRetryCompleted || result.WorkflowID != startResult.WorkflowID || result.TemporalRunID != startResult.TemporalRunID {
			t.Fatalf("open projection recovery result = %#v", result)
		}
		expectedUpdateID, err := projectionUpdateID(intent)
		if err != nil {
			t.Fatalf("derive expected projection update identity: %v", err)
		}
		updates := recorder.captured()
		if len(updates) != 2 || updates[0] != updates[1] || updates[0] != (capturedUpdate{
			updateID: expectedUpdateID, workflowID: startResult.WorkflowID,
			runID: startResult.TemporalRunID, updateName: ProjectionUpdateName,
			firstExecutionRunID: startResult.TemporalRunID,
		}) {
			t.Fatalf("captured projection updates = %#v; want two identical canonical updates", updates)
		}
		if activityFixture.attempts.Load() != 2 {
			t.Fatalf("Temporal update de-duplication reran activity; attempts=%d", activityFixture.attempts.Load())
		}
	})

	t.Run("R10_E02_C02_closed_retained_history", func(t *testing.T) {
		fixture := testfixture.NewAdmitting(t)
		workerClient := openPhysicalTemporal(t, randomPhysicalID(t, "r10-c02-worker-"))
		controlledWorker := worker.New(workerClient, ControlTaskQueue, worker.Options{})
		controlledWorker.RegisterWorkflowWithOptions(controlledClosedProjectionWorkflow, workflow.RegisterOptions{Name: RunWorkflowType})
		if err := controlledWorker.Start(); err != nil {
			t.Fatalf("start controlled closed-workflow worker: %v", err)
		}
		t.Cleanup(controlledWorker.Stop)

		backend := openPhysicalTemporal(t, randomPhysicalID(t, "r10-c02-relay-"))
		admitted, startResult, repairRoute := seedBoundPhysicalRun(t, fixture, "r10-c02-closed-history", backend)
		closedAt := waitForPhysicalWorkflowClose(t, workerClient, startResult.WorkflowID, startResult.TemporalRunID)
		before := loadPhysicalRun(t, fixture, admitted.RunRef)
		_, _, beforeDispatches, beforeEffects, beforePending := workflowRouteRows(t, fixture, admitted.RunRef)
		if before.OrchestrationCondition != contract.OrchestrationRunning {
			t.Fatalf("pre-repair orchestration condition = %q; want stale running", before.OrchestrationCondition)
		}

		repairBackend := openPhysicalTemporal(t, randomPhysicalID(t, "r10-c02-repair-"))
		counter := &countingWorkflowClient{WorkflowClient: repairBackend}
		relay, _ := newPhysicalRelay(t, fixture, counter, "r10-c02-repair-relay", physicalLongRouteTTL)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		result, err := relay.RepairProjection(ctx, repairRoute)
		if err != nil {
			t.Fatalf("repair closed retained history: %v", err)
		}
		if result.Disposition != ProjectionRepairedFromHistory || result.Condition != contract.OrchestrationCompleted || counter.updateCalls.Load() != 0 || counter.executeCalls.Load() != 0 {
			t.Fatalf("closed projection result = %#v updates=%d executes=%d", result, counter.updateCalls.Load(), counter.executeCalls.Load())
		}
		after := loadPhysicalRun(t, fixture, admitted.RunRef)
		_, _, afterDispatches, afterEffects, afterPending := workflowRouteRows(t, fixture, admitted.RunRef)
		expectedObservedAt := closedAt
		if expectedObservedAt.Before(before.ProjectionObservedAt) {
			expectedObservedAt = before.ProjectionObservedAt
		}
		if after.OrchestrationCondition != contract.OrchestrationCompleted || !after.ProjectionObservedAt.Equal(expectedObservedAt) {
			t.Fatalf("closed projection = condition:%q observed:%s; want completed at monotonic history time %s", after.OrchestrationCondition, after.ProjectionObservedAt, expectedObservedAt)
		}
		if after.State != before.State || after.EffectSummary != before.EffectSummary ||
			after.CancelRequested != before.CancelRequested || after.UnknownEffectCount != before.UnknownEffectCount ||
			after.BlockedReason != before.BlockedReason || !reflect.DeepEqual(after.History, before.History) ||
			beforeDispatches != afterDispatches || beforeEffects != afterEffects || beforePending != afterPending ||
			afterPending != 1 {
			t.Fatalf("closed history repair changed business/effect/pending truth: before=%#v after=%#v rows=%d/%d/%d -> %d/%d/%d", before, after, beforeDispatches, beforeEffects, beforePending, afterDispatches, afterEffects, afterPending)
		}
	})

	t.Run("R10_E02_C03_never_bound_not_found", func(t *testing.T) {
		fixture := testfixture.NewAdmitting(t)
		admitted := admitPhysicalRun(t, fixture, "r10-c03-never-bound")
		route := claimPhysicalRoute(t, fixture, "r10-c03-relay", physicalLongRouteTTL)
		backend := openPhysicalTemporal(t, randomPhysicalID(t, "r10-c03-backend-"))
		counter := &countingWorkflowClient{WorkflowClient: backend}
		relay, _ := newPhysicalRelay(t, fixture, counter, "r10-c03-relay", physicalLongRouteTTL)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		result, err := relay.RepairProjection(ctx, route)
		if err != nil {
			t.Fatalf("classify never-bound real NotFound: %v", err)
		}
		if result.Disposition != ProjectionNeverStarted || result.WorkflowID != "pm/run/"+admitted.RunRef ||
			counter.describeCalls.Load() != 1 || counter.executeCalls.Load() != 0 || counter.updateCalls.Load() != 0 {
			t.Fatalf("never-bound projection result = %#v describes=%d executes=%d updates=%d", result, counter.describeCalls.Load(), counter.executeCalls.Load(), counter.updateCalls.Load())
		}
		var witnesses int
		fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				SELECT count(*) FROM pm.workflow_reconciliation_witnesses
				WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
				  AND kind = 'never_bound_no_dispatch'
			`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, admitted.RunRef).Scan(&witnesses)
		})
		_, _, dispatches, effects, pending := workflowRouteRows(t, fixture, admitted.RunRef)
		if witnesses != 1 || dispatches != 0 || effects != 0 || pending != 1 {
			t.Fatalf("never-started evidence = witnesses:%d dispatches:%d effects:%d pending:%d", witnesses, dispatches, effects, pending)
		}
	})

	t.Run("R10_E02_C04_prior_observation_not_found", func(t *testing.T) {
		fixture := testfixture.NewAdmitting(t)
		admitted := admitPhysicalRun(t, fixture, "r10-c04-prior-observation")
		startRoute := claimPhysicalRoute(t, fixture, "r10-c04-start-relay", physicalLongRouteTTL)
		backend := openPhysicalTemporal(t, randomPhysicalID(t, "r10-c04-backend-"))
		counter := &countingWorkflowClient{WorkflowClient: backend}
		startRelay, _ := newPhysicalRelay(t, fixture, counter, "r10-c04-start-relay", physicalLongRouteTTL)
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		_, intent, err := startRelay.sealRoute(ctx, startRoute)
		if err != nil {
			t.Fatalf("seal prior-observation fixture route: %v", err)
		}
		input, err := startRunInput(startRoute.Claim, intent)
		if err != nil {
			t.Fatalf("construct prior-observation start input: %v", err)
		}
		workflowID := intent.Plan.WorkflowID
		run, err := backend.ExecuteWorkflow(
			ctx,
			physicalWorkflowOptions(workflowID, intent.Plan.ExecutionTimeout),
			intent.Plan.WorkflowType,
			input,
		)
		if err != nil {
			t.Fatalf("start prior-observation physical workflow: %v", err)
		}
		deletedTemporalRunID := run.GetRunID()
		observedRunID, observedInput := observeWorkflowStart(t, backend, workflowID)
		if deletedTemporalRunID == "" || observedRunID != deletedTemporalRunID ||
			observedInput.RunID != admitted.RunRef || observedInput.WorkflowID != workflowID {
			t.Fatalf("prior-observation physical start = returned:%q observed:%q input:%#v", deletedTemporalRunID, observedRunID, observedInput)
		}

		if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, startRoute.Actor, func(ctx context.Context, tx *store.Transaction) error {
			_, err := store.CommitWorkflowBinding(ctx, tx, store.WorkflowBindingCommit{
				Claim:               startRoute.Claim,
				IntentDigest:        intent.Digest,
				TemporalRunID:       deletedTemporalRunID,
				FirstExecutionRunID: deletedTemporalRunID,
				Condition:           contract.OrchestrationRunning,
			})
			return err
		}); err != nil {
			t.Fatalf("persist actual prior Temporal observation fixture: %v", err)
		}
		terminatePhysicalWorkflow(t, backend, workflowID, deletedTemporalRunID)
		deletePhysicalWorkflowHistory(t, backend, workflowID, deletedTemporalRunID)
		repairRoute := claimPhysicalRoute(t, fixture, "r10-c04-repair-relay", physicalLongRouteTTL)
		repairRelay, _ := newPhysicalRelay(t, fixture, counter, "r10-c04-repair-relay", physicalLongRouteTTL)
		repairCtx, cancelRepair := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancelRepair()
		result, err := repairRelay.RepairProjection(repairCtx, repairRoute)
		if err != nil {
			t.Fatalf("classify prior-bound real NotFound: %v", err)
		}
		if result.Disposition != ProjectionUncertain || result.TemporalRunID != deletedTemporalRunID ||
			result.Condition != contract.OrchestrationUnknown || counter.describeCalls.Load() != 1 ||
			counter.executeCalls.Load() != 0 || counter.updateCalls.Load() != 0 {
			t.Fatalf("prior-observation projection result = %#v describes=%d executes=%d updates=%d", result, counter.describeCalls.Load(), counter.executeCalls.Load(), counter.updateCalls.Load())
		}
		record := loadPhysicalRun(t, fixture, admitted.RunRef)
		if len(record.WorkflowBinding.Observations) != 1 || record.WorkflowBinding.Observations[0].TemporalRunID != deletedTemporalRunID || record.OrchestrationCondition != contract.OrchestrationUnknown {
			t.Fatalf("prior observation was lost or restarted: %#v", record)
		}
		_, _, dispatches, effects, pending := workflowRouteRows(t, fixture, admitted.RunRef)
		if dispatches != 0 || effects != 0 || pending != 1 {
			t.Fatalf("uncertain projection rows = dispatches:%d effects:%d pending:%d", dispatches, effects, pending)
		}
	})
}

func controlledOpenProjectionWorkflow(ctx workflow.Context, input StartRunInput) error {
	if err := input.Validate(); err != nil {
		return err
	}
	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout:    3 * time.Second,
		ScheduleToCloseTimeout: 8 * time.Second,
		RetryPolicy: &sdktemporal.RetryPolicy{
			InitialInterval:    20 * time.Millisecond,
			BackoffCoefficient: 1,
			MaximumInterval:    20 * time.Millisecond,
			MaximumAttempts:    2,
		},
	}
	if err := workflow.SetUpdateHandler(ctx, ProjectionUpdateName, func(handlerContext workflow.Context, command ProjectionRetryCommand) error {
		if err := command.Validate(); err != nil {
			return err
		}
		activityContext := workflow.WithActivityOptions(handlerContext, activityOptions)
		return workflow.ExecuteActivity(activityContext, controlledProjectionActivityName, command).Get(activityContext, nil)
	}); err != nil {
		return err
	}
	return workflow.Await(ctx, func() bool { return false })
}

func controlledClosedProjectionWorkflow(_ workflow.Context, input StartRunInput) error {
	return input.Validate()
}

func seedBoundPhysicalRun(
	t testing.TB,
	fixture *testfixture.Fixture,
	key string,
	backend WorkflowClient,
) (api.AdmissionResponse, HandoffResult, store.WorkflowRoute) {
	t.Helper()
	admitted := admitPhysicalRun(t, fixture, key)
	startRoute := claimPhysicalRoute(t, fixture, key+"-start-relay", physicalLongRouteTTL)
	relay, _ := newPhysicalRelay(t, fixture, backend, key+"-start-relay", physicalLongRouteTTL)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	startResult, err := relay.StartOrReconcile(ctx, startRoute)
	if err != nil {
		t.Fatalf("start controlled physical workflow: %v", err)
	}
	if startResult.Disposition != HandoffStarted || startResult.WorkflowID != "pm/run/"+admitted.RunRef || startResult.TemporalRunID == "" {
		t.Fatalf("controlled workflow start result = %#v", startResult)
	}
	repairRoute := claimPhysicalRoute(t, fixture, key+"-repair-relay", physicalLongRouteTTL)
	if repairRoute.Claim.RunID != admitted.RunRef || repairRoute.Claim.Command.Kind != store.WorkflowCommandRepair {
		t.Fatalf("controlled repair route = %#v", repairRoute)
	}
	return admitted, startResult, repairRoute
}

func waitForPhysicalWorkflowClose(t testing.TB, client sdkclient.Client, workflowID, runID string) time.Time {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		description, err := client.DescribeWorkflowExecution(ctx, workflowID, runID)
		cancel()
		if err != nil {
			t.Fatalf("describe controlled closing workflow: %v", err)
		}
		if description.GetWorkflowExecutionInfo().GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED {
			ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
			iterator := client.GetWorkflowHistory(ctx, workflowID, runID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_CLOSE_EVENT)
			if iterator == nil || !iterator.HasNext() {
				cancel()
				t.Fatal("closed controlled workflow has no retained close event")
			}
			event, err := iterator.Next()
			cancel()
			if err != nil || event == nil || event.GetEventType() != enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_COMPLETED || event.GetEventTime() == nil {
				t.Fatalf("read controlled workflow close event: event=%v error=%v", event, err)
			}
			return event.GetEventTime().AsTime().UTC()
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("controlled Temporal workflow %q did not close before deadline", workflowID)
	return time.Time{}
}
