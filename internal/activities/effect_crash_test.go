package activities_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	sdkworker "go.temporal.io/sdk/worker"

	"github.com/polymetrics-ai/pm-cli/internal/activities"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	pmtemporal "github.com/polymetrics-ai/pm-cli/internal/temporal"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
	"github.com/polymetrics-ai/pm-cli/internal/workflows"
)

const (
	crashHelperModeEnvironment         = "PM_EFFECT_CRASH_HELPER_MODE"
	crashHelperConfigEnvironment       = "PM_EFFECT_CRASH_HELPER_CONFIG"
	crashHelperSecretsEnvironment      = "PM_EFFECT_CRASH_HELPER_SECRETS"
	crashHelperDependenciesEnvironment = "PM_EFFECT_CRASH_HELPER_DEPENDENCIES"
	crashHelperProviderEnvironment     = "PM_EFFECT_CRASH_HELPER_PROVIDER"
	crashHelperProviderCAEnvironment   = "PM_EFFECT_CRASH_HELPER_PROVIDER_CA"
	crashHelperIdentity                = "pm-effect-crash-worker"
)

type crashHelperDependencies struct {
	DeploymentID      string                    `json:"deployment_id"`
	Scope             contract.Scope            `json:"scope"`
	ForeignScope      contract.Scope            `json:"foreign_scope"`
	Connection        testfixture.ConnectionRef `json:"connection"`
	ForeignConnection testfixture.ConnectionRef `json:"foreign_connection"`
	OperationID       string                    `json:"operation_id"`
}

type crashAdmissionIdentity struct {
	Scope                 contract.Scope
	RunID                 string
	WorkflowID            string
	Origin                contract.RunOrigin
	Generation            uint64
	AdmissionIntentDigest contract.SemanticDigest
}

func (dependencies crashHelperDependencies) serviceDependencies() testfixture.ServiceDependencies {
	return testfixture.ServiceDependencies{
		DeploymentID:      dependencies.DeploymentID,
		Scope:             dependencies.Scope,
		ForeignScope:      dependencies.ForeignScope,
		Connection:        dependencies.Connection,
		ForeignConnection: dependencies.ForeignConnection,
		OperationID:       dependencies.OperationID,
	}
}

type providerObservation struct {
	DispatchID string
	AttemptID  string
}

type controlledProvider struct {
	server   *httptest.Server
	accepted chan struct{}
	once     sync.Once

	mu              sync.Mutex
	sends           int
	reconciliations int
	observation     providerObservation
}

func newControlledProvider(t *testing.T) *controlledProvider {
	t.Helper()
	provider := &controlledProvider{accepted: make(chan struct{})}
	provider.server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/effect":
			_, _ = io.Copy(io.Discard, io.LimitReader(request.Body, 4097))
			provider.mu.Lock()
			provider.sends++
			sends := provider.sends
			provider.observation = providerObservation{
				DispatchID: request.Header.Get("X-PM-Dispatch"),
				AttemptID:  request.Header.Get("X-PM-Attempt"),
			}
			provider.mu.Unlock()
			provider.once.Do(func() { close(provider.accepted) })
			if sends > 1 {
				http.Error(writer, "duplicate physical effect", http.StatusConflict)
				return
			}
			<-request.Context().Done()
		case request.Method == http.MethodGet && request.URL.Path == "/effect":
			provider.mu.Lock()
			provider.reconciliations++
			provider.mu.Unlock()
			http.Error(writer, "no definitive witness", http.StatusNotFound)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(provider.server.Close)
	return provider
}

func (provider *controlledProvider) snapshot() (int, int, providerObservation) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.sends, provider.reconciliations, provider.observation
}

