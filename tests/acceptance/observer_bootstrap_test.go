package acceptance

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// openBootstrapObserver makes the observer of the bootstrap scenarios, in the
// pattern of openObserver: its own role, with BYPASSRLS, read-only, and
// SELECT only on the tables of the bootstrap state. It connects the role and
// checks its limits. One deadline, the limit of one lane command, bounds
// these requests. close drops the role.
func openBootstrapObserver(parent context.Context, stack stackInputs) (*observer, error) {
	ctx, cancel := context.WithTimeout(parent, laneCommandTimeout)
	defer cancel()
	role := "pm_acceptance_bootstrap_observer_" + randomHex(6)
	password := randomHex(24)
	result := &observer{adminDSN: stack.postgres.dsn(stackAdminRole, stack.postgres.adminPassword, applicationDatabase)}
	fail := func(cause error) (*observer, error) {
		return nil, errors.Join(cause, result.close())
	}
	if err := result.createBootstrapRole(ctx, role, password); err != nil {
		return fail(err)
	}
	database, err := pgx.Connect(ctx, stack.postgres.dsn(role, password, applicationDatabase))
	if err != nil {
		return fail(fmt.Errorf("connect the bootstrap observer role: %w", err))
	}
	result.database = database
	if err := result.checkReadOnly(ctx); err != nil {
		return fail(err)
	}
	return result, nil
}

// createBootstrapRole makes the bootstrap observer role with the stack
// administrator account.
func (o *observer) createBootstrapRole(ctx context.Context, role, password string) (err error) {
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
	tables := make([]string, 0, len(bootstrapTables))
	for _, table := range bootstrapTables {
		tables = append(tables, pgx.Identifier{"pm", table}.Sanitize())
	}
	// The password is lowercase hexadecimal, so the literal needs no escape.
	statements := []string{
		"CREATE ROLE " + name + " LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION BYPASSRLS CONNECTION LIMIT 2 PASSWORD '" + password + "'",
		"ALTER ROLE " + name + " SET default_transaction_read_only = on",
		"GRANT CONNECT ON DATABASE " + applicationDatabase + " TO " + name,
		"GRANT USAGE ON SCHEMA pm TO " + name,
		"GRANT SELECT ON " + strings.Join(tables, ", ") + " TO " + name,
	}
	for index, statement := range statements {
		if _, err := admin.Exec(ctx, statement); err != nil {
			return fmt.Errorf("create the bootstrap observer role (statement %d): %w", index+1, err)
		}
		if index == 0 {
			// From now on, close drops the role.
			o.role = role
		}
	}
	return nil
}

// readBootstrapState reads every row of each bootstrap table in one
// read-only snapshot, as JSON text in sorted order. An empty organization
// reads the whole stack. Another organization reads only its rows. One
// deadline, the limit of one lane command, bounds the whole snapshot: the
// begin, each read and the rollback that ends it. A failed rollback fails the
// reading.
func (o *observer) readBootstrapState(parent context.Context, orgID string) (state bootstrapState, err error) {
	ctx, cancel := context.WithTimeout(parent, laneCommandTimeout)
	defer cancel()
	tx, err := o.database.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin the bootstrap observer snapshot: %w", err)
	}
	// This deferred call runs before the deferred cancel, so the rollback
	// has the same deadline as the snapshot.
	defer func() {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
			state, err = nil, errors.Join(err, fmt.Errorf("end the bootstrap observer snapshot: %w", rollbackErr))
		}
	}()
	state = make(bootstrapState, len(bootstrapTables))
	for _, table := range bootstrapTables {
		query := "SELECT COALESCE(array_agg(r.row ORDER BY r.row COLLATE \"C\"), '{}') FROM (SELECT to_jsonb(t)::text AS row FROM " +
			pgx.Identifier{"pm", table}.Sanitize() + " t WHERE $1::text = '' OR t.org_id = $1::text) r"
		var rows []string
		if err := tx.QueryRow(ctx, query, orgID).Scan(&rows); err != nil {
			return nil, fmt.Errorf("read pm.%s: %w", table, err)
		}
		state[table] = rows
	}
	return state, nil
}
