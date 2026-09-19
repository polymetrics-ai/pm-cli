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
