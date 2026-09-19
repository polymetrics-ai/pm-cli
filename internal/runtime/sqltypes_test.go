package runtime

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// These physical PostgreSQL cases use the explicitly selected owned stack
// fixture. They provision one isolated customer database, customer role and
// typed relation through a superuser connection and remove exactly those
// objects again; the PM application, Temporal and object-metadata databases are
// never read or mutated. Missing fixture inputs fail rather than skip.
const (
	sqlFixtureDatabasePrefix = "pm_sql_fixture_db_"
	sqlFixtureRolePrefix     = "pm_sql_fixture_role_"
	sqlFixtureSchema         = "customer"
	sqlFixtureRelation       = "records"
	sqlFixtureQuotedRelation = `records"shadow`
	sqlFixtureUnrelated      = "unrelated"
	sqlFixtureProbeSequence  = "records_probe_seq"
	sqlFixtureAdminUser      = "pm_stack_admin"
	sqlFixtureAdminDatabase  = "postgres"
	sqlFixtureServerName     = "127.0.0.1"
	// sqlExpectedRelationList is the canonical byte order (COLLATE "C") of the
	// relations this case provisions in the isolated customer schema.
	sqlExpectedRelationList  = sqlFixtureRelation + "," + sqlFixtureQuotedRelation + "," + sqlFixtureUnrelated
	sqlRelationListStatement = `SELECT string_agg(tablename, ',' ORDER BY tablename COLLATE "C") AS names FROM pg_catalog.pg_tables WHERE schemaname = '` + sqlFixtureSchema + `'`
)

