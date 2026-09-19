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
	AnyOfPointers []string           `json:"any_of_pointers"`
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
	SourceOperationID string                           `json:"source_operation_id"`
	SourceFacts       []SourceFact                     `json:"source_facts"`
	Method            string                           `json:"method,omitempty"`
	Path              string                           `json:"path,omitempty"`
	StatementKind     contract.PostgreSQLStatementKind `json:"statement_kind,omitempty"`
	RelationBinding   string                           `json:"relation_binding,omitempty"`
	RequestVariants   []VariantRef                     `json:"request_variants"`
	ResponseVariants  []VariantRef                     `json:"response_variants"`
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
	HTTP                   *HTTPSemantics                   `json:"http,omitempty"`
	HTTPFacts              []SourceFact                     `json:"http_facts"`
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
	HTTP             *HTTPSemantics       `json:"http,omitempty"`
	RecordExtraction *RecordExtraction    `json:"record_extraction,omitempty"`
	Pagination       *Pagination          `json:"pagination,omitempty"`
	Retry            *RetrySemantics      `json:"retry,omitempty"`
	Lanes            []LaneSemantics      `json:"lanes"`
}

type HTTPSemantics struct {
	Origins       []string                         `json:"origins"`
	Authorization *contract.OperationAuthorization `json:"authorization,omitempty"`
	Headers       []contract.OperationHTTPHeader   `json:"headers,omitempty"`
	Witness       Witness                          `json:"witness"`
}

func (httpSemantics *HTTPSemantics) UnmarshalJSON(data []byte) error {
	type wire HTTPSemantics
	var decoded wire
	if err := contract.DecodeStrictRequiredObject(data, &decoded, "origins", "witness"); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if len(decoded.Origins) == 0 {
		return fmt.Errorf("HTTP semantics require at least one source-admitted origin")
	}
	if _, present := fields["headers"]; present && len(decoded.Headers) == 0 {
		return fmt.Errorf("explicit empty HTTP headers cannot stand in for absent fixed headers")
	}
	*httpSemantics = HTTPSemantics(decoded)
	return nil
}

type RecordExtraction struct {
	RecordsPointer string  `json:"records_pointer"`
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
	LinkRequestField        string  `json:"link_request_field,omitempty"`
	LinkResponseHeader      string  `json:"link_response_header,omitempty"`
	Witness                 Witness `json:"witness"`
}

