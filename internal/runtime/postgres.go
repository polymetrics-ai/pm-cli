package runtime

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

var (
	ErrSQLTransport        = errors.New("PostgreSQL transport failed")
	ErrSQLRequestTooLarge  = errors.New("encoded PostgreSQL statement exceeds its server-owned bound")
	ErrSQLResponseTooLarge = errors.New("PostgreSQL response exceeds its server-owned bound")
	ErrSQLResponseMismatch = errors.New("PostgreSQL response does not match the declared projection")
	ErrSQLLimits           = errors.New("invalid PostgreSQL transport limits")
)

const (
	maximumSQLResponseBytes = 1 << 30
	maximumSQLRows          = 1 << 24
	sqlCloseTimeout         = 5 * time.Second
)

// SQLOutcome classifies exactly what is known after one bounded attempt.
type SQLOutcome string

const (
	// SQLOutcomeKnownCommit means the server acknowledged the COMMIT.
	SQLOutcomeKnownCommit SQLOutcome = "known_commit"
	// SQLOutcomeKnownRollback means no commit could have happened: the
	// statement was refused, never sent, or the transaction ended without an
	// accepted COMMIT. A fresh re-admission may retry.
	SQLOutcomeKnownRollback SQLOutcome = "known_rollback"
	// SQLOutcomeUnknownCommit means the COMMIT was sent and its reply was
	// lost. The write may have been accepted; uncertainty is retained and the
	// attempt must never be replayed on this evidence.
	SQLOutcomeUnknownCommit SQLOutcome = "unknown_commit"
)

// Disposition projects the outcome onto the shared attempt disposition the
// admission and settlement seam already uses. A rollback was definitively not
// accepted; a lost COMMIT reply stays unknown.
func (outcome SQLOutcome) Disposition() (AttemptDisposition, bool) {
	switch outcome {
	case SQLOutcomeKnownCommit:
		return DispositionResponded, true
	case SQLOutcomeKnownRollback:
		return DispositionNotSent, true
	case SQLOutcomeUnknownCommit:
		return DispositionUnknown, true
	default:
		return "", false
	}
}

// SQLAttemptError classifies one failed PostgreSQL attempt without claiming an
// effect that was not observed.
type SQLAttemptError struct {
	Outcome SQLOutcome
	cause   error
}

func (e *SQLAttemptError) Error() string {
	switch e.Outcome {
	case SQLOutcomeUnknownCommit:
		return "PostgreSQL COMMIT outcome is unknown; the reply was lost after a possible accept"
	case SQLOutcomeKnownRollback:
		return "PostgreSQL attempt did not commit"
	default:
		return "PostgreSQL attempt failed"
	}
}

func (e *SQLAttemptError) Unwrap() error { return e.cause }

func (e *SQLAttemptError) Is(target error) bool {
	return target == ErrOutcomeUnknown && e.Outcome == SQLOutcomeUnknownCommit
}

// SQLLimits are server-owned bounds for one physical PostgreSQL attempt.
type SQLLimits struct {
	MaxConnectDuration   time.Duration
	MaxStatementDuration time.Duration
	MaxTotalDuration     time.Duration
	MaxRows              uint64
	MaxBytes             uint64
	AttemptNumber        uint32
	MaxAttempts          uint32
}

func (limits SQLLimits) validate() error {
	if limits.MaxConnectDuration <= 0 || limits.MaxStatementDuration <= 0 || limits.MaxTotalDuration <= 0 {
		return fmt.Errorf("%w: connect, statement, and total duration bounds are required", ErrSQLLimits)
	}
	if limits.MaxConnectDuration > limits.MaxTotalDuration || limits.MaxStatementDuration > limits.MaxTotalDuration {
		return fmt.Errorf("%w: phase bounds cannot exceed the total bound", ErrSQLLimits)
	}
	if limits.MaxRows == 0 || limits.MaxRows > maximumSQLRows {
		return fmt.Errorf("%w: row bound must be within 1..%d", ErrSQLLimits, maximumSQLRows)
	}
	if limits.MaxBytes == 0 || limits.MaxBytes > maximumSQLResponseBytes {
		return fmt.Errorf("%w: byte bound must be within 1..%d", ErrSQLLimits, maximumSQLResponseBytes)
	}
	if limits.AttemptNumber == 0 || limits.MaxAttempts == 0 || limits.AttemptNumber > limits.MaxAttempts {
		return fmt.Errorf("%w: attempt ordinal is outside its finite budget", ErrSQLLimits)
	}
	return nil
}

