package compile

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type sensitivityPolicy struct {
	secretParameters map[string]struct{}
	unknownSecurity  bool
}

func buildSensitivityPolicy(document *openapi3.T) sensitivityPolicy {
	policy := sensitivityPolicy{secretParameters: make(map[string]struct{})}
	if document == nil || document.Components == nil {
		return policy
	}
	for _, reference := range document.Components.SecuritySchemes {
		if reference == nil || reference.Value == nil {
			policy.unknownSecurity = true
			continue
		}
		scheme := reference.Value
		switch scheme.Type {
		case "apiKey":
			if scheme.Name == "" || !validSecurityLocation(scheme.In) {
				policy.unknownSecurity = true
				continue
			}
			policy.secretParameters[scheme.In+"\x00"+strings.ToLower(scheme.Name)] = struct{}{}
		case "http":
			if strings.EqualFold(scheme.Scheme, "bearer") || strings.EqualFold(scheme.Scheme, "basic") {
				policy.secretParameters["header\x00authorization"] = struct{}{}
			} else {
				policy.unknownSecurity = true
			}
		case "oauth2", "openIdConnect", "mutualTLS":
			policy.secretParameters["header\x00authorization"] = struct{}{}
		default:
			policy.unknownSecurity = true
		}
	}
	return policy
}

type derivedOperationHTTP struct {
	Semantics       *HTTPSemantics
	Facts           []SourceFact
	Credential      *BindingFact
	FixedParameters map[string]struct{}
}

type httpDerivationError struct {
	code string
	err  error
}

func (failure *httpDerivationError) Error() string {
	return failure.err.Error()
}

func httpDerivationDiagnosticCode(err error) string {
	if failure, ok := err.(*httpDerivationError); ok {
		return failure.code
	}
	return "unsupported_http_authority"
}

func classifyHTTPDerivationFailure(code string, err error) error {
	return &httpDerivationError{code: code, err: err}
}

func deriveOperationHTTP(
	discovered discoveredOperation,
	source *ResolvedSource,
	parameters []parameterEntry,
	policy sensitivityPolicy,
) (derivedOperationHTTP, error) {
	var references []contract.SourceRef
	servers, serverPointer, err := effectiveOperationServers(discovered, source)
	if err != nil {
		if _, present := source.RawValue(discovered.sourceID, serverPointer); present {
			references = append(references, contract.SourceRef{SourceID: discovered.sourceID, Pointer: "#" + serverPointer})
		}
		return derivedHTTPFailure(source, references, classifyHTTPDerivationFailure("unsupported_http_authority", err))
	}
	origins := make([]string, 0, len(servers))
	for index, server := range servers {
		pointer := serverPointer + "/" + strconv.Itoa(index)
		references = append(references, contract.SourceRef{SourceID: discovered.sourceID, Pointer: "#" + pointer})
		if server == nil || server.URL == "" {
			return derivedHTTPFailure(source, references, classifyHTTPDerivationFailure("unsupported_http_authority", fmt.Errorf("HTTP server %d has no captured URL", index)))
		}
		if len(server.Variables) != 0 || strings.ContainsAny(server.URL, "{}") {
			return derivedHTTPFailure(source, references, classifyHTTPDerivationFailure("unsupported_http_authority", fmt.Errorf("HTTP server %d is templated and cannot be admitted without an exact source value", index)))
		}
		origins = append(origins, server.URL)
	}

	authorization, credential, securityReferences, err := deriveOperationAuthorization(discovered, source, origins)
	references = append(references, securityReferences...)
	if err != nil {
		return derivedHTTPFailure(source, references, classifyHTTPDerivationFailure("unsupported_authentication", err))
	}
	headers, fixedParameters, headerReferences, err := deriveFixedHTTPHeaders(source, discovered.sourceID, parameters, policy)
	references = append(references, headerReferences...)
	if err != nil {
		return derivedHTTPFailure(source, references, classifyHTTPDerivationFailure("unsupported_fixed_headers", err))
	}
	if authorization != nil {
		for _, parameter := range parameters {
			if parameter.parameter == nil {
				continue
			}
			if parameterFieldID(parameter.parameter) == authorization.FieldID ||
				(contract.FieldLocation(parameter.parameter.In) == authorization.Placement &&
					strings.EqualFold(parameter.parameter.Name, authorization.WireName)) {
				references = append(references, contract.SourceRef{
					SourceID: discovered.sourceID,
					Pointer:  "#" + parameter.pointer,
				})
				return derivedHTTPFailure(source, references, classifyHTTPDerivationFailure("unsupported_authentication", fmt.Errorf("authorization declaration collides with an ordinary operation parameter")))
			}
		}
	}

	references = sortedUniqueSourceRefs(references)
	facts := sourceFacts(source, references)
	semantics := &HTTPSemantics{
		Origins:       origins,
		Authorization: authorization,
		Headers:       headers,
		Witness: Witness{
			ProofID:    "openapi-source-http",
			SourceRefs: slices.Clone(references),
		},
	}
	var inputs []contract.FieldBinding
	if credential != nil {
		credentialFacts := sourceFacts(source, securityReferences)
		credential.SourceFacts = credentialFacts
		credential.NormalizedSHA256 = hashJSON(*credential.Normalized)
		inputs = append(inputs, *credential.Normalized)
	}
	if err := semanticsToContractHTTP(*semantics).Validate(contract.Operation{
		Transport: contract.Transport{Kind: contract.TransportKindHTTP},
		Inputs:    inputs,
	}); err != nil {
		return derivedHTTPFailure(source, references, classifyHTTPDerivationFailure("unsupported_http_authority", err))
	}
	return derivedOperationHTTP{
		Semantics:       semantics,
		Facts:           facts,
		Credential:      credential,
		FixedParameters: fixedParameters,
	}, nil
}

