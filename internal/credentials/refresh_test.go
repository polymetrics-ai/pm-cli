package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/store"
)

func TestScopedRefreshLeaseAndRedaction(t *testing.T) {
	if requestPath := os.Getenv(credentialProcessRequestEnvironment); requestPath != "" {
		runCredentialProcessWorker(t, requestPath)
		return
	}
	pools := prepareCredentialTestPools(t)

	t.Run("authorization and key refusal perform zero provider operations", func(t *testing.T) {
		for _, test := range []struct {
			name         string
			authorizeErr error
			keyErr       error
			want         error
			wantKeyCalls int64
		}{
			{name: "authorization denied", authorizeErr: errors.New("denied refresh canary"), want: ErrAuthorizationDenied, wantKeyCalls: 0},
			{name: "wrapping key unavailable", keyErr: errors.New("missing refresh key canary"), want: ErrWrappingKeyUnavailable, wantKeyCalls: 1},
		} {
			test := test
			t.Run(test.name, func(t *testing.T) {
				fixture := newCredentialFixture(t, pools)
				_, binding := fixture.storeCredential(t, []byte("refresh-base-"+credentialRandomHex(t, 8)), "refresh-base")
				resolver := &countingKeyResolver{key: fixture.key, err: test.keyErr}
				authorization := &countingAuthorization{err: test.authorizeErr}
				var providerCalls atomic.Int64
				_, err := RefreshCAS(
					context.Background(),
					pools.first,
					resolver.resolve,
					authorization.authorize,
					func(context.Context, CredentialMaterial) RefreshAttempt {
						providerCalls.Add(1)
						return RefreshAttempt{Disposition: RefreshSucceeded, Credential: []byte("must-not-run")}
					},
					fixture.refreshRequest(binding, "lease-zero-effect", "owner-zero-effect", time.Second),
				)
				if !errors.Is(err, test.want) {
					t.Fatalf("refresh refusal error = %v; want %v", err, test.want)
				}
				if providerCalls.Load() != 0 || resolver.calls.Load() != test.wantKeyCalls {
					t.Fatalf("refresh refusal provider/key calls = %d/%d", providerCalls.Load(), resolver.calls.Load())
				}
				lifecycle, lifecycleErr := fixture.loadLifecycle(t)
				if lifecycleErr != nil {
					t.Fatalf("load lifecycle after refresh refusal: %v", lifecycleErr)
				}
				if lifecycle.Status != store.CredentialLifecycleActive || lifecycle.CiphertextVersion != 1 {
					t.Fatalf("refresh refusal mutated lifecycle: %+v", lifecycle)
				}
			})
		}

		fixture := newCredentialFixture(t, pools)
		_, binding := fixture.storeCredential(t, []byte("reconciliation-only-base"), "reconciliation-only-base")
		request := fixture.refreshRequest(binding, "lease-reconciliation-only", "owner-reconciliation-only", time.Second)
		request.Effect.ReconciliationOnly = true
		resolver := &countingKeyResolver{key: fixture.key}
		authorization := &countingAuthorization{}
		var providerCalls atomic.Int64
		_, err := RefreshCAS(
			context.Background(),
			pools.first,
			resolver.resolve,
			authorization.authorize,
			func(context.Context, CredentialMaterial) RefreshAttempt {
				providerCalls.Add(1)
				return RefreshAttempt{Disposition: RefreshSucceeded, Credential: []byte("must-not-run")}
			},
			request,
		)
		if err == nil {
			t.Fatal("reconciliation-only authority unexpectedly admitted credential refresh")
		}
		if authorization.calls.Load() != 0 || resolver.calls.Load() != 0 || providerCalls.Load() != 0 {
			t.Fatal("reconciliation-only refresh crossed authorization, key, or provider boundary")
		}
	})

	t.Run("known rejection restores active version and redacts provider diagnostics", func(t *testing.T) {
		fixture := newCredentialFixture(t, pools)
		original := []byte("refresh-rejected-old-" + credentialRandomHex(t, 8))
		_, binding := fixture.storeCredential(t, original, "rejected-base")
		resolver := &countingKeyResolver{key: fixture.key}
		authorization := &countingAuthorization{}
		canary := "refresh-rejection-error-" + credentialRandomHex(t, 8)
		status, err := RefreshCAS(
			context.Background(),
			pools.first,
			resolver.resolve,
			authorization.authorize,
			func(_ context.Context, material CredentialMaterial) RefreshAttempt {
				if useErr := material.Use(func(secret []byte) error {
					if !bytes.Equal(secret, original) {
						return errors.New("wrong refresh input")
					}
					return nil
				}); useErr != nil {
					return RefreshAttempt{Disposition: RefreshOutcomeUnknown}
				}
				return RefreshAttempt{
					Disposition: RefreshRejected,
					Diagnostic: Diagnostic{
						Message: canary,
						Headers: map[string][]string{"Authorization": {canary}},
						Body:    []byte(canary),
						Err:     errors.New(canary),
					},
				}
			},
			fixture.refreshRequest(binding, "lease-rejected", "owner-rejected", time.Second),
		)
		if !errors.Is(err, ErrRefreshRejected) {
			t.Fatalf("known refresh rejection error = %v; want ErrRefreshRejected", err)
		}
		if strings.Contains(err.Error(), canary) {
			t.Fatal("known refresh rejection exposed provider diagnostic canary")
		}
		if status.Status != store.CredentialLifecycleActive || status.CiphertextVersion != 1 {
			t.Fatalf("known rejection did not preserve active version: %+v", status)
		}
		fixture.requireEncryptedNotFound(t, 2)
	})

	t.Run("wrapping key loss after a rotating response forces reauthorization", func(t *testing.T) {
		fixture := newCredentialFixture(t, pools)
		original := []byte("post-response-key-old-" + credentialRandomHex(t, 8))
		_, binding := fixture.storeCredential(t, original, "post-response-key-base")
		var keyCalls atomic.Int64
		canary := "post-response-key-canary-" + credentialRandomHex(t, 8)
		resolveKey := func(context.Context, string) ([]byte, error) {
			if keyCalls.Add(1) == 1 {
				return fixture.key, nil
			}
			return nil, errors.New(canary)
		}
		status, err := RefreshCAS(
			context.Background(),
			pools.first,
			resolveKey,
			(&countingAuthorization{}).authorize,
			func(_ context.Context, material CredentialMaterial) RefreshAttempt {
				if useErr := material.Use(func(secret []byte) error {
					if !bytes.Equal(secret, original) {
						return errors.New("wrong post-response credential")
					}
					return nil
				}); useErr != nil {
					return RefreshAttempt{Disposition: RefreshOutcomeUnknown}
				}
				return RefreshAttempt{Disposition: RefreshSucceeded, Credential: []byte("post-response-key-new")}
			},
			fixture.refreshRequest(binding, "lease-post-response-key", "owner-post-response-key", time.Second),
		)
		if !errors.Is(err, ErrRefreshOutcomeUncertain) || strings.Contains(err.Error(), canary) {
			t.Fatalf("post-response key loss error = %v; want redacted uncertainty", err)
		}
		if status.Status != store.CredentialLifecycleReauthorizationRequired ||
			status.CiphertextVersion != binding.CiphertextVersion {
			t.Fatalf("post-response key loss status = %+v", status)
		}
		lifecycle, lifecycleErr := fixture.loadLifecycle(t)
		if lifecycleErr != nil {
			t.Fatalf("load post-response key-loss lifecycle: %v", lifecycleErr)
		}
		if lifecycle.Status != store.CredentialLifecycleReauthorizationRequired ||
			lifecycle.CiphertextVersion != binding.CiphertextVersion {
			t.Fatalf("post-response key loss did not force reauthorization: %+v", lifecycle)
		}
		fixture.requireEncryptedNotFound(t, binding.CiphertextVersion+1)
	})

	t.Run("authorization revoked after provider dispatch records uncertainty without ciphertext", func(t *testing.T) {
		fixture := newCredentialFixture(t, pools)
		original := []byte("post-dispatch-authorization-old-" + credentialRandomHex(t, 8))
		_, binding := fixture.storeCredential(t, original, "post-dispatch-authorization-base")
		resolver := &countingKeyResolver{key: fixture.key}
		var authorizationCalls atomic.Int64
		authorizationCanary := "post-dispatch-authorization-canary-" + credentialRandomHex(t, 8)
		authorize := func(_ context.Context, _ *store.Transaction, access Access) error {
			authorizationCalls.Add(1)
			if access.Stage == StagePersist {
				return errors.New(authorizationCanary)
			}
			return nil
		}
		var providerCalls atomic.Int64
		status, err := RefreshCAS(
			context.Background(),
			pools.first,
			resolver.resolve,
			authorize,
			func(_ context.Context, material CredentialMaterial) RefreshAttempt {
				providerCalls.Add(1)
				if useErr := material.Use(func(secret []byte) error {
					if !bytes.Equal(secret, original) {
						return errors.New("wrong post-dispatch authorization credential")
					}
					return nil
				}); useErr != nil {
					return RefreshAttempt{Disposition: RefreshOutcomeUnknown}
				}
				return RefreshAttempt{
					Disposition: RefreshSucceeded,
					Credential:  []byte("must-not-persist-after-authorization-revocation"),
				}
			},
			fixture.refreshRequest(binding, "lease-post-dispatch-authorization", "owner-post-dispatch-authorization", 5*time.Second),
		)
		if !errors.Is(err, ErrRefreshOutcomeUncertain) ||
			strings.Contains(err.Error(), authorizationCanary) ||
			status.Status != store.CredentialLifecycleReauthorizationRequired ||
			status.CiphertextVersion != binding.CiphertextVersion ||
			authorizationCalls.Load() != 2 ||
			resolver.calls.Load() != 1 ||
			providerCalls.Load() != 1 {
			t.Fatalf(
				"post-dispatch authorization loss was not safely retained: %T status=%+v calls=%d/%d/%d",
				err,
				status,
				authorizationCalls.Load(),
				resolver.calls.Load(),
				providerCalls.Load(),
			)
		}
		fixture.requireEncryptedNotFound(t, binding.CiphertextVersion+1)
	})

	t.Run("separate worker processes coordinate one provider refresh across an ahead account alias", func(t *testing.T) {
		fixture := newCredentialFixture(t, pools)
		original := []byte("shared-owner-old-" + credentialRandomHex(t, 8))
		_, binding := fixture.storeCredential(t, original, "two-process-owner-base")
		alias := newCredentialAlias(t, pools, fixture)
		aliasOldSecret := []byte("shared-alias-old-" + credentialRandomHex(t, 8))
		_, _ = alias.storeCredential(t, aliasOldSecret, "two-process-owner-alias-old")
		aliasSecret := []byte("shared-alias-current-" + credentialRandomHex(t, 8))
		_, aliasBinding := alias.storeCredential(t, aliasSecret, "two-process-owner-alias-current")
		if aliasBinding.CiphertextVersion <= binding.CiphertextVersion {
			t.Fatal("alias credential version did not advance beyond the current refresh owner")
		}

		directory := t.TempDir()
		ownerReady := filepath.Join(directory, "owner-ready")
		ownerRelease := filepath.Join(directory, "owner-release")
		ownerProvider := filepath.Join(directory, "owner-provider-call")
		ownerRequest := fixture.refreshRequest(binding, "lease-process-owner", "process-owner", 10*time.Second)
		owner := startCredentialProcess(t, directory, "owner", credentialProcessRequest{
			Action:       credentialProcessRefreshOwner,
			Refresh:      ownerRequest,
			ReadyPath:    ownerReady,
			ReleasePath:  ownerRelease,
			ProviderPath: ownerProvider,
		})
		waitForCredentialProcessPath(t, ownerReady, 5*time.Second)

		contenderProvider := filepath.Join(directory, "contender-provider-call")
		contenderRequest := alias.refreshRequest(aliasBinding, "lease-process-contender", "process-contender", 10*time.Second)
		contenderRequest.Effect.DispatchID = "dispatch-process-contender"
		contenderRequest.Effect.AttemptID = "attempt-process-contender"
		contender := startCredentialProcess(t, directory, "contender", credentialProcessRequest{
			Action:       credentialProcessRefreshContender,
			Refresh:      contenderRequest,
			ProviderPath: contenderProvider,
		})
		contenderReceipt := contender.wait(t)
		if contenderReceipt.Outcome != "refresh_outcome_uncertain" {
			t.Fatalf("alias worker outcome = %q; want refresh_outcome_uncertain", contenderReceipt.Outcome)
		}
		if contenderReceipt.ProviderCalls != 0 ||
			contenderReceipt.KeyCalls != 0 ||
			contenderReceipt.AuthorizationCalls != 1 {
			t.Fatalf(
				"alias worker provider/key/authorization calls = %d/%d/%d; want 0/0/1",
				contenderReceipt.ProviderCalls,
				contenderReceipt.KeyCalls,
				contenderReceipt.AuthorizationCalls,
			)
		}
		if _, err := os.Stat(contenderProvider); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("alias worker provider marker exists: %T", err)
		}

		writeCredentialProcessSignal(t, ownerRelease)
		ownerReceipt := owner.wait(t)
		if ownerReceipt.Outcome != "refresh_completed" ||
			ownerReceipt.ProviderCalls != 1 ||
			ownerReceipt.KeyCalls != 2 ||
			ownerReceipt.AuthorizationCalls != 2 ||
			ownerReceipt.Status == nil ||
			ownerReceipt.Status.CiphertextVersion != binding.CiphertextVersion+1 ||
			ownerReceipt.Status.Status != store.CredentialLifecycleActive {
			t.Fatalf("owner worker receipt = %+v", ownerReceipt)
		}
		if ownerReceipt.ProcessID == contenderReceipt.ProcessID ||
			ownerReceipt.ProcessID == os.Getpid() ||
			contenderReceipt.ProcessID == os.Getpid() {
			t.Fatalf("refresh workers were not distinct OS processes: owner=%d contender=%d parent=%d", ownerReceipt.ProcessID, contenderReceipt.ProcessID, os.Getpid())
		}
		if _, err := os.Stat(ownerProvider); err != nil {
			t.Fatalf("owner provider marker missing: %T", err)
		}

		aliasLifecycle, err := alias.loadLifecycle(t)
		if err != nil {
			t.Fatalf("load refused alias lifecycle: %v", err)
		}
		if aliasLifecycle.Status != store.CredentialLifecycleActive ||
			aliasLifecycle.CiphertextVersion != aliasBinding.CiphertextVersion {
			t.Fatalf("refused alias refresh mutated lifecycle: %+v", aliasLifecycle)
		}
		rotated := fixture.loadEncrypted(t, binding.CiphertextVersion+1)
		if !rotated.Scope.Equal(fixture.scope) ||
			rotated.ProviderAccountID != fixture.account.AccountID ||
			rotated.Binding.CredentialRef != binding.CredentialRef ||
			rotated.Binding.WorkspaceID != binding.WorkspaceID ||
			rotated.Binding.ConnectionID != binding.ConnectionID ||
			rotated.Binding.KeyRef != binding.KeyRef ||
			rotated.Binding.AuthEpoch != binding.AuthEpoch ||
			rotated.Binding.ProviderAccountWitness != binding.ProviderAccountWitness ||
			!slices.Equal(rotated.Binding.Scopes, binding.Scopes) {
			t.Fatalf("successful refresh widened or changed credential binding: %+v", rotated.Binding)
		}
		rotatedPlaintext, err := openCredential(fixture.key, rotated)
		if err != nil {
			t.Fatalf("decrypt successful refresh result: %v", err)
		}
		defer wipe(rotatedPlaintext)
		if !bytes.HasPrefix(rotatedPlaintext, []byte("separate-process-rotated-")) ||
			bytes.Equal(rotatedPlaintext, original) ||
			bytes.Equal(rotatedPlaintext, aliasOldSecret) ||
			bytes.Equal(rotatedPlaintext, aliasSecret) {
			t.Fatal("successful refresh stored the wrong credential")
		}
		assertCredentialProcessArtifactsSecretFree(
			t,
			[]string{
				owner.requestPath,
				owner.receiptPath,
				ownerReady,
				ownerRelease,
				ownerProvider,
				contender.requestPath,
				contender.receiptPath,
			},
			[][]byte{owner.output.Bytes(), contender.output.Bytes(), owner.launchProjection(), contender.launchProjection()},
			original,
			aliasOldSecret,
			aliasSecret,
			fixture.key,
			rotatedPlaintext,
		)
	})

	t.Run("separate worker processes fence an expired pre-dispatch owner", func(t *testing.T) {
		fixture := newCredentialFixture(t, pools)
		canary := []byte("fence-old-" + credentialRandomHex(t, 8))
		_, binding := fixture.storeCredential(t, canary, "process-fence-base")
		directory := t.TempDir()
		firstClaimPath := filepath.Join(directory, "first-claim.json")
		firstReadyPath := filepath.Join(directory, "first-ready")
		firstReleasePath := filepath.Join(directory, "first-release")
		firstRequest := fixture.refreshRequest(binding, "lease-process-fence-one", "process-fence-one", 250*time.Millisecond)
		first := startCredentialProcess(t, directory, "first-fence-owner", credentialProcessRequest{
			Action:      credentialProcessClaimThenAuthorize,
			Refresh:     firstRequest,
			ReadyPath:   firstReadyPath,
			ClaimPath:   firstClaimPath,
			ReleasePath: firstReleasePath,
		})
		waitForCredentialProcessPath(t, firstReadyPath, 5*time.Second)
		firstClaim := readCredentialProcessReceipt(t, firstClaimPath)
		if firstClaim.Outcome != "refresh_claimed" || firstClaim.Fence == 0 {
			t.Fatalf("first process claim receipt = %+v", firstClaim)
		}

		time.Sleep(750 * time.Millisecond)
		secondRequest := fixture.refreshRequest(binding, "lease-process-fence-two", "process-fence-two", 5*time.Second)
		secondRequest.ExpectedFence = firstClaim.Fence
		second := startCredentialProcess(t, directory, "second-fence-owner", credentialProcessRequest{
			Action:  credentialProcessReclaim,
			Refresh: secondRequest,
		})
		secondReceipt := second.wait(t)
		if secondReceipt.Outcome != "refresh_reclaimed" ||
			secondReceipt.Fence != firstClaim.Fence+1 {
			t.Fatalf("second process reclaim receipt = %+v", secondReceipt)
		}
		if secondReceipt.ProcessID == firstClaim.ProcessID ||
			secondReceipt.ProcessID == os.Getpid() ||
			firstClaim.ProcessID == os.Getpid() {
			t.Fatalf("fence owners were not distinct OS processes: first=%d second=%d parent=%d", firstClaim.ProcessID, secondReceipt.ProcessID, os.Getpid())
		}

		writeCredentialProcessSignal(t, firstReleasePath)
		firstReceipt := first.wait(t)
		if firstReceipt.Outcome != "stale_owner_fenced" ||
			firstReceipt.Fence != firstClaim.Fence ||
			firstReceipt.ProcessID != firstClaim.ProcessID {
			t.Fatalf("stale first process receipt = %+v", firstReceipt)
		}
		assertCredentialProcessArtifactsSecretFree(
			t,
			[]string{
				first.requestPath,
				firstClaimPath,
				first.receiptPath,
				firstReadyPath,
				firstReleasePath,
				second.requestPath,
				second.receiptPath,
			},
			[][]byte{first.output.Bytes(), second.output.Bytes(), first.launchProjection(), second.launchProjection()},
			canary,
			fixture.key,
		)
	})

	t.Run("account witness changed before refresh performs no provider call", func(t *testing.T) {
		fixture := newCredentialFixture(t, pools)
		_, binding := fixture.storeCredential(t, []byte("stale-account-old"), "stale-account-base")
		fixture.mutateAccountWitness(t, "changed-witness-"+credentialRandomHex(t, 8))
		resolver := &countingKeyResolver{key: fixture.key}
		authorization := &countingAuthorization{}
		var providerCalls atomic.Int64
		_, err := RefreshCAS(
			context.Background(), pools.first, resolver.resolve, authorization.authorize,
			func(context.Context, CredentialMaterial) RefreshAttempt {
				providerCalls.Add(1)
				return RefreshAttempt{Disposition: RefreshSucceeded, Credential: []byte("must-not-run")}
			},
			fixture.refreshRequest(binding, "lease-stale-account", "owner-stale-account", time.Second),
		)
		if !errors.Is(err, ErrCredentialStale) {
			t.Fatalf("stale account refresh error = %v; want ErrCredentialStale", err)
		}
		if providerCalls.Load() != 0 ||
			resolver.calls.Load() != 0 ||
			authorization.calls.Load() != 0 {
			t.Fatalf(
				"stale account provider/key/authorization calls = %d/%d/%d",
				providerCalls.Load(),
				resolver.calls.Load(),
				authorization.calls.Load(),
			)
		}
	})

	t.Run("account witness changed during refresh cannot persist rotated ciphertext", func(t *testing.T) {
		fixture := newCredentialFixture(t, pools)
		original := []byte("mid-refresh-old-" + credentialRandomHex(t, 8))
		_, binding := fixture.storeCredential(t, original, "mid-refresh-base")
		resolver := &countingKeyResolver{key: fixture.key}
		authorization := &countingAuthorization{}
		entered := make(chan struct{})
		release := make(chan struct{})
		result := make(chan error, 1)
		go func() {
			_, err := RefreshCAS(
				context.Background(), pools.first, resolver.resolve, authorization.authorize,
				func(_ context.Context, material CredentialMaterial) RefreshAttempt {
					if useErr := material.Use(func(secret []byte) error {
						if !bytes.Equal(secret, original) {
							return errors.New("wrong mid-refresh credential")
						}
						return nil
					}); useErr != nil {
						return RefreshAttempt{Disposition: RefreshOutcomeUnknown}
					}
					close(entered)
					<-release
					return RefreshAttempt{Disposition: RefreshSucceeded, Credential: []byte("mid-refresh-new")}
				},
				fixture.refreshRequest(binding, "lease-mid-account", "owner-mid-account", 10*time.Second),
			)
			result <- err
		}()
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("refresh did not reach provider before account witness change")
		}
		fixture.mutateAccountWitness(t, "mid-refresh-changed-"+credentialRandomHex(t, 8))
		close(release)
		select {
		case err := <-result:
			if !errors.Is(err, ErrRefreshOutcomeUncertain) {
				t.Fatalf("mid-refresh account change error = %v; want ErrRefreshOutcomeUncertain", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("refresh did not return after account witness change")
		}
		if authorization.calls.Load() != 1 || resolver.calls.Load() != 1 {
			t.Fatalf(
				"mid-refresh account invalidation authorization/key calls = %d/%d; want 1/1",
				authorization.calls.Load(),
				resolver.calls.Load(),
			)
		}
		lifecycle, err := fixture.loadLifecycle(t)
		if err != nil {
			t.Fatalf("load lifecycle after account witness change: %v", err)
		}
		if lifecycle.CiphertextVersion != 1 ||
			lifecycle.Status != store.CredentialLifecycleReauthorizationRequired {
			t.Fatalf("changed account witness did not force reauthorization: %+v", lifecycle)
		}
		fixture.requireEncryptedNotFound(t, 2)
	})

	t.Run("rotating response loss requires reauthorization independently", func(t *testing.T) {
		fixture := newCredentialFixture(t, pools)
		oldCanary := []byte("lost-response-old-" + credentialRandomHex(t, 8))
		_, binding := fixture.storeCredential(t, oldCanary, "lost-response-base")
		resolver := &countingKeyResolver{key: fixture.key}
		authorization := &countingAuthorization{}
		errorCanary := "lost-response-error-" + credentialRandomHex(t, 8)
		status, err := RefreshCAS(
			context.Background(), pools.first, resolver.resolve, authorization.authorize,
			func(_ context.Context, material CredentialMaterial) RefreshAttempt {
				if useErr := material.Use(func(secret []byte) error {
					if !bytes.Equal(secret, oldCanary) {
						return errors.New("wrong rotating input")
					}
					return nil
				}); useErr != nil {
					return RefreshAttempt{Disposition: RefreshOutcomeUnknown}
				}
				return RefreshAttempt{
					Disposition: RefreshOutcomeUnknown,
					Credential:  []byte("untrusted-lost-rotated-value"),
					Diagnostic:  Diagnostic{Message: errorCanary, Body: []byte(errorCanary), Err: errors.New(errorCanary)},
				}
			},
			fixture.refreshRequest(binding, "lease-lost-response", "owner-lost-response", time.Second),
		)
		if !errors.Is(err, ErrRefreshOutcomeUncertain) || strings.Contains(err.Error(), errorCanary) {
			t.Fatalf("lost rotating response error = %v", err)
		}
		if status.Status != store.CredentialLifecycleReauthorizationRequired || status.CiphertextVersion != 1 {
			t.Fatalf("lost rotating response status = %+v", status)
		}
		fixture.requireEncryptedNotFound(t, 2)

		postLossResolver := &countingKeyResolver{key: fixture.key}
		_, resolveErr := ResolveForEffect(
			context.Background(), pools.first, postLossResolver.resolve, authorization.authorize,
			ResolveRequest{Scope: fixture.scope, Actor: fixture.actor, Binding: binding, Effect: fixture.effect()},
		)
		if !errors.Is(resolveErr, ErrReauthorizationRequired) || postLossResolver.calls.Load() != 0 {
			t.Fatalf("post-loss resolution error/key calls = %v/%d", resolveErr, postLossResolver.calls.Load())
		}

		retryResolver := &countingKeyResolver{key: fixture.key}
		retryAuthorization := &countingAuthorization{}
		var retryProviderCalls atomic.Int64
		retryRequest := fixture.refreshRequest(binding, "lease-lost-response-retry", "owner-lost-response-retry", time.Second)
		retryRequest.Effect.DispatchID = "dispatch-lost-response-retry"
		retryRequest.Effect.AttemptID = "attempt-lost-response-retry"
		_, retryErr := RefreshCAS(
			context.Background(),
			pools.first,
			retryResolver.resolve,
			retryAuthorization.authorize,
			func(context.Context, CredentialMaterial) RefreshAttempt {
				retryProviderCalls.Add(1)
				return RefreshAttempt{Disposition: RefreshSucceeded, Credential: []byte("must-not-retry")}
			},
			retryRequest,
		)
		if !errors.Is(retryErr, ErrReauthorizationRequired) ||
			retryAuthorization.calls.Load() != 0 ||
			retryResolver.calls.Load() != 0 ||
			retryProviderCalls.Load() != 0 {
			t.Fatalf(
				"blind refresh retry was not blocked before authorization/key/provider: %T calls=%d/%d/%d",
				retryErr,
				retryAuthorization.calls.Load(),
				retryResolver.calls.Load(),
				retryProviderCalls.Load(),
			)
		}

		replacementCanary := []byte("reauthorized-credential-" + credentialRandomHex(t, 8))
		_, replacementBinding := fixture.storeCredential(t, replacementCanary, "lost-response-reauthorization")
		if replacementBinding.CiphertextVersion != binding.CiphertextVersion+1 {
			t.Fatalf("reauthorization ciphertext version = %d; want %d", replacementBinding.CiphertextVersion, binding.CiphertextVersion+1)
		}
		recoveryResolver := &countingKeyResolver{key: fixture.key}
		recoveryAuthorization := &countingAuthorization{}
		var recoveryProviderCalls atomic.Int64
		recoveryRequest := fixture.refreshRequest(replacementBinding, "lease-after-reauthorization", "owner-after-reauthorization", 5*time.Second)
		recoveryRequest.Effect.DispatchID = "dispatch-after-reauthorization"
		recoveryRequest.Effect.AttemptID = "attempt-after-reauthorization"
		recoveryStatus, recoveryErr := RefreshCAS(
			context.Background(),
			pools.first,
			recoveryResolver.resolve,
			recoveryAuthorization.authorize,
			func(_ context.Context, material CredentialMaterial) RefreshAttempt {
				recoveryProviderCalls.Add(1)
				if useErr := material.Use(func(secret []byte) error {
					if !bytes.Equal(secret, replacementCanary) {
						return errors.New("wrong reauthorized refresh input")
					}
					return nil
				}); useErr != nil {
					return RefreshAttempt{Disposition: RefreshOutcomeUnknown}
				}
				return RefreshAttempt{Disposition: RefreshRejected}
			},
			recoveryRequest,
		)
		if !errors.Is(recoveryErr, ErrRefreshRejected) ||
			recoveryStatus.Status != store.CredentialLifecycleActive ||
			recoveryStatus.CiphertextVersion != replacementBinding.CiphertextVersion ||
			recoveryProviderCalls.Load() != 1 ||
			recoveryResolver.calls.Load() != 1 ||
			recoveryAuthorization.calls.Load() != 2 {
			t.Fatalf(
				"explicit reauthorization did not restore refresh eligibility: %T status=%+v calls=%d/%d/%d",
				recoveryErr,
				recoveryStatus,
				recoveryProviderCalls.Load(),
				recoveryResolver.calls.Load(),
				recoveryAuthorization.calls.Load(),
			)
		}
	})

	t.Run("lease expiry before persistence refuses stale ciphertext overwrite", func(t *testing.T) {
		fixture := newCredentialFixture(t, pools)
		_, binding := fixture.storeCredential(t, []byte("expired-lease-old"), "expired-lease-base")
		resolver := &countingKeyResolver{key: fixture.key}
		authorization := &countingAuthorization{}
		status, err := RefreshCAS(
			context.Background(), pools.first, resolver.resolve, authorization.authorize,
			func(ctx context.Context, material CredentialMaterial) RefreshAttempt {
				if useErr := material.Use(func([]byte) error { return nil }); useErr != nil {
					return RefreshAttempt{Disposition: RefreshOutcomeUnknown}
				}
				<-ctx.Done()
				return RefreshAttempt{Disposition: RefreshSucceeded, Credential: []byte("expired-lease-new")}
			},
			fixture.refreshRequest(binding, "lease-expired", "owner-expired", 250*time.Millisecond),
		)
		if !errors.Is(err, ErrRefreshLeaseStale) {
			t.Fatalf("expired refresh lease error = %v; want ErrRefreshLeaseStale", err)
		}
		if status.Status != store.CredentialLifecycleReauthorizationRequired ||
			status.CiphertextVersion != binding.CiphertextVersion {
			t.Fatalf("expired refresh lease status = %+v", status)
		}
		lifecycle, lifecycleErr := fixture.loadLifecycle(t)
		if lifecycleErr != nil {
			t.Fatalf("load lifecycle after expired lease: %v", lifecycleErr)
		}
		if lifecycle.CiphertextVersion != 1 ||
			lifecycle.Status != store.CredentialLifecycleReauthorizationRequired {
			t.Fatalf("expired owner did not preserve version and force reauthorization: %+v", lifecycle)
		}
		fixture.requireEncryptedNotFound(t, 2)
	})
}

