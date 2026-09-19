package warehouse

import (
	"context"
	"errors"
	"fmt"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// DatasetManifest publishes immutable segments for one dataset and reads the
// state that describes them. Every publication is one PostgreSQL transaction
// that registers the verified bytes, advances the dataset manifest by
// compare-and-swap, records the registered ledger row, and advances the cursor
// to that same row: a crash before the commit leaves an orphan object and no
// state, and a crash after it leaves one row that the cursor already names.
type DatasetManifest struct {
	pool                *store.Pool
	publisher           *objects.Publisher
	layout              WarehouseLayout
	maximumSegmentBytes int64
}

// NewDatasetManifest validates the publisher before any object or database I/O.
func NewDatasetManifest(
	pool *store.Pool,
	publisher *objects.Publisher,
	layout WarehouseLayout,
	maximumSegmentBytes int64,
) (*DatasetManifest, error) {
	if pool == nil || publisher == nil {
		return nil, errors.New("a dataset manifest requires a store pool and a shared-object publisher")
	}
	if err := layout.Validate(); err != nil {
		return nil, err
	}
	if maximumSegmentBytes <= 0 {
		return nil, refusal(RefusalBoundExceeded, "a dataset manifest requires a positive segment byte cap")
	}
	return &DatasetManifest{
		pool: pool, publisher: publisher, layout: layout, maximumSegmentBytes: maximumSegmentBytes,
	}, nil
}

// Layout reports the dataset namespace this manifest publishes into.
func (manifest *DatasetManifest) Layout() WarehouseLayout { return manifest.layout }

// DatasetState is the durable state of one registered dataset.
type DatasetState struct {
	Layout   WarehouseLayout
	Revision uint64
	Cursor   store.WarehouseCursor
	Segments []store.WarehouseSegment
	// Live names the registered ordinals that currently serve the dataset, in
	// ordinal order. Superseded segments leave this set but stay registered.
	Live []uint64
}

// State loads the committed manifest revision, the registered ledger, the
// cursor, and the live set of one dataset.
func (manifest *DatasetManifest) State(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
) (DatasetState, error) {
	state := DatasetState{Layout: manifest.layout}
	err := store.WithinScope(ctx, manifest.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		if _, err := store.LoadWarehouseDataset(ctx, tx, manifest.layout.DatasetID()); err != nil {
			return err
		}
		committed, err := store.LoadManifest(ctx, tx, manifest.layout.DatasetID())
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err == nil {
			state.Revision = committed.Revision
		}
		cursor, err := store.LoadWarehouseCursor(ctx, tx, manifest.layout.DatasetID())
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err == nil {
			state.Cursor = cursor
		}
		segments, err := store.LoadWarehouseSegments(ctx, tx, manifest.layout.DatasetID())
		if err != nil {
			return err
		}
		state.Segments = segments
		for _, segment := range segments {
			if segment.State == store.WarehouseSegmentLive {
				state.Live = append(state.Live, segment.Ordinal)
			}
		}
		return nil
	})
	if err != nil {
		return DatasetState{}, err
	}
	return state, nil
}

// SegmentAt loads one registered ledger row by ordinal.
func (manifest *DatasetManifest) SegmentAt(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	ordinal uint64,
) (store.WarehouseSegment, error) {
	var segment store.WarehouseSegment
	if err := store.WithinScope(ctx, manifest.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		segment, err = store.LoadWarehouseSegmentAt(ctx, tx, manifest.layout.DatasetID(), ordinal)
		return err
	}); err != nil {
		return store.WarehouseSegment{}, err
	}
	return segment, nil
}

// PublishSegmentRequest is one bounded segment publication.
type PublishSegmentRequest struct {
	Actor contract.Actor
	Scope contract.Scope
	// AttemptID makes the publication replayable: the same attempt resolves to
	// the same artifact identity, so an exact replay returns the committed row
	// instead of publishing the same rows twice.
	AttemptID string
	Segment   SegmentBytes
	RowCount  int64
	// BaseRevision advances the manifest only from that exact committed
	// revision. Zero advances from whatever revision is committed, which is
	// what a first publication uses.
	BaseRevision uint64
	// JoinPublication may attach one caller-owned durable transition to the same
	// publication transaction. It runs after the ledger row and cursor are
	// registered, so a caller (for example a compaction) can retire superseded
	// segments atomically with the publication that replaced them.
	JoinPublication func(ctx context.Context, tx *store.Transaction) error
}

