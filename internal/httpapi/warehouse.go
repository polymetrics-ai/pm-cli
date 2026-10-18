package httpapi

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/oapi-codegen/nullable"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/warehouse"
)

// The warehouse read routes project the exact committed read view of one
// connection's dataset and the exact typed rows of one admitted query. They add
// no execution authority: a dataset is only reachable through an authorized
// connection, the object bytes are read by the bounded query child under this
// service's own staging root, and no route here publishes, retires, or retains
// anything. Statement text is never accepted, so nothing on this surface can
// name a file, an extension, or a relation outside the dataset.
var errUnprojectableValue = errors.New("the warehouse returned a value this surface cannot project exactly")

// WarehouseResources are the composition root's warehouse read dependencies.
// Every field is required: an absent object backend, restricted query child,
// staging root, or bound is an explicitly unavailable capability rather than an
// empty read.
type WarehouseResources struct {
	Pool                *store.Pool
	Backend             *objects.S3
	Child               *warehouse.QueryChild
	StagingRoot         string
	MaximumSegmentBytes int64
	Bounds              warehouse.QueryBounds
}

// warehouseSurface owns the admitted read surface of one process. A service
// without installed resources holds no surface at all.
type warehouseSurface struct {
	queries             *warehouse.QueryService
	maximumSegmentBytes int64
	bounds              warehouse.QueryBounds
}

// newWarehouseSurface validates the installed resources before any request can
// reach them. Absent resources produce no surface rather than a default one.
func newWarehouseSurface(resources *WarehouseResources) (*warehouseSurface, error) {
	if resources == nil {
		return nil, nil
	}
	if resources.MaximumSegmentBytes <= 0 {
		return nil, errors.New("the warehouse read surface requires a positive segment byte cap")
	}
	if err := resources.Bounds.Validate(); err != nil {
		return nil, err
	}
	queries, err := warehouse.NewQueryService(resources.Pool, resources.Backend, resources.Child, resources.StagingRoot)
	if err != nil {
		return nil, fmt.Errorf("construct the warehouse query service: %w", err)
	}
	return &warehouseSurface{
		queries:             queries,
		maximumSegmentBytes: resources.MaximumSegmentBytes,
		bounds:              resources.Bounds,
	}, nil
}

// warehouseLayout composes the dataset namespace of one authorized connection.
// The connection identity is never taken from the caller: it comes from the
// stored connection after the same grant check every connection read performs.
func (service *Service) warehouseLayout(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	connectionID string,
	dataset string,
) (warehouse.WarehouseLayout, error) {
	connection, err := service.identity.InspectConnection(ctx, actor, scope, connectionID)
	if err != nil {
		return warehouse.WarehouseLayout{}, err
	}
	return warehouse.NewWarehouseLayout(connection.ConnectorID, connection.ConnectionID, dataset)
}

// GetWarehouseDataset reports the committed read view of one dataset: its
// manifest revision, its acknowledged cursor, and the registered immutable
// segments that currently serve it.
func (service *Service) GetWarehouseDataset(
	ctx context.Context,
	request GetWarehouseDatasetRequestObject,
) (GetWarehouseDatasetResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	surface, err := service.warehouseReadSurface()
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	layout, err := service.warehouseLayout(ctx, state.identity.Actor, scope, request.ConnectionId, request.Dataset)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	snapshotID := optionalString(request.Params.SnapshotId)
	view, err := surface.queries.Snapshot(ctx, state.identity.Actor, scope, warehouse.SnapshotReadRequest{
		Layout: layout, SnapshotID: snapshotID,
	})
	if err != nil {
		return asProblemResponse(concealedWarehouseSelector(err), nil), nil
	}
	projected, err := warehouseSnapshotResponse(scope, layout, snapshotID, view)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	return GetWarehouseDataset200JSONResponse(projected), nil
}

