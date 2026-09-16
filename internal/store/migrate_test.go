package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestMigrationRecovery(t *testing.T) {
	credentials := loadTestStoreCredentials(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	if err := Migrate(ctx, credentials.migratorDSN); err != nil {
		t.Fatalf("initial migration: %v", err)
	}
	if err := Migrate(ctx, credentials.migratorDSN); err != nil {
		t.Fatalf("reopen migrated store: %v", err)
	}

	t.Run("interrupted_transaction_reopens_at_last_committed_schema", func(t *testing.T) {
		conn, err := pgx.Connect(ctx, credentials.migratorDSN)
		if err != nil {
			t.Fatal("connect before interrupted migration")
		}
		defer func() { _ = conn.Close(context.Background()) }()
		var originalInstalledAt time.Time
		if err := conn.QueryRow(ctx, `SELECT installed_at FROM pm.control_schema WHERE singleton`).Scan(&originalInstalledAt); err != nil {
			t.Fatal("read marker before interrupted migration")
		}
		defer func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cleanupCancel()
			if _, err := conn.Exec(cleanupCtx, `UPDATE pm.control_schema SET installed_at = $1 WHERE singleton AND installed_at = $2`, originalInstalledAt, originalInstalledAt.Add(24*time.Hour)); err != nil {
				t.Errorf("restore the owned migration fault if it escaped rollback: %v", err)
			}
		}()
		productionMigrations, err := fs.Sub(migrationFiles, "migrations/postgres")
		if err != nil {
			t.Fatal("open embedded production migrations")
		}
		migrationNames, err := fs.Glob(productionMigrations, "*.sql")
		if err != nil {
			t.Fatal("enumerate embedded production migrations")
		}
		interrupted := make(fstest.MapFS, len(migrationNames)+1)
		for _, name := range migrationNames {
			data, err := fs.ReadFile(productionMigrations, name)
			if err != nil {
				t.Fatal("read embedded production migration")
			}
			interrupted[name] = &fstest.MapFile{Data: data}
		}
		interrupted[fmt.Sprintf("%04d_interrupted.sql", ControlSchemaVersion+1)] = &fstest.MapFile{
			Data: []byte("-- +goose Up\nUPDATE pm.control_schema SET installed_at = installed_at + interval '1 day';\nSELECT 1 / 0;\n"),
		}
		err = migrateWithFiles(ctx, credentials.migratorDSN, interrupted)
		var interruptedFailure *pgconn.PgError
		if !errors.As(err, &interruptedFailure) || interruptedFailure.Code != "22012" {
			t.Fatalf("migration must reach the deliberate interruption after its write: %v", err)
		}
		var revision string
		var migrationVersion int64
		var installedAt time.Time
		if err := conn.QueryRow(ctx, `SELECT revision, installed_at FROM pm.control_schema WHERE singleton`).Scan(&revision, &installedAt); err != nil {
			t.Fatal("read control schema after interrupted migration")
		}
		if err := conn.QueryRow(ctx, `SELECT COALESCE(max(version_id) FILTER (WHERE is_applied), 0) FROM pm.control_schema_migrations`).Scan(&migrationVersion); err != nil {
			t.Fatal("read migration version after interrupted migration")
		}
		if !installedAt.Equal(originalInstalledAt) {
			t.Fatalf("interrupted migration committed its preceding marker write: before=%s after=%s", originalInstalledAt, installedAt)
		}
		if revision != ControlSchemaRevision || migrationVersion != ControlSchemaVersion {
			t.Fatalf("reopened schema = revision %q version %d; want %q/%d", revision, migrationVersion, ControlSchemaRevision, ControlSchemaVersion)
		}
		if err := Migrate(ctx, credentials.migratorDSN); err != nil {
			t.Fatalf("reopen production migration set: %v", err)
		}
	})

	t.Run("unsupported_downgrade_is_refused_without_reversal", func(t *testing.T) {
		conn, err := pgx.Connect(ctx, credentials.migratorDSN)
		if err != nil {
			t.Fatal("connect for downgrade control")
		}
		defer func() { _ = conn.Close(context.Background()) }()
		var syntheticID int64
		if err := conn.QueryRow(ctx, `INSERT INTO pm.control_schema_migrations (version_id, is_applied) VALUES ($1, true) RETURNING id`, ControlSchemaVersion+1).Scan(&syntheticID); err != nil {
			t.Fatal("install isolated future-version control")
		}
		removed := false
		defer func() {
			if removed {
				return
			}
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cleanupCancel()
			if _, err := conn.Exec(cleanupCtx, `DELETE FROM pm.control_schema_migrations WHERE id = $1 AND version_id = $2`, syntheticID, ControlSchemaVersion+1); err != nil {
				t.Errorf("remove owned synthetic migration version during cleanup: %T", err)
			}
		}()

		err = Migrate(ctx, credentials.migratorDSN)
		var downgrade *UnsupportedDowngradeError
		if !errors.As(err, &downgrade) {
			t.Fatalf("Migrate future schema error = %v; want UnsupportedDowngradeError", err)
		}
		var revision string
		if err := conn.QueryRow(ctx, `SELECT revision FROM pm.control_schema WHERE singleton`).Scan(&revision); err != nil {
			t.Fatal("read schema after downgrade refusal")
		}
		if revision != ControlSchemaRevision {
			t.Fatalf("schema revision after downgrade refusal = %q; want %q", revision, ControlSchemaRevision)
		}
		if _, err := conn.Exec(ctx, `DELETE FROM pm.control_schema_migrations WHERE id = $1 AND version_id = $2`, syntheticID, ControlSchemaVersion+1); err != nil {
			t.Fatal("remove isolated future-version control")
		}
		removed = true
		if err := Migrate(ctx, credentials.migratorDSN); err != nil {
			t.Fatalf("reopen after restoring supported version: %v", err)
		}
	})
}
