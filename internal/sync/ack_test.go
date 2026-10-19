package sync_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/schema"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	etl "github.com/polymetrics-ai/pm-cli/internal/sync"
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

// The ETL-QUERY-PUBLIC case installs the two read dependencies no composition
// in this tree binds yet: the service's warehouse resources, and the
// server-owned root of the connection's promoted metadata snapshot.
const (
	// etlSchemaCapability names the declared metadata capability the promoted
	// discovery was observed through.
	etlSchemaCapability = "orders_metadata"
	// etlSchemaPermissionRevision and etlSchemaSourceRevision are the declared
	// revisions the promoted discovery is bound to. The schema route serves a
	// snapshot only for the exact binding the caller asks about.
	etlSchemaPermissionRevision = "fixture-permission-1"
	etlSchemaSourceRevision     = "fixture-source-1"
	// childStartAnnouncement is the restricted child's own startup line, written
	// before it reads any request. The supervisor forwards every child
	// diagnostic byte to the writer the harness installs, so this line counts
	// real child processes rather than executions the read surface claims.
	childStartAnnouncement = "pm-query ready pid="
	// etlStartSettle bounds the wait for a started child's announcement to
	// reach the counting writer.
	etlStartSettle = 30 * time.Second
	// etlHTTPTimeout bounds one request to the fixture's authenticated router.
	etlHTTPTimeout = 2 * time.Minute
	// acknowledgedPages is how many pages the public-path case acknowledges
	// through the checkpoint protocol before it reads. It fixes every derived
	// expectation in that case: the registered ledger holds this many segments,
	// and the query service runs one bounded analytical attempt per registered
	// segment, so one answered query is this many child processes.
	acknowledgedPages = 2
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

// etlHarness owns only what this file's cases need from the selected stack: the
// PostgreSQL fixture, both selected artifact gateways, the real restricted
// query child, the owner-only admitted directory the child may write, the page
// protocol under test, and the count of real child processes the fixture's
// router reached.
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
	// starts counts the restricted child's own startup announcements, and
	// metadataRoot is the server-owned root the case installs on the HTTP
	// service together with the snapshot it promoted for the fixture's own
	// connection.
	starts       *childStarts
	metadataRoot string
	metadata     *connectionMetadata
}

// connectionMetadata is the connection state the case installed beneath the
// server-owned root: the exact binding the schema route must ask about and the
// snapshot the case promoted into it.
type connectionMetadata struct {
	binding  schema.Binding
	snapshot schema.Snapshot
}

// childStarts counts the restricted child's own startup announcements. Every
// count is observed from the child process itself; nothing here trusts the read
// surface's account of what it executed.
type childStarts struct {
	mutex   sync.Mutex
	written strings.Builder
	starts  int
}

func (starts *childStarts) Write(data []byte) (int, error) {
	starts.mutex.Lock()
	defer starts.mutex.Unlock()
	starts.written.Write(data)
	starts.starts = strings.Count(starts.written.String(), childStartAnnouncement)
	return len(data), nil
}

func (starts *childStarts) count() int {
	starts.mutex.Lock()
	defer starts.mutex.Unlock()
	return starts.starts
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
		starts:   &childStarts{},
		// The server-owned metadata root exists before the service is
		// constructed; the connection's own state directory and the promoted
		// snapshot are written into it once the fixture owns an identity.
		metadataRoot: t.TempDir(),
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
	harness.child = newRestrictedQueryChild(t, harness.admitted, harness.starts)
	harness.fixture = testfixture.New(t, harness.installReadSurface)
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
	harness.metadata = harness.promoteConnectionSchema(t)
	return harness
}

