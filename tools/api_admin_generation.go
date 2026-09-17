package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"sort"
	"strings"

	"github.com/oapi-codegen/oapi-codegen/v2/pkg/codegen"
	"go.yaml.in/yaml/v3"
)

// The ordinary document remains the only input to ordinary models and clients.
// Private declarations join it only for the server projection, where shared
// problem references resolve locally without another schema or network loader.
type adminAPIComposition struct {
	serverSource  []byte
	operationIDs  []string
	publicSchemas []string
}

func composeAdminAPI(publicNode, adminNode *yaml.Node) (adminAPIComposition, error) {
	var ordinary, private map[string]any
	if err := publicNode.Decode(&ordinary); err != nil {
		return adminAPIComposition{}, err
	}
	if err := adminNode.Decode(&private); err != nil {
		return adminAPIComposition{}, err
	}
	if private["openapi"] != "3.1.0" || private["jsonSchemaDialect"] != "https://json-schema.org/draft/2020-12/schema" {
		return adminAPIComposition{}, errors.New("private API must use the accepted OpenAPI version and JSON Schema dialect")
	}
	for field := range private {
		switch field {
		case "openapi", "jsonSchemaDialect", "info", "paths", "components":
		default:
			return adminAPIComposition{}, fmt.Errorf("unknown private API source field %q", field)
		}
	}
	paths, ok := private["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		return adminAPIComposition{}, errors.New("private API requires explicit admin routes")
	}
	publicPaths, ok := ordinary["paths"].(map[string]any)
	if !ok {
		return adminAPIComposition{}, errors.New("ordinary API paths are missing")
	}
	var composition adminAPIComposition
	for path, raw := range paths {
		if !strings.HasPrefix(path, "/admin/v1/") {
			return adminAPIComposition{}, errors.New("private API routes must remain under /admin/v1")
		}
		if _, duplicate := publicPaths[path]; duplicate {
			return adminAPIComposition{}, errors.New("private API route collides with ordinary API")
		}
		item, ok := raw.(map[string]any)
		if !ok {
			return adminAPIComposition{}, errors.New("private API path must be an object")
		}
		for method, value := range item {
			if method != "post" {
				return adminAPIComposition{}, errors.New("private credential ingress admits only explicit write-only POST operations")
			}
			operation, ok := value.(map[string]any)
			if !ok {
				return adminAPIComposition{}, errors.New("private API operation must be an object")
			}
			id, _ := operation["operationId"].(string)
			if !strings.HasPrefix(id, "admin") || len(id) <= len("admin") {
				return adminAPIComposition{}, errors.New("private operation IDs require the admin namespace")
			}
			composition.operationIDs = append(composition.operationIDs, id)
		}
		publicPaths[path] = raw
	}
	publicComponents, ok := ordinary["components"].(map[string]any)
	if !ok {
		return adminAPIComposition{}, errors.New("ordinary API components are missing")
	}
	publicSchemas, ok := publicComponents["schemas"].(map[string]any)
	if !ok {
		return adminAPIComposition{}, errors.New("ordinary API schemas are missing")
	}
	for name := range publicSchemas {
		composition.publicSchemas = append(composition.publicSchemas, name)
	}
	privateComponents, ok := private["components"].(map[string]any)
	if !ok {
		return adminAPIComposition{}, errors.New("private API components are missing")
	}
	for kind, raw := range privateComponents {
		if kind != "schemas" && kind != "securitySchemes" {
			return adminAPIComposition{}, errors.New("private API may declare only private schemas and authentication schemes")
		}
		members, ok := raw.(map[string]any)
		if !ok {
			return adminAPIComposition{}, errors.New("private API component category must be an object")
		}
		target, _ := publicComponents[kind].(map[string]any)
		if target == nil {
			target = make(map[string]any)
			publicComponents[kind] = target
		}
		for name, value := range members {
			if !strings.HasPrefix(name, "Admin") {
				return adminAPIComposition{}, errors.New("private API components require the Admin namespace")
			}
			if _, duplicate := target[name]; duplicate {
				return adminAPIComposition{}, errors.New("private API component collides with ordinary API")
			}
			target[name] = value
		}
	}
	sort.Strings(composition.operationIDs)
	sort.Strings(composition.publicSchemas)
	if err := requireUniqueNonempty("private operation IDs", composition.operationIDs); err != nil {
		return adminAPIComposition{}, err
	}
	var err error
	composition.serverSource, err = json.Marshal(ordinary)
	return composition, err
}