const credentialProcessRequestEnvironment = "PM_CREDENTIAL_PROCESS_REQUEST"

type credentialProcessAction string

const (
	credentialProcessRefreshOwner       credentialProcessAction = "refresh_owner"
	credentialProcessRefreshContender   credentialProcessAction = "refresh_contender"
	credentialProcessClaimThenAuthorize credentialProcessAction = "claim_then_authorize"
	credentialProcessReclaim            credentialProcessAction = "reclaim"
)

type credentialProcessRequest struct {
	Action       credentialProcessAction `json:"action"`
	Refresh      RefreshRequest          `json:"refresh"`
	ReadyPath    string                  `json:"ready_path,omitempty"`
	ReleasePath  string                  `json:"release_path,omitempty"`
	ProviderPath string                  `json:"provider_path,omitempty"`
	ClaimPath    string                  `json:"claim_path,omitempty"`
	ReceiptPath  string                  `json:"receipt_path"`
}

type credentialProcessReceipt struct {
	Outcome            string            `json:"outcome"`
	ProcessID          int               `json:"process_id"`
	ProviderCalls      int64             `json:"provider_calls"`
	KeyCalls           int64             `json:"key_calls"`
	AuthorizationCalls int64             `json:"authorization_calls"`
	Fence              uint64            `json:"fence,omitempty"`
	Status             *CredentialStatus `json:"status,omitempty"`
}

