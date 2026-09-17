package contract

import (
	"slices"
	"time"
)

// Attempt represents an immutable send-attempt receipt reference. Validation
// preserves this evidence shape but does not prove that storage is durable.
type Attempt struct {
	AttemptID  AttemptID  `json:"attempt_id"`
	DispatchID DispatchID `json:"dispatch_id"`
	ReceiptRef string     `json:"receipt_ref"`
	RecordedAt time.Time  `json:"recorded_at"`
}

func (a *Attempt) UnmarshalJSON(data []byte) error {
	type wire Attempt
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "attempt_id", "dispatch_id", "receipt_ref", "recorded_at"); err != nil {
		return err
	}
	value := Attempt(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*a = value
	return nil
}

func (a Attempt) Validate() error {
	if err := a.AttemptID.Validate(); err != nil {
		return err
	}
	if err := a.DispatchID.Validate(); err != nil {
		return err
	}
	if !validIdentityPart(a.ReceiptRef) {
		return contractError(ErrInvalidIdentity, "attempt.receipt_ref", "must bind one durable attempt receipt")
	}
	return validateUTCInstant(a.RecordedAt, "attempt.recorded_at")
}

type EffectTransition struct {
	From       OutcomeState `json:"from"`
	To         OutcomeState `json:"to"`
	Reason     string       `json:"reason"`
	ObservedAt time.Time    `json:"observed_at"`
}

func (t *EffectTransition) UnmarshalJSON(data []byte) error {
	type wire EffectTransition
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "from", "to", "reason", "observed_at"); err != nil {
		return err
	}
	value := EffectTransition(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*t = value
	return nil
}

func (t EffectTransition) Validate() error {
	legal := allowedEffectTransition(t.From, t.To) ||
		(t.From == OutcomeUnknown && t.To == OutcomeUnknown && t.Reason == "timeout")
	if !legal {
		return contractError(ErrInvalidContract, "effect_transition", "contains an illegal state transition")
	}
	if !validReasonCode(t.Reason) {
		return contractError(ErrInvalidContract, "effect_transition.reason", "must be a safe explicit reason code")
	}
	return validateUTCInstant(t.ObservedAt, "effect_transition.observed_at")
}

// EffectRecord owns one logical effect identity and its append-only attempts and
// transition history. Unknown is a first-class state, never an error alias.
type EffectRecord struct {
	OrgID       string             `json:"org_id"`
	WorkspaceID string             `json:"workspace_id"`
	RunID       string             `json:"run_id"`
	TaskID      string             `json:"task_id"`
	EffectKey   EffectKey          `json:"effect_key"`
	State       OutcomeState       `json:"state"`
	Attempts    []Attempt          `json:"attempts"`
	Revision    uint64             `json:"revision"`
	History     []EffectTransition `json:"history"`
}

func NewEffectRecord(orgID, workspaceID, runID, taskID string, effectKey EffectKey) (EffectRecord, error) {
	record := EffectRecord{
		OrgID: orgID, WorkspaceID: workspaceID, RunID: runID, TaskID: taskID,
		EffectKey: effectKey, State: OutcomeUnsent, Attempts: []Attempt{}, Revision: 1,
		History: []EffectTransition{},
	}
	if err := record.Validate(); err != nil {
		return EffectRecord{}, err
	}
	return record, nil
}

