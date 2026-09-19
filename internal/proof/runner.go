package proof

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// StepExecutor is the consumer-owned public-PM execution boundary. Its concrete
// implementation must obtain identities and observations from the executing
// components, not copy an expected record into an observation.
type StepExecutor interface {
	ExecuteStep(context.Context, StepRequest) (StepExecution, error)
}

type StepRequest struct {
	Identity   Currentness
	Step       contract.ScenarioStep
	Input      json.RawMessage
	RequestKey string
}

type StepExecution struct {
	Identity    Currentness
	RunID       string
	Command     []string
	Request     *contract.ProofRequestObservation
	Response    *contract.ProofResponseObservation
	Observation OracleObservation
	Effect      contract.ProofEffect
	StartedAt   time.Time
	FinishedAt  time.Time
	ExitCode    int
}

type CasePlan struct {
	Candidate      CaseCandidate
	Scenario       *contract.Scenario
	StepIdentities map[string]Currentness
}

type RunnerConfig struct {
	Store              *ReceiptStore
	Executor           StepExecutor
	Mode               Mode
	HostClass          string
	CaseTimeout        time.Duration
	CleanupTimeout     time.Duration
	PersistenceTimeout time.Duration
	Now                func() time.Time
}

type RunResult struct {
	Report LaneReport
	Failed bool
}

// RunLane preserves independent completed case checkpoints even when another
// case fails. A cancelled/failed assembly never replaces the previous report.
func RunLane(ctx context.Context, configuration RunnerConfig, inventoryHash contract.SemanticDigest, plans []CasePlan) (RunResult, error) {
	if err := validateRunner(configuration); err != nil {
		return RunResult{}, err
	}
	if ctx == nil || len(plans) == 0 || inventoryHash.Validate() != nil {
		return RunResult{}, errors.New("proof run requires a context, complete case inventory and inventory identity")
	}
	first := plans[0].Candidate.Current.Identity
	report := LaneReport{
		SchemaVersion: "pm.proof-report/v1", ConnectorID: first.ConnectorID, Lane: first.Lane,
		InventoryHash: inventoryHash, GeneratedAt: runnerNow(configuration), Cases: make([]ReportCase, 0, len(plans)),
	}
	seen := make(map[string]struct{}, len(plans))
	for _, plan := range plans {
		identity := plan.Candidate.Current.Identity
		if identity.ConnectorID != first.ConnectorID || identity.Lane != first.Lane {
			return RunResult{}, errors.New("proof report cases belong to different connector lanes")
		}
		key, err := CaseKey(identity)
		if err != nil {
			return RunResult{}, err
		}
		if _, duplicate := seen[key]; duplicate {
			return RunResult{}, errors.New("proof report contains a duplicate case")
		}
		seen[key] = struct{}{}
	}
	result := RunResult{Report: report}
	var assemblyErrors []error
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			return result, errors.Join(append(assemblyErrors, err)...)
		}
		decision, err := ExecuteCase(ctx, configuration, plan)
		if err != nil {
			// Do not stop another independent case merely because persistence or
			// validation of this one failed. An incomplete report is not published.
			assemblyErrors = append(assemblyErrors, err)
			result.Failed = true
			continue
		}
		entry := ReportCase{
			Identity: plan.Candidate.Current.Identity, Availability: decision.Candidate.Availability,
			Reason: decision.Reason, Reused: decision.Reused,
		}
		if decision.Receipt != nil {
			entry.ReceiptID = new(decision.Receipt.ID)
			status := decision.Receipt.Receipt.Status
			if status != contract.ProofPassedFake && status != contract.ProofPassedLive ||
				len(decision.Receipt.Receipt.CleanupDiagnostics) != 0 {
				result.Failed = true
			}
		} else if plan.Candidate.Availability != contract.LaneNotApplicable {
			result.Failed = true
		}
		result.Report.Cases = append(result.Report.Cases, entry)
	}
	if len(assemblyErrors) != 0 {
		return result, errors.Join(assemblyErrors...)
	}
	result.Report.GeneratedAt = runnerNow(configuration)
	if err := configuration.Store.PublishReport(ctx, result.Report); err != nil {
		return result, err
	}
	// The published report is only evidence while its receipts are still the
	// latest for their cases; a concurrent newer failure must not be masked by
	// the report this run just assembled.
	published, found, err := configuration.Store.ReadCurrentReport(
		ctx, result.Report.ConnectorID, result.Report.Lane,
	)
	if err != nil {
		return result, err
	}
	if !found || len(published.Cases) != len(result.Report.Cases) {
		return result, errors.New("published proof report is not readable as current evidence")
	}
	return result, nil
}

