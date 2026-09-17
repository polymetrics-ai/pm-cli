package compile

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type schemaLowering struct {
	Schema      *contract.Schema
	Facts       []SchemaFact
	Diagnostics []Diagnostic
}

type schemaContext struct {
	source            *ResolvedSource
	sourceID          string
	pointer           string
	operationID       string
	fieldID           string
	normalizedPointer string
	stack             map[string]struct{}
	bindingDefault    bool
	propertyBindings  bool
}

func lowerSchema(reference *openapi3.SchemaRef, context schemaContext) schemaLowering {
	if context.stack == nil {
		context.stack = make(map[string]struct{})
	}
	if reference == nil || reference.Value == nil {
		return unsupportedSchema(context, "missing_schema", "schema reference has no captured value")
	}

	originSourceID := context.sourceID
	originPointer := context.pointer
	identitySourceID := context.sourceID
	identityPointer := context.pointer
	var referenceIdentity *ReferenceIdentity
	if identity, ok := context.source.ReferenceAt(context.sourceID, context.pointer+"/$ref"); ok {
		identitySourceID = identity.TargetSourceID
		identityPointer = identity.TargetPointer
		copy := identity
		referenceIdentity = &copy
	}
	identityKey := identitySourceID + "\x00" + identityPointer
	if _, recursive := context.stack[identityKey]; recursive {
		lowering := unsupportedSchema(context, "recursive_schema_reference", "recursive schema identity is preserved but the by-value runtime schema cannot represent it")
		if len(lowering.Facts) == 0 {
			lowering.Facts = append(lowering.Facts, schemaFact(context.source, context.sourceID, context.pointer, reference.Value, referenceIdentity))
		}
		return lowering
	}
	context.stack[identityKey] = struct{}{}
	defer delete(context.stack, identityKey)
	if referenceIdentity != nil {
		context.sourceID = identitySourceID
		context.pointer = identityPointer
	}

	schema := reference.Value
	facts := make([]SchemaFact, 0, 2)
	if referenceIdentity != nil && (originSourceID != context.sourceID || originPointer != context.pointer) {
		facts = append(facts, schemaFact(context.source, originSourceID, originPointer, schema, referenceIdentity))
	}
	facts = append(facts, schemaFact(context.source, context.sourceID, context.pointer, schema, referenceIdentity))
	lowering := schemaLowering{Facts: facts}
	if !context.bindingDefault {
		lowering = appendUnrepresentedDefault(lowering, context)
	}
	if schema.Always != nil {
		return appendUnsupported(lowering, context, "boolean_schema", "boolean JSON Schema is unsupported by the typed runtime schema")
	}
	if len(schema.AnyOf) != 0 || schema.Not != nil || len(schema.PrefixItems) != 0 || schema.Contains != nil ||
		len(schema.PatternProperties) != 0 || len(schema.DependentSchemas) != 0 || schema.PropertyNames != nil ||
		schema.If != nil || schema.Then != nil || schema.Else != nil || len(schema.DependentRequired) != 0 ||
		schema.DynamicRef != "" || schema.DynamicAnchor != "" {
		return appendUnsupported(lowering, context, "unsupported_schema_keyword", "schema uses a conditional, dynamic or open-ended shape not represented by the typed runtime schema")
	}

	nullable, concreteTypes, ok := schemaTypes(schema)
	if !ok {
		return appendUnsupported(lowering, context, "unsupported_schema_type", "schema type is missing or unsupported")
	}
	if len(concreteTypes) > 1 {
		if len(schema.Enum) != 0 || schema.Min != nil || schema.Max != nil || schemaHasUnrepresentedConstraints(schema) {
			return appendUnsupported(lowering, context, "unsupported_union_constraint", "union-level constraints cannot be represented without changing source meaning")
		}
		variants := make([]contract.Schema, 0, len(concreteTypes))
		for index, concreteType := range concreteTypes {
			copy := *schema
			copy.Type = &openapi3.Types{concreteType}
			copy.Nullable = false
			variantContext := context
			variantContext.pointer = joinPointer(context.pointer, "type") + "/" + strconv.Itoa(index)
			variantContext.bindingDefault = false
			variantContext.propertyBindings = false
			variant := lowerSchema(&openapi3.SchemaRef{Value: &copy}, variantContext)
			lowering.Facts = append(lowering.Facts, variant.Facts...)
			lowering.Diagnostics = append(lowering.Diagnostics, variant.Diagnostics...)
			if variant.Schema == nil {
				return lowering
			}
			variants = append(variants, *variant.Schema)
		}
		result := contract.Schema{Type: contract.SchemaTypeUnion, Nullable: nullable, OneOf: variants}
		if err := result.Validate(); err != nil {
			return appendUnsupported(lowering, context, "unsupported_union", err.Error())
		}
		lowering.Schema = &result
		return lowering
	}

	if len(schema.OneOf) != 0 {
		if len(schema.Enum) != 0 || schema.Min != nil || schema.Max != nil || schemaHasUnrepresentedConstraints(schema) {
			return appendUnsupported(lowering, context, "unsupported_union_constraint", "union-level constraints cannot be represented without changing source meaning")
		}
		variants := make([]contract.Schema, 0, len(schema.OneOf))
		for index, variantReference := range schema.OneOf {
			variantContext := context
			variantContext.pointer = joinPointer(joinPointer(context.pointer, "oneOf"), strconv.Itoa(index))
			variantContext.bindingDefault = false
			variantContext.propertyBindings = false
			variant := lowerSchema(variantReference, variantContext)
			lowering.Facts = append(lowering.Facts, variant.Facts...)
			lowering.Diagnostics = append(lowering.Diagnostics, variant.Diagnostics...)
			if variant.Schema == nil {
				return lowering
			}
			variants = append(variants, *variant.Schema)
		}
		result := contract.Schema{Type: contract.SchemaTypeUnion, Nullable: nullable, OneOf: variants}
		if err := result.Validate(); err != nil {
			return appendUnsupported(lowering, context, "unsupported_union", err.Error())
		}
		lowering.Schema = &result
		return lowering
	}

	logicalType, supported := logicalSchemaType(concreteTypes[0], schema.Format)
	if !supported {
		return appendUnsupported(lowering, context, "unsupported_schema_type", "schema type or format is unsupported")
	}
	result := contract.Schema{Type: logicalType, Nullable: nullable || schema.Nullable}
	if err := applyValueConstraints(&result, schema, context); err != nil {
		return appendUnsupported(lowering, context, "unsupported_schema_constraint", err.Error())
	}

	if len(schema.AllOf) != 0 {
		if logicalType != contract.SchemaTypeObject {
			return appendUnsupported(lowering, context, "unsupported_all_of", "allOf is supported only for object branches")
		}
		result.AllOf = make([]contract.Schema, 0, len(schema.AllOf))
		for index, componentReference := range schema.AllOf {
			componentContext := context
			componentContext.pointer = joinPointer(joinPointer(context.pointer, "allOf"), strconv.Itoa(index))
			componentContext.bindingDefault = false
			component := lowerSchema(componentReference, componentContext)
			lowering.Facts = append(lowering.Facts, component.Facts...)
			lowering.Diagnostics = append(lowering.Diagnostics, component.Diagnostics...)
			if component.Schema == nil || component.Schema.Type != contract.SchemaTypeObject {
				return appendUnsupported(lowering, context, "unsupported_all_of", "allOf component is not a supported object schema")
			}
			component.Schema.Nullable = result.Nullable
			result.AllOf = append(result.AllOf, *component.Schema)
		}
	}

	switch logicalType {
	case contract.SchemaTypeObject:
		result.Properties = make(map[string]contract.Schema, len(schema.Properties))
		result.Required = slices.Clone(schema.Required)
		for name, propertyReference := range schema.Properties {
			propertyContext := context
			propertyContext.pointer = joinPointer(joinPointer(context.pointer, "properties"), name)
			propertyContext.fieldID = name
			propertyContext.bindingDefault = context.propertyBindings
			propertyContext.propertyBindings = false
			property := lowerSchema(propertyReference, propertyContext)
			lowering.Facts = append(lowering.Facts, property.Facts...)
			lowering.Diagnostics = append(lowering.Diagnostics, property.Diagnostics...)
			if property.Schema == nil {
				continue
			}
			result.Properties[name] = *property.Schema
		}
		if schema.AdditionalProperties.Schema != nil || schema.AdditionalProperties.Has == nil ||
			(schema.AdditionalProperties.Has != nil && *schema.AdditionalProperties.Has) {
			lowering = appendUnsupported(lowering, context, "open_object_shape", "additional object properties are retained as a blocking raw fact because the typed runtime schema is closed")
		}
	case contract.SchemaTypeArray:
		if schema.Items == nil {
			return appendUnsupported(lowering, context, "missing_array_items", "array item schema is required")
		}
		itemContext := context
		itemContext.pointer = joinPointer(context.pointer, "items")
		itemContext.bindingDefault = false
		itemContext.propertyBindings = false
		item := lowerSchema(schema.Items, itemContext)
		lowering.Facts = append(lowering.Facts, item.Facts...)
		lowering.Diagnostics = append(lowering.Diagnostics, item.Diagnostics...)
		if item.Schema == nil {
			return lowering
		}
		result.Items = item.Schema
	}

	if schemaHasUnrepresentedConstraints(schema) {
		lowering = appendUnsupported(lowering, context, "unsupported_schema_constraint", "schema constraints remain in raw facts but are not represented by the typed runtime schema")
	}
	if err := result.Validate(); err != nil {
		return appendUnsupported(lowering, context, "invalid_normalized_schema", err.Error())
	}
	lowering.Schema = &result
	return lowering
}

