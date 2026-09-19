package analytics

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrInvalidPath identifies an admitted-path or identifier violation.
var ErrInvalidPath = errors.New("invalid admitted path")

// ErrUnadmittedPath identifies a path outside the admitted set or of a type the
// child never opens.
var ErrUnadmittedPath = errors.New("unadmitted path")

// ErrDigestMismatch identifies admitted bytes that no longer match their
// declared identity.
var ErrDigestMismatch = errors.New("admitted digest mismatch")

// ErrSchemaMismatch identifies a declared schema the admitted bytes cannot
// represent exactly.
var ErrSchemaMismatch = errors.New("declared schema mismatch")

// Column is one declared relation column. Precision and Scale are required for
// a decimal and refused for every other kind, so a projection can never guess a
// scale.
type Column struct {
	Name      string
	Kind      Kind
	Precision int
	Scale     int
	Nullable  bool
}

// Validate refuses a column whose declared type cannot describe an exact
// projection.
func (column Column) Validate() error {
	if _, err := column.Identifier(); err != nil {
		return err
	}
	if !column.Kind.Valid() {
		return fmt.Errorf("%w: column %q declares an unsupported kind %q", ErrInvalidValue, column.Name, column.Kind)
	}
	if column.Kind == KindDecimal {
		if column.Precision <= 0 || column.Precision > MaximumDecimalDigits {
			return fmt.Errorf("%w: decimal column %q must declare a precision of 1..%d", ErrInvalidValue, column.Name, MaximumDecimalDigits)
		}
		if column.Scale < 0 || column.Scale > column.Precision {
			return fmt.Errorf("%w: decimal column %q must declare a scale of 0..precision", ErrInvalidValue, column.Name)
		}
		return nil
	}
	if column.Precision != 0 || column.Scale != 0 {
		return fmt.Errorf("%w: column %q declares precision or scale for a non-decimal kind", ErrInvalidValue, column.Name)
	}
	return nil
}

// Identifier returns the column name quoted for the engine. The accepted
// character set is closed, so a declared name can never carry statement text.
func (column Column) Identifier() (string, error) { return quotedIdentifier(column.Name) }

// declaredType renders the engine type that materializes this declared column
// exactly.
func (column Column) declaredType() (string, error) {
	if err := column.Validate(); err != nil {
		return "", err
	}
	switch column.Kind {
	case KindInt64:
		return "BIGINT", nil
	case KindDecimal:
		return fmt.Sprintf("DECIMAL(%d,%d)", column.Precision, column.Scale), nil
	case KindText:
		return "VARCHAR", nil
	case KindBoolean:
		return "BOOLEAN", nil
	case KindBytea:
		return "BLOB", nil
	case KindDate:
		return "DATE", nil
	case KindTimestamp:
		return "TIMESTAMP", nil
	case KindTimestampTZ:
		return "TIMESTAMPTZ", nil
	case KindUUID:
		return "UUID", nil
	case KindJSON:
		return "JSON", nil
	default:
		return "", fmt.Errorf("%w: the column kind is unsupported", ErrInvalidValue)
	}
}

// acceptsLogicalType reports whether one engine-reported column type is the
// exact declared class. Widening integer classes are accepted because they are
// lossless; everything else must match exactly.
func (column Column) acceptsLogicalType(logicalType string) bool {
	switch column.Kind {
	case KindInt64:
		switch logicalType {
		case "TINYINT", "SMALLINT", "INTEGER", "BIGINT":
			return true
		}
		return false
	case KindDecimal:
		return logicalType == fmt.Sprintf("DECIMAL(%d,%d)", column.Precision, column.Scale)
	case KindText:
		return logicalType == "VARCHAR"
	case KindBoolean:
		return logicalType == "BOOLEAN"
	case KindBytea:
		return logicalType == "BLOB"
	case KindDate:
		return logicalType == "DATE"
	case KindTimestamp:
		return logicalType == "TIMESTAMP"
	case KindTimestampTZ:
		return logicalType == "TIMESTAMPTZ"
	case KindUUID:
		return logicalType == "UUID"
	case KindJSON:
		return logicalType == "JSON"
	default:
		return false
	}
}

// Relation is one admitted local Parquet file registered read-only under a
// declared name. The digest and byte length are the admitted identity: a file
// whose bytes changed is refused rather than read.
type Relation struct {
	Name    string
	Path    string
	SHA256  string
	Bytes   int64
	Columns []Column
}

