package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

type recordingKeyring struct {
	values      map[string]string
	setCalls    int
	getErr      error
	deleteCalls int
}

func (backend *recordingKeyring) Set(service, user, password string) error {
	backend.setCalls++
	if backend.values == nil {
		backend.values = make(map[string]string)
	}
	backend.values[service+"\x00"+user] = password
	return nil
}
func (backend *recordingKeyring) Get(service, user string) (string, error) {
	if backend.getErr != nil {
		return "", backend.getErr
	}
	value, ok := backend.values[service+"\x00"+user]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return value, nil
}
func (backend *recordingKeyring) Delete(service, user string) error {
	backend.deleteCalls++
	key := service + "\x00" + user
	if _, ok := backend.values[key]; !ok {
		return keyring.ErrNotFound
	}
	delete(backend.values, key)
	return nil
}

func TestPMTokenOSStorage(t *testing.T) {
	ctx := context.Background()
	key, err := NewSessionKey("instance-v18", "profile-v18")
	if err != nil {
		t.Fatalf("session key: %v", err)
	}
	base := PMDeviceSession{
		ExpiresAt: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
		Principal: Principal{Kind: PrincipalKindUser, PrincipalId: "ada"},
		SessionId: "session-v18",
	}

	backend := &recordingKeyring{}
	store := newOSSessionStore(backend)
	var accepted PMDeviceSession
	for length := range maximumSessionBytes {
		if length == 0 {
			continue
		}
		candidate := base
		candidate.PmSessionCredential = strings.Repeat("x", length)
		record, marshalErr := jsonSessionForTest(key, candidate)
		if marshalErr != nil {
			t.Fatalf("marshal bounded session: %v", marshalErr)
		}
		if darwinSetCommandLength(sessionServiceName, key.account, string(record)) < maximumKeyringInput {
			accepted = candidate
			continue
		}
		break
	}
	if accepted.PmSessionCredential == "" {
		t.Fatal("did not find a credential below the Darwin command bound")
	}
	if err := store.Store(ctx, key, accepted); err != nil {
		t.Fatalf("store below command bound: %v", err)
	}
	if backend.setCalls != 1 {
		t.Fatalf("keyring Set calls below bound = %d, want 1", backend.setCalls)
	}
	loaded, err := store.Load(ctx, key)
	if err != nil || loaded.PmSessionCredential != accepted.PmSessionCredential {
		t.Fatalf("load stored PM session = (%#v, %v)", loaded, err)
	}

	oversized := accepted
	for {
		oversized.PmSessionCredential += "x"
		record, marshalErr := jsonSessionForTest(key, oversized)
		if marshalErr != nil {
			t.Fatalf("marshal oversized candidate: %v", marshalErr)
		}
		if darwinSetCommandLength(sessionServiceName, key.account, string(record)) >= maximumKeyringInput {
			break
		}
	}
	if err := store.Store(ctx, key, oversized); !errors.Is(err, ErrSessionTooLarge) {
		t.Fatalf("oversized store error = %v, want ErrSessionTooLarge", err)
	}
	if backend.setCalls != 1 {
		t.Fatalf("oversized credential started keyring Set: calls = %d", backend.setCalls)
	}

	unavailable := newOSSessionStore(&recordingKeyring{getErr: errors.New("OS store unavailable")})
	if _, err := unavailable.Load(ctx, key); !errors.Is(err, ErrSessionStoreUnavailable) {
		t.Fatalf("missing OS store error = %v", err)
	}
	if _, err := NewSessionStore(""); err == nil {
		t.Fatal("empty store mode selected an implicit fallback")
	}
	memory, err := NewSessionStore(SessionStoreMemory)
	if err != nil {
		t.Fatalf("explicit memory store: %v", err)
	}
	if err := memory.Store(ctx, key, accepted); err != nil {
		t.Fatalf("explicit memory store: %v", err)
	}
}

func jsonSessionForTest(key SessionKey, session PMDeviceSession) ([]byte, error) {
	return json.Marshal(storedSession{SchemaVersion: sessionSchemaVersion, InstanceID: key.account, Session: session})
}
