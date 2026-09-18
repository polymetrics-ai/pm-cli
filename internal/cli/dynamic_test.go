package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oapi-codegen/nullable"
	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestSelectorTreeAndOfflineHelp(t *testing.T) {
	t.Parallel()
	snapshot := commandSnapshot([]pmapi.CommandFieldDescriptor{commandField("--query-workspace", "query.workspace", "workspace", contract.FieldLocationQuery)})

	t.Run("duplicate PM selector refuses before dispatch", func(t *testing.T) {
		var calls int
		status := Execute(t.Context(), Options{
			Args:    []string{"--workspace", "alpha", "--workspace", "beta", "inspect-widget", "--query-workspace", "provider"},
			Stderr:  &bytes.Buffer{},
			Catalog: &snapshot,
			DynamicInvoke: func(context.Context, DynamicInvocation) error {
				calls++
				return nil
			},
		})
		if status != 2 || calls != 0 {
			t.Fatalf("duplicate selector result = (%d, %d calls), want usage refusal before dispatch", status, calls)
		}
	})

	t.Run("tokens after terminator cannot switch PM scope", func(t *testing.T) {
		var calls int
		status := Execute(t.Context(), Options{
			Args:    []string{"--workspace", "alpha", "inspect-widget", "--query-workspace", "provider", "--", "--workspace", "beta"},
			Stderr:  &bytes.Buffer{},
			Catalog: &snapshot,
			DynamicInvoke: func(context.Context, DynamicInvocation) error {
				calls++
				return nil
			},
		})
		if status != 2 || calls != 0 {
			t.Fatalf("post-terminator selector result = (%d, %d calls), want argument refusal without dispatch", status, calls)
		}
	})

	for _, commandName := range []string{"inspect-widget", "read-widget"} {
		commandName := commandName
		t.Run(commandName+" preserves operation identity", func(t *testing.T) {
			var captured DynamicInvocation
			root := NewRoot(Options{
				Args:    []string{"--workspace", "alpha", commandName, "--query-workspace", "provider"},
				Catalog: &snapshot,
				DynamicInvoke: func(_ context.Context, invocation DynamicInvocation) error {
					captured = invocation
					return nil
				},
			})
			if err := root.ExecuteContext(t.Context()); err != nil {
				t.Fatalf("execute %s: %v", commandName, err)
			}
			fields, present := captured.Input.Fields()
			workspace, text := fields["query.workspace"].TextValue()
			if !present || !text || workspace != "provider" {
				t.Fatalf("provider workspace binding = %#v", captured.Input)
			}
			if captured.Descriptor.OperationId != "op-1" || captured.Descriptor.OperationRevision != testOperationRevision ||
				captured.Binding.WorkspaceID != "alpha" {
				t.Fatalf("friendly command changed stable identity: %#v", captured)
			}
		})
	}
	t.Run("offline help has no execution configuration", testInjectedCatalogHelpIsOffline)
}

func testInjectedCatalogHelpIsOffline(t *testing.T) {
	snapshot := commandSnapshot([]pmapi.CommandFieldDescriptor{commandField("--query-workspace", "query.workspace", "workspace", contract.FieldLocationQuery)})
	var calls int
	output := &bytes.Buffer{}
	root := NewRoot(Options{
		Args:    []string{"inspect-widget", "--help"},
		Stdout:  output,
		Stderr:  &bytes.Buffer{},
		Catalog: &snapshot,
		DynamicInvoke: func(context.Context, DynamicInvocation) error {
			calls++
			return nil
		},
	})
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("render injected command help without configuration: %v", err)
	}
	if calls != 0 || !bytes.Contains(output.Bytes(), []byte("--query-workspace")) || !bytes.Contains(output.Bytes(), []byte("op-1")) {
		t.Fatalf("offline help = %q with %d invocations", output.String(), calls)
	}
}

type unavailableDoer struct {
	calls int
}

func (doer *unavailableDoer) Do(*http.Request) (*http.Response, error) {
	doer.calls++
	return nil, errors.New("network unavailable")
}

