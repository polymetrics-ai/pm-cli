package contract

import (
	"encoding/json"
	"time"
	"unicode/utf8"
)

const ProofSchemaVersion = "pm.proof/v1"

type ProofStatus string

const (
	ProofUnproven   ProofStatus = "unproven"
	ProofBlocked    ProofStatus = "blocked"
	ProofFailed     ProofStatus = "failed"
	ProofPassedFake ProofStatus = "passed_fake"
	ProofPassedLive ProofStatus = "passed_live"
)

type ProofReceipt struct {
	ProofVersion              string                      `json:"proof_version"`
	ConnectorID               string                      `json:"connector_id"`
	Lane                      Lane                        `json:"lane"`
	OperationID               string                      `json:"operation_id"`
	BehaviorID                string                      `json:"behavior_id"`
	CaseID                    string                      `json:"case_id"`
	RunID                     *string                     `json:"run_id"`
	SourceHashes              []SourceHash                `json:"source_hashes"`
	BundleHash                *SourceHash                 `json:"bundle_hash"`
	SchemaHash                *SourceHash                 `json:"schema_hash"`
	AssertionHash             SemanticDigest              `json:"assertion_hash"`
	PMClientHash              *SourceHash                 `json:"pm_client_hash"`
	PMAPIHash                 *SourceHash                 `json:"pm_api_hash"`
	PMWorkerHash              *SourceHash                 `json:"pm_worker_hash"`
	QueryHelperHashIfUsed     *SourceHash                 `json:"query_helper_hash_if_used"`
	HandlerVersions           map[string]string           `json:"handler_versions"`
	WorkspaceAccountScopeHash *SemanticDigest             `json:"workspace_account_scope_hash"`
	Command                   []string                    `json:"command"`
	Request                   *ProofRequestObservation    `json:"request"`
	Response                  *ProofResponseObservation   `json:"response"`
	Assertions                []ProofAssertionObservation `json:"assertions"`
	Effect                    ProofEffect                 `json:"effect"`
	Status                    ProofStatus                 `json:"status"`
	StartedAt                 *time.Time                  `json:"started_at"`
	FinishedAt                *time.Time                  `json:"finished_at"`
	Provenance                ProofProvenance             `json:"provenance"`
	ImplementationClosure     *ImplementationClosure      `json:"implementation_closure,omitempty"`
	CleanupDiagnostics        []ProofCleanupDiagnostic    `json:"cleanup_diagnostics,omitempty"`
}

type ProofRequestObservation struct {
	Method          string                     `json:"method"`
	Target          string                     `json:"target"`
	MediaType       string                     `json:"media_type"`
	BodySHA256      *SourceHash                `json:"body_sha256,omitempty"`
	SanitizedFields map[string]json.RawMessage `json:"sanitized_fields"`
	Redactions      []ProofRedaction           `json:"redactions"`
}

type ProofResponseObservation struct {
	Status          int                        `json:"status"`
	MediaType       string                     `json:"media_type"`
	BodySHA256      *SourceHash                `json:"body_sha256,omitempty"`
	SanitizedFields map[string]json.RawMessage `json:"sanitized_fields"`
	Redactions      []ProofRedaction           `json:"redactions"`
}

type ProofRedaction struct {
	Pointer     string `json:"pointer"`
	Replacement string `json:"replacement"`
}

type ProofAssertionObservation struct {
	AssertionID string          `json:"assertion_id"`
	Expected    json.RawMessage `json:"expected"`
	Observed    json.RawMessage `json:"observed"`
	Passed      *bool           `json:"passed"`
}

type ProofEffect struct {
	State           OutcomeState `json:"state"`
	ReceiptRefs     []string     `json:"receipt_refs"`
	OwnedObjectRefs []string     `json:"owned_object_refs"`
}

type ProofProvenance struct {
	ExecutionKind string  `json:"execution_kind"`
	HostClass     *string `json:"host_class"`
	OriginalRunID *string `json:"original_run_id"`
	Reused        *bool   `json:"reused"`
}

type ImplementationClosure struct {
	Known  bool            `json:"known"`
	Digest *SemanticDigest `json:"digest"`
}

type ProofCleanupDiagnostic struct {
	StepID string `json:"step_id"`
	Code   string `json:"code"`
}

