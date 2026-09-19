package sync_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	etl "github.com/polymetrics-ai/pm-cli/internal/sync"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
	"github.com/polymetrics-ai/pm-cli/internal/warehouse"
)

// This file owns the V39 acceptance case. It runs the real restricted query
// child, the real selected shared-object gateways and real PostgreSQL through
// the owned stack fixture: every acknowledged ETL page is materialized as
// immutable Parquet bytes, published into the shared object store, and
// registered together with its cursor advance in one manifest transaction. The
// case then observes the durable ledger after each acknowledgement shape a page
// can arrive in: lost, partial, out-of-order and stale.
const (
	etlDatasetName = "orders"
	etlPageRows    = 2
	// etlMaximumSegment stays well inside the child's own document capability,
	// so no admitted page is refused by an envelope bound after the child ran.
	etlMaximumSegment = warehouse.MaxQueryDocumentBytes / 4
)

// TestPartialAckContiguousCursor is the V39 acceptance case. Only the committed
// exact prefix of acknowledged pages may advance the dataset cursor: a lost,
// partial, out-of-order or stale acknowledgement leaves the previous checkpoint
// in place, and no acknowledgement ever registers a second segment for one
// position or advances the cursor over a position that was never published.
func TestPartialAckContiguousCursor(t *testing.T) {
	harness := newETLHarness(t)

	t.Run("committed_prefix_advances_one_page_at_a_time", func(t *testing.T) {
		first := harness.commit(t, harness.page(1))
		if first.Position != 1 {
			t.Fatalf("first acknowledged page = position %d; want 1", first.Position)
		}
		if first.Rows != etlPageRows {
			t.Fatalf("first acknowledged page registered %d rows; want %d", first.Rows, etlPageRows)
		}
		harness.assertPrefix(t, 1)

		second := harness.commit(t, harness.page(2))
		if second.Position != 2 {
			t.Fatalf("second acknowledged page = position %d; want 2", second.Position)
		}
		harness.assertPrefix(t, 2)
	})

	t.Run("lost_acknowledgement_replays_the_committed_page", func(t *testing.T) {
		// The commit that acknowledged page 2 succeeded; the caller lost the
		// reply and retries the same page, which it can only do by re-reading
		// the source. The retry verifies the same bytes and resolves to the
		// registered occurrence instead of publishing the rows twice.
		replayed := harness.commit(t, harness.page(2))
		if replayed.Position != 2 {
			t.Fatalf("replayed acknowledgement = position %d; want the committed 2", replayed.Position)
		}
		harness.assertPrefix(t, 2)
	})

	t.Run("partial_acknowledgement_never_registers", func(t *testing.T) {
		// A caller that acknowledges fewer rows than the page it fetched has
		// not covered the page, so nothing may be published for it.
		partial := harness.page(3)
		partial.DeclaredRows = etlPageRows - 1
		if _, err := harness.attempt(partial); !errors.Is(err, etl.ErrPartialAcknowledgement) {
			t.Fatalf("partial acknowledgement = %v; want a partial-acknowledgement refusal", err)
		}
		harness.assertPrefix(t, 2)

		// The same refusal holds for a page whose rows are not a complete
		// batch for the declared columns: verified bytes for it never exist.
		malformed := harness.page(3)
		malformed.Rows[0] = malformed.Rows[0][:1]
		if _, err := harness.attempt(malformed); err == nil {
			t.Fatal("a page with an incomplete row was materialized and acknowledged")
		}
		harness.assertPrefix(t, 2)
	})

	t.Run("out_of_order_acknowledgement_cannot_skip_a_position", func(t *testing.T) {
		// Page 4 arrives while page 3 was never acknowledged. It is refused,
		// and the cursor stays exactly on the committed prefix.
		if _, err := harness.attempt(harness.page(4)); !errors.Is(err, etl.ErrCheckpointGap) {
			t.Fatalf("out-of-order acknowledgement = %v; want a checkpoint-gap refusal", err)
		}
		harness.assertPrefix(t, 2)

		// The gap is closed by the page that was missing, never by the page
		// that skipped it.
		harness.commit(t, harness.page(3))
		harness.assertPrefix(t, 3)
		committed := harness.commit(t, harness.page(4))
		if committed.Position != 4 {
			t.Fatalf("page 4 after the gap closed = position %d; want 4", committed.Position)
		}
		harness.assertPrefix(t, 4)
	})

	t.Run("stale_acknowledgement_cannot_rewrite_or_rewind_the_prefix", func(t *testing.T) {
		// A superseded acknowledgement that re-reads a committed position and
		// observes different rows than the registered ones is refused: the
		// checkpoint keeps the rows that were actually published.
		stale := harness.page(1)
		stale.Rows = harness.rows(etlPageRows, 2*etlPageRows)
		if _, err := harness.attempt(stale); !errors.Is(err, etl.ErrCheckpointStale) {
			t.Fatalf("stale acknowledgement = %v; want a stale-checkpoint refusal", err)
		}
		harness.assertPrefix(t, 4)

		// An exact replay of a superseded position is idempotent and never
		// rewinds the checkpoint to the older page.
		replayed := harness.commit(t, harness.page(1))
		if replayed.Position != 4 {
			t.Fatalf("superseded replay = position %d; want the committed 4", replayed.Position)
		}
		harness.assertPrefix(t, 4)

		// A page that does not declare polling is refused outright: a poll
		// re-reads a position and is never an ordered change stream.
		stream := harness.page(5)
		stream.Semantics = "change_data_capture"
		if _, err := harness.attempt(stream); !errors.Is(err, etl.ErrNotPollingSource) {
			t.Fatalf("undeclared source semantics = %v; want a polling-declaration refusal", err)
		}
		harness.assertPrefix(t, 4)
	})

	t.Run("independent_host_reads_the_committed_prefix", func(t *testing.T) {
		// Close the producer's private scratch and replace the runtime pool: the
		// second host keeps only the database identity and the other shared
		// gateway, rebuilds its publisher and manifest against the new pool, and
		// stages its own relations in the same admitted root.
		if err := harness.producer.Close(); err != nil {
			t.Fatalf("close the first host's object scratch: %v", err)
		}
		harness.fixture.Restart(t)
		harness.rebind(t)
		checkpoint, err := harness.checkpoint.Load(context.Background(), harness.fixture.Actor, harness.fixture.Scope)
		if err != nil {
			t.Fatalf("second host checkpoint: %v", err)
		}
		if checkpoint.Position != 4 || checkpoint.Rows != etlPageRows {
			t.Fatalf("second host checkpoint = position %d over %d rows; want position 4 over %d rows",
				checkpoint.Position, checkpoint.Rows, etlPageRows)
		}
		harness.assertRows(t, harness.fixture.Pool, harness.consumer.Backend, harness.rows(0, 4*etlPageRows))
	})

	t.Run("another_writer_cannot_stand_in_for_a_page", func(t *testing.T) {
		// A registered segment that this protocol never published as the page
		// for a position cannot be acknowledged as one, and the acknowledged
		// prefix is left intact for every reader.
		foreign := harness.foreignSegment(t)
		if _, err := harness.manifest.PublishSegment(context.Background(), warehouse.PublishSegmentRequest{
			Actor:     harness.fixture.Actor,
			Scope:     harness.fixture.Scope,
			AttemptID: "foreign",
			Segment:   foreign,
			RowCount:  1,
		}); err != nil {
			t.Fatalf("publish the foreign segment: %v", err)
		}
		if _, err := harness.attempt(harness.page(5)); !errors.Is(err, etl.ErrDatasetNotCheckpointed) {
			t.Fatalf("page after a foreign publication = %v; want a non-page-dataset refusal", err)
		}
		state, err := harness.manifest.State(context.Background(), harness.fixture.Actor, harness.fixture.Scope)
		if err != nil {
			t.Fatalf("load the committed dataset state: %v", err)
		}
		var pageOrdinals []uint64
		for _, segment := range state.Segments {
			if segment.ArtifactID != foreignAttemptArtifactID(t, harness.layout, "foreign") {
				pageOrdinals = append(pageOrdinals, segment.Ordinal)
			}
		}
		if !reflect.DeepEqual(pageOrdinals, []uint64{1, 2, 3, 4}) {
			t.Fatalf("acknowledged page ordinals after a foreign publication = %v; want [1 2 3 4]", pageOrdinals)
		}
	})
}

