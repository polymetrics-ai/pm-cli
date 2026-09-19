package app_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/activities"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	pmtemporal "github.com/polymetrics-ai/pm-cli/internal/temporal"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
	"github.com/polymetrics-ai/pm-cli/internal/workflows"
	sdkclient "go.temporal.io/sdk/client"
	sdkworker "go.temporal.io/sdk/worker"
)

func newInitialRunFixture(t *testing.T) *testfixture.Fixture {
	t.Helper()
	return testfixture.NewAdmitting(t)
}

type initialRunResponse struct {
	status      int
	problemCode string
	problem     api.Problem
	admission   api.AdmissionResponse
	document    []byte
}

func TestAcceptedRunSurvivesClientExit(t *testing.T) {
	if requestPath := os.Getenv(lifecycleChildRequestEnvironment); requestPath != "" {
		runLifecycleChild(t, requestPath)
		return
	}

	temporal := newPermitTemporalHarness(t)

	t.Run("pm-cli-run-watch-E02-C01_logout_or_session_expiry_does_not_cancel", func(t *testing.T) {
		t.Run("accepted_grant_outlives_session", func(t *testing.T) {
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
				"issue20-session-expiry",
			)
			run := preparePermitWorkerRun(
				t,
				fixture,
				service,
				temporal,
				fixture.Scope,
				fixture.Actor,
				admitted,
			)

			expirePermitTestSessions(t, fixture, fixture.Actor)
			requirePermitTestSessionExpired(t, fixture)

			result, err := executePermitWorkerUnit(t, service, run)
			if err != nil {
				t.Fatalf("execute accepted run after its viewing session expired: %v", err)
			}
			record := loadLifecycleRunRecord(t, fixture, admitted.RunRef)
			if result.State != contract.RunStateSucceeded || sends.Load() != 1 ||
				record.Spec.RunID != admitted.RunRef || record.CancelRequested {
				t.Fatalf(
					"session-independent accepted run = result %#v sends %d record %#v; want same succeeded run without cancellation",
					result,
					sends.Load(),
					record,
				)
			}
		})

		t.Run("explicit_grant_revocation_stops_new_effects", func(t *testing.T) {
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
				"issue20-explicit-grant-revocation",
			)
			run := preparePermitWorkerRun(
				t,
				fixture,
				service,
				temporal,
				fixture.Scope,
				fixture.Actor,
				admitted,
			)
			revokePermitTestAuthority(t, fixture, admitted.RunRef, "grant")
			if _, err := executePermitWorkerUnit(t, service, run); !errors.Is(err, app.ErrWorkerAuthorization) {
				t.Fatalf("worker error after explicit run-grant revocation = %v; want ErrWorkerAuthorization", err)
			}
			permits, attempts := permitTestDurableCounts(t, fixture, fixture.Scope, admitted.RunRef)
			if sends.Load() != 0 || permits != 0 || attempts != 0 {
				t.Fatalf(
					"physical work after explicit run-grant revocation = sends %d permits %d attempts %d; want 0/0/0",
					sends.Load(),
					permits,
					attempts,
				)
			}
		})
	})

	t.Run("pm-cli-run-watch-E02-C02_api_and_worker_death_recovers_accepted_run", func(t *testing.T) {
		fixture := newLifecycleExecutingFixture(t)
		provider := newLifecycleProvider(t, true)
		configurationPath := lifecycleWorkerConfiguration(t, fixture)

		firstAPI, firstEndpoint := startLifecycleAPIProcess(t, fixture)
		apiClient := openLifecycleParentClient(t, fixture, firstEndpoint)
		admitted := admitLifecycleRunAt(
			t,
			fixture,
			apiClient.Client,
			apiClient.BaseURL,
			"issue20-api-worker-process-death",
		)
		beforeOutbox := loadLifecycleStartOutbox(t, fixture, admitted.RunRef)
		run := prepareLifecycleWorkerRun(t, fixture, temporal, admitted)

		departing := startLifecycleWorkerProcess(
			t,
			fixture,
			configurationPath,
			provider,
			lifecycleWorkerCrash,
		)
		provider.awaitAccepted(t)
		firstAPI.kill(t, "departing API")
		departing.kill(t, "departing worker")

		replacementAPI, replacementEndpoint := startLifecycleAPIProcess(t, fixture)
		replacement := startLifecycleWorkerProcess(
			t,
			fixture,
			configurationPath,
			provider,
			lifecycleWorkerReconcile,
		)
		projection := awaitLifecycleWorkflow(t, temporal, run)
		secondView := observeLifecycleRunInChild(t, fixture, replacementEndpoint, admitted.RunRef)
		afterOutbox := loadLifecycleStartOutbox(t, fixture, admitted.RunRef)

		replacement.stop(t, "replacement worker")
		replacementAPI.stop(t, "replacement API")
		assertLifecycleRecoveredEffect(t, fixture, run, provider, projection)
		if beforeOutbox.count != 1 || beforeOutbox.state != "pending" ||
			afterOutbox.count != 1 || afterOutbox.state != "acknowledged" ||
			beforeOutbox.commandID != afterOutbox.commandID ||
			secondView.RunRef != admitted.RunRef ||
			secondView.State != api.RunStateSucceeded ||
			secondView.CancelRequested {
			t.Fatalf(
				"API/worker process recovery = before outbox %#v after %#v second view %#v; want one same uncancelled succeeded run",
				beforeOutbox,
				afterOutbox,
				secondView,
			)
		}
	})

	t.Run("pm-cli-run-watch-E02-C03_client_exit_second_client_observes_run", func(t *testing.T) {
		fixture := newLifecycleExecutingFixture(t)
		provider := newLifecycleProvider(t, false)
		configurationPath := lifecycleWorkerConfiguration(t, fixture)
		admitted := admitPermitRun(
			t,
			fixture,
			fixture.Client,
			fixture.Scope,
			fixture.Connection.ConnectionID,
			"issue20-client-process-exit",
		)
		run := prepareLifecycleWorkerRun(t, fixture, temporal, admitted)

		firstAccess := fixture.WriteCurrentProcessClientAccess(t, fixture.Actor)
		firstClient, firstReceipt := startLifecycleClientProcess(
			t,
			firstAccess,
			lifecycleRunEventsPath(fixture, admitted.RunRef),
			true,
			true,
		)
		if firstReceipt.Status != http.StatusOK ||
			!strings.Contains(firstReceipt.Body, admitted.RunRef) {
			t.Fatalf("departing client watch readiness = %#v; want accepted run %q", firstReceipt, admitted.RunRef)
		}
		if active := fixture.ActiveRequests(); active != 1 {
			t.Fatalf("departing client established %d live API requests; want one SSE watch", active)
		}
		firstClient.kill(t, "departing client")
		awaitLifecycleActiveRequests(t, fixture, 0)

		worker := startLifecycleWorkerProcess(
			t,
			fixture,
			configurationPath,
			provider,
			lifecycleWorkerComplete,
		)
		projection := awaitLifecycleWorkflow(t, temporal, run)
		secondView := observeLifecycleRunInCurrentAPI(t, fixture, admitted.RunRef)
		worker.stop(t, "completing worker")
		assertLifecycleRecoveredEffect(t, fixture, run, provider, projection)
		if secondView.RunRef != admitted.RunRef ||
			secondView.State != api.RunStateSucceeded ||
			secondView.CancelRequested {
			t.Fatalf("second client observed %#v; want same uncancelled succeeded run", secondView)
		}
	})

	t.Run("pm-cli-run-watch-E02-C04_api_restart_serves_durable_run_and_outbox", func(t *testing.T) {
		fixture := testfixture.NewAdmitting(t)
		firstAPI, firstEndpoint := startLifecycleAPIProcess(t, fixture)
		apiClient := openLifecycleParentClient(t, fixture, firstEndpoint)
		admitted := admitLifecycleRunAt(
			t,
			fixture,
			apiClient.Client,
			apiClient.BaseURL,
			"issue20-api-process-restart",
		)
		beforeView := observeLifecycleRunInChild(t, fixture, firstEndpoint, admitted.RunRef)
		beforeOutbox := loadLifecycleStartOutbox(t, fixture, admitted.RunRef)

		firstAPI.kill(t, "departing API")
		replacementAPI, replacementEndpoint := startLifecycleAPIProcess(t, fixture)
		afterView := observeLifecycleRunInChild(t, fixture, replacementEndpoint, admitted.RunRef)
		afterOutbox := loadLifecycleStartOutbox(t, fixture, admitted.RunRef)
		replacementAPI.stop(t, "replacement API")

		if beforeView.RunRef != admitted.RunRef ||
			beforeView.State != api.RunStateAccepted ||
			beforeView.CancelRequested ||
			afterView.RunRef != beforeView.RunRef ||
			afterView.Revision != beforeView.Revision ||
			afterView.State != beforeView.State ||
			afterView.CancelRequested ||
			afterOutbox != beforeOutbox ||
			afterOutbox.count != 1 ||
			afterOutbox.state != "pending" {
			t.Fatalf(
				"post-process API durability = before view %#v outbox %#v; after view %#v outbox %#v",
				beforeView,
				beforeOutbox,
				afterView,
				afterOutbox,
			)
		}
	})

	t.Run("pm-cli-run-watch-E02-C05_worker_replacement_resumes_same_run_identity", func(t *testing.T) {
		fixture := newLifecycleExecutingFixture(t)
		provider := newLifecycleProvider(t, true)
		configurationPath := lifecycleWorkerConfiguration(t, fixture)
		admitted := admitPermitRun(
			t,
			fixture,
			fixture.Client,
			fixture.Scope,
			fixture.Connection.ConnectionID,
			"issue20-worker-process-replacement",
		)
		run := prepareLifecycleWorkerRun(t, fixture, temporal, admitted)

		departing := startLifecycleWorkerProcess(
			t,
			fixture,
			configurationPath,
			provider,
			lifecycleWorkerCrash,
		)
		provider.awaitAccepted(t)
		departing.kill(t, "departing worker")

		replacement := startLifecycleWorkerProcess(
			t,
			fixture,
			configurationPath,
			provider,
			lifecycleWorkerReconcile,
		)
		projection := awaitLifecycleWorkflow(t, temporal, run)
		secondView := observeLifecycleRunInCurrentAPI(t, fixture, admitted.RunRef)
		replacement.stop(t, "replacement worker")
		assertLifecycleRecoveredEffect(t, fixture, run, provider, projection)
		record := loadLifecycleRunRecord(t, fixture, admitted.RunRef)
		if run.reference.RunID != admitted.RunRef ||
			record.Spec.RunID != admitted.RunRef ||
			record.State != contract.RunStateSucceeded ||
			record.CancelRequested ||
			secondView.RunRef != admitted.RunRef ||
			secondView.State != api.RunStateSucceeded ||
			secondView.CancelRequested {
			t.Fatalf(
				"replacement worker recovery = reference %#v record %#v second view %#v; want same uncancelled succeeded run",
				run.reference,
				record,
				secondView,
			)
		}
	})
}

