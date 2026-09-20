package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

// The subprocess runs the product CLI entry point with a controlled session
// store and a trusted test certificate. Real fixture credentials remain in
// the parent-owned authenticated transport, never in child arguments or files.
type cliProcessInput struct {
	Args          []string
	ProfilesPath  string
	InstanceID    string
	SessionHandle string
	Certificate   []byte
	PrincipalID   string
}

func TestMain(m *testing.M) {
	if os.Getenv("PM_CLI_TEST_PROCESS") != "1" {
		os.Exit(m.Run())
	}
	var input cliProcessInput
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 64<<10)).Decode(&input); err != nil {
		os.Exit(90)
	}
	certificate, err := x509.ParseCertificate(input.Certificate)
	if err != nil {
		os.Exit(90)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	client := &http.Client{
		Transport:     transport,
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	store := pmapi.NewMemorySessionStore()
	key, err := pmapi.NewSessionKey(input.InstanceID, input.SessionHandle)
	if err != nil {
		os.Exit(90)
	}
	if err := store.Store(context.Background(), key, pmapi.PMDeviceSession{
		ExpiresAt:           time.Now().Add(time.Hour),
		PmSessionCredential: "synthetic-child-session",
		Principal:           pmapi.Principal{Kind: pmapi.PrincipalKindUser, PrincipalId: input.PrincipalID},
		SessionId:           "shared-fixture-session",
	}); err != nil {
		os.Exit(90)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code := Execute(ctx, Options{
		Args: input.Args, ProfilesPath: input.ProfilesPath,
		Stdin: strings.NewReader(""), Stdout: os.Stdout, Stderr: os.Stderr,
		HTTPClient:   client,
		SessionStore: func(pmapi.SessionStoreMode) (pmapi.SessionStore, error) { return store, nil },
	})
	transport.CloseIdleConnections()
	os.Exit(code)
}

func newCLIProcessPeer(t *testing.T, fixture *testfixture.Fixture, concurrentRequests int32) *httptest.Server {
	t.Helper()
	origin, err := url.Parse(fixture.BaseURL)
	if err != nil {
		t.Fatal("invalid fixture origin")
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(origin)
		},
		Transport: fixture.Client.Transport,
	}
	proxy.ErrorHandler = func(writer http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(writer, "fixture transport failed", http.StatusBadGateway)
	}
	var entered atomic.Int32
	ready := make(chan struct{})
	peer := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/v1/workspaces/") {
			if entered.Add(1) == concurrentRequests {
				close(ready)
			}
			select {
			case <-ready:
			case <-request.Context().Done():
				return
			}
		}
		proxy.ServeHTTP(writer, request)
	}))
	t.Cleanup(peer.Close)
	return peer
}

type cliProcess struct {
	command *exec.Cmd
	stdout  bytes.Buffer
	stderr  bytes.Buffer
}

func startCLIProcess(t *testing.T, input cliProcessInput) *cliProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate CLI test process: %v", err)
	}
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal("encode CLI process input")
	}
	process := &cliProcess{command: exec.CommandContext(t.Context(), executable)}
	process.command.Env = append(os.Environ(), "PM_CLI_TEST_PROCESS=1")
	process.command.Stdin = bytes.NewReader(payload)
	process.command.Stdout = &process.stdout
	process.command.Stderr = &process.stderr
	process.command.WaitDelay = 5 * time.Second
	if err := process.command.Start(); err != nil {
		t.Fatalf("start CLI process: %v", err)
	}
	t.Cleanup(func() {
		if process.command.ProcessState == nil {
			_ = process.command.Process.Kill()
			_ = process.command.Wait()
		}
	})
	return process
}

