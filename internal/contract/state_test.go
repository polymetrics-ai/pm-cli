package contract

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestAllowedTransitions(t *testing.T) {
	base := time.Date(2026, time.September, 16, 4, 0, 0, 0, time.UTC)
	spec, durableGrant := runSpecForIdentityTest(t, base)
	binding, err := NewAPIWorkflowBinding(spec.RunID)
	if err != nil {
		t.Fatalf("NewAPIWorkflowBinding: %v", err)
	}
	record, err := NewRunRecord(spec, binding, base)
	if err != nil {
		t.Fatalf("NewRunRecord: %v", err)
	}
	task := taskForIdentityTest(spec, base)
	effect := effectForIdentityTest(t, spec)
	t.Run("accepted_run_reconciliation_preserves_explicit_empty_history", func(t *testing.T) {
		observedAt := base.Add(time.Second)
		updated, err := record.ReconcileEffects([]EffectRecord{}, false, "", observedAt)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(updated)
		if err != nil {
			t.Fatal(err)
		}
		var restored RunRecord
		if err := json.Unmarshal(encoded, &restored); err != nil {
			t.Fatalf("reconciled record cannot be restored: %v", err)
		}
		if restored.State != RunStateAccepted || restored.History == nil || len(restored.History) != 0 ||
			!restored.ProjectionObservedAt.Equal(observedAt) {
			t.Fatalf("reconciliation lost accepted state, empty history, or observation: %+v", restored)
		}
	})

	t.Run("undispatched_effect_preserves_explicit_empty_attempts", func(t *testing.T) {
		settled, err := effect.Transition(OutcomeUnchanged, "already_satisfied", base.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(settled)
		if err != nil {
			t.Fatal(err)
		}
		var restored EffectRecord
		if err := json.Unmarshal(encoded, &restored); err != nil {
			t.Fatalf("settled effect lost its explicit empty attempt list on the wire: %v", err)
		}
		if restored.State != OutcomeUnchanged || restored.Attempts == nil || len(restored.Attempts) != 0 {
			t.Fatalf("settling without a send invented or erased attempt evidence: %#v", restored)
		}
		if effect.State != OutcomeUnsent || effect.Revision != 1 || len(effect.History) != 0 {
			t.Fatal("settling the effect mutated its prior immutable state")
		}
	})
	attempt := Attempt{
		AttemptID: "attempt-1", DispatchID: "dispatch-1", ReceiptRef: "receipt-A1",
		RecordedAt: base.Add(time.Minute),
	}
	dispatched, err := effect.RecordAttempt(attempt)
	if err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}
	timedOut, err := dispatched.ObserveTimeout(base.Add(2 * time.Minute))
	if err != nil {
		t.Fatalf("ObserveTimeout: %v", err)
	}

	t.Run("V04_timeout_preserves_receipt_and_uncertainty_dominates_blockage", func(t *testing.T) {
		if timedOut.State != OutcomeUnknown {
			t.Fatalf("timeout state = %q; want unknown", timedOut.State)
		}
		if !reflect.DeepEqual(timedOut.Attempts, dispatched.Attempts) || timedOut.Attempts[0].ReceiptRef != "receipt-A1" {
			t.Fatalf("timeout attempts = %#v; want the exact retained attempt receipt", timedOut.Attempts)
		}
		updated, err := record.ReconcileEffects(
			[]EffectRecord{timedOut}, true, "policy_prevents_progress", base.Add(3*time.Minute),
		)
		if err != nil {
			t.Fatalf("ReconcileEffects(unknown and blocked): %v", err)
		}
		if updated.State != RunStateReconciling || updated.UnknownEffectCount != 1 || updated.BlockedReason != "policy_prevents_progress" {
			t.Fatalf("run projection = state:%q unknown:%d reason:%q; want reconciling/1/preserved reason", updated.State, updated.UnknownEffectCount, updated.BlockedReason)
		}
		if got, err := updated.Transition(RunStateCancelled, "timeout", base.Add(4*time.Minute)); err == nil || !reflect.DeepEqual(got, updated) {
			t.Fatalf("unknown aggregate cancellation = %#v, %v; want exact reconciling record and error", got, err)
		}
		updatedTask, err := task.ApplyEffectSummary(
			EffectSummary{Unknown: 1}, true, "policy_prevents_progress", base.Add(3*time.Minute),
		)
		if err != nil {
			t.Fatalf("Task.ApplyEffectSummary(unknown and blocked): %v", err)
		}
		if updatedTask.State != RunStateReconciling || updatedTask.UnknownEffectCount != 1 || updatedTask.BlockedReason != "policy_prevents_progress" {
			t.Fatalf("task projection = %#v; uncertainty must dominate blockage", updatedTask)
		}
	})

	t.Run("C01_all_effects_known_allows_blocked_and_keeps_receipt", func(t *testing.T) {
		unknownRecord, err := record.ReconcileEffects(
			[]EffectRecord{timedOut}, true, "policy_prevents_progress", base.Add(3*time.Minute),
		)
		if err != nil {
			t.Fatal(err)
		}
		known, err := timedOut.Transition(OutcomeSucceeded, "provider_confirmed", base.Add(4*time.Minute))
		if err != nil {
			t.Fatalf("EffectRecord.Transition(known): %v", err)
		}
		blocked, err := unknownRecord.ReconcileEffects(
			[]EffectRecord{known}, true, "policy_prevents_progress", base.Add(5*time.Minute),
		)
		if err != nil {
			t.Fatalf("ReconcileEffects(known and blocked): %v", err)
		}
		if blocked.State != RunStateBlocked || blocked.UnknownEffectCount != 0 {
			t.Fatalf("known aggregate = state:%q unknown:%d; want blocked/0", blocked.State, blocked.UnknownEffectCount)
		}
		unknownTask, err := task.ApplyEffectSummary(
			EffectSummary{Unknown: 1}, true, "policy_prevents_progress", base.Add(3*time.Minute),
		)
		if err != nil {
			t.Fatal(err)
		}
		blockedTask, err := unknownTask.ApplyEffectSummary(
			EffectSummary{Succeeded: 1}, true, "policy_prevents_progress", base.Add(5*time.Minute),
		)
		if err != nil {
			t.Fatal(err)
		}
		if blockedTask.State != RunStateBlocked || blockedTask.UnknownEffectCount != 0 {
			t.Fatalf("known task aggregate = state:%q unknown:%d; want blocked/0", blockedTask.State, blockedTask.UnknownEffectCount)
		}
		if len(known.Attempts) != 1 || known.Attempts[0].ReceiptRef != "receipt-A1" {
			t.Fatalf("known effect attempts = %#v; prior receipt was not retained", known.Attempts)
		}
	})

	t.Run("C02_temporal_chain_changes_without_changing_PM_identity_or_intent", func(t *testing.T) {
		first, err := binding.ObserveTemporalRun("temporal-run-1", base.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		second, err := first.ObserveTemporalRun("temporal-run-2", base.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if second.RunID != record.Spec.RunID || second.WorkflowID != binding.WorkflowID || second.CurrentTemporalRunID() != "temporal-run-2" {
			t.Fatalf("workflow binding = %#v; stable PM identity was not preserved", second)
		}
		if len(second.Observations) != 2 || second.Observations[0].TemporalRunID != "temporal-run-1" {
			t.Fatalf("workflow observations = %#v; want the retained execution chain", second.Observations)
		}
		if record.Spec.ExecutionGrantRef != spec.ExecutionGrantRef || record.Spec.OperationOrPlanRef != spec.OperationOrPlanRef {
			t.Fatal("Temporal observation rewrote immutable intent or durable grant")
		}
	})

	t.Run("C03_login_session_expiry_does_not_erase_durable_grant", func(t *testing.T) {
		encoded, err := json.Marshal(durableGrant)
		if err != nil {
			t.Fatal(err)
		}
		var retained ExecutionGrant
		if err := json.Unmarshal(encoded, &retained); err != nil {
			t.Fatalf("durable grant round trip: %v", err)
		}
		if retained.GrantEpoch != durableGrant.GrantEpoch || !retained.ExpiresAt.Equal(durableGrant.ExpiresAt) || retained.PolicyEpochs != durableGrant.PolicyEpochs {
			t.Fatalf("retained grant = %#v; logout-independent epoch/expiry was lost", retained)
		}
	})

	t.Run("C04_one_hop_delegation_preserves_attribution_and_never_expands_parent", func(t *testing.T) {
		parent := GrantCeiling{
			Scope:         durableGrant.Scope,
			ConnectionIDs: []string{"connection-1"},
			OperationIDs:  []string{"github.issues.create"},
			ApprovalRefs:  []string{},
			ExpiresAt:     base.Add(48 * time.Hour),
		}
		requested := Principal{OrgID: spec.OrgID, PrincipalID: "ada", Kind: PrincipalUser}
		effective := Principal{OrgID: spec.OrgID, PrincipalID: "automation-1", Kind: PrincipalServiceAccount}
		delegation := Delegation{DelegationID: "delegation-1", ParentGrantRef: "parent-grant-1", Hop: 1, ParentCeiling: parent}
		actor := Actor{RequestedActor: requested, EffectiveActor: effective, Delegation: &delegation}
		grant := ExecutionGrant{
			GrantRef: "delegated-grant-1", GrantEpoch: 8, Scope: durableGrant.Scope, Actor: actor,
			ConnectionIDs: []string{"connection-1"}, OperationIDs: []string{"github.issues.create"}, ApprovalRefs: []string{},
			ExpiresAt: base.Add(24 * time.Hour), PolicyEpochs: PolicyEpochs{Membership: 4, Service: 6, Delegation: 7, Connection: 2},
		}
		if err := grant.Validate(); err != nil {
			t.Fatalf("valid one-hop grant: %v", err)
		}
		if !grant.Actor.RequestedActor.Equal(requested) || !grant.Actor.EffectiveActor.Equal(effective) {
			t.Fatalf("actor attribution = %#v; requested and effective identities were not retained", grant.Actor)
		}
		missingDelegationEpoch := grant
		missingDelegationEpoch.PolicyEpochs.Delegation = 0
		if err := missingDelegationEpoch.Validate(); err == nil {
			t.Fatal("delegated grant omitted its delegation policy epoch")
		}
		twoHops := grant
		twoHopDelegation := *grant.Actor.Delegation
		twoHopDelegation.Hop = 2
		twoHops.Actor.Delegation = &twoHopDelegation
		if err := twoHops.Validate(); err == nil {
			t.Fatal("two-hop delegation was accepted")
		}
		expanded := grant
		expanded.ConnectionIDs = []string{"connection-1", "connection-outside-parent"}
		if err := expanded.Validate(); err == nil {
			t.Fatal("delegated grant expanded beyond its immutable parent ceiling")
		}
	})

	t.Run("accepted_run_owns_constructor_inputs", func(t *testing.T) {
		intent, _ := runSpecForIdentityTest(t, base)
		intent.InputRefs = []string{"input-original"}
		intent.SchemaRefs = []string{"schema-original"}
		intent.CapabilityVersions = []CapabilityRequirement{{Capability: "http.request", Foundation: FoundationRef{ID: "FND-1", Version: "1.0.0"}}}
		intent.Actor.EffectiveActor = Principal{OrgID: intent.OrgID, PrincipalID: "automation-original", Kind: PrincipalServiceAccount}
		delegation := Delegation{
			DelegationID: "delegation-original", ParentGrantRef: "parent-grant-original", Hop: 1,
			ParentCeiling: GrantCeiling{
				Scope:         Scope{OrgID: intent.OrgID, WorkspaceID: intent.WorkspaceID},
				ConnectionIDs: []string{"connection-original"}, OperationIDs: []string{"operation-original"},
				ApprovalRefs: []string{"approval-original"}, ExpiresAt: base.Add(24 * time.Hour),
			},
		}
		intent.Actor.Delegation = &delegation
		chain, err := NewAPIWorkflowBinding(intent.RunID)
		if err != nil {
			t.Fatal(err)
		}
		chain, err = chain.ObserveTemporalRun("temporal-original", base)
		if err != nil {
			t.Fatal(err)
		}
		accepted, err := NewRunRecord(intent, chain, base)
		if err != nil {
			t.Fatal(err)
		}
		before, err := json.Marshal(accepted)
		if err != nil {
			t.Fatal(err)
		}
		intent.InputRefs[0] = "input-rewritten"
		intent.SchemaRefs[0] = "schema-rewritten"
		intent.CapabilityVersions[0].Foundation.Version = "2.0.0"
		delegation.DelegationID = "delegation-rewritten"
		delegation.ParentCeiling.ConnectionIDs[0] = "connection-rewritten"
		delegation.ParentCeiling.OperationIDs[0] = "operation-rewritten"
		delegation.ParentCeiling.ApprovalRefs[0] = "approval-rewritten"
		chain.Observations[0].TemporalRunID = "temporal-rewritten"
		after, err := json.Marshal(accepted)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("accepted intent or workflow history changed through caller-owned constructor inputs")
		}
	})

	t.Run("C05_foreign_workspace_is_explicit_and_never_aliased", func(t *testing.T) {
		alpha := Scope{OrgID: "org-alpha", WorkspaceID: "workspace-alpha"}
		foreign := Scope{OrgID: "org-alpha", WorkspaceID: "workspace-foreign"}
		if err := alpha.Validate(); err != nil {
			t.Fatal(err)
		}
		if err := foreign.Validate(); err != nil {
			t.Fatal(err)
		}
		if alpha.Equal(foreign) {
			t.Fatal("foreign workspace identity was aliased to alpha")
		}
		foreignGrant := durableGrant
		foreignGrant.Scope = foreign
		if err := foreignGrant.Validate(); err != nil {
			t.Fatalf("pure representation rejected explicit foreign scope and would overclaim API/RLS enforcement: %v", err)
		}
	})

	t.Run("C06_illegal_terminal_transitions_leave_history_and_receipts_unchanged", func(t *testing.T) {
		running, err := record.Transition(RunStateRunning, "started", base.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		known, err := timedOut.Transition(OutcomeSucceeded, "provider_confirmed", base.Add(4*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		succeeded, err := running.ReconcileEffects([]EffectRecord{known}, false, "", base.Add(5*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		beforeRun := succeeded
		if got, err := succeeded.Transition(RunStateRunning, "late_observation", base.Add(6*time.Minute)); err == nil || !reflect.DeepEqual(got, beforeRun) {
			t.Fatalf("illegal terminal run transition = %#v, %v; want exact original and error", got, err)
		}
		beforeEffect := known
		if got, err := known.Transition(OutcomeRejected, "late_conflict", base.Add(6*time.Minute)); err == nil || !reflect.DeepEqual(got, beforeEffect) {
			t.Fatalf("illegal terminal effect transition = %#v, %v; want exact original and error", got, err)
		}
		if len(beforeEffect.Attempts) != 1 || beforeEffect.Attempts[0].ReceiptRef != "receipt-A1" {
			t.Fatalf("terminal effect receipt history = %#v; want retained A1", beforeEffect.Attempts)
		}
	})
	t.Run("orchestration_and_cancel_observations_are_orthogonal_to_business_state", func(t *testing.T) {
		observed, err := record.ObserveOrchestration(OrchestrationCompleted, base.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		cancelled, err := observed.RequestCancel()
		if err != nil {
			t.Fatal(err)
		}
		if cancelled.State != RunStateAccepted || !cancelled.CancelRequested || cancelled.OrchestrationCondition != OrchestrationCompleted {
			t.Fatalf("orthogonal run observations rewrote business state: %#v", cancelled)
		}
		observedTask, err := task.ObserveOrchestration(OrchestrationCompleted, base.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		cancelledTask, err := observedTask.RequestCancel()
		if err != nil {
			t.Fatal(err)
		}
		if cancelledTask.State != RunStateAccepted || !cancelledTask.CancelRequested || cancelledTask.OrchestrationCondition != OrchestrationCompleted {
			t.Fatalf("orthogonal task observations rewrote scheduling state: %#v", cancelledTask)
		}
	})

	t.Run("transition_tables_reject_unknown_and_terminal_rewrites", func(t *testing.T) {
		runCases := []struct {
			from, to RunState
			want     bool
		}{
			{RunStateAccepted, RunStateQueued, true},
			{RunStateRunning, RunStateReconciling, true},
			{RunStateReconciling, RunStateBlocked, true},
			{RunStateSucceeded, RunStateRunning, false},
			{RunStateRunning, RunStateRunning, false},
			{RunState("invented"), RunStateRunning, false},
		}
		for _, test := range runCases {
			if got := allowedRunTransition(test.from, test.to); got != test.want {
				t.Errorf("allowedRunTransition(%q, %q) = %t; want %t", test.from, test.to, got, test.want)
			}
		}
		effectCases := []struct {
			from, to OutcomeState
			want     bool
		}{
			{OutcomeUnsent, OutcomeUnknown, true},
			{OutcomeUnknown, OutcomeSucceeded, true},
			{OutcomeUnknown, OutcomeRejected, true},
			{OutcomeSucceeded, OutcomeUnknown, false},
			{OutcomeUnknown, OutcomeUnknown, false},
			{OutcomeState("invented"), OutcomeRejected, false},
		}
		for _, test := range effectCases {
			if got := allowedEffectTransition(test.from, test.to); got != test.want {
				t.Errorf("allowedEffectTransition(%q, %q) = %t; want %t", test.from, test.to, got, test.want)
			}
		}
	})

	t.Run("metadata_case_is_exact_without_rewriting_provider_names", func(t *testing.T) {
		reference := ObjectRef{
			OrgID: "org-alpha", WorkspaceID: "workspace-alpha", ArtifactID: "artifact-original",
			ObjectAttemptKey: "attempt-original", SHA256: strings.Repeat("0", 64),
			ByteLength: 0, MediaType: "application/octet-stream", BackendGeneration: "generation-original",
		}
		encoded, err := json.Marshal(reference)
		if err != nil {
			t.Fatal(err)
		}
		var object ObjectRef
		if err := json.Unmarshal(bytes.Replace(encoded, []byte(`"org_id"`), []byte(`"ORG_ID"`), 1), &object); err == nil {
			t.Error("object reference accepted a wrongly cased metadata key")
		}
		var scope Scope
		if err := json.Unmarshal([]byte(`{"org_id":"org-alpha","ORG_ID":"org-beta","workspace_id":"workspace-alpha"}`), &scope); err == nil {
			t.Error("case-folded alias overwrote an explicitly supplied scope identity")
		}
		if _, err := DecodeSchema([]byte(`{"type":"object","properties":{"ORG_ID":{"TYPE":"string"}}}`)); err == nil {
			t.Error("nested schema accepted a wrongly cased metadata key")
		}
		schema, err := DecodeSchema([]byte(`{"type":"object","properties":{"ORG_ID":{"type":"string"}}}`))
		if err != nil {
			t.Fatal(err)
		}
		if schema.Properties["ORG_ID"].Type != SchemaTypeString {
			t.Fatal("strict metadata decoding changed the exact provider property name")
		}
	})

	t.Run("strict_wire_presence_preserves_zero_and_empty_values", func(t *testing.T) {
		encoded, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		var decoded RunSpec
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("RunSpec round trip: %v", err)
		}
		if decoded.InputRefs == nil || len(decoded.InputRefs) != 0 || decoded.SchemaRefs == nil || decoded.CapabilityVersions == nil {
			t.Fatalf("empty required collections were not preserved: %#v", decoded)
		}
		grantJSON, err := json.Marshal(durableGrant)
		if err != nil {
			t.Fatal(err)
		}
		var decodedGrant ExecutionGrant
		if err := json.Unmarshal(grantJSON, &decodedGrant); err != nil {
			t.Fatalf("ExecutionGrant round trip: %v", err)
		}
		if decodedGrant.PolicyEpochs.Service != 0 || decodedGrant.PolicyEpochs.Delegation != 0 || decodedGrant.PolicyEpochs.Approval != 0 {
			t.Fatalf("explicit zero epochs were not preserved: %#v", decodedGrant.PolicyEpochs)
		}
		objectJSON := []byte(`{"org_id":"org-alpha","workspace_id":"workspace-alpha","artifact_id":"artifact-empty","object_attempt_key":"object-attempt-1","sha256":"0000000000000000000000000000000000000000000000000000000000000000","byte_length":0,"media_type":"application/octet-stream","backend_generation":"generation-1"}`)
		var object ObjectRef
		if err := json.Unmarshal(objectJSON, &object); err != nil {
			t.Fatalf("ObjectRef explicit zero byte_length: %v", err)
		}
		if object.ByteLength != 0 {
			t.Fatalf("ObjectRef byte_length = %d; want explicit zero", object.ByteLength)
		}
		invalid := []string{
			`{"org_id":"org-alpha","workspace_id":"workspace-alpha","extra":0}`,
			`{"org_id":"org-alpha","org_id":"org-beta","workspace_id":"workspace-alpha"}`,
			`{"org_id":null,"workspace_id":"workspace-alpha"}`,
			`{"OrgID":"org-alpha","workspace_id":"workspace-alpha"}`,
			`{"org_id":"org-alpha","workspace_id":"workspace-alpha","artifact_id":"artifact-empty","object_attempt_key":"object-attempt-1","sha256":"0000000000000000000000000000000000000000000000000000000000000000","media_type":"application/octet-stream","backend_generation":"generation-1"}`,
		}
		for _, document := range invalid[:4] {
			var scope Scope
			if err := json.Unmarshal([]byte(document), &scope); err == nil {
				t.Errorf("Scope accepted malformed document %s", document)
			}
		}
		var missingByteLength ObjectRef
		if err := json.Unmarshal([]byte(invalid[4]), &missingByteLength); err == nil {
			t.Fatal("ObjectRef confused missing byte_length with explicit zero")
		}
		summaryJSON, err := json.Marshal(EffectSummary{})
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{`"unsent":0`, `"succeeded":0`, `"unchanged":0`, `"rejected":0`, `"unknown":0`} {
			if !bytes.Contains(summaryJSON, []byte(field)) {
				t.Fatalf("zero summary %s omits required field %s", summaryJSON, field)
			}
		}
		var schedule Schedule
		if err := json.Unmarshal([]byte(`{"schedule_id":"schedule-1","revision":1,"flow_or_profile_revision":"flow-1@1","timezone":"UTC","calendar_spec":"0 0 * * *","overlap":"SKIP","catchup_window":"1h","execution_policy_ref":"policy-1"}`), &schedule); err != nil {
			t.Fatalf("Schedule canonical zero-jitter default: %v", err)
		}
		if schedule.Jitter != "0" {
			t.Fatalf("Schedule jitter = %q; want explicit canonical zero after decode", schedule.Jitter)
		}
	})

	t.Run("published_identity_and_workflow_schemas_execute_against_typed_values", func(t *testing.T) {
		identitySchema := compileIdentitySchemaForTest(t, "identity")
		workflowSchema := compileIdentitySchemaForTest(t, "workflow-ref")
		validateIdentitySchemaValue(t, identitySchema, durableGrant)
		validateIdentitySchemaValue(t, identitySchema, ObjectRef{
			OrgID: spec.OrgID, WorkspaceID: spec.WorkspaceID, ArtifactID: "artifact-empty",
			ObjectAttemptKey: "object-attempt-1", SHA256: strings.Repeat("0", 64), ByteLength: 0,
			MediaType: "application/octet-stream", BackendGeneration: "generation-1",
		})
		validateIdentitySchemaValue(t, workflowSchema, binding)
	})
}

func runSpecForIdentityTest(t *testing.T, base time.Time) (RunSpec, ExecutionGrant) {
	t.Helper()
	digest := SemanticDigest("sha256:0000000000000000000000000000000000000000000000000000000000000000")
	scope := Scope{OrgID: "org-alpha", WorkspaceID: "workspace-alpha"}
	principal := Principal{OrgID: scope.OrgID, PrincipalID: "ada", Kind: PrincipalUser}
	actor := Actor{RequestedActor: principal, EffectiveActor: principal}
	grant := ExecutionGrant{
		GrantRef: "grant-1", GrantEpoch: 3, Scope: scope, Actor: actor,
		ConnectionIDs: []string{"connection-1"}, OperationIDs: []string{"github.issues.create"}, ApprovalRefs: []string{},
		ExpiresAt: base.Add(72 * time.Hour), PolicyEpochs: PolicyEpochs{Membership: 2, Connection: 5},
	}
	if err := grant.Validate(); err != nil {
		t.Fatalf("valid ExecutionGrant fixture: %v", err)
	}
	spec := RunSpec{
		RunID: "00000000-0000-4000-8000-000000000001", OrgID: scope.OrgID, WorkspaceID: scope.WorkspaceID,
		Actor: actor, ExecutionGrantRef: grant.GrantRef, Origin: RunOriginAPI,
		OperationOrPlanRef: OperationOrPlanRef{Kind: OperationReference, ID: "github.issues.create", Revision: "operation-revision-1"},
		InputRefs:          []string{}, SchemaRefs: []string{}, CapabilityVersions: []CapabilityRequirement{},
		Budget: BudgetBinding{BudgetRef: "budget-1", Revision: 1, Digest: digest}, IdempotencyFingerprint: digest,
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("valid RunSpec fixture: %v", err)
	}
	return spec, grant
}

func taskForIdentityTest(spec RunSpec, base time.Time) Task {
	return Task{
		TaskID: "task-1", RunID: spec.RunID, OrgID: spec.OrgID, WorkspaceID: spec.WorkspaceID,
		OperationID: "github.issues.create", Lane: LaneDirectWrite, BehaviorID: "behavior-1",
		InputRefs: []string{}, SchemaRefs: []string{}, State: RunStateAccepted, EffectSummary: EffectSummary{},
		UnknownEffectCount: 0, CancelRequested: false, OrchestrationCondition: OrchestrationPending,
		ProjectionObservedAt: base, Revision: 1,
	}
}

func effectForIdentityTest(t *testing.T, spec RunSpec) EffectRecord {
	t.Helper()
	entity, err := NewEntityKey("account-witness-1", "issue", "42", "generation-1")
	if err != nil {
		t.Fatal(err)
	}
	key, err := NewEffectKey(entity, "occurrence-1", "effect-revision-1")
	if err != nil {
		t.Fatal(err)
	}
	effect, err := NewEffectRecord(spec.OrgID, spec.WorkspaceID, spec.RunID, "task-1", key)
	if err != nil {
		t.Fatal(err)
	}
	return effect
}

func compileIdentitySchemaForTest(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
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
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	if err := compiler.AddResource(path, document); err != nil {
		t.Fatal(err)
	}
	compiled, err := compiler.Compile(path)
	if err != nil {
		t.Fatalf("%s schema cannot execute with the selected validator: %v", name, err)
	}
	return compiled
}

func validateIdentitySchemaValue(t *testing.T, schema *jsonschema.Schema, value any) {
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
		t.Fatalf("typed wire value %s does not satisfy the published schema: %v", encoded, err)
	}
}
