package credentials

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

const (
	credentialEnvelopeVersion = byte(1)
	wrappingKeyBytes          = 32
	maxCredentialBytes        = (1 << 20) - 64
	maxDatabaseBigint         = uint64(1<<63 - 1)
)

var (
	ErrAuthorizationDenied         = errors.New("credential authorization denied")
	ErrWrappingKeyUnavailable      = errors.New("credential wrapping key unavailable")
	ErrCredentialUnavailable       = errors.New("credential unavailable")
	ErrCredentialStale             = errors.New("credential binding is stale")
	ErrCredentialRefreshInProgress = errors.New("credential refresh is in progress")
	ErrReauthorizationRequired     = errors.New("credential reauthorization required")
	ErrIdempotencyConflict         = store.ErrIdempotencyConflict
	ErrEffectExpired               = errors.New("credential effect authority expired")
	ErrRefreshBusy                 = errors.New("credential refresh is already in progress")
	ErrRefreshRejected             = errors.New("credential refresh was rejected")
	ErrRefreshOutcomeUncertain     = errors.New("credential refresh outcome is uncertain")
	ErrRefreshLeaseStale           = errors.New("credential refresh lease is stale")
)

type Action string

const (
	ActionManage        Action = "manage"
	ActionResolveEffect Action = "resolve_effect"
	ActionRefresh       Action = "refresh"
)

type AuthorizationStage string

const (
	StageMutation  AuthorizationStage = "mutation"
	StagePreEffect AuthorizationStage = "pre_effect"
	StagePersist   AuthorizationStage = "persist"
)

// EffectAccess identifies the one admitted physical effect for which a secret
// may be resolved. It deliberately carries references only, never provider
// inputs, headers, bodies, or credential material.
type EffectAccess struct {
	ExecutionGrantRef  string
	OperationID        string
	OperationRevision  string
	EffectRef          string
	DispatchID         string
	AttemptID          string
	PermitRef          string
	ApprovalRef        string
	Deadline           time.Time
	ReconciliationOnly bool
}

func (effect EffectAccess) validate(now time.Time) error {
	for _, value := range []string{
		effect.ExecutionGrantRef,
		effect.OperationID,
		effect.OperationRevision,
		effect.EffectRef,
		effect.DispatchID,
		effect.AttemptID,
		effect.PermitRef,
	} {
		if !validOpaque(value) {
			return errors.New("credential effect reference is invalid")
		}
	}
	if effect.ApprovalRef != "" && !validOpaque(effect.ApprovalRef) {
		return errors.New("credential effect approval reference is invalid")
	}
	if effect.Deadline.IsZero() || !effect.Deadline.After(now) {
		return ErrEffectExpired
	}
	return nil
}

type CredentialAccess struct {
	CredentialRef           string
	BoundProviderAccountID  string
	BoundAccountWitness     string
	BoundConnectionRevision uint64
	BoundAccountRevision    uint64
	CiphertextVersion       uint64
	Status                  store.CredentialLifecycleState
	AuthEpoch               uint64
	RequestedScopes         []string
}

type Access struct {
	Action          Action
	Stage           AuthorizationStage
	Scope           contract.Scope
	Actor           contract.Actor
	Connection      store.Connection
	ConnectionState string
	Account         store.ProviderAccount
	Credential      CredentialAccess
	Effect          *EffectAccess
}

// AuthorizeFunc must decide current authority using the supplied caller-owned
// scoped transaction. A nil callback is rejected. Any callback error is mapped
// to ErrAuthorizationDenied so private policy or secret-bearing diagnostics do
// not cross this package boundary.
type AuthorizeFunc func(context.Context, *store.Transaction, Access) error

// WrappingKeyResolver resolves an operator-owned AES-256 wrapping key by its
// versioned reference. The resolver retains ownership of returned bytes; this
// package clones and wipes its working copy. Errors and invalid key lengths are
// exposed only as ErrWrappingKeyUnavailable.
type WrappingKeyResolver func(context.Context, string) ([]byte, error)

