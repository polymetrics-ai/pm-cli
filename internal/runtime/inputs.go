package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/credentials"
)

var (
	ErrInvalidHTTPInput     = errors.New("invalid typed HTTP input")
	ErrUnsupportedWireValue = errors.New("typed value cannot be represented on the declared HTTP wire")
)

type EncodedInputs struct {
	EscapedPath   string
	RawQuery      string
	Header        http.Header
	Body          []byte
	BodyReference *contract.ObjectRef
	MediaType     string
	EncodedBytes  uint64
}

// EncodeInputs validates the FieldID envelope, preserves absence/null/value,
// and produces ordinary provider wire values. Source defaults are retained in
// the contract but are never materialized into an absent caller field.
func EncodeInputs(
	operation contract.Operation,
	input contract.PresentValue,
) (EncodedInputs, error) {
	var httpFacts *contract.OperationHTTP
	if operation.Execution != nil {
		httpFacts = operation.Execution.HTTP
	}
	var authorization *contract.OperationAuthorization
	if httpFacts != nil {
		if err := httpFacts.Validate(operation); err != nil {
			return EncodedInputs{}, fmt.Errorf("%w: invalid source HTTP facts", ErrInvalidHTTPInput)
		}
		authorization = httpFacts.Authorization
	}
	if operation.Transport.Kind != contract.TransportKindHTTP {
		return EncodedInputs{}, fmt.Errorf("%w: selected operation is not HTTP", ErrInvalidHTTPInput)
	}
	fields, ok := input.Fields()
	if !ok {
		return EncodedInputs{}, fmt.Errorf("%w: input must be a present object", ErrInvalidHTTPInput)
	}
	validationOperation := operation
	if operation.Inputs != nil {
		validationOperation.Inputs = make([]contract.FieldBinding, len(operation.Inputs))
		copy(validationOperation.Inputs, operation.Inputs)
	}
	if authorization != nil {
		if supplied, exists := fields[authorization.FieldID]; exists && supplied.Presence() != contract.PresenceAbsent {
			return EncodedInputs{}, fmt.Errorf("%w: credential input must be resolved by the credential boundary", ErrInvalidHTTPInput)
		}
		binding, found := findCommonBinding(operation, authorization.FieldID)
		wireNameMatches := found && binding.WireName == authorization.WireName
		if found && binding.Location == contract.FieldLocationHeader {
			wireNameMatches = strings.EqualFold(binding.WireName, authorization.WireName)
		}
		if !found || binding.Sensitivity != contract.SensitivitySecret ||
			binding.Location != authorization.Placement || !wireNameMatches ||
			binding.Required != authorization.Required {
			return EncodedInputs{}, fmt.Errorf("%w: authorization does not match a source-declared secret binding", ErrInvalidHTTPInput)
		}
		if authorization.Required {
			for index := range validationOperation.Inputs {
				if validationOperation.Inputs[index].FieldID == authorization.FieldID {
					validationOperation.Inputs[index].Required = false
				}
			}
		}
	}
	for _, binding := range operation.Inputs {
		if binding.Sensitivity != contract.SensitivitySecret {
			continue
		}
		value, supplied := fields[binding.FieldID]
		if supplied && value.Presence() != contract.PresenceAbsent {
			return EncodedInputs{}, fmt.Errorf("%w: secret values cannot enter the ordinary input envelope", ErrInvalidHTTPInput)
		}
		if binding.Required && (authorization == nil || binding.FieldID != authorization.FieldID) {
			return EncodedInputs{}, fmt.Errorf("%w: required secret binding lacks admitted authorization semantics", ErrInvalidHTTPInput)
		}
	}
	resolved, err := contract.ResolveOperationRequest(validationOperation, contract.NewObjectValue(fields))
	if err != nil {
		return EncodedInputs{}, fmt.Errorf("%w: %v", ErrInvalidHTTPInput, err)
	}

	encoded := EncodedInputs{
		EscapedPath: operation.Transport.Path,
		Header:      make(http.Header),
	}
	query := make([]wirePair, 0)
	cookies := make([]wirePair, 0)
	for _, binding := range operation.Inputs {
		if binding.Sensitivity == contract.SensitivitySecret {
			continue
		}
		if binding.FieldID == operation.RequestMediaFieldID {
			continue
		}
		value, supplied := fields[binding.FieldID]
		if !supplied || value.Presence() == contract.PresenceAbsent {
			continue
		}
		switch binding.Location {
		case contract.FieldLocationPath:
			wire, wireErr := encodePathBinding(binding, value)
			if wireErr != nil {
				return EncodedInputs{}, wireErr
			}
			marker := "{" + binding.WireName + "}"
			if strings.Count(encoded.EscapedPath, marker) != 1 {
				return EncodedInputs{}, fmt.Errorf("%w: path template binding is not exact", ErrInvalidHTTPInput)
			}
			encoded.EscapedPath = strings.Replace(encoded.EscapedPath, marker, wire, 1)
		case contract.FieldLocationQuery:
			pairs, wireErr := encodeParameter(binding, value)
			if wireErr != nil {
				return EncodedInputs{}, wireErr
			}
			for index := range pairs {
				pairs[index].allowReserved = binding.AllowReserved
			}
			query = append(query, pairs...)
		case contract.FieldLocationHeader:
			pairs, wireErr := encodeParameter(binding, value)
			if wireErr != nil || len(pairs) != 1 {
				return EncodedInputs{}, fmt.Errorf("%w: header value is not representable", ErrUnsupportedWireValue)
			}
			if !validHTTPToken(binding.WireName) || forbiddenRequestHeader(binding.WireName) ||
				!validHTTPHeaderValue(pairs[0].value) {
				return EncodedInputs{}, fmt.Errorf("%w: forbidden request header", ErrUnsupportedWireValue)
			}
			encoded.Header.Add(binding.WireName, pairs[0].value)
		case contract.FieldLocationCookie:
			pairs, wireErr := encodeParameter(binding, value)
			if wireErr != nil {
				return EncodedInputs{}, wireErr
			}
			cookies = append(cookies, pairs...)
		case contract.FieldLocationRecord, contract.FieldLocationMetadata:
			return EncodedInputs{}, fmt.Errorf("%w: record and metadata bindings are not HTTP request fields", ErrInvalidHTTPInput)
		default:
			return EncodedInputs{}, fmt.Errorf("%w: unsupported common field location", ErrInvalidHTTPInput)
		}
	}
	if strings.Contains(encoded.EscapedPath, "{") || !strings.HasPrefix(encoded.EscapedPath, "/") || strings.HasPrefix(encoded.EscapedPath, "//") {
		return EncodedInputs{}, fmt.Errorf("%w: path template is unresolved or not origin-relative", ErrInvalidHTTPInput)
	}
	encoded.RawQuery = encodeQueryPairs(query)
	if len(cookies) != 0 {
		parts := make([]string, 0, len(cookies))
		for _, cookie := range cookies {
			if !validCookieName(cookie.name) || !validCookieValue(cookie.value) {
				return EncodedInputs{}, fmt.Errorf("%w: cookie value is invalid", ErrUnsupportedWireValue)
			}
			parts = append(parts, cookie.name+"="+cookie.value)
		}
		encoded.Header.Set("Cookie", strings.Join(parts, "; "))
	}

	if resolved.Body.Presence() != contract.PresenceAbsent {
		if resolved.Variant == nil {
			return EncodedInputs{}, fmt.Errorf("%w: body has no selected media variant", ErrInvalidHTTPInput)
		}
		encoded.MediaType = resolved.Variant.MediaType
		if schemaContainsBinaryRef(resolved.Variant.Schema) && resolved.Variant.Schema.Type != contract.SchemaTypeBinaryRef {
			return EncodedInputs{}, fmt.Errorf("%w: nested binary request values are unsupported", ErrUnsupportedWireValue)
		}
		if resolved.Body.Kind() == contract.ValueKindBinaryRef {
			reference, _ := resolved.Body.ArtifactReference()
			encoded.BodyReference = &reference
		} else {
			if !isJSONWireMediaType(encoded.MediaType) {
				return EncodedInputs{}, fmt.Errorf("%w: typed request media is not JSON", ErrUnsupportedWireValue)
			}
			body, bodyErr := marshalProviderJSON(resolved.Body)
			if bodyErr != nil {
				return EncodedInputs{}, bodyErr
			}
			encoded.Body = body
		}
		if declared := encoded.Header.Values(contract.RequestMediaTypeWireName); len(declared) != 0 {
			if len(declared) != 1 || declared[0] != encoded.MediaType {
				return EncodedInputs{}, fmt.Errorf("%w: declared request media selector conflicts with encoded body", ErrInvalidHTTPInput)
			}
		}
		encoded.Header.Set(contract.RequestMediaTypeWireName, encoded.MediaType)
	}
	if httpFacts != nil {
		if err := applyConstantHeaders(&encoded, operation, *httpFacts); err != nil {
			return EncodedInputs{}, err
		}
	}
	encodedBytes, err := encodedInputSize(encoded)
	if err != nil {
		return EncodedInputs{}, err
	}
	encoded.EncodedBytes = encodedBytes
	return encoded, nil
}

