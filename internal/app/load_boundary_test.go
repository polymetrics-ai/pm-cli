package app_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	sdklog "go.temporal.io/sdk/log"
	sdkworker "go.temporal.io/sdk/worker"

	"github.com/polymetrics-ai/pm-cli/internal/activities"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	pmtemporal "github.com/polymetrics-ai/pm-cli/internal/temporal"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
	"github.com/polymetrics-ai/pm-cli/internal/workflows"
)

const (
	loadBoundaryWait         = 30 * time.Second
	loadBoundaryIdentityText = "invalid identity at scope.org_id"
)

// errLoadBoundaryOperationUnavailable is the resolver failure a worker observes
// when the operation catalog no longer admits the operation its run was
// admitted against: a real, non-retryable capability refusal of the same kind a
// configuration or catalog change produces.
var errLoadBoundaryOperationUnavailable = errors.New("operation catalog no longer admits the admitted operation")

// capturingTemporalLogger records the worker's own log lines. The failure
// converter deliberately replaces failure messages before they reach Temporal
// history, so the worker log is the one place the raw error a payload boundary
// reports can be observed.
type capturingTemporalLogger struct {
	mu    sync.Mutex
	lines []string
}

var _ sdklog.Logger = (*capturingTemporalLogger)(nil)

func (logger *capturingTemporalLogger) Debug(message string, keyValues ...interface{}) {
	logger.record(message, keyValues)
}

func (logger *capturingTemporalLogger) Info(message string, keyValues ...interface{}) {
	logger.record(message, keyValues)
}

func (logger *capturingTemporalLogger) Warn(message string, keyValues ...interface{}) {
	logger.record(message, keyValues)
}

func (logger *capturingTemporalLogger) Error(message string, keyValues ...interface{}) {
	logger.record(message, keyValues)
}

func (logger *capturingTemporalLogger) record(message string, keyValues []interface{}) {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	logger.lines = append(logger.lines, fmt.Sprintln(append([]interface{}{message}, keyValues...)...))
}

