package contract

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateOperationInputPresencePrecisionAndLogicalIDs(t *testing.T) {
	large := mustOperationInputInteger(t, "9007199254740993")
	decimal := mustOperationInputDecimal(t, "1234567890.0123456789", 20, 10)
	zero := mustOperationInputInteger(t, "0")

	bindings := []FieldBinding{
		operationInputBindingForTest("enabled", "enabled", true, false, Schema{Type: SchemaTypeBool}),
		operationInputBindingForTest("large-id", "large_id", true, false, Schema{Type: SchemaTypeInteger}),
		operationInputBindingForTest("ratio", "ratio", true, false, Schema{Type: SchemaTypeDecimal}),
		operationInputBindingForTest("zero", "zero", true, false, Schema{Type: SchemaTypeInteger}),
		operationInputBindingForTest("label", "label", true, false, Schema{Type: SchemaTypeString}),
		operationInputBindingForTest("tags", "tags", true, false, Schema{
			Type:  SchemaTypeArray,
			Items: new(Schema{Type: SchemaTypeString}),
		}),
		operationInputBindingForTest("attributes", "attributes", true, false, Schema{
			Type:       SchemaTypeObject,
			Required:   []string{},
			Properties: map[string]Schema{},
		}),
		operationInputBindingForTest("per-page", "per_page", false, false, Schema{Type: SchemaTypeInteger}),
	}
	bindings[len(bindings)-1].SourceDefault = new(mustOperationInputInteger(t, "4"))
	operation := operationWithBodyBindingsForTest(bindings)

	t.Run("exact values remain unchanged and defaults remain absent", func(t *testing.T) {
		input := NewObjectValue(map[string]PresentValue{
			"enabled":    NewBooleanValue(false),
			"large-id":   large,
			"ratio":      decimal,
			"zero":       zero,
			"label":      NewStringValue(""),
			"tags":       NewArrayValue([]PresentValue{}),
			"attributes": NewObjectValue(map[string]PresentValue{}),
		})
		before := cloneOperationInputForTest(t, input)

		if err := ValidateOperationInput(operation, input); err != nil {
			t.Fatalf("ValidateOperationInput(exact values): %v", err)
		}
		if !input.Equal(before) {
			t.Fatal("validation mutated supplied input")
		}
		fields, _ := input.Fields()
		if _, inserted := fields["per-page"]; inserted {
			t.Fatal("validation materialized a declared source default")
		}
	})

	t.Run("provider wire name is not a logical field ID", func(t *testing.T) {
		input := NewObjectValue(map[string]PresentValue{
			"enabled":    NewBooleanValue(false),
			"large-id":   large,
			"ratio":      decimal,
			"zero":       zero,
			"label":      NewStringValue(""),
			"tags":       NewArrayValue(nil),
			"attributes": NewObjectValue(nil),
			"per_page":   mustOperationInputInteger(t, "4"),
		})
		err := ValidateOperationInput(operation, input)
		if !errors.Is(err, ErrUnknownField) {
			t.Fatalf("wire-name input error = %v; want unknown field", err)
		}
		if strings.Contains(err.Error(), "per_page") {
			t.Fatalf("unknown-field error echoed supplied field name: %v", err)
		}
	})

	t.Run("missing required and string coercion are refused", func(t *testing.T) {
		missing := NewObjectValue(map[string]PresentValue{})
		if err := ValidateOperationInput(operation, missing); !errors.Is(err, ErrInvalidValue) {
			t.Fatalf("missing required input error = %v; want invalid value", err)
		}

		coerced := NewObjectValue(map[string]PresentValue{
			"enabled":    NewBooleanValue(false),
			"large-id":   NewStringValue("private-9007199254740993"),
			"ratio":      decimal,
			"zero":       zero,
			"label":      NewStringValue(""),
			"tags":       NewArrayValue(nil),
			"attributes": NewObjectValue(nil),
		})
		err := ValidateOperationInput(operation, coerced)
		if !errors.Is(err, ErrInvalidValue) {
			t.Fatalf("coerced integer input error = %v; want invalid value", err)
		}
		if strings.Contains(err.Error(), "private-9007199254740993") {
			t.Fatalf("type error echoed supplied value: %v", err)
		}
	})

	t.Run("required nullable accepts explicit null but not absence", func(t *testing.T) {
		nullable := operationWithBodyBindingsForTest([]FieldBinding{
			operationInputBindingForTest("note", "note", true, true, Schema{Type: SchemaTypeString, Nullable: true}),
		})
		if err := ValidateOperationInput(nullable, NewObjectValue(map[string]PresentValue{"note": NullValue()})); err != nil {
			t.Fatalf("explicit nullable input: %v", err)
		}
		if err := ValidateOperationInput(nullable, NewObjectValue(map[string]PresentValue{"note": AbsentValue()})); !errors.Is(err, ErrInvalidValue) {
			t.Fatalf("absent required nullable input error = %v; want invalid value", err)
		}
		nonnullable := operationWithBodyBindingsForTest([]FieldBinding{
			operationInputBindingForTest("note", "note", true, false, Schema{Type: SchemaTypeString}),
		})
		if err := ValidateOperationInput(nonnullable, NewObjectValue(map[string]PresentValue{"note": NullValue()})); !errors.Is(err, ErrInvalidValue) {
			t.Fatalf("null non-nullable input error = %v; want invalid value", err)
		}
	})
}

