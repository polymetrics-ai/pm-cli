package contract

import (
	"math/big"
	"reflect"
	"strings"
)

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
	if err := input.Validate(); err != nil {
		return ResolvedRequest{}, contractError(ErrInvalidValue, "input", "contains an invalid typed value")
	}
	if input.presence != PresenceValue || input.kind != ValueKindObject {
		return ResolvedRequest{}, contractError(ErrInvalidValue, "input", "must be a present logical field-ID object")
	}

	for fieldID := range input.fields {
		if _, ok := common[fieldID]; ok {
			continue
		}
		if _, ok := variantByField[fieldID]; !ok {
			return ResolvedRequest{}, contractError(ErrUnknownField, "input.fields", "contains an undeclared logical field ID")
		}
	}
	for fieldID, binding := range common {
		value, present := input.fields[fieldID]
		if binding.Required && (!present || value.presence == PresenceAbsent) {
			return ResolvedRequest{}, contractError(ErrInvalidValue, "input.fields", "required common logical field is absent")
		}
		if present && !operationInputMatchesSchema(value, binding.Schema, true) {
			return ResolvedRequest{}, contractError(ErrInvalidValue, "input.fields", "common logical field does not match its declared schema")
		}
	}

	selected := -1
	explicitMediaSelection := false
	suppliedByVariant := make([]bool, len(operation.RequestVariants))
	for fieldID, variantIndex := range variantByField {
		if value, present := input.fields[fieldID]; present && value.presence != PresenceAbsent {
			suppliedByVariant[variantIndex] = true
		}
	}

	switch len(operation.RequestVariants) {
	case 0:
		return ResolvedRequest{Body: AbsentValue()}, nil
	case 1:
		if suppliedByVariant[0] {
			selected = 0
		} else if operation.RequestVariants[0].Required {
			return ResolvedRequest{}, contractError(ErrInvalidValue, "input.fields", "required request body is absent")
		} else {
			return ResolvedRequest{Body: AbsentValue()}, nil
		}
	default:
		selector, present := input.fields[operation.RequestMediaFieldID]
		if present && selector.presence != PresenceAbsent {
			mediaType, ok := selector.TextValue()
			if !ok {
				return ResolvedRequest{}, contractError(ErrInvalidValue, "input.fields", "request media selector must be a present string")
			}
			for index := range operation.RequestVariants {
				if operation.RequestVariants[index].MediaType == mediaType {
					selected = index
					break
				}
			}
			if selected < 0 {
				return ResolvedRequest{}, contractError(ErrInvalidValue, "input.fields", "request media selector does not identify a declared variant")
			}
			explicitMediaSelection = true
		} else {
			for _, supplied := range suppliedByVariant {
				if supplied {
					return ResolvedRequest{}, contractError(ErrInvalidValue, "input.fields", "request body media is ambiguous")
				}
			}
			for index := range operation.RequestVariants {
				if operation.RequestVariants[index].Required {
					return ResolvedRequest{}, contractError(ErrInvalidValue, "input.fields", "required request body media is absent")
				}
			}
			return ResolvedRequest{Body: AbsentValue()}, nil
		}
		for index, supplied := range suppliedByVariant {
			if supplied && index != selected {
				return ResolvedRequest{}, contractError(ErrInvalidValue, "input.fields", "request contains fields from a non-selected media variant")
			}
		}
	}

	variant := &operation.RequestVariants[selected]
	body, err := assembleRequestBody(*variant, input.fields, explicitMediaSelection)
	if err != nil {
		return ResolvedRequest{}, err
	}
	return ResolvedRequest{Variant: variant, Body: body}, nil
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

func assembleRequestBody(variant RequestVariant, fields map[string]PresentValue, explicitMediaSelection bool) (PresentValue, error) {
	present := make([]FieldBinding, 0, len(variant.Inputs))
	for _, binding := range variant.Inputs {
		value, supplied := fields[binding.FieldID]
		if binding.Required && (!supplied || value.presence == PresenceAbsent) {
			return PresentValue{}, contractError(ErrInvalidValue, "input.fields", "required variant-local logical field is absent")
		}
		if !supplied || value.presence == PresenceAbsent {
			continue
		}
		if !operationInputMatchesSchema(value, binding.Schema, false) {
			return PresentValue{}, contractError(ErrInvalidValue, "input.fields", "variant-local logical field does not match its declared schema")
		}
		present = append(present, binding)
	}

	for left := range present {
		for offset := range present[left+1:] {
			right := left + 1 + offset
			if requestPointersOverlap(present[left].JSONPointer, present[right].JSONPointer) {
				return PresentValue{}, contractError(ErrInvalidValue, "input.fields", "request body contains conflicting whole or nested assignments")
			}
		}
	}

	var body PresentValue
	switch {
	case len(present) == 0:
		if !explicitMediaSelection || variant.Schema.Type != SchemaTypeObject {
			return PresentValue{}, contractError(ErrInvalidValue, "input.fields", "selected request body has no supplied root value")
		}
		body = NewObjectValue(nil)
	case present[0].JSONPointer == "":
		body = fields[present[0].FieldID]
	default:
		if variant.Schema.Type != SchemaTypeObject {
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
	if !operationInputMatchesSchema(body, variant.Schema, false) {
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
