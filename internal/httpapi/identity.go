package httpapi

import (
	"context"
	"errors"
	"strconv"

	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func (service *Service) ListWorkspaces(ctx context.Context, request ListWorkspacesRequestObject) (ListWorkspacesResponseObject, error) {
	state, err := authenticatedState(ctx)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	afterID, pageSize, err := service.parsePage("workspaces", state.identity.Principal, contract.Scope{}, request.Params.Cursor, request.Params.PageSize)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	page, err := service.identity.ListWorkspaces(ctx, state.identity.Principal, afterID, pageSize)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	next, err := service.nextCursor("workspaces", state.identity.Principal, contract.Scope{}, pageSize, page.NextCursor)
	if err != nil {
		return nil, err
	}
	items := make([]api.Workspace, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, workspaceResponse(item))
	}
	return ListWorkspaces200JSONResponse(api.WorkspacePage{Items: items, NextCursor: next}), nil
}

func (service *Service) GetWorkspace(ctx context.Context, request GetWorkspaceRequestObject) (GetWorkspaceResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	workspace, err := service.identity.InspectWorkspace(ctx, state.identity.Actor, scope)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	setETag(ctx, workspace.MembershipRevision)
	return GetWorkspace200JSONResponse(workspaceResponse(workspace)), nil
}

func (service *Service) ListConnections(ctx context.Context, request ListConnectionsRequestObject) (ListConnectionsResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	afterID, pageSize, err := service.parsePage("connections", state.identity.Principal, scope, request.Params.Cursor, request.Params.PageSize)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	page, err := service.identity.ListConnections(ctx, state.identity.Actor, scope, afterID, pageSize)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	next, err := service.nextCursor("connections", state.identity.Principal, scope, pageSize, page.NextCursor)
	if err != nil {
		return nil, err
	}
	items := make([]api.Connection, 0, len(page.Items))
	for _, item := range page.Items {
		converted, err := connectionResponse(item)
		if err != nil {
			return nil, err
		}
		items = append(items, converted)
	}
	setETag(ctx, page.CollectionRevision)
	return ListConnections200JSONResponse(api.ConnectionPage{
		CollectionRevision: strconv.FormatUint(page.CollectionRevision, 10),
		Items:              items,
		NextCursor:         next,
	}), nil
}

func (service *Service) CreateConnection(ctx context.Context, request CreateConnectionRequestObject) (CreateConnectionResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if request.Body == nil {
		return invalidRequestResponse(), nil
	}
	control, err := parseMutationControl(request.Params.IdempotencyKey, request.Body.Protocol, request.Params.IfMatch)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	result, err := service.identity.CreateConnection(ctx, state.identity.Actor, scope, app.CreateConnection{
		ConnectorID:       request.Body.Connection.ConnectorId,
		ProviderAccountID: request.Body.Connection.ProviderAccountId,
		DisplayName:       request.Body.Connection.DisplayName,
		Control:           control,
	})
	if err != nil {
		return asMutationProblemResponse(err, request.Body.Protocol.RequestKey, api.SafeOutcomeReferenceKindConnection, ""), nil
	}
	response, err := connectionMutationResponse(result, request.Body.Protocol.RequestKey, api.ConnectionMutationResponseResultDispositionCreated)
	if err != nil {
		return nil, err
	}
	setETag(ctx, result.Connection.Revision)
	if result.Mutation.Replayed {
		return CreateConnection200JSONResponse(response), nil
	}
	return CreateConnection201JSONResponse(response), nil
}

