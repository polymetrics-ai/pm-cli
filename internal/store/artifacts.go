package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type ObjectVerification struct {
	ObservedSHA256            string
	ObservedByteLength        int64
	ObservedBackendGeneration string
}

// PreverifiedObjectRef is opaque outside this package. Construction proves
// descriptor equality only; upload and readback remain with the object owner.
type PreverifiedObjectRef struct {
	ref contract.ObjectRef
}

func NewPreverifiedObjectRef(ref contract.ObjectRef, verification ObjectVerification) (PreverifiedObjectRef, error) {
	if err := ref.Validate(); err != nil {
		return PreverifiedObjectRef{}, fmt.Errorf("validate object reference: %w", err)
	}
	if verification.ObservedSHA256 != ref.SHA256 ||
		verification.ObservedByteLength != ref.ByteLength ||
		verification.ObservedBackendGeneration != ref.BackendGeneration {
		return PreverifiedObjectRef{}, ErrObjectNotVerified
	}
	return PreverifiedObjectRef{ref: ref}, nil
}

type ManifestCAS struct {
	ConnectionID     string
	ResourceID       string
	ExpectedRevision uint64
	Object           PreverifiedObjectRef
}

type Manifest struct {
	Scope       contract.Scope
	ResourceID  string
	Revision    uint64
	Object      contract.ObjectRef
	CommittedAt time.Time
}

// ArtifactPurpose is the immutable visibility boundary of one registered object.
// Public artifacts may be projected to clients; continuation artifacts remain
// server-only and require receipt-bound producer lineage.
type ArtifactPurpose string

const (
	ArtifactPurposePublic       ArtifactPurpose = "public"
	ArtifactPurposeContinuation ArtifactPurpose = "continuation"
)

func (purpose ArtifactPurpose) valid() bool {
	return purpose == ArtifactPurposePublic || purpose == ArtifactPurposeContinuation
}

type artifactRecord struct {
	ref          contract.ObjectRef
	connectionID sql.NullString
	purpose      ArtifactPurpose
}

// RegisterArtifact registers an object for public artifact access.
func RegisterArtifact(
	ctx context.Context,
	tx *Transaction,
	connectionID string,
	object PreverifiedObjectRef,
) error {
	return registerArtifact(ctx, tx, connectionID, object, ArtifactPurposePublic)
}

// RegisterContinuationArtifact registers a server-only continuation object.
func RegisterContinuationArtifact(
	ctx context.Context,
	tx *Transaction,
	connectionID string,
	object PreverifiedObjectRef,
) error {
	return registerArtifact(ctx, tx, connectionID, object, ArtifactPurposeContinuation)
}

