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

func startRunInput(claim store.OutboxClaim, intent store.WorkflowCommandIntent) (contract.StartRunInput, error) {
	input := contract.StartRunInput{
		Schema:       contract.StartRunInputSchema,
		Scope:        claim.Scope,
		RunID:        claim.RunID,
		CommandID:    claim.Command.CommandID,
		WorkflowID:   intent.Plan.WorkflowID,
		Generation:   intent.Plan.Generation,
		Origin:       intent.Plan.Origin,
		IntentDigest: intent.Digest,
	}
	if err := input.ValidateRelayStart(); err != nil {
		return contract.StartRunInput{}, err
	}
	return input, nil
}

func projectionRetryCommand(intent store.WorkflowCommandIntent) (contract.ProjectionRetryCommand, error) {
	command := contract.ProjectionRetryCommand{
		Schema:       contract.ProjectionRetrySchema,
		Scope:        intent.Scope,
		RunID:        intent.RunID,
		WorkflowID:   intent.Plan.WorkflowID,
		Generation:   intent.Plan.Generation,
		IntentDigest: intent.Digest,
	}
	if err := command.Validate(); err != nil {
		return contract.ProjectionRetryCommand{}, err
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
		Schema: contract.ProjectionRetrySchema, Scope: intent.Scope, RunID: intent.RunID,
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

// temporalDuration is the one Temporal-boundary quantization, shared with the
// store's continuation observation through contract.QuantizedTemporalDuration.
func temporalDuration(value time.Duration) time.Duration {
	return contract.QuantizedTemporalDuration(value)
}