func TestValidateOperationInputNestedShapes(t *testing.T) {
	itemSchema := Schema{
		Type:     SchemaTypeObject,
		Required: []string{"enabled", "amount"},
		Properties: map[string]Schema{
			"enabled": {Type: SchemaTypeBool},
			"amount":  {Type: SchemaTypeDecimal},
		},
	}
	payloadSchema := Schema{
		Type:     SchemaTypeObject,
		Required: []string{"items"},
		Properties: map[string]Schema{
			"items": {
				Type:  SchemaTypeArray,
				Items: new(itemSchema),
			},
		},
	}
	operation := operationWithBodyBindingsForTest([]FieldBinding{
		operationInputBindingForTest("payload", "payload", true, false, payloadSchema),
	})
	amount := mustOperationInputDecimal(t, "9.10", 3, 2)

	valid := NewObjectValue(map[string]PresentValue{
		"payload": NewObjectValue(map[string]PresentValue{
			"items": NewArrayValue([]PresentValue{
				NewObjectValue(map[string]PresentValue{
					"enabled": NewBooleanValue(false),
					"amount":  amount,
				}),
			}),
		}),
	})
	if err := ValidateOperationInput(operation, valid); err != nil {
		t.Fatalf("ValidateOperationInput(valid nested value): %v", err)
	}

	tests := []struct {
		name  string
		value PresentValue
	}{
		{
			name: "wrong nested type",
			value: NewObjectValue(map[string]PresentValue{
				"payload": NewObjectValue(map[string]PresentValue{
					"items": NewArrayValue([]PresentValue{
						NewObjectValue(map[string]PresentValue{
							"enabled": NewBooleanValue(false),
							"amount":  NewStringValue("private-9.10"),
						}),
					}),
				}),
			}),
		},
		{
			name: "missing nested required property",
			value: NewObjectValue(map[string]PresentValue{
				"payload": NewObjectValue(map[string]PresentValue{
					"items": NewArrayValue([]PresentValue{
						NewObjectValue(map[string]PresentValue{"amount": amount}),
					}),
				}),
			}),
		},
		{
			name: "undeclared nested property",
			value: NewObjectValue(map[string]PresentValue{
				"payload": NewObjectValue(map[string]PresentValue{
					"items": NewArrayValue([]PresentValue{
						NewObjectValue(map[string]PresentValue{
							"enabled":       NewBooleanValue(false),
							"amount":        amount,
							"private-extra": NewStringValue("private-value"),
						}),
					}),
				}),
			}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateOperationInput(operation, test.value)
			if !errors.Is(err, ErrInvalidValue) {
				t.Fatalf("nested input error = %v; want invalid value", err)
			}
			if strings.Contains(err.Error(), "private-") {
				t.Fatalf("nested input error echoed supplied data: %v", err)
			}
		})
	}
}