func TestThinBinaryDependencyBoundary(t *testing.T) {
	root := os.Getenv("PM_STACK_ROOT")
	if !filepath.IsAbs(root) {
		t.Fatal("thin binary qualification requires the explicit owned-fixture checkout")
	}
	parentEnvironment := os.Environ()
	nestedBootstrapEnvironment := make([]string, 0, len(parentEnvironment))
	for _, entry := range parentEnvironment {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "CGO_CFLAGS", "CGO_CPPFLAGS", "CGO_CXXFLAGS", "CGO_LDFLAGS":
			// The outer bootstrap derives these for its child. A nested
			// bootstrap must recompute them instead of treating them as
			// caller-owned Linux native overrides.
			continue
		default:
			nestedBootstrapEnvironment = append(nestedBootstrapEnvironment, entry)
		}
	}
	command := exec.CommandContext(t.Context(), "python3", filepath.Join(root, "tools", "bootstrap.py"), "exec", "go", "--", "list", "-mod=readonly", "-deps", "-f", "{{.ImportPath}}", "./cmd/pm")
	command.Dir = root
	command.Env = nestedBootstrapEnvironment
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("list thin binary dependencies: %v output %q", err, output)
	}
	forbidden := []string{
		"github.com/polymetrics-ai/pm-cli/internal/server",
		"github.com/polymetrics-ai/pm-cli/internal/store",
		"github.com/polymetrics-ai/pm-cli/internal/credentials",
		"github.com/gin-gonic/gin",
		"github.com/jackc/pgx",
		"github.com/duckdb/",
		"go.temporal.io/",
	}
	forbiddenDependency := func(output []byte) string {
		for _, dependency := range strings.Fields(string(output)) {
			for _, prefix := range forbidden {
				if strings.HasPrefix(dependency, prefix) {
					return dependency
				}
			}
		}
		return ""
	}
	if dependency := forbiddenDependency(output); dependency != "" {
		t.Fatalf("thin binary imports forbidden dependency %q", dependency)
	}

	probeDirectory := t.TempDir()
	probeSource := filepath.Join(probeDirectory, "forbidden.go")
	if err := os.WriteFile(probeSource, []byte("package main\nimport _ \"github.com/polymetrics-ai/pm-cli/internal/server\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(probeDirectory, "overlay.json")
	overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{
		filepath.Join(root, "cmd", "pm", "forbidden_dependency_probe.go"): probeSource,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlayPath, overlay, 0o600); err != nil {
		t.Fatal(err)
	}
	fault := exec.CommandContext(t.Context(), "python3", filepath.Join(root, "tools", "bootstrap.py"), "exec", "go", "--", "list", "-mod=readonly", "-overlay", overlayPath, "-deps", "-f", "{{.ImportPath}}", "./cmd/pm")
	fault.Dir = root
	fault.Env = nestedBootstrapEnvironment
	faultOutput, err := fault.CombinedOutput()
	if err != nil {
		t.Fatalf("resolve forbidden dependency probe: %v output %q", err, faultOutput)
	}
	if forbiddenDependency(faultOutput) == "" {
		t.Fatal("boundary qualification accepted a real server import in the CLI dependency graph")
	}

	fixture := testfixture.New(t)
	peer := newCLIProcessPeer(t, fixture, 1)
	profilesPath := filepath.Join(t.TempDir(), "profiles.json")
	handle, err := pmapi.DefaultSessionHandle("fixture", fixture.DeploymentID)
	if err != nil {
		t.Fatalf("derive fixture session handle: %v", err)
	}
	profile := pmapi.ServerProfile{Name: "fixture", ServerURL: peer.URL, InstanceID: fixture.DeploymentID, SessionHandle: handle}
	if err := pmapi.AddServerProfile(profilesPath, profile); err != nil {
		t.Fatalf("write fixture profile: %v", err)
	}
	process := startCLIProcess(t, cliProcessInput{
		Args:         []string{"workspaces", "inspect", "--server-profile", "fixture", "--workspace", fixture.Scope.WorkspaceID, "--session-store", "memory", "--output", "json"},
		ProfilesPath: profilesPath, InstanceID: fixture.DeploymentID, SessionHandle: handle,
		Certificate: peer.Certificate().Raw, PrincipalID: fixture.Actor.RequestedActor.PrincipalID,
	})
	if err := process.command.Wait(); err != nil {
		t.Fatalf("thin client process exit: %v stderr %q", err, process.stderr.String())
	}
	if !strings.Contains(process.stdout.String(), fixture.Scope.WorkspaceID) {
		t.Fatalf("thin client process did not observe its workspace: %q", process.stdout.String())
	}

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, fixture.BaseURL+"/v1/info", nil)
	if err != nil {
		t.Fatalf("construct post-client server probe: %v", err)
	}
	response, err := fixture.Client.Do(request)
	if err != nil {
		t.Fatalf("server stopped with client: %v", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("server status after client exit = %d", response.StatusCode)
	}
}

func TestWatchInterruptPreservesRun(t *testing.T) {
	t.Run("supported_process_signal_closes_only_the_subscription", func(t *testing.T) {
		fixture := testfixture.NewAdmitting(t)
		admitted := admitBoundaryRun(t, fixture, "issue20-signal")
		behavior := &runCLIBehavior{runID: admitted.RunRef, eventEntered: make(chan struct{})}
		peer := newRunCLIProcessPeer(t, fixture, behavior)
		profilesPath, handle := writeRunCLIProfile(t, fixture, peer)
		process := startCLIProcess(t, runCLIProcessInput(
			fixture,
			peer,
			profilesPath,
			handle,
			"runs", "watch", admitted.RunRef,
			"--server-profile", "fixture",
			"--workspace", fixture.Scope.WorkspaceID,
			"--session-store", "memory",
			"--output", "json",
			"--request-timeout", "5s",
		))
		select {
		case <-behavior.eventEntered:
		case <-time.After(2 * time.Second):
			t.Fatal("run watch did not enter the canonical event route")
		}
		if err := process.command.Process.Signal(os.Interrupt); err != nil {
			t.Fatalf("interrupt run watch: %v", err)
		}
		if code := waitCLIProcess(t, process, 2*time.Second); code != 0 {
			t.Fatalf("interrupted watch exit = %d stderr %q", code, process.stderr.String())
		}
		if behavior.cancelCalls.Load() != 0 {
			t.Fatalf("watch interruption sent %d durable cancellation requests", behavior.cancelCalls.Load())
		}

		observer := startCLIProcess(t, runCLIProcessInput(
			fixture,
			peer,
			profilesPath,
			handle,
			"runs", "inspect", admitted.RunRef,
			"--server-profile", "fixture",
			"--workspace", fixture.Scope.WorkspaceID,
			"--session-store", "memory",
			"--output", "json",
		))
		if code := waitCLIProcess(t, observer, 2*time.Second); code != 0 ||
			!strings.Contains(observer.stdout.String(), admitted.RunRef) ||
			!strings.Contains(observer.stdout.String(), `"state": "accepted"`) {
			t.Fatalf("second client observation = exit %d stdout %q stderr %q", code, observer.stdout.String(), observer.stderr.String())
		}
	})

	t.Run("pm-cli-run-watch-E03-C01_explicit_cancel_positive_control", func(t *testing.T) {
		fixture := testfixture.NewAdmitting(t)
		admitted := admitBoundaryRun(t, fixture, "issue20-explicit-cancel")
		view := loadBoundaryRun(t, fixture, admitted.RunRef)
		behavior := &runCLIBehavior{runID: admitted.RunRef}
		peer := newRunCLIProcessPeer(t, fixture, behavior)
		profilesPath, handle := writeRunCLIProfile(t, fixture, peer)
		process := startCLIProcess(t, runCLIProcessInput(
			fixture,
			peer,
			profilesPath,
			handle,
			"runs", "cancel", admitted.RunRef,
			"--server-profile", "fixture",
			"--workspace", fixture.Scope.WorkspaceID,
			"--session-store", "memory",
			"--output", "json",
			"--request-key", "issue20-explicit-cancel-command",
			"--revision", view.Revision,
		))
		if code := waitCLIProcess(t, process, 3*time.Second); code != 0 {
			t.Fatalf("explicit cancel exit = %d stdout %q stderr %q", code, process.stdout.String(), process.stderr.String())
		}
		after := loadBoundaryRun(t, fixture, admitted.RunRef)
		if behavior.cancelCalls.Load() != 1 || !after.CancelRequested ||
			!strings.Contains(process.stdout.String(), `"cancel_requested": true`) {
			t.Fatalf("explicit cancel = calls %d view %#v stdout %q", behavior.cancelCalls.Load(), after, process.stdout.String())
		}
	})

	t.Run("pm-cli-run-watch-E03-C02_partial_unknown_and_terminal_results_remain_distinct", func(t *testing.T) {
		cases := []struct {
			name       string
			state      pmapi.RunState
			unknown    string
			exit       int
			wantOutput string
		}{
			{name: "pm-cli-run-watch-E03-C03_known_completed_success", state: pmapi.RunStateSucceeded, unknown: "0", exit: 0, wantOutput: `"state": "succeeded"`},
			{name: "pm-cli-run-watch-E03-C04_known_terminal_failure", state: pmapi.RunStateFailed, unknown: "0", exit: 6, wantOutput: `"state": "failed"`},
			{name: "pm-cli-run-watch-E03-C05_partial_result_retains_member_outcomes", state: pmapi.RunStatePartial, unknown: "0", exit: 6, wantOutput: `"state": "partial"`},
			{name: "pm-cli-run-watch-E03-C06_unknown_effect_requires_reconciliation", state: pmapi.RunStateReconciling, unknown: "1", exit: 7, wantOutput: `"unknown_effect_count": "1"`},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				fixture := testfixture.NewAdmitting(t)
				admitted := admitBoundaryRun(t, fixture, "issue20-"+testCase.name)
				behavior := &runCLIBehavior{
					runID: admitted.RunRef,
					projection: &runCLIProjection{
						state:   testCase.state,
						unknown: testCase.unknown,
					},
				}
				peer := newRunCLIProcessPeer(t, fixture, behavior)
				profilesPath, handle := writeRunCLIProfile(t, fixture, peer)
				process := startCLIProcess(t, runCLIProcessInput(
					fixture,
					peer,
					profilesPath,
					handle,
					"runs", "watch", admitted.RunRef,
					"--server-profile", "fixture",
					"--workspace", fixture.Scope.WorkspaceID,
					"--session-store", "memory",
					"--output", "json",
					"--request-timeout", "5s",
				))
				if code := waitCLIProcess(t, process, 3*time.Second); code != testCase.exit ||
					!strings.Contains(process.stdout.String(), testCase.wantOutput) ||
					behavior.cancelCalls.Load() != 0 {
					t.Fatalf(
						"terminal render = exit %d stdout %q stderr %q cancels %d; want exit %d output %q",
						code,
						process.stdout.String(),
						process.stderr.String(),
						behavior.cancelCalls.Load(),
						testCase.exit,
						testCase.wantOutput,
					)
				}
				if testCase.state == pmapi.RunStatePartial {
					tasks := startCLIProcess(t, runCLIProcessInput(
						fixture,
						peer,
						profilesPath,
						handle,
						"runs", "tasks", admitted.RunRef,
						"--server-profile", "fixture",
						"--workspace", fixture.Scope.WorkspaceID,
						"--session-store", "memory",
						"--output", "json",
					))
					if code := waitCLIProcess(t, tasks, 3*time.Second); code != 0 ||
						!strings.Contains(tasks.stdout.String(), "member-succeeded") ||
						!strings.Contains(tasks.stdout.String(), "member-rejected") {
						t.Fatalf("partial member outcomes = exit %d stdout %q stderr %q", code, tasks.stdout.String(), tasks.stderr.String())
					}
				}
			})
		}
	})

	t.Run("pm-cli-run-watch-E03-C07_view_deadline_keeps_run_accepted", func(t *testing.T) {
		fixture := testfixture.NewAdmitting(t)
		admitted := admitBoundaryRun(t, fixture, "issue20-view-deadline")
		behavior := &runCLIBehavior{runID: admitted.RunRef, holdEventStream: true, eventEntered: make(chan struct{})}
		peer := newRunCLIProcessPeer(t, fixture, behavior)
		profilesPath, handle := writeRunCLIProfile(t, fixture, peer)
		process := startCLIProcess(t, runCLIProcessInput(
			fixture,
			peer,
			profilesPath,
			handle,
			"runs", "watch", admitted.RunRef,
			"--server-profile", "fixture",
			"--workspace", fixture.Scope.WorkspaceID,
			"--session-store", "memory",
			"--output", "json",
			"--request-timeout", "1s",
		))
		if code := waitCLIProcess(t, process, 3*time.Second); code != 8 ||
			behavior.cancelCalls.Load() != 0 {
			t.Fatalf("view deadline = exit %d stderr %q cancels %d; want 8/0", code, process.stderr.String(), behavior.cancelCalls.Load())
		}
		view := loadBoundaryRun(t, fixture, admitted.RunRef)
		if view.State != pmapi.RunStateAccepted || view.CancelRequested {
			t.Fatalf("accepted run after view deadline = %#v", view)
		}
	})
}

