package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestStructuralScopeIsolation(t *testing.T) {
	credentials, pool := prepareTestStore(t, 1)
	scopeA := newTestScope(t)
	scopeB := newTestScope(t)
	t.Cleanup(func() {
		cleanupTestScope(t, credentials, scopeA)
		cleanupTestScope(t, credentials, scopeB)
	})

	requestA := testAdmissionRequest(t, scopeA, "same-key", "scope-a")
	requestB := testAdmissionRequest(t, scopeB, "same-key", "scope-b")
	requestB.Spec.RunID = requestA.Spec.RunID
	requestB.Spec.OperationOrPlanRef = requestA.Spec.OperationOrPlanRef
	requestB.Key = requestA.Key
	bindingB, err := contract.NewAPIWorkflowBinding(requestB.Spec.RunID)
	if err != nil {
		t.Fatal(err)
	}
	requestB.WorkflowBinding = bindingB
	requestB.Receipt = testReceipt("receipt-admission-scope-b-shared", "admission", requestB.Spec.RunID)
	requestB.Audit = testAudit("audit-admission-scope-b-shared", "run_admitted", requestB.Spec.RunID)
	requestB.Event = testEvent("event-admission-scope-b-shared", "run_admitted", requestB.Spec.RunID)
	requestB.Outbox = testOutbox("outbox-admission-scope-b-shared", "start_run", requestB.Spec.RunID)
	admitForTest(t, pool, scopeA, requestA)
	admitForTest(t, pool, scopeB, requestB)

	t.Run("equal_ids_remain_structurally_scoped", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, item := range []struct {
			scope   contract.Scope
			foreign contract.Scope
		}{
			{scope: scopeA, foreign: scopeB},
			{scope: scopeB, foreign: scopeA},
		} {
			if err := WithinScope(ctx, pool, item.scope, testActor(item.scope), func(ctx context.Context, tx *Transaction) error {
				record, err := LoadRun(ctx, tx, requestA.Spec.RunID)
				if err != nil {
					return err
				}
				if record.Spec.OrgID != item.scope.OrgID || record.Spec.WorkspaceID != item.scope.WorkspaceID {
					t.Fatalf("loaded run scope = %s/%s; want %s/%s", record.Spec.OrgID, record.Spec.WorkspaceID, item.scope.OrgID, item.scope.WorkspaceID)
				}
				var foreignCount int
				if err := tx.queryRow(ctx, `SELECT count(*) FROM pm.run_specs WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3`, item.foreign.OrgID, item.foreign.WorkspaceID, requestA.Spec.RunID).Scan(&foreignCount); err != nil {
					return err
				}
				if foreignCount != 0 {
					t.Fatalf("foreign scope exposed %d rows", foreignCount)
				}
				return nil
			}); err != nil {
				t.Fatalf("read isolated run: %v", err)
			}
		}
	})

	t.Run("cross_scope_insert_is_rejected_by_with_check", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := WithinScope(ctx, pool, scopeA, testActor(scopeA), func(ctx context.Context, tx *Transaction) error {
			_, err := tx.exec(ctx, `
				INSERT INTO pm.run_specs
					(org_id, workspace_id, run_id, intent_digest, idempotency_fingerprint, spec, accepted_at)
				VALUES ($1, $2, $3, $4, $4, '{}'::jsonb, clock_timestamp())
			`, scopeB.OrgID, scopeB.WorkspaceID, randomUUID(t), string(testDigest("foreign")))
			return err
		})
		if err == nil {
			t.Fatal("cross-scope insert passed RLS WITH CHECK")
		}
	})

	t.Run("missing_scope_denies_rows_and_writes", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := WithinTransaction(ctx, pool, func(ctx context.Context, tx *Transaction) error {
			var count int
			if err := tx.queryRow(ctx, `SELECT count(*) FROM pm.run_specs`).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				t.Fatalf("unscoped transaction read %d scoped rows", count)
			}
			_, err := tx.exec(ctx, `
				INSERT INTO pm.run_specs
					(org_id, workspace_id, run_id, intent_digest, idempotency_fingerprint, spec, accepted_at)
				VALUES ($1, $2, $3, $4, $4, '{}'::jsonb, clock_timestamp())
			`, scopeA.OrgID, scopeA.WorkspaceID, randomUUID(t), string(testDigest("missing-scope")))
			if err == nil {
				t.Fatal("unscoped insert passed RLS WITH CHECK")
			}
			return err
		}); err == nil {
			t.Fatal("unscoped write transaction unexpectedly committed")
		}
	})

	t.Run("scope_is_cleared_after_success_error_and_cancellation", func(t *testing.T) {
		assertScopeCleared := func(expectedPID int32) {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := WithinTransaction(ctx, pool, func(ctx context.Context, tx *Transaction) error {
				var actualPID int32
				var orgID, workspaceID, requestedActorID, requestedActorKind, effectiveActorID, effectiveActorKind string
				if err := tx.queryRow(ctx, `
					SELECT pg_backend_pid(),
					       COALESCE(current_setting('pm.org_id', true), ''),
					       COALESCE(current_setting('pm.workspace_id', true), ''),
					       COALESCE(current_setting('pm.requested_actor_id', true), ''),
					       COALESCE(current_setting('pm.requested_actor_kind', true), ''),
					       COALESCE(current_setting('pm.effective_actor_id', true), ''),
					       COALESCE(current_setting('pm.effective_actor_kind', true), '')
				`).Scan(&actualPID, &orgID, &workspaceID, &requestedActorID, &requestedActorKind, &effectiveActorID, &effectiveActorKind); err != nil {
					return err
				}
				if actualPID != expectedPID || orgID != "" || workspaceID != "" ||
					requestedActorID != "" || requestedActorKind != "" ||
					effectiveActorID != "" || effectiveActorKind != "" {
					t.Fatalf("reused connection retained scope or actor authority")
				}
				return nil
			}); err != nil {
				t.Fatalf("inspect reused connection: %v", err)
			}
		}
		readBackendPID := func(ctx context.Context, tx *Transaction, destination *int32) error {
			return tx.queryRow(ctx, `SELECT pg_backend_pid()`).Scan(destination)
		}

		var backendPID int32
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := WithinScope(ctx, pool, scopeA, testActor(scopeA), func(ctx context.Context, tx *Transaction) error {
			return readBackendPID(ctx, tx, &backendPID)
		}); err != nil {
			t.Fatal(err)
		}
		cancel()
		assertScopeCleared(backendPID)

		sentinel := errors.New("injected callback failure")
		ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
		err := WithinScope(ctx, pool, scopeA, testActor(scopeA), func(ctx context.Context, tx *Transaction) error {
			if err := readBackendPID(ctx, tx, &backendPID); err != nil {
				return err
			}
			return sentinel
		})
		cancel()
		if !errors.Is(err, sentinel) {
			t.Fatalf("callback error = %v; want injected failure", err)
		}
		assertScopeCleared(backendPID)

		ctx, cancel = context.WithCancel(context.Background())
		err = WithinScope(ctx, pool, scopeA, testActor(scopeA), func(ctx context.Context, tx *Transaction) error {
			if err := readBackendPID(ctx, tx, &backendPID); err != nil {
				return err
			}
			cancel()
			return ctx.Err()
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled transaction error = %v; want context.Canceled", err)
		}
		assertScopeCleared(backendPID)
	})

	t.Run("failed_cleanup_destroys_connection", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var poisonedPID int32
		err := WithinScope(ctx, pool, scopeA, testActor(scopeA), func(ctx context.Context, tx *Transaction) error {
			if err := tx.queryRow(ctx, `SELECT pg_backend_pid()`).Scan(&poisonedPID); err != nil {
				return err
			}
			var unused int
			return tx.queryRow(ctx, `SELECT 1 / 0`).Scan(&unused)
		})
		if err == nil {
			t.Fatal("aborted transaction unexpectedly committed")
		}
		var replacementPID int32
		if err := WithinTransaction(ctx, pool, func(ctx context.Context, tx *Transaction) error {
			return tx.queryRow(ctx, `SELECT pg_backend_pid()`).Scan(&replacementPID)
		}); err != nil {
			t.Fatalf("acquire replacement connection: %v", err)
		}
		if replacementPID == poisonedPID {
			t.Fatal("connection with failed scope cleanup was returned to the pool")
		}
	})

	t.Run("explicit_clear_removes_authority", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := WithinScope(ctx, pool, scopeA, testActor(scopeA), func(ctx context.Context, tx *Transaction) error {
			if err := ClearScope(ctx, tx); err != nil {
				return err
			}
			_, err := LoadRun(ctx, tx, requestA.Spec.RunID)
			return err
		})
		if !errors.Is(err, ErrScopeRequired) {
			t.Fatalf("LoadRun after ClearScope error = %v; want ErrScopeRequired", err)
		}
	})

	t.Run("runtime_role_has_no_bypass_ddl_or_truncate", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := WithinTransaction(ctx, pool, func(ctx context.Context, tx *Transaction) error {
			var role string
			var superuser, bypassRLS, createRole, createDB, databaseCreate, databaseTemporary, schemaCreate, truncateRuns, deleteSpecs, updateRunID bool
			if err := tx.queryRow(ctx, `
				SELECT current_user, rolsuper, rolbypassrls, rolcreaterole, rolcreatedb,
				       has_database_privilege(current_user, current_database(), 'CREATE'),
				       has_database_privilege(current_user, current_database(), 'TEMP'),
				       has_schema_privilege(current_user, 'pm', 'CREATE'),
				       has_table_privilege(current_user, 'pm.runs', 'TRUNCATE'),
				       has_table_privilege(current_user, 'pm.run_specs', 'DELETE'),
				       has_column_privilege(current_user, 'pm.runs', 'run_id', 'UPDATE')
				FROM pg_roles WHERE rolname = current_user
			`).Scan(&role, &superuser, &bypassRLS, &createRole, &createDB, &databaseCreate, &databaseTemporary, &schemaCreate, &truncateRuns, &deleteSpecs, &updateRunID); err != nil {
				return err
			}
			if role != RuntimeRole || superuser || bypassRLS || createRole || createDB || databaseCreate || databaseTemporary || schemaCreate || truncateRuns || deleteSpecs || updateRunID {
				t.Fatalf("runtime role retained prohibited PostgreSQL authority")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}
