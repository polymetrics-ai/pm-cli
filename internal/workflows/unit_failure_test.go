package workflows_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	sdkclient "go.temporal.io/sdk/client"
	sdktemporal "go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	sdkworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
	pmworkflows "github.com/polymetrics-ai/pm-cli/internal/workflows"
)

// The public names of issue #296 are literal here on purpose. The tests then
// compile against the code before the change, and a rename of a public value
// fails them.
const (
	unitFailureEndActivityName = "pm.end-run-after-unit-failure.v1"
	unitActivityFailedReason   = "unit_activity_failed"
	workerRunProjectedEvent    = "worker_run_projected"
)

// TestRunEndsWhenUnitActivityRetriesRunOut runs the production worker with
// PostgreSQL and Temporal. A unit activity fails until its ScheduleToClose
// ends. The run must then reach an end state that the run view shows, and no
// provider request may start again.
func TestRunEndsWhenUnitActivityRetriesRunOut(t *testing.T) {
	t.Run("possibly_sent_unit_keeps_the_run_reconciling", func(t *testing.T) {
		controller := newHungExecutionController()
		fixture := testfixture.NewExecuting(t, controller.Execute, controller.Reconcile)
		harness := newProductionWorkflowHarness(t, fixture, "")
		// Registered after the harness, so it runs before the worker stops.
		t.Cleanup(controller.releaseCallbacks)
		admitted := admitWorkflowInput(t, fixture, "unit-retries-run-out-possibly-sent")
		run, input := harness.relayAdmitted(t, admitted)

		awaitFailedWorkflow(t, harness, run)

		view := getRunView(t, fixture, input.RunID)
		if view.State != api.RunStateReconciling {
			t.Fatalf("state = %q; want %q", view.State, api.RunStateReconciling)
		}
		if view.UnknownEffectCount != "1" || view.EffectSummary.Unknown != "1" {
			t.Fatalf("unknown effects = %q, summary %#v; want exactly one", view.UnknownEffectCount, view.EffectSummary)
		}
		if view.OrchestrationCondition != api.OrchestrationConditionFailed {
			t.Fatalf("orchestration condition = %q; want %q", view.OrchestrationCondition, api.OrchestrationConditionFailed)
		}
		if calls := controller.executeCalls.Load(); calls != 1 {
			t.Fatalf("provider execute calls = %d; want one, with no resend", calls)
		}
		assertUnitFailureEvent(t, fixture, input.RunID)
	})

	t.Run("never_sent_unit_ends_the_run_failed", func(t *testing.T) {
		controller := newHungExecutionController()
		fixture := testfixture.NewExecuting(t, controller.Execute, controller.Reconcile)
		harness := newProductionWorkflowHarness(t, fixture, "")
		t.Cleanup(controller.releaseCallbacks)
		admitted := admitWorkflowInput(t, fixture, "unit-retries-run-out-never-sent")
		// Prepare locks the provider account row before it takes the permit and
		// records the attempt. Admission, planning and the end of the run do not
		// lock it. So every unit attempt waits here until its context ends.
		releaseAccount, lockDone := holdProviderAccountRow(t, fixture)
		run, input := harness.relayAdmitted(t, admitted)

		awaitFailedWorkflow(t, harness, run)
		releaseAccount()
		if err := <-lockDone; err != nil {
			t.Fatalf("hold provider account row: %v", err)
		}

		view := getRunView(t, fixture, input.RunID)
		if view.State != api.RunStateFailed {
			t.Fatalf("state = %q; want %q", view.State, api.RunStateFailed)
		}
		if view.EffectSummary.Unsent != "1" || view.EffectSummary.Unknown != "0" || view.UnknownEffectCount != "0" {
			t.Fatalf("effect summary = %#v, unknown %q; want one unsent effect and no unknown effect",
				view.EffectSummary, view.UnknownEffectCount)
		}
		if view.OrchestrationCondition != api.OrchestrationConditionFailed {
			t.Fatalf("orchestration condition = %q; want %q", view.OrchestrationCondition, api.OrchestrationConditionFailed)
		}
		if calls := controller.executeCalls.Load() + controller.reconcileCalls.Load(); calls != 0 {
			t.Fatalf("provider callbacks = %d; want none", calls)
		}
		assertUnitFailureEvent(t, fixture, input.RunID)
	})
}