// Validate refuses a relation that cannot be safely opened.
func (relation Relation) Validate() error {
	if _, err := quotedIdentifier(relation.Name); err != nil {
		return err
	}
	if !filepath.IsAbs(relation.Path) || filepath.Clean(relation.Path) != relation.Path {
		return fmt.Errorf("%w: relation %q must name one absolute, cleaned local path", ErrInvalidPath, relation.Name)
	}
	if err := validateDeclaredSHA256(relation.SHA256); err != nil {
		return err
	}
	if relation.Bytes <= 0 {
		return fmt.Errorf("%w: relation %q must declare the admitted byte length", ErrInvalidPath, relation.Name)
	}
	if len(relation.Columns) == 0 {
		return fmt.Errorf("%w: relation %q declares no columns", ErrInvalidPath, relation.Name)
	}
	for _, column := range relation.Columns {
		if err := column.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// validateAdmission refuses a path outside the declared admitted directories
// and any path that is not one regular, non-symlink file.
func (relation Relation) validateAdmission(admittedDirectories []string) error {
	if err := relation.Validate(); err != nil {
		return err
	}
	if !WithinAdmittedDirectories(relation.Path, admittedDirectories) {
		return fmt.Errorf("%w: relation %q is outside the admitted directories", ErrUnadmittedPath, relation.Name)
	}
	return validateRegularFile(relation.Path, fmt.Sprintf("relation %q", relation.Name))
}

// verifyAdmittedBytes streams the admitted file and requires its exact admitted
// identity. It returns the file length, bounded by the declared byte bound, so
// a growing or replaced file is refused instead of partially read.
func (relation Relation) verifyAdmittedBytes() error {
	if relation.Bytes > MaximumBytes {
		return fmt.Errorf("%w: relation %q exceeds the per-relation byte capability", ErrInvalidPath, relation.Name)
	}
	file, err := os.Open(relation.Path)
	if err != nil {
		return fmt.Errorf("%w: open relation %q: %v", ErrUnadmittedPath, relation.Name, err)
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	written, err := io.Copy(digest, io.LimitReader(file, relation.Bytes+1))
	if err != nil {
		return fmt.Errorf("%w: read relation %q: %v", ErrUnadmittedPath, relation.Name, err)
	}
	if written != relation.Bytes {
		return fmt.Errorf("%w: relation %q declared %d bytes but the admitted file has %d",
			ErrDigestMismatch, relation.Name, relation.Bytes, written)
	}
	actual := "sha256:" + hex.EncodeToString(digest.Sum(nil))
	if actual != relation.SHA256 {
		return fmt.Errorf("%w: relation %q does not match its admitted digest", ErrDigestMismatch, relation.Name)
	}
	return nil
}

// Output is one admitted immutable Parquet destination. The destination must
// live inside a declared admitted directory and must not already exist: an
// admitted segment is never overwritten.
type Output struct {
	Path string
}

func (output Output) validateAdmission(admittedDirectories []string) error {
	if !filepath.IsAbs(output.Path) || filepath.Clean(output.Path) != output.Path {
		return fmt.Errorf("%w: the output must name one absolute, cleaned local path", ErrUnadmittedPath)
	}
	if !WithinAdmittedDirectories(output.Path, admittedDirectories) {
		return fmt.Errorf("%w: the output path is outside the admitted directories", ErrUnadmittedPath)
	}
	directory := filepath.Dir(output.Path)
	if err := validateRegularDirectory(directory, "the output directory"); err != nil {
		return err
	}
	info, err := os.Lstat(output.Path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: the output path is a symlink", ErrUnadmittedPath)
		}
		return fmt.Errorf("%w: the output path already exists and an immutable segment is never overwritten", ErrUnadmittedPath)
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("%w: inspect the output path: %v", ErrUnadmittedPath, err)
	}
	return nil
}

// OutputDescriptor is the verified identity of one materialized Parquet
// segment.
type OutputDescriptor struct {
	Path   string
	SHA256 string
	Bytes  int64
}

func (output OutputDescriptor) Validate() error {
	if output.Path == "" || output.SHA256 == "" || output.Bytes <= 0 {
		return fmt.Errorf("%w: the output descriptor is incomplete", ErrInvalidPath)
	}
	return validateDeclaredSHA256(output.SHA256)
}

// WithinAdmittedDirectories reports whether path is inside one of the declared
// admitted directories.
func WithinAdmittedDirectories(path string, admittedDirectories []string) bool {
	for _, directory := range admittedDirectories {
		if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
			continue
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			continue
		}
		if relative == "." || strings.HasPrefix(relative, "..") {
			continue
		}
		return true
	}
	return false
}

// validateRegularFile refuses a symlink, a FIFO, a device, a directory, or a
// missing file before the engine can open it.
func validateRegularFile(path, description string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: %s is not readable: %v", ErrUnadmittedPath, description, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symlink", ErrUnadmittedPath, description)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file (%s)", ErrUnadmittedPath, description, info.Mode().Type())
	}
	return nil
}

