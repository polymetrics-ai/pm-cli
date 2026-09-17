package temporal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	sdkclient "go.temporal.io/sdk/client"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

const physicalCrashRequestEnvironment = "PM_TEMPORAL_CRASH_REQUEST"

const (
	physicalCrashBeforeExecute = "before_execute"
	physicalCrashAfterAccept   = "after_accept"
	physicalCrashBeforeExit    = 86
	physicalCrashAfterExit     = 87
)

type physicalCrashRequest struct {
	Mode        string              `json:"mode"`
	Route       store.WorkflowRoute `json:"route"`
	ReadyPath   string              `json:"ready_path,omitempty"`
	ReleasePath string              `json:"release_path,omitempty"`
}

type crashingWorkflowClient struct {
	WorkflowClient
	mode        string
	readyPath   string
	releasePath string
}

func (client *crashingWorkflowClient) ExecuteWorkflow(
	ctx context.Context,
	options sdkclient.StartWorkflowOptions,
	workflow any,
	arguments ...any,
) (sdkclient.WorkflowRun, error) {
	if client.mode == physicalCrashBeforeExecute {
		os.Exit(physicalCrashBeforeExit)
		return nil, errors.New("unreachable after pre-execute crash")
	}
	run, err := client.WorkflowClient.ExecuteWorkflow(ctx, options, workflow, arguments...)
	if err != nil {
		return nil, err
	}
	if client.mode != physicalCrashAfterAccept {
		return nil, errors.New("physical crash mode is invalid")
	}
	if err := os.WriteFile(client.readyPath, []byte(run.GetRunID()), 0o600); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(client.releasePath); err == nil {
			os.Exit(physicalCrashAfterExit)
			return nil, errors.New("unreachable after post-accept crash")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil, errors.New("independent observer did not release accepted crash process")
}

func TestBothStartHandoffWindows(t *testing.T) {
	if requestPath := os.Getenv(physicalCrashRequestEnvironment); requestPath != "" {
		runPhysicalCrashChild(t, requestPath)
		return
	}

	t.Run("R09_E01_C01_pg_commit_before_start", func(t *testing.T) {
		fixture := testfixture.NewAdmitting(t)
		admitted := admitPhysicalRun(t, fixture, "r09-c01-before-start")
		route := claimPhysicalRoute(t, fixture, "r09-c01-crashed-relay", physicalRouteTTL)
		if route.Claim.RunID != admitted.RunRef {
			t.Fatalf("claimed RunID = %q; want admitted %q", route.Claim.RunID, admitted.RunRef)
		}

		runCrashSubprocess(t, physicalCrashRequest{Mode: physicalCrashBeforeExecute, Route: route}, physicalCrashBeforeExit)
		starts, acknowledged, dispatches, effects, pending := workflowRouteRows(t, fixture, admitted.RunRef)
		var witnesses int
		fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				SELECT count(*) FROM pm.workflow_reconciliation_witnesses
				WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
				  AND kind = 'never_bound_no_dispatch'
			`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, admitted.RunRef).Scan(&witnesses)
		})
		if starts != 1 || acknowledged != 0 || dispatches != 0 || effects != 0 || pending != 1 || witnesses != 1 {
			t.Fatalf(
				"pre-start crash boundary = starts:%d acknowledged:%d dispatches:%d effects:%d pending:%d witnesses:%d",
				starts,
				acknowledged,
				dispatches,
				effects,
				pending,
				witnesses,
			)
		}
		preStartObserver := openPhysicalTemporal(t, randomPhysicalID(t, "r09-c01-pre-start-observer-"))
		preStartRecord := loadPhysicalRun(t, fixture, admitted.RunRef)
		if preStartRecord.State != contract.RunStateAccepted ||
			preStartRecord.WorkflowBinding.WorkflowID != "pm/run/"+admitted.RunRef ||
			len(preStartRecord.WorkflowBinding.Observations) != 0 {
			t.Fatalf("pre-start crash changed admitted run or adopted Temporal evidence: %#v", preStartRecord)
		}
		describeCtx, describeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, describeErr := preStartObserver.DescribeWorkflowExecution(describeCtx, "pm/run/"+admitted.RunRef, "")
		describeCancel()
		var notFound *serviceerror.NotFound
		if !errors.As(describeErr, &notFound) {
			t.Fatalf("pre-start crash Temporal lookup error = %T %v; want NotFound", describeErr, describeErr)
		}
		retryRoute := reclaimPhysicalRoute(t, fixture, admitted.RunRef, "r09-c01-retry-relay", physicalRouteTTL)
		backend := openPhysicalTemporal(t, randomPhysicalID(t, "r09-c01-retry-"))
		relay, _ := newPhysicalRelay(t, fixture, backend, "r09-c01-retry-relay", physicalRouteTTL)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		result, err := relay.StartOrReconcile(ctx, retryRoute)
		if err != nil {
			t.Fatalf("retry stable start after pre-execute crash: %v", err)
		}
		if result.Disposition != HandoffStarted || result.WorkflowID != "pm/run/"+admitted.RunRef || result.TemporalRunID == "" {
			t.Fatalf("pre-execute recovery result = %#v; want one stable start", result)
		}
		observer := openPhysicalTemporal(t, randomPhysicalID(t, "r09-c01-observer-"))
		observedRunID, input := observeWorkflowStart(t, observer, result.WorkflowID)
		if observedRunID != result.TemporalRunID || input.RunID != admitted.RunRef || input.WorkflowID != result.WorkflowID || input.Generation != 1 {
			t.Fatalf("recovered Temporal start = run:%q input:%#v; want exact durable identity", observedRunID, input)
		}
		terminatePhysicalWorkflow(t, observer, result.WorkflowID, result.TemporalRunID)
		assertPhysicalHandoffRows(t, fixture, admitted.RunRef)
	})

	t.Run("R09_E01_C02_temporal_accept_before_ack", func(t *testing.T) {
		fixture := testfixture.NewAdmitting(t)
		admitted := admitPhysicalRun(t, fixture, "r09-c02-after-accept")
		route := claimPhysicalRoute(t, fixture, "r09-c02-crashed-relay", physicalRouteTTL)
		request := physicalCrashRequest{
			Mode:        physicalCrashAfterAccept,
			Route:       route,
			ReadyPath:   filepath.Join(t.TempDir(), "accepted"),
			ReleasePath: filepath.Join(t.TempDir(), "release"),
		}
		command, output := startCrashSubprocess(t, request)
		waitForPhysicalPath(t, request.ReadyPath)

		observer := openPhysicalTemporal(t, randomPhysicalID(t, "r09-c02-observer-"))
		workflowID := "pm/run/" + admitted.RunRef
		observedRunID, input := observeWorkflowStart(t, observer, workflowID)
		if input.RunID != admitted.RunRef || input.WorkflowID != workflowID || input.Generation != 1 {
			t.Fatalf("independently observed start input = %#v; want exact accepted run", input)
		}
		readyRunID, err := os.ReadFile(request.ReadyPath)
		if err != nil || string(readyRunID) != observedRunID {
			t.Fatalf("crashed child accepted RunID = %q error=%v; observer saw %q", readyRunID, err, observedRunID)
		}
		starts, acknowledged, dispatches, effects, pending := workflowRouteRows(t, fixture, admitted.RunRef)
		if starts != 1 || acknowledged != 0 || dispatches != 0 || effects != 0 || pending != 1 {
			t.Fatalf(
				"post-accept pre-ack boundary = starts:%d acknowledged:%d dispatches:%d effects:%d pending:%d",
				starts,
				acknowledged,
				dispatches,
				effects,
				pending,
			)
		}
		preAckRecord := loadPhysicalRun(t, fixture, admitted.RunRef)
		if preAckRecord.State != contract.RunStateAccepted ||
			preAckRecord.WorkflowBinding.WorkflowID != workflowID ||
			len(preAckRecord.WorkflowBinding.Observations) != 0 {
			t.Fatalf("post-accept crash changed admitted run or acknowledged before release: %#v", preAckRecord)
		}
		if err := os.WriteFile(request.ReleasePath, []byte("observed"), 0o600); err != nil {
			t.Fatalf("release observed crash process: %v", err)
		}
		requireCrashExit(t, command.Wait(), physicalCrashAfterExit, output.String())

		retryRoute := reclaimPhysicalRoute(t, fixture, admitted.RunRef, "r09-c02-retry-relay", physicalRouteTTL)
		backend := openPhysicalTemporal(t, randomPhysicalID(t, "r09-c02-retry-"))
		relay, _ := newPhysicalRelay(t, fixture, backend, "r09-c02-retry-relay", physicalRouteTTL)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		result, err := relay.StartOrReconcile(ctx, retryRoute)
		if err != nil {
			t.Fatalf("reconcile accepted identity after process crash: %v", err)
		}
		if result.Disposition != HandoffReconciled || result.WorkflowID != workflowID || result.TemporalRunID != observedRunID {
			t.Fatalf("post-accept recovery result = %#v; want exact existing execution", result)
		}
		terminatePhysicalWorkflow(t, observer, workflowID, observedRunID)
		assertPhysicalHandoffRows(t, fixture, admitted.RunRef)
	})

	t.Run("R09_E01_C03_exact_already_started_race", func(t *testing.T) {
		exerciseAlreadyStartedRace(t, false)
	})

	t.Run("R09_E01_C04_unknown_member_already_started", func(t *testing.T) {
		exerciseAlreadyStartedRace(t, true)
	})

	t.Run("R09_E01_C05_terminal_guard_before_temporal", func(t *testing.T) {
		fixture := testfixture.NewAdmitting(t)
		admitted := admitPhysicalRun(t, fixture, "r09-c05-terminal-guard")
		route := claimPhysicalRoute(t, fixture, "r09-c05-relay", physicalLongRouteTTL)
		stored := loadPhysicalRun(t, fixture, admitted.RunRef)
		terminal, err := stored.Transition(contract.RunStateFailed, "terminal_before_start", time.Now().UTC())
		if err != nil {
			t.Fatalf("construct prior terminal run: %v", err)
		}
		encoded, err := json.Marshal(terminal)
		if err != nil {
			t.Fatalf("encode prior terminal run: %v", err)
		}
		fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE pm.runs
				SET state = $4, revision = $5, record = $6, updated_at = $7
				WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
			`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, admitted.RunRef,
				string(terminal.State), int64(terminal.Revision), encoded, time.Now().UTC())
			return err
		})

		backend := openPhysicalTemporal(t, randomPhysicalID(t, "r09-c05-backend-"))
		counter := &countingWorkflowClient{WorkflowClient: backend}
		relay, _ := newPhysicalRelay(t, fixture, counter, "r09-c05-relay", physicalLongRouteTTL)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		result, err := relay.StartOrReconcile(ctx, route)
		if err != nil {
			t.Fatalf("terminal PostgreSQL guard: %v", err)
		}
		if result.Disposition != HandoffUnsafeToRestart || counter.describeCalls.Load() != 0 || counter.executeCalls.Load() != 0 {
			t.Fatalf("terminal guard result = %#v describes=%d executes=%d", result, counter.describeCalls.Load(), counter.executeCalls.Load())
		}
		observer := openPhysicalTemporal(t, randomPhysicalID(t, "r09-c05-observer-"))
		ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
		_, describeErr := observer.DescribeWorkflowExecution(ctx, "pm/run/"+admitted.RunRef, "")
		cancel()
		var notFound *serviceerror.NotFound
		if !errors.As(describeErr, &notFound) {
			t.Fatalf("terminal guarded workflow lookup error = %v; want real Temporal NotFound", describeErr)
		}
		_, _, dispatches, effects, pending := workflowRouteRows(t, fixture, admitted.RunRef)
		if dispatches != 0 || effects != 0 || pending != 1 {
			t.Fatalf("terminal guard rows = dispatches:%d effects:%d pending:%d", dispatches, effects, pending)
		}
	})

	t.Run("R09_E01_C06_schedule_binding_only", func(t *testing.T) {
		fixture := testfixture.NewAdmitting(t)
		starter := openPhysicalTemporal(t, randomPhysicalID(t, "r09-c06-starter-"))
		workflowID := randomPhysicalID(t, "pm/schedule/")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		run, err := starter.ExecuteWorkflow(ctx, sdkclient.StartWorkflowOptions{
			ID:                       workflowID,
			TaskQueue:                contract.ControlTaskQueue,
			WorkflowExecutionTimeout: 30 * time.Minute,
		}, contract.RunWorkflowType)
		cancel()
		if err != nil {
			t.Fatalf("start scheduled fixture workflow: %v", err)
		}
		terminatePhysicalWorkflow(t, starter, workflowID, run.GetRunID())
		runID := admitScheduledPhysicalRun(t, fixture, workflowID, run.GetRunID())
		route := claimPhysicalRoute(t, fixture, "r09-c06-relay", physicalLongRouteTTL)
		if route.Claim.RunID != runID || route.Claim.Command.Kind != store.WorkflowCommandEnsureBinding {
			t.Fatalf("scheduled route = %#v; want exact ensure binding command", route)
		}
		backend := openPhysicalTemporal(t, randomPhysicalID(t, "r09-c06-backend-"))
		counter := &countingWorkflowClient{WorkflowClient: backend}
		relay, _ := newPhysicalRelay(t, fixture, counter, "r09-c06-relay", physicalLongRouteTTL)
		ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		result, err := relay.EnsureWorkflowBinding(ctx, route)
		cancel()
		if err != nil {
			t.Fatalf("confirm scheduled binding: %v", err)
		}
		if result.Disposition != HandoffBindingConfirmed || result.WorkflowID != workflowID ||
			result.TemporalRunID != run.GetRunID() || counter.describeCalls.Load() != 1 ||
			counter.executeCalls.Load() != 0 {
			t.Fatalf("scheduled binding result = %#v describes=%d executes=%d", result, counter.describeCalls.Load(), counter.executeCalls.Load())
		}
		var acknowledged, dispatches, effects int
		fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				SELECT
				    (SELECT count(*) FROM pm.outbox_commands
				     WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
				       AND kind = 'ensure_workflow_binding' AND acknowledged_at IS NOT NULL),
				    (SELECT count(*) FROM pm.dispatches
				     WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3),
				    (SELECT count(*) FROM pm.effects
				     WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3)
			`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID).Scan(&acknowledged, &dispatches, &effects)
		})
		if acknowledged != 1 || dispatches != 0 || effects != 0 {
			t.Fatalf("scheduled handoff rows = acknowledged:%d dispatches:%d effects:%d", acknowledged, dispatches, effects)
		}
	})
}

func runPhysicalCrashChild(t *testing.T, requestPath string) {
	t.Helper()
	document, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatalf("read physical crash request: %v", err)
	}
	var request physicalCrashRequest
	if err := json.Unmarshal(document, &request); err != nil {
		t.Fatalf("decode physical crash request: %v", err)
	}
	selection := loadPhysicalTemporalSelection(t)
	ctx, cancel := context.WithTimeout(context.Background(), physicalTemporalDialTimeout)
	backend, err := dialPhysicalTemporal(ctx, selection, "r09-crash-child")
	cancel()
	if err != nil {
		t.Fatalf("dial physical Temporal from crash child: %v", err)
	}
	crashing := &crashingWorkflowClient{
		WorkflowClient: backend,
		mode:           request.Mode,
		readyPath:      request.ReadyPath,
		releasePath:    request.ReleasePath,
	}
	client, err := NewClient(ClientConfig{
		Namespace:            selection.configuration.Temporal().Namespace,
		ControlTaskQueue:     contract.ControlTaskQueue,
		WorkflowType:         contract.RunWorkflowType,
		ProjectionUpdateName: contract.ProjectionUpdateName,
		DialTimeout:          physicalTemporalDialTimeout,
		RequestTimeout:       physicalTemporalRequestTimeout,
		Dial:                 func(context.Context) (WorkflowClient, error) { return crashing, nil },
	})
	if err != nil {
		t.Fatalf("construct crash-child Temporal client: %v", err)
	}
	pool := openPhysicalStore(t)
	relay, err := NewRelay(RelayConfig{
		Store: pool, Client: client, OwnerID: request.Route.Claim.OwnerID,
		ClaimLimit: 1, ClaimTTL: physicalRouteTTL,
		IdleDelay: 20 * time.Millisecond, FailureDelay: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("construct crash-child relay: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := relay.StartOrReconcile(ctx, request.Route); err != nil {
		t.Fatalf("crash-child handoff returned before injected crash: %v", err)
	}
	t.Fatal("crash-child handoff completed instead of exiting at the requested boundary")
}

func runCrashSubprocess(t *testing.T, request physicalCrashRequest, exitCode int) {
	t.Helper()
	command, output := startCrashSubprocess(t, request)
	requireCrashExit(t, command.Wait(), exitCode, output.String())
}

func startCrashSubprocess(t *testing.T, request physicalCrashRequest) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	requestPath := filepath.Join(t.TempDir(), "request.json")
	document, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("encode crash request: %v", err)
	}
	if err := os.WriteFile(requestPath, document, 0o600); err != nil {
		t.Fatalf("write crash request: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve physical test executable: %v", err)
	}
	command := exec.Command(executable, "-test.run=^TestBothStartHandoffWindows$", "-test.count=1")
	command.Env = append(os.Environ(), physicalCrashRequestEnvironment+"="+requestPath)
	output := &bytes.Buffer{}
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		t.Fatalf("start physical crash subprocess: %v", err)
	}
	return command, output
}

func requireCrashExit(t *testing.T, err error, exitCode int, output string) {
	t.Helper()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != exitCode {
		t.Fatalf("physical crash subprocess error = %v output=%q; want exit %d", err, output, exitCode)
	}
}

func waitForPhysicalPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("physical subprocess did not publish %q before deadline", path)
}

type alreadyStartedRaceClient struct {
	WorkflowClient
	starter       sdkclient.Client
	mismatch      bool
	describeCalls atomic.Int32
	executeCalls  atomic.Int32
}

func (client *alreadyStartedRaceClient) DescribeWorkflowExecution(
	ctx context.Context,
	workflowID string,
	runID string,
) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	if client.describeCalls.Add(1) == 1 {
		return nil, serviceerror.NewNotFound("injected exact pre-start race")
	}
	return client.WorkflowClient.DescribeWorkflowExecution(ctx, workflowID, runID)
}

func (client *alreadyStartedRaceClient) ExecuteWorkflow(
	ctx context.Context,
	options sdkclient.StartWorkflowOptions,
	workflow any,
	arguments ...any,
) (sdkclient.WorkflowRun, error) {
	client.executeCalls.Add(1)
	externalArguments := append([]any(nil), arguments...)
	if client.mismatch {
		input, ok := externalArguments[0].(contract.StartRunInput)
		if !ok {
			return nil, errors.New("expected typed StartRunInput in race fixture")
		}
		document, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		var ambiguous map[string]any
		if err := json.Unmarshal(document, &ambiguous); err != nil {
			return nil, err
		}
		ambiguous["unexpected_member"] = "must_be_rejected"
		externalArguments[0] = ambiguous
	}
	if _, err := client.starter.ExecuteWorkflow(ctx, options, workflow, externalArguments...); err != nil {
		return nil, err
	}
	return client.WorkflowClient.ExecuteWorkflow(ctx, options, workflow, arguments...)
}

type countingWorkflowClient struct {
	WorkflowClient
	describeCalls atomic.Int32
	executeCalls  atomic.Int32
	updateCalls   atomic.Int32
}

func (client *countingWorkflowClient) DescribeWorkflowExecution(
	ctx context.Context,
	workflowID string,
	runID string,
) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	client.describeCalls.Add(1)
	return client.WorkflowClient.DescribeWorkflowExecution(ctx, workflowID, runID)
}

func (client *countingWorkflowClient) ExecuteWorkflow(
	ctx context.Context,
	options sdkclient.StartWorkflowOptions,
	workflow any,
	arguments ...any,
) (sdkclient.WorkflowRun, error) {
	client.executeCalls.Add(1)
	return client.WorkflowClient.ExecuteWorkflow(ctx, options, workflow, arguments...)
}

func (client *countingWorkflowClient) UpdateWorkflow(
	ctx context.Context,
	options sdkclient.UpdateWorkflowOptions,
) (sdkclient.WorkflowUpdateHandle, error) {
	client.updateCalls.Add(1)
	return client.WorkflowClient.UpdateWorkflow(ctx, options)
}

func exerciseAlreadyStartedRace(t *testing.T, mismatch bool) {
	t.Helper()
	fixture := testfixture.NewAdmitting(t)
	key := "r09-c03-exact-race"
	if mismatch {
		key = "r09-c04-mismatched-race"
	}
	admitted := admitPhysicalRun(t, fixture, key)
	route := claimPhysicalRoute(t, fixture, key+"-relay", physicalLongRouteTTL)
	backend := openPhysicalTemporal(t, randomPhysicalID(t, key+"-backend-"))
	starter := openPhysicalTemporal(t, randomPhysicalID(t, key+"-starter-"))
	tracing := &alreadyStartedRaceClient{WorkflowClient: backend, starter: starter, mismatch: mismatch}
	relay, _ := newPhysicalRelay(t, fixture, tracing, key+"-relay", physicalLongRouteTTL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := relay.StartOrReconcile(ctx, route)
	if err != nil {
		t.Fatalf("exercise AlreadyStarted race: %v", err)
	}
	workflowID := "pm/run/" + admitted.RunRef
	observer := openPhysicalTemporal(t, randomPhysicalID(t, key+"-observer-"))
	var observedRunID string
	var observedInput contract.StartRunInput
	if mismatch {
		description := describeWorkflowForRace(t, observer, workflowID, "")
		observedRunID = description.GetWorkflowExecutionInfo().GetExecution().GetRunId()
	} else {
		observedRunID, observedInput = observeWorkflowStart(t, observer, workflowID)
	}
	terminatePhysicalWorkflow(t, observer, workflowID, observedRunID)
	if tracing.describeCalls.Load() != 2 || tracing.executeCalls.Load() != 1 {
		t.Fatalf(
			"race calls = describes:%d executes:%d; want initial NotFound, evidence lookup, and one losing start",
			tracing.describeCalls.Load(),
			tracing.executeCalls.Load(),
		)
	}
	if mismatch {
		if result.Disposition != HandoffRepairRequired || result.TemporalRunID != "" {
			t.Fatalf("ambiguous race result = %#v; want repair-required without adopting or disclosing unverified execution", result)
		}
		record := loadPhysicalRun(t, fixture, admitted.RunRef)
		if len(record.WorkflowBinding.Observations) != 0 || record.OrchestrationCondition != contract.OrchestrationUnknown {
			t.Fatalf("ambiguous execution was adopted: %#v", record)
		}
		startObject := physicalWorkflowStartObject(t, observer, workflowID, observedRunID)
		if value, present := startObject["unexpected_member"]; !present || string(value) != `"must_be_rejected"` {
			t.Fatalf("ambiguous external start payload = %#v; want retained unexpected member", startObject)
		}
		description := describeWorkflowForRace(t, observer, workflowID, observedRunID)
		if description.GetWorkflowExecutionInfo().GetStatus() != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
			t.Fatalf("ambiguous workflow status = %s; want running (not cancelled or terminated)", description.GetWorkflowExecutionInfo().GetStatus())
		}
		_, _, dispatches, effects, _ := workflowRouteRows(t, fixture, admitted.RunRef)
		if dispatches != 0 || effects != 0 {
			t.Fatalf("ambiguous race emitted dispatch/effect rows: %d/%d", dispatches, effects)
		}
		return
	}
	if result.Disposition != HandoffReconciled || result.TemporalRunID != observedRunID || observedInput.RunID != admitted.RunRef {
		t.Fatalf("exact race result = %#v input=%#v; want exact reconciliation", result, observedInput)
	}
	assertPhysicalHandoffRows(t, fixture, admitted.RunRef)
}

func describeWorkflowForRace(t testing.TB, client sdkclient.Client, workflowID, runID string) *workflowservice.DescribeWorkflowExecutionResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := client.DescribeWorkflowExecution(ctx, workflowID, runID)
	if err != nil {
		t.Fatalf("describe raced Temporal workflow: %v", err)
	}
	return response
}

func assertPhysicalHandoffRows(t testing.TB, fixture *testfixture.Fixture, runID string) {
	t.Helper()
	startCount, acknowledgedStarts, dispatches, effects, pending := workflowRouteRows(t, fixture, runID)
	if startCount != 1 || acknowledgedStarts != 1 || dispatches != 0 || effects != 0 || pending != 1 {
		t.Fatalf(
			"handoff rows = starts:%d acknowledged:%d dispatches:%d effects:%d pending:%d",
			startCount,
			acknowledgedStarts,
			dispatches,
			effects,
			pending,
		)
	}
}
