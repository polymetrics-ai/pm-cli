package warehouse_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/warehouse"
)

// TestRowsBeforeCheckpoint is the V38 acceptance case. It exercises the three
// publication windows that decide whether a cursor can ever name rows that were
// not verified and registered: a segment refused before its object completed, a
// verified object whose manifest transaction never committed, and a committed
// manifest/cursor transaction whose COMMIT reply was lost. After every window
// the registered ledger, the manifest revision, and the cursor still agree, and
// an independent host reads the exact registered rows through the other shared
// gateway without any shared filesystem.
func TestRowsBeforeCheckpoint(t *testing.T) {
	harness := newWarehouseHarness(t)

	t.Run("segment_refused_before_object_completion_registers_nothing", func(t *testing.T) {
		columns := []warehouse.QueryColumn{
			{Name: "id", Kind: warehouse.QueryKindInt64, Nullable: false},
			{Name: "note", Kind: warehouse.QueryKindText, Nullable: false},
		}
		rows := make([][]warehouse.QueryValue, 2000)
		for index := range rows {
			rows[index] = []warehouse.QueryValue{warehouse.Int64QueryValue(int64(index)), warehouse.TextQueryValue(fmt.Sprintf("bounded row %d", index))}
		}
		// The declared byte bound is far below the segment the child would
		// materialize, so the child refuses and removes its partial output.
		bounds := harness.bounds
		bounds.Bytes = 4096
		output := filepath.Join(harness.materials, "oversized.parquet")
		_, err := warehouse.MaterializeParquetSegment(context.Background(), harness.newChild(t, nil), warehouse.MaterializeRequest{
			RequestID:           "oversized",
			AdmittedDirectories: []string{harness.materials},
			OutputPath:          output,
			Columns:             columns,
			Rows:                rows,
			Bounds:              bounds,
			MaximumBytes:        warehouseMaximumSegment,
		})
		failure := requireRefusal(t, err)
		if failure.Code != warehouse.RefusalBoundExceeded || failure.Origin != warehouse.RefusalOriginChild {
			t.Fatalf("oversized materialization refusal = %v; want a child byte-bound refusal", failure)
		}
		if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a refused segment left a partial object behind: %v", err)
		}

		state := harness.state(t)
		if state.Revision != 0 || len(state.Segments) != 0 || state.Cursor.Ordinal != 0 {
			t.Fatalf("a refused materialization registered state: revision=%d segments=%d cursor=%d",
				state.Revision, len(state.Segments), state.Cursor.Ordinal)
		}
		// The dataset is still usable: the first admitted publication is the
		// first revision, never a later one.
		published := harness.publishSegment(t, "first-segment", harness.rows[:warehouseSegmentSplit])
		if published.Segment.Ordinal != 1 || published.Segment.ManifestRevision != 1 {
			t.Fatalf("first admitted publication = ordinal %d revision %d; want 1/1",
				published.Segment.Ordinal, published.Segment.ManifestRevision)
		}
	})

	t.Run("interrupted_manifest_transaction_retains_only_an_orphan", func(t *testing.T) {
		segment := harness.materialize(t, "interrupted", harness.rows[warehouseSegmentSplit:warehouseSegmentSplit*2])
		interrupted := errors.New("owned interruption inside the manifest and cursor transaction")
		_, err := harness.manifest.PublishSegment(context.Background(), warehouse.PublishSegmentRequest{
			Actor:     harness.fixture.Actor,
			Scope:     harness.fixture.Scope,
			AttemptID: "interrupted",
			Segment:   segment,
			RowCount:  warehouseSegmentSplit,
			JoinPublication: func(ctx context.Context, tx *store.Transaction) error {
				return interrupted
			},
		})
		var refused *warehouse.PublicationRefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("interrupted publication error = %v; want a retained refused publication", err)
		}
		if !errors.Is(err, interrupted) {
			t.Fatalf("interrupted publication lost its cause: %v", err)
		}
		// The bytes are retained and verified; nothing about them was
		// registered, and the cursor did not move.
		harness.verifyObjectBytes(t, refused.Object, mustReadFile(t, segment.Path))
		reservationID, err := harness.layout.SegmentReservationID("interrupted")
		if err != nil {
			t.Fatalf("compose the interrupted reservation identity: %v", err)
		}
		if err := store.WithinScope(context.Background(), harness.fixture.Pool, harness.fixture.Scope, harness.fixture.Actor,
			func(ctx context.Context, tx *store.Transaction) error {
				if _, err := store.LoadArtifact(ctx, tx, refused.Object.ArtifactID); !errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("an interrupted publication registered an artifact: %v", err)
				}
				if _, err := store.LoadWarehouseSegmentByArtifact(ctx, tx, refused.Object.ArtifactID); !errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("an interrupted publication registered a ledger row: %v", err)
				}
				return nil
			}); err != nil {
			t.Fatal(err)
		}
		// Object existence is never a publication witness.
		if _, err := harness.consumerReader.Recover(context.Background(), harness.fixture.Actor, harness.fixture.Scope, reservationID); err == nil {
			t.Fatal("retained orphan bytes stood in for a committed publication occurrence")
		}
		assertManifestAgrees(t, harness, 1, 1)

		// The retry publishes the same rows under a new immutable attempt and
		// the cursor advances to exactly that registered row.
		retried := harness.publishSegment(t, "repaired", harness.rows[warehouseSegmentSplit:warehouseSegmentSplit*2])
		if retried.Segment.Object.ObjectAttemptKey == refused.Object.ObjectAttemptKey {
			t.Fatal("the retry reused the retained orphan attempt")
		}
		if retried.Segment.Ordinal != 2 {
			t.Fatalf("retry registered ordinal %d; want the next contiguous revision 2", retried.Segment.Ordinal)
		}
		assertManifestAgrees(t, harness, 2, 2)
	})

	t.Run("verified_object_without_commit_keeps_only_a_retained_orphan", func(t *testing.T) {
		state := harness.state(t)
		if state.Revision != 2 {
			t.Fatalf("expected the retained publications to be committed at revision 2, found %d", state.Revision)
		}
		// A competing publisher that based its work on revision 1 uploads and
		// independently verifies real bytes, then loses the manifest CAS.
		segment := harness.materialize(t, "stale-attempt", harness.rows[warehouseSegmentSplit:warehouseSegmentSplit+1])
		_, err := harness.manifest.PublishSegment(context.Background(), warehouse.PublishSegmentRequest{
			Actor:        harness.fixture.Actor,
			Scope:        harness.fixture.Scope,
			AttemptID:    "stale-attempt",
			Segment:      segment,
			RowCount:     1,
			BaseRevision: 1,
		})
		var refused *warehouse.PublicationRefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("stale publication error = %v; want a retained refused publication", err)
		}
		if !errors.Is(err, store.ErrCASConflict) {
			t.Fatalf("stale publication did not report the manifest CAS conflict: %v", err)
		}
		// The uploaded bytes are real, verified, and retained; they are simply
		// not part of the dataset.
		if err := harness.consumer.Backend.Verify(context.Background(), refused.Object); err != nil {
			t.Fatalf("retained orphan bytes are unavailable to an independent gateway: %v", err)
		}
		harness.verifyObjectBytes(t, refused.Object, mustReadFile(t, segment.Path))
		if err := store.WithinScope(context.Background(), harness.fixture.Pool, harness.fixture.Scope, harness.fixture.Actor,
			func(ctx context.Context, tx *store.Transaction) error {
				if _, err := store.LoadArtifact(ctx, tx, refused.Object.ArtifactID); !errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("refused publication became a registered artifact: %v", err)
				}
				reservation, err := store.LoadObjectReservation(ctx, tx, refused.ReservationID)
				if err != nil {
					return err
				}
				if reservation.State != store.ObjectReservationBound ||
					reservation.Binding.SHA256 != refused.Object.SHA256 ||
					reservation.Binding.ByteLength != refused.Object.ByteLength {
					return fmt.Errorf("retained attempt is not the verified orphan: %+v", reservation)
				}
				return nil
			}); err != nil {
			t.Fatal(err)
		}
		assertManifestAgrees(t, harness, 2, 2)
	})

	t.Run("cursor_cannot_advance_ahead_of_registered_rows", func(t *testing.T) {
		// A caller writing SQL directly cannot name a cursor position that no
		// registered segment occupies.
		violation := harness.advanceCursorDirectly(t, 99)
		if violation == nil || violation.Code != "23503" {
			t.Fatalf("a cursor over a gap was not refused by the schema: %v", violation)
		}
		// The same refusal holds through this package's own CAS.
		err := store.WithinScope(context.Background(), harness.fixture.Pool, harness.fixture.Scope, harness.fixture.Actor,
			func(ctx context.Context, tx *store.Transaction) error {
				_, err := store.AdvanceWarehouseCursor(ctx, tx, harness.layout.DatasetID(), 2, 4)
				return err
			})
		if !errors.Is(err, store.ErrImmutableConflict) {
			t.Fatalf("advancing the cursor over a gap = %v; want an immutable-conflict refusal", err)
		}
		assertManifestAgrees(t, harness, 2, 2)

		// The cursor names exactly the immutable registered rows: every ordinal
		// below it exists, and the object at the cursor verifies independently.
		state := harness.state(t)
		for index, segment := range state.Segments {
			if segment.Ordinal != uint64(index+1) {
				t.Fatalf("registered ledger has a gap at position %d: ordinal %d", index, segment.Ordinal)
			}
			if err := harness.consumer.Backend.Verify(context.Background(), segment.Object); err != nil {
				t.Fatalf("registered segment %d is unreadable: %v", index, err)
			}
		}
		atCursor, err := harness.manifest.SegmentAt(context.Background(), harness.fixture.Actor, harness.fixture.Scope, state.Cursor.Ordinal)
		if err != nil {
			t.Fatalf("load the segment the cursor names: %v", err)
		}
		if atCursor.Object != state.Segments[state.Cursor.Ordinal-1].Object {
			t.Fatalf("cursor ordinal %d names %+v instead of the registered %+v",
				state.Cursor.Ordinal, atCursor.Object, state.Segments[state.Cursor.Ordinal-1].Object)
		}
	})

	t.Run("independent_host_reads_exact_rows_without_shared_filesystem", func(t *testing.T) {
		harness.publishSegment(t, "third-segment", harness.rows[warehouseSegmentSplit*2:])
		assertManifestAgrees(t, harness, 3, 3)

		// Close the producer's private scratch and replace the runtime pool:
		// the second host keeps only the database identity and the other shared
		// gateway, and stages its own relations in a fresh directory.
		if err := harness.producer.Close(); err != nil {
			t.Fatalf("close the first host's object scratch: %v", err)
		}
		harness.fixture.Restart(t)
		reader := harness.newPublisher(t, harness.consumer)
		manifest, err := warehouse.NewDatasetManifest(harness.fixture.Pool, reader, harness.layout, warehouseMaximumSegment)
		if err != nil {
			t.Fatalf("construct the second host manifest: %v", err)
		}
		secondHostRoot := filepath.Join(harness.root, "second-host")
		secondHostAdmitted := filepath.Join(secondHostRoot, "admitted")
		secondHostStaging := filepath.Join(secondHostAdmitted, "staging")
		if err := os.MkdirAll(secondHostStaging, 0o700); err != nil {
			t.Fatalf("create the second host staging root: %v", err)
		}
		service, err := warehouse.NewQueryService(harness.fixture.Pool, harness.consumer.Backend,
			harness.newChild(t, []string{secondHostAdmitted}), secondHostStaging)
		if err != nil {
			t.Fatalf("construct the second host query service: %v", err)
		}
		state, err := manifest.State(context.Background(), harness.fixture.Actor, harness.fixture.Scope)
		if err != nil {
			t.Fatalf("second host dataset state: %v", err)
		}
		if state.Revision != 3 || len(state.Segments) != 3 || state.Cursor.Ordinal != 3 {
			t.Fatalf("second host state = revision %d over %d segments at cursor %d; want 3/3/3",
				state.Revision, len(state.Segments), state.Cursor.Ordinal)
		}
		response, err := service.Query(context.Background(), warehouse.DatasetQueryRequest{
			Actor: harness.fixture.Actor, Scope: harness.fixture.Scope, Layout: harness.layout,
			QueryID: "second-host-query",
			Query: warehouse.SavedQuery{
				Name: "orders_all", Relation: warehouseRelationName,
				Project: allColumnProjections(harness.columns),
				Order:   []warehouse.QueryOrder{{Column: "id"}},
				Limit:   int64(len(harness.rows)),
			},
			MaximumSegmentBytes: warehouseMaximumSegment,
			Bounds:              harness.bounds,
		})
		if err != nil {
			t.Fatalf("second host query: %v", err)
		}
		if !reflect.DeepEqual(response.Rows, harness.rows) {
			t.Fatalf("second host rows differ:\n got %s\nwant %s", describeRows(response.Rows), describeRows(harness.rows))
		}
		if entries, err := os.ReadDir(secondHostStaging); err != nil || len(entries) != 0 {
			t.Fatalf("second host staging root retained %d entries: %v", len(entries), err)
		}
	})
}

