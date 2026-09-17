package auth_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"golang.org/x/oauth2"
)

type oidcTokenSpec struct {
	claims           map[string]any
	expectedVerifier string
	key              *rsa.PrivateKey
}

type oidcHarness struct {
	issuer  string
	client  string
	subject string
	key     *rsa.PrivateKey
	server  *httptest.Server
	mu      sync.Mutex
	tokens  map[string]oidcTokenSpec
	hits    atomic.Int64
}

func newOIDCHarness(t *testing.T) *oidcHarness {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate OIDC signing key: %v", err)
	}
	harness := &oidcHarness{
		issuer: "https://identity.example.test", client: "pm-test-client",
		subject: "subject-ada", key: key, tokens: make(map[string]oidcTokenSpec),
	}
	harness.server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		harness.hits.Add(1)
		if err := request.ParseForm(); err != nil {
			http.Error(response, "invalid form", http.StatusBadRequest)
			return
		}
		harness.mu.Lock()
		spec, ok := harness.tokens[request.Form.Get("code")]
		harness.mu.Unlock()
		if !ok || request.Form.Get("code_verifier") != spec.expectedVerifier {
			http.Error(response, "invalid grant", http.StatusBadRequest)
			return
		}
		rawToken := signTestIDToken(t, spec.key, spec.claims)
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(map[string]any{
			"access_token": "test-access-token",
			"token_type":   "Bearer",
			"expires_in":   300,
			"id_token":     rawToken,
		})
	}))
	t.Cleanup(harness.server.Close)
	return harness
}

func (harness *oidcHarness) config() auth.OIDCConfig {
	return auth.OIDCConfig{
		Issuer: harness.issuer,
		OAuth2: oauth2.Config{
			ClientID: harness.client,
			Endpoint: oauth2.Endpoint{
				AuthURL: harness.issuer + "/authorize", TokenURL: harness.server.URL,
			},
			RedirectURL: "https://pm.example.test/v1/sessions/browser/callback",
			Scopes:      []string{oidc.ScopeOpenID},
		},
		KeySet:   &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&harness.key.PublicKey}},
		LoginTTL: 5 * time.Minute,
	}
}

func (harness *oidcHarness) context() context.Context {
	return context.WithValue(context.Background(), oauth2.HTTPClient, harness.server.Client())
}

func (harness *oidcHarness) register(
	code string,
	challenge auth.OIDCLoginChallenge,
	authTime any,
	mutate func(map[string]any),
	signingKey *rsa.PrivateKey,
) {
	claims := map[string]any{
		"iss":   harness.issuer,
		"sub":   harness.subject,
		"aud":   harness.client,
		"exp":   time.Now().Add(5 * time.Minute).Unix(),
		"iat":   time.Now().Add(-time.Second).Unix(),
		"nonce": challenge.Nonce,
	}
	if authTime != nil {
		claims["auth_time"] = authTime
	}
	if mutate != nil {
		mutate(claims)
	}
	if signingKey == nil {
		signingKey = harness.key
	}
	harness.mu.Lock()
	harness.tokens[code] = oidcTokenSpec{
		claims: claims, expectedVerifier: challenge.PKCEVerifier, key: signingKey,
	}
	harness.mu.Unlock()
}

