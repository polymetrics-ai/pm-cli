package temporal

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

const (
	ControlTaskQueue      = "pm-control"
	RunWorkflowType       = "pm.run.v1"
	ProjectionUpdateName  = "pm.retry-final-projection.v1"
	StartRunInputSchema   = "pm.workflow-start/v1"
	ProjectionRetrySchema = "pm.projection-retry/v1"
)

// StartRunInput is the complete safe workflow argument. The sealed intent
// digest binds namespace, queue, workflow type, timeout and immutable budget;
// raw inputs, actors, credentials and provider payloads never enter history.
type StartRunInput struct {
	Schema       string                  `json:"schema"`
	Scope        contract.Scope          `json:"scope"`
	RunID        string                  `json:"run_id"`
	CommandID    string                  `json:"command_id"`
	WorkflowID   string                  `json:"workflow_id"`
	Generation   uint64                  `json:"generation"`
	Origin       contract.RunOrigin      `json:"origin"`
	IntentDigest contract.SemanticDigest `json:"intent_digest"`
}

func (input StartRunInput) Validate() error {
	if input.Schema != StartRunInputSchema {
		return errors.New("workflow start input schema is invalid")
	}
	if err := input.Scope.Validate(); err != nil {
		return err
	}
	if !validTemporalIdentity(input.RunID) || !validTemporalIdentity(input.CommandID) ||
		!validTemporalIdentity(input.WorkflowID) || input.Generation == 0 {
		return errors.New("workflow start input identity is invalid")
	}
	switch input.Origin {
	case contract.RunOriginAPI, contract.RunOriginRecovery:
	default:
		return errors.New("workflow start input origin is invalid")
	}
	if input.Origin == contract.RunOriginAPI && input.WorkflowID != "pm/run/"+input.RunID {
		return errors.New("API workflow start identity is invalid")
	}
	if err := input.IntentDigest.Validate(); err != nil {
		return fmt.Errorf("validate workflow start intent digest: %w", err)
	}
	return nil
}

// ProjectionRetryCommand asks a running workflow to retry only its owned final
// PostgreSQL projection. It does not assert a workflow or provider outcome.
type ProjectionRetryCommand struct {
	Schema       string                  `json:"schema"`
	Scope        contract.Scope          `json:"scope"`
	RunID        string                  `json:"run_id"`
	WorkflowID   string                  `json:"workflow_id"`
	Generation   uint64                  `json:"generation"`
	IntentDigest contract.SemanticDigest `json:"intent_digest"`
}

func (command ProjectionRetryCommand) Validate() error {
	if command.Schema != ProjectionRetrySchema {
		return errors.New("projection retry schema is invalid")
	}
	if err := command.Scope.Validate(); err != nil {
		return err
	}
	if !validTemporalIdentity(command.RunID) || !validTemporalIdentity(command.WorkflowID) || command.Generation == 0 {
		return errors.New("projection retry identity is invalid")
	}
	if err := command.IntentDigest.Validate(); err != nil {
		return err
	}
	return nil
}

func workflowCommandPlan(handoff store.WorkflowHandoff, configuration ClientConfig, kind string) (store.WorkflowCommandPlan, error) {
	binding := handoff.Record.WorkflowBinding
	plan := store.WorkflowCommandPlan{
		Namespace:        configuration.Namespace,
		TaskQueue:        configuration.ControlTaskQueue,
		WorkflowType:     configuration.WorkflowType,
		WorkflowID:       binding.WorkflowID,
		Origin:           binding.Origin,
		Generation:       binding.Generation,
		ExecutionTimeout: handoff.Budget.Policy.RunLifetime,
		BudgetDigest:     handoff.Budget.Digest,
	}
	if temporalDuration(plan.ExecutionTimeout) == 0 {
		return store.WorkflowCommandPlan{}, errors.New("run lifetime exceeds the Temporal duration range")
	}
	if kind == store.WorkflowCommandStartRun && binding.Origin == contract.RunOriginSchedule {
		return store.WorkflowCommandPlan{}, errors.New("scheduled workflow cannot use start command")
	}
	return plan, nil
}

func startRunInput(claim store.OutboxClaim, intent store.WorkflowCommandIntent) (StartRunInput, error) {
	input := StartRunInput{
		Schema:       StartRunInputSchema,
		Scope:        claim.Scope,
		RunID:        claim.RunID,
		CommandID:    claim.Command.CommandID,
		WorkflowID:   intent.Plan.WorkflowID,
		Generation:   intent.Plan.Generation,
		Origin:       intent.Plan.Origin,
		IntentDigest: intent.Digest,
	}
	if err := input.Validate(); err != nil {
		return StartRunInput{}, err
	}
	return input, nil
}

func projectionRetryCommand(intent store.WorkflowCommandIntent) (ProjectionRetryCommand, error) {
	command := ProjectionRetryCommand{
		Schema:       ProjectionRetrySchema,
		Scope:        intent.Scope,
		RunID:        intent.RunID,
		WorkflowID:   intent.Plan.WorkflowID,
		Generation:   intent.Plan.Generation,
		IntentDigest: intent.Digest,
	}
	if err := command.Validate(); err != nil {
		return ProjectionRetryCommand{}, err
	}
	return command, nil
}

func projectionUpdateID(intent store.WorkflowCommandIntent) (string, error) {
	encoded, err := json.Marshal(struct {
		Schema     string                  `json:"schema"`
		Scope      contract.Scope          `json:"scope"`
		RunID      string                  `json:"run_id"`
		CommandID  string                  `json:"command_id"`
		Generation uint64                  `json:"generation"`
		Digest     contract.SemanticDigest `json:"digest"`
	}{
		Schema: ProjectionRetrySchema, Scope: intent.Scope, RunID: intent.RunID,
		CommandID: intent.CommandID, Generation: intent.Plan.Generation, Digest: intent.Digest,
	})
	if err != nil {
		return "", fmt.Errorf("encode projection update identity: %w", err)
	}
	digest, err := contract.CanonicalDigest(encoded)
	if err != nil {
		return "", fmt.Errorf("digest projection update identity: %w", err)
	}
	return "pm/projection/" + strings.TrimPrefix(string(digest), "sha256:"), nil
}

func validTemporalIdentity(value string) bool {
	if value == "" || len(value) > 512 || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func temporalDuration(value time.Duration) time.Duration {
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
