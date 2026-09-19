// Package analytics is the only package that imports the native analytical
// engine. Everything here runs inside the restricted pm-query child: it opens a
// private in-memory engine, reads exactly the admitted local Parquet files the
// request names, executes one typed request, and projects exact typed values.
// No module outside this package links the engine, and this package never sees
// provider, object-store, or PostgreSQL credentials.
package analytics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "github.com/duckdb/duckdb-go/v2"
)

// encodedRelationName cannot collide with a declared relation name: declared
// identifiers never contain a hyphen.
const encodedRelationName = "pm-query-encoded"

// encodedInsertBatchRows bounds one bind statement so a large encoded relation
// never becomes one unbounded statement.
const encodedInsertBatchRows = 128

// PredicateOperator is one closed typed comparison.
type PredicateOperator string

const (
	OperatorEqual        PredicateOperator = "eq"
	OperatorNotEqual     PredicateOperator = "ne"
	OperatorLessThan     PredicateOperator = "lt"
	OperatorLessEqual    PredicateOperator = "lte"
	OperatorGreaterThan  PredicateOperator = "gt"
	OperatorGreaterEqual PredicateOperator = "gte"
	OperatorIsNull       PredicateOperator = "is_null"
	OperatorIsNotNull    PredicateOperator = "is_not_null"
	OperatorIn           PredicateOperator = "in"
	OperatorNotIn        PredicateOperator = "not_in"
)

// Valid reports whether the operator belongs to the closed set.
func (operator PredicateOperator) Valid() bool {
	switch operator {
	case OperatorEqual, OperatorNotEqual, OperatorLessThan, OperatorLessEqual,
		OperatorGreaterThan, OperatorGreaterEqual, OperatorIsNull, OperatorIsNotNull,
		OperatorIn, OperatorNotIn:
		return true
	default:
		return false
	}
}

// requiresOperand reports whether the operator consumes exactly one typed value
// or a bounded value list.
func (operator PredicateOperator) requiresOperand() bool {
	switch operator {
	case OperatorIsNull, OperatorIsNotNull:
		return false
	default:
		return true
	}
}

// operatorSQL renders the closed comparison. No operator text from a request
// ever reaches a statement.
func (operator PredicateOperator) operatorSQL() (string, error) {
	switch operator {
	case OperatorEqual:
		return "=", nil
	case OperatorNotEqual:
		return "<>", nil
	case OperatorLessThan:
		return "<", nil
	case OperatorLessEqual:
		return "<=", nil
	case OperatorGreaterThan:
		return ">", nil
	case OperatorGreaterEqual:
		return ">=", nil
	case OperatorIn:
		return "IN", nil
	case OperatorNotIn:
		return "NOT IN", nil
	default:
		return "", fmt.Errorf("%w: the predicate operator is unsupported", ErrInvalidValue)
	}
}

// Projection is one requested output column. An alias only renames the same
// exact column; it is never an expression.
type Projection struct {
	Column Column
	Alias  string
}

// OutputName is the name the projection carries in the response.
func (projection Projection) OutputName() string {
	if projection.Alias != "" {
		return projection.Alias
	}
	return projection.Column.Name
}

// Predicate is one typed comparison against a declared column. A comparison
// requires an operand; an explicit null operand is refused because SQL equality
// with NULL is never a filter, and a null test must be expressed as one.
type Predicate struct {
	Column   string
	Operator PredicateOperator
	Value    Value
	Values   []Value
}

func (predicate Predicate) Validate() error {
	if _, err := quotedIdentifier(predicate.Column); err != nil {
		return err
	}
	if !predicate.Operator.Valid() {
		return fmt.Errorf("%w: predicate operator %q is unsupported", ErrInvalidValue, predicate.Operator)
	}
	switch predicate.Operator {
	case OperatorIn, OperatorNotIn:
		if len(predicate.Values) == 0 || len(predicate.Values) > MaximumPredicateValues {
			return fmt.Errorf("%w: an in-list must carry 1..%d values", ErrInvalidValue, MaximumPredicateValues)
		}
		for _, value := range predicate.Values {
			if !value.Present() || value.Null() {
				return fmt.Errorf("%w: an in-list carries an absent or explicit null value", ErrInvalidValue)
			}
			if err := value.Validate(); err != nil {
				return err
			}
		}
		return nil
	default:
		if len(predicate.Values) != 0 {
			return fmt.Errorf("%w: predicate operator %q does not consume a value list", ErrInvalidValue, predicate.Operator)
		}
	}
	if !predicate.Operator.requiresOperand() {
		if predicate.Value.Present() {
			return fmt.Errorf("%w: predicate operator %q must not carry an operand", ErrInvalidValue, predicate.Operator)
		}
		return nil
	}
	if !predicate.Value.Present() {
		return fmt.Errorf("%w: predicate operator %q requires an operand", ErrInvalidValue, predicate.Operator)
	}
	if predicate.Value.Null() {
		return fmt.Errorf("%w: an explicit null operand is refused; a null test must be declared as one", ErrInvalidValue)
	}
	return predicate.Value.Validate()
}

