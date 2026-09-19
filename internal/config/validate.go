package config

import (
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const maxConfigInteger uint64 = 1<<63 - 1

const (
	// maxConnectorBundles bounds how many generated execution bundles one
	// deployment may declare, so startup work stays finite and inspectable.
	maxConnectorBundles = 32
	// maxConnectorBundlePathBytes bounds one declared bundle path. Operators
	// declare absolute, canonical paths; the configuration never searched or
	// resolved a relative or unclean path.
	maxConnectorBundlePathBytes = 4096
)

func Validate(document Document, prelaunch Prelaunch) (Configuration, error) {
	if !document.loaded {
		return Configuration{}, diagnostic("$", DiagnosticRequired)
	}
	data := document.data
	if data.schemaVersion != "1" {
		return Configuration{}, diagnostic("schema_version", DiagnosticValue)
	}
	if err := validateIdentifier(data.deploymentID, "deployment_id", 128); err != nil {
		return Configuration{}, err
	}
	if err := validateHTTPSURL(data.publicOrigin, "public_origin", true); err != nil {
		return Configuration{}, err
	}
	if err := validateOwnership(data.postgres.Ownership, "postgres.ownership"); err != nil {
		return Configuration{}, err
	}
	if data.postgres.Database != "pm_app" {
		return Configuration{}, diagnostic("postgres.database", DiagnosticValue)
	}
	if err := validateSecretReference(data.postgres.EndpointRef, "postgres.endpoint_ref"); err != nil {
		return Configuration{}, err
	}
	if err := validateOwnership(data.temporal.Ownership, "temporal.ownership"); err != nil {
		return Configuration{}, err
	}
	if err := validateHostPort(data.temporal.Endpoint, "temporal.endpoint"); err != nil {
		return Configuration{}, err
	}
	if err := validateIdentifier(data.temporal.Namespace, "temporal.namespace", 255); err != nil {
		return Configuration{}, err
	}
	if err := validateSecretReference(data.temporal.ClientIdentityRef, "temporal.client_identity_ref"); err != nil {
		return Configuration{}, err
	}
	if data.temporal.Retention <= 0 {
		return Configuration{}, diagnostic("temporal.retention", DiagnosticValue)
	}
	if err := validateTaskQueues(data.temporal.TaskQueues); err != nil {
		return Configuration{}, err
	}
	if err := validateOwnership(data.artifacts.Ownership, "artifacts.ownership"); err != nil {
		return Configuration{}, err
	}
	if data.artifacts.Driver != "s3" {
		return Configuration{}, diagnostic("artifacts.driver", DiagnosticValue)
	}
	if err := validateHTTPSURL(data.artifacts.Endpoint, "artifacts.endpoint", false); err != nil {
		return Configuration{}, err
	}
	if err := validateBucket(data.artifacts.Bucket); err != nil {
		return Configuration{}, err
	}
	if err := validateSecretReference(data.artifacts.CredentialRef, "artifacts.credential_ref"); err != nil {
		return Configuration{}, err
	}
	if data.artifacts.AttemptKeyPolicy != "unique_immutable" {
		return Configuration{}, diagnostic("artifacts.attempt_key_policy", DiagnosticValue)
	}
	if err := validateArtifactExecution(data.artifacts.Execution); err != nil {
		return Configuration{}, err
	}
	if err := validateHTTPSURL(data.identity.Issuer, "identity.issuer", false); err != nil {
		return Configuration{}, err
	}
	if err := validateClientID(data.identity.ClientID); err != nil {
		return Configuration{}, err
	}
	if err := validateSecretReference(data.identity.ClientSecretRef, "identity.client_secret_ref"); err != nil {
		return Configuration{}, err
	}
	if data.identity.StepUpMaximumAge < time.Second || data.identity.StepUpMaximumAge > 15*time.Minute {
		return Configuration{}, diagnostic("identity.step_up_maximum_age", DiagnosticBounds)
	}
	if err := validateSecretReference(data.encryption.WrappingKeyRef, "encryption.wrapping_key_ref"); err != nil {
		return Configuration{}, err
	}

	admission, err := resolveAdmission(data.admissionProfile, data.admissionOverrides)
	if err != nil {
		return Configuration{}, err
	}
	if err := validateResourceLimits(data.limits); err != nil {
		return Configuration{}, err
	}
	if err := validateServer(data.server); err != nil {
		return Configuration{}, err
	}
	if err := validateWorker(data.worker, data.temporal, data.limits); err != nil {
		return Configuration{}, err
	}
	providerHTTP, err := resolveProviderHTTP(data.providerHTTP)
	if err != nil {
		return Configuration{}, err
	}
	providerSQL, err := resolveProviderSQL(data.providerSQL)
	if err != nil {
		return Configuration{}, err
	}
	connectors, err := resolveConnectors(data.connectors)
	if err != nil {
		return Configuration{}, err
	}
	if err := validateLifecycle(data, prelaunch); err != nil {
		return Configuration{}, err
	}

	temporal := data.temporal
	temporal.TaskQueues = append([]string(nil), data.temporal.TaskQueues...)
	server := data.server
	server.TrustedProxies = append([]string(nil), data.server.TrustedProxies...)
	return Configuration{
		schemaVersion:      data.schemaVersion,
		deploymentID:       data.deploymentID,
		publicOrigin:       data.publicOrigin,
		architecture:       ArchitecturePostgresTemporalS3,
		postgres:           data.postgres,
		temporal:           temporal,
		artifacts:          data.artifacts,
		identity:           data.identity,
		encryption:         data.encryption,
		admissionProfile:   data.admissionProfile,
		admissionOverrides: data.admissionOverrides,
		admission:          admission,
		limits:             data.limits,
		server:             server,
		worker:             data.worker,
		providerHTTP:       providerHTTP,
		providerSQL:        providerSQL,
		connectors:         connectors,
	}, nil
}

func resolveAdmission(profile AdmissionProfile, overrides AdmissionOverrides) (AdmissionLimits, error) {
	var resolved AdmissionLimits
	switch profile {
	case AdmissionProfileDevelopment:
		resolved = AdmissionLimits{
			GlobalPending:          32,
			WorkspacePending:       8,
			WorkspaceMaxPendingAge: 2 * time.Minute,
		}
	case AdmissionProfileStandard:
		resolved = AdmissionLimits{
			GlobalPending:          256,
			WorkspacePending:       32,
			WorkspaceMaxPendingAge: 10 * time.Minute,
		}
	default:
		return AdmissionLimits{}, diagnostic("admission.profile", DiagnosticValue)
	}
	if value, present := overrides.GlobalPending(); present {
		if value == 0 {
			return AdmissionLimits{}, diagnostic("admission.overrides.global_pending", DiagnosticValue)
		}
		if value > maxConfigInteger {
			return AdmissionLimits{}, diagnostic("admission.overrides.global_pending", DiagnosticBounds)
		}
		resolved.GlobalPending = value
	}
	if value, present := overrides.WorkspacePending(); present {
		if value == 0 {
			return AdmissionLimits{}, diagnostic("admission.overrides.workspace_pending", DiagnosticValue)
		}
		if value > maxConfigInteger {
			return AdmissionLimits{}, diagnostic("admission.overrides.workspace_pending", DiagnosticBounds)
		}
		resolved.WorkspacePending = value
	}
	if value, present := overrides.WorkspaceMaxPendingAge(); present {
		if value <= 0 {
			return AdmissionLimits{}, diagnostic("admission.overrides.workspace_max_pending_age", DiagnosticValue)
		}
		resolved.WorkspaceMaxPendingAge = value
	}
	if resolved.WorkspacePending > resolved.GlobalPending {
		return AdmissionLimits{}, diagnostic("admission.overrides.workspace_pending", DiagnosticValue)
	}
	return resolved, nil
}

func validateLifecycle(data documentData, prelaunch Prelaunch) error {
	for index, request := range prelaunch.Lifecycle {
		requestField := "prelaunch.lifecycle[" + strconv.Itoa(index) + "]"
		var ownership Ownership
		var ownershipField string
		switch request.Dependency {
		case DependencyPostgres:
			ownership = data.postgres.Ownership
			ownershipField = "postgres.ownership"
		case DependencyTemporal:
			ownership = data.temporal.Ownership
			ownershipField = "temporal.ownership"
		case DependencyArtifacts:
			ownership = data.artifacts.Ownership
			ownershipField = "artifacts.ownership"
		default:
			return diagnostic(requestField+".dependency", DiagnosticValue)
		}
		switch request.Action {
		case LifecycleInit, LifecycleStop, LifecycleDelete:
		default:
			return diagnostic(requestField+".action", DiagnosticValue)
		}
		if ownership != OwnershipBundled {
			return diagnostic(ownershipField, DiagnosticOwnership)
		}
	}
	return nil
}

func validateOwnership(value Ownership, field string) error {
	switch value {
	case OwnershipBundled, OwnershipManaged:
		return nil
	default:
		return diagnostic(field, DiagnosticValue)
	}
}

func validateSecretReference(value, field string) error {
	if len(value) < 10 || len(value) > 2048 || strings.ContainsAny(value, "?#") {
		return diagnostic(field, DiagnosticValue)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "secret" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return diagnostic(field, DiagnosticValue)
	}
	if strings.Trim(parsed.EscapedPath(), "/") == "" {
		return diagnostic(field, DiagnosticValue)
	}
	return nil
}

func validateHTTPSURL(value, field string, originOnly bool) error {
	if len(value) < 9 || len(value) > 2048 || strings.ContainsAny(value, "?#") {
		return diagnostic(field, DiagnosticValue)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return diagnostic(field, DiagnosticValue)
	}
	if portText := parsed.Port(); portText != "" {
		port, portErr := strconv.ParseUint(portText, 10, 16)
		if portErr != nil || port == 0 {
			return diagnostic(field, DiagnosticValue)
		}
	}
	if originOnly && parsed.EscapedPath() != "" && parsed.EscapedPath() != "/" {
		return diagnostic(field, DiagnosticValue)
	}
	return nil
}

func validateHostPort(value, field string) error {
	if len(value) < 3 || len(value) > 512 {
		return diagnostic(field, DiagnosticValue)
	}
	host, portText, err := net.SplitHostPort(value)
	if err != nil || host == "" {
		return diagnostic(field, DiagnosticValue)
	}
	if net.ParseIP(host) == nil {
		for _, character := range host {
			allowed := character >= 'a' && character <= 'z' ||
				character >= 'A' && character <= 'Z' ||
				character >= '0' && character <= '9' ||
				character == '-' || character == '.'
			if !allowed {
				return diagnostic(field, DiagnosticValue)
			}
		}
	}
	if portText == "" || portText[0] == '0' {
		return diagnostic(field, DiagnosticValue)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return diagnostic(field, DiagnosticValue)
	}
	return nil
}

func validateIdentifier(value, field string, maximum int) error {
	if value == "" || len(value) > maximum {
		return diagnostic(field, DiagnosticValue)
	}
	for index, character := range value {
		allowed := character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '-' || character == '_' || character == '.'
		if !allowed || index == 0 && (character == '-' || character == '_' || character == '.') {
			return diagnostic(field, DiagnosticValue)
		}
	}
	return nil
}

func validateClientID(value string) error {
	if value == "" {
		return diagnostic("identity.client_id", DiagnosticValue)
	}
	// RFC 6749 Appendix A.1 permits VSCHAR, not a PM identifier grammar.
	// Keep the issued value exact; Load already bounds the whole document.
	for _, character := range value {
		if character < 0x20 || character > 0x7e {
			return diagnostic("identity.client_id", DiagnosticValue)
		}
	}
	return nil
}

func validateTaskQueues(values []string) error {
	if len(values) == 0 || len(values) > 32 {
		return diagnostic("temporal.task_queues", DiagnosticBounds)
	}
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		field := "temporal.task_queues[" + strconv.Itoa(index) + "]"
		if err := validateIdentifier(value, field, 255); err != nil {
			return err
		}
		if _, exists := seen[value]; exists {
			return diagnostic(field, DiagnosticDuplicate)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func validateBucket(value string) error {
	if value == "" || len(value) > 255 {
		return diagnostic("artifacts.bucket", DiagnosticValue)
	}
	for _, character := range value {
		allowed := character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' ||
			character == '-' || character == '.'
		if !allowed {
			return diagnostic("artifacts.bucket", DiagnosticValue)
		}
	}
	return nil
}

func validateResourceLimits(limits ResourceLimits) error {
	integerLimits := []struct {
		field string
		value uint64
	}{
		{field: "limits.workspace_active_runs", value: limits.WorkspaceActiveRuns},
		{field: "limits.workflow_payload_bytes", value: limits.WorkflowPayloadBytes},
		{field: "limits.workflow_fanout", value: limits.WorkflowFanout},
		{field: "limits.event_buffer", value: limits.EventBuffer},
		{field: "limits.query_children", value: limits.QueryChildren},
		{field: "limits.query_child_memory_bytes", value: limits.QueryChildMemoryBytes},
		{field: "limits.request_bytes", value: limits.RequestBytes},
	}
	for _, limit := range integerLimits {
		if limit.value == 0 {
			return diagnostic(limit.field, DiagnosticValue)
		}
		if limit.value > maxConfigInteger {
			return diagnostic(limit.field, DiagnosticBounds)
		}
	}
	if limits.RunLifetime <= 0 {
		return diagnostic("limits.run_lifetime", DiagnosticValue)
	}
	if limits.QueryChildDeadline <= 0 {
		return diagnostic("limits.query_child_deadline", DiagnosticValue)
	}
	if limits.MetadataDeadline <= 0 {
		return diagnostic("limits.metadata_deadline", DiagnosticValue)
	}
	return nil
}

// resolveConnectors validates declared generated execution bundle paths. It
// performs no filesystem access: an unreadable or undecodable declared bundle
// fails process composition, never configuration parsing.
func resolveConnectors(configuration ConnectorsConfiguration) (ConnectorsConfiguration, error) {
	if len(configuration.bundlePaths) == 0 {
		return ConnectorsConfiguration{}, nil
	}
	if len(configuration.bundlePaths) > maxConnectorBundles {
		return ConnectorsConfiguration{}, diagnostic("connectors.bundles", DiagnosticBounds)
	}
	paths := make([]string, 0, len(configuration.bundlePaths))
	seen := make(map[string]struct{}, len(configuration.bundlePaths))
	for index, path := range configuration.bundlePaths {
		field := "connectors.bundles[" + strconv.Itoa(index) + "]"
		if err := validateConnectorBundlePath(path, field); err != nil {
			return ConnectorsConfiguration{}, err
		}
		if _, duplicate := seen[path]; duplicate {
			return ConnectorsConfiguration{}, diagnostic(field, DiagnosticDuplicate)
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return ConnectorsConfiguration{bundlePaths: paths}, nil
}

func validateConnectorBundlePath(value, field string) error {
	if value == "" || len(value) > maxConnectorBundlePathBytes ||
		value != strings.TrimSpace(value) || !utf8.ValidString(value) ||
		!filepath.IsAbs(value) || filepath.Clean(value) != value {
		return diagnostic(field, DiagnosticValue)
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return diagnostic(field, DiagnosticValue)
		}
	}
	return nil
}
