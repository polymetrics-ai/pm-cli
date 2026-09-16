package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type rlsBackendState struct {
	pid                int32
	orgID              string
	workspaceID        string
	requestedActorID   string
	requestedActorKind string
	effectiveActorID   string
	effectiveActorKind string
}

func TestRLSAndPoolCleanup(t *testing.T) {
	credentials, pool := prepareTestStore(t, 1)
	alpha := newTestScope(t)
	beta := contract.Scope{
		OrgID:       alpha.OrgID,
		WorkspaceID: "workspace-" + randomHex(t, 8),
	}
	foreign := newTestScope(t)
	account := ProviderAccount{
		OrgID:          alpha.OrgID,
		AccountID:      "account-" + randomHex(t, 8),
		Provider:       "github",
		AccountWitness: "witness-" + randomHex(t, 8),
		Capacity:       16,
		Revision:       1,
	}
	alphaConnection := Connection{
		Scope:             alpha,
		ConnectionID:      "connection-alpha-" + randomHex(t, 8),
		ConnectorID:       "github",
		ProviderAccountID: account.AccountID,
		Revision:          1,
		CredentialRef:     "",
	}
	betaConnection := Connection{
		Scope:             beta,
		ConnectionID:      "connection-beta-" + randomHex(t, 8),
		ConnectorID:       "github",
		ProviderAccountID: account.AccountID,
		Revision:          1,
		CredentialRef:     "",
	}
	t.Cleanup(func() {
		cleanupTestScope(t, credentials, alpha, beta, foreign)
	})
	registerRLSFixture(t, pool, alpha, beta, account, alphaConnection, betaConnection)

	t.Run("runtime_pool_rejects_truncate_on_any_pm_table", func(t *testing.T) {
		testRuntimeTruncateGate(t, credentials)
	})

	t.Run("organization_metadata_and_workspace_resources_have_distinct_scope", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		permitID := "permit-correct-" + randomHex(t, 8)
		var storedAccount ProviderAccount
		var storedConnection Connection
		var permitCount int
		err := WithinScope(ctx, pool, alpha, testActor(alpha), func(ctx context.Context, tx *Transaction) error {
			var err error
			storedAccount, err = LoadProviderAccount(ctx, tx, account.AccountID)
			if err != nil {
				return err
			}
			storedConnection, err = LoadConnection(ctx, tx, alphaConnection.ConnectionID)
			if err != nil {
				return err
			}
			if err := insertRLSTestPermit(ctx, tx, alpha, permitID, alphaConnection.ConnectionID, account.AccountID); err != nil {
				return err
			}
			return tx.queryRow(ctx, `
				SELECT count(*)
				FROM pm.account_permits
				WHERE org_id = $1 AND workspace_id = $2 AND permit_id = $3
			`, alpha.OrgID, alpha.WorkspaceID, permitID).Scan(&permitCount)
		})
		if err != nil {
			t.Fatalf("correctly scoped metadata transaction failed: %T", err)
		}
		if storedAccount != account || storedConnection != alphaConnection || permitCount != 1 {
			t.Fatal("correct scope did not expose its account, connection, and permit")
		}

		var betaAccount ProviderAccount
		var betaOwnConnection Connection
		var alphaFromBetaErr error
		var betaPID int32
		var alphaRowsFromBeta, alphaPermitsFromBeta int
		err = WithinScope(ctx, pool, beta, testActor(beta), func(ctx context.Context, tx *Transaction) error {
			var err error
			betaAccount, err = LoadProviderAccount(ctx, tx, account.AccountID)
			if err != nil {
				return err
			}
			betaOwnConnection, err = LoadConnection(ctx, tx, betaConnection.ConnectionID)
			if err != nil {
				return err
			}
			_, alphaFromBetaErr = LoadConnection(ctx, tx, alphaConnection.ConnectionID)
			return tx.queryRow(ctx, `
				SELECT
					pg_backend_pid(),
					(SELECT count(*) FROM pm.connections WHERE workspace_id = $1),
					(SELECT count(*) FROM pm.account_permits WHERE workspace_id = $1)
			`, alpha.WorkspaceID).Scan(&betaPID, &alphaRowsFromBeta, &alphaPermitsFromBeta)
		})
		if err != nil {
			t.Fatalf("inspect sibling workspace: %T", err)
		}
		if betaAccount != account {
			t.Fatal("organization-owned account was not visible from a sibling workspace")
		}
		if betaOwnConnection != betaConnection {
			t.Fatal("sibling workspace did not expose its own connection")
		}
		if !errors.Is(alphaFromBetaErr, ErrNotFound) || alphaRowsFromBeta != 0 || alphaPermitsFromBeta != 0 {
			t.Fatal("workspace-owned connection or permit crossed the workspace boundary")
		}

		var alphaReusePID int32
		var alphaConnectionRows int
		err = WithinScope(ctx, pool, alpha, testActor(alpha), func(ctx context.Context, tx *Transaction) error {
			return tx.queryRow(ctx, `
				SELECT pg_backend_pid(), (
					SELECT count(*)
					FROM pm.connections
					WHERE connection_id = $1
				)
			`, alphaConnection.ConnectionID).Scan(&alphaReusePID, &alphaConnectionRows)
		})
		if err != nil {
			t.Fatalf("reuse sibling workspace backend in alpha: %T", err)
		}
		if alphaReusePID != betaPID || alphaConnectionRows != 1 {
			t.Fatal("reused physical backend retained the prior workspace or hid the current workspace")
		}

		var foreignAccountErr error
		var foreignConnectionRows int
		err = WithinScope(ctx, pool, foreign, testActor(foreign), func(ctx context.Context, tx *Transaction) error {
			_, foreignAccountErr = LoadProviderAccount(ctx, tx, account.AccountID)
			return tx.queryRow(ctx, `SELECT count(*) FROM pm.connections`).Scan(&foreignConnectionRows)
		})
		if err != nil {
			t.Fatalf("inspect foreign organization: %T", err)
		}
		if !errors.Is(foreignAccountErr, ErrNotFound) || foreignConnectionRows != 0 {
			t.Fatal("organization or workspace metadata crossed the tenant boundary")
		}
	})

	t.Run("missing_scope_denies_reads_and_cross_table_inserts", func(t *testing.T) {
		inspectUnscopedRLSBackend(t, pool)
		reservedAt := time.Date(2040, time.January, 1, 0, 0, 0, 0, time.UTC)
		cases := []struct {
			name      string
			statement string
			arguments []any
		}{
			{
				name: "provider_account",
				statement: `
					INSERT INTO pm.provider_accounts
						(org_id, provider_account_id, provider, account_witness, capacity, revision)
					VALUES ($1, $2, $3, $4, $5, $6)
				`,
				arguments: []any{alpha.OrgID, "account-unscoped-" + randomHex(t, 8), "github", "witness-unscoped-" + randomHex(t, 8), int64(1), uint64(1)},
			},
			{
				name: "connection",
				statement: `
					INSERT INTO pm.connections
						(org_id, workspace_id, connection_id, connector_id, provider_account_id, revision, credential_ref)
					VALUES ($1, $2, $3, $4, $5, $6, $7)
				`,
				arguments: []any{alpha.OrgID, alpha.WorkspaceID, "connection-unscoped-" + randomHex(t, 8), "github", account.AccountID, uint64(1), ""},
			},
			{
				name: "account_permit",
				statement: `
					INSERT INTO pm.account_permits
						(org_id, workspace_id, permit_id, connection_id, provider_account_id,
						 units, state, fence, owner_id, reserved_at, expires_at, resolved_at)
					VALUES ($1, $2, $3, $4, $5, $6, 'reserved', $7, $8, $9, $10, NULL)
				`,
				arguments: []any{alpha.OrgID, alpha.WorkspaceID, "permit-unscoped-" + randomHex(t, 8), alphaConnection.ConnectionID, account.AccountID, int64(1), int64(1), "owner-unscoped", reservedAt, reservedAt.Add(time.Minute)},
			},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				err := WithinTransaction(ctx, pool, func(ctx context.Context, tx *Transaction) error {
					_, err := tx.exec(ctx, testCase.statement, testCase.arguments...)
					return err
				})
				requireRLSSQLState(t, err, "42501")
			})
		}
	})

	t.Run("wrong_workspace_denies_cross_table_inserts", func(t *testing.T) {
		reservedAt := time.Date(2040, time.January, 1, 0, 0, 0, 0, time.UTC)
		cases := []struct {
			name      string
			statement string
			arguments []any
		}{
			{
				name: "organization_account_for_foreign_tenant",
				statement: `
					INSERT INTO pm.provider_accounts
						(org_id, provider_account_id, provider, account_witness, capacity, revision)
					VALUES ($1, $2, $3, $4, $5, $6)
				`,
				arguments: []any{foreign.OrgID, "account-foreign-" + randomHex(t, 8), "github", "witness-foreign-" + randomHex(t, 8), int64(1), uint64(1)},
			},
			{
				name: "workspace_connection",
				statement: `
					INSERT INTO pm.connections
						(org_id, workspace_id, connection_id, connector_id, provider_account_id, revision, credential_ref)
					VALUES ($1, $2, $3, $4, $5, $6, $7)
				`,
				arguments: []any{beta.OrgID, beta.WorkspaceID, "connection-cross-scope-" + randomHex(t, 8), "github", account.AccountID, uint64(1), ""},
			},
			{
				name: "workspace_permit",
				statement: `
					INSERT INTO pm.account_permits
						(org_id, workspace_id, permit_id, connection_id, provider_account_id,
						 units, state, fence, owner_id, reserved_at, expires_at, resolved_at)
					VALUES ($1, $2, $3, $4, $5, $6, 'reserved', $7, $8, $9, $10, NULL)
				`,
				arguments: []any{beta.OrgID, beta.WorkspaceID, "permit-cross-scope-" + randomHex(t, 8), betaConnection.ConnectionID, account.AccountID, int64(1), int64(1), "owner-cross-scope", reservedAt, reservedAt.Add(time.Minute)},
			},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				err := WithinScope(ctx, pool, alpha, testActor(alpha), func(ctx context.Context, tx *Transaction) error {
					_, err := tx.exec(ctx, testCase.statement, testCase.arguments...)
					return err
				})
				requireRLSSQLState(t, err, "42501")
			})
		}
	})

	t.Run("terminal_paths_clear_scope_and_preserve_only_commits", func(t *testing.T) {
		newConnection := func(id string) Connection {
			return Connection{
				Scope:             alpha,
				ConnectionID:      id,
				ConnectorID:       "github",
				ProviderAccountID: account.AccountID,
				Revision:          1,
				CredentialRef:     "",
			}
		}
		readPID := func(ctx context.Context, tx *Transaction, destination *int32) error {
			return tx.queryRow(ctx, `SELECT pg_backend_pid()`).Scan(destination)
		}

		t.Run("commit", func(t *testing.T) {
			connection := newConnection("connection-commit-" + randomHex(t, 8))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var transactionPID int32
			err := WithinScope(ctx, pool, alpha, testActor(alpha), func(ctx context.Context, tx *Transaction) error {
				if err := RegisterConnection(ctx, tx, connection); err != nil {
					return err
				}
				return readPID(ctx, tx, &transactionPID)
			})
			if err != nil {
				t.Fatalf("commit scoped connection: %T", err)
			}
			state := inspectUnscopedRLSBackend(t, pool)
			if state.pid != transactionPID {
				t.Fatal("healthy commit did not reuse the same physical backend")
			}
			requireRLSConnectionCount(t, pool, alpha, connection.ConnectionID, 1)
		})

		t.Run("application_error", func(t *testing.T) {
			connection := newConnection("connection-error-" + randomHex(t, 8))
			callbackFailure := errors.New("injected scoped callback failure")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var transactionPID int32
			err := WithinScope(ctx, pool, alpha, testActor(alpha), func(ctx context.Context, tx *Transaction) error {
				if err := RegisterConnection(ctx, tx, connection); err != nil {
					return err
				}
				if err := readPID(ctx, tx, &transactionPID); err != nil {
					return err
				}
				return callbackFailure
			})
			if !errors.Is(err, callbackFailure) {
				t.Fatalf("application error was not preserved: %T", err)
			}
			state := inspectUnscopedRLSBackend(t, pool)
			if state.pid != transactionPID {
				t.Fatal("healthy application rollback did not reuse the same physical backend")
			}
			requireRLSConnectionCount(t, pool, alpha, connection.ConnectionID, 0)
		})

		t.Run("request_cancellation", func(t *testing.T) {
			connection := newConnection("connection-cancel-" + randomHex(t, 8))
			deadlineCtx, cancelDeadline := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelDeadline()
			ctx, cancel := context.WithCancel(deadlineCtx)
			var transactionPID int32
			err := WithinScope(ctx, pool, alpha, testActor(alpha), func(ctx context.Context, tx *Transaction) error {
				if err := RegisterConnection(ctx, tx, connection); err != nil {
					return err
				}
				if err := readPID(ctx, tx, &transactionPID); err != nil {
					return err
				}
				cancel()
				return ctx.Err()
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled transaction did not preserve context cancellation: %T", err)
			}
			state := inspectUnscopedRLSBackend(t, pool)
			if state.pid != transactionPID {
				t.Fatal("healthy cancellation rollback did not reuse the same physical backend")
			}
			requireRLSConnectionCount(t, pool, alpha, connection.ConnectionID, 0)
		})

		t.Run("scope_reset_failure_destroys_backend", func(t *testing.T) {
			connection := newConnection("connection-reset-failure-" + randomHex(t, 8))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var transactionPID int32
			err := WithinScope(ctx, pool, alpha, testActor(alpha), func(ctx context.Context, tx *Transaction) error {
				if err := RegisterConnection(ctx, tx, connection); err != nil {
					return err
				}
				if err := readPID(ctx, tx, &transactionPID); err != nil {
					return err
				}
				var unused int
				return tx.queryRow(ctx, `SELECT 1 / 0`).Scan(&unused)
			})
			requireRLSSQLState(t, err, "22012")
			requireRLSSQLState(t, err, "25P02")
			state := inspectUnscopedRLSBackend(t, pool)
			if state.pid == transactionPID {
				t.Fatal("backend with a failed transaction-local scope reset was released to the pool")
			}
			requireRLSConnectionCount(t, pool, alpha, connection.ConnectionID, 0)
		})

		t.Run("rollback_failure_destroys_backend", func(t *testing.T) {
			connection := newConnection("connection-rollback-failure-" + randomHex(t, 8))
			callbackFailure := errors.New("injected callback failure before rollback")
			rollbackFailure := errors.New("injected cleanup rollback failure")
			pool.rollbackCleanup = func(ctx context.Context, tx pgx.Tx) error {
				if err := tx.Rollback(ctx); err != nil {
					return err
				}
				return rollbackFailure
			}
			t.Cleanup(func() { pool.rollbackCleanup = nil })
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var transactionPID int32
			err := WithinScope(ctx, pool, alpha, testActor(alpha), func(ctx context.Context, tx *Transaction) error {
				if err := RegisterConnection(ctx, tx, connection); err != nil {
					return err
				}
				if err := readPID(ctx, tx, &transactionPID); err != nil {
					return err
				}
				return callbackFailure
			})
			pool.rollbackCleanup = nil
			if !errors.Is(err, callbackFailure) || !errors.Is(err, rollbackFailure) {
				t.Fatalf("rollback failure did not preserve both errors: %T", err)
			}
			state := inspectUnscopedRLSBackend(t, pool)
			if state.pid == transactionPID {
				t.Fatal("backend with an uncertain cleanup rollback was released to the pool")
			}
			requireRLSConnectionCount(t, pool, alpha, connection.ConnectionID, 0)
		})
	})

	t.Run("runtime_role_cannot_bypass_rls_or_escalate_storage_authority", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var role string
		var superuser, inherit, createRole, createDB, bypassRLS, replication bool
		var databaseOwner, databaseCreate, databaseTemporary, schemaCreate, anyTruncate, ownsStoreObjects, migratorMember bool
		err := WithinTransaction(ctx, pool, func(ctx context.Context, tx *Transaction) error {
			return tx.queryRow(ctx, `
				SELECT current_user,
				       roles.rolsuper, roles.rolinherit, roles.rolcreaterole, roles.rolcreatedb,
				       roles.rolbypassrls, roles.rolreplication,
				       pg_get_userbyid(database_catalog.datdba) = current_user,
				       has_database_privilege(current_user, current_database(), 'CREATE'),
				       has_database_privilege(current_user, current_database(), 'TEMP'),
				       has_schema_privilege(current_user, 'pm', 'CREATE'),
				       EXISTS (
				           SELECT 1
				           FROM pg_class AS class
				           JOIN pg_namespace AS namespace ON namespace.oid = class.relnamespace
				           WHERE namespace.nspname = 'pm'
				             AND class.relkind IN ('r', 'p', 'f')
				             AND has_table_privilege(current_user, class.oid, 'TRUNCATE')
				       ),
				       EXISTS (
				           SELECT 1
				           FROM pg_class AS class
				           JOIN pg_namespace AS namespace ON namespace.oid = class.relnamespace
				           WHERE namespace.nspname = 'pm'
				             AND class.relkind IN ('r', 'p', 'S', 'v', 'm', 'f')
				             AND pg_get_userbyid(class.relowner) = current_user
				       ),
				       pg_has_role(current_user, 'pm_app_migrator', 'MEMBER')
				FROM pg_roles AS roles
				CROSS JOIN pg_database AS database_catalog
				WHERE roles.rolname = current_user
				  AND database_catalog.datname = current_database()
			`).Scan(
				&role,
				&superuser, &inherit, &createRole, &createDB, &bypassRLS, &replication,
				&databaseOwner, &databaseCreate, &databaseTemporary, &schemaCreate,
				&anyTruncate, &ownsStoreObjects, &migratorMember,
			)
		})
		if err != nil {
			t.Fatalf("inspect runtime authority: %T", err)
		}
		if role != RuntimeRole || superuser || inherit || createRole || createDB || bypassRLS || replication ||
			databaseOwner || databaseCreate || databaseTemporary || schemaCreate || anyTruncate || ownsStoreObjects || migratorMember {
			t.Fatal("runtime role retained prohibited PostgreSQL authority")
		}

		rollbackSuccess := errors.New("rollback unexpectedly permitted authority change")
		ddlProbe := pgx.Identifier{"pm", "rls_runtime_ddl_" + randomHex(t, 8)}.Sanitize()
		actions := []struct {
			name      string
			statement string
		}{
			{name: "bypass_rls", statement: `ALTER ROLE pm_app_runtime BYPASSRLS`},
			{name: "ddl", statement: `CREATE TABLE ` + ddlProbe + ` (id bigint PRIMARY KEY)`},
			{name: "truncate", statement: `TRUNCATE TABLE pm.connections`},
			{name: "ownership", statement: `ALTER TABLE pm.connections OWNER TO pm_app_runtime`},
		}
		for _, action := range actions {
			t.Run(action.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				var statementErr error
				err := WithinTransaction(ctx, pool, func(ctx context.Context, tx *Transaction) error {
					_, statementErr = tx.exec(ctx, action.statement)
					if statementErr == nil {
						return rollbackSuccess
					}
					return statementErr
				})
				if statementErr == nil {
					if !errors.Is(err, rollbackSuccess) {
						t.Fatalf("permitted authority change was not rolled back: %T", err)
					}
					t.Fatal("runtime privileged statement unexpectedly succeeded")
				}
				requireRLSSQLState(t, statementErr, "42501")
			})
		}
	})
}

