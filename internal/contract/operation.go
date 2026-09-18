package contract

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"unicode"
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

type OperationExecution struct {
	HTTP       *OperationHTTP       `json:"http,omitempty"`
	Pagination *OperationPagination `json:"pagination,omitempty"`
	Retry      *OperationRetry      `json:"retry,omitempty"`
}

func (e *OperationExecution) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := decodeContractJSON(data, &fields); err != nil {
		return err
	}
	for name, raw := range fields {
		switch name {
		case "http", "pagination", "retry":
			if strings.TrimSpace(string(raw)) == "null" {
				return contractError(ErrInvalidContract, "execution."+name, "explicit null cannot stand in for an absent execution fact")
			}
		default:
			return contractError(ErrInvalidContract, "execution."+name, "field is unsupported")
		}
	}
	type wire OperationExecution
	var decoded wire
	if err := decodeStrictJSON(data, &decoded); err != nil {
		return err
	}
	*e = OperationExecution(decoded)
	return nil
}

type OperationHTTP struct {
	Origins       []string                `json:"origins"`
	Authorization *OperationAuthorization `json:"authorization,omitempty"`
	Headers       []OperationHTTPHeader   `json:"headers,omitempty"`
	Witness       ExecutionWitness        `json:"witness"`
}

func (h *OperationHTTP) UnmarshalJSON(data []byte) error {
	type wire OperationHTTP
	var decoded wire
	values, err := decodeRequiredObjectFields(data, &decoded, "origins", "witness")
	if err != nil {
		return err
	}
	if len(decoded.Origins) == 0 {
		return contractError(ErrInvalidContract, "execution.http.origins", "must preserve at least one source-admitted HTTP origin")
	}
	if _, present := values["headers"]; present && len(decoded.Headers) == 0 {
		return contractError(ErrInvalidContract, "execution.http.headers", "explicit empty headers cannot stand in for absent fixed headers")
	}
	*h = OperationHTTP(decoded)
	return nil
}

type OperationHTTPHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (h *OperationHTTPHeader) UnmarshalJSON(data []byte) error {
	type wire OperationHTTPHeader
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "name", "value"); err != nil {
		return err
	}
	*h = OperationHTTPHeader(decoded)
	return nil
}

type OperationAuthorization struct {
	Required  bool          `json:"required"`
	Audience  string        `json:"audience"`
	Placement FieldLocation `json:"placement"`
	WireName  string        `json:"wire_name"`
	Prefix    string        `json:"prefix"`
	FieldID   string        `json:"field_id"`
}

func (a *OperationAuthorization) UnmarshalJSON(data []byte) error {
	type wire OperationAuthorization
	var decoded wire
	if err := decodeRequiredObject(data, &decoded,
		"required", "audience", "placement", "wire_name", "prefix", "field_id",
	); err != nil {
		return err
	}
	*a = OperationAuthorization(decoded)
	return nil
}

type ExecutionWitness struct {
	ProofID    string      `json:"proof_id"`
	SourceRefs []SourceRef `json:"source_refs"`
}

type OperationPaginationKind string

const (
	OperationPaginationCursor OperationPaginationKind = "cursor"
	OperationPaginationOffset OperationPaginationKind = "offset"
	OperationPaginationPage   OperationPaginationKind = "page"
	OperationPaginationLink   OperationPaginationKind = "link"
)

type OperationPagination struct {
	Kind                    OperationPaginationKind `json:"kind"`
	CursorRequestField      string                  `json:"cursor_request_field,omitempty"`
	CursorResponsePointer   string                  `json:"cursor_response_pointer,omitempty"`
	OffsetRequestField      string                  `json:"offset_request_field,omitempty"`
	LimitRequestField       string                  `json:"limit_request_field,omitempty"`
	TotalResponsePointer    string                  `json:"total_response_pointer,omitempty"`
	PageRequestField        string                  `json:"page_request_field,omitempty"`
	LastPageResponsePointer string                  `json:"last_page_response_pointer,omitempty"`
	LinkRequestField        string                  `json:"link_request_field,omitempty"`
	LinkResponseHeader      string                  `json:"link_response_header,omitempty"`
	Witness                 ExecutionWitness        `json:"witness"`
}