// RunWarehouseQuery answers one bounded typed query over the dataset's
// registered segments. Every refusal the query service produces happens before
// any object byte is read, and cancellation reaches it through the standard
// request context.
func (service *Service) RunWarehouseQuery(
	ctx context.Context,
	request RunWarehouseQueryRequestObject,
) (RunWarehouseQueryResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if request.Body == nil {
		return invalidRequestResponse(), nil
	}
	surface, err := service.warehouseReadSurface()
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	layout, err := service.warehouseLayout(ctx, state.identity.Actor, scope, request.ConnectionId, request.Dataset)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	query, bindings, err := admittedWarehouseQuery(*request.Body)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	snapshotID := nullableMemberText(request.Body.SnapshotId)
	// The requested projections are checked against the registered declaration
	// before the query runs, so a column this surface cannot project exactly is
	// a corrected request rather than a partial row set.
	view, err := surface.queries.Snapshot(ctx, state.identity.Actor, scope, warehouse.SnapshotReadRequest{
		Layout: layout, SnapshotID: snapshotID,
	})
	if err != nil {
		return asProblemResponse(concealedWarehouseSelector(err), nil), nil
	}
	if err := requireProjectableColumns(view, query); err != nil {
		return asProblemResponse(err, nil), nil
	}
	response, err := surface.queries.Query(ctx, warehouse.DatasetQueryRequest{
		Actor:               state.identity.Actor,
		Scope:               scope,
		Layout:              layout,
		QueryID:             warehouseQueryID(),
		SnapshotID:          snapshotID,
		Query:               query,
		Bindings:            bindings,
		MaximumSegmentBytes: surface.maximumSegmentBytes,
		Bounds:              surface.bounds,
	})
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	projected, err := warehouseQueryResponse(scope, layout, snapshotID, response)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	return RunWarehouseQuery200JSONResponse(projected), nil
}

// warehouseQueryID derives one fresh, unguessable identity for a single query
// execution. The query service stages each execution's registered objects in a
// private directory named by this identity, so concurrent queries never share
// or collide on one staging directory.
func warehouseQueryID() string {
	return "warehouse-query-" + rand.Text()
}

// concealedWarehouseSelector translates the query service's refusals about the
// caller's own selector into the shared concealment refusal. The selector —
// the dataset the caller names and the retained snapshot it asks for — is
// correctable input, so a read view this authenticated scope cannot reach is
// refused exactly like a restricted artifact: one 404 that does not disclose
// whether the dataset, the snapshot or the binding exists elsewhere, never an
// internal fault. Both read routes conceal the resolution step they take before
// reading anything, and that step answers a selector this scope cannot reach with
// the supervisor's unknown_relation and unadmitted_path refusals, so both codes
// are concealed here; a refusal the later read step produces is passed through
// unchanged, because no selector answers a corrupt, collected or unreadable
// registered segment. One resolution-step refusal is not about the selector's
// reachability — a selected segment that was collected and can no longer be read
// — and it shares the unadmitted_path code, so this mapping would conceal it as
// well. That arm is unreachable while retention never collects a segment a
// snapshot still holds and neither resolution caller selects an explicit
// ordinal; it is named here rather than silently relied on.
func concealedWarehouseSelector(err error) error {
	var refusal *warehouse.Refusal
	if !errors.As(err, &refusal) {
		return err
	}
	switch refusal.Code {
	case warehouse.RefusalUnknownRelation, warehouse.RefusalUnadmittedPath:
		return fmt.Errorf("%w: the requested dataset read view is unavailable in the authenticated scope",
			auth.ErrResourceNotFound)
	default:
		return err
	}
}

// warehouseReadSurface reports the installed surface or an explicit unavailable
// capability. A deployment without warehouse resources is unavailable, never an
// empty dataset.
func (service *Service) warehouseReadSurface() (*warehouseSurface, error) {
	if service.warehouse == nil {
		return nil, fmt.Errorf("warehouse reads are unavailable: %w", capabilityUnavailable())
	}
	return service.warehouse, nil
}

// requireProjectableColumns refuses a projection whose declared kind this wire
// form cannot carry exactly. The registered declaration decides, so a caller
// cannot obtain an approximate rendering by asking for one.
func requireProjectableColumns(view warehouse.QuerySnapshot, query warehouse.SavedQuery) error {
	declared := make(map[string]warehouse.QueryKind, len(view.Segments))
	for _, segment := range view.Segments {
		for _, column := range segment.Schema.Columns() {
			declared[column.Name] = column.Kind
		}
	}
	for _, projection := range query.Project {
		kind, present := declared[projection.Column]
		if !present {
			return fmt.Errorf("%w: the dataset declares no column %q", errInvalidHTTPInput, projection.Column)
		}
		if !projectableWarehouseKind(kind) {
			return fmt.Errorf("%w: column %q of kind %s has no exact wire projection",
				errInvalidHTTPInput, projection.Column, kind)
		}
	}
	return nil
}

