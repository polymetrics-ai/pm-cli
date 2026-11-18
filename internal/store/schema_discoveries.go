package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
)

// maxSchemaCandidateDocumentBytes is the stored snapshot document bound of
// ADR 0007 (4 MiB). The schema package applies the same bound before a
// candidate reaches the store.
const maxSchemaCandidateDocumentBytes = 4 << 20

// maxSchemaCandidateFields is the field bound of one stored snapshot.
const maxSchemaCandidateFields = 4096

// maxSchemaDiscoveryRelationNameBytes bounds the recorded schema and table
// names of a discovery: the PostgreSQL identifier length.
const maxSchemaDiscoveryRelationNameBytes = 63

var schemaReferencePattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// SchemaDiscovery is the immutable accepted intent of one discovery run: the
// connection, the resource and the declared relation binding that admission
// resolved, with its physical schema and table. The worker compares it with
// the current declaration before any provider I/O, so a declaration that
// changed after admission is refused.
type SchemaDiscovery struct {
	RunID              string
	ConnectionID       string
	ConnectorID        string
	Resource           string
	RelationBinding    string
	Schema             string
	Table              string
	ProviderAccountID  string
	PermissionRevision string
	SourceRevision     string
}

func (discovery SchemaDiscovery) validate() error {
	for _, value := range []string{
		discovery.RunID, discovery.ConnectionID, discovery.ConnectorID, discovery.Resource,
		discovery.RelationBinding, discovery.ProviderAccountID, discovery.PermissionRevision,
		discovery.SourceRevision,
	} {
		if !validStoreID(value) {
			return errors.New("schema discovery identity is invalid")
		}
	}
	// A PostgreSQL identifier has at most 63 bytes, as the declaration allows.
	for _, value := range []string{discovery.Schema, discovery.Table} {
		if !validStoreID(value) || len(value) > maxSchemaDiscoveryRelationNameBytes {
			return errors.New("schema discovery relation is invalid")
		}
	}
	return nil
}

// RecordSchemaDiscovery stores the discovery intent of one admitted run. It
// belongs to the admission transaction of that run. An exact replay of the
// same intent is accepted; a different intent for the same run is a conflict.
func RecordSchemaDiscovery(ctx context.Context, tx *Transaction, discovery SchemaDiscovery) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	if err := discovery.validate(); err != nil {
		return err
	}
	tag, err := tx.exec(ctx, `
		INSERT INTO pm.schema_discoveries (
			org_id, workspace_id, run_id, connection_id, connector_id, resource,
			relation_binding, relation_schema, relation_table, provider_account_id,
			permission_revision, source_revision
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT DO NOTHING
	`,
		scope.OrgID, scope.WorkspaceID, discovery.RunID, discovery.ConnectionID, discovery.ConnectorID,
		discovery.Resource, discovery.RelationBinding, discovery.Schema, discovery.Table,
		discovery.ProviderAccountID, discovery.PermissionRevision, discovery.SourceRevision,
	)
	if err != nil {
		return safeMetadataWriteError("record schema discovery", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	stored, err := LoadSchemaDiscovery(ctx, tx, discovery.RunID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrImmutableConflict
		}
		return err
	}
	if stored != discovery {
		return ErrImmutableConflict
	}
	return nil
}

// LoadSchemaDiscovery returns the discovery intent of one run. ErrNotFound
// means the run is not a discovery.
func LoadSchemaDiscovery(ctx context.Context, tx *Transaction, runID string) (SchemaDiscovery, error) {
	if _, err := tx.requireScope(); err != nil {
		return SchemaDiscovery{}, err
	}
	if !validStoreID(runID) {
		return SchemaDiscovery{}, errors.New("schema discovery run identity is invalid")
	}
	discovery := SchemaDiscovery{RunID: runID}
	if err := tx.queryRow(ctx, `
		SELECT connection_id, connector_id, resource, relation_binding, relation_schema, relation_table,
		       provider_account_id, permission_revision, source_revision
		FROM pm.schema_discoveries
		WHERE run_id = $1
	`, runID).Scan(
		&discovery.ConnectionID, &discovery.ConnectorID, &discovery.Resource, &discovery.RelationBinding,
		&discovery.Schema, &discovery.Table,
		&discovery.ProviderAccountID, &discovery.PermissionRevision, &discovery.SourceRevision,
	); errors.Is(err, pgx.ErrNoRows) {
		return SchemaDiscovery{}, ErrNotFound
	} else if err != nil {
		return SchemaDiscovery{}, fmt.Errorf("load schema discovery: %w", err)
	}
	if err := discovery.validate(); err != nil {
		return SchemaDiscovery{}, fmt.Errorf("validate stored schema discovery: %w", err)
	}
	return discovery, nil
}

