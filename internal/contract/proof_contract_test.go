package contract

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

const proofContractTestHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

func TestScenarioStrictContract(t *testing.T) {
	scenario := validScenarioForProofContractTest()
	document, err := json.Marshal(scenario)
	if err != nil {
		t.Fatalf("marshal valid scenario: %v", err)
	}
	var decoded Scenario
	if err := json.Unmarshal(document, &decoded); err != nil {
		t.Fatalf("decode valid scenario: %v", err)
	}
	if !bytes.Contains(decoded.Steps[1].Input, []byte("900719925474099312345678901234567890")) {
		t.Fatalf("native input number lost exact bytes: %s", decoded.Steps[1].Input)
	}

	cases := map[string][]byte{
		"unknown top-level field": bytes.Replace(document, []byte(`"scenario_id":`), []byte(`"unknown":true,"scenario_id":`), 1),
		"wrong-case field":        bytes.Replace(document, []byte(`"scenario_id":`), []byte(`"ScenarioID":`), 1),
		"duplicate field": bytes.Replace(
			document,
			[]byte(`"connector_id":"github"`),
			[]byte(`"connector_id":"github","connector_id":"other"`),
			1,
		),
		"null required collection": bytes.Replace(document, []byte(`"requirements":[]`), []byte(`"requirements":null`), 1),
		"null optional connection selector": bytes.Replace(
			document,
			[]byte(`"connection_ref":"github-connection"`),
			[]byte(`"connection_ref":null`),
			1,
		),
		"null optional result controls": bytes.Replace(
			document,
			[]byte(`"result_controls":{"total_record_cap":10}`),
			[]byte(`"result_controls":null`),
			1,
		),
		"unknown nested controls": bytes.Replace(
			document,
			[]byte(`"total_record_cap":10`),
			[]byte(`"total_record_cap":10,"TotalRecordCap":10`),
			1,
		),
	}
	for name, candidate := range cases {
		t.Run(name, func(t *testing.T) {
			var value Scenario
			if err := json.Unmarshal(candidate, &value); err == nil {
				t.Fatal("ambiguous or noncanonical scenario was accepted")
			}
		})
	}
}

func TestScenarioDependencyAndCleanupOwnership(t *testing.T) {
	tests := map[string]func(*Scenario){
		"later dependency": func(scenario *Scenario) {
			scenario.Steps[0].DependsOn = []string{"invoke"}
		},
		"capture is not from an explicit ancestor": func(scenario *Scenario) {
			scenario.Steps[1].DependsOn = []string{}
		},
		"binding pointer is absent": func(scenario *Scenario) {
			scenario.Steps[1].InputBindings[0].Pointer = "/missing"
		},
		"cleanup ownership came from inspection": func(scenario *Scenario) {
			scenario.Cleanup.OwnedIDCaptures = []string{"observed_id"}
			scenario.Cleanup.Steps[0].InputBindings[0].Capture = "observed_id"
			scenario.Cleanup.Steps[0].DependsOn = []string{"inspect"}
		},
		"cleanup uses non-owned setup capture": func(scenario *Scenario) {
			scenario.Cleanup.OwnedIDCaptures = []string{}
		},
		"cleanup cannot use a literal identity": func(scenario *Scenario) {
			scenario.Cleanup.Steps[0].InputBindings = nil
		},
		"secret reference becomes raw input": func(scenario *Scenario) {
			scenario.Steps[0].Captures[0].Sensitivity = "secret_reference"
		},
		"capture names are global": func(scenario *Scenario) {
			scenario.Steps[2].Captures[0].Name = "owned_id"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			scenario := validScenarioForProofContractTest()
			mutate(&scenario)
			if err := scenario.Validate(); err == nil {
				t.Fatal("invalid dependency or cleanup ownership was accepted")
			}
		})
	}
}

