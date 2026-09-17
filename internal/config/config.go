package config

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxDocumentBytes = 1 << 20
	maxJSONDepth     = 128
)

type DiagnosticKind string

const (
	DiagnosticSyntax    DiagnosticKind = "syntax"
	DiagnosticDuplicate DiagnosticKind = "duplicate"
	DiagnosticUnknown   DiagnosticKind = "unknown"
	DiagnosticType      DiagnosticKind = "type"
	DiagnosticRequired  DiagnosticKind = "required"
	DiagnosticValue     DiagnosticKind = "value"
	DiagnosticBounds    DiagnosticKind = "bounds"
	DiagnosticOwnership DiagnosticKind = "ownership"
)

type Diagnostic struct {
	Field string
	Kind  DiagnosticKind
}

func (d *Diagnostic) Error() string {
	field := d.Field
	if field == "" {
		field = "$"
	}
	return "configuration " + string(d.Kind) + " at " + field
}

type Ownership string

const (
	OwnershipBundled Ownership = "bundled"
	OwnershipManaged Ownership = "managed"
)

type Architecture string

const ArchitecturePostgresTemporalS3 Architecture = "postgresql-temporal-s3"

type AdmissionProfile string

const (
	AdmissionProfileDevelopment AdmissionProfile = "development"
	AdmissionProfileStandard    AdmissionProfile = "standard"
)

type Dependency string

const (
	DependencyPostgres  Dependency = "postgres"
	DependencyTemporal  Dependency = "temporal"
	DependencyArtifacts Dependency = "artifacts"
)

type LifecycleAction string

const (
	LifecycleInit   LifecycleAction = "init"
	LifecycleStop   LifecycleAction = "stop"
	LifecycleDelete LifecycleAction = "delete"
)

type LifecycleRequest struct {
	Dependency Dependency
	Action     LifecycleAction
}

type Prelaunch struct {
	Lifecycle []LifecycleRequest
}

type PostgresConfiguration struct {
	Ownership   Ownership
	Database    string
	EndpointRef string
}

type TemporalConfiguration struct {
	Ownership         Ownership
	Endpoint          string
	Namespace         string
	ClientIdentityRef string
	Retention         time.Duration
	TaskQueues        []string
}

type ArtifactsConfiguration struct {
	Ownership        Ownership
	Driver           string
	Endpoint         string
	Bucket           string
	CredentialRef    string
	AttemptKeyPolicy string
}

type IdentityConfiguration struct {
	Issuer           string
	ClientID         string
	ClientSecretRef  string
	StepUpMaximumAge time.Duration
}

type EncryptionConfiguration struct {
	WrappingKeyRef string
}

type AdmissionLimits struct {
	GlobalPending          uint64
	WorkspacePending       uint64
	WorkspaceMaxPendingAge time.Duration
}

type AdmissionOverrides struct {
	globalPending          optionalUint64
	workspacePending       optionalUint64
	workspaceMaxPendingAge optionalDuration
}

func (o AdmissionOverrides) GlobalPending() (uint64, bool) {
	return o.globalPending.value, o.globalPending.present
}

func (o AdmissionOverrides) WorkspacePending() (uint64, bool) {
	return o.workspacePending.value, o.workspacePending.present
}

func (o AdmissionOverrides) WorkspaceMaxPendingAge() (time.Duration, bool) {
	return o.workspaceMaxPendingAge.value, o.workspaceMaxPendingAge.present
}

type ResourceLimits struct {
	WorkspaceActiveRuns   uint64
	WorkflowPayloadBytes  uint64
	WorkflowFanout        uint64
	RunLifetime           time.Duration
	EventBuffer           uint64
	QueryChildren         uint64
	QueryChildMemoryBytes uint64
	QueryChildDeadline    time.Duration
	RequestBytes          uint64
	MetadataDeadline      time.Duration
}

type Document struct {
	data   documentData
	loaded bool
}

type Configuration struct {
	schemaVersion      string
	deploymentID       string
	publicOrigin       string
	architecture       Architecture
	postgres           PostgresConfiguration
	temporal           TemporalConfiguration
	artifacts          ArtifactsConfiguration
	identity           IdentityConfiguration
	encryption         EncryptionConfiguration
	admissionProfile   AdmissionProfile
	admissionOverrides AdmissionOverrides
	admission          AdmissionLimits
	limits             ResourceLimits
	server             ServerConfiguration
	worker             WorkerConfiguration
}

