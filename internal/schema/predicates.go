package schema

import (
	"slices"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// RequirementKind distinguishes a declared metadata capability from the exact
// installed behavioral revision of a runtime contract.
type RequirementKind string

const (
	RequirementMetadataCapability RequirementKind = "metadata_capability"
	RequirementRuntimeContract    RequirementKind = "runtime_contract"
)

func (kind RequirementKind) Valid() bool {
	return kind == RequirementMetadataCapability || kind == RequirementRuntimeContract
}

// Requirement is one typed requirement a cell declares. A metadata capability
// has an identity, a runtime contract has an identity and an exact revision.
type Requirement struct {
	ID      string          `json:"id"`
	Kind    RequirementKind `json:"kind"`
	Version string          `json:"version,omitempty"`
}

func (requirement Requirement) Validate() error {
	if !requirement.Kind.Valid() {
		return errorf(ErrInvalidPredicate, "requirement.kind", "is not a declared requirement kind")
	}
	if !validBoundedIdentity(requirement.ID) {
		return errorf(ErrInvalidPredicate, "requirement.id", "must be a canonical bounded identity")
	}
	switch requirement.Kind {
	case RequirementMetadataCapability:
		if requirement.Version != "" {
			return errorf(ErrInvalidPredicate, "requirement.version", "a declared metadata capability has no behavioral revision")
		}
	case RequirementRuntimeContract:
		reference := contract.FoundationRef{ID: requirement.ID, Version: requirement.Version}
		if err := reference.Validate(); err != nil {
			return errorf(ErrInvalidPredicate, "requirement", "must name an installed foundation revision")
		}
	}
	return nil
}

// Identity is the requirement's stable subject in a reported gap.
func (requirement Requirement) Identity() string {
	if requirement.Kind == RequirementRuntimeContract {
		return string(requirement.Kind) + ":" + requirement.ID + "@" + requirement.Version
	}
	return string(requirement.Kind) + ":" + requirement.ID
}

// ExpressionOperator is a declared predicate operator. An undeclared operator
// is invalid and is never evaluated.
type ExpressionOperator string

const (
	OperatorPresent  ExpressionOperator = "present"
	OperatorWritable ExpressionOperator = "writable"
	OperatorEquals   ExpressionOperator = "equals"
	OperatorIn       ExpressionOperator = "in"
)

func (operator ExpressionOperator) Valid() bool {
	switch operator {
	case OperatorPresent, OperatorWritable, OperatorEquals, OperatorIn:
		return true
	default:
		return false
	}
}

// Expression is one declared predicate over the typed fields of the effective
// schema.
type Expression struct {
	Operator ExpressionOperator     `json:"operator"`
	Field    string                 `json:"field"`
	Value    *contract.PresentValue `json:"value,omitempty"`
}

func (expression Expression) validate(fields map[string]Field) error {
	if !expression.Operator.Valid() {
		return errorf(ErrInvalidPredicate, "expression.operator", "is not a declared predicate operator")
	}
	field, declared := fields[expression.Field]
	if !declared {
		return errorf(ErrInvalidPredicate, "expression.field", "does not name a field of the effective schema")
	}
	switch expression.Operator {
	case OperatorPresent, OperatorWritable:
		if expression.Value != nil {
			return errorf(ErrInvalidPredicate, "expression.value", "is not declared by this operator")
		}
		return nil
	case OperatorEquals:
		if expression.Value == nil {
			return errorf(ErrInvalidPredicate, "expression.value", "is required by this operator")
		}
		if err := contract.ValidateValueAgainstSchema(*expression.Value, field.Schema); err != nil {
			return errorf(ErrInvalidPredicate, "expression.value", "does not satisfy the declared field schema")
		}
		return nil
	default:
		if expression.Value == nil {
			return errorf(ErrInvalidPredicate, "expression.value", "is required by this operator")
		}
		elements, ok := expression.Value.Elements()
		if !ok || len(elements) == 0 {
			return errorf(ErrInvalidPredicate, "expression.value", "must declare a nonempty array of candidate values")
		}
		for _, element := range elements {
			if err := contract.ValidateValueAgainstSchema(element, field.Schema); err != nil {
				return errorf(ErrInvalidPredicate, "expression.value", "contains a value that does not satisfy the declared field schema")
			}
		}
		return nil
	}
}

// Overlay is one source-linked predicate a cell declares in addition to its
// typed requirements.
type Overlay struct {
	OverlayID  string     `json:"overlay_id"`
	Expression Expression `json:"expression"`
}

// Cell is one declared operation or field surface whose compatibility is
// decided independently. An unsupported cell never suppresses another cell.
type Cell struct {
	CellID       string        `json:"cell_id"`
	Requirements []Requirement `json:"requirements"`
	Overlays     []Overlay     `json:"overlays"`
}

// Target declares what the connection can actually do: its declared metadata
// capabilities, its installed runtime contracts and the typed fields of the
// effective schema.
type Target struct {
	MetadataCapabilities []string                      `json:"metadata_capabilities"`
	RuntimeContracts     []contract.FoundationContract `json:"runtime_contracts"`
	Fields               []Field                       `json:"fields"`
}

// CellPredicate is the compatibility decision of one cell. Gaps references
// reported gap identities, so one shared gap is referenced rather than copied.
type CellPredicate struct {
	CellID     string   `json:"cell_id"`
	Compatible bool     `json:"compatible"`
	Gaps       []string `json:"gaps"`
	Evaluated  []string `json:"evaluated"`
}

// PredicateReport is one complete compatibility decision.
type PredicateReport struct {
	Gaps  []Gap           `json:"gaps"`
	Cells []CellPredicate `json:"cells"`
}

func (report PredicateReport) Compatible() bool {
	for _, cell := range report.Cells {
		if !cell.Compatible {
			return false
		}
	}
	return true
}

// ExpressionEvaluator evaluates one declared, validated predicate expression.
// An invalid overlay never reaches it.
type ExpressionEvaluator func(Expression) (bool, error)

// CheckPredicates decides every cell against the declared target. Unknown
// metadata capabilities and unavailable runtime contracts block only their
// cells, and one shared gap is reported once.
func CheckPredicates(target Target, cells []Cell, evaluate ExpressionEvaluator) (PredicateReport, error) {
	if target.MetadataCapabilities == nil {
		return PredicateReport{}, errorf(ErrInvalidPredicate, "metadata_capabilities", "must be declared explicitly, even when empty")
	}
	if target.RuntimeContracts == nil {
		return PredicateReport{}, errorf(ErrInvalidPredicate, "runtime_contracts", "must be declared explicitly, even when empty")
	}
	if target.Fields == nil {
		return PredicateReport{}, errorf(ErrInvalidPredicate, "fields", "must be declared explicitly, even when empty")
	}
	capabilities := make(map[string]struct{}, len(target.MetadataCapabilities))
	for _, capability := range target.MetadataCapabilities {
		if !validBoundedIdentity(capability) {
			return PredicateReport{}, errorf(ErrInvalidPredicate, "metadata_capabilities", "must name a declared metadata capability")
		}
		if _, duplicate := capabilities[capability]; duplicate {
			return PredicateReport{}, errorf(ErrInvalidPredicate, "metadata_capabilities", "declares the same capability more than once")
		}
		capabilities[capability] = struct{}{}
	}
	fields := make(map[string]Field, len(target.Fields))
	for _, field := range target.Fields {
		if err := field.Validate(); err != nil {
			return PredicateReport{}, err
		}
		if _, duplicate := fields[field.Name]; duplicate {
			return PredicateReport{}, errorf(ErrInvalidPredicate, "fields", "declares the same field more than once")
		}
		fields[field.Name] = field
	}
	seenCells := make(map[string]struct{}, len(cells))
	for _, cell := range cells {
		if !validBoundedIdentity(cell.CellID) {
			return PredicateReport{}, errorf(ErrInvalidPredicate, "cell_id", "must be a canonical bounded identity")
		}
		if _, duplicate := seenCells[cell.CellID]; duplicate {
			return PredicateReport{}, errorf(ErrInvalidPredicate, "cell_id", "declares the same cell more than once")
		}
		seenCells[cell.CellID] = struct{}{}
		if len(cell.Requirements) == 0 && len(cell.Overlays) == 0 {
			return PredicateReport{}, errorf(ErrInvalidPredicate, "cell."+cell.CellID, "declares neither a requirement nor an overlay")
		}
		seenRequirements := make(map[string]struct{}, len(cell.Requirements))
		for _, requirement := range cell.Requirements {
			if err := requirement.Validate(); err != nil {
				return PredicateReport{}, err
			}
			if _, duplicate := seenRequirements[requirement.Identity()]; duplicate {
				return PredicateReport{}, errorf(ErrInvalidPredicate, "cell."+cell.CellID, "declares the same requirement more than once")
			}
			seenRequirements[requirement.Identity()] = struct{}{}
		}
		seenOverlays := make(map[string]struct{}, len(cell.Overlays))
		for _, overlay := range cell.Overlays {
			if !validBoundedIdentity(overlay.OverlayID) {
				return PredicateReport{}, errorf(ErrInvalidPredicate, "cell."+cell.CellID+".overlay_id", "must be a canonical bounded identity")
			}
			if _, duplicate := seenOverlays[overlay.OverlayID]; duplicate {
				return PredicateReport{}, errorf(ErrInvalidPredicate, "cell."+cell.CellID, "declares the same overlay more than once")
			}
			seenOverlays[overlay.OverlayID] = struct{}{}
		}
	}
	if evaluate == nil {
		for _, cell := range cells {
			if len(cell.Overlays) > 0 {
				return PredicateReport{}, errorf(ErrEvaluatorUnavailable, "cells."+cell.CellID, "declares overlays that require an expression evaluator")
			}
		}
	}

	gaps := make([]Gap, 0, len(cells))
	reports := make([]CellPredicate, 0, len(cells))
	for _, cell := range cells {
		report := CellPredicate{CellID: cell.CellID, Compatible: true, Gaps: []string{}, Evaluated: []string{}}
		for _, requirement := range cell.Requirements {
			detail := ""
			switch requirement.Kind {
			case RequirementMetadataCapability:
				if _, available := capabilities[requirement.ID]; !available {
					detail = "the declared metadata capability is not available for this connection"
				}
			case RequirementRuntimeContract:
				reference := contract.FoundationRef{ID: requirement.ID, Version: requirement.Version}
				if err := contract.CheckFoundationCompatibility([]contract.FoundationRef{reference}, target.RuntimeContracts); err != nil {
					detail = "the required behavioral revision is not installed"
				}
			}
			if detail == "" {
				continue
			}
			gap, err := NewGap(GapRequirementUnavailable, requirement.Identity(), detail)
			if err != nil {
				return PredicateReport{}, err
			}
			gaps = append(gaps, gap)
			report.Gaps = append(report.Gaps, gap.ID)
			report.Compatible = false
		}
		for _, overlay := range cell.Overlays {
			subject := cell.CellID + "/" + overlay.OverlayID
			if err := overlay.Expression.validate(fields); err != nil {
				// An invalid overlay is recorded and never evaluated.
				gap, gapErr := NewGap(GapInvalidOverlay, subject, "the declared predicate is not a typed expression")
				if gapErr != nil {
					return PredicateReport{}, gapErr
				}
				gaps = append(gaps, gap)
				report.Gaps = append(report.Gaps, gap.ID)
				report.Compatible = false
				continue
			}
			satisfied, err := evaluate(overlay.Expression)
			if err != nil {
				return PredicateReport{}, err
			}
			report.Evaluated = append(report.Evaluated, overlay.OverlayID)
			if satisfied {
				continue
			}
			gap, err := NewGap(GapPredicateUnsatisfied, subject, "the declared predicate is not satisfied by the selected metadata")
			if err != nil {
				return PredicateReport{}, err
			}
			gaps = append(gaps, gap)
			report.Gaps = append(report.Gaps, gap.ID)
			report.Compatible = false
		}
		reports = append(reports, report)
	}
	// One shared gap is reported once, and every cell references it by identity.
	deduplicated, err := deduplicateGaps(gaps)
	if err != nil {
		return PredicateReport{}, err
	}
	for index := range reports {
		slices.Sort(reports[index].Gaps)
	}
	return PredicateReport{Gaps: deduplicated, Cells: reports}, nil
}