func (service *Service) GetConnection(ctx context.Context, request GetConnectionRequestObject) (GetConnectionResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	result, err := service.identity.InspectConnection(ctx, state.identity.Actor, scope, request.ConnectionId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	response, err := connectionResponse(result)
	if err != nil {
		return nil, err
	}
	setETag(ctx, result.Revision)
	return GetConnection200JSONResponse(response), nil
}

func (service *Service) UpdateConnection(ctx context.Context, request UpdateConnectionRequestObject) (UpdateConnectionResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if request.Body == nil {
		return invalidRequestResponse(), nil
	}
	control, err := parseMutationControl(request.Params.IdempotencyKey, request.Body.Protocol, request.Params.IfMatch)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	result, err := service.identity.UpdateConnection(ctx, state.identity.Actor, scope, app.UpdateConnection{
		ConnectionID: request.ConnectionId,
		DisplayName:  request.Body.Connection.DisplayName,
		Control:      control,
	})
	if err != nil {
		return asMutationProblemResponse(err, request.Body.Protocol.RequestKey, api.SafeOutcomeReferenceKindConnection, request.ConnectionId), nil
	}
	disposition := api.ConnectionMutationResponseResultDispositionUpdated
	if result.Mutation.Unchanged {
		disposition = api.ConnectionMutationResponseResultDispositionUnchanged
	}
	response, err := connectionMutationResponse(result, request.Body.Protocol.RequestKey, disposition)
	if err != nil {
		return nil, err
	}
	setETag(ctx, result.Connection.Revision)
	return UpdateConnection200JSONResponse(response), nil
}

func (service *Service) DeleteConnection(ctx context.Context, request DeleteConnectionRequestObject) (DeleteConnectionResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if request.Body == nil {
		return invalidRequestResponse(), nil
	}
	control, err := parseMutationControl(request.Params.IdempotencyKey, request.Body.Protocol, request.Params.IfMatch)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	result, err := service.identity.DeleteConnection(ctx, state.identity.Actor, scope, app.DeleteConnection{
		ConnectionID: request.ConnectionId,
		Control:      control,
	})
	if err != nil {
		return asMutationProblemResponse(err, request.Body.Protocol.RequestKey, api.SafeOutcomeReferenceKindConnection, request.ConnectionId), nil
	}
	disposition := api.ConnectionMutationResponseResultDispositionRevoked
	if result.Mutation.Unchanged {
		disposition = api.ConnectionMutationResponseResultDispositionUnchanged
	}
	response, err := connectionMutationResponse(result, request.Body.Protocol.RequestKey, disposition)
	if err != nil {
		return nil, err
	}
	setETag(ctx, result.Connection.Revision)
	return DeleteConnection200JSONResponse(response), nil
}

func (service *Service) ListMemberships(ctx context.Context, request ListMembershipsRequestObject) (ListMembershipsResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	afterID, pageSize, err := service.parsePage("memberships", state.identity.Principal, scope, request.Params.Cursor, request.Params.PageSize)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	page, err := service.identity.ListMemberships(ctx, state.identity.Actor, scope, afterID, pageSize)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	next, err := service.nextCursor("memberships", state.identity.Principal, scope, pageSize, page.NextCursor)
	if err != nil {
		return nil, err
	}
	items := make([]api.Membership, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, membershipResponse(item))
	}
	setETag(ctx, page.CollectionRevision)
	return ListMemberships200JSONResponse(api.MembershipPage{
		CollectionRevision: strconv.FormatUint(page.CollectionRevision, 10),
		Items:              items,
		NextCursor:         next,
	}), nil
}

