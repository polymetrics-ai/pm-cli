package keycloak

import (
	"encoding/base64"
	"encoding/json"
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
// returns for it. It holds no state of a provider.
type recorder struct {
	requests []recorded
	answer   func(method, target string) (int, http.Header, string)
}

func (r *recorder) RoundTrip(request *http.Request) (*http.Response, error) {
	var body []byte
	if request.Body != nil {
		body, _ = io.ReadAll(request.Body)
	}
	r.requests = append(r.requests, recorded{request.Method, request.URL.String(), request.Header.Clone(), string(body)})
	status, header, text := r.answer(request.Method, request.URL.String())
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(text)), Request: request}, nil
}

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

// Defect: a subject that is the client ID, a part of the Location other than
// the user ID, or a subject that needs another admin request.
func TestSubjectIsTheIDOfTheCreatedUser(t *testing.T) {
	const clientID, userID = "6f0d2c1a-0000-4000-8000-00000000c11e", "3c9e5b7d-0000-4000-8000-0000000005e7"
	answers := keycloakAnswers(nil)
	r := &recorder{answer: func(method, target string) (int, http.Header, string) {
		switch method + " " + target {
		case "POST " + testAdmin + "/clients":
			return 201, http.Header{"Location": {target + "/" + clientID}}, ""
		case "POST " + testAdmin + "/users":
			return 201, http.Header{"Location": {target + "/" + userID}}, ""
		}
		return answers(method, target)
	}}
	admin := newTestAdmin(t, r)
	var unprovisioned *Identity
	if subject := unprovisioned.Subject(); subject != "" {
		t.Fatalf("the subject of no identity is %q, want empty", subject)
	}
	if subject := (&Identity{}).Subject(); subject != "" {
		t.Fatalf("the subject of an identity with no user is %q, want empty", subject)
	}
	identity, err := admin.Provision(testCallback)
	if err != nil {
		t.Fatal(err)
	}
	sent := len(r.requests)
	if subject := identity.Subject(); subject != userID {
		t.Fatalf("Subject() = %q, want the user ID %q", subject, userID)
	}
	if len(r.requests) != sent {
		t.Fatalf("Subject sent %d requests, want none", len(r.requests)-sent)
	}
	if text := fmt.Sprint(identity); strings.Contains(text, identity.Password) || strings.Contains(text, identity.ClientSecret) {
		t.Fatalf("the identity prints a secret: %q", text)
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
	if strings.Contains(err.Error(), adminSecret) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("the cleanup error does not redact the echoed admin secret: %v", err)
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