func registerRLSFixture(
	t *testing.T,
	pool *Pool,
	alpha contract.Scope,
	beta contract.Scope,
	account ProviderAccount,
	alphaConnection Connection,
	betaConnection Connection,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := WithinScope(ctx, pool, alpha, testActor(alpha), func(ctx context.Context, tx *Transaction) error {
		if err := RegisterWorkspace(ctx, tx, WorkspaceIdentity{Scope: alpha, Name: "alpha-" + randomHex(t, 4)}); err != nil {
			return err
		}
		if err := RegisterProviderAccount(ctx, tx, account); err != nil {
			return err
		}
		return RegisterConnection(ctx, tx, alphaConnection)
	}); err != nil {
		t.Fatalf("register alpha RLS fixture: %T", err)
	}
	if err := WithinScope(ctx, pool, beta, testActor(beta), func(ctx context.Context, tx *Transaction) error {
		if err := RegisterWorkspace(ctx, tx, WorkspaceIdentity{Scope: beta, Name: "beta-" + randomHex(t, 4)}); err != nil {
			return err
		}
		return RegisterConnection(ctx, tx, betaConnection)
	}); err != nil {
		t.Fatalf("register beta RLS fixture: %T", err)
	}
}

func insertRLSTestPermit(
	ctx context.Context,
	tx *Transaction,
	scope contract.Scope,
	permitID string,
	connectionID string,
	accountID string,
) error {
	reservedAt := time.Date(2040, time.January, 1, 0, 0, 0, 0, time.UTC)
	_, err := tx.exec(ctx, `
		INSERT INTO pm.account_permits (
			org_id, workspace_id, permit_id, connection_id, provider_account_id,
			units, state, fence, owner_id, reserved_at, expires_at, resolved_at
		) VALUES ($1, $2, $3, $4, $5, $6, 'reserved', $7, $8, $9, $10, NULL)
	`,
		scope.OrgID,
		scope.WorkspaceID,
		permitID,
		connectionID,
		accountID,
		int64(1),
		int64(1),
		"owner-r03",
		reservedAt,
		reservedAt.Add(time.Minute),
	)
	return err
}

