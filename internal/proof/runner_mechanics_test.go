package proof

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// controlledExecutor returns scripted observations for one case. It never
// derives an expectation from the observation: assertions are evaluated by the
// runner against the authored expected bytes.
type controlledExecutor struct {
	documents   map[string]json.RawMessage
	owned       []string
	cleanupExit int
	exitCode    int
	calls       []StepRequest
}

func (executor *controlledExecutor) ExecuteStep(_ context.Context, request StepRequest) (StepExecution, error) {
	executor.calls = append(executor.calls, request)
	started := time.Date(2026, 9, 19, 14, 0, 0, 0, time.UTC)
	exit := executor.exitCode
	effect := contract.ProofEffect{
		State: contract.OutcomeSucceeded, ReceiptRefs: []string{"provider-receipt"},
		OwnedObjectRefs: append([]string{}, executor.owned...),
	}
	if request.Step.Kind == "cleanup" && executor.cleanupExit != 0 {
		exit = executor.cleanupExit
		effect.State = contract.OutcomeRejected
	}
	sanitized := map[string]json.RawMessage{}
	if document, present := executor.documents[request.Step.StepID]; present {
		sanitized["record"] = document
	}
	return StepExecution{
		Identity: request.Identity,
		RunID:    "run-" + request.Identity.Identity.CaseID + "-" + request.Step.StepID,
		Command:  []string{"pm", "invoke", request.Step.Action},
		Request: &contract.ProofRequestObservation{
			Method: "GET", Target: "https://api.example.test/repos/acme/widgets/issues",
			MediaType: "application/json", SanitizedFields: map[string]json.RawMessage{},
			Redactions: []contract.ProofRedaction{},
		},
		Response: &contract.ProofResponseObservation{
			Status: 200, MediaType: "application/json",
			SanitizedFields: sanitized, Redactions: []contract.ProofRedaction{},
		},
		Observation: OracleObservation{Document: executor.documents[request.Step.StepID]},
		Effect:      effect, StartedAt: started, FinishedAt: started.Add(time.Second), ExitCode: exit,
	}, nil
}

func runnerScenario(caseID string, cleanup bool) *contract.Scenario {
	steps := []contract.ScenarioStep{{
		StepID: "read", Kind: "invoke", DependsOn: []string{}, Action: "issues-list",
		Input: json.RawMessage(`{}`), Captures: []contract.ScenarioCapture{},
	}}
	cleanupDefinition := contract.ScenarioCleanup{Always: true, OwnedIDCaptures: []string{}, Steps: []contract.ScenarioStep{}}
	if cleanup {
		steps = append([]contract.ScenarioStep{{
			StepID: "setup", Kind: "setup", DependsOn: []string{}, Action: "issues-list",
			Input: json.RawMessage(`{}`),
			Captures: []contract.ScenarioCapture{{
				Name: "owned", Pointer: "/effect_reference", Sensitivity: "public",
			}},
		}}, steps...)
		steps[1].DependsOn = []string{"setup"}
		cleanupDefinition.OwnedIDCaptures = []string{"owned"}
		cleanupDefinition.Steps = []contract.ScenarioStep{{
			StepID: "clean", Kind: "cleanup", DependsOn: []string{"read"}, Action: "issues-list",
			Input: json.RawMessage(`{}`), Captures: []contract.ScenarioCapture{},
			InputBindings: []contract.ScenarioInputBinding{{Pointer: "", Capture: "owned"}},
		}}
	}
	return &contract.Scenario{
		ScenarioVersion: "pm.scenario/v1",
		ScenarioID:      caseID, ConnectorID: "github", Lane: contract.LaneDirectRead, OperationID: "issues-list",
		SourceRefs: []contract.SourceRef{{SourceID: "GITHUB-REST-SPEC-ACCEPTED", Pointer: "#/paths/~1repos~1{owner}~1{repo}~1issues/get"}},
		Requirements: []contract.ScenarioRequirement{{
			RequirementID: "direct-read", Kind: "capability", Value: "http-direct-read",
		}},
		Steps: steps,
		Assertions: []contract.ScenarioAssertion{{
			AssertionID: "record-equals", StepID: "read", Kind: "equals", Pointer: "/record",
			Expected: json.RawMessage(`{"id":"record-1"}`),
		}},
		Cleanup: cleanupDefinition,
	}
}

