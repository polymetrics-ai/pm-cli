package acceptance

import (
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
)

// TestDeviceLoginFailureMessagesRedactTheProof is a Go developer check of the
// device login step helpers (L-d RED, coverage map row 7; D4-10, D4-11). It
// is not acceptance proof: a Gherkin scenario cannot fail a step on purpose.
// It gives each failure message of the new steps an output that holds the
// device proof and the user code in several forms, and checks that the
// message holds none of them. It starts no process and needs no stack.
func TestDeviceLoginFailureMessagesRedactTheProof(t *testing.T) {
	const proof = "pm-device-proof-Zx81_q7Lm2Vt9Rk4-Wn3Hy6Pb0"
	const userCode = "WXYZ-7Q2K"
	leaky := strings.Join([]string{
		"error: device_poll_too_soon: proof " + proof + " (incident 1)",
		"base64 " + base64.StdEncoding.EncodeToString([]byte(proof)),
		"base64url " + base64.RawURLEncoding.EncodeToString([]byte(proof)),
		"query " + url.QueryEscape("proof="+proof),
		"hex " + hex.EncodeToString([]byte(proof)),
		"user code " + userCode,
	}, "\n") + "\n"

	scenario := func() *deviceLoginScenario {
		return &deviceLoginScenario{
			shared:    &stackScenario{},
			challenge: &deviceChallenge{challengeID: "challenge-1", userCode: userCode, proof: proof},
		}
	}
	// ended returns a login-exchange process that exited with the code and
	// wrote stderr, with its first stderr line already read.
	ended := func(code int, stderr string, first firstLine) *exchangeProcess {
		p := &exchangeProcess{
			first:       make(chan firstLine, 1),
			outputEnded: make(chan struct{}),
			exited:      make(chan struct{}),
			status:      processStatus{exited: true, code: code, text: "exit status"},
		}
		p.stderr.write([]byte(stderr))
		p.first <- first
		close(p.outputEnded)
		close(p.exited)
		return p
	}
	firstOf := func(text string) firstLine {
		line, _, found := strings.Cut(text, "\n")
		return firstLine{line: line, found: found}
	}
	pendingNotice := "notice: device login exchange received device_authorization_pending. The CLI waits 5s before the next exchange request."

	cases := []struct {
		name string
		run  func(*deviceLoginScenario) error
	}{
		{"notice: the first line is not the notice line", func(s *deviceLoginScenario) error {
			s.exchange = ended(5, leaky, firstOf(leaky))
			return s.noticeLine(t.Context(), "")
		}},
		{"notice: end of file before a complete line", func(s *deviceLoginScenario) error {
			s.exchange = ended(5, "partial "+proof, firstLine{line: "partial " + proof, found: false})
			return s.noticeLine(t.Context(), "")
		}},
		{"notice: the notice names the other code", func(s *deviceLoginScenario) error {
			s.exchange = ended(0, pendingNotice+"\n"+leaky, firstOf(pendingNotice+"\n"))
			return s.noticeLine(t.Context(), "device_poll_too_soon")
		}},
		{"exit: wrong exit code", func(s *deviceLoginScenario) error {
			s.exchange = ended(5, leaky, firstOf(leaky))
			return s.exitsWithCode(0)
		}},
		{"stderr names: code not named", func(s *deviceLoginScenario) error {
			s.exchange = ended(5, leaky, firstOf(leaky))
			return s.stderrNames("device_authorization_denied")
		}},
		{"SIGINT: the process ended before the signal", func(s *deviceLoginScenario) error {
			s.exchange = ended(5, leaky, firstOf(leaky))
			return s.sendSIGINT()
		}},
		{"stderr longer than its bound", func(s *deviceLoginScenario) error {
			s.exchange = ended(5, strings.Repeat("x", deviceLoginOutputLimit-10)+proof, firstLine{})
			return s.exitsWithCode(0)
		}},
		{"login-start: nonzero exit", func(s *deviceLoginScenario) error {
			s.challenge = nil
			_, err := s.checkLoginStart(5, "device_proof\t\""+proof+"\"\nuser_code\t\""+userCode+"\"\n", leaky)
			return err
		}},
		{"login-start: unreadable output", func(s *deviceLoginScenario) error {
			s.challenge = nil
			_, err := s.checkLoginStart(0, "device_proof\t\""+proof+"\"\ndevice_proof\t\""+proof+"\"\n", "")
			return err
		}},
		{"decision: not a problem document", func(s *deviceLoginScenario) error {
			return s.checkDecision("/v1/sessions/device/approve", 403, []byte(leaky))
		}},
		{"decision: a problem code that is not a code", func(s *deviceLoginScenario) error {
			return s.checkDecision("/v1/sessions/device/deny", 403, []byte(`{"code":"`+proof+`"}`))
		}},
		{"Keycloak page text", func(s *deviceLoginScenario) error {
			page := []byte(`<title>Sign in ` + proof + `</title><span id="input-error">` + userCode + `</span>`)
			_, err := loginFormAction(page, "https://keycloak.invalid/realms/test")
			if err == nil {
				return nil
			}
			return &messageError{err.Error() + ": " + s.keycloakMessage(page)}
		}},
	}
	forbidden := []string{proof, userCode}
	forbidden = append(forbidden, deviceLoginSecretForms(proof)...)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.run(scenario())
			if err == nil {
				t.Fatal("the step helper returned no failure")
			}
			for _, form := range forbidden {
				if form != "" && strings.Contains(err.Error(), form) {
					t.Fatalf("the failure message holds a form of a device login secret:\n%s", strings.ReplaceAll(err.Error(), form, "<SECRET>"))
				}
			}
		})
	}
}

type messageError struct{ text string }

func (e *messageError) Error() string { return e.text }
