package httpapi_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

const artifactCanary = "fixture-artifact-bytes-must-not-be-fabricated"

type artifactRouteResponse struct {
	status   int
	header   http.Header
	document []byte
}

// controlledArtifactBackend exercises the application/HTTP consumer contract.
// It is deliberately not evidence for S3 behavior or selected-stack publication.
type controlledArtifactBackend struct {
	mutex       sync.Mutex
	content     map[contract.ObjectRef][]byte
	verifyError error
	openError   error
	verifyCalls int
	openCalls   int
}

func newControlledArtifactBackend() *controlledArtifactBackend {
	return &controlledArtifactBackend{content: make(map[contract.ObjectRef][]byte)}
}

func (backend *controlledArtifactBackend) register(ref contract.ObjectRef, payload []byte) {
	backend.mutex.Lock()
	defer backend.mutex.Unlock()
	backend.content[ref] = append([]byte(nil), payload...)
}

func (backend *controlledArtifactBackend) setErrors(verifyError, openError error) {
	backend.mutex.Lock()
	defer backend.mutex.Unlock()
	backend.verifyError = verifyError
	backend.openError = openError
}

func (backend *controlledArtifactBackend) calls() (verify, open int) {
	backend.mutex.Lock()
	defer backend.mutex.Unlock()
	return backend.verifyCalls, backend.openCalls
}

func (backend *controlledArtifactBackend) Verify(ctx context.Context, ref contract.ObjectRef) error {
	backend.mutex.Lock()
	defer backend.mutex.Unlock()
	backend.verifyCalls++
	if ctx == nil || ctx.Err() != nil {
		return objects.ErrObjectUnavailable
	}
	if backend.verifyError != nil {
		return backend.verifyError
	}
	payload, ok := backend.content[ref]
	if !ok || !artifactPayloadMatches(ref, payload) {
		return objects.ErrObjectIntegrity
	}
	return nil
}

func (backend *controlledArtifactBackend) OpenVerified(
	ctx context.Context,
	ref contract.ObjectRef,
	selection objects.ByteRange,
) (io.ReadCloser, error) {
	backend.mutex.Lock()
	defer backend.mutex.Unlock()
	backend.openCalls++
	if ctx == nil || ctx.Err() != nil {
		return nil, objects.ErrObjectUnavailable
	}
	if backend.openError != nil {
		return nil, backend.openError
	}
	payload, ok := backend.content[ref]
	if !ok || !artifactPayloadMatches(ref, payload) {
		return nil, objects.ErrObjectIntegrity
	}
	if selection.Offset < 0 || selection.Length < 0 || selection.Offset > ref.ByteLength ||
		selection.Length > ref.ByteLength-selection.Offset {
		return nil, objects.ErrInvalidRange
	}
	selected := append([]byte(nil), payload[selection.Offset:selection.Offset+selection.Length]...)
	return io.NopCloser(bytes.NewReader(selected)), nil
}

func artifactPayloadMatches(ref contract.ObjectRef, payload []byte) bool {
	digest := sha256.Sum256(payload)
	return ref.ByteLength == int64(len(payload)) && ref.SHA256 == hex.EncodeToString(digest[:])
}

