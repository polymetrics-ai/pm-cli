package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const (
	selectionTestResource  = "customer.records"
	selectionTestConnector = "postgres"
	selectionTestPermRev   = "permissions-v1"
	selectionTestSourceRev = "source-v1"
	// selectionLockProbe bounds one probe statement that must wait on the
	// lock of an uncommitted selection. The selection stays uncommitted until
	// the probe returns, so the probe can never take the lock.
	selectionLockProbe = "100ms"
)

// schemaSelectionFixture is one isolated scope with one connection, one
// admitted discovery run and stored candidates of one resource.
type schemaSelectionFixture struct {
	credentials  testStoreCredentials
	pool         *Pool
	scope        contract.Scope
	actor        contract.Actor
	connectionID string
	accountID    string
	adminDSN     string
}

func newSchemaSelectionFixture(t *testing.T) *schemaSelectionFixture {
	t.Helper()
	credentials, pool := prepareTestStore(t, 4)
	scope := newTestScope(t)
	fixture := &schemaSelectionFixture{
		credentials:  credentials,
		pool:         pool,
		scope:        scope,
		actor:        testActor(scope),
		connectionID: "connection-" + randomHex(t, 6),
		accountID:    "account-" + randomHex(t, 6),
		adminDSN:     stackAdminDSN(t, credentials),
	}
	t.Cleanup(func() {
		fixture.removeAuditRecords(t)
		cleanupTestScope(t, credentials, scope)
	})
	fixture.asMigrator(t,
		`INSERT INTO pm.workspace_namespaces (org_id, workspace_id, name) VALUES ($1, $2, 'schema selection')`,
		scope.OrgID, scope.WorkspaceID)
	fixture.asMigrator(t, `
		INSERT INTO pm.provider_accounts (org_id, provider_account_id, provider, account_witness, capacity, revision)
		VALUES ($1, $2, $3, $4, 1, 1)`, scope.OrgID, fixture.accountID, selectionTestConnector, "witness-"+fixture.accountID)
	fixture.asMigrator(t, `
		INSERT INTO pm.connections (org_id, workspace_id, connection_id, connector_id, provider_account_id, revision, credential_ref)
		VALUES ($1, $2, $3, $4, $5, 1, '')`, scope.OrgID, scope.WorkspaceID, fixture.connectionID, selectionTestConnector, fixture.accountID)
	return fixture
}

// stackAdminDSN connects as the stack administrator, which bypasses
// row-level security. The checks read and remove workspace audit records with
// it, as the acceptance observer does.
func stackAdminDSN(t *testing.T, credentials testStoreCredentials) string {
	t.Helper()
	password, err := os.ReadFile(filepath.Join(os.Getenv("PM_STORE_TEST_SECRETS_DIR"), "superuser-password"))
	if err != nil || strings.TrimSpace(string(password)) == "" {
		t.Fatal("read the stack administrator credential")
	}
	defer clear(password)
	runtime, err := url.Parse(credentials.runtimeDSN)
	if err != nil {
		t.Fatal("parse the runtime endpoint")
	}
	admin := *runtime
	admin.User = url.UserPassword("pm_stack_admin", strings.TrimSpace(string(password)))
	return admin.String()
}

func (fixture *schemaSelectionFixture) asMigrator(t *testing.T, query string, arguments ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, fixture.credentials.migratorDSN)
	if err != nil {
		t.Fatalf("connect the migrator: %T", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the migrator fixture: %T", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('pm.org_id', $1, true), set_config('pm.workspace_id', $2, true)`,
		fixture.scope.OrgID, fixture.scope.WorkspaceID); err != nil {
		t.Fatalf("bind the migrator fixture scope: %T", err)
	}
	if _, err := tx.Exec(ctx, query, arguments...); err != nil {
		t.Fatalf("migrator fixture statement: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the migrator fixture: %T", err)
	}
}

