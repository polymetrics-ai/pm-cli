package compile

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const SemanticsSchemaVersion = "pm.semantics/v1"

type LaneStatus string

const (
	LaneEligible      LaneStatus = "eligible"
	LaneUnsupported   LaneStatus = "unsupported"
	LaneNotApplicable LaneStatus = "not_applicable"
	LaneBlocked       LaneStatus = "blocked"
	LaneUnproven      LaneStatus = "unproven"
)

var allLanes = [...]contract.Lane{
	contract.LaneDirectRead,
	contract.LaneDirectWrite,
	contract.LaneETL,
	contract.LaneReverseETL,
	contract.LaneBinaryDownload,
	contract.LaneBinaryUpload,
	contract.LaneSyncTransport,
}

type SourceFact struct {
	SourceRef contract.SourceRef `json:"source_ref"`
	SHA256    string             `json:"sha256"`
}

type DiagnosticSeverity string

const (
	SeverityError   DiagnosticSeverity = "error"
	SeverityWarning DiagnosticSeverity = "warning"
)

type Diagnostic struct {
	ID                string             `json:"id"`
	Code              string             `json:"code"`
	Severity          DiagnosticSeverity `json:"severity"`
	OperationID       string             `json:"operation_id,omitempty"`
	FieldID           string             `json:"field_id,omitempty"`
	SourceFacts       []SourceFact       `json:"source_facts"`
	NormalizedPointer string             `json:"normalized_pointer,omitempty"`
	NormalizedSHA256  string             `json:"normalized_sha256,omitempty"`
	AffectedCells     []contract.Lane    `json:"affected_cells"`
	Detail            string             `json:"detail"`
}

type SensitivityClass string

const (
	SensitivityPublic  SensitivityClass = "public"
	SensitivitySecret  SensitivityClass = "secret"
	SensitivityUnknown SensitivityClass = "unknown"
)

type RawValueFact struct {
	SourceFact    SourceFact      `json:"source_fact"`
	CanonicalJSON json.RawMessage `json:"canonical_json"`
}

type SchemaFact struct {
	SourceFact    SourceFact         `json:"source_fact"`
	Types         []string           `json:"types"`
	Format        string             `json:"format,omitempty"`
	Nullable      bool               `json:"nullable"`
	ReadOnly      bool               `json:"read_only"`
	WriteOnly     bool               `json:"write_only"`
	Required      []string           `json:"required"`
	AllOfPointers []string           `json:"all_of_pointers"`
	OneOfPointers []string           `json:"one_of_pointers"`
	Default       *RawValueFact      `json:"default,omitempty"`
	Reference     *ReferenceIdentity `json:"reference,omitempty"`
}

type BindingFact struct {
	FieldID             string                 `json:"field_id"`
	SourceFacts         []SourceFact           `json:"source_facts"`
	RawRequired         bool                   `json:"raw_required"`
	RawNullable         bool                   `json:"raw_nullable"`
	RawStyle            string                 `json:"raw_style"`
	RawExplode          bool                   `json:"raw_explode"`
	RawAllowReserved    bool                   `json:"raw_allow_reserved"`
	Sensitivity         SensitivityClass       `json:"sensitivity"`
	SchemaFacts         []SchemaFact           `json:"schema_facts"`
	Normalized          *contract.FieldBinding `json:"normalized,omitempty"`
	NormalizedSHA256    string                 `json:"normalized_sha256,omitempty"`
	BlockedDiagnosticID string                 `json:"blocked_diagnostic_id,omitempty"`
}

type RequestVariant struct {
	VariantID            string           `json:"variant_id"`
	MediaType            string           `json:"media_type"`
	SourceFacts          []SourceFact     `json:"source_facts"`
	BodyRequired         bool             `json:"body_required"`
	Schema               *contract.Schema `json:"schema,omitempty"`
	SchemaFacts          []SchemaFact     `json:"schema_facts"`
	NormalizedSHA256     string           `json:"normalized_sha256,omitempty"`
	Bindings             []BindingFact    `json:"bindings"`
	BlockedDiagnosticIDs []string         `json:"blocked_diagnostic_ids"`
}

