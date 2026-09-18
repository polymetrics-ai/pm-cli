package contract

import (
	"bytes"
	"encoding/json"
	"strconv"
)

const scenarioSchemaVersion = "pm.scenario/v1"

type Scenario struct {
	ScenarioVersion string                `json:"scenario_version,omitempty"`
	ScenarioID      string                `json:"scenario_id"`
	ConnectorID     string                `json:"connector_id"`
	Lane            Lane                  `json:"lane"`
	OperationID     string                `json:"operation_id"`
	SourceRefs      []SourceRef           `json:"source_refs"`
	Requirements    []ScenarioRequirement `json:"requirements"`
	Steps           []ScenarioStep        `json:"steps"`
	Assertions      []ScenarioAssertion   `json:"assertions"`
	Cleanup         ScenarioCleanup       `json:"cleanup"`
}

type ScenarioRequirement struct {
	RequirementID string `json:"requirement_id"`
	Kind          string `json:"kind"`
	Value         string `json:"value"`
}

type ScenarioStep struct {
	StepID         string                 `json:"step_id"`
	Kind           string                 `json:"kind"`
	DependsOn      []string               `json:"depends_on"`
	Action         string                 `json:"action"`
	Input          json.RawMessage        `json:"input"`
	Captures       []ScenarioCapture      `json:"captures"`
	InputBindings  []ScenarioInputBinding `json:"input_bindings,omitempty"`
	ConnectionRef  string                 `json:"connection_ref,omitempty"`
	ResultControls *ResultControls        `json:"result_controls,omitempty"`
}

type ScenarioCapture struct {
	Name        string `json:"name"`
	Pointer     string `json:"pointer"`
	Sensitivity string `json:"sensitivity"`
}

type ScenarioAssertion struct {
	AssertionID string          `json:"assertion_id"`
	StepID      string          `json:"step_id"`
	Kind        string          `json:"kind"`
	Pointer     string          `json:"pointer,omitempty"`
	Expected    json.RawMessage `json:"expected"`
}

type ScenarioCleanup struct {
	Always          bool           `json:"always"`
	OwnedIDCaptures []string       `json:"owned_id_captures"`
	Steps           []ScenarioStep `json:"steps"`
}

type ScenarioInputBinding struct {
	Pointer string `json:"pointer"`
	Capture string `json:"capture"`
}

func (scenario *Scenario) UnmarshalJSON(data []byte) error {
	type wire Scenario
	var decoded wire
	fields, err := decodeStrictNullableObject(data, &decoded,
		"scenario_id", "connector_id", "lane", "operation_id", "source_refs",
		"requirements", "steps", "assertions", "cleanup",
	)
	if err != nil {
		return err
	}
	if err := rejectExplicitNullFields(fields,
		"scenario_version", "scenario_id", "connector_id", "lane", "operation_id",
		"source_refs", "requirements", "steps", "assertions", "cleanup",
	); err != nil {
		return err
	}
	value := Scenario(decoded)
	if _, present := fields["scenario_version"]; present && value.ScenarioVersion == "" {
		return contractError(ErrInvalidContract, "scenario_version", "must name the supported scenario contract when supplied")
	}
	if err := value.Validate(); err != nil {
		return err
	}
	*scenario = value
	return nil
}