const (
	lifecycleChildRequestEnvironment = "PM_LIFECYCLE_CHILD_REQUEST"
	lifecycleChildAPI                = "api"
	lifecycleChildClient             = "client"
	lifecycleChildWorker             = "worker"
	lifecycleWorkerCrash             = "crash_after_provider_accept"
	lifecycleWorkerReconcile         = "reconcile_existing_effect"
	lifecycleWorkerComplete          = "complete_once"
	lifecycleProcessDocumentLimit    = 256 << 10
	lifecycleProcessReadyTimeout     = 30 * time.Second
	lifecycleProcessExitTimeout      = 30 * time.Second
)

var lifecycleProcessIdentitySequence atomic.Uint64

func nextLifecycleProcessIdentity(role string) string {
	return fmt.Sprintf("issue20-%s-%d", role, lifecycleProcessIdentitySequence.Add(1))
}

type lifecycleChildRequest struct {
	Role                string                          `json:"role"`
	ReadyPath           string                          `json:"ready_path"`
	Dependencies        testfixture.ProcessDependencies `json:"dependencies,omitzero"`
	AccessPath          string                          `json:"access_path,omitempty"`
	RequestPath         string                          `json:"request_path,omitempty"`
	Watch               bool                            `json:"watch,omitempty"`
	Hold                bool                            `json:"hold,omitempty"`
	ConfigurationPath   string                          `json:"configuration_path,omitempty"`
	SecretsDirectory    string                          `json:"secrets_directory,omitempty"`
	ProviderURL         string                          `json:"provider_url,omitempty"`
	ProviderCertificate []byte                          `json:"provider_certificate,omitempty"`
	WorkerMode          string                          `json:"worker_mode,omitempty"`
	WorkerIdentity      string                          `json:"worker_identity,omitempty"`
}

type lifecycleChildReceipt struct {
	Kind   string                       `json:"kind"`
	API    *testfixture.ProcessAPIReady `json:"api,omitempty"`
	Status int                          `json:"status,omitempty"`
	Body   string                       `json:"body,omitempty"`
}

type lifecycleProcess struct {
	command *exec.Cmd
	stdout  bytes.Buffer
	stderr  bytes.Buffer
	wait    <-chan error
	exited  bool
	waitErr error
}

func startLifecycleProcess(
	t *testing.T,
	request lifecycleChildRequest,
) (*lifecycleProcess, lifecycleChildReceipt) {
	t.Helper()
	directory := t.TempDir()
	requestPath := filepath.Join(directory, "request.json")
	request.ReadyPath = filepath.Join(directory, "ready.json")
	document, err := json.Marshal(request)
	if err != nil || len(document) > lifecycleProcessDocumentLimit {
		t.Fatal("encode bounded lifecycle child request")
	}
	if err := os.WriteFile(requestPath, document, 0o600); err != nil {
		t.Fatalf("write lifecycle child request: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate lifecycle test executable: %v", err)
	}
	process := &lifecycleProcess{}
	process.command = exec.Command(
		executable,
		"-test.run=^TestAcceptedRunSurvivesClientExit$",
		"-test.count=1",
	)
	process.command.Env = append(
		os.Environ(),
		lifecycleChildRequestEnvironment+"="+requestPath,
	)
	process.command.Stdout = &process.stdout
	process.command.Stderr = &process.stderr
	process.command.WaitDelay = 5 * time.Second
	if err := process.command.Start(); err != nil {
		t.Fatalf("start lifecycle %s child: %v", request.Role, err)
	}
	waited := make(chan error, 1)
	process.wait = waited
	go func() { waited <- process.command.Wait() }()
	t.Cleanup(func() {
		if process.exited {
			return
		}
		_ = process.command.Process.Kill()
		process.waitErr = <-process.wait
		process.exited = true
	})
	receipt := process.awaitReceipt(t, request.ReadyPath, request.Role)
	return process, receipt
}

func (process *lifecycleProcess) awaitReceipt(
	t *testing.T,
	path string,
	role string,
) lifecycleChildReceipt {
	t.Helper()
	deadline := time.NewTimer(lifecycleProcessReadyTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if receipt, ok := readLifecycleReceipt(path); ok {
			return receipt
		}
		select {
		case err := <-process.wait:
			process.exited = true
			process.waitErr = err
			if receipt, ok := readLifecycleReceipt(path); ok && err == nil {
				return receipt
			}
			t.Fatalf(
				"lifecycle %s child exited before readiness: %v stdout=%q stderr=%q",
				role,
				err,
				process.stdout.String(),
				process.stderr.String(),
			)
		case <-ticker.C:
		case <-deadline.C:
			_ = process.command.Process.Kill()
			process.waitErr = <-process.wait
			process.exited = true
			t.Fatalf(
				"lifecycle %s child readiness exceeded %s: stdout=%q stderr=%q",
				role,
				lifecycleProcessReadyTimeout,
				process.stdout.String(),
				process.stderr.String(),
			)
		}
	}
}

func readLifecycleReceipt(path string) (lifecycleChildReceipt, bool) {
	document, err := os.ReadFile(path)
	if err != nil || len(document) == 0 || len(document) > lifecycleProcessDocumentLimit {
		return lifecycleChildReceipt{}, false
	}
	var receipt lifecycleChildReceipt
	if json.Unmarshal(document, &receipt) != nil || receipt.Kind == "" {
		return lifecycleChildReceipt{}, false
	}
	return receipt, true
}

func (process *lifecycleProcess) kill(t *testing.T, label string) {
	t.Helper()
	if process == nil || process.command == nil || process.exited {
		t.Fatalf("%s was not a live child process", label)
	}
	if err := process.command.Process.Kill(); err != nil {
		t.Fatalf("hard-crash %s: %v", label, err)
	}
	err := process.awaitExit(t, label)
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) {
		t.Fatalf(
			"%s exit = %v; want SIGKILL; stdout=%q stderr=%q",
			label,
			err,
			process.stdout.String(),
			process.stderr.String(),
		)
	}
	status, ok := exitError.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf(
			"%s status = %v; want SIGKILL; stdout=%q stderr=%q",
			label,
			status,
			process.stdout.String(),
			process.stderr.String(),
		)
	}
}

func (process *lifecycleProcess) stop(t *testing.T, label string) {
	t.Helper()
	if process == nil || process.command == nil {
		t.Fatalf("%s child process is unavailable", label)
	}
	if process.exited {
		if process.waitErr != nil {
			t.Fatalf(
				"%s exited early: %v stdout=%q stderr=%q",
				label,
				process.waitErr,
				process.stdout.String(),
				process.stderr.String(),
			)
		}
		return
	}
	if err := process.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("stop %s: %v", label, err)
	}
	if err := process.awaitExit(t, label); err != nil {
		t.Fatalf(
			"%s graceful exit = %v stdout=%q stderr=%q",
			label,
			err,
			process.stdout.String(),
			process.stderr.String(),
		)
	}
}

func (process *lifecycleProcess) waitSuccess(t *testing.T, label string) {
	t.Helper()
	if process.exited {
		if process.waitErr != nil {
			t.Fatalf(
				"%s exit = %v stdout=%q stderr=%q",
				label,
				process.waitErr,
				process.stdout.String(),
				process.stderr.String(),
			)
		}
		return
	}
	if err := process.awaitExit(t, label); err != nil {
		t.Fatalf(
			"%s exit = %v stdout=%q stderr=%q",
			label,
			err,
			process.stdout.String(),
			process.stderr.String(),
		)
	}
}

func (process *lifecycleProcess) awaitExit(t *testing.T, label string) error {
	t.Helper()
	if process.exited {
		return process.waitErr
	}
	select {
	case err := <-process.wait:
		process.exited = true
		process.waitErr = err
		return err
	case <-time.After(lifecycleProcessExitTimeout):
		_ = process.command.Process.Kill()
		process.waitErr = <-process.wait
		process.exited = true
		t.Fatalf("%s did not exit within %s", label, lifecycleProcessExitTimeout)
		return process.waitErr
	}
}

func writeLifecycleReceipt(path string, receipt lifecycleChildReceipt) error {
	if path == "" || !filepath.IsAbs(path) || receipt.Kind == "" {
		return errors.New("lifecycle child receipt is invalid")
	}
	document, err := json.Marshal(receipt)
	if err != nil || len(document) > lifecycleProcessDocumentLimit {
		return errors.New("encode bounded lifecycle child receipt")
	}
	temporary := path + ".partial"
	if err := os.WriteFile(temporary, document, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func startLifecycleAPIProcess(
	t *testing.T,
	fixture *testfixture.Fixture,
) (*lifecycleProcess, testfixture.ProcessAPIReady) {
	t.Helper()
	process, receipt := startLifecycleProcess(t, lifecycleChildRequest{
		Role:         lifecycleChildAPI,
		Dependencies: fixture.ChildProcessDependencies(t),
	})
	if receipt.Kind != lifecycleChildAPI || receipt.API == nil {
		t.Fatalf("child API readiness = %#v", receipt)
	}
	return process, *receipt.API
}

func startLifecycleClientProcess(
	t *testing.T,
	accessPath string,
	requestPath string,
	watch bool,
	hold bool,
) (*lifecycleProcess, lifecycleChildReceipt) {
	t.Helper()
	process, receipt := startLifecycleProcess(t, lifecycleChildRequest{
		Role:        lifecycleChildClient,
		AccessPath:  accessPath,
		RequestPath: requestPath,
		Watch:       watch,
		Hold:        hold,
	})
	if receipt.Kind != lifecycleChildClient {
		t.Fatalf("child client readiness = %#v", receipt)
	}
	return process, receipt
}

func startLifecycleWorkerProcess(
	t *testing.T,
	fixture *testfixture.Fixture,
	configurationPath string,
	provider *lifecycleProvider,
	mode string,
) *lifecycleProcess {
	t.Helper()
	if provider == nil || provider.server == nil || provider.server.Certificate() == nil {
		t.Fatal("worker child requires a controlled provider")
	}
	secretsDirectory := os.Getenv("PM_STACK_SECRETS_DIR")
	if secretsDirectory == "" || !filepath.IsAbs(secretsDirectory) {
		t.Fatal("owned fixture secrets directory is required for worker child")
	}
	process, receipt := startLifecycleProcess(t, lifecycleChildRequest{
		Role:                lifecycleChildWorker,
		Dependencies:        fixture.ChildProcessDependencies(t),
		ConfigurationPath:   configurationPath,
		SecretsDirectory:    secretsDirectory,
		ProviderURL:         provider.server.URL,
		ProviderCertificate: provider.server.Certificate().Raw,
		WorkerMode:          mode,
		WorkerIdentity:      nextLifecycleProcessIdentity("worker"),
	})
	if receipt.Kind != lifecycleChildWorker {
		t.Fatalf("child worker readiness = %#v", receipt)
	}
	return process
}

func openLifecycleParentClient(
	t *testing.T,
	fixture *testfixture.Fixture,
	ready testfixture.ProcessAPIReady,
) testfixture.ProcessClientEndpoint {
	t.Helper()
	accessPath := fixture.WriteProcessClientAccess(t, fixture.Actor, ready)
	endpoint, err := testfixture.OpenProcessClientAccess(accessPath)
	if err != nil {
		t.Fatalf("open parent client for child API: %v", err)
	}
	t.Cleanup(endpoint.Close)
	return endpoint
}

func observeLifecycleRunInChild(
	t *testing.T,
	fixture *testfixture.Fixture,
	ready testfixture.ProcessAPIReady,
	runID string,
) api.RunView {
	t.Helper()
	accessPath := fixture.WriteProcessClientAccess(t, fixture.Actor, ready)
	return observeLifecycleRunWithAccess(t, accessPath, fixture.Scope.WorkspaceID, runID)
}

func observeLifecycleRunInCurrentAPI(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID string,
) api.RunView {
	t.Helper()
	accessPath := fixture.WriteCurrentProcessClientAccess(t, fixture.Actor)
	return observeLifecycleRunWithAccess(t, accessPath, fixture.Scope.WorkspaceID, runID)
}

func observeLifecycleRunWithAccess(
	t *testing.T,
	accessPath string,
	workspaceID string,
	runID string,
) api.RunView {
	t.Helper()
	process, receipt := startLifecycleClientProcess(
		t,
		accessPath,
		"/v1/workspaces/"+url.PathEscape(workspaceID)+"/runs/"+url.PathEscape(runID),
		false,
		false,
	)
	process.waitSuccess(t, "independent observing client")
	if receipt.Status != http.StatusOK {
		t.Fatalf("independent client status = %d body=%q; want 200", receipt.Status, receipt.Body)
	}
	var view api.RunView
	if err := json.Unmarshal([]byte(receipt.Body), &view); err != nil {
		t.Fatalf("decode independent client run view: %v body=%q", err, receipt.Body)
	}
	if view.RunRef != runID {
		t.Fatalf("independent client observed run %q; want %q", view.RunRef, runID)
	}
	return view
}

func awaitLifecycleActiveRequests(
	t *testing.T,
	fixture *testfixture.Fixture,
	expected int64,
) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if fixture.ActiveRequests() == expected {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf(
				"fixture active requests = %d; want %d after client process exit",
				fixture.ActiveRequests(),
				expected,
			)
		}
	}
}