func TestValidateOperationInputComposition(t *testing.T) {
	composed := Schema{
		Type: SchemaTypeObject,
		AllOf: []Schema{
			{
				Type:       SchemaTypeObject,
				Required:   []string{"kind"},
				Properties: map[string]Schema{"kind": {Type: SchemaTypeString}},
			},
			{
				Type:       SchemaTypeObject,
				Required:   []string{"active"},
				Properties: map[string]Schema{"active": {Type: SchemaTypeBool}},
			},
		},
	}
	union := Schema{
		Type: SchemaTypeUnion,
		OneOf: []Schema{
			{Type: SchemaTypeString},
			{
				Type:       SchemaTypeObject,
				Required:   []string{"nested"},
				Properties: map[string]Schema{"nested": {Type: SchemaTypeBool}},
			},
		},
	}
	operation := operationWithBodyBindingsForTest([]FieldBinding{
		operationInputBindingForTest("composed", "composed", true, false, composed),
		operationInputBindingForTest("choice", "choice", true, false, union),
	})
	choice, err := NewUnionValue("record", NewObjectValue(map[string]PresentValue{"nested": NewBooleanValue(false)}))
	if err != nil {
		t.Fatal(err)
	}
	valid := NewObjectValue(map[string]PresentValue{
		"composed": NewObjectValue(map[string]PresentValue{
			"kind":   NewStringValue("entry"),
			"active": NewBooleanValue(false),
		}),
		"choice": choice,
	})
	if err := ValidateOperationInput(operation, valid); err != nil {
		t.Fatalf("ValidateOperationInput(valid composition): %v", err)
	}

	missingAllOf := NewObjectValue(map[string]PresentValue{
		"composed": NewObjectValue(map[string]PresentValue{"kind": NewStringValue("entry")}),
		"choice":   choice,
	})
	if err := ValidateOperationInput(operation, missingAllOf); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("incomplete allOf error = %v; want invalid value", err)
	}

	plainMember := NewObjectValue(map[string]PresentValue{
		"composed": NewObjectValue(map[string]PresentValue{
			"kind":   NewStringValue("entry"),
			"active": NewBooleanValue(false),
		}),
		"choice": NewObjectValue(map[string]PresentValue{"nested": NewBooleanValue(false)}),
	})
	if err := ValidateOperationInput(operation, plainMember); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("unwrapped union member error = %v; want invalid value", err)
	}

	ambiguousSchema := Schema{
		Type: SchemaTypeUnion,
		OneOf: []Schema{
			{Type: SchemaTypeObject, Properties: map[string]Schema{"flag": {Type: SchemaTypeBool}}},
			{Type: SchemaTypeObject, Properties: map[string]Schema{"flag": {Type: SchemaTypeBool}}},
		},
	}
	ambiguousOperation := operationWithBodyBindingsForTest([]FieldBinding{
		operationInputBindingForTest("choice", "choice", true, false, ambiguousSchema),
	})
	ambiguous, err := NewUnionValue("ambiguous", NewObjectValue(map[string]PresentValue{"flag": NewBooleanValue(false)}))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateOperationInput(ambiguousOperation, NewObjectValue(map[string]PresentValue{"choice": ambiguous})); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("ambiguous oneOf error = %v; want invalid value", err)
	}
}

