package workflows_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/activity"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	sdkworker "go.temporal.io/sdk/worker"
	"google.golang.org/protobuf/proto"

	pmactivities "github.com/polymetrics-ai/pm-cli/internal/activities"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	pmtemporal "github.com/polymetrics-ai/pm-cli/internal/temporal"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
	pmworkflows "github.com/polymetrics-ai/pm-cli/internal/workflows"
)

func TestCommandsAcrossContinueAsNew(t *testing.T) {
	t.Run("R11_C01_durable_cancel_recovers_without_notification", func(t *testing.T) {
		controller := newBlockingExecutionController()
		fixture := testfixture.NewExecuting(t, controller.Execute, controller.Reconcile)
		harness := newProductionWorkflowHarness(t, fixture, "")
		admitted := admitWorkflowInput(t, fixture, "durable-cancel-without-notification")
		run, input := harness.relayAdmitted(t, admitted)
		select {
		case <-controller.entered:
		case <-time.After(20 * time.Second):
			t.Fatal("production worker callback did not start")
		}
		view := getRunView(t, fixture, input.RunID)
		cancelled := cancelRun(t, fixture, input.RunID, "durable-cancel", view.Revision)
		if cancelled.CommandRef == "" || !bool(cancelled.CancelRequested) {
			t.Fatalf("cancel response = %#v", cancelled)
		}
		close(controller.release)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var result contract.WorkflowProjectionResult
		if err := run.Get(ctx, &result); err != nil {
			t.Fatal(err)
		}
		if calls := controller.executeCalls.Load(); calls != 1 {
			t.Fatalf("provider execute calls = %d; want one", calls)
		}
		after := getRunView(t, fixture, input.RunID)
		if !after.CancelRequested {
			t.Fatal("durable cancellation disappeared after a lost notification")
		}
		assertCommandAppliedOnce(t, fixture, input.RunID, cancelled.CommandRef)
	})
	t.Run("R11_C02_signal_and_update_at_rollover_apply_once", func(t *testing.T) {
		exerciseCommandAtRollover(t, false)
	})
	t.Run("R11_C03_buffered_handler_drains_once_with_original_identity_and_deadline", func(t *testing.T) {
		exerciseCommandAtRollover(t, true)
	})
	t.Run("R11_C04_server_suggestion_rolls_production_command_below_local_limits", func(t *testing.T) {
		exerciseCommandAtRollover(t, false)
	})

}

