package contract

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"
)

const OperationSchemaVersion = "pm.operation/v1"

type SourceRef struct {
	SourceID string `json:"source_id"`
	Pointer  string `json:"pointer"`
}

const (
	TransportKindHTTP       = "http"
	TransportKindPostgreSQL = "postgresql"
)

const RequestMediaTypeWireName = "Content-Type"

type PostgreSQLStatementKind string

const (
	PostgreSQLStatementMetadata     PostgreSQLStatementKind = "metadata"
	PostgreSQLStatementSelect       PostgreSQLStatementKind = "select"
	PostgreSQLStatementMappedUpsert PostgreSQLStatementKind = "mapped_upsert"
)

type Transport struct {
	Kind            string                  `json:"kind"`
	Method          string                  `json:"method,omitempty"`
	Path            string                  `json:"path,omitempty"`
	StatementKind   PostgreSQLStatementKind `json:"statement_kind,omitempty"`
	RelationBinding string                  `json:"relation_binding,omitempty"`
}

type RequestVariant struct {
	VariantID  string         `json:"variant_id"`
	MediaType  string         `json:"media_type"`
	Required   bool           `json:"required"`
	Schema     Schema         `json:"schema"`
	Inputs     []FieldBinding `json:"inputs"`
	SourceRefs []SourceRef    `json:"source_refs"`
}

func (r *RequestVariant) UnmarshalJSON(data []byte) error {
	type wire struct {
		VariantID  string         `json:"variant_id"`
		MediaType  string         `json:"media_type"`
		Required   *bool          `json:"required"`
		Schema     Schema         `json:"schema"`
		Inputs     []FieldBinding `json:"inputs"`
		SourceRefs []SourceRef    `json:"source_refs"`
	}
	var decoded wire
	if err := decodeStrictJSON(data, &decoded); err != nil {
		return err
	}
	if decoded.Required == nil {
		return contractError(ErrInvalidContract, "request_variant.required", "must be explicit")
	}
	*r = RequestVariant{
		VariantID:  decoded.VariantID,
		MediaType:  decoded.MediaType,
		Required:   *decoded.Required,
		Schema:     decoded.Schema,
		Inputs:     decoded.Inputs,
		SourceRefs: decoded.SourceRefs,
	}
	return nil
}

type Response struct {
	Status        string      `json:"status"`
	NoContent     bool        `json:"no_content"`
	MediaType     string      `json:"media_type,omitempty"`
	Schema        *Schema     `json:"schema,omitempty"`
	SourceRefs    []SourceRef `json:"source_refs"`
	RecordPointer *string     `json:"record_pointer,omitempty"`
}

func (r *Response) UnmarshalJSON(data []byte) error {
	type wire struct {
		Status        string      `json:"status"`
		NoContent     *bool       `json:"no_content"`
		MediaType     string      `json:"media_type,omitempty"`
		Schema        *Schema     `json:"schema,omitempty"`
		SourceRefs    []SourceRef `json:"source_refs"`
		RecordPointer *string     `json:"record_pointer,omitempty"`
	}
	var decoded wire
	if err := decodeStrictJSON(data, &decoded); err != nil {
		return err
	}
	if decoded.NoContent == nil {
		return contractError(ErrInvalidContract, "response.no_content", "must be explicit")
	}
	*r = Response{
		Status:        decoded.Status,
		NoContent:     *decoded.NoContent,
		MediaType:     decoded.MediaType,
		Schema:        decoded.Schema,
		SourceRefs:    decoded.SourceRefs,
		RecordPointer: decoded.RecordPointer,
	}
	return nil
}

type Lane string

const (
	LaneDirectRead     Lane = "direct_read"
	LaneDirectWrite    Lane = "direct_write"
	LaneETL            Lane = "etl"
	LaneReverseETL     Lane = "reverse_etl"
	LaneBinaryDownload Lane = "binary_download"
	LaneBinaryUpload   Lane = "binary_upload"
	LaneSyncTransport  Lane = "sync_transport"
)

type LaneStatus string

const (
	LaneEligible      LaneStatus = "eligible"
	LaneUnsupported   LaneStatus = "unsupported"
	LaneNotApplicable LaneStatus = "not_applicable"
	LaneBlocked       LaneStatus = "blocked"
	LaneUnproven      LaneStatus = "unproven"
)

