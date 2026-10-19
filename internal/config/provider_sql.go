package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const (
	// maxProviderSQLConnections bounds how many customer SQL connections one
	// deployment may declare, so composition stays finite and inspectable.
	maxProviderSQLConnections = 32
	// maxProviderSQLBindings bounds the permitted relation bindings of one
	// connection.
	maxProviderSQLBindings = 64
	// maxProviderSQLKeyColumns bounds the declared key columns of one binding.
	maxProviderSQLKeyColumns = 8
	// maximumProviderSQLIdentifierBytes is the largest declared relation,
	// schema, table, database or user identifier, matching PostgreSQL's own
	// identifier length.
	maximumProviderSQLIdentifierBytes = 63
	// maxProviderSQLStateDirectoryBytes bounds one declared connection state
	// directory path.
	maxProviderSQLStateDirectoryBytes = 4096
	// providerSQLMaxRows and providerSQLMaxBytes are the largest bounds the
	// transport itself accepts for one attempt.
	providerSQLMaxRows  = 1 << 24
	providerSQLMaxBytes = 1 << 30
)

// ProviderSQLConfiguration is absent unless the operator explicitly declares
// complete finite SQL bounds and at least one scoped customer connection. Every
// member is server-owned: it binds one declared connector and connection
// identity to the structured endpoint facts, the operator secret references,
// the permitted relation bindings and the finite bounds this deployment admits.
// It never carries a raw DSN, a secret value or a provider body field.
type ProviderSQLConfiguration struct {
	bounds      ProviderSQLBounds
	connections []ProviderSQLConnection
	policies    map[providerSQLConnectionKey]ProviderSQLConnection
	configured  bool
}

// ProviderSQLBounds are the declared finite bounds of every admitted customer
// SQL attempt: the phase and total deadlines, the row and byte bounds of one
// statement response, and the attempt ceiling.
type ProviderSQLBounds struct {
	MaxConnectDuration   time.Duration
	MaxStatementDuration time.Duration
	MaxTotalDuration     time.Duration
	MaxRows              uint64
	MaxBytes             uint64
	MaxAttempts          uint32
}

// ProviderSQLConnection is one declared customer SQL connection. The credential
// itself is never declared: PasswordRef names the exact credential reference the
// connection's one-use material must be bound to, and TLSTrustRef names the
// operator secret bundle that carries the declared trust anchor.
type ProviderSQLConnection struct {
	ConnectorID    string
	ConnectionID   string
	Host           string
	Port           uint16
	Database       string
	User           string
	TLSTrustRef    string
	PasswordRef    string
	StateDirectory string
	Bindings       []ProviderSQLRelationBinding

	// scope is the declared organization and workspace this connection belongs
	// to. It is bound when the declaration is resolved and never travels as a
	// public connection fact.
	scope contract.Scope
}

// ProviderSQLRelationBinding binds one operation-declared logical relation
// identity to the concrete schema, table and key columns the server owns. The
// selected discovery that supplies the relation's column metadata is named by
// the snapshot facts, so no column type is ever inferred from a live guess.
type ProviderSQLRelationBinding struct {
	Binding    string
	Schema     string
	Table      string
	KeyColumns []string
	Snapshot   ProviderSQLSnapshotBinding
}

// ProviderSQLSnapshotBinding is the exact discovery scope the declared relation
// binding resolves its selected snapshot under.
type ProviderSQLSnapshotBinding struct {
	ProviderAccountID  string
	PermissionRevision string
	Resource           string
	SourceRevision     string
}

type providerSQLConnectionKey struct {
	orgID        string
	workspaceID  string
	connectionID string
}

// Scope returns the exact declared organization and workspace this connection
// belongs to. It is part of the declaration, never a public request fact.
func (connection ProviderSQLConnection) Scope() contract.Scope {
	return connection.scope
}

// Configured reports whether at least one customer SQL connection is declared.
func (configuration ProviderSQLConfiguration) Configured() bool {
	return configuration.configured
}

// Bounds returns the declared finite bounds.
func (configuration ProviderSQLConfiguration) Bounds() ProviderSQLBounds {
	if !configuration.configured {
		return ProviderSQLBounds{}
	}
	return configuration.bounds
}

