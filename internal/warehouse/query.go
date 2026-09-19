package warehouse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// QuerySnapshot is one retained read view: the exact registered segment set of
// one dataset revision, each segment with the immutable object it registered and
// the schema it declared.
type QuerySnapshot struct {
	Layout    WarehouseLayout
	DatasetID string
	Revision  uint64
	Segments  []QuerySnapshotSegment
}

// QuerySnapshotSegment is one registered segment of a read view.
type QuerySnapshotSegment struct {
	Ordinal uint64
	Object  contract.ObjectRef
	Schema  SegmentSchema
	Rows    int64
}

// QueryService answers bounded typed reads over registered warehouse segments.
// It downloads each registered object into a verified local relation inside the
// child's admitted root, asks the restricted child for the exact typed rows, and
// removes the local staging afterwards: no shared writable file and no native
// engine ever enters the worker.
type QueryService struct {
	pool        *store.Pool
	backend     *objects.S3
	child       *QueryChild
	stagingRoot string
}

// NewQueryService validates the read surface before any query. The staging root
// must be one of the child's admitted roots, so a query can only ever admit
// files this service itself staged there.
func NewQueryService(pool *store.Pool, backend *objects.S3, child *QueryChild, stagingRoot string) (*QueryService, error) {
	if pool == nil || backend == nil || child == nil {
		return nil, errors.New("a query service requires a store pool, an object backend, and a query child supervisor")
	}
	if stagingRoot == "" || !filepath.IsAbs(stagingRoot) || filepath.Clean(stagingRoot) != stagingRoot {
		return nil, refusal(RefusalUnadmittedPath, "the query staging root must be one absolute, cleaned path")
	}
	if err := inspectAdmittedDirectory(stagingRoot); err != nil {
		return nil, err
	}
	if !withinAdmittedRoots(stagingRoot, child.config.AdmittedRoots) {
		return nil, refusal(RefusalUnadmittedPath, "the query staging root must be admitted to the child")
	}
	return &QueryService{pool: pool, backend: backend, child: child, stagingRoot: stagingRoot}, nil
}

// SnapshotReadRequest selects one read view.
type SnapshotReadRequest struct {
	Layout WarehouseLayout
	// SnapshotID selects one retained snapshot's exact set. An empty identity
	// reads the dataset's current live set.
	SnapshotID string
}

// Snapshot resolves one read view without reading any object bytes.
func (service *QueryService) Snapshot(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	request SnapshotReadRequest,
) (QuerySnapshot, error) {
	return service.resolve(ctx, actor, scope, request.Layout, request.SnapshotID, nil)
}

// resolve loads one dataset's registered read set inside the caller's scope. A
// dataset registered for another scope, connector, or connection is refused;
// the store's own scoped reads cannot even observe it.
func (service *QueryService) resolve(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	layout WarehouseLayout,
	snapshotID string,
	ordinals []uint64,
) (QuerySnapshot, error) {
	if err := layout.Validate(); err != nil {
		return QuerySnapshot{}, err
	}
	snapshot := QuerySnapshot{Layout: layout, DatasetID: layout.DatasetID()}
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		dataset, err := store.LoadWarehouseDataset(ctx, tx, layout.DatasetID())
		if err != nil {
			return err
		}
		if dataset.ConnectorID != layout.ConnectorID || dataset.ConnectionID != layout.ConnectionID {
			return store.ErrScopeMismatch
		}
		selected := ordinals
		if snapshotID != "" {
			retained, err := store.LoadWarehouseSnapshot(ctx, tx, snapshotID)
			if err != nil {
				return err
			}
			if retained.DatasetID != layout.DatasetID() {
				return store.ErrScopeMismatch
			}
			selected = retained.Segments
			snapshot.Revision = retained.ManifestRevision
		}
		segments, err := store.LoadWarehouseSegments(ctx, tx, layout.DatasetID())
		if err != nil {
			return err
		}
		for _, segment := range segments {
			if selected == nil && segment.State != store.WarehouseSegmentLive {
				continue
			}
			if selected != nil && !containsOrdinal(selected, segment.Ordinal) {
				continue
			}
			if segment.State == store.WarehouseSegmentCollected {
				return refusal(RefusalUnadmittedPath,
					"registered segment %d of dataset %q was collected and can no longer be read",
					segment.Ordinal, layout.DatasetID())
			}
			schema, err := DecodeSegmentSchema(segment.Schema)
			if err != nil {
				return err
			}
			if schema.Fingerprint() != segment.ColumnFingerprint {
				return store.ErrImmutableConflict
			}
			snapshot.Segments = append(snapshot.Segments, QuerySnapshotSegment{
				Ordinal: segment.Ordinal, Object: segment.Object, Schema: schema, Rows: segment.RowCount,
			})
			if segment.Ordinal > snapshot.Revision {
				snapshot.Revision = segment.Ordinal
			}
		}
		return nil
	})
	if err != nil {
		return QuerySnapshot{}, shapeReadRefusal(layout, err)
	}
	if len(snapshot.Segments) == 0 {
		return QuerySnapshot{}, refusal(RefusalUnknownRelation,
			"dataset %q exposes no readable registered segment for this request", layout.DatasetID())
	}
	return snapshot, nil
}