func derivedHTTPFailure(
	source *ResolvedSource,
	references []contract.SourceRef,
	err error,
) (derivedOperationHTTP, error) {
	references = sortedUniqueSourceRefs(references)
	return derivedOperationHTTP{Facts: sourceFacts(source, references)}, err
}

func effectiveOperationServers(
	discovered discoveredOperation,
	source *ResolvedSource,
) (openapi3.Servers, string, error) {
	operationPointer := discovered.pointer + "/servers"
	if discovered.operation.Servers != nil {
		if len(*discovered.operation.Servers) == 0 {
			return nil, operationPointer, fmt.Errorf("operation-level HTTP servers are explicitly empty")
		}
		return *discovered.operation.Servers, operationPointer, nil
	}
	pathPointer := discovered.pointer[:strings.LastIndex(discovered.pointer, "/")] + "/servers"
	if _, present := source.RawValue(discovered.sourceID, pathPointer); present {
		if len(discovered.pathItem.Servers) == 0 {
			return nil, pathPointer, fmt.Errorf("path-level HTTP servers are explicitly empty")
		}
		return discovered.pathItem.Servers, pathPointer, nil
	}
	if _, present := source.RawValue(discovered.sourceID, "/servers"); present {
		if len(discovered.document.Servers) == 0 {
			return nil, "/servers", fmt.Errorf("root HTTP servers are explicitly empty")
		}
		return discovered.document.Servers, "/servers", nil
	}
	return nil, "/servers", fmt.Errorf("source has no absolute HTTP server authority; the implicit relative server is unsupported")
}

