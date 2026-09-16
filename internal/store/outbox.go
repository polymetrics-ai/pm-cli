package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

var ErrOutboxClaimLost = errors.New("outbox claim lost")

type OutboxClaimRequest struct {
	RunID         string
	CommandID     string
	OwnerID       string
	ExpectedFence uint64
	TTL           time.Duration
}

type OutboxClaim struct {
	Scope     contract.Scope
	RunID     string
	Command   OutboxCommand
	OwnerID   string
	Fence     uint64
	ClaimedAt time.Time
	ExpiresAt time.Time
}

func (request OutboxClaimRequest) validate() error {
	if !validStoreID(request.RunID) || !validStoreID(request.CommandID) || !validStoreID(request.OwnerID) {
		return errors.New("outbox claim request contains an invalid identity")
	}
	if request.ExpectedFence >= maxStoreFence {
		return errors.New("outbox claim fence is exhausted")
	}
	if _, err := leaseMicroseconds(request.TTL, "outbox claim"); err != nil {
		return err
	}
	return nil
}

func (claim OutboxClaim) validate() error {
	if err := claim.Scope.Validate(); err != nil {
		return err
	}
	if !validStoreID(claim.RunID) || !validStoreID(claim.OwnerID) || claim.Fence == 0 || claim.Fence > maxStoreFence {
		return errors.New("outbox claim contains an invalid identity or fence")
	}
	if err := claim.Command.validate(); err != nil {
		return err
	}
	if claim.ClaimedAt.IsZero() || claim.ExpiresAt.IsZero() || !claim.ExpiresAt.After(claim.ClaimedAt) {
		return errors.New("outbox claim contains an invalid lease window")
	}
	return nil
}

// InsertOutbox persists one immutable run-scoped command through the same seam
// used by atomic admission and settlement commits. Exact retries are idempotent;
// a changed command under the same run identity is an immutable conflict.
func InsertOutbox(ctx context.Context, tx *Transaction, runID string, command OutboxCommand) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	if !validStoreID(runID) {
		return errors.New("outbox RunID is invalid")
	}
	if err := command.validate(); err != nil {
		return err
	}
	lockMaterial := scope.OrgID + "\x00" + scope.WorkspaceID + "\x00" + runID + "\x00" + command.CommandID
	lockDigest := sha256.Sum256([]byte(lockMaterial))
	if _, err := tx.exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(binary.BigEndian.Uint64(lockDigest[:8]))); err != nil {
		return fmt.Errorf("serialize outbox command identity: %w", err)
	}
	var runExists bool
	err = tx.queryRow(ctx, `
		SELECT true
		FROM pm.runs
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
	`, scope.OrgID, scope.WorkspaceID, runID).Scan(&runExists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock outbox run: %w", err)
	}
	var stored OutboxCommand
	err = tx.queryRow(ctx, `
		SELECT command_id, kind, payload_ref
		FROM pm.outbox_commands
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND command_id = $4
	`, scope.OrgID, scope.WorkspaceID, runID, command.CommandID).Scan(
		&stored.CommandID,
		&stored.Kind,
		&stored.PayloadRef,
	)
	if err == nil {
		if stored != command {
			return ErrImmutableConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("load existing outbox command: %w", err)
	}
	createdAt, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return err
	}
	if err := insertOutboxCommand(ctx, tx, runID, command, createdAt); err != nil {
		return safeMetadataWriteError("insert outbox command", err)
	}
	return nil
}