func registerArtifact(
	ctx context.Context,
	tx *Transaction,
	connectionID string,
	object PreverifiedObjectRef,
	purpose ArtifactPurpose,
) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	if !purpose.valid() {
		return errors.New("artifact purpose is invalid")
	}
	if !validStoreID(connectionID) {
		return errors.New("artifact connection identity is invalid")
	}
	if _, err := LoadConnection(ctx, tx, connectionID); err != nil {
		return err
	}
	ref := object.ref
	if err := ref.Validate(); err != nil {
		return ErrObjectNotVerified
	}
	if ref.OrgID != scope.OrgID || ref.WorkspaceID != scope.WorkspaceID {
		return ErrScopeMismatch
	}
	encoded, err := json.Marshal(ref)
	if err != nil {
		return fmt.Errorf("encode artifact reference: %w", err)
	}
	registeredAt, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return err
	}
	tag, err := tx.exec(ctx, `
		INSERT INTO pm.artifacts (
			org_id, workspace_id, artifact_id, object_attempt_key, sha256,
			byte_length, media_type, backend_generation, connection_id,
			object_ref, registered_at, purpose
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (org_id, workspace_id, artifact_id) DO NOTHING
	`,
		ref.OrgID,
		ref.WorkspaceID,
		ref.ArtifactID,
		ref.ObjectAttemptKey,
		ref.SHA256,
		ref.ByteLength,
		ref.MediaType,
		ref.BackendGeneration,
		connectionID,
		json.RawMessage(encoded),
		registeredAt,
		string(purpose),
	)
	if err != nil {
		return fmt.Errorf("register artifact reference: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	stored, err := loadArtifactRecord(ctx, tx, ref.ArtifactID)
	if err != nil {
		return err
	}
	if stored.ref != ref || !stored.connectionID.Valid || stored.connectionID.String != connectionID ||
		stored.purpose != purpose {
		return ErrImmutableConflict
	}
	return nil
}

// LoadArtifact loads only an object registered for public artifact access.
func LoadArtifact(ctx context.Context, tx *Transaction, artifactID string) (contract.ObjectRef, error) {
	return loadArtifactByPurpose(ctx, tx, artifactID, ArtifactPurposePublic)
}

// LoadContinuationArtifact loads only a server-only continuation object.
func LoadContinuationArtifact(ctx context.Context, tx *Transaction, artifactID string) (contract.ObjectRef, error) {
	return loadArtifactByPurpose(ctx, tx, artifactID, ArtifactPurposeContinuation)
}

func loadArtifactByPurpose(
	ctx context.Context,
	tx *Transaction,
	artifactID string,
	purpose ArtifactPurpose,
) (contract.ObjectRef, error) {
	stored, err := loadArtifactRecordMatchingPurpose(ctx, tx, artifactID, purpose)
	if err != nil {
		return contract.ObjectRef{}, err
	}
	if stored.purpose != purpose {
		return contract.ObjectRef{}, ErrNotFound
	}
	return stored.ref, nil
}

// LoadArtifactConnectionID returns the immutable trusted connection binding
// used by public artifact authorization. Retained unbound rows fail closed.
func LoadArtifactConnectionID(ctx context.Context, tx *Transaction, artifactID string) (string, error) {
	if _, err := tx.requireScope(); err != nil {
		return "", err
	}
	if !validStoreID(artifactID) {
		return "", ErrNotFound
	}
	var connectionID sql.NullString
	if err := tx.queryRow(
		ctx,
		`SELECT connection_id FROM pm.artifacts WHERE artifact_id = $1 AND purpose = $2`,
		artifactID,
		string(ArtifactPurposePublic),
	).Scan(&connectionID); errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	} else if err != nil {
		return "", fmt.Errorf("load artifact connection authority: %w", err)
	}
	if !connectionID.Valid || !validStoreID(connectionID.String) {
		return "", ErrNotFound
	}
	return connectionID.String, nil
}

// loadArtifactRecord is purpose-agnostic for internal publication receipt
// validation. Exported object reads must use the purpose-filtered helper.
func loadArtifactRecord(ctx context.Context, tx *Transaction, artifactID string) (artifactRecord, error) {
	return loadArtifactRecordMatchingPurpose(ctx, tx, artifactID, "")
}

func loadArtifactRecordMatchingPurpose(
	ctx context.Context,
	tx *Transaction,
	artifactID string,
	expectedPurpose ArtifactPurpose,
) (artifactRecord, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return artifactRecord{}, err
	}
	if !validStoreID(artifactID) {
		return artifactRecord{}, errors.New("artifact identity is invalid")
	}
	var row pgx.Row
	if expectedPurpose == "" {
		row = tx.queryRow(
			ctx,
			`SELECT object_ref, connection_id, purpose FROM pm.artifacts WHERE artifact_id = $1`,
			artifactID,
		)
	} else {
		row = tx.queryRow(
			ctx,
			`SELECT object_ref, connection_id, purpose
			 FROM pm.artifacts
			 WHERE artifact_id = $1 AND purpose = $2`,
			artifactID,
			string(expectedPurpose),
		)
	}
	var stored artifactRecord
	var encoded []byte
	var purpose string
	if err := row.Scan(&encoded, &stored.connectionID, &purpose); errors.Is(err, pgx.ErrNoRows) {
		return artifactRecord{}, ErrNotFound
	} else if err != nil {
		return artifactRecord{}, fmt.Errorf("load artifact reference: %w", err)
	}
	stored.purpose = ArtifactPurpose(purpose)
	if !stored.purpose.valid() {
		return artifactRecord{}, ErrImmutableConflict
	}
	if err := json.Unmarshal(encoded, &stored.ref); err != nil {
		return artifactRecord{}, fmt.Errorf("decode stored artifact reference: %w", err)
	}
	if stored.ref.OrgID != scope.OrgID || stored.ref.WorkspaceID != scope.WorkspaceID ||
		stored.ref.ArtifactID != artifactID {
		return artifactRecord{}, ErrScopeMismatch
	}
	if stored.connectionID.Valid && !validStoreID(stored.connectionID.String) ||
		stored.purpose == ArtifactPurposeContinuation && !stored.connectionID.Valid {
		return artifactRecord{}, ErrImmutableConflict
	}
	return stored, nil
}

