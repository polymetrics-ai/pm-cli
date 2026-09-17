package auth_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

const (
	receiptProofOperation = "service_account.create"
	receiptProofTarget    = "service_accounts"
	receiptProofTimeout   = 20 * time.Second
)

type receiptAuthorizationBarrier struct {
	mu           sync.Mutex
	participants int
	arrivals     int
	release      chan struct{}
}

func newReceiptAuthorizationBarrier(participants int) *receiptAuthorizationBarrier {
	return &receiptAuthorizationBarrier{
		participants: participants,
		release:      make(chan struct{}),
	}
}

func (barrier *receiptAuthorizationBarrier) wrap(next auth.AuthorizationCallback) auth.AuthorizationCallback {
	return func(ctx context.Context, tx *store.Transaction, request auth.AuthorizationRequest) (auth.Authorization, error) {
		authorization, err := next(ctx, tx, request)
		if err != nil {
			return auth.Authorization{}, err
		}
		barrier.mu.Lock()
		barrier.arrivals++
		if barrier.arrivals == barrier.participants {
			close(barrier.release)
		}
		release := barrier.release
		barrier.mu.Unlock()
		select {
		case <-release:
			return authorization, nil
		case <-ctx.Done():
			return auth.Authorization{}, ctx.Err()
		}
	}
}

type serviceAccountCreateAttempt struct {
	request app.CreateServiceAccount
	result  app.ServiceAccountMutationResult
	err     error
}

type serviceAccountReceiptState struct {
	accountCount           int64
	credentialCount        int64
	receiptState           string
	resultRef              string
	hasSafeResult          bool
	hasResponseStatus      bool
	receiptTimesOrdered    bool
	responseAlreadyExpired bool
}

