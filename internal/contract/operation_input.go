package contract

import (
	"math/big"
	"reflect"
	"strings"
)

// RequestVariantProjection is the provenance-free request-body shape exposed
// to thin clients. FieldIDs bind back to the descriptor's exact bindings;
// they are never inferred from flag or wire names.
type RequestVariantProjection struct {
	VariantID string   `json:"variant_id"`
	MediaType string   `json:"media_type"`
	Required  bool     `json:"required"`
	Schema    Schema   `json:"schema"`
	FieldIDs  []string `json:"field_ids"`
}

// OperationRequestProjection retains the request-variant membership and body
// optionality that a flat command field list cannot represent.
type OperationRequestProjection struct {
	RequestMediaFieldID string                     `json:"request_media_field_id,omitempty"`
	Variants            []RequestVariantProjection `json:"variants"`
}

// ResolvedRequest is the one preflight result consumed by request encoders.
// A nil Variant and absent Body mean that the operation declares no request
// body, or that an optional request body was not supplied.
type ResolvedRequest struct {
	Variant *RequestVariant
	Body    PresentValue
}

// ValidateOperationInput validates the logical FieldID envelope without
// applying source defaults or rewriting supplied values.
func ValidateOperationInput(operation Operation, input PresentValue) error {
	_, err := ResolveOperationRequest(operation, input)
	return err
}

// ResolveOperationRequest validates common inputs, selects at most one
// source-declared request media variant, and assembles its complete root body.
// Selection and validation happen before a runtime may perform provider I/O.
func ResolveOperationRequest(operation Operation, input PresentValue) (ResolvedRequest, error) {
	common, variantByField, err := validateOperationRequestShape(operation)
	if err != nil {
		return ResolvedRequest{}, err
	}
	variants := requestVariantShapes(operation.RequestVariants)
	selected, body, err := resolveOperationRequestInput(
		input,
		common,
		variantByField,
		variants,
		operation.RequestMediaFieldID,
		false,
	)
	if err != nil {
		return ResolvedRequest{}, err
	}
	var variant *RequestVariant
	if selected >= 0 {
		variant = &operation.RequestVariants[selected]
	}
	return ResolvedRequest{Variant: variant, Body: body}, nil
}

// ProjectOperationRequest copies the admitted operation's request semantics
// into the minimal safe catalog projection.
func ProjectOperationRequest(operation Operation) (OperationRequestProjection, error) {
	if _, _, err := validateOperationRequestShape(operation); err != nil {
		return OperationRequestProjection{}, err
	}
	projected := OperationRequestProjection{
		RequestMediaFieldID: operation.RequestMediaFieldID,
		Variants:            make([]RequestVariantProjection, len(operation.RequestVariants)),
	}
	for index, variant := range operation.RequestVariants {
		fieldIDs := make([]string, len(variant.Inputs))
		for fieldIndex, binding := range variant.Inputs {
			fieldIDs[fieldIndex] = binding.FieldID
		}
		projected.Variants[index] = RequestVariantProjection{
			VariantID: variant.VariantID,
			MediaType: variant.MediaType,
			Required:  variant.Required,
			Schema:    cloneRequestSchema(variant.Schema),
			FieldIDs:  fieldIDs,
		}
	}
	return projected, nil
}

// Clone returns a deep copy suitable for crossing a catalog adapter boundary.
func (projection OperationRequestProjection) Clone() OperationRequestProjection {
	cloned := OperationRequestProjection{
		RequestMediaFieldID: projection.RequestMediaFieldID,
		Variants:            make([]RequestVariantProjection, len(projection.Variants)),
	}
	if projection.Variants == nil {
		cloned.Variants = nil
	}
	for index, variant := range projection.Variants {
		fieldIDs := make([]string, len(variant.FieldIDs))
		copy(fieldIDs, variant.FieldIDs)
		if variant.FieldIDs == nil {
			fieldIDs = nil
		}
		cloned.Variants[index] = RequestVariantProjection{
			VariantID: variant.VariantID,
			MediaType: variant.MediaType,
			Required:  variant.Required,
			Schema:    cloneRequestSchema(variant.Schema),
			FieldIDs:  fieldIDs,
		}
	}
	return cloned
}