func (c Configuration) SchemaVersion() string {
	return c.schemaVersion
}

func (c Configuration) DeploymentID() string {
	return c.deploymentID
}

func (c Configuration) PublicOrigin() string {
	return c.publicOrigin
}

func (c Configuration) Architecture() Architecture {
	return c.architecture
}

func (c Configuration) Postgres() PostgresConfiguration {
	return c.postgres
}

func (c Configuration) Temporal() TemporalConfiguration {
	value := c.temporal
	value.TaskQueues = append([]string(nil), c.temporal.TaskQueues...)
	return value
}

func (c Configuration) Artifacts() ArtifactsConfiguration {
	return c.artifacts
}

func (c Configuration) Identity() IdentityConfiguration {
	return c.identity
}

func (c Configuration) Encryption() EncryptionConfiguration {
	return c.encryption
}

func (c Configuration) AdmissionProfile() AdmissionProfile {
	return c.admissionProfile
}

func (c Configuration) AdmissionOverrides() AdmissionOverrides {
	return c.admissionOverrides
}

func (c Configuration) Admission() AdmissionLimits {
	return c.admission
}

func (c Configuration) Limits() ResourceLimits {
	return c.limits
}

func (c Configuration) Worker() WorkerConfiguration {
	return c.worker
}

type documentData struct {
	schemaVersion      string
	deploymentID       string
	publicOrigin       string
	postgres           PostgresConfiguration
	temporal           TemporalConfiguration
	artifacts          ArtifactsConfiguration
	identity           IdentityConfiguration
	encryption         EncryptionConfiguration
	admissionProfile   AdmissionProfile
	admissionOverrides AdmissionOverrides
	limits             ResourceLimits
	server             ServerConfiguration
	worker             WorkerConfiguration
}

type optionalUint64 struct {
	value   uint64
	present bool
}

type optionalDuration struct {
	value   time.Duration
	present bool
}

func Load(reader io.Reader) (Document, error) {
	if reader == nil {
		return Document{}, diagnostic("$", DiagnosticRequired)
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maxDocumentBytes+1))
	if readErr != nil {
		return Document{}, diagnostic("$", DiagnosticSyntax)
	}
	if len(data) > maxDocumentBytes {
		return Document{}, diagnostic("$", DiagnosticBounds)
	}
	if !utf8.Valid(data) {
		return Document{}, diagnostic("$", DiagnosticSyntax)
	}
	if !validJSONStringEncoding(data) {
		return Document{}, diagnostic("$", DiagnosticSyntax)
	}
	if scanErr := scanStrictJSON(data); scanErr != nil {
		return Document{}, scanErr
	}
	root, decodeErr := decodeObject(data, "$", allowedRootFields, rootFields)
	if decodeErr != nil {
		return Document{}, decodeErr
	}

	var result documentData
	var err error
	if result.schemaVersion, err = decodeString(root, "schema_version", "schema_version"); err != nil {
		return Document{}, err
	}
	if result.deploymentID, err = decodeString(root, "deployment_id", "deployment_id"); err != nil {
		return Document{}, err
	}
	if result.publicOrigin, err = decodeString(root, "public_origin", "public_origin"); err != nil {
		return Document{}, err
	}
	if result.postgres, err = decodePostgres(root["postgres"]); err != nil {
		return Document{}, err
	}
	if result.temporal, err = decodeTemporal(root["temporal"]); err != nil {
		return Document{}, err
	}
	if result.artifacts, err = decodeArtifacts(root["artifacts"]); err != nil {
		return Document{}, err
	}
	if result.identity, err = decodeIdentity(root["identity"]); err != nil {
		return Document{}, err
	}
	if result.encryption, err = decodeEncryption(root["encryption"]); err != nil {
		return Document{}, err
	}
	if result.admissionProfile, result.admissionOverrides, err = decodeAdmission(root["admission"]); err != nil {
		return Document{}, err
	}
	if result.limits, err = decodeLimits(root["limits"]); err != nil {
		return Document{}, err
	}
	if result.server, err = decodeServer(root["server"]); err != nil {
		return Document{}, err
	}
	if result.worker, err = decodeWorker(root["worker"]); err != nil {
		return Document{}, err
	}
	return Document{data: result, loaded: true}, nil
}