// assertManifestAgrees confirms that the manifest revision, the registered
// ledger, and the cursor describe one contiguous dataset.
func assertManifestAgrees(t *testing.T, harness *warehouseHarness, revision uint64, cursor uint64) {
	t.Helper()
	state := harness.state(t)
	if state.Revision != revision {
		t.Fatalf("manifest revision = %d; want %d", state.Revision, revision)
	}
	if state.Cursor.Ordinal != cursor || state.Cursor.ManifestRevision != revision {
		t.Fatalf("cursor = ordinal %d at revision %d; want ordinal %d at revision %d",
			state.Cursor.Ordinal, state.Cursor.ManifestRevision, cursor, revision)
	}
	if uint64(len(state.Segments)) != revision {
		t.Fatalf("registered ledger holds %d segments for revision %d", len(state.Segments), revision)
	}
	for index, segment := range state.Segments {
		if segment.Ordinal != uint64(index+1) || segment.ManifestRevision != segment.Ordinal {
			t.Fatalf("ledger position %d carries ordinal %d and revision %d", index, segment.Ordinal, segment.ManifestRevision)
		}
	}
}

// advanceCursorDirectly attempts to move the cursor onto an ordinal that no
// registered segment occupies, through the migrator boundary that this package
// does not control. The refused statement aborts its transaction, so nothing is
// committed.
func (harness *warehouseHarness) advanceCursorDirectly(t *testing.T, ordinal uint64) *pgconn.PgError {
	t.Helper()
	var violation *pgconn.PgError
	err := harness.fixture.WithinSetupSQL(context.Background(), harness.fixture.Scope,
		func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `
				UPDATE pm.warehouse_dataset_cursors
				SET cursor_ordinal = $4, manifest_revision = $4
				WHERE org_id = $1 AND workspace_id = $2 AND dataset_id = $3
			`, harness.fixture.Scope.OrgID, harness.fixture.Scope.WorkspaceID, harness.layout.DatasetID(), int64(ordinal))
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return fmt.Errorf("the cursor row for the dataset is absent")
			}
			return fmt.Errorf("cursor ordinal %d was accepted without a registered segment", ordinal)
		})
	if err == nil {
		t.Fatal("a cursor over a gap was accepted")
	}
	if !errors.As(err, &violation) {
		t.Fatalf("unregistered cursor refusal is not a database refusal: %v", err)
	}
	return violation
}

// verifyObjectBytes reads one object through the independent gateway and
// requires exactly the bytes it was published from.
func (harness *warehouseHarness) verifyObjectBytes(t *testing.T, ref contract.ObjectRef, want []byte) {
	t.Helper()
	body, err := harness.consumer.Backend.OpenVerified(context.Background(), ref, objects.ByteRange{Length: ref.ByteLength})
	if err != nil {
		t.Fatalf("read the retained object through the independent gateway: %v", err)
	}
	defer func() { _ = body.Close() }()
	read, err := io.ReadAll(io.LimitReader(body, ref.ByteLength+1))
	if err != nil {
		t.Fatalf("read the retained object bytes: %v", err)
	}
	if !bytes.Equal(read, want) {
		t.Fatalf("retained object bytes differ from the published segment: %d bytes vs %d", len(read), len(want))
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the admitted segment %s: %v", filepath.Base(path), err)
	}
	return content
}
