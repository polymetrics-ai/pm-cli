package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"strconv"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

type ServiceAccount struct {
	ServiceAccountID string
	Principal        contract.Principal
	DisplayName      *string
	WorkspaceScopes  []string
	ConnectionIDs    []string
	OperationIDs     []string
	GrantRevision    string
	PolicyEpoch      uint64
	ControlRevision  uint64
	ExpiresAt        time.Time
	RevokedAt        *time.Time
}

type ServiceAccountPage struct {
	Items              []ServiceAccount
	CollectionRevision uint64
	NextCursor         string
}

type CreateServiceAccount struct {
	DisplayName     *string
	WorkspaceScopes []string
	ConnectionIDs   []string
	OperationIDs    []string
	ExpiresAt       time.Time
	Control         MutationControl
}

type RevokeServiceAccount struct {
	ServiceAccountID string
	GrantRevision    string
	ExpiresAt        time.Time
	Control          MutationControl
}

type ServiceAccountMutationResult struct {
	ServiceAccount    ServiceAccount
	Mutation          MutationMetadata
	OneTimeCredential string `json:"-"`
}

func (service *IdentityService) ListServiceAccounts(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	afterID string,
	pageSize uint32,
) (ServiceAccountPage, error) {
	if service == nil || ctx == nil || pageSize == 0 || pageSize > 100 {
		return ServiceAccountPage{}, invalidRequestFailure()
	}
	var result ServiceAccountPage
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
		items, revisions, err := store.ListServiceAccountAuthorities(ctx, tx, afterID, pageSize+1)
		if err != nil {
			return err
		}
		result.CollectionRevision = revisions.ServiceAccounts
		result.Items = make([]ServiceAccount, 0, min(len(items), int(pageSize)))
		for index, item := range items {
			if index == int(pageSize) {
				result.NextCursor = result.Items[len(result.Items)-1].ServiceAccountID
				break
			}
			result.Items = append(result.Items, serviceAccountFromStore(item))
		}
		return nil
	})
	return result, err
}

func (service *IdentityService) CreateServiceAccount(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	request CreateServiceAccount,
) (ServiceAccountMutationResult, error) {
	if service == nil || ctx == nil {
		return ServiceAccountMutationResult{}, invalidRequestFailure()
	}
	var result ServiceAccountMutationResult
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		authorization, err := service.bindAndAuthorizeAccessMutation(ctx, tx, actor, scope)
		if err != nil {
			return err
		}
		intentRevision, err := validateMutationControl(request.Control)
		if err != nil {
			return err
		}
		if !validOptionalDisplayName(request.DisplayName) ||
			!validWorkspaceScopes(scope, request.WorkspaceScopes) ||
			!validSortedIDs(request.ConnectionIDs) ||
			!validSortedOperationIDs(request.OperationIDs) || request.ExpiresAt.IsZero() ||
			request.ExpiresAt.Location() != time.UTC {
			return invalidRequestFailure()
		}
		if !authorization.ContainsGrant(
			nil,
			request.ConnectionIDs,
			request.OperationIDs,
			&request.ExpiresAt,
		) {
			return mapAuthorizationFailure(auth.ErrResourceNotFound)
		}
		digest, err := canonicalIntent(struct {
			Operation       string         `json:"operation"`
			Scope           contract.Scope `json:"scope"`
			DisplayName     *string        `json:"display_name"`
			WorkspaceScopes []string       `json:"workspace_scopes"`
			ConnectionIDs   []string       `json:"connection_ids"`
			OperationIDs    []string       `json:"operation_ids"`
			ExpiresAt       time.Time      `json:"expires_at"`
			IntentRevision  uint64         `json:"expected_revision"`
		}{
			Operation:       "service_account.create",
			Scope:           scope,
			DisplayName:     request.DisplayName,
			WorkspaceScopes: request.WorkspaceScopes,
			ConnectionIDs:   request.ConnectionIDs,
			OperationIDs:    request.OperationIDs,
			ExpiresAt:       request.ExpiresAt,
			IntentRevision:  intentRevision,
		})
		if err != nil {
			return invalidRequestFailure()
		}
		key := mutationReceiptKey(scope, actor, "service_account.create", "service_accounts", request.Control.IdempotencyKey)
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
			result.OneTimeCredential = ""
			return nil
		}
		if err := requireFreshMutationPrecondition(request.Control); err != nil {
			return err
		}
		serviceAccountID, err := randomApplicationID(service.random, "service-", 18)
		if err != nil {
			return err
		}
		credentialID, err := randomApplicationID(service.random, "credential-", 18)
		if err != nil {
			return err
		}
		secret, err := randomApplicationID(service.random, "secret-", 32)
		if err != nil {
			return err
		}
		credential := auth.ServiceCredentialPrefix + "." + credentialID + "." + secret
		credentialHash := sha256.Sum256([]byte(credential))
		stored, collectionRevision, err := store.CreateServiceAccount(ctx, tx, store.CreateServiceAccountRequest{
			DeploymentID:               service.deploymentID,
			ServiceAccountID:           serviceAccountID,
			DisplayName:                request.DisplayName,
			CredentialID:               credentialID,
			CredentialHash:             credentialHash[:],
			Scope:                      scope,
			Permissions:                []string{},
			ConnectionIDs:              request.ConnectionIDs,
			OperationIDs:               request.OperationIDs,
			ExpiresAt:                  request.ExpiresAt,
			ExpectedCollectionRevision: intentRevision,
		})
		if err != nil {
			return mapMutationFailure(err)
		}
		result = ServiceAccountMutationResult{
			ServiceAccount: serviceAccountFromStore(stored),
			Mutation: MutationMetadata{
				ResultRef:       serviceAccountID,
				Revision:        collectionRevision,
				ControlRevision: stored.ControlRevision,
				Status:          StatusCreated,
			},
			OneTimeCredential: credential,
		}
		return service.finishMutation(ctx, tx, key, result.Mutation, result)
	})
	return result, err
}