func foreignAttemptArtifactID(t *testing.T, layout warehouse.WarehouseLayout, attemptID string) string {
	t.Helper()
	artifactID, err := layout.SegmentArtifactID(attemptID)
	if err != nil {
		t.Fatalf("compose the foreign artifact identity: %v", err)
	}
	return artifactID
}

// etlHarness owns only what this case needs from the selected stack: the
// PostgreSQL fixture, both selected artifact gateways, the real restricted
// query child, the owner-only admitted directory the child may write, and the
// page protocol under test.
type etlHarness struct {
	t          *testing.T
	fixture    *testfixture.Fixture
	producer   *app.ArtifactResources
	consumer   *app.ArtifactResources
	publisher  *objects.Publisher
	manifest   *warehouse.DatasetManifest
	checkpoint *etl.ETLCheckpoint
	layout     warehouse.WarehouseLayout
	child      *warehouse.QueryChild
	root       string
	admitted   string
	materials  string
	staging    string
	bounds     warehouse.QueryBounds
	columns    []warehouse.QueryColumn
}

func newETLHarness(t *testing.T) *etlHarness {
	t.Helper()
	if os.Getenv("PM_STACK_TEST_CONFIG") == "" && os.Getenv("PM_STORE_TEST_SECRETS_DIR") == "" {
		// Missing fixture inputs fail rather than skip.
		t.Fatal("this physical case requires the explicitly selected owned stack fixture; missing inputs fail rather than skip")
	}
	harness := &etlHarness{
		t:        t,
		producer: testfixture.OpenArtifacts(t, testfixture.PrimaryArtifactGateway),
		consumer: testfixture.OpenArtifacts(t, testfixture.SecondaryArtifactGateway),
		bounds:   etlQueryBounds(),
		columns: []warehouse.QueryColumn{
			{Name: "id", Kind: warehouse.QueryKindInt64, Nullable: false},
			{Name: "note", Kind: warehouse.QueryKindText, Nullable: false},
		},
	}
	// The child is admitted to one root only: its own materials and the reader
	// staging both live below it.
	harness.root = t.TempDir()
	harness.admitted = filepath.Join(harness.root, "admitted")
	harness.materials = filepath.Join(harness.admitted, "materials")
	harness.staging = filepath.Join(harness.admitted, "staging")
	for _, directory := range []string{harness.admitted, harness.materials, harness.staging} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatalf("create the admitted directory %s: %v", filepath.Base(directory), err)
		}
	}
	harness.child = newRestrictedQueryChild(t, harness.admitted)
	harness.fixture = testfixture.New(t)
	layout, err := warehouse.NewWarehouseLayout(
		harness.fixture.Connection.ConnectorID, harness.fixture.Connection.ConnectionID, etlDatasetName,
	)
	if err != nil {
		t.Fatalf("compose the dataset namespace: %v", err)
	}
	harness.layout = layout
	if err := store.WithinScope(context.Background(), harness.fixture.Pool, harness.fixture.Scope, harness.fixture.Actor,
		func(ctx context.Context, tx *store.Transaction) error {
			_, err := store.RegisterWarehouseDataset(ctx, tx, store.WarehouseDataset{
				Scope:        harness.fixture.Scope,
				DatasetID:    layout.DatasetID(),
				ConnectorID:  layout.ConnectorID,
				ConnectionID: layout.ConnectionID,
			})
			return err
		}); err != nil {
		t.Fatalf("register the dataset identity: %v", err)
	}
	harness.publisher = harness.newPublisher(t, harness.producer)
	manifest, err := warehouse.NewDatasetManifest(harness.fixture.Pool, harness.publisher, layout, etlMaximumSegment)
	if err != nil {
		t.Fatalf("construct the dataset manifest publisher: %v", err)
	}
	harness.manifest = manifest
	checkpoint, err := etl.NewETLCheckpoint(harness.fixture.Pool, manifest, harness.child, harness.materials, etlMaximumSegment, harness.bounds)
	if err != nil {
		t.Fatalf("construct the ETL checkpoint: %v", err)
	}
	harness.checkpoint = checkpoint
	return harness
}

