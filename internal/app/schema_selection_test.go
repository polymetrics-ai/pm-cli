package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

// TestSchemaSelectionControl drives the schema selection route and the schema
// route against PM PostgreSQL in the owned stack fixture, after one real
// discovery through the production worker stored one complete candidate. The
// steps share one harness and run in order, so each step starts from the state
// that the earlier steps left. Developer check only, not acceptance proof.
//
// Defects that make a step fail:
//   - a route that applies a selection with no If-Match (428 step);
//   - a 428 check that runs before the replay check, or a replay that writes
//     again (replay step);
//   - a selection path that skips connection.manage (authorization step);
//   - a selection of a relation or candidate that the connection does not
//     hold (404 step);
//   - a revision that counts from 0, does not add 1, or is missing from the
//     schema route, its ETag or the select answer's ETag (first selection);
//   - a stale revision that writes (412 step).
func TestSchemaSelectionControl(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	harness := newDiscoveryHarness(t, temporal, sqlExecutionTuning{declared: true, noSnapshot: true})
	audit := newWorkspaceAuditReader(t, harness.fixture)
	run := harness.discover(t, temporal, "schema-selection-discovery")
	if run.state != contract.RunStateSucceeded || run.dials != 1 {
		t.Fatalf("discovery settlement = %#v; want one succeeded discovery", run)
	}
	candidates := harness.schemaCandidates(t)
	if len(candidates) != 1 {
		t.Fatalf("stored candidates = %d; want exactly one", len(candidates))
	}
	candidate := string(candidates[0].Fingerprint)

	t.Run("before any selection the schema route reports revision 1", func(t *testing.T) {
		read := harness.schemaRoute(t)
		if !read.inspection.Selected.IsNull() || read.inspection.SelectionRevision != 1 || read.etag != `"1"` {
			t.Fatalf("schema route before selection = %#v, ETag %q; want no selection at revision 1", read.inspection, read.etag)
		}
	})

	t.Run("a fresh request with no If-Match gets 428 and writes nothing", func(t *testing.T) {
		response := harness.postSelection(t, selectionCall{
			resource: sqlExecutionResource, schemaRef: candidate, revision: "1", requestKey: "schema-selection-no-if-match",
		})
		if response.status != http.StatusPreconditionRequired || response.problem.Code != "control_revision_required" {
			t.Fatalf("selection with no If-Match = HTTP %d %q; want 428 control_revision_required", response.status, response.problem.Code)
		}
		requireSchemaSelectionState(t, harness, audit, nil, 0)
	})

	t.Run("an undeclared resource or an unknown candidate gets 404 and writes nothing", func(t *testing.T) {
		for _, call := range []selectionCall{
			{resource: "customer.missing", schemaRef: candidate, revision: "1", ifMatch: "1", requestKey: "schema-selection-undeclared"},
			{resource: sqlExecutionResource, schemaRef: "sha256:" + strings.Repeat("0", 64), revision: "1", ifMatch: "1",
				requestKey: "schema-selection-unknown-candidate"},
		} {
			response := harness.postSelection(t, call)
			if response.status != http.StatusNotFound || response.problem.Code != "schema_relation_unknown" {
				t.Fatalf("selection of %s %s = HTTP %d %q; want 404 schema_relation_unknown",
					call.resource, call.schemaRef, response.status, response.problem.Code)
			}
		}
		requireSchemaSelectionState(t, harness, audit, nil, 0)
	})

	t.Run("a first selection at revision 1 selects the candidate and gives revision 2", func(t *testing.T) {
		response := harness.postSelection(t, selectionCall{
			resource: sqlExecutionResource, schemaRef: candidate, revision: "1", ifMatch: "1", requestKey: "schema-selection-first",
		})
		if response.status != http.StatusOK || response.etag != `"2"` || response.result.ResultRevision != "2" ||
			response.result.ResultDisposition != "created" || response.result.Replay ||
			response.result.ResourceID != harness.fixture.Connection.ConnectionID ||
			response.result.OutcomeReference.Kind != "connection" {
			t.Fatalf("first selection = HTTP %d, ETag %q, %#v; want 200, ETag \"2\", created at revision 2",
				response.status, response.etag, response.result)
		}
		requireSchemaSelectionState(t, harness, audit, &schemaSelectionRow{schemaRef: candidate, revision: 2}, 1)
		records := audit.records(t)
		if records[0].action != "schema.select" || records[0].targetRef != harness.fixture.Connection.ConnectionID ||
			records[0].requestedActorID != harness.fixture.Actor.RequestedActor.PrincipalID {
			t.Fatalf("selection audit record = %#v; want schema.select of the connection by the fixture actor", records[0])
		}
		read := harness.schemaRoute(t)
		selected, err := read.inspection.Selected.Get()
		if err != nil || selected != candidate || read.inspection.SelectionRevision != 2 || read.etag != `"2"` ||
			!read.inspection.Snapshot.Complete || len(read.inspection.Fields) != len(sqlExecutionColumns()) {
			t.Fatalf("schema route after selection = %#v, ETag %q; want the candidate at revision 2", read.inspection, read.etag)
		}
	})

	t.Run("a replay of the stored receipt with no If-Match returns the stored result and writes nothing", func(t *testing.T) {
		response := harness.postSelection(t, selectionCall{
			resource: sqlExecutionResource, schemaRef: candidate, revision: "1", requestKey: "schema-selection-first",
		})
		if response.status != http.StatusOK || !response.result.Replay || response.result.ResultRevision != "2" ||
			response.result.ResultDisposition != "prior_mutation_result" || response.etag != `"2"` {
			t.Fatalf("replay with no If-Match = HTTP %d %q, ETag %q, %#v; want the stored result at revision 2",
				response.status, response.problem.Code, response.etag, response.result)
		}
		requireSchemaSelectionState(t, harness, audit, &schemaSelectionRow{schemaRef: candidate, revision: 2}, 1)
	})

	t.Run("a stale revision gets 412 and changes nothing", func(t *testing.T) {
		response := harness.postSelection(t, selectionCall{
			resource: sqlExecutionResource, schemaRef: candidate, revision: "1", ifMatch: "1", requestKey: "schema-selection-stale",
		})
		if response.status != http.StatusPreconditionFailed || response.problem.Code != "control_revision_mismatch" {
			t.Fatalf("stale selection = HTTP %d %q; want 412 control_revision_mismatch", response.status, response.problem.Code)
		}
		requireSchemaSelectionState(t, harness, audit, &schemaSelectionRow{schemaRef: candidate, revision: 2}, 1)
	})

	t.Run("a user without connection.manage is refused and changes nothing", func(t *testing.T) {
		harness.removeConnectionManage(t)
		response := harness.postSelection(t, selectionCall{
			resource: sqlExecutionResource, schemaRef: candidate, revision: "2", ifMatch: "2", requestKey: "schema-selection-no-manage",
		})
		if response.status != http.StatusForbidden && response.status != http.StatusNotFound {
			t.Fatalf("selection without connection.manage = HTTP %d %q; want an authorization refusal", response.status, response.problem.Code)
		}
		requireSchemaSelectionState(t, harness, audit, &schemaSelectionRow{schemaRef: candidate, revision: 2}, 1)
		// The same user still reads the schema with connection.read.
		if read := harness.schemaRoute(t); read.inspection.SelectionRevision != 2 {
			t.Fatalf("schema route after the refused selection = revision %d; want 2", read.inspection.SelectionRevision)
		}
	})
}

