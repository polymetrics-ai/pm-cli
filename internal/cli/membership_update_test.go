package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
)

// refusingDoer fails the test on any request. A usage refusal must send none.
type refusingDoer struct {
	t     *testing.T
	mutex sync.Mutex
	calls int
}

func (doer *refusingDoer) Do(request *http.Request) (*http.Response, error) {
	doer.mutex.Lock()
	doer.calls++
	doer.mutex.Unlock()
	doer.t.Errorf("usage refusal sent %s %s", request.Method, request.URL.Path)
	return nil, errors.New("no request is allowed")
}

// Check: pm access memberships update refuses a missing input with exit code 2
// and one exact stderr line, before it sends any request (M3). Defect it
// catches: a missing --revision that reaches the server, or another line.
func TestMembershipUpdateRefusesMissingRevisionBeforeAnyRequest(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "no revision",
			args: []string{"access", "memberships", "update", "membership-1", "--add-connections", "connection-1"},
			want: "error: --revision is required\n",
		},
		{
			name: "no connections",
			args: []string{"access", "memberships", "update", "membership-1", "--revision", "2"},
			want: "error: --add-connections is required\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			doer := &refusingDoer{t: t}
			var stdout, stderr bytes.Buffer
			code := Execute(t.Context(), Options{
				Args: test.args, Stdout: &stdout, Stderr: &stderr,
				ProfilesPath: filepath.Join(t.TempDir(), "profiles.json"), HTTPClient: doer,
			})
			if code != 2 || stderr.String() != test.want || stdout.Len() != 0 {
				t.Fatalf("exit %d stdout %q stderr %q; want exit 2 and stderr %q", code, stdout.String(), stderr.String(), test.want)
			}
			if doer.calls != 0 {
				t.Fatalf("usage refusal sent %d requests; want none", doer.calls)
			}
		})
	}
}

// membershipUpdateServer is a test PM server for the update route. It records
// the update request and answers with the configured status and body.
type membershipUpdateServer struct {
	server   *httptest.Server
	mutex    sync.Mutex
	requests []recordedMembershipUpdate
	status   int
	body     any
}

type recordedMembershipUpdate struct {
	method, path, ifMatch, idempotencyKey string
	body                                  map[string]any
}

const (
	membershipUpdateInstance  = "instance-membership-update"
	membershipUpdateWorkspace = "workspace-membership-update"
	membershipUpdateProfile   = "membership-update"
	membershipUpdateRef       = "membership-admin"
)

