package compile

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// TestLinkRoutePolicyReachesTheBundle proves the owner-approved continuation
// policy compiles end to end: the additional route templates and the declared
// record identity pointer survive normalization, the admitted copy never
// aliases the authoring document, and the emitted contract carries them.
func TestLinkRoutePolicyReachesTheBundle(t *testing.T) {
	document := []byte(`{
  "openapi": "3.0.3",
  "info": {"title": "Link route fixture", "version": "1.0.0"},
  "servers": [{"url": "https://api.example.test/v3"}],
  "paths": {
    "/repos/{owner}/{repo}/issues": {
      "get": {
        "operationId": "listIssues",
        "parameters": [
          {"name": "owner", "in": "path", "required": true, "schema": {"type": "string"}},
          {"name": "repo", "in": "path", "required": true, "schema": {"type": "string"}},
          {"name": "page", "in": "query", "schema": {"type": "integer"}},
          {"name": "per_page", "in": "query", "schema": {"type": "integer"}}
        ],
        "responses": {
          "200": {
            "description": "page",
            "headers": {
              "Link": {"schema": {"type": "string"}}
            },
            "content": {
              "application/json": {
                "schema": {
                  "type": "array",
                  "items": {
                    "type": "object",
                    "additionalProperties": false,
                    "required": ["id", "repository_url"],
                    "properties": {
                      "id": {"type": "integer"},
                      "repository_url": {"type": "string", "format": "uri"}
                    }
                  }
                }
              }
            }
          }
        }
      }
    }
  }
}`)
	sourceOperation := testOperation(
		"listIssues",
		"root",
		"#/paths/~1repos~1{owner}~1{repo}~1issues/get",
		nil,
		[]VariantRef{{
			Pointer:   "#/paths/~1repos~1{owner}~1{repo}~1issues/get/responses/200/content/application~1json",
			Status:    "200",
			MediaType: "application/json",
		}},
	)
	lock, captures := testSourceInput(
		t,
		DialectOpenAPI30,
		map[string][]byte{"root": document},
		nil,
		[]SourceOperation{sourceOperation},
	)
	captured, err := CaptureSource(lock, captures)
	if err != nil {
		t.Fatal(err)
	}
	resolved := testResolve(t, captured)
	loaded, err := LoadDialects(context.Background(), resolved)
	if err != nil {
		t.Fatal(err)
	}
	normalization, err := Normalize(context.Background(), loaded)
	if err != nil {
		t.Fatal(err)
	}
	normalizedOperation := &normalization.Operations[0]
	var pageFieldID, limitFieldID string
	for _, binding := range normalizedOperation.CommonBindings {
		if binding.Normalized == nil {
			continue
		}
		switch binding.Normalized.WireName {
		case "page":
			pageFieldID = binding.FieldID
		case "per_page":
			limitFieldID = binding.FieldID
		}
	}
	if pageFieldID == "" || limitFieldID == "" {
		t.Fatal("source navigation bindings were not admitted")
	}
	linkSourceRefs := []contract.SourceRef{{
		SourceID: "root",
		Pointer:  "#/paths/~1repos~1{owner}~1{repo}~1issues/get/responses/200/headers/Link",
	}}
	semantics := SemanticsDocument{
		SchemaVersion: SemanticsSchemaVersion,
		ConnectorID:   lock.ConnectorID,
		SourceDigest:  captured.Digest,
		Revision:      lock.SemanticsRevision,
		Operations: []OperationSemantics{{
			OperationID: "listIssues",
			SourceRefs:  []contract.SourceRef{{SourceID: "root", Pointer: "#/paths/~1repos~1{owner}~1{repo}~1issues/get"}},
			Pagination: &Pagination{
				Kind:                      string(contract.OperationPaginationLink),
				LinkRequestField:          pageFieldID,
				LinkResponseHeader:        "Link",
				LimitRequestField:         limitFieldID,
				LinkRoutes:                []string{"/repositories/{repository_id}/issues"},
				LinkRecordIdentityPointer: "/repository_url",
				Witness:                   Witness{ProofID: "source-reviewed-link", SourceRefs: linkSourceRefs},
			},
		}},
	}
	if err := ApplyAmendments(normalization, resolved, &semantics); err != nil {
		t.Fatal(err)
	}
	// The admitted copy must not alias the authoring document.
	semantics.Operations[0].Pagination.LinkRoutes[0] = "/mutated/{repository_id}/issues"
	semantics.Operations[0].Pagination.LinkRecordIdentityPointer = "/mutated"
	admitted := normalizedOperation.Semantics
	if admitted == nil || admitted.Pagination == nil ||
		len(admitted.Pagination.LinkRoutes) != 1 ||
		admitted.Pagination.LinkRoutes[0] != "/repositories/{repository_id}/issues" ||
		admitted.Pagination.LinkRecordIdentityPointer != "/repository_url" {
		t.Fatalf("admitted link policy aliases authoring input: %#v", admitted)
	}
	admitted.Pagination.LinkRoutes[0] = "/mutated/{repository_id}/issues"
	if semantics.Operations[0].Pagination.LinkRoutes[0] != "/mutated/{repository_id}/issues" {
		t.Fatal("mutating the admitted policy changed an unrelated value")
	}
	admitted.Pagination.LinkRoutes[0] = "/repositories/{repository_id}/issues"
	target := HTTPCompilationTargetFoundations()
	if err := CheckRequirements(normalization, resolved, target); err != nil {
		t.Fatal(err)
	}
	detached, err := emitOperation(*normalizedOperation, target)
	if err != nil {
		t.Fatalf("emit admitted operation: %v; diagnostics: %#v", err, normalization.Diagnostics)
	}
	if detached.Execution == nil || detached.Execution.Pagination == nil {
		t.Fatalf("emitted pagination is absent: %#v", detached.Execution)
	}
	pagination := detached.Execution.Pagination
	if pagination.Kind != contract.OperationPaginationLink ||
		!slices.Equal(pagination.LinkRoutes, []string{"/repositories/{repository_id}/issues"}) ||
		pagination.LinkRecordIdentityPointer != "/repository_url" {
		t.Fatalf("emitted link policy = %#v", pagination)
	}
	if err := pagination.Validate(detached); err != nil {
		t.Fatalf("emitted link policy does not validate: %v", err)
	}
}

