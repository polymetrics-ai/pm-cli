package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/connectors"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/credentials"
	pmruntime "github.com/polymetrics-ai/pm-cli/internal/runtime"
	"github.com/polymetrics-ai/pm-cli/internal/schema"
)

var (
	// ErrSQLConnectionUnavailable reports that no customer SQL connection is
	// declared for the requesting scope and connection. Resolution never
	// substitutes another connection.
	ErrSQLConnectionUnavailable = errors.New("declared customer SQL connection is unavailable")
	// ErrSQLBindingUnavailable reports that the operation's declared relation
	// binding is not permitted for this connection or that its selected relation
	// metadata is unusable. No relation, table or column is ever substituted.
	ErrSQLBindingUnavailable = errors.New("declared customer SQL relation binding is unavailable")
	// ErrSQLCredentialUnavailable reports that the one-use credential material
	// resolved for this attempt is not the credential the connection declares.
	ErrSQLCredentialUnavailable = errors.New("declared customer SQL credential is unavailable")
	// ErrSQLStatementUnavailable reports that the declared operation's statement
	// input does not bind the declared relation as declared.
	ErrSQLStatementUnavailable = errors.New("declared customer SQL statement input is unavailable")
)

// sqlTrustAnchorMember is the operator secret member carrying one declared
// customer connection's strict trust anchor.
const sqlTrustAnchorMember = "ca.pem"

// sqlInstantLayout renders an exact instant as canonical UTC RFC 3339 text with
// all significant fraction digits retained.
const sqlInstantLayout = "2006-01-02T15:04:05.999999999Z07:00"

// SQLBoundsResolver returns the declared finite bounds of one customer SQL
// connection for the exact scope. A connection without a declaration reports
// false, so planning refuses instead of inventing a bound.
type SQLBoundsResolver func(contract.Scope, string) (config.ProviderSQLBounds, bool)

// SQLConnectionResources are the process-static resources of one declared
// customer SQL connection: the declared strict trust anchor and the
// connection's own schema state. Credential material is deliberately absent
// here; it is resolved only at the execution boundary.
type SQLConnectionResources struct {
	// TrustAnchorPEM is the exact declared PEM trust-anchor bundle.
	TrustAnchorPEM []byte
	// Snapshots resolves the selected relation metadata of this connection.
	Snapshots *schema.Store
}

// SQLCompositionConfig declares the customer SQL connections one process admits.
// An absent declaration composes no SQL capability at all.
type SQLCompositionConfig struct {
	Declarations config.ProviderSQLConfiguration
	Bundles      *connectors.Registry
	Resources    func(config.ProviderSQLConnection) (SQLConnectionResources, error)
	Dialer       pmruntime.ContextDialer
	Now          func() time.Time
}

type sqlConnectionKey struct {
	orgID        string
	workspaceID  string
	connectionID string
}

type sqlDeclaredOperation struct {
	connectorID string
	operationID string
}

type sqlBoundConnection struct {
	declaration config.ProviderSQLConnection
	trustAnchor []byte
	snapshots   *schema.Store
	bindings    map[string]config.ProviderSQLRelationBinding
}

// SQLComposition owns the immutable customer SQL declaration of one process and
// the single SQL execution callback registered for the PostgreSQL operations
// the loaded bundle registry declares. It reads no provider document, holds no
// credential material and never executes a statement outside one admitted
// worker callback.
type SQLComposition struct {
	declared     bool
	bounds       config.ProviderSQLBounds
	connections  map[sqlConnectionKey]sqlBoundConnection
	operations   map[sqlDeclaredOperation]struct{}
	lanes        []contract.Lane
	capabilities []contract.CapabilityRequirement
	foundations  []contract.FoundationContract
	dialer       pmruntime.ContextDialer
	now          func() time.Time
}