func deriveOperationAuthorization(
	discovered discoveredOperation,
	source *ResolvedSource,
	origins []string,
) (*contract.OperationAuthorization, *BindingFact, []contract.SourceRef, error) {
	requirements := discovered.document.Security
	securityPointer := "/security"
	_, rootSecurityPresent := source.RawValue(discovered.sourceID, securityPointer)
	if discovered.operation.Security != nil {
		requirements = *discovered.operation.Security
		securityPointer = discovered.pointer + "/security"
		rootSecurityPresent = true
	}
	var references []contract.SourceRef
	if rootSecurityPresent {
		references = append(references, contract.SourceRef{SourceID: discovered.sourceID, Pointer: "#" + securityPointer})
	}
	if len(requirements) == 0 {
		return nil, nil, references, nil
	}

	optional := false
	selectedScheme := ""
	for index, alternative := range requirements {
		references = append(references, contract.SourceRef{
			SourceID: discovered.sourceID,
			Pointer:  "#" + securityPointer + "/" + strconv.Itoa(index),
		})
		if len(alternative) == 0 {
			optional = true
			continue
		}
		if len(alternative) != 1 {
			return nil, nil, references, fmt.Errorf("compound security requirement at alternative %d is unsupported", index)
		}
		names := make([]string, 0, len(alternative))
		for name := range alternative {
			names = append(names, name)
		}
		slices.Sort(names)
		name := names[0]
		if len(alternative[name]) != 0 {
			return nil, nil, references, fmt.Errorf("security requirement %q carries scopes that cannot be represented exactly", name)
		}
		if selectedScheme != "" && selectedScheme != name {
			return nil, nil, references, fmt.Errorf("alternative security schemes are ambiguous")
		}
		selectedScheme = name
	}
	if selectedScheme == "" {
		return nil, nil, references, nil
	}
	if discovered.document.Components == nil {
		return nil, nil, references, fmt.Errorf("security requirement %q has no captured scheme declaration", selectedScheme)
	}
	reference, ok := discovered.document.Components.SecuritySchemes[selectedScheme]
	if !ok || reference == nil || reference.Value == nil {
		return nil, nil, references, fmt.Errorf("security requirement %q has no captured scheme declaration", selectedScheme)
	}
	schemePointer := "/components/securitySchemes/" + escapePointerToken(selectedScheme)
	references = append(references, contract.SourceRef{SourceID: discovered.sourceID, Pointer: "#" + schemePointer})
	if identity, ok := source.ReferenceAt(discovered.sourceID, schemePointer+"/$ref"); ok {
		references = append(references, contract.SourceRef{
			SourceID: identity.TargetSourceID,
			Pointer:  "#" + identity.TargetPointer,
		})
	}

	audience, err := commonHTTPAudience(origins)
	if err != nil {
		return nil, nil, references, err
	}
	fieldID := "auth." + stableFieldSegment(selectedScheme)
	if extension, present := reference.Value.Extensions["x-pm-field-id"]; present {
		value, ok := extension.(string)
		if !ok || !validIdentity(value) {
			return nil, nil, references, fmt.Errorf("security requirement %q has an invalid x-pm-field-id", selectedScheme)
		}
		fieldID = value
	}
	if !validIdentity(fieldID) {
		return nil, nil, references, fmt.Errorf("security requirement %q cannot produce a stable credential field identity", selectedScheme)
	}

	authorization := &contract.OperationAuthorization{
		Required: !optional,
		Audience: audience,
		FieldID:  fieldID,
	}
	scheme := reference.Value
	switch scheme.Type {
	case "apiKey":
		if scheme.Name == "" || !validSecurityLocation(scheme.In) {
			return nil, nil, references, fmt.Errorf("API-key security requirement %q has an unsupported location", selectedScheme)
		}
		authorization.Placement = contract.FieldLocation(scheme.In)
		authorization.WireName = scheme.Name
	case "http":
		authorization.Placement = contract.FieldLocationHeader
		authorization.WireName = "Authorization"
		switch {
		case strings.EqualFold(scheme.Scheme, "bearer"):
			authorization.Prefix = "Bearer "
		case strings.EqualFold(scheme.Scheme, "basic"):
			authorization.Prefix = "Basic "
		default:
			return nil, nil, references, fmt.Errorf("HTTP security requirement %q uses unsupported scheme %q", selectedScheme, scheme.Scheme)
		}
	case "oauth2", "openIdConnect":
		authorization.Placement = contract.FieldLocationHeader
		authorization.WireName = "Authorization"
		authorization.Prefix = "Bearer "
	default:
		return nil, nil, references, fmt.Errorf("security requirement %q uses unsupported type %q", selectedScheme, scheme.Type)
	}
	if authorization.Placement == contract.FieldLocationHeader &&
		unsafeSourceAuthorizationHeader(authorization.WireName) {
		return nil, nil, references, fmt.Errorf("authorization header %q is transport-controlled or unsupported", authorization.WireName)
	}
	binding := authorizationBinding(*authorization)
	return authorization, &binding, references, nil
}

