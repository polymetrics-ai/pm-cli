package compile

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestDeterministicCompleteInventory(t *testing.T) {
	document := testFixture(t, "complete-openapi.yaml")
	operation := testOperation("getWidgets", "root", "#/paths/~1widgets/get", nil, []VariantRef{
		{Pointer: "#/paths/~1widgets/get/responses/200/content/application~1json", Status: "200", MediaType: "application/json"},
		{Pointer: "#/paths/~1widgets/get/responses/204", Status: "204", MediaType: noContentMediaType},
		{Pointer: "#/paths/~1widgets/get/responses/default/content/application~1problem+json", Status: "default", MediaType: "application/problem+json"},
	})
	captured := testCapturedSource(t, DialectOpenAPI30, map[string][]byte{"root": document}, nil, []SourceOperation{operation})
	resolved := testResolve(t, captured)
	loaded, err := LoadDialects(context.Background(), resolved)
	if err != nil {
		t.Fatal(err)
	}
	normalization, err := Normalize(context.Background(), loaded)
	if err != nil {
		t.Fatal(err)
	}
	target := HTTPCompilationTargetFoundations()
	if err := CheckRequirements(normalization, resolved, target); err != nil {
		t.Fatal(err)
	}
	projected, projectionDiagnostics := ProjectCommands(normalization)
	if len(projectionDiagnostics) != 0 {
		t.Fatalf("safe operation command diagnostics = %#v", projectionDiagnostics)
	}
	first, err := EmitBundle(normalization, target)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EmitBundle(normalization, target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("identical compiler inputs produced different bundle bytes")
	}
	var bundle Bundle
	if err := json.Unmarshal(first, &bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Operations) != 1 {
		t.Fatalf("operations = %d, want 1", len(bundle.Operations))
	}
	if bundle.Commands == nil || !reflect.DeepEqual(bundle.Commands, projected) {
		t.Fatalf("embedded commands = %#v, want exact ProjectCommands projection %#v", bundle.Commands, projected)
	}
	if bundle.UnavailableOperations == nil || len(bundle.UnavailableOperations) != 0 {
		t.Fatalf("unavailable operations = %#v, want explicit empty inventory", bundle.UnavailableOperations)
	}
	emitted := bundle.Operations[0]
	if len(emitted.LaneEligibility) != 7 || len(emitted.Responses) != 3 || len(emitted.RequestVariants) != 0 {
		t.Fatalf("incomplete emitted inventory: %#v", emitted)
	}
	if len(emitted.Inputs) != 1 || !emitted.Inputs[0].Required || emitted.Inputs[0].Style != "form" || emitted.Inputs[0].Explode {
		t.Fatalf("required/serialization facts changed: %#v", emitted.Inputs)
	}
	if emitted.Responses[1].Status != "204" || !emitted.Responses[1].NoContent || emitted.Responses[1].Schema != nil || emitted.Responses[1].MediaType != "" {
		t.Fatalf("no-content response changed: %#v", emitted.Responses)
	}
	capabilities := make([]string, 0, len(emitted.CapabilityRequirements))
	for _, requirement := range emitted.CapabilityRequirements {
		capabilities = append(capabilities, requirement.Capability)
	}
	wantCapabilities := []string{"effect.authorize", "effect.limit", "http.execute", "value.serialize"}
	if !slices.Equal(capabilities, wantCapabilities) {
		t.Fatalf("capability closure = %v, want %v", capabilities, wantCapabilities)
	}

	t.Run("mixed supported and unsupported operations remain in one runtime handoff", func(t *testing.T) {
		mixedDocument := []byte(`{
  "openapi": "3.0.3",
  "info": {"title": "Synthetic operation isolation", "version": "1.0.0"},
  "servers": [{"url": "https://isolation.example.test/v1"}],
  "paths": {
    "/good": {
      "get": {
        "operationId": "getGood",
        "responses": {
          "200": {
            "description": "bounded string",
            "content": {"application/json": {"schema": {"type": "string"}}}
          }
        }
      }
    },
    "/conditional": {
      "get": {
        "operationId": "getConditional",
        "responses": {
          "200": {
            "description": "unsupported conditional shape",
            "content": {"application/json": {"schema": {"type": "object", "properties": {"value": {"type": "string"}}, "not": {"required": ["value"]}}}}
          }
        }
      }
    }
  }
}`)
		operations := []SourceOperation{
			testOperation("getGood", "root", "#/paths/~1good/get", nil, []VariantRef{{
				Pointer: "#/paths/~1good/get/responses/200/content/application~1json", Status: "200", MediaType: "application/json",
			}}),
			testOperation("getConditional", "root", "#/paths/~1conditional/get", nil, []VariantRef{{
				Pointer: "#/paths/~1conditional/get/responses/200/content/application~1json", Status: "200", MediaType: "application/json",
			}}),
		}
		lock, captures := testSourceInput(t, DialectOpenAPI30, map[string][]byte{"root": mixedDocument}, nil, operations)
		mixedCaptured, err := CaptureSource(lock, captures)
		if err != nil {
			t.Fatal(err)
		}
		conditionalRef := []SourceRef{{SourceID: "root", Pointer: "#/paths/~1conditional/get"}}
		semantics, err := json.Marshal(SemanticsDocument{
			SchemaVersion: SemanticsSchemaVersion,
			ConnectorID:   lock.ConnectorID,
			SourceDigest:  mixedCaptured.Digest,
			Revision:      lock.SemanticsRevision,
			Operations: []OperationSemantics{{
				OperationID: "getConditional",
				SourceRefs:  conditionalRef,
				Lanes: []LaneSemantics{
					{
						Lane: contract.LaneDirectRead, Classification: LaneEligible,
						SourceRefs: conditionalRef,
					},
					{
						Lane: contract.LaneDirectWrite, Classification: LaneNotApplicable,
						Reason: "read operation has no write shape", SourceRefs: conditionalRef,
					},
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
		artifacts, err := Compile(context.Background(), CompilationInput{
			SourceLock: lockDocument, Captures: captures, Semantics: semantics,
			TargetFoundations: target,
		})
		if err != nil {
			t.Fatalf("mixed source compile: %v", err)
		}
		var mixedBundle Bundle
		if err := json.Unmarshal(artifacts.Bundle, &mixedBundle); err != nil {
			t.Fatal(err)
		}
		if len(mixedBundle.Operations) != 1 || mixedBundle.Operations[0].OperationID != "getGood" {
			t.Fatalf("executable operations = %#v, want only complete getGood", mixedBundle.Operations)
		}
		if len(mixedBundle.UnavailableOperations) != 1 {
			t.Fatalf("unavailable operations = %#v, want getConditional metadata", mixedBundle.UnavailableOperations)
		}
		unavailable := mixedBundle.UnavailableOperations[0]
		if unavailable.OperationID != "getConditional" ||
			!slices.Equal(unavailable.DiagnosticCodes, []string{"unsupported_schema_keyword"}) ||
			len(unavailable.SourceRefs) != 1 ||
			unavailable.SourceRefs[0].Pointer != "#/paths/~1conditional/get" {
			t.Fatalf("getConditional unavailable metadata = %#v", unavailable)
		}
		if len(unavailable.LaneEligibility) != 7 {
			t.Fatalf("getConditional lane inventory = %#v, want all seven lanes", unavailable.LaneEligibility)
		}
		var readBlocked, writeNotApplicable bool
		for _, lane := range unavailable.LaneEligibility {
			if lane.Status == contract.LaneEligible {
				t.Fatalf("unavailable operation retained executable lane: %#v", lane)
			}
			switch lane.Lane {
			case contract.LaneDirectRead:
				readBlocked = lane.Status == contract.LaneBlocked && strings.Contains(lane.Reason, "unsupported_schema_keyword")
			case contract.LaneDirectWrite:
				writeNotApplicable = lane.Status == contract.LaneNotApplicable && lane.Reason == "read operation has no write shape"
			}
		}
		if !readBlocked || !writeNotApplicable {
			t.Fatalf("getConditional lane reasons are not truthful: %#v", unavailable.LaneEligibility)
		}
		if len(mixedBundle.Commands) != 1 || mixedBundle.Commands[0].OperationID != "getGood" {
			t.Fatalf("embedded commands = %#v, want only executable getGood", mixedBundle.Commands)
		}
		var sidecar CommandManifest
		if err := json.Unmarshal(artifacts.CommandManifest, &sidecar); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(mixedBundle.Commands, sidecar.Commands) {
			t.Fatalf("runtime commands = %#v, diagnostic sidecar commands = %#v", mixedBundle.Commands, sidecar.Commands)
		}
		var report CompileReport
		if err := json.Unmarshal(artifacts.Report, &report); err != nil {
			t.Fatal(err)
		}
		var reportedConditional *NormalizedOperation
		for index := range report.Operations {
			if report.Operations[index].OperationID == "getConditional" {
				reportedConditional = &report.Operations[index]
			}
		}
		if reportedConditional == nil ||
			reportedConditional.Raw.Method != "GET" ||
			reportedConditional.Raw.Path != "/conditional" ||
			len(reportedConditional.Raw.SourceFacts) == 0 ||
			len(reportedConditional.Raw.RequestVariants) != 1 ||
			reportedConditional.Raw.RequestVariants[0].MediaType != parameterOnlyMediaType ||
			len(reportedConditional.Raw.ResponseVariants) != 1 ||
			reportedConditional.Raw.ResponseVariants[0].Status != "200" ||
			reportedConditional.Raw.ResponseVariants[0].MediaType != "application/json" ||
			len(reportedConditional.RequestVariants) != 1 ||
			len(reportedConditional.RequestVariants[0].SourceFacts) == 0 ||
			len(reportedConditional.ResponseVariants) != 1 ||
			len(reportedConditional.ResponseVariants[0].SourceFacts) == 0 ||
			len(reportedConditional.ResponseVariants[0].SchemaFacts) == 0 {
			t.Fatalf("authoring report hid getConditional source/status/media facts: %#v", reportedConditional)
		}
		var reportedDiagnostic bool
		for _, diagnostic := range report.Diagnostics {
			if diagnostic.OperationID == "getConditional" && diagnostic.Code == "unsupported_schema_keyword" && len(diagnostic.SourceFacts) != 0 {
				reportedDiagnostic = true
			}
		}
		if !reportedDiagnostic {
			t.Fatalf("authoring report diagnostics = %#v, want source-linked unsupported_schema_keyword", report.Diagnostics)
		}

		corruptCaptures := Captures{Documents: map[string][]byte{}, Licenses: map[string][]byte{}}
		for sourceID, capture := range captures.Documents {
			corruptCaptures.Documents[sourceID] = bytes.Clone(capture)
		}
		for sourceID, license := range captures.Licenses {
			corruptCaptures.Licenses[sourceID] = bytes.Clone(license)
		}
		corruptCaptures.Documents["root"] = append(corruptCaptures.Documents["root"], ' ')
		corruptArtifacts, corruptErr := Compile(context.Background(), CompilationInput{
			SourceLock: lockDocument, Captures: corruptCaptures, Semantics: semantics,
			TargetFoundations: target,
		})
		if corruptErr == nil || len(corruptArtifacts.Bundle) != 0 {
			t.Fatalf("corrupt capture published bundle bytes: err=%v bundle=%q", corruptErr, corruptArtifacts.Bundle)
		}
	})

	t.Run("source fidelity diagnostics still fail closed", func(t *testing.T) {
		faulty := *normalization
		faulty.Diagnostics = append(slices.Clone(normalization.Diagnostics), newDiagnostic(
			"source_fidelity_disagreement",
			SeverityError,
			normalization.Operations[0].OperationID,
			"",
			normalization.Operations[0].Raw.SourceFacts,
			"/operations/getWidgets",
			nil,
			allLaneSlice(),
			"synthetic normalized fact disagrees with captured source",
		))
		if _, err := EmitBundle(&faulty, target); err == nil {
			t.Fatal("source-fidelity error was downgraded to unavailable metadata")
		}
	})

	t.Run("dropped operation fails completeness", func(t *testing.T) {
		copy := *normalization
		copy.Operations = nil
		if err := CheckRequirements(&copy, resolved, target); err == nil {
			t.Fatal("dropped source operation was accepted")
		}
	})
}

func TestAdmittedExecutionFactsReachBundle(t *testing.T) {
	for _, test := range []struct {
		name        string
		headers     string
		bind        bool
		unavailable bool
	}{
		{name: "exact witnessed Link", headers: `"Link": {"schema": {"type": "string"}}`, bind: true},
		{name: "unbound Link remains unavailable", headers: `"Link": {"schema": {"type": "string"}}`, unavailable: true},
		{name: "other metadata remains unavailable", headers: `"Link": {"schema": {"type": "string"}}, "X-Unrepresented": {"schema": {"type": "string"}}`, bind: true, unavailable: true},
		{name: "unrepresented constraint remains unavailable", headers: `"Link": {"schema": {"type": "string", "enum": ["reserved"]}}`, bind: true, unavailable: true},
		{name: "case ambiguous declaration remains unavailable", headers: `"Link": {"schema": {"type": "string"}}, "link": {"schema": {"type": "string"}}`, bind: true, unavailable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			testExecutionFactsReachBundle(t, test.headers, test.bind, test.unavailable)
		})
	}
}

func testExecutionFactsReachBundle(t *testing.T, headers string, bindLink, wantUnavailable bool) {
	t.Helper()
	document := []byte(`{
  "openapi": "3.0.3",
  "info": {"title": "Execution fact fixture", "version": "1.0.0"},
  "servers": [{"url": "https://api.example.test/v3"}],
  "components": {
    "securitySchemes": {
      "GitHubToken": {"type": "http", "scheme": "bearer"}
    }
  },
  "paths": {
    "/items": {
      "get": {
        "operationId": "listItems",
        "security": [{"GitHubToken": []}],
        "parameters": [
          {"name": "page", "in": "query", "schema": {"type": "string"}},
          {"name": "limit", "in": "query", "schema": {"type": "integer"}},
          {
            "name": "X-GitHub-Api-Version",
            "in": "header",
            "required": true,
            "schema": {"type": "string", "enum": ["2022-11-28"]}
          }
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
                  "type": "object",
                  "additionalProperties": false,
                  "required": ["records", "total"],
                  "properties": {
                    "records": {
                      "type": "array",
                      "items": {
                        "type": "object",
                        "additionalProperties": false,
                        "required": ["id"],
                        "properties": {"id": {"type": "integer"}}
                      }
                    },
                    "total": {"type": "integer"}
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
	document = bytes.Replace(document, []byte(`"Link": {"schema": {"type": "string"}}`), []byte(headers), 1)
	sourceOperation := testOperation(
		"listItems",
		"root",
		"#/paths/~1items/get",
		nil,
		[]VariantRef{{
			Pointer:   "#/paths/~1items/get/responses/200/content/application~1json",
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
	if normalizedOperation.HTTP == nil || normalizedOperation.HTTP.Authorization == nil ||
		!normalizedOperation.HTTP.Authorization.Required ||
		!slices.Equal(normalizedOperation.HTTP.Origins, []string{"https://api.example.test/v3"}) ||
		len(normalizedOperation.HTTP.Headers) != 1 ||
		normalizedOperation.HTTP.Headers[0] != (contract.OperationHTTPHeader{Name: "X-GitHub-Api-Version", Value: "2022-11-28"}) ||
		len(normalizedOperation.HTTPFacts) == 0 {
		t.Fatalf("source HTTP authority was not normalized exactly: %#v", normalizedOperation.HTTP)
	}
	var pageFieldID, limitFieldID, authorizationFieldID string
	for _, binding := range normalizedOperation.CommonBindings {
		if binding.Normalized == nil {
			continue
		}
		switch binding.Normalized.WireName {
		case "page":
			pageFieldID = binding.FieldID
		case "limit":
			limitFieldID = binding.FieldID
		case "Authorization":
			authorizationFieldID = binding.FieldID
			if binding.Sensitivity != SensitivitySecret ||
				binding.FieldID != normalizedOperation.HTTP.Authorization.FieldID {
				t.Fatalf("authorization binding is not isolated as source-backed secret input: %#v", binding)
			}
		case "X-GitHub-Api-Version":
			t.Fatalf("fixed source header leaked into ordinary inputs: %#v", binding)
		}
	}
	if pageFieldID == "" || limitFieldID == "" || authorizationFieldID == "" {
		t.Fatal("source request and authorization bindings were not admitted")
	}
	sourceRefs := []contract.SourceRef{{
		SourceID: "root",
		Pointer:  "#/paths/~1items/get",
	}}
	linkSourceRefs := []contract.SourceRef{{
		SourceID: "root",
		Pointer:  "#/paths/~1items/get/responses/200/headers/Link",
	}}
	witness := Witness{ProofID: "source-reviewed-execution", SourceRefs: slices.Clone(sourceRefs)}
	linkWitness := Witness{ProofID: "source-reviewed-link", SourceRefs: slices.Clone(linkSourceRefs)}
	httpAmendment := normalizedOperation.HTTP.clone()
	httpAmendment.Witness.ProofID = "source-reviewed-http"
	httpSourceRefs := slices.Clone(httpAmendment.Witness.SourceRefs)
	semantics := SemanticsDocument{
		SchemaVersion: SemanticsSchemaVersion,
		ConnectorID:   lock.ConnectorID,
		SourceDigest:  captured.Digest,
		Revision:      lock.SemanticsRevision,
		Operations: []OperationSemantics{{
			OperationID: "listItems",
			SourceRefs:  slices.Clone(sourceRefs),
			HTTP:        &httpAmendment,
			RecordExtraction: &RecordExtraction{
				RecordsPointer: "/records",
				Witness:        witness,
			},
			Pagination: &Pagination{
				Kind:               string(contract.OperationPaginationLink),
				LinkRequestField:   pageFieldID,
				LinkResponseHeader: "Link",
				LimitRequestField:  limitFieldID,
				Witness:            linkWitness,
			},
			Retry: &RetrySemantics{
				Classification: string(contract.OperationRetrySafe),
				HTTPStatuses:   []uint16{429, 502},
				Witness:        witness,
			},
		}},
	}
	if !bindLink {
		semantics.Operations[0].Pagination = nil
	}
	if err := ApplyAmendments(normalization, resolved, &semantics); err != nil {
		t.Fatal(err)
	}
	if wantUnavailable {
		target := HTTPCompilationTargetFoundations()
		if err := CheckRequirements(normalization, resolved, target); err != nil {
			t.Fatal(err)
		}
		encoded, err := EmitBundle(normalization, target)
		if err != nil {
			t.Fatal(err)
		}
		var result Bundle
		if err := json.Unmarshal(encoded, &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Operations) != 0 || len(result.UnavailableOperations) != 1 ||
			result.UnavailableOperations[0].OperationID != "listItems" {
			t.Fatalf("unrepresented response metadata became executable: %#v", result)
		}
		return
	}
	semantics.Operations[0].SourceRefs[0].Pointer = "#/paths/~1items"
	semantics.Operations[0].HTTP.Origins[0] = "https://mutated.invalid"
	semantics.Operations[0].HTTP.Authorization.Prefix = "Mutated "
	semantics.Operations[0].HTTP.Headers[0].Value = "mutated"
	semantics.Operations[0].HTTP.Witness.SourceRefs[0].Pointer = "#/paths/~1items"
	semantics.Operations[0].RecordExtraction.Witness.SourceRefs[0].Pointer = "#/paths/~1items"
	semantics.Operations[0].Pagination.Witness.SourceRefs[0].Pointer = "#/paths/~1items"
	semantics.Operations[0].Retry.HTTPStatuses[0] = 500
	semantics.Operations[0].Retry.Witness.SourceRefs[0].Pointer = "#/paths/~1items"
	admitted := normalizedOperation.Semantics
	if admitted == nil || admitted.HTTP == nil ||
		admitted.SourceRefs[0].Pointer != sourceRefs[0].Pointer ||
		admitted.HTTP.Origins[0] != "https://api.example.test/v3" ||
		admitted.HTTP.Authorization.Prefix != "Bearer " ||
		admitted.HTTP.Headers[0].Value != "2022-11-28" ||
		!slices.Equal(admitted.HTTP.Witness.SourceRefs, httpSourceRefs) ||
		admitted.RecordExtraction.Witness.SourceRefs[0].Pointer != sourceRefs[0].Pointer ||
		admitted.Pagination.Witness.SourceRefs[0].Pointer != linkSourceRefs[0].Pointer ||
		!slices.Equal(admitted.Retry.HTTPStatuses, []uint16{429, 502}) ||
		admitted.Retry.Witness.SourceRefs[0].Pointer != sourceRefs[0].Pointer {
		t.Fatalf("admitted execution facts alias mutable authoring input: %#v", admitted)
	}
	target := HTTPCompilationTargetFoundations()
	if err := CheckRequirements(normalization, resolved, target); err != nil {
		t.Fatal(err)
	}
	detached, err := emitOperation(*normalizedOperation, target)
	if err != nil {
		t.Fatalf("emit admitted operation: %v; diagnostics: %#v", err, normalization.Diagnostics)
	}
	if detached.Execution == nil || detached.Execution.HTTP == nil ||
		detached.Execution.Pagination == nil || detached.Execution.Retry == nil {
		t.Fatalf("admitted execution facts were omitted from the emitted operation: %#v", detached.Execution)
	}
	admitted.HTTP.Origins[0] = "https://mutated.invalid"
	admitted.HTTP.Headers[0].Value = "mutated"
	admitted.HTTP.Witness.SourceRefs[0].Pointer = "#/paths/~1items"
	admitted.Pagination.Witness.SourceRefs[0].Pointer = "#/paths/~1items"
	admitted.Retry.HTTPStatuses[0] = 500
	admitted.Retry.Witness.SourceRefs[0].Pointer = "#/paths/~1items"
	if detached.Execution.HTTP.Origins[0] != "https://api.example.test/v3" ||
		detached.Execution.HTTP.Headers[0].Value != "2022-11-28" ||
		!slices.Equal(detached.Execution.HTTP.Witness.SourceRefs, httpSourceRefs) ||
		detached.Execution.Pagination.Witness.SourceRefs[0].Pointer != linkSourceRefs[0].Pointer ||
		!slices.Equal(detached.Execution.Retry.HTTPStatuses, []uint16{429, 502}) ||
		detached.Execution.Retry.Witness.SourceRefs[0].Pointer != sourceRefs[0].Pointer {
		t.Fatalf("emitted execution facts alias normalized source facts: %#v", detached.Execution)
	}
	admitted.HTTP.Origins[0] = "https://api.example.test/v3"
	admitted.HTTP.Headers[0].Value = "2022-11-28"
	admitted.HTTP.Witness.SourceRefs = slices.Clone(httpSourceRefs)
	admitted.Pagination.Witness.SourceRefs = slices.Clone(linkSourceRefs)
	admitted.Retry.HTTPStatuses = []uint16{429, 502}
	admitted.Retry.Witness.SourceRefs = slices.Clone(sourceRefs)
	encoded, err := EmitBundle(normalization, target)
	if err != nil {
		t.Fatal(err)
	}
	var bundle Bundle
	if err := json.Unmarshal(encoded, &bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Operations) != 1 {
		t.Fatalf("operations = %d, want 1", len(bundle.Operations))
	}
	operation := bundle.Operations[0]
	if operation.Execution == nil || operation.Execution.HTTP == nil ||
		operation.Execution.Pagination == nil || operation.Execution.Retry == nil {
		t.Fatalf("emitted execution facts = %#v", operation.Execution)
	}
	httpFact := operation.Execution.HTTP
	if !slices.Equal(httpFact.Origins, []string{"https://api.example.test/v3"}) ||
		httpFact.Authorization == nil ||
		httpFact.Authorization.Audience != "https://api.example.test" ||
		httpFact.Authorization.FieldID != authorizationFieldID ||
		httpFact.Authorization.Prefix != "Bearer " ||
		len(httpFact.Headers) != 1 ||
		httpFact.Headers[0] != (contract.OperationHTTPHeader{Name: "X-GitHub-Api-Version", Value: "2022-11-28"}) ||
		httpFact.Witness.ProofID != "source-reviewed-http" ||
		!slices.Equal(httpFact.Witness.SourceRefs, httpSourceRefs) {
		t.Fatalf("emitted HTTP facts = %#v", httpFact)
	}
	pagination := operation.Execution.Pagination
	if pagination.Kind != contract.OperationPaginationLink ||
		pagination.LinkRequestField != pageFieldID ||
		pagination.LinkResponseHeader != "Link" ||
		pagination.LimitRequestField != limitFieldID ||
		pagination.Witness.ProofID != linkWitness.ProofID ||
		!reflect.DeepEqual(pagination.Witness.SourceRefs, linkSourceRefs) {
		t.Fatalf("emitted pagination = %#v", pagination)
	}
	retry := operation.Execution.Retry
	if retry.Classification != contract.OperationRetrySafe ||
		retry.Condition != "" ||
		!slices.Equal(retry.HTTPStatuses, []uint16{429, 502}) ||
		retry.Witness.ProofID != witness.ProofID ||
		!reflect.DeepEqual(retry.Witness.SourceRefs, sourceRefs) {
		t.Fatalf("emitted retry = %#v", retry)
	}
	if len(operation.Responses) != 1 ||
		operation.Responses[0].RecordPointer == nil ||
		*operation.Responses[0].RecordPointer != "/records" {
		t.Fatalf("record extraction = %#v", operation.Responses)
	}
}

func TestUnsupportedHTTPSourceFactsRemainUnavailable(t *testing.T) {
	document := []byte(`{
  "openapi": "3.0.3",
  "info": {"title": "HTTP authority refusal fixture", "version": "1.0.0"},
  "servers": [{"url": "https://api.example.test/v1"}],
  "components": {
    "securitySchemes": {
      "A": {"type": "apiKey", "in": "header", "name": "X-A"},
      "B": {"type": "apiKey", "in": "header", "name": "X-B"}
    }
  },
  "paths": {
    "/good": {"get": {"operationId": "good", "responses": {"204": {"description": "ok"}}}},
    "/relative": {"get": {
      "operationId": "relative",
      "servers": [{"url": "/v1"}],
      "responses": {"204": {"description": "unsupported"}}
    }},
    "/templated": {"get": {
      "operationId": "templated",
      "servers": [{"url": "https://{region}.example.test/v1", "variables": {"region": {"default": "us"}}}],
      "responses": {"204": {"description": "unsupported"}}
    }},
    "/compound": {"get": {
      "operationId": "compound",
      "security": [{"A": [], "B": []}],
      "responses": {"204": {"description": "unsupported"}}
    }},
    "/unsafe-header": {"get": {
      "operationId": "unsafeHeader",
      "parameters": [{
        "name": "Private-Token",
        "in": "header",
        "required": true,
        "schema": {"type": "string", "enum": ["embedded-secret"]}
      }],
      "responses": {"204": {"description": "unsupported"}}
    }}
  }
}`)
	operationIDs := []string{"good", "relative", "templated", "compound", "unsafeHeader"}
	paths := []string{"good", "relative", "templated", "compound", "unsafe-header"}
	operations := make([]SourceOperation, 0, len(operationIDs))
	for index, operationID := range operationIDs {
		pointer := "#/paths/~1" + paths[index] + "/get"
		operations = append(operations, testOperation(operationID, "root", pointer, nil, []VariantRef{{
			Pointer: pointer + "/responses/204", Status: "204", MediaType: noContentMediaType,
		}}))
	}
	captured := testCapturedSource(t, DialectOpenAPI30, map[string][]byte{"root": document}, nil, operations)
	resolved := testResolve(t, captured)
	loaded, err := LoadDialects(context.Background(), resolved)
	if err != nil {
		t.Fatal(err)
	}
	normalization, err := Normalize(context.Background(), loaded)
	if err != nil {
		t.Fatal(err)
	}
	target := HTTPCompilationTargetFoundations()
	if err := CheckRequirements(normalization, resolved, target); err != nil {
		t.Fatal(err)
	}
	encoded, err := EmitBundle(normalization, target)
	if err != nil {
		t.Fatal(err)
	}
	var bundle Bundle
	if err := json.Unmarshal(encoded, &bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Operations) != 1 || bundle.Operations[0].OperationID != "good" ||
		len(bundle.UnavailableOperations) != 4 {
		t.Fatalf("HTTP source refusal boundary = %#v", bundle)
	}
	wantCodes := map[string]string{
		"relative":     "unsupported_http_authority",
		"templated":    "unsupported_http_authority",
		"compound":     "unsupported_authentication",
		"unsafeHeader": "unsupported_fixed_headers",
	}
	for _, unavailable := range bundle.UnavailableOperations {
		want, ok := wantCodes[unavailable.OperationID]
		if !ok || !slices.Contains(unavailable.DiagnosticCodes, want) {
			t.Fatalf("unavailable HTTP source metadata = %#v", unavailable)
		}
	}
}

func TestReservedProviderFlagsRemainInvocableThroughDeterministicAliases(t *testing.T) {
	document := []byte(`{
  "openapi": "3.0.3",
  "info": {"title": "Reserved provider flag fixture", "version": "1.0.0"},
  "servers": [{"url": "https://reserved-flags.example.test/v1"}],
  "paths": {
    "/records": {
      "get": {
        "operationId": "listRecords",
        "parameters": [
          {
            "name": "result-controls",
            "in": "query",
            "required": true,
            "schema": {"type": "string"}
          },
          {
            "name": "query-query-result-controls",
            "in": "query",
            "required": false,
            "schema": {"type": "string"}
          }
        ],
        "responses": {"204": {"description": "no content"}}
      }
    }
  }
}`)
	operation := testOperation(
		"listRecords",
		"root",
		"#/paths/~1records/get",
		nil,
		[]VariantRef{{
			Pointer: "#/paths/~1records/get/responses/204",
			Status:  "204", MediaType: noContentMediaType,
		}},
	)
	captured := testCapturedSource(t, DialectOpenAPI30, map[string][]byte{"root": document}, nil, []SourceOperation{operation})
	resolved := testResolve(t, captured)
	loaded, err := LoadDialects(context.Background(), resolved)
	if err != nil {
		t.Fatal(err)
	}
	normalization, err := Normalize(context.Background(), loaded)
	if err != nil {
		t.Fatal(err)
	}
	target := HTTPCompilationTargetFoundations()
	if err := CheckRequirements(normalization, resolved, target); err != nil {
		t.Fatal(err)
	}
	commands, diagnostics := ProjectCommands(normalization)
	if len(diagnostics) != 0 || len(commands) != 1 {
		t.Fatalf("reserved provider projection = commands %#v diagnostics %#v", commands, diagnostics)
	}
	fields := make(map[string]contract.CommandField, len(commands[0].Fields))
	for _, field := range commands[0].Fields {
		fields[field.FieldID] = field
		if contract.IsReservedPMControlFlag(field.Flag) {
			t.Fatalf("provider field retained reserved flag %q", field.Flag)
		}
	}
	resultControlsField, ok := fields["query.result-controls"]
	if !ok ||
		resultControlsField.Location != contract.FieldLocationQuery ||
		!resultControlsField.Required {
		t.Fatalf("result-controls alias = %#v", resultControlsField)
	}
	aliasCollisionField, ok := fields["query.query-query-result-controls"]
	if !ok ||
		aliasCollisionField.Location != contract.FieldLocationQuery ||
		aliasCollisionField.Required {
		t.Fatalf("alias collision field = %#v", aliasCollisionField)
	}
	if resultControlsField.Flag == aliasCollisionField.Flag {
		t.Fatal("provider flag collision did not preserve independently invocable fields")
	}

	encoded, err := EmitBundle(normalization, target)
	if err != nil {
		t.Fatalf("emit aliased provider operation: %v", err)
	}
	repeated, err := EmitBundle(normalization, target)
	if err != nil || !bytes.Equal(encoded, repeated) {
		t.Fatalf("alias collision changed deterministic bundle output: %v", err)
	}
	var bundle Bundle
	if err := json.Unmarshal(encoded, &bundle); err != nil {
		t.Fatalf("decode aliased provider bundle: %v", err)
	}
	if len(bundle.Operations) != 1 || len(bundle.Commands) != 1 || len(bundle.UnavailableOperations) != 0 {
		t.Fatalf("aliased provider operation was suppressed: %#v", bundle)
	}
	inputs := make(map[string]contract.FieldBinding, len(bundle.Operations[0].Inputs))
	for _, input := range bundle.Operations[0].Inputs {
		inputs[input.FieldID] = input
	}
	resultControlsInput, ok := inputs["query.result-controls"]
	if !ok ||
		resultControlsInput.WireName != "result-controls" ||
		resultControlsInput.Location != contract.FieldLocationQuery ||
		!resultControlsInput.Required {
		t.Fatalf("emitted provider wire binding changed: %#v", resultControlsInput)
	}
	aliasCollisionInput, ok := inputs["query.query-query-result-controls"]
	if !ok ||
		aliasCollisionInput.WireName != "query-query-result-controls" ||
		aliasCollisionInput.Location != contract.FieldLocationQuery ||
		aliasCollisionInput.Required {
		t.Fatalf("emitted alias-collision wire binding changed: %#v", aliasCollisionInput)
	}
}

// headerParameterDocument declares required and optional header parameters at
// the path-item level (shared by every operation under the path) and at the
// operation level, next to a query parameter with a source default.
const headerParameterDocument = `{
  "openapi": "3.0.3",
  "info": {"title": "Header parameter fixture", "version": "1.0.0"},
  "servers": [{"url": "https://headers.example.test/v1"}],
  "paths": {
    "/records": {
      "parameters": [
        {"name": "User-Agent", "in": "header", "required": true, "schema": {"type": "string"}},
        {"name": "X-Trace", "in": "header", "required": false, "schema": {"type": "string"}}
      ],
      "get": {
        "operationId": "listRecords",
        "parameters": [
          {"name": "X-Request-Id", "in": "header", "required": true, "schema": {"type": "string"}},
          {"name": "per-page", "in": "query", "required": false, "schema": {"type": "integer", "default": 30}}
        ],
        "responses": {"204": {"description": "no content"}}
      }
    }
  }
}`

// TestDeclaredHeaderParametersReachTheEmittedOperationInputs pins the parameter
// admission contract for the shape whose loss is invisible at the operation
// level: a required header parameter declared at the path-item level must reach
// the emitted operation's inputs with its exact wire name and requiredness, and
// so must one declared at the operation level. A compiler that quietly drops
// either declaration would publish an operation that cannot carry a header the
// source requires.
func TestDeclaredHeaderParametersReachTheEmittedOperationInputs(t *testing.T) {
	pointer := "#/paths/~1records/get"
	operation := testOperation("listRecords", "root", pointer, nil, []VariantRef{{
		Pointer: pointer + "/responses/204", Status: "204", MediaType: noContentMediaType,
	}})
	captured := testCapturedSource(t, DialectOpenAPI30, map[string][]byte{"root": []byte(headerParameterDocument)}, nil, []SourceOperation{operation})
	resolved := testResolve(t, captured)
	loaded, err := LoadDialects(context.Background(), resolved)
	if err != nil {
		t.Fatal(err)
	}
	normalization, err := Normalize(context.Background(), loaded)
	if err != nil {
		t.Fatal(err)
	}
	target := HTTPCompilationTargetFoundations()
	if err := CheckRequirements(normalization, resolved, target); err != nil {
		t.Fatal(err)
	}
	encoded, err := EmitBundle(normalization, target)
	if err != nil {
		t.Fatal(err)
	}
	var bundle Bundle
	if err := json.Unmarshal(encoded, &bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Operations) != 1 || bundle.Operations[0].OperationID != "listRecords" {
		t.Fatalf("emitted operations = %#v", bundle.Operations)
	}
	inputs := make(map[string]contract.FieldBinding, len(bundle.Operations[0].Inputs))
	for _, input := range bundle.Operations[0].Inputs {
		inputs[input.FieldID] = input
	}
	for _, want := range []struct {
		fieldID  string
		wireName string
		required bool
	}{
		{fieldID: "header.user-agent", wireName: "User-Agent", required: true},
		{fieldID: "header.x-request-id", wireName: "X-Request-Id", required: true},
		{fieldID: "header.x-trace", wireName: "X-Trace", required: false},
	} {
		binding, present := inputs[want.fieldID]
		if !present || binding.WireName != want.wireName ||
			binding.Location != contract.FieldLocationHeader || binding.Required != want.required {
			t.Fatalf("declared header parameter %q emitted as %#v (present=%t)", want.fieldID, binding, present)
		}
	}
	if perPage := inputs["query.per-page"]; perPage.SourceDefault == nil {
		t.Fatalf("declared query default was dropped: %#v", perPage)
	}
}

// TestParameterAbsentFromTheCaptureRefusesTheOperation pins the refusal side of
// the same contract: when the capture does not carry the target of a declared
// required parameter, the operation is refused instead of being emitted with
// the parameter silently missing.
func TestParameterAbsentFromTheCaptureRefusesTheOperation(t *testing.T) {
	document := []byte(`{
  "openapi": "3.0.3",
  "info": {"title": "Dropped parameter fixture", "version": "1.0.0"},
  "servers": [{"url": "https://dropped.example.test/v1"}],
  "paths": {
    "/records": {
      "get": {
        "operationId": "listRecords",
        "parameters": [{"$ref": "#/components/parameters/absent-user-agent"}],
        "responses": {"204": {"description": "no content"}}
      }
    }
  }
}`)
	pointer := "#/paths/~1records/get"
	operation := testOperation("listRecords", "root", pointer, nil, []VariantRef{{
		Pointer: pointer + "/responses/204", Status: "204", MediaType: noContentMediaType,
	}})
	lock, captures := testSourceInput(t, DialectOpenAPI30, map[string][]byte{"root": document}, nil, []SourceOperation{operation})
	captured, err := CaptureSource(lock, captures)
	if err != nil {
		t.Fatal(err)
	}
	_, resolveErr := ResolveRefs(captured)
	if resolveErr == nil {
		t.Fatal("a declared parameter absent from the capture resolved without refusal")
	}
	if !strings.Contains(resolveErr.Error(), "parameters/0/$ref") ||
		!strings.Contains(resolveErr.Error(), "captured reference target pointer does not exist") {
		t.Fatalf("refusal did not report the missing captured reference target: %v", resolveErr)
	}
}
