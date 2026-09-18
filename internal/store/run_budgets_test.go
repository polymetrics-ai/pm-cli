package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestAdmissionPersistsExactRunBudgetSnapshot(t *testing.T) {
	credentials, writer := prepareTestStore(t, 4)
	_, observer := prepareTestStore(t, 2)
	scope := newTestScope(t)
	t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	request := testAdmissionRequest(t, scope, "budget-accepted", "budget-accepted")
	httpPolicy := contract.HTTPExecutionPolicy{
		MaxRequestBytes:  1024,
		MaxResponseBytes: 4096,
		MaxDNSAnswers:    8,
		MaxAttempts:      3,
		MaxRetryDelay:    2 * time.Second,
		SelectedOrigin:   "https://api.example.test/v1",
	}
	policy := request.BudgetSnapshot.Policy
	policy.HTTP = &httpPolicy
	setAdmissionPolicyForTest(t, &request, policy)
	admittedHTTP := httpPolicy
	httpPolicy.MaxAttempts = 4
	httpPolicy.SelectedOrigin = "https://mutated.example.test/v2"
	accepted := admitForTest(t, writer, scope, request)
	if accepted.RunID != request.Spec.RunID || accepted.Replayed {
		t.Fatalf("budgeted admission = %#v; want fresh run %q", accepted, request.Spec.RunID)
	}
	var loaded RunBudgetSnapshot
	if err := WithinScope(ctx, observer, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		loaded, err = LoadRunBudgetSnapshot(ctx, tx, request.Spec.RunID)
		return err
	}); err != nil {
		t.Fatalf("load committed run budget independently: %v", err)
	}
	if !reflect.DeepEqual(loaded, request.BudgetSnapshot) || loaded.Binding() != request.Spec.Budget {
		t.Fatalf("stored run budget differs from applied policy\nwant: %#v\n got: %#v", request.BudgetSnapshot, loaded)
	}

	replayed := admitForTest(t, writer, scope, request)
	if !replayed.Replayed || replayed.RunID != request.Spec.RunID {
		t.Fatalf("budgeted replay = %#v; want original run", replayed)
	}
	if err := WithinScope(ctx, observer, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var count int
		if err := tx.queryRow(ctx, `
			SELECT count(*)
			FROM pm.run_budget_snapshots
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
		`, scope.OrgID, scope.WorkspaceID, request.Spec.RunID).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return errors.New("exact admission replay duplicated its immutable budget")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if loaded.Policy.HTTP == nil || *loaded.Policy.HTTP != admittedHTTP {
		t.Fatalf("stored HTTP policy = %#v; want owned admitted value %#v", loaded.Policy.HTTP, admittedHTTP)
	}
	tamperRunBudgetHTTPFixture(t, ctx, credentials.migratorDSN, scope, request.Spec.RunID)
	err := WithinScope(ctx, observer, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := LoadRunBudgetSnapshot(ctx, tx, request.Spec.RunID)
		return err
	})
	if !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("load HTTP policy changed without matching digest = %v; want ErrImmutableConflict", err)
	}
}

func TestRunBudgetBindingAndImmutability(t *testing.T) {
	credentials, pool := prepareTestStore(t, 3)
	scope := newTestScope(t)
	t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mismatch := testAdmissionRequest(t, scope, "budget-mismatch", "budget-mismatch")
	mismatch.Spec.Budget.Revision++
	err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := AdmitRun(ctx, tx, mismatch)
		return err
	})
	if !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("mismatched budget binding = %v; want ErrImmutableConflict", err)
	}
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		if _, err := LoadRunBudgetSnapshot(ctx, tx, mismatch.Spec.RunID); !errors.Is(err, ErrNotFound) {
			return errors.New("refused admission retained a budget snapshot")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	request := testAdmissionRequest(t, scope, "budget-immutable", "budget-immutable")
	admitForTest(t, pool, scope, request)
	for _, statement := range []string{
		`UPDATE pm.run_budget_snapshots SET revision = revision + 1 WHERE run_id = $1`,
		`DELETE FROM pm.run_budget_snapshots WHERE run_id = $1`,
	} {
		err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			_, err := tx.exec(ctx, statement, request.Spec.RunID)
			return err
		})
		var databaseError *pgconn.PgError
		if !errors.As(err, &databaseError) || databaseError.Code != "42501" {
			t.Fatalf("immutable budget mutation = %v; want SQLSTATE 42501", err)
		}
	}
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		loaded, err := LoadRunBudgetSnapshot(ctx, tx, request.Spec.RunID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(loaded, request.BudgetSnapshot) {
			return errors.New("refused runtime mutation changed immutable run budget")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRunBudgetPolicyWithoutHTTPRetainsCanonicalEncoding(t *testing.T) {
	encoded, err := json.Marshal(testRunBudgetPolicy())
	if err != nil {
		t.Fatal(err)
	}
	const priorEncoding = `{"pending":{"global_pending":256,"workspace_pending":32,"workspace_max_pending_age":600000000000},"run_lifetime":86400000000000,"input_max_bytes":1048576,"workspace_active_runs":32,"workflow_payload_bytes":65536,"workflow_fanout":32}`
	if string(encoded) != priorEncoding {
		t.Fatalf("absent HTTP policy encoding changed\nwant: %s\n got: %s", priorEncoding, encoded)
	}
}

func tamperRunBudgetHTTPFixture(
	t *testing.T,
	ctx context.Context,
	migratorDSN string,
	scope contract.Scope,
	runID string,
) {
	t.Helper()
	connection, err := pgx.Connect(ctx, migratorDSN)
	if err != nil {
		t.Fatalf("open owned run budget digest fixture: %T", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := detachedCleanupContext()
		defer cleanupCancel()
		if err := connection.Close(cleanupCtx); err != nil {
			t.Errorf("close owned run budget digest fixture: %T", err)
		}
	}()
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := detachedCleanupContext()
		defer cleanupCancel()
		if err := tx.Rollback(cleanupCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("close owned run budget digest transaction: %T", err)
		}
	}()
	if _, err := tx.Exec(ctx, `
		SELECT set_config('pm.org_id', $1, true), set_config('pm.workspace_id', $2, true)
	`, scope.OrgID, scope.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE pm.run_budget_snapshots
		SET snapshot = jsonb_set(
			snapshot,
			'{policy,http,max_attempts}',
			to_jsonb((snapshot #>> '{policy,http,max_attempts}')::bigint + 1),
			false
		)
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
	`, scope.OrgID, scope.WorkspaceID, runID)
	if err != nil {
		t.Fatalf("change owned stored HTTP policy fixture: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("changed stored HTTP policy rows = %d; want 1", tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
