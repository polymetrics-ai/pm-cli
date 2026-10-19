// Package warehouse owns the durable analytical warehouse boundary: immutable
// shareable segments registered through PostgreSQL, and the restricted
// analytical child that reads admitted local Parquet files without ever
// importing the native engine into the worker. This package never imports the
// DuckDB driver; only internal/analytics and cmd/pm-query do.
package warehouse

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

// The parent and child share one versioned protocol. These values publish the
// child's finite capability so the parent refuses a structurally impossible
// request before any process exists.
const (
	QueryProtocolVersion            = "pm.query/1"
	MaxQueryDocumentBytes           = 8 << 20
	MaximumQueryRelations           = 8
	MaximumQueryColumns             = 64
	MaximumQueryAdmittedDirectories = 8
	MaximumQueryIdentifierBytes     = 64
	MaximumQueryPredicates          = 16
	MaximumQueryOrderings           = 8
	MaximumQueryPredicateValues     = 64
	MaximumQueryRows                = 1 << 20
	MaximumQueryBytes               = 64 << 20
	MaximumQueryTempBytes           = 512 << 20
	MaximumQueryMemoryBytes         = 1 << 30
	MaximumQueryWallTime            = 5 * time.Minute
	MaximumQueryValueBytes          = 1 << 20
	MaximumQueryDecimalDigits       = 38
	// maximumQueryWallMillis is the wall-time capability in the wire form's own
	// unit. A decoded value is range-checked against it before it is converted
	// into a duration, because the nanosecond multiplication wraps.
	maximumQueryWallMillis = int64(MaximumQueryWallTime / time.Millisecond)
)

// QueryKind is the closed analytical value vocabulary. A value never changes
// class silently: an exact projection carries the declared kind end to end.
type QueryKind string

const (
	QueryKindInt64       QueryKind = "int64"
	QueryKindDecimal     QueryKind = "decimal"
	QueryKindText        QueryKind = "text"
	QueryKindBoolean     QueryKind = "boolean"
	QueryKindBytea       QueryKind = "bytea"
	QueryKindDate        QueryKind = "date"
	QueryKindTimestamp   QueryKind = "timestamp"
	QueryKindTimestampTZ QueryKind = "timestamptz"
	QueryKindUUID        QueryKind = "uuid"
	QueryKindJSON        QueryKind = "json"
)

// Valid reports whether the kind belongs to the closed vocabulary.
func (kind QueryKind) Valid() bool {
	switch kind {
	case QueryKindInt64, QueryKindDecimal, QueryKindText, QueryKindBoolean, QueryKindBytea,
		QueryKindDate, QueryKindTimestamp, QueryKindTimestampTZ, QueryKindUUID, QueryKindJSON:
		return true
	default:
		return false
	}
}

// QueryColumn is one declared column. A decimal must declare its precision and
// scale; every other kind must not.
type QueryColumn struct {
	Name      string
	Kind      QueryKind
	Precision int
	Scale     int
	Nullable  bool
}

// Validate refuses a column that cannot describe an exact projection.
func (column QueryColumn) Validate() error {
	if err := validateQueryIdentifier(column.Name); err != nil {
		return err
	}
	if !column.Kind.Valid() {
		return refusal(RefusalProtocol, "column %q declares an unsupported kind", column.Name)
	}
	if column.Kind == QueryKindDecimal {
		if column.Precision <= 0 || column.Precision > MaximumQueryDecimalDigits {
			return refusal(RefusalProtocol, "decimal column %q must declare a precision of 1..%d", column.Name, MaximumQueryDecimalDigits)
		}
		if column.Scale < 0 || column.Scale > column.Precision {
			return refusal(RefusalProtocol, "decimal column %q must declare a scale of 0..precision", column.Name)
		}
		return nil
	}
	if column.Precision != 0 || column.Scale != 0 {
		return refusal(RefusalProtocol, "column %q declares precision or scale for a non-decimal kind", column.Name)
	}
	return nil
}

// QueryValue is one exactly typed protocol value. Presence is explicit: the
// zero QueryValue is absent, NullQueryValue is an explicit SQL NULL, and every
// other value carries exactly one payload.
type QueryValue struct {
	present  bool
	kind     QueryKind
	null     bool
	integer  int64
	text     string
	unscaled string
	scale    int
	boolean  bool
	binary   []byte
}

// AbsentQueryValue is a member that was not supplied at all.
func AbsentQueryValue() QueryValue { return QueryValue{} }

// NullQueryValue is an explicit SQL NULL, which is never the same as an absent
// member.
func NullQueryValue() QueryValue { return QueryValue{present: true, null: true} }

func Int64QueryValue(value int64) QueryValue {
	return QueryValue{present: true, kind: QueryKindInt64, integer: value}
}

// DecimalQueryValue carries an exact unscaled integer and its declared scale.
func DecimalQueryValue(unscaled string, scale int) (QueryValue, error) {
	if err := validateQueryUnscaled(unscaled); err != nil {
		return QueryValue{}, err
	}
	if scale < 0 || scale > MaximumQueryDecimalDigits {
		return QueryValue{}, refusal(RefusalProtocol, "decimal scale %d is outside 0..%d", scale, MaximumQueryDecimalDigits)
	}
	digits := strings.TrimLeft(strings.TrimPrefix(unscaled, "-"), "0")
	if len(digits)+scale > MaximumQueryDecimalDigits {
		return QueryValue{}, refusal(RefusalProtocol, "decimal needs more than %d digits", MaximumQueryDecimalDigits)
	}
	return QueryValue{present: true, kind: QueryKindDecimal, unscaled: unscaled, scale: scale}, nil
}

// DecimalQueryValueText carries one exact decimal given in the shared contract
// form: base-10 text with an explicit scale. "10.20" with scale 2 is the
// unscaled integer 1020 at scale 2. The text must carry exactly the stated
// scale; it is never rescaled, rounded or padded.
func DecimalQueryValueText(text string, scale int) (QueryValue, error) {
	negative := strings.HasPrefix(text, "-")
	whole, fraction, found := strings.Cut(strings.TrimPrefix(text, "-"), ".")
	if whole == "" || (found && fraction == "") || len(fraction) != scale {
		return QueryValue{}, refusal(RefusalProtocol, "decimal text must carry exactly its declared scale %d", scale)
	}
	for _, character := range whole + fraction {
		if character < '0' || character > '9' {
			return QueryValue{}, refusal(RefusalProtocol, "decimal text must be base-10")
		}
	}
	unscaled := strings.TrimLeft(whole+fraction, "0")
	if unscaled == "" {
		unscaled = "0"
	}
	if negative {
		unscaled = "-" + unscaled
	}
	return DecimalQueryValue(unscaled, scale)
}

// TextQueryValue carries bounded text. An over-long payload is refused when the
// request is validated rather than truncated here.
func TextQueryValue(text string) QueryValue {
	return QueryValue{present: true, kind: QueryKindText, text: text}
}

func BooleanQueryValue(value bool) QueryValue {
	return QueryValue{present: true, kind: QueryKindBoolean, boolean: value}
}

// BytesQueryValue carries raw bytes. An over-long payload is refused when the
// request is validated rather than truncated here.
func BytesQueryValue(data []byte) QueryValue {
	return QueryValue{present: true, kind: QueryKindBytea, binary: append([]byte(nil), data...)}
}

// DateQueryValue carries one canonical YYYY-MM-DD calendar date.
func DateQueryValue(value string) (QueryValue, error) {
	parsed, err := time.Parse("2006-01-02", value)
	if err != nil || parsed.Format("2006-01-02") != value {
		return QueryValue{}, refusal(RefusalProtocol, "date must be an exact YYYY-MM-DD calendar date")
	}
	return QueryValue{present: true, kind: QueryKindDate, text: value}, nil
}

// TimestampQueryValue carries exact microseconds since 1970-01-01T00:00:00 for
// a timestamp without time zone.
func TimestampQueryValue(microseconds int64) QueryValue {
	return QueryValue{present: true, kind: QueryKindTimestamp, integer: microseconds}
}

// TimestampTZQueryValue carries the exact instant in microseconds since the
// Unix epoch.
func TimestampTZQueryValue(microseconds int64) QueryValue {
	return QueryValue{present: true, kind: QueryKindTimestampTZ, integer: microseconds}
}

