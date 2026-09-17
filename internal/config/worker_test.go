package config_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/config"
)

const workerConfiguration = `"worker":{
  "task_queue":"pm-control",
  "dial_timeout":"15s",
  "drain_timeout":"45s",
  "max_concurrent_activities":64,
  "max_concurrent_workflow_tasks":16,
  "activity_timeouts":{
    "schedule_to_close":"30m",
    "start_to_close":"5m",
    "heartbeat":"30s",
    "transport":"90s"
  }
},`

func TestTypedWorkerConfiguration(t *testing.T) {
	t.Run("API-only configuration remains valid without implicit worker defaults", func(t *testing.T) {
		configuration := loadWorkerConfiguration(t, startupConfiguration)
		if configuration.Worker().Configured() {
			t.Fatal("API-only configuration silently enabled a worker")
		}
	})

	t.Run("explicit finite controls reach worker composition", func(t *testing.T) {
		configuration := loadWorkerConfiguration(t, configuredWorkerDocument(workerConfiguration))
		worker := configuration.Worker()
		if !worker.Configured() || worker.TaskQueue != "pm-control" ||
			worker.DialTimeout != 15*time.Second || worker.DrainTimeout != 45*time.Second ||
			worker.MaxConcurrentActivities != 64 || worker.MaxConcurrentWorkflowTasks != 16 ||
			worker.ActivityTimeouts.ScheduleToClose != 30*time.Minute ||
			worker.ActivityTimeouts.StartToClose != 5*time.Minute ||
			worker.ActivityTimeouts.Heartbeat != 30*time.Second ||
			worker.ActivityTimeouts.Transport != 90*time.Second {
			t.Fatalf("worker configuration was not preserved: %#v", worker)
		}
	})

	for _, test := range []struct {
		name   string
		worker string
		field  string
		kind   config.DiagnosticKind
	}{
		{
			name:   "null block",
			worker: `"worker":null,`,
			field:  "worker",
			kind:   config.DiagnosticType,
		},
		{
			name:   "unknown retry owner",
			worker: strings.Replace(workerConfiguration, `"task_queue":"pm-control",`, `"task_queue":"pm-control","retry_timeout":"1s",`, 1),
			field:  "worker.retry_timeout",
			kind:   config.DiagnosticUnknown,
		},
		{
			name:   "noncanonical task queue",
			worker: strings.Replace(workerConfiguration, `"pm-control"`, `"pm-effect"`, 1),
			field:  "worker.task_queue",
			kind:   config.DiagnosticValue,
		},
		{
			name:   "zero dial timeout",
			worker: strings.Replace(workerConfiguration, `"dial_timeout":"15s"`, `"dial_timeout":"0s"`, 1),
			field:  "worker.dial_timeout",
			kind:   config.DiagnosticBounds,
		},
		{
			name:   "unbounded drain",
			worker: strings.Replace(workerConfiguration, `"drain_timeout":"45s"`, `"drain_timeout":"5m1s"`, 1),
			field:  "worker.drain_timeout",
			kind:   config.DiagnosticBounds,
		},
		{
			name:   "zero activity concurrency",
			worker: strings.Replace(workerConfiguration, `"max_concurrent_activities":64`, `"max_concurrent_activities":0`, 1),
			field:  "worker.max_concurrent_activities",
			kind:   config.DiagnosticBounds,
		},
		{
			name:   "workflow concurrency bound",
			worker: strings.Replace(workerConfiguration, `"max_concurrent_workflow_tasks":16`, `"max_concurrent_workflow_tasks":65537`, 1),
			field:  "worker.max_concurrent_workflow_tasks",
			kind:   config.DiagnosticBounds,
		},
		{
			name:   "activity outlives run",
			worker: strings.Replace(workerConfiguration, `"schedule_to_close":"30m"`, `"schedule_to_close":"1h1s"`, 1),
			field:  "worker.activity_timeouts.schedule_to_close",
			kind:   config.DiagnosticBounds,
		},
		{
			name:   "attempt exceeds schedule",
			worker: strings.Replace(workerConfiguration, `"start_to_close":"5m"`, `"start_to_close":"31m"`, 1),
			field:  "worker.activity_timeouts.start_to_close",
			kind:   config.DiagnosticBounds,
		},
		{
			name:   "heartbeat reaches attempt",
			worker: strings.Replace(workerConfiguration, `"heartbeat":"30s"`, `"heartbeat":"5m"`, 1),
			field:  "worker.activity_timeouts.heartbeat",
			kind:   config.DiagnosticBounds,
		},
		{
			name:   "transport reaches attempt",
			worker: strings.Replace(workerConfiguration, `"transport":"90s"`, `"transport":"5m"`, 1),
			field:  "worker.activity_timeouts.transport",
			kind:   config.DiagnosticBounds,
		},
		{
			name: "missing transport",
			worker: strings.Replace(workerConfiguration, `,
    "transport":"90s"`, ``, 1),
			field: "worker.activity_timeouts.transport",
			kind:  config.DiagnosticRequired,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			document, err := config.Load(strings.NewReader(configuredWorkerDocument(test.worker)))
			if err == nil {
				_, err = config.Validate(document, config.Prelaunch{})
			}
			var diagnostic *config.Diagnostic
			if !errors.As(err, &diagnostic) || diagnostic.Field != test.field || diagnostic.Kind != test.kind {
				t.Fatalf("worker refusal = %v; want %s/%s", err, test.field, test.kind)
			}
		})
	}
}

func configuredWorkerDocument(worker string) string {
	return strings.Replace(startupConfiguration, `"schema_version":"1"`, worker+`"schema_version":"1"`, 1)
}

func loadWorkerConfiguration(t *testing.T, raw string) config.Configuration {
	t.Helper()
	document, err := config.Load(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := config.Validate(document, config.Prelaunch{})
	if err != nil {
		t.Fatal(err)
	}
	return configuration
}
