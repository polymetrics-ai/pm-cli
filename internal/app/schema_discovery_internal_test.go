package app

import (
	"errors"
	"testing"
	"time"

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