// OpenSQLComposition resolves every declared customer connection against its
// process resources and the declared PostgreSQL operations of the loaded
// bundle registry. An undeclared configuration composes an unavailable
// composition; a declared but unusable one fails composition with a bounded
// error instead of degrading to a partial capability.
func OpenSQLComposition(configuration SQLCompositionConfig) (*SQLComposition, error) {
	if !configuration.Declarations.Configured() {
		return &SQLComposition{}, nil
	}
	if configuration.Bundles == nil {
		return nil, errors.New("declared customer SQL connections require the loaded connector registry")
	}
	if configuration.Resources == nil {
		return nil, errors.New("declared customer SQL connections require their connection resources")
	}
	composition := &SQLComposition{
		declared:    true,
		bounds:      configuration.Declarations.Bounds(),
		connections: make(map[sqlConnectionKey]sqlBoundConnection),
		operations:  make(map[sqlDeclaredOperation]struct{}),
		dialer:      configuration.Dialer,
		now:         configuration.Now,
	}
	connectorsSeen := make(map[string]struct{})
	for _, connection := range configuration.Declarations.Connections() {
		resources, err := configuration.Resources(connection)
		if err != nil {
			return nil, errors.New("declared customer SQL connection resources are unavailable")
		}
		bound, err := bindSQLConnection(connection, resources)
		if err != nil {
			return nil, err
		}
		scope := connection.Scope()
		key := sqlConnectionKey{
			orgID: scope.OrgID, workspaceID: scope.WorkspaceID, connectionID: connection.ConnectionID,
		}
		if _, duplicate := composition.connections[key]; duplicate {
			return nil, errors.New("declared customer SQL connection is duplicated")
		}
		composition.connections[key] = bound
		if _, seen := connectorsSeen[connection.ConnectorID]; seen {
			continue
		}
		connectorsSeen[connection.ConnectorID] = struct{}{}
		operations, err := declaredPostgreSQLOperations(configuration.Bundles, connection.ConnectorID)
		if err != nil {
			return nil, err
		}
		for _, operation := range operations {
			composition.operations[sqlDeclaredOperation{
				connectorID: connection.ConnectorID, operationID: operation.OperationID,
			}] = struct{}{}
			if err := composition.declareOperation(operation); err != nil {
				return nil, err
			}
		}
	}
	if len(composition.operations) == 0 || len(composition.lanes) == 0 || len(composition.capabilities) == 0 {
		return nil, errors.New("no declared customer SQL connection resolves a declared PostgreSQL operation")
	}
	return composition, nil
}

// Configured reports whether at least one customer SQL connection is declared
// and resolves a declared PostgreSQL operation. An unconfigured composition
// registers no callback and refuses every bound.
func (composition *SQLComposition) Configured() bool {
	return composition != nil && composition.declared
}

// Bounds reports the declared finite bounds for one exact connection. It never
// falls back to another scope or connection.
func (composition *SQLComposition) Bounds(scope contract.Scope, connectionID string) (config.ProviderSQLBounds, bool) {
	if composition == nil || !composition.declared {
		return config.ProviderSQLBounds{}, false
	}
	if _, present := composition.connections[sqlConnectionKey{
		orgID: scope.OrgID, workspaceID: scope.WorkspaceID, connectionID: connectionID,
	}]; !present {
		return config.ProviderSQLBounds{}, false
	}
	return composition.bounds, true
}

// Register installs the shared PostgreSQL execution callback for the declared
// operations' lanes, foundations and capability closure. An undeclared
// composition registers nothing, so admission can never advertise a statement
// kind whose callback is not really registered.
func (composition *SQLComposition) Register(handlers *HandlerRegistry) error {
	if composition == nil {
		return errors.New("SQL composition is required")
	}
	if handlers == nil {
		return errors.New("SQL handler registry is required")
	}
	if !composition.declared {
		return nil
	}
	if len(composition.lanes) == 0 || len(composition.capabilities) == 0 {
		return errors.New("declared customer SQL composition requires at least one declared operation")
	}
	if err := handlers.RegisterSQL(HandlerRegistration{
		Lanes:        append([]contract.Lane(nil), composition.lanes...),
		Foundations:  cloneFoundations(composition.foundations),
		Capabilities: append([]contract.CapabilityRequirement(nil), composition.capabilities...),
		Execute:      composition.execute,
	}); err != nil {
		return fmt.Errorf("register shared PostgreSQL execution handler: %w", err)
	}
	return nil
}

// LoadProviderSQLResources opens the process-static resources of one declared
// customer connection: the declared operator trust anchor and the connection's
// own schema state directory. A declared trust anchor that is missing, empty,
// oversized or unusable fails this call with a constant bounded error that
// never contains secret material. The declared password reference is
// deliberately not read here: credential material has exactly one path, the
// one-use resolution at the execution boundary.
func LoadProviderSQLResources(
	secrets *credentials.OperatorSecrets,
	connection config.ProviderSQLConnection,
) (SQLConnectionResources, error) {
	if secrets == nil {
		return SQLConnectionResources{}, errors.New("customer SQL trust anchor authority is unavailable")
	}
	trustAnchor, err := secrets.Read(connection.TLSTrustRef, sqlTrustAnchorMember)
	if err != nil {
		return SQLConnectionResources{}, errors.New("customer SQL trust anchor is unavailable")
	}
	snapshots, err := schema.NewStore(connection.StateDirectory)
	if err != nil {
		clear(trustAnchor)
		return SQLConnectionResources{}, errors.New("customer SQL connection state directory is unavailable")
	}
	return SQLConnectionResources{TrustAnchorPEM: trustAnchor, Snapshots: snapshots}, nil
}

