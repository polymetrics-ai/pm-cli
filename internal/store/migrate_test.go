package store

import (
	"context"
	"encoding/json"
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

func TestIdentityArraysUseCanonicalByteOrder(t *testing.T) {
	credentials := loadTestStoreCredentials(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Migrate(ctx, credentials.migratorDSN); err != nil {
		t.Fatalf("establish identity ordering schema: %T", err)
	}
	connection, err := pgx.Connect(ctx, credentials.migratorDSN)
	if err != nil {
		t.Fatalf("connect identity ordering fixture: %T", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := detachedCleanupContext()
		defer cleanupCancel()
		if err := connection.Close(cleanupCtx); err != nil {
			t.Errorf("close identity ordering fixture: %T", err)
		}
	}()

	var canonicalC, canonicalEnglish, reversedEnglish bool
	err = connection.QueryRow(ctx, `
		SELECT pm.text_array_is_strictly_sorted(ARRAY['Z', 'a'] COLLATE "C"),
		       pm.text_array_is_strictly_sorted(ARRAY['Z', 'a'] COLLATE "en-US-x-icu"),
		       pm.text_array_is_strictly_sorted(ARRAY['a', 'Z'] COLLATE "en-US-x-icu")
	`).Scan(&canonicalC, &canonicalEnglish, &reversedEnglish)
	if err != nil {
		t.Fatalf("evaluate identity ordering under explicit collations: %T", err)
	}
	if !canonicalC || !canonicalEnglish || reversedEnglish {
		t.Fatalf(
			"identity byte ordering: canonical C=%t English=%t, reversed English=%t; want true/true/false",
			canonicalC, canonicalEnglish, reversedEnglish,
		)
	}
}

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

		for version := ControlSchemaVersion; version > 12; version-- {
			if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, version); err != nil {
				t.Fatalf("return owned retained-event fixture to v12: %v", err)
			}
		}
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

		for version := ControlSchemaVersion; version > 12; version-- {
			if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, version); err != nil {
				t.Fatalf("return retained-tail fixture to v12: %v", err)
			}
		}
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

	for version := ControlSchemaVersion; version > 12; version-- {
		if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, version); err != nil {
			t.Fatalf("return retained artifact fixture to v12: %v", err)
		}
	}
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
	if revision != "pm.control/v12" {
		t.Fatalf("control revision after retained artifact authority downgrade refusal = %q; want v12", revision)
	}

	if err := Migrate(ctx, credentials.migratorDSN); err != nil {
		t.Fatalf("restore current schema after artifact downgrade refusal: %v", err)
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

func TestAccountCooldownMigrationRetainedData(t *testing.T) {
	credentials, pool := prepareTestStore(t, 3)
	fixture := registerPermitTestFixture(t, credentials, pool, permitFixtureOptions{Capacity: 2})
	t.Cleanup(func() { restoreEventWatermarkMigrationFixture(t, credentials) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	source, err := reservePermitForTest(ctx, pool, fixture.alpha, AccountPermitRequest{
		PermitID: "migration-source-" + fixture.suffix, ConnectionID: fixture.alphaConnections[0],
		OwnerID: "source-worker", Units: 1, TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("reserve migration cooldown source: %v", err)
	}
	source, err = authorizePermitForTest(ctx, pool, source)
	if err != nil {
		t.Fatalf("dispatch migration cooldown source: %v", err)
	}
	waiting, err := reservePermitForTest(ctx, pool, fixture.beta, AccountPermitRequest{
		PermitID: "migration-waiting-" + fixture.suffix, ConnectionID: fixture.betaConnection,
		OwnerID: "waiting-worker", Units: 1, TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("reserve pre-cooldown migration permit: %v", err)
	}
	until := source.ReservedAt.Add(time.Hour)
	if err := WithinScope(ctx, pool, fixture.alpha, testActor(fixture.alpha), func(ctx context.Context, tx *Transaction) error {
		return RecordAccountCooldown(ctx, tx, source, until)
	}); err != nil {
		t.Fatalf("retain migration cooldown: %v", err)
	}
	pool.Close()
	for version := ControlSchemaVersion; version > 13; version-- {
		if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, version); err != nil {
			t.Fatalf("return retained cooldown fixture to v13: %v", err)
		}
	}
	err = migrateOneStoreVersionDown(ctx, credentials.migratorDSN, 13)
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) || databaseError.Code != "P0001" {
		t.Fatalf("retained cooldown downgrade = %v; want transactional refusal", err)
	}
	if err := Migrate(ctx, credentials.migratorDSN); err != nil {
		t.Fatalf("restore current schema after cooldown downgrade refusal: %v", err)
	}
	reopened := openPermitTestPool(t, credentials, 3)
	_, err = authorizePermitForTest(ctx, reopened, waiting)
	var cooldown *AccountCooldownError
	if !errors.As(err, &cooldown) || !cooldown.NotBefore.Equal(until) {
		t.Fatalf("retained cooldown after refused downgrade = %v; want original deadline", err)
	}
}

func TestObjectPublicationMigrationRetainedData(t *testing.T) {
	credentials, pool := prepareTestStore(t, 3)
	fixture := registerPermitTestFixture(t, credentials, pool, permitFixtureOptions{Capacity: 2})
	t.Cleanup(func() { restoreEventWatermarkMigrationFixture(t, credentials) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	request := ObjectReservationRequest{
		ReservationID: "migration-object-" + fixture.suffix,
		ConnectionID:  fixture.alphaConnections[0],
		ArtifactID:    "migration-artifact-" + fixture.suffix,
		Purpose:       ArtifactPurposePublic,
		MaximumBytes:  8,
	}
	limits := ObjectCapacityLimits{OrganizationBytes: 16, WorkspaceBytes: 16, OrganizationAttempts: 2, WorkspaceAttempts: 2}
	var original ObjectReservation
	if err := WithinScope(ctx, pool, fixture.alpha, testActor(fixture.alpha), func(ctx context.Context, tx *Transaction) error {
		var err error
		original, err = ReserveObjectCapacity(ctx, tx, request, limits)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	pool.Close()
	for pass := range 2 {
		for version := ControlSchemaVersion; version > 14; version-- {
			if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, version); err != nil {
				t.Fatalf("return retained object fixture to v14: %v", err)
			}
		}
		err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, 14)
		var databaseError *pgconn.PgError
		if !errors.As(err, &databaseError) || databaseError.Code != "P0001" {
			t.Fatalf("retained object downgrade = %v; want transactional refusal", err)
		}
		if err := Migrate(ctx, credentials.migratorDSN); err != nil {
			t.Fatalf("restore schema after object downgrade refusal: %v", err)
		}
		reopened := openPermitTestPool(t, credentials, 3)
		if err := WithinScope(ctx, reopened, fixture.alpha, testActor(fixture.alpha), func(ctx context.Context, tx *Transaction) error {
			retained, err := LoadObjectReservation(ctx, tx, request.ReservationID)
			if err != nil {
				return err
			}
			wantState := ObjectReservationHeld
			if pass == 1 {
				wantState = ObjectReservationReleased
			}
			if retained.Request != original.Request || !retained.ReservedAt.Equal(original.ReservedAt) || retained.State != wantState {
				return errors.New("refused downgrade changed retained reservation history")
			}
			if pass == 0 {
				return CancelObjectReservation(ctx, tx, request.ReservationID)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		reopened.Close()
	}
}

func TestHTTPBudgetMigrationRetainedData(t *testing.T) {
	credentials, pool := prepareTestStore(t, 3)
	httpScope, plainScope := newTestScope(t), newTestScope(t)
	t.Cleanup(func() { restoreEventWatermarkMigrationFixture(t, credentials, httpScope, plainScope) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	httpRequest := testAdmissionRequest(t, httpScope, "http-budget-migration", "http-budget-migration")
	policy := httpRequest.BudgetSnapshot.Policy
	policy.HTTP = &contract.HTTPExecutionPolicy{
		MaxRequestBytes: 1024, MaxResponseBytes: 4096, MaxDNSAnswers: 8,
		MaxAttempts: 3, MaxRetryDelay: time.Second, SelectedOrigin: "https://api.example.test/v1",
	}
	setAdmissionPolicyForTest(t, &httpRequest, policy)
	admitForTest(t, pool, httpScope, httpRequest)
	plainRequest := testAdmissionRequest(t, plainScope, "plain-budget-migration", "plain-budget-migration")
	admitForTest(t, pool, plainScope, plainRequest)
	pool.Close()
	for version := ControlSchemaVersion; version > 15; version-- {
		if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, version); err != nil {
			t.Fatalf("return retained HTTP budget fixture to v15: %v", err)
		}
	}
	err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, 15)
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) || databaseError.Code != "P0001" {
		t.Fatalf("retained HTTP budget downgrade = %v; want transactional refusal", err)
	}
	if err := Migrate(ctx, credentials.migratorDSN); err != nil {
		t.Fatal(err)
	}
	reopened := openPermitTestPool(t, credentials, 3)
	if err := WithinScope(ctx, reopened, httpScope, testActor(httpScope), func(ctx context.Context, tx *Transaction) error {
		retained, err := LoadRunBudgetSnapshot(ctx, tx, httpRequest.Spec.RunID)
		if err != nil {
			return err
		}
		if retained.Binding() != httpRequest.Spec.Budget || retained.Policy.HTTP == nil || *retained.Policy.HTTP != *policy.HTTP {
			return errors.New("refused downgrade changed the accepted HTTP authority")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reopened.Close()

	// Legacy absent-policy snapshots survive a real down/up round trip once no
	// retained HTTP authority would be destroyed by the older closed shape.
	cleanupTestScope(t, credentials, httpScope)
	for version := ControlSchemaVersion; version > 15; version-- {
		if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, version); err != nil {
			t.Fatalf("return legacy HTTP budget fixture to v15: %v", err)
		}
	}
	if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, 15); err != nil {
		t.Fatalf("downgrade with only legacy budgets: %v", err)
	}
	if err := Migrate(ctx, credentials.migratorDSN); err != nil {
		t.Fatalf("restore HTTP budget schema: %v", err)
	}
	reopened = openPermitTestPool(t, credentials, 3)
	defer reopened.Close()
	if err := WithinScope(ctx, reopened, plainScope, testActor(plainScope), func(ctx context.Context, tx *Transaction) error {
		retained, err := LoadRunBudgetSnapshot(ctx, tx, plainRequest.Spec.RunID)
		if err != nil {
			return err
		}
		if retained.Binding() != plainRequest.Spec.Budget || retained.Policy.HTTP != nil {
			return errors.New("migration changed the legacy absent-policy budget")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactPurposeMigrationRetainedData(t *testing.T) {
	credentials := loadTestStoreCredentials(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := Migrate(ctx, credentials.migratorDSN); err != nil {
		t.Fatalf("establish artifact purpose schema: %v", err)
	}
	preverify := func(t *testing.T, ref contract.ObjectRef) PreverifiedObjectRef {
		t.Helper()
		verified, err := NewPreverifiedObjectRef(ref, ObjectVerification{
			ObservedSHA256:            ref.SHA256,
			ObservedByteLength:        ref.ByteLength,
			ObservedBackendGeneration: ref.BackendGeneration,
		})
		if err != nil {
			t.Fatalf("construct preverified artifact %q: %v", ref.ArtifactID, err)
		}
		return verified
	}

	t.Run("public_row_survives_v15_upgrade_as_public", func(t *testing.T) {
		scope := newTestScope(t)
		t.Cleanup(func() { restoreEventWatermarkMigrationFixture(t, credentials, scope) })
		pool := openEventWatermarkMigrationPool(t, ctx, credentials.runtimeDSN)
		connectionID := registerArtifactTestConnections(t, ctx, pool, scope, "purpose-upgrade")[0]
		ref := testObjectRef(scope, "artifact-purpose-upgrade", "attempt-purpose-upgrade", "purpose-upgrade")
		verified := preverify(t, ref)
		if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			return RegisterArtifact(ctx, tx, connectionID, verified)
		}); err != nil {
			t.Fatalf("register public artifact before purpose round trip: %v", err)
		}
		pool.Close()
		for version := ControlSchemaVersion; version > 16; version-- {
			if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, version); err != nil {
				t.Fatalf("return public artifact fixture to v16: %v", err)
			}
		}

		err := updateArtifactPurposeForMigrationTest(ctx, credentials.migratorDSN, scope, ref.ArtifactID, "continuation")
		var databaseError *pgconn.PgError
		if !errors.As(err, &databaseError) || databaseError.Code != "P0001" {
			t.Fatalf("public artifact purpose mutation = %v; want immutable trigger refusal", err)
		}
		if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, 16); err != nil {
			t.Fatalf("return public artifact fixture to v15: %v", err)
		}
		if err := Migrate(ctx, credentials.migratorDSN); err != nil {
			t.Fatalf("upgrade retained public artifact to v16: %v", err)
		}

		reopened := openEventWatermarkMigrationPool(t, ctx, credentials.runtimeDSN)
		defer reopened.Close()
		if err := WithinScope(ctx, reopened, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			stored, err := LoadArtifact(ctx, tx, ref.ArtifactID)
			if err != nil {
				return err
			}
			if stored != ref {
				t.Fatalf("upgraded public artifact = %#v; want %#v", stored, ref)
			}
			storedConnectionID, err := LoadArtifactConnectionID(ctx, tx, ref.ArtifactID)
			if err != nil {
				return err
			}
			if storedConnectionID != connectionID {
				t.Fatalf("upgraded public artifact connection = %q; want %q", storedConnectionID, connectionID)
			}
			if _, err := LoadContinuationArtifact(ctx, tx, ref.ArtifactID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("upgraded public artifact continuation read = %v; want ErrNotFound", err)
			}
			return nil
		}); err != nil {
			t.Fatalf("load public artifact after v15/v16 round trip: %v", err)
		}
	})

	t.Run("downgrade_refuses_retained_continuation", func(t *testing.T) {
		scope := newTestScope(t)
		t.Cleanup(func() { restoreEventWatermarkMigrationFixture(t, credentials, scope) })
		pool := openEventWatermarkMigrationPool(t, ctx, credentials.runtimeDSN)
		connectionID := registerArtifactTestConnections(t, ctx, pool, scope, "purpose-downgrade")[0]
		ref := testObjectRef(scope, "artifact-purpose-downgrade", "attempt-purpose-downgrade", "purpose-downgrade")
		verified := preverify(t, ref)
		if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			return RegisterContinuationArtifact(ctx, tx, connectionID, verified)
		}); err != nil {
			t.Fatalf("register retained continuation artifact: %v", err)
		}
		pool.Close()
		for version := ControlSchemaVersion; version > 16; version-- {
			if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, version); err != nil {
				t.Fatalf("return continuation artifact fixture to v16: %v", err)
			}
		}

		err := updateArtifactPurposeForMigrationTest(ctx, credentials.migratorDSN, scope, ref.ArtifactID, "public")
		var databaseError *pgconn.PgError
		if !errors.As(err, &databaseError) || databaseError.Code != "P0001" {
			t.Fatalf("continuation artifact purpose mutation = %v; want immutable trigger refusal", err)
		}
		err = migrateOneStoreVersionDown(ctx, credentials.migratorDSN, 16)
		if !errors.As(err, &databaseError) || databaseError.Code != "P0001" {
			t.Fatalf("retained continuation downgrade = %v; want transactional refusal", err)
		}

		conn, err := pgx.Connect(ctx, credentials.migratorDSN)
		if err != nil {
			t.Fatal("connect after continuation downgrade refusal")
		}
		var revision string
		if err := conn.QueryRow(ctx, `SELECT revision FROM pm.control_schema WHERE singleton`).Scan(&revision); err != nil {
			_ = conn.Close(context.Background())
			t.Fatal("read control schema after continuation downgrade refusal")
		}
		if err := conn.Close(ctx); err != nil {
			t.Fatal("close migrator after continuation downgrade refusal")
		}
		if revision != "pm.control/v16" {
			t.Fatalf("control revision after continuation downgrade refusal = %q; want v16", revision)
		}
		if err := Migrate(ctx, credentials.migratorDSN); err != nil {
			t.Fatalf("restore current schema after refused purpose downgrade: %v", err)
		}

		reopened := openEventWatermarkMigrationPool(t, ctx, credentials.runtimeDSN)
		defer reopened.Close()
		if err := WithinScope(ctx, reopened, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			stored, err := LoadContinuationArtifact(ctx, tx, ref.ArtifactID)
			if err != nil {
				return err
			}
			if stored != ref {
				t.Fatalf("retained continuation artifact = %#v; want %#v", stored, ref)
			}
			if _, err := LoadArtifact(ctx, tx, ref.ArtifactID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("retained continuation public read = %v; want ErrNotFound", err)
			}
			if _, err := LoadArtifactConnectionID(ctx, tx, ref.ArtifactID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("retained continuation authority read = %v; want ErrNotFound", err)
			}
			return nil
		}); err != nil {
			t.Fatalf("load continuation after refused downgrade: %v", err)
		}
	})
}

func TestDurableContinuationMigrationRetainedData(t *testing.T) {
	credentials := loadTestStoreCredentials(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	limits := ObjectCapacityLimits{
		OrganizationBytes: 16, WorkspaceBytes: 16,
		OrganizationAttempts: 4, WorkspaceAttempts: 4,
	}

	t.Run("legacy_reservation_round_trip_backfills_public", func(t *testing.T) {
		if err := Migrate(ctx, credentials.migratorDSN); err != nil {
			t.Fatalf("establish durable continuation schema: %v", err)
		}
		pool := openPermitTestPool(t, credentials, 3)
		fixture := registerPermitTestFixture(t, credentials, pool, permitFixtureOptions{Capacity: 2})
		t.Cleanup(func() { restoreEventWatermarkMigrationFixture(t, credentials) })
		request := ObjectReservationRequest{
			ReservationID: "legacy-purpose-" + fixture.suffix,
			ConnectionID:  fixture.alphaConnections[0],
			ArtifactID:    "legacy-purpose-artifact-" + fixture.suffix,
			Purpose:       ArtifactPurposePublic,
			MaximumBytes:  8,
		}
		if err := WithinScope(ctx, pool, fixture.alpha, testActor(fixture.alpha), func(ctx context.Context, tx *Transaction) error {
			_, err := ReserveObjectCapacity(ctx, tx, request, limits)
			return err
		}); err != nil {
			t.Fatalf("reserve public migration fixture: %v", err)
		}
		pool.Close()
		for version := ControlSchemaVersion; version > 18; version-- {
			if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, version); err != nil {
				t.Fatalf("return retained fixture to the v18 shape: %v", err)
			}
		}
		if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, 18); err != nil {
			t.Fatalf("return public reservation to v17 retry shape: %v", err)
		}
		if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, 17); err != nil {
			t.Fatalf("return public reservation to legacy v16 shape: %v", err)
		}
		if err := Migrate(ctx, credentials.migratorDSN); err != nil {
			t.Fatalf("upgrade legacy reservation through v18: %v", err)
		}
		reopened := openPermitTestPool(t, credentials, 3)
		defer reopened.Close()
		if err := WithinScope(ctx, reopened, fixture.alpha, testActor(fixture.alpha), func(ctx context.Context, tx *Transaction) error {
			retained, err := LoadObjectReservation(ctx, tx, request.ReservationID)
			if err != nil {
				return err
			}
			if retained.Request != request || retained.Request.Purpose != ArtifactPurposePublic {
				t.Fatalf("upgraded legacy reservation = %#v; want explicit public request", retained.Request)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("downgrade_refuses_private_reservation", func(t *testing.T) {
		if err := Migrate(ctx, credentials.migratorDSN); err != nil {
			t.Fatalf("establish durable continuation schema: %v", err)
		}
		pool := openPermitTestPool(t, credentials, 3)
		fixture := registerPermitTestFixture(t, credentials, pool, permitFixtureOptions{Capacity: 2})
		t.Cleanup(func() { restoreEventWatermarkMigrationFixture(t, credentials) })
		request := ObjectReservationRequest{
			ReservationID: "private-purpose-" + fixture.suffix,
			ConnectionID:  fixture.alphaConnections[0],
			ArtifactID:    "private-purpose-artifact-" + fixture.suffix,
			Purpose:       ArtifactPurposeContinuation,
			MaximumBytes:  8,
		}
		if err := WithinScope(ctx, pool, fixture.alpha, testActor(fixture.alpha), func(ctx context.Context, tx *Transaction) error {
			_, err := ReserveObjectCapacity(ctx, tx, request, limits)
			return err
		}); err != nil {
			t.Fatalf("reserve private migration fixture: %v", err)
		}
		pool.Close()
		for version := ControlSchemaVersion; version > 18; version-- {
			if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, version); err != nil {
				t.Fatalf("return retained fixture to the v18 shape: %v", err)
			}
		}
		if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, 18); err != nil {
			t.Fatalf("return private reservation to v17 retry shape: %v", err)
		}
		err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, 17)
		var databaseError *pgconn.PgError
		if !errors.As(err, &databaseError) || databaseError.Code != "P0001" {
			t.Fatalf("retained private reservation downgrade = %v; want transactional refusal", err)
		}
		if err := Migrate(ctx, credentials.migratorDSN); err != nil {
			t.Fatalf("restore v18 after refused private reservation downgrade: %v", err)
		}
		reopened := openPermitTestPool(t, credentials, 3)
		defer reopened.Close()
		if err := WithinScope(ctx, reopened, fixture.alpha, testActor(fixture.alpha), func(ctx context.Context, tx *Transaction) error {
			retained, err := LoadObjectReservation(ctx, tx, request.ReservationID)
			if err != nil {
				return err
			}
			if retained.Request != request {
				t.Fatalf("refused downgrade changed private reservation = %#v", retained.Request)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("downgrade_refuses_released_unused_permit", func(t *testing.T) {
		if err := Migrate(ctx, credentials.migratorDSN); err != nil {
			t.Fatalf("establish durable continuation schema: %v", err)
		}
		pool := openPermitTestPool(t, credentials, 3)
		fixture := registerPermitTestFixture(t, credentials, pool, permitFixtureOptions{Capacity: 1})
		t.Cleanup(func() { restoreEventWatermarkMigrationFixture(t, credentials) })
		permit, err := reservePermitForTest(
			ctx,
			pool,
			fixture.alpha,
			AccountPermitRequest{
				PermitID:     "migration-release-" + fixture.suffix,
				ConnectionID: fixture.alphaConnections[0],
				OwnerID:      "migration-release-owner",
				Units:        1,
				TTL:          time.Minute,
			},
		)
		if err != nil {
			t.Fatalf("reserve migration release fixture: %v", err)
		}
		released, err := releaseUnusedPermitForTest(ctx, pool, permit)
		if err != nil || released.State != AccountPermitReleased {
			t.Fatalf("release migration permit = %#v, %v", released, err)
		}
		pool.Close()
		for version := ControlSchemaVersion; version > 18; version-- {
			if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, version); err != nil {
				t.Fatalf("return retained fixture to the v18 shape: %v", err)
			}
		}
		if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, 18); err != nil {
			t.Fatalf("return released permit to v17 retry shape: %v", err)
		}

		err = migrateOneStoreVersionDown(ctx, credentials.migratorDSN, 17)
		var databaseError *pgconn.PgError
		if !errors.As(err, &databaseError) || databaseError.Code != "P0001" {
			t.Fatalf("retained released permit downgrade = %v; want transactional refusal", err)
		}
		if err := Migrate(ctx, credentials.migratorDSN); err != nil {
			t.Fatalf("restore v18 after refused released permit downgrade: %v", err)
		}
		reopened := openPermitTestPool(t, credentials, 3)
		defer reopened.Close()
		retained, err := loadPermitForTest(ctx, reopened, fixture.alpha, permit.PermitID)
		if err != nil ||
			retained.State != AccountPermitReleased ||
			retained.ResolvedAt == nil ||
			readReservedUnits(t, reopened, fixture.alpha, fixture.account.AccountID) != 0 {
			t.Fatalf("refused downgrade changed released permit = %#v, %v", retained, err)
		}
	})

	t.Run("downgrade_refuses_cap_and_continuation_snapshot", func(t *testing.T) {
		if err := Migrate(ctx, credentials.migratorDSN); err != nil {
			t.Fatalf("establish durable continuation schema: %v", err)
		}
		pool := openPermitTestPool(t, credentials, 3)
		scope := newTestScope(t)
		t.Cleanup(func() { restoreEventWatermarkMigrationFixture(t, credentials, scope) })
		request := testAdmissionRequest(t, scope, "continuation-controls", "continuation-controls")
		cap := uint64(3)
		continuation := contract.NewStringValue("opaque-migration-continuation")
		request.InputSnapshot.ResultControls = contract.ResultControls{
			TotalRecordCap: &cap,
			Continuation:   &continuation,
		}
		registerRunViewFixture(t, pool, scope, request)
		absentRequest := testAdmissionRequest(
			t,
			scope,
			"absent-continuation-controls",
			"absent-continuation-controls",
		)
		absent := contract.AbsentValue()
		absentRequest.InputSnapshot.ResultControls = contract.ResultControls{
			Continuation: &absent,
		}
		registerRunViewFixture(t, pool, scope, absentRequest)
		pool.Close()

		for version := ControlSchemaVersion; version > 18; version-- {
			if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, version); err != nil {
				t.Fatalf("return retained fixture to the v18 shape: %v", err)
			}
		}
		if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, 18); err != nil {
			t.Fatalf("return continuation controls to v17 retry shape: %v", err)
		}
		err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, 17)
		var databaseError *pgconn.PgError
		if !errors.As(err, &databaseError) || databaseError.Code != "P0001" {
			t.Fatalf("retained continuation controls downgrade = %v; want transactional refusal", err)
		}
		if err := Migrate(ctx, credentials.migratorDSN); err != nil {
			t.Fatalf("restore v18 after refused continuation controls downgrade: %v", err)
		}
		reopened := openPermitTestPool(t, credentials, 3)
		defer reopened.Close()
		if err := WithinScope(ctx, reopened, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			retained, err := LoadRunInputSnapshot(ctx, tx, request.Spec.RunID)
			if err != nil {
				return err
			}
			retainedContinuation := retained.ResultControls.ContinuationValue()
			opaque, text := retainedContinuation.TextValue()
			if retained.ResultControls.TotalRecordCap == nil ||
				*retained.ResultControls.TotalRecordCap != cap ||
				!retained.ResultControls.HasContinuation() ||
				!text ||
				opaque != "opaque-migration-continuation" {
				t.Fatalf("refused downgrade changed retained continuation controls = %#v", retained.ResultControls)
			}
			retainedAbsent, err := LoadRunInputSnapshot(
				ctx,
				tx,
				absentRequest.Spec.RunID,
			)
			if err != nil {
				return err
			}
			if retainedAbsent.ResultControls.Continuation == nil ||
				retainedAbsent.ResultControls.HasContinuation() ||
				!retainedAbsent.ResultControls.ContinuationValue().Equal(contract.AbsentValue()) {
				t.Fatalf("refused downgrade changed explicit absent continuation = %#v", retainedAbsent.ResultControls)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestWorkerRetryMigrationRetainedData(t *testing.T) {
	t.Run("legacy_root_round_trip_backfills_attempt_one", func(t *testing.T) {
		fixture := newWorkerRetryStoreFixture(t, 2)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		fixture.pool.Close()
		for version := ControlSchemaVersion; version > 18; version-- {
			if err := migrateOneStoreVersionDown(ctx, fixture.credentials.migratorDSN, version); err != nil {
				t.Fatalf("return retained retry fixture to the v18 shape: %v", err)
			}
		}
		if err := migrateOneStoreVersionDown(ctx, fixture.credentials.migratorDSN, 18); err != nil {
			t.Fatalf("return retry schema to legacy v17 shape: %v", err)
		}
		if err := Migrate(ctx, fixture.credentials.migratorDSN); err != nil {
			t.Fatalf("upgrade legacy dispatch root to v18: %v", err)
		}
		reopened := openPermitTestPool(t, fixture.credentials, 3)
		defer reopened.Close()
		if err := WithinScope(ctx, reopened, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
			root, err := LoadWorkerDispatch(ctx, tx, fixture.lease.Task)
			if err != nil {
				return err
			}
			if root.Intent.DispatchID != fixture.root.DispatchID ||
				root.AttemptNumber != 1 ||
				root.Retry != nil {
				t.Fatalf("upgraded legacy dispatch root = %#v", root)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("downgrade_refuses_retained_retry_lineage", func(t *testing.T) {
		fixture := newWorkerRetryStoreFixture(t, 2)
		commit := fixture.retrySettlement(
			t,
			"migration-retained",
			contract.OutcomeRejected,
			WorkerRetrySourceDeclaredSafe,
			0,
		)
		leaf := fixture.commitRetry(t, commit)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		fixture.pool.Close()
		for version := ControlSchemaVersion; version > 18; version-- {
			if err := migrateOneStoreVersionDown(ctx, fixture.credentials.migratorDSN, version); err != nil {
				t.Fatalf("return retained retry fixture to the v18 shape: %v", err)
			}
		}
		err := migrateOneStoreVersionDown(ctx, fixture.credentials.migratorDSN, 18)
		var databaseError *pgconn.PgError
		if !errors.As(err, &databaseError) || databaseError.Code != "P0001" {
			t.Fatalf("retained retry downgrade = %v; want transactional refusal", err)
		}
		if err := Migrate(ctx, fixture.credentials.migratorDSN); err != nil {
			t.Fatalf("restore the current schema after refused retry lineage downgrade: %v", err)
		}
		reopened := openPermitTestPool(t, fixture.credentials, 3)
		defer reopened.Close()
		if err := WithinScope(ctx, reopened, fixture.scope, testActor(fixture.scope), func(ctx context.Context, tx *Transaction) error {
			stored, err := LoadWorkerDispatch(ctx, tx, fixture.lease.Task)
			if err != nil {
				return err
			}
			if stored.Intent.DispatchID != leaf.Intent.DispatchID ||
				stored.AttemptNumber != 2 ||
				stored.Retry == nil ||
				stored.Retry.ParentDispatchID != fixture.root.DispatchID {
				t.Fatalf("refused downgrade changed retry lineage = %#v", stored)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
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

func updateArtifactPurposeForMigrationTest(
	ctx context.Context,
	connectionString string,
	scope contract.Scope,
	artifactID string,
	purpose string,
) error {
	connection, err := pgx.Connect(ctx, connectionString)
	if err != nil {
		return fmt.Errorf("connect artifact purpose migration fixture: %w", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	return pgx.BeginFunc(ctx, connection, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			SELECT set_config('pm.org_id', $1, true),
			       set_config('pm.workspace_id', $2, true)
		`, scope.OrgID, scope.WorkspaceID); err != nil {
			return fmt.Errorf("bind artifact purpose migration scope: %w", err)
		}
		tag, err := tx.Exec(ctx, `
			UPDATE pm.artifacts
			SET purpose = $1
			WHERE org_id = $2 AND workspace_id = $3 AND artifact_id = $4
		`, purpose, scope.OrgID, scope.WorkspaceID, artifactID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("artifact purpose migration fixture updated %d rows", tag.RowsAffected())
		}
		return nil
	})
}

// TestWarehouseSegmentMigrationRetainedData covers the warehouse segment
// migration: registered dataset membership survives a refused downgrade, and a
// downgrade that would erase it is refused transactionally.
func TestWarehouseSegmentMigrationRetainedData(t *testing.T) {
	credentials, pool := prepareTestStore(t, 3)
	fixture := registerPermitTestFixture(t, credentials, pool, permitFixtureOptions{Capacity: 2})
	t.Cleanup(func() {
		restoreEventWatermarkMigrationFixture(t, credentials, fixture.alpha, fixture.beta)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	scope := fixture.alpha
	connectionID := fixture.alphaConnections[0]
	datasetID := "wh1.fixture-connector.connection-primary.orders"
	artifactID := "warehouse-segment-" + fixture.suffix
	reservationID := "warehouse-segment-" + fixture.suffix
	objectAttemptKey := "pm/v1/warehouse-attempt/" + fixture.suffix
	snapshotID := "warehouse-snapshot-" + fixture.suffix
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const fingerprint = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	schemaDocument := []byte(`{"columns":[{"name":"id","kind":"int64","precision":0,"scale":0,"nullable":false}]}`)
	ref := contract.ObjectRef{
		OrgID: scope.OrgID, WorkspaceID: scope.WorkspaceID, ArtifactID: artifactID,
		ObjectAttemptKey: objectAttemptKey, SHA256: digest, ByteLength: 2048,
		MediaType: "application/vnd.apache.parquet", BackendGeneration: "generation-one",
	}
	verified, err := NewPreverifiedObjectRef(ref, ObjectVerification{
		ObservedSHA256: digest, ObservedByteLength: ref.ByteLength, ObservedBackendGeneration: ref.BackendGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	limits := ObjectCapacityLimits{
		OrganizationBytes: 1 << 20, WorkspaceBytes: 1 << 20,
		OrganizationAttempts: 4, WorkspaceAttempts: 4,
	}
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		if _, err := RegisterWarehouseDataset(ctx, tx, WarehouseDataset{
			Scope: scope, DatasetID: datasetID, ConnectorID: "fixture-connector", ConnectionID: connectionID,
		}); err != nil {
			return err
		}
		if _, err := ReserveObjectCapacity(ctx, tx, ObjectReservationRequest{
			ReservationID: reservationID, ConnectionID: connectionID, ArtifactID: artifactID,
			Purpose: ArtifactPurposePublic, MaximumBytes: uint64(ref.ByteLength),
		}, limits); err != nil {
			return err
		}
		if _, err := BindObjectAttempt(ctx, tx, reservationID, ObjectAttemptBinding{
			Scope: scope, ArtifactID: artifactID, ObjectAttemptKey: objectAttemptKey,
			SHA256: digest, ByteLength: ref.ByteLength, MediaType: ref.MediaType,
		}); err != nil {
			return err
		}
		if _, err := CommitObjectPublication(ctx, tx, reservationID, verified); err != nil {
			return err
		}
		if _, err := CommitManifestCAS(ctx, tx, ManifestCAS{
			ConnectionID: connectionID, ResourceID: datasetID, Object: verified,
		}); err != nil {
			return err
		}
		if _, err := RegisterWarehouseSegment(ctx, tx, datasetID, 2, fingerprint, schemaDocument); err != nil {
			return err
		}
		if _, err := AdvanceWarehouseCursor(ctx, tx, datasetID, 0, 1); err != nil {
			return err
		}
		if _, err := CreateWarehouseSnapshot(ctx, tx, WarehouseSnapshot{
			Scope: scope, SnapshotID: snapshotID, DatasetID: datasetID, ManifestRevision: 1,
		}); err != nil {
			return err
		}
		_, err := RetainWarehouseSegment(ctx, tx, WarehouseRetention{
			Scope: scope, DatasetID: datasetID, Ordinal: 1,
			Kind: WarehouseRetentionBackup, HolderID: "backup-" + fixture.suffix,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	pool.Close()

	// A downgrade that would erase registered membership is refused, and the
	// refused migration leaves the retained rows exactly as they were.
	for version := ControlSchemaVersion; version > 19; version-- {
		if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, version); err != nil {
			t.Fatalf("return retained warehouse fixture to v19: %v", err)
		}
	}
	err = migrateOneStoreVersionDown(ctx, credentials.migratorDSN, 19)
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) || databaseError.Code != "P0001" {
		t.Fatalf("retained warehouse downgrade = %v; want transactional refusal", err)
	}
	if err := Migrate(ctx, credentials.migratorDSN); err != nil {
		t.Fatalf("restore schema after warehouse downgrade refusal: %v", err)
	}

	reopened := openPermitTestPool(t, credentials, 3)
	defer reopened.Close()
	if err := WithinScope(ctx, reopened, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		dataset, err := LoadWarehouseDataset(ctx, tx, datasetID)
		if err != nil {
			return err
		}
		if dataset.ConnectionID != connectionID || dataset.ConnectorID != "fixture-connector" {
			return fmt.Errorf("retained dataset identity changed: %#v", dataset)
		}
		manifest, err := LoadManifest(ctx, tx, datasetID)
		if err != nil {
			return err
		}
		if manifest.Revision != 1 || manifest.Object != ref {
			return fmt.Errorf("retained manifest changed: %#v", manifest)
		}
		segments, err := LoadWarehouseSegments(ctx, tx, datasetID)
		if err != nil {
			return err
		}
		if len(segments) != 1 {
			return fmt.Errorf("retained segment ledger holds %d rows", len(segments))
		}
		segment := segments[0]
		if segment.Ordinal != 1 || segment.ManifestRevision != 1 ||
			segment.State != WarehouseSegmentLive || segment.RowCount != 2 ||
			segment.ColumnFingerprint != fingerprint || segment.Object != ref {
			return fmt.Errorf("retained segment changed: %#v", segment)
		}
		var stored struct {
			Columns []struct {
				Name string `json:"name"`
				Kind string `json:"kind"`
			} `json:"columns"`
		}
		if err := json.Unmarshal(segment.Schema, &stored); err != nil {
			return fmt.Errorf("retained segment schema is unreadable: %w", err)
		}
		if len(stored.Columns) != 1 || stored.Columns[0].Name != "id" || stored.Columns[0].Kind != "int64" {
			return fmt.Errorf("retained segment schema changed: %#v", stored)
		}
		cursor, err := LoadWarehouseCursor(ctx, tx, datasetID)
		if err != nil {
			return err
		}
		if cursor.Ordinal != 1 || cursor.ManifestRevision != 1 {
			return fmt.Errorf("retained cursor changed: %#v", cursor)
		}
		snapshot, err := LoadWarehouseSnapshot(ctx, tx, snapshotID)
		if err != nil {
			return err
		}
		if snapshot.DatasetID != datasetID || snapshot.ManifestRevision != 1 ||
			len(snapshot.Segments) != 1 || snapshot.Segments[0] != 1 {
			return fmt.Errorf("retained snapshot changed: %#v", snapshot)
		}
		retention, err := LoadWarehouseRetention(ctx, tx, datasetID, 1)
		if err != nil {
			return err
		}
		if len(retention) != 2 {
			return fmt.Errorf("retained holders changed: %#v", retention)
		}
		for _, holder := range retention {
			switch holder.Kind {
			case WarehouseRetentionSnapshot:
				if holder.HolderID != snapshotID {
					return fmt.Errorf("retained snapshot holder changed: %#v", holder)
				}
			case WarehouseRetentionBackup:
				if holder.HolderID != "backup-"+fixture.suffix {
					return fmt.Errorf("retained backup holder changed: %#v", holder)
				}
			default:
				return fmt.Errorf("unexpected retained holder kind: %#v", holder)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