// installReadSurface declares the composition this case reads through: the
// warehouse resources and the connection metadata root that no composition in
// this tree binds yet. The registered bytes are read through the fixture's own
// second shared gateway by the same restricted child the checkpoint protocol
// publishes through, and the metadata root is the case-owned directory the
// promoted snapshot was written into. The fixture re-runs this configurator on
// every restart, so a replacement host installs the surface against its own
// pool.
func (harness *etlHarness) installReadSurface(
	dependencies testfixture.ServiceDependencies,
	config *httpapi.ServiceConfig,
) error {
	config.Warehouse = &httpapi.WarehouseResources{
		Pool:                dependencies.Pool,
		Backend:             harness.consumer.Backend,
		Child:               harness.child,
		StagingRoot:         harness.staging,
		MaximumSegmentBytes: etlMaximumSegment,
		Bounds:              harness.bounds,
	}
	config.ConnectionMetadataRoot = harness.metadataRoot
	return nil
}

// promoteConnectionSchema installs the connection's own metadata state beneath
// the server-owned root. The case composes one complete declared discovery for
// the fixture's connection and promotes it exactly as a discovery boundary
// would, so the schema route reads a genuinely promoted snapshot rather than an
// arrangement the route was taught to expect.
func (harness *etlHarness) promoteConnectionSchema(t *testing.T) *connectionMetadata {
	t.Helper()
	return harness.promoteWorkspaceSchema(t, harness.fixture.Scope, []schema.Field{
		{
			Name:     "id",
			Pointer:  "/properties/id",
			Schema:   contract.Schema{Type: contract.SchemaTypeInteger},
			Required: true,
		},
		{
			Name:     "note",
			Pointer:  "/properties/note",
			Schema:   contract.Schema{Type: contract.SchemaTypeString},
			Required: true,
		},
	})
}

// promoteWorkspaceSchema promotes one complete declared discovery of the
// fixture's connection identity for one workspace. The state directory is the
// one the store namespaces by organization, workspace and connection, because a
// connection identity is unique only within its workspace.
func (harness *etlHarness) promoteWorkspaceSchema(
	t *testing.T,
	scope contract.Scope,
	fields []schema.Field,
) *connectionMetadata {
	t.Helper()
	connection := harness.fixture.Connection
	binding := schema.Binding{
		OrgID:              scope.OrgID,
		WorkspaceID:        scope.WorkspaceID,
		ConnectorID:        connection.ConnectorID,
		ConnectionID:       connection.ConnectionID,
		ProviderAccountID:  connection.ProviderAccountID,
		PermissionRevision: etlSchemaPermissionRevision,
		Resource:           etlDatasetName,
		SourceRevision:     etlSchemaSourceRevision,
	}
	store := harness.connectionSchemaStore(t, scope)
	candidate, err := schema.Discover(binding, []schema.Observation{{
		Capability: etlSchemaCapability,
		Resource:   binding.Resource,
		Available:  true,
		Fields:     fields,
	}})
	if err != nil {
		t.Fatalf("declare the connection discovery: %v", err)
	}
	if !candidate.Completeness.Complete {
		t.Fatal("the declared connection discovery is not complete")
	}
	snapshot, err := store.Promote(binding, candidate, "")
	if err != nil {
		t.Fatalf("promote the connection schema snapshot: %v", err)
	}
	return &connectionMetadata{binding: binding, snapshot: snapshot}
}

