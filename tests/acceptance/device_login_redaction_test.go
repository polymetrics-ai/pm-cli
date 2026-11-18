package acceptance

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These are Go developer checks of the device login step helpers (L-d RED,
// coverage map row 7; D4-10, D4-11; review findings F2 and F3). They are not
// acceptance proof: a Gherkin scenario cannot fail a step on purpose. They
// start no process and need no stack. Hosts and addresses are reserved names
// (RFC 2606) and documentation addresses (RFC 5737, RFC 3849).

// The synthetic secrets have the forms of the product values: the proof is
// "pm_device_" and base64url, the user code is "PM-" and base64url
// (internal/auth/device.go:56-61). The session credential is a value that
// the steps never know.
const (
	syntheticProof      = "pm_device_Zx81_q7Lm2Vt9Rk4-Wn3Hy6Pb0aQ5sT8uV1wX3yZ6cD9e"
	syntheticUserCode   = "PM-Wq3Ze8Rt5Yu2Io9Pa1Sd4F"
	syntheticCredential = "pm_session_Kd7Fh2Lq9Wz4Xc1Vb6Nm3Pt8Rg5Sj0Ty"
	syntheticIncident   = "pm-ABCDEFGHIJKLMNOPQRSTUVWXYZ"
)

// secretFormsOf returns the forms of a secret that a message must not hold:
// exact, JSON string content, base64, base64url with and without padding,
// URL-encoded and hex.
func secretFormsOf(secret string) []string {
	value := []byte(secret)
	encoded, _ := json.Marshal(secret)
	return []string{
		secret,
		strings.Trim(string(encoded), `"`),
		base64.StdEncoding.EncodeToString(value),
		base64.RawStdEncoding.EncodeToString(value),
		base64.URLEncoding.EncodeToString(value),
		base64.RawURLEncoding.EncodeToString(value),
		url.QueryEscape(secret),
		url.PathEscape(secret),
		hex.EncodeToString(value),
	}
}

// lowercaseProofPrefix is a value with the proof prefix and the form of a
// problem code.
var lowercaseProofPrefix = strings.ToLower(syntheticProof[:20])

// leakyText holds each synthetic secret in several forms and in each place
// of a problem line.
func leakyText() string {
	lines := []string{
		"error: device_poll_too_soon: proof " + syntheticProof + " (incident " + syntheticIncident + ")",
		"error: device_poll_too_soon: the proof as the incident (incident " + syntheticProof + ")",
		"error: " + lowercaseProofPrefix + ": a code with the proof prefix (incident " + syntheticIncident + ")",
		"base64 " + base64.StdEncoding.EncodeToString([]byte(syntheticProof)),
		"base64url " + base64.RawURLEncoding.EncodeToString([]byte(syntheticProof)),
		"query " + url.QueryEscape("proof="+syntheticProof),
		"hex " + hex.EncodeToString([]byte(syntheticProof)),
		"user code " + syntheticUserCode,
		"credential " + syntheticCredential,
		"STDERR-MARKER-LINE",
	}
	return strings.Join(lines, "\n") + "\n"
}

func forbiddenSecretForms() []string {
	var forms []string
	for _, secret := range []string{syntheticProof, syntheticUserCode, syntheticCredential, lowercaseProofPrefix} {
		forms = append(forms, secretFormsOf(secret)...)
	}
	return append(forms, "STDOUT-MARKER", "STDERR-MARKER-LINE")
}

func checkNoForms(t *testing.T, message string, forbidden []string) {
	t.Helper()
	for _, form := range forbidden {
		if form != "" && strings.Contains(message, form) {
			t.Fatalf("the failure message holds the forbidden text %q:\n%s", form, message)
		}
	}
}

// endedProcess returns a login-exchange process that exited with the code
// and wrote stderr, with its first stderr line ready.
func endedProcess(code int, stderr string, first firstLine) *exchangeProcess {
	p := &exchangeProcess{
		first:       make(chan firstLine, 1),
		outputEnded: make(chan struct{}),
		exited:      make(chan struct{}),
		status:      processStatus{exited: true, code: code, text: "exit status"},
		exitLimit:   time.Minute,
	}
	p.stderr.write([]byte(stderr))
	p.first <- first
	close(p.outputEnded)
	close(p.exited)
	return p
}

func firstLineOf(text string) firstLine {
	line, _, found := strings.Cut(text, "\n")
	return firstLine{line: line, found: found}
}

// TestDeviceLoginFailureMessagesHoldNoSecret checks that each failure message
// of the new steps that shows process output holds no proof, no user code and
// no unknown session credential (D4-10, D4-11, review finding F2).
func TestDeviceLoginFailureMessagesHoldNoSecret(t *testing.T) {
	leaky := leakyText()
	scenario := func() *deviceLoginScenario {
		return &deviceLoginScenario{
			shared:    &stackScenario{},
			challenge: &deviceChallenge{challengeID: "device-1", userCode: syntheticUserCode, proof: syntheticProof},
		}
	}
	pendingNotice := "notice: device login exchange received device_authorization_pending. The CLI waits 5s before the next exchange request."
	cases := []struct {
		name string
		run  func(*deviceLoginScenario) error
		want string
	}{
		// F2: a failed login-start, before the step knows the proof.
		{"login-start: nonzero exit, empty stdout", func(*deviceLoginScenario) error {
			_, err := checkLoginStart(5, "", leaky)
			return err
		}, "a problem line with code device_poll_too_soon and incident " + syntheticIncident},
		{"login-start: nonzero exit, malformed stdout", func(*deviceLoginScenario) error {
			_, err := checkLoginStart(5, "STDOUT-MARKER not a table\n{", leaky)
			return err
		}, "stdout: 27 bytes, not shown"},
		{"login-start: nonzero exit, stdout without the proof", func(*deviceLoginScenario) error {
			_, err := checkLoginStart(5, "challenge_id\t\"STDOUT-MARKER\"\n", leaky)
			return err
		}, "a line of"},
		{"login-start: nonzero exit, stdout with the proof", func(*deviceLoginScenario) error {
			_, err := checkLoginStart(5, "device_proof\t\""+syntheticProof+"\"\nuser_code\t\""+syntheticUserCode+"\"\n", leaky)
			return err
		}, "exited with code 5"},
		{"login-start: exit 0, unreadable output", func(*deviceLoginScenario) error {
			_, err := checkLoginStart(0, "device_proof\t\""+syntheticProof+"\"\ndevice_proof\t\""+syntheticProof+"\"\n", "")
			return err
		}, "stdout (142 bytes, not shown)"},
		// The login-exchange process output.
		{"notice: the first line is not the notice line", func(s *deviceLoginScenario) error {
			s.exchange = endedProcess(5, leaky, firstLineOf(leaky))
			return s.noticeLine(t.Context(), "")
		}, "the process exited with code 5"},
		{"notice: end of file before a complete line", func(s *deviceLoginScenario) error {
			s.exchange = endedProcess(5, "partial "+syntheticProof, firstLine{line: "partial " + syntheticProof})
			return s.noticeLine(t.Context(), "")
		}, "(no line feed)"},
		{"notice: the notice names the other code", func(s *deviceLoginScenario) error {
			s.exchange = endedProcess(0, pendingNotice+"\n"+leaky, firstLineOf(pendingNotice+"\n"))
			return s.noticeLine(t.Context(), "device_poll_too_soon")
		}, "before the first permitted poll time of the challenge"},
		{"exit: wrong exit code", func(s *deviceLoginScenario) error {
			s.exchange = endedProcess(5, leaky, firstLineOf(leaky))
			return s.exitsWithCode(0)
		}, "want exit code 0"},
		{"stderr names: code not named", func(s *deviceLoginScenario) error {
			s.exchange = endedProcess(5, leaky, firstLineOf(leaky))
			return s.stderrNames("device_authorization_denied")
		}, "does not name device_authorization_denied"},
		{"SIGINT: the process ended before the signal", func(s *deviceLoginScenario) error {
			s.exchange = endedProcess(5, leaky, firstLineOf(leaky))
			return s.sendSIGINT()
		}, "ended before the step sent SIGINT"},
		{"stderr longer than its bound", func(s *deviceLoginScenario) error {
			s.exchange = endedProcess(5, strings.Repeat("x", deviceLoginOutputLimit-10)+syntheticProof, firstLine{})
			return s.exitsWithCode(0)
		}, "not shown"},
		// The stored session command and the shared exit code step.
		{"stored session: the shared exit code step", func(*deviceLoginScenario) error {
			result := storedSessionResult(3, "STDOUT-MARKER "+syntheticCredential+"\n", leaky, "pm-acceptance-1")
			return result.exitCodeIs(0)
		}, "exit code 3, want 0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.run(scenario())
			if err == nil {
				t.Fatal("the step helper returned no failure")
			}
			checkNoForms(t, err.Error(), forbiddenSecretForms())
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("the failure message does not hold %q:\n%s", c.want, err.Error())
			}
		})
	}
}

