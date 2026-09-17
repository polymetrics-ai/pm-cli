package compile

import (
	"fmt"
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
func operationAuthentication(discovered discoveredOperation, source *ResolvedSource) (bool, []SourceFact, error) {
	requirements := discovered.document.Security
	pointer := "/security"
	if discovered.operation.Security != nil {
		requirements = *discovered.operation.Security
		pointer = discovered.pointer + "/security"
	}
	if len(requirements) == 0 {
		return false, []SourceFact{}, nil
	}
	for _, alternative := range requirements {
		if len(alternative) == 0 {
			return false, []SourceFact{}, nil
		}
		for schemeName := range alternative {
			if discovered.document.Components == nil {
				return false, nil, fmt.Errorf("security requirement %q has no captured scheme declaration", schemeName)
			}
			reference, ok := discovered.document.Components.SecuritySchemes[schemeName]
			if !ok || reference == nil || reference.Value == nil {
				return false, nil, fmt.Errorf("security requirement %q has no captured scheme declaration", schemeName)
			}
			scheme := reference.Value
			switch scheme.Type {
			case "apiKey":
				if scheme.Name == "" || !validSecurityLocation(scheme.In) {
					return false, nil, fmt.Errorf("API-key security requirement %q has an unsupported location", schemeName)
				}
			case "http":
				if !strings.EqualFold(scheme.Scheme, "bearer") && !strings.EqualFold(scheme.Scheme, "basic") {
					return false, nil, fmt.Errorf("HTTP security requirement %q uses unsupported scheme %q", schemeName, scheme.Scheme)
				}
			case "oauth2", "openIdConnect", "mutualTLS":
			default:
				return false, nil, fmt.Errorf("security requirement %q uses unsupported type %q", schemeName, scheme.Type)
			}
		}
	}
	facts := sourceFacts(source, []contract.SourceRef{{SourceID: discovered.sourceID, Pointer: "#" + pointer}})
	return true, facts, nil
}

func classifyParameterSensitivity(parameter *openapi3.Parameter, schema *openapi3.Schema, policy sensitivityPolicy) SensitivityClass {
	if parameter == nil {
		return SensitivityUnknown
	}
	if schema != nil {
		if strings.EqualFold(schema.Format, "password") {
			return SensitivitySecret
		}
	}
	if _, secret := policy.secretParameters[parameter.In+"\x00"+strings.ToLower(parameter.Name)]; secret {
		return SensitivitySecret
	}
	if credentialLikeName(parameter.Name) {
		return SensitivitySecret
	}
	if classification, explicit := explicitSensitivity(parameter.Extensions); explicit {
		return classification
	}
	if schema != nil {
		if classification, explicit := explicitSensitivity(schema.Extensions); explicit {
			return classification
		}
	}
	return SensitivityPublic
}

func classifyPropertySensitivity(name string, schema *openapi3.Schema) SensitivityClass {
	if schema == nil {
		return SensitivityUnknown
	}
	if strings.EqualFold(schema.Format, "password") || credentialLikeName(name) {
		return SensitivitySecret
	}
	if classification, explicit := explicitSensitivity(schema.Extensions); explicit {
		return classification
	}
	return SensitivityPublic
}

func schemaSensitivity(schema *openapi3.Schema, seen map[*openapi3.Schema]struct{}) SensitivityClass {
	if schema == nil {
		return SensitivityUnknown
	}
	if _, recursive := seen[schema]; recursive {
		return SensitivityUnknown
	}
	seen[schema] = struct{}{}
	defer delete(seen, schema)
	if strings.EqualFold(schema.Format, "password") {
		return SensitivitySecret
	}
	result := SensitivityPublic
	if classification, explicit := explicitSensitivity(schema.Extensions); explicit {
		result = classification
	}
	for name, reference := range schema.Properties {
		if reference == nil || reference.Value == nil {
			return SensitivityUnknown
		}
		classification := classifyPropertySensitivity(name, reference.Value)
		if classification == SensitivitySecret {
			return SensitivitySecret
		}
		if classification == SensitivityUnknown {
			result = SensitivityUnknown
		}
		child := schemaSensitivity(reference.Value, seen)
		if child == SensitivitySecret {
			return SensitivitySecret
		}
		if child == SensitivityUnknown {
			result = SensitivityUnknown
		}
	}
	if schema.Items != nil {
		if schema.Items.Value == nil {
			return SensitivityUnknown
		}
		child := schemaSensitivity(schema.Items.Value, seen)
		if child == SensitivitySecret {
			return SensitivitySecret
		}
		if child == SensitivityUnknown {
			result = SensitivityUnknown
		}
	}
	for _, collection := range []openapi3.SchemaRefs{schema.OneOf, schema.AnyOf, schema.AllOf} {
		for _, reference := range collection {
			if reference == nil || reference.Value == nil {
				return SensitivityUnknown
			}
			child := schemaSensitivity(reference.Value, seen)
			if child == SensitivitySecret {
				return SensitivitySecret
			}
			if child == SensitivityUnknown {
				result = SensitivityUnknown
			}
		}
	}
	return result
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
