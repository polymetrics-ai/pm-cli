package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestSessionCredentialAndInternalProblems(t *testing.T) {
	for _, test := range []struct {
		name        string
		status      int
		code        string
		action      string
		disposition ProblemDisposition
		reference   *SafeOutcomeReference
	}{
		{"device remains pending", 409, "device_authorization_pending", "poll_after_interval", ProblemConfirmedRefusal, nil},
		{"private ingress requires fresh identity", 403, "reauthentication_required", "reauthenticate", ProblemConfirmedRefusal, nil},
		{"connection mutation needs reconciliation", 503, "mutation_outcome_unknown", "same_key_reconcile_only", ProblemOutcomeUnknown, &SafeOutcomeReference{Kind: "connection", Ref: "connection-result"}},
		{"internal failure is not a confirmed refusal", 500, "internal_error", "contact_support_with_incident_id", ProblemDisposition("unclassified_failure"), nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			problem := NewInternalProblem(errors.New("synthetic untrusted dependency detail"))
			problem.Status = test.status
			problem.Code = test.code
			problem.SafeRetryAction = test.action
			problem.OutcomeReference = test.reference
			document, err := json.Marshal(problem)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := DecodeProblem(test.status, "application/problem+json", document)
			if err != nil {
				t.Fatalf("decode %s: %v", test.code, err)
			}
			if decoded.Disposition != test.disposition || decoded.Value.SafeRetryAction != test.action {
				t.Fatalf("unsafe classification: disposition=%q action=%q", decoded.Disposition, decoded.Value.SafeRetryAction)
			}
			if test.reference != nil {
				problem.OutcomeReference = nil
				document, err = json.Marshal(problem)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := DecodeProblem(test.status, "application/problem+json", document); !errors.Is(err, ErrMalformedEnvelope) {
					t.Fatalf("unresolved mutation without its reference: %v", err)
				}
			}
		})
	}
}