// Ordering is one declared sort key.
type Ordering struct {
	Column     string
	Descending bool
}

// Select is one typed relational request: project, filter, order, limit, and an
// optional immutable output segment. It carries no expression language.
type Select struct {
	From       string
	Project    []Projection
	Predicates []Predicate
	Order      []Ordering
	Limit      int64
	Output     *Output
}

// Encode is one typed row batch written as an immutable Parquet segment.
type Encode struct {
	Columns []Column
	Rows    [][]Value
	Output  Output
}

// Request is the one bounded typed request a child executes. Exactly one of
// Select and Encode is present.
type Request struct {
	AdmittedDirectories []string
	Bounds              BoundAnalytics
	Relations           []Relation
	Select              *Select
	Encode              *Encode
}

// Result is the exact typed outcome: columns, rows, a truthful truncation flag,
// and the verified identity of a materialized segment when one was requested.
type Result struct {
	Columns   []Column
	Rows      [][]Value
	Truncated bool
	Output    *OutputDescriptor
}

// Failure is one typed refusal cause. Its code is part of the child protocol;
// its reason is bounded and never carries a payload.
type Failure struct {
	Code   Code
	Reason string
}

func (failure *Failure) Error() string { return string(failure.Code) + ": " + failure.Reason }

// Unwrap exposes the class errors so callers may test them with errors.Is.
func (failure *Failure) Unwrap() error {
	switch failure.Code {
	case CodeProtocol, CodeUnsupportedKind, CodeUnknownRelation, CodeUnknownColumn:
		return ErrInvalidValue
	case CodeUnadmittedPath:
		return ErrUnadmittedPath
	case CodeDigestMismatch:
		return ErrDigestMismatch
	case CodeSchemaMismatch:
		return ErrSchemaMismatch
	case CodeBoundExceeded:
		return ErrInvalidBounds
	default:
		return nil
	}
}

// Code is one closed refusal code shared with the parent protocol.
type Code string

const (
	CodeProtocol        Code = "protocol"
	CodeUnadmittedPath  Code = "unadmitted_path"
	CodeUnknownRelation Code = "unknown_relation"
	CodeUnknownColumn   Code = "unknown_column"
	CodeSchemaMismatch  Code = "schema_mismatch"
	CodeDigestMismatch  Code = "digest_mismatch"
	CodeBoundExceeded   Code = "bound_exceeded"
	CodeUnsupportedKind Code = "unsupported_kind"
	CodeEngine          Code = "engine"
	CodeEnvironment     Code = "environment"
	CodeOutputFailed    Code = "output_failed"
)

// FailureCode reports the refusal code carried by an error, defaulting to the
// engine class for an unknown failure.
func FailureCode(err error) Code {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.Code
	}
	switch {
	case errors.Is(err, ErrInvalidBounds):
		return CodeBoundExceeded
	case errors.Is(err, ErrUnadmittedPath):
		return CodeUnadmittedPath
	case errors.Is(err, ErrDigestMismatch):
		return CodeDigestMismatch
	case errors.Is(err, ErrSchemaMismatch):
		return CodeSchemaMismatch
	case errors.Is(err, ErrInvalidValue):
		return CodeProtocol
	default:
		return CodeEngine
	}
}

func failure(code Code, format string, arguments ...any) error {
	return &Failure{Code: code, Reason: fmt.Sprintf(format, arguments...)}
}

// classify maps one class error into its refusal code, keeping a typed class
// visible to the caller.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var existing *Failure
	if errors.As(err, &existing) {
		return err
	}
	return &Failure{Code: FailureCode(err), Reason: err.Error()}
}

// RunAnalyticRequest executes exactly one bounded typed request inside the
// restricted child. Every refusal happens before the affected work: paths are
// admitted, digests verified, schemas compared, and bounds checked before any
// engine statement reads or writes anything.
func RunAnalyticRequest(ctx context.Context, request Request) (Result, error) {
	var result Result
	if err := request.validate(); err != nil {
		return result, classify(err)
	}
	if ctx == nil {
		return result, failure(CodeProtocol, "an analytical request requires a non-nil context")
	}
	ctx, cancel := context.WithTimeout(ctx, request.Bounds.WallTime)
	defer cancel()
	engine, err := openEngine(ctx, request)
	if err != nil {
		return result, wallTimeFailure(ctx, classify(err))
	}
	defer func() { _ = engine.Close() }()
	result, err = engine.execute(ctx, request)
	if err != nil {
		return result, wallTimeFailure(ctx, err)
	}
	return result, nil
}

