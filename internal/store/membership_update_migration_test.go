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

// membershipUpdateProbe runs one statement as the runtime role in its own
// transaction, with the scope and the actor of a request, and always rolls it
// back. So a probe leaves no row.
type membershipUpdateProbe struct {
	t          *testing.T
	runtimeDSN string
	scope      contract.Scope
	actorID    string
}

func (probe membershipUpdateProbe) exec(query string, arguments ...any) (pgconn.CommandTag, error) {
	probe.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, probe.runtimeDSN)
	if err != nil {
		probe.t.Fatalf("connect the runtime role: %T", err)
	}
	tx, err := connection.Begin(ctx)
	if err != nil {
		if closeErr := endMembershipUpdateTransaction(nil, connection, membershipUpdateCleanupLimit); closeErr != nil {
			probe.t.Errorf("close the runtime probe connection: %v", closeErr)
		}
		probe.t.Fatalf("begin the runtime probe: %T", err)
	}
	defer func() {
		if err := endMembershipUpdateTransaction(tx, connection, membershipUpdateCleanupLimit); err != nil {
			probe.t.Errorf("end the runtime probe: %v", err)
		}
	}()
	if _, err := tx.Exec(ctx, `
		SELECT set_config('pm.org_id', $1, true),
		       set_config('pm.workspace_id', $2, true),
		       set_config('pm.requested_actor_id', $3, true),
		       set_config('pm.requested_actor_kind', 'user', true),
		       set_config('pm.effective_actor_id', $3, true),
		       set_config('pm.effective_actor_kind', 'user', true)
	`, probe.scope.OrgID, probe.scope.WorkspaceID, probe.actorID); err != nil {
		probe.t.Fatalf("bind the runtime probe scope: %T", err)
	}
	return tx.Exec(ctx, query, arguments...)
}

// membershipUpdateCleanupLimit bounds each cleanup step of a probe or of a
// migrator fixture transaction.
const membershipUpdateCleanupLimit = 10 * time.Second

// endMembershipUpdateTransaction rolls the transaction back and closes its
// connection. Each step has its own context with the limit, so a PostgreSQL
// that does not answer cannot hold the check after its own deadline. A
// rollback after a commit (pgx.ErrTxClosed) is the normal end of a committed
// fixture. A nil tx skips the rollback.
func endMembershipUpdateTransaction(
	tx interface{ Rollback(context.Context) error },
	connection interface{ Close(context.Context) error },
	limit time.Duration,
) error {
	var rollbackErr error
	if tx != nil {
		ctx, cancel := context.WithTimeout(context.Background(), limit)
		rollbackErr = tx.Rollback(ctx)
		cancel()
		if errors.Is(rollbackErr, pgx.ErrTxClosed) {
			rollbackErr = nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	return errors.Join(rollbackErr, connection.Close(ctx))
}

// Go developer check, not acceptance proof. The probe and the migrator
// fixture must end their cleanup at its limit when PostgreSQL does not
// answer. The two stand-ins return only when the context of each call ends,
// as the driver does on a connection that does not answer. Defect: a cleanup
// on context.Background(), which never ends.
func TestMembershipUpdateCleanupEndsAtItsLimit(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		done <- endMembershipUpdateTransaction(stalledMembershipCleanup{}, stalledMembershipCleanup{}, 100*time.Millisecond)
	}()
	guard := time.NewTimer(10 * time.Second)
	defer guard.Stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("cleanup with a PostgreSQL that does not answer = %v; want the end of its limit", err)
		}
	case <-guard.C:
		t.Fatal("cleanup did not end 10 s after a PostgreSQL that does not answer; want an end at its limit")
	}
}

// stalledMembershipCleanup stands for a rollback and a close on a connection
// that does not answer: each call returns only when its context ends.
type stalledMembershipCleanup struct{}

