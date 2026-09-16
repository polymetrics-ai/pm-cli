package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type ClaimRequest struct {
	Task          TaskRef
	OwnerID       string
	ExpectedFence uint64
	TTL           time.Duration
}

type Lease struct {
	Scope     contract.Scope
	Task      TaskRef
	OwnerID   string
	Fence     uint64
	ClaimedAt time.Time
	ExpiresAt time.Time
}

func (lease Lease) validate() error {
	if err := lease.Scope.Validate(); err != nil {
		return err
	}
	if err := lease.Task.validate(); err != nil {
		return err
	}
	if !validStoreID(lease.OwnerID) || lease.Fence == 0 || lease.ClaimedAt.IsZero() || lease.ExpiresAt.IsZero() || !lease.ExpiresAt.After(lease.ClaimedAt) {
		return errors.New("task lease is invalid")
	}
	return nil
}

func ClaimTask(ctx context.Context, tx *Transaction, request ClaimRequest) (Lease, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return Lease{}, err
	}
	if err := request.Task.validate(); err != nil {
		return Lease{}, err
	}
	if !validStoreID(request.OwnerID) {
		return Lease{}, errors.New("lease owner identity is invalid")
	}
	microseconds := request.TTL.Microseconds()
	if microseconds <= 0 {
		return Lease{}, errors.New("lease TTL must be at least one microsecond")
	}
	lease := Lease{Scope: scope, Task: request.Task}
	err = tx.queryRow(ctx, `
		WITH lease_clock AS (
			SELECT clock_timestamp() AS claimed_at
		),
		updated AS (
			UPDATE pm.task_leases AS lease
			SET owner_id = $5,
			    fence = lease.fence + 1,
			    claimed_at = lease_clock.claimed_at,
			    lease_expires_at = lease_clock.claimed_at + make_interval(secs => $7::double precision / 1000000.0)
			FROM lease_clock
			WHERE lease.org_id = $1 AND lease.workspace_id = $2
			  AND lease.run_id = $3 AND lease.task_id = $4
			  AND lease.fence = $6
			  AND lease.lease_expires_at <= lease_clock.claimed_at
			RETURNING lease.owner_id, lease.fence, lease.claimed_at, lease.lease_expires_at
		),
		inserted AS (
			INSERT INTO pm.task_leases (
				org_id, workspace_id, run_id, task_id, owner_id, fence, claimed_at, lease_expires_at
			)
			SELECT $1, $2, $3, $4, $5, 1, claimed_at,
			       claimed_at + make_interval(secs => $7::double precision / 1000000.0)
			FROM lease_clock
			WHERE $6 = 0 AND NOT EXISTS (SELECT 1 FROM updated)
			ON CONFLICT (org_id, workspace_id, run_id, task_id) DO NOTHING
			RETURNING owner_id, fence, claimed_at, lease_expires_at
		)
		SELECT owner_id, fence, claimed_at, lease_expires_at FROM updated
		UNION ALL
		SELECT owner_id, fence, claimed_at, lease_expires_at FROM inserted
	`,
		scope.OrgID,
		scope.WorkspaceID,
		request.Task.RunID,
		request.Task.TaskID,
		request.OwnerID,
		request.ExpectedFence,
		microseconds,
	).Scan(&lease.OwnerID, &lease.Fence, &lease.ClaimedAt, &lease.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, ErrClaimLost
	}
	if err != nil {
		return Lease{}, fmt.Errorf("claim task: %w", err)
	}
	lease.ClaimedAt = lease.ClaimedAt.UTC()
	lease.ExpiresAt = lease.ExpiresAt.UTC()
	return lease, nil
}

func RenewLease(ctx context.Context, tx *Transaction, lease Lease, ttl time.Duration) (Lease, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return Lease{}, err
	}
	if err := lease.validate(); err != nil {
		return Lease{}, err
	}
	if !scope.Equal(lease.Scope) {
		return Lease{}, ErrScopeMismatch
	}
	microseconds := ttl.Microseconds()
	if microseconds <= 0 {
		return Lease{}, errors.New("lease TTL must be at least one microsecond")
	}
	var renewed Lease
	renewed.Scope = lease.Scope
	renewed.Task = lease.Task
	err = tx.queryRow(ctx, `
		UPDATE pm.task_leases
		SET lease_expires_at = clock_timestamp() + make_interval(secs => $7::double precision / 1000000.0)
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND task_id = $4
		  AND owner_id = $5 AND fence = $6 AND lease_expires_at > clock_timestamp()
		RETURNING owner_id, fence, claimed_at, lease_expires_at
	`,
		lease.Scope.OrgID, lease.Scope.WorkspaceID, lease.Task.RunID, lease.Task.TaskID,
		lease.OwnerID, lease.Fence, microseconds,
	).Scan(&renewed.OwnerID, &renewed.Fence, &renewed.ClaimedAt, &renewed.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, ErrStaleFence
	}
	if err != nil {
		return Lease{}, fmt.Errorf("renew task lease: %w", err)
	}
	renewed.ClaimedAt = renewed.ClaimedAt.UTC()
	renewed.ExpiresAt = renewed.ExpiresAt.UTC()
	return renewed, nil
}

func LoadLease(ctx context.Context, tx *Transaction, ref TaskRef) (Lease, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return Lease{}, err
	}
	if err := ref.validate(); err != nil {
		return Lease{}, err
	}
	lease := Lease{Scope: scope, Task: ref}
	err = tx.queryRow(ctx, `
		SELECT owner_id, fence, claimed_at, lease_expires_at
		FROM pm.task_leases
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND task_id = $4
	`, scope.OrgID, scope.WorkspaceID, ref.RunID, ref.TaskID).Scan(
		&lease.OwnerID, &lease.Fence, &lease.ClaimedAt, &lease.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Lease{}, ErrNotFound
	}
	if err != nil {
		return Lease{}, fmt.Errorf("load task lease: %w", err)
	}
	lease.ClaimedAt = lease.ClaimedAt.UTC()
	lease.ExpiresAt = lease.ExpiresAt.UTC()
	return lease, nil
}

func requireCurrentLease(ctx context.Context, tx *Transaction, lease Lease) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	if err := lease.validate(); err != nil {
		return err
	}
	if !scope.Equal(lease.Scope) {
		return ErrScopeMismatch
	}
	var exists bool
	err = tx.queryRow(ctx, `
		SELECT true
		FROM pm.task_leases
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND task_id = $4
		  AND owner_id = $5 AND fence = $6 AND lease_expires_at > clock_timestamp()
		FOR UPDATE
	`,
		lease.Scope.OrgID, lease.Scope.WorkspaceID, lease.Task.RunID, lease.Task.TaskID,
		lease.OwnerID, lease.Fence,
	).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrStaleFence
	}
	if err != nil {
		return fmt.Errorf("verify task lease fence: %w", err)
	}
	return nil
}