// wallTimeFailure reports the declared wall-time bound as its own refusal class
// instead of leaking a driver deadline error.
func wallTimeFailure(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return failure(CodeBoundExceeded, "the declared wall time was exhausted")
	}
	return err
}

// validate refuses an incomplete or over-bound request before any process work.
func (request Request) validate() error {
	if err := request.Bounds.Validate(); err != nil {
		return err
	}
	if len(request.AdmittedDirectories) == 0 || len(request.AdmittedDirectories) > MaximumRelations {
		return failure(CodeUnadmittedPath, "the request must declare 1..%d admitted directories", MaximumRelations)
	}
	for _, directory := range request.AdmittedDirectories {
		if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || strings.ContainsAny(directory, "\x00\n\r") {
			return failure(CodeUnadmittedPath, "an admitted directory must be one absolute, cleaned local path")
		}
	}
	if (request.Select == nil) == (request.Encode == nil) {
		return failure(CodeProtocol, "exactly one of the typed select and encode operations must be present")
	}
	if len(request.Relations) > request.Bounds.Relations {
		return failure(CodeBoundExceeded, "the request declares %d relations beyond its bound of %d",
			len(request.Relations), request.Bounds.Relations)
	}
	declared := make(map[string]Relation, len(request.Relations))
	for _, relation := range request.Relations {
		if err := relation.validateAdmission(request.AdmittedDirectories); err != nil {
			return classify(err)
		}
		if _, duplicate := declared[relation.Name]; duplicate {
			return failure(CodeProtocol, "relation %q is declared more than once", relation.Name)
		}
		declared[relation.Name] = relation
		if len(relation.Columns) > request.Bounds.Columns {
			return failure(CodeBoundExceeded, "relation %q declares %d columns beyond the column bound",
				relation.Name, len(relation.Columns))
		}
		if relation.Bytes > request.Bounds.Bytes {
			return failure(CodeBoundExceeded, "relation %q declares %d bytes beyond the byte bound", relation.Name, relation.Bytes)
		}
	}
	if request.Select != nil {
		return request.Select.validate(request.Bounds, declared, request.AdmittedDirectories)
	}
	return request.Encode.validate(request.Bounds, request.AdmittedDirectories)
}

func (selectRequest Select) validate(bounds BoundAnalytics, relations map[string]Relation, admittedDirectories []string) error {
	relation, declared := relations[selectRequest.From]
	if !declared {
		return failure(CodeUnknownRelation, "the query names relation %q, which this request did not declare", selectRequest.From)
	}
	if len(selectRequest.Project) == 0 || len(selectRequest.Project) > bounds.Columns {
		return failure(CodeBoundExceeded, "the projection must carry 1..%d columns", bounds.Columns)
	}
	if len(selectRequest.Predicates) > MaximumPredicates {
		return failure(CodeBoundExceeded, "the request declares %d predicates beyond the supported %d",
			len(selectRequest.Predicates), MaximumPredicates)
	}
	if len(selectRequest.Order) > MaximumOrderings {
		return failure(CodeBoundExceeded, "the request declares %d orderings beyond the supported %d",
			len(selectRequest.Order), MaximumOrderings)
	}
	if selectRequest.Limit <= 0 || selectRequest.Limit > MaximumRows {
		return failure(CodeBoundExceeded, "the declared row limit must be 1..%d", MaximumRows)
	}
	for _, projection := range selectRequest.Project {
		if _, present := relationColumn(relation, projection.Column.Name); !present {
			return failure(CodeUnknownColumn, "relation %q does not declare column %q", selectRequest.From, projection.Column.Name)
		}
		if projection.Alias != "" {
			if _, err := quotedIdentifier(projection.Alias); err != nil {
				return classify(err)
			}
		}
	}
	for _, predicate := range selectRequest.Predicates {
		if _, present := relationColumn(relation, predicate.Column); !present {
			return failure(CodeUnknownColumn, "relation %q does not declare column %q", selectRequest.From, predicate.Column)
		}
		if err := predicate.Validate(); err != nil {
			return classify(err)
		}
	}
	for _, ordering := range selectRequest.Order {
		if _, present := relationColumn(relation, ordering.Column); !present {
			return failure(CodeUnknownColumn, "relation %q does not declare column %q", selectRequest.From, ordering.Column)
		}
	}
	if selectRequest.Output != nil {
		return selectRequest.Output.validateAdmission(admittedDirectories)
	}
	return nil
}