// connectionSchemaStore creates and opens the private state directory of the
// fixture's connection identity in one workspace beneath the server-owned root.
func (harness *etlHarness) connectionSchemaStore(t *testing.T, scope contract.Scope) *schema.Store {
	t.Helper()
	connectionID := harness.fixture.Connection.ConnectionID
	directory, err := schema.ConnectionStateDirectory(harness.metadataRoot, scope, connectionID)
	if err != nil {
		t.Fatalf("name the connection metadata state directory: %v", err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("create the connection metadata state directory: %v", err)
	}
	store, err := schema.NewConnectionStore(harness.metadataRoot, scope, connectionID)
	if err != nil {
		t.Fatalf("open the connection metadata store: %v", err)
	}
	return store
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
// starts receives the child's own startup announcement, which is how this file
// counts real child processes instead of executions the surface claims.
func newRestrictedQueryChild(t *testing.T, admittedRoot string, starts io.Writer) *warehouse.QueryChild {
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
		Diagnostics:   starts,
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

// TestETLQueryPublicPath is the ETL-QUERY-PUBLIC acceptance case. It drives the
// three warehouse read routes this candidate added through the real
// authenticated router, over pages the checkpoint protocol genuinely published,
// with the read surface and the connection metadata root installed by the case
// itself. The case observes the answers rather than the handlers' own account of
// them: the schema read is checked against the snapshot the case promoted, the
// dataset read against the registered ledger in the database, the typed query
// against the exact expected rows and types, every concealed or refused request
// against the shared problem document, and every one of them against the number
// of real restricted child processes that announced their own startup.
func TestETLQueryPublicPath(t *testing.T) {
	harness := newETLHarness(t)
	// The public path reads what the protocol registered: acknowledged pages,
	// published and cursor-advancing in order.
	for position := 1; position <= acknowledgedPages; position++ {
		harness.commit(t, harness.page(uint64(position)))
	}
	published := harness.starts.count()
	workspace := harness.fixture.Scope.WorkspaceID
	client := harness.fixture.Client
	// concurrent is the number of authorized typed queries raced at once.
	const concurrent = 4

	t.Run("connection_schema_read_answers_the_promoted_snapshot", func(t *testing.T) {
		before := harness.starts.count()
		response := harness.send(t, client, http.MethodGet, harness.schemaRoutePath(workspace), nil)
		inspection := decodeWarehouseRoute[api.ConnectionSchemaInspection](t, response, http.StatusOK)
		harness.requireConnectionIdentity(t, "the connection schema read",
			inspection.OrgId, inspection.WorkspaceId, inspection.ConnectorId, inspection.ConnectionId)
		binding := harness.metadata.binding
		selected, err := inspection.Selected.Get()
		if err != nil || selected != string(harness.metadata.snapshot.Fingerprint) {
			t.Fatalf("connection schema selected = %q (%v); want the promoted %s",
				selected, err, harness.metadata.snapshot.Fingerprint)
		}
		snapshot := inspection.Snapshot
		if snapshot.ContentFingerprint != string(harness.metadata.snapshot.Fingerprint) ||
			!snapshot.Complete || len(snapshot.Blockers) != 0 ||
			snapshot.FieldCount != len(harness.metadata.snapshot.Fields) {
			t.Fatalf("connection schema snapshot = %#v; want the complete promoted snapshot over %d fields",
				snapshot, len(harness.metadata.snapshot.Fields))
		}
		if snapshot.ConnectorId != binding.ConnectorID || snapshot.ConnectionId != binding.ConnectionID ||
			snapshot.ProviderAccountId != binding.ProviderAccountID ||
			snapshot.PermissionRevision != binding.PermissionRevision ||
			snapshot.Resource != binding.Resource || snapshot.SourceRevision != binding.SourceRevision {
			t.Fatalf("connection schema snapshot is bound to %#v; want the promoted %#v", snapshot, binding)
		}
		want := []api.SchemaField{
			{Name: "id", Pointer: "/properties/id", Type: api.SchemaFieldTypeInteger, Required: true},
			{Name: "note", Pointer: "/properties/note", Type: api.SchemaFieldTypeString, Required: true},
		}
		if !reflect.DeepEqual(inspection.Fields, want) {
			t.Fatalf("connection schema fields = %#v; want the promoted %#v", inspection.Fields, want)
		}
		// Reading a promoted snapshot never runs the analytical child.
		harness.requireStartDelta(t, before, 0, "the connection schema read")
	})

	t.Run("dataset_read_answers_the_registered_ledger", func(t *testing.T) {
		before := harness.starts.count()
		response := harness.send(t, client, http.MethodGet, harness.datasetRoutePath(workspace), nil)
		view := decodeWarehouseRoute[api.WarehouseDatasetSnapshot](t, response, http.StatusOK)
		harness.requireConnectionIdentity(t, "the dataset read",
			view.OrgId, view.WorkspaceId, view.ConnectorId, view.ConnectionId)
		if view.Dataset != etlDatasetName {
			t.Fatalf("dataset read names dataset %q; want %q", view.Dataset, etlDatasetName)
		}
		// The comparison is the durable ledger the protocol committed, read
		// directly from the database rather than from the answer under test.
		state, err := harness.manifest.State(context.Background(), harness.fixture.Actor, harness.fixture.Scope)
		if err != nil {
			t.Fatalf("load the committed dataset state: %v", err)
		}
		if len(state.Segments) != acknowledgedPages {
			t.Fatalf("the acknowledged prefix registered %d segments; want the %d acknowledged pages",
				len(state.Segments), acknowledgedPages)
		}
		if view.Revision != int64(state.Revision) || view.CursorOrdinal != int64(state.Cursor.Ordinal) {
			t.Fatalf("dataset read reports revision %d at cursor %d; want the committed revision %d at cursor %d",
				view.Revision, view.CursorOrdinal, state.Revision, state.Cursor.Ordinal)
		}
		if len(view.Segments) != len(state.Segments) {
			t.Fatalf("dataset read reports %d segments; want the %d registered ones", len(view.Segments), len(state.Segments))
		}
		var reported int64
		for index, registered := range state.Segments {
			segment := view.Segments[index]
			reported += segment.RowCount
			if segment.Ordinal != int64(registered.Ordinal) ||
				segment.ManifestRevision != int64(registered.ManifestRevision) ||
				segment.RowCount != registered.RowCount || segment.State != api.Live ||
				segment.ColumnFingerprint != registered.ColumnFingerprint ||
				segment.Object.ArtifactId != registered.Object.ArtifactID ||
				segment.Object.ByteLength != registered.Object.ByteLength ||
				segment.Object.Sha256 != registered.Object.SHA256 ||
				segment.Object.MediaType != registered.Object.MediaType ||
				segment.Object.BackendGeneration != registered.Object.BackendGeneration {
				t.Fatalf("dataset read segment %d = %#v; want the registered %#v", index, segment, registered)
			}
			if err := harness.consumer.Backend.Verify(context.Background(), registered.Object); err != nil {
				t.Fatalf("registered segment %d is unreadable through the independent gateway: %v", index, err)
			}
		}
		if reported != int64(acknowledgedPages*etlPageRows) || view.Rows != reported {
			t.Fatalf("dataset read reports %d rows (%d in its segments); want the %d acknowledged rows",
				view.Rows, reported, acknowledgedPages*etlPageRows)
		}
		harness.requireStartDelta(t, before, 0, "the dataset read")
	})

	t.Run("typed_query_answers_the_registered_rows", func(t *testing.T) {
		before := harness.starts.count()
		response := harness.send(t, client, http.MethodPost, harness.queryRoutePath(workspace),
			harness.queryDocument(etlDatasetName, int64(acknowledgedPages*etlPageRows)+1))
		result := decodeWarehouseRoute[api.WarehouseQueryResult](t, response, http.StatusOK)
		harness.requireConnectionIdentity(t, "the dataset query",
			result.OrgId, result.WorkspaceId, result.ConnectorId, result.ConnectionId)
		if result.Dataset != etlDatasetName {
			t.Fatalf("dataset query names dataset %q; want %q", result.Dataset, etlDatasetName)
		}
		wantColumns := []api.WarehouseColumn{
			{Name: "id", Kind: api.WarehouseColumnKindInt64, Nullable: false},
			{Name: "note", Kind: api.WarehouseColumnKindText, Nullable: false},
		}
		if !reflect.DeepEqual(result.Columns, wantColumns) {
			t.Fatalf("dataset query columns = %#v; want the registered %#v", result.Columns, wantColumns)
		}
		if result.Truncated {
			t.Fatal("dataset query reports a truncated answer for a limit above its registered row count")
		}
		wantRows := harness.wireRows(0, acknowledgedPages*etlPageRows)
		if len(result.Rows) != len(wantRows) {
			t.Fatalf("dataset query answered %d rows; want the %d acknowledged rows", len(result.Rows), len(wantRows))
		}
		for index, row := range result.Rows {
			if len(row) != len(wantRows[index]) {
				t.Fatalf("dataset query row %d carries %d cells; want %d", index, len(row), len(wantRows[index]))
			}
			for cell, value := range row {
				if !value.Equal(wantRows[index][cell]) {
					t.Fatalf("dataset query row %d cell %d = %#v; want the exact typed %#v",
						index, cell, value, wantRows[index][cell])
				}
			}
		}
		// The query service runs one bounded analytical attempt per registered
		// segment, so one answer over the acknowledged prefix is one real child
		// process per acknowledged page.
		harness.requireStartDelta(t, before, acknowledgedPages, "the typed query")
	})

	t.Run("concurrent_typed_queries_stage_privately", func(t *testing.T) {
		before := harness.starts.count()
		staged := func() []string {
			entries, err := os.ReadDir(harness.staging)
			if err != nil {
				t.Fatalf("read the query staging root: %v", err)
			}
			names := make([]string, 0, len(entries))
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			return names
		}
		retained := staged()
		encoded, err := json.Marshal(harness.queryDocument(etlDatasetName, int64(acknowledgedPages*etlPageRows)+1))
		if err != nil {
			t.Fatalf("encode the declared query: %v", err)
		}
		endpoint := harness.queryRoutePath(workspace)
		type answer struct {
			response warehouseRouteResponse
			err      error
		}
		answers := make(chan answer, concurrent)
		start := make(chan struct{})
		for range concurrent {
			go func() {
				<-start
				ctx, cancel := context.WithTimeout(context.Background(), etlHTTPTimeout)
				defer cancel()
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
				if err != nil {
					answers <- answer{err: err}
					return
				}
				request.Header.Set("Content-Type", "application/json")
				response, err := client.Do(request)
				if err != nil {
					answers <- answer{err: err}
					return
				}
				defer func() { _ = response.Body.Close() }()
				document, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
				answers <- answer{response: warehouseRouteResponse{status: response.StatusCode, document: document}, err: err}
			}()
		}
		close(start)
		wantRows := acknowledgedPages * etlPageRows
		for range concurrent {
			got := <-answers
			if got.err != nil {
				t.Fatalf("concurrent authorized query failed to complete: %v", got.err)
			}
			result := decodeWarehouseRoute[api.WarehouseQueryResult](t, got.response, http.StatusOK)
			if result.Truncated || len(result.Rows) != wantRows {
				t.Fatalf("concurrent query answered %d rows (truncated=%t); want the %d acknowledged rows",
					len(result.Rows), result.Truncated, wantRows)
			}
		}
		harness.requireStartDelta(t, before, concurrent*acknowledgedPages, "the concurrent typed queries")
		// Every execution's private staging directory is removed once it answers.
		if after := staged(); !slices.Equal(after, retained) {
			t.Fatalf("query staging root holds %v after every query answered; want only the prior %v", after, retained)
		}
	})

	t.Run("a_wrong_workspace_or_another_callers_authority_is_concealed", func(t *testing.T) {
		before := harness.starts.count()
		foreignWorkspace := harness.fixture.ForeignScope.WorkspaceID
		for _, route := range []struct {
			what     string
			method   string
			endpoint string
			body     any
		}{
			{"the connection schema read in another workspace", http.MethodGet,
				harness.schemaRoutePath(foreignWorkspace), nil},
			{"the dataset read in another workspace", http.MethodGet,
				harness.datasetRoutePath(foreignWorkspace), nil},
			{"the query in another workspace", http.MethodPost,
				harness.queryRoutePath(foreignWorkspace), harness.queryDocument(etlDatasetName, int64(acknowledgedPages*etlPageRows)+1)},
		} {
			requireConcealed(t, harness.send(t, client, route.method, route.endpoint, route.body), route.what)
		}
		// The same connection is concealed from an authenticated caller of
		// another workspace, whose own authority never reaches it.
		stranger := harness.fixture.ClientFor(t, harness.fixture.ForeignActor)
		requireConcealed(t, harness.send(t, stranger, http.MethodGet, harness.datasetRoutePath(workspace), nil),
			"another workspace's caller reading the dataset")
		requireConcealed(t, harness.send(t, stranger, http.MethodPost, harness.queryRoutePath(workspace),
			harness.queryDocument(etlDatasetName, int64(acknowledgedPages*etlPageRows)+1)), "another workspace's caller querying the dataset")
		// Concealment is concealment: no concealed request reached the child.
		harness.requireStartDelta(t, before, 0, "every concealed read")
	})

	t.Run("the_connection_permission_is_required_and_sufficient", func(t *testing.T) {
		before := harness.starts.count()
		harness.withdrawConnectionRead(t, false)
		// The exact permission is restored for every later case even when an
		// assertion below stops this one.
		t.Cleanup(func() { harness.withdrawConnectionReadQuietly(t, true) })
		requireConcealed(t, harness.send(t, client, http.MethodGet, harness.datasetRoutePath(workspace), nil),
			"the dataset read without the connection permission")
		requireConcealed(t, harness.send(t, client, http.MethodPost, harness.queryRoutePath(workspace),
			harness.queryDocument(etlDatasetName, int64(acknowledgedPages*etlPageRows)+1)), "the query without the connection permission")
		harness.requireStartDelta(t, before, 0, "every read without the connection permission")
		// Restoring the exact permission makes the same read answer again, so
		// the concealment above is caused by the missing permission rather than
		// by an unavailable read surface.
		harness.withdrawConnectionRead(t, true)
		restored := harness.send(t, client, http.MethodGet, harness.datasetRoutePath(workspace), nil)
		if restored.status != http.StatusOK {
			t.Fatalf("dataset read after restoring the connection permission = HTTP %d; want 200: %s",
				restored.status, restored.document)
		}
		harness.requireStartDelta(t, before, 0, "the restored dataset read")
	})

	t.Run("raw_and_external_expressions_are_refused", func(t *testing.T) {
		before := harness.starts.count()
		for _, refusal := range []struct {
			what string
			body any
		}{
			// Statement text in the member that names the relation. The body is
			// otherwise the exact body the typed query above answered with, so
			// the refusal is caused by the expression alone.
			{"statement text in the relation", harness.queryDocument("SELECT id, note FROM orders", int64(acknowledgedPages*etlPageRows)+1)},
			// An external function expression naming a file outside the dataset.
			{"an external function expression in the relation", harness.queryDocument("read_parquet('/etc/passwd')", int64(acknowledgedPages*etlPageRows)+1)},
			// Statement text where this surface declares no member at all: the
			// declared request is closed, so raw text has nowhere to bind.
			{"statement text where the surface declares no member", map[string]any{"statement": "SELECT id, note FROM orders"}},
		} {
			response := harness.send(t, client, http.MethodPost, harness.queryRoutePath(workspace), refusal.body)
			if response.status != http.StatusBadRequest {
				t.Fatalf("a query carrying %s = HTTP %d; want the refused 400: %s",
					refusal.what, response.status, response.document)
			}
			problem := decodeWarehouseRoute[api.Problem](t, response, http.StatusBadRequest)
			if problem.Status != http.StatusBadRequest || problem.Code != "invalid_request" {
				t.Fatalf("a query carrying %s was refused as %#v; want the shared invalid_request problem",
					refusal.what, problem)
			}
		}
		harness.requireStartDelta(t, before, 0, "every refused expression")
	})

	// Across every route above, the only child processes that ran are the ones
	// the answered typed queries need, one per registered segment for the single
	// query and for each concurrent one. Every concealed and every refused
	// request ran none; the pages this case acknowledged are excluded by the
	// count taken after them.
	harness.requireStartDelta(t, published, (1+concurrent)*acknowledgedPages, "the public read path")
}

// requireConnectionIdentity asserts that one answered read reports the exact
// scope and connection that owns the dataset and the promoted snapshot.
func (harness *etlHarness) requireConnectionIdentity(t *testing.T, what, orgID, workspaceID, connectorID, connectionID string) {
	t.Helper()
	if orgID != harness.fixture.Scope.OrgID || workspaceID != harness.fixture.Scope.WorkspaceID ||
		connectorID != harness.fixture.Connection.ConnectorID ||
		connectionID != harness.fixture.Connection.ConnectionID {
		t.Fatalf("%s reported %s/%s/%s/%s; want the authorized connection of the fixture's own scope",
			what, orgID, workspaceID, connectorID, connectionID)
	}
}

// requireStartDelta asserts how many real child processes one observation
// started. A concealed or refused request must start none and an answered query
// must start exactly one; the count is the child's own startup announcement,
// which the supervisor forwards before the attempt can produce an answer, so a
// child that started is counted by the time the answer is observable.
func (harness *etlHarness) requireStartDelta(t *testing.T, before, want int, what string) {
	t.Helper()
	deadline := time.Now().Add(etlStartSettle)
	for {
		if started := harness.starts.count() - before; started >= want {
			if started != want {
				t.Fatalf("%s started %d query child process(es); want %d", what, started, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s started %d query child process(es); want %d", what, harness.starts.count()-before, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// wireRows declares the exact typed rows two acknowledged pages registered,
// independently of the read surface: the same values the checkpoint protocol
// published, projected onto the shared exact value union.
func (harness *etlHarness) wireRows(from, to int) [][]contract.PresentValue {
	rows := make([][]contract.PresentValue, 0, to-from)
	for index := from; index < to; index++ {
		integer, err := contract.NewIntegerValue(strconv.Itoa(index))
		if err != nil {
			harness.t.Fatalf("declare the expected integer row value %d: %v", index, err)
		}
		rows = append(rows, []contract.PresentValue{
			integer,
			contract.NewStringValue("bounded page row " + strconv.Itoa(index)),
		})
	}
	return rows
}

// queryDocument declares one bounded typed query in the closed wire form: a
// declared relation, a declared projection, a declared order and a declared
// limit. No member of it can carry statement text.
func (harness *etlHarness) queryDocument(relation string, limit int64) api.WarehouseQueryRequest {
	return api.WarehouseQueryRequest{
		Relation:   relation,
		Project:    []api.WarehouseProjection{{Column: "id"}, {Column: "note"}},
		Predicates: []api.WarehousePredicate{},
		Parameters: []api.WarehouseParameter{},
		Order:      []api.WarehouseOrdering{{Column: "id"}},
		Limit:      limit,
	}
}

// withdrawConnectionRead removes and restores the single permission the three
// read routes authorize with. The member keeps its workspace, its connection and
// every other permission, so a concealment it then observes is caused by the
// missing permission alone. Both directions rewrite the declared permission set
// in its own sorted order, and both are idempotent.
func (harness *etlHarness) withdrawConnectionRead(t *testing.T, restore bool) {
	t.Helper()
	// Revoking drops the entry and keeps the surrounding order; restoring folds
	// the entry back in, so the declared membership constraint holds either way.
	const revoke = `UPDATE pm.memberships
		SET permissions = COALESCE((
				SELECT array_agg(entry ORDER BY entry) FROM unnest(permissions) AS entry WHERE entry <> $4
			), ARRAY[]::text[]),
			control_revision = control_revision + 1
		WHERE org_id = $1 AND workspace_id = $2 AND identity_id = $3`
	const restorePermission = `UPDATE pm.memberships
		SET permissions = (
				SELECT array_agg(entry ORDER BY entry)
				FROM (SELECT unnest(permissions) AS entry UNION SELECT $4::text) AS merged
			),
			control_revision = control_revision + 1
		WHERE org_id = $1 AND workspace_id = $2 AND identity_id = $3`
	statement := revoke
	if restore {
		statement = restorePermission
	}
	harness.fixture.SetupSQL(t, harness.fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, statement,
			harness.fixture.Scope.OrgID, harness.fixture.Scope.WorkspaceID,
			harness.fixture.Actor.RequestedActor.PrincipalID, "connection.read")
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("the fixture membership update affected %d rows; want exactly one", tag.RowsAffected())
		}
		return nil
	})
}

// withdrawConnectionReadQuietly restores the permission without failing a case
// that has already reported its own outcome.
func (harness *etlHarness) withdrawConnectionReadQuietly(t *testing.T, restore bool) {
	t.Helper()
	defer func() { _ = recover() }()
	harness.withdrawConnectionRead(t, restore)
}

func (harness *etlHarness) schemaRoutePath(workspaceID string) string {
	return fmt.Sprintf("%s/v1/workspaces/%s/connections/%s/schema?resource=%s&permission_revision=%s&source_revision=%s",
		harness.fixture.BaseURL, url.PathEscape(workspaceID),
		url.PathEscape(harness.fixture.Connection.ConnectionID),
		url.QueryEscape(harness.metadata.binding.Resource),
		url.QueryEscape(harness.metadata.binding.PermissionRevision),
		url.QueryEscape(harness.metadata.binding.SourceRevision))
}

func (harness *etlHarness) datasetRoutePath(workspaceID string) string {
	return fmt.Sprintf("%s/v1/workspaces/%s/connections/%s/warehouse/datasets/%s",
		harness.fixture.BaseURL, url.PathEscape(workspaceID),
		url.PathEscape(harness.fixture.Connection.ConnectionID), url.PathEscape(etlDatasetName))
}

func (harness *etlHarness) queryRoutePath(workspaceID string) string {
	return harness.datasetRoutePath(workspaceID) + "/query"
}

// warehouseRouteResponse is one real answer from the fixture's authenticated
// router.
type warehouseRouteResponse struct {
	status   int
	document []byte
}

// send executes one request through the fixture's real TLS router with the
// caller's real session, and reads a bounded answer.
func (harness *etlHarness) send(
	t *testing.T,
	client *http.Client,
	method, endpoint string,
	body any,
) warehouseRouteResponse {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode the declared %s request: %v", method, err)
		}
		reader = bytes.NewReader(encoded)
	}
	ctx, cancel := context.WithTimeout(context.Background(), etlHTTPTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		t.Fatalf("construct the %s request: %v", method, err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("execute the %s request: %v", method, err)
	}
	defer func() { _ = response.Body.Close() }()
	document, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(document) > 1<<20 {
		t.Fatalf("the answer to %s exceeded its readable envelope: %v", method, err)
	}
	return warehouseRouteResponse{status: response.StatusCode, document: document}
}

func decodeWarehouseRoute[T any](t *testing.T, response warehouseRouteResponse, wantStatus int) T {
	t.Helper()
	if response.status != wantStatus {
		t.Fatalf("route answered HTTP %d; want %d: %s", response.status, wantStatus, response.document)
	}
	var decoded T
	if err := json.Unmarshal(response.document, &decoded); err != nil {
		t.Fatalf("decode the route answer: %v: %s", err, response.document)
	}
	return decoded
}

// requireConcealed asserts one refusal that must not reveal whether the
// connection, its metadata or its dataset exist.
func requireConcealed(t *testing.T, response warehouseRouteResponse, what string) {
	t.Helper()
	problem := decodeWarehouseRoute[api.Problem](t, response, http.StatusNotFound)
	if problem.Status != http.StatusNotFound || problem.Code != "resource_not_found" {
		t.Fatalf("%s was answered %#v; want the concealed resource_not_found problem", what, problem)
	}
}
