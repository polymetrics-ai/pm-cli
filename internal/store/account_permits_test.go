package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestSharedAccountPermits(t *testing.T) {
	credentials, replicaA := prepareTestStore(t, 6)
	replicaB := openPermitTestPool(t, credentials, 6)

	t.Run("aliases and workspaces share canonical account capacity", func(t *testing.T) {
		fixture := registerPermitTestFixture(t, credentials, replicaA, permitFixtureOptions{Capacity: 2})
		type reservationResult struct {
			permit AccountPermit
			err    error
		}
		const contenders = 8
		start := make(chan struct{})
		results := make(chan reservationResult, contenders)
		var workers sync.WaitGroup
		for index := range contenders {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				scope := fixture.alpha
				connectionID := fixture.alphaConnections[index%len(fixture.alphaConnections)]
				pool := replicaA
				if index%2 == 1 {
					scope = fixture.beta
					connectionID = fixture.betaConnection
					pool = replicaB
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				permit, err := reservePermitForTest(ctx, pool, scope, AccountPermitRequest{
					PermitID:     fmt.Sprintf("permit-contention-%02d-%s", index, fixture.suffix),
					ConnectionID: connectionID,
					OwnerID:      fmt.Sprintf("worker-%02d", index),
					Units:        1,
					TTL:          3 * time.Second,
				})
				results <- reservationResult{permit: permit, err: err}
			}()
		}
		close(start)
		workers.Wait()
		close(results)

		successful := make([]AccountPermit, 0, fixture.account.Capacity)
		for result := range results {
			if result.err == nil {
				successful = append(successful, result.permit)
				continue
			}
			if !errors.Is(result.err, ErrAccountCapacity) {
				t.Fatalf("concurrent reservation error = %v; want ErrAccountCapacity", result.err)
			}
		}
		if got, want := len(successful), int(fixture.account.Capacity); got != want {
			t.Fatalf("successful reservations = %d; want global capacity %d", got, want)
		}
		for _, permit := range successful {
			if permit.ProviderAccountID != fixture.account.AccountID || permit.Units != 1 {
				t.Fatalf("reservation escaped canonical account: %+v", permit)
			}
		}
		if got := readReservedUnits(t, replicaA, fixture.alpha, fixture.account.AccountID); got != fixture.account.Capacity {
			t.Fatalf("alpha sees reserved units %d; want %d", got, fixture.account.Capacity)
		}
		if got := readReservedUnits(t, replicaB, fixture.beta, fixture.account.AccountID); got != fixture.account.Capacity {
			t.Fatalf("beta sees reserved units %d; want %d", got, fixture.account.Capacity)
		}

		unknown, err := authorizePermitForTest(context.Background(), poolForPermit(successful[0], fixture, replicaA, replicaB), successful[0])
		if err != nil {
			t.Fatalf("authorize first account permit: %v", err)
		}
		deadline := unknown.ExpiresAt
		if successful[1].ExpiresAt.After(deadline) {
			deadline = successful[1].ExpiresAt
		}
		waitForStoreDeadline(t, replicaA, fixture.alpha, deadline)
		if got := readReservedUnits(t, replicaB, fixture.beta, fixture.account.AccountID); got != fixture.account.Capacity {
			t.Fatalf("expired dispatched and reserved work retained %d units; want %d", got, fixture.account.Capacity)
		}
		unknown, err = markUnknownPermitForTest(context.Background(), poolForPermit(unknown, fixture, replicaA, replicaB), unknown)
		if err != nil {
			t.Fatalf("retain expired dispatched work as unknown: %v", err)
		}
		if _, err := expirePermitForTest(context.Background(), poolForPermit(unknown, fixture, replicaA, replicaB), unknown); !errors.Is(err, ErrAccountPermitStale) {
			t.Fatalf("expire unknown permit error = %v; want ErrAccountPermitStale", err)
		}
		if _, err := authorizePermitForTest(context.Background(), poolForPermit(successful[1], fixture, replicaA, replicaB), successful[1]); !errors.Is(err, ErrAccountPermitStale) {
			t.Fatalf("expired permit authorization error = %v; want ErrAccountPermitStale", err)
		}
		reclaimed, err := reclaimPermitForTest(context.Background(), poolForPermit(successful[1], fixture, replicaA, replicaB), successful[1], "replacement-worker", time.Second)
		if err != nil {
			t.Fatalf("reclaim expired unused permit: %v", err)
		}
		if reclaimed.Fence != successful[1].Fence+1 || reclaimed.OwnerID != "replacement-worker" {
			t.Fatalf("reclaimed authority = owner %q fence %d; want replacement fence %d", reclaimed.OwnerID, reclaimed.Fence, successful[1].Fence+1)
		}
		if _, err := authorizePermitForTest(context.Background(), poolForPermit(successful[1], fixture, replicaA, replicaB), successful[1]); !errors.Is(err, ErrAccountPermitStale) {
			t.Fatalf("stale permit authorization error = %v; want ErrAccountPermitStale", err)
		}
		dispatched, err := authorizePermitForTest(context.Background(), poolForPermit(reclaimed, fixture, replicaA, replicaB), reclaimed)
		if err != nil {
			t.Fatalf("authorize reclaimed permit: %v", err)
		}
		resolved, err := resolvePermitForTest(context.Background(), poolForPermit(dispatched, fixture, replicaA, replicaB), dispatched)
		if err != nil {
			t.Fatalf("resolve dispatched permit: %v", err)
		}
		replayedResolution, err := resolvePermitForTest(context.Background(), poolForPermit(dispatched, fixture, replicaA, replicaB), dispatched)
		if err != nil {
			t.Fatalf("replay dispatched permit resolution: %v", err)
		}
		if replayedResolution.State != AccountPermitResolved || resolved.ResolvedAt == nil || replayedResolution.ResolvedAt == nil || !resolved.ResolvedAt.Equal(*replayedResolution.ResolvedAt) {
			t.Fatalf("resolution replay did not return the same terminal permit")
		}
		if got := readReservedUnits(t, replicaA, fixture.alpha, fixture.account.AccountID); got != 1 {
			t.Fatalf("reserved units after exactly-once resolution = %d; want 1", got)
		}

		_, err = reservePermitForTest(context.Background(), replicaB, fixture.beta, AccountPermitRequest{
			PermitID:     "permit-unknown-capacity-" + fixture.suffix,
			ConnectionID: fixture.betaConnection,
			OwnerID:      "capacity-probe",
			Units:        2,
			TTL:          time.Minute,
		})
		if !errors.Is(err, ErrAccountCapacity) {
			t.Fatalf("reservation over retained unknown charge error = %v; want ErrAccountCapacity", err)
		}
		resolvedUnknown, err := resolvePermitForTest(context.Background(), poolForPermit(unknown, fixture, replicaA, replicaB), unknown)
		if err != nil {
			t.Fatalf("resolve unknown permit with explicit settlement: %v", err)
		}
		if _, err := resolvePermitForTest(context.Background(), poolForPermit(unknown, fixture, replicaA, replicaB), unknown); err != nil {
			t.Fatalf("replay unknown permit resolution: %v", err)
		}
		if resolvedUnknown.State != AccountPermitResolved || readReservedUnits(t, replicaA, fixture.alpha, fixture.account.AccountID) != 0 {
			t.Fatalf("unknown resolution did not release exactly one retained unit")
		}

		unused, err := reservePermitForTest(context.Background(), replicaA, fixture.alpha, AccountPermitRequest{
			PermitID:     "permit-unused-" + fixture.suffix,
			ConnectionID: fixture.alphaConnections[0],
			OwnerID:      "unused-worker",
			Units:        1,
			TTL:          25 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("reserve unused permit: %v", err)
		}
		waitForStoreDeadline(t, replicaA, fixture.alpha, unused.ExpiresAt)
		expired, err := expirePermitForTest(context.Background(), replicaA, unused)
		if err != nil {
			t.Fatalf("expire unused permit: %v", err)
		}
		replayedExpiry, err := expirePermitForTest(context.Background(), replicaA, unused)
		if err != nil {
			t.Fatalf("replay unused permit expiry: %v", err)
		}
		if expired.State != AccountPermitExpired || replayedExpiry.State != AccountPermitExpired || readReservedUnits(t, replicaA, fixture.alpha, fixture.account.AccountID) != 0 {
			t.Fatalf("unused expiry did not release exactly once")
		}
	})

	t.Run("duplicate intent consumes once and cannot change alias", func(t *testing.T) {
		fixture := registerPermitTestFixture(t, credentials, replicaA, permitFixtureOptions{Capacity: 1})
		request := AccountPermitRequest{
			PermitID:     "permit-duplicate-" + fixture.suffix,
			ConnectionID: fixture.alphaConnections[0],
			OwnerID:      "duplicate-worker",
			Units:        1,
			TTL:          100 * time.Millisecond,
		}
		type reservationResult struct {
			permit AccountPermit
			err    error
		}
		start := make(chan struct{})
		results := make(chan reservationResult, 2)
		var workers sync.WaitGroup
		for _, pool := range []*Pool{replicaA, replicaB} {
			pool := pool
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				permit, err := reservePermitForTest(context.Background(), pool, fixture.alpha, request)
				results <- reservationResult{permit: permit, err: err}
			}()
		}
		close(start)
		workers.Wait()
		close(results)
		var permits []AccountPermit
		for result := range results {
			if result.err != nil {
				t.Fatalf("duplicate reservation: %v", result.err)
			}
			permits = append(permits, result.permit)
		}
		if len(permits) != 2 || !samePermitCAS(permits[0], permits[1]) || permits[0].State != AccountPermitReserved {
			t.Fatalf("duplicate reservations did not return one durable permit")
		}
		if got := readReservedUnits(t, replicaA, fixture.alpha, fixture.account.AccountID); got != 1 {
			t.Fatalf("duplicate intent reserved %d units; want 1", got)
		}
		changed := request
		changed.ConnectionID = fixture.alphaConnections[1]
		if _, err := reservePermitForTest(context.Background(), replicaA, fixture.alpha, changed); !errors.Is(err, ErrImmutableConflict) {
			t.Fatalf("changed alias under duplicate permit error = %v; want ErrImmutableConflict", err)
		}
		changed = request
		changed.TTL = 200 * time.Millisecond
		if _, err := reservePermitForTest(context.Background(), replicaB, fixture.alpha, changed); !errors.Is(err, ErrImmutableConflict) {
			t.Fatalf("changed TTL under duplicate permit error = %v; want ErrImmutableConflict", err)
		}
		if got := countAccountPermits(t, replicaA, fixture.alpha, request.PermitID); got != 1 {
			t.Fatalf("durable duplicate permit rows = %d; want 1", got)
		}
		waitForStoreDeadline(t, replicaA, fixture.alpha, permits[0].ExpiresAt)
		if _, err := expirePermitForTest(context.Background(), replicaA, permits[0]); err != nil {
			t.Fatalf("expire duplicate permit: %v", err)
		}
		if _, err := expirePermitForTest(context.Background(), replicaB, permits[1]); err != nil {
			t.Fatalf("replay duplicate permit expiry: %v", err)
		}
		if got := readReservedUnits(t, replicaA, fixture.alpha, fixture.account.AccountID); got != 0 {
			t.Fatalf("duplicate expiry left reserved units %d; want 0", got)
		}
	})

	t.Run("equal identities in unrelated organizations remain isolated", func(t *testing.T) {
		accountID := "provider-account-shared-" + randomHex(t, 8)
		witness := "account-witness-shared-" + randomHex(t, 8)
		first := registerPermitTestFixture(t, credentials, replicaA, permitFixtureOptions{AccountID: accountID, AccountWitness: witness, Capacity: 1})
		second := registerPermitTestFixture(t, credentials, replicaB, permitFixtureOptions{AccountID: accountID, AccountWitness: witness, Capacity: 1})
		permitID := "permit-equal-" + randomHex(t, 8)
		firstPermit, err := reservePermitForTest(context.Background(), replicaA, first.alpha, AccountPermitRequest{
			PermitID: permitID, ConnectionID: first.alphaConnections[0], OwnerID: "first-org-worker", Units: 1, TTL: time.Second,
		})
		if err != nil {
			t.Fatalf("reserve first organization: %v", err)
		}
		secondPermit, err := reservePermitForTest(context.Background(), replicaB, second.alpha, AccountPermitRequest{
			PermitID: permitID, ConnectionID: second.alphaConnections[0], OwnerID: "second-org-worker", Units: 1, TTL: time.Second,
		})
		if err != nil {
			t.Fatalf("reserve second organization: %v", err)
		}
		if firstPermit.Scope.OrgID == secondPermit.Scope.OrgID || readReservedUnits(t, replicaA, first.alpha, accountID) != 1 || readReservedUnits(t, replicaB, second.alpha, accountID) != 1 {
			t.Fatalf("equal account identities did not remain organization scoped")
		}
		if _, err := loadPermitForTest(context.Background(), replicaA, first.alpha, secondPermit.PermitID); err != nil {
			t.Fatalf("equal scoped permit identity must resolve within first organization: %v", err)
		}
		firstDispatched, err := authorizePermitForTest(context.Background(), replicaA, firstPermit)
		if err != nil {
			t.Fatalf("authorize first organization: %v", err)
		}
		secondDispatched, err := authorizePermitForTest(context.Background(), replicaB, secondPermit)
		if err != nil {
			t.Fatalf("authorize second organization: %v", err)
		}
		if _, err := resolvePermitForTest(context.Background(), replicaA, firstDispatched); err != nil {
			t.Fatalf("resolve first organization: %v", err)
		}
		if readReservedUnits(t, replicaB, second.alpha, accountID) != 1 {
			t.Fatalf("first organization settlement changed second organization capacity")
		}
		if _, err := resolvePermitForTest(context.Background(), replicaB, secondDispatched); err != nil {
			t.Fatalf("resolve second organization: %v", err)
		}
		foreignOnly, err := reservePermitForTest(context.Background(), replicaB, second.alpha, AccountPermitRequest{
			PermitID: "permit-second-only-" + randomHex(t, 8), ConnectionID: second.alphaConnections[0],
			OwnerID: "second-only-worker", Units: 1, TTL: time.Second,
		})
		if err != nil {
			t.Fatalf("reserve second-only permit: %v", err)
		}
		if _, err := loadPermitForTest(context.Background(), replicaA, first.alpha, foreignOnly.PermitID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign organization permit load error = %v; want ErrNotFound", err)
		}
		foreignDispatched, err := authorizePermitForTest(context.Background(), replicaB, foreignOnly)
		if err != nil {
			t.Fatalf("authorize second-only permit: %v", err)
		}
		if _, err := resolvePermitForTest(context.Background(), replicaB, foreignDispatched); err != nil {
			t.Fatalf("resolve second-only permit: %v", err)
		}
	})

	t.Run("owned connection loss fails closed", func(t *testing.T) {
		fixture := registerPermitTestFixture(t, credentials, replicaA, permitFixtureOptions{Capacity: 1})
		faultPool := openPermitTestPool(t, credentials, 1)
		request := AccountPermitRequest{
			PermitID:     "permit-pg-loss-" + fixture.suffix,
			ConnectionID: fixture.alphaConnections[0],
			OwnerID:      "faulted-worker",
			Units:        1,
			TTL:          time.Minute,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := WithinScope(ctx, faultPool, fixture.alpha, testActor(fixture.alpha), func(ctx context.Context, tx *Transaction) error {
			// This dedicated one-connection pool is owned by the subtest. Closing
			// only its checked-out physical connection faults PostgreSQL access
			// without changing the host, network, fixture, or sibling pools.
			if err := tx.tx.Conn().PgConn().Close(ctx); err != nil {
				return fmt.Errorf("close owned fault connection: %w", err)
			}
			_, reserveErr := ReserveAccountPermit(ctx, tx, request)
			if reserveErr == nil {
				return errors.New("reservation succeeded after its PostgreSQL connection was closed")
			}
			return reserveErr
		})
		if err == nil {
			t.Fatal("PostgreSQL loss permitted a new account reservation")
		}
		if got := readReservedUnits(t, replicaA, fixture.alpha, fixture.account.AccountID); got != 0 {
			t.Fatalf("failed PostgreSQL reservation retained %d units; want 0", got)
		}
		if _, err := loadPermitForTest(context.Background(), replicaA, fixture.alpha, request.PermitID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("failed PostgreSQL reservation load error = %v; want ErrNotFound", err)
		}
		permit, err := reservePermitForTest(context.Background(), replicaA, fixture.alpha, request)
		if err != nil {
			t.Fatalf("healthy PostgreSQL reservation after bounded fault: %v", err)
		}
		dispatched, err := authorizePermitForTest(context.Background(), replicaA, permit)
		if err != nil {
			t.Fatalf("authorize healthy reservation: %v", err)
		}
		if _, err := resolvePermitForTest(context.Background(), replicaA, dispatched); err != nil {
			t.Fatalf("resolve healthy reservation: %v", err)
		}
	})

	t.Run("outbox identity claim expiry and acknowledgement are durable", func(t *testing.T) {
		fixture := registerPermitTestFixture(t, credentials, replicaA, permitFixtureOptions{Capacity: 1})
		firstRequest := testAdmissionRequest(t, fixture.alpha, "outbox-key-first-"+fixture.suffix, "outbox-first-"+fixture.suffix)
		secondRequest := testAdmissionRequest(t, fixture.alpha, "outbox-key-second-"+fixture.suffix, "outbox-second-"+fixture.suffix)
		admitForTest(t, replicaA, fixture.alpha, firstRequest)
		admitForTest(t, replicaA, fixture.alpha, secondRequest)

		sharedID := "outbox-shared-" + fixture.suffix
		firstCommand := OutboxCommand{CommandID: sharedID, Kind: "start_run", PayloadRef: firstRequest.Spec.RunID}
		if err := insertOutboxForTest(context.Background(), replicaA, fixture.alpha, firstRequest.Spec.RunID, firstCommand); err != nil {
			t.Fatalf("insert first outbox command: %v", err)
		}
		if err := insertOutboxForTest(context.Background(), replicaB, fixture.alpha, firstRequest.Spec.RunID, firstCommand); err != nil {
			t.Fatalf("retry exact outbox command: %v", err)
		}
		changed := firstCommand
		changed.Kind = "reconcile_run"
		if err := insertOutboxForTest(context.Background(), replicaA, fixture.alpha, firstRequest.Spec.RunID, changed); !errors.Is(err, ErrImmutableConflict) {
			t.Fatalf("changed outbox identity error = %v; want ErrImmutableConflict", err)
		}
		secondCommand := OutboxCommand{CommandID: sharedID, Kind: "start_run", PayloadRef: secondRequest.Spec.RunID}
		if err := insertOutboxForTest(context.Background(), replicaB, fixture.alpha, secondRequest.Spec.RunID, secondCommand); err != nil {
			t.Fatalf("same command identity in a different run: %v", err)
		}
		if got := countOutboxCommands(t, replicaA, fixture.alpha, sharedID); got != 2 {
			t.Fatalf("run-scoped shared command rows = %d; want 2", got)
		}

		claim, err := claimOutboxForTest(context.Background(), replicaA, fixture.alpha, OutboxClaimRequest{
			RunID: firstRequest.Spec.RunID, CommandID: firstCommand.CommandID, OwnerID: "outbox-worker-one", ExpectedFence: 0, TTL: time.Second,
		})
		if err != nil {
			t.Fatalf("claim first outbox command: %v", err)
		}
		if claim.Command != firstCommand || outboxState(t, replicaA, fixture.alpha, firstRequest.Spec.RunID, firstCommand.CommandID) != "pending" {
			t.Fatalf("claim changed immutable command or asserted delivery")
		}
		if _, err := claimOutboxForTest(context.Background(), replicaB, fixture.alpha, OutboxClaimRequest{
			RunID: firstRequest.Spec.RunID, CommandID: firstCommand.CommandID, OwnerID: "outbox-worker-two", ExpectedFence: 0, TTL: time.Second,
		}); !errors.Is(err, ErrOutboxClaimLost) {
			t.Fatalf("live competing claim error = %v; want ErrOutboxClaimLost", err)
		}
		wrongFence := claim
		wrongFence.Fence++
		if _, err := acknowledgeOutboxForTest(context.Background(), replicaA, fixture.alpha, wrongFence); !errors.Is(err, ErrOutboxClaimLost) {
			t.Fatalf("wrong-fence acknowledgement error = %v; want ErrOutboxClaimLost", err)
		}
		acknowledgedAt, err := acknowledgeOutboxForTest(context.Background(), replicaA, fixture.alpha, claim)
		if err != nil || acknowledgedAt.IsZero() {
			t.Fatalf("acknowledge exact outbox claim: time=%v error=%v", acknowledgedAt, err)
		}
		if _, err := acknowledgeOutboxForTest(context.Background(), replicaB, fixture.alpha, claim); !errors.Is(err, ErrOutboxClaimLost) {
			t.Fatalf("repeated acknowledgement error = %v; want ErrOutboxClaimLost", err)
		}
		assertOutboxAcknowledged(t, replicaA, fixture.alpha, firstRequest.Spec.RunID, firstCommand.CommandID)

		retryCommand := firstRequest.Outbox
		expiringClaim, err := claimOutboxForTest(context.Background(), replicaA, fixture.alpha, OutboxClaimRequest{
			RunID: firstRequest.Spec.RunID, CommandID: retryCommand.CommandID, OwnerID: "outbox-stale-worker", ExpectedFence: 0, TTL: 25 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("claim retry command: %v", err)
		}
		waitForStoreDeadline(t, replicaB, fixture.alpha, expiringClaim.ExpiresAt)
		if _, err := acknowledgeOutboxForTest(context.Background(), replicaA, fixture.alpha, expiringClaim); !errors.Is(err, ErrOutboxClaimLost) {
			t.Fatalf("expired acknowledgement error = %v; want ErrOutboxClaimLost", err)
		}
		retryClaim, err := claimOutboxForTest(context.Background(), replicaB, fixture.alpha, OutboxClaimRequest{
			RunID: firstRequest.Spec.RunID, CommandID: retryCommand.CommandID, OwnerID: "outbox-retry-worker", ExpectedFence: expiringClaim.Fence, TTL: time.Second,
		})
		if err != nil {
			t.Fatalf("reclaim same outbox command: %v", err)
		}
		if retryClaim.Command != retryCommand || retryClaim.Fence != expiringClaim.Fence+1 {
			t.Fatalf("outbox retry changed command identity or fence")
		}
		if _, err := acknowledgeOutboxForTest(context.Background(), replicaA, fixture.alpha, expiringClaim); !errors.Is(err, ErrOutboxClaimLost) {
			t.Fatalf("superseded outbox claim acknowledgement error = %v; want ErrOutboxClaimLost", err)
		}
		if _, err := acknowledgeOutboxForTest(context.Background(), replicaB, fixture.alpha, retryClaim); err != nil {
			t.Fatalf("acknowledge reclaimed outbox command: %v", err)
		}

		isolatedCommand := OutboxCommand{CommandID: "outbox-isolated-" + fixture.suffix, Kind: "start_run", PayloadRef: firstRequest.Spec.RunID}
		if err := insertOutboxForTest(context.Background(), replicaA, fixture.alpha, firstRequest.Spec.RunID, isolatedCommand); err != nil {
			t.Fatalf("insert isolated outbox command: %v", err)
		}
		if _, err := claimOutboxForTest(context.Background(), replicaB, fixture.beta, OutboxClaimRequest{
			RunID: firstRequest.Spec.RunID, CommandID: isolatedCommand.CommandID, OwnerID: "foreign-workspace", ExpectedFence: 0, TTL: time.Second,
		}); !errors.Is(err, ErrOutboxClaimLost) {
			t.Fatalf("foreign workspace claim error = %v; want ErrOutboxClaimLost", err)
		}
		isolatedClaim, err := claimOutboxForTest(context.Background(), replicaA, fixture.alpha, OutboxClaimRequest{
			RunID: firstRequest.Spec.RunID, CommandID: isolatedCommand.CommandID, OwnerID: "own-workspace", ExpectedFence: 0, TTL: time.Second,
		})
		if err != nil {
			t.Fatalf("claim isolated outbox command in owning workspace: %v", err)
		}
		if _, err := acknowledgeOutboxForTest(context.Background(), replicaA, fixture.alpha, isolatedClaim); err != nil {
			t.Fatalf("acknowledge isolated outbox command: %v", err)
		}
	})
}

type permitFixtureOptions struct {
	AccountID      string
	AccountWitness string
	Capacity       int64
}

type permitTestFixture struct {
	suffix           string
	alpha            contract.Scope
	beta             contract.Scope
	account          ProviderAccount
	alphaConnections []string
	betaConnection   string
}

func registerPermitTestFixture(t *testing.T, credentials testStoreCredentials, pool *Pool, options permitFixtureOptions) permitTestFixture {
	t.Helper()
	suffix := randomHex(t, 8)
	if options.AccountID == "" {
		options.AccountID = "provider-account-" + suffix
	}
	if options.AccountWitness == "" {
		options.AccountWitness = "account-witness-" + suffix
	}
	fixture := permitTestFixture{
		suffix: suffix,
		alpha: contract.Scope{
			OrgID:       "org-permit-" + suffix,
			WorkspaceID: "workspace-alpha-" + suffix,
		},
		beta: contract.Scope{
			OrgID:       "org-permit-" + suffix,
			WorkspaceID: "workspace-beta-" + suffix,
		},
		account: ProviderAccount{
			OrgID:          "org-permit-" + suffix,
			AccountID:      options.AccountID,
			Provider:       "github",
			AccountWitness: options.AccountWitness,
			Capacity:       options.Capacity,
			Revision:       1,
		},
		alphaConnections: []string{"connection-alpha-a-" + suffix, "connection-alpha-b-" + suffix},
		betaConnection:   "connection-beta-" + suffix,
	}
	if fixture.account.Capacity <= 0 {
		t.Fatal("permit fixture capacity must be positive")
	}
	t.Cleanup(func() { cleanupTestScope(t, credentials, fixture.alpha, fixture.beta) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := WithinScope(ctx, pool, fixture.alpha, testActor(fixture.alpha), func(ctx context.Context, tx *Transaction) error {
		if err := RegisterWorkspace(ctx, tx, WorkspaceIdentity{Scope: fixture.alpha, Name: "alpha-" + suffix}); err != nil {
			return err
		}
		if err := RegisterProviderAccount(ctx, tx, fixture.account); err != nil {
			return err
		}
		for _, connectionID := range fixture.alphaConnections {
			if err := RegisterConnection(ctx, tx, Connection{
				Scope: fixture.alpha, ConnectionID: connectionID, ConnectorID: "github-rest",
				ProviderAccountID: fixture.account.AccountID, Revision: 1,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("register alpha permit fixture: %v", err)
	}
	if err := WithinScope(ctx, pool, fixture.beta, testActor(fixture.beta), func(ctx context.Context, tx *Transaction) error {
		if err := RegisterWorkspace(ctx, tx, WorkspaceIdentity{Scope: fixture.beta, Name: "beta-" + suffix}); err != nil {
			return err
		}
		return RegisterConnection(ctx, tx, Connection{
			Scope: fixture.beta, ConnectionID: fixture.betaConnection, ConnectorID: "github-rest",
			ProviderAccountID: fixture.account.AccountID, Revision: 1,
		})
	}); err != nil {
		t.Fatalf("register beta permit fixture: %v", err)
	}
	return fixture
}

func openPermitTestPool(t *testing.T, credentials testStoreCredentials, maxConns int32) *Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := OpenPool(ctx, PoolConfig{
		ConnectionString: credentials.runtimeDSN,
		MaxConns:         maxConns, MinConns: 0,
		MaxConnLifetime: 5 * time.Minute, MaxConnIdleTime: time.Minute, HealthCheckPeriod: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("open permit test replica: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func reservePermitForTest(ctx context.Context, pool *Pool, scope contract.Scope, request AccountPermitRequest) (AccountPermit, error) {
	var permit AccountPermit
	err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		permit, err = ReserveAccountPermit(ctx, tx, request)
		return err
	})
	return permit, err
}

func authorizePermitForTest(ctx context.Context, pool *Pool, permit AccountPermit) (AccountPermit, error) {
	var updated AccountPermit
	err := WithinScope(ctx, pool, permit.Scope, testActor(permit.Scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		updated, err = AuthorizeAccountPermit(ctx, tx, permit)
		return err
	})
	return updated, err
}

func markUnknownPermitForTest(ctx context.Context, pool *Pool, permit AccountPermit) (AccountPermit, error) {
	var updated AccountPermit
	err := WithinScope(ctx, pool, permit.Scope, testActor(permit.Scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		updated, err = MarkAccountPermitUnknown(ctx, tx, permit)
		return err
	})
	return updated, err
}

func reclaimPermitForTest(ctx context.Context, pool *Pool, permit AccountPermit, ownerID string, ttl time.Duration) (AccountPermit, error) {
	var updated AccountPermit
	err := WithinScope(ctx, pool, permit.Scope, testActor(permit.Scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		updated, err = ReclaimAccountPermit(ctx, tx, permit, ownerID, ttl)
		return err
	})
	return updated, err
}

func expirePermitForTest(ctx context.Context, pool *Pool, permit AccountPermit) (AccountPermit, error) {
	var updated AccountPermit
	err := WithinScope(ctx, pool, permit.Scope, testActor(permit.Scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		updated, err = ExpireAccountPermit(ctx, tx, permit)
		return err
	})
	return updated, err
}

func resolvePermitForTest(ctx context.Context, pool *Pool, permit AccountPermit) (AccountPermit, error) {
	var updated AccountPermit
	err := WithinScope(ctx, pool, permit.Scope, testActor(permit.Scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		updated, err = ResolveAccountPermit(ctx, tx, permit)
		return err
	})
	return updated, err
}

func loadPermitForTest(ctx context.Context, pool *Pool, scope contract.Scope, permitID string) (AccountPermit, error) {
	var permit AccountPermit
	err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		permit, err = LoadAccountPermit(ctx, tx, permitID)
		return err
	})
	return permit, err
}

func insertOutboxForTest(ctx context.Context, pool *Pool, scope contract.Scope, runID string, command OutboxCommand) error {
	return WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return InsertOutbox(ctx, tx, runID, command)
	})
}

func claimOutboxForTest(ctx context.Context, pool *Pool, scope contract.Scope, request OutboxClaimRequest) (OutboxClaim, error) {
	var claim OutboxClaim
	err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		claim, err = ClaimOutbox(ctx, tx, request)
		return err
	})
	return claim, err
}

func acknowledgeOutboxForTest(ctx context.Context, pool *Pool, scope contract.Scope, claim OutboxClaim) (time.Time, error) {
	var acknowledgedAt time.Time
	err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		acknowledgedAt, err = AcknowledgeOutbox(ctx, tx, claim)
		return err
	})
	return acknowledgedAt, err
}

