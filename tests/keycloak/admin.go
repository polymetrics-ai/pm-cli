// Package keycloak provisions the identity of each acceptance deployment in
// the job-owned Keycloak (slice L-a). It uses only the scoped admin client of
// the test realm: a service-account client with the realm-management roles
// that create and remove one client and one user (D1-4). It is test-harness
// code, not PM behaviour, and it is not an identity provider.
//
// Each deployment gets one confidential client with PKCE S256 required and
// the exact edge callback as its only redirect URI, and one user with a random
// password (D1-6). The secrets stay in memory. The admin client secret travels
// only in the Basic client authentication header of the HTTPS token request.
// The deployment secrets travel only in HTTPS request bodies. Every error text
// redacts them. Each request waits once, with one limit, and nothing repeats.
package keycloak

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const answerLimit = 64 << 10

// Config is the stack identity interface of slice L-a.
type Config struct {
	// Issuer is the exact issuer of the test realm:
	// https://<address>/realms/<test realm>.
	Issuer string
	// CA is the PEM certificate that signed the Keycloak certificate.
	CA []byte
	// ClientID and ClientSecret are the scoped admin client of the test realm.
	ClientID     string
	ClientSecret string
	// RequestLimit bounds each request.
	RequestLimit time.Duration
	// Transport replaces the HTTPS transport. Only the Go developer tests set it.
	Transport http.RoundTripper
}

// Admin is the scoped admin client. It holds the admin client secret.
type Admin struct {
	issuer   string
	origin   string
	realm    string
	clientID string
	secret   string
	limit    time.Duration
	client   *http.Client
}

func (*Admin) String() string   { return "Keycloak admin client [REDACTED]" }
func (*Admin) GoString() string { return "Keycloak admin client [REDACTED]" }

// Identity is the client and the user of one deployment. It holds secrets.
type Identity struct {
	ClientID     string
	ClientSecret string
	Username     string
	Password     string
	clientURL    string
	userURL      string
}

func (Identity) String() string   { return "deployment identity [REDACTED]" }
func (Identity) GoString() string { return "deployment identity [REDACTED]" }

