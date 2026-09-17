package contract

// ValidateOperationInput validates a caller-owned logical FieldID object against
// the operation's declared input bindings. It never applies source defaults or
// rewrites the supplied value.
func ValidateOperationInput(operation Operation, input PresentValue) error {
	if operation.Inputs == nil {
		return contractError(ErrInvalidContract, "operation.inputs", "input bindings must be explicitly declared")
	}

	declared := make(map[string]struct{}, len(operation.Inputs))
	for index := range operation.Inputs {
		binding := operation.Inputs[index]
		if err := binding.Validate(); err != nil {
			return err
		}
		if _, duplicate := declared[binding.FieldID]; duplicate {
			return contractError(ErrDuplicateField, "operation.inputs", "logical field ID is duplicated")
		}
		declared[binding.FieldID] = struct{}{}
	}

	if err := input.Validate(); err != nil {
		return contractError(ErrInvalidValue, "input", "contains an invalid typed value")
	}
	if input.presence != PresenceValue || input.kind != ValueKindObject {
		return contractError(ErrInvalidValue, "input", "must be a present logical field-ID object")
	}

	for fieldID := range input.fields {
		if _, ok := declared[fieldID]; !ok {
			return contractError(ErrUnknownField, "input.fields", "contains an undeclared logical field ID")
		}
	}

	for index := range operation.Inputs {
		binding := operation.Inputs[index]
		value, present := input.fields[binding.FieldID]
		if binding.Required && (!present || value.presence == PresenceAbsent) {
			return contractError(ErrInvalidValue, "input.fields", "required logical field is absent")
		}
		if !present {
			continue
		}
		if !operationInputMatchesSchema(value, binding.Schema, true) {
			return contractError(ErrInvalidValue, "input.fields", "logical field does not match its declared schema")
		}
	}
	return nil
}

func operationInputMatchesSchema(value PresentValue, schema Schema, allowAbsent bool) bool {
	switch value.presence {
	case PresenceAbsent:
		return allowAbsent
	case PresenceNull:
		return schema.Nullable
	case PresenceValue:
	default:
		return false
	}

	if schema.Type == SchemaTypeUnion {
		if value.kind != ValueKindUnion || value.member == nil {
			return false
		}
		// Schema variants have no variant-name field. The retained value tag is
		// therefore not a discriminator; the member must match exactly one oneOf
		// schema without coercion.
		matches := 0
		for _, variant := range schema.OneOf {
			if operationInputMatchesSchema(*value.member, variant, false) {
				matches++
			}
		}
		return matches == 1
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
		return operationInputObjectMatchesSchema(value.fields, schema)
	}
	return true
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