// Connections returns a detached copy of every declared connection in
// declaration order.
func (configuration ProviderSQLConfiguration) Connections() []ProviderSQLConnection {
	connections := make([]ProviderSQLConnection, 0, len(configuration.connections))
	for _, connection := range configuration.connections {
		connections = append(connections, cloneProviderSQLConnection(connection))
	}
	return connections
}

// Connection returns a detached copy only for the exact declared organization,
// workspace and connection binding. It never falls back to another scope,
// connection or relation binding.
func (configuration ProviderSQLConfiguration) Connection(
	scope contract.Scope,
	connectionID string,
) (ProviderSQLConnection, bool) {
	if !configuration.configured {
		return ProviderSQLConnection{}, false
	}
	connection, present := configuration.policies[providerSQLConnectionKey{
		orgID: scope.OrgID, workspaceID: scope.WorkspaceID, connectionID: connectionID,
	}]
	if !present {
		return ProviderSQLConnection{}, false
	}
	return cloneProviderSQLConnection(connection), true
}

func (configuration ProviderSQLConfiguration) clone() ProviderSQLConfiguration {
	cloned := configuration
	cloned.connections = make([]ProviderSQLConnection, 0, len(configuration.connections))
	for _, connection := range configuration.connections {
		cloned.connections = append(cloned.connections, cloneProviderSQLConnection(connection))
	}
	if configuration.policies != nil {
		cloned.policies = make(map[providerSQLConnectionKey]ProviderSQLConnection, len(configuration.policies))
		for key, connection := range configuration.policies {
			cloned.policies[key] = cloneProviderSQLConnection(connection)
		}
	}
	return cloned
}

func cloneProviderSQLConnection(connection ProviderSQLConnection) ProviderSQLConnection {
	cloned := connection
	cloned.Bindings = make([]ProviderSQLRelationBinding, 0, len(connection.Bindings))
	for _, binding := range connection.Bindings {
		clonedBinding := binding
		clonedBinding.KeyColumns = append([]string(nil), binding.KeyColumns...)
		cloned.Bindings = append(cloned.Bindings, clonedBinding)
	}
	return cloned
}

var providerSQLRequiredFields = []string{
	"max_connect_duration",
	"max_statement_duration",
	"max_total_duration",
	"max_rows",
	"max_bytes",
	"max_attempts",
	"connections",
}

var providerSQLFields = providerSQLRequiredFields

var providerSQLConnectionFields = []string{
	"org_id",
	"workspace_id",
	"connection_id",
	"connector_id",
	"host",
	"port",
	"database",
	"user",
	"tls_trust_ref",
	"password_ref",
	"state_directory",
	"relation_bindings",
}

var providerSQLBindingFields = []string{
	"binding",
	"schema",
	"table",
	"key_columns",
	"snapshot",
}

var providerSQLSnapshotFields = []string{
	"provider_account_id",
	"permission_revision",
	"resource",
	"source_revision",
}

func decodeProviderSQL(raw json.RawMessage) (ProviderSQLConfiguration, error) {
	if len(raw) == 0 {
		return ProviderSQLConfiguration{}, nil
	}
	object, err := decodeObject(raw, "provider_sql", providerSQLFields, providerSQLRequiredFields)
	if err != nil {
		return ProviderSQLConfiguration{}, err
	}
	configuration := ProviderSQLConfiguration{configured: true}
	if configuration.bounds.MaxConnectDuration, err = decodeDuration(
		object,
		"max_connect_duration",
		"provider_sql.max_connect_duration",
	); err != nil {
		return ProviderSQLConfiguration{}, err
	}
	if configuration.bounds.MaxStatementDuration, err = decodeDuration(
		object,
		"max_statement_duration",
		"provider_sql.max_statement_duration",
	); err != nil {
		return ProviderSQLConfiguration{}, err
	}
	if configuration.bounds.MaxTotalDuration, err = decodeDuration(
		object,
		"max_total_duration",
		"provider_sql.max_total_duration",
	); err != nil {
		return ProviderSQLConfiguration{}, err
	}
	if configuration.bounds.MaxRows, err = decodeUint64(
		object,
		"max_rows",
		"provider_sql.max_rows",
	); err != nil {
		return ProviderSQLConfiguration{}, err
	}
	if configuration.bounds.MaxBytes, err = decodeUint64(
		object,
		"max_bytes",
		"provider_sql.max_bytes",
	); err != nil {
		return ProviderSQLConfiguration{}, err
	}
	if configuration.bounds.MaxAttempts, err = decodeProviderSQLUint32(
		object,
		"max_attempts",
		"provider_sql.max_attempts",
	); err != nil {
		return ProviderSQLConfiguration{}, err
	}
	if configuration.connections, err = decodeProviderSQLConnections(object["connections"]); err != nil {
		return ProviderSQLConfiguration{}, err
	}
	return configuration, nil
}