// Validate verifies that a catalog projection preserves every body binding
// exactly once and retains the operation's media-selection contract.
func (projection OperationRequestProjection) Validate(fields []FieldBinding) error {
	_, _, _, err := validateOperationRequestProjection(fields, projection)
	return err
}

// ValidateInput applies the same request selection and body assembly semantics
// as ResolveOperationRequest without requiring source provenance in a client
// catalog descriptor.
func (projection OperationRequestProjection) ValidateInput(fields []FieldBinding, input PresentValue) error {
	common, variantByField, variants, err := validateOperationRequestProjection(fields, projection)
	if err != nil {
		return err
	}
	_, _, err = resolveOperationRequestInput(input, common, variantByField, variants, projection.RequestMediaFieldID, false)
	return err
}

// ValidateSuppliedInput validates every explicitly supplied logical field and
// observable request-variant conflict while deferring only required fields
// that are absent. Server restoration must follow with ordinary full
// ValidateInput; this method does not relax or rewrite projected bindings.
func (projection OperationRequestProjection) ValidateSuppliedInput(fields []FieldBinding, input PresentValue) error {
	common, variantByField, variants, err := validateOperationRequestProjection(fields, projection)
	if err != nil {
		return err
	}
	_, _, err = resolveOperationRequestInput(input, common, variantByField, variants, projection.RequestMediaFieldID, true)
	return err
}

type requestVariantShape struct {
	variantID string
	mediaType string
	required  bool
	schema    Schema
	inputs    []FieldBinding
}

func requestVariantShapes(variants []RequestVariant) []requestVariantShape {
	shapes := make([]requestVariantShape, len(variants))
	for index, variant := range variants {
		shapes[index] = requestVariantShape{
			variantID: variant.VariantID,
			mediaType: variant.MediaType,
			required:  variant.Required,
			schema:    variant.Schema,
			inputs:    variant.Inputs,
		}
	}
	return shapes
}

func resolveOperationRequestInput(
	input PresentValue,
	common map[string]FieldBinding,
	variantByField map[string]int,
	variants []requestVariantShape,
	requestMediaFieldID string,
	deferOmittedRequired bool,
) (int, PresentValue, error) {
	if err := input.Validate(); err != nil {
		return -1, PresentValue{}, contractError(ErrInvalidValue, "input", "contains an invalid typed value")
	}
	if input.presence != PresenceValue || input.kind != ValueKindObject {
		return -1, PresentValue{}, contractError(ErrInvalidValue, "input", "must be a present logical field-ID object")
	}

	for fieldID := range input.fields {
		if _, ok := common[fieldID]; ok {
			continue
		}
		if _, ok := variantByField[fieldID]; !ok {
			return -1, PresentValue{}, contractError(ErrUnknownField, "input.fields", "contains an undeclared logical field ID")
		}
	}
	for fieldID, binding := range common {
		value, present := input.fields[fieldID]
		if !deferOmittedRequired && binding.Required && (!present || value.presence == PresenceAbsent) {
			return -1, PresentValue{}, contractError(ErrInvalidValue, "input.fields", "required common logical field is absent")
		}
		if present && !operationInputMatchesSchema(value, binding.Schema, true) {
			return -1, PresentValue{}, contractError(ErrInvalidValue, "input.fields", "common logical field does not match its declared schema")
		}
	}

	selected := -1
	explicitMediaSelection := false
	suppliedByVariant := make([]bool, len(variants))
	for fieldID, variantIndex := range variantByField {
		if value, present := input.fields[fieldID]; present && value.presence != PresenceAbsent {
			suppliedByVariant[variantIndex] = true
		}
	}

	switch len(variants) {
	case 0:
		return -1, AbsentValue(), nil
	case 1:
		if suppliedByVariant[0] {
			selected = 0
		} else if variants[0].required && !deferOmittedRequired {
			return -1, PresentValue{}, contractError(ErrInvalidValue, "input.fields", "required request body is absent")
		} else {
			return -1, AbsentValue(), nil
		}
	default:
		selector, present := input.fields[requestMediaFieldID]
		if present && selector.presence != PresenceAbsent {
			mediaType, ok := selector.TextValue()
			if !ok {
				return -1, PresentValue{}, contractError(ErrInvalidValue, "input.fields", "request media selector must be a present string")
			}
			for index := range variants {
				if variants[index].mediaType == mediaType {
					selected = index
					break
				}
			}
			if selected < 0 {
				return -1, PresentValue{}, contractError(ErrInvalidValue, "input.fields", "request media selector does not identify a declared variant")
			}
			explicitMediaSelection = true
		} else {
			for index, supplied := range suppliedByVariant {
				if supplied {
					if !deferOmittedRequired || selected >= 0 {
						return -1, PresentValue{}, contractError(ErrInvalidValue, "input.fields", "request body media is ambiguous")
					}
					selected = index
				}
			}
			if selected < 0 {
				if !deferOmittedRequired {
					for index := range variants {
						if variants[index].required {
							return -1, PresentValue{}, contractError(ErrInvalidValue, "input.fields", "required request body media is absent")
						}
					}
				}
				return -1, AbsentValue(), nil
			}
		}
		for index, supplied := range suppliedByVariant {
			if supplied && index != selected {
				return -1, PresentValue{}, contractError(ErrInvalidValue, "input.fields", "request contains fields from a non-selected media variant")
			}
		}
	}

	if deferOmittedRequired {
		if err := validateSuppliedRequestBody(variants[selected], input.fields); err != nil {
			return -1, PresentValue{}, err
		}
		return selected, AbsentValue(), nil
	}

	body, err := assembleRequestBody(variants[selected], input.fields, explicitMediaSelection)
	if err != nil {
		return -1, PresentValue{}, err
	}
	return selected, body, nil
}

