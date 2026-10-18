package activities

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"

	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const (
	failureInvalidRequest        = "pm.worker.invalid_request"
	failureRouteMismatch         = "pm.worker.route_mismatch"
	failureAuthorization         = "pm.worker.authorization"
	failureCapabilityUnavailable = "pm.worker.capability_unavailable"
	failureConflict              = "pm.worker.conflict"
	failureTransient             = "pm.worker.transient"
)

// Service is the Temporal transport adapter for the authenticated application
// worker boundary. It derives execution identity from activity metadata rather
// than accepting it from workflow payloads.
type Service struct {
	worker *app.WorkerService
}

func New(application *app.WorkerService) (*Service, error) {
	if application == nil {
		return nil, errors.New("worker application service is required")
	}
	return &Service{worker: application}, nil
}

func RegisterActivities(registry worker.Registry, service *Service) error {
	if registry == nil || service == nil || service.worker == nil {
		return errors.New("activity registry and worker service are required")
	}
	registry.RegisterActivityWithOptions(service.LoadWorkflow, activity.RegisterOptions{Name: contract.LoadActivityName})
	registry.RegisterActivityWithOptions(service.ExecuteUnit, activity.RegisterOptions{Name: contract.ExecuteActivityName})
	registry.RegisterActivityWithOptions(service.ApplyCommand, activity.RegisterOptions{Name: contract.CommandActivityName})
	registry.RegisterActivityWithOptions(service.FinalizeRunProjection, activity.RegisterOptions{Name: contract.ProjectActivityName})
	if service.worker.SupportsScheduledAdmission() {
		registry.RegisterActivityWithOptions(service.AdmitScheduledRun, activity.RegisterOptions{Name: contract.ScheduleActivityName})
	}
	return nil
}

// Every activity below reports its result as a pointer that stays nil when the
// activity fails. The SDK serializes the declared result value even when the
// function returns an error, and the closed safe-reference converter refuses a
// zero-valued contract (its identity is empty by construction), so a struct
// zero value would replace the real, correctly classified failure before the
// workflow ever observes it. An activity emits either the admitted payload of a
// completed activity or the exact error it hit, never a fabricated identity.
func (service *Service) LoadWorkflow(
	ctx context.Context,
	reference contract.WorkflowRunRef,
) (*contract.WorkflowRunSnapshot, error) {
	execution, err := currentExecution(ctx, contract.RunWorkflowType, contract.ScheduledWorkflowType)
	if err != nil {
		return nil, nonRetryableInvalidRequest()
	}
	result, err := service.worker.LoadWorkflow(ctx, execution, reference)
	if err != nil {
		return nil, temporalWorkerError(err)
	}
	return &result, nil
}

func (service *Service) ExecuteUnit(
	ctx context.Context,
	request contract.WorkflowUnitRequest,
) (*contract.WorkflowUnitResult, error) {
	execution, err := currentExecution(ctx, contract.RunWorkflowType, contract.ScheduledWorkflowType)
	if err != nil {
		return nil, nonRetryableInvalidRequest()
	}
	if err := request.Validate(); err != nil {
		return nil, nonRetryableInvalidRequest()
	}
	bootstrapProgress := contract.WorkflowProgress{
		RunID: request.Run.RunID, TaskID: request.Unit.TaskID, ProgressRef: request.Unit.TaskID,
	}
	if err := bootstrapProgress.Validate(); err != nil {
		return nil, nonRetryableInvalidRequest()
	}
	heartbeatTimeout := activity.GetInfo(ctx).HeartbeatTimeout
	if heartbeatTimeout <= 0 {
		return nil, nonRetryableInvalidRequest()
	}
	updateHeartbeat, stopHeartbeats := startActivityHeartbeats(ctx, bootstrapProgress, heartbeatTimeout)
	defer stopHeartbeats()
	result, err := service.worker.ExecuteUnit(ctx, execution, request, updateHeartbeat)
	if err != nil {
		return nil, temporalWorkerError(err)
	}
	return &result, nil
}

func (service *Service) ApplyCommand(
	ctx context.Context,
	request contract.WorkflowCommandRequest,
) (*contract.WorkflowCommandResult, error) {
	execution, err := currentExecution(ctx, contract.RunWorkflowType, contract.ScheduledWorkflowType)
	if err != nil {
		return nil, nonRetryableInvalidRequest()
	}
	result, err := service.worker.ApplyCommand(ctx, execution, request)
	if err != nil {
		return nil, temporalWorkerError(err)
	}
	return &result, nil
}

