package runtime

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// A declared-open object must retain properties the pinned source does not
// enumerate, because the provider may add fields the source has not declared
// yet; a closed object must still refuse them. Both directions are asserted here
// through the real response decoder so a change to either rule fails.
func TestDecodeResponseOpenObjectRetainsUndeclaredProperties(t *testing.T) {
	operation := func(open bool) contract.Operation {
		schema := contract.Schema{
			Type:           contract.SchemaTypeObject,
			OpenProperties: open,
			Properties: map[string]contract.Schema{
				"id": {Type: contract.SchemaTypeInteger},
			},
			Required: []string{"id"},
		}
		return contract.Operation{
			SchemaVersion: contract.OperationSchemaVersion,
			OperationID:   "fixture/open-object",
			Transport:     contract.Transport{Kind: contract.TransportKindHTTP, Method: http.MethodGet, Path: "/fixture"},
			Responses: []contract.Response{{
				Status: "200", MediaType: "application/json", Schema: &schema,
			}},
		}
	}
	document := []byte(`{"id":9001,"undeclared_nested":{"count":42,"absent_optional":null},"undeclared_list":[1,2],"undeclared_text":"kept"}`)

	t.Run("declared open object retains undeclared properties exactly", func(t *testing.T) {
		decoded, err := DecodeResponse(operation(true), 200, http.Header{"Content-Type": []string{"application/json"}}, document)
		if err != nil {
			t.Fatalf("decode open object: %v", err)
		}
		fields, ok := decoded.Result.Data.Fields()
		if !ok {
			t.Fatalf("decoded data = %#v; want an object", decoded.Result.Data)
		}
		nested, present := fields["undeclared_nested"]
		if !present {
			t.Fatalf("undeclared nested property was dropped: %#v", fields)
		}
		nestedFields, ok := nested.Fields()
		if !ok {
			t.Fatalf("undeclared nested property = %#v; want an object", nested)
		}
		count, present := nestedFields["count"]
		if !present {
			t.Fatalf("nested undeclared field was dropped: %#v", nestedFields)
		}
		exact, ok := count.ExactNumber()
		if !ok || exact.Text != "42" || count.Kind() != contract.ValueKindInteger {
			t.Fatalf("nested exact integer = %#v; want exact 42", count)
		}
		if presence := nestedFields["absent_optional"].Presence(); presence != contract.PresenceNull {
			t.Fatalf("explicit null presence = %q; want null", presence)
		}
		list, present := fields["undeclared_list"]
		if !present {
			t.Fatalf("undeclared array property was dropped: %#v", fields)
		}
		elements, ok := list.Elements()
		if !ok || len(elements) != 2 {
			t.Fatalf("undeclared array = %#v; want two elements", list)
		}
		encoded, err := json.Marshal(decoded.Result)
		if err != nil {
			t.Fatal(err)
		}
		var roundTrip struct {
			Data struct {
				Fields map[string]json.RawMessage `json:"fields"`
			} `json:"data"`
		}
		if err := json.Unmarshal(encoded, &roundTrip); err != nil {
			t.Fatal(err)
		}
		if _, present := roundTrip.Data.Fields["undeclared_text"]; !present {
			t.Fatalf("published result lost an undeclared property: %s", encoded)
		}
	})

	t.Run("closed object still refuses undeclared properties", func(t *testing.T) {
		if _, err := DecodeResponse(operation(false), 200, http.Header{"Content-Type": []string{"application/json"}}, document); err == nil {
			t.Fatal("closed object accepted an undeclared provider property")
		}
	})
}

// A composition is open only when every member of it is open. One closed allOf
// component closes the whole composition, and every declared additional
// property schema in the composition applies to an undeclared field. The
// canonical typed-value matcher is the oracle for both directions.
func TestDecodeResponseCompositionOpennessMatchesCanonicalMatcher(t *testing.T) {
	composition := func(componentOpen bool, componentAdditional *contract.Schema) contract.Schema {
		component := contract.Schema{
			Type:           contract.SchemaTypeObject,
			OpenProperties: componentOpen,
			Properties: map[string]contract.Schema{
				"name": {Type: contract.SchemaTypeString},
			},
			Required:             []string{"name"},
			AdditionalProperties: componentAdditional,
		}
		return contract.Schema{
			Type:           contract.SchemaTypeObject,
			OpenProperties: true,
			Properties: map[string]contract.Schema{
				"id": {Type: contract.SchemaTypeInteger},
			},
			Required: []string{"id"},
			AllOf:    []contract.Schema{component},
		}
	}
	operation := func(schema contract.Schema) contract.Operation {
		return contract.Operation{
			SchemaVersion: contract.OperationSchemaVersion,
			OperationID:   "fixture/open-composition",
			Transport:     contract.Transport{Kind: contract.TransportKindHTTP, Method: http.MethodGet, Path: "/fixture"},
			Responses: []contract.Response{{
				Status: "200", MediaType: "application/json", Schema: &schema,
			}},
		}
	}
	document := []byte(`{"id":9001,"name":"x","extra":"kept"}`)
	header := http.Header{"Content-Type": []string{"application/json"}}

	// The oracle: the same document decoded exactly, then matched by the shared
	// canonical matcher against the same composition.
	canonicalValue := func(t *testing.T, schema contract.Schema) contract.PresentValue {
		t.Helper()
		open := schema
		open.OpenProperties = true
		open.AllOf = nil
		decoded, err := DecodeResponse(operation(open), 200, header, document)
		if err != nil {
			t.Fatalf("decode the exact fixture document: %v", err)
		}
		return decoded.Result.Data
	}

	t.Run("a closed component refuses an undeclared field", func(t *testing.T) {
		schema := composition(false, nil)
		if err := contract.ValidateValueAgainstSchema(canonicalValue(t, schema), schema); err == nil {
			t.Fatal("the canonical matcher accepted a document a closed component forbids")
		}
		if _, err := DecodeResponse(operation(schema), 200, header, document); err == nil {
			t.Fatal("an open outer node admitted an undeclared field a closed component forbids")
		}
	})

	t.Run("a component additional property schema applies to the undeclared field", func(t *testing.T) {
		integer := contract.Schema{Type: contract.SchemaTypeInteger}
		schema := composition(true, &integer)
		if err := contract.ValidateValueAgainstSchema(canonicalValue(t, schema), schema); err == nil {
			t.Fatal("the canonical matcher accepted a string where the component declares an integer")
		}
		if _, err := DecodeResponse(operation(schema), 200, header, document); err == nil {
			t.Fatal("an undeclared field ignored the component's declared additional property schema")
		}
	})

	t.Run("a fully open composition still retains the undeclared field", func(t *testing.T) {
		schema := composition(true, nil)
		if err := contract.ValidateValueAgainstSchema(canonicalValue(t, schema), schema); err != nil {
			t.Fatalf("the canonical matcher refused a fully open composition: %v", err)
		}
		decoded, err := DecodeResponse(operation(schema), 200, header, document)
		if err != nil {
			t.Fatalf("decode a fully open composition: %v", err)
		}
		fields, ok := decoded.Result.Data.Fields()
		if !ok {
			t.Fatalf("decoded data = %#v; want an object", decoded.Result.Data)
		}
		if _, present := fields["extra"]; !present {
			t.Fatalf("a fully open composition dropped the undeclared field: %#v", fields)
		}
	})
}