func (harness *etlHarness) newPublisher(t *testing.T, resources *app.ArtifactResources) *objects.Publisher {
	t.Helper()
	publisher, err := objects.NewPublisher(harness.fixture.Pool, resources.Backend, resources.Limits)
	if err != nil {
		t.Fatalf("open the selected shared-object publisher: %v", err)
	}
	return publisher
}

// rebind rebuilds the publication surface against the fixture's current pool
// and the independent gateway, which is what a replacement host owns after the
// previous host's pool and object scratch are gone.
func (harness *etlHarness) rebind(t *testing.T) {
	t.Helper()
	harness.publisher = harness.newPublisher(t, harness.consumer)
	manifest, err := warehouse.NewDatasetManifest(harness.fixture.Pool, harness.publisher, harness.layout, etlMaximumSegment)
	if err != nil {
		t.Fatalf("construct the replacement host manifest: %v", err)
	}
	harness.manifest = manifest
	checkpoint, err := etl.NewETLCheckpoint(
		harness.fixture.Pool, manifest, harness.child, harness.materials, etlMaximumSegment, harness.bounds,
	)
	if err != nil {
		t.Fatalf("construct the replacement host checkpoint: %v", err)
	}
	harness.checkpoint = checkpoint
}

// rows declares one exact run of typed rows across the dataset's pages.
func (harness *etlHarness) rows(from, to int) [][]warehouse.QueryValue {
	rows := make([][]warehouse.QueryValue, 0, to-from)
	for index := from; index < to; index++ {
		rows = append(rows, []warehouse.QueryValue{
			warehouse.Int64QueryValue(int64(index)),
			warehouse.TextQueryValue("bounded page row " + strconv.Itoa(index)),
		})
	}
	return rows
}