func (service *Service) GrantMembership(ctx context.Context, request GrantMembershipRequestObject) (GrantMembershipResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if request.Body == nil || request.Body.Membership.ResourceGrants == nil {
		return invalidRequestResponse(), nil
	}
	expiresAt, err := requiredNullableTime(request.Body.Membership.ExpiresAt)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	control, err := parseMutationControl(request.Params.IdempotencyKey, request.Body.Protocol, request.Params.IfMatch)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	result, err := service.identity.GrantMembership(ctx, state.identity.Actor, scope, app.GrantMembership{
		Principal:      contract.Principal{OrgID: scope.OrgID, PrincipalID: request.Body.Membership.PrincipalRef, Kind: contract.PrincipalUser},
		Role:           request.Body.Membership.Role,
		ResourceGrants: cloneStrings(request.Body.Membership.ResourceGrants),
		ExpiresAt:      expiresAt,
		Control:        control,
	})
	if err != nil {
		return asMutationProblemResponse(err, request.Body.Protocol.RequestKey, api.SafeOutcomeReferenceKindMembership, ""), nil
	}
	fresh, replay, err := mutationResponses(result.Mutation, result.Membership.MembershipID, api.SafeOutcomeReferenceKindMembership, state.identity.Actor, api.MutationResultDispositionCreated)
	if err != nil {
		return nil, err
	}
	setETag(ctx, result.Membership.ControlRevision)
	if result.Mutation.Replayed {
		return GrantMembership200JSONResponse(replay), nil
	}
	return GrantMembership201JSONResponse(fresh), nil
}

func (service *Service) RevokeMembership(ctx context.Context, request RevokeMembershipRequestObject) (RevokeMembershipResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if request.Body == nil || request.Body.Membership.MembershipRef != request.Id {
		return invalidRequestResponse(), nil
	}
	expiresAt, err := requiredNullableTime(request.Body.Membership.ExpiresAt)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	control, err := parseMutationControl(request.Params.IdempotencyKey, request.Body.Protocol, request.Params.IfMatch)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	result, err := service.identity.RevokeMembership(ctx, state.identity.Actor, scope, app.RevokeMembership{
		MembershipID:     request.Id,
		BoundScopeDigest: contract.SemanticDigest(request.Body.Membership.BoundScopeDigest),
		ExpiresAt:        expiresAt,
		Control:          control,
	})
	if err != nil {
		return asMutationProblemResponse(err, request.Body.Protocol.RequestKey, api.SafeOutcomeReferenceKindMembership, request.Id), nil
	}
	disposition := api.MutationResultDispositionRevoked
	if result.Mutation.Unchanged {
		disposition = api.MutationResultDispositionUnchanged
	}
	fresh, replay, err := mutationResponses(result.Mutation, result.Membership.MembershipID, api.SafeOutcomeReferenceKindMembership, state.identity.Actor, disposition)
	if err != nil {
		return nil, err
	}
	setETag(ctx, result.Membership.ControlRevision)
	body := api.RevokeMembership200JSONResponseBody{}
	if result.Mutation.Replayed {
		err = body.FromMutationReplayResponse(replay)
	} else {
		err = body.FromMutationResultResponse(fresh)
	}
	if err != nil {
		return nil, err
	}
	return RevokeMembership200JSONResponse(body), nil
}

func (service *Service) ListDelegations(ctx context.Context, request ListDelegationsRequestObject) (ListDelegationsResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	afterID, pageSize, err := service.parsePage("delegations", state.identity.Principal, scope, request.Params.Cursor, request.Params.PageSize)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	page, err := service.identity.ListDelegations(ctx, state.identity.Actor, scope, afterID, pageSize)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	next, err := service.nextCursor("delegations", state.identity.Principal, scope, pageSize, page.NextCursor)
	if err != nil {
		return nil, err
	}
	items := make([]api.Delegation, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, delegationResponse(item))
	}
	setETag(ctx, page.CollectionRevision)
	return ListDelegations200JSONResponse(api.DelegationPage{
		CollectionRevision: strconv.FormatUint(page.CollectionRevision, 10),
		Items:              items,
		NextCursor:         next,
	}), nil
}

