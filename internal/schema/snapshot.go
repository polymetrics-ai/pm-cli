package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const (
	// SnapshotSchemaVersion is the document format revision of a stored
	// snapshot. It is a cache format version, never a schema revision: the
	// content fingerprint and the declared source revision carry schema identity.
	SnapshotSchemaVersion    = "pm.schema-snapshot/v1"
	MaxSnapshotDocumentBytes = 4 << 20
	MaxSnapshotFields        = 4096
	MaxSnapshotGaps          = 64
	maxSchemaIdentityBytes   = 512
	maxSchemaPointerBytes    = 2048

	snapshotDirectoryName        = "schema"
	snapshotPayloadDirectoryName = "snapshots"
	selectionFileName            = "config.json"
	selectionLockFileName        = "selection.lock"
	selectionMemberName          = "schema_ref"

	privateFileMode     = os.FileMode(0o600)
	snapshotPrivateMode = os.FileMode(0o700)
)

var (
	// ErrInvalidSnapshot reports a snapshot or candidate that cannot be trusted.
	ErrInvalidSnapshot = errors.New("invalid schema snapshot")
	// ErrInvalidDiscovery reports declared metadata that is not a discovery.
	ErrInvalidDiscovery = errors.New("invalid metadata discovery")
	// ErrIncompleteDiscovery reports a failed or partial discovery, which never
	// replaces the last complete valid snapshot.
	ErrIncompleteDiscovery = errors.New("incomplete metadata discovery")
	// ErrSnapshotScope reports metadata bound to another organization,
	// workspace, connection, account, permission revision, resource or source
	// revision.
	ErrSnapshotScope = errors.New("schema snapshot scope mismatch")
	// ErrSnapshotCorrupt reports stored bytes that do not match their reference.
	ErrSnapshotCorrupt = errors.New("schema snapshot content does not match its fingerprint")
	// ErrSnapshotNotFound reports that no stored payload exists for a declared
	// reference, so the caller's own selector named nothing this store holds and
	// can name a reachable one instead. A payload that is present but cannot be
	// read stays ErrSnapshotUnavailable, which is never the caller's to correct.
	ErrSnapshotNotFound = errors.New("schema snapshot is not found")
	// ErrSnapshotUnavailable reports a snapshot or state directory that cannot be
	// made available under its reference, including a payload that is present but
	// unreadable.
	ErrSnapshotUnavailable = errors.New("schema snapshot is unavailable")
	// ErrSelectionConflict reports a connection selection that changed since it
	// was read, so the caller's expectation no longer holds.
	ErrSelectionConflict = errors.New("schema selection conflict")
	// ErrSelectionUnavailable reports a connection config that cannot be read as
	// the sole schema_ref selector.
	ErrSelectionUnavailable = errors.New("schema selection is unavailable")
	// ErrSchemaDrift reports field constraints that changed since a saved plan or
	// approval observed them.
	ErrSchemaDrift = errors.New("schema constraints drifted from the saved witness")
	// ErrInvalidPredicate reports a predicate declaration that is not typed.
	ErrInvalidPredicate = errors.New("invalid predicate declaration")
	// ErrEvaluatorUnavailable reports a missing declared expression evaluator.
	ErrEvaluatorUnavailable = errors.New("predicate expression evaluator is unavailable")
)

// Binding scopes one snapshot to the exact organization, workspace, connection,
// provider account, permission revision, resource and source revision it was
// discovered for. Connection identities are workspace-scoped, so the owning
// organization and workspace are part of the binding: two workspaces that
// declare the same connection identity, and two accounts that expose the same
// resource name, therefore never share metadata.
type Binding struct {
	OrgID              string `json:"org_id"`
	WorkspaceID        string `json:"workspace_id"`
	ConnectorID        string `json:"connector_id"`
	ConnectionID       string `json:"connection_id"`
	ProviderAccountID  string `json:"provider_account_id"`
	PermissionRevision string `json:"permission_revision"`
	Resource           string `json:"resource"`
	SourceRevision     string `json:"source_revision"`
}