func TestOfflineDocumentationRouting(t *testing.T) {
	t.Parallel()
	snapshot := commandSnapshot([]pmapi.CommandFieldDescriptor{
		commandField("--query-workspace", "query.workspace", "workspace", contract.FieldLocationQuery),
	})
	for _, test := range []struct {
		name     string
		args     []string
		catalog  *pmapi.CatalogSnapshot
		expected string
	}{
		{name: "manual", args: []string{"manual"}, expected: "COMMAND pm manual"},
		{name: "completion", args: []string{"completion", "bash"}, expected: "# PM-COMMAND pm completion"},
		{name: "help command", args: []string{"help", "completion"}},
		{name: "generated help from local authorized catalog", args: []string{"inspect-widget", "--help"}, catalog: &snapshot, expected: "--query-workspace"},
		{name: "generated help command from local authorized catalog", args: []string{"help", "inspect-widget"}, catalog: &snapshot, expected: "op-1"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			var environmentLookups, sessionLookups int
			network := &unavailableDoer{}
			var stdout, stderr bytes.Buffer
			code := Execute(t.Context(), Options{
				Args:         test.args,
				Stdout:       &stdout,
				Stderr:       &stderr,
				ProfilesPath: filepath.Join(t.TempDir(), "missing-profiles.json"),
				LookupEnv: func(string) (string, bool) {
					environmentLookups++
					return "", false
				},
				HTTPClient: network,
				SessionStore: func(pmapi.SessionStoreMode) (pmapi.SessionStore, error) {
					sessionLookups++
					return nil, errors.New("session store unavailable")
				},
				Catalog: test.catalog,
			})
			if code != 0 || environmentLookups != 0 || sessionLookups != 0 || network.calls != 0 ||
				(test.expected != "" && !bytes.Contains(stdout.Bytes(), []byte(test.expected))) {
				t.Fatalf(
					"offline surface = code %d env %d sessions %d network %d stdout %q stderr %q",
					code,
					environmentLookups,
					sessionLookups,
					network.calls,
					stdout.String(),
					stderr.String(),
				)
			}
		})
	}
}