// TestUnitFailureHistoryReplays records the history that the coordinator
// before issue #296 wrote when a unit activity failed. The current
// coordinator must replay it without a new command.
func TestUnitFailureHistoryReplays(t *testing.T) {
	input := syntheticWorkflowInput(t, "unit-failure-replay")
	timeouts := testActivityTimeouts()
	unit := contract.WorkflowUnitRef{TaskID: "unit-failure-replay-task"}
	deadline := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	load := func(_ context.Context, reference contract.WorkflowRunRef) (contract.WorkflowRunSnapshot, error) {
		if reference != input.RunRef() {
			return contract.WorkflowRunSnapshot{}, errors.New("retained history loaded a different run")
		}
		snapshot := contract.WorkflowRunSnapshot{
			Run: reference, Deadline: deadline, Timeouts: timeouts,
			Units: []contract.WorkflowUnitRef{unit}, Commands: []contract.WorkflowCommandRef{},
			Fanout: 1,
		}
		return snapshot, snapshot.Validate()
	}
	execute := func(context.Context, contract.WorkflowUnitRequest) (contract.WorkflowUnitResult, error) {
		return contract.WorkflowUnitResult{}, sdktemporal.NewNonRetryableApplicationError(
			"worker state conflict", "pm.worker.conflict", nil,
		)
	}
	// The coordinator before issue #296 recorded the deadline version marker,
	// loaded the run, executed the unit and failed with the unit error.
	retainedWorkflow := func(ctx workflow.Context, admitted contract.StartRunInput) (contract.WorkflowProjectionResult, error) {
		_ = workflow.GetVersion(
			ctx, contract.WorkflowRunDeadlineProjectionChangeID,
			workflow.DefaultVersion, contract.WorkflowRunDeadlineProjectionVersion,
		)
		loadCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			ScheduleToCloseTimeout: timeouts.ScheduleToClose,
			StartToCloseTimeout:    timeouts.StartToClose,
			WaitForCancellation:    true,
		})
		var snapshot contract.WorkflowRunSnapshot
		if err := workflow.ExecuteActivity(loadCtx, contract.LoadActivityName, admitted.RunRef()).Get(loadCtx, &snapshot); err != nil {
			return contract.WorkflowProjectionResult{}, err
		}
		executeCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			ScheduleToCloseTimeout: timeouts.ScheduleToClose,
			StartToCloseTimeout:    timeouts.StartToClose,
			HeartbeatTimeout:       timeouts.Heartbeat,
			WaitForCancellation:    true,
		})
		var result contract.WorkflowUnitResult
		err := workflow.ExecuteActivity(executeCtx, contract.ExecuteActivityName, contract.WorkflowUnitRequest{
			Run: admitted.RunRef(), Unit: snapshot.Units[0],
		}).Get(executeCtx, &result)
		return contract.WorkflowProjectionResult{}, err
	}
	harness := newWorkflowHarness(t, "", func(worker sdkworker.Worker) {
		worker.RegisterWorkflowWithOptions(retainedWorkflow, workflow.RegisterOptions{Name: contract.RunWorkflowType})
		worker.RegisterActivityWithOptions(load, activity.RegisterOptions{Name: contract.LoadActivityName})
		worker.RegisterActivityWithOptions(execute, activity.RegisterOptions{Name: contract.ExecuteActivityName})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run, err := harness.client.ExecuteWorkflow(ctx, sdkclient.StartWorkflowOptions{
		ID: input.WorkflowID, TaskQueue: contract.ControlTaskQueue,
	}, contract.RunWorkflowType, input)
	if err != nil {
		t.Fatal(err)
	}
	testfixture.CleanupWorkflowChain(t, harness.client, harness.namespace, input.WorkflowID, run.GetRunID())
	var result contract.WorkflowProjectionResult
	if err := run.Get(ctx, &result); err == nil {
		t.Fatal("retained unit failure workflow completed; want the unit failure")
	}
	history := readWorkflowHistory(t, ctx, harness.client, input.WorkflowID, run.GetRunID())
	events := history.GetEvents()
	if len(events) == 0 || events[len(events)-1].GetEventType() != enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_FAILED {
		t.Fatal("retained unit failure history does not end with a failed workflow")
	}
	coordinator, err := pmworkflows.NewCoordinator(timeouts)
	if err != nil {
		t.Fatal(err)
	}
	replayer := sdkworker.NewWorkflowReplayer()
	replayer.RegisterWorkflowWithOptions(coordinator.PMRunWorkflow, workflow.RegisterOptions{Name: contract.RunWorkflowType})
	if err := replayer.ReplayWorkflowHistoryWithOptions(nil, history, sdkworker.ReplayWorkflowHistoryOptions{
		OriginalExecution: workflow.Execution{ID: input.WorkflowID, RunID: run.GetRunID()},
	}); err != nil {
		t.Fatalf("retained unit failure history no longer replays: %v", err)
	}
}

// TestUnitActivityFailureCallsTheEndActivity checks the workflow branch with
// the SDK test environment and controlled activities. It needs no stack.
func TestUnitActivityFailureCallsTheEndActivity(t *testing.T) {
	t.Run("a non-retryable unit failure ends the run once", func(t *testing.T) {
		activities := newUnitFailureActivities(t, "non-retryable")
		activities.executeErr = func() error {
			return sdktemporal.NewNonRetryableApplicationError("worker state conflict", "pm.worker.conflict", nil)
		}
		environment := newUnitFailureEnvironment(t, activities)

		environment.ExecuteWorkflow(contract.RunWorkflowType, activities.input)

		activities.assertEndedOnce(t)
		assertUnitActivityFailure(t, environment.GetWorkflowError())
		if attempts := activities.executeAttempts.Load(); attempts != 1 {
			t.Fatalf("unit attempts = %d; want one", attempts)
		}
	})

	t.Run("a unit whose retries run out ends the run once", func(t *testing.T) {
		activities := newUnitFailureActivities(t, "retries-run-out")
		activities.executeErr = func() error {
			return sdktemporal.NewApplicationError("worker operation is temporarily unavailable", "pm.worker.transient")
		}
		environment := newUnitFailureEnvironment(t, activities)

		environment.ExecuteWorkflow(contract.RunWorkflowType, activities.input)

		activities.assertEndedOnce(t)
		assertUnitActivityFailure(t, environment.GetWorkflowError())
		if attempts := activities.executeAttempts.Load(); attempts < 2 {
			t.Fatalf("unit attempts = %d; want the retries that ScheduleToClose allows", attempts)
		}
	})

	t.Run("a failed end activity keeps the unit failure", func(t *testing.T) {
		activities := newUnitFailureActivities(t, "end-fails")
		activities.executeErr = func() error {
			return sdktemporal.NewNonRetryableApplicationError("worker state conflict", "pm.worker.conflict", nil)
		}
		activities.endErr = sdktemporal.NewNonRetryableApplicationError("worker state conflict", "pm.worker.conflict", nil)
		environment := newUnitFailureEnvironment(t, activities)

		environment.ExecuteWorkflow(contract.RunWorkflowType, activities.input)

		activities.assertEndedOnce(t)
		assertUnitActivityFailure(t, environment.GetWorkflowError())
	})

	t.Run("a mismatched unit result does not end the run", func(t *testing.T) {
		activities := newUnitFailureActivities(t, "mismatched-result")
		activities.mismatchedResult = true
		environment := newUnitFailureEnvironment(t, activities)

		environment.ExecuteWorkflow(contract.RunWorkflowType, activities.input)

		if err := environment.GetWorkflowError(); err == nil {
			t.Fatal("mismatched unit result completed the workflow; want a failure")
		}
		if ends := activities.endRequests(); len(ends) != 0 {
			t.Fatalf("end activity requests = %#v; want none", ends)
		}
	})
}

type hungExecutionController struct {
	release        chan struct{}
	releaseOnce    sync.Once
	executeCalls   atomic.Uint32
	reconcileCalls atomic.Uint32
}

var errHungCallbackReleased = errors.New("controlled provider callback released after its activity ended")

func newHungExecutionController() *hungExecutionController {
	return &hungExecutionController{release: make(chan struct{})}
}

// Execute does not return before the test releases it, and it ignores its
// context, like a provider call that hangs.
func (controller *hungExecutionController) Execute(
	_ context.Context,
	_ app.ExecutionRequest,
) (app.ExecutionObservation, error) {
	controller.executeCalls.Add(1)
	<-controller.release
	return app.ExecutionObservation{}, errHungCallbackReleased
}

func (controller *hungExecutionController) Reconcile(
	_ context.Context,
	_ app.ExecutionRequest,
) (app.ExecutionObservation, error) {
	controller.reconcileCalls.Add(1)
	<-controller.release
	return app.ExecutionObservation{}, errHungCallbackReleased
}

func (controller *hungExecutionController) releaseCallbacks() {
	controller.releaseOnce.Do(func() { close(controller.release) })
}

// awaitFailedWorkflow waits once for the workflow result. It does not change
// the fixture activity timeouts.
func awaitFailedWorkflow(t *testing.T, harness *productionWorkflowHarness, run sdkclient.WorkflowRun) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var result contract.WorkflowProjectionResult
	if err := run.Get(ctx, &result); err == nil {
		t.Fatalf("workflow completed with %#v; want the unit failure", result)
	}
	description, err := harness.client.DescribeWorkflowExecution(ctx, run.GetID(), run.GetRunID())
	if err != nil {
		t.Fatal(err)
	}
	if status := description.GetWorkflowExecutionInfo().GetStatus(); status != enumspb.WORKFLOW_EXECUTION_STATUS_FAILED {
		t.Fatalf("workflow status = %s; want FAILED", status)
	}
}

