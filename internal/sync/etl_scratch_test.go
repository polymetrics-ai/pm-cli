package sync_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/warehouse"
)

// This file owns the crashed-attempt scratch case. The page protocol has one
// window it cannot close in-process: a process that materializes a page and then
// dies before its own cleanup leaves the admitted scratch file behind. A page
// must still be acknowledgeable after that: the leftover names the attempt that
// wrote it, so it refuses nothing the next attempt at the same position does,
// and the registered segment identity stays position-derived, so the page is
// published exactly once and a replay resolves to that one occurrence.
//
// The crash window is driven deterministically rather than by killing a
// process: the bytes a dead attempt would have left are planted at the exact
// admitted path the naming rule derives for that attempt.

// The crafted identity pair keeps the naming rule honest about whose file a
// leftover is. craftedAttemptIdentity is composed from slashAttemptIdentity the
// way the rule before the digest separator moved outside the admitted alphabet
// spelled a sanitized rendering — the sanitized token, a hyphen, and the first
// eight bytes of the identity's own digest (sha256("a/b") begins
// c14cddc033f64b9d) — and every character of the result is admitted, so that
// rule rendered the crafted identity verbatim. Both identities then derived one
// admitted path, and a leftover of either refused the other: the very refusal
// the per-attempt naming exists to remove. The digests below are fixture
// constants of that attack, not a second copy of the naming rule.
const (
	slashAttemptIdentity   = "a/b"
	craftedAttemptIdentity = "a_b-c14cddc033f64b9d"
)

// TestPageAcknowledgedAfterCrashedAttemptScratch is the crashed-attempt case.
// Its cases share one harness but not one precondition: each later case reads
// the committed checkpoint for itself, so a failure in an earlier one cannot
// decide what the next observes.
func TestPageAcknowledgedAfterCrashedAttemptScratch(t *testing.T) {
	harness := newETLHarness(t)
	harness.commit(t, harness.page(1))

	t.Run("a_leftover_scratch_from_a_dead_attempt_never_refuses_its_page", func(t *testing.T) {
		// The crash window, twice over: the per-attempt scratch file a dead
		// attempt of this build leaves behind, and the position-only name an
		// attempt predating that naming left behind. Neither belongs to the
		// attempt that acknowledges the page now, so neither may refuse it.
		plantedAttemptScratch(t, harness, 2, "page-2-interrupted")
		plantedLegacyScratch(t, harness, 2)

		committed := harness.commit(t, harness.page(2))
		if committed.Position != 2 || committed.Rows != etlPageRows {
			t.Fatalf("page acknowledged after a dead attempt = position %d over %d rows; want position 2 over %d rows",
				committed.Position, committed.Rows, etlPageRows)
		}
		// Exactly one registration for the position, under the identity a
		// replay resolves to.
		harness.assertPrefix(t, 2)
		requirePageArtifact(t, harness, 2)

		// The acknowledgement is replayable in the ordinary way: the same page
		// resolves to the registered occurrence and registers nothing a second
		// time.
		replayed := harness.commit(t, harness.page(2))
		if replayed.Position != 2 || replayed.Digest != committed.Digest {
			t.Fatalf("replay after recovery = position %d at %q; want the registered position 2 at %q",
				replayed.Position, replayed.Digest, committed.Digest)
		}
		harness.assertPrefix(t, 2)
		requirePageArtifact(t, harness, 2)
	})

	t.Run("the_admitted_scratch_name_belongs_to_one_attempt", func(t *testing.T) {
		// The case above relies on the naming rule, so this case observes the
		// rule instead of assuming it: a file planted at the name the next
		// attempt derives for itself makes exactly that attempt refuse, which
		// proves the plant lands on the path the protocol uses. An attempt that
		// never materialized registers nothing and leaves the prefix alone.
		committed, err := harness.checkpoint.Load(t.Context(), harness.fixture.Actor, harness.fixture.Scope)
		if err != nil {
			t.Fatalf("load the committed checkpoint: %v", err)
		}
		live := harness.page(committed.Position + 1)
		plantedAttemptScratch(t, harness, live.Position, live.RequestID)
		if _, attemptErr := harness.attempt(live); !isUnadmittedPathRefusal(attemptErr) {
			t.Fatalf("attempt whose own scratch already exists = %v; want an unadmitted-path refusal", attemptErr)
		}
		harness.assertPrefix(t, committed.Position)
	})

	t.Run("a_crafted_identity_cannot_take_another_attempts_admitted_name", func(t *testing.T) {
		// The naming rule must keep every attempt identity apart, not only the
		// ones a page protocol ordinarily declares. The crafted identity above is
		// planted first, and the plant is observed before the other identity
		// attempts the page: the attempt that declares the crafted identity is
		// refused by its own leftover, which proves the plant landed on the path
		// the protocol derives for it, and only then does the attempt that
		// declares the identity the crafted one was built from have to be
		// admitted. Both identities derived this one path before the digest
		// separator moved outside the admitted alphabet, so the second attempt
		// was refused by the first identity's leftover.
		committed, err := harness.checkpoint.Load(t.Context(), harness.fixture.Actor, harness.fixture.Scope)
		if err != nil {
			t.Fatalf("load the committed checkpoint: %v", err)
		}
		position := committed.Position + 1
		plantedAttemptScratch(t, harness, position, craftedAttemptIdentity)

		crafted := harness.page(position)
		crafted.RequestID = craftedAttemptIdentity
		if _, craftedErr := harness.attempt(crafted); !isUnadmittedPathRefusal(craftedErr) {
			t.Fatalf("attempt that declares the identity whose own scratch is planted = %v; want an unadmitted-path refusal", craftedErr)
		}

		slash := harness.page(position)
		slash.RequestID = slashAttemptIdentity
		acknowledged := harness.commit(t, slash)
		if acknowledged.Position != position || acknowledged.Rows != etlPageRows {
			t.Fatalf("attempt beside another identity's leftover = position %d over %d rows; want position %d over %d rows",
				acknowledged.Position, acknowledged.Rows, position, etlPageRows)
		}
		harness.assertPrefix(t, position)
		requirePageArtifact(t, harness, position)
	})
}

