package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestPendingAdmissionCapacity(t *testing.T) {
	credentials, pool := prepareTestStore(t, 4)
	scope := newTestScope(t)
	t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	limits := AdmissionLimits{GlobalPending: 3, WorkspacePending: 2, WorkspaceMaxPendingAge: 30 * time.Second}

	for _, seed := range []string{"pending-first", "pending-final-slot"} {
		request := testAdmissionRequest(t, scope, seed, seed)
		setAdmissionLimitsForTest(t, &request, limits)
		if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			_, err := AdmitRun(ctx, tx, request)
			return err
		}); err != nil {
			t.Fatalf("admit an available pending slot: %v", err)
		}
	}

	refused := testAdmissionRequest(t, scope, "pending-over-capacity", "pending-over-capacity")
	setAdmissionLimitsForTest(t, &refused, limits)
	err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := AdmitRun(ctx, tx, refused)
		return err
	})
	if !errors.Is(err, ErrWorkspaceAdmissionCountLimit) {
		t.Fatalf("third new admission in a two-slot workspace must be refused: %v", err)
	}
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := LookupAdmission(ctx, tx, refused.Key)
		if !errors.Is(err, ErrNotFound) {
			return errors.New("refused admission retained an accepted identity")
		}
		var runs, pending, outbox int64
		if err := tx.queryRow(ctx, `SELECT count(*) FROM pm.run_specs`).Scan(&runs); err != nil {
			return err
		}
		if err := tx.queryRow(ctx, `SELECT count(*) FROM pm.pending_reservations WHERE released_at IS NULL`).Scan(&pending); err != nil {
			return err
		}
		if err := tx.queryRow(ctx, `SELECT count(*) FROM pm.outbox_commands`).Scan(&outbox); err != nil {
			return err
		}
		if runs != 2 || pending != 2 || outbox != 2 {
			return errors.New("capacity refusal changed the accepted run, reservation or outbox set")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPendingAdmissionReplayAndRollback(t *testing.T) {
	credentials, pool := prepareTestStore(t, 4)
	left, right := newTestScope(t), newTestScope(t)
	t.Cleanup(func() { cleanupTestScope(t, credentials, left, right) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first := pendingRequestForTest(t, left, "replay-first")
	accepted, err := tryPendingAdmission(ctx, pool, first)
	if err != nil {
		t.Fatal(err)
	}
	rolledBack := pendingRequestForTest(t, right, "rolled-back-slot")
	stop := errors.New("synthetic refusal after reservation")
	err = WithinScope(ctx, pool, right, testActor(right), func(ctx context.Context, tx *Transaction) error {
		if _, err := AdmitRun(ctx, tx, rolledBack); err != nil {
			return err
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("roll back the owned admission transaction: %v", err)
	}
	for _, request := range []AdmissionRequest{
		pendingRequestForTest(t, left, "replay-workspace-full"),
		pendingRequestForTest(t, right, "replay-global-full"),
	} {
		if _, err := tryPendingAdmission(ctx, pool, request); err != nil {
			t.Fatalf("rolled-back reservation consumed a later valid slot: %v", err)
		}
	}
	if _, err := tryPendingAdmission(ctx, pool, pendingRequestForTest(t, right, "global-refusal")); !errors.Is(err, ErrDeploymentAdmissionOverload) {
		t.Fatalf("new admission beyond the deployment cap was not refused: %v", err)
	}
	replayed, err := tryPendingAdmission(ctx, pool, first)
	if err != nil {
		t.Fatalf("exact replay incorrectly rechecked full capacity: %v", err)
	}
	if !replayed.Replayed || replayed.RunID != accepted.RunID || !replayed.AcceptedAt.Equal(accepted.AcceptedAt) {
		t.Fatal("exact replay replaced the original run or reset its pending age")
	}
	changed := pendingRequestForTest(t, left, "changed-at-capacity")
	changed.Key = first.Key
	_, err = tryPendingAdmission(ctx, pool, changed)
	var conflict *IdempotencyConflictError
	if !errors.As(err, &conflict) || conflict.ExistingRunID != accepted.RunID {
		t.Fatalf("changed intent at capacity must retain its original conflict: %v", err)
	}
}

func TestPendingAdmissionLastSlotAcrossPools(t *testing.T) {
	credentials, pool := prepareTestStore(t, 4)
	_, replica := prepareTestStore(t, 2)
	left, right := newTestScope(t), newTestScope(t)
	t.Cleanup(func() { cleanupTestScope(t, credentials, left, right) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, request := range []AdmissionRequest{
		pendingRequestForTest(t, left, "race-left-existing"),
		pendingRequestForTest(t, right, "race-right-existing"),
	} {
		if _, err := tryPendingAdmission(ctx, pool, request); err != nil {
			t.Fatal(err)
		}
	}

	// Hold the durable authority until both independent backends are waiting.
	// Without the pre-decision lock, both can check the old count and wait only
	// while their reservation inserts update the aggregate, then over-admit.
	guardReady, guardRelease, guardDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseGuard := sync.OnceFunc(func() { close(guardRelease) })
	var guardErr error
	go func() {
		guardErr = WithinScope(ctx, pool, left, testActor(left), func(ctx context.Context, tx *Transaction) error {
			var pending int64
			if err := tx.queryRow(ctx, `SELECT pending_count FROM pm.pending_admission_state WHERE singleton FOR UPDATE`).Scan(&pending); err != nil {
				return err
			}
			close(guardReady)
			select {
			case <-guardRelease:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		close(guardDone)
	}()
	defer func() {
		releaseGuard()
		<-guardDone
	}()
	select {
	case <-guardReady:
	case <-guardDone:
		t.Fatalf("hold the pending authority: %v", guardErr)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	type attempt struct {
		index int
		err   error
	}
	requests := []AdmissionRequest{
		pendingRequestForTest(t, left, "race-left-final"),
		pendingRequestForTest(t, right, "race-right-final"),
	}
	pools := []*Pool{pool, replica}
	pids, results := make(chan int32, 2), make(chan attempt, 2)
	start := make(chan struct{})
	startBoth := sync.OnceFunc(func() { close(start) })
	defer startBoth()
	for index, request := range requests {
		go func(index int, request AdmissionRequest) {
			scope := contract.Scope{OrgID: request.Spec.OrgID, WorkspaceID: request.Spec.WorkspaceID}
			err := WithinScope(ctx, pools[index], scope, request.Spec.Actor, func(ctx context.Context, tx *Transaction) error {
				var pid int32
				if err := tx.queryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
					return err
				}
				pids <- pid
				select {
				case <-start:
				case <-ctx.Done():
					return ctx.Err()
				}
				_, err := AdmitRun(ctx, tx, request)
				return err
			})
			results <- attempt{index: index, err: err}
		}(index, request)
	}
	backends := make([]int32, 0, 2)
	for len(backends) < 2 {
		select {
		case pid := <-pids:
			backends = append(backends, pid)
		case result := <-results:
			t.Fatalf("contender failed before the shared decision: %v", result.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	startBoth()
	waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
	defer waitCancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var waiting int64
		err := WithinScope(waitCtx, pool, left, testActor(left), func(ctx context.Context, tx *Transaction) error {
			return tx.queryRow(ctx, `
				SELECT count(*) FILTER (WHERE cardinality(pg_blocking_pids(pid)) > 0)
				FROM unnest($1::integer[]) AS contenders(pid)
			`, backends).Scan(&waiting)
		})
		if err != nil {
			t.Fatalf("observe independent contender waits: %v", err)
		}
		if waiting == 2 {
			break
		}
		select {
		case <-tick.C:
		case <-waitCtx.Done():
			t.Fatal("both contenders did not reach the held durable decision")
		}
	}
	releaseGuard()
	<-guardDone
	if guardErr != nil {
		t.Fatal(guardErr)
	}
	winners, refusedIndex := 0, -1
	for range 2 {
		select {
		case result := <-results:
			if result.err == nil {
				winners++
			} else if errors.Is(result.err, ErrDeploymentAdmissionOverload) {
				refusedIndex = result.index
			} else {
				t.Fatalf("unexpected last-slot result: %v", result.err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if winners != 1 || refusedIndex < 0 {
		t.Fatalf("one remaining deployment slot accepted %d contenders", winners)
	}
	refused := requests[refusedIndex]
	scope := contract.Scope{OrgID: refused.Spec.OrgID, WorkspaceID: refused.Spec.WorkspaceID}
	if err := WithinScope(ctx, pool, scope, refused.Spec.Actor, func(ctx context.Context, tx *Transaction) error {
		_, err := LookupAdmission(ctx, tx, refused.Key)
		if !errors.Is(err, ErrNotFound) {
			return errors.New("last-slot refusal retained a second accepted identity")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPendingAdmissionAgePrecedence(t *testing.T) {
	credentials, pool := prepareTestStore(t, 4)
	agedScope, fullScope := newTestScope(t), newTestScope(t)
	t.Cleanup(func() { cleanupTestScope(t, credentials, agedScope, fullScope) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	aged := pendingRequestForTest(t, agedScope, "age-existing")
	full := pendingRequestForTest(t, fullScope, "age-full-existing")
	for _, request := range []AdmissionRequest{aged, full, pendingRequestForTest(t, fullScope, "age-full-final")} {
		if _, err := tryPendingAdmission(ctx, pool, request); err != nil {
			t.Fatal(err)
		}
	}
	acceptedAt := agePendingFixture(t, ctx, credentials.migratorDSN, aged)
	agePendingFixture(t, ctx, credentials.migratorDSN, full)
	if _, err := tryPendingAdmission(ctx, pool, pendingRequestForTest(t, agedScope, "age-refusal")); !errors.Is(err, ErrWorkspaceAdmissionAgeLimit) {
		t.Fatalf("workspace age refusal must precede deployment overload: %v", err)
	}
	if _, err := tryPendingAdmission(ctx, pool, pendingRequestForTest(t, fullScope, "count-and-age-refusal")); !errors.Is(err, ErrWorkspaceAdmissionCountLimit) {
		t.Fatalf("workspace count refusal must precede age and deployment overload: %v", err)
	}
	replayed, err := tryPendingAdmission(ctx, pool, aged)
	if err != nil || !replayed.Replayed || !replayed.AcceptedAt.Equal(acceptedAt) {
		t.Fatalf("age refusal removed accepted work or replay reset its age: %v", err)
	}
}

func TestPendingAdmissionAgeBoundary(t *testing.T) {
	limits := AdmissionLimits{GlobalPending: 3, WorkspacePending: 2, WorkspaceMaxPendingAge: 30 * time.Second}
	now := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	oldest := now.Add(-limits.WorkspaceMaxPendingAge)
	if err := pendingAdmissionRefusal(limits, 2, 1, &oldest, now); !errors.Is(err, ErrWorkspaceAdmissionAgeLimit) {
		t.Fatal("the exact maximum pending age must be refused")
	}
	if err := pendingAdmissionRefusal(limits, 2, 1, &oldest, now.Add(-time.Nanosecond)); err != nil {
		t.Fatal("a pending age below the bound must leave the final slot available")
	}
	if err := pendingAdmissionRefusal(limits, 2, 0, nil, now); err != nil {
		t.Fatal("an empty workspace has no oldest-age refusal")
	}
}

func pendingRequestForTest(t *testing.T, scope contract.Scope, seed string) AdmissionRequest {
	t.Helper()
	request := testAdmissionRequest(t, scope, seed, seed)
	setAdmissionLimitsForTest(t, &request, AdmissionLimits{GlobalPending: 3, WorkspacePending: 2, WorkspaceMaxPendingAge: 30 * time.Second})
	return request
}

func tryPendingAdmission(ctx context.Context, pool *Pool, request AdmissionRequest) (AdmissionResult, error) {
	scope := contract.Scope{OrgID: request.Spec.OrgID, WorkspaceID: request.Spec.WorkspaceID}
	var result AdmissionResult
	err := WithinScope(ctx, pool, scope, request.Spec.Actor, func(ctx context.Context, tx *Transaction) error {
		var err error
		result, err = AdmitRun(ctx, tx, request)
		return err
	})
	return result, err
}

func agePendingFixture(t *testing.T, ctx context.Context, migratorDSN string, request AdmissionRequest) time.Time {
	t.Helper()
	conn, err := pgx.Connect(ctx, migratorDSN)
	if err != nil {
		t.Fatalf("open owned historical fixture connection: %T", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := detachedCleanupContext()
		defer cleanupCancel()
		if err := conn.Close(cleanupCtx); err != nil {
			t.Errorf("close owned historical fixture connection: %T", err)
		}
	}()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := detachedCleanupContext()
		defer cleanupCancel()
		if err := tx.Rollback(cleanupCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("close owned historical fixture transaction: %T", err)
		}
	}()
	if _, err := tx.Exec(ctx, `
		SELECT set_config('pm.org_id', $1, true), set_config('pm.workspace_id', $2, true)
	`, request.Spec.OrgID, request.Spec.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	var acceptedAt time.Time
	if err := tx.QueryRow(ctx, `
		WITH aged AS (
			UPDATE pm.pending_reservations
			SET accepted_at = clock_timestamp() - interval '30 seconds'
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND released_at IS NULL
			RETURNING accepted_at
		), specification AS (
			UPDATE pm.run_specs AS spec SET accepted_at = aged.accepted_at
			FROM aged
			WHERE spec.org_id = $1 AND spec.workspace_id = $2 AND spec.run_id = $3
			RETURNING spec.accepted_at
		)
		UPDATE pm.admission_keys AS admission
		SET accepted_at = specification.accepted_at,
		    safe_response = jsonb_set(safe_response, '{accepted_at}', to_jsonb(specification.accepted_at))
		FROM specification
		WHERE admission.org_id = $1 AND admission.workspace_id = $2 AND admission.run_id = $3
		RETURNING admission.accepted_at
	`, request.Spec.OrgID, request.Spec.WorkspaceID, request.Spec.RunID).Scan(&acceptedAt); err != nil {
		t.Fatalf("construct the single owned historical pending fixture: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return acceptedAt.UTC()
}