// bindSQLConnection validates one declared connection against its process
// resources. Only a strict, verifiable trust anchor and an existing private
// schema state directory produce a usable binding.
func bindSQLConnection(
	connection config.ProviderSQLConnection,
	resources SQLConnectionResources,
) (sqlBoundConnection, error) {
	if _, err := pmruntime.NewStrictSQLTLS(pmruntime.SQLTLSConfig{
		CACertificatePEM: resources.TrustAnchorPEM,
		ServerName:       connection.Host,
	}); err != nil {
		return sqlBoundConnection{}, errors.New("declared customer SQL trust anchor is not a strict usable anchor")
	}
	if resources.Snapshots == nil {
		return sqlBoundConnection{}, errors.New("declared customer SQL connection requires its schema state")
	}
	bound := sqlBoundConnection{
		declaration: connection,
		trustAnchor: append([]byte(nil), resources.TrustAnchorPEM...),
		snapshots:   resources.Snapshots,
		bindings:    make(map[string]config.ProviderSQLRelationBinding, len(connection.Bindings)),
	}
	for _, binding := range connection.Bindings {
		bound.bindings[binding.Binding] = binding
	}
	return bound, nil
}

// declaredPostgreSQLOperations returns every operation of one declared
// connector whose transport is the native PostgreSQL transport. A connector
// that declares none is unusable for a customer SQL connection.
func declaredPostgreSQLOperations(
	registry *connectors.Registry,
	connectorID string,
) ([]contract.Operation, error) {
	connector, err := registry.Connector(connectorID)
	if err != nil {
		return nil, errors.New("declared customer SQL connector is not installed")
	}
	operations := make([]contract.Operation, 0, len(connector.Operations))
	for _, operation := range connector.Operations {
		if operation.Contract.Transport.Kind == contract.TransportKindPostgreSQL {
			operations = append(operations, operation.Contract)
		}
	}
	if len(operations) == 0 {
		return nil, errors.New("declared customer SQL connector declares no PostgreSQL operation")
	}
	return operations, nil
}

// declareOperation adds one declared operation's eligible lanes and capability
// closure to the single SQL registration. Only lanes the operation itself
// declares eligible become reachable, and the capability set is exactly the
// declared requirements.
func (composition *SQLComposition) declareOperation(operation contract.Operation) error {
	for _, eligibility := range operation.LaneEligibility {
		if eligibility.Status != contract.LaneEligible {
			continue
		}
		if !containsLane(composition.lanes, eligibility.Lane) {
			composition.lanes = append(composition.lanes, eligibility.Lane)
		}
	}
	for _, requirement := range operation.CapabilityRequirements {
		if !containsCapability(composition.capabilities, requirement) {
			composition.capabilities = append(composition.capabilities, requirement)
		}
	}
	composition.foundations = shapeFoundations(foundationReferences(composition.capabilities))
	return nil
}

// execute is the shared SQL execution callback. It re-derives the declared
// connection and relation binding for the exact requesting scope, builds the
// statement the operation's declared statement kind selects, and performs at
// most one admitted PostgreSQL transaction through the reusable transport.
func (composition *SQLComposition) execute(
	ctx context.Context,
	request ExecutionRequest,
) (ExecutionObservation, error) {
	if ctx == nil {
		return ExecutionObservation{}, context.Canceled
	}
	call, admitter, credentialResolver, err := request.PhysicalCallAuthority()
	if err != nil {
		// Without a fresh physical-call boundary no durable attempt can exist,
		// so there is nothing this callback may project.
		return ExecutionObservation{}, err
	}
	observedAt := composition.observationTime()
	bound, err := composition.bind(request.Scope, request.ConnectionID, request.Operation)
	if err != nil {
		return refusedSQLObservation(request, observedAt, err)
	}
	input, err := bound.input(request.Operation, request.Input)
	if err != nil {
		return refusedSQLObservation(request, observedAt, err)
	}
	limits := composition.limits(request.AttemptNumber)
	observation, executeErr := pmruntime.SQLTransport{Dependencies: pmruntime.SQLDependencies{
		Admitter: admitter,
		Credentials: sqlCredentialExpectation{
			reference: bound.declaration.PasswordRef,
			inner:     credentialResolver,
		},
		Dialer: composition.dialer,
		Now:    composition.now,
	}}.Execute(ctx, pmruntime.SQLCall{
		Operation: request.Operation,
		Connection: pmruntime.SQLConnection{
			Host:     bound.declaration.Host,
			Port:     bound.declaration.Port,
			Database: bound.declaration.Database,
			User:     bound.declaration.User,
			TLS: pmruntime.SQLTLSConfig{
				CACertificatePEM: bound.trustAnchor,
				ServerName:       bound.declaration.Host,
			},
		},
		Relation: bound.relation,
		Call:     call,
		Input:    input,
		Limits:   limits,
	})
	return bound.project(request, observation, executeErr)
}

