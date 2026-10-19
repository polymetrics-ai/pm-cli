package contract

import (
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

type Presence string

const (
	PresenceAbsent Presence = "absent"
	PresenceNull   Presence = "null"
	PresenceValue  Presence = "value"
)

type ValueKind string

const (
	ValueKindString           ValueKind = "string"
	ValueKindBool             ValueKind = "bool"
	ValueKindInteger          ValueKind = "integer"
	ValueKindDecimal          ValueKind = "decimal"
	ValueKindDate             ValueKind = "date"
	ValueKindTimestampInstant ValueKind = "timestamp_instant"
	ValueKindTimestampLocal   ValueKind = "timestamp_local"
	ValueKindArray            ValueKind = "array"
	ValueKindObject           ValueKind = "object"
	ValueKindUnion            ValueKind = "union"
	ValueKindBinaryRef        ValueKind = "binary_ref"
)

type ExactNumber struct {
	Text      string `json:"exact"`
	Precision int    `json:"precision"`
	Scale     int    `json:"scale"`
}

func validSHA256(value string) bool {
	if !strings.HasPrefix(value, "sha256:") {
		return false
	}
	return validSHA256Hex(strings.TrimPrefix(value, "sha256:"))
}

func validSHA256Hex(encoded string) bool {
	if len(encoded) != 64 || encoded != strings.ToLower(encoded) {
		return false
	}
	_, err := hex.DecodeString(encoded)
	return err == nil
}

// PresentValue owns its containers; nested values may safely share private storage.
type PresentValue struct {
	presence Presence
	kind     ValueKind
	text     string
	boolean  bool
	number   ExactNumber
	elements []PresentValue
	fields   map[string]PresentValue
	variant  string
	member   *PresentValue
	binary   *ObjectRef
}

func AbsentValue() PresentValue {
	return PresentValue{presence: PresenceAbsent}
}

func NullValue() PresentValue {
	return PresentValue{presence: PresenceNull}
}

func NewStringValue(value string) PresentValue {
	return PresentValue{presence: PresenceValue, kind: ValueKindString, text: value}
}

func NewBooleanValue(value bool) PresentValue {
	return PresentValue{presence: PresenceValue, kind: ValueKindBool, boolean: value}
}

func NewIntegerValue(exact string) (PresentValue, error) {
	precision, ok := integerPrecision(exact)
	if !ok {
		return PresentValue{}, contractError(ErrInvalidValue, "integer", "must be a canonical base-10 integer string")
	}
	return PresentValue{
		presence: PresenceValue,
		kind:     ValueKindInteger,
		number:   ExactNumber{Text: exact, Precision: precision, Scale: 0},
	}, nil
}

func NewDecimalValue(exact string, precision, scale int) (PresentValue, error) {
	actualPrecision, actualScale, ok := decimalShape(exact)
	if !ok || precision != actualPrecision || scale != actualScale {
		return PresentValue{}, contractError(ErrInvalidValue, "decimal", "exact text must match its positive precision and scale")
	}
	return PresentValue{
		presence: PresenceValue,
		kind:     ValueKindDecimal,
		number:   ExactNumber{Text: exact, Precision: precision, Scale: scale},
	}, nil
}

// NewDecimalText builds one exact decimal from its base-10 text alone: every
// digit of the text counts toward its precision and the digits after its point
// are its scale, so "10.20" is precision 4 and scale 2. The text is kept
// exactly; it is never rescaled or normalized.
func NewDecimalText(exact string) (PresentValue, error) {
	precision, scale, ok := decimalShape(exact)
	if !ok {
		return PresentValue{}, contractError(ErrInvalidValue, "decimal", "exact text must match its positive precision and scale")
	}
	return NewDecimalValue(exact, precision, scale)
}

func NewDateValue(value string) (PresentValue, error) {
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil || parsed.Format("2006-01-02") != value {
		return PresentValue{}, contractError(ErrInvalidValue, "date", "must be an exact YYYY-MM-DD calendar date")
	}
	return PresentValue{presence: PresenceValue, kind: ValueKindDate, text: value}, nil
}

func NewInstantValue(value string) (PresentValue, error) {
	if len(value) < 20 || value[10] != 'T' || value[13] != ':' || value[16] != ':' {
		return PresentValue{}, contractError(ErrInvalidValue, "timestamp_instant", "must include a valid RFC3339 offset")
	}
	zone := value[19:]
	fraction := ""
	if zone[0] == '.' {
		end := 1
		for end < len(zone) && zone[end] >= '0' && zone[end] <= '9' {
			end++
		}
		if end == 1 {
			return PresentValue{}, contractError(ErrInvalidValue, "timestamp_instant", "fraction must contain decimal digits")
		}
		fraction = strings.TrimRight(zone[1:end], "0")
		zone = zone[end:]
	}
	if zone != "Z" && (len(zone) != 6 || (zone[0] != '+' && zone[0] != '-') || zone[3] != ':' || zone[1:3] > "23" || zone[4:6] > "59") {
		return PresentValue{}, contractError(ErrInvalidValue, "timestamp_instant", "must include a valid RFC3339 offset")
	}
	// Parse only whole seconds: time.Parse silently truncates longer fractions
	// to nanoseconds, while this typed value retains the exact decimal fraction.
	parsed, err := time.Parse(time.RFC3339, value[:19]+zone)
	if err != nil {
		return PresentValue{}, contractError(ErrInvalidValue, "timestamp_instant", "must include a valid RFC3339 offset")
	}
	utc := parsed.UTC()
	if utc.Year() < 0 || utc.Year() > 9999 {
		return PresentValue{}, contractError(ErrInvalidValue, "timestamp_instant", "normalized UTC year must fit RFC3339")
	}
	normalized := utc.Format("2006-01-02T15:04:05")
	if fraction != "" {
		normalized += "." + fraction
	}
	return PresentValue{presence: PresenceValue, kind: ValueKindTimestampInstant, text: normalized + "Z"}, nil
}

func NewLocalTimestampValue(value string) (PresentValue, error) {
	if strings.ContainsAny(value, "Zz+") || hasNegativeOffset(value) {
		return PresentValue{}, contractError(ErrInvalidValue, "timestamp_local", "must not contain a zone or offset")
	}
	parsed, err := time.Parse("2006-01-02T15:04:05.999999999", value)
	if err != nil || parsed.Format(localTimestampLayout(value)) != value {
		return PresentValue{}, contractError(ErrInvalidValue, "timestamp_local", "must be an exact local wall timestamp")
	}
	return PresentValue{presence: PresenceValue, kind: ValueKindTimestampLocal, text: value}, nil
}

func NewArrayValue(values []PresentValue) PresentValue {
	if values == nil {
		values = []PresentValue{}
	}
	return PresentValue{presence: PresenceValue, kind: ValueKindArray, elements: slices.Clone(values)}
}

func NewObjectValue(values map[string]PresentValue) PresentValue {
	if values == nil {
		values = map[string]PresentValue{}
	}
	return PresentValue{presence: PresenceValue, kind: ValueKindObject, fields: maps.Clone(values)}
}

func NewUnionValue(variant string, value PresentValue) (PresentValue, error) {
	if !validIdentityPart(variant) {
		return PresentValue{}, contractError(ErrInvalidValue, "union.variant", "must be a nonempty UTF-8 stable variant")
	}
	if err := value.Validate(); err != nil {
		return PresentValue{}, err
	}
	return PresentValue{presence: PresenceValue, kind: ValueKindUnion, variant: variant, member: new(value)}, nil
}

func NewBinaryRefValue(reference ObjectRef) (PresentValue, error) {
	if err := reference.Validate(); err != nil {
		return PresentValue{}, err
	}
	return PresentValue{presence: PresenceValue, kind: ValueKindBinaryRef, binary: new(reference)}, nil
}

func (v PresentValue) Presence() Presence {
	return v.presence
}

func (v PresentValue) Kind() ValueKind {
	return v.kind
}

func (v PresentValue) ExactNumber() (ExactNumber, bool) {
	if v.presence != PresenceValue || (v.kind != ValueKindInteger && v.kind != ValueKindDecimal) {
		return ExactNumber{}, false
	}
	return v.number, true
}

func (v PresentValue) TextValue() (string, bool) {
	if v.presence != PresenceValue || v.kind != ValueKindString {
		return "", false
	}
	return v.text, true
}

func (v PresentValue) TemporalText() (string, bool) {
	if v.presence != PresenceValue {
		return "", false
	}
	switch v.kind {
	case ValueKindDate, ValueKindTimestampInstant, ValueKindTimestampLocal:
		return v.text, true
	default:
		return "", false
	}
}

func (v PresentValue) Boolean() (bool, bool) {
	if v.presence != PresenceValue || v.kind != ValueKindBool {
		return false, false
	}
	return v.boolean, true
}

func (v PresentValue) Elements() ([]PresentValue, bool) {
	if v.presence != PresenceValue || v.kind != ValueKindArray {
		return nil, false
	}
	return slices.Clone(v.elements), true
}

func (v PresentValue) Fields() (map[string]PresentValue, bool) {
	if v.presence != PresenceValue || v.kind != ValueKindObject {
		return nil, false
	}
	return maps.Clone(v.fields), true
}

func (v PresentValue) UnionMember() (string, PresentValue, bool) {
	if v.presence != PresenceValue || v.kind != ValueKindUnion || v.member == nil {
		return "", PresentValue{}, false
	}
	return v.variant, *v.member, true
}

func (v PresentValue) ArtifactReference() (ObjectRef, bool) {
	if v.presence != PresenceValue || v.kind != ValueKindBinaryRef || v.binary == nil {
		return ObjectRef{}, false
	}
	return *v.binary, true
}

func (v PresentValue) Validate() error {
	switch v.presence {
	case PresenceAbsent, PresenceNull:
		if v.kind != "" {
			return contractError(ErrInvalidValue, "value", "absent and null values cannot carry a logical type")
		}
		return nil
	case PresenceValue:
	default:
		return contractError(ErrInvalidValue, "value.presence", "must be absent, null, or value")
	}
	switch v.kind {
	case ValueKindString:
		if !utf8.ValidString(v.text) {
			return contractError(ErrInvalidValue, "value.string", "must contain valid UTF-8")
		}
	case ValueKindBool:
		return nil
	case ValueKindInteger:
		precision, ok := integerPrecision(v.number.Text)
		if !ok || v.number.Precision != precision || v.number.Scale != 0 {
			return contractError(ErrInvalidValue, "value.integer", "has inconsistent exact-number metadata")
		}
	case ValueKindDecimal:
		precision, scale, ok := decimalShape(v.number.Text)
		if !ok || v.number.Precision != precision || v.number.Scale != scale {
			return contractError(ErrInvalidValue, "value.decimal", "has inconsistent exact-number metadata")
		}
	case ValueKindDate:
		_, err := NewDateValue(v.text)
		return err
	case ValueKindTimestampInstant:
		normalized, err := NewInstantValue(v.text)
		if err != nil || normalized.text != v.text {
			return contractError(ErrInvalidValue, "value.timestamp_instant", "must be normalized to UTC")
		}
	case ValueKindTimestampLocal:
		_, err := NewLocalTimestampValue(v.text)
		return err
	case ValueKindArray:
		if v.elements == nil {
			return contractError(ErrInvalidValue, "value.elements", "array presence must be explicit")
		}
		for _, element := range v.elements {
			if err := element.Validate(); err != nil {
				return err
			}
		}
	case ValueKindObject:
		if v.fields == nil {
			return contractError(ErrInvalidValue, "value.fields", "object presence must be explicit")
		}
		for name, field := range v.fields {
			if !utf8.ValidString(name) {
				return contractError(ErrInvalidValue, "value.fields", "provider property name must retain exact valid UTF-8")
			}
			if err := field.Validate(); err != nil {
				return err
			}
		}
	case ValueKindUnion:
		if !validIdentityPart(v.variant) || v.member == nil {
			return contractError(ErrInvalidValue, "value.union", "must identify and preserve one variant")
		}
		return v.member.Validate()
	case ValueKindBinaryRef:
		if v.binary == nil {
			return contractError(ErrInvalidValue, "value.binary", "reference is missing")
		}
		return v.binary.Validate()
	default:
		return contractError(ErrInvalidValue, "value.type", "logical type is unsupported")
	}
	return nil
}

func (v PresentValue) Equal(other PresentValue) bool {
	if v.presence != other.presence || v.kind != other.kind || v.text != other.text || v.boolean != other.boolean || v.number != other.number || v.variant != other.variant {
		return false
	}
	if len(v.elements) != len(other.elements) || len(v.fields) != len(other.fields) {
		return false
	}
	for index := range v.elements {
		if !v.elements[index].Equal(other.elements[index]) {
			return false
		}
	}
	for name, field := range v.fields {
		otherField, ok := other.fields[name]
		if !ok || !field.Equal(otherField) {
			return false
		}
	}
	if (v.member == nil) != (other.member == nil) || (v.binary == nil) != (other.binary == nil) {
		return false
	}
	if v.member != nil && !v.member.Equal(*other.member) {
		return false
	}
	if v.binary != nil && *v.binary != *other.binary {
		return false
	}
	return true
}

type presentValueWire struct {
	Presence  Presence                 `json:"presence"`
	Type      ValueKind                `json:"type,omitempty"`
	Text      *string                  `json:"text,omitempty"`
	Boolean   *bool                    `json:"boolean,omitempty"`
	Exact     *string                  `json:"exact,omitempty"`
	Precision *int                     `json:"precision,omitempty"`
	Scale     *int                     `json:"scale,omitempty"`
	Elements  *[]PresentValue          `json:"elements,omitempty"`
	Fields    *map[string]PresentValue `json:"fields,omitempty"`
	Variant   *string                  `json:"variant,omitempty"`
	Member    *PresentValue            `json:"member,omitempty"`
	Binary    *ObjectRef               `json:"binary,omitempty"`
}

func (v PresentValue) MarshalJSON() ([]byte, error) {
	if err := v.Validate(); err != nil {
		return nil, err
	}
	wire := presentValueWire{Presence: v.presence, Type: v.kind}
	switch v.presence {
	case PresenceAbsent, PresenceNull:
		wire.Type = ""
	case PresenceValue:
		switch v.kind {
		case ValueKindString, ValueKindDate, ValueKindTimestampInstant, ValueKindTimestampLocal:
			wire.Text = new(v.text)
		case ValueKindBool:
			wire.Boolean = new(v.boolean)
		case ValueKindInteger, ValueKindDecimal:
			wire.Exact = new(v.number.Text)
			wire.Precision = new(v.number.Precision)
			wire.Scale = new(v.number.Scale)
		case ValueKindArray:
			wire.Elements = new(v.elements)
		case ValueKindObject:
			wire.Fields = new(v.fields)
		case ValueKindUnion:
			wire.Variant = new(v.variant)
			wire.Member = new(*v.member)
		case ValueKindBinaryRef:
			wire.Binary = new(*v.binary)
		}
	}
	return json.Marshal(wire)
}

func (v *PresentValue) UnmarshalJSON(data []byte) error {
	var wire presentValueWire
	if err := decodeStrictJSON(data, &wire); err != nil {
		return err
	}
	mask := wire.payloadMask()
	switch wire.Presence {
	case PresenceAbsent:
		if wire.Type != "" || mask != 0 {
			return contractError(ErrInvalidValue, "value", "absent value carries payload")
		}
		*v = AbsentValue()
		return nil
	case PresenceNull:
		if wire.Type != "" || mask != 0 {
			return contractError(ErrInvalidValue, "value", "null value carries payload")
		}
		*v = NullValue()
		return nil
	case PresenceValue:
	default:
		return contractError(ErrInvalidValue, "value.presence", "must be absent, null, or value")
	}
	var decoded PresentValue
	var err error
	switch wire.Type {
	case ValueKindString:
		if mask != valuePayloadText {
			return contractError(ErrInvalidValue, "value", "string has missing or contradictory payload")
		}
		decoded = NewStringValue(*wire.Text)
	case ValueKindBool:
		if mask != valuePayloadBoolean {
			return contractError(ErrInvalidValue, "value", "bool has missing or contradictory payload")
		}
		decoded = NewBooleanValue(*wire.Boolean)
	case ValueKindInteger:
		if mask != valuePayloadExact|valuePayloadPrecision|valuePayloadScale || *wire.Scale != 0 {
			return contractError(ErrInvalidValue, "value", "integer has missing or contradictory exact metadata")
		}
		decoded, err = NewIntegerValue(*wire.Exact)
		if err == nil && decoded.number.Precision != *wire.Precision {
			err = contractError(ErrInvalidValue, "value.precision", "does not match exact integer")
		}
	case ValueKindDecimal:
		if mask != valuePayloadExact|valuePayloadPrecision|valuePayloadScale {
			return contractError(ErrInvalidValue, "value", "decimal has missing or contradictory exact metadata")
		}
		decoded, err = NewDecimalValue(*wire.Exact, *wire.Precision, *wire.Scale)
	case ValueKindDate:
		if mask != valuePayloadText {
			return contractError(ErrInvalidValue, "value", "date has missing or contradictory payload")
		}
		decoded, err = NewDateValue(*wire.Text)
	case ValueKindTimestampInstant:
		if mask != valuePayloadText {
			return contractError(ErrInvalidValue, "value", "instant has missing or contradictory payload")
		}
		decoded, err = NewInstantValue(*wire.Text)
	case ValueKindTimestampLocal:
		if mask != valuePayloadText {
			return contractError(ErrInvalidValue, "value", "local timestamp has missing or contradictory payload")
		}
		decoded, err = NewLocalTimestampValue(*wire.Text)
	case ValueKindArray:
		if mask != valuePayloadElements {
			return contractError(ErrInvalidValue, "value", "array has missing or contradictory payload")
		}
		decoded = NewArrayValue(*wire.Elements)
	case ValueKindObject:
		if mask != valuePayloadFields {
			return contractError(ErrInvalidValue, "value", "object has missing or contradictory payload")
		}
		decoded = NewObjectValue(*wire.Fields)
	case ValueKindUnion:
		if mask != valuePayloadVariant|valuePayloadMember {
			return contractError(ErrInvalidValue, "value", "union has missing or contradictory payload")
		}
		decoded, err = NewUnionValue(*wire.Variant, *wire.Member)
	case ValueKindBinaryRef:
		if mask != valuePayloadBinary {
			return contractError(ErrInvalidValue, "value", "binary reference has missing or contradictory payload")
		}
		decoded, err = NewBinaryRefValue(*wire.Binary)
	default:
		return contractError(ErrInvalidValue, "value.type", "logical type is unsupported")
	}
	if err != nil {
		return err
	}
	*v = decoded
	return nil
}

const (
	valuePayloadText = 1 << iota
	valuePayloadBoolean
	valuePayloadExact
	valuePayloadPrecision
	valuePayloadScale
	valuePayloadElements
	valuePayloadFields
	valuePayloadVariant
	valuePayloadMember
	valuePayloadBinary
)

func (w presentValueWire) payloadMask() int {
	mask := 0
	if w.Text != nil {
		mask |= valuePayloadText
	}
	if w.Boolean != nil {
		mask |= valuePayloadBoolean
	}
	if w.Exact != nil {
		mask |= valuePayloadExact
	}
	if w.Precision != nil {
		mask |= valuePayloadPrecision
	}
	if w.Scale != nil {
		mask |= valuePayloadScale
	}
	if w.Elements != nil {
		mask |= valuePayloadElements
	}
	if w.Fields != nil {
		mask |= valuePayloadFields
	}
	if w.Variant != nil {
		mask |= valuePayloadVariant
	}
	if w.Member != nil {
		mask |= valuePayloadMember
	}
	if w.Binary != nil {
		mask |= valuePayloadBinary
	}
	return mask
}

func EncodePresentValue(value PresentValue, mediaType string) ([]byte, error) {
	if !isJSONMediaType(mediaType) {
		return nil, contractError(ErrIncompatibleMedia, "media_type", "typed value encoding requires JSON media")
	}
	if err := value.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func DecodePresentValue(encoded []byte, mediaType string) (PresentValue, error) {
	if !isJSONMediaType(mediaType) {
		return PresentValue{}, contractError(ErrIncompatibleMedia, "media_type", "typed value decoding requires JSON media")
	}
	var value PresentValue
	if err := decodeContractJSON(encoded, &value); err != nil {
		return PresentValue{}, err
	}
	if err := value.Validate(); err != nil {
		return PresentValue{}, err
	}
	return value, nil
}

func integerPrecision(value string) (int, bool) {
	if value == "" || value == "-" || value[0] == '+' {
		return 0, false
	}
	digits := value
	if value[0] == '-' {
		digits = value[1:]
	}
	if digits == "" || (len(digits) > 1 && digits[0] == '0') || value == "-0" {
		return 0, false
	}
	for _, character := range digits {
		if character < '0' || character > '9' {
			return 0, false
		}
	}
	return len(digits), true
}

func decimalShape(value string) (int, int, bool) {
	if value == "" || value == "-" || value[0] == '+' {
		return 0, 0, false
	}
	unsigned := value
	if value[0] == '-' {
		unsigned = value[1:]
	}
	parts := strings.Split(unsigned, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || (len(parts[0]) > 1 && parts[0][0] == '0') {
		return 0, 0, false
	}
	allZero := true
	for _, part := range parts {
		for _, character := range part {
			if character < '0' || character > '9' {
				return 0, 0, false
			}
			if character != '0' {
				allZero = false
			}
		}
	}
	if strings.HasPrefix(value, "-") && allZero {
		return 0, 0, false
	}
	return len(parts[0]) + len(parts[1]), len(parts[1]), true
}

func hasNegativeOffset(value string) bool {
	if len(value) <= len("2006-01-02T15:04:05") {
		return false
	}
	return strings.Contains(value[len("2006-01-02T15:04:05"):], "-")
}

func localTimestampLayout(value string) string {
	if strings.Contains(value, ".") {
		fraction := len(value) - strings.IndexByte(value, '.') - 1
		return "2006-01-02T15:04:05." + strings.Repeat("0", fraction)
	}
	return "2006-01-02T15:04:05"
}

func valueMatchesSchema(value PresentValue, schema Schema) bool {
	return operationInputMatchesSchema(value, schema, true)
}

func kindMatchesSchema(kind ValueKind, schemaType SchemaType) bool {
	return string(kind) == string(schemaType)
}

type ResultPagination struct {
	// EffectiveWireLimit is nil when no provider page-size field was sent.
	EffectiveWireLimit *uint64        `json:"effective_wire_limit"`
	TotalRecordCap     *uint64        `json:"total_record_cap,omitempty"`
	Continuation       PresentValue   `json:"continuation"`
	UnreadTail         []PresentValue `json:"unread_tail"`
	FetchedCount       uint64         `json:"fetched_count"`
	ReturnedCount      uint64         `json:"returned_count"`
	// EmittedCount counts records in this action result after caller filtering.
	EmittedCount    uint64       `json:"emitted_count"`
	ProviderTotal   *ExactNumber `json:"provider_total,omitempty"`
	SourceExhausted bool         `json:"source_exhausted"`
}

func (p ResultPagination) Validate() error {
	if p.EffectiveWireLimit != nil && *p.EffectiveWireLimit == 0 {
		return contractError(ErrInvalidValue, "pagination.effective_wire_limit", "must be positive when present")
	}
	if p.TotalRecordCap != nil && *p.TotalRecordCap == 0 {
		return contractError(ErrInvalidValue, "pagination.total_record_cap", "must be positive when present")
	}
	if err := p.Continuation.Validate(); err != nil {
		return err
	}
	if p.UnreadTail == nil {
		return contractError(ErrInvalidValue, "pagination.unread_tail", "must be explicitly present, even when empty")
	}
	for _, value := range p.UnreadTail {
		if err := value.Validate(); err != nil {
			return err
		}
	}
	if p.ReturnedCount > p.FetchedCount || p.EmittedCount > p.ReturnedCount {
		return contractError(ErrInvalidValue, "pagination.counts", "must satisfy fetched >= returned >= emitted")
	}
	if p.TotalRecordCap != nil && p.EmittedCount > *p.TotalRecordCap {
		return contractError(ErrInvalidValue, "pagination.emitted_count", "exceeds the caller total record cap")
	}
	if p.ProviderTotal != nil {
		precision, ok := integerPrecision(p.ProviderTotal.Text)
		if !ok || p.ProviderTotal.Precision != precision || p.ProviderTotal.Scale != 0 {
			return contractError(ErrInvalidValue, "pagination.provider_total", "must be an exact nonnegative integer total")
		}
		if strings.HasPrefix(p.ProviderTotal.Text, "-") {
			return contractError(ErrInvalidValue, "pagination.provider_total", "must not be negative")
		}
	}
	if p.SourceExhausted && (p.Continuation.Presence() == PresenceValue || len(p.UnreadTail) != 0) {
		return contractError(ErrInvalidValue, "pagination", "exhausted source cannot retain continuation or unread tail")
	}
	return nil
}

func (p ResultPagination) clone() ResultPagination {
	copy := p
	copy.UnreadTail = slices.Clone(p.UnreadTail)
	if p.EffectiveWireLimit != nil {
		copy.EffectiveWireLimit = new(*p.EffectiveWireLimit)
	}
	if p.TotalRecordCap != nil {
		copy.TotalRecordCap = new(*p.TotalRecordCap)
	}
	if p.ProviderTotal != nil {
		copy.ProviderTotal = new(*p.ProviderTotal)
	}
	return copy
}

type ActionResult struct {
	MediaType  string            `json:"media_type,omitempty"`
	Data       PresentValue      `json:"data"`
	Pagination *ResultPagination `json:"pagination,omitempty"`
}

type FieldPath []string

type FieldSelection struct {
	Paths []FieldPath `json:"paths"`
}

type FilterOperator string

const (
	FilterEqual    FilterOperator = "equal"
	FilterIsAbsent FilterOperator = "is_absent"
	FilterIsNull   FilterOperator = "is_null"
)

type RecordFilter struct {
	Path     FieldPath      `json:"path"`
	Operator FilterOperator `json:"operator"`
	Value    *PresentValue  `json:"value,omitempty"`
}

const maxResultContinuationBytes = 4096

type ResultControls struct {
	Selection      *FieldSelection `json:"selection,omitempty"`
	Filter         *RecordFilter   `json:"filter,omitempty"`
	TotalRecordCap *uint64         `json:"total_record_cap,omitempty"`
	Continuation   *PresentValue   `json:"continuation,omitempty"`
}

func (r ActionResult) Validate() error {
	if err := r.Data.Validate(); err != nil {
		return err
	}
	if r.Data.Presence() == PresenceAbsent {
		if r.MediaType != "" {
			return contractError(ErrIncompatibleMedia, "action_result.media_type", "must be omitted when action data is absent")
		}
		if r.Pagination != nil {
			return contractError(ErrInvalidValue, "action_result.pagination", "must be omitted when action data is absent")
		}
		return nil
	}
	if err := validateMediaType(r.MediaType); err != nil {
		return err
	}
	if r.Data.Presence() == PresenceValue && r.Data.Kind() == ValueKindBinaryRef {
		reference, _ := r.Data.ArtifactReference()
		if r.MediaType != reference.MediaType {
			return contractError(ErrIncompatibleMedia, "action_result.media_type", "must match the registered binary media")
		}
	} else if !isJSONMediaType(r.MediaType) {
		return contractError(ErrIncompatibleMedia, "action_result.media_type", "non-binary typed action result requires JSON media")
	}
	if r.Pagination != nil {
		return r.Pagination.Validate()
	}
	return nil
}

func cloneResultPagination(value *ResultPagination) *ResultPagination {
	if value == nil {
		return nil
	}
	return new(value.clone())
}

// HasContinuation reports whether the caller supplied a present continuation
// value. Validate must still be called before the continuation is consumed.
func (c ResultControls) HasContinuation() bool {
	return c.ContinuationValue().Presence() == PresenceValue
}

// ContinuationValue returns explicit typed continuation state and maps an
// omitted continuation control to the shared absent value.
func (c ResultControls) ContinuationValue() PresentValue {
	if c.Continuation == nil {
		return AbsentValue()
	}
	return *c.Continuation
}

func (c ResultControls) Validate() error {
	if c.Selection != nil {
		if len(c.Selection.Paths) == 0 {
			return contractError(ErrInvalidControls, "selection.paths", "explicit selection must contain at least one path")
		}
		for index, path := range c.Selection.Paths {
			if err := validateFieldPath(path); err != nil {
				return err
			}
			for prior := range index {
				if pathsOverlap(c.Selection.Paths[prior], path) {
					return contractError(ErrInvalidControls, "selection.paths", "paths are duplicate or overlapping")
				}
			}
		}
	}
	if c.Filter != nil {
		if err := validateFieldPath(c.Filter.Path); err != nil {
			return err
		}
		switch c.Filter.Operator {
		case FilterEqual:
			if c.Filter.Value == nil {
				return contractError(ErrInvalidControls, "filter.value", "equal requires a declared null or present typed value")
			}
			if err := c.Filter.Value.Validate(); err != nil || c.Filter.Value.Presence() == PresenceAbsent {
				return contractError(ErrInvalidControls, "filter.value", "equal requires a declared null or present typed value")
			}
		case FilterIsAbsent, FilterIsNull:
			if c.Filter.Value != nil {
				return contractError(ErrInvalidControls, "filter.value", "presence operators do not accept a comparison value")
			}
		default:
			return contractError(ErrInvalidControls, "filter.operator", "operator is unsupported")
		}
	}
	if c.TotalRecordCap != nil && *c.TotalRecordCap == 0 {
		return contractError(ErrInvalidControls, "total_record_cap", "must be positive when supplied")
	}
	if c.Continuation != nil {
		continuation := c.ContinuationValue()
		if err := continuation.Validate(); err != nil {
			return contractError(ErrInvalidControls, "continuation", "must be absent or a bounded nonempty opaque string")
		}
		switch continuation.Presence() {
		case PresenceAbsent:
		case PresenceValue:
			text, ok := continuation.TextValue()
			if !ok || text == "" || len(text) > maxResultContinuationBytes {
				return contractError(ErrInvalidControls, "continuation", "must be absent or a bounded nonempty opaque string")
			}
		default:
			return contractError(ErrInvalidControls, "continuation", "must be absent or a bounded nonempty opaque string")
		}
	}
	return nil
}

func ApplyResultControls(result ActionResult, controls ResultControls) (ActionResult, error) {
	if err := result.Validate(); err != nil {
		return ActionResult{}, err
	}
	if err := controls.Validate(); err != nil {
		return ActionResult{}, err
	}
	if controls.Selection == nil && controls.Filter == nil {
		return ActionResult{MediaType: result.MediaType, Data: result.Data, Pagination: cloneResultPagination(result.Pagination)}, nil
	}
	if result.Data.presence != PresenceValue || result.Data.kind != ValueKindArray {
		return ActionResult{}, contractError(ErrInvalidControls, "action_result.data", "selection and filtering require an array of object records")
	}
	records := result.Data.elements
	output := make([]PresentValue, 0, len(records))
	for _, record := range records {
		if record.Kind() != ValueKindObject || record.Presence() != PresenceValue {
			return ActionResult{}, contractError(ErrInvalidControls, "action_result.data", "every controlled record must be an object")
		}
		matches, err := recordMatchesFilter(record, controls.Filter)
		if err != nil {
			return ActionResult{}, err
		}
		if !matches {
			continue
		}
		if controls.Selection == nil {
			output = append(output, record)
			continue
		}
		output = append(output, selectRecordFields(record, controls.Selection.Paths))
	}
	pagination := cloneResultPagination(result.Pagination)
	if pagination != nil {
		pagination.EmittedCount = uint64(len(output))
	}
	data := PresentValue{presence: PresenceValue, kind: ValueKindArray, elements: output}
	return ActionResult{MediaType: result.MediaType, Data: data, Pagination: pagination}, nil
}

func validateFieldPath(path FieldPath) error {
	if len(path) == 0 {
		return contractError(ErrInvalidControls, "field_path", "must contain at least one segment")
	}
	for _, segment := range path {
		if !utf8.ValidString(segment) {
			return contractError(ErrInvalidControls, "field_path", "segments must retain exact valid UTF-8 provider property names")
		}
	}
	return nil
}

func pathsOverlap(left, right FieldPath) bool {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for index := range limit {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func lookupFieldPath(value PresentValue, path FieldPath) (PresentValue, bool) {
	current := value
	for _, segment := range path {
		if current.presence != PresenceValue || current.kind != ValueKindObject {
			return PresentValue{}, false
		}
		next, ok := current.fields[segment]
		if !ok || next.presence == PresenceAbsent {
			return PresentValue{}, false
		}
		current = next
	}
	return current, true
}

func recordMatchesFilter(record PresentValue, filter *RecordFilter) (bool, error) {
	if filter == nil {
		return true, nil
	}
	value, found := lookupFieldPath(record, filter.Path)
	switch filter.Operator {
	case FilterIsAbsent:
		return !found, nil
	case FilterIsNull:
		return found && value.Presence() == PresenceNull, nil
	case FilterEqual:
		return found && value.Equal(*filter.Value), nil
	default:
		return false, contractError(ErrInvalidControls, "filter.operator", "operator is unsupported")
	}
}

func selectRecordFields(record PresentValue, paths []FieldPath) PresentValue {
	selected := make(map[string]PresentValue)
	for _, path := range paths {
		value, found := lookupFieldPath(record, path)
		if !found {
			continue
		}
		insertSelectedValue(selected, path, value)
	}
	return PresentValue{presence: PresenceValue, kind: ValueKindObject, fields: selected}
}

func insertSelectedValue(target map[string]PresentValue, path FieldPath, value PresentValue) {
	if len(path) == 1 {
		target[path[0]] = value
		return
	}
	child, ok := target[path[0]]
	if !ok {
		child = NewObjectValue(map[string]PresentValue{})
	}
	insertSelectedValue(child.fields, path[1:], value)
	target[path[0]] = child
}