// admittedWarehouseQuery converts one wire query into the admitted typed query
// the warehouse service resolves. Identifiers, projections, predicates, order
// and parameter bindings are all declared; no member of the request can carry
// statement text.
func admittedWarehouseQuery(document WarehouseQueryRequest) (warehouse.SavedQuery, []warehouse.QueryParameter, error) {
	if err := requireWarehouseIdentifier("relation", document.Relation, true); err != nil {
		return warehouse.SavedQuery{}, nil, err
	}
	query := warehouse.SavedQuery{
		Name:     document.Relation,
		Relation: document.Relation,
		Limit:    document.Limit,
	}
	if len(document.Project) == 0 {
		return warehouse.SavedQuery{}, nil, fmt.Errorf("%w: a query must project at least one column", errInvalidHTTPInput)
	}
	for _, projection := range document.Project {
		if err := requireWarehouseIdentifier("project", projection.Column, true); err != nil {
			return warehouse.SavedQuery{}, nil, err
		}
		if err := requireWarehouseIdentifier("project.alias", nullableMemberText(projection.Alias), false); err != nil {
			return warehouse.SavedQuery{}, nil, err
		}
		query.Project = append(query.Project, warehouse.QueryProjection{
			Column: projection.Column,
			Alias:  nullableMemberText(projection.Alias),
		})
	}
	for _, predicate := range document.Predicates {
		if err := requireWarehouseIdentifier("predicates.column", predicate.Column, true); err != nil {
			return warehouse.SavedQuery{}, nil, err
		}
		admitted := warehouse.SavedPredicate{
			Column:   predicate.Column,
			Operator: warehouse.QueryOperator(predicate.Operator),
		}
		if parameter := nullableMemberText(predicate.Parameter); parameter != "" {
			if err := requireWarehouseIdentifier("predicates.parameter", parameter, true); err != nil {
				return warehouse.SavedQuery{}, nil, err
			}
			admitted.Parameter = parameter
			query.Predicates = append(query.Predicates, admitted)
			continue
		}
		if takesScalarOperand(admitted.Operator) {
			value, err := admittedQueryValue(predicate.Value)
			if err != nil {
				return warehouse.SavedQuery{}, nil, err
			}
			admitted.Value = value
		} else if predicate.Value.Presence() != contract.PresenceAbsent {
			// A null test carries no operand and an in-list carries its list, so
			// a scalar here is refused rather than silently dropped.
			return warehouse.SavedQuery{}, nil, fmt.Errorf("%w: predicate operator %q does not take a scalar operand",
				errInvalidHTTPInput, predicate.Operator)
		}
		for _, element := range predicate.Values {
			bound, err := admittedQueryValue(element)
			if err != nil {
				return warehouse.SavedQuery{}, nil, err
			}
			admitted.Values = append(admitted.Values, bound)
		}
		query.Predicates = append(query.Predicates, admitted)
	}
	for _, ordering := range document.Order {
		if err := requireWarehouseIdentifier("order.column", ordering.Column, true); err != nil {
			return warehouse.SavedQuery{}, nil, err
		}
		query.Order = append(query.Order, warehouse.QueryOrder{
			Column:     ordering.Column,
			Descending: ordering.Descending,
		})
	}
	bindings := make([]warehouse.QueryParameter, 0, len(document.Parameters))
	for _, parameter := range document.Parameters {
		if err := requireWarehouseIdentifier("parameters.name", parameter.Name, true); err != nil {
			return warehouse.SavedQuery{}, nil, err
		}
		value, err := admittedQueryValue(parameter.Value)
		if err != nil {
			return warehouse.SavedQuery{}, nil, err
		}
		declaration, err := declareQueryParameter(parameter.Name, value)
		if err != nil {
			return warehouse.SavedQuery{}, nil, err
		}
		query.Parameters = append(query.Parameters, declaration)
		bindings = append(bindings, warehouse.QueryParameter{
			Name: parameter.Name, Kind: declaration.Kind, Value: value,
		})
	}
	return query, bindings, nil
}

// takesScalarOperand reports whether one predicate operator consumes the scalar
// operand. A null test consumes no operand and an in-list consumes only its
// list; every other operator, including one the warehouse refuses later,
// consumes the scalar.
func takesScalarOperand(operator warehouse.QueryOperator) bool {
	switch operator {
	case warehouse.QueryOperatorIsNull, warehouse.QueryOperatorIsNotNull,
		warehouse.QueryOperatorIn, warehouse.QueryOperatorNotIn:
		return false
	default:
		return true
	}
}

