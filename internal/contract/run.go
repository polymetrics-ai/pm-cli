package contract

import (
	"slices"
	"time"
)

type RunOrigin string

const (
	RunOriginAPI      RunOrigin = "api"
	RunOriginSchedule RunOrigin = "schedule"
	RunOriginRecovery RunOrigin = "recovery"
)

type OperationOrPlanKind string

const (
	OperationReference OperationOrPlanKind = "operation"
	PlanReference      OperationOrPlanKind = "plan"
)

// OperationOrPlanRef selects exactly one immutable operation or sealed plan.
type OperationOrPlanRef struct {
	Kind     OperationOrPlanKind `json:"kind"`
	ID       string              `json:"id"`
	Revision string              `json:"revision"`
}

func (r *OperationOrPlanRef) UnmarshalJSON(data []byte) error {
	type wire OperationOrPlanRef
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "kind", "id", "revision"); err != nil {
		return err
	}
	value := OperationOrPlanRef(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*r = value
	return nil
}

func (r OperationOrPlanRef) Validate() error {
	switch r.Kind {
	case OperationReference, PlanReference:
	default:
		return contractError(ErrInvalidContract, "operation_or_plan_ref.kind", "must be operation or plan")
	}
	if !validIdentityPart(r.ID) {
		return contractError(ErrInvalidIdentity, "operation_or_plan_ref.id", "must be a nonempty immutable identity")
	}
	if !validIdentityPart(r.Revision) {
		return contractError(ErrInvalidIdentity, "operation_or_plan_ref.revision", "must bind an immutable revision")
	}
	return nil
}

// BudgetBinding binds the accepted run to one immutable budget snapshot without
// inventing a second budget authority in the pure contract package.
type BudgetBinding struct {
	BudgetRef string         `json:"budget_ref"`
	Revision  uint64         `json:"revision"`
	Digest    SemanticDigest `json:"digest"`
}

func (b *BudgetBinding) UnmarshalJSON(data []byte) error {
	type wire BudgetBinding
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "budget_ref", "revision", "digest"); err != nil {
		return err
	}
	value := BudgetBinding(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*b = value
	return nil
}

func (b BudgetBinding) Validate() error {
	if !validIdentityPart(b.BudgetRef) {
		return contractError(ErrInvalidIdentity, "budget.budget_ref", "must identify one immutable budget snapshot")
	}
	if b.Revision == 0 {
		return contractError(ErrInvalidIdentity, "budget.revision", "must be a positive immutable revision")
	}
	if err := b.Digest.Validate(); err != nil {
		return err
	}
	return nil
}

// RunSpec is the immutable accepted-intent shape. Its grant reference binds a
// separately persisted grant whose lifetime is independent of login sessions.
type RunSpec struct {
	RunID                  string                  `json:"run_id"`
	OrgID                  string                  `json:"org_id"`
	WorkspaceID            string                  `json:"workspace_id"`
	Actor                  Actor                   `json:"actor"`
	ExecutionGrantRef      string                  `json:"execution_grant_ref"`
	Origin                 RunOrigin               `json:"origin"`
	OperationOrPlanRef     OperationOrPlanRef      `json:"operation_or_plan_ref"`
	InputRefs              []string                `json:"input_refs"`
	SchemaRefs             []string                `json:"schema_refs"`
	CapabilityVersions     []CapabilityRequirement `json:"capability_versions"`
	Budget                 BudgetBinding           `json:"budget"`
	IdempotencyFingerprint SemanticDigest          `json:"idempotency_fingerprint"`
}

