package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
)

func TestMutationControlAllocatesAndRecoversSelectedRequest(t *testing.T) {
	t.Parallel()
	const (
		instanceID  = "instance-client-key-recovery"
		workspaceID = "workspace-client-key-recovery"
		profileName = "client-key-recovery"
	)
	directory := t.TempDir()
	profilesPath := filepath.Join(directory, "profiles.json")
	recoveryDirectory, err := pmapi.DefaultRecoveryDirectory(profilesPath)
	if err != nil {
		t.Fatalf("recovery directory: %v", err)
	}
	var refuse atomic.Bool
	refuse.Store(true)
	var mutationCalls atomic.Int32
	var keyMutex sync.Mutex
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
		case "/v1/workspaces/" + workspaceID + "/connections":
			if request.Method != http.MethodPost {
				http.NotFound(writer, request)
				return
			}
			entries, readErr := os.ReadDir(recoveryDirectory)
			if readErr != nil || len(entries) == 0 {
				t.Errorf("mutation send preceded recovery persistence: entries %d error %v", len(entries), readErr)
			}
			var body pmapi.ConnectionCreateRequest
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode connection mutation: %v", err)
			}
			requestKey := request.Header.Get("Idempotency-Key")
			if requestKey == "" || body.Protocol.RequestKey != requestKey {
				t.Errorf("request key header/body = %q/%q", requestKey, body.Protocol.RequestKey)
			}
			keyMutex.Lock()
			observedKeys = append(observedKeys, requestKey)
			keyMutex.Unlock()
			mutationCalls.Add(1)
			if refuse.Load() {
				writer.Header().Set("Content-Type", "application/problem+json")
				writer.Header().Set("Retry-After", "0")
				writer.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(writer).Encode(pmapi.Problem{
					Type:            "https://pm.invalid/problems/workspace-admission-count-limit",
					Title:           "Workspace admission limit",
					Status:          http.StatusTooManyRequests,
					Detail:          "The bounded workspace queue is full.",
					Code:            "workspace_admission_count_limit",
					IncidentId:      "incident-client-key-recovery",
					SafeRetryAction: "after_backlog_progress",
				})
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusCreated)
			_, _ = writer.Write([]byte(`{}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	handle, err := pmapi.DefaultSessionHandle(profileName, instanceID)
	if err != nil {
		t.Fatalf("session handle: %v", err)
	}
	output := pmapi.OutputJSON
	if err := pmapi.AddServerProfile(profilesPath, pmapi.ServerProfile{
		Name: profileName, ServerURL: server.URL, InstanceID: instanceID,
		SessionHandle: handle, Output: &output,
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
		PmSessionCredential: "pm-client-key-recovery-session",
		Principal:           pmapi.Principal{Kind: pmapi.PrincipalKindUser, PrincipalId: "ada-client-key-recovery"},
		SessionId:           "session-client-key-recovery",
	}); err != nil {
		t.Fatalf("store session: %v", err)
	}
	factory := func(mode pmapi.SessionStoreMode) (pmapi.SessionStore, error) {
		if mode != pmapi.SessionStoreMemory {
			t.Fatalf("session mode = %q, want memory", mode)
		}
		return sessions, nil
	}
	execute := func(displayName string, extra ...string) (int, string, string) {
		args := []string{
			"connections", "create",
			"--server-profile", profileName,
			"--workspace", workspaceID,
			"--session-store", "memory",
			"--output", "json",
			"--revision", "1",
			"--connector-id", "github",
			"--display-name", displayName,
			"--provider-account-id", "provider-account-client-key-recovery",
		}
		args = append(args, extra...)
		var stdout, stderr bytes.Buffer
		code := Execute(t.Context(), Options{
			Args: args, Stdout: &stdout, Stderr: &stderr, ProfilesPath: profilesPath,
			HTTPClient: server.Client(), SessionStore: factory,
		})
		return code, stdout.String(), stderr.String()
	}

	code, _, failure := execute("original display")
	if code != 5 {
		t.Fatalf("automatic-key refusal = exit %d stderr %q, want dependency refusal", code, failure)
	}
	keyMutex.Lock()
	firstKeys := append([]string(nil), observedKeys...)
	keyMutex.Unlock()
	if len(firstKeys) != 3 || firstKeys[0] == "" || firstKeys[1] != firstKeys[0] || firstKeys[2] != firstKeys[0] {
		t.Fatalf("bounded automatic-key sends = %#v, want one retained key across three sends", firstKeys)
	}
	firstKey := firstKeys[0]
	if !strings.Contains(failure, "--request-key="+strconv.Quote(firstKey)) || !strings.Contains(failure, "unchanged retained request") {
		t.Fatalf("automatic-key recovery guidance = %q, want exact retained key", failure)
	}
	unknownCode, unknownGuidance := classifyCommandError(mutationRecoveryError(firstKey, pmapi.ErrMutationOutcomeUnknown))
	if unknownCode != 7 || !strings.Contains(unknownGuidance, "--request-key="+strconv.Quote(firstKey)) ||
		!strings.Contains(unknownGuidance, "outcome is unknown") {
		t.Fatalf("uncertain-send guidance = exit %d message %q, want exact retained key and unknown outcome", unknownCode, unknownGuidance)
	}

	refuse.Store(false)
	code, _, failure = execute("original display", "--request-key", firstKey)
	if code != 0 {
		t.Fatalf("explicit retained request = exit %d stderr %q", code, failure)
	}
	keyMutex.Lock()
	selectedKey := observedKeys[len(observedKeys)-1]
	keyMutex.Unlock()
	if selectedKey != firstKey {
		t.Fatalf("explicit recovery selected key %q, want %q", selectedKey, firstKey)
	}

	callsBeforeConflict := mutationCalls.Load()
	code, _, failure = execute("changed display", "--request-key", firstKey)
	if code != 4 || mutationCalls.Load() != callsBeforeConflict ||
		!strings.Contains(failure, "another canonical intent") || !strings.Contains(failure, "omit --request-key") {
		t.Fatalf("changed retained intent = exit %d calls %d/%d stderr %q", code, mutationCalls.Load(), callsBeforeConflict, failure)
	}

	code, _, failure = execute("original display")
	if code != 0 {
		t.Fatalf("later equal command = exit %d stderr %q", code, failure)
	}
	keyMutex.Lock()
	freshKey := observedKeys[len(observedKeys)-1]
	keyMutex.Unlock()
	if freshKey == firstKey || freshKey == "" {
		t.Fatalf("later equal command silently replayed key %q", freshKey)
	}
	entries, err := os.ReadDir(recoveryDirectory)
	if err != nil || len(entries) != 2 {
		t.Fatalf("scoped recovery records = %d error %v, want two distinct request keys", len(entries), err)
	}
}
