package app

import (
	"context"
	"slices"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

type Delegation struct {
	DelegationID    string
	RequestedActor  contract.Principal
	EffectiveActor  contract.Principal
	ParentGrantRef  string
	ParentCeiling   contract.GrantCeiling
	WorkspaceScopes []string
	ConnectionIDs   []string
	OperationIDs    []string
	ScopeDigest     contract.SemanticDigest
	PolicyEpoch     uint64
	ControlRevision uint64
	ExpiresAt       time.Time
	RevokedAt       *time.Time
	Hop             uint8
}

type DelegationPage struct {
	Items              []Delegation
	CollectionRevision uint64
	NextCursor         string
}

type CreateDelegation struct {
	RequestedActor  contract.Principal
	EffectiveActor  contract.Principal
	WorkspaceScopes []string
	ConnectionIDs   []string
	OperationIDs    []string
	ExpiresAt       time.Time
	Control         MutationControl
}

type RevokeDelegation struct {
	DelegationID   string
	RequestedActor contract.Principal
	EffectiveActor contract.Principal
	ScopeDigest    contract.SemanticDigest
	ExpiresAt      time.Time
	Control        MutationControl
}

type DelegationMutationResult struct {
	Delegation Delegation
	Mutation   MutationMetadata
}

func (service *IdentityService) ListDelegations(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	afterID string,
	pageSize uint32,
) (DelegationPage, error) {
	if service == nil || ctx == nil || pageSize == 0 || pageSize > 100 {
		return DelegationPage{}, invalidRequestFailure()
	}
	var result DelegationPage
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		if _, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:        scope,
			Actor:        actor,
			Permission:   "workspace.access.read",
			ResourceKind: auth.ResourceWorkspace,
			ResourceID:   scope.WorkspaceID,
		}); err != nil {
			return mapAuthorizationFailure(err)
		}
		items, revisions, err := store.ListDelegationAuthorities(ctx, tx, afterID, pageSize+1)
		if err != nil {
			return err
		}
		result.CollectionRevision = revisions.Delegations
		result.Items = make([]Delegation, 0, min(len(items), int(pageSize)))
		for index, item := range items {
			if index == int(pageSize) {
				result.NextCursor = result.Items[len(result.Items)-1].DelegationID
				break
			}
			delegation, convertErr := delegationFromStore(item)
			if convertErr != nil {
				return convertErr
			}
			result.Items = append(result.Items, delegation)
		}
		return nil
	})
	return result, err
}

