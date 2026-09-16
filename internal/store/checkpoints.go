package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const maxCheckpointContinuationBytes = 1 << 20

type CheckpointContinuation struct {
	Present       bool
	Kind          string
	Opaque        []byte
	TieBreaker    string
	UnreadTailRef string
}

func (continuation CheckpointContinuation) validate() error {
	if !continuation.Present {
		if continuation.Kind != "" || continuation.Opaque != nil {
			return errors.New("absent checkpoint continuation cannot carry a kind or opaque value")
		}
	} else if !validStoreName(continuation.Kind) || continuation.Opaque == nil || len(continuation.Opaque) > maxCheckpointContinuationBytes {
		return errors.New("checkpoint continuation kind or opaque value is invalid")
	}
	if continuation.TieBreaker != "" && !validStoreID(continuation.TieBreaker) {
		return errors.New("checkpoint continuation tie breaker is invalid")
	}
	if continuation.UnreadTailRef != "" && !validStoreID(continuation.UnreadTailRef) {
		return errors.New("checkpoint unread-tail reference is invalid")
	}
	return nil
}

func (continuation CheckpointContinuation) equal(other CheckpointContinuation) bool {
	return continuation.Present == other.Present &&
		continuation.Kind == other.Kind &&
		bytes.Equal(continuation.Opaque, other.Opaque) &&
		continuation.TieBreaker == other.TieBreaker &&
		continuation.UnreadTailRef == other.UnreadTailRef
}

type CheckpointIdentity struct {
	ConnectionID      string
	ResourceID        string
	SourceGeneration  string
	SchemaFingerprint contract.SemanticDigest
	Partition         string
	Continuation      CheckpointContinuation
}

func (identity CheckpointIdentity) validate() error {
	if !validStoreID(identity.ConnectionID) || !validStoreID(identity.ResourceID) ||
		!validStoreID(identity.SourceGeneration) || !validStoreID(identity.Partition) {
		return errors.New("checkpoint connection, resource, generation, or partition identity is invalid")
	}
	if err := identity.SchemaFingerprint.Validate(); err != nil {
		return fmt.Errorf("validate checkpoint schema fingerprint: %w", err)
	}
	return identity.Continuation.validate()
}

func (identity CheckpointIdentity) equal(other CheckpointIdentity) bool {
	return identity.ConnectionID == other.ConnectionID &&
		identity.ResourceID == other.ResourceID &&
		identity.SourceGeneration == other.SourceGeneration &&
		identity.SchemaFingerprint == other.SchemaFingerprint &&
		identity.Partition == other.Partition &&
		identity.Continuation.equal(other.Continuation)
}

type Checkpoint struct {
	Scope                     contract.Scope
	Identity                  CheckpointIdentity
	CommittedManifestID       string
	CommittedManifestRevision uint64
	Revision                  uint64
	Writer                    TaskRef
	WriterOwnerID             string
	Fence                     uint64
	CommittedAt               time.Time
}

func (checkpoint Checkpoint) validate() error {
	if err := checkpoint.Scope.Validate(); err != nil {
		return fmt.Errorf("validate checkpoint scope: %w", err)
	}
	if err := checkpoint.Identity.validate(); err != nil {
		return err
	}
	if !validStoreID(checkpoint.CommittedManifestID) || checkpoint.CommittedManifestRevision == 0 || checkpoint.Revision == 0 {
		return errors.New("checkpoint manifest binding or revision is invalid")
	}
	if err := checkpoint.Writer.validate(); err != nil {
		return err
	}
	if !validStoreID(checkpoint.WriterOwnerID) || checkpoint.Fence == 0 {
		return errors.New("checkpoint writer fence is invalid")
	}
	if checkpoint.CommittedAt.IsZero() || checkpoint.CommittedAt.Location() != time.UTC {
		return errors.New("checkpoint commit time must be an explicit UTC instant")
	}
	return nil
}

func (checkpoint Checkpoint) equal(other Checkpoint) bool {
	return checkpoint.Scope.Equal(other.Scope) &&
		checkpoint.Identity.equal(other.Identity) &&
		checkpoint.CommittedManifestID == other.CommittedManifestID &&
		checkpoint.CommittedManifestRevision == other.CommittedManifestRevision &&
		checkpoint.Revision == other.Revision &&
		checkpoint.Writer == other.Writer &&
		checkpoint.WriterOwnerID == other.WriterOwnerID &&
		checkpoint.Fence == other.Fence &&
		checkpoint.CommittedAt.Equal(other.CommittedAt)
}

