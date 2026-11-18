package keycloak

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// These Go developer checks use a recording RoundTripper in place of the
// network. It answers each request with a fixed status. It is not an identity
// provider and serves no OpenID Connect endpoint. A signal from it is never a
// product event.

const (
	testOrigin   = "https://identity.test.invalid:8443"
	testIssuer   = testOrigin + "/realms/pm-test"
	testAdmin    = testOrigin + "/admin/realms/pm-test"
	testCallback = "https://pm.test.invalid:9443/v1/sessions/browser/callback"
	testClientID = "pm-test-admin"
	adminSecret  = "admin-secret-value-for-the-check"
	adminToken   = "admin-token-value-for-the-check"
)

type recorded struct {
	method, target string
	header         http.Header
	body           string
}

// recorder records each request and answers with the status that answer
// returns for it. It holds no state of a provider. When fail returns an
// error, the request gets that transport error and no answer. When bodyFail
// returns an error, the answer body ends with that read error.
type recorder struct {
	requests []recorded
	answer   func(method, target string) (int, http.Header, string)
	fail     func(request *http.Request, body string) error
	bodyFail func(method, target string) error
}

func (r *recorder) RoundTrip(request *http.Request) (*http.Response, error) {
	var body []byte
	if request.Body != nil {
		body, _ = io.ReadAll(request.Body)
	}
	r.requests = append(r.requests, recorded{request.Method, request.URL.String(), request.Header.Clone(), string(body)})
	if r.fail != nil {
		if err := r.fail(request, string(body)); err != nil {
			return nil, err
		}
	}
	status, header, text := r.answer(request.Method, request.URL.String())
	if header == nil {
		header = http.Header{}
	}
	var reader io.Reader = strings.NewReader(text)
	if r.bodyFail != nil {
		if err := r.bodyFail(request.Method, request.URL.String()); err != nil {
			reader = io.MultiReader(reader, failingReader{err})
		}
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(reader), Request: request}, nil
}

// failingReader returns its error on each read.
type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

// keycloakAnswers answers as the scoped admin client of the test realm gets
// answered: a token, 201 with a Location for a creation, 204 for a deletion
// and 403 for a realm change. overrides changes the status of one request.
func keycloakAnswers(overrides map[string]int) func(string, string) (int, http.Header, string) {
	return func(method, target string) (int, http.Header, string) {
		key := method + " " + target
		switch {
		case key == "POST "+testIssuer+"/protocol/openid-connect/token":
			return 200, nil, `{"access_token":"` + adminToken + `","token_type":"Bearer"}`
		case overrides[key] != 0:
			return overrides[key], nil, `{"error":"echo ` + adminSecret + `"}`
		case method == http.MethodPost:
			return 201, http.Header{"Location": {target + "/0b9a7e1c-0000-4000-8000-000000000001"}}, ""
		case method == http.MethodDelete:
			return 204, nil, ""
		case method == http.MethodPut:
			return 403, nil, `{"error":"unknown_error"}`
		}
		return 500, nil, ""
	}
}

func newTestAdmin(t *testing.T, r *recorder) *Admin {
	t.Helper()
	admin, err := New(Config{Issuer: testIssuer, ClientID: testClientID, ClientSecret: adminSecret, RequestLimit: 10 * time.Second, Transport: r})
	if err != nil {
		t.Fatal(err)
	}
	return admin
}

