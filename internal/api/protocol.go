package api

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const maxPublicEnvelopeBytes = 1 << 20

var (
	ErrUnsupportedProtocolMajor   = errors.New("unsupported protocol major")
	ErrUnsupportedRequiredFeature = errors.New("unsupported required feature")
	ErrProblemMediaType           = errors.New("problem response has unsupported media type")
	ErrMalformedEnvelope          = errors.New("malformed public envelope")
	ErrForbiddenSecretField       = errors.New("forbidden secret-bearing public field")
	ErrDuplicateMetadataField     = errors.New("duplicate public metadata field")
	ErrUnknownMetadataField       = errors.New("unknown public metadata field")
	ErrWrongCaseMetadataField     = errors.New("wrong-case public metadata field")
	ErrOneTimeCredentialContext   = errors.New("one-time PM credential outside original creation response")
)

var intentDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type DecodedServerInfo struct {
	Value          ServerInfo
	AdditiveFields map[string]json.RawMessage
}

type DecodedProblem struct {
	Value          Problem
	Disposition    ProblemDisposition
	AdditiveFields map[string]json.RawMessage
}

// DecodeServerInfo applies client compatibility before any submission. Unknown
// response members survive as exact JSON bytes unless their names collide with
// known fields or the public secret boundary.
func DecodeServerInfo(document []byte, supportedMajor int, supportedFeatures []string) (DecodedServerInfo, error) {
	object, err := decodeJSONObject(document)
	if err != nil {
		return DecodedServerInfo{}, err
	}
	known := []string{"protocol_major", "protocol_version", "instance_id", "features", "required_features"}
	if err := requireResponseFields(object, known); err != nil {
		return DecodedServerInfo{}, err
	}
	additions, err := safeAdditiveFields(object, known)
	if err != nil {
		return DecodedServerInfo{}, err
	}
	var value ServerInfo
	if err := json.Unmarshal(document, &value); err != nil {
		return DecodedServerInfo{}, wrapMalformed(err)
	}
	if supportedMajor != ProtocolMajor || value.ProtocolMajor != ProtocolMajor || value.ProtocolVersion != WireVersion {
		return DecodedServerInfo{}, ErrUnsupportedProtocolMajor
	}
	if !validIdentity(value.InstanceId, 512) || !validFeatureList(value.Features) || !validFeatureList(value.RequiredFeatures) {
		return DecodedServerInfo{}, ErrMalformedEnvelope
	}
	clientFeatures := stringSet(supportedFeatures)
	for _, feature := range value.RequiredFeatures {
		if _, supported := clientFeatures[feature]; !supported || !IsSupportedFeature(feature) {
			return DecodedServerInfo{}, fmt.Errorf("%w: %s", ErrUnsupportedRequiredFeature, feature)
		}
	}
	return DecodedServerInfo{Value: value, AdditiveFields: additions}, nil
}

// DecodeProblem decodes the accepted RFC9457/F01 envelope. HTTP status, PM
// code, retry action, and acceptance disposition must be one exact table row.
func DecodeProblem(status int, contentType string, document []byte) (DecodedProblem, error) {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.EqualFold(mediaType, "application/problem+json") {
		return DecodedProblem{}, ErrProblemMediaType
	}
	object, err := decodeJSONObject(document)
	if err != nil {
		return DecodedProblem{}, err
	}
	known := []string{"type", "title", "status", "detail", "code", "incident_id", "safe_retry_action", "outcome_reference"}
	required := known[:7]
	if err := requireResponseFields(object, required); err != nil {
		return DecodedProblem{}, err
	}
	additions, err := safeAdditiveFields(object, known)
	if err != nil {
		return DecodedProblem{}, err
	}
	var value Problem
	if err := json.Unmarshal(document, &value); err != nil {
		return DecodedProblem{}, wrapMalformed(err)
	}
	if value.Status != status || status < 400 || status > 599 ||
		strings.TrimSpace(value.Title) == "" || len(value.Title) > 256 ||
		strings.TrimSpace(value.Detail) == "" || len(value.Detail) > 2048 ||
		!validIdentity(value.Code, 128) || !validIdentity(value.IncidentId, 512) ||
		!validIdentity(value.SafeRetryAction, 128) || !validProblemType(value.Type) {
		return DecodedProblem{}, ErrMalformedEnvelope
	}
	rule, ok := ProblemRuleFor(status, value.Code)
	if !ok || rule.SafeRetryAction != value.SafeRetryAction {
		return DecodedProblem{}, ErrMalformedEnvelope
	}
	if (rule.Disposition == ProblemOutcomeUnknown || rule.Disposition == ProblemResolvedReceiptExpired) && value.OutcomeReference == nil {
		return DecodedProblem{}, ErrMalformedEnvelope
	}
	if value.OutcomeReference != nil && !validOutcomeReference(*value.OutcomeReference) {
		return DecodedProblem{}, ErrMalformedEnvelope
	}
	return DecodedProblem{Value: value, Disposition: rule.Disposition, AdditiveFields: additions}, nil
}