type selectionCall struct {
	resource   string
	schemaRef  string
	revision   string
	ifMatch    string
	requestKey string
}

// selectionResult holds the members that the fresh and the replay answers of
// the select route share.
type selectionResult struct {
	ResourceID        string                   `json:"resource_id"`
	ResultRevision    string                   `json:"result_revision"`
	ResultDisposition string                   `json:"result_disposition"`
	Replay            bool                     `json:"replay"`
	OutcomeReference  api.SafeOutcomeReference `json:"outcome_reference"`
}

type selectionResponse struct {
	status  int
	etag    string
	result  selectionResult
	problem api.Problem
}

// postSelection sends one request to the select route as the fixture actor.
// An empty ifMatch sends no If-Match header.
func (harness *sqlExecutionHarness) postSelection(t *testing.T, call selectionCall) selectionResponse {
	t.Helper()
	var body api.SchemaSelectionRequest
	body.Protocol = api.ProtocolMetadata{
		WireVersion:      api.ProtocolMetadataWireVersion(api.WireVersion),
		RequiredFeatures: []string{},
		RequestKey:       call.requestKey,
		IntentDigest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		ExpectedRevision: call.revision,
	}
	body.Selection.Resource = call.resource
	body.Selection.SchemaRef = call.schemaRef
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		harness.fixture.BaseURL+"/v1/workspaces/"+url.PathEscape(harness.fixture.Scope.WorkspaceID)+
			"/connections/"+url.PathEscape(harness.fixture.Connection.ConnectionID)+"/schema/selections",
		bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", call.requestKey)
	if call.ifMatch != "" {
		request.Header.Set("If-Match", call.ifMatch)
	}
	response, err := harness.fixture.Client.Do(request)
	if err != nil {
		t.Fatalf("send selection request: %v", err)
	}
	defer response.Body.Close()
	document, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		t.Fatal(err)
	}
	result := selectionResponse{status: response.StatusCode, etag: response.Header.Get("ETag")}
	if response.StatusCode == http.StatusOK {
		err = json.Unmarshal(document, &result.result)
	} else {
		err = json.Unmarshal(document, &result.problem)
	}
	if err != nil {
		t.Fatalf("decode selection response HTTP %d: %v", response.StatusCode, err)
	}
	return result
}

