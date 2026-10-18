package warehouse_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
	"github.com/polymetrics-ai/pm-cli/internal/warehouse"
)

// This file owns the shared physical fixture for the warehouse acceptance cases
// (V37, V38, V57) and the V37 case itself. Every case runs the real restricted
// child, the real selected shared-object gateways, and real PostgreSQL through
// the owned fixture: recorded Parquet segments are produced by the child,
// streamed into the shared immutable object store, registered through a
// transactional manifest, and read back by a query that never sees a shared
// writable file.
const (
	warehouseRelationName = "orders"
	warehouseSegmentRows  = 8
	warehouseSegmentSplit = 3
	// warehouseMaximumSegment is the largest segment this fixture admits. It
	// matches the largest declared byte bound the child's own document
	// capability can carry, so no admitted segment is ever refused later.
	warehouseMaximumSegment = warehouse.MaxQueryDocumentBytes - 2*warehouseResponseEnvelopeSlack
)

// The fixture mirrors the two package constants that bound one declared
// document, because the external test package cannot reach the package's own
// unexported values. warehouseResponseEnvelopeSlack is the document envelope
// the supervisor reserves above a declared payload byte bound, and
// warehouseMaximumResponseCap is the largest response byte cap it can honour:
// the supervisor reads a declared response cap plus the envelope, and the
// protocol codec refuses any document above warehouse.MaxQueryDocumentBytes, so
// a larger cap would admit an attempt whose typed document the codec refuses
// after the child has run.
//
// Both are probe magnitudes, not a private copy of the package's rule: every
// case that uses them asserts the supervisor's own behaviour on both sides of
// the boundary (the largest honourable configuration is accepted and executes,
// one byte more is refused by admission), so a moved envelope fails the case
// instead of quietly probing a different point.
const (
	warehouseResponseEnvelopeSlack = 1 << 16
	warehouseMaximumResponseCap    = warehouse.MaxQueryDocumentBytes - warehouseResponseEnvelopeSlack
)

// allColumnProjections declares one projection per declared column, in the
// declared order: the fixture's queries read the whole relation.
func allColumnProjections(columns []warehouse.QueryColumn) []warehouse.QueryProjection {
	projections := make([]warehouse.QueryProjection, 0, len(columns))
	for _, column := range columns {
		projections = append(projections, warehouse.QueryProjection{Column: column.Name})
	}
	return projections
}

// warehouseQueryChildPath builds the restricted child for one case. The build
// helper writes into that case's own temporary directory, so every acceptance
// case owns its child binary life cycle.
func warehouseQueryChildPath(t *testing.T) string {
	t.Helper()
	path := buildRestrictedQueryChild(t)
	if path == "" {
		t.Fatal("the restricted query child binary is unavailable for this test binary")
	}
	return path
}

// warehouseHarness owns only what this slice needs from the selected stack: the
// PostgreSQL fixture, both selected artifact gateways (producer and independent
// consumer), the real restricted query child, and the owner-only admitted and
// scratch directories the child may read and write.
type warehouseHarness struct {
	t              *testing.T
	fixture        *testfixture.Fixture
	producer       *app.ArtifactResources
	consumer       *app.ArtifactResources
	publisher      *objects.Publisher
	manifest       *warehouse.DatasetManifest
	consumerReader *objects.Publisher
	layout         warehouse.WarehouseLayout
	childPath      string
	root           string
	admitted       string
	materials      string
	staging        string
	scratch        string
	limits         warehouse.ChildLimits
	bounds         warehouse.QueryBounds
	columns        []warehouse.QueryColumn
	rows           [][]warehouse.QueryValue
	// raceWinnerRows records the rows of the segment that won the concurrent
	// publication race in the V57 case, so a later compaction can name them.
	raceWinnerRows [][]warehouse.QueryValue
}

// mustSegmentSchema composes the canonical registered segment schema.
func mustSegmentSchema(t *testing.T, columns []warehouse.QueryColumn) warehouse.SegmentSchema {
	t.Helper()
	schema, err := warehouse.NewSegmentSchema(columns)
	if err != nil {
		t.Fatalf("compose the declared segment schema: %v", err)
	}
	return schema
}

// warehouseChildLimits stay inside the protocol's own document capability: a
// request or response cap above it would admit an attempt whose typed document
// the codec refuses after a child has already run.
func warehouseChildLimits() warehouse.ChildLimits {
	return warehouse.ChildLimits{
		MaxRequestBytes:     warehouse.MaxQueryDocumentBytes,
		MaxResponseBytes:    warehouse.MaxQueryDocumentBytes - warehouseResponseEnvelopeSlack,
		MaxDiagnosticsBytes: 64 << 10,
		MemoryBytes:         256 << 20,
		TempBytes:           256 << 20,
		WallTime:            2 * time.Minute,
		StartupTimeout:      30 * time.Second,
	}
}

// warehouseQueryBounds leave room for the response envelope inside the
// supervisor's own response cap.
func warehouseQueryBounds() warehouse.QueryBounds {
	return warehouse.QueryBounds{
		Relations:   4,
		Columns:     32,
		Rows:        1 << 16,
		Bytes:       warehouse.MaxQueryDocumentBytes - 2*warehouseResponseEnvelopeSlack,
		TempBytes:   64 << 20,
		MemoryBytes: 128 << 20,
		WallTime:    time.Minute,
	}
}

