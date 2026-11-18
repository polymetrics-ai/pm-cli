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
// The deployment secrets travel only in HTTPS request bodies. No error text
// holds an answer body or the text of a transport error. Each secret is a
// Secret: fmt prints a fixed text for it, or only an address when fmt cannot
// call its methods, and encoding/json sees no field of it. Each request waits
// once, with one limit, and nothing repeats.
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

// errNotSent marks a request that the admin client refused or could not build.
// No byte of it reached the network.
var errNotSent = errors.New("no request was sent")

// Config is the stack identity interface of slice L-a. It holds the admin
// client secret, so its Format prints no field.
type Config struct {
	// Issuer is the exact issuer of the test realm:
	// https://<address>/realms/<test realm>.
	Issuer string
	// CA is the PEM certificate that signed the Keycloak certificate.
	CA []byte
	// ClientID and ClientSecret are the scoped admin client of the test realm.
	ClientID     string
	ClientSecret Secret
	// RequestLimit bounds each request.
	RequestLimit time.Duration
	// Transport replaces the HTTPS transport. Only the Go developer tests set it.
	Transport http.RoundTripper
}

const (
	redactedConfig   = "Keycloak admin configuration [REDACTED]"
	redactedAdmin    = "Keycloak admin client [REDACTED]"
	redactedIdentity = "deployment identity [REDACTED]"
	redactedSecret   = "secret [REDACTED]"
)

// Secret holds one secret text. Its methods write the same fixed text for
// every verb. fmt does not call these methods for an invalid verb, such as %p
// or %w on a value, or for a field that is not exported. Then fmt prints the
// struct field by field. The only field is a pointer to a string, and fmt
// prints such a pointer only as an address, for every verb. (fmt follows a
// pointer to a struct, an array, a slice or a map, so the pointer must not
// point to one of them.) encoding/json sees no exported field. The zero Secret
// holds no text.
type Secret struct{ text *string }

// NewSecret returns a Secret that holds value.
func NewSecret(value string) Secret { return Secret{text: &value} }

// Reveal returns the secret text, or "" for the zero Secret. Use it only
// where the text leaves the harness: an HTTPS request, a secret file or a
// leak check.
func (s Secret) Reveal() string {
	if s.text == nil {
		return ""
	}
	return *s.text
}

func (Secret) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, redactedSecret) }
func (Secret) String() string                 { return redactedSecret }
func (Secret) GoString() string               { return redactedSecret }

// Format writes the same text for every verb and flag, for a value and a
// pointer, so that no verb prints a field.
func (Config) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, redactedConfig) }
func (Config) String() string                 { return redactedConfig }
func (Config) GoString() string               { return redactedConfig }

// Admin is the scoped admin client. It holds the admin client secret as a
// Secret.
type Admin struct {
	issuer   string
	origin   string
	realm    string
	clientID string
	secret   Secret
	limit    time.Duration
	client   *http.Client
}

func (Admin) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, redactedAdmin) }
func (Admin) String() string                 { return redactedAdmin }
func (Admin) GoString() string               { return redactedAdmin }

// Identity is the client and the user of one deployment. It holds secrets.
// issuer binds it to the admin client that created it. unresolved names each
// collection whose creation request has no known outcome: the object can
// exist although Provision has no URL for it.
type Identity struct {
	ClientID     string
	ClientSecret Secret
	Username     string
	Password     Secret
	issuer       string
	clientURL    string
	userURL      string
	unresolved   []string
}

func (Identity) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, redactedIdentity) }
func (Identity) String() string                 { return redactedIdentity }
func (Identity) GoString() string               { return redactedIdentity }

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
	if config.ClientID == "" || config.ClientSecret.Reveal() == "" || config.RequestLimit <= 0 {
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
			return fmt.Errorf("keycloak: the scoped admin client was not refused a change to %s: status %d, want 403%s", target.name, status, answerCode(text))
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
		issuer:       a.issuer,
		ClientID:     "pm-acceptance-" + randomHex(6),
		ClientSecret: NewSecret(randomSecret()),
		Username:     "pm-operator-" + randomHex(6),
		Password:     NewSecret(randomSecret()),
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
		"secret":                    identity.ClientSecret.Reveal(),
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
		"credentials":     []map[string]any{{"type": "password", "value": identity.Password.Reveal(), "temporary": false}},
	}
	identity.userURL, err = a.create(token, "users", user, identity)
	return identity, err
}