// UUIDQueryValue carries one canonical lowercase UUID.
func UUIDQueryValue(value string) (QueryValue, error) {
	if len(value) != 36 {
		return QueryValue{}, refusal(RefusalProtocol, "UUID must be a canonical 36-character value")
	}
	for index, character := range value {
		switch index {
		case 8, 13, 18, 23:
			if character != '-' {
				return QueryValue{}, refusal(RefusalProtocol, "UUID must be a canonical lowercase value")
			}
		default:
			if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
				return QueryValue{}, refusal(RefusalProtocol, "UUID must be a canonical lowercase value")
			}
		}
	}
	return QueryValue{present: true, kind: QueryKindUUID, text: value}, nil
}

// JSONQueryValue carries one document's exact JSON text.
func JSONQueryValue(value string) (QueryValue, error) {
	if !json.Valid([]byte(value)) {
		return QueryValue{}, refusal(RefusalProtocol, "JSON text is not a valid document")
	}
	return QueryValue{present: true, kind: QueryKindJSON, text: value}, nil
}

// Present reports whether the value was supplied at all.
func (value QueryValue) Present() bool { return value.present }

// IsNull reports an explicit SQL NULL.
func (value QueryValue) IsNull() bool { return value.null }

// Kind reports the exact class of a present, non-null value.
func (value QueryValue) Kind() QueryKind {
	if !value.present || value.null {
		return ""
	}
	return value.kind
}

// Int64 returns the exact integer payload of an int64 or timestamp value.
func (value QueryValue) Int64() (int64, bool) {
	switch value.Kind() {
	case QueryKindInt64, QueryKindTimestamp, QueryKindTimestampTZ:
		return value.integer, true
	default:
		return 0, false
	}
}

// Unscaled returns the exact unscaled decimal payload and its scale.
func (value QueryValue) Unscaled() (string, int, bool) {
	if value.Kind() != QueryKindDecimal {
		return "", 0, false
	}
	return value.unscaled, value.scale, true
}

// DecimalText renders a decimal value in the shared contract form: exact
// base-10 text with its explicit scale, so unscaled 1020 at scale 2 is "10.20"
// and unscaled 5 at scale 2 is "0.05".
func (value QueryValue) DecimalText() (string, int, bool) {
	if value.Kind() != QueryKindDecimal {
		return "", 0, false
	}
	if value.scale == 0 {
		return value.unscaled, 0, true
	}
	digits := strings.TrimPrefix(value.unscaled, "-")
	if len(digits) <= value.scale {
		digits = strings.Repeat("0", value.scale-len(digits)+1) + digits
	}
	text := digits[:len(digits)-value.scale] + "." + digits[len(digits)-value.scale:]
	if strings.HasPrefix(value.unscaled, "-") {
		text = "-" + text
	}
	return text, value.scale, true
}

// Text returns the exact text payload of a text, date, UUID, or JSON value.
func (value QueryValue) Text() (string, bool) {
	switch value.Kind() {
	case QueryKindText, QueryKindDate, QueryKindUUID, QueryKindJSON:
		return value.text, true
	default:
		return "", false
	}
}

// Bytes returns the exact byte payload of a bytea value.
func (value QueryValue) Bytes() ([]byte, bool) {
	if value.Kind() != QueryKindBytea {
		return nil, false
	}
	return value.binary, true
}

// Boolean returns the exact boolean payload.
func (value QueryValue) Boolean() (bool, bool) {
	if value.Kind() != QueryKindBoolean {
		return false, false
	}
	return value.boolean, true
}

// Validate rechecks one value against the protocol's exact classes.
func (value QueryValue) Validate() error {
	if !value.present {
		return nil
	}
	if value.null {
		if value.kind != "" {
			return refusal(RefusalProtocol, "an explicit null carries a payload kind")
		}
		return nil
	}
	switch value.kind {
	case QueryKindInt64, QueryKindBoolean, QueryKindTimestamp, QueryKindTimestampTZ:
		return nil
	case QueryKindDecimal:
		_, err := DecimalQueryValue(value.unscaled, value.scale)
		return err
	case QueryKindText:
		if !utf8.ValidString(value.text) || len(value.text) > MaximumQueryValueBytes {
			return refusal(RefusalProtocol, "text must be valid UTF-8 within the per-value byte bound")
		}
		return nil
	case QueryKindBytea:
		if len(value.binary) > MaximumQueryValueBytes {
			return refusal(RefusalProtocol, "bytea exceeds the per-value byte bound")
		}
		return nil
	case QueryKindDate:
		_, err := DateQueryValue(value.text)
		return err
	case QueryKindUUID:
		_, err := UUIDQueryValue(value.text)
		return err
	case QueryKindJSON:
		_, err := JSONQueryValue(value.text)
		return err
	default:
		return refusal(RefusalProtocol, "the value kind is unsupported")
	}
}

// String is a bounded description for diagnostics that never renders a full
// payload.
func (value QueryValue) String() string {
	switch {
	case !value.present:
		return "absent"
	case value.null:
		return "null"
	}
	switch value.kind {
	case QueryKindInt64, QueryKindTimestamp, QueryKindTimestampTZ:
		return fmt.Sprintf("%s(%d)", value.kind, value.integer)
	case QueryKindDecimal:
		return fmt.Sprintf("decimal(%s,%d)", value.unscaled, value.scale)
	case QueryKindBoolean:
		return fmt.Sprintf("boolean(%t)", value.boolean)
	case QueryKindBytea:
		return fmt.Sprintf("bytea(%d bytes)", len(value.binary))
	case QueryKindText, QueryKindDate, QueryKindUUID, QueryKindJSON:
		return fmt.Sprintf("%s(%d bytes)", value.kind, len(value.text))
	default:
		return "invalid"
	}
}

// RefusalCode is one closed refusal class. The parent and the child use the
// same vocabulary so a caller can act on a refusal without parsing prose.
type RefusalCode string

const (
	RefusalProtocol          RefusalCode = "protocol"
	RefusalUnadmittedPath    RefusalCode = "unadmitted_path"
	RefusalUnknownRelation   RefusalCode = "unknown_relation"
	RefusalUnknownColumn     RefusalCode = "unknown_column"
	RefusalSchemaMismatch    RefusalCode = "schema_mismatch"
	RefusalDigestMismatch    RefusalCode = "digest_mismatch"
	RefusalBoundExceeded     RefusalCode = "bound_exceeded"
	RefusalUnsupportedKind   RefusalCode = "unsupported_kind"
	RefusalEngine            RefusalCode = "engine"
	RefusalEnvironment       RefusalCode = "environment"
	RefusalOutputFailed      RefusalCode = "output_failed"
	RefusalDescriptorLeak    RefusalCode = "descriptor_leak"
	RefusalChildTimeout      RefusalCode = "child_timeout"
	RefusalChildStartup      RefusalCode = "child_startup"
	RefusalChildKilled       RefusalCode = "child_killed"
	RefusalChildExit         RefusalCode = "child_exit"
	RefusalChildOutputExceed RefusalCode = "child_output_exceeded"
)

// Valid reports whether the code belongs to the published vocabulary.
func (code RefusalCode) Valid() bool {
	switch code {
	case RefusalProtocol, RefusalUnadmittedPath, RefusalUnknownRelation, RefusalUnknownColumn,
		RefusalSchemaMismatch, RefusalDigestMismatch, RefusalBoundExceeded, RefusalUnsupportedKind,
		RefusalEngine, RefusalEnvironment, RefusalOutputFailed, RefusalDescriptorLeak,
		RefusalChildTimeout, RefusalChildStartup, RefusalChildKilled, RefusalChildExit,
		RefusalChildOutputExceed:
		return true
	default:
		return false
	}
}

// RefusalOrigin records which side refused, so a caller can tell a
// pre-execution admission refusal from a child observation.
type RefusalOrigin string

const (
	// RefusalOriginSupervisor is a refusal taken before or without a child run.
	RefusalOriginSupervisor RefusalOrigin = "supervisor"
	// RefusalOriginChild is a refusal reported by the restricted child itself.
	RefusalOriginChild RefusalOrigin = "child"
)

// Refusal is the explicit, typed outcome of a refused request. It never carries
// rows: an unavailable answer is refused rather than invented.
type Refusal struct {
	Code   RefusalCode
	Origin RefusalOrigin
	Reason string
	// ExitCode is the child exit status the refusing side observed: 0 for a
	// child that exited zero, the real status for any other exit, and -1 when no
	// exit status exists because the child never ran or was never reaped.
	ExitCode int
	Child    *ChildReport
}

func (failure *Refusal) Error() string {
	if failure == nil {
		return "query refusal"
	}
	return fmt.Sprintf("%s refusal (%s): %s", failure.Origin, failure.Code, failure.Reason)
}