type ResponseVariant struct {
	Status               string           `json:"status"`
	MediaType            string           `json:"media_type"`
	NoContent            bool             `json:"no_content"`
	SourceFacts          []SourceFact     `json:"source_facts"`
	SchemaFacts          []SchemaFact     `json:"schema_facts"`
	Sensitivity          SensitivityClass `json:"sensitivity"`
	Schema               *contract.Schema `json:"schema,omitempty"`
	NormalizedSHA256     string           `json:"normalized_sha256,omitempty"`
	BlockedDiagnosticIDs []string         `json:"blocked_diagnostic_ids"`
}

type LaneDisposition struct {
	Lane       contract.Lane        `json:"lane"`
	Status     LaneStatus           `json:"status"`
	Reason     string               `json:"reason"`
	SourceRefs []contract.SourceRef `json:"source_refs"`
}

type RawOperationFact struct {
	SourceOperationID string       `json:"source_operation_id"`
	SourceFacts       []SourceFact `json:"source_facts"`
	Method            string       `json:"method"`
	Path              string       `json:"path"`
	RequestVariants   []VariantRef `json:"request_variants"`
	ResponseVariants  []VariantRef `json:"response_variants"`
}

type NormalizedOperation struct {
	OperationID            string                           `json:"operation_id"`
	SourceRefs             []contract.SourceRef             `json:"source_refs"`
	Transport              contract.Transport               `json:"transport"`
	Raw                    RawOperationFact                 `json:"raw"`
	CommonBindings         []BindingFact                    `json:"common_bindings"`
	RequestVariants        []RequestVariant                 `json:"request_variants"`
	ResponseVariants       []ResponseVariant                `json:"response_variants"`
	CapabilityRequirements []contract.CapabilityRequirement `json:"capability_requirements"`
	AuthenticationRequired bool                             `json:"authentication_required"`
	AuthenticationFacts    []SourceFact                     `json:"authentication_facts"`
	LaneInventory          []LaneDisposition                `json:"lane_inventory"`
	Semantics              *OperationSemantics              `json:"semantics,omitempty"`
	BlockedDiagnosticIDs   []string                         `json:"blocked_diagnostic_ids"`
}

type Normalization struct {
	ConnectorID       string `json:"connector_id"`
	SemanticsRevision string `json:"semantics_revision"`
	SourceDigest      string `json:"source_digest"`
	// SourceOperationIDs retains the exact source-lock inventory for emission completeness checks.
	SourceOperationIDs []string               `json:"-"`
	Conversions        []ConversionProvenance `json:"conversions"`
	Operations         []NormalizedOperation  `json:"operations"`
	Diagnostics        []Diagnostic           `json:"diagnostics"`
}

type SemanticsDocument struct {
	SchemaVersion string               `json:"schema_version"`
	ConnectorID   string               `json:"connector_id"`
	SourceDigest  string               `json:"source_digest"`
	Revision      string               `json:"revision"`
	Operations    []OperationSemantics `json:"operations"`
}

type OperationSemantics struct {
	OperationID      string               `json:"operation_id"`
	SourceRefs       []contract.SourceRef `json:"source_refs"`
	RecordExtraction *RecordExtraction    `json:"record_extraction,omitempty"`
	Pagination       *Pagination          `json:"pagination,omitempty"`
	Retry            *RetrySemantics      `json:"retry,omitempty"`
	Lanes            []LaneSemantics      `json:"lanes"`
}

type RecordExtraction struct {
	RecordsPointer string  `json:"records_pointer"`
	NextPointer    string  `json:"next_pointer,omitempty"`
	Witness        Witness `json:"witness"`
}

type Pagination struct {
	Kind                    string  `json:"kind"`
	CursorRequestField      string  `json:"cursor_request_field,omitempty"`
	CursorResponsePointer   string  `json:"cursor_response_pointer,omitempty"`
	OffsetRequestField      string  `json:"offset_request_field,omitempty"`
	LimitRequestField       string  `json:"limit_request_field,omitempty"`
	TotalResponsePointer    string  `json:"total_response_pointer,omitempty"`
	PageRequestField        string  `json:"page_request_field,omitempty"`
	LastPageResponsePointer string  `json:"last_page_response_pointer,omitempty"`
	Witness                 Witness `json:"witness"`
}

type RetrySemantics struct {
	Classification string  `json:"classification"`
	Condition      string  `json:"condition,omitempty"`
	Witness        Witness `json:"witness"`
}

type LaneSemantics struct {
	Lane           contract.Lane        `json:"lane"`
	Classification LaneStatus           `json:"classification"`
	Reason         string               `json:"reason,omitempty"`
	SourceRefs     []contract.SourceRef `json:"source_refs"`
}