func validateRegularDirectory(path, description string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: %s is not readable: %v", ErrUnadmittedPath, description, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symlink", ErrUnadmittedPath, description)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrUnadmittedPath, description)
	}
	return nil
}

func validateDeclaredSHA256(value string) error {
	encoded := strings.TrimPrefix(value, "sha256:")
	if len(encoded) != 64 || encoded != strings.ToLower(encoded) {
		return fmt.Errorf("%w: a declared digest must be a lowercase sha256 value", ErrDigestMismatch)
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		return fmt.Errorf("%w: a declared digest must be a lowercase sha256 value", ErrDigestMismatch)
	}
	return nil
}

// verifyRelationSchema compares the file's actual engine types with the
// declared columns before anything is materialized. A declared class that the
// admitted bytes cannot represent is refused, never widened.
func verifyRelationSchema(ctx context.Context, database *sql.DB, relation Relation) error {
	rows, err := database.QueryContext(ctx, "SELECT * FROM read_parquet(?) LIMIT 0", relation.Path)
	if err != nil {
		return fmt.Errorf("%w: inspect relation %q: %v", ErrSchemaMismatch, relation.Name, err)
	}
	defer func() { _ = rows.Close() }()
	types, err := rows.ColumnTypes()
	if err != nil {
		return fmt.Errorf("%w: inspect relation %q columns: %v", ErrSchemaMismatch, relation.Name, err)
	}
	actual := make(map[string]string, len(types))
	for _, column := range types {
		actual[column.Name()] = column.DatabaseTypeName()
	}
	for _, column := range relation.Columns {
		logicalType, ok := actual[column.Name]
		if !ok {
			return fmt.Errorf("%w: relation %q does not contain the declared column %q", ErrSchemaMismatch, relation.Name, column.Name)
		}
		if !column.acceptsLogicalType(logicalType) {
			return fmt.Errorf("%w: relation %q column %q is %s and cannot be projected as %s",
				ErrSchemaMismatch, relation.Name, column.Name, logicalType, column.Kind)
		}
	}
	return nil
}

// registerRelation materializes one admitted relation into a named in-memory
// relation. The read is a parameterized Parquet scan of exactly the admitted
// path, so no path text or statement text is ever composed from request input.
func registerRelation(ctx context.Context, database *sql.DB, relation Relation) error {
	identifier, err := quotedIdentifier(relation.Name)
	if err != nil {
		return err
	}
	projection := make([]string, 0, len(relation.Columns))
	for _, column := range relation.Columns {
		columnIdentifier, err := column.Identifier()
		if err != nil {
			return err
		}
		projection = append(projection, columnIdentifier)
	}
	statement := "CREATE TABLE " + identifier + " AS SELECT " + strings.Join(projection, ", ") +
		" FROM read_parquet(?)"
	if _, err := database.ExecContext(ctx, statement, relation.Path); err != nil {
		return fmt.Errorf("%w: read relation %q: %v", ErrSchemaMismatch, relation.Name, err)
	}
	return nil
}

// MaterializeParquet writes typed rows into one immutable Parquet segment at
// the admitted output path and returns its verified identity. The declared
// columns decide the physical schema; no value is widened to fit it.
func MaterializeParquet(ctx context.Context, database *sql.DB, columns []Column, rows [][]Value, output Output) (OutputDescriptor, error) {
	var descriptor OutputDescriptor
	if len(columns) == 0 {
		return descriptor, fmt.Errorf("%w: materialization requires declared columns", ErrInvalidValue)
	}
	declared := make([]string, 0, len(columns))
	for _, column := range columns {
		identifier, err := column.Identifier()
		if err != nil {
			return descriptor, err
		}
		declaredType, err := column.declaredType()
		if err != nil {
			return descriptor, err
		}
		declared = append(declared, identifier+" "+declaredType)
	}
	table := quotedInternalIdentifier(encodedRelationName)
	if _, err := database.ExecContext(ctx, "CREATE TABLE "+table+" ("+strings.Join(declared, ", ")+")"); err != nil {
		return descriptor, fmt.Errorf("create the encoded relation: %w", err)
	}
	if err := insertTypedRows(ctx, database, table, columns, rows); err != nil {
		return descriptor, err
	}
	projection := make([]string, 0, len(columns))
	for _, column := range columns {
		identifier, err := column.Identifier()
		if err != nil {
			return descriptor, err
		}
		projection = append(projection, identifier)
	}
	statement := "COPY (SELECT " + strings.Join(projection, ", ") + " FROM " + table + ") TO " +
		quoteSQLString(output.Path) + " (FORMAT PARQUET, COMPRESSION zstd)"
	if _, err := database.ExecContext(ctx, statement); err != nil {
		return descriptor, fmt.Errorf("write the admitted Parquet segment: %w", err)
	}
	return DescribeParquetOutput(output.Path)
}