func (p *OperationPagination) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := decodeContractJSON(data, &fields); err != nil {
		return err
	}
	type wire OperationPagination
	var decoded wire
	if err := decodeStrictJSON(data, &decoded); err != nil {
		return err
	}
	allowed := map[string]struct{}{"kind": {}, "witness": {}}
	switch decoded.Kind {
	case OperationPaginationCursor:
		allowed["cursor_request_field"] = struct{}{}
		allowed["cursor_response_pointer"] = struct{}{}
	case OperationPaginationOffset:
		allowed["offset_request_field"] = struct{}{}
		allowed["limit_request_field"] = struct{}{}
		allowed["total_response_pointer"] = struct{}{}
		if raw, present := fields["total_response_pointer"]; present && strings.TrimSpace(string(raw)) == `""` {
			return contractError(ErrInvalidContract, "execution.pagination.total_response_pointer", "explicit empty pointer cannot stand in for an absent response fact")
		}
	case OperationPaginationPage:
		allowed["page_request_field"] = struct{}{}
		allowed["last_page_response_pointer"] = struct{}{}
		if raw, present := fields["last_page_response_pointer"]; present && strings.TrimSpace(string(raw)) == `""` {
			return contractError(ErrInvalidContract, "execution.pagination.last_page_response_pointer", "explicit empty pointer cannot stand in for an absent response fact")
		}
	case OperationPaginationLink:
		allowed["link_request_field"] = struct{}{}
		allowed["link_response_header"] = struct{}{}
		allowed["limit_request_field"] = struct{}{}
		if raw, present := fields["limit_request_field"]; present && strings.TrimSpace(string(raw)) == `""` {
			return contractError(ErrInvalidContract, "execution.pagination.limit_request_field", "explicit empty field cannot stand in for an absent limit fact")
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
			return contractError(ErrInvalidContract, "execution.pagination."+name, "field contradicts the selected pagination kind")
		}
	}
	*p = OperationPagination(decoded)
	return nil
}

func (r *OperationRetry) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := decodeContractJSON(data, &fields); err != nil {
		return err
	}
	type wire OperationRetry
	var decoded wire
	if err := decodeStrictJSON(data, &decoded); err != nil {
		return err
	}
	_, conditionPresent := fields["condition"]
	if decoded.Classification == OperationRetryConditional {
		if !conditionPresent {
			return contractError(ErrInvalidContract, "execution.retry.condition", "conditional retry classification requires an explicit condition")
		}
	} else if conditionPresent {
		return contractError(ErrInvalidContract, "execution.retry.condition", "unconditional retry classification cannot carry a condition field")
	}
	if _, statusesPresent := fields["http_statuses"]; statusesPresent && len(decoded.HTTPStatuses) == 0 {
		return contractError(ErrInvalidContract, "execution.retry.http_statuses", "explicit empty HTTP statuses cannot stand in for absent status facts")
	}
	*r = OperationRetry(decoded)
	return nil
}

type OperationRetryClassification string

const (
	OperationRetryUnsafe      OperationRetryClassification = "unsafe"
	OperationRetrySafe        OperationRetryClassification = "safe"
	OperationRetryConditional OperationRetryClassification = "conditional"
)

type OperationRetry struct {
	Classification OperationRetryClassification `json:"classification"`
	Condition      string                       `json:"condition,omitempty"`
	HTTPStatuses   []uint16                     `json:"http_statuses,omitempty"`
	Witness        ExecutionWitness             `json:"witness"`
}

