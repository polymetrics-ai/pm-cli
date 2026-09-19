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
			if operation.HTTP != nil && operation.HTTP.Authorization != nil &&
				operation.HTTP.Authorization.Required && len(operation.HTTPFacts) != 0 {
				for _, binding := range operation.CommonBindings {
					if binding.FieldID == operation.HTTP.Authorization.FieldID &&
						binding.Sensitivity == SensitivitySecret && binding.Normalized != nil {
						oauthRequired = true
					}
				}
			}
		}
	}
	if !unknownBlocked {
		t.Fatal("unknown sensitivity did not block its operation binding")
	}
	if !oauthRequired {
		t.Fatal("used OAuth bearer security did not produce an exact source-backed authorization binding")
	}

	t.Run("nested schema edges cannot become public bindings", func(t *testing.T) {
		document := []byte(`{
  "openapi": "3.0.3",
  "info": {"title": "Synthetic nested sensitivity boundary", "version": "1.0.0"},
  "servers": [{"url": "https://nested.example.test/v1"}],
  "paths": {
    "/business": {
      "get": {
        "operationId": "listVisibleRecords",
        "parameters": [{
          "name": "status",
          "in": "query",
          "required": false,
          "schema": {"type": "string"}
        }],
        "responses": {"204": {"description": "no content"}}
      }
    },
    "/settings": {
      "post": {
        "operationId": "updateSettings",
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "schema": {
                "type": "object",
                "additionalProperties": false,
                "required": ["settings"],
                "properties": {
                  "settings": {
                    "type": "object",
                    "additionalProperties": false,
                    "x-pm-sensitivity": "public",
                    "required": ["login"],
                    "properties": {
                      "login": {"type": "string", "format": "password"}
                    }
                  }
                }
              }
            }
          }
        },
        "responses": {"204": {"description": "no content"}}
      }
    },
    "/members": {
      "get": {
        "operationId": "searchMembers",
        "parameters": [{
          "name": "members",
          "in": "query",
          "required": false,
          "style": "form",
          "explode": true,
          "x-pm-sensitivity": "public",
          "schema": {
            "type": "array",
            "items": {
              "type": "object",
              "additionalProperties": false,
              "required": ["access_token"],
              "properties": {
                "access_token": {"type": "string"}
              }
            }
          }
        }],
        "responses": {"204": {"description": "no content"}}
      }
    },
    "/profile": {
      "post": {
        "operationId": "selectProfile",
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "schema": {
                "type": "object",
                "additionalProperties": false,
                "required": ["profile"],
                "properties": {
                  "profile": {
                    "type": "object",
                    "additionalProperties": false,
                    "x-pm-sensitivity": "public",
                    "oneOf": [
                      {
                        "type": "object",
                        "additionalProperties": false,
                        "required": ["display_name"],
                        "properties": {
                          "display_name": {"type": "string"}
                        }
                      },
                      {
                        "type": "object",
                        "additionalProperties": false,
                        "required": ["opaque"],
                        "properties": {
                          "opaque": {
                            "type": "string",
                            "x-pm-sensitivity": "unclassified"
                          }
                        }
                      }
                    ]
                  }
                }
              }
            }
          }
        },
        "responses": {"204": {"description": "no content"}}
      }
    },
    "/preferences": {
      "post": {
        "operationId": "updatePreferences",
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "schema": {
                "type": "object",
                "additionalProperties": false,
                "required": ["preferences"],
                "properties": {
                  "preferences": {
                    "type": "object",
                    "additionalProperties": false,
                    "x-pm-sensitivity": "public",
                    "allOf": [
                      {
                        "type": "object",
                        "additionalProperties": false,
                        "required": ["theme"],
                        "properties": {
                          "theme": {"type": "string"}
                        }
                      },
                      {
                        "type": "object",
                        "additionalProperties": false,
                        "required": ["password"],
                        "properties": {
                          "password": {"type": "string"}
                        }
                      }
                    ]
                  }
                }
              }
            }
          }
        },
        "responses": {"204": {"description": "no content"}}
      }
    }
  }
}`)
		operations := []SourceOperation{
			testOperation("listVisibleRecords", "root", "#/paths/~1business/get", nil, []VariantRef{{
				Pointer: "#/paths/~1business/get/responses/204", Status: "204", MediaType: noContentMediaType,
			}}),
			testOperation("updateSettings", "root", "#/paths/~1settings/post", []VariantRef{{
				Pointer: "#/paths/~1settings/post/requestBody/content/application~1json", MediaType: "application/json",
			}}, []VariantRef{{
				Pointer: "#/paths/~1settings/post/responses/204", Status: "204", MediaType: noContentMediaType,
			}}),
			testOperation("searchMembers", "root", "#/paths/~1members/get", nil, []VariantRef{{
				Pointer: "#/paths/~1members/get/responses/204", Status: "204", MediaType: noContentMediaType,
			}}),
			testOperation("selectProfile", "root", "#/paths/~1profile/post", []VariantRef{{
				Pointer: "#/paths/~1profile/post/requestBody/content/application~1json", MediaType: "application/json",
			}}, []VariantRef{{
				Pointer: "#/paths/~1profile/post/responses/204", Status: "204", MediaType: noContentMediaType,
			}}),
			testOperation("updatePreferences", "root", "#/paths/~1preferences/post", []VariantRef{{
				Pointer: "#/paths/~1preferences/post/requestBody/content/application~1json", MediaType: "application/json",
			}}, []VariantRef{{
				Pointer: "#/paths/~1preferences/post/responses/204", Status: "204", MediaType: noContentMediaType,
			}}),
		}
		normalization := testNormalize(t, DialectOpenAPI30, map[string][]byte{"root": document}, nil, operations)

		bindingFor := func(operationID string) BindingFact {
			t.Helper()
			for _, operation := range normalization.Operations {
				if operation.OperationID != operationID {
					continue
				}
				if len(operation.CommonBindings) == 1 {
					return operation.CommonBindings[0]
				}
				if len(operation.RequestVariants) == 1 && len(operation.RequestVariants[0].Bindings) == 1 {
					return operation.RequestVariants[0].Bindings[0]
				}
				t.Fatalf("%s bindings = common %#v, variants %#v", operationID, operation.CommonBindings, operation.RequestVariants)
			}
			t.Fatalf("operation %s was not normalized", operationID)
			return BindingFact{}
		}

		for _, operationID := range []string{"updateSettings", "searchMembers", "updatePreferences"} {
			binding := bindingFor(operationID)
			if binding.Sensitivity != SensitivitySecret || binding.Normalized == nil || binding.BlockedDiagnosticID != "" {
				t.Fatalf("%s nested secret binding was not admitted as secret: %#v", operationID, binding)
			}
		}
		unknown := bindingFor("selectProfile")
		if unknown.Sensitivity != SensitivityUnknown || unknown.Normalized != nil || unknown.BlockedDiagnosticID == "" {
			t.Fatalf("nested unknown binding was not blocked before projection: %#v", unknown)
		}

		commands, diagnostics := ProjectCommands(normalization)
		if len(commands) != 1 || commands[0].OperationID != "listVisibleRecords" {
			t.Fatalf("public commands = %#v, want only the nonsecret operation", commands)
		}
		if len(commands[0].Fields) != 1 || commands[0].Fields[0].Flag != "--status" || commands[0].Fields[0].Sensitive {
			t.Fatalf("nonsecret operation became unusable: %#v", commands[0])
		}
		if len(diagnostics) != 4 {
			t.Fatalf("command projection diagnostics = %#v, want four nested sensitivity refusals", diagnostics)
		}
		blocked := make(map[string]struct{}, len(diagnostics))
		for _, diagnostic := range diagnostics {
			if diagnostic.Code != "unsafe_command_projection" {
				t.Fatalf("projection diagnostic = %#v, want unsafe_command_projection", diagnostic)
			}
			blocked[diagnostic.OperationID] = struct{}{}
		}
		for _, operationID := range []string{"updateSettings", "searchMembers", "selectProfile", "updatePreferences"} {
			if _, ok := blocked[operationID]; !ok {
				t.Fatalf("%s did not receive a command projection refusal: %#v", operationID, diagnostics)
			}
		}
	})
}