func lifecycleRunEventsPath(fixture *testfixture.Fixture, runID string) string {
	return "/v1/workspaces/" + url.PathEscape(fixture.Scope.WorkspaceID) +
		"/runs/" + url.PathEscape(runID) + "/events"
}

func admitLifecycleRunAt(
	t *testing.T,
	fixture *testfixture.Fixture,
	client *http.Client,
	baseURL string,
	requestKey string,
) api.AdmissionResponse {
	t.Helper()
	invocation := testfixture.Invocation(t, fixture, requestKey)
	document, err := json.Marshal(invocation)
	if err != nil {
		t.Fatalf("encode lifecycle invocation: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		baseURL+"/v1/workspaces/"+url.PathEscape(fixture.Scope.WorkspaceID)+"/invocations",
		bytes.NewReader(document),
	)
	if err != nil {
		t.Fatalf("construct lifecycle invocation: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", requestKey)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("submit lifecycle invocation to child API: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(body) > 64*1024 {
		t.Fatal("read bounded child API admission response")
	}
	var admitted api.AdmissionResponse
	if response.StatusCode == http.StatusAccepted {
		if err := json.Unmarshal(body, &admitted); err != nil {
			t.Fatalf("decode child API admission: %v", err)
		}
	}
	return requireFreshInitialAdmission(t, initialRunResponse{
		status: response.StatusCode, admission: admitted, document: body,
	}, requestKey)
}

func runLifecycleChild(t *testing.T, requestPath string) {
	t.Helper()
	request := readLifecycleChildRequest(t, requestPath)
	ctx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
		syscall.SIGHUP,
	)
	defer stop()
	switch request.Role {
	case lifecycleChildAPI:
		testfixture.ServeProcessAPI(
			t,
			ctx,
			request.Dependencies,
			func(ready testfixture.ProcessAPIReady) error {
				return writeLifecycleReceipt(request.ReadyPath, lifecycleChildReceipt{
					Kind: lifecycleChildAPI,
					API:  &ready,
				})
			},
		)
	case lifecycleChildClient:
		runLifecycleClientChild(t, ctx, request)
	case lifecycleChildWorker:
		runLifecycleWorkerChild(t, ctx, request)
	default:
		t.Fatalf("unsupported lifecycle child role %q", request.Role)
	}
}

func readLifecycleChildRequest(t *testing.T, path string) lifecycleChildRequest {
	t.Helper()
	if path == "" || !filepath.IsAbs(path) {
		t.Fatal("lifecycle child request path must be absolute")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 ||
		info.Size() <= 0 || info.Size() > lifecycleProcessDocumentLimit {
		t.Fatal("lifecycle child request is not a private bounded regular file")
	}
	document, err := os.ReadFile(path)
	if err != nil || len(document) == 0 || len(document) > lifecycleProcessDocumentLimit {
		t.Fatal("read bounded lifecycle child request")
	}
	var request lifecycleChildRequest
	if err := json.Unmarshal(document, &request); err != nil ||
		request.Role == "" || request.ReadyPath == "" {
		t.Fatal("decode lifecycle child request")
	}
	return request
}

func runLifecycleClientChild(
	t *testing.T,
	ctx context.Context,
	request lifecycleChildRequest,
) {
	t.Helper()
	if request.AccessPath == "" || request.RequestPath == "" ||
		!strings.HasPrefix(request.RequestPath, "/") ||
		strings.HasPrefix(request.RequestPath, "//") ||
		len(request.RequestPath) > 4096 {
		t.Fatal("client child request target is invalid")
	}
	endpoint, err := testfixture.OpenProcessClientAccess(request.AccessPath)
	if err != nil {
		t.Fatalf("open client child access: %v", err)
	}
	defer endpoint.Close()
	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		endpoint.BaseURL+request.RequestPath,
		nil,
	)
	if err != nil {
		t.Fatalf("construct client child request: %v", err)
	}
	response, err := endpoint.Client.Do(httpRequest)
	if err != nil {
		t.Fatalf("execute client child request: %v", err)
	}
	defer response.Body.Close()
	var body []byte
	if request.Watch && response.StatusCode == http.StatusOK {
		body = readLifecycleEventFrame(t, response.Body)
	} else {
		body, err = io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
		if err != nil || len(body) > 64*1024 {
			t.Fatal("read bounded client child response")
		}
	}
	if err := writeLifecycleReceipt(request.ReadyPath, lifecycleChildReceipt{
		Kind: lifecycleChildClient, Status: response.StatusCode, Body: string(body),
	}); err != nil {
		t.Fatalf("publish client child readiness: %v", err)
	}
	if request.Hold {
		<-ctx.Done()
	}
}

func readLifecycleEventFrame(t *testing.T, source io.Reader) []byte {
	t.Helper()
	reader := bufio.NewReader(io.LimitReader(source, 64*1024+1))
	frame := make([]byte, 0, 4096)
	total := 0
	for {
		line, err := reader.ReadBytes('\n')
		total += len(line)
		if total > 64*1024 {
			t.Fatal("client child event readiness exceeded its bound")
		}
		frame = append(frame, line...)
		if bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n")) {
			if bytes.HasPrefix(frame, []byte("event: ")) {
				return frame
			}
			frame = frame[:0]
		}
		if err != nil {
			t.Fatalf("read client child event frame: %v", err)
		}
	}
}

type lifecycleProviderRequest struct {
	EffectKey  contract.EffectKey  `json:"effect_key"`
	DispatchID contract.DispatchID `json:"dispatch_id"`
	AttemptID  contract.AttemptID  `json:"attempt_id"`
	MemberIDs  []contract.MemberID `json:"member_ids"`
}

