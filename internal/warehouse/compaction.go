package warehouse

import (
	"context"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// CompactionRequest compacts the live registered segments of one dataset
// through one revision into a single new immutable segment.
type CompactionRequest struct {
	Actor contract.Actor
	Scope contract.Scope
	// ThroughRevision is the manifest revision the compaction was computed from.
	// The publication advances only from that exact revision, so a competing
	// publication is refused instead of being silently overwritten.
	ThroughRevision uint64
	AttemptID       string
	Bounds          QueryBounds
}

// CompactionResult reports the new live segment beside what happened to the
// segments it replaced: a segment named by any retention holder is retained, and
// only an unreferenced segment is collected.
type CompactionResult struct {
	Published  store.WarehouseSegment
	Superseded []uint64
	Collected  []uint64
	Retained   []store.WarehouseRetainedSegment
	Rows       int64
}

// CompactDatasetSegments rewrites the live registered segments at one revision
// into one new immutable object, advances the manifest by compare-and-swap, and
// retires the sources in the same transaction. The retirement is reachability
// aware: a segment a query snapshot, plan, proof, backup, or reservation names is
// never collected.
func CompactDatasetSegments(
	ctx context.Context,
	manifest *DatasetManifest,
	service *QueryService,
	request CompactionRequest,
) (CompactionResult, error) {
	var result CompactionResult
	if ctx == nil || manifest == nil || service == nil {
		return result, refusal(RefusalProtocol, "compaction requires a context, a dataset manifest, and a query service")
	}
	if request.ThroughRevision == 0 {
		return result, refusal(RefusalProtocol, "compaction requires the manifest revision it was computed from")
	}
	if err := request.Bounds.Validate(); err != nil {
		return result, err
	}
	state, err := manifest.State(ctx, request.Actor, request.Scope)
	if err != nil {
		return result, err
	}
	sources := make([]store.WarehouseSegment, 0, len(state.Live))
	for _, segment := range state.Segments {
		if segment.State != store.WarehouseSegmentLive || segment.Ordinal > request.ThroughRevision {
			continue
		}
		sources = append(sources, segment)
	}
	if len(sources) == 0 {
		return result, refusal(RefusalUnadmittedPath,
			"dataset %q has no live registered segment at revision %d to compact",
			manifest.layout.DatasetID(), request.ThroughRevision)
	}
	fingerprint := sources[0].ColumnFingerprint
	segments := make([]QuerySnapshotSegment, 0, len(sources))
	ordinals := make([]uint64, 0, len(sources))
	for _, source := range sources {
		if source.ColumnFingerprint != fingerprint {
			return result, refusal(RefusalSchemaMismatch, "the compaction sources disagree on the dataset schema")
		}
		schema, err := DecodeSegmentSchema(source.Schema)
		if err != nil {
			return result, err
		}
		segments = append(segments, QuerySnapshotSegment{
			Ordinal: source.Ordinal, Object: source.Object, Schema: schema, Rows: source.RowCount,
		})
		ordinals = append(ordinals, source.Ordinal)
	}
	columns := segments[0].Schema.Columns()

	// Read the exact source rows: the whole compacted object must be the exact
	// union of the segments it replaces, so a truncated read is refused rather
	// than published as a smaller dataset.
	read, err := service.Read(ctx, ReadRequest{
		Actor:   request.Actor,
		Scope:   request.Scope,
		Layout:  manifest.layout,
		QueryID: compactionQueryID(request.AttemptID),
		Select: QuerySelect{
			From:    manifest.layout.RelationName(),
			Project: allColumnProjections(columns),
			Limit:   request.Bounds.Rows,
		},
		Segments:            segments,
		MaximumSegmentBytes: manifest.maximumSegmentBytes,
		Bounds:              request.Bounds,
	})
	if err != nil {
		return result, err
	}
	if read.Truncated {
		return result, refusal(RefusalBoundExceeded,
			"the compaction source rows exceed the declared row bound and were not fully read")
	}

	compacted, cleanup, err := service.materializeSegment(
		ctx, compactionMaterializeID(request.AttemptID), columns, read.Rows, request.Bounds, manifest.maximumSegmentBytes,
	)
	if err != nil {
		return result, err
	}
	defer cleanup()

	var retirement store.WarehouseRetirement
	published, err := manifest.PublishSegment(ctx, PublishSegmentRequest{
		Actor:        request.Actor,
		Scope:        request.Scope,
		AttemptID:    request.AttemptID,
		Segment:      compacted,
		RowCount:     int64(len(read.Rows)),
		BaseRevision: request.ThroughRevision,
		JoinPublication: func(ctx context.Context, tx *store.Transaction) error {
			retired, err := store.RetireWarehouseSegments(ctx, tx, manifest.layout.DatasetID(), ordinals)
			if err != nil {
				return err
			}
			retirement = retired
			return nil
		},
	})
	if err != nil {
		return result, err
	}
	result.Published = published
	result.Superseded = ordinals
	result.Collected = retirement.Collected
	result.Retained = retirement.Retained
	result.Rows = int64(len(read.Rows))
	if result.Collected == nil {
		result.Collected = []uint64{}
	}
	if result.Retained == nil {
		result.Retained = []store.WarehouseRetainedSegment{}
	}
	return result, nil
}

// compactionQueryID keeps one compaction's staged relations separate from every
// other read on this host.
func compactionQueryID(attemptID string) string { return "compaction-" + attemptID }

// compactionMaterializeID keeps the compacted output directory separate too.
func compactionMaterializeID(attemptID string) string { return "compacted-" + attemptID }
