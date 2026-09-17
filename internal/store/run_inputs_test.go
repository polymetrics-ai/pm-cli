package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const runInputTestMaxBytes = uint64(1 << 20)

func TestAdmissionRunInputSnapshotAtomicity(t *testing.T) {
	credentials, pool := prepareTestStore(t, 4)
	_, observer := prepareTestStore(t, 2)
	scope := newTestScope(t)
	t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	acceptedRequest := testAdmissionRequest(t, scope, "input-atomic-accepted", "input-atomic-accepted")
	accepted := admitForTest(t, pool, scope, acceptedRequest)
	if accepted.RunID != acceptedRequest.Spec.RunID || accepted.Replayed {
		t.Fatalf("initial admission = %#v; want new run %q", accepted, acceptedRequest.Spec.RunID)
	}
	var stored RunInputSnapshot
	if err := WithinScope(ctx, observer, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		stored, err = LoadRunInputSnapshot(ctx, tx, acceptedRequest.Spec.RunID)
		return err
	}); err != nil {
		t.Fatalf("load accepted input from independent transaction: %v", err)
	}
	requireRunInputSnapshotEqual(t, acceptedRequest.InputSnapshot, stored)

	invalidBound := testAdmissionRequest(t, scope, "input-atomic-bound", "input-atomic-bound")
	invalidPolicy := invalidBound.BudgetSnapshot.Policy
	invalidPolicy.InputMaxBytes = 1
	setAdmissionPolicyForTest(t, &invalidBound, invalidPolicy)
	err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := AdmitRun(ctx, tx, invalidBound)
		return err
	})
	if !errors.Is(err, ErrRunInputTooLarge) {
		t.Fatalf("invalid input bound admission error = %v; want ErrRunInputTooLarge", err)
	}
	requireNoAcceptedRunInput(t, ctx, observer, scope, invalidBound)

	lateFailure := testAdmissionRequest(t, scope, "input-atomic-late", "input-atomic-late")
	lateFailure.Spec.ExecutionGrantRef = acceptedRequest.Spec.ExecutionGrantRef
	lateFailure.Grant.GrantRef = acceptedRequest.Spec.ExecutionGrantRef
	if err := lateFailure.Spec.Validate(); err != nil {
		t.Fatal(err)
	}
	err = WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := AdmitRun(ctx, tx, lateFailure)
		return err
	})
	if !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("late admission failure = %v; want ErrImmutableConflict", err)
	}
	requireNoAcceptedRunInput(t, ctx, observer, scope, lateFailure)

	replay := acceptedRequest
	replay.Spec.RunID = randomUUID(t)
	rebindAdmissionBudgetForTest(t, &replay)
	replay.InputSnapshot.RunID = replay.Spec.RunID
	replay.InputSnapshot.Input = contract.NewObjectValue(map[string]contract.PresentValue{
		"seed": contract.NewStringValue("replacement-must-not-persist"),
	})
	binding, err := contract.NewAPIWorkflowBinding(replay.Spec.RunID)
	if err != nil {
		t.Fatal(err)
	}
	replay.WorkflowBinding = binding
	replay.Receipt = testReceipt("receipt-unused-input-replay", "admission", replay.Spec.RunID)
	replay.Audit = testAudit("audit-unused-input-replay", "run_admitted", replay.Spec.RunID)
	replay.Event = testEvent("event-unused-input-replay", "run_admitted", replay.Spec.RunID)
	replay.Outbox = testOutbox("outbox-unused-input-replay", "start_run", replay.Spec.RunID)
	replayed := admitForTest(t, pool, scope, replay)
	if replayed.RunID != acceptedRequest.Spec.RunID || !replayed.Replayed {
		t.Fatalf("input replay result = %#v; want original RunID and Replayed", replayed)
	}
	if err := WithinScope(ctx, observer, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		stored, err = LoadRunInputSnapshot(ctx, tx, acceptedRequest.Spec.RunID)
		return err
	}); err != nil {
		t.Fatalf("reload original input after replay: %v", err)
	}
	requireRunInputSnapshotEqual(t, acceptedRequest.InputSnapshot, stored)
	requireRunInputNotFound(t, ctx, observer, scope, replay.Spec.RunID)
}

