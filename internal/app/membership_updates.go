package app

import (
	"context"
	"errors"
	"slices"

	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

const membershipUpdateOperation = "membership.update"

// UpdateMembership adds connections to one membership at its current control
// revision. ConnectionIDs is strictly sorted and unique.
type UpdateMembership struct {
	MembershipID  string
	ConnectionIDs []string
	Control       MutationControl
}

// UpdateMembership adds active connections of the workspace to a membership.
// The current grants are checked inside the update transaction. The rule for
// the added connections is in authorizeMembershipConnections. A changed update
// writes one workspace audit record. An unchanged update writes no audit
// record and no revision.
func (service *IdentityService) UpdateMembership(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	request UpdateMembership,
) (MembershipMutationResult, error) {
	if service == nil || ctx == nil || !validApplicationID(request.MembershipID) {
		return MembershipMutationResult{}, invalidRequestFailure()
	}
	var result MembershipMutationResult
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		authorization, err := service.bindAndAuthorizeAccessMutation(ctx, tx, actor, scope)
		if err != nil {
			return err
		}
		intentRevision, err := validateMutationControl(request.Control)
		if err != nil {
			return err
		}
		if len(request.ConnectionIDs) == 0 || !validSortedIDs(request.ConnectionIDs) {
			return invalidRequestFailure()
		}
		target, err := store.LoadMembershipByID(ctx, tx, request.MembershipID)
		if err != nil {
			return mapMutationFailure(err)
		}
		ownMembership := ownsMembership(actor, target)
		if err := service.authorizeMembershipConnections(
			ctx, tx, actor, scope, authorization, target, ownMembership, request.ConnectionIDs,
		); err != nil {
			return err
		}
		digest, err := canonicalIntent(struct {
			Operation      string         `json:"operation"`
			Scope          contract.Scope `json:"scope"`
			MembershipID   string         `json:"membership_id"`
			ConnectionIDs  []string       `json:"add_connection_ids"`
			IntentRevision uint64         `json:"expected_revision"`
		}{
			Operation:      membershipUpdateOperation,
			Scope:          scope,
			MembershipID:   request.MembershipID,
			ConnectionIDs:  request.ConnectionIDs,
			IntentRevision: intentRevision,
		})
		if err != nil {
			return invalidRequestFailure()
		}
		key := mutationReceiptKey(scope, actor, membershipUpdateOperation, request.MembershipID, request.Control.IdempotencyKey)
		begin, err := store.BeginIdentityMutation(ctx, tx, key, digest, intentRevision)
		if err != nil {
			return mapMutationFailure(err)
		}
		if !begin.Fresh {
			if err := decodeReplay(begin.Receipt, &result); err != nil {
				return err
			}
			result.Mutation.Status = StatusOK
			result.Mutation.Replayed = true
			return nil
		}
		if err := requireFreshMutationPrecondition(request.Control); err != nil {
			return err
		}
		auditID, err := randomApplicationID(service.random, "audit-", 18)
		if err != nil {
			return err
		}
		update, err := store.UpdateMembershipConnections(ctx, tx, store.UpdateMembershipConnectionsRequest{
			Scope:            scope,
			MembershipID:     request.MembershipID,
			ConnectionIDs:    request.ConnectionIDs,
			ExpectedRevision: intentRevision,
			AuditID:          auditID,
		})
		if err != nil {
			if errors.Is(err, store.ErrMembershipGrantLimit) {
				return invalidRequestFailure()
			}
			return mapMutationFailure(err)
		}
		// The locked row is the actor's own authority for a self-add. Check it
		// again, so the decision and the write use the same row.
		if ownMembership && !holdsSelfAddPermissions(update.Previous.Permissions) {
			return mapAuthorizationFailure(auth.ErrResourceNotFound)
		}
		membership, err := membershipFromStore(update.Current)
		if err != nil {
			return err
		}
		result = MembershipMutationResult{
			Membership: membership,
			Mutation: MutationMetadata{
				ResultRef:       request.MembershipID,
				Revision:        update.Current.ControlRevision,
				ControlRevision: update.Current.ControlRevision,
				Status:          StatusOK,
				Unchanged:       update.Unchanged,
			},
		}
		return service.finishMutation(ctx, tx, key, result.Mutation, result)
	})
	if errors.Is(err, store.ErrCommitOutcomeUnknown) {
		// The COMMIT may have taken effect. The outcome stays unknown, and only
		// a request with the same key may reconcile it, as for a run cancel.
		return MembershipMutationResult{}, &Failure{
			HTTPStatus:  503,
			Code:        "mutation_outcome_unknown",
			RetryAction: "same_key_reconcile_only",
			cause:       err,
		}
	}
	return result, err
}

// authorizeMembershipConnections applies the rule for added connections
// (#284, slice 284c, option A):
//  1. The actor holds workspace.access.manage (checked by the caller) and, for
//     its own membership, connection.manage, in its current grants inside the
//     update transaction.
//  2. A self-add names only the actor's own membership and only active
//     connections of the same workspace.
//  3. The update only adds connections. It removes nothing and adds no other
//     grant kind.
//  4. For another member's membership, the actor adds only connections that
//     its own current grants hold, as a grant does.
//  5. Each changed update writes the audit record (store).
func (service *IdentityService) authorizeMembershipConnections(
	ctx context.Context,
	tx *store.Transaction,
	actor contract.Actor,
	scope contract.Scope,
	authorization auth.Authorization,
	target store.MembershipAuthority,
	ownMembership bool,
	connectionIDs []string,
) error {
	for _, connectionID := range connectionIDs {
		connection, err := store.LoadConnectionAuthority(ctx, tx, connectionID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrScopeMismatch) {
				return resourceNotFoundFailure(err)
			}
			return err
		}
		if connection.State != "active" || connection.RevokedAt != nil {
			return resourceNotFoundFailure(store.ErrNotFound)
		}
	}
	if ownMembership {
		if _, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:        scope,
			Actor:        actor,
			Permission:   "connection.manage",
			ResourceKind: auth.ResourceWorkspace,
			ResourceID:   scope.WorkspaceID,
		}); err != nil {
			return mapAuthorizationFailure(err)
		}
		return nil
	}
	if !authorization.ContainsGrant(nil, connectionIDs, nil, target.ExpiresAt) {
		return mapAuthorizationFailure(auth.ErrResourceNotFound)
	}
	return nil
}

// ownsMembership reports whether the target is the direct user actor's own
// membership. A delegated or service-account actor owns no membership here.
func ownsMembership(actor contract.Actor, target store.MembershipAuthority) bool {
	return actor.Delegation == nil &&
		actor.RequestedActor.Kind == contract.PrincipalUser &&
		actor.RequestedActor.Equal(actor.EffectiveActor) &&
		target.Principal.Equal(actor.RequestedActor)
}

func holdsSelfAddPermissions(permissions []string) bool {
	return slices.Contains(permissions, "workspace.access.manage") &&
		slices.Contains(permissions, "connection.manage")
}
