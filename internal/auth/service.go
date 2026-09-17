package auth

import (
	"context"
	"errors"
	"strings"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

const ServiceCredentialPrefix = "pm_sa"

func (service *Service) AuthenticateServiceCredential(ctx context.Context, credential string) (AuthenticatedIdentity, error) {
	if service == nil || service.pool == nil || rejectNilContext(ctx) != nil || !validOpaque(credential, maximumTokenBytes) {
		return AuthenticatedIdentity{}, ErrAuthenticationRequired
	}
	prefix, remainder, found := strings.Cut(credential, ".")
	if !found || prefix != ServiceCredentialPrefix {
		return AuthenticatedIdentity{}, ErrAuthenticationRequired
	}
	credentialID, secret, found := strings.Cut(remainder, ".")
	if !found || strings.Contains(secret, ".") ||
		!validOpaque(credentialID, 512) || !validOpaque(secret, maximumTokenBytes) {
		return AuthenticatedIdentity{}, ErrAuthenticationRequired
	}
	var record store.ServiceCredentialRecord
	err := store.WithinAuthDeployment(ctx, service.pool, service.deploymentID, func(ctx context.Context, tx *store.Transaction) error {
		var loadErr error
		record, loadErr = store.LoadServiceCredential(
			ctx,
			tx,
			service.deploymentID,
			credentialID,
			sha256Bytes(credential),
		)
		if loadErr != nil {
			return loadErr
		}
		now, timeErr := store.ReadAuthenticationTime(ctx, tx)
		if timeErr != nil {
			return timeErr
		}
		if record.RevokedAt != nil || !record.ExpiresAt.After(now) {
			return store.ErrNotFound
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return AuthenticatedIdentity{}, ErrAuthenticationRequired
		}
		return AuthenticatedIdentity{}, err
	}
	principal := contract.Principal{
		OrgID:       record.Scope.OrgID,
		PrincipalID: record.ServiceAccountID,
		Kind:        contract.PrincipalServiceAccount,
	}
	return AuthenticatedIdentity{
		Principal:    principal,
		Actor:        directActor(principal),
		Source:       AuthenticationServiceCredential,
		CredentialID: record.CredentialID,
		ExpiresAt:    record.ExpiresAt,
	}, nil
}
