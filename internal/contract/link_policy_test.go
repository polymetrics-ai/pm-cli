package contract

import (
	"encoding/json"
	"slices"
	"testing"
)

// linkPolicyOperation builds the smallest executable contract that declares the
// owner-approved link continuation policy: a query navigation field, a declared
// page size, a canonical repository route and a record identity field.
func linkPolicyOperation(t *testing.T) Operation {
	t.Helper()
	operation := decodeOperationForTest(t, validOperationDocumentForTest())
	record := Schema{
		Type:     SchemaTypeObject,
		Required: []string{"id", "repository_url"},
		Properties: map[string]Schema{
			"id":             {Type: SchemaTypeInteger},
			"repository_url": {Type: SchemaTypeString},
		},
	}
	owner := operation.Inputs[0]
	repo := owner
	repo.FieldID = "repo"
	repo.WireName = "repo"
	repo.JSONPointer = "/path/repo"
	page := owner
	page.FieldID = "query.page"
	page.WireName = "page"
	page.Location = FieldLocationQuery
	page.JSONPointer = "/query/page"
	page.Required = false
	page.Style = "form"
	page.Explode = true
	page.Schema = Schema{Type: SchemaTypeInteger}
	perPage := page
	perPage.FieldID = "query.per-page"
	perPage.WireName = "per_page"
	perPage.JSONPointer = "/query/per_page"
	operation.Transport = Transport{Kind: TransportKindHTTP, Method: "GET", Path: "/repos/{owner}/{repo}/issues"}
	operation.Inputs = []FieldBinding{owner, repo, page, perPage}
	operation.Responses = []Response{{
		Status: "200", MediaType: "application/json",
		Schema:     &Schema{Type: SchemaTypeArray, Items: &record},
		SourceRefs: []SourceRef{{SourceID: "github-rest-api", Pointer: "#/responses/200"}},
	}}
	operation.Execution = &OperationExecution{
		HTTP: &OperationHTTP{
			Origins: []string{"https://api.github.com"},
			Witness: ExecutionWitness{
				ProofID:    "github-http-proof",
				SourceRefs: []SourceRef{{SourceID: "github-rest-api", Pointer: "#/servers/0"}},
			},
		},
		Pagination: &OperationPagination{
			Kind:                      OperationPaginationLink,
			LinkRequestField:          "query.page",
			LinkResponseHeader:        "Link",
			LimitRequestField:         "query.per-page",
			LinkRoutes:                []string{"/repositories/{repository_id}/issues"},
			LinkRecordIdentityPointer: "/repository_url",
			Witness: ExecutionWitness{
				ProofID:    "github-link-proof",
				SourceRefs: []SourceRef{{SourceID: "github-rest-api", Pointer: "#/responses/200/headers/Link"}},
			},
		},
	}
	if err := operation.Validate(supportedFoundationsForTest()); err != nil {
		t.Fatalf("declared link policy does not validate: %v", err)
	}
	return operation
}

func TestLinkPaginationPolicyValidation(t *testing.T) {
	operation := linkPolicyOperation(t)
	document, err := json.Marshal(operation)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeOperation(document, supportedFoundationsForTest())
	if err != nil {
		t.Fatalf("DecodeOperation(link policy): %v", err)
	}
	pagination := decoded.Execution.Pagination
	if pagination.Kind != OperationPaginationLink ||
		!slices.Equal(pagination.LinkRoutes, []string{"/repositories/{repository_id}/issues"}) ||
		pagination.LinkRecordIdentityPointer != "/repository_url" {
		t.Fatalf("decoded link policy = %#v", pagination)
	}
	if err := pagination.Validate(decoded); err != nil {
		t.Fatalf("decoded link policy does not validate: %v", err)
	}

	for _, testCase := range []struct {
		name   string
		mutate func(*OperationPagination)
	}{
		{name: "route that repeats the operation path", mutate: func(p *OperationPagination) {
			p.LinkRoutes = []string{"/repos/{owner}/{repo}/issues"}
		}},
		{name: "route with a query", mutate: func(p *OperationPagination) {
			p.LinkRoutes = []string{"/repositories/{id}/issues?state=open"}
		}},
		{name: "duplicated route", mutate: func(p *OperationPagination) {
			p.LinkRoutes = []string{"/repositories/{id}/issues", "/repositories/{id}/issues"}
		}},
		{name: "route widening without an identity pointer", mutate: func(p *OperationPagination) {
			p.LinkRecordIdentityPointer = ""
		}},
		{name: "identity pointer that is not a record field", mutate: func(p *OperationPagination) {
			p.LinkRecordIdentityPointer = "/repository"
		}},
		{name: "identity pointer that is not a pointer", mutate: func(p *OperationPagination) {
			p.LinkRecordIdentityPointer = "repository_url"
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			candidate := *pagination
			candidate.LinkRoutes = slices.Clone(pagination.LinkRoutes)
			testCase.mutate(&candidate)
			if err := candidate.Validate(operation); err == nil {
				t.Fatal("malformed link policy was admitted")
			}
		})
	}

	cursor := linkPolicyOperation(t)
	cursor.Execution.Pagination = &OperationPagination{
		Kind:                  OperationPaginationCursor,
		CursorRequestField:    "query.page",
		CursorResponsePointer: "/id",
		LinkRoutes:            []string{"/repositories/{id}/issues"},
		Witness:               cursor.Execution.Pagination.Witness,
	}
	if err := cursor.Execution.Pagination.Validate(cursor); err == nil {
		t.Fatal("cursor pagination accepted a link route policy")
	}
}

func TestLinkRouteMatching(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		path     string
		template string
		want     bool
	}{
		{name: "literal route", path: "/repositories/issues", template: "/repositories/issues", want: true},
		{name: "placeholder route", path: "/repositories/1300192/issues", template: "/repositories/{repository_id}/issues", want: true},
		{name: "placeholder must be nonempty", path: "/repositories//issues", template: "/repositories/{repository_id}/issues", want: false},
		{name: "literal mismatch", path: "/repositories/1300192/comments", template: "/repositories/{repository_id}/issues", want: false},
		{name: "segment count mismatch", path: "/repositories/1300192/issues/1", template: "/repositories/{repository_id}/issues", want: false},
		{name: "encoded slash cannot widen a placeholder", path: "/repositories/13%2F00192/issues", template: "/repositories/{repository_id}/issues", want: false},
		{name: "encoded backslash cannot widen a placeholder", path: "/repositories/13%5C00192/issues", template: "/repositories/{repository_id}/issues", want: false},
		{name: "query cannot appear in a matched path", path: "/repositories/1300192/issues?state=open", template: "/repositories/{repository_id}/issues", want: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := LinkRouteMatches(testCase.path, testCase.template); got != testCase.want {
				t.Fatalf("LinkRouteMatches(%q, %q) = %t; want %t", testCase.path, testCase.template, got, testCase.want)
			}
		})
	}
}
