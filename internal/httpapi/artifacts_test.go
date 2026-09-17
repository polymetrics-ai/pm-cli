package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

const artifactCanary = "fixture-artifact-bytes-must-not-be-fabricated"

type artifactRouteResponse struct {
	status   int
	header   http.Header
	document []byte
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
				byte_length, media_type, backend_generation, object_ref, registered_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, clock_timestamp())
		`, ref.OrgID, ref.WorkspaceID, ref.ArtifactID, ref.ObjectAttemptKey, ref.SHA256,
			ref.ByteLength, ref.MediaType, ref.BackendGeneration, json.RawMessage(encoded))
		return err
	})
	return ref
}

func artifactMetadataRef(scope contract.Scope, artifactID string) contract.ObjectRef {
	return contract.ObjectRef{
		OrgID:             scope.OrgID,
		WorkspaceID:       scope.WorkspaceID,
		ArtifactID:        artifactID,
		ObjectAttemptKey:  artifactID + "-attempt-1",
		SHA256:            strings.Repeat("a", 64),
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