type StoreRequest struct {
	Scope          contract.Scope `json:"-"`
	Actor          contract.Actor `json:"-"`
	ConnectionID   string         `json:"connection_id"`
	CredentialRef  string         `json:"credential_ref"`
	IdempotencyKey string         `json:"idempotency_key"`
	KeyRef         string         `json:"-"`
	Scopes         []string       `json:"-"`
	AuthEpoch      uint64         `json:"auth_epoch"`
	Secret         []byte         `json:"-"`
}

func (request StoreRequest) String() string {
	return "credential store request [REDACTED]"
}

func (request StoreRequest) GoString() string {
	return request.String()
}

func (request StoreRequest) validate() error {
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
	for _, value := range []string{
		request.ConnectionID,
		request.CredentialRef,
		request.IdempotencyKey,
		request.KeyRef,
	} {
		if !validOpaque(value) {
			return errors.New("credential store request identity is invalid")
		}
	}
	if request.Scopes == nil {
		return errors.New("credential scopes must be explicitly present")
	}
	seen := make(map[string]struct{}, len(request.Scopes))
	for _, scope := range request.Scopes {
		if !validOpaque(scope) {
			return errors.New("credential scope is invalid")
		}
		if _, duplicate := seen[scope]; duplicate {
			return errors.New("credential scope is duplicated")
		}
		seen[scope] = struct{}{}
	}
	if request.AuthEpoch == 0 || request.AuthEpoch > maxDatabaseBigint {
		return errors.New("credential authorization epoch is invalid")
	}
	if len(request.Secret) == 0 || len(request.Secret) > maxCredentialBytes {
		return errors.New("credential secret is outside the allowed bound")
	}
	return nil
}

type CredentialStatus struct {
	CredentialRef     string                         `json:"credential_ref"`
	CiphertextVersion uint64                         `json:"ciphertext_version"`
	Status            store.CredentialLifecycleState `json:"status"`
	Replayed          bool                           `json:"replayed"`
}

func statusFromLifecycle(lifecycle store.CredentialLifecycle, replayed bool) CredentialStatus {
	return CredentialStatus{
		CredentialRef:     lifecycle.CredentialRef,
		CiphertextVersion: lifecycle.CiphertextVersion,
		Status:            lifecycle.Status,
		Replayed:          replayed,
	}
}

func statusFromMutation(mutation store.CredentialMutation) CredentialStatus {
	return CredentialStatus{
		CredentialRef:     mutation.CredentialRef,
		CiphertextVersion: mutation.CiphertextVersion,
		Status:            mutation.Status,
		Replayed:          true,
	}
}

