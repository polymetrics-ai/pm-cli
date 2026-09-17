package contract

import "time"

// EffectSummary is an exact count projection. Every counter is serialized,
// including zero, so absence cannot masquerade as a known zero.
type EffectSummary struct {
	Unsent    uint64 `json:"unsent"`
	Succeeded uint64 `json:"succeeded"`
	Unchanged uint64 `json:"unchanged"`
	Rejected  uint64 `json:"rejected"`
	Unknown   uint64 `json:"unknown"`
}

func (s *EffectSummary) UnmarshalJSON(data []byte) error {
	type wire EffectSummary
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "unsent", "succeeded", "unchanged", "rejected", "unknown"); err != nil {
		return err
	}
	value := EffectSummary(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*s = value
	return nil
}

func (s EffectSummary) Validate() error {
	values := [...]uint64{s.Unsent, s.Succeeded, s.Unchanged, s.Rejected, s.Unknown}
	var total uint64
	for _, value := range values {
		if ^uint64(0)-total < value {
			return contractError(ErrInvalidContract, "effect_summary", "counter total overflows uint64")
		}
		total += value
	}
	return nil
}

func (s EffectSummary) Total() uint64 {
	return s.Unsent + s.Succeeded + s.Unchanged + s.Rejected + s.Unknown
}

// Task separates scheduling state from effect disposition, cancel intent and
// orchestration observations.
type Task struct {
	TaskID                 string                 `json:"task_id"`
	RunID                  string                 `json:"run_id"`
	OrgID                  string                 `json:"org_id"`
	WorkspaceID            string                 `json:"workspace_id"`
	OperationID            string                 `json:"operation_id"`
	Lane                   Lane                   `json:"lane"`
	BehaviorID             string                 `json:"behavior_id"`
	InputRefs              []string               `json:"input_refs"`
	SchemaRefs             []string               `json:"schema_refs"`
	State                  RunState               `json:"state"`
	EffectSummary          EffectSummary          `json:"effect_summary"`
	UnknownEffectCount     uint64                 `json:"unknown_effect_count"`
	CancelRequested        bool                   `json:"cancel_requested"`
	OrchestrationCondition OrchestrationCondition `json:"orchestration_condition"`
	ProjectionObservedAt   time.Time              `json:"projection_observed_at"`
	BlockedReason          string                 `json:"blocked_reason,omitempty"`
	Revision               uint64                 `json:"revision"`
}