func newWarehouseHarness(t *testing.T) *warehouseHarness {
	t.Helper()
	if os.Getenv("PM_STACK_TEST_CONFIG") == "" && os.Getenv("PM_STORE_TEST_SECRETS_DIR") == "" {
		// Missing fixture inputs fail rather than skip.
		t.Fatal("this physical case requires the explicitly selected owned stack fixture; missing inputs fail rather than skip")
	}
	harness := &warehouseHarness{
		t:         t,
		producer:  testfixture.OpenArtifacts(t, testfixture.PrimaryArtifactGateway),
		consumer:  testfixture.OpenArtifacts(t, testfixture.SecondaryArtifactGateway),
		childPath: warehouseQueryChildPath(t),
		limits:    warehouseChildLimits(),
		bounds:    warehouseQueryBounds(),
	}
	harness.fixture = testfixture.New(t)
	layout, err := warehouse.NewWarehouseLayout(
		harness.fixture.Connection.ConnectorID,
		harness.fixture.Connection.ConnectionID,
		warehouseRelationName,
	)
	if err != nil {
		t.Fatalf("compose the dataset namespace: %v", err)
	}
	harness.layout = layout
	harness.columns, harness.rows = warehouseSourceRows(t)
	harness.root = t.TempDir()
	harness.admitted = filepath.Join(harness.root, "admitted")
	harness.scratch = filepath.Join(harness.root, "scratch")
	if err := os.Mkdir(harness.admitted, 0o700); err != nil {
		t.Fatalf("create the admitted root: %v", err)
	}
	if err := os.Mkdir(harness.scratch, 0o700); err != nil {
		t.Fatalf("create the supervisor scratch root: %v", err)
	}
	// The child is admitted to one root only: its own materials and the staged
	// relations both live below it.
	harness.materials = filepath.Join(harness.admitted, "materials")
	harness.staging = filepath.Join(harness.admitted, "staging")
	for name, directory := range map[string]string{"materials": harness.materials, "staging": harness.staging} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatalf("create the %s directory: %v", name, err)
		}
	}
	harness.registerDataset(t, harness.fixture.Scope, harness.layout)
	harness.publisher = harness.newPublisher(t, harness.producer)
	harness.manifest = harness.newManifest(t, harness.layout)
	harness.consumerReader = harness.newPublisher(t, harness.consumer)
	return harness
}

func (harness *warehouseHarness) newPublisher(t *testing.T, resources *app.ArtifactResources) *objects.Publisher {
	t.Helper()
	publisher, err := objects.NewPublisher(harness.fixture.Pool, resources.Backend, resources.Limits)
	if err != nil {
		t.Fatalf("open the selected shared-object publisher: %v", err)
	}
	return publisher
}

func (harness *warehouseHarness) newManifest(t *testing.T, layout warehouse.WarehouseLayout) *warehouse.DatasetManifest {
	t.Helper()
	manifest, err := warehouse.NewDatasetManifest(harness.fixture.Pool, harness.publisher, layout, warehouseMaximumSegment)
	if err != nil {
		t.Fatalf("construct the dataset manifest publisher: %v", err)
	}
	return manifest
}

func (harness *warehouseHarness) actorFor(scope contract.Scope) contract.Actor {
	if scope.Equal(harness.fixture.ForeignScope) {
		return harness.fixture.ForeignActor
	}
	return harness.fixture.Actor
}

func (harness *warehouseHarness) registerDataset(t *testing.T, scope contract.Scope, layout warehouse.WarehouseLayout) {
	t.Helper()
	if err := store.WithinScope(context.Background(), harness.fixture.Pool, scope, harness.actorFor(scope),
		func(ctx context.Context, tx *store.Transaction) error {
			_, err := store.RegisterWarehouseDataset(ctx, tx, store.WarehouseDataset{
				Scope:        scope,
				DatasetID:    layout.DatasetID(),
				ConnectorID:  layout.ConnectorID,
				ConnectionID: layout.ConnectionID,
			})
			return err
		}); err != nil {
		t.Fatalf("register the dataset identity: %v", err)
	}
}

// newChild hands out one supervisor over the harness's admitted root so a case
// can observe the real child's own refusals separately from this slice's.
func (harness *warehouseHarness) newChild(t *testing.T, roots []string) *warehouse.QueryChild {
	t.Helper()
	if len(roots) == 0 {
		roots = []string{harness.admitted}
	}
	child, err := warehouse.NewQueryChild(warehouse.ChildConfig{
		BinaryPath:    harness.childPath,
		ScratchRoot:   harness.scratch,
		AdmittedRoots: roots,
		Limits:        harness.limits,
	})
	if err != nil {
		t.Fatalf("construct the query child supervisor: %v", err)
	}
	return child
}

func (harness *warehouseHarness) newService(t *testing.T) *warehouse.QueryService {
	t.Helper()
	service, err := warehouse.NewQueryService(
		harness.fixture.Pool, harness.producer.Backend, harness.newChild(t, nil), harness.staging,
	)
	if err != nil {
		t.Fatalf("construct the query service: %v", err)
	}
	return service
}

// materialize writes one immutable Parquet segment through the real child into
// the admitted materials directory and returns its independently re-verified
// byte identity.
func (harness *warehouseHarness) materialize(t *testing.T, requestID string, rows [][]warehouse.QueryValue) warehouse.SegmentBytes {
	t.Helper()
	segment, err := warehouse.MaterializeParquetSegment(context.Background(), harness.newChild(t, nil), warehouse.MaterializeRequest{
		RequestID:           requestID,
		AdmittedDirectories: []string{harness.materials},
		OutputPath:          filepath.Join(harness.materials, requestID+".parquet"),
		Columns:             harness.columnCopy(),
		Rows:                rows,
		Bounds:              harness.bounds,
		MaximumBytes:        warehouseMaximumSegment,
	})
	if err != nil {
		t.Fatalf("materialize the admitted Parquet segment %s: %v", requestID, err)
	}
	return segment
}