// syntheticPlaces are the synthetic hosts, addresses and port of the browser
// checks.
var syntheticPlaces = []string{"pm.test.invalid", "test.invalid", "192.0.2.10", "192.0.2.53", "2001:db8::1", "2001:db8", "9443"}

// fakeTransport answers each request with its error, or with its status and
// body. It is a developer-check double of the network, never acceptance.
type fakeTransport struct {
	err    error
	status int
	body   io.Reader
}

func (f fakeTransport) RoundTrip(*http.Request) (*http.Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{StatusCode: f.status, Header: http.Header{}, Body: io.NopCloser(f.body)}, nil
}

// failingReader fails each read with an error whose text holds an address.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, &net.OpError{Op: "read", Net: "tcp", Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 9443}, Err: syscall.ECONNRESET}
}

// TestDeviceLoginBrowserFailuresOmitHostsAndAddresses checks that each
// browser failure message names the request by its fixed operation name and
// the failure by a fixed category, with no host, address or port of the
// target or of the transport error (review finding F3).
func TestDeviceLoginBrowserFailuresOmitHostsAndAddresses(t *testing.T) {
	targets := []struct{ class, url string }{
		{"host name", "https://pm.test.invalid:9443/v1/sessions/browser/challenges"},
		{"IPv4 address", "https://192.0.2.10/realms/test/protocol/openid-connect/auth?client_id=pm.test.invalid"},
		{"IPv6 address", "https://[2001:db8::1]:9443/realms/test/login-actions/authenticate?session_code=192.0.2.10"},
	}
	ipv6 := &net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 9443}
	ipv4 := &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 9443}
	transports := []struct {
		name      string
		transport func() fakeTransport
		category  string
	}{
		{"timeout", func() fakeTransport {
			return fakeTransport{err: &net.OpError{Op: "dial", Net: "tcp", Addr: ipv4, Err: os.ErrDeadlineExceeded}}
		}, "timeout"},
		{"connection refused", func() fakeTransport {
			return fakeTransport{err: &net.OpError{Op: "dial", Net: "tcp", Addr: ipv6, Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}}}
		}, "connection refused"},
		{"name lookup", func() fakeTransport {
			return fakeTransport{err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "pm.test.invalid", Server: "192.0.2.53:53"}}}
		}, "name lookup failure"},
		{"TLS", func() fakeTransport {
			return fakeTransport{err: &tls.CertificateVerificationError{Err: errors.New("x509: certificate is valid for pm.test.invalid, not 192.0.2.10")}}
		}, "TLS failure"},
		{"other", func() fakeTransport {
			return fakeTransport{err: errors.New("proxyconnect tcp: dial tcp [2001:db8::1]:9443: unexpected answer from pm.test.invalid")}
		}, "other transport failure"},
		{"answer read failure", func() fakeTransport {
			return fakeTransport{status: 200, body: failingReader{}}
		}, "connection closed"},
		{"answer too large", func() fakeTransport {
			return fakeTransport{status: 200, body: bytes.NewReader(bytes.Repeat([]byte("pm.test.invalid "), laneFileLimit/16+1))}
		}, "answer too large"},
	}
	for _, target := range targets {
		for _, transport := range transports {
			t.Run(transport.name+", "+target.class, func(t *testing.T) {
				b := &browserSession{client: &http.Client{
					Transport:     transport.transport(),
					CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
				}}
				_, _, _, err := b.send(context.Background(), operationKeycloakForm, http.MethodGet, target.url, nil, nil)
				if err == nil {
					t.Fatal("send returned no failure")
				}
				checkNoForms(t, err.Error(), syntheticPlaces)
				for _, want := range []string{operationKeycloakForm, transport.category} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("the failure message does not hold %q:\n%s", want, err.Error())
					}
				}
			})
		}
	}

	answers := []struct {
		name    string
		message func() string
		want    string
	}{
		{"Keycloak page texts", func() string {
			page := `<html><head><title>Sign in to https://192.0.2.10:9443/realms/test</title></head>` +
				`<span id="input-error">Invalid username or password.</span>` +
				`<span class="kc-feedback-text">Unexpected error at pm.test.invalid and [2001:db8::1]</span>` +
				`<p class="instruction">Go to https://pm.test.invalid:9443/realms/test</p></html>`
			return keycloakMessage([]byte(page))
		}, `known texts ["Invalid username or password."] and 2 other texts`},
		{"Keycloak form action of another origin", func() string {
			page := `<form id="kc-form-login" action="https://[2001:db8::1]:9443/realms/test/login-actions/authenticate?x=192.0.2.10" method="post">`
			_, err := loginFormAction([]byte(page), "https://pm.test.invalid:9443/realms/test")
			return err.Error()
		}, "not an https URL of the Keycloak origin"},
		{"callback error with a host", func() string {
			_, _, err := callbackCodeAndState("https://pm.test.invalid:9443/v1/sessions/browser/callback?error=https://192.0.2.10&error_description=pm.test.invalid", "https://pm.test.invalid:9443")
			return err.Error()
		}, "does not have the form of an OAuth error code"},
		{"callback error code", func() string {
			_, _, err := callbackCodeAndState("https://pm.test.invalid:9443/v1/sessions/browser/callback?error=access_denied&error_description=see+192.0.2.10", "https://pm.test.invalid:9443")
			return err.Error()
		}, "with the error access_denied"},
		{"callback to another place", func() string {
			_, _, err := callbackCodeAndState("https://[2001:db8::1]:9443/other?code=192.0.2.10", "https://pm.test.invalid:9443")
			return err.Error()
		}, "did not redirect to the PM callback path"},
		{"PM problem code with a host", func() string {
			return checkDecision(operationDeviceApprove, 403, []byte(`{"code":"https://192.0.2.10:9443"}`)).Error()
		}, "not a PM problem document"},
		{"PM answer that is not a problem", func() string {
			return checkDecision(operationDeviceDeny, 502, []byte("bad gateway from [2001:db8::1]:9443 pm.test.invalid")).Error()
		}, operationDeviceDeny},
	}
	for _, answer := range answers {
		t.Run(answer.name, func(t *testing.T) {
			message := answer.message()
			checkNoForms(t, message, syntheticPlaces)
			if !strings.Contains(message, answer.want) {
				t.Fatalf("the failure message does not hold %q:\n%s", answer.want, message)
			}
		})
	}
}
