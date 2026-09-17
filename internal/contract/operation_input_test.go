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
	operation := Operation{Inputs: bindings}

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
		nullable := Operation{Inputs: []FieldBinding{
			operationInputBindingForTest("note", "note", true, true, Schema{Type: SchemaTypeString, Nullable: true}),
		}}
		if err := ValidateOperationInput(nullable, NewObjectValue(map[string]PresentValue{"note": NullValue()})); err != nil {
			t.Fatalf("explicit nullable input: %v", err)
		}
		if err := ValidateOperationInput(nullable, NewObjectValue(map[string]PresentValue{"note": AbsentValue()})); !errors.Is(err, ErrInvalidValue) {
			t.Fatalf("absent required nullable input error = %v; want invalid value", err)
		}
		nonnullable := Operation{Inputs: []FieldBinding{
			operationInputBindingForTest("note", "note", true, false, Schema{Type: SchemaTypeString}),
		}}
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
	operation := Operation{Inputs: []FieldBinding{
		operationInputBindingForTest("payload", "payload", true, false, payloadSchema),
	}}
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
	operation := Operation{Inputs: []FieldBinding{
		operationInputBindingForTest("composed", "composed", true, false, composed),
		operationInputBindingForTest("choice", "choice", true, false, union),
	}}
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
	ambiguousOperation := Operation{Inputs: []FieldBinding{
		operationInputBindingForTest("choice", "choice", true, false, ambiguousSchema),
	}}
	ambiguous, err := NewUnionValue("ambiguous", NewObjectValue(map[string]PresentValue{"flag": NewBooleanValue(false)}))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateOperationInput(ambiguousOperation, NewObjectValue(map[string]PresentValue{"choice": ambiguous})); !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("ambiguous oneOf error = %v; want invalid value", err)
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