func validateAuthorizationChannel(encoded *EncodedInputs, semantics contract.OperationAuthorization) error {
	if encoded == nil || encoded.Header == nil {
		return fmt.Errorf("%w: encoded request headers are absent", ErrInvalidHTTPInput)
	}
	switch semantics.Placement {
	case contract.FieldLocationHeader:
		if unsafeAuthorizationHeader(semantics.WireName) {
			return ErrCredentialRequired
		}
		if _, present := encoded.Header[http.CanonicalHeaderKey(semantics.WireName)]; present {
			return fmt.Errorf("%w: authorization channel collides with encoded input", ErrInvalidHTTPInput)
		}
	case contract.FieldLocationQuery:
		query, err := url.ParseQuery(encoded.RawQuery)
		if err != nil {
			return fmt.Errorf("%w: authorization query cannot be isolated", ErrInvalidHTTPInput)
		}
		if _, present := query[semantics.WireName]; present {
			return fmt.Errorf("%w: authorization channel collides with encoded input", ErrInvalidHTTPInput)
		}
	case contract.FieldLocationCookie:
		if !validCookieName(semantics.WireName) ||
			encodedCookieHasName(encoded.Header.Get("Cookie"), semantics.WireName) {
			return fmt.Errorf("%w: authorization cookie channel is invalid or collides with encoded input", ErrInvalidHTTPInput)
		}
	default:
		return ErrCredentialRequired
	}
	return nil
}

