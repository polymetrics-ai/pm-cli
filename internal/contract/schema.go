package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"reflect"
	"strings"
	"unicode/utf8"
)

const (
	MaxContractDocumentBytes = 16 << 20
	MaxSchemaDepth           = 64
)

type SchemaType string

const (
	SchemaTypeString           SchemaType = "string"
	SchemaTypeBool             SchemaType = "bool"
	SchemaTypeInteger          SchemaType = "integer"
	SchemaTypeDecimal          SchemaType = "decimal"
	SchemaTypeDate             SchemaType = "date"
	SchemaTypeTimestampInstant SchemaType = "timestamp_instant"
	SchemaTypeTimestampLocal   SchemaType = "timestamp_local"
	SchemaTypeArray            SchemaType = "array"
	SchemaTypeObject           SchemaType = "object"
	SchemaTypeUnion            SchemaType = "union"
	SchemaTypeBinaryRef        SchemaType = "binary_ref"
)

type Schema struct {
	Type       SchemaType        `json:"type"`
	Nullable   bool              `json:"nullable,omitempty"`
	Required   []string          `json:"required,omitempty"`
	Properties map[string]Schema `json:"properties,omitempty"`
	Items      *Schema           `json:"items,omitempty"`
	OneOf      []Schema          `json:"oneOf,omitempty"`
	AllOf      []Schema          `json:"allOf,omitempty"`
}

func DecodeSchema(document []byte) (Schema, error) {
	var schema Schema
	if err := decodeContractJSON(document, &schema); err != nil {
		return Schema{}, err
	}
	if err := schema.Validate(); err != nil {
		return Schema{}, err
	}
	return schema, nil
}

func (s Schema) Validate() error {
	return validateSchema(s, "schema", 0)
}

func validateSchema(schema Schema, path string, depth int) error {
	if depth > MaxSchemaDepth {
		return contractError(ErrUnsupportedSchema, path, "schema nesting exceeds the supported depth")
	}
	if !validSchemaType(schema.Type) {
		return contractError(ErrUnsupportedSchema, path+".type", "logical type is unsupported")
	}
	for index, component := range schema.AllOf {
		if component.Type != schema.Type {
			return contractError(ErrUnsupportedSchema, fmt.Sprintf("%s.allOf[%d].type", path, index), "allOf component type must match its parent")
		}
		if component.Nullable != schema.Nullable {
			return contractError(ErrUnsupportedSchema, fmt.Sprintf("%s.allOf[%d].nullable", path, index), "allOf component nullability must match its parent")
		}
		if err := validateSchema(component, fmt.Sprintf("%s.allOf[%d]", path, index), depth+1); err != nil {
			return err
		}
	}
	if schema.AllOf != nil && len(schema.AllOf) == 0 {
		return contractError(ErrUnsupportedSchema, path+".allOf", "must contain at least one component")
	}

	switch schema.Type {
	case SchemaTypeObject:
		if schema.Items != nil || schema.OneOf != nil {
			return contractError(ErrUnsupportedSchema, path, "object cannot declare items or oneOf")
		}
		seenRequired := make(map[string]struct{}, len(schema.Required))
		for _, name := range schema.Required {
			if !utf8.ValidString(name) {
				return contractError(ErrInvalidContract, path+".required", "provider property name must retain exact valid UTF-8")
			}
			if _, duplicate := seenRequired[name]; duplicate {
				return contractError(ErrDuplicateField, path+".required", "required property is duplicated")
			}
			seenRequired[name] = struct{}{}
			if _, declared := schema.Properties[name]; !declared {
				return contractError(ErrInvalidContract, path+".required", "required property is not declared")
			}
		}
		for name, property := range schema.Properties {
			if !utf8.ValidString(name) {
				return contractError(ErrInvalidContract, path+".properties", "provider property name must retain exact valid UTF-8")
			}
			if err := validateSchema(property, path+".properties", depth+1); err != nil {
				return err
			}
		}
	case SchemaTypeArray:
		if schema.Items == nil {
			return contractError(ErrUnsupportedSchema, path+".items", "array items must be declared")
		}
		if schema.Properties != nil || schema.Required != nil || schema.OneOf != nil || schema.AllOf != nil {
			return contractError(ErrUnsupportedSchema, path, "array cannot declare object, union, or allOf shape")
		}
		if err := validateSchema(*schema.Items, path+".items", depth+1); err != nil {
			return err
		}
	case SchemaTypeUnion:
		if len(schema.OneOf) < 2 {
			return contractError(ErrUnsupportedSchema, path+".oneOf", "union must preserve at least two variants")
		}
		if schema.Items != nil || schema.Properties != nil || schema.Required != nil || schema.AllOf != nil {
			return contractError(ErrUnsupportedSchema, path, "union cannot combine oneOf with object, array, or allOf shape")
		}
		for index, variant := range schema.OneOf {
			if err := validateSchema(variant, fmt.Sprintf("%s.oneOf[%d]", path, index), depth+1); err != nil {
				return err
			}
		}
	default:
		if schema.Items != nil || schema.Properties != nil || schema.Required != nil || schema.OneOf != nil || schema.AllOf != nil {
			return contractError(ErrUnsupportedSchema, path, "scalar cannot declare object, array, union, or allOf shape")
		}
	}
	return nil
}

