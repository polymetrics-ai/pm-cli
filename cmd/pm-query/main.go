// Command pm-query is the restricted analytical child. It reads exactly one
// bounded typed request from standard input, opens only the admitted local
// files that request names, executes one typed operation in a private engine,
// and writes exactly one bounded typed response to standard output. Diagnostics
// stay on standard error and it never requires provider, object-store, or
// PostgreSQL credentials, statement text, file functions, or external
// extensions.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/polymetrics-ai/pm-cli/internal/analytics"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const (
	// protocolVersion is the only accepted request and response version.
	protocolVersion = "pm.query/1"
	// descriptorDeclarationEnvironment names the inherited descriptors the
	// supervisor declares for closing. A child without the declaration refuses
	// to run, so an undeclared inherited descriptor can never reach execution.
	descriptorDeclarationEnvironment = "PM_QUERY_CLOSE_DESCRIPTORS"
	// descriptorScanLimit bounds the descriptor sweep and report.
	descriptorScanLimit = 256
	// temporaryDirectoryName is the child's private engine scratch below TMPDIR.
	temporaryDirectoryName = "duckdb-temp"
	// maximumDiagnosticBytes bounds one diagnostic line.
	maximumDiagnosticBytes = 256
	// maximumRefusalReasonBytes bounds one refusal reason.
	maximumRefusalReasonBytes = 512
	// maximumEnvironmentNames bounds the reported environment size.
	maximumEnvironmentNames = 64
	// readyAnnouncement is emitted before the request is read so the supervisor
	// can confirm the restricted startup and observe the live process.
	readyAnnouncement = "pm-query ready pid="
	// maximumWallMillis is the child's own wall-time capability in the wire
	// form's unit. A decoded millisecond value is range-checked against it
	// before it is converted into a duration, because the nanosecond
	// multiplication wraps.
	maximumWallMillis = int64(analytics.MaximumWallTime / time.Millisecond)
)

const (
	exitSuccess        = 0
	exitRefused        = 1
	exitStartupFailure = 2
)

func main() {
	os.Exit(run(os.Stdin, os.Stdout, os.Stderr))
}

// run executes one child invocation. Every failure path either reports a typed
// refusal document, which never contains rows, or a bounded startup diagnostic
// on standard error with a non-zero exit status.
func run(stdin io.Reader, stdout, stderr io.Writer) int {
	declaration, declared := os.LookupEnv(descriptorDeclarationEnvironment)
	if !declared {
		diagnostic(stderr, "refusing to run without the inherited descriptor declaration")
		return exitStartupFailure
	}
	descriptors, err := parseDescriptorDeclaration(declaration)
	if err != nil {
		diagnostic(stderr, "invalid inherited descriptor declaration: %v", err)
		return exitStartupFailure
	}
	inherited, closed, unclosed := closeInheritedDescriptors(descriptors)
	if len(unclosed) != 0 {
		diagnostic(stderr, "inherited descriptors could not be closed: %v", unclosed)
		return exitStartupFailure
	}
	temporaryRoot, err := resolvedTemporaryRoot()
	if err != nil {
		diagnostic(stderr, "%v", err)
		return exitStartupFailure
	}
	observation := childObservation{
		PID:                  os.Getpid(),
		Environment:          environmentNames(),
		InheritedDescriptors: inherited,
		ClosedDescriptors:    closed,
		UnclosedDescriptors:  unclosed,
		RemainingDescriptors: remainingDescriptors(descriptors),
		TemporaryDirectory:   filepath.Join(temporaryRoot, temporaryDirectoryName),
	}
	fmt.Fprintf(stderr, "%s%d env=%d closed=%d\n",
		readyAnnouncement, observation.PID, len(observation.Environment), len(observation.ClosedDescriptors))

	document, err := readBoundedDocument(stdin, analytics.MaximumRequestBytes)
	if err != nil {
		return refuse(stdout, stderr, observation, "", analytics.CodeProtocol, err.Error())
	}
	request, requestID, err := decodeRequest(document)
	if err != nil {
		return refuse(stdout, stderr, observation, requestID, analytics.FailureCode(err), err.Error())
	}
	result, err := analytics.RunAnalyticRequest(context.Background(), request)
	if err != nil {
		return refuse(stdout, stderr, observation, requestID, analytics.FailureCode(err), err.Error())
	}
	columns := make([]columnDocument, 0, len(result.Columns))
	for _, column := range result.Columns {
		columns = append(columns, newColumnDocument(column))
	}
	rows := make([][]analytics.Value, 0, len(result.Rows))
	rows = append(rows, result.Rows...)
	response := responseDocument{
		Protocol:  protocolVersion,
		RequestID: requestID,
		Columns:   &columns,
		Rows:      &rows,
		Truncated: &result.Truncated,
		Child:     observation.document(),
	}
	if result.Output != nil {
		response.Output = &outputDocument{
			Path:   result.Output.Path,
			SHA256: result.Output.SHA256,
			Bytes:  strconv.FormatInt(result.Output.Bytes, 10),
		}
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return refuse(stdout, stderr, observation, requestID, analytics.CodeEngine, "encode the typed response")
	}
	if len(encoded) > analytics.MaximumResponseBytes {
		return refuse(stdout, stderr, observation, requestID, analytics.CodeBoundExceeded,
			"the encoded response exceeds the child response capability")
	}
	if _, err := stdout.Write(append(encoded, '\n')); err != nil {
		diagnostic(stderr, "write the typed response: %v", err)
		return exitStartupFailure
	}
	return exitSuccess
}