func applyAuthorization(encoded *EncodedInputs, semantics contract.OperationAuthorization, material credentials.CredentialMaterial) error {
	if material == nil {
		return ErrCredentialRequired
	}
	if err := validateAuthorizationChannel(encoded, semantics); err != nil {
		return err
	}
	return material.Use(func(secret []byte) error {
		if len(secret) == 0 || !utf8.Valid(secret) {
			return ErrCredentialRequired
		}
		value := semantics.Prefix + string(secret)
		if !validHTTPHeaderValue(value) {
			return ErrCredentialRequired
		}
		switch semantics.Placement {
		case contract.FieldLocationHeader:
			encoded.Header.Set(semantics.WireName, value)
		case contract.FieldLocationQuery:
			addition := escapeQueryValue(semantics.WireName, false) + "=" + escapeQueryValue(value, false)
			if encoded.RawQuery == "" {
				encoded.RawQuery = addition
			} else {
				encoded.RawQuery += "&" + addition
			}
		case contract.FieldLocationCookie:
			if !validCookieValue(value) {
				return ErrCredentialRequired
			}
			if prior := encoded.Header.Get("Cookie"); prior != "" {
				encoded.Header.Set("Cookie", prior+"; "+semantics.WireName+"="+value)
			} else {
				encoded.Header.Set("Cookie", semantics.WireName+"="+value)
			}
		}
		return nil
	})
}

func encodedCookieHasName(header, name string) bool {
	for segment := range strings.SplitSeq(header, ";") {
		cookieName, _, present := strings.Cut(strings.TrimSpace(segment), "=")
		if present && cookieName == name {
			return true
		}
	}
	return false
}

func unsafeAuthorizationHeader(name string) bool {
	if forbiddenRequestHeader(name) {
		return true
	}
	switch http.CanonicalHeaderKey(name) {
	case "Accept-Encoding", "Content-Type", "Cookie", "Expect",
		"Idempotency-Key", "Range", "Set-Cookie", "User-Agent":
		return true
	default:
		return false
	}
}