// TestPageAcknowledgedAfterUploadedUncommittedAttempt is the crash window after
// a page's verified bytes reached the shared store but before its manifest
// transaction committed. The dead attempt leaves a reservation permanently bound
// to its own physical upload, so the next attempt at the same page must
// supersede it under its own upload identity rather than collide with that
// binding, while the cursor stays at the committed prefix until the retry
// commits and the page is still published exactly once under its
// position-derived identity.
//
// The window is driven for real: the dead attempt runs the whole protocol
// through materialization, reservation, binding, upload and readback
// verification, and only its manifest transaction is refused by a trigger
// scoped to this dataset, so the retained bytes are exactly the ones a process
// that died before COMMIT leaves behind.
func TestPageAcknowledgedAfterUploadedUncommittedAttempt(t *testing.T) {
	harness := newETLHarness(t)
	harness.commit(t, harness.page(1))

	fault := installUncommittedPublicationFault(t, harness)
	dead := harness.page(2)
	dead.RequestID = "page-2-uncommitted"
	_, err := harness.attempt(dead)
	var refused *warehouse.PublicationRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("attempt whose manifest transaction is refused = %v; want verified bytes retained unregistered", err)
	}
	if err := harness.consumer.Backend.Verify(context.Background(), refused.Object); err != nil {
		t.Fatalf("the dead attempt's uploaded bytes are not verified in the shared store: %v", err)
	}
	// Nothing about the dead attempt is visible: the cursor and the ledger are
	// the committed prefix.
	harness.assertPrefix(t, 1)
	fault.remove(t)

	retried := harness.commit(t, harness.page(2))
	if retried.Position != 2 || retried.Rows != etlPageRows {
		t.Fatalf("retry after an uploaded but uncommitted attempt = position %d over %d rows; want position 2 over %d rows",
			retried.Position, retried.Rows, etlPageRows)
	}
	harness.assertPrefix(t, 2)
	requirePageArtifact(t, harness, 2)

	// The acknowledgement stays replayable: the same page resolves to the one
	// registered occurrence and registers nothing a second time.
	replayed := harness.commit(t, harness.page(2))
	if replayed.Position != 2 || replayed.Digest != retried.Digest {
		t.Fatalf("replay after recovery = position %d at %q; want the registered position 2 at %q",
			replayed.Position, replayed.Digest, retried.Digest)
	}
	harness.assertPrefix(t, 2)
	requirePageArtifact(t, harness, 2)
}

// uncommittedPublicationFault is one installed refusal of the manifest
// transaction that registers a segment of the harness dataset.
type uncommittedPublicationFault struct {
	harness  *etlHarness
	function string
	trigger  string
}