func (requirement *ScenarioRequirement) UnmarshalJSON(data []byte) error {
	type wire ScenarioRequirement
	var decoded wire
	fields, err := decodeStrictNullableObject(data, &decoded, "requirement_id", "kind", "value")
	if err != nil {
		return err
	}
	if err := rejectExplicitNullFields(fields, "requirement_id", "kind", "value"); err != nil {
		return err
	}
	value := ScenarioRequirement(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*requirement = value
	return nil
}

func (step *ScenarioStep) UnmarshalJSON(data []byte) error {
	type wire ScenarioStep
	var decoded wire
	fields, err := decodeStrictNullableObject(data, &decoded,
		"step_id", "kind", "depends_on", "action", "input", "captures",
	)
	if err != nil {
		return err
	}
	if err := rejectExplicitNullFields(fields,
		"step_id", "kind", "depends_on", "action", "captures",
		"input_bindings", "connection_ref", "result_controls",
	); err != nil {
		return err
	}
	value := ScenarioStep(decoded)
	if _, present := fields["connection_ref"]; present && value.ConnectionRef == "" {
		return contractError(ErrInvalidContract, "scenario_step.connection_ref", "must name one connection when supplied")
	}
	if raw, present := fields["result_controls"]; present {
		if err := validateScenarioResultControlsJSON(raw); err != nil {
			return err
		}
	}
	if err := value.Validate(); err != nil {
		return err
	}
	*step = value
	return nil
}

func (capture *ScenarioCapture) UnmarshalJSON(data []byte) error {
	type wire ScenarioCapture
	var decoded wire
	fields, err := decodeStrictNullableObject(data, &decoded, "name", "pointer", "sensitivity")
	if err != nil {
		return err
	}
	if err := rejectExplicitNullFields(fields, "name", "pointer", "sensitivity"); err != nil {
		return err
	}
	value := ScenarioCapture(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*capture = value
	return nil
}

func (assertion *ScenarioAssertion) UnmarshalJSON(data []byte) error {
	type wire ScenarioAssertion
	var decoded wire
	fields, err := decodeStrictNullableObject(data, &decoded, "assertion_id", "step_id", "kind", "expected")
	if err != nil {
		return err
	}
	if err := rejectExplicitNullFields(fields, "assertion_id", "step_id", "kind", "pointer"); err != nil {
		return err
	}
	value := ScenarioAssertion(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*assertion = value
	return nil
}

func (cleanup *ScenarioCleanup) UnmarshalJSON(data []byte) error {
	type wire ScenarioCleanup
	var decoded wire
	fields, err := decodeStrictNullableObject(data, &decoded, "always", "owned_id_captures", "steps")
	if err != nil {
		return err
	}
	if err := rejectExplicitNullFields(fields, "always", "owned_id_captures", "steps"); err != nil {
		return err
	}
	value := ScenarioCleanup(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*cleanup = value
	return nil
}

func (binding *ScenarioInputBinding) UnmarshalJSON(data []byte) error {
	type wire ScenarioInputBinding
	var decoded wire
	fields, err := decodeStrictNullableObject(data, &decoded, "pointer", "capture")
	if err != nil {
		return err
	}
	if err := rejectExplicitNullFields(fields, "pointer", "capture"); err != nil {
		return err
	}
	value := ScenarioInputBinding(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*binding = value
	return nil
}

func (scenario Scenario) Validate() error {
	if scenario.ScenarioVersion != "" && scenario.ScenarioVersion != scenarioSchemaVersion {
		return contractError(ErrInvalidContract, "scenario_version", "scenario contract version is unsupported")
	}
	if !validBoundedIdentityPart(scenario.ScenarioID, 1024) {
		return contractError(ErrInvalidIdentity, "scenario.scenario_id", "must identify one bounded scenario")
	}
	if !validBoundedIdentityPart(scenario.ConnectorID, 1024) {
		return contractError(ErrInvalidIdentity, "scenario.connector_id", "must identify one bounded connector")
	}
	if err := ValidateLane(scenario.Lane); err != nil {
		return err
	}
	if !validBoundedIdentityPart(scenario.OperationID, 1024) {
		return contractError(ErrInvalidIdentity, "scenario.operation_id", "must identify one bounded operation")
	}
	if scenario.SourceRefs == nil || len(scenario.SourceRefs) == 0 {
		return contractError(ErrInvalidContract, "scenario.source_refs", "must preserve at least one exact source reference")
	}
	seenSourceRefs := make(map[string]struct{}, len(scenario.SourceRefs))
	for _, reference := range scenario.SourceRefs {
		if !validBoundedIdentityPart(reference.SourceID, 1024) || !validSourcePointer(reference.Pointer) {
			return contractError(ErrInvalidContract, "scenario.source_refs", "contains an incomplete or malformed source identity")
		}
		key := reference.SourceID + "\x00" + reference.Pointer
		if _, duplicate := seenSourceRefs[key]; duplicate {
			return contractError(ErrDuplicateField, "scenario.source_refs", "contains a duplicate source reference")
		}
		seenSourceRefs[key] = struct{}{}
	}
	if scenario.Requirements == nil {
		return contractError(ErrInvalidContract, "scenario.requirements", "must be explicitly present, even when empty")
	}
	seenRequirements := make(map[string]struct{}, len(scenario.Requirements))
	for _, requirement := range scenario.Requirements {
		if err := requirement.Validate(); err != nil {
			return err
		}
		if _, duplicate := seenRequirements[requirement.RequirementID]; duplicate {
			return contractError(ErrDuplicateField, "scenario.requirements.requirement_id", "requirement identity is duplicated")
		}
		seenRequirements[requirement.RequirementID] = struct{}{}
	}
	if scenario.Steps == nil || len(scenario.Steps) == 0 {
		return contractError(ErrInvalidContract, "scenario.steps", "must contain at least one ordered step")
	}
	for _, step := range scenario.Steps {
		if err := step.Validate(); err != nil {
			return err
		}
	}
	if scenario.Assertions == nil || len(scenario.Assertions) == 0 {
		return contractError(ErrInvalidContract, "scenario.assertions", "must contain at least one independent assertion")
	}
	if err := scenario.Cleanup.Validate(); err != nil {
		return err
	}
	return validateScenarioGraph(scenario)
}

func (requirement ScenarioRequirement) Validate() error {
	if !validBoundedIdentityPart(requirement.RequirementID, 1024) {
		return contractError(ErrInvalidIdentity, "scenario_requirement.requirement_id", "must identify one bounded requirement")
	}
	switch requirement.Kind {
	case "capability", "fixture", "secret_reference", "foundation", "service":
	default:
		return contractError(ErrInvalidContract, "scenario_requirement.kind", "requirement kind is unsupported")
	}
	if !validBoundedIdentityPart(requirement.Value, 1024) {
		return contractError(ErrInvalidContract, "scenario_requirement.value", "must preserve one bounded requirement value")
	}
	return nil
}

func (step ScenarioStep) Validate() error {
	if !validBoundedIdentityPart(step.StepID, 1024) {
		return contractError(ErrInvalidIdentity, "scenario_step.step_id", "must identify one bounded step")
	}
	switch step.Kind {
	case "setup", "invoke", "readback", "inspect", "cleanup":
	default:
		return contractError(ErrInvalidContract, "scenario_step.kind", "step kind is unsupported")
	}
	if step.DependsOn == nil {
		return contractError(ErrInvalidContract, "scenario_step.depends_on", "must be explicitly present, even when empty")
	}
	seenDependencies := make(map[string]struct{}, len(step.DependsOn))
	for _, dependency := range step.DependsOn {
		if !validBoundedIdentityPart(dependency, 1024) {
			return contractError(ErrInvalidIdentity, "scenario_step.depends_on", "contains an invalid step identity")
		}
		if _, duplicate := seenDependencies[dependency]; duplicate {
			return contractError(ErrDuplicateField, "scenario_step.depends_on", "dependency is duplicated")
		}
		seenDependencies[dependency] = struct{}{}
	}
	if !validBoundedIdentityPart(step.Action, 1024) {
		return contractError(ErrInvalidContract, "scenario_step.action", "must name one bounded action")
	}
	if err := validateNativeJSON(step.Input, "scenario_step.input"); err != nil {
		return err
	}
	if step.Captures == nil {
		return contractError(ErrInvalidContract, "scenario_step.captures", "must be explicitly present, even when empty")
	}
	for _, capture := range step.Captures {
		if err := capture.Validate(); err != nil {
			return err
		}
	}
	for index, binding := range step.InputBindings {
		if err := binding.Validate(); err != nil {
			return err
		}
		for prior := range index {
			if requestPointersOverlap(step.InputBindings[prior].Pointer, binding.Pointer) {
				return contractError(ErrDuplicateField, "scenario_step.input_bindings.pointer", "input binding pointers are duplicate or overlapping")
			}
		}
	}
	if step.ConnectionRef != "" && !validBoundedIdentityPart(step.ConnectionRef, 1024) {
		return contractError(ErrInvalidIdentity, "scenario_step.connection_ref", "must name one bounded connection")
	}
	if step.ResultControls != nil {
		if err := step.ResultControls.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (capture ScenarioCapture) Validate() error {
	if !validBoundedIdentityPart(capture.Name, 1024) {
		return contractError(ErrInvalidIdentity, "scenario_capture.name", "must identify one bounded capture")
	}
	if len(capture.Pointer) > 4096 || !validJSONPointer(capture.Pointer) {
		return contractError(ErrInvalidContract, "scenario_capture.pointer", "must be a bounded RFC 6901 pointer")
	}
	switch capture.Sensitivity {
	case "public", "secret_reference":
	default:
		return contractError(ErrInvalidContract, "scenario_capture.sensitivity", "capture sensitivity is unsupported")
	}
	return nil
}

func (assertion ScenarioAssertion) Validate() error {
	if !validBoundedIdentityPart(assertion.AssertionID, 1024) {
		return contractError(ErrInvalidIdentity, "scenario_assertion.assertion_id", "must identify one bounded assertion")
	}
	if !validBoundedIdentityPart(assertion.StepID, 1024) {
		return contractError(ErrInvalidIdentity, "scenario_assertion.step_id", "must identify one bounded step")
	}
	switch assertion.Kind {
	case "equals", "contains", "absent", "row", "effect", "sha256", "cursor_transition":
	default:
		return contractError(ErrInvalidContract, "scenario_assertion.kind", "assertion kind is unsupported")
	}
	if len(assertion.Pointer) > 4096 || !validJSONPointer(assertion.Pointer) {
		return contractError(ErrInvalidContract, "scenario_assertion.pointer", "must be a bounded RFC 6901 pointer")
	}
	if err := validateNativeJSON(assertion.Expected, "scenario_assertion.expected"); err != nil {
		return err
	}
	if assertion.Kind == "sha256" {
		var expected string
		if err := json.Unmarshal(assertion.Expected, &expected); err != nil || SourceHash(expected).Validate() != nil {
			return contractError(ErrInvalidContract, "scenario_assertion.expected", "sha256 expectation must be one exact source hash")
		}
	}
	return nil
}

func (cleanup ScenarioCleanup) Validate() error {
	if !cleanup.Always {
		return contractError(ErrInvalidContract, "scenario_cleanup.always", "cleanup must remain enabled after every outcome")
	}
	if cleanup.OwnedIDCaptures == nil {
		return contractError(ErrInvalidContract, "scenario_cleanup.owned_id_captures", "must be explicitly present, even when empty")
	}
	seenOwned := make(map[string]struct{}, len(cleanup.OwnedIDCaptures))
	for _, name := range cleanup.OwnedIDCaptures {
		if !validBoundedIdentityPart(name, 1024) {
			return contractError(ErrInvalidIdentity, "scenario_cleanup.owned_id_captures", "contains an invalid capture identity")
		}
		if _, duplicate := seenOwned[name]; duplicate {
			return contractError(ErrDuplicateField, "scenario_cleanup.owned_id_captures", "owned capture is duplicated")
		}
		seenOwned[name] = struct{}{}
	}
	if cleanup.Steps == nil {
		return contractError(ErrInvalidContract, "scenario_cleanup.steps", "must be explicitly present, even when empty")
	}
	for _, step := range cleanup.Steps {
		if err := step.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (binding ScenarioInputBinding) Validate() error {
	if len(binding.Pointer) > 4096 || !validJSONPointer(binding.Pointer) {
		return contractError(ErrInvalidContract, "scenario_input_binding.pointer", "must be a bounded RFC 6901 pointer")
	}
	if !validBoundedIdentityPart(binding.Capture, 1024) {
		return contractError(ErrInvalidIdentity, "scenario_input_binding.capture", "must identify one bounded capture")
	}
	return nil
}

type scenarioCaptureOrigin struct {
	stepID      string
	stepKind    string
	sensitivity string
	main        bool
}

func validateScenarioGraph(scenario Scenario) error {
	stepAncestors := make(map[string]map[string]struct{}, len(scenario.Steps)+len(scenario.Cleanup.Steps))
	mainSteps := make(map[string]struct{}, len(scenario.Steps))
	captures := make(map[string]scenarioCaptureOrigin)

	processStep := func(step ScenarioStep, main bool, owned map[string]struct{}) error {
		if _, duplicate := stepAncestors[step.StepID]; duplicate {
			return contractError(ErrDuplicateField, "scenario.steps.step_id", "step identity is duplicated")
		}
		if main && step.Kind == "cleanup" {
			return contractError(ErrInvalidContract, "scenario.steps.kind", "cleanup steps belong only to the cleanup plan")
		}
		if !main && step.Kind != "cleanup" {
			return contractError(ErrInvalidContract, "scenario.cleanup.steps.kind", "cleanup plan contains a non-cleanup step")
		}
		if !main && len(step.InputBindings) == 0 {
			return contractError(ErrInvalidContract, "scenario.cleanup.steps.input_bindings", "cleanup must bind at least one explicitly owned setup capture")
		}
		ancestors := make(map[string]struct{})
		for _, dependency := range step.DependsOn {
			dependencyAncestors, known := stepAncestors[dependency]
			if !known {
				return contractError(ErrInvalidContract, "scenario_step.depends_on", "dependency is unknown, cyclic, or later in the ordered DAG")
			}
			ancestors[dependency] = struct{}{}
			for ancestor := range dependencyAncestors {
				ancestors[ancestor] = struct{}{}
			}
		}
		for _, binding := range step.InputBindings {
			origin, known := captures[binding.Capture]
			if !known {
				return contractError(ErrInvalidContract, "scenario_step.input_bindings.capture", "binding references an unknown or later capture")
			}
			if _, ancestor := ancestors[origin.stepID]; !ancestor {
				return contractError(ErrInvalidContract, "scenario_step.input_bindings.capture", "binding capture does not come from an explicit ancestor")
			}
			if origin.sensitivity != "public" {
				return contractError(ErrInvalidContract, "scenario_step.input_bindings.capture", "secret references cannot be materialized as raw input values")
			}
			if !main {
				if _, authorized := owned[binding.Capture]; !authorized {
					return contractError(ErrInvalidContract, "scenario.cleanup.steps.input_bindings", "cleanup can bind only an explicitly owned setup capture")
				}
			}
			exists, err := nativeJSONPointerExists(step.Input, binding.Pointer)
			if err != nil {
				return err
			}
			if !exists {
				return contractError(ErrInvalidContract, "scenario_step.input_bindings.pointer", "binding pointer does not select an existing input value")
			}
		}
		stepAncestors[step.StepID] = ancestors
		if main {
			mainSteps[step.StepID] = struct{}{}
		}
		for _, capture := range step.Captures {
			if _, duplicate := captures[capture.Name]; duplicate {
				return contractError(ErrDuplicateField, "scenario.steps.captures.name", "capture name is duplicated across the scenario")
			}
			captures[capture.Name] = scenarioCaptureOrigin{
				stepID: step.StepID, stepKind: step.Kind, sensitivity: capture.Sensitivity, main: main,
			}
		}
		return nil
	}

	for _, step := range scenario.Steps {
		if err := processStep(step, true, nil); err != nil {
			return err
		}
	}

	owned := make(map[string]struct{}, len(scenario.Cleanup.OwnedIDCaptures))
	for _, capture := range scenario.Cleanup.OwnedIDCaptures {
		origin, known := captures[capture]
		if !known || !origin.main || origin.stepKind != "setup" || origin.sensitivity != "public" {
			return contractError(ErrInvalidContract, "scenario.cleanup.owned_id_captures", "ownership must originate from a declared public setup capture")
		}
		owned[capture] = struct{}{}
	}
	for _, step := range scenario.Cleanup.Steps {
		if err := processStep(step, false, owned); err != nil {
			return err
		}
	}

	seenAssertions := make(map[string]struct{}, len(scenario.Assertions))
	for _, assertion := range scenario.Assertions {
		if err := assertion.Validate(); err != nil {
			return err
		}
		if _, duplicate := seenAssertions[assertion.AssertionID]; duplicate {
			return contractError(ErrDuplicateField, "scenario.assertions.assertion_id", "assertion identity is duplicated")
		}
		seenAssertions[assertion.AssertionID] = struct{}{}
		if _, known := mainSteps[assertion.StepID]; !known {
			return contractError(ErrInvalidContract, "scenario.assertions.step_id", "assertion must reference one declared non-cleanup step")
		}
	}
	return nil
}

func validateNativeJSON(document json.RawMessage, path string) error {
	_, err := decodeNativeJSONValue(document, path)
	return err
}

func decodeNativeJSONValue(document json.RawMessage, path string) (any, error) {
	if document == nil {
		return nil, contractError(ErrInvalidContract, path, "required native JSON value is absent")
	}
	var value any
	if err := decodeStrictJSON(document, &value); err != nil {
		return nil, contractError(ErrInvalidContract, path, "native JSON value is malformed, ambiguous, or outside bounds")
	}
	return value, nil
}

func nativeJSONPointerExists(document json.RawMessage, pointer string) (bool, error) {
	current, err := decodeNativeJSONValue(document, "scenario_step.input")
	if err != nil {
		return false, err
	}
	for _, token := range decodeJSONPointerParts(pointer) {
		switch value := current.(type) {
		case map[string]any:
			next, present := value[token]
			if !present {
				return false, nil
			}
			current = next
		case []any:
			if token == "" || (len(token) > 1 && token[0] == '0') {
				return false, nil
			}
			index, err := strconv.ParseUint(token, 10, 63)
			if err != nil || index >= uint64(len(value)) {
				return false, nil
			}
			current = value[index]
		default:
			return false, nil
		}
	}
	return true, nil
}

func decodeStrictNullableObject(data []byte, target any, required ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := decodeStrictJSON(data, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, contractError(ErrInvalidContract, "$", "document must be an object")
	}
	for _, field := range required {
		if _, present := fields[field]; !present {
			return nil, contractError(ErrInvalidContract, field, "required field is absent")
		}
	}
	if err := decodeStrictJSON(data, target); err != nil {
		return nil, err
	}
	return fields, nil
}

func rejectExplicitNullFields(fields map[string]json.RawMessage, names ...string) error {
	for _, name := range names {
		raw, present := fields[name]
		if present && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return contractError(ErrInvalidContract, name, "field cannot be null")
		}
	}
	return nil
}

func validateScenarioResultControlsJSON(data json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := decodeStrictJSON(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return contractError(ErrInvalidContract, "scenario_step.result_controls", "must be an object when supplied")
	}
	if err := rejectExplicitNullFields(fields, "selection", "filter", "total_record_cap", "continuation"); err != nil {
		return err
	}
	if filter, present := fields["filter"]; present {
		var filterFields map[string]json.RawMessage
		if err := decodeStrictJSON(filter, &filterFields); err != nil {
			return err
		}
		if err := rejectExplicitNullFields(filterFields, "path", "operator", "value"); err != nil {
			return err
		}
	}
	return nil
}
