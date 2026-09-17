package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestAdmissionCommitVisibility(t *testing.T) {
	credentials, pool := prepareTestStore(t, 4)
	scope := newTestScope(t)
	t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })
	request := testAdmissionRequest(t, scope, "durable-key", "durable")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		if _, err := AdmitRun(ctx, tx, request); err != nil {
			return err
		}
		// This observer necessarily uses another pooled connection while the
		// admission transaction remains open.
		return WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, observer *Transaction) error {
			if _, err := LookupAdmission(ctx, observer, request.Key); !errors.Is(err, ErrNotFound) {
				t.Fatalf("uncommitted admission became visible: %v", err)
			}
			var commands int
			if err := observer.queryRow(ctx, `SELECT count(*) FROM pm.outbox_commands WHERE run_id = $1`, request.Spec.RunID).Scan(&commands); err != nil {
				return err
			}
			if commands != 0 {
				t.Fatal("uncommitted outbox command became observable")
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, observer *Transaction) error {
		accepted, err := LookupAdmission(ctx, observer, request.Key)
		if err != nil {
			return err
		}
		var commands int
		if err := observer.queryRow(ctx, `SELECT count(*) FROM pm.outbox_commands WHERE run_id = $1`, request.Spec.RunID).Scan(&commands); err != nil {
			return err
		}
		if accepted.RunID != request.Spec.RunID || commands != 1 {
			t.Fatal("committed admission and its single outbox command were not visible together")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStoreAdmissionCommit(t *testing.T) {
	credentials, pool := prepareTestStore(t, 4)
	scope := newTestScope(t)
	t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })

	t.Run("rollback_before_commit_leaves_no_accepted_identity", func(t *testing.T) {
		request := testAdmissionRequest(t, scope, "rollback-key", "rollback")
		injected := errors.New("injected precommit rollback")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			if _, err := AdmitRun(ctx, tx, request); err != nil {
				return err
			}
			return injected
		})
		if !errors.Is(err, injected) {
			t.Fatalf("rolled-back admission error = %v; want injected failure", err)
		}
		if errors.Is(err, ErrCommitOutcomeUnknown) {
			t.Fatal("confirmed precommit rollback was classified as an unknown commit")
		}
		if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			if _, err := LookupAdmission(ctx, tx, request.Key); !errors.Is(err, ErrNotFound) {
				t.Fatalf("rolled-back idempotency identity error = %v; want ErrNotFound", err)
			}
			if _, err := LoadRun(ctx, tx, request.Spec.RunID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("rolled-back RunID error = %v; want ErrNotFound", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	request := testAdmissionRequest(t, scope, "durable-key", "durable")
	accepted := admitForTest(t, pool, scope, request)
	if accepted.RunID != request.Spec.RunID || accepted.Replayed {
		t.Fatalf("initial admission = %#v; want new run %q", accepted, request.Spec.RunID)
	}
	pool.Close()
	reopenCtx, reopenCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer reopenCancel()
	reopened, err := OpenPool(reopenCtx, PoolConfig{
		ConnectionString:  credentials.runtimeDSN,
		MaxConns:          4,
		MinConns:          0,
		MaxConnLifetime:   5 * time.Minute,
		MaxConnIdleTime:   time.Minute,
		HealthCheckPeriod: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("reopen selected store: %T", err)
	}
	t.Cleanup(reopened.Close)

	t.Run("identical_replay_returns_original_run_without_second_durable_rows", func(t *testing.T) {
		replay := request
		replay.Spec.RunID = randomUUID(t)
		rebindAdmissionBudgetForTest(t, &replay)
		replay.InputSnapshot.RunID = replay.Spec.RunID
		binding, err := contract.NewAPIWorkflowBinding(replay.Spec.RunID)
		if err != nil {
			t.Fatal(err)
		}
		replay.WorkflowBinding = binding
		replay.Receipt = testReceipt("receipt-unused-replay", "admission", replay.Spec.RunID)
		replay.Audit = testAudit("audit-unused-replay", "run_admitted", replay.Spec.RunID)
		replay.Event = testEvent("event-unused-replay", "run_admitted", replay.Spec.RunID)
		replay.Outbox = testOutbox("outbox-unused-replay", "start_run", replay.Spec.RunID)

		result := admitForTest(t, reopened, scope, replay)
		if result.RunID != request.Spec.RunID || !result.Replayed {
			t.Fatalf("replay result = %#v; want original RunID and Replayed", result)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := WithinScope(ctx, reopened, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			var runs, outbox, reservations, events int
			if err := tx.queryRow(ctx, `SELECT count(*) FROM pm.run_specs WHERE run_id = $1`, request.Spec.RunID).Scan(&runs); err != nil {
				return err
			}
			if err := tx.queryRow(ctx, `SELECT count(*) FROM pm.outbox_commands WHERE run_id = $1`, request.Spec.RunID).Scan(&outbox); err != nil {
				return err
			}
			if err := tx.queryRow(ctx, `SELECT count(*) FROM pm.pending_reservations WHERE run_id = $1`, request.Spec.RunID).Scan(&reservations); err != nil {
				return err
			}
			if err := tx.queryRow(ctx, `SELECT count(*) FROM pm.server_events WHERE run_id = $1`, request.Spec.RunID).Scan(&events); err != nil {
				return err
			}
			if runs != 1 || outbox != 1 || reservations != 1 || events != 1 {
				t.Fatalf("durable row counts run=%d outbox=%d reservation=%d events=%d; want one each", runs, outbox, reservations, events)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("changed_digest_returns_typed_conflict_and_preserves_original", func(t *testing.T) {
		changed := testAdmissionRequest(t, scope, request.Key.IdempotencyKey, "changed")
		changed.Key = request.Key
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := WithinScope(ctx, reopened, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			_, err := AdmitRun(ctx, tx, changed)
			return err
		})
		var conflict *IdempotencyConflictError
		if !errors.As(err, &conflict) || !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("changed digest error = %v; want IdempotencyConflictError", err)
		}
		if conflict.ExistingRunID != request.Spec.RunID {
			t.Fatalf("conflict existing RunID = %q; want %q", conflict.ExistingRunID, request.Spec.RunID)
		}
		if err := WithinScope(ctx, reopened, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			stored, err := LookupAdmission(ctx, tx, request.Key)
			if err != nil {
				return err
			}
			if stored.RunID != request.Spec.RunID || stored.IntentDigest != request.Spec.IdempotencyFingerprint {
				t.Fatalf("stored admission changed after conflict: %#v", stored)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}