// bind resolves the declared connection and the operation's declared relation
// binding for the exact scope. It never substitutes another connection, table,
// relation or column, and a missing selected discovery refuses explicitly.
func (composition *SQLComposition) bind(
	scope contract.Scope,
	connectionID string,
	operation contract.Operation,
) (sqlBoundOperation, error) {
	if composition == nil || !composition.declared {
		return sqlBoundOperation{}, ErrSQLConnectionUnavailable
	}
	connection, present := composition.connections[sqlConnectionKey{
		orgID: scope.OrgID, workspaceID: scope.WorkspaceID, connectionID: connectionID,
	}]
	if !present {
		return sqlBoundOperation{}, fmt.Errorf("%w: no customer SQL connection is declared for this scope", ErrSQLConnectionUnavailable)
	}
	if _, declared := composition.operations[sqlDeclaredOperation{
		connectorID: connection.declaration.ConnectorID, operationID: operation.OperationID,
	}]; !declared {
		return sqlBoundOperation{}, fmt.Errorf("%w: the requested operation is not a declared PostgreSQL operation of this connector", ErrSQLConnectionUnavailable)
	}
	binding, present := connection.bindings[operation.Transport.RelationBinding]
	if !present {
		return sqlBoundOperation{}, fmt.Errorf(
			"%w: relation binding %q is not permitted for this connection",
			ErrSQLBindingUnavailable, operation.Transport.RelationBinding,
		)
	}
	snapshot, present, err := connection.snapshots.Active(schema.Binding{
		ConnectorID:        connection.declaration.ConnectorID,
		ConnectionID:       connection.declaration.ConnectionID,
		ProviderAccountID:  binding.Snapshot.ProviderAccountID,
		PermissionRevision: binding.Snapshot.PermissionRevision,
		Resource:           binding.Snapshot.Resource,
		SourceRevision:     binding.Snapshot.SourceRevision,
	})
	if err != nil {
		return sqlBoundOperation{}, fmt.Errorf("%w: selected relation metadata is unusable", ErrSQLBindingUnavailable)
	}
	if !present {
		return sqlBoundOperation{}, fmt.Errorf(
			"%w: no complete selected discovery is active for relation binding %q",
			ErrSQLBindingUnavailable, binding.Binding,
		)
	}
	relation, writable, err := sqlRelation(binding, snapshot)
	if err != nil {
		return sqlBoundOperation{}, err
	}
	return sqlBoundOperation{
		declaration: connection.declaration,
		trustAnchor: connection.trustAnchor,
		relation:    relation,
		writable:    writable,
	}, nil
}

// limits projects the declared finite bounds onto one exact attempt. The
// transport re-validates them before any physical work, so an unusable declared
// bound refuses there instead of being silently widened here. MaxAttempts is the
// declared ceiling the transport compares this attempt against; while no SQL
// retry is scheduled (see project) it bounds the one attempt that runs.
func (composition *SQLComposition) limits(attemptNumber uint32) pmruntime.SQLLimits {
	return pmruntime.SQLLimits{
		MaxConnectDuration:   composition.bounds.MaxConnectDuration,
		MaxStatementDuration: composition.bounds.MaxStatementDuration,
		MaxTotalDuration:     composition.bounds.MaxTotalDuration,
		MaxRows:              composition.bounds.MaxRows,
		MaxBytes:             composition.bounds.MaxBytes,
		AttemptNumber:        attemptNumber,
		MaxAttempts:          composition.bounds.MaxAttempts,
	}
}

func (composition *SQLComposition) observationTime() time.Time {
	now := composition.now
	if now == nil {
		now = time.Now
	}
	return now().UTC()
}

// sqlBoundOperation is one resolved execution target: the declared connection
// facts, its strict trust anchor and the exact relation binding of this
// operation.
type sqlBoundOperation struct {
	declaration config.ProviderSQLConnection
	trustAnchor []byte
	relation    pmruntime.SQLRelation
	writable    map[string]struct{}
}