// ClaimOutbox leases one exact immutable command. A claim permits bounded
// handoff work only; it is not evidence that an external send happened.
func ClaimOutbox(ctx context.Context, tx *Transaction, request OutboxClaimRequest) (OutboxClaim, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return OutboxClaim{}, err
	}
	if err := request.validate(); err != nil {
		return OutboxClaim{}, err
	}
	microseconds, _ := leaseMicroseconds(request.TTL, "outbox claim")
	claim := OutboxClaim{
		Scope: scope,
		RunID: request.RunID,
	}
	err = tx.queryRow(ctx, `
		WITH claim_clock AS (
			SELECT clock_timestamp() AS claimed_at
		)
		UPDATE pm.outbox_commands AS command
		SET claim_owner = $6,
		    claim_fence = command.claim_fence + 1,
		    claim_expires_at = claim_clock.claimed_at + make_interval(secs => $8::double precision / 1000000.0)
		FROM claim_clock
		WHERE command.org_id = $1 AND command.workspace_id = $2
		  AND command.run_id = $3 AND command.command_id = $4
		  AND command.state = $5 AND command.acknowledged_at IS NULL
		  AND command.available_at <= claim_clock.claimed_at
		  AND command.claim_fence = $7
		  AND (
		      ($7 = 0 AND command.claim_owner IS NULL AND command.claim_expires_at IS NULL)
		      OR
		      ($7 > 0 AND command.claim_owner IS NOT NULL AND command.claim_expires_at <= claim_clock.claimed_at)
		  )
		RETURNING command.command_id, command.kind, command.payload_ref,
		          command.claim_owner, command.claim_fence,
		          claim_clock.claimed_at, command.claim_expires_at
	`,
		scope.OrgID,
		scope.WorkspaceID,
		request.RunID,
		request.CommandID,
		"pending",
		request.OwnerID,
		request.ExpectedFence,
		microseconds,
	).Scan(
		&claim.Command.CommandID,
		&claim.Command.Kind,
		&claim.Command.PayloadRef,
		&claim.OwnerID,
		&claim.Fence,
		&claim.ClaimedAt,
		&claim.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return OutboxClaim{}, ErrOutboxClaimLost
	}
	if err != nil {
		return OutboxClaim{}, fmt.Errorf("claim outbox command: %w", err)
	}
	claim.ClaimedAt = claim.ClaimedAt.UTC()
	claim.ExpiresAt = claim.ExpiresAt.UTC()
	if err := claim.validate(); err != nil {
		return OutboxClaim{}, fmt.Errorf("validate claimed outbox command: %w", err)
	}
	return claim, nil
}

// AcknowledgeOutbox records acknowledgement only for the exact current,
// unexpired owner and fence. Expired or superseded claims cannot acknowledge.
func AcknowledgeOutbox(ctx context.Context, tx *Transaction, claim OutboxClaim) (time.Time, error) {
	if err := claim.validate(); err != nil {
		return time.Time{}, err
	}
	scope, err := tx.requireScope()
	if err != nil {
		return time.Time{}, err
	}
	if !scope.Equal(claim.Scope) {
		return time.Time{}, ErrScopeMismatch
	}
	var acknowledgedAt time.Time
	err = tx.queryRow(ctx, `
		WITH acknowledgement_clock AS (
			SELECT clock_timestamp() AS acknowledged_at
		)
		UPDATE pm.outbox_commands AS command
		SET state = $10,
		    acknowledged_at = acknowledgement_clock.acknowledged_at,
		    claim_owner = NULL,
		    claim_expires_at = NULL
		FROM acknowledgement_clock
		WHERE command.org_id = $1 AND command.workspace_id = $2
		  AND command.run_id = $3 AND command.command_id = $4
		  AND command.kind = $5 AND command.payload_ref = $6
		  AND command.state = $7 AND command.claim_owner = $8 AND command.claim_fence = $9
		  AND command.acknowledged_at IS NULL
		  AND command.claim_expires_at > acknowledgement_clock.acknowledged_at
		RETURNING command.acknowledged_at
	`,
		claim.Scope.OrgID,
		claim.Scope.WorkspaceID,
		claim.RunID,
		claim.Command.CommandID,
		claim.Command.Kind,
		claim.Command.PayloadRef,
		"pending",
		claim.OwnerID,
		claim.Fence,
		"acknowledged",
	).Scan(&acknowledgedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrOutboxClaimLost
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("acknowledge outbox command: %w", err)
	}
	return acknowledgedAt.UTC(), nil
}
