package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/connectors"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/credentials"
	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	pmruntime "github.com/polymetrics-ai/pm-cli/internal/runtime"
	"github.com/polymetrics-ai/pm-cli/internal/schema"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

// This case composes the real PostgreSQL execution path end to end: it declares
// one server-owned customer connection through the strict configuration
// boundary, registers the shared SQL callback for the postgresql operations the
// loaded bundle registry declares, and executes them through the authenticated
// admission path and the production worker planner against an isolated customer
// database this case creates inside the owned stack fixture.
//
// The customer role, database, relation and trust anchor are separate from every
// PM, Temporal and object-store identity. No raw DSN, secret value or provider
// body field is declared, logged or asserted anywhere in this case.
const (
	sqlExecutionConnectorID     = "postgres"
	sqlExecutionBinding         = "postgres.reusable-relation"
	sqlExecutionMetadataID      = "relation/metadata"
	sqlExecutionSelectID        = "relation/select"
	sqlExecutionUpsertID        = "relation/mapped-upsert"
	sqlExecutionSchema          = "customer"
	sqlExecutionRelation        = "records"
	sqlExecutionDatabasePrefix  = "pm_sql_composition_db_"
	sqlExecutionRolePrefix      = "pm_sql_composition_role_"
	sqlExecutionAdminUser       = "pm_stack_admin"
	sqlExecutionAdminDatabase   = "postgres"
	sqlExecutionServerName      = "127.0.0.1"
	sqlExecutionResource        = "customer.records"
	sqlExecutionSourceRevision  = "postgres16-source-v1"
	sqlExecutionPermissionRev   = "permissions-v1"
	sqlExecutionTrustMember     = "ca.pem"
	sqlExecutionSuperuserMember = "superuser-password"
	sqlExecutionKeyRef          = "sql-execution-wrapping-key-v1"

	sqlExecutionMetadataNameColumn = "column_name"
	sqlExecutionMetadataTypeColumn = "data_type"

	sqlExecutionKeyText = "9007199254740993"
	// sqlExecutionAmountText is an exact decimal whose own precision (five
	// digits) is below the numeric(9,4) column's, with trailing zeros: the
	// column, not the value's digit count, decides whether it fits, and the
	// exact text survives both the bound write and the published read.
	sqlExecutionAmountText      = "1.2300"
	sqlExecutionAmountPrecision = 5
	sqlExecutionInstantText     = "2026-09-19T06:00:00.123456+02:00"
	sqlExecutionLocalText       = "2026-09-19T06:00:00.123456"
	sqlExecutionInstantUTC      = "2026-09-19T04:00:00.123456Z"

	sqlExecutionUpsertNote = "declared mapped upsert"
	sqlExecutionSelectNote = "seeded bounded select row"

	sqlExecutionResultBytes = 1 << 20
	sqlExecutionMaxRows     = 64
	sqlExecutionMaxBytes    = 65536
	sqlExecutionAttempts    = 2
	sqlExecutionDuration    = "20s"
	sqlExecutionStatement   = "10s"
	sqlExecutionConnect     = "5s"
)

// sqlExecutionColumn is one declared relation column: the live column the
// customer database creates, the transport type the declared relation binds,
// and the snapshot field the selected discovery records.
type sqlExecutionColumn struct {
	name      string
	ddl       string
	schema    contract.SchemaType
	kind      pmruntime.SQLValueKind
	precision int
	scale     int
	key       bool
	writable  bool
	required  bool
}

func sqlExecutionColumns() []sqlExecutionColumn {
	return []sqlExecutionColumn{
		{name: "record_id", ddl: `"record_id" bigint PRIMARY KEY`, schema: contract.SchemaTypeInteger, kind: pmruntime.SQLInt8, key: true, writable: true, required: true},
		{name: "amount", ddl: `"amount" numeric(9,4) NOT NULL`, schema: contract.SchemaTypeDecimal, kind: pmruntime.SQLNumeric, precision: 9, scale: 4, writable: true, required: true},
		{name: "flagged", ddl: `"flagged" boolean NOT NULL`, schema: contract.SchemaTypeBool, kind: pmruntime.SQLBoolean, writable: true, required: true},
		{name: "note", ddl: `"note" text NOT NULL`, schema: contract.SchemaTypeString, kind: pmruntime.SQLText, writable: true, required: true},
		{name: "optional_note", ddl: `"optional_note" text`, schema: contract.SchemaTypeString, kind: pmruntime.SQLText, writable: false, required: false},
		{name: "observed_at", ddl: `"observed_at" timestamp with time zone NOT NULL`, schema: contract.SchemaTypeTimestampInstant, kind: pmruntime.SQLTimestamptz, writable: true, required: true},
		{name: "local_wall", ddl: `"local_wall" timestamp NOT NULL`, schema: contract.SchemaTypeTimestampLocal, kind: pmruntime.SQLTimestamp, writable: true, required: true},
	}
}

