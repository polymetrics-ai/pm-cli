package app

import (
	"context"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

type Connection struct {
	Scope             contract.Scope
	ConnectionID      string
	ConnectorID       string
	ProviderAccountID string
	CredentialRef     string
	DisplayName       string
	State             string
	Revision          uint64
	RevokedAt         *time.Time
}

type ConnectionPage struct {
	Items              []Connection
	CollectionRevision uint64
	NextCursor         string
}

type CreateConnection struct {
	ConnectorID       string
	ProviderAccountID string
	DisplayName       string
	Control           MutationControl
}

type UpdateConnection struct {
	ConnectionID string
	DisplayName  string
	Control      MutationControl
}

type DeleteConnection struct {
	ConnectionID string
	Control      MutationControl
}

type ConnectionMutationResult struct {
	Connection Connection
	Mutation   MutationMetadata
}

func (service *IdentityService) ListConnections(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	afterID string,
	pageSize uint32,
) (ConnectionPage, error) {
	if service == nil || ctx == nil || pageSize == 0 || pageSize > 100 {
		return ConnectionPage{}, invalidRequestFailure()
	}
	var result ConnectionPage
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		authorization, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:        scope,
			Actor:        actor,
			Permission:   "connection.read",
			ResourceKind: auth.ResourceWorkspace,
			ResourceID:   scope.WorkspaceID,
		})
		if err != nil {
			return mapAuthorizationFailure(err)
		}
		items, revisions, err := store.ListConnectionAuthorities(ctx, tx, authorization.ConnectionIDs, afterID, pageSize+1)
		if err != nil {
			return err
		}
		result.CollectionRevision = revisions.Connections
		result.Items = make([]Connection, 0, min(len(items), int(pageSize)))
		for index, item := range items {
			if index == int(pageSize) {
				result.NextCursor = result.Items[len(result.Items)-1].ConnectionID
				break
			}
			result.Items = append(result.Items, connectionFromStore(item))
		}
		return nil
	})
	return result, err
}

func (service *IdentityService) InspectConnection(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	connectionID string,
) (Connection, error) {
	if service == nil || ctx == nil || !validApplicationID(connectionID) {
		return Connection{}, invalidRequestFailure()
	}
	var result Connection
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		if _, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:        scope,
			Actor:        actor,
			Permission:   "connection.read",
			ResourceKind: auth.ResourceConnection,
			ResourceID:   connectionID,
		}); err != nil {
			return mapAuthorizationFailure(err)
		}
		stored, err := store.LoadConnectionAuthority(ctx, tx, connectionID)
		if err != nil {
			return err
		}
		result = connectionFromStore(stored)
		return nil
	})
	return result, err
}

func (service *IdentityService) CreateConnection(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	request CreateConnection,
) (ConnectionMutationResult, error) {
	if service == nil || ctx == nil {
		return ConnectionMutationResult{}, invalidRequestFailure()
	}
	var result ConnectionMutationResult
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		if err := store.BindAuthDeployment(ctx, tx, service.deploymentID); err != nil {
			return err
		}
		if _, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:        scope,
			Actor:        actor,
			Permission:   "connection.manage",
			ResourceKind: auth.ResourceWorkspace,
			ResourceID:   scope.WorkspaceID,
		}); err != nil {
			return mapAuthorizationFailure(err)
		}
		intentRevision, err := validateMutationControl(request.Control)
		if err != nil {
			return err
		}
		if !validApplicationID(request.ConnectorID) || !validApplicationID(request.ProviderAccountID) ||
			!validDisplayName(request.DisplayName) {
			return invalidRequestFailure()
		}
		digest, err := canonicalIntent(struct {
			Operation         string         `json:"operation"`
			Scope             contract.Scope `json:"scope"`
			ConnectorID       string         `json:"connector_id"`
			ProviderAccountID string         `json:"provider_account_id"`
			DisplayName       string         `json:"display_name"`
			IntentRevision    uint64         `json:"expected_revision"`
		}{
			Operation:         "connection.create",
			Scope:             scope,
			ConnectorID:       request.ConnectorID,
			ProviderAccountID: request.ProviderAccountID,
			DisplayName:       request.DisplayName,
			IntentRevision:    intentRevision,
		})
		if err != nil {
			return invalidRequestFailure()
		}
		key := mutationReceiptKey(scope, actor, "connection.create", "connections", request.Control.IdempotencyKey)
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
		connectionID, err := randomApplicationID(service.random, "connection-", 18)
		if err != nil {
			return err
		}
		credentialRef, err := randomApplicationID(service.random, "credential-", 18)
		if err != nil {
			return err
		}
		stored, collectionRevision, err := store.CreateConnectionControl(ctx, tx, store.CreateConnectionRequest{
			Scope:                      scope,
			ConnectionID:               connectionID,
			ConnectorID:                request.ConnectorID,
			ProviderAccountID:          request.ProviderAccountID,
			CredentialRef:              credentialRef,
			DisplayName:                request.DisplayName,
			ExpectedCollectionRevision: intentRevision,
		})
		if err != nil {
			return mapMutationFailure(err)
		}
		result = ConnectionMutationResult{
			Connection: connectionFromStore(stored),
			Mutation: MutationMetadata{
				ResultRef:       connectionID,
				Revision:        collectionRevision,
				ControlRevision: stored.Revision,
				Status:          StatusCreated,
			},
		}
		return service.finishMutation(ctx, tx, key, result.Mutation, result)
	})
	return result, err
}

