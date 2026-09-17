package credentials

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

type RefreshDisposition string

const (
	RefreshSucceeded      RefreshDisposition = "succeeded"
	RefreshRejected       RefreshDisposition = "rejected"
	RefreshOutcomeUnknown RefreshDisposition = "outcome_unknown"
)

// RefreshAttempt is the complete result of one bounded provider refresh call.
// Credential is required only for RefreshSucceeded. Returning it transfers
// ownership to RefreshCAS, which wipes it. Diagnostic is unsafe input and is
// never returned or durably recorded by this package.
type RefreshAttempt struct {
	Disposition RefreshDisposition `json:"disposition"`
	Credential  []byte             `json:"-"`
	Diagnostic  Diagnostic         `json:"-"`
}

func (attempt RefreshAttempt) String() string {
	return "credential refresh attempt [REDACTED]"
}

func (attempt RefreshAttempt) GoString() string {
	return attempt.String()
}

// RefreshOperation performs exactly one provider-specific refresh under the
// admitted capability. It must honor context cancellation/deadline and must not
// retry, switch providers, or expose a generic transport escape.
type RefreshOperation func(context.Context, CredentialMaterial) RefreshAttempt

type RefreshRequest struct {
	Scope         contract.Scope
	Actor         contract.Actor
	Binding       contract.CredentialRef
	Effect        EffectAccess
	LeaseID       string
	OwnerID       string
	ExpectedFence uint64
	LeaseTTL      time.Duration
}

func (request RefreshRequest) validate(now time.Time) error {
	resolve := ResolveRequest{
		Scope:   request.Scope,
		Actor:   request.Actor,
		Binding: request.Binding,
		Effect:  request.Effect,
	}
	if err := resolve.validate(now); err != nil {
		return err
	}
	if request.Effect.ReconciliationOnly {
		return errors.New("reconciliation-only authority cannot refresh credentials")
	}
	if !validOpaque(request.LeaseID) || !validOpaque(request.OwnerID) {
		return errors.New("credential refresh lease or owner identity is invalid")
	}
	if request.LeaseTTL.Microseconds() <= 0 {
		return errors.New("credential refresh lease TTL must be at least one microsecond")
	}
	return nil
}

