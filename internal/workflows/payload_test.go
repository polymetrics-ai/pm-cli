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

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/activity"
	sdkclient "go.temporal.io/sdk/client"
	sdktemporal "go.temporal.io/sdk/temporal"
	sdkworker "go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/proto"

	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	pmtemporal "github.com/polymetrics-ai/pm-cli/internal/temporal"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
	pmworkflows "github.com/polymetrics-ai/pm-cli/internal/workflows"
)

const (
	payloadAcceptanceWorkflowName = "pm.test.payload-acceptance.v1"
	diagnostic1024WorkflowName    = "pm.test.diagnostic-1024.v1"
	diagnostic1025WorkflowName    = "pm.test.diagnostic-1025.v1"
	forbiddenChannelWorkflowName  = "pm.test.forbidden-channels.v1"
	diagnostic1024ActivityName    = "pm.test.diagnostic-activity-1024.v1"
	diagnostic1025ActivityName    = "pm.test.diagnostic-activity-1025.v1"
	forbiddenChannelActivityName  = "pm.test.forbidden-channels-activity.v1"
	secretCanary                  = "PM_SECRET_CANARY_DO_NOT_PERSIST"
	rowCanary                     = "PM_ROW_CANARY_DO_NOT_PERSIST"
	cursorCanary                  = "PM_CURSOR_CANARY_DO_NOT_PERSIST"
	bodyCanary                    = "PM_BODY_CANARY_DO_NOT_PERSIST"
	diagnosticCanary1024          = secretCanary + "|" + rowCanary + "|" + cursorCanary + "|" + bodyCanary + "|1024"
	diagnosticCanary1025          = secretCanary + "|" + rowCanary + "|" + cursorCanary + "|" + bodyCanary + "|1025"
)

var (
	forbiddenHeartbeatMarshalCalls atomic.Int32
	forbiddenResultMarshalCalls    atomic.Int32
	diagnosticCanaries             = []string{secretCanary, rowCanary, cursorCanary, bodyCanary}
)

type workflowHarness struct {
	client    sdkclient.Client
	worker    sdkworker.Worker
	namespace string
}