// ExecuteCase selects under a process-safe case lock, then executes the ordered
// scenario and its independently armed cleanup. Only the runner evaluates the
// authored assertions; command exit zero is never a passing oracle.
func ExecuteCase(ctx context.Context, configuration RunnerConfig, plan CasePlan) (decision CaseDecision, resultErr error) {
	if err := validateRunner(configuration); err != nil {
		return CaseDecision{}, err
	}
	if ctx == nil {
		return CaseDecision{}, errors.New("proof case context is required")
	}
	identity := plan.Candidate.Current.Identity
	release, err := configuration.Store.LockCase(ctx, identity)
	if err != nil {
		return CaseDecision{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, release()) }()
	key, err := CaseKey(identity)
	if err != nil {
		return CaseDecision{}, err
	}
	latest, found, err := configuration.Store.Latest(ctx, identity)
	if err != nil {
		return CaseDecision{}, err
	}
	history := make(map[string]ReceiptRecord, 1)
	if found {
		history[key] = latest
	}
	decisions, err := SelectCases(configuration.Mode, []CaseCandidate{plan.Candidate}, history)
	if err != nil {
		return CaseDecision{}, err
	}
	decision = decisions[0]
	if !decision.Execute {
		return decision, nil
	}
	if plan.Scenario == nil || plan.Scenario.Validate() != nil ||
		plan.Scenario.ConnectorID != identity.ConnectorID || plan.Scenario.OperationID != identity.OperationID ||
		plan.Scenario.Lane != identity.Lane || plan.Scenario.ScenarioID != identity.CaseID {
		return CaseDecision{}, errors.New("executable proof case lacks its exact valid authored scenario")
	}
	for _, step := range append(slices.Clone(plan.Scenario.Steps), plan.Scenario.Cleanup.Steps...) {
		if _, err := runnerStepIdentity(plan, step); err != nil {
			return CaseDecision{}, err
		}
	}
	receipt := runnerInitialReceipt(configuration, plan)
	if found && latest.Receipt.Effect.State == contract.OutcomeUnknown {
		// A proof rerun is not authorization to replay an unresolved provider
		// effect under a new RunID. Independent reconciliation must happen first.
		receipt.Status = contract.ProofBlocked
		receipt.Effect = latest.Receipt.Effect
		record, err := runnerCheckpoint(configuration, receipt)
		decision.Execute = false
		decision.Reason = "unresolved_prior_effect"
		decision.Receipt = &record
		return decision, err
	}
	if _, err := runnerCheckpoint(configuration, receipt); err != nil {
		return CaseDecision{}, err
	}
	caseContext, cancel := context.WithTimeout(ctx, configuration.CaseTimeout)
	defer cancel()
	captures := make(map[string]runnerCapture)
	completed := make(map[string]bool, len(plan.Scenario.Steps))
	assertionIndexes := make(map[string][]int)
	for index, assertion := range plan.Scenario.Assertions {
		assertionIndexes[assertion.StepID] = append(assertionIndexes[assertion.StepID], index)
	}
	failed := false
	var checkpointErrors []error
	for _, step := range plan.Scenario.Steps {
		if caseContext.Err() != nil || !runnerDependenciesComplete(step, completed) {
			failed = true
			continue
		}
		expected, _ := runnerStepIdentity(plan, step)
		input, err := runnerBindInput(step, captures, false, expected.Identity.ScopeHash)
		if err != nil {
			failed = true
			continue
		}
		requestKey, err := runnerRequestKey()
		if err != nil {
			failed = true
			checkpointErrors = append(checkpointErrors, err)
			break
		}
		execution, executionErr := configuration.Executor.ExecuteStep(caseContext, StepRequest{
			Identity: expected, Step: step, Input: input, RequestKey: requestKey,
		})
		if runnerValidateExecution(execution, expected) != nil {
			failed = true
			receipt.Effect.State = contract.OutcomeUnknown
			break
		}
		runnerObserveExecution(&receipt, execution)
		stepPassed := executionErr == nil && execution.ExitCode == 0
		if err := runnerCaptureValues(step, execution, captures); err != nil {
			stepPassed = false
		}
		for _, index := range assertionIndexes[step.StepID] {
			assertion, assertionErr := AssertExpected(plan.Scenario.Assertions[index], execution.Observation)
			if assertionErr != nil {
				stepPassed = false
				continue
			}
			receipt.Assertions[index] = assertion
			if assertion.Passed == nil || !*assertion.Passed {
				stepPassed = false
			}
		}
		completed[step.StepID] = stepPassed
		failed = failed || !stepPassed
		if _, err := runnerCheckpoint(configuration, receipt); err != nil {
			failed = true
			checkpointErrors = append(checkpointErrors, err)
			break
		}
	}
	// Cleanup has its own finite budget and only consumes independently
	// witnessed owned IDs. Cancellation of observation does not erase ownership.
	cleanupContext, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), configuration.CleanupTimeout)
	defer cleanupCancel()
	for _, step := range plan.Scenario.Cleanup.Steps {
		expected, _ := runnerStepIdentity(plan, step)
		input, err := runnerBindInput(step, captures, true, expected.Identity.ScopeHash)
		if err != nil || cleanupContext.Err() != nil {
			receipt.CleanupDiagnostics = append(receipt.CleanupDiagnostics,
				contract.ProofCleanupDiagnostic{StepID: step.StepID, Code: "owned_cleanup_unavailable"})
			continue
		}
		requestKey, err := runnerRequestKey()
		if err != nil {
			receipt.CleanupDiagnostics = append(receipt.CleanupDiagnostics,
				contract.ProofCleanupDiagnostic{StepID: step.StepID, Code: "cleanup_identity_unavailable"})
			continue
		}
		execution, executionErr := configuration.Executor.ExecuteStep(cleanupContext, StepRequest{
			Identity: expected, Step: step, Input: input, RequestKey: requestKey,
		})
		validExecution := runnerValidateExecution(execution, expected) == nil
		if executionErr != nil || execution.ExitCode != 0 || !validExecution ||
			(execution.Effect.State != contract.OutcomeSucceeded && execution.Effect.State != contract.OutcomeUnchanged) {
			receipt.CleanupDiagnostics = append(receipt.CleanupDiagnostics,
				contract.ProofCleanupDiagnostic{StepID: step.StepID, Code: "owned_cleanup_failed"})
		}
		if validExecution {
			cleanupReceipt := receipt
			cleanupReceipt.Effect.ReceiptRefs = slices.Clone(receipt.Effect.ReceiptRefs)
			cleanupReceipt.Effect.OwnedObjectRefs = slices.Clone(receipt.Effect.OwnedObjectRefs)
			runnerObserveExecution(&cleanupReceipt, execution)
			if _, err := runnerCheckpoint(configuration, cleanupReceipt); err != nil {
				checkpointErrors = append(checkpointErrors, err)
			}
		}
	}
	for _, assertion := range receipt.Assertions {
		if assertion.Passed == nil || !*assertion.Passed {
			failed = true
		}
	}
	if len(receipt.CleanupDiagnostics) != 0 {
		// Owned cleanup that did not complete leaves real resources behind; the
		// case cannot be recorded as a pass even when its assertions matched.
		failed = true
	}
	if receipt.RunID == nil || receipt.Request == nil || receipt.Response == nil || receipt.Effect.State == contract.OutcomeUnknown {
		failed = true
	}
	if failed {
		receipt.Status = contract.ProofFailed
	} else if identity.ExecutionKind == "fake" {
		receipt.Status = contract.ProofPassedFake
	} else {
		receipt.Status = contract.ProofPassedLive
	}
	// StartedAt and FinishedAt come from the observed physical executions. A
	// runner-local clock never rewrites them.
	record, err := runnerCheckpoint(configuration, receipt)
	decision.Receipt = &record
	decision.Reused = false
	decision.Reason = string(receipt.Status)
	return decision, errors.Join(append(checkpointErrors, err)...)
}