type runCLIProjection struct {
	state   pmapi.RunState
	unknown string
}

type runCLIBehavior struct {
	runID            string
	projection       *runCLIProjection
	holdEventStream  bool
	eventEntered     chan struct{}
	eventEnteredOnce atomic.Bool
	cancelCalls      atomic.Int32
}

func newRunCLIProcessPeer(t *testing.T, fixture *testfixture.Fixture, behavior *runCLIBehavior) *httptest.Server {
	t.Helper()
	origin, err := url.Parse(fixture.BaseURL)
	if err != nil {
		t.Fatal("invalid fixture origin")
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(origin)
		},
		Transport:     fixture.Client.Transport,
		FlushInterval: -1,
	}
	proxy.ErrorHandler = func(writer http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(writer, "fixture transport failed", http.StatusBadGateway)
	}
	peer := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		runPrefix := "/v1/workspaces/" + fixture.Scope.WorkspaceID + "/runs/" + behavior.runID
		switch request.URL.Path {
		case runPrefix + "/events":
			if behavior.eventEntered != nil && behavior.eventEnteredOnce.CompareAndSwap(false, true) {
				close(behavior.eventEntered)
			}
			if behavior.holdEventStream {
				writeBoundaryEventSnapshot(t, writer, behavior.runID, pmapi.RunStateRunning, "0")
				<-request.Context().Done()
				return
			}
			if behavior.projection != nil {
				writeBoundaryEventSnapshot(t, writer, behavior.runID, behavior.projection.state, behavior.projection.unknown)
				return
			}
		case runPrefix + "/result":
			if behavior.projection != nil {
				writeBoundaryRunResult(t, writer, behavior.runID, *behavior.projection)
				return
			}
		case runPrefix + "/tasks":
			if behavior.projection != nil && behavior.projection.state == pmapi.RunStatePartial {
				writeBoundaryPartialTasks(t, writer, behavior.runID)
				return
			}
		case runPrefix + "/cancel":
			behavior.cancelCalls.Add(1)
		}
		proxy.ServeHTTP(writer, request)
	}))
	t.Cleanup(peer.Close)
	return peer
}