func TestWireVersionAndEnvelope(t *testing.T) {
	t.Run("E02_C01_supported_API1_and_safe_additive_response", func(t *testing.T) {
		document := []byte(`{
			"protocol_major":1,
			"protocol_version":"pm.api/v1",
			"instance_id":"instance-public-01",
			"features":["exact-values","typed-result-controls"],
			"required_features":["exact-values"],
			"future_context":{"exact":"9007199254740993123456789","nested":[false,null,{}]}
		}`)
		decoded, err := DecodeServerInfo(document, 1, []string{"exact-values", "typed-result-controls"})
		if err != nil {
			t.Fatalf("DecodeServerInfo: %v", err)
		}
		if decoded.Value.ProtocolMajor != 1 || decoded.Value.ProtocolVersion != WireVersion {
			t.Fatalf("decoded compatibility = %#v; want API1 %q", decoded.Value, WireVersion)
		}
		addition, ok := decoded.AdditiveFields["future_context"]
		if !ok {
			t.Fatal("safe additive response field was dropped")
		}
		if !bytes.Contains(addition, []byte("9007199254740993123456789")) {
			t.Fatalf("safe additive response field lost exact bytes: %s", addition)
		}
	})

	t.Run("additive_metadata_cannot_impersonate_provider_fields", func(t *testing.T) {
		document := []byte(`{
			"protocol_major":1,
			"protocol_version":"pm.api/v1",
			"instance_id":"instance-public-01",
			"features":[],
			"required_features":[],
			"future_context":{"fields":{"pm_automation_credential":"synthetic-not-a-credential"}}
		}`)
		if _, err := DecodeServerInfo(document, 1, nil); !errors.Is(err, ErrForbiddenSecretField) {
			t.Fatalf("metadata disguised as provider fields returned %v; want forbidden secret field", err)
		}
	})

	t.Run("E02_C02_unsupported_major_or_required_feature", func(t *testing.T) {
		fixtures := []struct {
			name      string
			document  string
			major     int
			features  []string
			wantError error
		}{
			{
				name:      "unsupported_major",
				document:  `{"protocol_major":2,"protocol_version":"pm.api/v2","instance_id":"i","features":[],"required_features":[]}`,
				major:     1,
				wantError: ErrUnsupportedProtocolMajor,
			},
			{
				name:      "unsupported_required_feature",
				document:  `{"protocol_major":1,"protocol_version":"pm.api/v1","instance_id":"i","features":["future-required"],"required_features":["future-required"]}`,
				major:     1,
				features:  []string{"exact-values"},
				wantError: ErrUnsupportedRequiredFeature,
			},
		}
		for _, fixture := range fixtures {
			t.Run(fixture.name, func(t *testing.T) {
				_, err := DecodeServerInfo([]byte(fixture.document), fixture.major, fixture.features)
				if !errors.Is(err, fixture.wantError) {
					t.Fatalf("DecodeServerInfo error = %v; want %v", err, fixture.wantError)
				}
			})
		}
	})

	f01 := []struct {
		status      int
		code        string
		retryAction string
		disposition ProblemDisposition
	}{
		{401, "authentication_required", "refresh_authorization", ProblemConfirmedRefusal},
		{404, "resource_not_found", "correct_scope", ProblemConfirmedRefusal},
		{403, "grant_unavailable", "refresh_authorization", ProblemConfirmedRefusal},
		{503, "capability_unavailable", "after_capability_change", ProblemConfirmedRefusal},
		{409, "foundation_incompatible", "after_capability_change", ProblemConfirmedRefusal},
		{409, "specification_stale", "refresh_specification", ProblemConfirmedRefusal},
		{429, "workspace_admission_count_limit", "after_backlog_progress", ProblemConfirmedRefusal},
		{429, "workspace_admission_age_limit", "after_backlog_progress", ProblemConfirmedRefusal},
		{503, "deployment_admission_overload", "after_backlog_progress", ProblemConfirmedRefusal},
		{503, "control_store_unavailable", "same_key_after_dependency_recovery", ProblemConfirmedRefusal},
		{503, "admission_outcome_unknown", "same_key_reconcile_only", ProblemOutcomeUnknown},
		{409, "resume_unavailable", "refresh_run_state", ProblemConfirmedRefusal},
		{428, "control_revision_required", "refresh_control_revision", ProblemConfirmedRefusal},
		{412, "control_revision_mismatch", "refresh_control_revision", ProblemConfirmedRefusal},
		{409, "idempotency_conflict", "new_key_for_new_intent", ProblemConfirmedRefusal},
		{400, "invalid_request", "correct_input", ProblemConfirmedRefusal},
		{410, "retry_receipt_expired", "inspect_existing_result", ProblemResolvedReceiptExpired},
	}

	t.Run("F01_exact_wire_retry_revision_replay_expiry_recovery_distinctions", func(t *testing.T) {
		for _, want := range f01 {
			want := want
			t.Run(want.code, func(t *testing.T) {
				rule, ok := ProblemRuleFor(want.status, want.code)
				if !ok {
					t.Fatalf("missing F01 rule for HTTP %d %s", want.status, want.code)
				}
				if rule.SafeRetryAction != want.retryAction || rule.Disposition != want.disposition {
					t.Fatalf("rule = %#v; want retry %q disposition %q", rule, want.retryAction, want.disposition)
				}
				document := problemFixture(want.status, want.code, want.retryAction)
				decoded, err := DecodeProblem(want.status, "application/problem+json; charset=utf-8", document)
				if err != nil {
					t.Fatalf("DecodeProblem: %v", err)
				}
				if decoded.Disposition != want.disposition {
					t.Fatalf("problem disposition = %q; want %q", decoded.Disposition, want.disposition)
				}
			})
		}
	})

	t.Run("F01_workspace_429_is_distinct_from_deployment_503", func(t *testing.T) {
		workspace, err := DecodeProblem(429, "application/problem+json", problemFixture(429, "workspace_admission_count_limit", "after_backlog_progress"))
		if err != nil {
			t.Fatalf("workspace problem: %v", err)
		}
		deployment, err := DecodeProblem(503, "application/problem+json", problemFixture(503, "deployment_admission_overload", "after_backlog_progress"))
		if err != nil {
			t.Fatalf("deployment problem: %v", err)
		}
		if workspace.Value.Status == deployment.Value.Status || workspace.Value.Code == deployment.Value.Code {
			t.Fatalf("workspace and deployment pressure collapsed: workspace=%#v deployment=%#v", workspace.Value, deployment.Value)
		}
	})

	t.Run("F01_unknown_acceptance_is_not_confirmed_refusal", func(t *testing.T) {
		unknown, err := DecodeProblem(503, "application/problem+json", problemFixture(503, "admission_outcome_unknown", "same_key_reconcile_only"))
		if err != nil {
			t.Fatalf("unknown problem: %v", err)
		}
		refused, err := DecodeProblem(503, "application/problem+json", problemFixture(503, "control_store_unavailable", "same_key_after_dependency_recovery"))
		if err != nil {
			t.Fatalf("refused problem: %v", err)
		}
		if unknown.Disposition != ProblemOutcomeUnknown || refused.Disposition != ProblemConfirmedRefusal {
			t.Fatalf("outcome distinction lost: unknown=%q refused=%q", unknown.Disposition, refused.Disposition)
		}
	})

	t.Run("E02_C03_problem_requires_RFC9457_and_PM_safe_fields", func(t *testing.T) {
		var complete map[string]json.RawMessage
		if err := json.Unmarshal(problemFixture(409, "idempotency_conflict", "new_key_for_new_intent"), &complete); err != nil {
			t.Fatalf("fixture: %v", err)
		}
		for _, field := range []string{"type", "title", "status", "detail", "code", "incident_id", "safe_retry_action"} {
			field := field
			t.Run("missing_"+field, func(t *testing.T) {
				incomplete := make(map[string]json.RawMessage, len(complete)-1)
				for name, value := range complete {
					if name != field {
						incomplete[name] = value
					}
				}
				document, err := json.Marshal(incomplete)
				if err != nil {
					t.Fatalf("marshal fixture: %v", err)
				}
				if _, err := DecodeProblem(409, "application/problem+json", document); err == nil {
					t.Fatalf("problem missing %q was accepted", field)
				}
			})
		}
	})

	t.Run("E02_C04_HTML_or_malformed_problem_is_refused", func(t *testing.T) {
		if _, err := DecodeProblem(503, "text/html", []byte("<html>provider panic: token=secret</html>")); !errors.Is(err, ErrProblemMediaType) {
			t.Fatalf("HTML problem error = %v; want %v", err, ErrProblemMediaType)
		}
		if _, err := DecodeProblem(503, "application/problem+json", []byte(`{"status":`)); !errors.Is(err, ErrMalformedEnvelope) {
			t.Fatalf("malformed JSON error = %v; want %v", err, ErrMalformedEnvelope)
		}
	})

	t.Run("safe_additions_survive_but_known_and_additive_secrets_fail", func(t *testing.T) {
		safe := setJSONField(t, problemFixture(409, "idempotency_conflict", "new_key_for_new_intent"), "future_safe", json.RawMessage(`{"exact":"9007199254740993"}`))
		decoded, err := DecodeProblem(409, "application/problem+json", safe)
		if err != nil {
			t.Fatalf("safe addition: %v", err)
		}
		if !bytes.Contains(decoded.AdditiveFields["future_safe"], []byte("9007199254740993")) {
			t.Fatal("safe additive problem field did not survive exactly")
		}
		unsafe := setJSONField(t, problemFixture(409, "idempotency_conflict", "new_key_for_new_intent"), "provider_secret", json.RawMessage(`"must-not-cross"`))
		if _, err := DecodeProblem(409, "application/problem+json", unsafe); !errors.Is(err, ErrForbiddenSecretField) {
			t.Fatalf("secret-bearing problem error = %v; want %v", err, ErrForbiddenSecretField)
		}
		knownUnsafe := setJSONField(t, problemFixture(409, "idempotency_conflict", "new_key_for_new_intent"), "outcome_reference", json.RawMessage(`{"kind":"run","ref":"run-01","provider_secret":"must-not-cross"}`))
		if _, err := DecodeProblem(409, "application/problem+json", knownUnsafe); !errors.Is(err, ErrForbiddenSecretField) {
			t.Fatalf("secret nested in known problem field error = %v; want %v", err, ErrForbiddenSecretField)
		}
	})

	t.Run("E02_C05_accepted_is_nonterminal", func(t *testing.T) {
		document := []byte(`{
			"state":"accepted",
			"run_ref":"run-01",
			"request_key":"request-01",
			"intent_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"outcome_reference":{"kind":"run","ref":"run-01"},
			"replay":false
		}`)
		accepted, err := DecodeAdmissionResponse(document)
		if err != nil {
			t.Fatalf("DecodeAdmissionResponse: %v", err)
		}
		if IsTerminalRunState(accepted.State) {
			t.Fatalf("accepted state %q was treated as terminal success", accepted.State)
		}
	})

	t.Run("protocol_metadata_rejects_duplicate_unknown_wrong_case_and_secret_before_application_IO", func(t *testing.T) {
		valid := invocationFixture(`"required_features":["exact-values"]`)
		if err := ValidateIncomingJSON("submitInvocation", valid); err != nil {
			t.Fatalf("valid invocation metadata: %v", err)
		}
		fixtures := []struct {
			name     string
			document []byte
			want     error
		}{
			{"duplicate", invocationFixture(`"required_features":[],"request_key":"duplicate"`), ErrDuplicateMetadataField},
			{"unknown", invocationFixture(`"required_features":[],"unknown_control":true`), ErrUnknownMetadataField},
			{"wrong_case", invocationFixture(`"required_features":[],"Request_Key":"wrong-case"`), ErrWrongCaseMetadataField},
			{"forbidden_secret", invocationFixture(`"required_features":[],"provider_secret":"no"`), ErrForbiddenSecretField},
		}
		for _, fixture := range fixtures {
			t.Run(fixture.name, func(t *testing.T) {
				if err := ValidateIncomingJSON("submitInvocation", fixture.document); !errors.Is(err, fixture.want) {
					t.Fatalf("ValidateIncomingJSON error = %v; want %v", err, fixture.want)
				}
			})
		}
	})

	t.Run("controlled_request_object_nullability", func(t *testing.T) {
		for _, fixture := range []struct {
			name        string
			operationID string
			objectField string
		}{
			{name: "membership", operationID: "grantMembership", objectField: "membership"},
			{name: "service_account", operationID: "createServiceAccount", objectField: "service_account"},
			{name: "delegation", operationID: "createDelegation", objectField: "delegation"},
		} {
			t.Run(fixture.name+"_rejects_null", func(t *testing.T) {
				document := controlledObjectFixture(fixture.objectField, json.RawMessage("null"))
				if err := ValidateIncomingJSON(fixture.operationID, document); !errors.Is(err, ErrMalformedEnvelope) {
					t.Fatalf("null %s error = %v; want %v", fixture.objectField, err, ErrMalformedEnvelope)
				}
			})
		}

		optionalNull := bytes.Replace(
			invocationFixture(`"required_features":[]`),
			[]byte(`"result_controls":{}`),
			[]byte(`"result_controls":{"selection":null}`),
			1,
		)
		if err := ValidateIncomingJSON("submitInvocation", optionalNull); !errors.Is(err, ErrMalformedEnvelope) {
			t.Fatalf("null nonnullable optional object error = %v; want %v", err, ErrMalformedEnvelope)
		}

		const operationID = "testNullableObjectControl"
		operationControls[operationID] = OperationControl{
			OperationID:        operationID,
			RequiredBodyFields: []string{"protocol"},
			OptionalBodyFields: []string{"options"},
		}
		operationObjectControls[operationID] = map[string]JSONFieldControl{
			"": {
				AllowedFields:  []string{"options", "protocol"},
				RequiredFields: []string{"protocol"},
			},
			"/options": {
				AllowedFields:  []string{"child"},
				RequiredFields: []string{"child"},
				Nullable:       true,
			},
			"/options/child": {
				AllowedFields:  []string{"value"},
				RequiredFields: []string{"value"},
			},
		}
		t.Cleanup(func() {
			delete(operationControls, operationID)
			delete(operationObjectControls, operationID)
		})
		for _, fixture := range []struct {
			name     string
			document []byte
		}{
			{name: "optional_absent", document: controlledObjectFixture("", nil)},
			{name: "nullable_parent", document: controlledObjectFixture("options", json.RawMessage("null"))},
			{name: "present_descendant", document: controlledObjectFixture("options", json.RawMessage(`{"child":{"value":true}}`))},
		} {
			t.Run(fixture.name, func(t *testing.T) {
				if err := ValidateIncomingJSON(operationID, fixture.document); err != nil {
					t.Fatalf("ValidateIncomingJSON: %v", err)
				}
			})
		}
		invalidDescendant := controlledObjectFixture("options", json.RawMessage(`{"child":null}`))
		if err := ValidateIncomingJSON(operationID, invalidDescendant); !errors.Is(err, ErrMalformedEnvelope) {
			t.Fatalf("null nonnullable descendant error = %v; want %v", err, ErrMalformedEnvelope)
		}
	})

	t.Run("provider_owned_field_names_remain_exact", func(t *testing.T) {
		for _, name := range []string{"MiXeD.Provider_Field", "password"} {
			t.Run(name, func(t *testing.T) {
				document := invocationFixtureWithProviderField(name)
				if err := ValidateIncomingJSON("submitInvocation", document); err != nil {
					t.Fatalf("exact provider field name rejected: %v", err)
				}
				var request InvocationRequest
				if err := json.Unmarshal(document, &request); err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(encoded, []byte(fmt.Sprintf("%q:", name))) {
					t.Fatalf("generated request changed provider-owned field %q: %s", name, encoded)
				}
			})
		}

		t.Run("explicit_null", func(t *testing.T) {
			document := invocationFixtureWithProviderValue("provider_secret", `{"presence":"null"}`)
			if err := ValidateIncomingJSON("submitInvocation", document); err != nil {
				t.Fatalf("provider-owned explicit null rejected: %v", err)
			}
			var request InvocationRequest
			if err := json.Unmarshal(document, &request); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			var roundTrip InvocationRequest
			if err := json.Unmarshal(encoded, &roundTrip); err != nil {
				t.Fatal(err)
			}
			fields, ok := roundTrip.Input.Fields()
			if !ok || fields["provider_secret"].Presence() != contract.PresenceNull {
				t.Fatalf("provider-owned explicit null was not preserved: %#v", roundTrip.Input)
			}
		})
	})

	t.Run("metadata_only_replay_and_one_time_credential_boundary", func(t *testing.T) {
		replay := []byte(`{
			"resource_id":"service-account-01",
			"result_revision":"revision-02",
			"outcome_reference":{"kind":"service_account","ref":"service-account-01"},
			"result_disposition":"prior_mutation_result",
			"replay":true
		}`)
		if _, err := DecodeMutationReplayResponse(replay); err != nil {
			t.Fatalf("metadata-only replay: %v", err)
		}
		secretReplay := setJSONField(t, replay, "pm_automation_credential", json.RawMessage(`"one-time-only"`))
		if _, err := DecodeMutationReplayResponse(secretReplay); !errors.Is(err, ErrForbiddenSecretField) {
			t.Fatalf("secret-bearing replay error = %v; want %v", err, ErrForbiddenSecretField)
		}

		original := []byte(`{
			"resource_id":"service-account-01",
			"result_revision":"revision-02",
			"outcome_reference":{"kind":"service_account","ref":"service-account-01"},
			"result_disposition":"created",
			"replay":false,
			"pm_automation_credential":"pm_once_opaque"
		}`)
		created, err := DecodeServiceAccountCreationResponse(201, original)
		if err != nil {
			t.Fatalf("original one-time credential response: %v", err)
		}
		if created.PmAutomationCredential != "pm_once_opaque" {
			t.Fatal("original one-time PM credential was not represented")
		}
		if _, err := DecodeServiceAccountCreationResponse(200, original); !errors.Is(err, ErrOneTimeCredentialContext) {
			t.Fatalf("replay accepted one-time credential: %v", err)
		}
	})

	t.Run("complete_default_values_controls_and_pagination_round_trip", func(t *testing.T) {
		exactInteger, err := contract.NewIntegerValue("9007199254740993123456789")
		if err != nil {
			t.Fatalf("exact integer: %v", err)
		}
		record := contract.NewObjectValue(map[string]contract.PresentValue{
			"nested": contract.NewObjectValue(map[string]contract.PresentValue{
				"explicit_null": contract.NullValue(),
				"false":         contract.NewBooleanValue(false),
				"empty":         contract.NewArrayValue(nil),
			}),
			"exact": exactInteger,
		})
		providerTotal := contract.ExactNumber{Text: "9007199254740993123456790", Precision: 25, Scale: 0}
		result := contract.ActionResult{
			MediaType: "application/json",
			Data:      contract.NewArrayValue([]contract.PresentValue{record}),
			Pagination: &contract.ResultPagination{
				EffectiveWireLimit: 100,
				Continuation:       contract.NewStringValue("next-source-page"),
				UnreadTail:         []contract.PresentValue{},
				FetchedCount:       1,
				ReturnedCount:      1,
				EmittedCount:       1,
				ProviderTotal:      &providerTotal,
				SourceExhausted:    false,
			},
		}
		encoded, err := json.Marshal(ActionResult(result))
		if err != nil {
			t.Fatalf("marshal ActionResult: %v", err)
		}
		var decoded ActionResult
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("unmarshal ActionResult: %v", err)
		}
		if !decoded.Data.Equal(result.Data) || decoded.Pagination == nil || decoded.Pagination.ProviderTotal == nil || decoded.Pagination.ProviderTotal.Text != providerTotal.Text {
			t.Fatalf("generated ActionResult lost complete value/pagination semantics: %#v", decoded)
		}
		controls := ResultControls{
			Selection: &contract.FieldSelection{Paths: []contract.FieldPath{{"nested"}}},
			Filter:    &contract.RecordFilter{Path: contract.FieldPath{"exact"}, Operator: contract.FilterEqual, Value: &exactInteger},
		}
		if err := controls.Validate(); err != nil {
			t.Fatalf("generated controls alias rejected exact shared controls: %v", err)
		}
	})
}

