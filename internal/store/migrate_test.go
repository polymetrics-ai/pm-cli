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
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/pressly/goose/v3"
)

func TestEventWatermarkMigrationRetainedData(t *testing.T) {
	credentials := loadTestStoreCredentials(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := Migrate(ctx, credentials.migratorDSN); err != nil {
		t.Fatalf("establish current schema before event watermark migration regression: %v", err)
	}

	t.Run("retained_v10_events_backfill_every_scope_and_continue_sequence", func(t *testing.T) {
		scopes := []contract.Scope{newTestScope(t), newTestScope(t)}
		t.Cleanup(func() {
			restoreEventWatermarkMigrationFixture(t, credentials, scopes...)
		})
		pool := openEventWatermarkMigrationPool(t, ctx, credentials.runtimeDSN)
		runIDs := make([]string, len(scopes))
		for index, scope := range scopes {
			request := testAdmissionRequest(
				t,
				scope,
				"event-watermark-retained-"+randomHex(t, 8),
				fmt.Sprintf("event-watermark-retained-%d", index),
			)
			admitForTest(t, pool, scope, request)
			runIDs[index] = request.Spec.RunID
		}
		pool.Close()

		if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, 12); err != nil {
			t.Fatalf("return owned retained-event fixture to v11: %v", err)
		}
		if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, 11); err != nil {
			t.Fatalf("return owned retained-event fixture to v10: %v", err)
		}
		if err := Migrate(ctx, credentials.migratorDSN); err != nil {
			t.Fatalf("migrate retained cross-scope events from v10 to current schema: %v", err)
		}

		reopened := openEventWatermarkMigrationPool(t, ctx, credentials.runtimeDSN)
		defer reopened.Close()
		backfilled := true
		for index, scope := range scopes {
			var watermark uint64
			err := WithinScope(ctx, reopened, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
				return tx.queryRow(ctx, `
					SELECT last_event_sequence
					FROM pm.runs
					WHERE run_id = $1
				`, runIDs[index]).Scan(&watermark)
			})
			if err != nil {
				t.Fatalf("read backfilled event watermark for scope %d: %v", index, err)
			}
			if watermark != 1 {
				t.Errorf("backfilled event watermark for scope %d = %d; want retained maximum 1", index, watermark)
				backfilled = false
			}
		}
		if !backfilled {
			return
		}
		for index, scope := range scopes {
			var sequence, watermark uint64
			err := WithinScope(ctx, reopened, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
				committedAt, err := readDatabaseTime(ctx, tx)
				if err != nil {
					return err
				}
				event, err := insertEvent(
					ctx,
					tx,
					runIDs[index],
					1,
					testEvent("event-watermark-next-"+randomHex(t, 8), "migration_event", runIDs[index]),
					committedAt,
				)
				if err != nil {
					return err
				}
				sequence = event.Sequence
				return tx.queryRow(ctx, `
					SELECT last_event_sequence
					FROM pm.runs
					WHERE run_id = $1
				`, runIDs[index]).Scan(&watermark)
			})
			if err != nil {
				t.Fatalf("append after retained event migration for scope %d: %v", index, err)
			}
			if sequence != 2 || watermark != 2 {
				t.Fatalf(
					"post-migration event state for scope %d = sequence %d watermark %d; want 2/2",
					index,
					sequence,
					watermark,
				)
			}
		}
	})

	t.Run("downgrade_refuses_a_retained_tail_gap_visible_only_through_rls", func(t *testing.T) {
		scope := newTestScope(t)
		t.Cleanup(func() {
			restoreEventWatermarkMigrationFixture(t, credentials, scope)
		})
		pool := openEventWatermarkMigrationPool(t, ctx, credentials.runtimeDSN)
		request := testAdmissionRequest(
			t,
			scope,
			"event-watermark-downgrade-"+randomHex(t, 8),
			"event-watermark-downgrade",
		)
		admitForTest(t, pool, scope, request)

		var appendedSequence, watermark, retainedMaximum uint64
		err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			committedAt, err := readDatabaseTime(ctx, tx)
			if err != nil {
				return err
			}
			event, err := insertEvent(
				ctx,
				tx,
				request.Spec.RunID,
				1,
				testEvent("event-watermark-retained-tail-"+randomHex(t, 8), "migration_event", request.Spec.RunID),
				committedAt,
			)
			if err != nil {
				return err
			}
			appendedSequence = event.Sequence
			return nil
		})
		if err != nil {
			t.Fatalf("append owned retained-tail event: %v", err)
		}
		migrator, err := pgx.Connect(ctx, credentials.migratorDSN)
		if err != nil {
			t.Fatal("connect migrator for owned retention fixture")
		}
		defer func() {
			cleanupCtx, cleanupCancel := detachedCleanupContext()
			defer cleanupCancel()
			if err := migrator.Close(cleanupCtx); err != nil {
				t.Errorf("close owned retention fixture: %T", err)
			}
		}()
		if err := pgx.BeginFunc(ctx, migrator, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `
				SELECT set_config('pm.org_id', $1, true), set_config('pm.workspace_id', $2, true)
			`, scope.OrgID, scope.WorkspaceID); err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `
				DELETE FROM pm.server_events
				WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND sequence = $4
			`, scope.OrgID, scope.WorkspaceID, request.Spec.RunID, appendedSequence)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("owned retention fixture deleted %d events", tag.RowsAffected())
			}
			return tx.QueryRow(ctx, `
				SELECT run.last_event_sequence, COALESCE(MAX(event.sequence), 0)
				FROM pm.runs AS run
				LEFT JOIN pm.server_events AS event
				  ON event.org_id = run.org_id AND event.workspace_id = run.workspace_id
				     AND event.run_id = run.run_id
				WHERE run.org_id = $1 AND run.workspace_id = $2 AND run.run_id = $3
				GROUP BY run.last_event_sequence
			`, scope.OrgID, scope.WorkspaceID, request.Spec.RunID).Scan(&watermark, &retainedMaximum)
		}); err != nil {
			t.Fatalf("create owned retained-tail event gap: %v", err)
		}
		if appendedSequence != 2 || watermark != 2 || retainedMaximum != 1 {
			t.Fatalf(
				"retained-tail fixture = sequence %d watermark %d maximum %d; want 2/2/1",
				appendedSequence,
				watermark,
				retainedMaximum,
			)
		}
		pool.Close()

		if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, 12); err != nil {
			t.Fatalf("return retained-tail fixture to v11: %v", err)
		}
		err = migrateOneStoreVersionDown(ctx, credentials.migratorDSN, 11)
		if err == nil {
			t.Fatal("event watermark downgrade succeeded after retained tail deletion")
		}
		var databaseError *pgconn.PgError
		if !errors.As(err, &databaseError) || databaseError.Code != "P0001" {
			t.Fatalf("retained-tail downgrade error = %v; want migration refusal P0001", err)
		}

		conn, err := pgx.Connect(ctx, credentials.migratorDSN)
		if err != nil {
			t.Fatal("connect after retained-tail downgrade refusal")
		}
		defer func() { _ = conn.Close(context.Background()) }()
		var revision string
		if err := conn.QueryRow(ctx, `SELECT revision FROM pm.control_schema WHERE singleton`).Scan(&revision); err != nil {
			t.Fatal("read control schema after retained-tail downgrade refusal")
		}
		if revision != "pm.control/v11" {
			t.Fatalf("control revision after retained-tail downgrade refusal = %q; want %q", revision, "pm.control/v11")
		}
	})
}