// Defect: a client without PKCE S256, with a redirect URI other than the
// exact edge callback, with a grant that the login does not use, or a secret
// or password outside an HTTPS request body (D1-6). The admin client secret
// goes only in the Basic client authentication of the token request (L-a).
func TestProvisionSendsOneClientAndOneUserInRequestBodies(t *testing.T) {
	r := &recorder{answer: keycloakAnswers(nil)}
	admin := newTestAdmin(t, r)
	identity, err := admin.Provision(testCallback)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.requests) != 3 {
		t.Fatalf("Provision sent %d requests, want the token, the client and the user", len(r.requests))
	}
	token, client, user := r.requests[0], r.requests[1], r.requests[2]
	if token.body != "grant_type=client_credentials" {
		t.Fatalf("the token request body is %d bytes, want only the client credentials grant", len(token.body))
	}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(testClientID+":"+adminSecret))
	if token.header.Get("Authorization") != basic || len(token.header.Values("Authorization")) != 1 {
		t.Fatal("the token request does not use the Basic client authentication of the admin client")
	}
	if client.method+" "+client.target != "POST "+testAdmin+"/clients" || user.method+" "+user.target != "POST "+testAdmin+"/users" {
		t.Fatalf("Provision sent %s %s and %s %s", client.method, client.target, user.method, user.target)
	}
	var clientBody struct {
		ClientID, Secret, ClientAuthenticatorType                                                                 string
		PublicClient, StandardFlowEnabled, ImplicitFlowEnabled, DirectAccessGrantsEnabled, ServiceAccountsEnabled bool
		RedirectURIs                                                                                              []string `json:"redirectUris"`
		WebOrigins                                                                                                []string
		Attributes                                                                                                map[string]string
	}
	if err := json.Unmarshal([]byte(client.body), &clientBody); err != nil {
		t.Fatal(err)
	}
	switch {
	case clientBody.ClientID != identity.ClientID || clientBody.Secret != identity.ClientSecret || clientBody.ClientAuthenticatorType != "client-secret":
		t.Errorf("the client is not the confidential deployment client: %s", identity)
	case clientBody.PublicClient || !clientBody.StandardFlowEnabled || clientBody.ImplicitFlowEnabled || clientBody.DirectAccessGrantsEnabled || clientBody.ServiceAccountsEnabled:
		t.Errorf("the client allows a flow other than the code flow: %+v", clientBody.Attributes)
	case len(clientBody.RedirectURIs) != 1 || clientBody.RedirectURIs[0] != testCallback || len(clientBody.WebOrigins) != 0:
		t.Errorf("the client redirect URIs are %q and web origins %q, want only the exact callback", clientBody.RedirectURIs, clientBody.WebOrigins)
	case clientBody.Attributes["pkce.code.challenge.method"] != "S256":
		t.Errorf("the client does not require PKCE S256: %q", clientBody.Attributes)
	}
	var userBody struct {
		Username    string
		Enabled     bool
		Credentials []struct {
			Type, Value string
			Temporary   bool
		}
	}
	if err := json.Unmarshal([]byte(user.body), &userBody); err != nil {
		t.Fatal(err)
	}
	if userBody.Username != identity.Username || !userBody.Enabled || len(userBody.Credentials) != 1 ||
		userBody.Credentials[0].Type != "password" || userBody.Credentials[0].Value != identity.Password || userBody.Credentials[0].Temporary {
		t.Errorf("the user is not the deployment user with its password: %s", identity)
	}
	for index, request := range r.requests {
		header := request.header.Clone()
		if index == 0 {
			// Only the token request carries the Basic credential.
			header.Del("Authorization")
		}
		if found := admin.Leaks([]byte(request.target+" "+fmt.Sprint(header)), identity); len(found) != 0 {
			t.Errorf("%q are in the URL or a header of %s %s", found, request.method, request.target)
		}
		if (index != 1 && strings.Contains(request.body, identity.ClientSecret)) || (index != 2 && strings.Contains(request.body, identity.Password)) ||
			strings.Contains(request.body, adminSecret) {
			t.Errorf("a secret is in the body of %s %s, which does not own it", request.method, request.target)
		}
		if request.method != http.MethodPost || (request.target != testIssuer+"/protocol/openid-connect/token" && request.header.Get("Authorization") != "Bearer "+adminToken) {
			t.Errorf("%s %s is not authorized with the admin token", request.method, request.target)
		}
	}
	for _, text := range []string{fmt.Sprint(identity), fmt.Sprintf("%+v %#v %v", *identity, identity, admin)} {
		if strings.Contains(text, identity.Password) || strings.Contains(text, identity.ClientSecret) || strings.Contains(text, adminSecret) {
			t.Errorf("the formatted identity or admin client holds a secret: %s", text)
		}
	}
}

