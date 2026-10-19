package analytics

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// ErrInvalidValue identifies a typed analytical value that cannot be projected
// exactly. It is never repaired by coercion: a decimal with an unexpected
// scale, an integer that does not fit, or a temporal text outside the declared
// calendar is refused instead.
var ErrInvalidValue = errors.New("invalid analytical value")

// Kind is one exact analytical value class. The vocabulary is closed: a value
// that does not fit one of these kinds is refused rather than widened.
type Kind string

const (
	KindInt64       Kind = "int64"
	KindDecimal     Kind = "decimal"
	KindText        Kind = "text"
	KindBoolean     Kind = "boolean"
	KindBytea       Kind = "bytea"
	KindDate        Kind = "date"
	KindTimestamp   Kind = "timestamp"
	KindTimestampTZ Kind = "timestamptz"
	KindUUID        Kind = "uuid"
	KindJSON        Kind = "json"
)

// Valid reports whether the kind belongs to the closed vocabulary.
func (kind Kind) Valid() bool {
	switch kind {
	case KindInt64, KindDecimal, KindText, KindBoolean, KindBytea,
		KindDate, KindTimestamp, KindTimestampTZ, KindUUID, KindJSON:
		return true
	default:
		return false
	}
}

// MaximumDecimalDigits is DuckDB's DECIMAL width. A decimal that needs more
// digits cannot be represented exactly and is refused.
const MaximumDecimalDigits = 38

// MaximumValueBytes bounds one text, bytea, uuid, or JSON payload.
const MaximumValueBytes = 1 << 20

// Value is one exactly typed analytical value. Presence is explicit: an absent
// value is the zero Value, an explicit SQL NULL is NullValue, and every other
// value carries exactly one payload. A present value is never silently
// converted to another kind.
type Value struct {
	present  bool
	kind     Kind
	null     bool
	integer  int64
	text     string
	unscaled string
	scale    int
	boolean  bool
	binary   []byte
}

// AbsentValue is a member that was not supplied at all.
func AbsentValue() Value { return Value{} }

// NullValue is an explicit SQL NULL.
func NullValue() Value { return Value{present: true, kind: "", null: true} }

func Int64Value(value int64) Value {
	return Value{present: true, kind: KindInt64, integer: value}
}

// DecimalValue carries an exact unscaled integer and a non-negative declared
// scale. No float ever participates.
func DecimalValue(unscaled string, scale int) (Value, error) {
	if err := validateUnscaled(unscaled); err != nil {
		return Value{}, err
	}
	if scale < 0 || scale > MaximumDecimalDigits {
		return Value{}, fmt.Errorf("%w: decimal scale %d is outside 0..%d", ErrInvalidValue, scale, MaximumDecimalDigits)
	}
	digits := strings.TrimLeft(strings.TrimPrefix(unscaled, "-"), "0")
	if len(digits)+scale > MaximumDecimalDigits {
		return Value{}, fmt.Errorf("%w: decimal needs more than %d digits", ErrInvalidValue, MaximumDecimalDigits)
	}
	return Value{present: true, kind: KindDecimal, unscaled: unscaled, scale: scale}, nil
}

func TextValue(text string) (Value, error) {
	if !utf8.ValidString(text) {
		return Value{}, fmt.Errorf("%w: text is not valid UTF-8", ErrInvalidValue)
	}
	if len(text) > MaximumValueBytes {
		return Value{}, fmt.Errorf("%w: text exceeds the per-value byte bound", ErrInvalidValue)
	}
	return Value{present: true, kind: KindText, text: text}, nil
}

func BooleanValue(value bool) Value {
	return Value{present: true, kind: KindBoolean, boolean: value}
}

func BytesValue(data []byte) (Value, error) {
	if len(data) > MaximumValueBytes {
		return Value{}, fmt.Errorf("%w: bytea exceeds the per-value byte bound", ErrInvalidValue)
	}
	return Value{present: true, kind: KindBytea, binary: append([]byte(nil), data...)}, nil
}

// DateValue carries one calendar date in the canonical YYYY-MM-DD form. A date
// the calendar cannot express exactly is refused, not shifted.
func DateValue(value string) (Value, error) {
	if err := validateCalendarDate(value); err != nil {
		return Value{}, err
	}
	return Value{present: true, kind: KindDate, text: value}, nil
}

// TimestampValue is a timestamp without time zone: exact microseconds since
// 1970-01-01T00:00:00, matching the engine's own representation.
func TimestampValue(microseconds int64) Value {
	return Value{present: true, kind: KindTimestamp, integer: microseconds}
}

