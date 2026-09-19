package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

		if len(operation.RequestVariants) != 0 || operation.RequestMediaFieldID != "" {
			t.Fatalf("request variants = %#v, selector %q; want an explicit body-free request", operation.RequestVariants, operation.RequestMediaFieldID)
		}
		if len(operation.Responses) != 1 || operation.Responses[0].MediaType != "application/json" ||
			operation.Responses[0].NoContent || operation.Responses[0].Schema == nil ||
			len(operation.Responses[0].SourceRefs) != 1 {
			t.Fatalf("responses = %#v; want one source-linked JSON body response", operation.Responses)
		}
		response := *operation.Responses[0].Schema
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
		if len(operation.LaneEligibility) != len(canonicalLanes) {
			t.Fatalf("lane eligibility = %#v; want all seven canonical lanes", operation.LaneEligibility)
		}
		wantStatuses := [...]LaneStatus{
			LaneEligible,
			LaneUnsupported,
			LaneUnproven,
			LaneNotApplicable,
			LaneNotApplicable,
			LaneNotApplicable,
			LaneBlocked,
		}
		for index, lane := range canonicalLanes {
			disposition := operation.LaneEligibility[index]
			if disposition.Lane != lane || disposition.Status != wantStatuses[index] || disposition.Reason == "" {
				t.Fatalf("lane eligibility[%d] = %#v; want %q/%q with reason", index, disposition, lane, wantStatuses[index])
			}
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
			document:    replaceOperationDocumentForTest(t, document, `"transport":{"kind":"http","method":"GET","path":"/repos/{owner}/{repo}/issues"}`, `"transport":{"kind":"http","method":"GET","path":"/repos/{owner}/{repo}/issues","statement_kind":null}`),
			foundations: supportedFoundationsForTest(),
		},
		{
			name:        "V01_lane_disposition_must_be_explicit",
			document:    replaceOperationDocumentForTest(t, document, `"lane":"direct_read","status":"eligible","reason":"source-declared-read"`, `"lane":"direct_read","reason":"missing explicit disposition"`),
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

func TestOperationExecutionFacts(t *testing.T) {
	operation := decodeOperationForTest(t, validOperationDocumentForTest())
	if operation.Execution != nil {
		t.Fatalf("undeclared execution facts = %#v; want unknown", operation.Execution)
	}
	records := *operation.Responses[0].Schema
	operation.Responses[0].Schema = &Schema{
		Type: SchemaTypeObject,
		Properties: map[string]Schema{
			"records": records,
			"next":    {Type: SchemaTypeString},
		},
	}
	recordPointer := "/records"
	operation.Responses[0].RecordPointer = &recordPointer
	cursor := operation.Inputs[1]
	cursor.FieldID = "cursor"
	cursor.WireName = "cursor"
	cursor.JSONPointer = "/query/cursor"
	cursor.Schema = Schema{Type: SchemaTypeString}
	cursor.SourceDefault = nil
	operation.Inputs = append(operation.Inputs, cursor)
	credential := operation.Inputs[2]
	witness := ExecutionWitness{
		ProofID: "proof-pagination-source",
		SourceRefs: []SourceRef{{
			SourceID: "github-rest-api",
			Pointer:  "#/paths/~1repos~1{owner}~1{repo}~1issues/get/x-pagination",
		}},
	}
	operation.Execution = &OperationExecution{
		HTTP: &OperationHTTP{
			Origins: []string{"https://api.github.example/v3"},
			Authorization: &OperationAuthorization{
				Required:  true,
				Audience:  "https://api.github.example",
				Placement: FieldLocationHeader,
				WireName:  "Authorization",
				Prefix:    "Bearer ",
				FieldID:   credential.FieldID,
			},
			Headers: []OperationHTTPHeader{{Name: "X-API-Version", Value: "2022-11-28"}},
			Witness: witness,
		},
		Pagination: &OperationPagination{
			Kind:                  OperationPaginationCursor,
			CursorRequestField:    "cursor",
			CursorResponsePointer: "/next",
			Witness:               witness,
		},
		Retry: &OperationRetry{
			Classification: OperationRetryUnsafe,
			HTTPStatuses:   []uint16{429, 502},
			Witness:        witness,
		},
	}
	document, err := json.Marshal(operation)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeOperation(document, supportedFoundationsForTest())
	if err != nil {
		t.Fatalf("DecodeOperation(execution facts): %v", err)
	}
	if decoded.Execution == nil || decoded.Execution.HTTP == nil ||
		decoded.Execution.Pagination == nil || decoded.Execution.Retry == nil ||
		!slices.Equal(decoded.Execution.HTTP.Origins, []string{"https://api.github.example/v3"}) ||
		decoded.Execution.HTTP.Authorization == nil ||
		decoded.Execution.HTTP.Authorization.FieldID != credential.FieldID ||
		decoded.Execution.HTTP.Headers[0] != (OperationHTTPHeader{Name: "X-API-Version", Value: "2022-11-28"}) ||
		decoded.Execution.Pagination.CursorRequestField != "cursor" ||
		decoded.Execution.Pagination.CursorResponsePointer != "/next" ||
		decoded.Execution.Retry.Classification != OperationRetryUnsafe ||
		!slices.Equal(decoded.Execution.Retry.HTTPStatuses, []uint16{429, 502}) {
		t.Fatalf("execution facts changed: %#v", decoded.Execution)
	}

	t.Run("retry classifications preserve unknown unsafe safe and conditional", func(t *testing.T) {
		for _, test := range []struct {
			name           string
			classification OperationRetryClassification
			condition      string
		}{
			{name: "unsafe", classification: OperationRetryUnsafe},
			{name: "safe", classification: OperationRetrySafe},
			{name: "conditional", classification: OperationRetryConditional, condition: "idempotency-key-present"},
		} {
			t.Run(test.name, func(t *testing.T) {
				candidate := operation
				execution := *operation.Execution
				retry := *operation.Execution.Retry
				retry.Classification = test.classification
				retry.Condition = test.condition
				execution.Retry = &retry
				candidate.Execution = &execution
				candidateDocument, marshalErr := json.Marshal(candidate)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				if _, decodeErr := DecodeOperation(candidateDocument, supportedFoundationsForTest()); decodeErr != nil {
					t.Fatalf("DecodeOperation(%s): %v", test.name, decodeErr)
				}
			})
		}
	})

	t.Run("cursor may bind one selected request body variant", func(t *testing.T) {
		candidate := operation
		candidate.Inputs = make([]FieldBinding, 0, len(operation.Inputs)-1)
		for _, input := range operation.Inputs {
			if input.FieldID != cursor.FieldID {
				candidate.Inputs = append(candidate.Inputs, input)
			}
		}
		bodyCursor := cursor
		bodyCursor.Location = FieldLocationBody
		bodyCursor.JSONPointer = "/cursor"
		bodyCursor.Style = "json"
		bodyCursor.Explode = false
		candidate.RequestVariants = []RequestVariant{{
			VariantID: "json",
			MediaType: "application/json",
			Required:  false,
			Schema: Schema{
				Type:       SchemaTypeObject,
				Properties: map[string]Schema{"cursor": {Type: SchemaTypeString}},
			},
			Inputs:     []FieldBinding{bodyCursor},
			SourceRefs: []SourceRef{{SourceID: "github-rest-api", Pointer: "#/requestBody"}},
		}}
		execution := *operation.Execution
		pagination := *operation.Execution.Pagination
		pagination.CursorRequestField = bodyCursor.FieldID
		execution.Pagination = &pagination
		candidate.Execution = &execution
		candidateDocument, marshalErr := json.Marshal(candidate)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		decodedCandidate, decodeErr := DecodeOperation(candidateDocument, supportedFoundationsForTest())
		if decodeErr != nil {
			t.Fatalf("DecodeOperation(body cursor): %v", decodeErr)
		}
		if decodedCandidate.Execution.Pagination.CursorRequestField != bodyCursor.FieldID ||
			len(decodedCandidate.RequestVariants) != 1 ||
			decodedCandidate.RequestVariants[0].Inputs[0].Location != FieldLocationBody {
			t.Fatalf("body cursor facts changed: %#v", decodedCandidate)
		}
	})

	t.Run("execution facts are strict and operation-bound", func(t *testing.T) {
		invalid := []struct {
			name   string
			mutate func(*Operation)
		}{
			{
				name: "empty execution wrapper",
				mutate: func(candidate *Operation) {
					candidate.Execution = &OperationExecution{}
				},
			},
			{
				name: "cursor carries contradictory limit field",
				mutate: func(candidate *Operation) {
					candidate.Execution.Pagination.LimitRequestField = "per-page"
				},
			},
			{
				name: "cursor response pointer is not in a response schema",
				mutate: func(candidate *Operation) {
					candidate.Execution.Pagination.CursorResponsePointer = "/missing"
				},
			},
			{
				name: "conditional retry lacks its admitted condition",
				mutate: func(candidate *Operation) {
					candidate.Execution.Retry.Classification = OperationRetryConditional
				},
			},
			{
				name: "pagination witness is not source-linked",
				mutate: func(candidate *Operation) {
					candidate.Execution.Pagination.Witness.SourceRefs = nil
				},
			},
			{
				name: "relative HTTP origin",
				mutate: func(candidate *Operation) {
					candidate.Execution.HTTP.Origins = []string{"/v3"}
				},
			},
			{
				name: "authorization audience is outside admitted origin",
				mutate: func(candidate *Operation) {
					candidate.Execution.HTTP.Authorization.Audience = "https://attacker.invalid"
				},
			},
			{
				name: "fixed header embeds a credential channel",
				mutate: func(candidate *Operation) {
					candidate.Execution.HTTP.Headers[0].Name = "X-Password"
				},
			},
			{
				name: "authorization field is not secret",
				mutate: func(candidate *Operation) {
					for index := range candidate.Inputs {
						if candidate.Inputs[index].FieldID == credential.FieldID {
							candidate.Inputs[index].Sensitivity = SensitivityPublic
						}
					}
				},
			},
			{
				name: "retry statuses are not sorted",
				mutate: func(candidate *Operation) {
					candidate.Execution.Retry.HTTPStatuses = []uint16{502, 429}
				},
			},
		}
		for _, test := range invalid {
			t.Run(test.name, func(t *testing.T) {
				candidate := operation
				candidate.Inputs = slices.Clone(operation.Inputs)
				execution := *operation.Execution
				pagination := *operation.Execution.Pagination
				pagination.Witness.SourceRefs = append([]SourceRef(nil), operation.Execution.Pagination.Witness.SourceRefs...)
				retry := *operation.Execution.Retry
				httpFact := *operation.Execution.HTTP
				httpFact.Origins = slices.Clone(operation.Execution.HTTP.Origins)
				httpFact.Headers = slices.Clone(operation.Execution.HTTP.Headers)
				httpFact.Witness.SourceRefs = slices.Clone(operation.Execution.HTTP.Witness.SourceRefs)
				authorization := *operation.Execution.HTTP.Authorization
				httpFact.Authorization = &authorization
				execution.HTTP = &httpFact
				retry.Witness.SourceRefs = append([]SourceRef(nil), operation.Execution.Retry.Witness.SourceRefs...)
				execution.Pagination = &pagination
				execution.Retry = &retry
				retry.HTTPStatuses = slices.Clone(operation.Execution.Retry.HTTPStatuses)
				candidate.Execution = &execution
				test.mutate(&candidate)
				candidateDocument, marshalErr := json.Marshal(candidate)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				if _, decodeErr := DecodeOperation(candidateDocument, supportedFoundationsForTest()); decodeErr == nil {
					t.Fatal("DecodeOperation accepted invalid execution facts")
				}
			})
		}

		withUnknownField := replaceOperationDocumentForTest(
			t,
			document,
			`"classification":"unsafe"`,
			`"classification":"unsafe","unexpected":true`,
		)
		if _, decodeErr := DecodeOperation(withUnknownField, supportedFoundationsForTest()); decodeErr == nil {
			t.Fatal("DecodeOperation accepted an unknown retry fact")
		}

		emptyContradiction := replaceOperationDocumentForTest(
			t,
			document,
			`"cursor_response_pointer":"/next"`,
			`"cursor_response_pointer":"/next","offset_request_field":""`,
		)
		if _, decodeErr := DecodeOperation(emptyContradiction, supportedFoundationsForTest()); decodeErr == nil {
			t.Fatal("DecodeOperation accepted an empty contradictory pagination field")
		}
		emptyCondition := replaceOperationDocumentForTest(
			t,
			document,
			`"classification":"unsafe"`,
			`"classification":"unsafe","condition":""`,
		)
		if _, decodeErr := DecodeOperation(emptyCondition, supportedFoundationsForTest()); decodeErr == nil {
			t.Fatal("DecodeOperation accepted an explicit empty condition on an unconditional retry fact")
		}
		emptyStatuses := replaceOperationDocumentForTest(
			t,
			document,
			`"http_statuses":[429,502]`,
			`"http_statuses":[]`,
		)
		if _, decodeErr := DecodeOperation(emptyStatuses, supportedFoundationsForTest()); decodeErr == nil {
			t.Fatal("DecodeOperation accepted explicit empty retry HTTP statuses")
		}
		var invalidHTTP OperationHTTP
		if decodeErr := json.Unmarshal([]byte(
			`{"origins":["https://api.example.test"],"headers":[],"witness":{"proof_id":"source","source_refs":[{"source_id":"root","pointer":"#/servers/0"}]}}`,
		), &invalidHTTP); decodeErr == nil {
			t.Fatal("OperationHTTP accepted explicit empty fixed headers")
		}
		if decodeErr := json.Unmarshal([]byte(
			`{"origins":["https://api.example.test"],"authorization":null,"witness":{"proof_id":"source","source_refs":[{"source_id":"root","pointer":"#/servers/0"}]}}`,
		), &invalidHTTP); decodeErr == nil {
			t.Fatal("OperationHTTP accepted null authorization")
		}

		nullExecution := replaceOperationDocumentForTest(
			t,
			validOperationDocumentForTest(),
			`"responses":`,
			`"execution":null,"responses":`,
		)
		if _, decodeErr := DecodeOperation(nullExecution, supportedFoundationsForTest()); decodeErr == nil {
			t.Fatal("DecodeOperation accepted explicit null as unknown execution facts")
		}
		var nestedNull OperationExecution
		if decodeErr := json.Unmarshal([]byte(`{"pagination":null}`), &nestedNull); decodeErr == nil {
			t.Fatal("OperationExecution accepted explicit null as an absent pagination fact")
		}
	})
}

func TestOperationWireLocations(t *testing.T) {
	operation := decodeOperationForTest(t, validOperationDocumentForTest())
	source := operation.Inputs[0]
	source.FieldID = "source-id"
	source.WireName = "id"
	source.Location = FieldLocationBody
	source.JSONPointer = "/source/id"
	source.Style = "json"
	destination := source
	destination.FieldID = "destination-id"
	destination.JSONPointer = "/destination/id"
	samePointer := destination
	samePointer.WireName = "other-id"
	samePointer.JSONPointer = source.JSONPointer
	root := Schema{
		Type:     SchemaTypeObject,
		Required: []string{"source", "destination"},
		Properties: map[string]Schema{
			"source": {
				Type:       SchemaTypeObject,
				Required:   []string{"id"},
				Properties: map[string]Schema{"id": {Type: SchemaTypeString}},
			},
			"destination": {
				Type:       SchemaTypeObject,
				Required:   []string{"id"},
				Properties: map[string]Schema{"id": {Type: SchemaTypeString}},
			},
		},
	}
	for _, test := range []struct {
		name          string
		inputs        []FieldBinding
		wantDuplicate bool
	}{
		{"distinct_nested_locations", []FieldBinding{source, destination}, false},
		{"same_json_location", []FieldBinding{source, samePointer}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := operation
			candidate.RequestVariants = []RequestVariant{{
				VariantID:  "json",
				MediaType:  "application/json",
				Required:   true,
				Schema:     root,
				Inputs:     test.inputs,
				SourceRefs: []SourceRef{{SourceID: "fixture-source", Pointer: "#/requestBody"}},
			}}
			document, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := DecodeOperation(document, supportedFoundationsForTest())
			if test.wantDuplicate {
				if !errors.Is(err, ErrDuplicateField) {
					t.Fatalf("ambiguous wire location error = %v; want duplicate field", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("distinct nested wire locations rejected: %v", err)
			}
			if !reflect.DeepEqual(decoded.RequestVariants[0].Inputs, test.inputs) {
				t.Fatalf("nested binding locations changed: got %#v; want %#v", decoded.RequestVariants[0].Inputs, test.inputs)
			}
		})
	}

	t.Run("duplicate common transport name", func(t *testing.T) {
		querySource, queryDestination := source, destination
		querySource.Location, queryDestination.Location = FieldLocationQuery, FieldLocationQuery
		querySource.Style, queryDestination.Style = "form", "form"
		candidate := operation
		candidate.Inputs = []FieldBinding{querySource, queryDestination}
		document, err := json.Marshal(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeOperation(document, supportedFoundationsForTest()); !errors.Is(err, ErrDuplicateField) {
			t.Fatalf("duplicate common wire name error = %v; want duplicate field", err)
		}
	})

	t.Run("same body pointer across media variants", func(t *testing.T) {
		selector := FieldBinding{
			FieldID:       "request-content-type",
			WireName:      RequestMediaTypeWireName,
			Location:      FieldLocationHeader,
			JSONPointer:   "/headers/Content-Type",
			Required:      false,
			Nullable:      false,
			Style:         "simple",
			Explode:       false,
			AllowReserved: false,
			Sensitivity:   SensitivityPublic,
			Schema: Schema{
				Type: SchemaTypeString,
				Enum: []PresentValue{
					NewStringValue("application/json"),
					NewStringValue("application/merge-patch+json"),
				},
			},
		}
		second := source
		second.FieldID = "merge-source-id"
		crossMediaRoot := Schema{
			Type:     SchemaTypeObject,
			Required: []string{"source"},
			Properties: map[string]Schema{
				"source": {
					Type:       SchemaTypeObject,
					Required:   []string{"id"},
					Properties: map[string]Schema{"id": {Type: SchemaTypeString}},
				},
			},
		}
		candidate := operation
		candidate.Inputs = append(candidate.Inputs, selector)
		candidate.RequestMediaFieldID = selector.FieldID
		candidate.RequestVariants = []RequestVariant{
			{
				VariantID:  "json",
				MediaType:  "application/json",
				Required:   true,
				Schema:     crossMediaRoot,
				Inputs:     []FieldBinding{source},
				SourceRefs: []SourceRef{{SourceID: "fixture-source", Pointer: "#/requestBody/content/application~1json"}},
			},
			{
				VariantID:  "merge",
				MediaType:  "application/merge-patch+json",
				Required:   true,
				Schema:     crossMediaRoot,
				Inputs:     []FieldBinding{second},
				SourceRefs: []SourceRef{{SourceID: "fixture-source", Pointer: "#/requestBody/content/application~1merge-patch+json"}},
			},
		}
		document, err := json.Marshal(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeOperation(document, supportedFoundationsForTest()); err != nil {
			t.Fatalf("same pointer across distinct media variants rejected: %v", err)
		}
	})
}
func TestResponseVariantsAndPostgreSQLTransport(t *testing.T) {
	operation := decodeOperationForTest(t, validOperationDocumentForTest())
	source := []SourceRef{{SourceID: "fixture-source", Pointer: "#/responses"}}
	recordPointer := "/record"
	operation.Responses = []Response{
		{
			Status:    "201",
			NoContent: false,
			MediaType: "application/json",
			Schema: &Schema{
				Type: SchemaTypeObject,
				Properties: map[string]Schema{
					"record": {Type: SchemaTypeObject, Properties: map[string]Schema{"id": {Type: SchemaTypeString}}},
				},
			},
			SourceRefs:    source,
			RecordPointer: &recordPointer,
		},
		{
			Status:     "204",
			NoContent:  true,
			SourceRefs: source,
		},
		{
			Status:     "default",
			NoContent:  false,
			MediaType:  "application/problem+json",
			Schema:     &Schema{Type: SchemaTypeObject, Properties: map[string]Schema{"code": {Type: SchemaTypeString}}},
			SourceRefs: source,
		},
	}
	if err := operation.Validate(supportedFoundationsForTest()); err != nil {
		t.Fatalf("201/204/default response variants: %v", err)
	}

	responseDocument, err := json.Marshal(operation)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := DecodeOperation(responseDocument, supportedFoundationsForTest())
	if err != nil {
		t.Fatalf("decode response variants after wire round trip: %v", err)
	}
	if roundTrip.Responses[0].RecordPointer == nil || *roundTrip.Responses[0].RecordPointer != recordPointer ||
		roundTrip.Responses[1].RecordPointer != nil || !roundTrip.Responses[1].NoContent {
		t.Fatalf("response pointer/no-content presence changed across wire round trip: %#v", roundTrip.Responses)
	}
	invalidNoContent := operation
	invalidNoContent.Responses = []Response{{
		Status:     "204",
		NoContent:  true,
		MediaType:  "application/json",
		Schema:     &Schema{Type: SchemaTypeObject, Properties: map[string]Schema{}},
		SourceRefs: source,
	}}
	if err := invalidNoContent.Validate(supportedFoundationsForTest()); err == nil {
		t.Fatal("no-content response accepted body media and schema")
	}
	invalidStatus := operation
	invalidStatus.Responses = []Response{{
		Status:     "20",
		NoContent:  true,
		SourceRefs: source,
	}}
	if err := invalidStatus.Validate(supportedFoundationsForTest()); err == nil {
		t.Fatal("HTTP response accepted a noncanonical status")
	}

	postgres := operation
	postgres.Transport = Transport{
		Kind:            TransportKindPostgreSQL,
		StatementKind:   PostgreSQLStatementSelect,
		RelationBinding: "fixture-relation",
	}
	postgres.Inputs = []FieldBinding{}
	postgres.Responses = []Response{{
		Status:     "success",
		NoContent:  false,
		MediaType:  "application/json",
		Schema:     &Schema{Type: SchemaTypeArray, Items: &Schema{Type: SchemaTypeObject, Properties: map[string]Schema{}}},
		SourceRefs: source,
	}}
	if err := postgres.Validate(supportedFoundationsForTest()); err != nil {
		t.Fatalf("canonical PostgreSQL typed transport: %v", err)
	}
	encoded, err := json.Marshal(postgres.Transport)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"kind":"postgresql"`) ||
		!strings.Contains(string(encoded), `"statement_kind":"select"`) ||
		!strings.Contains(string(encoded), `"relation_binding":"fixture-relation"`) ||
		strings.Contains(string(encoded), `"operation":`) {
		t.Fatalf("PostgreSQL transport wire shape = %s", encoded)
	}
}

func TestPublishedSourceSchemas(t *testing.T) {
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	paths := make(map[string]string)
	compiled := make(map[string]*jsonschema.Schema)
	for _, name := range []string{"foundation", "field-binding", "semantics", "bundle"} {
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
		resource := "https://polymetrics.ai/schemas/" + name + ".schema.json"
		if err := compiler.AddResource(resource, document); err != nil {
			t.Fatal(err)
		}
		paths[name] = resource
		compiled[name], err = compiler.Compile(resource)
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
	checkInvalid := func(name string, schema *jsonschema.Schema, value any) {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(document); err == nil {
			t.Fatalf("%s unexpectedly satisfied the published schema", name)
		}
	}
	foundation, err := DecodeFoundationContract([]byte(`{"schema_version":"pm.foundation/v1","id":"FND-03","version":"1.0.0","name":"Typed values","owner":"contract","dependencies":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	check("foundation", compiled["foundation"], foundation)
	semanticsWitness := map[string]any{
		"proof_id": "source-reviewed-execution",
		"source_refs": []map[string]any{{
			"source_id": "github-rest-api",
			"pointer":   "#/paths/~1repos~1{owner}~1{repo}~1issues/get",
		}},
	}
	check("semantics", compiled["semantics"], map[string]any{
		"schema_version": "pm.semantics/v1",
		"connector_id":   "github",
		"source_digest":  "sha256:" + strings.Repeat("1", 64),
		"revision":       "sha256:" + strings.Repeat("2", 64),
		"operations": []map[string]any{{
			"operation_id": "issues/list-for-repo",
			"source_refs":  semanticsWitness["source_refs"],
			"http": map[string]any{
				"origins": []string{"https://api.github.example/v3"},
				"authorization": map[string]any{
					"required":  false,
					"audience":  "https://api.github.example",
					"placement": "header",
					"wire_name": "Authorization",
					"prefix":    "Bearer ",
					"field_id":  "auth.github-token",
				},
				"headers": []map[string]any{{"name": "X-API-Version", "value": "2022-11-28"}},
				"witness": semanticsWitness,
			},
			"record_extraction": map[string]any{
				"records_pointer": "/records",
				"witness":         semanticsWitness,
			},
			"pagination": map[string]any{
				"kind":                    "cursor",
				"cursor_request_field":    "cursor",
				"cursor_response_pointer": "/next",
				"witness":                 semanticsWitness,
			},
			"retry": map[string]any{
				"classification": "unsafe",
				"http_statuses":  []uint16{429, 502},
				"witness":        semanticsWitness,
			},
			"lanes": []any{},
		}},
	})
	check("semantics", compiled["semantics"], map[string]any{
		"schema_version": "pm.semantics/v1",
		"connector_id":   "github",
		"source_digest":  "sha256:" + strings.Repeat("1", 64),
		"revision":       "sha256:" + strings.Repeat("2", 64),
		"operations": []map[string]any{{
			"operation_id": "issues/list-for-repo",
			"source_refs":  semanticsWitness["source_refs"],
			"pagination": map[string]any{
				"kind":                         "link",
				"link_request_field":           "query.page",
				"link_response_header":         "Link",
				"limit_request_field":          "query.per-page",
				"link_routes":                  []string{"/repositories/{repository_id}/issues"},
				"link_record_identity_pointer": "/repository_url",
				"witness":                      semanticsWitness,
			},
			"lanes": []any{},
		}},
	})
	checkInvalid("semantics", compiled["semantics"], map[string]any{
		"schema_version": "pm.semantics/v1",
		"connector_id":   "github",
		"source_digest":  "sha256:" + strings.Repeat("1", 64),
		"revision":       "sha256:" + strings.Repeat("2", 64),
		"operations": []map[string]any{{
			"operation_id": "issues/list-for-repo",
			"source_refs":  semanticsWitness["source_refs"],
			"pagination": map[string]any{
				"kind":                 "link",
				"link_request_field":   "query.page",
				"link_response_header": "Link",
				"link_routes":          []string{"/repositories/{repository_id}/issues?state=open"},
				"witness":              semanticsWitness,
			},
			"lanes": []any{},
		}},
	})
	operation := decodeOperationForTest(t, validOperationDocumentForTest())
	for _, binding := range operation.Inputs {
		check("field-binding", compiled["field-binding"], binding)
	}
	safeOperation := operation
	safeOperation.Inputs = append([]FieldBinding(nil), operation.Inputs[:2]...)
	safeOperation.Execution = &OperationExecution{
		HTTP: &OperationHTTP{
			Origins: []string{"https://api.github.example/v3"},
			Headers: []OperationHTTPHeader{{Name: "X-API-Version", Value: "2022-11-28"}},
			Witness: ExecutionWitness{
				ProofID:    "published-schema-http-proof",
				SourceRefs: []SourceRef{{SourceID: "github-rest-api", Pointer: "#/servers/0"}},
			},
		},
		Retry: &OperationRetry{
			Classification: OperationRetryUnsafe,
			HTTPStatuses:   []uint16{429, 502},
			Witness: ExecutionWitness{
				ProofID:    "published-schema-retry-proof",
				SourceRefs: []SourceRef{{SourceID: "github-rest-api", Pointer: "#/x-pm-retry"}},
			},
		},
	}
	if err := safeOperation.Validate(supportedFoundationsForTest()); err != nil {
		t.Fatalf("safe operation fixture: %v", err)
	}
	command := Command{
		Name: "issues-list-for-repo", OperationID: safeOperation.OperationID,
		Fields: []CommandField{
			{
				Flag: "--owner", FieldID: "owner", Location: FieldLocationPath,
				Required: true, Sensitive: false,
			},
			{
				Flag: "--per-page", FieldID: "per-page", Location: FieldLocationQuery,
				Required: false, Sensitive: false,
			},
		},
	}
	if err := command.Validate(); err != nil {
		t.Fatalf("command fixture: %v", err)
	}
	unavailableLanes := append([]LaneEligibility(nil), safeOperation.LaneEligibility...)
	for index := range unavailableLanes {
		if unavailableLanes[index].Status == LaneEligible {
			unavailableLanes[index].Status = LaneBlocked
			unavailableLanes[index].Reason = "open object response cannot enter the executable contract"
		}
	}
	unavailable := UnavailableOperation{
		OperationID: "issues/get-open-shape",
		SourceRefs: []SourceRef{{
			SourceID: "github-rest-api", Pointer: "#/paths/~1repos~1{owner}~1{repo}~1issues~1open/get",
		}},
		DiagnosticCodes: []string{"open_object_shape", "unsupported_response_metadata"},
		LaneEligibility: unavailableLanes,
	}
	if err := unavailable.Validate(); err != nil {
		t.Fatalf("unavailable operation fixture: %v", err)
	}
	unsorted := unavailable
	unsorted.DiagnosticCodes = []string{"unsupported_response_metadata", "open_object_shape"}
	if err := unsorted.Validate(); err == nil {
		t.Fatal("unavailable operation accepted noncanonical diagnostic order")
	}
	executableLane := unavailable
	executableLane.LaneEligibility = append([]LaneEligibility(nil), unavailable.LaneEligibility...)
	executableLane.LaneEligibility[0].Status = LaneEligible
	if err := executableLane.Validate(); err == nil {
		t.Fatal("unavailable operation accepted an executable lane")
	}
	bundleValue := map[string]any{
		"schema_version":         "pm.bundle/v1",
		"connector_id":           "github",
		"source_digest":          "sha256:" + strings.Repeat("1", 64),
		"semantics_revision":     "sha256:" + strings.Repeat("2", 64),
		"operations":             []Operation{safeOperation},
		"commands":               []Command{command},
		"unavailable_operations": []UnavailableOperation{unavailable},
	}
	check("bundle", compiled["bundle"], bundleValue)
	eligibleUnavailable := make(map[string]any, len(bundleValue))
	for name, value := range bundleValue {
		eligibleUnavailable[name] = value
	}
	eligibleUnavailable["unavailable_operations"] = []UnavailableOperation{executableLane}
	checkInvalid("bundle with executable unavailable lane", compiled["bundle"], eligibleUnavailable)
	executableOnly := make(map[string]any, len(bundleValue))
	for name, value := range bundleValue {
		executableOnly[name] = value
	}
	executableOnly["unavailable_operations"] = []UnavailableOperation{}
	check("executable-only bundle", compiled["bundle"], executableOnly)
	unavailableOnly := make(map[string]any, len(bundleValue))
	for name, value := range bundleValue {
		unavailableOnly[name] = value
	}
	unavailableOnly["operations"] = []Operation{}
	unavailableOnly["commands"] = []Command{}
	check("unavailable-only bundle", compiled["bundle"], unavailableOnly)
	for _, missing := range []string{"commands", "unavailable_operations"} {
		incomplete := make(map[string]any, len(bundleValue)-1)
		for name, value := range bundleValue {
			if name != missing {
				incomplete[name] = value
			}
		}
		checkInvalid("bundle missing "+missing, compiled["bundle"], incomplete)
	}
	emptyInventory := make(map[string]any, len(bundleValue))
	for name, value := range bundleValue {
		emptyInventory[name] = value
	}
	emptyInventory["operations"] = []Operation{}
	emptyInventory["commands"] = []Command{}
	emptyInventory["unavailable_operations"] = []UnavailableOperation{}
	checkInvalid("bundle with empty combined inventory", compiled["bundle"], emptyInventory)
	minimum := mustIntegerValueForTest(t, "0")
	sourceDefault := mustIntegerValueForTest(t, "4")
	maximum := mustIntegerValueForTest(t, "10")
	constrained := operation.Inputs[1]
	constrained.Schema = Schema{
		Type:    SchemaTypeInteger,
		Enum:    []PresentValue{minimum, sourceDefault, maximum},
		Minimum: &minimum,
		Maximum: &maximum,
	}
	if err := constrained.Validate(); err != nil {
		t.Fatalf("constrained field binding: %v", err)
	}
	check("field-binding constraints", compiled["field-binding"], constrained)
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
  "request_variants":[],
  "responses":[{
    "status":"200",
    "no_content":false,
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
    },
    "source_refs":[{"source_id":"github-rest-api","pointer":"#/paths/~1repos~1{owner}~1{repo}~1issues/get/responses/200"}]
  }],
  "capability_requirements":[
    {"capability":"typed-values","foundation":{"id":"FND-03","version":"1.0.0"}},
    {"capability":"field-binding","foundation":{"id":"FND-03","version":"1.0.0"}}
  ],
  "lane_eligibility":[
    {"lane":"direct_read","status":"eligible","reason":"source-declared-read"},
    {"lane":"direct_write","status":"unsupported","reason":"source-does-not-declare-write"},
    {"lane":"etl","status":"unproven","reason":"runtime-proof-not-yet-established"},
    {"lane":"reverse_etl","status":"not_applicable","reason":"read-operation-has-no-destination-write"},
    {"lane":"binary_download","status":"not_applicable","reason":"response-is-not-binary"},
    {"lane":"binary_upload","status":"not_applicable","reason":"request-is-not-binary"},
    {"lane":"sync_transport","status":"blocked","reason":"sync-semantics-are-not-declared"}
  ]
}`)
}
