package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// TestReservationExpiresUnusedAccountPermits covers the expiry of a reserved
// account permit that no settlement or retry releases. A reservation that finds
// its provider account short of capacity expires the account's reserved
// permits whose expiry has passed, in every workspace that shares the account.
// It never touches an unexpired or a dispatched permit. SQL moves an expiry
// into the past, so no check sleeps, polls or retries.
func TestReservationExpiresUnusedAccountPermits(t *testing.T) {
	credentials, pool := prepareTestStore(t, 4)

	t.Run("an expired reserved permit of another workspace returns its capacity", func(t *testing.T) {
		fixture := registerPermitTestFixture(t, credentials, pool, permitFixtureOptions{Capacity: 1})
		leaked := reserveExpiryFixturePermit(t, pool, fixture.beta, fixture.betaConnection, "leaked-"+fixture.suffix)
		moveExpiryFixturePermitPastExpiry(t, credentials, leaked, AccountPermitReserved)

		next, err := reservePermitForTest(context.Background(), pool, fixture.alpha, expiryFixtureRequest(
			fixture.alphaConnections[0], "next-"+fixture.suffix,
		))
		if err != nil {
			t.Fatalf("reservation on an account with an expired reserved permit: %v; want the expired permit to return its capacity", err)
		}
		expired, err := loadPermitForTest(context.Background(), pool, fixture.beta, leaked.PermitID)
		if err != nil {
			t.Fatal(err)
		}
		if expired.State != AccountPermitExpired || expired.ResolvedAt == nil ||
			expired.Fence != leaked.Fence || expired.OwnerID != leaked.OwnerID {
			t.Fatalf("leaked permit after the reservation = %+v; want the same permit expired", expired)
		}
		if next.State != AccountPermitReserved || readReservedUnits(t, pool, fixture.alpha, fixture.account.AccountID) != 1 {
			t.Fatalf("next permit %+v with reserved units %d; want one reserved unit",
				next, readReservedUnits(t, pool, fixture.alpha, fixture.account.AccountID))
		}
		if _, err := authorizePermitForTest(context.Background(), pool, leaked); !errors.Is(err, ErrAccountPermitStale) {
			t.Fatalf("admission of the expired permit error = %v; want ErrAccountPermitStale", err)
		}
		replayed, err := expirePermitForTest(context.Background(), pool, leaked)
		if err != nil || replayed.State != AccountPermitExpired ||
			readReservedUnits(t, pool, fixture.alpha, fixture.account.AccountID) != 1 {
			t.Fatalf("store expiry replay = %+v error %v; want the terminal permit and no second release", replayed, err)
		}
	})

	t.Run("an unexpired reserved permit keeps its capacity", func(t *testing.T) {
		fixture := registerPermitTestFixture(t, credentials, pool, permitFixtureOptions{Capacity: 1})
		held := reserveExpiryFixturePermit(t, pool, fixture.beta, fixture.betaConnection, "held-"+fixture.suffix)
		_, err := reservePermitForTest(context.Background(), pool, fixture.alpha, expiryFixtureRequest(
			fixture.alphaConnections[0], "refused-"+fixture.suffix,
		))
		if !errors.Is(err, ErrAccountCapacity) {
			t.Fatalf("reservation over an unexpired reserved permit error = %v; want ErrAccountCapacity", err)
		}
		requireExpiryFixturePermitState(t, pool, fixture.beta, held, AccountPermitReserved)
	})

	t.Run("a dispatched permit past its expiry keeps its capacity", func(t *testing.T) {
		fixture := registerPermitTestFixture(t, credentials, pool, permitFixtureOptions{Capacity: 1})
		held := reserveExpiryFixturePermit(t, pool, fixture.beta, fixture.betaConnection, "dispatched-"+fixture.suffix)
		dispatched, err := authorizePermitForTest(context.Background(), pool, held)
		if err != nil {
			t.Fatal(err)
		}
		moveExpiryFixturePermitPastExpiry(t, credentials, dispatched, AccountPermitDispatched)
		_, err = reservePermitForTest(context.Background(), pool, fixture.alpha, expiryFixtureRequest(
			fixture.alphaConnections[0], "refused-"+fixture.suffix,
		))
		if !errors.Is(err, ErrAccountCapacity) {
			t.Fatalf("reservation over a dispatched permit error = %v; want ErrAccountCapacity", err)
		}
		requireExpiryFixturePermitState(t, pool, fixture.beta, held, AccountPermitDispatched)
	})

	t.Run("a reservation with free capacity expires nothing", func(t *testing.T) {
		fixture := registerPermitTestFixture(t, credentials, pool, permitFixtureOptions{Capacity: 2})
		leaked := reserveExpiryFixturePermit(t, pool, fixture.beta, fixture.betaConnection, "leaked-"+fixture.suffix)
		moveExpiryFixturePermitPastExpiry(t, credentials, leaked, AccountPermitReserved)
		if _, err := reservePermitForTest(context.Background(), pool, fixture.alpha, expiryFixtureRequest(
			fixture.alphaConnections[0], "next-"+fixture.suffix,
		)); err != nil {
			t.Fatal(err)
		}
		requireExpiryFixturePermitState(t, pool, fixture.beta, leaked, AccountPermitReserved)
		if got := readReservedUnits(t, pool, fixture.alpha, fixture.account.AccountID); got != 2 {
			t.Fatalf("reserved units = %d; want 2", got)
		}
	})

	t.Run("the expiry function is a bounded definer for the installed scope", func(t *testing.T) {
		fixture := registerPermitTestFixture(t, credentials, pool, permitFixtureOptions{Capacity: 1})
		foreign := registerPermitTestFixture(t, credentials, pool, permitFixtureOptions{Capacity: 1})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var definer, publicExecute, runtimeExecute bool
		var owner string
		var settings []string
		if err := WithinTransaction(ctx, pool, func(ctx context.Context, tx *Transaction) error {
			return tx.queryRow(ctx, `
				SELECT procedure.prosecdef,
				       pg_get_userbyid(procedure.proowner),
				       COALESCE(procedure.proconfig, ARRAY[]::text[]),
				       EXISTS (
				           SELECT 1
				           FROM aclexplode(COALESCE(procedure.proacl, acldefault('f', procedure.proowner))) AS expanded_acl
				           WHERE expanded_acl.grantee = 0 AND expanded_acl.privilege_type = 'EXECUTE'
				       ),
				       has_function_privilege('pm_app_runtime', procedure.oid, 'EXECUTE')
				FROM pg_proc AS procedure
				WHERE procedure.oid = to_regprocedure('pm.expire_unused_account_permits(text,text)')
			`).Scan(&definer, &owner, &settings, &publicExecute, &runtimeExecute)
		}); err != nil {
			t.Fatalf("inspect the account permit expiry function: %v", err)
		}
		if !definer || owner != MigratorRole || publicExecute || !runtimeExecute ||
			!slices.Equal(settings, []string{"search_path=pg_catalog, pg_temp"}) {
			t.Fatalf("expiry function authority = definer:%t owner:%q settings:%v public:%t runtime:%t",
				definer, owner, settings, publicExecute, runtimeExecute)
		}

		leaked := reserveExpiryFixturePermit(t, pool, foreign.beta, foreign.betaConnection, "foreign-"+foreign.suffix)
		moveExpiryFixturePermitPastExpiry(t, credentials, leaked, AccountPermitReserved)
		for _, target := range []struct {
			name, orgID, accountID string
		}{
			{"another organization", foreign.account.OrgID, foreign.account.AccountID},
			{"an account with no connection in the workspace", fixture.account.OrgID, foreign.account.AccountID},
		} {
			err := WithinScope(ctx, pool, fixture.alpha, testActor(fixture.alpha), func(ctx context.Context, tx *Transaction) error {
				var units int64
				return tx.queryRow(ctx, `SELECT pm.expire_unused_account_permits($1, $2)`, target.orgID, target.accountID).Scan(&units)
			})
			var databaseError *pgconn.PgError
			if !errors.As(err, &databaseError) || databaseError.Code != "42501" {
				t.Fatalf("expiry for %s error = %v; want SQLSTATE 42501", target.name, err)
			}
		}
		requireExpiryFixturePermitState(t, pool, foreign.beta, leaked, AccountPermitReserved)
	})
}