func authorizationBinding(authorization contract.OperationAuthorization) BindingFact {
	style := "simple"
	explode := false
	if authorization.Placement == contract.FieldLocationQuery || authorization.Placement == contract.FieldLocationCookie {
		style = "form"
		explode = true
	}
	binding := contract.FieldBinding{
		FieldID:       authorization.FieldID,
		WireName:      authorization.WireName,
		Location:      authorization.Placement,
		JSONPointer:   "",
		Required:      authorization.Required,
		Nullable:      false,
		Style:         style,
		Explode:       explode,
		AllowReserved: false,
		Sensitivity:   contract.SensitivitySecret,
		Schema:        contract.Schema{Type: contract.SchemaTypeString},
	}
	return BindingFact{
		FieldID:          binding.FieldID,
		RawRequired:      binding.Required,
		RawNullable:      false,
		RawStyle:         binding.Style,
		RawExplode:       binding.Explode,
		RawAllowReserved: false,
		Sensitivity:      SensitivitySecret,
		Normalized:       &binding,
	}
}

func commonHTTPAudience(origins []string) (string, error) {
	audience := ""
	for _, origin := range origins {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return "", fmt.Errorf("HTTP server origin is malformed")
		}
		candidate := strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host)
		if audience != "" && audience != candidate {
			return "", fmt.Errorf("authorization cannot be admitted across multiple HTTP audiences")
		}
		audience = candidate
	}
	if audience == "" {
		return "", fmt.Errorf("authorization has no admitted HTTP audience")
	}
	return audience, nil
}

func deriveFixedHTTPHeaders(
	source *ResolvedSource,
	sourceID string,
	parameters []parameterEntry,
	policy sensitivityPolicy,
) ([]contract.OperationHTTPHeader, map[string]struct{}, []contract.SourceRef, error) {
	var headers []contract.OperationHTTPHeader
	fixedParameters := make(map[string]struct{})
	var references []contract.SourceRef
	seen := make(map[string]struct{})
	for _, entry := range parameters {
		parameter := entry.parameter
		if parameter == nil || parameter.In != string(contract.FieldLocationHeader) || !parameter.Required ||
			parameter.Content != nil || parameter.Schema == nil || parameter.Schema.Value == nil {
			continue
		}
		value, fixed, err := fixedStringSchemaValue(parameter.Schema.Value)
		if err != nil {
			references = append(references, contract.SourceRef{SourceID: sourceID, Pointer: "#" + entry.pointer})
			return nil, nil, references, fmt.Errorf("fixed header %q is not a scalar string: %w", parameter.Name, err)
		}
		if !fixed {
			continue
		}
		reference := contract.SourceRef{SourceID: sourceID, Pointer: "#" + entry.pointer}
		references = append(references, reference)
		if identity, ok := source.ReferenceAt(sourceID, entry.pointer+"/$ref"); ok {
			references = append(references, contract.SourceRef{
				SourceID: identity.TargetSourceID,
				Pointer:  "#" + identity.TargetPointer,
			})
		}
		if classifyParameterSensitivity(parameter, parameter.Schema.Value, policy) != SensitivityPublic {
			return nil, nil, references, fmt.Errorf("fixed header %q is a credential or has non-public sensitivity", parameter.Name)
		}
		if unsafeSourceFixedHeader(parameter.Name) {
			return nil, nil, references, fmt.Errorf("fixed header %q is transport-owned, replay-sensitive, or a credential channel", parameter.Name)
		}
		key := strings.ToLower(parameter.Name)
		if _, duplicate := seen[key]; duplicate {
			return nil, nil, references, fmt.Errorf("fixed header %q is duplicated", parameter.Name)
		}
		seen[key] = struct{}{}
		headers = append(headers, contract.OperationHTTPHeader{Name: parameter.Name, Value: value})
		fixedParameters[entry.pointer] = struct{}{}
	}
	slices.SortFunc(headers, func(left, right contract.OperationHTTPHeader) int {
		return strings.Compare(strings.ToLower(left.Name), strings.ToLower(right.Name))
	})
	return headers, fixedParameters, references, nil
}

