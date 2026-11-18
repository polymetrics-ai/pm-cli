package store

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// asBootstrapRole runs one statement as the bootstrap role, with the scope
// settings of the given deployment, organization and workspace.
func (s bootstrapTestStore) asBootstrapRole(ctx context.Context, deploymentID, orgID, workspaceID, statement string, arguments ...any) error {
	return WithinTransaction(ctx, s.pool, func(ctx context.Context, tx *Transaction) error {
		if _, err := tx.exec(ctx, `
			SELECT set_config('pm.deployment_id', $1, true), set_config('pm.org_id', $2, true),
			       set_config('pm.workspace_id', $3, true)`, deploymentID, orgID, workspaceID); err != nil {
			return err
		}
		_, err := tx.exec(ctx, statement, arguments...)
		return err
	})
}

func refusedForAuthority(err error) bool {
	var databaseError *pgconn.PgError
	return errors.As(err, &databaseError) && databaseError.Code == "42501"
}

// Check (ADR 0006, F1): the bootstrap credential cannot add a row to an
// organization that a bootstrap created. Direct SQL as the bootstrap role
// sets the scope settings of that organization and inserts one row into each
// bootstrap table, also an identity, a login binding and an admin membership.
// Each insert must be refused (42501), and nothing changes. Defect: insert
// grants and caller-set scope settings let a leaked credential add an admin
// to an existing organization.
func TestBootstrapCredentialCannotChangeAnExistingOrganization(t *testing.T) {
	s := prepareBootstrapTestStore(t)
	deploymentID := "deployment-" + randomHex(t, 8)
	scope := newTestScope(t)
	t.Cleanup(func() { s.removeBootstrapRows(t, deploymentID, scope.OrgID) })
	ctx, cancel := context.WithTimeout(context.Background(), bootstrapWaitLimit)
	defer cancel()
	if err := Bootstrap(ctx, s.pool, newBootstrapTestRequest(t, deploymentID, scope)); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	before := s.readBootstrapRows(t, deploymentID, scope.OrgID)

	identityID := "identity-" + randomHex(t, 8)
	subject := "subject-" + randomHex(t, 8)
	for _, insert := range []struct {
		table, sql string
		arguments  []any
	}{
		{"identities", `
			INSERT INTO pm.identities (org_id, identity_id, kind, issuer, subject, revision)
			VALUES ($1, $2, 'user', 'https://identity.test.invalid', $3, 1)`, []any{scope.OrgID, identityID, subject}},
		{"auth_bootstrap_identities", `
			INSERT INTO pm.auth_bootstrap_identities (
				deployment_id, issuer, subject, org_id, principal_id, principal_kind, can_approve_devices, enabled
			) VALUES ($1, 'https://identity.test.invalid', $2, $3, $4, 'user', true, true)`,
			[]any{deploymentID, subject, scope.OrgID, identityID}},
		{"memberships", `
			INSERT INTO pm.memberships (
				org_id, workspace_id, membership_id, identity_id, role,
				resource_grants, permissions, connection_ids, operation_ids, policy_epoch, control_revision
			) VALUES ($1, $2, $3, $4, 'admin', ARRAY['permission:workspace.access.manage'],
				ARRAY['workspace.access.manage'], ARRAY[]::text[], ARRAY[]::text[], 1, 1)`,
			[]any{scope.OrgID, scope.WorkspaceID, "membership-" + randomHex(t, 8), identityID}},
		{"workspace_namespaces", `
			INSERT INTO pm.workspace_namespaces (org_id, workspace_id, name) VALUES ($1, $2, 'Second workspace')`,
			[]any{scope.OrgID, "workspace-" + randomHex(t, 8)}},
		{"provider_accounts", `
			INSERT INTO pm.provider_accounts (org_id, provider_account_id, provider, account_witness, capacity, revision, display_name)
			VALUES ($1, $2, 'postgres', $2, 1, 1, 'Second account')`, []any{scope.OrgID, "provider-account-" + randomHex(t, 8)}},
		{"bootstrap_records", `
			INSERT INTO pm.bootstrap_records (deployment_id, org_id, workspace_id, admin_identity_id, membership_id)
			VALUES ($1, $2, $3, $4, 'membership')`, []any{"deployment-" + randomHex(t, 8), scope.OrgID, scope.WorkspaceID, identityID}},
		{"workspace_audit_entries", `
			INSERT INTO pm.workspace_audit_entries (
				org_id, workspace_id, audit_id, requested_actor_id, requested_actor_kind,
				effective_actor_id, effective_actor_kind, action, target_ref, payload
			) VALUES ($1, $2, $3, 'bootstrap', 'operator', 'bootstrap', 'operator', 'workspace.bootstrap', $2, '{}'::jsonb)`,
			[]any{scope.OrgID, scope.WorkspaceID, "audit-" + randomHex(t, 8)}},
	} {
		if err := s.asBootstrapRole(ctx, deploymentID, scope.OrgID, scope.WorkspaceID, insert.sql, insert.arguments...); !refusedForAuthority(err) {
			t.Errorf("direct insert into pm.%s of an existing organization as the bootstrap role = %v; want refusal 42501", insert.table, err)
		}
	}
	if got := s.readBootstrapRows(t, deploymentID, scope.OrgID); !reflect.DeepEqual(got, before) {
		t.Fatalf("refused direct inserts changed the organization:\n got %v\nwant %v", got, before)
	}
}