// PublicationRefusedError reports a publication whose verified bytes were
// retained in the shared store but never registered. The reference names those
// retained bytes so a caller can reconcile instead of assuming they were
// removed.
type PublicationRefusedError struct {
	ReservationID string
	Object        contract.ObjectRef
	Reason        error
}

func (failure *PublicationRefusedError) Error() string {
	return fmt.Sprintf("warehouse publication refused and its verified bytes retained: %v", failure.Reason)
}

// Unwrap keeps the underlying cause (for example a manifest CAS conflict)
// observable to a caller.
func (failure *PublicationRefusedError) Unwrap() error { return failure.Reason }

// PublishManifest is the declared warehouse publication entry point: it
// composes one dataset manifest from the validated construction inputs and
// delegates exactly one publication to (*DatasetManifest).PublishSegment. A
// caller that holds the dataset configuration rather than a constructed
// manifest uses this seam; a caller that already retains a manifest for repeated
// publication and state reads keeps calling PublishSegment directly.
func PublishManifest(
	ctx context.Context,
	pool *store.Pool,
	publisher *objects.Publisher,
	layout WarehouseLayout,
	maximumSegmentBytes int64,
	request PublishSegmentRequest,
) (store.WarehouseSegment, error) {
	manifest, err := NewDatasetManifest(pool, publisher, layout, maximumSegmentBytes)
	if err != nil {
		return store.WarehouseSegment{}, err
	}
	return manifest.PublishSegment(ctx, request)
}

