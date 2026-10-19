package sync_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"

	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/schema"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/warehouse"
)

// This file owns the read-selector refusal cases. Both read routes take
// caller-supplied selectors: the connection schema route takes the snapshot
// binding (permission revision, resource, source revision, provider account) and
// an optional snapshot identity, and the warehouse dataset route takes an
// optional retained snapshot. A selector that names nothing the authenticated
// scope can reach is correctable input, so both routes must answer it the way
// the artifact routes answer a restricted or unknown artifact: one concealed
// 404, never an internal fault. The positive paths keep answering the authorized
// document. The converse is owned here too: a selected snapshot whose stored
// payload cannot be read is no selector's mistake, so it keeps the store's own
// refusal instead of being reported as a missing one.
const (
	readRefusalForeignDataset  = "returns"
	readRefusalSnapshot        = "snapshot-orders-1"
	readRefusalUnknownSnapshot = "snapshot-unknown-1"
)

// TestConnectionReadSelectorRefusalsAreConcealed is the read-selector case.
// Every refusal below is produced before any object byte is read: the schema
// route never runs the analytical child, and the dataset route resolves the
// caller's selector before it stages a registered segment.
func TestConnectionReadSelectorRefusalsAreConcealed(t *testing.T) {
	harness := newETLHarness(t)
	harness.commit(t, harness.page(1))
	// A retained snapshot of this dataset, and a second registered dataset in the
	// same scope: together they let a selector name a snapshot that exists and one
	// that belongs to another dataset.
	retainReadSnapshot(t, harness)
	registerReadDataset(t, harness, readRefusalForeignDataset)

	workspace := harness.fixture.Scope.WorkspaceID
	client := harness.fixture.Client

	t.Run("connection_schema_selectors_the_binding_cannot_see_are_concealed", func(t *testing.T) {
		before := harness.starts.count()
		// The positive path first: the exact promoted binding answers the
		// authorized document, so the refusals below are caused by the selector
		// alone. Reading a promoted snapshot runs no child.
		authorized := decodeWarehouseRoute[api.ConnectionSchemaInspection](t,
			harness.send(t, client, http.MethodGet, schemaSelectorPath(harness, workspace, nil), nil), http.StatusOK)
		selected, err := authorized.Selected.Get()
		if err != nil || selected != string(harness.metadata.snapshot.Fingerprint) ||
			authorized.Snapshot.ContentFingerprint != string(harness.metadata.snapshot.Fingerprint) ||
			!authorized.Snapshot.Complete || len(authorized.Fields) != len(harness.metadata.snapshot.Fields) {
			t.Fatalf("connection schema read of the promoted binding = %#v (%v); want the complete promoted %s over %d fields",
				authorized, err, harness.metadata.snapshot.Fingerprint, len(harness.metadata.snapshot.Fields))
		}

		unknown := unknownSchemaSnapshot(t)
		for _, selector := range []struct {
			name   string
			value  string
			values url.Values
		}{
			{"another permission revision", "fixture-permission-other",
				url.Values{"permission_revision": {"fixture-permission-other"}}},
			{"another resource", "returns", url.Values{"resource": {"returns"}}},
			{"another source revision", "fixture-source-other",
				url.Values{"source_revision": {"fixture-source-other"}}},
			{"another provider account", "fixture-account-other",
				url.Values{"provider_account_id": {"fixture-account-other"}}},
			{"an unknown snapshot", string(unknown), url.Values{"snapshot_id": {string(unknown)}}},
		} {
			response := harness.send(t, client, http.MethodGet, schemaSelectorPath(harness, workspace, selector.values), nil)
			requireConcealed(t, response, "the connection schema read of "+selector.name)
			// A concealed refusal does not repeat the selector it refused.
			if bytes.Contains(response.document, []byte(selector.value)) {
				t.Fatalf("the concealed schema answer disclosed %q: %s", selector.value, response.document)
			}
		}
		harness.requireStartDelta(t, before, 0, "every schema selector refusal")
	})

	t.Run("dataset_snapshot_selectors_that_name_nothing_here_are_concealed", func(t *testing.T) {
		before := harness.starts.count()
		// The positive paths: the live read view, and the retained snapshot the
		// caller is entitled to. Neither stages a registered segment.
		live := decodeWarehouseRoute[api.WarehouseDatasetSnapshot](t,
			harness.send(t, client, http.MethodGet, harness.datasetRoutePath(workspace), nil), http.StatusOK)
		if live.Dataset != etlDatasetName || live.Revision != 1 || live.CursorOrdinal != 1 ||
			len(live.Segments) != 1 || live.Rows != etlPageRows {
			t.Fatalf("dataset read answered %#v; want the committed revision 1 at cursor 1 over one page", live)
		}
		if named, err := live.SnapshotId.Get(); err == nil {
			t.Fatalf("the unselected dataset read named snapshot %q", named)
		}
		retained := decodeWarehouseRoute[api.WarehouseDatasetSnapshot](t,
			harness.send(t, client, http.MethodGet,
				harness.datasetRoutePath(workspace)+"?snapshot_id="+readRefusalSnapshot, nil), http.StatusOK)
		if named, err := retained.SnapshotId.Get(); err != nil || named != readRefusalSnapshot {
			t.Fatalf("the retained snapshot read named %q (%v); want %q", named, err, readRefusalSnapshot)
		}

		// The refusals: a snapshot identity nothing stored, and a retained
		// snapshot that belongs to another dataset of the same scope. The typed
		// query route resolves the same selector before it reads anything, so it
		// answers the same way.
		for _, refusal := range []struct {
			what     string
			dataset  string
			snapshot string
		}{
			{"an unknown retained snapshot", etlDatasetName, readRefusalUnknownSnapshot},
			{"a retained snapshot of another dataset", readRefusalForeignDataset, readRefusalSnapshot},
		} {
			response := harness.send(t, client, http.MethodGet,
				readRefusalDatasetPath(harness, workspace, refusal.dataset)+"?snapshot_id="+refusal.snapshot, nil)
			requireConcealed(t, response, "the dataset read of "+refusal.what)
			if bytes.Contains(response.document, []byte(refusal.snapshot)) {
				t.Fatalf("the concealed dataset answer disclosed %q: %s", refusal.snapshot, response.document)
			}
			concealedQuery(t, harness, client, refusal.dataset, refusal.snapshot)
		}
		harness.requireStartDelta(t, before, 0, "every dataset selector refusal")
	})

	t.Run("a_snapshot_that_cannot_be_read_is_not_reported_as_a_missing_one", func(t *testing.T) {
		before := harness.starts.count()
		// The promoted payload is present and the positive paths above read it.
		// Closing its read permission leaves exactly the state the store answers
		// for itself: its private-file check admits a mode 0o000 payload and its
		// own read then fails. No selector the caller can name reaches that
		// payload, so the refusal is not the caller's to correct and must not be
		// concealed as a missing one — neither for the connection's own selection
		// nor for the same promoted identity declared explicitly, which names the
		// same stored payload.
		payload := promotedSchemaPayload(t, harness)
		if err := os.Chmod(payload, 0o000); err != nil {
			t.Fatalf("close the read permission of the promoted payload: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(payload, 0o600) })
		if _, err := os.ReadFile(payload); err == nil {
			t.Fatal("this case needs a process that cannot read a mode 0o000 payload, but this one still reads it")
		}
		for _, selector := range []struct {
			what   string
			values url.Values
		}{
			{"the selected snapshot", nil},
			{"the promoted identity declared explicitly",
				url.Values{"snapshot_id": {string(harness.metadata.snapshot.Fingerprint)}}},
		} {
			response := harness.send(t, client, http.MethodGet, schemaSelectorPath(harness, workspace, selector.values), nil)
			problem := decodeWarehouseRoute[api.Problem](t, response, http.StatusInternalServerError)
			if problem.Code != "internal_error" {
				t.Fatalf("the read of %s was answered %#v; want the store's own refusal for a payload it cannot read",
					selector.what, problem)
			}
		}
		harness.requireStartDelta(t, before, 0, "the unreadable-snapshot refusal")
	})
}

// TestConnectionSchemaIsolatesWorkspacesThatShareAConnectionID is the
// cross-workspace metadata case. Connection identities are workspace-scoped, so
// a second workspace may declare a connection with the very same identity,
// connector and provider account. The schema route must namespace and authorize
// snapshot state by organization, workspace and connection: the second
// workspace never reads the first workspace's snapshot, and naming that
// snapshot explicitly is concealed exactly like an unknown one.
func TestConnectionSchemaIsolatesWorkspacesThatShareAConnectionID(t *testing.T) {
	harness := newETLHarness(t)
	fixture := harness.fixture
	shared := fixture.Connection
	foreignScope := fixture.ForeignScope
	fixture.SetupSQL(t, foreignScope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO pm.connections (
				org_id, workspace_id, connection_id, connector_id, provider_account_id,
				revision, credential_ref, display_name, state
			) VALUES ($1, $2, $3, $4, $5, 1, $6, 'same-identity foreign connection', 'active')
		`, foreignScope.OrgID, foreignScope.WorkspaceID, shared.ConnectionID, shared.ConnectorID,
			shared.ProviderAccountID, "credential-shared-identity"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE pm.memberships
			SET resource_grants = ARRAY(
					SELECT DISTINCT value FROM unnest(resource_grants || $4::text[]) AS value ORDER BY value
				),
				connection_ids = ARRAY(
					SELECT DISTINCT value FROM unnest(connection_ids || $5::text[]) AS value ORDER BY value
				),
				policy_epoch = policy_epoch + 1,
				control_revision = control_revision + 1
			WHERE org_id = $1 AND workspace_id = $2 AND identity_id = $3
		`, foreignScope.OrgID, foreignScope.WorkspaceID, fixture.ForeignActor.RequestedActor.PrincipalID,
			[]string{"connection:" + shared.ConnectionID}, []string{shared.ConnectionID})
		return err
	})
	primary := fixture.Client
	foreign := fixture.ClientFor(t, fixture.ForeignActor)
	primarySnapshot := harness.metadata.snapshot.Fingerprint
	// The foreign workspace's own state directory exists but has promoted
	// nothing yet.
	harness.connectionSchemaStore(t, foreignScope)

	t.Run("a_workspace_without_its_own_discovery_reads_no_other_workspace_snapshot", func(t *testing.T) {
		inspection := decodeWarehouseRoute[api.ConnectionSchemaInspection](t,
			harness.send(t, foreign, http.MethodGet, schemaSelectorPath(harness, foreignScope.WorkspaceID, nil), nil),
			http.StatusOK)
		if selected, err := inspection.Selected.Get(); err == nil && selected != "" {
			t.Fatalf("workspace %s read the snapshot %s selected by workspace %s through the shared connection identity",
				foreignScope.WorkspaceID, selected, fixture.Scope.WorkspaceID)
		}
		if inspection.WorkspaceId != foreignScope.WorkspaceID || len(inspection.Fields) != 0 ||
			inspection.Snapshot.ContentFingerprint != "" || inspection.Snapshot.Complete {
			t.Fatalf("workspace %s schema inspection = %#v; want its own absent selection", foreignScope.WorkspaceID, inspection)
		}
		named := url.Values{"snapshot_id": {string(primarySnapshot)}}
		response := harness.send(t, foreign, http.MethodGet, schemaSelectorPath(harness, foreignScope.WorkspaceID, named), nil)
		requireConcealed(t, response, "another workspace's snapshot named by its identity")
		if bytes.Contains(response.document, []byte(primarySnapshot)) {
			t.Fatalf("the concealed cross-workspace answer disclosed %s: %s", primarySnapshot, response.document)
		}
	})

	t.Run("each_workspace_reads_only_its_own_snapshot", func(t *testing.T) {
		foreignMetadata := harness.promoteWorkspaceSchema(t, foreignScope, []schema.Field{{
			Name:     "foreign_only",
			Pointer:  "/properties/foreign_only",
			Schema:   contract.Schema{Type: contract.SchemaTypeString},
			Required: true,
		}})
		foreignSnapshot := foreignMetadata.snapshot.Fingerprint
		if foreignSnapshot == primarySnapshot {
			t.Fatalf("two workspaces' snapshots share the identity %s", foreignSnapshot)
		}
		for _, read := range []struct {
			name      string
			client    *http.Client
			workspace string
			want      contract.SemanticDigest
			fields    int
		}{
			{"primary", primary, fixture.Scope.WorkspaceID, primarySnapshot, len(harness.metadata.snapshot.Fields)},
			{"foreign", foreign, foreignScope.WorkspaceID, foreignSnapshot, len(foreignMetadata.snapshot.Fields)},
		} {
			inspection := decodeWarehouseRoute[api.ConnectionSchemaInspection](t,
				harness.send(t, read.client, http.MethodGet, schemaSelectorPath(harness, read.workspace, nil), nil),
				http.StatusOK)
			selected, err := inspection.Selected.Get()
			if err != nil || selected != string(read.want) || inspection.Snapshot.ContentFingerprint != string(read.want) ||
				inspection.WorkspaceId != read.workspace || len(inspection.Fields) != read.fields {
				t.Fatalf("the %s workspace schema read = %#v (%v); want only its own %s over %d fields",
					read.name, inspection, err, read.want, read.fields)
			}
		}
		for _, cross := range []struct {
			name      string
			client    *http.Client
			workspace string
			named     contract.SemanticDigest
		}{
			{"the primary workspace naming the foreign snapshot", primary, fixture.Scope.WorkspaceID, foreignSnapshot},
			{"the foreign workspace naming the primary snapshot", foreign, foreignScope.WorkspaceID, primarySnapshot},
		} {
			named := url.Values{"snapshot_id": {string(cross.named)}}
			response := harness.send(t, cross.client, http.MethodGet, schemaSelectorPath(harness, cross.workspace, named), nil)
			requireConcealed(t, response, cross.name)
		}
		// A workspace cannot read another workspace's connection state at all:
		// the primary actor has no authority in the foreign workspace.
		requireConcealedOrForbidden(t, harness.send(t, primary, http.MethodGet,
			schemaSelectorPath(harness, foreignScope.WorkspaceID, nil), nil))
	})
}