func TestGeneratedContractBoundary(t *testing.T) {
	root := repositoryRoot(t)
	generator := generatorLauncher(t, root)
	toolPath := t.TempDir()
	for _, name := range []string{"python3", "sh", "dirname"} {
		executable, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("resolve declared generation helper %s: %v", name, err)
		}
		if err := os.Symlink(executable, filepath.Join(toolPath, name)); err != nil {
			t.Fatalf("bind declared generation helper %s: %v", name, err)
		}
	}
	t.Setenv("PATH", toolPath)
	outputs := []string{
		"api/generated.sha256",
		"api/pm.openapi.json",
		"internal/api/client.gen.go",
		"internal/api/controls.gen.go",
		"internal/api/types.gen.go",
		"internal/httpapi/admin.gen.go",
		"internal/httpapi/server.gen.go",
	}
	const fidelityFixture = `
    E03FidelityBase:
      type: object
      required: [display_name]
      properties:
        display_name:
          type: [string, "null"]
    E03FidelityFixture:
      allOf:
        - $ref: "#/components/schemas/E03FidelityBase"
        - type: object
          required: [count, label]
          properties:
            count:
              type: integer
            label:
              type: string
              readOnly: true
`

	t.Run("E01_two_owned_directories_are_byte_identical", func(t *testing.T) {
		first := filepath.Join(t.TempDir(), "first")
		second := filepath.Join(t.TempDir(), "second")
		runGenerator(t, generator, root, first, false, "")
		runGenerator(t, generator, root, second, false, "")
		for _, relative := range outputs {
			left := readFile(t, filepath.Join(first, filepath.FromSlash(relative)))
			right := readFile(t, filepath.Join(second, filepath.FromSlash(relative)))
			if !bytes.Equal(left, right) {
				t.Errorf("generated %s differs across owned output directories", relative)
			}
		}
	})

	t.Run("E01_C02_and_E03_C04_stale_output_fails_exact_byte_check", func(t *testing.T) {
		output := filepath.Join(t.TempDir(), "stale")
		runGenerator(t, generator, root, output, false, "")
		path := filepath.Join(output, "internal", "api", "types.gen.go")
		if err := os.WriteFile(path, append(readFile(t, path), []byte("// stale\n")...), 0o600); err != nil {
			t.Fatalf("inject stale output: %v", err)
		}
		command := exec.Command(generator, "--root", root, "--output-root", output, "--check")
		if result, err := command.CombinedOutput(); err == nil {
			t.Fatalf("stale output passed exact-byte check: %s", result)
		}
	})

	t.Run("generated_digest_manifest_matches_owned_bytes", func(t *testing.T) {
		output := filepath.Join(t.TempDir(), "digest")
		runGenerator(t, generator, root, output, false, "")
		manifest := string(readFile(t, filepath.Join(output, "api", "generated.sha256")))
		lines := strings.Split(strings.TrimSpace(manifest), "\n")
		digested := make(map[string]struct{}, len(lines))
		for _, line := range lines {
			parts := strings.SplitN(line, "  ", 2)
			if len(parts) != 2 {
				t.Fatalf("malformed digest line %q", line)
			}
			if _, duplicate := digested[parts[1]]; duplicate {
				t.Fatalf("duplicate digest for %s", parts[1])
			}
			digested[parts[1]] = struct{}{}
			content := readFile(t, filepath.Join(output, filepath.FromSlash(parts[1])))
			digest := sha256.Sum256(content)
			if parts[0] != hex.EncodeToString(digest[:]) {
				t.Fatalf("digest for %s = %s; want %x", parts[1], parts[0], digest)
			}
		}
		for _, path := range outputs {
			if path == "api/generated.sha256" {
				continue
			}
			if _, exists := digested[path]; !exists {
				t.Fatalf("generated artifact %s is missing from the digest manifest", path)
			}
		}
	})
	t.Run("controls_reject_duplicate_or_wrong_case_keys", func(t *testing.T) {
		canonical := bytes.TrimSpace(readFile(t, filepath.Join(root, "api", "controls.json")))
		var controls map[string]json.RawMessage
		if err := json.Unmarshal(canonical, &controls); err != nil {
			t.Fatal(err)
		}
		version, ok := controls["schema_version"]
		if !ok {
			t.Fatal("canonical controls have no schema_version")
		}
		duplicate := append([]byte(fmt.Sprintf(`{"schema_version":%s,`, version)), canonical[1:]...)
		delete(controls, "schema_version")
		controls["Schema_Version"] = version
		wrongCase, err := json.Marshal(controls)
		if err != nil {
			t.Fatal(err)
		}
		fixtures := map[string][]byte{"duplicate": duplicate, "wrong_case": wrongCase}
		for name, fixture := range fixtures {
			path := filepath.Join(t.TempDir(), name+".controls.json")
			if err := os.WriteFile(path, fixture, 0o600); err != nil {
				t.Fatalf("write %s controls: %v", name, err)
			}
			command := exec.Command(generator, "--root", root, "--controls", path, "--output-root", filepath.Join(t.TempDir(), name))
			if result, err := command.CombinedOutput(); err == nil {
				t.Errorf("%s controls succeeded: %s", name, result)
			}
		}
	})

	t.Run("E01_C01_and_E03_C05_generator_failure_is_not_masked", func(t *testing.T) {
		invalidSpec := filepath.Join(t.TempDir(), "invalid.openapi.json")
		if err := os.WriteFile(invalidSpec, []byte(`{"openapi":"3.1.0","info":`), 0o600); err != nil {
			t.Fatalf("write invalid spec: %v", err)
		}
		command := exec.Command(generator, "--root", root, "--spec", invalidSpec, "--output-root", filepath.Join(t.TempDir(), "invalid"))
		if result, err := command.CombinedOutput(); err == nil {
			t.Fatalf("invalid source generation succeeded: %s", result)
		}

		failing := filepath.Join(t.TempDir(), "failing-generator.sh")
		if err := os.WriteFile(failing, []byte("#!/bin/sh\nexit 23\n"), 0o700); err != nil {
			t.Fatalf("write failing generator: %v", err)
		}
		script := exec.Command("sh", filepath.Join(root, "scripts", "generate.sh"), "--output-root", filepath.Join(t.TempDir(), "script-output"))
		script.Dir = root
		script.Env = append(os.Environ(), "PM_API_GENERATE="+failing)
		if result, err := script.CombinedOutput(); err == nil {
			t.Fatalf("scripts/generate.sh masked selected generator failure: %s", result)
		}
	})

	t.Run("E03_nullable_required_allOf_constraints_are_preserved_or_rejected", func(t *testing.T) {
		canonical := string(readFile(t, filepath.Join(root, "api", "pm.openapi.yaml")))
		supportedPath := filepath.Join(t.TempDir(), "supported.openapi.yaml")
		if err := os.WriteFile(supportedPath, []byte(canonical+fidelityFixture), 0o600); err != nil {
			t.Fatalf("write supported fidelity source: %v", err)
		}
		supportedOutput := filepath.Join(t.TempDir(), "supported")
		runGenerator(t, generator, root, supportedOutput, false, supportedPath)
		compileProjection := func(output, pointer string) *jsonschema.Schema {
			t.Helper()
			return compileJSONSchema(
				t,
				filepath.Join(output, "api", "pm.openapi.json"),
				"#/components/schemas/E03FidelityFixture"+pointer,
			)
		}
		mutations := []struct {
			name             string
			old              string
			new              string
			witness          string
			originalAccepts  bool
			projectedAccepts bool
			readOnly         bool
		}{
			{
				name:    "C01_display_name_made_optional",
				old:     "required: [display_name]\n      properties:\n        display_name:\n          type: [string, \"null\"]",
				new:     "required: []\n      properties:\n        display_name:\n          type: [string, \"null\"]",
				witness: `{"count":0,"label":"owned"}`, originalAccepts: false, projectedAccepts: true,
			},
			{
				name:    "C02_display_name_null_removed",
				old:     "required: [display_name]\n      properties:\n        display_name:\n          type: [string, \"null\"]",
				new:     "required: [display_name]\n      properties:\n        display_name:\n          type: string",
				witness: `{"display_name":null,"count":0,"label":"owned"}`, originalAccepts: true, projectedAccepts: false,
			},
			{
				name:    "C03_allOf_count_required_removed",
				old:     "required: [count, label]\n          properties:\n            count:\n              type: integer",
				new:     "required: [label]\n          properties:\n            count:\n              type: integer",
				witness: `{"display_name":"exact","label":"owned"}`, originalAccepts: false, projectedAccepts: true,
			},
			{
				name:     "C03_allOf_read_only_label_weakened",
				old:      "label:\n              type: string\n              readOnly: true",
				new:      "label:\n              type: string\n              readOnly: false",
				readOnly: true,
			},
		}
		for _, mutation := range mutations {
			t.Run(mutation.name, func(t *testing.T) {
				if strings.Count(fidelityFixture, mutation.old) != 1 {
					t.Fatalf("fixture selector occurrence count = %d; want 1", strings.Count(fidelityFixture, mutation.old))
				}
				path := filepath.Join(t.TempDir(), "fidelity.openapi.yaml")
				mutated := canonical + strings.Replace(fidelityFixture, mutation.old, mutation.new, 1)
				if err := os.WriteFile(path, []byte(mutated), 0o600); err != nil {
					t.Fatalf("write fidelity source: %v", err)
				}
				output := filepath.Join(t.TempDir(), "fidelity")
				runGenerator(t, generator, root, output, false, path)
				// The altered source constructs an independently observable lossy
				// output fixture. Qualification still uses the original source.
				check := exec.Command(generator, "--root", root, "--spec", supportedPath, "--output-root", output, "--check")
				if result, err := check.CombinedOutput(); err == nil {
					t.Fatalf("lossy output passed qualification against the original constraints: %s", result)
				}
				if mutation.readOnly {
					const pointer = "/allOf/1/properties/label"
					if !compileProjection(supportedOutput, pointer).ReadOnly || compileProjection(output, pointer).ReadOnly {
						t.Fatal("generated public schema did not preserve the independently changed read-only constraint")
					}
					return
				}
				witness, err := jsonschema.UnmarshalJSON(strings.NewReader(mutation.witness))
				if err != nil {
					t.Fatal(err)
				}
				originalAccepts := compileProjection(supportedOutput, "").Validate(witness) == nil
				projectedAccepts := compileProjection(output, "").Validate(witness) == nil
				if originalAccepts != mutation.originalAccepts || projectedAccepts != mutation.projectedAccepts {
					t.Fatalf("public schema constraint was lost: original accepts=%v, want %v; changed accepts=%v, want %v", originalAccepts, mutation.originalAccepts, projectedAccepts, mutation.projectedAccepts)
				}
			})
		}
	})

	t.Run("unsupported_schema_projection_fails_explicitly", func(t *testing.T) {
		source := string(readFile(t, filepath.Join(root, "api", "pm.openapi.yaml"))) + fidelityFixture
		marker := "E03FidelityBase:\n      type: object"
		if strings.Count(source, marker) != 1 {
			t.Fatalf("fixture marker occurrence count = %d; want 1", strings.Count(source, marker))
		}
		unsupported := strings.Replace(source, marker, marker+"\n      patternProperties:\n        '^x-':\n          type: string", 1)
		path := filepath.Join(t.TempDir(), "unsupported.openapi.yaml")
		if err := os.WriteFile(path, []byte(unsupported), 0o600); err != nil {
			t.Fatalf("write unsupported source: %v", err)
		}
		command := exec.Command(generator, "--root", root, "--spec", path, "--output-root", filepath.Join(t.TempDir(), "unsupported"))
		if result, err := command.CombinedOutput(); err == nil {
			t.Fatalf("unsupported projection succeeded: %s", result)
		}
	})

	t.Run("explicit_null_const_is_rejected_instead_of_dropped", func(t *testing.T) {
		canonical := string(readFile(t, filepath.Join(root, "api", "pm.openapi.yaml")))
		literalSource := canonical + `
    E03LiteralNullConst:
      type: object
      properties:
        default:
          type: string
        enum:
          type: string
        const:
          type: object
          additionalProperties: true
          example:
            const: null
`
		literalPath := filepath.Join(t.TempDir(), "literal-null-const.openapi.yaml")
		if err := os.WriteFile(literalPath, []byte(literalSource), 0o600); err != nil {
			t.Fatalf("write literal null const source: %v", err)
		}
		runGenerator(t, generator, root, filepath.Join(t.TempDir(), "literal-null-const"), false, literalPath)

		source := canonical + `
    E03ExplicitNullConst:
      const: null
`
		path := filepath.Join(t.TempDir(), "null-const.openapi.yaml")
		if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
			t.Fatalf("write null const source: %v", err)
		}
		command := exec.Command(generator, "--root", root, "--spec", path, "--output-root", filepath.Join(t.TempDir(), "null-const"))
		if result, err := command.CombinedOutput(); err == nil {
			t.Fatalf("explicit null const was silently projected: %s", result)
		}
	})

	t.Run("named_response_component_cannot_hide_one_time_credential_reuse", func(t *testing.T) {
		const (
			safeSchemaRef       = "#/components/schemas/ServerInfo"
			credentialSchemaRef = "#/components/schemas/ServiceAccountCreationResponse"
		)
		fixture := func(componentName, schemaRef string) []byte {
			return openAPIFixture(t, root, func(document map[string]any) {
				paths := fixtureObject(t, document, "paths")
				info := fixtureObject(t, paths, "/v1/info")
				get := fixtureObject(t, info, "get")
				responses := fixtureObject(t, get, "responses")
				responses["200"] = map[string]any{"$ref": "#/components/responses/" + componentName}

				components := fixtureObject(t, document, "components")
				componentResponses := fixtureObject(t, components, "responses")
				componentResponses[componentName] = map[string]any{
					"description": "Synthetic ordinary response reuse witness",
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": map[string]any{"$ref": schemaRef},
						},
					},
				}
				examples := map[string]any{}
				if existing, present := components["examples"]; present {
					var ok bool
					examples, ok = existing.(map[string]any)
					if !ok {
						t.Fatalf("canonical OpenAPI components.examples = %T; want object", existing)
					}
				} else {
					components["examples"] = examples
				}
				examples["credential_reference_literal"] = map[string]any{
					"summary": "A reference-shaped literal example",
					"value":   map[string]any{"$ref": credentialSchemaRef},
				}
			})
		}
		for _, componentName := range []string{"value", "x-audit"} {
			t.Run(componentName, func(t *testing.T) {

				safePath := filepath.Join(t.TempDir(), "named-response-safe.openapi.json")
				if err := os.WriteFile(safePath, fixture(componentName, safeSchemaRef), 0o600); err != nil {
					t.Fatalf("write safe named-response source: %v", err)
				}
				runGenerator(t, generator, root, filepath.Join(t.TempDir(), "named-response-safe"), false, safePath)

				forbiddenPath := filepath.Join(t.TempDir(), "named-response-forbidden.openapi.json")
				if err := os.WriteFile(forbiddenPath, fixture(componentName, credentialSchemaRef), 0o600); err != nil {
					t.Fatalf("write forbidden named-response source: %v", err)
				}
				command := exec.Command(generator, "--root", root, "--spec", forbiddenPath, "--output-root", filepath.Join(t.TempDir(), "named-response-forbidden"))
				if result, err := command.CombinedOutput(); err == nil {
					t.Fatalf("named response reused the one-time credential schema: %s", result)
				}
			})
		}
	})

	t.Run("literal_extensions_on_openapi_object_maps_remain_opaque", func(t *testing.T) {
		const credentialSchemaRef = "#/components/schemas/ServiceAccountCreationResponse"
		source := openAPIFixture(t, root, func(document map[string]any) {
			paths := fixtureObject(t, document, "paths")
			paths["x-audit"] = map[string]any{"$ref": credentialSchemaRef}
			info := fixtureObject(t, paths, "/v1/info")
			get := fixtureObject(t, info, "get")
			responses := fixtureObject(t, get, "responses")
			responses["x-notes"] = map[string]any{"$ref": credentialSchemaRef}
		})
		path := filepath.Join(t.TempDir(), "literal-map-extensions.openapi.json")
		if err := os.WriteFile(path, source, 0o600); err != nil {
			t.Fatalf("write literal map extensions: %v", err)
		}
		output := filepath.Join(t.TempDir(), "literal-map-extensions")
		runGenerator(t, generator, root, output, false, path)
		var published map[string]any
		if err := json.Unmarshal(readFile(t, filepath.Join(output, "api", "pm.openapi.json")), &published); err != nil {
			t.Fatalf("decode published literal map extensions: %v", err)
		}
		paths := fixtureObject(t, published, "paths")
		if fixtureObject(t, paths, "x-audit")["$ref"] != credentialSchemaRef {
			t.Fatal("published Paths extension lost its literal reference value")
		}
		info := fixtureObject(t, paths, "/v1/info")
		get := fixtureObject(t, info, "get")
		responses := fixtureObject(t, get, "responses")
		if fixtureObject(t, responses, "x-notes")["$ref"] != credentialSchemaRef {
			t.Fatal("published Responses extension lost its literal reference value")
		}
	})

	t.Run("property_map_wire_names_are_not_schema_keywords", func(t *testing.T) {
		source := openAPIFixture(t, root, func(document map[string]any) {
			components := fixtureObject(t, document, "components")
			schemas := fixtureObject(t, components, "schemas")
			schemas["E04PropertyMapRole"] = map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"properties"},
				"properties": map[string]any{
					"properties": map[string]any{
						"type":                 "object",
						"additionalProperties": false,
						"required":             []string{"if"},
						"properties": map[string]any{
							"if": map[string]any{"type": "string"},
						},
					},
				},
			}
		})
		path := filepath.Join(t.TempDir(), "property-map-role.openapi.json")
		if err := os.WriteFile(path, source, 0o600); err != nil {
			t.Fatalf("write property-map role source: %v", err)
		}
		output := filepath.Join(t.TempDir(), "property-map-role")
		runGenerator(t, generator, root, output, false, path)
		schema := compileJSONSchema(
			t,
			filepath.Join(output, "api", "pm.openapi.json"),
			"#/components/schemas/E04PropertyMapRole",
		)
		for _, fixture := range []struct {
			name     string
			document string
			valid    bool
		}{
			{name: "exact_wire_names", document: `{"properties":{"if":"retained"}}`, valid: true},
			{name: "nested_type_is_preserved", document: `{"properties":{"if":false}}`, valid: false},
		} {
			t.Run(fixture.name, func(t *testing.T) {
				document, err := jsonschema.UnmarshalJSON(strings.NewReader(fixture.document))
				if err != nil {
					t.Fatal(err)
				}
				err = schema.Validate(document)
				if (err == nil) != fixture.valid {
					t.Fatalf("generated schema validation error = %v; valid = %t", err, fixture.valid)
				}
			})
		}
	})

	t.Run("PresentValue_presence_discriminators_are_exact", func(t *testing.T) {
		schema := compileJSONSchema(
			t,
			filepath.Join(root, "api", "pm.openapi.json"),
			"#/components/schemas/PresentValue",
		)
		valid := []struct {
			name     string
			document string
		}{
			{name: "absent", document: `{"presence":"absent"}`},
			{name: "null", document: `{"presence":"null"}`},
			{name: "string", document: `{"presence":"value","type":"string","text":""}`},
			{name: "boolean_false", document: `{"presence":"value","type":"bool","boolean":false}`},
			{name: "exact_integer", document: `{"presence":"value","type":"integer","exact":"0","precision":1,"scale":0}`},
			{name: "array", document: `{"presence":"value","type":"array","elements":[{"presence":"absent"},{"presence":"null"}]}`},
			{name: "object", document: `{"presence":"value","type":"object","fields":{"provider_secret":{"presence":"null"}}}`},
			{name: "union", document: `{"presence":"value","type":"union","variant":"known","member":{"presence":"absent"}}`},
			{name: "binary_ref", document: `{"presence":"value","type":"binary_ref","binary":{"org_id":"org-01","workspace_id":"workspace-01","artifact_id":"artifact-01","object_attempt_key":"attempt-01","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","byte_length":0,"media_type":"application/octet-stream","backend_generation":"generation-01"}}`},
		}
		for _, fixture := range valid {
			t.Run("valid_"+fixture.name, func(t *testing.T) {
				value, err := jsonschema.UnmarshalJSON(strings.NewReader(fixture.document))
				if err != nil {
					t.Fatal(err)
				}
				if err := schema.Validate(value); err != nil {
					t.Fatalf("valid PresentValue rejected: %v", err)
				}
			})
		}
		invalid := []struct {
			name     string
			document string
		}{
			{name: "unknown_presence", document: `{"presence":"nil"}`},
			{name: "JSON_null_presence", document: `{"presence":null}`},
			{name: "missing_value_shape", document: `{"presence":"value"}`},
			{name: "absent_with_value_fields", document: `{"presence":"absent","type":"string","text":"unexpected"}`},
			{name: "unknown_value_type", document: `{"presence":"value","type":"unknown","text":"unexpected"}`},
		}
		for _, fixture := range invalid {
			t.Run("invalid_"+fixture.name, func(t *testing.T) {
				value, err := jsonschema.UnmarshalJSON(strings.NewReader(fixture.document))
				if err != nil {
					t.Fatal(err)
				}
				if err := schema.Validate(value); err == nil {
					t.Fatal("invalid PresentValue was accepted")
				}
			})
		}
	})

	t.Run("E02_C06_admin_or_secret_schema_cannot_enter_ordinary_output", func(t *testing.T) {
		source := string(readFile(t, filepath.Join(root, "api", "pm.openapi.yaml")))
		adminPath := strings.Replace(source, "paths:\n", "paths:\n  /admin/v1/leak:\n    get:\n      operationId: leakAdminSecret\n      responses:\n        '204':\n          description: forbidden\n", 1)
		secretProperty := strings.Replace(source, "instance_id:\n          type: string", "instance_id:\n          type: string\n        provider_secret:\n          type: string", 1)
		for name, mutated := range map[string]string{"admin_path": adminPath, "secret_property": secretProperty} {
			path := filepath.Join(t.TempDir(), name+".openapi.yaml")
			if err := os.WriteFile(path, []byte(mutated), 0o600); err != nil {
				t.Fatalf("write %s source: %v", name, err)
			}
			command := exec.Command(generator, "--root", root, "--spec", path, "--output-root", filepath.Join(t.TempDir(), name))
			if result, err := command.CombinedOutput(); err == nil {
				t.Errorf("%s contamination generation succeeded: %s", name, result)
			}
		}
	})

	t.Run("canonical_JSON_keeps_discovery_and_credential_boundaries", func(t *testing.T) {
		var document struct {
			OpenAPI    string                     `json:"openapi"`
			Paths      map[string]json.RawMessage `json:"paths"`
			Components struct {
				Schemas map[string]json.RawMessage `json:"schemas"`
			} `json:"components"`
		}
		if err := json.Unmarshal(readFile(t, filepath.Join(root, "api", "pm.openapi.json")), &document); err != nil {
			t.Fatalf("decode generated OpenAPI JSON: %v", err)
		}
		if document.OpenAPI != "3.1.0" {
			t.Fatalf("OpenAPI version = %q; want 3.1.0", document.OpenAPI)
		}
		if _, ok := document.Paths["/v1/info"]; !ok {
			t.Fatal("canonical compatibility route /v1/info is absent")
		}
		for path := range document.Paths {
			if strings.HasPrefix(path, "/admin/") {
				t.Errorf("ordinary generated schema contains admin ingress %q", path)
			}
			if path != "/v1/info" && strings.Contains(path, "/info") {
				t.Errorf("second compatibility discovery route found: %q", path)
			}
		}
		for _, path := range []string{
			"/v1/workspaces/{workspace_id}/invocations",
			"/v1/workspaces/:workspace_id/invocations",
		} {
			operationID, ok := OperationIDForRoute("post", path)
			if !ok || operationID != "submitInvocation" {
				t.Fatalf("route %q resolved to %q, %t; want submitInvocation", path, operationID, ok)
			}
		}
		credentialLocations := collectJSONPropertyLocations(t, document.Components.Schemas, "pm_automation_credential")
		want := []string{"ServiceAccountCreationResponse.pm_automation_credential"}
		if fmt.Sprint(credentialLocations) != fmt.Sprint(want) {
			t.Fatalf("one-time credential schema locations = %v; want %v", credentialLocations, want)
		}
		for name := range document.Components.Schemas {
			if strings.HasPrefix(name, "E03Fidelity") || strings.HasPrefix(name, "GenerationFidelity") {
				t.Errorf("canonical ordinary API contains proof-only schema %q", name)
			}
		}
	})

}