func validSchemaType(value SchemaType) bool {
	switch value {
	case SchemaTypeString, SchemaTypeBool, SchemaTypeInteger, SchemaTypeDecimal,
		SchemaTypeDate, SchemaTypeTimestampInstant, SchemaTypeTimestampLocal,
		SchemaTypeArray, SchemaTypeObject, SchemaTypeUnion, SchemaTypeBinaryRef:
		return true
	default:
		return false
	}
}

func isJSONMediaType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

func validateMediaType(value string) error {
	if value == "" {
		return contractError(ErrIncompatibleMedia, "media_type", "must not be empty")
	}
	if _, _, err := mime.ParseMediaType(value); err != nil {
		return contractError(ErrIncompatibleMedia, "media_type", "is malformed")
	}
	return nil
}

func decodeContractJSON(data []byte, target any) error {
	return decodeJSON(data, target, true)
}

func decodeStrictJSON(data []byte, target any) error {
	return decodeJSON(data, target, false)
}

func decodeJSON(data []byte, target any, rejectNull bool) error {
	if len(data) == 0 {
		return contractError(ErrInvalidContract, "$", "document is empty")
	}
	if len(data) > MaxContractDocumentBytes {
		return contractError(ErrInvalidContract, "$", "document exceeds the contract byte limit")
	}
	if err := validateJSONUnicode(data); err != nil {
		return err
	}
	if err := validateStrictJSON(data, rejectNull, reflect.TypeOf(target)); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		var contractFailure *ContractError
		if errors.As(err, &contractFailure) {
			return err
		}
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			return contractError(ErrUnknownField, "$", "document contains an unsupported field")
		}
		return contractError(ErrInvalidContract, "$", "document has a wrong field type or malformed value")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return contractError(ErrInvalidContract, "$", "document contains a trailing value")
	}
	return nil
}

func validateJSONUnicode(data []byte) error {
	if !utf8.Valid(data) {
		return contractError(ErrInvalidContract, "$", "document contains invalid UTF-8")
	}
	inString := false
	for index := 0; index < len(data); index++ {
		switch data[index] {
		case '"':
			inString = !inString
		case '\\':
			if !inString || index+1 >= len(data) {
				continue
			}
			if data[index+1] != 'u' {
				index++
				continue
			}
			code, ok := decodeHexQuad(data[index+2:])
			if !ok {
				continue
			}
			switch {
			case code >= 0xd800 && code <= 0xdbff:
				if index+12 > len(data) || data[index+6] != '\\' || data[index+7] != 'u' {
					return contractError(ErrInvalidContract, "$", "document contains an unpaired UTF-16 surrogate")
				}
				low, validLow := decodeHexQuad(data[index+8:])
				if !validLow || low < 0xdc00 || low > 0xdfff {
					return contractError(ErrInvalidContract, "$", "document contains an unpaired UTF-16 surrogate")
				}
				index += 11
			case code >= 0xdc00 && code <= 0xdfff:
				return contractError(ErrInvalidContract, "$", "document contains an unpaired UTF-16 surrogate")
			default:
				index += 5
			}
		}
	}
	return nil
}