func schemaFact(source *ResolvedSource, sourceID, pointer string, schema *openapi3.Schema, reference *ReferenceIdentity) SchemaFact {
	hash, _ := source.PointerHash(sourceID, pointer)
	fact := SchemaFact{
		SourceFact: SourceFact{
			SourceRef: contract.SourceRef{SourceID: sourceID, Pointer: "#" + pointer},
			SHA256:    hash,
		},
		Format:    schema.Format,
		Nullable:  schema.Nullable,
		ReadOnly:  schema.ReadOnly,
		WriteOnly: schema.WriteOnly,
		Required:  slices.Clone(schema.Required),
		Reference: reference,
	}
	if rawDefault, ok := source.RawValue(sourceID, joinPointer(pointer, "default")); ok {
		encoded, err := json.Marshal(rawDefault)
		if err == nil {
			defaultPointer := joinPointer(pointer, "default")
			defaultHash, _ := source.PointerHash(sourceID, defaultPointer)
			fact.Default = &RawValueFact{
				SourceFact: SourceFact{
					SourceRef: contract.SourceRef{SourceID: sourceID, Pointer: "#" + defaultPointer},
					SHA256:    defaultHash,
				},
				CanonicalJSON: encoded,
			}
		}
	}
	if schema.Type != nil {
		fact.Types = slices.Clone(schema.Type.Slice())
	}
	for index := range schema.AllOf {
		fact.AllOfPointers = append(fact.AllOfPointers, "#"+joinPointer(joinPointer(pointer, "allOf"), strconv.Itoa(index)))
	}
	for index := range schema.OneOf {
		fact.OneOfPointers = append(fact.OneOfPointers, "#"+joinPointer(joinPointer(pointer, "oneOf"), strconv.Itoa(index)))
	}
	return fact
}
func resolvedSchemaDefault(source *ResolvedSource, sourceID, pointer string) (any, bool) {
	seen := make(map[string]struct{})
	for {
		key := sourceID + "\x00" + pointer
		if _, duplicate := seen[key]; duplicate {
			return nil, false
		}
		seen[key] = struct{}{}
		if value, ok := source.RawValue(sourceID, joinPointer(pointer, "default")); ok {
			return value, true
		}
		reference, ok := source.ReferenceAt(sourceID, joinPointer(pointer, "$ref"))
		if !ok {
			return nil, false
		}
		sourceID = reference.TargetSourceID
		pointer = reference.TargetPointer
	}
}