func TestScopedUnavailableArtifactRoutes(t *testing.T) {
	t.Run("R16_E02_C01_unauthorized_and_unknown_artifacts_are_concealed_before_backend_io", func(t *testing.T) {
		fixture := testfixture.New(t)
		artifact := registerArtifactMetadata(t, fixture, "artifact-authorized")
		foreignClient := fixture.ClientFor(t, fixture.ForeignActor)
		primaryCollection := artifactCollectionPath(fixture, fixture.Scope)

		for _, test := range []struct {
			name    string
			client  *http.Client
			method  string
			path    string
			headers map[string]string
		}{
			{
				name:   "foreign principal inspect",
				client: foreignClient,
				method: http.MethodGet,
				path:   artifactPath(fixture, fixture.Scope, artifact.ArtifactID),
			},
			{
				name:    "foreign principal download",
				client:  foreignClient,
				method:  http.MethodGet,
				path:    artifactPath(fixture, fixture.Scope, artifact.ArtifactID) + "/content",
				headers: map[string]string{"Range": "bytes=0-3"},
			},
			{
				name:   "unauthorized workspace inspect",
				client: fixture.Client,
				method: http.MethodGet,
				path:   artifactPath(fixture, fixture.ForeignScope, artifact.ArtifactID),
			},
			{
				name:   "unknown artifact inspect",
				client: fixture.Client,
				method: http.MethodGet,
				path:   artifactPath(fixture, fixture.Scope, "artifact-unknown"),
			},
			{
				name:   "unknown artifact download",
				client: fixture.Client,
				method: http.MethodGet,
				path:   artifactPath(fixture, fixture.Scope, "artifact-unknown") + "/content",
			},
			{
				name:   "foreign principal upload",
				client: foreignClient,
				method: http.MethodPost,
				path:   primaryCollection,
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				response := sendArtifactRoute(t, test.client, test.method, test.path, test.headers)
				requireArtifactProblem(t, response, http.StatusNotFound, "resource_not_found")
				for _, canary := range []string{
					artifact.ArtifactID,
					artifact.ObjectAttemptKey,
					artifact.SHA256,
					artifactCanary,
				} {
					if bytes.Contains(response.document, []byte(canary)) {
						t.Fatalf("concealed artifact response disclosed %q: %s", canary, response.document)
					}
				}
			})
		}
	})

	t.Run("R16_E02_C02_absent_backend_is_explicitly_unavailable_without_bytes_or_metadata", func(t *testing.T) {
		fixture := testfixture.New(t)
		grantArtifactWrite(t, fixture)
		artifact := registerArtifactMetadata(t, fixture, "artifact-unavailable")
		collection := artifactCollectionPath(fixture, fixture.Scope)
		resource := artifactPath(fixture, fixture.Scope, artifact.ArtifactID)

		for _, test := range []struct {
			name    string
			method  string
			path    string
			headers map[string]string
		}{
			{name: "inspect", method: http.MethodGet, path: resource},
			{
				name:    "download malformed range",
				method:  http.MethodGet,
				path:    resource + "/content",
				headers: map[string]string{"Range": "bytes=not-a-range"},
			},
			{
				name:    "download",
				method:  http.MethodGet,
				path:    resource + "/content",
				headers: map[string]string{"Range": "bytes=0-3"},
			},
			{
				name:   "upload",
				method: http.MethodPost,
				path:   collection,
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				response := sendArtifactRoute(t, fixture.Client, test.method, test.path, test.headers)
				requireArtifactProblem(t, response, http.StatusServiceUnavailable, "capability_unavailable")
				if response.header.Get("ETag") != "" || response.header.Get("Content-Range") != "" ||
					response.header.Get("Accept-Ranges") != "" {
					t.Fatalf("unavailable artifact route advertised byte availability: %#v", response.header)
				}
				for _, canary := range []string{
					artifact.ArtifactID,
					artifact.ObjectAttemptKey,
					artifact.SHA256,
					artifactCanary,
				} {
					if bytes.Contains(response.document, []byte(canary)) {
						t.Fatalf("unavailable artifact route returned %q: %s", canary, response.document)
					}
				}
			})
		}

		stored := loadArtifactMetadata(t, fixture, artifact.ArtifactID)
		if stored != artifact {
			t.Fatalf("unavailable artifact routes changed immutable metadata: got %#v want %#v", stored, artifact)
		}
	})

	t.Run("R16_E02_C04_artifact_connection_authority_is_derived_from_trusted_metadata", func(t *testing.T) {
		fixture := testfixture.New(t)
		restrictedConnectionID := fixture.Connection.ConnectionID + "-restricted"
		fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO pm.connections (
					org_id, workspace_id, connection_id, connector_id, provider_account_id,
					revision, credential_ref, display_name, state
				) VALUES ($1, $2, $3, $4, $5, 1, '', 'Restricted artifact connection', 'active')
			`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, restrictedConnectionID,
				fixture.Connection.ConnectorID, fixture.Connection.ProviderAccountID)
			return err
		})
		restricted := registerScopedArtifactMetadata(
			t,
			fixture,
			fixture.Scope,
			fixture.Actor,
			restrictedConnectionID,
			"artifact-restricted-connection",
		)
		revoked := registerArtifactMetadata(t, fixture, "artifact-revoked-connection")
		unbound := insertUnboundArtifactMetadata(t, fixture, "artifact-unbound-legacy")
		foreign := registerScopedArtifactMetadata(
			t,
			fixture,
			fixture.ForeignScope,
			fixture.ForeignActor,
			fixture.ForeignConnection.ConnectionID,
			"artifact-foreign-workspace",
		)
		fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `
				UPDATE pm.connections
				SET state = 'revoked',
				    revoked_at = clock_timestamp(),
				    revision = revision + 1,
				    updated_at = clock_timestamp()
				WHERE org_id = $1 AND workspace_id = $2 AND connection_id = $3
			`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, fixture.Connection.ConnectionID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errors.New("artifact connection fixture was not revoked")
			}
			return nil
		})

		for _, test := range []struct {
			name     string
			scope    contract.Scope
			artifact contract.ObjectRef
		}{
			{name: "active connection outside actor grant", scope: fixture.Scope, artifact: restricted},
			{name: "revoked associated connection", scope: fixture.Scope, artifact: revoked},
			{name: "retained unbound artifact", scope: fixture.Scope, artifact: unbound},
			{name: "sibling workspace artifact", scope: fixture.Scope, artifact: foreign},
		} {
			t.Run(test.name, func(t *testing.T) {
				resource := artifactPath(fixture, test.scope, test.artifact.ArtifactID)
				for _, route := range []struct {
					name    string
					path    string
					headers map[string]string
				}{
					{name: "inspect", path: resource},
					{name: "download", path: resource + "/content", headers: map[string]string{"Range": "bytes=0-3"}},
				} {
					t.Run(route.name, func(t *testing.T) {
						response := sendArtifactRoute(t, fixture.Client, http.MethodGet, route.path, route.headers)
						requireArtifactProblem(t, response, http.StatusNotFound, "resource_not_found")
						for _, canary := range []string{
							test.artifact.ArtifactID,
							test.artifact.ObjectAttemptKey,
							test.artifact.SHA256,
							artifactCanary,
						} {
							if bytes.Contains(response.document, []byte(canary)) {
								t.Fatalf("concealed artifact response disclosed %q: %s", canary, response.document)
							}
						}
					})
				}
			})
		}
	})

	t.Run("R16_E02_C03_raw_upload_body_is_refused_at_the_protocol_boundary", func(t *testing.T) {
		fixture := testfixture.New(t)
		collection := artifactCollectionPath(fixture, fixture.Scope)
		for _, test := range []struct {
			name   string
			client *http.Client
		}{
			{name: "authenticated principal", client: fixture.Client},
			{name: "foreign principal", client: fixture.ClientFor(t, fixture.ForeignActor)},
		} {
			t.Run(test.name, func(t *testing.T) {
				response := sendArtifactRouteWithBody(
					t,
					test.client,
					http.MethodPost,
					collection,
					strings.NewReader(artifactCanary),
					map[string]string{"Content-Type": "application/octet-stream"},
				)
				problem := requireArtifactProblem(t, response, http.StatusBadRequest, "invalid_request")
				if problem.SafeRetryAction != "correct_input" {
					t.Fatalf("raw artifact upload retry action = %q; want correct_input", problem.SafeRetryAction)
				}
				if bytes.Contains(response.document, []byte(artifactCanary)) {
					t.Fatalf("raw artifact refusal reflected request bytes: %s", response.document)
				}
			})
		}
	})
}

func TestScopedArtifactReadRoutes(t *testing.T) {
	t.Run("R17_E03_C02_inspection_returns_only_verified_public_metadata", func(t *testing.T) {
		fixture, backend := newControlledArtifactFixture(t)
		artifact := registerArtifactMetadata(t, fixture, "artifact-inspect-verified")
		backend.register(artifact, []byte(artifactCanary))

		response := sendArtifactRoute(
			t,
			fixture.Client,
			http.MethodGet,
			artifactPath(fixture, fixture.Scope, artifact.ArtifactID),
			nil,
		)
		if response.status != http.StatusOK {
			t.Fatalf("inspect status = %d; want 200: %s", response.status, response.document)
		}
		if contentType := response.header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
			t.Fatalf("inspect content type = %q; want application/json", contentType)
		}
		if response.header.Get("Cache-Control") != "no-store" {
			t.Fatalf("inspect cache control = %q; want no-store", response.header.Get("Cache-Control"))
		}
		if response.header.Get("ETag") != artifactETag(artifact) {
			t.Fatalf("inspect ETag = %q; want digest validator", response.header.Get("ETag"))
		}

		var members map[string]json.RawMessage
		if err := json.Unmarshal(response.document, &members); err != nil {
			t.Fatalf("decode inspect metadata: %v: %s", err, response.document)
		}
		allowed := map[string]bool{
			"artifact_id": true,
			"sha256":      true,
			"byte_length": true,
			"media_type":  true,
		}
		if len(members) != len(allowed) {
			t.Fatalf("inspect metadata members = %v; want only public metadata", members)
		}
		for name := range members {
			if !allowed[name] {
				t.Fatalf("inspect metadata exposed %q", name)
			}
		}
		var metadata struct {
			ArtifactID string `json:"artifact_id"`
			SHA256     string `json:"sha256"`
			ByteLength int64  `json:"byte_length"`
			MediaType  string `json:"media_type"`
		}
		if err := json.Unmarshal(response.document, &metadata); err != nil {
			t.Fatalf("decode typed inspect metadata: %v", err)
		}
		if metadata.ArtifactID != artifact.ArtifactID || metadata.SHA256 != artifact.SHA256 ||
			metadata.ByteLength != artifact.ByteLength || metadata.MediaType != artifact.MediaType {
			t.Fatalf("inspect metadata = %#v; want registered public projection", metadata)
		}
		for _, private := range []string{
			artifact.ObjectAttemptKey,
			artifact.BackendGeneration,
			artifact.OrgID,
			artifact.WorkspaceID,
		} {
			if bytes.Contains(response.document, []byte(private)) {
				t.Fatalf("inspect metadata exposed private backend or scope value %q: %s", private, response.document)
			}
		}
		verifyCalls, openCalls := backend.calls()
		if verifyCalls != 1 || openCalls != 0 {
			t.Fatalf("inspect backend calls = verify %d/open %d; want 1/0", verifyCalls, openCalls)
		}
	})

	t.Run("installed_read_backend_does_not_enable_the_bodyless_upload_route", func(t *testing.T) {
		fixture, backend := newControlledArtifactFixture(t)
		grantArtifactWrite(t, fixture)
		response := sendArtifactRoute(
			t,
			fixture.Client,
			http.MethodPost,
			artifactCollectionPath(fixture, fixture.Scope),
			nil,
		)
		requireArtifactProblem(t, response, http.StatusServiceUnavailable, "capability_unavailable")
		verifyCalls, openCalls := backend.calls()
		if verifyCalls != 0 || openCalls != 0 {
			t.Fatalf("unavailable upload reached read backend: verify %d/open %d", verifyCalls, openCalls)
		}
	})

	t.Run("R17_E03_C02_full_closed_from_and_suffix_ranges_return_exact_verified_bytes", func(t *testing.T) {
		fixture, backend := newControlledArtifactFixture(t)
		artifact := registerArtifactMetadata(t, fixture, "artifact-range-verified")
		payload := []byte(artifactCanary)
		backend.register(artifact, payload)
		resource := artifactPath(fixture, fixture.Scope, artifact.ArtifactID) + "/content"

		for _, test := range []struct {
			name        string
			rangeHeader string
			offset      int64
			length      int64
			partial     bool
		}{
			{name: "full", offset: 0, length: int64(len(payload))},
			{name: "closed", rangeHeader: "bytes=1-4", offset: 1, length: 4, partial: true},
			{name: "closed end is clipped", rangeHeader: "bytes=5-999", offset: 5, length: int64(len(payload)) - 5, partial: true},
			{name: "from", rangeHeader: "bytes=9-", offset: 9, length: int64(len(payload)) - 9, partial: true},
			{name: "suffix", rangeHeader: "bytes=-6", offset: int64(len(payload)) - 6, length: 6, partial: true},
			{name: "oversized suffix", rangeHeader: "bytes=-999", offset: 0, length: int64(len(payload)), partial: true},
		} {
			t.Run(test.name, func(t *testing.T) {
				var headers map[string]string
				if test.rangeHeader != "" {
					headers = map[string]string{"Range": test.rangeHeader}
				}
				response := sendArtifactRoute(t, fixture.Client, http.MethodGet, resource, headers)
				wantStatus := http.StatusOK
				if test.partial {
					wantStatus = http.StatusPartialContent
				}
				if response.status != wantStatus {
					t.Fatalf("download status = %d; want %d: %s", response.status, wantStatus, response.document)
				}
				wantBody := payload[test.offset : test.offset+test.length]
				if !bytes.Equal(response.document, wantBody) {
					t.Fatalf("download bytes = %q; want %q", response.document, wantBody)
				}
				if response.header.Get("Content-Length") != strconv.FormatInt(test.length, 10) {
					t.Fatalf("content length = %q; want %d", response.header.Get("Content-Length"), test.length)
				}
				if response.header.Get("Content-Type") != artifact.MediaType {
					t.Fatalf("content type = %q; want %q", response.header.Get("Content-Type"), artifact.MediaType)
				}
				if response.header.Get("Accept-Ranges") != "bytes" ||
					response.header.Get("Cache-Control") != "no-store" ||
					response.header.Get("ETag") != artifactETag(artifact) {
					t.Fatalf("download metadata headers are incomplete: %#v", response.header)
				}
				wantContentRange := ""
				if test.partial {
					wantContentRange = "bytes " + strconv.FormatInt(test.offset, 10) + "-" +
						strconv.FormatInt(test.offset+test.length-1, 10) + "/" +
						strconv.FormatInt(int64(len(payload)), 10)
				}
				if response.header.Get("Content-Range") != wantContentRange {
					t.Fatalf("content range = %q; want %q", response.header.Get("Content-Range"), wantContentRange)
				}
			})
		}
		verifyCalls, openCalls := backend.calls()
		if verifyCalls != 0 || openCalls != 6 {
			t.Fatalf("download backend calls = verify %d/open %d; want 0/6", verifyCalls, openCalls)
		}
	})

	t.Run("range_rejection_follows_authority_and_backend_availability_without_backend_io", func(t *testing.T) {
		fixture, backend := newControlledArtifactFixture(t)
		artifact := registerArtifactMetadata(t, fixture, "artifact-range-refusal")
		backend.register(artifact, []byte(artifactCanary))
		resource := artifactPath(fixture, fixture.Scope, artifact.ArtifactID) + "/content"
		for _, value := range []string{
			"bytes=0-1,3-4",
			"items=0-1",
			"bytes=-0",
			"bytes=5-4",
			"bytes=9223372036854775808-",
			"bytes=" + strings.Repeat("9", 257) + "-",
			"bytes=999-",
		} {
			response := sendArtifactRoute(
				t,
				fixture.Client,
				http.MethodGet,
				resource,
				map[string]string{"Range": value},
			)
			requireArtifactProblem(t, response, http.StatusRequestedRangeNotSatisfiable, "artifact_range_not_satisfiable")
			wantContentRange := "bytes */" + strconv.FormatInt(artifact.ByteLength, 10)
			if response.header.Get("Content-Range") != wantContentRange ||
				response.header.Get("Accept-Ranges") != "bytes" ||
				response.header.Get("Cache-Control") != "no-store" {
				t.Fatalf("range refusal headers for %q = %#v; want authorized length", value, response.header)
			}
			if response.header.Get("ETag") != "" {
				t.Fatalf("range refusal for %q exposed a success validator", value)
			}
		}
		verifyCalls, openCalls := backend.calls()
		if verifyCalls != 0 || openCalls != 0 {
			t.Fatalf("rejected ranges reached backend: verify %d/open %d", verifyCalls, openCalls)
		}
	})

	t.Run("R17_E03_C01_C03_C04_concealment_precedes_configured_backend_and_range_disclosure", func(t *testing.T) {
		fixture, backend := newControlledArtifactFixture(t)
		artifact := registerArtifactMetadata(t, fixture, "artifact-configured-concealment")
		backend.register(artifact, []byte(artifactCanary))
		foreignClient := fixture.ClientFor(t, fixture.ForeignActor)
		for _, request := range []struct {
			name   string
			client *http.Client
			path   string
		}{
			{
				name:   "foreign download",
				client: foreignClient,
				path:   artifactPath(fixture, fixture.Scope, artifact.ArtifactID) + "/content",
			},
			{
				name:   "unknown download",
				client: fixture.Client,
				path:   artifactPath(fixture, fixture.Scope, "artifact-configured-unknown") + "/content",
			},
		} {
			t.Run(request.name, func(t *testing.T) {
				response := sendArtifactRoute(
					t,
					request.client,
					http.MethodGet,
					request.path,
					map[string]string{"Range": "bytes=not-a-range"},
				)
				requireArtifactProblem(t, response, http.StatusNotFound, "resource_not_found")
				if response.header.Get("Content-Range") != "" || response.header.Get("Accept-Ranges") != "" {
					t.Fatalf("concealed request disclosed range availability: %#v", response.header)
				}
			})
		}
		verifyCalls, openCalls := backend.calls()
		if verifyCalls != 0 || openCalls != 0 {
			t.Fatalf("concealed requests reached backend: verify %d/open %d", verifyCalls, openCalls)
		}
	})

	t.Run("controlled_backend_faults_are_sanitized_and_are_not_real_S3_proof", func(t *testing.T) {
		const faultCanary = "controlled-backend-fault access_key=must-not-leak"
		for _, test := range []struct {
			name        string
			inspect     bool
			verifyError error
			openError   error
			payload     []byte
			status      int
			code        string
		}{
			{
				name:        "inspection verification fault",
				inspect:     true,
				verifyError: errors.New(faultCanary),
				status:      http.StatusInternalServerError,
				code:        "internal_error",
			},
			{
				name:      "download verification fault",
				openError: errors.New(faultCanary),
				status:    http.StatusInternalServerError,
				code:      "internal_error",
			},
			{
				name:    "registered digest mismatch",
				payload: []byte("controlled-corrupt-object-bytes"),
				status:  http.StatusInternalServerError,
				code:    "internal_error",
			},
			{
				name:      "download backend unavailable",
				openError: objects.ErrObjectUnavailable,
				status:    http.StatusServiceUnavailable,
				code:      "capability_unavailable",
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				fixture, backend := newControlledArtifactFixture(t)
				artifact := registerArtifactMetadata(t, fixture, "artifact-controlled-fault")
				payload := []byte(artifactCanary)
				if test.payload != nil {
					payload = test.payload
				}
				backend.register(artifact, payload)
				backend.setErrors(test.verifyError, test.openError)
				resource := artifactPath(fixture, fixture.Scope, artifact.ArtifactID)
				var headers map[string]string
				if !test.inspect {
					resource += "/content"
					headers = map[string]string{"Range": "bytes=0-3"}
				}
				response := sendArtifactRoute(t, fixture.Client, http.MethodGet, resource, headers)
				requireArtifactProblem(t, response, test.status, test.code)
				if bytes.Contains(response.document, []byte(faultCanary)) {
					t.Fatalf("controlled backend detail leaked: %s", response.document)
				}
				if response.header.Get("ETag") != "" || response.header.Get("Content-Range") != "" ||
					response.header.Get("Accept-Ranges") != "" {
					t.Fatalf("backend fault advertised successful content: %#v", response.header)
				}
				if test.payload != nil && bytes.Contains(response.document, test.payload) {
					t.Fatalf("controlled corrupt object bytes leaked: %s", response.document)
				}
				verifyCalls, openCalls := backend.calls()
				if test.inspect && (verifyCalls != 1 || openCalls != 0) {
					t.Fatalf("inspection fault calls = verify %d/open %d; want 1/0", verifyCalls, openCalls)
				}
				if !test.inspect && (verifyCalls != 0 || openCalls != 1) {
					t.Fatalf("download fault calls = verify %d/open %d; want 0/1", verifyCalls, openCalls)
				}
			})
		}
	})
	t.Run("active_content_download_is_not_a_PM_origin_document", func(t *testing.T) {
		fixture, backend := newControlledArtifactFixture(t)
		payload := []byte("<!doctype html><script>document.title='artifact-script'</script>")
		digest := sha256.Sum256(payload)
		ref := artifactMetadataRef(fixture.Scope, "artifact-active-content")
		ref.SHA256 = hex.EncodeToString(digest[:])
		ref.ByteLength = int64(len(payload))
		ref.MediaType = "text/html"
		verified, err := store.NewPreverifiedObjectRef(ref, store.ObjectVerification{
			ObservedSHA256:            ref.SHA256,
			ObservedByteLength:        ref.ByteLength,
			ObservedBackendGeneration: ref.BackendGeneration,
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
			return store.RegisterArtifact(ctx, tx, fixture.Connection.ConnectionID, verified)
		}); err != nil {
			t.Fatal(err)
		}
		backend.register(ref, payload)
		response := sendArtifactRoute(t, fixture.Client, http.MethodGet, artifactPath(fixture, fixture.Scope, ref.ArtifactID)+"/content", nil)
		if response.status != http.StatusOK || !bytes.Equal(response.document, payload) {
			t.Fatalf("active content download changed: status=%d body=%q", response.status, response.document)
		}
		if response.header.Get("Content-Disposition") != "attachment" {
			t.Fatalf("untrusted active content may render in the authenticated PM origin: disposition=%q", response.header.Get("Content-Disposition"))
		}
	})
}

func newControlledArtifactFixture(
	t *testing.T,
) (*testfixture.Fixture, *controlledArtifactBackend) {
	t.Helper()
	backend := newControlledArtifactBackend()
	fixture := testfixture.New(t, func(
		_ testfixture.ServiceDependencies,
		configuration *httpapi.ServiceConfig,
	) error {
		configuration.Artifacts = backend
		return nil
	})
	return fixture, backend
}

func artifactETag(ref contract.ObjectRef) string {
	return `"sha256:` + ref.SHA256 + `"`
}

func registerArtifactMetadata(t *testing.T, fixture *testfixture.Fixture, artifactID string) contract.ObjectRef {
	t.Helper()
	return registerScopedArtifactMetadata(
		t,
		fixture,
		fixture.Scope,
		fixture.Actor,
		fixture.Connection.ConnectionID,
		artifactID,
	)
}

func registerScopedArtifactMetadata(
	t *testing.T,
	fixture *testfixture.Fixture,
	scope contract.Scope,
	actor contract.Actor,
	connectionID string,
	artifactID string,
) contract.ObjectRef {
	t.Helper()
	ref := artifactMetadataRef(scope, artifactID)
	verified, err := store.NewPreverifiedObjectRef(ref, store.ObjectVerification{
		ObservedSHA256:            ref.SHA256,
		ObservedByteLength:        ref.ByteLength,
		ObservedBackendGeneration: ref.BackendGeneration,
	})
	if err != nil {
		t.Fatalf("construct preverified artifact fixture metadata: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.WithinScope(ctx, fixture.Pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		return store.RegisterArtifact(ctx, tx, connectionID, verified)
	}); err != nil {
		t.Fatalf("register artifact fixture metadata: %v", err)
	}
	return ref
}

func insertUnboundArtifactMetadata(
	t *testing.T,
	fixture *testfixture.Fixture,
	artifactID string,
) contract.ObjectRef {
	t.Helper()
	ref := artifactMetadataRef(fixture.Scope, artifactID)
	encoded, err := json.Marshal(ref)
	if err != nil {
		t.Fatalf("encode unbound artifact fixture metadata: %v", err)
	}
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO pm.artifacts (
				org_id, workspace_id, artifact_id, object_attempt_key, sha256,
				byte_length, media_type, backend_generation, object_ref, registered_at, purpose
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, clock_timestamp(), 'public')
		`, ref.OrgID, ref.WorkspaceID, ref.ArtifactID, ref.ObjectAttemptKey, ref.SHA256,
			ref.ByteLength, ref.MediaType, ref.BackendGeneration, json.RawMessage(encoded))
		return err
	})
	return ref
}