func (request lifecycleProviderRequest) validate() error {
	if err := request.EffectKey.Validate(); err != nil {
		return err
	}
	if err := request.DispatchID.Validate(); err != nil {
		return err
	}
	if err := request.AttemptID.Validate(); err != nil {
		return err
	}
	if len(request.MemberIDs) == 0 || len(request.MemberIDs) > 1024 {
		return errors.New("controlled provider member identities are invalid")
	}
	for _, memberID := range request.MemberIDs {
		if err := memberID.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func lifecycleProviderRequestFor(request app.ExecutionRequest) lifecycleProviderRequest {
	return lifecycleProviderRequest{
		EffectKey:  request.Dispatch.EffectKey,
		DispatchID: request.Dispatch.DispatchID,
		AttemptID:  request.Attempt.AttemptID,
		MemberIDs:  slices.Clone(request.Dispatch.OrderedMemberIDs),
	}
}

func runLifecycleWorkerChild(
	t *testing.T,
	ctx context.Context,
	request lifecycleChildRequest,
) {
	t.Helper()
	if request.ConfigurationPath == "" || !filepath.IsAbs(request.ConfigurationPath) ||
		request.SecretsDirectory == "" || !filepath.IsAbs(request.SecretsDirectory) ||
		request.ProviderURL == "" || len(request.ProviderCertificate) == 0 ||
		request.WorkerIdentity == "" {
		t.Fatal("worker child inputs are incomplete")
	}
	switch request.WorkerMode {
	case lifecycleWorkerCrash, lifecycleWorkerReconcile, lifecycleWorkerComplete:
	default:
		t.Fatalf("unsupported lifecycle worker mode %q", request.WorkerMode)
	}
	providerClient := lifecycleProviderClient(t, request.ProviderCertificate)
	defer providerClient.CloseIdleConnections()
	execute := func(
		callbackContext context.Context,
		executionRequest app.ExecutionRequest,
	) (app.ExecutionObservation, error) {
		if executionRequest.ReconciliationOnly {
			return app.ExecutionObservation{}, errors.New("reconciliation-only work reached execute")
		}
		if request.WorkerMode == lifecycleWorkerReconcile {
			return app.ExecutionObservation{}, errors.New("replacement worker refused a blind physical resend")
		}
		witness, err := callLifecycleProvider(
			callbackContext,
			providerClient,
			request.ProviderURL+"/effect",
			lifecycleProviderRequestFor(executionRequest),
		)
		if err != nil {
			return app.ExecutionObservation{}, err
		}
		if request.WorkerMode == lifecycleWorkerCrash {
			return app.ExecutionObservation{}, errors.New("crash worker returned after provider acceptance")
		}
		if !sameLifecycleProviderRequest(witness, lifecycleProviderRequestFor(executionRequest)) {
			return app.ExecutionObservation{}, errors.New("controlled provider returned a mismatched physical witness")
		}
		return successfulPermitObservation(executionRequest), nil
	}
	reconcile := func(
		callbackContext context.Context,
		executionRequest app.ExecutionRequest,
	) (app.ExecutionObservation, error) {
		if !executionRequest.ReconciliationOnly {
			return app.ExecutionObservation{}, errors.New("reconciliation callback lacked its restricted marker")
		}
		if request.WorkerMode != lifecycleWorkerReconcile {
			return app.ExecutionObservation{}, errors.New("nonreplacement worker reached reconciliation")
		}
		expected := lifecycleProviderRequestFor(executionRequest)
		witness, err := callLifecycleProvider(
			callbackContext,
			providerClient,
			request.ProviderURL+"/reconcile",
			expected,
		)
		if err != nil {
			return app.ExecutionObservation{}, err
		}
		if !sameLifecycleProviderRequest(witness, expected) {
			return app.ExecutionObservation{}, errors.New("provider reconciliation witness did not identify the sealed effect")
		}
		return successfulPermitObservation(executionRequest), nil
	}
	registry, resolver, err := testfixture.NewExecutionBoundary(
		request.Dependencies.ExecutionDependencies(),
		execute,
		reconcile,
	)
	if err != nil {
		t.Fatalf("construct worker child execution boundary: %v", err)
	}
	configurationFile, err := os.Open(request.ConfigurationPath)
	if err != nil {
		t.Fatalf("open worker child configuration: %v", err)
	}
	document, loadErr := config.Load(io.LimitReader(configurationFile, 2<<20))
	closeErr := configurationFile.Close()
	if loadErr != nil || closeErr != nil {
		t.Fatal("load worker child configuration")
	}
	configuration, err := config.Validate(document, config.Prelaunch{})
	if err != nil {
		t.Fatalf("validate worker child configuration: %v", err)
	}
	if configuration.DeploymentID() != request.Dependencies.DeploymentID {
		t.Fatal("worker child configuration changed fixture deployment identity")
	}
	runtime, err := app.OpenWorkerRuntime(ctx, app.WorkerRuntimeConfig{
		Configuration:    configuration,
		SecretsDirectory: request.SecretsDirectory,
		Registry:         registry,
		ResolveOperation: resolver,
		ClientIdentity:   testClientIdentity,
	})
	if err != nil {
		t.Fatalf("open worker child runtime: %v", err)
	}
	defer func() {
		if err := runtime.Close(); err != nil {
			t.Errorf("close worker child runtime: %v", err)
		}
	}()
	if runtime.TemporalTLS == nil || len(runtime.TemporalTLS.Certificates) != 1 {
		t.Fatal("worker child Temporal identity is unavailable")
	}
	certificate := runtime.TemporalTLS.Certificates[0]
	connectionTLS := runtime.TemporalTLS.Clone()
	connectionTLS.Certificates = nil
	temporalConfiguration := configuration.Temporal()
	workerConfiguration := configuration.Worker()
	if workerConfiguration.TaskQueue != contract.ControlTaskQueue {
		t.Fatalf(
			"worker child task queue = %q; want relayed control queue %q",
			workerConfiguration.TaskQueue,
			contract.ControlTaskQueue,
		)
	}
	client, err := pmtemporal.DialWorkerClient(ctx, sdkclient.Options{
		HostPort:          temporalConfiguration.Endpoint,
		Namespace:         temporalConfiguration.Namespace,
		Identity:          request.WorkerIdentity,
		Credentials:       sdkclient.NewMTLSCredentials(certificate),
		ConnectionOptions: sdkclient.ConnectionOptions{TLS: connectionTLS},
	}, workerConfiguration.DialTimeout)
	if err != nil {
		t.Fatalf("dial worker child Temporal client: %v", err)
	}
	defer client.Close()
	observer, err := pmtemporal.NewWorkflowContinuationObserver(
		client,
		temporalConfiguration.Namespace,
		workerConfiguration.ActivityTimeouts.Transport,
	)
	if err != nil {
		t.Fatalf("construct worker child continuation observer: %v", err)
	}
	service, err := runtime.NewService(observer)
	if err != nil {
		t.Fatalf("construct worker child application service: %v", err)
	}
	activityService, err := activities.New(service)
	if err != nil {
		t.Fatalf("construct worker child activity service: %v", err)
	}
	coordinator, err := workflows.NewCoordinator(workerConfiguration.ActivityTimeouts)
	if err != nil {
		t.Fatalf("construct worker child workflow coordinator: %v", err)
	}
	worker := sdkworker.New(client, workerConfiguration.TaskQueue, sdkworker.Options{
		MaxConcurrentActivityExecutionSize:     int(workerConfiguration.MaxConcurrentActivities),
		MaxConcurrentWorkflowTaskExecutionSize: int(workerConfiguration.MaxConcurrentWorkflowTasks),
		WorkerStopTimeout:                      workerConfiguration.DrainTimeout,
	})
	if err := workflows.RegisterWorkflows(
		worker,
		coordinator,
		service.SupportsScheduledAdmission(),
	); err != nil {
		t.Fatalf("register worker child workflows: %v", err)
	}
	if err := activities.RegisterActivities(worker, activityService); err != nil {
		t.Fatalf("register worker child activities: %v", err)
	}
	if err := worker.Start(); err != nil {
		t.Fatalf("start worker child poller: %v", err)
	}
	if err := writeLifecycleReceipt(request.ReadyPath, lifecycleChildReceipt{
		Kind: lifecycleChildWorker,
	}); err != nil {
		worker.Stop()
		t.Fatalf("publish worker child readiness: %v", err)
	}
	<-ctx.Done()
	worker.Stop()
}

func lifecycleProviderClient(t *testing.T, certificateDER []byte) *http.Client {
	t.Helper()
	certificate, err := x509.ParseCertificate(certificateDER)
	if err != nil {
		t.Fatal("parse controlled provider certificate")
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	transport := &http.Transport{
		Proxy: nil,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
		},
		DisableKeepAlives: true,
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("controlled provider redirects are disabled")
		},
	}
}

func callLifecycleProvider(
	ctx context.Context,
	client *http.Client,
	endpoint string,
	request lifecycleProviderRequest,
) (lifecycleProviderRequest, error) {
	if err := request.validate(); err != nil {
		return lifecycleProviderRequest{}, err
	}
	document, err := json.Marshal(request)
	if err != nil {
		return lifecycleProviderRequest{}, err
	}
	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		endpoint,
		bytes.NewReader(document),
	)
	if err != nil {
		return lifecycleProviderRequest{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := client.Do(httpRequest)
	if err != nil {
		return lifecycleProviderRequest{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(body) > 64*1024 {
		return lifecycleProviderRequest{}, errors.New("controlled provider response exceeded its bound")
	}
	if response.StatusCode != http.StatusOK {
		return lifecycleProviderRequest{}, fmt.Errorf("controlled provider status = %d", response.StatusCode)
	}
	var witness lifecycleProviderRequest
	if err := json.Unmarshal(body, &witness); err != nil {
		return lifecycleProviderRequest{}, err
	}
	if err := witness.validate(); err != nil {
		return lifecycleProviderRequest{}, err
	}
	return witness, nil
}

func sameLifecycleProviderRequest(left, right lifecycleProviderRequest) bool {
	return left.EffectKey == right.EffectKey &&
		left.DispatchID == right.DispatchID &&
		left.AttemptID == right.AttemptID &&
		slices.Equal(left.MemberIDs, right.MemberIDs)
}

type lifecycleProvider struct {
	server   *httptest.Server
	accepted chan struct{}
	once     sync.Once
	block    bool

	mu              sync.Mutex
	sends           int
	reconciliations int
	witness         lifecycleProviderRequest
}

func newLifecycleProvider(t *testing.T, block bool) *lifecycleProvider {
	t.Helper()
	provider := &lifecycleProvider{
		accepted: make(chan struct{}),
		block:    block,
	}
	provider.server = httptest.NewTLSServer(http.HandlerFunc(provider.serveHTTP))
	t.Cleanup(provider.server.Close)
	return provider
}

func (provider *lifecycleProvider) serveHTTP(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if request.Method != http.MethodPost ||
		(request.URL.Path != "/effect" && request.URL.Path != "/reconcile") {
		http.NotFound(writer, request)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, 64*1024+1))
	if err != nil || len(body) > 64*1024 {
		http.Error(writer, "invalid bounded provider request", http.StatusBadRequest)
		return
	}
	var observed lifecycleProviderRequest
	if json.Unmarshal(body, &observed) != nil || observed.validate() != nil {
		http.Error(writer, "invalid provider identity", http.StatusBadRequest)
		return
	}
	switch request.URL.Path {
	case "/effect":
		provider.mu.Lock()
		provider.sends++
		sends := provider.sends
		if sends == 1 {
			provider.witness = observed
		}
		witness := provider.witness
		provider.mu.Unlock()
		provider.once.Do(func() { close(provider.accepted) })
		if sends != 1 || !sameLifecycleProviderRequest(witness, observed) {
			http.Error(writer, "duplicate physical effect", http.StatusConflict)
			return
		}
		if provider.block {
			<-request.Context().Done()
			return
		}
		writeLifecycleProviderWitness(writer, witness)
	case "/reconcile":
		provider.mu.Lock()
		provider.reconciliations++
		witness := provider.witness
		provider.mu.Unlock()
		if !sameLifecycleProviderRequest(witness, observed) {
			http.Error(writer, "effect witness not found", http.StatusNotFound)
			return
		}
		writeLifecycleProviderWitness(writer, witness)
	}
}

func writeLifecycleProviderWitness(
	writer http.ResponseWriter,
	witness lifecycleProviderRequest,
) {
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(witness); err != nil {
		return
	}
}

func (provider *lifecycleProvider) awaitAccepted(t *testing.T) {
	t.Helper()
	select {
	case <-provider.accepted:
	case <-time.After(20 * time.Second):
		t.Fatal("controlled provider did not accept the physical effect")
	}
}

func (provider *lifecycleProvider) snapshot() (
	int,
	int,
	lifecycleProviderRequest,
) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.sends, provider.reconciliations, provider.witness
}

func newLifecycleExecutingFixture(t *testing.T) *testfixture.Fixture {
	t.Helper()
	var cursorKey [32]byte
	if _, err := rand.Read(cursorKey[:]); err != nil {
		t.Fatal("generate owned lifecycle event cursor key")
	}
	return testfixture.NewExecuting(
		t,
		func(context.Context, app.ExecutionRequest) (app.ExecutionObservation, error) {
			return app.ExecutionObservation{}, errors.New("parent process must not execute controlled lifecycle effects")
		},
		func(context.Context, app.ExecutionRequest) (app.ExecutionObservation, error) {
			return app.ExecutionObservation{}, errors.New("parent process must not reconcile controlled lifecycle effects")
		},
		func(_ testfixture.ServiceDependencies, configuration *httpapi.ServiceConfig) error {
			configuration.EventBufferSize = 2
			configuration.EventWriteTimeout = time.Second
			configuration.ResolveEventCursorKey = func(ctx context.Context) ([]byte, error) {
				if ctx == nil || ctx.Err() != nil {
					return nil, errors.New("event cursor key request is not live")
				}
				return bytes.Clone(cursorKey[:]), nil
			}
			return nil
		},
	)
}

func lifecycleWorkerConfiguration(
	t *testing.T,
	fixture *testfixture.Fixture,
) string {
	t.Helper()
	artifact := testfixture.NewWorkerConfiguration(t)
	artifact.Verify(t)
	document, err := os.ReadFile(artifact.Path)
	if err != nil || len(document) == 0 || len(document) > 2<<20 {
		t.Fatal("read bounded lifecycle worker configuration")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(document, &object); err != nil {
		t.Fatalf("decode lifecycle worker configuration: %v", err)
	}
	deploymentID, err := json.Marshal(fixture.DeploymentID)
	if err != nil {
		t.Fatal("encode lifecycle deployment identity")
	}
	object["deployment_id"] = deploymentID
	derived, err := json.Marshal(object)
	if err != nil || len(derived) > 2<<20 {
		t.Fatal("encode bounded lifecycle worker configuration")
	}
	path := filepath.Join(t.TempDir(), "worker.json")
	if err := os.WriteFile(path, derived, 0o600); err != nil {
		t.Fatalf("write lifecycle worker configuration: %v", err)
	}
	return path
}

func prepareLifecycleWorkerRun(
	t *testing.T,
	fixture *testfixture.Fixture,
	temporal *permitTemporalHarness,
	admitted api.AdmissionResponse,
) permitWorkerRun {
	t.Helper()
	var commandID string
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT command_id
			FROM pm.outbox_commands
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
			  AND kind = 'start_run' AND state = 'pending'
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, admitted.RunRef).Scan(&commandID)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	relayOwner := nextLifecycleProcessIdentity("relay")
	relay, err := pmtemporal.NewRelay(pmtemporal.RelayConfig{
		Store:        fixture.Pool,
		Client:       temporal.relayClient,
		OwnerID:      relayOwner,
		ClaimLimit:   1,
		ClaimTTL:     30 * time.Second,
		IdleDelay:    10 * time.Millisecond,
		FailureDelay: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("construct production issue20 relay: %v", err)
	}
	if err := relay.RunOnce(ctx); err != nil {
		t.Fatalf("relay accepted issue20 outbox command: %v", err)
	}

	var bound permitWorkerRun
	var firstExecutionRunID string
	if err := store.WithinScope(
		ctx,
		fixture.Pool,
		fixture.Scope,
		fixture.Actor,
		func(ctx context.Context, tx *store.Transaction) error {
			record, err := store.LoadRun(ctx, tx, admitted.RunRef)
			if err != nil {
				return err
			}
			intent, err := store.LoadWorkerCommandIntent(ctx, tx, admitted.RunRef, commandID)
			if err != nil {
				return err
			}
			if len(record.WorkflowBinding.Observations) == 0 ||
				record.WorkflowBinding.WorkflowID != intent.Plan.WorkflowID ||
				record.WorkflowBinding.Generation != intent.Plan.Generation ||
				intent.Plan.Namespace != temporal.namespace ||
				intent.Plan.TaskQueue != contract.ControlTaskQueue ||
				intent.Plan.WorkflowType != contract.RunWorkflowType {
				return errors.New("production relay did not persist its exact observed Temporal execution")
			}
			firstExecutionRunID = record.WorkflowBinding.Observations[0].TemporalRunID
			bound.execution = contract.WorkflowExecution{
				Namespace:  temporal.namespace,
				WorkflowID: record.WorkflowBinding.WorkflowID,
				RunID:      record.WorkflowBinding.CurrentTemporalRunID(),
			}
			bound.reference = contract.WorkflowRunRef{
				Scope:          fixture.Scope,
				RunID:          admitted.RunRef,
				WorkflowID:     record.WorkflowBinding.WorkflowID,
				StartCommandID: commandID,
				Generation:     record.WorkflowBinding.Generation,
				IntentDigest:   intent.Digest,
			}
			if err := bound.execution.Validate(); err != nil {
				return err
			}
			return bound.reference.Validate()
		},
	); err != nil {
		t.Fatalf("load relayed issue20 workflow binding: %v", err)
	}
	description, err := temporal.backend.DescribeWorkflowExecution(
		ctx,
		bound.execution.WorkflowID,
		bound.execution.RunID,
	)
	if err != nil {
		t.Fatalf("describe relayed issue20 workflow: %v", err)
	}
	if actual := description.GetWorkflowExecutionInfo().GetExecution().GetRunId(); actual != bound.execution.RunID {
		t.Fatalf("relayed issue20 Temporal run = %q; want %q", actual, bound.execution.RunID)
	}
	testfixture.CleanupWorkflowChain(
		t,
		temporal.backend,
		temporal.namespace,
		bound.execution.WorkflowID,
		firstExecutionRunID,
	)
	parentBoundary := newPermitWorkerService(t, fixture, temporal)
	bound.plan = sealPermitWorkerPlan(
		t,
		fixture,
		parentBoundary,
		fixture.Scope,
		fixture.Actor,
		bound.execution,
		bound.reference,
	)
	return bound
}

func awaitLifecycleWorkflow(
	t *testing.T,
	temporal *permitTemporalHarness,
	run permitWorkerRun,
) contract.WorkflowProjectionResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var projection contract.WorkflowProjectionResult
	if err := temporal.backend.GetWorkflow(
		ctx,
		run.execution.WorkflowID,
		run.execution.RunID,
	).Get(ctx, &projection); err != nil {
		t.Fatalf("await recovered lifecycle workflow: %v", err)
	}
	if projection.RunID != run.reference.RunID ||
		projection.State != contract.RunStateSucceeded ||
		projection.Revision == 0 {
		t.Fatalf("recovered lifecycle projection = %#v; want same succeeded run", projection)
	}
	return projection
}

func assertLifecycleRecoveredEffect(
	t *testing.T,
	fixture *testfixture.Fixture,
	run permitWorkerRun,
	provider *lifecycleProvider,
	projection contract.WorkflowProjectionResult,
) {
	t.Helper()
	sends, reconciliations, witness := provider.snapshot()
	expectedWitness := lifecycleProviderRequest{
		EffectKey:  run.plan.Dispatch.EffectKey,
		DispatchID: run.plan.Dispatch.DispatchID,
		AttemptID:  run.plan.Dispatch.AttemptID,
		MemberIDs:  slices.Clone(run.plan.Dispatch.OrderedMemberIDs),
	}
	if sends != 1 || !sameLifecycleProviderRequest(witness, expectedWitness) {
		t.Fatalf(
			"controlled physical effects = sends %d witness %#v; want one exact sealed effect %#v",
			sends,
			witness,
			expectedWitness,
		)
	}
	if provider.block && reconciliations == 0 {
		t.Fatal("replacement worker did not reconcile the accepted physical effect")
	}
	if !provider.block && reconciliations != 0 {
		t.Fatalf("fresh successful worker performed %d reconciliations; want zero", reconciliations)
	}
	if projection.RunID != run.reference.RunID ||
		run.execution.WorkflowID != run.reference.WorkflowID ||
		run.reference.RunID == "" ||
		run.reference.StartCommandID == "" {
		t.Fatalf(
			"recovered workflow identity = execution %#v reference %#v projection %#v",
			run.execution,
			run.reference,
			projection,
		)
	}
	outbox := loadLifecycleStartOutbox(t, fixture, run.reference.RunID)
	if outbox.count != 1 ||
		outbox.commandID != run.reference.StartCommandID ||
		outbox.state != "acknowledged" {
		t.Fatalf(
			"recovered start outbox = %#v; want one acknowledged original command %q",
			outbox,
			run.reference.StartCommandID,
		)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.WithinScope(
		ctx,
		fixture.Pool,
		fixture.Scope,
		fixture.Actor,
		func(ctx context.Context, tx *store.Transaction) error {
			task := store.TaskRef{RunID: run.reference.RunID, TaskID: run.plan.TaskID}
			dispatch, err := store.LoadWorkerDispatch(ctx, tx, task)
			if err != nil {
				return err
			}
			if dispatch.Intent.EffectKey != run.plan.Dispatch.EffectKey ||
				dispatch.Intent.DispatchID != run.plan.Dispatch.DispatchID ||
				dispatch.Intent.AttemptID != run.plan.Dispatch.AttemptID ||
				!slices.Equal(dispatch.Intent.OrderedMemberIDs, run.plan.Dispatch.OrderedMemberIDs) ||
				len(dispatch.Attempts) != 1 ||
				dispatch.Attempts[0].AttemptID != run.plan.Dispatch.AttemptID ||
				dispatch.Attempts[0].DispatchID != run.plan.Dispatch.DispatchID {
				return fmt.Errorf("durable dispatch lost its sealed identity: %#v", dispatch)
			}
			outcomes, err := store.ListMemberOutcomes(
				ctx,
				tx,
				task,
				run.plan.Dispatch.DispatchID,
			)
			if err != nil {
				return err
			}
			if len(outcomes) != len(run.plan.Dispatch.OrderedMemberIDs) {
				return fmt.Errorf(
					"durable member outcomes = %d; want %d",
					len(outcomes),
					len(run.plan.Dispatch.OrderedMemberIDs),
				)
			}
			for index, outcome := range outcomes {
				if outcome.MemberID != run.plan.Dispatch.OrderedMemberIDs[index] ||
					outcome.EffectKey != run.plan.Dispatch.EffectKey ||
					outcome.DispatchID != run.plan.Dispatch.DispatchID ||
					outcome.State != contract.OutcomeSucceeded {
					return fmt.Errorf("durable member outcome %d lost exact effect truth", index)
				}
			}
			effects, err := store.ListWorkerRunEffects(ctx, tx, run.reference.RunID)
			if err != nil {
				return err
			}
			if len(effects) != 1 ||
				effects[0].EffectKey != run.plan.Dispatch.EffectKey ||
				effects[0].State != contract.OutcomeSucceeded ||
				len(effects[0].Attempts) != 1 ||
				effects[0].Attempts[0].AttemptID != run.plan.Dispatch.AttemptID {
				return fmt.Errorf("durable logical effects = %#v; want one exact succeeded effect", effects)
			}
			view, err := store.LoadRunView(ctx, tx, run.reference.RunID)
			if err != nil {
				return err
			}
			if view.Record.Spec.RunID != run.reference.RunID ||
				view.Record.WorkflowBinding.WorkflowID != run.execution.WorkflowID ||
				view.Record.WorkflowBinding.CurrentTemporalRunID() != run.execution.RunID ||
				view.Record.WorkflowBinding.Generation != run.reference.Generation ||
				view.Record.State != contract.RunStateSucceeded ||
				view.Record.CancelRequested ||
				view.Record.EffectSummary.Succeeded != uint64(len(outcomes)) ||
				view.Record.EffectSummary.Unknown != 0 {
				return fmt.Errorf("durable recovered run view = %#v", view)
			}
			return nil
		},
	); err != nil {
		t.Fatalf("assert recovered lifecycle effect: %v", err)
	}
}

type lifecycleOutboxIdentity struct {
	count     int64
	commandID string
	state     string
}

func loadLifecycleStartOutbox(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID string,
) lifecycleOutboxIdentity {
	t.Helper()
	var identity lifecycleOutboxIdentity
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*), COALESCE(min(command_id), ''), COALESCE(min(state::text), '')
			FROM pm.outbox_commands
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND kind = 'start_run'
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID).Scan(
			&identity.count,
			&identity.commandID,
			&identity.state,
		)
	})
	if identity.count != 1 || identity.commandID == "" || identity.state == "" {
		t.Fatalf("accepted run start outbox = %#v; want one durable command", identity)
	}
	return identity
}