// refuse writes one typed refusal document and returns the refusal exit status.
// It never writes a row.
func refuse(stdout, stderr io.Writer, observation childObservation, requestID string, code analytics.Code, reason string) int {
	document := responseDocument{
		Protocol:  protocolVersion,
		RequestID: requestID,
		Child:     observation.document(),
		Refusal: &refusalDocument{
			Code:   string(code),
			Reason: sanitized(reason, maximumRefusalReasonBytes),
		},
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		diagnostic(stderr, "encode the refusal document: %v", err)
		return exitStartupFailure
	}
	if _, err := stdout.Write(append(encoded, '\n')); err != nil {
		diagnostic(stderr, "write the refusal document: %v", err)
		return exitStartupFailure
	}
	return exitRefused
}

// diagnostic writes one bounded, sanitized startup diagnostic.
func diagnostic(stderr io.Writer, format string, arguments ...any) {
	_, _ = fmt.Fprintf(stderr, "pm-query: %s\n", sanitized(fmt.Sprintf(format, arguments...), maximumDiagnosticBytes))
}

// sanitized removes control characters and bounds one diagnostic or refusal
// text so no payload or terminal escape leaves the child.
func sanitized(text string, maximum int) string {
	cleaned := strings.Map(func(character rune) rune {
		if character < 0x20 || character == 0x7f {
			return ' '
		}
		return character
	}, text)
	if len(cleaned) > maximum {
		cleaned = cleaned[:maximum]
		for !utf8.ValidString(cleaned) {
			cleaned = cleaned[:len(cleaned)-1]
		}
	}
	return cleaned
}

// resolvedTemporaryRoot resolves the child's own disposable scratch directory.
// A relative or absent value is refused: the engine never falls back to a
// shared directory and never opens a shared writable database file.
func resolvedTemporaryRoot() (string, error) {
	root := os.Getenv("TMPDIR")
	if root == "" {
		return "", errors.New("an explicit TMPDIR is required")
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", errors.New("TMPDIR must be one absolute, cleaned path")
	}
	return root, nil
}