func TestResolveOperationRequestVariants(t *testing.T) {
	selector := FieldBinding{
		FieldID:       "request-content-type",
		WireName:      RequestMediaTypeWireName,
		Location:      FieldLocationHeader,
		JSONPointer:   "/headers/Content-Type",
		Required:      false,
		Nullable:      false,
		Style:         "simple",
		Explode:       false,
		AllowReserved: false,
		Sensitivity:   SensitivityPublic,
		Schema: Schema{
			Type: SchemaTypeString,
			Enum: []PresentValue{
				NewStringValue("application/json"),
				NewStringValue("application/merge-patch+json"),
			},
		},
	}
	root := Schema{
		Type:       SchemaTypeObject,
		Required:   []string{"name"},
		Properties: map[string]Schema{"name": {Type: SchemaTypeString}},
	}
	jsonBinding := operationInputBindingForTest("json-name", "name", true, false, Schema{Type: SchemaTypeString})
	mergeBinding := operationInputBindingForTest("merge-name", "name", true, false, Schema{Type: SchemaTypeString})
	operation := Operation{
		Inputs:              []FieldBinding{selector},
		RequestMediaFieldID: selector.FieldID,
		RequestVariants: []RequestVariant{
			{
				VariantID:  "json",
				MediaType:  "application/json",
				Required:   false,
				Schema:     root,
				Inputs:     []FieldBinding{jsonBinding},
				SourceRefs: []SourceRef{{SourceID: "fixture-source", Pointer: "#/requestBody/content/application~1json"}},
			},
			{
				VariantID:  "merge-patch",
				MediaType:  "application/merge-patch+json",
				Required:   false,
				Schema:     root,
				Inputs:     []FieldBinding{mergeBinding},
				SourceRefs: []SourceRef{{SourceID: "fixture-source", Pointer: "#/requestBody/content/application~1merge-patch+json"}},
			},
		},
	}

	selected, err := ResolveOperationRequest(operation, NewObjectValue(map[string]PresentValue{
		selector.FieldID:    NewStringValue("application/json"),
		jsonBinding.FieldID: NewStringValue("Ada"),
	}))
	if err != nil {
		t.Fatalf("ResolveOperationRequest(selected JSON): %v", err)
	}
	if selected.Variant == nil || selected.Variant.VariantID != "json" {
		t.Fatalf("selected variant = %#v; want json", selected.Variant)
	}
	bodyFields, ok := selected.Body.Fields()
	if !ok || !bodyFields["name"].Equal(NewStringValue("Ada")) {
		t.Fatalf("assembled body = %#v; want exact selected variant field", selected.Body)
	}

	absent, err := ResolveOperationRequest(operation, NewObjectValue(nil))
	if err != nil {
		t.Fatalf("ResolveOperationRequest(optional absent body): %v", err)
	}
	if absent.Variant != nil || absent.Body.Presence() != PresenceAbsent {
		t.Fatalf("optional absent request = %#v; want no selected variant and absent body", absent)
	}

	incompleteSelector := operation
	incompleteSelector.Inputs = append([]FieldBinding(nil), operation.Inputs...)
	incompleteSelector.Inputs[0].Schema.Enum = []PresentValue{NewStringValue("application/json")}
	if _, err := ResolveOperationRequest(incompleteSelector, NewObjectValue(nil)); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("incomplete media selector error = %v; want invalid contract", err)
	}

	for _, test := range []struct {
		name  string
		input PresentValue
	}{
		{
			name: "body field without explicit media",
			input: NewObjectValue(map[string]PresentValue{
				jsonBinding.FieldID: NewStringValue("Ada"),
			}),
		},
		{
			name: "field from wrong selected variant",
			input: NewObjectValue(map[string]PresentValue{
				selector.FieldID:    NewStringValue("application/merge-patch+json"),
				jsonBinding.FieldID: NewStringValue("Ada"),
			}),
		},
		{
			name: "mixed variant fields",
			input: NewObjectValue(map[string]PresentValue{
				selector.FieldID:     NewStringValue("application/json"),
				jsonBinding.FieldID:  NewStringValue("Ada"),
				mergeBinding.FieldID: NewStringValue("Grace"),
			}),
		},
		{
			name: "undeclared media",
			input: NewObjectValue(map[string]PresentValue{
				selector.FieldID: NewStringValue("text/plain"),
			}),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ResolveOperationRequest(operation, test.input); !errors.Is(err, ErrInvalidValue) {
				t.Fatalf("ResolveOperationRequest error = %v; want invalid value", err)
			}
		})
	}
}

func TestResolveOperationRequestRejectsOverlappingBodyAssignments(t *testing.T) {
	root := Schema{
		Type:       SchemaTypeObject,
		Required:   []string{},
		Properties: map[string]Schema{"name": {Type: SchemaTypeString}},
	}
	whole := operationInputBindingForTest("whole-body", "", true, false, root)
	whole.JSONPointer = ""
	nested := operationInputBindingForTest("body-name", "name", false, false, Schema{Type: SchemaTypeString})
	operation := Operation{
		Inputs: []FieldBinding{},
		RequestVariants: []RequestVariant{{
			VariantID:  "json",
			MediaType:  "application/json",
			Required:   true,
			Schema:     root,
			Inputs:     []FieldBinding{whole, nested},
			SourceRefs: []SourceRef{{SourceID: "fixture-source", Pointer: "#/requestBody"}},
		}},
	}

	empty, err := ResolveOperationRequest(operation, NewObjectValue(map[string]PresentValue{
		whole.FieldID: NewObjectValue(nil),
	}))
	if err != nil {
		t.Fatalf("ResolveOperationRequest(explicit empty object): %v", err)
	}
	fields, ok := empty.Body.Fields()
	if empty.Variant == nil || !ok || len(fields) != 0 {
		t.Fatalf("explicit empty object request = %#v", empty)
	}

	unrepresentable := operation
	unrepresentable.RequestVariants = []RequestVariant{operation.RequestVariants[0]}
	unrepresentable.RequestVariants[0].Inputs = []FieldBinding{nested}
	if _, err := ResolveOperationRequest(unrepresentable, NewObjectValue(nil)); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("all-optional required body without root binding error = %v; want invalid contract", err)
	}

	_, err = ResolveOperationRequest(operation, NewObjectValue(map[string]PresentValue{
		whole.FieldID:  NewObjectValue(map[string]PresentValue{"name": NewStringValue("Ada")}),
		nested.FieldID: NewStringValue("Grace"),
	}))
	if !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("overlapping body assignment error = %v; want invalid value", err)
	}
}