func (stalledMembershipCleanup) Rollback(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (stalledMembershipCleanup) Close(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func requireInsufficientPrivilege(t *testing.T, name string, err error) {
	t.Helper()
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) || databaseError.Code != "42501" {
		t.Fatalf("%s = %v; want refusal 42501", name, err)
	}
}

// Check: after migration 0022 the runtime role can add connection grants to a
// membership and insert a workspace audit record, only in the scope and with
// the actors of its own transaction. It still cannot read or remove an audit
// record or change any other membership column. The down step removes this
// authority again. Defect it catches: a membership update that pm-server
// cannot write (permission denied), or a grant that lets pm-server forge
// another actor or scope, or change the identity, role, permissions,
// operations or expiry of a membership.
func TestMembershipUpdateMigrationGrantsOnlyTheUpdateAuthority(t *testing.T) {
	credentials, pool := prepareTestStore(t, 2)
	pool.Close()
	scope := newTestScope(t)
	actorID := "ada-" + randomHex(t, 6)
	membershipID := "membership-" + randomHex(t, 6)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	type statement struct {
		query     string
		arguments []any
	}
	// asMigrator has its own time limit, because a cleanup runs after the
	// test's own context ends.
	asMigrator := func(statements ...statement) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		connection, err := pgx.Connect(ctx, credentials.migratorDSN)
		if err != nil {
			t.Fatalf("connect the migrator: %T", err)
		}
		tx, err := connection.Begin(ctx)
		if err != nil {
			if closeErr := endMembershipUpdateTransaction(nil, connection, membershipUpdateCleanupLimit); closeErr != nil {
				t.Errorf("close the migrator connection: %v", closeErr)
			}
			t.Fatalf("begin the migrator fixture: %T", err)
		}
		defer func() {
			if err := endMembershipUpdateTransaction(tx, connection, membershipUpdateCleanupLimit); err != nil {
				t.Errorf("end the migrator fixture: %v", err)
			}
		}()
		if _, err := tx.Exec(ctx, `
			SELECT set_config('pm.org_id', $1, true),
			       set_config('pm.workspace_id', $2, true),
			       set_config('pm.requested_actor_id', $3, true),
			       set_config('pm.requested_actor_kind', 'user', true),
			       set_config('pm.effective_actor_id', $3, true),
			       set_config('pm.effective_actor_kind', 'user', true)
		`, scope.OrgID, scope.WorkspaceID, actorID); err != nil {
			t.Fatalf("bind the migrator fixture scope: %T", err)
		}
		for _, item := range statements {
			if _, err := tx.Exec(ctx, item.query, item.arguments...); err != nil {
				t.Fatalf("migrator fixture statement: %v", err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit the migrator fixture: %T", err)
		}
	}
	asMigrator(
		statement{`INSERT INTO pm.workspace_namespaces (org_id, workspace_id, name) VALUES ($1, $2, 'membership update')`,
			[]any{scope.OrgID, scope.WorkspaceID}},
		statement{`INSERT INTO pm.identities (org_id, identity_id, kind, issuer, subject, revision)
			VALUES ($1, $2, 'user', 'https://fixture.example/' || $2, $2, 1)`, []any{scope.OrgID, actorID}},
		statement{`INSERT INTO pm.memberships (
				org_id, workspace_id, membership_id, identity_id, role, resource_grants, permissions,
				connection_ids, operation_ids, policy_epoch, control_revision
			) VALUES ($1, $2, $3, $4, 'admin', ARRAY['permission:workspace.read'], ARRAY['workspace.read'],
				ARRAY[]::text[], ARRAY[]::text[], 1, 1)`, []any{scope.OrgID, scope.WorkspaceID, membershipID, actorID}},
	)
	t.Cleanup(func() {
		asMigrator(
			statement{`DELETE FROM pm.memberships WHERE org_id = $1 AND workspace_id = $2 AND membership_id = $3`,
				[]any{scope.OrgID, scope.WorkspaceID, membershipID}},
			statement{`DELETE FROM pm.identities WHERE org_id = $1 AND identity_id = $2`, []any{scope.OrgID, actorID}},
			statement{`DELETE FROM pm.workspace_namespaces WHERE org_id = $1 AND workspace_id = $2`,
				[]any{scope.OrgID, scope.WorkspaceID}},
		)
	})

	probe := membershipUpdateProbe{t: t, runtimeDSN: credentials.runtimeDSN, scope: scope, actorID: actorID}
	insertAudit := func(workspaceID, requestedActorID, effectiveActorKind string) error {
		_, err := probe.exec(`
			INSERT INTO pm.workspace_audit_entries (
				org_id, workspace_id, audit_id, requested_actor_id, requested_actor_kind,
				effective_actor_id, effective_actor_kind, action, target_ref, payload
			) VALUES ($1, $2, 'audit-probe', $3, 'user', $4, $5, 'membership.update', $6, '{}'::jsonb)
		`, scope.OrgID, workspaceID, requestedActorID, actorID, effectiveActorKind, membershipID)
		return err
	}
	updateGrants := func() (pgconn.CommandTag, error) {
		return probe.exec(`
			UPDATE pm.memberships
			SET resource_grants = ARRAY['connection:connection-probe', 'permission:workspace.read'],
			    connection_ids = ARRAY['connection-probe']
			WHERE membership_id = $1
		`, membershipID)
	}

	// The runtime role adds connection grants and writes its own audit record.
	if err := insertAudit(scope.WorkspaceID, actorID, "user"); err != nil {
		t.Fatalf("runtime insert of a workspace audit record in its own scope = %v; want success", err)
	}
	if tag, err := updateGrants(); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("runtime update of the connection grants = %v rows, %v; want 1 row", tag.RowsAffected(), err)
	}

	// The policy binds the scope and both actors to the transaction.
	requireInsufficientPrivilege(t, "audit insert for another workspace", insertAudit("workspace-other-"+randomHex(t, 4), actorID, "user"))
	requireInsufficientPrivilege(t, "audit insert for another requested actor", insertAudit(scope.WorkspaceID, "mallory", "user"))
	requireInsufficientPrivilege(t, "audit insert for another effective actor kind", insertAudit(scope.WorkspaceID, actorID, "operator"))

	// The runtime role cannot read or remove an audit record, or change any
	// other membership column.
	for name, query := range map[string]string{
		"audit read":        `SELECT count(*) FROM pm.workspace_audit_entries WHERE target_ref = $1`,
		"audit delete":      `DELETE FROM pm.workspace_audit_entries WHERE target_ref = $1`,
		"role change":       `UPDATE pm.memberships SET role = 'owner' WHERE membership_id = $1`,
		"identity change":   `UPDATE pm.memberships SET identity_id = 'mallory' WHERE membership_id = $1`,
		"permission change": `UPDATE pm.memberships SET permissions = ARRAY['workspace.access.manage'] WHERE membership_id = $1`,
		"operation change":  `UPDATE pm.memberships SET operation_ids = ARRAY['operation-probe'] WHERE membership_id = $1`,
		"expiry change":     `UPDATE pm.memberships SET expires_at = clock_timestamp() WHERE membership_id = $1`,
	} {
		_, err := probe.exec(query, membershipID)
		requireInsufficientPrivilege(t, name, err)
	}

	// The down step removes the authority and keeps the earlier revision.
	revision := func() string {
		t.Helper()
		connection, err := pgx.Connect(ctx, credentials.migratorDSN)
		if err != nil {
			t.Fatalf("connect the migrator: %T", err)
		}
		defer func() { _ = connection.Close(context.Background()) }()
		var value string
		if err := connection.QueryRow(ctx, `SELECT revision FROM pm.control_schema WHERE singleton`).Scan(&value); err != nil {
			t.Fatalf("read the control schema revision: %v", err)
		}
		return value
	}
	// Later migrations step down first.
	for version := ControlSchemaVersion; version > 21; version-- {
		if err := migrateOneStoreVersionDown(ctx, credentials.migratorDSN, version); err != nil {
			t.Fatalf("step down to the version before migration 0022: %v", err)
		}
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		if err := Migrate(cleanupCtx, credentials.migratorDSN); err != nil {
			t.Errorf("restore the current store schema: %v", err)
		}
	})
	if got := revision(); got != "pm.control/v21" {
		t.Fatalf("control schema revision after the down step = %q; want pm.control/v21", got)
	}
	requireInsufficientPrivilege(t, "audit insert after the down step", insertAudit(scope.WorkspaceID, actorID, "user"))
	_, err := updateGrants()
	requireInsufficientPrivilege(t, "grant update after the down step", err)
	if err := Migrate(ctx, credentials.migratorDSN); err != nil {
		t.Fatalf("apply migration 0022 again: %v", err)
	}
	if got := revision(); got != ControlSchemaRevision {
		t.Fatalf("control schema revision = %q; want %q", got, ControlSchemaRevision)
	}
	if err := insertAudit(scope.WorkspaceID, actorID, "user"); err != nil {
		t.Fatalf("runtime audit insert after the second up step = %v; want success", err)
	}
}
