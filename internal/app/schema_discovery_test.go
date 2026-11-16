package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/schema"
)

// TestSchemaDiscoveryStoresOnlyACompleteCandidate drives the discovery route,
// the production planner and the shared SQL callback against an isolated
// customer database in the owned stack fixture. A discovery stores one complete
// candidate and selects nothing. A discovery with no stored credential, or with
// a source that fails, stores no candidate. Developer check only.
func TestSchemaDiscoveryStoresOnlyACompleteCandidate(t *testing.T) {
	temporal := newPermitTemporalHarness(t)

	t.Run("a first discovery with no selection stores one complete candidate and selects nothing", func(t *testing.T) {
		harness := newSQLExecutionHarness(t, temporal, sqlExecutionMetadataID, sqlExecutionTuning{declared: true, noSnapshot: true})
		run := harness.discover(t, temporal, "schema-discovery-complete")
		if run.state != contract.RunStateSucceeded || run.unknownEffects != 0 ||
			run.outcome != contract.OutcomeSucceeded || run.attempts != 1 || run.dials != 1 {
			t.Fatalf("discovery settlement = %#v; want one succeeded admitted attempt", run)
		}
		candidates := harness.schemaCandidates(t)
		if len(candidates) != 1 {
			t.Fatalf("stored candidates = %d; want exactly one", len(candidates))
		}
		candidate := candidates[0]
		if !candidate.Completeness.Complete || len(candidate.Completeness.Gaps) != 0 {
			t.Fatalf("stored candidate completeness = %#v; want complete", candidate.Completeness)
		}
		if candidate.Binding.Resource != sqlExecutionResource || candidate.Binding.ConnectionID != harness.fixture.Connection.ConnectionID {
			t.Fatalf("stored candidate binding = %#v", candidate.Binding)
		}
		fields := make(map[string]schema.Field, len(candidate.Fields))
		for _, field := range candidate.Fields {
			fields[field.Name] = field
		}
		columns := sqlExecutionColumns()
		if len(fields) != len(columns) {
			t.Fatalf("stored fields = %d; want one for each live column", len(fields))
		}
		for _, column := range columns {
			field, present := fields[column.name]
			if !present || field.Schema.Type != column.schema || field.Precision != column.precision || field.Scale != column.scale {
				t.Fatalf("stored field %q = %#v; want type %s (%d,%d)", column.name, field, column.schema, column.precision, column.scale)
			}
		}
		if _, selected, err := harness.customer.snapshots.Selection(); err != nil || selected {
			t.Fatalf("connection selection after discovery = %v, %v; want no selection", selected, err)
		}
	})

	t.Run("a discovery with no stored credential stores no candidate", func(t *testing.T) {
		harness := newSQLExecutionHarness(t, temporal, sqlExecutionMetadataID, sqlExecutionTuning{declared: true, noSnapshot: true, noCredential: true})
		run := harness.discover(t, temporal, "schema-discovery-no-credential")
		if run.outcome != contract.OutcomeUnsent || run.unknownEffects != 0 || run.dials != 0 {
			t.Fatalf("discovery without a credential = %#v; want a known-unsent refusal before any send", run)
		}
		if candidates := harness.schemaCandidates(t); len(candidates) != 0 {
			t.Fatalf("discovery without a credential stored %d candidates", len(candidates))
		}
	})

	t.Run("a discovery whose source relation is gone stores no candidate", func(t *testing.T) {
		harness := newSQLExecutionHarness(t, temporal, sqlExecutionMetadataID, sqlExecutionTuning{declared: true, noSnapshot: true})
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		harness.customer.ownerExec(t, ctx, `DROP TABLE "`+sqlExecutionSchema+`"."`+sqlExecutionRelation+`"`)
		run := harness.discover(t, temporal, "schema-discovery-source-gone")
		if run.state == contract.RunStateSucceeded || run.outcome != contract.OutcomeRejected || run.unknownEffects != 0 || run.dials != 1 {
			t.Fatalf("discovery of a missing relation = %#v; want one rejected attempt", run)
		}
		if candidates := harness.schemaCandidates(t); len(candidates) != 0 {
			t.Fatalf("failed discovery stored %d candidates", len(candidates))
		}
	})

	t.Run("an undeclared resource is refused before any run", func(t *testing.T) {
		harness := newSQLExecutionHarness(t, temporal, sqlExecutionMetadataID, sqlExecutionTuning{declared: true, noSnapshot: true})
		response := harness.postDiscovery(t, "customer.missing", "schema-discovery-undeclared")
		if response.status != http.StatusNotFound || response.problemCode != "schema_relation_unknown" {
			t.Fatalf("undeclared discovery = HTTP %d %q; want 404 schema_relation_unknown", response.status, response.problemCode)
		}
		if harness.customer.dials.Load() != 0 || len(harness.schemaCandidates(t)) != 0 || harness.discoveryIntents(t) != 0 {
			t.Fatal("a refused discovery dialed the source or wrote a discovery row")
		}
	})
}

