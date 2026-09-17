package app

import (
	"context"

	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// AuthorizeArtifactRead establishes current scoped artifact authority without
// claiming that an object-byte backend is installed or reachable.
func (service *IdentityService) AuthorizeArtifactRead(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	artifactID string,
) error {
	if service == nil || ctx == nil || !validApplicationID(artifactID) {
		return invalidRequestFailure()
	}
	return store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		_, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:           scope,
			Actor:           actor,
			Permission:      "artifact.read",
			ResourceKind:    auth.ResourceArtifact,
			ResourceID:      artifactID,
			AutomationScope: actor.RequestedActor.Kind == contract.PrincipalServiceAccount,
		})
		return mapAuthorizationFailure(err)
	})
}

// AuthorizeArtifactWrite establishes current workspace authority before an
// upload capability is consulted. Actual artifact bytes remain backend-owned.
func (service *IdentityService) AuthorizeArtifactWrite(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
) error {
	if service == nil || ctx == nil {
		return invalidRequestFailure()
	}
	return store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		_, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:           scope,
			Actor:           actor,
			Permission:      "artifact.write",
			ResourceKind:    auth.ResourceWorkspace,
			ResourceID:      scope.WorkspaceID,
			AutomationScope: actor.RequestedActor.Kind == contract.PrincipalServiceAccount,
		})
		return mapAuthorizationFailure(err)
	})
}
