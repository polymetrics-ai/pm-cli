package objects

import (
	"context"
	"errors"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// Publisher coordinates separate PostgreSQL and shared-object commits. Neither
// backend is owned or closed here; process composition owns their lifetimes.
type Publisher struct {
	pool    *store.Pool
	backend *S3
	limits  store.ObjectCapacityLimits
}

func NewPublisher(pool *store.Pool, backend *S3, limits store.ObjectCapacityLimits) (*Publisher, error) {
	if pool == nil || backend == nil || backend.client == nil {
		return nil, ErrInvalidObject
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if uint64(backend.bounds.MaxObjectBytes) > limits.WorkspaceBytes {
		return nil, ErrObjectLimit
	}
	return &Publisher{pool: pool, backend: backend, limits: limits}, nil
}

// Reserve commits finite storage capacity before provider/object I/O. A lost
// acknowledgement is reconciled only against this exact durable reservation.
func (publisher *Publisher) Reserve(ctx context.Context, actor contract.Actor, scope contract.Scope, request store.ObjectReservationRequest) (store.ObjectReservation, error) {
	if publisher == nil || !validArtifactID(request.ArtifactID) ||
		request.MaximumBytes == 0 || request.MaximumBytes > uint64(publisher.backend.bounds.MaxObjectBytes) {
		return store.ObjectReservation{}, ErrObjectLimit
	}
	var reservation store.ObjectReservation
	err := store.WithinScope(ctx, publisher.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		reservation, err = publisher.ReserveInTransaction(ctx, tx, request)
		return err
	})
	if !errors.Is(err, store.ErrCommitOutcomeUnknown) {
		return reservation, err
	}
	var retained store.ObjectReservation
	reconcileErr := store.WithinScope(ctx, publisher.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		var readErr error
		retained, readErr = store.LoadObjectReservation(ctx, tx, request.ReservationID)
		return readErr
	})
	if reconcileErr != nil {
		return store.ObjectReservation{}, errors.Join(err, reconcileErr)
	}
	if retained.Request != request {
		return store.ObjectReservation{}, store.ErrImmutableConflict
	}
	if retained.State == store.ObjectReservationReleased {
		return store.ObjectReservation{}, store.ErrObjectReservationClosed
	}
	return retained, nil
}

// ReserveInTransaction joins capacity reservation to the caller's durable
// dispatch preparation. The caller must commit before any provider or object I/O.
func (publisher *Publisher) ReserveInTransaction(ctx context.Context, tx *store.Transaction, request store.ObjectReservationRequest) (store.ObjectReservation, error) {
	if publisher == nil || !validArtifactID(request.ArtifactID) ||
		request.MaximumBytes == 0 || request.MaximumBytes > uint64(publisher.backend.bounds.MaxObjectBytes) {
		return store.ObjectReservation{}, ErrObjectLimit
	}
	return store.ReserveObjectCapacity(ctx, tx, request, publisher.limits)
}

// CancelUnused frees only a reservation to which no physical attempt was ever
// bound. The store rejects cancellation of unknown, bound or published attempts.
func (publisher *Publisher) CancelUnused(ctx context.Context, actor contract.Actor, scope contract.Scope, reservationID string) error {
	if publisher == nil {
		return ErrInvalidObject
	}
	return store.WithinScope(ctx, publisher.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		return store.CancelObjectReservation(ctx, tx, reservationID)
	})
}

// PreparedPublication contains real readback evidence but is not yet registered
// for its sealed purpose. It may join a caller-owned result/outcome/manifest
// transaction through Commit.
type PreparedPublication struct {
	reservationID string
	purpose       store.ArtifactPurpose
	object        VerifiedObject
}

func (publication PreparedPublication) Reference() (contract.ObjectRef, error) {
	if publication.reservationID == "" ||
		publication.purpose != store.ArtifactPurposePublic &&
			publication.purpose != store.ArtifactPurposeContinuation {
		return contract.ObjectRef{}, ErrInvalidObject
	}
	return publication.object.Reference()
}