func (service *Service) CreateDelegation(ctx context.Context, request CreateDelegationRequestObject) (CreateDelegationResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if request.Body == nil || request.Body.Delegation.WorkspaceScopes == nil ||
		request.Body.Delegation.ConnectionScopes == nil || request.Body.Delegation.OperationScopes == nil {
		return invalidRequestResponse(), nil
	}
	control, err := parseMutationControl(request.Params.IdempotencyKey, request.Body.Protocol, request.Params.IfMatch)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	result, err := service.identity.CreateDelegation(ctx, state.identity.Actor, scope, app.CreateDelegation{
		RequestedActor:  contract.Principal{OrgID: scope.OrgID, PrincipalID: request.Body.Delegation.RequestedActorRef, Kind: contract.PrincipalServiceAccount},
		EffectiveActor:  contract.Principal{OrgID: scope.OrgID, PrincipalID: request.Body.Delegation.EffectiveActorRef, Kind: contract.PrincipalUser},
		WorkspaceScopes: cloneStrings(request.Body.Delegation.WorkspaceScopes),
		ConnectionIDs:   cloneStrings(request.Body.Delegation.ConnectionScopes),
		OperationIDs:    cloneStrings(request.Body.Delegation.OperationScopes),
		ExpiresAt:       request.Body.Delegation.ExpiresAt,
		Control:         control,
	})
	if err != nil {
		return asMutationProblemResponse(err, request.Body.Protocol.RequestKey, api.SafeOutcomeReferenceKindDelegation, ""), nil
	}
	fresh, replay, err := mutationResponses(result.Mutation, result.Delegation.DelegationID, api.SafeOutcomeReferenceKindDelegation, state.identity.Actor, api.MutationResultDispositionCreated)
	if err != nil {
		return nil, err
	}
	setETag(ctx, result.Delegation.ControlRevision)
	if result.Mutation.Replayed {
		return CreateDelegation200JSONResponse(replay), nil
	}
	return CreateDelegation201JSONResponse(fresh), nil
}

func (service *Service) RevokeDelegation(ctx context.Context, request RevokeDelegationRequestObject) (RevokeDelegationResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if request.Body == nil || request.Body.Delegation.DelegationRef != request.Id {
		return invalidRequestResponse(), nil
	}
	control, err := parseMutationControl(request.Params.IdempotencyKey, request.Body.Protocol, request.Params.IfMatch)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	result, err := service.identity.RevokeDelegation(ctx, state.identity.Actor, scope, app.RevokeDelegation{
		DelegationID:   request.Id,
		RequestedActor: contract.Principal{OrgID: scope.OrgID, PrincipalID: request.Body.Delegation.RequestedActorRef, Kind: contract.PrincipalServiceAccount},
		EffectiveActor: contract.Principal{OrgID: scope.OrgID, PrincipalID: request.Body.Delegation.EffectiveActorRef, Kind: contract.PrincipalUser},
		ScopeDigest:    contract.SemanticDigest(request.Body.Delegation.ScopeDigest),
		ExpiresAt:      request.Body.Delegation.ExpiresAt,
		Control:        control,
	})
	if err != nil {
		return asMutationProblemResponse(err, request.Body.Protocol.RequestKey, api.SafeOutcomeReferenceKindDelegation, request.Id), nil
	}
	disposition := api.MutationResultDispositionRevoked
	if result.Mutation.Unchanged {
		disposition = api.MutationResultDispositionUnchanged
	}
	fresh, replay, err := mutationResponses(result.Mutation, result.Delegation.DelegationID, api.SafeOutcomeReferenceKindDelegation, state.identity.Actor, disposition)
	if err != nil {
		return nil, err
	}
	setETag(ctx, result.Delegation.ControlRevision)
	body := api.RevokeDelegation200JSONResponseBody{}
	if result.Mutation.Replayed {
		err = body.FromMutationReplayResponse(replay)
	} else {
		err = body.FromMutationResultResponse(fresh)
	}
	if err != nil {
		return nil, err
	}
	return RevokeDelegation200JSONResponse(body), nil
}