func (service *IdentityService) CreateDelegation(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	request CreateDelegation,
) (DelegationMutationResult, error) {
	if service == nil || ctx == nil {
		return DelegationMutationResult{}, invalidRequestFailure()
	}
	var result DelegationMutationResult
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		if err := store.BindAuthDeployment(ctx, tx, service.deploymentID); err != nil {
			return err
		}
		if actor.Delegation != nil {
			return mapAuthorizationFailure(auth.ErrDelegationInvalid)
		}
		parent, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:           scope,
			Actor:           actor,
			Permission:      "workspace.access.manage",
			ResourceKind:    auth.ResourceWorkspace,
			ResourceID:      scope.WorkspaceID,
			AutomationScope: true,
		})
		if err != nil {
			return mapAuthorizationFailure(err)
		}
		intentRevision, err := validateMutationControl(request.Control)
		if err != nil {
			return err
		}
		if err := request.RequestedActor.Validate(); err != nil ||
			!request.RequestedActor.Equal(actor.RequestedActor) ||
			request.RequestedActor.Kind != contract.PrincipalServiceAccount ||
			request.EffectiveActor.Validate() != nil ||
			request.EffectiveActor.Kind != contract.PrincipalUser ||
			request.EffectiveActor.OrgID != scope.OrgID || request.EffectiveActor.Equal(request.RequestedActor) ||
			!validWorkspaceScopes(scope, request.WorkspaceScopes) ||
			!validSortedIDs(request.ConnectionIDs) || !validSortedOperationIDs(request.OperationIDs) ||
			request.ExpiresAt.IsZero() || request.ExpiresAt.Location() != time.UTC ||
			request.ExpiresAt.After(parent.ExpiresAt) ||
			!sortedSubset(request.ConnectionIDs, parent.ConnectionIDs) ||
			!sortedSubset(request.OperationIDs, parent.OperationIDs) {
			return mapAuthorizationFailure(auth.ErrDelegationInvalid)
		}
		effectiveActor := contract.Actor{
			RequestedActor: request.EffectiveActor,
			EffectiveActor: request.EffectiveActor,
		}
		var effective auth.Authorization
		if err := store.WithinAuthorizationActor(ctx, tx, effectiveActor, func(ctx context.Context, tx *store.Transaction) error {
			var authorizeErr error
			effective, authorizeErr = service.authorize(ctx, tx, auth.AuthorizationRequest{
				Scope:        scope,
				Actor:        effectiveActor,
				Permission:   "operation.invoke",
				ResourceKind: auth.ResourceWorkspace,
				ResourceID:   scope.WorkspaceID,
			})
			return authorizeErr
		}); err != nil {
			return mapAuthorizationFailure(err)
		}
		if request.ExpiresAt.After(effective.ExpiresAt) ||
			!sortedSubset(request.ConnectionIDs, effective.ConnectionIDs) ||
			!sortedSubset(request.OperationIDs, effective.OperationIDs) {
			return mapAuthorizationFailure(auth.ErrDelegationInvalid)
		}
		parentCeiling := contract.GrantCeiling{
			Scope:         scope,
			ConnectionIDs: slices.Clone(parent.ConnectionIDs),
			OperationIDs:  slices.Clone(parent.OperationIDs),
			ApprovalRefs:  []string{},
			ExpiresAt:     parent.ExpiresAt,
		}
		if err := parentCeiling.Validate(); err != nil {
			return mapAuthorizationFailure(auth.ErrDelegationInvalid)
		}
		digest, err := canonicalIntent(struct {
			Operation       string             `json:"operation"`
			Scope           contract.Scope     `json:"scope"`
			RequestedActor  contract.Principal `json:"requested_actor"`
			EffectiveActor  contract.Principal `json:"effective_actor"`
			WorkspaceScopes []string           `json:"workspace_scopes"`
			ConnectionIDs   []string           `json:"connection_ids"`
			OperationIDs    []string           `json:"operation_ids"`
			ExpiresAt       time.Time          `json:"expires_at"`
			IntentRevision  uint64             `json:"expected_revision"`
		}{
			Operation:       "delegation.create",
			Scope:           scope,
			RequestedActor:  request.RequestedActor,
			EffectiveActor:  request.EffectiveActor,
			WorkspaceScopes: request.WorkspaceScopes,
			ConnectionIDs:   request.ConnectionIDs,
			OperationIDs:    request.OperationIDs,
			ExpiresAt:       request.ExpiresAt,
			IntentRevision:  intentRevision,
		})
		if err != nil {
			return invalidRequestFailure()
		}
		key := mutationReceiptKey(scope, actor, "delegation.create", "delegations", request.Control.IdempotencyKey)
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
		delegationID, err := randomApplicationID(service.random, "delegation-", 18)
		if err != nil {
			return err
		}
		stored, collectionRevision, err := store.CreateDelegation(ctx, tx, store.CreateDelegationRequest{
			DelegationID:               delegationID,
			Scope:                      scope,
			RequestedActor:             request.RequestedActor,
			EffectiveActor:             request.EffectiveActor,
			ParentGrantRef:             parent.AuthorityRef,
			ParentCeiling:              parentCeiling,
			ConnectionIDs:              request.ConnectionIDs,
			OperationIDs:               request.OperationIDs,
			ExpiresAt:                  request.ExpiresAt,
			ExpectedCollectionRevision: intentRevision,
		})
		if err != nil {
			return mapMutationFailure(err)
		}
		delegation, err := delegationFromStore(stored)
		if err != nil {
			return err
		}
		result = DelegationMutationResult{
			Delegation: delegation,
			Mutation: MutationMetadata{
				ResultRef:       delegationID,
				Revision:        collectionRevision,
				ControlRevision: stored.ControlRevision,
				Status:          StatusCreated,
			},
		}
		return service.finishMutation(ctx, tx, key, result.Mutation, result)
	})
	return result, err
}

