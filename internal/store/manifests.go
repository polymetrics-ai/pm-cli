package store

import (
	"context"
	"errors"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type ManifestCheckpointCAS struct {
	Expected *Checkpoint
	Next     CheckpointIdentity
	Manifest ManifestCAS
	Lease    Lease
}

// CommitManifestCheckpoint atomically advances the existing manifest authority
// and its complete checkpoint identity under a current task fence. The opaque
// PreverifiedObjectRef proves descriptor equality only; this metadata commit
// performs no object upload, readback, or independent byte verification.
func CommitManifestCheckpoint(ctx context.Context, tx *Transaction, change ManifestCheckpointCAS) (Checkpoint, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return Checkpoint{}, err
	}
	if err := change.Next.validate(); err != nil {
		return Checkpoint{}, err
	}
	if change.Manifest.ResourceID == "" {
		return Checkpoint{}, errors.New("manifest identity is required")
	}
	if change.Manifest.ConnectionID != change.Next.ConnectionID {
		return Checkpoint{}, ErrImmutableConflict
	}
	connection, err := LoadConnection(ctx, tx, change.Next.ConnectionID)
	if err != nil {
		return Checkpoint{}, err
	}
	if !connection.Scope.Equal(scope) {
		return Checkpoint{}, ErrScopeMismatch
	}
	if err := change.Lease.validate(); err != nil {
		return Checkpoint{}, err
	}
	if err := requireCurrentLease(ctx, tx, change.Lease); err != nil {
		return Checkpoint{}, err
	}

	var revision uint64
	if change.Expected == nil {
		if change.Manifest.ExpectedRevision != 0 {
			return Checkpoint{}, ErrCASConflict
		}
		if _, err := loadCheckpointForUpdate(ctx, tx, change.Next.ConnectionID, change.Next.ResourceID); err == nil {
			return Checkpoint{}, ErrCASConflict
		} else if !errors.Is(err, ErrNotFound) {
			return Checkpoint{}, err
		}
		revision = 1
	} else {
		if err := change.Expected.validate(); err != nil {
			return Checkpoint{}, err
		}
		if !change.Expected.Scope.Equal(scope) ||
			change.Expected.Identity.ConnectionID != change.Next.ConnectionID ||
			change.Expected.Identity.ResourceID != change.Next.ResourceID ||
			change.Manifest.ResourceID != change.Expected.CommittedManifestID ||
			change.Manifest.ExpectedRevision != change.Expected.CommittedManifestRevision {
			return Checkpoint{}, ErrCASConflict
		}
		stored, err := loadCheckpointForUpdate(ctx, tx, change.Next.ConnectionID, change.Next.ResourceID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return Checkpoint{}, ErrCASConflict
			}
			return Checkpoint{}, err
		}
		if !stored.equal(*change.Expected) || change.Expected.Revision == ^uint64(0) {
			return Checkpoint{}, ErrCASConflict
		}
		revision = change.Expected.Revision + 1
	}

	manifest, err := CommitManifestCAS(ctx, tx, change.Manifest)
	if err != nil {
		return Checkpoint{}, err
	}
	next := Checkpoint{
		Scope:                     contract.Scope{OrgID: scope.OrgID, WorkspaceID: scope.WorkspaceID},
		Identity:                  cloneCheckpointIdentity(change.Next),
		CommittedManifestID:       manifest.ResourceID,
		CommittedManifestRevision: manifest.Revision,
		Revision:                  revision,
		Writer:                    change.Lease.Task,
		WriterOwnerID:             change.Lease.OwnerID,
		Fence:                     change.Lease.Fence,
		CommittedAt:               manifest.CommittedAt.UTC(),
	}
	if change.Expected == nil {
		if err := insertCheckpoint(ctx, tx, next); err != nil {
			if errors.Is(err, ErrImmutableConflict) {
				return Checkpoint{}, ErrCASConflict
			}
			return Checkpoint{}, err
		}
	} else if err := replaceCheckpointCAS(ctx, tx, *change.Expected, next); err != nil {
		return Checkpoint{}, err
	}
	return next, nil
}

func cloneCheckpointIdentity(identity CheckpointIdentity) CheckpointIdentity {
	identity.Continuation.Opaque = append([]byte(nil), identity.Continuation.Opaque...)
	if identity.Continuation.Present && identity.Continuation.Opaque == nil {
		identity.Continuation.Opaque = []byte{}
	}
	return identity
}