func validProblemType(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.IsAbs()
}
func validOutcomeReference(reference SafeOutcomeReference) bool {
	if !validIdentity(reference.Ref, 512) {
		return false
	}
	switch string(reference.Kind) {
	case "request", "run", "connection", "membership", "service_account", "delegation":
		return true
	default:
		return false
	}
}

func validIdentity(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && strings.TrimSpace(value) == value
}

func validFeatureList(features []string) bool {
	if hasDuplicateStrings(features) {
		return false
	}
	for _, feature := range features {
		if !validIdentity(feature, 128) {
			return false
		}
	}
	return true
}

func DecodeAdmissionResponse(document []byte) (AdmissionResponse, error) {
	known := []string{"state", "run_ref", "request_key", "intent_digest", "outcome_reference", "replay"}
	if _, err := decodeExactJSONObject(document, known, known); err != nil {
		return AdmissionResponse{}, err
	}
	var response AdmissionResponse
	if err := json.Unmarshal(document, &response); err != nil {
		return AdmissionResponse{}, wrapMalformed(err)
	}
	if string(response.State) != "accepted" || !validIdentity(response.RunRef, 512) ||
		!validIdentity(response.RequestKey, 200) || !intentDigestPattern.MatchString(response.IntentDigest) ||
		!validOutcomeReference(response.OutcomeReference) {
		return AdmissionResponse{}, ErrMalformedEnvelope
	}
	return response, nil
}

func DecodeMutationReplayResponse(document []byte) (MutationReplayResponse, error) {
	known := []string{"resource_id", "result_revision", "outcome_reference", "result_disposition", "requested_actor_ref", "effective_actor_ref", "replay"}
	required := []string{"resource_id", "result_revision", "outcome_reference", "result_disposition", "replay"}
	if _, err := decodeExactJSONObject(document, known, required); err != nil {
		return MutationReplayResponse{}, err
	}
	var response MutationReplayResponse
	if err := json.Unmarshal(document, &response); err != nil {
		return MutationReplayResponse{}, wrapMalformed(err)
	}
	if !bool(response.Replay) || string(response.ResultDisposition) != "prior_mutation_result" ||
		!validIdentity(response.ResourceId, 512) || !validIdentity(response.ResultRevision, 512) ||
		!validOutcomeReference(response.OutcomeReference) {
		return MutationReplayResponse{}, ErrMalformedEnvelope
	}
	return response, nil
}

func DecodeServiceAccountCreationResponse(status int, document []byte) (ServiceAccountCreationResponse, error) {
	if status != http.StatusCreated {
		return ServiceAccountCreationResponse{}, ErrOneTimeCredentialContext
	}
	known := []string{"resource_id", "result_revision", "outcome_reference", "result_disposition", "replay", "pm_automation_credential"}
	if _, err := decodeExactJSONObjectAllowingCredential(document, known, known); err != nil {
		return ServiceAccountCreationResponse{}, err
	}
	var response ServiceAccountCreationResponse
	if err := json.Unmarshal(document, &response); err != nil {
		return ServiceAccountCreationResponse{}, wrapMalformed(err)
	}
	if bool(response.Replay) || string(response.ResultDisposition) != "created" ||
		!validIdentity(response.ResourceId, 512) || !validIdentity(response.ResultRevision, 512) ||
		strings.TrimSpace(response.PmAutomationCredential) == "" || len(response.PmAutomationCredential) > 4096 ||
		!validOutcomeReference(response.OutcomeReference) {
		return ServiceAccountCreationResponse{}, ErrOneTimeCredentialContext
	}
	return response, nil
}