func (publication PreparedPublication) Commit(ctx context.Context, tx *store.Transaction) (store.ArtifactPublicationReceipt, error) {
	ref, err := publication.Reference()
	if err != nil {
		return store.ArtifactPublicationReceipt{}, err
	}
	verified, err := store.NewPreverifiedObjectRef(ref, store.ObjectVerification{
		ObservedSHA256:            ref.SHA256,
		ObservedByteLength:        ref.ByteLength,
		ObservedBackendGeneration: ref.BackendGeneration,
	})
	if err != nil {
		return store.ArtifactPublicationReceipt{}, err
	}
	receipt, err := store.CommitObjectPublication(ctx, tx, publication.reservationID, verified)
	if err != nil {
		return store.ArtifactPublicationReceipt{}, err
	}
	if receipt.Purpose != publication.purpose {
		return store.ArtifactPublicationReceipt{}, store.ErrImmutableConflict
	}
	return receipt, nil
}

// Upload first commits the exact physical-attempt binding, then uploads once and
// independently verifies actual bytes. The caller still owns Attempt.Close on
// errors before backend Upload takes ownership; no upload is automatically retried.
func (publisher *Publisher) Upload(ctx context.Context, actor contract.Actor, scope contract.Scope, reservationID string, attempt Attempt) (PreparedPublication, error) {
	if publisher == nil {
		return PreparedPublication{}, ErrInvalidObject
	}
	descriptor, err := attempt.Descriptor()
	if err != nil {
		return PreparedPublication{}, err
	}
	if descriptor.Scope != scope {
		return PreparedPublication{}, store.ErrScopeMismatch
	}
	binding := store.ObjectAttemptBinding{
		Scope: descriptor.Scope, ArtifactID: descriptor.ArtifactID,
		ObjectAttemptKey: descriptor.ObjectAttemptKey, SHA256: descriptor.SHA256,
		ByteLength: descriptor.ByteLength, MediaType: descriptor.MediaType,
	}
	var reservation store.ObjectReservation
	err = store.WithinScope(ctx, publisher.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		var bindErr error
		reservation, bindErr = store.BindObjectAttempt(ctx, tx, reservationID, binding)
		return bindErr
	})
	if errors.Is(err, store.ErrCommitOutcomeUnknown) {
		var retained store.ObjectReservation
		reconcileErr := store.WithinScope(ctx, publisher.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
			var readErr error
			retained, readErr = store.LoadObjectReservation(ctx, tx, reservationID)
			return readErr
		})
		if reconcileErr != nil {
			return PreparedPublication{}, errors.Join(err, reconcileErr)
		}
		if retained.Binding != binding {
			return PreparedPublication{}, errors.Join(err, store.ErrImmutableConflict)
		}
		reservation = retained
		err = nil
	}
	if err != nil {
		return PreparedPublication{}, err
	}
	if reservation.State != store.ObjectReservationBound {
		return PreparedPublication{}, store.ErrObjectReservationClosed
	}
	verified, err := publisher.backend.Upload(ctx, attempt)
	if err != nil {
		return PreparedPublication{}, err
	}
	return PreparedPublication{
		reservationID: reservationID,
		purpose:       reservation.Request.Purpose,
		object:        verified,
	}, nil
}

// Recover uses the immutable PostgreSQL occurrence receipt after a lost commit
// reply and rechecks its actual bytes. Object existence is never a commit witness.
// Callers compare the returned reference with their expected prepared reference
// before resuming any dependent outcome or cursor transition.
func (publisher *Publisher) Recover(ctx context.Context, actor contract.Actor, scope contract.Scope, reservationID string) (store.ArtifactPublicationReceipt, error) {
	if publisher == nil {
		return store.ArtifactPublicationReceipt{}, ErrInvalidObject
	}
	var receipt store.ArtifactPublicationReceipt
	if err := store.WithinScope(ctx, publisher.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		receipt, err = store.LoadArtifactPublication(ctx, tx, reservationID)
		return err
	}); err != nil {
		return store.ArtifactPublicationReceipt{}, err
	}
	if err := publisher.backend.Verify(ctx, receipt.Object); err != nil {
		return store.ArtifactPublicationReceipt{}, err
	}
	return receipt, nil
}