func (fixture *schemaSelectionFixture) removeAuditRecords(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, fixture.adminDSN)
	if err != nil {
		t.Errorf("connect the stack administrator for cleanup: %T", err)
		return
	}
	defer func() { _ = connection.Close(context.Background()) }()
	if _, err := connection.Exec(ctx, `DELETE FROM pm.workspace_audit_entries WHERE org_id = $1`, fixture.scope.OrgID); err != nil {
		t.Errorf("remove the workspace audit records: %v", err)
	}
}

// auditActions returns the action of each workspace audit record of the scope.
func (fixture *schemaSelectionFixture) auditActions(t *testing.T) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, fixture.adminDSN)
	if err != nil {
		t.Fatalf("connect the stack administrator: %T", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	rows, err := connection.Query(ctx, `
		SELECT action FROM pm.workspace_audit_entries
		WHERE org_id = $1 AND workspace_id = $2 ORDER BY recorded_at, audit_id
	`, fixture.scope.OrgID, fixture.scope.WorkspaceID)
	if err != nil {
		t.Fatalf("read the workspace audit records: %v", err)
	}
	actions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect the workspace audit records: %v", err)
	}
	return actions
}

// storeCandidates admits one discovery run and stores one complete candidate
// for each seed. The documents carry only the members that the table checks
// read: the store never decodes them.
func (fixture *schemaSelectionFixture) storeCandidates(t *testing.T, seeds ...string) []string {
	t.Helper()
	request := testAdmissionRequest(t, fixture.scope, "schema-selection-"+randomHex(t, 4), "schema selection")
	admitted := admitForTest(t, fixture.pool, fixture.scope, request)
	discovery := SchemaDiscovery{
		RunID: admitted.RunID, ConnectionID: fixture.connectionID, ConnectorID: selectionTestConnector,
		Resource: selectionTestResource, RelationBinding: "postgres.reusable-relation",
		ProviderAccountID: fixture.accountID, PermissionRevision: selectionTestPermRev, SourceRevision: selectionTestSourceRev,
	}
	references := make([]string, 0, len(seeds))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := WithinScope(ctx, fixture.pool, fixture.scope, fixture.actor, func(ctx context.Context, tx *Transaction) error {
		if err := RecordSchemaDiscovery(ctx, tx, discovery); err != nil {
			return err
		}
		for _, seed := range seeds {
			reference := string(testDigest("schema selection candidate " + seed + fixture.connectionID))
			document, err := json.Marshal(map[string]any{
				"fingerprint":  reference,
				"completeness": map[string]any{"complete": true},
				"binding": map[string]any{
					"org_id": fixture.scope.OrgID, "workspace_id": fixture.scope.WorkspaceID,
					"connector_id": selectionTestConnector, "connection_id": fixture.connectionID,
					"provider_account_id": fixture.accountID, "permission_revision": selectionTestPermRev,
					"resource": selectionTestResource, "source_revision": selectionTestSourceRev,
				},
				"fields": []any{},
			})
			if err != nil {
				return err
			}
			if _, err := StoreSchemaCandidate(ctx, tx, SchemaCandidate{
				ConnectionID: fixture.connectionID, SchemaRef: reference, ConnectorID: selectionTestConnector,
				ProviderAccountID: fixture.accountID, PermissionRevision: selectionTestPermRev,
				Resource: selectionTestResource, SourceRevision: selectionTestSourceRev, FieldCount: 0,
				Document: document, DiscoveredByRunID: admitted.RunID,
			}); err != nil {
				return err
			}
			references = append(references, reference)
		}
		return nil
	}); err != nil {
		t.Fatalf("store the schema candidates: %v", err)
	}
	return references
}

func (fixture *schemaSelectionFixture) selectCandidate(t *testing.T, schemaRef string, expected uint64) (SchemaSelectionChange, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var change SchemaSelectionChange
	err := WithinScope(ctx, fixture.pool, fixture.scope, fixture.actor, func(ctx context.Context, tx *Transaction) error {
		var err error
		change, err = SelectSchemaCandidate(ctx, tx, fixture.selectionRequest(t, schemaRef, expected))
		return err
	})
	return change, err
}