type LaneEligibility struct {
	Lane   Lane       `json:"lane"`
	Status LaneStatus `json:"status"`
	Reason string     `json:"reason"`
}

// UnavailableOperation preserves source-selected operation identity and exact
// refusal classifications without manufacturing a partial executable contract.
type UnavailableOperation struct {
	OperationID     string            `json:"operation_id"`
	SourceRefs      []SourceRef       `json:"source_refs"`
	DiagnosticCodes []string          `json:"diagnostic_codes"`
	LaneEligibility []LaneEligibility `json:"lane_eligibility"`
}

func (o UnavailableOperation) Validate() error {
	if !validIdentityPart(o.OperationID) {
		return contractError(ErrInvalidContract, "unavailable_operation.operation_id", "must be a nonempty stable operation identity")
	}
	if err := validateSourceRefs(o.SourceRefs, "unavailable_operation.source_refs"); err != nil {
		return err
	}
	if len(o.DiagnosticCodes) == 0 {
		return contractError(ErrInvalidContract, "unavailable_operation.diagnostic_codes", "must preserve at least one exact diagnostic classification")
	}
	previousCode := ""
	for _, code := range o.DiagnosticCodes {
		if !validIdentityPart(code) || strings.ContainsAny(code, " \t\r\n") {
			return contractError(ErrInvalidContract, "unavailable_operation.diagnostic_codes", "contains an invalid diagnostic classification")
		}
		if previousCode != "" {
			switch strings.Compare(previousCode, code) {
			case 0:
				return contractError(ErrDuplicateField, "unavailable_operation.diagnostic_codes", "diagnostic classification is duplicated")
			case 1:
				return contractError(ErrInvalidContract, "unavailable_operation.diagnostic_codes", "diagnostic classifications must be sorted")
			}
		}
		previousCode = code
	}
	if len(o.LaneEligibility) != len(canonicalLanes) {
		return contractError(ErrInvalidContract, "unavailable_operation.lane_eligibility", "must declare all seven canonical lanes exactly once")
	}
	seenLanes := make(map[Lane]struct{}, len(o.LaneEligibility))
	for _, eligibility := range o.LaneEligibility {
		if !validLane(eligibility.Lane) {
			return contractError(ErrInvalidContract, "unavailable_operation.lane_eligibility.lane", "lane is unsupported")
		}
		if _, duplicate := seenLanes[eligibility.Lane]; duplicate {
			return contractError(ErrDuplicateField, "unavailable_operation.lane_eligibility", "lane disposition is duplicated")
		}
		seenLanes[eligibility.Lane] = struct{}{}
		if !validLaneStatus(eligibility.Status) || eligibility.Status == LaneEligible {
			return contractError(ErrInvalidContract, "unavailable_operation.lane_eligibility.status", "unavailable operation cannot retain an executable lane")
		}
		if !validBoundedIdentityPart(eligibility.Reason, 4096) {
			return contractError(ErrInvalidContract, "unavailable_operation.lane_eligibility.reason", "every lane requires a canonical reason")
		}
	}
	return nil
}

type Operation struct {
	SchemaVersion          string                  `json:"schema_version"`
	OperationID            string                  `json:"operation_id"`
	SourceRefs             []SourceRef             `json:"source_refs"`
	Transport              Transport               `json:"transport"`
	Inputs                 []FieldBinding          `json:"inputs"`
	RequestVariants        []RequestVariant        `json:"request_variants"`
	RequestMediaFieldID    string                  `json:"request_media_field_id,omitempty"`
	Responses              []Response              `json:"responses"`
	CapabilityRequirements []CapabilityRequirement `json:"capability_requirements"`
	LaneEligibility        []LaneEligibility       `json:"lane_eligibility"`
}

func DecodeOperation(document []byte, supported []FoundationContract) (Operation, error) {
	var fields map[string]json.RawMessage
	if err := decodeContractJSON(document, &fields); err != nil {
		return Operation{}, err
	}
	for _, required := range []string{
		"schema_version", "operation_id", "source_refs", "transport", "inputs",
		"request_variants", "responses", "capability_requirements", "lane_eligibility",
	} {
		if _, ok := fields[required]; !ok {
			return Operation{}, contractError(ErrInvalidContract, required, "required field is absent")
		}
	}
	var operation Operation
	if err := decodeStrictJSON(document, &operation); err != nil {
		return Operation{}, err
	}
	if err := operation.Validate(supported); err != nil {
		return Operation{}, err
	}
	return operation, nil
}