func validateRunner(configuration RunnerConfig) error {
	if configuration.Store == nil || configuration.Executor == nil || configuration.HostClass == "" ||
		configuration.CaseTimeout <= 0 || configuration.CleanupTimeout <= 0 || configuration.PersistenceTimeout <= 0 {
		return errors.New("proof runner requires explicit execution, storage, host and finite timeout controls")
	}
	_, err := ParseMode(string(configuration.Mode))
	return err
}

func runnerNow(configuration RunnerConfig) time.Time {
	if configuration.Now != nil {
		return configuration.Now().UTC()
	}
	return time.Now().UTC()
}

func runnerInitialReceipt(configuration RunnerConfig, plan CasePlan) contract.ProofReceipt {
	current := plan.Candidate.Current
	identity := current.Identity
	assertions := make([]contract.ProofAssertionObservation, len(plan.Scenario.Assertions))
	for index, assertion := range plan.Scenario.Assertions {
		assertions[index] = contract.ProofAssertionObservation{
			AssertionID: assertion.AssertionID, Expected: bytes.Clone(assertion.Expected), Observed: json.RawMessage("null"),
		}
	}
	return contract.ProofReceipt{
		ProofVersion: "pm.proof/v1", ConnectorID: identity.ConnectorID, Lane: identity.Lane,
		OperationID: identity.OperationID, BehaviorID: identity.BehaviorID, CaseID: identity.CaseID,
		SourceHashes: slices.Clone(current.SourceHashes), BundleHash: new(current.BundleHash), SchemaHash: new(current.SchemaHash),
		AssertionHash: current.AssertionHash, PMClientHash: new(current.PMClientHash), PMAPIHash: new(current.PMAPIHash),
		PMWorkerHash: new(current.PMWorkerHash), QueryHelperHashIfUsed: current.QueryHelperHash,
		HandlerVersions: current.HandlerVersions, WorkspaceAccountScopeHash: new(identity.ScopeHash),
		Command: []string{"pm", plan.Scenario.OperationID}, Assertions: assertions,
		Effect:                contract.ProofEffect{State: contract.OutcomeUnsent, ReceiptRefs: []string{}, OwnedObjectRefs: []string{}},
		Status:                contract.ProofUnproven,
		ImplementationClosure: &current.ImplementationClosure,
		Provenance: contract.ProofProvenance{
			ExecutionKind: identity.ExecutionKind, HostClass: new(configuration.HostClass), Reused: new(false),
		},
	}
}