// TestSQLExecutionCompositionWorkerBehavior proves the declared postgresql
// operation reaches the real customer database through the shared handler
// registry, and that every unavailability refuses before any physical send.
func TestSQLExecutionCompositionWorkerBehavior(t *testing.T) {
	temporal := newPermitTemporalHarness(t)

	t.Run("declared metadata binds the live relation and publishes exact column rows", func(t *testing.T) {
		harness := newSQLExecutionHarness(t, temporal, sqlExecutionMetadataID, sqlExecutionTuning{declared: true})
		run := harness.executeOperation(
			t, temporal, sqlExecutionMetadataID, contract.LaneDirectRead, sqlExecutionEmptyInput(), "sql-execution-metadata",
		)
		if run.state != contract.RunStateSucceeded || run.unknownEffects != 0 ||
			run.outcome != contract.OutcomeSucceeded || run.attempts != 1 {
			t.Fatalf("metadata settlement = %#v; want one succeeded known effect", run)
		}
		if run.dials != 1 {
			t.Fatalf("metadata dials = %d; want exactly one admitted physical attempt", run.dials)
		}
		rows := harness.publishedRows(t, run)
		columns := sqlExecutionColumns()
		if len(rows) != len(columns) {
			t.Fatalf("published metadata rows = %d; want one row per declared column", len(rows))
		}
		for index, column := range columns {
			fields, ok := rows[index].Fields()
			if !ok {
				t.Fatalf("metadata row %d is not an object: %#v", index, rows[index])
			}
			name, nameOK := fields[sqlExecutionMetadataNameColumn].TextValue()
			kind, kindOK := fields[sqlExecutionMetadataTypeColumn].TextValue()
			if !nameOK || !kindOK || name != column.name || kind != sqlExecutionLiveTypeName(t, column) {
				t.Fatalf("metadata row %d = (%q, %q); want (%q, %q)", index, name, kind, column.name, sqlExecutionLiveTypeName(t, column))
			}
		}
	})

	t.Run("declared bounded select publishes exact typed rows", func(t *testing.T) {
		harness := newSQLExecutionHarness(t, temporal, sqlExecutionMetadataID, sqlExecutionTuning{declared: true})
		harness.customer.seedRow(t)
		run := harness.executeOperation(
			t, temporal, sqlExecutionSelectID, contract.LaneDirectRead, sqlExecutionEmptyInput(), "sql-execution-select",
		)
		if run.state != contract.RunStateSucceeded || run.unknownEffects != 0 ||
			run.outcome != contract.OutcomeSucceeded || run.attempts != 1 || run.dials != 1 {
			t.Fatalf("select settlement = %#v; want one succeeded admitted attempt", run)
		}
		rows := harness.publishedRows(t, run)
		if len(rows) != 1 {
			t.Fatalf("published select rows = %d; want the one seeded row", len(rows))
		}
		assertSQLExecutionExactRow(t, rows[0], sqlExecutionSelectNote)
	})

	t.Run("declared mapped upsert commits exact typed values to the customer relation", func(t *testing.T) {
		harness := newSQLExecutionHarness(t, temporal, sqlExecutionMetadataID, sqlExecutionTuning{declared: true})
		run := harness.executeOperation(
			t, temporal, sqlExecutionUpsertID, contract.LaneDirectWrite, sqlExecutionUpsertInput(t), "sql-execution-upsert",
		)
		if run.state != contract.RunStateSucceeded || run.unknownEffects != 0 ||
			run.outcome != contract.OutcomeSucceeded || run.attempts != 1 || run.dials != 1 {
			t.Fatalf("upsert settlement = %#v; want one committed admitted attempt", run)
		}
		committed := harness.customer.rowVersion(t)
		// An exact repeat of the same mapped values commits with no assignment
		// and settles unchanged rather than as a second applied effect.
		repeat := harness.executeOperation(
			t, temporal, sqlExecutionUpsertID, contract.LaneDirectWrite, sqlExecutionUpsertInput(t), "sql-execution-upsert-repeat",
		)
		if repeat.state != contract.RunStateSucceeded || repeat.unknownEffects != 0 ||
			repeat.outcome != contract.OutcomeUnchanged || repeat.attempts != 1 || repeat.dials != 1 {
			t.Fatalf("identical upsert repeat settlement = %#v; want one unchanged admitted attempt", repeat)
		}
		if version := harness.customer.rowVersion(t); version != committed {
			t.Fatalf("identical upsert repeat physically updated the row: xmin changed from %s to %s", committed, version)
		}
		rows := harness.publishedRows(t, run)
		if len(rows) != 1 {
			t.Fatalf("upsert returned rows = %d; want the one committed returning row", len(rows))
		}
		assertSQLExecutionExactRow(t, rows[0], sqlExecutionUpsertNote)
		// The committed row is observed independently through the customer
		// relation's own owner, not from the effect the transport reported.
		harness.customer.requireCommittedUpsertRow(t)
	})

	t.Run("a select beyond the declared row bound is refused rather than published partially", func(t *testing.T) {
		harness := newSQLExecutionHarness(t, temporal, sqlExecutionMetadataID, sqlExecutionTuning{declared: true})
		harness.customer.seedRows(t, sqlExecutionMaxRows+1)
		run := harness.executeOperation(
			t, temporal, sqlExecutionSelectID, contract.LaneDirectRead, sqlExecutionEmptyInput(), "sql-execution-select-over-bound",
		)
		if run.state == contract.RunStateSucceeded || run.outcome == contract.OutcomeSucceeded ||
			run.outcome != contract.OutcomeRejected || run.unknownEffects != 0 || run.attempts != 1 || run.dials != 1 {
			t.Fatalf("over-bound select settlement = %#v; want one rejected attempt, never a complete success", run)
		}
		// A run with no published result answers the shared unavailable result
		// read; no bounded prefix is reachable as its answer.
		workerResultGET(
			t, harness.fixture,
			"/v1/workspaces/"+harness.fixture.Scope.WorkspaceID+"/runs/"+run.reference+"/result",
			http.StatusServiceUnavailable,
		)
	})

	t.Run("removing the SQL registration refuses the capability with zero sends", func(t *testing.T) {
		harness := newSQLExecutionHarness(t, temporal, sqlExecutionMetadataID, sqlExecutionTuning{})
		problem := harness.admitProblem(t, sqlExecutionMetadataID, contract.LaneDirectRead, "sql-execution-unregistered")
		if problem != "capability_unavailable" {
			t.Fatalf("unregistered SQL admission problem = %q; want capability_unavailable", problem)
		}
		if harness.customer.dials.Load() != 0 {
			t.Fatalf("unregistered capability dialed the customer endpoint %d times", harness.customer.dials.Load())
		}
	})

	t.Run("a declaration for another workspace refuses before any send", func(t *testing.T) {
		harness := newSQLExecutionHarness(t, temporal, sqlExecutionMetadataID, sqlExecutionTuning{declared: true, wrongWorkspace: true})
		err := harness.planError(t, sqlExecutionMetadataID, contract.LaneDirectRead, "sql-execution-wrong-workspace")
		if !errors.Is(err, app.ErrWorkerCapabilityUnavailable) {
			t.Fatalf("wrong-workspace planning error = %v; want explicit capability unavailability", err)
		}
		if harness.customer.dials.Load() != 0 {
			t.Fatalf("wrong-workspace declaration dialed the customer endpoint %d times", harness.customer.dials.Load())
		}
	})

	t.Run("an unusable relation binding refuses before any send", func(t *testing.T) {
		harness := newSQLExecutionHarness(t, temporal, sqlExecutionSelectID, sqlExecutionTuning{declared: true, noSnapshot: true})
		run := harness.executeOperation(
			t, temporal, sqlExecutionSelectID, contract.LaneDirectRead, sqlExecutionEmptyInput(), "sql-execution-unusable-binding",
		)
		if run.state != contract.RunStateBlocked || run.unknownEffects != 0 ||
			run.outcome != contract.OutcomeUnsent || run.attempts != 1 || run.dials != 0 {
			t.Fatalf("unusable binding settlement = %#v; want a known-unsent refusal", run)
		}
	})

	t.Run("a credential that is not the declared one refuses before any send", func(t *testing.T) {
		harness := newSQLExecutionHarness(t, temporal, sqlExecutionSelectID, sqlExecutionTuning{declared: true, wrongCredential: true})
		run := harness.executeOperation(
			t, temporal, sqlExecutionSelectID, contract.LaneDirectRead, sqlExecutionEmptyInput(), "sql-execution-wrong-credential",
		)
		if run.state != contract.RunStateBlocked || run.unknownEffects != 0 ||
			run.outcome != contract.OutcomeUnsent || run.attempts != 1 || run.dials != 0 {
			t.Fatalf("wrong-credential settlement = %#v; want a known-unsent refusal", run)
		}
	})
}

// sqlExecutionTuning selects the one declared-input variation a sub-case needs.
// A zero value registers nothing, which is the undeclared deployment.
type sqlExecutionTuning struct {
	declared        bool
	wrongWorkspace  bool
	noSnapshot      bool
	wrongCredential bool
}

// sqlExecutionRun is the durable settlement of one executed unit together with
// the physical facts this case observed from outside the transport.
type sqlExecutionRun struct {
	reference      string
	state          contract.RunState
	unknownEffects uint64
	outcome        contract.OutcomeState
	attempts       int64
	dials          int32
}

func sqlExecutionEmptyInput() contract.PresentValue {
	return contract.NewObjectValue(map[string]contract.PresentValue{})
}

// sqlExecutionFieldID is one declared logical input identity. An admitted
// invocation carries these exact keys, and the declared binding publishes the
// same identity as its FieldID; the provider wire name a binding also declares
// is a separate fact the transport maps onto.
func sqlExecutionFieldID(column string) string {
	return "column." + column
}

// sqlExecutionUpsertInput is the exact typed input object one mapped upsert
// admits: keyed by the declared logical FieldIDs, carrying an int8 outside
// float64's exact range, an exact decimal with its declared scale, a boolean,
// text, an exact instant and an exact local wall clock.
func sqlExecutionUpsertInput(t *testing.T) contract.PresentValue {
	t.Helper()
	return contract.NewObjectValue(map[string]contract.PresentValue{
		sqlExecutionFieldID("record_id"):   sqlExecutionInteger(t, sqlExecutionKeyText),
		sqlExecutionFieldID("amount"):      sqlExecutionDecimal(t, sqlExecutionAmountText, sqlExecutionAmountPrecision, 4),
		sqlExecutionFieldID("flagged"):     contract.NewBooleanValue(true),
		sqlExecutionFieldID("note"):        contract.NewStringValue(sqlExecutionUpsertNote),
		sqlExecutionFieldID("observed_at"): sqlExecutionInstant(t, sqlExecutionInstantText),
		sqlExecutionFieldID("local_wall"):  sqlExecutionLocalTimestamp(t, sqlExecutionLocalText),
	})
}

func sqlExecutionInteger(t *testing.T, exact string) contract.PresentValue {
	t.Helper()
	value, err := contract.NewIntegerValue(exact)
	if err != nil {
		t.Fatalf("declare exact integer %q: %v", exact, err)
	}
	return value
}

