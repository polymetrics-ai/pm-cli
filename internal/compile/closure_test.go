package compile

// This file owns the focused compiler checks for the two shapes that previously
// blocked the pinned GitHub response closure: objects that the source declares
// open (additionalProperties absent, true, or a declared schema) and anyOf
// unions. The checks are behavioural: declared openness keeps refusing nothing
// the source permits, closed objects keep rejecting undeclared properties, every
// union variant keeps its declared shape and identity, and a genuinely unknown
// keyword keeps blocking only its own operation.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// githubShapedClosureDocument is a small GitHub-shaped source. It declares a
// record array of open issue objects reaching a free-form object, a typed
// additional-property map, closed objects, oneOf and anyOf unions, nested
// requiredness and an exact integer outside the float64 range.
const githubShapedClosureDocument = `{
  "openapi": "3.0.3",
  "info": {"title": "GitHub-shaped closure fixture", "version": "1.0.0"},
  "servers": [{"url": "https://api.github.example.test"}],
  "paths": {
    "/repos/{owner}/{repo}/issues": {
      "get": {
        "operationId": "issues/list-for-repo",
        "parameters": [
          {"name": "owner", "in": "path", "required": true, "schema": {"type": "string"}},
          {"name": "repo", "in": "path", "required": true, "schema": {"type": "string"}},
          {"name": "per_page", "in": "query", "schema": {"type": "integer", "default": 30}}
        ],
        "responses": {
          "200": {
            "description": "Response",
            "content": {"application/json": {"schema": {"type": "array", "items": {"$ref": "#/components/schemas/issue"}}}}
          },
          "404": {
            "description": "Resource not found",
            "content": {"application/json": {"schema": {"$ref": "#/components/schemas/basic-error"}}}
          }
        }
      }
    }
  },
  "components": {
    "schemas": {
      "issue": {
        "type": "object",
        "required": ["id", "number", "title", "labels", "milestone", "field"],
        "properties": {
          "id": {"type": "integer", "format": "int64"},
          "number": {"type": "integer", "enum": [9007199254740993]},
          "title": {"type": "string"},
          "labels": {"type": "array", "items": {"$ref": "#/components/schemas/label"}},
          "milestone": {"$ref": "#/components/schemas/milestone"},
          "field": {"$ref": "#/components/schemas/issue-field-value"},
          "permissions": {"$ref": "#/components/schemas/permissions"},
          "metadata": {"$ref": "#/components/schemas/metadata"},
          "closed_meta": {"$ref": "#/components/schemas/closed-meta"}
        }
      },
      "label": {
        "oneOf": [
          {"type": "string"},
          {
            "type": "object",
            "required": ["id", "name"],
            "properties": {"id": {"type": "integer"}, "name": {"type": "string"}}
          }
        ]
      },
      "milestone": {
        "type": "object",
        "nullable": true,
        "required": ["title"],
        "properties": {
          "title": {"type": "string"},
          "creator": {
            "type": "object",
            "required": ["login"],
            "properties": {"login": {"type": "string"}}
          }
        }
      },
      "issue-field-value": {
        "type": "object",
        "required": ["issue_field_id", "value"],
        "properties": {
          "issue_field_id": {"type": "integer"},
          "value": {
            "anyOf": [{"type": "string"}, {"type": "integer"}],
            "nullable": true
          }
        }
      },
      "permissions": {
        "type": "object",
        "properties": {"issues": {"type": "string"}, "checks": {"type": "string"}},
        "additionalProperties": {"type": "string"}
      },
      "metadata": {"type": "object"},
      "closed-meta": {
        "type": "object",
        "additionalProperties": false,
        "required": ["state"],
        "properties": {"state": {"type": "string"}}
      },
      "basic-error": {
        "type": "object",
        "properties": {"message": {"type": "string"}, "documentation_url": {"type": "string"}}
      }
    }
  }
}`

