package acceptance

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// writtenIdentity writes the configuration of the oracle deployment and
// returns its identity object, each value as the raw bytes of the file.
// The file must be valid JSON and hold no placeholder.
func writtenIdentity(t *testing.T, d *deployment) map[string]json.RawMessage {
	t.Helper()
	path, err := d.configurationPath()
	if err != nil {
		t.Fatalf("write the oracle configuration: %v", err)
	}
	contents, err := readBoundedFile(path, laneFileLimit)
	if err != nil {
		t.Fatalf("read the oracle configuration: %v", err)
	}
	if !json.Valid(contents) {
		t.Fatal("the written configuration is not valid JSON")
	}
	if bytes.Contains(contents, []byte("pm-acceptance-device-poll-interval-")) {
		t.Fatal("the written configuration holds the placeholder")
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(contents, &document); err != nil {
		t.Fatalf("decode the oracle configuration: %v", err)
	}
	var identity map[string]json.RawMessage
	if err := json.Unmarshal(document["identity"], &identity); err != nil {
		t.Fatalf("decode the oracle identity object: %v", err)
	}
	return identity
}

// The configuration writer writes the token of a setting step byte for
// byte, with no escaping, compaction or indentation, null included. It
// writes no key without a token. Developer check, not acceptance proof.
func TestDevicePollIntervalWriterKeepsTheToken(t *testing.T) {
	_, d := oracleBundleScenario(t)
	identity := writtenIdentity(t, d)
	if _, found := identity["device_poll_interval"]; found {
		t.Fatal("the writer writes identity.device_poll_interval without a token")
	}
	if len(identity) != 4 {
		t.Fatalf("the identity object has %d keys without a token, want 4", len(identity))
	}
	steps := []struct {
		name  string
		set   func(*devicePollIntervalScenario) error
		token string
	}{
		{"60 seconds", func(s *devicePollIntervalScenario) error { return s.setSeconds("60") }, `"60s"`},
		{"1 second", func(s *devicePollIntervalScenario) error { return s.setSeconds("1") }, `"1s"`},
		{`"60s"`, func(s *devicePollIntervalScenario) error { return s.setJSONValue(`"60s"`) }, `"60s"`},
		{"60", func(s *devicePollIntervalScenario) error { return s.setJSONValue(`60`) }, `60`},
		{"null", func(s *devicePollIntervalScenario) error { return s.setJSONValue(`null`) }, `null`},
		{"object with a space", func(s *devicePollIntervalScenario) error { return s.setJSONValue(`{"a": 1}`) }, `{"a": 1}`},
		{`"<5s>"`, func(s *devicePollIntervalScenario) error { return s.setJSONValue(`"<5s>"`) }, `"<5s>"`},
		{`"+5s"`, func(s *devicePollIntervalScenario) error { return s.setJSONValue(`"+5s"`) }, `"+5s"`},
		{"65 bytes", func(s *devicePollIntervalScenario) error {
			return s.setJSONValue(`"0000000000000000000000000000000000000000000000000000000000000001s"`)
		}, `"0000000000000000000000000000000000000000000000000000000000000001s"`},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			bundle, d := oracleBundleScenario(t)
			s := &devicePollIntervalScenario{shared: bundle.shared}
			if err := step.set(s); err != nil {
				t.Fatalf("the step refuses the value: %v", err)
			}
			if d.configuration != "" {
				t.Fatal("the setting step wrote the configuration")
			}
			identity := writtenIdentity(t, d)
			if got := identity["device_poll_interval"]; !bytes.Equal(got, []byte(step.token)) {
				t.Fatalf("identity.device_poll_interval is %s, want %s", got, step.token)
			}
			if len(identity) != 5 {
				t.Fatalf("the identity object has %d keys with a token, want 5", len(identity))
			}
			if err := s.setSeconds("60"); err == nil {
				t.Fatal("the step accepts a second setting")
			}
		})
	}
}