// requireConcealedOrForbidden requires a refusal of a read outside the actor's
// workspace authority, without asserting which refusal the identity boundary
// selects.
func requireConcealedOrForbidden(t *testing.T, response warehouseRouteResponse) {
	t.Helper()
	if response.status != http.StatusNotFound && response.status != http.StatusForbidden {
		t.Fatalf("a read outside the actor's workspace answered HTTP %d: %s", response.status, response.document)
	}
}

// unknownSchemaSnapshot declares one well-formed snapshot identity that no
// metadata store of this fixture ever wrote, so the selector names a snapshot
// that cannot exist rather than a malformed one.
func unknownSchemaSnapshot(t *testing.T) contract.SemanticDigest {
	t.Helper()
	digest := sha256.Sum256([]byte("an unknown schema snapshot identity"))
	reference := contract.SemanticDigest("sha256:" + hex.EncodeToString(digest[:]))
	if err := reference.Validate(); err != nil {
		t.Fatalf("declare an unknown snapshot identity: %v", err)
	}
	return reference
}

// promotedSchemaPayload resolves the one stored payload the fixture promoted
// without restating the store's own layout: every regular file beneath the
// installed metadata root is decoded as a snapshot, and the single file that
// decodes with the promoted content fingerprint is the payload the store reads.
func promotedSchemaPayload(t *testing.T, harness *etlHarness) string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(harness.metadataRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		document, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read the stored connection metadata %s: %v", path, readErr)
		}
		snapshot, decodeErr := schema.DecodeSnapshot(document)
		if decodeErr != nil || snapshot.Fingerprint != harness.metadata.snapshot.Fingerprint {
			return nil
		}
		found = append(found, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk the installed connection metadata state: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("the promoted snapshot payload = %v; want exactly one stored payload for %s",
			found, harness.metadata.snapshot.Fingerprint)
	}
	return found[0]
}