// shapeReadRefusal keeps this package's refusals typed while preserving the
// store's own error identity for a caller that needs it.
func shapeReadRefusal(layout WarehouseLayout, err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return refusal(RefusalUnknownRelation,
			"dataset %q has no registered segment for its scope, connector, and connection", layout.DatasetID())
	case errors.Is(err, store.ErrScopeMismatch):
		return refusal(RefusalUnadmittedPath,
			"dataset %q is bound to another connection or scope", layout.DatasetID())
	default:
		return err
	}
}

func containsOrdinal(values []uint64, wanted uint64) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// QueryParameterDeclaration declares one typed parameter of a saved query.
type QueryParameterDeclaration struct {
	Name      string
	Kind      QueryKind
	Nullable  bool
	Precision int
	Scale     int
}

// QueryParameter binds one declared parameter to an exact typed value.
type QueryParameter struct {
	Name  string
	Kind  QueryKind
	Value QueryValue
}

// SavedPredicate is one typed comparison. It carries a literal operand, an
// in-list, or the name of a declared parameter; it never carries statement text.
type SavedPredicate struct {
	Column    string
	Operator  QueryOperator
	Value     QueryValue
	Values    []QueryValue
	Parameter string
}

// SavedQuery is one admitted typed query over the dataset's registered relation.
// Its parameters are declared here and bound by the caller; the protocol the
// child receives never contains a statement.
type SavedQuery struct {
	Name       string
	Relation   string
	Project    []QueryProjection
	Predicates []SavedPredicate
	Order      []QueryOrder
	Limit      int64
	Parameters []QueryParameterDeclaration
}

// DatasetQueryRequest is one bounded read of a registered dataset.
type DatasetQueryRequest struct {
	Actor contract.Actor
	Scope contract.Scope
	// Layout identifies the dataset, its connector, and its connection.
	Layout WarehouseLayout
	// QueryID identifies this read; its staging directory is private to it.
	QueryID string
	// SnapshotID reads one retained snapshot instead of the live set.
	SnapshotID string
	Query      SavedQuery
	Bindings   []QueryParameter
	// MaximumSegmentBytes bounds one registered object this read may stage.
	MaximumSegmentBytes int64
	Bounds              QueryBounds
}