// callLimits projects the PostgreSQL bounds onto the shared physical-call
// validation. One declared host is dialed with no name resolution and no
// provider-supplied retry delay is accepted, so those HTTP-only fields carry
// the smallest truthful values.
func (limits SQLLimits) callLimits() CallLimits {
	return CallLimits{
		MaxRequestBytes:  limits.MaxBytes,
		MaxResponseBytes: limits.MaxBytes,
		MaxDNSAnswers:    1,
		AttemptNumber:    limits.AttemptNumber,
		MaxAttempts:      limits.MaxAttempts,
		MaxRetryDelay:    limits.MaxStatementDuration,
	}
}

// SQLConnection is the exact structured connection binding for one customer
// PostgreSQL endpoint. Public input never carries a raw DSN.
type SQLConnection struct {
	Host     string
	Port     uint16
	Database string
	User     string
	TLS      SQLTLSConfig
}

func (connection SQLConnection) Validate() error {
	if err := connection.TLS.validate(); err != nil {
		return err
	}
	if err := validateSQLConnectionPart("host", connection.Host, 253); err != nil {
		return err
	}
	if connection.Port == 0 {
		return fmt.Errorf("%w: port is absent", ErrSQLTransport)
	}
	if err := validateSQLConnectionPart("database", connection.Database, maximumSQLIdentifierBytes); err != nil {
		return err
	}
	return validateSQLConnectionPart("user", connection.User, maximumSQLIdentifierBytes)
}

func validateSQLConnectionPart(name, value string, maximum int) error {
	if value == "" {
		return fmt.Errorf("%w: %s is absent", ErrSQLTransport, name)
	}
	if len(value) > maximum || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return fmt.Errorf("%w: %s is not a bounded declared value", ErrSQLTransport, name)
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character <= 0x20 || character >= 0x7f {
			return fmt.Errorf("%w: %s contains an unusable character", ErrSQLTransport, name)
		}
		switch character {
		case ',', '\'', '"', '\\', '/', '@', ':', '?', '#', '=', '[':
			return fmt.Errorf("%w: %s must be one plain declared value", ErrSQLTransport, name)
		}
	}
	return nil
}

// SQLCall is one declared PostgreSQL execution request: the source-declared
// operation, the structured endpoint facts, the bound relation, the immutable
// physical-call authority, and the exact mapped input.
type SQLCall struct {
	Operation  contract.Operation
	Connection SQLConnection
	Relation   SQLRelation
	Call       PhysicalCall
	Input      []SQLColumnValue
	Limits     SQLLimits
}

// SQLObservation is the bounded result of one PostgreSQL attempt. It never
// claims more than the transport observed: a lost COMMIT reply stays unknown.
// Rows is the bounded projection of the statement response. A read keeps its
// rows for every outcome; a mapped write's returned rows are withheld unless
// the COMMIT was acknowledged, so an attempted effect is never presented as an
// accepted one.
type SQLObservation struct {
	Outcome       SQLOutcome
	PhysicalSend  bool
	Retry         RetryDirective
	Statement     string
	Parameters    int
	Rows          []SQLRow
	Truncated     bool
	BufferedBytes uint64
	TLS           SQLTLSWitness
	ObservedAt    time.Time
}

// SQLDependencies are the process-static seams of the reusable transport.
type SQLDependencies struct {
	// Admitter re-authorizes every physical send against current durable state.
	Admitter PhysicalCallAdmitter
	// Credentials resolves one-use credential material at the execution
	// boundary. There is no other credential path.
	Credentials CredentialResolver
	// Dialer is the context-aware dialer used for the physical connection.
	Dialer ContextDialer
	// Now supplies the observation time; nil uses the process clock.
	Now func() time.Time

	// frontend is the in-package protocol seam used by the bounded transport
	// tests to observe or fault the exact PostgreSQL protocol boundary. Nil
	// uses the driver's own frontend.
	frontend func(io.Reader, io.Writer) *pgproto3.Frontend
}