func (e OperationExecution) Validate(operation Operation) error {
	if operation.Transport.Kind != TransportKindHTTP {
		return contractError(ErrInvalidContract, "execution", "execution semantics are only supported for HTTP operations")
	}
	if e.HTTP == nil && e.Pagination == nil && e.Retry == nil {
		return contractError(ErrInvalidContract, "execution", "must preserve at least one admitted execution fact")
	}
	if e.HTTP != nil {
		if err := e.HTTP.Validate(operation); err != nil {
			return err
		}
	}
	if e.Pagination != nil {
		if err := e.Pagination.Validate(operation); err != nil {
			return err
		}
	}
	if e.Retry != nil {
		if err := e.Retry.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (h OperationHTTP) Validate(operation Operation) error {
	if err := h.Witness.validate("execution.http.witness"); err != nil {
		return err
	}
	if len(h.Origins) == 0 {
		return contractError(ErrInvalidContract, "execution.http.origins", "must preserve at least one source-admitted HTTP origin")
	}
	seenOrigins := make(map[string]struct{}, len(h.Origins))
	originAudiences := make(map[string]struct{}, len(h.Origins))
	for _, origin := range h.Origins {
		identity, audience, err := validateHTTPOrigin(origin)
		if err != nil {
			return err
		}
		if _, duplicate := seenOrigins[identity]; duplicate {
			return contractError(ErrDuplicateField, "execution.http.origins", "HTTP origin is duplicated")
		}
		seenOrigins[identity] = struct{}{}
		originAudiences[audience] = struct{}{}
	}

	seenHeaders := make(map[string]struct{}, len(h.Headers))
	for _, header := range h.Headers {
		if !validHTTPToken(header.Name) || !validHTTPHeaderValue(header.Value) || unsafeFixedHTTPHeader(header.Name) {
			return contractError(ErrInvalidContract, "execution.http.headers", "fixed header name or value is unsafe or unsupported")
		}
		key := strings.ToLower(header.Name)
		if _, duplicate := seenHeaders[key]; duplicate {
			return contractError(ErrDuplicateField, "execution.http.headers", "fixed header name is duplicated")
		}
		seenHeaders[key] = struct{}{}
		for _, input := range operation.Inputs {
			if input.Location == FieldLocationHeader && strings.EqualFold(input.WireName, header.Name) {
				return contractError(ErrInvalidContract, "execution.http.headers", "fixed header collides with an operation input")
			}
		}
	}

	if h.Authorization == nil {
		return nil
	}
	authorization := *h.Authorization
	if !validIdentityPart(authorization.FieldID) || !validHTTPToken(authorization.WireName) ||
		!validHTTPHeaderValue(authorization.Prefix) || len(authorization.Prefix) > 1024 {
		return contractError(ErrInvalidContract, "execution.http.authorization", "authorization field identity, wire name, or prefix is invalid")
	}
	switch authorization.Placement {
	case FieldLocationHeader, FieldLocationQuery, FieldLocationCookie:
	default:
		return contractError(ErrInvalidContract, "execution.http.authorization.placement", "authorization placement is unsupported")
	}
	audience, err := validateHTTPAudience(authorization.Audience)
	if err != nil {
		return err
	}
	if len(originAudiences) != 1 {
		return contractError(ErrInvalidContract, "execution.http.authorization.audience", "authorization cannot span multiple HTTP audiences")
	}
	if _, admitted := originAudiences[audience]; !admitted {
		return contractError(ErrInvalidContract, "execution.http.authorization.audience", "authorization audience is not admitted by the source origins")
	}
	if authorization.Placement == FieldLocationHeader {
		if unsafeAuthorizationHTTPHeader(authorization.WireName) {
			return contractError(ErrInvalidContract, "execution.http.authorization.wire_name", "authorization header is transport-controlled or unsupported")
		}
		if _, collision := seenHeaders[strings.ToLower(authorization.WireName)]; collision {
			return contractError(ErrInvalidContract, "execution.http.authorization.wire_name", "authorization collides with a fixed header")
		}
	}
	var credential *FieldBinding
	for index := range operation.Inputs {
		if operation.Inputs[index].FieldID == authorization.FieldID {
			credential = &operation.Inputs[index]
			break
		}
	}
	if credential == nil || credential.Location != authorization.Placement ||
		!strings.EqualFold(credential.WireName, authorization.WireName) ||
		credential.Required != authorization.Required || credential.Nullable ||
		credential.Sensitivity != SensitivitySecret || credential.Schema.Type != SchemaTypeString ||
		credential.SourceDefault != nil {
		return contractError(ErrInvalidContract, "execution.http.authorization.field_id", "must identify the matching source-backed secret string input")
	}
	return nil
}

func validateHTTPOrigin(value string) (string, string, error) {
	if value == "" || len(value) > 4096 || !utf8.ValidString(value) ||
		strings.ContainsAny(value, "{}\r\n\t ") {
		return "", "", contractError(ErrInvalidContract, "execution.http.origins", "HTTP origin must be a bounded absolute non-templated URL")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Opaque != "" || parsed.User != nil || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", "", contractError(ErrInvalidContract, "execution.http.origins", "HTTP origin must be an absolute http or https base URL without credentials, query, or fragment")
	}
	if parsed.Path != "" && !strings.HasPrefix(parsed.Path, "/") {
		return "", "", contractError(ErrInvalidContract, "execution.http.origins", "HTTP origin path must be origin-relative")
	}
	for _, character := range parsed.Path {
		if unicode.IsControl(character) {
			return "", "", contractError(ErrInvalidContract, "execution.http.origins", "HTTP origin path contains a control character")
		}
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Host)
	audience := scheme + "://" + host
	return audience + parsed.EscapedPath(), audience, nil
}

func validateHTTPAudience(value string) (string, error) {
	identity, audience, err := validateHTTPOrigin(value)
	if err != nil {
		return "", contractError(ErrInvalidContract, "execution.http.authorization.audience", "authorization audience must be an absolute source-admitted HTTP origin")
	}
	if identity != audience {
		return "", contractError(ErrInvalidContract, "execution.http.authorization.audience", "authorization audience cannot contain a base path")
	}
	return audience, nil
}

func validHTTPToken(value string) bool {
	if value == "" || len(value) > 1024 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("!#$%&'*+-.^_`|~", character) {
			continue
		}
		return false
	}
	return true
}

func validHTTPHeaderValue(value string) bool {
	if len(value) > 8192 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character == '\t' {
			continue
		}
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func unsafeFixedHTTPHeader(name string) bool {
	var compact strings.Builder
	for _, character := range strings.ToLower(name) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			compact.WriteRune(character)
		}
	}
	for _, fragment := range []string{
		"apikey", "authorization", "authentication", "token",
		"credential", "password", "privatekey", "secret",
	} {
		if strings.Contains(compact.String(), fragment) {
			return true
		}
	}
	switch strings.ToLower(name) {
	case "authorization", "proxy-authorization", "cookie", "set-cookie",
		"host", "content-length", "content-type", "transfer-encoding",
		"connection", "proxy-connection", "upgrade", "trailer", "te",
		"user-agent", "accept-encoding", "expect", "range", "idempotency-key":
		return true
	default:
		return false
	}
}

