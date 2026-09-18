package config_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const providerHTTPDestinations = `[{"org_id":"org-alpha","workspace_id":"workspace-alpha","connection_id":"github-main","origin":"https://api.github.com"},{"org_id":"org-alpha","workspace_id":"workspace-beta","connection_id":"github-main","origin":"https://github.enterprise.invalid/api/v3"}]`

const providerHTTPConfiguration = `{"max_request_bytes":1048576,"max_response_bytes":8388608,"max_dns_answers":8,"max_attempts":3,"max_retry_delay":"45s","destinations":` + providerHTTPDestinations + `}`

func TestProviderHTTPPolicySelection(t *testing.T) {
	alpha := contract.Scope{OrgID: "org-alpha", WorkspaceID: "workspace-alpha"}
	beta := contract.Scope{OrgID: "org-alpha", WorkspaceID: "workspace-beta"}

	t.Run("absence grants no HTTP authority", func(t *testing.T) {
		selector := loadProviderHTTPConfiguration(t, startupConfiguration).ProviderHTTP()
		if selector.Configured() {
			t.Fatal("absent provider HTTP configuration became configured")
		}
		if policy, present := selector.Policy(alpha, "github-main"); present || policy != (contract.HTTPExecutionPolicy{}) {
			t.Fatalf("absent provider HTTP policy = (%#v, %t); want zero, false", policy, present)
		}
	})

	configuration := loadProviderHTTPConfiguration(t, configuredProviderHTTPDocument(providerHTTPConfiguration))
	selector := configuration.ProviderHTTP()
	if !selector.Configured() {
		t.Fatal("complete provider HTTP configuration remained unavailable")
	}
	wantAlpha := contract.HTTPExecutionPolicy{
		MaxRequestBytes:  1048576,
		MaxResponseBytes: 8388608,
		MaxDNSAnswers:    8,
		MaxAttempts:      3,
		MaxRetryDelay:    45 * time.Second,
		SelectedOrigin:   "https://api.github.com",
	}
	alphaPolicy, present := selector.Policy(alpha, "github-main")
	if !present || alphaPolicy != wantAlpha {
		t.Fatalf("alpha policy = (%#v, %t); want (%#v, true)", alphaPolicy, present, wantAlpha)
	}
	wantBeta := wantAlpha
	wantBeta.SelectedOrigin = "https://github.enterprise.invalid/api/v3"
	betaPolicy, present := selector.Policy(beta, "github-main")
	if !present || betaPolicy != wantBeta {
		t.Fatalf("beta policy = (%#v, %t); want (%#v, true)", betaPolicy, present, wantBeta)
	}

	t.Run("inclusive exact integer bounds remain selectable", func(t *testing.T) {
		atBounds := strings.NewReplacer(
			`"max_request_bytes":1048576`, `"max_request_bytes":9223372036854775806`,
			`"max_response_bytes":8388608`, `"max_response_bytes":9223372036854775806`,
			`"max_dns_answers":8`, `"max_dns_answers":4294967295`,
			`"max_attempts":3`, `"max_attempts":4294967295`,
			`"max_retry_delay":"45s"`, `"max_retry_delay":"1ns"`,
		).Replace(providerHTTPConfiguration)
		policy, selected := loadProviderHTTPConfiguration(
			t,
			configuredProviderHTTPDocument(atBounds),
		).ProviderHTTP().Policy(alpha, "github-main")
		if !selected ||
			policy.MaxRequestBytes != 9223372036854775806 ||
			policy.MaxResponseBytes != 9223372036854775806 ||
			policy.MaxDNSAnswers != 4294967295 ||
			policy.MaxAttempts != 4294967295 ||
			policy.MaxRetryDelay != time.Nanosecond {
			t.Fatalf("boundary policy = (%#v, %t); want exact inclusive controls", policy, selected)
		}
	})
	for _, query := range []struct {
		scope        contract.Scope
		connectionID string
	}{
		{scope: contract.Scope{OrgID: "org-alpha", WorkspaceID: "workspace-foreign"}, connectionID: "github-main"},
		{scope: contract.Scope{OrgID: "org-foreign", WorkspaceID: "workspace-alpha"}, connectionID: "github-main"},
		{scope: alpha, connectionID: "GitHub-main"},
		{scope: alpha, connectionID: "missing"},
	} {
		if policy, selected := selector.Policy(query.scope, query.connectionID); selected || policy != (contract.HTTPExecutionPolicy{}) {
			t.Fatalf("foreign policy selection for %#v/%q = (%#v, %t); want zero, false", query.scope, query.connectionID, policy, selected)
		}
	}

	alphaPolicy.MaxAttempts = 99
	alphaPolicy.SelectedOrigin = "https://mutated.example.invalid"
	selectedAgain, present := selector.Policy(alpha, "github-main")
	if !present || selectedAgain != wantAlpha {
		t.Fatalf("caller mutation changed retained policy: (%#v, %t); want (%#v, true)", selectedAgain, present, wantAlpha)
	}
	fromConfiguration, present := configuration.ProviderHTTP().Policy(alpha, "github-main")
	if !present || fromConfiguration != wantAlpha {
		t.Fatalf("configuration getter exposed mutable policy storage: (%#v, %t); want (%#v, true)", fromConfiguration, present, wantAlpha)
	}
}