// New checks the configuration. It sends no request.
func New(config Config) (*Admin, error) {
	issuer, err := url.Parse(config.Issuer)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" {
		return nil, errors.New("keycloak: the issuer must be an https URL with no user, query or fragment")
	}
	realm, ok := strings.CutPrefix(issuer.Path, "/realms/")
	if !ok || realm == "" || strings.Contains(realm, "/") || realm == "master" {
		return nil, errors.New("keycloak: the issuer must be a test realm, not master: https://<address>/realms/<realm>")
	}
	if config.ClientID == "" || config.ClientSecret == "" || config.RequestLimit <= 0 {
		return nil, errors.New("keycloak: the admin client and the request limit are required")
	}
	transport := config.Transport
	if transport == nil {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(config.CA) {
			return nil, errors.New("keycloak: the identity provider CA holds no certificate")
		}
		transport = &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			DisableKeepAlives: true,
		}
	}
	return &Admin{
		issuer:   config.Issuer,
		origin:   issuer.Scheme + "://" + issuer.Host,
		realm:    realm,
		clientID: config.ClientID,
		secret:   config.ClientSecret,
		limit:    config.RequestLimit,
		client: &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// Issuer is the exact issuer of the test realm.
func (a *Admin) Issuer() string { return a.issuer }

// CheckScope shows that the admin client is refused a change outside its
// roles: a change to the master realm and a change to the settings of its own
// test realm must each get 403 (D1-4, D1-5). Each change request sends an
// empty realm representation.
func (a *Admin) CheckScope() error {
	token, err := a.token()
	if err != nil {
		return err
	}
	for _, target := range []struct{ name, realm string }{{"the master realm", "master"}, {"the test realm settings", a.realm}} {
		status, _, text, err := a.do(http.MethodPut, a.origin+"/admin/realms/"+target.realm, token, []byte("{}"))
		if err != nil {
			return fmt.Errorf("keycloak: ask for a change to %s: %w", target.name, err)
		}
		if status != http.StatusForbidden {
			return fmt.Errorf("keycloak: the scoped admin client was not refused a change to %s: status %d, want 403: %s", target.name, status, a.redact(text, token))
		}
	}
	return nil
}

// Provision creates the client and the user of one deployment. The result
// holds what it created, also with an error, so that Remove can remove it.
func (a *Admin) Provision(callback string) (*Identity, error) {
	target, err := url.Parse(callback)
	if err != nil || target.Scheme != "https" || target.Host == "" || target.RawQuery != "" || target.Fragment != "" {
		return nil, errors.New("keycloak: the callback must be an https URL with no query or fragment")
	}
	identity := &Identity{
		ClientID:     "pm-acceptance-" + randomHex(6),
		ClientSecret: randomSecret(),
		Username:     "pm-operator-" + randomHex(6),
		Password:     randomSecret(),
	}
	token, err := a.token()
	if err != nil {
		return identity, err
	}
	client := map[string]any{
		"clientId":                  identity.ClientID,
		"enabled":                   true,
		"protocol":                  "openid-connect",
		"publicClient":              false,
		"bearerOnly":                false,
		"clientAuthenticatorType":   "client-secret",
		"secret":                    identity.ClientSecret,
		"standardFlowEnabled":       true,
		"implicitFlowEnabled":       false,
		"directAccessGrantsEnabled": false,
		"serviceAccountsEnabled":    false,
		"consentRequired":           false,
		"fullScopeAllowed":          false,
		"redirectUris":              []string{callback},
		"webOrigins":                []string{},
		"attributes": map[string]string{
			"pkce.code.challenge.method":                "S256",
			"oauth2.device.authorization.grant.enabled": "false",
		},
	}
	if identity.clientURL, err = a.create(token, "clients", client, identity); err != nil {
		return identity, err
	}
	user := map[string]any{
		"username":        identity.Username,
		"enabled":         true,
		"email":           identity.Username + "@acceptance.invalid",
		"emailVerified":   true,
		"firstName":       "PM",
		"lastName":        "Operator",
		"requiredActions": []string{},
		"credentials":     []map[string]any{{"type": "password", "value": identity.Password, "temporary": false}},
	}
	identity.userURL, err = a.create(token, "users", user, identity)
	return identity, err
}

// Remove deletes the user and the client that Provision created. It sends
// each deletion once and reports each one that failed.
func (a *Admin) Remove(identity *Identity) error {
	if identity == nil || (identity.userURL == "" && identity.clientURL == "") {
		return nil
	}
	token, err := a.token()
	if err != nil {
		return fmt.Errorf("keycloak: remove the deployment identity: %w", err)
	}
	var errs []error
	for _, item := range []struct{ name, target string }{{"user", identity.userURL}, {"client", identity.clientURL}} {
		if item.target == "" {
			continue
		}
		status, _, text, err := a.do(http.MethodDelete, item.target, token, nil)
		if err == nil && status != http.StatusNoContent {
			err = fmt.Errorf("status %d, want 204: %s", status, a.redact(text, token, identity.ClientSecret, identity.Password))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("keycloak: remove the deployment %s: %w", item.name, err))
		}
	}
	return errors.Join(errs...)
}

// Leaks names each secret of the admin client and of the identity that data
// holds in a likely form (D1-5). The forms of the admin client secret include
// the forms of its Basic credential, client ID and secret joined by a colon.
// It is detection, not a boundary.
func (a *Admin) Leaks(data []byte, identity *Identity) []string {
	names := []string{"admin client secret"}
	values := [][]string{append(forms(a.secret), forms(a.basicCredential())...)}
	if identity != nil {
		names = append(names, "deployment client secret", "deployment user password")
		values = append(values, forms(identity.ClientSecret), forms(identity.Password))
	}
	var found []string
	for index, value := range values {
		for _, form := range value {
			if bytes.Contains(data, []byte(form)) {
				found = append(found, names[index])
				break
			}
		}
	}
	return found
}

// basicCredential is the credential of the Basic client authentication: the
// form-encoded client ID and secret, joined by a colon (RFC 6749, section
// 2.3.1).
func (a *Admin) basicCredential() string {
	return url.QueryEscape(a.clientID) + ":" + url.QueryEscape(a.secret)
}