// Query answers one bounded typed read of the dataset's registered relation.
// Every refusal this method can produce happens before any object byte is read
// and before any child process exists: identifiers, parameters, and bounds are
// validated against the registered declaration first.
func (service *QueryService) Query(
	ctx context.Context,
	request DatasetQueryRequest,
) (QueryResponse, error) {
	if ctx == nil {
		return QueryResponse{}, refusal(RefusalProtocol, "a query requires a non-nil context")
	}
	if err := request.Bounds.Validate(); err != nil {
		return QueryResponse{}, err
	}
	if request.MaximumSegmentBytes <= 0 {
		return QueryResponse{}, refusal(RefusalBoundExceeded, "a query requires a positive segment byte cap")
	}
	if err := validateWarehouseRequestID(request.QueryID); err != nil {
		return QueryResponse{}, err
	}
	if request.SnapshotID != "" {
		if err := validateWarehouseRequestID(request.SnapshotID); err != nil {
			return QueryResponse{}, err
		}
	}
	snapshot, err := service.resolve(ctx, request.Actor, request.Scope, request.Layout, request.SnapshotID, nil)
	if err != nil {
		return QueryResponse{}, err
	}
	selectRequest, err := resolveSavedQuery(request.Query, request.Bindings, snapshot, request.Bounds,
		service.stagedPath(request.QueryID, snapshot.Segments[0].Ordinal))
	if err != nil {
		return QueryResponse{}, err
	}
	return service.Read(ctx, ReadRequest{
		Actor:               request.Actor,
		Scope:               request.Scope,
		Layout:              request.Layout,
		QueryID:             request.QueryID,
		Select:              selectRequest,
		Segments:            snapshot.Segments,
		MaximumSegmentBytes: request.MaximumSegmentBytes,
		Bounds:              request.Bounds,
	})
}

// ReadRequest is one bounded typed read of an explicit registered segment set.
type ReadRequest struct {
	Actor   contract.Actor
	Scope   contract.Scope
	Layout  WarehouseLayout
	QueryID string
	// Select is the resolved typed request against the dataset's relation.
	Select QuerySelect
	// Segments are the registered segments to read, in ordinal order.
	Segments            []QuerySnapshotSegment
	MaximumSegmentBytes int64
	Bounds              QueryBounds
}

// Read stages every named registered object, executes one bounded typed request
// per segment through the restricted child, and merges the exact rows in
// ordinal order.
func (service *QueryService) Read(ctx context.Context, request ReadRequest) (QueryResponse, error) {
	var response QueryResponse
	if ctx == nil {
		return response, refusal(RefusalProtocol, "a read requires a non-nil context")
	}
	if err := request.Layout.Validate(); err != nil {
		return response, err
	}
	if request.Select.From != request.Layout.RelationName() {
		return response, refusal(RefusalUnknownRelation,
			"the read names relation %q, which this dataset does not expose", request.Select.From)
	}
	if len(request.Segments) == 0 {
		return response, refusal(RefusalUnknownRelation, "the read names no registered segment")
	}
	if request.MaximumSegmentBytes <= 0 {
		return response, refusal(RefusalBoundExceeded, "a read requires a positive segment byte cap")
	}
	if err := validateWarehouseRequestID(request.QueryID); err != nil {
		return response, err
	}
	fingerprint := request.Segments[0].Schema.Fingerprint()
	var totalBytes int64
	for _, segment := range request.Segments {
		if segment.Object.ByteLength <= 0 || segment.Object.ByteLength > request.MaximumSegmentBytes {
			return response, refusal(RefusalBoundExceeded, "a registered segment exceeds the admitted byte cap")
		}
		if segment.Schema.Fingerprint() != fingerprint {
			return response, refusal(RefusalSchemaMismatch, "registered segments disagree on the dataset schema")
		}
		totalBytes += segment.Object.ByteLength
	}
	if totalBytes > request.Bounds.Bytes {
		return response, refusal(RefusalBoundExceeded, "the registered segments exceed the declared byte bound")
	}

	directory := filepath.Join(service.stagingRoot, request.QueryID)
	if err := os.Mkdir(directory, 0o700); err != nil {
		return response, refusal(RefusalUnadmittedPath, "the query staging directory is unavailable: %v", err)
	}
	defer func() { _ = os.RemoveAll(directory) }()

	relations := make([]QueryRelation, 0, len(request.Segments))
	for _, segment := range request.Segments {
		relations = append(relations, QueryRelation{
			Name:    request.Layout.RelationName(),
			Path:    service.stagedPath(request.QueryID, segment.Ordinal),
			SHA256:  "sha256:" + segment.Object.SHA256,
			Bytes:   segment.Object.ByteLength,
			Columns: segment.Schema.Columns(),
		})
	}
	// The declared selection is validated against the registered declaration
	// before any registered byte is staged or any child process exists.
	if err := request.Select.Validate(map[string]QueryRelation{relations[0].Name: relations[0]}, request.Bounds); err != nil {
		return response, err
	}
	for index := range relations {
		if err := service.stageSegment(ctx, relations[index].Path, request.Segments[index]); err != nil {
			return response, err
		}
	}

	responses := make([]QueryResponse, 0, len(relations))
	for _, relation := range relations {
		segmentSelect := request.Select
		segmentBounds := request.Bounds
		if segmentBounds.Rows > segmentSelect.Limit {
			segmentBounds.Rows = segmentSelect.Limit
		}
		segmentResponse, err := service.child.Execute(ctx, QueryRequest{
			RequestID:           request.QueryID,
			AdmittedDirectories: []string{directory},
			Bounds:              segmentBounds,
			Relations:           []QueryRelation{relation},
			Select:              &segmentSelect,
		})
		if err != nil {
			return QueryResponse{}, err
		}
		responses = append(responses, segmentResponse)
	}
	rows, truncated, err := mergeSegmentResponses(request.Select.Order, responses, request.Select.Limit)
	if err != nil {
		return QueryResponse{}, err
	}
	response.RequestID = request.QueryID
	response.Columns = responses[0].Columns
	response.Rows = rows
	response.Truncated = truncated
	response.Child = responses[0].Child
	return response, nil
}