// SQLTransport is the reusable bounded PostgreSQL connector transport. Its zero
// value is unavailable: without a current admission seam every attempt is
// refused before any physical work.
type SQLTransport struct {
	Dependencies SQLDependencies
}

// Execute performs at most one admitted PostgreSQL transaction: one connection
// and BEGIN, the declared parameterized statements, and one COMMIT. A metadata
// or select operation sends one statement. A mapped upsert locks and compares
// its keyed row, then inserts it, updates exactly its changed columns, or
// writes nothing, all inside the same transaction (see sqlMappedWrite). Every
// physical send is separately re-authorized through the shared admission seam,
// and one-use credential material is resolved only at the execution boundary.
// It never retries the attempt, never sends a write statement twice, never
// creates a unit-ledger table, and never claims transactional idempotency on an
// unmanaged customer target.
func (transport SQLTransport) Execute(ctx context.Context, call SQLCall) (SQLObservation, error) {
	if ctx == nil {
		return SQLObservation{}, context.Canceled
	}
	now := time.Now
	if transport.Dependencies.Now != nil {
		now = transport.Dependencies.Now
	}
	current := now().UTC()
	observation := SQLObservation{Outcome: SQLOutcomeKnownRollback, ObservedAt: current}
	if err := call.Limits.validate(); err != nil {
		return observation, err
	}
	if err := validateSQLSourceFacts(call.Operation); err != nil {
		return observation, err
	}
	if err := call.Connection.Validate(); err != nil {
		return observation, err
	}
	if err := call.Call.validate(call.Operation, call.Limits.callLimits()); err != nil {
		return observation, err
	}
	if !current.Before(call.Call.Deadline) {
		return observation, context.DeadlineExceeded
	}
	statement, err := BuildSQLStatement(call.Operation, call.Relation, call.Input, call.Limits.MaxRows+1)
	if err != nil {
		return observation, err
	}
	var plan *sqlMappedWrite
	planned := []SQLStatement{statement}
	if statement.Kind == contract.PostgreSQLStatementMappedUpsert {
		write, err := buildSQLMappedWrite(call.Operation, call.Relation, call.Input)
		if err != nil {
			return observation, err
		}
		plan = &write
		planned = []SQLStatement{write.compare, write.insert}
	}
	// Every statement this attempt may send is bounded before any physical
	// work; a later update statement is bounded again before its own send.
	for _, candidate := range planned {
		if _, err := sqlStatementBindings(candidate, call.Limits); err != nil {
			return observation, err
		}
	}
	observation.Statement = statement.Text
	observation.Parameters = len(statement.Parameters)
	tlsConfig, err := NewStrictSQLTLS(call.Connection.TLS)
	if err != nil {
		return observation, err
	}
	boundedCtx, cancel := context.WithDeadline(ctx, earlierDeadline(call.Call.Deadline, current.Add(call.Limits.MaxTotalDuration)))
	defer cancel()
	config, err := buildSQLConnectConfig(
		call.Connection, call.Limits, tlsConfig, transport.Dependencies.Dialer, transport.Dependencies.frontend,
	)
	if err != nil {
		return observation, err
	}
	connectCtx, cancelConnect := context.WithTimeout(boundedCtx, call.Limits.MaxConnectDuration)
	defer cancelConnect()
	// Boundary one: the connection and its BEGIN are admitted together.
	if err := admitPhysicalCall(connectCtx, transport.Dependencies.Admitter, call.Call); err != nil {
		return observation, err
	}
	password, err := resolveSQLPassword(connectCtx, transport.Dependencies.Credentials, call.Call)
	if err != nil {
		return observation, err
	}
	config.Password = password
	connection, err := pgconn.ConnectConfig(connectCtx, config)
	config.Password = ""
	password = ""
	if err != nil {
		observation.Retry = sqlRetryFor(DispositionNotSent, call.Limits, "definitive_unsent")
		return observation, &SQLAttemptError{Outcome: SQLOutcomeKnownRollback, cause: fmt.Errorf("%w: connect: %w", ErrSQLTransport, err)}
	}
	defer closeSQLConnection(connection)
	observation.PhysicalSend = true
	witness, err := ObserveStrictSQLTLS(connection.Conn(), call.Connection.TLS.ServerName, now().UTC())
	if err != nil {
		return observation, err
	}
	observation.TLS = witness
	if err := sqlSimpleStatement(connectCtx, connection, "begin"); err != nil {
		observation.Retry = sqlRetryFor(DispositionNotSent, call.Limits, "definitive_unsent")
		return observation, &SQLAttemptError{Outcome: SQLOutcomeKnownRollback, cause: fmt.Errorf("%w: begin: %w", ErrSQLTransport, err)}
	}
	// Boundary two: every parameterized statement send of the transaction.
	session := sqlSession{transport: transport, connection: connection, call: call, observation: &observation}
	var rows []SQLRow
	var truncated bool
	if plan != nil {
		rows, err = session.writeMapped(boundedCtx, *plan)
	} else {
		rows, truncated, err = session.send(boundedCtx, statement)
	}
	if err != nil {
		transport.abort(boundedCtx, connection, call.Call)
		var refused *sqlAdmissionRefusal
		if errors.As(err, &refused) {
			observation.withholdWriteRows(statement.Kind)
			return observation, refused.cause
		}
		observation.Retry = sqlRetryFor(DispositionNotSent, call.Limits, "definitive_rolled_back")
		return observation, &SQLAttemptError{Outcome: SQLOutcomeKnownRollback, cause: err}
	}
	observation.Rows = rows
	observation.Truncated = truncated
	commitCtx, cancelCommit := context.WithTimeout(boundedCtx, call.Limits.MaxStatementDuration)
	defer cancelCommit()
	// Boundary three: the COMMIT request.
	if err := admitPhysicalCall(commitCtx, transport.Dependencies.Admitter, call.Call); err != nil {
		transport.abort(boundedCtx, connection, call.Call)
		observation.withholdWriteRows(statement.Kind)
		return observation, err
	}
	outcome, commitErr := sqlCommit(commitCtx, connection)
	observation.Outcome = outcome
	if commitErr != nil {
		if outcome == SQLOutcomeKnownRollback {
			observation.Retry = sqlRetryFor(DispositionNotSent, call.Limits, "definitive_rolled_back")
		}
		observation.withholdWriteRows(statement.Kind)
		return observation, &SQLAttemptError{Outcome: outcome, cause: commitErr}
	}
	return observation, nil
}