func loadLifecycleRunRecord(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID string,
) contract.RunRecord {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var record contract.RunRecord
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		view, err := store.LoadRunView(ctx, tx, runID)
		if err != nil {
			return err
		}
		record = view.Record
		return nil
	}); err != nil {
		t.Fatalf("load accepted lifecycle run %q: %v", runID, err)
	}
	return record
}

func submitInitialRun(
	t *testing.T,
	fixture *testfixture.Fixture,
	client *http.Client,
	workspaceID, requestKey string,
) initialRunResponse {
	t.Helper()
	return submitInitialInvocation(t, fixture, client, workspaceID, testfixture.Invocation(t, fixture, requestKey))
}

func submitInitialInvocation(
	t *testing.T,
	fixture *testfixture.Fixture,
	client *http.Client,
	workspaceID string,
	request api.InvocationRequest,
) initialRunResponse {
	t.Helper()
	response, err := executeInitialInvocation(fixture, client, workspaceID, request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func executeInitialInvocation(
	fixture *testfixture.Fixture,
	client *http.Client,
	workspaceID string,
	request api.InvocationRequest,
) (initialRunResponse, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return initialRunResponse{}, fmt.Errorf("encode typed initial invocation: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fixture.BaseURL+"/v1/workspaces/"+url.PathEscape(workspaceID)+"/invocations",
		bytes.NewReader(encoded),
	)
	if err != nil {
		return initialRunResponse{}, fmt.Errorf("construct initial invocation: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Idempotency-Key", request.Protocol.RequestKey)
	httpResponse, err := client.Do(httpRequest)
	if err != nil {
		return initialRunResponse{}, fmt.Errorf("send initial invocation: %w", err)
	}
	defer httpResponse.Body.Close()
	document, err := io.ReadAll(io.LimitReader(httpResponse.Body, 64*1024+1))
	if err != nil || len(document) > 64*1024 {
		return initialRunResponse{}, errors.New("initial invocation response exceeded its bounded readable envelope")
	}
	result := initialRunResponse{status: httpResponse.StatusCode, document: document}
	if httpResponse.StatusCode == http.StatusAccepted {
		if err := json.Unmarshal(document, &result.admission); err != nil {
			return initialRunResponse{}, fmt.Errorf("decode admission response: %w", err)
		}
		return result, nil
	}
	if err := json.Unmarshal(document, &result.problem); err != nil {
		return initialRunResponse{}, errors.New("initial invocation returned a non-JSON problem envelope")
	}
	result.problemCode = result.problem.Code
	return result, nil
}

func exerciseInitialUnavailableHandler(t *testing.T) {
	t.Helper()
	t.Run("unavailable_handler", func(t *testing.T) {
		t.Run("C01_missing_handler", func(t *testing.T) {
			fixture := testfixture.New(t)
			response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "missing-handler")
			requireInitialProblem(t, response, http.StatusServiceUnavailable, "capability_unavailable")
		})

		t.Run("C02_foundation_mismatch", func(t *testing.T) {
			fixture := newConfiguredInitialFixture(
				t,
				initialSelection("fixture.read", "/fixture", "fixture-schema-v1"),
				initialRegistration("2.0.0"),
				initialRunPolicy(),
			)
			response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "foundation-mismatch")
			requireInitialProblem(t, response, http.StatusConflict, "foundation_incompatible")
		})

		t.Run("C03_registered_control", func(t *testing.T) {
			fixture := newInitialRunFixture(t)
			response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "registered-control")
			requireFreshInitialAdmission(t, response, "registered-control")
			var snapshot store.RunInputSnapshot
			if err := store.WithinScope(context.Background(), fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
				var err error
				snapshot, err = store.LoadRunInputSnapshot(ctx, tx, response.admission.RunRef)
				return err
			}); err != nil {
				t.Fatalf("load committed admission input: %v", err)
			}
			if snapshot.Lane != contract.LaneDirectRead {
				t.Fatalf("committed admission lane = %q; want requested %q", snapshot.Lane, contract.LaneDirectRead)
			}
		})
	})
}

