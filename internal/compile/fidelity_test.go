package compile

import (
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestFieldSourceFidelity(t *testing.T) {
	document := testFixture(t, "synthetic-city-openapi.yaml")
	operation := testOperation(
		"createSyntheticAddress", "root", "#/paths/~1addresses/post",
		[]VariantRef{{Pointer: "#/paths/~1addresses/post/requestBody/content/application~1json", MediaType: "application/json"}},
		[]VariantRef{{Pointer: "#/paths/~1addresses/post/responses/204", Status: "204", MediaType: noContentMediaType}},
	)
	normalization := testNormalize(t, DialectOpenAPI30, map[string][]byte{"root": document}, nil, []SourceOperation{operation})
	variant := normalization.Operations[0].RequestVariants[0]
	var city, id BindingFact
	for _, binding := range variant.Bindings {
		if binding.Normalized == nil {
			continue
		}
		switch binding.Normalized.WireName {
		case "City":
			city = binding
		case "id":
			id = binding
		}
	}
	if city.Normalized == nil || city.RawNullable || city.Normalized.Schema.Type != contract.SchemaTypeString {
		t.Fatalf("synthetic raw City fact = %#v", city)
	}
	if id.Normalized == nil || id.BlockedDiagnosticID != "" {
		t.Fatalf("unaffected synthetic id was blocked: %#v", id)
	}
	faulty := *city.Normalized
	faulty.Nullable = true
	faulty.Schema.Nullable = true
	diagnostic := CheckBindingFidelity(BindingExpectation{
		OperationID:       normalization.Operations[0].OperationID,
		FieldID:           city.FieldID,
		SourceFacts:       city.SourceFacts,
		Required:          city.RawRequired,
		Nullable:          city.RawNullable,
		Style:             city.RawStyle,
		Explode:           city.RawExplode,
		AllowReserved:     city.RawAllowReserved,
		NormalizedPointer: "/operations/createSyntheticAddress/request_variants/request.application-json/bindings/City",
		AffectedCells:     allLaneSlice(),
	}, faulty)
	if diagnostic == nil || diagnostic.Code != "source_fidelity_disagreement" || diagnostic.NormalizedSHA256 == "" || len(diagnostic.SourceFacts) != 1 || diagnostic.SourceFacts[0].SHA256 == "" {
		t.Fatalf("synthetic City disagreement = %#v", diagnostic)
	}
	if diagnostic.NormalizedPointer == "" || diagnostic.SourceFacts[0].SourceRef.Pointer != "#/paths/~1addresses/post/requestBody/content/application~1json/schema/properties/City" {
		t.Fatalf("synthetic City pointers = %#v", diagnostic)
	}

	t.Run("explicit defaults retain presence without implicit application", func(t *testing.T) {
		defaultsDocument := testFixture(t, "defaults-openapi.yaml")
		operations := []SourceOperation{
			testOperation("getDefaults", "defaults", "#/paths/~1defaults/get", nil, []VariantRef{{
				Pointer: "#/paths/~1defaults/get/responses/200/content/application~1json",
				Status:  "200", MediaType: "application/json",
			}}),
			testOperation("postDefaultBody", "defaults", "#/paths/~1body/post", []VariantRef{{
				Pointer:   "#/paths/~1body/post/requestBody/content/application~1json",
				MediaType: "application/json",
			}}, []VariantRef{{
				Pointer: "#/paths/~1body/post/responses/204",
				Status:  "204", MediaType: noContentMediaType,
			}}),
		}
		references := map[string][]SourceReference{"defaults": {{
			FromPointer:    "/paths/~1defaults/get/parameters/1/schema/$ref",
			Reference:      "#/components/schemas/LimitWithDefault",
			TargetSourceID: "defaults", TargetPointer: "/components/schemas/LimitWithDefault",
		}}}
		defaults := testNormalize(t, DialectOpenAPI30, map[string][]byte{"defaults": defaultsDocument}, references, operations)
		var getDefaults, postDefaultBody NormalizedOperation
		for _, operation := range defaults.Operations {
			switch operation.OperationID {
			case "getDefaults":
				getDefaults = operation
			case "postDefaultBody":
				postDefaultBody = operation
			}
		}
		seen := map[string]bool{}
		for _, binding := range getDefaults.CommonBindings {
			if binding.Normalized == nil || binding.Normalized.SourceDefault == nil {
				t.Fatalf("binding default disappeared: %#v", binding)
			}
			if contract.AbsentValue().Equal(*binding.Normalized.SourceDefault) {
				t.Fatalf("absent supplied presence collapsed into source default for %s", binding.Normalized.WireName)
			}
			switch binding.Normalized.WireName {
			case "enabled":
				value, ok := binding.Normalized.SourceDefault.Boolean()
				seen["false"] = ok && !value
			case "limit":
				number, ok := binding.Normalized.SourceDefault.ExactNumber()
				seen["zero"] = ok && number.Text == "0"
			case "note":
				seen["null"] = binding.Normalized.SourceDefault.Presence() == contract.PresenceNull
			}
		}
		if !seen["false"] || !seen["zero"] || !seen["null"] {
			t.Fatalf("false/zero/null source defaults = %#v", seen)
		}
		body := postDefaultBody.RequestVariants[0].Bindings
		if len(body) != 1 || body[0].Normalized == nil || body[0].Normalized.SourceDefault == nil ||
			body[0].Normalized.JSONPointer != "" {
			t.Fatalf("whole-body source default = %#v", body)
		}
		var foundNestedDiagnostic, foundRawDefault bool
		for _, diagnostic := range defaults.Diagnostics {
			foundNestedDiagnostic = foundNestedDiagnostic || diagnostic.Code == "unrepresented_schema_default"
		}
		for _, fact := range getDefaults.ResponseVariants[0].SchemaFacts {
			if fact.Default != nil && string(fact.Default.CanonicalJSON) == `"pending"` {
				foundRawDefault = fact.Default.SourceFact.SHA256 != ""
			}
		}
		if !foundNestedDiagnostic || !foundRawDefault {
			t.Fatalf("nested response default was not retained and diagnosed: diagnostics=%#v facts=%#v", defaults.Diagnostics, getDefaults.ResponseVariants[0].SchemaFacts)
		}
	})
}