func TestConcurrentServiceAccountMutationReceipts(t *testing.T) {
	t.Run("equal intents commit one account and disclose one credential", func(t *testing.T) {
		fixture := newAuthFixture(t)
		authService := fixture.newAuthService(t)
		barrier := newReceiptAuthorizationBarrier(2)
		identityService := newReceiptProofIdentityService(t, fixture, barrier.wrap(authService.AuthorizeResource))
		plainIdentityService := newReceiptProofIdentityService(t, fixture, authService.AuthorizeResource)
		admin := directTestActor(fixture.ada)
		expiresAt := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Microsecond)
		request := receiptProofCreateRequest(fixture, "receipt-race-equal", "Equal race account", expiresAt)

		attempts := runConcurrentServiceAccountCreates(t, identityService, admin, fixture.alpha, [2]app.CreateServiceAccount{request, request})

		var original, replay *serviceAccountCreateAttempt
		for index := range attempts {
			attempt := &attempts[index]
			if attempt.err != nil {
				t.Fatalf("equal concurrent create %d failed: %v", index, attempt.err)
			}
			switch attempt.result.Mutation.Status {
			case app.StatusCreated:
				if original != nil {
					t.Fatal("equal concurrent creates returned more than one original result")
				}
				original = attempt
			case app.StatusOK:
				if replay != nil {
					t.Fatal("equal concurrent creates returned more than one replay result")
				}
				replay = attempt
			default:
				t.Fatalf("equal concurrent create status = %d, want 201 or 200", attempt.result.Mutation.Status)
			}
		}
		if original == nil || replay == nil {
			t.Fatal("equal concurrent creates did not return one original and one replay")
		}
		requireOriginalServiceAccountCreate(t, original.result)
		if !replay.result.Mutation.Replayed {
			t.Fatal("equal concurrent replay was not marked replayed")
		}
		if replay.result.OneTimeCredential != "" {
			t.Fatal("equal concurrent replay disclosed credential material")
		}
		if !equalServiceAccountMetadata(original.result.ServiceAccount, replay.result.ServiceAccount) ||
			original.result.Mutation.ResultRef != replay.result.Mutation.ResultRef ||
			original.result.Mutation.Revision != replay.result.Mutation.Revision ||
			original.result.Mutation.ControlRevision != replay.result.Mutation.ControlRevision {
			t.Fatal("equal concurrent replay did not return the original safe account metadata and revisions")
		}

		ctx, cancel := context.WithTimeout(context.Background(), receiptProofTimeout)
		defer cancel()
		requireOnlyServiceAccount(t, ctx, plainIdentityService, admin, fixture.alpha, original.result.ServiceAccount.ServiceAccountID)
		state := readServiceAccountReceiptState(t, fixture, request.Control.IdempotencyKey)
		requireServiceAccountReceiptState(t, state, store.MutationReceiptResolved, original.result.Mutation.ResultRef, true)
		identity, err := authService.AuthenticateServiceCredential(ctx, original.result.OneTimeCredential)
		if err != nil {
			t.Fatalf("authenticate original concurrent credential: %v", err)
		}
		if identity.Principal != original.result.ServiceAccount.Principal {
			t.Fatal("original concurrent credential did not authenticate its created account")
		}

		fixture.revokeMembership(t, fixture.alpha, fixture.ada)
		replayRequest := request
		replayRequest.Control.ExpectedRevision = nil
		blockedResult, blockedErr := plainIdentityService.CreateServiceAccount(ctx, admin, fixture.alpha, replayRequest)
		requireReceiptFailure(t, blockedErr, 403, "grant_unavailable", "refresh_authorization", "")
		requireEmptyServiceAccountMutation(t, blockedResult, "revoked authorization replay")
	})

	t.Run("conflicting intents select one winner before stale CAS", func(t *testing.T) {
		fixture := newAuthFixture(t)
		authService := fixture.newAuthService(t)
		barrier := newReceiptAuthorizationBarrier(2)
		identityService := newReceiptProofIdentityService(t, fixture, barrier.wrap(authService.AuthorizeResource))
		plainIdentityService := newReceiptProofIdentityService(t, fixture, authService.AuthorizeResource)
		admin := directTestActor(fixture.ada)
		expiresAt := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Microsecond)
		first := receiptProofCreateRequest(fixture, "receipt-race-conflict", "First conflicting account", expiresAt)
		second := receiptProofCreateRequest(fixture, "receipt-race-conflict", "Second conflicting account", expiresAt)

		attempts := runConcurrentServiceAccountCreates(t, identityService, admin, fixture.alpha, [2]app.CreateServiceAccount{first, second})

		var winner, loser *serviceAccountCreateAttempt
		for index := range attempts {
			attempt := &attempts[index]
			if attempt.err == nil {
				if winner != nil {
					t.Fatal("conflicting concurrent creates returned two winners")
				}
				winner = attempt
				continue
			}
			if loser != nil {
				t.Fatal("conflicting concurrent creates returned two failures")
			}
			loser = attempt
		}
		if winner == nil || loser == nil {
			t.Fatal("conflicting concurrent creates did not return one winner and one refusal")
		}
		requireOriginalServiceAccountCreate(t, winner.result)
		requireReceiptFailure(t, loser.err, app.StatusConflict, "idempotency_conflict", "new_key_for_new_intent", "")
		if !errors.Is(loser.err, store.ErrIdempotencyConflict) {
			t.Fatalf("conflicting concurrent refusal cause = %v, want idempotency conflict", loser.err)
		}
		requireEmptyServiceAccountMutation(t, loser.result, "conflicting concurrent loser")
		if winner.request.DisplayName == nil || winner.result.ServiceAccount.DisplayName == nil ||
			*winner.result.ServiceAccount.DisplayName != *winner.request.DisplayName {
			t.Fatal("conflicting concurrent winner did not preserve its selected intent")
		}

		ctx, cancel := context.WithTimeout(context.Background(), receiptProofTimeout)
		defer cancel()
		requireOnlyServiceAccount(t, ctx, plainIdentityService, admin, fixture.alpha, winner.result.ServiceAccount.ServiceAccountID)
		state := readServiceAccountReceiptState(t, fixture, first.Control.IdempotencyKey)
		requireServiceAccountReceiptState(t, state, store.MutationReceiptResolved, winner.result.Mutation.ResultRef, true)
	})
}

