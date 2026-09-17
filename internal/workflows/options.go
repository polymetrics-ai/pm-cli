package workflows

import (
	"errors"

	"go.temporal.io/sdk/workflow"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func activityContext(ctx workflow.Context, timeouts contract.ActivityTimeouts) (workflow.Context, error) {
	return activityContextWithHeartbeat(ctx, timeouts, false)
}

func executionActivityContext(ctx workflow.Context, timeouts contract.ActivityTimeouts) (workflow.Context, error) {
	return activityContextWithHeartbeat(ctx, timeouts, true)
}

func activityContextWithHeartbeat(
	ctx workflow.Context,
	timeouts contract.ActivityTimeouts,
	heartbeat bool,
) (workflow.Context, error) {
	if err := timeouts.Validate(); err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, errors.New("workflow context is required")
	}
	options := workflow.ActivityOptions{
		ScheduleToCloseTimeout: timeouts.ScheduleToClose,
		StartToCloseTimeout:    timeouts.StartToClose,
		WaitForCancellation:    true,
	}
	if heartbeat {
		options.HeartbeatTimeout = timeouts.Heartbeat
	}
	return workflow.WithActivityOptions(ctx, options), nil
}