// Check (crrao N1): the bootstrap credential cannot read the login bindings
// or the bootstrap records. Defect: a column read lets a leaked credential
// list the issuer and subject of every login of the deployment.
func TestBootstrapCredentialCannotReadTheLoginBindings(t *testing.T) {
	s := prepareBootstrapTestStore(t)
	deploymentID := "deployment-" + randomHex(t, 8)
	scope := newTestScope(t)
	t.Cleanup(func() { s.removeBootstrapRows(t, deploymentID, scope.OrgID) })
	ctx, cancel := context.WithTimeout(context.Background(), bootstrapWaitLimit)
	defer cancel()
	if err := Bootstrap(ctx, s.pool, newBootstrapTestRequest(t, deploymentID, scope)); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	for name, statement := range map[string]string{
		"login bindings":    `SELECT issuer, subject FROM pm.auth_bootstrap_identities`,
		"bootstrap records": `SELECT deployment_id, org_id FROM pm.bootstrap_records`,
	} {
		if err := s.asBootstrapRole(ctx, deploymentID, scope.OrgID, scope.WorkspaceID, statement); !refusedForAuthority(err) {
			t.Errorf("read of the %s as the bootstrap role = %v; want refusal 42501", name, err)
		}
	}
}

// Check (ADR 0006, F1 and F2): pm.operator_bootstrap refuses a taken scope
// and tells the same input from another one. Each refusal writes nothing.
// Defect: a bootstrap of an organization that already has rows, or a retry
// that cannot tell its own committed bootstrap from another one.
func TestOperatorBootstrapRefusesATakenScope(t *testing.T) {
	s := prepareBootstrapTestStore(t)
	deploymentID := "deployment-" + randomHex(t, 8)
	scope, existing := newTestScope(t), newTestScope(t)
	t.Cleanup(func() { s.removeBootstrapRows(t, deploymentID, scope.OrgID, existing.OrgID) })
	ctx, cancel := context.WithTimeout(context.Background(), bootstrapWaitLimit)
	defer cancel()
	first := newBootstrapTestRequest(t, deploymentID, scope)
	if err := Bootstrap(ctx, s.pool, first); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}

	// An organization with rows and no bootstrap record, made by the stack
	// administrator as a later product path would.
	admin, err := pgx.Connect(ctx, s.admin)
	if err != nil {
		t.Fatalf("connect the stack administrator: %v", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	if _, err := admin.Exec(ctx, `
		INSERT INTO pm.identities (org_id, identity_id, kind, issuer, subject, revision)
		VALUES ($1, $2, 'user', 'https://identity.test.invalid', $3, 1)`,
		existing.OrgID, "identity-"+randomHex(t, 8), "subject-"+randomHex(t, 8)); err != nil {
		t.Fatalf("make an organization with rows: %v", err)
	}
	before := s.readBootstrapRows(t, deploymentID, scope.OrgID, existing.OrgID)

	// The same operator input with new identifiers, as a retry makes them.
	same := newBootstrapTestRequest(t, deploymentID, scope)
	same.WorkspaceName, same.AdminIssuer, same.AdminSubject = first.WorkspaceName, first.AdminIssuer, first.AdminSubject
	for index := range same.ProviderAccounts {
		same.ProviderAccounts[index].ConnectorID = first.ProviderAccounts[index].ConnectorID
		same.ProviderAccounts[index].DisplayName = first.ProviderAccounts[index].DisplayName
	}
	if err := Bootstrap(ctx, s.pool, same); !errors.Is(err, ErrBootstrapScopeExists) || errors.Is(err, ErrBootstrapScopeOtherInput) {
		t.Errorf("retry with the same input = %v; want %v and not %v", err, ErrBootstrapScopeExists, ErrBootstrapScopeOtherInput)
	}
	other := same
	other.WorkspaceName = "Another workspace"
	if err := Bootstrap(ctx, s.pool, other); !errors.Is(err, ErrBootstrapScopeOtherInput) {
		t.Errorf("bootstrap of the scope with another input = %v; want %v", err, ErrBootstrapScopeOtherInput)
	}
	if err := Bootstrap(ctx, s.pool, newBootstrapTestRequest(t, deploymentID, existing)); !errors.Is(err, ErrBootstrapScopeOtherInput) {
		t.Errorf("bootstrap of an organization that has rows = %v; want %v", err, ErrBootstrapScopeOtherInput)
	}
	if got := s.readBootstrapRows(t, deploymentID, scope.OrgID, existing.OrgID); !reflect.DeepEqual(got, before) {
		t.Fatalf("refused bootstraps changed the rows:\n got %v\nwant %v", got, before)
	}
}

// Check (ADR 0006, F1): pm.operator_bootstrap is a migrator-owned definer
// function with a fixed search path, and only the bootstrap role may execute
// it. Defect: another role, or PUBLIC, can write a bootstrap.
func TestOperatorBootstrapFunctionIsOnlyForTheBootstrapRole(t *testing.T) {
	s := prepareBootstrapTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), bootstrapWaitLimit)
	defer cancel()
	admin, err := pgx.Connect(ctx, s.admin)
	if err != nil {
		t.Fatalf("connect the stack administrator: %v", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	var definer, public, runtime, bootstrap bool
	var owner string
	var configuration []string
	if err := admin.QueryRow(ctx, `
		SELECT p.prosecdef, pg_get_userbyid(p.proowner), p.proconfig,
		       EXISTS (SELECT 1 FROM aclexplode(p.proacl) AS acl WHERE acl.grantee = 0),
		       has_function_privilege('pm_app_runtime', p.oid, 'EXECUTE'),
		       has_function_privilege('pm_app_bootstrap', p.oid, 'EXECUTE')
		FROM pg_proc AS p
		WHERE p.oid = 'pm.operator_bootstrap(text, text, text, text, text, text, text, text, text, text[], text[], text[], text[], bigint[], text, text)'::regprocedure
	`).Scan(&definer, &owner, &configuration, &public, &runtime, &bootstrap); err != nil {
		t.Fatalf("read pm.operator_bootstrap: %v", err)
	}
	if !definer || owner != "pm_app_migrator" || !reflect.DeepEqual(configuration, []string{"search_path=pg_catalog, pg_temp"}) ||
		public || runtime || !bootstrap {
		t.Fatalf("pm.operator_bootstrap: definer %t, owner %q, settings %q, PUBLIC %t, runtime %t, bootstrap %t; "+
			"want a definer owned by pm_app_migrator with only the fixed search path, executable only by pm_app_bootstrap",
			definer, owner, configuration, public, runtime, bootstrap)
	}

	runtimeConnection, err := pgx.Connect(ctx, s.credentials.runtimeDSN)
	if err != nil {
		t.Fatalf("connect the runtime role: %v", err)
	}
	defer func() { _ = runtimeConnection.Close(context.Background()) }()
	scope := newTestScope(t)
	_, err = runtimeConnection.Exec(ctx, `
		SELECT pm.operator_bootstrap($1, $2, $3, 'First workspace', 'identity-1', 'https://identity.test.invalid',
			'subject-1', 'membership-1', 'admin', ARRAY['workspace.read'], ARRAY[]::text[], ARRAY[]::text[],
			ARRAY[]::text[], ARRAY[]::bigint[], 'audit-1', 'workspace.bootstrap')`,
		"deployment-"+randomHex(t, 8), scope.OrgID, scope.WorkspaceID)
	if !refusedForAuthority(err) {
		t.Fatalf("pm.operator_bootstrap as the runtime role = %v; want refusal 42501", err)
	}
}