func exerciseInitialAuthority(t *testing.T) {
	t.Helper()
	t.Run("authority", func(t *testing.T) {
		t.Run("C01_wrong_workspace", func(t *testing.T) {
			fixture := testfixture.New(t)
			foreignClient := fixture.ClientFor(t, fixture.ForeignActor)
			response := submitInitialRun(t, fixture, foreignClient, fixture.Scope.WorkspaceID, "wrong-workspace")
			requireInitialProblem(t, response, http.StatusNotFound, "resource_not_found")
		})

		t.Run("C02_denied_connection", func(t *testing.T) {
			fixture := testfixture.New(t)
			request := testfixture.Invocation(t, fixture, "denied-connection")
			request.ConnectionRef = fixture.ForeignConnection.ConnectionID
			response := submitInitialInvocation(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, request)
			requireInitialProblem(t, response, http.StatusNotFound, "resource_not_found")
		})

		t.Run("C03_expired_grant", func(t *testing.T) {
			fixture := testfixture.New(t)
			fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `
					UPDATE pm.memberships
					SET expires_at = clock_timestamp() - interval '1 second'
					WHERE identity_id = $1
				`, fixture.Actor.RequestedActor.PrincipalID)
				return err
			})
			response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "expired-grant")
			requireInitialProblem(t, response, http.StatusForbidden, "grant_unavailable")
		})

		t.Run("C04_revoked_grant", func(t *testing.T) {
			fixture := testfixture.New(t)
			fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `
					UPDATE pm.memberships
					SET revoked_at = clock_timestamp(), policy_epoch = policy_epoch + 1,
					    control_revision = control_revision + 1
					WHERE identity_id = $1
				`, fixture.Actor.RequestedActor.PrincipalID)
				return err
			})
			response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "revoked-grant")
			requireInitialProblem(t, response, http.StatusForbidden, "grant_unavailable")
		})

		t.Run("C05_stale_schema", func(t *testing.T) {
			fixture := newConfiguredInitialFixture(
				t,
				initialSelection("fixture.read", "/fixture", "fixture-schema-v2"),
				initialRegistration("1.0.0"),
				initialRunPolicy(),
			)
			response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "stale-schema")
			requireInitialProblem(t, response, http.StatusConflict, "specification_stale")
		})

		t.Run("C06_stale_descriptor", func(t *testing.T) {
			fixture := newConfiguredInitialFixture(
				t,
				initialSelection("fixture.read", "/fixture-v2", "fixture-schema-v1"),
				initialRegistration("1.0.0"),
				initialRunPolicy(),
			)
			response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "stale-descriptor")
			requireInitialProblem(t, response, http.StatusConflict, "specification_stale")
		})
	})
}