func (service *IdentityService) UpdateConnection(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	request UpdateConnection,
) (ConnectionMutationResult, error) {
	if service == nil || ctx == nil || !validApplicationID(request.ConnectionID) {
		return ConnectionMutationResult{}, invalidRequestFailure()
	}
	var result ConnectionMutationResult
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		if err := store.BindAuthDeployment(ctx, tx, service.deploymentID); err != nil {
			return err
		}
		if _, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:        scope,
			Actor:        actor,
			Permission:   "connection.manage",
			ResourceKind: auth.ResourceConnection,
			ResourceID:   request.ConnectionID,
		}); err != nil {
			return mapAuthorizationFailure(err)
		}
		intentRevision, err := validateMutationControl(request.Control)
		if err != nil {
			return err
		}
		if !validDisplayName(request.DisplayName) {
			return invalidRequestFailure()
		}
		digest, err := canonicalIntent(struct {
			Operation      string         `json:"operation"`
			Scope          contract.Scope `json:"scope"`
			ConnectionID   string         `json:"connection_id"`
			DisplayName    string         `json:"display_name"`
			IntentRevision uint64         `json:"expected_revision"`
		}{
			Operation:      "connection.update",
			Scope:          scope,
			ConnectionID:   request.ConnectionID,
			DisplayName:    request.DisplayName,
			IntentRevision: intentRevision,
		})
		if err != nil {
			return invalidRequestFailure()
		}
		key := mutationReceiptKey(scope, actor, "connection.update", request.ConnectionID, request.Control.IdempotencyKey)
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
		current, err := store.LoadConnectionAuthority(ctx, tx, request.ConnectionID)
		if err != nil {
			return mapMutationFailure(err)
		}
		stored, unchanged, err := store.UpdateConnectionControl(ctx, tx, store.UpdateConnectionRequest{
			ConnectionID:     request.ConnectionID,
			CredentialRef:    current.CredentialRef,
			DisplayName:      request.DisplayName,
			ExpectedRevision: intentRevision,
		})
		if err != nil {
			return mapMutationFailure(err)
		}
		result = ConnectionMutationResult{
			Connection: connectionFromStore(stored),
			Mutation: MutationMetadata{
				ResultRef:       request.ConnectionID,
				Revision:        stored.Revision,
				ControlRevision: stored.Revision,
				Status:          StatusOK,
				Unchanged:       unchanged,
			},
		}
		return service.finishMutation(ctx, tx, key, result.Mutation, result)
	})
	return result, err
}

func (service *IdentityService) DeleteConnection(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	request DeleteConnection,
) (ConnectionMutationResult, error) {
	if service == nil || ctx == nil || !validApplicationID(request.ConnectionID) {
		return ConnectionMutationResult{}, invalidRequestFailure()
	}
	var result ConnectionMutationResult
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		if err := store.BindAuthDeployment(ctx, tx, service.deploymentID); err != nil {
			return err
		}
		if _, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:        scope,
			Actor:        actor,
			Permission:   "connection.manage",
			ResourceKind: auth.ResourceConnection,
			ResourceID:   request.ConnectionID,
		}); err != nil {
			return mapAuthorizationFailure(err)
		}
		intentRevision, err := validateMutationControl(request.Control)
		if err != nil {
			return err
		}
		digest, err := canonicalIntent(struct {
			Operation      string         `json:"operation"`
			Scope          contract.Scope `json:"scope"`
			ConnectionID   string         `json:"connection_id"`
			IntentRevision uint64         `json:"expected_revision"`
		}{"connection.delete", scope, request.ConnectionID, intentRevision})
		if err != nil {
			return invalidRequestFailure()
		}
		key := mutationReceiptKey(scope, actor, "connection.delete", request.ConnectionID, request.Control.IdempotencyKey)
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
		stored, unchanged, err := store.RevokeConnectionControl(ctx, tx, request.ConnectionID, intentRevision)
		if err != nil {
			return mapMutationFailure(err)
		}
		result = ConnectionMutationResult{
			Connection: connectionFromStore(stored),
			Mutation: MutationMetadata{
				ResultRef:       request.ConnectionID,
				Revision:        stored.Revision,
				ControlRevision: stored.Revision,
				Status:          StatusOK,
				Unchanged:       unchanged,
			},
		}
		return service.finishMutation(ctx, tx, key, result.Mutation, result)
	})
	return result, err
}

func connectionFromStore(value store.ConnectionAuthority) Connection {
	return Connection{
		Scope:             value.Scope,
		ConnectionID:      value.ConnectionID,
		ConnectorID:       value.ConnectorID,
		ProviderAccountID: value.ProviderAccountID,
		CredentialRef:     value.CredentialRef,
		DisplayName:       value.DisplayName,
		State:             value.State,
		Revision:          value.Revision,
		RevokedAt:         copyTime(value.RevokedAt),
	}
}
