package config

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// WorkerConfiguration is absent for API-only configurations. pm-worker refuses
// startup unless this complete finite block is present.
type WorkerConfiguration struct {
	TaskQueue                  string
	DialTimeout                time.Duration
	DrainTimeout               time.Duration
	MaxConcurrentActivities    uint64
	MaxConcurrentWorkflowTasks uint64
	ActivityTimeouts           contract.ActivityTimeouts
	configured                 bool
}

func (configuration WorkerConfiguration) Configured() bool {
	return configuration.configured
}

var workerFields = []string{
	"task_queue",
	"dial_timeout",
	"drain_timeout",
	"max_concurrent_activities",
	"max_concurrent_workflow_tasks",
	"activity_timeouts",
}

var workerActivityTimeoutFields = []string{
	"schedule_to_close",
	"start_to_close",
	"heartbeat",
	"transport",
}

func decodeWorker(raw json.RawMessage) (WorkerConfiguration, error) {
	if len(raw) == 0 {
		return WorkerConfiguration{}, nil
	}
	object, err := decodeObject(raw, "worker", workerFields, workerFields)
	if err != nil {
		return WorkerConfiguration{}, err
	}
	activityObject, err := decodeObject(
		object["activity_timeouts"],
		"worker.activity_timeouts",
		workerActivityTimeoutFields,
		workerActivityTimeoutFields,
	)
	if err != nil {
		return WorkerConfiguration{}, err
	}
	configuration := WorkerConfiguration{configured: true}
	if configuration.TaskQueue, err = decodeString(object, "task_queue", "worker.task_queue"); err != nil {
		return WorkerConfiguration{}, err
	}
	for _, field := range []struct {
		object map[string]json.RawMessage
		name   string
		path   string
		value  *time.Duration
	}{
		{object, "dial_timeout", "worker.dial_timeout", &configuration.DialTimeout},
		{object, "drain_timeout", "worker.drain_timeout", &configuration.DrainTimeout},
		{activityObject, "schedule_to_close", "worker.activity_timeouts.schedule_to_close", &configuration.ActivityTimeouts.ScheduleToClose},
		{activityObject, "start_to_close", "worker.activity_timeouts.start_to_close", &configuration.ActivityTimeouts.StartToClose},
		{activityObject, "heartbeat", "worker.activity_timeouts.heartbeat", &configuration.ActivityTimeouts.Heartbeat},
		{activityObject, "transport", "worker.activity_timeouts.transport", &configuration.ActivityTimeouts.Transport},
	} {
		if *field.value, err = decodeDuration(field.object, field.name, field.path); err != nil {
			return WorkerConfiguration{}, err
		}
	}
	if configuration.MaxConcurrentActivities, err = decodeUint64(
		object,
		"max_concurrent_activities",
		"worker.max_concurrent_activities",
	); err != nil {
		return WorkerConfiguration{}, err
	}
	if configuration.MaxConcurrentWorkflowTasks, err = decodeUint64(
		object,
		"max_concurrent_workflow_tasks",
		"worker.max_concurrent_workflow_tasks",
	); err != nil {
		return WorkerConfiguration{}, err
	}
	return configuration, nil
}

func validateWorker(configuration WorkerConfiguration, temporal TemporalConfiguration, limits ResourceLimits) error {
	if !configuration.configured {
		return nil
	}
	if configuration.TaskQueue != contract.ControlTaskQueue || !slices.Contains(temporal.TaskQueues, configuration.TaskQueue) {
		return diagnostic("worker.task_queue", DiagnosticValue)
	}
	if configuration.DialTimeout <= 0 || configuration.DialTimeout > 5*time.Minute {
		return diagnostic("worker.dial_timeout", DiagnosticBounds)
	}
	if configuration.DrainTimeout <= 0 || configuration.DrainTimeout > 5*time.Minute {
		return diagnostic("worker.drain_timeout", DiagnosticBounds)
	}
	if configuration.MaxConcurrentActivities == 0 || configuration.MaxConcurrentActivities > 1<<16 {
		return diagnostic("worker.max_concurrent_activities", DiagnosticBounds)
	}
	if configuration.MaxConcurrentWorkflowTasks == 0 || configuration.MaxConcurrentWorkflowTasks > 1<<16 {
		return diagnostic("worker.max_concurrent_workflow_tasks", DiagnosticBounds)
	}
	timeouts := configuration.ActivityTimeouts
	if err := timeouts.Validate(); err != nil {
		field := "worker.activity_timeouts"
		switch {
		case timeouts.ScheduleToClose <= 0:
			field = "worker.activity_timeouts.schedule_to_close"
		case timeouts.StartToClose <= 0 || timeouts.ScheduleToClose <= timeouts.StartToClose:
			field = "worker.activity_timeouts.start_to_close"
		case timeouts.Heartbeat <= 0 || timeouts.Heartbeat >= timeouts.StartToClose:
			field = "worker.activity_timeouts.heartbeat"
		case timeouts.Transport <= 0 || timeouts.Transport >= timeouts.StartToClose:
			field = "worker.activity_timeouts.transport"
		}
		return diagnostic(field, DiagnosticBounds)
	}
	if timeouts.ScheduleToClose > limits.RunLifetime {
		return diagnostic("worker.activity_timeouts.schedule_to_close", DiagnosticBounds)
	}
	return nil
}