func problemFixture(status int, code, retryAction string) []byte {
	return []byte(fmt.Sprintf(`{
		"type":"https://polymetrics.ai/problems/%s",
		"title":"Request refused",
		"status":%d,
		"detail":"The request could not be completed under the declared scope.",
		"code":%q,
		"incident_id":"incident-public-01",
		"safe_retry_action":%q,
		"outcome_reference":{"kind":"request","ref":"request-01"}
	}`, code, status, code, retryAction))
}

func setJSONField(t *testing.T, document []byte, name string, value json.RawMessage) []byte {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(document, &object); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	object[name] = value
	encoded, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return encoded
}

func invocationFixture(extraMetadata string) []byte {
	return []byte(`{
		"protocol":{
			"wire_version":"pm.api/v1",
			` + extraMetadata + `,
			"request_key":"request-01",
			"intent_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"expected_revision":"revision-01"
		},
		"connection_ref":"connection-01",
		"operation_ref":"operation-01@revision-01",
		"lane":"direct_read",
		"input":{"presence":"value","type":"object","fields":{}},
		"result_controls":{}
	}`)
}

func invocationFixtureWithProviderField(name string) []byte {
	return invocationFixtureWithProviderValue(name, `{"presence":"value","type":"string","text":"kept"}`)
}

