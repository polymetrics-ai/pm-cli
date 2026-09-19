package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"
)

const (
	SessionStoreOS     SessionStoreMode = "os"
	SessionStoreMemory SessionStoreMode = "memory"

	sessionServiceName   = "polymetrics.pm"
	sessionSchemaVersion = "pm.session/v1"
	maximumSessionBytes  = 16 << 10
	maximumKeyringInput  = 4096
	darwinEncodingPrefix = "go-keyring-base64:"
)

type SessionStoreMode string

var (
	ErrSessionNotFound         = errors.New("PM session not found")
	ErrSessionStoreUnavailable = errors.New("PM session store unavailable")
	ErrSessionTooLarge         = errors.New("PM session exceeds OS store command bound")
	keyringUnsafePattern       = regexp.MustCompile(`[^\w@%+=:,./-]`)
)

// SessionKey is an opaque fixed-size OS-store key derived from the selected
// instance and non-secret profile handle.
type SessionKey struct {
	account string
}

func NewSessionKey(instanceID, handle string) (SessionKey, error) {
	if !validIdentity(instanceID, 512) || !handlePattern.MatchString(handle) {
		return SessionKey{}, errors.New("PM session key identity is invalid")
	}
	digest := sha256.Sum256([]byte(instanceID + "\x00" + handle))
	return SessionKey{account: "pm-session:" + hex.EncodeToString(digest[:])}, nil
}

func (key SessionKey) valid() bool {
	return len(key.account) == len("pm-session:")+sha256.Size*2 && strings.HasPrefix(key.account, "pm-session:")
}

func (SessionKey) String() string   { return "PM session key [REDACTED]" }
func (SessionKey) GoString() string { return "PM session key [REDACTED]" }

// SessionStore accepts only the generated PM session type. It cannot be used as
// a generic credential vault or for provider secrets.
type SessionStore interface {
	Store(context.Context, SessionKey, PMDeviceSession) error
	Load(context.Context, SessionKey) (PMDeviceSession, error)
	Delete(context.Context, SessionKey) error
}

func NewSessionStore(mode SessionStoreMode) (SessionStore, error) {
	switch mode {
	case SessionStoreOS:
		return newOSSessionStore(systemKeyring{}), nil
	case SessionStoreMemory:
		return NewMemorySessionStore(), nil
	default:
		return nil, errors.New("session store mode must be explicitly selected as os or memory")
	}
}

type keyringBackend interface {
	Set(service, user, password string) error
	Get(service, user string) (string, error)
	Delete(service, user string) error
}

type systemKeyring struct{}

func (systemKeyring) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}
func (systemKeyring) Get(service, user string) (string, error) { return keyring.Get(service, user) }
func (systemKeyring) Delete(service, user string) error        { return keyring.Delete(service, user) }

type osSessionStore struct {
	backend keyringBackend
}

func newOSSessionStore(backend keyringBackend) *osSessionStore {
	return &osSessionStore{backend: backend}
}

type storedSession struct {
	SchemaVersion string          `json:"schema_version"`
	InstanceID    string          `json:"instance_id"`
	Session       PMDeviceSession `json:"session"`
}

func (store *osSessionStore) Store(ctx context.Context, key SessionKey, session PMDeviceSession) error {
	if err := checkSessionContextAndKey(ctx, key); err != nil {
		return err
	}
	if store == nil || store.backend == nil {
		return ErrSessionStoreUnavailable
	}
	if err := validatePMSession(session); err != nil {
		return err
	}
	// Instance identity is already bound into key.account and deliberately is
	// not recoverable from the OS-store account name.
	record := storedSession{SchemaVersion: sessionSchemaVersion, InstanceID: key.account, Session: session}
	document, err := json.Marshal(record)
	if err != nil || len(document) > maximumSessionBytes {
		return ErrSessionTooLarge
	}
	// go-keyring v0.2.8 starts /usr/bin/security before its own size check on
	// Darwin. Mirror its exact base64 and shell command encoding first, and
	// require the command to be strictly below 4096 bytes before Set is called.
	if darwinSetCommandLength(sessionServiceName, key.account, string(document)) >= maximumKeyringInput {
		return ErrSessionTooLarge
	}
	if err := store.backend.Set(sessionServiceName, key.account, string(document)); err != nil {
		return ErrSessionStoreUnavailable
	}
	return nil
}

func (store *osSessionStore) Load(ctx context.Context, key SessionKey) (PMDeviceSession, error) {
	if err := checkSessionContextAndKey(ctx, key); err != nil {
		return PMDeviceSession{}, err
	}
	if store == nil || store.backend == nil {
		return PMDeviceSession{}, ErrSessionStoreUnavailable
	}
	document, err := store.backend.Get(sessionServiceName, key.account)
	if errors.Is(err, keyring.ErrNotFound) {
		return PMDeviceSession{}, ErrSessionNotFound
	}
	if err != nil {
		return PMDeviceSession{}, ErrSessionStoreUnavailable
	}
	if len(document) == 0 || len(document) > maximumSessionBytes {
		return PMDeviceSession{}, ErrSessionStoreUnavailable
	}
	record, err := decodeStoredSession([]byte(document))
	if err != nil || record.InstanceID != key.account {
		return PMDeviceSession{}, ErrSessionStoreUnavailable
	}
	return record.Session, nil
}