// DescribeParquetOutput reads back the materialized segment and reports its
// exact identity.
func DescribeParquetOutput(path string) (OutputDescriptor, error) {
	var descriptor OutputDescriptor
	if err := validateRegularFile(path, "the materialized segment"); err != nil {
		return descriptor, err
	}
	file, err := os.Open(path)
	if err != nil {
		return descriptor, fmt.Errorf("open the materialized segment: %w", err)
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	written, err := io.Copy(digest, file)
	if err != nil {
		return descriptor, fmt.Errorf("read the materialized segment: %w", err)
	}
	descriptor = OutputDescriptor{
		Path:   path,
		SHA256: "sha256:" + hex.EncodeToString(digest.Sum(nil)),
		Bytes:  written,
	}
	return descriptor, nil
}

// insertTypedRows binds every payload as a parameter. Values travel as typed
// parameters rather than as statement text, and an absent cell is refused
// because only an explicit NULL may become SQL NULL.
func insertTypedRows(ctx context.Context, database *sql.DB, table string, columns []Column, rows [][]Value) error {
	prefix := "INSERT INTO " + table + " VALUES "
	statements := make([]string, 0, encodedInsertBatchRows)
	arguments := make([]any, 0, encodedInsertBatchRows*len(columns))
	flush := func() error {
		if len(statements) == 0 {
			return nil
		}
		if _, err := database.ExecContext(ctx, prefix+strings.Join(statements, ", "), arguments...); err != nil {
			return fmt.Errorf("bind the encoded rows: %w", err)
		}
		statements = statements[:0]
		arguments = arguments[:0]
		return nil
	}
	for index, row := range rows {
		if len(row) != len(columns) {
			return fmt.Errorf("%w: encoded row %d carries %d cells for %d columns", ErrInvalidValue, index, len(row), len(columns))
		}
		cells := make([]string, 0, len(columns))
		for position, column := range columns {
			value := row[position]
			if !value.Present() {
				return fmt.Errorf("%w: encoded row %d column %q is absent; an explicit null must be declared as one",
					ErrInvalidValue, index, column.Name)
			}
			if value.Null() {
				if !column.Nullable {
					return fmt.Errorf("%w: encoded row %d column %q is null in a non-nullable column",
						ErrInvalidValue, index, column.Name)
				}
				cells = append(cells, "NULL")
				continue
			}
			if value.Kind() != column.Kind {
				return fmt.Errorf("%w: encoded row %d column %q carries %s for a %s column",
					ErrInvalidValue, index, column.Name, value.Kind(), column.Kind)
			}
			expression, argument, err := bindingExpression(column, value)
			if err != nil {
				return err
			}
			cells = append(cells, expression)
			arguments = append(arguments, argument)
		}
		statements = append(statements, "("+strings.Join(cells, ", ")+")")
		if len(statements) >= encodedInsertBatchRows {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// quotedIdentifier validates a declared identifier against a closed character
// set and then quotes it. Statement text can never be carried by a name.
func quotedIdentifier(name string) (string, error) {
	if name == "" || len(name) > MaximumIdentifierBytes {
		return "", fmt.Errorf("%w: an identifier must be 1..%d bytes", ErrInvalidPath, MaximumIdentifierBytes)
	}
	for index, character := range name {
		valid := character == '_' || (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z')
		if index > 0 && character >= '0' && character <= '9' {
			valid = true
		}
		if !valid {
			return "", fmt.Errorf("%w: identifier %q contains characters outside the supported set", ErrInvalidPath, name)
		}
	}
	return `"` + name + `"`, nil
}

func quotedInternalIdentifier(name string) string { return `"` + name + `"` }

// quoteSQLString renders one already validated absolute path as a SQL string
// literal. Single quotes are doubled, and paths carrying control characters are
// never quoted because they cannot reach this function.
func quoteSQLString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// sortedUnique returns the sorted unique values of one list.
func sortedUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if _, present := seen[value]; present {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	sort.Strings(unique)
	return unique
}