func newMembershipUpdateServer(t *testing.T, status int, body any) *membershipUpdateServer {
	t.Helper()
	fixture := &membershipUpdateServer{status: status, body: body}
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/info" {
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(pmapi.ServerInfo{
				ProtocolMajor:    pmapi.ProtocolMajor,
				ProtocolVersion:  pmapi.ServerInfoProtocolVersion(pmapi.WireVersion),
				InstanceId:       membershipUpdateInstance,
				Features:         []string{"exact-values", "wire-problem-v1"},
				RequiredFeatures: []string{"exact-values"},
			})
			return
		}
		var decoded map[string]any
		if err := json.NewDecoder(request.Body).Decode(&decoded); err != nil {
			t.Errorf("decode membership update request: %v", err)
		}
		fixture.mutex.Lock()
		fixture.requests = append(fixture.requests, recordedMembershipUpdate{
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

func (fixture *membershipUpdateServer) run(t *testing.T, extra ...string) (int, string, string) {
	t.Helper()
	directory := t.TempDir()
	profilesPath := filepath.Join(directory, "profiles.json")
	handle, err := pmapi.DefaultSessionHandle(membershipUpdateProfile, membershipUpdateInstance)
	if err != nil {
		t.Fatalf("session handle: %v", err)
	}
	output := pmapi.OutputJSON
	if err := pmapi.AddServerProfile(profilesPath, pmapi.ServerProfile{
		Name: membershipUpdateProfile, ServerURL: fixture.server.URL, InstanceID: membershipUpdateInstance,
		SessionHandle: handle, Output: &output,
	}); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	sessions := pmapi.NewMemorySessionStore()
	sessionKey, err := pmapi.NewSessionKey(membershipUpdateInstance, handle)
	if err != nil {
		t.Fatalf("session key: %v", err)
	}
	if err := sessions.Store(t.Context(), sessionKey, pmapi.PMDeviceSession{
		ExpiresAt:           time.Now().Add(time.Hour),
		PmSessionCredential: "pm-membership-update-session",
		Principal:           pmapi.Principal{Kind: pmapi.PrincipalKindUser, PrincipalId: "ada-membership-update"},
		SessionId:           "session-membership-update",
	}); err != nil {
		t.Fatalf("store session: %v", err)
	}
	args := append([]string{
		"access", "memberships", "update", membershipUpdateRef,
		"--server-profile", membershipUpdateProfile,
		"--workspace", membershipUpdateWorkspace,
		"--session-store", "memory",
		"--output", "json",
	}, extra...)
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

// Check: pm access memberships update sends one PATCH to the membership with
// the revision in If-Match and in the protocol, the request key in the header
// and the body, and the connections sorted. Defect it catches: an update that
// sends no revision, another route, or an unsorted intent.
func TestMembershipUpdateSendsTheRevisionedIntent(t *testing.T) {
	t.Parallel()
	server := newMembershipUpdateServer(t, http.StatusOK, map[string]any{
		"resource_id":        membershipUpdateRef,
		"result_revision":    "4",
		"outcome_reference":  map[string]any{"kind": "membership", "ref": membershipUpdateRef},
		"result_disposition": "updated",
		"replay":             false,
	})
	code, stdout, stderr := server.run(t, "--add-connections", "connection-b,connection-a", "--revision", "3")
	var rendered map[string]any
	if code != 0 || stderr != "" || json.Unmarshal([]byte(stdout), &rendered) != nil ||
		rendered["result_disposition"] != "updated" || rendered["result_revision"] != "4" {
		t.Fatalf("exit %d stdout %q stderr %q; want exit 0 and the update result", code, stdout, stderr)
	}
	server.mutex.Lock()
	defer server.mutex.Unlock()
	if len(server.requests) != 1 {
		t.Fatalf("update requests = %d; want 1", len(server.requests))
	}
	sent := server.requests[0]
	wantPath := "/v1/workspaces/" + membershipUpdateWorkspace + "/memberships/" + membershipUpdateRef
	if sent.method != http.MethodPatch || sent.path != wantPath || sent.ifMatch != "3" || sent.idempotencyKey == "" {
		t.Fatalf("request = %s %s If-Match %q key %q; want PATCH %s with If-Match 3 and a key",
			sent.method, sent.path, sent.ifMatch, sent.idempotencyKey, wantPath)
	}
	protocol, _ := sent.body["protocol"].(map[string]any)
	if protocol["request_key"] != sent.idempotencyKey || protocol["expected_revision"] != "3" {
		t.Fatalf("protocol = %#v; want the header key and expected revision 3", protocol)
	}
	membership, _ := sent.body["membership"].(map[string]any)
	connections, _ := membership["add_connection_ids"].([]any)
	if len(membership) != 2 || membership["membership_ref"] != membershipUpdateRef ||
		!slices.Equal(connections, []any{"connection-a", "connection-b"}) {
		t.Fatalf("membership = %#v; want the reference and the sorted connections only", membership)
	}
}

// Check: a stale revision answer (412 control_revision_mismatch) gives exit
// code 4 and a stderr line that starts with the problem code (M2). Defect it
// catches: another exit code or a hidden problem code.
func TestMembershipUpdateMapsStaleRevisionToExitFour(t *testing.T) {
	t.Parallel()
	server := newMembershipUpdateServer(t, http.StatusPreconditionFailed, pmapi.Problem{
		Type:            "https://pm.invalid/problems/control-revision-mismatch",
		Title:           "Control revision mismatch",
		Status:          http.StatusPreconditionFailed,
		Detail:          "The supplied control revision is stale.",
		Code:            "control_revision_mismatch",
		IncidentId:      "incident-membership-update",
		SafeRetryAction: "refresh_control_revision",
	})
	code, stdout, stderr := server.run(t, "--add-connections", "connection-a", "--revision", "1")
	if code != 4 || stdout != "" || !strings.HasPrefix(stderr, "error: control_revision_mismatch:") ||
		strings.Count(stderr, "\n") != 1 {
		t.Fatalf("exit %d stdout %q stderr %q; want exit 4 and one line that starts with the problem code", code, stdout, stderr)
	}
}