func TestArtifactAuthorityMigrationRetainedData(t *testing.T) {
	credentials, pool := prepareTestStore(t, 3)
	scope := newTestScope(t)
	t.Cleanup(func() {
		restoreEventWatermarkMigrationFixture(t, credentials, scope)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	connectionID := registerArtifactTestConnections(t, ctx, pool, scope, "migration")[0]
	ref := testObjectRef(scope, "artifact-migration-authority", "attempt-migration-authority", "migration")
	verified, err := NewPreverifiedObjectRef(ref, ObjectVerification{
		ObservedSHA256:            ref.SHA256,
		ObservedByteLength:        ref.ByteLength,
		ObservedBackendGeneration: ref.BackendGeneration,
	})
	if err != nil {
		t.Fatalf("construct migration artifact authority: %v", err)
	}
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return RegisterArtifact(ctx, tx, connectionID, verified)
	}); err != nil {
		t.Fatalf("register retained artifact authority: %v", err)
	}
	pool.Close()

	err = migrateOneStoreVersionDown(ctx, credentials.migratorDSN, 12)
	if err == nil {
		t.Fatal("artifact authority downgrade succeeded with a retained binding")
	}
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) || databaseError.Code != "P0001" {
		t.Fatalf("retained artifact authority downgrade error = %v; want migration refusal P0001", err)
	}

	conn, err := pgx.Connect(ctx, credentials.migratorDSN)
	if err != nil {
		t.Fatal("connect after retained artifact authority downgrade refusal")
	}
	var revision string
	if err := conn.QueryRow(ctx, `SELECT revision FROM pm.control_schema WHERE singleton`).Scan(&revision); err != nil {
		_ = conn.Close(context.Background())
		t.Fatal("read control schema after retained artifact authority downgrade refusal")
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatal("close migrator after retained artifact authority downgrade refusal")
	}
	if revision != ControlSchemaRevision {
		t.Fatalf("control revision after retained artifact authority downgrade refusal = %q; want %q", revision, ControlSchemaRevision)
	}

	reopened := openEventWatermarkMigrationPool(t, ctx, credentials.runtimeDSN)
	defer reopened.Close()
	if err := WithinScope(ctx, reopened, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		storedConnectionID, err := LoadArtifactConnectionID(ctx, tx, ref.ArtifactID)
		if err != nil {
			return err
		}
		if storedConnectionID != connectionID {
			t.Fatalf("retained artifact authority = %q; want %q", storedConnectionID, connectionID)
		}
		return nil
	}); err != nil {
		t.Fatalf("load retained artifact authority after downgrade refusal: %v", err)
	}
}

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