func TestRunInputSnapshotAtomicPersistence(t *testing.T) {
	credentials, writer := prepareTestStore(t, 4)
	_, observer := prepareTestStore(t, 2)
	scope := newTestScope(t)
	t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	spec, snapshot := testRunInputSnapshot(t, scope, "atomic")
	if err := WithinScope(ctx, writer, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		if err := insertRunSpecForInputTest(ctx, tx, spec); err != nil {
			return err
		}
		if err := PutRunInputSnapshot(ctx, tx, spec, snapshot, runInputTestMaxBytes); err != nil {
			return err
		}
		return WithinScope(ctx, observer, scope, testActor(scope), func(ctx context.Context, other *Transaction) error {
			if _, err := LoadRunInputSnapshot(ctx, other, spec.RunID); err == nil {
				return errors.New("uncommitted run input became independently visible")
			} else if !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("observe uncommitted run input: %w", err)
			}
			return nil
		})
	}); err != nil {
		t.Fatalf("commit run input snapshot: %v", err)
	}

	writer.Close()
	reopened := openRunInputTestPool(t, ctx, credentials.runtimeDSN)
	t.Cleanup(reopened.Close)
	var loaded RunInputSnapshot
	if err := WithinScope(ctx, reopened, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		loaded, err = LoadRunInputSnapshot(ctx, tx, spec.RunID)
		return err
	}); err != nil {
		t.Fatalf("load run input after reopening store: %v", err)
	}
	requireRunInputSnapshotEqual(t, snapshot, loaded)

	rollbackSpec, rollbackSnapshot := testRunInputSnapshot(t, scope, "rollback")
	stop := errors.New("injected rollback after run input write")
	err := WithinScope(ctx, reopened, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		if err := insertRunSpecForInputTest(ctx, tx, rollbackSpec); err != nil {
			return err
		}
		if err := PutRunInputSnapshot(ctx, tx, rollbackSpec, rollbackSnapshot, runInputTestMaxBytes); err != nil {
			return err
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("rollback transaction error = %v; want injected rollback", err)
	}
	if err := WithinScope(ctx, observer, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := LoadRunInputSnapshot(ctx, tx, rollbackSpec.RunID)
		return err
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rolled-back run input load error = %v; want ErrNotFound", err)
	}
}