func sqlExecutionDecimal(t *testing.T, exact string, precision, scale int) contract.PresentValue {
	t.Helper()
	value, err := contract.NewDecimalValue(exact, precision, scale)
	if err != nil {
		t.Fatalf("declare exact decimal %q(%d,%d): %v", exact, precision, scale, err)
	}
	return value
}

func sqlExecutionInstant(t *testing.T, exact string) contract.PresentValue {
	t.Helper()
	value, err := contract.NewInstantValue(exact)
	if err != nil {
		t.Fatalf("declare exact instant %q: %v", exact, err)
	}
	return value
}

func sqlExecutionLocalTimestamp(t *testing.T, exact string) contract.PresentValue {
	t.Helper()
	value, err := contract.NewLocalTimestampValue(exact)
	if err != nil {
		t.Fatalf("declare exact local timestamp %q: %v", exact, err)
	}
	return value
}

// assertSQLExecutionExactRow asserts the published row carries every declared
// column as its exact typed value: an int8 outside float64's exact range, a
// numeric that retains its declared scale, a boolean, text, an explicit null in
// the nullable column, an exact instant and an exact local wall clock.
func assertSQLExecutionExactRow(t *testing.T, row contract.PresentValue, note string) {
	t.Helper()
	fields, ok := row.Fields()
	if !ok {
		t.Fatalf("published row is not an object: %#v", row)
	}
	if len(fields) != len(sqlExecutionColumns()) {
		t.Fatalf("published row columns = %d; want %d", len(fields), len(sqlExecutionColumns()))
	}
	exact, ok := fields["record_id"].ExactNumber()
	if !ok || exact.Text != sqlExecutionKeyText || exact.Precision != 16 || exact.Scale != 0 {
		t.Fatalf("published record_id = %#v; want exact %s", fields["record_id"], sqlExecutionKeyText)
	}
	amount, ok := fields["amount"].ExactNumber()
	if !ok || amount.Text != sqlExecutionAmountText || amount.Precision != sqlExecutionAmountPrecision || amount.Scale != 4 {
		t.Fatalf("published amount = %#v; want exact %s(%d,4) from the numeric(9,4) column",
			fields["amount"], sqlExecutionAmountText, sqlExecutionAmountPrecision)
	}
	if flagged, ok := fields["flagged"].Boolean(); !ok || !flagged {
		t.Fatalf("published flagged = %#v; want true", fields["flagged"])
	}
	if text, ok := fields["note"].TextValue(); !ok || text != note {
		t.Fatalf("published note = %#v; want %q", fields["note"], note)
	}
	if fields["optional_note"].Presence() != contract.PresenceNull {
		t.Fatalf("published optional_note presence = %q; want an explicit null", fields["optional_note"].Presence())
	}
	instant, ok := fields["observed_at"].TemporalText()
	if !ok || instant != sqlExecutionInstantUTC {
		t.Fatalf("published observed_at = %#v; want the exact instant %q", fields["observed_at"], sqlExecutionInstantUTC)
	}
	if localWall := fields["local_wall"]; localWall.Kind() != contract.ValueKindTimestampLocal {
		t.Fatalf("published local_wall kind = %q; want the declared local wall clock", localWall.Kind())
	} else if text, ok := localWall.TemporalText(); !ok || text != sqlExecutionLocalText {
		t.Fatalf("published local_wall = %#v; want the exact local wall clock %q", localWall, sqlExecutionLocalText)
	}
}

// sqlExecutionLiveTypeName is the exact PostgreSQL type name the live relation
// reports for one declared column and therefore the metadata row this case
// asserts.
func sqlExecutionLiveTypeName(t *testing.T, column sqlExecutionColumn) string {
	t.Helper()
	switch column.kind {
	case pmruntime.SQLInt8:
		return "bigint"
	case pmruntime.SQLNumeric:
		return "numeric(" + strconv.Itoa(column.precision) + "," + strconv.Itoa(column.scale) + ")"
	case pmruntime.SQLBoolean:
		return "boolean"
	case pmruntime.SQLText:
		return "text"
	case pmruntime.SQLTimestamptz:
		return "timestamp with time zone"
	case pmruntime.SQLTimestamp:
		return "timestamp without time zone"
	default:
		t.Fatalf("declared column %q has no live type name", column.name)
		return ""
	}
}

// sqlExecutionHarness owns the authenticated fixture, the composed SQL
// capability and the isolated customer database one sub-case exercises.
type sqlExecutionHarness struct {
	fixture    *testfixture.Fixture
	service    *app.WorkerService
	customer   *sqlCustomerDatabase
	selections map[string]app.OperationSelection
	resolver   app.OperationResolver
	producer   *app.ArtifactResources
	temporal   *permitTemporalHarness
	closeOnce  sync.Once
}

// executeOperation admits the declared operation through the authenticated path,
// drives the production planner and the shared SQL callback, and settles the
// unit, then reports the durable outcome and the physical facts this case
// observed from outside the transport.
func (harness *sqlExecutionHarness) executeOperation(
	t *testing.T,
	temporal *permitTemporalHarness,
	operationID string,
	lane contract.Lane,
	input contract.PresentValue,
	requestKey string,
) sqlExecutionRun {
	t.Helper()
	admitted := harness.admitRun(t, operationID, lane, input, requestKey)
	before := harness.customer.dials.Load()
	run := preparePermitWorkerRun(
		t, harness.fixture, harness.service, temporal, harness.fixture.Scope, harness.fixture.Actor, admitted,
	)
	result, err := executePermitWorkerUnit(t, harness.service, run)
	if err != nil {
		t.Fatalf("execute declared %s unit: %v", operationID, err)
	}
	outcomes, attempts := loadHTTPDirectReadSettlement(
		t, harness.fixture, harness.fixture.Scope, harness.fixture.Actor, run,
	)
	if len(outcomes) != 1 || len(attempts) != 1 {
		t.Fatalf(
			"settlement outcomes/attempts = %d/%d; want one ordered member and one physical attempt",
			len(outcomes), len(attempts),
		)
	}
	wantRefs := []string{}
	if outcomes[0].State == contract.OutcomeUnsent || outcomes[0].State == contract.OutcomeUnknown {
		wantRefs = []string{attempts[0].ReceiptRef}
	}
	if !slices.Equal(outcomes[0].AssertionRefs, wantRefs) {
		t.Fatalf("settled refs = %v; want %v", outcomes[0].AssertionRefs, wantRefs)
	}
	_, durableAttempts := permitTestDurableCounts(t, harness.fixture, harness.fixture.Scope, admitted.RunRef)
	return sqlExecutionRun{
		reference:      admitted.RunRef,
		state:          result.State,
		unknownEffects: result.UnknownEffectCount,
		outcome:        outcomes[0].State,
		attempts:       durableAttempts,
		dials:          harness.customer.dials.Load() - before,
	}
}

// admitRun admits one declared operation and requires fresh acceptance.
func (harness *sqlExecutionHarness) admitRun(
	t *testing.T,
	operationID string,
	lane contract.Lane,
	input contract.PresentValue,
	requestKey string,
) api.AdmissionResponse {
	t.Helper()
	response, err := harness.admitOperation(t, operationID, lane, input, requestKey)
	if err != nil {
		t.Fatalf("admit declared operation %s: %v", operationID, err)
	}
	return response
}

// admitProblem reports the exact problem code the authenticated path returns.
func (harness *sqlExecutionHarness) admitProblem(
	t *testing.T,
	operationID string,
	lane contract.Lane,
	requestKey string,
) string {
	t.Helper()
	_, err := harness.admitOperation(t, operationID, lane, sqlExecutionEmptyInput(), requestKey)
	var problem *sqlExecutionProblem
	if !errors.As(err, &problem) {
		t.Fatalf("admission error = %v; want a typed problem response", err)
	}
	return problem.code
}

// planError admits the run, binds its real Temporal execution, and reports the
// production planner's refusal for the declared operation.
func (harness *sqlExecutionHarness) planError(
	t *testing.T,
	operationID string,
	lane contract.Lane,
	requestKey string,
) error {
	t.Helper()
	admitted := harness.admitRun(t, operationID, lane, sqlExecutionEmptyInput(), requestKey)
	bound := bindPermitWorkerRun(
		t, harness.fixture, harness.temporal, harness.fixture.Scope, harness.fixture.Actor, admitted.RunRef,
	)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := harness.service.LoadWorkflow(ctx, bound.execution, bound.reference)
	if err == nil {
		t.Fatal("planning a declared operation without a server-owned connection declaration succeeded")
	}
	return err
}