func exerciseInitialPendingBudget(t *testing.T) {
	t.Helper()
	t.Run("pending_budget", func(t *testing.T) {
		t.Run("C01_below_count", func(t *testing.T) {
			fixture := newInitialPolicyFixture(t, initialRunPolicy())
			first := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "pending-below-first")
			requireFreshInitialAdmission(t, first, "pending-below-first")
			finalSlot := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "pending-below-final-slot")
			requireFreshInitialAdmission(t, finalSlot, "pending-below-final-slot")
			if countInitialReservations(t, fixture, fixture.Scope) != 2 {
				t.Fatal("the final available workspace slot did not commit exactly one reservation")
			}
		})

		t.Run("C02_equal_count", func(t *testing.T) {
			t.Run("workspace", func(t *testing.T) {
				fixture := newInitialPolicyFixture(t, initialRunPolicy())
				for _, key := range []string{"pending-equal-one", "pending-equal-two"} {
					requireFreshInitialAdmission(t, submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, key), key)
				}
				response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "pending-equal-refused")
				requireInitialProblem(t, response, http.StatusTooManyRequests, "workspace_admission_count_limit")
				if countInitialReservations(t, fixture, fixture.Scope) != 2 {
					t.Fatal("workspace refusal discarded an accepted reservation")
				}
			})

			t.Run("deployment", func(t *testing.T) {
				policy := initialRunPolicy()
				policy.Pending.GlobalPending = 2
				fixture := newInitialPolicyFixture(t, policy)
				requireFreshInitialAdmission(
					t,
					submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "pending-global-primary"),
					"pending-global-primary",
				)
				foreignRequest := testfixture.Invocation(t, fixture, "pending-global-foreign")
				foreignRequest.ConnectionRef = fixture.ForeignConnection.ConnectionID
				foreignClient := fixture.ClientFor(t, fixture.ForeignActor)
				requireFreshInitialAdmission(
					t,
					submitInitialInvocation(t, fixture, foreignClient, fixture.ForeignScope.WorkspaceID, foreignRequest),
					"pending-global-foreign",
				)
				response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "pending-global-refused")
				requireInitialProblem(t, response, http.StatusServiceUnavailable, "deployment_admission_overload")
				if countInitialReservations(t, fixture, fixture.Scope) != 1 ||
					countInitialReservations(t, fixture, fixture.ForeignScope) != 1 {
					t.Fatal("deployment refusal discarded an accepted reservation")
				}
			})
		})

		t.Run("C03_above_count", func(t *testing.T) {
			higher := initialRunPolicy()
			higher.Pending.GlobalPending = 3
			higher.Pending.WorkspacePending = 3
			fixture := newInitialPolicyFixture(t, higher)
			for _, key := range []string{"pending-above-one", "pending-above-two", "pending-above-three"} {
				requireFreshInitialAdmission(t, submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, key), key)
			}
			lower := initialRunPolicy()
			lower.Pending.GlobalPending = 2
			lower.Pending.WorkspacePending = 2
			fixture.Restart(t, initialAdmissionConfigurator(
				initialSelection("fixture.read", "/fixture", "fixture-schema-v1"),
				initialRegistration("1.0.0"),
				lower,
			))
			response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "pending-above-refused")
			requireInitialProblem(t, response, http.StatusTooManyRequests, "workspace_admission_count_limit")
			if countInitialReservations(t, fixture, fixture.Scope) != 3 {
				t.Fatal("above-limit refusal altered pre-existing accepted reservations")
			}
		})

		t.Run("C04_age_limit", func(t *testing.T) {
			for _, test := range []struct {
				name       string
				ageSeconds int
				wantStatus int
			}{
				{name: "just_below", ageSeconds: 29, wantStatus: http.StatusAccepted},
				{name: "equal", ageSeconds: 30, wantStatus: http.StatusTooManyRequests},
				{name: "above", ageSeconds: 31, wantStatus: http.StatusTooManyRequests},
			} {
				t.Run(test.name, func(t *testing.T) {
					fixture := newInitialPolicyFixture(t, initialRunPolicy())
					first := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "pending-age-"+test.name+"-first")
					requireFreshInitialAdmission(t, first, "pending-age-"+test.name+"-first")
					setInitialReservationAge(t, fixture, first.admission.RunRef, test.ageSeconds)
					response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "pending-age-"+test.name+"-next")
					if test.wantStatus == http.StatusAccepted {
						requireFreshInitialAdmission(t, response, "pending-age-"+test.name+"-next")
						return
					}
					requireInitialProblem(t, response, http.StatusTooManyRequests, "workspace_admission_age_limit")
					if countInitialReservations(t, fixture, fixture.Scope) != 1 {
						t.Fatal("age refusal discarded or replaced the accepted reservation")
					}
				})
			}
		})

		t.Run("C05_concurrent_last_slot", func(t *testing.T) {
			policy := initialRunPolicy()
			policy.Pending.GlobalPending = 1
			policy.Pending.WorkspacePending = 1
			fixture := newInitialPolicyFixture(t, policy)
			requests := map[string]api.InvocationRequest{
				"pending-race-left":  testfixture.Invocation(t, fixture, "pending-race-left"),
				"pending-race-right": testfixture.Invocation(t, fixture, "pending-race-right"),
			}
			type outcome struct {
				key      string
				response initialRunResponse
				err      error
			}
			outcomes := make(chan outcome, 2)
			for key, request := range requests {
				key, request := key, request
				go func() {
					response, err := executeInitialInvocation(fixture, fixture.Client, fixture.Scope.WorkspaceID, request)
					outcomes <- outcome{key: key, response: response, err: err}
				}()
			}
			statuses := make([]int, 0, 2)
			var accepted outcome
			for range 2 {
				result := <-outcomes
				if result.err != nil {
					t.Fatal(result.err)
				}
				statuses = append(statuses, result.response.status)
				if result.response.status == http.StatusAccepted {
					accepted = result
				} else {
					requireInitialProblem(t, result.response, http.StatusTooManyRequests, "workspace_admission_count_limit")
				}
			}
			slices.Sort(statuses)
			if !slices.Equal(statuses, []int{http.StatusAccepted, http.StatusTooManyRequests}) {
				t.Fatalf("last-slot statuses = %v; want one 202 and one 429", statuses)
			}
			replayed := submitInitialInvocation(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, requests[accepted.key])
			if replayed.status != http.StatusAccepted || !replayed.admission.Replay ||
				replayed.admission.RunRef != accepted.response.admission.RunRef {
				t.Fatalf("last-slot exact replay = %#v; want original accepted run", replayed)
			}
			if countInitialReservations(t, fixture, fixture.Scope) != 1 {
				t.Fatal("last-slot replay reserved a second slot")
			}
		})

		t.Run("C06_profiles_and_overrides", func(t *testing.T) {
			for _, test := range []struct {
				name      string
				admission string
				want      store.AdmissionLimits
			}{
				{
					name:      "development",
					admission: `{"profile":"development"}`,
					want: store.AdmissionLimits{
						GlobalPending: 32, WorkspacePending: 8, WorkspaceMaxPendingAge: 2 * time.Minute,
					},
				},
				{
					name:      "standard",
					admission: `{"profile":"standard"}`,
					want: store.AdmissionLimits{
						GlobalPending: 256, WorkspacePending: 32, WorkspaceMaxPendingAge: 10 * time.Minute,
					},
				},
				{
					name: "explicit overrides",
					admission: `{"profile":"development","overrides":{` +
						`"global_pending":5,"workspace_pending":2,"workspace_max_pending_age":"45s"}}`,
					want: store.AdmissionLimits{
						GlobalPending: 5, WorkspacePending: 2, WorkspaceMaxPendingAge: 45 * time.Second,
					},
				},
			} {
				t.Run(test.name, func(t *testing.T) {
					document, err := config.Load(bytes.NewBufferString(initialConfiguration(test.admission)))
					if err != nil {
						t.Fatal(err)
					}
					configuration, err := config.Validate(document, config.Prelaunch{})
					if err != nil {
						t.Fatal(err)
					}
					policy, err := app.RunPolicyFromConfiguration(configuration)
					if err != nil {
						t.Fatal(err)
					}
					if policy.Pending != test.want || policy.RunLifetime != time.Hour ||
						policy.InputMaxBytes != 1<<20 || policy.WorkspaceActiveRuns != 4 ||
						policy.WorkflowPayloadBytes != 1<<16 || policy.WorkflowFanout != 32 {
						t.Fatalf("resolved policy = %#v; want pending %#v and explicit finite limits", policy, test.want)
					}
				})
			}
		})

		t.Run("C07_prestart_projection_and_start_transition", func(t *testing.T) {
			fixture := newInitialRunFixture(t)
			accepted := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "prestart-projection")
			requireFreshInitialAdmission(t, accepted, "prestart-projection")
			before := getRunViewForTest(t, fixture.Client, runPathForTest(fixture, accepted.admission.RunRef))
			if contract.RunState(before.State) != contract.RunStateAccepted {
				t.Fatalf("pre-start run state = %q; want accepted", before.State)
			}
			pendingBefore, _, _ := pendingReservationForTest(t, fixture, accepted.admission.RunRef)
			if pendingBefore != 1 {
				t.Fatalf("pre-start outstanding reservation = %d; want 1", pendingBefore)
			}
			transitionInitialRun(t, fixture, accepted.admission.RunRef, contract.RunStateQueued, "temporal_submission_acknowledged")
			prestart := getRunViewForTest(t, fixture.Client, runPathForTest(fixture, accepted.admission.RunRef))
			pendingPrestart, releasedPrestart, _ := pendingReservationForTest(t, fixture, accepted.admission.RunRef)
			if contract.RunState(prestart.State) != contract.RunStateQueued || pendingPrestart != 1 || releasedPrestart {
				t.Fatalf("pre-start acknowledgement released reservation: view=%#v pending=%d released=%v", prestart, pendingPrestart, releasedPrestart)
			}
			transitionInitialRun(t, fixture, accepted.admission.RunRef, contract.RunStateRunning, "worker_execution_started")
			after := getRunViewForTest(t, fixture.Client, runPathForTest(fixture, accepted.admission.RunRef))
			if contract.RunState(after.State) != contract.RunStateRunning || after.Revision <= prestart.Revision {
				t.Fatalf("started run projection = %#v; want later running revision", after)
			}
			pendingAfter, released, reason := pendingReservationForTest(t, fixture, accepted.admission.RunRef)
			if pendingAfter != 0 || !released || reason != "execution_started" {
				t.Fatalf("started reservation = count %d released %v reason %q", pendingAfter, released, reason)
			}
		})
	})
}

func exerciseInitialRetryReceipts(t *testing.T) {
	t.Helper()
	t.Run("retry_receipts", func(t *testing.T) {
		t.Run("C01_unresolved_beyond_retention", func(t *testing.T) {
			fixture := newInitialRunFixture(t)
			first := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "receipt-unresolved")
			requireFreshInitialAdmission(t, first, "receipt-unresolved")
			ageAdmissionReceipt(t, fixture, first.admission.RunRef, false)
			replayed := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "receipt-unresolved")
			if replayed.status != http.StatusAccepted || !replayed.admission.Replay ||
				replayed.admission.RunRef != first.admission.RunRef {
				t.Fatalf("unresolved aged replay = %#v; want original accepted run", replayed)
			}
			if compactedAdmissionReceipt(t, fixture, first.admission.RunRef) {
				t.Fatal("unresolved admission response was compacted")
			}
		})

		t.Run("C02_resolved_response_expired", func(t *testing.T) {
			fixture := newInitialRunFixture(t)
			first := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "receipt-expired")
			requireFreshInitialAdmission(t, first, "receipt-expired")
			transitionInitialRun(t, fixture, first.admission.RunRef, contract.RunStateFailed, "fixture_terminal_failure")
			ageAdmissionReceipt(t, fixture, first.admission.RunRef, true)
			expired := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "receipt-expired")
			problem := requireInitialProblem(t, expired, http.StatusGone, "retry_receipt_expired")
			if problem.OutcomeReference == nil || problem.OutcomeReference.Ref != first.admission.RunRef ||
				problem.OutcomeReference.Kind != api.SafeOutcomeReferenceKindRun {
				t.Fatalf("expired receipt outcome = %#v; want authorized run reference", problem.OutcomeReference)
			}
			if !compactedAdmissionReceipt(t, fixture, first.admission.RunRef) {
				t.Fatal("410 was emitted before admission compaction committed")
			}
		})

		t.Run("C03_changed_intent_after_compaction", func(t *testing.T) {
			fixture := newInitialRunFixture(t)
			first := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "receipt-changed")
			requireFreshInitialAdmission(t, first, "receipt-changed")
			transitionInitialRun(t, fixture, first.admission.RunRef, contract.RunStateFailed, "fixture_terminal_failure")
			ageAdmissionReceipt(t, fixture, first.admission.RunRef, true)
			requireInitialProblem(
				t,
				submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "receipt-changed"),
				http.StatusGone,
				"retry_receipt_expired",
			)
			changed := testfixture.Invocation(t, fixture, "receipt-changed")
			changed.Input = contract.NewObjectValue(map[string]contract.PresentValue{
				"changed": contract.NewStringValue("intent"),
			})
			response := submitInitialInvocation(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, changed)
			requireInitialProblem(t, response, http.StatusConflict, "idempotency_conflict")
		})

		t.Run("C04_compaction_races_recovery", func(t *testing.T) {
			fixture := newInitialRunFixture(t)
			first := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "receipt-race")
			requireFreshInitialAdmission(t, first, "receipt-race")
			transitionInitialRun(t, fixture, first.admission.RunRef, contract.RunStateFailed, "fixture_terminal_failure")
			ageAdmissionReceipt(t, fixture, first.admission.RunRef, true)
			type outcome struct {
				response initialRunResponse
				err      error
			}
			outcomes := make(chan outcome, 2)
			request := testfixture.Invocation(t, fixture, "receipt-race")
			for range 2 {
				go func() {
					response, err := executeInitialInvocation(fixture, fixture.Client, fixture.Scope.WorkspaceID, request)
					outcomes <- outcome{response: response, err: err}
				}()
			}
			for range 2 {
				result := <-outcomes
				if result.err != nil {
					t.Fatal(result.err)
				}
				requireInitialProblem(t, result.response, http.StatusGone, "retry_receipt_expired")
			}
			if !compactedAdmissionReceipt(t, fixture, first.admission.RunRef) {
				t.Fatal("racing recovery left the resolved response uncompact")
			}
			assertSingleAdmissionRun(t, fixture, first.admission.RunRef)
		})
	})
}

func newInitialPolicyFixture(t *testing.T, policy store.RunBudgetPolicy) *testfixture.Fixture {
	t.Helper()
	return newConfiguredInitialFixture(
		t,
		initialSelection("fixture.read", "/fixture", "fixture-schema-v1"),
		initialRegistration("1.0.0"),
		policy,
	)
}

