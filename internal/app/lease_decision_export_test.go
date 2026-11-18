package app

import "context"

// SetBeforeLeaseDecision sets the hook that ExecuteUnit runs in its prepare
// transaction just before the lease decision reads the stored lease. Only the
// Go developer checks call it. Production sets no hook.
func SetBeforeLeaseDecision(service *WorkerService, hook func(context.Context) error) {
	service.beforeLeaseDecision = hook
}