// holdProviderAccountRow holds a row lock on the fixture connection's provider
// account until the returned function runs. FOR NO KEY UPDATE stops the
// permit's FOR UPDATE lock but not the foreign-key checks of other inserts.
func holdProviderAccountRow(t *testing.T, fixture *testfixture.Fixture) (func(), <-chan error) {
	t.Helper()
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	go func() {
		defer cancel()
		done <- fixture.WithinSetupSQL(ctx, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
			var marker int
			if err := tx.QueryRow(ctx, `
				SELECT 1
				FROM pm.provider_accounts
				WHERE org_id = $1 AND provider_account_id = $2
				FOR NO KEY UPDATE
			`, fixture.Scope.OrgID, fixture.Connection.ProviderAccountID).Scan(&marker); err != nil {
				return err
			}
			close(locked)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
				return nil
			}
		})
	}()
	select {
	case <-locked:
	case err := <-done:
		t.Fatalf("lock provider account row: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("provider account row lock was not acquired")
	}
	var once sync.Once
	releaseLock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseLock)
	return releaseLock, done
}

// assertUnitFailureEvent requires exactly one run projection event that names
// the unit activity failure.
func assertUnitFailureEvent(t *testing.T, fixture *testfixture.Fixture, runID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var matching int
	err := fixture.WithinSetupSQL(ctx, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT safe_payload
			FROM pm.server_events
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND event_type = $4
			ORDER BY sequence
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID, workerRunProjectedEvent)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var encoded []byte
			if err := rows.Scan(&encoded); err != nil {
				return err
			}
			var payload struct {
				ReasonCodes []string `json:"reason_codes"`
			}
			if err := json.Unmarshal(encoded, &payload); err != nil {
				return err
			}
			if slices.Contains(payload.ReasonCodes, unitActivityFailedReason) {
				matching++
			}
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read run projection events: %v", err)
	}
	if matching != 1 {
		t.Fatalf("%s events with %s = %d; want one", workerRunProjectedEvent, unitActivityFailedReason, matching)
	}
}

