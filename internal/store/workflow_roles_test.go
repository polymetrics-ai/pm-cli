package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestWorkflowRouteRoleBoundary(t *testing.T) {
	credentials, pool := prepareTestStore(t, 2)
	scope := newTestScope(t)
	t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })

	request := testAdmissionRequest(t, scope, "workflow-role-boundary", "workflow-role-boundary")
	admitForTest(t, pool, scope, request)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var routes []WorkflowRoute
	if err := WithinTransaction(ctx, pool, func(ctx context.Context, tx *Transaction) error {
		var err error
		routes, err = ClaimWorkflowRoutes(ctx, tx, WorkflowRouteRequest{
			OwnerID: "workflow-role-boundary",
			Limit:   1,
			TTL:     30 * time.Second,
		})
		return err
	}); err != nil {
		t.Fatalf("claim workflow role fixture: %v", err)
	}
	if len(routes) != 1 || !routes[0].Claim.Scope.Equal(scope) || routes[0].Claim.RunID != request.Spec.RunID {
		t.Fatalf("claimed workflow route = %#v; want exact admitted route", routes)
	}
	if err := WithinScope(ctx, pool, scope, routes[0].Actor, func(ctx context.Context, tx *Transaction) error {
		intent, err := SealWorkflowCommand(ctx, tx, routes[0].Claim, WorkflowCommandPlan{
			Namespace:        "pm-test",
			TaskQueue:        "pm-control",
			WorkflowType:     "pm.run.v1",
			WorkflowID:       request.WorkflowBinding.WorkflowID,
			Origin:           request.WorkflowBinding.Origin,
			Generation:       request.WorkflowBinding.Generation,
			ExecutionTimeout: request.BudgetSnapshot.Policy.RunLifetime,
			BudgetDigest:     request.BudgetSnapshot.Digest,
		})
		if err != nil {
			return err
		}
		_, err = PersistNoDispatchWitness(ctx, tx, routes[0].Claim, intent.Digest)
		return err
	}); err != nil {
		t.Fatalf("seed scoped workflow metadata: %v", err)
	}

	for _, table := range []string{"pm.workflow_command_intents", "pm.workflow_reconciliation_witnesses"} {
		t.Run(table, func(t *testing.T) {
			var count int
			if err := WithinTransaction(ctx, pool, func(ctx context.Context, tx *Transaction) error {
				return tx.queryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count)
			}); err != nil {
				t.Fatalf("read unscoped workflow metadata: %v", err)
			}
			if count != 0 {
				t.Fatalf("unscoped runtime SELECT exposed %d workflow rows", count)
			}

			err := WithinTransaction(ctx, pool, func(ctx context.Context, tx *Transaction) error {
				_, err := tx.exec(ctx, "UPDATE "+table+" SET run_id = run_id WHERE run_id = $1", request.Spec.RunID)
				return err
			})
			var databaseError *pgconn.PgError
			if !errors.As(err, &databaseError) || databaseError.Code != "42501" {
				t.Fatalf("unscoped runtime UPDATE error = %v; want SQLSTATE 42501", err)
			}
		})
	}

	var runtimeMigratorMember bool
	var functionOwner string
	var securityDefiner, publicExecute, runtimeExecute bool
	var functionSettings []string
	type tableAuthority struct {
		name                     string
		rowSecurity, forceRLS    bool
		selectGrant, insertGrant bool
		prohibitedGrant          bool
	}
	authorities := []tableAuthority{{name: "workflow_command_intents"}, {name: "workflow_reconciliation_witnesses"}}
	if err := WithinTransaction(ctx, pool, func(ctx context.Context, tx *Transaction) error {
		if err := tx.queryRow(ctx, `
			SELECT pg_has_role(current_user, 'pm_app_migrator', 'MEMBER')
		`).Scan(&runtimeMigratorMember); err != nil {
			return err
		}
		if err := tx.queryRow(ctx, `
			SELECT procedure.prosecdef,
			       pg_get_userbyid(procedure.proowner),
			       COALESCE(procedure.proconfig, ARRAY[]::text[]),
			       EXISTS (
			           SELECT 1
			           FROM aclexplode(COALESCE(procedure.proacl, acldefault('f', procedure.proowner))) AS expanded_acl
			           WHERE expanded_acl.grantee = 0 AND expanded_acl.privilege_type = 'EXECUTE'
			       ),
			       has_function_privilege(
			           'pm_app_runtime',
			           procedure.oid,
			           'EXECUTE'
			       )
			FROM pg_proc AS procedure
			WHERE procedure.oid = 'pm.claim_workflow_outbox(text,integer,interval)'::regprocedure
		`).Scan(&securityDefiner, &functionOwner, &functionSettings, &publicExecute, &runtimeExecute); err != nil {
			return err
		}
		for index := range authorities {
			authority := &authorities[index]
			if err := tx.queryRow(ctx, `
				SELECT class.relrowsecurity,
				       class.relforcerowsecurity,
				       has_table_privilege('pm_app_runtime', class.oid, 'SELECT'),
				       has_table_privilege('pm_app_runtime', class.oid, 'INSERT'),
				       has_table_privilege('pm_app_runtime', class.oid, 'UPDATE')
				           OR has_table_privilege('pm_app_runtime', class.oid, 'DELETE')
				           OR has_table_privilege('pm_app_runtime', class.oid, 'TRUNCATE')
				           OR has_table_privilege('pm_app_runtime', class.oid, 'REFERENCES')
				           OR has_table_privilege('pm_app_runtime', class.oid, 'TRIGGER')
				FROM pg_class AS class
				JOIN pg_namespace AS namespace ON namespace.oid = class.relnamespace
				WHERE namespace.nspname = 'pm' AND class.relname = $1
			`, authority.name).Scan(
				&authority.rowSecurity,
				&authority.forceRLS,
				&authority.selectGrant,
				&authority.insertGrant,
				&authority.prohibitedGrant,
			); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("inspect workflow role boundary: %v", err)
	}
	if runtimeMigratorMember {
		t.Fatal("runtime role can assume migrator authority")
	}
	if !securityDefiner || functionOwner != MigratorRole || publicExecute || !runtimeExecute ||
		!slices.Equal(functionSettings, []string{"search_path=pg_catalog, pg_temp"}) {
		t.Fatalf(
			"claim function authority = definer:%t owner:%q settings:%v public:%t runtime:%t",
			securityDefiner,
			functionOwner,
			functionSettings,
			publicExecute,
			runtimeExecute,
		)
	}
	for _, authority := range authorities {
		if !authority.rowSecurity || !authority.forceRLS || !authority.selectGrant || !authority.insertGrant || authority.prohibitedGrant {
			t.Fatalf("workflow table authority = %#v; want forced scoped RLS and SELECT/INSERT only", authority)
		}
	}
}