func TestWorkerDeathAfterEffect(t *testing.T) {
	provider := newControlledProvider(t)
	fixture := testfixture.NewExecuting(
		t,
		func(context.Context, app.ExecutionRequest) (app.ExecutionObservation, error) {
			return app.ExecutionObservation{}, errors.New("parent process must not execute provider work")
		},
		func(context.Context, app.ExecutionRequest) (app.ExecutionObservation, error) {
			return app.ExecutionObservation{}, errors.New("parent process must not reconcile provider work")
		},
	)
	artifact := testfixture.NewWorkerConfiguration(t)
	workerConfigPath := workerConfigurationForDeployment(t, artifact.Path, fixture.DeploymentID)
	providerCAPath := writeProviderCA(t, provider.server)
	dependencies := crashHelperDependencies{
		DeploymentID:      fixture.DeploymentID,
		Scope:             fixture.Scope,
		ForeignScope:      fixture.ForeignScope,
		Connection:        fixture.Connection,
		ForeignConnection: fixture.ForeignConnection,
		OperationID:       fixture.OperationID,
	}
	encodedDependencies, err := json.Marshal(dependencies)
	if err != nil {
		t.Fatal(err)
	}
	secretsDirectory := os.Getenv("PM_STACK_SECRETS_DIR")
	if secretsDirectory == "" || !filepath.IsAbs(secretsDirectory) {
		t.Fatal("owned fixture secrets directory is required")
	}
	helperEnvironment := []string{
		crashHelperConfigEnvironment + "=" + workerConfigPath,
		crashHelperSecretsEnvironment + "=" + secretsDirectory,
		crashHelperDependenciesEnvironment + "=" + base64.RawStdEncoding.EncodeToString(encodedDependencies),
		crashHelperProviderEnvironment + "=" + provider.server.URL,
		crashHelperProviderCAEnvironment + "=" + providerCAPath,
	}
	admitted := admitCrashRun(t, fixture)
	client, relay := crashTemporalClients(t, artifact.Configuration, fixture)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	runID, reference := relayAndObserveCrashWorkflow(
		t,
		ctx,
		client,
		relay,
		artifact.Configuration.Temporal().Namespace,
		fixture,
		admitted,
	)
	observer, err := pmtemporal.NewWorkflowContinuationObserver(
		client,
		artifact.Configuration.Temporal().Namespace,
		artifact.Configuration.Worker().ActivityTimeouts.Transport,
	)
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
		artifact.Configuration.Worker().ActivityTimeouts,
	)

	testfixture.SealWorkerPlan(t, fixture, service, contract.WorkflowExecution{
		Namespace:  artifact.Configuration.Temporal().Namespace,
		WorkflowID: admitted.WorkflowID,
		RunID:      runID,
	}, reference)
	crashing := startCrashHelper(t, "crash", helperEnvironment)
	run := client.GetWorkflow(ctx, admitted.WorkflowID, runID)
	select {
	case <-provider.accepted:
	case <-ctx.Done():
		t.Fatal("controlled provider did not accept the physical effect")
	}
	if err := crashing.command.Process.Kill(); err != nil {
		t.Fatalf("hard-crash worker after provider acceptance: %v", err)
	}
	err = crashing.command.Wait()
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf("crashing worker exit = %v; want SIGKILL; stderr=%q", err, crashing.stderr.String())
	}
	status, ok := exitError.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("crashing worker status = %v; want SIGKILL; stderr=%q", status, crashing.stderr.String())
	}

	replacement := startCrashHelper(t, "reconcile", helperEnvironment)
	var projection contract.WorkflowProjectionResult
	if err := run.Get(ctx, &projection); err != nil {
		t.Fatal(err)
	}
	if projection.State != contract.RunStateReconciling {
		t.Fatalf("replacement projection state = %q; want reconciling", projection.State)
	}
	if err := replacement.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := replacement.command.Wait(); err != nil {
		t.Fatalf("replacement worker shutdown = %v; stderr=%q", err, replacement.stderr.String())
	}

	sends, reconciliations, observed := provider.snapshot()
	if sends != 1 {
		t.Fatalf("physical provider sends = %d; want exactly one", sends)
	}
	if reconciliations == 0 {
		t.Fatal("replacement worker did not consult the provider witness boundary")
	}
	assertUnknownDispatch(t, fixture, admitted.RunID, observed)
}

func TestCancelBeforeProviderSendProjectsKnownNoEffect(t *testing.T) {
	var providerSends atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/effect" {
			http.NotFound(writer, request)
			return
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(request.Body, 4097))
		providerSends.Add(1)
		writer.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(provider.Close)
	providerClient := crashProviderClient(t, writeProviderCA(t, provider))
	execute := func(
		ctx context.Context,
		request app.ExecutionRequest,
	) (app.ExecutionObservation, error) {
		if request.ReconciliationOnly {
			return app.ExecutionObservation{}, errors.New("prepared unsent dispatch unexpectedly reconciled")
		}
		if err := sendProviderEffect(ctx, providerClient, provider.URL, request); err != nil {
			return app.ExecutionObservation{}, err
		}
		observedAt := time.Now().UTC().Truncate(time.Microsecond)
		outcomes := make([]contract.MemberOutcome, 0, len(request.Dispatch.OrderedMemberIDs))
		for _, memberID := range request.Dispatch.OrderedMemberIDs {
			outcomes = append(outcomes, contract.MemberOutcome{
				MemberID:      memberID,
				EffectKey:     request.Dispatch.EffectKey,
				DispatchID:    request.Dispatch.DispatchID,
				State:         contract.OutcomeSucceeded,
				ObservedAt:    observedAt,
				AssertionRefs: []string{},
			})
		}
		return app.ExecutionObservation{Outcomes: outcomes}, nil
	}
	fixture := testfixture.NewExecuting(
		t,
		execute,
		func(context.Context, app.ExecutionRequest) (app.ExecutionObservation, error) {
			return app.ExecutionObservation{}, errors.New("prepared unsent dispatch unexpectedly reached reconciliation")
		},
	)
	artifact := testfixture.NewWorkerConfiguration(t)
	admitted := admitCrashRun(t, fixture)
	client, relay := crashTemporalClients(t, artifact.Configuration, fixture)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	runID, reference := relayAndObserveCrashWorkflow(
		t,
		ctx,
		client,
		relay,
		artifact.Configuration.Temporal().Namespace,
		fixture,
		admitted,
	)
	observer, err := pmtemporal.NewWorkflowContinuationObserver(
		client,
		artifact.Configuration.Temporal().Namespace,
		artifact.Configuration.Worker().ActivityTimeouts.Transport,
	)
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
		artifact.Configuration.Worker().ActivityTimeouts,
	)

	plan := testfixture.SealWorkerPlan(t, fixture, service, contract.WorkflowExecution{
		Namespace:  artifact.Configuration.Temporal().Namespace,
		WorkflowID: admitted.WorkflowID,
		RunID:      runID,
	}, reference)
	cancelled := cancelPreparedCrashRun(t, fixture, admitted.RunID)
	if cancelled.CommandRef == "" || !bool(cancelled.CancelRequested) {
		t.Fatalf("prepared-run cancellation response = %#v", cancelled)
	}

	worker := startKnownOutcomeWorker(t, artifact.Configuration, client, service)
	defer worker.Stop()
	var projection contract.WorkflowProjectionResult
	if err := client.GetWorkflow(ctx, admitted.WorkflowID, runID).Get(ctx, &projection); err != nil {
		t.Fatal(err)
	}
	if projection.State != contract.RunStateCancelled {
		t.Fatalf("pre-send cancellation projection state = %q; want cancelled", projection.State)
	}
	if sends := providerSends.Load(); sends != 0 {
		t.Fatalf("provider sends after authorized pre-send cancellation = %d; want zero", sends)
	}
	assertKnownUnsentCancellation(t, fixture, admitted.RunID, plan)
}