// sqlSession is one open admitted transaction of one attempt.
type sqlSession struct {
	transport   SQLTransport
	connection  *pgconn.PgConn
	call        SQLCall
	observation *SQLObservation
}

// sqlAdmissionRefusal reports that current authority refused one statement
// send, so the statement never reached the target.
type sqlAdmissionRefusal struct{ cause error }

func (refusal *sqlAdmissionRefusal) Error() string { return refusal.cause.Error() }

func (refusal *sqlAdmissionRefusal) Unwrap() error { return refusal.cause }

// maximumSQLMappedWriteRounds bounds the compare-then-write rounds of one
// mapped write. A second round happens only when a concurrent transaction
// inserted the same key after this one compared it; the compare of the second
// round locks that committed row, so a third round cannot be needed.
const maximumSQLMappedWriteRounds = 2

// send admits and sends one parameterized statement of the open transaction
// and reads at most its declared row and byte bounds.
func (session sqlSession) send(ctx context.Context, statement SQLStatement) ([]SQLRow, bool, error) {
	bindings, err := sqlStatementBindings(statement, session.call.Limits)
	if err != nil {
		return nil, false, err
	}
	statementCtx, cancel := context.WithTimeout(ctx, session.call.Limits.MaxStatementDuration)
	defer cancel()
	if err := admitPhysicalCall(statementCtx, session.transport.Dependencies.Admitter, session.call.Call); err != nil {
		return nil, false, &sqlAdmissionRefusal{cause: err}
	}
	session.observation.Statement = statement.Text
	session.observation.Parameters = len(statement.Parameters)
	rows, truncated, buffered, err := readSQLStatement(statementCtx, session.connection, statement, bindings, session.call.Limits)
	session.observation.BufferedBytes, _ = addUint64(session.observation.BufferedBytes, buffered)
	if err != nil {
		return nil, false, err
	}
	return rows, truncated, nil
}

