package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oapi-codegen/nullable"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestNegotiatedCompatibility(t *testing.T) {
	t.Parallel()
	const instanceID = "instance-v79"
	var submissions atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/info":
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(ServerInfo{
				ProtocolMajor:    ProtocolMajor,
				ProtocolVersion:  ServerInfoProtocolVersion(WireVersion),
				InstanceId:       instanceID,
				Features:         []string{"exact-values", "wire-problem-v1"},
				RequiredFeatures: []string{"exact-values"},
			})
		case "/v1/workspaces/workspace-v79/invocations":
			submissions.Add(1)
			writer.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	profile := ServerProfile{Name: "v79", ServerURL: server.URL, InstanceID: instanceID, SessionHandle: "session-v79"}

	if _, err := NewQualifiedClient(context.Background(), profile, QualifiedClientOptions{HTTPClient: server.Client()}); err != nil {
		t.Fatalf("current API1 client qualification: %v", err)
	}
	priorMinor := ClientCompatibility{ProtocolMajor: ProtocolMajor, SupportedFeatures: []string{"exact-values"}}
	if _, err := NewQualifiedClient(context.Background(), profile, QualifiedClientOptions{HTTPClient: server.Client(), Compatibility: priorMinor}); err != nil {
		t.Fatalf("prior-minor API1 feature qualification: %v", err)
	}

	incompatibleServer := httptest.NewTLSServer(serverInfoHandler(2, instanceID, []string{"exact-values"}))
	defer incompatibleServer.Close()
	incompatibleProfile := profile
	incompatibleProfile.ServerURL = incompatibleServer.URL
	if _, err := NewQualifiedClient(context.Background(), incompatibleProfile, QualifiedClientOptions{HTTPClient: incompatibleServer.Client()}); !errors.Is(err, ErrCompatibility) {
		t.Fatalf("incompatible major error = %v, want ErrCompatibility", err)
	}

	unsupportedServer := httptest.NewTLSServer(serverInfoHandler(ProtocolMajor, instanceID, []string{"future-api1-feature"}))
	defer unsupportedServer.Close()
	unsupportedProfile := profile
	unsupportedProfile.ServerURL = unsupportedServer.URL
	if _, err := NewQualifiedClient(context.Background(), unsupportedProfile, QualifiedClientOptions{HTTPClient: unsupportedServer.Client()}); !errors.Is(err, ErrCompatibility) {
		t.Fatalf("unsupported required feature error = %v, want ErrCompatibility", err)
	}

	mismatchedServer := httptest.NewTLSServer(serverInfoHandler(ProtocolMajor, "different-instance", []string{"exact-values"}))
	defer mismatchedServer.Close()
	mismatchedProfile := profile
	mismatchedProfile.ServerURL = mismatchedServer.URL
	if _, err := NewQualifiedClient(context.Background(), mismatchedProfile, QualifiedClientOptions{HTTPClient: mismatchedServer.Client()}); !errors.Is(err, ErrInstanceMismatch) {
		t.Fatalf("instance mismatch error = %v, want ErrInstanceMismatch", err)
	}
	if submissions.Load() != 0 {
		t.Fatalf("qualification failure submitted %d invocation(s)", submissions.Load())
	}
}