func TestKnownEffectTruthIsIndependentOfResultPublicationAndCallbackError(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		withResult  bool
		callbackErr bool
	}{
		{name: "raw_action_result_does_not_claim_publication", withResult: true},
		{name: "callback_error_does_not_erase_valid_member_truth", callbackErr: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			rawResultCanary := "PM_RAW_ACTION_RESULT_MUST_NOT_BECOME_A_REFERENCE_" + testCase.name
			execute := func(
				_ context.Context,
				request app.ExecutionRequest,
			) (app.ExecutionObservation, error) {
				if request.ReconciliationOnly {
					return app.ExecutionObservation{}, errors.New("fresh controlled execution was marked reconciliation-only")
				}
				observedAt := time.Now().UTC().Truncate(time.Microsecond)
				outcomes := make([]contract.MemberOutcome, 0, len(request.Dispatch.OrderedMemberIDs))
				for _, memberID := range request.Dispatch.OrderedMemberIDs {
					outcomes = append(outcomes, contract.MemberOutcome{
						MemberID:      memberID,
						EffectKey:     request.Dispatch.EffectKey,
						DispatchID:    request.Dispatch.DispatchID,
						State:         contract.OutcomeSucceeded,
						ObservedAt:    observedAt,
						AssertionRefs: []string{},
					})
				}
				observation := app.ExecutionObservation{Outcomes: outcomes}
				if testCase.withResult {
					observation.Result = &contract.ActionResult{
						MediaType: "application/json",
						Data:      contract.NewStringValue(rawResultCanary),
					}
				}
				if testCase.callbackErr {
					return observation, errors.New("provider callback failed after definitive member observation")
				}
				return observation, nil
			}
			fixture := testfixture.NewExecuting(
				t,
				execute,
				func(context.Context, app.ExecutionRequest) (app.ExecutionObservation, error) {
					return app.ExecutionObservation{}, errors.New("fresh controlled dispatch unexpectedly reconciled")
				},
			)
			artifact := testfixture.NewWorkerConfiguration(t)
			admitted := admitCrashRun(t, fixture)
			client, relay := crashTemporalClients(t, artifact.Configuration, fixture)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			runID, reference := relayAndObserveCrashWorkflow(
				t,
				ctx,
				client,
				relay,
				artifact.Configuration.Temporal().Namespace,
				fixture,
				admitted,
			)
			observer, err := pmtemporal.NewWorkflowContinuationObserver(
				client,
				artifact.Configuration.Temporal().Namespace,
				artifact.Configuration.Worker().ActivityTimeouts.Transport,
			)
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
				artifact.Configuration.Worker().ActivityTimeouts,
			)

			plan := testfixture.SealWorkerPlan(t, fixture, service, contract.WorkflowExecution{
				Namespace:  artifact.Configuration.Temporal().Namespace,
				WorkflowID: admitted.WorkflowID,
				RunID:      runID,
			}, reference)
			worker := startKnownOutcomeWorker(t, artifact.Configuration, client, service)
			defer worker.Stop()

			var projection contract.WorkflowProjectionResult
			if err := client.GetWorkflow(ctx, admitted.WorkflowID, runID).Get(ctx, &projection); err != nil {
				t.Fatal(err)
			}
			if projection.State != contract.RunStateSucceeded {
				t.Fatalf("known-outcome projection state = %q; want succeeded", projection.State)
			}
			assertKnownDispatchWithoutPublishedResult(t, fixture, admitted.RunID, plan)
			if testCase.withResult {
				assertRunResultUnavailable(t, fixture, admitted.RunID, rawResultCanary)
			} else {
				assertRunResultUnavailable(t, fixture, admitted.RunID, "")
			}
		})
	}
}