var rootFields = []string{
	"schema_version",
	"deployment_id",
	"public_origin",
	"postgres",
	"temporal",
	"artifacts",
	"identity",
	"encryption",
	"admission",
	"limits",
}

var allowedRootFields = append([]string{"server", "worker"}, rootFields...)

func decodePostgres(raw json.RawMessage) (PostgresConfiguration, error) {
	fields := []string{"ownership", "database", "endpoint_ref"}
	object, err := decodeObject(raw, "postgres", fields, fields)
	if err != nil {
		return PostgresConfiguration{}, err
	}
	ownership, err := decodeString(object, "ownership", "postgres.ownership")
	if err != nil {
		return PostgresConfiguration{}, err
	}
	database, err := decodeString(object, "database", "postgres.database")
	if err != nil {
		return PostgresConfiguration{}, err
	}
	endpointRef, err := decodeString(object, "endpoint_ref", "postgres.endpoint_ref")
	if err != nil {
		return PostgresConfiguration{}, err
	}
	return PostgresConfiguration{Ownership: Ownership(ownership), Database: database, EndpointRef: endpointRef}, nil
}

func decodeTemporal(raw json.RawMessage) (TemporalConfiguration, error) {
	fields := []string{"ownership", "endpoint", "namespace", "client_identity_ref", "retention", "task_queues"}
	object, err := decodeObject(raw, "temporal", fields, fields)
	if err != nil {
		return TemporalConfiguration{}, err
	}
	ownership, err := decodeString(object, "ownership", "temporal.ownership")
	if err != nil {
		return TemporalConfiguration{}, err
	}
	endpoint, err := decodeString(object, "endpoint", "temporal.endpoint")
	if err != nil {
		return TemporalConfiguration{}, err
	}
	namespace, err := decodeString(object, "namespace", "temporal.namespace")
	if err != nil {
		return TemporalConfiguration{}, err
	}
	clientIdentityRef, err := decodeString(object, "client_identity_ref", "temporal.client_identity_ref")
	if err != nil {
		return TemporalConfiguration{}, err
	}
	retention, err := decodeDuration(object, "retention", "temporal.retention")
	if err != nil {
		return TemporalConfiguration{}, err
	}
	taskQueues, err := decodeStringArray(object, "task_queues", "temporal.task_queues")
	if err != nil {
		return TemporalConfiguration{}, err
	}
	return TemporalConfiguration{
		Ownership:         Ownership(ownership),
		Endpoint:          endpoint,
		Namespace:         namespace,
		ClientIdentityRef: clientIdentityRef,
		Retention:         retention,
		TaskQueues:        taskQueues,
	}, nil
}

func decodeArtifacts(raw json.RawMessage) (ArtifactsConfiguration, error) {
	fields := []string{"ownership", "driver", "endpoint", "bucket", "credential_ref", "attempt_key_policy"}
	object, err := decodeObject(raw, "artifacts", fields, fields)
	if err != nil {
		return ArtifactsConfiguration{}, err
	}
	ownership, err := decodeString(object, "ownership", "artifacts.ownership")
	if err != nil {
		return ArtifactsConfiguration{}, err
	}
	driver, err := decodeString(object, "driver", "artifacts.driver")
	if err != nil {
		return ArtifactsConfiguration{}, err
	}
	endpoint, err := decodeString(object, "endpoint", "artifacts.endpoint")
	if err != nil {
		return ArtifactsConfiguration{}, err
	}
	bucket, err := decodeString(object, "bucket", "artifacts.bucket")
	if err != nil {
		return ArtifactsConfiguration{}, err
	}
	credentialRef, err := decodeString(object, "credential_ref", "artifacts.credential_ref")
	if err != nil {
		return ArtifactsConfiguration{}, err
	}
	attemptKeyPolicy, err := decodeString(object, "attempt_key_policy", "artifacts.attempt_key_policy")
	if err != nil {
		return ArtifactsConfiguration{}, err
	}
	return ArtifactsConfiguration{
		Ownership:        Ownership(ownership),
		Driver:           driver,
		Endpoint:         endpoint,
		Bucket:           bucket,
		CredentialRef:    credentialRef,
		AttemptKeyPolicy: attemptKeyPolicy,
	}, nil
}

