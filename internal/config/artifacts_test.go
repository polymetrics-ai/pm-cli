package config_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/config"
)

const artifactExecutionConfiguration = `{"region":"us-east-1","max_object_bytes":16777216,"max_in_flight":4,"request_timeout":"30s","organization_reserved_bytes":1073741824,"workspace_reserved_bytes":268435456,"organization_reserved_attempts":4096,"workspace_reserved_attempts":1024}`

func TestTypedArtifactExecutionConfiguration(t *testing.T) {
	t.Run("absence leaves artifact execution unavailable", func(t *testing.T) {
		configuration := loadArtifactConfiguration(t, startupConfiguration)
		if configuration.Artifacts().Execution.Configured() {
			t.Fatal("artifact dependency configuration silently enabled byte execution")
		}
	})

	t.Run("complete explicit controls reach composition", func(t *testing.T) {
		configuration := loadArtifactConfiguration(t, configuredArtifactExecutionDocument(artifactExecutionConfiguration))
		execution := configuration.Artifacts().Execution
		if !execution.Configured() ||
			execution.Region != "us-east-1" ||
			execution.MaxObjectBytes != 16777216 ||
			execution.MaxInFlight != 4 ||
			execution.RequestTimeout != 30*time.Second ||
			execution.OrganizationReservedBytes != 1073741824 ||
			execution.WorkspaceReservedBytes != 268435456 ||
			execution.OrganizationReservedAttempts != 4096 ||
			execution.WorkspaceReservedAttempts != 1024 {
			t.Fatalf("artifact execution configuration was not preserved: %#v", execution)
		}
	})

	t.Run("inclusive finite upper bounds remain valid", func(t *testing.T) {
		atBounds := strings.NewReplacer(
			`"max_object_bytes":16777216`, `"max_object_bytes":140737488355327`,
			`"max_in_flight":4`, `"max_in_flight":65536`,
			`"request_timeout":"30s"`, `"request_timeout":"5m"`,
			`"organization_reserved_bytes":1073741824`, `"organization_reserved_bytes":9223372036854775807`,
			`"workspace_reserved_bytes":268435456`, `"workspace_reserved_bytes":9223372036854775807`,
			`"organization_reserved_attempts":4096`, `"organization_reserved_attempts":9223372036854775807`,
			`"workspace_reserved_attempts":1024`, `"workspace_reserved_attempts":9223372036854775807`,
		).Replace(artifactExecutionConfiguration)
		execution := loadArtifactConfiguration(
			t,
			configuredArtifactExecutionDocument(atBounds),
		).Artifacts().Execution
		if execution.MaxInFlight != 65536 || execution.RequestTimeout != 5*time.Minute {
			t.Fatalf("artifact execution boundary values were not preserved: %#v", execution)
		}
	})
}