func TestGitHubShapedClosureLowersFaithfully(t *testing.T) {
	lock, captures := testSourceInput(t, DialectOpenAPI30, map[string][]byte{
		"root": []byte(githubShapedClosureDocument),
	}, nil, []SourceOperation{
		testOperation("issues/list-for-repo", "root", "#/paths/~1repos~1{owner}~1{repo}~1issues/get", nil, []VariantRef{
			{Pointer: "#/paths/~1repos~1{owner}~1{repo}~1issues/get/responses/200/content/application~1json", Status: "200", MediaType: "application/json"},
			{Pointer: "#/paths/~1repos~1{owner}~1{repo}~1issues/get/responses/404/content/application~1json", Status: "404", MediaType: "application/json"},
		}),
	})
	captured, err := CaptureSource(lock, captures)
	if err != nil {
		t.Fatal(err)
	}
	operationRef := []SourceRef{{SourceID: "root", Pointer: "#/paths/~1repos~1{owner}~1{repo}~1issues/get"}}
	semantics, err := json.Marshal(SemanticsDocument{
		SchemaVersion: SemanticsSchemaVersion,
		ConnectorID:   lock.ConnectorID,
		SourceDigest:  captured.Digest,
		Revision:      lock.SemanticsRevision,
		Operations: []OperationSemantics{{
			OperationID: "issues/list-for-repo",
			SourceRefs:  operationRef,
			Lanes: []LaneSemantics{
				{Lane: contract.LaneDirectRead, Classification: LaneEligible, SourceRefs: operationRef},
				{Lane: contract.LaneDirectWrite, Classification: LaneNotApplicable, Reason: "read operation has no write shape", SourceRefs: operationRef},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	lockDocument, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	input := CompilationInput{
		SourceLock: lockDocument, Captures: captures, Semantics: semantics,
		TargetFoundations: HTTPCompilationTargetFoundations(),
	}
	first, err := Compile(context.Background(), input)
	if err != nil {
		t.Fatalf("GitHub-shaped closure compile: %v", err)
	}
	second, err := Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bundle, second.Bundle) {
		t.Fatal("identical GitHub-shaped closure inputs produced different bundle bytes")
	}
	if !bytes.Equal(first.Report, second.Report) {
		t.Fatal("identical GitHub-shaped closure inputs produced different report bytes")
	}
	if !bytes.Equal(first.CommandManifest, second.CommandManifest) {
		t.Fatal("identical GitHub-shaped closure inputs produced different command manifest bytes")
	}
	var report CompileReport
	if err := json.Unmarshal(first.Report, &report); err != nil {
		t.Fatal(err)
	}
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Severity == SeverityError {
			t.Fatalf("GitHub-shaped closure produced a blocking diagnostic: %#v", diagnostic)
		}
	}
	var bundle Bundle
	if err := json.Unmarshal(first.Bundle, &bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle.UnavailableOperations) != 0 {
		t.Fatalf("GitHub-shaped closure stayed unavailable: %#v", bundle.UnavailableOperations)
	}
	if len(bundle.Operations) != 1 || bundle.Operations[0].OperationID != "issues/list-for-repo" {
		t.Fatalf("executable operations = %#v", bundle.Operations)
	}
	operation := bundle.Operations[0]
	if status := emittedLaneStatus(t, operation, contract.LaneDirectRead); status != contract.LaneEligible {
		t.Fatalf("direct_read status = %q, want eligible", status)
	}
	if status := emittedLaneStatus(t, operation, contract.LaneDirectWrite); status != contract.LaneNotApplicable {
		t.Fatalf("direct_write status = %q, want not_applicable", status)
	}

	recordArray := emittedResponse(t, operation, "200").Schema
	if recordArray == nil || recordArray.Type != contract.SchemaTypeArray || recordArray.Items == nil {
		t.Fatalf("200 response record array = %#v", recordArray)
	}
	issue := *recordArray.Items
	if issue.Type != contract.SchemaTypeObject || !issue.OpenProperties {
		t.Fatalf("issue object lowering = %#v", issue)
	}
	if !slices.Equal(issue.Required, []string{"id", "number", "title", "labels", "milestone", "field"}) {
		t.Fatalf("issue required list = %v", issue.Required)
	}

	t.Run("declared-open objects keep the declared property set", func(t *testing.T) {
		if len(issue.Properties) != 9 {
			t.Fatalf("issue properties = %#v", issue.Properties)
		}
		metadata := issue.Properties["metadata"]
		if metadata.Type != contract.SchemaTypeObject || !metadata.OpenProperties ||
			len(metadata.Properties) != 0 || metadata.Required != nil || metadata.AdditionalProperties != nil {
			t.Fatalf("free-form object lowering = %#v", metadata)
		}
		value := contract.NewObjectValue(map[string]contract.PresentValue{"anything": contract.NewStringValue("x")})
		if err := contract.ValidateValueAgainstSchema(value, metadata); err != nil {
			t.Fatalf("declared-open object refused a property the source permits: %v", err)
		}
		permissions := issue.Properties["permissions"]
		if !permissions.OpenProperties || permissions.AdditionalProperties == nil ||
			permissions.AdditionalProperties.Type != contract.SchemaTypeString || len(permissions.Properties) != 2 {
			t.Fatalf("typed declared-open object lowering = %#v", permissions)
		}
		err := contract.ValidateValueAgainstSchema(contract.NewObjectValue(map[string]contract.PresentValue{
			"issues":      contract.NewStringValue("read"),
			"deployments": contract.NewStringValue("write"),
		}), permissions)
		if err != nil {
			t.Fatalf("typed declared-open object refused a property the source permits: %v", err)
		}
		unpermitted, err := contract.NewIntegerValue("7")
		if err != nil {
			t.Fatal(err)
		}
		err = contract.ValidateValueAgainstSchema(contract.NewObjectValue(map[string]contract.PresentValue{
			"deployments": unpermitted,
		}), permissions)
		if !errors.Is(err, contract.ErrInvalidValue) {
			t.Fatalf("typed declared-open object accepted a value outside the declared additional property schema: %v", err)
		}
		notFound := emittedResponse(t, operation, "404").Schema
		if notFound == nil || !notFound.OpenProperties || len(notFound.Properties) != 2 {
			t.Fatalf("basic-error object lowering = %#v", notFound)
		}
	})

	t.Run("closed objects still reject undeclared properties", func(t *testing.T) {
		closed := issue.Properties["closed_meta"]
		if closed.Type != contract.SchemaTypeObject || closed.OpenProperties || closed.AdditionalProperties != nil {
			t.Fatalf("closed object lowering = %#v", closed)
		}
		if !slices.Equal(closed.Required, []string{"state"}) {
			t.Fatalf("closed object required list = %v", closed.Required)
		}
		err := contract.ValidateValueAgainstSchema(contract.NewObjectValue(map[string]contract.PresentValue{
			"state": contract.NewStringValue("open"),
			"extra": contract.NewStringValue("x"),
		}), closed)
		if !errors.Is(err, contract.ErrInvalidValue) {
			t.Fatalf("closed object accepted an undeclared property: %v", err)
		}
		declared := contract.NewObjectValue(map[string]contract.PresentValue{"state": contract.NewStringValue("open")})
		if err := contract.ValidateValueAgainstSchema(declared, closed); err != nil {
			t.Fatalf("closed object refused its declared property: %v", err)
		}
	})

	t.Run("anyOf and oneOf keep every declared variant", func(t *testing.T) {
		anyOf := issue.Properties["field"].Properties["value"]
		if anyOf.Type != contract.SchemaTypeUnion || !anyOf.Nullable || len(anyOf.OneOf) != 2 ||
			anyOf.OneOf[0].Type != contract.SchemaTypeString || anyOf.OneOf[1].Type != contract.SchemaTypeInteger {
			t.Fatalf("anyOf union lowering = %#v", anyOf)
		}
		integerMember, err := contract.NewIntegerValue("9007199254740993")
		if err != nil {
			t.Fatal(err)
		}
		integerVariant, err := contract.NewUnionValue("variant-1", integerMember)
		if err != nil {
			t.Fatal(err)
		}
		if err := contract.ValidateValueAgainstSchema(integerVariant, anyOf); err != nil {
			t.Fatalf("declared anyOf variant refused its own value: %v", err)
		}
		stringVariant, err := contract.NewUnionValue("variant-0", contract.NewStringValue("sample"))
		if err != nil {
			t.Fatal(err)
		}
		if err := contract.ValidateValueAgainstSchema(stringVariant, anyOf); err != nil {
			t.Fatalf("declared anyOf string variant refused its own value: %v", err)
		}
		decimalMember, err := contract.NewDecimalValue("0.5", 2, 1)
		if err != nil {
			t.Fatal(err)
		}
		widened, err := contract.NewUnionValue("variant-1", decimalMember)
		if err != nil {
			t.Fatal(err)
		}
		if err := contract.ValidateValueAgainstSchema(widened, anyOf); !errors.Is(err, contract.ErrInvalidValue) {
			t.Fatalf("anyOf accepted a member outside every declared variant: %v", err)
		}
		if err := contract.ValidateValueAgainstSchema(contract.NullValue(), anyOf); err != nil {
			t.Fatalf("nullable anyOf refused null: %v", err)
		}

		labels := issue.Properties["labels"]
		if labels.Type != contract.SchemaTypeArray || labels.Items == nil ||
			labels.Items.Type != contract.SchemaTypeUnion || len(labels.Items.OneOf) != 2 ||
			labels.Items.OneOf[0].Type != contract.SchemaTypeString {
			t.Fatalf("oneOf union lowering = %#v", labels.Items)
		}
		labelObject := labels.Items.OneOf[1]
		if labelObject.Type != contract.SchemaTypeObject || !labelObject.OpenProperties ||
			!slices.Equal(labelObject.Required, []string{"id", "name"}) {
			t.Fatalf("oneOf object variant lowering = %#v", labelObject)
		}
	})

	t.Run("requiredness, nullability and exact integers survive lowering", func(t *testing.T) {
		milestone := issue.Properties["milestone"]
		if !milestone.Nullable || !slices.Equal(milestone.Required, []string{"title"}) {
			t.Fatalf("nullable object lowering = %#v", milestone)
		}
		if !slices.Equal(milestone.Properties["creator"].Required, []string{"login"}) {
			t.Fatalf("nested required list = %v", milestone.Properties["creator"].Required)
		}
		number := issue.Properties["number"]
		exact, err := contract.NewIntegerValue("9007199254740993")
		if err != nil {
			t.Fatal(err)
		}
		if number.Type != contract.SchemaTypeInteger || len(number.Enum) != 1 || !number.Enum[0].Equal(exact) {
			t.Fatalf("exact integer enum lowering = %#v", number)
		}
	})
}

func TestUnknownSchemaKeywordStillBlocksOnlyItsOperation(t *testing.T) {
	document := []byte(`{
  "openapi": "3.0.3",
  "info": {"title": "Unknown keyword isolation", "version": "1.0.0"},
  "servers": [{"url": "https://api.example.test/v3"}],
  "paths": {
    "/issues": {
      "get": {
        "operationId": "listIssues",
        "responses": {
          "200": {
            "description": "records",
            "content": {"application/json": {"schema": {"type": "array", "items": {"$ref": "#/components/schemas/issue"}}}}
          }
        }
      }
    },
    "/conditional": {
      "get": {
        "operationId": "listConditional",
        "responses": {
          "200": {
            "description": "unsupported conditional shape",
            "content": {"application/json": {"schema": {"type": "object", "properties": {"value": {"type": "string"}}, "not": {"required": ["value"]}}}}
          }
        }
      }
    }
  },
  "components": {
    "schemas": {
      "issue": {"type": "object", "required": ["id"], "properties": {"id": {"type": "integer"}}}
    }
  }
}`)
	operations := []SourceOperation{
		testOperation("listIssues", "root", "#/paths/~1issues/get", nil, []VariantRef{{
			Pointer: "#/paths/~1issues/get/responses/200/content/application~1json", Status: "200", MediaType: "application/json",
		}}),
		testOperation("listConditional", "root", "#/paths/~1conditional/get", nil, []VariantRef{{
			Pointer: "#/paths/~1conditional/get/responses/200/content/application~1json", Status: "200", MediaType: "application/json",
		}}),
	}
	lock, captures := testSourceInput(t, DialectOpenAPI30, map[string][]byte{"root": document}, nil, operations)
	captured, err := CaptureSource(lock, captures)
	if err != nil {
		t.Fatal(err)
	}
	issuesRef := []SourceRef{{SourceID: "root", Pointer: "#/paths/~1issues/get"}}
	conditionalRef := []SourceRef{{SourceID: "root", Pointer: "#/paths/~1conditional/get"}}
	semantics, err := json.Marshal(SemanticsDocument{
		SchemaVersion: SemanticsSchemaVersion,
		ConnectorID:   lock.ConnectorID,
		SourceDigest:  captured.Digest,
		Revision:      lock.SemanticsRevision,
		Operations: []OperationSemantics{
			{
				OperationID: "listIssues", SourceRefs: issuesRef,
				Lanes: []LaneSemantics{
					{Lane: contract.LaneDirectRead, Classification: LaneEligible, SourceRefs: issuesRef},
					{Lane: contract.LaneDirectWrite, Classification: LaneNotApplicable, Reason: "read operation has no write shape", SourceRefs: issuesRef},
				},
			},
			{
				OperationID: "listConditional", SourceRefs: conditionalRef,
				Lanes: []LaneSemantics{
					{Lane: contract.LaneDirectRead, Classification: LaneEligible, SourceRefs: conditionalRef},
					{Lane: contract.LaneDirectWrite, Classification: LaneNotApplicable, Reason: "read operation has no write shape", SourceRefs: conditionalRef},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	lockDocument, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := Compile(context.Background(), CompilationInput{
		SourceLock: lockDocument, Captures: captures, Semantics: semantics,
		TargetFoundations: HTTPCompilationTargetFoundations(),
	})
	if err != nil {
		t.Fatalf("unknown keyword isolation compile: %v", err)
	}
	var bundle Bundle
	if err := json.Unmarshal(artifacts.Bundle, &bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Operations) != 1 || bundle.Operations[0].OperationID != "listIssues" {
		t.Fatalf("unrelated supported operation was suppressed: %#v", bundle.Operations)
	}
	if status := emittedLaneStatus(t, bundle.Operations[0], contract.LaneDirectRead); status != contract.LaneEligible {
		t.Fatalf("unrelated direct_read status = %q, want eligible", status)
	}
	if len(bundle.UnavailableOperations) != 1 {
		t.Fatalf("unavailable operations = %#v", bundle.UnavailableOperations)
	}
	unavailable := bundle.UnavailableOperations[0]
	if unavailable.OperationID != "listConditional" ||
		!slices.Equal(unavailable.DiagnosticCodes, []string{"unsupported_schema_keyword"}) {
		t.Fatalf("unavailable metadata = %#v", unavailable)
	}
	for _, lane := range unavailable.LaneEligibility {
		if lane.Status == contract.LaneEligible {
			t.Fatalf("unavailable operation retained an executable lane: %#v", lane)
		}
		if lane.Lane == contract.LaneDirectWrite &&
			(lane.Status != contract.LaneNotApplicable || lane.Reason != "read operation has no write shape") {
			t.Fatalf("unrelated lane disposition changed: %#v", lane)
		}
	}
	var report CompileReport
	if err := json.Unmarshal(artifacts.Report, &report); err != nil {
		t.Fatal(err)
	}
	var blockedPointer string
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.OperationID != "listConditional" || diagnostic.Code != "unsupported_schema_keyword" {
			continue
		}
		if len(diagnostic.SourceFacts) != 1 {
			t.Fatalf("unknown keyword diagnostic facts = %#v", diagnostic.SourceFacts)
		}
		blockedPointer = diagnostic.SourceFacts[0].SourceRef.Pointer
	}
	if blockedPointer != "#/paths/~1conditional/get/responses/200/content/application~1json/schema" {
		t.Fatalf("unknown keyword diagnostic pointer = %q", blockedPointer)
	}
}

func TestPinnedGitHubResponseClosureIsDirectReadEligible(t *testing.T) {
	input, connectorID := pinnedConnectorInput(t, "github")
	if connectorID != "github" {
		t.Fatalf("pinned connector identity = %q", connectorID)
	}
	first, err := Compile(context.Background(), input)
	if err != nil {
		t.Fatalf("pinned github compile: %v", err)
	}
	second, err := Compile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bundle, second.Bundle) {
		t.Fatal("identical pinned github inputs produced different bundle bytes")
	}
	if !bytes.Equal(first.Report, second.Report) {
		t.Fatal("identical pinned github inputs produced different report bytes")
	}
	if !bytes.Equal(first.CommandManifest, second.CommandManifest) {
		t.Fatal("identical pinned github inputs produced different command manifest bytes")
	}
	var report CompileReport
	if err := json.Unmarshal(first.Report, &report); err != nil {
		t.Fatal(err)
	}
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Severity != SeverityWarning || diagnostic.Code != "unrepresented_schema_default" || len(diagnostic.SourceFacts) == 0 {
			t.Fatalf("pinned github closure diagnostic = %#v, want only source-linked unrepresented default warnings", diagnostic)
		}
	}
	var bundle Bundle
	if err := json.Unmarshal(first.Bundle, &bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle.UnavailableOperations) != 0 {
		t.Fatalf("pinned github operation stayed unavailable: %#v", bundle.UnavailableOperations)
	}
	if len(bundle.Operations) != 1 || bundle.Operations[0].OperationID != "issues/list-for-repo" {
		t.Fatalf("pinned github executable operations = %#v", bundle.Operations)
	}
	if len(bundle.Commands) != 1 || bundle.Commands[0].Name != "issues-list-for-repo" {
		t.Fatalf("pinned github commands = %#v", bundle.Commands)
	}
	if len(bundle.Commands[0].Fields) != 15 {
		t.Fatalf("pinned github command fields = %#v", bundle.Commands[0].Fields)
	}
	operation := bundle.Operations[0]
	wantLanes := []struct {
		lane   contract.Lane
		status contract.LaneStatus
	}{
		{contract.LaneDirectRead, contract.LaneEligible},
		{contract.LaneDirectWrite, contract.LaneNotApplicable},
		{contract.LaneETL, contract.LaneUnproven},
		{contract.LaneReverseETL, contract.LaneNotApplicable},
		{contract.LaneBinaryDownload, contract.LaneNotApplicable},
		{contract.LaneBinaryUpload, contract.LaneNotApplicable},
		{contract.LaneSyncTransport, contract.LaneUnproven},
	}
	if len(operation.LaneEligibility) != len(wantLanes) {
		t.Fatalf("pinned github lane inventory = %#v", operation.LaneEligibility)
	}
	for index, want := range wantLanes {
		if lane := operation.LaneEligibility[index]; lane.Lane != want.lane || lane.Status != want.status {
			t.Fatalf("pinned github lane %d = %q %q, want %q %q", index, lane.Lane, lane.Status, want.lane, want.status)
		}
	}
	statuses := make([]string, 0, len(operation.Responses))
	for _, response := range operation.Responses {
		statuses = append(statuses, response.Status)
	}
	if !slices.Equal(statuses, []string{"200", "301", "404", "422"}) {
		t.Fatalf("pinned github response statuses = %v", statuses)
	}
	issue := emittedResponse(t, operation, "200").Schema
	if issue == nil || issue.Type != contract.SchemaTypeArray || issue.Items == nil {
		t.Fatalf("pinned github 200 schema = %#v", issue)
	}
	record := *issue.Items
	if record.Type != contract.SchemaTypeObject || !record.OpenProperties {
		t.Fatalf("pinned github issue object = %#v", record)
	}
	wantRequired := []string{
		"assignee", "closed_at", "comments", "comments_url", "events_url", "html_url", "id", "node_id",
		"labels", "labels_url", "milestone", "number", "repository_url", "state", "locked", "title",
		"url", "user", "created_at", "updated_at",
	}
	if !slices.Equal(record.Required, wantRequired) {
		t.Fatalf("pinned github issue required list = %v", record.Required)
	}
	labels := record.Properties["labels"]
	if labels.Type != contract.SchemaTypeArray || labels.Items == nil || labels.Items.Type != contract.SchemaTypeUnion ||
		len(labels.Items.OneOf) != 2 || labels.Items.OneOf[0].Type != contract.SchemaTypeString ||
		labels.Items.OneOf[1].Type != contract.SchemaTypeObject || !labels.Items.OneOf[1].OpenProperties {
		t.Fatalf("pinned github label union = %#v", labels.Items)
	}
	pullRequest := record.Properties["pull_request"]
	if !slices.Equal(pullRequest.Required, []string{"diff_url", "html_url", "patch_url", "url"}) {
		t.Fatalf("pinned github pull_request required list = %v", pullRequest.Required)
	}
	milestone := record.Properties["milestone"]
	if !milestone.Nullable || len(milestone.Required) != 16 {
		t.Fatalf("pinned github nullable milestone = %#v", milestone)
	}
	fieldValue := record.Properties["issue_field_values"].Items.Properties["value"]
	if fieldValue.Type != contract.SchemaTypeUnion || !fieldValue.Nullable || len(fieldValue.OneOf) != 3 ||
		fieldValue.OneOf[0].Type != contract.SchemaTypeString || fieldValue.OneOf[1].Type != contract.SchemaTypeDecimal ||
		fieldValue.OneOf[2].Type != contract.SchemaTypeInteger {
		t.Fatalf("pinned github anyOf union = %#v", fieldValue)
	}
	integration := record.Properties["performed_via_github_app"]
	if !integration.Nullable || integration.Properties["owner"].Type != contract.SchemaTypeUnion ||
		len(integration.Properties["owner"].OneOf) != 2 ||
		!slices.Contains(integration.Properties["owner"].OneOf[0].Required, "login") ||
		!slices.Contains(integration.Properties["owner"].OneOf[1].Required, "slug") {
		t.Fatalf("pinned github integration union = %#v", integration.Properties["owner"])
	}
	permissions := integration.Properties["permissions"]
	if !permissions.OpenProperties || permissions.AdditionalProperties == nil ||
		permissions.AdditionalProperties.Type != contract.SchemaTypeString || len(permissions.Properties) != 5 {
		t.Fatalf("pinned github typed declared-open permissions = %#v", permissions)
	}
	if id := record.Properties["id"]; id.Type != contract.SchemaTypeInteger {
		t.Fatalf("pinned github integer fact = %#v", id)
	}
	perPage := emittedInput(t, operation, "query.per-page")
	exactThirty, err := contract.NewIntegerValue("30")
	if err != nil {
		t.Fatal(err)
	}
	if perPage.Required || perPage.SourceDefault == nil || !perPage.SourceDefault.Equal(exactThirty) {
		t.Fatalf("pinned github per-page binding = %#v", perPage)
	}
	direction := emittedInput(t, operation, "query.direction")
	if direction.SourceDefault == nil || !direction.SourceDefault.Equal(contract.NewStringValue("desc")) {
		t.Fatalf("pinned github direction binding = %#v", direction)
	}
	notFound := emittedResponse(t, operation, "404").Schema
	if notFound == nil || notFound.Type != contract.SchemaTypeObject || !notFound.OpenProperties ||
		len(notFound.Properties) != 4 {
		t.Fatalf("pinned github 404 object = %#v", notFound)
	}
}

func emittedResponse(t *testing.T, operation contract.Operation, status string) contract.Response {
	t.Helper()
	for _, response := range operation.Responses {
		if response.Status == status {
			return response
		}
	}
	t.Fatalf("operation %q has no %s response", operation.OperationID, status)
	return contract.Response{}
}

func emittedLaneStatus(t *testing.T, operation contract.Operation, lane contract.Lane) contract.LaneStatus {
	t.Helper()
	for _, eligibility := range operation.LaneEligibility {
		if eligibility.Lane == lane {
			return eligibility.Status
		}
	}
	t.Fatalf("operation %q has no %q lane", operation.OperationID, lane)
	return ""
}

func emittedInput(t *testing.T, operation contract.Operation, fieldID string) contract.FieldBinding {
	t.Helper()
	for _, binding := range operation.Inputs {
		if binding.FieldID == fieldID {
			return binding
		}
	}
	t.Fatalf("operation %q has no %q input binding", operation.OperationID, fieldID)
	return contract.FieldBinding{}
}

// pinnedConnectorInput loads one committed connector source lock, capture and
// semantics document. Bazel test sandboxes do not carry the connector checkout,
// so the pinned check is skipped there instead of asserting absent inputs.
func pinnedConnectorInput(t *testing.T, connector string) (CompilationInput, string) {
	t.Helper()
	root := filepath.Join("..", "..", "connectors", connector)
	lockDocument, err := os.ReadFile(filepath.Join(root, "source.lock.json"))
	if errors.Is(err, fs.ErrNotExist) {
		t.Skipf("pinned %s connector checkout is unavailable in this test sandbox", connector)
	}
	if err != nil {
		t.Fatal(err)
	}
	semantics, err := os.ReadFile(filepath.Join(root, "semantics.json"))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := DecodeSourceLock(lockDocument)
	if err != nil {
		t.Fatal(err)
	}
	captures := Captures{Documents: map[string][]byte{}, Licenses: map[string][]byte{}}
	for _, document := range lock.SourceSet.Documents {
		capture, err := os.ReadFile(filepath.Join(root, "captures", document.SourceID))
		if err != nil {
			t.Fatal(err)
		}
		license, err := os.ReadFile(filepath.Join("..", "..", document.License.Path))
		if err != nil {
			t.Fatal(err)
		}
		captures.Documents[document.SourceID] = capture
		captures.Licenses[document.SourceID] = license
	}
	return CompilationInput{
		SourceLock: lockDocument, Captures: captures, Semantics: semantics,
		TargetFoundations: HTTPCompilationTargetFoundations(),
	}, lock.ConnectorID
}
