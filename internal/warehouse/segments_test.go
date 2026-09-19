package warehouse_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/warehouse"
)

// TestBoundedSegmentsManifestAndGC is the V57 acceptance case. It observes that
// dataset membership is exactly the registered segment ledger at one manifest
// revision, that a competing publication is refused by compare-and-swap while
// its verified bytes are retained as an orphan, that compaction publishes new
// immutable objects before advancing the manifest, and that an object retained
// by a query snapshot, plan, proof, backup, or reservation is never collected.
// Objects that exist in the shared store without a registered row never
// establish membership.
func TestBoundedSegmentsManifestAndGC(t *testing.T) {
	harness := newWarehouseHarness(t)

	// Four immutable segments of two rows each are the bounded source set every
	// later case works from.
	for index := range 4 {
		harness.publishSegment(t, fmt.Sprintf("bounded-%d", index+1), harness.rows[index*2:(index+1)*2])
	}
	assertManifestAgrees(t, harness, 4, 4)

	t.Run("segments_are_bounded_and_never_overwritten", func(t *testing.T) {
		state := harness.state(t)
		digests := map[string]uint64{}
		attempts := map[string]uint64{}
		for index, segment := range state.Segments {
			if _, duplicate := digests[segment.Object.SHA256]; duplicate {
				t.Fatalf("segment %d reuses a digest another segment already published", index)
			}
			if _, duplicate := attempts[segment.Object.ObjectAttemptKey]; duplicate {
				t.Fatalf("segment %d reuses a physical attempt key", index)
			}
			digests[segment.Object.SHA256] = segment.Ordinal
			attempts[segment.Object.ObjectAttemptKey] = segment.Ordinal
			if segment.Object.MediaType != warehouse.ParquetSegmentMediaType {
				t.Fatalf("segment %d carries media type %q", index, segment.Object.MediaType)
			}
			if segment.Object.ByteLength <= 0 || segment.Object.ByteLength > warehouseMaximumSegment {
				t.Fatalf("segment %d declares %d bytes outside its bound", index, segment.Object.ByteLength)
			}
			if segment.RowCount != 2 {
				t.Fatalf("segment %d records %d rows for a two-row batch", index, segment.RowCount)
			}
			if segment.ColumnFingerprint != mustSegmentSchema(t, harness.columns).Fingerprint() {
				t.Fatalf("segment %d schema fingerprint %q disagrees with the registered schema",
					index, segment.ColumnFingerprint)
			}
		}

		// A segment above the manifest's own byte bound is refused before any
		// reservation or object byte exists.
		oversized := warehouse.SegmentBytes{
			Path:   filepath.Join(harness.materials, "oversized.parquet"),
			SHA256: segmentDigestOf(make([]byte, 8)),
			Bytes:  warehouseMaximumSegment + 1,
			Schema: mustSegmentSchema(t, harness.columns),
		}
		_, err := harness.manifest.PublishSegment(context.Background(), warehouse.PublishSegmentRequest{
			Actor: harness.fixture.Actor, Scope: harness.fixture.Scope,
			AttemptID: "oversized", Segment: oversized, RowCount: 1,
		})
		if warehouse.FailureCodeOf(err) != warehouse.RefusalBoundExceeded {
			t.Fatalf("oversized segment publication = %v; want a byte-bound refusal", err)
		}
		reservationID, err := harness.layout.SegmentReservationID("oversized")
		if err != nil {
			t.Fatalf("compose the oversized reservation identity: %v", err)
		}
		err = store.WithinScope(context.Background(), harness.fixture.Pool, harness.fixture.Scope, harness.fixture.Actor,
			func(ctx context.Context, tx *store.Transaction) error {
				_, err := store.LoadObjectReservation(ctx, tx, reservationID)
				return err
			})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a refused oversized segment reserved capacity: %v", err)
		}
		assertManifestAgrees(t, harness, 4, 4)
	})

	t.Run("competing_publication_is_refused_and_never_registers", func(t *testing.T) {
		// A publication based on revision 3 loses against committed revision 4
		// after its bytes were really uploaded and verified.
		segment := harness.materialize(t, "lagging", harness.rows[:2])
		_, err := harness.manifest.PublishSegment(context.Background(), warehouse.PublishSegmentRequest{
			Actor: harness.fixture.Actor, Scope: harness.fixture.Scope,
			AttemptID: "lagging", Segment: segment, RowCount: 2, BaseRevision: 3,
		})
		var refused *warehouse.PublicationRefusedError
		if !errors.As(err, &refused) || !errors.Is(err, store.ErrCASConflict) {
			t.Fatalf("lagging publication = %v; want a refused publication reporting the CAS conflict", err)
		}
		harness.verifyObjectBytes(t, refused.Object, mustReadFile(t, segment.Path))
		assertManifestAgrees(t, harness, 4, 4)

		// Two genuinely concurrent publications that both base on revision 4
		// cannot both advance the manifest. Their rows carry disjoint
		// identifiers, so the registered outcome is unambiguous.
		leftRows := warehouseRows(t, 91, 92)
		rightRows := warehouseRows(t, 93, 94)
		left := harness.materialize(t, "race-left", leftRows)
		right := harness.materialize(t, "race-right", rightRows)
		type outcome struct {
			attemptID string
			rows      [][]warehouse.QueryValue
			result    store.WarehouseSegment
			err       error
		}
		outcomes := make(chan outcome, 2)
		start := make(chan struct{})
		run := func(attemptID string, rows [][]warehouse.QueryValue, segment warehouse.SegmentBytes) {
			candidate, err := warehouse.NewDatasetManifest(harness.fixture.Pool, harness.publisher, harness.layout, warehouseMaximumSegment)
			if err != nil {
				outcomes <- outcome{attemptID: attemptID, err: err}
				return
			}
			<-start
			result, err := candidate.PublishSegment(context.Background(), warehouse.PublishSegmentRequest{
				Actor: harness.fixture.Actor, Scope: harness.fixture.Scope,
				AttemptID: attemptID, Segment: segment, RowCount: int64(len(rows)), BaseRevision: 4,
			})
			outcomes <- outcome{attemptID: attemptID, rows: rows, result: result, err: err}
		}
		go run("race-left", leftRows, left)
		go run("race-right", rightRows, right)
		close(start)
		var winner, loser outcome
		for range 2 {
			received := <-outcomes
			if received.err == nil {
				winner = received
			} else {
				loser = received
			}
		}
		if winner.err != nil || loser.err == nil {
			t.Fatalf("competing publications = winner error %v, loser error %v; want exactly one of each",
				winner.err, loser.err)
		}
		var refusedLoser *warehouse.PublicationRefusedError
		if !errors.As(loser.err, &refusedLoser) || !errors.Is(loser.err, store.ErrCASConflict) {
			t.Fatalf("losing publication = %v; want a refused publication reporting the CAS conflict", loser.err)
		}
		if winner.result.Ordinal != 5 || winner.result.ManifestRevision != 5 {
			t.Fatalf("winning publication = ordinal %d revision %d; want 5/5",
				winner.result.Ordinal, winner.result.ManifestRevision)
		}
		harness.raceWinnerRows = winner.rows
		harness.verifyObjectBytes(t, winner.result.Object, mustReadFile(t, segmentPath(left, right, winner.attemptID)))
		harness.verifyObjectBytes(t, refusedLoser.Object, mustReadFile(t, segmentPath(left, right, loser.attemptID)))
		// The losing attempt stays unpublished: no artifact registration and no
		// ledger row carries its rows.
		if err := store.WithinScope(context.Background(), harness.fixture.Pool, harness.fixture.Scope, harness.fixture.Actor,
			func(ctx context.Context, tx *store.Transaction) error {
				if _, err := store.LoadArtifact(ctx, tx, refusedLoser.Object.ArtifactID); !errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("the losing publication registered an artifact: %v", err)
				}
				if _, err := store.LoadWarehouseSegmentByArtifact(ctx, tx, refusedLoser.Object.ArtifactID); !errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("the losing publication registered a ledger row: %v", err)
				}
				return nil
			}); err != nil {
			t.Fatal(err)
		}
		assertManifestAgrees(t, harness, 5, 5)
	})

	t.Run("retained_objects_are_never_collected", func(t *testing.T) {
		// A query snapshot pins the exact set at revision 3, and every remaining
		// retention kind pins one registered segment.
		snapshot, err := harness.manifest.TakeSnapshot(context.Background(), harness.fixture.Actor, harness.fixture.Scope,
			warehouse.SnapshotRequest{SnapshotID: "snapshot-1", Revision: 3})
		if err != nil {
			t.Fatalf("take a query snapshot: %v", err)
		}
		if !reflect.DeepEqual(snapshot.Segments, []uint64{1, 2, 3}) {
			t.Fatalf("snapshot pins segments %v; want the exact registered set [1 2 3]", snapshot.Segments)
		}
		for _, holder := range []struct {
			kind store.WarehouseRetentionKind
			id   string
			at   []uint64
		}{
			{store.WarehouseRetentionPlan, "plan-1", []uint64{1}},
			{store.WarehouseRetentionProof, "proof-1", []uint64{2}},
			{store.WarehouseRetentionBackup, "backup-1", []uint64{3}},
			{store.WarehouseRetentionReservation, "reservation-1", []uint64{4}},
		} {
			if err := harness.manifest.RetainSegments(context.Background(), harness.fixture.Actor, harness.fixture.Scope,
				warehouse.RetainRequest{Kind: holder.kind, HolderID: holder.id, Ordinals: holder.at}); err != nil {
				t.Fatalf("record retention holder %s: %v", holder.id, err)
			}
		}

		before := harness.state(t)
		result, err := warehouse.CompactDatasetSegments(context.Background(), harness.manifest, harness.newService(t), warehouse.CompactionRequest{
			Actor: harness.fixture.Actor, Scope: harness.fixture.Scope,
			ThroughRevision: before.Revision, AttemptID: "compaction-1", Bounds: harness.bounds,
		})
		if err != nil {
			t.Fatalf("compact the live segment set: %v", err)
		}
		if result.Published.Ordinal != before.Revision+1 || result.Published.ManifestRevision != result.Published.Ordinal {
			t.Fatalf("compaction published ordinal %d at revision %d; want the next contiguous revision",
				result.Published.Ordinal, result.Published.ManifestRevision)
		}
		if !reflect.DeepEqual(result.Superseded, []uint64{1, 2, 3, 4, 5}) {
			t.Fatalf("compaction superseded %v; want every live source segment", result.Superseded)
		}
		// Exactly one source segment is unreferenced: it is the only one that
		// may be collected.
		if !reflect.DeepEqual(result.Collected, []uint64{5}) {
			t.Fatalf("compaction collected %v; want only the unreferenced segment [5]", result.Collected)
		}
		held := map[uint64][]string{}
		for _, retained := range result.Retained {
			for _, holder := range retained.Holders {
				held[retained.Ordinal] = append(held[retained.Ordinal], string(holder))
			}
			sort.Strings(held[retained.Ordinal])
		}
		want := map[uint64][]string{
			1: {"plan", "snapshot"},
			2: {"proof", "snapshot"},
			3: {"backup", "snapshot"},
			4: {"reservation"},
		}
		if !reflect.DeepEqual(held, want) {
			t.Fatalf("retained holders = %v; want %v", held, want)
		}

		// Every referenced object stays readable and part of its snapshot; only
		// the unreferenced segment stopped being dataset membership.
		state := harness.state(t)
		if state.Revision != result.Published.Ordinal || uint64(len(state.Segments)) != result.Published.Ordinal {
			t.Fatalf("post-compaction state = revision %d over %d segments", state.Revision, len(state.Segments))
		}
		if !reflect.DeepEqual(state.Live, []uint64{result.Published.Ordinal}) {
			t.Fatalf("live dataset set after compaction = %v; want only the compacted segment", state.Live)
		}
		for _, ordinal := range []uint64{1, 2, 3, 4} {
			segment, err := harness.manifest.SegmentAt(context.Background(), harness.fixture.Actor, harness.fixture.Scope, ordinal)
			if err != nil {
				t.Fatalf("load retained segment %d: %v", ordinal, err)
			}
			if segment.State == store.WarehouseSegmentCollected {
				t.Fatalf("retained segment %d was collected", ordinal)
			}
			if err := harness.consumer.Backend.Verify(context.Background(), segment.Object); err != nil {
				t.Fatalf("retained segment %d is unreadable through the independent gateway: %v", ordinal, err)
			}
		}
		collected, err := harness.manifest.SegmentAt(context.Background(), harness.fixture.Actor, harness.fixture.Scope, 5)
		if err != nil {
			t.Fatalf("load collected segment 5: %v", err)
		}
		if collected.State != store.WarehouseSegmentCollected {
			t.Fatalf("unreferenced segment 5 state = %q; want collected", collected.State)
		}

		// The retained snapshot still reads its exact rows after compaction.
		loaded, err := harness.manifest.LoadSnapshot(context.Background(), harness.fixture.Actor, harness.fixture.Scope, "snapshot-1")
		if err != nil {
			t.Fatalf("load the retained query snapshot: %v", err)
		}
		if !reflect.DeepEqual(loaded.Segments, []uint64{1, 2, 3}) {
			t.Fatalf("retained snapshot set changed to %v", loaded.Segments)
		}
		response, err := harness.newService(t).Query(context.Background(), harness.datasetQuery(t, func(request *warehouse.DatasetQueryRequest) {
			request.SnapshotID = "snapshot-1"
		}))
		if err != nil {
			t.Fatalf("query the retained snapshot: %v", err)
		}
		if !reflect.DeepEqual(response.Rows, harness.rows[:6]) {
			t.Fatalf("retained snapshot rows differ:\n got %s\nwant %s",
				describeRows(response.Rows), describeRows(harness.rows[:6]))
		}
		// The whole dataset now reads exactly the compacted rows.
		total, err := harness.newService(t).Query(context.Background(), harness.datasetQuery(t, func(request *warehouse.DatasetQueryRequest) {
			request.Query.Limit = int64(len(harness.rows) + len(harness.raceWinnerRows))
		}))
		if err != nil {
			t.Fatalf("query the compacted dataset: %v", err)
		}
		compacted := append(append([][]warehouse.QueryValue{}, harness.rows...), harness.raceWinnerRows...)
		if !reflect.DeepEqual(total.Rows, compacted) {
			t.Fatalf("compacted dataset rows differ:\n got %s\nwant %s", describeRows(total.Rows), describeRows(compacted))
		}
		// A row set that was already collected can no longer be pinned.
		if _, err := harness.manifest.TakeSnapshot(context.Background(), harness.fixture.Actor, harness.fixture.Scope,
			warehouse.SnapshotRequest{SnapshotID: "snapshot-collected", Revision: 5}); err == nil {
			t.Fatal("a snapshot pinned a row set whose segment was already collected")
		}
		if _, err := harness.manifest.LoadSnapshot(context.Background(), harness.fixture.Actor, harness.fixture.Scope, "snapshot-collected"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("a refused snapshot left a registered row: %v", err)
		}
	})

	t.Run("unregistered_object_bytes_never_establish_membership", func(t *testing.T) {
		before := harness.state(t)
		// Upload one real object into this dataset's namespace that no manifest
		// transaction ever registers.
		artifactID, err := harness.layout.SegmentArtifactID("orphan")
		if err != nil {
			t.Fatalf("compose the orphan artifact identity: %v", err)
		}
		reservationID, err := harness.layout.SegmentReservationID("orphan")
		if err != nil {
			t.Fatalf("compose the orphan reservation identity: %v", err)
		}
		segment := harness.materialize(t, "orphan", harness.rows[:1])
		payload := mustReadFile(t, segment.Path)
		if _, err := harness.publisher.Reserve(context.Background(), harness.fixture.Actor, harness.fixture.Scope,
			store.ObjectReservationRequest{
				ReservationID: reservationID,
				ConnectionID:  harness.layout.ConnectionID,
				ArtifactID:    artifactID,
				Purpose:       store.ArtifactPurposePublic,
				MaximumBytes:  warehouseMaximumSegment,
			}); err != nil {
			t.Fatalf("reserve the orphan attempt: %v", err)
		}
		attempt, err := objects.NewAttempt(harness.fixture.Scope, artifactID, warehouse.ParquetSegmentMediaType, payload, warehouseMaximumSegment)
		if err != nil {
			t.Fatalf("compose the orphan attempt: %v", err)
		}
		defer func() { _ = attempt.Close() }()
		prepared, err := harness.publisher.Upload(context.Background(), harness.fixture.Actor, harness.fixture.Scope, reservationID, attempt)
		if err != nil {
			t.Fatalf("upload the orphan object: %v", err)
		}
		orphan, err := prepared.Reference()
		if err != nil {
			t.Fatalf("read the orphan reference: %v", err)
		}
		// The bytes really exist in the shared store and verify independently.
		if err := harness.consumer.Backend.Verify(context.Background(), orphan); err != nil {
			t.Fatalf("orphan bytes are unavailable to an independent gateway: %v", err)
		}

		// Membership is the registered ledger, never a listing of the shared
		// store: no ledger row, no artifact registration, no manifest change.
		if err := store.WithinScope(context.Background(), harness.fixture.Pool, harness.fixture.Scope, harness.fixture.Actor,
			func(ctx context.Context, tx *store.Transaction) error {
				if _, err := store.LoadWarehouseSegmentByArtifact(ctx, tx, artifactID); !errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("an unregistered object became a dataset segment: %v", err)
				}
				if _, err := store.LoadArtifact(ctx, tx, artifactID); !errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("an unpublished object became a registered artifact: %v", err)
				}
				return nil
			}); err != nil {
			t.Fatal(err)
		}
		after := harness.state(t)
		if after.Revision != before.Revision || len(after.Segments) != len(before.Segments) ||
			!reflect.DeepEqual(after.Live, before.Live) {
			t.Fatalf("unregistered object bytes changed dataset membership: %d/%d -> %d/%d",
				before.Revision, len(before.Segments), after.Revision, len(after.Segments))
		}
		response, err := harness.newService(t).Query(context.Background(), harness.datasetQuery(t, func(request *warehouse.DatasetQueryRequest) {
			request.Query.Limit = int64(len(harness.rows) + len(harness.raceWinnerRows))
		}))
		if err != nil {
			t.Fatalf("query the live dataset beside the orphan: %v", err)
		}
		if len(response.Rows) != len(harness.rows)+len(harness.raceWinnerRows) {
			t.Fatalf("live dataset returned %d rows beside an unregistered object", len(response.Rows))
		}
	})
}

// segmentPath returns the admitted local path of one competing segment.
func segmentPath(left, right warehouse.SegmentBytes, attemptID string) string {
	if attemptID == "race-left" {
		return left.Path
	}
	return right.Path
}

// segmentDigestOf renders the canonical digest of a local byte payload.
func segmentDigestOf(payload []byte) string {
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}
