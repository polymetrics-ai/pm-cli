package server

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"html/template"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/credentials"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

const (
	integrationOIDCClientID         = "pm-server-integration"
	integrationSecretReference      = "secret://fixture/server"
	integrationBrowserReadyFileEnv  = "PM_SERVER_BROWSER_PROBE_READY_FILE"
	integrationBrowserStopFileEnv   = "PM_SERVER_BROWSER_PROBE_STOP_FILE"
	integrationBrowserPauseEnv      = "PM_SERVER_BROWSER_PROBE_TIMEOUT"
	integrationMaximumResponseBytes = 2 << 20
)

type integrationAuthTimeMode string

const (
	integrationAuthTimeFresh  integrationAuthTimeMode = "fresh"
	integrationAuthTimeStale  integrationAuthTimeMode = "stale"
	integrationAuthTimeAbsent integrationAuthTimeMode = "absent"
)

type integrationAuthorizationCode struct {
	redirectURI   string
	nonce         string
	pkceChallenge string
	authTime      integrationAuthTimeMode
}

type integrationAuthorizationRequest struct {
	ClientID            string
	RedirectURI         string
	Scope               string
	State               string
	Nonce               string
	PKCEChallenge       string
	PKCEChallengeMethod string
}

type integrationOIDCFixture struct {
	server        *httptest.Server
	issuer        string
	clientSecret  string
	caCertificate *x509.Certificate
	caKey         *rsa.PrivateKey
	signingKey    *rsa.PrivateKey

	mu    sync.Mutex
	codes map[string]integrationAuthorizationCode
}

var integrationConsentTemplate = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>PM integration identity provider</title>
</head>
<body>
  <main>
    <h1>PM integration sign-in</h1>
    <p>This local provider will authenticate the synthetic integration operator for <code>{{.ClientID}}</code>.</p>
    <form method="post" action="{{.AuthorizationEndpoint}}">
      <input type="hidden" name="response_type" value="code">
      <input type="hidden" name="client_id" value="{{.ClientID}}">
      <input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
      <input type="hidden" name="scope" value="{{.Scope}}">
      <input type="hidden" name="state" value="{{.State}}">
      <input type="hidden" name="nonce" value="{{.Nonce}}">
      <input type="hidden" name="code_challenge" value="{{.PKCEChallenge}}">
      <input type="hidden" name="code_challenge_method" value="{{.PKCEChallengeMethod}}">
      <label>Signed authentication time
        <select name="auth_time_mode">
          <option value="fresh">Fresh</option>
          <option value="stale">Stale</option>
          <option value="absent">Absent</option>
        </select>
      </label>
      <button type="submit" name="decision" value="allow">Continue as integration operator</button>
      <button type="submit" name="decision" value="deny">Deny</button>
    </form>
  </main>
