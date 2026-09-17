package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/oapi-codegen/oapi-codegen/v2/pkg/codegen"
	"go.yaml.in/yaml/v3"
)

const (
	apiGeneratorVersion = "v2.8.0"
	maxAPISourceBytes   = 8 << 20
)

var generatedAPIPaths = []string{
	"api/pm.openapi.json",
	"internal/api/client.gen.go",
	"internal/api/controls.gen.go",
	"internal/api/types.gen.go",
	"internal/httpapi/admin.gen.go",
	"internal/httpapi/server.gen.go",
}

// APIGenerationOptions identifies the accepted source files and the owned
// output tree. Source paths may be absolute for isolated fault fixtures.
type APIGenerationOptions struct {
	Root          string
	SpecPath      string
	AdminSpecPath string
	ControlsPath  string
	ConfigPath    string
	OutputRoot    string
	Check         bool
}

type apiCodegenConfiguration struct {
	codegen.Configuration `yaml:",inline"`
	OutputFile            string `yaml:"output"`
}

type apiControls struct {
	SchemaVersion         string             `json:"schema_version"`
	WireVersion           string             `json:"wire_version"`
	ProtocolMajor         int                `json:"protocol_major"`
	SupportedFeatures     []string           `json:"supported_features"`
	ProtocolMetadata      metadataControls   `json:"protocol_metadata"`
	ForbiddenPublicFields []string           `json:"forbidden_public_fields"`
	RunStates             []string           `json:"run_states"`
	TerminalRunStates     []string           `json:"terminal_run_states"`
	Problems              []problemControl   `json:"problems"`
	Operations            []operationControl `json:"operations"`
}

type metadataControls struct {
	Required []string `json:"required"`
	Optional []string `json:"optional"`
}

type problemControl struct {
	Status          int    `json:"status"`
	Code            string `json:"code"`
	SafeRetryAction string `json:"safe_retry_action"`
	Disposition     string `json:"disposition"`
}

type operationControl struct {
	OperationID            string   `json:"operation_id"`
	Method                 string   `json:"method"`
	Path                   string   `json:"path"`
	RequiredBodyFields     []string `json:"required_body_fields"`
	OptionalBodyFields     []string `json:"optional_body_fields"`
	RequiresIdempotencyKey bool     `json:"requires_idempotency_key"`
	RequiresIfMatch        bool     `json:"requires_if_match"`
}

// GenerateAPI validates the accepted authoring sources, generates every owned
// projection in memory, and then either atomically writes or exactly compares
// the complete output set.
func GenerateAPI(ctx context.Context, options APIGenerationOptions) error {
	resolved, err := resolveAPIGenerationOptions(options)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	specSource, err := readBoundedFile(resolved.SpecPath)
	if err != nil {
		return fmt.Errorf("read OpenAPI source: %w", err)
	}
	specNode, err := parseYAMLDocument(specSource)
	if err != nil {
		return fmt.Errorf("parse OpenAPI source: %w", err)
	}
	if err := validateAuthoringSpecNode(specNode); err != nil {
		return fmt.Errorf("unsupported ordinary public API source: %w", err)
	}

	document, err := loadOpenAPIDocument(ctx, specSource)
	if err != nil {
		return err
	}
	if err := validateAuthoringDocument(document); err != nil {
		return err
	}

	controls, err := loadAPIControls(resolved.ControlsPath)
	if err != nil {
		return err
	}
	if err := validateAPIControls(controls, document); err != nil {
		return err
	}
	configuration, err := loadAPICodegenConfiguration(resolved.ConfigPath)
	if err != nil {
		return err
	}
	if err := validateAPICodegenConfiguration(configuration); err != nil {
		return err
	}

	adminSource, err := readBoundedFile(resolved.AdminSpecPath)
	if err != nil {
		return fmt.Errorf("read private API source: %w", err)
	}
	adminNode, err := parseYAMLDocument(adminSource)
	if err != nil {
		return fmt.Errorf("parse private API source: %w", err)
	}
	admin, err := composeAdminAPI(specNode, adminNode)
	if err != nil {
		return fmt.Errorf("compose isolated admin API: %w", err)
	}

	outputs, err := generateAPIOutputs(ctx, document, controls, configuration, admin)
	if err != nil {
		return err
	}
	if options.Check {
		return compareGeneratedAPIOutputs(resolved.OutputRoot, outputs)
	}
	return writeGeneratedAPIOutputs(resolved.OutputRoot, outputs)
}

func resolveAPIGenerationOptions(options APIGenerationOptions) (APIGenerationOptions, error) {
	if strings.TrimSpace(options.Root) == "" {
		return APIGenerationOptions{}, errors.New("repository root is required")
	}
	root, err := filepath.Abs(options.Root)
	if err != nil {
		return APIGenerationOptions{}, fmt.Errorf("resolve repository root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return APIGenerationOptions{}, fmt.Errorf("stat repository root: %w", err)
	}
	if !info.IsDir() {
		return APIGenerationOptions{}, fmt.Errorf("repository root %q is not a directory", root)
	}
	resolve := func(value, fallback string) (string, error) {
		if value == "" {
			value = fallback
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(root, value)
		}
		return filepath.Abs(value)
	}
	options.Root = root
	if options.SpecPath, err = resolve(options.SpecPath, "api/pm.openapi.yaml"); err != nil {
		return APIGenerationOptions{}, fmt.Errorf("resolve OpenAPI source: %w", err)
	}
	if options.AdminSpecPath, err = resolve(options.AdminSpecPath, "api/admin.openapi.yaml"); err != nil {
		return APIGenerationOptions{}, fmt.Errorf("resolve private API source: %w", err)
	}
	if options.ControlsPath, err = resolve(options.ControlsPath, "api/controls.json"); err != nil {
		return APIGenerationOptions{}, fmt.Errorf("resolve controls source: %w", err)
	}
	if options.ConfigPath, err = resolve(options.ConfigPath, "api/oapi-codegen.yaml"); err != nil {
		return APIGenerationOptions{}, fmt.Errorf("resolve generator configuration: %w", err)
	}
	if options.OutputRoot, err = resolve(options.OutputRoot, "."); err != nil {
		return APIGenerationOptions{}, fmt.Errorf("resolve output root: %w", err)
	}
	return options, nil
}

func readBoundedFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxAPISourceBytes+1))
	if err != nil {
		return nil, err
	}
	if len(content) > maxAPISourceBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maxAPISourceBytes)
	}
	return content, nil
}

func parseYAMLDocument(content []byte) (*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 {
		return nil, errors.New("expected one YAML document")
	}
	if err := validateYAMLNode(document.Content[0], "$", map[*yaml.Node]bool{}); err != nil {
		return nil, err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple YAML documents are not allowed")
		}
		return nil, err
	}
	return document.Content[0], nil
}

