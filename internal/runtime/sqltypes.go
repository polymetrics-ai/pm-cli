package runtime

import (
	"bytes"
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

var (
	ErrSQLValue       = errors.New("invalid PostgreSQL typed value")
	ErrSQLIdentifier  = errors.New("invalid PostgreSQL identifier")
	ErrSQLStatement   = errors.New("invalid PostgreSQL statement")
	ErrSQLRelation    = errors.New("invalid PostgreSQL relation binding")
	ErrUnsupportedSQL = errors.New("unsupported PostgreSQL transport behavior")
)

const (
	// maximumSQLIdentifierBytes is PostgreSQL's NAMEDATALEN-1 for one
	// identifier part.
	maximumSQLIdentifierBytes = 63
	// maximumSQLByteaBytes bounds one bound bytea parameter.
	maximumSQLByteaBytes = 1 << 30
	// maximumSQLTextBytes bounds one bound text parameter.
	maximumSQLTextBytes = 1 << 30
	// maximumSQLNumericPrecision is PostgreSQL's numeric precision ceiling.
	maximumSQLNumericPrecision = 1000
)

// SQLValueKind is one declared PostgreSQL column type. Only these types are
// transportable; any other column type is refused rather than coerced.
type SQLValueKind string

const (
	SQLText        SQLValueKind = "text"
	SQLInt8        SQLValueKind = "int8"
	SQLNumeric     SQLValueKind = "numeric"
	SQLBoolean     SQLValueKind = "boolean"
	SQLBytea       SQLValueKind = "bytea"
	SQLUUID        SQLValueKind = "uuid"
	SQLJSON        SQLValueKind = "json"
	SQLJSONB       SQLValueKind = "jsonb"
	SQLTimestamptz SQLValueKind = "timestamptz"
	SQLTimestamp   SQLValueKind = "timestamp"
)

// sqlTypeName is the exact PostgreSQL type name used in parameter casts and in
// live metadata comparison for one declared column type.
func (kind SQLValueKind) sqlTypeName(precision, scale int) (string, error) {
	switch kind {
	case SQLText:
		if precision != 0 || scale != 0 {
			return "", fmt.Errorf("%w: %s cannot declare precision or scale", ErrSQLValue, kind)
		}
		return "text", nil
	case SQLInt8:
		if precision != 0 || scale != 0 {
			return "", fmt.Errorf("%w: %s cannot declare precision or scale", ErrSQLValue, kind)
		}
		return "bigint", nil
	case SQLNumeric:
		if precision < 1 || precision > maximumSQLNumericPrecision || scale < 0 || scale > precision {
			return "", fmt.Errorf("%w: numeric requires a precision within 1..%d and a scale within 0..precision", ErrSQLValue, maximumSQLNumericPrecision)
		}
		return fmt.Sprintf("numeric(%d,%d)", precision, scale), nil
	case SQLBoolean:
		if precision != 0 || scale != 0 {
			return "", fmt.Errorf("%w: %s cannot declare precision or scale", ErrSQLValue, kind)
		}
		return "boolean", nil
	case SQLBytea:
		if precision != 0 || scale != 0 {
			return "", fmt.Errorf("%w: %s cannot declare precision or scale", ErrSQLValue, kind)
		}
		return "bytea", nil
	case SQLUUID:
		if precision != 0 || scale != 0 {
			return "", fmt.Errorf("%w: %s cannot declare precision or scale", ErrSQLValue, kind)
		}
		return "uuid", nil
	case SQLJSON:
		if precision != 0 || scale != 0 {
			return "", fmt.Errorf("%w: %s cannot declare precision or scale", ErrSQLValue, kind)
		}
		return "json", nil
	case SQLJSONB:
		if precision != 0 || scale != 0 {
			return "", fmt.Errorf("%w: %s cannot declare precision or scale", ErrSQLValue, kind)
		}
		return "jsonb", nil
	case SQLTimestamptz:
		if precision != 0 || scale != 0 {
			return "", fmt.Errorf("%w: %s cannot declare precision or scale", ErrSQLValue, kind)
		}
		return "timestamp with time zone", nil
	case SQLTimestamp:
		if precision != 0 || scale != 0 {
			return "", fmt.Errorf("%w: %s cannot declare precision or scale", ErrSQLValue, kind)
		}
		return "timestamp without time zone", nil
	default:
		return "", fmt.Errorf("%w: column type %q is unsupported", ErrUnsupportedSQL, kind)
	}
}

// SQLValue is one exact typed value. Absent (no value at all) and explicit
// typed null are distinct and stay distinct through binding and decoding.
type SQLValue struct {
	kind      SQLValueKind
	presence  contract.Presence
	text      string
	raw       []byte
	boolean   bool
	precision int
	scale     int
}

func SQLTextValue(text string) SQLValue {
	return SQLValue{kind: SQLText, presence: contract.PresenceValue, text: text}
}

func SQLBooleanValue(value bool) SQLValue {
	return SQLValue{kind: SQLBoolean, presence: contract.PresenceValue, boolean: value}
}

// SQLByteaValue owns the supplied bytes; the caller may reuse its slice.
func SQLByteaValue(raw []byte) SQLValue {
	return SQLValue{kind: SQLBytea, presence: contract.PresenceValue, raw: append([]byte(nil), raw...)}
}

func SQLInt8Value(exact string) (SQLValue, error) {
	if err := validateSQLInteger(exact); err != nil {
		return SQLValue{}, err
	}
	return SQLValue{kind: SQLInt8, presence: contract.PresenceValue, text: exact}, nil
}

// SQLNumericValue preserves the declared scale exactly: the text must already
// carry the declared number of fraction digits, so 12345.6700 can never
// silently become 12345.67.
func SQLNumericValue(exact string, precision, scale int) (SQLValue, error) {
	if _, err := SQLNumeric.sqlTypeName(precision, scale); err != nil {
		return SQLValue{}, err
	}
	integerDigits, fractionDigits, ok := sqlDecimalShape(exact)
	if !ok || fractionDigits != scale || integerDigits > precision-scale {
		return SQLValue{}, fmt.Errorf("%w: numeric(%d,%d) text must carry exactly the declared scale", ErrSQLValue, precision, scale)
	}
	return SQLValue{kind: SQLNumeric, presence: contract.PresenceValue, text: exact, precision: precision, scale: scale}, nil
}

func SQLUUIDValue(exact string) (SQLValue, error) {
	canonical, err := canonicalSQLUUID(exact)
	if err != nil {
		return SQLValue{}, err
	}
	return SQLValue{kind: SQLUUID, presence: contract.PresenceValue, text: canonical}, nil
}

// SQLJSONValue keeps the exact JSON text. jsonb is canonicalized by
// PostgreSQL itself, so its text is whatever the server returned.
func SQLJSONValue(kind SQLValueKind, exact string) (SQLValue, error) {
	if kind != SQLJSON && kind != SQLJSONB {
		return SQLValue{}, fmt.Errorf("%w: %q is not a JSON column type", ErrSQLValue, kind)
	}
	if !json.Valid([]byte(exact)) {
		return SQLValue{}, fmt.Errorf("%w: %s text is not valid JSON", ErrSQLValue, kind)
	}
	return SQLValue{kind: kind, presence: contract.PresenceValue, text: exact}, nil
}

// SQLInstantValue requires an explicit RFC 3339 offset. A timestamptz value is
// an instant; a naive local time is not accepted as one.
func SQLInstantValue(exact string) (SQLValue, error) {
	if _, _, err := parseSQLInstantText(exact); err != nil {
		return SQLValue{}, err
	}
	return SQLValue{kind: SQLTimestamptz, presence: contract.PresenceValue, text: exact}, nil
}

// SQLLocalTimestampValue is a local wall clock with no timezone or offset, and
// is never converted through one.
func SQLLocalTimestampValue(exact string) (SQLValue, error) {
	if _, err := time.Parse("2006-01-02 15:04:05.999999", exact); err != nil {
		return SQLValue{}, fmt.Errorf("%w: timestamp must be an exact local wall clock", ErrSQLValue)
	}
	if err := requireSQLMicrosecondFraction(exact[len("2006-01-02 15:04:05"):]); err != nil {
		return SQLValue{}, err
	}
	return SQLValue{kind: SQLTimestamp, presence: contract.PresenceValue, text: exact}, nil
}

// SQLNullValue is an explicit typed null for the declared column type.
func SQLNullValue(kind SQLValueKind) SQLValue {
	return SQLValue{kind: kind, presence: contract.PresenceNull}
}

// SQLAbsentValue is no value at all. On a mapped write the declared column
// default applies; a null never does.
func SQLAbsentValue(kind SQLValueKind) SQLValue {
	return SQLValue{kind: kind, presence: contract.PresenceAbsent}
}

func (value SQLValue) Kind() SQLValueKind { return value.kind }

func (value SQLValue) IsAbsent() bool { return value.presence == contract.PresenceAbsent }

func (value SQLValue) IsNull() bool { return value.presence == contract.PresenceNull }

// Text returns the exact retained text of a text-shaped value, including the
// exact digits of an integer or numeric value. A null or absent value has no
// text.
func (value SQLValue) Text() (string, bool) {
	if value.presence != contract.PresenceValue {
		return "", false
	}
	switch value.kind {
	case SQLText, SQLUUID, SQLJSON, SQLJSONB, SQLTimestamp, SQLTimestamptz, SQLInt8, SQLNumeric:
		return value.text, true
	default:
		return "", false
	}
}

// Bytes returns a copy of an exact bytea value.
func (value SQLValue) Bytes() ([]byte, bool) {
	if value.presence != contract.PresenceValue || value.kind != SQLBytea {
		return nil, false
	}
	return append([]byte(nil), value.raw...), true
}

func (value SQLValue) Boolean() (bool, bool) {
	if value.presence != contract.PresenceValue || value.kind != SQLBoolean {
		return false, false
	}
	return value.boolean, true
}

// Number returns the exact integer or numeric value including its declared
// precision and scale.
func (value SQLValue) Number() (contract.ExactNumber, bool) {
	if value.presence != contract.PresenceValue {
		return contract.ExactNumber{}, false
	}
	switch value.kind {
	case SQLInt8:
		precision, _, ok := sqlDecimalShape(value.text)
		if !ok {
			return contract.ExactNumber{}, false
		}
		return contract.ExactNumber{Text: value.text, Precision: precision, Scale: 0}, true
	case SQLNumeric:
		return contract.ExactNumber{Text: value.text, Precision: value.precision, Scale: value.scale}, true
	default:
		return contract.ExactNumber{}, false
	}
}

// Instant returns the exact instant of a timestamptz value.
func (value SQLValue) Instant() (time.Time, bool) {
	if value.presence != contract.PresenceValue || value.kind != SQLTimestamptz {
		return time.Time{}, false
	}
	instant, _, err := parseSQLInstantText(value.text)
	if err != nil {
		return time.Time{}, false
	}
	return instant, true
}

func (value SQLValue) Validate() error {
	if !value.kind.supported() {
		return fmt.Errorf("%w: column type %q is unsupported", ErrUnsupportedSQL, value.kind)
	}
	if _, err := value.kind.sqlTypeName(value.precision, value.scale); err != nil && value.presence == contract.PresenceValue {
		return err
	}
	switch value.presence {
	case contract.PresenceAbsent, contract.PresenceNull:
		return nil
	case contract.PresenceValue:
	default:
		return fmt.Errorf("%w: value presence is unsupported", ErrSQLValue)
	}
	switch value.kind {
	case SQLText:
		return validateSQLText(value.text)
	case SQLInt8:
		return validateSQLInteger(value.text)
	case SQLNumeric:
		_, err := SQLNumericValue(value.text, value.precision, value.scale)
		return err
	case SQLBoolean:
		return nil
	case SQLBytea:
		if len(value.raw) > maximumSQLByteaBytes {
			return fmt.Errorf("%w: bytea value exceeds its byte bound", ErrSQLValue)
		}
		return nil
	case SQLUUID:
		_, err := canonicalSQLUUID(value.text)
		return err
	case SQLJSON, SQLJSONB:
		if !json.Valid([]byte(value.text)) {
			return fmt.Errorf("%w: %s text is not valid JSON", ErrSQLValue, value.kind)
		}
		return nil
	case SQLTimestamptz:
		_, _, err := parseSQLInstantText(value.text)
		return err
	case SQLTimestamp:
		_, err := SQLLocalTimestampValue(value.text)
		return err
	default:
		return fmt.Errorf("%w: column type %q is unsupported", ErrUnsupportedSQL, value.kind)
	}
}

// Equal compares exact typed values. A timestamptz value is identified by its
// instant, which is what PostgreSQL itself stores; every other kind compares
// its exact retained value.
func (value SQLValue) Equal(other SQLValue) bool {
	if value.presence != other.presence || value.kind != other.kind || value.precision != other.precision || value.scale != other.scale {
		return false
	}
	if value.presence != contract.PresenceValue {
		return true
	}
	if value.kind == SQLTimestamptz {
		left, leftOK := value.Instant()
		right, rightOK := other.Instant()
		return leftOK && rightOK && left.Equal(right)
	}
	return value.text == other.text && value.boolean == other.boolean && bytes.Equal(value.raw, other.raw)
}

// parameterBytes returns the exact text-format binding for one value. A null
// binds as SQL NULL; an absent value is never bound at all.
func (value SQLValue) parameterBytes() ([]byte, error) {
	if err := value.Validate(); err != nil {
		return nil, err
	}
	switch value.presence {
	case contract.PresenceNull:
		return nil, nil
	case contract.PresenceAbsent:
		return nil, fmt.Errorf("%w: absent value cannot be bound as a parameter", ErrSQLValue)
	}
	switch value.kind {
	case SQLBytea:
		return []byte(`\x` + hex.EncodeToString(value.raw)), nil
	case SQLBoolean:
		if value.boolean {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case SQLTimestamp:
		return []byte(value.text), nil
	case SQLTimestamptz:
		instant, _, err := parseSQLInstantText(value.text)
		if err != nil {
			return nil, err
		}
		return []byte(instant.UTC().Format("2006-01-02T15:04:05.999999Z07:00")), nil
	default:
		return []byte(value.text), nil
	}
}

// decodeSQLTextValue decodes one exact PostgreSQL text-format output into the
// declared column type. Nothing is coerced: an unexpected rendering is refused.
func decodeSQLTextValue(column SQLColumn, raw []byte) (SQLValue, error) {
	if raw == nil {
		return SQLNullValue(column.Kind), nil
	}
	text := string(raw)
	switch column.Kind {
	case SQLText:
		value := SQLTextValue(text)
		if err := value.Validate(); err != nil {
			return SQLValue{}, err
		}
		return value, nil
	case SQLInt8:
		return SQLInt8Value(text)
	case SQLNumeric:
		return SQLNumericValue(text, column.Precision, column.Scale)
	case SQLBoolean:
		switch text {
		case "t", "true", "1":
			return SQLBooleanValue(true), nil
		case "f", "false", "0":
			return SQLBooleanValue(false), nil
		default:
			return SQLValue{}, fmt.Errorf("%w: %q is not a boolean", ErrSQLValue, text)
		}
	case SQLBytea:
		raw, err := hex.DecodeString(strings.TrimPrefix(text, `\x`))
		if err != nil {
			return SQLValue{}, fmt.Errorf("%w: bytea text is not hexadecimal", ErrSQLValue)
		}
		return SQLByteaValue(raw), nil
	case SQLUUID:
		return SQLUUIDValue(text)
	case SQLJSON, SQLJSONB:
		return SQLJSONValue(column.Kind, text)
	case SQLTimestamptz:
		_, canonical, err := parseSQLInstantText(text)
		if err != nil {
			return SQLValue{}, err
		}
		return SQLValue{kind: SQLTimestamptz, presence: contract.PresenceValue, text: canonical}, nil
	case SQLTimestamp:
		return SQLLocalTimestampValue(text)
	default:
		return SQLValue{}, fmt.Errorf("%w: column type %q is unsupported", ErrUnsupportedSQL, column.Kind)
	}
}

// QuoteSQLIdentifier renders exactly one identifier part. Embedded quotes are
// doubled; empty, NUL, control, over-long, padded, or multi-part names are
// refused rather than silently reinterpreted.
func QuoteSQLIdentifier(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: identifier is empty", ErrSQLIdentifier)
	}
	if len(name) > maximumSQLIdentifierBytes {
		return "", fmt.Errorf("%w: identifier exceeds %d bytes", ErrSQLIdentifier, maximumSQLIdentifierBytes)
	}
	if !utf8.ValidString(name) {
		return "", fmt.Errorf("%w: identifier is not valid UTF-8", ErrSQLIdentifier)
	}
	if strings.TrimSpace(name) != name {
		return "", fmt.Errorf("%w: identifier is padded with whitespace", ErrSQLIdentifier)
	}
	if strings.Contains(name, ".") {
		return "", fmt.Errorf("%w: identifier must be one name part", ErrSQLIdentifier)
	}
	for _, character := range name {
		if character == 0 || character < 0x20 || character == 0x7f {
			return "", fmt.Errorf("%w: identifier contains an unusable character", ErrSQLIdentifier)
		}
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`, nil
}

func quoteSQLRelation(schema, name string) (string, error) {
	quotedSchema, err := QuoteSQLIdentifier(schema)
	if err != nil {
		return "", err
	}
	quotedName, err := QuoteSQLIdentifier(name)
	if err != nil {
		return "", err
	}
	return quotedSchema + "." + quotedName, nil
}

func validateSQLText(text string) error {
	if len(text) > maximumSQLTextBytes {
		return fmt.Errorf("%w: text value exceeds its byte bound", ErrSQLValue)
	}
	if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
		return fmt.Errorf("%w: text value is not a NUL-free UTF-8 string", ErrSQLValue)
	}
	return nil
}

func validateSQLInteger(exact string) error {
	_, fraction, ok := sqlDecimalShape(exact)
	if !ok || fraction != 0 {
		return fmt.Errorf("%w: integer text is not a canonical base-10 integer", ErrSQLValue)
	}
	if _, err := strconv.ParseInt(exact, 10, 64); err != nil {
		return fmt.Errorf("%w: integer text is outside the int8 range", ErrSQLValue)
	}
	return nil
}

// sqlDecimalShape returns the exact integer and fraction digit counts of a
// canonical base-10 decimal string.
func sqlDecimalShape(exact string) (int, int, bool) {
	if exact == "" {
		return 0, 0, false
	}
	digits := exact
	negative := false
	if digits[0] == '-' {
		negative = true
		digits = digits[1:]
	}
	if digits == "" {
		return 0, 0, false
	}
	fraction := 0
	if index := strings.IndexByte(digits, '.'); index >= 0 {
		fraction = len(digits) - index - 1
		if fraction == 0 {
			return 0, 0, false
		}
		digits = digits[:index] + digits[index+1:]
	}
	if digits == "" {
		return 0, 0, false
	}
	for index := 0; index < len(digits); index++ {
		if digits[index] < '0' || digits[index] > '9' {
			return 0, 0, false
		}
	}
	integerDigits := len(digits) - fraction
	if integerDigits <= 0 {
		return 0, 0, false
	}
	if integerDigits > 1 && digits[0] == '0' {
		return 0, 0, false
	}
	if negative && strings.Trim(digits, "0") == "" {
		return 0, 0, false
	}
	return integerDigits, fraction, true
}

func canonicalSQLUUID(exact string) (string, error) {
	if len(exact) != 36 || exact[8] != '-' || exact[13] != '-' || exact[18] != '-' || exact[23] != '-' {
		return "", fmt.Errorf("%w: uuid must be 8-4-4-4-12 hexadecimal text", ErrSQLValue)
	}
	decoded, err := hex.DecodeString(strings.ReplaceAll(exact, "-", ""))
	if err != nil {
		return "", fmt.Errorf("%w: uuid text is not hexadecimal", ErrSQLValue)
	}
	canonical := hex.EncodeToString(decoded)
	return canonical[:8] + "-" + canonical[8:12] + "-" + canonical[12:16] + "-" + canonical[16:20] + "-" + canonical[20:], nil
}

// parseSQLInstantText accepts an RFC 3339 instant or PostgreSQL's own
// timestamptz rendering, and returns the instant together with its canonical
// RFC 3339 offset text. A text without an explicit offset is refused.
func parseSQLInstantText(text string) (time.Time, string, error) {
	canonical, err := canonicalSQLInstantText(text)
	if err != nil {
		return time.Time{}, "", err
	}
	instant, err := time.Parse(time.RFC3339, canonical)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: instant is not a valid RFC 3339 timestamp", ErrSQLValue)
	}
	return instant, canonical, nil
}

// requireSQLMicrosecondFraction refuses a fractional-second rendering that
// carries a non-zero digit below the microsecond. PostgreSQL timestamp and
// timestamptz store microseconds, so such a value has no exact representation:
// binding it would silently round or truncate it to a different instant or wall
// clock. Zero digits below the microsecond change nothing and are admitted. The
// fraction is the text after the whole seconds, with its leading dot.
func requireSQLMicrosecondFraction(fraction string) error {
	if !strings.HasPrefix(fraction, ".") {
		return nil
	}
	digits := fraction[1:]
	if len(digits) > sqlMicrosecondDigits && strings.Trim(digits[sqlMicrosecondDigits:], "0") != "" {
		return fmt.Errorf("%w: timestamp fraction .%s is finer than the microsecond PostgreSQL can represent",
			ErrSQLValue, digits)
	}
	return nil
}

// sqlMicrosecondDigits is the fractional-second precision PostgreSQL timestamp
// and timestamptz store.
const sqlMicrosecondDigits = 6

func canonicalSQLInstantText(text string) (string, error) {
	if len(text) < 20 {
		return "", fmt.Errorf("%w: instant %q is too short to carry an offset", ErrSQLValue, text)
	}
	body := text
	offset := ""
	if strings.ContainsAny(body, "Zz") {
		if !strings.HasSuffix(body, "Z") && !strings.HasSuffix(body, "z") {
			return "", fmt.Errorf("%w: instant has an unusable zone designator", ErrSQLValue)
		}
		body = body[:len(body)-1]
		offset = "Z"
	} else {
		index := 19
		for index < len(body) && body[index] != '+' && body[index] != '-' {
			index++
		}
		if index == len(body) {
			return "", fmt.Errorf("%w: instant must carry an explicit offset", ErrSQLValue)
		}
		offset, body = body[index:], body[:index]
	}
	if len(body) < 19 || body[4] != '-' || body[7] != '-' || body[10] != ' ' && body[10] != 'T' || body[13] != ':' || body[16] != ':' {
		return "", fmt.Errorf("%w: instant %q is not a supported timestamp rendering", ErrSQLValue, text)
	}
	body = body[:10] + "T" + body[11:]
	if err := requireSQLMicrosecondFraction(body[19:]); err != nil {
		return "", err
	}
	canonicalOffset, err := canonicalSQLOffset(offset)
	if err != nil {
		return "", err
	}
	if _, err := time.Parse(time.RFC3339, body+canonicalOffset); err != nil {
		return "", fmt.Errorf("%w: instant %q is not a valid timestamp", ErrSQLValue, text)
	}
	return body + canonicalOffset, nil
}

func canonicalSQLOffset(offset string) (string, error) {
	switch offset {
	case "Z", "z":
		return "Z", nil
	}
	if len(offset) < 3 || (offset[0] != '+' && offset[0] != '-') {
		return "", fmt.Errorf("%w: instant offset is invalid", ErrSQLValue)
	}
	digits := offset[1:]
	normalized := ""
	switch {
	case len(digits) == 2:
		normalized = digits + ":00"
	case len(digits) == 4:
		normalized = digits[:2] + ":" + digits[2:]
	case len(digits) == 5 && digits[2] == ':':
		normalized = digits
	default:
		return "", fmt.Errorf("%w: instant offset is invalid", ErrSQLValue)
	}
	hours, hourErr := strconv.Atoi(normalized[:2])
	minutes, minuteErr := strconv.Atoi(normalized[3:])
	if hourErr != nil || minuteErr != nil || hours > 23 || minutes > 59 {
		return "", fmt.Errorf("%w: instant offset is outside the permitted range", ErrSQLValue)
	}
	if offset[0] == '+' && hours == 0 && minutes == 0 {
		return "Z", nil
	}
	return offset[0:1] + normalized, nil
}

// supported reports whether one declared column type is transportable at all,
// independent of any numeric precision facts.
func (kind SQLValueKind) supported() bool {
	switch kind {
	case SQLText, SQLInt8, SQLNumeric, SQLBoolean, SQLBytea, SQLUUID, SQLJSON, SQLJSONB, SQLTimestamptz, SQLTimestamp:
		return true
	default:
		return false
	}
}

// SQLColumn is one declared column of the bound relation.
type SQLColumn struct {
	Name string
	Kind SQLValueKind
	// Precision and Scale are the declared numeric(p,s) facts and are zero for
	// every other type.
	Precision int
	Scale     int
	// Key marks the declared key column of a mapped upsert or a bounded keyed
	// select. Only declared key columns may filter a select.
	Key bool
}

func (column SQLColumn) Validate() error {
	if _, err := QuoteSQLIdentifier(column.Name); err != nil {
		return err
	}
	_, err := column.Kind.sqlTypeName(column.Precision, column.Scale)
	return err
}

// SQLRelation is the exact relation binding declared by one operation.
type SQLRelation struct {
	// Binding is the operation's declared transport relation_binding.
	Binding string
	Schema  string
	Name    string
	Columns []SQLColumn
}

func (relation SQLRelation) Validate() error {
	if relation.Binding == "" || !utf8.ValidString(relation.Binding) ||
		strings.ContainsAny(relation.Binding, "\x00\r\n") || strings.TrimSpace(relation.Binding) != relation.Binding {
		return fmt.Errorf("%w: binding identity must be a nonempty stable reference", ErrSQLRelation)
	}
	if _, err := QuoteSQLIdentifier(relation.Schema); err != nil {
		return fmt.Errorf("%w: schema: %v", ErrSQLRelation, err)
	}
	if _, err := QuoteSQLIdentifier(relation.Name); err != nil {
		return fmt.Errorf("%w: relation: %v", ErrSQLRelation, err)
	}
	if len(relation.Columns) == 0 {
		return fmt.Errorf("%w: relation must declare at least one column", ErrSQLRelation)
	}
	seen := make(map[string]struct{}, len(relation.Columns))
	keys := 0
	for _, column := range relation.Columns {
		if err := column.Validate(); err != nil {
			return err
		}
		if _, duplicate := seen[column.Name]; duplicate {
			return fmt.Errorf("%w: column %q is declared twice", ErrSQLRelation, column.Name)
		}
		seen[column.Name] = struct{}{}
		if column.Key {
			keys++
		}
	}
	if keys == 0 {
		return fmt.Errorf("%w: relation must declare a key column", ErrSQLRelation)
	}
	return nil
}

func (relation SQLRelation) Column(name string) (SQLColumn, bool) {
	for _, column := range relation.Columns {
		if column.Name == name {
			return column, true
		}
	}
	return SQLColumn{}, false
}

func (relation SQLRelation) qualifiedName() (string, error) {
	return quoteSQLRelation(relation.Schema, relation.Name)
}

// SQLColumnValue binds one declared column to one exact typed value. The same
// type carries mapped write inputs and decoded observation rows.
type SQLColumnValue struct {
	Column string
	Value  SQLValue
}

func (value SQLColumnValue) Validate() error {
	if _, err := QuoteSQLIdentifier(value.Column); err != nil {
		return err
	}
	return value.Value.Validate()
}

// SQLParameter is one bound statement parameter. Column is empty for the
// relation-binding parameter of a metadata statement.
type SQLParameter struct {
	Column string
	Value  SQLValue
}

// SQLStatement is one fully parameterized statement: exact SQL text containing
// only declared identifiers and positional parameters, plus the ordered
// bindings. Caller data is never part of Text.
type SQLStatement struct {
	Kind       contract.PostgreSQLStatementKind
	Text       string
	Parameters []SQLParameter
	Columns    []SQLColumn
}

func (statement SQLStatement) parameterBytes() ([][]byte, error) {
	bindings := make([][]byte, 0, len(statement.Parameters))
	for _, parameter := range statement.Parameters {
		binding, err := parameter.Value.parameterBytes()
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	return bindings, nil
}

// SQLRow is one exact typed row in declared column order.
type SQLRow struct {
	Columns []string
	Values  []SQLValue
}

func (row SQLRow) Value(column string) (SQLValue, bool) {
	for index, name := range row.Columns {
		if name == column && index < len(row.Values) {
			return row.Values[index], true
		}
	}
	return SQLValue{}, false
}

func (row SQLRow) Validate() error {
	if len(row.Columns) != len(row.Values) {
		return fmt.Errorf("%w: row column and value counts differ", ErrSQLValue)
	}
	for _, value := range row.Values {
		if err := value.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Metadata statement column names.
const (
	SQLMetadataColumnName = "column_name"
	SQLMetadataColumnType = "data_type"
)

// VerifySQLRelation binds one declared relation to the live relation metadata
// observed by the metadata statement. A missing, extra, or differently typed
// column is refused.
func VerifySQLRelation(relation SQLRelation, metadata []SQLRow) error {
	if err := relation.Validate(); err != nil {
		return err
	}
	if len(metadata) == 0 {
		return fmt.Errorf("%w: live relation metadata is absent", ErrSQLRelation)
	}
	live := make(map[string]string, len(metadata))
	for _, row := range metadata {
		name, ok := row.Value(SQLMetadataColumnName)
		if !ok {
			return fmt.Errorf("%w: live relation metadata has no column name", ErrSQLRelation)
		}
		kind, ok := row.Value(SQLMetadataColumnType)
		if !ok {
			return fmt.Errorf("%w: live relation metadata has no column type", ErrSQLRelation)
		}
		columnName, nameOK := name.Text()
		columnType, typeOK := kind.Text()
		if !nameOK || !typeOK {
			return fmt.Errorf("%w: live relation metadata is not text-typed", ErrSQLRelation)
		}
		live[columnName] = columnType
	}
	for _, column := range relation.Columns {
		expected, err := column.Kind.sqlTypeName(column.Precision, column.Scale)
		if err != nil {
			return err
		}
		observed, ok := live[column.Name]
		if !ok {
			return fmt.Errorf("%w: live relation is missing declared column %q", ErrSQLRelation, column.Name)
		}
		if observed != expected {
			return fmt.Errorf("%w: declared column %q is %s but the live relation is %s", ErrSQLRelation, column.Name, expected, observed)
		}
		delete(live, column.Name)
	}
	if len(live) != 0 {
		names := make([]string, 0, len(live))
		for name := range live {
			names = append(names, name)
		}
		return fmt.Errorf("%w: live relation has undeclared columns %v", ErrSQLRelation, names)
	}
	return nil
}

// validateSQLSourceFacts refuses any operation that is not a declared
// PostgreSQL transport before any physical work happens.
func validateSQLSourceFacts(operation contract.Operation) error {
	if operation.Transport.Kind != contract.TransportKindPostgreSQL {
		return ErrUnsupportedSQL
	}
	if err := operation.Transport.Validate(); err != nil {
		return fmt.Errorf("%w: invalid PostgreSQL transport contract", ErrUnsupportedSQL)
	}
	if len(operation.Responses) == 0 {
		return fmt.Errorf("%w: response contract is absent", ErrUnsupportedSQL)
	}
	for _, response := range operation.Responses {
		if err := response.Validate(contract.TransportKindPostgreSQL); err != nil {
			return fmt.Errorf("%w: invalid response contract", ErrUnsupportedSQL)
		}
	}
	return nil
}

// BuildSQLStatement builds the one declared statement for the operation's
// statement kind. Every value becomes a positional parameter; only declared
// identifiers and fixed keywords appear in the SQL text.
func BuildSQLStatement(
	operation contract.Operation,
	relation SQLRelation,
	input []SQLColumnValue,
	rowBound uint64,
) (SQLStatement, error) {
	if err := validateSQLSourceFacts(operation); err != nil {
		return SQLStatement{}, err
	}
	if err := relation.Validate(); err != nil {
		return SQLStatement{}, err
	}
	if relation.Binding != operation.Transport.RelationBinding {
		return SQLStatement{}, fmt.Errorf("%w: relation binding does not match the declared operation", ErrSQLRelation)
	}
	mapped, err := orderSQLInput(relation, input)
	if err != nil {
		return SQLStatement{}, err
	}
	qualified, err := relation.qualifiedName()
	if err != nil {
		return SQLStatement{}, err
	}
	projection, err := sqlProjection(relation)
	if err != nil {
		return SQLStatement{}, err
	}
	switch operation.Transport.StatementKind {
	case contract.PostgreSQLStatementMetadata:
		if len(input) != 0 {
			return SQLStatement{}, fmt.Errorf("%w: metadata statement takes no mapped input", ErrSQLStatement)
		}
		return sqlMetadataStatement(relation), nil
	case contract.PostgreSQLStatementSelect:
		return sqlSelectStatement(relation, mapped, qualified, projection, rowBound)
	case contract.PostgreSQLStatementMappedUpsert:
		return sqlMappedUpsertStatement(relation, mapped, qualified, projection)
	default:
		return SQLStatement{}, fmt.Errorf("%w: statement kind %q is unsupported", ErrUnsupportedSQL, operation.Transport.StatementKind)
	}
}

func sqlMetadataStatement(relation SQLRelation) SQLStatement {
	text := `SELECT attribute.attname::text AS ` + SQLMetadataColumnName +
		`, pg_catalog.format_type(attribute.atttypid, attribute.atttypmod)::text AS ` + SQLMetadataColumnType +
		` FROM pg_catalog.pg_attribute AS attribute` +
		` JOIN pg_catalog.pg_class AS relation ON relation.oid = attribute.attrelid` +
		` JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid = relation.relnamespace` +
		` WHERE namespace.nspname = CAST($1 AS text) AND relation.relname = CAST($2 AS text)` +
		` AND relation.relkind IN ('r', 'p') AND attribute.attnum > 0 AND NOT attribute.attisdropped` +
		` ORDER BY attribute.attnum`
	return SQLStatement{
		Kind: contract.PostgreSQLStatementMetadata,
		Text: text,
		Parameters: []SQLParameter{
			{Value: SQLTextValue(relation.Schema)},
			{Value: SQLTextValue(relation.Name)},
		},
		Columns: []SQLColumn{
			{Name: SQLMetadataColumnName, Kind: SQLText},
			{Name: SQLMetadataColumnType, Kind: SQLText},
		},
	}
}

func sqlSelectStatement(
	relation SQLRelation,
	mapped map[string]SQLValue,
	qualified string,
	projection string,
	rowBound uint64,
) (SQLStatement, error) {
	if rowBound == 0 {
		return SQLStatement{}, fmt.Errorf("%w: bounded select requires a positive row bound", ErrSQLStatement)
	}
	parameters := make([]SQLParameter, 0, len(mapped)+1)
	conditions := make([]string, 0, len(mapped))
	keys := make([]SQLColumn, 0, len(relation.Columns))
	for _, column := range relation.Columns {
		if !column.Key {
			continue
		}
		keys = append(keys, column)
		value, present := mapped[column.Name]
		if !present {
			continue
		}
		if value.IsAbsent() || value.IsNull() {
			return SQLStatement{}, fmt.Errorf("%w: key column %q requires an exact value", ErrSQLStatement, column.Name)
		}
		quoted, err := QuoteSQLIdentifier(column.Name)
		if err != nil {
			return SQLStatement{}, err
		}
		typeName, err := column.Kind.sqlTypeName(column.Precision, column.Scale)
		if err != nil {
			return SQLStatement{}, err
		}
		parameters = append(parameters, SQLParameter{Column: column.Name, Value: value})
		conditions = append(conditions, quoted+" = CAST($"+strconv.Itoa(len(parameters))+" AS "+typeName+")")
	}
	for name := range mapped {
		column, ok := relation.Column(name)
		if !ok || !column.Key {
			return SQLStatement{}, fmt.Errorf("%w: only declared key columns can filter a bounded select", ErrSQLStatement)
		}
	}
	text := "SELECT " + projection + " FROM " + qualified
	if len(conditions) != 0 {
		text += " WHERE " + strings.Join(conditions, " AND ")
	}
	if len(keys) != 0 {
		ordered := make([]string, 0, len(keys))
		for _, column := range keys {
			quoted, err := QuoteSQLIdentifier(column.Name)
			if err != nil {
				return SQLStatement{}, err
			}
			ordered = append(ordered, quoted)
		}
		text += " ORDER BY " + strings.Join(ordered, ", ")
	}
	parameters = append(parameters, SQLParameter{Value: sqlInt8Bound(rowBound)})
	text += " LIMIT CAST($" + strconv.Itoa(len(parameters)) + " AS bigint)"
	return SQLStatement{Kind: contract.PostgreSQLStatementSelect, Text: text, Parameters: parameters, Columns: relation.Columns}, nil
}

func sqlMappedUpsertStatement(
	relation SQLRelation,
	mapped map[string]SQLValue,
	qualified string,
	projection string,
) (SQLStatement, error) {
	parameters := make([]SQLParameter, 0, len(mapped))
	insertColumns := make([]string, 0, len(mapped))
	valueExpressions := make([]string, 0, len(mapped))
	updates := make([]string, 0, len(mapped))
	retained := make([]string, 0, len(mapped))
	proposed := make([]string, 0, len(mapped))
	keys := make([]string, 0, len(relation.Columns))
	for _, column := range relation.Columns {
		quoted, err := QuoteSQLIdentifier(column.Name)
		if err != nil {
			return SQLStatement{}, err
		}
		if column.Key {
			keys = append(keys, quoted)
		}
		value, present := mapped[column.Name]
		if !present || value.IsAbsent() {
			// An absent column keeps the declared default on insert and the
			// retained value on conflict.
			if column.Key {
				return SQLStatement{}, fmt.Errorf("%w: key column %q requires an exact value", ErrSQLStatement, column.Name)
			}
			continue
		}
		if column.Key && value.IsNull() {
			return SQLStatement{}, fmt.Errorf("%w: key column %q requires an exact value", ErrSQLStatement, column.Name)
		}
		typeName, err := column.Kind.sqlTypeName(column.Precision, column.Scale)
		if err != nil {
			return SQLStatement{}, err
		}
		parameters = append(parameters, SQLParameter{Column: column.Name, Value: value})
		insertColumns = append(insertColumns, quoted)
		valueExpressions = append(valueExpressions, "CAST($"+strconv.Itoa(len(parameters))+" AS "+typeName+")")
		if !column.Key {
			updates = append(updates, quoted+" = EXCLUDED."+quoted)
			retained = append(retained, sqlUpsertComparable(column.Kind, sqlUpsertTargetAlias+"."+quoted))
			proposed = append(proposed, sqlUpsertComparable(column.Kind, "EXCLUDED."+quoted))
		}
	}
	if len(insertColumns) == 0 {
		return SQLStatement{}, fmt.Errorf("%w: mapped upsert requires at least one mapped column", ErrSQLStatement)
	}
	text := "INSERT INTO " + qualified + " AS " + sqlUpsertTargetAlias + " (" + strings.Join(insertColumns, ", ") +
		") VALUES (" + strings.Join(valueExpressions, ", ") + ") ON CONFLICT (" + strings.Join(keys, ", ") + ")"
	if len(updates) == 0 {
		text += " DO NOTHING"
	} else {
		// A retained row whose mapped values already equal the proposed ones is
		// never assigned: the conflict arm makes no physical update and returns
		// no row, which the caller settles as unchanged.
		text += " DO UPDATE SET " + strings.Join(updates, ", ") +
			" WHERE ROW(" + strings.Join(retained, ", ") + ") IS DISTINCT FROM ROW(" + strings.Join(proposed, ", ") + ")"
	}
	text += " RETURNING " + projection
	return SQLStatement{
		Kind: contract.PostgreSQLStatementMappedUpsert, Text: text,
		Parameters: parameters, Columns: relation.Columns,
	}, nil
}

// sqlUpsertTargetAlias names the retained row of a mapped upsert's conflict arm.
const sqlUpsertTargetAlias = "pm_target"

// sqlUpsertComparable renders one mapped column for the unchanged-row guard.
// json has no equality operator, so its exact stored text is compared; every
// other declared kind compares by its own type equality.
func sqlUpsertComparable(kind SQLValueKind, reference string) string {
	if kind == SQLJSON {
		return reference + "::text"
	}
	return reference
}

// sqlProjection renders every declared column as exactly one text-format
// output so no driver-side decoding can alter a value.
func sqlProjection(relation SQLRelation) (string, error) {
	projections := make([]string, 0, len(relation.Columns))
	for _, column := range relation.Columns {
		quoted, err := QuoteSQLIdentifier(column.Name)
		if err != nil {
			return "", err
		}
		if column.Kind == SQLBytea {
			projections = append(projections, "encode("+quoted+", 'hex')::text AS "+quoted)
			continue
		}
		projections = append(projections, quoted+"::text AS "+quoted)
	}
	return strings.Join(projections, ", "), nil
}

// sqlInt8Bound renders one server-owned row bound as an exact int8 value.
func sqlInt8Bound(bound uint64) SQLValue {
	if bound > 1<<62 {
		bound = 1 << 62
	}
	return SQLValue{kind: SQLInt8, presence: contract.PresenceValue, text: strconv.FormatUint(bound, 10)}
}

// orderSQLInput refuses undeclared or duplicated columns and returns the
// mapped values in declared column order.
func orderSQLInput(relation SQLRelation, input []SQLColumnValue) (map[string]SQLValue, error) {
	mapped := make(map[string]SQLValue, len(input))
	for _, bound := range input {
		column, ok := relation.Column(bound.Column)
		if !ok {
			return nil, fmt.Errorf("%w: column %q is not declared by the relation binding", ErrSQLStatement, bound.Column)
		}
		if _, duplicate := mapped[bound.Column]; duplicate {
			return nil, fmt.Errorf("%w: column %q is bound twice", ErrSQLStatement, bound.Column)
		}
		if err := bound.Value.Validate(); err != nil {
			return nil, err
		}
		if bound.Value.Kind() != column.Kind ||
			(bound.Value.presence == contract.PresenceValue &&
				(bound.Value.precision != column.Precision || bound.Value.scale != column.Scale)) {
			return nil, fmt.Errorf("%w: bound value for column %q does not match its declared type", ErrSQLStatement, bound.Column)
		}
		mapped[bound.Column] = bound.Value
	}
	return mapped, nil
}