func unsupportedSchema(context schemaContext, code, detail string) schemaLowering {
	fact := SourceFact{SourceRef: contract.SourceRef{SourceID: context.sourceID, Pointer: "#" + context.pointer}}
	fact.SHA256, _ = context.source.PointerHash(context.sourceID, context.pointer)
	diagnostic := newDiagnostic(
		code,
		SeverityError,
		context.operationID,
		context.fieldID,
		[]SourceFact{fact},
		context.normalizedPointer,
		nil,
		allLaneSlice(),
		detail,
	)
	return schemaLowering{Diagnostics: []Diagnostic{diagnostic}}
}
func appendUnrepresentedDefault(lowering schemaLowering, context schemaContext) schemaLowering {
	defaults := make([]SourceFact, 0, len(lowering.Facts))
	for _, fact := range lowering.Facts {
		if fact.Default != nil {
			defaults = append(defaults, fact.Default.SourceFact)
		}
	}
	if len(defaults) == 0 {
		return lowering
	}
	diagnostic := newDiagnostic(
		"unrepresented_schema_default",
		SeverityWarning,
		context.operationID,
		context.fieldID,
		defaults,
		context.normalizedPointer,
		nil,
		allLaneSlice(),
		"explicit schema default is preserved as a raw fact but is not implicitly applied at this nested or response position",
	)
	lowering.Diagnostics = append(lowering.Diagnostics, diagnostic)
	return lowering
}