func inspectUnscopedRLSBackend(t *testing.T, pool *Pool) rlsBackendState {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var state rlsBackendState
	var accountRows, connectionRows, permitRows int
	err := WithinTransaction(ctx, pool, func(ctx context.Context, tx *Transaction) error {
		if err := tx.queryRow(ctx, `
			SELECT pg_backend_pid(),
			       COALESCE(current_setting('pm.org_id', true), ''),
			       COALESCE(current_setting('pm.workspace_id', true), ''),
			       COALESCE(current_setting('pm.requested_actor_id', true), ''),
			       COALESCE(current_setting('pm.requested_actor_kind', true), ''),
			       COALESCE(current_setting('pm.effective_actor_id', true), ''),
			       COALESCE(current_setting('pm.effective_actor_kind', true), '')
		`).Scan(
			&state.pid,
			&state.orgID,
			&state.workspaceID,
			&state.requestedActorID,
			&state.requestedActorKind,
			&state.effectiveActorID,
			&state.effectiveActorKind,
		); err != nil {
			return err
		}
		return tx.queryRow(ctx, `
			SELECT
				(SELECT count(*) FROM pm.provider_accounts),
				(SELECT count(*) FROM pm.connections),
				(SELECT count(*) FROM pm.account_permits)
		`).Scan(&accountRows, &connectionRows, &permitRows)
	})
	if err != nil {
		t.Fatalf("inspect unscoped physical backend: %T", err)
	}
	if state.orgID != "" || state.workspaceID != "" ||
		state.requestedActorID != "" || state.requestedActorKind != "" ||
		state.effectiveActorID != "" || state.effectiveActorKind != "" {
		t.Fatal("unscoped physical backend retained transaction-local authority")
	}
	if accountRows != 0 || connectionRows != 0 || permitRows != 0 {
		t.Fatal("unscoped physical backend exposed tenant rows")
	}
	return state
}