func validateOperationRequestShape(operation Operation) (map[string]FieldBinding, map[string]int, error) {
	if operation.Inputs == nil {
		return nil, nil, contractError(ErrInvalidContract, "operation.inputs", "input bindings must be explicitly declared")
	}
	if operation.RequestVariants == nil {
		return nil, nil, contractError(ErrInvalidContract, "operation.request_variants", "request variants must be explicitly declared")
	}
	common := make(map[string]FieldBinding, len(operation.Inputs))
	for index := range operation.Inputs {
		binding := operation.Inputs[index]
		if err := binding.Validate(); err != nil {
			return nil, nil, err
		}
		if binding.Location == FieldLocationBody {
			return nil, nil, contractError(ErrInvalidContract, "operation.inputs", "request-body bindings must be local to a request variant")
		}
		if _, duplicate := common[binding.FieldID]; duplicate {
			return nil, nil, contractError(ErrDuplicateField, "operation.inputs", "logical field ID is duplicated")
		}
		common[binding.FieldID] = binding
	}

	variantByField := make(map[string]int)
	seenVariantIDs := make(map[string]struct{}, len(operation.RequestVariants))
	seenMediaTypes := make(map[string]struct{}, len(operation.RequestVariants))
	for variantIndex := range operation.RequestVariants {
		variant := operation.RequestVariants[variantIndex]
		if err := variant.Validate(); err != nil {
			return nil, nil, err
		}
		if _, duplicate := seenVariantIDs[variant.VariantID]; duplicate {
			return nil, nil, contractError(ErrDuplicateField, "operation.request_variants", "request variant identity is duplicated")
		}
		seenVariantIDs[variant.VariantID] = struct{}{}
		if _, duplicate := seenMediaTypes[variant.MediaType]; duplicate {
			return nil, nil, contractError(ErrDuplicateField, "operation.request_variants", "request media type is duplicated")
		}
		seenMediaTypes[variant.MediaType] = struct{}{}
		for _, binding := range variant.Inputs {
			if _, collision := common[binding.FieldID]; collision {
				return nil, nil, contractError(ErrDuplicateField, "operation.request_variants", "variant-local logical field ID collides with a common input")
			}
			if _, collision := variantByField[binding.FieldID]; collision {
				return nil, nil, contractError(ErrDuplicateField, "operation.request_variants", "variant-local logical field ID collides within the operation")
			}
			variantByField[binding.FieldID] = variantIndex
		}
	}
	if len(operation.RequestVariants) == 1 && operation.RequestVariants[0].Required &&
		!singleRequiredVariantCanBeRepresented(operation.RequestVariants[0]) {
		return nil, nil, contractError(ErrInvalidContract, "operation.request_variants.inputs", "single required request body cannot represent its required root")
	}
	if err := validateRequestMediaSelector(operation, common); err != nil {
		return nil, nil, err
	}
	return common, variantByField, nil
}