// environmentNames reports the child's own environment names in sorted order.
// Names are reported, never values: an operator can see what the child was
// given without a value ever leaving the process.
func environmentNames() []string {
	environment := os.Environ()
	names := make([]string, 0, len(environment))
	seen := make(map[string]struct{}, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if _, present := seen[name]; present {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > maximumEnvironmentNames {
		names = names[:maximumEnvironmentNames]
	}
	return names
}

// parseDescriptorDeclaration reads the supervisor's inherited descriptor
// declaration. Only non-negative decimal numbers are accepted, and the standard
// descriptors are never closable.
func parseDescriptorDeclaration(declaration string) ([]int, error) {
	trimmed := strings.TrimSpace(declaration)
	if trimmed == "" {
		return nil, nil
	}
	descriptors := make([]int, 0, 8)
	for _, field := range strings.Split(trimmed, ",") {
		descriptor, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || descriptor < 3 || descriptor >= descriptorScanLimit {
			return nil, fmt.Errorf("descriptor %q is outside the closable range", field)
		}
		descriptors = append(descriptors, descriptor)
	}
	sort.Ints(descriptors)
	return descriptors, nil
}

// closeInheritedDescriptors closes exactly the declared descriptors and
// verifies none of them remained open. It never closes a descriptor the child's
// own runtime created, and it never mutates the parent's descriptors.
func closeInheritedDescriptors(descriptors []int) (inherited, closed, unclosed []int) {
	inherited = make([]int, 0, len(descriptors))
	closed = make([]int, 0, len(descriptors))
	unclosed = make([]int, 0)
	for _, descriptor := range descriptors {
		if !descriptorOpen(descriptor) {
			continue
		}
		inherited = append(inherited, descriptor)
		if err := syscall.Close(descriptor); err != nil {
			continue
		}
		closed = append(closed, descriptor)
	}
	for _, descriptor := range descriptors {
		if descriptorOpen(descriptor) {
			unclosed = append(unclosed, descriptor)
		}
	}
	return inherited, closed, unclosed
}

// descriptorOpen reports whether the process currently holds the descriptor.
func descriptorOpen(descriptor int) bool {
	var stat syscall.Stat_t
	return syscall.Fstat(descriptor, &stat) == nil
}

// remainingDescriptors lists every descriptor above the standard three that the
// child still holds, with its file type, for operator diagnosis. The declared
// set is the complete inherited set, so an entry here is the child's own
// runtime descriptor and never a credential-bearing parent file.
func remainingDescriptors(declared []int) []descriptorDocument {
	declaredSet := make(map[int]struct{}, len(declared))
	for _, descriptor := range declared {
		declaredSet[descriptor] = struct{}{}
	}
	remaining := []descriptorDocument{}
	for descriptor := 3; descriptor < descriptorScanLimit; descriptor++ {
		var stat syscall.Stat_t
		if err := syscall.Fstat(descriptor, &stat); err != nil {
			continue
		}
		if _, declared := declaredSet[descriptor]; declared {
			continue
		}
		remaining = append(remaining, descriptorDocument{
			Descriptor: strconv.Itoa(descriptor),
			Kind:       descriptorKind(uint32(stat.Mode)),
		})
	}
	return remaining
}

func descriptorKind(mode uint32) string {
	switch mode & syscall.S_IFMT {
	case syscall.S_IFREG:
		return "regular"
	case syscall.S_IFDIR:
		return "directory"
	case syscall.S_IFCHR:
		return "character"
	case syscall.S_IFBLK:
		return "block"
	case syscall.S_IFIFO:
		return "pipe"
	case syscall.S_IFSOCK:
		return "socket"
	default:
		return "other"
	}
}

// readBoundedDocument reads one request document and refuses a stream longer
// than the child request capability.
func readBoundedDocument(stdin io.Reader, maximum int64) ([]byte, error) {
	document, err := io.ReadAll(io.LimitReader(stdin, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("read the request document: %w", err)
	}
	if int64(len(document)) > maximum {
		return nil, errors.New("the request document exceeds the child request capability")
	}
	return document, nil
}

// columnDocument is one declared column in the wire protocol. Every integral
// member travels as a decimal string so no JSON number can lose precision, and
// a decimal must declare both its precision and its scale.
type columnDocument struct {
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	Precision *string `json:"precision,omitempty"`
	Scale     *string `json:"scale,omitempty"`
	Nullable  *bool   `json:"nullable,omitempty"`
}

type outputDocument struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
	Bytes  string `json:"bytes,omitempty"`
}

type descriptorDocument struct {
	Descriptor string `json:"descriptor"`
	Kind       string `json:"kind"`
}

type childDocument struct {
	PID                  string               `json:"pid"`
	Environment          []string             `json:"environment"`
	InheritedDescriptors []string             `json:"inherited_descriptors"`
	ClosedDescriptors    []string             `json:"closed_descriptors"`
	UnclosedDescriptors  []string             `json:"unclosed_descriptors"`
	RemainingDescriptors []descriptorDocument `json:"remaining_descriptors"`
	TemporaryDirectory   string               `json:"temporary_directory"`
}

// childObservation is the child's own account of its isolation posture.
type childObservation struct {
	PID                  int
	Environment          []string
	InheritedDescriptors []int
	ClosedDescriptors    []int
	UnclosedDescriptors  []int
	RemainingDescriptors []descriptorDocument
	TemporaryDirectory   string
}

func (observation childObservation) document() childDocument {
	return childDocument{
		PID:                  strconv.Itoa(observation.PID),
		Environment:          observation.Environment,
		InheritedDescriptors: descriptorStrings(observation.InheritedDescriptors),
		ClosedDescriptors:    descriptorStrings(observation.ClosedDescriptors),
		UnclosedDescriptors:  descriptorStrings(observation.UnclosedDescriptors),
		RemainingDescriptors: observation.RemainingDescriptors,
		TemporaryDirectory:   observation.TemporaryDirectory,
	}
}

func descriptorStrings(descriptors []int) []string {
	values := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		values = append(values, strconv.Itoa(descriptor))
	}
	return values
}

type refusalDocument struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// responseDocument is the closed response member set. A successful document
// always carries its columns, rows, and truncation flag, even when the result
// holds no row; a refusal document carries none of them.
type responseDocument struct {
	Protocol  string               `json:"protocol"`
	RequestID string               `json:"request_id"`
	Columns   *[]columnDocument    `json:"columns,omitempty"`
	Rows      *[][]analytics.Value `json:"rows,omitempty"`
	Truncated *bool                `json:"truncated,omitempty"`
	Output    *outputDocument      `json:"output,omitempty"`
	Refusal   *refusalDocument     `json:"refusal,omitempty"`
	Child     childDocument        `json:"child"`
}

// requestDocument is the closed request member set. The strict decoder refuses
// an unknown, duplicated, wrongly typed, or wrongly cased member, so statement
// text, a file function, an extension, or a network location has no place to
// travel.
type requestDocument struct {
	Protocol            string          `json:"protocol"`
	RequestID           string          `json:"request_id"`
	AdmittedDirectories []string        `json:"admitted_directories"`
	Bounds              boundsDocument  `json:"bounds"`
	Relations           []relationPart  `json:"relations"`
	Select              *selectDocument `json:"select,omitempty"`
	Encode              *encodeDocument `json:"encode,omitempty"`
}

type boundsDocument struct {
	Relations   string `json:"relations"`
	Columns     string `json:"columns"`
	Rows        string `json:"rows"`
	Bytes       string `json:"bytes"`
	TempBytes   string `json:"temp_bytes"`
	MemoryBytes string `json:"memory_bytes"`
	WallMillis  string `json:"wall_millis"`
}

type relationPart struct {
	Name    string           `json:"name"`
	Path    string           `json:"path"`
	SHA256  string           `json:"sha256"`
	Bytes   string           `json:"bytes"`
	Columns []columnDocument `json:"columns"`
}

type projectionPart struct {
	Column string  `json:"column"`
	Alias  *string `json:"alias,omitempty"`
}

type predicatePart struct {
	Column   string            `json:"column"`
	Operator string            `json:"operator"`
	Value    *analytics.Value  `json:"value,omitempty"`
	Values   []analytics.Value `json:"values,omitempty"`
}

type orderingPart struct {
	Column    string `json:"column"`
	Direction string `json:"direction"`
}

type selectDocument struct {
	From       string           `json:"from"`
	Project    []projectionPart `json:"project"`
	Predicates []predicatePart  `json:"predicates"`
	Order      []orderingPart   `json:"order"`
	Limit      string           `json:"limit"`
	Output     *outputDocument  `json:"output,omitempty"`
}

type encodeDocument struct {
	Columns []columnDocument    `json:"columns"`
	Rows    [][]analytics.Value `json:"rows"`
	Output  outputDocument      `json:"output"`
}

// decodeRequest strictly decodes one request document into the engine's typed
// request. It returns the request and the declared identity, which is echoed in
// a refusal so a caller can match its own attempt.
func decodeRequest(document []byte) (analytics.Request, string, error) {
	var request analytics.Request
	var wire requestDocument
	if err := contract.DecodeStrictRequiredObject(document, &wire,
		"protocol", "request_id", "admitted_directories", "bounds", "relations"); err != nil {
		return request, "", protocolFailure("the request document is not a valid %s request: %v", protocolVersion, err)
	}
	if wire.Protocol != protocolVersion {
		return request, "", protocolFailure("the request declares an unsupported protocol version")
	}
	if wire.RequestID == "" || len(wire.RequestID) > 128 || strings.ContainsAny(wire.RequestID, "\x00\n\r") {
		return request, "", protocolFailure("the request identity must be a bounded nonempty value")
	}
	requestID := wire.RequestID
	bounds, err := decodeBounds(wire.Bounds)
	if err != nil {
		return request, requestID, err
	}
	request.Bounds = bounds
	request.AdmittedDirectories = append(request.AdmittedDirectories, wire.AdmittedDirectories...)
	for _, part := range wire.Relations {
		relation, err := decodeRelation(part)
		if err != nil {
			return request, requestID, err
		}
		request.Relations = append(request.Relations, relation)
	}
	if (wire.Select == nil) == (wire.Encode == nil) {
		return request, requestID, protocolFailure("exactly one of the typed select and encode operations must be present")
	}
	if wire.Select != nil {
		selected, err := decodeSelect(*wire.Select)
		if err != nil {
			return request, requestID, err
		}
		request.Select = selected
		return request, requestID, nil
	}
	encoded, err := decodeEncode(*wire.Encode)
	if err != nil {
		return request, requestID, err
	}
	request.Encode = encoded
	return request, requestID, nil
}

func decodeBounds(wire boundsDocument) (analytics.BoundAnalytics, error) {
	var bounds analytics.BoundAnalytics
	relations, err := decodeIntegral(wire.Relations, "bounds.relations")
	if err != nil {
		return bounds, err
	}
	columns, err := decodeIntegral(wire.Columns, "bounds.columns")
	if err != nil {
		return bounds, err
	}
	rows, err := decodeIntegral(wire.Rows, "bounds.rows")
	if err != nil {
		return bounds, err
	}
	bytesBound, err := decodeIntegral(wire.Bytes, "bounds.bytes")
	if err != nil {
		return bounds, err
	}
	tempBytes, err := decodeIntegral(wire.TempBytes, "bounds.temp_bytes")
	if err != nil {
		return bounds, err
	}
	memoryBytes, err := decodeIntegral(wire.MemoryBytes, "bounds.memory_bytes")
	if err != nil {
		return bounds, err
	}
	wallMillis, err := decodeIntegral(wire.WallMillis, "bounds.wall_millis")
	if err != nil {
		return bounds, err
	}
	// The declared bound is range-checked before the conversion: the nanosecond
	// multiplication wraps for a value beyond the capability, and a wrapped
	// value lands inside the accepted range where the documented refusal can
	// never fire.
	switch {
	case wallMillis > maximumWallMillis:
		return bounds, boundFailure("the wall-time bound exceeds the child capability of %s", analytics.MaximumWallTime)
	case wallMillis < -maximumWallMillis:
		return bounds, boundFailure("the wall-time bound is beyond the child capability of %s", analytics.MaximumWallTime)
	}
	bounds = analytics.BoundAnalytics{
		Relations:   int(relations),
		Columns:     int(columns),
		Rows:        rows,
		Bytes:       bytesBound,
		TempBytes:   tempBytes,
		MemoryBytes: memoryBytes,
		WallTime:    time.Duration(wallMillis) * time.Millisecond,
	}
	return bounds, nil
}

// boundFailure reports one declared bound the child capability cannot honour.
func boundFailure(format string, arguments ...any) error {
	return &analytics.Failure{Code: analytics.CodeBoundExceeded, Reason: fmt.Sprintf(format, arguments...)}
}

// decodeIntegral reads one integral member. A JSON number, a zero-padded value,
// or a value outside the signed 64-bit range is refused rather than coerced.
func decodeIntegral(value, member string) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || strconv.FormatInt(parsed, 10) != value {
		return 0, protocolFailure("%s must be a canonical base-10 integer string", member)
	}
	return parsed, nil
}