func admitBoundaryRun(t *testing.T, fixture *testfixture.Fixture, requestKey string) pmapi.AdmissionResponse {
	t.Helper()
	document, err := json.Marshal(testfixture.Invocation(t, fixture, requestKey))
	if err != nil {
		t.Fatalf("encode boundary admission: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fixture.BaseURL+"/v1/workspaces/"+fixture.Scope.WorkspaceID+"/invocations",
		bytes.NewReader(document),
	)
	if err != nil {
		t.Fatalf("construct boundary admission: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", requestKey)
	response, err := fixture.Client.Do(request)
	if err != nil {
		t.Fatalf("submit boundary admission: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		t.Fatalf("boundary admission = HTTP %d: %s", response.StatusCode, body)
	}
	var admitted pmapi.AdmissionResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&admitted); err != nil || admitted.RunRef == "" {
		t.Fatalf("decode boundary admission: %v %#v", err, admitted)
	}
	return admitted
}

func loadBoundaryRun(t *testing.T, fixture *testfixture.Fixture, runID string) pmapi.RunView {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fixture.BaseURL+"/v1/workspaces/"+fixture.Scope.WorkspaceID+"/runs/"+runID,
		nil,
	)
	if err != nil {
		t.Fatalf("construct boundary run view: %v", err)
	}
	response, err := fixture.Client.Do(request)
	if err != nil {
		t.Fatalf("load boundary run view: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("boundary run view = HTTP %d", response.StatusCode)
	}
	var view pmapi.RunView
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&view); err != nil {
		t.Fatalf("decode boundary run view: %v", err)
	}
	return view
}

func writeRunCLIProfile(t *testing.T, fixture *testfixture.Fixture, peer *httptest.Server) (string, string) {
	t.Helper()
	profilesPath := filepath.Join(t.TempDir(), "profiles.json")
	handle, err := pmapi.DefaultSessionHandle("fixture", fixture.DeploymentID)
	if err != nil {
		t.Fatalf("derive run fixture session handle: %v", err)
	}
	profile := pmapi.ServerProfile{Name: "fixture", ServerURL: peer.URL, InstanceID: fixture.DeploymentID, SessionHandle: handle}
	if err := pmapi.AddServerProfile(profilesPath, profile); err != nil {
		t.Fatalf("write run fixture profile: %v", err)
	}
	return profilesPath, handle
}

func runCLIProcessInput(
	fixture *testfixture.Fixture,
	peer *httptest.Server,
	profilesPath string,
	handle string,
	arguments ...string,
) cliProcessInput {
	return cliProcessInput{
		Args:          arguments,
		ProfilesPath:  profilesPath,
		InstanceID:    fixture.DeploymentID,
		SessionHandle: handle,
		Certificate:   peer.Certificate().Raw,
		PrincipalID:   fixture.Actor.RequestedActor.PrincipalID,
	}
}

func waitCLIProcess(t *testing.T, process *cliProcess, timeout time.Duration) int {
	t.Helper()
	waited := make(chan error, 1)
	go func() { waited <- process.command.Wait() }()
	select {
	case err := <-waited:
		if err == nil {
			return 0
		}
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		t.Fatalf("wait for CLI process: %v stderr %q", err, process.stderr.String())
	case <-time.After(timeout):
		_ = process.command.Process.Kill()
		<-waited
		t.Fatalf("CLI process exceeded %s; stdout %q stderr %q", timeout, process.stdout.String(), process.stderr.String())
	}
	return -1
}

func writeBoundaryEventSnapshot(t *testing.T, writer http.ResponseWriter, runID string, state pmapi.RunState, unknown string) {
	t.Helper()
	summary := boundaryEffectSummary(state, unknown)
	orchestration := "completed"
	if state == pmapi.RunStateRunning {
		orchestration = "running"
	}
	if state == pmapi.RunStateFailed {
		orchestration = "failed"
	}
	if state == pmapi.RunStateReconciling {
		orchestration = "unknown"
	}
	snapshot := map[string]any{
		"cursor":             "cursor-" + runID,
		"retention_gap":      false,
		"watermark_sequence": "0",
		"run": map[string]any{
			"blocked_reason":          nil,
			"cancel_requested":        false,
			"connection_refs":         []string{"connection-alpha"},
			"effect_summary":          summary,
			"intent":                  map[string]any{"id": "operation-alpha", "kind": "operation", "revision": "1"},
			"orchestration_condition": orchestration,
			"origin":                  "api",
			"projection_observed_at":  "2026-09-18T00:00:00Z",
			"result_references":       []any{},
			"revision":                "2",
			"run_ref":                 runID,
			"state":                   state,
			"unknown_effect_count":    unknown,
		},
	}
	document, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("encode boundary event snapshot: %v", err)
	}
	writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte("event: snapshot\ndata: "))
	_, _ = writer.Write(document)
	_, _ = writer.Write([]byte("\n\n"))
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeBoundaryRunResult(t *testing.T, writer http.ResponseWriter, runID string, projection runCLIProjection) {
	t.Helper()
	document, err := json.Marshal(map[string]any{
		"effect_summary":       boundaryEffectSummary(projection.state, projection.unknown),
		"result_references":    []map[string]any{{"result_ref": "result-" + runID}},
		"revision":             "2",
		"run_ref":              runID,
		"state":                projection.state,
		"unknown_effect_count": projection.unknown,
	})
	if err != nil {
		t.Fatalf("encode boundary run result: %v", err)
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(document)
}

func boundaryEffectSummary(state pmapi.RunState, unknown string) map[string]string {
	summary := map[string]string{"rejected": "0", "succeeded": "0", "unchanged": "0", "unknown": unknown, "unsent": "0"}
	switch state {
	case pmapi.RunStateSucceeded:
		summary["succeeded"] = "1"
	case pmapi.RunStateFailed:
		summary["rejected"] = "1"
	case pmapi.RunStatePartial:
		summary["succeeded"] = "1"
		summary["rejected"] = "1"
	case pmapi.RunStateRunning:
		summary["unsent"] = "1"
	}
	return summary
}

func writeBoundaryPartialTasks(t *testing.T, writer http.ResponseWriter, runID string) {
	t.Helper()
	document, err := json.Marshal(map[string]any{
		"items": []map[string]any{{
			"behavior_id":      "behavior-alpha",
			"blocked_reason":   nil,
			"cancel_requested": false,
			"effect_summary":   boundaryEffectSummary(pmapi.RunStatePartial, "0"),
			"lane":             "direct_write",
			"member_outcomes": []map[string]any{
				{"assertion_refs": []string{}, "dispatch_id": "dispatch-alpha", "effect_key": "effect-alpha", "member_id": "member-succeeded", "observed_at": "2026-09-18T00:00:00Z", "state": "succeeded"},
				{"assertion_refs": []string{}, "dispatch_id": "dispatch-alpha", "effect_key": "effect-beta", "member_id": "member-rejected", "observed_at": "2026-09-18T00:00:00Z", "state": "rejected"},
			},
			"operation_id":            "operation-alpha",
			"orchestration_condition": "completed",
			"projection_observed_at":  "2026-09-18T00:00:00Z",
			"revision":                "2",
			"run_ref":                 runID,
			"state":                   "partial",
			"task_id":                 "task-alpha",
			"unknown_effect_count":    "0",
		}},
		"next_cursor": nil,
	})
	if err != nil {
		t.Fatalf("encode boundary partial tasks: %v", err)
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(document)
}