// retainReadSnapshot pins the registered read view of the acknowledged dataset
// under the identity the selector cases name.
func retainReadSnapshot(t *testing.T, harness *etlHarness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), etlHTTPTimeout)
	defer cancel()
	if err := store.WithinScope(ctx, harness.fixture.Pool, harness.fixture.Scope, harness.fixture.Actor,
		func(ctx context.Context, tx *store.Transaction) error {
			snapshot, err := store.CreateWarehouseSnapshot(ctx, tx, store.WarehouseSnapshot{
				Scope:            harness.fixture.Scope,
				DatasetID:        harness.layout.DatasetID(),
				SnapshotID:       readRefusalSnapshot,
				ManifestRevision: 1,
			})
			if err != nil {
				return err
			}
			if len(snapshot.Segments) != 1 {
				return fmt.Errorf("the retained snapshot pinned %d segments; want the one registered page",
					len(snapshot.Segments))
			}
			return nil
		}); err != nil {
		t.Fatalf("retain the dataset read view: %v", err)
	}
}

// registerReadDataset registers one more dataset identity in the fixture's own
// scope, so a selector can name a dataset that exists but is not this one.
func registerReadDataset(t *testing.T, harness *etlHarness, dataset string) warehouse.WarehouseLayout {
	t.Helper()
	layout, err := warehouse.NewWarehouseLayout(
		harness.fixture.Connection.ConnectorID, harness.fixture.Connection.ConnectionID, dataset)
	if err != nil {
		t.Fatalf("compose the %s dataset namespace: %v", dataset, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), etlHTTPTimeout)
	defer cancel()
	if err := store.WithinScope(ctx, harness.fixture.Pool, harness.fixture.Scope, harness.fixture.Actor,
		func(ctx context.Context, tx *store.Transaction) error {
			_, err := store.RegisterWarehouseDataset(ctx, tx, store.WarehouseDataset{
				Scope:        harness.fixture.Scope,
				DatasetID:    layout.DatasetID(),
				ConnectorID:  layout.ConnectorID,
				ConnectionID: layout.ConnectionID,
			})
			return err
		}); err != nil {
		t.Fatalf("register the %s dataset identity: %v", dataset, err)
	}
	return layout
}