func TestManualArtifactOperationsUseQualifiedSession(t *testing.T) {
	t.Parallel()
	const (
		instanceID        = "instance-artifacts"
		workspaceID       = "workspace-artifacts"
		sessionCredential = "pm-artifact-session"
	)
	var artifactCalls atomic.Int32
	var artifactRoutes atomic.Uint32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/info" {
			serverInfoHandler(ProtocolMajor, instanceID, []string{"exact-values"}).ServeHTTP(writer, request)
			return
		}
		if request.Header.Get("Authorization") != "Bearer "+sessionCredential {
			t.Errorf("artifact request authorization = %q", request.Header.Get("Authorization"))
		}
		if request.Body != nil && request.ContentLength != 0 {
			t.Errorf("artifact capability request carried body length %d", request.ContentLength)
		}
		switch request.Method + " " + request.URL.Path {
		case "POST /v1/workspaces/workspace-artifacts/artifacts":
			artifactRoutes.Or(1)
		case "GET /v1/workspaces/workspace-artifacts/artifacts/artifact-one":
			artifactRoutes.Or(2)
		case "GET /v1/workspaces/workspace-artifacts/artifacts/artifact-one/content":
			artifactRoutes.Or(4)
		default:
			t.Errorf("noncanonical artifact route %s %s", request.Method, request.URL.Path)
		}
		artifactCalls.Add(1)
		writer.Header().Set("Content-Type", "application/problem+json")
		writer.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(writer).Encode(Problem{
			Type:            "https://pm.invalid/problems/capability-unavailable",
			Title:           "Capability unavailable",
			Status:          http.StatusServiceUnavailable,
			Detail:          "Artifact byte storage is not installed.",
			Code:            "capability_unavailable",
			IncidentId:      "incident-artifacts",
			SafeRetryAction: "after_capability_change",
		})
	}))
	defer server.Close()

	profile := ServerProfile{Name: "artifacts", ServerURL: server.URL, InstanceID: instanceID, SessionHandle: "session-artifacts"}
	client, err := NewQualifiedClient(t.Context(), profile, QualifiedClientOptions{HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("qualify artifact client: %v", err)
	}
	if _, err := client.UploadArtifactWithResponse(t.Context(), WorkspaceID(workspaceID)); !errors.Is(err, ErrUnsafeClientRequest) {
		t.Fatalf("unauthenticated artifact request error = %v, want ErrUnsafeClientRequest", err)
	}
	store := NewMemorySessionStore()
	key, err := NewSessionKey(instanceID, profile.SessionHandle)
	if err != nil {
		t.Fatalf("artifact session key: %v", err)
	}
	if err := store.Store(t.Context(), key, PMDeviceSession{
		ExpiresAt:           time.Now().Add(time.Hour),
		PmSessionCredential: sessionCredential,
		Principal:           Principal{Kind: PrincipalKindUser, PrincipalId: "ada-artifacts"},
		SessionId:           "session-artifacts",
	}); err != nil {
		t.Fatalf("store artifact session: %v", err)
	}
	client, err = client.WithSession(t.Context(), store)
	if err != nil {
		t.Fatalf("authenticate artifact client: %v", err)
	}
	requests := []struct {
		name string
		call func() (*ArtifactOperationResponse, error)
	}{
		{name: "upload", call: func() (*ArtifactOperationResponse, error) {
			return client.UploadArtifactWithResponse(t.Context(), WorkspaceID(workspaceID))
		}},
		{name: "inspect", call: func() (*ArtifactOperationResponse, error) {
			return client.InspectArtifactWithResponse(t.Context(), WorkspaceID(workspaceID), ArtifactID("artifact-one"))
		}},
		{name: "download", call: func() (*ArtifactOperationResponse, error) {
			return client.DownloadArtifactWithResponse(t.Context(), WorkspaceID(workspaceID), ArtifactID("artifact-one"))
		}},
	}
	for _, request := range requests {
		response, err := request.call()
		if err != nil {
			t.Fatalf("%s artifact request: %v", request.name, err)
		}
		envelope, err := DecodeEnvelope(response)
		if err != nil || envelope.Problem == nil || envelope.Problem.Value.Code != "capability_unavailable" {
			t.Fatalf("%s artifact response = (%#v, %v)", request.name, envelope, err)
		}
	}
	if artifactCalls.Load() != 3 {
		t.Fatalf("artifact route calls = %d, want 3", artifactCalls.Load())
	}
	if artifactRoutes.Load() != 7 {
		t.Fatalf("artifact route mask = %03b, want 111", artifactRoutes.Load())
	}
}