// ValidateIncomingJSON rejects duplicate, wrong-case, unknown, missing, and
// secret-bearing control members without rewriting provider-owned field names.
func ValidateIncomingJSON(operationID string, document []byte) error {
	control, ok := OperationControlFor(operationID)
	if !ok {
		return fmt.Errorf("%w: uncontrolled operation", ErrUnknownMetadataField)
	}
	object, err := decodeJSONObject(document)
	if err != nil {
		return err
	}
	if err := rejectForbiddenFields(object, true); err != nil {
		return err
	}
	allowed := append(append([]string(nil), control.RequiredBodyFields...), control.OptionalBodyFields...)
	if err := validateExactObjectFields(object, allowed, control.RequiredBodyFields); err != nil {
		return err
	}
	if control.HasProtocol {
		protocolRaw, ok := object["protocol"]
		if !ok {
			return ErrMalformedEnvelope
		}
		var protocol map[string]json.RawMessage
		if err := json.Unmarshal(protocolRaw, &protocol); err != nil || protocol == nil {
			return ErrMalformedEnvelope
		}
		metadataAllowed := append(append([]string(nil), ProtocolMetadataRequiredFields[:]...), ProtocolMetadataOptionalFields[:]...)
		if err := validateExactObjectFields(protocol, metadataAllowed, ProtocolMetadataRequiredFields[:]); err != nil {
			return err
		}
		if err := validateProtocolMetadata(protocol); err != nil {
			return err
		}
	}
	if err := validateOperationObjectFields(operationID, object); err != nil {
		return err
	}
	return nil
}