func (harness *warehouseHarness) columnCopy() []warehouse.QueryColumn {
	return append([]warehouse.QueryColumn(nil), harness.columns...)
}

// publishSegment materializes and publishes one segment at the next manifest
// revision, so the segment ledger and the manifest agree by construction.
func (harness *warehouseHarness) publishSegment(t *testing.T, requestID string, rows [][]warehouse.QueryValue) PublishedSegment {
	t.Helper()
	segment := harness.materialize(t, requestID, rows)
	published, err := harness.manifest.PublishSegment(context.Background(), warehouse.PublishSegmentRequest{
		Actor:     harness.fixture.Actor,
		Scope:     harness.fixture.Scope,
		AttemptID: requestID,
		Segment:   segment,
		RowCount:  int64(len(rows)),
	})
	if err != nil {
		t.Fatalf("publish the admitted segment %s: %v", requestID, err)
	}
	return PublishedSegment{Segment: published, Bytes: segment}
}

// PublishedSegment keeps the registered ledger row beside the locally verified
// bytes it was published from, so a case can compare both independently.
type PublishedSegment struct {
	Segment store.WarehouseSegment
	Bytes   warehouse.SegmentBytes
}

// publishSegments publishes the fixture rows split across two immutable
// segments, which is the smallest dataset that exercises a cross-segment read.
func (harness *warehouseHarness) publishSegments(t *testing.T) []PublishedSegment {
	t.Helper()
	return []PublishedSegment{
		harness.publishSegment(t, "segment-1", harness.rows[:warehouseSegmentSplit]),
		harness.publishSegment(t, "segment-2", harness.rows[warehouseSegmentSplit:]),
	}
}

func (harness *warehouseHarness) state(t *testing.T) warehouse.DatasetState {
	t.Helper()
	return harness.stateFor(t, harness.layout)
}

// stateFor loads the committed state of one dataset in this fixture, whether or
// not the harness retains a manifest for it.
func (harness *warehouseHarness) stateFor(t *testing.T, layout warehouse.WarehouseLayout) warehouse.DatasetState {
	t.Helper()
	manifest, err := warehouse.NewDatasetManifest(harness.fixture.Pool, harness.publisher, layout, warehouseMaximumSegment)
	if err != nil {
		t.Fatalf("construct the dataset manifest for %s: %v", layout.DatasetID(), err)
	}
	state, err := manifest.State(context.Background(), harness.fixture.Actor, harness.fixture.Scope)
	if err != nil {
		t.Fatalf("load the committed dataset state: %v", err)
	}
	return state
}

func (harness *warehouseHarness) datasetQuery(t *testing.T, mutate func(*warehouse.DatasetQueryRequest)) warehouse.DatasetQueryRequest {
	t.Helper()
	request := warehouse.DatasetQueryRequest{
		Actor:   harness.fixture.Actor,
		Scope:   harness.fixture.Scope,
		Layout:  harness.layout,
		QueryID: "warehouse-query",
		Query: warehouse.SavedQuery{
			Name:     "orders_all",
			Relation: warehouseRelationName,
			Project:  allColumnProjections(harness.columns),
			Order:    []warehouse.QueryOrder{{Column: "id"}},
			Limit:    int64(len(harness.rows)),
		},
		MaximumSegmentBytes: warehouseMaximumSegment,
		Bounds:              harness.bounds,
	}
	if mutate != nil {
		mutate(&request)
	}
	return request
}

// warehouseSourceRows declares the exact typed rows the acceptance cases
// publish. They cover every nullable and temporal distinction the protocol must
// preserve: an explicit null, a decimal scale, an exact instant, and text that
// must never be interpreted as statement text.
func warehouseSourceRows(t *testing.T) ([]warehouse.QueryColumn, [][]warehouse.QueryValue) {
	t.Helper()
	columns := []warehouse.QueryColumn{
		{Name: "id", Kind: warehouse.QueryKindInt64, Nullable: false},
		{Name: "region", Kind: warehouse.QueryKindText, Nullable: false},
		{Name: "amount", Kind: warehouse.QueryKindDecimal, Precision: 18, Scale: 4, Nullable: true},
		{Name: "active", Kind: warehouse.QueryKindBoolean, Nullable: false},
		{Name: "recorded_at", Kind: warehouse.QueryKindTimestampTZ, Nullable: false},
		{Name: "note", Kind: warehouse.QueryKindText, Nullable: true},
		{Name: "tag", Kind: warehouse.QueryKindUUID, Nullable: false},
		{Name: "day", Kind: warehouse.QueryKindDate, Nullable: false},
		{Name: "document", Kind: warehouse.QueryKindJSON, Nullable: true},
	}
	regions := []string{"north", "south", "north", "east", "south", "east", "west", "north"}
	ids := make([]int64, 0, warehouseSegmentRows)
	for index := range warehouseSegmentRows {
		ids = append(ids, int64(index+1))
	}
	rows := warehouseRows(t, ids...)
	for index, row := range rows {
		row[1] = warehouse.TextQueryValue(regions[index])
		if index == 2 {
			row[5] = warehouse.NullQueryValue()
		}
		if index == 4 {
			row[8] = warehouse.NullQueryValue()
			row[2] = warehouse.NullQueryValue()
		}
	}
	return columns, rows
}