// writeMapped performs one changed-column-only mapped write inside the open
// transaction. It returns the written row, or no row when every mapped value
// already equals the stored one and nothing was assigned.
func (session sqlSession) writeMapped(ctx context.Context, plan sqlMappedWrite) ([]SQLRow, error) {
	for round := 0; round < maximumSQLMappedWriteRounds; round++ {
		compared, err := session.sendKeyed(ctx, plan.compare)
		if err != nil {
			return nil, err
		}
		present, changed, err := plan.changedColumns(compared)
		if err != nil {
			return nil, err
		}
		if present {
			if len(changed) == 0 {
				return nil, nil
			}
			written, err := session.sendKeyed(ctx, plan.updateStatement(changed))
			if err != nil {
				return nil, err
			}
			if len(written) != 1 {
				return nil, fmt.Errorf("%w: the locked keyed row was not updated exactly once", ErrSQLResponseMismatch)
			}
			return written, nil
		}
		inserted, err := session.sendKeyed(ctx, plan.insert)
		if err != nil {
			return nil, err
		}
		if len(inserted) == 1 {
			return inserted, nil
		}
		if len(inserted) != 0 {
			return nil, ErrSQLResponseMismatch
		}
		// A concurrent transaction committed the same key after the compare;
		// compare the now-present row again instead of assigning blindly.
	}
	return nil, fmt.Errorf("%w: the keyed row stayed contended across the bounded write rounds", ErrSQLStatement)
}

// sendKeyed sends one statement of a mapped write. Each one addresses exactly
// one declared key, so a response beyond the row bound is a key that matched
// more than one row, never a prefix to settle on.
func (session sqlSession) sendKeyed(ctx context.Context, statement SQLStatement) ([]SQLRow, error) {
	rows, truncated, err := session.send(ctx, statement)
	if err != nil {
		return nil, err
	}
	if truncated || len(rows) > 1 {
		return nil, fmt.Errorf("%w: the declared key matched more than one row", ErrSQLStatement)
	}
	return rows, nil
}

// sqlStatementBindings encodes one statement's parameters and refuses a
// statement whose encoded text and bindings exceed the server-owned bound.
func sqlStatementBindings(statement SQLStatement, limits SQLLimits) ([][]byte, error) {
	bindings, err := statement.parameterBytes()
	if err != nil {
		return nil, err
	}
	encodedBytes := uint64(len(statement.Text))
	for _, binding := range bindings {
		encodedBytes, err = addUint64(encodedBytes, uint64(len(binding)))
		if err != nil {
			return nil, err
		}
	}
	if encodedBytes > limits.MaxBytes {
		return nil, ErrSQLRequestTooLarge
	}
	return bindings, nil
}

// abort ends an open transaction. A ROLLBACK is itself a physical send, so it
// is admitted too; when current authority refuses it the connection is closed,
// which makes PostgreSQL abort the transaction.
func (transport SQLTransport) abort(ctx context.Context, connection *pgconn.PgConn, call PhysicalCall) {
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sqlCloseTimeout)
	defer cancel()
	if err := admitPhysicalCall(abortCtx, transport.Dependencies.Admitter, call); err != nil {
		return
	}
	_ = sqlSimpleStatement(abortCtx, connection, "rollback")
}

// sqlRetryFor states the only retry a PostgreSQL attempt ever earns: a
// definitively unsent or definitively rolled-back transaction may be retried
// after a fresh admission. An unknown COMMIT outcome never is.
func sqlRetryFor(disposition AttemptDisposition, limits SQLLimits, reason string) RetryDirective {
	if limits.AttemptNumber >= limits.MaxAttempts {
		return RetryDirective{}
	}
	switch disposition {
	case DispositionNotSent:
		return RetryDirective{Eligible: true, Reason: reason}
	default:
		return RetryDirective{}
	}
}

