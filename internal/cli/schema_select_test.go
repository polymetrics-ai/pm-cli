package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
)

const (
	schemaSelectInstance   = "instance-schema-select"
	schemaSelectWorkspace  = "workspace-schema-select"
	schemaSelectProfile    = "schema-select"
	schemaSelectConnection = "connection-schema-select"
	schemaSelectResource   = "customer.records"
	schemaSelectReference  = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

// Check: pm schema select refuses each missing input with exit code 2 and one
// exact stderr line, before it sends any request (S5 for --revision). Defect
// it catches: a missing --revision that reaches the server, or another line.
func TestSchemaSelectRefusesMissingInputBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	complete := map[string]string{
		"--connection": schemaSelectConnection,
		"--resource":   schemaSelectResource,
		"--schema-ref": schemaSelectReference,
		"--revision":   "1",
	}
	for _, missing := range []string{"--connection", "--resource", "--schema-ref", "--revision"} {
		t.Run(missing, func(t *testing.T) {
			args := []string{"schema", "select"}
			for _, flag := range []string{"--connection", "--resource", "--schema-ref", "--revision"} {
				if flag != missing {
					args = append(args, flag, complete[flag])
				}
			}
			doer := &refusingDoer{t: t}
			var stdout, stderr bytes.Buffer
			code := Execute(t.Context(), Options{
				Args: args, Stdout: &stdout, Stderr: &stderr,
				ProfilesPath: filepath.Join(t.TempDir(), "profiles.json"), HTTPClient: doer,
			})
			want := "error: " + missing + " is required\n"
			if code != 2 || stderr.String() != want || stdout.Len() != 0 {
				t.Fatalf("exit %d stdout %q stderr %q; want exit 2 and stderr %q", code, stdout.String(), stderr.String(), want)
			}
			if doer.calls != 0 {
				t.Fatalf("usage refusal sent %d requests; want none", doer.calls)
			}
		})
	}
}

// schemaSelectServer is a test PM server for the select route. It records each
// select request and answers with the configured status and body.
type schemaSelectServer struct {
	server   *httptest.Server
	mutex    sync.Mutex
	requests []recordedSchemaSelect
	status   int
	body     any
}

type recordedSchemaSelect struct {
	method, path, ifMatch, idempotencyKey string
	body                                  map[string]any
}

func newSchemaSelectServer(t *testing.T, status int, body any) *schemaSelectServer {
	t.Helper()
	fixture := &schemaSelectServer{status: status, body: body}
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/info" {
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(pmapi.ServerInfo{
				ProtocolMajor:    pmapi.ProtocolMajor,
				ProtocolVersion:  pmapi.ServerInfoProtocolVersion(pmapi.WireVersion),
				InstanceId:       schemaSelectInstance,
				Features:         []string{"exact-values", "wire-problem-v1"},
				RequiredFeatures: []string{"exact-values"},
			})
			return
		}
		var decoded map[string]any
		if err := json.NewDecoder(request.Body).Decode(&decoded); err != nil {
			t.Errorf("decode select request: %v", err)
		}
		fixture.mutex.Lock()
		fixture.requests = append(fixture.requests, recordedSchemaSelect{
			method: request.Method, path: request.URL.Path, ifMatch: request.Header.Get("If-Match"),
			idempotencyKey: request.Header.Get("Idempotency-Key"), body: decoded,
		})
		fixture.mutex.Unlock()
		contentType := "application/json"
		if fixture.status >= 400 {
			contentType = "application/problem+json"
		}
		writer.Header().Set("Content-Type", contentType)
		writer.WriteHeader(fixture.status)
		_ = json.NewEncoder(writer).Encode(fixture.body)
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (fixture *schemaSelectServer) run(t *testing.T, revision string) (int, string, string) {
	t.Helper()
	directory := t.TempDir()
	profilesPath := filepath.Join(directory, "profiles.json")
	handle, err := pmapi.DefaultSessionHandle(schemaSelectProfile, schemaSelectInstance)
	if err != nil {
		t.Fatalf("session handle: %v", err)
	}
	output := pmapi.OutputJSON
	if err := pmapi.AddServerProfile(profilesPath, pmapi.ServerProfile{
		Name: schemaSelectProfile, ServerURL: fixture.server.URL, InstanceID: schemaSelectInstance,
		SessionHandle: handle, Output: &output,
	}); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	sessions := pmapi.NewMemorySessionStore()
	sessionKey, err := pmapi.NewSessionKey(schemaSelectInstance, handle)
	if err != nil {
		t.Fatalf("session key: %v", err)
	}
	if err := sessions.Store(t.Context(), sessionKey, pmapi.PMDeviceSession{
		ExpiresAt:           time.Now().Add(time.Hour),
		PmSessionCredential: "pm-schema-select-session",
		Principal:           pmapi.Principal{Kind: pmapi.PrincipalKindUser, PrincipalId: "ada-schema-select"},
		SessionId:           "session-schema-select",
	}); err != nil {
		t.Fatalf("store session: %v", err)
	}
	args := []string{
		"schema", "select",
		"--server-profile", schemaSelectProfile,
		"--workspace", schemaSelectWorkspace,
		"--session-store", "memory",
		"--output", "json",
		"--connection", schemaSelectConnection,
		"--resource", schemaSelectResource,
		"--schema-ref", schemaSelectReference,
		"--revision", revision,
	}
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), Options{
		Args: args, Stdout: &stdout, Stderr: &stderr, ProfilesPath: profilesPath,
		HTTPClient: fixture.server.Client(),
		SessionStore: func(pmapi.SessionStoreMode) (pmapi.SessionStore, error) {
			return sessions, nil
		},
	})
	return code, stdout.String(), stderr.String()
}