type credentialProcessHandle struct {
	command     *exec.Cmd
	cancel      context.CancelFunc
	output      bytes.Buffer
	requestPath string
	receiptPath string
	waited      bool
}

func startCredentialProcess(t *testing.T, directory, name string, request credentialProcessRequest) *credentialProcessHandle {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate credential test executable: %T", err)
	}
	requestPath := filepath.Join(directory, name+"-request.json")
	request.ReceiptPath = filepath.Join(directory, name+"-receipt.json")
	writeCredentialProcessJSON(t, requestPath, request)
	runtimeSecretsDirectory := stageCredentialProcessRuntimeSecrets(t, directory, name)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	command := exec.CommandContext(ctx, executable, "-test.run=^TestScopedRefreshLeaseAndRedaction$", "-test.count=1")
	command.Env = []string{
		"LANG=C",
		"TZ=UTC",
		"PM_STORE_TEST_SECRETS_DIR=" + runtimeSecretsDirectory,
		credentialProcessRequestEnvironment + "=" + requestPath,
	}
	handle := &credentialProcessHandle{
		command:     command,
		cancel:      cancel,
		requestPath: requestPath,
		receiptPath: request.ReceiptPath,
	}
	command.Stdout = &handle.output
	command.Stderr = &handle.output
	if err := command.Start(); err != nil {
		cancel()
		t.Fatalf("start credential worker process: %T", err)
	}
	t.Cleanup(func() {
		cancel()
		if !handle.waited {
			_ = command.Wait()
			handle.waited = true
		}
	})
	return handle
}