func TestCompactedServiceAccountMutationReceipt(t *testing.T) {
	fixture := newAuthFixture(t)
	authService := fixture.newAuthService(t)
	identityService := newReceiptProofIdentityService(t, fixture, authService.AuthorizeResource)
	admin := directTestActor(fixture.ada)
	ctx, cancel := context.WithTimeout(context.Background(), receiptProofTimeout)
	defer cancel()
	expiresAt := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Microsecond)
	request := receiptProofCreateRequest(fixture, "receipt-compaction", "Compacted receipt account", expiresAt)

	created, err := identityService.CreateServiceAccount(ctx, admin, fixture.alpha, request)
	if err != nil {
		t.Fatalf("create account for receipt compaction: %v", err)
	}
	requireOriginalServiceAccountCreate(t, created)
	state := readServiceAccountReceiptState(t, fixture, request.Control.IdempotencyKey)
	requireServiceAccountReceiptState(t, state, store.MutationReceiptResolved, created.Mutation.ResultRef, true)

	fixture.execMigrator(t, fixture.alpha, `
		UPDATE pm.identity_mutation_keys
		SET resolved_at = clock_timestamp() - interval '8 days',
		    response_expires_at = clock_timestamp() - interval '1 second',
		    updated_at = clock_timestamp()
		WHERE org_id = $1 AND workspace_id = $2 AND principal_id = $3
		  AND operation_id = $4 AND target_id = $5 AND idempotency_key = $6
		  AND receipt_state = 'resolved' AND result_ref = $7
	`, fixture.alpha.OrgID, fixture.alpha.WorkspaceID, fixture.ada.PrincipalID,
		receiptProofOperation, receiptProofTarget, request.Control.IdempotencyKey, created.Mutation.ResultRef)
	state = readServiceAccountReceiptState(t, fixture, request.Control.IdempotencyKey)
	if !state.receiptTimesOrdered || !state.responseAlreadyExpired {
		t.Fatalf("expired receipt SQL chronology/expiry = %t/%t, want true/true", state.receiptTimesOrdered, state.responseAlreadyExpired)
	}

	var compacted uint32
	err = store.WithinScope(ctx, fixture.pool, fixture.alpha, admin, func(ctx context.Context, tx *store.Transaction) error {
		through, timeErr := store.ReadAuthenticationTime(ctx, tx)
		if timeErr != nil {
			return timeErr
		}
		compacted, timeErr = store.CompactIdentityMutationReceipts(ctx, tx, through.UTC(), 1)
		return timeErr
	})
	if err != nil {
		t.Fatalf("compact expired identity mutation receipt: %v", err)
	}
	if compacted != 1 {
		t.Fatalf("compacted receipt count = %d, want 1", compacted)
	}
	state = readServiceAccountReceiptState(t, fixture, request.Control.IdempotencyKey)
	requireServiceAccountReceiptState(t, state, store.MutationReceiptCompacted, created.Mutation.ResultRef, false)

	replayRequest := request
	replayRequest.Control.ExpectedRevision = nil
	expiredResult, expiredErr := identityService.CreateServiceAccount(ctx, admin, fixture.alpha, replayRequest)
	requireReceiptFailure(t, expiredErr, app.StatusGone, "retry_receipt_expired", "inspect_existing_result", created.Mutation.ResultRef)
	if !errors.Is(expiredErr, store.ErrMutationReceiptExpired) {
		t.Fatalf("expired receipt refusal cause = %v, want mutation receipt expired", expiredErr)
	}
	requireEmptyServiceAccountMutation(t, expiredResult, "expired receipt replay")

	conflicting := request
	changedDisplayName := "Changed compacted receipt intent"
	conflicting.DisplayName = &changedDisplayName
	conflictResult, conflictErr := identityService.CreateServiceAccount(ctx, admin, fixture.alpha, conflicting)
	requireReceiptFailure(t, conflictErr, app.StatusConflict, "idempotency_conflict", "new_key_for_new_intent", "")
	if !errors.Is(conflictErr, store.ErrIdempotencyConflict) {
		t.Fatalf("compacted conflicting refusal cause = %v, want idempotency conflict", conflictErr)
	}
	requireEmptyServiceAccountMutation(t, conflictResult, "compacted conflicting intent")
	requireOnlyServiceAccount(t, ctx, identityService, admin, fixture.alpha, created.ServiceAccount.ServiceAccountID)
	state = readServiceAccountReceiptState(t, fixture, request.Control.IdempotencyKey)
	requireServiceAccountReceiptState(t, state, store.MutationReceiptCompacted, created.Mutation.ResultRef, false)

	identity, err := authService.AuthenticateServiceCredential(ctx, created.OneTimeCredential)
	if err != nil {
		t.Fatalf("authenticate original credential after receipt compaction: %v", err)
	}
	if identity.Principal != created.ServiceAccount.Principal {
		t.Fatal("receipt compaction changed the original service-account identity")
	}

	fixture.revokeMembership(t, fixture.alpha, fixture.ada)
	blockedResult, blockedErr := identityService.CreateServiceAccount(ctx, admin, fixture.alpha, replayRequest)
	requireReceiptFailure(t, blockedErr, 403, "grant_unavailable", "refresh_authorization", "")
	if errors.Is(blockedErr, store.ErrMutationReceiptExpired) {
		t.Fatal("revoked authorization disclosed expired receipt metadata")
	}
	requireEmptyServiceAccountMutation(t, blockedResult, "revoked authorization expired replay")
	state = readServiceAccountReceiptState(t, fixture, request.Control.IdempotencyKey)
	requireServiceAccountReceiptState(t, state, store.MutationReceiptCompacted, created.Mutation.ResultRef, false)
}