// RefreshCAS coordinates aliases and replicas through a PostgreSQL
// provider-account lease. It commits the dispatched fence before one remote
// call, then requires current authorization, account witness, version and live
// fence again before ciphertext persistence. A possibly rotating lost response
// is retained as reauthorization-required; it is never retried here.
func RefreshCAS(
	ctx context.Context,
	pool *store.Pool,
	resolveKey WrappingKeyResolver,
	authorize AuthorizeFunc,
	refresh RefreshOperation,
	request RefreshRequest,
) (CredentialStatus, error) {
	if ctx == nil {
		return CredentialStatus{}, errors.New("credential refresh context is required")
	}
	if pool == nil {
		return CredentialStatus{}, errors.New("credential refresh pool is required")
	}
	if resolveKey == nil {
		return CredentialStatus{}, errors.New("credential wrapping-key resolver is required")
	}
	if authorize == nil {
		return CredentialStatus{}, errors.New("credential authorization callback is required")
	}
	if refresh == nil {
		return CredentialStatus{}, errors.New("credential refresh operation is required")
	}
	if err := request.validate(time.Now()); err != nil {
		return CredentialStatus{}, err
	}

	var material *credentialMaterial
	var dispatched store.CredentialRefreshLease
	err := store.WithinScope(ctx, pool, request.Scope, request.Actor, func(ctx context.Context, tx *store.Transaction) error {
		locked, err := store.LockCredentialLifecycle(ctx, tx, request.Binding.ConnectionID)
		if err != nil {
			return err
		}
		if err := currentBinding(locked, request.Binding); err != nil {
			return err
		}
		effect := request.Effect
		if err := callAuthorize(ctx, tx, authorize, accessForLock(
			ActionRefresh,
			StagePreEffect,
			request.Scope,
			request.Actor,
			locked,
			request.Binding.Scopes,
			&effect,
		)); err != nil {
			return err
		}

		claimed, err := store.ClaimCredentialRefresh(ctx, tx, store.CredentialRefreshClaimRequest{
			LeaseID:                   request.LeaseID,
			ConnectionID:              request.Binding.ConnectionID,
			OwnerID:                   request.OwnerID,
			ExpectedFence:             request.ExpectedFence,
			ExpectedCiphertextVersion: request.Binding.CiphertextVersion,
			ExpectedAccountWitness:    request.Binding.ProviderAccountWitness,
			TTL:                       request.LeaseTTL,
		})
		if err != nil {
			return err
		}
		_, dispatched, err = store.AuthorizeCredentialRefresh(ctx, tx, claimed)
		if err != nil {
			return err
		}

		encrypted, err := store.LoadEncryptedSecret(ctx, tx, request.Binding.CredentialRef, request.Binding.CiphertextVersion)
		if err != nil {
			return err
		}
		if encrypted.ProviderAccountID != locked.Account.AccountID ||
			!sameCredentialBinding(encrypted.Binding, request.Binding) {
			return ErrCredentialStale
		}
		key, err := resolveWrappingKey(ctx, resolveKey, encrypted.Binding.KeyRef)
		if err != nil {
			return err
		}
		defer wipe(key)
		plaintext, err := openCredential(key, encrypted)
		if err != nil {
			return err
		}
		deadline := request.Effect.Deadline
		if dispatched.ExpiresAt.Before(deadline) {
			deadline = dispatched.ExpiresAt
		}
		if !time.Now().Before(deadline) {
			wipe(plaintext)
			return ErrEffectExpired
		}
		material = newCredentialMaterial(encrypted.Binding, plaintext, deadline)
		return nil
	})
	if err != nil {
		if material != nil {
			material.Destroy()
		}
		return CredentialStatus{}, mapPreRefreshError(err)
	}

	defer material.Destroy()
	operationDeadline := request.Effect.Deadline
	if dispatched.ExpiresAt.Before(operationDeadline) {
		operationDeadline = dispatched.ExpiresAt
	}
	operationContext, cancel := context.WithDeadline(ctx, operationDeadline)
	if operationErr := operationContext.Err(); operationErr != nil {
		cancel()
		material.Destroy()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return CredentialStatus{}, ctxErr
		}
		status, cleanupErr := persistRefreshFailure(ctx, pool, authorize, request, dispatched)
		if cleanupErr != nil && !errors.Is(cleanupErr, ErrRefreshRejected) {
			return CredentialStatus{}, cleanupErr
		}
		return status, ErrRefreshLeaseStale
	}
	attempt := refresh(operationContext, material)
	cancel()
	material.Destroy()
	rotated := attempt.Credential
	defer wipe(rotated)

	switch attempt.Disposition {
	case RefreshSucceeded:
		if len(rotated) == 0 || len(rotated) > maxCredentialBytes {
			return markRefreshUncertain(ctx, pool, request, dispatched)
		}
		return persistRefreshSuccess(ctx, pool, resolveKey, authorize, request, dispatched, rotated)
	case RefreshRejected:
		return persistRefreshFailure(ctx, pool, authorize, request, dispatched)
	case RefreshOutcomeUnknown:
		return markRefreshUncertain(ctx, pool, request, dispatched)
	default:
		return markRefreshUncertain(ctx, pool, request, dispatched)
	}
}