func (s *RunSpec) UnmarshalJSON(data []byte) error {
	type wire RunSpec
	var decoded wire
	if err := decodeRequiredObject(data, &decoded,
		"run_id", "org_id", "workspace_id", "actor", "execution_grant_ref",
		"origin", "operation_or_plan_ref", "input_refs", "schema_refs",
		"capability_versions", "budget", "idempotency_fingerprint",
	); err != nil {
		return err
	}
	value := RunSpec(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*s = value
	return nil
}

func (s RunSpec) Validate() error {
	if !validIdentityPart(s.RunID) {
		return contractError(ErrInvalidIdentity, "run_spec.run_id", "must identify one stable PM run")
	}
	scope := Scope{OrgID: s.OrgID, WorkspaceID: s.WorkspaceID}
	if err := scope.Validate(); err != nil {
		return err
	}
	if err := s.Actor.Validate(); err != nil {
		return err
	}
	if !validIdentityPart(s.ExecutionGrantRef) {
		return contractError(ErrInvalidIdentity, "run_spec.execution_grant_ref", "must identify one immutable durable grant")
	}
	switch s.Origin {
	case RunOriginAPI, RunOriginSchedule, RunOriginRecovery:
	default:
		return contractError(ErrInvalidContract, "run_spec.origin", "origin is unsupported")
	}
	if err := s.OperationOrPlanRef.Validate(); err != nil {
		return err
	}
	if err := validateIdentityList(s.InputRefs, "run_spec.input_refs", true); err != nil {
		return err
	}
	if err := validateIdentityList(s.SchemaRefs, "run_spec.schema_refs", true); err != nil {
		return err
	}
	if s.CapabilityVersions == nil {
		return contractError(ErrInvalidContract, "run_spec.capability_versions", "must be explicitly present, even when empty")
	}
	seenCapabilities := make(map[string]struct{}, len(s.CapabilityVersions))
	for _, capability := range s.CapabilityVersions {
		if err := capability.Validate(); err != nil {
			return err
		}
		key := capability.Capability + "\x00" + capability.Foundation.ID + "@" + capability.Foundation.Version
		if _, duplicate := seenCapabilities[key]; duplicate {
			return contractError(ErrDuplicateField, "run_spec.capability_versions", "capability version is duplicated")
		}
		seenCapabilities[key] = struct{}{}
	}
	if err := s.Budget.Validate(); err != nil {
		return err
	}
	if err := s.IdempotencyFingerprint.Validate(); err != nil {
		return err
	}
	return nil
}

type RunState string

const (
	RunStateAccepted         RunState = "accepted"
	RunStateQueued           RunState = "queued"
	RunStateRunning          RunState = "running"
	RunStateAwaitingApproval RunState = "awaiting_approval"
	RunStateBlocked          RunState = "blocked"
	RunStateReconciling      RunState = "reconciling"
	RunStateSucceeded        RunState = "succeeded"
	RunStatePartial          RunState = "partial"
	RunStateFailed           RunState = "failed"
	RunStateCancelled        RunState = "cancelled"
)

type OrchestrationCondition string

const (
	OrchestrationPending           OrchestrationCondition = "pending"
	OrchestrationRunning           OrchestrationCondition = "running"
	OrchestrationCompleted         OrchestrationCondition = "completed"
	OrchestrationFailed            OrchestrationCondition = "failed"
	OrchestrationCancelled         OrchestrationCondition = "cancelled"
	OrchestrationTerminated        OrchestrationCondition = "terminated"
	OrchestrationTimedOut          OrchestrationCondition = "timed_out"
	OrchestrationProjectionPending OrchestrationCondition = "projection_pending"
	OrchestrationUnknown           OrchestrationCondition = "unknown"
)

type RunTransition struct {
	From       RunState  `json:"from"`
	To         RunState  `json:"to"`
	Reason     string    `json:"reason"`
	ObservedAt time.Time `json:"observed_at"`
}

func (t RunTransition) Validate() error {
	if !validRunState(t.From) || !validRunState(t.To) || !allowedRunTransition(t.From, t.To) {
		return contractError(ErrInvalidContract, "run_transition", "contains an illegal state transition")
	}
	if !validReasonCode(t.Reason) {
		return contractError(ErrInvalidContract, "run_transition.reason", "must be a safe explicit reason code")
	}
	return validateUTCInstant(t.ObservedAt, "run_transition.observed_at")
}

// RunRecord keeps mutable projection fields separate from the immutable spec.
type RunRecord struct {
	Spec                   RunSpec                `json:"spec"`
	WorkflowBinding        WorkflowBinding        `json:"workflow_binding"`
	State                  RunState               `json:"state"`
	EffectSummary          EffectSummary          `json:"effect_summary"`
	CancelRequested        bool                   `json:"cancel_requested"`
	OrchestrationCondition OrchestrationCondition `json:"orchestration_condition"`
	ProjectionObservedAt   time.Time              `json:"projection_observed_at"`
	UnknownEffectCount     uint64                 `json:"unknown_effect_count"`
	BlockedReason          string                 `json:"blocked_reason,omitempty"`
	Revision               uint64                 `json:"revision"`
	History                []RunTransition        `json:"history"`
}

func NewRunRecord(spec RunSpec, binding WorkflowBinding, acceptedAt time.Time) (RunRecord, error) {
	record := RunRecord{
		Spec:                   spec,
		WorkflowBinding:        binding,
		State:                  RunStateAccepted,
		EffectSummary:          EffectSummary{},
		CancelRequested:        false,
		OrchestrationCondition: OrchestrationPending,
		ProjectionObservedAt:   acceptedAt,
		UnknownEffectCount:     0,
		Revision:               1,
		History:                []RunTransition{},
	}
	if err := record.Validate(); err != nil {
		return RunRecord{}, err
	}
	// Own the accepted snapshot even when callers reuse their request containers.
	record.Spec.InputRefs = slices.Clone(spec.InputRefs)
	record.Spec.SchemaRefs = slices.Clone(spec.SchemaRefs)
	record.Spec.CapabilityVersions = slices.Clone(spec.CapabilityVersions)
	if spec.Actor.Delegation != nil {
		delegation := *spec.Actor.Delegation
		delegation.ParentCeiling.ConnectionIDs = slices.Clone(delegation.ParentCeiling.ConnectionIDs)
		delegation.ParentCeiling.OperationIDs = slices.Clone(delegation.ParentCeiling.OperationIDs)
		delegation.ParentCeiling.ApprovalRefs = slices.Clone(delegation.ParentCeiling.ApprovalRefs)
		record.Spec.Actor.Delegation = &delegation
	}
	record.WorkflowBinding.Observations = slices.Clone(binding.Observations)
	return record, nil
}

func (r *RunRecord) UnmarshalJSON(data []byte) error {
	type wire RunRecord
	var decoded wire
	if err := decodeRequiredObject(data, &decoded,
		"spec", "workflow_binding", "state", "effect_summary", "cancel_requested",
		"orchestration_condition", "projection_observed_at", "unknown_effect_count",
		"revision", "history",
	); err != nil {
		return err
	}
	value := RunRecord(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*r = value
	return nil
}

func (r RunRecord) Validate() error {
	if err := r.Spec.Validate(); err != nil {
		return err
	}
	if err := r.WorkflowBinding.Validate(); err != nil {
		return err
	}
	if r.WorkflowBinding.RunID != r.Spec.RunID || r.WorkflowBinding.Origin != r.Spec.Origin {
		return contractError(ErrInvalidIdentity, "run_record.workflow_binding", "must preserve the run and origin of the immutable spec")
	}
	if !validRunState(r.State) {
		return contractError(ErrInvalidContract, "run_record.state", "run state is unsupported")
	}
	if err := r.EffectSummary.Validate(); err != nil {
		return err
	}
	if r.UnknownEffectCount != r.EffectSummary.Unknown {
		return contractError(ErrInvalidContract, "run_record.unknown_effect_count", "must equal the exact effect summary")
	}
	if r.UnknownEffectCount != 0 && r.State != RunStateReconciling {
		return contractError(ErrInvalidContract, "run_record.state", "unknown effects require reconciling aggregate state")
	}
	if r.BlockedReason != "" && !validReasonCode(r.BlockedReason) {
		return contractError(ErrInvalidContract, "run_record.blocked_reason", "must be a safe explicit reason code")
	}
	if r.State == RunStateBlocked {
		if r.UnknownEffectCount != 0 || r.BlockedReason == "" {
			return contractError(ErrInvalidContract, "run_record.blocked_reason", "blocked requires a known-effect aggregate and an explicit safe reason")
		}
	} else if r.State != RunStateReconciling && r.BlockedReason != "" {
		return contractError(ErrInvalidContract, "run_record.blocked_reason", "is only retained for blocked or uncertainty-dominant reconciliation")
	}
	if terminalRunState(r.State) && r.UnknownEffectCount != 0 {
		return contractError(ErrInvalidContract, "run_record.state", "terminal state cannot hide an unknown effect")
	}
	if !validOrchestrationCondition(r.OrchestrationCondition) {
		return contractError(ErrInvalidContract, "run_record.orchestration_condition", "orchestration observation is unsupported")
	}
	if err := validateUTCInstant(r.ProjectionObservedAt, "run_record.projection_observed_at"); err != nil {
		return err
	}
	if r.Revision == 0 {
		return contractError(ErrInvalidContract, "run_record.revision", "must be a positive state revision")
	}
	if r.History == nil {
		return contractError(ErrInvalidContract, "run_record.history", "must be explicitly present, even when empty")
	}
	if r.Revision < uint64(len(r.History))+1 {
		return contractError(ErrInvalidContract, "run_record.revision", "cannot precede its append-only transition history")
	}
	state := RunStateAccepted
	var previous time.Time
	for _, transition := range r.History {
		if err := transition.Validate(); err != nil {
			return err
		}
		if transition.From != state {
			return contractError(ErrInvalidContract, "run_record.history", "transition history is not contiguous")
		}
		if !previous.IsZero() && transition.ObservedAt.Before(previous) {
			return contractError(ErrInvalidContract, "run_record.history", "transition history is not chronological")
		}
		state = transition.To
		previous = transition.ObservedAt
	}
	if state != r.State {
		return contractError(ErrInvalidContract, "run_record.history", "history does not end at the current state")
	}
	return nil
}

func (r RunRecord) Transition(next RunState, reason string, observedAt time.Time) (RunRecord, error) {
	if err := r.Validate(); err != nil {
		return r, err
	}
	if terminalRunState(r.State) || !allowedRunTransition(r.State, next) {
		return r, contractError(ErrInvalidContract, "run_record.state", "transition is not allowed")
	}
	if !validReasonCode(reason) {
		return r, contractError(ErrInvalidContract, "run_transition.reason", "must be a safe explicit reason code")
	}
	if err := validateUTCInstant(observedAt, "run_transition.observed_at"); err != nil {
		return r, err
	}
	if (next == RunStateBlocked || terminalRunState(next)) && r.UnknownEffectCount != 0 {
		return r, contractError(ErrInvalidContract, "run_record.state", "unknown effects must remain reconciling")
	}
	updated := r
	updated.History = append(append([]RunTransition(nil), r.History...), RunTransition{
		From: r.State, To: next, Reason: reason, ObservedAt: observedAt,
	})
	updated.State = next
	updated.Revision++
	switch next {
	case RunStateBlocked:
		updated.BlockedReason = reason
	case RunStateReconciling:
		// A previously observed policy reason remains orthogonal to uncertainty.
	default:
		updated.BlockedReason = ""
	}
	if err := updated.Validate(); err != nil {
		return r, err
	}
	return updated, nil
}

func (r RunRecord) ReconcileEffects(effects []EffectRecord, policyBlocked bool, blockedReason string, observedAt time.Time) (RunRecord, error) {
	if err := r.Validate(); err != nil {
		return r, err
	}
	if terminalRunState(r.State) {
		return r, contractError(ErrInvalidContract, "run_record.state", "terminal state cannot be rewritten by later orchestration observations")
	}
	if err := validateUTCInstant(observedAt, "run_record.projection_observed_at"); err != nil {
		return r, err
	}
	if observedAt.Before(r.ProjectionObservedAt) {
		return r, contractError(ErrInvalidContract, "run_record.projection_observed_at", "cannot move projection time backwards")
	}
	if policyBlocked && !validReasonCode(blockedReason) {
		return r, contractError(ErrInvalidContract, "run_record.blocked_reason", "policy blockage requires an explicit safe reason")
	}
	summary, err := summarizeEffects(r.Spec, effects)
	if err != nil {
		return r, err
	}
	target := aggregateRunState(r.State, summary, policyBlocked)
	if target != r.State && !allowedRunTransition(r.State, target) {
		return r, contractError(ErrInvalidContract, "run_record.state", "aggregate requires an illegal state transition")
	}
	updated := r
	updated.EffectSummary = summary
	updated.UnknownEffectCount = summary.Unknown
	updated.ProjectionObservedAt = observedAt
	updated.Revision++
	switch {
	case summary.Unknown != 0 && policyBlocked:
		updated.BlockedReason = blockedReason
	case target == RunStateBlocked:
		updated.BlockedReason = blockedReason
	default:
		updated.BlockedReason = ""
	}
	if target != r.State {
		reason := "effect_aggregate"
		if summary.Unknown != 0 {
			reason = "unknown_effects"
		} else if target == RunStateBlocked {
			reason = blockedReason
		}
		updated.History = append(append([]RunTransition(nil), r.History...), RunTransition{
			From: r.State, To: target, Reason: reason, ObservedAt: observedAt,
		})
		updated.State = target
	} else {
		updated.History = append([]RunTransition(nil), r.History...)
	}
	if err := updated.Validate(); err != nil {
		return r, err
	}
	return updated, nil
}

func (r RunRecord) RequestCancel() (RunRecord, error) {
	if err := r.Validate(); err != nil {
		return r, err
	}
	if terminalRunState(r.State) {
		return r, contractError(ErrInvalidContract, "run_record.cancel_requested", "terminal run cannot accept a new cancel request")
	}
	if r.CancelRequested {
		return r, nil
	}
	updated := r
	updated.CancelRequested = true
	updated.Revision++
	if err := updated.Validate(); err != nil {
		return r, err
	}
	return updated, nil
}

func (r RunRecord) ObserveOrchestration(condition OrchestrationCondition, observedAt time.Time) (RunRecord, error) {
	if err := r.Validate(); err != nil {
		return r, err
	}
	if !validOrchestrationCondition(condition) {
		return r, contractError(ErrInvalidContract, "run_record.orchestration_condition", "orchestration observation is unsupported")
	}
	if err := validateUTCInstant(observedAt, "run_record.projection_observed_at"); err != nil {
		return r, err
	}
	if observedAt.Before(r.ProjectionObservedAt) {
		return r, contractError(ErrInvalidContract, "run_record.projection_observed_at", "cannot move projection time backwards")
	}
	updated := r
	updated.OrchestrationCondition = condition
	updated.ProjectionObservedAt = observedAt
	updated.Revision++
	if err := updated.Validate(); err != nil {
		return r, err
	}
	return updated, nil
}

func validRunState(state RunState) bool {
	switch state {
	case RunStateAccepted, RunStateQueued, RunStateRunning, RunStateAwaitingApproval,
		RunStateBlocked, RunStateReconciling, RunStateSucceeded, RunStatePartial,
		RunStateFailed, RunStateCancelled:
		return true
	default:
		return false
	}
}

func terminalRunState(state RunState) bool {
	switch state {
	case RunStateSucceeded, RunStatePartial, RunStateFailed, RunStateCancelled:
		return true
	default:
		return false
	}
}

func allowedRunTransition(from, to RunState) bool {
	if from == to || !validRunState(from) || !validRunState(to) || terminalRunState(from) {
		return false
	}
	switch from {
	case RunStateAccepted:
		return to == RunStateQueued || to == RunStateRunning || to == RunStateAwaitingApproval ||
			to == RunStateBlocked || to == RunStateReconciling || to == RunStateFailed || to == RunStateCancelled
	case RunStateQueued:
		return to == RunStateRunning || to == RunStateAwaitingApproval || to == RunStateBlocked ||
			to == RunStateReconciling || to == RunStateFailed || to == RunStateCancelled
	case RunStateRunning:
		return to == RunStateQueued || to == RunStateAwaitingApproval || to == RunStateBlocked || to == RunStateReconciling || terminalRunState(to)
	case RunStateAwaitingApproval:
		return to == RunStateQueued || to == RunStateRunning || to == RunStateBlocked ||
			to == RunStateReconciling || to == RunStateFailed || to == RunStateCancelled
	case RunStateBlocked:
		return to == RunStateQueued || to == RunStateRunning || to == RunStateAwaitingApproval ||
			to == RunStateReconciling || to == RunStateFailed || to == RunStateCancelled
	case RunStateReconciling:
		return to == RunStateQueued || to == RunStateRunning || to == RunStateAwaitingApproval ||
			to == RunStateBlocked || terminalRunState(to)
	default:
		return false
	}
}

func validOrchestrationCondition(condition OrchestrationCondition) bool {
	switch condition {
	case OrchestrationPending, OrchestrationRunning, OrchestrationCompleted,
		OrchestrationFailed, OrchestrationCancelled, OrchestrationTerminated,
		OrchestrationTimedOut, OrchestrationProjectionPending, OrchestrationUnknown:
		return true
	default:
		return false
	}
}

func summarizeEffects(spec RunSpec, effects []EffectRecord) (EffectSummary, error) {
	if effects == nil {
		return EffectSummary{}, contractError(ErrInvalidContract, "effects", "must be explicitly present, even when empty")
	}
	var summary EffectSummary
	seen := make(map[EffectKey]struct{}, len(effects))
	for _, effect := range effects {
		if err := effect.Validate(); err != nil {
			return EffectSummary{}, err
		}
		if effect.RunID != spec.RunID || effect.OrgID != spec.OrgID || effect.WorkspaceID != spec.WorkspaceID {
			return EffectSummary{}, contractError(ErrInvalidIdentity, "effects", "effect does not belong to the immutable run scope")
		}
		if _, duplicate := seen[effect.EffectKey]; duplicate {
			return EffectSummary{}, contractError(ErrDuplicateField, "effects", "effect identity is duplicated")
		}
		seen[effect.EffectKey] = struct{}{}
		switch effect.State {
		case OutcomeUnsent:
			summary.Unsent++
		case OutcomeSucceeded:
			summary.Succeeded++
		case OutcomeUnchanged:
			summary.Unchanged++
		case OutcomeRejected:
			summary.Rejected++
		case OutcomeUnknown:
			summary.Unknown++
		}
	}
	return summary, nil
}

func aggregateRunState(current RunState, summary EffectSummary, policyBlocked bool) RunState {
	if summary.Unknown != 0 {
		return RunStateReconciling
	}
	if policyBlocked {
		return RunStateBlocked
	}
	if summary.Total() == 0 {
		return current
	}
	if summary.Unsent != 0 {
		switch current {
		case RunStateAccepted, RunStateQueued, RunStateAwaitingApproval:
			return current
		default:
			return RunStateRunning
		}
	}
	knownSuccess := summary.Succeeded + summary.Unchanged
	switch {
	case summary.Rejected == 0:
		return RunStateSucceeded
	case knownSuccess == 0:
		return RunStateFailed
	default:
		return RunStatePartial
	}
}
