package httpapi

import (
	"context"

	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
)

func (service *Service) AdminStoreCredential(ctx context.Context, request AdminStoreCredentialRequestObject) (AdminStoreCredentialResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if request.Body == nil || state.identity.Source != auth.AuthenticationBrowserSession {
		return asProblemResponse(auth.ErrAuthenticationRequired, nil), nil
	}
	secret := request.Body.CredentialValue
	request.Body.CredentialValue = nil
	defer clear(secret)
	stored, err := service.credentials.Store(ctx, app.CredentialIngressRequest{
		Identity:       state.identity,
		Scope:          scope,
		ConnectionID:   request.ConnectionId,
		IdempotencyKey: request.Params.IdempotencyKey,
		Secret:         secret,
		Scopes:         request.Body.Scopes,
	})
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	setETag(ctx, stored.LifecycleRevision)
	return AdminStoreCredential200JSONResponse(AdminCredentialStatus{
		CiphertextVersion: int64(stored.CiphertextVersion),
		CredentialRef:     stored.CredentialRef,
		LifecycleRevision: int64(stored.LifecycleRevision),
		Status:            AdminCredentialActive,
	}), nil
}