func (logger *capturingTemporalLogger) first(needle string) string {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	for _, line := range logger.lines {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}

func (logger *capturingTemporalLogger) joined() string {
	logger.mu.Lock()
	defer logger.mu.Unlock()
	return strings.Join(logger.lines, "")
}

func TestLoadRunBoundaryCarriesTheAdmittedScopeAndTheRealFailure(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	artifact := testfixture.NewWorkerConfiguration(t)
	origin, tlsConfig, dialer := newHTTPDirectReadTLSPeer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		t.Error("the refused run reached the provider origin")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(durableHTTPRetryResult))
	}))
	selection := httpDirectReadSelection("fixture.load.boundary", origin, http.StatusOK, false)
	policy := httpDirectReadPolicy(origin)
	fixture := newHTTPDirectReadFixture(
		t,
		selection,
		policy,
		app.HTTPDirectReadDependencies{ClientIdentity: testClientIdentity,
			Resolver: httpDirectReadResolver{}, Dialer: dialer, TLSConfig: tlsConfig,
		},
		nil,
	)

	// The worker's own resolver stops admitting the operation after the run was
	// admitted against it, so the run's first coordination load observes a real
	// capability refusal at the boundary.
	var refuse atomic.Bool
	admitting := fixture.ResolveOperation
	fixture.ResolveOperation = func(
		ctx context.Context,
		scope contract.Scope,
		connectionID, operationID string,
	) (app.OperationSelection, error) {
		if refuse.Load() {
			return app.OperationSelection{}, errLoadBoundaryOperationUnavailable
		}
		return admitting(ctx, scope, connectionID, operationID)
	}
	service := newPermitWorkerService(t, fixture, temporal)
	admitted := admitHTTPDirectReadRun(
		t, fixture, selection, fixture.Client, fixture.Scope,
		fixture.Connection.ConnectionID, "load-boundary-refusal",
	)
	// The production relay starts the run's real workflow, which immediately
	// schedules the accepted run's first load activity. No worker polls yet, so
	// nothing observes the refusal before the run is bound.
	bound := bindPermitWorkerRun(t, fixture, temporal, fixture.Scope, fixture.Actor, admitted.RunRef)
	refuse.Store(true)

	logger := &capturingTemporalLogger{}
	client := dialLoadBoundaryClient(t, artifact.Configuration, temporal.namespace, logger)
	testfixture.CleanupWorkflowChain(
		t, client, temporal.namespace, bound.execution.WorkflowID, bound.execution.RunID,
	)
	worker := startLoadBoundaryWorker(t, client, temporal.timeouts, service)
	defer worker.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), loadBoundaryWait)
	defer cancel()

	// Hop 2: the admitted run's workflow start input, as Temporal recorded it.
	if start := loadBoundaryStartInput(t, ctx, client, bound.execution); start.RunRef() != bound.reference {
		t.Fatalf("workflow start input identity = %#v; want the admitted reference %#v",
			start.RunRef(), bound.reference)
	}
	// Hop 4: the exact payload the pm.load-run.v1 activity receives.
	scheduledEventID, received := waitLoadBoundaryScheduled(t, ctx, client, bound.execution)
	if received != bound.reference {
		t.Fatalf("pm.load-run.v1 input identity = %#v; want the admitted reference %#v",
			received, bound.reference)
	}
	if !received.Scope.Equal(fixture.Scope) || received.Scope.OrgID == "" ||
		received.Scope.WorkspaceID == "" {
		t.Fatalf("pm.load-run.v1 received scope = %#v; want the admitted scope %#v",
			received.Scope, fixture.Scope)
	}
	t.Logf("admitted scope carried to the activity: %#v", received)

	// The refusal the boundary reports for the first attempt, the item's failure
	// class as Temporal recorded it, and whether the run's coordination ever
	// observed a terminal outcome: a payload the admitted identity never
	// produced must not stand in for the refusal the run actually hit.
	reported := waitLoadBoundaryReportedError(t, logger)
	failed := waitLoadBoundaryFailure(t, ctx, client, bound.execution, scheduledEventID)
	status := waitLoadBoundaryTerminal(t, ctx, client, bound.execution)
	application := failed.GetFailure().GetApplicationFailureInfo()
	starts, failures, timeouts := loadBoundaryAttempts(t, ctx, client, bound.execution, scheduledEventID)
	observed := fmt.Sprintf(
		"reported=%s failure=%#v retryable=%v retry_state=%s starts=%d failures=%d timeouts=%d status=%s",
		strings.TrimSpace(reported), application, !application.GetNonRetryable(),
		failed.GetRetryState(), starts, failures, timeouts, status,
	)
	t.Logf("pm.load-run.v1 boundary observation: %s", observed)
	if strings.Contains(reported, loadBoundaryIdentityText) {
		t.Fatalf("pm.load-run.v1 reported a fabricated identity conversion instead of its real "+
			"refusal: %s", observed)
	}
	if !strings.Contains(reported, "pm.worker.capability_unavailable") {
		t.Fatalf("pm.load-run.v1 reported failure is not the real capability refusal: %s", observed)
	}
	if application == nil || !application.GetNonRetryable() {
		t.Fatalf("pm.load-run.v1 lost the non-retryable class of the real refusal: %s", observed)
	}
	if status != enumspb.WORKFLOW_EXECUTION_STATUS_FAILED {
		t.Fatalf("the refused run did not fail with the delivered refusal: %s", observed)
	}
	handle := client.GetWorkflow(ctx, bound.execution.WorkflowID, bound.execution.RunID)
	var projection contract.WorkflowProjectionResult
	if err := handle.Get(ctx, &projection); err == nil {
		t.Fatalf("workflow result = %#v; want the refused run to fail", projection)
	}
}

// loadBoundaryAttempts counts the activity task events Temporal recorded for one
// scheduled activity, so a lost failure class shows up as a boundary outcome the
// run had to keep retrying or time out on.
func loadBoundaryAttempts(
	t *testing.T,
	ctx context.Context,
	client sdkclient.Client,
	execution contract.WorkflowExecution,
	scheduledEventID int64,
) (starts, failures, timeouts int) {
	t.Helper()
	for _, event := range loadBoundaryHistory(t, ctx, client, execution) {
		if started := event.GetActivityTaskStartedEventAttributes(); started != nil &&
			started.GetScheduledEventId() == scheduledEventID {
			starts++
		}
		if failed := event.GetActivityTaskFailedEventAttributes(); failed != nil &&
			failed.GetScheduledEventId() == scheduledEventID {
			failures++
		}
		if timedOut := event.GetActivityTaskTimedOutEventAttributes(); timedOut != nil &&
			timedOut.GetScheduledEventId() == scheduledEventID {
			timeouts++
		}
	}
	return starts, failures, timeouts
}

