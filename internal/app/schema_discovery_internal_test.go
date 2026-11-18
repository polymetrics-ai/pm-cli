package app

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	pmruntime "github.com/polymetrics-ai/pm-cli/internal/runtime"
	"github.com/polymetrics-ai/pm-cli/internal/schema"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// These checks need no dependency stack. They drive the discovery projection
// and the settlement gate with the observations the PostgreSQL transport
// returns, and they are developer checks only, never acceptance proof.

var discoveryTestScope = contract.Scope{OrgID: "org-discovery", WorkspaceID: "workspace-discovery"}

func discoveryTestIntent() store.SchemaDiscovery {
	return store.SchemaDiscovery{
		RunID:              "run-discovery",
		ConnectionID:       "connection-discovery",
		ConnectorID:        "postgres",
		Resource:           "customer.records",
		RelationBinding:    "postgres.reusable-relation",
		Schema:             "customer",
		Table:              "records",
		ProviderAccountID:  "account-discovery",
		PermissionRevision: "permissions-v1",
		SourceRevision:     "postgres16-source-v1",
	}
}

func discoveryTestBound() sqlBoundOperation {
	binding := discoveryBinding(discoveryTestScope, discoveryTestIntent())
	return sqlBoundOperation{
		relation:  pmruntime.SQLRelation{Binding: "postgres.reusable-relation", Schema: "customer", Name: "records"},
		writable:  map[string]struct{}{},
		discovery: &binding,
	}
}

func discoveryTestRequest() ExecutionRequest {
	return ExecutionRequest{
		Scope:        discoveryTestScope,
		ConnectionID: "connection-discovery",
		Operation:    contract.Operation{OperationID: "relation/metadata"},
		Dispatch: contract.Dispatch{
			DispatchID:       "dispatch-discovery",
			OrderedMemberIDs: []contract.MemberID{"member-discovery"},
		},
		Attempt: contract.Attempt{ReceiptRef: "receipt-discovery"},
	}
}

func discoveryTestRow(name, kind string) pmruntime.SQLRow {
	return pmruntime.SQLRow{
		Columns: []string{pmruntime.SQLMetadataColumnName, pmruntime.SQLMetadataColumnType},
		Values:  []pmruntime.SQLValue{pmruntime.SQLTextValue(name), pmruntime.SQLTextValue(kind)},
	}
}

// discoveryTestRows are the metadata rows of one table that uses every type the
// read path binds exactly.
func discoveryTestRows() []pmruntime.SQLRow {
	return []pmruntime.SQLRow{
		discoveryTestRow("record_id", "bigint"),
		discoveryTestRow("amount", "numeric(9,4)"),
		discoveryTestRow("flagged", "boolean"),
		discoveryTestRow("note", "text"),
		discoveryTestRow("observed_at", "timestamp with time zone"),
		discoveryTestRow("local_wall", "timestamp without time zone"),
	}
}

func TestDiscoveredFieldsKeepTheExactLiveColumnTypes(t *testing.T) {
	binding := discoveryBinding(discoveryTestScope, discoveryTestIntent())
	candidate, err := discoverRelation(binding, "relation/metadata", discoveryTestRows())
	if err != nil {
		t.Fatalf("discover the declared relation: %v", err)
	}
	if !candidate.Completeness.Complete || len(candidate.Completeness.Gaps) != 0 {
		t.Fatalf("completeness = %#v; want complete with no gap", candidate.Completeness)
	}
	want := map[string]struct {
		kind      contract.SchemaType
		precision int
		scale     int
	}{
		"record_id":   {kind: contract.SchemaTypeInteger},
		"amount":      {kind: contract.SchemaTypeDecimal, precision: 9, scale: 4},
		"flagged":     {kind: contract.SchemaTypeBool},
		"note":        {kind: contract.SchemaTypeString},
		"observed_at": {kind: contract.SchemaTypeTimestampInstant},
		"local_wall":  {kind: contract.SchemaTypeTimestampLocal},
	}
	if len(candidate.Fields) != len(want) {
		t.Fatalf("fields = %d; want one field for each live column", len(candidate.Fields))
	}
	for _, field := range candidate.Fields {
		expected, present := want[field.Name]
		if !present || field.Schema.Type != expected.kind || field.Precision != expected.precision ||
			field.Scale != expected.scale || field.Required || field.Writable {
			t.Fatalf("field %#v; want %#v with no declared requiredness or writability", field, expected)
		}
		// The read path must bind the discovered field back to the same live type.
		kind, precision, scale, err := sqlColumnKind(field)
		if err != nil || precision != expected.precision || scale != expected.scale || kind == "" {
			t.Fatalf("read-path kind of %q = %q (%d,%d) %v", field.Name, kind, precision, scale, err)
		}
	}

	withUnsupported := append(discoveryTestRows(), discoveryTestRow("token", "uuid"))
	candidate, err = discoverRelation(binding, "relation/metadata", withUnsupported)
	if err != nil {
		t.Fatalf("discover a relation with an unsupported column: %v", err)
	}
	if len(candidate.Fields) != len(want) || len(candidate.Completeness.Gaps) != 1 ||
		candidate.Completeness.Gaps[0].Kind != schema.GapUnsupportedShape || candidate.Completeness.Gaps[0].Subject != "token" {
		t.Fatalf("unsupported column = %#v; want an explicit unsupported_shape gap and no invented field", candidate.Completeness)
	}

	candidate, err = discoverRelation(binding, "relation/metadata", nil)
	if err != nil {
		t.Fatalf("discover an absent relation: %v", err)
	}
	if candidate.Completeness.Complete {
		t.Fatal("a relation with no live columns was reported as a complete discovery")
	}
}