func decodeIdentity(raw json.RawMessage) (IdentityConfiguration, error) {
	fields := []string{"issuer", "client_id", "client_secret_ref", "step_up_maximum_age"}
	object, err := decodeObject(raw, "identity", fields, fields[:3])
	if err != nil {
		return IdentityConfiguration{}, err
	}
	issuer, err := decodeString(object, "issuer", "identity.issuer")
	if err != nil {
		return IdentityConfiguration{}, err
	}
	clientID, err := decodeString(object, "client_id", "identity.client_id")
	if err != nil {
		return IdentityConfiguration{}, err
	}
	clientSecretRef, err := decodeString(object, "client_secret_ref", "identity.client_secret_ref")
	if err != nil {
		return IdentityConfiguration{}, err
	}
	maximumAge := 5 * time.Minute
	if _, present := object["step_up_maximum_age"]; present {
		maximumAge, err = decodeDuration(object, "step_up_maximum_age", "identity.step_up_maximum_age")
		if err != nil {
			return IdentityConfiguration{}, err
		}
	}
	return IdentityConfiguration{
		Issuer: issuer, ClientID: clientID, ClientSecretRef: clientSecretRef, StepUpMaximumAge: maximumAge,
	}, nil
}

func decodeEncryption(raw json.RawMessage) (EncryptionConfiguration, error) {
	fields := []string{"wrapping_key_ref"}
	object, err := decodeObject(raw, "encryption", fields, fields)
	if err != nil {
		return EncryptionConfiguration{}, err
	}
	wrappingKeyRef, err := decodeString(object, "wrapping_key_ref", "encryption.wrapping_key_ref")
	if err != nil {
		return EncryptionConfiguration{}, err
	}
	return EncryptionConfiguration{WrappingKeyRef: wrappingKeyRef}, nil
}

func decodeAdmission(raw json.RawMessage) (AdmissionProfile, AdmissionOverrides, error) {
	allowed := []string{"profile", "overrides"}
	required := []string{"profile"}
	object, err := decodeObject(raw, "admission", allowed, required)
	if err != nil {
		return "", AdmissionOverrides{}, err
	}
	profile, err := decodeString(object, "profile", "admission.profile")
	if err != nil {
		return "", AdmissionOverrides{}, err
	}
	var overrides AdmissionOverrides
	if rawOverrides, present := object["overrides"]; present {
		overrideFields := []string{"global_pending", "workspace_pending", "workspace_max_pending_age"}
		overrideObject, objectErr := decodeObject(rawOverrides, "admission.overrides", overrideFields, nil)
		if objectErr != nil {
			return "", AdmissionOverrides{}, objectErr
		}
		if _, present := overrideObject["global_pending"]; present {
			value, valueErr := decodeUint64(overrideObject, "global_pending", "admission.overrides.global_pending")
			if valueErr != nil {
				return "", AdmissionOverrides{}, valueErr
			}
			overrides.globalPending = optionalUint64{value: value, present: true}
		}
		if _, present := overrideObject["workspace_pending"]; present {
			value, valueErr := decodeUint64(overrideObject, "workspace_pending", "admission.overrides.workspace_pending")
			if valueErr != nil {
				return "", AdmissionOverrides{}, valueErr
			}
			overrides.workspacePending = optionalUint64{value: value, present: true}
		}
		if _, present := overrideObject["workspace_max_pending_age"]; present {
			value, valueErr := decodeDuration(overrideObject, "workspace_max_pending_age", "admission.overrides.workspace_max_pending_age")
			if valueErr != nil {
				return "", AdmissionOverrides{}, valueErr
			}
			overrides.workspaceMaxPendingAge = optionalDuration{value: value, present: true}
		}
	}
	return AdmissionProfile(profile), overrides, nil
}

