package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/credentials"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

const maximumCredentialIngressBytes = 65538

var errCredentialReauthenticationRequired = errors.New("recent signed authentication is required")

// CredentialIngressConfig contains resolved server controls, never request fields.
// The key loader transfers its returned bytes; ingress wipes them after use.
type CredentialIngressConfig struct {
	StepUpMaximumAge time.Duration
	WrappingKeyRef   string
	LoadWrappingKey  func(context.Context, string) ([]byte, error)
}

type CredentialIngress struct {
	identity      *IdentityService
	configuration CredentialIngressConfig
}

// CredentialIngressRequest is a private, write-only application boundary.
type CredentialIngressRequest struct {
	Identity       auth.AuthenticatedIdentity
	Scope          contract.Scope
	ConnectionID   string
	IdempotencyKey string
	Secret         []byte
	Scopes         []string
}

func (CredentialIngressRequest) String() string           { return "provider credential ingress [REDACTED]" }
func (request CredentialIngressRequest) GoString() string { return request.String() }

type StoredCredential struct {
	CredentialRef     string
	CiphertextVersion uint64
	LifecycleRevision uint64
}

func NewCredentialIngress(identity *IdentityService, configuration CredentialIngressConfig) (*CredentialIngress, error) {
	if identity == nil || identity.pool == nil || identity.authorize == nil ||
		configuration.StepUpMaximumAge < time.Second || configuration.StepUpMaximumAge > 15*time.Minute ||
		!validApplicationID(configuration.WrappingKeyRef) {
		return nil, errors.New("validated credential ingress dependencies are required")
	}
	return &CredentialIngress{identity: identity, configuration: configuration}, nil
}

// Store authorizes fresh browser authentication before resolving a wrapping key
// or touching ciphertext. It returns only the durable, safe mutation projection.
func (service *CredentialIngress) Store(ctx context.Context, request CredentialIngressRequest) (StoredCredential, error) {
	if service == nil || ctx == nil || !validApplicationID(request.ConnectionID) ||
		!validApplicationID(request.IdempotencyKey) || !validCredentialIngressInput(request.Secret, request.Scopes) {
		return StoredCredential{}, invalidRequestFailure()
	}
	if request.Identity.Source != auth.AuthenticationBrowserSession {
		return StoredCredential{}, auth.ErrAuthenticationRequired
	}
	credentialRef, err := service.resolveCredentialRef(ctx, request)
	if err != nil {
		return StoredCredential{}, credentialIngressFailure(err)
	}
	var transferredKey []byte
	resolveKey := func(ctx context.Context, keyRef string) ([]byte, error) {
		if service.configuration.LoadWrappingKey == nil {
			return nil, credentials.ErrWrappingKeyUnavailable
		}
		key, err := service.configuration.LoadWrappingKey(ctx, keyRef)
		if err != nil {
			clear(key)
			return nil, err
		}
		transferredKey = key
		return key, nil
	}
	defer func() { clear(transferredKey) }()
	authorize := func(ctx context.Context, tx *store.Transaction, access credentials.Access) error {
		if access.Action != credentials.ActionManage || access.Stage != credentials.StageMutation ||
			!access.Scope.Equal(request.Scope) || !access.Actor.Equal(request.Identity.Actor) ||
			access.Connection.ConnectionID != request.ConnectionID || access.Effect != nil {
			return auth.ErrAuthorizationDenied
		}
		now, err := store.ReadAuthenticationTime(ctx, tx)
		if err != nil {
			return err
		}
		if !recentCredentialAuthentication(request.Identity.AuthenticatedAt, service.configuration.StepUpMaximumAge, now) {
			return errCredentialReauthenticationRequired
		}
		_, err = service.identity.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:        access.Scope,
			Actor:        access.Actor,
			Permission:   "connection.credentials.manage",
			ResourceKind: auth.ResourceConnection,
			ResourceID:   request.ConnectionID,
			ConnectionID: request.ConnectionID,
		})
		return err
	}
	status, err := credentials.StoreEncryptedCredential(ctx, service.identity.pool, resolveKey, authorize, credentials.StoreRequest{
		Scope:          request.Scope,
		Actor:          request.Identity.Actor,
		ConnectionID:   request.ConnectionID,
		CredentialRef:  credentialRef,
		IdempotencyKey: request.IdempotencyKey,
		KeyRef:         service.configuration.WrappingKeyRef,
		Scopes:         slices.Clone(request.Scopes),
		AuthEpoch:      uint64(request.Identity.AuthenticatedAt.Unix()),
		Secret:         request.Secret,
	})
	if err != nil {
		return StoredCredential{}, credentialIngressFailure(err)
	}

	mutation, err := service.loadCredentialMutation(ctx, request)
	if err != nil {
		// Persistence already returned success. A later scope/read failure must
		// not be projected as a confirmed pre-effect refusal.
		return StoredCredential{}, errors.New("credential mutation response is unavailable")
	}
	if mutation.CredentialRef != status.CredentialRef || mutation.CiphertextVersion != status.CiphertextVersion ||
		mutation.Status != status.Status || mutation.Status != store.CredentialLifecycleActive ||
		mutation.CiphertextVersion > uint64(^uint64(0)>>1) || mutation.LifecycleRevision == 0 ||
		mutation.LifecycleRevision > uint64(^uint64(0)>>1) {
		return StoredCredential{}, errors.New("credential mutation returned inconsistent durable metadata")
	}
	return StoredCredential{
		CredentialRef:     mutation.CredentialRef,
		CiphertextVersion: mutation.CiphertextVersion,
		LifecycleRevision: mutation.LifecycleRevision,
	}, nil
}