func (o Operation) Validate(supported []FoundationContract) error {
	if o.SchemaVersion != OperationSchemaVersion {
		return contractError(ErrUnsupportedVersion, "schema_version", "operation schema revision is unsupported")
	}
	if !validIdentityPart(o.OperationID) {
		return contractError(ErrInvalidContract, "operation_id", "must be a nonempty stable operation identity")
	}
	if err := validateSourceRefs(o.SourceRefs, "source_refs"); err != nil {
		return err
	}
	if err := o.Transport.Validate(); err != nil {
		return err
	}
	if o.Inputs == nil {
		return contractError(ErrInvalidContract, "inputs", "must be explicitly declared, even when empty")
	}

	seenFieldIDs := make(map[string]struct{}, len(o.Inputs))
	type wireLocation struct {
		location FieldLocation
		address  string
	}
	seenWireLocations := make(map[wireLocation]struct{}, len(o.Inputs))
	commonBindings := make(map[string]FieldBinding, len(o.Inputs))
	for _, input := range o.Inputs {
		if err := input.Validate(); err != nil {
			return err
		}
		if input.Location == FieldLocationBody {
			return contractError(ErrInvalidContract, "inputs", "request-body bindings must be local to a request variant")
		}
		if _, duplicate := seenFieldIDs[input.FieldID]; duplicate {
			return contractError(ErrDuplicateField, "inputs.field_id", "stable field ID is duplicated")
		}
		seenFieldIDs[input.FieldID] = struct{}{}
		commonBindings[input.FieldID] = input
		address := input.WireName
		if input.Location == FieldLocationRecord || input.Location == FieldLocationMetadata {
			address = input.JSONPointer
		}
		wireKey := wireLocation{location: input.Location, address: address}
		if _, duplicate := seenWireLocations[wireKey]; duplicate {
			return contractError(ErrDuplicateField, "inputs", "wire location is duplicated")
		}
		seenWireLocations[wireKey] = struct{}{}
	}

	if o.RequestVariants == nil {
		return contractError(ErrInvalidContract, "request_variants", "must be explicitly declared, even when empty")
	}
	seenVariantIDs := make(map[string]struct{}, len(o.RequestVariants))
	seenMediaTypes := make(map[string]struct{}, len(o.RequestVariants))
	for index := range o.RequestVariants {
		variant := o.RequestVariants[index]
		if err := variant.Validate(); err != nil {
			return err
		}
		if _, duplicate := seenVariantIDs[variant.VariantID]; duplicate {
			return contractError(ErrDuplicateField, "request_variants.variant_id", "request variant identity is duplicated")
		}
		seenVariantIDs[variant.VariantID] = struct{}{}
		if _, duplicate := seenMediaTypes[variant.MediaType]; duplicate {
			return contractError(ErrDuplicateField, "request_variants.media_type", "request media type is duplicated")
		}
		seenMediaTypes[variant.MediaType] = struct{}{}
		for _, input := range variant.Inputs {
			if _, duplicate := seenFieldIDs[input.FieldID]; duplicate {
				return contractError(ErrDuplicateField, "request_variants.inputs.field_id", "variant-local field ID collides within the operation")
			}
			seenFieldIDs[input.FieldID] = struct{}{}
		}
	}
	if len(o.RequestVariants) == 1 && o.RequestVariants[0].Required &&
		!singleRequiredVariantCanBeRepresented(o.RequestVariants[0]) {
		return contractError(ErrInvalidContract, "request_variants.inputs", "single required request body needs bindings that can represent its required root, including an explicit whole-root binding for an all-optional object")
	}
	if err := validateRequestMediaSelector(o, commonBindings); err != nil {
		return err
	}

	if len(o.Responses) == 0 {
		return contractError(ErrInvalidContract, "responses", "must preserve every declared response variant")
	}
	seenResponses := make(map[string]struct{}, len(o.Responses))
	for _, response := range o.Responses {
		if err := response.Validate(o.Transport.Kind); err != nil {
			return err
		}
		key := response.Status + "\x00" + response.MediaType
		if _, duplicate := seenResponses[key]; duplicate {
			return contractError(ErrDuplicateField, "responses", "status/media variant is duplicated")
		}
		seenResponses[key] = struct{}{}
	}
	if len(o.CapabilityRequirements) == 0 {
		return contractError(ErrInvalidContract, "capability_requirements", "must declare the exact capability closure")
	}
	requiredFoundations := make([]FoundationRef, 0, len(o.CapabilityRequirements))
	seenCapabilities := make(map[string]struct{}, len(o.CapabilityRequirements))
	seenFoundations := make(map[string]struct{}, len(o.CapabilityRequirements))
	for _, requirement := range o.CapabilityRequirements {
		if err := requirement.Validate(); err != nil {
			return err
		}
		key := requirement.Capability + "\x00" + requirement.Foundation.ID
		if _, duplicate := seenCapabilities[key]; duplicate {
			return contractError(ErrDuplicateField, "capability_requirements", "capability/foundation binding is duplicated")
		}
		seenCapabilities[key] = struct{}{}
		foundationKey := requirement.Foundation.ID + "@" + requirement.Foundation.Version
		if _, seen := seenFoundations[foundationKey]; !seen {
			seenFoundations[foundationKey] = struct{}{}
			requiredFoundations = append(requiredFoundations, requirement.Foundation)
		}
	}
	if err := CheckFoundationCompatibility(requiredFoundations, supported); err != nil {
		return err
	}
	if len(o.LaneEligibility) != len(canonicalLanes) {
		return contractError(ErrInvalidContract, "lane_eligibility", "must declare all seven canonical lanes exactly once")
	}
	seenLanes := make(map[Lane]struct{}, len(o.LaneEligibility))
	for _, eligibility := range o.LaneEligibility {
		if !validLane(eligibility.Lane) {
			return contractError(ErrInvalidContract, "lane_eligibility.lane", "lane is unsupported")
		}
		if _, duplicate := seenLanes[eligibility.Lane]; duplicate {
			return contractError(ErrDuplicateField, "lane_eligibility", "lane disposition is duplicated")
		}
		seenLanes[eligibility.Lane] = struct{}{}
		if !validLaneStatus(eligibility.Status) {
			return contractError(ErrInvalidContract, "lane_eligibility.status", "lane status is unsupported")
		}
		if !validBoundedIdentityPart(eligibility.Reason, 4096) {
			return contractError(ErrInvalidContract, "lane_eligibility.reason", "every lane requires a canonical reason")
		}
	}
	return nil
}