func (t *Task) UnmarshalJSON(data []byte) error {
	type wire Task
	var decoded wire
	if err := decodeRequiredObject(data, &decoded,
		"task_id", "run_id", "org_id", "workspace_id", "operation_id", "lane",
		"behavior_id", "input_refs", "schema_refs", "state", "effect_summary",
		"unknown_effect_count", "cancel_requested", "orchestration_condition",
		"projection_observed_at", "revision",
	); err != nil {
		return err
	}
	value := Task(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*t = value
	return nil
}

func (t Task) Validate() error {
	identities := [...]struct {
		path  string
		value string
	}{
		{path: "task.task_id", value: t.TaskID},
		{path: "task.run_id", value: t.RunID},
		{path: "task.operation_id", value: t.OperationID},
		{path: "task.behavior_id", value: t.BehaviorID},
	}
	for _, identity := range identities {
		if !validIdentityPart(identity.value) {
			return contractError(ErrInvalidIdentity, identity.path, "must be a nonempty immutable identity")
		}
	}
	if err := (Scope{OrgID: t.OrgID, WorkspaceID: t.WorkspaceID}).Validate(); err != nil {
		return err
	}
	if !validLane(t.Lane) {
		return contractError(ErrInvalidContract, "task.lane", "must be a supported scheduling lane")
	}
	if err := validateIdentityList(t.InputRefs, "task.input_refs", true); err != nil {
		return err
	}
	if err := validateIdentityList(t.SchemaRefs, "task.schema_refs", true); err != nil {
		return err
	}
	if !validRunState(t.State) {
		return contractError(ErrInvalidContract, "task.state", "task scheduling state is unsupported")
	}
	if err := t.EffectSummary.Validate(); err != nil {
		return err
	}
	if t.UnknownEffectCount != t.EffectSummary.Unknown {
		return contractError(ErrInvalidContract, "task.unknown_effect_count", "must equal the exact effect summary")
	}
	if t.UnknownEffectCount != 0 && t.State != RunStateReconciling {
		return contractError(ErrInvalidContract, "task.state", "unknown effects require reconciling scheduling state")
	}
	if t.BlockedReason != "" && !validReasonCode(t.BlockedReason) {
		return contractError(ErrInvalidContract, "task.blocked_reason", "must be a safe explicit reason code")
	}
	if t.State == RunStateBlocked {
		if t.UnknownEffectCount != 0 || t.BlockedReason == "" {
			return contractError(ErrInvalidContract, "task.blocked_reason", "blocked requires all effects known and an explicit safe reason")
		}
	} else if t.State != RunStateReconciling && t.BlockedReason != "" {
		return contractError(ErrInvalidContract, "task.blocked_reason", "is only retained for blocked or uncertainty-dominant reconciliation")
	}
	if terminalRunState(t.State) && t.UnknownEffectCount != 0 {
		return contractError(ErrInvalidContract, "task.state", "terminal task state cannot hide an unknown effect")
	}
	if !validOrchestrationCondition(t.OrchestrationCondition) {
		return contractError(ErrInvalidContract, "task.orchestration_condition", "orchestration observation is unsupported")
	}
	if err := validateUTCInstant(t.ProjectionObservedAt, "task.projection_observed_at"); err != nil {
		return err
	}
	if t.Revision == 0 {
		return contractError(ErrInvalidContract, "task.revision", "must be a positive state revision")
	}
	return nil
}

func (t Task) ApplyEffectSummary(summary EffectSummary, policyBlocked bool, blockedReason string, observedAt time.Time) (Task, error) {
	if err := t.Validate(); err != nil {
		return t, err
	}
	if terminalRunState(t.State) {
		return t, contractError(ErrInvalidContract, "task.state", "terminal task cannot be rewritten by a later observation")
	}
	if err := summary.Validate(); err != nil {
		return t, err
	}
	if err := validateUTCInstant(observedAt, "task.projection_observed_at"); err != nil {
		return t, err
	}
	if observedAt.Before(t.ProjectionObservedAt) {
		return t, contractError(ErrInvalidContract, "task.projection_observed_at", "cannot move projection time backwards")
	}
	if policyBlocked && !validReasonCode(blockedReason) {
		return t, contractError(ErrInvalidContract, "task.blocked_reason", "policy blockage requires an explicit safe reason")
	}
	target := aggregateRunState(t.State, summary, policyBlocked)
	if target != t.State && !allowedRunTransition(t.State, target) {
		return t, contractError(ErrInvalidContract, "task.state", "aggregate requires an illegal scheduling transition")
	}
	updated := t
	updated.EffectSummary = summary
	updated.UnknownEffectCount = summary.Unknown
	updated.State = target
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
	if err := updated.Validate(); err != nil {
		return t, err
	}
	return updated, nil
}

func (t Task) RequestCancel() (Task, error) {
	if err := t.Validate(); err != nil {
		return t, err
	}
	if terminalRunState(t.State) {
		return t, contractError(ErrInvalidContract, "task.cancel_requested", "terminal task cannot accept a new cancel request")
	}
	if t.CancelRequested {
		return t, nil
	}
	updated := t
	updated.CancelRequested = true
	updated.Revision++
	if err := updated.Validate(); err != nil {
		return t, err
	}
	return updated, nil
}

func (t Task) ObserveOrchestration(condition OrchestrationCondition, observedAt time.Time) (Task, error) {
	if err := t.Validate(); err != nil {
		return t, err
	}
	if !validOrchestrationCondition(condition) {
		return t, contractError(ErrInvalidContract, "task.orchestration_condition", "orchestration observation is unsupported")
	}
	if err := validateUTCInstant(observedAt, "task.projection_observed_at"); err != nil {
		return t, err
	}
	if observedAt.Before(t.ProjectionObservedAt) {
		return t, contractError(ErrInvalidContract, "task.projection_observed_at", "cannot move projection time backwards")
	}
	updated := t
	updated.OrchestrationCondition = condition
	updated.ProjectionObservedAt = observedAt
	updated.Revision++
	if err := updated.Validate(); err != nil {
		return t, err
	}
	return updated, nil
}
