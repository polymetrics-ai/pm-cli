package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/config"
)

// withDevicePollInterval returns the startup configuration with the JSON token
// as the value of identity.device_poll_interval.
func withDevicePollInterval(t *testing.T, token string) string {
	t.Helper()
	raw := strings.Replace(
		startupConfiguration,
		`"client_secret_ref":"secret://fixture/oidc"`,
		`"client_secret_ref":"secret://fixture/oidc","device_poll_interval":`+token,
		1,
	)
	if raw == startupConfiguration {
		t.Fatal("identity fixture was not replaced")
	}
	return raw
}

// loadAndValidate runs the shared refusal path of the executables:
// config.Load, then config.Validate.
func loadAndValidate(raw string) (config.Configuration, error) {
	document, err := config.Load(strings.NewReader(raw))
	if err != nil {
		return config.Configuration{}, err
	}
	return config.Validate(document, config.Prelaunch{})
}

// Go developer check, not acceptance proof (issue #366, developer check C1a).
// It checks the whole contract of identity.device_poll_interval through
// config.Load and config.Validate only. Each refusal gives its exact kind and
// field, and so the exact text that pm-server writes after "pm-server: ".
// Defect: a lost bound, a missing whole-second check, a lost decoder limit,
// or a parse failure that is reported as bounds.
func TestDevicePollIntervalContract(t *testing.T) {
	// P1i: 65 bytes that would parse as 1 s.
	tooLong := strings.Repeat("0", 63) + "1s"
	if len(tooLong) != 65 {
		t.Fatalf("the long fixture has %d bytes, want 65", len(tooLong))
	}
	for _, token := range []string{`"1s"`, `"60s"`, `"1m"`} {
		t.Run("accepts "+token, func(t *testing.T) {
			if _, err := loadAndValidate(withDevicePollInterval(t, token)); err != nil {
				t.Fatalf("device poll interval %s was refused: %v", token, err)
			}
		})
	}
	for _, test := range []struct {
		token string
		kind  config.DiagnosticKind
	}{
		{`"0s"`, config.DiagnosticBounds},
		{`"61s"`, config.DiagnosticBounds},
		{`"1500ms"`, config.DiagnosticBounds},
		{`"-5s"`, config.DiagnosticBounds},
		{`"5"`, config.DiagnosticValue},
		{`"+5s"`, config.DiagnosticValue},
		{`"` + tooLong + `"`, config.DiagnosticValue},
		{`"9999999999999999999s"`, config.DiagnosticValue},
		{`60`, config.DiagnosticType},
		{`null`, config.DiagnosticType},
	} {
		t.Run("refuses "+test.token, func(t *testing.T) {
			_, err := loadAndValidate(withDevicePollInterval(t, test.token))
			want := "configuration " + string(test.kind) + " at identity.device_poll_interval"
			var diagnostic *config.Diagnostic
			if !errors.As(err, &diagnostic) || err.Error() != want {
				t.Fatalf("device poll interval %s gave %v, want %q", test.token, err, want)
			}
		})
	}
}