func unsafeAuthorizationHTTPHeader(name string) bool {
	switch strings.ToLower(name) {
	case "proxy-authorization", "cookie", "set-cookie",
		"host", "content-length", "content-type", "transfer-encoding",
		"connection", "proxy-connection", "upgrade", "trailer", "te",
		"user-agent", "accept-encoding", "expect", "range", "idempotency-key":
		return true
	default:
		return false
	}
}

func (p OperationPagination) Validate(operation Operation) error {
	if err := p.Witness.validate("execution.pagination.witness"); err != nil {
		return err
	}
	if !operationHasPaginatedRecords(operation) {
		return contractError(ErrInvalidContract, "execution.pagination", "must identify an array response or a response record pointer that addresses an array")
	}
	switch p.Kind {
	case OperationPaginationCursor:
		if !validIdentityPart(p.CursorRequestField) || p.CursorResponsePointer == "" ||
			len(p.CursorResponsePointer) > 4096 || !validJSONPointer(p.CursorResponsePointer) ||
			p.OffsetRequestField != "" || p.LimitRequestField != "" || p.TotalResponsePointer != "" ||
			p.PageRequestField != "" || p.LastPageResponsePointer != "" ||
			p.LinkRequestField != "" || p.LinkResponseHeader != "" {
			return contractError(ErrInvalidContract, "execution.pagination", "cursor fields are incomplete or contradictory")
		}
		cursor, ok := paginationCursorBinding(operation, p.CursorRequestField)
		if !ok || (cursor.Schema.Type != SchemaTypeString && cursor.Schema.Type != SchemaTypeInteger) {
			return contractError(ErrInvalidContract, "execution.pagination.cursor_request_field", "must identify an unambiguous string or integer query or request-body binding")
		}
		if !responsePointerHasType(operation, p.CursorResponsePointer, cursor.Schema.Type) {
			return contractError(ErrInvalidContract, "execution.pagination.cursor_response_pointer", "must identify a response value compatible with the cursor request binding")
		}
	case OperationPaginationOffset:
		if !validIdentityPart(p.OffsetRequestField) || !validIdentityPart(p.LimitRequestField) ||
			p.OffsetRequestField == p.LimitRequestField ||
			(p.TotalResponsePointer != "" && (len(p.TotalResponsePointer) > 4096 || !validJSONPointer(p.TotalResponsePointer))) ||
			p.CursorRequestField != "" || p.CursorResponsePointer != "" ||
			p.PageRequestField != "" || p.LastPageResponsePointer != "" ||
			p.LinkRequestField != "" || p.LinkResponseHeader != "" {
			return contractError(ErrInvalidContract, "execution.pagination", "offset fields are incomplete or contradictory")
		}
		if !paginationQueryBindingHasType(operation, p.OffsetRequestField, SchemaTypeInteger) ||
			!paginationQueryBindingHasType(operation, p.LimitRequestField, SchemaTypeInteger) {
			return contractError(ErrInvalidContract, "execution.pagination", "offset and limit fields must identify distinct integer query bindings")
		}
		if p.TotalResponsePointer != "" && !responsePointerHasType(operation, p.TotalResponsePointer, SchemaTypeInteger) {
			return contractError(ErrInvalidContract, "execution.pagination.total_response_pointer", "must identify an integer response value")
		}
	case OperationPaginationPage:
		if !validIdentityPart(p.PageRequestField) ||
			(p.LastPageResponsePointer != "" && (len(p.LastPageResponsePointer) > 4096 || !validJSONPointer(p.LastPageResponsePointer))) ||
			p.CursorRequestField != "" || p.CursorResponsePointer != "" ||
			p.OffsetRequestField != "" || p.LimitRequestField != "" || p.TotalResponsePointer != "" ||
			p.LinkRequestField != "" || p.LinkResponseHeader != "" {
			return contractError(ErrInvalidContract, "execution.pagination", "page fields are incomplete or contradictory")
		}
		if !paginationQueryBindingHasType(operation, p.PageRequestField, SchemaTypeInteger) {
			return contractError(ErrInvalidContract, "execution.pagination.page_request_field", "must identify an integer query binding")
		}
		if p.LastPageResponsePointer != "" && !responsePointerHasType(operation, p.LastPageResponsePointer, SchemaTypeBool) {
			return contractError(ErrInvalidContract, "execution.pagination.last_page_response_pointer", "must identify a boolean response value")
		}
	case OperationPaginationLink:
		if !validIdentityPart(p.LinkRequestField) || !strings.EqualFold(p.LinkResponseHeader, "Link") ||
			p.CursorRequestField != "" || p.CursorResponsePointer != "" ||
			p.OffsetRequestField != "" || p.TotalResponsePointer != "" ||
			p.PageRequestField != "" || p.LastPageResponsePointer != "" {
			return contractError(ErrInvalidContract, "execution.pagination", "link fields are incomplete or contradictory")
		}
		link, ok := paginationQueryBinding(operation, p.LinkRequestField)
		if !ok || (link.Schema.Type != SchemaTypeString && link.Schema.Type != SchemaTypeInteger) {
			return contractError(ErrInvalidContract, "execution.pagination.link_request_field", "must identify a string or integer query binding")
		}
		if p.LimitRequestField != "" {
			if p.LimitRequestField == p.LinkRequestField ||
				!paginationQueryBindingHasType(operation, p.LimitRequestField, SchemaTypeInteger) {
				return contractError(ErrInvalidContract, "execution.pagination.limit_request_field", "must identify a distinct integer query binding")
			}
		}
	default:
		return contractError(ErrInvalidContract, "execution.pagination.kind", "pagination kind is unsupported")
	}
	return nil
}

