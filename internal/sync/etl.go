package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/warehouse"
)

// ETLCheckpoint turns one fetched ETL page into verified immutable bytes and
// then, in one manifest transaction, into a registered segment plus its cursor
// advance. The order is the whole point of the type: a page is only ever
// acknowledged after its exact bytes are durably readable through the shared
// object store, and the cursor only ever moves to a registered revision. A
// crash before the transaction leaves the previous checkpoint and, at most, an
// unregistered retained object, the reservation bound to it, and the admitted
// scratch of the attempt that died; the scratch and the reservation both name
// the attempt that made them, so a newer attempt at the same page is never
// refused by either, and a crash after the transaction leaves the complete next
// checkpoint. Nothing in between is observable.
type ETLCheckpoint struct {
	pool                *store.Pool
	manifest            *warehouse.DatasetManifest
	child               *warehouse.QueryChild
	admittedDirectory   string
	maximumSegmentBytes int64
	bounds              warehouse.QueryBounds
}

// NewETLCheckpoint validates the publication surface before any object, child,
// or database work. The admitted directory is the only place this protocol
// materializes a page, and every bound is required rather than defaulted.
func NewETLCheckpoint(
	pool *store.Pool,
	manifest *warehouse.DatasetManifest,
	child *warehouse.QueryChild,
	admittedDirectory string,
	maximumSegmentBytes int64,
	bounds warehouse.QueryBounds,
) (*ETLCheckpoint, error) {
	if pool == nil || manifest == nil || child == nil {
		return nil, errors.New("an ETL checkpoint requires a store pool, a dataset manifest, and a query child supervisor")
	}
	if admittedDirectory == "" || !filepath.IsAbs(admittedDirectory) || filepath.Clean(admittedDirectory) != admittedDirectory {
		return nil, errors.New("the admitted page directory must be one absolute, cleaned path")
	}
	info, err := os.Lstat(admittedDirectory)
	if err != nil {
		return nil, fmt.Errorf("inspect the admitted page directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("the admitted page directory must be a directory, not a symlink")
	}
	if maximumSegmentBytes <= 0 {
		return nil, errors.New("an ETL checkpoint requires a positive segment byte cap")
	}
	if err := bounds.Validate(); err != nil {
		return nil, err
	}
	return &ETLCheckpoint{
		pool:                pool,
		manifest:            manifest,
		child:               child,
		admittedDirectory:   admittedDirectory,
		maximumSegmentBytes: maximumSegmentBytes,
		bounds:              bounds,
	}, nil
}

// PageRequest is one fetched ETL page proposed for acknowledgement.
type PageRequest struct {
	Actor contract.Actor
	Scope contract.Scope
	// Semantics is how the source produced this page. An acknowledgement must
	// declare it, and only polling is admitted.
	Semantics ETLSemantics
	// Position is the one-based page position this acknowledgement names.
	Position uint64
	// Rows is the complete typed row set the fetch returned for this page.
	Rows [][]warehouse.QueryValue
	// Columns is the page's declared column schema.
	Columns []warehouse.QueryColumn
	// DeclaredRows is how many rows this acknowledgement covers. It must equal
	// the page's complete row count: an acknowledgement that covers less would
	// otherwise advance the checkpoint over rows that were never published.
	DeclaredRows int64
	// RequestID names this materialization attempt. It is unique per attempt:
	// it is the attempt's own admitted scratch identity and physical upload
	// identity, an immutable admitted segment is never overwritten by a second
	// attempt, and a reservation is never bound to a second upload. A page
	// re-driven after a crash therefore declares a new identity of its own
	// rather than reusing the dead attempt's.
	RequestID string
}

// CommitPage acknowledges one page. It refuses anything that is not the exact
// successor of the committed checkpoint, materializes the complete page through
// the restricted child, publishes the verified bytes, and returns the
// checkpoint that the single manifest transaction committed.
func (checkpoint *ETLCheckpoint) CommitPage(ctx context.Context, request PageRequest) (Checkpoint, error) {
	if ctx == nil {
		return Checkpoint{}, errors.New("an ETL page acknowledgement requires a non-nil context")
	}
	if request.Semantics != ETLSemanticsPolling {
		return Checkpoint{}, fmt.Errorf("%w: the acknowledgement declared %q",
			ErrNotPollingSource, string(request.Semantics))
	}
	if request.Position == 0 {
		return Checkpoint{}, errors.New("an ETL page acknowledgement requires a position of at least one")
	}
	if request.RequestID == "" {
		return Checkpoint{}, errors.New("an ETL page acknowledgement requires a materialization identity")
	}
	total := int64(len(request.Rows))
	switch {
	case total == 0:
		return Checkpoint{}, fmt.Errorf("%w: page %d carries no row", ErrPartialAcknowledgement, request.Position)
	case request.DeclaredRows != total:
		return Checkpoint{}, fmt.Errorf("%w: page %d acknowledges %d of its %d rows",
			ErrPartialAcknowledgement, request.Position, request.DeclaredRows, total)
	}
	schema, err := warehouse.NewSegmentSchema(request.Columns)
	if err != nil {
		return Checkpoint{}, err
	}
	committed, err := checkpoint.Load(ctx, request.Actor, request.Scope)
	if err != nil {
		return Checkpoint{}, err
	}
	if request.Position > committed.Position+1 {
		return Checkpoint{}, fmt.Errorf("%w: page %d follows the committed position %d",
			ErrCheckpointGap, request.Position, committed.Position)
	}
	if request.Position < committed.Position {
		// The acknowledged position is already covered by the committed prefix.
		// Its registered segment must be this protocol's page for that position,
		// so a superseded acknowledgement can never re-describe it.
		segment, err := checkpoint.manifest.SegmentAt(ctx, request.Actor, request.Scope, request.Position)
		if err != nil {
			return Checkpoint{}, err
		}
		if err := requirePageSegment(committed.Layout, segment); err != nil {
			return Checkpoint{}, fmt.Errorf("%w: %v", ErrCheckpointStale, err)
		}
	}
	published, err := checkpoint.publish(ctx, request, schema, committed.Position)
	if err != nil {
		return Checkpoint{}, err
	}
	if published.Ordinal != request.Position {
		return Checkpoint{}, fmt.Errorf("%w: page %d was registered as ordinal %d",
			ErrDatasetNotCheckpointed, request.Position, published.Ordinal)
	}
	if request.Position <= committed.Position {
		// An exact replay of an acknowledged page is idempotent, and a replay of
		// a superseded position never rewinds the checkpoint.
		return committed, nil
	}
	return Checkpoint{
		Layout:   committed.Layout,
		Position: published.Ordinal,
		Rows:     published.RowCount,
		Digest:   published.Object.SHA256,
	}, nil
}

// publish materializes the complete page and registers the verified bytes. The
// manifest compare-and-swap advances only from the committed revision this
// acknowledgement observed, so a stale acknowledgement loses the race instead
// of publishing its page over a newer one, and the registered row and the
// cursor advance are committed by one transaction. An acknowledgement that
// already resolved to a registered page never reaches the compare-and-swap.
//
// The admitted scratch file carries this attempt's own identity, so the file a
// process that died after materializing left behind refuses nothing the next
// attempt at the same page does. The physical upload carries the same attempt
// identity: a reservation stays bound to the first upload made under it, so a
// process that died after its upload was bound or verified but before the
// manifest transaction committed leaves a reservation the next attempt never
// reuses. That attempt uploads under its own reservation and supersedes the
// dead upload, which stays retained and unregistered. The registered segment
// identity stays position-derived, so a replay of the page still resolves to
// the one registered occurrence and the page is published at most once.
func (checkpoint *ETLCheckpoint) publish(
	ctx context.Context,
	request PageRequest,
	schema warehouse.SegmentSchema,
	baseRevision uint64,
) (store.WarehouseSegment, error) {
	segment, err := warehouse.MaterializeParquetSegment(ctx, checkpoint.child, warehouse.MaterializeRequest{
		RequestID:           request.RequestID,
		AdmittedDirectories: []string{checkpoint.admittedDirectory},
		OutputPath: filepath.Join(
			checkpoint.admittedDirectory,
			pageScratchName(request.Position, request.RequestID),
		),
		Columns:      schema.Columns(),
		Rows:         request.Rows,
		Bounds:       checkpoint.bounds,
		MaximumBytes: checkpoint.maximumSegmentBytes,
	})
	if err != nil {
		return store.WarehouseSegment{}, err
	}
	// The admitted segment is disposable scratch: the bytes that matter are the
	// ones the shared object store now holds, and an immutable admitted file is
	// never rewritten.
	defer func() { _ = os.Remove(segment.Path) }()
	published, err := checkpoint.manifest.PublishSegment(ctx, warehouse.PublishSegmentRequest{
		Actor:        request.Actor,
		Scope:        request.Scope,
		AttemptID:    pageAttemptID(request.Position),
		UploadID:     pageUploadID(request.RequestID),
		Segment:      segment,
		RowCount:     int64(len(request.Rows)),
		BaseRevision: baseRevision,
	})
	if err != nil {
		if errors.Is(err, store.ErrCASConflict) || errors.Is(err, store.ErrImmutableConflict) {
			// Either the committed revision moved since this acknowledgement
			// read it, or the position already holds a different page. Both are
			// a stale acknowledgement: the retained bytes stay unregistered and
			// the cursor does not move.
			return store.WarehouseSegment{}, fmt.Errorf("%w: %w", ErrCheckpointStale, err)
		}
		return store.WarehouseSegment{}, err
	}
	return published, nil
}

const (
	// maximumScratchTokenBytes bounds the attempt-identity part of one admitted
	// scratch name, so the whole name stays inside one file-system component
	// whatever identity an attempt declares.
	maximumScratchTokenBytes = 48
	// scratchDigestBytes is how much of an identity digest binds a sanitized or
	// truncated token to the exact attempt identity it was derived from. Two
	// identities can only share such a token by colliding over these bytes, so
	// the whole digest the name has room for is kept.
	scratchDigestBytes = 16
	// scratchDigestSeparator joins a sanitized or truncated rendering to its
	// identity digest. isScratchCharacter excludes it, so no identity the
	// verbatim rendering admits can spell a digest-bearing token: the two
	// renderings of scratchAttemptToken stay disjoint whatever identity an
	// attempt declares.
	scratchDigestSeparator = "+"
)

// pageUploadID is the physical upload identity of one materialization attempt:
// a bounded digest of the attempt's own identity in the structural alphabet the
// warehouse layout admits, so two attempts at one page never share a
// reservation whatever identities they declare.
func pageUploadID(requestID string) string {
	digest := sha256.Sum256([]byte(requestID))
	return "u" + hex.EncodeToString(digest[:scratchDigestBytes])
}

// pageScratchName is the admitted scratch identity of one materialization
// attempt: the page position this attempt materializes plus the attempt's own
// identity. The position keeps the file recognizable, and the attempt identity
// keeps two attempts at one position from sharing a name, so the scratch file a
// dead attempt left behind refuses nothing the next attempt at that page does.
// The registered segment identity stays position-derived (pageAttemptID), so a
// replay still resolves to the one registered occurrence.
func pageScratchName(position uint64, requestID string) string {
	return pageAttemptID(position) + "-" + scratchAttemptToken(requestID) + ".parquet"
}

// scratchAttemptToken renders one attempt identity as one bounded file-name
// token. An identity that already is one run of admitted characters is kept
// exactly as declared, so an operator can attribute a leftover scratch file to
// the attempt that wrote it. Every other identity is sanitized into that
// alphabet, bounded to the same length, and carries a digest of the exact
// identity after a separator the admitted alphabet excludes, so sanitizing can
// never collapse two attempts onto one name, no admitted identity can spell a
// digest-bearing token of another attempt, and no identity can reach outside
// the admitted directory.
func scratchAttemptToken(requestID string) string {
	token := make([]byte, 0, min(len(requestID), maximumScratchTokenBytes))
	for _, character := range requestID {
		if len(token) == maximumScratchTokenBytes {
			break
		}
		if isScratchCharacter(character) {
			token = append(token, byte(character))
			continue
		}
		token = append(token, '_')
	}
	if string(token) == requestID {
		return string(token)
	}
	digest := sha256.Sum256([]byte(requestID))
	return string(token) + scratchDigestSeparator + hex.EncodeToString(digest[:scratchDigestBytes])
}

// isScratchCharacter reports whether one character may appear verbatim in an
// admitted scratch name: the identifier alphabet of a declared attempt identity
// plus the dot that keeps a name readable.
func isScratchCharacter(character rune) bool {
	switch {
	case character >= 'a' && character <= 'z',
		character >= 'A' && character <= 'Z',
		character >= '0' && character <= '9',
		character == '-', character == '_', character == '.':
		return true
	default:
		return false
	}
}