// warehouseRows composes one exactly typed row per identifier, using the
// fixture's registered column order.
func warehouseRows(t *testing.T, ids ...int64) [][]warehouse.QueryValue {
	t.Helper()
	rows := make([][]warehouse.QueryValue, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, []warehouse.QueryValue{
			warehouse.Int64QueryValue(id),
			warehouse.TextQueryValue("north"),
			mustDecimal(t, fmt.Sprintf("%d12345", id), 4),
			warehouse.BooleanQueryValue(id%2 == 0),
			warehouse.TimestampTZQueryValue(1_710_000_000_000_000 + id),
			warehouse.TextQueryValue(fmt.Sprintf("row %d", id)),
			mustUUID(t, fmt.Sprintf("00000000-0000-4000-8000-%012d", id)),
			mustDate(t, fmt.Sprintf("2024-03-%02d", id%28+1)),
			mustJSON(t, fmt.Sprintf(`{"id": %d}`, id)),
		})
	}
	return rows
}

// TestCrossSegmentReadMatchesSingleSegment requires a read merged across
// registered segments to return exactly what the restricted child returns for
// the same rows held in one segment: ordering names the relation's own columns,
// never an output alias, and the merged result never exceeds the declared row
// bound. Every read is real: the segments are published, staged and executed
// through the restricted child against the owned stack.
func TestCrossSegmentReadMatchesSingleSegment(t *testing.T) {
	harness := newWarehouseHarness(t)
	ctx := context.Background()
	service := harness.newService(t)
	// rows[0] is id 1 in region north and rows[3] is id 4 in region east, so
	// ordering by the relation's id and ordering by the region text disagree.
	low, high := harness.rows[0], harness.rows[3]
	harness.publishSegment(t, "cross-high", [][]warehouse.QueryValue{high})
	harness.publishSegment(t, "cross-low", [][]warehouse.QueryValue{low})
	harness.publishSegment(t, "cross-both", [][]warehouse.QueryValue{low, high})
	snapshot, err := service.Snapshot(ctx, harness.fixture.Actor, harness.fixture.Scope,
		warehouse.SnapshotReadRequest{Layout: harness.layout})
	if err != nil {
		t.Fatalf("resolve the published segments: %v", err)
	}
	if len(snapshot.Segments) != 3 {
		t.Fatalf("published read view holds %d segments; want 3", len(snapshot.Segments))
	}
	split, single := snapshot.Segments[:2], snapshot.Segments[2:]
	read := func(t *testing.T, queryID string, segments []warehouse.QuerySnapshotSegment,
		selection warehouse.QuerySelect, bounds warehouse.QueryBounds) warehouse.QueryResponse {
		t.Helper()
		response, err := service.Read(ctx, warehouse.ReadRequest{
			Actor: harness.fixture.Actor, Scope: harness.fixture.Scope, Layout: harness.layout,
			QueryID: queryID, Select: selection, Segments: segments,
			MaximumSegmentBytes: warehouseMaximumSegment, Bounds: bounds,
		})
		if err != nil {
			t.Fatalf("read %s: %v", queryID, err)
		}
		return response
	}
	requireSameResult := func(t *testing.T, merged, reference warehouse.QueryResponse) {
		t.Helper()
		if !reflect.DeepEqual(merged.Columns, reference.Columns) {
			t.Fatalf("merged columns differ from the single-segment columns:\n got %s\nwant %s",
				describeColumns(merged.Columns), describeColumns(reference.Columns))
		}
		if !reflect.DeepEqual(merged.Rows, reference.Rows) || merged.Truncated != reference.Truncated {
			t.Fatalf("merged read differs from the single-segment read:\n got %s (truncated %t)\nwant %s (truncated %t)",
				describeRows(merged.Rows), merged.Truncated, describeRows(reference.Rows), reference.Truncated)
		}
	}

	t.Run("ordering_names_relation_columns_not_output_aliases", func(t *testing.T) {
		// id is projected as shown, and region is projected under the name id.
		// The ordering still names the relation's id column.
		for _, descending := range []bool{false, true} {
			selection := warehouse.QuerySelect{
				From: harness.layout.RelationName(),
				Project: []warehouse.QueryProjection{
					{Column: "id", Alias: "shown"},
					{Column: "region", Alias: "id"},
				},
				Order: []warehouse.QueryOrder{{Column: "id", Descending: descending}},
				Limit: 1,
			}
			suffix := fmt.Sprintf("%t", descending)
			reference := read(t, "alias-single-"+suffix, single, selection, harness.bounds)
			wanted := low
			if descending {
				wanted = high
			}
			if len(reference.Rows) != 1 || !reflect.DeepEqual(reference.Rows[0][0], wanted[0]) {
				t.Fatalf("single-segment ORDER BY id (descending %t) LIMIT 1 = %s; want relation id %s",
					descending, describeRows(reference.Rows), describeRows([][]warehouse.QueryValue{{wanted[0]}}))
			}
			requireSameResult(t, read(t, "alias-merged-"+suffix, split, selection, harness.bounds), reference)
		}
	})

	t.Run("an_ordering_column_that_is_not_projected_is_refused_across_segments", func(t *testing.T) {
		// Only region is projected, under the relation column name id: the
		// relation's id cannot be compared across segments from the output.
		_, err := service.Read(ctx, warehouse.ReadRequest{
			Actor: harness.fixture.Actor, Scope: harness.fixture.Scope, Layout: harness.layout,
			QueryID: "alias-unprojected",
			Select: warehouse.QuerySelect{
				From:    harness.layout.RelationName(),
				Project: []warehouse.QueryProjection{{Column: "region", Alias: "id"}},
				Order:   []warehouse.QueryOrder{{Column: "id"}},
				Limit:   1,
			},
			Segments: split, MaximumSegmentBytes: warehouseMaximumSegment, Bounds: harness.bounds,
		})
		var refused *warehouse.Refusal
		if !errors.As(err, &refused) || refused.Code != warehouse.RefusalUnknownColumn {
			t.Fatalf("cross-segment ordering by an unprojected relation column = %v; want an unknown-column refusal", err)
		}
	})

	t.Run("the_merged_result_never_exceeds_the_declared_row_bound", func(t *testing.T) {
		bounds := harness.bounds
		bounds.Rows = 1
		selection := warehouse.QuerySelect{
			From:    harness.layout.RelationName(),
			Project: allColumnProjections(harness.columns),
			Order:   []warehouse.QueryOrder{{Column: "id"}},
			Limit:   2,
		}
		reference := read(t, "bound-single", single, selection, bounds)
		if len(reference.Rows) != 1 || !reference.Truncated {
			t.Fatalf("single-segment read under a one-row bound = %d rows (truncated %t); want one truncated row",
				len(reference.Rows), reference.Truncated)
		}
		merged := read(t, "bound-merged", split, selection, bounds)
		if int64(len(merged.Rows)) > bounds.Rows {
			t.Fatalf("merged read returned %d rows despite the declared row bound %d", len(merged.Rows), bounds.Rows)
		}
		requireSameResult(t, merged, reference)
	})
}