func runnerCheckpoint(configuration RunnerConfig, receipt contract.ProofReceipt) (ReceiptRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), configuration.PersistenceTimeout)
	defer cancel()
	return configuration.Store.Append(ctx, contract.LaneEligible, receipt)
}

func runnerStepIdentity(plan CasePlan, step contract.ScenarioStep) (Currentness, error) {
	if selected, exists := plan.StepIdentities[step.StepID]; exists {
		if selected.Identity.OperationID != step.Action {
			return Currentness{}, errors.New("proof step identity selects a different operation")
		}
		return selected, nil
	}
	if step.Action != plan.Candidate.Current.Identity.OperationID || step.ConnectionRef != "" {
		return Currentness{}, errors.New("proof step requires its exact independently selected execution identity")
	}
	return plan.Candidate.Current, nil
}

func runnerValidateExecution(execution StepExecution, expected Currentness) error {
	actualIdentity, expectedIdentity := execution.Identity, expected
	// Closure evidence governs reuse, not the truth of a fresh physical run.
	actualIdentity.ImplementationClosure = contract.ImplementationClosure{}
	expectedIdentity.ImplementationClosure = contract.ImplementationClosure{}
	actualIdentity.SourceHashes = slices.Clone(actualIdentity.SourceHashes)
	expectedIdentity.SourceHashes = slices.Clone(expectedIdentity.SourceHashes)
	slices.Sort(actualIdentity.SourceHashes)
	slices.Sort(expectedIdentity.SourceHashes)
	actual, err := json.Marshal(actualIdentity)
	if err != nil {
		return err
	}
	wanted, err := json.Marshal(expectedIdentity)
	if err != nil {
		return err
	}
	actualDigest, err := contract.CanonicalDigest(actual)
	if err != nil {
		return err
	}
	wantedDigest, err := contract.CanonicalDigest(wanted)
	if err != nil {
		return err
	}
	if actualDigest != wantedDigest || execution.RunID == "" || len(execution.Command) == 0 ||
		execution.Request == nil || execution.Response == nil || execution.StartedAt.IsZero() ||
		execution.FinishedAt.Before(execution.StartedAt) {
		return errors.New("actual proof execution identity or observation is incomplete")
	}
	return nil
}

func runnerObserveExecution(receipt *contract.ProofReceipt, execution StepExecution) {
	receipt.RunID = new(execution.RunID)
	receipt.Command = slices.Clone(execution.Command)
	receipt.Request = execution.Request
	receipt.Response = execution.Response
	if receipt.StartedAt == nil {
		receipt.StartedAt = new(execution.StartedAt.UTC())
	}
	receipt.FinishedAt = new(execution.FinishedAt.UTC())
	if receipt.Effect.State == contract.OutcomeUnknown || execution.Effect.State == contract.OutcomeUnknown {
		receipt.Effect.State = contract.OutcomeUnknown
	} else {
		receipt.Effect.State = execution.Effect.State
	}
	receipt.Effect.ReceiptRefs = append(receipt.Effect.ReceiptRefs, execution.Effect.ReceiptRefs...)
	receipt.Effect.OwnedObjectRefs = append(receipt.Effect.OwnedObjectRefs, execution.Effect.OwnedObjectRefs...)
	// OriginalRunID and Reused describe derived reuse only. Fresh evidence keeps
	// RunID as the physical execution identity and never claims reuse.
}