// PublishSegment materializes no bytes itself: it takes one verified local
// segment, reserves scoped capacity, uploads once through the shared object
// store, and registers the result in one transaction. A refused publication
// leaves no registered state and no advanced cursor.
func (manifest *DatasetManifest) PublishSegment(
	ctx context.Context,
	request PublishSegmentRequest,
) (store.WarehouseSegment, error) {
	if ctx == nil {
		return store.WarehouseSegment{}, refusal(RefusalProtocol, "a publication requires a non-nil context")
	}
	if err := request.Segment.Validate(); err != nil {
		return store.WarehouseSegment{}, err
	}
	if request.RowCount < 0 {
		return store.WarehouseSegment{}, refusal(RefusalSchemaMismatch, "a publication must declare a non-negative row count")
	}
	if request.Segment.Bytes > manifest.maximumSegmentBytes {
		return store.WarehouseSegment{}, refusal(RefusalBoundExceeded, "the segment exceeds this dataset's admitted byte cap")
	}
	artifactID, err := manifest.layout.SegmentArtifactID(request.AttemptID)
	if err != nil {
		return store.WarehouseSegment{}, err
	}
	reservationID, err := manifest.layout.SegmentReservationID(request.AttemptID)
	if err != nil {
		return store.WarehouseSegment{}, err
	}
	schemaDocument, err := request.Segment.Schema.Encode()
	if err != nil {
		return store.WarehouseSegment{}, err
	}
	expectedRevision, replay, found, err := manifest.resolution(ctx, request, artifactID)
	if err != nil {
		return store.WarehouseSegment{}, err
	}
	if found {
		return replay, nil
	}

	if _, err := manifest.publisher.Reserve(ctx, request.Actor, request.Scope, store.ObjectReservationRequest{
		ReservationID: reservationID,
		ConnectionID:  manifest.layout.ConnectionID,
		ArtifactID:    artifactID,
		Purpose:       store.ArtifactPurposePublic,
		MaximumBytes:  uint64(manifest.maximumSegmentBytes),
	}); err != nil {
		return store.WarehouseSegment{}, fmt.Errorf("reserve warehouse segment capacity: %w", err)
	}
	payload, err := LoadSegmentBytes(request.Segment, manifest.maximumSegmentBytes)
	if err != nil {
		manifest.releaseUnused(ctx, request, reservationID)
		return store.WarehouseSegment{}, err
	}
	attempt, err := objects.NewAttempt(request.Scope, artifactID, ParquetSegmentMediaType, payload, manifest.maximumSegmentBytes)
	if err != nil {
		manifest.releaseUnused(ctx, request, reservationID)
		return store.WarehouseSegment{}, err
	}
	defer func() { _ = attempt.Close() }()
	prepared, err := manifest.publisher.Upload(ctx, request.Actor, request.Scope, reservationID, attempt)
	if err != nil {
		manifest.releaseUnused(ctx, request, reservationID)
		return store.WarehouseSegment{}, fmt.Errorf("upload the verified warehouse segment: %w", err)
	}
	verified, err := prepared.Reference()
	if err != nil {
		return store.WarehouseSegment{}, err
	}
	objectRef, err := store.NewPreverifiedObjectRef(verified, store.ObjectVerification{
		ObservedSHA256:            verified.SHA256,
		ObservedByteLength:        verified.ByteLength,
		ObservedBackendGeneration: verified.BackendGeneration,
	})
	if err != nil {
		return store.WarehouseSegment{}, err
	}

	var published store.WarehouseSegment
	err = store.WithinScope(ctx, manifest.pool, request.Scope, request.Actor, func(ctx context.Context, tx *store.Transaction) error {
		receipt, err := prepared.Commit(ctx, tx)
		if err != nil {
			return err
		}
		if receipt.Object != verified {
			return store.ErrImmutableConflict
		}
		committed, err := store.CommitManifestCAS(ctx, tx, store.ManifestCAS{
			ConnectionID:     manifest.layout.ConnectionID,
			ResourceID:       manifest.layout.DatasetID(),
			ExpectedRevision: expectedRevision,
			Object:           objectRef,
		})
		if err != nil {
			return err
		}
		if committed.Object != verified {
			return store.ErrImmutableConflict
		}
		segment, err := store.RegisterWarehouseSegment(
			ctx, tx, manifest.layout.DatasetID(), request.RowCount,
			request.Segment.Schema.Fingerprint(), schemaDocument,
		)
		if err != nil {
			return err
		}
		if _, err := store.AdvanceWarehouseCursor(ctx, tx, manifest.layout.DatasetID(), expectedRevision, segment.Ordinal); err != nil {
			return err
		}
		if request.JoinPublication != nil {
			if err := request.JoinPublication(ctx, tx); err != nil {
				return err
			}
		}
		published = segment
		return nil
	})
	if err == nil {
		return published, nil
	}
	if !errors.Is(err, store.ErrCommitOutcomeUnknown) {
		return store.WarehouseSegment{}, &PublicationRefusedError{
			ReservationID: reservationID, Object: verified, Reason: err,
		}
	}
	// The publication outcome is unknown: reconcile against the immutable
	// occurrence receipt rather than inferring from object existence.
	recovered, err := manifest.publisher.Recover(ctx, request.Actor, request.Scope, reservationID)
	if err != nil {
		return store.WarehouseSegment{}, &PublicationRefusedError{
			ReservationID: reservationID, Object: verified, Reason: errors.Join(store.ErrCommitOutcomeUnknown, err),
		}
	}
	if recovered.Object != verified {
		return store.WarehouseSegment{}, &PublicationRefusedError{
			ReservationID: reservationID, Object: verified, Reason: store.ErrImmutableConflict,
		}
	}
	registered, found, err := manifest.registered(ctx, request.Actor, request.Scope, artifactID, request.Segment)
	if err != nil {
		return store.WarehouseSegment{}, err
	}
	if !found {
		return store.WarehouseSegment{}, &PublicationRefusedError{
			ReservationID: reservationID, Object: verified, Reason: store.ErrNotFound,
		}
	}
	return registered, nil
}

// resolution reports the revision one publication must advance from and whether
// the exact attempt was already registered.
func (manifest *DatasetManifest) resolution(
	ctx context.Context,
	request PublishSegmentRequest,
	artifactID string,
) (uint64, store.WarehouseSegment, bool, error) {
	var expected uint64
	var replay store.WarehouseSegment
	var found bool
	err := store.WithinScope(ctx, manifest.pool, request.Scope, request.Actor, func(ctx context.Context, tx *store.Transaction) error {
		if _, err := store.LoadWarehouseDataset(ctx, tx, manifest.layout.DatasetID()); err != nil {
			return err
		}
		registered, err := store.LoadWarehouseSegmentByArtifact(ctx, tx, artifactID)
		switch {
		case err == nil:
			if registered.Object.SHA256 != segmentDigestHex(request.Segment.SHA256) ||
				registered.Object.ByteLength != request.Segment.Bytes ||
				registered.RowCount != request.RowCount {
				return store.ErrImmutableConflict
			}
			replay = registered
			found = true
			return nil
		case errors.Is(err, store.ErrNotFound):
		default:
			return err
		}
		committed, err := store.LoadManifest(ctx, tx, manifest.layout.DatasetID())
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err == nil {
			expected = committed.Revision
		}
		return nil
	})
	if err != nil {
		return 0, store.WarehouseSegment{}, false, err
	}
	if request.BaseRevision != 0 {
		expected = request.BaseRevision
	}
	if found {
		return expected, replay, true, nil
	}
	return expected, store.WarehouseSegment{}, false, nil
}