func TestRunInputSnapshotBindingsAndImmutability(t *testing.T) {
	credentials, pool := prepareTestStore(t, 4)
	scope := newTestScope(t)
	foreign := contract.Scope{
		OrgID:       scope.OrgID,
		WorkspaceID: "workspace-" + randomHex(t, 8),
	}
	t.Cleanup(func() { cleanupTestScope(t, credentials, scope, foreign) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	spec, snapshot := testRunInputSnapshot(t, scope, "original")
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		if err := insertRunSpecForInputTest(ctx, tx, spec); err != nil {
			return err
		}
		return PutRunInputSnapshot(ctx, tx, spec, snapshot, runInputTestMaxBytes)
	}); err != nil {
		t.Fatalf("persist original run input: %v", err)
	}

	t.Run("exact_duplicate_is_refused", func(t *testing.T) {
		err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			return PutRunInputSnapshot(ctx, tx, spec, snapshot, runInputTestMaxBytes)
		})
		if !errors.Is(err, ErrImmutableConflict) {
			t.Fatalf("duplicate error = %v; want ErrImmutableConflict", err)
		}
	})

	t.Run("scoped_input_reference_collision_is_refused", func(t *testing.T) {
		collidingSpec, collidingSnapshot := testRunInputSnapshot(t, scope, "collision")
		collidingSpec.InputRefs = []string{snapshot.InputRef}
		collidingSnapshot.InputRef = snapshot.InputRef
		if err := collidingSpec.Validate(); err != nil {
			t.Fatal(err)
		}
		err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			if err := insertRunSpecForInputTest(ctx, tx, collidingSpec); err != nil {
				return err
			}
			return PutRunInputSnapshot(ctx, tx, collidingSpec, collidingSnapshot, runInputTestMaxBytes)
		})
		if !errors.Is(err, ErrImmutableConflict) {
			t.Fatalf("input reference collision error = %v; want ErrImmutableConflict", err)
		}
	})

	t.Run("supplied_RunSpec_must_match_durable_RunSpec", func(t *testing.T) {
		storedSpec, candidate := testRunInputSnapshot(t, scope, "spec-binding")
		if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			return insertRunSpecForInputTest(ctx, tx, storedSpec)
		}); err != nil {
			t.Fatal(err)
		}
		changedSpec := storedSpec
		changedSpec.OperationOrPlanRef.Revision = "revision-changed"
		if err := changedSpec.Validate(); err != nil {
			t.Fatal(err)
		}
		err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			return PutRunInputSnapshot(ctx, tx, changedSpec, candidate, runInputTestMaxBytes)
		})
		if !errors.Is(err, ErrImmutableConflict) {
			t.Fatalf("changed RunSpec binding error = %v; want ErrImmutableConflict", err)
		}
		requireRunInputNotFound(t, ctx, pool, scope, storedSpec.RunID)
	})

	t.Run("input_reference_must_be_declared_by_RunSpec", func(t *testing.T) {
		storedSpec, candidate := testRunInputSnapshot(t, scope, "ref-binding")
		if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			return insertRunSpecForInputTest(ctx, tx, storedSpec)
		}); err != nil {
			t.Fatal(err)
		}
		candidate.InputRef = "input-unbound"
		err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			return PutRunInputSnapshot(ctx, tx, storedSpec, candidate, runInputTestMaxBytes)
		})
		if !errors.Is(err, ErrImmutableConflict) {
			t.Fatalf("unbound input reference error = %v; want ErrImmutableConflict", err)
		}
		requireRunInputNotFound(t, ctx, pool, scope, storedSpec.RunID)
	})

	t.Run("cross_scope_access_is_refused_but_reference_namespace_is_scoped", func(t *testing.T) {
		if err := WithinScope(ctx, pool, foreign, testActor(foreign), func(ctx context.Context, tx *Transaction) error {
			_, err := LoadRunInputSnapshot(ctx, tx, spec.RunID)
			return err
		}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign load error = %v; want ErrNotFound", err)
		}
		if err := WithinScope(ctx, pool, foreign, testActor(foreign), func(ctx context.Context, tx *Transaction) error {
			return PutRunInputSnapshot(ctx, tx, spec, snapshot, runInputTestMaxBytes)
		}); !errors.Is(err, ErrScopeMismatch) {
			t.Fatalf("foreign write error = %v; want ErrScopeMismatch", err)
		}

		foreignSpec, foreignSnapshot := testRunInputSnapshot(t, foreign, "foreign")
		foreignSpec.InputRefs = []string{snapshot.InputRef}
		foreignSnapshot.InputRef = snapshot.InputRef
		if err := foreignSpec.Validate(); err != nil {
			t.Fatal(err)
		}
		if err := WithinScope(ctx, pool, foreign, testActor(foreign), func(ctx context.Context, tx *Transaction) error {
			if err := insertRunSpecForInputTest(ctx, tx, foreignSpec); err != nil {
				return err
			}
			return PutRunInputSnapshot(ctx, tx, foreignSpec, foreignSnapshot, runInputTestMaxBytes)
		}); err != nil {
			t.Fatalf("same input reference in independent scope: %v", err)
		}
		var visible int
		if err := WithinScope(ctx, pool, foreign, testActor(foreign), func(ctx context.Context, tx *Transaction) error {
			return tx.queryRow(ctx, `
				SELECT count(*)
				FROM pm.run_input_snapshots
				WHERE input_ref = $1
			`, snapshot.InputRef).Scan(&visible)
		}); err != nil {
			t.Fatal(err)
		}
		if visible != 1 {
			t.Fatalf("foreign scope observed %d rows for a shared input reference; want its one row", visible)
		}
	})

	t.Run("runtime_cannot_update_or_delete_snapshot", func(t *testing.T) {
		statements := []string{
			`UPDATE pm.run_input_snapshots SET connection_id = connection_id WHERE run_id = $1`,
			`DELETE FROM pm.run_input_snapshots WHERE run_id = $1`,
		}
		for _, statement := range statements {
			err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
				_, err := tx.exec(ctx, statement, spec.RunID)
				return err
			})
			var databaseError *pgconn.PgError
			if !errors.As(err, &databaseError) || databaseError.Code != "42501" {
				t.Fatalf("immutable statement error = %v; want SQLSTATE 42501", err)
			}
		}
	})

	var unchanged RunInputSnapshot
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		unchanged, err = LoadRunInputSnapshot(ctx, tx, spec.RunID)
		return err
	}); err != nil {
		t.Fatalf("load original after refused mutations: %v", err)
	}
	requireRunInputSnapshotEqual(t, snapshot, unchanged)
}