// stagedPath is the deterministic local identity of one staged relation. It is
// known before any byte is read, so a query's declared relation can be validated
// against the registered schema before staging begins.
func (service *QueryService) stagedPath(queryID string, ordinal uint64) string {
	return filepath.Join(service.stagingRoot, queryID, fmt.Sprintf("segment-%020d.parquet", ordinal))
}

// stageSegment downloads one registered object into the admitted staging
// directory and re-verifies its digest and length while writing.
func (service *QueryService) stageSegment(ctx context.Context, path string, segment QuerySnapshotSegment) error {
	body, err := service.backend.OpenVerified(ctx, segment.Object, objects.ByteRange{Length: segment.Object.ByteLength})
	if err != nil {
		return refusal(RefusalDigestMismatch, "the registered segment object is unavailable or corrupt: %v", err)
	}
	defer func() { _ = body.Close() }()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return refusal(RefusalUnadmittedPath, "create the staged relation: %v", err)
	}
	digest := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, digest), io.LimitReader(body, segment.Object.ByteLength+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil ||
		written != segment.Object.ByteLength ||
		hex.EncodeToString(digest.Sum(nil)) != segment.Object.SHA256 {
		_ = os.Remove(path)
		return refusal(RefusalDigestMismatch, "the staged relation does not match the registered segment identity")
	}
	return nil
}

// materializeSegment writes one immutable segment into a private directory under
// the admitted staging root and returns its verified identity beside the cleanup
// that removes the local copy. The compacted bytes must survive until the
// publication has uploaded them, so the caller owns the cleanup.
func (service *QueryService) materializeSegment(
	ctx context.Context,
	directoryName string,
	columns []QueryColumn,
	rows [][]QueryValue,
	bounds QueryBounds,
	maximumBytes int64,
) (SegmentBytes, func(), error) {
	cleanup := func() {}
	if err := validateWarehouseRequestID(directoryName); err != nil {
		return SegmentBytes{}, cleanup, err
	}
	directory := filepath.Join(service.stagingRoot, directoryName)
	if err := os.Mkdir(directory, 0o700); err != nil {
		return SegmentBytes{}, cleanup, refusal(RefusalUnadmittedPath, "the materialization directory is unavailable: %v", err)
	}
	cleanup = func() { _ = os.RemoveAll(directory) }
	segment, err := MaterializeParquetSegment(ctx, service.child, MaterializeRequest{
		RequestID:           directoryName,
		AdmittedDirectories: []string{directory},
		OutputPath:          filepath.Join(directory, "segment.parquet"),
		Columns:             columns,
		Rows:                rows,
		Bounds:              bounds,
		MaximumBytes:        maximumBytes,
	})
	if err != nil {
		cleanup()
		return SegmentBytes{}, func() {}, err
	}
	return segment, cleanup, nil
}