// sqlRelation builds the transport relation from the declared binding and the
// selected discovery. Every selected field becomes exactly one declared column;
// a field whose logical type has no exact transport kind refuses the whole
// binding instead of dropping or coercing it.
func sqlRelation(
	binding config.ProviderSQLRelationBinding,
	snapshot schema.Snapshot,
) (pmruntime.SQLRelation, map[string]struct{}, error) {
	if len(snapshot.Fields) == 0 {
		return pmruntime.SQLRelation{}, nil, fmt.Errorf("%w: the selected discovery declares no fields", ErrSQLBindingUnavailable)
	}
	keys := make(map[string]struct{}, len(binding.KeyColumns))
	for _, column := range binding.KeyColumns {
		keys[column] = struct{}{}
	}
	columns := make([]pmruntime.SQLColumn, 0, len(snapshot.Fields))
	declared := make(map[string]struct{}, len(snapshot.Fields))
	writable := make(map[string]struct{}, len(snapshot.Fields))
	for _, field := range snapshot.Fields {
		if _, duplicate := declared[field.Name]; duplicate {
			return pmruntime.SQLRelation{}, nil, fmt.Errorf(
				"%w: the selected discovery declares column %q twice", ErrSQLBindingUnavailable, field.Name,
			)
		}
		kind, precision, scale, err := sqlColumnKind(field)
		if err != nil {
			return pmruntime.SQLRelation{}, nil, err
		}
		_, key := keys[field.Name]
		declared[field.Name] = struct{}{}
		if field.Writable {
			writable[field.Name] = struct{}{}
		}
		columns = append(columns, pmruntime.SQLColumn{
			Name: field.Name, Kind: kind, Precision: precision, Scale: scale, Key: key,
		})
	}
	for _, column := range binding.KeyColumns {
		if _, present := declared[column]; !present {
			return pmruntime.SQLRelation{}, nil, fmt.Errorf(
				"%w: declared key column %q is absent from the selected discovery",
				ErrSQLBindingUnavailable, column,
			)
		}
	}
	relation := pmruntime.SQLRelation{
		Binding: binding.Binding,
		Schema:  binding.Schema,
		Name:    binding.Table,
		Columns: columns,
	}
	if err := relation.Validate(); err != nil {
		return pmruntime.SQLRelation{}, nil, fmt.Errorf("%w: the declared relation binding is unusable", ErrSQLBindingUnavailable)
	}
	return relation, writable, nil
}

// sqlColumnKind maps one selected discovery field onto the exact transport
// column type. A logical type without an exact transport kind is refused.
func sqlColumnKind(field schema.Field) (pmruntime.SQLValueKind, int, int, error) {
	switch field.Schema.Type {
	case contract.SchemaTypeString:
		return pmruntime.SQLText, 0, 0, nil
	case contract.SchemaTypeBool:
		return pmruntime.SQLBoolean, 0, 0, nil
	case contract.SchemaTypeInteger:
		if field.Precision != 0 || field.Scale != 0 {
			return "", 0, 0, fmt.Errorf(
				"%w: selected column %q declares precision or scale for an integer",
				ErrSQLBindingUnavailable, field.Name,
			)
		}
		return pmruntime.SQLInt8, 0, 0, nil
	case contract.SchemaTypeDecimal:
		if field.Precision == 0 {
			return "", 0, 0, fmt.Errorf(
				"%w: selected column %q declares no numeric precision", ErrSQLBindingUnavailable, field.Name,
			)
		}
		return pmruntime.SQLNumeric, field.Precision, field.Scale, nil
	case contract.SchemaTypeTimestampInstant:
		return pmruntime.SQLTimestamptz, 0, 0, nil
	case contract.SchemaTypeTimestampLocal:
		return pmruntime.SQLTimestamp, 0, 0, nil
	default:
		return "", 0, 0, fmt.Errorf(
			"%w: selected column %q has no exact transport type", ErrSQLBindingUnavailable, field.Name,
		)
	}
}

// input maps the admitted typed input onto the declared relation columns. Only
// the operation's own declared input bindings may supply a value, and each
// declared wire name must be exactly one declared relation column.
func (bound sqlBoundOperation) input(
	operation contract.Operation,
	input contract.PresentValue,
) ([]pmruntime.SQLColumnValue, error) {
	fields, _ := input.Fields()
	values := make([]pmruntime.SQLColumnValue, 0, len(operation.Inputs))
	seen := make(map[string]struct{}, len(operation.Inputs))
	for _, binding := range operation.Inputs {
		column, present := bound.relation.Column(binding.WireName)
		if !present {
			return nil, fmt.Errorf(
				"%w: declared input %q does not bind a declared relation column",
				ErrSQLStatementUnavailable, binding.FieldID,
			)
		}
		if _, duplicate := seen[column.Name]; duplicate {
			return nil, fmt.Errorf(
				"%w: relation column %q is bound by more than one declared input",
				ErrSQLStatementUnavailable, column.Name,
			)
		}
		seen[column.Name] = struct{}{}
		value, present := fields[binding.FieldID]
		if !present || value.Presence() == contract.PresenceAbsent {
			// An absent value is no value at all: a mapped write keeps the
			// declared default and every other statement kind ignores it.
			continue
		}
		if operation.Transport.StatementKind == contract.PostgreSQLStatementMappedUpsert {
			if _, writable := bound.writable[column.Name]; !writable {
				return nil, fmt.Errorf(
					"%w: selected column %q is not writable and cannot be mapped",
					ErrSQLStatementUnavailable, column.Name,
				)
			}
		}
		converted, err := sqlColumnValue(column, value)
		if err != nil {
			return nil, err
		}
		values = append(values, pmruntime.SQLColumnValue{Column: column.Name, Value: converted})
	}
	return values, nil
}