func decodeProviderSQLUint32(
	object map[string]json.RawMessage,
	name string,
	field string,
) (uint32, error) {
	value, err := decodeUint64(object, name, field)
	if err != nil {
		return 0, err
	}
	const maximum = uint64(1<<32 - 1)
	if value > maximum {
		return 0, diagnostic(field, DiagnosticBounds)
	}
	return uint32(value), nil
}

func decodeProviderSQLConnections(raw json.RawMessage) ([]ProviderSQLConnection, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, diagnostic("provider_sql.connections", DiagnosticType)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(trimmed, &entries); err != nil {
		return nil, diagnostic("provider_sql.connections", DiagnosticType)
	}
	connections := make([]ProviderSQLConnection, len(entries))
	for index, entry := range entries {
		path := "provider_sql.connections[" + strconv.Itoa(index) + "]"
		object, err := decodeObject(entry, path, providerSQLConnectionFields, providerSQLConnectionFields)
		if err != nil {
			return nil, err
		}
		if connections[index], err = decodeProviderSQLConnection(object, path); err != nil {
			return nil, err
		}
	}
	return connections, nil
}

func decodeProviderSQLConnection(
	object map[string]json.RawMessage,
	path string,
) (ProviderSQLConnection, error) {
	connection := ProviderSQLConnection{}
	scope := contract.Scope{}
	var err error
	if scope.OrgID, err = decodeString(object, "org_id", path+".org_id"); err != nil {
		return ProviderSQLConnection{}, err
	}
	if scope.WorkspaceID, err = decodeString(object, "workspace_id", path+".workspace_id"); err != nil {
		return ProviderSQLConnection{}, err
	}
	connection.scope = scope
	if connection.ConnectionID, err = decodeString(object, "connection_id", path+".connection_id"); err != nil {
		return ProviderSQLConnection{}, err
	}
	if connection.ConnectorID, err = decodeString(object, "connector_id", path+".connector_id"); err != nil {
		return ProviderSQLConnection{}, err
	}
	if connection.Host, err = decodeString(object, "host", path+".host"); err != nil {
		return ProviderSQLConnection{}, err
	}
	port, err := decodeUint64(object, "port", path+".port")
	if err != nil {
		return ProviderSQLConnection{}, err
	}
	const maximumPort = uint64(1<<16 - 1)
	if port > maximumPort {
		return ProviderSQLConnection{}, diagnostic(path+".port", DiagnosticBounds)
	}
	connection.Port = uint16(port)
	if connection.Database, err = decodeString(object, "database", path+".database"); err != nil {
		return ProviderSQLConnection{}, err
	}
	if connection.User, err = decodeString(object, "user", path+".user"); err != nil {
		return ProviderSQLConnection{}, err
	}
	if connection.TLSTrustRef, err = decodeString(object, "tls_trust_ref", path+".tls_trust_ref"); err != nil {
		return ProviderSQLConnection{}, err
	}
	if connection.PasswordRef, err = decodeString(object, "password_ref", path+".password_ref"); err != nil {
		return ProviderSQLConnection{}, err
	}
	if connection.StateDirectory, err = decodeString(object, "state_directory", path+".state_directory"); err != nil {
		return ProviderSQLConnection{}, err
	}
	if connection.Bindings, err = decodeProviderSQLBindings(
		object["relation_bindings"],
		path+".relation_bindings",
	); err != nil {
		return ProviderSQLConnection{}, err
	}
	return connection, nil
}