func newWorkflowHarness(
	t *testing.T,
	namespace string,
	register func(sdkworker.Worker),
) *workflowHarness {
	t.Helper()
	artifact := testfixture.NewWorkerConfiguration(t)
	if namespace == "" {
		namespace = artifact.Configuration.Temporal().Namespace
	}
	options := testfixture.TemporalOptions(t, artifact.Configuration, namespace, "workflow-test-"+testIdentity(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := pmtemporal.DialWorkerClient(ctx, options, artifact.Configuration.Worker().DialTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	worker := sdkworker.New(client, contract.ControlTaskQueue, sdkworker.Options{
		MaxConcurrentActivityExecutionSize:     64,
		MaxConcurrentWorkflowTaskExecutionSize: 8,
		WorkerStopTimeout:                      artifact.Configuration.Worker().DrainTimeout,
	})
	register(worker)
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(worker.Stop)
	return &workflowHarness{client: client, worker: worker, namespace: namespace}
}

func TestHistoryAndPayloadBounds(t *testing.T) {
	t.Run("C01_unknown_and_oversized_body_refused_before_serialization", func(t *testing.T) {
		harness := newWorkflowHarness(t, "", func(worker sdkworker.Worker) {
			worker.RegisterWorkflowWithOptions(payloadAcceptanceWorkflow, workflow.RegisterOptions{Name: payloadAcceptanceWorkflowName})
		})
		var marshalCalls atomic.Int32
		workflowID := "pm/test/rejected/" + testIdentity(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		unexpected, err := harness.client.ExecuteWorkflow(ctx, sdkclient.StartWorkflowOptions{
			ID: workflowID, TaskQueue: contract.ControlTaskQueue,
		}, payloadAcceptanceWorkflowName, &forbiddenWorkflowBody{
			marshalCalls: &marshalCalls,
			secret:       secretCanary,
			row:          rowCanary,
			cursor:       cursorCanary,
			body:         strings.Repeat(bodyCanary, 4096),
		})
		if err == nil {
			testfixture.CleanupWorkflowChain(
				t, harness.client, harness.namespace, workflowID, unexpected.GetRunID(),
			)
			t.Fatal("unsupported raw workflow body was accepted")
		}
		for _, canary := range diagnosticCanaries {
			if strings.Contains(err.Error(), canary) {
				t.Fatalf("client-side refusal leaked %q: %q", canary, err.Error())
			}
		}
		if calls := marshalCalls.Load(); calls != 0 {
			t.Fatalf("unsupported body MarshalJSON calls = %d; want zero", calls)
		}
		_, err = harness.client.DescribeWorkflowExecution(ctx, workflowID, "")
		var notFound *serviceerror.NotFound
		if !errors.As(err, &notFound) {
			t.Fatalf("rejected workflow Describe error = %v; want NotFound", err)
		}
	})

	t.Run("C02_forbidden_channels_are_refused_without_serialization_or_server_leakage", func(t *testing.T) {
		harness := newWorkflowHarness(t, "", registerDiagnosticWorkflows)
		input := syntheticWorkflowInput(t, "payload-diagnostics")
		request := contract.WorkflowUnitRequest{
			Run:  input.RunRef(),
			Unit: contract.WorkflowUnitRef{TaskID: "diagnostic-task"},
		}
		forbiddenHeartbeatMarshalCalls.Store(0)
		forbiddenResultMarshalCalls.Store(0)
		workflowID := "pm/test/forbidden-channels/" + testIdentity(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		run, err := harness.client.ExecuteWorkflow(ctx, sdkclient.StartWorkflowOptions{
			ID: workflowID, TaskQueue: contract.ControlTaskQueue,
		}, forbiddenChannelWorkflowName, request)
		if err != nil {
			t.Fatal(err)
		}
		testfixture.CleanupWorkflowChain(
			t, harness.client, harness.namespace, workflowID, run.GetRunID(),
		)
		if err := run.Get(ctx, nil); err == nil {
			t.Fatal("workflow with forbidden heartbeat/result channels unexpectedly succeeded")
		}
		if calls := forbiddenHeartbeatMarshalCalls.Load(); calls != 0 {
			t.Fatalf("forbidden heartbeat MarshalJSON calls = %d; want zero", calls)
		}
		for _, channel := range []string{"memo", "search"} {
			t.Run(channel+"_refused_before_server_state", func(t *testing.T) {
				var marshalCalls atomic.Int32
				forbidden := &forbiddenWorkflowBody{
					marshalCalls: &marshalCalls,
					secret:       secretCanary,
					row:          rowCanary,
					cursor:       cursorCanary,
					body:         bodyCanary,
				}
				workflowID := "pm/test/forbidden-" + channel + "/" + testIdentity(t)
				options := sdkclient.StartWorkflowOptions{ID: workflowID, TaskQueue: contract.ControlTaskQueue}
				if channel == "memo" {
					options.Memo = map[string]any{"pm_forbidden_canary": forbidden}
				} else {
					options.SearchAttributes = map[string]any{
						"pm_forbidden_canary": strings.Join(diagnosticCanaries, "|"),
					}
				}
				unexpected, err := harness.client.ExecuteWorkflow(
					ctx,
					options,
					diagnostic1024WorkflowName,
					request,
				)
				if err == nil {
					testfixture.CleanupWorkflowChain(
						t, harness.client, harness.namespace, workflowID, unexpected.GetRunID(),
					)
					t.Fatalf("forbidden %s value was accepted", channel)
				}
				if channel == "memo" {
					if calls := marshalCalls.Load(); calls != 0 {
						t.Fatalf("forbidden memo MarshalJSON calls = %d; want zero", calls)
					}
				}
				for _, canary := range diagnosticCanaries {
					if strings.Contains(err.Error(), canary) {
						t.Fatalf("%s refusal leaked %q: %q", channel, canary, err.Error())
					}
				}
				_, err = harness.client.DescribeWorkflowExecution(ctx, workflowID, "")
				var notFound *serviceerror.NotFound
				if !errors.As(err, &notFound) {
					t.Fatalf("forbidden %s workflow Describe error = %v; want NotFound", channel, err)
				}
			})
		}
		if calls := forbiddenResultMarshalCalls.Load(); calls != 0 {
			t.Fatalf("forbidden result MarshalJSON calls = %d; want zero", calls)
		}
		assertTemporalRecordOmits(t, ctx, harness.client, workflowID, run.GetRunID(), diagnosticCanaries...)
	})

	t.Run("C03_exact_safe_reference_payload_limit_is_inclusive", func(t *testing.T) {
		harness := newWorkflowHarness(t, "", func(worker sdkworker.Worker) {
			worker.RegisterWorkflowWithOptions(payloadAcceptanceWorkflow, workflow.RegisterOptions{Name: payloadAcceptanceWorkflowName})
		})
		input := syntheticWorkflowInput(t, "payload-size")
		for _, size := range []int{65535, 65536} {
			snapshot := workflowSnapshotWithJSONSize(t, input.RunRef(), size)
			workflowID := fmt.Sprintf("pm/test/payload/%d/%s", size, testIdentity(t))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			run, err := harness.client.ExecuteWorkflow(ctx, sdkclient.StartWorkflowOptions{
				ID: workflowID, TaskQueue: contract.ControlTaskQueue,
			}, payloadAcceptanceWorkflowName, snapshot)
			if err != nil {
				cancel()
				t.Fatalf("%d-byte safe payload start failed: %v", size, err)
			}
			testfixture.CleanupWorkflowChain(
				t, harness.client, harness.namespace, workflowID, run.GetRunID(),
			)
			var result contract.WorkflowProjectionResult
			err = run.Get(ctx, &result)
			cancel()
			if err != nil {
				t.Fatalf("%d-byte safe payload failed: %v", size, err)
			}
		}
	})

	t.Run("C04_65537_byte_safe_reference_payload_is_refused", func(t *testing.T) {
		harness := newWorkflowHarness(t, "", func(worker sdkworker.Worker) {
			worker.RegisterWorkflowWithOptions(payloadAcceptanceWorkflow, workflow.RegisterOptions{Name: payloadAcceptanceWorkflowName})
		})
		input := syntheticWorkflowInput(t, "payload-size-refused")
		snapshot := workflowSnapshotWithJSONSize(t, input.RunRef(), 65537)
		workflowID := input.WorkflowID
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		unexpected, err := harness.client.ExecuteWorkflow(ctx, sdkclient.StartWorkflowOptions{
			ID: workflowID, TaskQueue: contract.ControlTaskQueue,
		}, payloadAcceptanceWorkflowName, snapshot)
		if err == nil {
			testfixture.CleanupWorkflowChain(
				t, harness.client, harness.namespace, workflowID, unexpected.GetRunID(),
			)
			t.Fatal("65537-byte safe payload was accepted")
		}
		_, err = harness.client.DescribeWorkflowExecution(ctx, workflowID, "")
		var notFound *serviceerror.NotFound
		if !errors.As(err, &notFound) {
			t.Fatalf("oversized workflow Describe error = %v; want NotFound", err)
		}
	})

	t.Run("C05_diagnostic_1024_and_1025_summaries_omit_canaries", func(t *testing.T) {
		harness := newWorkflowHarness(t, "", registerDiagnosticWorkflows)
		input := syntheticWorkflowInput(t, "payload-diagnostics")
		request := contract.WorkflowUnitRequest{
			Run:  input.RunRef(),
			Unit: contract.WorkflowUnitRef{TaskID: "diagnostic-task"},
		}
		for _, testCase := range []struct {
			name         string
			workflowName string
		}{
			{name: "1024", workflowName: diagnostic1024WorkflowName},
			{name: "1025", workflowName: diagnostic1025WorkflowName},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				workflowID := "pm/test/diagnostic/" + testCase.name + "/" + testIdentity(t)
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				run, err := harness.client.ExecuteWorkflow(ctx, sdkclient.StartWorkflowOptions{
					ID: workflowID, TaskQueue: contract.ControlTaskQueue,
				}, testCase.workflowName, request)
				if err != nil {
					t.Fatal(err)
				}
				testfixture.CleanupWorkflowChain(
					t, harness.client, harness.namespace, workflowID, run.GetRunID(),
				)
				err = run.Get(ctx, nil)
				if err == nil {
					t.Fatal("diagnostic workflow unexpectedly succeeded")
				}
				for _, canary := range diagnosticCanaries {
					if strings.Contains(err.Error(), canary) {
						t.Fatalf("returned error leaked %q: %q", canary, err.Error())
					}
				}
				if len(err.Error()) > 1024 {
					t.Fatalf("unbounded returned error: %q", err.Error())
				}
				assertTemporalRecordOmits(t, ctx, harness.client, workflowID, run.GetRunID(), diagnosticCanaries...)
			})
		}
	})

	t.Run("C06_fanout_caps_active_activities_at_32", func(t *testing.T) {
		input := syntheticWorkflowInput(t, "fanout-32")
		activities := newFanoutActivities(input.RunRef())
		harness := newWorkflowHarness(t, "", func(worker sdkworker.Worker) {
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
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
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
		select {
		case <-activities.reached32:
		case <-ctx.Done():
			t.Fatal("32 concurrent activities did not start")
		}
		select {
		case <-activities.started33:
			t.Fatal("33rd activity started before capacity was released")
		case <-time.After(250 * time.Millisecond):
		}
		close(activities.release)
		select {
		case <-activities.started33:
		case <-ctx.Done():
			t.Fatal("33rd activity did not start after capacity was released")
		}
		var result contract.WorkflowProjectionResult
		if err := run.Get(ctx, &result); err != nil {
			t.Fatal(err)
		}
		if maximum := activities.maximumActive(); maximum != contract.MaxWorkflowFanout {
			t.Fatalf("maximum active activities = %d; want %d", maximum, contract.MaxWorkflowFanout)
		}
	})
	t.Run("C07_local_event_threshold_rolls_at_2000_after_subthreshold_history", func(t *testing.T) {
		input := syntheticWorkflowInput(t, "event-threshold-rollover")
		activities := newRolloverActivities(input.RunRef(), rolloverActivityModeNormal)
		harness := rolloverHarness(t, "", activities)
		firstRunID, currentRunID, firstDescription := executeUntilContinuation(
			t, harness.client, harness.namespace, input, activities,
		)
		info := firstDescription.GetWorkflowExecutionInfo()
		if length := info.GetHistoryLength(); length < 2000 {
			t.Fatalf("event-threshold predecessor history length = %d; want at least 2000", length)
		}
		assertContinuedAsNewLink(t, harness.client, input.WorkflowID, firstRunID, currentRunID)
		activities.assertImmutableReference(t, input.RunRef())
	})

	t.Run("C08_local_byte_threshold_rolls_before_event_threshold", func(t *testing.T) {
		input := syntheticWorkflowInput(t, "byte-threshold-rollover")
		activities := newRolloverActivities(input.RunRef(), rolloverActivityModeLargeSnapshot)
		harness := rolloverHarness(t, "", activities)
		firstRunID, currentRunID, firstDescription := executeUntilContinuation(
			t, harness.client, harness.namespace, input, activities,
		)
		info := firstDescription.GetWorkflowExecutionInfo()
		if size := info.GetHistorySizeBytes(); size < 2<<20 {
			t.Fatalf("byte-threshold predecessor history size = %d; want at least 2 MiB", size)
		}
		if length := info.GetHistoryLength(); length >= 2000 {
			t.Fatalf("byte-threshold predecessor history length = %d; event threshold won", length)
		}
		assertContinuedAsNewLink(t, harness.client, input.WorkflowID, firstRunID, currentRunID)
		activities.assertImmutableReference(t, input.RunRef())
	})
	t.Run("R12_C09_activity_and_transport_timeouts_are_actual", func(t *testing.T) {
		controller := newTimeoutExecutionController()
		fixture := testfixture.NewExecuting(t, controller.Execute, controller.Reconcile)
		harness := newProductionWorkflowHarness(t, fixture, "")
		admitted := admitWorkflowInput(t, fixture, "actual-timeout-contract")
		run, input := harness.relayAdmitted(t, admitted)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var result contract.WorkflowProjectionResult
		if err := run.Get(ctx, &result); err != nil {
			t.Fatal(err)
		}
		var timing callbackTiming
		select {
		case timing = <-controller.finished:
		case <-ctx.Done():
			t.Fatal("provider callback did not observe its transport deadline")
		}
		if timing.remaining < 4500*time.Millisecond || timing.remaining > 5*time.Second {
			t.Fatalf("provider callback deadline remaining = %s; want the explicit 5s transport bound", timing.remaining)
		}
		if timing.elapsed < 4500*time.Millisecond || timing.elapsed >= 10*time.Second {
			t.Fatalf("provider callback stopped after %s; want transport stop before 10s attempt expiry", timing.elapsed)
		}
		history := readWorkflowHistory(t, ctx, harness.client, input.WorkflowID, run.GetRunID())
		var executeSchedules int
		for _, event := range history.GetEvents() {
			attributes := event.GetActivityTaskScheduledEventAttributes()
			if attributes == nil || attributes.GetActivityType().GetName() != contract.ExecuteActivityName {
				continue
			}
			executeSchedules++
			if attributes.GetTaskQueue().GetName() != contract.ControlTaskQueue {
				t.Fatalf("ExecuteUnit task queue = %q; want %q", attributes.GetTaskQueue().GetName(), contract.ControlTaskQueue)
			}
			if got := attributes.GetScheduleToCloseTimeout().AsDuration(); got != 20*time.Second {
				t.Fatalf("ExecuteUnit ScheduleToClose = %s; want 20s", got)
			}
			if got := attributes.GetStartToCloseTimeout().AsDuration(); got != 10*time.Second {
				t.Fatalf("ExecuteUnit StartToClose = %s; want 10s", got)
			}
			if attributes.GetHeartbeatTimeout() == nil {
				t.Fatal("ExecuteUnit heartbeat timeout is absent")
			}
			if got := attributes.GetHeartbeatTimeout().AsDuration(); got != 2*time.Second {
				t.Fatalf("ExecuteUnit Heartbeat = %s; want 2s", got)
			}
		}
		if executeSchedules != 1 {
			t.Fatalf("ExecuteUnit scheduled activities = %d; want one", executeSchedules)
		}
	})
}

type forbiddenWorkflowBody struct {
	marshalCalls *atomic.Int32
	secret       string
	row          string
	cursor       string
	body         string
}

func (body *forbiddenWorkflowBody) MarshalJSON() ([]byte, error) {
	if body.marshalCalls != nil {
		body.marshalCalls.Add(1)
	}
	return json.Marshal([]string{body.secret, body.row, body.cursor, body.body})
}

func payloadAcceptanceWorkflow(
	_ workflow.Context,
	snapshot contract.WorkflowRunSnapshot,
) (contract.WorkflowProjectionResult, error) {
	if err := snapshot.Validate(); err != nil {
		return contract.WorkflowProjectionResult{}, err
	}
	return contract.WorkflowProjectionResult{
		RunID: snapshot.Run.RunID, Revision: 1, State: contract.RunStateSucceeded,
	}, nil
}

func registerDiagnosticWorkflows(worker sdkworker.Worker) {
	worker.RegisterWorkflowWithOptions(diagnostic1024Workflow, workflow.RegisterOptions{Name: diagnostic1024WorkflowName})
	worker.RegisterWorkflowWithOptions(diagnostic1025Workflow, workflow.RegisterOptions{Name: diagnostic1025WorkflowName})
	worker.RegisterWorkflowWithOptions(forbiddenChannelWorkflow, workflow.RegisterOptions{Name: forbiddenChannelWorkflowName})
	worker.RegisterActivityWithOptions(diagnostic1024Activity, activity.RegisterOptions{Name: diagnostic1024ActivityName})
	worker.RegisterActivityWithOptions(diagnostic1025Activity, activity.RegisterOptions{Name: diagnostic1025ActivityName})
	worker.RegisterActivityWithOptions(forbiddenChannelActivity, activity.RegisterOptions{Name: forbiddenChannelActivityName})
}

func diagnostic1024Workflow(ctx workflow.Context, request contract.WorkflowUnitRequest) error {
	return diagnosticWorkflow(ctx, diagnostic1024ActivityName, request)
}

func diagnostic1025Workflow(ctx workflow.Context, request contract.WorkflowUnitRequest) error {
	return diagnosticWorkflow(ctx, diagnostic1025ActivityName, request)
}

func diagnosticWorkflow(ctx workflow.Context, activityName string, request contract.WorkflowUnitRequest) error {
	activityCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		ScheduleToCloseTimeout: 20 * time.Second,
		StartToCloseTimeout:    10 * time.Second,
		HeartbeatTimeout:       2 * time.Second,
		RetryPolicy:            &sdktemporal.RetryPolicy{MaximumAttempts: 1},
	})
	return workflow.ExecuteActivity(activityCtx, activityName, request).Get(activityCtx, nil)
}

func diagnostic1024Activity(ctx context.Context, request contract.WorkflowUnitRequest) error {
	activity.RecordHeartbeat(ctx, contract.WorkflowProgress{
		RunID: request.Run.RunID, TaskID: request.Unit.TaskID, ProgressRef: "safe-progress-ref",
	})
	return errors.New(diagnosticErrorText(diagnosticCanary1024, 1024))
}

func diagnostic1025Activity(ctx context.Context, request contract.WorkflowUnitRequest) error {
	activity.RecordHeartbeat(ctx, contract.WorkflowProgress{
		RunID: request.Run.RunID, TaskID: request.Unit.TaskID, ProgressRef: "safe-progress-ref",
	})
	return errors.New(diagnosticErrorText(diagnosticCanary1025, 1025))
}

func forbiddenChannelWorkflow(ctx workflow.Context, request contract.WorkflowUnitRequest) error {
	activityCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		ScheduleToCloseTimeout: 20 * time.Second,
		StartToCloseTimeout:    10 * time.Second,
		HeartbeatTimeout:       2 * time.Second,
		RetryPolicy:            &sdktemporal.RetryPolicy{MaximumAttempts: 1},
	})
	var result forbiddenWorkflowBody
	return workflow.ExecuteActivity(activityCtx, forbiddenChannelActivityName, request).Get(activityCtx, &result)
}

func forbiddenChannelActivity(
	ctx context.Context,
	_ contract.WorkflowUnitRequest,
) (*forbiddenWorkflowBody, error) {
	activity.RecordHeartbeat(ctx, &forbiddenWorkflowBody{
		marshalCalls: &forbiddenHeartbeatMarshalCalls,
		secret:       secretCanary,
		row:          rowCanary,
		cursor:       cursorCanary,
		body:         bodyCanary,
	})
	return &forbiddenWorkflowBody{
		marshalCalls: &forbiddenResultMarshalCalls,
		secret:       secretCanary,
		row:          rowCanary,
		cursor:       cursorCanary,
		body:         bodyCanary,
	}, nil
}

func diagnosticErrorText(canary string, size int) string {
	return canary + strings.Repeat("x", size-len(canary))
}

func assertTemporalRecordOmits(
	t *testing.T,
	ctx context.Context,
	client sdkclient.Client,
	workflowID, runID string,
	canaries ...string,
) {
	t.Helper()
	description, err := client.DescribeWorkflowExecution(ctx, workflowID, runID)
	if err != nil {
		t.Fatal(err)
	}
	encodedDescription, err := proto.Marshal(description)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range canaries {
		if bytes.Contains(encodedDescription, []byte(canary)) {
			t.Fatalf("diagnostic canary %q reached workflow search or description state", canary)
		}
	}
	iterator := client.GetWorkflowHistory(ctx, workflowID, runID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iterator.HasNext() {
		event, err := iterator.Next()
		if err != nil {
			t.Fatal(err)
		}
		encodedEvent, err := proto.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		for _, canary := range canaries {
			if bytes.Contains(encodedEvent, []byte(canary)) {
				t.Fatalf("diagnostic canary %q reached Temporal history event %d", canary, event.GetEventId())
			}
		}
	}
}

type admittedWorkflowIdentity struct {
	Scope                 contract.Scope
	RunID                 string
	WorkflowID            string
	Origin                contract.RunOrigin
	Generation            uint64
	AdmissionIntentDigest contract.SemanticDigest
}

func admitWorkflowInput(
	t *testing.T,
	fixture *testfixture.Fixture,
	requestKey string,
) admittedWorkflowIdentity {
	t.Helper()
	request := testfixture.Invocation(t, fixture, requestKey)
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fixture.BaseURL+"/v1/workspaces/"+url.PathEscape(fixture.Scope.WorkspaceID)+"/invocations",
		bytes.NewReader(encoded),
	)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Idempotency-Key", request.Protocol.RequestKey)
	response, err := fixture.Client.Do(httpRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	document, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(document) > 64*1024 {
		t.Fatal("admission response exceeded its bounded envelope")
	}
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("admission status = %d; body = %q", response.StatusCode, document)
	}
	var admitted api.AdmissionResponse
	if err := json.Unmarshal(document, &admitted); err != nil {
		t.Fatal(err)
	}
	admissionDigest := contract.SemanticDigest(admitted.IntentDigest)
	if err := admissionDigest.Validate(); err != nil {
		t.Fatalf("validate admitted API intent digest: %v", err)
	}
	identity := admittedWorkflowIdentity{
		Scope:                 fixture.Scope,
		RunID:                 admitted.RunRef,
		WorkflowID:            "pm/run/" + admitted.RunRef,
		Origin:                contract.RunOriginAPI,
		Generation:            1,
		AdmissionIntentDigest: admissionDigest,
	}
	err = store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(
		ctx context.Context,
		tx *store.Transaction,
	) error {
		record, err := store.LoadRun(ctx, tx, identity.RunID)
		if err != nil {
			return err
		}
		if record.Spec.RunID != identity.RunID ||
			record.Spec.OrgID != identity.Scope.OrgID ||
			record.Spec.WorkspaceID != identity.Scope.WorkspaceID ||
			record.Spec.Origin != identity.Origin ||
			record.Spec.IdempotencyFingerprint != identity.AdmissionIntentDigest ||
			record.WorkflowBinding.RunID != identity.RunID ||
			record.WorkflowBinding.WorkflowID != identity.WorkflowID ||
			record.WorkflowBinding.Origin != identity.Origin ||
			record.WorkflowBinding.Generation != identity.Generation ||
			record.WorkflowBinding.Adopted {
			return errors.New("admitted API identity differs from its durable run and workflow binding")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify admitted API identity: %v", err)
	}
	return identity
}

func syntheticWorkflowInput(t *testing.T, label string) contract.StartRunInput {
	t.Helper()
	identity := label + "-" + testIdentity(t)
	encoded, err := json.Marshal(struct {
		Identity string `json:"identity"`
	}{Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	digest, err := contract.CanonicalDigest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	input := contract.StartRunInput{
		Schema: contract.StartRunInputSchema,
		Scope: contract.Scope{
			OrgID:       "synthetic-workflow-tests",
			WorkspaceID: "workspace-" + identity,
		},
		RunID:        "run-" + identity,
		CommandID:    "command-" + identity,
		WorkflowID:   "pm/run/run-" + identity,
		Generation:   1,
		Origin:       contract.RunOriginAPI,
		IntentDigest: digest,
	}
	if err := input.ValidateRelayStart(); err != nil {
		t.Fatal(err)
	}
	return input
}

func workflowSnapshotWithJSONSize(
	t *testing.T,
	reference contract.WorkflowRunRef,
	target int,
) contract.WorkflowRunSnapshot {
	t.Helper()
	snapshot := contract.WorkflowRunSnapshot{
		Run:      reference,
		Deadline: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond),
		Timeouts: testActivityTimeouts(),
		Units:    []contract.WorkflowUnitRef{},
		Commands: []contract.WorkflowCommandRef{},
		Fanout:   1,
	}
	for index := range 256 {
		prefix := fmt.Sprintf("unit-%03d-", index)
		snapshot.Units = append(snapshot.Units, contract.WorkflowUnitRef{TaskID: prefix + "x"})
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) > target {
			break
		}
		remaining := target - len(encoded)
		capacity := 0
		for _, unit := range snapshot.Units {
			capacity += 512 - len(unit.TaskID)
		}
		if remaining > capacity {
			continue
		}
		for unitIndex := range snapshot.Units {
			padding := 512 - len(snapshot.Units[unitIndex].TaskID)
			if padding > remaining {
				padding = remaining
			}
			snapshot.Units[unitIndex].TaskID += strings.Repeat("x", padding)
			remaining -= padding
			if remaining == 0 {
				break
			}
		}
		encoded, err = json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) != target {
			t.Fatalf("constructed payload bytes = %d; want %d", len(encoded), target)
		}
		if err := snapshot.Validate(); err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	t.Fatalf("could not construct a valid %d-byte workflow snapshot", target)
	return contract.WorkflowRunSnapshot{}
}

func testActivityTimeouts() contract.ActivityTimeouts {
	return contract.ActivityTimeouts{
		ScheduleToClose: 20 * time.Second,
		StartToClose:    10 * time.Second,
		Heartbeat:       2 * time.Second,
		Transport:       5 * time.Second,
	}
}

func testIdentity(t *testing.T) string {
	t.Helper()
	return strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()) + "-" + fmt.Sprint(time.Now().UnixNano())
}

type fanoutActivities struct {
	reference contract.WorkflowRunRef
	deadline  time.Time
	reached32 chan struct{}
	started33 chan struct{}
	release   chan struct{}

	mu      sync.Mutex
	loads   int
	active  uint32
	maximum uint32
	started uint32
	once32  sync.Once
	once33  sync.Once
}

func newFanoutActivities(reference contract.WorkflowRunRef) *fanoutActivities {
	return &fanoutActivities{
		reference: reference,
		deadline:  time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond),
		reached32: make(chan struct{}),
		started33: make(chan struct{}),
		release:   make(chan struct{}),
	}
}

