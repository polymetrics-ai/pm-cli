package schema

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestTypedRequirementsAndAtlasGaps(t *testing.T) {
	binding := testBinding(testAccountID)
	snapshot := snapshotFor(t, testCandidate(t, binding, testFields()))
	target := Target{
		MetadataCapabilities: []string{"hubspot.property-metadata", "hubspot.custom-object-metadata"},
		RuntimeContracts: []contract.FoundationContract{
			{ID: "FND-09", Version: "1.1.0", Dependencies: []contract.FoundationRef{}},
		},
		Fields: effectiveFieldsOf(effectiveFor(t, snapshot, StaticSchema{
			OperationID: "contacts/create",
			Fields: []Field{
				{Name: "email", Pointer: "/request/email", Schema: contract.Schema{Type: contract.SchemaTypeString}, Required: true, Writable: true},
			},
		})),
	}

	t.Run("V25_unavailable_requirements_block_once_while_unrelated_cells_continue", func(t *testing.T) {
		report, err := CheckPredicates(target, []Cell{
			{CellID: "contacts.create", Requirements: []Requirement{
				{ID: "hubspot.deal-metadata", Kind: RequirementMetadataCapability},
				{ID: "FND-09", Kind: RequirementRuntimeContract, Version: "1.1.0"},
			}},
			{CellID: "contacts.update", Requirements: []Requirement{
				{ID: "hubspot.deal-metadata", Kind: RequirementMetadataCapability},
				{ID: "FND-99", Kind: RequirementRuntimeContract, Version: "9.9.9"},
			}},
			{CellID: "contacts.read", Requirements: []Requirement{
				{ID: "hubspot.property-metadata", Kind: RequirementMetadataCapability},
				{ID: "FND-09", Kind: RequirementRuntimeContract, Version: "1.1.0"},
			}},
		}, nil)
		if err != nil {
			t.Fatalf("CheckPredicates: %v", err)
		}
		if report.Compatible() {
			t.Fatalf("report %#v claimed compatibility with an unavailable requirement", report)
		}
		if len(report.Gaps) != 2 {
			t.Fatalf("gaps = %#v; want one metadata gap and one runtime gap", report.Gaps)
		}
		metadataGap := gapFor(t, report, GapRequirementUnavailable, "metadata_capability:hubspot.deal-metadata")
		runtimeGap := gapFor(t, report, GapRequirementUnavailable, "runtime_contract:FND-99@9.9.9")
		if metadataGap.Detail == "" || runtimeGap.Detail == "" {
			t.Fatalf("gaps %#v carry no reason", report.Gaps)
		}
		if !slices.Equal(cellGaps(t, report, "contacts.create"), []string{metadataGap.ID}) {
			t.Fatalf("contacts.create gaps = %v; want the shared metadata gap once", cellGaps(t, report, "contacts.create"))
		}
		if !slices.Equal(cellGaps(t, report, "contacts.update"), []string{metadataGap.ID, runtimeGap.ID}) {
			t.Fatalf("contacts.update gaps = %v; want the shared metadata gap and its runtime gap", cellGaps(t, report, "contacts.update"))
		}
		unrelated := cellReport(t, report, "contacts.read")
		if !unrelated.Compatible || len(unrelated.Gaps) != 0 {
			t.Fatalf("unrelated cell = %#v; want it to continue unchanged", unrelated)
		}
	})

	t.Run("V25_invalid_overlays_never_execute_expressions", func(t *testing.T) {
		var evaluated []Expression
		evaluate := func(expression Expression) (bool, error) {
			evaluated = append(evaluated, expression)
			return expression.Operator != OperatorEquals, nil
		}
		report, err := CheckPredicates(target, []Cell{
			{CellID: "contacts.create", Requirements: []Requirement{
				{ID: "hubspot.property-metadata", Kind: RequirementMetadataCapability},
			}, Overlays: []Overlay{
				{OverlayID: "overlay-undeclared-operator", Expression: Expression{Operator: "matches", Field: "stage"}},
				{OverlayID: "overlay-undeclared-field", Expression: Expression{Operator: OperatorPresent, Field: "not_declared"}},
				{OverlayID: "overlay-invalid-value", Expression: Expression{Operator: OperatorIn, Field: "stage", Value: pointerTo(contract.NewStringValue("open"))}},
				{OverlayID: "overlay-unsatisfied", Expression: Expression{Operator: OperatorEquals, Field: "stage", Value: pointerTo(contract.NewStringValue("open"))}},
			}},
			{CellID: "contacts.update", Requirements: []Requirement{
				{ID: "hubspot.property-metadata", Kind: RequirementMetadataCapability},
			}, Overlays: []Overlay{
				{OverlayID: "overlay-satisfied", Expression: Expression{Operator: OperatorPresent, Field: "stage"}},
			}},
		}, evaluate)
		if err != nil {
			t.Fatalf("CheckPredicates: %v", err)
		}
		if len(evaluated) != 2 {
			t.Fatalf("evaluated expressions = %#v; want exactly the two valid overlays", evaluated)
		}
		for _, expression := range evaluated {
			if expression.Field == "not_declared" || !expression.Operator.Valid() {
				t.Fatalf("invalid overlay reached the evaluator: %#v", expression)
			}
		}
		if evaluated[0].Operator != OperatorEquals || evaluated[1].Operator != OperatorPresent {
			t.Fatalf("evaluated order = %#v; want declared overlay order", evaluated)
		}
		if len(report.Gaps) != 4 {
			t.Fatalf("gaps = %#v; want three invalid overlays and one unsatisfied predicate", report.Gaps)
		}
		for _, overlay := range []string{"overlay-undeclared-operator", "overlay-undeclared-field", "overlay-invalid-value"} {
			gap := gapFor(t, report, GapInvalidOverlay, "contacts.create/"+overlay)
			if gap.Detail == "" {
				t.Fatalf("invalid overlay %q gap carries no reason", overlay)
			}
		}
		gapFor(t, report, GapPredicateUnsatisfied, "contacts.create/overlay-unsatisfied")
		blocked := cellReport(t, report, "contacts.create")
		if blocked.Compatible || !slices.Contains(blocked.Gaps, gapFor(t, report, GapInvalidOverlay, "contacts.create/overlay-invalid-value").ID) {
			t.Fatalf("contacts.create = %#v; want an explicit blocked cell", blocked)
		}
		continued := cellReport(t, report, "contacts.update")
		if !continued.Compatible || !slices.Equal(continued.Evaluated, []string{"overlay-satisfied"}) {
			t.Fatalf("contacts.update = %#v; want the unrelated cell evaluated once", continued)
		}
	})

	t.Run("V25_malformed_declarations_and_a_missing_evaluator_are_refused_explicitly", func(t *testing.T) {
		overlay := []Cell{{CellID: "contacts.create", Overlays: []Overlay{
			{OverlayID: "overlay-present", Expression: Expression{Operator: OperatorPresent, Field: "stage"}},
		}}}
		if _, err := CheckPredicates(target, overlay, nil); !errors.Is(err, ErrEvaluatorUnavailable) {
			t.Fatalf("missing evaluator = %v; want %v", err, ErrEvaluatorUnavailable)
		}
		for _, test := range []struct {
			name  string
			cells []Cell
		}{
			{name: "runtime_contract_without_a_foundation_identity", cells: []Cell{{
				CellID:       "cell",
				Requirements: []Requirement{{ID: "runtime-contract", Kind: RequirementRuntimeContract, Version: "1.0.0"}},
			}}},
			{name: "metadata_capability_with_a_revision", cells: []Cell{{
				CellID:       "cell",
				Requirements: []Requirement{{ID: "hubspot.property-metadata", Kind: RequirementMetadataCapability, Version: "1.0.0"}},
			}}},
			{name: "duplicate_cell_identity", cells: []Cell{
				{CellID: "cell", Requirements: []Requirement{{ID: "hubspot.property-metadata", Kind: RequirementMetadataCapability}}},
				{CellID: "cell", Requirements: []Requirement{{ID: "hubspot.property-metadata", Kind: RequirementMetadataCapability}}},
			}},
			{name: "duplicate_requirement", cells: []Cell{{CellID: "cell", Requirements: []Requirement{
				{ID: "hubspot.property-metadata", Kind: RequirementMetadataCapability},
				{ID: "hubspot.property-metadata", Kind: RequirementMetadataCapability},
			}}}},
		} {
			if _, err := CheckPredicates(target, test.cells, nil); !errors.Is(err, ErrInvalidPredicate) {
				t.Fatalf("%s = %v; want %v", test.name, err, ErrInvalidPredicate)
			}
		}
	})

	t.Run("V25_typed_dynamic_flags_come_from_the_selected_snapshot_only", func(t *testing.T) {
		effective := effectiveFor(t, snapshot, StaticSchema{
			OperationID: "contacts/create",
			Fields: []Field{
				{Name: "email", Pointer: "/request/email", Schema: contract.Schema{Type: contract.SchemaTypeString}, Required: true, Writable: true},
			},
		})
		if effective.SchemaRef != snapshot.Fingerprint || effective.Binding != binding {
			t.Fatalf("effective identity = %q %#v; want the selected snapshot binding", effective.SchemaRef, effective.Binding)
		}
		if !slices.Equal(effectiveFieldNames(effective), []string{"amount", "email", "id", "stage", "tier"}) {
			t.Fatalf("effective flags = %v; want the static and selected dynamic flags", effectiveFieldNames(effective))
		}
		if effectiveFieldNamed(t, effective, "email").Dynamic {
			t.Fatalf("static operation field was reported as a discovered account field")
		}
		if !effectiveFieldNamed(t, effective, "tier").Dynamic {
			t.Fatalf("selected snapshot field was not projected as a dynamic flag")
		}
		if effectiveFieldNamed(t, effective, "id").Writable {
			t.Fatalf("read-only metadata was projected as a writable flag")
		}

		// A different account's selected snapshot is the only source of its flags:
		// provider spelling is retained and the earlier projection is untouched.
		otherBinding := testBinding("account-other")
		other := snapshotFor(t, testCandidate(t, otherBinding, []Field{
			{Name: "Custom_Tier", Pointer: "/properties/Custom_Tier", Schema: declaredStringEnumSchema("gold"), Writable: true},
		}))
		otherEffective := effectiveFor(t, other, StaticSchema{
			OperationID: "contacts/create",
			Fields: []Field{
				{Name: "email", Pointer: "/request/email", Schema: contract.Schema{Type: contract.SchemaTypeString}, Required: true, Writable: true},
			},
		})
		if !slices.Equal(effectiveFieldNames(otherEffective), []string{"Custom_Tier", "email"}) {
			t.Fatalf("other effective flags = %v; want only the selected snapshot's declaration", effectiveFieldNames(otherEffective))
		}
		if !slices.Equal(effectiveFieldNames(effective), []string{"amount", "email", "id", "stage", "tier"}) {
			t.Fatalf("selecting another snapshot changed the earlier projection: %v", effectiveFieldNames(effective))
		}

		// A conflicting declaration is explicit and never silently remapped.
		conflict := effectiveFor(t, snapshot, StaticSchema{
			OperationID: "contacts/create",
			Fields: []Field{
				{Name: "stage", Pointer: "/request/stage", Schema: contract.Schema{Type: contract.SchemaTypeDecimal}, Writable: true},
			},
		})
		blocked := effectiveFieldNamed(t, conflict, "stage")
		if !blocked.Blocked || blocked.Writable {
			t.Fatalf("conflicting field = %#v; want an explicit blocked, non-writable field", blocked)
		}
		if blocked.Schema.Type != contract.SchemaTypeDecimal {
			t.Fatalf("conflicting field schema = %#v; want the declared request type retained", blocked.Schema)
		}
		conflictGap := gapFor(t, PredicateReport{Gaps: conflict.Gaps}, GapConflictingField, "stage")
		if conflictGap.Detail == "" {
			t.Fatalf("conflicting field gap carries no reason")
		}
		if !slices.Contains(effectiveFieldNames(conflict), "tier") {
			t.Fatalf("a conflicting field suppressed unrelated flags: %v", effectiveFieldNames(conflict))
		}
	})

	t.Run("V25_gap_identity_is_stable_and_requires_a_subject", func(t *testing.T) {
		gap, err := NewGap(GapRequirementUnavailable, "runtime_contract:FND-99@9.9.9", "the required behavioral revision is not installed")
		if err != nil {
			t.Fatalf("NewGap: %v", err)
		}
		replay, err := NewGap(GapRequirementUnavailable, "runtime_contract:FND-99@9.9.9", "the required behavioral revision is not installed")
		if err != nil {
			t.Fatalf("NewGap replay: %v", err)
		}
		if gap.ID != replay.ID || strings.Contains(gap.ID, "\x00") {
			t.Fatalf("gap identity = %q / %q; want one stable identity", gap.ID, replay.ID)
		}
		if _, err := NewGap(GapRequirementUnavailable, "", "the declared predicate is not a typed expression"); !errors.Is(err, ErrInvalidGap) {
			t.Fatalf("gap without a subject = %v; want %v", err, ErrInvalidGap)
		}
	})
}