func signTestIDToken(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	headerJSON, err := json.Marshal(map[string]any{"alg": "RS256", "kid": "pm-test-key", "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal JWT header: %v", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal JWT claims: %v", err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	signingInput := encode(headerJSON) + "." + encode(claimsJSON)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return signingInput + "." + encode(signature)
}

func TestCookieCSRFAndDeviceSessions(t *testing.T) {
	fixture := newAuthFixture(t)
	harness := newOIDCHarness(t)
	service, err := auth.NewService(fixture.serviceConfig("", harness.config()))
	if err != nil {
		t.Fatalf("NewService(): %v", err)
	}
	fixture.addBootstrapIdentity(t, harness.issuer, harness.subject, fixture.ada, true)

	begin := func(reauthenticate bool) auth.OIDCLoginChallenge {
		t.Helper()
		challenge, beginErr := service.BeginOIDCLogin(context.Background(), auth.OIDCLoginStart{
			TLS: true, Host: testHost, Origin: testOrigin, Reauthenticate: reauthenticate,
		})
		if beginErr != nil {
			t.Fatalf("BeginOIDCLogin(reauthenticate=%v): %v", reauthenticate, beginErr)
		}
		return challenge
	}
	callback := func(challenge auth.OIDCLoginChallenge, code string) auth.OIDCLoginCallback {
		return auth.OIDCLoginCallback{
			ChallengeID: challenge.ChallengeID, Code: code,
			State: challenge.State, PKCEVerifier: challenge.PKCEVerifier,
			TLS: true, Host: testHost, Origin: testOrigin,
		}
	}

	normal := begin(false)
	authorizationURL, err := url.Parse(normal.AuthorizationURL)
	if err != nil {
		t.Fatalf("parse normal authorization URL: %v", err)
	}
	query := authorizationURL.Query()
	if query.Get("state") != normal.State || query.Get("nonce") != normal.Nonce ||
		query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") == "" ||
		query.Has("max_age") || query.Has("prompt") {
		t.Fatalf("normal authorization query = %v", query)
	}
	if normal.ReauthenticationRequested {
		t.Fatal("normal challenge marked as reauthentication")
	}
	encodedChallenge, err := json.Marshal(normal)
	if err != nil {
		t.Fatalf("json.Marshal(OIDCLoginChallenge): %v", err)
	}
	if strings.Contains(string(encodedChallenge), normal.PKCEVerifier) {
		t.Fatal("PKCE verifier crossed the JSON response boundary")
	}

	initialHits := harness.hits.Load()
	wrongState := callback(normal, "wrong-state")
	wrongState.State += "-wrong"
	if _, err := service.CompleteOIDCLogin(harness.context(), wrongState); err != auth.ErrOIDCVerification {
		t.Fatalf("wrong state error = %v, want ErrOIDCVerification", err)
	}
	if harness.hits.Load() != initialHits {
		t.Fatal("wrong state reached the token endpoint")
	}
	wrongPKCEChallenge := begin(false)
	wrongPKCE := callback(wrongPKCEChallenge, "wrong-pkce")
	wrongPKCE.PKCEVerifier += "-wrong"
	if _, err := service.CompleteOIDCLogin(harness.context(), wrongPKCE); err != auth.ErrOIDCVerification {
		t.Fatalf("wrong PKCE verifier error = %v, want ErrOIDCVerification", err)
	}
	if harness.hits.Load() != initialHits {
		t.Fatal("wrong PKCE verifier reached the token endpoint")
	}

	witnessSeconds := time.Now().Add(-10 * time.Second).Unix()
	harness.register("normal-success", normal, witnessSeconds, nil, nil)
	result, err := service.CompleteOIDCLogin(harness.context(), callback(normal, "normal-success"))
	if err != nil {
		t.Fatalf("CompleteOIDCLogin(normal): %v", err)
	}
	witness := time.Unix(witnessSeconds, 0).UTC()
	if result.Session.Transport != store.SessionTransportBrowser ||
		result.Session.Principal != fixture.ada || !result.Session.AuthenticatedAt.Equal(witness) ||
		result.Session.Token == "" || result.Session.CSRFToken == "" {
		t.Fatalf("browser session = %#v", result.Session)
	}
	if result.Cookie.Name != auth.BrowserSessionCookieName || result.Cookie.Value != result.Session.Token ||
		result.Cookie.Path != "/" || !result.Cookie.Secure || !result.Cookie.HTTPOnly ||
		result.Cookie.SameSite != "Lax" {
		t.Fatalf("browser cookie = %#v", result.Cookie)
	}
	encodedResult, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal(OIDCLoginResult): %v", err)
	}
	if strings.Contains(string(encodedResult), result.Session.Token) ||
		strings.Contains(string(encodedResult), result.Session.CSRFToken) {
		t.Fatal("browser credentials crossed the JSON response boundary")
	}
	identity, err := service.Authenticate(context.Background(), auth.SessionAuthentication{
		BrowserCookie: result.Cookie.Value,
	})
	if err != nil {
		t.Fatalf("Authenticate(browser cookie): %v", err)
	}
	if !identity.AuthenticatedAt.Equal(witness) || identity.Source != auth.AuthenticationBrowserSession {
		t.Fatalf("browser identity = %#v", identity)
	}
	csrfIdentity, err := service.ValidateCSRF(context.Background(), auth.BrowserCSRFRequest{
		CookieToken: result.Cookie.Value, CSRFToken: result.Session.CSRFToken,
	})
	if err != nil {
		t.Fatalf("ValidateCSRF(): %v", err)
	}
	if !csrfIdentity.AuthenticatedAt.Equal(witness) {
		t.Fatalf("CSRF identity authenticated_at = %v, want %v", csrfIdentity.AuthenticatedAt, witness)
	}
	if _, err := service.ValidateCSRF(context.Background(), auth.BrowserCSRFRequest{
		CookieToken: result.Cookie.Value, CSRFToken: result.Session.CSRFToken + "-wrong",
	}); err != auth.ErrInvalidCSRF {
		t.Fatalf("wrong CSRF error = %v, want ErrInvalidCSRF", err)
	}
	if _, err := service.Authenticate(context.Background(), auth.SessionAuthentication{
		BearerToken: result.Cookie.Value,
	}); err != auth.ErrAuthenticationRequired {
		t.Fatalf("browser token accepted as bearer: %v", err)
	}
	wrongIdentity := identity
	wrongIdentity.Principal = fixture.bob
	wrongIdentity.Actor = directTestActor(fixture.bob)
	if err := service.RevokeSession(context.Background(), wrongIdentity); err == nil {
		t.Fatal("different principal revoked a browser session by session ID")
	}
	if _, err := service.Authenticate(context.Background(), auth.SessionAuthentication{
		BrowserCookie: result.Cookie.Value,
	}); err != nil {
		t.Fatalf("browser session disappeared after foreign revoke attempt: %v", err)
	}
	if err := service.RevokeSession(context.Background(), identity); err != nil {
		t.Fatalf("RevokeSession(): %v", err)
	}
	if _, err := service.Authenticate(context.Background(), auth.SessionAuthentication{
		BrowserCookie: result.Cookie.Value,
	}); err != auth.ErrAuthenticationRequired {
		t.Fatalf("revoked browser session error = %v, want ErrAuthenticationRequired", err)
	}
	if _, err := service.CompleteOIDCLogin(harness.context(), callback(normal, "normal-success")); err != auth.ErrOIDCVerification {
		t.Fatalf("reused OIDC challenge error = %v, want ErrOIDCVerification", err)
	}

	withoutWitness := begin(false)
	harness.register("normal-no-auth-time", withoutWitness, nil, nil, nil)
	missingResult, err := service.CompleteOIDCLogin(harness.context(), callback(withoutWitness, "normal-no-auth-time"))
	if err != nil {
		t.Fatalf("normal login without auth_time: %v", err)
	}
	if !missingResult.Session.AuthenticatedAt.IsZero() {
		t.Fatalf("normal login missing auth_time produced witness %v", missingResult.Session.AuthenticatedAt)
	}

	stepUp := begin(true)
	stepUpURL, err := url.Parse(stepUp.AuthorizationURL)
	if err != nil {
		t.Fatalf("parse step-up authorization URL: %v", err)
	}
	if stepUpURL.Query().Get("max_age") != "0" || stepUpURL.Query().Get("prompt") != "login" ||
		!stepUp.ReauthenticationRequested {
		t.Fatalf("step-up authorization query = %v, challenge = %#v", stepUpURL.Query(), stepUp)
	}
	freshSeconds := time.Now().UTC().Unix()
	harness.register("step-up-success", stepUp, freshSeconds, nil, nil)
	stepUpResult, err := service.CompleteOIDCLogin(harness.context(), callback(stepUp, "step-up-success"))
	if err != nil {
		t.Fatalf("CompleteOIDCLogin(step-up): %v", err)
	}
	if !stepUpResult.Session.AuthenticatedAt.Equal(time.Unix(freshSeconds, 0).UTC()) {
		t.Fatalf("step-up authenticated_at = %v", stepUpResult.Session.AuthenticatedAt)
	}

	rejectionCases := []struct {
		name     string
		authTime any
		mutate   func(map[string]any)
		key      *rsa.PrivateKey
	}{
		{name: "missing auth time"},
		{name: "stale auth time", authTime: time.Now().Add(-time.Minute).Unix()},
		{name: "malformed auth time", authTime: "not-a-numeric-date"},
		{name: "future auth time", authTime: time.Now().Add(time.Minute).Unix()},
		{name: "wrong nonce", authTime: time.Now().Unix(), mutate: func(claims map[string]any) { claims["nonce"] = "wrong-nonce" }},
		{name: "wrong issuer", authTime: time.Now().Unix(), mutate: func(claims map[string]any) { claims["iss"] = "https://attacker.example.test" }},
		{name: "wrong audience", authTime: time.Now().Unix(), mutate: func(claims map[string]any) { claims["aud"] = "another-client" }},
		{name: "expired token", authTime: time.Now().Unix(), mutate: func(claims map[string]any) { claims["exp"] = time.Now().Add(-time.Minute).Unix() }},
	}
	wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate wrong signing key: %v", err)
	}
	rejectionCases = append(rejectionCases, struct {
		name     string
		authTime any
		mutate   func(map[string]any)
		key      *rsa.PrivateKey
	}{name: "wrong signature", authTime: time.Now().Unix(), key: wrongKey})
	for index, test := range rejectionCases {
		t.Run(test.name, func(t *testing.T) {
			challenge := begin(true)
			code := "reject-" + test.name
			harness.register(code, challenge, test.authTime, test.mutate, test.key)
			if _, err := service.CompleteOIDCLogin(harness.context(), callback(challenge, code)); err != auth.ErrOIDCVerification {
				t.Fatalf("CompleteOIDCLogin() error = %v, want ErrOIDCVerification (case %d)", err, index)
			}
		})
	}

	expiredChallenge := begin(false)
	fixture.expireOIDCChallenge(t, expiredChallenge.ChallengeID)
	harness.register("expired-challenge", expiredChallenge, nil, nil, nil)
	hitsBeforeExpired := harness.hits.Load()
	if _, err := service.CompleteOIDCLogin(harness.context(), callback(expiredChallenge, "expired-challenge")); err != auth.ErrOIDCVerification {
		t.Fatalf("expired challenge error = %v, want ErrOIDCVerification", err)
	}
	if harness.hits.Load() != hitsBeforeExpired {
		t.Fatal("expired challenge reached the token endpoint")
	}
	if count := fixture.sessionCount(t); count != 3 {
		t.Fatalf("persisted browser session count = %d, want 3 successful signed logins", count)
	}
}