// TestLosslessParameterizedSQL asserts V34 against the real fixture PostgreSQL.
func TestLosslessParameterizedSQL(t *testing.T) {
	fixture := openSQLFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	now := time.Now().UTC().Truncate(time.Microsecond)
	connection := fixture.connection()
	upsert := sqlTestOperation(t, contract.PostgreSQLStatementMappedUpsert, fixture.relation.Binding)
	read := sqlTestOperation(t, contract.PostgreSQLStatementSelect, fixture.relation.Binding)
	metadata := sqlTestOperation(t, contract.PostgreSQLStatementMetadata, fixture.relation.Binding)

	t.Run("declared_relation_binds_live_metadata", func(t *testing.T) {
		observation, err := fixture.execute(t, ctx, sqlTestCall(t, metadata, connection, fixture.relation, nil, sqlTestLimits(), now))
		if err != nil || observation.Outcome != SQLOutcomeKnownCommit {
			t.Fatalf("metadata observation=%#v error=%v", observation, err)
		}
		if len(observation.Rows) == 0 {
			t.Fatal("metadata statement returned no live relation columns")
		}
		if err := VerifySQLRelation(fixture.relation, observation.Rows); err != nil {
			t.Fatalf("declared relation does not bind the live relation: %v", err)
		}
		mismatched := fixture.relation
		mismatched.Columns = append([]SQLColumn(nil), fixture.relation.Columns...)
		mismatched.Columns[0].Kind = SQLText
		if err := VerifySQLRelation(mismatched, observation.Rows); err == nil {
			t.Fatal("a declared column kind was accepted against a different live type")
		}
	})

	t.Run("typed_row_round_trips_exactly", func(t *testing.T) {
		const instantText = "2026-09-19T05:00:00.123456+02:00"
		payload := []byte{0x00, 0x01, 0xff, 0xfe}
		const jsonDocument = `{"b": 1, "a": [1, 2]}`
		input := []SQLColumnValue{
			{Column: "record_id", Value: mustSQLInt8(t, "9007199254740993")},
			{Column: "amount", Value: mustSQLNumeric(t, "12345.6700", 9, 4)},
			{Column: "flagged", Value: SQLBooleanValue(true)},
			{Column: "note", Value: SQLTextValue("typed round trip")},
			{Column: "optional_note", Value: SQLNullValue(SQLText)},
			{Column: "observed_at", Value: mustSQLInstant(t, instantText)},
			{Column: "local_wall", Value: mustSQLLocalTimestamp(t, "2026-09-19 05:00:00.123456")},
			{Column: "payload", Value: SQLByteaValue(payload)},
			{Column: "token", Value: mustSQLUUID(t, "3f2504e0-4f89-11d3-9a0c-0305e82c3301")},
			{Column: "document_json", Value: mustSQLJSON(t, SQLJSON, jsonDocument)},
			{Column: "document", Value: mustSQLJSON(t, SQLJSONB, jsonDocument)},
			// probe_id is deliberately absent: the declared column default must
			// apply, which advances the non-transactional sequence once.
		}
		observation, err := fixture.execute(t, ctx, sqlTestCall(t, upsert, connection, fixture.relation, input, sqlTestLimits(), now))
		if err != nil || observation.Outcome != SQLOutcomeKnownCommit {
			t.Fatalf("typed upsert observation=%#v error=%v", observation, err)
		}
		if strings.Contains(observation.Statement, "typed round trip") || !strings.Contains(observation.Statement, "CAST($1 AS bigint)") {
			t.Fatalf("built statement interpolated caller data: %s", observation.Statement)
		}
		exact := fixture.selectRow(t, ctx, read, connection, now, "9007199254740993")
		if number, ok := sqlValueOf(t, exact, "record_id").Number(); !ok ||
			number != (contract.ExactNumber{Text: "9007199254740993", Precision: 16, Scale: 0}) {
			t.Fatalf("int8 lost its exact value: %#v", number)
		}
		if number, ok := sqlValueOf(t, exact, "amount").Number(); !ok ||
			number != (contract.ExactNumber{Text: "12345.6700", Precision: 9, Scale: 4}) {
			t.Fatalf("numeric(9,4) lost its declared scale: %#v", number)
		}
		if flagged, ok := sqlValueOf(t, exact, "flagged").Boolean(); !ok || !flagged {
			t.Fatalf("boolean did not round trip: %v", flagged)
		}
		if note, ok := sqlValueOf(t, exact, "note").Text(); !ok || note != "typed round trip" {
			t.Fatalf("text did not round trip: %q", note)
		}
		if nullNote := sqlValueOf(t, exact, "optional_note"); !nullNote.IsNull() || nullNote.IsAbsent() {
			t.Fatalf("explicit typed null became %#v", nullNote)
		}
		instant, ok := sqlValueOf(t, exact, "observed_at").Instant()
		want, parseErr := time.Parse(time.RFC3339, instantText)
		if !ok || parseErr != nil || !instant.Equal(want) {
			t.Fatalf("timestamptz instant %v did not retain %v (error=%v)", instant, want, parseErr)
		}
		if text, _ := sqlValueOf(t, exact, "observed_at").Text(); !strings.Contains(text, "T") ||
			!(strings.HasSuffix(text, "Z") || strings.ContainsAny(text[19:], "+-")) {
			t.Fatalf("timestamptz lost its explicit offset: %q", text)
		}
		if local, ok := sqlValueOf(t, exact, "local_wall").Text(); !ok || local != "2026-09-19 05:00:00.123456" {
			t.Fatalf("timestamp without time zone was coerced: %q", local)
		}
		if raw, ok := sqlValueOf(t, exact, "payload").Bytes(); !ok || !bytes.Equal(raw, payload) {
			t.Fatalf("bytea did not round trip: %x", raw)
		}
		if token, ok := sqlValueOf(t, exact, "token").Text(); !ok || token != "3f2504e0-4f89-11d3-9a0c-0305e82c3301" {
			t.Fatalf("uuid did not round trip: %q", token)
		}
		if document, ok := sqlValueOf(t, exact, "document_json").Text(); !ok || document != jsonDocument {
			t.Fatalf("json text changed: %q", document)
		}
		if jsonb, ok := sqlValueOf(t, exact, "document").Text(); !ok || !sqlJSONEqual(t, jsonb, jsonDocument) {
			t.Fatalf("jsonb document changed: %q", jsonb)
		}
		if number, ok := sqlValueOf(t, exact, "probe_id").Number(); !ok || number.Text != "1" {
			t.Fatalf("absent column did not apply its declared default: %#v", number)
		}
		// A different session timezone must not change the local wall clock,
		// while the instant keeps its own offset rendering.
		fixture.adminExec(t, ctx, `SET timezone = 'Asia/Kolkata'`)
		localized := fixture.adminRow(t, ctx, `SELECT "local_wall"::text AS local_wall, "observed_at"::text AS observed_at FROM "customer"."records" WHERE "record_id" = 9007199254740993`)
		if *localized["local_wall"] != "2026-09-19 05:00:00.123456" || !strings.Contains(*localized["observed_at"], "+05:30") {
			t.Fatalf("session timezone changed typed temporal values: %v", localized)
		}
		fixture.adminExec(t, ctx, `SET timezone = 'UTC'`)
	})

	t.Run("injection_cannot_alter_declared_query_or_scope", func(t *testing.T) {
		const payload = `'; DROP TABLE "customer"."unrelated"; --`
		before := fixture.adminRow(t, ctx, `SELECT count(*)::text AS rows, min("note") AS note FROM "customer"."unrelated"`)
		relations := fixture.adminRow(t, ctx, sqlRelationListStatement)
		input := []SQLColumnValue{
			{Column: "record_id", Value: mustSQLInt8(t, "7")},
			{Column: "amount", Value: mustSQLNumeric(t, "1.0000", 9, 4)},
			{Column: "flagged", Value: SQLBooleanValue(false)},
			{Column: "note", Value: SQLTextValue(payload)},
			{Column: "observed_at", Value: mustSQLInstant(t, "2026-09-19T03:00:00Z")},
			{Column: "local_wall", Value: mustSQLLocalTimestamp(t, "2026-09-19 03:00:00")},
			{Column: "payload", Value: SQLByteaValue([]byte(payload))},
			{Column: "document_json", Value: mustSQLJSON(t, SQLJSON, `{"payload": "'; DROP TABLE \"customer\".\"unrelated\"; --"}`)},
			// An explicit typed null must not evaluate the column default.
			{Column: "probe_id", Value: SQLNullValue(SQLInt8)},
		}
		statement, err := BuildSQLStatement(upsert, fixture.relation, input, 2)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(statement.Text, "DROP TABLE") || !strings.Contains(statement.Text, `"customer"."`+sqlFixtureRelation+`"`) {
			t.Fatalf("caller data reached the statement text: %s", statement.Text)
		}
		observation, err := fixture.execute(t, ctx, sqlTestCall(t, upsert, connection, fixture.relation, input, sqlTestLimits(), now))
		if err != nil || observation.Outcome != SQLOutcomeKnownCommit {
			t.Fatalf("injection upsert observation=%#v error=%v", observation, err)
		}
		injected := fixture.selectRow(t, ctx, read, connection, now, "7")
		if note, _ := sqlValueOf(t, injected, "note").Text(); note != payload {
			t.Fatalf("payload was not retained as data: %q", note)
		}
		if raw, _ := sqlValueOf(t, injected, "payload").Bytes(); !bytes.Equal(raw, []byte(payload)) {
			t.Fatalf("bytea payload was not retained as data: %q", raw)
		}
		if !sqlValueOf(t, injected, "probe_id").IsNull() {
			t.Fatal("explicit typed null evaluated the column default")
		}
		after := fixture.adminRow(t, ctx, `SELECT count(*)::text AS rows, min("note") AS note FROM "customer"."unrelated"`)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("unrelated relation changed: before=%v after=%v", before, after)
		}
		unchanged := fixture.adminRow(t, ctx, sqlRelationListStatement)
		if !reflect.DeepEqual(relations, unchanged) {
			t.Fatalf("injected value created or removed a relation: before=%v after=%v", relations, unchanged)
		}
		names := *unchanged["names"]
		if strings.Contains(strings.ToLower(names), "ledger") || names != sqlExpectedRelationList {
			t.Fatalf("unmanaged target gained an undeclared relation: %q", names)
		}
		sequence := fixture.adminRow(t, ctx, `SELECT last_value::text AS last_value FROM "customer"."records_probe_seq"`)
		if *sequence["last_value"] != "1" {
			t.Fatalf("explicit typed null advanced the declared default: %q", *sequence["last_value"])
		}
		customer := fixture.customerConnection(t, ctx)
		if _, err := customer.Exec(ctx, `SELECT * FROM "customer"."unrelated"`).ReadAll(); !sqlPermissionDenied(t, err) {
			t.Fatalf("customer role read an unrelated relation: %v", err)
		}
		for _, name := range []string{"", `customer.records`, "records\x00", strings.Repeat("a", 64), " leading", "trailing ", "line\nbreak"} {
			if _, err := QuoteSQLIdentifier(name); err == nil {
				t.Fatalf("hostile identifier %q was accepted", name)
			}
		}
		quoted, err := QuoteSQLIdentifier(sqlFixtureQuotedRelation)
		if err != nil || quoted != `"records""shadow"` {
			t.Fatalf("embedded quote in an identifier was not doubled: %q error=%v", quoted, err)
		}
		if _, err := SQLInt8Value("1; DROP TABLE \"customer\".\"unrelated\"; --"); err == nil {
			t.Fatal("hostile integer text was accepted as a typed value")
		}
		if _, err := SQLUUIDValue(payload); err == nil {
			t.Fatal("hostile uuid text was accepted as a typed value")
		}
		if _, err := BuildSQLStatement(read, fixture.relation, []SQLColumnValue{
			{Column: "note", Value: SQLTextValue(payload)},
		}, 2); err == nil {
			t.Fatal("an undeclared key column was accepted as a bounded filter")
		}
	})

	t.Run("quoted_identifier_targets_the_declared_relation", func(t *testing.T) {
		quoted := SQLRelation{
			Binding: "customer-records-quoted",
			Schema:  sqlFixtureSchema,
			Name:    sqlFixtureQuotedRelation,
			Columns: []SQLColumn{
				{Name: "record_id", Kind: SQLInt8, Key: true},
				{Name: "note", Kind: SQLText},
			},
		}
		operation := sqlTestOperation(t, contract.PostgreSQLStatementSelect, quoted.Binding)
		observation, err := fixture.execute(t, ctx, sqlTestCall(t, operation, connection, quoted, nil, sqlTestLimits(), now))
		if err != nil || observation.Outcome != SQLOutcomeKnownCommit {
			t.Fatalf("quoted relation observation=%#v error=%v", observation, err)
		}
		if !strings.Contains(observation.Statement, `"customer"."records""shadow"`) || len(observation.Rows) != 1 {
			t.Fatalf("quoted relation was not addressed exactly: %s rows=%#v", observation.Statement, observation.Rows)
		}
		if note, _ := sqlValueOf(t, observation.Rows[0], "note").Text(); note != "quoted identifier canary" {
			t.Fatalf("quoted relation returned %q", note)
		}
	})

	t.Run("confirmed_rollback_is_retry_safe", func(t *testing.T) {
		input := []SQLColumnValue{
			{Column: "record_id", Value: mustSQLInt8(t, "11")},
			{Column: "amount", Value: mustSQLNumeric(t, "2.0000", 9, 4)},
			{Column: "flagged", Value: SQLBooleanValue(true)},
			// The NOT NULL constraint refuses this statement after the send.
			{Column: "note", Value: SQLNullValue(SQLText)},
			{Column: "observed_at", Value: mustSQLInstant(t, "2026-09-19T04:00:00Z")},
			{Column: "local_wall", Value: mustSQLLocalTimestamp(t, "2026-09-19 04:00:00")},
		}
		observation, err := fixture.execute(t, ctx, sqlTestCall(t, upsert, connection, fixture.relation, input, sqlTestLimits(), now))
		if err == nil {
			t.Fatal("a refused statement was reported as an accepted effect")
		}
		if errors.Is(err, ErrOutcomeUnknown) || observation.Outcome != SQLOutcomeKnownRollback || !observation.Retry.Eligible || !observation.PhysicalSend {
			t.Fatalf("confirmed rollback observation=%#v error=%v", observation, err)
		}
		committed := fixture.adminRow(t, ctx, `SELECT count(*)::text AS rows FROM "customer"."records" WHERE "record_id" = 11`)
		if *committed["rows"] != "0" {
			t.Fatal("a rolled-back statement left a committed row")
		}
	})

	t.Run("lost_commit_reply_stays_unknown", func(t *testing.T) {
		input := []SQLColumnValue{
			{Column: "record_id", Value: mustSQLInt8(t, "13")},
			{Column: "amount", Value: mustSQLNumeric(t, "3.0000", 9, 4)},
			{Column: "flagged", Value: SQLBooleanValue(true)},
			{Column: "note", Value: SQLTextValue("committed without a reply")},
			{Column: "observed_at", Value: mustSQLInstant(t, "2026-09-19T06:00:00Z")},
			{Column: "local_wall", Value: mustSQLLocalTimestamp(t, "2026-09-19 06:00:00")},
		}
		probe := &sqlProtocolProbe{fault: &sqlCommitFault{}}
		observed := false
		probe.fault.observe = func() error {
			deadline := time.Now().Add(20 * time.Second)
			for {
				row := fixture.adminRow(t, ctx, `SELECT count(*)::text AS rows FROM "customer"."records" WHERE "record_id" = 13`)
				if *row["rows"] == "1" {
					observed = true
					return nil
				}
				if time.Now().After(deadline) {
					return errors.New("independent observer never saw the accepted row")
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
		observation, err := fixture.executeWith(t, ctx, sqlTestCall(t, upsert, connection, fixture.relation, input, sqlTestLimits(), now), probe)
		if err == nil || !errors.Is(err, ErrOutcomeUnknown) {
			t.Fatalf("lost COMMIT reply observation=%#v error=%v", observation, err)
		}
		if observation.Outcome != SQLOutcomeUnknownCommit || observation.Retry.Eligible || observation.Retry.RequiresCondition {
			t.Fatalf("lost COMMIT reply was not retained as an unknown, non-retryable outcome: %#v", observation)
		}
		if !observed || !probe.fault.completed() {
			t.Fatal("the withheld reply did not follow an independently observed accepted commit")
		}
		if statements := probe.statementTexts(); len(statements) != 1 {
			t.Fatalf("a possibly committed statement was retried: %v", statements)
		}
	})
}

// verifyScopeForStrictTLS asserts that the isolated customer role keeps only
// the declared relation privileges inside its own isolated database.
func verifyScopeForStrictTLS(t *testing.T, ctx context.Context, fixture *sqlFixture, customer *pgconn.PgConn) {
	t.Helper()
	if _, err := customer.Exec(ctx, `CREATE TABLE "customer"."sneaky" ("id" int8)`).ReadAll(); !sqlPermissionDenied(t, err) {
		t.Fatalf("customer role could create a relation: %v", err)
	}
	if _, err := customer.Exec(ctx, `SELECT * FROM "customer"."unrelated"`).ReadAll(); !sqlPermissionDenied(t, err) {
		t.Fatalf("customer role could read an unrelated relation: %v", err)
	}
	if _, err := customer.Exec(ctx, `DELETE FROM "customer"."records"`).ReadAll(); !sqlPermissionDenied(t, err) {
		t.Fatalf("customer role could delete declared rows: %v", err)
	}
	tables := fixture.adminRow(t, ctx, sqlRelationListStatement)
	if *tables["names"] != sqlExpectedRelationList {
		t.Fatalf("isolated database gained an undeclared relation: %q", *tables["names"])
	}
}

// --- fixture helpers ---

type sqlFixture struct {
	t                       *testing.T
	port                    uint16
	caPEM                   []byte
	serverCertificateSHA256 string
	adminPassword           string
	cluster                 *pgconn.PgConn // superuser connection to the administrative database
	owner                   *pgconn.PgConn // superuser connection to the isolated customer database
	database                string
	role                    string
	rolePassword            string
	relation                SQLRelation
}

func openSQLFixture(t *testing.T) *sqlFixture {
	t.Helper()
	secretsDirectory := os.Getenv("PM_STORE_TEST_SECRETS_DIR")
	if secretsDirectory == "" {
		t.Fatal("this physical PostgreSQL case requires the explicitly selected owned stack fixture: PM_STORE_TEST_SECRETS_DIR is required and missing inputs fail rather than skip")
	}
	portValue := os.Getenv("PM_STACK_POSTGRES_PORT")
	port, err := strconv.Atoi(portValue)
	if portValue == "" || err != nil || port < 1024 || port > 65535 {
		t.Fatalf("PM_STACK_POSTGRES_PORT must be the selected non-privileged fixture port: %q", portValue)
	}
	absolute, err := filepath.Abs(secretsDirectory)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &sqlFixture{
		t:             t,
		port:          uint16(port),
		caPEM:         readSQLSecret(t, absolute, "ca.pem"),
		adminPassword: string(readSQLSecret(t, absolute, "superuser-password")),
		database:      sqlFixtureDatabasePrefix + randomSQLToken(t, 6),
		role:          sqlFixtureRolePrefix + randomSQLToken(t, 6),
		rolePassword:  randomSQLToken(t, 24),
	}
	serverCertificate := readSQLSecret(t, absolute, "server.pem")
	block, _ := pem.Decode(serverCertificate)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("fixture server certificate is not a PEM certificate")
	}
	digest := sha256.Sum256(block.Bytes)
	fixture.serverCertificateSHA256 = hex.EncodeToString(digest[:])
	fixture.relation = SQLRelation{
		Binding: "customer-records",
		Schema:  sqlFixtureSchema,
		Name:    sqlFixtureRelation,
		Columns: []SQLColumn{
			{Name: "record_id", Kind: SQLInt8, Key: true},
			{Name: "amount", Kind: SQLNumeric, Precision: 9, Scale: 4},
			{Name: "flagged", Kind: SQLBoolean},
			{Name: "note", Kind: SQLText},
			{Name: "optional_note", Kind: SQLText},
			{Name: "observed_at", Kind: SQLTimestamptz},
			{Name: "local_wall", Kind: SQLTimestamp},
			{Name: "payload", Kind: SQLBytea},
			{Name: "token", Kind: SQLUUID},
			{Name: "document_json", Kind: SQLJSON},
			{Name: "document", Kind: SQLJSONB},
			{Name: "probe_id", Kind: SQLInt8},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	t.Cleanup(func() { fixture.cleanup() })
	fixture.cluster = fixture.connect(t, ctx, sqlFixtureAdminUser, fixture.adminPassword, sqlFixtureAdminDatabase)
	role := quoteSQLIdentifierForTest(t, fixture.role)
	database := quoteSQLIdentifierForTest(t, fixture.database)
	fixture.clusterExec(t, ctx, `CREATE ROLE `+role+` LOGIN PASSWORD '`+fixture.rolePassword+`'`)
	fixture.clusterExec(t, ctx, `CREATE DATABASE `+database)
	fixture.clusterExec(t, ctx, `REVOKE ALL ON DATABASE `+database+` FROM PUBLIC`)
	fixture.clusterExec(t, ctx, `GRANT CONNECT ON DATABASE `+database+` TO `+role)
	fixture.owner = fixture.connect(t, ctx, sqlFixtureAdminUser, fixture.adminPassword, fixture.database)
	for _, statement := range append(fixture.provisionStatements(), fixture.grants()...) {
		fixture.adminExec(t, ctx, statement)
	}
	return fixture
}

func (fixture *sqlFixture) provisionStatements() []string {
	relation := `"` + sqlFixtureSchema + `"."` + sqlFixtureRelation + `"`
	return []string{
		`CREATE SCHEMA "` + sqlFixtureSchema + `"`,
		`REVOKE ALL ON SCHEMA "` + sqlFixtureSchema + `" FROM PUBLIC`,
		`CREATE SEQUENCE "` + sqlFixtureSchema + `"."` + sqlFixtureProbeSequence + `"`,
		`CREATE TABLE ` + relation + ` (` +
			`"record_id" int8 PRIMARY KEY,` +
			`"amount" numeric(9,4) NOT NULL,` +
			`"flagged" boolean NOT NULL,` +
			`"note" text NOT NULL,` +
			`"optional_note" text,` +
			`"observed_at" timestamptz NOT NULL,` +
			`"local_wall" timestamp NOT NULL,` +
			`"payload" bytea,` +
			`"token" uuid,` +
			`"document_json" json,` +
			`"document" jsonb,` +
			`"probe_id" int8 DEFAULT nextval('"` + sqlFixtureSchema + `"."` + sqlFixtureProbeSequence + `"')` +
			`)`,
		`CREATE TABLE "` + sqlFixtureSchema + `"."records""shadow" ("record_id" int8 PRIMARY KEY, "note" text NOT NULL)`,
		`INSERT INTO "` + sqlFixtureSchema + `"."records""shadow" ("record_id", "note") VALUES (1, 'quoted identifier canary')`,
		`CREATE TABLE "` + sqlFixtureSchema + `"."` + sqlFixtureUnrelated + `" ("record_id" int8 PRIMARY KEY, "note" text NOT NULL)`,
		`INSERT INTO "` + sqlFixtureSchema + `"."` + sqlFixtureUnrelated + `" ("record_id", "note") VALUES (1, 'untouched canary')`,
	}
}

func (fixture *sqlFixture) grants() []string {
	return []string{
		`GRANT USAGE ON SCHEMA "` + sqlFixtureSchema + `" TO ` + quoteSQLIdentifierForTest(fixture.t, fixture.role),
		`GRANT SELECT, INSERT, UPDATE ON "` + sqlFixtureSchema + `"."` + sqlFixtureRelation + `" TO ` + quoteSQLIdentifierForTest(fixture.t, fixture.role),
		`GRANT SELECT ON "` + sqlFixtureSchema + `"."records""shadow" TO ` + quoteSQLIdentifierForTest(fixture.t, fixture.role),
		`GRANT USAGE, SELECT ON SEQUENCE "` + sqlFixtureSchema + `"."` + sqlFixtureProbeSequence + `" TO ` + quoteSQLIdentifierForTest(fixture.t, fixture.role),
	}
}

func (fixture *sqlFixture) connection() SQLConnection {
	return SQLConnection{
		Host:     sqlFixtureServerName,
		Port:     fixture.port,
		Database: fixture.database,
		User:     fixture.role,
		TLS:      SQLTLSConfig{CACertificatePEM: fixture.caPEM, ServerName: sqlFixtureServerName},
	}
}

func (fixture *sqlFixture) connect(t *testing.T, ctx context.Context, user, password, database string) *pgconn.PgConn {
	t.Helper()
	tlsConfig, err := NewStrictSQLTLS(SQLTLSConfig{CACertificatePEM: fixture.caPEM, ServerName: sqlFixtureServerName})
	if err != nil {
		t.Fatalf("strict fixture TLS: %v", err)
	}
	config, err := buildSQLConnectConfig(SQLConnection{
		Host: sqlFixtureServerName, Port: fixture.port, Database: database, User: user,
		TLS: SQLTLSConfig{CACertificatePEM: fixture.caPEM, ServerName: sqlFixtureServerName},
	}, sqlTestLimits(), tlsConfig, nil, nil)
	if err != nil {
		t.Fatalf("fixture connection configuration: %v", err)
	}
	config.Password = password
	connection, err := pgconn.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect fixture role %q to %q: %v", user, database, err)
	}
	if err := sqlWireTLSWitness(t, connection); err != nil {
		t.Fatalf("fixture connection is not strictly encrypted: %v", err)
	}
	return connection
}

func sqlWireTLSWitness(t *testing.T, connection *pgconn.PgConn) error {
	t.Helper()
	witness, err := ObserveStrictSQLTLS(connection.Conn(), sqlFixtureServerName, time.Now().UTC())
	if err != nil {
		return err
	}
	if witness.ProtocolVersion < minimumSQLTLSVersion || witness.CipherSuite == 0 || len(witness.PeerCertificateSHA256) != 64 {
		return errors.New("fixture TLS witness is incomplete")
	}
	return nil
}

func (fixture *sqlFixture) customerConnection(t *testing.T, ctx context.Context) *pgconn.PgConn {
	t.Helper()
	connection := fixture.connect(t, ctx, fixture.role, fixture.rolePassword, fixture.database)
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	return connection
}

// execute runs one call with a healthy current admitter and a one-use
// credential resolver, and records the exact admitted execution.
func (fixture *sqlFixture) execute(t *testing.T, ctx context.Context, call SQLCall) (SQLObservation, error) {
	t.Helper()
	return fixture.executeWith(t, ctx, call, &sqlProtocolProbe{})
}

func (fixture *sqlFixture) executeWith(t *testing.T, ctx context.Context, call SQLCall, probe *sqlProtocolProbe) (SQLObservation, error) {
	t.Helper()
	admitter := &testAdmitter{}
	resolver := &staticCredentialResolver{secret: []byte(fixture.rolePassword), call: call.Call}
	dependencies := SQLDependencies{
		Admitter:    admitter,
		Credentials: resolver,
		Now:         func() time.Time { return time.Now().UTC() },
	}
	dependencies.frontend = probe.frontend
	observation, err := SQLTransport{Dependencies: dependencies}.Execute(ctx, call)
	if resolver.calls.Load() > 1 {
		t.Fatalf("credential material was resolved %d times for one attempt", resolver.calls.Load())
	}
	return observation, err
}

func (fixture *sqlFixture) selectAll(t *testing.T, ctx context.Context, operation contract.Operation, connection SQLConnection, now time.Time) []SQLRow {
	t.Helper()
	observation, err := fixture.execute(t, ctx, sqlTestCall(t, operation, connection, fixture.relation, nil, sqlTestLimits(), now))
	if err != nil {
		t.Fatalf("bounded select: %v", err)
	}
	return observation.Rows
}

func (fixture *sqlFixture) selectRow(t *testing.T, ctx context.Context, operation contract.Operation, connection SQLConnection, now time.Time, key string) SQLRow {
	t.Helper()
	filter := []SQLColumnValue{{Column: "record_id", Value: mustSQLInt8(t, key)}}
	observation, err := fixture.execute(t, ctx, sqlTestCall(t, operation, connection, fixture.relation, filter, sqlTestLimits(), now))
	if err != nil {
		t.Fatalf("keyed select %s: %v", key, err)
	}
	if len(observation.Rows) != 1 {
		t.Fatalf("keyed select %s returned %d rows: %#v", key, len(observation.Rows), observation.Rows)
	}
	return observation.Rows[0]
}

func (fixture *sqlFixture) clusterExec(t *testing.T, ctx context.Context, statement string) {
	t.Helper()
	if err := sqlExecute(ctx, fixture.cluster, statement); err != nil {
		t.Fatalf("fixture administrative statement %q: %v", statement, err)
	}
}

func (fixture *sqlFixture) adminExec(t *testing.T, ctx context.Context, statement string) {
	t.Helper()
	if err := sqlExecute(ctx, fixture.owner, statement); err != nil {
		t.Fatalf("fixture statement %q: %v", statement, err)
	}
}

func (fixture *sqlFixture) adminRow(t *testing.T, ctx context.Context, statement string) map[string]*string {
	t.Helper()
	rows := fixture.adminRows(t, ctx, statement)
	if len(rows) != 1 {
		t.Fatalf("fixture observation %q returned %d rows", statement, len(rows))
	}
	return rows[0]
}

func (fixture *sqlFixture) adminRows(t *testing.T, ctx context.Context, statement string) []map[string]*string {
	t.Helper()
	rows, err := sqlQuery(ctx, fixture.owner, statement)
	if err != nil {
		t.Fatalf("fixture observation %q: %v", statement, err)
	}
	return rows
}

func (fixture *sqlFixture) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if fixture.owner != nil {
		_ = fixture.owner.Close(ctx)
		fixture.owner = nil
	}
	if fixture.cluster == nil {
		return
	}
	defer func() { _ = fixture.cluster.Close(ctx) }()
	role := quoteSQLIdentifierForTest(fixture.t, fixture.role)
	database := quoteSQLIdentifierForTest(fixture.t, fixture.database)
	if err := sqlExecute(ctx, fixture.cluster, `DROP DATABASE IF EXISTS `+database+` WITH (FORCE)`); err != nil {
		fixture.t.Errorf("isolated fixture database cleanup failed: %v", err)
		return
	}
	if err := sqlExecute(ctx, fixture.cluster, `DROP ROLE IF EXISTS `+role); err != nil {
		fixture.t.Errorf("isolated fixture role cleanup failed: %v", err)
		return
	}
	for _, statement := range []string{
		`SELECT count(*)::text AS remaining FROM pg_catalog.pg_database WHERE datname = '` + fixture.database + `'`,
		`SELECT count(*)::text AS remaining FROM pg_catalog.pg_roles WHERE rolname = '` + fixture.role + `'`,
	} {
		rows, err := sqlQuery(ctx, fixture.cluster, statement)
		if err != nil || len(rows) != 1 || *rows[0]["remaining"] != "0" {
			fixture.t.Errorf("isolated fixture object was not removed: %q error=%v rows=%v", statement, err, rows)
		}
	}
}

func sqlExecute(ctx context.Context, connection *pgconn.PgConn, statement string) error {
	results, err := connection.Exec(ctx, statement).ReadAll()
	if err != nil {
		return err
	}
	for _, result := range results {
		if result.Err != nil {
			return result.Err
		}
	}
	return nil
}

func sqlQuery(ctx context.Context, connection *pgconn.PgConn, statement string) ([]map[string]*string, error) {
	reader := connection.ExecParams(ctx, statement, nil, nil, nil, nil)
	result := reader.Read()
	if _, err := reader.Close(); err != nil {
		return nil, err
	}
	if result.Err != nil {
		return nil, result.Err
	}
	rows := make([]map[string]*string, 0, len(result.Rows))
	for _, values := range result.Rows {
		row := make(map[string]*string, len(result.FieldDescriptions))
		for index, field := range result.FieldDescriptions {
			if index >= len(values) || values[index] == nil {
				row[field.Name] = nil
				continue
			}
			text := string(values[index])
			row[field.Name] = &text
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func sqlPermissionDenied(t *testing.T, err error) bool {
	t.Helper()
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42501"
}

func readSQLSecret(t *testing.T, directory, name string) []byte {
	t.Helper()
	value, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil {
		t.Fatalf("read fixture secret %q: %v", name, err)
	}
	value = bytes.TrimRight(value, "\r\n")
	if len(value) == 0 {
		t.Fatalf("fixture secret %q is empty", name)
	}
	return value
}

func randomSQLToken(t *testing.T, length int) string {
	t.Helper()
	buffer := make([]byte, length)
	if _, err := rand.Read(buffer); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buffer)
}

func quoteSQLIdentifierForTest(t *testing.T, name string) string {
	t.Helper()
	quoted, err := QuoteSQLIdentifier(name)
	if err != nil {
		t.Fatalf("quote fixture identifier %q: %v", name, err)
	}
	return quoted
}

// --- typed value and call helpers ---

func sqlTestLimits() SQLLimits {
	return SQLLimits{
		MaxConnectDuration:   30 * time.Second,
		MaxStatementDuration: 30 * time.Second,
		MaxTotalDuration:     time.Minute,
		MaxRows:              64,
		MaxBytes:             1 << 20,
		AttemptNumber:        1,
		MaxAttempts:          3,
	}
}

func sqlTestOperation(t *testing.T, kind contract.PostgreSQLStatementKind, binding string) contract.Operation {
	t.Helper()
	return contract.Operation{
		SchemaVersion: contract.OperationSchemaVersion,
		OperationID:   "fixture.sql." + string(kind),
		SourceRefs:    testSourceRefs("#/operation"),
		Transport: contract.Transport{
			Kind: contract.TransportKindPostgreSQL, StatementKind: kind, RelationBinding: binding,
		},
		Inputs:          []contract.FieldBinding{},
		RequestVariants: []contract.RequestVariant{},
		Responses: []contract.Response{{
			Status: "success", MediaType: "application/json",
			Schema:     &contract.Schema{Type: contract.SchemaTypeArray, Items: &contract.Schema{Type: contract.SchemaTypeObject, Properties: map[string]contract.Schema{}}},
			SourceRefs: testSourceRefs("#/responses/success"),
		}},
		CapabilityRequirements: []contract.CapabilityRequirement{},
		LaneEligibility:        []contract.LaneEligibility{{Lane: contract.LaneDirectRead, Status: contract.LaneEligible, Reason: "fixture"}},
	}
}

func sqlPhysicalCall(t *testing.T, operationID string, now time.Time) PhysicalCall {
	t.Helper()
	dispatchID, err := contract.NewDispatchID("dispatch-" + operationID)
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := contract.NewAttemptID("attempt-" + operationID)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := contract.CanonicalDigest([]byte(`{"fixture":true}`))
	if err != nil {
		t.Fatal(err)
	}
	return PhysicalCall{
		Scope:        contract.Scope{OrgID: "org-sql-fixture", WorkspaceID: "workspace-sql-fixture"},
		ConnectionID: "connection-sql-fixture", OperationID: operationID, OperationRevision: "operation-revision-sql-fixture",
		RunID: "run-sql-fixture", TaskID: "task-sql-fixture", ExecutionGrantRef: "grant-sql-fixture", GrantEpoch: 1,
		DispatchID: dispatchID,
		Attempt: contract.Attempt{
			AttemptID: attemptID, DispatchID: dispatchID, ReceiptRef: "receipt-sql-fixture",
			RecordedAt: now.Add(-time.Second).UTC(),
		},
		PermitRef: "permit-sql-fixture", AccountWitness: "account-sql-fixture",
		Budget:   contract.BudgetBinding{BudgetRef: "budget-sql-fixture", Revision: 1, Digest: digest},
		Capacity: contract.CapacityReservation{OutcomeSlots: 1, EncodedBytes: 1 << 20},
		Deadline: now.Add(2 * time.Minute).UTC(),
	}
}

func sqlTestCall(
	t *testing.T,
	operation contract.Operation,
	connection SQLConnection,
	relation SQLRelation,
	input []SQLColumnValue,
	limits SQLLimits,
	now time.Time,
) SQLCall {
	t.Helper()
	return SQLCall{
		Operation: operation, Connection: connection, Relation: relation,
		Call: sqlPhysicalCall(t, operation.OperationID, now), Input: input, Limits: limits,
	}
}

func mustSQLInt8(t *testing.T, exact string) SQLValue {
	t.Helper()
	value, err := SQLInt8Value(exact)
	if err != nil {
		t.Fatalf("SQLInt8Value(%q): %v", exact, err)
	}
	return value
}

func mustSQLNumeric(t *testing.T, exact string, precision, scale int) SQLValue {
	t.Helper()
	value, err := SQLNumericValue(exact, precision, scale)
	if err != nil {
		t.Fatalf("SQLNumericValue(%q, %d, %d): %v", exact, precision, scale, err)
	}
	return value
}

func mustSQLInstant(t *testing.T, exact string) SQLValue {
	t.Helper()
	value, err := SQLInstantValue(exact)
	if err != nil {
		t.Fatalf("SQLInstantValue(%q): %v", exact, err)
	}
	return value
}

func mustSQLLocalTimestamp(t *testing.T, exact string) SQLValue {
	t.Helper()
	value, err := SQLLocalTimestampValue(exact)
	if err != nil {
		t.Fatalf("SQLLocalTimestampValue(%q): %v", exact, err)
	}
	return value
}

func mustSQLUUID(t *testing.T, exact string) SQLValue {
	t.Helper()
	value, err := SQLUUIDValue(exact)
	if err != nil {
		t.Fatalf("SQLUUIDValue(%q): %v", exact, err)
	}
	return value
}

func mustSQLJSON(t *testing.T, kind SQLValueKind, exact string) SQLValue {
	t.Helper()
	value, err := SQLJSONValue(kind, exact)
	if err != nil {
		t.Fatalf("SQLJSONValue(%q, %q): %v", kind, exact, err)
	}
	return value
}

func sqlValueOf(t *testing.T, row SQLRow, column string) SQLValue {
	t.Helper()
	value, ok := row.Value(column)
	if !ok {
		t.Fatalf("row does not contain column %q: %#v", column, row.Columns)
	}
	return value
}

func sqlJSONEqual(t *testing.T, left, right string) bool {
	t.Helper()
	var leftValue, rightValue any
	if err := json.Unmarshal([]byte(left), &leftValue); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(right), &rightValue); err != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

// --- protocol probe helpers ---

type sqlProtocolProbe struct {
	mu         sync.Mutex
	buffer     []byte
	statements []string
	begins     int
	commits    int
	rollbacks  int
	typed      bool
	fault      *sqlCommitFault
}

// sqlStartupProtocolVersion is the PostgreSQL protocol 3.0 startup version.
const sqlStartupProtocolVersion = 0x00030000

type sqlCommitFault struct {
	sent     atomic.Bool
	dropped  atomic.Bool
	observed atomic.Bool
	observe  func() error
}

func (fault *sqlCommitFault) completed() bool {
	return fault != nil && fault.sent.Load() && fault.dropped.Load() && fault.observed.Load()
}

// frontend wraps the exact PostgreSQL protocol boundary above TLS so the case
// can count physical sends and withhold a real COMMIT acknowledgement.
func (probe *sqlProtocolProbe) frontend(reader io.Reader, writer io.Writer) *pgproto3.Frontend {
	return pgproto3.NewFrontend(&sqlProbeReader{reader: reader, probe: probe}, &sqlProbeWriter{writer: writer, probe: probe})
}

func (probe *sqlProtocolProbe) counters() (begins, statements, commits int) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	return probe.begins, len(probe.statements), probe.commits
}

func (probe *sqlProtocolProbe) statementTexts() []string {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	return append([]string(nil), probe.statements...)
}

func (probe *sqlProtocolProbe) observe(frontend []byte) {
	probe.mu.Lock()
	defer probe.mu.Unlock()
	probe.buffer = append(probe.buffer, frontend...)
	for {
		if !probe.typed {
			// The connection's first message is the untyped startup message:
			// an int32 length followed by the protocol version.
			if len(probe.buffer) < 8 {
				return
			}
			length := int(binary.BigEndian.Uint32(probe.buffer[0:4]))
			if length < 8 || length > 1<<20 ||
				binary.BigEndian.Uint32(probe.buffer[4:8]) != sqlStartupProtocolVersion {
				probe.typed = true
				continue
			}
			if length > len(probe.buffer) {
				return
			}
			probe.buffer = append(probe.buffer[:0], probe.buffer[length:]...)
			probe.typed = true
			continue
		}
		if len(probe.buffer) < 5 {
			return
		}
		length := int(binary.BigEndian.Uint32(probe.buffer[1:5])) + 1
		if length < 5 || length > 1<<20 {
			probe.buffer = nil
			return
		}
		if length > len(probe.buffer) {
			return
		}
		probe.classify(probe.buffer[:length])
		probe.buffer = append(probe.buffer[:0], probe.buffer[length:]...)
	}
}

func (probe *sqlProtocolProbe) classify(frame []byte) {
	var text string
	switch frame[0] {
	case 'Q':
		text = strings.TrimRight(string(frame[5:]), "\x00")
	case 'P':
		payload := frame[5:]
		nameEnd := bytes.IndexByte(payload, 0)
		if nameEnd < 0 {
			return
		}
		payload = payload[nameEnd+1:]
		textEnd := bytes.IndexByte(payload, 0)
		if textEnd < 0 {
			return
		}
		text = string(payload[:textEnd])
	default:
		return
	}
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "begin":
		probe.begins++
	case "commit":
		probe.commits++
		if probe.fault != nil {
			probe.fault.sent.Store(true)
		}
	case "rollback":
		probe.rollbacks++
	default:
		probe.statements = append(probe.statements, text)
	}
}

type sqlProbeWriter struct {
	writer io.Writer
	probe  *sqlProtocolProbe
}

func (writer *sqlProbeWriter) Write(data []byte) (int, error) {
	n, err := writer.writer.Write(data)
	if n > 0 {
		writer.probe.observe(data[:n])
	}
	return n, err
}

type sqlProbeReader struct {
	reader io.Reader
	probe  *sqlProtocolProbe
}

func (reader *sqlProbeReader) Read(destination []byte) (int, error) {
	n, err := reader.reader.Read(destination)
	fault := reader.probe.fault
	if fault == nil || !fault.sent.Load() || !fault.dropped.CompareAndSwap(false, true) {
		return n, err
	}
	if observeErr := fault.observe(); observeErr != nil {
		return 0, observeErr
	}
	fault.observed.Store(true)
	// The server accepted the commit; only the reply is lost.
	return 0, io.ErrUnexpectedEOF
}

// --- scope and boundary doubles ---

// stagedAdmitter refuses exactly one configured admission ordinal and records
// every re-authorization the transport requested.
type stagedAdmitter struct {
	failAt int64
	err    error
	calls  atomic.Int64
	scopes []contract.Scope
	mu     sync.Mutex
}

func (admitter *stagedAdmitter) AdmitPhysicalCall(ctx context.Context, call PhysicalCall) error {
	ordinal := admitter.calls.Add(1)
	admitter.mu.Lock()
	admitter.scopes = append(admitter.scopes, call.Scope)
	admitter.mu.Unlock()
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("unbounded context")
	}
	if admitter.failAt != 0 && ordinal == admitter.failAt {
		return admitter.err
	}
	return nil
}

