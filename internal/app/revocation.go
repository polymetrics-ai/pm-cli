package app

import (
	"context"
	"errors"

	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// RunGrantCheck identifies the accepted run whose immutable grant must be
// rechecked against current authority. It deliberately carries no replacement
// grant, actor, grant reference, or grant epoch.
type RunGrantCheck struct {
	RunID                string
	Permission           string
	ResourceKind         auth.ResourceKind
	ResourceID           string
	ConnectionID         string
	OperationID          string
	CurrentApprovalEpoch uint64
}

// RecheckRunGrant loads the accepted grant before checking current authority.
// Login-session state is intentionally absent: accepted work is governed by its
// independently revocable durable grant and current policy epochs.
func RecheckRunGrant(
	ctx context.Context,
	tx *store.Transaction,
	authorization auth.ResourceAuthorization,
	check RunGrantCheck,
) (contract.ExecutionGrant, error) {
	if ctx == nil || tx == nil || authorization == nil || !validApplicationID(check.RunID) {
		return contract.ExecutionGrant{}, invalidRequestFailure()
	}

	accepted, err := store.LoadRunExecutionGrant(ctx, tx, check.RunID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrScopeMismatch) {
			return contract.ExecutionGrant{}, resourceNotFoundFailure(err)
		}
		return contract.ExecutionGrant{}, err
	}
	if err := store.RequireAuthorizationContext(tx, accepted.Grant.Scope, accepted.Grant.Actor); err != nil {
		return contract.ExecutionGrant{}, resourceNotFoundFailure(err)
	}
	if accepted.RevokedAt != nil {
		return contract.ExecutionGrant{}, mapAuthorizationFailure(auth.ErrGrantUnavailable)
	}

	// ResourceRun names the same accepted run. ResourceEvent uses RunID as its
	// parent selector; the remaining resource kinds must not receive it because
	// auth treats a nonempty RunID as valid only for an event lookup.
	if check.ResourceKind == auth.ResourceRun && check.ResourceID != check.RunID {
		return contract.ExecutionGrant{}, resourceNotFoundFailure(auth.ErrResourceNotFound)
	}
	authorizationRunID := ""
	if check.ResourceKind == auth.ResourceEvent {
		authorizationRunID = check.RunID
	}

	if _, err := authorization.RecheckGrant(ctx, tx, auth.GrantCheck{
		Grant:                accepted.Grant,
		Permission:           check.Permission,
		ResourceKind:         check.ResourceKind,
		ResourceID:           check.ResourceID,
		RunID:                authorizationRunID,
		ConnectionID:         check.ConnectionID,
		OperationID:          check.OperationID,
		CurrentApprovalEpoch: check.CurrentApprovalEpoch,
	}); err != nil {
		return contract.ExecutionGrant{}, mapAuthorizationFailure(err)
	}
	return accepted.Grant, nil
}

// RevokeFutureEffects records explicit revocation of an accepted run grant
// without rewriting its immutable attribution. The caller owns current
// authorization and the audit/control transaction; public revocation
// orchestration is a later boundary.
func RevokeFutureEffects(
	ctx context.Context,
	tx *store.Transaction,
	runID string,
	expectedGrantEpoch uint64,
) (store.RunExecutionGrant, bool, error) {
	if ctx == nil || tx == nil || !validApplicationID(runID) || expectedGrantEpoch == 0 {
		return store.RunExecutionGrant{}, false, invalidRequestFailure()
	}
	revoked, changed, err := store.RevokeRunExecutionGrant(
		ctx,
		tx,
		runID,
		expectedGrantEpoch,
	)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrScopeMismatch) {
		return store.RunExecutionGrant{}, false, resourceNotFoundFailure(err)
	}
	return revoked, changed, err
}