func decodeProviderSQLBindings(raw json.RawMessage, path string) ([]ProviderSQLRelationBinding, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, diagnostic(path, DiagnosticType)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(trimmed, &entries); err != nil {
		return nil, diagnostic(path, DiagnosticType)
	}
	bindings := make([]ProviderSQLRelationBinding, len(entries))
	for index, entry := range entries {
		entryPath := path + "[" + strconv.Itoa(index) + "]"
		object, err := decodeObject(entry, entryPath, providerSQLBindingFields, providerSQLBindingFields)
		if err != nil {
			return nil, err
		}
		binding := ProviderSQLRelationBinding{}
		for _, member := range []struct {
			name   string
			target *string
		}{
			{name: "binding", target: &binding.Binding},
			{name: "schema", target: &binding.Schema},
			{name: "table", target: &binding.Table},
		} {
			value, err := decodeString(object, member.name, entryPath+"."+member.name)
			if err != nil {
				return nil, err
			}
			*member.target = value
		}
		if binding.KeyColumns, err = decodeStringArray(object, "key_columns", entryPath+".key_columns"); err != nil {
			return nil, err
		}
		snapshot, err := decodeObject(
			object["snapshot"],
			entryPath+".snapshot",
			providerSQLSnapshotFields,
			providerSQLSnapshotFields,
		)
		if err != nil {
			return nil, err
		}
		for _, member := range []struct {
			name   string
			target *string
		}{
			{name: "provider_account_id", target: &binding.Snapshot.ProviderAccountID},
			{name: "permission_revision", target: &binding.Snapshot.PermissionRevision},
			{name: "resource", target: &binding.Snapshot.Resource},
			{name: "source_revision", target: &binding.Snapshot.SourceRevision},
		} {
			value, err := decodeString(snapshot, member.name, entryPath+".snapshot."+member.name)
			if err != nil {
				return nil, err
			}
			*member.target = value
		}
		bindings[index] = binding
	}
	return bindings, nil
}