func (activities *fanoutActivities) Load(context.Context, contract.WorkflowRunRef) (contract.WorkflowRunSnapshot, error) {
	activities.mu.Lock()
	defer activities.mu.Unlock()
	activities.loads++
	units := []contract.WorkflowUnitRef{}
	done := activities.loads > 1
	if !done {
		for index := range 33 {
			units = append(units, contract.WorkflowUnitRef{TaskID: fmt.Sprintf("fanout-task-%02d", index)})
		}
	}
	return contract.WorkflowRunSnapshot{
		Run: activities.reference, Deadline: activities.deadline,
		Timeouts: testActivityTimeouts(), Units: units, Commands: []contract.WorkflowCommandRef{},
		Done: done, Fanout: contract.MaxWorkflowFanout,
	}, nil
}

func (activities *fanoutActivities) Execute(
	ctx context.Context,
	request contract.WorkflowUnitRequest,
) (contract.WorkflowUnitResult, error) {
	activities.mu.Lock()
	activities.active++
	activities.started++
	if activities.active > activities.maximum {
		activities.maximum = activities.active
	}
	active := activities.active
	started := activities.started
	activities.mu.Unlock()
	if active == contract.MaxWorkflowFanout {
		activities.once32.Do(func() { close(activities.reached32) })
	}
	if started == contract.MaxWorkflowFanout+1 {
		activities.once33.Do(func() { close(activities.started33) })
	}
	select {
	case <-ctx.Done():
		return contract.WorkflowUnitResult{}, ctx.Err()
	case <-activities.release:
	}
	activities.mu.Lock()
	activities.active--
	activities.mu.Unlock()
	return contract.WorkflowUnitResult{
		TaskID: request.Unit.TaskID, State: contract.RunStateSucceeded, ResultRef: "fanout-result",
	}, nil
}

func (*fanoutActivities) Command(
	context.Context,
	contract.WorkflowCommandRequest,
) (contract.WorkflowCommandResult, error) {
	return contract.WorkflowCommandResult{}, errors.New("unexpected fanout command")
}

func (activities *fanoutActivities) Project(
	context.Context,
	contract.WorkflowProjectionRequest,
) (contract.WorkflowProjectionResult, error) {
	return contract.WorkflowProjectionResult{
		RunID: activities.reference.RunID, Revision: 1, State: contract.RunStateSucceeded,
	}, nil
}

func (activities *fanoutActivities) maximumActive() uint32 {
	activities.mu.Lock()
	defer activities.mu.Unlock()
	return activities.maximum
}