func (process *credentialProcessHandle) wait(t *testing.T) credentialProcessReceipt {
	t.Helper()
	if process.waited {
		t.Fatal("credential worker process was already awaited")
	}
	err := process.command.Wait()
	process.waited = true
	process.cancel()
	if err != nil {
		t.Fatalf("credential worker process exited unsuccessfully: %T", err)
	}
	return readCredentialProcessReceipt(t, process.receiptPath)
}

func (process *credentialProcessHandle) launchProjection() []byte {
	values := append(slices.Clone(process.command.Args), process.command.Env...)
	return []byte(strings.Join(values, "\x00"))
}

func stageCredentialProcessRuntimeSecrets(t *testing.T, directory, name string) string {
	t.Helper()
	endpoint, err := os.ReadFile(filepath.Join(credentialTestSecretsDirectory(t), "pm-app-endpoint"))
	if err != nil {
		t.Fatal("read credential process runtime endpoint")
	}
	runtimeSecretsDirectory := filepath.Join(directory, name+"-runtime-secrets")
	if err := os.Mkdir(runtimeSecretsDirectory, 0o700); err != nil {
		t.Fatalf("create credential process runtime secret directory: %T", err)
	}
	if err := os.WriteFile(filepath.Join(runtimeSecretsDirectory, "pm-app-endpoint"), endpoint, 0o600); err != nil {
		t.Fatalf("write credential process runtime endpoint: %T", err)
	}
	return runtimeSecretsDirectory
}