func (binding Binding) Validate() error {
	for _, member := range []struct {
		path  string
		value string
	}{
		{"org_id", binding.OrgID},
		{"workspace_id", binding.WorkspaceID},
		{"connector_id", binding.ConnectorID},
		{"connection_id", binding.ConnectionID},
		{"provider_account_id", binding.ProviderAccountID},
		{"permission_revision", binding.PermissionRevision},
		{"resource", binding.Resource},
		{"source_revision", binding.SourceRevision},
	} {
		if !validBoundedIdentity(member.value) {
			return errorf(ErrSnapshotScope, "binding."+member.path, "must be a canonical bounded identity")
		}
	}
	return nil
}

// Field is one normalized declared metadata field. Name retains the exact
// provider identifier spelling; Pointer records the declared metadata location
// it was normalized from. Requiredness, writability, logical type, enums,
// association, precision and scale survive normalization unchanged.
type Field struct {
	Name        string          `json:"name"`
	Pointer     string          `json:"pointer"`
	Schema      contract.Schema `json:"schema"`
	Required    bool            `json:"required"`
	Writable    bool            `json:"writable"`
	Association string          `json:"association,omitempty"`
	Precision   int             `json:"precision,omitempty"`
	Scale       int             `json:"scale,omitempty"`
}

func (field Field) validateDeclaration() error {
	if !validBoundedIdentity(field.Name) {
		return errorf(ErrInvalidSnapshot, "field.name", "must retain a canonical bounded provider identifier")
	}
	if field.Pointer == "" || len(field.Pointer) > maxSchemaPointerBytes || !utf8.ValidString(field.Pointer) {
		return errorf(ErrInvalidSnapshot, "field.pointer", "must record the declared metadata location")
	}
	return nil
}

func (field Field) Validate() error {
	if err := field.validateDeclaration(); err != nil {
		return err
	}
	if err := field.Schema.Validate(); err != nil {
		return errorf(ErrInvalidSnapshot, "field."+field.Name+".schema", "does not declare a supported typed schema")
	}
	switch field.Schema.Type {
	case contract.SchemaTypeUnion, contract.SchemaTypeBinaryRef:
		return errorf(ErrInvalidSnapshot, "field."+field.Name+".schema", "declares a shape dynamic metadata cannot carry")
	}
	if field.Association != "" && !validBoundedIdentity(field.Association) {
		return errorf(ErrInvalidSnapshot, "field."+field.Name+".association", "must name a declared associated object")
	}
	if field.Precision < 0 || field.Scale < 0 || field.Scale > field.Precision {
		return errorf(ErrInvalidSnapshot, "field."+field.Name+".precision", "must declare scale within precision")
	}
	if field.Precision > 0 && field.Schema.Type != contract.SchemaTypeInteger && field.Schema.Type != contract.SchemaTypeDecimal {
		return errorf(ErrInvalidSnapshot, "field."+field.Name+".precision", "is valid only for numeric logical types")
	}
	if field.Scale > 0 && field.Schema.Type != contract.SchemaTypeDecimal {
		return errorf(ErrInvalidSnapshot, "field."+field.Name+".scale", "is valid only for a decimal logical type")
	}
	return nil
}

// Fingerprint is the constraint fingerprint of one field. Provenance moves do
// not change it; a relevant enum, type, requiredness, writability, association
// or precision change does.
func (field Field) Fingerprint() (contract.SemanticDigest, error) {
	document, err := json.Marshal(struct {
		Name        string          `json:"name"`
		Schema      contract.Schema `json:"schema"`
		Required    bool            `json:"required"`
		Writable    bool            `json:"writable"`
		Association string          `json:"association,omitempty"`
		Precision   int             `json:"precision,omitempty"`
		Scale       int             `json:"scale,omitempty"`
	}{
		Name: field.Name, Schema: field.Schema, Required: field.Required, Writable: field.Writable,
		Association: field.Association, Precision: field.Precision, Scale: field.Scale,
	})
	if err != nil {
		return "", errorf(ErrInvalidSnapshot, "field."+field.Name, "cannot be fingerprinted")
	}
	return contract.CanonicalDigest(document)
}

// Candidate is a discovered but not yet durable snapshot scope. Its
// completeness is always explicit, so a caller cannot promote a partial
// discovery by accident.
type Candidate struct {
	Binding      Binding      `json:"binding"`
	Fields       []Field      `json:"fields"`
	Completeness Completeness `json:"completeness"`
}