func TestProviderHTTPConfigurationRefusals(t *testing.T) {
	withoutAttempt := strings.Replace(providerHTTPConfiguration, `,"max_attempts":3`, ``, 1)
	unknownLimit := strings.Replace(providerHTTPConfiguration, `"max_request_bytes":1048576`, `"max_request_bytes":1048576,"request_bytes":1048576`, 1)
	duplicateAttempt := strings.Replace(providerHTTPConfiguration, `"max_attempts":3`, `"max_attempts":3,"max_attempts":3`, 1)
	missingOrigin := strings.Replace(providerHTTPConfiguration, `,"origin":"https://api.github.com"`, ``, 1)
	unknownDestination := strings.Replace(providerHTTPConfiguration, `"origin":"https://api.github.com"`, `"origin":"https://api.github.com","base_url":"https://api.github.com"`, 1)
	duplicateBinding := strings.Replace(providerHTTPConfiguration, `"workspace_id":"workspace-beta"`, `"workspace_id":"workspace-alpha"`, 1)

	for _, test := range []struct {
		name     string
		document string
		field    string
		kind     config.DiagnosticKind
	}{
		{
			name:     "null block",
			document: configuredProviderHTTPDocument(`null`),
			field:    "provider_http",
			kind:     config.DiagnosticType,
		},
		{
			name:     "partial block",
			document: configuredProviderHTTPDocument(withoutAttempt),
			field:    "provider_http.max_attempts",
			kind:     config.DiagnosticRequired,
		},
		{
			name:     "wrong-case root",
			document: strings.Replace(configuredProviderHTTPDocument(providerHTTPConfiguration), `"provider_http"`, `"Provider_HTTP"`, 1),
			field:    "Provider_HTTP",
			kind:     config.DiagnosticUnknown,
		},
		{
			name:     "unknown request limit alias",
			document: configuredProviderHTTPDocument(unknownLimit),
			field:    "provider_http.request_bytes",
			kind:     config.DiagnosticUnknown,
		},
		{
			name:     "wrong-case control",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, `"max_attempts"`, `"MaxAttempts"`, 1)),
			field:    "provider_http.MaxAttempts",
			kind:     config.DiagnosticUnknown,
		},
		{
			name:     "duplicate control",
			document: configuredProviderHTTPDocument(duplicateAttempt),
			field:    "provider_http.max_attempts",
			kind:     config.DiagnosticDuplicate,
		},
		{
			name:     "integer string is not coerced",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, `"max_request_bytes":1048576`, `"max_request_bytes":"1048576"`, 1)),
			field:    "provider_http.max_request_bytes",
			kind:     config.DiagnosticType,
		},
		{
			name:     "fractional attempt count",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, `"max_attempts":3`, `"max_attempts":3.0`, 1)),
			field:    "provider_http.max_attempts",
			kind:     config.DiagnosticType,
		},
		{
			name:     "negative DNS answer count",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, `"max_dns_answers":8`, `"max_dns_answers":-1`, 1)),
			field:    "provider_http.max_dns_answers",
			kind:     config.DiagnosticValue,
		},
		{
			name:     "DNS answer width overflow",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, `"max_dns_answers":8`, `"max_dns_answers":4294967296`, 1)),
			field:    "provider_http.max_dns_answers",
			kind:     config.DiagnosticBounds,
		},
		{
			name:     "attempt width overflow",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, `"max_attempts":3`, `"max_attempts":4294967296`, 1)),
			field:    "provider_http.max_attempts",
			kind:     config.DiagnosticBounds,
		},
		{
			name:     "zero request byte bound",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, `"max_request_bytes":1048576`, `"max_request_bytes":0`, 1)),
			field:    "provider_http.max_request_bytes",
			kind:     config.DiagnosticBounds,
		},
		{
			name:     "request sentinel byte overflows signed read",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, `"max_request_bytes":1048576`, `"max_request_bytes":9223372036854775807`, 1)),
			field:    "provider_http.max_request_bytes",
			kind:     config.DiagnosticBounds,
		},
		{
			name:     "response sentinel byte overflows signed read",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, `"max_response_bytes":8388608`, `"max_response_bytes":9223372036854775807`, 1)),
			field:    "provider_http.max_response_bytes",
			kind:     config.DiagnosticBounds,
		},
		{
			name:     "zero attempt count",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, `"max_attempts":3`, `"max_attempts":0`, 1)),
			field:    "provider_http.max_attempts",
			kind:     config.DiagnosticBounds,
		},
		{
			name:     "duration number is not a string",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, `"max_retry_delay":"45s"`, `"max_retry_delay":45`, 1)),
			field:    "provider_http.max_retry_delay",
			kind:     config.DiagnosticType,
		},
		{
			name:     "malformed duration",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, `"45s"`, `"forever"`, 1)),
			field:    "provider_http.max_retry_delay",
			kind:     config.DiagnosticValue,
		},
		{
			name:     "zero duration",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, `"45s"`, `"0s"`, 1)),
			field:    "provider_http.max_retry_delay",
			kind:     config.DiagnosticBounds,
		},
		{
			name:     "null destinations",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, providerHTTPDestinations, `null`, 1)),
			field:    "provider_http.destinations",
			kind:     config.DiagnosticType,
		},
		{
			name:     "destinations object is not an array",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, providerHTTPDestinations, `{}`, 1)),
			field:    "provider_http.destinations",
			kind:     config.DiagnosticType,
		},
		{
			name:     "empty destinations",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, providerHTTPDestinations, `[]`, 1)),
			field:    "provider_http.destinations",
			kind:     config.DiagnosticBounds,
		},
		{
			name:     "null destination entry",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, providerHTTPDestinations, `[null]`, 1)),
			field:    "provider_http.destinations[0]",
			kind:     config.DiagnosticType,
		},
		{
			name:     "missing destination origin",
			document: configuredProviderHTTPDocument(missingOrigin),
			field:    "provider_http.destinations[0].origin",
			kind:     config.DiagnosticRequired,
		},
		{
			name:     "unknown destination alias",
			document: configuredProviderHTTPDocument(unknownDestination),
			field:    "provider_http.destinations[0].base_url",
			kind:     config.DiagnosticUnknown,
		},
		{
			name:     "wrong-case destination field",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, `"org_id"`, `"OrgID"`, 1)),
			field:    "provider_http.destinations[0].OrgID",
			kind:     config.DiagnosticUnknown,
		},
		{
			name:     "null connection identity",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, `"connection_id":"github-main"`, `"connection_id":null`, 1)),
			field:    "provider_http.destinations[0].connection_id",
			kind:     config.DiagnosticType,
		},
		{
			name:     "noncanonical organization identity",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, `"org_id":"org-alpha"`, `"org_id":" org-alpha"`, 1)),
			field:    "provider_http.destinations[0].org_id",
			kind:     config.DiagnosticValue,
		},
		{
			name:     "oversized workspace identity",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, `"workspace_id":"workspace-alpha"`, `"workspace_id":"`+strings.Repeat("w", 513)+`"`, 1)),
			field:    "provider_http.destinations[0].workspace_id",
			kind:     config.DiagnosticValue,
		},
		{
			name:     "control character in connection identity",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, `"connection_id":"github-main"`, `"connection_id":"github\u0000main"`, 1)),
			field:    "provider_http.destinations[0].connection_id",
			kind:     config.DiagnosticValue,
		},
		{
			name:     "plain HTTP origin",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, `https://api.github.com`, `http://api.github.com`, 1)),
			field:    "provider_http.destinations[0].origin",
			kind:     config.DiagnosticValue,
		},
		{
			name:     "templated origin",
			document: configuredProviderHTTPDocument(strings.Replace(providerHTTPConfiguration, `https://api.github.com`, `https://{tenant}.github.com`, 1)),
			field:    "provider_http.destinations[0].origin",
			kind:     config.DiagnosticValue,
		},
		{
			name:     "duplicate exact destination binding",
			document: configuredProviderHTTPDocument(duplicateBinding),
			field:    "provider_http.destinations[1].connection_id",
			kind:     config.DiagnosticDuplicate,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			document, err := config.Load(strings.NewReader(test.document))
			if err == nil {
				_, err = config.Validate(document, config.Prelaunch{})
			}
			var diagnostic *config.Diagnostic
			if !errors.As(err, &diagnostic) || diagnostic.Field != test.field || diagnostic.Kind != test.kind {
				t.Fatalf("provider HTTP refusal = %v; want %s/%s", err, test.field, test.kind)
			}
		})
	}
}

func configuredProviderHTTPDocument(providerHTTP string) string {
	return strings.Replace(
		startupConfiguration,
		`"schema_version":"1"`,
		`"provider_http":`+providerHTTP+`,"schema_version":"1"`,
		1,
	)
}

func loadProviderHTTPConfiguration(t *testing.T, raw string) config.Configuration {
	t.Helper()
	document, err := config.Load(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := config.Validate(document, config.Prelaunch{})
	if err != nil {
		t.Fatal(err)
	}
	return configuration
}
