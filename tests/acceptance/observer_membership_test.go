package acceptance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// membershipStateTables are the tables of the membership state of a
// workspace (criteria-worker v4, line 67): the memberships with their grants
// and revisions, the membership collection revision, and the audit records of
// the workspace. pm.workspace_audit_entries holds the audit records that
// belong to no run, and pm.audit_entries holds the audit records of runs.
var membershipStateTables = []string{
	"memberships",
	"identity_collection_revisions",
	"workspace_audit_entries",
	"audit_entries",
}

// membershipAuditTables are the audit tables of the membership state.
var membershipAuditTables = []string{"workspace_audit_entries", "audit_entries"}

// storedCredentialTables are the tables that hold the stored provider
// credential of a connection: the ciphertext, the lifecycle and the mutation
// receipt (0002_tenancy_rls.sql and 0004_credential_lifecycle.sql).
var storedCredentialTables = []string{
	"encrypted_secrets",
	"credential_lifecycles",
	"credential_mutation_receipts",
}

// membershipState is one observer reading of the membership state of a
// workspace: for each table, the rows as JSON text, in sorted order.
type membershipState map[string][]string

// openMembershipObserver makes the observer of the 284c scenarios, in the
// pattern of openObserver and openBootstrapObserver: its own role, with
// BYPASSRLS, read-only, and SELECT only on the tables of the membership state
// and of the stored credentials. It connects the role and checks its limits.
// One deadline, the limit of one lane command, bounds these requests. close
// drops the role.
func openMembershipObserver(parent context.Context, stack stackInputs) (*observer, error) {
	ctx, cancel := context.WithTimeout(parent, laneCommandTimeout)
	defer cancel()
	role := "pm_acceptance_membership_observer_" + randomHex(6)
	password := randomHex(24)
	result := &observer{adminDSN: stack.postgres.dsn(stackAdminRole, stack.postgres.adminPassword, applicationDatabase)}
	fail := func(cause error) (*observer, error) {
		return nil, errors.Join(cause, result.close())
	}
	tables := slices.Concat(membershipStateTables, storedCredentialTables)
	if err := result.createMembershipRole(ctx, role, password, tables); err != nil {
		return fail(err)
	}
	database, err := pgx.Connect(ctx, stack.postgres.dsn(role, password, applicationDatabase))
	if err != nil {
		return fail(fmt.Errorf("connect the membership observer role: %w", err))
	}
	result.database = database
	if err := result.checkReadOnly(ctx); err != nil {
		return fail(err)
	}
	return result, nil
}

// createMembershipRole makes the membership observer role with the stack
// administrator account.
func (o *observer) createMembershipRole(ctx context.Context, role, password string, tables []string) (err error) {
	admin, err := pgx.Connect(ctx, o.adminDSN)
	if err != nil {
		return fmt.Errorf("connect the stack administrator: %w", err)
	}
	// The close uses the deadline of ctx, the limit of one lane command. A
	// failed close fails the step.
	defer func() {
		if closeErr := admin.Close(ctx); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close the stack administrator connection: %w", closeErr))
		}
	}()
	name := pgx.Identifier{role}.Sanitize()
	names := make([]string, 0, len(tables))
	for _, table := range tables {
		names = append(names, pgx.Identifier{"pm", table}.Sanitize())
	}
	// The password is lowercase hexadecimal, so the literal needs no escape.
	statements := []string{
		"CREATE ROLE " + name + " LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION BYPASSRLS CONNECTION LIMIT 2 PASSWORD '" + password + "'",
		"ALTER ROLE " + name + " SET default_transaction_read_only = on",
		"GRANT CONNECT ON DATABASE " + applicationDatabase + " TO " + name,
		"GRANT USAGE ON SCHEMA pm TO " + name,
		"GRANT SELECT ON " + strings.Join(names, ", ") + " TO " + name,
	}
	for index, statement := range statements {
		if _, err := admin.Exec(ctx, statement); err != nil {
			return fmt.Errorf("create the membership observer role (statement %d): %w", index+1, err)
		}
		if index == 0 {
			// From now on, close drops the role.
			o.role = role
		}
	}
	return nil
}