// page declares the acknowledgement of one position's page.
func (harness *etlHarness) page(position uint64) etl.PageRequest {
	from := int(position-1) * etlPageRows
	rows := harness.rows(from, from+etlPageRows)
	return etl.PageRequest{
		Actor:        harness.fixture.Actor,
		Scope:        harness.fixture.Scope,
		Semantics:    etl.ETLSemanticsPolling,
		Position:     position,
		DeclaredRows: int64(len(rows)),
		Rows:         rows,
		Columns:      harness.columnCopy(),
		RequestID:    "page-" + strconv.FormatUint(position, 10),
	}
}

func (harness *etlHarness) columnCopy() []warehouse.QueryColumn {
	return append([]warehouse.QueryColumn(nil), harness.columns...)
}

// commit acknowledges one page and fails the case when it is refused.
func (harness *etlHarness) commit(t *testing.T, page etl.PageRequest) etl.Checkpoint {
	t.Helper()
	checkpoint, err := harness.attempt(page)
	if err != nil {
		t.Fatalf("acknowledge page %d: %v", page.Position, err)
	}
	return checkpoint
}

func (harness *etlHarness) attempt(page etl.PageRequest) (etl.Checkpoint, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return harness.checkpoint.CommitPage(ctx, page)
}

// assertPrefix reads the durable state back for itself: the committed
// checkpoint names exactly the acknowledged position and the page it
// registered, the registry is dense from one to that position, and every
// registered object is readable through the independent gateway.
func (harness *etlHarness) assertPrefix(t *testing.T, position uint64) {
	t.Helper()
	checkpoint, err := harness.checkpoint.Load(context.Background(), harness.fixture.Actor, harness.fixture.Scope)
	if err != nil {
		t.Fatalf("load the committed checkpoint: %v", err)
	}
	if checkpoint.Position != position {
		t.Fatalf("committed checkpoint = position %d; want %d", checkpoint.Position, position)
	}
	if checkpoint.Rows != etlPageRows {
		t.Fatalf("committed checkpoint reports %d rows for its acknowledged page; want %d",
			checkpoint.Rows, etlPageRows)
	}
	state, err := harness.manifest.State(context.Background(), harness.fixture.Actor, harness.fixture.Scope)
	if err != nil {
		t.Fatalf("load the committed dataset state: %v", err)
	}
	if state.Revision != position || state.Cursor.Ordinal != position {
		t.Fatalf("dataset committed revision %d at cursor %d; want %d", state.Revision, state.Cursor.Ordinal, position)
	}
	if len(state.Segments) != int(position) {
		t.Fatalf("registered ledger holds %d segments for %d acknowledged pages", len(state.Segments), position)
	}
	var registered int64
	for index, segment := range state.Segments {
		if segment.Ordinal != uint64(index+1) || segment.ManifestRevision != segment.Ordinal {
			t.Fatalf("ledger position %d carries ordinal %d at revision %d",
				index, segment.Ordinal, segment.ManifestRevision)
		}
		registered += segment.RowCount
		if err := harness.consumer.Backend.Verify(context.Background(), segment.Object); err != nil {
			t.Fatalf("registered segment %d is unreadable through the independent gateway: %v", index, err)
		}
	}
	if registered != int64(position)*etlPageRows {
		t.Fatalf("registered ledger holds %d rows over %d pages; want %d",
			registered, position, int64(position)*etlPageRows)
	}
}