func decodeLimits(raw json.RawMessage) (ResourceLimits, error) {
	fields := []string{
		"workspace_active_runs",
		"workflow_payload_bytes",
		"workflow_fanout",
		"run_lifetime",
		"event_buffer",
		"query_children",
		"query_child_memory_bytes",
		"query_child_deadline",
		"request_bytes",
		"metadata_deadline",
	}
	object, err := decodeObject(raw, "limits", fields, fields)
	if err != nil {
		return ResourceLimits{}, err
	}
	var limits ResourceLimits
	if limits.WorkspaceActiveRuns, err = decodeUint64(object, "workspace_active_runs", "limits.workspace_active_runs"); err != nil {
		return ResourceLimits{}, err
	}
	if limits.WorkflowPayloadBytes, err = decodeUint64(object, "workflow_payload_bytes", "limits.workflow_payload_bytes"); err != nil {
		return ResourceLimits{}, err
	}
	if limits.WorkflowFanout, err = decodeUint64(object, "workflow_fanout", "limits.workflow_fanout"); err != nil {
		return ResourceLimits{}, err
	}
	if limits.RunLifetime, err = decodeDuration(object, "run_lifetime", "limits.run_lifetime"); err != nil {
		return ResourceLimits{}, err
	}
	if limits.EventBuffer, err = decodeUint64(object, "event_buffer", "limits.event_buffer"); err != nil {
		return ResourceLimits{}, err
	}
	if limits.QueryChildren, err = decodeUint64(object, "query_children", "limits.query_children"); err != nil {
		return ResourceLimits{}, err
	}
	if limits.QueryChildMemoryBytes, err = decodeUint64(object, "query_child_memory_bytes", "limits.query_child_memory_bytes"); err != nil {
		return ResourceLimits{}, err
	}
	if limits.QueryChildDeadline, err = decodeDuration(object, "query_child_deadline", "limits.query_child_deadline"); err != nil {
		return ResourceLimits{}, err
	}
	if limits.RequestBytes, err = decodeUint64(object, "request_bytes", "limits.request_bytes"); err != nil {
		return ResourceLimits{}, err
	}
	if limits.MetadataDeadline, err = decodeDuration(object, "metadata_deadline", "limits.metadata_deadline"); err != nil {
		return ResourceLimits{}, err
	}
	return limits, nil
}

func validJSONStringEncoding(data []byte) bool {
	for index := 0; index < len(data); index++ {
		if data[index] != '"' {
			continue
		}
		end, ok := validJSONString(data, index+1)
		if !ok {
			return false
		}
		index = end
	}
	return true
}

func validJSONString(data []byte, index int) (int, bool) {
	for index < len(data) {
		switch data[index] {
		case '"':
			return index, true
		case '\\':
			index++
			if index >= len(data) {
				return 0, false
			}
			if data[index] != 'u' {
				switch data[index] {
				case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
					index++
					continue
				default:
					return 0, false
				}
			}
			code, ok := decodeHexQuad(data, index+1)
			if !ok {
				return 0, false
			}
			index += 5
			switch {
			case code >= 0xd800 && code <= 0xdbff:
				if index+6 > len(data) || data[index] != '\\' || data[index+1] != 'u' {
					return 0, false
				}
				low, lowOK := decodeHexQuad(data, index+2)
				if !lowOK || low < 0xdc00 || low > 0xdfff {
					return 0, false
				}
				index += 6
			case code >= 0xdc00 && code <= 0xdfff:
				return 0, false
			}
			continue
		default:
			if data[index] < 0x20 {
				return 0, false
			}
			index++
		}
	}
	return 0, false
}

