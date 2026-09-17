package compile

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type BindingExpectation struct {
	OperationID       string
	FieldID           string
	SourceFacts       []SourceFact
	Required          bool
	Nullable          bool
	Style             string
	Explode           bool
	AllowReserved     bool
	NormalizedPointer string
	AffectedCells     []contract.Lane
}

func CheckBindingFidelity(expectation BindingExpectation, binding contract.FieldBinding) *Diagnostic {
	if expectation.Required == binding.Required &&
		expectation.Nullable == binding.Nullable &&
		expectation.Style == binding.Style &&
		expectation.Explode == binding.Explode &&
		expectation.AllowReserved == binding.AllowReserved {
		return nil
	}
	diagnostic := newDiagnostic(
		"source_fidelity_disagreement",
		SeverityError,
		expectation.OperationID,
		expectation.FieldID,
		expectation.SourceFacts,
		expectation.NormalizedPointer,
		binding,
		expectation.AffectedCells,
		"raw required, nullable or serialization facts disagree with the normalized field; the affected binding is blocked",
	)
	return &diagnostic
}

func sourceFacts(source *ResolvedSource, references []contract.SourceRef) []SourceFact {
	facts := make([]SourceFact, 0, len(references))
	for _, reference := range references {
		hash, _ := source.PointerHash(reference.SourceID, reference.Pointer)
		facts = append(facts, SourceFact{SourceRef: reference, SHA256: hash})
	}
	slices.SortFunc(facts, func(left, right SourceFact) int {
		if compared := strings.Compare(left.SourceRef.SourceID, right.SourceRef.SourceID); compared != 0 {
			return compared
		}
		return strings.Compare(left.SourceRef.Pointer, right.SourceRef.Pointer)
	})
	return facts
}

func newDiagnostic(
	code string,
	severity DiagnosticSeverity,
	operationID string,
	fieldID string,
	facts []SourceFact,
	normalizedPointer string,
	normalized any,
	affected []contract.Lane,
	detail string,
) Diagnostic {
	diagnostic := Diagnostic{
		Code:              code,
		Severity:          severity,
		OperationID:       operationID,
		FieldID:           fieldID,
		SourceFacts:       slices.Clone(facts),
		NormalizedPointer: normalizedPointer,
		AffectedCells:     slices.Clone(affected),
		Detail:            detail,
	}
	if normalized != nil {
		encoded, err := json.Marshal(normalized)
		if err == nil {
			diagnostic.NormalizedSHA256 = "sha256:" + digestHex(encoded)
		}
	}
	identity, err := json.Marshal(struct {
		Code              string          `json:"code"`
		OperationID       string          `json:"operation_id"`
		FieldID           string          `json:"field_id"`
		Facts             []SourceFact    `json:"facts"`
		NormalizedPointer string          `json:"normalized_pointer"`
		Affected          []contract.Lane `json:"affected"`
	}{
		Code:              code,
		OperationID:       operationID,
		FieldID:           fieldID,
		Facts:             diagnostic.SourceFacts,
		NormalizedPointer: normalizedPointer,
		Affected:          diagnostic.AffectedCells,
	})
	if err != nil {
		panic(err)
	}
	diagnostic.ID = "diag-" + digestHex(identity)[:24]
	return diagnostic
}

func allLaneSlice() []contract.Lane {
	lanes := make([]contract.Lane, len(allLanes))
	copy(lanes, allLanes[:])
	return lanes
}
