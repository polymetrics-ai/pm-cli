package acceptance

import (
	"strings"
	"testing"
	"time"
)

// These are Go developer checks of the scenario 1 parts of criteria v3 P5
// (L-d RED round 3). They are not acceptance proof. They start no process and
// need no stack. A failure message must hold no proof, no user code, no host
// and no address.

// TestDeviceLoginExchangeExitLimit checks the exit limit that the step
// derives from the started command line: --request-timeout plus 30 s, the
// CLI default 30 s plus 30 s without the flag, and a fixed refusal for a
// value that the CLI does not accept.
func TestDeviceLoginExchangeExitLimit(t *testing.T) {
	accepted := []struct {
		args []string
		want time.Duration
	}{
		{[]string{"sessions", "login-exchange", "--wait=true", "--request-timeout=2m0s"}, 150 * time.Second},
		{[]string{"sessions", "login-exchange", "--wait=true"}, 60 * time.Second},
		{[]string{"sessions", "login-exchange", "--request-timeout", "1s"}, 31 * time.Second},
		{[]string{"sessions", "login-exchange", "--request-timeout=5m0s"}, 330 * time.Second},
	}
	for _, c := range accepted {
		got, err := exchangeExitLimit(c.args)
		if err != nil || got != c.want {
			t.Fatalf("exchangeExitLimit(%q) = %s, %v; want %s", c.args, got, err, c.want)
		}
	}
	refused := [][]string{
		{"--request-timeout=120s"},
		{"--request-timeout=0s"},
		{"--request-timeout=500ms"},
		{"--request-timeout=6m0s"},
		{"--request-timeout=two-minutes"},
		{"--request-timeout="},
		{"--request-timeout"},
		{"--request-timeout=2m0s", "--request-timeout=1m0s"},
	}
	for _, args := range refused {
		if got, err := exchangeExitLimit(args); err == nil {
			t.Fatalf("exchangeExitLimit(%q) = %s, want a refusal", args, got)
		} else if strings.Contains(err.Error(), "two-minutes") || strings.Contains(err.Error(), "120s") {
			t.Fatalf("the refusal shows the value: %s", err)
		}
	}
}

// TestDeviceLoginNoticeLineWithWait checks the notice step of scenario 1:
// the line must name device_poll_too_soon and a wait of at least the given
// seconds.
func TestDeviceLoginNoticeLineWithWait(t *testing.T) {
	notice := func(code string, wait string) string {
		return "notice: device login exchange received " + code + ". The CLI waits " + wait + "s before the next exchange request."
	}
	cases := []struct {
		name string
		line string
		pass bool
		want string
	}{
		{"too soon, wait 10", notice("device_poll_too_soon", "10"), false, "a wait of 10 seconds, want at least 11"},
		{"too soon, wait 11", notice("device_poll_too_soon", "11"), true, ""},
		{"too soon, wait 65", notice("device_poll_too_soon", "65"), true, ""},
		{"pending, wait 65", notice("device_authorization_pending", "65"), false, "names device_authorization_pending, want device_poll_too_soon"},
		{"wait too large for an integer", notice("device_poll_too_soon", strings.Repeat("9", 40)), false, "not an integer that the step can read"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &deviceLoginScenario{
				shared:    &stackScenario{},
				challenge: &deviceChallenge{challengeID: "device-1", userCode: syntheticUserCode, proof: syntheticProof},
			}
			s.exchange = endedProcess(0, c.line+"\n"+leakyText(), firstLineOf(c.line+"\n"))
			err := s.noticeLineWithWait(t.Context(), 11)
			if c.pass {
				if err != nil {
					t.Fatalf("the step failed: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("the step passed")
			}
			checkNoForms(t, err.Error(), append(forbiddenSecretForms(), syntheticPlaces...))
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("the failure message does not hold %q:\n%s", c.want, err.Error())
			}
		})
	}
	// The step of scenarios 2 to 4 still accepts either code with any wait.
	s := &deviceLoginScenario{shared: &stackScenario{}}
	line := notice("device_authorization_pending", "5")
	s.exchange = endedProcess(0, line+"\n", firstLineOf(line+"\n"))
	if err := s.noticeLine(t.Context(), ""); err != nil {
		t.Fatalf("the notice step of scenarios 2 to 4 failed: %v", err)
	}
}

// TestDeviceLoginChallengePollInterval checks the step that reads
// poll_interval_seconds from the kept login-start stdout.
func TestDeviceLoginChallengePollInterval(t *testing.T) {
	stdout := "challenge_id\t\"device-1\"\n" +
		"device_proof\t\"" + syntheticProof + "\"\n" +
		"expires_at\t\"2026-11-17T12:00:00Z\"\n" +
		"poll_interval_seconds\t60\n" +
		"user_code\t\"" + syntheticUserCode + "\"\n" +
		"verification_uri\t\"https://pm.test.invalid:9443/login/device\"\n"
	scenario := func(stdout string) *deviceLoginScenario {
		return &deviceLoginScenario{challenge: &deviceChallenge{challengeID: "device-1", userCode: syntheticUserCode, proof: syntheticProof, stdout: []byte(stdout)}}
	}
	if err := scenario(stdout).challengePollInterval(60); err != nil {
		t.Fatalf("the step failed for a matching interval: %v", err)
	}
	forbidden := append(forbiddenSecretForms(), syntheticPlaces...)
	err := scenario(stdout).challengePollInterval(5)
	if err == nil {
		t.Fatal("the step passed for a mismatching interval")
	}
	checkNoForms(t, err.Error(), forbidden)
	if err.Error() != "the device login challenge states a poll interval of 60 seconds, want 5" {
		t.Fatalf("the mismatch message is not the two integers only: %s", err)
	}
	err = scenario(strings.Replace(stdout, "poll_interval_seconds\t60", "poll_interval_seconds\t\"60\"", 1)).challengePollInterval(60)
	if err == nil {
		t.Fatal("the step passed for a poll interval that is not a JSON integer")
	}
	checkNoForms(t, err.Error(), forbidden)
	if err := (&deviceLoginScenario{}).challengePollInterval(60); err == nil {
		t.Fatal("the step passed with no device login")
	}
}
