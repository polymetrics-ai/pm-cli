package workflows

import (
	"errors"

	"go.temporal.io/sdk/workflow"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func (coordinator *Coordinator) PMScheduledRunWorkflow(
	ctx workflow.Context,
	input contract.ScheduledRunInput,
) (contract.WorkflowProjectionResult, error) {
	if coordinator == nil {
		return contract.WorkflowProjectionResult{}, errors.New("workflow coordinator is required")
	}
	if err := input.Validate(); err != nil {
		return contract.WorkflowProjectionResult{}, err
	}
	if ctx == nil {
		return contract.WorkflowProjectionResult{}, errors.New("workflow context is required")
	}
	info := workflow.GetInfo(ctx)
	if info == nil || info.WorkflowType.Name != contract.ScheduledWorkflowType ||
		info.WorkflowExecution.ID == "" || info.WorkflowExecution.RunID == "" || info.Namespace == "" {
		return contract.WorkflowProjectionResult{}, errors.New("actual Temporal scheduled execution identity is invalid")
	}
	activityCtx, err := activityContext(ctx, coordinator.bootstrapTimeouts)
	if err != nil {
		return contract.WorkflowProjectionResult{}, err
	}
	var admitted contract.StartRunInput
	if err := workflow.ExecuteActivity(activityCtx, contract.ScheduleActivityName, input).Get(activityCtx, &admitted); err != nil {
		return contract.WorkflowProjectionResult{}, err
	}
	if err := admitted.ValidateScheduledStart(); err != nil {
		return contract.WorkflowProjectionResult{}, err
	}
	if !admitted.Scope.Equal(input.Scope) || admitted.WorkflowID != info.WorkflowExecution.ID {
		return contract.WorkflowProjectionResult{}, errors.New("scheduled admission returned a mismatched run binding")
	}
	return coordinator.CoordinateRun(ctx, admitted)
}