func persistRefreshSuccess(
	ctx context.Context,
	pool *store.Pool,
	resolveKey WrappingKeyResolver,
	authorize AuthorizeFunc,
	request RefreshRequest,
	lease store.CredentialRefreshLease,
	rotated []byte,
) (CredentialStatus, error) {
	var result CredentialStatus
	postCommitError := error(nil)
	err := store.WithinScope(ctx, pool, request.Scope, request.Actor, func(ctx context.Context, tx *store.Transaction) error {
		locked, err := store.LockCredentialLifecycle(ctx, tx, request.Binding.ConnectionID)
		if err != nil {
			return err
		}
		if err := refreshingBinding(locked, request.Binding); err != nil {
			return err
		}
		effect := request.Effect
		if err := callAuthorize(ctx, tx, authorize, accessForLock(
			ActionRefresh,
			StagePersist,
			request.Scope,
			request.Actor,
			locked,
			request.Binding.Scopes,
			&effect,
		)); err != nil {
			return err
		}

		key, err := resolveWrappingKey(ctx, resolveKey, request.Binding.KeyRef)
		if err != nil {
			lifecycle, _, markErr := store.MarkCredentialRefreshUncertain(ctx, tx, lease)
			if markErr != nil {
				return markErr
			}
			result = statusFromLifecycle(lifecycle, false)
			postCommitError = ErrRefreshOutcomeUncertain
			return nil
		}
		defer wipe(key)
		if request.Binding.CiphertextVersion >= maxDatabaseBigint {
			return ErrRefreshOutcomeUncertain
		}
		next := request.Binding
		next.CiphertextVersion++
		next.Scopes = slices.Clone(request.Binding.Scopes)
		encrypted := store.EncryptedSecret{
			Scope:             request.Scope,
			Binding:           next,
			ProviderAccountID: locked.Account.AccountID,
		}
		encrypted.Ciphertext, err = sealCredential(key, encrypted, rotated)
		if err != nil {
			lifecycle, _, markErr := store.MarkCredentialRefreshUncertain(ctx, tx, lease)
			if markErr != nil {
				return markErr
			}
			result = statusFromLifecycle(lifecycle, false)
			postCommitError = ErrRefreshOutcomeUncertain
			return nil
		}
		if err := store.RegisterEncryptedSecret(ctx, tx, encrypted); err != nil {
			return err
		}
		lifecycle, _, err := store.CompleteCredentialRefresh(ctx, tx, lease, next)
		if err != nil {
			return err
		}
		result = statusFromLifecycle(lifecycle, false)
		return nil
	})
	if err != nil {
		mapped := mapPostRefreshError(err)
		status, markErr := markRefreshUncertain(ctx, pool, request, lease)
		if errors.Is(markErr, ErrRefreshOutcomeUncertain) {
			return status, mapped
		}
		return CredentialStatus{}, mapped
	}
	if postCommitError != nil {
		return result, postCommitError
	}
	return result, nil
}

func persistRefreshFailure(
	ctx context.Context,
	pool *store.Pool,
	authorize AuthorizeFunc,
	request RefreshRequest,
	lease store.CredentialRefreshLease,
) (CredentialStatus, error) {
	var result CredentialStatus
	err := store.WithinScope(ctx, pool, request.Scope, request.Actor, func(ctx context.Context, tx *store.Transaction) error {
		locked, err := store.LockCredentialLifecycle(ctx, tx, request.Binding.ConnectionID)
		if err != nil {
			return err
		}
		if err := refreshingBinding(locked, request.Binding); err != nil {
			return err
		}
		effect := request.Effect
		if err := callAuthorize(ctx, tx, authorize, accessForLock(
			ActionRefresh,
			StagePersist,
			request.Scope,
			request.Actor,
			locked,
			request.Binding.Scopes,
			&effect,
		)); err != nil {
			return err
		}
		lifecycle, _, err := store.FailCredentialRefresh(ctx, tx, lease)
		if err != nil {
			return err
		}
		result = statusFromLifecycle(lifecycle, false)
		return nil
	})
	if err != nil {
		mapped := mapPostRefreshError(err)
		status, markErr := markRefreshUncertain(ctx, pool, request, lease)
		if errors.Is(markErr, ErrRefreshOutcomeUncertain) {
			return status, mapped
		}
		return CredentialStatus{}, mapped
	}
	return result, ErrRefreshRejected
}