func TestProofReceiptStrictAndSemanticContract(t *testing.T) {
	receipt := validProofReceiptForContractTest()
	document, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal valid proof receipt: %v", err)
	}
	var decoded ProofReceipt
	if err := json.Unmarshal(document, &decoded); err != nil {
		t.Fatalf("decode valid proof receipt: %v", err)
	}

	nullable := validProofReceiptForContractTest()
	nullable.Status = ProofUnproven
	nullable.RunID = nil
	nullable.BundleHash = nil
	nullable.SchemaHash = nil
	nullable.PMClientHash = nil
	nullable.PMAPIHash = nil
	nullable.PMWorkerHash = nil
	nullable.QueryHelperHashIfUsed = nil
	nullable.HandlerVersions = nil
	nullable.WorkspaceAccountScopeHash = nil
	nullable.Request = nil
	nullable.Response = nil
	nullable.Assertions[0].Passed = nil
	nullable.StartedAt = nil
	nullable.FinishedAt = nil
	nullable.ImplementationClosure = nil
	nullable.CleanupDiagnostics = nil
	nullable.Provenance = ProofProvenance{ExecutionKind: "planned"}
	nullableDocument, err := json.Marshal(nullable)
	if err != nil {
		t.Fatalf("marshal nullable unproven receipt: %v", err)
	}
	if err := json.Unmarshal(nullableDocument, &decoded); err != nil {
		t.Fatalf("required nullable proof fields were rejected: %v", err)
	}

	strictCases := map[string][]byte{
		"unknown top-level field": bytes.Replace(document, []byte(`"proof_version":`), []byte(`"unknown":true,"proof_version":`), 1),
		"wrong-case field":        bytes.Replace(document, []byte(`"case_id":`), []byte(`"CaseID":`), 1),
		"duplicate field": bytes.Replace(
			document,
			[]byte(`"behavior_id":"behavior"`),
			[]byte(`"behavior_id":"behavior","behavior_id":"other"`),
			1,
		),
		"null nonnullable nested field": bytes.Replace(
			document,
			[]byte(`"media_type":"application/json"`),
			[]byte(`"media_type":null`),
			1,
		),
		"null optional body hash": bytes.Replace(
			document,
			[]byte(`"body_sha256":"`+proofContractTestHash+`"`),
			[]byte(`"body_sha256":null`),
			1,
		),
		"null optional implementation closure": bytes.Replace(
			document,
			[]byte(`"implementation_closure":{"known":true,"digest":"`+proofContractTestHash+`"}`),
			[]byte(`"implementation_closure":null`),
			1,
		),
	}
	for name, candidate := range strictCases {
		t.Run(name, func(t *testing.T) {
			var value ProofReceipt
			if err := json.Unmarshal(candidate, &value); err == nil {
				t.Fatal("ambiguous or noncanonical proof receipt was accepted")
			}
		})
	}

	semanticCases := map[string]func(*ProofReceipt){
		"false assertion cannot support pass": func(receipt *ProofReceipt) {
			*receipt.Assertions[0].Passed = false
		},
		"unknown effect cannot support pass": func(receipt *ProofReceipt) {
			receipt.Effect.State = OutcomeUnknown
		},
		"fake evidence cannot be relabelled live": func(receipt *ProofReceipt) {
			receipt.Status = ProofPassedLive
		},
		"completed pass needs exact component evidence": func(receipt *ProofReceipt) {
			receipt.PMWorkerHash = nil
		},
		"finished time cannot precede started time": func(receipt *ProofReceipt) {
			finished := receipt.StartedAt.Add(-time.Second)
			receipt.FinishedAt = &finished
		},
		"known closure needs digest": func(receipt *ProofReceipt) {
			receipt.ImplementationClosure = &ImplementationClosure{Known: true}
		},
		"unknown closure cannot claim digest": func(receipt *ProofReceipt) {
			digest := SemanticDigest(proofContractTestHash)
			receipt.ImplementationClosure = &ImplementationClosure{Digest: &digest}
		},
		"reuse needs known closure": func(receipt *ProofReceipt) {
			*receipt.Provenance.Reused = true
			original := "original-run"
			receipt.Provenance.OriginalRunID = &original
			receipt.ImplementationClosure = nil
		},
		"fresh evidence cannot claim original run": func(receipt *ProofReceipt) {
			original := "not-a-reuse"
			receipt.Provenance.OriginalRunID = &original
		},
		"cleanup diagnostics cannot carry arbitrary text": func(receipt *ProofReceipt) {
			receipt.CleanupDiagnostics = []ProofCleanupDiagnostic{{StepID: "cleanup", Code: "token=secret"}}
		},
	}
	for name, mutate := range semanticCases {
		t.Run(name, func(t *testing.T) {
			receipt := validProofReceiptForContractTest()
			mutate(&receipt)
			if err := receipt.Validate(); err == nil {
				t.Fatal("dishonest proof evidence was accepted")
			}
		})
	}
}