func LoadCheckpoint(ctx context.Context, tx *Transaction, connectionID, resourceID string) (Checkpoint, error) {
	return loadCheckpoint(ctx, tx, connectionID, resourceID, false)
}

func loadCheckpointForUpdate(ctx context.Context, tx *Transaction, connectionID, resourceID string) (Checkpoint, error) {
	return loadCheckpoint(ctx, tx, connectionID, resourceID, true)
}

func loadCheckpoint(ctx context.Context, tx *Transaction, connectionID, resourceID string, forUpdate bool) (Checkpoint, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return Checkpoint{}, err
	}
	if !validStoreID(connectionID) || !validStoreID(resourceID) {
		return Checkpoint{}, errors.New("checkpoint identity is invalid")
	}
	query := `
		SELECT source_generation, schema_fingerprint, partition_key,
		       continuation_present, continuation_kind, continuation_bytes,
		       tie_breaker, unread_tail_ref,
		       committed_manifest_id, committed_manifest_revision, revision,
		       writer_run_id, writer_task_id, writer_owner_id, writer_fence, committed_at
		FROM pm.checkpoints
		WHERE connection_id = $1 AND resource_id = $2
	`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	checkpoint := Checkpoint{
		Scope: scope,
		Identity: CheckpointIdentity{
			ConnectionID: connectionID,
			ResourceID:   resourceID,
		},
	}
	if err := tx.queryRow(ctx, query, connectionID, resourceID).Scan(
		&checkpoint.Identity.SourceGeneration,
		&checkpoint.Identity.SchemaFingerprint,
		&checkpoint.Identity.Partition,
		&checkpoint.Identity.Continuation.Present,
		&checkpoint.Identity.Continuation.Kind,
		&checkpoint.Identity.Continuation.Opaque,
		&checkpoint.Identity.Continuation.TieBreaker,
		&checkpoint.Identity.Continuation.UnreadTailRef,
		&checkpoint.CommittedManifestID,
		&checkpoint.CommittedManifestRevision,
		&checkpoint.Revision,
		&checkpoint.Writer.RunID,
		&checkpoint.Writer.TaskID,
		&checkpoint.WriterOwnerID,
		&checkpoint.Fence,
		&checkpoint.CommittedAt,
	); errors.Is(err, pgx.ErrNoRows) {
		return Checkpoint{}, ErrNotFound
	} else if err != nil {
		return Checkpoint{}, fmt.Errorf("load checkpoint: %w", err)
	}
	if checkpoint.Identity.Continuation.Present && checkpoint.Identity.Continuation.Opaque == nil {
		checkpoint.Identity.Continuation.Opaque = []byte{}
	}
	checkpoint.Identity.Continuation.Opaque = bytes.Clone(checkpoint.Identity.Continuation.Opaque)
	checkpoint.CommittedAt = checkpoint.CommittedAt.UTC()
	if err := checkpoint.validate(); err != nil {
		return Checkpoint{}, fmt.Errorf("validate stored checkpoint: %w", err)
	}
	return checkpoint, nil
}