func invocationFixtureWithProviderValue(name, value string) []byte {
	return []byte(`{
		"protocol":{
			"wire_version":"pm.api/v1",
			"required_features":["exact-values"],
			"request_key":"request-01",
			"intent_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"expected_revision":"revision-01"
		},
		"connection_ref":"connection-01",
		"operation_ref":"operation-01@revision-01",
		"lane":"direct_read",
		"input":{"presence":"value","type":"object","fields":{` + fmt.Sprintf("%q", name) + `:` + value + `}},
		"result_controls":{}
	}`)
}

func controlledObjectFixture(name string, value json.RawMessage) []byte {
	field := ""
	if name != "" {
		field = fmt.Sprintf(",%q:%s", name, value)
	}
	return []byte(`{
		"protocol":{
			"wire_version":"pm.api/v1",
			"required_features":[],
			"request_key":"request-01",
			"intent_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"expected_revision":"revision-01"
		}` + field + `
	}`)
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	return root
}

func generatorLauncher(t *testing.T, root string) string {
	t.Helper()
	path := os.Getenv("PM_API_GENERATE")
	if path == "" {
		path = filepath.Join(".cache", "bin", "pm-api-generate")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("required generated-contract tool %s is unavailable: %v", path, err)
	}
	t.Setenv("PM_API_GENERATE", path)
	return filepath.Join(root, "scripts", "generate.sh")
}