func applyConstantHeaders(encoded *EncodedInputs, operation contract.Operation, facts contract.OperationHTTP) error {
	if encoded == nil {
		return fmt.Errorf("%w: encoded request is absent", ErrInvalidHTTPInput)
	}
	for _, header := range facts.Headers {
		if !validHTTPToken(header.Name) || !validHTTPHeaderValue(header.Value) ||
			unsafeConstantHeader(header.Name) {
			return fmt.Errorf("%w: source constant header is unsafe", ErrInvalidHTTPInput)
		}
		if operationDeclaresHeaderBinding(operation, header.Name) {
			return fmt.Errorf("%w: source constant header collides with an input binding", ErrInvalidHTTPInput)
		}
		if facts.Authorization != nil && facts.Authorization.Placement == contract.FieldLocationHeader &&
			strings.EqualFold(facts.Authorization.WireName, header.Name) {
			return fmt.Errorf("%w: source constant header collides with authorization", ErrInvalidHTTPInput)
		}
		canonical := http.CanonicalHeaderKey(header.Name)
		if _, present := encoded.Header[canonical]; present {
			return fmt.Errorf("%w: source constant header collides with the encoded request", ErrInvalidHTTPInput)
		}
		encoded.Header[canonical] = []string{header.Value}
	}
	return nil
}

// clientIdentityWireName is the request field the shared runtime reserves for
// its own bounded product identity. It is a client identity and never a
// provider-specific behavior: an operation that declares this field keeps
// ownership of it, and the runtime writes nothing over it.
const clientIdentityWireName = "User-Agent"

// maximumClientIdentityBytes bounds the identity one process may send, so a
// build-reported version can never carry an unbounded value onto the wire.
const maximumClientIdentityBytes = 256

// ProductClientIdentityPrefix names the product every PM process uses when it
// identifies itself to a provider destination.
const ProductClientIdentityPrefix = "pm-cli/"

// ClientIdentity is the bounded product identity one process sends as the
// default provider user agent. Its zero value is invalid: a process that sends
// provider requests must be able to identify itself, and the runtime refuses a
// request it cannot identify instead of sending an empty value.
type ClientIdentity string

// ProductClientIdentity composes the product identity for one build-reported
// version string, for example "pm-cli/development". The composed value is
// validated by the runtime, so a build that reports an unusable version fails
// closed rather than sending a malformed identity.
func ProductClientIdentity(version string) ClientIdentity {
	return ClientIdentity(ProductClientIdentityPrefix + version)
}

// Validate accepts exactly one bounded printable-ASCII identity. An empty,
// padded, oversized or control-bearing value is refused: an unconfigured
// identity must fail closed, and a configured identity must reach the peer
// exactly as it was configured.
func (identity ClientIdentity) Validate() error {
	value := string(identity)
	if value == "" || len(value) > maximumClientIdentityBytes || strings.TrimSpace(value) != value {
		return ErrClientIdentityRequired
	}
	for index := range len(value) {
		if character := value[index]; character < 0x20 || character > 0x7e {
			return ErrClientIdentityRequired
		}
	}
	return nil
}

// resolveClientIdentity reports the default client identity this request must
// send and whether the runtime owns that field for the request. An operation
// that declares its own user-agent input owns the field: the runtime never
// writes, replaces or augments it, and needs no identity of its own. Every
// other operation requires a configured identity, so a process that cannot
// identify itself refuses the request before any credential resolution,
// request construction, admission or provider I/O.
func resolveClientIdentity(operation contract.Operation, configured ClientIdentity) (ClientIdentity, bool, error) {
	if declaresHeader(operation, clientIdentityWireName) {
		return "", false, nil
	}
	if err := configured.Validate(); err != nil {
		return "", false, err
	}
	return configured, true, nil
}

// applyClientIdentity writes the process client identity onto the encoded
// request. It never overwrites an already encoded header: a wire field the
// source declared is reported as a conflict instead of being replaced.
func applyClientIdentity(encoded *EncodedInputs, identity ClientIdentity) error {
	if encoded == nil {
		return fmt.Errorf("%w: encoded request is absent", ErrInvalidHTTPInput)
	}
	if err := identity.Validate(); err != nil {
		return err
	}
	canonical := http.CanonicalHeaderKey(clientIdentityWireName)
	if _, present := encoded.Header[canonical]; present {
		return fmt.Errorf("%w: the runtime client identity would replace an encoded provider field", ErrInvalidHTTPInput)
	}
	encoded.Header[canonical] = []string{string(identity)}
	return nil
}

func operationDeclaresHeaderBinding(operation contract.Operation, name string) bool {
	for _, binding := range operation.Inputs {
		if binding.Location == contract.FieldLocationHeader && strings.EqualFold(binding.WireName, name) {
			return true
		}
	}
	return false
}