// publishedRows reads the immutable published result of one settled run through
// the public artifact API after the producer is closed.
func (harness *sqlExecutionHarness) publishedRows(t *testing.T, run sqlExecutionRun) []contract.PresentValue {
	t.Helper()
	references := workerResultReferences(t, harness.fixture, run.reference)
	if len(references) != 1 {
		t.Fatalf("published result references = %v; want one immutable shared artifact", references)
	}
	harness.closeProducer(t)
	payload := workerResultGET(
		t,
		harness.fixture,
		"/v1/workspaces/"+harness.fixture.Scope.WorkspaceID+"/artifacts/"+references[0]+"/content",
		http.StatusOK,
	)
	var result contract.ActionResult
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode published result: %v", err)
	}
	if err := result.Validate(); err != nil || result.MediaType != "application/json" || result.Pagination != nil {
		t.Fatalf("published result = %#v error %v; want exact typed JSON rows", result, err)
	}
	rows, ok := result.Data.Elements()
	if !ok {
		t.Fatalf("published result data = %#v; want an array of typed rows", result.Data)
	}
	return rows
}

func (harness *sqlExecutionHarness) closeProducer(t *testing.T) {
	t.Helper()
	harness.closeOnce.Do(func() {
		if harness.producer == nil {
			return
		}
		if err := harness.producer.Close(); err != nil {
			t.Fatalf("close the producer of the published result: %v", err)
		}
	})
}

// admitOperation resolves the declared operation from the loaded bundle registry
// and submits it through the fixture's authenticated invocation route.
func (harness *sqlExecutionHarness) admitOperation(
	t *testing.T,
	operationID string,
	lane contract.Lane,
	input contract.PresentValue,
	requestKey string,
) (api.AdmissionResponse, error) {
	t.Helper()
	selection, present := harness.selections[operationID]
	if !present {
		t.Fatalf("declared operation %q is not installed by the loaded bundle registry", operationID)
	}
	revision, err := app.OperationSelectionRevision(selection)
	if err != nil {
		t.Fatalf("resolve declared operation revision: %v", err)
	}
	invocation := api.InvocationRequest{
		ConnectionRef:  harness.fixture.Connection.ConnectionID,
		OperationRef:   operationID,
		Lane:           api.InvocationRequestLane(lane),
		Input:          input,
		ResultControls: contract.ResultControls{},
		Protocol: api.ProtocolMetadata{
			WireVersion:      api.ProtocolMetadataWireVersion(api.WireVersion),
			RequiredFeatures: []string{},
			RequestKey:       requestKey,
			IntentDigest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			ExpectedRevision: revision,
		},
	}
	response := submitInitialInvocation(
		t, harness.fixture, harness.fixture.Client, harness.fixture.Scope.WorkspaceID, invocation,
	)
	if response.status == http.StatusAccepted {
		return requireFreshInitialAdmission(t, response, requestKey), nil
	}
	return api.AdmissionResponse{}, &sqlExecutionProblem{
		status: response.status, code: response.problemCode, document: response.document,
	}
}

type sqlExecutionProblem struct {
	status   int
	code     string
	document []byte
}

func (problem *sqlExecutionProblem) Error() string {
	return "invocation refused with HTTP " + strconv.Itoa(problem.status) + " " + problem.code +
		" body=" + string(problem.document)
}

// newSQLExecutionHarness declares one server-owned customer SQL connection for
// the fixture's primary scope, registers the shared SQL callback for the
// operations the loaded bundle registry declares, extends the fixture's own
// membership to exactly those declared operation identities, and composes the
// production worker over that exact registry. The fixture seeds one controlled
// operation identity, so without that extension the authenticated path would
// conceal every other declared identity exactly as it conceals any identity the
// membership does not grant.
func newSQLExecutionHarness(
	t *testing.T,
	temporal *permitTemporalHarness,
	operationID string,
	tuning sqlExecutionTuning,
) *sqlExecutionHarness {
	t.Helper()
	harness := &sqlExecutionHarness{customer: newSQLCustomerDatabase(t), temporal: temporal}
	connectorComposition, err := app.OpenConnectorComposition(app.ConnectorCompositionConfig{
		BundlePaths: []string{sqlExecutionBundlePath(t)},
	})
	if err != nil {
		t.Fatalf("open the declared connector composition: %v", err)
	}
	harness.selections = sqlExecutionSelections(t, connectorComposition.Registry())
	registry := &app.HandlerRegistry{}
	producer := testfixture.OpenArtifacts(t, testfixture.PrimaryArtifactGateway)
	consumer := testfixture.OpenArtifacts(t, testfixture.SecondaryArtifactGateway)
	var composition *app.SQLComposition
	fixture := testfixture.NewOperation(t, operationID, func(
		dependencies testfixture.ServiceDependencies,
		configuration *httpapi.ServiceConfig,
	) error {
		declaration := sqlExecutionDeclaration(t, harness.customer, dependencies, tuning)
		document, err := config.Load(strings.NewReader(sqlExecutionStartupDocument(declaration)))
		if err != nil {
			return err
		}
		resolved, err := config.Validate(document, config.Prelaunch{})
		if err != nil {
			return err
		}
		if !tuning.noSnapshot {
			harness.customer.promoteSnapshot(t, dependencies)
		}
		composition, err = app.OpenSQLComposition(app.SQLCompositionConfig{
			Declarations: resolved.ProviderSQL(),
			Bundles:      connectorComposition.Registry(),
			Dialer:       harness.customer,
			Resources:    harness.customer.resources,
		})
		if err != nil {
			return err
		}
		if tuning.declared {
			if err := composition.Register(registry); err != nil {
				return err
			}
		}
		resolver := func(
			ctx context.Context,
			scope contract.Scope,
			connectionID string,
			operationID string,
		) (app.OperationSelection, error) {
			if ctx == nil || ctx.Err() != nil {
				return app.OperationSelection{}, context.Canceled
			}
			primary := scope.Equal(dependencies.Scope) && connectionID == dependencies.Connection.ConnectionID
			foreign := scope.Equal(dependencies.ForeignScope) && connectionID == dependencies.ForeignConnection.ConnectionID
			if !primary && !foreign {
				return app.OperationSelection{}, app.ErrConnectorUnavailable
			}
			selection, present := harness.selections[operationID]
			if !present {
				return app.OperationSelection{}, app.ErrConnectorUnavailable
			}
			return selection, nil
		}
		harness.resolver = resolver
		admission, err := app.NewAdmissionService(app.AdmissionServiceConfig{
			Pool:              dependencies.Pool,
			DeploymentID:      dependencies.DeploymentID,
			Authorize:         configuration.Auth.AuthorizeResource,
			Registry:          registry,
			ResolveOperation:  resolver,
			ResolveHTTPPolicy: nil,
			Policy:            httpDirectReadAdmissionPolicy(),
			Random:            rand.Reader,
		})
		if err != nil {
			return err
		}
		configuration.Admission = admission
		configuration.Artifacts = consumer.Backend
		return nil
	})
	if composition == nil {
		t.Fatal("the declared SQL composition was not built for the fixture")
	}
	harness.fixture = fixture
	grantSQLExecutionDeclaredOperations(t, fixture)
	publisher, err := objects.NewPublisher(fixture.Pool, producer.Backend, producer.Limits)
	if err != nil {
		t.Fatalf("compose the shared result publisher: %v", err)
	}
	harness.producer = producer
	key := []byte("0123456789abcdef0123456789abcdef")
	t.Cleanup(func() { clear(key) })
	loader := &sqlExecutionKeyLoader{key: key}
	seedSQLExecutionCredential(t, fixture, key, []byte(harness.customer.rolePassword))
	service, err := app.NewWorkerService(app.WorkerServiceConfig{
		Pool:                      fixture.Pool,
		DeploymentID:              fixture.DeploymentID,
		Registry:                  registry,
		ResolveOperation:          harness.resolver,
		Authorization:             auth.NewResourceAuthorizer(),
		ObserveContinuation:       temporal.observer,
		ActivityTimeouts:          temporal.timeouts,
		LoadCredentialWrappingKey: loader.load,
		ArtifactPublisher:         publisher,
		MaximumResultBytes:        sqlExecutionResultBytes,
		SQLBounds:                 composition.Bounds,
	})
	if err != nil {
		t.Fatalf("compose the production worker over the registered SQL capability: %v", err)
	}
	harness.service = service
	return harness
}

