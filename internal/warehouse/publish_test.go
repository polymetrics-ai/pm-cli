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
	"strconv"
	"testing"
	"time"

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
// verified object whose manifest transaction never committed, and a
// manifest/cursor transaction whose worker was killed while PostgreSQL was
// committing it, so the COMMIT reply never arrived and the outcome was unknown
// to the publication. After every window the registered ledger, the manifest
// revision, and the cursor still agree, and an independent host reads the exact
// registered rows through the other shared gateway without any shared
// filesystem.
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

	t.Run("worker_killed_during_the_manifest_commit_keeps_one_complete_transaction", func(t *testing.T) {
		// V38's third window, driven on its own dataset so the checkpoint
		// history the other windows assert stays exact: the worker publishing a
		// manifest/cursor transaction is killed while PostgreSQL is committing
		// it, so the COMMIT reply never arrives and the publication cannot know
		// whether the transaction committed or rolled back. The publication
		// must reconcile through the immutable occurrence instead of inferring
		// anything from object existence, and the dataset must hold exactly one
		// of the two complete transactions: the prior checkpoint, or the
		// committed new revision whose cursor names the registered row.
		layout, err := warehouse.NewWarehouseLayout(
			harness.fixture.Connection.ConnectorID,
			harness.fixture.Connection.ConnectionID,
			killedCommitRelationName,
		)
		if err != nil {
			t.Fatalf("compose the killed-commit dataset namespace: %v", err)
		}
		harness.registerDataset(t, harness.fixture.Scope, layout)
		// The declared entry points are the seams this window is driven
		// through, as a worker holding a configuration rather than retained
		// instances holds them.
		writeSegment := func(requestID string, rows [][]warehouse.QueryValue) warehouse.SegmentBytes {
			t.Helper()
			segment, err := warehouse.WriteSegment(context.Background(), warehouse.ChildConfig{
				BinaryPath:    harness.childPath,
				ScratchRoot:   harness.scratch,
				AdmittedRoots: []string{harness.materials},
				Limits:        harness.limits,
			}, warehouse.MaterializeRequest{
				RequestID:           requestID,
				AdmittedDirectories: []string{harness.materials},
				OutputPath:          filepath.Join(harness.materials, requestID+".parquet"),
				Columns:             harness.columnCopy(),
				Rows:                rows,
				Bounds:              harness.bounds,
				MaximumBytes:        warehouseMaximumSegment,
			})
			if err != nil {
				t.Fatalf("materialize the segment %s through the declared writer entry point: %v", requestID, err)
			}
			return segment
		}
		publish := func(attemptID string, segment warehouse.SegmentBytes, rows [][]warehouse.QueryValue) (store.WarehouseSegment, error) {
			t.Helper()
			return warehouse.PublishManifest(
				context.Background(), harness.fixture.Pool, harness.publisher, layout, warehouseMaximumSegment,
				warehouse.PublishSegmentRequest{
					Actor:     harness.fixture.Actor,
					Scope:     harness.fixture.Scope,
					AttemptID: attemptID,
					Segment:   segment,
					RowCount:  int64(len(rows)),
				},
			)
		}

		// The prior checkpoint the lost COMMIT must never disturb.
		priorRows := harness.rows[:warehouseSegmentSplit]
		prior, err := publish("killed-commit-prior", writeSegment("killed-commit-prior", priorRows), priorRows)
		if err != nil {
			t.Fatalf("publish the prior checkpoint: %v", err)
		}
		if prior.Ordinal != 1 || prior.ManifestRevision != 1 {
			t.Fatalf("prior publication = ordinal %d at revision %d; want 1/1", prior.Ordinal, prior.ManifestRevision)
		}

		// Kill the publishing backend while PostgreSQL commits the next
		// manifest/cursor transaction.
		fault := harness.injectKilledCommitFault(t, layout)
		lostRows := harness.rows[warehouseSegmentSplit : warehouseSegmentSplit*2]
		lostSegment := writeSegment("killed-commit", lostRows)
		published, err := publish("killed-commit", lostSegment, lostRows)
		if activations := fault.activations(t); activations != 1 {
			t.Fatalf("the injected manifest-commit kill did not fire exactly once: sequence position %d", activations)
		}
		fault.remove(t)

		state := harness.stateFor(t, layout)
		if err == nil {
			t.Logf("the injected kill left the committed occurrence at revision %d", published.ManifestRevision)
			assertStateAgrees(t, state, 2, 2)
			// The kill did not stop the commit: the durable occurrence is the
			// registered row, so the lost reply was reconciled from it rather
			// than re-published.
			if published.Ordinal != 2 || published.ManifestRevision != 2 {
				t.Fatalf("recovered publication = ordinal %d at revision %d; want 2/2", published.Ordinal, published.ManifestRevision)
			}
			registered := state.Segments[1]
			if registered.Object != published.Object || !registered.PublishedAt.Equal(published.PublishedAt) {
				t.Fatalf("the recovered publication is not the committed ledger row: %+v", registered)
			}
			harness.verifyObjectBytes(t, published.Object, mustReadFile(t, lostSegment.Path))
		} else {
			// The kill aborted the commit: the verified bytes are retained as
			// an orphan, and the prior checkpoint is exactly what it was.
			t.Logf("the injected kill aborted the commit and retained an orphan: %v", err)
			var refused *warehouse.PublicationRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("killed publication error = %v; want a retained refused publication", err)
			}
			if !errors.Is(err, store.ErrCommitOutcomeUnknown) {
				t.Fatalf("the lost COMMIT reply was not reconciled as an unknown outcome: %v", err)
			}
			// The publication reconciled through the immutable occurrence: the
			// refusal names the absent occurrence, not only the failed COMMIT,
			// so the unknown outcome was never inferred from object existence.
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("the unknown outcome was refused without consulting the immutable occurrence: %v", err)
			}
			assertStateAgrees(t, state, 1, 1)
			harness.verifyObjectBytes(t, refused.Object, mustReadFile(t, lostSegment.Path))
			reservationID, err := layout.SegmentReservationID("killed-commit")
			if err != nil {
				t.Fatalf("compose the killed-commit reservation identity: %v", err)
			}
			if err := store.WithinScope(context.Background(), harness.fixture.Pool, harness.fixture.Scope, harness.fixture.Actor,
				func(ctx context.Context, tx *store.Transaction) error {
					if _, err := store.LoadArtifact(ctx, tx, refused.Object.ArtifactID); !errors.Is(err, store.ErrNotFound) {
						return fmt.Errorf("a killed publication registered an artifact: %v", err)
					}
					if _, err := store.LoadWarehouseSegmentByArtifact(ctx, tx, refused.Object.ArtifactID); !errors.Is(err, store.ErrNotFound) {
						return fmt.Errorf("a killed publication registered a ledger row: %v", err)
					}
					return nil
				}); err != nil {
				t.Fatal(err)
			}
			// Object existence is never a publication witness.
			if _, err := harness.consumerReader.Recover(context.Background(), harness.fixture.Actor, harness.fixture.Scope, reservationID); err == nil {
				t.Fatal("retained orphan bytes stood in for a committed publication occurrence")
			}
			// The dataset stays publishable: the retry advances exactly one
			// revision onto the prior checkpoint and registers the same rows.
			retried, err := publish("killed-commit-repaired", lostSegment, lostRows)
			if err != nil {
				t.Fatalf("publish after the lost manifest COMMIT: %v", err)
			}
			if retried.Ordinal != 2 || retried.ManifestRevision != 2 {
				t.Fatalf("retry after the lost COMMIT = ordinal %d at revision %d; want 2/2", retried.Ordinal, retried.ManifestRevision)
			}
			state = harness.stateFor(t, layout)
		}
		// Either way the dataset is one complete transaction: the cursor names
		// the last registered row and never runs ahead of the ledger.
		assertStateAgrees(t, state, 2, 2)
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