func exerciseCommandAtRollover(t *testing.T, holdHandler bool) {
	t.Helper()
	artifact := testfixture.NewWorkerConfiguration(t)
	namespace := testfixture.RolloverNamespace(t, artifact.Configuration).Name
	controller := newBlockingExecutionController()
	fixture := testfixture.NewExecuting(t, controller.Execute, controller.Reconcile)
	harness := newProductionWorkflowHarness(t, fixture, namespace)
	admitted := admitWorkflowInput(t, fixture, "rollover-command-"+fmt.Sprint(holdHandler))
	originalDeadline := durableRunDeadline(t, fixture, admitted.RunID)
	run, input := harness.relayAdmitted(t, admitted)
	firstRunID := run.GetRunID()
	select {
	case <-controller.entered:
	case <-time.After(20 * time.Second):
		t.Fatal("provider callback did not hold the rollover boundary")
	}
	view := getRunView(t, fixture, input.RunID)
	cancelled := cancelRun(t, fixture, input.RunID, "rollover-cancel-"+fmt.Sprint(holdHandler), view.Revision)
	command := contract.WorkflowCommandRef{CommandID: cancelled.CommandRef}
	if err := command.Validate(); err != nil {
		t.Fatal(err)
	}

	releaseDatabaseLock := func() {}
	lockDone := make(chan error, 1)
	if holdHandler {
		releaseDatabaseLock = lockWorkerCommand(t, fixture, input.RunID, command.CommandID, lockDone)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	update, err := harness.client.UpdateWorkflow(ctx, sdkclient.UpdateWorkflowOptions{
		UpdateID:            "rollover-update-" + testIdentity(t),
		WorkflowID:          input.WorkflowID,
		RunID:               firstRunID,
		UpdateName:          contract.ControlUpdateName,
		Args:                []any{command},
		WaitForStage:        sdkclient.WorkflowUpdateStageAccepted,
		FirstExecutionRunID: firstRunID,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 60 {
		if err := harness.client.SignalWorkflow(ctx, input.WorkflowID, firstRunID, contract.ControlSignalName, command); err != nil {
			t.Fatal(err)
		}
	}
	waitForHistoryLength(t, ctx, harness.client, input.WorkflowID, firstRunID, 64)
	close(controller.release)

	if holdHandler {
		waitForStartedActivity(t, ctx, harness.client, input.WorkflowID, firstRunID, contract.CommandActivityName)
		description, err := harness.client.DescribeWorkflowExecution(ctx, input.WorkflowID, "")
		if err != nil {
			t.Fatal(err)
		}
		if current := description.GetWorkflowExecutionInfo().GetExecution().GetRunId(); current != firstRunID {
			t.Fatalf("workflow continued to %q before its accepted update handler drained", current)
		}
		releaseDatabaseLock()
		if err := <-lockDone; err != nil {
			t.Fatal(err)
		}
	}

	var updated contract.WorkflowCommandResult
	if err := update.Get(ctx, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.CommandID != command.CommandID || !updated.Applied || !updated.CancelRequested {
		t.Fatalf("update command result = %#v", updated)
	}
	currentRunID, firstDescription := waitForWorkflowSuccessor(t, ctx, harness.client, input.WorkflowID, firstRunID)
	var result contract.WorkflowProjectionResult
	if err := run.Get(ctx, &result); err != nil {
		t.Fatal(err)
	}
	info := firstDescription.GetWorkflowExecutionInfo()
	if info.GetHistoryLength() >= 2000 || info.GetHistorySizeBytes() >= 2<<20 {
		t.Fatalf(
			"server rollover reached local limits: events=%d bytes=%d",
			info.GetHistoryLength(),
			info.GetHistorySizeBytes(),
		)
	}
	assertContinuedAsNewLink(t, harness.client, input.WorkflowID, firstRunID, currentRunID)
	assertContinuationInputsEqual(t, harness.client, input.WorkflowID, firstRunID, currentRunID)
	assertWorkflowBindingMembers(t, fixture, input.RunID, firstRunID, currentRunID)
	assertCommandAppliedOnce(t, fixture, input.RunID, command.CommandID)
	if after := durableRunDeadline(t, fixture, input.RunID); !after.Equal(originalDeadline) {
		t.Fatalf("durable run deadline changed across continuation: before=%s after=%s", originalDeadline, after)
	}
	if calls := controller.executeCalls.Load(); calls != 1 {
		t.Fatalf("provider execute calls = %d; want one", calls)
	}
	if holdHandler {
		assertObserverRefusesFreshRootsAndWorkflowIDReuse(t, harness, input)
	}
}

func lockWorkerCommand(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID, commandID string,
	done chan error,
) func() {
	t.Helper()
	locked := make(chan struct{})
	release := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	go func() {
		defer cancel()
		done <- fixture.WithinSetupSQL(ctx, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
			var marker int
			err := tx.QueryRow(ctx, `
				SELECT 1
				FROM pm.outbox_commands AS command
				WHERE command.org_id = $1 AND command.workspace_id = $2
				  AND command.run_id = $3 AND command.command_id = $4
				FOR UPDATE OF command
			`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID, commandID).Scan(&marker)
			if err != nil {
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
		t.Fatalf("lock durable worker command: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("durable worker command lock was not acquired")
	}
	var once sync.Once
	return func() { once.Do(func() { close(release) }) }
}

func waitForHistoryLength(
	t *testing.T,
	ctx context.Context,
	client sdkclient.Client,
	workflowID, runID string,
	minimum int64,
) {
	t.Helper()
	for {
		description, err := client.DescribeWorkflowExecution(ctx, workflowID, runID)
		if err == nil && description.GetWorkflowExecutionInfo().GetHistoryLength() >= minimum {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("history did not reach %d events", minimum)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func waitForStartedActivity(
	t *testing.T,
	ctx context.Context,
	client sdkclient.Client,
	workflowID, runID, activityName string,
) {
	t.Helper()
	for {
		description, err := client.DescribeWorkflowExecution(ctx, workflowID, runID)
		if err == nil {
			for _, pending := range description.GetPendingActivities() {
				if pending.GetActivityType().GetName() == activityName &&
					pending.GetState() == enumspb.PENDING_ACTIVITY_STATE_STARTED {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("activity %q was not started", activityName)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func waitForWorkflowSuccessor(
	t *testing.T,
	ctx context.Context,
	client sdkclient.Client,
	workflowID, firstRunID string,
) (string, *workflowservice.DescribeWorkflowExecutionResponse) {
	t.Helper()
	for {
		current, err := client.DescribeWorkflowExecution(ctx, workflowID, "")
		if err == nil {
			currentRunID := current.GetWorkflowExecutionInfo().GetExecution().GetRunId()
			if currentRunID != "" && currentRunID != firstRunID {
				first, err := client.DescribeWorkflowExecution(ctx, workflowID, firstRunID)
				if err != nil {
					t.Fatal(err)
				}
				return currentRunID, first
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("workflow did not produce a continuation successor")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func durableRunDeadline(t *testing.T, fixture *testfixture.Fixture, runID string) time.Time {
	t.Helper()
	var deadline time.Time
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		acceptedAt, err := store.LoadWorkerAcceptedAt(ctx, tx, runID)
		if err != nil {
			return err
		}
		budget, err := store.LoadRunBudgetSnapshot(ctx, tx, runID)
		if err != nil {
			return err
		}
		deadline = acceptedAt.Add(budget.Policy.RunLifetime).UTC()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return deadline
}

func assertWorkflowBindingMembers(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID, firstRunID, currentRunID string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		run, err := store.LoadRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		observations := run.WorkflowBinding.Observations
		if len(observations) < 2 ||
			observations[0].TemporalRunID != firstRunID ||
			observations[len(observations)-1].TemporalRunID != currentRunID {
			return fmt.Errorf("workflow binding observations = %#v", observations)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func readCanonicalStartInput(
	t *testing.T,
	client sdkclient.Client,
	workflowID, runID string,
) contract.StartRunInput {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	history := readWorkflowHistory(t, ctx, client, workflowID, runID)
	if len(history.GetEvents()) == 0 {
		t.Fatal("canonical workflow start history is empty")
	}
	payloads := history.GetEvents()[0].GetWorkflowExecutionStartedEventAttributes().GetInput()
	var input contract.StartRunInput
	if err := converter.GetDefaultDataConverter().FromPayloads(payloads, &input); err != nil {
		t.Fatalf("decode canonical workflow start input: %v", err)
	}
	if err := input.ValidateRelayStart(); err != nil {
		t.Fatalf("validate canonical workflow start input: %v", err)
	}
	return input
}

func assertContinuationInputsEqual(
	t *testing.T,
	client sdkclient.Client,
	workflowID, firstRunID, currentRunID string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first := readWorkflowHistory(t, ctx, client, workflowID, firstRunID)
	current := readWorkflowHistory(t, ctx, client, workflowID, currentRunID)
	firstInput := first.GetEvents()[0].GetWorkflowExecutionStartedEventAttributes().GetInput()
	currentInput := current.GetEvents()[0].GetWorkflowExecutionStartedEventAttributes().GetInput()
	if firstInput == nil || currentInput == nil || !proto.Equal(firstInput, currentInput) {
		t.Fatal("Continue-As-New rewrote the opaque canonical start input")
	}
}

func assertObserverRefusesFreshRootsAndWorkflowIDReuse(
	t *testing.T,
	harness *productionWorkflowHarness,
	input contract.StartRunInput,
) {
	t.Helper()
	harness.worker.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	reused, err := harness.client.ExecuteWorkflow(ctx, sdkclient.StartWorkflowOptions{
		ID:                    input.WorkflowID,
		TaskQueue:             contract.ControlTaskQueue,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}, contract.RunWorkflowType, input)
	if err != nil {
		t.Fatal(err)
	}
	reusedRunID := reused.GetRunID()
	if reusedRunID == "" {
		t.Fatal("reused workflow start returned no exact run identity")
	}
	testfixture.CleanupWorkflowChain(
		t, harness.client, harness.options.Namespace, input.WorkflowID, reusedRunID,
	)
	reusedExecution := contract.WorkflowExecution{
		Namespace: harness.options.Namespace, WorkflowID: input.WorkflowID, RunID: reusedRunID,
	}
	if _, err := harness.service.LoadWorkflow(ctx, reusedExecution, input.RunRef()); err == nil {
		t.Fatal("fresh workflow-ID reuse was accepted as a continuation")
	}

	other := input
	other.RunID = "11111111-1111-4111-8111-111111111111"
	other.WorkflowID = "pm/run/" + other.RunID
	other.CommandID = "unbound-fresh-root"
	if err := other.ValidateRelayStart(); err != nil {
		t.Fatal(err)
	}
	otherRun, err := harness.client.ExecuteWorkflow(ctx, sdkclient.StartWorkflowOptions{
		ID: other.WorkflowID, TaskQueue: contract.ControlTaskQueue,
	}, contract.RunWorkflowType, other)
	if err != nil {
		t.Fatal(err)
	}
	otherRunID := otherRun.GetRunID()
	if otherRunID == "" {
		t.Fatal("unbound workflow start returned no exact run identity")
	}
	testfixture.CleanupWorkflowChain(
		t, harness.client, harness.options.Namespace, other.WorkflowID, otherRunID,
	)
	otherExecution := contract.WorkflowExecution{
		Namespace:  harness.options.Namespace,
		WorkflowID: other.WorkflowID,
		RunID:      otherRunID,
	}
	if _, err := harness.service.LoadWorkflow(ctx, otherExecution, input.RunRef()); err == nil {
		t.Fatal("run from a different root was accepted under the durable workflow identity")
	}
	if _, err := harness.service.LoadWorkflow(ctx, otherExecution, other.RunRef()); err == nil {
		t.Fatal("unbound initial root was auto-adopted as a continuation")
	}
}

type productionWorkflowHarness struct {
	client   sdkclient.Client
	worker   sdkworker.Worker
	options  sdkclient.Options
	timeouts contract.ActivityTimeouts
	fixture  *testfixture.Fixture
	service  *app.WorkerService
	started  bool
}

func newProductionWorkflowHarness(
	t *testing.T,
	fixture *testfixture.Fixture,
	namespace string,
) *productionWorkflowHarness {
	t.Helper()
	artifact := testfixture.NewWorkerConfiguration(t)
	if namespace == "" {
		namespace = artifact.Configuration.Temporal().Namespace
	}
	options := testfixture.TemporalOptions(t, artifact.Configuration, namespace, "production-workflow-test-"+testIdentity(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := pmtemporal.DialWorkerClient(ctx, options, artifact.Configuration.Worker().DialTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	timeouts := artifact.Configuration.Worker().ActivityTimeouts
	observer, err := pmtemporal.NewWorkflowContinuationObserver(client, namespace, timeouts.Transport)
	if err != nil {
		t.Fatal(err)
	}
	service := testfixture.NewWorkerService(
		t,
		fixture,
		fixture.WorkerRegistry,
		fixture.ResolveOperation,
		nil,
		observer,
		timeouts,
	)
	activityService, err := pmactivities.New(service)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := pmworkflows.NewCoordinator(timeouts)
	if err != nil {
		t.Fatal(err)
	}
	worker := sdkworker.New(client, contract.ControlTaskQueue, sdkworker.Options{
		MaxConcurrentActivityExecutionSize:     int(artifact.Configuration.Worker().MaxConcurrentActivities),
		MaxConcurrentWorkflowTaskExecutionSize: int(artifact.Configuration.Worker().MaxConcurrentWorkflowTasks),
		WorkerStopTimeout:                      artifact.Configuration.Worker().DrainTimeout,
	})
	if err := pmworkflows.RegisterWorkflows(worker, coordinator, service.SupportsScheduledAdmission()); err != nil {
		t.Fatal(err)
	}
	if err := pmactivities.RegisterActivities(worker, activityService); err != nil {
		t.Fatal(err)
	}
	harness := &productionWorkflowHarness{
		client: client, worker: worker, options: options, timeouts: timeouts, fixture: fixture, service: service,
	}
	t.Cleanup(func() {
		if harness.started {
			harness.worker.Stop()
		}
	})
	return harness
}

func (harness *productionWorkflowHarness) relayAdmitted(
	t *testing.T,
	admitted admittedWorkflowIdentity,
) (sdkclient.WorkflowRun, contract.StartRunInput) {
	t.Helper()
	relayOptions := harness.options
	relayOptions.Identity += "/relay"
	client, err := pmtemporal.NewClient(pmtemporal.ClientConfig{
		Namespace:            relayOptions.Namespace,
		ControlTaskQueue:     contract.ControlTaskQueue,
		WorkflowType:         contract.RunWorkflowType,
		ProjectionUpdateName: contract.ProjectionUpdateName,
		DialTimeout:          harness.timeouts.Transport,
		RequestTimeout:       harness.timeouts.Transport,
		Dial: func(ctx context.Context) (pmtemporal.WorkflowClient, error) {
			return pmtemporal.DialWorkerClient(ctx, relayOptions, harness.timeouts.Transport)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	relay, err := pmtemporal.NewRelay(pmtemporal.RelayConfig{
		Store: harness.fixture.Pool, Client: client, OwnerID: "workflow-test-relay-" + testIdentity(t),
		ClaimLimit: 1, ClaimTTL: 10 * time.Second, IdleDelay: 10 * time.Millisecond, FailureDelay: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if harness.started {
		t.Fatal("production workflow harness supports exactly one controlled admitted run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := relay.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var runID string
	for runID == "" {
		description, err := harness.client.DescribeWorkflowExecution(ctx, admitted.WorkflowID, "")
		if err == nil {
			runID = description.GetWorkflowExecutionInfo().GetExecution().GetRunId()
			if runID != "" {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("relayed workflow did not become observable")
		case <-time.After(20 * time.Millisecond):
		}
	}
	testfixture.CleanupWorkflowChain(
		t, harness.client, harness.options.Namespace, admitted.WorkflowID, runID,
	)
	canonical := readCanonicalStartInput(t, harness.client, admitted.WorkflowID, runID)
	if canonical.RunID != admitted.RunID ||
		!canonical.Scope.Equal(admitted.Scope) ||
		canonical.WorkflowID != admitted.WorkflowID ||
		canonical.Origin != admitted.Origin ||
		canonical.Generation != admitted.Generation {
		t.Fatalf("relayed canonical input = %#v; want admitted durable identity %#v", canonical, admitted)
	}

	var intent store.WorkflowCommandIntent
	var budget store.RunBudgetSnapshot
	if err := store.WithinScope(
		ctx, harness.fixture.Pool, admitted.Scope, harness.fixture.Actor,
		func(ctx context.Context, tx *store.Transaction) error {
			loaded, err := store.LoadWorkerCommandIntent(ctx, tx, canonical.RunID, canonical.CommandID)
			if err != nil {
				return err
			}
			intent = loaded
			budget, err = store.LoadRunBudgetSnapshot(ctx, tx, canonical.RunID)
			return err
		},
	); err != nil {
		t.Fatalf("load exact sealed workflow intent: %v", err)
	}
	sealed := contract.StartRunInput{
		Schema:       contract.StartRunInputSchema,
		Scope:        intent.Scope,
		RunID:        intent.RunID,
		CommandID:    intent.CommandID,
		WorkflowID:   intent.Plan.WorkflowID,
		Generation:   intent.Plan.Generation,
		Origin:       intent.Plan.Origin,
		IntentDigest: intent.Digest,
	}
	if canonical != sealed {
		t.Fatalf("relayed canonical input = %#v; want exact sealed intent projection %#v", canonical, sealed)
	}
	if intent.Kind != store.WorkflowCommandStartRun ||
		intent.Plan.Namespace != harness.options.Namespace ||
		intent.Plan.TaskQueue != contract.ControlTaskQueue ||
		intent.Plan.WorkflowType != contract.RunWorkflowType ||
		intent.Plan.ExecutionTimeout != budget.Policy.RunLifetime ||
		intent.Plan.BudgetDigest != budget.Digest {
		t.Fatalf("sealed start workflow intent plan = %#v; budget = %#v", intent, budget)
	}
	testfixture.SealWorkerPlan(t, harness.fixture, harness.service, contract.WorkflowExecution{
		Namespace: harness.options.Namespace, WorkflowID: canonical.WorkflowID, RunID: runID,
	}, canonical.RunRef())
	if err := harness.worker.Start(); err != nil {
		t.Fatal(err)
	}
	harness.started = true
	return harness.client.GetWorkflow(ctx, canonical.WorkflowID, runID), canonical
}

type blockingExecutionController struct {
	entered      chan struct{}
	release      chan struct{}
	enterOnce    sync.Once
	executeCalls atomic.Uint32
}

func newBlockingExecutionController() *blockingExecutionController {
	return &blockingExecutionController{entered: make(chan struct{}), release: make(chan struct{})}
}

func (controller *blockingExecutionController) Execute(
	ctx context.Context,
	request app.ExecutionRequest,
) (app.ExecutionObservation, error) {
	controller.executeCalls.Add(1)
	controller.enterOnce.Do(func() { close(controller.entered) })
	select {
	case <-ctx.Done():
		return app.ExecutionObservation{}, ctx.Err()
	case <-controller.release:
		return successfulExecutionObservation(request), nil
	}
}

func (controller *blockingExecutionController) Reconcile(
	_ context.Context,
	request app.ExecutionRequest,
) (app.ExecutionObservation, error) {
	return successfulExecutionObservation(request), nil
}

type callbackTiming struct {
	remaining time.Duration
	elapsed   time.Duration
}

type timeoutExecutionController struct {
	finished chan callbackTiming
}

func newTimeoutExecutionController() *timeoutExecutionController {
	return &timeoutExecutionController{finished: make(chan callbackTiming, 1)}
}

func (controller *timeoutExecutionController) Execute(
	ctx context.Context,
	_ app.ExecutionRequest,
) (app.ExecutionObservation, error) {
	started := time.Now()
	deadline, ok := ctx.Deadline()
	if !ok {
		return app.ExecutionObservation{}, errors.New("provider transport context has no deadline")
	}
	remaining := time.Until(deadline)
	<-ctx.Done()
	controller.finished <- callbackTiming{remaining: remaining, elapsed: time.Since(started)}
	return app.ExecutionObservation{}, ctx.Err()
}

func (controller *timeoutExecutionController) Reconcile(
	ctx context.Context,
	request app.ExecutionRequest,
) (app.ExecutionObservation, error) {
	return controller.Execute(ctx, request)
}

func successfulExecutionObservation(request app.ExecutionRequest) app.ExecutionObservation {
	observedAt := time.Now().UTC()
	outcomes := make([]contract.MemberOutcome, 0, len(request.Dispatch.OrderedMemberIDs))
	for _, memberID := range request.Dispatch.OrderedMemberIDs {
		outcomes = append(outcomes, contract.MemberOutcome{
			MemberID: memberID, EffectKey: request.Dispatch.EffectKey, DispatchID: request.Dispatch.DispatchID,
			State: contract.OutcomeSucceeded, ObservedAt: observedAt, AssertionRefs: []string{},
		})
	}
	return app.ExecutionObservation{Outcomes: outcomes}
}

func getRunView(t *testing.T, fixture *testfixture.Fixture, runID string) api.RunView {
	t.Helper()
	path := fixture.BaseURL + "/v1/workspaces/" + url.PathEscape(fixture.Scope.WorkspaceID) +
		"/runs/" + url.PathEscape(runID)
	var view api.RunView
	sendWorkflowJSON(t, fixture.Client, http.MethodGet, path, nil, nil, http.StatusOK, &view)
	return view
}

func cancelRun(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID, requestKey, revision string,
) api.RunCancelResponse {
	t.Helper()
	path := fixture.BaseURL + "/v1/workspaces/" + url.PathEscape(fixture.Scope.WorkspaceID) +
		"/runs/" + url.PathEscape(runID) + "/cancel"
	protocol := api.ProtocolMetadata{
		WireVersion: api.ProtocolMetadataWireVersion(api.WireVersion), RequiredFeatures: []string{},
		RequestKey:       requestKey,
		IntentDigest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		ExpectedRevision: revision,
	}
	var response api.RunCancelResponse
	sendWorkflowJSON(
		t,
		fixture.Client,
		http.MethodPost,
		path,
		api.RunCancelRequest{Protocol: protocol},
		map[string]string{"Idempotency-Key": requestKey, "If-Match": revision},
		http.StatusAccepted,
		&response,
	)
	return response
}

func sendWorkflowJSON(
	t *testing.T,
	client *http.Client,
	method, endpoint string,
	body any,
	headers map[string]string,
	expectedStatus int,
	output any,
) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	document, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(document) > 64*1024 {
		t.Fatal("run control response exceeded its bounded envelope")
	}
	if response.StatusCode != expectedStatus {
		t.Fatalf("run control status = %d; body = %q", response.StatusCode, document)
	}
	if output != nil {
		if err := json.Unmarshal(document, output); err != nil {
			t.Fatal(err)
		}
	}
}

func assertCommandAppliedOnce(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID, commandID string,
) {
	t.Helper()
	var count int
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*)
			FROM pm.workflow_command_receipts
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND command_id = $4
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID, commandID).Scan(&count)
	})
	if count != 1 {
		t.Fatalf("durable command receipts = %d; want exactly one", count)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		commands, err := store.ListWorkerCommands(ctx, tx, runID)
		if err != nil {
			return err
		}
		if len(commands) != 0 {
			return fmt.Errorf("pending worker commands = %d; want zero", len(commands))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func rolloverHarness(
	t *testing.T,
	namespace string,
	activities *rolloverActivities,
) *workflowHarness {
	t.Helper()
	return newWorkflowHarness(t, namespace, func(worker sdkworker.Worker) {
		coordinator, err := pmworkflows.NewCoordinator(testActivityTimeouts())
		if err != nil {
			t.Fatal(err)
		}
		if err := pmworkflows.RegisterWorkflows(worker, coordinator, false); err != nil {
			t.Fatal(err)
		}
		worker.RegisterActivityWithOptions(activities.Load, activity.RegisterOptions{Name: contract.LoadActivityName})
		worker.RegisterActivityWithOptions(activities.Execute, activity.RegisterOptions{Name: contract.ExecuteActivityName})
		worker.RegisterActivityWithOptions(activities.Command, activity.RegisterOptions{Name: contract.CommandActivityName})
		worker.RegisterActivityWithOptions(activities.Project, activity.RegisterOptions{Name: contract.ProjectActivityName})
	})
}

func executeUntilContinuation(
	t *testing.T,
	client sdkclient.Client,
	namespace string,
	input contract.StartRunInput,
	activities *rolloverActivities,
) (string, string, *workflowservice.DescribeWorkflowExecutionResponse) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	run, err := client.ExecuteWorkflow(ctx, sdkclient.StartWorkflowOptions{
		ID: input.WorkflowID, TaskQueue: contract.ControlTaskQueue,
	}, contract.RunWorkflowType, input)
	if err != nil {
		t.Fatal(err)
	}
	firstRunID := run.GetRunID()
	if firstRunID == "" {
		t.Fatal("workflow start returned no exact run identity")
	}
	testfixture.CleanupWorkflowChain(t, client, namespace, input.WorkflowID, firstRunID)
	var currentRunID string
	for currentRunID == "" {
		description, describeErr := client.DescribeWorkflowExecution(ctx, input.WorkflowID, "")
		if describeErr == nil && description.GetWorkflowExecutionInfo().GetExecution().GetRunId() != firstRunID {
			currentRunID = description.GetWorkflowExecutionInfo().GetExecution().GetRunId()
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("workflow did not Continue-As-New within the bounded fixture interval")
		case <-time.After(20 * time.Millisecond):
		}
	}
	activities.stop.Store(true)
	var result contract.WorkflowProjectionResult
	if err := run.Get(ctx, &result); err != nil {
		t.Fatal(err)
	}
	firstDescription, err := client.DescribeWorkflowExecution(ctx, input.WorkflowID, firstRunID)
	if err != nil {
		t.Fatal(err)
	}
	return firstRunID, currentRunID, firstDescription
}

func assertContinuedAsNewLink(
	t *testing.T,
	client sdkclient.Client,
	workflowID, firstRunID, currentRunID string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	history := readWorkflowHistory(t, ctx, client, workflowID, firstRunID)
	if len(history.GetEvents()) == 0 {
		t.Fatal("continued predecessor history is empty")
	}
	last := history.GetEvents()[len(history.GetEvents())-1]
	attributes := last.GetWorkflowExecutionContinuedAsNewEventAttributes()
	if last.GetEventType() != enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_CONTINUED_AS_NEW || attributes == nil {
		t.Fatalf("predecessor closed with %v; want Continue-As-New", last.GetEventType())
	}
	if got := attributes.GetNewExecutionRunId(); got != currentRunID {
		t.Fatalf("continued successor = %q; want %q", got, currentRunID)
	}
}

func readWorkflowHistory(
	t *testing.T,
	ctx context.Context,
	client sdkclient.Client,
	workflowID, runID string,
) *historypb.History {
	t.Helper()
	iterator := client.GetWorkflowHistory(ctx, workflowID, runID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	history := &historypb.History{}
	for iterator.HasNext() {
		event, err := iterator.Next()
		if err != nil {
			t.Fatal(err)
		}
		history.Events = append(history.Events, event)
	}
	if proto.Size(history) == 0 {
		t.Fatal("workflow history was empty")
	}
	return history
}

type rolloverActivityMode uint8

const (
	rolloverActivityModeNormal rolloverActivityMode = iota
	rolloverActivityModeLargeSnapshot
)

type rolloverActivities struct {
	reference contract.WorkflowRunRef
	deadline  time.Time
	timeouts  contract.ActivityTimeouts
	mode      rolloverActivityMode
	stop      atomic.Bool

	mu         sync.Mutex
	loads      int
	references []contract.WorkflowRunRef
}

func newRolloverActivities(reference contract.WorkflowRunRef, mode rolloverActivityMode) *rolloverActivities {
	timeouts := testActivityTimeouts()
	if mode == rolloverActivityModeLargeSnapshot {
		timeouts.Heartbeat = 10 * time.Millisecond
	}
	return &rolloverActivities{
		reference: reference,
		deadline:  time.Now().UTC().Add(5 * time.Minute).Truncate(time.Microsecond),
		timeouts:  timeouts,
		mode:      mode,
	}
}

func (activities *rolloverActivities) Load(
	_ context.Context,
	reference contract.WorkflowRunRef,
) (contract.WorkflowRunSnapshot, error) {
	activities.mu.Lock()
	activities.loads++
	activities.references = append(activities.references, reference)
	activities.mu.Unlock()
	if reference != activities.reference {
		return contract.WorkflowRunSnapshot{}, errors.New("workflow reference changed across rollover")
	}
	if activities.stop.Load() {
		return contract.WorkflowRunSnapshot{
			Run: reference, Deadline: activities.deadline, Timeouts: activities.timeouts,
			Units: []contract.WorkflowUnitRef{}, Commands: []contract.WorkflowCommandRef{},
			Done: true, Fanout: 1,
		}, nil
	}
	units := []contract.WorkflowUnitRef{{TaskID: "rollover-task"}}
	if activities.mode == rolloverActivityModeLargeSnapshot {
		units = make([]contract.WorkflowUnitRef, 20)
		for index := range units {
			prefix := fmt.Sprintf("large-history-unit-%03d-", index)
			units[index] = contract.WorkflowUnitRef{
				TaskID: prefix + strings.Repeat("<", 512-len(prefix)),
			}
		}
	}
	snapshot := contract.WorkflowRunSnapshot{
		Run: reference, Deadline: activities.deadline, Timeouts: activities.timeouts,
		Units: units, Commands: []contract.WorkflowCommandRef{}, Fanout: 1,
	}
	if encoded, err := json.Marshal(snapshot); err != nil {
		return contract.WorkflowRunSnapshot{}, err
	} else if len(encoded) > 65536 {
		return contract.WorkflowRunSnapshot{}, errors.New("synthetic rollover snapshot exceeds the safe payload bound")
	}
	return snapshot, nil
}

func (activities *rolloverActivities) Execute(
	_ context.Context,
	request contract.WorkflowUnitRequest,
) (contract.WorkflowUnitResult, error) {
	resultReference := "rollover-result"
	if activities.mode == rolloverActivityModeLargeSnapshot {
		resultReference = strings.Repeat("<", 512)
	}
	return contract.WorkflowUnitResult{
		TaskID: request.Unit.TaskID, State: contract.RunStateSucceeded, ResultRef: resultReference,
	}, nil
}

func (*rolloverActivities) Command(
	context.Context,
	contract.WorkflowCommandRequest,
) (contract.WorkflowCommandResult, error) {
	return contract.WorkflowCommandResult{}, errors.New("unexpected rollover command")
}

func (activities *rolloverActivities) Project(
	context.Context,
	contract.WorkflowProjectionRequest,
) (contract.WorkflowProjectionResult, error) {
	return contract.WorkflowProjectionResult{
		RunID: activities.reference.RunID, Revision: 1, State: contract.RunStateSucceeded,
	}, nil
}

func (activities *rolloverActivities) assertImmutableReference(t *testing.T, expected contract.WorkflowRunRef) {
	t.Helper()
	activities.mu.Lock()
	defer activities.mu.Unlock()
	if activities.loads < 2 {
		t.Fatalf("load count = %d; continuation did not reload durable state", activities.loads)
	}
	for index, reference := range activities.references {
		if reference != expected {
			t.Fatalf("load %d reference changed: %#v", index, reference)
		}
	}
}