// TestQueryReturnsRowsAndRejectsWrites is the V37 acceptance case. It reads
// registered immutable segments through the real restricted child, binds typed
// saved-query parameters, and observes that statement text, write shapes, file
// functions, external extensions, unknown identifiers, and cross-scope datasets
// are refused before any child process executes.
func TestQueryReturnsRowsAndRejectsWrites(t *testing.T) {
	harness := newWarehouseHarness(t)
	published := harness.publishSegments(t)

	t.Run("owned_relation_returns_exact_rows_and_types", func(t *testing.T) {
		state := harness.state(t)
		if state.Revision != 2 || len(state.Segments) != 2 {
			t.Fatalf("committed dataset state = revision %d over %d segments; want revision 2 over 2",
				state.Revision, len(state.Segments))
		}
		if state.Cursor.Ordinal != state.Revision {
			t.Fatalf("cursor ordinal %d disagrees with committed revision %d", state.Cursor.Ordinal, state.Revision)
		}
		for index, segment := range state.Segments {
			// The registered row repeats the locally verified bytes it was
			// published from, and those bytes are readable through the
			// independent gateway without this host's scratch directory.
			if segment.Object.SHA256 != strings.TrimPrefix(published[index].Bytes.SHA256, "sha256:") ||
				segment.Object.ByteLength != published[index].Bytes.Bytes {
				t.Fatalf("registered segment %d identity %s/%d disagrees with the verified local bytes %s/%d",
					index, segment.Object.SHA256, segment.Object.ByteLength,
					published[index].Bytes.SHA256, published[index].Bytes.Bytes)
			}
			if err := harness.consumer.Backend.Verify(context.Background(), segment.Object); err != nil {
				t.Fatalf("independent gateway could not verify registered segment %d: %v", index, err)
			}
		}

		response, err := harness.newService(t).Query(context.Background(), harness.datasetQuery(t, nil))
		if err != nil {
			t.Fatalf("query the registered dataset relation: %v", err)
		}
		if response.Truncated {
			t.Fatalf("declared row limit cut a %d-row result", len(harness.rows))
		}
		if !reflect.DeepEqual(response.Columns, harness.columns) {
			t.Fatalf("projected columns differ:\n got %s\nwant %s", describeColumns(response.Columns), describeColumns(harness.columns))
		}
		if !reflect.DeepEqual(response.Rows, harness.rows) {
			t.Fatalf("exact typed rows differ:\n got %s\nwant %s", describeRows(response.Rows), describeRows(harness.rows))
		}
		if response.Child == nil || response.Child.PID <= 0 {
			t.Fatal("dataset query returned no real child observation")
		}
		if entries := harness.stagingEntries(t); entries != 0 {
			t.Fatalf("query left %d staged relation directories behind", entries)
		}
	})

	t.Run("retained_snapshot_returns_its_exact_rows", func(t *testing.T) {
		if _, err := harness.manifest.TakeSnapshot(context.Background(), harness.fixture.Actor, harness.fixture.Scope,
			warehouse.SnapshotRequest{SnapshotID: "orders-revision-1", Revision: 1}); err != nil {
			t.Fatalf("take the first publication's snapshot: %v", err)
		}
		response, err := harness.newService(t).Query(context.Background(), harness.datasetQuery(t, func(request *warehouse.DatasetQueryRequest) {
			request.SnapshotID = "orders-revision-1"
		}))
		if err != nil {
			t.Fatalf("query the retained snapshot: %v", err)
		}
		if !reflect.DeepEqual(response.Rows, harness.rows[:warehouseSegmentSplit]) {
			t.Fatalf("retained snapshot rows differ:\n got %s\nwant %s",
				describeRows(response.Rows), describeRows(harness.rows[:warehouseSegmentSplit]))
		}
	})

	t.Run("saved_query_parameters_bind_exact_typed_values", func(t *testing.T) {
		query := harness.datasetQuery(t, func(request *warehouse.DatasetQueryRequest) {
			request.Query.Project = []warehouse.QueryProjection{{Column: "id"}, {Column: "region", Alias: "label"}}
			request.Query.Order = []warehouse.QueryOrder{{Column: "id", Descending: true}}
			request.Query.Limit = 1
			request.Query.Parameters = []warehouse.QueryParameterDeclaration{
				{Name: "minimum_id", Kind: warehouse.QueryKindInt64},
				{Name: "region_name", Kind: warehouse.QueryKindText},
			}
			request.Query.Predicates = []warehouse.SavedPredicate{
				{Column: "id", Operator: warehouse.QueryOperatorGreaterEqual, Parameter: "minimum_id"},
				{Column: "region", Operator: warehouse.QueryOperatorEqual, Parameter: "region_name"},
			}
			request.Bindings = []warehouse.QueryParameter{
				{Name: "minimum_id", Kind: warehouse.QueryKindInt64, Value: warehouse.Int64QueryValue(2)},
				{Name: "region_name", Kind: warehouse.QueryKindText, Value: warehouse.TextQueryValue("south")},
			}
		})
		response, err := harness.newService(t).Query(context.Background(), query)
		if err != nil {
			t.Fatalf("query with bound saved-query parameters: %v", err)
		}
		want := [][]warehouse.QueryValue{
			{warehouse.Int64QueryValue(5), warehouse.TextQueryValue("south")},
		}
		if !reflect.DeepEqual(response.Rows, want) {
			t.Fatalf("parameter-bound rows differ:\n got %s\nwant %s", describeRows(response.Rows), describeRows(want))
		}
		if !response.Truncated {
			t.Fatal("a row bound that cut the parameter-bound result was not reported as truncated")
		}
		wantColumns := []warehouse.QueryColumn{
			{Name: "id", Kind: warehouse.QueryKindInt64, Nullable: false},
			{Name: "label", Kind: warehouse.QueryKindText, Nullable: false},
		}
		if !reflect.DeepEqual(response.Columns, wantColumns) {
			t.Fatalf("aliased projection columns differ:\n got %s\nwant %s", describeColumns(response.Columns), describeColumns(wantColumns))
		}

		refusals := []struct {
			name   string
			mutate func(*warehouse.DatasetQueryRequest)
		}{
			{"unknown_binding", func(request *warehouse.DatasetQueryRequest) {
				request.Bindings = append(request.Bindings, warehouse.QueryParameter{Name: "unlisted", Kind: warehouse.QueryKindText, Value: warehouse.TextQueryValue("south")})
			}},
			{"missing_binding", func(request *warehouse.DatasetQueryRequest) { request.Bindings = request.Bindings[:1] }},
			{"duplicate_binding", func(request *warehouse.DatasetQueryRequest) {
				request.Bindings = append(request.Bindings, request.Bindings[0])
			}},
			{"binding_kind_mismatch", func(request *warehouse.DatasetQueryRequest) {
				request.Bindings[0] = warehouse.QueryParameter{Name: "minimum_id", Kind: warehouse.QueryKindText, Value: warehouse.TextQueryValue("2")}
			}},
			{"undeclared_parameter_use", func(request *warehouse.DatasetQueryRequest) {
				request.Query.Predicates[0].Parameter = "unlisted"
			}},
			{"parameter_in_list", func(request *warehouse.DatasetQueryRequest) {
				request.Query.Predicates[0] = warehouse.SavedPredicate{Column: "id", Operator: warehouse.QueryOperatorIn, Parameter: "minimum_id"}
			}},
			{"null_for_non_nullable_parameter", func(request *warehouse.DatasetQueryRequest) {
				request.Bindings[0] = warehouse.QueryParameter{Name: "minimum_id", Kind: warehouse.QueryKindInt64, Value: warehouse.NullQueryValue()}
			}},
		}
		for _, refusal := range refusals {
			t.Run(refusal.name, func(t *testing.T) {
				candidate := query
				candidate.Query.Parameters = append([]warehouse.QueryParameterDeclaration(nil), query.Query.Parameters...)
				candidate.Query.Predicates = append([]warehouse.SavedPredicate(nil), query.Query.Predicates...)
				candidate.Bindings = append([]warehouse.QueryParameter(nil), query.Bindings...)
				refusal.mutate(&candidate)
				canary := newCanaryChild(t, harness)
				service, err := warehouse.NewQueryService(harness.fixture.Pool, harness.producer.Backend, canary.child, harness.staging)
				if err != nil {
					t.Fatalf("construct the canary query service: %v", err)
				}
				_, err = service.Query(context.Background(), candidate)
				failure := requireRefusal(t, err)
				if failure.Origin != warehouse.RefusalOriginSupervisor {
					t.Fatalf("parameter refusal %q came from the child: %v", refusal.name, failure)
				}
				canary.expectNoExecution(t)
			})
		}
	})

	t.Run("statement_text_and_unknown_identifiers_are_refused", func(t *testing.T) {
		// Every hostile shape is data in this protocol: it can only arrive as a
		// declared name or a bound value, and none of it may reach a child.
		cases := map[string]func(*warehouse.DatasetQueryRequest){
			"raw_sql_relation": func(request *warehouse.DatasetQueryRequest) {
				request.Query.Relation = "select * from orders"
			},
			"raw_sql_alias": func(request *warehouse.DatasetQueryRequest) {
				request.Query.Project = []warehouse.QueryProjection{{Column: "id", Alias: "id; DROP TABLE pm.artifacts"}}
			},
			"file_function_column": func(request *warehouse.DatasetQueryRequest) {
				request.Query.Project = []warehouse.QueryProjection{{Column: "read_parquet"}}
			},
			"extension_parameter": func(request *warehouse.DatasetQueryRequest) {
				request.Query.Parameters = []warehouse.QueryParameterDeclaration{{Name: "INSTALL httpfs", Kind: warehouse.QueryKindText}}
			},
			"write_statement_predicate": func(request *warehouse.DatasetQueryRequest) {
				request.Query.Predicates = []warehouse.SavedPredicate{{Column: "INSERT", Operator: warehouse.QueryOperatorEqual, Value: warehouse.TextQueryValue("x")}}
			},
			"copy_output": func(request *warehouse.DatasetQueryRequest) {
				request.Query.Name = "COPY orders TO '/tmp/leak.parquet'"
			},
			"unknown_relation": func(request *warehouse.DatasetQueryRequest) { request.Query.Relation = "pm_artifacts" },
			"unknown_projection": func(request *warehouse.DatasetQueryRequest) {
				request.Query.Project = []warehouse.QueryProjection{{Column: "credential_ref"}}
			},
			"unknown_predicate": func(request *warehouse.DatasetQueryRequest) {
				request.Query.Predicates = []warehouse.SavedPredicate{{Column: "provider_account_id", Operator: warehouse.QueryOperatorEqual, Value: warehouse.TextQueryValue("x")}}
			},
			"unknown_ordering": func(request *warehouse.DatasetQueryRequest) {
				request.Query.Order = []warehouse.QueryOrder{{Column: "unknown_column"}}
			},
		}
		for name, mutate := range cases {
			t.Run(name, func(t *testing.T) {
				canary := newCanaryChild(t, harness)
				service, err := warehouse.NewQueryService(harness.fixture.Pool, harness.producer.Backend, canary.child, harness.staging)
				if err != nil {
					t.Fatalf("construct the canary query service: %v", err)
				}
				_, err = service.Query(context.Background(), harness.datasetQuery(t, mutate))
				failure := requireRefusal(t, err)
				if failure.Origin != warehouse.RefusalOriginSupervisor {
					t.Fatalf("refusal %q came from the child: %v", name, failure)
				}
				canary.expectNoExecution(t)
			})
		}
	})

	t.Run("hostile_text_stays_exact_data", func(t *testing.T) {
		// The same text as a declared column value is data, never statement
		// text: it must round-trip exactly through the registered segments.
		hostile := "DROP TABLE pm.artifacts; INSTALL httpfs"
		rows := [][]warehouse.QueryValue{
			{warehouse.Int64QueryValue(100), warehouse.TextQueryValue(hostile)},
			{warehouse.Int64QueryValue(101), warehouse.TextQueryValue("read_parquet('/etc/passwd')")},
		}
		columns := []warehouse.QueryColumn{
			{Name: "id", Kind: warehouse.QueryKindInt64, Nullable: false},
			{Name: "note", Kind: warehouse.QueryKindText, Nullable: false},
		}
		segment, err := warehouse.MaterializeParquetSegment(context.Background(), harness.newChild(t, nil), warehouse.MaterializeRequest{
			RequestID:           "hostile-text",
			AdmittedDirectories: []string{harness.materials},
			OutputPath:          filepath.Join(harness.materials, "hostile-text.parquet"),
			Columns:             columns,
			Rows:                rows,
			Bounds:              harness.bounds,
			MaximumBytes:        warehouseMaximumSegment,
		})
		if err != nil {
			t.Fatalf("materialize the hostile-text segment: %v", err)
		}
		layout, err := warehouse.NewWarehouseLayout(harness.layout.ConnectorID, harness.layout.ConnectionID, "hostile_notes")
		if err != nil {
			t.Fatalf("compose the hostile-text dataset namespace: %v", err)
		}
		harness.registerDataset(t, harness.fixture.Scope, layout)
		manifest := harness.newManifest(t, layout)
		if _, err := manifest.PublishSegment(context.Background(), warehouse.PublishSegmentRequest{
			Actor: harness.fixture.Actor, Scope: harness.fixture.Scope, AttemptID: "hostile-text",
			Segment: segment, RowCount: int64(len(rows)),
		}); err != nil {
			t.Fatalf("publish the hostile-text segment: %v", err)
		}
		service, err := warehouse.NewQueryService(harness.fixture.Pool, harness.producer.Backend, harness.newChild(t, nil), harness.staging)
		if err != nil {
			t.Fatalf("construct the query service: %v", err)
		}
		response, err := service.Query(context.Background(), warehouse.DatasetQueryRequest{
			Actor: harness.fixture.Actor, Scope: harness.fixture.Scope, Layout: layout,
			QueryID: "hostile-query",
			Query: warehouse.SavedQuery{
				Name: "hostile_notes", Relation: "hostile_notes",
				Project: []warehouse.QueryProjection{{Column: "id"}, {Column: "note"}},
				Order:   []warehouse.QueryOrder{{Column: "id"}},
				Limit:   int64(len(rows)),
			},
			MaximumSegmentBytes: warehouseMaximumSegment,
			Bounds:              harness.bounds,
		})
		if err != nil {
			t.Fatalf("query the hostile-text dataset: %v", err)
		}
		if !reflect.DeepEqual(response.Rows, rows) {
			t.Fatalf("hostile text rows differ:\n got %s\nwant %s", describeRows(response.Rows), describeRows(rows))
		}
	})

	t.Run("cross_scope_datasets_are_refused", func(t *testing.T) {
		foreignLayout, err := warehouse.NewWarehouseLayout(
			harness.fixture.ForeignConnection.ConnectorID,
			harness.fixture.ForeignConnection.ConnectionID,
			warehouseRelationName,
		)
		if err != nil {
			t.Fatalf("compose the foreign dataset namespace: %v", err)
		}
		// The same dataset name is registered for another connection in another
		// workspace. It is not this workspace's dataset.
		harness.registerDataset(t, harness.fixture.ForeignScope, foreignLayout)
		otherConnector, err := warehouse.NewWarehouseLayout("other-connector", harness.layout.ConnectionID, warehouseRelationName)
		if err != nil {
			t.Fatalf("compose the foreign connector namespace: %v", err)
		}
		otherDataset, err := warehouse.NewWarehouseLayout(harness.layout.ConnectorID, harness.layout.ConnectionID, "secret_orders")
		if err != nil {
			t.Fatalf("compose the unregistered dataset namespace: %v", err)
		}
		cases := map[string]warehouse.DatasetQueryRequest{
			"foreign_connection": harness.datasetQuery(t, func(request *warehouse.DatasetQueryRequest) {
				request.Layout = foreignLayout
			}),
			"foreign_connector": harness.datasetQuery(t, func(request *warehouse.DatasetQueryRequest) {
				request.Layout = otherConnector
			}),
			"unregistered_dataset": harness.datasetQuery(t, func(request *warehouse.DatasetQueryRequest) {
				request.Layout = otherDataset
			}),
		}
		for name, request := range cases {
			t.Run(name, func(t *testing.T) {
				_, err := harness.newService(t).Query(context.Background(), request)
				if err == nil {
					t.Fatalf("cross-scope dataset %q was answered", name)
				}
				if failure := requireRefusal(t, err); failure.Origin != warehouse.RefusalOriginSupervisor {
					t.Fatalf("cross-scope refusal %q came from the child: %v", name, failure)
				}
			})
		}
		// A snapshot of another workspace cannot be read even when the caller
		// knows its identity.
		if _, err := harness.manifest.LoadSnapshot(context.Background(), harness.fixture.Actor, harness.fixture.ForeignScope, "orders-revision-1"); err == nil {
			t.Fatal("this workspace could read a foreign dataset snapshot")
		}
	})

	t.Run("layout_namespace_refuses_ambiguity_and_collisions", func(t *testing.T) {
		for _, invalid := range [][3]string{
			{"github", "connection/../primary", "orders"},
			{"github", "connection-primary", "../orders"},
			{"github", "connection-primary", "public.orders"},
			{"github", "connection primary", "orders"},
			{"GitHub", "connection-primary", "orders"},
			{"github", "connection-primary", "orders/other"},
			{"github", "connection-primary", ""},
			{"", "connection-primary", "orders"},
		} {
			if _, err := warehouse.NewWarehouseLayout(invalid[0], invalid[1], invalid[2]); !errors.Is(err, warehouse.ErrObjectLayout) {
				t.Fatalf("unsafe layout component accepted for %#v: %v", invalid, err)
			}
		}
		// Distinct identities never share one dataset identity.
		identities := map[string][3]string{}
		canonical := ""
		for _, candidate := range [][3]string{
			{"github", "connection-primary", "orders"},
			{"github", "connection-primary1", "orders"},
			{"github", "connection-primary", "orders1"},
			{"githubs", "connection-primary", "orders"},
			{"github", "connection-primary", "orders-1"},
			{"github", "connection-primary", "orders_1"},
		} {
			layout, err := warehouse.NewWarehouseLayout(candidate[0], candidate[1], candidate[2])
			if err != nil {
				t.Fatalf("compose layout %#v: %v", candidate, err)
			}
			identity := layout.DatasetID()
			if previous, duplicate := identities[identity]; duplicate {
				t.Fatalf("layouts %#v and %#v share the dataset identity %q", previous, candidate, identity)
			}
			identities[identity] = candidate
			if canonical == "" {
				canonical = identity
			}
			parsed, err := warehouse.ParseWarehouseDatasetID(identity)
			if err != nil || parsed != layout {
				t.Fatalf("dataset identity %q did not round-trip: %v", identity, err)
			}
			// Two different attempt identities never share one segment
			// identity, including identifiers that differ only by length.
			seen := map[string]string{}
			for _, attemptID := range []string{"a", "a1", "a-1", "a_1", "1", "11"} {
				identity, err := layout.SegmentArtifactID(attemptID)
				if err != nil {
					t.Fatalf("compose the segment identity for %q: %v", attemptID, err)
				}
				if previous, duplicate := seen[identity]; duplicate {
					t.Fatalf("attempts %q and %q share the segment identity %q", previous, attemptID, identity)
				}
				seen[identity] = attemptID
			}
		}
		for _, legacy := range []string{
			warehouseRelationName,
			"github/orders",
			"github/connection-primary/orders",
			"wh1.github.orders",
			"wh2.github.connection-primary.orders",
			strings.ToUpper(canonical),
		} {
			if _, err := warehouse.ParseWarehouseDatasetID(legacy); !errors.Is(err, warehouse.ErrAmbiguousLayout) {
				t.Fatalf("legacy dataset identity %q was accepted or misreported: %v", legacy, err)
			}
		}
	})
}