// Defect: a cleanup that stops at the first failed deletion, hides a failed
// deletion, or prints a secret that the answer echoes (Part 5).
func TestRemoveReportsEachFailedDeletion(t *testing.T) {
	r := &recorder{answer: keycloakAnswers(nil)}
	admin := newTestAdmin(t, r)
	identity, err := admin.Provision(testCallback)
	if err != nil {
		t.Fatal(err)
	}
	r.requests = nil
	failed := identity.userURL
	r.answer = keycloakAnswers(map[string]int{"DELETE " + failed: 500})
	err = admin.Remove(identity)
	if err == nil || !strings.Contains(err.Error(), "remove the deployment user") || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("Remove() = %v, want the failed user deletion", err)
	}
	// The answer echoes the admin secret. No text of the answer may reach the
	// error, so neither the secret nor the rest of the answer is in it.
	if strings.Contains(err.Error(), adminSecret) || strings.Contains(err.Error(), "echo") {
		t.Fatalf("the cleanup error holds text of the answer: %v", err)
	}
	var deletions []string
	for _, request := range r.requests {
		if request.method == http.MethodDelete {
			deletions = append(deletions, request.target)
		}
	}
	if len(deletions) != 2 || deletions[0] != identity.userURL || deletions[1] != identity.clientURL {
		t.Fatalf("Remove sent the deletions %q, want the user and then the client", deletions)
	}
}

// Defect: the harness accepts an admin client that may change the master
// realm or the settings of its own realm (D1-4, D1-5).
func TestCheckScopeRequiresARefusalOfEachRealmChange(t *testing.T) {
	for _, target := range []string{"PUT " + testOrigin + "/admin/realms/master", "PUT " + testAdmin} {
		r := &recorder{answer: keycloakAnswers(map[string]int{target: 204})}
		if err := newTestAdmin(t, r).CheckScope(); err == nil || !strings.Contains(err.Error(), "was not refused") {
			t.Errorf("CheckScope() with %s accepted = %v, want the scope failure", target, err)
		}
	}
	r := &recorder{answer: keycloakAnswers(nil)}
	if err := newTestAdmin(t, r).CheckScope(); err != nil {
		t.Fatalf("CheckScope() with both changes refused = %v", err)
	}
}

// Defect: the leak check misses an encoded form, so a provider log with the
// base64url user password passes (D1-5, D1-6).
func TestLeaksFindsEachEncodedForm(t *testing.T) {
	admin := newTestAdmin(t, &recorder{answer: keycloakAnswers(nil)})
	// The password bytes encode differently in base64 and base64url, and
	// need escapes in a URL.
	identity := &Identity{ClientSecret: "client-secret-value", Password: "pass word+/\xfb\xff"}
	for _, data := range []string{
		"log: " + base64.RawURLEncoding.EncodeToString([]byte(identity.Password)),
		"log: " + url.QueryEscape(identity.Password),
		"env: " + fmt.Sprintf("%x", identity.Password),
	} {
		if found := admin.Leaks([]byte(data), identity); len(found) != 1 || found[0] != "deployment user password" {
			t.Errorf("Leaks(%q) = %q, want the deployment user password", data, found)
		}
	}
	for _, value := range []string{adminSecret, testClientID + ":" + adminSecret} {
		if found := admin.Leaks([]byte("record: "+base64.StdEncoding.EncodeToString([]byte(value))), nil); len(found) != 1 || found[0] != "admin client secret" {
			t.Errorf("Leaks() of the base64 form of %d bytes = %q, want the admin client secret", len(value), found)
		}
	}
	if found := admin.Leaks([]byte("a clean record"), identity); len(found) != 0 {
		t.Errorf("Leaks() of a clean record = %q", found)
	}
}

// Defect: the harness provisions through the master realm (D1-4).
func TestNewRefusesTheMasterRealm(t *testing.T) {
	for _, issuer := range []string{testOrigin + "/realms/master", "http://identity.test.invalid/realms/pm-test", testOrigin + "/realms/pm-test/extra"} {
		if _, err := New(Config{Issuer: issuer, ClientID: "a", ClientSecret: "b", RequestLimit: time.Second, Transport: &recorder{}}); err == nil {
			t.Errorf("New() accepted the issuer %q", issuer)
		}
	}
}