// TestFailedOrPartialDiscoveryStoresNoCandidate is the 285b developer check:
// a discovery with no stored credential, or with a source that fails, stores
// no candidate. Defect it catches: a partial or failed discovery stores one.
func TestFailedOrPartialDiscoveryStoresNoCandidate(t *testing.T) {
	observedAt := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		observation pmruntime.SQLObservation
		executeErr  error
		stores      bool
	}{
		{
			// The credential resolves before the connection opens, so the
			// attempt is a known rollback with no physical send.
			name:        "no stored credential",
			observation: pmruntime.SQLObservation{Outcome: pmruntime.SQLOutcomeKnownRollback, ObservedAt: observedAt},
			executeErr:  ErrSQLCredentialUnavailable,
		},
		{
			name:        "the source refuses the connection",
			observation: pmruntime.SQLObservation{Outcome: pmruntime.SQLOutcomeKnownRollback, PhysicalSend: true, ObservedAt: observedAt},
			executeErr:  &pmruntime.SQLAttemptError{Outcome: pmruntime.SQLOutcomeKnownRollback},
		},
		{
			name:        "the source loses the commit reply",
			observation: pmruntime.SQLObservation{Outcome: pmruntime.SQLOutcomeUnknownCommit, PhysicalSend: true, Rows: discoveryTestRows(), ObservedAt: observedAt},
			executeErr:  &pmruntime.SQLAttemptError{Outcome: pmruntime.SQLOutcomeUnknownCommit},
		},
		{
			name:        "the declared relation does not exist at the source",
			observation: pmruntime.SQLObservation{Outcome: pmruntime.SQLOutcomeKnownCommit, PhysicalSend: true, ObservedAt: observedAt},
		},
		{
			name: "the metadata exceeds the declared row bound",
			observation: pmruntime.SQLObservation{
				Outcome: pmruntime.SQLOutcomeKnownCommit, PhysicalSend: true, Rows: discoveryTestRows(), Truncated: true, ObservedAt: observedAt,
			},
		},
		{
			name:        "a complete discovery",
			observation: pmruntime.SQLObservation{Outcome: pmruntime.SQLOutcomeKnownCommit, PhysicalSend: true, Rows: discoveryTestRows(), ObservedAt: observedAt},
			stores:      true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			observation, callbackErr := discoveryTestBound().projectDiscovery(
				discoveryTestRequest(), testCase.observation, testCase.executeErr,
			)
			candidate, stored, err := settledDiscoveryCandidate(
				discoveryTestScope, discoveryTestIntent(), observation.Discovery, observation.Outcomes, callbackErr,
			)
			if err != nil {
				t.Fatalf("settlement gate: %v", err)
			}
			if stored != testCase.stores {
				t.Fatalf("stored = %v; want %v (outcomes %#v, callback error %v)", stored, testCase.stores, observation.Outcomes, callbackErr)
			}
			if !stored {
				if observation.Discovery != nil {
					t.Fatal("a failed or partial discovery carried an observed snapshot")
				}
				return
			}
			if candidate.Resource != "customer.records" || candidate.FieldCount != len(discoveryTestRows()) ||
				candidate.DiscoveredByRunID != "run-discovery" || candidate.SchemaRef != string(observation.Discovery.Fingerprint) {
				t.Fatalf("stored candidate = %#v", candidate)
			}
			decoded, err := schema.DecodeSnapshot(candidate.Document)
			if err != nil || !decoded.Completeness.Complete || decoded.Binding != discoveryBinding(discoveryTestScope, discoveryTestIntent()) {
				t.Fatalf("stored document = %#v, %v; want the complete snapshot of the accepted binding", decoded, err)
			}
		})
	}

	t.Run("a succeeded member with a callback error stores nothing", func(t *testing.T) {
		observation, _ := discoveryTestBound().projectDiscovery(
			discoveryTestRequest(),
			pmruntime.SQLObservation{Outcome: pmruntime.SQLOutcomeKnownCommit, Rows: discoveryTestRows(), ObservedAt: observedAt},
			nil,
		)
		_, stored, err := settledDiscoveryCandidate(
			discoveryTestScope, discoveryTestIntent(), observation.Discovery, observation.Outcomes, errors.New("late callback failure"),
		)
		if err != nil || stored {
			t.Fatalf("stored = %v, %v; want no candidate", stored, err)
		}
	})

	t.Run("an observed snapshot of another binding is refused", func(t *testing.T) {
		observation, callbackErr := discoveryTestBound().projectDiscovery(
			discoveryTestRequest(),
			pmruntime.SQLObservation{Outcome: pmruntime.SQLOutcomeKnownCommit, Rows: discoveryTestRows(), ObservedAt: observedAt},
			nil,
		)
		other := discoveryTestIntent()
		other.Resource = "customer.other"
		if _, stored, err := settledDiscoveryCandidate(
			discoveryTestScope, other, observation.Discovery, observation.Outcomes, callbackErr,
		); err == nil || stored {
			t.Fatalf("stored = %v, %v; want a refusal", stored, err)
		}
	})
}