func (r RequestVariant) Validate() error {
	if !validIdentityPart(r.VariantID) {
		return contractError(ErrInvalidContract, "request_variant.variant_id", "must be a stable request variant identity")
	}
	if err := validateMediaType(r.MediaType); err != nil {
		return err
	}
	if err := r.Schema.Validate(); err != nil {
		return err
	}
	if r.Inputs == nil {
		return contractError(ErrInvalidContract, "request_variant.inputs", "must be explicitly declared, even when empty")
	}
	if err := validateSourceRefs(r.SourceRefs, "request_variant.source_refs"); err != nil {
		return err
	}
	seenFields := make(map[string]struct{}, len(r.Inputs))
	seenPointers := make(map[string]struct{}, len(r.Inputs))
	for _, input := range r.Inputs {
		if err := input.Validate(); err != nil {
			return err
		}
		if input.Location != FieldLocationBody {
			return contractError(ErrInvalidContract, "request_variant.inputs", "variant-local bindings must be request-body bindings")
		}
		if _, duplicate := seenFields[input.FieldID]; duplicate {
			return contractError(ErrDuplicateField, "request_variant.inputs.field_id", "variant-local field ID is duplicated")
		}
		seenFields[input.FieldID] = struct{}{}
		if _, duplicate := seenPointers[input.JSONPointer]; duplicate {
			return contractError(ErrDuplicateField, "request_variant.inputs.json_pointer", "variant-local body location is duplicated")
		}
		seenPointers[input.JSONPointer] = struct{}{}
		if !requestBindingMatchesRootSchema(input, r.Schema) {
			return contractError(ErrInvalidContract, "request_variant.inputs.schema", "binding schema does not match its full-root request schema location")
		}
	}
	return nil
}