// installUncommittedPublicationFault refuses, inside its own transaction, every
// segment registration of the harness dataset. The publication has already
// reserved, bound, uploaded and verified its bytes when the registration runs,
// so the refusal leaves exactly the state of a worker that died before COMMIT.
func installUncommittedPublicationFault(t *testing.T, harness *etlHarness) *uncommittedPublicationFault {
	t.Helper()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	fault := &uncommittedPublicationFault{
		harness:  harness,
		function: "pm.fixture_uncommitted_publication_" + suffix,
		trigger:  "fixture_uncommitted_publication_" + suffix,
	}
	// Every component of the dataset identity is a validated layout component,
	// so the rendered identity carries no text that a literal could escape.
	datasetID := harness.layout.DatasetID()
	harness.fixture.SetupSQL(t, harness.fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		for _, statement := range []string{
			`CREATE FUNCTION ` + fault.function + `() RETURNS trigger
				LANGUAGE plpgsql SECURITY INVOKER SET search_path = pg_catalog AS $$
			BEGIN
				IF NEW.dataset_id = '` + datasetID + `' THEN
					RAISE EXCEPTION 'fixture: the manifest transaction died before COMMIT';
				END IF;
				RETURN NEW;
			END
			$$`,
			`REVOKE ALL ON FUNCTION ` + fault.function + `() FROM PUBLIC`,
			`GRANT EXECUTE ON FUNCTION ` + fault.function + `() TO ` + store.RuntimeRole,
			`CREATE TRIGGER ` + fault.trigger + ` AFTER INSERT ON pm.warehouse_segments
				FOR EACH ROW EXECUTE FUNCTION ` + fault.function + `()`,
		} {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return fmt.Errorf("install the uncommitted-publication fault: %w", err)
			}
		}
		return nil
	})
	t.Cleanup(func() { fault.remove(t) })
	return fault
}

// remove drops the installed fault again. It is idempotent, so the case can
// remove it before the retry and the cleanup can remove it once more.
func (fault *uncommittedPublicationFault) remove(t *testing.T) {
	t.Helper()
	fault.harness.fixture.SetupSQL(t, fault.harness.fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		for _, statement := range []string{
			`DROP TRIGGER IF EXISTS ` + fault.trigger + ` ON pm.warehouse_segments`,
			`DROP FUNCTION IF EXISTS ` + fault.function + `()`,
		} {
			if _, err := tx.Exec(ctx, statement); err != nil {
				return fmt.Errorf("remove the uncommitted-publication fault: %w", err)
			}
		}
		return nil
	})
}

// isUnadmittedPathRefusal reports whether one refusal is the supervisor's own
// unadmitted-path refusal, the state a second attempt at a live attempt's
// scratch file is refused with.
func isUnadmittedPathRefusal(err error) bool {
	var refusal *warehouse.Refusal
	return errors.As(err, &refusal) && refusal.Code == warehouse.RefusalUnadmittedPath
}

// plantedAttemptScratch writes the admitted scratch file one attempt derives
// for itself: the page position the attempt materializes plus the attempt's own
// identity.
func plantedAttemptScratch(t *testing.T, harness *etlHarness, position uint64, requestID string) string {
	t.Helper()
	return plantAdmittedScratch(t, harness, fmt.Sprintf("p%d-%s.parquet", position, requestID))
}

// plantedLegacyScratch writes the position-only scratch name the protocol
// derived before the attempt identity was part of it. A dead attempt that
// predates that correction leaves exactly this file.
func plantedLegacyScratch(t *testing.T, harness *etlHarness, position uint64) string {
	t.Helper()
	return plantAdmittedScratch(t, harness, fmt.Sprintf("p%d.parquet", position))
}

// plantAdmittedScratch materializes the bytes a dead attempt's materialization
// would have left: one immutable admitted file no later attempt may overwrite.
func plantAdmittedScratch(t *testing.T, harness *etlHarness, name string) string {
	t.Helper()
	path := filepath.Join(harness.materials, name)
	if err := os.WriteFile(path, []byte("crashed attempt scratch"), 0o600); err != nil {
		t.Fatalf("plant the dead attempt's admitted scratch %s: %v", name, err)
	}
	return path
}

// requirePageArtifact requires the registered segment of one acknowledged
// position to carry the position-derived artifact identity, which is what makes
// the acknowledgement idempotent.
func requirePageArtifact(t *testing.T, harness *etlHarness, position uint64) {
	t.Helper()
	state, err := harness.manifest.State(t.Context(), harness.fixture.Actor, harness.fixture.Scope)
	if err != nil {
		t.Fatalf("load the committed dataset state: %v", err)
	}
	wanted, err := harness.layout.SegmentArtifactID(fmt.Sprintf("p%d", position))
	if err != nil {
		t.Fatalf("compose the position-derived artifact identity: %v", err)
	}
	registered := 0
	for _, segment := range state.Segments {
		if segment.Ordinal != position {
			continue
		}
		registered++
		if segment.ArtifactID != wanted {
			t.Fatalf("registered segment %d carries artifact %q; want the position-derived %q",
				position, segment.ArtifactID, wanted)
		}
		if segment.RowCount != etlPageRows {
			t.Fatalf("registered segment %d covers %d rows; want %d", position, segment.RowCount, etlPageRows)
		}
	}
	if registered != 1 {
		t.Fatalf("position %d holds %d registered segments; want exactly one", position, registered)
	}
}