func runGenerator(t *testing.T, generator, root, outputRoot string, check bool, spec string) {
	t.Helper()
	arguments := []string{"--root", root, "--output-root", outputRoot}
	if check {
		arguments = append(arguments, "--check")
	}
	if spec != "" {
		arguments = append(arguments, "--spec", spec)
	}
	command := exec.Command(generator, arguments...)
	if result, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate %v: %v\n%s", arguments, err, result)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return content
}

func openAPIFixture(t *testing.T, root string, mutate func(map[string]any)) []byte {
	t.Helper()
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(readFile(t, filepath.Join(root, "api", "pm.openapi.json"))))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode canonical OpenAPI projection: %v", err)
	}
	mutate(document)
	source, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatalf("encode OpenAPI fixture: %v", err)
	}
	return append(source, '\n')
}

func fixtureObject(t *testing.T, parent map[string]any, field string) map[string]any {
	t.Helper()
	value, present := parent[field]
	if !present {
		t.Fatalf("canonical OpenAPI fixture has no %q field", field)
	}
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("canonical OpenAPI fixture field %q = %T; want object", field, value)
	}
	return object
}

func compileJSONSchema(t *testing.T, path, pointer string) *jsonschema.Schema {
	t.Helper()
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(readFile(t, path)))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	if err := compiler.AddResource(path, document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(path + pointer)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func collectJSONPropertyLocations(t *testing.T, schemas map[string]json.RawMessage, property string) []string {
	t.Helper()
	var locations []string
	for name, raw := range schemas {
		var schema any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&schema); err != nil {
			t.Fatalf("decode schema %s: %v", name, err)
		}
		collectPropertyLocations(name, schema, property, &locations)
	}
	sort.Strings(locations)
	return locations
}