func openEventWatermarkMigrationPool(t *testing.T, ctx context.Context, connectionString string) *Pool {
	t.Helper()
	pool, err := OpenPool(ctx, PoolConfig{
		ConnectionString:  connectionString,
		MaxConns:          4,
		MinConns:          0,
		MaxConnLifetime:   5 * time.Minute,
		MaxConnIdleTime:   time.Minute,
		HealthCheckPeriod: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("open event watermark migration pool: %v", err)
	}
	return pool
}

func restoreEventWatermarkMigrationFixture(
	t *testing.T,
	credentials testStoreCredentials,
	scopes ...contract.Scope,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Migrate(ctx, credentials.migratorDSN); err != nil {
		t.Errorf("restore current schema after event watermark migration regression: %v", err)
		return
	}
	cleanupTestScope(t, credentials, scopes...)
}

func migrateOneStoreVersionDown(ctx context.Context, connectionString string, expectedCurrent int64) error {
	configuration, err := pgx.ParseConfig(connectionString)
	if err != nil {
		return fmt.Errorf("parse migration connection: %w", err)
	}
	configuration.DefaultQueryExecMode = pgx.QueryExecModeExec
	if configuration.RuntimeParams == nil {
		configuration.RuntimeParams = make(map[string]string)
	}
	configuration.RuntimeParams["timezone"] = "UTC"
	configuration.RuntimeParams["search_path"] = "pg_catalog,pm"

	database := stdlib.OpenDB(*configuration)
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	defer database.Close()
	if err := verifyMigratorIdentity(ctx, database); err != nil {
		return err
	}
	rooted, err := fs.Sub(migrationFiles, "migrations/postgres")
	if err != nil {
		return fmt.Errorf("open store migrations: %w", err)
	}
	provider, err := goose.NewProvider(
		goose.DialectPostgres,
		database,
		rooted,
		goose.WithTableName(migrationTableName),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		return fmt.Errorf("create store migration provider: %w", err)
	}
	current, _, err := provider.GetVersions(ctx)
	if err != nil {
		return fmt.Errorf("read store migration version: %w", err)
	}
	if current != expectedCurrent {
		return fmt.Errorf("store migration current version = %d; want %d", current, expectedCurrent)
	}
	if _, err := provider.Down(ctx); err != nil {
		return fmt.Errorf("apply store downgrade from version %d: %w", expectedCurrent, err)
	}
	return nil
}