func singleRequiredVariantCanBeRepresented(variant RequestVariant) bool {
	hasRootBinding := false
	for _, input := range variant.Inputs {
		if input.JSONPointer == "" {
			hasRootBinding = true
			break
		}
	}
	if variant.Schema.Type != SchemaTypeObject {
		return hasRootBinding
	}
	if schemaObjectHasRequiredProperty(variant.Schema) {
		return len(variant.Inputs) != 0
	}
	return hasRootBinding
}

func schemaObjectHasRequiredProperty(schema Schema) bool {
	if len(schema.Required) != 0 {
		return true
	}
	for _, component := range schema.AllOf {
		if schemaObjectHasRequiredProperty(component) {
			return true
		}
	}
	return false
}

func (t Transport) Validate() error {
	switch t.Kind {
	case TransportKindHTTP:
		if t.Method == "" || t.Method != strings.ToUpper(t.Method) {
			return contractError(ErrInvalidContract, "transport.method", "HTTP method must be explicit uppercase")
		}
		if _, ok := allowedHTTPMethods[t.Method]; !ok {
			return contractError(ErrInvalidContract, "transport.method", "HTTP method is unsupported")
		}
		if t.Path == "" || len(t.Path) > 4096 || t.Path[0] != '/' || !utf8.ValidString(t.Path) {
			return contractError(ErrInvalidContract, "transport.path", "HTTP path must be a bounded valid UTF-8 origin-relative path")
		}
		if t.StatementKind != "" || t.RelationBinding != "" {
			return contractError(ErrInvalidContract, "transport", "HTTP transport cannot carry a PostgreSQL statement")
		}
	case TransportKindPostgreSQL:
		if !validPostgreSQLStatementKind(t.StatementKind) || !validIdentityPart(t.RelationBinding) ||
			t.Method != "" || t.Path != "" {
			return contractError(ErrInvalidContract, "transport", "PostgreSQL transport requires a typed statement kind and relation binding")
		}
	default:
		return contractError(ErrInvalidContract, "transport.kind", "transport kind is unsupported")
	}
	return nil
}

var allowedHTTPMethods = map[string]struct{}{
	http.MethodGet: {}, http.MethodHead: {}, http.MethodPost: {}, http.MethodPut: {},
	http.MethodPatch: {}, http.MethodDelete: {}, http.MethodOptions: {},
}

func (r Response) Validate(transportKind string) error {
	switch transportKind {
	case TransportKindHTTP:
		if !validHTTPResponseStatus(r.Status) {
			return contractError(ErrInvalidContract, "response.status", "HTTP response status must be default or an exact 100-599 status")
		}
	case TransportKindPostgreSQL:
		if r.Status != "success" {
			return contractError(ErrInvalidContract, "response.status", "native PostgreSQL response status must be success")
		}
	default:
		return contractError(ErrInvalidContract, "response.status", "response transport is unsupported")
	}
	if err := validateSourceRefs(r.SourceRefs, "response.source_refs"); err != nil {
		return err
	}
	if r.NoContent {
		if transportKind == TransportKindPostgreSQL {
			return contractError(ErrInvalidContract, "response.no_content", "native PostgreSQL responses are typed body responses")
		}
		if r.MediaType != "" || r.Schema != nil || r.RecordPointer != nil {
			return contractError(ErrInvalidContract, "response.no_content", "no-content response cannot declare media, schema, or a record pointer")
		}
		return nil
	}
	if err := validateMediaType(r.MediaType); err != nil {
		return err
	}
	if r.Schema == nil {
		return contractError(ErrInvalidContract, "response.schema", "body response requires an explicit schema")
	}
	if err := r.Schema.Validate(); err != nil {
		return err
	}
	if r.Schema.Type != SchemaTypeBinaryRef && !isJSONMediaType(r.MediaType) {
		return contractError(ErrIncompatibleMedia, "response.media_type", "non-binary typed response requires JSON media")
	}
	if r.RecordPointer != nil && (len(*r.RecordPointer) > 4096 || !validJSONPointer(*r.RecordPointer)) {
		return contractError(ErrInvalidContract, "response.record_pointer", "must be a bounded absolute RFC 6901 JSON pointer")
	}
	return nil
}