// TimestampTZValue is a timestamp with time zone: the exact instant in
// microseconds since the Unix epoch, independent of any session offset.
func TimestampTZValue(microseconds int64) Value {
	return Value{present: true, kind: KindTimestampTZ, integer: microseconds}
}

// UUIDValue carries one canonical lowercase UUID.
func UUIDValue(value string) (Value, error) {
	if err := validateCanonicalUUID(value); err != nil {
		return Value{}, err
	}
	return Value{present: true, kind: KindUUID, text: value}, nil
}

// JSONValue carries one document's exact JSON text.
func JSONValue(document string) (Value, error) {
	if len(document) > MaximumValueBytes {
		return Value{}, fmt.Errorf("%w: JSON exceeds the per-value byte bound", ErrInvalidValue)
	}
	if !json.Valid([]byte(document)) {
		return Value{}, fmt.Errorf("%w: JSON text is not a valid document", ErrInvalidValue)
	}
	return Value{present: true, kind: KindJSON, text: document}, nil
}

// Present reports whether the value was supplied at all.
func (value Value) Present() bool { return value.present }

// Null reports an explicit SQL NULL, which is distinct from an absent member.
func (value Value) Null() bool { return value.null }

// Kind reports the exact class of a present value; an explicit NULL has no
// payload kind.
func (value Value) Kind() Kind {
	if !value.present || value.null {
		return ""
	}
	return value.kind
}

func (value Value) Int64() (int64, bool) {
	if value.Kind() != KindInt64 && value.Kind() != KindTimestamp && value.Kind() != KindTimestampTZ {
		return 0, false
	}
	return value.integer, true
}

func (value Value) Unscaled() (string, int, bool) {
	if value.Kind() != KindDecimal {
		return "", 0, false
	}
	return value.unscaled, value.scale, true
}

func (value Value) Text() (string, bool) {
	switch value.Kind() {
	case KindText, KindDate, KindUUID, KindJSON:
		return value.text, true
	default:
		return "", false
	}
}

func (value Value) Boolean() (bool, bool) {
	if value.Kind() != KindBoolean {
		return false, false
	}
	return value.boolean, true
}

func (value Value) Bytes() ([]byte, bool) {
	if value.Kind() != KindBytea {
		return nil, false
	}
	return value.binary, true
}

// Validate rechecks a constructed value. It refuses a value that is neither
// absent, nor an explicit null, nor a valid payload of a known kind.
func (value Value) Validate() error {
	if !value.present {
		return nil
	}
	if value.null {
		if value.kind != "" {
			return fmt.Errorf("%w: an explicit null carries a payload kind", ErrInvalidValue)
		}
		return nil
	}
	switch value.kind {
	case KindInt64:
		return nil
	case KindDecimal:
		_, err := DecimalValue(value.unscaled, value.scale)
		return err
	case KindText:
		_, err := TextValue(value.text)
		return err
	case KindBoolean:
		return nil
	case KindBytea:
		_, err := BytesValue(value.binary)
		return err
	case KindDate:
		return validateCalendarDate(value.text)
	case KindTimestamp, KindTimestampTZ:
		return nil
	case KindUUID:
		return validateCanonicalUUID(value.text)
	case KindJSON:
		_, err := JSONValue(value.text)
		return err
	default:
		return fmt.Errorf("%w: the value kind is unsupported", ErrInvalidValue)
	}
}

// String is a bounded description for diagnostics. It never renders a full
// payload: text, bytea, and JSON values are reported by length.
func (value Value) String() string {
	switch {
	case !value.present:
		return "absent"
	case value.null:
		return "null"
	}
	switch value.kind {
	case KindInt64, KindTimestamp, KindTimestampTZ:
		return fmt.Sprintf("%s(%d)", value.kind, value.integer)
	case KindDecimal:
		return fmt.Sprintf("decimal(%s,%d)", value.unscaled, value.scale)
	case KindBoolean:
		return fmt.Sprintf("boolean(%t)", value.boolean)
	case KindBytea:
		return fmt.Sprintf("bytea(%d bytes)", len(value.binary))
	case KindText, KindDate, KindUUID, KindJSON:
		return fmt.Sprintf("%s(%d bytes)", value.kind, len(value.text))
	default:
		return "invalid"
	}
}

const (
	valueMemberKind     = "kind"
	valueMemberValue    = "value"
	valueMemberUnscaled = "unscaled"
	valueMemberScale    = "scale"
	valueMemberBase64   = "base64"
	valueMemberMicros   = "micros"
)

