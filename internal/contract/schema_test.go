package contract

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestStrictContractShape(t *testing.T) {
	document := validOperationDocumentForTest()
	operation := decodeOperationForTest(t, document)

	t.Run("V01_valid_shape_preserves_typed_bindings_and_complete_schema", func(t *testing.T) {
		if operation.SchemaVersion != "pm.operation/v1" || operation.OperationID != "issues/list-for-repo" {
			t.Fatalf("decoded operation identity = %q %q", operation.SchemaVersion, operation.OperationID)
		}
		if len(operation.SourceRefs) != 1 || operation.SourceRefs[0].SourceID != "github-rest-api" || operation.SourceRefs[0].Pointer != "#/paths/~1repos~1{owner}~1{repo}~1issues/get" {
			t.Fatalf("source references = %#v; want the exact declared source location", operation.SourceRefs)
		}
		if operation.Transport.Kind != "http" || operation.Transport.Method != "GET" || operation.Transport.Path != "/repos/{owner}/{repo}/issues" {
			t.Fatalf("transport = %#v; want the complete declared HTTP binding", operation.Transport)
		}
		if len(operation.Inputs) != 3 {
			t.Fatalf("decoded inputs = %d; want 3", len(operation.Inputs))
		}
		owner := operation.Inputs[0]
		if owner.FieldID != "owner" || owner.WireName != "owner" || owner.Location != FieldLocationPath || owner.JSONPointer != "/path/owner" {
			t.Fatalf("owner binding = %#v; want exact path binding", owner)
		}
		if !owner.Required || owner.Nullable || owner.Schema.Type != SchemaTypeString {
			t.Fatalf("owner requiredness/type = required:%t nullable:%t type:%q", owner.Required, owner.Nullable, owner.Schema.Type)
		}
		if owner.Style != "simple" || owner.Explode || owner.AllowReserved || owner.Sensitivity != SensitivityPublic {
			t.Fatalf("owner wire metadata = style:%q explode:%t allowReserved:%t sensitivity:%q", owner.Style, owner.Explode, owner.AllowReserved, owner.Sensitivity)
		}
		limit := operation.Inputs[1]
		if limit.Required || limit.Nullable || limit.SourceDefault == nil {
			t.Fatalf("limit binding = %#v; want optional non-null binding with source default", limit)
		}
		defaultNumber, ok := limit.SourceDefault.ExactNumber()
		if !ok || defaultNumber.Text != "4" || defaultNumber.Precision != 1 || defaultNumber.Scale != 0 {
			t.Fatalf("limit source default = %#v, %t; want exact integer 4 separate from supplied presence", defaultNumber, ok)
		}
		if limit.Style != "form" || !limit.Explode || limit.AllowReserved || limit.Location != FieldLocationQuery {
			t.Fatalf("limit wire metadata = %#v; want exact query form/explode binding", limit)
		}
		credential := operation.Inputs[2]
		if credential.WireName != "Authorization" || credential.Sensitivity != SensitivitySecret {
			t.Fatalf("credential binding = %#v; want metadata-only secret binding", credential)
		}

		if len(operation.Responses) != 1 || operation.Responses[0].MediaType != "application/json" {
			t.Fatalf("responses = %#v; want one JSON response", operation.Responses)
		}
		response := operation.Responses[0].Schema
		if response.Type != SchemaTypeArray || response.Items == nil || response.Items.Type != SchemaTypeObject {
			t.Fatalf("response schema = %#v; want array of complete objects", response)
		}
		record := response.Items
		wantRequired := []string{"id", "number", "title", "labels", "Password", "payload", "composed"}
		if !reflect.DeepEqual(record.Required, wantRequired) {
			t.Fatalf("response required fields = %v; want %v", record.Required, wantRequired)
		}
		if len(record.Properties) != 9 {
			t.Fatalf("response properties = %d; want the complete 9-field fixture, not a display subset", len(record.Properties))
		}
		if password, ok := record.Properties["Password"]; !ok || password.Type != SchemaTypeString {
			t.Fatalf("provider-defined property Password = %#v, %t; exact spelling must be preserved", password, ok)
		}
		if _, wrongCase := record.Properties["password"]; wrongCase {
			t.Fatal("provider-defined property Password was case-folded")
		}
		body := record.Properties["body"]
		if !body.Nullable || body.Type != SchemaTypeString {
			t.Fatalf("nullable body schema = %#v; want nullable string", body)
		}
		payload := record.Properties["payload"]
		if payload.Type != SchemaTypeUnion || len(payload.OneOf) != 2 || payload.OneOf[1].Type != SchemaTypeObject {
			t.Fatalf("union payload schema = %#v; want both declared variants", payload)
		}
		composed := record.Properties["composed"]
		if composed.Type != SchemaTypeObject || len(composed.AllOf) != 2 {
			t.Fatalf("allOf schema = %#v; want both declared components", composed)
		}
		if !reflect.DeepEqual(composed.AllOf[0].Required, []string{"kind"}) || !reflect.DeepEqual(composed.AllOf[1].Required, []string{"active"}) {
			t.Fatalf("allOf requiredness = %#v; want component-local required fields", composed.AllOf)
		}
		if len(operation.CapabilityRequirements) != 2 {
			t.Fatalf("capability requirements = %#v; want two capabilities from one exact FND-03 revision", operation.CapabilityRequirements)
		}
		typedValues := operation.CapabilityRequirements[0]
		fieldBinding := operation.CapabilityRequirements[1]
		if typedValues.Capability != "typed-values" || fieldBinding.Capability != "field-binding" ||
			typedValues.Foundation != fieldBinding.Foundation ||
			typedValues.Foundation.ID != "FND-03" || typedValues.Foundation.Version != "1.0.0" {
			t.Fatalf("capability requirements = %#v; want typed-values and field-binding on FND-03@1.0.0", operation.CapabilityRequirements)
		}
		if len(operation.LaneEligibility) != 1 || operation.LaneEligibility[0].Lane != "direct_read" || !operation.LaneEligibility[0].Eligible {
			t.Fatalf("lane eligibility = %#v; want only declared direct_read eligibility", operation.LaneEligibility)
		}
	})

	t.Run("V01_RFC6901_root_pointer_is_explicit", func(t *testing.T) {
		binding, err := DecodeFieldBinding([]byte(`{
		  "field_id":"payload",
		  "wire_name":"body",
		  "location":"body",
		  "json_pointer":"",
		  "required":true,
		  "nullable":false,
		  "style":"json",
		  "explode":false,
		  "allow_reserved":false,
		  "sensitivity":"public",
		  "schema":{"type":"object","required":[],"properties":{}}
		}`))
		if err != nil {
			t.Fatalf("DecodeFieldBinding(root pointer): %v", err)
		}
		if binding.JSONPointer != "" {
			t.Fatalf("root JSON pointer = %q; want the exact empty RFC 6901 pointer", binding.JSONPointer)
		}
	})

	t.Run("V01_allOf_nullability_must_be_unambiguous", func(t *testing.T) {
		_, err := DecodeSchema([]byte(`{
		  "type":"object",
		  "nullable":true,
		  "required":[],
		  "properties":{},
		  "allOf":[{"type":"object","required":[],"properties":{}}]
		}`))
		if err == nil {
			t.Fatal("schema accepted an allOf component with contradictory nullability")
		}
	})

	t.Run("V01_provider_schema_preserves_empty_JSON_property_names", func(t *testing.T) {
		schema, err := DecodeSchema([]byte(`{
		  "type":"object",
		  "required":[""],
		  "properties":{"":{"type":"string"}}
		}`))
		if err != nil {
			t.Fatalf("DecodeSchema(empty provider property name): %v", err)
		}
		value := NewObjectValue(map[string]PresentValue{"": NewStringValue("exact provider value")})
		if err := value.Validate(); err != nil {
			t.Fatalf("Validate(empty provider property name): %v", err)
		}
		if !valueMatchesSchema(value, schema) {
			t.Fatal("empty provider property name did not match its admitted schema")
		}
		bindingDocument := []byte(`{
		  "field_id":"empty-provider-property",
		  "wire_name":"",
		  "location":"record",
		  "json_pointer":"/",
		  "required":true,
		  "nullable":false,
		  "style":"json",
		  "explode":false,
		  "allow_reserved":false,
		  "sensitivity":"public",
		  "schema":{"type":"string"}
		}`)
		binding, err := DecodeFieldBinding(bindingDocument)
		if err != nil {
			t.Fatalf("DecodeFieldBinding(empty provider property name): %v", err)
		}
		if binding.WireName != "" || binding.JSONPointer != "/" {
			t.Fatalf("empty provider binding = %#v; want exact wire name and JSON pointer", binding)
		}
		withoutWireName := bytes.Replace(bindingDocument, []byte(`"wire_name":"",`), nil, 1)
		if _, err := DecodeFieldBinding(withoutWireName); err == nil {
			t.Fatal("missing wire-name presence was confused with an explicitly empty provider property name")
		}
		withoutPointer := bytes.Replace(bindingDocument, []byte(`"json_pointer":"/",`), nil, 1)
		if _, err := DecodeFieldBinding(withoutPointer); err == nil {
			t.Fatal("missing JSON-pointer presence was confused with the explicit RFC 6901 root pointer")
		}
	})

	t.Run("V01_exact_foundation_revisions_can_coexist", func(t *testing.T) {
		supported := append(supportedFoundationsForTest(), FoundationContract{ID: "FND-03", Version: "2.0.0"})
		if _, err := DecodeOperation(document, supported); err != nil {
			t.Fatalf("DecodeOperation(coexisting revisions): %v", err)
		}
	})

	t.Run("V01_foundation_dependency_closure_is_exact_and_acyclic", func(t *testing.T) {
		withClosure := []FoundationContract{
			{ID: "FND-03", Version: "1.0.0", Dependencies: []FoundationRef{{ID: "FND-04", Version: "1.0.0"}}},
			{ID: "FND-04", Version: "1.0.0"},
		}
		if _, err := DecodeOperation(document, withClosure); err != nil {
			t.Fatalf("DecodeOperation(complete dependency closure): %v", err)
		}
		if _, err := DecodeOperation(document, withClosure[:1]); err == nil {
			t.Fatal("operation accepted a missing exact foundation dependency")
		}
		withClosure[1].Dependencies = []FoundationRef{{ID: "FND-03", Version: "1.0.0"}}
		if _, err := DecodeOperation(document, withClosure); err == nil {
			t.Fatal("operation accepted a cyclic foundation dependency closure")
		}
	})

	t.Run("V01_foundation_contract_has_explicit_exact_revision", func(t *testing.T) {
		foundation, err := DecodeFoundationContract([]byte(`{"schema_version":"pm.foundation/v1","id":"FND-03","version":"1.0.0","name":"Typed values/serialization","owner":"contract","dependencies":[]}`))
		if err != nil {
			t.Fatalf("DecodeFoundationContract(valid): %v", err)
		}
		if foundation.ID != "FND-03" || foundation.Version != "1.0.0" || foundation.Dependencies == nil {
			t.Fatalf("foundation contract = %#v; want explicit FND-03@1.0.0 with empty dependency closure", foundation)
		}
		encoded, err := json.Marshal(foundation)
		if err != nil {
			t.Fatal(err)
		}
		roundTrip, err := DecodeFoundationContract(encoded)
		if err != nil || !reflect.DeepEqual(roundTrip, foundation) {
			t.Fatalf("foundation wire round trip changed the explicit dependency closure: %#v, %v", roundTrip, err)
		}
		if _, err := DecodeFoundationContract([]byte(`{"schema_version":"pm.foundation/v1","id":"FND-03","version":"1.0.0","name":"Typed values/serialization","owner":"contract"}`)); err == nil {
			t.Fatal("foundation contract accepted an absent dependency closure")
		}
	})

	t.Run("V01_foundation_source_requires_supported_document_revision", func(t *testing.T) {
		for _, revision := range []string{`""`, `null`, `"pm.foundation/v2"`} {
			document := []byte(`{"schema_version":` + revision + `,"id":"FND-03","version":"1.0.0","name":"","owner":"","dependencies":[]}`)
			if _, err := DecodeFoundationContract(document); err == nil {
				t.Fatalf("source document accepted revision %s", revision)
			}
		}
	})

	invalidUTF8Document := append([]byte(nil), document...)
	invalidUTF8Index := bytes.Index(invalidUTF8Document, []byte("issues/list-for-repo"))
	if invalidUTF8Index < 0 {
		t.Fatal("valid fixture has no operation ID marker")
	}
	invalidUTF8Document[invalidUTF8Index] = 0xff
	invalid := []struct {
		name        string
		document    []byte
		foundations []FoundationContract
	}{
		{
			name:        "V01_duplicate_even_when_equal",
			document:    replaceOperationDocumentForTest(t, document, `"schema_version":"pm.operation/v1",`, `"schema_version":"pm.operation/v1","schema_version":"pm.operation/v1",`),
			foundations: supportedFoundationsForTest(),
		},
		{
			name:        "V01_wrong_case",
			document:    replaceOperationDocumentForTest(t, document, `"operation_id":`, `"OperationID":`),
			foundations: supportedFoundationsForTest(),
		},
		{
			name:        "V01_unknown_contract_field",
			document:    replaceOperationDocumentForTest(t, document, `"operation_id":"issues/list-for-repo",`, `"operation_id":"issues/list-for-repo","unexpected":true,`),
			foundations: supportedFoundationsForTest(),
		},
		{
			name:        "V01_secret_public_value",
			document:    replaceOperationDocumentForTest(t, document, `"sensitivity":"secret","schema":{"type":"string"}`, `"sensitivity":"secret","schema":{"type":"string"},"source_default":{"presence":"value","type":"string","text":"synthetic-secret"}`),
			foundations: supportedFoundationsForTest(),
		},
		{
			name:        "V01_unsupported_operation_version",
			document:    replaceOperationDocumentForTest(t, document, `pm.operation/v1`, `pm.operation/v2`),
			foundations: supportedFoundationsForTest(),
		},
		{
			name:        "V01_dangling_foundation_revision",
			document:    document,
			foundations: []FoundationContract{{ID: "FND-03", Version: "2.0.0"}},
		},
		{
			name:        "V01_missing_foundation_implementation",
			document:    document,
			foundations: nil,
		},
		{
			name:        "V01_wrong_json_type",
			document:    replaceOperationDocumentForTest(t, document, `"operation_id":"issues/list-for-repo"`, `"operation_id":17`),
			foundations: supportedFoundationsForTest(),
		},
		{
			name:        "V01_null_optional_transport_field_is_not_treated_as_absent",
			document:    replaceOperationDocumentForTest(t, document, `"transport":{"kind":"http","method":"GET","path":"/repos/{owner}/{repo}/issues"}`, `"transport":{"kind":"http","method":"GET","path":"/repos/{owner}/{repo}/issues","operation":null}`),
			foundations: supportedFoundationsForTest(),
		},
		{
			name:        "V01_lane_disposition_must_be_explicit",
			document:    replaceOperationDocumentForTest(t, document, `"lane_eligibility":[{"lane":"direct_read","eligible":true}]`, `"lane_eligibility":[{"lane":"direct_read","reason":"missing explicit disposition"}]`),
			foundations: supportedFoundationsForTest(),
		},
		{
			name:        "V01_malformed_JSON_pointer_escape",
			document:    replaceOperationDocumentForTest(t, document, `"/query/per_page"`, `"/query/~2per_page"`),
			foundations: supportedFoundationsForTest(),
		},
		{
			name:        "V01_unsupported_schema_shape_is_explicitly_refused",
			document:    replaceOperationDocumentForTest(t, document, `"type":"string","nullable":true`, `"type":"string","nullable":true,"not":{"type":"null"}`),
			foundations: supportedFoundationsForTest(),
		},
		{
			name:        "V01_empty_structural_keyword_is_not_treated_as_absent",
			document:    replaceOperationDocumentForTest(t, document, `"type":"string","nullable":true`, `"type":"string","nullable":true,"properties":{}`),
			foundations: supportedFoundationsForTest(),
		},
		{
			name:        "V01_invalid_UTF8_is_not_replaced",
			document:    invalidUTF8Document,
			foundations: supportedFoundationsForTest(),
		},
		{
			name:        "V01_unpaired_UTF16_surrogate_is_not_replaced",
			document:    replaceOperationDocumentForTest(t, document, `issues/list-for-repo`, `issues/\ud800`),
			foundations: supportedFoundationsForTest(),
		},
		{
			name:        "V01_malformed_document",
			document:    document[:len(document)-1],
			foundations: supportedFoundationsForTest(),
		},
		{
			name:        "V01_trailing_document",
			document:    append(append([]byte(nil), document...), []byte(` {}`)...),
			foundations: supportedFoundationsForTest(),
		},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeOperation(test.document, test.foundations); err == nil {
				t.Fatal("DecodeOperation accepted an invalid contract")
			}
		})
	}
}