func (service *CredentialIngress) resolveCredentialRef(ctx context.Context, request CredentialIngressRequest) (string, error) {
	var credentialRef string
	err := store.WithinScope(ctx, service.identity.pool, request.Scope, request.Identity.Actor, func(ctx context.Context, tx *store.Transaction) error {
		now, err := store.ReadAuthenticationTime(ctx, tx)
		if err != nil {
			return err
		}
		if !recentCredentialAuthentication(request.Identity.AuthenticatedAt, service.configuration.StepUpMaximumAge, now) {
			return errCredentialReauthenticationRequired
		}
		if _, err := service.identity.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:        request.Scope,
			Actor:        request.Identity.Actor,
			Permission:   "connection.credentials.manage",
			ResourceKind: auth.ResourceConnection,
			ResourceID:   request.ConnectionID,
			ConnectionID: request.ConnectionID,
		}); err != nil {
			return err
		}
		connection, err := store.LoadConnectionAuthority(ctx, tx, request.ConnectionID)
		if err != nil {
			return err
		}
		credentialRef = connection.CredentialRef
		return nil
	})
	if err != nil {
		return "", err
	}
	if credentialRef == "" {
		return "", store.ErrNotFound
	}
	return credentialRef, nil
}

func (service *CredentialIngress) loadCredentialMutation(ctx context.Context, request CredentialIngressRequest) (store.CredentialMutation, error) {
	var result store.CredentialMutation
	err := store.WithinScope(ctx, service.identity.pool, request.Scope, request.Identity.Actor, func(ctx context.Context, tx *store.Transaction) error {
		if _, err := service.identity.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:        request.Scope,
			Actor:        request.Identity.Actor,
			Permission:   "connection.credentials.manage",
			ResourceKind: auth.ResourceConnection,
			ResourceID:   request.ConnectionID,
			ConnectionID: request.ConnectionID,
		}); err != nil {
			return err
		}
		var err error
		result, err = store.LoadCredentialMutation(ctx, tx, store.CredentialMutationKey{
			Principal:      request.Identity.Actor.RequestedActor,
			ConnectionID:   request.ConnectionID,
			IdempotencyKey: request.IdempotencyKey,
		})
		return err
	})
	return result, err
}

func credentialIngressFailure(err error) error {
	switch {
	case errors.Is(err, errCredentialReauthenticationRequired):
		return &Failure{HTTPStatus: 403, Code: "reauthentication_required", RetryAction: "reauthenticate", cause: err}
	case errors.Is(err, credentials.ErrAuthorizationDenied), errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrScopeMismatch):
		return &Failure{HTTPStatus: 404, Code: "resource_not_found", RetryAction: "correct_scope", cause: err}
	case errors.Is(err, credentials.ErrWrappingKeyUnavailable):
		return &Failure{HTTPStatus: 503, Code: "credential_storage_unavailable", RetryAction: "after_capability_change", cause: err}
	case errors.Is(err, credentials.ErrCredentialStale), errors.Is(err, credentials.ErrCredentialUnavailable):
		return &Failure{HTTPStatus: 409, Code: "credential_revision_stale", RetryAction: "refresh_control_revision", cause: err}
	case errors.Is(err, credentials.ErrIdempotencyConflict):
		return &Failure{HTTPStatus: 409, Code: "idempotency_conflict", RetryAction: "new_key_for_new_intent", cause: err}
	default:
		return err
	}
}

func validCredentialIngressInput(secret []byte, scopes []string) bool {
	if len(secret) == 0 || len(secret) > maximumCredentialIngressBytes || scopes == nil || len(scopes) > 128 {
		return false
	}
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		if scope == "" || len(scope) > 512 || !utf8.ValidString(scope) || strings.TrimSpace(scope) != scope {
			return false
		}
		for _, character := range scope {
			if unicode.IsControl(character) {
				return false
			}
		}
		if _, duplicate := seen[scope]; duplicate {
			return false
		}
		seen[scope] = struct{}{}
	}
	return true
}

func recentCredentialAuthentication(authenticatedAt time.Time, maximumAge time.Duration, now time.Time) bool {
	if authenticatedAt.IsZero() || authenticatedAt.Location() != time.UTC || authenticatedAt.Unix() <= 0 ||
		authenticatedAt.After(now) || maximumAge <= 0 {
		return false
	}
	return now.Sub(authenticatedAt) <= maximumAge
}