func fixedStringSchemaValue(schema *openapi3.Schema) (string, bool, error) {
	if schema == nil {
		return "", false, nil
	}
	var value any
	switch {
	case schema.Const != nil:
		value = schema.Const
	case len(schema.Enum) == 1:
		value = schema.Enum[0]
	default:
		return "", false, nil
	}
	if !schema.Type.Is("string") || schema.Nullable {
		return "", true, fmt.Errorf("fixed value does not have one non-null string type")
	}
	text, ok := value.(string)
	if !ok {
		return "", true, fmt.Errorf("fixed value is not a string")
	}
	return text, true, nil
}

func unsafeSourceFixedHeader(name string) bool {
	compact := compactIdentifier(name)
	for _, fragment := range []string{
		"apikey", "authorization", "authentication", "token",
		"credential", "password", "privatekey", "secret",
	} {
		if strings.Contains(compact, fragment) {
			return true
		}
	}
	switch strings.ToLower(name) {
	case "authorization", "proxy-authorization", "cookie", "set-cookie",
		"host", "content-length", "content-type", "transfer-encoding",
		"connection", "proxy-connection", "upgrade", "trailer", "te",
		"user-agent", "accept-encoding", "expect", "range", "idempotency-key":
		return true
	default:
		return false
	}
}

func unsafeSourceAuthorizationHeader(name string) bool {
	switch strings.ToLower(name) {
	case "proxy-authorization", "cookie", "set-cookie",
		"host", "content-length", "content-type", "transfer-encoding",
		"connection", "proxy-connection", "upgrade", "trailer", "te",
		"user-agent", "accept-encoding", "expect", "range", "idempotency-key":
		return true
	default:
		return false
	}
}

func sortedUniqueSourceRefs(references []contract.SourceRef) []contract.SourceRef {
	slices.SortFunc(references, func(left, right contract.SourceRef) int {
		if compared := strings.Compare(left.SourceID, right.SourceID); compared != 0 {
			return compared
		}
		return strings.Compare(left.Pointer, right.Pointer)
	})
	return slices.CompactFunc(references, func(left, right contract.SourceRef) bool {
		return left.SourceID == right.SourceID && left.Pointer == right.Pointer
	})
}

func classifyParameterSensitivity(parameter *openapi3.Parameter, schema *openapi3.Schema, policy sensitivityPolicy) SensitivityClass {
	if parameter == nil {
		return SensitivityUnknown
	}
	if _, secret := policy.secretParameters[parameter.In+"\x00"+strings.ToLower(parameter.Name)]; secret {
		return SensitivitySecret
	}
	if credentialLikeName(parameter.Name) {
		return SensitivitySecret
	}
	classification := SensitivityPublic
	if explicit, present := explicitSensitivity(parameter.Extensions); present {
		classification = explicit
	}
	if schema == nil {
		return classification
	}
	return aggregateSensitivity(classification, schemaSensitivity(schema, make(map[*openapi3.Schema]struct{})))
}

func classifyPropertySensitivity(name string, schema *openapi3.Schema) SensitivityClass {
	if schema == nil {
		return SensitivityUnknown
	}
	classification := SensitivityPublic
	if credentialLikeName(name) {
		classification = SensitivitySecret
	}
	return aggregateSensitivity(classification, schemaSensitivity(schema, make(map[*openapi3.Schema]struct{})))
}