func expiryFixtureRequest(connectionID, permitID string) AccountPermitRequest {
	return AccountPermitRequest{
		PermitID: "permit-expiry-" + permitID, ConnectionID: connectionID,
		OwnerID: "expiry-worker-" + permitID, Units: 1, TTL: time.Hour,
	}
}

func reserveExpiryFixturePermit(t *testing.T, pool *Pool, scope contract.Scope, connectionID, permitID string) AccountPermit {
	t.Helper()
	permit, err := reservePermitForTest(context.Background(), pool, scope, expiryFixtureRequest(connectionID, permitID))
	if err != nil {
		t.Fatalf("reserve expiry fixture permit: %v", err)
	}
	return permit
}

func requireExpiryFixturePermitState(t *testing.T, pool *Pool, scope contract.Scope, permit AccountPermit, state AccountPermitState) {
	t.Helper()
	current, err := loadPermitForTest(context.Background(), pool, scope, permit.PermitID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != state || current.Fence != permit.Fence || current.OwnerID != permit.OwnerID {
		t.Fatalf("permit %s = %+v; want state %q with its fence and owner", permit.PermitID, current, state)
	}
}

// moveExpiryFixturePermitPastExpiry sets the permit's expiry to one
// microsecond after its reservation through an owned migrator connection.
func moveExpiryFixturePermitPastExpiry(t *testing.T, credentials testStoreCredentials, permit AccountPermit, state AccountPermitState) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, credentials.migratorDSN)
	if err != nil {
		t.Fatalf("open owned permit fixture connection: %T", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := detachedCleanupContext()
		defer cleanupCancel()
		if err := conn.Close(cleanupCtx); err != nil {
			t.Errorf("close owned permit fixture connection: %T", err)
		}
	}()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := detachedCleanupContext()
		defer cleanupCancel()
		if err := tx.Rollback(cleanupCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("close owned permit fixture transaction: %T", err)
		}
	}()
	if _, err := tx.Exec(ctx, `
		SELECT set_config('pm.org_id', $1, true), set_config('pm.workspace_id', $2, true)
	`, permit.Scope.OrgID, permit.Scope.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE pm.account_permits
		SET expires_at = reserved_at + interval '1 microsecond'
		WHERE org_id = $1 AND workspace_id = $2 AND permit_id = $3 AND state = $4
	`, permit.Scope.OrgID, permit.Scope.WorkspaceID, permit.PermitID, string(state))
	if err != nil {
		t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("permit expiry fixture changed %d rows; want 1", tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