// dialLoadBoundaryClient opens the real worker client whose collected log lines
// carry the raw errors the payload boundary reports.
func dialLoadBoundaryClient(
	t *testing.T,
	configuration config.Configuration,
	namespace string,
	logger sdklog.Logger,
) sdkclient.Client {
	t.Helper()
	options := testfixture.TemporalOptions(
		t, configuration, namespace, nextPermitTemporalIdentity("load-boundary"),
	)
	options.Logger = logger
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := pmtemporal.DialWorkerClient(ctx, options, configuration.Worker().DialTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func loadBoundaryStartInput(
	t *testing.T,
	ctx context.Context,
	client sdkclient.Client,
	execution contract.WorkflowExecution,
) contract.StartRunInput {
	t.Helper()
	for _, event := range loadBoundaryHistory(t, ctx, client, execution) {
		started := event.GetWorkflowExecutionStartedEventAttributes()
		if started == nil {
			continue
		}
		var input contract.StartRunInput
		if err := converter.GetDefaultDataConverter().FromPayloads(started.GetInput(), &input); err != nil {
			t.Fatalf("decode admitted workflow start input: %v", err)
		}
		if err := input.ValidateRelayStart(); err != nil {
			t.Fatalf("validate admitted workflow start input: %v", err)
		}
		return input
	}
	t.Fatal("workflow history does not begin with its start event")
	return contract.StartRunInput{}
}

func waitLoadBoundaryScheduled(
	t *testing.T,
	ctx context.Context,
	client sdkclient.Client,
	execution contract.WorkflowExecution,
) (int64, contract.WorkflowRunRef) {
	t.Helper()
	for {
		for _, event := range loadBoundaryHistory(t, ctx, client, execution) {
			scheduled := event.GetActivityTaskScheduledEventAttributes()
			if scheduled == nil || scheduled.GetActivityType().GetName() != contract.LoadActivityName {
				continue
			}
			var received contract.WorkflowRunRef
			if err := converter.GetDefaultDataConverter().FromPayloads(scheduled.GetInput(), &received); err != nil {
				t.Fatalf("decode pm.load-run.v1 input payload: %v", err)
			}
			if err := received.Validate(); err != nil {
				t.Fatalf("validate pm.load-run.v1 input payload: %v", err)
			}
			return event.GetEventId(), received
		}
		if !loadBoundaryPoll(ctx) {
			t.Fatal("the accepted run's first pm.load-run.v1 activity was not scheduled")
		}
	}
}

func waitLoadBoundaryReportedError(t *testing.T, logger *capturingTemporalLogger) string {
	t.Helper()
	deadline := time.Now().Add(loadBoundaryWait)
	for {
		if line := logger.first("Activity error."); line != "" {
			return line
		}
		if time.Now().After(deadline) {
			t.Fatalf("the worker never reported the pm.load-run.v1 activity error; log: %s",
				logger.joined())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitLoadBoundaryFailure(
	t *testing.T,
	ctx context.Context,
	client sdkclient.Client,
	execution contract.WorkflowExecution,
	scheduledEventID int64,
) *historypb.ActivityTaskFailedEventAttributes {
	t.Helper()
	for {
		for _, event := range loadBoundaryHistory(t, ctx, client, execution) {
			failed := event.GetActivityTaskFailedEventAttributes()
			if failed == nil || failed.GetScheduledEventId() != scheduledEventID {
				continue
			}
			return failed
		}
		if !loadBoundaryPoll(ctx) {
			t.Fatal("the refused pm.load-run.v1 attempt never reported a failure")
		}
	}
}

// waitLoadBoundaryTerminal reports the first non-running status of the run's
// workflow, or the last observed status when the bound expires. The caller
// decides what the observed status means.
func waitLoadBoundaryTerminal(
	t *testing.T,
	ctx context.Context,
	client sdkclient.Client,
	execution contract.WorkflowExecution,
) enumspb.WorkflowExecutionStatus {
	t.Helper()
	status := enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING
	for {
		description, err := client.DescribeWorkflowExecution(ctx, execution.WorkflowID, execution.RunID)
		if err != nil {
			t.Fatalf("describe the accepted run's workflow: %v", err)
		}
		status = description.GetWorkflowExecutionInfo().GetStatus()
		if status != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
			return status
		}
		if !loadBoundaryPoll(ctx) {
			return status
		}
	}
}

func loadBoundaryHistory(
	t *testing.T,
	ctx context.Context,
	client sdkclient.Client,
	execution contract.WorkflowExecution,
) []*historypb.HistoryEvent {
	t.Helper()
	history := client.GetWorkflowHistory(
		ctx,
		execution.WorkflowID,
		execution.RunID,
		false,
		enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT,
	)
	var events []*historypb.HistoryEvent
	for history.HasNext() {
		event, err := history.Next()
		if err != nil {
			return events
		}
		events = append(events, event)
	}
	return events
}

func loadBoundaryPoll(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(20 * time.Millisecond):
		return true
	}
}

func startLoadBoundaryWorker(
	t *testing.T,
	client sdkclient.Client,
	timeouts contract.ActivityTimeouts,
	service *app.WorkerService,
) sdkworker.Worker {
	t.Helper()
	activityService, err := activities.New(service)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := workflows.NewCoordinator(timeouts)
	if err != nil {
		t.Fatal(err)
	}
	worker := sdkworker.New(client, contract.ControlTaskQueue, sdkworker.Options{})
	if err := workflows.RegisterWorkflows(worker, coordinator, service.SupportsScheduledAdmission()); err != nil {
		t.Fatal(err)
	}
	if err := activities.RegisterActivities(worker, activityService); err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	return worker
}