type Witness struct {
	ProofID    string               `json:"proof_id"`
	SourceRefs []contract.SourceRef `json:"source_refs"`
}

func DecodeSemantics(document []byte) (SemanticsDocument, error) {
	var semantics SemanticsDocument
	if err := contract.DecodeStrictRequiredObject(
		document,
		&semantics,
		"schema_version",
		"connector_id",
		"source_digest",
		"revision",
		"operations",
	); err != nil {
		return SemanticsDocument{}, sourceError(ErrUnsafeAmendment, "", "", err.Error())
	}
	if err := semantics.Validate(); err != nil {
		return SemanticsDocument{}, err
	}
	return semantics, nil
}

func (document SemanticsDocument) Validate() error {
	if document.SchemaVersion != SemanticsSchemaVersion {
		return sourceError(ErrUnsafeAmendment, "", "/schema_version", "semantic amendment revision is unsupported")
	}
	if !validIdentity(document.ConnectorID) || !validDigest(document.SourceDigest) || !validDigest(document.Revision) {
		return sourceError(ErrUnsafeAmendment, "", "", "connector, source digest and amendment revision must be canonical")
	}
	seenOperations := make(map[string]struct{}, len(document.Operations))
	for index := range document.Operations {
		operation := &document.Operations[index]
		pointer := fmt.Sprintf("/operations/%d", index)
		if !validIdentity(operation.OperationID) {
			return sourceError(ErrUnsafeAmendment, "", pointer+"/operation_id", "operation identity is invalid")
		}
		if _, duplicate := seenOperations[operation.OperationID]; duplicate {
			return sourceError(ErrUnsafeAmendment, "", pointer+"/operation_id", "operation amendment is duplicated")
		}
		seenOperations[operation.OperationID] = struct{}{}
		if len(operation.SourceRefs) == 0 {
			return sourceError(ErrUnsafeAmendment, "", pointer+"/source_refs", "amendment must be source-linked")
		}
		if operation.RecordExtraction != nil {
			if !validJSONPointer(operation.RecordExtraction.RecordsPointer) ||
				(operation.RecordExtraction.NextPointer != "" && !validJSONPointer(operation.RecordExtraction.NextPointer)) {
				return sourceError(ErrUnsafeAmendment, "", pointer+"/record_extraction", "record pointers are malformed")
			}
			if err := operation.RecordExtraction.Witness.validate(pointer + "/record_extraction/witness"); err != nil {
				return err
			}
		}
		if operation.Pagination != nil {
			if err := operation.Pagination.validate(pointer + "/pagination"); err != nil {
				return err
			}
		}
		if operation.Retry != nil {
			if err := operation.Retry.validate(pointer + "/retry"); err != nil {
				return err
			}
		}
		seenLanes := make(map[contract.Lane]struct{}, len(operation.Lanes))
		for laneIndex, lane := range operation.Lanes {
			lanePointer := fmt.Sprintf("%s/lanes/%d", pointer, laneIndex)
			if err := contract.ValidateLane(lane.Lane); err != nil {
				return sourceError(ErrUnsafeAmendment, "", lanePointer+"/lane", "lane is unsupported")
			}
			if _, duplicate := seenLanes[lane.Lane]; duplicate {
				return sourceError(ErrUnsafeAmendment, "", lanePointer+"/lane", "lane amendment is duplicated")
			}
			seenLanes[lane.Lane] = struct{}{}
			if !validLaneStatus(lane.Classification) {
				return sourceError(ErrUnsafeAmendment, "", lanePointer+"/classification", "lane classification is unsupported")
			}
			if lane.Classification != LaneEligible && !validIdentity(lane.Reason) {
				return sourceError(ErrUnsafeAmendment, "", lanePointer+"/reason", "non-eligible lane requires an explicit reason")
			}
			if len(lane.SourceRefs) == 0 {
				return sourceError(ErrUnsafeAmendment, "", lanePointer+"/source_refs", "lane classification must be source-linked")
			}
		}
	}
	return nil
}

