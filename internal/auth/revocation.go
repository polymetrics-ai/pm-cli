package auth

import (
	"context"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// RevokePrincipalLoginSessions revokes only short-lived PM login sessions in
// the caller-owned scoped transaction. It intentionally does not update runs,
// accepted execution grants, or reconciliation authority.
func (service *Service) RevokePrincipalLoginSessions(
	ctx context.Context,
	tx *store.Transaction,
	principal contract.Principal,
) error {
	if service == nil || tx == nil || rejectNilContext(ctx) != nil {
		return ErrInvalidRequest
	}
	if err := principal.Validate(); err != nil {
		return ErrInvalidRequest
	}
	if err := store.BindAuthDeployment(ctx, tx, service.deploymentID); err != nil {
		return err
	}
	return store.RevokePrincipalPMSessions(ctx, tx, service.deploymentID, principal)
}