type schemaRouteRead struct {
	inspection api.ConnectionSchemaInspection
	etag       string
}

// schemaRoute reads the schema route for the declared resource and keeps its
// ETag header.
func (harness *sqlExecutionHarness) schemaRoute(t *testing.T) schemaRouteRead {
	t.Helper()
	query := url.Values{}
	query.Set("resource", sqlExecutionResource)
	query.Set("permission_revision", sqlExecutionPermissionRev)
	query.Set("source_revision", sqlExecutionSourceRevision)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		harness.fixture.BaseURL+"/v1/workspaces/"+url.PathEscape(harness.fixture.Scope.WorkspaceID)+
			"/connections/"+url.PathEscape(harness.fixture.Connection.ConnectionID)+"/schema?"+query.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := harness.fixture.Client.Do(request)
	if err != nil {
		t.Fatalf("read the schema route: %v", err)
	}
	defer response.Body.Close()
	document, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("schema route = HTTP %d: %s", response.StatusCode, document)
	}
	read := schemaRouteRead{etag: response.Header.Get("ETag")}
	if err := json.Unmarshal(document, &read.inspection); err != nil {
		t.Fatalf("decode schema inspection: %v", err)
	}
	return read
}

type schemaSelectionRow struct {
	resource  string
	schemaRef string
	revision  int64
}

// schemaSelectionRows reads every selection row of the fixture connection.
func (harness *sqlExecutionHarness) schemaSelectionRows(t *testing.T) []schemaSelectionRow {
	t.Helper()
	var rows []schemaSelectionRow
	harness.fixture.SetupSQL(t, harness.fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		result, err := tx.Query(ctx, `
			SELECT resource, schema_ref, selection_revision FROM pm.schema_selections
			WHERE connection_id = $1 ORDER BY resource
		`, harness.fixture.Connection.ConnectionID)
		if err != nil {
			return err
		}
		defer result.Close()
		for result.Next() {
			var row schemaSelectionRow
			if err := result.Scan(&row.resource, &row.schemaRef, &row.revision); err != nil {
				return err
			}
			rows = append(rows, row)
		}
		return result.Err()
	})
	return rows
}

