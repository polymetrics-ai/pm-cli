package config_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/config"
)

const startupConfiguration = `{
  "schema_version":"1", "deployment_id":"lifecycle-fixture", "public_origin":"https://pm.example.invalid",
  "postgres":{"ownership":"managed","database":"pm_app","endpoint_ref":"secret://fixture/postgres"},
  "temporal":{"ownership":"managed","endpoint":"temporal.example.invalid:7233","namespace":"fixture","client_identity_ref":"secret://fixture/temporal","retention":"720h","task_queues":["pm-control"]},
  "artifacts":{"ownership":"managed","driver":"s3","endpoint":"https://objects.example.invalid","bucket":"fixture-objects","credential_ref":"secret://fixture/objects","attempt_key_policy":"unique_immutable"},
  "identity":{"issuer":"https://identity.example.invalid","client_id":"pm-server","client_secret_ref":"secret://fixture/oidc"},
  "encryption":{"wrapping_key_ref":"secret://fixture/wrapping"},
  "admission":{"profile":"development"},
  "limits":{"workspace_active_runs":4,"workflow_payload_bytes":65536,"workflow_fanout":32,"run_lifetime":"1h","event_buffer":128,"query_children":1,"query_child_memory_bytes":4294967296,"query_child_deadline":"5m","request_bytes":1048576,"metadata_deadline":"5s"}
}`

func TestTypedServerStartup(t *testing.T) {
	t.Run("E01-C02 explicit bounded settings reach the typed startup boundary", func(t *testing.T) {
		raw := strings.Replace(startupConfiguration, `"schema_version":"1"`, `"server":{"listen":"127.0.0.1:8080","drain_timeout":"2s","trusted_proxies":["127.0.0.1/32"]},"schema_version":"1"`, 1)
		document, err := config.Load(strings.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		configuration, err := config.Validate(document, config.Prelaunch{})
		if err != nil {
			t.Fatal(err)
		}
		if configuration.Server().DrainTimeout != 2*time.Second {
			t.Fatal("explicit fixture drain bound was not preserved")
		}
	})

	for _, fixture := range []struct {
		name   string
		server string
		field  string
		kind   config.DiagnosticKind
	}{
		{"unknown timeout alias", `{"shutdown_timeout":"2s"}`, "server.shutdown_timeout", config.DiagnosticUnknown},
		{"wrong-case field", `{"Drain_Timeout":"2s"}`, "server.Drain_Timeout", config.DiagnosticUnknown},
		{"equal duplicate timeout", `{"drain_timeout":"2s","drain_timeout":"2s"}`, "server.drain_timeout", config.DiagnosticDuplicate},
		{"null object", `null`, "server", config.DiagnosticType},
		{"zero means zero rather than omitted", `{"drain_timeout":"0s"}`, "server.drain_timeout", config.DiagnosticValue},
		{"duration number is not a string", `{"drain_timeout":2}`, "server.drain_timeout", config.DiagnosticType},
		{"unbounded connections", `{"max_connections":0}`, "server.max_connections", config.DiagnosticBounds},
		{"header memory bound", `{"max_header_bytes":1048577}`, "server.max_header_bytes", config.DiagnosticBounds},
		{"fractional connection bound", `{"max_connections":2.5}`, "server.max_connections", config.DiagnosticType},
		{"ambiguous listen host", `{"listen":":8080"}`, "server.listen", config.DiagnosticValue},
		{"wildcard proxy trust", `{"trusted_proxies":["0.0.0.0/0"]}`, "server.trusted_proxies[0]", config.DiagnosticValue},
		{"ambiguous proxy network", `{"trusted_proxies":["127.0.0.1/24"]}`, "server.trusted_proxies[0]", config.DiagnosticValue},
		{"equivalent proxy aliases", `{"trusted_proxies":["127.0.0.1","127.0.0.1/32"]}`, "server.trusted_proxies[1]", config.DiagnosticDuplicate},
	} {
		t.Run("E01-C02 "+fixture.name, func(t *testing.T) {
			raw := strings.Replace(startupConfiguration, `"schema_version":"1"`, `"server":`+fixture.server+`,"schema_version":"1"`, 1)
			document, err := config.Load(strings.NewReader(raw))
			if err == nil {
				_, err = config.Validate(document, config.Prelaunch{})
			}
			var diagnostic *config.Diagnostic
			if !errors.As(err, &diagnostic) || diagnostic.Field != fixture.field || diagnostic.Kind != fixture.kind {
				t.Fatalf("expected %s/%s refusal, got %v", fixture.field, fixture.kind, err)
			}
		})
	}
}
func TestRunLifetimeIsRequiredFinitePolicy(t *testing.T) {
	for _, test := range []struct {
		name        string
		replaceFrom string
		replaceWith string
		kind        config.DiagnosticKind
	}{
		{
			name:        "missing",
			replaceFrom: `,"run_lifetime":"1h"`,
			kind:        config.DiagnosticRequired,
		},
		{
			name:        "zero",
			replaceFrom: `"run_lifetime":"1h"`,
			replaceWith: `"run_lifetime":"0s"`,
			kind:        config.DiagnosticValue,
		},
		{
			name:        "wrong type",
			replaceFrom: `"run_lifetime":"1h"`,
			replaceWith: `"run_lifetime":3600`,
			kind:        config.DiagnosticType,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw := strings.Replace(startupConfiguration, test.replaceFrom, test.replaceWith, 1)
			document, err := config.Load(strings.NewReader(raw))
			if err == nil {
				_, err = config.Validate(document, config.Prelaunch{})
			}
			var diagnostic *config.Diagnostic
			if !errors.As(err, &diagnostic) || diagnostic.Field != "limits.run_lifetime" || diagnostic.Kind != test.kind {
				t.Fatalf("run lifetime refusal = %v; want limits.run_lifetime/%s", err, test.kind)
			}
		})
	}
}
