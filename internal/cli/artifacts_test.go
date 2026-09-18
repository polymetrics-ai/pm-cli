package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
)

const (
	artifactCLIInstanceID        = "instance-artifact-cli"
	artifactCLIProfileName       = "artifact-cli"
	artifactCLIWorkspaceID       = "workspace-artifact-cli"
	artifactCLISessionCredential = "pm-artifact-cli-session"
)

type artifactCLIFixture struct {
	server       *httptest.Server
	profilesPath string
	store        pmapi.SessionStore
}

type artifactCLIResult struct {
	code   int
	stdout []byte
	stderr string
}

func newArtifactCLIFixture(t *testing.T, handler http.HandlerFunc) *artifactCLIFixture {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/info" {
			writer.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(writer).Encode(pmapi.ServerInfo{
				ProtocolMajor:    pmapi.ProtocolMajor,
				ProtocolVersion:  pmapi.ServerInfoProtocolVersion(pmapi.WireVersion),
				InstanceId:       artifactCLIInstanceID,
				Features:         []string{"exact-values", "wire-problem-v1"},
				RequiredFeatures: []string{"exact-values"},
			}); err != nil {
				t.Errorf("encode server information: %v", err)
			}
			return
		}
		if request.Header.Get("Authorization") != "Bearer "+artifactCLISessionCredential {
			t.Errorf("artifact request used an unqualified PM session")
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)

	profilesPath := filepath.Join(t.TempDir(), "profiles.json")
	handle, err := pmapi.DefaultSessionHandle(artifactCLIProfileName, artifactCLIInstanceID)
	if err != nil {
		t.Fatalf("derive artifact CLI session handle: %v", err)
	}
	if err := pmapi.AddServerProfile(profilesPath, pmapi.ServerProfile{
		Name:          artifactCLIProfileName,
		ServerURL:     server.URL,
		InstanceID:    artifactCLIInstanceID,
		SessionHandle: handle,
	}); err != nil {
		t.Fatalf("write artifact CLI profile: %v", err)
	}
	store := pmapi.NewMemorySessionStore()
	key, err := pmapi.NewSessionKey(artifactCLIInstanceID, handle)
	if err != nil {
		t.Fatalf("derive artifact CLI session key: %v", err)
	}
	if err := store.Store(t.Context(), key, pmapi.PMDeviceSession{
		ExpiresAt:           time.Now().Add(time.Hour),
		PmSessionCredential: artifactCLISessionCredential,
		Principal: pmapi.Principal{
			Kind:        pmapi.PrincipalKindUser,
			PrincipalId: "ada-artifact-cli",
		},
		SessionId: "session-artifact-cli",
	}); err != nil {
		t.Fatalf("store artifact CLI session: %v", err)
	}
	return &artifactCLIFixture{server: server, profilesPath: profilesPath, store: store}
}

func (fixture *artifactCLIFixture) execute(ctx context.Context, arguments ...string) artifactCLIResult {
	args := append([]string(nil), arguments...)
	args = append(args,
		"--server-profile", artifactCLIProfileName,
		"--workspace", artifactCLIWorkspaceID,
		"--session-store", "memory",
	)
	var stdout, stderr bytes.Buffer
	code := Execute(ctx, Options{
		Args:         args,
		Stdout:       &stdout,
		Stderr:       &stderr,
		ProfilesPath: fixture.profilesPath,
		HTTPClient:   fixture.server.Client(),
		SessionStore: func(mode pmapi.SessionStoreMode) (pmapi.SessionStore, error) {
			if mode != pmapi.SessionStoreMemory {
				return nil, fmt.Errorf("unexpected artifact CLI session mode %q", mode)
			}
			return fixture.store, nil
		},
	})
	return artifactCLIResult{code: code, stdout: append([]byte(nil), stdout.Bytes()...), stderr: stderr.String()}
}

func TestArtifactDownloadHelpIsOffline(t *testing.T) {
	network := &unavailableDoer{}
	var environmentLookups, sessionLookups int
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), Options{
		Args:         []string{"artifacts", "download", "--help"},
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
			return nil, fmt.Errorf("offline help must not open a session store")
		},
	})
	if code != 0 || environmentLookups != 0 || sessionLookups != 0 || network.calls != 0 || stderr.Len() != 0 {
		t.Fatalf(
			"artifact download help = code %d env %d sessions %d network %d stderr %q",
			code,
			environmentLookups,
			sessionLookups,
			network.calls,
			stderr.String(),
		)
	}
}

