package runtime

import (
	"errors"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// TestMetadataStatementNeedsOnlyTheRelationIdentity covers the first discovery
// of a relation: no selected snapshot exists, so the relation names no column.
// The metadata statement binds only the declared schema and table as values.
// Defect it catches: a first discovery cannot build its metadata statement.
func TestMetadataStatementNeedsOnlyTheRelationIdentity(t *testing.T) {
	const binding = "postgres.reusable-relation"
	identity := SQLRelation{Binding: binding, Schema: "customer", Name: "records"}
	metadata := sqlTestOperation(t, contract.PostgreSQLStatementMetadata, binding)

	statement, err := BuildSQLStatement(metadata, identity, nil, 2)
	if err != nil {
		t.Fatalf("build the metadata statement of a relation with no declared columns: %v", err)
	}
	if statement.Kind != contract.PostgreSQLStatementMetadata || len(statement.Parameters) != 2 {
		t.Fatalf("metadata statement = %#v; want the two bound relation parameters", statement)
	}
	for index, want := range []string{"customer", "records"} {
		got, ok := statement.Parameters[index].Value.Text()
		if !ok || got != want {
			t.Fatalf("parameter %d = %q; want the bound value %q, never SQL text", index, got, want)
		}
	}

	if _, err := BuildSQLStatement(
		sqlTestOperation(t, contract.PostgreSQLStatementMetadata, "postgres.other-relation"), identity, nil, 2,
	); !errors.Is(err, ErrSQLRelation) {
		t.Fatalf("metadata statement of another relation binding = %v; want a relation refusal", err)
	}
	if _, err := BuildSQLStatement(
		metadata, identity, []SQLColumnValue{{Column: "note", Value: SQLTextValue("x")}}, 2,
	); !errors.Is(err, ErrSQLStatement) {
		t.Fatalf("metadata statement with mapped input = %v; want a statement refusal", err)
	}
	if _, err := BuildSQLStatement(
		metadata, SQLRelation{Binding: binding, Schema: "customer", Name: ""}, nil, 2,
	); !errors.Is(err, ErrSQLRelation) {
		t.Fatalf("metadata statement with no table = %v; want a relation refusal", err)
	}
	// Every other statement still needs the declared column list.
	if _, err := BuildSQLStatement(
		sqlTestOperation(t, contract.PostgreSQLStatementSelect, binding), identity, nil, 2,
	); !errors.Is(err, ErrSQLRelation) {
		t.Fatalf("select of a relation with no declared columns = %v; want a relation refusal", err)
	}
}