</body>
</html>`))

func newIntegrationOIDCFixture(t *testing.T) *integrationOIDCFixture {
	t.Helper()
	caCertificate, caKey := newIntegrationCertificateAuthority(t)
	serverCertificate := issueIntegrationServerCertificate(t, caCertificate, caKey)
	signingKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate integration OIDC signing key: %T", err)
	}
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		t.Fatalf("generate integration OIDC client secret: %T", err)
	}
	fixture := &integrationOIDCFixture{
		clientSecret:  base64.RawURLEncoding.EncodeToString(secretBytes),
		caCertificate: caCertificate,
		caKey:         caKey,
		signingKey:    signingKey,
		codes:         make(map[string]integrationAuthorizationCode),
	}
	clear(secretBytes)
	fixture.server = httptest.NewUnstartedServer(http.HandlerFunc(fixture.serveHTTP))
	fixture.server.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCertificate},
		MinVersion:   tls.VersionTLS12,
	}
	fixture.server.StartTLS()
	fixture.issuer = fixture.server.URL
	t.Cleanup(fixture.server.Close)
	return fixture
}

func newIntegrationCertificateAuthority(t *testing.T) (*x509.Certificate, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate integration certificate authority key: %T", err)
	}
	serial := integrationCertificateSerial(t)
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "PM server integration fixture CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	encoded, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create integration certificate authority: %T", err)
	}
	certificate, err := x509.ParseCertificate(encoded)
	if err != nil {
		t.Fatalf("parse integration certificate authority: %T", err)
	}
	return certificate, key
}

func issueIntegrationServerCertificate(
	t *testing.T,
	caCertificate *x509.Certificate,
	caKey *rsa.PrivateKey,
) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate integration HTTPS key: %T", err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: integrationCertificateSerial(t),
		Subject:      pkix.Name{CommonName: "PM server integration fixture"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	encoded, err := x509.CreateCertificate(rand.Reader, template, caCertificate, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create integration HTTPS certificate: %T", err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: encoded})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		t.Fatalf("load integration HTTPS certificate: %T", err)
	}
	return certificate
}

func integrationCertificateSerial(t *testing.T) *big.Int {
	t.Helper()
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		t.Fatalf("generate integration certificate serial: %T", err)
	}
	if serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	return serial
}

func (fixture *integrationOIDCFixture) serveHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	switch request.URL.Path {
	case "/.well-known/openid-configuration":
		fixture.serveDiscovery(response, request)
	case "/authorize":
		fixture.serveAuthorization(response, request)
	case "/token":
		fixture.serveToken(response, request)
	case "/jwks":
		fixture.serveJWKS(response, request)
	default:
		http.NotFound(response, request)
	}
}

func (fixture *integrationOIDCFixture) serveDiscovery(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(map[string]any{
		"issuer":                                fixture.issuer,
		"authorization_endpoint":                fixture.issuer + "/authorize",
		"token_endpoint":                        fixture.issuer + "/token",
		"jwks_uri":                              fixture.issuer + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (fixture *integrationOIDCFixture) serveAuthorization(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodPost {
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := request.ParseForm(); err != nil {
		http.Error(response, "invalid authorization request", http.StatusBadRequest)
		return
	}
	authorization, err := fixture.parseAuthorizationRequest(request.Form)
	if err != nil {
		http.Error(response, "invalid authorization request", http.StatusBadRequest)
		return
	}
	redirect, err := url.Parse(authorization.RedirectURI)
	if err != nil {
		http.Error(response, "invalid authorization redirect", http.StatusBadRequest)
		return
	}
	if request.Method == http.MethodGet {
		response.Header().Set("Content-Type", "text/html; charset=utf-8")
		response.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self' "+redirect.Scheme+"://"+redirect.Host+"; base-uri 'none'; frame-ancestors 'none'")
		data := struct {
			AuthorizationEndpoint string
			integrationAuthorizationRequest
		}{AuthorizationEndpoint: fixture.issuer + "/authorize", integrationAuthorizationRequest: authorization}
		if err := integrationConsentTemplate.Execute(response, data); err != nil {
			http.Error(response, "authorization page unavailable", http.StatusInternalServerError)
		}
		return
	}

	query := redirect.Query()
	if request.Form.Get("decision") == "deny" {
		query.Set("error", "access_denied")
		query.Set("state", authorization.State)
		redirect.RawQuery = query.Encode()
		http.Redirect(response, request, redirect.String(), http.StatusSeeOther)
		return
	}
	mode := integrationAuthTimeMode(request.Form.Get("auth_time_mode"))
	if mode == "" {
		mode = integrationAuthTimeFresh
	}
	if mode != integrationAuthTimeFresh && mode != integrationAuthTimeStale && mode != integrationAuthTimeAbsent {
		http.Error(response, "invalid authentication time selection", http.StatusBadRequest)
		return
	}
	code, err := integrationOpaque("oidc-code-", 24)
	if err != nil {
		http.Error(response, "authorization unavailable", http.StatusInternalServerError)
		return
	}
	fixture.mu.Lock()
	fixture.codes[code] = integrationAuthorizationCode{
		redirectURI:   authorization.RedirectURI,
		nonce:         authorization.Nonce,
		pkceChallenge: authorization.PKCEChallenge,
		authTime:      mode,
	}
	fixture.mu.Unlock()
	query.Set("code", code)
	query.Set("state", authorization.State)
	redirect.RawQuery = query.Encode()
	http.Redirect(response, request, redirect.String(), http.StatusSeeOther)
}

func (fixture *integrationOIDCFixture) parseAuthorizationRequest(values url.Values) (integrationAuthorizationRequest, error) {
	value := func(name string) (string, bool) {
		items, ok := values[name]
		returnValue := ""
		if len(items) == 1 {
			returnValue = items[0]
		}
		return returnValue, ok && len(items) == 1 && returnValue != ""
	}
	responseType, responseTypeOK := value("response_type")
	clientID, clientIDOK := value("client_id")
	redirectURI, redirectOK := value("redirect_uri")
	scope, scopeOK := value("scope")
	state, stateOK := value("state")
	nonce, nonceOK := value("nonce")
	challenge, challengeOK := value("code_challenge")
	challengeMethod, challengeMethodOK := value("code_challenge_method")
	if !responseTypeOK || responseType != "code" || !clientIDOK || clientID != integrationOIDCClientID ||
		!redirectOK || !scopeOK || !stateOK || !nonceOK || !challengeOK ||
		!challengeMethodOK || challengeMethod != "S256" || !strings.Contains(" "+scope+" ", " openid ") ||
		len(state) > 512 || len(nonce) > 512 || len(challenge) > 512 || !integrationLocalHTTPSURL(redirectURI) {
		return integrationAuthorizationRequest{}, errors.New("authorization request is invalid")
	}
	return integrationAuthorizationRequest{
		ClientID:            clientID,
		RedirectURI:         redirectURI,
		Scope:               scope,
		State:               state,
		Nonce:               nonce,
		PKCEChallenge:       challenge,
		PKCEChallengeMethod: challengeMethod,
	}, nil
}

func (fixture *integrationOIDCFixture) serveToken(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	clientID, clientSecret, ok := request.BasicAuth()
	if !ok || clientID != integrationOIDCClientID || clientSecret != fixture.clientSecret {
		response.Header().Set("WWW-Authenticate", `Basic realm="pm-integration"`)
		http.Error(response, "invalid client", http.StatusUnauthorized)
		return
	}
	if err := request.ParseForm(); err != nil || request.Form.Get("grant_type") != "authorization_code" {
		http.Error(response, "invalid grant", http.StatusBadRequest)
		return
	}
	code := request.Form.Get("code")
	fixture.mu.Lock()
	authorization, found := fixture.codes[code]
	if found && request.Form.Get("redirect_uri") == authorization.redirectURI &&
		integrationPKCEChallenge(request.Form.Get("code_verifier")) == authorization.pkceChallenge {
		delete(fixture.codes, code)
	} else {
		found = false
	}
	fixture.mu.Unlock()
	if !found {
		http.Error(response, "invalid grant", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	claims := map[string]any{
		"iss":   fixture.issuer,
		"sub":   "integration-operator",
		"aud":   integrationOIDCClientID,
		"exp":   now.Add(5 * time.Minute).Unix(),
		"iat":   now.Add(-time.Second).Unix(),
		"nonce": authorization.nonce,
	}
	switch authorization.authTime {
	case integrationAuthTimeFresh:
		claims["auth_time"] = now.Unix()
	case integrationAuthTimeStale:
		claims["auth_time"] = now.Add(-time.Hour).Unix()
	case integrationAuthTimeAbsent:
	default:
		http.Error(response, "invalid grant", http.StatusBadRequest)
		return
	}
	idToken, err := fixture.signIDToken(claims)
	if err != nil {
		http.Error(response, "token unavailable", http.StatusInternalServerError)
		return
	}
	accessToken, err := integrationOpaque("fixture-access-", 24)
	if err != nil {
		http.Error(response, "token unavailable", http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(map[string]any{
		"access_token": accessToken,
		"token_type":   "Bearer",
		"expires_in":   300,
		"id_token":     idToken,
	})
}

func (fixture *integrationOIDCFixture) serveJWKS(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	exponent := make([]byte, 8)
	binary.BigEndian.PutUint64(exponent, uint64(fixture.signingKey.PublicKey.E))
	exponent = bytes.TrimLeft(exponent, "\x00")
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": "pm-integration-key",
			"n":   base64.RawURLEncoding.EncodeToString(fixture.signingKey.PublicKey.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(exponent),
		}},
	})
}

func (fixture *integrationOIDCFixture) signIDToken(claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]any{"alg": "RS256", "kid": "pm-integration-key", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encode := base64.RawURLEncoding.EncodeToString
	signingInput := encode(header) + "." + encode(payload)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, fixture.signingKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + encode(signature), nil
}

func integrationPKCEChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func integrationOpaque(prefix string, byteCount int) (string, error) {
	value := make([]byte, byteCount)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(value), nil
}

func integrationLocalHTTPSURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	if strings.EqualFold(parsed.Hostname(), "localhost") {
		return true
	}
	address := net.ParseIP(parsed.Hostname())
	return address != nil && address.IsLoopback()
}

type serverIntegrationFixture struct {
	oidc                *integrationOIDCFixture
	application         *Application
	configuration       config.Configuration
	serveCancel         context.CancelFunc
	serveResult         chan error
	origin              string
	secretsRoot         string
	wrappingPath        string
	wrappingKey         []byte
	runtimeDSN          string
	migratorDSN         string
	deploymentID        string
	scope               contract.Scope
	foreignScope        contract.Scope
	operator            contract.Principal
	foreignUser         contract.Principal
	grantTarget         contract.Principal
	connectionID        string
	foreignConnectionID string
	providerAccountID   string
	accountWitness      string
}

func newServerIntegrationFixture(t *testing.T) *serverIntegrationFixture {
	t.Helper()
	oidcFixture := newIntegrationOIDCFixture(t)
	runtimeDSN, migratorDSN := loadIntegrationStoreCredentials(t)
	migrationContext, cancelMigration := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelMigration()
	if err := store.Migrate(migrationContext, migratorDSN); err != nil {
		t.Fatalf("migrate selected server integration store: %T", err)
	}

	suffix := integrationRandomHex(t, 8)
	fixture := &serverIntegrationFixture{
		oidc:         oidcFixture,
		runtimeDSN:   runtimeDSN,
		migratorDSN:  migratorDSN,
		deploymentID: "server-integration-" + suffix,
		scope: contract.Scope{
			OrgID:       "org-server-" + suffix,
			WorkspaceID: "workspace-server-" + suffix,
		},
		foreignScope: contract.Scope{
			OrgID:       "org-server-" + suffix,
			WorkspaceID: "workspace-foreign-" + suffix,
		},
		connectionID:        "connection-server-" + suffix,
		foreignConnectionID: "connection-foreign-" + suffix,
		providerAccountID:   "provider-server-" + suffix,
		accountWitness:      "account-witness-server-" + suffix,
	}
	fixture.operator = contract.Principal{OrgID: fixture.scope.OrgID, PrincipalID: "operator-" + suffix, Kind: contract.PrincipalUser}
	fixture.foreignUser = contract.Principal{OrgID: fixture.scope.OrgID, PrincipalID: "foreign-user-" + suffix, Kind: contract.PrincipalUser}
	fixture.grantTarget = contract.Principal{OrgID: fixture.scope.OrgID, PrincipalID: "grant-target-" + suffix, Kind: contract.PrincipalUser}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("open server integration listener: %T", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	// Match the executable's raw HTTP listener behind an explicitly trusted
	// HTTPS edge. Wrapping a TLS listener in the connection limiter hides its
	// TLS connection metadata from net/http.
	backendURL := &url.URL{Scheme: "http", Host: listener.Addr().String()}
	edgeTransport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		MaxConnsPerHost:       64,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}
	t.Cleanup(edgeTransport.CloseIdleConnections)
	edge := httptest.NewUnstartedServer(&httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(backendURL)
			request.Out.Host = request.In.Host
			request.SetXForwarded()
		},
		Transport: edgeTransport,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "fixture backend unavailable", http.StatusBadGateway)
		},
	})
	t.Cleanup(edge.Close)
	serverCertificate := issueIntegrationServerCertificate(t, fixture.oidc.caCertificate, fixture.oidc.caKey)
	edge.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCertificate},
		MinVersion:   tls.VersionTLS12,
	}
	edge.Config.ReadHeaderTimeout = 2 * time.Second
	edge.Config.IdleTimeout = 30 * time.Second
	port := edge.Listener.Addr().(*net.TCPAddr).Port
	fixture.origin = fmt.Sprintf("https://localhost:%d", port)

	fixture.materializeOperatorSecrets(t)
	fixture.configuration = fixture.loadConfiguration(t, listener.Addr().String())
	t.Cleanup(func() { fixture.cleanupDatabase(t) })
	fixture.seedDatabase(t)

	factoryCalled := false
	application, err := openWithOIDCFactory(
		context.Background(),
		fixture.configuration,
		fixture.secretsRoot,
		io.Discard,
		func(trustRoot *x509.CertPool) (*http.Client, *boundedOIDCTransport) {
			factoryCalled = true
			if trustRoot == nil {
				t.Fatal("authenticated server integration did not resolve its declared identity trust root")
			}
			return newOIDCHTTPClient(trustRoot)
		},
	)
	if err != nil {
		t.Fatalf("open authenticated server integration application: %v", err)
	}
	fixture.application = application
	t.Cleanup(func() { fixture.closeApplication(t) })
	if !factoryCalled {
		t.Fatal("authenticated server integration did not allocate its bounded OIDC client")
	}
	serveContext, cancelServe := context.WithCancel(context.Background())
	fixture.serveCancel = cancelServe
	fixture.serveResult = make(chan error, 1)
	go func() {
		fixture.serveResult <- fixture.application.Serve(serveContext, listener)
	}()
	edge.StartTLS()
	fixture.waitForLiveApplication(t)
	return fixture
}

func loadIntegrationStoreCredentials(t *testing.T) (string, string) {
	t.Helper()
	secretsDirectory := os.Getenv("PM_STORE_TEST_SECRETS_DIR")
	if secretsDirectory == "" {
		t.Fatal("PM_STORE_TEST_SECRETS_DIR is required")
	}
	absolute, err := filepath.Abs(secretsDirectory)
	if err != nil {
		t.Fatal("resolve server integration fixture secret directory")
	}
	endpointBytes, err := os.ReadFile(filepath.Join(absolute, "pm-app-endpoint"))
	if err != nil {
		t.Fatal("read server integration runtime endpoint")
	}
	passwordBytes, err := os.ReadFile(filepath.Join(absolute, "pm-app-migrator-password"))
	if err != nil {
		t.Fatal("read server integration migrator password")
	}
	endpoint := strings.TrimSpace(string(endpointBytes))
	password := strings.TrimSpace(string(passwordBytes))
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.User == nil ||
		parsed.User.Username() != store.RuntimeRole || strings.TrimPrefix(parsed.Path, "/") != store.ApplicationDatabase || password == "" {
		t.Fatal("server integration PostgreSQL fixture identity is invalid")
	}
	migrator := *parsed
	migrator.User = url.UserPassword(store.MigratorRole, password)
	return endpoint, migrator.String()
}

func (fixture *serverIntegrationFixture) materializeOperatorSecrets(t *testing.T) {
	t.Helper()
	fixture.secretsRoot = t.TempDir()
	bundle := filepath.Join(fixture.secretsRoot, "fixture", "server")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatalf("create private server integration operator bundle: %T", err)
	}
	fixture.wrappingKey = make([]byte, wrappingKeyBytes)
	if _, err := rand.Read(fixture.wrappingKey); err != nil {
		t.Fatalf("generate synthetic integration wrapping key: %T", err)
	}
	members := []struct {
		name  string
		value []byte
	}{
		{name: "pm-app-endpoint", value: []byte(fixture.runtimeDSN)},
		{name: oidcClientSecretMember, value: []byte(fixture.oidc.clientSecret)},
		{name: oidcTrustRootMember, value: pem.EncodeToMemory(&pem.Block{
			Type:  "CERTIFICATE",
			Bytes: fixture.oidc.caCertificate.Raw,
		})},
		{name: wrappingKeyMember, value: fixture.wrappingKey},
	}
	for _, member := range members {
		if err := os.WriteFile(filepath.Join(bundle, member.name), member.value, 0o600); err != nil {
			t.Fatalf("write private server integration operator bundle member: %T", err)
		}
	}
	fixture.wrappingPath = filepath.Join(bundle, wrappingKeyMember)
}

func (fixture *serverIntegrationFixture) loadConfiguration(t *testing.T, listen string) config.Configuration {
	t.Helper()
	documentBytes, err := json.Marshal(map[string]any{
		"schema_version": "1",
		"deployment_id":  fixture.deploymentID,
		"public_origin":  fixture.origin,
		"server": map[string]any{
			"listen":              listen,
			"trusted_proxies":     []string{"127.0.0.1/32"},
			"read_header_timeout": "2s",
			"request_timeout":     "10s",
			"write_timeout":       "10s",
			"idle_timeout":        "30s",
			"drain_timeout":       "2s",
			"probe_timeout":       "5s",
			"max_header_bytes":    65536,
			"max_connections":     64,
		},
		"postgres": map[string]any{
			"ownership":    "managed",
			"database":     store.ApplicationDatabase,
			"endpoint_ref": integrationSecretReference,
		},
		"temporal": map[string]any{
			"ownership":           "managed",
			"endpoint":            "temporal.example.invalid:7233",
			"namespace":           "server-integration",
			"client_identity_ref": "secret://fixture/temporal",
			"retention":           "720h",
			"task_queues":         []string{"pm-control"},
		},
		"artifacts": map[string]any{
			"ownership":          "managed",
			"driver":             "s3",
			"endpoint":           "https://objects.example.invalid",
			"bucket":             "server-integration",
			"credential_ref":     "secret://fixture/artifacts",
			"attempt_key_policy": "unique_immutable",
		},
		"identity": map[string]any{
			"issuer":              fixture.oidc.issuer,
			"client_id":           integrationOIDCClientID,
			"client_secret_ref":   integrationSecretReference,
			"step_up_maximum_age": "2m",
			"ca_ref":              integrationSecretReference,
		},
		"encryption": map[string]any{
			"wrapping_key_ref": integrationSecretReference,
		},
		"admission": map[string]any{"profile": "development"},
		"limits": map[string]any{
			"workspace_active_runs":    4,
			"workflow_payload_bytes":   65536,
			"workflow_fanout":          32,
			"run_lifetime":             "1h",
			"event_buffer":             128,
			"query_children":           1,
			"query_child_memory_bytes": uint64(4294967296),
			"query_child_deadline":     "5m",
			"request_bytes":            1048576,
			"metadata_deadline":        "5s",
		},
	})
	if err != nil {
		t.Fatalf("encode server integration configuration: %T", err)
	}
	document, err := config.Load(bytes.NewReader(documentBytes))
	if err != nil {
		t.Fatalf("load server integration configuration: %v", err)
	}
	configuration, err := config.Validate(document, config.Prelaunch{})
	if err != nil {
		t.Fatalf("validate server integration configuration: %v", err)
	}
	return configuration
}

func (fixture *serverIntegrationFixture) seedDatabase(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, fixture.migratorDSN)
	if err != nil {
		t.Fatalf("connect server integration seed: %T", err)
	}
	defer connection.Close(ctx)
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin server integration seed: %T", err)
	}
	defer tx.Rollback(ctx)
	if err := bindServerIntegrationScope(ctx, tx, fixture.deploymentID, fixture.scope, fixture.operator); err != nil {
		t.Fatalf("bind primary server integration scope: %T", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pm.workspace_namespaces (org_id, workspace_id, name)
		VALUES ($1, $2, 'Primary integration workspace')
	`, fixture.scope.OrgID, fixture.scope.WorkspaceID); err != nil {
		t.Fatalf("seed primary server integration workspace: %T", err)
	}
	for _, identity := range []contract.Principal{fixture.operator, fixture.foreignUser, fixture.grantTarget} {
		issuer := fixture.oidc.issuer + "/synthetic"
		subject := identity.PrincipalID
		if identity == fixture.operator {
			issuer = fixture.oidc.issuer
			subject = "integration-operator"
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO pm.identities (org_id, identity_id, kind, issuer, subject, revision)
			VALUES ($1, $2, $3, $4, $5, 1)
		`, identity.OrgID, identity.PrincipalID, string(identity.Kind), issuer, subject); err != nil {
			t.Fatalf("seed server integration identity: %T", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pm.provider_accounts (
			org_id, provider_account_id, provider, account_witness, capacity, revision
		) VALUES ($1, $2, 'fixture', $3, 8, 1)
	`, fixture.scope.OrgID, fixture.providerAccountID, fixture.accountWitness); err != nil {
		t.Fatalf("seed server integration provider account: %T", err)
	}
	permissions := []string{
		"connection.credentials.manage",
		"connection.manage",
		"connection.read",
		"operation.invoke",
		"workspace.access.manage",
		"workspace.access.read",
		"workspace.read",
	}
	resourceGrants := []string{
		"connection:" + fixture.connectionID,
		"permission:connection.credentials.manage",
		"permission:connection.manage",
		"permission:connection.read",
		"permission:operation.invoke",
		"permission:workspace.access.manage",
		"permission:workspace.access.read",
		"permission:workspace.read",
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pm.memberships (
			org_id, workspace_id, membership_id, identity_id, role,
			resource_grants, permissions, connection_ids, operation_ids,
			policy_epoch, control_revision
		) VALUES ($1, $2, $3, $4, 'admin', $5, $6, $7, $8, 1, 1)
	`, fixture.scope.OrgID, fixture.scope.WorkspaceID, "membership-"+fixture.operator.PrincipalID,
		fixture.operator.PrincipalID, resourceGrants, permissions, []string{fixture.connectionID}, []string{}); err != nil {
		t.Fatalf("seed primary server integration membership: %T", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pm.connections (
			org_id, workspace_id, connection_id, connector_id, provider_account_id,
			revision, credential_ref, display_name, state
		) VALUES ($1, $2, $3, 'fixture-connector', $4, 1, $5, 'Primary integration connection', 'active')
	`, fixture.scope.OrgID, fixture.scope.WorkspaceID, fixture.connectionID,
		fixture.providerAccountID, "credential-"+fixture.connectionID); err != nil {
		t.Fatalf("seed primary server integration connection: %T", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pm.auth_bootstrap_identities (
			deployment_id, issuer, subject, org_id, principal_id, principal_kind, can_approve_devices
		) VALUES ($1, $2, 'integration-operator', $3, $4, 'user', true)
	`, fixture.deploymentID, fixture.oidc.issuer, fixture.scope.OrgID, fixture.operator.PrincipalID); err != nil {
		t.Fatalf("seed server integration bootstrap identity: %T", err)
	}

	if err := bindServerIntegrationScope(ctx, tx, fixture.deploymentID, fixture.foreignScope, fixture.foreignUser); err != nil {
		t.Fatalf("bind foreign server integration scope: %T", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pm.workspace_namespaces (org_id, workspace_id, name)
		VALUES ($1, $2, 'Concealed foreign workspace')
	`, fixture.foreignScope.OrgID, fixture.foreignScope.WorkspaceID); err != nil {
		t.Fatalf("seed foreign server integration workspace: %T", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pm.memberships (
			org_id, workspace_id, membership_id, identity_id, role,
			resource_grants, permissions, connection_ids, operation_ids,
			policy_epoch, control_revision
		) VALUES ($1, $2, $3, $4, 'admin', $5, $6, $7, $8, 1, 1)
	`, fixture.foreignScope.OrgID, fixture.foreignScope.WorkspaceID, "membership-"+fixture.foreignUser.PrincipalID,
		fixture.foreignUser.PrincipalID,
		[]string{"connection:" + fixture.foreignConnectionID, "permission:connection.read", "permission:workspace.read"},
		[]string{"connection.read", "workspace.read"}, []string{fixture.foreignConnectionID}, []string{}); err != nil {
		t.Fatalf("seed foreign server integration membership: %T", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pm.connections (
			org_id, workspace_id, connection_id, connector_id, provider_account_id,
			revision, credential_ref, display_name, state
		) VALUES ($1, $2, $3, 'fixture-connector', $4, 1, $5, 'Concealed foreign connection', 'active')
	`, fixture.foreignScope.OrgID, fixture.foreignScope.WorkspaceID, fixture.foreignConnectionID,
		fixture.providerAccountID, "credential-"+fixture.foreignConnectionID); err != nil {
		t.Fatalf("seed foreign server integration connection: %T", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit server integration seed: %T", err)
	}
}

func bindServerIntegrationScope(
	ctx context.Context,
	tx pgx.Tx,
	deploymentID string,
	scope contract.Scope,
	principal contract.Principal,
) error {
	_, err := tx.Exec(ctx, `
		SELECT set_config('pm.deployment_id', $1, true),
		       set_config('pm.org_id', $2, true),
		       set_config('pm.workspace_id', $3, true),
		       set_config('pm.requested_actor_id', $4, true),
		       set_config('pm.requested_actor_kind', $5, true),
		       set_config('pm.effective_actor_id', $4, true),
		       set_config('pm.effective_actor_kind', $5, true)
	`, deploymentID, scope.OrgID, scope.WorkspaceID, principal.PrincipalID, string(principal.Kind))
	return err
}

func (fixture *serverIntegrationFixture) cleanupDatabase(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, fixture.migratorDSN)
	if err != nil {
		t.Errorf("connect server integration cleanup: %T", err)
		return
	}
	defer connection.Close(ctx)
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Errorf("begin server integration cleanup: %T", err)
		return
	}
	defer tx.Rollback(ctx)
	if err := bindServerIntegrationScope(ctx, tx, fixture.deploymentID, fixture.scope, fixture.operator); err != nil {
		t.Errorf("bind server integration cleanup: %T", err)
		return
	}
	for _, statement := range []string{
		`DELETE FROM pm.pm_sessions WHERE deployment_id = $1`,
		`DELETE FROM pm.oidc_login_challenges WHERE deployment_id = $1`,
		`DELETE FROM pm.device_login_challenges WHERE deployment_id = $1`,
		`DELETE FROM pm.device_start_windows WHERE deployment_id = $1`,
		`DELETE FROM pm.auth_bootstrap_identities WHERE deployment_id = $1`,
	} {
		if _, err := tx.Exec(ctx, statement, fixture.deploymentID); err != nil {
			t.Errorf("clean server integration deployment rows: %T", err)
			return
		}
	}
	for _, scope := range []contract.Scope{fixture.scope, fixture.foreignScope} {
		if err := bindServerIntegrationScope(ctx, tx, fixture.deploymentID, scope, fixture.operator); err != nil {
			t.Errorf("bind scoped server integration cleanup: %T", err)
			return
		}
		for _, statement := range []string{
			`DELETE FROM pm.credential_refresh_leases WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.credential_mutation_receipts WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.credential_lifecycles WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.encrypted_secrets WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.delegations WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.service_account_credentials WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.service_accounts WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.identity_mutation_keys WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.identity_collection_revisions WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.memberships WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.connections WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.run_specs WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.workspace_namespaces WHERE org_id = $1 AND workspace_id = $2`,
		} {
			if _, err := tx.Exec(ctx, statement, scope.OrgID, scope.WorkspaceID); err != nil {
				t.Errorf("clean scoped server integration rows: %T", err)
				return
			}
		}
	}
	if err := bindServerIntegrationScope(ctx, tx, fixture.deploymentID, fixture.scope, fixture.operator); err != nil {
		t.Errorf("bind server integration root cleanup: %T", err)
		return
	}
	if _, err := tx.Exec(ctx, `DELETE FROM pm.identities WHERE org_id = $1`, fixture.scope.OrgID); err != nil {
		t.Errorf("clean server integration identities: %T", err)
		return
	}
	if _, err := tx.Exec(ctx, `DELETE FROM pm.provider_accounts WHERE org_id = $1`, fixture.scope.OrgID); err != nil {
		t.Errorf("clean server integration provider account: %T", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		t.Errorf("commit server integration cleanup: %T", err)
	}
}

func (fixture *serverIntegrationFixture) closeApplication(t *testing.T) {
	t.Helper()
	if fixture.serveCancel != nil {
		fixture.serveCancel()
	}
	if fixture.application != nil {
		if err := fixture.application.Close(); err != nil {
			t.Errorf("close server integration application: %v", err)
		}
	}
	if fixture.serveResult != nil {
		select {
		case err := <-fixture.serveResult:
			if err != nil {
				t.Errorf("serve server integration application: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server integration Serve did not return after owned drain")
		}
	}
	clear(fixture.wrappingKey)
}

func (fixture *serverIntegrationFixture) waitForLiveApplication(t *testing.T) {
	t.Helper()
	client := fixture.newHTTPClient(t, false)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		response, err := client.Get(fixture.origin + "/health/live")
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("server integration application did not become live")
}

func (fixture *serverIntegrationFixture) newHTTPClient(t *testing.T, withCookies bool) *http.Client {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(fixture.oidc.caCertificate)
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		MaxIdleConns:    8,
		IdleConnTimeout: 15 * time.Second,
	}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	if withCookies {
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatalf("create server integration cookie jar: %T", err)
		}
		client.Jar = jar
	}
	t.Cleanup(transport.CloseIdleConnections)
	return client
}

type integrationHTTPResponse struct {
	status int
	header http.Header
	body   []byte
}

func (fixture *serverIntegrationFixture) request(
	t *testing.T,
	client *http.Client,
	method string,
	path string,
	body []byte,
	headers http.Header,
) integrationHTTPResponse {
	t.Helper()
	return integrationRequest(t, client, method, fixture.origin+path, body, headers)
}

func integrationRequest(
	t *testing.T,
	client *http.Client,
	method string,
	requestURL string,
	body []byte,
	headers http.Header,
) integrationHTTPResponse {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequest(method, requestURL, reader)
	if err != nil {
		t.Fatalf("construct server integration request: %T", err)
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("execute server integration request: %T", err)
	}
	defer response.Body.Close()
	for _, cookie := range response.Cookies() {
		if strings.HasPrefix(cookie.Name, "__Host-") && (!cookie.Secure || cookie.Domain != "" || cookie.Path != "/") {
			t.Fatal("response contains a host-prefixed cookie that a conforming browser rejects")
		}
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, integrationMaximumResponseBytes+1))
	if err != nil || len(responseBody) > integrationMaximumResponseBytes {
		t.Fatal("read bounded server integration response")
	}
	return integrationHTTPResponse{status: response.StatusCode, header: response.Header.Clone(), body: responseBody}
}