func TestInvocationRecoveryPersistsBeforeBoundedSameKeySends(t *testing.T) {
	t.Parallel()
	const (
		instanceID  = "instance-recovery"
		workspaceID = "workspace-recovery"
		requestKey  = "request-recovery"
	)
	recoveryDirectory := filepath.Join(t.TempDir(), "recovery")
	var sends atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/info" {
			serverInfoHandler(ProtocolMajor, instanceID, []string{"exact-values"}).ServeHTTP(writer, request)
			return
		}
		if request.URL.Path != "/v1/workspaces/"+workspaceID+"/invocations" {
			http.NotFound(writer, request)
			return
		}
		entries, err := os.ReadDir(recoveryDirectory)
		if err != nil || len(entries) != 1 {
			t.Errorf("invocation send preceded durable recovery record: entries %d error %v", len(entries), err)
		}
		attempt := sends.Add(1)
		if request.Header.Get("Idempotency-Key") != requestKey {
			t.Errorf("attempt %d changed request key to %q", attempt, request.Header.Get("Idempotency-Key"))
		}
		if attempt < 3 {
			writer.Header().Set("Content-Type", "application/problem+json")
			writer.Header().Set("Retry-After", "0")
			writer.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(writer).Encode(Problem{
				Type:            "https://pm.invalid/problems/workspace-admission-count-limit",
				Title:           "Workspace admission limit",
				Status:          http.StatusTooManyRequests,
				Detail:          "The bounded workspace queue is full.",
				Code:            "workspace_admission_count_limit",
				IncidentId:      "incident-recovery",
				SafeRetryAction: "after_backlog_progress",
			})
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusAccepted)
		_, _ = writer.Write([]byte(`{"state":"accepted"}`))
	}))
	defer server.Close()
	profile := ServerProfile{Name: "recovery", ServerURL: server.URL, InstanceID: instanceID, SessionHandle: "session-recovery"}
	client, err := NewQualifiedClient(context.Background(), profile, QualifiedClientOptions{HTTPClient: server.Client()})
	if err != nil {
		t.Fatalf("qualify recovery client: %v", err)
	}
	sessionStore := NewMemorySessionStore()
	sessionKey, err := NewSessionKey(instanceID, profile.SessionHandle)
	if err != nil {
		t.Fatalf("recovery session key: %v", err)
	}
	if err := sessionStore.Store(t.Context(), sessionKey, PMDeviceSession{
		ExpiresAt:           time.Now().Add(time.Hour),
		PmSessionCredential: "pm-recovery-session",
		Principal:           Principal{Kind: PrincipalKindUser, PrincipalId: "ada-recovery"},
		SessionId:           "session-recovery",
	}); err != nil {
		t.Fatalf("store recovery session: %v", err)
	}
	client, err = client.WithSession(t.Context(), sessionStore)
	if err != nil {
		t.Fatalf("authenticate recovery client: %v", err)
	}
	store, err := NewFileRecoveryStore(recoveryDirectory)
	if err != nil {
		t.Fatalf("recovery store: %v", err)
	}
	body := InvocationRequest{
		ApprovalRef:   nullable.NewNullNullable[string](),
		ConnectionRef: "connection-recovery",
		Input:         contract.NewObjectValue(map[string]contract.PresentValue{"enabled": contract.NewBooleanValue(false)}),
		Lane:          InvocationRequestLaneDirectRead,
		OperationRef:  "operation-recovery",
		Protocol: ProtocolMetadata{
			ExpectedRevision: "1",
			IntentDigest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			RequestKey:       requestKey,
			RequiredFeatures: []string{},
			WireVersion:      ProtocolMetadataWireVersion(WireVersion),
		},
		ResultControls: contract.ResultControls{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := client.SubmitInvocationWithRecovery(ctx, store, WorkspaceID(workspaceID), &SubmitInvocationParams{IdempotencyKey: IdempotencyKey(requestKey)}, body)
	if err != nil || response.StatusCode() != http.StatusAccepted {
		t.Fatalf("bounded recovery result = (%v, %v)", response, err)
	}
	if sends.Load() != 3 {
		t.Fatalf("same-key sends = %d, want 3", sends.Load())
	}

	changed := body
	changed.Input = contract.NewObjectValue(map[string]contract.PresentValue{"enabled": contract.NewBooleanValue(true)})
	if _, err := client.SubmitInvocationWithRecovery(ctx, store, WorkspaceID(workspaceID), &SubmitInvocationParams{IdempotencyKey: IdempotencyKey(requestKey)}, changed); !errors.Is(err, ErrRecoveryConflict) {
		t.Fatalf("changed same-key intent error = %v, want recovery conflict", err)
	}
	if sends.Load() != 3 {
		t.Fatalf("conflicting persisted intent reached API: sends = %d", sends.Load())
	}
}

func serverInfoHandler(major int, instanceID string, required []string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(ServerInfo{
			ProtocolMajor:    major,
			ProtocolVersion:  ServerInfoProtocolVersion(WireVersion),
			InstanceId:       instanceID,
			Features:         []string{"exact-values", "wire-problem-v1"},
			RequiredFeatures: required,
		})
	})
}