// withholdWriteRows drops a mapped write's returned rows when the transaction
// did not commit.
func (observation *SQLObservation) withholdWriteRows(kind contract.PostgreSQLStatementKind) {
	if kind == contract.PostgreSQLStatementMappedUpsert {
		observation.Rows = nil
		observation.Truncated = false
	}
}

func earlierDeadline(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}

func resolveSQLPassword(ctx context.Context, resolver CredentialResolver, call PhysicalCall) (string, error) {
	if resolver == nil {
		return "", ErrCredentialRequired
	}
	material, err := resolver.ResolveCredential(ctx, call)
	if err != nil || material == nil {
		return "", ErrCredentialRequired
	}
	defer material.Destroy()
	binding := material.CredentialBinding()
	if err := binding.Validate(); err != nil || binding.WorkspaceID != call.Scope.WorkspaceID ||
		binding.ConnectionID != call.ConnectionID || binding.ProviderAccountWitness != call.AccountWitness {
		return "", ErrCredentialRequired
	}
	password := ""
	useErr := material.Use(func(secret []byte) error {
		if len(secret) == 0 {
			return ErrCredentialRequired
		}
		// The driver's connection configuration carries the password as a
		// string for the duration of the handshake only; the one-use material
		// is destroyed as soon as this returns.
		password = string(secret)
		return nil
	})
	if useErr != nil {
		return "", ErrCredentialRequired
	}
	return password, nil
}

// buildSQLConnectConfig derives the driver configuration from the declared
// structured facts. No raw DSN is accepted from public input, and every
// security-relevant parsed fact is compared back before use.
func buildSQLConnectConfig(
	connection SQLConnection,
	limits SQLLimits,
	tlsConfig *tls.Config,
	dialer ContextDialer,
	frontend func(io.Reader, io.Writer) *pgproto3.Frontend,
) (*pgconn.Config, error) {
	if err := connection.Validate(); err != nil {
		return nil, err
	}
	if err := limits.validate(); err != nil {
		return nil, err
	}
	if tlsConfig == nil || tlsConfig.InsecureSkipVerify || tlsConfig.RootCAs == nil ||
		tlsConfig.MinVersion < minimumSQLTLSVersion || tlsConfig.ServerName != connection.TLS.ServerName {
		return nil, ErrSQLTLSRequired
	}
	connString := "host=" + quoteSQLConnStringValue(connection.Host) +
		" port=" + strconv.Itoa(int(connection.Port)) +
		" dbname=" + quoteSQLConnStringValue(connection.Database) +
		" user=" + quoteSQLConnStringValue(connection.User) +
		" sslmode=verify-full"
	config, err := pgconn.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("%w: declared endpoint facts could not be applied", ErrSQLTransport)
	}
	if config.Host != connection.Host || config.Port != connection.Port ||
		config.Database != connection.Database || config.User != connection.User || len(config.Fallbacks) != 0 {
		return nil, fmt.Errorf("%w: parsed endpoint facts differ from the declared endpoint", ErrSQLTransport)
	}
	config.TLSConfig = tlsConfig
	config.Password = ""
	config.ConnectTimeout = limits.MaxConnectDuration
	config.SSLNegotiation = ""
	config.KerberosSrvName = ""
	config.KerberosSpn = ""
	config.OAuthTokenProvider = nil
	config.AfterConnect = nil
	config.AfterNetConnect = nil
	config.ValidateConnect = nil
	config.OnNotice = nil
	config.OnNotification = nil
	config.RequireAuth = ""
	config.ChannelBinding = ""
	config.RuntimeParams = map[string]string{
		// Fully qualified statements never depend on an ambient search path,
		// and instants are rendered in UTC so their exact text is deterministic.
		"search_path": "pg_catalog",
		"timezone":    "UTC",
	}
	bodyLimit := limits.MaxBytes + (1 << 20)
	if bodyLimit > uint64(maximumSQLResponseBytes) {
		bodyLimit = maximumSQLResponseBytes
	}
	config.MaxProtocolMessageBodyLen = int(bodyLimit)
	if frontend != nil {
		config.BuildFrontend = frontend
	}
	if dialer != nil {
		config.DialFunc = dialer.DialContext
	}
	return config, nil
}