func startKnownOutcomeWorker(
	t *testing.T,
	configuration config.Configuration,
	client sdkclient.Client,
	service *app.WorkerService,
) sdkworker.Worker {
	t.Helper()
	activityService, err := activities.New(service)
	if err != nil {
		t.Fatal(err)
	}
	workerConfiguration := configuration.Worker()
	coordinator, err := workflows.NewCoordinator(workerConfiguration.ActivityTimeouts)
	if err != nil {
		t.Fatal(err)
	}
	worker := sdkworker.New(client, contract.ControlTaskQueue, sdkworker.Options{
		MaxConcurrentActivityExecutionSize:     int(workerConfiguration.MaxConcurrentActivities),
		MaxConcurrentWorkflowTaskExecutionSize: int(workerConfiguration.MaxConcurrentWorkflowTasks),
		WorkerStopTimeout:                      workerConfiguration.DrainTimeout,
	})
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

func assertKnownDispatchWithoutPublishedResult(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID string,
	plan testfixture.SealedWorkerPlan,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(
		ctx context.Context,
		tx *store.Transaction,
	) error {
		dispatch, err := store.LoadWorkerDispatch(
			ctx,
			tx,
			store.TaskRef{RunID: runID, TaskID: plan.TaskID},
		)
		if err != nil {
			return err
		}
		if dispatch.Intent.DispatchID != plan.Dispatch.DispatchID || len(dispatch.Attempts) != 1 {
			return errors.New("durable dispatch lost its exact sealed plan or attempt")
		}
		outcomes, err := store.ListMemberOutcomes(
			ctx,
			tx,
			store.TaskRef{RunID: runID, TaskID: plan.TaskID},
			plan.Dispatch.DispatchID,
		)
		if err != nil {
			return err
		}
		if len(outcomes) != len(plan.Dispatch.OrderedMemberIDs) {
			return fmt.Errorf("known member outcomes = %d; want %d", len(outcomes), len(plan.Dispatch.OrderedMemberIDs))
		}
		for index, outcome := range outcomes {
			if outcome.MemberID != plan.Dispatch.OrderedMemberIDs[index] ||
				outcome.EffectKey != plan.Dispatch.EffectKey ||
				outcome.DispatchID != plan.Dispatch.DispatchID ||
				outcome.State != contract.OutcomeSucceeded {
				return fmt.Errorf("known member outcome %d lost definitive effect truth", index)
			}
		}
		view, err := store.LoadRunView(ctx, tx, runID)
		if err != nil {
			return err
		}
		summary := view.Record.EffectSummary
		if len(view.ResultRefs) != 0 ||
			summary.Succeeded != uint64(len(plan.Dispatch.OrderedMemberIDs)) ||
			summary.Unsent != 0 || summary.Unchanged != 0 ||
			summary.Rejected != 0 || summary.Unknown != 0 {
			return fmt.Errorf("run publication/effect summary = refs:%v summary:%#v", view.ResultRefs, summary)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertRunResultUnavailable(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID, rawResultCanary string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fixture.BaseURL+"/v1/workspaces/"+url.PathEscape(fixture.Scope.WorkspaceID)+
			"/runs/"+url.PathEscape(runID)+"/result",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := fixture.Client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(body) > 64*1024 {
		t.Fatal("run result refusal exceeded its bounded envelope")
	}
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("run result status = %d; body=%q; want 503", response.StatusCode, body)
	}
	var problem api.Problem
	if err := json.Unmarshal(body, &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "capability_unavailable" {
		t.Fatalf("run result problem code = %q; want capability_unavailable", problem.Code)
	}
	if rawResultCanary != "" && bytes.Contains(body, []byte(rawResultCanary)) {
		t.Fatal("raw callback ActionResult leaked through the unavailable result boundary")
	}
}

type crashProcess struct {
	command *exec.Cmd
	stderr  *bytes.Buffer
}

func startCrashHelper(t *testing.T, mode string, environment []string) crashProcess {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestEffectCrashHelperProcess$", "-test.count=1")
	stderr := &bytes.Buffer{}
	command.Stderr = stderr
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Env = append(append(os.Environ(), environment...), crashHelperModeEnvironment+"="+mode)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	ready := make(chan error, 1)
	go func() {
		announced := false
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if !announced && scanner.Text() == "PM_EFFECT_CRASH_READY" {
				announced = true
				ready <- nil
			}
		}
		if announced {
			return
		}
		if err := scanner.Err(); err != nil {
			ready <- err
			return
		}
		ready <- errors.New("worker helper exited before readiness")
	}()
	select {
	case err := <-ready:
		if err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatalf("start worker helper: %v; stderr=%q", err, stderr.String())
		}
	case <-time.After(20 * time.Second):
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatalf("worker helper readiness timed out; stderr=%q", stderr.String())
	}
	return crashProcess{command: command, stderr: stderr}
}

func TestEffectCrashHelperProcess(t *testing.T) {
	mode := os.Getenv(crashHelperModeEnvironment)
	if mode == "" {
		return
	}
	if mode != "crash" && mode != "reconcile" {
		t.Fatal("unsupported crash helper mode")
	}
	configurationPath := os.Getenv(crashHelperConfigEnvironment)
	secretsDirectory := os.Getenv(crashHelperSecretsEnvironment)
	providerURL := os.Getenv(crashHelperProviderEnvironment)
	providerCAPath := os.Getenv(crashHelperProviderCAEnvironment)
	encodedDependencies := os.Getenv(crashHelperDependenciesEnvironment)
	decodedDependencies, err := base64.RawStdEncoding.DecodeString(encodedDependencies)
	if err != nil {
		t.Fatal("decode helper dependencies")
	}
	var dependencies crashHelperDependencies
	if err := json.Unmarshal(decodedDependencies, &dependencies); err != nil {
		t.Fatal("decode typed helper dependencies")
	}
	providerClient := crashProviderClient(t, providerCAPath)
	execute := func(ctx context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
		if request.ReconciliationOnly {
			return app.ExecutionObservation{}, errors.New("reconciliation-only work reached Execute")
		}
		if err := sendProviderEffect(ctx, providerClient, providerURL, request); err != nil {
			return app.ExecutionObservation{}, err
		}
		return app.ExecutionObservation{}, errors.New("replacement attempted a blind resend")
	}
	reconcile := func(ctx context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
		if !request.ReconciliationOnly {
			return app.ExecutionObservation{}, errors.New("reconciliation callback lacked its restricted marker")
		}
		httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, providerURL+"/effect", nil)
		if err != nil {
			return app.ExecutionObservation{}, err
		}
		httpRequest.Header.Set("X-PM-Dispatch", string(request.Dispatch.DispatchID))
		response, err := providerClient.Do(httpRequest)
		if err != nil {
			return app.ExecutionObservation{}, err
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4097))
		return app.ExecutionObservation{}, errors.New("provider supplied no definitive reconciliation witness")
	}
	registry, resolver, err := testfixture.NewExecutionBoundary(dependencies.serviceDependencies(), execute, reconcile)
	if err != nil {
		t.Fatal(err)
	}
	configurationFile, err := os.Open(configurationPath)
	if err != nil {
		t.Fatal("open helper configuration")
	}
	document, err := config.Load(configurationFile)
	_ = configurationFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := config.Validate(document, config.Prelaunch{})
	if err != nil {
		t.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runtime, err := app.OpenWorkerRuntime(ctx, app.WorkerRuntimeConfig{
		Configuration: configuration, SecretsDirectory: secretsDirectory,
		Registry: registry, ResolveOperation: resolver,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if runtime.TemporalTLS == nil || len(runtime.TemporalTLS.Certificates) != 1 {
		t.Fatal("helper Temporal identity is unavailable")
	}
	certificate := runtime.TemporalTLS.Certificates[0]
	connectionTLS := runtime.TemporalTLS.Clone()
	connectionTLS.Certificates = nil
	temporalConfiguration := configuration.Temporal()
	workerConfiguration := configuration.Worker()
	client, err := pmtemporal.DialWorkerClient(ctx, sdkclient.Options{
		HostPort: temporalConfiguration.Endpoint, Namespace: temporalConfiguration.Namespace,
		Identity:          crashHelperIdentity,
		Credentials:       sdkclient.NewMTLSCredentials(certificate),
		ConnectionOptions: sdkclient.ConnectionOptions{TLS: connectionTLS},
	}, workerConfiguration.DialTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	observer, err := pmtemporal.NewWorkflowContinuationObserver(
		client, temporalConfiguration.Namespace, workerConfiguration.ActivityTimeouts.Transport,
	)
	if err != nil {
		t.Fatal(err)
	}
	service, err := runtime.NewService(observer)
	if err != nil {
		t.Fatal(err)
	}
	activityService, err := activities.New(service)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := workflows.NewCoordinator(workerConfiguration.ActivityTimeouts)
	if err != nil {
		t.Fatal(err)
	}
	worker := sdkworker.New(client, contract.ControlTaskQueue, sdkworker.Options{
		MaxConcurrentActivityExecutionSize:     int(workerConfiguration.MaxConcurrentActivities),
		MaxConcurrentWorkflowTaskExecutionSize: int(workerConfiguration.MaxConcurrentWorkflowTasks),
		WorkerStopTimeout:                      workerConfiguration.DrainTimeout,
	})
	if err := workflows.RegisterWorkflows(worker, coordinator, service.SupportsScheduledAdmission()); err != nil {
		t.Fatal(err)
	}
	if err := activities.RegisterActivities(worker, activityService); err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	defer worker.Stop()
	fmt.Fprintln(os.Stdout, "PM_EFFECT_CRASH_READY")
	_ = os.Stdout.Sync()
	<-ctx.Done()
}

func sendProviderEffect(
	ctx context.Context,
	client *http.Client,
	providerURL string,
	request app.ExecutionRequest,
) error {
	body := strings.NewReader(string(request.Dispatch.DispatchID))
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, providerURL+"/effect", body)
	if err != nil {
		return err
	}
	httpRequest.Header.Set("Content-Type", "text/plain")
	httpRequest.Header.Set("X-PM-Dispatch", string(request.Dispatch.DispatchID))
	httpRequest.Header.Set("X-PM-Attempt", string(request.Attempt.AttemptID))
	response, err := client.Do(httpRequest)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4097))
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("provider effect status %d", response.StatusCode)
	}
	return nil
}