func (encode Encode) validate(bounds BoundAnalytics, admittedDirectories []string) error {
	if len(encode.Columns) == 0 || len(encode.Columns) > bounds.Columns {
		return failure(CodeBoundExceeded, "an encoded segment must declare 1..%d columns", bounds.Columns)
	}
	if int64(len(encode.Rows)) > bounds.Rows {
		return failure(CodeBoundExceeded, "the encoded batch carries %d rows beyond the row bound", len(encode.Rows))
	}
	for _, column := range encode.Columns {
		if err := column.Validate(); err != nil {
			return classify(err)
		}
	}
	return encode.Output.validateAdmission(admittedDirectories)
}

// engine is one private analytical session: an in-memory engine with the pinned
// restrictions, an owner-only temporary directory, and the admitted relations
// materialized under their declared names.
type engine struct {
	database           *sql.DB
	bounds             BoundAnalytics
	temporaryDirectory string
}

// temporaryRoot resolves the child's own disposable scratch. A relative or
// absent value is refused: the engine never falls back to a shared directory.
func temporaryRoot() (string, error) {
	root := os.Getenv("TMPDIR")
	if root == "" {
		return "", failure(CodeEnvironment, "the restricted child requires an explicit TMPDIR")
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", failure(CodeEnvironment, "the restricted child requires one absolute, cleaned TMPDIR")
	}
	return root, nil
}

// openEngine opens the private in-memory engine with the pinned settings, loads
// only the bundled Parquet extension, allows exactly the admitted paths, and
// then disables external access and locks the configuration.
func openEngine(ctx context.Context, request Request) (*engine, error) {
	root, err := temporaryRoot()
	if err != nil {
		return nil, err
	}
	temporaryDirectory := filepath.Join(root, "duckdb-temp")
	if err := os.MkdirAll(temporaryDirectory, 0o700); err != nil {
		return nil, failure(CodeEnvironment, "create the private engine temporary directory: %v", err)
	}
	if err := os.Chmod(temporaryDirectory, 0o700); err != nil {
		return nil, failure(CodeEnvironment, "restrict the private engine temporary directory: %v", err)
	}
	dsn := "?threads=1&memory_limit=" + request.Bounds.memoryLimitSetting() +
		"&autoinstall_known_extensions=false&autoload_known_extensions=false"
	database, err := sql.Open("duckdb", dsn)
	if err != nil {
		return nil, failure(CodeEngine, "open the private analytical engine: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	engine := &engine{database: database, bounds: request.Bounds, temporaryDirectory: temporaryDirectory}
	if err := engine.harden(ctx, request, root, temporaryDirectory); err != nil {
		_ = database.Close()
		return nil, err
	}
	return engine, nil
}

// harden applies the pinned restrictions in order: the bundled Parquet
// extension is loaded while extension loading is still permitted, the admitted
// paths are allowed, and only then is external access disabled and the
// configuration locked. After this point no file, extension, or network access
// outside the admitted paths is reachable, even through a defect.
func (engine *engine) harden(ctx context.Context, request Request, root, temporaryDirectory string) error {
	allowedPaths := make([]string, 0, len(request.Relations)+1)
	allowedDirectories := make([]string, 0, len(request.Relations)+1)
	for _, relation := range request.Relations {
		allowedPaths = append(allowedPaths, relation.Path)
		allowedDirectories = append(allowedDirectories, filepath.Dir(relation.Path))
	}
	if request.Select != nil && request.Select.Output != nil {
		allowedPaths = append(allowedPaths, request.Select.Output.Path)
		allowedDirectories = append(allowedDirectories, filepath.Dir(request.Select.Output.Path))
	}
	if request.Encode != nil {
		allowedPaths = append(allowedPaths, request.Encode.Output.Path)
		allowedDirectories = append(allowedDirectories, filepath.Dir(request.Encode.Output.Path))
	}
	secrets := filepath.Join(root, "duckdb-secrets")
	if err := os.MkdirAll(secrets, 0o700); err != nil {
		return failure(CodeEnvironment, "create the private engine secret directory: %v", err)
	}
	settings := []struct {
		statement string
		code      Code
	}{
		{"LOAD parquet", CodeEngine},
		{"SET memory_limit = " + quoteSQLString(engine.bounds.memoryLimitSetting()), CodeBoundExceeded},
		{"SET threads = 1", CodeEngine},
		{"SET temp_directory = " + quoteSQLString(temporaryDirectory), CodeEngine},
		{"SET max_temp_directory_size = " + quoteSQLString(engine.bounds.tempLimitSetting()), CodeBoundExceeded},
		{"SET TimeZone = 'UTC'", CodeEngine},
		{"SET home_directory = " + quoteSQLString(root), CodeEngine},
		{"SET secret_directory = " + quoteSQLString(secrets), CodeEngine},
		{"SET allow_persistent_secrets = false", CodeEngine},
		{"SET autoinstall_known_extensions = false", CodeEngine},
		{"SET autoload_known_extensions = false", CodeEngine},
		{"SET allow_community_extensions = false", CodeEngine},
		{"SET allow_unsigned_extensions = false", CodeEngine},
		{"SET allowed_paths = [" + quoteSQLList(sortedUnique(allowedPaths)) + "]", CodeUnadmittedPath},
		{"SET allowed_directories = [" + quoteSQLList(sortedUnique(allowedDirectories)) + "]", CodeUnadmittedPath},
		{"SET enable_external_access = false", CodeEngine},
		{"SET lock_configuration = true", CodeEngine},
	}
	for _, setting := range settings {
		if _, err := engine.database.ExecContext(ctx, setting.statement); err != nil {
			return failure(setting.code, "apply the restricted engine setting %q: %v", settingName(setting.statement), err)
		}
	}
	return nil
}

// settingName keeps diagnostics bounded and free of admitted paths.
func settingName(statement string) string {
	fields := strings.Fields(statement)
	if len(fields) < 2 {
		return "engine"
	}
	if fields[0] == "LOAD" {
		return "LOAD " + fields[1]
	}
	return fields[1]
}

func quoteSQLList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, quoteSQLString(value))
	}
	return strings.Join(quoted, ", ")
}