type unitFailureActivities struct {
	input            contract.StartRunInput
	unit             contract.WorkflowUnitRef
	deadline         time.Time
	executeErr       func() error
	endErr           error
	mismatchedResult bool

	executeAttempts atomic.Int32
	mu              sync.Mutex
	loads           int
	ends            []contract.WorkflowUnitRequest
	projections     int
}

func newUnitFailureActivities(t *testing.T, label string) *unitFailureActivities {
	t.Helper()
	start := time.Date(2026, time.November, 17, 9, 0, 0, 0, time.UTC)
	return &unitFailureActivities{
		input:    retryWorkflowInput(t, "unit-failure-"+label),
		unit:     contract.WorkflowUnitRef{TaskID: "unit-failure-task"},
		deadline: start.Add(time.Hour),
	}
}

func (activities *unitFailureActivities) Load(
	_ context.Context,
	reference contract.WorkflowRunRef,
) (contract.WorkflowRunSnapshot, error) {
	activities.mu.Lock()
	defer activities.mu.Unlock()
	activities.loads++
	units := []contract.WorkflowUnitRef{}
	done := activities.loads > 1
	if !done {
		units = append(units, activities.unit)
	}
	snapshot := contract.WorkflowRunSnapshot{
		Run: reference, Deadline: activities.deadline, Timeouts: testActivityTimeouts(),
		Units: units, Commands: []contract.WorkflowCommandRef{}, Done: done, Fanout: 1,
	}
	return snapshot, snapshot.Validate()
}