func (receipt *ProofReceipt) UnmarshalJSON(data []byte) error {
	type wire ProofReceipt
	var decoded wire
	fields, err := decodeStrictNullableObject(data, &decoded,
		"proof_version", "connector_id", "lane", "operation_id", "behavior_id", "case_id",
		"run_id", "source_hashes", "bundle_hash", "schema_hash", "assertion_hash",
		"pm_client_hash", "pm_api_hash", "pm_worker_hash", "query_helper_hash_if_used",
		"handler_versions", "workspace_account_scope_hash", "command", "request", "response",
		"assertions", "effect", "status", "started_at", "finished_at", "provenance",
	)
	if err != nil {
		return err
	}
	if err := rejectExplicitNullFields(fields,
		"proof_version", "connector_id", "lane", "operation_id", "behavior_id", "case_id",
		"source_hashes", "assertion_hash", "command", "assertions", "effect", "status", "provenance",
		"implementation_closure", "cleanup_diagnostics",
	); err != nil {
		return err
	}
	value := ProofReceipt(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*receipt = value
	return nil
}

func (observation *ProofRequestObservation) UnmarshalJSON(data []byte) error {
	type wire ProofRequestObservation
	var decoded wire
	fields, err := decodeStrictNullableObject(data, &decoded,
		"method", "target", "media_type", "sanitized_fields", "redactions",
	)
	if err != nil {
		return err
	}
	if err := rejectExplicitNullFields(fields,
		"method", "target", "media_type", "body_sha256", "sanitized_fields", "redactions",
	); err != nil {
		return err
	}
	value := ProofRequestObservation(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*observation = value
	return nil
}

func (observation *ProofResponseObservation) UnmarshalJSON(data []byte) error {
	type wire ProofResponseObservation
	var decoded wire
	fields, err := decodeStrictNullableObject(data, &decoded,
		"status", "media_type", "sanitized_fields", "redactions",
	)
	if err != nil {
		return err
	}
	if err := rejectExplicitNullFields(fields,
		"status", "media_type", "body_sha256", "sanitized_fields", "redactions",
	); err != nil {
		return err
	}
	value := ProofResponseObservation(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*observation = value
	return nil
}

func (redaction *ProofRedaction) UnmarshalJSON(data []byte) error {
	type wire ProofRedaction
	var decoded wire
	fields, err := decodeStrictNullableObject(data, &decoded, "pointer", "replacement")
	if err != nil {
		return err
	}
	if err := rejectExplicitNullFields(fields, "pointer", "replacement"); err != nil {
		return err
	}
	value := ProofRedaction(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*redaction = value
	return nil
}

func (observation *ProofAssertionObservation) UnmarshalJSON(data []byte) error {
	type wire ProofAssertionObservation
	var decoded wire
	fields, err := decodeStrictNullableObject(data, &decoded, "assertion_id", "expected", "observed", "passed")
	if err != nil {
		return err
	}
	if err := rejectExplicitNullFields(fields, "assertion_id"); err != nil {
		return err
	}
	value := ProofAssertionObservation(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*observation = value
	return nil
}

func (effect *ProofEffect) UnmarshalJSON(data []byte) error {
	type wire ProofEffect
	var decoded wire
	fields, err := decodeStrictNullableObject(data, &decoded, "state", "receipt_refs", "owned_object_refs")
	if err != nil {
		return err
	}
	if err := rejectExplicitNullFields(fields, "state", "receipt_refs", "owned_object_refs"); err != nil {
		return err
	}
	value := ProofEffect(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*effect = value
	return nil
}

func (provenance *ProofProvenance) UnmarshalJSON(data []byte) error {
	type wire ProofProvenance
	var decoded wire
	fields, err := decodeStrictNullableObject(data, &decoded, "execution_kind", "host_class", "original_run_id", "reused")
	if err != nil {
		return err
	}
	if err := rejectExplicitNullFields(fields, "execution_kind"); err != nil {
		return err
	}
	value := ProofProvenance(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*provenance = value
	return nil
}

func (closure *ImplementationClosure) UnmarshalJSON(data []byte) error {
	type wire ImplementationClosure
	var decoded wire
	fields, err := decodeStrictNullableObject(data, &decoded, "known", "digest")
	if err != nil {
		return err
	}
	if err := rejectExplicitNullFields(fields, "known"); err != nil {
		return err
	}
	value := ImplementationClosure(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*closure = value
	return nil
}

func (diagnostic *ProofCleanupDiagnostic) UnmarshalJSON(data []byte) error {
	type wire ProofCleanupDiagnostic
	var decoded wire
	fields, err := decodeStrictNullableObject(data, &decoded, "step_id", "code")
	if err != nil {
		return err
	}
	if err := rejectExplicitNullFields(fields, "step_id", "code"); err != nil {
		return err
	}
	value := ProofCleanupDiagnostic(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*diagnostic = value
	return nil
}

func (receipt ProofReceipt) Validate() error {
	if receipt.ProofVersion != ProofSchemaVersion {
		return contractError(ErrInvalidContract, "proof.proof_version", "proof contract version is unsupported")
	}
	identities := [...]struct {
		path  string
		value string
	}{
		{path: "proof.connector_id", value: receipt.ConnectorID},
		{path: "proof.operation_id", value: receipt.OperationID},
		{path: "proof.behavior_id", value: receipt.BehaviorID},
		{path: "proof.case_id", value: receipt.CaseID},
	}
	for _, identity := range identities {
		if !validBoundedIdentityPart(identity.value, 1024) {
			return contractError(ErrInvalidIdentity, identity.path, "must preserve one bounded proof identity")
		}
	}
	if err := ValidateLane(receipt.Lane); err != nil {
		return err
	}
	if receipt.RunID != nil && !validBoundedIdentityPart(*receipt.RunID, 1024) {
		return contractError(ErrInvalidIdentity, "proof.run_id", "must bind one bounded run identity")
	}
	if receipt.SourceHashes == nil || len(receipt.SourceHashes) == 0 {
		return contractError(ErrInvalidContract, "proof.source_hashes", "must bind at least one exact source hash")
	}
	seenHashes := make(map[SourceHash]struct{}, len(receipt.SourceHashes))
	for _, hash := range receipt.SourceHashes {
		if err := hash.Validate(); err != nil {
			return err
		}
		if _, duplicate := seenHashes[hash]; duplicate {
			return contractError(ErrDuplicateField, "proof.source_hashes", "source hash is duplicated")
		}
		seenHashes[hash] = struct{}{}
	}
	hashes := [...]struct {
		path string
		hash *SourceHash
	}{
		{path: "proof.bundle_hash", hash: receipt.BundleHash},
		{path: "proof.schema_hash", hash: receipt.SchemaHash},
		{path: "proof.pm_client_hash", hash: receipt.PMClientHash},
		{path: "proof.pm_api_hash", hash: receipt.PMAPIHash},
		{path: "proof.pm_worker_hash", hash: receipt.PMWorkerHash},
		{path: "proof.query_helper_hash_if_used", hash: receipt.QueryHelperHashIfUsed},
	}
	for _, field := range hashes {
		if field.hash != nil {
			if err := field.hash.Validate(); err != nil {
				return contractError(ErrInvalidIdentity, field.path, "must be an exact sha256 source hash")
			}
		}
	}
	if err := receipt.AssertionHash.Validate(); err != nil {
		return err
	}
	if receipt.HandlerVersions != nil {
		for handler, version := range receipt.HandlerVersions {
			if !validBoundedIdentityPart(handler, 1024) || !validBoundedIdentityPart(version, 1024) {
				return contractError(ErrInvalidIdentity, "proof.handler_versions", "contains an invalid bounded handler identity or version")
			}
		}
	}
	if receipt.WorkspaceAccountScopeHash != nil {
		if err := receipt.WorkspaceAccountScopeHash.Validate(); err != nil {
			return err
		}
	}
	if receipt.Command == nil || len(receipt.Command) == 0 {
		return contractError(ErrInvalidContract, "proof.command", "must preserve the invoked command")
	}
	commandBytes := 0
	for _, argument := range receipt.Command {
		if !utf8.ValidString(argument) || utf8.RuneCountInString(argument) > 4096 {
			return contractError(ErrInvalidContract, "proof.command", "contains an invalid or oversized argument")
		}
		commandBytes += len(argument)
		if commandBytes > MaxContractDocumentBytes {
			return contractError(ErrInvalidContract, "proof.command", "command exceeds the contract byte limit")
		}
	}
	if receipt.Request != nil {
		if err := receipt.Request.Validate(); err != nil {
			return err
		}
	}
	if receipt.Response != nil {
		if err := receipt.Response.Validate(); err != nil {
			return err
		}
	}
	if receipt.Assertions == nil || len(receipt.Assertions) == 0 {
		return contractError(ErrInvalidContract, "proof.assertions", "must preserve at least one independent assertion")
	}
	seenAssertions := make(map[string]struct{}, len(receipt.Assertions))
	for _, assertion := range receipt.Assertions {
		if err := assertion.Validate(); err != nil {
			return err
		}
		if _, duplicate := seenAssertions[assertion.AssertionID]; duplicate {
			return contractError(ErrDuplicateField, "proof.assertions.assertion_id", "assertion identity is duplicated")
		}
		seenAssertions[assertion.AssertionID] = struct{}{}
	}
	if err := receipt.Effect.Validate(); err != nil {
		return err
	}
	switch receipt.Status {
	case ProofUnproven, ProofBlocked, ProofFailed, ProofPassedFake, ProofPassedLive:
	default:
		return contractError(ErrInvalidContract, "proof.status", "proof status is unsupported")
	}
	if receipt.StartedAt != nil {
		if err := validateUTCInstant(*receipt.StartedAt, "proof.started_at"); err != nil {
			return err
		}
	}
	if receipt.FinishedAt != nil {
		if err := validateUTCInstant(*receipt.FinishedAt, "proof.finished_at"); err != nil {
			return err
		}
		if receipt.StartedAt == nil {
			return contractError(ErrInvalidContract, "proof.finished_at", "cannot precede an absent start time")
		}
		if receipt.FinishedAt.Before(*receipt.StartedAt) {
			return contractError(ErrInvalidContract, "proof.finished_at", "cannot precede the physical start time")
		}
	}
	if err := receipt.Provenance.Validate(); err != nil {
		return err
	}
	if receipt.ImplementationClosure != nil {
		if err := receipt.ImplementationClosure.Validate(); err != nil {
			return err
		}
	}
	seenDiagnostics := make(map[string]struct{}, len(receipt.CleanupDiagnostics))
	for _, diagnostic := range receipt.CleanupDiagnostics {
		if err := diagnostic.Validate(); err != nil {
			return err
		}
		key := diagnostic.StepID + "\x00" + diagnostic.Code
		if _, duplicate := seenDiagnostics[key]; duplicate {
			return contractError(ErrDuplicateField, "proof.cleanup_diagnostics", "cleanup diagnostic is duplicated")
		}
		seenDiagnostics[key] = struct{}{}
	}
	if receipt.Provenance.Reused == nil {
		if receipt.Provenance.OriginalRunID != nil {
			return contractError(ErrInvalidContract, "proof.provenance.original_run_id", "original run requires explicit reuse provenance")
		}
	} else if *receipt.Provenance.Reused {
		if receipt.Provenance.OriginalRunID == nil {
			return contractError(ErrInvalidContract, "proof.provenance.original_run_id", "reused evidence must retain the original physical run identity")
		}
		if receipt.Status != ProofPassedFake && receipt.Status != ProofPassedLive {
			return contractError(ErrInvalidContract, "proof.provenance.reused", "only a completed pass can be reused")
		}
		if receipt.ImplementationClosure == nil || !receipt.ImplementationClosure.Known || receipt.ImplementationClosure.Digest == nil {
			return contractError(ErrInvalidContract, "proof.implementation_closure", "reuse requires a known exact implementation closure")
		}
	} else if receipt.Provenance.OriginalRunID != nil {
		return contractError(ErrInvalidContract, "proof.provenance.original_run_id", "fresh evidence cannot claim an original reused run")
	}
	if receipt.Status == ProofPassedFake || receipt.Status == ProofPassedLive {
		if err := receipt.validateCompletedPass(); err != nil {
			return err
		}
	}
	return nil
}

func (receipt ProofReceipt) validateCompletedPass() error {
	if receipt.RunID == nil || receipt.BundleHash == nil || receipt.SchemaHash == nil ||
		receipt.PMClientHash == nil || receipt.PMAPIHash == nil || receipt.PMWorkerHash == nil ||
		receipt.HandlerVersions == nil || len(receipt.HandlerVersions) == 0 ||
		receipt.WorkspaceAccountScopeHash == nil || receipt.Request == nil || receipt.Response == nil ||
		receipt.StartedAt == nil || receipt.FinishedAt == nil || receipt.Provenance.HostClass == nil ||
		receipt.Provenance.Reused == nil {
		return contractError(ErrInvalidContract, "proof", "completed pass lacks required exact physical component, source, scope, or timing evidence")
	}
	if receipt.Status == ProofPassedFake && receipt.Provenance.ExecutionKind != "fake" {
		return contractError(ErrInvalidContract, "proof.provenance.execution_kind", "fake pass must retain fake execution provenance")
	}
	if receipt.Status == ProofPassedLive && receipt.Provenance.ExecutionKind != "live" {
		return contractError(ErrInvalidContract, "proof.provenance.execution_kind", "live pass must retain live execution provenance")
	}
	for _, assertion := range receipt.Assertions {
		if assertion.Passed == nil || !*assertion.Passed {
			return contractError(ErrInvalidContract, "proof.assertions.passed", "completed pass requires every independent assertion to pass")
		}
	}
	if receipt.Effect.State == OutcomeUnknown {
		return contractError(ErrInvalidContract, "proof.effect.state", "unknown effect truth cannot support a completed pass")
	}
	return nil
}

func (observation ProofRequestObservation) Validate() error {
	if !validBoundedText(observation.Method, 1, 64) {
		return contractError(ErrInvalidContract, "proof.request.method", "must be a bounded method")
	}
	if !validBoundedText(observation.Target, 1, 4096) {
		return contractError(ErrInvalidContract, "proof.request.target", "must be a bounded sanitized target")
	}
	if !validBoundedText(observation.MediaType, 0, 512) {
		return contractError(ErrInvalidContract, "proof.request.media_type", "must be a bounded media type")
	}
	if observation.BodySHA256 != nil {
		if err := observation.BodySHA256.Validate(); err != nil {
			return err
		}
	}
	if err := validateSanitizedFields(observation.SanitizedFields, "proof.request.sanitized_fields"); err != nil {
		return err
	}
	return validateProofRedactions(observation.Redactions, "proof.request.redactions")
}

func (observation ProofResponseObservation) Validate() error {
	if observation.Status < 100 || observation.Status > 599 {
		return contractError(ErrInvalidContract, "proof.response.status", "must be one valid HTTP response status")
	}
	if !validBoundedText(observation.MediaType, 0, 512) {
		return contractError(ErrInvalidContract, "proof.response.media_type", "must be a bounded media type")
	}
	if observation.BodySHA256 != nil {
		if err := observation.BodySHA256.Validate(); err != nil {
			return err
		}
	}
	if err := validateSanitizedFields(observation.SanitizedFields, "proof.response.sanitized_fields"); err != nil {
		return err
	}
	return validateProofRedactions(observation.Redactions, "proof.response.redactions")
}

func (redaction ProofRedaction) Validate() error {
	if len(redaction.Pointer) > 4096 || !validJSONPointer(redaction.Pointer) {
		return contractError(ErrInvalidContract, "proof.redaction.pointer", "must be a bounded RFC 6901 pointer")
	}
	switch redaction.Replacement {
	case "hash", "scoped_reference", "excluded":
	default:
		return contractError(ErrInvalidContract, "proof.redaction.replacement", "redaction replacement is unsupported")
	}
	return nil
}

func (observation ProofAssertionObservation) Validate() error {
	if !validBoundedIdentityPart(observation.AssertionID, 1024) {
		return contractError(ErrInvalidIdentity, "proof.assertion.assertion_id", "must identify one bounded assertion")
	}
	if err := validateSanitizedJSON(observation.Expected, "proof.assertion.expected"); err != nil {
		return err
	}
	if err := validateSanitizedJSON(observation.Observed, "proof.assertion.observed"); err != nil {
		return err
	}
	return nil
}

func (effect ProofEffect) Validate() error {
	switch effect.State {
	case OutcomeUnsent, OutcomeSucceeded, OutcomeUnchanged, OutcomeRejected, OutcomeUnknown:
	default:
		return contractError(ErrInvalidContract, "proof.effect.state", "effect state is unsupported")
	}
	if err := validateProofIdentityList(effect.ReceiptRefs, "proof.effect.receipt_refs"); err != nil {
		return err
	}
	return validateProofIdentityList(effect.OwnedObjectRefs, "proof.effect.owned_object_refs")
}

func (provenance ProofProvenance) Validate() error {
	switch provenance.ExecutionKind {
	case "planned", "fake", "live":
	default:
		return contractError(ErrInvalidContract, "proof.provenance.execution_kind", "execution kind is unsupported")
	}
	if provenance.HostClass != nil && !validBoundedIdentityPart(*provenance.HostClass, 1024) {
		return contractError(ErrInvalidIdentity, "proof.provenance.host_class", "must retain one bounded host class")
	}
	if provenance.OriginalRunID != nil && !validBoundedIdentityPart(*provenance.OriginalRunID, 1024) {
		return contractError(ErrInvalidIdentity, "proof.provenance.original_run_id", "must retain one bounded original run identity")
	}
	return nil
}

func (closure ImplementationClosure) Validate() error {
	if closure.Known {
		if closure.Digest == nil {
			return contractError(ErrInvalidContract, "implementation_closure.digest", "known closure requires one exact digest")
		}
		return closure.Digest.Validate()
	}
	if closure.Digest != nil {
		return contractError(ErrInvalidContract, "implementation_closure.digest", "unknown closure cannot claim an exact digest")
	}
	return nil
}

func (diagnostic ProofCleanupDiagnostic) Validate() error {
	if !validBoundedIdentityPart(diagnostic.StepID, 1024) {
		return contractError(ErrInvalidIdentity, "proof.cleanup_diagnostic.step_id", "must identify one bounded cleanup step")
	}
	if !validReasonCode(diagnostic.Code) {
		return contractError(ErrInvalidContract, "proof.cleanup_diagnostic.code", "must be a bounded safe reason code")
	}
	return nil
}

func validateProofIdentityList(values []string, path string) error {
	if values == nil {
		return contractError(ErrInvalidContract, path, "must be explicitly present, even when empty")
	}
	for _, value := range values {
		if !validBoundedIdentityPart(value, 1024) {
			return contractError(ErrInvalidIdentity, path, "contains an invalid bounded identity")
		}
	}
	return nil
}

func validateProofRedactions(redactions []ProofRedaction, path string) error {
	if redactions == nil {
		return contractError(ErrInvalidContract, path, "must be explicitly present, even when empty")
	}
	seen := make(map[string]struct{}, len(redactions))
	for _, redaction := range redactions {
		if err := redaction.Validate(); err != nil {
			return err
		}
		if _, duplicate := seen[redaction.Pointer]; duplicate {
			return contractError(ErrDuplicateField, path, "redaction pointer is duplicated")
		}
		seen[redaction.Pointer] = struct{}{}
	}
	return nil
}

func validateSanitizedFields(fields map[string]json.RawMessage, path string) error {
	if fields == nil {
		return contractError(ErrInvalidContract, path, "must be an explicitly present object")
	}
	totalBytes := 0
	for name, value := range fields {
		if !validBoundedText(name, 1, 1024) {
			return contractError(ErrInvalidContract, path, "contains an invalid bounded field name")
		}
		if err := validateSanitizedJSON(value, path); err != nil {
			return err
		}
		totalBytes += len(name) + len(value)
		if totalBytes > MaxContractDocumentBytes {
			return contractError(ErrInvalidContract, path, "sanitized fields exceed the contract byte limit")
		}
	}
	return nil
}

func validateSanitizedJSON(document json.RawMessage, path string) error {
	if document == nil {
		return contractError(ErrInvalidContract, path, "required sanitized value is absent")
	}
	if len(document) > MaxContractDocumentBytes {
		return contractError(ErrInvalidContract, path, "sanitized value exceeds the contract byte limit")
	}
	var value any
	if err := decodeStrictJSON(document, &value); err != nil {
		return contractError(ErrInvalidContract, path, "sanitized value is malformed or ambiguous")
	}
	if !validSanitizedNode(value, 0) {
		return contractError(ErrInvalidContract, path, "sanitized value exceeds receipt schema bounds")
	}
	return nil
}

func validSanitizedNode(value any, depth int) bool {
	if depth > MaxSchemaDepth+16 {
		return false
	}
	switch typed := value.(type) {
	case nil, bool, json.Number:
		return true
	case string:
		return validBoundedText(typed, 0, 16384)
	case []any:
		for _, element := range typed {
			if !validSanitizedNode(element, depth+1) {
				return false
			}
		}
		return true
	case map[string]any:
		for name, element := range typed {
			if !validBoundedText(name, 1, 1024) || !validSanitizedNode(element, depth+1) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func validBoundedText(value string, minimum, maximum int) bool {
	if !utf8.ValidString(value) {
		return false
	}
	length := utf8.RuneCountInString(value)
	return length >= minimum && length <= maximum
}