func (store *osSessionStore) Delete(ctx context.Context, key SessionKey) error {
	if err := checkSessionContextAndKey(ctx, key); err != nil {
		return err
	}
	if store == nil || store.backend == nil {
		return ErrSessionStoreUnavailable
	}
	if err := store.backend.Delete(sessionServiceName, key.account); err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return ErrSessionNotFound
		}
		return ErrSessionStoreUnavailable
	}
	return nil
}

func decodeStoredSession(document []byte) (storedSession, error) {
	object, err := decodeJSONObject(document)
	if err != nil {
		return storedSession{}, err
	}
	if err := validateObjectFieldNames(object, []string{"schema_version", "instance_id", "session"}, []string{"schema_version", "instance_id", "session"}); err != nil {
		return storedSession{}, err
	}
	sessionObject, err := decodeJSONObject(object["session"])
	if err != nil {
		return storedSession{}, err
	}
	if err := validateObjectFieldNames(sessionObject, []string{"expires_at", "pm_session_credential", "principal", "session_id"}, []string{"expires_at", "pm_session_credential", "principal", "session_id"}); err != nil {
		return storedSession{}, err
	}
	principalObject, err := decodeJSONObject(sessionObject["principal"])
	if err != nil {
		return storedSession{}, err
	}
	if err := validateObjectFieldNames(principalObject, []string{"kind", "principal_id"}, []string{"kind", "principal_id"}); err != nil {
		return storedSession{}, err
	}
	var record storedSession
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || requireJSONDecoderEOF(decoder) != nil || record.SchemaVersion != sessionSchemaVersion {
		return storedSession{}, ErrMalformedEnvelope
	}
	if !validIdentity(record.InstanceID, 256) || validatePMSession(record.Session) != nil {
		return storedSession{}, ErrMalformedEnvelope
	}
	return record, nil
}

func validatePMSession(session PMDeviceSession) error {
	if !validIdentity(session.SessionId, 512) || !validIdentity(session.PmSessionCredential, maximumSessionBytes) ||
		!validIdentity(session.Principal.PrincipalId, 512) || !session.Principal.Kind.Valid() || session.ExpiresAt.IsZero() {
		return errors.New("PM session is invalid")
	}
	return nil
}

func checkSessionContextAndKey(ctx context.Context, key SessionKey) error {
	if ctx == nil {
		return errors.New("PM session context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !key.valid() {
		return errors.New("PM session key is invalid")
	}
	return nil
}

func darwinSetCommandLength(service, account, password string) int {
	encoded := darwinEncodingPrefix + base64.StdEncoding.EncodeToString([]byte(password))
	command := fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n", keyringQuote(service), keyringQuote(account), keyringQuote(encoded))
	return len(command)
}

func keyringQuote(value string) string {
	if value == "" {
		return "''"
	}
	if keyringUnsafePattern.MatchString(value) {
		return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
	}
	return value
}

// MemorySessionStore is intentionally process-local and exists only when the
// caller explicitly selects memory mode. It is never an automatic fallback.
type MemorySessionStore struct {
	mu       sync.RWMutex
	sessions map[string][]byte
}

func NewMemorySessionStore() *MemorySessionStore {
	return &MemorySessionStore{sessions: make(map[string][]byte)}
}

func (store *MemorySessionStore) Store(ctx context.Context, key SessionKey, session PMDeviceSession) error {
	if err := checkSessionContextAndKey(ctx, key); err != nil {
		return err
	}
	if store == nil {
		return ErrSessionStoreUnavailable
	}
	if err := validatePMSession(session); err != nil {
		return err
	}
	document, err := json.Marshal(storedSession{SchemaVersion: sessionSchemaVersion, InstanceID: key.account, Session: session})
	if err != nil || len(document) > maximumSessionBytes {
		return ErrSessionTooLarge
	}
	store.mu.Lock()
	store.sessions[key.account] = append([]byte(nil), document...)
	store.mu.Unlock()
	return nil
}

func (store *MemorySessionStore) Load(ctx context.Context, key SessionKey) (PMDeviceSession, error) {
	if err := checkSessionContextAndKey(ctx, key); err != nil {
		return PMDeviceSession{}, err
	}
	if store == nil {
		return PMDeviceSession{}, ErrSessionStoreUnavailable
	}
	store.mu.RLock()
	document, ok := store.sessions[key.account]
	copy := append([]byte(nil), document...)
	store.mu.RUnlock()
	if !ok {
		return PMDeviceSession{}, ErrSessionNotFound
	}
	record, err := decodeStoredSession(copy)
	if err != nil || record.InstanceID != key.account {
		return PMDeviceSession{}, ErrSessionStoreUnavailable
	}
	return record.Session, nil
}

func (store *MemorySessionStore) Delete(ctx context.Context, key SessionKey) error {
	if err := checkSessionContextAndKey(ctx, key); err != nil {
		return err
	}
	if store == nil {
		return ErrSessionStoreUnavailable
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.sessions[key.account]; !ok {
		return ErrSessionNotFound
	}
	delete(store.sessions, key.account)
	return nil
}