func runCredentialProcessWorker(t *testing.T, requestPath string) {
	t.Helper()
	var request credentialProcessRequest
	readCredentialProcessJSON(t, requestPath, &request)
	runtimeDSN := loadCredentialTestRuntimeDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := store.OpenPool(ctx, store.PoolConfig{
		ConnectionString:  runtimeDSN,
		MaxConns:          1,
		MinConns:          0,
		MaxConnLifetime:   5 * time.Minute,
		MaxConnIdleTime:   time.Minute,
		HealthCheckPeriod: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("open credential worker pool: %T", err)
	}
	defer pool.Close()

	resolver := &countingKeyResolver{key: bytes.Repeat([]byte{0x5a}, wrappingKeyBytes)}
	authorization := &countingAuthorization{}
	receipt := credentialProcessReceipt{ProcessID: os.Getpid()}
	switch request.Action {
	case credentialProcessRefreshOwner:
		var providerCalls atomic.Int64
		status, err := RefreshCAS(
			ctx,
			pool,
			resolver.resolve,
			authorization.authorize,
			func(_ context.Context, material CredentialMaterial) RefreshAttempt {
				providerCalls.Add(1)
				if err := material.Use(func(secret []byte) error {
					if len(secret) == 0 {
						return errors.New("empty provider credential")
					}
					return nil
				}); err != nil {
					return RefreshAttempt{Disposition: RefreshOutcomeUnknown}
				}
				writeCredentialProcessSignal(t, request.ProviderPath)
				writeCredentialProcessSignal(t, request.ReadyPath)
				waitForCredentialProcessPath(t, request.ReleasePath, 10*time.Second)
				return RefreshAttempt{
					Disposition: RefreshSucceeded,
					Credential:  []byte("separate-process-rotated-" + credentialRandomHex(t, 16)),
				}
			},
			request.Refresh,
		)
		if err != nil {
			t.Fatalf("owner credential refresh failed: %T", err)
		}
		receipt.Outcome = "refresh_completed"
		receipt.ProviderCalls = providerCalls.Load()
		receipt.KeyCalls = resolver.calls.Load()
		receipt.AuthorizationCalls = authorization.calls.Load()
		receipt.Status = &status
	case credentialProcessRefreshContender:
		var providerCalls atomic.Int64
		_, err := RefreshCAS(
			ctx,
			pool,
			resolver.resolve,
			authorization.authorize,
			func(context.Context, CredentialMaterial) RefreshAttempt {
				providerCalls.Add(1)
				writeCredentialProcessSignal(t, request.ProviderPath)
				return RefreshAttempt{
					Disposition: RefreshSucceeded,
					Credential:  []byte("must-not-run"),
				}
			},
			request.Refresh,
		)
		receipt.Outcome = credentialProcessRefreshRefusal(err)
		if receipt.Outcome == "" {
			t.Fatalf("contending credential refresh was not safely refused: %T", err)
		}
		receipt.ProviderCalls = providerCalls.Load()
		receipt.KeyCalls = resolver.calls.Load()
		receipt.AuthorizationCalls = authorization.calls.Load()
		if receipt.ProviderCalls != 0 {
			t.Fatal("contending credential process performed provider I/O")
		}
	case credentialProcessClaimThenAuthorize:
		lease, err := claimCredentialProcessLease(ctx, pool, request.Refresh)
		if err != nil {
			t.Fatalf("claim first credential process lease: %T", err)
		}
		writeCredentialProcessJSON(t, request.ClaimPath, credentialProcessReceipt{
			Outcome:   "refresh_claimed",
			ProcessID: os.Getpid(),
			Fence:     lease.Fence,
		})
		writeCredentialProcessSignal(t, request.ReadyPath)
		waitForCredentialProcessPath(t, request.ReleasePath, 10*time.Second)
		err = store.WithinScope(ctx, pool, request.Refresh.Scope, request.Refresh.Actor, func(ctx context.Context, tx *store.Transaction) error {
			_, _, err := store.AuthorizeCredentialRefresh(ctx, tx, lease)
			return err
		})
		if !errors.Is(err, store.ErrStaleFence) {
			t.Fatalf("expired credential owner was not fenced: %T", err)
		}
		receipt.Outcome = "stale_owner_fenced"
		receipt.Fence = lease.Fence
	case credentialProcessReclaim:
		lease, err := claimCredentialProcessLease(ctx, pool, request.Refresh)
		if err != nil {
			t.Fatalf("reclaim credential process lease: %T", err)
		}
		receipt.Outcome = "refresh_reclaimed"
		receipt.Fence = lease.Fence
	default:
		t.Fatal("credential worker action is unsupported")
	}
	writeCredentialProcessJSON(t, request.ReceiptPath, receipt)
}

func claimCredentialProcessLease(ctx context.Context, pool *store.Pool, request RefreshRequest) (store.CredentialRefreshLease, error) {
	var lease store.CredentialRefreshLease
	err := store.WithinScope(ctx, pool, request.Scope, request.Actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		lease, err = store.ClaimCredentialRefresh(ctx, tx, store.CredentialRefreshClaimRequest{
			LeaseID:                   request.LeaseID,
			ConnectionID:              request.Binding.ConnectionID,
			OwnerID:                   request.OwnerID,
			ExpectedFence:             request.ExpectedFence,
			ExpectedCiphertextVersion: request.Binding.CiphertextVersion,
			ExpectedAccountWitness:    request.Binding.ProviderAccountWitness,
			TTL:                       request.LeaseTTL,
		})
		return err
	})
	return lease, err
}