func poolForPermit(permit AccountPermit, fixture permitTestFixture, replicaA, replicaB *Pool) *Pool {
	if permit.Scope.Equal(fixture.beta) {
		return replicaB
	}
	return replicaA
}

func readReservedUnits(t *testing.T, pool *Pool, scope contract.Scope, accountID string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var reserved int64
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return tx.queryRow(ctx, `
			SELECT reserved_units
			FROM pm.provider_accounts
			WHERE org_id = $1 AND provider_account_id = $2
		`, scope.OrgID, accountID).Scan(&reserved)
	}); err != nil {
		t.Fatalf("read provider account reserved units: %v", err)
	}
	return reserved
}

func countAccountPermits(t *testing.T, pool *Pool, scope contract.Scope, permitID string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var count int
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return tx.queryRow(ctx, `SELECT count(*) FROM pm.account_permits WHERE permit_id = $1`, permitID).Scan(&count)
	}); err != nil {
		t.Fatalf("count account permits: %v", err)
	}
	return count
}

func countOutboxCommands(t *testing.T, pool *Pool, scope contract.Scope, commandID string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var count int
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return tx.queryRow(ctx, `SELECT count(*) FROM pm.outbox_commands WHERE command_id = $1`, commandID).Scan(&count)
	}); err != nil {
		t.Fatalf("count outbox commands: %v", err)
	}
	return count
}