func TestArtifactInspectRendersTypedMetadataAndProblem(t *testing.T) {
	metadata := pmapi.ArtifactMetadata{
		ArtifactId: "artifact-metadata",
		ByteLength: 17,
		MediaType:  "application/octet-stream",
		Sha256:     strings.Repeat("a", 64),
	}
	fixture := newArtifactCLIFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.Header.Get("Range") != "" {
			t.Errorf("inspect request = %s range %q", request.Method, request.Header.Get("Range"))
		}
		switch request.URL.Path {
		case "/v1/workspaces/" + artifactCLIWorkspaceID + "/artifacts/" + metadata.ArtifactId:
			writer.Header().Set("Cache-Control", "no-store")
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("ETag", `"sha256:`+metadata.Sha256+`"`)
			if err := json.NewEncoder(writer).Encode(metadata); err != nil {
				t.Errorf("encode artifact metadata: %v", err)
			}
		case "/v1/workspaces/" + artifactCLIWorkspaceID + "/artifacts/artifact-missing":
			writeArtifactCLIProblem(
				t,
				writer,
				http.StatusNotFound,
				"resource_not_found",
				"correct_scope",
				"The selected artifact is not available.",
				"incident-artifact-missing",
			)
		default:
			http.NotFound(writer, request)
		}
	})

	success := fixture.execute(t.Context(), "artifacts", "inspect", metadata.ArtifactId, "--output", "json")
	if success.code != 0 || success.stderr != "" {
		t.Fatalf("artifact metadata = exit %d stdout %q stderr %q", success.code, success.stdout, success.stderr)
	}
	var rendered pmapi.ArtifactMetadata
	if err := json.Unmarshal(success.stdout, &rendered); err != nil {
		t.Fatalf("decode rendered artifact metadata: %v", err)
	}
	if rendered != metadata {
		t.Fatalf("rendered artifact metadata = %#v, want %#v", rendered, metadata)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(success.stdout, &fields); err != nil {
		t.Fatalf("decode rendered artifact metadata fields: %v", err)
	}
	for _, name := range []string{"artifact_id", "sha256", "byte_length", "media_type"} {
		if _, present := fields[name]; !present {
			t.Fatalf("rendered artifact metadata omits %q", name)
		}
	}
	if len(fields) != 4 {
		t.Fatalf("rendered artifact metadata exposed unexpected fields: %#v", fields)
	}

	refusal := fixture.execute(t.Context(), "artifacts", "inspect", "artifact-missing", "--output", "text")
	if refusal.code != 3 || len(refusal.stdout) != 0 ||
		!strings.Contains(refusal.stderr, "resource_not_found") ||
		!strings.Contains(refusal.stderr, "incident-artifact-missing") {
		t.Fatalf("artifact inspect refusal = exit %d stdout %q stderr %q", refusal.code, refusal.stdout, refusal.stderr)
	}
}

func TestArtifactDownloadWritesExactFullAndRangeBytes(t *testing.T) {
	artifactID := "artifact-bytes"
	full := []byte{0x00, 'P', 'M', '\n', 0xff, '{', '}', 0x00}
	partial := full[2:6]
	digest := fmt.Sprintf("%x", sha256.Sum256(full))
	fixture := newArtifactCLIFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/v1/workspaces/"+artifactCLIWorkspaceID+"/artifacts/"+artifactID+"/content" {
			t.Errorf("download request = %s %s", request.Method, request.URL.Path)
			http.NotFound(writer, request)
			return
		}
		switch request.Header.Get("Range") {
		case "":
			writeArtifactCLIBytes(writer, http.StatusOK, full, digest, "")
		case "bytes=2-5":
			writeArtifactCLIBytes(writer, http.StatusPartialContent, partial, digest, "bytes 2-5/8")
		default:
			t.Errorf("download range = %q", request.Header.Get("Range"))
			writeArtifactCLIProblem(
				t,
				writer,
				http.StatusRequestedRangeNotSatisfiable,
				"artifact_range_not_satisfiable",
				"correct_input",
				"The selected range is not satisfiable.",
				"incident-artifact-range-unexpected",
			)
		}
	})

	complete := fixture.execute(t.Context(), "artifacts", "download", artifactID, "--output", "json")
	if complete.code != 0 || !bytes.Equal(complete.stdout, full) || complete.stderr != "" {
		t.Fatalf("full artifact download = exit %d stdout %v stderr %q", complete.code, complete.stdout, complete.stderr)
	}
	ranged := fixture.execute(
		t.Context(),
		"artifacts", "download", artifactID,
		"--range", "bytes=2-5",
		"--json=false",
	)
	if ranged.code != 0 || !bytes.Equal(ranged.stdout, partial) || ranged.stderr != "" {
		t.Fatalf("ranged artifact download = exit %d stdout %v stderr %q", ranged.code, ranged.stdout, ranged.stderr)
	}
}