// assertRows reads the acknowledged prefix back through an independent query
// service over the second gateway and the caller's own connection.
func (harness *etlHarness) assertRows(t *testing.T, pool *store.Pool, backend *objects.S3, want [][]warehouse.QueryValue) {
	t.Helper()
	service, err := warehouse.NewQueryService(pool, backend, harness.child, harness.staging)
	if err != nil {
		t.Fatalf("construct the reader query service: %v", err)
	}
	response, err := service.Query(context.Background(), warehouse.DatasetQueryRequest{
		Actor:   harness.fixture.Actor,
		Scope:   harness.fixture.Scope,
		Layout:  harness.layout,
		QueryID: "prefix-readback",
		Query: warehouse.SavedQuery{
			Name:     "orders_all",
			Relation: etlDatasetName,
			Project: []warehouse.QueryProjection{
				{Column: "id"}, {Column: "note"},
			},
			Order: []warehouse.QueryOrder{{Column: "id"}},
			Limit: int64(len(want)) + 1,
		},
		MaximumSegmentBytes: etlMaximumSegment,
		Bounds:              harness.bounds,
	})
	if err != nil {
		t.Fatalf("read the acknowledged prefix: %v", err)
	}
	if !reflect.DeepEqual(response.Rows, want) {
		t.Fatalf("acknowledged prefix rows differ:\n got %d rows\nwant %d rows", len(response.Rows), len(want))
	}
}

// foreignSegment materializes admitted Parquet bytes that this protocol never
// published as a page.
func (harness *etlHarness) foreignSegment(t *testing.T) warehouse.SegmentBytes {
	t.Helper()
	output := filepath.Join(harness.materials, "foreign.parquet")
	segment, err := warehouse.MaterializeParquetSegment(context.Background(), harness.child, warehouse.MaterializeRequest{
		RequestID:           "foreign",
		AdmittedDirectories: []string{harness.materials},
		OutputPath:          output,
		Columns:             harness.columnCopy(),
		Rows:                harness.rows(etlPageRows*4, etlPageRows*4+1),
		Bounds:              harness.bounds,
		MaximumBytes:        etlMaximumSegment,
	})
	if err != nil {
		t.Fatalf("materialize the foreign segment: %v", err)
	}
	return segment
}