func appendUnsupported(lowering schemaLowering, context schemaContext, code, detail string) schemaLowering {
	unsupported := unsupportedSchema(context, code, detail)
	lowering.Diagnostics = append(lowering.Diagnostics, unsupported.Diagnostics...)
	return lowering
}

func schemaTypes(schema *openapi3.Schema) (bool, []string, bool) {
	nullable := schema.Nullable
	var types []string
	if schema.Type != nil {
		for _, value := range schema.Type.Slice() {
			if value == "null" {
				nullable = true
				continue
			}
			types = append(types, value)
		}
	}
	if len(types) == 0 {
		switch {
		case len(schema.OneOf) != 0:
			return nullable, []string{"union"}, true
		case len(schema.AllOf) != 0 || schema.Properties != nil:
			types = []string{"object"}
		case schema.Items != nil:
			types = []string{"array"}
		default:
			return nullable, nil, false
		}
	}
	for _, value := range types {
		switch value {
		case "string", "boolean", "integer", "number", "array", "object":
		default:
			return nullable, nil, false
		}
	}
	return nullable, types, true
}

func logicalSchemaType(sourceType, format string) (contract.SchemaType, bool) {
	switch sourceType {
	case "string":
		switch format {
		case "date":
			return contract.SchemaTypeDate, true
		case "date-time":
			return contract.SchemaTypeTimestampInstant, true
		case "local-date-time":
			return contract.SchemaTypeTimestampLocal, true
		case "binary", "byte":
			return contract.SchemaTypeBinaryRef, true
		default:
			return contract.SchemaTypeString, true
		}
	case "boolean":
		return contract.SchemaTypeBool, true
	case "integer":
		return contract.SchemaTypeInteger, true
	case "number":
		return contract.SchemaTypeDecimal, true
	case "array":
		return contract.SchemaTypeArray, true
	case "object":
		return contract.SchemaTypeObject, true
	default:
		return "", false
	}
}

func schemaHasUnrepresentedConstraints(schema *openapi3.Schema) bool {
	return schema.Const != nil || schema.MultipleOf != nil ||
		exclusiveBoundActive(schema.ExclusiveMin) || exclusiveBoundActive(schema.ExclusiveMax) || schema.MinLength != 0 || schema.MaxLength != nil || schema.Pattern != "" ||
		schema.MinItems != 0 || schema.MaxItems != nil || schema.UniqueItems || schema.MinProps != 0 || schema.MaxProps != nil ||
		schema.SchemaDialect != "" || schema.SchemaID != "" || schema.Anchor != "" || schema.ContentEncoding != "" ||
		schema.ContentMediaType != "" || schema.ContentSchema != nil
}

