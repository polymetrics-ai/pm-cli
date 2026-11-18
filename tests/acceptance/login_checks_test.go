package acceptance

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
)

// The Go developer checks of the W1a login helpers. They run in process,
// with no stack. They are never acceptance proof.

// recordedResponse is the next transport of the recorder in these checks. It
// answers each request with 204 and sends nothing.
type recordedResponse struct{ sent []string }

func (r *recordedResponse) RoundTrip(request *http.Request) (*http.Response, error) {
	r.sent = append(r.sent, request.URL.Host)
	return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
}

// browserWithJar is a client like the browser session: one jar, the
// recorder as its transport, no redirect.
func browserWithJar(t *testing.T, issuer string) (*http.Client, *providerRecorder, http.CookieJar) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	recorder, err := newProviderRecorder(issuer, &recordedResponse{})
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: recorder, Jar: jar}, recorder, jar
}

func setPMCookies(t *testing.T, jar http.CookieJar, origin string) {
	t.Helper()
	target, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(target, []*http.Cookie{
		{Name: "__Host-pm_session", Value: "session-secret-value", Path: "/", Secure: true},
		{Name: "__Host-pm_csrf", Value: "csrf-secret-value", Path: "/", Secure: true},
	})
}

func get(t *testing.T, client *http.Client, target string) {
	t.Helper()
	response, err := client.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
}

// TestProviderRecorderObservesEveryProviderBoundRequest checks the D1-12
// recorder: it counts each request to the Keycloak origin after the jar added
// its cookies, it ignores PM-bound requests, it names a PM cookie and never
// its value, and it fails when it observed nothing.
func TestProviderRecorderObservesEveryProviderBoundRequest(t *testing.T) {
	const (
		pmOrigin = "https://pm.example.test:8443"
		issuer   = "https://idp.example.test:9443/realms/pm-test"
	)
	t.Run("distinct cookie hosts send no PM cookie to the provider", func(t *testing.T) {
		client, recorder, jar := browserWithJar(t, issuer)
		setPMCookies(t, jar, pmOrigin)
		get(t, client, pmOrigin+"/v1/sessions/me")
		get(t, client, issuer+"/protocol/openid-connect/auth")
		get(t, client, issuer+"/login-actions/authenticate")
		if err := recorder.check(); err != nil {
			t.Fatalf("check = %v, want no error", err)
		}
		if recorder.requests != 2 {
			t.Fatalf("recorded %d provider-bound requests, want 2", recorder.requests)
		}
	})
	t.Run("a shared cookie host sends the PM cookies to the provider", func(t *testing.T) {
		// Cookies ignore the port. With one host for PM and Keycloak, the
		// jar sends the PM cookies to Keycloak (D1-3, D1-12).
		client, recorder, jar := browserWithJar(t, "https://pm.example.test:9443/realms/pm-test")
		setPMCookies(t, jar, pmOrigin)
		get(t, client, "https://pm.example.test:9443/realms/pm-test/protocol/openid-connect/auth")
		err := recorder.check()
		if err == nil {
			t.Fatal("check passed, want a failure for the PM cookies on a provider-bound request")
		}
		for _, name := range []string{"__Host-pm_session", "__Host-pm_csrf"} {
			if !strings.Contains(err.Error(), name) {
				t.Fatalf("check error %q does not name %s", err, name)
			}
		}
		for _, value := range []string{"session-secret-value", "csrf-secret-value"} {
			if strings.Contains(err.Error(), value) {
				t.Fatalf("check error shows a cookie value: %q", err)
			}
		}
	})
	t.Run("no provider-bound request observes nothing and fails", func(t *testing.T) {
		client, recorder, jar := browserWithJar(t, issuer)
		setPMCookies(t, jar, pmOrigin)
		get(t, client, pmOrigin+"/v1/sessions/browser/challenges")
		if err := recorder.check(); err == nil || !strings.Contains(err.Error(), "observed nothing") {
			t.Fatalf("check = %v, want the failure for no provider-bound request", err)
		}
	})
	t.Run("the issuer must be an absolute URL", func(t *testing.T) {
		if _, err := newProviderRecorder("/realms/pm-test", &recordedResponse{}); err == nil {
			t.Fatal("newProviderRecorder accepted an issuer with no scheme and host")
		}
	})
}

