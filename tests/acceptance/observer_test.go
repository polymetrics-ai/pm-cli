package acceptance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// observedTables are the PM run and audit tables that the observer counts.
// The admission key and outbox tables are included because an admitted run
// writes to them first.
var observedTables = []string{"runs", "run_specs", "admission_keys", "outbox_commands", "audit_entries"}

// stackAdminRole is the stack administrator account. The observer uses it
// only to make and drop its own role.
const stackAdminRole = "pm_stack_admin"

// observer reads the stack independently of the PM processes. It reads
// PostgreSQL through its own role, which can only read the observed tables.
// It never decrypts, repairs or writes. Step 29 also reads the lane server
// information through the observer HTTPS client of steps 29 and 30
// (readServerInstanceID).
//
// The stack has no observer role, so the lane makes one with the stack
// administrator account and drops it when the suite ends. The role has
// BYPASSRLS, so row-level security cannot hide a row from a count.
type observer struct {
	adminDSN string
	role     string
	database *pgx.Conn
}

// openObserver makes the observer role, connects it and checks its limits.
// One deadline, the limit of one lane command, bounds these requests.
func openObserver(parent context.Context, stack stackInputs) (*observer, error) {
	ctx, cancel := context.WithTimeout(parent, laneCommandTimeout)
	defer cancel()
	role := "pm_acceptance_observer_" + randomHex(6)
	password := randomHex(24)
	result := &observer{adminDSN: stack.postgres.dsn(stackAdminRole, stack.postgres.adminPassword, applicationDatabase)}
	fail := func(cause error) (*observer, error) {
		return nil, errors.Join(cause, result.close())
	}
	if err := result.createRole(ctx, role, password); err != nil {
		return fail(err)
	}
	database, err := pgx.Connect(ctx, stack.postgres.dsn(role, password, applicationDatabase))
	if err != nil {
		return fail(fmt.Errorf("connect the observer role: %w", err))
	}
	result.database = database
	if err := result.checkReadOnly(ctx); err != nil {
		return fail(err)
	}
	return result, nil
}

func (o *observer) createRole(ctx context.Context, role, password string) error {
	admin, err := pgx.Connect(ctx, o.adminDSN)
	if err != nil {
		return fmt.Errorf("connect the stack administrator: %w", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	name := pgx.Identifier{role}.Sanitize()
	tables := make([]string, 0, len(observedTables))
	for _, table := range observedTables {
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
			return fmt.Errorf("create the observer role (statement %d): %w", index+1, err)
		}
		if index == 0 {
			// From now on, close drops the role.
			o.role = role
		}
	}
	return nil
}

// checkReadOnly confirms the observer's own limits before it observes.
func (o *observer) checkReadOnly(ctx context.Context) error {
	var readOnly string
	var bypass, superuser, writes bool
	err := o.database.QueryRow(ctx, `
		SELECT current_setting('transaction_read_only'), r.rolbypassrls, r.rolsuper,
		       COALESCE(bool_or(has_table_privilege(current_user, c.oid, 'INSERT, UPDATE, DELETE, TRUNCATE')), false)
		FROM pg_roles r
		CROSS JOIN pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'pm'
		WHERE r.rolname = current_user AND c.relkind IN ('r', 'p')
		GROUP BY 1, 2, 3`).Scan(&readOnly, &bypass, &superuser, &writes)
	if err != nil {
		return fmt.Errorf("read the observer role limits: %w", err)
	}
	if readOnly != "on" || !bypass || superuser || writes {
		return fmt.Errorf("observer role is not a read-only reader of all rows (read_only=%s bypassrls=%t superuser=%t writes=%t)",
			readOnly, bypass, superuser, writes)
	}
	return nil
}

// countRows counts the rows of each observed table in one read-only snapshot.
// One deadline, the limit of one lane command, bounds the snapshot.
func (o *observer) countRows(parent context.Context) (map[string]int64, error) {
	ctx, cancel := context.WithTimeout(parent, laneCommandTimeout)
	defer cancel()
	tx, err := o.database.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin the observer snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	counts := make(map[string]int64, len(observedTables))
	for _, table := range observedTables {
		var count int64
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{"pm", table}.Sanitize()).Scan(&count); err != nil {
			return nil, fmt.Errorf("count pm.%s: %w", table, err)
		}
		counts[table] = count
	}
	return counts, nil
}

// close closes the observer connection and drops the observer role.
func (o *observer) close() error {
	if o == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if o.database != nil {
		_ = o.database.Close(ctx)
	}
	if o.role == "" {
		return nil
	}
	admin, err := pgx.Connect(ctx, o.adminDSN)
	if err != nil {
		return fmt.Errorf("connect the stack administrator to drop the observer role: %w", err)
	}
	defer func() { _ = admin.Close(context.Background()) }()
	name := pgx.Identifier{o.role}.Sanitize()
	// DROP OWNED also revokes the database and schema privileges of the role.
	for _, statement := range []string{"DROP OWNED BY " + name, "DROP ROLE " + name} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			return fmt.Errorf("drop the observer role: %w", err)
		}
	}
	return nil
}