func insertCheckpoint(ctx context.Context, tx *Transaction, checkpoint Checkpoint) error {
	if err := checkpoint.validate(); err != nil {
		return err
	}
	var continuation any
	if checkpoint.Identity.Continuation.Present {
		continuation = checkpoint.Identity.Continuation.Opaque
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.checkpoints (
			org_id, workspace_id, connection_id, resource_id,
			source_generation, schema_fingerprint, partition_key,
			continuation_present, continuation_kind, continuation_bytes,
			tie_breaker, unread_tail_ref,
			committed_manifest_id, committed_manifest_revision, revision,
			writer_run_id, writer_task_id, writer_owner_id, writer_fence, committed_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
			$11, $12, $13, $14, $15, $16, $17, $18, $19, $20
		)
	`,
		checkpoint.Scope.OrgID,
		checkpoint.Scope.WorkspaceID,
		checkpoint.Identity.ConnectionID,
		checkpoint.Identity.ResourceID,
		checkpoint.Identity.SourceGeneration,
		string(checkpoint.Identity.SchemaFingerprint),
		checkpoint.Identity.Partition,
		checkpoint.Identity.Continuation.Present,
		checkpoint.Identity.Continuation.Kind,
		continuation,
		checkpoint.Identity.Continuation.TieBreaker,
		checkpoint.Identity.Continuation.UnreadTailRef,
		checkpoint.CommittedManifestID,
		checkpoint.CommittedManifestRevision,
		checkpoint.Revision,
		checkpoint.Writer.RunID,
		checkpoint.Writer.TaskID,
		checkpoint.WriterOwnerID,
		checkpoint.Fence,
		checkpoint.CommittedAt,
	); err != nil {
		return safeMetadataWriteError("insert checkpoint", err)
	}
	return nil
}

func replaceCheckpointCAS(ctx context.Context, tx *Transaction, expected, next Checkpoint) error {
	if err := expected.validate(); err != nil {
		return err
	}
	if err := next.validate(); err != nil {
		return err
	}
	if !expected.Scope.Equal(next.Scope) || expected.Identity.ConnectionID != next.Identity.ConnectionID || expected.Identity.ResourceID != next.Identity.ResourceID {
		return ErrScopeMismatch
	}
	var expectedContinuation any
	if expected.Identity.Continuation.Present {
		expectedContinuation = expected.Identity.Continuation.Opaque
	}
	var nextContinuation any
	if next.Identity.Continuation.Present {
		nextContinuation = next.Identity.Continuation.Opaque
	}
	tag, err := tx.exec(ctx, `
		UPDATE pm.checkpoints
		SET source_generation = $1,
		    schema_fingerprint = $2,
		    partition_key = $3,
		    continuation_present = $4,
		    continuation_kind = $5,
		    continuation_bytes = $6,
		    tie_breaker = $7,
		    unread_tail_ref = $8,
		    committed_manifest_id = $9,
		    committed_manifest_revision = $10,
		    revision = $11,
		    writer_run_id = $12,
		    writer_task_id = $13,
		    writer_owner_id = $14,
		    writer_fence = $15,
		    committed_at = $16
		WHERE connection_id = $17 AND resource_id = $18
		  AND source_generation = $19
		  AND schema_fingerprint = $20
		  AND partition_key = $21
		  AND continuation_present = $22
		  AND continuation_kind = $23
		  AND continuation_bytes IS NOT DISTINCT FROM $24
		  AND tie_breaker = $25
		  AND unread_tail_ref = $26
		  AND committed_manifest_id = $27
		  AND committed_manifest_revision = $28
		  AND revision = $29
		  AND writer_run_id = $30
		  AND writer_task_id = $31
		  AND writer_owner_id = $32
		  AND writer_fence = $33
	`,
		next.Identity.SourceGeneration,
		string(next.Identity.SchemaFingerprint),
		next.Identity.Partition,
		next.Identity.Continuation.Present,
		next.Identity.Continuation.Kind,
		nextContinuation,
		next.Identity.Continuation.TieBreaker,
		next.Identity.Continuation.UnreadTailRef,
		next.CommittedManifestID,
		next.CommittedManifestRevision,
		next.Revision,
		next.Writer.RunID,
		next.Writer.TaskID,
		next.WriterOwnerID,
		next.Fence,
		next.CommittedAt,
		expected.Identity.ConnectionID,
		expected.Identity.ResourceID,
		expected.Identity.SourceGeneration,
		string(expected.Identity.SchemaFingerprint),
		expected.Identity.Partition,
		expected.Identity.Continuation.Present,
		expected.Identity.Continuation.Kind,
		expectedContinuation,
		expected.Identity.Continuation.TieBreaker,
		expected.Identity.Continuation.UnreadTailRef,
		expected.CommittedManifestID,
		expected.CommittedManifestRevision,
		expected.Revision,
		expected.Writer.RunID,
		expected.Writer.TaskID,
		expected.WriterOwnerID,
		expected.Fence,
	)
	if err != nil {
		return safeMetadataWriteError("update checkpoint", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrCASConflict
	}
	return nil
}