func outboxState(t *testing.T, pool *Pool, scope contract.Scope, runID, commandID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var state string
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return tx.queryRow(ctx, `SELECT state FROM pm.outbox_commands WHERE run_id = $1 AND command_id = $2`, runID, commandID).Scan(&state)
	}); err != nil {
		t.Fatalf("read outbox state: %v", err)
	}
	return state
}

func assertOutboxAcknowledged(t *testing.T, pool *Pool, scope contract.Scope, runID, commandID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var state string
	var ownerCleared, expiryCleared, acknowledged bool
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return tx.queryRow(ctx, `
			SELECT state, claim_owner IS NULL, claim_expires_at IS NULL, acknowledged_at IS NOT NULL
			FROM pm.outbox_commands
			WHERE run_id = $1 AND command_id = $2
		`, runID, commandID).Scan(&state, &ownerCleared, &expiryCleared, &acknowledged)
	}); err != nil {
		t.Fatalf("read acknowledged outbox command: %v", err)
	}
	if state != "acknowledged" || !ownerCleared || !expiryCleared || !acknowledged {
		t.Fatalf("outbox acknowledgement state = %q owner-cleared=%v expiry-cleared=%v acknowledged=%v", state, ownerCleared, expiryCleared, acknowledged)
	}
}

func waitForStoreDeadline(t *testing.T, pool *Pool, scope contract.Scope, deadline time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var reached bool
		err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			return tx.queryRow(ctx, `SELECT clock_timestamp() >= $1`, deadline).Scan(&reached)
		})
		if err != nil {
			t.Fatalf("wait for store deadline: %v", err)
		}
		if reached {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("store deadline was not reached")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
