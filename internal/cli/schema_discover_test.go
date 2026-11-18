package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
)

// TestSchemaDiscoverReportsAnUnknownAdmissionWithItsRequestKey is the
// developer check of #301 review finding D3. Defect: pm schema discover sends
// its admission once with a new request key that it never prints. When the
// server admits the discovery and the response is lost, the command reports a
// transport failure, the caller cannot replay the admission, and a new run of
// the command admits a second discovery. The controlled peer below admits the
// discovery and then closes the connection with no response.
func TestSchemaDiscoverReportsAnUnknownAdmissionWithItsRequestKey(t *testing.T) {
	t.Parallel()
	const (
		instanceID   = "instance-discovery-recovery"
		workspaceID  = "workspace-discovery-recovery"
		connectionID = "connection-discovery-recovery"
		profileName  = "discovery-recovery"
	)
	var dropResponse atomic.Bool
	dropResponse.Store(true)
	var admittedMutex sync.Mutex
	admitted := map[string]string{}
	var observedKeys []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/info":
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(pmapi.ServerInfo{
				ProtocolMajor:    pmapi.ProtocolMajor,
				ProtocolVersion:  pmapi.ServerInfoProtocolVersion(pmapi.WireVersion),
				InstanceId:       instanceID,
				Features:         []string{"exact-values", "wire-problem-v1"},
				RequiredFeatures: []string{"exact-values"},
			})
		case "/v1/workspaces/" + workspaceID + "/connections/" + connectionID + "/schema/discoveries":
			if request.Method != http.MethodPost {
				http.NotFound(writer, request)
				return
			}
			var body pmapi.SchemaDiscoveryRequest
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.Resource != "customer.records" {
				t.Errorf("discovery request body = %#v (%v); want the resource customer.records", body, err)
			}
			requestKey := request.Header.Get("Idempotency-Key")
			admittedMutex.Lock()
			observedKeys = append(observedKeys, requestKey)
			runRef, replay := admitted[requestKey]
			if !replay {
				runRef = "run-discovery-" + strconv.Itoa(len(admitted)+1)
				admitted[requestKey] = runRef
			}
			admittedMutex.Unlock()
			if dropResponse.Load() {
				// The admission is recorded above. The response is lost: the
				// connection closes before any status line.
				hijacker, ok := writer.(http.Hijacker)
				if !ok {
					t.Errorf("the controlled peer cannot drop the response")
					return
				}
				connection, _, err := hijacker.Hijack()
				if err != nil {
					t.Errorf("drop the discovery response: %v", err)
					return
				}
				_ = connection.Close()
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(writer).Encode(pmapi.AdmissionResponse{
				IntentDigest: "sha256:" + strings.Repeat("b", 64),
				OutcomeReference: pmapi.SafeOutcomeReference{
					Kind: pmapi.SafeOutcomeReferenceKindRun,
					Ref:  runRef,
				},
				Replay:     replay,
				RequestKey: requestKey,
				RunRef:     runRef,
				State:      pmapi.AdmissionResponseStateAccepted,
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	profilesPath := filepath.Join(t.TempDir(), "profiles.json")
	handle, err := pmapi.DefaultSessionHandle(profileName, instanceID)
	if err != nil {
		t.Fatalf("session handle: %v", err)
	}
	if err := pmapi.AddServerProfile(profilesPath, pmapi.ServerProfile{
		Name: profileName, ServerURL: server.URL, InstanceID: instanceID, SessionHandle: handle,
	}); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	sessions := pmapi.NewMemorySessionStore()
	sessionKey, err := pmapi.NewSessionKey(instanceID, handle)
	if err != nil {
		t.Fatalf("session key: %v", err)
	}
	if err := sessions.Store(t.Context(), sessionKey, pmapi.PMDeviceSession{
		ExpiresAt:           time.Now().Add(time.Hour),
		PmSessionCredential: "pm-discovery-recovery-session",
		Principal:           pmapi.Principal{Kind: pmapi.PrincipalKindUser, PrincipalId: "ada-discovery-recovery"},
		SessionId:           "session-discovery-recovery",
	}); err != nil {
		t.Fatalf("store session: %v", err)
	}
	factory := func(mode pmapi.SessionStoreMode) (pmapi.SessionStore, error) {
		if mode != pmapi.SessionStoreMemory {
			t.Errorf("session mode = %q, want memory", mode)
		}
		return sessions, nil
	}
	execute := func(extra ...string) (int, string, string) {
		args := append([]string{
			"schema", "discover",
			"--server-profile", profileName,
			"--workspace", workspaceID,
			"--session-store", "memory",
			"--output", "json",
			"--connection", connectionID,
			"--resource", "customer.records",
		}, extra...)
		var stdout, stderr bytes.Buffer
		code := Execute(t.Context(), Options{
			Args: args, Stdout: &stdout, Stderr: &stderr, ProfilesPath: profilesPath,
			HTTPClient: server.Client(), SessionStore: factory,
		})
		return code, stdout.String(), stderr.String()
	}

	code, stdout, stderr := execute()
	admittedMutex.Lock()
	firstKeys := append([]string(nil), observedKeys...)
	firstAdmitted := len(admitted)
	admittedMutex.Unlock()
	if len(firstKeys) == 0 || firstAdmitted != 1 {
		t.Fatalf("lost-response admission: keys %#v and %d admitted runs; want one admitted discovery", firstKeys, firstAdmitted)
	}
	firstKey := firstKeys[0]
	for _, key := range firstKeys {
		if key == "" || key != firstKey {
			t.Fatalf("lost-response sends used keys %#v; want one request key", firstKeys)
		}
	}
	if code != 7 || stdout != "" ||
		!strings.Contains(stderr, "outcome is unknown") ||
		!strings.Contains(stderr, "--request-key="+strconv.Quote(firstKey)) {
		t.Fatalf("lost-response discovery = exit %d stdout %q stderr %q; want exit 7 with the unknown outcome and --request-key=%q",
			code, stdout, stderr, firstKey)
	}

	dropResponse.Store(false)
	code, stdout, stderr = execute("--request-key", firstKey)
	if code != 0 {
		t.Fatalf("replay with the named key = exit %d stderr %q; want exit 0", code, stderr)
	}
	var replayed pmapi.AdmissionResponse
	if err := json.Unmarshal([]byte(stdout), &replayed); err != nil {
		t.Fatalf("decode the replayed admission %q: %v", stdout, err)
	}
	admittedMutex.Lock()
	admittedRuns := len(admitted)
	firstRun := admitted[firstKey]
	admittedMutex.Unlock()
	if replayed.RunRef != firstRun || !replayed.Replay || admittedRuns != 1 {
		t.Fatalf("replay with the named key = run %q replay %t, %d admitted runs; want run %q replayed and one admitted run",
			replayed.RunRef, replayed.Replay, admittedRuns, firstRun)
	}
}