func schemaSensitivity(schema *openapi3.Schema, seen map[*openapi3.Schema]struct{}) SensitivityClass {
	if schema == nil {
		return SensitivityUnknown
	}
	if seen == nil {
		seen = make(map[*openapi3.Schema]struct{})
	}
	if _, recursive := seen[schema]; recursive {
		return SensitivityUnknown
	}
	seen[schema] = struct{}{}
	defer delete(seen, schema)

	result := SensitivityPublic
	if strings.EqualFold(schema.Format, "password") {
		result = SensitivitySecret
	}
	if classification, explicit := explicitSensitivity(schema.Extensions); explicit {
		result = aggregateSensitivity(result, classification)
	}
	for name, reference := range schema.Properties {
		if credentialLikeName(name) {
			return SensitivitySecret
		}
		if reference == nil || reference.Value == nil {
			result = aggregateSensitivity(result, SensitivityUnknown)
			continue
		}
		result = aggregateSensitivity(result, schemaSensitivity(reference.Value, seen))
		if result == SensitivitySecret {
			return result
		}
	}
	if schema.Items != nil {
		if schema.Items.Value == nil {
			result = aggregateSensitivity(result, SensitivityUnknown)
		} else {
			result = aggregateSensitivity(result, schemaSensitivity(schema.Items.Value, seen))
		}
		if result == SensitivitySecret {
			return result
		}
	}
	for _, collection := range []openapi3.SchemaRefs{schema.OneOf, schema.AnyOf, schema.AllOf} {
		for _, reference := range collection {
			if reference == nil || reference.Value == nil {
				result = aggregateSensitivity(result, SensitivityUnknown)
				continue
			}
			result = aggregateSensitivity(result, schemaSensitivity(reference.Value, seen))
			if result == SensitivitySecret {
				return result
			}
		}
	}
	return result
}

func aggregateSensitivity(left, right SensitivityClass) SensitivityClass {
	if left == SensitivitySecret || right == SensitivitySecret {
		return SensitivitySecret
	}
	if left == SensitivityUnknown || right == SensitivityUnknown {
		return SensitivityUnknown
	}
	if left == SensitivityPublic && right == SensitivityPublic {
		return SensitivityPublic
	}
	return SensitivityUnknown
}

func explicitSensitivity(extensions map[string]any) (SensitivityClass, bool) {
	if len(extensions) == 0 {
		return "", false
	}
	value, present := extensions["x-pm-sensitivity"]
	if !present {
		return "", false
	}
	text, ok := value.(string)
	if !ok {
		return SensitivityUnknown, true
	}
	switch SensitivityClass(strings.ToLower(text)) {
	case SensitivityPublic:
		return SensitivityPublic, true
	case SensitivitySecret:
		return SensitivitySecret, true
	default:
		return SensitivityUnknown, true
	}
}

func credentialLikeName(value string) bool {
	compact := compactIdentifier(value)
	switch compact {
	case "authorization", "password", "passwd", "secret", "token":
		return true
	}
	for _, suffix := range []string{"apikey", "accesstoken", "refreshtoken", "clientsecret", "privatekey", "password"} {
		if strings.HasSuffix(compact, suffix) {
			return true
		}
	}
	return false
}

func credentialOperationID(value string) bool {
	compact := compactIdentifier(value)
	for _, action := range []string{"create", "generate", "issue", "rotate"} {
		if !strings.HasPrefix(compact, action) {
			continue
		}
		remainder := strings.TrimPrefix(compact, action)
		switch remainder {
		case "token", "accesstoken", "refreshtoken", "apikey", "secret", "clientsecret", "credential", "credentials", "password":
			return true
		}
	}
	return false
}

func compactIdentifier(value string) string {
	var builder strings.Builder
	for _, character := range strings.ToLower(value) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			builder.WriteRune(character)
		}
	}
	return builder.String()
}

func validSecurityLocation(location string) bool {
	return location == "header" || location == "query" || location == "cookie"
}
