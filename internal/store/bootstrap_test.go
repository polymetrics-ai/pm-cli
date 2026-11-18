package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// bootstrapWaitLimit is the one time limit of each wait in these checks.
const bootstrapWaitLimit = 30 * time.Second

// bootstrapTables are the tables of the bootstrap rows, in the order that a
// cleanup deletes them.
var bootstrapTables = []string{
	"workspace_audit_entries", "bootstrap_records", "provider_accounts",
	"memberships", "auth_bootstrap_identities", "identities", "workspace_namespaces",
}

type bootstrapTestStore struct {
	credentials testStoreCredentials
	pool        *Pool
	bootstrap   string
	admin       string
}

// prepareBootstrapTestStore migrates the store and opens the bootstrap pool.
// The checks read and remove rows as the stack administrator, which bypasses
// row-level security, as the acceptance observer does.
func prepareBootstrapTestStore(t *testing.T) bootstrapTestStore {
	t.Helper()
	credentials, _ := prepareTestStore(t, 2)
	directory := os.Getenv("PM_STORE_TEST_SECRETS_DIR")
	read := func(member string) string {
		contents, err := os.ReadFile(filepath.Join(directory, member))
		if err != nil || strings.TrimSpace(string(contents)) == "" {
			t.Fatalf("read %s", member)
		}
		return strings.TrimSpace(string(contents))
	}
	runtime, err := url.Parse(credentials.runtimeDSN)
	if err != nil {
		t.Fatal("parse the runtime endpoint")
	}
	admin := *runtime
	admin.User = url.UserPassword("pm_stack_admin", read("superuser-password"))
	result := bootstrapTestStore{credentials: credentials, bootstrap: read("pm-app-bootstrap-endpoint"), admin: admin.String()}
	ctx, cancel := context.WithTimeout(context.Background(), bootstrapWaitLimit)
	defer cancel()
	result.pool, err = OpenBootstrapPool(ctx, result.bootstrap)
	if err != nil {
		t.Fatalf("open the bootstrap pool: %v", err)
	}
	t.Cleanup(result.pool.Close)
	return result
}

func newBootstrapTestRequest(t *testing.T, deploymentID string, scope contract.Scope) BootstrapRequest {
	t.Helper()
	return BootstrapRequest{
		DeploymentID:    deploymentID,
		Scope:           scope,
		WorkspaceName:   "First workspace",
		AdminIdentityID: "identity-" + randomHex(t, 8),
		AdminIssuer:     "https://identity.test.invalid",
		AdminSubject:    "subject-" + randomHex(t, 8),
		MembershipID:    "membership-" + randomHex(t, 8),
		Role:            "admin",
		Permissions:     []string{"connection.manage", "workspace.access.manage", "workspace.read"},
		ProviderAccounts: []BootstrapProviderAccount{{
			AccountID: "provider-account-" + randomHex(t, 8), ConnectorID: "postgres",
			DisplayName: "Customer database", Capacity: 1,
		}},
		AuditID: "audit-" + randomHex(t, 8),
		Action:  "workspace.bootstrap",
	}
}

// bootstrapCleanupLimit bounds each cleanup step of a bootstrap snapshot.
const bootstrapCleanupLimit = 10 * time.Second

// endBootstrapSnapshot rolls the snapshot back and closes its connection. Each
// step has its own context with the limit, so a PostgreSQL that does not
// answer cannot hold the check after its own deadline. A nil tx skips the
// rollback.
func endBootstrapSnapshot(
	tx interface{ Rollback(context.Context) error },
	connection interface{ Close(context.Context) error },
	limit time.Duration,
) error {
	var rollbackErr error
	if tx != nil {
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		rollbackErr = tx.Rollback(ctx)
		cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	return errors.Join(rollbackErr, connection.Close(ctx))
}

// readBootstrapRows reads every bootstrap row of the deployment and the
// organizations in one snapshot, one JSON text for each row.
func (s bootstrapTestStore) readBootstrapRows(t *testing.T, deploymentID string, orgIDs ...string) map[string][]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), bootstrapWaitLimit)
	defer cancel()
	connection, err := pgx.Connect(ctx, s.admin)
	if err != nil {
		t.Fatalf("connect the stack administrator: %v", err)
	}
	tx, err := connection.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		if closeErr := endBootstrapSnapshot(nil, connection, bootstrapCleanupLimit); closeErr != nil {
			t.Errorf("close the stack administrator connection: %v", closeErr)
		}
		t.Fatalf("begin the bootstrap snapshot: %v", err)
	}
	defer func() {
		if err := endBootstrapSnapshot(tx, connection, bootstrapCleanupLimit); err != nil {
			t.Errorf("end the bootstrap snapshot: %v", err)
		}
	}()
	rows := make(map[string][]string, len(bootstrapTables))
	for _, table := range bootstrapTables {
		filter := "org_id = ANY($2) AND $1 <> ''"
		if table == "bootstrap_records" || table == "auth_bootstrap_identities" {
			filter = "deployment_id = $1 AND org_id = ANY($2)"
		}
		result, err := tx.Query(ctx, "SELECT to_jsonb(row)::text FROM pm."+table+" AS row WHERE "+filter+" ORDER BY 1", deploymentID, orgIDs)
		if err != nil {
			t.Fatalf("read pm.%s: %v", table, err)
		}
		values, err := pgx.CollectRows(result, pgx.RowTo[string])
		if err != nil {
			t.Fatalf("collect pm.%s: %v", table, err)
		}
		rows[table] = values
	}
	return rows
}