func validateOperationRequestProjection(
	fields []FieldBinding,
	projection OperationRequestProjection,
) (map[string]FieldBinding, map[string]int, []requestVariantShape, error) {
	if fields == nil {
		return nil, nil, nil, contractError(ErrInvalidContract, "command.fields", "must be explicitly declared")
	}
	if projection.Variants == nil {
		return nil, nil, nil, contractError(ErrInvalidContract, "command.request_body.variants", "must be explicitly declared")
	}

	common := make(map[string]FieldBinding, len(fields))
	body := make(map[string]FieldBinding)
	seenFields := make(map[string]struct{}, len(fields))
	for _, binding := range fields {
		if err := binding.Validate(); err != nil {
			return nil, nil, nil, err
		}
		if _, duplicate := seenFields[binding.FieldID]; duplicate {
			return nil, nil, nil, contractError(ErrDuplicateField, "command.fields", "logical field ID is duplicated")
		}
		seenFields[binding.FieldID] = struct{}{}
		if binding.Location == FieldLocationBody {
			body[binding.FieldID] = binding
		} else {
			common[binding.FieldID] = binding
		}
	}
	variants := make([]requestVariantShape, len(projection.Variants))
	variantByField := make(map[string]int, len(body))
	seenVariantIDs := make(map[string]struct{}, len(projection.Variants))
	seenMediaTypes := make(map[string]struct{}, len(projection.Variants))
	requestVariants := make([]RequestVariant, len(projection.Variants))
	for variantIndex, projected := range projection.Variants {
		if !validIdentityPart(projected.VariantID) {
			return nil, nil, nil, contractError(ErrInvalidContract, "command.request_body.variants.variant_id", "must be a stable request variant identity")
		}
		if err := validateMediaType(projected.MediaType); err != nil {
			return nil, nil, nil, err
		}
		if err := projected.Schema.Validate(); err != nil {
			return nil, nil, nil, err
		}
		if projected.FieldIDs == nil {
			return nil, nil, nil, contractError(ErrInvalidContract, "command.request_body.variants.field_ids", "must be explicitly declared")
		}
		if _, duplicate := seenVariantIDs[projected.VariantID]; duplicate {
			return nil, nil, nil, contractError(ErrDuplicateField, "command.request_body.variants", "request variant identity is duplicated")
		}
		seenVariantIDs[projected.VariantID] = struct{}{}
		if _, duplicate := seenMediaTypes[projected.MediaType]; duplicate {
			return nil, nil, nil, contractError(ErrDuplicateField, "command.request_body.variants", "request media type is duplicated")
		}
		seenMediaTypes[projected.MediaType] = struct{}{}

		inputs := make([]FieldBinding, len(projected.FieldIDs))
		seenFields := make(map[string]struct{}, len(projected.FieldIDs))
		seenPointers := make(map[string]struct{}, len(projected.FieldIDs))
		for fieldIndex, fieldID := range projected.FieldIDs {
			binding, present := body[fieldID]
			if !present {
				return nil, nil, nil, contractError(ErrInvalidContract, "command.request_body.variants.field_ids", "must identify a projected request-body field")
			}
			if _, duplicate := seenFields[fieldID]; duplicate {
				return nil, nil, nil, contractError(ErrDuplicateField, "command.request_body.variants.field_ids", "field is duplicated within a request variant")
			}
			if _, assigned := variantByField[fieldID]; assigned {
				return nil, nil, nil, contractError(ErrDuplicateField, "command.request_body.variants.field_ids", "field belongs to more than one request variant")
			}
			if _, duplicate := seenPointers[binding.JSONPointer]; duplicate {
				return nil, nil, nil, contractError(ErrDuplicateField, "command.request_body.variants.field_ids", "request-body location is duplicated")
			}
			if !requestBindingMatchesRootSchema(binding, projected.Schema) {
				return nil, nil, nil, contractError(ErrInvalidContract, "command.request_body.variants.schema", "field schema does not match its full-root request schema location")
			}
			seenFields[fieldID] = struct{}{}
			seenPointers[binding.JSONPointer] = struct{}{}
			variantByField[fieldID] = variantIndex
			inputs[fieldIndex] = binding
		}
		variants[variantIndex] = requestVariantShape{
			variantID: projected.VariantID,
			mediaType: projected.MediaType,
			required:  projected.Required,
			schema:    projected.Schema,
			inputs:    inputs,
		}
		requestVariants[variantIndex] = RequestVariant{
			VariantID: projected.VariantID,
			MediaType: projected.MediaType,
			Required:  projected.Required,
			Schema:    projected.Schema,
			Inputs:    inputs,
		}
	}
	for fieldID := range body {
		if _, assigned := variantByField[fieldID]; !assigned {
			return nil, nil, nil, contractError(ErrInvalidContract, "command.request_body.variants.field_ids", "request-body field has no variant membership")
		}
	}
	if len(requestVariants) == 1 && requestVariants[0].Required &&
		!singleRequiredVariantCanBeRepresented(requestVariants[0]) {
		return nil, nil, nil, contractError(ErrInvalidContract, "command.request_body.variants.field_ids", "single required request body cannot represent its required root")
	}
	if err := validateRequestMediaSelector(Operation{
		RequestVariants:     requestVariants,
		RequestMediaFieldID: projection.RequestMediaFieldID,
	}, common); err != nil {
		return nil, nil, nil, err
	}
	return common, variantByField, variants, nil
}