// sqlColumnValue converts one exact admitted value into the declared column's
// transport value. Nothing is coerced: a value that does not carry the declared
// column's exact type is refused.
func sqlColumnValue(column pmruntime.SQLColumn, value contract.PresentValue) (pmruntime.SQLValue, error) {
	switch value.Presence() {
	case contract.PresenceNull:
		return pmruntime.SQLNullValue(column.Kind), nil
	case contract.PresenceAbsent:
		return pmruntime.SQLAbsentValue(column.Kind), nil
	case contract.PresenceValue:
	default:
		return pmruntime.SQLValue{}, fmt.Errorf("%w: bound value presence is unsupported", ErrSQLStatementUnavailable)
	}
	switch column.Kind {
	case pmruntime.SQLText:
		text, ok := value.TextValue()
		if !ok {
			return pmruntime.SQLValue{}, sqlValueTypeError(column)
		}
		return pmruntime.SQLTextValue(text), nil
	case pmruntime.SQLInt8:
		number, ok := value.ExactNumber()
		if !ok || number.Scale != 0 {
			return pmruntime.SQLValue{}, sqlValueTypeError(column)
		}
		converted, err := pmruntime.SQLInt8Value(number.Text)
		if err != nil {
			return pmruntime.SQLValue{}, fmt.Errorf("%w: %v", ErrSQLStatementUnavailable, err)
		}
		return converted, nil
	case pmruntime.SQLNumeric:
		number, ok := value.ExactNumber()
		if !ok || number.Precision != column.Precision || number.Scale != column.Scale {
			return pmruntime.SQLValue{}, sqlValueTypeError(column)
		}
		converted, err := pmruntime.SQLNumericValue(number.Text, column.Precision, column.Scale)
		if err != nil {
			return pmruntime.SQLValue{}, fmt.Errorf("%w: %v", ErrSQLStatementUnavailable, err)
		}
		return converted, nil
	case pmruntime.SQLBoolean:
		boolean, ok := value.Boolean()
		if !ok {
			return pmruntime.SQLValue{}, sqlValueTypeError(column)
		}
		return pmruntime.SQLBooleanValue(boolean), nil
	case pmruntime.SQLTimestamptz:
		if value.Kind() != contract.ValueKindTimestampInstant {
			return pmruntime.SQLValue{}, sqlValueTypeError(column)
		}
		text, ok := value.TemporalText()
		if !ok {
			return pmruntime.SQLValue{}, sqlValueTypeError(column)
		}
		converted, err := pmruntime.SQLInstantValue(text)
		if err != nil {
			return pmruntime.SQLValue{}, fmt.Errorf("%w: %v", ErrSQLStatementUnavailable, err)
		}
		return converted, nil
	case pmruntime.SQLTimestamp:
		if value.Kind() != contract.ValueKindTimestampLocal {
			return pmruntime.SQLValue{}, sqlValueTypeError(column)
		}
		text, ok := value.TemporalText()
		if !ok {
			return pmruntime.SQLValue{}, sqlValueTypeError(column)
		}
		converted, err := pmruntime.SQLLocalTimestampValue(sqlTransportLocalTimestampText(text))
		if err != nil {
			return pmruntime.SQLValue{}, fmt.Errorf("%w: %v", ErrSQLStatementUnavailable, err)
		}
		return converted, nil
	default:
		return pmruntime.SQLValue{}, sqlValueTypeError(column)
	}
}

func sqlValueTypeError(column pmruntime.SQLColumn) error {
	return fmt.Errorf(
		"%w: bound value does not carry the exact declared type of column %q",
		ErrSQLStatementUnavailable, column.Name,
	)
}