func (fixture *schemaSelectionFixture) selectionRequest(t *testing.T, schemaRef string, expected uint64) SelectSchemaCandidateRequest {
	return SelectSchemaCandidateRequest{
		ConnectionID: fixture.connectionID, Resource: selectionTestResource, SchemaRef: schemaRef,
		ExpectedRevision: expected, AuditID: "audit-" + randomHex(t, 8),
	}
}

func (fixture *schemaSelectionFixture) currentSelection(t *testing.T) SchemaSelection {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var selection SchemaSelection
	if err := WithinScope(ctx, fixture.pool, fixture.scope, fixture.actor, func(ctx context.Context, tx *Transaction) error {
		var err error
		selection, err = LoadSchemaSelection(ctx, tx, fixture.connectionID, selectionTestResource)
		return err
	}); err != nil {
		t.Fatalf("load the schema selection: %v", err)
	}
	return selection
}

func requireSelection(t *testing.T, got SchemaSelection, schemaRef string, revision uint64) {
	t.Helper()
	if got.Revision != revision || got.SchemaRef != schemaRef || got.Present != (schemaRef != "") {
		t.Fatalf("selection = %#v; want %q at revision %d", got, schemaRef, revision)
	}
}

// Check: the selection revision is 1 with no selection, the first selection
// at revision 1 gives 2, a replacement at 2 gives 3, and each success writes
// one audit record. A stale expected revision returns ErrCASConflict and
// changes no row and no audit record. A candidate that the resource does not
// hold returns ErrNotFound. Defect it catches: a revision that counts from 0
// or does not add 1, a stale selection that lands, or a selection without its
// audit record.
func TestSchemaSelectionCompareAndSet(t *testing.T) {
	fixture := newSchemaSelectionFixture(t)
	candidates := fixture.storeCandidates(t, "a", "b")
	requireSelection(t, fixture.currentSelection(t), "", 1)

	change, err := fixture.selectCandidate(t, candidates[0], 1)
	if err != nil || change.Previous.Present || change.Current.Revision != 2 {
		t.Fatalf("first selection = %#v, %v; want revision 2 from no selection", change, err)
	}
	requireSelection(t, fixture.currentSelection(t), candidates[0], 2)

	if _, err := fixture.selectCandidate(t, candidates[1], 1); !errors.Is(err, ErrCASConflict) {
		t.Fatalf("stale selection error = %v; want ErrCASConflict", err)
	}
	requireSelection(t, fixture.currentSelection(t), candidates[0], 2)
	if actions := fixture.auditActions(t); len(actions) != 1 || actions[0] != SchemaSelectionAuditAction {
		t.Fatalf("audit actions after a stale selection = %v; want one %s", actions, SchemaSelectionAuditAction)
	}

	unknown := string(testDigest("a candidate that no discovery stored"))
	if _, err := fixture.selectCandidate(t, unknown, 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("selection of an unknown candidate error = %v; want ErrNotFound", err)
	}
	requireSelection(t, fixture.currentSelection(t), candidates[0], 2)

	change, err = fixture.selectCandidate(t, candidates[1], 2)
	if err != nil || !change.Previous.Present || change.Previous.SchemaRef != candidates[0] || change.Current.Revision != 3 {
		t.Fatalf("replacement = %#v, %v; want revision 3 after candidate A", change, err)
	}
	requireSelection(t, fixture.currentSelection(t), candidates[1], 3)
	if actions := fixture.auditActions(t); len(actions) != 2 {
		t.Fatalf("audit actions = %v; want one for each of the two selections", actions)
	}
}

