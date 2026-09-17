package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestAdmissionReceiptRetention(t *testing.T) {
	credentials, pool := prepareTestStore(t, 4)
	newRequest := func(t *testing.T, seed string) (contract.Scope, AdmissionRequest) {
		t.Helper()
		scope := newTestScope(t)
		t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })
		request := testAdmissionRequest(t, scope, "retention-"+seed, "retention-"+seed)
		admitForTest(t, pool, scope, request)
		return scope, request
	}
	terminal := func(ctx context.Context, scope contract.Scope, request AdmissionRequest, rollback error) error {
		return WithinScope(ctx, pool, scope, request.Spec.Actor, func(ctx context.Context, tx *Transaction) error {
			run, err := LoadRun(ctx, tx, request.Spec.RunID)
			if err != nil {
				return err
			}
			now, err := readDatabaseTime(ctx, tx)
			if err != nil {
				return err
			}
			failed, err := run.Transition(contract.RunStateFailed, "startup_failed", now)
			if err != nil {
				return err
			}
			_, err = CommitRunTransition(ctx, tx, RunTransitionCommit{
				Run:     failed,
				Receipt: testReceipt("retention-terminal-receipt", "run_transition", run.Spec.RunID),
				Audit:   testAudit("retention-terminal-audit", "run_transition", run.Spec.RunID),
				Event:   testEvent("retention-terminal-event", "run_transition", run.Spec.RunID),
				Outbox:  testOutbox("retention-terminal-outbox", "project_run", run.Spec.RunID),
			})
			if err != nil {
				return err
			}
			return rollback
		})
	}
	compact := func(ctx context.Context, scope contract.Scope, request AdmissionRequest) (bool, error) {
		var changed bool
		err := WithinScope(ctx, pool, scope, request.Spec.Actor, func(ctx context.Context, tx *Transaction) error {
			var err error
			changed, err = CompactAdmissionReceipt(ctx, tx, request.Key)
			return err
		})
		return changed, err
	}

	t.Run("old_unresolved_acceptance_never_expires", func(t *testing.T) {
		scope, request := newRequest(t, "unresolved")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		ageAdmissionReceiptFixture(t, ctx, credentials.migratorDSN, scope, request.Spec.RunID, 169*time.Hour, false)
		if changed, err := compact(ctx, scope, request); err != nil || changed {
			t.Fatalf("unresolved compaction = %v, %v", changed, err)
		}
		replayed, err := tryPendingAdmission(ctx, pool, request)
		if err != nil || !replayed.Replayed || replayed.ReceiptExpired || replayed.RunID != request.Spec.RunID {
			t.Fatalf("unresolved recovery = %#v, %v", replayed, err)
		}
		if err := WithinScope(ctx, pool, scope, request.Spec.Actor, func(ctx context.Context, tx *Transaction) error {
			var guards, pending, outbox int
			if err := tx.queryRow(ctx, `SELECT
				(SELECT count(*) FROM pm.admission_keys WHERE run_id = $1 AND safe_response IS NOT NULL AND resolved_at IS NULL),
				(SELECT count(*) FROM pm.pending_reservations WHERE run_id = $1 AND released_at IS NULL),
				(SELECT count(*) FROM pm.outbox_commands WHERE run_id = $1)
			`, request.Spec.RunID).Scan(&guards, &pending, &outbox); err != nil {
				return err
			}
			if guards != 1 || pending != 1 || outbox != 1 {
				t.Fatalf("unresolved recovery changed durable work: guards=%d pending=%d outbox=%d", guards, pending, outbox)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("resolution_is_atomic_and_cannot_shorten_retention", func(t *testing.T) {
		scope, request := newRequest(t, "resolution")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		injected := errors.New("rollback terminal projection")
		if err := terminal(ctx, scope, request, injected); !errors.Is(err, injected) {
			t.Fatalf("terminal rollback = %v", err)
		}
		if err := WithinScope(ctx, pool, scope, request.Spec.Actor, func(ctx context.Context, tx *Transaction) error {
			var resolved *time.Time
			if err := tx.queryRow(ctx, `SELECT resolved_at FROM pm.admission_keys WHERE run_id = $1`, request.Spec.RunID).Scan(&resolved); err != nil {
				return err
			}
			if resolved != nil {
				t.Fatal("rolled-back terminal state started response expiry")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := terminal(ctx, scope, request, nil); err != nil {
			t.Fatal(err)
		}
		if changed, err := compact(ctx, scope, request); err != nil || changed {
			t.Fatalf("early compaction = %v, %v", changed, err)
		}
		if err := WithinScope(ctx, pool, scope, request.Spec.Actor, func(ctx context.Context, tx *Transaction) error {
			var resolved *time.Time
			if err := tx.queryRow(ctx, `SELECT resolved_at FROM pm.admission_keys WHERE run_id = $1`, request.Spec.RunID).Scan(&resolved); err != nil {
				return err
			}
			if resolved == nil {
				t.Fatal("known terminal projection did not start response retention")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		err := WithinScope(ctx, pool, scope, request.Spec.Actor, func(ctx context.Context, tx *Transaction) error {
			_, err := tx.exec(ctx, `UPDATE pm.admission_keys SET resolved_at = resolved_at - interval '169 hours' WHERE run_id = $1`, request.Spec.RunID)
			return err
		})
		var refusal *pgconn.PgError
		if !errors.As(err, &refusal) || refusal.Code != "23514" {
			t.Fatalf("runtime resolution retiming = %v; want constraint refusal", err)
		}
		ageAdmissionReceiptFixture(t, ctx, credentials.migratorDSN, scope, request.Spec.RunID, 168*time.Hour-time.Minute, true)
		if changed, err := compact(ctx, scope, request); err != nil || changed {
			t.Fatalf("compaction before the full 168-hour boundary = %v, %v", changed, err)
		}
	})

	t.Run("compacted_guard_expires_exact_replay_but_still_conflicts", func(t *testing.T) {
		scope, request := newRequest(t, "compacted")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := terminal(ctx, scope, request, nil); err != nil {
			t.Fatal(err)
		}
		ageAdmissionReceiptFixture(t, ctx, credentials.migratorDSN, scope, request.Spec.RunID, 169*time.Hour, true)
		if changed, err := compact(ctx, scope, request); err != nil || !changed {
			t.Fatalf("eligible compaction = %v, %v", changed, err)
		}
		if changed, err := compact(ctx, scope, request); err != nil || changed {
			t.Fatalf("repeated compaction = %v, %v", changed, err)
		}
		_, err := tryPendingAdmission(ctx, pool, request)
		var expired *AdmissionReceiptExpiredError
		if !errors.As(err, &expired) || expired.ExistingRunID != request.Spec.RunID {
			t.Fatalf("exact old recovery = %v; want original run expiry", err)
		}
		changed := testAdmissionRequest(t, scope, request.Key.IdempotencyKey, "retention-different-intent")
		changed.Key = request.Key
		if _, err := tryPendingAdmission(ctx, pool, changed); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("changed old intent = %v; want retained conflict", err)
		}
		if err := WithinScope(ctx, pool, scope, request.Spec.Actor, func(ctx context.Context, tx *Transaction) error {
			guard, err := LookupAdmission(ctx, tx, request.Key)
			if err != nil {
				return err
			}
			if !guard.ReceiptExpired || guard.RunID != request.Spec.RunID || guard.IntentDigest != request.Spec.IdempotencyFingerprint {
				t.Fatalf("compacted refusal guard = %#v", guard)
			}
			var runs, commands int
			if err := tx.queryRow(ctx, `SELECT (SELECT count(*) FROM pm.runs), (SELECT count(*) FROM pm.outbox_commands WHERE kind = 'start_run')`).Scan(&runs, &commands); err != nil {
				return err
			}
			if runs != 1 || commands != 1 {
				t.Fatalf("compacted replay created work: runs=%d starts=%d", runs, commands)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		err = WithinScope(ctx, pool, scope, request.Spec.Actor, func(ctx context.Context, tx *Transaction) error {
			_, err := tx.exec(ctx, `UPDATE pm.admission_keys
				SET safe_response = jsonb_build_object('run_id', run_id, 'intent_digest', intent_digest, 'accepted_at', accepted_at)
				WHERE run_id = $1`, request.Spec.RunID)
			return err
		})
		var refusal *pgconn.PgError
		if !errors.As(err, &refusal) || refusal.Code != "23514" {
			t.Fatalf("runtime response regeneration = %v; want constraint refusal", err)
		}
	})

	t.Run("compaction_races_recovery_without_recycling_identity", func(t *testing.T) {
		scope, request := newRequest(t, "race")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := terminal(ctx, scope, request, nil); err != nil {
			t.Fatal(err)
		}
		ageAdmissionReceiptFixture(t, ctx, credentials.migratorDSN, scope, request.Spec.RunID, 169*time.Hour, true)
		start := make(chan struct{})
		var workers sync.WaitGroup
		workers.Add(2)
		var compactErr, replayErr error
		var replay AdmissionResult
		go func() {
			defer workers.Done()
			<-start
			_, compactErr = compact(ctx, scope, request)
		}()
		go func() {
			defer workers.Done()
			<-start
			replay, replayErr = tryPendingAdmission(ctx, pool, request)
		}()
		close(start)
		workers.Wait()
		if compactErr != nil {
			t.Fatal(compactErr)
		}
		var expired *AdmissionReceiptExpiredError
		if replayErr == nil {
			if !replay.Replayed || replay.RunID != request.Spec.RunID {
				t.Fatalf("racing recovery created a new acceptance: %#v", replay)
			}
		} else if !errors.As(replayErr, &expired) || expired.ExistingRunID != request.Spec.RunID {
			t.Fatalf("racing recovery = %v", replayErr)
		}
		if _, err := tryPendingAdmission(ctx, pool, request); !errors.As(err, &expired) {
			t.Fatalf("post-race recovery = %v; want retained expiry", err)
		}
		foreign := newTestScope(t)
		t.Cleanup(func() { cleanupTestScope(t, credentials, foreign) })
		if err := WithinScope(ctx, pool, foreign, testActor(foreign), func(ctx context.Context, tx *Transaction) error {
			if _, err := LookupAdmission(ctx, tx, request.Key); !errors.Is(err, ErrNotFound) {
				t.Fatalf("foreign receipt lookup = %v", err)
			}
			if _, err := CompactAdmissionReceipt(ctx, tx, request.Key); !errors.Is(err, ErrNotFound) {
				t.Fatalf("foreign compaction = %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

// Only the owned fixture migrator may move retained timestamps into history.
// Runtime paths never receive clock overrides or timestamp-mutation privileges.
func ageAdmissionReceiptFixture(t *testing.T, ctx context.Context, dsn string, scope contract.Scope, runID string, age time.Duration, resolved bool) {
	t.Helper()
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := connection.Close(cleanup); err != nil {
			t.Errorf("close receipt fixture migrator: %v", err)
		}
	}()
	if err := pgx.BeginFunc(ctx, connection, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('pm.org_id', $1, true), set_config('pm.workspace_id', $2, true)`, scope.OrgID, scope.WorkspaceID); err != nil {
			return err
		}
		var historical time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp() - ($1::bigint * interval '1 microsecond')`, age.Microseconds()).Scan(&historical); err != nil {
			return err
		}
		acceptedAt := historical
		if resolved {
			acceptedAt = historical.Add(-time.Hour)
		}
		tag, err := tx.Exec(ctx, `UPDATE pm.admission_keys
			SET accepted_at = $4,
			    safe_response = jsonb_set(safe_response, '{accepted_at}', to_jsonb($4::timestamptz)),
			    resolved_at = CASE WHEN $5::boolean THEN $6::timestamptz ELSE resolved_at END
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3`,
			scope.OrgID, scope.WorkspaceID, runID, acceptedAt, resolved, historical)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("owned receipt fixture is missing")
		}
		if _, err := tx.Exec(ctx, `UPDATE pm.run_specs SET accepted_at = $4 WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3`, scope.OrgID, scope.WorkspaceID, runID, acceptedAt); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE pm.pending_reservations SET accepted_at = $4 WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3`, scope.OrgID, scope.WorkspaceID, runID, acceptedAt)
		return err
	}); err != nil {
		t.Fatalf("age owned receipt fixture: %v", err)
	}
}
