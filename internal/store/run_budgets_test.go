package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestAdmissionPersistsExactRunBudgetSnapshot(t *testing.T) {
	credentials, writer := prepareTestStore(t, 4)
	_, observer := prepareTestStore(t, 2)
	scope := newTestScope(t)
	t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	request := testAdmissionRequest(t, scope, "budget-accepted", "budget-accepted")
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
	if loaded != request.BudgetSnapshot || loaded.Binding() != request.Spec.Budget {
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
		if loaded != request.BudgetSnapshot {
			return errors.New("refused runtime mutation changed immutable run budget")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