// workspaceScopeAdmitter re-authorizes the exact admitted workspace scope the
// way the application boundary does: a call for another workspace is refused
// without touching the physical transport.
type workspaceScopeAdmitter struct {
	admitted contract.Scope
	calls    atomic.Int64
	seen     []contract.Scope
	mu       sync.Mutex
}

func (admitter *workspaceScopeAdmitter) AdmitPhysicalCall(ctx context.Context, call PhysicalCall) error {
	admitter.calls.Add(1)
	admitter.mu.Lock()
	admitter.seen = append(admitter.seen, call.Scope)
	admitter.mu.Unlock()
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("unbounded context")
	}
	if call.Scope != admitter.admitted {
		return errors.New("durable grant does not cover this workspace scope")
	}
	return nil
}

func (admitter *workspaceScopeAdmitter) observedScopes() []contract.Scope {
	admitter.mu.Lock()
	defer admitter.mu.Unlock()
	return append([]contract.Scope(nil), admitter.seen...)
}

// --- cryptographic test material for the strict TLS case ---

func sqlTestForeignCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "PMCLI unrelated test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// sqlTestLeafCertificate returns a self-signed end-entity certificate that must
// never be accepted as a trust anchor.
func sqlTestLeafCertificate(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "PMCLI unrelated test leaf"},
		DNSNames:              []string{"wrong-hostname.invalid"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// MARKER-PROBE