func collectPropertyLocations(path string, value any, property string, locations *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		if properties, ok := typed["properties"].(map[string]any); ok {
			if _, found := properties[property]; found {
				*locations = append(*locations, path+"."+property)
			}
		}
		for key, child := range typed {
			collectPropertyLocations(path+"."+key, child, property, locations)
		}
	case []any:
		for index, child := range typed {
			collectPropertyLocations(fmt.Sprintf("%s[%d]", path, index), child, property, locations)
		}
	}
}

func FuzzPublicResponseMetadata(f *testing.F) {
	f.Add([]byte(`{"protocol_major":1,"protocol_version":"pm.api/v1","instance_id":"instance-fuzz","features":[],"required_features":[]}`))
	f.Add([]byte(`{"protocol_major":1,"protocol_version":"pm.api/v1","instance_id":"instance-fuzz","features":[],"required_features":[],"future_context":{"fields":{"safe_label":"retained"}}}`))
	f.Fuzz(func(t *testing.T, document []byte) {
		if _, err := DecodeServerInfo(document, 1, nil); err != nil {
			return
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(document, &fields); err != nil {
			t.Fatalf("accepted response is not a JSON object: %v", err)
		}
		canonical, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		duplicate := append([]byte(`{"protocol_major":1,`), canonical[1:]...)
		if _, err := DecodeServerInfo(duplicate, 1, nil); err == nil || (len(duplicate) <= maxPublicEnvelopeBytes && !errors.Is(err, ErrDuplicateMetadataField)) {
			t.Fatalf("duplicate metadata did not fail closed: %v", err)
		}
		fields["fuzz_context"] = json.RawMessage(`{"fields":{"pm_automation_credential":"synthetic-not-a-credential"}}`)
		secretBearing, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeServerInfo(secretBearing, 1, nil); err == nil || (len(secretBearing) <= maxPublicEnvelopeBytes && !errors.Is(err, ErrForbiddenSecretField)) {
			t.Fatalf("secret-bearing additive metadata did not fail closed: %v", err)
		}
	})
}

func TestSensitivePMTransportDiagnostics(t *testing.T) {
	const canary = "synthetic-one-time-pm-transport-canary"
	for _, test := range []struct {
		name  string
		value any
	}{
		{"device proof", DeviceProof{Proof: canary}},
		{"device challenge", DeviceChallenge{DeviceProof: canary}},
		{"device session", PMDeviceSession{PmSessionCredential: canary}},
		{"browser code", BrowserLogin{Code: canary}},
		{"service-account creation", ServiceAccountCreationResponse{PmAutomationCredential: canary}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if strings.Contains(fmt.Sprintf("%v %#v", test.value, test.value), canary) {
				t.Fatal("one-time PM authentication material leaked through diagnostic formatting")
			}
		})
	}
}
