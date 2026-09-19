package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

var (
	ErrUnexpectedHTTPResponse = errors.New("unexpected HTTP response")
	ErrInvalidProviderValue   = errors.New("provider response violates the typed schema")
	ErrBinaryResponseBoundary = errors.New("binary response requires the shared object publisher")
)

type DecodedResponse struct {
	StatusCode int
	Header     http.Header
	Document   contract.PresentValue
	Result     *contract.ActionResult
}

// DecodeResponse selects the exact status/default and media variant before it
// converts ordinary provider JSON into PresentValue. A no-content response is
// represented by a valid ActionResult whose data is absent and media omitted.
func DecodeResponse(
	operation contract.Operation,
	statusCode int,
	header http.Header,
	body []byte,
) (DecodedResponse, error) {
	variants := selectStatusVariants(operation.Responses, statusCode)
	if len(variants) == 0 {
		return DecodedResponse{}, fmt.Errorf("%w: status is not source-declared", ErrUnexpectedHTTPResponse)
	}
	decoded := DecodedResponse{StatusCode: statusCode, Header: make(http.Header), Document: contract.AbsentValue()}
	for _, variant := range variants {
		if !variant.NoContent {
			continue
		}
		if len(variants) != 1 {
			return DecodedResponse{}, fmt.Errorf("%w: ambiguous no-content response", ErrUnexpectedHTTPResponse)
		}
		if len(body) != 0 {
			return DecodedResponse{}, fmt.Errorf("%w: no-content response carried a body", ErrUnexpectedHTTPResponse)
		}
		result := contract.ActionResult{Data: contract.AbsentValue()}
		if err := result.Validate(); err != nil {
			return DecodedResponse{}, fmt.Errorf("%w: no-content action result is invalid", ErrInvalidProviderValue)
		}
		decoded.Result = &result
		return decoded, nil
	}
	encodings := header.Values("Content-Encoding")
	if len(encodings) > 1 {
		return DecodedResponse{}, fmt.Errorf("%w: content encoding is ambiguous", ErrUnexpectedHTTPResponse)
	}
	if len(encodings) == 1 {
		encoding := strings.TrimSpace(encodings[0])
		if encoding != "" && !strings.EqualFold(encoding, "identity") {
			return DecodedResponse{}, fmt.Errorf("%w: content encoding is not admitted", ErrUnexpectedHTTPResponse)
		}
	}
	contentTypes := header.Values("Content-Type")
	if len(contentTypes) != 1 || strings.TrimSpace(contentTypes[0]) == "" {
		return DecodedResponse{}, fmt.Errorf("%w: content type is absent or ambiguous", ErrUnexpectedHTTPResponse)
	}
	contentType := strings.TrimSpace(contentTypes[0])
	variant, ok := selectMediaVariant(variants, contentType)
	if !ok || variant.Schema == nil {
		return DecodedResponse{}, fmt.Errorf("%w: media type is not source-declared", ErrUnexpectedHTTPResponse)
	}
	if variant.Schema.Type == contract.SchemaTypeBinaryRef {
		return DecodedResponse{}, ErrBinaryResponseBoundary
	}
	if !isJSONWireMediaType(variant.MediaType) {
		return DecodedResponse{}, fmt.Errorf("%w: typed response media is not JSON", ErrUnexpectedHTTPResponse)
	}
	value, err := decodeProviderJSON(body, *variant.Schema)
	if err != nil {
		return DecodedResponse{}, err
	}
	resultValue := value
	if variant.RecordPointer != nil {
		resultValue, err = valueAtJSONPointer(value, *variant.RecordPointer)
		if err != nil {
			return DecodedResponse{}, fmt.Errorf("%w: record pointer does not identify a typed value", ErrInvalidProviderValue)
		}
	}
	result := contract.ActionResult{MediaType: variant.MediaType, Data: resultValue}
	if err := result.Validate(); err != nil {
		return DecodedResponse{}, fmt.Errorf("%w: action result is invalid: %v", ErrInvalidProviderValue, err)
	}
	decoded.Document = value
	decoded.Result = &result
	return decoded, nil
}