// resolveSavedQuery validates one saved query against the registered dataset
// declaration and substitutes its bound parameters. It never produces statement
// text: the child receives the same typed structure a literal query would.
func resolveSavedQuery(
	query SavedQuery,
	bindings []QueryParameter,
	snapshot QuerySnapshot,
	bounds QueryBounds,
	stagedPath string,
) (QuerySelect, error) {
	var resolved QuerySelect
	if err := validateQueryIdentifier(query.Name); err != nil {
		return resolved, err
	}
	if query.Relation != snapshot.Layout.RelationName() {
		return resolved, refusal(RefusalUnknownRelation,
			"dataset %q exposes relation %q, not %q", snapshot.DatasetID, snapshot.Layout.RelationName(), query.Relation)
	}
	declared := make(map[string]QueryParameterDeclaration, len(query.Parameters))
	for _, parameter := range query.Parameters {
		column := QueryColumn{
			Name: parameter.Name, Kind: parameter.Kind,
			Precision: parameter.Precision, Scale: parameter.Scale, Nullable: parameter.Nullable,
		}
		if err := column.Validate(); err != nil {
			return resolved, err
		}
		if _, duplicate := declared[parameter.Name]; duplicate {
			return resolved, refusal(RefusalProtocol, "parameter %q is declared more than once", parameter.Name)
		}
		declared[parameter.Name] = parameter
	}
	bound := make(map[string]QueryParameter, len(bindings))
	for _, parameter := range bindings {
		if err := validateQueryIdentifier(parameter.Name); err != nil {
			return resolved, err
		}
		declaration, ok := declared[parameter.Name]
		if !ok {
			return resolved, refusal(RefusalProtocol, "the saved query does not declare parameter %q", parameter.Name)
		}
		if _, duplicate := bound[parameter.Name]; duplicate {
			return resolved, refusal(RefusalProtocol, "parameter %q is bound more than once", parameter.Name)
		}
		if parameter.Kind != declaration.Kind {
			return resolved, refusal(RefusalSchemaMismatch,
				"parameter %q is declared %s but bound as %s", parameter.Name, declaration.Kind, parameter.Kind)
		}
		if !parameter.Value.Present() {
			return resolved, refusal(RefusalProtocol, "parameter %q carries no value", parameter.Name)
		}
		if parameter.Value.IsNull() {
			if !declaration.Nullable {
				return resolved, refusal(RefusalSchemaMismatch, "parameter %q is not nullable", parameter.Name)
			}
		} else if err := parameter.Value.Validate(); err != nil {
			return resolved, err
		}
		bound[parameter.Name] = parameter
	}
	for name := range declared {
		if _, ok := bound[name]; !ok {
			return resolved, refusal(RefusalProtocol, "parameter %q is declared but not bound", name)
		}
	}

	predicates := make([]QueryPredicate, 0, len(query.Predicates))
	for _, predicate := range query.Predicates {
		resolvedPredicate := QueryPredicate{Column: predicate.Column, Operator: predicate.Operator}
		switch {
		case predicate.Parameter != "":
			if predicate.Value.Present() || len(predicate.Values) != 0 {
				return resolved, refusal(RefusalProtocol, "predicate on %q carries both a parameter and a literal operand", predicate.Column)
			}
			if !consumesSingleOperand(predicate.Operator) {
				return resolved, refusal(RefusalProtocol, "operator %q does not consume one parameter operand", predicate.Operator)
			}
			parameter, ok := bound[predicate.Parameter]
			if !ok {
				return resolved, refusal(RefusalProtocol, "predicate on %q consumes the undeclared parameter %q",
					predicate.Column, predicate.Parameter)
			}
			resolvedPredicate.Value = parameter.Value
		default:
			resolvedPredicate.Value = predicate.Value
			resolvedPredicate.Values = predicate.Values
		}
		predicates = append(predicates, resolvedPredicate)
	}

	relation := QueryRelation{
		Name:    snapshot.Layout.RelationName(),
		Path:    stagedPath,
		SHA256:  "sha256:" + snapshot.Segments[0].Object.SHA256,
		Bytes:   snapshot.Segments[0].Object.ByteLength,
		Columns: snapshot.Segments[0].Schema.Columns(),
	}
	resolved = QuerySelect{
		From:       query.Relation,
		Project:    query.Project,
		Predicates: predicates,
		Order:      query.Order,
		Limit:      query.Limit,
	}
	if err := resolved.Validate(map[string]QueryRelation{relation.Name: relation}, bounds); err != nil {
		return QuerySelect{}, err
	}
	return resolved, nil
}