// declareQueryParameter declares one parameter from the exact value the caller
// bound, so a declaration and its binding can never disagree.
func declareQueryParameter(name string, value warehouse.QueryValue) (warehouse.QueryParameterDeclaration, error) {
	declaration := warehouse.QueryParameterDeclaration{Name: name, Kind: value.Kind(), Nullable: value.IsNull()}
	if !warehouse.QueryKind(declaration.Kind).Valid() {
		return warehouse.QueryParameterDeclaration{}, fmt.Errorf("%w: parameter %q carries no admitted value kind",
			errInvalidHTTPInput, name)
	}
	if unscaled, scale, ok := value.Unscaled(); ok {
		declaration.Precision = decimalPrecision(unscaled)
		declaration.Scale = scale
	}
	return declaration, nil
}

// admittedQueryValue converts one exact wire value into the warehouse's own
// exact value class. The wire form is the shared typed value union, so absence,
// explicit null, false, zero, empty text and exact decimals are preserved, and
// a shape this surface cannot bind exactly is refused.
func admittedQueryValue(value PresentValue) (warehouse.QueryValue, error) {
	switch value.Presence() {
	case contract.PresenceAbsent:
		return warehouse.QueryValue{}, fmt.Errorf("%w: an absent value cannot be a query operand", errInvalidHTTPInput)
	case contract.PresenceNull:
		return warehouse.NullQueryValue(), nil
	}
	number, isNumber := value.ExactNumber()
	switch value.Kind() {
	case contract.ValueKindInteger:
		if !isNumber {
			return warehouse.QueryValue{}, errUnbindableValue
		}
		integer, err := strconv.ParseInt(number.Text, 10, 64)
		if err != nil {
			return warehouse.QueryValue{}, fmt.Errorf("%w: an integer operand is outside the exact 64-bit range", errInvalidHTTPInput)
		}
		return warehouse.Int64QueryValue(integer), nil
	case contract.ValueKindDecimal:
		if !isNumber {
			return warehouse.QueryValue{}, errUnbindableValue
		}
		return warehouse.DecimalQueryValue(number.Text, number.Scale)
	case contract.ValueKindString:
		text, ok := value.TextValue()
		if !ok {
			return warehouse.QueryValue{}, errUnbindableValue
		}
		return warehouse.TextQueryValue(text), nil
	case contract.ValueKindBool:
		boolean, ok := value.Boolean()
		if !ok {
			return warehouse.QueryValue{}, errUnbindableValue
		}
		return warehouse.BooleanQueryValue(boolean), nil
	case contract.ValueKindDate:
		text, ok := value.TemporalText()
		if !ok {
			return warehouse.QueryValue{}, errUnbindableValue
		}
		return warehouse.DateQueryValue(text)
	case contract.ValueKindTimestampInstant:
		text, ok := value.TemporalText()
		if !ok {
			return warehouse.QueryValue{}, errUnbindableValue
		}
		microseconds, err := instantMicroseconds(text)
		if err != nil {
			return warehouse.QueryValue{}, err
		}
		return warehouse.TimestampTZQueryValue(microseconds), nil
	case contract.ValueKindTimestampLocal:
		text, ok := value.TemporalText()
		if !ok {
			return warehouse.QueryValue{}, errUnbindableValue
		}
		microseconds, err := localMicroseconds(text)
		if err != nil {
			return warehouse.QueryValue{}, err
		}
		return warehouse.TimestampQueryValue(microseconds), nil
	default:
		return warehouse.QueryValue{}, fmt.Errorf("%w: value kind %s has no exact operand form",
			errInvalidHTTPInput, value.Kind())
	}
}

var errUnbindableValue = errors.New("a declared typed value is missing its payload")

// projectableWarehouseKind reports whether one registered column kind has an
// exact projection on this wire form. A bytea payload has no exact projection
// without an admitted object reference, so it is refused before the query runs.
func projectableWarehouseKind(kind warehouse.QueryKind) bool {
	switch kind {
	case warehouse.QueryKindInt64, warehouse.QueryKindDecimal, warehouse.QueryKindText,
		warehouse.QueryKindBoolean, warehouse.QueryKindDate, warehouse.QueryKindTimestamp,
		warehouse.QueryKindTimestampTZ, warehouse.QueryKindUUID, warehouse.QueryKindJSON:
		return true
	default:
		return false
	}
}