func runnerPlan(t *testing.T, receipt contract.ProofReceipt, scenario *contract.Scenario) CasePlan {
	t.Helper()
	current := currentnessFixture(t, receipt)
	return CasePlan{
		Candidate: CaseCandidate{Current: current, Availability: contract.LaneEligible, Reason: "eligible direct-read case"},
		Scenario:  scenario, StepIdentities: map[string]Currentness{},
	}
}

func runnerConfig(store *ReceiptStore, executor StepExecutor) RunnerConfig {
	return RunnerConfig{
		Store: store, Executor: executor, Mode: ModeRetry, HostClass: "controlled-darwin-arm64",
		CaseTimeout: 30 * time.Second, CleanupTimeout: 5 * time.Second, PersistenceTimeout: 5 * time.Second,
	}
}

func TestRunnerRejectsWrongRecordAndPreservesSiblings(t *testing.T) {
	store, _ := openProofStore(t)
	base := time.Date(2026, 9, 19, 15, 0, 0, 0, time.UTC)
	failing := proofReceiptFixture(t, "case-wrong-record", base)
	passing := proofReceiptFixture(t, "case-sibling", base)
	executor := &controlledExecutor{documents: map[string]json.RawMessage{
		"read": json.RawMessage(`{"record":{"id":"record-2"}}`),
	}}
	passingExecutor := &controlledExecutor{documents: map[string]json.RawMessage{
		"read": json.RawMessage(`{"record":{"id":"record-1"}}`),
	}}
	plans := []CasePlan{
		runnerPlan(t, failing, runnerScenario("case-wrong-record", false)),
		runnerPlan(t, passing, runnerScenario("case-sibling", false)),
	}
	result, err := RunLane(t.Context(), runnerConfig(store, &sequencedExecutor{
		byCase: map[string]StepExecutor{"case-wrong-record": executor, "case-sibling": passingExecutor},
	}), proofSemanticHash("inventory"), plans)
	if err != nil {
		t.Fatalf("run lane over controlled cases: %v", err)
	}
	if !result.Failed {
		t.Fatal("lane reported success although one case received the wrong record")
	}
	if _, found, err := store.Latest(t.Context(), plans[0].Candidate.Current.Identity); err != nil || !found {
		t.Fatalf("failing case checkpoint missing: found=%v err=%v", found, err)
	}
	sibling, found, err := store.Latest(t.Context(), plans[1].Candidate.Current.Identity)
	if err != nil || !found || sibling.Receipt.Status != contract.ProofPassedLive {
		t.Fatalf("independent sibling evidence lost: %#v found=%v err=%v", sibling, found, err)
	}

	// A retry run must reuse the passing sibling and re-execute only the failure.
	retryExecutor := &sequencedExecutor{byCase: map[string]StepExecutor{
		"case-wrong-record": &controlledExecutor{documents: map[string]json.RawMessage{"read": json.RawMessage(`{"record":{"id":"record-1"}}`)}},
		"case-sibling":      &controlledExecutor{documents: map[string]json.RawMessage{"read": json.RawMessage(`{"record":{"id":"record-1"}}`)}},
	}}
	retried, err := RunLane(t.Context(), runnerConfig(store, retryExecutor), proofSemanticHash("inventory"), plans)
	if err != nil {
		t.Fatalf("retry lane over controlled cases: %v", err)
	}
	if retried.Failed {
		t.Fatal("retry lane failed after the corrected record arrived")
	}
	executed := retryExecutor.cases()
	if len(executed) != 1 || executed[0] != "case-wrong-record" {
		t.Fatalf("retry executed %v; want only the failed case", executed)
	}
	if len(retried.Report.Cases) != 2 || !retried.Report.Cases[1].Reused {
		t.Fatalf("reused sibling provenance missing: %#v", retried.Report.Cases)
	}
}