func (candidate Candidate) Validate() error {
	if err := candidate.Binding.Validate(); err != nil {
		return err
	}
	if candidate.Fields == nil {
		return errorf(ErrInvalidSnapshot, "fields", "must be declared explicitly, even when empty")
	}
	if len(candidate.Fields) > MaxSnapshotFields {
		return errorf(ErrInvalidSnapshot, "fields", "exceeds the supported field count")
	}
	seen := make(map[string]struct{}, len(candidate.Fields))
	for _, field := range candidate.Fields {
		if err := field.Validate(); err != nil {
			return err
		}
		if _, duplicate := seen[field.Name]; duplicate {
			return errorf(ErrInvalidSnapshot, "fields", "declares the same field more than once")
		}
		seen[field.Name] = struct{}{}
	}
	return candidate.Completeness.Validate()
}

// Fingerprint addresses the discovered content. The document format revision is
// deliberately not part of it: a cache format change is not a schema revision.
func (candidate Candidate) Fingerprint() (contract.SemanticDigest, error) {
	document, err := json.Marshal(struct {
		Binding      Binding      `json:"binding"`
		Fields       []Field      `json:"fields"`
		Completeness Completeness `json:"completeness"`
	}{Binding: candidate.Binding, Fields: candidate.Fields, Completeness: candidate.Completeness})
	if err != nil {
		return "", errorf(ErrInvalidSnapshot, "binding", "cannot be fingerprinted")
	}
	return contract.CanonicalDigest(document)
}

// Snapshot is the durable, immutable document of one binding.
type Snapshot struct {
	SchemaVersion string                  `json:"schema_version"`
	Binding       Binding                 `json:"binding"`
	Fingerprint   contract.SemanticDigest `json:"fingerprint"`
	Completeness  Completeness            `json:"completeness"`
	Fields        []Field                 `json:"fields"`
}

// Candidate projects the durable document of this content.
func (candidate Candidate) Snapshot() (Snapshot, error) {
	if err := candidate.Validate(); err != nil {
		return Snapshot{}, err
	}
	fingerprint, err := candidate.Fingerprint()
	if err != nil {
		return Snapshot{}, err
	}
	fields := candidate.Fields
	if fields == nil {
		fields = []Field{}
	}
	gaps := candidate.Completeness.Gaps
	if gaps == nil {
		gaps = []Gap{}
	}
	return Snapshot{
		SchemaVersion: SnapshotSchemaVersion,
		Binding:       candidate.Binding,
		Fingerprint:   fingerprint,
		Completeness:  Completeness{Complete: candidate.Completeness.Complete, Gaps: gaps},
		Fields:        fields,
	}, nil
}

// Candidate projects the discovered content of this durable document.
func (snapshot Snapshot) Candidate() Candidate {
	return Candidate{
		Binding:      snapshot.Binding,
		Fields:       slices.Clone(snapshot.Fields),
		Completeness: Completeness{Complete: snapshot.Completeness.Complete, Gaps: slices.Clone(snapshot.Completeness.Gaps)},
	}
}

// ContentFingerprint recomputes the content address of this document.
func (snapshot Snapshot) ContentFingerprint() (contract.SemanticDigest, error) {
	return snapshot.Candidate().Fingerprint()
}

func (snapshot Snapshot) Validate() error {
	if snapshot.SchemaVersion != SnapshotSchemaVersion {
		return errorf(ErrInvalidSnapshot, "schema_version", "document revision is unsupported")
	}
	if err := snapshot.Candidate().Validate(); err != nil {
		return err
	}
	if err := snapshot.Fingerprint.Validate(); err != nil {
		return errorf(ErrInvalidSnapshot, "fingerprint", "must be a declared content digest")
	}
	content, err := snapshot.ContentFingerprint()
	if err != nil {
		return err
	}
	if content != snapshot.Fingerprint {
		return errorf(ErrSnapshotCorrupt, "fingerprint", "does not match the declared content")
	}
	return nil
}

