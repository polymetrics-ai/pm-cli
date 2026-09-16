package contract

import (
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"
)

const OperationSchemaVersion = "pm.operation/v1"

type SourceRef struct {
	SourceID string `json:"source_id"`
	Pointer  string `json:"pointer"`
}

type Transport struct {
	Kind      string `json:"kind"`
	Method    string `json:"method,omitempty"`
	Path      string `json:"path,omitempty"`
	Operation string `json:"operation,omitempty"`
}

type Response struct {
	Status    string `json:"status"`
	MediaType string `json:"media_type"`
	Schema    Schema `json:"schema"`
}

type Lane string

const (
	LaneDirectRead     Lane = "direct_read"
	LaneDirectWrite    Lane = "direct_write"
	LaneETL            Lane = "etl"
	LaneReverseETL     Lane = "reverse_etl"
	LaneBinaryDownload Lane = "binary_download"
	LaneBinaryUpload   Lane = "binary_upload"
	LaneSyncTransport  Lane = "sync_transport"
)

type LaneEligibility struct {
	Lane     Lane   `json:"lane"`
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason,omitempty"`
}

func (l *LaneEligibility) UnmarshalJSON(data []byte) error {
	type wire struct {
		Lane     Lane   `json:"lane"`
		Eligible *bool  `json:"eligible"`
		Reason   string `json:"reason,omitempty"`
	}
	var decoded wire
	if err := decodeStrictJSON(data, &decoded); err != nil {
		return err
	}
	if decoded.Eligible == nil {
		return contractError(ErrInvalidContract, "lane_eligibility.eligible", "must be explicit")
	}
	*l = LaneEligibility{Lane: decoded.Lane, Eligible: *decoded.Eligible, Reason: decoded.Reason}
	return nil
}

type Operation struct {
	SchemaVersion          string                  `json:"schema_version"`
	OperationID            string                  `json:"operation_id"`
	SourceRefs             []SourceRef             `json:"source_refs"`
	Transport              Transport               `json:"transport"`
	Inputs                 []FieldBinding          `json:"inputs"`
	Responses              []Response              `json:"responses"`
	CapabilityRequirements []CapabilityRequirement `json:"capability_requirements"`
	LaneEligibility        []LaneEligibility       `json:"lane_eligibility"`
}

func DecodeOperation(document []byte, supported []FoundationContract) (Operation, error) {
	var fields map[string]json.RawMessage
	if err := decodeContractJSON(document, &fields); err != nil {
		return Operation{}, err
	}
	for _, required := range []string{
		"schema_version", "operation_id", "source_refs", "transport", "inputs",
		"responses", "capability_requirements", "lane_eligibility",
	} {
		if _, ok := fields[required]; !ok {
			return Operation{}, contractError(ErrInvalidContract, required, "required field is absent")
		}
	}
	var operation Operation
	if err := decodeStrictJSON(document, &operation); err != nil {
		return Operation{}, err
	}
	if err := operation.Validate(supported); err != nil {
		return Operation{}, err
	}
	return operation, nil
}