// concealedQuery sends one otherwise valid typed query under the same refused
// selector and requires the same concealed answer.
func concealedQuery(t *testing.T, harness *etlHarness, client *http.Client, dataset, snapshotID string) {
	t.Helper()
	document := harness.queryDocument(etlDatasetName, 3)
	document.SnapshotId = nullable.NewNullableWithValue(snapshotID)
	requireConcealed(t, harness.send(t, client, http.MethodPost,
		readRefusalQueryPath(harness, harness.fixture.Scope.WorkspaceID, dataset), document),
		"the typed query under the refused selector "+snapshotID)
}

// schemaSelectorPath composes the schema route for the promoted binding with
// caller-declared overrides. Every binding member is always present, so a
// refusal is caused by the overridden selector alone.
func schemaSelectorPath(harness *etlHarness, workspaceID string, overrides url.Values) string {
	binding := harness.metadata.binding
	values := url.Values{
		"resource":            {binding.Resource},
		"permission_revision": {binding.PermissionRevision},
		"source_revision":     {binding.SourceRevision},
	}
	for name, entries := range overrides {
		values[name] = entries
	}
	return fmt.Sprintf("%s/v1/workspaces/%s/connections/%s/schema?%s",
		harness.fixture.BaseURL, url.PathEscape(workspaceID),
		url.PathEscape(harness.fixture.Connection.ConnectionID), values.Encode())
}

// readRefusalDatasetPath composes the read route of one dataset of this scope.
func readRefusalDatasetPath(harness *etlHarness, workspaceID, dataset string) string {
	return fmt.Sprintf("%s/v1/workspaces/%s/connections/%s/warehouse/datasets/%s",
		harness.fixture.BaseURL, url.PathEscape(workspaceID),
		url.PathEscape(harness.fixture.Connection.ConnectionID), url.PathEscape(dataset))
}

// readRefusalQueryPath composes the typed query route of one dataset of this
// scope.
func readRefusalQueryPath(harness *etlHarness, workspaceID, dataset string) string {
	return readRefusalDatasetPath(harness, workspaceID, dataset) + "/query"
}
