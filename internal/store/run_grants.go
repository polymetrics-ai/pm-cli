package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

var ErrRunGrantUnavailable = errors.New("accepted execution grant is unavailable")

// RunExecutionGrant keeps immutable accepted authority separate from its
// explicit revocation. It contains no login-session identity or session expiry.
type RunExecutionGrant struct {
	RunID     string
	Grant     contract.ExecutionGrant
	RevokedAt *time.Time
}

func validateRunGrantBinding(spec contract.RunSpec, grant contract.ExecutionGrant) error {
	if err := grant.Validate(); err != nil {
		return err
	}
	if grant.GrantEpoch > maxStoreFence {
		return errors.New("execution grant epoch exceeds the durable fence range")
	}
	if grant.GrantRef != spec.ExecutionGrantRef ||
		grant.Scope.OrgID != spec.OrgID || grant.Scope.WorkspaceID != spec.WorkspaceID ||
		!grant.Actor.Equal(spec.Actor) {
		return ErrScopeMismatch
	}
	return nil
}

func insertRunExecutionGrant(ctx context.Context, tx *Transaction, runID string, grant contract.ExecutionGrant) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(grant)
	if err != nil {
		return fmt.Errorf("encode accepted execution grant: %w", err)
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.run_execution_grants (
			org_id, workspace_id, run_id, grant_ref, grant_epoch, grant_record
		) VALUES ($1, $2, $3, $4, $5, $6)
	`, scope.OrgID, scope.WorkspaceID, runID, grant.GrantRef, grant.GrantEpoch, json.RawMessage(encoded)); err != nil {
		return safeMetadataWriteError("insert accepted execution grant", err)
	}
	return nil
}

func LoadRunExecutionGrant(ctx context.Context, tx *Transaction, runID string) (RunExecutionGrant, error) {
	return loadRunExecutionGrant(ctx, tx, runID, false)
}

func loadRunExecutionGrant(ctx context.Context, tx *Transaction, runID string, forUpdate bool) (RunExecutionGrant, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return RunExecutionGrant{}, err
	}
	if !validStoreID(runID) {
		return RunExecutionGrant{}, errors.New("execution grant RunID is invalid")
	}
	const selectGrant = `
		SELECT grant_ref, grant_epoch, grant_record, revoked_at
		FROM pm.run_execution_grants
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
	`
	query := selectGrant
	if forUpdate {
		query = selectGrant + " FOR UPDATE"
	}
	result := RunExecutionGrant{RunID: runID}
	var grantRef string
	var epoch int64
	var encoded []byte
	if err := tx.queryRow(ctx, query, scope.OrgID, scope.WorkspaceID, runID).Scan(
		&grantRef, &epoch, &encoded, &result.RevokedAt,
	); errors.Is(err, pgx.ErrNoRows) {
		return RunExecutionGrant{}, ErrNotFound
	} else if err != nil {
		return RunExecutionGrant{}, fmt.Errorf("read accepted execution grant: %w", err)
	}
	if err := json.Unmarshal(encoded, &result.Grant); err != nil {
		return RunExecutionGrant{}, fmt.Errorf("decode accepted execution grant: %w", err)
	}
	if !result.Grant.Scope.Equal(scope) || result.Grant.GrantRef != grantRef || result.Grant.GrantEpoch != uint64(epoch) {
		return RunExecutionGrant{}, ErrImmutableConflict
	}
	if result.RevokedAt != nil {
		revokedAt := result.RevokedAt.UTC()
		result.RevokedAt = &revokedAt
	}
	return result, nil
}

// RevokeRunExecutionGrant is an internal transaction primitive, not an HTTP
// authorization boundary. The caller must authorize and commit its audit/event
// records in this same transaction. Revocation never rewrites the issued grant.
func RevokeRunExecutionGrant(ctx context.Context, tx *Transaction, runID string, expectedGrantEpoch uint64) (RunExecutionGrant, bool, error) {
	if expectedGrantEpoch == 0 || expectedGrantEpoch > maxStoreFence {
		return RunExecutionGrant{}, false, ErrCASConflict
	}
	current, err := loadRunExecutionGrant(ctx, tx, runID, true)
	if err != nil {
		return RunExecutionGrant{}, false, err
	}
	if current.Grant.GrantEpoch != expectedGrantEpoch {
		return RunExecutionGrant{}, false, ErrCASConflict
	}
	if current.RevokedAt != nil {
		return current, false, nil
	}
	var revokedAt time.Time
	if err := tx.queryRow(ctx, `
		UPDATE pm.run_execution_grants SET revoked_at = clock_timestamp()
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
			AND grant_epoch = $4 AND revoked_at IS NULL
		RETURNING revoked_at
	`, current.Grant.Scope.OrgID, current.Grant.Scope.WorkspaceID, runID, expectedGrantEpoch).Scan(&revokedAt); errors.Is(err, pgx.ErrNoRows) {
		return RunExecutionGrant{}, false, ErrCASConflict
	} else if err != nil {
		return RunExecutionGrant{}, false, safeMetadataWriteError("revoke accepted execution grant", err)
	}
	revokedAt = revokedAt.UTC()
	current.RevokedAt = &revokedAt
	return current, true, nil
}
