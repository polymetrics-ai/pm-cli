package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

const (
	migrationTableName = "pm.control_schema_migrations"
	migrationLockID    = int64(0x504d434f4e54524c)
)

//go:embed migrations/postgres/*.sql
var migrationFiles embed.FS

type UnsupportedDowngradeError struct {
	Current int64
	Target  int64
}

func (err *UnsupportedDowngradeError) Error() string {
	return fmt.Sprintf("%v: current version %d is newer than target version %d", ErrUnsupportedDowngrade, err.Current, err.Target)
}

func (err *UnsupportedDowngradeError) Unwrap() error {
	return ErrUnsupportedDowngrade
}

func Migrate(ctx context.Context, connectionString string) error {
	rooted, err := fs.Sub(migrationFiles, "migrations/postgres")
	if err != nil {
		return fmt.Errorf("open embedded store migrations: %w", err)
	}
	return migrateWithFiles(ctx, connectionString, rooted)
}

func migrateWithFiles(ctx context.Context, connectionString string, migrations fs.FS) (resultErr error) {
	if ctx == nil {
		return errors.New("migration context is required")
	}
	if connectionString == "" {
		return errors.New("PostgreSQL migrator connection string is required")
	}
	if migrations == nil {
		return errors.New("migration filesystem is required")
	}
	configuration, err := pgx.ParseConfig(connectionString)
	if err != nil {
		return errors.New("parse PostgreSQL migrator configuration")
	}
	if configuration.Database != ApplicationDatabase || configuration.User != MigratorRole {
		return fmt.Errorf("migrations require role %s on database %s", MigratorRole, ApplicationDatabase)
	}
	configuration.DefaultQueryExecMode = pgx.QueryExecModeExec
	if configuration.RuntimeParams == nil {
		configuration.RuntimeParams = make(map[string]string)
	}
	configuration.RuntimeParams["timezone"] = "UTC"
	configuration.RuntimeParams["search_path"] = "pg_catalog,pm"

	database := stdlib.OpenDB(*configuration)
	database.SetMaxOpenConns(2)
	database.SetMaxIdleConns(2)
	database.SetConnMaxLifetime(30 * time.Minute)
	database.SetConnMaxIdleTime(5 * time.Minute)
	defer database.Close()
	if err := verifyMigratorIdentity(ctx, database); err != nil {
		return err
	}

	lockConnection, err := database.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration lock connection: %w", err)
	}
	defer lockConnection.Close()
	if _, err := lockConnection.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := detachedCleanupContext()
		defer cancel()
		var unlocked bool
		unlockErr := lockConnection.QueryRowContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrationLockID).Scan(&unlocked)
		if unlockErr == nil && !unlocked {
			unlockErr = errors.New("migration advisory lock was not held")
		}
		if unlockErr != nil {
			resultErr = joinStoreErrors(resultErr, fmt.Errorf("release migration lock: %w", unlockErr))
		}
	}()

	provider, err := goose.NewProvider(
		goose.DialectPostgres,
		database,
		migrations,
		goose.WithTableName(migrationTableName),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		return fmt.Errorf("create store migration provider: %w", err)
	}
	if len(provider.ListSources()) == 0 {
		return errors.New("store migration set is empty")
	}
	current, target, err := provider.GetVersions(ctx)
	if err != nil {
		return fmt.Errorf("read store migration versions: %w", err)
	}
	if current > target {
		return &UnsupportedDowngradeError{Current: current, Target: target}
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply store migration: %w", err)
	}
	current, target, err = provider.GetVersions(ctx)
	if err != nil {
		return fmt.Errorf("verify store migration versions: %w", err)
	}
	if current != target {
		return fmt.Errorf("store migration incomplete: current version %d, target version %d", current, target)
	}
	if target == ControlSchemaVersion {
		var revision string
		if err := database.QueryRowContext(ctx, `SELECT revision FROM pm.control_schema WHERE singleton`).Scan(&revision); err != nil {
			return fmt.Errorf("read control schema revision: %w", err)
		}
		if revision != ControlSchemaRevision {
			return errors.New("control schema revision mismatch")
		}
	}
	return nil
}

func verifyMigratorIdentity(ctx context.Context, database *sql.DB) error {
	var databaseName, role string
	var superuser, inherit, createRole, createDB, bypassRLS, replication, runtimeMember, databaseOwner, schemaCreate, schemaOwner bool
	err := database.QueryRowContext(ctx, `
		SELECT current_database(), current_user,
		       roles.rolsuper, roles.rolinherit, roles.rolcreaterole, roles.rolcreatedb,
		       roles.rolbypassrls, roles.rolreplication,
		       pg_has_role(current_user, 'pm_app_runtime', 'MEMBER'),
		       pg_get_userbyid(database_catalog.datdba) = current_user,
		       has_schema_privilege(current_user, 'pm', 'CREATE'),
		       pg_get_userbyid(namespace.nspowner) = current_user
		FROM pg_roles AS roles
		CROSS JOIN pg_namespace AS namespace
		CROSS JOIN pg_database AS database_catalog
		WHERE roles.rolname = current_user
		  AND namespace.nspname = 'pm'
		  AND database_catalog.datname = current_database()
	`).Scan(
		&databaseName, &role,
		&superuser, &inherit, &createRole, &createDB, &bypassRLS, &replication,
		&runtimeMember, &databaseOwner, &schemaCreate, &schemaOwner,
	)
	if err != nil {
		return fmt.Errorf("verify migrator identity: %w", err)
	}
	if databaseName != ApplicationDatabase || role != MigratorRole {
		return errors.New("migrator database or role mismatch")
	}
	if superuser || inherit || createRole || createDB || bypassRLS || replication || runtimeMember || !databaseOwner || !schemaCreate || !schemaOwner {
		return errors.New("migrator role has prohibited PostgreSQL authority")
	}
	return nil
}