func (service *Service) ListServiceAccounts(ctx context.Context, request ListServiceAccountsRequestObject) (ListServiceAccountsResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	afterID, pageSize, err := service.parsePage("service_accounts", state.identity.Principal, scope, request.Params.Cursor, request.Params.PageSize)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	page, err := service.identity.ListServiceAccounts(ctx, state.identity.Actor, scope, afterID, pageSize)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	next, err := service.nextCursor("service_accounts", state.identity.Principal, scope, pageSize, page.NextCursor)
	if err != nil {
		return nil, err
	}
	items := make([]api.ServiceAccount, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, serviceAccountResponse(item))
	}
	setETag(ctx, page.CollectionRevision)
	return ListServiceAccounts200JSONResponse(api.ServiceAccountPage{
		CollectionRevision: strconv.FormatUint(page.CollectionRevision, 10),
		Items:              items,
		NextCursor:         next,
	}), nil
}

func (service *Service) CreateServiceAccount(ctx context.Context, request CreateServiceAccountRequestObject) (CreateServiceAccountResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if request.Body == nil || request.Body.ServiceAccount.WorkspaceScopes == nil ||
		request.Body.ServiceAccount.ConnectionScopes == nil || request.Body.ServiceAccount.OperationScopes == nil {
		return invalidRequestResponse(), nil
	}
	displayName, err := requiredNullableString(request.Body.ServiceAccount.DisplayName)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	control, err := parseMutationControl(request.Params.IdempotencyKey, request.Body.Protocol, request.Params.IfMatch)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	result, err := service.identity.CreateServiceAccount(ctx, state.identity.Actor, scope, app.CreateServiceAccount{
		DisplayName:     displayName,
		WorkspaceScopes: cloneStrings(request.Body.ServiceAccount.WorkspaceScopes),
		ConnectionIDs:   cloneStrings(request.Body.ServiceAccount.ConnectionScopes),
		OperationIDs:    cloneStrings(request.Body.ServiceAccount.OperationScopes),
		ExpiresAt:       request.Body.ServiceAccount.ExpiresAt,
		Control:         control,
	})
	if err != nil {
		return asMutationProblemResponse(err, request.Body.Protocol.RequestKey, api.SafeOutcomeReferenceKindServiceAccount, ""), nil
	}
	if result.Mutation.ResultRef == "" || result.Mutation.Revision == 0 || result.ServiceAccount.ServiceAccountID == "" {
		return nil, errors.New("service-account mutation returned incomplete durable metadata")
	}
	outcome := api.SafeOutcomeReference{Kind: api.SafeOutcomeReferenceKindServiceAccount, Ref: result.Mutation.ResultRef}
	setETag(ctx, result.ServiceAccount.ControlRevision)
	if result.Mutation.Replayed {
		_, replay, err := mutationResponses(result.Mutation, result.ServiceAccount.ServiceAccountID, api.SafeOutcomeReferenceKindServiceAccount, state.identity.Actor, api.MutationResultDispositionCreated)
		if err != nil {
			return nil, err
		}
		return CreateServiceAccount200JSONResponse(replay), nil
	}
	if result.OneTimeCredential == "" {
		return nil, errors.New("new service account did not return its one-time credential")
	}
	return CreateServiceAccount201JSONResponse(api.ServiceAccountCreationResponse{
		OutcomeReference:       outcome,
		PmAutomationCredential: result.OneTimeCredential,
		Replay:                 api.ServiceAccountCreationResponseReplay(false),
		ResourceId:             result.ServiceAccount.ServiceAccountID,
		ResultDisposition:      api.ServiceAccountCreationResponseResultDispositionCreated,
		ResultRevision:         strconv.FormatUint(result.Mutation.Revision, 10),
	}), nil
}