// Check: pm schema select sends one POST to the select route with the
// revision in If-Match and in the protocol, the request key in the header and
// the body, and only the resource and the schema reference in the selection.
// Defect it catches: a selection that sends no revision, another route, or
// another selection intent.
func TestSchemaSelectSendsTheRevisionedSelection(t *testing.T) {
	t.Parallel()
	server := newSchemaSelectServer(t, http.StatusOK, map[string]any{
		"resource_id":        schemaSelectConnection,
		"result_revision":    "2",
		"outcome_reference":  map[string]any{"kind": "connection", "ref": schemaSelectConnection},
		"result_disposition": "created",
		"replay":             false,
	})
	code, stdout, stderr := server.run(t, "1")
	var rendered map[string]any
	if code != 0 || stderr != "" || json.Unmarshal([]byte(stdout), &rendered) != nil ||
		rendered["result_disposition"] != "created" || rendered["result_revision"] != "2" {
		t.Fatalf("exit %d stdout %q stderr %q; want exit 0 and the selection result", code, stdout, stderr)
	}
	server.mutex.Lock()
	defer server.mutex.Unlock()
	if len(server.requests) != 1 {
		t.Fatalf("select requests = %d; want 1", len(server.requests))
	}
	sent := server.requests[0]
	wantPath := "/v1/workspaces/" + schemaSelectWorkspace + "/connections/" + schemaSelectConnection + "/schema/selections"
	if sent.method != http.MethodPost || sent.path != wantPath || sent.ifMatch != "1" || sent.idempotencyKey == "" {
		t.Fatalf("request = %s %s If-Match %q key %q; want POST %s with If-Match 1 and a key",
			sent.method, sent.path, sent.ifMatch, sent.idempotencyKey, wantPath)
	}
	protocol, _ := sent.body["protocol"].(map[string]any)
	if protocol["request_key"] != sent.idempotencyKey || protocol["expected_revision"] != "1" {
		t.Fatalf("protocol = %#v; want the header key and expected revision 1", protocol)
	}
	selection, _ := sent.body["selection"].(map[string]any)
	if len(sent.body) != 2 || len(selection) != 2 || selection["resource"] != schemaSelectResource ||
		selection["schema_ref"] != schemaSelectReference {
		t.Fatalf("body = %#v; want the protocol and a selection of the resource and the reference only", sent.body)
	}
}

// Check: a stale revision answer (412 control_revision_mismatch) gives exit
// code 4, and a resource that the connection does not declare (404
// schema_relation_unknown) gives exit code 3. Each stderr line starts with the
// problem code (S2, S3, S4). Defect it catches: another exit code or a hidden
// problem code.
func TestSchemaSelectMapsRefusalsToExitCodes(t *testing.T) {
	t.Parallel()
	for _, refusal := range []struct {
		status int
		code   string
		action string
		exit   int
	}{
		{http.StatusPreconditionFailed, "control_revision_mismatch", "refresh_control_revision", 4},
		{http.StatusNotFound, "schema_relation_unknown", "correct_input", 3},
	} {
		t.Run(refusal.code, func(t *testing.T) {
			t.Parallel()
			server := newSchemaSelectServer(t, refusal.status, pmapi.Problem{
				Type:            "https://pm.invalid/problems/" + refusal.code,
				Title:           "Refused",
				Status:          refusal.status,
				Detail:          "The selection was refused.",
				Code:            refusal.code,
				IncidentId:      "incident-schema-select",
				SafeRetryAction: refusal.action,
			})
			code, stdout, stderr := server.run(t, "1")
			if code != refusal.exit || stdout != "" || !strings.HasPrefix(stderr, "error: "+refusal.code+":") ||
				strings.Count(stderr, "\n") != 1 {
				t.Fatalf("exit %d stdout %q stderr %q; want exit %d and one line that starts with the problem code",
					code, stdout, stderr, refusal.exit)
			}
		})
	}
}
