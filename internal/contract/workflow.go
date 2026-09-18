package contract

import (
	"context"
	"strings"
	"time"
)

const WorkflowReferenceSchemaVersion = "pm.workflow-ref/v1"

// QuantizedTemporalDuration returns the effective duration the Temporal boundary
// records for a sealed plan. Temporal stores timeouts with second granularity, so
// positive sub-second precision rounds up to the next whole second; zero and
// negative values stay zero. Every comparison against a value Temporal reports
// back must use this quantization.
func QuantizedTemporalDuration(value time.Duration) time.Duration {
	if value <= 0 {
		return 0
	}
	remainder := value % time.Second
	if remainder == 0 {
		return value
	}
	increment := time.Second - remainder
	if value > time.Duration(1<<63-1)-increment {
		return 0
	}
	return value + increment
}

// These fixed values are the only SDK version-marker scalars admitted to history.
const WorkflowRunDeadlineProjectionChangeID = "run-deadline-projection"
const WorkflowRunDeadlineProjectionVersion = 1

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

const (
	ControlTaskQueue      = "pm-control"
	RunWorkflowType       = "pm.run.v1"
	ScheduledWorkflowType = "pm.scheduled-run.v1"
	ProjectionUpdateName  = "pm.retry-final-projection.v1"
	ControlSignalName     = "pm.control-notify.v1"
	ControlUpdateName     = "pm.control-command.v1"
	LoadActivityName      = "pm.load-run.v1"
	ExecuteActivityName   = "pm.execute-unit.v1"
	CommandActivityName   = "pm.apply-control.v1"
	ProjectActivityName   = "pm.finalize-run.v1"
	ScheduleActivityName  = "pm.admit-scheduled-run.v1"
	StartRunInputSchema   = "pm.workflow-start/v1"
	ProjectionRetrySchema = "pm.projection-retry/v1"
)

const MaxWorkflowFanout uint32 = 32

// StartRunInput is the complete safe workflow argument. It carries only
// durable identities and a sealed semantic digest; raw inputs, actors,
// credentials, and provider payloads never enter workflow history.
type StartRunInput struct {
	Schema       string         `json:"schema"`
	Scope        Scope          `json:"scope"`
	RunID        string         `json:"run_id"`
	CommandID    string         `json:"command_id"`
	WorkflowID   string         `json:"workflow_id"`
	Generation   uint64         `json:"generation"`
	Origin       RunOrigin      `json:"origin"`
	IntentDigest SemanticDigest `json:"intent_digest"`
}