// EncodeAnalyticValue renders one value into the protocol's exact wire form.
// Every integral payload travels as a decimal string so a JSON number can
// never lose precision, and an explicit null is written as its own member
// rather than omitted.
func EncodeAnalyticValue(value Value) (json.RawMessage, error) {
	if err := value.Validate(); err != nil {
		return nil, err
	}
	var wire map[string]any
	switch {
	case !value.present:
		return nil, fmt.Errorf("%w: an absent value has no wire form", ErrInvalidValue)
	case value.null:
		wire = map[string]any{valueMemberKind: "null"}
	default:
		switch value.kind {
		case KindInt64:
			wire = map[string]any{valueMemberKind: string(KindInt64), valueMemberValue: strconv.FormatInt(value.integer, 10)}
		case KindDecimal:
			wire = map[string]any{
				valueMemberKind:     string(KindDecimal),
				valueMemberUnscaled: value.unscaled,
				valueMemberScale:    strconv.Itoa(value.scale),
			}
		case KindText:
			wire = map[string]any{valueMemberKind: string(KindText), valueMemberValue: value.text}
		case KindBoolean:
			wire = map[string]any{valueMemberKind: string(KindBoolean), valueMemberValue: value.boolean}
		case KindBytea:
			wire = map[string]any{valueMemberKind: string(KindBytea), valueMemberBase64: base64.StdEncoding.EncodeToString(value.binary)}
		case KindDate:
			wire = map[string]any{valueMemberKind: string(KindDate), valueMemberValue: value.text}
		case KindTimestamp:
			wire = map[string]any{valueMemberKind: string(KindTimestamp), valueMemberMicros: strconv.FormatInt(value.integer, 10)}
		case KindTimestampTZ:
			wire = map[string]any{valueMemberKind: string(KindTimestampTZ), valueMemberMicros: strconv.FormatInt(value.integer, 10)}
		case KindUUID:
			wire = map[string]any{valueMemberKind: string(KindUUID), valueMemberValue: value.text}
		case KindJSON:
			wire = map[string]any{valueMemberKind: string(KindJSON), valueMemberValue: value.text}
		default:
			return nil, fmt.Errorf("%w: the value kind is unsupported", ErrInvalidValue)
		}
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("encode analytical value: %w", err)
	}
	return encoded, nil
}

// valueWire is the closed member set of one encoded value. Contradictory or
// missing payload members are refused after decoding, and contract's strict
// decoder refuses unknown, duplicated, null, or wrongly cased members.
type valueWire struct {
	Kind     string          `json:"kind"`
	Value    json.RawMessage `json:"value,omitempty"`
	Unscaled *string         `json:"unscaled,omitempty"`
	Scale    *string         `json:"scale,omitempty"`
	Base64   *string         `json:"base64,omitempty"`
	Micros   *string         `json:"micros,omitempty"`
}