// Close releases the private engine and removes its disposable directory. It
// never touches an admitted file or a committed segment.
func (engine *engine) Close() error {
	closeErr := engine.database.Close()
	if removeErr := os.RemoveAll(engine.temporaryDirectory); removeErr != nil && closeErr == nil {
		closeErr = failure(CodeEnvironment, "remove the private engine temporary directory: %v", removeErr)
	}
	return closeErr
}

// execute materializes the admitted relations, runs the one declared
// operation, and reports the exact typed outcome.
func (engine *engine) execute(ctx context.Context, request Request) (Result, error) {
	var result Result
	for _, relation := range request.Relations {
		if err := relation.verifyAdmittedBytes(); err != nil {
			return result, classify(err)
		}
		if err := verifyRelationSchema(ctx, engine.database, relation); err != nil {
			return result, classify(err)
		}
		if err := registerRelation(ctx, engine.database, relation); err != nil {
			return result, classify(err)
		}
	}
	if request.Encode != nil {
		output := request.Encode.Output
		if err := output.validateAdmission(request.AdmittedDirectories); err != nil {
			return result, classify(err)
		}
		descriptor, err := MaterializeParquet(ctx, engine.database, request.Encode.Columns, request.Encode.Rows, output)
		if err != nil {
			return result, classify(err)
		}
		if descriptor.Bytes > request.Bounds.Bytes {
			_ = os.Remove(descriptor.Path)
			return result, failure(CodeBoundExceeded, "the materialized segment exceeds the declared byte bound")
		}
		result.Columns = append([]Column(nil), request.Encode.Columns...)
		result.Output = &descriptor
		return result, nil
	}
	return engine.selectRows(ctx, request)
}