// registered loads the ledger row of one artifact and requires it to describe
// the segment being published.
func (manifest *DatasetManifest) registered(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	artifactID string,
	segment SegmentBytes,
) (store.WarehouseSegment, bool, error) {
	var registered store.WarehouseSegment
	var found bool
	err := store.WithinScope(ctx, manifest.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		stored, err := store.LoadWarehouseSegmentByArtifact(ctx, tx, artifactID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if stored.Object.SHA256 != segmentDigestHex(segment.SHA256) || stored.Object.ByteLength != segment.Bytes {
			return store.ErrImmutableConflict
		}
		registered = stored
		found = true
		return nil
	})
	if err != nil {
		return store.WarehouseSegment{}, false, err
	}
	return registered, found, nil
}

// releaseUnused frees capacity for a publication that never bound a physical
// attempt. A bound or published attempt keeps its charge, and the cleanup never
// replaces the caller's cause.
func (manifest *DatasetManifest) releaseUnused(ctx context.Context, request PublishSegmentRequest, reservationID string) {
	if err := manifest.publisher.CancelUnused(ctx, request.Actor, request.Scope, reservationID); err != nil &&
		!errors.Is(err, store.ErrObjectReservationClosed) {
		return
	}
}

// SnapshotRequest pins one retained read view.
type SnapshotRequest struct {
	SnapshotID string
	// Revision is the committed manifest revision the snapshot pins. It may be
	// an older revision whose rows are all still retained.
	Revision uint64
}

// TakeSnapshot retains the exact registered set at one revision. The snapshot
// itself becomes a retention holder of every segment in that set.
func (manifest *DatasetManifest) TakeSnapshot(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	request SnapshotRequest,
) (store.WarehouseSnapshot, error) {
	var snapshot store.WarehouseSnapshot
	if err := store.WithinScope(ctx, manifest.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		snapshot, err = store.CreateWarehouseSnapshot(ctx, tx, store.WarehouseSnapshot{
			Scope:            scope,
			SnapshotID:       request.SnapshotID,
			DatasetID:        manifest.layout.DatasetID(),
			ManifestRevision: request.Revision,
		})
		return err
	}); err != nil {
		return store.WarehouseSnapshot{}, err
	}
	return snapshot, nil
}

// LoadSnapshot loads one retained snapshot and its exact segment set.
func (manifest *DatasetManifest) LoadSnapshot(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	snapshotID string,
) (store.WarehouseSnapshot, error) {
	var snapshot store.WarehouseSnapshot
	if err := store.WithinScope(ctx, manifest.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		snapshot, err = store.LoadWarehouseSnapshot(ctx, tx, snapshotID)
		if err != nil {
			return err
		}
		if snapshot.DatasetID != manifest.layout.DatasetID() {
			return store.ErrScopeMismatch
		}
		return nil
	}); err != nil {
		return store.WarehouseSnapshot{}, err
	}
	return snapshot, nil
}

// RetainRequest records one durable holder of registered segments.
type RetainRequest struct {
	Kind     store.WarehouseRetentionKind
	HolderID string
	Ordinals []uint64
}

// RetainSegments records one retention holder over registered segments. A
// holder may only name segments that are still retained: a collected object can
// never be brought back.
func (manifest *DatasetManifest) RetainSegments(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	request RetainRequest,
) error {
	if !request.Kind.Valid() || request.HolderID == "" || len(request.Ordinals) == 0 {
		return refusal(RefusalProtocol, "a retention request requires a kind, a holder identity, and at least one segment")
	}
	return store.WithinScope(ctx, manifest.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		for _, ordinal := range request.Ordinals {
			if _, err := store.RetainWarehouseSegment(ctx, tx, store.WarehouseRetention{
				Scope: scope, DatasetID: manifest.layout.DatasetID(), Ordinal: ordinal,
				Kind: request.Kind, HolderID: request.HolderID,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}