// Check: two database sessions on the same connection and resource select
// against the same expected revision. Exactly one succeeds, for the first
// selection (the insert) and for a replacement (the update), in two orders:
//
//   - Sequential: session 1 commits, then session 2 runs. This order is
//     deterministic.
//   - Overlapping: session 1 holds its uncommitted selection. A probe in a
//     second session, with a short lock_timeout, must fail with 55P03. This
//     shows that a second session's insert or locking read waits on the lock
//     of session 1 while session 1 is uncommitted. Session 2 then runs the
//     real selection in a goroutine, and session 1 commits. The check does not
//     show whether session 2 reached the lock before the commit. It needs no
//     sleep, no poll and no retry, because exactly one success must hold in
//     both interleavings.
//
// Defect it catches: a lost update that replaces a newer selection, or two
// first selections that both succeed.
func TestSchemaSelectionSameExpectedRevisionSucceedsOnce(t *testing.T) {
	for _, path := range []struct {
		name     string
		expected uint64
	}{
		{"first selection", 1},
		{"replacement", 2},
	} {
		t.Run(path.name+" sequential", func(t *testing.T) {
			fixture := newSchemaSelectionFixture(t)
			candidates := fixture.prepareSelectionPath(t, path.expected)
			if _, err := fixture.selectCandidate(t, candidates[1], path.expected); err != nil {
				t.Fatalf("session 1 selection = %v; want success", err)
			}
			if _, err := fixture.selectCandidate(t, candidates[2], path.expected); !errors.Is(err, ErrCASConflict) {
				t.Fatalf("session 2 selection = %v; want ErrCASConflict", err)
			}
			requireSelection(t, fixture.currentSelection(t), candidates[1], path.expected+1)
		})
		t.Run(path.name+" overlapping", func(t *testing.T) {
			fixture := newSchemaSelectionFixture(t)
			candidates := fixture.prepareSelectionPath(t, path.expected)
			done := make(chan error, 1)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := WithinScope(ctx, fixture.pool, fixture.scope, fixture.actor, func(ctx context.Context, tx *Transaction) error {
				if _, err := SelectSchemaCandidate(ctx, tx, fixture.selectionRequest(t, candidates[1], path.expected)); err != nil {
					return err
				}
				fixture.requireLockWait(t, candidates[2], path.expected)
				request := fixture.selectionRequest(t, candidates[2], path.expected)
				go func() {
					_, err := fixture.selectCandidateFrom(request)
					done <- err
				}()
				return nil
			}); err != nil {
				t.Fatalf("session 1 selection = %v; want success", err)
			}
			if err := <-done; !errors.Is(err, ErrCASConflict) {
				t.Fatalf("session 2 selection = %v; want ErrCASConflict", err)
			}
			requireSelection(t, fixture.currentSelection(t), candidates[1], path.expected+1)
			if actions := fixture.auditActions(t); uint64(len(actions)) != path.expected {
				t.Fatalf("audit actions = %v; want one for each successful selection", actions)
			}
		})
	}
}

// prepareSelectionPath stores three candidates and, for a replacement, makes
// the first selection so that the current revision is the expected one.
func (fixture *schemaSelectionFixture) prepareSelectionPath(t *testing.T, expected uint64) []string {
	t.Helper()
	candidates := fixture.storeCandidates(t, "a", "b", "c")
	if expected == 2 {
		if _, err := fixture.selectCandidate(t, candidates[0], 1); err != nil {
			t.Fatalf("prepare the current selection: %v", err)
		}
	}
	return candidates
}

// selectCandidateFrom runs one selection in its own session. It reports no
// test failure itself, because it runs in a goroutine.
func (fixture *schemaSelectionFixture) selectCandidateFrom(request SelectSchemaCandidateRequest) (SchemaSelectionChange, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var change SchemaSelectionChange
	err := WithinScope(ctx, fixture.pool, fixture.scope, fixture.actor, func(ctx context.Context, tx *Transaction) error {
		var err error
		change, err = SelectSchemaCandidate(ctx, tx, request)
		return err
	})
	return change, err
}