// selectRows executes the typed projection over the materialized relations.
func (engine *engine) selectRows(ctx context.Context, request Request) (Result, error) {
	var result Result
	selectRequest := *request.Select
	relations := make(map[string]Relation, len(request.Relations))
	for _, relation := range request.Relations {
		relations[relation.Name] = relation
	}
	from, err := quotedIdentifier(selectRequest.From)
	if err != nil {
		return result, classify(err)
	}
	relation := relations[selectRequest.From]
	columns := make(map[string]Column, len(relation.Columns))
	for _, column := range relation.Columns {
		columns[column.Name] = column
	}
	projection := make([]string, 0, len(selectRequest.Project))
	declared := make([]Column, 0, len(selectRequest.Project))
	for _, requested := range selectRequest.Project {
		column, declaredBefore := columns[requested.Column.Name]
		if !declaredBefore {
			return result, failure(CodeUnknownColumn, "relation %q does not declare column %q", selectRequest.From, requested.Column.Name)
		}
		if requested.Column.Kind != "" && requested.Column.Kind != column.Kind {
			return result, failure(CodeSchemaMismatch, "column %q is declared as %s, not %s", column.Name, column.Kind, requested.Column.Kind)
		}
		if requested.Column.Kind == KindDecimal && requested.Column.Precision != 0 &&
			(requested.Column.Precision != column.Precision || requested.Column.Scale != column.Scale) {
			return result, failure(CodeSchemaMismatch, "column %q is declared DECIMAL(%d,%d), not DECIMAL(%d,%d)",
				column.Name, column.Precision, column.Scale, requested.Column.Precision, requested.Column.Scale)
		}
		expression, err := projectionExpression(column)
		if err != nil {
			return result, classify(err)
		}
		projection = append(projection, expression)
		output := column
		if requested.Alias != "" {
			output.Name = requested.Alias
		}
		output.Nullable = column.Nullable
		declared = append(declared, output)
	}
	statement := "SELECT " + strings.Join(projection, ", ") + " FROM " + from
	arguments := make([]any, 0, len(selectRequest.Predicates))
	if len(selectRequest.Predicates) > 0 {
		clauses := make([]string, 0, len(selectRequest.Predicates))
		for _, predicate := range selectRequest.Predicates {
			column, declaredBefore := columns[predicate.Column]
			if !declaredBefore {
				return result, failure(CodeUnknownColumn, "relation %q does not declare column %q", selectRequest.From, predicate.Column)
			}
			clause, clauseArguments, err := predicateClause(column, predicate)
			if err != nil {
				return result, classify(err)
			}
			clauses = append(clauses, clause)
			arguments = append(arguments, clauseArguments...)
		}
		statement += " WHERE " + strings.Join(clauses, " AND ")
	}
	if len(selectRequest.Order) > 0 {
		orderings := make([]string, 0, len(selectRequest.Order))
		for _, ordering := range selectRequest.Order {
			column, declaredBefore := columns[ordering.Column]
			if !declaredBefore {
				return result, failure(CodeUnknownColumn, "relation %q does not declare column %q", selectRequest.From, ordering.Column)
			}
			identifier, err := column.Identifier()
			if err != nil {
				return result, classify(err)
			}
			direction := "ASC"
			if ordering.Descending {
				direction = "DESC"
			}
			orderings = append(orderings, identifier+" "+direction)
		}
		statement += " ORDER BY " + strings.Join(orderings, ", ")
	}
	if selectRequest.Output != nil {
		return engine.writeSegment(ctx, request, statement, arguments, declared)
	}
	return engine.readRows(ctx, request, statement, arguments, declared)
}

// writeSegment materializes the exact query result as one immutable Parquet
// segment. A segment is never truncated: a result beyond the declared bounds is
// refused and the partial file removed.
func (engine *engine) writeSegment(ctx context.Context, request Request, statement string, arguments []any, declared []Column) (Result, error) {
	var result Result
	output := *request.Select.Output
	if err := output.validateAdmission(request.AdmittedDirectories); err != nil {
		return result, classify(err)
	}
	statement = "COPY (" + statement + " LIMIT " + fmt.Sprintf("%d", request.Select.Limit) + ") TO " +
		quoteSQLString(output.Path) + " (FORMAT PARQUET, COMPRESSION zstd)"
	if _, err := engine.database.ExecContext(ctx, statement, arguments...); err != nil {
		return result, failure(CodeOutputFailed, "write the admitted Parquet segment: %v", err)
	}
	descriptor, err := DescribeParquetOutput(output.Path)
	if err != nil {
		return result, classify(err)
	}
	if descriptor.Bytes > request.Bounds.Bytes {
		_ = os.Remove(descriptor.Path)
		return result, failure(CodeBoundExceeded, "the materialized segment exceeds the declared byte bound")
	}
	result.Columns = declared
	result.Output = &descriptor
	return result, nil
}

// readRows reads at most the declared row and byte bounds and reports whether
// either bound cut the declared result. The flag is truthful: it is only set
// when rows the query would have returned were withheld.
func (engine *engine) readRows(ctx context.Context, request Request, statement string, arguments []any, declared []Column) (Result, error) {
	var result Result
	rowBound := minInt64(request.Bounds.Rows, request.Select.Limit)
	statement += " LIMIT " + fmt.Sprintf("%d", rowBound+1)
	rows, err := engine.database.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return result, failure(CodeEngine, "execute the typed selection: %v", err)
	}
	defer func() { _ = rows.Close() }()
	scanners := make([]any, 0, len(declared))
	for _, column := range declared {
		scanners = append(scanners, scanDestination(column.Kind))
	}
	byteBudget := request.Bounds.Bytes
	usedBytes := 0
	truncated := false
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return result, failure(CodeBoundExceeded, "the declared wall time was exhausted")
		}
		for index := range scanners {
			resetScanDestination(scanners[index])
		}
		if err := rows.Scan(scanners...); err != nil {
			return result, failure(CodeEngine, "read the typed selection: %v", err)
		}
		row := make([]Value, 0, len(declared))
		rowBytes := 0
		for index, column := range declared {
			value, err := scanValue(column, scanners[index])
			if err != nil {
				return result, classify(err)
			}
			if value.Null() && !column.Nullable {
				return result, failure(CodeSchemaMismatch, "column %q is declared non-nullable but the admitted row is null", column.Name)
			}
			encoded, err := EncodeAnalyticValue(value)
			if err != nil {
				return result, classify(err)
			}
			rowBytes += len(encoded)
			row = append(row, value)
		}
		if int64(len(result.Rows)) >= rowBound || int64(usedBytes+rowBytes) > byteBudget {
			truncated = true
			break
		}
		usedBytes += rowBytes
		result.Rows = append(result.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return result, failure(CodeEngine, "read the typed selection: %v", err)
	}
	result.Columns = declared
	result.Truncated = truncated
	return result, nil
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