// quoteSQLConnStringValue renders one already-declared fact for the driver's
// keyword/value connection string.
func quoteSQLConnStringValue(value string) string {
	return "'" + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `'`, `\'`) + "'"
}

func sqlSimpleStatement(ctx context.Context, connection *pgconn.PgConn, statement string) error {
	results, err := connection.Exec(ctx, statement).ReadAll()
	if err != nil {
		return err
	}
	for _, result := range results {
		if result.Err != nil {
			return result.Err
		}
	}
	return nil
}

// readSQLStatement sends the parameterized statement and reads at most its
// declared row and byte bounds. Every parameter is bound; no caller value
// appears in the statement text.
func readSQLStatement(
	ctx context.Context,
	connection *pgconn.PgConn,
	statement SQLStatement,
	bindings [][]byte,
	limits SQLLimits,
) ([]SQLRow, bool, uint64, error) {
	reader := connection.ExecParams(ctx, statement.Text, bindings, nil, nil, nil)
	defer reader.Close()
	columns := statement.Columns
	rows := make([]SQLRow, 0, minUint64(limits.MaxRows, 64))
	buffered := uint64(0)
	truncated := false
	for reader.NextRow() {
		if uint64(len(rows)) >= limits.MaxRows {
			truncated = true
			break
		}
		values := reader.Values()
		if len(values) != len(columns) {
			return nil, false, buffered, ErrSQLResponseMismatch
		}
		row := SQLRow{Columns: make([]string, len(columns)), Values: make([]SQLValue, len(columns))}
		rowBytes := uint64(0)
		for index, column := range columns {
			value, err := decodeSQLTextValue(column, values[index])
			if err != nil {
				return nil, false, buffered, err
			}
			row.Columns[index] = column.Name
			row.Values[index] = value
			rowBytes, err = addUint64(rowBytes, uint64(len(value.text)+len(value.raw)))
			if err != nil {
				return nil, false, buffered, err
			}
		}
		total, err := addUint64(buffered, rowBytes)
		if err != nil {
			return nil, false, buffered, err
		}
		buffered = total
		if buffered > limits.MaxBytes {
			return nil, false, buffered, ErrSQLResponseTooLarge
		}
		rows = append(rows, row)
	}
	if _, err := reader.Close(); err != nil {
		return nil, false, buffered, fmt.Errorf("%w: statement: %w", ErrSQLTransport, err)
	}
	return rows, truncated, buffered, nil
}

// sqlCommit sends the COMMIT and classifies exactly what is known. A server
// error response means the transaction was aborted; a lost reply after the
// COMMIT was sent stays unknown.
func sqlCommit(ctx context.Context, connection *pgconn.PgConn) (SQLOutcome, error) {
	results, err := connection.Exec(ctx, "commit").ReadAll()
	if err == nil {
		for _, result := range results {
			if result.Err != nil {
				err = result.Err
				break
			}
		}
	}
	if err == nil {
		return SQLOutcomeKnownCommit, nil
	}
	var serverError *pgconn.PgError
	if errors.As(err, &serverError) {
		// The server answered: the COMMIT was refused and the transaction was
		// aborted, so nothing was committed.
		return SQLOutcomeKnownRollback, fmt.Errorf("%w: commit refused: %s", ErrSQLTransport, serverError.Message)
	}
	return SQLOutcomeUnknownCommit, fmt.Errorf("%w: commit: %w", ErrSQLTransport, err)
}

func closeSQLConnection(connection *pgconn.PgConn) {
	if connection == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), sqlCloseTimeout)
	defer cancel()
	_ = connection.Close(ctx)
}

func minUint64(left, right uint64) uint64 {
	if left < right {
		return left
	}
	return right
}