// StoreEncryptedCredential encrypts one write-only admin ingress and atomically
// records its immutable ciphertext version, current lifecycle, and safe replay
// receipt. Authorization and wrapping-key availability precede every write.
func StoreEncryptedCredential(
	ctx context.Context,
	pool *store.Pool,
	resolveKey WrappingKeyResolver,
	authorize AuthorizeFunc,
	request StoreRequest,
) (CredentialStatus, error) {
	if ctx == nil {
		return CredentialStatus{}, errors.New("credential store context is required")
	}
	if pool == nil {
		return CredentialStatus{}, errors.New("credential store pool is required")
	}
	if resolveKey == nil {
		return CredentialStatus{}, errors.New("credential wrapping-key resolver is required")
	}
	if authorize == nil {
		return CredentialStatus{}, errors.New("credential authorization callback is required")
	}
	if err := request.validate(); err != nil {
		return CredentialStatus{}, err
	}

	secret := bytes.Clone(request.Secret)
	defer wipe(secret)
	var result CredentialStatus
	err := store.WithinScope(ctx, pool, request.Scope, request.Actor, func(ctx context.Context, tx *store.Transaction) error {
		locked, err := store.LockCredentialLifecycle(ctx, tx, request.ConnectionID)
		if err != nil {
			return err
		}
		if locked.ConnectionState != "active" ||
			locked.Connection.CredentialRef == "" ||
			locked.Connection.CredentialRef != request.CredentialRef ||
			locked.Connection.ProviderAccountID != locked.Account.AccountID {
			return ErrCredentialStale
		}
		access := accessForLock(ActionManage, StageMutation, request.Scope, request.Actor, locked, request.Scopes, nil)
		access.Credential.CredentialRef = request.CredentialRef
		access.Credential.AuthEpoch = request.AuthEpoch
		if err := callAuthorize(ctx, tx, authorize, access); err != nil {
			return err
		}

		key, err := resolveWrappingKey(ctx, resolveKey, request.KeyRef)
		if err != nil {
			return err
		}
		defer wipe(key)
		fingerprint := credentialIntentFingerprint(key, request, secret)
		mutationKey := store.CredentialMutationKey{
			Principal:      request.Actor.RequestedActor,
			ConnectionID:   request.ConnectionID,
			IdempotencyKey: request.IdempotencyKey,
		}
		existing, err := store.LoadCredentialMutation(ctx, tx, mutationKey)
		if err == nil {
			if !hmac.Equal([]byte(existing.IntentFingerprint), []byte(fingerprint)) {
				return ErrIdempotencyConflict
			}
			result = statusFromMutation(existing)
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}

		expectedVersion := uint64(0)
		expectedRevision := uint64(0)
		if locked.Lifecycle != nil {
			expectedVersion = locked.Lifecycle.CiphertextVersion
			expectedRevision = locked.Lifecycle.Revision
			if expectedVersion >= maxDatabaseBigint || expectedRevision >= maxDatabaseBigint {
				return ErrCredentialUnavailable
			}
		}
		binding := contract.CredentialRef{
			CredentialRef:          request.CredentialRef,
			WorkspaceID:            request.Scope.WorkspaceID,
			ConnectionID:           request.ConnectionID,
			ProviderAccountWitness: locked.Account.AccountWitness,
			CiphertextVersion:      expectedVersion + 1,
			KeyRef:                 request.KeyRef,
			Scopes:                 slices.Clone(request.Scopes),
			AuthEpoch:              request.AuthEpoch,
		}
		if err := binding.Validate(); err != nil {
			return err
		}
		encrypted := store.EncryptedSecret{
			Scope:             request.Scope,
			Binding:           binding,
			ProviderAccountID: locked.Account.AccountID,
		}
		encrypted.Ciphertext, err = sealCredential(key, encrypted, secret)
		if err != nil {
			return err
		}
		if err := store.RegisterEncryptedSecret(ctx, tx, encrypted); err != nil {
			return err
		}

		next := store.CredentialLifecycle{
			Scope:              request.Scope,
			ConnectionID:       request.ConnectionID,
			CredentialRef:      request.CredentialRef,
			ProviderAccountID:  locked.Account.AccountID,
			AccountWitness:     locked.Account.AccountWitness,
			ConnectionRevision: locked.Connection.Revision,
			AccountRevision:    locked.Account.Revision,
			CiphertextVersion:  binding.CiphertextVersion,
			Status:             store.CredentialLifecycleActive,
			AuthEpoch:          request.AuthEpoch,
			Revision:           expectedRevision + 1,
		}
		mutation := store.CredentialMutation{
			Scope:             request.Scope,
			Key:               mutationKey,
			IntentFingerprint: fingerprint,
			CredentialRef:     request.CredentialRef,
			CiphertextVersion: binding.CiphertextVersion,
			Status:            store.CredentialLifecycleActive,
			LifecycleRevision: next.Revision,
		}
		committed, _, err := store.CommitCredentialVersion(ctx, tx, store.CredentialVersionCommit{
			ExpectedCiphertextVersion: expectedVersion,
			ExpectedLifecycleRevision: expectedRevision,
			Next:                      next,
			Mutation:                  mutation,
		})
		if err != nil {
			return err
		}
		result = statusFromLifecycle(committed, false)
		return nil
	})
	if err != nil {
		return CredentialStatus{}, err
	}
	return result, nil
}