func cloneRequestSchema(schema Schema) Schema {
	cloned := schema
	if schema.Required != nil {
		cloned.Required = make([]string, len(schema.Required))
		copy(cloned.Required, schema.Required)
	}
	if schema.Enum != nil {
		cloned.Enum = make([]PresentValue, len(schema.Enum))
		copy(cloned.Enum, schema.Enum)
	}
	if schema.OneOf != nil {
		cloned.OneOf = make([]Schema, len(schema.OneOf))
		for index, variant := range schema.OneOf {
			cloned.OneOf[index] = cloneRequestSchema(variant)
		}
	}
	if schema.AllOf != nil {
		cloned.AllOf = make([]Schema, len(schema.AllOf))
		for index, component := range schema.AllOf {
			cloned.AllOf[index] = cloneRequestSchema(component)
		}
	}
	if schema.Properties != nil {
		cloned.Properties = make(map[string]Schema, len(schema.Properties))
		for name, property := range schema.Properties {
			cloned.Properties[name] = cloneRequestSchema(property)
		}
	}
	if schema.Items != nil {
		items := cloneRequestSchema(*schema.Items)
		cloned.Items = &items
	}
	if schema.Minimum != nil {
		minimum := *schema.Minimum
		cloned.Minimum = &minimum
	}
	if schema.Maximum != nil {
		maximum := *schema.Maximum
		cloned.Maximum = &maximum
	}
	return cloned
}

func validateSuppliedRequestBody(variant requestVariantShape, fields map[string]PresentValue) error {
	_, err := validatedSuppliedRequestBindings(variant, fields, false)
	return err
}