func (fixture *serverIntegrationFixture) requestJSON(
	t *testing.T,
	client *http.Client,
	method string,
	path string,
	body any,
	headers http.Header,
) integrationHTTPResponse {
	t.Helper()
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("encode server integration request: %T", err)
		}
		if headers == nil {
			headers = make(http.Header)
		} else {
			headers = headers.Clone()
		}
		headers.Set("Content-Type", "application/json")
	}
	return fixture.request(t, client, method, path, encoded, headers)
}

type integrationBrowserSession struct {
	client *http.Client
	csrf   string
}

func (fixture *serverIntegrationFixture) loginBrowser(
	t *testing.T,
	mode integrationAuthTimeMode,
	reauthenticate bool,
) integrationBrowserSession {
	t.Helper()
	client := fixture.newHTTPClient(t, true)
	challengeResponse := fixture.requestJSON(t, client, http.MethodPost, "/v1/sessions/browser/challenges", map[string]any{
		"reauthenticate": reauthenticate,
	}, http.Header{"Origin": []string{fixture.origin}})
	if challengeResponse.status != http.StatusCreated {
		t.Fatalf("start browser integration login status = %d, want 201", challengeResponse.status)
	}
	var challenge struct {
		AuthorizationURL string    `json:"authorization_url"`
		ExpiresAt        time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(challengeResponse.body, &challenge); err != nil || challenge.AuthorizationURL == "" || !challenge.ExpiresAt.After(time.Now()) {
		t.Fatal("browser integration challenge response is invalid")
	}
	callback := fixture.authorizeBrowser(t, client, challenge.AuthorizationURL, mode)
	completeResponse := fixture.requestJSON(t, client, http.MethodPost, "/v1/sessions/browser", map[string]any{
		"code":  callback.Get("code"),
		"state": callback.Get("state"),
	}, http.Header{"Origin": []string{fixture.origin}})
	if completeResponse.status != http.StatusNoContent {
		t.Fatalf("complete browser integration login status = %d, want 204", completeResponse.status)
	}
	originURL, err := url.Parse(fixture.origin)
	if err != nil {
		t.Fatal("parse server integration origin")
	}
	csrfToken := ""
	sessionCookie := ""
	for _, cookie := range client.Jar.Cookies(originURL) {
		switch cookie.Name {
		case "__Host-pm_session":
			sessionCookie = cookie.Value
		case "__Host-pm_csrf":
			csrfToken = cookie.Value
		}
	}
	if csrfToken == "" || sessionCookie == "" {
		t.Fatal("browser integration login did not install session and CSRF cookies")
	}
	return integrationBrowserSession{client: client, csrf: csrfToken}
}

func (fixture *serverIntegrationFixture) authorizeBrowser(
	t *testing.T,
	client *http.Client,
	authorizationURL string,
	mode integrationAuthTimeMode,
) url.Values {
	t.Helper()
	parsed, err := url.Parse(authorizationURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host != strings.TrimPrefix(fixture.oidc.issuer, "https://") || parsed.Path != "/authorize" {
		t.Fatal("browser integration authorization target is not the owned OIDC fixture")
	}
	withoutRedirect := *client
	withoutRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	pageResponse := integrationRequest(t, &withoutRedirect, http.MethodGet, authorizationURL, nil, nil)
	if pageResponse.status != http.StatusOK || !bytes.Contains(pageResponse.body, []byte("Continue as integration operator")) {
		t.Fatal("owned OIDC fixture did not serve its consent page")
	}
	form := parsed.Query()
	form.Set("decision", "allow")
	form.Set("auth_time_mode", string(mode))
	request, err := http.NewRequest(http.MethodPost, fixture.oidc.issuer+"/authorize", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal("construct owned OIDC consent request")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := withoutRedirect.Do(request)
	if err != nil {
		t.Fatalf("submit owned OIDC consent: %T", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("owned OIDC consent status = %d, want 303", response.StatusCode)
	}
	location, err := url.Parse(response.Header.Get("Location"))
	origin, originErr := url.Parse(fixture.origin)
	if err != nil || originErr != nil || location.Scheme != origin.Scheme || location.Host != origin.Host ||
		location.Path != browserCallbackPath || location.Query().Get("code") == "" || location.Query().Get("state") == "" {
		t.Fatal("owned OIDC fixture returned an invalid local callback")
	}
	return location.Query()
}

func (session integrationBrowserSession) mutationHeaders(origin string) http.Header {
	return http.Header{
		"Origin":       []string{origin},
		"X-CSRF-Token": []string{session.csrf},
	}
}

func (fixture *serverIntegrationFixture) makeDevicePollEligible(t *testing.T, challengeID string) {
	t.Helper()
	rows := fixture.execScoped(t, fixture.scope, `
		UPDATE pm.device_login_challenges
		SET next_poll_at = clock_timestamp() - interval '1 second'
		WHERE deployment_id = $1 AND challenge_id = $2
	`, fixture.deploymentID, challengeID)
	if rows != 1 {
		t.Fatal("device poll eligibility fault injection did not affect the owned challenge")
	}
}

func (fixture *serverIntegrationFixture) execScoped(
	t *testing.T,
	scope contract.Scope,
	statement string,
	arguments ...any,
) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, fixture.migratorDSN)
	if err != nil {
		t.Fatalf("connect server integration scoped mutation: %T", err)
	}
	defer connection.Close(ctx)
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin server integration scoped mutation: %T", err)
	}
	defer tx.Rollback(ctx)
	if err := bindServerIntegrationScope(ctx, tx, fixture.deploymentID, scope, fixture.operator); err != nil {
		t.Fatalf("bind server integration scoped mutation: %T", err)
	}
	tag, err := tx.Exec(ctx, statement, arguments...)
	if err != nil {
		t.Fatalf("execute server integration scoped mutation: %T", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit server integration scoped mutation: %T", err)
	}
	return tag.RowsAffected()
}

func (fixture *serverIntegrationFixture) credentialStorageCounts(t *testing.T, scope contract.Scope) (int64, int64, int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, fixture.migratorDSN)
	if err != nil {
		t.Fatalf("connect server integration credential observation: %T", err)
	}
	defer connection.Close(ctx)
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin server integration credential observation: %T", err)
	}
	defer tx.Rollback(ctx)
	if err := bindServerIntegrationScope(ctx, tx, fixture.deploymentID, scope, fixture.operator); err != nil {
		t.Fatalf("bind server integration credential observation: %T", err)
	}
	var encrypted, lifecycle, receipts int64
	if err := tx.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM pm.encrypted_secrets WHERE org_id = $1 AND workspace_id = $2),
			(SELECT count(*) FROM pm.credential_lifecycles WHERE org_id = $1 AND workspace_id = $2),
			(SELECT count(*) FROM pm.credential_mutation_receipts WHERE org_id = $1 AND workspace_id = $2)
	`, scope.OrgID, scope.WorkspaceID).Scan(&encrypted, &lifecycle, &receipts); err != nil {
		t.Fatalf("observe server integration credential storage: %T", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit server integration credential observation: %T", err)
	}
	return encrypted, lifecycle, receipts
}

func (fixture *serverIntegrationFixture) hideWrappingKey(t *testing.T) func() {
	t.Helper()
	hidden := fixture.wrappingPath + ".unavailable"
	if err := os.Rename(fixture.wrappingPath, hidden); err != nil {
		t.Fatalf("hide synthetic integration wrapping key: %T", err)
	}
	var once sync.Once
	restore := func() {
		once.Do(func() {
			if err := os.Rename(hidden, fixture.wrappingPath); err != nil {
				t.Errorf("restore synthetic integration wrapping key: %T", err)
			}
		})
	}
	t.Cleanup(restore)
	return restore
}

func (fixture *serverIntegrationFixture) pauseForBrowserProbe(t *testing.T) {
	t.Helper()
	readyPath := os.Getenv(integrationBrowserReadyFileEnv)
	stopPath := os.Getenv(integrationBrowserStopFileEnv)
	timeoutText := os.Getenv(integrationBrowserPauseEnv)
	if readyPath == "" && stopPath == "" && timeoutText == "" {
		return
	}
	if readyPath == "" || stopPath == "" || !filepath.IsAbs(readyPath) || !filepath.IsAbs(stopPath) {
		t.Fatalf("%s and %s must name distinct absolute files", integrationBrowserReadyFileEnv, integrationBrowserStopFileEnv)
	}
	readyPath = filepath.Clean(readyPath)
	stopPath = filepath.Clean(stopPath)
	if readyPath == stopPath {
		t.Fatalf("%s and %s must name distinct absolute files", integrationBrowserReadyFileEnv, integrationBrowserStopFileEnv)
	}
	for _, path := range []string{readyPath, stopPath} {
		info, err := os.Stat(filepath.Dir(path))
		if err != nil || !info.IsDir() {
			t.Fatal("browser probe signal parent must be an existing directory")
		}
	}
	if _, err := os.Lstat(readyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("browser probe ready file must not already exist: %T", err)
	}
	if _, err := os.Lstat(stopPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("browser probe stop file must not already exist: %T", err)
	}
	pause := 5 * time.Minute
	if timeoutText != "" {
		parsed, err := time.ParseDuration(timeoutText)
		if err != nil || parsed < time.Second || parsed > 10*time.Minute {
			t.Fatalf("%s must be between 1s and 10m", integrationBrowserPauseEnv)
		}
		pause = parsed
	}
	descriptor := map[string]string{
		"login_url":        fixture.origin + "/login",
		"device_login_url": fixture.origin + "/login/device",
		"credentials_url":  fixture.origin + "/admin/credentials",
	}
	for _, value := range descriptor {
		if !integrationLocalHTTPSURL(value) {
			t.Fatal("browser probe descriptor contains a nonlocal URL")
		}
	}
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		t.Fatal("encode browser probe descriptor")
	}
	if err := writePrivateReadyFile(readyPath, encoded); err != nil {
		t.Fatalf("publish private browser probe ready file: %T", err)
	}
	t.Cleanup(func() {
		_ = os.Remove(readyPath)
		_ = os.Remove(stopPath)
	})
	deadline := time.NewTimer(pause)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			info, err := os.Lstat(stopPath)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil || !info.Mode().IsRegular() {
				t.Fatal("browser probe stop signal must be a regular file")
			}
			if err := os.Remove(readyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("remove browser probe ready file: %T", err)
			}
			if err := os.Remove(stopPath); err != nil {
				t.Fatalf("consume browser probe stop file: %T", err)
			}
			return
		case <-deadline.C:
			t.Fatal("browser probe stop file was not supplied before the bounded deadline")
		}
	}
}

func writePrivateReadyFile(path string, contents []byte) error {
	directory := filepath.Dir(path)
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return errors.New("ready-file directory is unavailable")
	}
	temporary, err := os.CreateTemp(directory, ".pm-server-browser-probe-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Link(temporaryPath, path)
}

func integrationRandomHex(t *testing.T, byteCount int) string {
	t.Helper()
	value := make([]byte, byteCount)
	if _, err := rand.Read(value); err != nil {
		t.Fatalf("generate isolated server integration identity: %T", err)
	}
	return hex.EncodeToString(value)
}

// identityTrustRootDeclaration is the optional operator-owned OIDC issuer trust
// root declaration; its confined bundle member is ca.pem.
const identityTrustRootDeclaration = `,"ca_ref":"` + integrationSecretReference + `"`

// identityTrustRootDocument is one complete valid startup document whose
// identity block optionally declares that trust root. The %s slot takes the
// raw optional key/value pair, so an empty slot keeps the declaration absent.
const identityTrustRootDocument = `{
  "schema_version":"1","deployment_id":"identity-trust-root","public_origin":"https://pm.example.invalid",
  "postgres":{"ownership":"managed","database":"pm_app","endpoint_ref":"secret://fixture/server"},
  "temporal":{"ownership":"managed","endpoint":"temporal.example.invalid:7233","namespace":"fixture","client_identity_ref":"secret://fixture/temporal","retention":"720h","task_queues":["pm-control"]},
  "artifacts":{"ownership":"managed","driver":"s3","endpoint":"https://objects.example.invalid","bucket":"fixture-objects","credential_ref":"secret://fixture/objects","attempt_key_policy":"unique_immutable"},
  "identity":{"issuer":"https://identity.example.invalid","client_id":"pm-server","client_secret_ref":"secret://fixture/server"%s},
  "encryption":{"wrapping_key_ref":"secret://fixture/server"},
  "admission":{"profile":"development"},
  "limits":{"workspace_active_runs":4,"workflow_payload_bytes":65536,"workflow_fanout":32,"run_lifetime":"1h","event_buffer":128,"query_children":1,"query_child_memory_bytes":4294967296,"query_child_deadline":"5m","request_bytes":1048576,"metadata_deadline":"5s"}
}`

// identityTrustRootIdentity resolves the identity settings a real startup
// document produces, so the declaration reaches this composition exactly as
// operator configuration does.
func identityTrustRootIdentity(t *testing.T, declaration string) config.IdentityConfiguration {
	t.Helper()
	document, err := config.Load(strings.NewReader(fmt.Sprintf(identityTrustRootDocument, declaration)))
	if err != nil {
		t.Fatalf("load identity trust root document: %v", err)
	}
	configuration, err := config.Validate(document, config.Prelaunch{})
	if err != nil {
		t.Fatalf("validate identity trust root document: %v", err)
	}
	return configuration.Identity()
}

// openIdentityTrustRootSecrets materializes one confined operator bundle whose
// members are the supplied ones. The bundle reference is the fixture reference
// secret://fixture/server.
func openIdentityTrustRootSecrets(t *testing.T, members map[string][]byte) *credentials.OperatorSecrets {
	t.Helper()
	root := t.TempDir()
	bundle := filepath.Join(root, "fixture", "server")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatalf("create identity trust root bundle: %v", err)
	}
	for name, contents := range members {
		if err := os.WriteFile(filepath.Join(bundle, name), contents, 0o600); err != nil {
			t.Fatalf("write identity trust root bundle member: %v", err)
		}
	}
	secrets, err := credentials.OpenOperatorSecrets(root)
	if err != nil {
		t.Fatalf("open identity trust root secrets: %v", err)
	}
	t.Cleanup(func() { _ = secrets.Close() })
	return secrets
}

// startIdentityTrustRootPeer serves one HTTPS peer on loopback with the
// supplied certificate. No network or public authority is involved.
func startIdentityTrustRootPeer(t *testing.T, certificate tls.Certificate) *httptest.Server {
	t.Helper()
	peer := httptest.NewUnstartedServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))
	peer.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	peer.StartTLS()
	t.Cleanup(peer.Close)
	return peer
}

// identityTrustRootTrusted performs one bounded OIDC client request against the
// controlled loopback peer and reports whether its certificate verified.
func identityTrustRootTrusted(t *testing.T, client *http.Client, peer *httptest.Server) bool {
	t.Helper()
	response, err := client.Get(peer.URL)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("identity trust root peer status = %d; want %d", response.StatusCode, http.StatusNoContent)
	}
	return true
}

// TestIdentityIssuerTrustRootBoundsOIDCClientTrust proves a declared root
// changes nothing but the OIDC client root store: exactly the declared
// certificate authority is trusted, an undeclared one is refused, and a client
// without the declaration stays on the unchanged process system root store.
func TestIdentityIssuerTrustRootBoundsOIDCClientTrust(t *testing.T) {
	demoCertificate, demoKey := newIntegrationCertificateAuthority(t)
	foreignCertificate, foreignKey := newIntegrationCertificateAuthority(t)
	demoPeer := startIdentityTrustRootPeer(t, issueIntegrationServerCertificate(t, demoCertificate, demoKey))
	foreignPeer := startIdentityTrustRootPeer(t, issueIntegrationServerCertificate(t, foreignCertificate, foreignKey))
	secrets := openIdentityTrustRootSecrets(t, map[string][]byte{
		oidcTrustRootMember: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: demoCertificate.Raw}),
	})

	roots, err := identityIssuerTrustRoot(secrets, identityTrustRootIdentity(t, identityTrustRootDeclaration))
	if err != nil || roots == nil {
		t.Fatalf("declared identity trust root = (pool set %t, %v); want the declared demo pool", roots != nil, err)
	}
	client, transport := newOIDCHTTPClient(roots)
	t.Cleanup(transport.Close)
	configuration := transport.next.TLSClientConfig
	if configuration == nil || configuration.RootCAs != roots {
		t.Fatal("declared identity trust root was not applied as the OIDC client root pool")
	}
	if configuration.MinVersion != tls.VersionTLS12 || configuration.InsecureSkipVerify {
		t.Fatalf(
			"declared identity trust root client TLS = (MinVersion %d, InsecureSkipVerify %t); want TLS 1.2 with certificate and hostname verification",
			configuration.MinVersion,
			configuration.InsecureSkipVerify,
		)
	}
	if !identityTrustRootTrusted(t, client, demoPeer) {
		t.Fatal("declared identity trust root did not trust the demo certificate authority it declares")
	}
	if identityTrustRootTrusted(t, client, foreignPeer) {
		t.Fatal("declared identity trust root trusted a certificate authority it does not declare")
	}

	absentClient, absentTransport := newOIDCHTTPClient(nil)
	t.Cleanup(absentTransport.Close)
	absentConfiguration := absentTransport.next.TLSClientConfig
	if absentConfiguration == nil {
		t.Fatal("client without a declared identity trust root has no TLS configuration")
	}
	if absentConfiguration.RootCAs != nil ||
		absentConfiguration.MinVersion != tls.VersionTLS12 ||
		absentConfiguration.InsecureSkipVerify {
		t.Fatalf(
			"client without a declared identity trust root = (RootCAs set %t, MinVersion %d, InsecureSkipVerify %t); want the unchanged process system root store at TLS 1.2",
			absentConfiguration.RootCAs != nil,
			absentConfiguration.MinVersion,
			absentConfiguration.InsecureSkipVerify,
		)
	}
	if identityTrustRootTrusted(t, absentClient, demoPeer) {
		t.Fatal("OIDC client without a declared identity trust root trusted a private certificate authority")
	}
}

// TestIdentityIssuerTrustRootRequiresDeclaration is the negative control: with
// no declared reference the confined bundle is never read, so the same code
// path stays unreachable even when it holds an unusable ca.pem.
func TestIdentityIssuerTrustRootRequiresDeclaration(t *testing.T) {
	secrets := openIdentityTrustRootSecrets(t, map[string][]byte{
		oidcTrustRootMember: []byte("not a certificate"),
	})
	identity := identityTrustRootIdentity(t, "")
	if reference, declared := identity.CATrustRootRef(); declared || reference != "" {
		t.Fatalf("identity configuration without ca_ref = (%q, %t); want empty, false", reference, declared)
	}
	roots, err := identityIssuerTrustRoot(secrets, identity)
	if err != nil || roots != nil {
		t.Fatalf("identity trust root without a declared reference = (pool set %t, %v); want no pool and no read", roots != nil, err)
	}
	if roots, err := identityIssuerTrustRoot(nil, identity); err != nil || roots != nil {
		t.Fatalf("identity trust root without a secret root = (pool set %t, %v); want no pool and no error", roots != nil, err)
	}
}

// TestIdentityIssuerTrustRootFailsClosed covers every refusal the declared
// bounded read can produce. Each one must yield no pool and a constant error
// that carries no certificate material.
func TestIdentityIssuerTrustRootFailsClosed(t *testing.T) {
	authority, authorityKey := newIntegrationCertificateAuthority(t)
	leaf := issueIntegrationServerCertificate(t, authority, authorityKey)
	authorityPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: authority.Raw})
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Certificate[0]})
	for _, test := range []struct {
		name    string
		members map[string][]byte
		want    error
	}{
		{"missing member", nil, errIdentityTrustRootUnavailable},
		{"empty member", map[string][]byte{oidcTrustRootMember: {}}, errIdentityTrustRootUnavailable},
		{"oversized member", map[string][]byte{oidcTrustRootMember: bytes.Repeat([]byte("A"), (64<<10)+1)}, errIdentityTrustRootUnavailable},
		{"non-PEM body", map[string][]byte{oidcTrustRootMember: []byte("not a certificate")}, errIdentityTrustRootInvalid},
		{"truncated PEM body", map[string][]byte{oidcTrustRootMember: []byte("-----BEGIN CERTIFICATE-----\nAAAA\n")}, errIdentityTrustRootInvalid},
		{"non-certificate PEM entry", map[string][]byte{
			oidcTrustRootMember: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("synthetic")}),
		}, errIdentityTrustRootInvalid},
		{"leaf certificate entry", map[string][]byte{oidcTrustRootMember: leafPEM}, errIdentityTrustRootInvalid},
		{"leaf entry beside an authority", map[string][]byte{oidcTrustRootMember: append(authorityPEM, leafPEM...)}, errIdentityTrustRootInvalid},
		{"trailing bytes after an authority", map[string][]byte{
			oidcTrustRootMember: append(authorityPEM, []byte("trailing text")...),
		}, errIdentityTrustRootInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			secrets := openIdentityTrustRootSecrets(t, test.members)
			roots, err := identityIssuerTrustRoot(secrets, identityTrustRootIdentity(t, identityTrustRootDeclaration))
			if roots != nil || !errors.Is(err, test.want) {
				t.Fatalf("identity trust root = (pool set %t, %v); want no pool and %v", roots != nil, err, test.want)
			}
		})
	}
}