// DecodeAnalyticValue decodes exactly one encoded value. Presence is explicit:
// a missing payload member is absent, while a `null` kind is an explicit SQL
// NULL.
func DecodeAnalyticValue(document []byte) (Value, error) {
	var wire valueWire
	if err := contract.DecodeStrictRequiredObject(document, &wire, valueMemberKind); err != nil {
		return Value{}, fmt.Errorf("%w: %s", ErrInvalidValue, err.Error())
	}
	switch wire.Kind {
	case "null":
		if wire.Value != nil || wire.Unscaled != nil || wire.Scale != nil || wire.Base64 != nil || wire.Micros != nil {
			return Value{}, fmt.Errorf("%w: an explicit null carries a payload", ErrInvalidValue)
		}
		return NullValue(), nil
	case string(KindInt64):
		text, err := exactStringMember(wire, valueMemberValue)
		if err != nil {
			return Value{}, err
		}
		parsed, err := strconv.ParseInt(text, 10, 64)
		if err != nil || strconv.FormatInt(parsed, 10) != text {
			return Value{}, fmt.Errorf("%w: int64 must be a canonical base-10 integer string", ErrInvalidValue)
		}
		return Int64Value(parsed), nil
	case string(KindDecimal):
		unscaled, scaleText, err := exactDecimalMembers(wire)
		if err != nil {
			return Value{}, err
		}
		scale, err := strconv.Atoi(scaleText)
		if err != nil || strconv.Itoa(scale) != scaleText {
			return Value{}, fmt.Errorf("%w: decimal scale must be a canonical integer string", ErrInvalidValue)
		}
		return DecimalValue(unscaled, scale)
	case string(KindText):
		text, err := exactStringMember(wire, valueMemberValue)
		if err != nil {
			return Value{}, err
		}
		return TextValue(text)
	case string(KindBoolean):
		if wire.Value == nil || wire.Unscaled != nil || wire.Scale != nil || wire.Base64 != nil || wire.Micros != nil {
			return Value{}, fmt.Errorf("%w: boolean requires exactly one boolean value member", ErrInvalidValue)
		}
		var boolean bool
		if err := json.Unmarshal(wire.Value, &boolean); err != nil {
			return Value{}, fmt.Errorf("%w: boolean must be a JSON boolean", ErrInvalidValue)
		}
		return BooleanValue(boolean), nil
	case string(KindBytea):
		if wire.Base64 == nil || wire.Value != nil || wire.Unscaled != nil || wire.Scale != nil || wire.Micros != nil {
			return Value{}, fmt.Errorf("%w: bytea requires exactly one base64 member", ErrInvalidValue)
		}
		decoded, err := base64.StdEncoding.DecodeString(*wire.Base64)
		if err != nil || base64.StdEncoding.EncodeToString(decoded) != *wire.Base64 {
			return Value{}, fmt.Errorf("%w: bytea must be canonical standard base64", ErrInvalidValue)
		}
		return BytesValue(decoded)
	case string(KindDate):
		text, err := exactStringMember(wire, valueMemberValue)
		if err != nil {
			return Value{}, err
		}
		return DateValue(text)
	case string(KindTimestamp), string(KindTimestampTZ):
		micros, err := exactStringMember(wire, valueMemberMicros)
		if err != nil {
			return Value{}, err
		}
		parsed, err := strconv.ParseInt(micros, 10, 64)
		if err != nil || strconv.FormatInt(parsed, 10) != micros {
			return Value{}, fmt.Errorf("%w: timestamp micros must be a canonical base-10 integer string", ErrInvalidValue)
		}
		if wire.Kind == string(KindTimestamp) {
			return TimestampValue(parsed), nil
		}
		return TimestampTZValue(parsed), nil
	case string(KindUUID):
		text, err := exactStringMember(wire, valueMemberValue)
		if err != nil {
			return Value{}, err
		}
		return UUIDValue(text)
	case string(KindJSON):
		text, err := exactStringMember(wire, valueMemberValue)
		if err != nil {
			return Value{}, err
		}
		return JSONValue(text)
	default:
		return Value{}, fmt.Errorf("%w: the value kind is unsupported", ErrInvalidValue)
	}
}

// MarshalJSON renders the value through the exact wire encoding.
func (value Value) MarshalJSON() ([]byte, error) { return EncodeAnalyticValue(value) }

// UnmarshalJSON decodes the exact wire encoding; a JSON null is not a value and
// is refused here (an explicit SQL NULL travels as its own kind).
func (value *Value) UnmarshalJSON(document []byte) error {
	decoded, err := DecodeAnalyticValue(document)
	if err != nil {
		return err
	}
	*value = decoded
	return nil
}

// payloadMembers reports which payload members the decoded value carries.
func (wire valueWire) payloadMembers() map[string]bool {
	return map[string]bool{
		valueMemberValue:    wire.Value != nil,
		valueMemberUnscaled: wire.Unscaled != nil,
		valueMemberScale:    wire.Scale != nil,
		valueMemberBase64:   wire.Base64 != nil,
		valueMemberMicros:   wire.Micros != nil,
	}
}

// exactStringMember requires exactly one string payload member and refuses a
// missing or contradictory one, so no payload is ever ignored.
func exactStringMember(wire valueWire, wanted string) (string, error) {
	present := wire.payloadMembers()
	if !present[wanted] {
		return "", fmt.Errorf("%w: the %s member is absent", ErrInvalidValue, wanted)
	}
	for name, isPresent := range present {
		if name != wanted && isPresent {
			return "", fmt.Errorf("%w: contradictory payload members", ErrInvalidValue)
		}
	}
	switch wanted {
	case valueMemberValue:
		var text string
		if err := json.Unmarshal(wire.Value, &text); err != nil {
			return "", fmt.Errorf("%w: the %s member must be a JSON string", ErrInvalidValue, wanted)
		}
		return text, nil
	case valueMemberUnscaled:
		return *wire.Unscaled, nil
	case valueMemberScale:
		return *wire.Scale, nil
	case valueMemberBase64:
		return *wire.Base64, nil
	case valueMemberMicros:
		return *wire.Micros, nil
	default:
		return "", fmt.Errorf("%w: the %s member is unsupported", ErrInvalidValue, wanted)
	}
}