func validatedSuppliedRequestBindings(
	variant requestVariantShape,
	fields map[string]PresentValue,
	requireRequired bool,
) ([]FieldBinding, error) {
	present := make([]FieldBinding, 0, len(variant.inputs))
	for _, binding := range variant.inputs {
		value, supplied := fields[binding.FieldID]
		if requireRequired && binding.Required && (!supplied || value.presence == PresenceAbsent) {
			return nil, contractError(ErrInvalidValue, "input.fields", "required variant-local logical field is absent")
		}
		if !supplied || value.presence == PresenceAbsent {
			continue
		}
		if !operationInputMatchesSchema(value, binding.Schema, false) {
			return nil, contractError(ErrInvalidValue, "input.fields", "variant-local logical field does not match its declared schema")
		}
		present = append(present, binding)
	}

	for left := range present {
		for offset := range present[left+1:] {
			right := left + 1 + offset
			if requestPointersOverlap(present[left].JSONPointer, present[right].JSONPointer) {
				return nil, contractError(ErrInvalidValue, "input.fields", "request body contains conflicting whole or nested assignments")
			}
		}
	}
	return present, nil
}

func assembleRequestBody(variant requestVariantShape, fields map[string]PresentValue, explicitMediaSelection bool) (PresentValue, error) {
	present, err := validatedSuppliedRequestBindings(variant, fields, true)
	if err != nil {
		return PresentValue{}, err
	}

	var body PresentValue
	switch {
	case len(present) == 0:
		if !explicitMediaSelection || variant.schema.Type != SchemaTypeObject {
			return PresentValue{}, contractError(ErrInvalidValue, "input.fields", "selected request body has no supplied root value")
		}
		body = NewObjectValue(nil)
	case present[0].JSONPointer == "":
		body = fields[present[0].FieldID]
	default:
		if variant.schema.Type != SchemaTypeObject {
			return PresentValue{}, contractError(ErrInvalidValue, "input.fields", "nested request assignments require an object root schema")
		}
		root := make(map[string]PresentValue)
		for _, binding := range present {
			parts := decodeJSONPointerParts(binding.JSONPointer)
			cursor := root
			for _, part := range parts[:len(parts)-1] {
				next, ok := cursor[part]
				if !ok {
					next = PresentValue{presence: PresenceValue, kind: ValueKindObject, fields: make(map[string]PresentValue)}
					cursor[part] = next
				}
				if next.presence != PresenceValue || next.kind != ValueKindObject {
					return PresentValue{}, contractError(ErrInvalidValue, "input.fields", "nested request assignments conflict")
				}
				cursor = next.fields
			}
			cursor[parts[len(parts)-1]] = fields[binding.FieldID]
		}
		body = NewObjectValue(root)
	}
	if !operationInputMatchesSchema(body, variant.schema, false) {
		return PresentValue{}, contractError(ErrInvalidValue, "input.fields", "assembled request body violates its full root schema")
	}
	return body, nil
}

func requestBindingMatchesRootSchema(binding FieldBinding, root Schema) bool {
	schema, ok := requestSchemaAtPointer(root, decodeJSONPointerParts(binding.JSONPointer))
	return ok && reflect.DeepEqual(schema, binding.Schema)
}

func requestSchemaAtPointer(schema Schema, parts []string) (Schema, bool) {
	if len(parts) == 0 {
		return schema, true
	}
	property, ok := requestObjectProperty(schema, parts[0])
	if !ok {
		return Schema{}, false
	}
	return requestSchemaAtPointer(property, parts[1:])
}

func requestObjectProperty(schema Schema, name string) (Schema, bool) {
	if schema.Type != SchemaTypeObject {
		return Schema{}, false
	}
	var selected Schema
	found := false
	if property, ok := schema.Properties[name]; ok {
		selected, found = property, true
	}
	for _, component := range schema.AllOf {
		property, ok := requestObjectProperty(component, name)
		if !ok {
			continue
		}
		if found && !reflect.DeepEqual(selected, property) {
			return Schema{}, false
		}
		selected, found = property, true
	}
	return selected, found
}

func requestPointersOverlap(left, right string) bool {
	leftParts := decodeJSONPointerParts(left)
	rightParts := decodeJSONPointerParts(right)
	shorter := len(leftParts)
	if len(rightParts) < shorter {
		shorter = len(rightParts)
	}
	for index := 0; index < shorter; index++ {
		if leftParts[index] != rightParts[index] {
			return false
		}
	}
	return true
}

func decodeJSONPointerParts(pointer string) []string {
	if pointer == "" {
		return nil
	}
	encoded := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	for index := range encoded {
		encoded[index] = strings.ReplaceAll(strings.ReplaceAll(encoded[index], "~1", "/"), "~0", "~")
	}
	return encoded
}