func newConfiguredInitialFixture(
	t *testing.T,
	selection app.OperationSelection,
	registration app.HandlerRegistration,
	policy store.RunBudgetPolicy,
) *testfixture.Fixture {
	t.Helper()
	return testfixture.New(t, initialAdmissionConfigurator(selection, registration, policy))
}

func initialAdmissionConfigurator(
	selection app.OperationSelection,
	registration app.HandlerRegistration,
	policy store.RunBudgetPolicy,
) testfixture.ConfigureService {
	return func(dependencies testfixture.ServiceDependencies, configuration *httpapi.ServiceConfig) error {
		registry := &app.HandlerRegistry{}
		if err := registry.RegisterHTTP(registration); err != nil {
			return err
		}
		admission, err := app.NewAdmissionService(app.AdmissionServiceConfig{
			Pool:         dependencies.Pool,
			DeploymentID: dependencies.DeploymentID,
			Authorize:    configuration.Auth.AuthorizeResource,
			Registry:     registry,
			ResolveOperation: func(
				_ context.Context,
				scope contract.Scope,
				connectionID string,
				operationID string,
			) (app.OperationSelection, error) {
				primary := scope.Equal(dependencies.Scope) && connectionID == dependencies.Connection.ConnectionID
				foreign := scope.Equal(dependencies.ForeignScope) && connectionID == dependencies.ForeignConnection.ConnectionID
				if operationID != dependencies.OperationID || !primary && !foreign {
					return app.OperationSelection{}, errors.New("controlled operation is unavailable")
				}
				return selection, nil
			},
			Policy: policy,
			Random: rand.Reader,
		})
		if err != nil {
			return err
		}
		configuration.Admission = admission
		return nil
	}
}

func initialSelection(operationID, path, schemaRef string) app.OperationSelection {
	foundation := contract.FoundationRef{ID: "FND-6", Version: "1.0.0"}
	sourceRefs := []contract.SourceRef{
		{SourceID: "fixture-source", Pointer: "#/operations/fixture"},
	}
	return app.OperationSelection{
		Operation: contract.Operation{
			SchemaVersion:   contract.OperationSchemaVersion,
			OperationID:     operationID,
			SourceRefs:      sourceRefs,
			Transport:       contract.Transport{Kind: contract.TransportKindHTTP, Method: http.MethodGet, Path: path},
			Inputs:          []contract.FieldBinding{},
			RequestVariants: []contract.RequestVariant{},
			Responses: []contract.Response{
				{
					Status:     "200",
					NoContent:  false,
					MediaType:  "application/json",
					Schema:     &contract.Schema{Type: contract.SchemaTypeString},
					SourceRefs: sourceRefs,
				},
			},
			CapabilityRequirements: []contract.CapabilityRequirement{
				{Capability: "http.execute", Foundation: foundation},
			},
			LaneEligibility: operationLaneEligibilityForTest(contract.LaneDirectRead),
		},
		SchemaRefs: []string{schemaRef},
	}
}

func initialRegistration(version string) app.HandlerRegistration {
	foundation := contract.FoundationRef{ID: "FND-6", Version: version}
	return app.HandlerRegistration{
		Lanes: []contract.Lane{contract.LaneDirectRead},
		Foundations: []contract.FoundationContract{
			{ID: foundation.ID, Version: foundation.Version, Dependencies: []contract.FoundationRef{}},
		},
		Capabilities: []contract.CapabilityRequirement{
			{Capability: "http.execute", Foundation: foundation},
		},
		Execute: func(context.Context, app.ExecutionRequest) (app.ExecutionObservation, error) {
			return app.ExecutionObservation{}, errors.New("admission must not execute the controlled handler callback")
		},
	}
}

func initialRunPolicy() store.RunBudgetPolicy {
	return store.RunBudgetPolicy{
		Pending: store.AdmissionLimits{
			GlobalPending:          3,
			WorkspacePending:       2,
			WorkspaceMaxPendingAge: 30 * time.Second,
		},
		RunLifetime:          time.Hour,
		InputMaxBytes:        1 << 20,
		WorkspaceActiveRuns:  4,
		WorkflowPayloadBytes: 1 << 16,
		WorkflowFanout:       32,
	}
}

func initialConfiguration(admission string) string {
	return `{
		"schema_version":"1",
		"deployment_id":"admission-policy-fixture",
		"public_origin":"https://pm.example.invalid",
		"server":{"listen":"127.0.0.1:8080","drain_timeout":"2s","trusted_proxies":["127.0.0.1/32"]},
		"postgres":{"ownership":"managed","database":"pm_app","endpoint_ref":"secret://fixture/postgres"},
		"temporal":{"ownership":"managed","endpoint":"temporal.example.invalid:7233","namespace":"fixture","client_identity_ref":"secret://fixture/temporal","retention":"720h","task_queues":["pm-control"]},
		"artifacts":{"ownership":"managed","driver":"s3","endpoint":"https://objects.example.invalid","bucket":"fixture-objects","credential_ref":"secret://fixture/objects","attempt_key_policy":"unique_immutable"},
		"identity":{"issuer":"https://identity.example.invalid","client_id":"pm-server","client_secret_ref":"secret://fixture/oidc","step_up_maximum_age":"2m"},
		"encryption":{"wrapping_key_ref":"secret://fixture/wrapping"},
		"admission":` + admission + `,
		"limits":{"workspace_active_runs":4,"workflow_payload_bytes":65536,"workflow_fanout":32,"run_lifetime":"1h","event_buffer":128,"query_children":1,"query_child_memory_bytes":4294967296,"query_child_deadline":"5m","request_bytes":1048576,"metadata_deadline":"5s"}
	}`
}

func requireInitialProblem(t *testing.T, response initialRunResponse, status int, code string) api.Problem {
	t.Helper()
	if response.status != status || response.problem.Status != status || response.problemCode != code {
		t.Fatalf("initial problem = HTTP %d %#v; want %d/%s", response.status, response.problem, status, code)
	}
	return response.problem
}

func requireFreshInitialAdmission(t *testing.T, response initialRunResponse, requestKey string) api.AdmissionResponse {
	t.Helper()
	if response.status != http.StatusAccepted || response.admission.RunRef == "" ||
		response.admission.RequestKey != requestKey || response.admission.Replay ||
		response.admission.State != api.AdmissionResponseStateAccepted || response.admission.IntentDigest == "" {
		t.Fatalf("fresh admission = HTTP %d problem=%q %#v", response.status, response.problemCode, response.admission)
	}
	return response.admission
}
func countInitialReservations(t *testing.T, fixture *testfixture.Fixture, scope contract.Scope) int64 {
	t.Helper()
	var reservations int64
	fixture.SetupSQL(t, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*)
			FROM pm.pending_reservations
			WHERE released_at IS NULL
		`).Scan(&reservations)
	})
	return reservations
}

func setInitialReservationAge(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID string,
	ageSeconds int,
) {
	t.Helper()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE pm.pending_reservations
			SET accepted_at = clock_timestamp() - make_interval(secs => $2)
			WHERE run_id = $1 AND released_at IS NULL
		`, runID, ageSeconds)
		if err == nil && tag.RowsAffected() != 1 {
			return errors.New("controlled pending reservation is absent")
		}
		return err
	})
}

func transitionInitialRun(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID string,
	state contract.RunState,
	reason string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		run, err := store.LoadRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		next, err := run.Transition(state, reason, run.ProjectionObservedAt.Add(time.Second))
		if err != nil {
			return err
		}
		identity := fmt.Sprintf("%s-%d", runID, next.Revision)
		safe := contract.SafePayload{
			References:  []contract.SafeReference{{Name: "run", Ref: runID}},
			Counts:      []contract.SafeCount{},
			ReasonCodes: []string{reason},
		}
		_, err = store.CommitRunTransition(ctx, tx, store.RunTransitionCommit{
			Run: next,
			Receipt: store.Receipt{
				ReceiptID: "receipt-transition-" + identity,
				Kind:      "run_transition",
				RunID:     runID,
				Payload:   safe,
			},
			Audit: store.AuditEntry{
				AuditID:   "audit-transition-" + identity,
				Action:    "run_transitioned",
				TargetRef: runID,
				Payload:   safe,
			},
			Event: store.EventInput{
				EventID: "event-transition-" + identity,
				Type:    "run_transitioned",
				Payload: safe,
			},
			Outbox: store.OutboxCommand{
				CommandID:  "outbox-transition-" + identity,
				Kind:       "project_run",
				PayloadRef: runID,
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("commit controlled run transition: %v", err)
	}
}

func pendingReservationForTest(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID string,
) (int64, bool, string) {
	t.Helper()
	var outstanding int64
	var releasedAt *time.Time
	var reason *string
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE released_at IS NULL), max(released_at), max(release_reason)
			FROM pm.pending_reservations
			WHERE run_id = $1
		`, runID).Scan(&outstanding, &releasedAt, &reason)
	})
	resolvedReason := ""
	if reason != nil {
		resolvedReason = *reason
	}
	return outstanding, releasedAt != nil, resolvedReason
}

func ageAdmissionReceipt(t *testing.T, fixture *testfixture.Fixture, runID string, resolved bool) {
	t.Helper()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		if resolved {
			_, err := tx.Exec(ctx, `
				UPDATE pm.admission_keys
				SET resolved_at = clock_timestamp() - interval '169 hours'
				WHERE run_id = $1
			`, runID)
			return err
		}
		_, err := tx.Exec(ctx, `
			WITH aged AS (
				SELECT clock_timestamp() - interval '169 hours' AS accepted_at
			)
			UPDATE pm.admission_keys AS admission
			SET accepted_at = aged.accepted_at,
			    safe_response = jsonb_set(
			        admission.safe_response,
			        '{accepted_at}',
			        to_jsonb(aged.accepted_at),
			        false
			    )
			FROM aged
			WHERE admission.run_id = $1
			  AND admission.resolved_at IS NULL
			  AND admission.safe_response IS NOT NULL
		`, runID)
		return err
	})
}

func compactedAdmissionReceipt(t *testing.T, fixture *testfixture.Fixture, runID string) bool {
	t.Helper()
	var compacted bool
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT safe_response IS NULL
			FROM pm.admission_keys
			WHERE run_id = $1
		`, runID).Scan(&compacted)
	})
	return compacted
}

func assertSingleAdmissionRun(t *testing.T, fixture *testfixture.Fixture, runID string) {
	t.Helper()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		var keys, runs, commands int64
		if err := tx.QueryRow(ctx, `
			SELECT
				(SELECT count(*) FROM pm.admission_keys WHERE run_id = $1),
				(SELECT count(*) FROM pm.run_specs WHERE run_id = $1),
				(SELECT count(*) FROM pm.outbox_commands WHERE run_id = $1 AND kind = 'start_run')
		`, runID).Scan(&keys, &runs, &commands); err != nil {
			return err
		}
		if keys != 1 || runs != 1 || commands != 1 {
			return fmt.Errorf("recovery duplicated admission rows: keys=%d runs=%d start_commands=%d", keys, runs, commands)
		}
		return nil
	})
}