func selectStatusVariants(responses []contract.Response, statusCode int) []contract.Response {
	exactStatus := strconv.Itoa(statusCode)
	exact := make([]contract.Response, 0)
	fallback := make([]contract.Response, 0)
	for _, response := range responses {
		switch response.Status {
		case exactStatus:
			exact = append(exact, response)
		case "default":
			fallback = append(fallback, response)
		}
	}
	if len(exact) != 0 {
		return exact
	}
	return fallback
}

func selectMediaVariant(variants []contract.Response, actual string) (contract.Response, bool) {
	actualType, actualParams, err := mime.ParseMediaType(actual)
	if err != nil {
		return contract.Response{}, false
	}
	var selected contract.Response
	found := false
	for _, variant := range variants {
		declaredType, declaredParams, parseErr := mime.ParseMediaType(variant.MediaType)
		if parseErr != nil || !strings.EqualFold(actualType, declaredType) {
			continue
		}
		matched := true
		for name, value := range declaredParams {
			if actualParams[name] != value {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		if found {
			return contract.Response{}, false
		}
		selected = variant
		found = true
	}
	return selected, found
}

func decodeProviderJSON(document []byte, schema contract.Schema) (contract.PresentValue, error) {
	if !utf8.Valid(document) {
		return contract.PresentValue{}, fmt.Errorf("%w: JSON is not valid UTF-8", ErrInvalidProviderValue)
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	raw, err := decodeProviderJSONNode(decoder, 0)
	if err != nil {
		return contract.PresentValue{}, fmt.Errorf("%w: malformed or ambiguous JSON", ErrInvalidProviderValue)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return contract.PresentValue{}, fmt.Errorf("%w: trailing JSON data", ErrInvalidProviderValue)
	}
	value, err := typedProviderValue(raw, schema, 0)
	if err != nil {
		return contract.PresentValue{}, err
	}
	return value, nil
}

func decodeProviderJSONNode(decoder *json.Decoder, depth int) (any, error) {
	if depth > contract.MaxSchemaDepth {
		return nil, ErrInvalidProviderValue
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := make(map[string]any)
		for decoder.More() {
			nameToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			name, ok := nameToken.(string)
			if !ok {
				return nil, ErrInvalidProviderValue
			}
			if _, duplicate := object[name]; duplicate {
				return nil, ErrInvalidProviderValue
			}
			value, err := decodeProviderJSONNode(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[name] = value
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return nil, ErrInvalidProviderValue
		}
		return object, nil
	case '[':
		array := make([]any, 0)
		for decoder.More() {
			value, err := decodeProviderJSONNode(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return nil, ErrInvalidProviderValue
		}
		return array, nil
	default:
		return nil, ErrInvalidProviderValue
	}
}

func typedProviderValue(raw any, schema contract.Schema, depth int) (contract.PresentValue, error) {
	if depth > contract.MaxSchemaDepth {
		return contract.PresentValue{}, fmt.Errorf("%w: value nesting exceeds the schema bound", ErrInvalidProviderValue)
	}
	if raw == nil {
		if !schema.Nullable {
			return contract.PresentValue{}, fmt.Errorf("%w: unexpected null", ErrInvalidProviderValue)
		}
		return contract.NullValue(), nil
	}
	var value contract.PresentValue
	var err error
	switch schema.Type {
	case contract.SchemaTypeString:
		text, ok := raw.(string)
		if !ok {
			return contract.PresentValue{}, typeMismatch()
		}
		value = contract.NewStringValue(text)
	case contract.SchemaTypeBool:
		boolean, ok := raw.(bool)
		if !ok {
			return contract.PresentValue{}, typeMismatch()
		}
		value = contract.NewBooleanValue(boolean)
	case contract.SchemaTypeInteger:
		number, ok := raw.(json.Number)
		if !ok {
			return contract.PresentValue{}, typeMismatch()
		}
		value, err = contract.NewIntegerValue(number.String())
	case contract.SchemaTypeDecimal:
		number, ok := raw.(json.Number)
		if !ok {
			return contract.PresentValue{}, typeMismatch()
		}
		exact := number.String()
		if strings.ContainsAny(exact, "eE") {
			return contract.PresentValue{}, fmt.Errorf("%w: exponential decimal cannot preserve declared scale", ErrInvalidProviderValue)
		}
		if !strings.Contains(exact, ".") {
			return contract.PresentValue{}, fmt.Errorf("%w: decimal scale is absent", ErrInvalidProviderValue)
		}
		precision, scale, shapeOK := decimalShape(exact)
		if !shapeOK {
			return contract.PresentValue{}, typeMismatch()
		}
		value, err = contract.NewDecimalValue(exact, precision, scale)
	case contract.SchemaTypeDate:
		text, ok := raw.(string)
		if !ok {
			return contract.PresentValue{}, typeMismatch()
		}
		value, err = contract.NewDateValue(text)
	case contract.SchemaTypeTimestampInstant:
		text, ok := raw.(string)
		if !ok {
			return contract.PresentValue{}, typeMismatch()
		}
		value, err = contract.NewInstantValue(text)
	case contract.SchemaTypeTimestampLocal:
		text, ok := raw.(string)
		if !ok {
			return contract.PresentValue{}, typeMismatch()
		}
		value, err = contract.NewLocalTimestampValue(text)
	case contract.SchemaTypeArray:
		rawElements, ok := raw.([]any)
		if !ok || schema.Items == nil {
			return contract.PresentValue{}, typeMismatch()
		}
		elements := make([]contract.PresentValue, 0, len(rawElements))
		for _, rawElement := range rawElements {
			element, elementErr := typedProviderValue(rawElement, *schema.Items, depth+1)
			if elementErr != nil {
				return contract.PresentValue{}, elementErr
			}
			elements = append(elements, element)
		}
		value = contract.NewArrayValue(elements)
	case contract.SchemaTypeObject:
		rawFields, ok := raw.(map[string]any)
		if !ok {
			return contract.PresentValue{}, typeMismatch()
		}
		properties, required, mergeErr := mergedObjectSchema(schema)
		if mergeErr != nil {
			return contract.PresentValue{}, mergeErr
		}
		fields := make(map[string]contract.PresentValue, len(rawFields))
		for name, rawField := range rawFields {
			property, declared := properties[name]
			if !declared {
				// A declared-open object retains properties the pinned source
				// does not enumerate; a closed object still refuses them.
				if !schema.OpenProperties {
					return contract.PresentValue{}, fmt.Errorf("%w: undeclared provider object field", ErrInvalidProviderValue)
				}
				if schema.AdditionalProperties != nil {
					property = *schema.AdditionalProperties
				} else {
					field, fieldErr := exactProviderValue(rawField, depth+1)
					if fieldErr != nil {
						return contract.PresentValue{}, fieldErr
					}
					fields[name] = field
					continue
				}
			}
			field, fieldErr := typedProviderValue(rawField, property, depth+1)
			if fieldErr != nil {
				return contract.PresentValue{}, fieldErr
			}
			fields[name] = field
		}
		for name := range required {
			if _, present := fields[name]; !present {
				return contract.PresentValue{}, fmt.Errorf("%w: required provider object field is absent", ErrInvalidProviderValue)
			}
		}
		value = contract.NewObjectValue(fields)
	case contract.SchemaTypeUnion:
		matched := make([]contract.PresentValue, 0, 1)
		matchedIndex := -1
		for index, variant := range schema.OneOf {
			candidate, candidateErr := typedProviderValue(raw, variant, depth+1)
			if candidateErr == nil {
				matched = append(matched, candidate)
				matchedIndex = index
			}
		}
		if len(matched) != 1 {
			return contract.PresentValue{}, fmt.Errorf("%w: union value does not select exactly one variant", ErrInvalidProviderValue)
		}
		value, err = contract.NewUnionValue("variant-"+strconv.Itoa(matchedIndex), matched[0])
	case contract.SchemaTypeBinaryRef:
		return contract.PresentValue{}, ErrBinaryResponseBoundary
	default:
		return contract.PresentValue{}, typeMismatch()
	}
	if err != nil {
		return contract.PresentValue{}, fmt.Errorf("%w: typed conversion failed", ErrInvalidProviderValue)
	}
	if err := validateValueConstraints(value, schema); err != nil {
		return contract.PresentValue{}, err
	}
	return value, nil
}

// exactProviderValue converts one decoded JSON node that the declared schema
// does not enumerate. A declared-open object retains every property the provider
// sent, so this path preserves exact numbers, explicit null, nested arrays and
// nested objects instead of refusing or coercing them.
func exactProviderValue(raw any, depth int) (contract.PresentValue, error) {
	if depth > contract.MaxSchemaDepth {
		return contract.PresentValue{}, fmt.Errorf("%w: value nesting exceeds the schema bound", ErrInvalidProviderValue)
	}
	switch typed := raw.(type) {
	case nil:
		return contract.NullValue(), nil
	case bool:
		return contract.NewBooleanValue(typed), nil
	case string:
		return contract.NewStringValue(typed), nil
	case json.Number:
		exact := typed.String()
		if strings.ContainsAny(exact, "eE") {
			return contract.PresentValue{}, fmt.Errorf("%w: exponential number cannot preserve its exact value", ErrInvalidProviderValue)
		}
		if strings.Contains(exact, ".") {
			precision, scale, shapeOK := decimalShape(exact)
			if !shapeOK {
				return contract.PresentValue{}, typeMismatch()
			}
			return contract.NewDecimalValue(exact, precision, scale)
		}
		return contract.NewIntegerValue(exact)
	case []any:
		elements := make([]contract.PresentValue, 0, len(typed))
		for _, rawElement := range typed {
			element, err := exactProviderValue(rawElement, depth+1)
			if err != nil {
				return contract.PresentValue{}, err
			}
			elements = append(elements, element)
		}
		return contract.NewArrayValue(elements), nil
	case map[string]any:
		fields := make(map[string]contract.PresentValue, len(typed))
		for name, rawField := range typed {
			field, err := exactProviderValue(rawField, depth+1)
			if err != nil {
				return contract.PresentValue{}, err
			}
			fields[name] = field
		}
		return contract.NewObjectValue(fields), nil
	default:
		return contract.PresentValue{}, typeMismatch()
	}
}

func mergedObjectSchema(schema contract.Schema) (map[string]contract.Schema, map[string]struct{}, error) {
	properties := make(map[string]contract.Schema)
	required := make(map[string]struct{})
	var merge func(contract.Schema) error
	merge = func(component contract.Schema) error {
		for name, property := range component.Properties {
			if existing, duplicate := properties[name]; duplicate {
				if !reflect.DeepEqual(existing, property) {
					return fmt.Errorf("%w: conflicting allOf property", ErrInvalidProviderValue)
				}
			} else {
				properties[name] = property
			}
		}
		for _, name := range component.Required {
			required[name] = struct{}{}
		}
		for _, child := range component.AllOf {
			if err := merge(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := merge(schema); err != nil {
		return nil, nil, err
	}
	return properties, required, nil
}

func validateValueConstraints(value contract.PresentValue, schema contract.Schema) error {
	if len(schema.Enum) != 0 {
		matched := false
		for _, candidate := range schema.Enum {
			if value.Equal(candidate) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%w: value is outside its enum", ErrInvalidProviderValue)
		}
	}
	if schema.Minimum == nil && schema.Maximum == nil {
		return nil
	}
	number, ok := value.ExactNumber()
	if !ok {
		return fmt.Errorf("%w: numeric constraint applied to non-number", ErrInvalidProviderValue)
	}
	parsed := new(big.Rat)
	if _, ok := parsed.SetString(number.Text); !ok {
		return fmt.Errorf("%w: number is not exact", ErrInvalidProviderValue)
	}
	for _, bound := range []struct {
		value   *contract.PresentValue
		minimum bool
	}{{schema.Minimum, true}, {schema.Maximum, false}} {
		if bound.value == nil {
			continue
		}
		exact, _ := bound.value.ExactNumber()
		parsedBound := new(big.Rat)
		if _, ok := parsedBound.SetString(exact.Text); !ok {
			return fmt.Errorf("%w: constraint is not exact", ErrInvalidProviderValue)
		}
		comparison := parsed.Cmp(parsedBound)
		if (bound.minimum && comparison < 0) || (!bound.minimum && comparison > 0) {
			return fmt.Errorf("%w: number is outside its declared range", ErrInvalidProviderValue)
		}
	}
	return nil
}

func valueAtJSONPointer(value contract.PresentValue, pointer string) (contract.PresentValue, error) {
	parts, err := decodePointerSegments(pointer)
	if err != nil {
		return contract.PresentValue{}, ErrInvalidProviderValue
	}
	current := value
	for _, part := range parts {
		if fields, ok := current.Fields(); ok {
			next, present := fields[part]
			if !present {
				return contract.PresentValue{}, ErrInvalidProviderValue
			}
			current = next
			continue
		}
		if elements, ok := current.Elements(); ok {
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(elements) || strconv.Itoa(index) != part {
				return contract.PresentValue{}, ErrInvalidProviderValue
			}
			current = elements[index]
			continue
		}
		return contract.PresentValue{}, ErrInvalidProviderValue
	}
	return current, nil
}

func optionalValueAtJSONPointer(value contract.PresentValue, pointer string) (contract.PresentValue, bool, error) {
	parts, err := decodePointerSegments(pointer)
	if err != nil {
		return contract.PresentValue{}, false, ErrInvalidProviderValue
	}
	current := value
	for _, part := range parts {
		if fields, ok := current.Fields(); ok {
			next, present := fields[part]
			if !present {
				return contract.AbsentValue(), false, nil
			}
			current = next
			continue
		}
		if elements, ok := current.Elements(); ok {
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(elements) || strconv.Itoa(index) != part {
				return contract.PresentValue{}, false, ErrInvalidProviderValue
			}
			current = elements[index]
			continue
		}
		return contract.PresentValue{}, false, ErrInvalidProviderValue
	}
	return current, true, nil
}

func decodePointerSegments(pointer string) ([]string, error) {
	if pointer == "" {
		return []string{}, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, ErrInvalidProviderValue
	}
	encodedParts := strings.Split(pointer[1:], "/")
	parts := make([]string, len(encodedParts))
	for index, encoded := range encodedParts {
		var decoded strings.Builder
		decoded.Grow(len(encoded))
		for offset := 0; offset < len(encoded); offset++ {
			if encoded[offset] != '~' {
				decoded.WriteByte(encoded[offset])
				continue
			}
			if offset+1 >= len(encoded) {
				return nil, ErrInvalidProviderValue
			}
			offset++
			switch encoded[offset] {
			case '0':
				decoded.WriteByte('~')
			case '1':
				decoded.WriteByte('/')
			default:
				return nil, ErrInvalidProviderValue
			}
		}
		parts[index] = decoded.String()
	}
	return parts, nil
}

func decimalShape(value string) (int, int, bool) {
	unsigned := strings.TrimPrefix(value, "-")
	parts := strings.Split(unsigned, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || (len(parts[0]) > 1 && parts[0][0] == '0') {
		return 0, 0, false
	}
	for _, part := range parts {
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return 0, 0, false
			}
		}
	}
	return len(parts[0]) + len(parts[1]), len(parts[1]), true
}

func schemaContainsBinaryRef(schema contract.Schema) bool {
	if schema.Type == contract.SchemaTypeBinaryRef {
		return true
	}
	if schema.Items != nil && schemaContainsBinaryRef(*schema.Items) {
		return true
	}
	for _, property := range schema.Properties {
		if schemaContainsBinaryRef(property) {
			return true
		}
	}
	for _, variant := range schema.OneOf {
		if schemaContainsBinaryRef(variant) {
			return true
		}
	}
	for _, component := range schema.AllOf {
		if schemaContainsBinaryRef(component) {
			return true
		}
	}
	return false
}

func typeMismatch() error {
	return fmt.Errorf("%w: JSON type does not match the schema", ErrInvalidProviderValue)
}