func artifactMetadataRef(scope contract.Scope, artifactID string) contract.ObjectRef {
	digest := sha256.Sum256([]byte(artifactCanary))
	return contract.ObjectRef{
		OrgID:             scope.OrgID,
		WorkspaceID:       scope.WorkspaceID,
		ArtifactID:        artifactID,
		ObjectAttemptKey:  artifactID + "-attempt-1",
		SHA256:            hex.EncodeToString(digest[:]),
		ByteLength:        int64(len(artifactCanary)),
		MediaType:         "application/octet-stream",
		BackendGeneration: "fixture-generation-1",
	}
}

func grantArtifactWrite(t *testing.T, fixture *testfixture.Fixture) {
	t.Helper()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE pm.memberships
			SET permissions = ARRAY(
			        SELECT item.value
			        FROM unnest(permissions || ARRAY['artifact.write']) AS item(value)
			        GROUP BY item.value
			        ORDER BY item.value
			    ),
			    resource_grants = ARRAY(
			        SELECT item.value
			        FROM unnest(resource_grants || ARRAY['permission:artifact.write']) AS item(value)
			        GROUP BY item.value
			        ORDER BY item.value
			    )
			WHERE org_id = $1 AND workspace_id = $2 AND identity_id = $3
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, fixture.Actor.RequestedActor.PrincipalID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("artifact write fixture grant was not updated")
		}
		return nil
	})
}