// requireSchemaSelectionState requires the one selection row of the declared
// resource, or none, and the count of workspace audit records.
func requireSchemaSelectionState(
	t *testing.T,
	harness *sqlExecutionHarness,
	audit workspaceAuditReader,
	want *schemaSelectionRow,
	auditRecords int,
) {
	t.Helper()
	rows := harness.schemaSelectionRows(t)
	switch {
	case want == nil && len(rows) != 0:
		t.Fatalf("selection rows = %#v; want none", rows)
	case want != nil && (len(rows) != 1 || rows[0].resource != sqlExecutionResource ||
		rows[0].schemaRef != want.schemaRef || rows[0].revision != want.revision):
		t.Fatalf("selection rows = %#v; want only %s at revision %d", rows, want.schemaRef, want.revision)
	}
	if records := audit.records(t); len(records) != auditRecords {
		t.Fatalf("workspace audit records = %#v; want %d", records, auditRecords)
	}
}

// removeConnectionManage removes connection.manage from the fixture actor's
// membership, and keeps connection.read.
func (harness *sqlExecutionHarness) removeConnectionManage(t *testing.T) {
	t.Helper()
	harness.fixture.SetupSQL(t, harness.fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE pm.memberships
			SET permissions = array_remove(permissions, 'connection.manage'),
			    resource_grants = array_remove(resource_grants, 'permission:connection.manage'),
			    policy_epoch = policy_epoch + 1,
			    control_revision = control_revision + 1
			WHERE identity_id = $1
		`, harness.fixture.Actor.RequestedActor.PrincipalID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("remove connection.manage: %d rows", tag.RowsAffected())
		}
		return nil
	})
}

// workspaceAuditReader reads and removes the workspace audit records of the
// fixture organization as the stack administrator. Row-level security lets the
// runtime role insert these records only, and the migrator cannot read them.
type workspaceAuditReader struct {
	adminDSN string
	orgID    string
}

type workspaceAuditRecord struct {
	action           string
	targetRef        string
	requestedActorID string
}

func newWorkspaceAuditReader(t *testing.T, fixture *testfixture.Fixture) workspaceAuditReader {
	t.Helper()
	directory := os.Getenv("PM_STORE_TEST_SECRETS_DIR")
	password, err := os.ReadFile(filepath.Join(directory, "superuser-password"))
	if err != nil || strings.TrimSpace(string(password)) == "" {
		t.Fatal("read the stack administrator credential")
	}
	defer clear(password)
	endpoint, err := os.ReadFile(filepath.Join(directory, "pm-app-endpoint"))
	if err != nil {
		t.Fatal("read pm-app-endpoint")
	}
	parsed, err := url.Parse(strings.TrimSpace(string(endpoint)))
	if err != nil {
		t.Fatal("parse pm-app-endpoint")
	}
	parsed.User = url.UserPassword("pm_stack_admin", strings.TrimSpace(string(password)))
	reader := workspaceAuditReader{adminDSN: parsed.String(), orgID: fixture.Scope.OrgID}
	// This cleanup runs before the fixture cleanup, which removes the
	// workspaces that these records name.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		connection, err := pgx.Connect(ctx, reader.adminDSN)
		if err != nil {
			t.Errorf("connect the stack administrator for cleanup: %T", err)
			return
		}
		defer func() { _ = connection.Close(context.Background()) }()
		if _, err := connection.Exec(ctx, `DELETE FROM pm.workspace_audit_entries WHERE org_id = $1`, reader.orgID); err != nil {
			t.Errorf("remove the workspace audit records: %v", err)
		}
	})
	return reader
}

func (reader workspaceAuditReader) records(t *testing.T) []workspaceAuditRecord {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, reader.adminDSN)
	if err != nil {
		t.Fatalf("connect the stack administrator: %T", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	rows, err := connection.Query(ctx, `
		SELECT action, target_ref, requested_actor_id
		FROM pm.workspace_audit_entries
		WHERE org_id = $1
		ORDER BY recorded_at, audit_id
	`, reader.orgID)
	if err != nil {
		t.Fatalf("read the workspace audit records: %v", err)
	}
	defer rows.Close()
	var records []workspaceAuditRecord
	for rows.Next() {
		var record workspaceAuditRecord
		if err := rows.Scan(&record.action, &record.targetRef, &record.requestedActorID); err != nil {
			t.Fatalf("scan a workspace audit record: %v", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the workspace audit records: %v", err)
	}
	return records
}