// projectionExpression renders the exact extraction for one declared column.
// Every extraction is a widening or a lossless canonical rendering; a value
// that the declared class cannot represent is reported instead of coerced.
func projectionExpression(column Column) (string, error) {
	identifier, err := column.Identifier()
	if err != nil {
		return "", err
	}
	switch column.Kind {
	case KindInt64:
		return "CAST(" + identifier + " AS BIGINT)", nil
	case KindText, KindUUID, KindJSON:
		return "CAST(" + identifier + " AS VARCHAR)", nil
	case KindDecimal:
		return "CAST(" + identifier + " AS VARCHAR)", nil
	case KindBoolean:
		return "CAST(" + identifier + " AS BOOLEAN)", nil
	case KindBytea:
		return "hex(" + identifier + ")", nil
	case KindDate:
		return "CAST(" + identifier + " AS VARCHAR)", nil
	case KindTimestamp, KindTimestampTZ:
		return "epoch_us(" + identifier + ")", nil
	default:
		return "", failure(CodeUnsupportedKind, "column %q declares an unsupported kind", column.Name)
	}
}

// bindingExpression renders one typed parameter: the placeholder expression the
// engine evaluates and the exact Go argument it receives.
func bindingExpression(column Column, value Value) (string, any, error) {
	switch column.Kind {
	case KindInt64:
		integer, ok := value.Int64()
		if !ok {
			return "", nil, failure(CodeUnsupportedKind, "column %q requires an int64 value", column.Name)
		}
		return "CAST(? AS BIGINT)", integer, nil
	case KindText:
		text, ok := value.Text()
		if !ok {
			return "", nil, failure(CodeUnsupportedKind, "column %q requires a text value", column.Name)
		}
		return "CAST(? AS VARCHAR)", text, nil
	case KindBoolean:
		boolean, ok := value.Boolean()
		if !ok {
			return "", nil, failure(CodeUnsupportedKind, "column %q requires a boolean value", column.Name)
		}
		return "CAST(? AS BOOLEAN)", boolean, nil
	case KindBytea:
		data, ok := value.Bytes()
		if !ok {
			return "", nil, failure(CodeUnsupportedKind, "column %q requires a bytea value", column.Name)
		}
		return "from_hex(?)", hexEncode(data), nil
	case KindDecimal:
		unscaled, scale, ok := value.Unscaled()
		if !ok || scale != column.Scale {
			return "", nil, failure(CodeSchemaMismatch, "column %q is DECIMAL(%d,%d) and requires a value at that exact scale",
				column.Name, column.Precision, column.Scale)
		}
		text, err := decimalText(unscaled, scale)
		if err != nil {
			return "", nil, classify(err)
		}
		return fmt.Sprintf("CAST(? AS DECIMAL(%d,%d))", column.Precision, column.Scale), text, nil
	case KindDate:
		text, ok := value.Text()
		if !ok {
			return "", nil, failure(CodeUnsupportedKind, "column %q requires a date value", column.Name)
		}
		return "CAST(? AS DATE)", text, nil
	case KindTimestamp:
		micros, ok := value.Int64()
		if !ok {
			return "", nil, failure(CodeUnsupportedKind, "column %q requires a timestamp value", column.Name)
		}
		return "make_timestamp(CAST(? AS BIGINT))", micros, nil
	case KindTimestampTZ:
		micros, ok := value.Int64()
		if !ok {
			return "", nil, failure(CodeUnsupportedKind, "column %q requires a timestamp with time zone value", column.Name)
		}
		return "make_timestamptz(CAST(? AS BIGINT))", micros, nil
	case KindUUID:
		text, ok := value.Text()
		if !ok {
			return "", nil, failure(CodeUnsupportedKind, "column %q requires a UUID value", column.Name)
		}
		return "CAST(? AS UUID)", text, nil
	case KindJSON:
		text, ok := value.Text()
		if !ok {
			return "", nil, failure(CodeUnsupportedKind, "column %q requires a JSON value", column.Name)
		}
		return "CAST(? AS JSON)", text, nil
	default:
		return "", nil, failure(CodeUnsupportedKind, "column %q declares an unsupported kind", column.Name)
	}
}