// resolveProviderSQL validates one complete declaration. It performs no
// filesystem, secret or network access: an unusable endpoint, trust anchor or
// connection state directory fails process composition, never configuration
// parsing.
func resolveProviderSQL(configuration ProviderSQLConfiguration) (ProviderSQLConfiguration, error) {
	if !configuration.configured {
		return ProviderSQLConfiguration{}, nil
	}
	if err := validateProviderSQLBounds(configuration.bounds); err != nil {
		return ProviderSQLConfiguration{}, err
	}
	if len(configuration.connections) == 0 || len(configuration.connections) > maxProviderSQLConnections {
		return ProviderSQLConfiguration{}, diagnostic("provider_sql.connections", DiagnosticBounds)
	}
	policies := make(map[providerSQLConnectionKey]ProviderSQLConnection, len(configuration.connections))
	connections := make([]ProviderSQLConnection, 0, len(configuration.connections))
	// One state directory holds exactly one workspace-scoped connection's
	// schema state, so two declarations never share a schema snapshot store.
	directories := make(map[string]struct{}, len(configuration.connections))
	for index, connection := range configuration.connections {
		path := "provider_sql.connections[" + strconv.Itoa(index) + "]"
		if err := connection.scope.Validate(); err != nil {
			return ProviderSQLConfiguration{}, providerSQLScopeDiagnostic(err, path)
		}
		if !validProviderSQLIdentity(connection.ConnectionID, 512) {
			return ProviderSQLConfiguration{}, diagnostic(path+".connection_id", DiagnosticValue)
		}
		if !validProviderSQLIdentity(connection.ConnectorID, 512) {
			return ProviderSQLConfiguration{}, diagnostic(path+".connector_id", DiagnosticValue)
		}
		if err := validateProviderSQLHost(connection.Host, path+".host"); err != nil {
			return ProviderSQLConfiguration{}, err
		}
		if connection.Port == 0 {
			return ProviderSQLConfiguration{}, diagnostic(path+".port", DiagnosticValue)
		}
		if err := validateProviderSQLEndpointPart(connection.Database, path+".database"); err != nil {
			return ProviderSQLConfiguration{}, err
		}
		if err := validateProviderSQLEndpointPart(connection.User, path+".user"); err != nil {
			return ProviderSQLConfiguration{}, err
		}
		if err := validateSecretReference(connection.TLSTrustRef, path+".tls_trust_ref"); err != nil {
			return ProviderSQLConfiguration{}, err
		}
		if !validProviderSQLIdentity(connection.PasswordRef, 512) {
			return ProviderSQLConfiguration{}, diagnostic(path+".password_ref", DiagnosticValue)
		}
		if err := validateProviderSQLStateDirectory(connection.StateDirectory, path+".state_directory"); err != nil {
			return ProviderSQLConfiguration{}, err
		}
		if _, shared := directories[connection.StateDirectory]; shared {
			return ProviderSQLConfiguration{}, diagnostic(path+".state_directory", DiagnosticDuplicate)
		}
		directories[connection.StateDirectory] = struct{}{}
		if err := validateProviderSQLBindings(connection, path); err != nil {
			return ProviderSQLConfiguration{}, err
		}
		key := providerSQLConnectionKey{
			orgID: connection.scope.OrgID, workspaceID: connection.scope.WorkspaceID,
			connectionID: connection.ConnectionID,
		}
		if _, duplicate := policies[key]; duplicate {
			return ProviderSQLConfiguration{}, diagnostic(path+".connection_id", DiagnosticDuplicate)
		}
		policies[key] = cloneProviderSQLConnection(connection)
		connections = append(connections, cloneProviderSQLConnection(connection))
	}
	return ProviderSQLConfiguration{
		bounds:      configuration.bounds,
		connections: connections,
		policies:    policies,
		configured:  true,
	}, nil
}

func validateProviderSQLBounds(bounds ProviderSQLBounds) error {
	if bounds.MaxConnectDuration <= 0 || bounds.MaxStatementDuration <= 0 || bounds.MaxTotalDuration <= 0 {
		return diagnostic("provider_sql.max_total_duration", DiagnosticValue)
	}
	if bounds.MaxConnectDuration > bounds.MaxTotalDuration {
		return diagnostic("provider_sql.max_connect_duration", DiagnosticBounds)
	}
	if bounds.MaxStatementDuration > bounds.MaxTotalDuration {
		return diagnostic("provider_sql.max_statement_duration", DiagnosticBounds)
	}
	if bounds.MaxRows == 0 || bounds.MaxRows > providerSQLMaxRows {
		return diagnostic("provider_sql.max_rows", DiagnosticBounds)
	}
	if bounds.MaxBytes == 0 || bounds.MaxBytes > providerSQLMaxBytes {
		return diagnostic("provider_sql.max_bytes", DiagnosticBounds)
	}
	if bounds.MaxAttempts == 0 {
		return diagnostic("provider_sql.max_attempts", DiagnosticValue)
	}
	return nil
}