// The setting steps refuse invalid JSON and a written configuration.
// Developer check, not acceptance proof.
func TestDevicePollIntervalStepsRefuse(t *testing.T) {
	bundle, d := oracleBundleScenario(t)
	s := &devicePollIntervalScenario{shared: bundle.shared}
	for _, value := range []string{`60s`, `"60s`, `{"a":`, `60 60`} {
		if err := s.setJSONValue(value); err == nil {
			t.Errorf("the step accepts the JSON value %s", value)
		}
	}
	if d.devicePollInterval != nil {
		t.Fatal("a refused value is kept")
	}
	if _, err := d.configurationPath(); err != nil {
		t.Fatalf("write the oracle configuration: %v", err)
	}
	if err := s.setSeconds("60"); err == nil {
		t.Error("the step accepts a written configuration")
	}
	if err := s.setJSONValue(`null`); err == nil {
		t.Error("the step accepts a written configuration")
	}
	if d.devicePollInterval != nil {
		t.Fatal("a refused setting is kept")
	}
}

// The D1 parser keeps only an integer poll_interval_seconds of a login-start
// answer. Developer check, not acceptance proof.
func TestLoginStartPollIntervalParse(t *testing.T) {
	text := "challenge_id\t\"c1\"\ndevice_proof\t\"p1\"\npoll_interval_seconds\t60\nuser_code\t\"u1\"\n"
	if got, ok := loginStartPollInterval([]byte(text)); !ok || got != 60 {
		t.Errorf("text output: got %d, %t, want 60, true", got, ok)
	}
	document := "{\n  \"device_proof\": \"p1\",\n  \"poll_interval_seconds\": 5\n}\n"
	if got, ok := loginStartPollInterval([]byte(document)); !ok || got != 5 {
		t.Errorf("JSON output: got %d, %t, want 5, true", got, ok)
	}
	for _, bad := range []string{
		"",
		"poll_interval_seconds\t60",
		"poll_interval_seconds\t\"60\"\n",
		"poll_interval_seconds\t6e1\n",
		"poll_interval_seconds\t60.0\n",
		"poll_interval_seconds\t60\npoll_interval_seconds\t60\n",
		"device_proof\t\"p1\"\n",
		"device_proof \"p1\"\npoll_interval_seconds\t60\n",
		"{\"poll_interval_seconds\": \"5\"}\n",
		"{\"poll_interval_seconds\": 5\n",
	} {
		if got, ok := loginStartPollInterval([]byte(bad)); ok {
			t.Errorf("the parser accepts %q as %d", bad, got)
		}
	}
}

// The bootstrap stderr description shows one configuration refusal line and
// no other text. Developer check, not acceptance proof.
func TestBootstrapStderrShowsOnlyConfigurationRefusalLines(t *testing.T) {
	refusal := "pm-server: configuration unknown at identity.device_poll_interval\n"
	want := `66 bytes, the configuration refusal line "pm-server: configuration unknown at identity.device_poll_interval"`
	if got := describeStderr(refusal); got != want {
		t.Fatalf("the description of a refusal line is %q, want %q", got, want)
	}
	const password = "Zq9-oracle-Pass"
	for name, output := range map[string]string{
		"a configured value":    "pm-server: configuration value at identity.device_poll_interval: " + password + "\n",
		"a value in the field":  "pm-server: configuration value at " + password + "\n",
		"two lines":             refusal + refusal,
		"a second line":         refusal + password + "\n",
		"no line feed":          strings.TrimSuffix(refusal, "\n"),
		"text before the line":  password + " " + refusal,
		"a kind with a capital": "pm-server: configuration Value at identity.device_poll_interval\n",
	} {
		got := describeStderr(output)
		if got != describeOutput(output) || strings.Contains(got, "configuration") || strings.Contains(got, password) {
			t.Errorf("%s: the description shows the text: %q", name, got)
		}
	}
}
