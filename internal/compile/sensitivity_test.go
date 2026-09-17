package compile

import "testing"

func TestSecretOperationsNeverProjectToCLI(t *testing.T) {
	document := testFixture(t, "sensitivity-openapi.yaml")
	operations := []SourceOperation{
		testOperation("listBusinessRecords", "root", "#/paths/~1business/get", nil, []VariantRef{{Pointer: "#/paths/~1business/get/responses/200/content/application~1json", Status: "200", MediaType: "application/json"}}),
		testOperation("getKeyedRecord", "root", "#/paths/~1keyed/get", nil, []VariantRef{{Pointer: "#/paths/~1keyed/get/responses/204", Status: "204", MediaType: noContentMediaType}}),
		testOperation("createToken", "root", "#/paths/~1tokens/post", []VariantRef{{Pointer: "#/paths/~1tokens/post/requestBody/content/application~1json", MediaType: "application/json"}}, []VariantRef{{Pointer: "#/paths/~1tokens/post/responses/201/content/application~1json", Status: "201", MediaType: "application/json"}}),
		testOperation("unknownClassification", "root", "#/paths/~1unknown/get", nil, []VariantRef{{Pointer: "#/paths/~1unknown/get/responses/204", Status: "204", MediaType: noContentMediaType}}),
	}
	normalization := testNormalize(t, DialectOpenAPI30, map[string][]byte{"root": document}, nil, operations)
	commands, diagnostics := ProjectCommands(normalization)
	if len(commands) != 1 || commands[0].OperationID != "listBusinessRecords" {
		t.Fatalf("public commands = %#v, want only ordinary business operation", commands)
	}
	if len(commands[0].Fields) != 1 || commands[0].Fields[0].Flag != "--status" || commands[0].Fields[0].Sensitive {
		t.Fatalf("safe business field was not preserved: %#v", commands[0].Fields)
	}
	if len(diagnostics) != 3 {
		t.Fatalf("command projection diagnostics = %#v, want keyed/token/unknown refusals", diagnostics)
	}
	var unknownBlocked bool
	var oauthRequired bool
	for _, operation := range normalization.Operations {
		switch operation.OperationID {
		case "unknownClassification":
			unknownBlocked = len(operation.BlockedDiagnosticIDs) != 0 && operation.CommonBindings[0].Normalized == nil
		case "listBusinessRecords":
			oauthRequired = operation.AuthenticationRequired && len(operation.AuthenticationFacts) != 0
		}
	}
	if !unknownBlocked {
		t.Fatal("unknown sensitivity did not block its operation binding")
	}
	if !oauthRequired {
		t.Fatal("used OAuth bearer security did not produce an exact authentication requirement")
	}
}