func applyValueConstraints(result *contract.Schema, schema *openapi3.Schema, context schemaContext) error {
	if len(schema.Enum) != 0 {
		raw, ok := context.source.RawValue(context.sourceID, context.pointer+"/enum")
		if !ok {
			return fmt.Errorf("enum values are absent from the captured source pointer")
		}
		values, ok := raw.([]any)
		if !ok || len(values) != len(schema.Enum) {
			return fmt.Errorf("enum values cannot be recovered exactly from the captured source")
		}
		result.Enum = make([]contract.PresentValue, 0, len(values))
		for _, value := range values {
			typed, err := presentValueFromRaw(value, *result)
			if err != nil {
				return fmt.Errorf("enum value cannot be represented exactly: %w", err)
			}
			result.Enum = append(result.Enum, typed)
		}
	}
	for _, constraint := range []struct {
		name   string
		source *float64
		target **contract.PresentValue
	}{
		{name: "minimum", source: schema.Min, target: &result.Minimum},
		{name: "maximum", source: schema.Max, target: &result.Maximum},
	} {
		if constraint.source == nil {
			continue
		}
		raw, ok := context.source.RawValue(context.sourceID, context.pointer+"/"+constraint.name)
		if !ok {
			return fmt.Errorf("%s is absent from the captured source pointer", constraint.name)
		}
		typed, err := presentValueFromRaw(raw, *result)
		if err != nil {
			return fmt.Errorf("%s cannot be represented exactly: %w", constraint.name, err)
		}
		*constraint.target = &typed
	}
	return nil
}

func exclusiveBoundActive(bound openapi3.ExclusiveBound) bool {
	return bound.Value != nil || (bound.Bool != nil && *bound.Bool)
}

