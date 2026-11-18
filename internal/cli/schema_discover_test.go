package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

const (
	discoverPeerInstanceID   = "instance-discovery-recovery"
	discoverPeerWorkspaceID  = "workspace-discovery-recovery"
	discoverPeerConnectionID = "connection-discovery-recovery"
	discoverPeerProfileName  = "discovery-recovery"
)

// newDiscoverPeer starts a controlled TLS peer that answers the server info
// route and passes each discovery admission to discoveries. Its cleanup closes
// the peer; it is registered before any step that can fail. The returned
// function runs pm schema discover against the peer with the given context and
// extra arguments.
func newDiscoverPeer(t *testing.T, discoveries http.HandlerFunc) func(context.Context, ...string) (int, string, string) {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/info":
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(pmapi.ServerInfo{
				ProtocolMajor:    pmapi.ProtocolMajor,
				ProtocolVersion:  pmapi.ServerInfoProtocolVersion(pmapi.WireVersion),
				InstanceId:       discoverPeerInstanceID,
				Features:         []string{"exact-values", "wire-problem-v1"},
				RequiredFeatures: []string{"exact-values"},
			})
		case "/v1/workspaces/" + discoverPeerWorkspaceID + "/connections/" + discoverPeerConnectionID + "/schema/discoveries":
			if request.Method != http.MethodPost {
				http.NotFound(writer, request)
				return
			}
			discoveries(writer, request)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)

	profilesPath := filepath.Join(t.TempDir(), "profiles.json")
	handle, err := pmapi.DefaultSessionHandle(discoverPeerProfileName, discoverPeerInstanceID)
	if err != nil {
		t.Fatalf("session handle: %v", err)
	}
	if err := pmapi.AddServerProfile(profilesPath, pmapi.ServerProfile{
		Name: discoverPeerProfileName, ServerURL: server.URL, InstanceID: discoverPeerInstanceID, SessionHandle: handle,
	}); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	sessions := pmapi.NewMemorySessionStore()
	sessionKey, err := pmapi.NewSessionKey(discoverPeerInstanceID, handle)
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
	return func(ctx context.Context, extra ...string) (int, string, string) {
		args := append([]string{
			"schema", "discover",
			"--server-profile", discoverPeerProfileName,
			"--workspace", discoverPeerWorkspaceID,
			"--session-store", "memory",
			"--output", "json",
			"--connection", discoverPeerConnectionID,
			"--resource", "customer.records",
		}, extra...)
		var stdout, stderr bytes.Buffer
		code := Execute(ctx, Options{
			Args: args, Stdout: &stdout, Stderr: &stderr, ProfilesPath: profilesPath,
			HTTPClient: server.Client(), SessionStore: factory,
		})
		return code, stdout.String(), stderr.String()
	}
}

