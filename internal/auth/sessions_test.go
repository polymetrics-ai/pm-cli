package auth_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

func TestDeviceBootstrapProofs(t *testing.T) {
	fixture := newAuthFixture(t)
	oidcHarness := newOIDCHarness(t)
	service, err := auth.NewService(fixture.serviceConfig("", oidcHarness.config()))
	if err != nil {
		t.Fatalf("NewService(): %v", err)
	}
	fixture.addBootstrapIdentity(t, oidcHarness.issuer, oidcHarness.subject, fixture.ada, true)
	fixture.addBootstrapIdentity(t, "https://alternate.example.test", "alternate-ada", fixture.ada, true)
	login, err := service.BeginOIDCLogin(context.Background(), auth.OIDCLoginStart{
		TLS: true, Host: testHost, Origin: testOrigin,
	})
	if err != nil {
		t.Fatalf("BeginOIDCLogin(browser approver): %v", err)
	}
	oidcHarness.register("device-approver", login, nil, nil, nil)
	loginResult, err := service.CompleteOIDCLogin(oidcHarness.context(), auth.OIDCLoginCallback{
		ChallengeID:  login.ChallengeID,
		Code:         "device-approver",
		State:        login.State,
		PKCEVerifier: login.PKCEVerifier,
		TLS:          true, Host: testHost, Origin: testOrigin,
	})
	if err != nil {
		t.Fatalf("CompleteOIDCLogin(browser approver): %v", err)
	}
	browserSession := loginResult.Session
	start := func(clientIP, deviceID string) auth.DeviceChallenge {
		t.Helper()
		challenge, err := service.StartDeviceLogin(context.Background(), auth.DeviceStart{
			ClientIP: clientIP, DeviceID: deviceID, RequestBytes: 128,
		})
		if err != nil {
			t.Fatalf("StartDeviceLogin(): %v", err)
		}
		return challenge
	}
	decision := func(challenge auth.DeviceChallenge) auth.BrowserDeviceDecision {
		return auth.BrowserDeviceDecision{
			ChallengeID: challenge.ChallengeID,
			UserCode:    challenge.UserCode,
			CookieToken: browserSession.Token,
			CSRFToken:   browserSession.CSRFToken,
		}
	}
	exchange := func(challenge auth.DeviceChallenge) (auth.PMSession, error) {
		return service.ExchangeDeviceProof(context.Background(), auth.DeviceProof{
			ChallengeID: challenge.ChallengeID,
			Proof:       challenge.DeviceProof,
		})
	}

	challenge := start("192.0.2.10", "device-primary")
	encodedChallenge, err := json.Marshal(challenge)
	if err != nil {
		t.Fatalf("json.Marshal(DeviceChallenge): %v", err)
	}
	if strings.Contains(string(encodedChallenge), challenge.DeviceProof) {
		t.Fatal("device proof crossed the JSON response boundary")
	}
	encodedDecision, err := json.Marshal(decision(challenge))
	if err != nil {
		t.Fatalf("json.Marshal(BrowserDeviceDecision): %v", err)
	}
	var decisionBody map[string]any
	if err := json.Unmarshal(encodedDecision, &decisionBody); err != nil {
		t.Fatalf("json.Unmarshal(BrowserDeviceDecision): %v", err)
	}
	if len(decisionBody) != 2 || decisionBody["challenge_id"] != challenge.ChallengeID ||
		decisionBody["user_code"] != challenge.UserCode ||
		strings.Contains(string(encodedDecision), browserSession.Token) ||
		strings.Contains(string(encodedDecision), browserSession.CSRFToken) {
		t.Fatalf("browser device decision body = %s", encodedDecision)
	}
	if _, err := exchange(challenge); err != auth.ErrPollTooSoon {
		t.Fatalf("immediate pending exchange error = %v, want ErrPollTooSoon", err)
	}
	fixture.makeDevicePollEligible(t, challenge.ChallengeID)
	if _, err := exchange(challenge); err != auth.ErrChallengePending {
		t.Fatalf("eligible pending exchange error = %v, want ErrChallengePending", err)
	}
	if err := service.ApproveDeviceLoginFromBrowser(context.Background(), decision(challenge)); err != nil {
		t.Fatalf("ApproveDeviceLoginFromBrowser(): %v", err)
	}
	if _, err := exchange(challenge); err != auth.ErrPollTooSoon {
		t.Fatalf("immediate approved exchange error = %v, want ErrPollTooSoon", err)
	}
	fixture.makeDevicePollEligible(t, challenge.ChallengeID)
	session, err := exchange(challenge)
	if err != nil {
		t.Fatalf("approved ExchangeDeviceProof(): %v", err)
	}
	if session.Transport != store.SessionTransportDevice || session.Principal != fixture.ada ||
		session.Token == "" || session.CSRFToken != "" || !session.AuthenticatedAt.IsZero() {
		t.Fatalf("device session = %#v", session)
	}
	encodedSession, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("json.Marshal(PMSession): %v", err)
	}
	if strings.Contains(string(encodedSession), session.Token) {
		t.Fatal("session token crossed the JSON response boundary")
	}
	identity, err := service.Authenticate(context.Background(), auth.SessionAuthentication{BearerToken: session.Token})
	if err != nil {
		t.Fatalf("Authenticate(device bearer): %v", err)
	}
	if identity.Principal != fixture.ada || identity.Source != auth.AuthenticationDeviceSession ||
		!identity.AuthenticatedAt.IsZero() {
		t.Fatalf("device identity = %#v", identity)
	}
	if _, err := service.Authenticate(context.Background(), auth.SessionAuthentication{BrowserCookie: session.Token}); err != auth.ErrAuthenticationRequired {
		t.Fatalf("Authenticate(device as cookie) error = %v, want ErrAuthenticationRequired", err)
	}
	if _, err := service.Authenticate(context.Background(), auth.SessionAuthentication{
		BearerToken: session.Token, QueryCredential: session.Token,
	}); err != auth.ErrAuthenticationRequired {
		t.Fatalf("Authenticate(query credential) error = %v, want ErrAuthenticationRequired", err)
	}
	if _, err := exchange(challenge); err != auth.ErrChallengeUsed {
		t.Fatalf("reused exchange error = %v, want ErrChallengeUsed", err)
	}

	wrongProof := start("192.0.2.11", "device-wrong-proof")
	fixture.makeDevicePollEligible(t, wrongProof.ChallengeID)
	if _, err := service.ExchangeDeviceProof(context.Background(), auth.DeviceProof{
		ChallengeID: wrongProof.ChallengeID, Proof: wrongProof.DeviceProof + "-wrong",
	}); err != auth.ErrChallengeInvalid {
		t.Fatalf("wrong proof error = %v, want ErrChallengeInvalid", err)
	}

	denied := start("192.0.2.12", "device-denied")
	if err := service.DenyDeviceLoginFromBrowser(context.Background(), decision(denied)); err != nil {
		t.Fatalf("DenyDeviceLoginFromBrowser(): %v", err)
	}
	fixture.makeDevicePollEligible(t, denied.ChallengeID)
	if _, err := exchange(denied); err != auth.ErrChallengeDenied {
		t.Fatalf("denied exchange error = %v, want ErrChallengeDenied", err)
	}

	expired := start("192.0.2.13", "device-expired")
	fixture.expireDeviceChallenge(t, expired.ChallengeID)
	if _, err := exchange(expired); err != auth.ErrChallengeExpired {
		t.Fatalf("expired exchange error = %v, want ErrChallengeExpired", err)
	}

	if _, err := service.StartDeviceLogin(context.Background(), auth.DeviceStart{
		ClientIP: "192.0.2.14", DeviceID: "device-large", RequestBytes: 1025,
	}); err != auth.ErrRequestTooLarge {
		t.Fatalf("oversized start error = %v, want ErrRequestTooLarge", err)
	}
	for index, deviceID := range []string{"device-rate-client-a", "device-rate-client-b", "device-rate-client-c"} {
		if _, err := service.StartDeviceLogin(context.Background(), auth.DeviceStart{
			ClientIP: "192.0.2.20", DeviceID: deviceID, RequestBytes: 128,
		}); err != nil {
			t.Fatalf("client rate-window start %d: %v", index+1, err)
		}
	}
	if _, err := service.StartDeviceLogin(context.Background(), auth.DeviceStart{
		ClientIP: "192.0.2.20", DeviceID: "device-rate-client-d", RequestBytes: 128,
	}); err != auth.ErrRateLimited {
		t.Fatalf("fourth client rate-window start error = %v, want ErrRateLimited", err)
	}
	for index, clientIP := range []string{"192.0.2.30", "192.0.2.31", "192.0.2.32"} {
		if _, err := service.StartDeviceLogin(context.Background(), auth.DeviceStart{
			ClientIP: clientIP, DeviceID: "device-rate-device", RequestBytes: 128,
		}); err != nil {
			t.Fatalf("device rate-window start %d: %v", index+1, err)
		}
	}
	if _, err := service.StartDeviceLogin(context.Background(), auth.DeviceStart{
		ClientIP: "192.0.2.33", DeviceID: "device-rate-device", RequestBytes: 128,
	}); err != auth.ErrRateLimited {
		t.Fatalf("fourth device rate-window start error = %v, want ErrRateLimited", err)
	}

	disabledApprover := start("192.0.2.40", "device-disabled-approver")
	fixture.execMigrator(t, fixture.alpha, `
		UPDATE pm.auth_bootstrap_identities
		SET enabled = false, disabled_at = clock_timestamp()
		WHERE deployment_id = $1 AND issuer = $2 AND subject = $3
	`, fixture.deployment, oidcHarness.issuer, oidcHarness.subject)
	if err := service.ApproveDeviceLoginFromBrowser(context.Background(), decision(disabledApprover)); err != auth.ErrChallengeInvalid {
		t.Fatalf("disabled current approver error = %v, want ErrChallengeInvalid", err)
	}
	fixture.execMigrator(t, fixture.alpha, `
		UPDATE pm.auth_bootstrap_identities
		SET enabled = true, disabled_at = NULL
		WHERE deployment_id = $1 AND issuer = $2 AND subject = $3
	`, fixture.deployment, oidcHarness.issuer, oidcHarness.subject)
	browserIdentity, err := service.ValidateCSRF(context.Background(), auth.BrowserCSRFRequest{
		CookieToken: browserSession.Token, CSRFToken: browserSession.CSRFToken,
	})
	if err != nil {
		t.Fatalf("ValidateCSRF(browser approver): %v", err)
	}
	if err := service.RevokeSession(context.Background(), browserIdentity); err != nil {
		t.Fatalf("RevokeSession(browser approver): %v", err)
	}
	revokedApprover := start("192.0.2.41", "device-revoked-approver")
	if err := service.ApproveDeviceLoginFromBrowser(context.Background(), decision(revokedApprover)); err != auth.ErrInvalidCSRF {
		t.Fatalf("revoked browser approver error = %v, want ErrInvalidCSRF", err)
	}
}