func (r OperationRetry) Validate() error {
	if err := r.Witness.validate("execution.retry.witness"); err != nil {
		return err
	}
	switch r.Classification {
	case OperationRetryUnsafe, OperationRetrySafe:
		if r.Condition != "" {
			return contractError(ErrInvalidContract, "execution.retry.condition", "unconditional retry classification cannot carry a condition")
		}
	case OperationRetryConditional:
		if !validBoundedIdentityPart(r.Condition, 1024) {
			return contractError(ErrInvalidContract, "execution.retry.condition", "conditional retry classification requires an explicit bounded condition")
		}
	default:
		return contractError(ErrInvalidContract, "execution.retry.classification", "retry classification is unsupported")
	}
	var previous uint16
	for index, status := range r.HTTPStatuses {
		if status < 400 || status > 599 {
			return contractError(ErrInvalidContract, "execution.retry.http_statuses", "retry HTTP status must be between 400 and 599")
		}
		if index != 0 && status <= previous {
			if status == previous {
				return contractError(ErrDuplicateField, "execution.retry.http_statuses", "retry HTTP status is duplicated")
			}
			return contractError(ErrInvalidContract, "execution.retry.http_statuses", "retry HTTP statuses must be sorted")
		}
		previous = status
	}
	return nil
}

func (w ExecutionWitness) validate(path string) error {
	if !validBoundedIdentityPart(w.ProofID, 1024) {
		return contractError(ErrInvalidContract, path+".proof_id", "must preserve a bounded proof identity")
	}
	return validateSourceRefs(w.SourceRefs, path+".source_refs")
}