func TestRunInputSnapshotValidationAndByteBound(t *testing.T) {
	credentials, pool := prepareTestStore(t, 3)
	scope := newTestScope(t)
	t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	spec, snapshot := testRunInputSnapshot(t, scope, "bounded")
	snapshot.Input = contract.NewObjectValue(map[string]contract.PresentValue{
		"payload": contract.NewStringValue(strings.Repeat("x", 4096)),
	})
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return insertRunSpecForInputTest(ctx, tx, spec)
	}); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}

	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return PutRunInputSnapshot(ctx, tx, spec, snapshot, 0)
	}); !errors.Is(err, ErrRunInputBoundInvalid) {
		t.Fatalf("zero byte bound error = %v; want ErrRunInputBoundInvalid", err)
	}
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return PutRunInputSnapshot(ctx, tx, spec, snapshot, uint64(len(encoded)-1))
	}); !errors.Is(err, ErrRunInputTooLarge) {
		t.Fatalf("undersized byte bound error = %v; want ErrRunInputTooLarge", err)
	}

	invalidInput := snapshot
	invalidInput.Input = contract.NullValue()
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return PutRunInputSnapshot(ctx, tx, spec, invalidInput, uint64(len(encoded)))
	}); err == nil {
		t.Fatal("null logical input was accepted")
	}
	invalidFieldID := snapshot
	invalidFieldID.Input = contract.NewObjectValue(map[string]contract.PresentValue{
		"": contract.NewStringValue("value"),
	})
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return PutRunInputSnapshot(ctx, tx, spec, invalidFieldID, uint64(len(encoded)))
	}); err == nil {
		t.Fatal("invalid logical field ID was accepted")
	}
	invalidControls := snapshot
	invalidControls.ResultControls.Selection = &contract.FieldSelection{Paths: []contract.FieldPath{}}
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return PutRunInputSnapshot(ctx, tx, spec, invalidControls, uint64(len(encoded)))
	}); !errors.Is(err, contract.ErrInvalidControls) {
		t.Fatalf("invalid result controls error = %v; want contract.ErrInvalidControls", err)
	}
	requireRunInputNotFound(t, ctx, pool, scope, spec.RunID)

	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return PutRunInputSnapshot(ctx, tx, spec, snapshot, uint64(len(encoded)))
	}); err != nil {
		t.Fatalf("persist snapshot at exact byte bound: %v", err)
	}
	var loaded RunInputSnapshot
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		loaded, err = LoadRunInputSnapshot(ctx, tx, spec.RunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	requireRunInputSnapshotEqual(t, snapshot, loaded)
}

func TestRunInputSnapshotApprovalPresenceRoundTrip(t *testing.T) {
	credentials, pool := prepareTestStore(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, test := range []struct {
		name  string
		value contract.PresentValue
	}{
		{name: "absent", value: contract.AbsentValue()},
		{name: "null", value: contract.NullValue()},
		{name: "string", value: contract.NewStringValue("approval-fixture")},
	} {
		t.Run(test.name, func(t *testing.T) {
			scope := newTestScope(t)
			t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })
			spec, snapshot := testRunInputSnapshot(t, scope, "approval-"+test.name)
			snapshot.ApprovalRef = test.value
			if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
				if err := insertRunSpecForInputTest(ctx, tx, spec); err != nil {
					return err
				}
				return PutRunInputSnapshot(ctx, tx, spec, snapshot, runInputTestMaxBytes)
			}); err != nil {
				t.Fatalf("persist %s approval presence: %v", test.name, err)
			}
			var loaded RunInputSnapshot
			if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
				var err error
				loaded, err = LoadRunInputSnapshot(ctx, tx, spec.RunID)
				return err
			}); err != nil {
				t.Fatalf("load %s approval presence: %v", test.name, err)
			}
			if !loaded.ApprovalRef.Equal(test.value) {
				t.Fatalf("approval presence changed across storage: got %#v", loaded.ApprovalRef)
			}
		})
	}
}

