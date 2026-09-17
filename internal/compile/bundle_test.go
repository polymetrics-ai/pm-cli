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
    "/open": {
      "get": {
        "operationId": "getOpen",
        "responses": {
          "200": {
            "description": "unsupported open shape",
            "content": {"application/json": {"schema": {"type": "object"}}}
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
			testOperation("getOpen", "root", "#/paths/~1open/get", nil, []VariantRef{{
				Pointer: "#/paths/~1open/get/responses/200/content/application~1json", Status: "200", MediaType: "application/json",
			}}),
		}
		lock, captures := testSourceInput(t, DialectOpenAPI30, map[string][]byte{"root": mixedDocument}, nil, operations)
		mixedCaptured, err := CaptureSource(lock, captures)
		if err != nil {
			t.Fatal(err)
		}
		openRef := []SourceRef{{SourceID: "root", Pointer: "#/paths/~1open/get"}}
		semantics, err := json.Marshal(SemanticsDocument{
			SchemaVersion: SemanticsSchemaVersion,
			ConnectorID:   lock.ConnectorID,
			SourceDigest:  mixedCaptured.Digest,
			Revision:      lock.SemanticsRevision,
			Operations: []OperationSemantics{{
				OperationID: "getOpen",
				SourceRefs:  openRef,
				Lanes: []LaneSemantics{
					{
						Lane: contract.LaneDirectRead, Classification: LaneEligible,
						SourceRefs: openRef,
					},
					{
						Lane: contract.LaneDirectWrite, Classification: LaneNotApplicable,
						Reason: "read operation has no write shape", SourceRefs: openRef,
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
			t.Fatalf("unavailable operations = %#v, want getOpen metadata", mixedBundle.UnavailableOperations)
		}
		unavailable := mixedBundle.UnavailableOperations[0]
		if unavailable.OperationID != "getOpen" ||
			!slices.Equal(unavailable.DiagnosticCodes, []string{"open_object_shape"}) ||
			len(unavailable.SourceRefs) != 1 ||
			unavailable.SourceRefs[0].Pointer != "#/paths/~1open/get" {
			t.Fatalf("getOpen unavailable metadata = %#v", unavailable)
		}
		if len(unavailable.LaneEligibility) != 7 {
			t.Fatalf("getOpen lane inventory = %#v, want all seven lanes", unavailable.LaneEligibility)
		}
		var readBlocked, writeNotApplicable bool
		for _, lane := range unavailable.LaneEligibility {
			if lane.Status == contract.LaneEligible {
				t.Fatalf("unavailable operation retained executable lane: %#v", lane)
			}
			switch lane.Lane {
			case contract.LaneDirectRead:
				readBlocked = lane.Status == contract.LaneBlocked && strings.Contains(lane.Reason, "open_object_shape")
			case contract.LaneDirectWrite:
				writeNotApplicable = lane.Status == contract.LaneNotApplicable && lane.Reason == "read operation has no write shape"
			}
		}
		if !readBlocked || !writeNotApplicable {
			t.Fatalf("getOpen lane reasons are not truthful: %#v", unavailable.LaneEligibility)
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
		var reportedOpen *NormalizedOperation
		for index := range report.Operations {
			if report.Operations[index].OperationID == "getOpen" {
				reportedOpen = &report.Operations[index]
			}
		}
		if reportedOpen == nil ||
			reportedOpen.Raw.Method != "GET" ||
			reportedOpen.Raw.Path != "/open" ||
			len(reportedOpen.Raw.SourceFacts) == 0 ||
			len(reportedOpen.Raw.RequestVariants) != 1 ||
			reportedOpen.Raw.RequestVariants[0].MediaType != parameterOnlyMediaType ||
			len(reportedOpen.Raw.ResponseVariants) != 1 ||
			reportedOpen.Raw.ResponseVariants[0].Status != "200" ||
			reportedOpen.Raw.ResponseVariants[0].MediaType != "application/json" ||
			len(reportedOpen.RequestVariants) != 1 ||
			len(reportedOpen.RequestVariants[0].SourceFacts) == 0 ||
			len(reportedOpen.ResponseVariants) != 1 ||
			len(reportedOpen.ResponseVariants[0].SourceFacts) == 0 ||
			len(reportedOpen.ResponseVariants[0].SchemaFacts) == 0 {
			t.Fatalf("authoring report hid getOpen source/status/media facts: %#v", reportedOpen)
		}
		var reportedDiagnostic bool
		for _, diagnostic := range report.Diagnostics {
			if diagnostic.OperationID == "getOpen" && diagnostic.Code == "open_object_shape" && len(diagnostic.SourceFacts) != 0 {
				reportedDiagnostic = true
			}
		}
		if !reportedDiagnostic {
			t.Fatalf("authoring report diagnostics = %#v, want source-linked open_object_shape", report.Diagnostics)
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