// discover admits one discovery of the declared resource through the route and
// runs it through the production worker.
func (harness *sqlExecutionHarness) discover(t *testing.T, temporal *permitTemporalHarness, requestKey string) sqlExecutionRun {
	t.Helper()
	response := harness.postDiscovery(t, sqlExecutionResource, requestKey)
	admitted := requireFreshInitialAdmission(t, response, requestKey)
	return harness.executeAdmitted(t, temporal, sqlExecutionMetadataID, admitted)
}

func (harness *sqlExecutionHarness) postDiscovery(t *testing.T, resource, requestKey string) initialRunResponse {
	t.Helper()
	encoded, err := json.Marshal(api.SchemaDiscoveryRequest{Resource: resource})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		harness.fixture.BaseURL+"/v1/workspaces/"+url.PathEscape(harness.fixture.Scope.WorkspaceID)+
			"/connections/"+url.PathEscape(harness.fixture.Connection.ConnectionID)+"/schema/discoveries",
		bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", requestKey)
	response, err := harness.fixture.Client.Do(request)
	if err != nil {
		t.Fatalf("send discovery request: %v", err)
	}
	defer response.Body.Close()
	document, err := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if err != nil {
		t.Fatal(err)
	}
	result := initialRunResponse{status: response.StatusCode, document: document}
	if response.StatusCode == http.StatusAccepted {
		err = json.Unmarshal(document, &result.admission)
	} else {
		err = json.Unmarshal(document, &result.problem)
		result.problemCode = result.problem.Code
	}
	if err != nil {
		t.Fatalf("decode discovery response HTTP %d: %v", response.StatusCode, err)
	}
	return result
}

// schemaCandidates reads the stored candidates of the declared resource and
// decodes each one through the strict snapshot decoder.
func (harness *sqlExecutionHarness) schemaCandidates(t *testing.T) []schema.Snapshot {
	t.Helper()
	var documents [][]byte
	harness.fixture.SetupSQL(t, harness.fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT document::text FROM pm.schema_snapshots
			WHERE connection_id = $1 AND resource = $2
			ORDER BY schema_ref
		`, harness.fixture.Connection.ConnectionID, sqlExecutionResource)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var document string
			if err := rows.Scan(&document); err != nil {
				return err
			}
			documents = append(documents, []byte(document))
		}
		return rows.Err()
	})
	snapshots := make([]schema.Snapshot, 0, len(documents))
	for _, document := range documents {
		snapshot, err := schema.DecodeSnapshot(document)
		if err != nil {
			t.Fatalf("stored candidate does not decode as a snapshot: %v", err)
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots
}

func (harness *sqlExecutionHarness) discoveryIntents(t *testing.T) int {
	t.Helper()
	var count int
	harness.fixture.SetupSQL(t, harness.fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pm.schema_discoveries`).Scan(&count); err != nil {
			return fmt.Errorf("count discovery intents: %w", err)
		}
		return nil
	})
	return count
}