func TestRunnerDoesNotReplayUnresolvedEffect(t *testing.T) {
	store, _ := openProofStore(t)
	base := time.Date(2026, 9, 19, 16, 0, 0, 0, time.UTC)
	receipt := proofReceiptFixture(t, "case-unresolved", base)
	unknown := cloneProofReceipt(t, receipt)
	unknown.Effect.State = contract.OutcomeUnknown
	unknown.Status = contract.ProofFailed
	if err := unknown.Validate(); err != nil {
		t.Fatalf("unknown-effect receipt fixture: %v", err)
	}
	if _, err := store.Append(t.Context(), contract.LaneEligible, unknown); err != nil {
		t.Fatalf("append unresolved receipt: %v", err)
	}
	executor := &controlledExecutor{documents: map[string]json.RawMessage{"read": json.RawMessage(`{"id":"record-1"}`)}}
	plan := runnerPlan(t, receipt, runnerScenario("case-unresolved", false))
	decision, err := ExecuteCase(t.Context(), runnerConfig(store, executor), plan)
	if err != nil {
		t.Fatalf("execute blocked case: %v", err)
	}
	if decision.Execute || decision.Reason != "unresolved_prior_effect" {
		t.Fatalf("unresolved effect decision = %#v", decision)
	}
	if len(executor.calls) != 0 {
		t.Fatalf("unresolved effect was replayed through %d provider steps", len(executor.calls))
	}
	if decision.Receipt == nil || decision.Receipt.Receipt.Status != contract.ProofBlocked {
		t.Fatalf("blocked receipt not recorded: %#v", decision.Receipt)
	}
}

func TestRunnerRetainsCleanupDiagnostics(t *testing.T) {
	store, _ := openProofStore(t)
	base := time.Date(2026, 9, 19, 17, 0, 0, 0, time.UTC)
	receipt := proofReceiptFixture(t, "case-cleanup", base)
	executor := &controlledExecutor{
		documents: map[string]json.RawMessage{
			"setup": json.RawMessage(`{"effect_reference":"owned-1"}`),
			"read":  json.RawMessage(`{"record":{"id":"record-1"},"effect_reference":"owned-1"}`),
		},
		owned:       []string{"owned-1"},
		cleanupExit: 1,
	}
	plan := runnerPlan(t, receipt, runnerScenario("case-cleanup", true))
	decision, err := ExecuteCase(t.Context(), runnerConfig(store, executor), plan)
	if err != nil {
		t.Fatalf("execute cleanup case: %v", err)
	}
	if decision.Receipt == nil {
		t.Fatal("cleanup case produced no checkpoint")
	}
	if decision.Receipt.Receipt.Status != contract.ProofFailed {
		t.Fatalf("cleanup failure status = %s; want failed", decision.Receipt.Receipt.Status)
	}
	if len(decision.Receipt.Receipt.CleanupDiagnostics) == 0 {
		t.Fatal("cleanup failure diagnostics were not retained")
	}
	var cleanupCalls int
	for _, call := range executor.calls {
		if call.Step.Kind == "cleanup" {
			cleanupCalls++
		}
	}
	if cleanupCalls != 1 {
		t.Fatalf("cleanup steps executed %d times; want exactly one armed cleanup", cleanupCalls)
	}
	if !errors.Is(err, context.Canceled) && err != nil {
		t.Fatalf("unexpected cleanup execution error: %v", err)
	}
}