func validateProviderSQLBindings(connection ProviderSQLConnection, path string) error {
	if len(connection.Bindings) == 0 || len(connection.Bindings) > maxProviderSQLBindings {
		return diagnostic(path+".relation_bindings", DiagnosticBounds)
	}
	seen := make(map[string]struct{}, len(connection.Bindings))
	for index, binding := range connection.Bindings {
		bindingPath := path + ".relation_bindings[" + strconv.Itoa(index) + "]"
		if !validProviderSQLIdentity(binding.Binding, 1024) {
			return diagnostic(bindingPath+".binding", DiagnosticValue)
		}
		if _, duplicate := seen[binding.Binding]; duplicate {
			return diagnostic(bindingPath+".binding", DiagnosticDuplicate)
		}
		seen[binding.Binding] = struct{}{}
		if err := validateProviderSQLIdentifier(binding.Schema, bindingPath+".schema", maximumProviderSQLIdentifierBytes); err != nil {
			return err
		}
		if err := validateProviderSQLIdentifier(binding.Table, bindingPath+".table", maximumProviderSQLIdentifierBytes); err != nil {
			return err
		}
		if len(binding.KeyColumns) == 0 || len(binding.KeyColumns) > maxProviderSQLKeyColumns {
			return diagnostic(bindingPath+".key_columns", DiagnosticBounds)
		}
		keys := make(map[string]struct{}, len(binding.KeyColumns))
		for keyIndex, column := range binding.KeyColumns {
			keyPath := bindingPath + ".key_columns[" + strconv.Itoa(keyIndex) + "]"
			if err := validateProviderSQLIdentifier(column, keyPath, maximumProviderSQLIdentifierBytes); err != nil {
				return err
			}
			if _, duplicate := keys[column]; duplicate {
				return diagnostic(keyPath, DiagnosticDuplicate)
			}
			keys[column] = struct{}{}
		}
		for _, member := range []struct {
			name  string
			value string
		}{
			{name: "provider_account_id", value: binding.Snapshot.ProviderAccountID},
			{name: "permission_revision", value: binding.Snapshot.PermissionRevision},
			{name: "resource", value: binding.Snapshot.Resource},
			{name: "source_revision", value: binding.Snapshot.SourceRevision},
		} {
			if !validProviderSQLIdentity(member.value, 512) {
				return diagnostic(bindingPath+".snapshot."+member.name, DiagnosticValue)
			}
		}
	}
	return nil
}

// validateProviderSQLHost refuses anything but one plain declared host: no
// scheme, credentials, port, path or whitespace, so a declaration can never
// smuggle a DSN or a second endpoint into the connection facts.
func validateProviderSQLHost(value, field string) error {
	if value == "" || len(value) > 253 || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return diagnostic(field, DiagnosticValue)
	}
	if value[0] == '.' || value[0] == '-' || value[len(value)-1] == '.' || strings.Contains(value, "..") {
		return diagnostic(field, DiagnosticValue)
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-' || character == '.':
		default:
			return diagnostic(field, DiagnosticValue)
		}
	}
	return nil
}

// validateProviderSQLEndpointPart applies the same bounded plain-value grammar
// the transport applies to a declared database or user, so an unusable endpoint
// fails configuration instead of the first physical attempt.
func validateProviderSQLEndpointPart(value, field string) error {
	if value == "" || len(value) > maximumProviderSQLIdentifierBytes ||
		value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return diagnostic(field, DiagnosticValue)
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character <= 0x20 || character >= 0x7f {
			return diagnostic(field, DiagnosticValue)
		}
		switch character {
		case ',', '\'', '"', '\\', '/', '@', ':', '?', '#', '=', '[':
			return diagnostic(field, DiagnosticValue)
		}
	}
	return nil
}

func validateProviderSQLIdentifier(value, field string, maximum int) error {
	if value == "" || len(value) > maximum || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
		return diagnostic(field, DiagnosticValue)
	}
	return nil
}

func validateProviderSQLStateDirectory(value, field string) error {
	if value == "" || len(value) > maxProviderSQLStateDirectoryBytes ||
		value != strings.TrimSpace(value) || !utf8.ValidString(value) ||
		!filepath.IsAbs(value) || filepath.Clean(value) != value {
		return diagnostic(field, DiagnosticValue)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return diagnostic(field, DiagnosticValue)
		}
	}
	return nil
}

func providerSQLScopeDiagnostic(err error, path string) error {
	var contractErr *contract.ContractError
	if errors.As(err, &contractErr) {
		switch contractErr.Path {
		case "scope.org_id":
			return diagnostic(path+".org_id", DiagnosticValue)
		case "scope.workspace_id":
			return diagnostic(path+".workspace_id", DiagnosticValue)
		}
	}
	return diagnostic(path, DiagnosticValue)
}

// validProviderSQLIdentity is the bounded opaque identity grammar the declared
// connection, connector, credential and snapshot facts use.
func validProviderSQLIdentity(value string, maximum int) bool {
	if value == "" || len(value) > maximum || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}