func newReceiptProofIdentityService(
	t *testing.T,
	fixture *authFixture,
	authorize auth.AuthorizationCallback,
) *app.IdentityService {
	t.Helper()
	service, err := app.NewIdentityService(app.IdentityServiceConfig{
		Pool:             fixture.pool,
		DeploymentID:     fixture.deployment,
		Authorize:        authorize,
		ReceiptRetention: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("construct receipt proof identity service: %v", err)
	}
	return service
}

func receiptProofCreateRequest(
	fixture *authFixture,
	key string,
	displayName string,
	expiresAt time.Time,
) app.CreateServiceAccount {
	return app.CreateServiceAccount{
		DisplayName:     &displayName,
		WorkspaceScopes: []string{fixture.alpha.WorkspaceID},
		ConnectionIDs:   []string{fixture.connection},
		OperationIDs:    []string{"operation-read"},
		ExpiresAt:       expiresAt,
		Control:         mutationControl(key, 1, true),
	}
}

func runConcurrentServiceAccountCreates(
	t *testing.T,
	service *app.IdentityService,
	actor contract.Actor,
	scope contract.Scope,
	requests [2]app.CreateServiceAccount,
) []serviceAccountCreateAttempt {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), receiptProofTimeout)
	defer cancel()
	start := make(chan struct{})
	results := make(chan serviceAccountCreateAttempt, len(requests))
	for index := range requests {
		request := requests[index]
		go func() {
			select {
			case <-start:
			case <-ctx.Done():
				results <- serviceAccountCreateAttempt{request: request, err: ctx.Err()}
				return
			}
			result, err := service.CreateServiceAccount(ctx, actor, scope, request)
			results <- serviceAccountCreateAttempt{request: request, result: result, err: err}
		}()
	}
	close(start)

	attempts := make([]serviceAccountCreateAttempt, 0, len(requests))
	for len(attempts) < len(requests) {
		select {
		case attempt := <-results:
			attempts = append(attempts, attempt)
		case <-ctx.Done():
			t.Fatalf("concurrent service-account creates exceeded bounded context after %d results", len(attempts))
		}
	}
	return attempts
}