func (o Operation) Validate(supported []FoundationContract) error {
	if o.SchemaVersion != OperationSchemaVersion {
		return contractError(ErrUnsupportedVersion, "schema_version", "operation schema revision is unsupported")
	}
	if !validIdentityPart(o.OperationID) {
		return contractError(ErrInvalidContract, "operation_id", "must be a nonempty stable operation identity")
	}
	if len(o.SourceRefs) == 0 {
		return contractError(ErrInvalidContract, "source_refs", "must preserve at least one exact source reference")
	}
	seenSourceRefs := make(map[string]struct{}, len(o.SourceRefs))
	for _, reference := range o.SourceRefs {
		if !validIdentityPart(reference.SourceID) || !validSourcePointer(reference.Pointer) {
			return contractError(ErrInvalidContract, "source_refs", "contains an incomplete or malformed source identity")
		}
		key := reference.SourceID + "\x00" + reference.Pointer
		if _, duplicate := seenSourceRefs[key]; duplicate {
			return contractError(ErrDuplicateField, "source_refs", "contains a duplicate source reference")
		}
		seenSourceRefs[key] = struct{}{}
	}
	if err := o.Transport.Validate(); err != nil {
		return err
	}
	if o.Inputs == nil {
		return contractError(ErrInvalidContract, "inputs", "must be explicitly declared, even when empty")
	}
	seenFieldIDs := make(map[string]struct{}, len(o.Inputs))
	seenWireLocations := make(map[string]struct{}, len(o.Inputs))
	for _, input := range o.Inputs {
		if err := input.Validate(); err != nil {
			return err
		}
		if _, duplicate := seenFieldIDs[input.FieldID]; duplicate {
			return contractError(ErrDuplicateField, "inputs.field_id", "stable field ID is duplicated")
		}
		seenFieldIDs[input.FieldID] = struct{}{}
		wireKey := string(input.Location) + "\x00" + input.WireName
		if _, duplicate := seenWireLocations[wireKey]; duplicate {
			return contractError(ErrDuplicateField, "inputs.wire_name", "wire name is duplicated at one location")
		}
		seenWireLocations[wireKey] = struct{}{}
	}
	if len(o.Responses) == 0 {
		return contractError(ErrInvalidContract, "responses", "must preserve every declared response variant")
	}
	seenResponses := make(map[string]struct{}, len(o.Responses))
	for _, response := range o.Responses {
		if err := response.Validate(o.Transport.Kind); err != nil {
			return err
		}
		key := response.Status + "\x00" + response.MediaType
		if _, duplicate := seenResponses[key]; duplicate {
			return contractError(ErrDuplicateField, "responses", "status/media variant is duplicated")
		}
		seenResponses[key] = struct{}{}
	}
	if len(o.CapabilityRequirements) == 0 {
		return contractError(ErrInvalidContract, "capability_requirements", "must declare the exact capability closure")
	}
	requiredFoundations := make([]FoundationRef, 0, len(o.CapabilityRequirements))
	seenCapabilities := make(map[string]struct{}, len(o.CapabilityRequirements))
	seenFoundations := make(map[string]struct{}, len(o.CapabilityRequirements))
	for _, requirement := range o.CapabilityRequirements {
		if err := requirement.Validate(); err != nil {
			return err
		}
		key := requirement.Capability + "\x00" + requirement.Foundation.ID
		if _, duplicate := seenCapabilities[key]; duplicate {
			return contractError(ErrDuplicateField, "capability_requirements", "capability/foundation binding is duplicated")
		}
		seenCapabilities[key] = struct{}{}
		foundationKey := requirement.Foundation.ID + "@" + requirement.Foundation.Version
		if _, seen := seenFoundations[foundationKey]; !seen {
			seenFoundations[foundationKey] = struct{}{}
			requiredFoundations = append(requiredFoundations, requirement.Foundation)
		}
	}
	if err := CheckFoundationCompatibility(requiredFoundations, supported); err != nil {
		return err
	}
	if len(o.LaneEligibility) == 0 {
		return contractError(ErrInvalidContract, "lane_eligibility", "must declare at least one lane disposition")
	}
	seenLanes := make(map[Lane]struct{}, len(o.LaneEligibility))
	for _, eligibility := range o.LaneEligibility {
		if !validLane(eligibility.Lane) {
			return contractError(ErrInvalidContract, "lane_eligibility.lane", "lane is unsupported")
		}
		if _, duplicate := seenLanes[eligibility.Lane]; duplicate {
			return contractError(ErrDuplicateField, "lane_eligibility", "lane disposition is duplicated")
		}
		seenLanes[eligibility.Lane] = struct{}{}
		if eligibility.Reason != "" && !validIdentityPart(eligibility.Reason) {
			return contractError(ErrInvalidContract, "lane_eligibility.reason", "must be canonical valid UTF-8 when present")
		}
		if !eligibility.Eligible && eligibility.Reason == "" {
			return contractError(ErrInvalidContract, "lane_eligibility.reason", "ineligible lane requires an explicit reason")
		}
	}
	return nil
}

func (t Transport) Validate() error {
	switch t.Kind {
	case "http":
		if t.Method == "" || t.Method != strings.ToUpper(t.Method) {
			return contractError(ErrInvalidContract, "transport.method", "HTTP method must be explicit uppercase")
		}
		if _, ok := allowedHTTPMethods[t.Method]; !ok {
			return contractError(ErrInvalidContract, "transport.method", "HTTP method is unsupported")
		}
		if t.Path == "" || t.Path[0] != '/' || !utf8.ValidString(t.Path) {
			return contractError(ErrInvalidContract, "transport.path", "HTTP path must be a valid UTF-8 origin-relative path")
		}
		if t.Operation != "" {
			return contractError(ErrInvalidContract, "transport.operation", "HTTP transport cannot carry a native operation")
		}
	case "postgres":
		if !validIdentityPart(t.Operation) || t.Method != "" || t.Path != "" {
			return contractError(ErrInvalidContract, "transport", "PostgreSQL transport requires only a declared typed operation")
		}
	default:
		return contractError(ErrInvalidContract, "transport.kind", "transport kind is unsupported")
	}
	return nil
}

var allowedHTTPMethods = map[string]struct{}{
	http.MethodGet: {}, http.MethodHead: {}, http.MethodPost: {}, http.MethodPut: {},
	http.MethodPatch: {}, http.MethodDelete: {}, http.MethodOptions: {},
}

func (r Response) Validate(transportKind string) error {
	if !validIdentityPart(r.Status) {
		return contractError(ErrInvalidContract, "response.status", "must identify the declared response variant")
	}
	if err := validateMediaType(r.MediaType); err != nil {
		return err
	}
	if err := r.Schema.Validate(); err != nil {
		return err
	}
	if r.Schema.Type != SchemaTypeBinaryRef && !isJSONMediaType(r.MediaType) {
		return contractError(ErrIncompatibleMedia, "response.media_type", "non-binary typed response requires JSON media")
	}
	if transportKind == "postgres" && r.Status != "success" {
		return contractError(ErrInvalidContract, "response.status", "native PostgreSQL response status must be success")
	}
	return nil
}

func validSourcePointer(value string) bool {
	if value == "" {
		return false
	}
	if strings.HasPrefix(value, "#") {
		value = strings.TrimPrefix(value, "#")
	}
	return validJSONPointer(value)
}

func validLane(lane Lane) bool {
	switch lane {
	case LaneDirectRead, LaneDirectWrite, LaneETL, LaneReverseETL,
		LaneBinaryDownload, LaneBinaryUpload, LaneSyncTransport:
		return true
	default:
		return false
	}
}
