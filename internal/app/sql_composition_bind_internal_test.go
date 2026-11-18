package app

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/connectors"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/schema"
)

// Check (285c-2 review): admission pins exactly one schema reference for each
// PostgreSQL operation of the generated postgres bundle. pm-worker uses a
// snapshot only when the run spec pins exactly one reference
// (loadWorkerSchemaSnapshot). AcceptRun adds the selected reference to the
// references that the operation resolver gives (mergeSchemaRefs). This check
// does the same steps with the production resolver. Defect it catches: the
// resolver gives a PostgreSQL operation a schema reference of its own, so
// admission pins two references and pm-worker refuses every accepted SQL run.
func TestAdmissionPinsOneSchemaReferenceForEachPostgreSQLOperation(t *testing.T) {
	document, err := os.ReadFile(filepath.Join("..", "connectors", "data", "postgres.bundle.json"))
	if err != nil {
		t.Fatalf("read the generated postgres bundle: %v", err)
	}
	bundle, err := connectors.LoadBundle(bytes.NewReader(document))
	if err != nil {
		t.Fatalf("load the generated postgres bundle: %v", err)
	}
	registry, err := connectors.NewRegistry(bundle)
	if err != nil {
		t.Fatalf("build the connector registry: %v", err)
	}
	pinned := "sha256:" + strings.Repeat("a", 64)
	checked := 0
	for _, operation := range bundle.Operations {
		if operation.Transport.Kind != contract.TransportKindPostgreSQL {
			continue
		}
		selection, err := ResolveBundledOperation(registry, bundle.ConnectorID, operation.OperationID)
		if err != nil {
			t.Fatalf("resolve %q: %v", operation.OperationID, err)
		}
		selection, err = normalizeOperationSelection(selection)
		if err != nil {
			t.Fatalf("normalize %q: %v", operation.OperationID, err)
		}
		if got := mergeSchemaRefs(slices.Clone(selection.SchemaRefs), []string{pinned}); !slices.Equal(got, []string{pinned}) {
			t.Errorf("operation %q: admission pins %q; want exactly [%q]", operation.OperationID, got, pinned)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("the generated postgres bundle declares no PostgreSQL operation")
	}
}

// Check (plan 285c, check 13): the SQL callback binds a run only with the
// schema snapshot that admission pinned for it. A run that pins no snapshot,
// a snapshot of another binding, or an incomplete snapshot is refused before
// any send. Defect it catches: pm-worker reads with no selection, or with the
// snapshot of another scope, account, permission revision, resource or source
// revision (ADR 0007, line 87: the worker checks it again).
func TestBindRefusesARunWithoutAUsablePinnedSnapshot(t *testing.T) {
	scope := contract.Scope{OrgID: "org-bind", WorkspaceID: "workspace-bind"}
	const connectionID = "connection-bind"
	const connectorID = "postgres"
	relation := config.ProviderSQLRelationBinding{
		Binding:    "postgres.reusable-relation",
		Schema:     "customer",
		Table:      "records",
		KeyColumns: []string{"record_id"},
		Snapshot: config.ProviderSQLSnapshotBinding{
			ProviderAccountID:  "account-bind",
			PermissionRevision: "permissions-v1",
			Resource:           "customer.records",
			SourceRevision:     "source-v1",
		},
	}
	operation := contract.Operation{
		OperationID: "relation/select",
		Transport: contract.Transport{
			Kind:            contract.TransportKindPostgreSQL,
			StatementKind:   contract.PostgreSQLStatementSelect,
			RelationBinding: relation.Binding,
		},
	}
	composition := &SQLComposition{
		declared: true,
		connections: map[sqlConnectionKey]sqlBoundConnection{
			{orgID: scope.OrgID, workspaceID: scope.WorkspaceID, connectionID: connectionID}: {
				declaration: config.ProviderSQLConnection{
					ConnectorID:  connectorID,
					ConnectionID: connectionID,
					Bindings:     []config.ProviderSQLRelationBinding{relation},
				},
				bindings: map[string]config.ProviderSQLRelationBinding{relation.Binding: relation},
			},
		},
		operations: map[sqlDeclaredOperation]struct{}{
			{connectorID: connectorID, operationID: operation.OperationID}: {},
		},
	}
	declared := schema.Binding{
		OrgID:              scope.OrgID,
		WorkspaceID:        scope.WorkspaceID,
		ConnectorID:        connectorID,
		ConnectionID:       connectionID,
		ProviderAccountID:  relation.Snapshot.ProviderAccountID,
		PermissionRevision: relation.Snapshot.PermissionRevision,
		Resource:           relation.Snapshot.Resource,
		SourceRevision:     relation.Snapshot.SourceRevision,
	}
	snapshotOf := func(binding schema.Binding, completeness schema.Completeness) *schema.Snapshot {
		t.Helper()
		snapshot, err := schema.Candidate{
			Binding: binding,
			Fields: []schema.Field{
				{Name: "record_id", Pointer: "/columns/record_id", Schema: contract.Schema{Type: contract.SchemaTypeInteger},
					Required: true, Writable: true},
				{Name: "note", Pointer: "/columns/note", Schema: contract.Schema{Type: contract.SchemaTypeString},
					Required: true, Writable: true},
			},
			Completeness: completeness,
		}.Snapshot()
		if err != nil {
			t.Fatalf("build the pinned snapshot: %v", err)
		}
		return &snapshot
	}
	complete := schema.Completeness{Complete: true, Gaps: []schema.Gap{}}
	truncated, err := schema.NewGap(schema.GapTruncatedMetadata, "customer.records", "metadata page not read")
	if err != nil {
		t.Fatalf("declare a blocking gap: %v", err)
	}

	bound, err := composition.bind(scope, connectionID, operation, snapshotOf(declared, complete))
	if err != nil {
		t.Fatalf("bind with the pinned snapshot of the declared binding: %v", err)
	}
	if bound.relation.Schema != "customer" || bound.relation.Name != "records" || len(bound.relation.Columns) != 2 {
		t.Fatalf("bound relation = %#v; want customer.records with the two pinned columns", bound.relation)
	}

	otherAccount := declared
	otherAccount.ProviderAccountID = "account-other"
	otherPermission := declared
	otherPermission.PermissionRevision = "permissions-v0"
	otherSource := declared
	otherSource.SourceRevision = "source-v0"
	otherWorkspace := declared
	otherWorkspace.WorkspaceID = "workspace-other"
	for _, refusal := range []struct {
		name     string
		snapshot *schema.Snapshot
	}{
		{"no pinned snapshot", nil},
		{"another provider account", snapshotOf(otherAccount, complete)},
		{"another permission revision", snapshotOf(otherPermission, complete)},
		{"another source revision", snapshotOf(otherSource, complete)},
		{"another workspace", snapshotOf(otherWorkspace, complete)},
		{"an incomplete snapshot", snapshotOf(declared, schema.Completeness{Complete: false, Gaps: []schema.Gap{truncated}})},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			if _, err := composition.bind(scope, connectionID, operation, refusal.snapshot); !errors.Is(err, ErrSQLBindingUnavailable) {
				t.Fatalf("bind = %v; want ErrSQLBindingUnavailable before any send", err)
			}
		})
	}
}