func effectiveFor(t *testing.T, snapshot Snapshot, static StaticSchema) EffectiveSchema {
	t.Helper()
	effective, err := BuildEffectiveSchema(static, snapshot)
	if err != nil {
		t.Fatalf("BuildEffectiveSchema: %v", err)
	}
	return effective
}

func snapshotFor(t *testing.T, candidate Candidate) Snapshot {
	t.Helper()
	snapshot, err := candidate.Snapshot()
	if err != nil {
		t.Fatalf("Candidate.Snapshot: %v", err)
	}
	return snapshot
}

func effectiveFieldNames(effective EffectiveSchema) []string {
	names := make([]string, 0, len(effective.Fields))
	for _, field := range effective.Fields {
		names = append(names, field.Name)
	}
	slices.Sort(names)
	return names
}

func effectiveFieldsOf(effective EffectiveSchema) []Field {
	fields := make([]Field, 0, len(effective.Fields))
	for _, field := range effective.Fields {
		fields = append(fields, field.Field)
	}
	return fields
}

func effectiveFieldNamed(t *testing.T, effective EffectiveSchema, name string) EffectiveField {
	t.Helper()
	for _, field := range effective.Fields {
		if field.Name == name {
			return field
		}
	}
	t.Fatalf("effective field %q is absent from %v", name, effectiveFieldNames(effective))
	return EffectiveField{}
}

func cellReport(t *testing.T, report PredicateReport, cellID string) CellPredicate {
	t.Helper()
	for _, cell := range report.Cells {
		if cell.CellID == cellID {
			return cell
		}
	}
	t.Fatalf("cell %q is absent from %#v", cellID, report.Cells)
	return CellPredicate{}
}

func cellGaps(t *testing.T, report PredicateReport, cellID string) []string {
	t.Helper()
	gaps := cellReport(t, report, cellID).Gaps
	sorted := slices.Clone(gaps)
	slices.Sort(sorted)
	return sorted
}

func gapFor(t *testing.T, report PredicateReport, kind GapKind, subject string) Gap {
	t.Helper()
	for _, gap := range report.Gaps {
		if gap.Kind == kind && gap.Subject == subject {
			return gap
		}
	}
	t.Fatalf("no %s gap for %q in %#v", kind, subject, report.Gaps)
	return Gap{}
}

func pointerTo(value contract.PresentValue) *contract.PresentValue {
	return &value
}