func credentialProcessRefreshRefusal(err error) string {
	switch {
	case errors.Is(err, ErrCredentialRefreshInProgress):
		return "refresh_in_progress"
	case errors.Is(err, ErrRefreshBusy):
		return "refresh_busy"
	case errors.Is(err, ErrRefreshOutcomeUncertain):
		return "refresh_outcome_uncertain"
	default:
		return ""
	}
}

func writeCredentialProcessJSON(t *testing.T, path string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode credential process record: %T", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("create credential process record: %T", err)
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		t.Fatalf("write credential process record: %T", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close credential process record: %T", err)
	}
}

func readCredentialProcessJSON(t *testing.T, path string, target any) {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read credential process record: %T", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		t.Fatalf("decode credential process record: %T", err)
	}
}

func readCredentialProcessReceipt(t *testing.T, path string) credentialProcessReceipt {
	t.Helper()
	var receipt credentialProcessReceipt
	readCredentialProcessJSON(t, path, &receipt)
	return receipt
}

func writeCredentialProcessSignal(t *testing.T, path string) {
	t.Helper()
	if path == "" {
		t.Fatal("credential process signal path is required")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("create credential process signal: %T", err)
	}
	if _, err := file.WriteString("ready\n"); err != nil {
		_ = file.Close()
		t.Fatalf("write credential process signal: %T", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close credential process signal: %T", err)
	}
}

func waitForCredentialProcessPath(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("observe credential process path: %T", err)
		}
		if !time.Now().Before(deadline) {
			t.Fatal("credential process path was not produced before the deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertCredentialProcessArtifactsSecretFree(t *testing.T, paths []string, outputs [][]byte, secrets ...[]byte) {
	t.Helper()
	for _, path := range paths {
		encoded, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read credential process artifact: %T", err)
		}
		outputs = append(outputs, encoded)
	}
	for _, output := range outputs {
		for _, secret := range secrets {
			if projectionContainsSecret(output, secret) {
				t.Fatal("credential process argument, receipt, or diagnostic exposed secret material")
			}
		}
	}
}