func (r *EffectRecord) UnmarshalJSON(data []byte) error {
	type wire EffectRecord
	var decoded wire
	if err := decodeRequiredObject(data, &decoded,
		"org_id", "workspace_id", "run_id", "task_id", "effect_key", "state",
		"attempts", "revision", "history",
	); err != nil {
		return err
	}
	value := EffectRecord(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*r = value
	return nil
}

func (r EffectRecord) Validate() error {
	if err := (Scope{OrgID: r.OrgID, WorkspaceID: r.WorkspaceID}).Validate(); err != nil {
		return err
	}
	if !validIdentityPart(r.RunID) {
		return contractError(ErrInvalidIdentity, "effect_record.run_id", "must bind one stable PM run")
	}
	if !validIdentityPart(r.TaskID) {
		return contractError(ErrInvalidIdentity, "effect_record.task_id", "must bind one immutable task")
	}
	if err := r.EffectKey.Validate(); err != nil {
		return err
	}
	if !validOutcomeState(r.State) {
		return contractError(ErrInvalidContract, "effect_record.state", "effect state is unsupported")
	}
	if r.Attempts == nil {
		return contractError(ErrInvalidContract, "effect_record.attempts", "must be explicitly present, even when empty")
	}
	seenAttempts := make(map[AttemptID]struct{}, len(r.Attempts))
	seenDispatches := make(map[DispatchID]struct{}, len(r.Attempts))
	seenReceipts := make(map[string]struct{}, len(r.Attempts))
	var previousAttempt time.Time
	for _, attempt := range r.Attempts {
		if err := attempt.Validate(); err != nil {
			return err
		}
		if _, duplicate := seenAttempts[attempt.AttemptID]; duplicate {
			return contractError(ErrDuplicateField, "effect_record.attempts.attempt_id", "attempt identity is duplicated")
		}
		if _, duplicate := seenDispatches[attempt.DispatchID]; duplicate {
			return contractError(ErrDuplicateField, "effect_record.attempts.dispatch_id", "dispatch identity is duplicated")
		}
		if _, duplicate := seenReceipts[attempt.ReceiptRef]; duplicate {
			return contractError(ErrDuplicateField, "effect_record.attempts.receipt_ref", "receipt reference is duplicated")
		}
		if !previousAttempt.IsZero() && attempt.RecordedAt.Before(previousAttempt) {
			return contractError(ErrInvalidContract, "effect_record.attempts", "attempts are not chronological")
		}
		seenAttempts[attempt.AttemptID] = struct{}{}
		seenDispatches[attempt.DispatchID] = struct{}{}
		seenReceipts[attempt.ReceiptRef] = struct{}{}
		previousAttempt = attempt.RecordedAt
	}
	if r.State == OutcomeUnsent && len(r.Attempts) != 0 {
		return contractError(ErrInvalidContract, "effect_record.attempts", "unsent effect cannot have a send-attempt receipt")
	}
	if r.Revision == 0 {
		return contractError(ErrInvalidContract, "effect_record.revision", "must be a positive state revision")
	}
	if r.History == nil {
		return contractError(ErrInvalidContract, "effect_record.history", "must be explicitly present, even when empty")
	}
	if r.Revision != uint64(len(r.History))+1 {
		return contractError(ErrInvalidContract, "effect_record.revision", "must equal its append-only transition history revision")
	}
	state := OutcomeUnsent
	var previousTransition time.Time
	for _, transition := range r.History {
		if err := transition.Validate(); err != nil {
			return err
		}
		if transition.From != state {
			return contractError(ErrInvalidContract, "effect_record.history", "transition history is not contiguous")
		}
		if !previousTransition.IsZero() && transition.ObservedAt.Before(previousTransition) {
			return contractError(ErrInvalidContract, "effect_record.history", "transition history is not chronological")
		}
		state = transition.To
		previousTransition = transition.ObservedAt
	}
	if state != r.State {
		return contractError(ErrInvalidContract, "effect_record.history", "history does not end at the current state")
	}
	return nil
}

func (r EffectRecord) RecordAttempt(attempt Attempt) (EffectRecord, error) {
	if err := r.Validate(); err != nil {
		return r, err
	}
	if r.State != OutcomeUnsent {
		return r, contractError(ErrInvalidContract, "effect_record.state", "an attempt can only dispatch an unsent logical effect")
	}
	if err := attempt.Validate(); err != nil {
		return r, err
	}
	updated := r
	updated.Attempts = append(append([]Attempt(nil), r.Attempts...), attempt)
	updated.History = append(append([]EffectTransition(nil), r.History...), EffectTransition{
		From: OutcomeUnsent, To: OutcomeUnknown, Reason: "dispatched", ObservedAt: attempt.RecordedAt,
	})
	updated.State = OutcomeUnknown
	updated.Revision++
	if err := updated.Validate(); err != nil {
		return r, err
	}
	return updated, nil
}

// ObserveTimeout records an observation while preserving the unknown state and
// every durable attempt receipt.
func (r EffectRecord) ObserveTimeout(observedAt time.Time) (EffectRecord, error) {
	if err := r.Validate(); err != nil {
		return r, err
	}
	if r.State != OutcomeUnknown {
		return r, contractError(ErrInvalidContract, "effect_record.state", "timeout is only meaningful for an unknown dispatched effect")
	}
	if err := validateUTCInstant(observedAt, "effect_transition.observed_at"); err != nil {
		return r, err
	}
	if len(r.History) != 0 && observedAt.Before(r.History[len(r.History)-1].ObservedAt) {
		return r, contractError(ErrInvalidContract, "effect_record.history", "cannot move observation time backwards")
	}
	updated := r
	updated.Attempts = slices.Clone(r.Attempts)
	updated.History = append(append([]EffectTransition(nil), r.History...), EffectTransition{
		From: OutcomeUnknown, To: OutcomeUnknown, Reason: "timeout", ObservedAt: observedAt,
	})
	updated.Revision++
	if err := updated.Validate(); err != nil {
		return r, err
	}
	return updated, nil
}

func (r EffectRecord) Transition(next OutcomeState, reason string, observedAt time.Time) (EffectRecord, error) {
	if err := r.Validate(); err != nil {
		return r, err
	}
	if !allowedEffectTransition(r.State, next) {
		return r, contractError(ErrInvalidContract, "effect_record.state", "transition is not allowed")
	}
	if !validReasonCode(reason) {
		return r, contractError(ErrInvalidContract, "effect_transition.reason", "must be a safe explicit reason code")
	}
	if err := validateUTCInstant(observedAt, "effect_transition.observed_at"); err != nil {
		return r, err
	}
	if len(r.History) != 0 && observedAt.Before(r.History[len(r.History)-1].ObservedAt) {
		return r, contractError(ErrInvalidContract, "effect_record.history", "cannot move observation time backwards")
	}
	updated := r
	updated.Attempts = slices.Clone(r.Attempts)
	updated.History = append(append([]EffectTransition(nil), r.History...), EffectTransition{
		From: r.State, To: next, Reason: reason, ObservedAt: observedAt,
	})
	updated.State = next
	updated.Revision++
	if err := updated.Validate(); err != nil {
		return r, err
	}
	return updated, nil
}

func validOutcomeState(state OutcomeState) bool {
	switch state {
	case OutcomeUnsent, OutcomeSucceeded, OutcomeUnchanged, OutcomeRejected, OutcomeUnknown:
		return true
	default:
		return false
	}
}

func terminalEffectState(state OutcomeState) bool {
	switch state {
	case OutcomeSucceeded, OutcomeUnchanged, OutcomeRejected:
		return true
	default:
		return false
	}
}

func allowedEffectTransition(from, to OutcomeState) bool {
	if from == to || !validOutcomeState(from) || !validOutcomeState(to) || terminalEffectState(from) {
		return false
	}
	switch from {
	case OutcomeUnsent:
		return to == OutcomeSucceeded || to == OutcomeUnchanged || to == OutcomeRejected || to == OutcomeUnknown
	case OutcomeUnknown:
		return to == OutcomeSucceeded || to == OutcomeUnchanged || to == OutcomeRejected
	default:
		return false
	}
}