func crashProviderClient(t *testing.T, caPath string) *http.Client {
	t.Helper()
	ca, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatal("read controlled provider CA")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("controlled provider CA is invalid")
	}
	transport := &http.Transport{
		Proxy:             nil,
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		DisableKeepAlives: true,
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("controlled provider redirects are disabled")
		},
	}
}

func workerConfigurationForDeployment(t *testing.T, sourcePath, deploymentID string) string {
	t.Helper()
	document, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(document, &object); err != nil {
		t.Fatal(err)
	}
	encodedDeployment, err := json.Marshal(deploymentID)
	if err != nil {
		t.Fatal(err)
	}
	object["deployment_id"] = encodedDeployment
	encoded, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(sourcePath), "effect-crash-worker-"+fmt.Sprint(time.Now().UnixNano())+".json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove crash worker configuration: %v", err)
		}
	})
	return path
}

func writeProviderCA(t *testing.T, server *httptest.Server) string {
	t.Helper()
	certificate := server.Certificate()
	if certificate == nil {
		t.Fatal("controlled provider certificate is unavailable")
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	path := filepath.Join(t.TempDir(), "provider-ca.pem")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func admitCrashRun(t *testing.T, fixture *testfixture.Fixture) crashAdmissionIdentity {
	t.Helper()
	request := testfixture.Invocation(t, fixture, "effect-crash")
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
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(body) > 64*1024 || response.StatusCode != http.StatusAccepted {
		t.Fatalf("effect crash admission = HTTP %d %q", response.StatusCode, body)
	}
	var admitted api.AdmissionResponse
	if err := json.Unmarshal(body, &admitted); err != nil {
		t.Fatal(err)
	}
	admissionDigest := contract.SemanticDigest(admitted.IntentDigest)
	if err := admissionDigest.Validate(); err != nil {
		t.Fatalf("validate effect admission digest: %v", err)
	}
	identity := crashAdmissionIdentity{
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
			return errors.New("effect admission differs from its durable run and workflow binding")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify effect admission identity: %v", err)
	}
	return identity
}

func crashTemporalClients(
	t *testing.T,
	configuration config.Configuration,
	fixture *testfixture.Fixture,
) (sdkclient.Client, *pmtemporal.Relay) {
	t.Helper()
	options := testfixture.TemporalOptions(t, configuration, configuration.Temporal().Namespace, "effect-crash-parent")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := pmtemporal.DialWorkerClient(ctx, options, configuration.Worker().DialTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	relayOptions := options
	relayOptions.Identity = "effect-crash-relay"

	temporalClient, err := pmtemporal.NewClient(pmtemporal.ClientConfig{
		Namespace: configuration.Temporal().Namespace, ControlTaskQueue: contract.ControlTaskQueue,
		WorkflowType: contract.RunWorkflowType, ProjectionUpdateName: contract.ProjectionUpdateName,
		DialTimeout:    configuration.Worker().DialTimeout,
		RequestTimeout: configuration.Worker().ActivityTimeouts.Transport,
		Dial: func(ctx context.Context) (pmtemporal.WorkflowClient, error) {
			return pmtemporal.DialWorkerClient(ctx, relayOptions, configuration.Worker().DialTimeout)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(temporalClient.Close)
	relay, err := pmtemporal.NewRelay(pmtemporal.RelayConfig{
		Store: fixture.Pool, Client: temporalClient, OwnerID: "effect-crash-relay",
		ClaimLimit: 1, ClaimTTL: 10 * time.Second, IdleDelay: 10 * time.Millisecond, FailureDelay: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, relay
}
func relayAndObserveCrashWorkflow(
	t *testing.T,
	ctx context.Context,
	client sdkclient.Client,
	relay *pmtemporal.Relay,
	namespace string,
	fixture *testfixture.Fixture,
	admitted crashAdmissionIdentity,
) (string, contract.WorkflowRunRef) {
	t.Helper()
	var lastRelayErr error
	for {
		if err := relay.RunOnce(ctx); err != nil {
			lastRelayErr = err
		}
		description, err := client.DescribeWorkflowExecution(ctx, admitted.WorkflowID, "")
		if err == nil {
			runID := description.GetWorkflowExecutionInfo().GetExecution().GetRunId()
			if runID != "" {
				testfixture.CleanupWorkflowChain(
					t,
					client,
					namespace,
					admitted.WorkflowID,
					runID,
				)
				canonical := loadCrashWorkflowInput(
					t,
					ctx,
					client,
					admitted.WorkflowID,
					runID,
				)
				if canonical.RunID != admitted.RunID ||
					!canonical.Scope.Equal(admitted.Scope) ||
					canonical.WorkflowID != admitted.WorkflowID ||
					canonical.Origin != admitted.Origin ||
					canonical.Generation != admitted.Generation {
					t.Fatalf("canonical crash start identity = %#v; want admitted core %#v", canonical, admitted)
				}
				err = store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(
					ctx context.Context,
					tx *store.Transaction,
				) error {
					intent, err := store.LoadWorkerCommandIntent(
						ctx,
						tx,
						canonical.RunID,
						canonical.CommandID,
					)
					if err != nil {
						return err
					}
					budget, err := store.LoadRunBudgetSnapshot(ctx, tx, canonical.RunID)
					if err != nil {
						return err
					}
					if !intent.Scope.Equal(canonical.Scope) ||
						intent.RunID != canonical.RunID ||
						intent.CommandID != canonical.CommandID ||
						intent.Kind != store.WorkflowCommandStartRun ||
						intent.Digest != canonical.IntentDigest ||
						intent.Plan.Namespace != namespace ||
						intent.Plan.TaskQueue != contract.ControlTaskQueue ||
						intent.Plan.WorkflowType != contract.RunWorkflowType ||
						intent.Plan.WorkflowID != canonical.WorkflowID ||
						intent.Plan.Origin != canonical.Origin ||
						intent.Plan.Generation != canonical.Generation ||
						intent.Plan.ExecutionTimeout != budget.Policy.RunLifetime ||
						intent.Plan.BudgetDigest != budget.Digest {
						return errors.New("canonical crash start differs from the exact sealed workflow intent")
					}
					return nil
				})
				if err != nil {
					t.Fatalf("verify canonical crash start intent: %v", err)
				}
				return runID, canonical.RunRef()
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf(
				"relay did not observe its owned crash workflow before the bound: %v",
				lastRelayErr,
			)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func loadCrashWorkflowInput(
	t *testing.T,
	ctx context.Context,
	client sdkclient.Client,
	workflowID, runID string,
) contract.StartRunInput {
	t.Helper()
	history := client.GetWorkflowHistory(
		ctx,
		workflowID,
		runID,
		false,
		enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT,
	)
	if !history.HasNext() {
		t.Fatal("crash workflow start history is empty")
	}
	event, err := history.Next()
	if err != nil {
		t.Fatal(err)
	}
	started := event.GetWorkflowExecutionStartedEventAttributes()
	if started == nil {
		t.Fatal("crash workflow history does not begin with its start event")
	}
	var input contract.StartRunInput
	if err := converter.GetDefaultDataConverter().FromPayloads(started.GetInput(), &input); err != nil {
		t.Fatalf("decode crash workflow start input: %v", err)
	}
	if err := input.ValidateRelayStart(); err != nil {
		t.Fatalf("validate crash workflow start input: %v", err)
	}
	return input
}

func assertUnknownDispatch(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID string,
	observed providerObservation,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		tasks, err := store.ListWorkerTasks(ctx, tx, runID)
		if err != nil {
			return err
		}
		if len(tasks) != 1 {
			return fmt.Errorf("durable worker tasks = %d; want one", len(tasks))
		}
		task := tasks[0]
		dispatch, err := store.LoadWorkerDispatch(ctx, tx, store.TaskRef{RunID: runID, TaskID: task.TaskID})
		if err != nil {
			return err
		}
		if len(dispatch.Attempts) != 1 || string(dispatch.Intent.DispatchID) != observed.DispatchID ||
			string(dispatch.Attempts[0].AttemptID) != observed.AttemptID {
			return fmt.Errorf("durable dispatch/attempt differs from provider acceptance")
		}
		outcomes, err := store.ListMemberOutcomes(ctx, tx, store.TaskRef{RunID: runID, TaskID: task.TaskID}, dispatch.Intent.DispatchID)
		if err != nil {
			return err
		}
		if len(outcomes) != len(dispatch.Intent.OrderedMemberIDs) {
			return fmt.Errorf("durable outcomes = %d; want %d", len(outcomes), len(dispatch.Intent.OrderedMemberIDs))
		}
		for index, outcome := range outcomes {
			if outcome.MemberID != dispatch.Intent.OrderedMemberIDs[index] || outcome.State != contract.OutcomeUnknown ||
				outcome.DispatchID != dispatch.Intent.DispatchID || outcome.EffectKey != dispatch.Intent.EffectKey {
				return fmt.Errorf("durable unknown outcome %d differs from sealed member identity", index)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func cancelPreparedCrashRun(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID string,
) api.RunCancelResponse {
	t.Helper()
	endpoint := fixture.BaseURL + "/v1/workspaces/" + url.PathEscape(fixture.Scope.WorkspaceID) +
		"/runs/" + url.PathEscape(runID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	viewRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	viewResponse, err := fixture.Client.Do(viewRequest)
	if err != nil {
		t.Fatal(err)
	}
	viewBody, readErr := io.ReadAll(io.LimitReader(viewResponse.Body, 64*1024+1))
	closeErr := viewResponse.Body.Close()
	if readErr != nil || closeErr != nil || len(viewBody) > 64*1024 || viewResponse.StatusCode != http.StatusOK {
		t.Fatalf(
			"read prepared run before cancellation = HTTP %d body=%q read=%v close=%v",
			viewResponse.StatusCode,
			viewBody,
			readErr,
			closeErr,
		)
	}
	var view api.RunView
	if err := json.Unmarshal(viewBody, &view); err != nil {
		t.Fatal(err)
	}
	requestKey := "cancel-before-provider-send-" + fmt.Sprint(time.Now().UnixNano())
	body, err := json.Marshal(api.RunCancelRequest{Protocol: api.ProtocolMetadata{
		WireVersion:      api.ProtocolMetadataWireVersion(api.WireVersion),
		RequiredFeatures: []string{},
		RequestKey:       requestKey,
		IntentDigest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		ExpectedRevision: view.Revision,
	}})
	if err != nil {
		t.Fatal(err)
	}
	cancelRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		endpoint+"/cancel",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	cancelRequest.Header.Set("Content-Type", "application/json")
	cancelRequest.Header.Set("Idempotency-Key", requestKey)
	cancelRequest.Header.Set("If-Match", view.Revision)
	cancelResponse, err := fixture.Client.Do(cancelRequest)
	if err != nil {
		t.Fatal(err)
	}
	cancelBody, readErr := io.ReadAll(io.LimitReader(cancelResponse.Body, 64*1024+1))
	closeErr = cancelResponse.Body.Close()
	if readErr != nil || closeErr != nil || len(cancelBody) > 64*1024 ||
		cancelResponse.StatusCode != http.StatusAccepted {
		t.Fatalf(
			"cancel prepared run = HTTP %d body=%q read=%v close=%v",
			cancelResponse.StatusCode,
			cancelBody,
			readErr,
			closeErr,
		)
	}
	var response api.RunCancelResponse
	if err := json.Unmarshal(cancelBody, &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func assertKnownUnsentCancellation(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID string,
	plan testfixture.SealedWorkerPlan,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(
		ctx context.Context,
		tx *store.Transaction,
	) error {
		task := store.TaskRef{RunID: runID, TaskID: plan.TaskID}
		dispatch, err := store.LoadWorkerDispatch(ctx, tx, task)
		if err != nil {
			return err
		}
		if dispatch.Intent.DispatchID != plan.Dispatch.DispatchID ||
			dispatch.State != store.DispatchPrepared ||
			len(dispatch.Attempts) != 0 {
			return fmt.Errorf("cancelled pre-send dispatch = %#v; want exact prepared intent without attempts", dispatch)
		}
		effects, err := store.ListWorkerRunEffects(ctx, tx, runID)
		if err != nil {
			return err
		}
		if len(effects) != 1 ||
			effects[0].EffectKey != plan.Dispatch.EffectKey ||
			effects[0].State != contract.OutcomeUnsent ||
			len(effects[0].Attempts) != 0 {
			return fmt.Errorf("cancelled pre-send effects = %#v; want one known-unsent effect", effects)
		}
		outcomes, err := store.ListMemberOutcomes(ctx, tx, task, plan.Dispatch.DispatchID)
		if err != nil {
			return err
		}
		if len(outcomes) != 0 {
			return fmt.Errorf("cancelled pre-send member outcomes = %#v; want none", outcomes)
		}
		view, err := store.LoadRunView(ctx, tx, runID)
		if err != nil {
			return err
		}
		summary := view.Record.EffectSummary
		if view.Record.State != contract.RunStateCancelled ||
			!view.Record.CancelRequested ||
			summary.Unsent != 1 ||
			summary.Succeeded != 0 ||
			summary.Unchanged != 0 ||
			summary.Rejected != 0 ||
			summary.Unknown != 0 ||
			view.Record.UnknownEffectCount != 0 {
			return fmt.Errorf("cancelled pre-send run view = %#v; want known-unsent cancelled truth", view)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