func requireRLSConnectionCount(t *testing.T, pool *Pool, scope contract.Scope, connectionID string, expected int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var actual int
	err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return tx.queryRow(ctx, `
			SELECT count(*)
			FROM pm.connections
			WHERE org_id = $1 AND workspace_id = $2 AND connection_id = $3
		`, scope.OrgID, scope.WorkspaceID, connectionID).Scan(&actual)
	})
	if err != nil {
		t.Fatalf("count scoped connection: %T", err)
	}
	if actual != expected {
		t.Fatalf("scoped connection count = %d; want %d", actual, expected)
	}
}

func testRuntimeTruncateGate(t *testing.T, credentials testStoreCredentials) {
	t.Helper()
	tableName := "rls_truncate_gate_" + randomHex(t, 8)
	qualifiedTable := pgx.Identifier{"pm", tableName}.Sanitize()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	migrator, err := pgx.Connect(ctx, credentials.migratorDSN)
	if err != nil {
		t.Fatal("connect migrator for runtime privilege gate")
	}
	if _, err := migrator.Exec(ctx, `CREATE TABLE `+qualifiedTable+` (id bigint PRIMARY KEY)`); err != nil {
		_ = migrator.Close(context.Background())
		t.Fatalf("create runtime privilege gate table: SQLSTATE %s", rlsSQLState(err))
	}
	created := true
	cleanup := func() error {
		if !created {
			return nil
		}
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelCleanup()
		cleanupConnection, err := pgx.Connect(cleanupCtx, credentials.migratorDSN)
		if err != nil {
			return err
		}
		defer func() { _ = cleanupConnection.Close(context.Background()) }()
		_, revokeErr := cleanupConnection.Exec(cleanupCtx, `REVOKE TRUNCATE ON TABLE `+qualifiedTable+` FROM pm_app_runtime`)
		_, dropErr := cleanupConnection.Exec(cleanupCtx, `DROP TABLE `+qualifiedTable)
		if dropErr == nil {
			created = false
		}
		return errors.Join(revokeErr, dropErr)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Fatalf("cleanup runtime privilege gate object: SQLSTATE %s", rlsSQLState(err))
		}
	})
	if _, err := migrator.Exec(ctx, `REVOKE ALL ON TABLE `+qualifiedTable+` FROM PUBLIC, pm_app_runtime`); err != nil {
		_ = migrator.Close(context.Background())
		t.Fatalf("confine runtime privilege gate table: SQLSTATE %s", rlsSQLState(err))
	}
	if _, err := migrator.Exec(ctx, `GRANT TRUNCATE ON TABLE `+qualifiedTable+` TO pm_app_runtime`); err != nil {
		_ = migrator.Close(context.Background())
		t.Fatalf("install runtime privilege gate fault: SQLSTATE %s", rlsSQLState(err))
	}
	var granted bool
	if err := migrator.QueryRow(ctx, `SELECT has_table_privilege('pm_app_runtime', $1::text, 'TRUNCATE')`, qualifiedTable).Scan(&granted); err != nil {
		_ = migrator.Close(context.Background())
		t.Fatalf("inspect runtime privilege gate fault: SQLSTATE %s", rlsSQLState(err))
	}
	if err := migrator.Close(ctx); err != nil {
		t.Fatal("close runtime privilege gate connection")
	}
	if !granted {
		t.Fatal("runtime privilege gate fault was not installed")
	}

	candidate, openErr := OpenPool(ctx, rlsTestPoolConfig(credentials.runtimeDSN))
	if candidate != nil {
		candidate.Close()
	}
	if openErr == nil {
		t.Error("runtime pool accepted TRUNCATE authority on a PM table")
	}
	if err := cleanup(); err != nil {
		t.Fatalf("remove runtime privilege gate fault: SQLSTATE %s", rlsSQLState(err))
	}
	positive, err := OpenPool(ctx, rlsTestPoolConfig(credentials.runtimeDSN))
	if err != nil {
		t.Fatalf("runtime pool did not recover after test-owned privilege cleanup: %T", err)
	}
	positive.Close()
}

func rlsTestPoolConfig(connectionString string) PoolConfig {
	return PoolConfig{
		ConnectionString:  connectionString,
		MaxConns:          1,
		MinConns:          0,
		MaxConnLifetime:   5 * time.Minute,
		MaxConnIdleTime:   time.Minute,
		HealthCheckPeriod: 30 * time.Second,
	}
}

func requireRLSSQLState(t *testing.T, err error, expected string) {
	t.Helper()
	if !rlsErrorHasSQLState(err, expected) {
		t.Fatalf("PostgreSQL error did not include SQLSTATE %s", expected)
	}
}

func rlsErrorHasSQLState(err error, expected string) bool {
	if err == nil {
		return false
	}
	if databaseError, ok := err.(*pgconn.PgError); ok {
		return databaseError.Code == expected
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, nested := range joined.Unwrap() {
			if rlsErrorHasSQLState(nested, expected) {
				return true
			}
		}
		return false
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return rlsErrorHasSQLState(wrapped.Unwrap(), expected)
	}
	return false
}

func rlsSQLState(err error) string {
	if err == nil {
		return ""
	}
	var databaseError *pgconn.PgError
	if errors.As(err, &databaseError) {
		return databaseError.Code
	}
	return "non-postgresql-error"
}
