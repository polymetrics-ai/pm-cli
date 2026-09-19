package sync_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

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

// TestPageAcknowledgedAfterCrashedAttemptScratch is the crashed-attempt case.
// Its two cases share one harness but not one precondition: the second case
// reads the committed checkpoint for itself, so a failure in the first cannot
// decide what the second observes.
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