// removeBootstrapRows deletes the rows that a check wrote, so that a later
// downgrade of the shared test store finds no retained bootstrap.
func (s bootstrapTestStore) removeBootstrapRows(t *testing.T, deploymentID string, orgIDs ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), bootstrapWaitLimit)
	defer cancel()
	connection, err := pgx.Connect(ctx, s.admin)
	if err != nil {
		t.Errorf("connect the stack administrator for cleanup: %v", err)
		return
	}
	defer func() { _ = connection.Close(context.Background()) }()
	for _, table := range bootstrapTables {
		statement := "DELETE FROM pm." + table + " WHERE org_id = ANY($1)"
		arguments := []any{orgIDs}
		if table == "bootstrap_records" || table == "auth_bootstrap_identities" {
			statement = "DELETE FROM pm." + table + " WHERE deployment_id = $1 AND org_id = ANY($2)"
			arguments = []any{deploymentID, orgIDs}
		}
		if _, err := connection.Exec(ctx, statement, arguments...); err != nil {
			t.Errorf("remove pm.%s rows: %v", table, err)
			return
		}
	}
}

func rowCounts(rows map[string][]string) map[string]int {
	counts := make(map[string]int, len(rows))
	for table, values := range rows {
		counts[table] = len(values)
	}
	return counts
}

func everyBootstrapTable(count int) map[string]int {
	counts := make(map[string]int, len(bootstrapTables))
	for _, table := range bootstrapTables {
		counts[table] = count
	}
	return counts
}

// Check: one bootstrap for each deployment and organization; a second
// organization with a bound admin login is refused; a second organization
// with its own admin succeeds and leaves the first unchanged.
func TestBootstrapWritesTheFirstWorkspaceOnceForEachScope(t *testing.T) {
	s := prepareBootstrapTestStore(t)
	deploymentID := "deployment-" + randomHex(t, 8)
	first, second := newTestScope(t), newTestScope(t)
	t.Cleanup(func() { s.removeBootstrapRows(t, deploymentID, first.OrgID, second.OrgID) })
	ctx, cancel := context.WithTimeout(context.Background(), bootstrapWaitLimit)
	defer cancel()

	request := newBootstrapTestRequest(t, deploymentID, first)
	if err := Bootstrap(ctx, s.pool, request); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	rows := s.readBootstrapRows(t, deploymentID, first.OrgID)
	if got := rowCounts(rows); !reflect.DeepEqual(got, everyBootstrapTable(1)) {
		t.Fatalf("first bootstrap rows = %v; want one in each table", got)
	}
	for table, want := range map[string]string{
		"auth_bootstrap_identities": `"can_approve_devices": true`,
		"memberships":               `"resource_grants": ["permission:connection.manage", "permission:workspace.access.manage", "permission:workspace.read"]`,
		"provider_accounts":         `"display_name": "Customer database"`,
		"workspace_audit_entries":   `"requested_actor_kind": "operator"`,
	} {
		if !strings.Contains(rows[table][0], want) {
			t.Fatalf("pm.%s row %s does not hold %s", table, rows[table][0], want)
		}
	}
	for _, want := range []string{`"connection_ids": []`, `"operation_ids": []`, `"role": "admin"`} {
		if !strings.Contains(rows["memberships"][0], want) {
			t.Fatalf("admin membership %s does not hold %s", rows["memberships"][0], want)
		}
	}

	if err := Bootstrap(ctx, s.pool, request); !errors.Is(err, ErrBootstrapScopeExists) {
		t.Fatalf("second bootstrap of the same scope = %v; want %v", err, ErrBootstrapScopeExists)
	}
	sameLogin := newBootstrapTestRequest(t, deploymentID, second)
	sameLogin.AdminIssuer, sameLogin.AdminSubject = request.AdminIssuer, request.AdminSubject
	if err := Bootstrap(ctx, s.pool, sameLogin); !errors.Is(err, ErrBootstrapLoginBound) {
		t.Fatalf("bootstrap of a second organization with a bound login = %v; want %v", err, ErrBootstrapLoginBound)
	}
	if got := s.readBootstrapRows(t, deploymentID, first.OrgID, second.OrgID); !reflect.DeepEqual(got, rows) {
		t.Fatalf("refused bootstraps changed the rows:\n got %v\nwant %v", got, rows)
	}

	if err := Bootstrap(ctx, s.pool, newBootstrapTestRequest(t, deploymentID, second)); err != nil {
		t.Fatalf("bootstrap of a second organization: %v", err)
	}
	if got := s.readBootstrapRows(t, deploymentID, first.OrgID); !reflect.DeepEqual(got, rows) {
		t.Fatalf("second organization changed the first scope:\n got %v\nwant %v", got, rows)
	}
	if got := rowCounts(s.readBootstrapRows(t, deploymentID, second.OrgID)); !reflect.DeepEqual(got, everyBootstrapTable(1)) {
		t.Fatalf("second organization rows = %v; want one in each table", got)
	}
}