func accessForLock(
	action Action,
	stage AuthorizationStage,
	scope contract.Scope,
	actor contract.Actor,
	locked store.CredentialLifecycleLock,
	requestedScopes []string,
	effect *EffectAccess,
) Access {
	access := Access{
		Action:          action,
		Stage:           stage,
		Scope:           scope,
		Actor:           actor,
		Connection:      locked.Connection,
		ConnectionState: locked.ConnectionState,
		Account:         locked.Account,
		Effect:          effect,
	}
	if locked.Lifecycle != nil {
		access.Credential = CredentialAccess{
			CredentialRef:           locked.Lifecycle.CredentialRef,
			BoundProviderAccountID:  locked.Lifecycle.ProviderAccountID,
			BoundAccountWitness:     locked.Lifecycle.AccountWitness,
			BoundConnectionRevision: locked.Lifecycle.ConnectionRevision,
			BoundAccountRevision:    locked.Lifecycle.AccountRevision,
			CiphertextVersion:       locked.Lifecycle.CiphertextVersion,
			Status:                  locked.Lifecycle.Status,
			AuthEpoch:               locked.Lifecycle.AuthEpoch,
		}
	}
	access.Credential.RequestedScopes = slices.Clone(requestedScopes)
	return access
}

func callAuthorize(ctx context.Context, tx *store.Transaction, authorize AuthorizeFunc, access Access) error {
	if err := authorize(ctx, tx, access); err != nil {
		return ErrAuthorizationDenied
	}
	return nil
}

func resolveWrappingKey(ctx context.Context, resolver WrappingKeyResolver, keyRef string) ([]byte, error) {
	resolved, err := resolver(ctx, keyRef)
	if err != nil || len(resolved) != wrappingKeyBytes {
		return nil, ErrWrappingKeyUnavailable
	}
	return bytes.Clone(resolved), nil
}

func sealCredential(key []byte, secret store.EncryptedSecret, plaintext []byte) ([]byte, error) {
	gcm, err := credentialGCM(key)
	if err != nil {
		return nil, err
	}
	nonceSize := gcm.NonceSize()
	envelope := make([]byte, 1+nonceSize, 1+nonceSize+len(plaintext)+gcm.Overhead())
	envelope[0] = credentialEnvelopeVersion
	nonce := envelope[1 : 1+nonceSize]
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, errors.New("generate credential envelope nonce")
	}
	return gcm.Seal(envelope, nonce, plaintext, credentialAAD(secret)), nil
}

func openCredential(key []byte, secret store.EncryptedSecret) ([]byte, error) {
	gcm, err := credentialGCM(key)
	if err != nil {
		return nil, err
	}
	nonceSize := gcm.NonceSize()
	if len(secret.Ciphertext) <= 1+nonceSize+gcm.Overhead() || secret.Ciphertext[0] != credentialEnvelopeVersion {
		return nil, ErrCredentialUnavailable
	}
	nonce := secret.Ciphertext[1 : 1+nonceSize]
	ciphertext := secret.Ciphertext[1+nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, credentialAAD(secret))
	if err != nil {
		return nil, ErrCredentialUnavailable
	}
	if len(plaintext) == 0 || len(plaintext) > maxCredentialBytes {
		wipe(plaintext)
		return nil, ErrCredentialUnavailable
	}
	return plaintext, nil
}

func credentialGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != wrappingKeyBytes {
		return nil, ErrWrappingKeyUnavailable
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrWrappingKeyUnavailable
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrWrappingKeyUnavailable
	}
	return gcm, nil
}

func credentialAAD(secret store.EncryptedSecret) []byte {
	binding := secret.Binding
	result := make([]byte, 0, 256)
	result = appendLengthPrefixed(result, "pm/provider-credential/aes-256-gcm/v1")
	result = appendLengthPrefixed(result, secret.Scope.OrgID)
	result = appendLengthPrefixed(result, secret.Scope.WorkspaceID)
	result = appendLengthPrefixed(result, binding.ConnectionID)
	result = appendLengthPrefixed(result, binding.CredentialRef)
	result = appendLengthPrefixed(result, secret.ProviderAccountID)
	result = appendLengthPrefixed(result, binding.ProviderAccountWitness)
	result = appendUint64(result, binding.CiphertextVersion)
	result = appendLengthPrefixed(result, binding.KeyRef)
	result = appendUint64(result, binding.AuthEpoch)
	result = appendUint64(result, uint64(len(binding.Scopes)))
	for _, scope := range binding.Scopes {
		result = appendLengthPrefixed(result, scope)
	}
	return result
}