// TestSchemaDiscoverReportsAnUnknownAdmissionWithItsRequestKey is the
// developer check of #301 review finding D3. Defect: pm schema discover sends
// its admission once with a new request key that it never prints. When the
// server admits the discovery and the response is lost, the command reports a
// transport failure, the caller cannot replay the admission, and a new run of
// the command admits a second discovery. The controlled peer below admits the
// discovery and then closes the connection with no response.
func TestSchemaDiscoverReportsAnUnknownAdmissionWithItsRequestKey(t *testing.T) {
	t.Parallel()
	var dropResponse atomic.Bool
	dropResponse.Store(true)
	var admittedMutex sync.Mutex
	admitted := map[string]string{}
	var observedKeys []string
	execute := newDiscoverPeer(t, func(writer http.ResponseWriter, request *http.Request) {
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
	})

	code, stdout, stderr := execute(t.Context())
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
	code, stdout, stderr = execute(t.Context(), "--request-key", firstKey)
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

// TestSchemaDiscoverNamesTheRequestKeyForEveryUnknownAdmission is the
// developer check of #301 review round 2 (raman A and B on 3ec9b25). Defect:
// only a transport failure named the request key. A server-reported unknown
// outcome (503 admission_outcome_unknown) exited 7 with no key, and a send that
// ended by the request deadline or by an interrupt exited 8 or 6 with no key,
// although each can follow a committed admission.
func TestSchemaDiscoverNamesTheRequestKeyForEveryUnknownAdmission(t *testing.T) {
	t.Parallel()
	unknownOutcome := func(writer http.ResponseWriter, request *http.Request) {
		requestKey := request.Header.Get("Idempotency-Key")
		writer.Header().Set("Content-Type", "application/problem+json")
		writer.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(writer).Encode(pmapi.Problem{
			Type:            "https://polymetrics.ai/problems/admission_outcome_unknown",
			Title:           "Outcome unknown",
			Status:          http.StatusServiceUnavailable,
			Detail:          "Reconcile the original request before attempting another mutation.",
			Code:            "admission_outcome_unknown",
			IncidentId:      "incident-discovery-recovery",
			SafeRetryAction: "same_key_reconcile_only",
			OutcomeReference: &pmapi.SafeOutcomeReference{
				Kind: pmapi.SafeOutcomeReferenceKindRequest,
				Ref:  requestKey,
			},
		})
	}
	// heldUntilTheClientEnds holds the admission until the client gives up the
	// request, so the command ends during the send. The handler reads the whole
	// body first: only then does the server watch the connection, so the
	// request context ends when the client closes it. The handler then
	// returns, and the peer cleanup waits for it.
	heldUntilTheClientEnds := func(before func()) http.HandlerFunc {
		return func(writer http.ResponseWriter, request *http.Request) {
			_, _ = io.Copy(io.Discard, request.Body)
			before()
			<-request.Context().Done()
		}
	}
	cases := []struct {
		name string
		run  func(t *testing.T) (int, string, string, string)
	}{
		{
			name: "A, a server-reported unknown outcome without --wait",
			run: func(t *testing.T) (int, string, string, string) {
				var key atomic.Value
				execute := newDiscoverPeer(t, func(writer http.ResponseWriter, request *http.Request) {
					key.Store(request.Header.Get("Idempotency-Key"))
					unknownOutcome(writer, request)
				})
				code, stdout, stderr := execute(t.Context())
				sent, _ := key.Load().(string)
				return code, stdout, stderr, sent
			},
		},
		{
			name: "A, a server-reported unknown outcome with --wait",
			run: func(t *testing.T) (int, string, string, string) {
				var key atomic.Value
				execute := newDiscoverPeer(t, func(writer http.ResponseWriter, request *http.Request) {
					key.Store(request.Header.Get("Idempotency-Key"))
					unknownOutcome(writer, request)
				})
				code, stdout, stderr := execute(t.Context(), "--wait=true")
				sent, _ := key.Load().(string)
				return code, stdout, stderr, sent
			},
		},
		{
			name: "B1, the request deadline during the send",
			run: func(t *testing.T) (int, string, string, string) {
				var key atomic.Value
				execute := newDiscoverPeer(t, func(writer http.ResponseWriter, request *http.Request) {
					heldUntilTheClientEnds(func() { key.Store(request.Header.Get("Idempotency-Key")) })(writer, request)
				})
				code, stdout, stderr := execute(t.Context(), "--request-timeout", "2s")
				sent, _ := key.Load().(string)
				return code, stdout, stderr, sent
			},
		},
		{
			name: "B2, an interrupt during the send",
			run: func(t *testing.T) (int, string, string, string) {
				ctx, interrupt := context.WithCancel(t.Context())
				t.Cleanup(interrupt)
				var key atomic.Value
				execute := newDiscoverPeer(t, func(writer http.ResponseWriter, request *http.Request) {
					heldUntilTheClientEnds(func() {
						key.Store(request.Header.Get("Idempotency-Key"))
						interrupt()
					})(writer, request)
				})
				code, stdout, stderr := execute(ctx)
				sent, _ := key.Load().(string)
				return code, stdout, stderr, sent
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			code, stdout, stderr, sentKey := test.run(t)
			if sentKey == "" {
				t.Fatalf("the peer received no discovery admission; exit %d stderr %q", code, stderr)
			}
			if code != 7 || stdout != "" || !strings.Contains(stderr, "--request-key="+strconv.Quote(sentKey)) {
				t.Errorf("exit %d stdout %q stderr %q; want exit 7 and --request-key=%q", code, stdout, stderr, sentKey)
			}
		})
	}
}