// DecodeSnapshot decodes one closed snapshot document through the shared
// contract duplicate, unknown-field, presence and trailing-data checks.
func DecodeSnapshot(document []byte) (Snapshot, error) {
	if len(document) == 0 || len(document) > MaxSnapshotDocumentBytes {
		return Snapshot{}, errorf(ErrInvalidSnapshot, "$", "document size is outside the supported bound")
	}
	var snapshot Snapshot
	if err := contract.DecodeStrictRequiredObject(document, &snapshot,
		"schema_version", "binding", "fingerprint", "completeness", "fields"); err != nil {
		return Snapshot{}, errorf(ErrInvalidSnapshot, "$", "document is not a typed snapshot")
	}
	if err := snapshot.Validate(); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

// Store owns the snapshot state of exactly one connection. Its directory is the
// connection's owned state directory: snapshots are immutable payloads below
// it, and the connection's config.json is the sole schema_ref selection.
//
// A scoped store also records the organization, workspace and connection that
// own its directory, and refuses every binding of another owner before it reads
// or writes anything, so a caller authorized for one workspace never reads or
// replaces another workspace's metadata through a shared connection identity.
type Store struct {
	directory string
	owner     *storeOwner
}

// storeOwner is the exact workspace-scoped connection one scoped store serves.
type storeOwner struct {
	scope        contract.Scope
	connectionID string
}

// NewStore binds a store to an existing private connection state directory.
func NewStore(connectionStateDirectory string) (*Store, error) {
	if connectionStateDirectory == "" || !filepath.IsAbs(connectionStateDirectory) {
		return nil, errorf(ErrSnapshotUnavailable, "connection_state_directory", "must be an absolute path")
	}
	directory := filepath.Clean(connectionStateDirectory)
	information, err := os.Lstat(directory)
	if err != nil || information.Mode()&os.ModeSymlink != 0 || !information.IsDir() || information.Mode().Perm()&0o077 != 0 {
		return nil, errorf(ErrSnapshotUnavailable, "connection_state_directory", "must be an existing private directory")
	}
	return &Store{directory: directory}, nil
}

// NewScopedStore binds a store to one existing private connection state
// directory owned by exactly one organization, workspace and connection. Every
// later read or write must name that owner in its binding.
func NewScopedStore(connectionStateDirectory string, scope contract.Scope, connectionID string) (*Store, error) {
	owner, err := newStoreOwner(scope, connectionID)
	if err != nil {
		return nil, err
	}
	store, err := NewStore(connectionStateDirectory)
	if err != nil {
		return nil, err
	}
	store.owner = owner
	return store, nil
}

// ConnectionStateDirectory names the owned state directory of one
// workspace-scoped connection beneath a server-owned metadata root:
// root/<org>/<workspace>/<connection>. Each component must be one plain path
// segment, so no identity can traverse into another owner's state.
func ConnectionStateDirectory(root string, scope contract.Scope, connectionID string) (string, error) {
	if root == "" || !filepath.IsAbs(root) {
		return "", errorf(ErrSnapshotUnavailable, "metadata_root", "must be an absolute path")
	}
	if _, err := newStoreOwner(scope, connectionID); err != nil {
		return "", err
	}
	return filepath.Join(filepath.Clean(root), scope.OrgID, scope.WorkspaceID, connectionID), nil
}

// NewConnectionStore opens the scoped store of one workspace-scoped connection
// beneath a server-owned metadata root. The directory must already exist as a
// private directory; this never creates or defaults a location.
func NewConnectionStore(root string, scope contract.Scope, connectionID string) (*Store, error) {
	directory, err := ConnectionStateDirectory(root, scope, connectionID)
	if err != nil {
		return nil, err
	}
	return NewScopedStore(directory, scope, connectionID)
}

func newStoreOwner(scope contract.Scope, connectionID string) (*storeOwner, error) {
	for _, member := range []struct {
		path  string
		value string
	}{
		{"org_id", scope.OrgID},
		{"workspace_id", scope.WorkspaceID},
		{"connection_id", connectionID},
	} {
		if !validPathSegment(member.value) {
			return nil, errorf(ErrSnapshotScope, member.path, "must be one plain bounded path segment")
		}
	}
	return &storeOwner{scope: scope, connectionID: connectionID}, nil
}

// authorize refuses a binding that belongs to another owner of a scoped store.
func (store *Store) authorize(binding Binding) error {
	if err := binding.Validate(); err != nil {
		return err
	}
	if store.owner == nil {
		return nil
	}
	if binding.OrgID != store.owner.scope.OrgID || binding.WorkspaceID != store.owner.scope.WorkspaceID ||
		binding.ConnectionID != store.owner.connectionID {
		return errorf(ErrSnapshotScope, "binding", "the binding belongs to another organization, workspace or connection than this store")
	}
	return nil
}

func validPathSegment(value string) bool {
	return validBoundedIdentity(value) && value != "." && value != ".." &&
		!strings.ContainsAny(value, `/\`)
}

func snapshotFileName(reference contract.SemanticDigest) string {
	return strings.TrimPrefix(string(reference), "sha256:") + ".json"
}

func (store *Store) payloadPath(reference contract.SemanticDigest) (string, error) {
	if err := reference.Validate(); err != nil {
		return "", errorf(ErrSnapshotUnavailable, "reference", "must be a declared content digest")
	}
	return filepath.Join(store.directory, snapshotDirectoryName, snapshotPayloadDirectoryName, snapshotFileName(reference)), nil
}

func (store *Store) selectionPath() string {
	return filepath.Join(store.directory, selectionFileName)
}

// readConfigDocument reads the connection config that owns the selection. Every
// other member of that document belongs to the connection and is preserved.
func (store *Store) readConfigDocument() ([]byte, bool, error) {
	path := store.selectionPath()
	information, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errorf(ErrSelectionUnavailable, selectionFileName, "is unavailable")
	}
	if information.Mode()&os.ModeSymlink != 0 || !information.Mode().IsRegular() ||
		information.Mode().Perm()&0o077 != 0 || information.Size() <= 0 || information.Size() > MaxSnapshotDocumentBytes {
		return nil, false, errorf(ErrSelectionUnavailable, selectionFileName, "must be a private bounded regular file")
	}
	document, err := os.ReadFile(path)
	if err != nil {
		return nil, false, errorf(ErrSelectionUnavailable, selectionFileName, "could not be read")
	}
	return document, true, nil
}

// Selection reports the single schema reference the connection selects.
func (store *Store) Selection() (contract.SemanticDigest, bool, error) {
	document, present, err := store.readConfigDocument()
	if err != nil || !present {
		return "", false, err
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(document, &members); err != nil {
		return "", false, errorf(ErrSelectionUnavailable, selectionFileName, "is not a declared JSON object")
	}
	raw, declared := members[selectionMemberName]
	if !declared {
		return "", false, nil
	}
	var selected string
	if err := json.Unmarshal(raw, &selected); err != nil {
		return "", false, errorf(ErrSelectionUnavailable, selectionMemberName, "must be a declared reference string")
	}
	reference := contract.SemanticDigest(selected)
	if err := reference.Validate(); err != nil {
		return "", false, errorf(ErrSelectionUnavailable, selectionMemberName, "must be a declared content digest")
	}
	return reference, true, nil
}

// writeSelection replaces only the schema_ref member of the connection config.
func (store *Store) writeSelection(reference contract.SemanticDigest) error {
	members := map[string]json.RawMessage{}
	if document, present, err := store.readConfigDocument(); err != nil {
		return err
	} else if present {
		if err := json.Unmarshal(document, &members); err != nil {
			return errorf(ErrSelectionUnavailable, selectionFileName, "is not a declared JSON object")
		}
	}
	encoded, err := json.Marshal(string(reference))
	if err != nil {
		return errorf(ErrSelectionUnavailable, selectionMemberName, "could not be encoded")
	}
	members[selectionMemberName] = encoded
	document, err := json.MarshalIndent(members, "", "  ")
	if err != nil {
		return errorf(ErrSelectionUnavailable, selectionFileName, "could not be encoded")
	}
	document = append(document, '\n')
	temporary, err := os.CreateTemp(store.directory, ".config-*")
	if err != nil {
		return errorf(ErrSelectionUnavailable, selectionFileName, "temporary file is unavailable")
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(privateFileMode); err != nil {
		temporary.Close()
		return errorf(ErrSelectionUnavailable, selectionFileName, "permissions could not be set")
	}
	if _, err := temporary.Write(document); err != nil {
		temporary.Close()
		return errorf(ErrSelectionUnavailable, selectionFileName, "could not be written")
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return errorf(ErrSelectionUnavailable, selectionFileName, "could not be synchronized")
	}
	if err := temporary.Close(); err != nil {
		return errorf(ErrSelectionUnavailable, selectionFileName, "could not be closed")
	}
	if err := os.Rename(temporaryName, store.selectionPath()); err != nil {
		return errorf(ErrSelectionUnavailable, selectionFileName, "could not be committed")
	}
	return nil
}

// writePayload stores one immutable payload. Content addressing makes an exact
// replay idempotent and refuses different bytes for the same reference.
func (store *Store) writePayload(snapshot Snapshot) error {
	document, err := json.Marshal(snapshot)
	if err != nil {
		return errorf(ErrInvalidSnapshot, "fields", "could not be encoded")
	}
	if len(document) > MaxSnapshotDocumentBytes {
		return errorf(ErrInvalidSnapshot, "$", "document exceeds the supported bound")
	}
	path, err := store.payloadPath(snapshot.Fingerprint)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, snapshotPrivateMode); err != nil {
		return errorf(ErrSnapshotUnavailable, "payload directory", "is unavailable")
	}
	information, err := os.Lstat(directory)
	if err != nil || information.Mode()&os.ModeSymlink != 0 || !information.IsDir() || information.Mode().Perm()&0o077 != 0 {
		return errorf(ErrSnapshotUnavailable, "payload directory", "must be a private directory")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, privateFileMode)
	if errors.Is(err, os.ErrExist) {
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return errorf(ErrSnapshotUnavailable, "payload", "is unavailable")
		}
		if !bytes.Equal(existing, document) {
			return errorf(ErrSnapshotCorrupt, "payload", "an immutable snapshot already exists for this reference")
		}
		return nil
	}
	if err != nil {
		return errorf(ErrSnapshotUnavailable, "payload", "could not be created")
	}
	defer file.Close()
	if _, err := file.Write(document); err != nil {
		return errorf(ErrSnapshotUnavailable, "payload", "could not be written")
	}
	if err := file.Sync(); err != nil {
		return errorf(ErrSnapshotUnavailable, "payload", "could not be synchronized")
	}
	return nil
}

func (store *Store) load(reference contract.SemanticDigest) (Snapshot, error) {
	path, err := store.payloadPath(reference)
	if err != nil {
		return Snapshot{}, err
	}
	information, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, errorf(ErrSnapshotNotFound, "reference", "no snapshot payload is stored for this reference")
	}
	if err != nil || information.Mode()&os.ModeSymlink != 0 || !information.Mode().IsRegular() ||
		information.Mode().Perm()&0o077 != 0 || information.Size() <= 0 || information.Size() > MaxSnapshotDocumentBytes {
		return Snapshot{}, errorf(ErrSnapshotCorrupt, "payload", "must be a private bounded regular file")
	}
	document, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, errorf(ErrSnapshotUnavailable, "payload", "could not be read")
	}
	snapshot, err := DecodeSnapshot(document)
	if err != nil {
		return Snapshot{}, errorf(ErrSnapshotCorrupt, "payload", "does not decode for this reference")
	}
	if snapshot.Fingerprint != reference {
		return Snapshot{}, errorf(ErrSnapshotCorrupt, "payload", "does not match the requested reference")
	}
	return snapshot, nil
}

// LoadFor loads one snapshot and requires that it belongs to the caller's
// binding, so metadata of another account, connection, permission revision,
// resource or source revision is never served.
func (store *Store) LoadFor(binding Binding, reference contract.SemanticDigest) (Snapshot, error) {
	if err := store.authorize(binding); err != nil {
		return Snapshot{}, err
	}
	snapshot, err := store.load(reference)
	if err != nil {
		return Snapshot{}, err
	}
	if snapshot.Binding != binding {
		return Snapshot{}, errorf(ErrSnapshotScope, "binding", "the stored snapshot belongs to another organization, workspace, connection, account, permission revision, resource or source revision")
	}
	return snapshot, nil
}

// Active reports the snapshot the connection config currently selects.
func (store *Store) Active(binding Binding) (Snapshot, bool, error) {
	if err := store.authorize(binding); err != nil {
		return Snapshot{}, false, err
	}
	selected, present, err := store.Selection()
	if err != nil || !present {
		return Snapshot{}, false, err
	}
	snapshot, err := store.LoadFor(binding, selected)
	if err != nil {
		return Snapshot{}, false, err
	}
	if !snapshot.Completeness.Complete {
		return Snapshot{}, false, errorf(ErrIncompleteDiscovery, "completeness", "the selected snapshot is not a complete discovery")
	}
	return snapshot, true, nil
}

// Promote durably binds a complete discovery and makes it the connection's
// selection. expected is the reference the caller read as selected, or the
// empty reference when it read no selection.
func (store *Store) Promote(binding Binding, candidate Candidate, expected contract.SemanticDigest) (Snapshot, error) {
	if err := store.authorize(binding); err != nil {
		return Snapshot{}, err
	}
	if candidate.Binding != binding {
		return Snapshot{}, errorf(ErrSnapshotScope, "binding", "the candidate belongs to another organization, workspace, connection, account, permission revision, resource or source revision")
	}
	snapshot, err := candidate.Snapshot()
	if err != nil {
		return Snapshot{}, err
	}
	if expected != "" {
		if err := expected.Validate(); err != nil {
			return Snapshot{}, errorf(ErrSelectionConflict, "expected", "must be a declared content digest")
		}
	}
	// The expected-selection comparison and the replacement are one critical
	// section across every store and process bound to this connection state, so
	// competing promotions of the same expected selection admit exactly one.
	unlock, err := store.lockSelection()
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	selected, present, err := store.Selection()
	if err != nil {
		return Snapshot{}, err
	}
	if present != (expected != "") || (present && selected != expected) {
		return Snapshot{}, errorf(ErrSelectionConflict, selectionMemberName, "the connection selection changed since it was read")
	}
	if !snapshot.Completeness.Complete {
		return Snapshot{}, errorf(ErrIncompleteDiscovery, "completeness", "a failed or partial discovery never replaces the last complete valid snapshot")
	}
	if err := store.writePayload(snapshot); err != nil {
		return Snapshot{}, err
	}
	if err := store.writeSelection(snapshot.Fingerprint); err != nil {
		return Snapshot{}, err
	}
	return snapshot, nil
}

// lockSelection takes the connection's exclusive selection lock. The lock is an
// advisory lock on a private regular file below the snapshot directory, so it
// excludes every other open of the same connection state, in this process or
// another, until the returned release runs.
func (store *Store) lockSelection() (func(), error) {
	directory := filepath.Join(store.directory, snapshotDirectoryName)
	if err := os.MkdirAll(directory, snapshotPrivateMode); err != nil {
		return nil, errorf(ErrSelectionUnavailable, "selection lock", "directory is unavailable")
	}
	information, err := os.Lstat(directory)
	if err != nil || information.Mode()&os.ModeSymlink != 0 || !information.IsDir() || information.Mode().Perm()&0o077 != 0 {
		return nil, errorf(ErrSelectionUnavailable, "selection lock", "directory must be private")
	}
	lock, err := os.OpenFile(
		filepath.Join(directory, selectionLockFileName),
		os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, privateFileMode,
	)
	if err != nil {
		return nil, errorf(ErrSelectionUnavailable, "selection lock", "could not be opened")
	}
	held, err := lock.Stat()
	if err != nil || !held.Mode().IsRegular() || held.Mode().Perm()&0o077 != 0 {
		lock.Close()
		return nil, errorf(ErrSelectionUnavailable, "selection lock", "must be a private regular file")
	}
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		lock.Close()
		return nil, errorf(ErrSelectionUnavailable, "selection lock", "could not be acquired")
	}
	return func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}, nil
}

func validBoundedIdentity(value string) bool {
	if value == "" || len(value) > maxSchemaIdentityBytes || !utf8.ValidString(value) {
		return false
	}
	if strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}