// predicateClause renders one closed comparison with bound arguments.
func predicateClause(column Column, predicate Predicate) (string, []any, error) {
	identifier, err := column.Identifier()
	if err != nil {
		return "", nil, err
	}
	switch predicate.Operator {
	case OperatorIsNull:
		return identifier + " IS NULL", nil, nil
	case OperatorIsNotNull:
		return identifier + " IS NOT NULL", nil, nil
	case OperatorIn, OperatorNotIn:
		operator, err := predicate.Operator.operatorSQL()
		if err != nil {
			return "", nil, classify(err)
		}
		placeholders := make([]string, 0, len(predicate.Values))
		arguments := make([]any, 0, len(predicate.Values))
		for _, value := range predicate.Values {
			expression, argument, err := bindingExpression(column, value)
			if err != nil {
				return "", nil, err
			}
			placeholders = append(placeholders, expression)
			arguments = append(arguments, argument)
		}
		return identifier + " " + operator + " (" + strings.Join(placeholders, ", ") + ")", arguments, nil
	}
	operator, err := predicate.Operator.operatorSQL()
	if err != nil {
		return "", nil, classify(err)
	}
	expression, argument, err := bindingExpression(column, predicate.Value)
	if err != nil {
		return "", nil, err
	}
	return identifier + " " + operator + " " + expression, []any{argument}, nil
}

// scanDestination allocates the exact scan target for one projection kind.
func scanDestination(kind Kind) any {
	switch kind {
	case KindInt64, KindTimestamp, KindTimestampTZ:
		return new(sql.NullInt64)
	case KindBoolean:
		return new(sql.NullBool)
	default:
		return new(sql.NullString)
	}
}

func resetScanDestination(destination any) {
	switch target := destination.(type) {
	case *sql.NullInt64:
		*target = sql.NullInt64{}
	case *sql.NullBool:
		*target = sql.NullBool{}
	case *sql.NullString:
		*target = sql.NullString{}
	}
}

// scanValue converts one scanned cell into its exact typed value. A null stays
// an explicit null; every payload class is revalidated rather than trusted.
func scanValue(column Column, destination any) (Value, error) {
	switch target := destination.(type) {
	case *sql.NullInt64:
		if !target.Valid {
			return NullValue(), nil
		}
		switch column.Kind {
		case KindInt64:
			return Int64Value(target.Int64), nil
		case KindTimestamp:
			return TimestampValue(target.Int64), nil
		case KindTimestampTZ:
			return TimestampTZValue(target.Int64), nil
		default:
			return Value{}, failure(CodeUnsupportedKind, "the scan class %s does not match its projection", column.Kind)
		}
	case *sql.NullBool:
		if !target.Valid {
			return NullValue(), nil
		}
		if column.Kind != KindBoolean {
			return Value{}, failure(CodeUnsupportedKind, "the scan class %s does not match its projection", column.Kind)
		}
		return BooleanValue(target.Bool), nil
	case *sql.NullString:
		if !target.Valid {
			return NullValue(), nil
		}
		return parseProjectedText(column, target.String)
	default:
		return Value{}, failure(CodeEngine, "the scan destination is unsupported")
	}
}

// parseProjectedText converts one canonical engine rendering back into its
// declared class. Text that does not carry the declared shape is refused.
func parseProjectedText(column Column, text string) (Value, error) {
	switch column.Kind {
	case KindText:
		return TextValue(text)
	case KindDecimal:
		unscaled, err := parseDecimalText(text, column.Scale)
		if err != nil {
			return Value{}, classify(err)
		}
		return DecimalValue(unscaled, column.Scale)
	case KindDate:
		return DateValue(text)
	case KindUUID:
		return UUIDValue(text)
	case KindJSON:
		return JSONValue(text)
	case KindBytea:
		decoded, err := hexDecode(text)
		if err != nil {
			return Value{}, classify(err)
		}
		return BytesValue(decoded)
	default:
		return Value{}, failure(CodeUnsupportedKind, "the projected text class %s is unsupported", column.Kind)
	}
}

// relationColumn reports whether one relation declares the column by name.
func relationColumn(relation Relation, name string) (Column, bool) {
	for _, column := range relation.Columns {
		if column.Name == name {
			return column, true
		}
	}
	return Column{}, false
}