func (service *Service) RevokeServiceAccount(ctx context.Context, request RevokeServiceAccountRequestObject) (RevokeServiceAccountResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if request.Body == nil || request.Body.ServiceAccount.ServiceAccountRef != request.Id {
		return invalidRequestResponse(), nil
	}
	control, err := parseMutationControl(request.Params.IdempotencyKey, request.Body.Protocol, request.Params.IfMatch)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	result, err := service.identity.RevokeServiceAccount(ctx, state.identity.Actor, scope, app.RevokeServiceAccount{
		ServiceAccountID: request.Id,
		GrantRevision:    request.Body.ServiceAccount.GrantRevision,
		ExpiresAt:        request.Body.ServiceAccount.ExpiresAt,
		Control:          control,
	})
	if err != nil {
		return asMutationProblemResponse(err, request.Body.Protocol.RequestKey, api.SafeOutcomeReferenceKindServiceAccount, request.Id), nil
	}
	disposition := api.MutationResultDispositionRevoked
	if result.Mutation.Unchanged {
		disposition = api.MutationResultDispositionUnchanged
	}
	fresh, replay, err := mutationResponses(result.Mutation, result.ServiceAccount.ServiceAccountID, api.SafeOutcomeReferenceKindServiceAccount, state.identity.Actor, disposition)
	if err != nil {
		return nil, err
	}
	setETag(ctx, result.ServiceAccount.ControlRevision)
	body := api.RevokeServiceAccount200JSONResponseBody{}
	if result.Mutation.Replayed {
		err = body.FromMutationReplayResponse(replay)
	} else {
		err = body.FromMutationResultResponse(fresh)
	}
	if err != nil {
		return nil, err
	}
	return RevokeServiceAccount200JSONResponse(body), nil
}

func scopedState(ctx context.Context, workspaceID string) (requestState, contract.Scope, error) {
	state, err := authenticatedState(ctx)
	if err != nil {
		return requestState{}, contract.Scope{}, err
	}
	scope := contract.Scope{OrgID: state.identity.Principal.OrgID, WorkspaceID: workspaceID}
	if err := scope.Validate(); err != nil {
		return requestState{}, contract.Scope{}, errInvalidHTTPInput
	}
	return state, scope, nil
}

func workspaceResponse(value app.Workspace) api.Workspace {
	return api.Workspace{
		MembershipId:       value.MembershipID,
		MembershipRevision: strconv.FormatUint(value.MembershipRevision, 10),
		Name:               value.Name,
		OrgId:              value.Scope.OrgID,
		Role:               value.Role,
		WorkspaceId:        value.Scope.WorkspaceID,
	}
}

func principalResponse(value contract.Principal) api.Principal {
	return api.Principal{Kind: api.PrincipalKind(value.Kind), PrincipalId: value.PrincipalID}
}

func connectionResponse(value app.Connection) (api.Connection, error) {
	state := api.ConnectionState(value.State)
	if !state.Valid() || value.Revision == 0 {
		return api.Connection{}, errors.New("connection application result is invalid")
	}
	return api.Connection{
		ConnectionId:      value.ConnectionID,
		ConnectorId:       value.ConnectorID,
		CredentialRef:     value.CredentialRef,
		DisplayName:       value.DisplayName,
		OrgId:             value.Scope.OrgID,
		ProviderAccountId: value.ProviderAccountID,
		Revision:          strconv.FormatUint(value.Revision, 10),
		RevokedAt:         nullableTime(value.RevokedAt),
		State:             state,
		WorkspaceId:       value.Scope.WorkspaceID,
	}, nil
}

func membershipResponse(value app.Membership) api.Membership {
	return api.Membership{
		BoundScopeDigest: string(value.BoundScopeDigest),
		ControlRevision:  strconv.FormatUint(value.ControlRevision, 10),
		ExpiresAt:        nullableTime(value.ExpiresAt),
		MembershipRef:    value.MembershipID,
		PrincipalRef:     value.Principal.PrincipalID,
		ResourceGrants:   cloneStrings(value.ResourceGrants),
		RevokedAt:        nullableTime(value.RevokedAt),
		Role:             value.Role,
	}
}