// Defect: a format verb prints a field of a secret-bearing value: Config,
// Admin or Identity, as a value, a pointer or an exported field (B1).
func TestNoFormatVerbPrintsASecret(t *testing.T) {
	r := &recorder{answer: keycloakAnswers(nil)}
	admin := newTestAdmin(t, r)
	identity, err := admin.Provision(testCallback)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Issuer: testIssuer, ClientID: testClientID, ClientSecret: adminSecret, RequestLimit: time.Second}
	nested := struct {
		C Config
		A Admin
		I Identity
	}{config, *admin, *identity}
	values := map[string]any{
		"Config": config, "*Config": &config, "Admin": *admin, "*Admin": admin,
		"Identity": *identity, "*Identity": identity, "a struct with exported fields": nested,
	}
	for name, value := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%d", "%x", "%X", "%q"} {
			text := fmt.Sprintf(verb, value)
			if found := admin.Leaks([]byte(text), identity); len(found) != 0 || strings.Contains(text, adminSecret) {
				t.Errorf("%s with %s holds %q: %s", verb, name, found, text)
			}
		}
	}
}

// Defect: the error of a transport or of an answer reader reaches the caller
// with its text, which can hold the Basic credential, the bearer token or a
// deployment secret (B2).
func TestRequestFailuresHoldNoSecret(t *testing.T) {
	// echo returns a transport error that quotes the credentials and the body
	// of the request, as a diagnostic of a transport can.
	echo := func(request *http.Request, body string) error {
		return fmt.Errorf("transport failure: %s %s", request.Header.Get("Authorization"), body)
	}
	check := func(t *testing.T, step string, admin *Admin, identity *Identity, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: no error, want the request failure", step)
		}
		if found := admin.Leaks([]byte(err.Error()), identity); len(found) != 0 || strings.Contains(err.Error(), adminToken) {
			t.Errorf("%s: the error holds %q or the admin token: %v", step, found, err)
		}
		if identity != nil && (strings.Contains(err.Error(), identity.ClientSecret) || strings.Contains(err.Error(), identity.Password)) {
			t.Errorf("%s: the error holds a deployment secret: %v", step, err)
		}
	}

	t.Run("the token request", func(t *testing.T) {
		r := &recorder{answer: keycloakAnswers(nil), fail: echo}
		admin := newTestAdmin(t, r)
		identity, err := admin.Provision(testCallback)
		check(t, "Provision", admin, identity, err)
	})
	t.Run("a creation", func(t *testing.T) {
		r := &recorder{answer: keycloakAnswers(nil)}
		admin := newTestAdmin(t, r)
		r.fail = func(request *http.Request, body string) error {
			if request.Method != http.MethodPost || request.URL.String() != testAdmin+"/clients" {
				return nil
			}
			return echo(request, body)
		}
		identity, err := admin.Provision(testCallback)
		check(t, "Provision", admin, identity, err)
	})
	t.Run("a deletion", func(t *testing.T) {
		r := &recorder{answer: keycloakAnswers(nil)}
		admin := newTestAdmin(t, r)
		identity, err := admin.Provision(testCallback)
		if err != nil {
			t.Fatal(err)
		}
		r.fail = func(request *http.Request, body string) error {
			if request.Method != http.MethodDelete {
				return nil
			}
			return echo(request, body)
		}
		check(t, "Remove", admin, identity, admin.Remove(identity))
	})
	t.Run("an answer body read", func(t *testing.T) {
		r := &recorder{answer: keycloakAnswers(nil)}
		admin := newTestAdmin(t, r)
		r.bodyFail = func(method, target string) error {
			return errors.New("read failure after " + adminToken + " and " + adminSecret)
		}
		identity, err := admin.Provision(testCallback)
		check(t, "Provision", admin, identity, err)
	})
}

// Defect: an error text copies an answer that echoes a secret in an encoding
// that the redaction does not know, for example as a JSON string (B3). The
// leak check misses that encoding too.
func TestAnswerTextStaysOutOfErrors(t *testing.T) {
	const quotedSecret = `admin"secret\value-for-the-check`
	escaped, err := json.Marshal(quotedSecret)
	if err != nil {
		t.Fatal(err)
	}
	r := &recorder{answer: func(method, target string) (int, http.Header, string) {
		return 401, nil, `{"error":"invalid_client","error_description":"echo ` + strings.Trim(string(escaped), `"`) + `"}`
	}}
	admin, err := New(Config{Issuer: testIssuer, ClientID: testClientID, ClientSecret: quotedSecret, RequestLimit: 10 * time.Second, Transport: r})
	if err != nil {
		t.Fatal(err)
	}
	_, err = admin.Provision(testCallback)
	if err == nil || strings.Contains(err.Error(), strings.Trim(string(escaped), `"`)) || strings.Contains(err.Error(), quotedSecret) ||
		strings.Contains(err.Error(), "echo") {
		t.Fatalf("Provision() = %v, want an error with no text of the answer", err)
	}
	if !strings.Contains(err.Error(), "status 401") || !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("Provision() = %v, want the status and the OAuth error code", err)
	}
	if found := admin.Leaks([]byte(`{"record":"`+strings.Trim(string(escaped), `"`)+`"}`), nil); len(found) != 1 {
		t.Fatalf("Leaks() of the JSON-escaped admin secret = %q, want the admin client secret", found)
	}
}