// nullableText renders one optional identity member as a required nullable wire
// member: an absent selector is explicit, never an empty string.
func nullableText(value string) nullable.Nullable[string] {
	if value == "" {
		return nullable.NewNullNullable[string]()
	}
	return nullable.NewNullableWithValue(value)
}

// instantMicroseconds converts one RFC3339 instant into the warehouse's exact
// microsecond instant. A finer fraction is refused rather than truncated.
func instantMicroseconds(text string) (int64, error) {
	if err := requireMicrosecondFraction(text); err != nil {
		return 0, err
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		return 0, fmt.Errorf("%w: an instant operand must be RFC3339", errInvalidHTTPInput)
	}
	return parsed.UnixMicro(), nil
}

// localMicroseconds converts one wall timestamp without a zone into the
// warehouse's exact microsecond timestamp. A finer fraction is refused rather
// than truncated.
func localMicroseconds(text string) (int64, error) {
	if err := requireMicrosecondFraction(text); err != nil {
		return 0, err
	}
	parsed, err := time.Parse("2006-01-02T15:04:05.999999999", text)
	if err != nil {
		return 0, fmt.Errorf("%w: a local timestamp operand must be an exact wall timestamp", errInvalidHTTPInput)
	}
	return parsed.UnixMicro(), nil
}

// requireMicrosecondFraction refuses a temporal operand whose fractional part
// is finer than the registered microsecond resolution.
func requireMicrosecondFraction(text string) error {
	separator := strings.IndexByte(text, '.')
	if separator < 0 {
		return nil
	}
	fraction := text[separator+1:]
	if end := strings.IndexAny(fraction, "Zz+-"); end >= 0 {
		fraction = fraction[:end]
	}
	if len(fraction) > 6 {
		return fmt.Errorf("%w: a temporal operand is finer than the registered microsecond resolution",
			errInvalidHTTPInput)
	}
	return nil
}

// decimalPrecision reports the digit count of one exact unscaled decimal.
func decimalPrecision(unscaled string) int {
	digits := len(unscaled)
	if digits > 0 && (unscaled[0] == '-' || unscaled[0] == '+') {
		digits--
	}
	return digits
}

// optionalString renders an optional parameter member without inventing a value
// for its absence.
func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// nullableMemberText renders a required-but-nullable body member without
// collapsing an explicit null into an empty string.
func nullableMemberText(value nullable.Nullable[string]) string {
	text, err := value.Get()
	if err != nil {
		return ""
	}
	return text
}

// requireWarehouseIdentifier refuses a non-canonical identifier before it can
// reach the warehouse service. A member that declares an operand must be
// present; an optional member may be absent.
func requireWarehouseIdentifier(field, value string, required bool) error {
	if value == "" {
		if required {
			return fmt.Errorf("%w: %s is required", errInvalidHTTPInput, field)
		}
		return nil
	}
	if len(value) > warehouse.MaximumQueryIdentifierBytes {
		return fmt.Errorf("%w: %s exceeds the admitted identifier length", errInvalidHTTPInput, field)
	}
	for _, character := range value {
		valid := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '_'
		if !valid {
			return fmt.Errorf("%w: %s must use only letters, digits and '_'", errInvalidHTTPInput, field)
		}
	}
	return nil
}

// warehouseSnapshotResponse projects one committed read view.
func warehouseSnapshotResponse(
	scope contract.Scope,
	layout warehouse.WarehouseLayout,
	snapshotID string,
	view warehouse.QuerySnapshot,
) (api.WarehouseDatasetSnapshot, error) {
	projected := api.WarehouseDatasetSnapshot{
		OrgId:         scope.OrgID,
		WorkspaceId:   scope.WorkspaceID,
		ConnectorId:   layout.ConnectorID,
		ConnectionId:  layout.ConnectionID,
		Dataset:       layout.Dataset,
		Revision:      int64(view.Revision),
		CursorOrdinal: int64(view.Revision),
		SnapshotId:    nullableText(snapshotID),
		Segments:      make([]api.WarehouseSegment, 0, len(view.Segments)),
	}
	for _, segment := range view.Segments {
		projected.Rows += segment.Rows
		projected.Segments = append(projected.Segments, api.WarehouseSegment{
			Ordinal:           int64(segment.Ordinal),
			ManifestRevision:  int64(segment.Ordinal),
			RowCount:          segment.Rows,
			ColumnFingerprint: segment.Schema.Fingerprint(),
			State:             api.Live,
			Object: api.WarehouseObject{
				ArtifactId:        segment.Object.ArtifactID,
				MediaType:         segment.Object.MediaType,
				ByteLength:        segment.Object.ByteLength,
				Sha256:            segment.Object.SHA256,
				BackendGeneration: segment.Object.BackendGeneration,
			},
		})
	}
	return projected, nil
}