func delegationResponse(value app.Delegation) api.Delegation {
	return api.Delegation{
		ConnectionScopes:  cloneStrings(value.ConnectionIDs),
		ControlRevision:   strconv.FormatUint(value.ControlRevision, 10),
		DelegationRef:     value.DelegationID,
		EffectiveActorRef: value.EffectiveActor.PrincipalID,
		ExpiresAt:         value.ExpiresAt,
		OperationScopes:   cloneStrings(value.OperationIDs),
		RequestedActorRef: value.RequestedActor.PrincipalID,
		RevokedAt:         nullableTime(value.RevokedAt),
		ScopeDigest:       string(value.ScopeDigest),
		WorkspaceScopes:   cloneStrings(value.WorkspaceScopes),
	}
}

func serviceAccountResponse(value app.ServiceAccount) api.ServiceAccount {
	return api.ServiceAccount{
		ConnectionScopes:  cloneStrings(value.ConnectionIDs),
		ControlRevision:   strconv.FormatUint(value.ControlRevision, 10),
		DisplayName:       nullableString(value.DisplayName),
		ExpiresAt:         value.ExpiresAt,
		GrantRevision:     value.GrantRevision,
		OperationScopes:   cloneStrings(value.OperationIDs),
		RevokedAt:         nullableTime(value.RevokedAt),
		ServiceAccountRef: value.ServiceAccountID,
		WorkspaceScopes:   cloneStrings(value.WorkspaceScopes),
	}
}

func connectionMutationResponse(
	result app.ConnectionMutationResult,
	requestKey string,
	disposition api.ConnectionMutationResponseResultDisposition,
) (api.ConnectionMutationResponse, error) {
	connection, err := connectionResponse(result.Connection)
	if err != nil {
		return api.ConnectionMutationResponse{}, err
	}
	if result.Mutation.ResultRef == "" || result.Mutation.Revision == 0 || requestKey == "" ||
		result.Mutation.ResultRef != result.Connection.ConnectionID {
		return api.ConnectionMutationResponse{}, errors.New("connection mutation returned incomplete durable metadata")
	}
	return api.ConnectionMutationResponse{
		Connection: connection,
		OutcomeReference: api.SafeOutcomeReference{
			Kind: api.SafeOutcomeReferenceKindConnection,
			Ref:  result.Mutation.ResultRef,
		},
		Replay:            result.Mutation.Replayed,
		ResultDisposition: disposition,
		ResultRevision:    strconv.FormatUint(result.Mutation.Revision, 10),
	}, nil
}

func mutationResponses(
	metadata app.MutationMetadata,
	resourceID string,
	kind api.SafeOutcomeReferenceKind,
	actor contract.Actor,
	disposition api.MutationResultDisposition,
) (api.MutationResultResponse, api.MutationReplayResponse, error) {
	if metadata.ResultRef == "" || metadata.Revision == 0 || resourceID == "" || metadata.ResultRef != resourceID {
		return api.MutationResultResponse{}, api.MutationReplayResponse{}, errors.New("identity mutation returned inconsistent durable metadata")
	}
	requested := actor.RequestedActor.PrincipalID
	effective := actor.EffectiveActor.PrincipalID
	outcome := api.SafeOutcomeReference{Kind: kind, Ref: metadata.ResultRef}
	fresh := api.MutationResultResponse{
		RequestedActorRef: &requested,
		EffectiveActorRef: &effective,
		OutcomeReference:  outcome,
		Replay:            api.MutationResultResponseReplay(false),
		ResourceId:        resourceID,
		ResultDisposition: disposition,
		ResultRevision:    strconv.FormatUint(metadata.Revision, 10),
	}
	replay := api.MutationReplayResponse{
		RequestedActorRef: &requested,
		EffectiveActorRef: &effective,
		OutcomeReference:  outcome,
		Replay:            api.MutationReplayResponseReplay(true),
		ResourceId:        resourceID,
		ResultDisposition: api.PriorMutationResult,
		ResultRevision:    strconv.FormatUint(metadata.Revision, 10),
	}
	return fresh, replay, nil
}

func contractScopeNone() contract.Scope { return contract.Scope{} }