func (service *IdentityService) RevokeDelegation(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	request RevokeDelegation,
) (DelegationMutationResult, error) {
	if service == nil || ctx == nil || !validApplicationID(request.DelegationID) {
		return DelegationMutationResult{}, invalidRequestFailure()
	}
	var result DelegationMutationResult
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		if _, err := service.bindAndAuthorizeAccessMutation(ctx, tx, actor, scope); err != nil {
			return err
		}
		intentRevision, err := validateMutationControl(request.Control)
		if err != nil {
			return err
		}
		if err := request.RequestedActor.Validate(); err != nil ||
			request.RequestedActor.Kind != contract.PrincipalServiceAccount ||
			request.RequestedActor.OrgID != scope.OrgID ||
			request.EffectiveActor.Validate() != nil ||
			request.EffectiveActor.Kind != contract.PrincipalUser ||
			request.EffectiveActor.OrgID != scope.OrgID ||
			request.RequestedActor.Equal(request.EffectiveActor) ||
			request.ScopeDigest.Validate() != nil ||
			request.ExpiresAt.IsZero() || request.ExpiresAt.Location() != time.UTC {
			return invalidRequestFailure()
		}
		digest, err := canonicalIntent(struct {
			Operation      string                  `json:"operation"`
			Scope          contract.Scope          `json:"scope"`
			DelegationID   string                  `json:"delegation_id"`
			RequestedActor contract.Principal      `json:"requested_actor"`
			EffectiveActor contract.Principal      `json:"effective_actor"`
			ScopeDigest    contract.SemanticDigest `json:"scope_digest"`
			ExpiresAt      time.Time               `json:"expires_at"`
			IntentRevision uint64                  `json:"expected_revision"`
		}{
			Operation:      "delegation.revoke",
			Scope:          scope,
			DelegationID:   request.DelegationID,
			RequestedActor: request.RequestedActor,
			EffectiveActor: request.EffectiveActor,
			ScopeDigest:    request.ScopeDigest,
			ExpiresAt:      request.ExpiresAt,
			IntentRevision: intentRevision,
		})
		if err != nil {
			return invalidRequestFailure()
		}
		key := mutationReceiptKey(scope, actor, "delegation.revoke", request.DelegationID, request.Control.IdempotencyKey)
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
		current, err := store.LoadDelegationAuthority(ctx, tx, request.DelegationID)
		if err != nil {
			return mapMutationFailure(err)
		}
		currentDelegation, err := delegationFromStore(current)
		if err != nil {
			return err
		}
		if !current.RequestedActor.Equal(request.RequestedActor) ||
			!current.EffectiveActor.Equal(request.EffectiveActor) ||
			currentDelegation.ScopeDigest != request.ScopeDigest ||
			!current.ExpiresAt.Equal(request.ExpiresAt) {
			return mapMutationFailure(store.ErrCASConflict)
		}
		stored, unchanged, err := store.RevokeDelegation(ctx, tx, store.RevokeDelegationRequest{
			Scope:            scope,
			DelegationID:     current.DelegationID,
			RequestedActor:   current.RequestedActor,
			EffectiveActor:   current.EffectiveActor,
			ParentGrantRef:   current.ParentGrantRef,
			ParentCeiling:    current.ParentCeiling,
			ConnectionIDs:    current.ConnectionIDs,
			OperationIDs:     current.OperationIDs,
			ExpiresAt:        current.ExpiresAt,
			ExpectedRevision: intentRevision,
		})
		if err != nil {
			return mapMutationFailure(err)
		}
		delegation, err := delegationFromStore(stored)
		if err != nil {
			return err
		}
		result = DelegationMutationResult{
			Delegation: delegation,
			Mutation: MutationMetadata{
				ResultRef:       request.DelegationID,
				Revision:        stored.ControlRevision,
				ControlRevision: stored.ControlRevision,
				Status:          StatusOK,
				Unchanged:       unchanged,
			},
		}
		return service.finishMutation(ctx, tx, key, result.Mutation, result)
	})
	return result, err
}

