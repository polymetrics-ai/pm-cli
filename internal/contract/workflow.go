package contract

import (
	"strings"
	"time"
)

const WorkflowReferenceSchemaVersion = "pm.workflow-ref/v1"

type WorkflowObservation struct {
	TemporalRunID string    `json:"temporal_run_id"`
	ObservedAt    time.Time `json:"observed_at"`
}

func (o *WorkflowObservation) UnmarshalJSON(data []byte) error {
	type wire WorkflowObservation
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "temporal_run_id", "observed_at"); err != nil {
		return err
	}
	value := WorkflowObservation(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*o = value
	return nil
}

func (o WorkflowObservation) Validate() error {
	if !validIdentityPart(o.TemporalRunID) {
		return contractError(ErrInvalidIdentity, "workflow_binding.observations.temporal_run_id", "must identify one Temporal execution-chain member")
	}
	return validateUTCInstant(o.ObservedAt, "workflow_binding.observations.observed_at")
}

// WorkflowBinding maps mutable Temporal chain observations to one stable PM
// run. Schedule-origin bindings explicitly adopt the already-running,
// effect-free execution instead of requesting a second workflow start.
type WorkflowBinding struct {
	RunID        string                `json:"run_id"`
	Origin       RunOrigin             `json:"origin"`
	WorkflowID   string                `json:"workflow_id"`
	Adopted      bool                  `json:"adopted"`
	Generation   uint64                `json:"generation"`
	Observations []WorkflowObservation `json:"observations"`
}

func NewAPIWorkflowBinding(runID string) (WorkflowBinding, error) {
	binding := WorkflowBinding{
		RunID:        runID,
		Origin:       RunOriginAPI,
		WorkflowID:   "pm/run/" + runID,
		Adopted:      false,
		Generation:   1,
		Observations: []WorkflowObservation{},
	}
	if err := binding.Validate(); err != nil {
		return WorkflowBinding{}, err
	}
	return binding, nil
}

func AdoptScheduledWorkflow(runID, workflowID, temporalRunID string, observedAt time.Time) (WorkflowBinding, error) {
	binding := WorkflowBinding{
		RunID:      runID,
		Origin:     RunOriginSchedule,
		WorkflowID: workflowID,
		Adopted:    true,
		Generation: 1,
		Observations: []WorkflowObservation{{
			TemporalRunID: temporalRunID,
			ObservedAt:    observedAt,
		}},
	}
	if err := binding.Validate(); err != nil {
		return WorkflowBinding{}, err
	}
	return binding, nil
}

func (b *WorkflowBinding) UnmarshalJSON(data []byte) error {
	type wire WorkflowBinding
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "run_id", "origin", "workflow_id", "adopted", "generation", "observations"); err != nil {
		return err
	}
	value := WorkflowBinding(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*b = value
	return nil
}

func (b WorkflowBinding) Validate() error {
	if !validIdentityPart(b.RunID) {
		return contractError(ErrInvalidIdentity, "workflow_binding.run_id", "must identify one stable PM run")
	}
	if !validIdentityPart(b.WorkflowID) {
		return contractError(ErrInvalidIdentity, "workflow_binding.workflow_id", "must identify one Temporal workflow")
	}
	if b.Generation == 0 {
		return contractError(ErrInvalidIdentity, "workflow_binding.generation", "must be a positive orchestration generation")
	}
	if b.Observations == nil {
		return contractError(ErrInvalidIdentity, "workflow_binding.observations", "must be explicitly present, even before the first Temporal observation")
	}
	switch b.Origin {
	case RunOriginAPI:
		if b.Adopted {
			return contractError(ErrInvalidIdentity, "workflow_binding.adopted", "API-origin workflows are started, not adopted")
		}
		if !validOpaqueUUID(b.RunID) || b.WorkflowID != "pm/run/"+b.RunID {
			return contractError(ErrInvalidIdentity, "workflow_binding.workflow_id", "API origin must use pm/run/<opaque UUID> for the same PM run")
		}
	case RunOriginSchedule:
		if !b.Adopted || len(b.Observations) == 0 {
			return contractError(ErrInvalidIdentity, "workflow_binding", "schedule origin must adopt its already-running effect-free execution")
		}
	case RunOriginRecovery:
		if b.Adopted || b.Generation < 2 {
			return contractError(ErrInvalidIdentity, "workflow_binding", "recovery must use a later explicit orchestration generation")
		}
	default:
		return contractError(ErrInvalidIdentity, "workflow_binding.origin", "origin is unsupported")
	}
	seen := make(map[string]struct{}, len(b.Observations))
	var previous time.Time
	for _, observation := range b.Observations {
		if err := observation.Validate(); err != nil {
			return err
		}
		if _, duplicate := seen[observation.TemporalRunID]; duplicate {
			return contractError(ErrDuplicateField, "workflow_binding.observations", "Temporal run identity is duplicated")
		}
		if !previous.IsZero() && observation.ObservedAt.Before(previous) {
			return contractError(ErrInvalidIdentity, "workflow_binding.observations", "observations must retain chronological chain order")
		}
		seen[observation.TemporalRunID] = struct{}{}
		previous = observation.ObservedAt
	}
	return nil
}

func (b WorkflowBinding) ObserveTemporalRun(temporalRunID string, observedAt time.Time) (WorkflowBinding, error) {
	if err := b.Validate(); err != nil {
		return b, err
	}
	observation := WorkflowObservation{TemporalRunID: temporalRunID, ObservedAt: observedAt}
	if err := observation.Validate(); err != nil {
		return b, err
	}
	for _, existing := range b.Observations {
		if existing.TemporalRunID == temporalRunID {
			return b, contractError(ErrDuplicateField, "workflow_binding.observations", "Temporal run identity is already recorded")
		}
	}
	if len(b.Observations) != 0 && observedAt.Before(b.Observations[len(b.Observations)-1].ObservedAt) {
		return b, contractError(ErrInvalidIdentity, "workflow_binding.observations", "new observation cannot precede existing chain history")
	}
	updated := b
	updated.Observations = append(append([]WorkflowObservation(nil), b.Observations...), observation)
	return updated, nil
}

func (b WorkflowBinding) CurrentTemporalRunID() string {
	if len(b.Observations) == 0 {
		return ""
	}
	return b.Observations[len(b.Observations)-1].TemporalRunID
}

func validOpaqueUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return strings.ContainsRune("89ab", rune(value[19]))
}
