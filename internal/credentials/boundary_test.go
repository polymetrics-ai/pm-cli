package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/store"
)

func TestNoProviderSecretCrossesClientBoundary(t *testing.T) {
	pools := prepareCredentialTestPools(t)

	t.Run("encrypted storage authenticates metadata and exposes no readback", func(t *testing.T) {
		fixture := newCredentialFixture(t, pools)
		canary := []byte("vault-canary-" + credentialRandomHex(t, 12))
		request := fixture.storeRequest(canary, "projection-only")
		encodedRequest, err := json.Marshal(request)
		if err != nil {
			t.Fatal("marshal redacted credential store request")
		}
		formattedRequest := fmt.Sprintf("%v %+v %#v", request, request, request)
		if projectionContainsSecret(encodedRequest, canary) ||
			projectionContainsSecret([]byte(formattedRequest), canary) ||
			projectionContainsSecret(encodedRequest, fixture.key) ||
			projectionContainsSecret([]byte(formattedRequest), fixture.key) {
			t.Fatal("credential store request projection contains provider secret or wrapping key")
		}
		status, binding := fixture.storeCredential(t, canary, "store-once")
		if status.CiphertextVersion != 1 || status.Status != store.CredentialLifecycleActive || status.Replayed {
			t.Fatalf("unexpected safe credential status: %+v", status)
		}
		encodedStatus, err := json.Marshal(status)
		if err != nil {
			t.Fatal("marshal safe credential status")
		}
		if projectionContainsSecret(encodedStatus, canary) ||
			projectionContainsSecret(encodedStatus, fixture.key) {
			t.Fatal("safe credential status contains credential or wrapping-key bytes")
		}

		encrypted := fixture.loadEncrypted(t, status.CiphertextVersion)
		if bytes.Contains(encrypted.Ciphertext, canary) || bytes.Equal(encrypted.Ciphertext, canary) {
			t.Fatal("stored ciphertext exposes plaintext credential canary")
		}
		resolver := &countingKeyResolver{key: fixture.key}
		authorization := &countingAuthorization{}
		material, err := ResolveForEffect(
			context.Background(),
			pools.first,
			resolver.resolve,
			authorization.authorize,
			ResolveRequest{Scope: fixture.scope, Actor: fixture.actor, Binding: binding, Effect: fixture.effect()},
		)
		if err != nil {
			t.Fatalf("resolve admitted credential: %v", err)
		}
		if err := material.Use(func(secret []byte) error {
			if !bytes.Equal(secret, canary) {
				return errors.New("resolved credential differs from stored input")
			}
			return nil
		}); err != nil {
			t.Fatalf("use admitted credential: %v", err)
		}
		encodedMaterial, err := json.Marshal(material)
		if err != nil {
			t.Fatal("marshal redacted credential material")
		}
		formattedMaterial := fmt.Sprintf("%v %+v %#v", material, material, material)
		if projectionContainsSecret(encodedMaterial, canary) ||
			projectionContainsSecret([]byte(formattedMaterial), canary) {
			t.Fatal("resolved material projection contains provider secret canary")
		}
		if err := material.Use(func([]byte) error { return nil }); !errors.Is(err, ErrCredentialUnavailable) {
			t.Fatalf("reused credential material error = %v; want ErrCredentialUnavailable", err)
		}
		material.Destroy()

		tampered := encrypted
		tampered.Binding.CiphertextVersion++
		if plaintext, err := openCredential(fixture.key, tampered); !errors.Is(err, ErrCredentialUnavailable) {
			wipe(plaintext)
			t.Fatalf("metadata-tampered credential error = %v; want ErrCredentialUnavailable", err)
		}
	})

	for _, denial := range []string{
		"missing credentials.manage",
		"missing step-up",
		"wrong origin",
		"missing csrf",
	} {
		denial := denial
		t.Run("authorization denial has zero vault effects "+denial, func(t *testing.T) {
			fixture := newCredentialFixture(t, pools)
			canary := []byte("denied-canary-" + credentialRandomHex(t, 8))
			resolver := &countingKeyResolver{key: fixture.key}
			authorization := &countingAuthorization{err: errors.New(denial + " " + string(canary))}
			_, err := StoreEncryptedCredential(
				context.Background(),
				pools.first,
				resolver.resolve,
				authorization.authorize,
				fixture.storeRequest(canary, "denied-key"),
			)
			if !errors.Is(err, ErrAuthorizationDenied) {
				t.Fatalf("authorization denial error = %v; want ErrAuthorizationDenied", err)
			}
			if resolver.calls.Load() != 0 {
				t.Fatal("authorization denial resolved a wrapping key")
			}
			if err != nil && strings.Contains(err.Error(), string(canary)) {
				t.Fatal("authorization error exposed provider secret canary")
			}
			if _, lifecycleErr := fixture.loadLifecycle(t); !errors.Is(lifecycleErr, store.ErrNotFound) {
				t.Fatalf("authorization denial lifecycle error = %v; want ErrNotFound", lifecycleErr)
			}
			fixture.requireEncryptedNotFound(t, 1)
		})
	}

	t.Run("unavailable wrapping key has zero persistence and no plaintext fallback", func(t *testing.T) {
		fixture := newCredentialFixture(t, pools)
		canary := []byte("key-unavailable-canary-" + credentialRandomHex(t, 8))
		resolver := &countingKeyResolver{err: errors.New("operator key service unavailable " + string(canary))}
		authorization := &countingAuthorization{}
		_, err := StoreEncryptedCredential(
			context.Background(),
			pools.first,
			resolver.resolve,
			authorization.authorize,
			fixture.storeRequest(canary, "key-unavailable"),
		)
		if !errors.Is(err, ErrWrappingKeyUnavailable) {
			t.Fatalf("wrapping-key error = %v; want ErrWrappingKeyUnavailable", err)
		}
		if strings.Contains(err.Error(), string(canary)) {
			t.Fatal("wrapping-key error exposed provider secret canary")
		}
		if _, lifecycleErr := fixture.loadLifecycle(t); !errors.Is(lifecycleErr, store.ErrNotFound) {
			t.Fatalf("wrapping-key refusal lifecycle error = %v; want ErrNotFound", lifecycleErr)
		}
		fixture.requireEncryptedNotFound(t, 1)
	})

	t.Run("same key replay is safe and changed input conflicts", func(t *testing.T) {
		fixture := newCredentialFixture(t, pools)
		canary := []byte("idempotency-canary-" + credentialRandomHex(t, 8))
		resolver := &countingKeyResolver{key: fixture.key}
		authorization := &countingAuthorization{}
		request := fixture.storeRequest(canary, "stable-admin-key")
		first, err := StoreEncryptedCredential(context.Background(), pools.first, resolver.resolve, authorization.authorize, request)
		if err != nil {
			t.Fatalf("first credential store: %v", err)
		}
		replay, err := StoreEncryptedCredential(context.Background(), pools.first, resolver.resolve, authorization.authorize, request)
		if err != nil {
			t.Fatalf("exact credential replay: %v", err)
		}
		if !replay.Replayed || replay.CredentialRef != first.CredentialRef ||
			replay.CiphertextVersion != first.CiphertextVersion || replay.Status != first.Status {
			t.Fatalf("exact replay changed safe result: first=%+v replay=%+v", first, replay)
		}

		changed := request
		changed.Secret = []byte("changed-credential-" + credentialRandomHex(t, 8))
		if _, err := StoreEncryptedCredential(context.Background(), pools.first, resolver.resolve, authorization.authorize, changed); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("changed credential replay error = %v; want ErrIdempotencyConflict", err)
		}
		lifecycle, err := fixture.loadLifecycle(t)
		if err != nil {
			t.Fatalf("load lifecycle after idempotency conflict: %v", err)
		}
		if lifecycle.CiphertextVersion != 1 {
			t.Fatalf("idempotency conflict advanced ciphertext version to %d", lifecycle.CiphertextVersion)
		}
		fixture.requireEncryptedNotFound(t, 2)
	})

	t.Run("ordinary status lifecycle and mutation receipt projections are secret free", func(t *testing.T) {
		fixture := newCredentialFixture(t, pools)
		canary := []byte("receipt-canary-" + credentialRandomHex(t, 12))
		status, _ := fixture.storeCredential(t, canary, "receipt-projection")
		lifecycle, err := fixture.loadLifecycle(t)
		if err != nil {
			t.Fatalf("load lifecycle projection: %v", err)
		}
		var receipt store.CredentialMutation
		err = store.WithinScope(context.Background(), pools.first, fixture.scope, fixture.actor, func(ctx context.Context, tx *store.Transaction) error {
			var err error
			receipt, err = store.LoadCredentialMutation(ctx, tx, store.CredentialMutationKey{
				Principal:      fixture.actor.RequestedActor,
				ConnectionID:   fixture.connection.ConnectionID,
				IdempotencyKey: "receipt-projection",
			})
			return err
		})
		if err != nil {
			t.Fatalf("load credential mutation receipt: %v", err)
		}
		projection, err := json.Marshal(struct {
			Status    CredentialStatus          `json:"status"`
			Lifecycle store.CredentialLifecycle `json:"lifecycle"`
			Receipt   store.CredentialMutation  `json:"receipt"`
		}{
			Status:    status,
			Lifecycle: lifecycle,
			Receipt:   receipt,
		})
		if err != nil {
			t.Fatal("marshal ordinary credential projections")
		}
		formatted := []byte(fmt.Sprintf("%+v %#v %+v", status, lifecycle, receipt))
		for _, secret := range [][]byte{canary, fixture.key} {
			if projectionContainsSecret(projection, secret) || projectionContainsSecret(formatted, secret) {
				t.Fatal("ordinary credential status, lifecycle, or receipt exposed secret material")
			}
		}
	})

	t.Run("resolution denial and missing key perform no secret resolution", func(t *testing.T) {
		fixture := newCredentialFixture(t, pools)
		canary := []byte("resolve-canary-" + credentialRandomHex(t, 8))
		_, binding := fixture.storeCredential(t, canary, "resolve-base")

		deniedResolver := &countingKeyResolver{key: fixture.key}
		deniedAuthorization := &countingAuthorization{err: errors.New("denied " + string(canary))}
		_, err := ResolveForEffect(
			context.Background(), pools.first, deniedResolver.resolve, deniedAuthorization.authorize,
			ResolveRequest{Scope: fixture.scope, Actor: fixture.actor, Binding: binding, Effect: fixture.effect()},
		)
		if !errors.Is(err, ErrAuthorizationDenied) || deniedResolver.calls.Load() != 0 {
			t.Fatalf("denied resolution error/calls = %v/%d", err, deniedResolver.calls.Load())
		}

		missingResolver := &countingKeyResolver{err: errors.New("missing " + string(canary))}
		allowedAuthorization := &countingAuthorization{}
		_, err = ResolveForEffect(
			context.Background(), pools.first, missingResolver.resolve, allowedAuthorization.authorize,
			ResolveRequest{Scope: fixture.scope, Actor: fixture.actor, Binding: binding, Effect: fixture.effect()},
		)
		if !errors.Is(err, ErrWrappingKeyUnavailable) || strings.Contains(err.Error(), string(canary)) {
			t.Fatalf("missing-key resolution error = %v", err)
		}
	})

	t.Run("unsafe provider diagnostics are dropped rather than reflected", func(t *testing.T) {
		canaries := [][]byte{
			[]byte("header-canary-" + credentialRandomHex(t, 8)),
			[]byte("body-canary-" + credentialRandomHex(t, 8)),
			[]byte("error-canary-" + credentialRandomHex(t, 8)),
		}
		diagnostic := Diagnostic{
			Code:    "credential_refresh_rejected",
			Message: "provider reflected " + string(canaries[2]),
			Headers: map[string][]string{
				"Authorization": {"Bearer " + string(canaries[0])},
				"Set-Cookie":    {"provider_session=" + string(canaries[0])},
			},
			Body: []byte("response=" + string(canaries[1])),
			Err:  errors.New("provider error " + string(canaries[2])),
		}
		safe := Redact(diagnostic, canaries...)
		encoded, err := json.Marshal(safe)
		if err != nil {
			t.Fatal("marshal safe diagnostic")
		}
		formatted := fmt.Sprintf("%v %+v %#v", diagnostic, diagnostic, diagnostic)
		encodedDiagnostic, err := json.Marshal(diagnostic)
		if err != nil {
			t.Fatal("marshal redacted raw diagnostic")
		}
		attempt := RefreshAttempt{
			Disposition: RefreshOutcomeUnknown,
			Credential:  canaries[0],
			Diagnostic:  diagnostic,
		}
		encodedAttempt, err := json.Marshal(attempt)
		if err != nil {
			t.Fatal("marshal redacted refresh attempt")
		}
		formattedAttempt := fmt.Sprintf("%v %+v %#v", attempt, attempt, attempt)
		for _, canary := range canaries {
			if projectionContainsSecret(encoded, canary) ||
				projectionContainsSecret(encodedDiagnostic, canary) ||
				projectionContainsSecret(encodedAttempt, canary) ||
				projectionContainsSecret([]byte(formatted), canary) ||
				projectionContainsSecret([]byte(formattedAttempt), canary) {
				t.Fatal("redacted diagnostic exposed a provider secret canary")
			}
		}
		if safe.Code != "credential_refresh_rejected" {
			t.Fatalf("safe diagnostic code = %q", safe.Code)
		}

		unsafeCode := Diagnostic{Code: string(canaries[0]), Message: string(canaries[1])}
		fallback := Redact(unsafeCode, canaries...)
		if fallback.Code != "credential_operation_failed" || strings.Contains(fallback.Detail, string(canaries[0])) {
			t.Fatalf("unsafe diagnostic code was reflected: %+v", fallback)
		}
	})

	t.Run("current connection and account invalidation precede authorization and secret access", func(t *testing.T) {
		for _, test := range []struct {
			name   string
			mutate func(*testing.T, credentialFixture)
		}{
			{
				name: "connection revoked",
				mutate: func(t *testing.T, fixture credentialFixture) {
					fixture.revokeConnection(t)
				},
			},
			{
				name: "provider account witness changed",
				mutate: func(t *testing.T, fixture credentialFixture) {
					fixture.mutateAccountWitness(t, "revoked-account-witness-"+credentialRandomHex(t, 8))
				},
			},
		} {
			test := test
			t.Run(test.name, func(t *testing.T) {
				fixture := newCredentialFixture(t, pools)
				_, binding := fixture.storeCredential(t, []byte("revocation-canary-"+credentialRandomHex(t, 8)), "revocation-base")
				test.mutate(t, fixture)
				historical := fixture.loadEncrypted(t, binding.CiphertextVersion)
				if !sameCredentialBinding(historical.Binding, binding) {
					t.Fatal("historical encrypted credential binding changed during current-authority invalidation")
				}
				resolver := &countingKeyResolver{key: fixture.key}
				authorization := &countingAuthorization{}
				_, err := ResolveForEffect(
					context.Background(), pools.first, resolver.resolve, authorization.authorize,
					ResolveRequest{Scope: fixture.scope, Actor: fixture.actor, Binding: binding, Effect: fixture.effect()},
				)
				if !errors.Is(err, ErrCredentialStale) {
					t.Fatalf("invalidated credential resolution error = %v; want ErrCredentialStale", err)
				}
				if authorization.calls.Load() != 0 || resolver.calls.Load() != 0 {
					t.Fatalf("invalidated credential crossed authorization/key boundary: %d/%d", authorization.calls.Load(), resolver.calls.Load())
				}
			})
		}
	})

	t.Run("expired effect cannot resolve a credential", func(t *testing.T) {
		fixture := newCredentialFixture(t, pools)
		_, binding := fixture.storeCredential(t, []byte("deadline-canary"), "deadline-base")
		effect := fixture.effect()
		effect.Deadline = time.Now().Add(-time.Second)
		resolver := &countingKeyResolver{key: fixture.key}
		authorization := &countingAuthorization{}
		_, err := ResolveForEffect(
			context.Background(), pools.first, resolver.resolve, authorization.authorize,
			ResolveRequest{Scope: fixture.scope, Actor: fixture.actor, Binding: binding, Effect: effect},
		)
		if !errors.Is(err, ErrEffectExpired) || resolver.calls.Load() != 0 || authorization.calls.Load() != 0 {
			t.Fatalf("expired effect error/auth/key calls = %v/%d/%d", err, authorization.calls.Load(), resolver.calls.Load())
		}
	})
}