func DelegatedActor(value Delegation) (contract.Actor, error) {
	delegation := contract.Delegation{
		DelegationID:   value.DelegationID,
		ParentGrantRef: value.ParentGrantRef,
		Hop:            value.Hop,
		ParentCeiling:  value.ParentCeiling,
	}
	actor := contract.Actor{
		RequestedActor: value.RequestedActor,
		EffectiveActor: value.EffectiveActor,
		Delegation:     &delegation,
	}
	if err := actor.Validate(); err != nil {
		return contract.Actor{}, err
	}
	return actor, nil
}

func delegationFromStore(value store.DelegationAuthority) (Delegation, error) {
	parentCeiling := contract.GrantCeiling{
		Scope:         value.ParentCeiling.Scope,
		ConnectionIDs: slices.Clone(value.ParentCeiling.ConnectionIDs),
		OperationIDs:  slices.Clone(value.ParentCeiling.OperationIDs),
		ApprovalRefs:  slices.Clone(value.ParentCeiling.ApprovalRefs),
		ExpiresAt:     value.ParentCeiling.ExpiresAt,
	}
	workspaceScopes := []string{value.Scope.WorkspaceID}
	connectionIDs := slices.Clone(value.ConnectionIDs)
	operationIDs := slices.Clone(value.OperationIDs)
	scopeDigest, err := canonicalIntent(struct {
		Kind            string                `json:"kind"`
		Scope           contract.Scope        `json:"scope"`
		DelegationID    string                `json:"delegation_id"`
		RequestedActor  contract.Principal    `json:"requested_actor"`
		EffectiveActor  contract.Principal    `json:"effective_actor"`
		ParentGrantRef  string                `json:"parent_grant_ref"`
		ParentCeiling   contract.GrantCeiling `json:"parent_ceiling"`
		WorkspaceScopes []string              `json:"workspace_scopes"`
		ConnectionIDs   []string              `json:"connection_ids"`
		OperationIDs    []string              `json:"operation_ids"`
		ExpiresAt       time.Time             `json:"expires_at"`
	}{
		Kind:            "delegation.scope",
		Scope:           value.Scope,
		DelegationID:    value.DelegationID,
		RequestedActor:  value.RequestedActor,
		EffectiveActor:  value.EffectiveActor,
		ParentGrantRef:  value.ParentGrantRef,
		ParentCeiling:   parentCeiling,
		WorkspaceScopes: workspaceScopes,
		ConnectionIDs:   connectionIDs,
		OperationIDs:    operationIDs,
		ExpiresAt:       value.ExpiresAt,
	})
	if err != nil {
		return Delegation{}, err
	}
	return Delegation{
		DelegationID:    value.DelegationID,
		RequestedActor:  value.RequestedActor,
		EffectiveActor:  value.EffectiveActor,
		ParentGrantRef:  value.ParentGrantRef,
		ParentCeiling:   parentCeiling,
		WorkspaceScopes: workspaceScopes,
		ConnectionIDs:   connectionIDs,
		OperationIDs:    operationIDs,
		ScopeDigest:     scopeDigest,
		PolicyEpoch:     value.PolicyEpoch,
		ControlRevision: value.ControlRevision,
		ExpiresAt:       value.ExpiresAt,
		RevokedAt:       copyTime(value.RevokedAt),
		Hop:             value.Hop,
	}, nil
}
