package config

import (
	"encoding/json"
	"time"
)

// ArtifactExecutionConfiguration is absent unless the operator explicitly
// enables the complete finite artifact-byte execution policy.
type ArtifactExecutionConfiguration struct {
	Region                       string
	MaxObjectBytes               uint64
	MaxInFlight                  uint64
	RequestTimeout               time.Duration
	OrganizationReservedBytes    uint64
	WorkspaceReservedBytes       uint64
	OrganizationReservedAttempts uint64
	WorkspaceReservedAttempts    uint64
	configured                   bool
}

func (configuration ArtifactExecutionConfiguration) Configured() bool {
	return configuration.configured
}

var artifactExecutionFields = []string{
	"region",
	"max_object_bytes",
	"max_in_flight",
	"request_timeout",
	"organization_reserved_bytes",
	"workspace_reserved_bytes",
	"organization_reserved_attempts",
	"workspace_reserved_attempts",
}

func decodeArtifactExecution(raw json.RawMessage) (ArtifactExecutionConfiguration, error) {
	if len(raw) == 0 {
		return ArtifactExecutionConfiguration{}, nil
	}
	object, err := decodeObject(raw, "artifacts.execution", artifactExecutionFields, artifactExecutionFields)
	if err != nil {
		return ArtifactExecutionConfiguration{}, err
	}
	configuration := ArtifactExecutionConfiguration{configured: true}
	if configuration.Region, err = decodeString(object, "region", "artifacts.execution.region"); err != nil {
		return ArtifactExecutionConfiguration{}, err
	}
	for _, field := range []struct {
		name  string
		value *uint64
	}{
		{"max_object_bytes", &configuration.MaxObjectBytes},
		{"max_in_flight", &configuration.MaxInFlight},
		{"organization_reserved_bytes", &configuration.OrganizationReservedBytes},
		{"workspace_reserved_bytes", &configuration.WorkspaceReservedBytes},
		{"organization_reserved_attempts", &configuration.OrganizationReservedAttempts},
		{"workspace_reserved_attempts", &configuration.WorkspaceReservedAttempts},
	} {
		path := "artifacts.execution." + field.name
		if *field.value, err = decodeUint64(object, field.name, path); err != nil {
			return ArtifactExecutionConfiguration{}, err
		}
	}
	if configuration.RequestTimeout, err = decodeDuration(
		object,
		"request_timeout",
		"artifacts.execution.request_timeout",
	); err != nil {
		return ArtifactExecutionConfiguration{}, err
	}
	return configuration, nil
}

func validateArtifactExecution(configuration ArtifactExecutionConfiguration) error {
	if !configuration.configured {
		return nil
	}
	if !validArtifactRegion(configuration.Region) {
		return diagnostic("artifacts.execution.region", DiagnosticValue)
	}
	for _, field := range []struct {
		path  string
		value uint64
	}{
		{"artifacts.execution.max_object_bytes", configuration.MaxObjectBytes},
		{"artifacts.execution.max_in_flight", configuration.MaxInFlight},
		{"artifacts.execution.organization_reserved_bytes", configuration.OrganizationReservedBytes},
		{"artifacts.execution.workspace_reserved_bytes", configuration.WorkspaceReservedBytes},
		{"artifacts.execution.organization_reserved_attempts", configuration.OrganizationReservedAttempts},
		{"artifacts.execution.workspace_reserved_attempts", configuration.WorkspaceReservedAttempts},
	} {
		if field.value == 0 || field.value > maxConfigInteger {
			return diagnostic(field.path, DiagnosticBounds)
		}
	}
	// Whole-object verification reads one sentinel byte past the declared bound.
	if configuration.MaxObjectBytes == maxConfigInteger {
		return diagnostic("artifacts.execution.max_object_bytes", DiagnosticBounds)
	}
	if configuration.MaxInFlight > 1<<16 {
		return diagnostic("artifacts.execution.max_in_flight", DiagnosticBounds)
	}
	if configuration.RequestTimeout <= 0 || configuration.RequestTimeout > 5*time.Minute {
		return diagnostic("artifacts.execution.request_timeout", DiagnosticBounds)
	}
	if configuration.MaxObjectBytes > configuration.WorkspaceReservedBytes {
		return diagnostic("artifacts.execution.max_object_bytes", DiagnosticBounds)
	}
	if configuration.WorkspaceReservedBytes > configuration.OrganizationReservedBytes {
		return diagnostic("artifacts.execution.workspace_reserved_bytes", DiagnosticBounds)
	}
	if configuration.WorkspaceReservedAttempts > configuration.OrganizationReservedAttempts {
		return diagnostic("artifacts.execution.workspace_reserved_attempts", DiagnosticBounds)
	}
	if configuration.MaxObjectBytes > maxConfigInteger/configuration.MaxInFlight {
		return diagnostic("artifacts.execution.max_in_flight", DiagnosticBounds)
	}
	return nil
}

func validArtifactRegion(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' {
			continue
		}
		return false
	}
	return true
}