func decodeRelation(part relationPart) (analytics.Relation, error) {
	var relation analytics.Relation
	if part.Name == "" || part.Path == "" || part.SHA256 == "" {
		return relation, protocolFailure("a relation must declare its name, path, and admitted digest")
	}
	length, err := decodeIntegral(part.Bytes, "relation.bytes")
	if err != nil {
		return relation, err
	}
	columns := make([]analytics.Column, 0, len(part.Columns))
	for _, column := range part.Columns {
		decoded, err := decodeColumn(column)
		if err != nil {
			return relation, err
		}
		columns = append(columns, decoded)
	}
	relation = analytics.Relation{
		Name:    part.Name,
		Path:    part.Path,
		SHA256:  part.SHA256,
		Bytes:   length,
		Columns: columns,
	}
	return relation, nil
}

func decodeColumn(wire columnDocument) (analytics.Column, error) {
	var column analytics.Column
	if wire.Name == "" || wire.Kind == "" {
		return column, protocolFailure("a column must declare its name and kind")
	}
	if wire.Nullable == nil {
		return column, protocolFailure("column %q must declare whether it is nullable", wire.Name)
	}
	kind := analytics.Kind(wire.Kind)
	if !kind.Valid() {
		return column, protocolFailure("column %q declares an unsupported kind", wire.Name)
	}
	column = analytics.Column{Name: wire.Name, Kind: kind, Nullable: *wire.Nullable}
	if kind != analytics.KindDecimal {
		if wire.Precision != nil || wire.Scale != nil {
			return column, protocolFailure("column %q declares precision or scale for a non-decimal kind", wire.Name)
		}
		return column, nil
	}
	if wire.Precision == nil || wire.Scale == nil {
		return column, protocolFailure("decimal column %q must declare its precision and scale", wire.Name)
	}
	precision, err := decodeIntegral(*wire.Precision, "column.precision")
	if err != nil {
		return column, err
	}
	scale, err := decodeIntegral(*wire.Scale, "column.scale")
	if err != nil {
		return column, err
	}
	column.Precision = int(precision)
	column.Scale = int(scale)
	return column, nil
}