func validateRequestMediaSelector(operation Operation, common map[string]FieldBinding) error {
	if len(operation.RequestVariants) < 2 {
		if operation.RequestMediaFieldID != "" {
			return contractError(ErrInvalidContract, "request_media_field_id", "is valid only for a request with multiple media variants")
		}
		return nil
	}
	if !validIdentityPart(operation.RequestMediaFieldID) {
		return contractError(ErrInvalidContract, "request_media_field_id", "multiple request media variants require a stable selector field")
	}
	binding, ok := common[operation.RequestMediaFieldID]
	if !ok {
		return contractError(ErrInvalidContract, "request_media_field_id", "must identify a common input binding")
	}
	if binding.Location != FieldLocationHeader || !strings.EqualFold(binding.WireName, RequestMediaTypeWireName) ||
		binding.Required || binding.Nullable || binding.Sensitivity != SensitivityPublic ||
		binding.Schema.Type != SchemaTypeString || binding.SourceDefault != nil {
		return contractError(ErrInvalidContract, "request_media_field_id", "must identify an optional public non-null Content-Type header binding without a default")
	}
	if len(binding.Schema.Enum) != len(operation.RequestVariants) {
		return contractError(ErrInvalidContract, "request_media_field_id", "selector enum must contain every request media variant exactly once")
	}
	declaredMedia := make(map[string]struct{}, len(operation.RequestVariants))
	for _, variant := range operation.RequestVariants {
		declaredMedia[variant.MediaType] = struct{}{}
	}
	for _, candidate := range binding.Schema.Enum {
		mediaType, present := candidate.TextValue()
		if !present {
			return contractError(ErrInvalidContract, "request_media_field_id", "selector enum must contain present string media types")
		}
		if _, declared := declaredMedia[mediaType]; !declared {
			return contractError(ErrInvalidContract, "request_media_field_id", "selector enum contains an undeclared request media variant")
		}
	}
	return nil
}

func validateSourceRefs(references []SourceRef, path string) error {
	if len(references) == 0 {
		return contractError(ErrInvalidContract, path, "must preserve at least one exact source reference")
	}
	seen := make(map[string]struct{}, len(references))
	for _, reference := range references {
		if !validIdentityPart(reference.SourceID) || !validSourcePointer(reference.Pointer) {
			return contractError(ErrInvalidContract, path, "contains an incomplete or malformed source identity")
		}
		key := reference.SourceID + "\x00" + reference.Pointer
		if _, duplicate := seen[key]; duplicate {
			return contractError(ErrDuplicateField, path, "contains a duplicate source reference")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validHTTPResponseStatus(status string) bool {
	if status == "default" {
		return true
	}
	return len(status) == 3 && status[0] >= '1' && status[0] <= '5' &&
		status[1] >= '0' && status[1] <= '9' && status[2] >= '0' && status[2] <= '9'
}

func validPostgreSQLStatementKind(kind PostgreSQLStatementKind) bool {
	switch kind {
	case PostgreSQLStatementMetadata, PostgreSQLStatementSelect, PostgreSQLStatementMappedUpsert:
		return true
	default:
		return false
	}
}

func validLaneStatus(status LaneStatus) bool {
	switch status {
	case LaneEligible, LaneUnsupported, LaneNotApplicable, LaneBlocked, LaneUnproven:
		return true
	default:
		return false
	}
}

var canonicalLanes = [...]Lane{
	LaneDirectRead,
	LaneDirectWrite,
	LaneETL,
	LaneReverseETL,
	LaneBinaryDownload,
	LaneBinaryUpload,
	LaneSyncTransport,
}

func validSourcePointer(value string) bool {
	if value == "" || len(value) > 4096 {
		return false
	}
	if strings.HasPrefix(value, "#") {
		value = strings.TrimPrefix(value, "#")
	}
	return validJSONPointer(value)
}

// ValidateLane validates one canonical execution lane without duplicating the
// lane registry in store or application boundaries.
func ValidateLane(lane Lane) error {
	if !validLane(lane) {
		return contractError(ErrInvalidContract, "lane", "lane is unsupported")
	}
	return nil
}

func validLane(lane Lane) bool {
	switch lane {
	case LaneDirectRead, LaneDirectWrite, LaneETL, LaneReverseETL,
		LaneBinaryDownload, LaneBinaryUpload, LaneSyncTransport:
		return true
	default:
		return false
	}
}
