package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	ApplicationDatabase   = "pm_app"
	MigratorRole          = "pm_app_migrator"
	RuntimeRole           = "pm_app_runtime"
	ControlSchemaRevision = "pm.control/v12"
	ControlSchemaVersion  = int64(12)
)

type PoolConfig struct {
	ConnectionString  string
	MaxConns          int32
	MinConns          int32
	MaxConnLifetime   time.Duration
	MaxConnIdleTime   time.Duration
	HealthCheckPeriod time.Duration
}

type Pool struct {
	inner           *pgxpool.Pool
	rollbackCleanup func(context.Context, pgx.Tx) error
}

func OpenPool(ctx context.Context, configuration PoolConfig) (*Pool, error) {
	if ctx == nil {
		return nil, errors.New("pool context is required")
	}
	if configuration.ConnectionString == "" {
		return nil, errors.New("PostgreSQL connection string is required")
	}
	if configuration.MaxConns <= 0 || configuration.MinConns < 0 || configuration.MinConns > configuration.MaxConns {
		return nil, errors.New("pool connection bounds are invalid")
	}
	if configuration.MaxConnLifetime <= 0 || configuration.MaxConnIdleTime <= 0 || configuration.HealthCheckPeriod <= 0 {
		return nil, errors.New("pool lifetime and health bounds must be positive")
	}
	parsed, err := pgxpool.ParseConfig(configuration.ConnectionString)
	if err != nil {
		return nil, errors.New("parse PostgreSQL pool configuration")
	}
	if parsed.ConnConfig.Database != ApplicationDatabase || parsed.ConnConfig.User != RuntimeRole {
		return nil, fmt.Errorf("runtime pool requires role %s on database %s", RuntimeRole, ApplicationDatabase)
	}
	parsed.MaxConns = configuration.MaxConns
	parsed.MinConns = configuration.MinConns
	parsed.MinIdleConns = 0
	parsed.MaxConnLifetime = configuration.MaxConnLifetime
	parsed.MaxConnIdleTime = configuration.MaxConnIdleTime
	parsed.HealthCheckPeriod = configuration.HealthCheckPeriod
	parsed.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	if parsed.ConnConfig.RuntimeParams == nil {
		parsed.ConnConfig.RuntimeParams = make(map[string]string)
	}
	parsed.ConnConfig.RuntimeParams["timezone"] = "UTC"
	parsed.ConnConfig.RuntimeParams["search_path"] = "pg_catalog,pm"
	parsed.AfterConnect = func(ctx context.Context, connection *pgx.Conn) error {
		_, err := connection.Exec(ctx, `
			SELECT set_config('pm.org_id', '', false),
			       set_config('pm.workspace_id', '', false),
			       set_config('pm.requested_actor_id', '', false),
			       set_config('pm.requested_actor_kind', '', false),
			       set_config('pm.effective_actor_id', '', false),
			       set_config('pm.effective_actor_kind', '', false)
		`)
		return err
	}
	parsed.PrepareConn = func(ctx context.Context, connection *pgx.Conn) (bool, error) {
		if connection.PgConn().TxStatus() != 'I' {
			return false, nil
		}
		var orgID, workspaceID, requestedActorID, requestedActorKind, effectiveActorID, effectiveActorKind string
		err := connection.QueryRow(ctx, `
			SELECT COALESCE(current_setting('pm.org_id', true), ''),
			       COALESCE(current_setting('pm.workspace_id', true), ''),
			       COALESCE(current_setting('pm.requested_actor_id', true), ''),
			       COALESCE(current_setting('pm.requested_actor_kind', true), ''),
			       COALESCE(current_setting('pm.effective_actor_id', true), ''),
			       COALESCE(current_setting('pm.effective_actor_kind', true), '')
		`).Scan(&orgID, &workspaceID, &requestedActorID, &requestedActorKind, &effectiveActorID, &effectiveActorKind)
		if err != nil {
			return false, err
		}
		return orgID == "" && workspaceID == "" &&
			requestedActorID == "" && requestedActorKind == "" &&
			effectiveActorID == "" && effectiveActorKind == "", nil
	}

	inner, err := pgxpool.NewWithConfig(ctx, parsed)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL pool: %w", err)
	}
	pool := &Pool{inner: inner}
	if err := pool.verifyRuntimeIdentity(ctx); err != nil {
		inner.Close()
		return nil, err
	}
	return pool, nil
}

func (pool *Pool) verifyRuntimeIdentity(ctx context.Context) error {
	var database, role, revision string
	var superuser, inherit, createRole, createDB, bypassRLS, replication bool
	var databaseOwner, databaseCreate, databaseTemporary, schemaCreate, truncateStoreTables, deleteSpecs, updateRunID, ownsStoreObjects, migratorMember bool
	err := pool.inner.QueryRow(ctx, `
		SELECT current_database(), current_user, control.revision,
		       roles.rolsuper, roles.rolinherit, roles.rolcreaterole, roles.rolcreatedb,
		       roles.rolbypassrls, roles.rolreplication,
		       pg_get_userbyid(databaseCatalog.datdba) = current_user,
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
		       has_table_privilege(current_user, 'pm.run_specs', 'DELETE'),
		       has_column_privilege(current_user, 'pm.runs', 'run_id', 'UPDATE'),
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
		CROSS JOIN pm.control_schema AS control
		CROSS JOIN pg_database AS databaseCatalog
		WHERE roles.rolname = current_user
		  AND control.singleton
		  AND databaseCatalog.datname = current_database()
	`).Scan(
		&database, &role, &revision,
		&superuser, &inherit, &createRole, &createDB, &bypassRLS, &replication,
		&databaseOwner, &databaseCreate, &databaseTemporary, &schemaCreate, &truncateStoreTables, &deleteSpecs, &updateRunID, &ownsStoreObjects, &migratorMember,
	)
	if err != nil {
		return fmt.Errorf("verify runtime store identity: %w", err)
	}
	if database != ApplicationDatabase || role != RuntimeRole || revision != ControlSchemaRevision {
		return errors.New("runtime database, role, or control schema revision mismatch")
	}
	if superuser || inherit || createRole || createDB || bypassRLS || replication || databaseOwner || databaseCreate || databaseTemporary || schemaCreate || truncateStoreTables || deleteSpecs || updateRunID || ownsStoreObjects || migratorMember {
		return errors.New("runtime role has prohibited PostgreSQL authority")
	}
	return nil
}

func (pool *Pool) Ping(ctx context.Context) error {
	if pool == nil || pool.inner == nil {
		return errors.New("store pool is required")
	}
	return pool.inner.Ping(ctx)
}

func (pool *Pool) Close() {
	if pool != nil && pool.inner != nil {
		pool.inner.Close()
	}
}