func decodeHexQuad(data []byte) (uint16, bool) {
	if len(data) < 4 {
		return 0, false
	}
	var value uint16
	for _, character := range data[:4] {
		value <<= 4
		switch {
		case character >= '0' && character <= '9':
			value |= uint16(character - '0')
		case character >= 'a' && character <= 'f':
			value |= uint16(character-'a') + 10
		case character >= 'A' && character <= 'F':
			value |= uint16(character-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

func validateStrictJSON(data []byte, rejectNull bool, target reflect.Type) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanStrictJSONValue(decoder, "$", 0, rejectNull, target); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return contractError(ErrInvalidContract, "$", "document contains a trailing value")
	}
	return nil
}

func scanStrictJSONValue(decoder *json.Decoder, path string, depth int, rejectNull bool, target reflect.Type) error {
	if depth > MaxSchemaDepth+16 {
		return contractError(ErrInvalidContract, path, "JSON nesting exceeds the supported depth")
	}
	token, err := decoder.Token()
	if err != nil {
		return contractError(ErrInvalidContract, path, "malformed JSON")
	}
	if rejectNull && token == nil {
		return contractError(ErrInvalidContract, path, "raw JSON null is not a typed contract value")
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	target = strictJSONType(target)
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return contractError(ErrInvalidContract, path, "malformed object key")
			}
			key, ok := keyToken.(string)
			if !ok {
				return contractError(ErrInvalidContract, path, "object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return contractError(ErrDuplicateField, path, "object field appears more than once")
			}
			seen[key] = struct{}{}
			fieldType, declared := strictJSONFieldType(target, key)
			if !declared {
				return contractError(ErrUnknownField, path, "document contains an unsupported or wrongly cased field")
			}
			if err := scanStrictJSONValue(decoder, path+".field", depth+1, rejectNull, fieldType); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return contractError(ErrInvalidContract, path, "malformed object")
		}
	case '[':
		var elementType reflect.Type
		if target != nil && (target.Kind() == reflect.Array || target.Kind() == reflect.Slice) {
			elementType = target.Elem()
		}
		index := 0
		for decoder.More() {
			if err := scanStrictJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index), depth+1, rejectNull, elementType); err != nil {
				return err
			}
			index++
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return contractError(ErrInvalidContract, path, "malformed array")
		}
	default:
		return contractError(ErrInvalidContract, path, "unexpected JSON delimiter")
	}
	return nil
}

// Custom decoders enforce their own wire shape through the same strict decoder.
// Ordinary structs retain exact JSON tags; map keys remain provider-owned data.
func strictJSONType(target reflect.Type) reflect.Type {
	for target != nil {
		if target.Implements(reflect.TypeFor[json.Unmarshaler]()) {
			return nil
		}
		if target.Kind() != reflect.Pointer {
			break
		}
		target = target.Elem()
	}
	if target != nil && reflect.PointerTo(target).Implements(reflect.TypeFor[json.Unmarshaler]()) {
		return nil
	}
	return target
}

func strictJSONFieldType(target reflect.Type, key string) (reflect.Type, bool) {
	if target == nil {
		return nil, true
	}
	switch target.Kind() {
	case reflect.Map:
		return target.Elem(), true
	case reflect.Struct:
		for index := range target.NumField() {
			field := target.Field(index)
			if !field.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			if name == key {
				return field.Type, true
			}
		}
		return nil, false
	default:
		return nil, true
	}
}