// etlQueryBounds leaves the response envelope inside the supervisor's own cap.
func etlQueryBounds() warehouse.QueryBounds {
	return warehouse.QueryBounds{
		Relations:   4,
		Columns:     32,
		Rows:        1 << 16,
		Bytes:       warehouse.MaxQueryDocumentBytes / 4,
		TempBytes:   64 << 20,
		MemoryBytes: 128 << 20,
		WallTime:    time.Minute,
	}
}

func etlChildLimits() warehouse.ChildLimits {
	return warehouse.ChildLimits{
		MaxRequestBytes:     warehouse.MaxQueryDocumentBytes,
		MaxResponseBytes:    warehouse.MaxQueryDocumentBytes / 2,
		MaxDiagnosticsBytes: 64 << 10,
		MemoryBytes:         256 << 20,
		TempBytes:           256 << 20,
		WallTime:            2 * time.Minute,
		StartupTimeout:      30 * time.Second,
	}
}

// newRestrictedQueryChild builds cmd/pm-query with the pinned toolchain. The
// child is the only process that links the native analytical engine, so the
// acceptance case must drive a real binary rather than an in-test stand-in.
func newRestrictedQueryChild(t *testing.T, admittedRoot string) *warehouse.QueryChild {
	t.Helper()
	root := isolationRepositoryRoot(t)
	output := filepath.Join(t.TempDir(), "pm-query")
	environment := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "CGO_CFLAGS", "CGO_CPPFLAGS", "CGO_CXXFLAGS", "CGO_LDFLAGS":
			continue
		default:
			environment = append(environment, entry)
		}
	}
	environment = append(environment, "CGO_ENABLED=1")
	command := exec.CommandContext(t.Context(), "python3", filepath.Join(root, "tools", "bootstrap.py"),
		"exec", "go", "--", "build", "-mod=readonly", "-o", output, "./cmd/pm-query")
	command.Dir = root
	command.Env = environment
	combined, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build the restricted query child: %v\n%s", err, combined)
	}
	scratch := filepath.Join(t.TempDir(), "scratch")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatalf("create the supervisor scratch root: %v", err)
	}
	child, err := warehouse.NewQueryChild(warehouse.ChildConfig{
		BinaryPath:    output,
		ScratchRoot:   scratch,
		AdmittedRoots: []string{admittedRoot},
		Limits:        etlChildLimits(),
	})
	if err != nil {
		t.Fatalf("construct the query child supervisor: %v", err)
	}
	return child
}

// isolationRepositoryRoot resolves the checkout the owned fixture was started
// for, so the child is built from the same source this test binary came from.
func isolationRepositoryRoot(t *testing.T) string {
	t.Helper()
	if root := os.Getenv("PM_STACK_ROOT"); filepath.IsAbs(root) {
		if info, err := os.Stat(filepath.Join(root, "tools", "bootstrap.py")); err == nil && info.Mode().IsRegular() {
			return root
		}
	}
	if runfiles := os.Getenv("TEST_SRCDIR"); runfiles != "" {
		workspace := os.Getenv("TEST_WORKSPACE")
		if workspace == "" {
			t.Fatal("declared Bazel workspace name is required to locate the checkout")
		}
		candidate := filepath.Join(runfiles, workspace)
		if info, err := os.Stat(filepath.Join(candidate, "tools", "bootstrap.py")); err == nil && info.Mode().IsRegular() {
			return candidate
		}
		t.Fatal("declared runfiles tree does not carry tools/bootstrap.py; run this case through the owned fixture")
	}
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve the working directory: %v", err)
	}
	for {
		if info, err := os.Stat(filepath.Join(directory, "tools", "bootstrap.py")); err == nil && info.Mode().IsRegular() {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("the checkout root carrying tools/bootstrap.py was not found above the working directory")
		}
		directory = parent
	}
}