// TestRunDeviceExchangeRedactsTheProof checks that a failed login-exchange
// step shows no device proof and no user code, also when the process writes
// them to its output (D4-11, LOGIN proposal v3 Part 5, "Proof redaction on a
// failed step"). The process is /bin/sh, which reads the proof from stdin.
func TestRunDeviceExchangeRedactsTheProof(t *testing.T) {
	challenge := &deviceChallenge{
		challengeID: "device-challenge-1",
		userCode:    "PM-SECRETUSERCODE",
		proof:       "pm_device_SECRETPROOFVALUE0123456789",
	}
	environment := []string{"PATH=/usr/bin:/bin", "LANG=C"}
	t.Run("a failed exchange that prints the proof and the user code", func(t *testing.T) {
		script := `read proof; printf '%s\n' "$proof" >&2; printf '%s PM-SECRETUSERCODE\n' "$proof"; exit 3`
		err := runDeviceExchange("/bin/sh", []string{"-c", script}, environment, challenge)
		if err == nil {
			t.Fatal("runDeviceExchange passed, want exit code 3 to fail")
		}
		if !strings.Contains(err.Error(), "exited with code 3") {
			t.Fatalf("error %q does not name the exit code", err)
		}
		for _, secret := range []string{challenge.proof, challenge.userCode, "SECRETPROOFVALUE"} {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error shows a secret: %q", err)
			}
		}
	})
	t.Run("an exchange that exits with code 0 passes", func(t *testing.T) {
		if err := runDeviceExchange("/bin/sh", []string{"-c", "read proof; exit 0"}, environment, challenge); err != nil {
			t.Fatalf("runDeviceExchange = %v, want no error", err)
		}
	})
	t.Run("the step adds the challenge ID itself", func(t *testing.T) {
		script := `read proof; [ "$0" = "--challenge-id" ] && [ "$1" = "device-challenge-1" ] && [ "$proof" = "pm_device_SECRETPROOFVALUE0123456789" ]`
		if err := runDeviceExchange("/bin/sh", []string{"-c", script}, environment, challenge); err != nil {
			t.Fatalf("the exchange did not get --challenge-id and the proof on stdin: %v", err)
		}
	})
}

// TestCheckOnlyWorkspace checks the K1b oracle on the two output forms of
// pm workspaces list.
func TestCheckOnlyWorkspace(t *testing.T) {
	const (
		org       = "acceptance-org-1"
		workspace = "acceptance-workspace-1"
	)
	one := `{"membership_id":"m1","membership_revision":"1","name":"W","org_id":"acceptance-org-1","role":"admin","workspace_id":"acceptance-workspace-1"}`
	other := `{"membership_id":"m2","membership_revision":"1","name":"X","org_id":"acceptance-org-1","role":"admin","workspace_id":"acceptance-workspace-2"}`
	for _, test := range []struct {
		name   string
		stdout string
		pass   bool
	}{
		{name: "text output with only the workspace", stdout: "items\t[" + one + "]\nnext_cursor\tnull\n", pass: true},
		{name: "JSON output with only the workspace", stdout: "{\n  \"items\": [" + one + "],\n  \"next_cursor\": null\n}\n", pass: true},
		{name: "another workspace", stdout: "items\t[" + other + "]\nnext_cursor\tnull\n"},
		{name: "a second workspace", stdout: "items\t[" + one + "," + other + "]\nnext_cursor\tnull\n"},
		{name: "no workspace", stdout: "items\t[]\nnext_cursor\tnull\n"},
		{name: "a next page", stdout: "items\t[" + one + "]\nnext_cursor\t\"cursor-1\"\n"},
		{name: "another organization", stdout: "items\t[" + strings.Replace(one, "acceptance-org-1", "acceptance-org-2", 1) + "]\nnext_cursor\tnull\n"},
		{name: "no next_cursor field", stdout: "items\t[" + one + "]\n"},
		{name: "JSON output with no next_cursor", stdout: "{\"items\": [" + one + "]}\n"},
		{name: "an extra line", stdout: "items\t[" + one + "]\nnext_cursor\tnull\nok\n"},
		{name: "the items field twice", stdout: "items\t[" + one + "]\nitems\t[" + one + "]\nnext_cursor\tnull\n"},
		{name: "empty stdout", stdout: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := checkOnlyWorkspace(test.stdout, org, workspace)
			if test.pass && err != nil {
				t.Fatalf("checkOnlyWorkspace = %v, want a pass", err)
			}
			if !test.pass && err == nil {
				t.Fatal("checkOnlyWorkspace passed, want a failure")
			}
		})
	}
}