func decodeSelect(wire selectDocument) (*analytics.Select, error) {
	if wire.From == "" {
		return nil, protocolFailure("a selection must declare the relation it reads")
	}
	limit, err := decodeIntegral(wire.Limit, "select.limit")
	if err != nil {
		return nil, err
	}
	selected := &analytics.Select{From: wire.From, Limit: limit}
	for _, projection := range wire.Project {
		if projection.Column == "" {
			return nil, protocolFailure("a projection must name a declared column")
		}
		entry := analytics.Projection{Column: analytics.Column{Name: projection.Column}}
		if projection.Alias != nil {
			entry.Alias = *projection.Alias
		}
		selected.Project = append(selected.Project, entry)
	}
	for _, predicate := range wire.Predicates {
		if predicate.Column == "" || predicate.Operator == "" {
			return nil, protocolFailure("a predicate must declare its column and operator")
		}
		entry := analytics.Predicate{
			Column:   predicate.Column,
			Operator: analytics.PredicateOperator(predicate.Operator),
		}
		if predicate.Value != nil {
			entry.Value = *predicate.Value
		}
		entry.Values = append(entry.Values, predicate.Values...)
		selected.Predicates = append(selected.Predicates, entry)
	}
	for _, ordering := range wire.Order {
		if ordering.Column == "" {
			return nil, protocolFailure("an ordering must name a declared column")
		}
		descending := false
		switch ordering.Direction {
		case "asc":
		case "desc":
			descending = true
		default:
			return nil, protocolFailure("an ordering direction must be asc or desc")
		}
		selected.Order = append(selected.Order, analytics.Ordering{Column: ordering.Column, Descending: descending})
	}
	if wire.Output != nil {
		if wire.Output.Path == "" {
			return nil, protocolFailure("an output must declare its admitted path")
		}
		selected.Output = &analytics.Output{Path: wire.Output.Path}
	}
	return selected, nil
}

func decodeEncode(wire encodeDocument) (*analytics.Encode, error) {
	if wire.Output.Path == "" {
		return nil, protocolFailure("an encoded segment must declare its admitted output path")
	}
	columns := make([]analytics.Column, 0, len(wire.Columns))
	for _, column := range wire.Columns {
		decoded, err := decodeColumn(column)
		if err != nil {
			return nil, err
		}
		columns = append(columns, decoded)
	}
	return &analytics.Encode{
		Columns: columns,
		Rows:    wire.Rows,
		Output:  analytics.Output{Path: wire.Output.Path},
	}, nil
}

// protocolFailure reports one protocol-class refusal.
func protocolFailure(format string, arguments ...any) error {
	return &analytics.Failure{Code: analytics.CodeProtocol, Reason: fmt.Sprintf(format, arguments...)}
}

// newColumnDocument renders one declared column in the wire form.
func newColumnDocument(column analytics.Column) columnDocument {
	document := columnDocument{Name: column.Name, Kind: string(column.Kind), Nullable: &column.Nullable}
	if column.Kind == analytics.KindDecimal {
		precision := strconv.Itoa(column.Precision)
		scale := strconv.Itoa(column.Scale)
		document.Precision = &precision
		document.Scale = &scale
	}
	return document
}