func (pagination *Pagination) UnmarshalJSON(data []byte) error {
	type wire Pagination
	var decoded wire
	if err := contract.DecodeStrictRequiredObject(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	allowed := map[string]struct{}{"kind": {}, "witness": {}}
	switch decoded.Kind {
	case string(contract.OperationPaginationCursor):
		allowed["cursor_request_field"] = struct{}{}
		allowed["cursor_response_pointer"] = struct{}{}
	case string(contract.OperationPaginationOffset):
		allowed["offset_request_field"] = struct{}{}
		allowed["limit_request_field"] = struct{}{}
		allowed["total_response_pointer"] = struct{}{}
		if raw, present := fields["total_response_pointer"]; present && strings.TrimSpace(string(raw)) == `""` {
			return fmt.Errorf("offset pagination cannot declare an empty total response pointer")
		}
	case string(contract.OperationPaginationPage):
		allowed["page_request_field"] = struct{}{}
		allowed["last_page_response_pointer"] = struct{}{}
		if raw, present := fields["last_page_response_pointer"]; present && strings.TrimSpace(string(raw)) == `""` {
			return fmt.Errorf("page pagination cannot declare an empty last-page response pointer")
		}
	case string(contract.OperationPaginationLink):
		allowed["link_request_field"] = struct{}{}
		allowed["link_response_header"] = struct{}{}
		allowed["limit_request_field"] = struct{}{}
		if raw, present := fields["limit_request_field"]; present && strings.TrimSpace(string(raw)) == `""` {
			return fmt.Errorf("link pagination cannot declare an empty limit request field")
		}
	default:
		allowed["cursor_request_field"] = struct{}{}
		allowed["cursor_response_pointer"] = struct{}{}
		allowed["offset_request_field"] = struct{}{}
		allowed["limit_request_field"] = struct{}{}
		allowed["total_response_pointer"] = struct{}{}
		allowed["page_request_field"] = struct{}{}
		allowed["last_page_response_pointer"] = struct{}{}
		allowed["link_request_field"] = struct{}{}
		allowed["link_response_header"] = struct{}{}
	}
	for name := range fields {
		if _, ok := allowed[name]; !ok {
			return fmt.Errorf("pagination field %q contradicts kind %q", name, decoded.Kind)
		}
	}
	*pagination = Pagination(decoded)
	return nil
}

func (retry *RetrySemantics) UnmarshalJSON(data []byte) error {
	type wire RetrySemantics
	var decoded wire
	if err := contract.DecodeStrictRequiredObject(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	_, conditionPresent := fields["condition"]
	if decoded.Classification == string(contract.OperationRetryConditional) {
		if !conditionPresent {
			return fmt.Errorf("conditional retry classification requires an explicit condition")
		}
	} else if conditionPresent {
		return fmt.Errorf("unconditional retry classification cannot carry a condition field")
	}
	if _, statusesPresent := fields["http_statuses"]; statusesPresent && len(decoded.HTTPStatuses) == 0 {
		return fmt.Errorf("explicit empty retry HTTP statuses cannot stand in for absent status facts")
	}
	*retry = RetrySemantics(decoded)
	return nil
}

type RetrySemantics struct {
	Classification string   `json:"classification"`
	Condition      string   `json:"condition,omitempty"`
	HTTPStatuses   []uint16 `json:"http_statuses,omitempty"`
	Witness        Witness  `json:"witness"`
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

func (semantics OperationSemantics) clone() OperationSemantics {
	cloned := semantics
	cloned.SourceRefs = slices.Clone(semantics.SourceRefs)
	if semantics.HTTP != nil {
		httpSemantics := semantics.HTTP.clone()
		cloned.HTTP = &httpSemantics
	}
	if semantics.RecordExtraction != nil {
		recordExtraction := *semantics.RecordExtraction
		recordExtraction.Witness = semantics.RecordExtraction.Witness.clone()
		cloned.RecordExtraction = &recordExtraction
	}
	if semantics.Pagination != nil {
		pagination := *semantics.Pagination
		pagination.Witness = semantics.Pagination.Witness.clone()
		cloned.Pagination = &pagination
	}
	if semantics.Retry != nil {
		retry := *semantics.Retry
		retry.HTTPStatuses = slices.Clone(semantics.Retry.HTTPStatuses)
		retry.Witness = semantics.Retry.Witness.clone()
		cloned.Retry = &retry
	}
	cloned.Lanes = slices.Clone(semantics.Lanes)
	for index := range cloned.Lanes {
		cloned.Lanes[index].SourceRefs = slices.Clone(semantics.Lanes[index].SourceRefs)
	}
	return cloned
}

func (httpSemantics HTTPSemantics) clone() HTTPSemantics {
	httpSemantics.Origins = slices.Clone(httpSemantics.Origins)
	httpSemantics.Headers = slices.Clone(httpSemantics.Headers)
	if httpSemantics.Authorization != nil {
		authorization := *httpSemantics.Authorization
		httpSemantics.Authorization = &authorization
	}
	httpSemantics.Witness = httpSemantics.Witness.clone()
	return httpSemantics
}

func semanticsToContractHTTP(httpSemantics HTTPSemantics) contract.OperationHTTP {
	var authorization *contract.OperationAuthorization
	if httpSemantics.Authorization != nil {
		cloned := *httpSemantics.Authorization
		authorization = &cloned
	}
	return contract.OperationHTTP{
		Origins:       slices.Clone(httpSemantics.Origins),
		Authorization: authorization,
		Headers:       slices.Clone(httpSemantics.Headers),
		Witness: contract.ExecutionWitness{
			ProofID:    httpSemantics.Witness.ProofID,
			SourceRefs: slices.Clone(httpSemantics.Witness.SourceRefs),
		},
	}
}

func (witness Witness) clone() Witness {
	witness.SourceRefs = slices.Clone(witness.SourceRefs)
	return witness
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
		if operation.HTTP != nil {
			if err := operation.HTTP.validate(pointer + "/http"); err != nil {
				return err
			}
		}
		if operation.RecordExtraction != nil {
			if len(operation.RecordExtraction.RecordsPointer) > 4096 || !validJSONPointer(operation.RecordExtraction.RecordsPointer) {
				return sourceError(ErrUnsafeAmendment, "", pointer+"/record_extraction", "record pointer is malformed")
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

func (httpSemantics HTTPSemantics) validate(pointer string) error {
	if err := httpSemantics.Witness.validate(pointer + "/witness"); err != nil {
		return err
	}
	inputs := []contract.FieldBinding{}
	if httpSemantics.Authorization != nil {
		authorization := httpSemantics.Authorization
		inputs = append(inputs, contract.FieldBinding{
			FieldID:     authorization.FieldID,
			WireName:    authorization.WireName,
			Location:    authorization.Placement,
			Required:    authorization.Required,
			Sensitivity: contract.SensitivitySecret,
			Schema:      contract.Schema{Type: contract.SchemaTypeString},
		})
	}
	httpFact := contract.OperationHTTP{
		Origins:       slices.Clone(httpSemantics.Origins),
		Authorization: httpSemantics.Authorization,
		Headers:       slices.Clone(httpSemantics.Headers),
		Witness: contract.ExecutionWitness{
			ProofID:    httpSemantics.Witness.ProofID,
			SourceRefs: slices.Clone(httpSemantics.Witness.SourceRefs),
		},
	}
	if err := httpFact.Validate(contract.Operation{
		Transport: contract.Transport{Kind: contract.TransportKindHTTP},
		Inputs:    inputs,
	}); err != nil {
		return sourceError(ErrUnsafeAmendment, "", pointer, err.Error())
	}
	return nil
}
func (pagination Pagination) validate(pointer string) error {
	if err := pagination.Witness.validate(pointer + "/witness"); err != nil {
		return err
	}
	switch pagination.Kind {
	case string(contract.OperationPaginationCursor):
		if !validIdentity(pagination.CursorRequestField) || pagination.CursorResponsePointer == "" ||
			len(pagination.CursorResponsePointer) > 4096 || !validJSONPointer(pagination.CursorResponsePointer) ||
			pagination.OffsetRequestField != "" || pagination.LimitRequestField != "" || pagination.TotalResponsePointer != "" ||
			pagination.PageRequestField != "" || pagination.LastPageResponsePointer != "" ||
			pagination.LinkRequestField != "" || pagination.LinkResponseHeader != "" {
			return sourceError(ErrUnsafeAmendment, "", pointer, "cursor pagination fields are incomplete or contradictory")
		}
	case string(contract.OperationPaginationOffset):
		if !validIdentity(pagination.OffsetRequestField) || !validIdentity(pagination.LimitRequestField) ||
			pagination.OffsetRequestField == pagination.LimitRequestField ||
			(pagination.TotalResponsePointer != "" && (len(pagination.TotalResponsePointer) > 4096 || !validJSONPointer(pagination.TotalResponsePointer))) ||
			pagination.CursorRequestField != "" || pagination.CursorResponsePointer != "" ||
			pagination.PageRequestField != "" || pagination.LastPageResponsePointer != "" ||
			pagination.LinkRequestField != "" || pagination.LinkResponseHeader != "" {
			return sourceError(ErrUnsafeAmendment, "", pointer, "offset pagination fields are incomplete or contradictory")
		}
	case string(contract.OperationPaginationPage):
		if !validIdentity(pagination.PageRequestField) ||
			(pagination.LastPageResponsePointer != "" && (len(pagination.LastPageResponsePointer) > 4096 || !validJSONPointer(pagination.LastPageResponsePointer))) ||
			pagination.CursorRequestField != "" || pagination.CursorResponsePointer != "" ||
			pagination.OffsetRequestField != "" || pagination.LimitRequestField != "" || pagination.TotalResponsePointer != "" ||
			pagination.LinkRequestField != "" || pagination.LinkResponseHeader != "" {
			return sourceError(ErrUnsafeAmendment, "", pointer, "page pagination fields are incomplete or contradictory")
		}
	case string(contract.OperationPaginationLink):
		if !validIdentity(pagination.LinkRequestField) || !strings.EqualFold(pagination.LinkResponseHeader, "Link") ||
			pagination.CursorRequestField != "" || pagination.CursorResponsePointer != "" ||
			pagination.OffsetRequestField != "" || pagination.TotalResponsePointer != "" ||
			pagination.PageRequestField != "" || pagination.LastPageResponsePointer != "" ||
			(pagination.LimitRequestField != "" && (!validIdentity(pagination.LimitRequestField) ||
				pagination.LimitRequestField == pagination.LinkRequestField)) {
			return sourceError(ErrUnsafeAmendment, "", pointer, "link pagination fields are incomplete or contradictory")
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
	case string(contract.OperationRetrySafe), string(contract.OperationRetryUnsafe):
		if retry.Condition != "" {
			return sourceError(ErrUnsafeAmendment, "", pointer+"/condition", "unconditional retry classification cannot carry a condition")
		}
	case string(contract.OperationRetryConditional):
		if !validIdentity(retry.Condition) {
			return sourceError(ErrUnsafeAmendment, "", pointer+"/condition", "conditional retry classification requires an explicit condition")
		}
	default:
		return sourceError(ErrUnsafeAmendment, "", pointer+"/classification", "retry classification is unsupported")
	}
	var previous uint16
	for index, status := range retry.HTTPStatuses {
		if status < 400 || status > 599 {
			return sourceError(ErrUnsafeAmendment, "", pointer+"/http_statuses", "retry HTTP status must be between 400 and 599")
		}
		if index != 0 && status <= previous {
			return sourceError(ErrUnsafeAmendment, "", pointer+"/http_statuses", "retry HTTP statuses must be unique and sorted")
		}
		previous = status
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
