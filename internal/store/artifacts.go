package store

import (
	"context"
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

func RegisterArtifact(ctx context.Context, tx *Transaction, object PreverifiedObjectRef) error {
	scope, err := tx.requireScope()
	if err != nil {
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
			byte_length, media_type, backend_generation, object_ref, registered_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (org_id, workspace_id, artifact_id) DO NOTHING
	`,
		ref.OrgID,
		ref.WorkspaceID,
		ref.ArtifactID,
		ref.ObjectAttemptKey,
		ref.SHA256,
		ref.ByteLength,
		ref.MediaType,
		ref.BackendGeneration, json.RawMessage(encoded), registeredAt)
	if err != nil {
		return fmt.Errorf("register artifact reference: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	stored, err := LoadArtifact(ctx, tx, ref.ArtifactID)
	if err != nil {
		return err
	}
	if stored != ref {
		return ErrImmutableConflict
	}
	return nil
}

func LoadArtifact(ctx context.Context, tx *Transaction, artifactID string) (contract.ObjectRef, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return contract.ObjectRef{}, err
	}
	if !validStoreID(artifactID) {
		return contract.ObjectRef{}, errors.New("artifact identity is invalid")
	}
	var encoded []byte
	if err := tx.queryRow(ctx, `SELECT object_ref FROM pm.artifacts WHERE artifact_id = $1`, artifactID).Scan(&encoded); errors.Is(err, pgx.ErrNoRows) {
		return contract.ObjectRef{}, ErrNotFound
	} else if err != nil {
		return contract.ObjectRef{}, fmt.Errorf("load artifact reference: %w", err)
	}
	var ref contract.ObjectRef
	if err := json.Unmarshal(encoded, &ref); err != nil {
		return contract.ObjectRef{}, fmt.Errorf("decode stored artifact reference: %w", err)
	}
	if ref.OrgID != scope.OrgID || ref.WorkspaceID != scope.WorkspaceID || ref.ArtifactID != artifactID {
		return contract.ObjectRef{}, ErrScopeMismatch
	}
	return ref, nil
}

func CommitManifestCAS(ctx context.Context, tx *Transaction, change ManifestCAS) (Manifest, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return Manifest{}, err
	}
	if !validStoreID(change.ResourceID) {
		return Manifest{}, errors.New("manifest resource identity is invalid")
	}
	if err := RegisterArtifact(ctx, tx, change.Object); err != nil {
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