// refusal builds one supervisor-origin refusal.
func refusal(code RefusalCode, format string, arguments ...any) error {
	return &Refusal{
		Code:     code,
		Origin:   RefusalOriginSupervisor,
		Reason:   sanitizeReason(fmt.Sprintf(format, arguments...)),
		ExitCode: -1,
	}
}

// Unwrap exposes the class error for errors.Is checks by code family.
func (failure *Refusal) Unwrap() error {
	switch failure.Code {
	case RefusalProtocol:
		return errQueryProtocol
	case RefusalUnadmittedPath:
		return errQueryPath
	case RefusalDigestMismatch:
		return errQueryDigest
	case RefusalSchemaMismatch:
		return errQuerySchema
	case RefusalBoundExceeded:
		return errQueryBound
	default:
		return nil
	}
}

var (
	errQueryProtocol = errors.New("query protocol violation")
	errQueryPath     = errors.New("query path not admitted")
	errQueryDigest   = errors.New("query digest mismatch")
	errQuerySchema   = errors.New("query schema mismatch")
	errQueryBound    = errors.New("query bound exceeded")
)

// sanitizeReason bounds and cleans one refusal reason so a refusal never
// carries a payload or a terminal escape.
func sanitizeReason(reason string) string {
	cleaned := strings.Map(func(character rune) rune {
		if character < 0x20 || character == 0x7f {
			return ' '
		}
		return character
	}, reason)
	if len(cleaned) > 512 {
		cleaned = cleaned[:512]
		for !utf8.ValidString(cleaned) && len(cleaned) > 0 {
			cleaned = cleaned[:len(cleaned)-1]
		}
	}
	return cleaned
}