func unsafeConstantHeader(name string) bool {
	if forbiddenRequestHeader(name) {
		return true
	}
	canonical := http.CanonicalHeaderKey(name)
	switch canonical {
	case "Accept-Encoding", "Authorization", "Content-Type", "Cookie", "Expect",
		"Idempotency-Key", "Range", "Set-Cookie", "User-Agent":
		return true
	}
	var compact strings.Builder
	for _, character := range strings.ToLower(name) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			compact.WriteRune(character)
		}
	}
	normalized := compact.String()
	return strings.Contains(normalized, "apikey") ||
		strings.Contains(normalized, "authorization") ||
		strings.Contains(normalized, "authentication") ||
		strings.Contains(normalized, "token") ||
		strings.Contains(normalized, "credential") ||
		strings.Contains(normalized, "password") ||
		strings.Contains(normalized, "privatekey") ||
		strings.Contains(normalized, "secret")
}

func findCommonBinding(operation contract.Operation, fieldID string) (contract.FieldBinding, bool) {
	for _, binding := range operation.Inputs {
		if binding.FieldID == fieldID {
			return binding, true
		}
	}
	return contract.FieldBinding{}, false
}

type wirePair struct {
	name          string
	value         string
	allowReserved bool
}

func encodePathBinding(binding contract.FieldBinding, value contract.PresentValue) (string, error) {
	parts, object, err := wireParts(value)
	if err != nil {
		return "", err
	}
	escaped := escapePathPrimitive
	switch binding.Style {
	case "simple":
		if object {
			return joinObject(parts, binding.Explode, "=", ",", escaped), nil
		}
		return joinValues(parts, ",", escaped), nil
	case "label":
		if object {
			separator := ","
			if binding.Explode {
				separator = "."
			}
			return "." + joinObject(parts, binding.Explode, "=", separator, escaped), nil
		}
		separator := ","
		if binding.Explode {
			separator = "."
		}
		return "." + joinValues(parts, separator, escaped), nil
	case "matrix":
		if object {
			if binding.Explode {
				return ";" + joinObject(parts, true, "=", ";", escaped), nil
			}
			return ";" + url.PathEscape(binding.WireName) + "=" + joinObject(parts, false, "=", ",", escaped), nil
		}
		if binding.Explode {
			values := make([]string, 0, len(parts))
			for _, part := range parts {
				values = append(values, url.PathEscape(binding.WireName)+"="+escaped(part))
			}
			return ";" + strings.Join(values, ";"), nil
		}
		return ";" + url.PathEscape(binding.WireName) + "=" + joinValues(parts, ",", escaped), nil
	default:
		return "", fmt.Errorf("%w: unsupported path style", ErrUnsupportedWireValue)
	}
}

func encodeParameter(binding contract.FieldBinding, value contract.PresentValue) ([]wirePair, error) {
	parts, object, err := wireParts(value)
	if err != nil {
		return nil, err
	}
	name := binding.WireName
	switch binding.Location {
	case contract.FieldLocationQuery:
		switch binding.Style {
		case "deepObject":
			if !object || !binding.Explode {
				return nil, fmt.Errorf("%w: deepObject requires an exploded object", ErrUnsupportedWireValue)
			}
			pairs := make([]wirePair, 0, len(parts)/2)
			for index := 0; index < len(parts); index += 2 {
				pairs = append(pairs, wirePair{name: name + "[" + parts[index] + "]", value: parts[index+1]})
			}
			return pairs, nil
		case "form":
			return formPairs(name, parts, object, binding.Explode), nil
		case "spaceDelimited", "pipeDelimited":
			if _, array := value.Elements(); !array || binding.Explode {
				return nil, fmt.Errorf("%w: delimited query style requires a non-exploded array", ErrUnsupportedWireValue)
			}
			separator := " "
			if binding.Style == "pipeDelimited" {
				separator = "|"
			}
			return []wirePair{{name: name, value: strings.Join(parts, separator)}}, nil
		}
	case contract.FieldLocationHeader:
		if binding.Style == "simple" {
			if object {
				return []wirePair{{name: name, value: joinObject(parts, binding.Explode, "=", ",", identityString)}}, nil
			}
			return []wirePair{{name: name, value: strings.Join(parts, ",")}}, nil
		}
	case contract.FieldLocationCookie:
		if binding.Style == "form" {
			return formPairs(name, parts, object, binding.Explode), nil
		}
	}
	return nil, fmt.Errorf("%w: unsupported parameter style", ErrUnsupportedWireValue)
}

