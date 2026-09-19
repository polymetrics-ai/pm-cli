package workflows_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	sdkworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
	pmworkflows "github.com/polymetrics-ai/pm-cli/internal/workflows"
)

const temporalTestWorkflowID = "default-test-workflow-id"

func TestQualifiedRetryScheduling(t *testing.T) {
	t.Run("no activity starts before eligibility and the eligibility timer wakes it", func(t *testing.T) {
		start := time.Date(2026, time.September, 18, 10, 0, 0, 0, time.UTC)
		input := retryWorkflowInput(t, "eligibility-wake")
		activities := newRetryWorkflowActivities(
			input.RunRef(), start.Add(20*time.Minute), start.Add(4*time.Minute),
			retryWorkflowTimeouts(10*time.Minute), 2,
		)
		environment := newRetryWorkflowEnvironment(t, start, activities)

		environment.ExecuteWorkflow(contract.RunWorkflowType, input)
		if err := environment.GetWorkflowError(); err != nil {
			t.Fatalf("qualified retry workflow: %v", err)
		}
		requests := activities.executionRequests()
		if len(requests) != 1 || requests[0].Unit != activities.unit {
			t.Fatalf("qualified execution requests = %#v; want exactly %#v", requests, activities.unit)
		}
		starts := activities.activityStarts(contract.ExecuteActivityName)
		if len(starts) != 1 || !starts[0].Equal(activities.retryNotBefore) {
			t.Fatalf("qualified execution starts = %v; want exactly %s", starts, activities.retryNotBefore)
		}
	})

	t.Run("bounded recovery polling does not bypass later eligibility", func(t *testing.T) {
		start := time.Date(2026, time.September, 18, 11, 0, 0, 0, time.UTC)
		input := retryWorkflowInput(t, "lost-notification-recovery")
		activities := newRetryWorkflowActivities(
			input.RunRef(), start.Add(20*time.Minute), start.Add(4*time.Minute),
			retryWorkflowTimeouts(time.Minute), 5,
		)
		environment := newRetryWorkflowEnvironment(t, start, activities)

		environment.ExecuteWorkflow(contract.RunWorkflowType, input)
		if err := environment.GetWorkflowError(); err != nil {
			t.Fatalf("recovery-polled retry workflow: %v", err)
		}
		loadStarts := activities.activityStarts(contract.LoadActivityName)
		wantPrefix := []time.Time{
			start,
			start.Add(time.Minute),
			start.Add(2 * time.Minute),
			start.Add(3 * time.Minute),
			activities.retryNotBefore,
		}
		if len(loadStarts) < len(wantPrefix) {
			t.Fatalf("load starts = %v; want bounded polling through eligibility", loadStarts)
		}
		for index, want := range wantPrefix {
			if !loadStarts[index].Equal(want) {
				t.Fatalf("load start %d = %s; want %s", index, loadStarts[index], want)
			}
		}
		executeStarts := activities.activityStarts(contract.ExecuteActivityName)
		if len(executeStarts) != 1 || !executeStarts[0].Equal(activities.retryNotBefore) {
			t.Fatalf("execute starts = %v; want one at %s", executeStarts, activities.retryNotBefore)
		}
	})

	t.Run("control cancellation interrupts the retry timer", func(t *testing.T) {
		start := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
		input := retryWorkflowInput(t, "cancel-retry-wait")
		activities := newRetryWorkflowActivities(
			input.RunRef(), start.Add(20*time.Minute), start.Add(5*time.Minute),
			retryWorkflowTimeouts(10*time.Minute), 2,
		)
		activities.allowCancellation = true
		environment := newRetryWorkflowEnvironment(t, start, activities)
		command := contract.WorkflowCommandRef{CommandID: "cancel-before-retry"}
		environment.RegisterDelayedCallback(func() {
			environment.SignalWorkflow(contract.ControlSignalName, command)
		}, 2*time.Minute)

		environment.ExecuteWorkflow(contract.RunWorkflowType, input)
		if err := environment.GetWorkflowError(); err != nil {
			t.Fatalf("cancelled retry workflow: %v", err)
		}
		if requests := activities.executionRequests(); len(requests) != 0 {
			t.Fatalf("retry activity ran after cancellation: %#v", requests)
		}
		commandStarts := activities.activityStarts(contract.CommandActivityName)
		if len(commandStarts) != 1 || !commandStarts[0].Equal(start.Add(2*time.Minute)) {
			t.Fatalf("command starts = %v; want cancellation at %s", commandStarts, start.Add(2*time.Minute))
		}
		if calls := activities.commandCallCount(); calls != 1 {
			t.Fatalf("command activity calls = %d; want one", calls)
		}
	})

	t.Run("original deadline prevents an eligibility-edge dispatch", func(t *testing.T) {
		start := time.Date(2026, time.September, 18, 13, 0, 0, 0, time.UTC)
		input := retryWorkflowInput(t, "original-deadline")
		deadline := start.Add(3 * time.Minute)
		activities := newRetryWorkflowActivities(
			input.RunRef(), deadline, deadline,
			retryWorkflowTimeouts(10*time.Minute), 2,
		)
		environment := newRetryWorkflowEnvironment(t, start, activities)

		environment.ExecuteWorkflow(contract.RunWorkflowType, input)
		if err := environment.GetWorkflowError(); err != nil {
			t.Fatalf("deadline-edge workflow: %v", err)
		}
		var result contract.WorkflowProjectionResult
		if err := environment.GetWorkflowResult(&result); err != nil {
			t.Fatalf("deadline projection result: %v", err)
		}
		if result.State != contract.RunStateFailed {
			t.Fatalf("deadline projection state = %q; want failed without invented completion", result.State)
		}
		if requests := activities.executionRequests(); len(requests) != 0 {
			t.Fatalf("qualified work dispatched at the original deadline: %#v", requests)
		}
		projectionStarts := activities.activityStarts(contract.ProjectActivityName)
		if len(projectionStarts) != 1 || !projectionStarts[0].Equal(deadline) {
			t.Fatalf("projection starts = %v; want deadline closure at %s", projectionStarts, deadline)
		}
	})

	t.Run("continue as new retains absolute eligibility without duplicate work", func(t *testing.T) {
		start := time.Date(2026, time.September, 18, 14, 0, 0, 0, time.UTC)
		input := retryWorkflowInput(t, "retry-rollover")
		deadline := start.Add(20 * time.Minute)
		retryNotBefore := start.Add(5 * time.Minute)
		firstActivities := newRetryWorkflowActivities(
			input.RunRef(), deadline, retryNotBefore, retryWorkflowTimeouts(10*time.Minute), 2,
		)
		first := newRetryWorkflowEnvironment(t, start, firstActivities)
		first.SetContinueAsNewSuggested(true)

		first.ExecuteWorkflow(contract.RunWorkflowType, input)
		var continued *workflow.ContinueAsNewError
		if err := first.GetWorkflowError(); !errors.As(err, &continued) {
			t.Fatalf("rollover error = %v; want ContinueAsNew", err)
		}
		if continued.WorkflowType == nil || continued.WorkflowType.Name != contract.RunWorkflowType {
			t.Fatalf("continued workflow type = %#v; want %q", continued.WorkflowType, contract.RunWorkflowType)
		}
		var carried contract.StartRunInput
		if err := converter.GetDefaultDataConverter().FromPayloads(continued.Input, &carried); err != nil {
			t.Fatalf("decode continued input: %v", err)
		}
		if carried != input {
			t.Fatalf("continued input = %#v; want unchanged %#v", carried, input)
		}
		if requests := firstActivities.executionRequests(); len(requests) != 0 {
			t.Fatalf("predecessor dispatched retry work before rollover: %#v", requests)
		}

		secondActivities := newRetryWorkflowActivities(
			input.RunRef(), deadline, retryNotBefore, retryWorkflowTimeouts(10*time.Minute), 2,
		)
		second := newRetryWorkflowEnvironment(t, start.Add(2*time.Minute), secondActivities)
		second.ExecuteWorkflow(contract.RunWorkflowType, carried)
		if err := second.GetWorkflowError(); err != nil {
			t.Fatalf("continued retry workflow: %v", err)
		}
		executeStarts := secondActivities.activityStarts(contract.ExecuteActivityName)
		if len(executeStarts) != 1 || !executeStarts[0].Equal(retryNotBefore) {
			t.Fatalf("continued execute starts = %v; want one at retained eligibility %s", executeStarts, retryNotBefore)
		}
		if len(firstActivities.executionRequests())+len(secondActivities.executionRequests()) != 1 {
			t.Fatal("qualified retry work was duplicated across rollover")
		}
	})
}