func exactDecimalMembers(wire valueWire) (string, string, error) {
	if wire.Unscaled == nil || wire.Scale == nil || wire.Value != nil || wire.Base64 != nil || wire.Micros != nil {
		return "", "", fmt.Errorf("%w: decimal requires exactly one unscaled and one scale member", ErrInvalidValue)
	}
	return *wire.Unscaled, *wire.Scale, nil
}

func validateUnscaled(unscaled string) error {
	digits := unscaled
	if strings.HasPrefix(digits, "-") {
		digits = digits[1:]
	}
	if digits == "" || len(digits) > MaximumDecimalDigits {
		return fmt.Errorf("%w: decimal unscaled digits are absent or too wide", ErrInvalidValue)
	}
	for _, character := range digits {
		if character < '0' || character > '9' {
			return fmt.Errorf("%w: decimal unscaled digits must be base-10", ErrInvalidValue)
		}
	}
	if digits == "0" {
		if strings.HasPrefix(unscaled, "-") {
			return fmt.Errorf("%w: decimal zero must not carry a sign", ErrInvalidValue)
		}
		return nil
	}
	if strings.HasPrefix(digits, "0") {
		return fmt.Errorf("%w: decimal unscaled digits must not be zero-padded", ErrInvalidValue)
	}
	return nil
}

func validateCalendarDate(value string) error {
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil || parsed.Format("2006-01-02") != value {
		return fmt.Errorf("%w: date must be an exact YYYY-MM-DD calendar date", ErrInvalidValue)
	}
	return nil
}

func validateCanonicalUUID(value string) error {
	if len(value) != 36 {
		return fmt.Errorf("%w: UUID must be a canonical 36-character value", ErrInvalidValue)
	}
	for index, character := range value {
		switch index {
		case 8, 13, 18, 23:
			if character != '-' {
				return fmt.Errorf("%w: UUID must be a canonical lowercase value", ErrInvalidValue)
			}
		default:
			if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
				return fmt.Errorf("%w: UUID must be a canonical lowercase value", ErrInvalidValue)
			}
		}
	}
	return nil
}

// decimalText renders the exact base-10 text of an unscaled integer and scale.
// It is used when a decimal must be presented to the engine as text.
func decimalText(unscaled string, scale int) (string, error) {
	if err := validateUnscaled(unscaled); err != nil {
		return "", err
	}
	if scale == 0 {
		return unscaled, nil
	}
	negative := strings.HasPrefix(unscaled, "-")
	digits := strings.TrimPrefix(unscaled, "-")
	if len(digits) <= scale {
		digits = strings.Repeat("0", scale-len(digits)+1) + digits
	}
	text := digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
	if negative {
		text = "-" + text
	}
	return text, nil
}

// parseDecimalText converts exact engine text back into an unscaled integer and
// the declared scale. Text whose scale differs from the declaration is refused
// rather than rescaled.
func parseDecimalText(text string, scale int) (string, error) {
	negative := strings.HasPrefix(text, "-")
	digits := strings.TrimPrefix(text, "-")
	whole, fraction, found := strings.Cut(digits, ".")
	if !found {
		fraction = ""
		if scale != 0 {
			return "", fmt.Errorf("%w: decimal text %q has no declared scale", ErrInvalidValue, text)
		}
	}
	if len(fraction) != scale {
		return "", fmt.Errorf("%w: decimal text %q does not carry the declared scale %d", ErrInvalidValue, text, scale)
	}
	for _, component := range []string{whole, fraction} {
		for _, character := range component {
			if character < '0' || character > '9' {
				return "", fmt.Errorf("%w: decimal text %q is not base-10", ErrInvalidValue, text)
			}
		}
	}
	trimmed := strings.TrimLeft(whole+fraction, "0")
	if trimmed == "" {
		return "0", nil
	}
	if negative {
		trimmed = "-" + trimmed
	}
	if err := validateUnscaled(trimmed); err != nil {
		return "", err
	}
	return trimmed, nil
}

func hexEncode(data []byte) string { return strings.ToUpper(hex.EncodeToString(data)) }

func hexDecode(text string) ([]byte, error) {
	if text == "" {
		return []byte{}, nil
	}
	decoded, err := hex.DecodeString(text)
	if err != nil {
		return nil, fmt.Errorf("%w: bytea text is not hexadecimal", ErrInvalidValue)
	}
	if hexEncode(decoded) != strings.ToUpper(text) {
		return nil, fmt.Errorf("%w: bytea text is not canonical hexadecimal", ErrInvalidValue)
	}
	return decoded, nil
}
