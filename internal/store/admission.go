package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	ErrWorkspaceAdmissionCountLimit = errors.New("workspace pending-admission count is full")
	ErrWorkspaceAdmissionAgeLimit   = errors.New("workspace oldest pending admission reached its age limit")
	ErrDeploymentAdmissionOverload  = errors.New("deployment pending-admission count is full")
)

// AdmissionLimits are resolved once by the server configuration owner. Storage
// has no default, environment reader, or process-local capacity authority.
type AdmissionLimits struct {
	GlobalPending          uint64        `json:"global_pending"`
	WorkspacePending       uint64        `json:"workspace_pending"`
	WorkspaceMaxPendingAge time.Duration `json:"workspace_max_pending_age"`
}

func (limits AdmissionLimits) Validate() error {
	if limits.GlobalPending == 0 || limits.WorkspacePending == 0 ||
		limits.WorkspacePending > limits.GlobalPending || limits.WorkspaceMaxPendingAge <= 0 {
		return errors.New("pending-admission bounds must be positive with workspace count no greater than deployment count")
	}
	return nil
}

func lockPendingAdmissions(ctx context.Context, tx *Transaction) (uint64, error) {
	var pending int64
	if err := tx.queryRow(ctx, `
		SELECT pending_count
		FROM pm.pending_admission_state
		WHERE singleton
		FOR UPDATE
	`).Scan(&pending); err != nil {
		return 0, fmt.Errorf("lock pending-admission authority: %w", err)
	}
	return uint64(pending), nil
}

func checkPendingAdmission(ctx context.Context, tx *Transaction, limits AdmissionLimits) (time.Time, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return time.Time{}, err
	}
	globalPending, err := lockPendingAdmissions(ctx, tx)
	if err != nil {
		return time.Time{}, err
	}
	var now time.Time
	var workspacePending int64
	var oldest *time.Time
	if err := tx.queryRow(ctx, `
		SELECT clock_timestamp(), count(*), min(accepted_at)
		FROM pm.pending_reservations
		WHERE org_id = $1 AND workspace_id = $2 AND released_at IS NULL
	`, scope.OrgID, scope.WorkspaceID).Scan(&now, &workspacePending, &oldest); err != nil {
		return time.Time{}, fmt.Errorf("read scoped pending admissions: %w", err)
	}
	if err := pendingAdmissionRefusal(limits, globalPending, uint64(workspacePending), oldest, now); err != nil {
		return time.Time{}, err
	}
	return now.UTC(), nil
}

func pendingAdmissionRefusal(limits AdmissionLimits, globalPending, workspacePending uint64, oldest *time.Time, now time.Time) error {
	if workspacePending >= limits.WorkspacePending {
		return ErrWorkspaceAdmissionCountLimit
	}
	if oldest != nil && now.Sub(*oldest) >= limits.WorkspaceMaxPendingAge {
		return ErrWorkspaceAdmissionAgeLimit
	}
	if globalPending >= limits.GlobalPending {
		return ErrDeploymentAdmissionOverload
	}
	return nil
}