func (service *Service) FinalizeRunProjection(
	ctx context.Context,
	request contract.WorkflowProjectionRequest,
) (*contract.WorkflowProjectionResult, error) {
	execution, err := currentExecution(ctx, contract.RunWorkflowType, contract.ScheduledWorkflowType)
	if err != nil {
		return nil, nonRetryableInvalidRequest()
	}
	result, err := service.worker.FinalizeRunProjection(ctx, execution, request)
	if err != nil {
		return nil, temporalWorkerError(err)
	}
	return &result, nil
}

func (service *Service) AdmitScheduledRun(
	ctx context.Context,
	input contract.ScheduledRunInput,
) (*contract.StartRunInput, error) {
	execution, err := currentExecution(ctx, contract.ScheduledWorkflowType)
	if err != nil {
		return nil, nonRetryableInvalidRequest()
	}
	result, err := service.worker.AdmitScheduledRun(ctx, execution, input)
	if err != nil {
		return nil, temporalWorkerError(err)
	}
	return &result, nil
}

func currentExecution(ctx context.Context, expectedWorkflowTypes ...string) (contract.WorkflowExecution, error) {
	if ctx == nil || !activity.IsActivity(ctx) {
		return contract.WorkflowExecution{}, errors.New("Temporal activity context is required")
	}
	info := activity.GetInfo(ctx)
	allowedType := false
	if info.WorkflowType != nil {
		for _, expected := range expectedWorkflowTypes {
			if info.WorkflowType.Name == expected {
				allowedType = true
				break
			}
		}
	}
	if !allowedType ||
		info.Namespace == "" || info.WorkflowExecution.ID == "" || info.WorkflowExecution.RunID == "" {
		return contract.WorkflowExecution{}, errors.New("Temporal activity execution identity is invalid")
	}
	execution := contract.WorkflowExecution{
		Namespace:  info.Namespace,
		WorkflowID: info.WorkflowExecution.ID,
		RunID:      info.WorkflowExecution.RunID,
	}
	if err := execution.Validate(); err != nil {
		return contract.WorkflowExecution{}, err
	}
	return execution, nil
}

func startActivityHeartbeats(
	ctx context.Context,
	progress contract.WorkflowProgress,
	heartbeatTimeout time.Duration,
) (func(contract.WorkflowProgress), func()) {
	var progressMu sync.Mutex
	record := func(next contract.WorkflowProgress) {
		if next.Validate() != nil {
			return
		}
		progressMu.Lock()
		defer progressMu.Unlock()
		progress = next
		activity.RecordHeartbeat(ctx, progress)
	}
	record(progress)
	interval := heartbeatTimeout / 2
	if interval <= 0 {
		interval = time.Nanosecond
	}
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				progressMu.Lock()
				activity.RecordHeartbeat(ctx, progress)
				progressMu.Unlock()
			case <-ctx.Done():
				return
			case <-stop:
				return
			}
		}
	}()
	var once sync.Once
	return record, func() {
		once.Do(func() {
			close(stop)
			<-stopped
		})
	}
}

func temporalWorkerError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	switch {
	case errors.Is(err, app.ErrWorkerInvalidRequest):
		return temporal.NewNonRetryableApplicationError("worker request rejected", failureInvalidRequest, nil)
	case errors.Is(err, app.ErrWorkerRouteMismatch):
		return temporal.NewNonRetryableApplicationError("worker route rejected", failureRouteMismatch, nil)
	case errors.Is(err, app.ErrWorkerAuthorization):
		return temporal.NewNonRetryableApplicationError("worker authorization rejected", failureAuthorization, nil)
	case errors.Is(err, app.ErrWorkerCapabilityUnavailable):
		return temporal.NewNonRetryableApplicationError("worker capability unavailable", failureCapabilityUnavailable, nil)
	case errors.Is(err, app.ErrWorkerConflict):
		return temporal.NewNonRetryableApplicationError("worker state conflict", failureConflict, nil)
	default:
		return temporal.NewApplicationError("worker operation is temporarily unavailable", failureTransient)
	}
}

func nonRetryableInvalidRequest() error {
	return temporal.NewNonRetryableApplicationError("worker request rejected", failureInvalidRequest, nil)
}