// project converts one bounded transport observation into the shared execution
// observation: the exact member state, the exact typed row projection when the
// statement committed, and the immediate callback error. A mapped write's rows
// exist only for an acknowledged COMMIT, so a possibly accepted write never
// presents rows as an accepted effect.
//
// The transport's own retry directive is deliberately not projected. The shared
// worker planner admits one retry basis, for the registered HTTP handler only,
// so a PostgreSQL attempt has no durable retry path today: the directive states
// what a definitively unsent or definitively rolled-back attempt could earn, and
// nothing here may publish it as a scheduled retry. provider_sql.max_attempts is
// consequently a declared, validated ceiling for the one physical attempt this
// callback performs, never evidence that more than one attempt is scheduled.
// Forwarding the directive into this observation and admitting a SQL retry basis
// in the planner is later integration work.
func (bound sqlBoundOperation) project(
	request ExecutionRequest,
	observation pmruntime.SQLObservation,
	executeErr error,
) (ExecutionObservation, error) {
	observedAt := observation.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	observedAt = observedAt.UTC()
	state := contract.OutcomeUnsent
	var result *contract.ActionResult
	var projectionErr error
	switch observation.Outcome {
	case pmruntime.SQLOutcomeKnownCommit:
		if request.Operation.Transport.StatementKind == contract.PostgreSQLStatementMetadata {
			if err := pmruntime.VerifySQLRelation(bound.relation, observation.Rows); err != nil {
				state = contract.OutcomeRejected
				projectionErr = fmt.Errorf("declared relation does not bind the live relation: %w", err)
				break
			}
		}
		result, projectionErr = bound.result(observation)
		if projectionErr != nil {
			state = contract.OutcomeRejected
			break
		}
		state = contract.OutcomeSucceeded
	case pmruntime.SQLOutcomeUnknownCommit:
		state = contract.OutcomeUnknown
	default:
		state = contract.OutcomeUnsent
	}
	return ExecutionObservation{
		Result:   result,
		Outcomes: sqlMemberOutcomes(request, state, observedAt),
	}, errors.Join(projectionErr, executeErr)
}

// result publishes the exact typed row projection of one committed statement.
// Every row value keeps its exact declared type; a value without an exact typed
// projection refuses the whole result instead of rendering an approximation.
func (bound sqlBoundOperation) result(observation pmruntime.SQLObservation) (*contract.ActionResult, error) {
	rows := make([]contract.PresentValue, 0, len(observation.Rows))
	for _, row := range observation.Rows {
		if err := row.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrSQLStatementUnavailable, err)
		}
		fields := make(map[string]contract.PresentValue, len(row.Columns))
		for index, name := range row.Columns {
			if index >= len(row.Values) {
				return nil, fmt.Errorf("%w: returned row is incomplete", ErrSQLStatementUnavailable)
			}
			value, err := sqlRowValue(row.Values[index])
			if err != nil {
				return nil, err
			}
			fields[name] = value
		}
		rows = append(rows, contract.NewObjectValue(fields))
	}
	return &contract.ActionResult{
		MediaType: "application/json",
		Data:      contract.NewArrayValue(rows),
	}, nil
}

// sqlRowValue projects one decoded transport value onto its exact typed value.
func sqlRowValue(value pmruntime.SQLValue) (contract.PresentValue, error) {
	if value.IsAbsent() {
		return contract.PresentValue{}, fmt.Errorf("%w: a returned value cannot be absent", ErrSQLStatementUnavailable)
	}
	if value.IsNull() {
		return contract.NullValue(), nil
	}
	switch value.Kind() {
	case pmruntime.SQLText:
		text, ok := value.Text()
		if !ok {
			return contract.PresentValue{}, sqlRowValueError(value)
		}
		return contract.NewStringValue(text), nil
	case pmruntime.SQLInt8:
		number, ok := value.Number()
		if !ok || number.Scale != 0 {
			return contract.PresentValue{}, sqlRowValueError(value)
		}
		converted, err := contract.NewIntegerValue(number.Text)
		if err != nil {
			return contract.PresentValue{}, fmt.Errorf("%w: %v", ErrSQLStatementUnavailable, err)
		}
		return converted, nil
	case pmruntime.SQLNumeric:
		number, ok := value.Number()
		if !ok {
			return contract.PresentValue{}, sqlRowValueError(value)
		}
		converted, err := contract.NewDecimalValue(number.Text, number.Precision, number.Scale)
		if err != nil {
			return contract.PresentValue{}, fmt.Errorf("%w: %v", ErrSQLStatementUnavailable, err)
		}
		return converted, nil
	case pmruntime.SQLBoolean:
		boolean, ok := value.Boolean()
		if !ok {
			return contract.PresentValue{}, sqlRowValueError(value)
		}
		return contract.NewBooleanValue(boolean), nil
	case pmruntime.SQLTimestamptz:
		instant, ok := value.Instant()
		if !ok {
			return contract.PresentValue{}, sqlRowValueError(value)
		}
		converted, err := contract.NewInstantValue(instant.UTC().Format(sqlInstantLayout))
		if err != nil {
			return contract.PresentValue{}, fmt.Errorf("%w: %v", ErrSQLStatementUnavailable, err)
		}
		return converted, nil
	case pmruntime.SQLTimestamp:
		text, ok := value.Text()
		if !ok {
			return contract.PresentValue{}, sqlRowValueError(value)
		}
		converted, err := contract.NewLocalTimestampValue(sqlLocalTimestampText(text))
		if err != nil {
			return contract.PresentValue{}, fmt.Errorf("%w: %v", ErrSQLStatementUnavailable, err)
		}
		return converted, nil
	case pmruntime.SQLUUID, pmruntime.SQLJSON, pmruntime.SQLJSONB:
		text, ok := value.Text()
		if !ok {
			return contract.PresentValue{}, sqlRowValueError(value)
		}
		return contract.NewStringValue(text), nil
	default:
		return contract.PresentValue{}, sqlRowValueError(value)
	}
}

