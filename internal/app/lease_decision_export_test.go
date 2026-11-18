package app

import (
	"context"
	"time"
)

// SetBeforeLeaseDecision sets the hook that ExecuteUnit runs in its prepare
// transaction after the lease decision samples the database time and before it
// reads the stored lease. The hook gets that sample. Only the Go developer
// checks call it. Production sets no hook.
func SetBeforeLeaseDecision(service *WorkerService, hook func(context.Context, time.Time) error) {
	service.beforeLeaseDecision = hook
}