// warehouseQueryResponse projects exact typed rows onto the shared exact value
// union.
func warehouseQueryResponse(
	scope contract.Scope,
	layout warehouse.WarehouseLayout,
	snapshotID string,
	response warehouse.QueryResponse,
) (api.WarehouseQueryResult, error) {
	projected := api.WarehouseQueryResult{
		OrgId:        scope.OrgID,
		WorkspaceId:  scope.WorkspaceID,
		ConnectorId:  layout.ConnectorID,
		ConnectionId: layout.ConnectionID,
		Dataset:      layout.Dataset,
		SnapshotId:   nullableText(snapshotID),
		Columns:      make([]api.WarehouseColumn, 0, len(response.Columns)),
		Rows:         make([][]PresentValue, 0, len(response.Rows)),
		Truncated:    response.Truncated,
	}
	for _, column := range response.Columns {
		projected.Columns = append(projected.Columns, api.WarehouseColumn{
			Name:      column.Name,
			Kind:      api.WarehouseColumnKind(column.Kind),
			Precision: column.Precision,
			Scale:     column.Scale,
			Nullable:  column.Nullable,
		})
	}
	for _, row := range response.Rows {
		cells := make([]PresentValue, 0, len(row))
		for _, value := range row {
			projected, err := projectQueryValue(value)
			if err != nil {
				return api.WarehouseQueryResult{}, err
			}
			cells = append(cells, projected)
		}
		projected.Rows = append(projected.Rows, cells)
	}
	return projected, nil
}

// projectQueryValue renders one exact warehouse value as the shared exact value
// union. Absence, explicit null, false, zero and empty text all survive.
func projectQueryValue(value warehouse.QueryValue) (PresentValue, error) {
	if !value.Present() {
		return contract.AbsentValue(), nil
	}
	if value.IsNull() {
		return contract.NullValue(), nil
	}
	switch value.Kind() {
	case warehouse.QueryKindInt64:
		integer, ok := value.Int64()
		if !ok {
			return PresentValue{}, errUnprojectableValue
		}
		return contract.NewIntegerValue(strconv.FormatInt(integer, 10))
	case warehouse.QueryKindDecimal:
		unscaled, scale, ok := value.Unscaled()
		if !ok {
			return PresentValue{}, errUnprojectableValue
		}
		return contract.NewDecimalValue(unscaled, decimalPrecision(unscaled), scale)
	case warehouse.QueryKindBoolean:
		boolean, ok := value.Boolean()
		if !ok {
			return PresentValue{}, errUnprojectableValue
		}
		return contract.NewBooleanValue(boolean), nil
	case warehouse.QueryKindText, warehouse.QueryKindUUID, warehouse.QueryKindJSON:
		text, ok := value.Text()
		if !ok {
			return PresentValue{}, errUnprojectableValue
		}
		return contract.NewStringValue(text), nil
	case warehouse.QueryKindDate:
		text, ok := value.Text()
		if !ok {
			return PresentValue{}, errUnprojectableValue
		}
		return contract.NewDateValue(text)
	case warehouse.QueryKindTimestampTZ:
		microseconds, ok := value.Int64()
		if !ok {
			return PresentValue{}, errUnprojectableValue
		}
		return contract.NewInstantValue(time.UnixMicro(microseconds).UTC().Format("2006-01-02T15:04:05.000000Z"))
	case warehouse.QueryKindTimestamp:
		microseconds, ok := value.Int64()
		if !ok {
			return PresentValue{}, errUnprojectableValue
		}
		return contract.NewLocalTimestampValue(time.UnixMicro(microseconds).UTC().Format("2006-01-02T15:04:05.000000"))
	default:
		return PresentValue{}, errUnprojectableValue
	}
}

// The warehouse read routes carry no mutation identity, so every refusal
// serializes as the shared problem document.
func (r problemResponse) VisitGetWarehouseDatasetResponse(w http.ResponseWriter) error {
	return r.write(w)
}

func (r problemResponse) VisitRunWarehouseQueryResponse(w http.ResponseWriter) error {
	return r.write(w)
}
