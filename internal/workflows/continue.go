package workflows

import (
	"errors"

	"go.temporal.io/sdk/workflow"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const (
	continueHistoryEventLimit = 2000
	continueHistoryByteLimit  = 2 << 20
)

func shouldContinue(ctx workflow.Context) bool {
	if ctx == nil {
		return false
	}
	info := workflow.GetInfo(ctx)
	return info.GetContinueAsNewSuggested() ||
		info.GetCurrentHistoryLength() >= continueHistoryEventLimit ||
		info.GetCurrentHistorySize() >= continueHistoryByteLimit
}

// ContinueSafely waits for accepted update handlers to finish before rolling
// the chain. The immutable start reference is the only carried application
// state; the next execution reloads deadline, commands, and progress from
// PostgreSQL.
func ContinueSafely(ctx workflow.Context, input contract.StartRunInput) error {
	if ctx == nil {
		return errors.New("workflow context is required")
	}
	if err := input.Validate(); err != nil {
		return err
	}
	if !shouldContinue(ctx) {
		return nil
	}
	if err := workflow.Await(ctx, func() bool { return workflow.AllHandlersFinished(ctx) }); err != nil {
		return err
	}
	return workflow.NewContinueAsNewError(ctx, contract.RunWorkflowType, input)
}