func credentialIntentFingerprint(key []byte, request StoreRequest, secret []byte) string {
	derivation := hmac.New(sha256.New, key)
	_, _ = derivation.Write([]byte("pm/provider-credential/intent-key/v1"))
	fingerprintKey := derivation.Sum(nil)
	defer wipe(fingerprintKey)

	fingerprint := hmac.New(sha256.New, fingerprintKey)
	writeUint64 := func(value uint64) {
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], value)
		_, _ = fingerprint.Write(encoded[:])
	}
	writeBytes := func(value []byte) {
		writeUint64(uint64(len(value)))
		_, _ = fingerprint.Write(value)
	}
	writeString := func(value string) {
		writeBytes([]byte(value))
	}

	writeString("pm/provider-credential/intent/v1")
	writeString(request.Scope.OrgID)
	writeString(request.Scope.WorkspaceID)
	writeString(request.Actor.RequestedActor.PrincipalID)
	writeString(string(request.Actor.RequestedActor.Kind))
	writeString(request.Actor.EffectiveActor.PrincipalID)
	writeString(string(request.Actor.EffectiveActor.Kind))
	if request.Actor.Delegation == nil {
		writeString("")
		writeString("")
	} else {
		writeString(request.Actor.Delegation.DelegationID)
		writeString(request.Actor.Delegation.ParentGrantRef)
	}
	writeString(request.ConnectionID)
	writeString(request.CredentialRef)
	writeString(request.KeyRef)
	writeUint64(request.AuthEpoch)
	writeUint64(uint64(len(request.Scopes)))
	for _, scope := range request.Scopes {
		writeString(scope)
	}
	writeBytes(secret)
	return "hmac-sha256:" + hex.EncodeToString(fingerprint.Sum(nil))
}

func appendLengthPrefixed(target []byte, value string) []byte {
	return appendBytes(target, []byte(value))
}

func appendBytes(target, value []byte) []byte {
	target = appendUint64(target, uint64(len(value)))
	return append(target, value...)
}

func appendUint64(target []byte, value uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	return append(target, encoded[:]...)
}

func sameCredentialBinding(left, right contract.CredentialRef) bool {
	return left.CredentialRef == right.CredentialRef &&
		left.WorkspaceID == right.WorkspaceID &&
		left.ConnectionID == right.ConnectionID &&
		left.ProviderAccountWitness == right.ProviderAccountWitness &&
		left.CiphertextVersion == right.CiphertextVersion &&
		left.KeyRef == right.KeyRef &&
		left.AuthEpoch == right.AuthEpoch &&
		slices.Equal(left.Scopes, right.Scopes)
}

func currentBinding(locked store.CredentialLifecycleLock, expected contract.CredentialRef) error {
	if locked.Lifecycle == nil {
		return ErrCredentialUnavailable
	}
	if locked.ConnectionState != "active" ||
		locked.Connection.CredentialRef != expected.CredentialRef ||
		locked.Connection.ConnectionID != expected.ConnectionID ||
		locked.Connection.Scope.WorkspaceID != expected.WorkspaceID ||
		locked.Connection.ProviderAccountID != locked.Account.AccountID ||
		locked.Connection.Revision != locked.Lifecycle.ConnectionRevision ||
		locked.Account.AccountID != locked.Lifecycle.ProviderAccountID ||
		locked.Account.AccountWitness != expected.ProviderAccountWitness ||
		locked.Account.AccountWitness != locked.Lifecycle.AccountWitness ||
		locked.Account.Revision != locked.Lifecycle.AccountRevision ||
		locked.Lifecycle.CredentialRef != expected.CredentialRef ||
		locked.Lifecycle.CiphertextVersion != expected.CiphertextVersion ||
		locked.Lifecycle.AuthEpoch != expected.AuthEpoch {
		return ErrCredentialStale
	}
	switch locked.Lifecycle.Status {
	case store.CredentialLifecycleActive:
		return nil
	case store.CredentialLifecycleRefreshing:
		return ErrCredentialRefreshInProgress
	case store.CredentialLifecycleReauthorizationRequired:
		return ErrReauthorizationRequired
	default:
		return ErrCredentialUnavailable
	}
}

func validOpaque(value string) bool {
	if value == "" || len(value) > 512 || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func wipe(value []byte) {
	clear(value)
}