// TestRunnerModesTerminalExitAndRedaction covers the remaining runner
// mechanics: all-mode fresh execution, non-zero terminal exits, and the refusal
// to copy secret-bearing observation fields into a receipt.
func TestRunnerModesTerminalExitAndRedaction(t *testing.T) {
	t.Run("all_mode_executes_fresh_while_retry_reuses", func(t *testing.T) {
		store, _ := openProofStore(t)
		base := time.Date(2026, 9, 19, 18, 0, 0, 0, time.UTC)
		receipt := proofReceiptFixture(t, "case-all-mode", base)
		plan := runnerPlan(t, receipt, runnerScenario("case-all-mode", false))
		first := &sequencedExecutor{byCase: map[string]StepExecutor{
			"case-all-mode": &controlledExecutor{documents: map[string]json.RawMessage{"read": json.RawMessage(`{"record":{"id":"record-1"}}`)}},
		}}
		if _, err := RunLane(t.Context(), runnerConfig(store, first), proofSemanticHash("inventory"), []CasePlan{plan}); err != nil {
			t.Fatalf("initial lane run: %v", err)
		}
		if len(first.cases()) != 1 {
			t.Fatalf("initial run executed %v; want one fresh case", first.cases())
		}
		retryRun := &sequencedExecutor{byCase: map[string]StepExecutor{
			"case-all-mode": &controlledExecutor{documents: map[string]json.RawMessage{"read": json.RawMessage(`{"record":{"id":"record-1"}}`)}},
		}}
		if _, err := RunLane(t.Context(), runnerConfig(store, retryRun), proofSemanticHash("inventory"), []CasePlan{plan}); err != nil {
			t.Fatalf("retry lane run: %v", err)
		}
		if len(retryRun.cases()) != 0 {
			t.Fatalf("retry re-executed %v; want reuse of the current pass", retryRun.cases())
		}
		freshConfig := runnerConfig(store, &sequencedExecutor{byCase: map[string]StepExecutor{
			"case-all-mode": &controlledExecutor{documents: map[string]json.RawMessage{"read": json.RawMessage(`{"record":{"id":"record-1"}}`)}},
		}})
		freshConfig.Mode = ModeAll
		fresh := freshConfig.Executor.(*sequencedExecutor)
		if _, err := RunLane(t.Context(), freshConfig, proofSemanticHash("inventory"), []CasePlan{plan}); err != nil {
			t.Fatalf("all-mode lane run: %v", err)
		}
		if len(fresh.cases()) != 1 {
			t.Fatalf("all-mode executed %v; want one fresh physical case", fresh.cases())
		}
	})

	t.Run("non_zero_terminal_exit_cannot_pass", func(t *testing.T) {
		store, _ := openProofStore(t)
		base := time.Date(2026, 9, 19, 18, 30, 0, 0, time.UTC)
		receipt := proofReceiptFixture(t, "case-exit", base)
		executor := &controlledExecutor{
			documents: map[string]json.RawMessage{"read": json.RawMessage(`{"record":{"id":"record-1"}}`)},
			exitCode:  3,
		}
		decision, err := ExecuteCase(t.Context(), runnerConfig(store, executor), runnerPlan(t, receipt, runnerScenario("case-exit", false)))
		if err != nil {
			t.Fatalf("execute failing-exit case: %v", err)
		}
		if decision.Receipt == nil || decision.Receipt.Receipt.Status != contract.ProofFailed {
			t.Fatalf("non-zero terminal exit status = %#v; want failed", decision.Receipt)
		}
		assertionsPassed := true
		for _, assertion := range decision.Receipt.Receipt.Assertions {
			if assertion.Passed == nil || !*assertion.Passed {
				assertionsPassed = false
			}
		}
		if !assertionsPassed {
			t.Fatal("fixture did not isolate the terminal-exit rule")
		}
	})

	t.Run("secret_bearing_observation_fields_are_refused", func(t *testing.T) {
		if err := rejectSecretBearingFields(map[string]json.RawMessage{
			"X-Api-Key": json.RawMessage(`"synthetic-not-a-credential"`),
		}); err == nil {
			t.Fatal("secret-bearing observation field was accepted")
		}
		if err := rejectSecretBearingFields(map[string]json.RawMessage{
			"page": json.RawMessage(`1`),
		}); err != nil {
			t.Fatalf("ordinary observation field was refused: %v", err)
		}
	})
}

// sequencedExecutor routes one case to its own controlled executor so each case
// owns independent observations.
type sequencedExecutor struct {
	byCase map[string]StepExecutor
	order  []string
}

func (executor *sequencedExecutor) ExecuteStep(ctx context.Context, request StepRequest) (StepExecution, error) {
	selected, present := executor.byCase[request.Identity.Identity.CaseID]
	if !present {
		return StepExecution{}, errors.New("controlled executor has no scripted case")
	}
	if request.Step.Kind != "cleanup" {
		already := false
		for _, caseID := range executor.order {
			if caseID == request.Identity.Identity.CaseID {
				already = true
				break
			}
		}
		if !already {
			executor.order = append(executor.order, request.Identity.Identity.CaseID)
		}
	}
	return selected.ExecuteStep(ctx, request)
}

func (executor *sequencedExecutor) cases() []string {
	return append([]string(nil), executor.order...)
}