func TestQualifiedRetryTimerHistoryReplays(t *testing.T) {
	input := syntheticWorkflowInput(t, "retry-timer-replay")
	replayTimeouts := retryWorkflowTimeouts(10 * time.Second)
	activities := newRetryWorkflowActivities(
		input.RunRef(), time.Time{}, time.Time{}, replayTimeouts, 2,
	)
	activities.initializeWindowAtFirstLoad(time.Minute, 5*time.Second)
	harness := newWorkflowHarness(t, "", func(worker sdkworker.Worker) {
		coordinator, err := pmworkflows.NewCoordinator(replayTimeouts)
		if err != nil {
			t.Fatal(err)
		}
		if err := pmworkflows.RegisterWorkflows(worker, coordinator, false); err != nil {
			t.Fatal(err)
		}
		registerRetryActivities(worker, activities)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run, err := harness.client.ExecuteWorkflow(ctx, sdkclient.StartWorkflowOptions{
		ID: input.WorkflowID, TaskQueue: contract.ControlTaskQueue,
	}, contract.RunWorkflowType, input)
	if err != nil {
		t.Fatal(err)
	}
	testfixture.CleanupWorkflowChain(
		t, harness.client, harness.namespace, input.WorkflowID, run.GetRunID(),
	)
	var result contract.WorkflowProjectionResult
	if err := run.Get(ctx, &result); err != nil {
		t.Fatal(err)
	}
	if requests := activities.executionRequests(); len(requests) != 1 || requests[0].Unit != activities.unit {
		t.Fatalf("recorded qualified execution requests = %#v; want exactly %#v", requests, activities.unit)
	}
	history := readWorkflowHistory(t, ctx, harness.client, input.WorkflowID, run.GetRunID())
	var timers, executions int
	for _, event := range history.GetEvents() {
		if event.GetTimerStartedEventAttributes() != nil {
			timers++
		}
		if scheduled := event.GetActivityTaskScheduledEventAttributes(); scheduled != nil &&
			scheduled.GetActivityType().GetName() == contract.ExecuteActivityName {
			executions++
		}
	}
	if timers == 0 || executions != 1 {
		t.Fatalf("retry history timers=%d execute schedules=%d; want a durable timer and one qualified execution", timers, executions)
	}

	replayer := sdkworker.NewWorkflowReplayer()
	coordinator, err := pmworkflows.NewCoordinator(replayTimeouts)
	if err != nil {
		t.Fatal(err)
	}
	replayer.RegisterWorkflowWithOptions(
		coordinator.PMRunWorkflow,
		workflow.RegisterOptions{Name: contract.RunWorkflowType},
	)
	if err := replayer.ReplayWorkflowHistoryWithOptions(nil, history, sdkworker.ReplayWorkflowHistoryOptions{
		OriginalExecution: workflow.Execution{ID: input.WorkflowID, RunID: run.GetRunID()},
	}); err != nil {
		t.Fatalf("replay qualified retry timer history: %v", err)
	}
}

func TestQualifiedRetryRetainsInitialDeadlineHistory(t *testing.T) {
	input := syntheticWorkflowInput(t, "retained-deadline-history")
	timeouts := retryWorkflowTimeouts(10 * time.Second)
	deadline := time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)
	var mu sync.Mutex
	loads := 0
	load := func(_ context.Context, reference contract.WorkflowRunRef) (contract.WorkflowRunSnapshot, error) {
		mu.Lock()
		defer mu.Unlock()
		if reference != input.RunRef() {
			return contract.WorkflowRunSnapshot{}, errors.New("retained history changed the admitted run")
		}
		loads++
		snapshot := contract.WorkflowRunSnapshot{
			Run: reference, Deadline: deadline, Timeouts: timeouts,
			Units: []contract.WorkflowUnitRef{}, Commands: []contract.WorkflowCommandRef{},
			Fanout: 1, Done: loads > 1,
		}
		return snapshot, snapshot.Validate()
	}
	project := func(_ context.Context, request contract.WorkflowProjectionRequest) (contract.WorkflowProjectionResult, error) {
		if request.Run != input.RunRef() {
			return contract.WorkflowProjectionResult{}, errors.New("retained history projected a different run")
		}
		return contract.WorkflowProjectionResult{
			RunID: request.Run.RunID, Revision: 1, State: contract.RunStateFailed,
		}, nil
	}
	// The published pre-retry coordinator could reload an expired snapshot before
	// observing Done. Record that retained command trace on actual Temporal; the
	// current coordinator must replay it without replacing the second load.
	retainedWorkflow := func(ctx workflow.Context, admitted contract.StartRunInput) (contract.WorkflowProjectionResult, error) {
		ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			ScheduleToCloseTimeout: timeouts.ScheduleToClose,
			StartToCloseTimeout:    timeouts.StartToClose,
			WaitForCancellation:    true,
		})
		for {
			var snapshot contract.WorkflowRunSnapshot
			if err := workflow.ExecuteActivity(ctx, contract.LoadActivityName, admitted.RunRef()).Get(ctx, &snapshot); err != nil {
				return contract.WorkflowProjectionResult{}, err
			}
			if !snapshot.Done {
				continue
			}
			var result contract.WorkflowProjectionResult
			err := workflow.ExecuteActivity(ctx, contract.ProjectActivityName, contract.WorkflowProjectionRequest{
				Run: admitted.RunRef(),
			}).Get(ctx, &result)
			return result, err
		}
	}
	harness := newWorkflowHarness(t, "", func(worker sdkworker.Worker) {
		worker.RegisterWorkflowWithOptions(retainedWorkflow, workflow.RegisterOptions{Name: contract.RunWorkflowType})
		worker.RegisterActivityWithOptions(load, activity.RegisterOptions{Name: contract.LoadActivityName})
		worker.RegisterActivityWithOptions(project, activity.RegisterOptions{Name: contract.ProjectActivityName})
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
	if err := run.Get(ctx, &result); err != nil {
		t.Fatal(err)
	}
	if result.State != contract.RunStateFailed {
		t.Fatalf("retained deadline result = %q; want failed", result.State)
	}
	history := readWorkflowHistory(t, ctx, harness.client, input.WorkflowID, run.GetRunID())
	coordinator, err := pmworkflows.NewCoordinator(timeouts)
	if err != nil {
		t.Fatal(err)
	}
	replayer := sdkworker.NewWorkflowReplayer()
	replayer.RegisterWorkflowWithOptions(coordinator.PMRunWorkflow, workflow.RegisterOptions{Name: contract.RunWorkflowType})
	if err := replayer.ReplayWorkflowHistoryWithOptions(nil, history, sdkworker.ReplayWorkflowHistoryOptions{
		OriginalExecution: workflow.Execution{ID: input.WorkflowID, RunID: run.GetRunID()},
	}); err != nil {
		t.Fatalf("retained initial deadline history no longer replays: %v", err)
	}
}

type retryActivityStart struct {
	name string
	at   time.Time
}

type retryWorkflowActivities struct {
	mu sync.Mutex

	reference           contract.WorkflowRunRef
	deadline            time.Time
	retryNotBefore      time.Time
	timeouts            contract.ActivityTimeouts
	unit                contract.WorkflowUnitRef
	eligibleLoad        int
	loads               int
	executions          []contract.WorkflowUnitRequest
	starts              []retryActivityStart
	allowCancellation   bool
	cancelled           bool
	commandCalls        int
	projectionCalls     int
	deferredRunLifetime time.Duration
	deferredRetryDelay  time.Duration
}

func newRetryWorkflowActivities(
	reference contract.WorkflowRunRef,
	deadline time.Time,
	retryNotBefore time.Time,
	timeouts contract.ActivityTimeouts,
	eligibleLoad int,
) *retryWorkflowActivities {
	return &retryWorkflowActivities{
		reference:      reference,
		deadline:       deadline,
		retryNotBefore: retryNotBefore,
		timeouts:       timeouts,
		unit: contract.WorkflowUnitRef{
			TaskID:     "retry-task",
			DispatchID: contract.DispatchID("retry-dispatch-2"),
		},
		eligibleLoad: eligibleLoad,
		executions:   []contract.WorkflowUnitRequest{},
		starts:       []retryActivityStart{},
	}
}

func (activities *retryWorkflowActivities) initializeWindowAtFirstLoad(
	runLifetime time.Duration,
	retryDelay time.Duration,
) {
	activities.mu.Lock()
	defer activities.mu.Unlock()
	activities.deferredRunLifetime = runLifetime
	activities.deferredRetryDelay = retryDelay
}

func (activities *retryWorkflowActivities) Load(
	_ context.Context,
	reference contract.WorkflowRunRef,
) (contract.WorkflowRunSnapshot, error) {
	activities.mu.Lock()
	defer activities.mu.Unlock()
	if reference != activities.reference {
		return contract.WorkflowRunSnapshot{}, errors.New("workflow reference changed during retry scheduling")
	}
	if activities.deadline.IsZero() || activities.retryNotBefore.IsZero() {
		if activities.deferredRunLifetime <= 0 || activities.deferredRetryDelay <= 0 ||
			activities.deferredRetryDelay >= activities.deferredRunLifetime {
			return contract.WorkflowRunSnapshot{}, errors.New("deferred retry window is invalid")
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		activities.deadline = now.Add(activities.deferredRunLifetime)
		activities.retryNotBefore = now.Add(activities.deferredRetryDelay)
	}
	activities.loads++
	snapshot := contract.WorkflowRunSnapshot{
		Run: reference, Deadline: activities.deadline, Timeouts: activities.timeouts,
		Units: []contract.WorkflowUnitRef{}, Commands: []contract.WorkflowCommandRef{}, Fanout: 1,
	}
	switch {
	case len(activities.executions) != 0:
		snapshot.Done = true
	case activities.cancelled:
		snapshot.CancelRequested = true
	case activities.loads >= activities.eligibleLoad:
		snapshot.Units = []contract.WorkflowUnitRef{activities.unit}
	default:
		retryNotBefore := activities.retryNotBefore
		snapshot.RetryNotBefore = &retryNotBefore
	}
	return snapshot, snapshot.Validate()
}

func (activities *retryWorkflowActivities) Execute(
	_ context.Context,
	request contract.WorkflowUnitRequest,
) (contract.WorkflowUnitResult, error) {
	activities.mu.Lock()
	defer activities.mu.Unlock()
	if err := request.Validate(); err != nil {
		return contract.WorkflowUnitResult{}, err
	}
	if request.Run != activities.reference || request.Unit != activities.unit {
		return contract.WorkflowUnitResult{}, errors.New("workflow executed a different qualified retry unit")
	}
	activities.executions = append(activities.executions, request)
	return contract.WorkflowUnitResult{
		TaskID: request.Unit.TaskID, State: contract.RunStateSucceeded,
		ResultRef: "result-" + request.Unit.TaskID,
	}, nil
}

func (activities *retryWorkflowActivities) Command(
	_ context.Context,
	request contract.WorkflowCommandRequest,
) (contract.WorkflowCommandResult, error) {
	activities.mu.Lock()
	defer activities.mu.Unlock()
	if !activities.allowCancellation {
		return contract.WorkflowCommandResult{}, errors.New("unexpected retry workflow command")
	}
	if err := request.Validate(); err != nil {
		return contract.WorkflowCommandResult{}, err
	}
	activities.cancelled = true
	activities.commandCalls++
	return contract.WorkflowCommandResult{
		CommandID: request.Command.CommandID, Applied: true, CancelRequested: true,
	}, nil
}

func (activities *retryWorkflowActivities) Project(
	_ context.Context,
	request contract.WorkflowProjectionRequest,
) (contract.WorkflowProjectionResult, error) {
	activities.mu.Lock()
	defer activities.mu.Unlock()
	if err := request.Validate(); err != nil {
		return contract.WorkflowProjectionResult{}, err
	}
	if request.Run != activities.reference {
		return contract.WorkflowProjectionResult{}, errors.New("workflow projected a different run")
	}
	activities.projectionCalls++
	state := contract.RunStateSucceeded
	switch {
	case activities.cancelled:
		state = contract.RunStateCancelled
	case len(activities.executions) == 0:
		state = contract.RunStateFailed
	}
	return contract.WorkflowProjectionResult{
		RunID: activities.reference.RunID, Revision: uint64(activities.projectionCalls), State: state,
	}, nil
}

func (activities *retryWorkflowActivities) recordActivityStart(name string, at time.Time) {
	activities.mu.Lock()
	defer activities.mu.Unlock()
	activities.starts = append(activities.starts, retryActivityStart{name: name, at: at})
}

func (activities *retryWorkflowActivities) activityStarts(name string) []time.Time {
	activities.mu.Lock()
	defer activities.mu.Unlock()
	starts := make([]time.Time, 0, len(activities.starts))
	for _, start := range activities.starts {
		if start.name == name {
			starts = append(starts, start.at)
		}
	}
	return starts
}

func (activities *retryWorkflowActivities) executionRequests() []contract.WorkflowUnitRequest {
	activities.mu.Lock()
	defer activities.mu.Unlock()
	return append([]contract.WorkflowUnitRequest(nil), activities.executions...)
}

func (activities *retryWorkflowActivities) commandCallCount() int {
	activities.mu.Lock()
	defer activities.mu.Unlock()
	return activities.commandCalls
}

func newRetryWorkflowEnvironment(
	t *testing.T,
	start time.Time,
	activities *retryWorkflowActivities,
) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	coordinator, err := pmworkflows.NewCoordinator(activities.timeouts)
	if err != nil {
		t.Fatal(err)
	}
	var suite testsuite.WorkflowTestSuite
	environment := suite.NewTestWorkflowEnvironment()
	environment.SetStartTime(start)
	environment.RegisterWorkflowWithOptions(
		coordinator.PMRunWorkflow,
		workflow.RegisterOptions{Name: contract.RunWorkflowType},
	)
	environment.RegisterActivityWithOptions(
		activities.Load,
		activity.RegisterOptions{Name: contract.LoadActivityName},
	)
	environment.RegisterActivityWithOptions(
		activities.Execute,
		activity.RegisterOptions{Name: contract.ExecuteActivityName},
	)
	environment.RegisterActivityWithOptions(
		activities.Command,
		activity.RegisterOptions{Name: contract.CommandActivityName},
	)
	environment.RegisterActivityWithOptions(
		activities.Project,
		activity.RegisterOptions{Name: contract.ProjectActivityName},
	)
	environment.SetOnActivityStartedListener(func(
		info *activity.Info,
		_ context.Context,
		_ converter.EncodedValues,
	) {
		activities.recordActivityStart(info.ActivityType.Name, environment.Now())
	})
	return environment
}

func registerRetryActivities(worker sdkworker.Worker, activities *retryWorkflowActivities) {
	worker.RegisterActivityWithOptions(activities.Load, activity.RegisterOptions{Name: contract.LoadActivityName})
	worker.RegisterActivityWithOptions(activities.Execute, activity.RegisterOptions{Name: contract.ExecuteActivityName})
	worker.RegisterActivityWithOptions(activities.Command, activity.RegisterOptions{Name: contract.CommandActivityName})
	worker.RegisterActivityWithOptions(activities.Project, activity.RegisterOptions{Name: contract.ProjectActivityName})
}

func retryWorkflowInput(t *testing.T, label string) contract.StartRunInput {
	t.Helper()
	input := syntheticWorkflowInput(t, label)
	input.WorkflowID = temporalTestWorkflowID
	input.Generation = 2
	input.Origin = contract.RunOriginRecovery
	if err := input.ValidateRelayStart(); err != nil {
		t.Fatal(err)
	}
	return input
}

func retryWorkflowTimeouts(poll time.Duration) contract.ActivityTimeouts {
	return contract.ActivityTimeouts{
		ScheduleToClose: 30 * time.Minute,
		StartToClose:    20 * time.Minute,
		Heartbeat:       poll,
		Transport:       30 * time.Second,
	}
}