// A dispatched provider call has already crossed the effect boundary.
// Recording uncertainty only narrows authority, so it remains descriptor- and
// fence-bound but does not depend on authority that may have been revoked while
// the provider operation was in flight.
func markRefreshUncertain(
	ctx context.Context,
	pool *store.Pool,
	request RefreshRequest,
	lease store.CredentialRefreshLease,
) (CredentialStatus, error) {
	var result CredentialStatus
	err := store.WithinScope(ctx, pool, request.Scope, request.Actor, func(ctx context.Context, tx *store.Transaction) error {
		locked, err := store.LockCredentialLifecycle(ctx, tx, request.Binding.ConnectionID)
		if err != nil {
			return err
		}
		if locked.Lifecycle == nil ||
			locked.Lifecycle.Status != store.CredentialLifecycleRefreshing ||
			!locked.Lifecycle.Scope.Equal(request.Scope) ||
			locked.Lifecycle.ConnectionID != request.Binding.ConnectionID ||
			locked.Lifecycle.CredentialRef != request.Binding.CredentialRef ||
			locked.Lifecycle.CiphertextVersion != request.Binding.CiphertextVersion ||
			locked.Lifecycle.AuthEpoch != request.Binding.AuthEpoch {
			return ErrCredentialStale
		}
		lifecycle, _, err := store.MarkCredentialRefreshUncertain(ctx, tx, lease)
		if err != nil {
			return err
		}
		result = statusFromLifecycle(lifecycle, false)
		return nil
	})
	if err != nil {
		return CredentialStatus{}, mapPostRefreshError(err)
	}
	return result, ErrRefreshOutcomeUncertain
}

func refreshingBinding(locked store.CredentialLifecycleLock, expected contract.CredentialRef) error {
	if locked.Lifecycle == nil || locked.Lifecycle.Status != store.CredentialLifecycleRefreshing {
		return ErrCredentialStale
	}
	if locked.ConnectionState != "active" ||
		locked.Connection.Scope.WorkspaceID != expected.WorkspaceID ||
		locked.Connection.ConnectionID != expected.ConnectionID ||
		locked.Connection.CredentialRef != expected.CredentialRef ||
		locked.Connection.ProviderAccountID != locked.Account.AccountID ||
		locked.Connection.Revision != locked.Lifecycle.ConnectionRevision ||
		locked.Account.AccountID != locked.Lifecycle.ProviderAccountID ||
		locked.Account.AccountWitness != expected.ProviderAccountWitness ||
		locked.Account.AccountWitness != locked.Lifecycle.AccountWitness ||
		locked.Account.Revision != locked.Lifecycle.AccountRevision ||
		locked.Lifecycle.CredentialRef != expected.CredentialRef ||
		locked.Lifecycle.CiphertextVersion != expected.CiphertextVersion ||
		locked.Lifecycle.AuthEpoch != expected.AuthEpoch {
		return ErrCredentialStale
	}
	return nil
}

func mapPreRefreshError(err error) error {
	switch {
	case errors.Is(err, store.ErrCredentialRefreshBusy):
		return ErrRefreshBusy
	case errors.Is(err, store.ErrCredentialRefreshUncertain):
		return ErrRefreshOutcomeUncertain
	case errors.Is(err, store.ErrStaleFence):
		return ErrRefreshLeaseStale
	case errors.Is(err, store.ErrCredentialLifecycleStale):
		return ErrCredentialStale
	default:
		return err
	}
}

func mapPostRefreshError(err error) error {
	switch {
	case errors.Is(err, ErrAuthorizationDenied):
		return ErrRefreshOutcomeUncertain
	case errors.Is(err, store.ErrStaleFence), errors.Is(err, store.ErrCredentialLifecycleStale):
		return ErrRefreshLeaseStale
	case errors.Is(err, ErrCredentialStale):
		return ErrRefreshOutcomeUncertain
	default:
		return ErrRefreshOutcomeUncertain
	}
}