func formPairs(name string, parts []string, object, explode bool) []wirePair {
	if object {
		if explode {
			pairs := make([]wirePair, 0, len(parts)/2)
			for index := 0; index < len(parts); index += 2 {
				pairs = append(pairs, wirePair{name: parts[index], value: parts[index+1]})
			}
			return pairs
		}
		return []wirePair{{name: name, value: strings.Join(parts, ",")}}
	}
	if explode && len(parts) > 1 {
		pairs := make([]wirePair, 0, len(parts))
		for _, part := range parts {
			pairs = append(pairs, wirePair{name: name, value: part})
		}
		return pairs
	}
	return []wirePair{{name: name, value: strings.Join(parts, ",")}}
}

func wireParts(value contract.PresentValue) ([]string, bool, error) {
	if value.Presence() == contract.PresenceNull {
		return nil, false, fmt.Errorf("%w: null parameter serialization is not source-declared", ErrUnsupportedWireValue)
	}
	if value.Presence() != contract.PresenceValue {
		return nil, false, fmt.Errorf("%w: absent wire value", ErrUnsupportedWireValue)
	}
	if _, member, ok := value.UnionMember(); ok {
		return wireParts(member)
	}
	if elements, ok := value.Elements(); ok {
		parts := make([]string, 0, len(elements))
		for _, element := range elements {
			text, err := primitiveWireText(element)
			if err != nil {
				return nil, false, err
			}
			parts = append(parts, text)
		}
		return parts, false, nil
	}
	if fields, ok := value.Fields(); ok {
		names := make([]string, 0, len(fields))
		for name := range fields {
			names = append(names, name)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names)*2)
		for _, name := range names {
			text, err := primitiveWireText(fields[name])
			if err != nil {
				return nil, true, err
			}
			parts = append(parts, name, text)
		}
		return parts, true, nil
	}
	text, err := primitiveWireText(value)
	if err != nil {
		return nil, false, err
	}
	return []string{text}, false, nil
}

func primitiveWireText(value contract.PresentValue) (string, error) {
	if value.Presence() == contract.PresenceNull {
		return "", fmt.Errorf("%w: null parameter serialization is not source-declared", ErrUnsupportedWireValue)
	}
	if text, ok := value.TextValue(); ok {
		return text, nil
	}
	if text, ok := value.TemporalText(); ok {
		return text, nil
	}
	if boolean, ok := value.Boolean(); ok {
		return strconv.FormatBool(boolean), nil
	}
	if number, ok := value.ExactNumber(); ok {
		return number.Text, nil
	}
	if _, member, ok := value.UnionMember(); ok {
		return primitiveWireText(member)
	}
	return "", fmt.Errorf("%w: non-primitive parameter value", ErrUnsupportedWireValue)
}

