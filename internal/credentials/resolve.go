package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

type ResolveRequest struct {
	Scope   contract.Scope
	Actor   contract.Actor
	Binding contract.CredentialRef
	Effect  EffectAccess
}

func (request ResolveRequest) validate(now time.Time) error {
	if err := request.Scope.Validate(); err != nil {
		return err
	}
	if err := request.Actor.Validate(); err != nil {
		return err
	}
	if request.Actor.RequestedActor.OrgID != request.Scope.OrgID ||
		request.Actor.EffectiveActor.OrgID != request.Scope.OrgID {
		return store.ErrScopeMismatch
	}
	if err := request.Binding.Validate(); err != nil {
		return err
	}
	if request.Binding.WorkspaceID != request.Scope.WorkspaceID {
		return store.ErrScopeMismatch
	}
	return request.Effect.validate(now)
}

// CredentialMaterial is a one-use, short-lived server-only capability. Use
// transfers the bytes only for the duration of its callback, wipes them
// afterward, and refuses reuse, use after the admitted effect deadline, or use
// after Destroy. All string and JSON projections are redacted.
type CredentialMaterial interface {
	CredentialBinding() contract.CredentialRef
	Use(func([]byte) error) error
	Destroy()
}

type credentialMaterial struct {
	mu        sync.Mutex
	binding   contract.CredentialRef
	secret    []byte
	deadline  time.Time
	destroyed bool
}

func newCredentialMaterial(binding contract.CredentialRef, secret []byte, deadline time.Time) *credentialMaterial {
	binding.Scopes = slices.Clone(binding.Scopes)
	return &credentialMaterial{binding: binding, secret: secret, deadline: deadline}
}

func (material *credentialMaterial) CredentialBinding() contract.CredentialRef {
	material.mu.Lock()
	defer material.mu.Unlock()
	binding := material.binding
	binding.Scopes = slices.Clone(binding.Scopes)
	return binding
}

func (material *credentialMaterial) Use(consume func([]byte) error) error {
	if consume == nil {
		return errors.New("credential material consumer is required")
	}
	material.mu.Lock()
	if material.destroyed || len(material.secret) == 0 {
		material.mu.Unlock()
		return ErrCredentialUnavailable
	}
	if !time.Now().Before(material.deadline) {
		material.mu.Unlock()
		return ErrEffectExpired
	}
	secret := material.secret
	material.secret = nil
	material.destroyed = true
	material.mu.Unlock()
	defer wipe(secret)
	return consume(secret)
}

func (material *credentialMaterial) Destroy() {
	if material == nil {
		return
	}
	material.mu.Lock()
	defer material.mu.Unlock()
	if material.destroyed {
		return
	}
	wipe(material.secret)
	material.secret = nil
	material.destroyed = true
}

func (material *credentialMaterial) String() string {
	return "credential material [REDACTED]"
}

func (material *credentialMaterial) GoString() string {
	return material.String()
}

func (material *credentialMaterial) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Credential string `json:"credential"`
	}{Credential: "[REDACTED]"})
}

// ResolveForEffect rechecks the current connection, provider-account witness,
// credential version and explicit admitted effect inside one scoped
// transaction. Ciphertext and wrapping-key resolution happen only after the
// authorization callback succeeds.
func ResolveForEffect(
	ctx context.Context,
	pool *store.Pool,
	resolveKey WrappingKeyResolver,
	authorize AuthorizeFunc,
	request ResolveRequest,
) (CredentialMaterial, error) {
	if ctx == nil {
		return nil, errors.New("credential resolution context is required")
	}
	if pool == nil {
		return nil, errors.New("credential resolution pool is required")
	}
	if resolveKey == nil {
		return nil, errors.New("credential wrapping-key resolver is required")
	}
	if authorize == nil {
		return nil, errors.New("credential authorization callback is required")
	}
	if err := request.validate(time.Now()); err != nil {
		return nil, err
	}

	var material *credentialMaterial
	err := store.WithinScope(ctx, pool, request.Scope, request.Actor, func(ctx context.Context, tx *store.Transaction) error {
		locked, err := store.LockCredentialLifecycle(ctx, tx, request.Binding.ConnectionID)
		if err != nil {
			return err
		}
		if err := currentBinding(locked, request.Binding); err != nil {
			return err
		}
		effect := request.Effect
		access := accessForLock(ActionResolveEffect, StagePreEffect, request.Scope, request.Actor, locked, request.Binding.Scopes, &effect)
		if err := callAuthorize(ctx, tx, authorize, access); err != nil {
			return err
		}

		encrypted, err := store.LoadEncryptedSecret(ctx, tx, request.Binding.CredentialRef, request.Binding.CiphertextVersion)
		if err != nil {
			return err
		}
		if encrypted.ProviderAccountID != locked.Account.AccountID ||
			!sameCredentialBinding(encrypted.Binding, request.Binding) {
			return ErrCredentialStale
		}
		key, err := resolveWrappingKey(ctx, resolveKey, encrypted.Binding.KeyRef)
		if err != nil {
			return err
		}
		defer wipe(key)
		plaintext, err := openCredential(key, encrypted)
		if err != nil {
			return err
		}
		material = newCredentialMaterial(encrypted.Binding, plaintext, request.Effect.Deadline)
		return nil
	})
	if err != nil {
		if material != nil {
			material.Destroy()
		}
		return nil, err
	}
	return material, nil
}