func decodeHexQuad(data []byte, index int) (uint16, bool) {
	if index+4 > len(data) {
		return 0, false
	}
	var value uint16
	for _, character := range data[index : index+4] {
		value <<= 4
		switch {
		case character >= '0' && character <= '9':
			value |= uint16(character - '0')
		case character >= 'a' && character <= 'f':
			value |= uint16(character-'a') + 10
		case character >= 'A' && character <= 'F':
			value |= uint16(character-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func scanStrictJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder, "", 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return diagnostic("$", DiagnosticSyntax)
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder, field string, depth int) error {
	if depth > maxJSONDepth {
		return diagnostic("$", DiagnosticBounds)
	}
	token, err := decoder.Token()
	if err != nil {
		return diagnostic("$", DiagnosticSyntax)
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, keyErr := decoder.Token()
			if keyErr != nil {
				return diagnostic("$", DiagnosticSyntax)
			}
			key, ok := keyToken.(string)
			if !ok {
				return diagnostic("$", DiagnosticSyntax)
			}
			childField := joinField(field, key)
			if _, exists := seen[key]; exists {
				return diagnostic(childField, DiagnosticDuplicate)
			}
			seen[key] = struct{}{}
			if valueErr := consumeJSONValue(decoder, childField, depth+1); valueErr != nil {
				return valueErr
			}
		}
		closing, closeErr := decoder.Token()
		if closeErr != nil || closing != json.Delim('}') {
			return diagnostic("$", DiagnosticSyntax)
		}
	case '[':
		index := 0
		for decoder.More() {
			childField := field + "[" + strconv.Itoa(index) + "]"
			if valueErr := consumeJSONValue(decoder, childField, depth+1); valueErr != nil {
				return valueErr
			}
			index++
		}
		closing, closeErr := decoder.Token()
		if closeErr != nil || closing != json.Delim(']') {
			return diagnostic("$", DiagnosticSyntax)
		}
	default:
		return diagnostic("$", DiagnosticSyntax)
	}
	return nil
}

func decodeObject(raw []byte, field string, allowed, required []string) (map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, diagnostic(field, DiagnosticType)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil {
		return nil, diagnostic(field, DiagnosticSyntax)
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = struct{}{}
	}
	unknown := make([]string, 0)
	for name := range object {
		if _, ok := allowedSet[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) != 0 {
		sort.Strings(unknown)
		return nil, diagnostic(joinField(normalizeRoot(field), unknown[0]), DiagnosticUnknown)
	}
	for _, name := range required {
		if _, ok := object[name]; !ok {
			return nil, diagnostic(joinField(normalizeRoot(field), name), DiagnosticRequired)
		}
	}
	return object, nil
}

func decodeString(object map[string]json.RawMessage, name, field string) (string, error) {
	raw := bytes.TrimSpace(object[name])
	if len(raw) == 0 || raw[0] != '"' {
		return "", diagnostic(field, DiagnosticType)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", diagnostic(field, DiagnosticType)
	}
	return value, nil
}

func decodeUint64(object map[string]json.RawMessage, name, field string) (uint64, error) {
	raw := strings.TrimSpace(string(object[name]))
	if raw == "" || strings.ContainsAny(raw, ".eE") {
		return 0, diagnostic(field, DiagnosticType)
	}
	if strings.HasPrefix(raw, "-") {
		return 0, diagnostic(field, DiagnosticValue)
	}
	for _, character := range raw {
		if character < '0' || character > '9' {
			return 0, diagnostic(field, DiagnosticType)
		}
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, diagnostic(field, DiagnosticBounds)
	}
	return value, nil
}

func decodeDuration(object map[string]json.RawMessage, name, field string) (time.Duration, error) {
	value, err := decodeString(object, name, field)
	if err != nil {
		return 0, err
	}
	if len(value) > 64 || strings.HasPrefix(value, "+") {
		return 0, diagnostic(field, DiagnosticValue)
	}
	duration, parseErr := time.ParseDuration(value)
	if parseErr != nil {
		return 0, diagnostic(field, DiagnosticValue)
	}
	return duration, nil
}

func decodeStringArray(object map[string]json.RawMessage, name, field string) ([]string, error) {
	raw := bytes.TrimSpace(object[name])
	if len(raw) == 0 || raw[0] != '[' {
		return nil, diagnostic(field, DiagnosticType)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, diagnostic(field, DiagnosticType)
	}
	values := make([]string, len(entries))
	for index, entry := range entries {
		entryField := field + "[" + strconv.Itoa(index) + "]"
		trimmed := bytes.TrimSpace(entry)
		if len(trimmed) == 0 || trimmed[0] != '"' {
			return nil, diagnostic(entryField, DiagnosticType)
		}
		if err := json.Unmarshal(trimmed, &values[index]); err != nil {
			return nil, diagnostic(entryField, DiagnosticType)
		}
	}
	return values, nil
}

func diagnostic(field string, kind DiagnosticKind) *Diagnostic {
	if field == "" {
		field = "$"
	}
	return &Diagnostic{Field: field, Kind: kind}
}

func joinField(prefix, name string) string {
	if prefix == "" || prefix == "$" {
		return name
	}
	return prefix + "." + name
}

func normalizeRoot(field string) string {
	if field == "$" {
		return ""
	}
	return field
}
