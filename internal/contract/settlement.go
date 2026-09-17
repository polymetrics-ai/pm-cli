package contract

import "time"

type OutcomeState string

const (
	OutcomeUnsent    OutcomeState = "unsent"
	OutcomeSucceeded OutcomeState = "succeeded"
	OutcomeUnchanged OutcomeState = "unchanged"
	OutcomeRejected  OutcomeState = "rejected"
	OutcomeUnknown   OutcomeState = "unknown"
)

type MemberOutcome struct {
	MemberID      MemberID     `json:"member_id"`
	EffectKey     EffectKey    `json:"effect_key"`
	DispatchID    DispatchID   `json:"dispatch_id"`
	State         OutcomeState `json:"state"`
	ObservedAt    time.Time    `json:"observed_at"`
	AssertionRefs []string     `json:"assertion_refs"`
}

func (o MemberOutcome) Validate() error {
	if err := o.MemberID.Validate(); err != nil {
		return err
	}
	if err := o.EffectKey.Validate(); err != nil {
		return err
	}
	if err := o.DispatchID.Validate(); err != nil {
		return err
	}
	switch o.State {
	case OutcomeUnsent, OutcomeSucceeded, OutcomeUnchanged, OutcomeRejected, OutcomeUnknown:
	default:
		return contractError(ErrInvalidSettlement, "member_outcome.state", "state is unsupported")
	}
	if o.ObservedAt.IsZero() {
		return contractError(ErrInvalidSettlement, "member_outcome.observed_at", "must be recorded explicitly")
	}
	_, offset := o.ObservedAt.Zone()
	if offset != 0 {
		return contractError(ErrInvalidSettlement, "member_outcome.observed_at", "must be normalized to UTC")
	}
	if o.AssertionRefs == nil {
		return contractError(ErrInvalidSettlement, "member_outcome.assertion_refs", "must be explicitly present, even when empty")
	}
	seen := make(map[string]struct{}, len(o.AssertionRefs))
	for _, reference := range o.AssertionRefs {
		if !validIdentityPart(reference) {
			return contractError(ErrInvalidSettlement, "member_outcome.assertion_refs", "contains an invalid safe reference")
		}
		if _, duplicate := seen[reference]; duplicate {
			return contractError(ErrDuplicateField, "member_outcome.assertion_refs", "reference is duplicated")
		}
		seen[reference] = struct{}{}
	}
	return nil
}

// ValidateSettlement requires an explicit outcome, including unknown, for every ordered member.
// It never derives member success from aggregate counts or response position.
func ValidateSettlement(dispatch Dispatch, outcomes []MemberOutcome) error {
	if err := dispatch.Validate(); err != nil {
		return err
	}
	if len(outcomes) != len(dispatch.OrderedMemberIDs) {
		return contractError(ErrInvalidSettlement, "member_outcomes", "must explicitly settle every ordered member")
	}
	expected := make(map[MemberID]struct{}, len(dispatch.OrderedMemberIDs))
	for _, memberID := range dispatch.OrderedMemberIDs {
		expected[memberID] = struct{}{}
	}
	seen := make(map[MemberID]struct{}, len(outcomes))
	for _, outcome := range outcomes {
		if err := outcome.Validate(); err != nil {
			return err
		}
		if _, ok := expected[outcome.MemberID]; !ok {
			return contractError(ErrInvalidSettlement, "member_outcomes.member_id", "outcome is not part of the sealed dispatch")
		}
		if _, duplicate := seen[outcome.MemberID]; duplicate {
			return contractError(ErrDuplicateField, "member_outcomes.member_id", "member outcome is duplicated")
		}
		if outcome.DispatchID != dispatch.DispatchID || outcome.EffectKey != dispatch.EffectKey {
			return contractError(ErrInvalidSettlement, "member_outcomes", "effect or dispatch identity does not match sealed intent")
		}
		seen[outcome.MemberID] = struct{}{}
	}
	return nil
}