func readServiceAccountReceiptState(
	t *testing.T,
	fixture *authFixture,
	idempotencyKey string,
) serviceAccountReceiptState {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), receiptProofTimeout)
	defer cancel()
	connection, err := pgx.Connect(ctx, fixture.migratorDSN)
	if err != nil {
		t.Fatalf("connect receipt proof migrator: %T", err)
	}
	defer connection.Close(ctx)
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin receipt proof query: %T", err)
	}
	defer tx.Rollback(ctx)
	if err := bindFixtureScope(ctx, tx, fixture.deployment, fixture.alpha, fixture.ada); err != nil {
		t.Fatalf("bind receipt proof query scope: %T", err)
	}

	var state serviceAccountReceiptState
	err = tx.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM pm.service_accounts
			 WHERE org_id = $1 AND workspace_id = $2),
			(SELECT count(*) FROM pm.service_account_credentials
			 WHERE deployment_id = $3 AND org_id = $1 AND workspace_id = $2),
			receipt_state, COALESCE(result_ref, ''), safe_result IS NOT NULL,
			response_status IS NOT NULL,
			resolved_at IS NOT NULL AND response_expires_at IS NOT NULL AND response_expires_at > resolved_at,
			response_expires_at <= clock_timestamp()
		FROM pm.identity_mutation_keys
		WHERE org_id = $1 AND workspace_id = $2 AND principal_id = $4
		  AND operation_id = $5 AND target_id = $6 AND idempotency_key = $7
	`, fixture.alpha.OrgID, fixture.alpha.WorkspaceID, fixture.deployment,
		fixture.ada.PrincipalID, receiptProofOperation, receiptProofTarget, idempotencyKey).Scan(
		&state.accountCount,
		&state.credentialCount,
		&state.receiptState,
		&state.resultRef,
		&state.hasSafeResult,
		&state.hasResponseStatus,
		&state.receiptTimesOrdered,
		&state.responseAlreadyExpired,
	)
	if err != nil {
		t.Fatalf("query receipt proof state: %T", err)
	}
	return state
}

func requireOriginalServiceAccountCreate(t *testing.T, result app.ServiceAccountMutationResult) {
	t.Helper()
	if result.Mutation.Status != app.StatusCreated || result.Mutation.Replayed ||
		result.Mutation.ResultRef == "" || result.Mutation.ResultRef != result.ServiceAccount.ServiceAccountID {
		t.Fatalf("original service-account metadata was not a non-replayed 201: status=%d replayed=%t", result.Mutation.Status, result.Mutation.Replayed)
	}
	if result.OneTimeCredential == "" {
		t.Fatal("original service-account create omitted its one-time credential")
	}
}

func requireOnlyServiceAccount(
	t *testing.T,
	ctx context.Context,
	service *app.IdentityService,
	actor contract.Actor,
	scope contract.Scope,
	wantID string,
) {
	t.Helper()
	page, err := service.ListServiceAccounts(ctx, actor, scope, "", 100)
	if err != nil {
		t.Fatalf("list service accounts for receipt proof: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("visible service-account count = %d, want 1", len(page.Items))
	}
	if page.Items[0].ServiceAccountID != wantID {
		t.Fatalf("visible service-account id = %q, want committed result ref %q", page.Items[0].ServiceAccountID, wantID)
	}
}

func requireServiceAccountReceiptState(
	t *testing.T,
	state serviceAccountReceiptState,
	wantState store.MutationReceiptState,
	wantResultRef string,
	wantSafeResponse bool,
) {
	t.Helper()
	if state.accountCount != 1 || state.credentialCount != 1 {
		t.Fatalf("physical service-account/credential counts = %d/%d, want 1/1", state.accountCount, state.credentialCount)
	}
	if state.receiptState != string(wantState) || state.resultRef != wantResultRef {
		t.Fatalf("receipt state/result ref = %q/%q, want %q/%q", state.receiptState, state.resultRef, wantState, wantResultRef)
	}
	if state.hasSafeResult != wantSafeResponse || state.hasResponseStatus != wantSafeResponse {
		t.Fatalf("receipt safe-result/status presence = %t/%t, want %t/%t", state.hasSafeResult, state.hasResponseStatus, wantSafeResponse, wantSafeResponse)
	}
}

func requireReceiptFailure(
	t *testing.T,
	err error,
	wantStatus int,
	wantCode string,
	wantRetryAction string,
	wantResultRef string,
) *app.Failure {
	t.Helper()
	var failure *app.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("error type = %T, want application failure", err)
	}
	if failure.HTTPStatus != wantStatus || failure.Code != wantCode ||
		failure.RetryAction != wantRetryAction || failure.ExistingResultRef != wantResultRef {
		t.Fatalf("application failure status/code/retry/ref = %d/%q/%q/%q, want %d/%q/%q/%q",
			failure.HTTPStatus, failure.Code, failure.RetryAction, failure.ExistingResultRef,
			wantStatus, wantCode, wantRetryAction, wantResultRef)
	}
	return failure
}

func requireEmptyServiceAccountMutation(t *testing.T, result app.ServiceAccountMutationResult, operation string) {
	t.Helper()
	if result.OneTimeCredential != "" {
		t.Fatalf("%s disclosed credential material", operation)
	}
	if result.ServiceAccount.ServiceAccountID != "" || result.Mutation.ResultRef != "" ||
		result.Mutation.Status != 0 || result.Mutation.Replayed {
		t.Fatalf("%s returned mutation metadata despite refusal", operation)
	}
}

func equalServiceAccountMetadata(left, right app.ServiceAccount) bool {
	return left.ServiceAccountID == right.ServiceAccountID &&
		left.Principal == right.Principal &&
		equalOptionalString(left.DisplayName, right.DisplayName) &&
		slices.Equal(left.WorkspaceScopes, right.WorkspaceScopes) &&
		slices.Equal(left.ConnectionIDs, right.ConnectionIDs) &&
		slices.Equal(left.OperationIDs, right.OperationIDs) &&
		left.GrantRevision == right.GrantRevision &&
		left.PolicyEpoch == right.PolicyEpoch &&
		left.ControlRevision == right.ControlRevision &&
		left.ExpiresAt.Equal(right.ExpiresAt) &&
		equalOptionalTime(left.RevokedAt, right.RevokedAt)
}

func equalOptionalString(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func equalOptionalTime(left, right *time.Time) bool {
	return left == nil && right == nil || left != nil && right != nil && left.Equal(*right)
}