func runnerDependenciesComplete(step contract.ScenarioStep, completed map[string]bool) bool {
	for _, dependency := range step.DependsOn {
		if !completed[dependency] {
			return false
		}
	}
	return true
}

type runnerCapture struct {
	value       json.RawMessage
	owned       bool
	sensitivity string
	scopeHash   contract.SemanticDigest
}

func runnerCaptureValues(step contract.ScenarioStep, execution StepExecution, captures map[string]runnerCapture) error {
	var captureErrors []error
	for _, capture := range step.Captures {
		value, present, err := LookupJSONPointer(execution.Observation.Document, capture.Pointer)
		if err != nil || !present {
			captureErrors = append(captureErrors, errors.New("declared capture is absent from the actual result"))
			continue
		}
		identity, hasIdentity := runnerCapturedIdentity(value)
		owned := step.Kind == "setup" && hasIdentity && slices.Contains(execution.Effect.OwnedObjectRefs, identity)
		captures[capture.Name] = runnerCapture{
			value: bytes.Clone(value), owned: owned, sensitivity: capture.Sensitivity,
			scopeHash: execution.Identity.Identity.ScopeHash,
		}
	}
	return errors.Join(captureErrors...)
}

func runnerCapturedIdentity(value json.RawMessage) (string, bool) {
	var identity string
	if json.Unmarshal(value, &identity) == nil {
		return identity, identity != ""
	}
	var typed contract.PresentValue
	if json.Unmarshal(value, &typed) == nil {
		if text, ok := typed.TextValue(); ok {
			return text, text != ""
		}
		if number, ok := typed.ExactNumber(); ok && typed.Kind() == contract.ValueKindInteger {
			return number.Text, true
		}
	}
	var number json.Number
	if json.Unmarshal(value, &number) == nil {
		return number.String(), number.String() != ""
	}
	return "", false
}

func runnerBindInput(step contract.ScenarioStep, captures map[string]runnerCapture, cleanup bool, scopeHash contract.SemanticDigest) (json.RawMessage, error) {
	if cleanup && len(step.InputBindings) == 0 {
		return nil, errors.New("cleanup cannot execute without an owned returned identity")
	}
	if _, err := contract.CanonicalDigest(step.Input); err != nil {
		return nil, err
	}
	var input any
	decoder := json.NewDecoder(bytes.NewReader(step.Input))
	decoder.UseNumber()
	if err := decoder.Decode(&input); err != nil {
		return nil, err
	}
	for _, binding := range step.InputBindings {
		capture, exists := captures[binding.Capture]
		if !exists || capture.sensitivity != "public" || cleanup && (!capture.owned || capture.scopeHash != scopeHash) {
			return nil, errors.New("input binding has no permitted actual capture")
		}
		if _, present, err := LookupJSONPointer(step.Input, binding.Pointer); err != nil || !present {
			return nil, errors.New("input binding target is absent")
		}
		var replacement any
		decoder := json.NewDecoder(bytes.NewReader(capture.value))
		decoder.UseNumber()
		if err := decoder.Decode(&replacement); err != nil {
			return nil, err
		}
		if binding.Pointer == "" {
			input = replacement
			continue
		}
		parts := strings.Split(binding.Pointer[1:], "/")
		for index := range parts {
			parts[index] = strings.ReplaceAll(strings.ReplaceAll(parts[index], "~1", "/"), "~0", "~")
		}
		container := input
		for index, part := range parts {
			last := index == len(parts)-1
			switch value := container.(type) {
			case map[string]any:
				child, present := value[part]
				if !present {
					return nil, errors.New("bound input target disappeared")
				}
				if last {
					value[part] = replacement
				} else {
					container = child
				}
			case []any:
				position, err := strconv.Atoi(part)
				if err != nil || position < 0 || position >= len(value) || strconv.Itoa(position) != part {
					return nil, errors.New("input binding array index is invalid")
				}
				if last {
					value[position] = replacement
				} else {
					container = value[position]
				}
			default:
				return nil, errors.New("input binding crosses a scalar value")
			}
		}
	}
	return json.Marshal(input)
}

func runnerRequestKey() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("allocate proof request identity: %w", err)
	}
	return "proof-" + hex.EncodeToString(random[:]), nil
}
