package sync

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/warehouse"
)

// An ETL page checkpoint is the dataset's acknowledged position. The durable
// truth of that position is PostgreSQL: one page is one manifest revision, so
// the committed manifest revision, the registered segment ordinal and the
// dataset cursor ordinal are the same number, and the registered ledger row at
// that ordinal names exactly one page. Nothing here is a change-data-capture
// position: a poll re-reads what a source currently exposes, so a position is
// the number of pages this protocol has already published, never a change
// sequence that a resuming reader may skip into.
var (
	// ErrNotPollingSource reports a page that did not declare how its source
	// produced it, or declared something other than a poll.
	ErrNotPollingSource = errors.New("an acknowledged ETL page must declare a polling source")
	// ErrCheckpointGap reports an acknowledgement that would leave an
	// unpublished position between the committed prefix and itself.
	ErrCheckpointGap = errors.New("an acknowledged ETL page is not the next position of the committed checkpoint")
	// ErrCheckpointStale reports an acknowledgement for a position the
	// committed prefix already covers whose page is not the committed one.
	ErrCheckpointStale = errors.New("an acknowledged ETL page disagrees with the committed checkpoint")
	// ErrPartialAcknowledgement reports an acknowledgement that does not cover
	// the complete page it names.
	ErrPartialAcknowledgement = errors.New("an acknowledged ETL page does not cover the complete fetched page")
	// ErrDatasetNotCheckpointed reports a dataset whose committed state was not
	// produced by this protocol, so no position can be derived from it.
	ErrDatasetNotCheckpointed = errors.New("the dataset's committed rows are not acknowledged ETL pages")
)

// ETLSemantics is how one ETL source produced a fetched page. Polling is the
// only admitted value: an ordered change stream has a different acknowledgement
// contract, and presenting a poll as one would let a reader treat a re-read
// position as a change sequence.
type ETLSemantics string

// ETLSemanticsPolling declares that the page came from an explicit poll.
const ETLSemanticsPolling ETLSemantics = "polling"

// Checkpoint is the committed ETL position of one dataset. Position, the
// registered segment ordinal and the committed manifest revision are the same
// number by construction, so a reader never has to reconcile three cursors.
type Checkpoint struct {
	Layout warehouse.WarehouseLayout
	// Position is the highest acknowledged page position. Zero means no page
	// has been acknowledged yet.
	Position uint64
	// Rows is the registered row count of the acknowledged page. It is the
	// count this protocol verified, not the count an acknowledgement declared.
	Rows int64
	// Digest is the registered shared-object digest of the acknowledged page.
	Digest string
}

// pageAttemptID is the canonical publication attempt identity of one page
// position. Deriving it from the position alone keeps an acknowledgement
// replayable: the same position always resolves to the same immutable artifact,
// so a lost reply is recovered instead of publishing the page twice.
func pageAttemptID(position uint64) string {
	return "p" + strconv.FormatUint(position, 10)
}

// Load reads the committed checkpoint of this dataset. It refuses to derive a
// position from committed rows that this protocol did not publish, so a dataset
// shared with another writer can never be advanced as if it were a page
// sequence.
func (checkpoint *ETLCheckpoint) Load(ctx context.Context, actor contract.Actor, scope contract.Scope) (Checkpoint, error) {
	if ctx == nil {
		return Checkpoint{}, errors.New("an ETL checkpoint read requires a non-nil context")
	}
	layout := checkpoint.manifest.Layout()
	committed := Checkpoint{Layout: layout}
	err := store.WithinScope(ctx, checkpoint.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		dataset, err := store.LoadWarehouseDataset(ctx, tx, layout.DatasetID())
		if err != nil {
			return err
		}
		if dataset.ConnectorID != layout.ConnectorID || dataset.ConnectionID != layout.ConnectionID {
			return store.ErrScopeMismatch
		}
		manifest, err := store.LoadManifest(ctx, tx, layout.DatasetID())
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if manifest.Revision == 0 {
			return notPageDataset(layout, manifest.Revision, "the committed manifest names no revision")
		}
		cursor, err := store.LoadWarehouseCursor(ctx, tx, layout.DatasetID())
		if errors.Is(err, store.ErrNotFound) {
			return notPageDataset(layout, manifest.Revision, "the committed revision has no acknowledged cursor")
		}
		if err != nil {
			return err
		}
		if cursor.Ordinal != manifest.Revision {
			return notPageDataset(layout, manifest.Revision,
				"the dataset cursor is at ordinal "+strconv.FormatUint(cursor.Ordinal, 10))
		}
		segment, err := store.LoadWarehouseSegmentAt(ctx, tx, layout.DatasetID(), cursor.Ordinal)
		if err != nil {
			return err
		}
		if err := requirePageSegment(layout, segment); err != nil {
			return err
		}
		if segment.Object.SHA256 != manifest.Object.SHA256 {
			return notPageDataset(layout, manifest.Revision,
				"the committed manifest object is not the registered page object")
		}
		committed.Position = cursor.Ordinal
		committed.Rows = segment.RowCount
		committed.Digest = segment.Object.SHA256
		return nil
	})
	if err != nil {
		return Checkpoint{}, err
	}
	return committed, nil
}

// requirePageSegment refuses a registered row that is not this protocol's page
// for its own ordinal.
func requirePageSegment(layout warehouse.WarehouseLayout, segment store.WarehouseSegment) error {
	expected, err := layout.SegmentArtifactID(pageAttemptID(segment.Ordinal))
	if err != nil {
		return err
	}
	if segment.ArtifactID != expected {
		return notPageDataset(layout, segment.Ordinal,
			"the registered segment at that ordinal was not published as an ETL page")
	}
	if segment.ManifestRevision != segment.Ordinal {
		return notPageDataset(layout, segment.Ordinal,
			"the registered segment spans a different manifest revision")
	}
	return nil
}

func notPageDataset(layout warehouse.WarehouseLayout, position uint64, detail string) error {
	return fmt.Errorf("%w: dataset %q at position %d: %s", ErrDatasetNotCheckpointed, layout.DatasetID(), position, detail)
}