type sqlExecutionKeyLoader struct {
	key []byte
}

func (loader *sqlExecutionKeyLoader) load(ctx context.Context, reference string) ([]byte, error) {
	if ctx == nil || ctx.Err() != nil || reference != sqlExecutionKeyRef {
		return nil, errors.New("controlled wrapping key is unavailable")
	}
	return append([]byte(nil), loader.key...), nil
}

// seedSQLExecutionCredential installs the customer SQL password as one PM-managed
// one-use credential of the fixture connection, exactly as the credential
// ingress does. The declaration names that same credential reference, so the
// composed connection and the stored credential are the same secret.
func seedSQLExecutionCredential(
	t *testing.T,
	fixture *testfixture.Fixture,
	key []byte,
	secret []byte,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := credentials.StoreEncryptedCredential(
		ctx,
		fixture.Pool,
		func(ctx context.Context, reference string) ([]byte, error) {
			if ctx == nil || ctx.Err() != nil || reference != sqlExecutionKeyRef {
				return nil, errors.New("controlled wrapping key is unavailable")
			}
			return append([]byte(nil), key...), nil
		},
		func(_ context.Context, _ *store.Transaction, access credentials.Access) error {
			if access.Action != credentials.ActionManage || access.Stage != credentials.StageMutation ||
				!access.Scope.Equal(fixture.Scope) || !access.Actor.Equal(fixture.Actor) ||
				access.Connection.ConnectionID != fixture.Connection.ConnectionID || access.Effect != nil {
				return errors.New("controlled credential mutation authority differs")
			}
			return nil
		},
		credentials.StoreRequest{
			Scope: fixture.Scope, Actor: fixture.Actor,
			ConnectionID: fixture.Connection.ConnectionID, CredentialRef: fixture.Connection.CredentialRef,
			IdempotencyKey: "sql-execution-credential-v1", KeyRef: sqlExecutionKeyRef,
			Scopes: []string{"postgres.execute"}, AuthEpoch: 1, Secret: secret,
		},
	)
	if err != nil {
		t.Fatalf("seed the customer SQL credential: %v", err)
	}
}

// sqlCustomerDatabase is the isolated customer PostgreSQL database, role and
// relation this case creates inside the owned stack fixture. Its superuser
// connections exist only to provision and independently observe that database;
// the transport never sees them.
type sqlCustomerDatabase struct {
	t            *testing.T
	host         string
	port         uint16
	role         string
	rolePassword string
	database     string
	trustPEM     []byte
	stateDir     string
	snapshots    *schema.Store
	cluster      *pgconn.PgConn
	owner        *pgconn.PgConn
	dialAddress  string
	dials        atomic.Int32
	closeOnce    sync.Once
}

// DialContext dials exactly the declared endpoint. A different address is
// refused, so this counter is the independent observation that no other
// endpoint was ever contacted.
func (customer *sqlCustomerDatabase) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if address != customer.dialAddress {
		return nil, errors.New("customer SQL dial target is not the declared endpoint")
	}
	customer.dials.Add(1)
	return (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, address)
}

func newSQLCustomerDatabase(t *testing.T) *sqlCustomerDatabase {
	t.Helper()
	directory := os.Getenv("PM_STORE_TEST_SECRETS_DIR")
	if directory == "" {
		t.Fatal("this same-stack case requires the owned stack fixture: PM_STORE_TEST_SECRETS_DIR is required and missing inputs fail rather than skip")
	}
	portValue := os.Getenv("PM_STACK_POSTGRES_PORT")
	port, err := strconv.Atoi(portValue)
	if portValue == "" || err != nil || port < 1024 || port > 65535 {
		t.Fatalf("PM_STACK_POSTGRES_PORT must be the selected non-privileged fixture port: %q", portValue)
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		t.Fatal(err)
	}
	customer := &sqlCustomerDatabase{
		t:            t,
		host:         sqlExecutionServerName,
		port:         uint16(port),
		role:         sqlExecutionRolePrefix + sqlExecutionToken(t, 6),
		database:     sqlExecutionDatabasePrefix + sqlExecutionToken(t, 6),
		rolePassword: sqlExecutionToken(t, 24),
		trustPEM:     sqlExecutionSecret(t, absolute, sqlExecutionTrustMember),
		stateDir:     sqlExecutionStateDirectory(t),
	}
	customer.dialAddress = net.JoinHostPort(customer.host, strconv.Itoa(int(customer.port)))
	t.Cleanup(customer.close)
	snapshots, err := schema.NewStore(customer.stateDir)
	if err != nil {
		t.Fatalf("open the connection schema state directory: %v", err)
	}
	customer.snapshots = snapshots
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	superuser := string(sqlExecutionSecret(t, absolute, sqlExecutionSuperuserMember))
	customer.cluster = customer.connect(t, ctx, sqlExecutionAdminUser, superuser, sqlExecutionAdminDatabase)
	role := `"` + customer.role + `"`
	database := `"` + customer.database + `"`
	customer.clusterExec(t, ctx, `CREATE ROLE `+role+` LOGIN PASSWORD '`+customer.rolePassword+`'`)
	customer.clusterExec(t, ctx, `CREATE DATABASE `+database)
	customer.clusterExec(t, ctx, `REVOKE ALL ON DATABASE `+database+` FROM PUBLIC`)
	customer.clusterExec(t, ctx, `GRANT CONNECT ON DATABASE `+database+` TO `+role)
	customer.owner = customer.connect(t, ctx, sqlExecutionAdminUser, superuser, customer.database)
	customer.ownerExec(t, ctx, `SET timezone = 'UTC'`)
	for _, statement := range customer.provisionStatements() {
		customer.ownerExec(t, ctx, statement)
	}
	return customer
}

// provisionStatements create exactly the declared relation with exactly the
// declared columns and grant the customer role read and write access to that
// one relation: no schema DDL, no other relation and no delete privilege.
func (customer *sqlCustomerDatabase) provisionStatements() []string {
	declared := sqlExecutionColumns()
	columns := make([]string, 0, len(declared))
	for _, column := range declared {
		columns = append(columns, column.ddl)
	}
	relation := `"` + sqlExecutionSchema + `"."` + sqlExecutionRelation + `"`
	role := `"` + customer.role + `"`
	return []string{
		`CREATE SCHEMA "` + sqlExecutionSchema + `"`,
		`REVOKE ALL ON SCHEMA "` + sqlExecutionSchema + `" FROM PUBLIC`,
		`CREATE TABLE ` + relation + ` (` + strings.Join(columns, ", ") + `)`,
		`GRANT USAGE ON SCHEMA "` + sqlExecutionSchema + `" TO ` + role,
		`GRANT SELECT, INSERT, UPDATE ON ` + relation + ` TO ` + role,
	}
}

// promoteSnapshot binds the declared relation's discovered metadata: the exact
// declared columns with the exact declared logical types. The composed relation
// therefore derives its transport columns from this selected snapshot, never
// from an unsnapshotted guess.
func (customer *sqlCustomerDatabase) promoteSnapshot(t *testing.T, dependencies testfixture.ServiceDependencies) {
	t.Helper()
	candidate := schema.Candidate{
		Binding:      sqlExecutionSnapshotBinding(dependencies),
		Fields:       sqlExecutionSnapshotFields(t),
		Completeness: schema.Completeness{Complete: true, Gaps: []schema.Gap{}},
	}
	if _, err := customer.snapshots.Promote(candidate.Binding, candidate, ""); err != nil {
		t.Fatalf("promote the declared relation snapshot: %v", err)
	}
}