func TestValidateOperationInputEnumAndExactBounds(t *testing.T) {
	minimum := mustOperationInputInteger(t, "9007199254740993")
	maximum := mustOperationInputInteger(t, "9007199254740995")
	bindings := []FieldBinding{
		operationInputBindingForTest("mode", "mode", true, false, Schema{
			Type: SchemaTypeString,
			Enum: []PresentValue{NewStringValue("strict"), NewStringValue("safe")},
		}),
		operationInputBindingForTest("sequence", "sequence", true, false, Schema{
			Type:    SchemaTypeInteger,
			Minimum: &minimum,
			Maximum: &maximum,
		}),
	}
	operation := operationWithBodyBindingsForTest(bindings)
	within := mustOperationInputInteger(t, "9007199254740994")
	if err := ValidateOperationInput(operation, NewObjectValue(map[string]PresentValue{
		"mode":     NewStringValue("strict"),
		"sequence": within,
	})); err != nil {
		t.Fatalf("ValidateOperationInput(exact constrained values): %v", err)
	}

	below := mustOperationInputInteger(t, "9007199254740992")
	above := mustOperationInputInteger(t, "9007199254740996")
	for _, test := range []struct {
		name     string
		mode     string
		sequence PresentValue
	}{
		{name: "enum", mode: "other", sequence: within},
		{name: "minimum", mode: "strict", sequence: below},
		{name: "maximum", mode: "strict", sequence: above},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateOperationInput(operation, NewObjectValue(map[string]PresentValue{
				"mode":     NewStringValue(test.mode),
				"sequence": test.sequence,
			}))
			if !errors.Is(err, ErrInvalidValue) {
				t.Fatalf("constraint error = %v; want invalid value", err)
			}
		})
	}
	if err := (Schema{
		Type:    SchemaTypeInteger,
		Minimum: &maximum,
		Maximum: &minimum,
	}).Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("reversed exact bounds error = %v; want invalid contract", err)
	}
	if err := (Schema{
		Type: SchemaTypeInteger,
		Enum: []PresentValue{NewStringValue("1")},
	}).Validate(); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("wrong-type enum error = %v; want invalid contract", err)
	}
}

func operationWithBodyBindingsForTest(bindings []FieldBinding) Operation {
	root := Schema{
		Type:       SchemaTypeObject,
		Required:   []string{},
		Properties: make(map[string]Schema, len(bindings)),
	}
	for _, binding := range bindings {
		parts := decodeJSONPointerParts(binding.JSONPointer)
		if len(parts) != 1 {
			panic("operation input test binding must address one root property")
		}
		root.Properties[parts[0]] = binding.Schema
		if binding.Required {
			root.Required = append(root.Required, parts[0])
		}
	}
	return Operation{
		Inputs: []FieldBinding{},
		RequestVariants: []RequestVariant{{
			VariantID:  "application-json",
			MediaType:  "application/json",
			Required:   true,
			Schema:     root,
			Inputs:     bindings,
			SourceRefs: []SourceRef{{SourceID: "fixture-source", Pointer: "#/requestBody"}},
		}},
	}
}

func operationInputBindingForTest(fieldID, wireName string, required, nullable bool, schema Schema) FieldBinding {
	return FieldBinding{
		FieldID:       fieldID,
		WireName:      wireName,
		Location:      FieldLocationBody,
		JSONPointer:   "/" + wireName,
		Required:      required,
		Nullable:      nullable,
		Style:         "json",
		Explode:       false,
		AllowReserved: false,
		Sensitivity:   SensitivityPublic,
		Schema:        schema,
	}
}

func mustOperationInputInteger(t *testing.T, exact string) PresentValue {
	t.Helper()
	value, err := NewIntegerValue(exact)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustOperationInputDecimal(t *testing.T, exact string, precision, scale int) PresentValue {
	t.Helper()
	value, err := NewDecimalValue(exact, precision, scale)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func cloneOperationInputForTest(t *testing.T, input PresentValue) PresentValue {
	t.Helper()
	encoded, err := EncodePresentValue(input, "application/json")
	if err != nil {
		t.Fatal(err)
	}
	copy, err := DecodePresentValue(encoded, "application/json")
	if err != nil {
		t.Fatal(err)
	}
	return copy
}