// validateQueryIdentifier refuses a name outside the closed identifier set, so
// a declared name can never carry statement text into the child.
func validateQueryIdentifier(name string) error {
	if name == "" || len(name) > MaximumQueryIdentifierBytes {
		return refusal(RefusalProtocol, "an identifier must be 1..%d bytes", MaximumQueryIdentifierBytes)
	}
	for index, character := range name {
		valid := character == '_' || (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z')
		if index > 0 && character >= '0' && character <= '9' {
			valid = true
		}
		if !valid {
			return refusal(RefusalProtocol, "identifier %q contains characters outside the supported set", name)
		}
	}
	return nil
}

func validateQueryUnscaled(unscaled string) error {
	digits := strings.TrimPrefix(unscaled, "-")
	if digits == "" || len(digits) > MaximumQueryDecimalDigits {
		return refusal(RefusalProtocol, "decimal unscaled digits are absent or too wide")
	}
	for _, character := range digits {
		if character < '0' || character > '9' {
			return refusal(RefusalProtocol, "decimal unscaled digits must be base-10")
		}
	}
	if digits == "0" {
		if strings.HasPrefix(unscaled, "-") {
			return refusal(RefusalProtocol, "decimal zero must not carry a sign")
		}
		return nil
	}
	if strings.HasPrefix(digits, "0") {
		return refusal(RefusalProtocol, "decimal unscaled digits must not be zero-padded")
	}
	return nil
}

func hexQueryValue(data []byte) string { return strings.ToUpper(hex.EncodeToString(data)) }

func base64QueryValue(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

func integerText(value int64) string { return strconv.FormatInt(value, 10) }

// QueryRelation is one admitted, read-only local Parquet input. The digest and
// byte length are the admitted identity; the child re-verifies both.
type QueryRelation struct {
	Name    string
	Path    string
	SHA256  string
	Bytes   int64
	Columns []QueryColumn
}

// Validate refuses a relation that cannot be safely admitted.
func (relation QueryRelation) Validate() error {
	if err := validateQueryIdentifier(relation.Name); err != nil {
		return err
	}
	if relation.Path == "" || !strings.HasPrefix(relation.Path, "/") || strings.ContainsAny(relation.Path, "\x00\n\r") {
		return refusal(RefusalUnadmittedPath, "relation %q must name one absolute local path", relation.Name)
	}
	if strings.TrimPrefix(relation.SHA256, "sha256:") == relation.SHA256 {
		return refusal(RefusalDigestMismatch, "relation %q must declare a sha256 digest", relation.Name)
	}
	if len(strings.TrimPrefix(relation.SHA256, "sha256:")) != 64 ||
		strings.TrimPrefix(relation.SHA256, "sha256:") != strings.ToLower(strings.TrimPrefix(relation.SHA256, "sha256:")) {
		return refusal(RefusalDigestMismatch, "relation %q must declare a lowercase sha256 digest", relation.Name)
	}
	if relation.Bytes <= 0 {
		return refusal(RefusalUnadmittedPath, "relation %q must declare the admitted byte length", relation.Name)
	}
	if len(relation.Columns) == 0 {
		return refusal(RefusalUnadmittedPath, "relation %q declares no columns", relation.Name)
	}
	for _, column := range relation.Columns {
		if err := column.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// QueryProjection is one output column. An alias only renames the same exact
// column; this protocol has no expression language.
type QueryProjection struct {
	Column string
	Alias  string
}

// QueryOperator is one closed typed comparison.
type QueryOperator string

const (
	QueryOperatorEqual        QueryOperator = "eq"
	QueryOperatorNotEqual     QueryOperator = "ne"
	QueryOperatorLessThan     QueryOperator = "lt"
	QueryOperatorLessEqual    QueryOperator = "lte"
	QueryOperatorGreaterThan  QueryOperator = "gt"
	QueryOperatorGreaterEqual QueryOperator = "gte"
	QueryOperatorIsNull       QueryOperator = "is_null"
	QueryOperatorIsNotNull    QueryOperator = "is_not_null"
	QueryOperatorIn           QueryOperator = "in"
	QueryOperatorNotIn        QueryOperator = "not_in"
)

// Valid reports whether the operator belongs to the closed set.
func (operator QueryOperator) Valid() bool {
	switch operator {
	case QueryOperatorEqual, QueryOperatorNotEqual, QueryOperatorLessThan, QueryOperatorLessEqual,
		QueryOperatorGreaterThan, QueryOperatorGreaterEqual, QueryOperatorIsNull, QueryOperatorIsNotNull,
		QueryOperatorIn, QueryOperatorNotIn:
		return true
	default:
		return false
	}
}

// QueryPredicate is one typed comparison against a declared column. A null test
// must be declared as one: an explicit null operand is refused.
type QueryPredicate struct {
	Column   string
	Operator QueryOperator
	Value    QueryValue
	Values   []QueryValue
}

// Validate refuses an incomplete or contradictory comparison.
func (predicate QueryPredicate) Validate() error {
	if err := validateQueryIdentifier(predicate.Column); err != nil {
		return err
	}
	if !predicate.Operator.Valid() {
		return refusal(RefusalProtocol, "predicate operator %q is unsupported", predicate.Operator)
	}
	switch predicate.Operator {
	case QueryOperatorIn, QueryOperatorNotIn:
		if len(predicate.Values) == 0 || len(predicate.Values) > MaximumQueryPredicateValues {
			return refusal(RefusalProtocol, "an in-list must carry 1..%d values", MaximumQueryPredicateValues)
		}
		for _, value := range predicate.Values {
			if !value.Present() || value.IsNull() {
				return refusal(RefusalProtocol, "an in-list carries an absent or explicit null value")
			}
			if err := value.Validate(); err != nil {
				return err
			}
		}
		return nil
	case QueryOperatorIsNull, QueryOperatorIsNotNull:
		if predicate.Value.Present() || len(predicate.Values) != 0 {
			return refusal(RefusalProtocol, "predicate operator %q must not carry an operand", predicate.Operator)
		}
		return nil
	default:
		if len(predicate.Values) != 0 {
			return refusal(RefusalProtocol, "predicate operator %q does not consume a value list", predicate.Operator)
		}
	}
	if !predicate.Value.Present() {
		return refusal(RefusalProtocol, "predicate operator %q requires an operand", predicate.Operator)
	}
	if predicate.Value.IsNull() {
		return refusal(RefusalProtocol, "an explicit null operand is refused; a null test must be declared as one")
	}
	return predicate.Value.Validate()
}

// QueryOrder is one declared sort key.
type QueryOrder struct {
	Column     string
	Descending bool
}

// QueryOutput is one admitted immutable Parquet destination.
type QueryOutput struct {
	Path string
}

// Validate refuses an output path that cannot be admitted.
func (output QueryOutput) Validate() error {
	if output.Path == "" || !strings.HasPrefix(output.Path, "/") || strings.ContainsAny(output.Path, "\x00\n\r") {
		return refusal(RefusalUnadmittedPath, "the output must name one absolute local path")
	}
	return nil
}

// QuerySelect is one typed relational request. It carries no statement text and
// no expression language: project, filter, order, limit, optional output.
type QuerySelect struct {
	From       string
	Project    []QueryProjection
	Predicates []QueryPredicate
	Order      []QueryOrder
	Limit      int64
	Output     *QueryOutput
}

// Validate refuses a selection that cannot be executed within the declared
// bounds.
func (selectRequest QuerySelect) Validate(relations map[string]QueryRelation, bounds QueryBounds) error {
	relation, declared := relations[selectRequest.From]
	if !declared {
		return refusal(RefusalUnknownRelation, "the query names relation %q, which this request did not declare", selectRequest.From)
	}
	if len(selectRequest.Project) == 0 || len(selectRequest.Project) > bounds.Columns {
		return refusal(RefusalBoundExceeded, "the projection must carry 1..%d columns", bounds.Columns)
	}
	if len(selectRequest.Predicates) > MaximumQueryPredicates {
		return refusal(RefusalBoundExceeded, "the request declares %d predicates beyond the supported %d",
			len(selectRequest.Predicates), MaximumQueryPredicates)
	}
	if len(selectRequest.Order) > MaximumQueryOrderings {
		return refusal(RefusalBoundExceeded, "the request declares %d orderings beyond the supported %d",
			len(selectRequest.Order), MaximumQueryOrderings)
	}
	if selectRequest.Limit <= 0 || selectRequest.Limit > MaximumQueryRows {
		return refusal(RefusalBoundExceeded, "the declared row limit must be 1..%d", MaximumQueryRows)
	}
	columns := make(map[string]QueryColumn, len(relation.Columns))
	for _, column := range relation.Columns {
		columns[column.Name] = column
	}
	for _, projection := range selectRequest.Project {
		column, declared := columns[projection.Column]
		if !declared {
			return refusal(RefusalUnknownColumn, "relation %q does not declare column %q", selectRequest.From, projection.Column)
		}
		if projection.Alias != "" {
			if err := validateQueryIdentifier(projection.Alias); err != nil {
				return err
			}
		}
		if _, err := queryKindOfColumn(column); err != nil {
			return err
		}
	}
	for _, predicate := range selectRequest.Predicates {
		if _, declared := columns[predicate.Column]; !declared {
			return refusal(RefusalUnknownColumn, "relation %q does not declare column %q", selectRequest.From, predicate.Column)
		}
		if err := predicate.Validate(); err != nil {
			return err
		}
	}
	for _, ordering := range selectRequest.Order {
		if _, declared := columns[ordering.Column]; !declared {
			return refusal(RefusalUnknownColumn, "relation %q does not declare column %q", selectRequest.From, ordering.Column)
		}
	}
	if selectRequest.Output != nil {
		return selectRequest.Output.Validate()
	}
	return nil
}

// queryKindOfColumn confirms one declared column describes a supported exact
// projection.
func queryKindOfColumn(column QueryColumn) (QueryKind, error) {
	if err := column.Validate(); err != nil {
		return "", err
	}
	return column.Kind, nil
}

// QueryEncode is one typed row batch written as an immutable Parquet segment.
type QueryEncode struct {
	Columns []QueryColumn
	Rows    [][]QueryValue
	Output  QueryOutput
}

// Validate refuses a batch that cannot be materialized within the declared
// bounds. An absent cell is refused: only an explicit null becomes SQL NULL.
func (encode QueryEncode) Validate(bounds QueryBounds) error {
	if len(encode.Columns) == 0 || len(encode.Columns) > bounds.Columns {
		return refusal(RefusalBoundExceeded, "an encoded segment must declare 1..%d columns", bounds.Columns)
	}
	if int64(len(encode.Rows)) > bounds.Rows {
		return refusal(RefusalBoundExceeded, "the encoded batch carries %d rows beyond the row bound", len(encode.Rows))
	}
	for _, column := range encode.Columns {
		if err := column.Validate(); err != nil {
			return err
		}
	}
	for index, row := range encode.Rows {
		if len(row) != len(encode.Columns) {
			return refusal(RefusalProtocol, "encoded row %d carries %d cells for %d columns", index, len(row), len(encode.Columns))
		}
		for position, value := range row {
			column := encode.Columns[position]
			if !value.Present() {
				return refusal(RefusalProtocol, "encoded row %d column %q is absent; an explicit null must be declared as one",
					index, column.Name)
			}
			if value.IsNull() {
				if !column.Nullable {
					return refusal(RefusalSchemaMismatch, "encoded row %d column %q is null in a non-nullable column", index, column.Name)
				}
				continue
			}
			if value.Kind() != column.Kind {
				return refusal(RefusalSchemaMismatch, "encoded row %d column %q carries %s for a %s column",
					index, column.Name, value.Kind(), column.Kind)
			}
			if value.Kind() == QueryKindDecimal {
				if _, scale, _ := value.Unscaled(); scale != column.Scale {
					return refusal(RefusalSchemaMismatch, "encoded row %d column %q must carry the declared scale %d",
						index, column.Name, column.Scale)
				}
			}
			if err := value.Validate(); err != nil {
				return err
			}
		}
	}
	return encode.Output.Validate()
}

// QueryBounds is the finite resource declaration one request must carry. Every
// bound is required and positive; the supervisor refuses a bound above its own
// configured capability and the child refuses one above its hard capability.
type QueryBounds struct {
	Relations   int
	Columns     int
	Rows        int64
	Bytes       int64
	TempBytes   int64
	MemoryBytes int64
	WallTime    time.Duration
}

// Validate refuses an absent, non-positive, or over-capability bound.
func (bounds QueryBounds) Validate() error {
	switch {
	case bounds.Relations <= 0:
		return refusal(RefusalBoundExceeded, "the relation bound is absent or non-positive")
	case bounds.Relations > MaximumQueryRelations:
		return refusal(RefusalBoundExceeded, "the relation bound exceeds the protocol capability of %d", MaximumQueryRelations)
	case bounds.Columns <= 0:
		return refusal(RefusalBoundExceeded, "the column bound is absent or non-positive")
	case bounds.Columns > MaximumQueryColumns:
		return refusal(RefusalBoundExceeded, "the column bound exceeds the protocol capability of %d", MaximumQueryColumns)
	case bounds.Rows <= 0:
		return refusal(RefusalBoundExceeded, "the row bound is absent or non-positive")
	case bounds.Rows > MaximumQueryRows:
		return refusal(RefusalBoundExceeded, "the row bound exceeds the protocol capability of %d", MaximumQueryRows)
	case bounds.Bytes <= 0:
		return refusal(RefusalBoundExceeded, "the byte bound is absent or non-positive")
	case bounds.Bytes > MaximumQueryBytes:
		return refusal(RefusalBoundExceeded, "the byte bound exceeds the protocol capability of %d", MaximumQueryBytes)
	case bounds.TempBytes <= 0:
		return refusal(RefusalBoundExceeded, "the temporary byte bound is absent or non-positive")
	case bounds.TempBytes > MaximumQueryTempBytes:
		return refusal(RefusalBoundExceeded, "the temporary byte bound exceeds the protocol capability of %d", MaximumQueryTempBytes)
	case bounds.MemoryBytes <= 0:
		return refusal(RefusalBoundExceeded, "the memory byte bound is absent or non-positive")
	case bounds.MemoryBytes > MaximumQueryMemoryBytes:
		return refusal(RefusalBoundExceeded, "the memory byte bound exceeds the protocol capability of %d", MaximumQueryMemoryBytes)
	case bounds.WallTime <= 0:
		return refusal(RefusalBoundExceeded, "the wall-time bound is absent or non-positive")
	case bounds.WallTime > MaximumQueryWallTime:
		return refusal(RefusalBoundExceeded, "the wall-time bound exceeds the protocol capability of %s", MaximumQueryWallTime)
	}
	return nil
}

// QueryRequest is the one bounded typed request the parent sends.
type QueryRequest struct {
	RequestID           string
	AdmittedDirectories []string
	Bounds              QueryBounds
	Relations           []QueryRelation
	Select              *QuerySelect
	Encode              *QueryEncode
}

// Validate refuses an incomplete, contradictory, or unadmitted request before
// any child process exists.
func (request QueryRequest) Validate() error {
	if request.RequestID == "" || len(request.RequestID) > 128 || strings.ContainsAny(request.RequestID, "\x00\n\r") {
		return refusal(RefusalProtocol, "the request identity must be a bounded nonempty value")
	}
	if err := request.Bounds.Validate(); err != nil {
		return err
	}
	if len(request.AdmittedDirectories) == 0 || len(request.AdmittedDirectories) > MaximumQueryAdmittedDirectories {
		return refusal(RefusalUnadmittedPath, "the request must declare 1..%d admitted directories", MaximumQueryAdmittedDirectories)
	}
	for _, directory := range request.AdmittedDirectories {
		if !strings.HasPrefix(directory, "/") || strings.ContainsAny(directory, "\x00\n\r") {
			return refusal(RefusalUnadmittedPath, "an admitted directory must be one absolute local path")
		}
	}
	if (request.Select == nil) == (request.Encode == nil) {
		return refusal(RefusalProtocol, "exactly one of the typed select and encode operations must be present")
	}
	if len(request.Relations) > request.Bounds.Relations {
		return refusal(RefusalBoundExceeded, "the request declares %d relations beyond its bound of %d",
			len(request.Relations), request.Bounds.Relations)
	}
	relations := make(map[string]QueryRelation, len(request.Relations))
	for _, relation := range request.Relations {
		if err := relation.Validate(); err != nil {
			return err
		}
		if _, duplicate := relations[relation.Name]; duplicate {
			return refusal(RefusalProtocol, "relation %q is declared more than once", relation.Name)
		}
		if len(relation.Columns) > request.Bounds.Columns {
			return refusal(RefusalBoundExceeded, "relation %q declares %d columns beyond the column bound",
				relation.Name, len(relation.Columns))
		}
		if relation.Bytes > request.Bounds.Bytes {
			return refusal(RefusalBoundExceeded, "relation %q declares %d bytes beyond the byte bound", relation.Name, relation.Bytes)
		}
		relations[relation.Name] = relation
	}
	if request.Select != nil {
		return request.Select.Validate(relations, request.Bounds)
	}
	return request.Encode.Validate(request.Bounds)
}

// QueryOutputDescriptor is the verified identity of one materialized segment.
type QueryOutputDescriptor struct {
	Path   string
	SHA256 string
	Bytes  int64
}

// ChildDescriptor is one descriptor the child still holds, with its file type.
type ChildDescriptor struct {
	Descriptor int
	Kind       string
}

// ChildReport is the child's own account of its isolation posture: its process
// identity, its environment names (never values), the inherited descriptors it
// closed, and its private temporary directory.
type ChildReport struct {
	PID                  int
	Environment          []string
	InheritedDescriptors []int
	ClosedDescriptors    []int
	UnclosedDescriptors  []int
	RemainingDescriptors []ChildDescriptor
	TemporaryDirectory   string
}

// QueryResponse is one bounded typed outcome. A refusal never carries rows.
type QueryResponse struct {
	RequestID string
	Columns   []QueryColumn
	Rows      [][]QueryValue
	Truncated bool
	Output    *QueryOutputDescriptor
	Refusal   *Refusal
	Child     *ChildReport
}

// queryValueDocument is the closed member set of one encoded value. Contradictory
// or missing payload members are refused, and contract's strict decoder refuses
// unknown, duplicated, null, or wrongly cased members.
type queryValueDocument struct {
	Kind     string          `json:"kind"`
	Value    json.RawMessage `json:"value,omitempty"`
	Unscaled *string         `json:"unscaled,omitempty"`
	Scale    *string         `json:"scale,omitempty"`
	Base64   *string         `json:"base64,omitempty"`
	Micros   *string         `json:"micros,omitempty"`
}

// MarshalJSON renders one value through the exact wire encoding.
func (value QueryValue) MarshalJSON() ([]byte, error) { return encodeQueryValue(value) }

// UnmarshalJSON decodes one value through the exact wire encoding.
func (value *QueryValue) UnmarshalJSON(document []byte) error {
	decoded, err := decodeQueryValue(document)
	if err != nil {
		return err
	}
	*value = decoded
	return nil
}

// encodeQueryValue renders one value into the protocol's exact wire form. Every
// integral payload travels as a decimal string, and an explicit null is written
// as its own member rather than omitted.
func encodeQueryValue(value QueryValue) (json.RawMessage, error) {
	if err := value.Validate(); err != nil {
		return nil, err
	}
	var wire map[string]any
	switch {
	case !value.present:
		return nil, refusal(RefusalProtocol, "an absent value has no wire form")
	case value.null:
		wire = map[string]any{"kind": "null"}
	default:
		switch value.kind {
		case QueryKindInt64:
			wire = map[string]any{"kind": "int64", "value": integerText(value.integer)}
		case QueryKindDecimal:
			wire = map[string]any{"kind": "decimal", "unscaled": value.unscaled, "scale": integerText(int64(value.scale))}
		case QueryKindText:
			wire = map[string]any{"kind": "text", "value": value.text}
		case QueryKindBoolean:
			wire = map[string]any{"kind": "boolean", "value": value.boolean}
		case QueryKindBytea:
			wire = map[string]any{"kind": "bytea", "base64": base64QueryValue(value.binary)}
		case QueryKindDate:
			wire = map[string]any{"kind": "date", "value": value.text}
		case QueryKindTimestamp:
			wire = map[string]any{"kind": "timestamp", "micros": integerText(value.integer)}
		case QueryKindTimestampTZ:
			wire = map[string]any{"kind": "timestamptz", "micros": integerText(value.integer)}
		case QueryKindUUID:
			wire = map[string]any{"kind": "uuid", "value": value.text}
		case QueryKindJSON:
			wire = map[string]any{"kind": "json", "value": value.text}
		default:
			return nil, refusal(RefusalProtocol, "the value kind is unsupported")
		}
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, refusal(RefusalProtocol, "encode the typed value")
	}
	return encoded, nil
}

// decodeQueryValue decodes exactly one encoded value. A missing payload member
// is refused, and presence is explicit: the `null` kind is an SQL NULL.
func decodeQueryValue(document []byte) (QueryValue, error) {
	var wire queryValueDocument
	if err := contract.DecodeStrictRequiredObject(document, &wire, "kind"); err != nil {
		return QueryValue{}, refusal(RefusalProtocol, "the typed value is not a valid encoding")
	}
	present := wire.payloadMembers()
	exact := func(wanted string) error {
		if !present[wanted] {
			return refusal(RefusalProtocol, "the typed value is missing its %s member", wanted)
		}
		return exactPayloadMember(present, wanted)
	}
	switch wire.Kind {
	case "null":
		if anyPayloadMember(present) {
			return QueryValue{}, refusal(RefusalProtocol, "an explicit null carries a payload")
		}
		return NullQueryValue(), nil
	case string(QueryKindInt64):
		if err := exact("value"); err != nil {
			return QueryValue{}, err
		}
		parsed, err := canonicalInteger(wire.Value)
		if err != nil {
			return QueryValue{}, err
		}
		return Int64QueryValue(parsed), nil
	case string(QueryKindDecimal):
		if !present["unscaled"] || !present["scale"] {
			return QueryValue{}, refusal(RefusalProtocol, "a decimal requires exactly one unscaled and one scale member")
		}
		for name, isPresent := range present {
			if name != "unscaled" && name != "scale" && isPresent {
				return QueryValue{}, refusal(RefusalProtocol, "the decimal carries contradictory payload members")
			}
		}
		scale, err := canonicalIntegerText(*wire.Scale)
		if err != nil {
			return QueryValue{}, err
		}
		return DecimalQueryValue(*wire.Unscaled, int(scale))
	case string(QueryKindText):
		if err := exact("value"); err != nil {
			return QueryValue{}, err
		}
		text, err := stringMember(wire.Value)
		if err != nil {
			return QueryValue{}, err
		}
		value := TextQueryValue(text)
		if err := value.Validate(); err != nil {
			return QueryValue{}, err
		}
		return value, nil
	case string(QueryKindBoolean):
		if err := exact("value"); err != nil {
			return QueryValue{}, err
		}
		var boolean bool
		if err := json.Unmarshal(wire.Value, &boolean); err != nil {
			return QueryValue{}, refusal(RefusalProtocol, "a boolean value must be a JSON boolean")
		}
		return BooleanQueryValue(boolean), nil
	case string(QueryKindBytea):
		if err := exact("base64"); err != nil {
			return QueryValue{}, err
		}
		decoded, err := base64.StdEncoding.DecodeString(*wire.Base64)
		if err != nil || base64QueryValue(decoded) != *wire.Base64 {
			return QueryValue{}, refusal(RefusalProtocol, "bytea must be canonical standard base64")
		}
		return BytesQueryValue(decoded), nil
	case string(QueryKindDate):
		if err := exact("value"); err != nil {
			return QueryValue{}, err
		}
		text, err := stringMember(wire.Value)
		if err != nil {
			return QueryValue{}, err
		}
		return DateQueryValue(text)
	case string(QueryKindTimestamp), string(QueryKindTimestampTZ):
		if err := exact("micros"); err != nil {
			return QueryValue{}, err
		}
		micros, err := canonicalIntegerText(*wire.Micros)
		if err != nil {
			return QueryValue{}, err
		}
		if wire.Kind == string(QueryKindTimestamp) {
			return TimestampQueryValue(micros), nil
		}
		return TimestampTZQueryValue(micros), nil
	case string(QueryKindUUID):
		if err := exact("value"); err != nil {
			return QueryValue{}, err
		}
		text, err := stringMember(wire.Value)
		if err != nil {
			return QueryValue{}, err
		}
		return UUIDQueryValue(text)
	case string(QueryKindJSON):
		if err := exact("value"); err != nil {
			return QueryValue{}, err
		}
		text, err := stringMember(wire.Value)
		if err != nil {
			return QueryValue{}, err
		}
		return JSONQueryValue(text)
	default:
		return QueryValue{}, refusal(RefusalProtocol, "the value kind is unsupported")
	}
}

// anyPayloadMember reports whether any payload member is present.
func anyPayloadMember(members map[string]bool) bool {
	for _, isPresent := range members {
		if isPresent {
			return true
		}
	}
	return false
}

// exactPayloadMember refuses a payload member outside the one expected.
func exactPayloadMember(members map[string]bool, wanted string) error {
	for name, isPresent := range members {
		if name != wanted && isPresent {
			return refusal(RefusalProtocol, "the typed value carries contradictory payload members")
		}
	}
	return nil
}

func (wire queryValueDocument) payloadMembers() map[string]bool {
	return map[string]bool{
		"value":    wire.Value != nil,
		"unscaled": wire.Unscaled != nil,
		"scale":    wire.Scale != nil,
		"base64":   wire.Base64 != nil,
		"micros":   wire.Micros != nil,
	}
}

func stringMember(payload json.RawMessage) (string, error) {
	if payload == nil {
		return "", refusal(RefusalProtocol, "the value member is absent")
	}
	var text string
	if err := json.Unmarshal(payload, &text); err != nil {
		return "", refusal(RefusalProtocol, "the value member must be a JSON string")
	}
	return text, nil
}

func canonicalInteger(payload json.RawMessage) (int64, error) {
	text, err := stringMember(payload)
	if err != nil {
		return 0, err
	}
	return canonicalIntegerText(text)
}

// canonicalIntegerText accepts only a canonical base-10 rendering, so a
// zero-padded or exponent form is refused instead of coerced.
func canonicalIntegerText(text string) (int64, error) {
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil || integerText(parsed) != text {
		return 0, refusal(RefusalProtocol, "an integral member must be a canonical base-10 integer string")
	}
	return parsed, nil
}

// The wire documents below are the exact counterpart of the child's own
// schema; the child decodes them with its own strict decoder and never imports
// this package.
type queryColumnDocument struct {
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	Precision *string `json:"precision,omitempty"`
	Scale     *string `json:"scale,omitempty"`
	Nullable  bool    `json:"nullable"`
}

func newQueryColumnDocument(column QueryColumn) queryColumnDocument {
	document := queryColumnDocument{Name: column.Name, Kind: string(column.Kind), Nullable: column.Nullable}
	if column.Kind == QueryKindDecimal {
		precision := integerText(int64(column.Precision))
		scale := integerText(int64(column.Scale))
		document.Precision = &precision
		document.Scale = &scale
	}
	return document
}

func (document queryColumnDocument) column() (QueryColumn, error) {
	column := QueryColumn{Name: document.Name, Kind: QueryKind(document.Kind), Nullable: document.Nullable}
	if document.Precision != nil {
		precision, err := canonicalIntegerText(*document.Precision)
		if err != nil {
			return column, err
		}
		column.Precision = int(precision)
	}
	if document.Scale != nil {
		scale, err := canonicalIntegerText(*document.Scale)
		if err != nil {
			return column, err
		}
		column.Scale = int(scale)
	}
	if err := column.Validate(); err != nil {
		return column, err
	}
	return column, nil
}

type queryOutputDocument struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
	Bytes  string `json:"bytes,omitempty"`
}

type queryRelationDocument struct {
	Name    string                `json:"name"`
	Path    string                `json:"path"`
	SHA256  string                `json:"sha256"`
	Bytes   string                `json:"bytes"`
	Columns []queryColumnDocument `json:"columns"`
}

type queryProjectionDocument struct {
	Column string `json:"column"`
	Alias  string `json:"alias,omitempty"`
}

type queryPredicateDocument struct {
	Column   string       `json:"column"`
	Operator string       `json:"operator"`
	Value    *QueryValue  `json:"value,omitempty"`
	Values   []QueryValue `json:"values,omitempty"`
}

type queryOrderDocument struct {
	Column    string `json:"column"`
	Direction string `json:"direction"`
}

type querySelectDocument struct {
	From       string                    `json:"from"`
	Project    []queryProjectionDocument `json:"project"`
	Predicates []queryPredicateDocument  `json:"predicates,omitempty"`
	Order      []queryOrderDocument      `json:"order,omitempty"`
	Limit      string                    `json:"limit"`
	Output     *queryOutputDocument      `json:"output,omitempty"`
}

type queryEncodeDocument struct {
	Columns []queryColumnDocument `json:"columns"`
	Rows    [][]QueryValue        `json:"rows"`
	Output  queryOutputDocument   `json:"output"`
}

type queryBoundsDocument struct {
	Relations   string `json:"relations"`
	Columns     string `json:"columns"`
	Rows        string `json:"rows"`
	Bytes       string `json:"bytes"`
	TempBytes   string `json:"temp_bytes"`
	MemoryBytes string `json:"memory_bytes"`
	WallMillis  string `json:"wall_millis"`
}

type queryRequestDocument struct {
	Protocol            string                  `json:"protocol"`
	RequestID           string                  `json:"request_id"`
	AdmittedDirectories []string                `json:"admitted_directories"`
	Bounds              queryBoundsDocument     `json:"bounds"`
	Relations           []queryRelationDocument `json:"relations"`
	Select              *querySelectDocument    `json:"select,omitempty"`
	Encode              *queryEncodeDocument    `json:"encode,omitempty"`
}

type queryRefusalDocument struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

type queryDescriptorDocument struct {
	Descriptor string `json:"descriptor"`
	Kind       string `json:"kind"`
}

type queryChildDocument struct {
	PID                  string                    `json:"pid"`
	Environment          []string                  `json:"environment"`
	InheritedDescriptors []string                  `json:"inherited_descriptors"`
	ClosedDescriptors    []string                  `json:"closed_descriptors"`
	UnclosedDescriptors  []string                  `json:"unclosed_descriptors"`
	RemainingDescriptors []queryDescriptorDocument `json:"remaining_descriptors"`
	TemporaryDirectory   string                    `json:"temporary_directory"`
}

type queryResponseDocument struct {
	Protocol  string                 `json:"protocol"`
	RequestID string                 `json:"request_id"`
	Columns   *[]queryColumnDocument `json:"columns,omitempty"`
	Rows      *[][]QueryValue        `json:"rows,omitempty"`
	Truncated *bool                  `json:"truncated,omitempty"`
	Output    *queryOutputDocument   `json:"output,omitempty"`
	Refusal   *queryRefusalDocument  `json:"refusal,omitempty"`
	Child     queryChildDocument     `json:"child"`
}

// EncodeQueryRequest renders one validated request into the protocol's exact
// wire form. It refuses an invalid request, so a caller never sends a document
// the child would reject for a structural reason.
func EncodeQueryRequest(request QueryRequest) ([]byte, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return renderQueryRequest(request)
}

// renderQueryRequest renders one request without applying the parent's own
// admission checks. EncodeQueryRequest always validates first; the unvalidated
// renderer exists so the isolation case can act as a hostile counterparty and
// observe the child's independent guards.
func renderQueryRequest(request QueryRequest) ([]byte, error) {
	document := queryRequestDocument{
		Protocol:            QueryProtocolVersion,
		RequestID:           request.RequestID,
		AdmittedDirectories: request.AdmittedDirectories,
		Bounds: queryBoundsDocument{
			Relations:   integerText(int64(request.Bounds.Relations)),
			Columns:     integerText(int64(request.Bounds.Columns)),
			Rows:        integerText(request.Bounds.Rows),
			Bytes:       integerText(request.Bounds.Bytes),
			TempBytes:   integerText(request.Bounds.TempBytes),
			MemoryBytes: integerText(request.Bounds.MemoryBytes),
			WallMillis:  integerText(request.Bounds.WallTime.Milliseconds()),
		},
		Relations: make([]queryRelationDocument, 0, len(request.Relations)),
	}
	if document.AdmittedDirectories == nil {
		document.AdmittedDirectories = []string{}
	}
	for _, relation := range request.Relations {
		entry := queryRelationDocument{
			Name:    relation.Name,
			Path:    relation.Path,
			SHA256:  relation.SHA256,
			Bytes:   integerText(relation.Bytes),
			Columns: make([]queryColumnDocument, 0, len(relation.Columns)),
		}
		for _, column := range relation.Columns {
			entry.Columns = append(entry.Columns, newQueryColumnDocument(column))
		}
		document.Relations = append(document.Relations, entry)
	}
	if request.Select != nil {
		selected := &querySelectDocument{
			From:       request.Select.From,
			Limit:      integerText(request.Select.Limit),
			Project:    make([]queryProjectionDocument, 0, len(request.Select.Project)),
			Order:      make([]queryOrderDocument, 0, len(request.Select.Order)),
			Predicates: make([]queryPredicateDocument, 0, len(request.Select.Predicates)),
		}
		for _, projection := range request.Select.Project {
			selected.Project = append(selected.Project, queryProjectionDocument{Column: projection.Column, Alias: projection.Alias})
		}
		for _, predicate := range request.Select.Predicates {
			entry := queryPredicateDocument{Column: predicate.Column, Operator: string(predicate.Operator)}
			if predicate.Value.Present() {
				value := predicate.Value
				entry.Value = &value
			}
			entry.Values = append(entry.Values, predicate.Values...)
			selected.Predicates = append(selected.Predicates, entry)
		}
		for _, ordering := range request.Select.Order {
			direction := "asc"
			if ordering.Descending {
				direction = "desc"
			}
			selected.Order = append(selected.Order, queryOrderDocument{Column: ordering.Column, Direction: direction})
		}
		if request.Select.Output != nil {
			selected.Output = &queryOutputDocument{Path: request.Select.Output.Path}
		}
		document.Select = selected
	}
	if request.Encode != nil {
		rows := make([][]QueryValue, 0, len(request.Encode.Rows))
		rows = append(rows, request.Encode.Rows...)
		encoded := &queryEncodeDocument{
			Columns: make([]queryColumnDocument, 0, len(request.Encode.Columns)),
			Rows:    rows,
			Output:  queryOutputDocument{Path: request.Encode.Output.Path},
		}
		for _, column := range request.Encode.Columns {
			encoded.Columns = append(encoded.Columns, newQueryColumnDocument(column))
		}
		document.Encode = encoded
	}
	rendered, err := json.Marshal(document)
	if err != nil {
		return nil, refusal(RefusalProtocol, "encode the typed request document")
	}
	if len(rendered) > MaxQueryDocumentBytes {
		return nil, refusal(RefusalBoundExceeded, "the encoded request document exceeds the protocol byte bound")
	}
	return rendered, nil
}

// DecodeQueryRequest strictly decodes one request document. Unknown,
// duplicated, absent, wrongly typed, or wrongly cased members, trailing data,
// and an oversized document are refused.
func DecodeQueryRequest(document []byte) (QueryRequest, error) {
	var request QueryRequest
	if len(document) == 0 || len(document) > MaxQueryDocumentBytes {
		return request, refusal(RefusalProtocol, "the request document is empty or beyond the protocol byte bound")
	}
	var wire queryRequestDocument
	if err := contract.DecodeStrictRequiredObject(document, &wire,
		"protocol", "request_id", "admitted_directories", "bounds", "relations"); err != nil {
		return request, refusal(RefusalProtocol, "the request document is not a valid %s request", QueryProtocolVersion)
	}
	if wire.Protocol != QueryProtocolVersion {
		return request, refusal(RefusalProtocol, "the request declares an unsupported protocol version")
	}
	request.RequestID = wire.RequestID
	request.AdmittedDirectories = append(request.AdmittedDirectories, wire.AdmittedDirectories...)
	bounds, err := wire.Bounds.bounds()
	if err != nil {
		return request, err
	}
	request.Bounds = bounds
	for _, entry := range wire.Relations {
		length, err := canonicalIntegerText(entry.Bytes)
		if err != nil {
			return request, err
		}
		relation := QueryRelation{Name: entry.Name, Path: entry.Path, SHA256: entry.SHA256, Bytes: length}
		for _, column := range entry.Columns {
			decoded, err := column.column()
			if err != nil {
				return request, err
			}
			relation.Columns = append(relation.Columns, decoded)
		}
		request.Relations = append(request.Relations, relation)
	}
	if (wire.Select == nil) == (wire.Encode == nil) {
		return request, refusal(RefusalProtocol, "exactly one of the typed select and encode operations must be present")
	}
	if wire.Select != nil {
		selected := &QuerySelect{From: wire.Select.From}
		limit, err := canonicalIntegerText(wire.Select.Limit)
		if err != nil {
			return request, err
		}
		selected.Limit = limit
		for _, projection := range wire.Select.Project {
			selected.Project = append(selected.Project, QueryProjection{Column: projection.Column, Alias: projection.Alias})
		}
		for _, predicate := range wire.Select.Predicates {
			entry := QueryPredicate{Column: predicate.Column, Operator: QueryOperator(predicate.Operator)}
			if predicate.Value != nil {
				entry.Value = *predicate.Value
			}
			entry.Values = append(entry.Values, predicate.Values...)
			selected.Predicates = append(selected.Predicates, entry)
		}
		for _, ordering := range wire.Select.Order {
			switch ordering.Direction {
			case "asc":
				selected.Order = append(selected.Order, QueryOrder{Column: ordering.Column})
			case "desc":
				selected.Order = append(selected.Order, QueryOrder{Column: ordering.Column, Descending: true})
			default:
				return request, refusal(RefusalProtocol, "an ordering direction must be asc or desc")
			}
		}
		if wire.Select.Output != nil {
			selected.Output = &QueryOutput{Path: wire.Select.Output.Path}
		}
		request.Select = selected
	} else {
		encoded := &QueryEncode{Output: QueryOutput{Path: wire.Encode.Output.Path}}
		for _, column := range wire.Encode.Columns {
			decoded, err := column.column()
			if err != nil {
				return request, err
			}
			encoded.Columns = append(encoded.Columns, decoded)
		}
		encoded.Rows = append(encoded.Rows, wire.Encode.Rows...)
		request.Encode = encoded
	}
	if err := request.Validate(); err != nil {
		return request, err
	}
	return request, nil
}

func (document queryBoundsDocument) bounds() (QueryBounds, error) {
	var bounds QueryBounds
	relations, err := canonicalIntegerText(document.Relations)
	if err != nil {
		return bounds, err
	}
	columns, err := canonicalIntegerText(document.Columns)
	if err != nil {
		return bounds, err
	}
	rows, err := canonicalIntegerText(document.Rows)
	if err != nil {
		return bounds, err
	}
	bytesBound, err := canonicalIntegerText(document.Bytes)
	if err != nil {
		return bounds, err
	}
	tempBytes, err := canonicalIntegerText(document.TempBytes)
	if err != nil {
		return bounds, err
	}
	memoryBytes, err := canonicalIntegerText(document.MemoryBytes)
	if err != nil {
		return bounds, err
	}
	wallMillis, err := canonicalIntegerText(document.WallMillis)
	if err != nil {
		return bounds, err
	}
	// The declared bound is range-checked before the conversion: the nanosecond
	// multiplication wraps for a value beyond the capability, and a wrapped
	// value lands inside the accepted range where the documented refusal can
	// never fire.
	switch {
	case wallMillis > maximumQueryWallMillis:
		return bounds, refusal(RefusalBoundExceeded, "the wall-time bound exceeds the protocol capability of %s", MaximumQueryWallTime)
	case wallMillis < -maximumQueryWallMillis:
		return bounds, refusal(RefusalBoundExceeded, "the wall-time bound is beyond the protocol capability of %s", MaximumQueryWallTime)
	}
	return QueryBounds{
		Relations:   int(relations),
		Columns:     int(columns),
		Rows:        rows,
		Bytes:       bytesBound,
		TempBytes:   tempBytes,
		MemoryBytes: memoryBytes,
		WallTime:    time.Duration(wallMillis) * time.Millisecond,
	}, nil
}

// EncodeQueryResponse renders one validated response into the protocol's exact
// wire form. The parent codec owns both directions so an in-process producer
// never hand-builds a document. The restricted child implements the same schema
// independently, because it may not import this package.
func EncodeQueryResponse(response QueryResponse) ([]byte, error) {
	document := queryResponseDocument{
		Protocol:  QueryProtocolVersion,
		RequestID: response.RequestID,
		Child:     newQueryChildDocument(response.Child),
	}
	if response.Refusal != nil {
		if !response.Refusal.Code.Valid() {
			return nil, refusal(RefusalProtocol, "the refusal code is outside the published vocabulary")
		}
		document.Refusal = &queryRefusalDocument{Code: string(response.Refusal.Code), Reason: sanitizeReason(response.Refusal.Reason)}
	} else {
		if len(response.Columns) == 0 {
			return nil, refusal(RefusalProtocol, "a successful response must declare its columns")
		}
		truncated := response.Truncated
		document.Truncated = &truncated
		columns := make([]queryColumnDocument, 0, len(response.Columns))
		for _, column := range response.Columns {
			if err := column.Validate(); err != nil {
				return nil, err
			}
			columns = append(columns, newQueryColumnDocument(column))
		}
		document.Columns = &columns
		rows := make([][]QueryValue, 0, len(response.Rows))
		for index, row := range response.Rows {
			if len(row) != len(response.Columns) {
				return nil, refusal(RefusalProtocol, "row %d carries %d cells for %d columns", index, len(row), len(response.Columns))
			}
			for position, value := range row {
				if !value.Present() {
					return nil, refusal(RefusalProtocol, "row %d column %q is absent; an explicit null must be declared as one",
						index, response.Columns[position].Name)
				}
				if err := value.Validate(); err != nil {
					return nil, err
				}
			}
			rows = append(rows, row)
		}
		document.Rows = &rows
		if response.Output != nil {
			document.Output = &queryOutputDocument{
				Path:   response.Output.Path,
				SHA256: response.Output.SHA256,
				Bytes:  integerText(response.Output.Bytes),
			}
		}
	}
	rendered, err := json.Marshal(document)
	if err != nil {
		return nil, refusal(RefusalProtocol, "encode the typed response document")
	}
	if len(rendered) > MaxQueryDocumentBytes {
		return nil, refusal(RefusalBoundExceeded, "the encoded response document exceeds the protocol byte bound")
	}
	return rendered, nil
}

// DecodeQueryResponse strictly decodes one response document. A refusal
// document decodes into a child-origin Refusal error and never into rows.
func DecodeQueryResponse(document []byte) (QueryResponse, error) {
	var response QueryResponse
	if len(document) == 0 || len(document) > MaxQueryDocumentBytes {
		return response, refusal(RefusalProtocol, "the response document is empty or beyond the protocol byte bound")
	}
	var wire queryResponseDocument
	if err := contract.DecodeStrictRequiredObject(document, &wire, "protocol", "request_id", "child"); err != nil {
		return response, refusal(RefusalProtocol, "the response document is not a valid %s response: %v", QueryProtocolVersion, err)
	}
	if wire.Protocol != QueryProtocolVersion {
		return response, refusal(RefusalProtocol, "the response declares an unsupported protocol version")
	}
	child, err := wire.Child.report()
	if err != nil {
		return response, err
	}
	response.RequestID = wire.RequestID
	response.Child = child
	if wire.Refusal != nil {
		if wire.Columns != nil || wire.Rows != nil || wire.Truncated != nil || wire.Output != nil {
			return response, refusal(RefusalProtocol, "a refusal document carries rows or an output")
		}
		if !RefusalCode(wire.Refusal.Code).Valid() {
			return response, refusal(RefusalProtocol, "the child reported a refusal code outside the published vocabulary")
		}
		failure := &Refusal{
			Code:     RefusalCode(wire.Refusal.Code),
			Origin:   RefusalOriginChild,
			Reason:   sanitizeReason(wire.Refusal.Reason),
			ExitCode: -1,
			Child:    child,
		}
		if len(child.UnclosedDescriptors) != 0 {
			return response, &Refusal{
				Code:     RefusalDescriptorLeak,
				Origin:   RefusalOriginChild,
				Reason:   "the child could not close every declared inherited descriptor",
				ExitCode: -1,
				Child:    child,
			}
		}
		return response, failure
	}
	if wire.Columns == nil || wire.Rows == nil || wire.Truncated == nil {
		return response, refusal(RefusalProtocol, "a successful response must declare its columns, rows, and truncation flag")
	}
	for _, column := range *wire.Columns {
		decoded, err := column.column()
		if err != nil {
			return response, err
		}
		response.Columns = append(response.Columns, decoded)
	}
	if len(response.Columns) == 0 {
		return response, refusal(RefusalProtocol, "a successful response must declare at least one column")
	}
	for index, row := range *wire.Rows {
		if len(row) != len(response.Columns) {
			return response, refusal(RefusalProtocol, "row %d carries %d cells for %d columns", index, len(row), len(response.Columns))
		}
		for position, value := range row {
			if !value.Present() {
				return response, refusal(RefusalProtocol, "row %d column %q is absent; an explicit null must be declared as one",
					index, response.Columns[position].Name)
			}
			if err := value.Validate(); err != nil {
				return response, err
			}
			if value.Kind() != "" && value.Kind() != response.Columns[position].Kind {
				return response, refusal(RefusalProtocol, "row %d column %q carries %s for a %s column",
					index, response.Columns[position].Name, value.Kind(), response.Columns[position].Kind)
			}
		}
		response.Rows = append(response.Rows, row)
	}
	response.Truncated = *wire.Truncated
	if wire.Output != nil {
		length, err := canonicalIntegerText(wire.Output.Bytes)
		if err != nil {
			return response, err
		}
		response.Output = &QueryOutputDescriptor{Path: wire.Output.Path, SHA256: wire.Output.SHA256, Bytes: length}
	}
	if len(child.UnclosedDescriptors) != 0 {
		return response, &Refusal{
			Code:     RefusalDescriptorLeak,
			Origin:   RefusalOriginChild,
			Reason:   "the child could not close every declared inherited descriptor",
			ExitCode: -1,
			Child:    child,
		}
	}
	return response, nil
}

func newQueryChildDocument(report *ChildReport) queryChildDocument {
	if report == nil {
		return queryChildDocument{
			PID:                  "0",
			Environment:          []string{},
			InheritedDescriptors: []string{},
			ClosedDescriptors:    []string{},
			UnclosedDescriptors:  []string{},
			RemainingDescriptors: []queryDescriptorDocument{},
		}
	}
	document := queryChildDocument{
		PID:                  integerText(int64(report.PID)),
		Environment:          report.Environment,
		InheritedDescriptors: descriptorTexts(report.InheritedDescriptors),
		ClosedDescriptors:    descriptorTexts(report.ClosedDescriptors),
		UnclosedDescriptors:  descriptorTexts(report.UnclosedDescriptors),
		TemporaryDirectory:   report.TemporaryDirectory,
	}
	for _, descriptor := range report.RemainingDescriptors {
		document.RemainingDescriptors = append(document.RemainingDescriptors, queryDescriptorDocument{
			Descriptor: integerText(int64(descriptor.Descriptor)),
			Kind:       descriptor.Kind,
		})
	}
	return document
}

func (document queryChildDocument) report() (*ChildReport, error) {
	pid, err := canonicalIntegerText(document.PID)
	if err != nil {
		return nil, err
	}
	inherited, err := descriptorNumbers(document.InheritedDescriptors)
	if err != nil {
		return nil, err
	}
	closed, err := descriptorNumbers(document.ClosedDescriptors)
	if err != nil {
		return nil, err
	}
	unclosed, err := descriptorNumbers(document.UnclosedDescriptors)
	if err != nil {
		return nil, err
	}
	report := &ChildReport{
		PID:                  int(pid),
		Environment:          document.Environment,
		InheritedDescriptors: inherited,
		ClosedDescriptors:    closed,
		UnclosedDescriptors:  unclosed,
		TemporaryDirectory:   document.TemporaryDirectory,
	}
	for _, descriptor := range document.RemainingDescriptors {
		value, err := canonicalIntegerText(descriptor.Descriptor)
		if err != nil {
			return nil, err
		}
		report.RemainingDescriptors = append(report.RemainingDescriptors, ChildDescriptor{Descriptor: int(value), Kind: descriptor.Kind})
	}
	return report, nil
}

func descriptorNumbers(values []string) ([]int, error) {
	descriptors := make([]int, 0, len(values))
	for _, value := range values {
		descriptor, err := canonicalIntegerText(value)
		if err != nil {
			return nil, err
		}
		descriptors = append(descriptors, int(descriptor))
	}
	return descriptors, nil
}

func descriptorTexts(descriptors []int) []string {
	values := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		values = append(values, integerText(int64(descriptor)))
	}
	return values
}