// ValidateHTTPRequest enforces generated header/body controls while restoring
// the exact body bytes for the generated strict binder.
func ValidateHTTPRequest(operationID string, request *http.Request) error {
	if request == nil || request.URL == nil {
		return ErrMalformedEnvelope
	}
	control, ok := OperationControlFor(operationID)
	if !ok || request.Method != control.Method {
		return ErrMalformedEnvelope
	}
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return ErrMalformedEnvelope
	}
	for name, values := range query {
		allowed := false
		for _, field := range control.QueryFields {
			allowed = allowed || name == field
		}
		if !allowed || len(values) != 1 {
			return ErrMalformedEnvelope
		}
	}
	if control.RequiresIdempotencyKey {
		if err := requireSingleHeader(request.Header, "Idempotency-Key"); err != nil {
			return err
		}
	}
	// New mutations require a revision only after current authorization and
	// receipt lookup. The wire boundary validates a supplied value, not absence.
	hasIfMatch := len(request.Header.Values("If-Match")) != 0
	if control.RequiresIfMatch && hasIfMatch {
		if err := requireSingleHeader(request.Header, "If-Match"); err != nil {
			return err
		}
	}
	if !control.HasBody {
		if request.Body != nil {
			var extra [1]byte
			n, err := io.ReadFull(request.Body, extra[:])
			request.Body.Close()
			request.Body = http.NoBody
			if n != 0 || err != io.EOF {
				return ErrMalformedEnvelope
			}
		}
		return nil
	}
	if err := requireSingleHeader(request.Header, "Content-Type"); err != nil {
		return err
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return ErrMalformedEnvelope
	}
	if request.Body == nil {
		return ErrMalformedEnvelope
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxPublicEnvelopeBytes+1))
	if err != nil {
		return ErrMalformedEnvelope
	}
	request.Body.Close()
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	if len(body) > maxPublicEnvelopeBytes {
		return ErrMalformedEnvelope
	}
	if err := ValidateIncomingJSON(operationID, body); err != nil {
		return err
	}
	if !control.HasProtocol {
		return nil
	}
	var envelope struct {
		Protocol struct {
			RequestKey       string `json:"request_key"`
			ExpectedRevision string `json:"expected_revision"`
		} `json:"protocol"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ErrMalformedEnvelope
	}
	if control.RequiresIdempotencyKey && request.Header.Get("Idempotency-Key") != envelope.Protocol.RequestKey {
		return ErrMalformedEnvelope
	}
	if control.RequiresIfMatch && hasIfMatch && request.Header.Get("If-Match") != envelope.Protocol.ExpectedRevision {
		return ErrMalformedEnvelope
	}
	return nil
}

func requireSingleHeader(header http.Header, name string) error {
	values := header.Values(name)
	if len(values) != 1 || strings.TrimSpace(values[0]) == "" {
		return ErrMalformedEnvelope
	}
	return nil
}

func validateProtocolMetadata(protocol map[string]json.RawMessage) error {
	var metadata struct {
		WireVersion      string   `json:"wire_version"`
		RequiredFeatures []string `json:"required_features"`
		RequestKey       string   `json:"request_key"`
		IntentDigest     string   `json:"intent_digest"`
		ExpectedRevision string   `json:"expected_revision"`
	}
	encoded, err := json.Marshal(protocol)
	if err != nil || json.Unmarshal(encoded, &metadata) != nil {
		return ErrMalformedEnvelope
	}
	if metadata.WireVersion != WireVersion || !validIdentity(metadata.RequestKey, 200) ||
		!intentDigestPattern.MatchString(metadata.IntentDigest) || !validIdentity(metadata.ExpectedRevision, 512) ||
		!validFeatureList(metadata.RequiredFeatures) {
		return ErrMalformedEnvelope
	}
	for _, feature := range metadata.RequiredFeatures {
		if !IsSupportedFeature(feature) {
			return fmt.Errorf("%w: %s", ErrUnsupportedRequiredFeature, feature)
		}
	}
	return nil
}

func validateOperationObjectFields(operationID string, root map[string]json.RawMessage) error {
	controls := operationObjectControls[operationID]
	for pointer, control := range controls {
		if pointer == "" {
			continue
		}
		value, present, err := rawValueAtPointer(root, pointer)
		if err != nil {
			return err
		}
		if !present {
			continue
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			if control.Nullable {
				continue
			}
			return ErrMalformedEnvelope
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(value, &object); err != nil || object == nil {
			return ErrMalformedEnvelope
		}
		if err := validateExactObjectFields(object, control.AllowedFields, control.RequiredFields); err != nil {
			return err
		}
	}
	return nil
}

func rawValueAtPointer(root map[string]json.RawMessage, pointer string) (json.RawMessage, bool, error) {
	if pointer == "" {
		encoded, err := json.Marshal(root)
		return encoded, true, err
	}
	current := root
	parts := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	for index, encodedPart := range parts {
		part := strings.ReplaceAll(strings.ReplaceAll(encodedPart, "~1", "/"), "~0", "~")
		raw, ok := current[part]
		if !ok {
			return nil, false, nil
		}
		if index == len(parts)-1 {
			return raw, true, nil
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return nil, false, nil
		}
		if err := json.Unmarshal(raw, &current); err != nil || current == nil {
			return nil, false, ErrMalformedEnvelope
		}
	}
	return nil, false, nil
}

func validateExactObjectFields(object map[string]json.RawMessage, allowed, required []string) error {
	for name := range object {
		if isForbiddenPublicField(name) {
			return fmt.Errorf("%w: %s", ErrForbiddenSecretField, name)
		}
	}
	return validateObjectFieldNames(object, allowed, required)
}

// ValidateServerObjectJSON checks the generated root-field grammar for a
// dedicated server-owned surface. It does not admit an ordinary public request:
// those must use ValidateIncomingJSON and its public-secret restrictions.
// Authentication and typed value binding remain mandatory at the caller.
func ValidateServerObjectJSON(document []byte, allowed, required []string) error {
	object, err := decodeJSONObject(document)
	if err != nil {
		return err
	}
	return validateObjectFieldNames(object, allowed, required)
}

func validateObjectFieldNames(object map[string]json.RawMessage, allowed, required []string) error {
	allowedSet := stringSet(allowed)
	for name := range object {
		if _, ok := allowedSet[name]; ok {
			continue
		}
		for exact := range allowedSet {
			if strings.EqualFold(name, exact) {
				return fmt.Errorf("%w: %s", ErrWrongCaseMetadataField, name)
			}
		}
		return fmt.Errorf("%w: %s", ErrUnknownMetadataField, name)
	}
	for _, name := range required {
		if _, ok := object[name]; !ok {
			return fmt.Errorf("%w: missing %s", ErrMalformedEnvelope, name)
		}
	}
	return nil
}

func decodeExactJSONObject(document []byte, allowed, required []string) (map[string]json.RawMessage, error) {
	object, err := decodeJSONObject(document)
	if err != nil {
		return nil, err
	}
	if err := rejectForbiddenFields(object, false); err != nil {
		return nil, err
	}
	if err := validateExactObjectFields(object, allowed, required); err != nil {
		return nil, err
	}
	return object, nil
}

func decodeExactJSONObjectAllowingCredential(document []byte, allowed, required []string) (map[string]json.RawMessage, error) {
	object, err := decodeJSONObject(document)
	if err != nil {
		return nil, err
	}
	ordinaryFields := make(map[string]json.RawMessage, len(object))
	for name, value := range object {
		if name != "pm_automation_credential" {
			ordinaryFields[name] = value
		}
	}
	if err := rejectForbiddenFields(ordinaryFields, false); err != nil {
		return nil, err
	}
	if err := validateExactObjectFieldsAllowingCredential(object, allowed, required); err != nil {
		return nil, err
	}
	return object, nil
}

func validateExactObjectFieldsAllowingCredential(object map[string]json.RawMessage, allowed, required []string) error {
	for name := range object {
		if name != "pm_automation_credential" && isForbiddenPublicField(name) {
			return fmt.Errorf("%w: %s", ErrForbiddenSecretField, name)
		}
	}
	return validateObjectFieldNames(object, allowed, required)
}

func decodeJSONObject(document []byte) (map[string]json.RawMessage, error) {
	if len(document) == 0 || len(document) > maxPublicEnvelopeBytes {
		return nil, ErrMalformedEnvelope
	}
	if err := rejectDuplicateJSONFields(document); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, ErrMalformedEnvelope
	}
	if err := requireJSONDecoderEOF(decoder); err != nil {
		return nil, ErrMalformedEnvelope
	}
	return object, nil
}

func rejectDuplicateJSONFields(document []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if err := requireJSONDecoderEOF(decoder); err != nil {
		return ErrMalformedEnvelope
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return ErrMalformedEnvelope
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return ErrMalformedEnvelope
			}
			name, ok := nameToken.(string)
			if !ok {
				return ErrMalformedEnvelope
			}
			if _, duplicate := seen[name]; duplicate {
				return fmt.Errorf("%w: %s", ErrDuplicateMetadataField, name)
			}
			seen[name] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return ErrMalformedEnvelope
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return ErrMalformedEnvelope
		}
	default:
		return ErrMalformedEnvelope
	}
	return nil
}

func requireJSONDecoderEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return ErrMalformedEnvelope
	}
	return nil
}

func requireResponseFields(object map[string]json.RawMessage, required []string) error {
	for _, name := range required {
		if _, exists := object[name]; !exists {
			return fmt.Errorf("%w: missing %s", ErrMalformedEnvelope, name)
		}
	}
	return nil
}

func safeAdditiveFields(object map[string]json.RawMessage, known []string) (map[string]json.RawMessage, error) {
	if err := rejectForbiddenFields(object, false); err != nil {
		return nil, err
	}
	knownSet := stringSet(known)
	additions := make(map[string]json.RawMessage)
	for name, value := range object {
		if _, exists := knownSet[name]; exists {
			continue
		}
		for exact := range knownSet {
			if strings.EqualFold(name, exact) {
				return nil, fmt.Errorf("%w: %s", ErrWrongCaseMetadataField, name)
			}
		}
		additions[name] = append(json.RawMessage(nil), value...)
	}
	return additions, nil
}

func rejectForbiddenFields(value any, allowProviderFields bool) error {
	var visit func(any, bool) error
	visit = func(value any, providerFieldNames bool) error {
		switch typed := value.(type) {
		case map[string]json.RawMessage:
			for name, raw := range typed {
				if !providerFieldNames && isForbiddenPublicField(name) {
					return fmt.Errorf("%w: %s", ErrForbiddenSecretField, name)
				}
				var child any
				decoder := json.NewDecoder(bytes.NewReader(raw))
				decoder.UseNumber()
				if err := decoder.Decode(&child); err != nil {
					return ErrMalformedEnvelope
				}
				if err := visit(child, allowProviderFields && name == "fields"); err != nil {
					return err
				}
			}
		case map[string]any:
			for name, child := range typed {
				if !providerFieldNames && isForbiddenPublicField(name) {
					return fmt.Errorf("%w: %s", ErrForbiddenSecretField, name)
				}
				if err := visit(child, allowProviderFields && name == "fields"); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range typed {
				if err := visit(child, false); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return visit(value, false)
}

func isForbiddenPublicField(name string) bool {
	return strings.EqualFold(name, "pm_automation_credential") || IsForbiddenPublicField(name)
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func hasDuplicateStrings(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return true
		}
		if _, exists := seen[value]; exists {
			return true
		}
		seen[value] = struct{}{}
	}
	return false
}

func wrapMalformed(error) error {
	return ErrMalformedEnvelope
}

// NewProtocolProblem returns a safe pre-handler refusal and intentionally does
// not copy parser, provider, credential, or application errors into detail.
func NewProtocolProblem(_ error) Problem {
	return Problem{
		Type:            "https://polymetrics.ai/problems/invalid_request",
		Title:           "Invalid request",
		Status:          http.StatusBadRequest,
		Detail:          "The public request envelope is malformed or unsupported.",
		Code:            "invalid_request",
		IncidentId:      "pm-" + rand.Text(),
		SafeRetryAction: "correct_input",
	}
}

// NewInternalProblem returns a safe boundary failure without exposing the
// underlying error. Actual application error mapping remains with its owner.
func NewInternalProblem(_ error) Problem {
	return Problem{
		Type:            "https://polymetrics.ai/problems/internal_error",
		Title:           "Internal error",
		Status:          http.StatusInternalServerError,
		Detail:          "The response could not be completed safely.",
		Code:            "internal_error",
		IncidentId:      "pm-" + rand.Text(),
		SafeRetryAction: "contact_support_with_incident_id",
	}
}

// These DTOs deliberately carry one-time PM authentication material on their
// designated wire paths. Diagnostic formatting must never become a second
// credential transport; JSON encoding remains the explicit client boundary.
func (DeviceProof) String() string       { return "PM device proof [REDACTED]" }
func (DeviceProof) GoString() string     { return "PM device proof [REDACTED]" }
func (DeviceChallenge) String() string   { return "PM device challenge [REDACTED]" }
func (DeviceChallenge) GoString() string { return "PM device challenge [REDACTED]" }
func (PMDeviceSession) String() string   { return "PM device session [REDACTED]" }
func (PMDeviceSession) GoString() string { return "PM device session [REDACTED]" }
func (BrowserLogin) String() string      { return "PM browser callback [REDACTED]" }
func (BrowserLogin) GoString() string    { return "PM browser callback [REDACTED]" }
func (ServiceAccountCreationResponse) String() string {
	return "PM service-account creation [REDACTED]"
}
func (ServiceAccountCreationResponse) GoString() string {
	return "PM service-account creation [REDACTED]"
}