// consumesSingleOperand reports whether one operator consumes a single typed
// operand. An in-list or a null test is declared literally; a parameter never
// changes the operator's arity.
func consumesSingleOperand(operator QueryOperator) bool {
	switch operator {
	case QueryOperatorEqual, QueryOperatorNotEqual, QueryOperatorLessThan, QueryOperatorLessEqual,
		QueryOperatorGreaterThan, QueryOperatorGreaterEqual:
		return true
	default:
		return false
	}
}

// allColumnProjections projects every declared column in its declared order.
func allColumnProjections(columns []QueryColumn) []QueryProjection {
	projections := make([]QueryProjection, 0, len(columns))
	for _, column := range columns {
		projections = append(projections, QueryProjection{Column: column.Name})
	}
	return projections
}

// validateWarehouseRequestID bounds one caller-supplied request identity so it
// can never carry path or statement text.
func validateWarehouseRequestID(value string) error {
	if value == "" || len(value) > MaximumQueryIdentifierBytes {
		return refusal(RefusalProtocol, "a request identity must be a bounded nonempty value")
	}
	for _, character := range value {
		valid := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_'
		if !valid {
			return refusal(RefusalProtocol, "a request identity must use only identifier characters")
		}
	}
	return nil
}

// mergeSegmentResponses concatenates the exact rows of several registered
// segments. A declared ordering is applied across segments only when every
// ordering key is present in every row: the protocol declares no null placement,
// so a merge over a nullable ordering key is refused rather than guessed.
func mergeSegmentResponses(
	order []QueryOrder,
	responses []QueryResponse,
	limit int64,
) ([][]QueryValue, bool, error) {
	truncated := false
	rows := make([][]QueryValue, 0, len(responses))
	for _, response := range responses {
		if response.Truncated {
			truncated = true
		}
		rows = append(rows, response.Rows...)
	}
	if len(order) > 0 && len(responses) > 1 {
		for _, decision := range order {
			if decision.Column == "" {
				return nil, false, refusal(RefusalProtocol, "an ordering must name a registered column")
			}
		}
		position := make(map[string]int, len(order))
		for index, column := range responses[0].Columns {
			position[column.Name] = index
		}
		for _, row := range rows {
			for _, decision := range order {
				index, declared := position[decision.Column]
				if !declared {
					return nil, false, refusal(RefusalUnknownColumn, "the ordering names column %q, which is not projected", decision.Column)
				}
				if row[index].IsNull() {
					return nil, false, refusal(RefusalProtocol,
						"a cross-segment ordering over the nullable column %q has no declared null placement", decision.Column)
				}
			}
		}
		var compareErr error
		sort.SliceStable(rows, func(left, right int) bool {
			if compareErr != nil {
				return false
			}
			comparison, err := compareOrderedRows(order, responses[0].Columns, position, rows[left], rows[right])
			if err != nil {
				compareErr = err
				return false
			}
			return comparison < 0
		})
		if compareErr != nil {
			return nil, false, compareErr
		}
	}
	if int64(len(rows)) > limit {
		rows = rows[:limit]
		truncated = true
	}
	return rows, truncated, nil
}