func TestArtifactExecutionConfigurationRefusals(t *testing.T) {
	productOverflow := strings.NewReplacer(
		`"max_object_bytes":16777216`, `"max_object_bytes":140737488355328`,
		`"max_in_flight":4`, `"max_in_flight":65536`,
		`"organization_reserved_bytes":1073741824`, `"organization_reserved_bytes":140737488355328`,
		`"workspace_reserved_bytes":268435456`, `"workspace_reserved_bytes":140737488355328`,
	).Replace(artifactExecutionConfiguration)
	verificationOverflow := strings.NewReplacer(
		`"max_object_bytes":16777216`, `"max_object_bytes":9223372036854775807`,
		`"max_in_flight":4`, `"max_in_flight":1`,
		`"organization_reserved_bytes":1073741824`, `"organization_reserved_bytes":9223372036854775807`,
		`"workspace_reserved_bytes":268435456`, `"workspace_reserved_bytes":9223372036854775807`,
	).Replace(artifactExecutionConfiguration)

	for _, test := range []struct {
		name      string
		execution string
		field     string
		kind      config.DiagnosticKind
	}{
		{
			name:      "null block",
			execution: `null`,
			field:     "artifacts.execution",
			kind:      config.DiagnosticType,
		},
		{
			name: "partial block",
			execution: strings.Replace(
				artifactExecutionConfiguration,
				`,"workspace_reserved_attempts":1024`,
				``,
				1,
			),
			field: "artifacts.execution.workspace_reserved_attempts",
			kind:  config.DiagnosticRequired,
		},
		{
			name: "unknown request limit alias",
			execution: strings.Replace(
				artifactExecutionConfiguration,
				`"region":"us-east-1"`,
				`"region":"us-east-1","request_bytes":1048576`,
				1,
			),
			field: "artifacts.execution.request_bytes",
			kind:  config.DiagnosticUnknown,
		},
		{
			name:      "wrong-case field",
			execution: strings.Replace(artifactExecutionConfiguration, `"region"`, `"Region"`, 1),
			field:     "artifacts.execution.Region",
			kind:      config.DiagnosticUnknown,
		},
		{
			name: "duplicate field",
			execution: strings.Replace(
				artifactExecutionConfiguration,
				`"region":"us-east-1"`,
				`"region":"us-east-1","region":"us-east-1"`,
				1,
			),
			field: "artifacts.execution.region",
			kind:  config.DiagnosticDuplicate,
		},
		{
			name:      "region type",
			execution: strings.Replace(artifactExecutionConfiguration, `"region":"us-east-1"`, `"region":1`, 1),
			field:     "artifacts.execution.region",
			kind:      config.DiagnosticType,
		},
		{
			name:      "integer type",
			execution: strings.Replace(artifactExecutionConfiguration, `"max_object_bytes":16777216`, `"max_object_bytes":"16777216"`, 1),
			field:     "artifacts.execution.max_object_bytes",
			kind:      config.DiagnosticType,
		},
		{
			name:      "duration type",
			execution: strings.Replace(artifactExecutionConfiguration, `"request_timeout":"30s"`, `"request_timeout":30`, 1),
			field:     "artifacts.execution.request_timeout",
			kind:      config.DiagnosticType,
		},
		{
			name:      "noncanonical region spelling",
			execution: strings.Replace(artifactExecutionConfiguration, `us-east-1`, `US-EAST-1`, 1),
			field:     "artifacts.execution.region",
			kind:      config.DiagnosticValue,
		},
		{
			name:      "region length bound",
			execution: strings.Replace(artifactExecutionConfiguration, `us-east-1`, strings.Repeat("a", 65), 1),
			field:     "artifacts.execution.region",
			kind:      config.DiagnosticValue,
		},
		{
			name:      "positive object bound",
			execution: strings.Replace(artifactExecutionConfiguration, `"max_object_bytes":16777216`, `"max_object_bytes":0`, 1),
			field:     "artifacts.execution.max_object_bytes",
			kind:      config.DiagnosticBounds,
		},
		{
			name:      "object verification sentinel byte overflows",
			execution: verificationOverflow,
			field:     "artifacts.execution.max_object_bytes",
			kind:      config.DiagnosticBounds,
		},
		{
			name:      "concurrency bound",
			execution: strings.Replace(artifactExecutionConfiguration, `"max_in_flight":4`, `"max_in_flight":65537`, 1),
			field:     "artifacts.execution.max_in_flight",
			kind:      config.DiagnosticBounds,
		},
		{
			name:      "request timeout bound",
			execution: strings.Replace(artifactExecutionConfiguration, `"request_timeout":"30s"`, `"request_timeout":"5m1s"`, 1),
			field:     "artifacts.execution.request_timeout",
			kind:      config.DiagnosticBounds,
		},
		{
			name:      "SQL counter bound",
			execution: strings.Replace(artifactExecutionConfiguration, `"organization_reserved_attempts":4096`, `"organization_reserved_attempts":9223372036854775808`, 1),
			field:     "artifacts.execution.organization_reserved_attempts",
			kind:      config.DiagnosticBounds,
		},
		{
			name:      "object exceeds workspace bytes",
			execution: strings.Replace(artifactExecutionConfiguration, `"max_object_bytes":16777216`, `"max_object_bytes":268435457`, 1),
			field:     "artifacts.execution.max_object_bytes",
			kind:      config.DiagnosticBounds,
		},
		{
			name:      "workspace exceeds organization bytes",
			execution: strings.Replace(artifactExecutionConfiguration, `"organization_reserved_bytes":1073741824`, `"organization_reserved_bytes":268435455`, 1),
			field:     "artifacts.execution.workspace_reserved_bytes",
			kind:      config.DiagnosticBounds,
		},
		{
			name:      "workspace exceeds organization attempts",
			execution: strings.Replace(artifactExecutionConfiguration, `"workspace_reserved_attempts":1024`, `"workspace_reserved_attempts":4097`, 1),
			field:     "artifacts.execution.workspace_reserved_attempts",
			kind:      config.DiagnosticBounds,
		},
		{
			name:      "in-flight byte product overflows signed conversion",
			execution: productOverflow,
			field:     "artifacts.execution.max_in_flight",
			kind:      config.DiagnosticBounds,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			document, err := config.Load(strings.NewReader(configuredArtifactExecutionDocument(test.execution)))
			if err == nil {
				_, err = config.Validate(document, config.Prelaunch{})
			}
			var diagnostic *config.Diagnostic
			if !errors.As(err, &diagnostic) || diagnostic.Field != test.field || diagnostic.Kind != test.kind {
				t.Fatalf("artifact execution refusal = %v; want %s/%s", err, test.field, test.kind)
			}
		})
	}
}

func configuredArtifactExecutionDocument(execution string) string {
	return strings.Replace(
		startupConfiguration,
		`"attempt_key_policy":"unique_immutable"`,
		`"attempt_key_policy":"unique_immutable","execution":`+execution,
		1,
	)
}

func loadArtifactConfiguration(t *testing.T, raw string) config.Configuration {
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