func loadArtifactMetadata(t *testing.T, fixture *testfixture.Fixture, artifactID string) contract.ObjectRef {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ref contract.ObjectRef
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		ref, err = store.LoadArtifact(ctx, tx, artifactID)
		return err
	}); err != nil {
		t.Fatalf("load artifact fixture metadata: %v", err)
	}
	return ref
}

func artifactCollectionPath(fixture *testfixture.Fixture, scope contract.Scope) string {
	return fixture.BaseURL + "/v1/workspaces/" + url.PathEscape(scope.WorkspaceID) + "/artifacts"
}

func artifactPath(fixture *testfixture.Fixture, scope contract.Scope, artifactID string) string {
	return artifactCollectionPath(fixture, scope) + "/" + url.PathEscape(artifactID)
}

func sendArtifactRoute(
	t *testing.T,
	client *http.Client,
	method string,
	endpoint string,
	headers map[string]string,
) artifactRouteResponse {
	t.Helper()
	return sendArtifactRouteWithBody(t, client, method, endpoint, nil, headers)
}

func sendArtifactRouteWithBody(
	t *testing.T,
	client *http.Client,
	method string,
	endpoint string,
	body io.Reader,
	headers map[string]string,
) artifactRouteResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		t.Fatalf("construct artifact route request: %v", err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("send artifact route request: %v", err)
	}
	defer response.Body.Close()
	document, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(document) > 64*1024 {
		t.Fatal("artifact route response exceeded its bounded envelope")
	}
	return artifactRouteResponse{status: response.StatusCode, header: response.Header.Clone(), document: document}
}

func requireArtifactProblem(t *testing.T, response artifactRouteResponse, status int, code string) api.Problem {
	t.Helper()
	if !strings.HasPrefix(response.header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("artifact problem content type = %q, want application/problem+json: %s", response.header.Get("Content-Type"), response.document)
	}
	var problem api.Problem
	if err := json.Unmarshal(response.document, &problem); err != nil {
		t.Fatalf("decode artifact problem: %v: %s", err, response.document)
	}
	if response.status != status || problem.Status != status || problem.Code != code {
		t.Fatalf("artifact problem = HTTP %d %#v; want %d/%s", response.status, problem, status, code)
	}
	return problem
}