func TestExecutableDiscoveryCachesOnlyRevisionBoundOfflineDocumentation(t *testing.T) {
	const (
		instanceID        = "server-production-catalog"
		profileName       = "production-catalog"
		workspaceID       = "alpha"
		connectionID      = "c1"
		sessionCredential = "pm-production-catalog-session"
	)
	snapshot := commandSnapshot([]pmapi.CommandFieldDescriptor{
		commandField("--query-workspace", "query.workspace", "workspace", contract.FieldLocationQuery),
	})
	snapshot.Binding.ServerInstanceID = instanceID
	snapshot.Binding.Principal = pmapi.Principal{Kind: pmapi.PrincipalKindUser, PrincipalId: "ada-production-catalog"}
	snapshot.Binding.WorkspaceID = pmapi.WorkspaceID(workspaceID)
	snapshot.Binding.ConnectionID = connectionID
	for index := range snapshot.Commands {
		snapshot.Commands[index].ConnectionId = connectionID
	}
	initialPrincipal := snapshot.Binding.Principal
	var activePrincipal atomic.Pointer[pmapi.Principal]
	activePrincipal.Store(&initialPrincipal)

	var infoCalls, catalogCalls, invocationCalls atomic.Int32
	var paginatedDiscovery atomic.Bool
	paginatedDiscovery.Store(true)
	invocations := make(chan pmapi.InvocationRequest, 5)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/info":
			infoCalls.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(pmapi.ServerInfo{
				ProtocolMajor:    pmapi.ProtocolMajor,
				ProtocolVersion:  pmapi.ServerInfoProtocolVersion(pmapi.WireVersion),
				InstanceId:       instanceID,
				Features:         []string{"exact-values", "wire-problem-v1"},
				RequiredFeatures: []string{"exact-values"},
			})
		case "/v1/workspaces/" + workspaceID + "/commands":
			catalogCalls.Add(1)
			if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer "+sessionCredential ||
				request.URL.Query().Get("connection_id") != connectionID {
				t.Errorf("catalog request = %s %s authorization %q", request.Method, request.URL.String(), request.Header.Get("Authorization"))
			}
			items := snapshot.Commands
			nextCursor := nullable.NewNullNullable[string]()
			if request.URL.Query().Get("filter") == "not-present" {
				items = []pmapi.CommandDescriptor{}
			} else if paginatedDiscovery.Load() && request.URL.Query().Get("cursor") == "" {
				items = []pmapi.CommandDescriptor{}
				nextCursor = nullable.NewNullableWithValue("remaining-commands")
			}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(pmapi.SafeCommandPage{
				CatalogRevision:  snapshot.Binding.CatalogRevision,
				ConnectionId:     connectionID,
				Items:            items,
				NextCursor:       nextCursor,
				Principal:        *activePrincipal.Load(),
				ServerInstanceId: instanceID,
				WorkspaceId:      workspaceID,
			})
		case "/v1/workspaces/" + workspaceID + "/commands/op-1":
			if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer "+sessionCredential ||
				request.URL.Query().Get("connection_id") != connectionID {
				t.Errorf("command inspection request = %s %s", request.Method, request.URL.String())
			}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(snapshot.Commands[0])
		case "/v1/workspaces/" + workspaceID + "/invocations":
			invocationCalls.Add(1)
			if request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer "+sessionCredential {
				t.Errorf("invocation request = %s authorization %q", request.Method, request.Header.Get("Authorization"))
			}
			var body pmapi.InvocationRequest
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode invocation: %v", err)
			}
			invocations <- body
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(writer).Encode(pmapi.AdmissionResponse{
				IntentDigest: "sha256:" + strings.Repeat("a", 64),
				OutcomeReference: pmapi.SafeOutcomeReference{
					Kind: pmapi.SafeOutcomeReferenceKindRun,
					Ref:  "run-catalog-projection-fixture",
				},
				RequestKey: request.Header.Get("Idempotency-Key"),
				RunRef:     "run-catalog-projection-fixture",
				State:      pmapi.AdmissionResponseStateAccepted,
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	profilesPath := filepath.Join(t.TempDir(), "profiles.json")
	handle, err := pmapi.DefaultSessionHandle(profileName, instanceID)
	if err != nil {
		t.Fatalf("session handle: %v", err)
	}
	profile := pmapi.ServerProfile{
		Name: profileName, ServerURL: server.URL, InstanceID: instanceID, SessionHandle: handle,
	}
	if err := pmapi.AddServerProfile(profilesPath, profile); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	sessions := pmapi.NewMemorySessionStore()
	sessionKey, err := pmapi.NewSessionKey(instanceID, handle)
	if err != nil {
		t.Fatalf("session key: %v", err)
	}
	if err := sessions.Store(t.Context(), sessionKey, pmapi.PMDeviceSession{
		ExpiresAt:           time.Now().Add(time.Hour),
		PmSessionCredential: sessionCredential,
		Principal:           snapshot.Binding.Principal,
		SessionId:           "session-production-catalog",
	}); err != nil {
		t.Fatalf("store session: %v", err)
	}
	projectionPath, err := catalogProjectionPath(
		profilesPath,
		profile,
		snapshot.Binding.WorkspaceID,
		snapshot.Binding.ConnectionID,
	)
	if err != nil {
		t.Fatalf("catalog projection path: %v", err)
	}
	factory := func(mode pmapi.SessionStoreMode) (pmapi.SessionStore, error) {
		if mode != pmapi.SessionStoreMemory {
			t.Fatalf("session mode = %q, want memory", mode)
		}
		return sessions, nil
	}
	discoverySelectors := []string{
		"--server-profile", profileName,
		"--workspace", workspaceID,
		"--connection", connectionID,
		"--session-store", "memory",
	}
	for _, discovery := range []struct {
		name string
		args []string
	}{
		{name: "paged list", args: []string{"commands", "list"}},
		{name: "filtered list", args: []string{"commands", "list", "--filter", "not-present"}},
		{name: "inspection", args: []string{"commands", "inspect", "op-1"}},
	} {
		t.Run("read-only discovery/"+discovery.name, func(t *testing.T) {
			defer func() {
				if err := os.Remove(projectionPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Errorf("remove owned synthetic projection: %v", err)
				}
			}()
			var stdout, stderr bytes.Buffer
			code := Execute(t.Context(), Options{
				Args:   append(append([]string{}, discoverySelectors...), discovery.args...),
				Stdout: &stdout, Stderr: &stderr, ProfilesPath: profilesPath,
				HTTPClient: server.Client(), SessionStore: factory,
			})
			if code != 0 || invocationCalls.Load() != 0 {
				t.Fatalf("read-only discovery = code %d invocations %d stdout %q stderr %q",
					code, invocationCalls.Load(), stdout.String(), stderr.String())
			}
			if discovery.name == "filtered list" && strings.Contains(stdout.String(), "inspect-widget") {
				t.Fatalf("filtered discovery output includes an excluded command: %s", stdout.String())
			}
			network := &unavailableDoer{}
			stdout.Reset()
			stderr.Reset()
			code = Execute(t.Context(), Options{
				Args:   append(append([]string{}, discoverySelectors...), "manual"),
				Stdout: &stdout, Stderr: &stderr, ProfilesPath: profilesPath,
				HTTPClient: network,
				SessionStore: func(pmapi.SessionStoreMode) (pmapi.SessionStore, error) {
					t.Fatal("offline documentation attempted session access")
					return nil, errors.New("offline session access")
				},
			})
			if code != 0 || network.calls != 0 || invocationCalls.Load() != 0 ||
				!strings.Contains(stdout.String(), "COMMAND pm inspect-widget") {
				t.Fatalf("read-only discovery did not prime the complete offline manual: code %d network %d invocations %d stdout %q stderr %q",
					code, network.calls, invocationCalls.Load(), stdout.String(), stderr.String())
			}
		})
	}
	paginatedDiscovery.Store(false)
	infoCalls.Store(0)
	catalogCalls.Store(0)
	for index, providerValue := range []string{"--help", "-h", "--", "--workspace"} {
		var stdout, stderr bytes.Buffer
		code := Execute(t.Context(), Options{
			Args: []string{
				"--server-profile", profileName,
				"--workspace", workspaceID,
				"--connection", connectionID,
				"--session-store", "memory",
				"inspect-widget",
				"--query-workspace", providerValue,
				"--request-key", fmt.Sprintf("request-provider-value-%d", index),
			},
			Stdout: &stdout, Stderr: &stderr, ProfilesPath: profilesPath,
			HTTPClient: server.Client(), SessionStore: factory,
		})
		if code != 0 {
			t.Fatalf("provider value %q exit = %d stdout %q stderr %q", providerValue, code, stdout.String(), stderr.String())
		}
		var invocation pmapi.InvocationRequest
		select {
		case invocation = <-invocations:
		default:
			t.Fatalf("provider value %q returned without an invocation: stdout %q stderr %q", providerValue, stdout.String(), stderr.String())
		}
		fields, present := invocation.Input.Fields()
		value, text := fields["query.workspace"].TextValue()
		if !present || !text || value != providerValue {
			t.Fatalf("provider value %q became input %#v", providerValue, invocation.Input)
		}
	}
	switchedPrincipal := pmapi.Principal{
		Kind:        pmapi.PrincipalKindUser,
		PrincipalId: "grace-production-catalog",
	}
	activePrincipal.Store(&switchedPrincipal)
	if err := sessions.Store(t.Context(), sessionKey, pmapi.PMDeviceSession{
		ExpiresAt:           time.Now().Add(time.Hour),
		PmSessionCredential: sessionCredential,
		Principal:           switchedPrincipal,
		SessionId:           "session-production-catalog-switched",
	}); err != nil {
		t.Fatalf("switch session principal: %v", err)
	}
	var switchedStdout, switchedStderr bytes.Buffer
	switchedCode := Execute(t.Context(), Options{
		Args: []string{
			"--server-profile", profileName,
			"--workspace", workspaceID,
			"--connection", connectionID,
			"--session-store", "memory",
			"inspect-widget",
			"--query-workspace", "account-switch",
			"--request-key", "request-account-switch",
		},
		Stdout: &switchedStdout, Stderr: &switchedStderr, ProfilesPath: profilesPath,
		HTTPClient: server.Client(), SessionStore: factory,
	})
	if switchedCode != 0 {
		t.Fatalf(
			"account switch exit = %d stdout %q stderr %q",
			switchedCode,
			switchedStdout.String(),
			switchedStderr.String(),
		)
	}
	var switchedInvocation pmapi.InvocationRequest
	select {
	case switchedInvocation = <-invocations:
	default:
		t.Fatalf("account switch returned without an invocation: stdout %q stderr %q", switchedStdout.String(), switchedStderr.String())
	}
	switchedFields, switchedPresent := switchedInvocation.Input.Fields()
	switchedValue, switchedText := switchedFields["query.workspace"].TextValue()
	if !switchedPresent || !switchedText || switchedValue != "account-switch" {
		t.Fatalf("account switch became input %#v", switchedInvocation.Input)
	}
	if infoCalls.Load() != 5 || catalogCalls.Load() != 5 || invocationCalls.Load() != 5 {
		t.Fatalf(
			"fresh executable discovery calls = info %d catalog %d invocation %d, want 5/5/5",
			infoCalls.Load(),
			catalogCalls.Load(),
			invocationCalls.Load(),
		)
	}
	projectionDocument, err := os.ReadFile(projectionPath)
	if err != nil {
		t.Fatalf("read catalog projection: %v", err)
	}
	if bytes.Contains(projectionDocument, []byte(sessionCredential)) {
		t.Fatal("catalog projection persisted a session credential")
	}

	selectorArgs := []string{
		"--server-profile", profileName,
		"--workspace", workspaceID,
		"--connection", connectionID,
	}
	for _, test := range []struct {
		name     string
		args     []string
		expected string
	}{
		{name: "root help", args: append(append([]string{}, selectorArgs...), "--help"), expected: "inspect-widget"},
		{name: "generated help", args: append(append([]string{}, selectorArgs...), "inspect-widget", "--help"), expected: "--query-workspace"},
		{name: "generated help command", args: append(append([]string{}, selectorArgs...), "help", "inspect-widget"), expected: "op-1"},
		{name: "manual", args: append(append([]string{}, selectorArgs...), "manual"), expected: "COMMAND pm inspect-widget"},
		{name: "completion", args: append(append([]string{}, selectorArgs...), "completion", "bash"), expected: "# PM-COMMAND pm inspect-widget"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			network := &unavailableDoer{}
			var environmentLookups, sessionLookups int
			var stdout, stderr bytes.Buffer
			code := Execute(t.Context(), Options{
				Args: test.args, Stdout: &stdout, Stderr: &stderr, ProfilesPath: profilesPath,
				LookupEnv: func(string) (string, bool) {
					environmentLookups++
					return "broken-execution-setting", true
				},
				HTTPClient: network,
				SessionStore: func(pmapi.SessionStoreMode) (pmapi.SessionStore, error) {
					sessionLookups++
					return nil, errors.New("session store unavailable")
				},
			})
			if code != 0 || environmentLookups != 0 || sessionLookups != 0 || network.calls != 0 ||
				!strings.Contains(stdout.String(), test.expected) ||
				!strings.Contains(stderr.String(), snapshot.Binding.CatalogRevision) ||
				!strings.Contains(stderr.String(), switchedPrincipal.PrincipalId) ||
				strings.Contains(stderr.String(), snapshot.Binding.Principal.PrincipalId) {
				t.Fatalf(
					"offline %s = code %d env %d sessions %d network %d stdout %q stderr %q",
					test.name,
					code,
					environmentLookups,
					sessionLookups,
					network.calls,
					stdout.String(),
					stderr.String(),
				)
			}
		})
	}

	network := &unavailableDoer{}
	sessionLookups := 0
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), Options{
		Args: append(
			append([]string{}, selectorArgs...),
			"inspect-widget", "--query-workspace", "offline-attempt",
		),
		Stdout: &stdout, Stderr: &stderr, ProfilesPath: profilesPath, HTTPClient: network,
		LookupEnv: func(string) (string, bool) { return "", false },
		SessionStore: func(pmapi.SessionStoreMode) (pmapi.SessionStore, error) {
			sessionLookups++
			return nil, errors.New("fresh session qualification required")
		},
	})
	if code != 5 || sessionLookups != 0 || network.calls != 1 {
		t.Fatalf(
			"offline execution attempt = code %d sessions %d network %d stdout %q stderr %q",
			code,
			sessionLookups,
			network.calls,
			stdout.String(),
			stderr.String(),
		)
	}
}