// assertManifestAgrees confirms that the dataset this harness retains agrees
// with its manifest revision and cursor.
func assertManifestAgrees(t *testing.T, harness *warehouseHarness, revision uint64, cursor uint64) {
	t.Helper()
	assertStateAgrees(t, harness.state(t), revision, cursor)
}

// assertStateAgrees confirms that one committed dataset state describes a
// contiguous dataset: the manifest revision, the registered ledger, and the
// cursor name the same rows, so the cursor never runs ahead of the ledger.
func assertStateAgrees(t *testing.T, state warehouse.DatasetState, revision uint64, cursor uint64) {
	t.Helper()
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

// killedCommitRelationName is the dataset the killed-COMMIT window drives. It
// is a second dataset in the same fixture, so the checkpoint history the other
// windows assert stays exact.
const killedCommitRelationName = "orders_killed_commit"

// killedCommitFault is one installed kill of the backend that commits a
// dataset's manifest/cursor transaction.
type killedCommitFault struct {
	harness  *warehouseHarness
	function string
	trigger  string
	sequence string
}

// injectKilledCommitFault installs a deferred constraint trigger that
// terminates the backend committing the manifest/cursor transaction of one
// dataset. PostgreSQL raises the termination itself while the COMMIT is
// executing, so the publication never receives the COMMIT reply and cannot tell
// whether the transaction committed: this is the window a worker killed during
// the manifest/cursor transaction leaves behind. The trigger is scoped to the
// dataset identity and fires for no other dataset.
func (harness *warehouseHarness) injectKilledCommitFault(t *testing.T, layout warehouse.WarehouseLayout) *killedCommitFault {
	t.Helper()
	// The injected objects are uniquely named, so a stale leftover from an
	// interrupted run can never be mistaken for this case's injection.
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	fault := &killedCommitFault{
		harness:  harness,
		function: "pm.fixture_killed_commit_" + suffix,
		trigger:  "fixture_killed_commit_" + suffix,
		sequence: "pm.fixture_killed_commit_" + suffix + "_evidence",
	}
	// Every component of the dataset identity is a validated layout component,
	// so the rendered identity carries no text that a literal could escape.
	datasetID := layout.DatasetID()
	harness.fixture.SetupSQL(t, harness.fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		for _, statement := range []string{
			`CREATE SEQUENCE ` + fault.sequence,
			`CREATE FUNCTION ` + fault.function + `() RETURNS trigger
				LANGUAGE plpgsql SECURITY INVOKER SET search_path = pg_catalog AS $$
			BEGIN
				IF NEW.dataset_id = '` + datasetID + `' THEN
					PERFORM nextval('` + fault.sequence + `');
					PERFORM pg_terminate_backend(pg_backend_pid());
				END IF;
				RETURN NULL;
			END
			$$`,
			`REVOKE ALL ON FUNCTION ` + fault.function + `() FROM PUBLIC`,
			`GRANT EXECUTE ON FUNCTION ` + fault.function + `() TO ` + store.RuntimeRole,
			`GRANT USAGE ON SEQUENCE ` + fault.sequence + ` TO ` + store.RuntimeRole,
			`CREATE CONSTRAINT TRIGGER ` + fault.trigger + ` AFTER INSERT ON pm.warehouse_segments
				DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION ` + fault.function + `()`,
		} {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return fmt.Errorf("install the killed-commit fault: %w", err)
			}
		}
		return nil
	})
	t.Cleanup(func() { fault.remove(t) })
	return fault
}

// remove drops the installed fault again. It is idempotent, so a case can
// remove it before a retry and the cleanup can remove it once more.
func (fault *killedCommitFault) remove(t *testing.T) {
	t.Helper()
	fault.harness.fixture.SetupSQL(t, fault.harness.fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		for _, statement := range []string{
			`DROP TRIGGER IF EXISTS ` + fault.trigger + ` ON pm.warehouse_segments`,
			`DROP FUNCTION IF EXISTS ` + fault.function + `()`,
			`DROP SEQUENCE IF EXISTS ` + fault.sequence,
		} {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return fmt.Errorf("remove the killed-commit fault: %w", err)
			}
		}
		return nil
	})
}

// activations reports how often the injected trigger fired. Every firing is
// recorded in a sequence, whose position a rolled-back transaction cannot undo,
// so the case can prove the window was actually driven.
func (fault *killedCommitFault) activations(t *testing.T) int64 {
	t.Helper()
	var position int64
	var called bool
	fault.harness.fixture.SetupSQL(t, fault.harness.fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT last_value, is_called FROM `+fault.sequence).Scan(&position, &called)
	})
	if !called {
		return 0
	}
	return position
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