// ValidateValueAgainstSchema applies the canonical typed-value matcher used by
// operation preflight without inventing adapter-specific object semantics.
func ValidateValueAgainstSchema(value PresentValue, schema Schema) error {
	if err := schema.Validate(); err != nil {
		return err
	}
	if err := value.Validate(); err != nil {
		return contractError(ErrInvalidValue, "value", "contains an invalid typed value")
	}
	if !operationInputMatchesSchema(value, schema, false) {
		return contractError(ErrInvalidValue, "value", "does not satisfy its declared schema")
	}
	return nil
}

func operationInputMatchesSchema(value PresentValue, schema Schema, allowAbsent bool) bool {
	switch value.presence {
	case PresenceAbsent:
		return allowAbsent
	case PresenceNull:
		return schema.Nullable && schemaValueSatisfiesConstraints(value, schema)
	case PresenceValue:
	default:
		return false
	}

	if schema.Type == SchemaTypeUnion {
		if value.kind != ValueKindUnion || value.member == nil {
			return false
		}
		matches := 0
		for _, variant := range schema.OneOf {
			if operationInputMatchesSchema(*value.member, variant, false) {
				matches++
			}
		}
		return matches == 1 && schemaValueSatisfiesConstraints(value, schema)
	}
	if !kindMatchesSchema(value.kind, schema.Type) {
		return false
	}

	switch schema.Type {
	case SchemaTypeArray:
		if schema.Items == nil {
			return false
		}
		for _, element := range value.elements {
			if !operationInputMatchesSchema(element, *schema.Items, false) {
				return false
			}
		}
	case SchemaTypeObject:
		if !operationInputObjectMatchesSchema(value.fields, schema) {
			return false
		}
	}
	return schemaValueSatisfiesConstraints(value, schema)
}

func operationInputObjectMatchesSchema(fields map[string]PresentValue, schema Schema) bool {
	for name := range fields {
		if !operationInputObjectDeclaresField(schema, name) {
			return false
		}
	}
	return operationInputObjectSatisfiesConstraints(fields, schema)
}

func operationInputObjectDeclaresField(schema Schema, name string) bool {
	if _, declared := schema.Properties[name]; declared {
		return true
	}
	for _, component := range schema.AllOf {
		if operationInputObjectDeclaresField(component, name) {
			return true
		}
	}
	return false
}

func operationInputObjectSatisfiesConstraints(fields map[string]PresentValue, schema Schema) bool {
	for _, required := range schema.Required {
		field, present := fields[required]
		if !present || field.presence == PresenceAbsent {
			return false
		}
	}
	for name, property := range schema.Properties {
		if field, present := fields[name]; present && !operationInputMatchesSchema(field, property, true) {
			return false
		}
	}
	for _, component := range schema.AllOf {
		if !operationInputObjectSatisfiesConstraints(fields, component) {
			return false
		}
	}
	return true
}

func schemaValueSatisfiesConstraints(value PresentValue, schema Schema) bool {
	if schema.Enum != nil {
		matched := false
		for _, candidate := range schema.Enum {
			if value.Equal(candidate) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if schema.Minimum != nil {
		comparison, ok := compareExactNumericValues(value, *schema.Minimum)
		if !ok || comparison < 0 {
			return false
		}
	}
	if schema.Maximum != nil {
		comparison, ok := compareExactNumericValues(value, *schema.Maximum)
		if !ok || comparison > 0 {
			return false
		}
	}
	return true
}

func compareExactNumericValues(left, right PresentValue) (int, bool) {
	leftNumber, leftOK := left.ExactNumber()
	rightNumber, rightOK := right.ExactNumber()
	if !leftOK || !rightOK {
		return 0, false
	}
	var leftRat, rightRat big.Rat
	if _, ok := leftRat.SetString(leftNumber.Text); !ok {
		return 0, false
	}
	if _, ok := rightRat.SetString(rightNumber.Text); !ok {
		return 0, false
	}
	return leftRat.Cmp(&rightRat), true
}