func (service *IdentityService) RevokeServiceAccount(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	request RevokeServiceAccount,
) (ServiceAccountMutationResult, error) {
	if service == nil || ctx == nil || !validApplicationID(request.ServiceAccountID) {
		return ServiceAccountMutationResult{}, invalidRequestFailure()
	}
	var result ServiceAccountMutationResult
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		if _, err := service.bindAndAuthorizeAccessMutation(ctx, tx, actor, scope); err != nil {
			return err
		}
		intentRevision, err := validateMutationControl(request.Control)
		if err != nil {
			return err
		}
		if !validApplicationID(request.GrantRevision) ||
			request.ExpiresAt.IsZero() || request.ExpiresAt.Location() != time.UTC {
			return invalidRequestFailure()
		}
		digest, err := canonicalIntent(struct {
			Operation        string         `json:"operation"`
			Scope            contract.Scope `json:"scope"`
			ServiceAccountID string         `json:"service_account_id"`
			GrantRevision    string         `json:"grant_revision"`
			ExpiresAt        time.Time      `json:"expires_at"`
			IntentRevision   uint64         `json:"expected_revision"`
		}{
			Operation:        "service_account.revoke",
			Scope:            scope,
			ServiceAccountID: request.ServiceAccountID,
			GrantRevision:    request.GrantRevision,
			ExpiresAt:        request.ExpiresAt,
			IntentRevision:   intentRevision,
		})
		if err != nil {
			return invalidRequestFailure()
		}
		key := mutationReceiptKey(scope, actor, "service_account.revoke", request.ServiceAccountID, request.Control.IdempotencyKey)
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
			result.OneTimeCredential = ""
			return nil
		}
		if err := requireFreshMutationPrecondition(request.Control); err != nil {
			return err
		}
		if request.GrantRevision != strconv.FormatUint(intentRevision, 10) {
			return invalidRequestFailure()
		}
		current, err := store.LoadServiceAccountAuthority(ctx, tx, request.ServiceAccountID)
		if err != nil {
			return mapMutationFailure(err)
		}
		if !current.ExpiresAt.Equal(request.ExpiresAt) {
			return mapMutationFailure(store.ErrCASConflict)
		}
		stored, unchanged, err := store.RevokeServiceAccount(ctx, tx, store.RevokeServiceAccountRequest{
			Scope:            scope,
			ServiceAccountID: current.ServiceAccountID,
			Permissions:      current.Permissions,
			ConnectionIDs:    current.ConnectionIDs,
			OperationIDs:     current.OperationIDs,
			ExpiresAt:        current.ExpiresAt,
			ExpectedRevision: intentRevision,
		})
		if err != nil {
			return mapMutationFailure(err)
		}
		result = ServiceAccountMutationResult{
			ServiceAccount: serviceAccountFromStore(stored),
			Mutation: MutationMetadata{
				ResultRef:       request.ServiceAccountID,
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

func serviceAccountFromStore(value store.ServiceAccountAuthority) ServiceAccount {
	return ServiceAccount{
		ServiceAccountID: value.ServiceAccountID,
		Principal:        value.Principal,
		DisplayName:      copyString(value.DisplayName),
		WorkspaceScopes:  []string{value.Scope.WorkspaceID},
		ConnectionIDs:    append([]string(nil), value.ConnectionIDs...),
		OperationIDs:     append([]string(nil), value.OperationIDs...),
		GrantRevision:    strconv.FormatUint(value.ControlRevision, 10),
		PolicyEpoch:      value.PolicyEpoch,
		ControlRevision:  value.ControlRevision,
		ExpiresAt:        value.ExpiresAt,
		RevokedAt:        copyTime(value.RevokedAt),
	}
}

func IsOneTimeCredentialUnavailable(err error) bool {
	var failure *Failure
	return errors.As(err, &failure) && failure.Code == "retry_receipt_expired"
}