// token gets an access token for the admin client with the client
// credentials grant and Basic client authentication. The admin client secret
// goes only in the Authorization header of this request. The body holds only
// the grant type.
func (a *Admin) token() (string, error) {
	form := url.Values{"grant_type": {"client_credentials"}}
	ctx, cancel := context.WithTimeout(context.Background(), a.limit)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.issuer+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("keycloak: build the token request: %w", err)
	}
	request.SetBasicAuth(url.QueryEscape(a.clientID), url.QueryEscape(a.secret))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	status, _, text, err := a.send(request)
	if err != nil {
		return "", fmt.Errorf("keycloak: ask for an admin token: %w", err)
	}
	var answer struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if status != http.StatusOK || json.Unmarshal(text, &answer) != nil || answer.AccessToken == "" || !strings.EqualFold(answer.TokenType, "Bearer") {
		return "", fmt.Errorf("keycloak: the token request got status %d, want 200 with a bearer token: %s", status, a.redact(text, answer.AccessToken))
	}
	return answer.AccessToken, nil
}

// create posts one representation to a collection of the test realm and
// returns the URL of the new object from the Location header.
func (a *Admin) create(token, collection string, representation map[string]any, identity *Identity) (string, error) {
	body, err := json.Marshal(representation)
	if err != nil {
		return "", fmt.Errorf("keycloak: encode the deployment %s: %w", collection, err)
	}
	defer clear(body)
	base := a.origin + "/admin/realms/" + a.realm + "/" + collection
	status, header, text, err := a.do(http.MethodPost, base, token, body)
	id, ok := strings.CutPrefix(header.Get("Location"), base+"/")
	switch {
	case err != nil:
		return "", fmt.Errorf("keycloak: create the deployment %s: %w", collection, err)
	case status != http.StatusCreated:
		return "", fmt.Errorf("keycloak: create the deployment %s: status %d, want 201: %s", collection, status,
			a.redact(text, token, identity.ClientSecret, identity.Password))
	case !ok || id == "" || strings.ContainsAny(id, "/?#"):
		return "", fmt.Errorf("keycloak: the new deployment %s has no Location in the test realm", collection)
	}
	return base + "/" + id, nil
}

// do sends one admin request with the bearer token and an optional JSON body.
func (a *Admin) do(method, target, token string, body []byte) (int, http.Header, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), a.limit)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return a.send(request)
}

// send sends one request and reads at most answerLimit bytes of the answer.
func (a *Admin) send(request *http.Request) (int, http.Header, []byte, error) {
	answer, err := a.client.Do(request)
	if err != nil {
		return 0, nil, nil, err
	}
	defer answer.Body.Close()
	text, err := io.ReadAll(io.LimitReader(answer.Body, answerLimit))
	if err != nil {
		return 0, nil, nil, err
	}
	return answer.StatusCode, answer.Header, text, nil
}

// redact returns at most 512 bytes of an answer for an error text, with every
// form of the admin client secret, of its Basic credential and of the given
// secrets replaced.
func (a *Admin) redact(text []byte, secrets ...string) string {
	value := string(text)
	for _, secret := range append(secrets, a.basicCredential(), a.secret) {
		for _, form := range forms(secret) {
			value = strings.ReplaceAll(value, form, "[REDACTED]")
		}
	}
	if len(value) > 512 {
		value = value[:512] + "..."
	}
	return fmt.Sprintf("%q", value)
}

// forms returns the forms of a secret that the leak checks search: exact,
// base64, base64url with and without padding, URL-encoded and hex.
func forms(secret string) []string {
	if secret == "" {
		return nil
	}
	value := []byte(secret)
	return []string{
		secret,
		base64.StdEncoding.EncodeToString(value),
		base64.RawStdEncoding.EncodeToString(value),
		base64.URLEncoding.EncodeToString(value),
		base64.RawURLEncoding.EncodeToString(value),
		url.QueryEscape(secret),
		hex.EncodeToString(value),
	}
}

func randomSecret() string {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		panic(fmt.Sprintf("read random bytes: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(value)
}

func randomHex(size int) string {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		panic(fmt.Sprintf("read random bytes: %v", err))
	}
	return hex.EncodeToString(value)
}