func sqlExecutionSnapshotBinding(dependencies testfixture.ServiceDependencies) schema.Binding {
	return schema.Binding{
		ConnectorID:        sqlExecutionConnectorID,
		ConnectionID:       dependencies.Connection.ConnectionID,
		ProviderAccountID:  dependencies.Connection.ProviderAccountID,
		PermissionRevision: sqlExecutionPermissionRev,
		Resource:           sqlExecutionResource,
		SourceRevision:     sqlExecutionSourceRevision,
	}
}

func sqlExecutionSnapshotFields(t *testing.T) []schema.Field {
	t.Helper()
	declared := sqlExecutionColumns()
	fields := make([]schema.Field, 0, len(declared))
	for _, column := range declared {
		candidate := schema.Field{
			Name:      column.name,
			Pointer:   "#/relations/" + sqlExecutionResource + "/columns/" + column.name,
			Schema:    contract.Schema{Type: column.schema, Nullable: !column.required},
			Required:  column.required,
			Writable:  column.writable,
			Precision: column.precision,
			Scale:     column.scale,
		}
		if err := candidate.Validate(); err != nil {
			t.Fatalf("declared snapshot field %q is invalid: %v", column.name, err)
		}
		fields = append(fields, candidate)
	}
	return fields
}

// resources supplies the composed capability with this connection's process
// resources: the declared strict trust anchor and the selected snapshot store.
// It never reads the declared password reference: credential material is
// resolved only at the execution boundary.
func (customer *sqlCustomerDatabase) resources(
	connection config.ProviderSQLConnection,
) (app.SQLConnectionResources, error) {
	if connection.Host != customer.host || connection.Port != customer.port {
		return app.SQLConnectionResources{}, errors.New("declared customer endpoint is not the isolated fixture endpoint")
	}
	if _, err := pmruntime.NewStrictSQLTLS(pmruntime.SQLTLSConfig{
		CACertificatePEM: customer.trustPEM,
		ServerName:       connection.Host,
	}); err != nil {
		return app.SQLConnectionResources{}, err
	}
	return app.SQLConnectionResources{TrustAnchorPEM: customer.trustPEM, Snapshots: customer.snapshots}, nil
}

func (customer *sqlCustomerDatabase) connect(
	t *testing.T,
	ctx context.Context,
	user string,
	password string,
	database string,
) *pgconn.PgConn {
	t.Helper()
	strict, err := pmruntime.NewStrictSQLTLS(pmruntime.SQLTLSConfig{
		CACertificatePEM: customer.trustPEM,
		ServerName:       customer.host,
	})
	if err != nil {
		t.Fatalf("build the fixture strict TLS configuration: %v", err)
	}
	config, err := pgconn.ParseConfig(
		"host=" + customer.host + " port=" + strconv.Itoa(int(customer.port)) +
			" dbname=" + database + " user=" + user + " sslmode=verify-full",
	)
	if err != nil {
		t.Fatalf("parse the fixture endpoint facts: %v", err)
	}
	config.Password = password
	config.TLSConfig = strict
	config.ConnectTimeout = 15 * time.Second
	connection, err := pgconn.ConnectConfig(ctx, config)
	config.Password = ""
	config.TLSConfig = nil
	if err != nil {
		t.Fatalf("connect the fixture %s identity: %T", user, err)
	}
	if witness, err := pmruntime.ObserveStrictSQLTLS(connection.Conn(), customer.host, time.Now().UTC()); err != nil {
		t.Fatalf("fixture connection is not a strictly verified TLS session: %v", err)
	} else if err := witness.Validate(); err != nil {
		t.Fatalf("fixture TLS witness is incomplete: %v", err)
	}
	return connection
}

func (customer *sqlCustomerDatabase) clusterExec(t *testing.T, ctx context.Context, statement string) {
	t.Helper()
	customer.exec(t, ctx, customer.cluster, statement)
}

func (customer *sqlCustomerDatabase) ownerExec(t *testing.T, ctx context.Context, statement string) {
	t.Helper()
	customer.exec(t, ctx, customer.owner, statement)
}

func (customer *sqlCustomerDatabase) exec(t *testing.T, ctx context.Context, connection *pgconn.PgConn, statement string) {
	t.Helper()
	if _, err := connection.Exec(ctx, statement).ReadAll(); err != nil {
		t.Fatalf("apply one isolated customer database statement: %T", err)
	}
}

// seedRow writes the exact typed row the declared bounded select must return.
func (customer *sqlCustomerDatabase) seedRow(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	customer.ownerExec(t, ctx, `INSERT INTO "`+sqlExecutionSchema+`"."`+sqlExecutionRelation+`" `+
		`("record_id", "amount", "flagged", "note", "optional_note", "observed_at", "local_wall") VALUES (`+
		sqlExecutionKeyText+`, `+sqlExecutionAmountText+`, true, '`+sqlExecutionSelectNote+`', NULL, `+
		`'`+sqlExecutionInstantText+`'::timestamptz, '`+sqlExecutionLocalText+`'::timestamp)`)
}

// seedRows writes count distinct rows, so a bounded select can observe more
// matching rows than its declared row bound.
func (customer *sqlCustomerDatabase) seedRows(t *testing.T, count int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	customer.ownerExec(t, ctx, `INSERT INTO "`+sqlExecutionSchema+`"."`+sqlExecutionRelation+`" `+
		`("record_id", "amount", "flagged", "note", "optional_note", "observed_at", "local_wall") `+
		`SELECT series, `+sqlExecutionAmountText+`, true, '`+sqlExecutionSelectNote+`', NULL, `+
		`'`+sqlExecutionInstantText+`'::timestamptz, '`+sqlExecutionLocalText+`'::timestamp `+
		`FROM generate_series(1, `+strconv.Itoa(count)+`) AS series`)
}

// rowVersion reports the physical row version of the declared key through the
// customer relation's own owner connection.
func (customer *sqlCustomerDatabase) rowVersion(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	results, readErr := customer.owner.Exec(ctx, `SELECT xmin::text FROM "`+sqlExecutionSchema+`"."`+
		sqlExecutionRelation+`" WHERE "record_id" = `+sqlExecutionKeyText).ReadAll()
	if readErr != nil || len(results) != 1 || results[0].Err != nil || len(results[0].Rows) != 1 ||
		len(results[0].Rows[0]) != 1 || results[0].Rows[0][0] == nil {
		t.Fatalf("independent customer row version observation = %#v error %v", results, readErr)
	}
	return string(results[0].Rows[0][0])
}

// requireCommittedUpsertRow observes the committed row through the customer
// relation's own owner connection, independently of the effect the transport
// reported.
func (customer *sqlCustomerDatabase) requireCommittedUpsertRow(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	results, readErr := customer.owner.Exec(ctx, `SELECT "record_id"::text, "amount"::text, "flagged"::text, "note", `+
		`"optional_note", to_char("observed_at" AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.USZ'), `+
		`to_char("local_wall", 'YYYY-MM-DD"T"HH24:MI:SS.US') FROM "`+sqlExecutionSchema+`"."`+sqlExecutionRelation+`" `+
		`WHERE "record_id" = `+sqlExecutionKeyText).ReadAll()
	if readErr != nil || len(results) != 1 || results[0].Err != nil || len(results[0].Rows) != 1 {
		t.Fatalf("independent customer row observation = %#v error %v", results, readErr)
	}
	row := results[0].Rows[0]
	fields := make([]string, 0, len(row))
	nulls := make([]bool, 0, len(row))
	for index := range row {
		fields = append(fields, string(row[index]))
		nulls = append(nulls, row[index] == nil)
	}
	want := []string{
		sqlExecutionKeyText, sqlExecutionAmountText, "true", sqlExecutionUpsertNote, "", sqlExecutionInstantUTC, sqlExecutionLocalText,
	}
	wantNull := []bool{false, false, false, false, true, false, false}
	for index := range want {
		if index >= len(fields) {
			t.Fatalf("committed customer row has %d columns; want %d", len(fields), len(want))
		}
		if fields[index] != want[index] || nulls[index] != wantNull[index] {
			t.Fatalf(
				"committed customer column %d = (%q, null=%t); want (%q, null=%t)",
				index, fields[index], nulls[index], want[index], wantNull[index],
			)
		}
	}
}