// Defect: Remove sends the bearer token of its admin client to the URLs of an
// identity from another admin client, or to a URL outside its own test realm
// (B4).
func TestRemoveRefusesAnIdentityOfAnotherAdmin(t *testing.T) {
	first := &recorder{answer: keycloakAnswers(nil)}
	identity, err := newTestAdmin(t, first).Provision(testCallback)
	if err != nil {
		t.Fatal(err)
	}
	other := &recorder{answer: keycloakAnswers(nil)}
	otherAdmin, err := New(Config{Issuer: "https://other.test.invalid:8443/realms/pm-test", ClientID: testClientID,
		ClientSecret: adminSecret, RequestLimit: 10 * time.Second, Transport: other})
	if err != nil {
		t.Fatal(err)
	}
	if err := otherAdmin.Remove(identity); err == nil || len(other.requests) != 0 {
		t.Fatalf("Remove() of another admin's identity = %v after %d requests, want a refusal before any request", err, len(other.requests))
	}

	same := &recorder{answer: keycloakAnswers(nil)}
	sameAdmin := newTestAdmin(t, same)
	moved := *identity
	moved.userURL = "https://other.test.invalid:8443/admin/realms/pm-test/users/0b9a7e1c-0000-4000-8000-000000000001"
	if err := sameAdmin.Remove(&moved); err == nil || len(same.requests) != 0 {
		t.Fatalf("Remove() of a user URL on another origin = %v after %d requests, want a refusal before any request", err, len(same.requests))
	}
}

// Defect: an answer body that could not be read after 201 with a Location
// loses the URL of the new client, so Remove deletes nothing and reports
// success (M1).
func TestRemoveDeletesAnObjectWhoseAnswerBodyFailed(t *testing.T) {
	r := &recorder{answer: keycloakAnswers(nil)}
	admin := newTestAdmin(t, r)
	r.bodyFail = func(method, target string) error {
		if method == http.MethodPost && target == testAdmin+"/clients" {
			return errors.New("the answer body broke")
		}
		return nil
	}
	identity, err := admin.Provision(testCallback)
	if err == nil {
		t.Fatal("Provision() = nil, want the read failure of the client creation")
	}
	r.bodyFail = nil
	r.requests = nil
	if err := admin.Remove(identity); err != nil {
		t.Fatalf("Remove() = %v", err)
	}
	var deletions []string
	for _, request := range r.requests {
		if request.method == http.MethodDelete {
			deletions = append(deletions, request.target)
		}
	}
	if len(deletions) != 1 || !strings.HasPrefix(deletions[0], testAdmin+"/clients/") {
		t.Fatalf("Remove sent the deletions %q, want the created client", deletions)
	}
}

// Defect: a creation request with no answer leaves no trace, so Remove
// reports success although the user can exist (M1).
func TestRemoveReportsACreationWithNoKnownOutcome(t *testing.T) {
	r := &recorder{answer: keycloakAnswers(nil)}
	admin := newTestAdmin(t, r)
	r.fail = func(request *http.Request, _ string) error {
		if request.Method == http.MethodPost && request.URL.String() == testAdmin+"/users" {
			return errors.New("the connection broke after the request")
		}
		return nil
	}
	identity, err := admin.Provision(testCallback)
	if err == nil {
		t.Fatal("Provision() = nil, want the failure of the user creation")
	}
	r.fail = nil
	err = admin.Remove(identity)
	if err == nil || !strings.Contains(err.Error(), "user can exist") {
		t.Fatalf("Remove() = %v, want the user creation with no known outcome", err)
	}
	creations := 0
	for _, request := range r.requests {
		if request.method == http.MethodPost && request.target == testAdmin+"/users" {
			creations++
		}
	}
	if creations != 1 {
		t.Fatalf("the user creation was sent %d times, want once", creations)
	}
}
