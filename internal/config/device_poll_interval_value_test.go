package config_test

import (
	"testing"
	"time"
)

// Go developer check, not acceptance proof (issue #366, developer check C1b).
// The decoded identity.device_poll_interval: 5 s without the key, and the
// parsed duration of each valid value. This file does not build on 4dd99e3,
// which has no such field. Defect: a wrong default or a wrong decoded value.
func TestDevicePollIntervalValue(t *testing.T) {
	for _, test := range []struct {
		token string // empty: the configuration has no such key
		want  time.Duration
	}{
		{"", 5 * time.Second},
		{`"60s"`, time.Minute},
		{`"1m"`, time.Minute},
		{`".5m"`, 30 * time.Second},
		{`"1s"`, time.Second},
	} {
		t.Run("token "+test.token, func(t *testing.T) {
			raw := startupConfiguration
			if test.token != "" {
				raw = withDevicePollInterval(t, test.token)
			}
			configuration, err := loadAndValidate(raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := configuration.Identity().DevicePollInterval; got != test.want {
				t.Fatalf("device poll interval = %s, want %s", got, test.want)
			}
		})
	}
}