func (input *StartRunInput) UnmarshalJSON(data []byte) error {
	type wire StartRunInput
	var decoded wire
	if err := decodeRequiredObject(data, &decoded,
		"schema", "scope", "run_id", "command_id", "workflow_id",
		"generation", "origin", "intent_digest",
	); err != nil {
		return err
	}
	value := StartRunInput(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*input = value
	return nil
}

func (input StartRunInput) Validate() error {
	if input.Schema != StartRunInputSchema {
		return contractError(ErrUnsupportedVersion, "workflow_start.schema", "workflow start input schema is unsupported")
	}
	if err := input.Scope.Validate(); err != nil {
		return err
	}
	if !validIdentityPart(input.RunID) || !validIdentityPart(input.CommandID) ||
		!validIdentityPart(input.WorkflowID) || input.Generation == 0 {
		return contractError(ErrInvalidIdentity, "workflow_start", "workflow start identity is invalid")
	}
	switch input.Origin {
	case RunOriginAPI:
		if input.WorkflowID != "pm/run/"+input.RunID {
			return contractError(ErrInvalidIdentity, "workflow_start.workflow_id", "API workflow must use pm/run/<run_id>")
		}
	case RunOriginSchedule, RunOriginRecovery:
	default:
		return contractError(ErrInvalidContract, "workflow_start.origin", "workflow start origin is unsupported")
	}
	if err := input.IntentDigest.Validate(); err != nil {
		return err
	}
	return nil
}

// ValidateRelayStart preserves the API/recovery start boundary. Scheduled
// execution is admitted inside PMScheduledRunWorkflow and is never started by
// the API relay.
func (input StartRunInput) ValidateRelayStart() error {
	if err := input.Validate(); err != nil {
		return err
	}
	if input.Origin != RunOriginAPI && input.Origin != RunOriginRecovery {
		return contractError(ErrInvalidContract, "workflow_start.origin", "API relay cannot start a scheduled workflow")
	}
	return nil
}

func (input StartRunInput) ValidateScheduledStart() error {
	if err := input.Validate(); err != nil {
		return err
	}
	if input.Origin != RunOriginSchedule {
		return contractError(ErrInvalidContract, "workflow_start.origin", "scheduled admission must return schedule origin")
	}
	return nil
}

func (input StartRunInput) RunRef() WorkflowRunRef {
	return WorkflowRunRef{
		Scope:          input.Scope,
		RunID:          input.RunID,
		WorkflowID:     input.WorkflowID,
		StartCommandID: input.CommandID,
		Generation:     input.Generation,
		IntentDigest:   input.IntentDigest,
	}
}

// ProjectionRetryCommand asks a running workflow to retry only its owned final
// PostgreSQL projection. Its digest binds the repair command, not the original
// workflow-start intent.
type ProjectionRetryCommand struct {
	Schema       string         `json:"schema"`
	Scope        Scope          `json:"scope"`
	RunID        string         `json:"run_id"`
	WorkflowID   string         `json:"workflow_id"`
	Generation   uint64         `json:"generation"`
	IntentDigest SemanticDigest `json:"intent_digest"`
}

func (command *ProjectionRetryCommand) UnmarshalJSON(data []byte) error {
	type wire ProjectionRetryCommand
	var decoded wire
	if err := decodeRequiredObject(data, &decoded,
		"schema", "scope", "run_id", "workflow_id", "generation", "intent_digest",
	); err != nil {
		return err
	}
	value := ProjectionRetryCommand(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*command = value
	return nil
}

func (command ProjectionRetryCommand) Validate() error {
	if command.Schema != ProjectionRetrySchema {
		return contractError(ErrUnsupportedVersion, "projection_retry.schema", "projection retry schema is unsupported")
	}
	if err := command.Scope.Validate(); err != nil {
		return err
	}
	if !validIdentityPart(command.RunID) || !validIdentityPart(command.WorkflowID) || command.Generation == 0 {
		return contractError(ErrInvalidIdentity, "projection_retry", "projection retry identity is invalid")
	}
	if err := command.IntentDigest.Validate(); err != nil {
		return err
	}
	return nil
}

type WorkflowRunRef struct {
	Scope          Scope          `json:"scope"`
	RunID          string         `json:"run_id"`
	WorkflowID     string         `json:"workflow_id"`
	StartCommandID string         `json:"start_command_id"`
	Generation     uint64         `json:"generation"`
	IntentDigest   SemanticDigest `json:"intent_digest"`
}

func (reference *WorkflowRunRef) UnmarshalJSON(data []byte) error {
	type wire WorkflowRunRef
	var decoded wire
	if err := decodeRequiredObject(data, &decoded,
		"scope", "run_id", "workflow_id", "start_command_id", "generation", "intent_digest",
	); err != nil {
		return err
	}
	value := WorkflowRunRef(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*reference = value
	return nil
}

func (reference WorkflowRunRef) Validate() error {
	if err := reference.Scope.Validate(); err != nil {
		return err
	}
	if !validIdentityPart(reference.RunID) || !validIdentityPart(reference.WorkflowID) ||
		!validIdentityPart(reference.StartCommandID) || reference.Generation == 0 {
		return contractError(ErrInvalidIdentity, "workflow_run_ref", "workflow run reference is incomplete")
	}
	if err := reference.IntentDigest.Validate(); err != nil {
		return err
	}
	return nil
}

type WorkflowUnitRef struct {
	TaskID     string     `json:"task_id"`
	DispatchID DispatchID `json:"dispatch_id,omitempty"`
}

func (reference *WorkflowUnitRef) UnmarshalJSON(data []byte) error {
	type wire WorkflowUnitRef
	var decoded wire
	fields, err := decodeRequiredObjectFields(data, &decoded, "task_id")
	if err != nil {
		return err
	}
	if raw, present := fields["dispatch_id"]; present &&
		(strings.TrimSpace(string(raw)) == "null" || decoded.DispatchID == "") {
		return contractError(ErrInvalidIdentity, "workflow_unit_ref.dispatch_id", "must identify one exact durable dispatch when present")
	}
	value := WorkflowUnitRef(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*reference = value
	return nil
}

func (reference WorkflowUnitRef) Validate() error {
	if !validIdentityPart(reference.TaskID) {
		return contractError(ErrInvalidIdentity, "workflow_unit_ref.task_id", "must identify one durable task")
	}
	if reference.DispatchID != "" {
		if err := reference.DispatchID.Validate(); err != nil {
			return err
		}
	}
	return nil
}

type WorkflowCommandRef struct {
	CommandID string `json:"command_id"`
}

func (reference *WorkflowCommandRef) UnmarshalJSON(data []byte) error {
	type wire WorkflowCommandRef
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "command_id"); err != nil {
		return err
	}
	value := WorkflowCommandRef(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*reference = value
	return nil
}

func (reference WorkflowCommandRef) Validate() error {
	if !validIdentityPart(reference.CommandID) {
		return contractError(ErrInvalidIdentity, "workflow_command_ref.command_id", "must identify one durable command")
	}
	return nil
}

type ActivityTimeouts struct {
	ScheduleToClose time.Duration `json:"schedule_to_close"`
	StartToClose    time.Duration `json:"start_to_close"`
	Heartbeat       time.Duration `json:"heartbeat"`
	Transport       time.Duration `json:"transport"`
}

func (timeouts *ActivityTimeouts) UnmarshalJSON(data []byte) error {
	type wire ActivityTimeouts
	var decoded wire
	if err := decodeRequiredObject(data, &decoded,
		"schedule_to_close", "start_to_close", "heartbeat", "transport",
	); err != nil {
		return err
	}
	value := ActivityTimeouts(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*timeouts = value
	return nil
}

func (timeouts ActivityTimeouts) Validate() error {
	if timeouts.ScheduleToClose <= 0 || timeouts.StartToClose <= 0 ||
		timeouts.Heartbeat <= 0 || timeouts.Transport <= 0 {
		return contractError(ErrInvalidContract, "activity_timeouts", "all activity timeouts must be finite and positive")
	}
	if timeouts.ScheduleToClose <= timeouts.StartToClose {
		return contractError(ErrInvalidContract, "activity_timeouts.schedule_to_close", "must exceed the per-attempt timeout")
	}
	if timeouts.Heartbeat >= timeouts.StartToClose {
		return contractError(ErrInvalidContract, "activity_timeouts.heartbeat", "must be shorter than the per-attempt timeout")
	}
	if timeouts.Transport >= timeouts.StartToClose {
		return contractError(ErrInvalidContract, "activity_timeouts.transport", "must be shorter than the per-attempt timeout")
	}
	return nil
}

type WorkflowRunSnapshot struct {
	Run             WorkflowRunRef       `json:"run"`
	Deadline        time.Time            `json:"deadline"`
	RetryNotBefore  *time.Time           `json:"retry_not_before,omitempty"`
	Timeouts        ActivityTimeouts     `json:"timeouts"`
	Units           []WorkflowUnitRef    `json:"units"`
	Commands        []WorkflowCommandRef `json:"commands"`
	Done            bool                 `json:"done"`
	CancelRequested bool                 `json:"cancel_requested"`
	Fanout          uint32               `json:"fanout"`
}

func (snapshot *WorkflowRunSnapshot) UnmarshalJSON(data []byte) error {
	type wire WorkflowRunSnapshot
	var decoded wire
	fields, err := decodeRequiredObjectFields(data, &decoded,
		"run", "deadline", "timeouts", "units", "commands",
		"done", "cancel_requested", "fanout",
	)
	if err != nil {
		return err
	}
	if raw, present := fields["retry_not_before"]; present &&
		strings.TrimSpace(string(raw)) == "null" {
		return contractError(ErrInvalidContract, "workflow_run_snapshot.retry_not_before", "explicit null cannot stand in for absent retry eligibility")
	}
	value := WorkflowRunSnapshot(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*snapshot = value
	return nil
}

func (snapshot WorkflowRunSnapshot) Validate() error {
	if err := snapshot.Run.Validate(); err != nil {
		return err
	}
	if err := validateUTCInstant(snapshot.Deadline, "workflow_run_snapshot.deadline"); err != nil {
		return err
	}
	if snapshot.RetryNotBefore != nil {
		if err := validateUTCInstant(*snapshot.RetryNotBefore, "workflow_run_snapshot.retry_not_before"); err != nil {
			return err
		}
		if snapshot.RetryNotBefore.After(snapshot.Deadline) {
			return contractError(ErrInvalidContract, "workflow_run_snapshot.retry_not_before", "cannot exceed the immutable run deadline")
		}
		if len(snapshot.Units) != 0 {
			return contractError(ErrInvalidContract, "workflow_run_snapshot.retry_not_before", "must be absent while a unit is runnable")
		}
		if snapshot.Done {
			return contractError(ErrInvalidContract, "workflow_run_snapshot.retry_not_before", "must be absent after coordination is complete")
		}
	}
	if err := snapshot.Timeouts.Validate(); err != nil {
		return err
	}
	if snapshot.Units == nil || snapshot.Commands == nil {
		return contractError(ErrInvalidContract, "workflow_run_snapshot", "unit and command references must be explicitly present")
	}
	if snapshot.Fanout == 0 || snapshot.Fanout > MaxWorkflowFanout {
		return contractError(ErrInvalidContract, "workflow_run_snapshot.fanout", "must be between one and 32")
	}
	seenUnits := make(map[string]struct{}, len(snapshot.Units))
	seenDispatches := make(map[DispatchID]struct{}, len(snapshot.Units))
	hasQualifiedUnits := false
	hasUnqualifiedUnits := false
	for _, unit := range snapshot.Units {
		if err := unit.Validate(); err != nil {
			return err
		}
		if _, duplicate := seenUnits[unit.TaskID]; duplicate {
			return contractError(ErrDuplicateField, "workflow_run_snapshot.units", "task identity is duplicated")
		}
		seenUnits[unit.TaskID] = struct{}{}
		if unit.DispatchID == "" {
			hasUnqualifiedUnits = true
			continue
		}
		hasQualifiedUnits = true
		if _, duplicate := seenDispatches[unit.DispatchID]; duplicate {
			return contractError(ErrDuplicateField, "workflow_run_snapshot.units", "dispatch identity is duplicated")
		}
		seenDispatches[unit.DispatchID] = struct{}{}
	}
	if hasQualifiedUnits && hasUnqualifiedUnits {
		return contractError(ErrInvalidContract, "workflow_run_snapshot.units", "cannot mix retained initial and qualified dispatch references")
	}
	seenCommands := make(map[string]struct{}, len(snapshot.Commands))
	for _, command := range snapshot.Commands {
		if err := command.Validate(); err != nil {
			return err
		}
		if _, duplicate := seenCommands[command.CommandID]; duplicate {
			return contractError(ErrDuplicateField, "workflow_run_snapshot.commands", "command identity is duplicated")
		}
		seenCommands[command.CommandID] = struct{}{}
	}
	return nil
}

type WorkflowUnitRequest struct {
	Run  WorkflowRunRef  `json:"run"`
	Unit WorkflowUnitRef `json:"unit"`
}

func (request *WorkflowUnitRequest) UnmarshalJSON(data []byte) error {
	type wire WorkflowUnitRequest
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "run", "unit"); err != nil {
		return err
	}
	value := WorkflowUnitRequest(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*request = value
	return nil
}

func (request WorkflowUnitRequest) Validate() error {
	if err := request.Run.Validate(); err != nil {
		return err
	}
	return request.Unit.Validate()
}

type WorkflowUnitResult struct {
	TaskID             string   `json:"task_id"`
	State              RunState `json:"state"`
	UnknownEffectCount uint64   `json:"unknown_effect_count"`
	ResultRef          string   `json:"result_ref"`
}

func (result *WorkflowUnitResult) UnmarshalJSON(data []byte) error {
	type wire WorkflowUnitResult
	var decoded wire
	if err := decodeRequiredObject(data, &decoded,
		"task_id", "state", "unknown_effect_count", "result_ref",
	); err != nil {
		return err
	}
	value := WorkflowUnitResult(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*result = value
	return nil
}

func (result WorkflowUnitResult) Validate() error {
	if !validIdentityPart(result.TaskID) || !validIdentityPart(result.ResultRef) {
		return contractError(ErrInvalidIdentity, "workflow_unit_result", "task and result references are required")
	}
	if !validRunState(result.State) {
		return contractError(ErrInvalidContract, "workflow_unit_result.state", "run state is unsupported")
	}
	if result.UnknownEffectCount != 0 && result.State != RunStateReconciling {
		return contractError(ErrInvalidContract, "workflow_unit_result.state", "unknown effects require reconciling state")
	}
	return nil
}

type WorkflowCommandRequest struct {
	Run     WorkflowRunRef     `json:"run"`
	Command WorkflowCommandRef `json:"command"`
}

func (request *WorkflowCommandRequest) UnmarshalJSON(data []byte) error {
	type wire WorkflowCommandRequest
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "run", "command"); err != nil {
		return err
	}
	value := WorkflowCommandRequest(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*request = value
	return nil
}

func (request WorkflowCommandRequest) Validate() error {
	if err := request.Run.Validate(); err != nil {
		return err
	}
	return request.Command.Validate()
}

type WorkflowCommandResult struct {
	CommandID       string `json:"command_id"`
	Applied         bool   `json:"applied"`
	CancelRequested bool   `json:"cancel_requested"`
}

func (result *WorkflowCommandResult) UnmarshalJSON(data []byte) error {
	type wire WorkflowCommandResult
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "command_id", "applied", "cancel_requested"); err != nil {
		return err
	}
	value := WorkflowCommandResult(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*result = value
	return nil
}

func (result WorkflowCommandResult) Validate() error {
	if !validIdentityPart(result.CommandID) {
		return contractError(ErrInvalidIdentity, "workflow_command_result.command_id", "must identify one durable command")
	}
	return nil
}

type WorkflowProjectionRequest struct {
	Run                WorkflowRunRef `json:"run"`
	RepairIntentDigest SemanticDigest `json:"repair_intent_digest"`
}

func (request *WorkflowProjectionRequest) UnmarshalJSON(data []byte) error {
	type wire WorkflowProjectionRequest
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "run", "repair_intent_digest"); err != nil {
		return err
	}
	value := WorkflowProjectionRequest(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*request = value
	return nil
}

func (request WorkflowProjectionRequest) Validate() error {
	if err := request.Run.Validate(); err != nil {
		return err
	}
	if request.RepairIntentDigest == "" {
		return nil
	}
	if err := request.RepairIntentDigest.Validate(); err != nil {
		return err
	}
	if request.RepairIntentDigest == request.Run.IntentDigest {
		return contractError(ErrInvalidIdentity, "workflow_projection_request.repair_intent_digest", "repair digest must differ from the start intent digest")
	}
	return nil
}

type WorkflowProjectionResult struct {
	RunID    string   `json:"run_id"`
	Revision uint64   `json:"revision"`
	State    RunState `json:"state"`
}

func (result *WorkflowProjectionResult) UnmarshalJSON(data []byte) error {
	type wire WorkflowProjectionResult
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "run_id", "revision", "state"); err != nil {
		return err
	}
	value := WorkflowProjectionResult(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*result = value
	return nil
}

func (result WorkflowProjectionResult) Validate() error {
	if !validIdentityPart(result.RunID) || result.Revision == 0 {
		return contractError(ErrInvalidIdentity, "workflow_projection_result", "run identity and positive revision are required")
	}
	if !validRunState(result.State) {
		return contractError(ErrInvalidContract, "workflow_projection_result.state", "run state is unsupported")
	}
	return nil
}

type WorkflowProgress struct {
	RunID       string `json:"run_id"`
	TaskID      string `json:"task_id"`
	ProgressRef string `json:"progress_ref"`
}

func (progress *WorkflowProgress) UnmarshalJSON(data []byte) error {
	type wire WorkflowProgress
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "run_id", "task_id", "progress_ref"); err != nil {
		return err
	}
	value := WorkflowProgress(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*progress = value
	return nil
}

func (progress WorkflowProgress) Validate() error {
	if !validIdentityPart(progress.RunID) || !validIdentityPart(progress.TaskID) ||
		!validIdentityPart(progress.ProgressRef) {
		return contractError(ErrInvalidIdentity, "workflow_progress", "run, task, and progress references are required")
	}
	return nil
}

type WorkflowExecution struct {
	Namespace  string `json:"namespace"`
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id"`
}

func (execution *WorkflowExecution) UnmarshalJSON(data []byte) error {
	type wire WorkflowExecution
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "namespace", "workflow_id", "run_id"); err != nil {
		return err
	}
	value := WorkflowExecution(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*execution = value
	return nil
}

func (execution WorkflowExecution) Validate() error {
	if !validIdentityPart(execution.Namespace) || !validIdentityPart(execution.WorkflowID) ||
		!validIdentityPart(execution.RunID) {
		return contractError(ErrInvalidIdentity, "workflow_execution", "trusted Temporal execution identity is incomplete")
	}
	return nil
}

// WorkflowContinuationObservation is private verification evidence produced
// from Temporal's actual Describe and history APIs. It is never workflow wire
// data and carries no caller-selected scope or actor authority.
type WorkflowContinuationObservation struct {
	Execution           WorkflowExecution
	FirstExecutionRunID string
	PreviousRunID       string
	WorkflowType        string
	TaskQueue           string
	ExecutionTimeout    time.Duration
}

func (observation WorkflowContinuationObservation) Validate() error {
	if err := observation.Execution.Validate(); err != nil {
		return err
	}
	if !validIdentityPart(observation.FirstExecutionRunID) ||
		!validIdentityPart(observation.PreviousRunID) {
		return contractError(ErrInvalidIdentity, "workflow_continuation_observation", "verified chain identity is incomplete")
	}
	if observation.Execution.RunID == observation.FirstExecutionRunID ||
		observation.Execution.RunID == observation.PreviousRunID {
		return contractError(ErrInvalidIdentity, "workflow_continuation_observation", "execution is not a new continuation member")
	}
	if observation.WorkflowType != RunWorkflowType || observation.TaskQueue != ControlTaskQueue {
		return contractError(ErrInvalidContract, "workflow_continuation_observation", "continued coordinator protocol is invalid")
	}
	if observation.ExecutionTimeout <= 0 {
		return contractError(ErrInvalidContract, "workflow_continuation_observation.execution_timeout", "must be finite and positive")
	}
	return nil
}

type WorkflowContinuationObserver func(
	context.Context,
	WorkflowExecution,
) (WorkflowContinuationObservation, error)

type ScheduledRunInput struct {
	Scope      Scope  `json:"scope"`
	ScheduleID string `json:"schedule_id"`
	Revision   uint64 `json:"revision"`
}

func (input *ScheduledRunInput) UnmarshalJSON(data []byte) error {
	type wire ScheduledRunInput
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "scope", "schedule_id", "revision"); err != nil {
		return err
	}
	value := ScheduledRunInput(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*input = value
	return nil
}

func (input ScheduledRunInput) Validate() error {
	if err := input.Scope.Validate(); err != nil {
		return err
	}
	if !validIdentityPart(input.ScheduleID) || input.Revision == 0 {
		return contractError(ErrInvalidIdentity, "scheduled_run_input", "schedule identity and positive revision are required")
	}
	return nil
}
