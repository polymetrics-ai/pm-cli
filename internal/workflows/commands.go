package workflows

import (
	"errors"
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type coordinatorState struct {
	ready            bool
	run              contract.WorkflowRunRef
	timeouts         contract.ActivityTimeouts
	originalDeadline time.Time
	cancelRequested  bool
	controlSignals   workflow.ReceiveChannel
}

func (coordinator *Coordinator) installHandlers(
	ctx workflow.Context,
	input contract.StartRunInput,
	state *coordinatorState,
) error {
	if state == nil {
		return errors.New("workflow coordinator state is required")
	}
	state.controlSignals = workflow.GetSignalChannel(ctx, contract.ControlSignalName)
	if err := workflow.SetUpdateHandlerWithOptions(
		ctx,
		contract.ControlUpdateName,
		func(handlerContext workflow.Context, command contract.WorkflowCommandRef) (contract.WorkflowCommandResult, error) {
			if err := workflow.Await(handlerContext, func() bool { return state.ready }); err != nil {
				return contract.WorkflowCommandResult{}, err
			}
			result, err := executeCommand(handlerContext, state.run, command, state.timeouts)
			if err == nil && result.CancelRequested {
				state.cancelRequested = true
			}
			return result, err
		},
		workflow.UpdateHandlerOptions{
			Validator: func(command contract.WorkflowCommandRef) error {
				return command.Validate()
			},
		},
	); err != nil {
		return err
	}
	return workflow.SetUpdateHandlerWithOptions(
		ctx,
		contract.ProjectionUpdateName,
		func(handlerContext workflow.Context, command contract.ProjectionRetryCommand) (contract.WorkflowProjectionResult, error) {
			if err := workflow.Await(handlerContext, func() bool { return state.ready }); err != nil {
				return contract.WorkflowProjectionResult{}, err
			}
			if err := validateProjectionCommand(input, command); err != nil {
				return contract.WorkflowProjectionResult{}, err
			}
			return executeProjection(handlerContext, contract.WorkflowProjectionRequest{
				Run:                state.run,
				RepairIntentDigest: command.IntentDigest,
			}, state.timeouts)
		},
		workflow.UpdateHandlerOptions{
			Validator: func(command contract.ProjectionRetryCommand) error {
				return validateProjectionCommand(input, command)
			},
		},
	)
}

func validateProjectionCommand(input contract.StartRunInput, command contract.ProjectionRetryCommand) error {
	if err := command.Validate(); err != nil {
		return err
	}
	if !command.Scope.Equal(input.Scope) || command.RunID != input.RunID ||
		command.WorkflowID != input.WorkflowID || command.Generation != input.Generation {
		return errors.New("projection repair identity does not match the running workflow")
	}
	if command.IntentDigest == input.IntentDigest {
		return errors.New("projection repair digest must differ from the workflow start digest")
	}
	return nil
}

func executeCommand(
	ctx workflow.Context,
	run contract.WorkflowRunRef,
	command contract.WorkflowCommandRef,
	timeouts contract.ActivityTimeouts,
) (contract.WorkflowCommandResult, error) {
	request := contract.WorkflowCommandRequest{Run: run, Command: command}
	if err := request.Validate(); err != nil {
		return contract.WorkflowCommandResult{}, err
	}
	activityCtx, err := activityContext(ctx, timeouts)
	if err != nil {
		return contract.WorkflowCommandResult{}, err
	}
	var result contract.WorkflowCommandResult
	if err := workflow.ExecuteActivity(activityCtx, contract.CommandActivityName, request).Get(activityCtx, &result); err != nil {
		return contract.WorkflowCommandResult{}, err
	}
	if err := result.Validate(); err != nil {
		return contract.WorkflowCommandResult{}, err
	}
	if result.CommandID != command.CommandID {
		return contract.WorkflowCommandResult{}, errors.New("command activity returned a mismatched command identity")
	}
	return result, nil
}

func executeProjection(
	ctx workflow.Context,
	request contract.WorkflowProjectionRequest,
	timeouts contract.ActivityTimeouts,
) (contract.WorkflowProjectionResult, error) {
	if err := request.Validate(); err != nil {
		return contract.WorkflowProjectionResult{}, err
	}
	activityCtx, err := activityContext(ctx, timeouts)
	if err != nil {
		return contract.WorkflowProjectionResult{}, err
	}
	var result contract.WorkflowProjectionResult
	if err := workflow.ExecuteActivity(activityCtx, contract.ProjectActivityName, request).Get(activityCtx, &result); err != nil {
		return contract.WorkflowProjectionResult{}, err
	}
	if err := result.Validate(); err != nil {
		return contract.WorkflowProjectionResult{}, err
	}
	if result.RunID != request.Run.RunID {
		return contract.WorkflowProjectionResult{}, errors.New("projection activity returned a mismatched run identity")
	}
	return result, nil
}

func drainControlSignals(
	ctx workflow.Context,
	state *coordinatorState,
) error {
	for {
		var command contract.WorkflowCommandRef
		if !state.controlSignals.ReceiveAsync(&command) {
			return nil
		}
		if err := command.Validate(); err != nil {
			return err
		}
		result, err := executeCommand(ctx, state.run, command, state.timeouts)
		if err != nil {
			return err
		}
		if result.CancelRequested {
			state.cancelRequested = true
		}
	}
}