func TestOfflineCatalogProjectionFailureModes(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, Options, pmapi.ServerProfile, pmapi.CatalogSnapshot)
	}{
		{
			name: "missing",
		},
		{
			name: "cross profile isolated",
			prepare: func(t *testing.T, options Options, profile pmapi.ServerProfile, snapshot pmapi.CatalogSnapshot) {
				t.Helper()
				foreign := profile
				foreign.Name = "other-profile"
				handle, err := pmapi.DefaultSessionHandle(foreign.Name, foreign.InstanceID)
				if err != nil {
					t.Fatalf("foreign session handle: %v", err)
				}
				foreign.SessionHandle = handle
				if err := pmapi.AddServerProfile(options.ProfilesPath, foreign); err != nil {
					t.Fatalf("write foreign profile: %v", err)
				}
				if err := storeCatalogProjection(options, foreign, snapshot); err != nil {
					t.Fatalf("store cross-profile projection: %v", err)
				}
			},
		},
		{
			name: "corrupt",
			prepare: func(t *testing.T, options Options, profile pmapi.ServerProfile, snapshot pmapi.CatalogSnapshot) {
				t.Helper()
				path, err := catalogProjectionPath(options.ProfilesPath, profile, snapshot.Binding.WorkspaceID, snapshot.Binding.ConnectionID)
				if err != nil {
					t.Fatalf("catalog projection path: %v", err)
				}
				if err := writeCatalogProjection(path, []byte("{\n")); err != nil {
					t.Fatalf("write corrupt projection fixture: %v", err)
				}
			},
		},
		{
			name: "foreign",
			prepare: func(t *testing.T, options Options, profile pmapi.ServerProfile, snapshot pmapi.CatalogSnapshot) {
				t.Helper()
				foreign := snapshot
				foreign.Binding.WorkspaceID = "foreign-workspace"
				if err := storeCatalogProjection(options, profile, foreign); err != nil {
					t.Fatalf("store foreign projection: %v", err)
				}
				foreignPath, err := catalogProjectionPath(options.ProfilesPath, profile, foreign.Binding.WorkspaceID, foreign.Binding.ConnectionID)
				if err != nil {
					t.Fatalf("foreign projection path: %v", err)
				}
				selectedPath, err := catalogProjectionPath(options.ProfilesPath, profile, snapshot.Binding.WorkspaceID, snapshot.Binding.ConnectionID)
				if err != nil {
					t.Fatalf("selected projection path: %v", err)
				}
				if err := os.Rename(foreignPath, selectedPath); err != nil {
					t.Fatalf("place foreign projection: %v", err)
				}
			},
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			const (
				instanceID   = "server-offline-failure"
				profileName  = "offline-failure"
				workspaceID  = "alpha"
				connectionID = "c1"
			)
			profilesPath := filepath.Join(t.TempDir(), "profiles.json")
			handle, err := pmapi.DefaultSessionHandle(profileName, instanceID)
			if err != nil {
				t.Fatalf("session handle: %v", err)
			}
			profile := pmapi.ServerProfile{
				Name: profileName, ServerURL: "https://pm.example.test", InstanceID: instanceID, SessionHandle: handle,
			}
			if err := pmapi.AddServerProfile(profilesPath, profile); err != nil {
				t.Fatalf("write profile: %v", err)
			}
			snapshot := commandSnapshot([]pmapi.CommandFieldDescriptor{
				commandField("--query-workspace", "query.workspace", "workspace", contract.FieldLocationQuery),
			})
			snapshot.Binding.ServerInstanceID = instanceID
			snapshot.Binding.WorkspaceID = workspaceID
			snapshot.Binding.ConnectionID = connectionID
			for index := range snapshot.Commands {
				snapshot.Commands[index].ConnectionId = connectionID
			}
			options := Options{ProfilesPath: profilesPath}
			if test.prepare != nil {
				test.prepare(t, options, profile, snapshot)
			}

			network := &unavailableDoer{}
			var sessionLookups int
			var stdout, stderr bytes.Buffer
			code := Execute(t.Context(), Options{
				Args: []string{
					"--server-profile", profileName,
					"--workspace", workspaceID,
					"--connection", connectionID,
					"manual",
				},
				Stdout: &stdout, Stderr: &stderr, ProfilesPath: profilesPath, HTTPClient: network,
				SessionStore: func(pmapi.SessionStoreMode) (pmapi.SessionStore, error) {
					sessionLookups++
					return nil, errors.New("session store unavailable")
				},
			})
			if code != 0 || sessionLookups != 0 || network.calls != 0 ||
				!strings.Contains(stdout.String(), "COMMAND pm manual") ||
				strings.Contains(stdout.String(), "COMMAND pm inspect-widget") {
				t.Fatalf(
					"%s projection = code %d sessions %d network %d stdout %q stderr %q",
					test.name,
					code,
					sessionLookups,
					network.calls,
					stdout.String(),
					stderr.String(),
				)
			}
		})
	}
}