func testRunInputSnapshot(t *testing.T, scope contract.Scope, seed string) (contract.RunSpec, RunInputSnapshot) {
	t.Helper()
	request := testAdmissionRequest(t, scope, "run-input-"+seed, "run-input-"+seed)
	inputRef := "input-" + seed
	request.Spec.InputRefs = []string{inputRef}
	if err := request.Spec.Validate(); err != nil {
		t.Fatalf("construct run input RunSpec: %v", err)
	}
	zero, err := contract.NewIntegerValue("0")
	if err != nil {
		t.Fatal(err)
	}
	exact, err := contract.NewDecimalValue("9007199254740993.00", 18, 2)
	if err != nil {
		t.Fatal(err)
	}
	filterValue := contract.NewBooleanValue(false)
	return request.Spec, RunInputSnapshot{
		Scope:        scope,
		RunID:        request.Spec.RunID,
		InputRef:     inputRef,
		ConnectionID: "connection-" + seed,
		Input: contract.NewObjectValue(map[string]contract.PresentValue{
			"absent":       contract.AbsentValue(),
			"null":         contract.NullValue(),
			"false":        contract.NewBooleanValue(false),
			"zero":         zero,
			"empty_string": contract.NewStringValue(""),
			"empty_array":  contract.NewArrayValue([]contract.PresentValue{}),
			"empty_object": contract.NewObjectValue(map[string]contract.PresentValue{}),
			"exact_number": exact,
		}),
		ResultControls: contract.ResultControls{
			Selection: &contract.FieldSelection{Paths: []contract.FieldPath{{"record", "id"}, {"record", "name"}}},
			Filter: &contract.RecordFilter{
				Path:     contract.FieldPath{"enabled"},
				Operator: contract.FilterEqual,
				Value:    &filterValue,
			},
		},
		ApprovalRef: contract.AbsentValue(),
	}
}

func insertRunSpecForInputTest(ctx context.Context, tx *Transaction, spec contract.RunSpec) error {
	if err := spec.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	tag, err := tx.exec(ctx, `
		INSERT INTO pm.run_specs (
			org_id, workspace_id, run_id, intent_digest, idempotency_fingerprint, spec, accepted_at
		) VALUES ($1, $2, $3, $4, $4, $5, clock_timestamp())
	`, spec.OrgID, spec.WorkspaceID, spec.RunID, string(spec.IdempotencyFingerprint), json.RawMessage(encoded))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("test RunSpec was not inserted")
	}
	return nil
}

func openRunInputTestPool(t *testing.T, ctx context.Context, connectionString string) *Pool {
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
		t.Fatalf("reopen selected store for run input: %T", err)
	}
	return pool
}

func requireNoAcceptedRunInput(
	t *testing.T,
	ctx context.Context,
	pool *Pool,
	scope contract.Scope,
	request AdmissionRequest,
) {
	t.Helper()
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		if _, err := LookupAdmission(ctx, tx, request.Key); err == nil {
			return errors.New("admission identity remained after refusal")
		} else if !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("inspect admission identity after refusal: %w", err)
		}
		if _, err := LoadRunInputSnapshot(ctx, tx, request.Spec.RunID); err == nil {
			return errors.New("run input remained after refusal")
		} else if !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("inspect run input after refusal: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func requireRunInputNotFound(t *testing.T, ctx context.Context, pool *Pool, scope contract.Scope, runID string) {
	t.Helper()
	err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := LoadRunInputSnapshot(ctx, tx, runID)
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("run input load error = %v; want ErrNotFound", err)
	}
}

func requireRunInputSnapshotEqual(t *testing.T, want, got RunInputSnapshot) {
	t.Helper()
	if !want.Scope.Equal(got.Scope) || want.RunID != got.RunID || want.InputRef != got.InputRef ||
		want.ConnectionID != got.ConnectionID || !want.Input.Equal(got.Input) ||
		!reflect.DeepEqual(want.ResultControls, got.ResultControls) ||
		!want.ApprovalRef.Equal(got.ApprovalRef) {
		t.Fatalf("loaded run input snapshot differs from exact accepted input\nwant: %#v\n got: %#v", want, got)
	}
}