func marshalProviderJSON(value contract.PresentValue) ([]byte, error) {
	var buffer bytes.Buffer
	if err := writeProviderJSON(&buffer, value); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func writeProviderJSON(buffer *bytes.Buffer, value contract.PresentValue) error {
	switch value.Presence() {
	case contract.PresenceNull:
		buffer.WriteString("null")
		return nil
	case contract.PresenceAbsent:
		return fmt.Errorf("%w: absent value has no JSON representation", ErrUnsupportedWireValue)
	}
	if text, ok := value.TextValue(); ok {
		encoded, _ := json.Marshal(text)
		buffer.Write(encoded)
		return nil
	}
	if text, ok := value.TemporalText(); ok {
		encoded, _ := json.Marshal(text)
		buffer.Write(encoded)
		return nil
	}
	if boolean, ok := value.Boolean(); ok {
		buffer.WriteString(strconv.FormatBool(boolean))
		return nil
	}
	if number, ok := value.ExactNumber(); ok {
		buffer.WriteString(number.Text)
		return nil
	}
	if elements, ok := value.Elements(); ok {
		buffer.WriteByte('[')
		for index, element := range elements {
			if index != 0 {
				buffer.WriteByte(',')
			}
			if err := writeProviderJSON(buffer, element); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
		return nil
	}
	if fields, ok := value.Fields(); ok {
		names := make([]string, 0, len(fields))
		for name, field := range fields {
			if field.Presence() != contract.PresenceAbsent {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		buffer.WriteByte('{')
		for index, name := range names {
			if index != 0 {
				buffer.WriteByte(',')
			}
			encodedName, _ := json.Marshal(name)
			buffer.Write(encodedName)
			buffer.WriteByte(':')
			if err := writeProviderJSON(buffer, fields[name]); err != nil {
				return err
			}
		}
		buffer.WriteByte('}')
		return nil
	}
	if _, member, ok := value.UnionMember(); ok {
		return writeProviderJSON(buffer, member)
	}
	return fmt.Errorf("%w: binary references require the bounded object reader", ErrUnsupportedWireValue)
}

func escapePathPrimitive(value string) string {
	escaped := url.PathEscape(value)
	switch escaped {
	case ".":
		return "%2E"
	case "..":
		return "%2E%2E"
	default:
		return escaped
	}
}

func joinValues(values []string, separator string, escape func(string) string) string {
	escaped := make([]string, len(values))
	for index, value := range values {
		escaped[index] = escape(value)
	}
	return strings.Join(escaped, separator)
}

func joinObject(parts []string, explode bool, equals, separator string, escape func(string) string) string {
	encoded := make([]string, 0, len(parts))
	for index := 0; index < len(parts); index += 2 {
		if explode {
			encoded = append(encoded, escape(parts[index])+equals+escape(parts[index+1]))
		} else {
			encoded = append(encoded, escape(parts[index]), escape(parts[index+1]))
		}
	}
	return strings.Join(encoded, separator)
}

func encodeQueryPairs(pairs []wirePair) string {
	parts := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		parts = append(parts, escapeQueryValue(pair.name, false)+"="+escapeQueryValue(pair.value, pair.allowReserved))
	}
	return strings.Join(parts, "&")
}

func escapeQueryValue(value string, allowReserved bool) string {
	escaped := strings.ReplaceAll(url.QueryEscape(value), "+", "%20")
	if !allowReserved {
		return escaped
	}
	for encoded, literal := range map[string]string{
		"%2F": "/", "%3F": "?", "%3A": ":", "%40": "@", "%21": "!",
		"%24": "$", "%26": "&", "%27": "'", "%28": "(", "%29": ")",
		"%2A": "*", "%2B": "+", "%2C": ",", "%3B": ";", "%3D": "=",
		"%5B": "[", "%5D": "]",
	} {
		escaped = strings.ReplaceAll(escaped, encoded, literal)
	}
	return escaped
}

func encodedInputSize(encoded EncodedInputs) (uint64, error) {
	total := uint64(0)
	for _, size := range []int{len(encoded.EscapedPath), len(encoded.Body)} {
		var err error
		total, err = addUint64(total, uint64(size))
		if err != nil {
			return 0, err
		}
	}
	if encoded.RawQuery != "" {
		var err error
		total, err = addUint64(total, uint64(1+len(encoded.RawQuery)))
		if err != nil {
			return 0, err
		}
	}
	for name, values := range encoded.Header {
		for _, value := range values {
			lineBytes, err := addUint64(uint64(len(name)), 2)
			if err == nil {
				lineBytes, err = addUint64(lineBytes, uint64(len(value)))
			}
			if err == nil {
				lineBytes, err = addUint64(lineBytes, 2)
			}
			if err != nil {
				return 0, err
			}
			total, err = addUint64(total, lineBytes)
			if err != nil {
				return 0, err
			}
		}
	}
	return addUint64(total, 2)
}

func isJSONWireMediaType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	mediaType = strings.ToLower(mediaType)
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

func forbiddenRequestHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Content-Length", "Host", "Proxy-Authorization", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	default:
		return false
	}
}

func validCookieName(value string) bool {
	return validHTTPToken(value)
}

func validCookieValue(value string) bool {
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character != 0x21 &&
			(character < 0x23 || character > 0x2b) &&
			(character < 0x2d || character > 0x3a) &&
			(character < 0x3c || character > 0x5b) &&
			(character < 0x5d || character > 0x7e) {
			return false
		}
	}
	return true
}

func validHTTPToken(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character <= 0x20 || character >= 0x7f ||
			strings.ContainsRune("()<>@,;:\\\"/[]?={}", rune(character)) {
			return false
		}
	}
	return true
}

func validHTTPHeaderValue(value string) bool {
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character == '\t' {
			continue
		}
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func identityString(value string) string { return value }