// SchemaCandidate is one immutable complete candidate snapshot. Document is
// the snapshot document; SchemaRef is its content digest. The binding members
// repeat facts of the document so that a scoped read needs no decode.
type SchemaCandidate struct {
	ConnectionID       string
	SchemaRef          string
	ConnectorID        string
	ProviderAccountID  string
	PermissionRevision string
	Resource           string
	SourceRevision     string
	FieldCount         int
	Document           []byte
	DiscoveredByRunID  string
}

func (candidate SchemaCandidate) validate() error {
	for _, value := range []string{
		candidate.ConnectionID, candidate.ConnectorID, candidate.ProviderAccountID,
		candidate.PermissionRevision, candidate.Resource, candidate.SourceRevision, candidate.DiscoveredByRunID,
	} {
		if !validStoreID(value) {
			return errors.New("schema candidate identity is invalid")
		}
	}
	if !schemaReferencePattern.MatchString(candidate.SchemaRef) {
		return errors.New("schema candidate reference is not a content digest")
	}
	if candidate.FieldCount < 0 || candidate.FieldCount > maxSchemaCandidateFields {
		return errors.New("schema candidate field count is outside its bound")
	}
	if len(candidate.Document) == 0 || len(candidate.Document) > maxSchemaCandidateDocumentBytes ||
		!json.Valid(candidate.Document) {
		return errors.New("schema candidate document is outside its bound")
	}
	return nil
}

// StoreSchemaCandidate stores one complete candidate. It belongs to the
// settlement transaction of the discovery run that observed it. The key is the
// content digest within the connection, so a later discovery that observes the
// same content stores nothing new and reports inserted false.
func StoreSchemaCandidate(ctx context.Context, tx *Transaction, candidate SchemaCandidate) (bool, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return false, err
	}
	if err := candidate.validate(); err != nil {
		return false, err
	}
	tag, err := tx.exec(ctx, `
		INSERT INTO pm.schema_snapshots (
			org_id, workspace_id, connection_id, schema_ref, connector_id, provider_account_id,
			permission_revision, resource, source_revision, complete, field_count, document,
			discovered_by_run_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, true, $10, $11, $12)
		ON CONFLICT DO NOTHING
	`,
		scope.OrgID, scope.WorkspaceID, candidate.ConnectionID, candidate.SchemaRef, candidate.ConnectorID,
		candidate.ProviderAccountID, candidate.PermissionRevision, candidate.Resource, candidate.SourceRevision,
		candidate.FieldCount, json.RawMessage(candidate.Document), candidate.DiscoveredByRunID,
	)
	if err != nil {
		return false, safeMetadataWriteError("store schema candidate", err)
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	stored, err := LoadSchemaCandidate(ctx, tx, candidate.ConnectionID, candidate.SchemaRef)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, ErrImmutableConflict
		}
		return false, err
	}
	// The digest covers the whole binding and every field, so the same
	// reference with another binding is not the same content.
	if stored.ConnectorID != candidate.ConnectorID || stored.ProviderAccountID != candidate.ProviderAccountID ||
		stored.PermissionRevision != candidate.PermissionRevision || stored.Resource != candidate.Resource ||
		stored.SourceRevision != candidate.SourceRevision || stored.FieldCount != candidate.FieldCount {
		return false, ErrImmutableConflict
	}
	return false, nil
}

// LoadSchemaCandidate returns one stored candidate of the connection by its
// content digest.
func LoadSchemaCandidate(ctx context.Context, tx *Transaction, connectionID, schemaRef string) (SchemaCandidate, error) {
	if _, err := tx.requireScope(); err != nil {
		return SchemaCandidate{}, err
	}
	if !validStoreID(connectionID) || !schemaReferencePattern.MatchString(schemaRef) {
		return SchemaCandidate{}, errors.New("schema candidate selector is invalid")
	}
	candidate := SchemaCandidate{ConnectionID: connectionID, SchemaRef: schemaRef}
	if err := tx.queryRow(ctx, `
		SELECT connector_id, provider_account_id, permission_revision, resource, source_revision,
		       field_count, document, discovered_by_run_id
		FROM pm.schema_snapshots
		WHERE connection_id = $1 AND schema_ref = $2
	`, connectionID, schemaRef).Scan(
		&candidate.ConnectorID, &candidate.ProviderAccountID, &candidate.PermissionRevision,
		&candidate.Resource, &candidate.SourceRevision, &candidate.FieldCount, &candidate.Document,
		&candidate.DiscoveredByRunID,
	); errors.Is(err, pgx.ErrNoRows) {
		return SchemaCandidate{}, ErrNotFound
	} else if err != nil {
		return SchemaCandidate{}, fmt.Errorf("load schema candidate: %w", err)
	}
	if err := candidate.validate(); err != nil {
		return SchemaCandidate{}, fmt.Errorf("validate stored schema candidate: %w", err)
	}
	return candidate, nil
}
