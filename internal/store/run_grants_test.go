package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestAcceptedExecutionGrant(t *testing.T) {
	credentials, pool := prepareTestStore(t, 4)
	_, observer := prepareTestStore(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	t.Run("grant_is_atomic_and_authorized_replay_cannot_replace_it", func(t *testing.T) {
		scope := newTestScope(t)
		t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })
		request := testAdmissionRequest(t, scope, "grant-atomic", "grant-atomic")
		request.Grant.ExpiresAt = time.Date(2099, 1, 1, 0, 0, 0, 123456789, time.UTC)
		if err := WithinScope(ctx, pool, scope, request.Spec.Actor, func(ctx context.Context, tx *Transaction) error {
			if _, err := AdmitRun(ctx, tx, request); err != nil {
				return err
			}
			return WithinScope(ctx, observer, scope, request.Spec.Actor, func(ctx context.Context, other *Transaction) error {
				if _, err := LoadRunExecutionGrant(ctx, other, request.Spec.RunID); !errors.Is(err, ErrNotFound) {
					return errors.New("uncommitted execution grant became visible independently")
				}
				return nil
			})
		}); err != nil {
			t.Fatal(err)
		}
		stored := loadRunGrantForTest(t, observer, scope, request.Spec.RunID)
		if !stored.Grant.Equal(request.Grant) || stored.RevokedAt != nil {
			t.Fatal("accepted authority lost its exact immutable grant or expiry precision")
		}

		replay := testAdmissionRequest(t, scope, "grant-atomic", "grant-atomic")
		replay.Grant.PolicyEpochs.Membership++
		result, err := tryPendingAdmission(ctx, pool, replay)
		if err != nil || !result.Replayed || result.RunID != request.Spec.RunID {
			t.Fatalf("replay did not recover the original acceptance: %v", err)
		}
		stored = loadRunGrantForTest(t, observer, scope, request.Spec.RunID)
		if !stored.Grant.Equal(request.Grant) {
			t.Fatal("replay replaced accepted authority with a fresh grant or epoch")
		}
		if err := WithinScope(ctx, observer, scope, request.Spec.Actor, func(ctx context.Context, tx *Transaction) error {
			_, err := LoadRunExecutionGrant(ctx, tx, replay.Spec.RunID)
			if !errors.Is(err, ErrNotFound) {
				return errors.New("replay persisted a second grant under a replacement RunID")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("foreign_scope_and_expired_new_grants_leave_no_acceptance", func(t *testing.T) {
		scope := newTestScope(t)
		t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })
		foreign := testAdmissionRequest(t, scope, "grant-foreign", "grant-foreign")
		foreign.Grant.Scope.WorkspaceID = "another-owned-synthetic-workspace"
		if _, err := tryPendingAdmission(ctx, pool, foreign); !errors.Is(err, ErrScopeMismatch) {
			t.Fatalf("foreign-workspace authority was not refused: %v", err)
		}
		expired := testAdmissionRequest(t, scope, "grant-expired", "grant-expired")
		expired.Grant.ExpiresAt = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		if _, err := tryPendingAdmission(ctx, pool, expired); !errors.Is(err, ErrRunGrantUnavailable) {
			t.Fatalf("expired new authority was not refused: %v", err)
		}
		if err := WithinScope(ctx, observer, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			for _, request := range []AdmissionRequest{foreign, expired} {
				if _, err := LookupAdmission(ctx, tx, request.Key); !errors.Is(err, ErrNotFound) {
					return errors.New("refused execution grant retained an admission identity")
				}
				if _, err := LoadRunExecutionGrant(ctx, tx, request.Spec.RunID); !errors.Is(err, ErrNotFound) {
					return errors.New("refused execution grant became durable")
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("revocation_is_scoped_fenced_atomic_and_final", func(t *testing.T) {
		scope, foreign := newTestScope(t), newTestScope(t)
		t.Cleanup(func() { cleanupTestScope(t, credentials, scope, foreign) })
		request := testAdmissionRequest(t, scope, "grant-revoke", "grant-revoke")
		if _, err := tryPendingAdmission(ctx, pool, request); err != nil {
			t.Fatal(err)
		}
		if err := WithinScope(ctx, pool, foreign, testActor(foreign), func(ctx context.Context, tx *Transaction) error {
			_, _, err := RevokeRunExecutionGrant(ctx, tx, request.Spec.RunID, request.Grant.GrantEpoch)
			return err
		}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign workspace could observe or revoke this run grant: %v", err)
		}
		if err := WithinScope(ctx, pool, scope, request.Spec.Actor, func(ctx context.Context, tx *Transaction) error {
			_, _, err := RevokeRunExecutionGrant(ctx, tx, request.Spec.RunID, request.Grant.GrantEpoch+1)
			return err
		}); !errors.Is(err, ErrCASConflict) {
			t.Fatalf("stale grant epoch was silently rebased: %v", err)
		}
		stop := errors.New("synthetic failure after revocation")
		if err := WithinScope(ctx, pool, scope, request.Spec.Actor, func(ctx context.Context, tx *Transaction) error {
			if _, _, err := RevokeRunExecutionGrant(ctx, tx, request.Spec.RunID, request.Grant.GrantEpoch); err != nil {
				return err
			}
			return stop
		}); !errors.Is(err, stop) {
			t.Fatal(err)
		}
		if stored := loadRunGrantForTest(t, observer, scope, request.Spec.RunID); stored.RevokedAt != nil {
			t.Fatal("failed revocation transaction left authority revoked")
		}
		var revoked RunExecutionGrant
		if err := WithinScope(ctx, pool, scope, request.Spec.Actor, func(ctx context.Context, tx *Transaction) error {
			var changed bool
			var err error
			revoked, changed, err = RevokeRunExecutionGrant(ctx, tx, request.Spec.RunID, request.Grant.GrantEpoch)
			if err == nil && !changed {
				return errors.New("first successful revocation did not change durable authority")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if revoked.RevokedAt == nil {
			t.Fatal("successful revocation has no durable time")
		}
		if err := WithinScope(ctx, pool, scope, request.Spec.Actor, func(ctx context.Context, tx *Transaction) error {
			repeated, changed, err := RevokeRunExecutionGrant(ctx, tx, request.Spec.RunID, request.Grant.GrantEpoch)
			if err != nil {
				return err
			}
			if changed || repeated.RevokedAt == nil || !repeated.RevokedAt.Equal(*revoked.RevokedAt) {
				return errors.New("repeated revocation rewrote its original durable outcome")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		err := WithinScope(ctx, pool, scope, request.Spec.Actor, func(ctx context.Context, tx *Transaction) error {
			_, err := tx.exec(ctx, `UPDATE pm.run_execution_grants SET revoked_at = NULL WHERE run_id = $1`, request.Spec.RunID)
			return err
		})
		var refused *pgconn.PgError
		if !errors.As(err, &refused) || refused.Code != "23514" {
			t.Fatal("runtime mutation could silently clear a completed grant revocation")
		}
		stored := loadRunGrantForTest(t, observer, scope, request.Spec.RunID)
		if stored.RevokedAt == nil || !stored.RevokedAt.Equal(*revoked.RevokedAt) || !stored.Grant.Equal(request.Grant) {
			t.Fatal("grant revival attempt changed the immutable authority or revocation")
		}
	})
}

func loadRunGrantForTest(t *testing.T, pool *Pool, scope contract.Scope, runID string) RunExecutionGrant {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var result RunExecutionGrant
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		result, err = LoadRunExecutionGrant(ctx, tx, runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return result
}