func (customer *sqlCustomerDatabase) close() {
	customer.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if customer.owner != nil {
			if err := customer.owner.Close(ctx); err != nil {
				customer.t.Errorf("close the isolated customer owner connection: %T", err)
			}
		}
		if customer.cluster != nil {
			if _, err := customer.cluster.Exec(
				ctx, `DROP DATABASE IF EXISTS "`+customer.database+`" WITH (FORCE)`,
			).ReadAll(); err != nil {
				customer.t.Errorf("drop the isolated customer database: %T", err)
			}
			if _, err := customer.cluster.Exec(
				ctx, `DROP ROLE IF EXISTS "`+customer.role+`"`,
			).ReadAll(); err != nil {
				customer.t.Errorf("drop the isolated customer role: %T", err)
			}
			if err := customer.cluster.Close(ctx); err != nil {
				customer.t.Errorf("close the isolated customer cluster connection: %T", err)
			}
		}
	})
}

// sqlExecutionStateDirectory is the private connection state directory the
// declared connection owns. The schema store requires an existing private
// directory, so this case creates exactly that.
func sqlExecutionStateDirectory(t *testing.T) string {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "connection-state")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatalf("create the connection state directory: %v", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("restrict the connection state directory: %v", err)
	}
	return directory
}

func sqlExecutionSecret(t *testing.T, directory, member string) []byte {
	t.Helper()
	value, err := os.ReadFile(filepath.Join(directory, member))
	if err != nil {
		t.Fatalf("read the owned fixture secret member %s: %v", member, err)
	}
	return bytes.TrimSpace(value)
}

func sqlExecutionToken(t *testing.T, length int) string {
	t.Helper()
	raw := make([]byte, length)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("generate an isolated customer identity: %v", err)
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	token := make([]byte, length)
	for index, value := range raw {
		token[index] = alphabet[int(value)%len(alphabet)]
	}
	return string(token)
}

func sqlExecutionBundlePath(t *testing.T) string {
	t.Helper()
	document, err := json.Marshal(sqlExecutionBundle(t))
	if err != nil {
		t.Fatalf("encode the declared connector bundle: %v", err)
	}
	if _, err := connectors.LoadBundle(bytes.NewReader(document)); err != nil {
		t.Fatalf("the declared connector bundle is invalid: %v", err)
	}
	path := filepath.Join(t.TempDir(), "postgres.bundle.json")
	if err := os.WriteFile(path, document, 0o600); err != nil {
		t.Fatalf("write the declared connector bundle: %v", err)
	}
	return path
}

// sqlExecutionBundle declares the postgresql operations the loaded bundle
// registry installs for the postgres connector. The statement kinds, relation
// binding, capability closure and eligible lanes mirror the committed generated
// PostgreSQL declaration.
func sqlExecutionBundle(t *testing.T) connectors.Bundle {
	t.Helper()
	bundle := connectors.Bundle{
		SchemaVersion:     connectors.BundleSchemaVersion,
		ConnectorID:       sqlExecutionConnectorID,
		SourceDigest:      "sha256:" + strings.Repeat("c", 64),
		SemanticsRevision: "sha256:" + strings.Repeat("d", 64),
		Operations: []contract.Operation{
			sqlExecutionOperation(t, sqlExecutionMetadataID, contract.PostgreSQLStatementMetadata, contract.LaneDirectRead),
			sqlExecutionOperation(t, sqlExecutionSelectID, contract.PostgreSQLStatementSelect, contract.LaneDirectRead),
			sqlExecutionOperation(t, sqlExecutionUpsertID, contract.PostgreSQLStatementMappedUpsert, contract.LaneDirectWrite),
		},
		Commands:              []contract.Command{},
		UnavailableOperations: []contract.UnavailableOperation{},
	}
	if err := bundle.Validate(); err != nil {
		t.Fatalf("the declared connector bundle is invalid: %v", err)
	}
	return bundle
}

func sqlExecutionOperation(
	t *testing.T,
	operationID string,
	statementKind contract.PostgreSQLStatementKind,
	eligible contract.Lane,
) contract.Operation {
	t.Helper()
	sourceRefs := []contract.SourceRef{{SourceID: "postgres-fixture", Pointer: "#/operations/" + operationID}}
	lanes := []contract.Lane{
		contract.LaneDirectRead, contract.LaneDirectWrite, contract.LaneETL,
		contract.LaneReverseETL, contract.LaneBinaryDownload, contract.LaneBinaryUpload, contract.LaneSyncTransport,
	}
	eligibility := make([]contract.LaneEligibility, 0, len(lanes))
	for _, lane := range lanes {
		status, reason := contract.LaneNotApplicable, "fixture-lane-not-applicable"
		if lane == eligible {
			status, reason = contract.LaneEligible, "declared-postgresql-statement"
		}
		eligibility = append(eligibility, contract.LaneEligibility{Lane: lane, Status: status, Reason: reason})
	}
	operation := contract.Operation{
		SchemaVersion: contract.OperationSchemaVersion,
		OperationID:   operationID,
		SourceRefs:    sourceRefs,
		Transport: contract.Transport{
			Kind:            contract.TransportKindPostgreSQL,
			StatementKind:   statementKind,
			RelationBinding: sqlExecutionBinding,
		},
		Inputs:          sqlExecutionOperationInputs(statementKind),
		RequestVariants: []contract.RequestVariant{},
		Responses: []contract.Response{{
			Status:     "success",
			NoContent:  false,
			MediaType:  "application/json",
			Schema:     &contract.Schema{Type: contract.SchemaTypeArray, Items: &contract.Schema{Type: contract.SchemaTypeObject, OpenProperties: true}},
			SourceRefs: sourceRefs,
		}},
		CapabilityRequirements: sqlExecutionCapabilities(),
		LaneEligibility:        eligibility,
	}
	return operation
}

// sqlExecutionOperationInputs declares the mapped columns of a declared mapped
// upsert. Every input's wire name is exactly the declared relation column it
// binds, so no positional or name-based guess is needed at execution time.
func sqlExecutionOperationInputs(statementKind contract.PostgreSQLStatementKind) []contract.FieldBinding {
	if statementKind != contract.PostgreSQLStatementMappedUpsert {
		return []contract.FieldBinding{}
	}
	declared := sqlExecutionColumns()
	inputs := make([]contract.FieldBinding, 0, len(declared))
	for _, column := range declared {
		if !column.writable {
			continue
		}
		inputs = append(inputs, contract.FieldBinding{
			FieldID:     sqlExecutionFieldID(column.name),
			WireName:    column.name,
			Location:    contract.FieldLocationQuery,
			JSONPointer: "",
			Required:    column.required,
			Nullable:    !column.required,
			Style:       "form",
			Explode:     true,
			Sensitivity: contract.SensitivityPublic,
			Schema:      contract.Schema{Type: column.schema, Nullable: !column.required},
		})
	}
	return inputs
}

// sqlExecutionCapabilities is the declared capability closure of a PostgreSQL
// operation: authorization, bounded effects, the native SQL executor and exact
// value serialization.
func sqlExecutionCapabilities() []contract.CapabilityRequirement {
	return []contract.CapabilityRequirement{
		{Capability: "effect.authorize", Foundation: contract.FoundationRef{ID: "FND-05", Version: "2.0.0"}},
		{Capability: "effect.limit", Foundation: contract.FoundationRef{ID: "FND-10", Version: "2.0.0"}},
		{Capability: "sql.execute", Foundation: contract.FoundationRef{ID: "FND-07", Version: "1.0.0"}},
		{Capability: "value.serialize", Foundation: contract.FoundationRef{ID: "FND-03", Version: "1.0.0"}},
	}
}

// sqlExecutionDeclaredOperations is every operation identity this case declares
// in its bundle and admits through the authenticated invocation route, with the
// declared postgresql statement kind it must carry. It is the one definition of
// the declared operation set: both the resolved selections and the fixture
// authority this case grants derive from it, so the case can never admit an
// identity it did not declare.
func sqlExecutionDeclaredOperations() map[string]contract.PostgreSQLStatementKind {
	return map[string]contract.PostgreSQLStatementKind{
		sqlExecutionMetadataID: contract.PostgreSQLStatementMetadata,
		sqlExecutionSelectID:   contract.PostgreSQLStatementSelect,
		sqlExecutionUpsertID:   contract.PostgreSQLStatementMappedUpsert,
	}
}