// Remove deletes the user and the client that Provision created. It sends
// each deletion once and reports each one that failed. It reports each
// creation with no known outcome, because that object can still exist. It
// refuses, before any request, an identity of another admin client or realm
// and a deletion URL outside the users and clients of its own test realm.
func (a *Admin) Remove(identity *Identity) error {
	if identity == nil || (identity.userURL == "" && identity.clientURL == "" && len(identity.unresolved) == 0) {
		return nil
	}
	if identity.issuer != a.issuer {
		return errors.New("keycloak: the deployment identity belongs to another admin client or realm: no request was sent")
	}
	items := []struct{ name, collection, target string }{
		{"user", "users", identity.userURL},
		{"client", "clients", identity.clientURL},
	}
	for _, item := range items {
		if item.target != "" && !a.ownsObject(item.target, item.collection) {
			return fmt.Errorf("keycloak: the deployment %s URL is not in the %s of the test realm: no request was sent", item.name, item.collection)
		}
	}
	var errs []error
	if identity.userURL != "" || identity.clientURL != "" {
		token, err := a.token()
		if err != nil {
			errs = append(errs, fmt.Errorf("keycloak: remove the deployment identity: %w", err))
		} else {
			for _, item := range items {
				if item.target == "" {
					continue
				}
				status, _, text, err := a.do(http.MethodDelete, item.target, token, nil)
				if err == nil && status != http.StatusNoContent {
					err = fmt.Errorf("status %d, want 204%s", status, answerCode(text))
				}
				if err != nil {
					errs = append(errs, fmt.Errorf("keycloak: remove the deployment %s: %w", item.name, err))
				}
			}
		}
	}
	for _, collection := range identity.unresolved {
		errs = append(errs, fmt.Errorf("keycloak: the deployment %s can exist: its creation request has no known outcome, and Remove has no URL for it",
			strings.TrimSuffix(collection, "s")))
	}
	return errors.Join(errs...)
}

// ownsObject reports whether target is one object of collection in the test
// realm of this admin client.
func (a *Admin) ownsObject(target, collection string) bool {
	id, ok := strings.CutPrefix(target, a.origin+"/admin/realms/"+a.realm+"/"+collection+"/")
	return ok && id != "" && !strings.ContainsAny(id, "/?#%")
}

// Leaks names each secret of the admin client and of the identity that data
// holds in a likely form (D1-5). The forms of the admin client secret include
// the forms of its Basic credential, client ID and secret joined by a colon.
// It is detection, not a boundary.
func (a *Admin) Leaks(data []byte, identity *Identity) []string {
	names := []string{"admin client secret"}
	values := [][]string{append(forms(a.secret.Reveal()), forms(a.basicCredential())...)}
	if identity != nil {
		names = append(names, "deployment client secret", "deployment user password")
		values = append(values, forms(identity.ClientSecret.Reveal()), forms(identity.Password.Reveal()))
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
	return url.QueryEscape(a.clientID) + ":" + url.QueryEscape(a.secret.Reveal())
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
		return "", errors.New("keycloak: build the token request")
	}
	request.SetBasicAuth(url.QueryEscape(a.clientID), url.QueryEscape(a.secret.Reveal()))
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
		return "", fmt.Errorf("keycloak: the token request got status %d, want 200 with a bearer token%s", status, answerCode(text))
	}
	return answer.AccessToken, nil
}

// create posts one representation to a collection of the test realm and
// returns the URL of the new object from the Location header. It returns a
// valid URL also with an error, for example when the answer body could not
// be read after 201: Remove can then delete the object. It records the
// collection in identity.unresolved when the outcome is not known: no answer,
// or 201 with no valid Location. It never sends the request again.
func (a *Admin) create(token, collection string, representation map[string]any, identity *Identity) (string, error) {
	body, err := json.Marshal(representation)
	if err != nil {
		return "", fmt.Errorf("keycloak: encode the deployment %s", collection)
	}
	defer clear(body)
	base := a.origin + "/admin/realms/" + a.realm + "/" + collection
	status, header, text, err := a.do(http.MethodPost, base, token, body)
	target := base + "/" + strings.TrimPrefix(header.Get("Location"), base+"/")
	located := strings.HasPrefix(header.Get("Location"), base+"/") && a.ownsObject(target, collection)
	switch {
	case status == http.StatusCreated && located:
		if err != nil {
			return target, fmt.Errorf("keycloak: create the deployment %s: %w", collection, err)
		}
		return target, nil
	case (status == 0 && !errors.Is(err, errNotSent)) || status == http.StatusCreated:
		identity.unresolved = append(identity.unresolved, collection)
		if err != nil {
			return "", fmt.Errorf("keycloak: create the deployment %s: %w", collection, err)
		}
		return "", fmt.Errorf("keycloak: the new deployment %s has no Location in the test realm", collection)
	case err != nil:
		return "", fmt.Errorf("keycloak: create the deployment %s: status %d: %w", collection, status, err)
	}
	return "", fmt.Errorf("keycloak: create the deployment %s: status %d, want 201%s", collection, status, answerCode(text))
}