func TestPublishedSourceSchemas(t *testing.T) {
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	paths := make(map[string]string)
	compiled := make(map[string]*jsonschema.Schema)
	for _, name := range []string{"foundation", "field-binding"} {
		path, err := filepath.Abs(filepath.Join("..", "..", "schemas", name+".schema.json"))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := compiler.AddResource(path, document); err != nil {
			t.Fatal(err)
		}
		paths[name] = path
		compiled[name], err = compiler.Compile(path)
		if err != nil {
			t.Fatalf("%s schema cannot execute with the selected validator: %v", name, err)
		}
	}
	check := func(name string, schema *jsonschema.Schema, value any) {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(document); err != nil {
			t.Fatalf("%s typed wire value does not satisfy the published schema: %v", name, err)
		}
	}
	foundation, err := DecodeFoundationContract([]byte(`{"schema_version":"pm.foundation/v1","id":"FND-03","version":"1.0.0","name":"Typed values","owner":"contract","dependencies":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	check("foundation", compiled["foundation"], foundation)
	for _, binding := range decodeOperationForTest(t, validOperationDocumentForTest()).Inputs {
		check("field-binding", compiled["field-binding"], binding)
	}
	valueSchema, err := compiler.Compile(paths["field-binding"] + "#/$defs/presentValue")
	if err != nil {
		t.Fatal(err)
	}
	check("exact instant", valueSchema, mustInstantValueForTest(t, "2026-09-16T00:00:00.1234567891Z"))
	variant, err := NewUnionValue(strings.Repeat("v", 512), NullValue())
	if err != nil {
		t.Fatal(err)
	}
	check("bounded union variant", valueSchema, variant)
	if _, err := NewUnionValue(strings.Repeat("v", 513), NullValue()); err == nil {
		t.Fatal("union constructor accepted a variant outside its published identity bound")
	}
	check("negative fraction", valueSchema, mustDecimalValueForTest(t, "-0.1", 2, 1))
	negativeZero, err := jsonschema.UnmarshalJSON(strings.NewReader(`{"presence":"value","type":"decimal","exact":"-0.000","precision":4,"scale":3}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := valueSchema.Validate(negativeZero); err == nil {
		t.Fatal("published decimal schema accepted noncanonical negative zero")
	}
}

func decodeOperationForTest(t *testing.T, document []byte) Operation {
	t.Helper()
	operation, err := DecodeOperation(document, supportedFoundationsForTest())
	if err != nil {
		t.Fatalf("DecodeOperation(valid fixture): %v", err)
	}
	return operation
}

func supportedFoundationsForTest() []FoundationContract {
	return []FoundationContract{{ID: "FND-03", Version: "1.0.0"}}
}

func replaceOperationDocumentForTest(t *testing.T, document []byte, old, replacement string) []byte {
	t.Helper()
	if strings.Count(string(document), old) != 1 {
		t.Fatalf("fixture marker %q occurs %d times; want exactly once", old, strings.Count(string(document), old))
	}
	return []byte(strings.Replace(string(document), old, replacement, 1))
}

func validOperationDocumentForTest() []byte {
	return []byte(`{
  "schema_version":"pm.operation/v1",
  "operation_id":"issues/list-for-repo",
  "source_refs":[{"source_id":"github-rest-api","pointer":"#/paths/~1repos~1{owner}~1{repo}~1issues/get"}],
  "transport":{"kind":"http","method":"GET","path":"/repos/{owner}/{repo}/issues"},
  "inputs":[
    {"field_id":"owner","wire_name":"owner","location":"path","json_pointer":"/path/owner","required":true,"nullable":false,"style":"simple","explode":false,"allow_reserved":false,"sensitivity":"public","schema":{"type":"string"}},
    {"field_id":"per-page","wire_name":"per_page","location":"query","json_pointer":"/query/per_page","required":false,"nullable":false,"style":"form","explode":true,"allow_reserved":false,"sensitivity":"public","schema":{"type":"integer"},"source_default":{"presence":"value","type":"integer","exact":"4","precision":1,"scale":0}},
    {"field_id":"authorization","wire_name":"Authorization","location":"header","json_pointer":"/headers/Authorization","required":true,"nullable":false,"style":"simple","explode":false,"allow_reserved":false,"sensitivity":"secret","schema":{"type":"string"}}
  ],
  "responses":[{
    "status":"200",
    "media_type":"application/json",
    "schema":{
      "type":"array",
      "items":{
        "type":"object",
        "required":["id","number","title","labels","Password","payload","composed"],
        "properties":{
          "id":{"type":"integer"},
          "number":{"type":"integer"},
          "title":{"type":"string"},
          "body":{"type":"string","nullable":true},
          "labels":{"type":"array","items":{"type":"object","required":["name"],"properties":{"name":{"type":"string"},"color":{"type":"string"}}}},
          "pull_request":{"type":"object","nullable":true,"properties":{"url":{"type":"string"}}},
          "Password":{"type":"string"},
          "payload":{"type":"union","oneOf":[{"type":"string"},{"type":"object","required":["nested"],"properties":{"nested":{"type":"array","items":{"type":"bool"}}}}]},
          "composed":{"type":"object","allOf":[{"type":"object","required":["kind"],"properties":{"kind":{"type":"string"}}},{"type":"object","required":["active"],"properties":{"active":{"type":"bool"}}}]}
        }
      }
    }
  }],
  "capability_requirements":[
    {"capability":"typed-values","foundation":{"id":"FND-03","version":"1.0.0"}},
    {"capability":"field-binding","foundation":{"id":"FND-03","version":"1.0.0"}}
  ],
  "lane_eligibility":[{"lane":"direct_read","eligible":true}]
}`)
}