// discoveryDeclarationDocument is one complete startup document whose
// provider_sql declares one customer connection with one relation binding of
// the given physical schema and table. Every other fact stays the same, so
// two documents differ only in the physical relation.
func discoveryDeclarationDocument(schemaName, tableName string) string {
	return `{"schema_version":"1","deployment_id":"discovery-binding-check","public_origin":"https://pm.example.invalid",` +
		`"postgres":{"ownership":"managed","database":"pm_app","endpoint_ref":"secret://fixture/postgres"},` +
		`"temporal":{"ownership":"managed","endpoint":"temporal.example.invalid:7233","namespace":"fixture","client_identity_ref":"secret://fixture/temporal","retention":"720h","task_queues":["pm-control"]},` +
		`"artifacts":{"ownership":"managed","driver":"s3","endpoint":"https://objects.example.invalid","bucket":"fixture-objects","credential_ref":"secret://fixture/objects","attempt_key_policy":"unique_immutable"},` +
		`"identity":{"issuer":"https://identity.example.invalid","client_id":"pm-server","client_secret_ref":"secret://fixture/oidc"},` +
		`"encryption":{"wrapping_key_ref":"secret://fixture/wrapping"},` +
		`"admission":{"profile":"development"},` +
		`"limits":{"workspace_active_runs":4,"workflow_payload_bytes":65536,"workflow_fanout":32,"run_lifetime":"1h","event_buffer":128,"query_children":1,"query_child_memory_bytes":4294967296,"query_child_deadline":"5m","request_bytes":1048576,"metadata_deadline":"5s"},` +
		`"provider_sql":{"max_connect_duration":"5s","max_statement_duration":"5s","max_total_duration":"10s","max_rows":100,"max_bytes":65536,"max_attempts":1,` +
		`"connections":[{"org_id":"org-discovery","workspace_id":"workspace-discovery","connection_id":"connection-discovery",` +
		`"connector_id":"postgres","host":"customer.example.invalid","port":5432,"database":"customer","user":"pm_reader",` +
		`"tls_trust_ref":"secret://fixture/provider-sql","password_ref":"secret://fixture/customer",` +
		`"state_directory":"/nonexistent/pm-discovery-state",` +
		`"relation_bindings":[{"binding":"postgres.reusable-relation","schema":"` + schemaName + `","table":"` + tableName + `",` +
		`"key_columns":["record_id"],"snapshot":{"provider_account_id":"account-discovery","permission_revision":"permissions-v1",` +
		`"resource":"customer.records","source_revision":"postgres16-source-v1"}}]}]}}`
}

