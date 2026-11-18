package activities

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// TestClaimLostRetryDelayIsBoundedByScheduleToClose checks the mapping of an
// ExecuteUnit error whose task lease another attempt holds. The retry waits for
// the remaining lease time only when the lease ends before the activity's
// ScheduleToClose end. Every other case keeps the default retry. It needs no
// stack: the times are fixed values.
func TestClaimLostRetryDelayIsBoundedByScheduleToClose(t *testing.T) {
	scheduled := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	info := activity.Info{ScheduledTime: scheduled, ScheduleToCloseTimeout: 20 * time.Second}
	held := &app.WorkerLeaseHeldError{RetryAfter: 6 * time.Second}

	for _, testCase := range []struct {
		name      string
		info      activity.Info
		now       time.Time
		err       error
		wantDelay time.Duration
	}{
		{"lease ends before ScheduleToClose", info, scheduled.Add(2 * time.Second), held, 6 * time.Second},
		{"lease held behind a joined transaction error", info, scheduled.Add(2 * time.Second),
			errors.Join(held, errors.New("rollback")), 6 * time.Second},
		{"lease ends at the ScheduleToClose end", info, scheduled.Add(14 * time.Second), held, 0},
		{"lease ends after ScheduleToClose", info, scheduled.Add(15 * time.Second), held, 0},
		{"no scheduled time", activity.Info{ScheduleToCloseTimeout: 20 * time.Second}, scheduled, held, 0},
		{"planning claim lost without a lease time", info, scheduled, store.ErrClaimLost, 0},
		{"other transient error", info, scheduled, fmt.Errorf("store unavailable"), 0},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var applicationError *temporal.ApplicationError
			if !errors.As(temporalUnitError(testCase.info, testCase.now, testCase.err), &applicationError) {
				t.Fatal("unit error is not a Temporal application error")
			}
			if applicationError.Type() != failureTransient || applicationError.NonRetryable() ||
				applicationError.NextRetryDelay() != testCase.wantDelay {
				t.Fatalf("unit error = type %q non-retryable %t delay %s; want %q retryable with delay %s",
					applicationError.Type(), applicationError.NonRetryable(), applicationError.NextRetryDelay(),
					failureTransient, testCase.wantDelay)
			}
		})
	}

	conflict := temporalUnitError(info, scheduled, fmt.Errorf("wrapped: %w", app.ErrWorkerConflict))
	var applicationError *temporal.ApplicationError
	if !errors.As(conflict, &applicationError) || !applicationError.NonRetryable() ||
		applicationError.NextRetryDelay() != 0 {
		t.Fatalf("conflict error = %v; want the unchanged non-retryable conflict", conflict)
	}
}