func generateAdminModels(ctx context.Context, composition adminAPIComposition, base codegen.Configuration, publicModels *ast.File) ([]byte, error) {
	document, err := loadOpenAPIDocument(ctx, composition.serverSource)
	if err != nil {
		return nil, err
	}
	base.PackageName = "httpapi"
	base.Generate = codegen.GenerateOptions{Models: true}
	base.OutputOptions.SkipPrune = false
	base.OutputOptions.IncludeOperationIDs = composition.operationIDs
	base.OutputOptions.ExcludeSchemas = composition.publicSchemas
	source, err := codegen.Generate(document, base)
	if err != nil {
		return nil, fmt.Errorf("generate isolated admin models: %w", err)
	}
	var additional strings.Builder
	// Public parameter and response aliases can survive the generator's schema
	// exclusion. They already exist through the single canonical DTO projection;
	// retain only private declarations, without rewriting any references.
	shared := make(map[string]struct{})
	for _, declaration := range publicModels.Decls {
		if generic, ok := declaration.(*ast.GenDecl); ok && generic.Tok == token.TYPE {
			for _, specification := range generic.Specs {
				shared[specification.(*ast.TypeSpec).Name.Name] = struct{}{}
			}
		}
	}
	fileSet := token.NewFileSet()
	privateModels, err := parser.ParseFile(fileSet, "admin.gen.go", source, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	declarations := privateModels.Decls[:0]
	for _, declaration := range privateModels.Decls {
		if generic, ok := declaration.(*ast.GenDecl); ok && generic.Tok == token.TYPE {
			specifications := generic.Specs[:0]
			for _, specification := range generic.Specs {
				if _, public := shared[specification.(*ast.TypeSpec).Name.Name]; !public {
					specifications = append(specifications, specification)
				}
			}
			generic.Specs = specifications
			if len(specifications) == 0 {
				continue
			}
		}
		declarations = append(declarations, declaration)
	}
	privateModels.Decls = declarations
	if err := format.Node(&additional, fileSet, privateModels); err != nil {
		return nil, err
	}
	input := document.Components.Schemas["AdminCredentialInput"]
	if input == nil || input.Value == nil {
		return nil, errors.New("private credential ingress schema is missing")
	}
	objects := make(map[string]generatedObjectControl)
	collectSchemaObjectControls(input, "", objects, make(map[string]bool))
	root, ok := objects[""]
	if !ok {
		return nil, errors.New("private credential ingress must declare an exact root object")
	}
	writeStringSlice(&additional, "adminCredentialAllowedFields", root.Allowed)
	writeStringSlice(&additional, "adminCredentialRequiredFields", root.Required)
	additional.WriteString("\n// Sensitive ingress DTOs never expose their value through diagnostic formatting.\n")
	additional.WriteString("func (AdminCredentialInput) String() string { return \"admin credential input [REDACTED]\" }\n")
	additional.WriteString("func (AdminCredentialInput) GoString() string { return \"admin credential input [REDACTED]\" }\n")
	additional.WriteString("\nfunc isAdminAPIRoute(method, path string) bool {\n switch method + \" \" + path {\n")
	for _, route := range publicOperationRoutes(document) {
		if strings.HasPrefix(route.Path, "/admin/v1/") {
			fmt.Fprintf(&additional, "case %q: return true\n", route.Method+" "+ginRoutePath(route.Path))
		}
	}
	additional.WriteString("default: return false\n }\n}\n")
	return format.Source([]byte(additional.String()))
}