// Check: concurrent bootstraps of one scope. Exactly one commits. Defect: a
// race makes two first workspaces or two admins. The first bootstrap holds
// its open transaction before its function call while the second commits.
// pm.operator_bootstrap of the first then finds the committed scope.
func TestConcurrentBootstrapsOfOneScopeCommitOnce(t *testing.T) {
	s := prepareBootstrapTestStore(t)
	deploymentID := "deployment-" + randomHex(t, 8)
	scope := newTestScope(t)
	t.Cleanup(func() { s.removeBootstrapRows(t, deploymentID, scope.OrgID) })
	ctx, cancel := context.WithTimeout(context.Background(), 2*bootstrapWaitLimit)
	defer cancel()

	first := newBootstrapTestRequest(t, deploymentID, scope)
	second := newBootstrapTestRequest(t, deploymentID, contract.Scope{OrgID: scope.OrgID, WorkspaceID: "workspace-" + randomHex(t, 8)})
	checked := make(chan struct{})
	secondDone := make(chan struct{})
	first.step = func(ctx context.Context, step bootstrapStep) error {
		if step != bootstrapStepChecked {
			return nil
		}
		close(checked)
		timer := time.NewTimer(bootstrapWaitLimit)
		defer timer.Stop()
		select {
		case <-secondDone:
			return nil
		case <-timer.C:
			return errors.New("the second bootstrap did not end within the limit")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	firstResult := make(chan error, 1)
	go func() { firstResult <- Bootstrap(ctx, s.pool, first) }()
	timer := time.NewTimer(bootstrapWaitLimit)
	defer timer.Stop()
	select {
	case <-checked:
	case err := <-firstResult:
		t.Fatalf("first bootstrap ended before its scope check passed: %v", err)
	case <-timer.C:
		t.Fatal("the first bootstrap did not pass its scope check within the limit")
	}
	secondErr := Bootstrap(ctx, s.pool, second)
	close(secondDone)
	firstErr := <-firstResult

	if secondErr != nil {
		t.Fatalf("second bootstrap = %v; want success", secondErr)
	}
	if !errors.Is(firstErr, ErrBootstrapScopeExists) {
		t.Fatalf("first bootstrap = %v; want %v", firstErr, ErrBootstrapScopeExists)
	}
	rows := s.readBootstrapRows(t, deploymentID, scope.OrgID)
	if got := rowCounts(rows); !reflect.DeepEqual(got, everyBootstrapTable(1)) {
		t.Fatalf("rows after two concurrent bootstraps = %v; want one in each table", got)
	}
	if !strings.Contains(rows["workspace_namespaces"][0], second.Scope.WorkspaceID) {
		t.Fatalf("committed workspace %s is not the workspace of the committed bootstrap", rows["workspace_namespaces"][0])
	}
}

// Check: two bootstraps of one organization in two deployments, run at the
// same time, commit once (crrao m1). Defect: each read of the organization
// misses the uncommitted rows of the other, nothing else conflicts, and both
// commit. The first bootstrap writes its rows and holds its transaction open.
// The second holds the session advisory lock "arrival" and then calls
// pm.operator_bootstrap. A trigger of this check releases "arrival" when the
// second inserts its bootstrap record, after its reads. The check waits once
// for "arrival", then lets the first commit. Each wait has one time limit. No
// sleep, no poll, no retry.
func TestConcurrentBootstrapsOfOneOrganizationInTwoDeploymentsCommitOnce(t *testing.T) {
	s := prepareBootstrapTestStore(t)
	firstDeployment := "deployment-" + randomHex(t, 8)
	secondDeployment := "deployment-" + randomHex(t, 8)
	scope := newTestScope(t)
	t.Cleanup(func() {
		// Without the fix both bootstraps commit. Their records and login
		// bindings reference the rows of the organization, so they go first.
		cleanup, cancel := context.WithTimeout(context.Background(), bootstrapWaitLimit)
		defer cancel()
		if connection, err := pgx.Connect(cleanup, s.admin); err != nil {
			t.Errorf("connect the stack administrator for cleanup: %v", err)
		} else {
			for _, table := range []string{"workspace_audit_entries", "bootstrap_records", "auth_bootstrap_identities"} {
				if _, err := connection.Exec(cleanup, "DELETE FROM pm."+table+" WHERE org_id = $1", scope.OrgID); err != nil {
					t.Errorf("remove pm.%s rows: %v", table, err)
				}
			}
			_ = connection.Close(context.Background())
		}
		s.removeBootstrapRows(t, firstDeployment, scope.OrgID)
		s.removeBootstrapRows(t, secondDeployment, scope.OrgID)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*bootstrapWaitLimit)
	defer cancel()

	admin, err := pgx.Connect(ctx, s.admin)
	if err != nil {
		t.Fatalf("connect the stack administrator: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	arrival := rand.Int64()
	name := "test_bootstrap_arrival_" + randomHex(t, 8)
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION pm.%s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.deployment_id = %s THEN
        PERFORM pg_advisory_unlock(%d);
    END IF;
    RETURN NEW;
END
$$`, name, quoteLiteral(secondDeployment), arrival)); err != nil {
		t.Fatalf("create the arrival trigger function: %v", err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), bootstrapWaitLimit)
		defer cancel()
		connection, err := pgx.Connect(cleanup, s.admin)
		if err != nil {
			t.Errorf("connect the stack administrator for the trigger cleanup: %v", err)
			return
		}
		defer func() { _ = connection.Close(context.Background()) }()
		if _, err := connection.Exec(cleanup, fmt.Sprintf("DROP TRIGGER IF EXISTS %s ON pm.bootstrap_records", name)); err != nil {
			t.Errorf("drop the arrival trigger: %v", err)
		}
		if _, err := connection.Exec(cleanup, fmt.Sprintf("DROP FUNCTION IF EXISTS pm.%s()", name)); err != nil {
			t.Errorf("drop the arrival trigger function: %v", err)
		}
	})
	if _, err := admin.Exec(ctx, fmt.Sprintf(
		"CREATE TRIGGER %s BEFORE INSERT ON pm.bootstrap_records FOR EACH ROW EXECUTE FUNCTION pm.%s()", name, name)); err != nil {
		t.Fatalf("create the arrival trigger: %v", err)
	}

	first := newBootstrapTestRequest(t, firstDeployment, scope)
	written := make(chan struct{})
	release := make(chan struct{})
	first.step = func(ctx context.Context, step bootstrapStep) error {
		if step != bootstrapStepWritten {
			return nil
		}
		close(written)
		timer := time.NewTimer(bootstrapWaitLimit)
		defer timer.Stop()
		select {
		case <-release:
			return nil
		case <-timer.C:
			return errors.New("the check did not release the first bootstrap within the limit")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	firstResult := make(chan error, 1)
	go func() { firstResult <- Bootstrap(ctx, s.pool, first) }()
	awaitBootstrapSignal(t, written, firstResult, "the first bootstrap did not write its rows")

	second := newBootstrapTestRequest(t, secondDeployment, contract.Scope{OrgID: scope.OrgID, WorkspaceID: "workspace-" + randomHex(t, 8)})
	armed := make(chan struct{})
	second.step = func(ctx context.Context, step bootstrapStep) error {
		if step != bootstrapStepChecked {
			return nil
		}
		tx, ok := ScopedTransaction(ctx)
		if !ok {
			return errors.New("the second bootstrap has no transaction")
		}
		if _, err := tx.exec(ctx, "SELECT pg_advisory_lock($1)", arrival); err != nil {
			return fmt.Errorf("take the arrival lock: %w", err)
		}
		close(armed)
		return nil
	}
	secondResult := make(chan error, 1)
	go func() { secondResult <- Bootstrap(ctx, s.pool, second) }()
	awaitBootstrapSignal(t, armed, secondResult, "the second bootstrap did not take the arrival lock")

	// The second bootstrap holds "arrival" until its trigger releases it, so
	// this lock returns only after the second has read the organization and
	// started the insert of its bootstrap record.
	arrived := make(chan error, 1)
	go func() {
		_, err := admin.Exec(ctx, "SELECT pg_advisory_lock($1)", arrival)
		arrived <- err
	}()
	timer := time.NewTimer(bootstrapWaitLimit)
	defer timer.Stop()
	select {
	case err := <-arrived:
		if err != nil {
			t.Fatalf("wait for the arrival of the second bootstrap: %v", err)
		}
	case err := <-secondResult:
		t.Fatalf("the second bootstrap ended before its insert: %v", err)
	case <-timer.C:
		t.Fatal("the second bootstrap did not reach its insert within the limit")
	}
	close(release)
	firstErr := <-firstResult
	secondErr := <-secondResult
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_unlock($1)", arrival); err != nil {
		t.Errorf("release the arrival lock: %v", err)
	}

	if firstErr != nil {
		t.Fatalf("first bootstrap = %v; want success", firstErr)
	}
	if !errors.Is(secondErr, ErrBootstrapScopeOtherInput) {
		t.Fatalf("second bootstrap of the organization in another deployment = %v; want %v", secondErr, ErrBootstrapScopeOtherInput)
	}
	var records, workspaces int
	if err := admin.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM pm.bootstrap_records WHERE org_id = $1),
		       (SELECT count(*) FROM pm.workspace_namespaces WHERE org_id = $1)
	`, scope.OrgID).Scan(&records, &workspaces); err != nil {
		t.Fatalf("count the bootstraps of the organization: %v", err)
	}
	if records != 1 || workspaces != 1 {
		t.Fatalf("the organization has %d bootstrap records and %d workspaces; want one of each", records, workspaces)
	}
}

// awaitBootstrapSignal waits once, with one limit, for signal. An early end
// of the bootstrap is a failure.
func awaitBootstrapSignal(t *testing.T, signal <-chan struct{}, result <-chan error, failure string) {
	t.Helper()
	timer := time.NewTimer(bootstrapWaitLimit)
	defer timer.Stop()
	select {
	case <-signal:
	case err := <-result:
		t.Fatalf("%s: the bootstrap ended first: %v", failure, err)
	case <-timer.C:
		t.Fatalf("%s within %s", failure, bootstrapWaitLimit)
	}
}

// quoteLiteral quotes text as an SQL string literal.
func quoteLiteral(text string) string {
	return "'" + strings.ReplaceAll(text, "'", "''") + "'"
}

// Check: a bootstrap that fails part of the way writes nothing, and a retry
// starts clean. Defect: a workspace remains without its admin membership, its
// login binding or its audit record.
func TestBootstrapThatFailsPartWayWritesNothing(t *testing.T) {
	s := prepareBootstrapTestStore(t)
	deploymentID := "deployment-" + randomHex(t, 8)
	scope := newTestScope(t)
	t.Cleanup(func() { s.removeBootstrapRows(t, deploymentID, scope.OrgID) })
	ctx, cancel := context.WithTimeout(context.Background(), bootstrapWaitLimit)
	defer cancel()

	request := newBootstrapTestRequest(t, deploymentID, scope)
	stopped := errors.New("stop after the last row")
	failing := request
	failing.step = func(_ context.Context, step bootstrapStep) error {
		if step == bootstrapStepWritten {
			return stopped
		}
		return nil
	}
	if err := Bootstrap(ctx, s.pool, failing); !errors.Is(err, stopped) {
		t.Fatalf("bootstrap that fails after its last row = %v; want %v", err, stopped)
	}
	if got := rowCounts(s.readBootstrapRows(t, deploymentID, scope.OrgID)); !reflect.DeepEqual(got, everyBootstrapTable(0)) {
		t.Fatalf("rows after a failed bootstrap = %v; want none", got)
	}
	if err := Bootstrap(ctx, s.pool, request); err != nil {
		t.Fatalf("retry after a failed bootstrap: %v", err)
	}
	if got := rowCounts(s.readBootstrapRows(t, deploymentID, scope.OrgID)); !reflect.DeepEqual(got, everyBootstrapTable(1)) {
		t.Fatalf("rows after the retry = %v; want one in each table", got)
	}
}

// Check: the bootstrap role can write the bootstrap rows only through
// pm.operator_bootstrap. It holds no table privilege. Defect: a role with more
// rights than the bootstrap needs.
func TestBootstrapRoleCanWriteOnlyTheBootstrapRows(t *testing.T) {
	s := prepareBootstrapTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), bootstrapWaitLimit)
	defer cancel()
	admin, err := pgx.Connect(ctx, s.admin)
	if err != nil {
		t.Fatalf("connect the stack administrator: %v", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()

	rows, err := admin.Query(ctx, `
		SELECT c.relname,
		       has_any_column_privilege('pm_app_bootstrap', c.oid, 'UPDATE')
		           OR has_table_privilege('pm_app_bootstrap', c.oid, 'DELETE')
		           OR has_table_privilege('pm_app_bootstrap', c.oid, 'TRUNCATE')
		           OR has_any_column_privilege('pm_app_bootstrap', c.oid, 'REFERENCES')
		           OR has_table_privilege('pm_app_bootstrap', c.oid, 'TRIGGER'),
		       has_any_column_privilege('pm_app_bootstrap', c.oid, 'INSERT'),
		       has_any_column_privilege('pm_app_bootstrap', c.oid, 'SELECT')
		FROM pg_class AS c
		JOIN pg_namespace AS n ON n.oid = c.relnamespace
		WHERE n.nspname = 'pm' AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
		ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("read the bootstrap role table privileges: %v", err)
	}
	var changes, inserts, selects []string
	for rows.Next() {
		var table string
		var change, insert, read bool
		if err := rows.Scan(&table, &change, &insert, &read); err != nil {
			t.Fatalf("scan the bootstrap role table privileges: %v", err)
		}
		if change {
			changes = append(changes, table)
		}
		if insert {
			inserts = append(inserts, table)
		}
		if read {
			selects = append(selects, table)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the bootstrap role table privileges: %v", err)
	}
	// The role writes only through pm.operator_bootstrap, so it holds no
	// privilege on any table or column of the schema (ADR 0006, F1 and N1).
	if len(changes) != 0 || len(inserts) != 0 || len(selects) != 0 {
		t.Fatalf("bootstrap role privileges: change %v, insert %v, select %v; want none",
			changes, inserts, selects)
	}
	var forbidden bool
	if err := admin.QueryRow(ctx, `
		SELECT r.rolsuper OR r.rolinherit OR r.rolcreaterole OR r.rolcreatedb OR r.rolbypassrls OR r.rolreplication
		       OR pg_has_role('pm_app_bootstrap', 'pm_app_migrator', 'MEMBER')
		       OR pg_has_role('pm_app_bootstrap', 'pm_app_runtime', 'MEMBER')
		       OR has_database_privilege('pm_app_bootstrap', 'pm_app', 'CREATE')
		       OR has_database_privilege('pm_app_bootstrap', 'pm_app', 'TEMP')
		       OR has_schema_privilege('pm_app_bootstrap', 'pm', 'CREATE')
		       OR EXISTS (
		           SELECT 1 FROM pg_class AS c JOIN pg_namespace AS n ON n.oid = c.relnamespace
		           WHERE n.nspname = 'pm'
		             AND CASE WHEN c.relkind = 'S' THEN has_sequence_privilege('pm_app_bootstrap', c.oid, 'USAGE, SELECT, UPDATE') ELSE false END)
		       OR EXISTS (
		           SELECT 1 FROM pg_proc AS p JOIN pg_namespace AS n ON n.oid = p.pronamespace
		           WHERE n.nspname = 'pm' AND p.prosecdef
		             AND p.oid <> 'pm.operator_bootstrap(text, text, text, text, text, text, text, text, text, text[], text[], text[], text[], bigint[], text, text)'::regprocedure
		             AND has_function_privilege('pm_app_bootstrap', p.oid, 'EXECUTE'))
		FROM pg_roles AS r WHERE r.rolname = 'pm_app_bootstrap'`).Scan(&forbidden); err != nil {
		t.Fatalf("read the bootstrap role authority: %v", err)
	}
	if forbidden {
		t.Fatal("bootstrap role has a role attribute, membership, database, schema, sequence or other security-definer function authority")
	}

	// With no table grant, each direct insert, change or removal is refused.
	deploymentID := "deployment-" + randomHex(t, 8)
	scope := newTestScope(t)
	other := "org-" + randomHex(t, 8)
	for name, statement := range map[string]struct {
		sql       string
		arguments []any
	}{
		"login binding of another organization": {`
			INSERT INTO pm.auth_bootstrap_identities (deployment_id, issuer, subject, org_id, principal_id, principal_kind, can_approve_devices, enabled)
			VALUES ($1, 'https://identity.test.invalid', 'subject', $2, 'identity', 'user', true, true)`, []any{deploymentID, other}},
		"bootstrap record of another deployment": {`
			INSERT INTO pm.bootstrap_records (deployment_id, org_id, workspace_id, admin_identity_id, membership_id)
			VALUES ($1, $2, $3, 'identity', 'membership')`, []any{"deployment-" + randomHex(t, 8), scope.OrgID, scope.WorkspaceID}},
		"workspace of another organization": {`
			INSERT INTO pm.workspace_namespaces (org_id, workspace_id, name) VALUES ($1, $2, 'Other')`, []any{other, scope.WorkspaceID}},
		"membership update":       {`UPDATE pm.memberships SET role = 'owner' WHERE org_id = $1`, []any{scope.OrgID}},
		"identity removal":        {`DELETE FROM pm.identities WHERE org_id = $1`, []any{scope.OrgID}},
		"bootstrap removal":       {`DELETE FROM pm.bootstrap_records WHERE org_id = $1`, []any{scope.OrgID}},
		"provider account update": {`UPDATE pm.provider_accounts SET capacity = 9 WHERE org_id = $1`, []any{scope.OrgID}},
	} {
		err := WithinTransaction(ctx, s.pool, func(ctx context.Context, tx *Transaction) error {
			if _, err := tx.exec(ctx, `
				SELECT set_config('pm.deployment_id', $1, true), set_config('pm.org_id', $2, true),
				       set_config('pm.workspace_id', $3, true)`, deploymentID, scope.OrgID, scope.WorkspaceID); err != nil {
				return err
			}
			_, err := tx.exec(ctx, statement.sql, statement.arguments...)
			return err
		})
		var databaseError *pgconn.PgError
		if !errors.As(err, &databaseError) || databaseError.Code != "42501" {
			t.Errorf("%s as the bootstrap role = %v; want refusal 42501", name, err)
		}
	}
}

// Check: the migration refuses an absent bootstrap role and a role with a
// prohibited authority, and changes nothing. Defect: a migration that grants
// insert rights to a missing role or to a role with more authority.
func TestBootstrapMigrationChecksTheBootstrapRole(t *testing.T) {
	s := prepareBootstrapTestStore(t)
	s.pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*bootstrapWaitLimit)
	defer cancel()
	admin, err := pgx.Connect(ctx, s.admin)
	if err != nil {
		t.Fatalf("connect the stack administrator: %v", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	if err := migrateOneStoreVersionDown(ctx, s.credentials.migratorDSN, ControlSchemaVersion); err != nil {
		t.Fatalf("return the store to the version before the bootstrap schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), bootstrapWaitLimit)
		defer cleanupCancel()
		if err := Migrate(cleanupCtx, s.credentials.migratorDSN); err != nil {
			t.Errorf("restore the current store schema: %v", err)
		}
	})
	revision := func() string {
		connection, err := pgx.Connect(ctx, s.credentials.migratorDSN)
		if err != nil {
			t.Fatalf("connect the migrator: %v", err)
		}
		defer func() { _ = connection.Close(context.Background()) }()
		var value string
		if err := connection.QueryRow(ctx, `SELECT revision FROM pm.control_schema WHERE singleton`).Scan(&value); err != nil {
			t.Fatalf("read the control schema revision: %v", err)
		}
		return value
	}
	before := revision()
	for _, change := range []struct {
		name, apply, undo, refusal string
	}{
		{"absent", `ALTER ROLE pm_app_bootstrap RENAME TO pm_app_bootstrap_absent`,
			`ALTER ROLE pm_app_bootstrap_absent RENAME TO pm_app_bootstrap`, "required bootstrap role is missing"},
		{"create-database authority", `ALTER ROLE pm_app_bootstrap CREATEDB`,
			`ALTER ROLE pm_app_bootstrap NOCREATEDB`, "bootstrap role has prohibited PostgreSQL authority"},
		{"row-level security bypass", `ALTER ROLE pm_app_bootstrap BYPASSRLS`,
			`ALTER ROLE pm_app_bootstrap NOBYPASSRLS`, "bootstrap role has prohibited PostgreSQL authority"},
		{"member of the runtime role", `GRANT pm_app_runtime TO pm_app_bootstrap`,
			`REVOKE pm_app_runtime FROM pm_app_bootstrap`, "bootstrap role has prohibited PostgreSQL authority"},
		{"runtime role is a member", `GRANT pm_app_bootstrap TO pm_app_runtime`,
			`REVOKE pm_app_bootstrap FROM pm_app_runtime`, "bootstrap role has prohibited PostgreSQL authority"},
	} {
		if _, err := admin.Exec(ctx, change.apply); err != nil {
			t.Fatalf("give the bootstrap role the case %q: %v", change.name, err)
		}
		migrateErr := Migrate(ctx, s.credentials.migratorDSN)
		if _, err := admin.Exec(ctx, change.undo); err != nil {
			t.Fatalf("undo the case %q: %v", change.name, err)
		}
		var databaseError *pgconn.PgError
		if !errors.As(migrateErr, &databaseError) || databaseError.Code != "P0001" || databaseError.Message != change.refusal {
			t.Fatalf("migration with the case %q = %v; want refusal %q", change.name, migrateErr, change.refusal)
		}
		if got := revision(); got != before {
			t.Fatalf("control schema revision after the refused case %q = %q; want %q", change.name, got, before)
		}
	}
	if err := Migrate(ctx, s.credentials.migratorDSN); err != nil {
		t.Fatalf("migration with the bootstrap role as created: %v", err)
	}
	if got := revision(); got != ControlSchemaRevision {
		t.Fatalf("control schema revision = %q; want %q", got, ControlSchemaRevision)
	}
}

// Check: the bootstrap pool refuses another role and a refused password as a
// missing operator authority. Defect: a bootstrap that runs with the runtime
// credential, or reports a refused credential as an unavailable store.
func TestOpenBootstrapPoolRefusesAnotherCredential(t *testing.T) {
	s := prepareBootstrapTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), bootstrapWaitLimit)
	defer cancel()
	wrongPassword, err := url.Parse(s.bootstrap)
	if err != nil {
		t.Fatal("parse the bootstrap endpoint")
	}
	wrongPassword.User = url.UserPassword(BootstrapRole, "refused-"+randomHex(t, 8))
	for name, endpoint := range map[string]string{
		"runtime credential": s.credentials.runtimeDSN,
		"refused password":   wrongPassword.String(),
	} {
		pool, err := OpenBootstrapPool(ctx, endpoint)
		if pool != nil {
			pool.Close()
		}
		if !errors.Is(err, ErrBootstrapAuthority) {
			t.Errorf("bootstrap pool with the %s = %v; want %v", name, err, ErrBootstrapAuthority)
		}
	}
}

// Go developer check, not acceptance proof. The snapshot cleanup must end at
// its limit when PostgreSQL does not answer. The two stand-ins wait for the
// end of the context that each call gets, as the driver does on a connection
// that does not answer. Defect: a cleanup on context.Background(), which never
// ends.
func TestBootstrapSnapshotCleanupEndsAtItsLimit(t *testing.T) {
	done := make(chan error, 1)
	go func() { done <- endBootstrapSnapshot(stalledCleanup{}, stalledCleanup{}, 100*time.Millisecond) }()
	guard := time.NewTimer(10 * time.Second)
	defer guard.Stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("snapshot cleanup with a PostgreSQL that does not answer = %v; want the end of its limit", err)
		}
	case <-guard.C:
		t.Fatal("snapshot cleanup did not end 10 s after a PostgreSQL that does not answer; want an end at its limit")
	}
}

// stalledCleanup stands for a rollback and a close on a connection that does
// not answer: each call returns only when its context ends.
type stalledCleanup struct{}

func (stalledCleanup) Rollback(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (stalledCleanup) Close(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