func validScenarioForProofContractTest() Scenario {
	cap := uint64(10)
	return Scenario{
		ScenarioVersion: scenarioSchemaVersion,
		ScenarioID:      "scenario",
		ConnectorID:     "github",
		Lane:            LaneDirectRead,
		OperationID:     "github.issue.get",
		SourceRefs:      []SourceRef{{SourceID: "github-source", Pointer: "#/paths/~1repos"}},
		Requirements:    []ScenarioRequirement{},
		Steps: []ScenarioStep{
			{
				StepID: "setup", Kind: "setup", DependsOn: []string{}, Action: "fixture.create",
				Input:    json.RawMessage(`{"seed":1}`),
				Captures: []ScenarioCapture{{Name: "owned_id", Pointer: "/id", Sensitivity: "public"}},
			},
			{
				StepID: "invoke", Kind: "invoke", DependsOn: []string{"setup"}, Action: "github.issue.get",
				Input:         json.RawMessage(`{"resource_id":null,"exact":900719925474099312345678901234567890}`),
				Captures:      []ScenarioCapture{},
				InputBindings: []ScenarioInputBinding{{Pointer: "/resource_id", Capture: "owned_id"}},
				ConnectionRef: "github-connection", ResultControls: &ResultControls{TotalRecordCap: &cap},
			},
			{
				StepID: "inspect", Kind: "inspect", DependsOn: []string{"invoke"}, Action: "result.inspect",
				Input:    json.RawMessage(`{}`),
				Captures: []ScenarioCapture{{Name: "observed_id", Pointer: "/id", Sensitivity: "public"}},
			},
		},
		Assertions: []ScenarioAssertion{{
			AssertionID: "record", StepID: "inspect", Kind: "equals", Pointer: "/id", Expected: json.RawMessage(`"expected"`),
		}},
		Cleanup: ScenarioCleanup{
			Always: true, OwnedIDCaptures: []string{"owned_id"},
			Steps: []ScenarioStep{{
				StepID: "cleanup", Kind: "cleanup", DependsOn: []string{"setup"}, Action: "fixture.delete",
				Input: json.RawMessage(`{"resource_id":null}`), Captures: []ScenarioCapture{},
				InputBindings: []ScenarioInputBinding{{Pointer: "/resource_id", Capture: "owned_id"}},
			}},
		},
	}
}

func validProofReceiptForContractTest() ProofReceipt {
	hash := SourceHash(proofContractTestHash)
	digest := SemanticDigest(proofContractTestHash)
	runID := "run"
	hostClass := "controlled-fixture"
	reused := false
	passed := true
	started := time.Date(2026, 9, 19, 1, 2, 3, 0, time.UTC)
	finished := started.Add(time.Second)
	return ProofReceipt{
		ProofVersion: ProofSchemaVersion, ConnectorID: "github", Lane: LaneDirectRead,
		OperationID: "github.issue.get", BehaviorID: "behavior", CaseID: "case", RunID: &runID,
		SourceHashes: []SourceHash{hash}, BundleHash: &hash, SchemaHash: &hash, AssertionHash: digest,
		PMClientHash: &hash, PMAPIHash: &hash, PMWorkerHash: &hash, QueryHelperHashIfUsed: &hash,
		HandlerVersions: map[string]string{"http-direct": "1.0.0"}, WorkspaceAccountScopeHash: &digest,
		Command: []string{"pm", "run"},
		Request: &ProofRequestObservation{
			Method: "GET", Target: "https://api.example.invalid/resource", MediaType: "application/json", BodySHA256: &hash,
			SanitizedFields: map[string]json.RawMessage{}, Redactions: []ProofRedaction{},
		},
		Response: &ProofResponseObservation{
			Status: 200, MediaType: "application/json", BodySHA256: &hash,
			SanitizedFields: map[string]json.RawMessage{}, Redactions: []ProofRedaction{},
		},
		Assertions: []ProofAssertionObservation{{AssertionID: "record", Expected: json.RawMessage(`1`), Observed: json.RawMessage(`1`), Passed: &passed}},
		Effect:     ProofEffect{State: OutcomeSucceeded, ReceiptRefs: []string{}, OwnedObjectRefs: []string{}},
		Status:     ProofPassedFake, StartedAt: &started, FinishedAt: &finished,
		Provenance:            ProofProvenance{ExecutionKind: "fake", HostClass: &hostClass, OriginalRunID: nil, Reused: &reused},
		ImplementationClosure: &ImplementationClosure{Known: true, Digest: &digest},
		CleanupDiagnostics:    []ProofCleanupDiagnostic{{StepID: "cleanup", Code: "removed_owned_fixture"}},
	}
}