// grantSQLExecutionDeclaredOperations extends this fixture's own membership so it
// grants exactly the operation identities this case declares. The controlled
// fixture seeds one operation identity; a case that declares several must extend
// its own authority rows rather than weaken the real authorizer, so an identity
// outside the declaration stays concealed. Both authority columns are kept
// coherent and both revisions advance, exactly as any other authority mutation.
func grantSQLExecutionDeclaredOperations(t *testing.T, fixture *testfixture.Fixture) {
	t.Helper()
	declared := sqlExecutionDeclaredOperations()
	operationIDs := make([]string, 0, len(declared))
	resourceGrants := make([]string, 0, len(declared))
	for operationID := range declared {
		operationIDs = append(operationIDs, operationID)
		resourceGrants = append(resourceGrants, "operation:"+operationID)
	}
	slices.Sort(operationIDs)
	slices.Sort(resourceGrants)
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE pm.memberships
			SET resource_grants = ARRAY(
					SELECT DISTINCT value
					FROM unnest(resource_grants || $4::text[]) AS value
					ORDER BY value
				),
				operation_ids = ARRAY(
					SELECT DISTINCT value
					FROM unnest(operation_ids || $5::text[]) AS value
					ORDER BY value
				),
				policy_epoch = policy_epoch + 1,
				control_revision = control_revision + 1
			WHERE org_id = $1 AND workspace_id = $2 AND identity_id = $3
		`,
			fixture.Scope.OrgID,
			fixture.Scope.WorkspaceID,
			fixture.Actor.RequestedActor.PrincipalID,
			resourceGrants,
			operationIDs,
		)
		return err
	})
}

// sqlExecutionSelections resolves every declared postgresql operation from the
// loaded bundle registry and asserts the declared facts this case relies on.
func sqlExecutionSelections(t *testing.T, registry *connectors.Registry) map[string]app.OperationSelection {
	t.Helper()
	want := sqlExecutionDeclaredOperations()
	selections := make(map[string]app.OperationSelection, len(want))
	for operationID, statementKind := range want {
		selection, err := app.ResolveBundledOperation(registry, sqlExecutionConnectorID, operationID)
		if err != nil {
			t.Fatalf("resolve declared postgresql operation %q: %v", operationID, err)
		}
		if selection.Operation.Transport.Kind != contract.TransportKindPostgreSQL ||
			selection.Operation.Transport.StatementKind != statementKind ||
			selection.Operation.Transport.RelationBinding != sqlExecutionBinding {
			t.Fatalf("declared operation %q transport = %#v", operationID, selection.Operation.Transport)
		}
		selections[operationID] = selection
	}
	return selections
}

// sqlExecutionDeclaration builds the server-owned customer SQL declaration for
// this fixture scope. It carries the endpoint facts, the trust anchor and
// password references, the permitted relation binding with its concrete
// schema, table and key columns, and the declared bounds. A wrong-workspace or
// wrong-credential declaration is exactly the declaration a sub-case needs; no
// secret value is ever declared.
func sqlExecutionDeclaration(
	t *testing.T,
	customerDatabase *sqlCustomerDatabase,
	dependencies testfixture.ServiceDependencies,
	tuning sqlExecutionTuning,
) string {
	t.Helper()
	customer := dependencies
	workspace := customer.Scope.WorkspaceID
	if tuning.wrongWorkspace {
		workspace = customer.ForeignScope.WorkspaceID
	}
	passwordRef := customer.Connection.CredentialRef
	if tuning.wrongCredential {
		passwordRef = customer.Connection.CredentialRef + "-undeclared"
	}
	keyColumns := make([]string, 0, 1)
	for _, column := range sqlExecutionColumns() {
		if column.key {
			keyColumns = append(keyColumns, column.name)
		}
	}
	binding := sqlExecutionSnapshotBinding(customer)
	encodedKeys, err := json.Marshal(keyColumns)
	if err != nil {
		t.Fatalf("encode declared key columns: %v", err)
	}
	connection := `{"org_id":` + sqlExecutionJSON(customer.Scope.OrgID) +
		`,"workspace_id":` + sqlExecutionJSON(workspace) +
		`,"connection_id":` + sqlExecutionJSON(customer.Connection.ConnectionID) +
		`,"connector_id":` + sqlExecutionJSON(sqlExecutionConnectorID) +
		`,"host":` + sqlExecutionJSON(sqlExecutionServerName) +
		`,"port":` + strconv.Itoa(int(customerDatabase.port)) +
		`,"database":` + sqlExecutionJSON(customerDatabase.database) +
		`,"user":` + sqlExecutionJSON(customerDatabase.role) +
		`,"tls_trust_ref":"secret://fixture/provider-sql"` +
		`,"password_ref":` + sqlExecutionJSON(passwordRef) +
		`,"state_directory":` + sqlExecutionJSON(customerDatabase.stateDir) +
		`,"relation_bindings":[{"binding":` + sqlExecutionJSON(sqlExecutionBinding) +
		`,"schema":` + sqlExecutionJSON(sqlExecutionSchema) +
		`,"table":` + sqlExecutionJSON(sqlExecutionRelation) +
		`,"key_columns":` + string(encodedKeys) +
		`,"snapshot":{"provider_account_id":` + sqlExecutionJSON(binding.ProviderAccountID) +
		`,"permission_revision":` + sqlExecutionJSON(binding.PermissionRevision) +
		`,"resource":` + sqlExecutionJSON(binding.Resource) +
		`,"source_revision":` + sqlExecutionJSON(binding.SourceRevision) + `}}]}`
	return `{"max_connect_duration":"` + sqlExecutionConnect +
		`","max_statement_duration":"` + sqlExecutionStatement +
		`","max_total_duration":"` + sqlExecutionDuration +
		`","max_rows":` + strconv.Itoa(sqlExecutionMaxRows) +
		`,"max_bytes":` + strconv.Itoa(sqlExecutionMaxBytes) +
		`,"max_attempts":` + strconv.Itoa(sqlExecutionAttempts) +
		`,"connections":[` + connection + `]}`
}

// sqlExecutionStartupDocument places the declared provider_sql block in one
// complete startup document, so the declaration this case composes is validated
// by the same strict configuration boundary an operator deployment uses.
func sqlExecutionStartupDocument(providerSQL string) string {
	return `{"schema_version":"1","deployment_id":"sql-execution-fixture","public_origin":"https://pm.example.invalid",` +
		`"postgres":{"ownership":"managed","database":"pm_app","endpoint_ref":"secret://fixture/postgres"},` +
		`"temporal":{"ownership":"managed","endpoint":"temporal.example.invalid:7233","namespace":"fixture","client_identity_ref":"secret://fixture/temporal","retention":"720h","task_queues":["pm-control"]},` +
		`"artifacts":{"ownership":"managed","driver":"s3","endpoint":"https://objects.example.invalid","bucket":"fixture-objects","credential_ref":"secret://fixture/objects","attempt_key_policy":"unique_immutable"},` +
		`"identity":{"issuer":"https://identity.example.invalid","client_id":"pm-server","client_secret_ref":"secret://fixture/oidc"},` +
		`"encryption":{"wrapping_key_ref":"secret://fixture/wrapping"},` +
		`"admission":{"profile":"development"},` +
		`"limits":{"workspace_active_runs":4,"workflow_payload_bytes":65536,"workflow_fanout":32,"run_lifetime":"1h","event_buffer":128,"query_children":1,"query_child_memory_bytes":4294967296,"query_child_deadline":"5m","request_bytes":1048576,"metadata_deadline":"5s"},` +
		`"provider_sql":` + providerSQL + `}`
}

func sqlExecutionJSON(value string) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic("an exact declared fact cannot be encoded: " + err.Error())
	}
	return string(encoded)
}

func slicesEqualStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