// requireLockWait runs, in a second session of the runtime role, the
// statement that a concurrent selection with the same expected revision runs
// first on the locked state: the insert of a first selection, or the locking
// read of a replacement. With a short lock_timeout it must fail with 55P03.
func (fixture *schemaSelectionFixture) requireLockWait(t *testing.T, schemaRef string, expected uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, fixture.credentials.runtimeDSN)
	if err != nil {
		t.Fatalf("connect the runtime probe: %T", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the runtime probe: %T", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('pm.org_id', $1, true), set_config('pm.workspace_id', $2, true),
			set_config('lock_timeout', $3, true)`,
		fixture.scope.OrgID, fixture.scope.WorkspaceID, selectionLockProbe); err != nil {
		t.Fatalf("bind the runtime probe: %T", err)
	}
	if expected == 1 {
		_, err = tx.Exec(ctx, `
			INSERT INTO pm.schema_selections (org_id, workspace_id, connection_id, resource, schema_ref, selection_revision)
			VALUES ($1, $2, $3, $4, $5, 2) ON CONFLICT DO NOTHING
		`, fixture.scope.OrgID, fixture.scope.WorkspaceID, fixture.connectionID, selectionTestResource, schemaRef)
	} else {
		_, err = tx.Exec(ctx, `
			SELECT schema_ref FROM pm.schema_selections WHERE connection_id = $1 AND resource = $2 FOR UPDATE
		`, fixture.connectionID, selectionTestResource)
	}
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) || databaseError.Code != "55P03" {
		t.Fatalf("second session statement during the uncommitted selection = %v; want lock_not_available 55P03", err)
	}
}

// Check: after migration 0024 the runtime role reads, inserts and changes a
// selection only through the reference, the revision and the time columns. It
// cannot remove a selection or change its key. The down step refuses while a
// selection exists, and on an empty table it removes the table and restores
// pm.control/v23. Defect it catches: a runtime role that can delete or rekey a
// selection, or a downgrade that drops a selection.
func TestSchemaSelectionMigrationGrantsAndDownStep(t *testing.T) {
	fixture := newSchemaSelectionFixture(t)
	candidates := fixture.storeCandidates(t, "a")
	if _, err := fixture.selectCandidate(t, candidates[0], 1); err != nil {
		t.Fatalf("select the candidate: %v", err)
	}
	probe := membershipUpdateProbe{t: t, runtimeDSN: fixture.credentials.runtimeDSN, scope: fixture.scope, actorID: "ada"}
	for name, query := range map[string]string{
		"delete":            `DELETE FROM pm.schema_selections WHERE connection_id = $1`,
		"connection change": `UPDATE pm.schema_selections SET connection_id = 'connection-other' WHERE connection_id = $1`,
		"resource change":   `UPDATE pm.schema_selections SET resource = 'customer.other' WHERE connection_id = $1`,
	} {
		_, err := probe.exec(query, fixture.connectionID)
		requireInsufficientPrivilege(t, name, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		if err := Migrate(cleanupCtx, fixture.credentials.migratorDSN); err != nil {
			t.Errorf("restore the current store schema: %v", err)
		}
	})
	err := migrateOneStoreVersionDown(ctx, fixture.credentials.migratorDSN, 24)
	if err == nil || !strings.Contains(err.Error(), "cannot roll back retained schema selections") {
		t.Fatalf("down step with a retained selection = %v; want the retained-selection refusal", err)
	}
	requireSelection(t, fixture.currentSelection(t), candidates[0], 2)

	connection, err := pgx.Connect(ctx, fixture.adminDSN)
	if err != nil {
		t.Fatalf("connect the stack administrator: %T", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	if _, err := connection.Exec(ctx, `DELETE FROM pm.schema_selections WHERE org_id = $1`, fixture.scope.OrgID); err != nil {
		t.Fatalf("remove the retained selection: %v", err)
	}
	if err := migrateOneStoreVersionDown(ctx, fixture.credentials.migratorDSN, 24); err != nil {
		t.Fatalf("down step on an empty selection table: %v", err)
	}
	var revision string
	var table *string
	if err := connection.QueryRow(ctx, `
		SELECT (SELECT revision FROM pm.control_schema WHERE singleton), to_regclass('pm.schema_selections')::text
	`).Scan(&revision, &table); err != nil {
		t.Fatalf("read the schema after the down step: %v", err)
	}
	if revision != "pm.control/v23" || table != nil {
		t.Fatalf("after the down step: revision %q, table %v; want pm.control/v23 and no table", revision, table)
	}
	if err := Migrate(ctx, fixture.credentials.migratorDSN); err != nil {
		t.Fatalf("apply migration 0024 again: %v", err)
	}
	requireSelection(t, fixture.currentSelection(t), "", 1)
}