func (activities *unitFailureActivities) Execute(
	_ context.Context,
	request contract.WorkflowUnitRequest,
) (contract.WorkflowUnitResult, error) {
	activities.executeAttempts.Add(1)
	if activities.mismatchedResult {
		return contract.WorkflowUnitResult{
			TaskID: request.Unit.TaskID + "-other", State: contract.RunStateSucceeded, ResultRef: "unit-failure-result",
		}, nil
	}
	return contract.WorkflowUnitResult{}, activities.executeErr()
}

func (*unitFailureActivities) Command(
	context.Context,
	contract.WorkflowCommandRequest,
) (contract.WorkflowCommandResult, error) {
	return contract.WorkflowCommandResult{}, errors.New("unexpected command activity")
}

func (activities *unitFailureActivities) Project(
	_ context.Context,
	request contract.WorkflowProjectionRequest,
) (contract.WorkflowProjectionResult, error) {
	activities.mu.Lock()
	activities.projections++
	activities.mu.Unlock()
	return contract.WorkflowProjectionResult{RunID: request.Run.RunID, Revision: 1, State: contract.RunStateSucceeded}, nil
}

func (activities *unitFailureActivities) End(
	_ context.Context,
	request contract.WorkflowUnitRequest,
) (contract.WorkflowProjectionResult, error) {
	activities.mu.Lock()
	activities.ends = append(activities.ends, request)
	activities.mu.Unlock()
	if activities.endErr != nil {
		return contract.WorkflowProjectionResult{}, activities.endErr
	}
	return contract.WorkflowProjectionResult{RunID: request.Run.RunID, Revision: 2, State: contract.RunStateFailed}, nil
}

func (activities *unitFailureActivities) endRequests() []contract.WorkflowUnitRequest {
	activities.mu.Lock()
	defer activities.mu.Unlock()
	return slices.Clone(activities.ends)
}

func (activities *unitFailureActivities) assertEndedOnce(t *testing.T) {
	t.Helper()
	ends := activities.endRequests()
	if len(ends) != 1 {
		t.Fatalf("end activity calls = %d; want 1", len(ends))
	}
	want := contract.WorkflowUnitRequest{Run: activities.input.RunRef(), Unit: activities.unit}
	if ends[0] != want {
		t.Fatalf("end activity request = %#v; want %#v", ends[0], want)
	}
	activities.mu.Lock()
	defer activities.mu.Unlock()
	if activities.projections != 0 {
		t.Fatalf("final projection calls = %d; want none after a unit failure", activities.projections)
	}
}

func newUnitFailureEnvironment(t *testing.T, activities *unitFailureActivities) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	coordinator, err := pmworkflows.NewCoordinator(testActivityTimeouts())
	if err != nil {
		t.Fatal(err)
	}
	var suite testsuite.WorkflowTestSuite
	environment := suite.NewTestWorkflowEnvironment()
	environment.SetStartTime(activities.deadline.Add(-time.Hour))
	environment.RegisterWorkflowWithOptions(coordinator.PMRunWorkflow, workflow.RegisterOptions{Name: contract.RunWorkflowType})
	environment.RegisterActivityWithOptions(activities.Load, activity.RegisterOptions{Name: contract.LoadActivityName})
	environment.RegisterActivityWithOptions(activities.Execute, activity.RegisterOptions{Name: contract.ExecuteActivityName})
	environment.RegisterActivityWithOptions(activities.Command, activity.RegisterOptions{Name: contract.CommandActivityName})
	environment.RegisterActivityWithOptions(activities.Project, activity.RegisterOptions{Name: contract.ProjectActivityName})
	environment.RegisterActivityWithOptions(activities.End, activity.RegisterOptions{Name: unitFailureEndActivityName})
	return environment
}

// assertUnitActivityFailure requires that the workflow failed with the unit
// activity failure, not with the end activity failure.
func assertUnitActivityFailure(t *testing.T, err error) {
	t.Helper()
	var activityErr *sdktemporal.ActivityError
	if !errors.As(err, &activityErr) {
		t.Fatalf("workflow error = %v; want the unit activity failure", err)
	}
	if name := activityErr.ActivityType().GetName(); name != contract.ExecuteActivityName {
		t.Fatalf("workflow failed with activity %q; want %q", name, contract.ExecuteActivityName)
	}
}