func TestProjectedRequestVariantSemantics(t *testing.T) {
	t.Parallel()
	selector := commandField(
		"--request-content-type",
		"request-content-type",
		contract.RequestMediaTypeWireName,
		contract.FieldLocationHeader,
	)
	selector.Binding.Required = false
	selector.Binding.Schema.Enum = []contract.PresentValue{
		contract.NewStringValue("application/json"),
		contract.NewStringValue("application/merge-patch+json"),
	}
	root := contract.Schema{
		Type:       contract.SchemaTypeObject,
		Required:   []string{"name"},
		Properties: map[string]contract.Schema{"name": {Type: contract.SchemaTypeString}},
	}
	jsonName := bodyCommandField("--json-name", "json-name", "name", true, contract.Schema{Type: contract.SchemaTypeString})
	mergeName := bodyCommandField("--merge-name", "merge-name", "name", true, contract.Schema{Type: contract.SchemaTypeString})
	snapshot := commandSnapshot([]pmapi.CommandFieldDescriptor{selector, jsonName, mergeName})
	snapshot.Commands[0].RequestBody = contract.OperationRequestProjection{
		RequestMediaFieldID: selector.Binding.FieldID,
		Variants: []contract.RequestVariantProjection{
			{
				VariantID: "json", MediaType: "application/json", Required: false,
				Schema: root, FieldIDs: []string{jsonName.Binding.FieldID},
			},
			{
				VariantID: "merge", MediaType: "application/merge-patch+json", Required: false,
				Schema: root, FieldIDs: []string{mergeName.Binding.FieldID},
			},
		},
	}

	for _, test := range []struct {
		name         string
		args         []string
		wantCode     int
		wantCalls    int
		wantField    string
		wantFieldSet bool
	}{
		{
			name:      "selected JSON variant",
			args:      []string{"--request-content-type", "application/json", "--json-name", "Ada"},
			wantField: jsonName.Binding.FieldID, wantFieldSet: true, wantCalls: 1,
		},
		{
			name:      "selected merge variant",
			args:      []string{"--request-content-type", "application/merge-patch+json", "--merge-name", "Grace"},
			wantField: mergeName.Binding.FieldID, wantFieldSet: true, wantCalls: 1,
		},
		{name: "optional body absent", wantCalls: 1},
		{
			name:     "selected variant missing required field",
			args:     []string{"--request-content-type", "application/json"},
			wantCode: 2,
		},
		{
			name:     "nonselected variant field",
			args:     []string{"--request-content-type", "application/json", "--merge-name", "Grace"},
			wantCode: 2,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			var calls int
			var captured DynamicInvocation
			var stderr bytes.Buffer
			args := append([]string{"inspect-widget"}, test.args...)
			code := Execute(t.Context(), Options{
				Args: args, Stderr: &stderr, Catalog: &snapshot,
				DynamicInvoke: func(_ context.Context, invocation DynamicInvocation) error {
					calls++
					captured = invocation
					return nil
				},
			})
			if code != test.wantCode || calls != test.wantCalls {
				t.Fatalf("variant result = code %d calls %d stderr %q; want code %d calls %d", code, calls, stderr.String(), test.wantCode, test.wantCalls)
			}
			if calls == 0 {
				return
			}
			fields, present := captured.Input.Fields()
			if !present {
				t.Fatalf("captured input = %#v; want field object", captured.Input)
			}
			_, fieldSet := fields[test.wantField]
			if fieldSet != test.wantFieldSet {
				t.Fatalf("captured fields = %#v; field %q presence = %t", fields, test.wantField, fieldSet)
			}
		})
	}
	t.Run("malformed variant membership refuses before invocation", func(t *testing.T) {
		malformed := commandSnapshot([]pmapi.CommandFieldDescriptor{selector, jsonName, mergeName})
		malformed.Commands[0].RequestBody = snapshot.Commands[0].RequestBody.Clone()
		malformed.Commands[0].RequestBody.Variants[0].FieldIDs = []string{mergeName.Binding.FieldID}
		var calls int
		code := Execute(t.Context(), Options{
			Args: []string{"inspect-widget"}, Catalog: &malformed,
			DynamicInvoke: func(context.Context, DynamicInvocation) error {
				calls++
				return nil
			},
		})
		if code != 2 || calls != 0 {
			t.Fatalf("malformed membership = code %d calls %d; want code 2 calls 0", code, calls)
		}
	})
}