// do sends one admin request with the bearer token and an optional JSON body.
// It refuses a target outside the origin of the test realm before it builds
// the request, so the token goes to no other origin.
func (a *Admin) do(method, target, token string, body []byte) (int, http.Header, []byte, error) {
	if parsed, err := url.Parse(target); err != nil || parsed.Scheme+"://"+parsed.Host != a.origin || parsed.User != nil {
		return 0, http.Header{}, nil, fmt.Errorf("keycloak: the request target is not on the origin of the test realm: %w", errNotSent)
	}
	ctx, cancel := context.WithTimeout(context.Background(), a.limit)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return 0, http.Header{}, nil, fmt.Errorf("keycloak: build the admin request: %w", errNotSent)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return a.send(request)
}

// send sends one request and reads at most answerLimit bytes of the answer.
// The error of the transport or of the answer reader can hold a credential,
// for example a request header or body. So send never returns its text. It
// keeps the status and the headers when only the body read failed.
func (a *Admin) send(request *http.Request) (int, http.Header, []byte, error) {
	answer, err := a.client.Do(request)
	if err != nil {
		return 0, http.Header{}, nil, requestFailure(request.Context(), "send the request")
	}
	defer answer.Body.Close()
	text, err := io.ReadAll(io.LimitReader(answer.Body, answerLimit))
	if err != nil {
		return answer.StatusCode, answer.Header, nil, requestFailure(request.Context(), "read the answer")
	}
	return answer.StatusCode, answer.Header, text, nil
}

// requestFailure is the safe error of a failed request step. A passed request
// limit keeps context.DeadlineExceeded as its cause. No other text of the
// failure goes into it.
func requestFailure(ctx context.Context, step string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("keycloak: %s: the request limit passed: %w", step, context.DeadlineExceeded)
	}
	return fmt.Errorf("keycloak: %s: the request failed", step)
}

// oauthErrorCodes are the error codes that an error text may name. They come
// from RFC 6749, section 5.2, and RFC 6750, section 3.1, and Keycloak's
// unknown_error. Each one is a fixed word, never a value of a request.
var oauthErrorCodes = map[string]bool{
	"invalid_request": true, "invalid_client": true, "invalid_grant": true,
	"unauthorized_client": true, "unsupported_grant_type": true, "invalid_scope": true,
	"invalid_token": true, "insufficient_scope": true, "access_denied": true, "unknown_error": true,
}

// answerCode names the OAuth error code of an answer for an error text, only
// when it is a known fixed code. It never copies other text of the answer: an
// answer can echo a secret in any encoding.
func answerCode(text []byte) string {
	var answer struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(text, &answer) != nil || !oauthErrorCodes[answer.Error] {
		return ""
	}
	return " (error code " + answer.Error + ")"
}

// forms returns the forms of a secret that the leak checks search: exact; the
// content of a JSON string, with and without the HTML escapes of Go;
// URL-encoded as a query value and as a path segment; hex in lower and upper
// case; base64 and base64url, each with and without padding.
func forms(secret string) []string {
	if secret == "" {
		return nil
	}
	value := []byte(secret)
	return []string{
		secret,
		jsonContent(secret, true),
		jsonContent(secret, false),
		url.QueryEscape(secret),
		url.PathEscape(secret),
		hex.EncodeToString(value),
		strings.ToUpper(hex.EncodeToString(value)),
		base64.StdEncoding.EncodeToString(value),
		base64.RawStdEncoding.EncodeToString(value),
		base64.URLEncoding.EncodeToString(value),
		base64.RawURLEncoding.EncodeToString(value),
	}
}

// jsonContent returns secret as the content of a JSON string, with no quotes.
func jsonContent(secret string, escapeHTML bool) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(escapeHTML)
	if encoder.Encode(secret) != nil {
		return secret
	}
	text := strings.TrimSuffix(buffer.String(), "\n")
	return strings.TrimSuffix(strings.TrimPrefix(text, `"`), `"`)
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