// canaryChild is an executable stand-in that records its own execution. A
// request refused before execution never creates the canary file, which is an
// observation this test process makes from outside both the supervisor and the
// child.
type canaryChild struct {
	child  *warehouse.QueryChild
	canary string
}

func newCanaryChild(t *testing.T, harness *warehouseHarness) canaryChild {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("protect the canary directory: %v", err)
	}
	canary := filepath.Join(directory, "executed")
	binary := filepath.Join(directory, "pm-query-canary")
	script := "#!/bin/sh\n: > " + canary + "\nexit 1\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatalf("write the canary child: %v", err)
	}
	child, err := warehouse.NewQueryChild(warehouse.ChildConfig{
		BinaryPath:    binary,
		ScratchRoot:   harness.scratch,
		AdmittedRoots: []string{harness.admitted},
		Limits:        harness.limits,
	})
	if err != nil {
		t.Fatalf("construct the canary supervisor: %v", err)
	}
	return canaryChild{child: child, canary: canary}
}

func (canary canaryChild) expectNoExecution(t *testing.T) {
	t.Helper()
	if _, err := os.Lstat(canary.canary); err == nil {
		t.Fatal("a refused request reached an executed child process")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inspect the canary: %v", err)
	}
}

// stagingEntries counts the staged relation directories a query left behind.
func (harness *warehouseHarness) stagingEntries(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(harness.staging)
	if err != nil {
		t.Fatalf("read the staging root: %v", err)
	}
	return len(entries)
}