func CommitManifestCAS(ctx context.Context, tx *Transaction, change ManifestCAS) (Manifest, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return Manifest{}, err
	}
	if !validStoreID(change.ResourceID) {
		return Manifest{}, errors.New("manifest resource identity is invalid")
	}
	if err := RegisterArtifact(ctx, tx, change.ConnectionID, change.Object); err != nil {
		return Manifest{}, err
	}
	ref := change.Object.ref
	committedAt, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return Manifest{}, err
	}
	var revision uint64
	if change.ExpectedRevision == 0 {
		err = tx.queryRow(ctx, `
			INSERT INTO pm.manifests (
				org_id, workspace_id, resource_id, revision, artifact_id, object_attempt_key, committed_at
			) VALUES ($1, $2, $3, 1, $4, $5, $6)
			ON CONFLICT (org_id, workspace_id, resource_id) DO NOTHING
			RETURNING revision
		`, scope.OrgID, scope.WorkspaceID, change.ResourceID, ref.ArtifactID, ref.ObjectAttemptKey, committedAt).Scan(&revision)
	} else {
		err = tx.queryRow(ctx, `
			UPDATE pm.manifests
			SET revision = revision + 1, artifact_id = $2, object_attempt_key = $3, committed_at = $4
			WHERE resource_id = $1 AND revision = $5
			RETURNING revision
		`, change.ResourceID, ref.ArtifactID, ref.ObjectAttemptKey, committedAt, change.ExpectedRevision).Scan(&revision)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Manifest{}, ErrCASConflict
	}
	if err != nil {
		return Manifest{}, fmt.Errorf("commit manifest compare-and-swap: %w", err)
	}
	return Manifest{
		Scope: scope, ResourceID: change.ResourceID, Revision: revision,
		Object: ref, CommittedAt: committedAt,
	}, nil
}

func LoadManifest(ctx context.Context, tx *Transaction, resourceID string) (Manifest, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return Manifest{}, err
	}
	if !validStoreID(resourceID) {
		return Manifest{}, errors.New("manifest resource identity is invalid")
	}
	var manifest Manifest
	var encoded []byte
	manifest.Scope = scope
	manifest.ResourceID = resourceID
	if err := tx.queryRow(ctx, `
		SELECT manifest.revision, artifact.object_ref, manifest.committed_at
		FROM pm.manifests AS manifest
		JOIN pm.artifacts AS artifact
		  ON artifact.org_id = manifest.org_id
		 AND artifact.workspace_id = manifest.workspace_id
		 AND artifact.artifact_id = manifest.artifact_id
		 AND artifact.object_attempt_key = manifest.object_attempt_key
		WHERE manifest.resource_id = $1
	`, resourceID).Scan(&manifest.Revision, &encoded, &manifest.CommittedAt); errors.Is(err, pgx.ErrNoRows) {
		return Manifest{}, ErrNotFound
	} else if err != nil {
		return Manifest{}, fmt.Errorf("load manifest: %w", err)
	}
	if err := json.Unmarshal(encoded, &manifest.Object); err != nil {
		return Manifest{}, fmt.Errorf("decode stored manifest: %w", err)
	}
	if manifest.Object.OrgID != scope.OrgID || manifest.Object.WorkspaceID != scope.WorkspaceID {
		return Manifest{}, ErrScopeMismatch
	}
	manifest.CommittedAt = manifest.CommittedAt.UTC()
	return manifest, nil
}