func TestProjectedAllOfInputUsesSharedSemantics(t *testing.T) {
	t.Parallel()
	schema := contract.Schema{
		Type: contract.SchemaTypeObject,
		AllOf: []contract.Schema{
			{
				Type:       contract.SchemaTypeObject,
				Required:   []string{"kind"},
				Properties: map[string]contract.Schema{"kind": {Type: contract.SchemaTypeString}},
			},
			{
				Type:     contract.SchemaTypeObject,
				Required: []string{"active"},
				Properties: map[string]contract.Schema{
					"active": {Type: contract.SchemaTypeBool, Enum: []contract.PresentValue{contract.NewBooleanValue(true)}},
				},
			},
		},
	}
	field := commandField("--filter", "query.filter", "filter", contract.FieldLocationQuery)
	field.Binding.Schema = schema
	snapshot := commandSnapshot([]pmapi.CommandFieldDescriptor{field})
	for _, test := range []struct {
		name      string
		value     contract.PresentValue
		wantCode  int
		wantCalls int
	}{
		{
			name: "disjoint required properties",
			value: contract.NewObjectValue(map[string]contract.PresentValue{
				"kind": contract.NewStringValue("widget"), "active": contract.NewBooleanValue(true),
			}),
			wantCalls: 1,
		},
		{
			name: "unknown property refused",
			value: contract.NewObjectValue(map[string]contract.PresentValue{
				"kind": contract.NewStringValue("widget"), "active": contract.NewBooleanValue(true),
				"extra": contract.NewStringValue("refuse"),
			}),
			wantCode: 2,
		},
		{
			name: "branch constraint retained",
			value: contract.NewObjectValue(map[string]contract.PresentValue{
				"kind": contract.NewStringValue("widget"), "active": contract.NewBooleanValue(false),
			}),
			wantCode: 2,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			var calls int
			var stderr bytes.Buffer
			code := Execute(t.Context(), Options{
				Args:   []string{"inspect-widget", "--filter", encodeDynamicValue(t, test.value)},
				Stderr: &stderr, Catalog: &snapshot,
				DynamicInvoke: func(context.Context, DynamicInvocation) error {
					calls++
					return nil
				},
			})
			if code != test.wantCode || calls != test.wantCalls {
				t.Fatalf("allOf result = code %d calls %d stderr %q; want code %d calls %d", code, calls, stderr.String(), test.wantCode, test.wantCalls)
			}
		})
	}
}

func bodyCommandField(flag, fieldID, wireName string, required bool, schema contract.Schema) pmapi.CommandFieldDescriptor {
	return pmapi.CommandFieldDescriptor{
		Flag: flag,
		Binding: contract.FieldBinding{
			FieldID: fieldID, WireName: wireName, Location: contract.FieldLocationBody, JSONPointer: "/" + wireName,
			Required: required, Nullable: schema.Nullable, Style: "json", Explode: false, AllowReserved: false,
			Sensitivity: contract.SensitivityPublic, Schema: schema,
		},
	}
}

func encodeDynamicValue(t *testing.T, value contract.PresentValue) string {
	t.Helper()
	document, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode typed dynamic value: %v", err)
	}
	return string(document)
}