// compareOrderedRows compares two exact typed rows by the declared ordering.
func compareOrderedRows(
	order []QueryOrder,
	columns []QueryColumn,
	position map[string]int,
	left []QueryValue,
	right []QueryValue,
) (int, error) {
	for _, decision := range order {
		index := position[decision.Column]
		comparison, err := compareQueryValues(columns[index].Kind, left[index], right[index])
		if err != nil {
			return 0, err
		}
		if decision.Descending {
			comparison = -comparison
		}
		if comparison != 0 {
			return comparison, nil
		}
	}
	return 0, nil
}

// compareQueryValues compares two present, non-null values of one declared kind.
func compareQueryValues(kind QueryKind, left, right QueryValue) (int, error) {
	switch kind {
	case QueryKindInt64, QueryKindTimestamp, QueryKindTimestampTZ:
		leftValue, _ := left.Int64()
		rightValue, _ := right.Int64()
		return compareInt64(leftValue, rightValue), nil
	case QueryKindDecimal:
		leftUnscaled, leftScale, _ := left.Unscaled()
		rightUnscaled, rightScale, _ := right.Unscaled()
		if leftScale != rightScale {
			return 0, refusal(RefusalSchemaMismatch, "a cross-segment ordering cannot compare two decimal scales")
		}
		return compareSignedIntegers(leftUnscaled, rightUnscaled), nil
	case QueryKindText, QueryKindDate, QueryKindUUID:
		leftText, _ := left.Text()
		rightText, _ := right.Text()
		return strings.Compare(leftText, rightText), nil
	case QueryKindBoolean:
		leftValue, _ := left.Boolean()
		rightValue, _ := right.Boolean()
		switch {
		case leftValue == rightValue:
			return 0, nil
		case !leftValue:
			return -1, nil
		default:
			return 1, nil
		}
	case QueryKindBytea:
		leftBytes, _ := left.Bytes()
		rightBytes, _ := right.Bytes()
		return bytes.Compare(leftBytes, rightBytes), nil
	default:
		return 0, refusal(RefusalProtocol, "a cross-segment ordering over a %s column is unsupported", kind)
	}
}

func compareInt64(left, right int64) int {
	switch {
	case left < right:
		return -1
	case left > right:
		return 1
	default:
		return 0
	}
}

// compareSignedIntegers compares two exact signed integer renderings without
// losing precision or overflowing.
func compareSignedIntegers(left, right string) int {
	leftNegative := strings.HasPrefix(left, "-")
	rightNegative := strings.HasPrefix(right, "-")
	leftDigits := strings.TrimLeft(strings.TrimPrefix(left, "-"), "0")
	rightDigits := strings.TrimLeft(strings.TrimPrefix(right, "-"), "0")
	if leftDigits == "" {
		leftDigits = "0"
	}
	if rightDigits == "" {
		rightDigits = "0"
	}
	leftZero := leftDigits == "0"
	rightZero := rightDigits == "0"
	if leftZero && rightZero {
		return 0
	}
	// A negative zero is the same value as zero.
	if leftZero {
		leftNegative = false
	}
	if rightZero {
		rightNegative = false
	}
	switch {
	case leftNegative != rightNegative:
		if leftNegative {
			return -1
		}
		return 1
	case len(leftDigits) != len(rightDigits):
		comparison := 1
		if len(leftDigits) < len(rightDigits) {
			comparison = -1
		}
		if leftNegative {
			return -comparison
		}
		return comparison
	default:
		comparison := strings.Compare(leftDigits, rightDigits)
		if leftNegative {
			return -comparison
		}
		return comparison
	}
}