func presentValueFromRaw(value any, schema contract.Schema) (contract.PresentValue, error) {
	if value == nil {
		if !schema.Nullable {
			return contract.PresentValue{}, fmt.Errorf("null default is not allowed by the schema")
		}
		return contract.NullValue(), nil
	}
	if schema.Type == contract.SchemaTypeUnion {
		matches := make([]struct {
			index int
			value contract.PresentValue
		}, 0, len(schema.OneOf))
		for index, variant := range schema.OneOf {
			candidate, err := presentValueFromRaw(value, variant)
			if err == nil {
				matches = append(matches, struct {
					index int
					value contract.PresentValue
				}{index: index, value: candidate})
			}
		}
		if len(matches) != 1 {
			return contract.PresentValue{}, fmt.Errorf("default does not select exactly one union variant")
		}
		return contract.NewUnionValue("variant-"+strconv.Itoa(matches[0].index), matches[0].value)
	}
	switch schema.Type {
	case contract.SchemaTypeString:
		text, ok := value.(string)
		if !ok {
			return contract.PresentValue{}, fmt.Errorf("default is not a string")
		}
		return contract.NewStringValue(text), nil
	case contract.SchemaTypeBool:
		boolean, ok := value.(bool)
		if !ok {
			return contract.PresentValue{}, fmt.Errorf("default is not a boolean")
		}
		return contract.NewBooleanValue(boolean), nil
	case contract.SchemaTypeInteger:
		number, ok := value.(json.Number)
		if !ok || strings.ContainsAny(number.String(), ".eE") {
			return contract.PresentValue{}, fmt.Errorf("default is not an exact integer")
		}
		return contract.NewIntegerValue(number.String())
	case contract.SchemaTypeDecimal:
		number, ok := value.(json.Number)
		if !ok {
			return contract.PresentValue{}, fmt.Errorf("default is not an exact decimal")
		}
		exact, precision, scale, ok := exactDecimalShape(number.String())
		if !ok {
			return contract.PresentValue{}, fmt.Errorf("default decimal cannot be represented exactly")
		}
		return contract.NewDecimalValue(exact, precision, scale)
	case contract.SchemaTypeDate:
		text, ok := value.(string)
		if !ok {
			return contract.PresentValue{}, fmt.Errorf("default is not a date string")
		}
		return contract.NewDateValue(text)
	case contract.SchemaTypeTimestampInstant:
		text, ok := value.(string)
		if !ok {
			return contract.PresentValue{}, fmt.Errorf("default is not an instant string")
		}
		return contract.NewInstantValue(text)
	case contract.SchemaTypeTimestampLocal:
		text, ok := value.(string)
		if !ok {
			return contract.PresentValue{}, fmt.Errorf("default is not a local timestamp string")
		}
		return contract.NewLocalTimestampValue(text)
	case contract.SchemaTypeArray:
		array, ok := value.([]any)
		if !ok || schema.Items == nil {
			return contract.PresentValue{}, fmt.Errorf("default is not a declared array")
		}
		items := make([]contract.PresentValue, 0, len(array))
		for _, raw := range array {
			item, err := presentValueFromRaw(raw, *schema.Items)
			if err != nil {
				return contract.PresentValue{}, err
			}
			items = append(items, item)
		}
		return contract.NewArrayValue(items), nil
	case contract.SchemaTypeObject:
		object, ok := value.(map[string]any)
		if !ok {
			return contract.PresentValue{}, fmt.Errorf("default is not a declared object")
		}
		fields := make(map[string]contract.PresentValue, len(object))
		for name, raw := range object {
			property, declared := schema.Properties[name]
			if !declared {
				return contract.PresentValue{}, fmt.Errorf("default contains an undeclared object property")
			}
			field, err := presentValueFromRaw(raw, property)
			if err != nil {
				return contract.PresentValue{}, err
			}
			fields[name] = field
		}
		return contract.NewObjectValue(fields), nil
	default:
		return contract.PresentValue{}, fmt.Errorf("default type is unsupported")
	}
}

func exactDecimalShape(value string) (string, int, int, bool) {
	if !jsonNumberPattern.MatchString(value) || strings.ContainsAny(value, "eE") {
		return "", 0, 0, false
	}
	unsigned := strings.TrimPrefix(value, "-")
	parts := strings.SplitN(unsigned, ".", 2)
	digits := strings.TrimLeft(parts[0], "0")
	if len(parts) == 2 {
		digits += parts[1]
	}
	if digits == "" {
		digits = "0"
	}
	scale := 0
	if len(parts) == 2 {
		scale = len(parts[1])
	}
	return value, len(digits), scale, true
}

func schemaContainsPointer(schema contract.Schema, pointer string) bool {
	if pointer == "" {
		return true
	}
	if !validJSONPointer(pointer) || !strings.HasPrefix(pointer, "/") {
		return false
	}
	tokens := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	for index := range tokens {
		tokens[index] = strings.ReplaceAll(strings.ReplaceAll(tokens[index], "~1", "/"), "~0", "~")
	}
	var contains func(contract.Schema, []string) bool
	contains = func(current contract.Schema, remaining []string) bool {
		if len(remaining) == 0 {
			return true
		}
		switch current.Type {
		case contract.SchemaTypeObject:
			if property, ok := current.Properties[remaining[0]]; ok && contains(property, remaining[1:]) {
				return true
			}
			for _, component := range current.AllOf {
				if contains(component, remaining) {
					return true
				}
			}
			return false
		case contract.SchemaTypeArray:
			if current.Items == nil {
				return false
			}
			if _, err := strconv.Atoi(remaining[0]); err != nil {
				return false
			}
			return contains(*current.Items, remaining[1:])
		case contract.SchemaTypeUnion:
			if len(current.OneOf) == 0 {
				return false
			}
			for _, variant := range current.OneOf {
				if !contains(variant, remaining) {
					return false
				}
			}
			return true
		default:
			return false
		}
	}
	return contains(schema, tokens)
}