// discoveryDeclaredComposition builds the declared state of one process from
// a validated startup document: the connection, its relation bindings and the
// connector's declared metadata operation of the binding. It opens no
// connection and reads no credential.
func discoveryDeclaredComposition(t *testing.T, schemaName, tableName string) *SQLComposition {
	t.Helper()
	document, err := config.Load(strings.NewReader(discoveryDeclarationDocument(schemaName, tableName)))
	if err != nil {
		t.Fatalf("load the discovery declaration: %v", err)
	}
	resolved, err := config.Validate(document, config.Prelaunch{})
	if err != nil {
		t.Fatalf("validate the discovery declaration: %v", err)
	}
	connections := resolved.ProviderSQL().Connections()
	if len(connections) != 1 || len(connections[0].Bindings) != 1 {
		t.Fatalf("declared connections = %#v; want one connection with one binding", connections)
	}
	connection := connections[0]
	scope := connection.Scope()
	binding := connection.Bindings[0]
	return &SQLComposition{
		declared: true,
		connections: map[sqlConnectionKey]sqlBoundConnection{
			{orgID: scope.OrgID, workspaceID: scope.WorkspaceID, connectionID: connection.ConnectionID}: {
				declaration: connection,
				bindings:    map[string]config.ProviderSQLRelationBinding{binding.Binding: binding},
			},
		},
		metadata: map[sqlDeclaredRelation]string{
			{connectorID: connection.ConnectorID, relationBinding: binding.Binding}: "relation/metadata",
		},
	}
}

// TestDiscoveryRefusesAnotherPhysicalRelationUnderTheSameBinding is the
// developer check of #301 review finding D1. Defect: the worker compares the
// binding name and the four snapshot fields with the accepted intent, but it
// takes the physical schema and table from its own current declaration. A
// worker that restarts with another schema or table under the same binding
// then reads another relation and can store its columns under the accepted
// binding. bindDiscovery runs before the SQL transport (execute), so a refusal
// here sends nothing and the refused observation stores no candidate.
func TestDiscoveryRefusesAnotherPhysicalRelationUnderTheSameBinding(t *testing.T) {
	operation := contract.Operation{
		OperationID: "relation/metadata",
		Transport: contract.Transport{
			StatementKind:   contract.PostgreSQLStatementMetadata,
			RelationBinding: "postgres.reusable-relation",
		},
	}
	admission := discoveryDeclaredComposition(t, "customer", "records")
	declared, found := admission.ResolveDiscovery(discoveryTestScope, "connection-discovery", "customer.records")
	if !found {
		t.Fatal("admission did not resolve the declared relation")
	}
	intent := declared.intent("run-discovery", "connection-discovery")

	same := discoveryDeclaredComposition(t, "customer", "records")
	bound, err := same.bindDiscovery(discoveryTestScope, "connection-discovery", operation, intent)
	if err != nil {
		t.Fatalf("bind the unchanged declaration: %v", err)
	}
	if bound.relation.Schema != "customer" || bound.relation.Name != "records" {
		t.Fatalf("bound relation = %#v; want customer.records", bound.relation)
	}

	for _, changed := range []struct {
		name, schema, table string
	}{
		{name: "another schema", schema: "archive", table: "records"},
		{name: "another table", schema: "customer", table: "records_old"},
	} {
		worker := discoveryDeclaredComposition(t, changed.schema, changed.table)
		bound, err := worker.bindDiscovery(discoveryTestScope, "connection-discovery", operation, intent)
		if !errors.Is(err, ErrSQLBindingUnavailable) {
			t.Errorf("%s: bindDiscovery = %#v, %v; want ErrSQLBindingUnavailable before any send",
				changed.name, bound.relation, err)
		}
	}
}

// TestSelectionCommitOutcomeUnknownIsReconcileOnly is the developer check of
// #401 review finding QA-401-1. Defect: Select returned an unknown COMMIT
// outcome unchanged, so the route answered 500 internal_error and pm schema
// select exited 6. Select passes the end of its transaction through
// selectionOutcome. The check covers that mapping only: it does not drive a
// real lost COMMIT reply through Select. The store checks show that
// WithinScope returns ErrCommitOutcomeUnknown on a lost reply.
func TestSelectionCommitOutcomeUnknownIsReconcileOnly(t *testing.T) {
	unknown := errors.Join(store.ErrCommitOutcomeUnknown, errors.New("commit the schema selection"))
	mapped := selectionOutcome(unknown)
	var failure *Failure
	if !errors.As(mapped, &failure) || failure.HTTPStatus != 503 || failure.Code != "mutation_outcome_unknown" ||
		failure.RetryAction != "same_key_reconcile_only" {
		t.Fatalf("selection with an unknown COMMIT outcome = %#v; want 503 mutation_outcome_unknown with same_key_reconcile_only", mapped)
	}
	if !errors.Is(mapped, store.ErrCommitOutcomeUnknown) {
		t.Fatalf("selection with an unknown COMMIT outcome = %v; want ErrCommitOutcomeUnknown kept as the cause", mapped)
	}
	known := errors.New("a known refusal")
	if got := selectionOutcome(known); got != known {
		t.Fatalf("selection with a known error = %v; want the error unchanged", got)
	}
	if got := selectionOutcome(nil); got != nil {
		t.Fatalf("selection with no error = %v; want nil", got)
	}
}