func paginationQueryBinding(operation Operation, fieldID string) (FieldBinding, bool) {
	for _, binding := range operation.Inputs {
		if binding.FieldID == fieldID && binding.Location == FieldLocationQuery {
			return binding, true
		}
	}
	return FieldBinding{}, false
}

func paginationCursorBinding(operation Operation, fieldID string) (FieldBinding, bool) {
	if binding, ok := paginationQueryBinding(operation, fieldID); ok {
		return binding, true
	}
	var matched *FieldBinding
	for variantIndex := range operation.RequestVariants {
		for bindingIndex := range operation.RequestVariants[variantIndex].Inputs {
			binding := &operation.RequestVariants[variantIndex].Inputs[bindingIndex]
			if binding.FieldID != fieldID || binding.Location != FieldLocationBody {
				continue
			}
			if matched != nil {
				return FieldBinding{}, false
			}
			matched = binding
		}
	}
	if matched == nil {
		return FieldBinding{}, false
	}
	return *matched, true
}

func paginationQueryBindingHasType(operation Operation, fieldID string, schemaType SchemaType) bool {
	binding, ok := paginationQueryBinding(operation, fieldID)
	return ok && binding.Schema.Type == schemaType
}

func responsePointerHasType(operation Operation, pointer string, schemaType SchemaType) bool {
	parts := decodeJSONPointerParts(pointer)
	for _, response := range operation.Responses {
		if !responseHasPaginatedRecords(response) {
			continue
		}
		schema, ok := requestSchemaAtPointer(*response.Schema, parts)
		if ok && schema.Type == schemaType {
			return true
		}
	}
	return false
}

func operationHasPaginatedRecords(operation Operation) bool {
	for _, response := range operation.Responses {
		if responseHasPaginatedRecords(response) {
			return true
		}
	}
	return false
}

func responseHasPaginatedRecords(response Response) bool {
	if response.NoContent || response.Schema == nil {
		return false
	}
	if response.RecordPointer == nil {
		return response.Schema.Type == SchemaTypeArray
	}
	if len(*response.RecordPointer) > 4096 || !validJSONPointer(*response.RecordPointer) {
		return false
	}
	schema, ok := requestSchemaAtPointer(*response.Schema, decodeJSONPointerParts(*response.RecordPointer))
	return ok && schema.Type == SchemaTypeArray
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
	Execution              *OperationExecution     `json:"execution,omitempty"`
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
	if raw, ok := fields["execution"]; ok && strings.TrimSpace(string(raw)) == "null" {
		return Operation{}, contractError(ErrInvalidContract, "execution", "explicit null cannot stand in for unknown execution facts")
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
	if o.Execution != nil {
		if err := o.Execution.Validate(o); err != nil {
			return err
		}
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