func (pagination Pagination) validate(pointer string) error {
	if err := pagination.Witness.validate(pointer + "/witness"); err != nil {
		return err
	}
	switch pagination.Kind {
	case "cursor":
		if !validIdentity(pagination.CursorRequestField) || !validJSONPointer(pagination.CursorResponsePointer) || pagination.CursorResponsePointer == "" ||
			pagination.OffsetRequestField != "" || pagination.LimitRequestField != "" || pagination.TotalResponsePointer != "" ||
			pagination.PageRequestField != "" || pagination.LastPageResponsePointer != "" {
			return sourceError(ErrUnsafeAmendment, "", pointer, "cursor pagination fields are incomplete or contradictory")
		}
	case "offset":
		if !validIdentity(pagination.OffsetRequestField) || !validIdentity(pagination.LimitRequestField) ||
			(pagination.TotalResponsePointer != "" && !validJSONPointer(pagination.TotalResponsePointer)) ||
			pagination.CursorRequestField != "" || pagination.CursorResponsePointer != "" ||
			pagination.PageRequestField != "" || pagination.LastPageResponsePointer != "" {
			return sourceError(ErrUnsafeAmendment, "", pointer, "offset pagination fields are incomplete or contradictory")
		}
	case "page":
		if !validIdentity(pagination.PageRequestField) ||
			(pagination.LastPageResponsePointer != "" && !validJSONPointer(pagination.LastPageResponsePointer)) ||
			pagination.CursorRequestField != "" || pagination.CursorResponsePointer != "" ||
			pagination.OffsetRequestField != "" || pagination.LimitRequestField != "" || pagination.TotalResponsePointer != "" {
			return sourceError(ErrUnsafeAmendment, "", pointer, "page pagination fields are incomplete or contradictory")
		}
	default:
		return sourceError(ErrUnsafeAmendment, "", pointer+"/kind", "pagination kind is unsupported")
	}
	return nil
}

func (retry RetrySemantics) validate(pointer string) error {
	if err := retry.Witness.validate(pointer + "/witness"); err != nil {
		return err
	}
	switch retry.Classification {
	case "safe", "unsafe":
		if retry.Condition != "" {
			return sourceError(ErrUnsafeAmendment, "", pointer+"/condition", "unconditional retry classification cannot carry a condition")
		}
	case "conditional":
		if !validIdentity(retry.Condition) {
			return sourceError(ErrUnsafeAmendment, "", pointer+"/condition", "conditional retry classification requires an explicit condition")
		}
	default:
		return sourceError(ErrUnsafeAmendment, "", pointer+"/classification", "retry classification is unsupported")
	}
	return nil
}

func (witness Witness) validate(pointer string) error {
	if !validIdentity(witness.ProofID) || len(witness.SourceRefs) == 0 {
		return sourceError(ErrUnsafeAmendment, "", pointer, "witness identity and source references are required")
	}
	return nil
}

func defaultLaneInventory(sourceRefs []contract.SourceRef) []LaneDisposition {
	inventory := make([]LaneDisposition, 0, len(allLanes))
	for _, lane := range allLanes {
		inventory = append(inventory, LaneDisposition{
			Lane:       lane,
			Status:     LaneUnproven,
			Reason:     "no admitted source-linked lane classification",
			SourceRefs: slices.Clone(sourceRefs),
		})
	}
	return inventory
}

func applyLaneSemantics(inventory []LaneDisposition, semantics []LaneSemantics) []LaneDisposition {
	result := slices.Clone(inventory)
	byLane := make(map[contract.Lane]LaneSemantics, len(semantics))
	for _, lane := range semantics {
		byLane[lane.Lane] = lane
	}
	for index := range result {
		lane, ok := byLane[result[index].Lane]
		if !ok {
			continue
		}
		result[index].Status = lane.Classification
		result[index].Reason = lane.Reason
		if lane.Classification == LaneEligible && lane.Reason == "" {
			result[index].Reason = "source-linked semantic amendment classifies this lane as eligible"
		}
		result[index].SourceRefs = slices.Clone(lane.SourceRefs)
	}
	return result
}

func validLaneStatus(status LaneStatus) bool {
	switch status {
	case LaneEligible, LaneUnsupported, LaneNotApplicable, LaneBlocked, LaneUnproven:
		return true
	default:
		return false
	}
}

func sortDiagnostics(diagnostics []Diagnostic) {
	slices.SortFunc(diagnostics, func(left, right Diagnostic) int {
		if compared := strings.Compare(left.ID, right.ID); compared != 0 {
			return compared
		}
		return strings.Compare(left.Code, right.Code)
	})
}