func sqlRowValueError(value pmruntime.SQLValue) error {
	return fmt.Errorf(
		"%w: returned %s value has no exact typed projection", ErrSQLStatementUnavailable, value.Kind(),
	)
}

// sqlLocalTimestampText renders one local wall clock with the canonical
// separator. Only the separator changes: the exact wall clock is retained.
func sqlLocalTimestampText(text string) string {
	if len(text) > 10 && text[10] == ' ' {
		return text[:10] + "T" + text[11:]
	}
	return text
}

// sqlTransportLocalTimestampText renders one exact contract local wall clock as
// the exact local wall clock this transport binds as a parameter. Only the
// separator changes, the exact inverse of sqlLocalTimestampText: a contract
// local timestamp always carries its canonical separator, and the transport
// binds exactly the PostgreSQL text a returned timestamp column carries.
func sqlTransportLocalTimestampText(text string) string {
	if len(text) > 10 && text[10] == 'T' {
		return text[:10] + " " + text[11:]
	}
	return text
}

// sqlMemberOutcomes settles every ordered member with the exact observed state.
// A definitive no-effect or unknown outcome carries the durable attempt receipt
// that lets settlement distinguish this attempt, never an inferred success.
func sqlMemberOutcomes(
	request ExecutionRequest,
	state contract.OutcomeState,
	observedAt time.Time,
) []contract.MemberOutcome {
	assertionRefs := []string{}
	if state == contract.OutcomeUnsent || state == contract.OutcomeUnknown {
		assertionRefs = []string{request.Attempt.ReceiptRef}
	}
	outcomes := make([]contract.MemberOutcome, 0, len(request.Dispatch.OrderedMemberIDs))
	for _, memberID := range request.Dispatch.OrderedMemberIDs {
		outcomes = append(outcomes, contract.MemberOutcome{
			MemberID:      memberID,
			EffectKey:     request.Dispatch.EffectKey,
			DispatchID:    request.Dispatch.DispatchID,
			State:         state,
			ObservedAt:    observedAt,
			AssertionRefs: append(make([]string, 0, len(assertionRefs)), assertionRefs...),
		})
	}
	return outcomes
}

// refusedSQLObservation projects one refusal that happened before the transport
// performed any physical work. The durable attempt exists, nothing was sent, so
// the settlement records an exact known-unsent member state.
func refusedSQLObservation(
	request ExecutionRequest,
	observedAt time.Time,
	cause error,
) (ExecutionObservation, error) {
	return ExecutionObservation{
		Outcomes: sqlMemberOutcomes(request, contract.OutcomeUnsent, observedAt),
	}, cause
}

// sqlCredentialExpectation requires the one-use credential material resolved at
// the execution boundary to be bound to the declared credential reference of
// this connection. A different credential refuses the attempt before any send.
type sqlCredentialExpectation struct {
	reference string
	inner     pmruntime.CredentialResolver
}

func (expectation sqlCredentialExpectation) ResolveCredential(
	ctx context.Context,
	call pmruntime.PhysicalCall,
) (credentials.CredentialMaterial, error) {
	if expectation.inner == nil {
		return nil, ErrSQLCredentialUnavailable
	}
	material, err := expectation.inner.ResolveCredential(ctx, call)
	if err != nil || material == nil {
		return nil, ErrSQLCredentialUnavailable
	}
	if material.CredentialBinding().CredentialRef != expectation.reference {
		material.Destroy()
		return nil, ErrSQLCredentialUnavailable
	}
	return material, nil
}

func containsLane(lanes []contract.Lane, lane contract.Lane) bool {
	for _, existing := range lanes {
		if existing == lane {
			return true
		}
	}
	return false
}

func containsCapability(
	capabilities []contract.CapabilityRequirement,
	requirement contract.CapabilityRequirement,
) bool {
	for _, existing := range capabilities {
		if existing == requirement {
			return true
		}
	}
	return false
}