// readMembershipState reads every row of the workspace in each membership
// state table in one read-only snapshot, as JSON text in sorted order. One
// deadline, the limit of one lane command, bounds the whole snapshot: the
// begin, each read and the rollback that ends it. A failed rollback fails the
// reading.
func (o *observer) readMembershipState(parent context.Context, orgID, workspaceID string) (state membershipState, err error) {
	ctx, cancel := context.WithTimeout(parent, laneCommandTimeout)
	defer cancel()
	tx, err := o.database.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin the membership observer snapshot: %w", err)
	}
	// This deferred call runs before the deferred cancel, so the rollback
	// has the same deadline as the snapshot.
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
			state, err = nil, errors.Join(err, fmt.Errorf("end the membership observer snapshot: %w", rollbackErr))
		}
	}()
	state = make(membershipState, len(membershipStateTables))
	for _, table := range membershipStateTables {
		query := "SELECT COALESCE(array_agg(r.row ORDER BY r.row COLLATE \"C\"), '{}') FROM (SELECT to_jsonb(t)::text AS row FROM " +
			pgx.Identifier{"pm", table}.Sanitize() + " t WHERE t.org_id = $1::text AND t.workspace_id = $2::text) r"
		var rows []string
		if err := tx.QueryRow(ctx, query, orgID, workspaceID).Scan(&rows); err != nil {
			return nil, fmt.Errorf("read pm.%s: %w", table, err)
		}
		state[table] = rows
	}
	return state, nil
}

// storedCredentials is one observer reading of the stored credential of one
// connection. It holds no ciphertext: for each ciphertext, it keeps only
// whether the ciphertext holds the secret that the step gave.
type storedCredentials struct {
	lifecycles []storedLifecycle
	secrets    []storedSecret
	receipts   []storedReceipt
}

type storedLifecycle struct {
	credentialRef     string
	ciphertextVersion int64
	status            string
	revision          int64
}

type storedSecret struct {
	credentialRef     string
	ciphertextVersion int64
	// holdsSecret is true when the ciphertext holds the secret bytes.
	holdsSecret bool
}

type storedReceipt struct {
	principalID       string
	principalKind     string
	credentialRef     string
	ciphertextVersion int64
}

// rowCount is the number of stored credential rows of the reading.
func (c storedCredentials) rowCount() int {
	return len(c.lifecycles) + len(c.secrets) + len(c.receipts)
}

// readStoredCredentials reads the stored credential rows of the connection in
// one read-only snapshot. For each ciphertext it checks whether the
// ciphertext holds secret, when secret is not empty, and then clears its copy
// of the ciphertext. One deadline, the limit of one lane command, bounds the
// whole snapshot. A failed rollback fails the reading.
func (o *observer) readStoredCredentials(parent context.Context, orgID, workspaceID, connectionID string, secret []byte) (stored storedCredentials, err error) {
	ctx, cancel := context.WithTimeout(parent, laneCommandTimeout)
	defer cancel()
	tx, err := o.database.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return storedCredentials{}, fmt.Errorf("begin the stored credential snapshot: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
			stored, err = storedCredentials{}, errors.Join(err, fmt.Errorf("end the stored credential snapshot: %w", rollbackErr))
		}
	}()
	scope := []any{orgID, workspaceID, connectionID}
	const where = " WHERE org_id = $1::text AND workspace_id = $2::text AND connection_id = $3::text"

	rows, err := tx.Query(ctx, "SELECT credential_ref, current_ciphertext_version, status, revision FROM pm.credential_lifecycles"+where, scope...)
	if err != nil {
		return storedCredentials{}, fmt.Errorf("read pm.credential_lifecycles: %w", err)
	}
	stored.lifecycles, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedLifecycle, error) {
		var value storedLifecycle
		err := row.Scan(&value.credentialRef, &value.ciphertextVersion, &value.status, &value.revision)
		return value, err
	})
	if err != nil {
		return storedCredentials{}, fmt.Errorf("read pm.credential_lifecycles: %w", err)
	}

	rows, err = tx.Query(ctx, "SELECT credential_ref, ciphertext_version, ciphertext FROM pm.encrypted_secrets"+where, scope...)
	if err != nil {
		return storedCredentials{}, fmt.Errorf("read pm.encrypted_secrets: %w", err)
	}
	stored.secrets, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedSecret, error) {
		var value storedSecret
		var ciphertext []byte
		err := row.Scan(&value.credentialRef, &value.ciphertextVersion, &ciphertext)
		value.holdsSecret = len(secret) > 0 && bytes.Contains(ciphertext, secret)
		clear(ciphertext)
		return value, err
	})
	if err != nil {
		return storedCredentials{}, fmt.Errorf("read pm.encrypted_secrets: %w", err)
	}

	rows, err = tx.Query(ctx, "SELECT principal_id, principal_kind, credential_ref, result_ciphertext_version FROM pm.credential_mutation_receipts"+where, scope...)
	if err != nil {
		return storedCredentials{}, fmt.Errorf("read pm.credential_mutation_receipts: %w", err)
	}
	stored.receipts, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedReceipt, error) {
		var value storedReceipt
		err := row.Scan(&value.principalID, &value.principalKind, &value.credentialRef, &value.ciphertextVersion)
		return value, err
	})
	if err != nil {
		return storedCredentials{}, fmt.Errorf("read pm.credential_mutation_receipts: %w", err)
	}
	return stored, nil
}