// TestLinkPolicyDeclarationRefusals pins the shape rules of the new link
// policy: a route template that is not a clean absolute path, a duplicated
// route, an identity pointer that is not a record pointer, and route widening
// without an identity witness are all refused while decoding.
func TestLinkPolicyDeclarationRefusals(t *testing.T) {
	base := func(pagination string) string {
		return fmt.Sprintf(`{
  "schema_version": "pm.semantics/v1",
  "connector_id": "github",
  "source_digest": "sha256:%s",
  "revision": "sha256:%s",
  "operations": [{
    "operation_id": "issues/list-for-repo",
    "source_refs": [{"source_id": "github-rest-api-description", "pointer": "#/paths/~1repos~1{owner}~1{repo}~1issues/get"}],
    "pagination": %s,
    "lanes": []
  }]
}`, strings.Repeat("1", 64), strings.Repeat("2", 64), pagination)
	}
	witness := `{"proof_id": "github-list-for-repo-link-continuation", "source_refs": [{"source_id": "github-rest-api-description", "pointer": "#/paths/~1repos~1{owner}~1{repo}~1issues/get/responses/200/headers/Link"}]}`
	link := func(extra string) string {
		return fmt.Sprintf(`{"kind": "link", "link_request_field": "query.page", "link_response_header": "Link", "limit_request_field": "query.per-page"%s, "witness": %s}`, extra, witness)
	}
	for _, testCase := range []struct {
		name       string
		pagination string
	}{
		{name: "declared routes and identity", pagination: link(`, "link_routes": ["/repositories/{repository_id}/issues"], "link_record_identity_pointer": "/repository_url"`)},
		{name: "route with a query", pagination: link(`, "link_routes": ["/repositories/{id}/issues?state=open"], "link_record_identity_pointer": "/repository_url"`)},
		{name: "route with an empty segment", pagination: link(`, "link_routes": ["/repositories//issues"], "link_record_identity_pointer": "/repository_url"`)},
		{name: "route with a relative segment", pagination: link(`, "link_routes": ["/repositories/../issues"], "link_record_identity_pointer": "/repository_url"`)},
		{name: "route with an escaped literal", pagination: link(`, "link_routes": ["/repositories/%32/issues"], "link_record_identity_pointer": "/repository_url"`)},
		{name: "route with a partial placeholder", pagination: link(`, "link_routes": ["/repositories/x{id}/issues"], "link_record_identity_pointer": "/repository_url"`)},
		{name: "duplicated route", pagination: link(`, "link_routes": ["/repositories/{id}/issues", "/repositories/{id}/issues"], "link_record_identity_pointer": "/repository_url"`)},
		{name: "explicit empty routes", pagination: link(`, "link_routes": []`)},
		{name: "routes without an identity pointer", pagination: link(`, "link_routes": ["/repositories/{id}/issues"]`)},
		{name: "identity pointer that is not a pointer", pagination: link(`, "link_record_identity_pointer": "repository_url"`)},
		{name: "explicit empty identity pointer", pagination: link(`, "link_record_identity_pointer": ""`)},
		{name: "routes on a cursor kind", pagination: `{"kind": "cursor", "cursor_request_field": "query.cursor", "cursor_response_pointer": "/next", "link_routes": ["/repositories/{id}/issues"], "witness": ` + witness + `}`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			document := []byte(base(testCase.pagination))
			_, err := DecodeSemantics(document)
			valid := testCase.name == "declared routes and identity"
			if valid && err != nil {
				t.Fatalf("valid link policy was refused: %v", err)
			}
			if !valid && err == nil {
				t.Fatal("malformed link policy was admitted")
			}
		})
	}
}