func TestArtifactDownloadFailuresNeverExitSuccess(t *testing.T) {
	t.Run("typed range refusal remains a PM problem", func(t *testing.T) {
		fixture := newArtifactCLIFixture(t, func(writer http.ResponseWriter, request *http.Request) {
			if request.Header.Get("Range") != "bytes=99-" {
				t.Errorf("refused download range = %q", request.Header.Get("Range"))
			}
			writer.Header().Set("Accept-Ranges", "bytes")
			writer.Header().Set("Cache-Control", "no-store")
			writer.Header().Set("Content-Range", "bytes */8")
			writeArtifactCLIProblem(
				t,
				writer,
				http.StatusRequestedRangeNotSatisfiable,
				"artifact_range_not_satisfiable",
				"correct_input",
				"The selected range is not satisfiable.",
				"incident-artifact-range",
			)
		})
		result := fixture.execute(
			t.Context(),
			"artifacts", "download", "artifact-range",
			"--range", "bytes=99-",
		)
		if result.code == 0 || len(result.stdout) != 0 ||
			!strings.Contains(result.stderr, "artifact_range_not_satisfiable") ||
			!strings.Contains(result.stderr, "incident-artifact-range") {
			t.Fatalf("artifact range refusal = exit %d stdout %q stderr %q", result.code, result.stdout, result.stderr)
		}
	})

	t.Run("truncated response fails after any streamed prefix", func(t *testing.T) {
		complete := []byte("sensitive-payload")
		prefix := complete[:5]
		digest := fmt.Sprintf("%x", sha256.Sum256(complete))
		fixture := newArtifactCLIFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Accept-Ranges", "bytes")
			writer.Header().Set("Cache-Control", "no-store")
			writer.Header().Set("Content-Disposition", "attachment")
			writer.Header().Set("Content-Length", strconv.Itoa(len(complete)))
			writer.Header().Set("Content-Type", "application/octet-stream")
			writer.Header().Set("ETag", `"sha256:`+digest+`"`)
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write(prefix)
		})
		result := fixture.execute(t.Context(), "artifacts", "download", "artifact-truncated")
		if result.code == 0 || result.stderr == "" || strings.Contains(result.stderr, string(prefix)) {
			t.Fatalf("truncated artifact = exit %d stdout %q stderr %q", result.code, result.stdout, result.stderr)
		}
	})

	t.Run("canceled request fails", func(t *testing.T) {
		started := make(chan struct{})
		fixture := newArtifactCLIFixture(t, func(_ http.ResponseWriter, request *http.Request) {
			close(started)
			<-request.Context().Done()
		})
		ctx, cancel := context.WithCancel(t.Context())
		results := make(chan artifactCLIResult, 1)
		go func() {
			results <- fixture.execute(ctx, "artifacts", "download", "artifact-canceled")
		}()
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			cancel()
			t.Fatal("artifact download did not reach the controlled response")
		}
		cancel()
		select {
		case result := <-results:
			if result.code == 0 || len(result.stdout) != 0 || result.stderr == "" {
				t.Fatalf("canceled artifact = exit %d stdout %q stderr %q", result.code, result.stdout, result.stderr)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("canceled artifact download did not return")
		}
	})
}

func TestArtifactUploadRemainsBodylessAndUnavailable(t *testing.T) {
	fixture := newArtifactCLIFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost ||
			request.URL.Path != "/v1/workspaces/"+artifactCLIWorkspaceID+"/artifacts" ||
			request.ContentLength != 0 {
			t.Errorf("artifact upload probe = %s %s content length %d", request.Method, request.URL.Path, request.ContentLength)
		}
		writeArtifactCLIProblem(
			t,
			writer,
			http.StatusServiceUnavailable,
			"capability_unavailable",
			"after_capability_change",
			"Public artifact upload is unavailable.",
			"incident-artifact-upload",
		)
	})
	result := fixture.execute(t.Context(), "artifacts", "upload")
	if result.code != 5 || len(result.stdout) != 0 || !strings.Contains(result.stderr, "capability_unavailable") {
		t.Fatalf("artifact upload = exit %d stdout %q stderr %q", result.code, result.stdout, result.stderr)
	}
}

func writeArtifactCLIBytes(writer http.ResponseWriter, status int, body []byte, digest, contentRange string) {
	writer.Header().Set("Accept-Ranges", "bytes")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Disposition", "attachment")
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("ETag", `"sha256:`+digest+`"`)
	if contentRange != "" {
		writer.Header().Set("Content-Range", contentRange)
	}
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}

func writeArtifactCLIProblem(
	t *testing.T,
	writer http.ResponseWriter,
	status int,
	code string,
	retryAction string,
	detail string,
	incidentID string,
) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/problem+json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(pmapi.Problem{
		Type:            "https://pm.invalid/problems/" + strings.ReplaceAll(code, "_", "-"),
		Title:           "Artifact request refused",
		Status:          status,
		Detail:          detail,
		Code:            code,
		IncidentId:      incidentID,
		SafeRetryAction: retryAction,
	}); err != nil {
		t.Errorf("encode artifact problem: %v", err)
	}
}