func validateYAMLNode(node *yaml.Node, path string, visiting map[*yaml.Node]bool) error {
	if node == nil {
		return fmt.Errorf("%s: nil YAML node", path)
	}
	if visiting[node] {
		return fmt.Errorf("%s: YAML aliases or cycles are not allowed", path)
	}
	visiting[node] = true
	defer delete(visiting, node)
	if node.Kind == yaml.AliasNode || node.Anchor != "" {
		return fmt.Errorf("%s: YAML anchors and aliases are not allowed", path)
	}
	switch node.Kind {
	case yaml.MappingNode:
		seen := make(map[string]struct{}, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return fmt.Errorf("%s: mapping keys must be strings", path)
			}
			if _, exists := seen[key.Value]; exists {
				return fmt.Errorf("%s: duplicate key %q", path, key.Value)
			}
			seen[key.Value] = struct{}{}
			if err := validateYAMLNode(node.Content[index+1], path+"."+key.Value, visiting); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for index, child := range node.Content {
			if err := validateYAMLNode(child, fmt.Sprintf("%s[%d]", path, index), visiting); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		allowed := map[string]bool{
			"!!str": true, "!!int": true, "!!bool": true, "!!null": true, "!!float": true,
		}
		if !allowed[node.Tag] {
			return fmt.Errorf("%s: unsupported YAML tag %q", path, node.Tag)
		}
	default:
		return fmt.Errorf("%s: unsupported YAML node kind %d", path, node.Kind)
	}
	return nil
}

func validateAuthoringSpecNode(node *yaml.Node) error {
	var source map[string]any
	if err := node.Decode(&source); err != nil {
		return err
	}
	unsupportedKeywords := map[string]bool{
		"contains": true, "dependentSchemas": true, "if": true, "patternProperties": true,
		"prefixItems": true, "propertyNames": true, "then": true,
		"unevaluatedItems": true, "unevaluatedProperties": true,
	}
	literalSchemaKeywords := map[string]bool{
		"const": true, "default": true, "enum": true, "example": true, "examples": true,
	}
	schemaChildren := map[string]bool{
		"additionalProperties": true, "allOf": true, "anyOf": true,
		"items": true, "not": true, "oneOf": true,
	}
	componentMaps := map[string]bool{
		"callbacks": true, "headers": true, "links": true, "parameters": true,
		"pathItems": true, "requestBodies": true, "responses": true, "securitySchemes": true,
	}
	openAPIObjectMaps := map[string]bool{
		"callbacks": true, "content": true, "encoding": true, "headers": true,
		"links": true, "variables": true, "webhooks": true,
	}
	forbiddenProperties := map[string]bool{
		"access_token": true, "api_key": true, "client_secret": true,
		"credential_value": true, "password": true, "pm_automation_credential": true,
		"provider_credential": true, "provider_secret": true, "refresh_token": true, "secret": true,
	}

	type nodeRole uint8
	const (
		openAPIObjectRole nodeRole = iota
		componentsObjectRole
		componentNameMapRole
		namedOpenAPIObjectMapRole
		extensibleOpenAPIObjectMapRole
		schemaObjectRole
		namedSchemaMapRole
		propertyMapRole
		exampleNameMapRole
		exampleObjectRole
		literalDataRole
	)

	credentialRefs := make([]string, 0, 1)
	var walk func(any, []string, nodeRole) error
	walk = func(value any, path []string, role nodeRole) error {
		if role == literalDataRole {
			return nil
		}
		switch typed := value.(type) {
		case map[string]any:
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				childPath := appendPath(path, key)
				objectField := role == openAPIObjectRole || role == componentsObjectRole ||
					role == schemaObjectRole || role == exampleObjectRole
				if (objectField || role == extensibleOpenAPIObjectMapRole) && strings.HasPrefix(strings.ToLower(key), "x-") {
					continue
				}
				if role == schemaObjectRole && unsupportedKeywords[key] {
					return fmt.Errorf("%s uses schema keyword %q that the selected generator does not project losslessly", strings.Join(childPath, "."), key)
				}
				if role == schemaObjectRole && literalSchemaKeywords[key] {
					if key == "const" && typed[key] == nil {
						return fmt.Errorf("%s: explicit null const cannot be projected losslessly", strings.Join(childPath, "."))
					}
					continue
				}
				if role == propertyMapRole && forbiddenProperties[strings.ToLower(key)] {
					allowed := equalPath(childPath, []string{"components", "schemas", "ServiceAccountCreationResponse", "properties", "pm_automation_credential"})
					if !allowed {
						return fmt.Errorf("%s exposes forbidden secret-bearing public field %q", strings.Join(childPath, "."), key)
					}
				}
				if objectField && key == "$ref" {
					if reference, ok := typed[key].(string); ok && reference == "#/components/schemas/ServiceAccountCreationResponse" {
						credentialRefs = append(credentialRefs, strings.Join(childPath, "."))
					}
				}

				childRole := openAPIObjectRole
				switch role {
				case componentNameMapRole, namedOpenAPIObjectMapRole, extensibleOpenAPIObjectMapRole:
					childRole = openAPIObjectRole
				case namedSchemaMapRole, propertyMapRole:
					childRole = schemaObjectRole
				case exampleNameMapRole:
					childRole = exampleObjectRole
				case componentsObjectRole:
					switch {
					case key == "schemas":
						childRole = namedSchemaMapRole
					case key == "examples":
						childRole = exampleNameMapRole
					case componentMaps[key]:
						childRole = componentNameMapRole
					}
				case schemaObjectRole:
					switch {
					case key == "properties":
						childRole = propertyMapRole
					case schemaChildren[key]:
						childRole = schemaObjectRole
					}
				case exampleObjectRole:
					if key == "value" {
						childRole = literalDataRole
					}
				case openAPIObjectRole:
					switch {
					case key == "components":
						childRole = componentsObjectRole
					case key == "schema":
						childRole = schemaObjectRole
					case key == "examples":
						childRole = exampleNameMapRole
					case key == "example" || key == "default" || key == "enum":
						childRole = literalDataRole
					case key == "mapping" || key == "scopes" || key == "security":
						childRole = literalDataRole
					case key == "paths" || key == "responses":
						childRole = extensibleOpenAPIObjectMapRole
					case openAPIObjectMaps[key]:
						childRole = namedOpenAPIObjectMapRole
					}
				}
				if err := walk(typed[key], childPath, childRole); err != nil {
					return err
				}
			}
		case []any:
			for index, child := range typed {
				if err := walk(child, appendPath(path, strconv.Itoa(index)), role); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(source, nil, openAPIObjectRole); err != nil {
		return err
	}
	paths, ok := source["paths"].(map[string]any)
	if !ok {
		return errors.New("paths must be an object")
	}
	for path := range paths {
		if strings.HasPrefix(strings.ToLower(path), "/admin/") || strings.EqualFold(path, "/admin") {
			return fmt.Errorf("ordinary generated API contains admin ingress %q", path)
		}
	}
	if len(credentialRefs) != 1 || !strings.Contains(credentialRefs[0], "paths./v1/workspaces/{workspace_id}/service-accounts.post.responses.201.") {
		return fmt.Errorf("one-time credential response must be referenced only from service-account creation HTTP 201; references=%v", credentialRefs)
	}
	return nil
}

func appendPath(path []string, element string) []string {
	result := make([]string, len(path)+1)
	copy(result, path)
	result[len(path)] = element
	return result
}

func equalPath(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func loadOpenAPIDocument(ctx context.Context, source []byte) (*openapi3.T, error) {
	loader := openapi3.NewLoader()
	loader.Context = ctx
	loader.IsExternalRefsAllowed = false
	document, err := loader.LoadFromData(source)
	if err != nil {
		return nil, fmt.Errorf("load OpenAPI 3.1 source: %w", err)
	}
	if document.OpenAPI != "3.1.0" {
		return nil, fmt.Errorf("OpenAPI version %q is unsupported; require exact 3.1.0", document.OpenAPI)
	}
	if document.JSONSchemaDialect != "https://json-schema.org/draft/2020-12/schema" {
		return nil, fmt.Errorf("JSON Schema dialect %q is unsupported", document.JSONSchemaDialect)
	}
	if err := document.Validate(ctx); err != nil {
		return nil, fmt.Errorf("validate OpenAPI 3.1 source: %w", err)
	}
	return document, nil
}

func validateAuthoringDocument(document *openapi3.T) error {
	if document.Paths == nil || document.Paths.Value("/v1/info") == nil {
		return errors.New("ordinary public API must expose /v1/info")
	}
	for path, item := range document.Paths.Map() {
		if path != "/v1/info" && strings.Contains(strings.ToLower(path), "/info") {
			return fmt.Errorf("ordinary public API contains a second compatibility route %q", path)
		}
		for _, operation := range item.Operations() {
			for _, tag := range operation.Tags {
				if strings.EqualFold(tag, "admin") || strings.HasPrefix(strings.ToLower(tag), "admin-") {
					return fmt.Errorf("operation %q carries forbidden admin tag %q", operation.OperationID, tag)
				}
			}
		}
	}
	if document.Components == nil {
		return errors.New("OpenAPI components are required")
	}
	for name := range document.Components.Schemas {
		if strings.Contains(strings.ToLower(name), "admin") {
			return fmt.Errorf("ordinary public API contains admin schema %q", name)
		}
	}
	return nil
}

func loadAPIControls(path string) (apiControls, error) {
	content, err := readBoundedFile(path)
	if err != nil {
		return apiControls{}, fmt.Errorf("read API controls: %w", err)
	}
	if _, err := parseYAMLDocument(content); err != nil {
		return apiControls{}, fmt.Errorf("decode API controls: %w", err)
	}
	var raw any
	if err := json.Unmarshal(content, &raw); err != nil {
		return apiControls{}, fmt.Errorf("decode API controls: %w", err)
	}
	if err := validateCanonicalJSONFields(raw, reflect.TypeOf(apiControls{}), "$"); err != nil {
		return apiControls{}, fmt.Errorf("decode API controls: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var controls apiControls
	if err := decoder.Decode(&controls); err != nil {
		return apiControls{}, fmt.Errorf("decode API controls: %w", err)
	}
	if err := requireAPIGenerationJSONEOF(decoder); err != nil {
		return apiControls{}, fmt.Errorf("decode API controls: %w", err)
	}
	return controls, nil
}

func validateCanonicalJSONFields(value any, expected reflect.Type, path string) error {
	for expected.Kind() == reflect.Pointer {
		expected = expected.Elem()
	}
	switch expected.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be an object", path)
		}
		fields := make(map[string]reflect.Type, expected.NumField())
		for index := range expected.NumField() {
			field := expected.Field(index)
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name != "" && name != "-" {
				fields[name] = field.Type
			}
		}
		for name, child := range object {
			fieldType, ok := fields[name]
			if !ok {
				return fmt.Errorf("%s contains non-canonical or unknown field %q", path, name)
			}
			if err := validateCanonicalJSONFields(child, fieldType, path+"."+name); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		array, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s must be an array", path)
		}
		for index, child := range array {
			if err := validateCanonicalJSONFields(child, expected.Elem(), fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	case reflect.Map:
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s must be an object", path)
		}
		for name, child := range object {
			if err := validateCanonicalJSONFields(child, expected.Elem(), path+"."+name); err != nil {
				return err
			}
		}
	}
	return nil
}

func requireAPIGenerationJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func validateAPIControls(controls apiControls, document *openapi3.T) error {
	if controls.SchemaVersion != "pm.api.controls/v1" || controls.WireVersion != "pm.api/v1" || controls.ProtocolMajor != 1 {
		return errors.New("controls schema, wire version, and protocol major must remain at accepted v1 values")
	}
	for name, values := range map[string][]string{
		"supported_features":         controls.SupportedFeatures,
		"protocol_metadata.required": controls.ProtocolMetadata.Required,
		"protocol_metadata.optional": controls.ProtocolMetadata.Optional,
		"forbidden_public_fields":    controls.ForbiddenPublicFields,
		"run_states":                 controls.RunStates,
		"terminal_run_states":        controls.TerminalRunStates,
	} {
		if err := requireUniqueNonempty(name, values); err != nil {
			return err
		}
	}
	if len(controls.Problems) == 0 {
		return errors.New("problem rules are required")
	}
	problemKeys := make(map[string]struct{}, len(controls.Problems))
	for _, rule := range controls.Problems {
		if rule.Status < 400 || rule.Status > 599 || rule.Code == "" || rule.SafeRetryAction == "" {
			return fmt.Errorf("invalid problem rule %#v", rule)
		}
		switch rule.Disposition {
		case "confirmed_refusal", "outcome_unknown", "resolved_receipt_expired", "unclassified_failure":
		default:
			return fmt.Errorf("invalid problem disposition %q", rule.Disposition)
		}
		key := fmt.Sprintf("%d\x00%s", rule.Status, rule.Code)
		if _, exists := problemKeys[key]; exists {
			return fmt.Errorf("duplicate problem rule for HTTP %d %s", rule.Status, rule.Code)
		}
		problemKeys[key] = struct{}{}
	}
	operationIDs := make(map[string]struct{}, len(controls.Operations))
	routes := make(map[string]struct{}, len(controls.Operations))
	for _, control := range controls.Operations {
		if control.OperationID == "" || control.Method == "" || control.Path == "" {
			return fmt.Errorf("incomplete operation control %#v", control)
		}
		if _, exists := operationIDs[control.OperationID]; exists {
			return fmt.Errorf("duplicate operation control %q", control.OperationID)
		}
		operationIDs[control.OperationID] = struct{}{}
		routeKey := strings.ToUpper(control.Method) + " " + control.Path
		if _, exists := routes[routeKey]; exists {
			return fmt.Errorf("duplicate controlled route %q", routeKey)
		}
		routes[routeKey] = struct{}{}
		pathItem := document.Paths.Value(control.Path)
		if pathItem == nil {
			return fmt.Errorf("controlled route %q is absent from OpenAPI", routeKey)
		}
		operation := pathItem.Operations()[strings.ToUpper(control.Method)]
		if operation == nil || operation.OperationID != control.OperationID {
			return fmt.Errorf("controlled route %q does not bind operation %q", routeKey, control.OperationID)
		}
		if err := requireUniqueNonempty(control.OperationID+".required_body_fields", control.RequiredBodyFields); err != nil {
			return err
		}
		if err := requireUniqueNonempty(control.OperationID+".optional_body_fields", control.OptionalBodyFields); err != nil {
			return err
		}
	}
	for path, item := range document.Paths.Map() {
		for method, operation := range item.Operations() {
			if operation.OperationID == "getServerInfo" {
				continue
			}
			if _, exists := operationIDs[operation.OperationID]; !exists {
				return fmt.Errorf("public operation %s %s (%s) has no pre-handler control", method, path, operation.OperationID)
			}
		}
	}
	return nil
}

func requireUniqueNonempty(name string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s contains an empty value", name)
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("%s contains duplicate %q", name, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func loadAPICodegenConfiguration(path string) (apiCodegenConfiguration, error) {
	content, err := readBoundedFile(path)
	if err != nil {
		return apiCodegenConfiguration{}, fmt.Errorf("read oapi-codegen configuration: %w", err)
	}
	if _, err := parseYAMLDocument(content); err != nil {
		return apiCodegenConfiguration{}, fmt.Errorf("parse oapi-codegen configuration: %w", err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	var configuration apiCodegenConfiguration
	if err := decoder.Decode(&configuration); err != nil {
		return apiCodegenConfiguration{}, fmt.Errorf("decode oapi-codegen configuration: %w", err)
	}
	return configuration, nil
}

func validateAPICodegenConfiguration(configuration apiCodegenConfiguration) error {
	if configuration.PackageName != "api" || configuration.OutputFile != "internal/api/types.gen.go" {
		return errors.New("oapi-codegen configuration must own internal/api/types.gen.go in package api")
	}
	if !configuration.Generate.Models || configuration.Generate.AnyOperationGenerator() {
		return errors.New("base oapi-codegen configuration must generate models only")
	}
	if !configuration.OutputOptions.NullableType {
		return errors.New("nullable-type generation is required for OpenAPI 3.1 nullable unions")
	}
	if !configuration.Compatibility.DisableRequiredReadOnlyAsPointer {
		return errors.New("required readOnly fields must remain required values")
	}
	if err := configuration.Configuration.Validate(); err != nil {
		return err
	}
	return nil
}

func generateAPIOutputs(ctx context.Context, document *openapi3.T, controls apiControls, configuration apiCodegenConfiguration, admin adminAPIComposition) (map[string][]byte, error) {
	version := apiGeneratorVersion
	base := configuration.Configuration
	base.NoVCSVersionOverride = &version

	// Codegen filters excluded operations in place. Preserve the complete
	// public contract and protocol controls before generating ordinary clients
	// and handlers; manual streaming routes still belong to that contract.
	controlsSource, err := generateControlsSource(controls, document)
	if err != nil {
		return nil, err
	}
	canonicalJSON, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal canonical OpenAPI JSON: %w", err)
	}
	canonicalJSON = append(canonicalJSON, '\n')

	modelSource, err := codegen.Generate(document, base)
	if err != nil {
		return nil, fmt.Errorf("generate API models: %w", err)
	}
	modelFileSet := token.NewFileSet()
	modelFile, err := parser.ParseFile(modelFileSet, "types.gen.go", modelSource, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse generated API models: %w", err)
	}
	if err := verifyGeneratedFidelity(document, modelFile, modelFileSet); err != nil {
		return nil, fmt.Errorf("generated API models are lossy: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	clientConfiguration := base
	clientConfiguration.Generate = codegen.GenerateOptions{Client: true}
	clientSource, err := codegen.Generate(document, clientConfiguration)
	if err != nil {
		return nil, fmt.Errorf("generate API client: %w", err)
	}

	serverDocument, err := loadOpenAPIDocument(ctx, admin.serverSource)
	if err != nil {
		return nil, fmt.Errorf("reload OpenAPI for server projection: %w", err)
	}
	serverConfiguration := base
	serverConfiguration.PackageName = "httpapi"
	serverConfiguration.Generate = codegen.GenerateOptions{GinServer: true, Strict: true}
	serverSource, err := codegen.Generate(serverDocument, serverConfiguration)
	if err != nil {
		return nil, fmt.Errorf("generate Gin server boundary: %w", err)
	}
	serverSource += generateServerSchemaAliases(modelFile)
	serverSource += generatedServerBoundary
	serverSource, err = addGeneratedNamedImport(serverSource, "api", "github.com/polymetrics-ai/pm-cli/internal/api")
	if err != nil {
		return nil, err
	}
	formattedServer, err := projectServerBoundary(serverSource)
	if err != nil {
		return nil, fmt.Errorf("format generated Gin server boundary: %w", err)
	}

	adminModels, err := generateAdminModels(ctx, admin, base, modelFile)
	if err != nil {
		return nil, err
	}

	outputs := map[string][]byte{
		"api/pm.openapi.json":            canonicalJSON,
		"internal/api/client.gen.go":     []byte(clientSource),
		"internal/api/controls.gen.go":   controlsSource,
		"internal/api/types.gen.go":      []byte(modelSource),
		"internal/httpapi/admin.gen.go":  adminModels,
		"internal/httpapi/server.gen.go": formattedServer,
	}
	outputs["api/generated.sha256"] = generateAPIDigestManifest(outputs)
	return outputs, nil
}

// Strict operations receive the standard request context, never Gin's pooled
// context. Strict response visitors remain local types but serialize through the
// canonical API DTO and its generated MarshalJSON implementation.
func projectServerBoundary(source string) ([]byte, error) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "server.gen.go", source, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	aliases := make(map[string]*ast.TypeSpec)
	expectedContexts, projectedContexts := 0, 0
	for _, declaration := range file.Decls {
		if generic, ok := declaration.(*ast.GenDecl); ok {
			for _, specification := range generic.Specs {
				definition, ok := specification.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if definition.Name.Name == "StrictServerInterface" {
					boundary, ok := definition.Type.(*ast.InterfaceType)
					if !ok {
						return nil, errors.New("strict server interface has an unsupported projection")
					}
					expectedContexts = boundary.Methods.NumFields()
				}
				if definition.Assign.IsValid() {
					if _, local := definition.Type.(*ast.Ident); local {
						aliases[definition.Name.Name] = definition
					}
				}
			}
		}
	}
	for _, declaration := range file.Decls {
		visitor, ok := declaration.(*ast.FuncDecl)
		if !ok || visitor.Recv == nil {
			continue
		}
		if pointer, ok := visitor.Recv.List[0].Type.(*ast.StarExpr); ok {
			if receiver, ok := pointer.X.(*ast.Ident); ok && receiver.Name == "strictHandler" {
				projected := 0
				ast.Inspect(visitor.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok || len(call.Args) != 2 {
						return true
					}
					method, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					service, ok := method.X.(*ast.SelectorExpr)
					if !ok || service.Sel.Name != "ssi" {
						return true
					}
					instance, ok := service.X.(*ast.Ident)
					if !ok || instance.Name != visitor.Recv.List[0].Names[0].Name {
						return true
					}
					requestContext, ok := call.Args[0].(*ast.Ident)
					if !ok || requestContext.Name != "ctx" {
						return true
					}
					call.Args[0] = &ast.CallExpr{Fun: &ast.SelectorExpr{
						X:   &ast.SelectorExpr{X: requestContext, Sel: ast.NewIdent("Request")},
						Sel: ast.NewIdent("Context"),
					}}
					projected++
					return true
				})
				if projected != 1 {
					return nil, fmt.Errorf("strict operation %s has an unsupported request context projection", visitor.Name.Name)
				}
				projectedContexts++
				continue
			}
		}
		if !strings.HasPrefix(visitor.Name.Name, "Visit") {
			continue
		}
		receiver := visitor.Recv.List[0]
		receiverType, ok := receiver.Type.(*ast.Ident)
		if !ok || aliases[receiverType.Name] == nil {
			continue
		}
		definition := aliases[receiverType.Name]
		receiverName := receiver.Names[0].Name
		projected := 0
		ast.Inspect(visitor.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			method, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || method.Sel.Name != "Encode" {
				return true
			}
			value := call.Args[0]
			if field, ok := value.(*ast.SelectorExpr); ok && field.Sel.Name == "union" {
				value = field.X
			}
			if identifier, ok := value.(*ast.Ident); ok && identifier.Name == receiverName {
				call.Args[0] = &ast.CallExpr{Fun: definition.Type, Args: []ast.Expr{ast.NewIdent(receiverName)}}
				projected++
			}
			return true
		})
		if projected != 1 {
			return nil, fmt.Errorf("strict response %s has an unsupported canonical payload projection", receiverType.Name)
		}
		definition.Assign = token.NoPos
	}
	if expectedContexts == 0 || projectedContexts != expectedContexts {
		return nil, fmt.Errorf("strict request context projection covers %d of %d operations", projectedContexts, expectedContexts)
	}
	var output bytes.Buffer
	if err := format.Node(&output, fileSet, file); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func addGeneratedNamedImport(source, name, importPath string) (string, error) {
	const marker = "\n\n\t\"github.com/gin-gonic/gin\""
	if strings.Count(source, marker) != 1 {
		return "", errors.New("generated Gin import block is missing or ambiguous")
	}
	declaration := fmt.Sprintf("\n\n\t%s %q\n\t\"github.com/gin-gonic/gin\"", name, importPath)
	return strings.Replace(source, marker, declaration, 1), nil
}

func generateServerSchemaAliases(modelFile *ast.File) string {
	var names []string
	for _, declaration := range modelFile.Decls {
		generic, ok := declaration.(*ast.GenDecl)
		if !ok || generic.Tok != token.TYPE {
			continue
		}
		for _, specification := range generic.Specs {
			if modelType, ok := specification.(*ast.TypeSpec); ok {
				names = append(names, modelType.Name.Name)
			}
		}
	}
	sort.Strings(names)
	var aliases strings.Builder
	aliases.WriteString("\n// The Gin boundary aliases the single generated DTO projection in internal/api.\n")
	for _, name := range names {
		fmt.Fprintf(&aliases, "type %s = api.%s\n", name, name)
	}
	return aliases.String()
}

const generatedServerBoundary = `

// PMProtocolMiddleware validates the generated operation's exact public
// metadata envelope before request-body binding or application code.
func PMProtocolMiddleware(c *gin.Context) {
	if isAdminAPIRoute(c.Request.Method, c.FullPath()) {
		if err := validateAdminHTTPRequest(c.Request); err != nil {
			problem := api.NewProtocolProblem(err)
			c.Header("Content-Type", "application/problem+json")
			c.AbortWithStatusJSON(http.StatusBadRequest, problem)
		}
		return
	}
	operationID, ok := api.OperationIDForRoute(c.Request.Method, c.FullPath())
	if !ok {
		problem := api.NewProtocolProblem(nil)
		c.Header("Content-Type", "application/problem+json")
		c.AbortWithStatusJSON(http.StatusBadRequest, problem)
		return
	}
	if err := api.ValidateHTTPRequest(operationID, c.Request); err != nil {
		problem := api.NewProtocolProblem(err)
		c.Header("Content-Type", "application/problem+json")
		c.AbortWithStatusJSON(http.StatusBadRequest, problem)
	}
}

// RegisterPMStrictHandlers installs the exact pre-handler protocol guard ahead
// of caller middleware and the generated strict request binder. It performs no
// application, authentication, durable-state, or provider work.
func RegisterPMStrictHandlers(router gin.IRouter, service StrictServerInterface, strictMiddlewares []StrictMiddlewareFunc, strictOptions StrictGinServerOptions, options GinServerOptions) error {
	if options.BaseURL != "" {
		return fmt.Errorf("ordinary PM API base URL must be empty; canonical routes already include /v1")
	}
	if strictOptions.RequestErrorHandlerFunc == nil {
		strictOptions.RequestErrorHandlerFunc = func(c *gin.Context, err error) {
			problem := api.NewProtocolProblem(err)
			c.Header("Content-Type", "application/problem+json")
			c.AbortWithStatusJSON(http.StatusBadRequest, problem)
		}
	}
	if strictOptions.HandlerErrorFunc == nil {
		strictOptions.HandlerErrorFunc = func(c *gin.Context, err error) {
			problem := api.NewInternalProblem(err)
			c.Header("Content-Type", "application/problem+json")
			c.AbortWithStatusJSON(http.StatusInternalServerError, problem)
		}
	}
	if strictOptions.ResponseErrorHandlerFunc == nil {
		strictOptions.ResponseErrorHandlerFunc = strictOptions.HandlerErrorFunc
	}
	if options.ErrorHandler == nil {
		options.ErrorHandler = func(c *gin.Context, err error, status int) {
			problem := api.NewProtocolProblem(err)
			c.Header("Content-Type", "application/problem+json")
			c.AbortWithStatusJSON(status, problem)
		}
	}
	options.Middlewares = append([]MiddlewareFunc{PMProtocolMiddleware}, options.Middlewares...)
	RegisterHandlersWithOptions(router, NewStrictHandlerWithOptions(service, strictMiddlewares, strictOptions), options)
	return nil
}
`

func generateControlsSource(controls apiControls, document *openapi3.T) ([]byte, error) {
	var source strings.Builder
	source.WriteString("// Package api contains code generated from api/controls.json; DO NOT EDIT.\n")
	source.WriteString("package api\n\nimport \"strings\"\n\n")
	fmt.Fprintf(&source, "const WireVersion = %q\n", controls.WireVersion)
	fmt.Fprintf(&source, "const ProtocolMajor = %d\n\n", controls.ProtocolMajor)
	source.WriteString("type ProblemDisposition string\n\nconst (\n")
	source.WriteString("\tProblemConfirmedRefusal ProblemDisposition = \"confirmed_refusal\"\n")
	source.WriteString("\tProblemOutcomeUnknown ProblemDisposition = \"outcome_unknown\"\n")
	source.WriteString("\tProblemResolvedReceiptExpired ProblemDisposition = \"resolved_receipt_expired\"\n")
	source.WriteString("\tProblemUnclassifiedFailure ProblemDisposition = \"unclassified_failure\"\n")
	source.WriteString(")\n\n")
	source.WriteString("type ProblemRule struct {\n\tStatus int\n\tCode string\n\tSafeRetryAction string\n\tDisposition ProblemDisposition\n}\n\n")
	source.WriteString("var ProblemRules = [...]ProblemRule{\n")
	for _, rule := range controls.Problems {
		fmt.Fprintf(&source, "\t{Status: %d, Code: %q, SafeRetryAction: %q, Disposition: ProblemDisposition(%q)},\n", rule.Status, rule.Code, rule.SafeRetryAction, rule.Disposition)
	}
	source.WriteString("}\n\n")
	source.WriteString("func ProblemRuleFor(status int, code string) (ProblemRule, bool) {\n\tfor _, rule := range ProblemRules {\n\t\tif rule.Status == status && rule.Code == code {\n\t\t\treturn rule, true\n\t\t}\n\t}\n\treturn ProblemRule{}, false\n}\n\n")

	writeStringSlice(&source, "SupportedFeatures", controls.SupportedFeatures)
	writeStringSlice(&source, "ProtocolMetadataRequiredFields", controls.ProtocolMetadata.Required)
	writeStringSlice(&source, "ProtocolMetadataOptionalFields", controls.ProtocolMetadata.Optional)
	writeStringSlice(&source, "ForbiddenPublicFields", controls.ForbiddenPublicFields)
	writeStringSlice(&source, "RunStates", controls.RunStates)
	writeStringSlice(&source, "TerminalRunStates", controls.TerminalRunStates)

	source.WriteString("type OperationControl struct {\n\tOperationID string\n\tMethod string\n\tPath string\n\tRequiredBodyFields []string\n\tOptionalBodyFields []string\n\tRequiresIdempotencyKey bool\n\tRequiresIfMatch bool\n\tHasBody bool\n\tHasProtocol bool\n\tQueryFields []string\n}\n\n")
	source.WriteString("var operationControls = map[string]OperationControl{\n")
	for _, operation := range controls.Operations {
		pathItem := document.Paths.Value(operation.Path)
		definition := pathItem.Operations()[strings.ToUpper(operation.Method)]
		queryFields := make([]string, 0)
		for _, parameters := range []openapi3.Parameters{pathItem.Parameters, definition.Parameters} {
			for _, parameter := range parameters {
				if parameter.Value == nil || parameter.Value.In != "query" {
					continue
				}
				if parameter.Value.Schema == nil || parameter.Value.Schema.Value == nil || parameter.Value.Schema.Value.Type.Is("array") || parameter.Value.Schema.Value.Type.Is("object") {
					return nil, fmt.Errorf("operation %s requires an unsupported query parameter shape", operation.OperationID)
				}
				queryFields = append(queryFields, parameter.Value.Name)
			}
		}
		sort.Strings(queryFields)
		hasProtocol := false
		for _, field := range operation.RequiredBodyFields {
			hasProtocol = hasProtocol || field == "protocol"
		}
		fmt.Fprintf(&source, "\t%q: {OperationID: %q, Method: %q, Path: %q, RequiredBodyFields: %#v, OptionalBodyFields: %#v, RequiresIdempotencyKey: %t, RequiresIfMatch: %t, HasBody: %t, HasProtocol: %t, QueryFields: %#v},\n",
			operation.OperationID, operation.OperationID, strings.ToUpper(operation.Method), operation.Path, operation.RequiredBodyFields, operation.OptionalBodyFields, operation.RequiresIdempotencyKey, operation.RequiresIfMatch, definition.RequestBody != nil, hasProtocol, queryFields)
	}
	source.WriteString("}\n\n")
	source.WriteString("var operationIDsByRoute = map[string]string{\n")
	for _, route := range publicOperationRoutes(document) {
		fmt.Fprintf(&source, "\t%q: %q,\n", route.Method+" "+route.Path, route.OperationID)
		ginPath := ginRoutePath(route.Path)
		if ginPath != route.Path {
			fmt.Fprintf(&source, "\t%q: %q,\n", route.Method+" "+ginPath, route.OperationID)
		}
	}
	source.WriteString("}\n\n")
	objectControls, err := collectOperationObjectControls(document, controls)
	if err != nil {
		return nil, err
	}
	source.WriteString("type JSONFieldControl struct {\n\tAllowedFields []string\n\tRequiredFields []string\n\tNullable bool\n}\n\n")
	source.WriteString("var operationObjectControls = map[string]map[string]JSONFieldControl{\n")
	for _, operation := range controls.Operations {
		source.WriteString("\t" + strconv.Quote(operation.OperationID) + ": {\n")
		pointers := make([]string, 0, len(objectControls[operation.OperationID]))
		for pointer := range objectControls[operation.OperationID] {
			pointers = append(pointers, pointer)
		}
		sort.Strings(pointers)
		for _, pointer := range pointers {
			control := objectControls[operation.OperationID][pointer]
			fmt.Fprintf(&source, "\t\t%q: {AllowedFields: %#v, RequiredFields: %#v, Nullable: %t},\n", pointer, control.Allowed, control.Required, control.Nullable)
		}
		source.WriteString("\t},\n")
	}
	source.WriteString("}\n\n")
	source.WriteString("func OperationControlFor(operationID string) (OperationControl, bool) {\n\tcontrol, ok := operationControls[operationID]\n\tif !ok {\n\t\treturn OperationControl{}, false\n\t}\n\tcontrol.RequiredBodyFields = append([]string(nil), control.RequiredBodyFields...)\n\tcontrol.OptionalBodyFields = append([]string(nil), control.OptionalBodyFields...)\n\tcontrol.QueryFields = append([]string(nil), control.QueryFields...)\n\treturn control, true\n}\n\n")
	source.WriteString("func OperationIDForRoute(method, path string) (string, bool) {\n\toperationID, ok := operationIDsByRoute[strings.ToUpper(method)+\" \"+path]\n\treturn operationID, ok\n}\n\n")
	source.WriteString("func IsTerminalRunState[T ~string](state T) bool {\n\tswitch state {\n")
	writeGeneratedStringCase(&source, controls.TerminalRunStates)
	source.WriteString("\t\treturn true\n\tdefault:\n\t\treturn false\n\t}\n}\n\n")
	source.WriteString("func IsSupportedFeature(feature string) bool {\n\tswitch feature {\n")
	writeGeneratedStringCase(&source, controls.SupportedFeatures)
	source.WriteString("\t\treturn true\n\tdefault:\n\t\treturn false\n\t}\n}\n\n")
	source.WriteString("func IsForbiddenPublicField(field string) bool {\n\tswitch strings.ToLower(field) {\n")
	lowerForbiddenFields := make([]string, len(controls.ForbiddenPublicFields))
	for index, field := range controls.ForbiddenPublicFields {
		lowerForbiddenFields[index] = strings.ToLower(field)
	}
	writeGeneratedStringCase(&source, lowerForbiddenFields)
	source.WriteString("\t\treturn true\n\tdefault:\n\t\treturn false\n\t}\n}\n")
	formatted, err := format.Source([]byte(source.String()))
	if err != nil {
		return nil, fmt.Errorf("format generated controls: %w", err)
	}
	return formatted, nil
}

func writeGeneratedStringCase(source *strings.Builder, values []string) {
	source.WriteString("\tcase ")
	for index, value := range values {
		if index != 0 {
			source.WriteString(", ")
		}
		fmt.Fprintf(source, "%q", value)
	}
	source.WriteString(":\n")
}

type publicOperationRoute struct {
	Method      string
	Path        string
	OperationID string
}

func publicOperationRoutes(document *openapi3.T) []publicOperationRoute {
	var routes []publicOperationRoute
	for path, item := range document.Paths.Map() {
		for method, operation := range item.Operations() {
			if operation != nil && operation.OperationID != "" {
				routes = append(routes, publicOperationRoute{
					Method:      strings.ToUpper(method),
					Path:        path,
					OperationID: operation.OperationID,
				})
			}
		}
	}
	sort.Slice(routes, func(left, right int) bool {
		leftKey := routes[left].Method + " " + routes[left].Path
		rightKey := routes[right].Method + " " + routes[right].Path
		return leftKey < rightKey
	})
	return routes
}

func ginRoutePath(path string) string {
	segments := strings.Split(path, "/")
	for index, segment := range segments {
		if len(segment) > 2 && strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			segments[index] = ":" + segment[1:len(segment)-1]
		}
	}
	return strings.Join(segments, "/")
}

type generatedObjectControl struct {
	Allowed  []string
	Required []string
	Nullable bool
}

func collectOperationObjectControls(document *openapi3.T, controls apiControls) (map[string]map[string]generatedObjectControl, error) {
	result := make(map[string]map[string]generatedObjectControl, len(controls.Operations))
	for _, control := range controls.Operations {
		pathItem := document.Paths.Value(control.Path)
		if pathItem == nil {
			return nil, fmt.Errorf("collect object controls: missing path %s", control.Path)
		}
		operation := pathItem.Operations()[strings.ToUpper(control.Method)]
		if operation == nil {
			return nil, fmt.Errorf("collect object controls: missing operation %s", control.OperationID)
		}
		if operation.RequestBody == nil {
			if len(control.RequiredBodyFields) != 0 || len(control.OptionalBodyFields) != 0 {
				return nil, fmt.Errorf("collect object controls: bodyless operation %s declares body fields", control.OperationID)
			}
			result[control.OperationID] = map[string]generatedObjectControl{}
			continue
		}
		if operation.RequestBody.Value == nil {
			return nil, fmt.Errorf("collect object controls: %s has an unresolved request body", control.OperationID)
		}
		mediaType := operation.RequestBody.Value.Content["application/json"]
		if mediaType == nil || mediaType.Schema == nil {
			return nil, fmt.Errorf("collect object controls: %s has no application/json schema", control.OperationID)
		}
		objects := make(map[string]generatedObjectControl)
		collectSchemaObjectControls(mediaType.Schema, "", objects, make(map[string]bool))
		root, ok := objects[""]
		if !ok {
			return nil, fmt.Errorf("collect object controls: %s request root is not an object", control.OperationID)
		}
		expectedAllowed := append(append([]string(nil), control.RequiredBodyFields...), control.OptionalBodyFields...)
		sort.Strings(expectedAllowed)
		expectedRequired := append([]string(nil), control.RequiredBodyFields...)
		sort.Strings(expectedRequired)
		if !equalStrings(root.Allowed, expectedAllowed) || !equalStrings(root.Required, expectedRequired) {
			return nil, fmt.Errorf("control fields for %s do not match its OpenAPI request schema", control.OperationID)
		}
		result[control.OperationID] = objects
	}
	return result, nil
}

func collectSchemaObjectControls(reference *openapi3.SchemaRef, pointer string, result map[string]generatedObjectControl, visiting map[string]bool) {
	if reference == nil || reference.Value == nil || strings.HasSuffix(reference.Ref, "/PresentValue") {
		return
	}
	visitKey := reference.Ref + "\x00" + pointer
	if visiting[visitKey] {
		return
	}
	visiting[visitKey] = true
	defer delete(visiting, visitKey)

	schema := reference.Value
	objectSchema := len(schema.Properties) != 0 || (schema.Type != nil && schema.Type.Includes(openapi3.TypeObject))
	if objectSchema {
		allowed := make([]string, 0, len(schema.Properties))
		for name := range schema.Properties {
			allowed = append(allowed, name)
		}
		sort.Strings(allowed)
		required := append([]string(nil), schema.Required...)
		sort.Strings(required)
		mergeGeneratedObjectControl(result, pointer, allowed, required, schemaAllowsNull(schema))
		for _, name := range allowed {
			collectSchemaObjectControls(schema.Properties[name], pointer+"/"+escapeJSONPointer(name), result, visiting)
		}
	}
	for _, member := range schema.AllOf {
		collectSchemaObjectControls(member, pointer, result, visiting)
	}
}

func schemaAllowsNull(schema *openapi3.Schema) bool {
	if schema == nil {
		return false
	}
	if schema.Always != nil {
		return *schema.Always
	}
	if !schema.Nullable && schema.Type != nil && !schema.Type.IncludesNull() {
		return false
	}
	if schema.Const != nil {
		return false
	}
	if len(schema.Enum) != 0 {
		for _, value := range schema.Enum {
			if value == nil {
				return true
			}
		}
		return false
	}
	return true
}

func mergeGeneratedObjectControl(result map[string]generatedObjectControl, pointer string, allowed, required []string, nullable bool) {
	existing, present := result[pointer]
	if !present {
		existing.Nullable = nullable
	} else {
		existing.Nullable = existing.Nullable && nullable
	}
	existing.Allowed = unionSortedStrings(existing.Allowed, allowed)
	existing.Required = unionSortedStrings(existing.Required, required)
	result[pointer] = existing
}

func unionSortedStrings(left, right []string) []string {
	set := make(map[string]struct{}, len(left)+len(right))
	for _, value := range left {
		set[value] = struct{}{}
	}
	for _, value := range right {
		set[value] = struct{}{}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func escapeJSONPointer(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func writeStringSlice(source *strings.Builder, name string, values []string) {
	fmt.Fprintf(source, "var %s = [...]string{", name)
	for index, value := range values {
		if index != 0 {
			source.WriteString(", ")
		}
		fmt.Fprintf(source, "%q", value)
	}
	source.WriteString("}\n\n")
}

func verifyGeneratedFidelity(document *openapi3.T, file *ast.File, fileSet *token.FileSet) error {
	structures := make(map[string]*ast.StructType)
	for _, declaration := range file.Decls {
		generic, ok := declaration.(*ast.GenDecl)
		if !ok || generic.Tok != token.TYPE {
			continue
		}
		for _, specification := range generic.Specs {
			typeSpecification, ok := specification.(*ast.TypeSpec)
			if !ok {
				continue
			}
			if structure, ok := typeSpecification.Type.(*ast.StructType); ok {
				structures[typeSpecification.Name.Name] = structure
			}
		}
	}
	names := make([]string, 0, len(document.Components.Schemas))
	for name := range document.Components.Schemas {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		reference := document.Components.Schemas[name]
		if reference == nil || reference.Value == nil {
			continue
		}
		if _, customType := reference.Value.Extensions["x-go-type"]; customType {
			continue
		}
		if len(reference.Value.Properties) == 0 && len(reference.Value.AllOf) == 0 {
			continue
		}
		structure := structures[name]
		if structure == nil {
			return fmt.Errorf("schema %s has object constraints but did not generate a same-named struct", name)
		}
		if err := verifyGeneratedSchemaFields(name, reference.Value, structure, structures, fileSet, map[*openapi3.Schema]bool{}); err != nil {
			return err
		}
	}
	return nil
}

func verifyGeneratedSchemaFields(name string, schema *openapi3.Schema, structure *ast.StructType, structures map[string]*ast.StructType, fileSet *token.FileSet, visiting map[*openapi3.Schema]bool) error {
	if schema == nil || visiting[schema] {
		return nil
	}
	visiting[schema] = true
	defer delete(visiting, schema)

	required := make(map[string]bool, len(schema.Required))
	for _, property := range schema.Required {
		required[property] = true
	}
	properties := make([]string, 0, len(schema.Properties))
	for property := range schema.Properties {
		properties = append(properties, property)
	}
	sort.Strings(properties)
	for _, property := range properties {
		field, ok := generatedJSONField(structure, property, structures, map[*ast.StructType]bool{})
		if !ok {
			return fmt.Errorf("schema %s property %s was dropped from the generated model", name, property)
		}
		omitsEmpty := jsonTagHasOmitEmpty(field.Tag)
		if required[property] && omitsEmpty {
			return fmt.Errorf("schema %s required property %s became optional", name, property)
		}
		if !required[property] && !omitsEmpty {
			return fmt.Errorf("schema %s optional property %s became required", name, property)
		}
		propertyReference := schema.Properties[property]
		if propertyReference == nil || propertyReference.Value == nil {
			continue
		}
		nullable := propertyReference.Value.Type != nil && propertyReference.Value.Type.IncludesNull()
		explicitNullable := isExplicitNullableType(field.Type, fileSet)
		if nullable && !explicitNullable {
			return fmt.Errorf("schema %s nullable property %s lost explicit null presence", name, property)
		}
		if !nullable && explicitNullable {
			return fmt.Errorf("schema %s non-null property %s became nullable", name, property)
		}
		if required[property] && propertyReference.Value.ReadOnly {
			if _, pointer := field.Type.(*ast.StarExpr); pointer {
				return fmt.Errorf("schema %s required readOnly property %s became a pointer", name, property)
			}
		}
	}
	for _, branch := range schema.AllOf {
		if branch == nil || branch.Value == nil {
			return fmt.Errorf("schema %s contains an unresolved allOf branch", name)
		}
		if err := verifyGeneratedSchemaFields(name, branch.Value, structure, structures, fileSet, visiting); err != nil {
			return err
		}
	}
	return nil
}

func isExplicitNullableType(expression ast.Expr, fileSet *token.FileSet) bool {
	switch typed := expression.(type) {
	case *ast.StarExpr:
		return isExplicitNullableType(typed.X, fileSet)
	case *ast.IndexExpr:
		return renderedNode(fileSet, typed.X) == "nullable.Nullable"
	case *ast.IndexListExpr:
		return renderedNode(fileSet, typed.X) == "nullable.Nullable"
	default:
		return false
	}
}

func generatedJSONField(structure *ast.StructType, jsonName string, structures map[string]*ast.StructType, visiting map[*ast.StructType]bool) (*ast.Field, bool) {
	if structure == nil || visiting[structure] {
		return nil, false
	}
	visiting[structure] = true
	defer delete(visiting, structure)
	for _, field := range structure.Fields.List {
		if len(field.Names) != 0 {
			if jsonTagName(field.Tag) == jsonName {
				return field, true
			}
			continue
		}
		identifier, ok := field.Type.(*ast.Ident)
		if !ok {
			continue
		}
		if nested, ok := generatedJSONField(structures[identifier.Name], jsonName, structures, visiting); ok {
			return nested, true
		}
	}
	return nil, false
}

func jsonTagName(tag *ast.BasicLit) string {
	if tag == nil {
		return ""
	}
	unquoted, err := strconv.Unquote(tag.Value)
	if err != nil {
		return ""
	}
	for _, part := range strings.Fields(unquoted) {
		if !strings.HasPrefix(part, `json:"`) {
			continue
		}
		value := strings.TrimSuffix(strings.TrimPrefix(part, `json:"`), `"`)
		return strings.Split(value, ",")[0]
	}
	return ""
}

func renderedNode(fileSet *token.FileSet, node ast.Node) string {
	var output bytes.Buffer
	if err := format.Node(&output, fileSet, node); err != nil {
		return ""
	}
	return output.String()
}

func jsonTagHasOmitEmpty(tag *ast.BasicLit) bool {
	if tag == nil {
		return false
	}
	unquoted, err := strconv.Unquote(tag.Value)
	if err != nil {
		return true
	}
	return strings.Contains(unquoted, "omitempty")
}

func generateAPIDigestManifest(outputs map[string][]byte) []byte {
	paths := append([]string(nil), generatedAPIPaths...)
	sort.Strings(paths)
	var manifest strings.Builder
	for _, path := range paths {
		digest := sha256.Sum256(outputs[path])
		manifest.WriteString(hex.EncodeToString(digest[:]))
		manifest.WriteString("  ")
		manifest.WriteString(path)
		manifest.WriteByte('\n')
	}
	return []byte(manifest.String())
}

func compareGeneratedAPIOutputs(outputRoot string, outputs map[string][]byte) error {
	paths := sortedOutputPaths(outputs)
	var mismatches []string
	for _, relative := range paths {
		actual, err := os.ReadFile(filepath.Join(outputRoot, filepath.FromSlash(relative)))
		if err != nil {
			mismatches = append(mismatches, fmt.Sprintf("%s: %v", relative, err))
			continue
		}
		if !bytes.Equal(actual, outputs[relative]) {
			mismatches = append(mismatches, relative+": differs")
		}
	}
	if len(mismatches) != 0 {
		return fmt.Errorf("generated outputs are stale: %s", strings.Join(mismatches, "; "))
	}
	return nil
}

func writeGeneratedAPIOutputs(outputRoot string, outputs map[string][]byte) error {
	for _, relative := range sortedOutputPaths(outputs) {
		path := filepath.Join(outputRoot, filepath.FromSlash(relative))
		if err := writeFileAtomically(path, outputs[relative]); err != nil {
			return fmt.Errorf("write generated %s: %w", relative, err)
		}
	}
	return nil
}

func sortedOutputPaths(outputs map[string][]byte) []string {
	paths := make([]string, 0, len(outputs))
	for path := range outputs {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func writeFileAtomically(path string, content []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".pm-api-generate-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
