package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type Membership struct {
	Scope        contract.Scope
	MembershipID string
	IdentityID   string
	Role         string
	PolicyEpoch  uint64
	ExpiresAt    *time.Time
	RevokedAt    *time.Time
}

func (membership Membership) validate() error {
	if err := membership.Scope.Validate(); err != nil {
		return fmt.Errorf("validate membership scope: %w", err)
	}
	if !validStoreID(membership.MembershipID) || !validStoreID(membership.IdentityID) || !validStoreName(membership.Role) {
		return errors.New("membership identity, principal, or role is invalid")
	}
	if membership.PolicyEpoch == 0 {
		return errors.New("membership policy epoch must be positive")
	}
	for _, instant := range []*time.Time{membership.ExpiresAt, membership.RevokedAt} {
		if instant != nil && (instant.IsZero() || instant.Location() != time.UTC) {
			return errors.New("membership times must be explicit UTC instants")
		}
	}
	return nil
}

func RegisterMembership(ctx context.Context, tx *Transaction, membership Membership) error {
	if err := tx.requireMatchingScope(membership.Scope); err != nil {
		return err
	}
	if err := membership.validate(); err != nil {
		return err
	}
	membership = normalizeMembershipTimes(membership)
	if err := requireActiveWorkspace(ctx, tx); err != nil {
		return err
	}
	tag, err := tx.exec(ctx, `
		INSERT INTO pm.memberships (
			org_id, workspace_id, membership_id, identity_id, role,
			policy_epoch, expires_at, revoked_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT DO NOTHING
	`,
		membership.Scope.OrgID,
		membership.Scope.WorkspaceID,
		membership.MembershipID,
		membership.IdentityID,
		membership.Role,
		membership.PolicyEpoch,
		membership.ExpiresAt,
		membership.RevokedAt,
	)
	if err != nil {
		return safeMetadataWriteError("register membership", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	stored, err := LoadMembership(ctx, tx, membership.MembershipID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrImmutableConflict
		}
		return err
	}
	if !equalMembership(stored, membership) {
		return ErrImmutableConflict
	}
	return nil
}

func LoadMembership(ctx context.Context, tx *Transaction, membershipID string) (Membership, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return Membership{}, err
	}
	if !validStoreID(membershipID) {
		return Membership{}, errors.New("membership identity is invalid")
	}
	membership := Membership{Scope: scope, MembershipID: membershipID}
	if err := tx.queryRow(ctx, `
		SELECT identity_id, role, policy_epoch, expires_at, revoked_at
		FROM pm.memberships
		WHERE membership_id = $1
	`, membershipID).Scan(
		&membership.IdentityID,
		&membership.Role,
		&membership.PolicyEpoch,
		&membership.ExpiresAt,
		&membership.RevokedAt,
	); errors.Is(err, pgx.ErrNoRows) {
		return Membership{}, ErrNotFound
	} else if err != nil {
		return Membership{}, fmt.Errorf("load membership: %w", err)
	}
	membership = normalizeMembershipTimes(membership)
	if err := membership.validate(); err != nil {
		return Membership{}, fmt.Errorf("validate stored membership: %w", err)
	}
	return membership, nil
}

func normalizeMembershipTimes(membership Membership) Membership {
	if membership.ExpiresAt != nil {
		value := membership.ExpiresAt.UTC().Truncate(time.Microsecond)
		membership.ExpiresAt = &value
	}
	if membership.RevokedAt != nil {
		value := membership.RevokedAt.UTC().Truncate(time.Microsecond)
		membership.RevokedAt = &value
	}
	return membership
}

func equalMembership(left, right Membership) bool {
	return left.Scope.Equal(right.Scope) &&
		left.MembershipID == right.MembershipID &&
		left.IdentityID == right.IdentityID &&
		left.Role == right.Role &&
		left.PolicyEpoch == right.PolicyEpoch &&
		equalOptionalTime(left.ExpiresAt, right.ExpiresAt) &&
		equalOptionalTime(left.RevokedAt, right.RevokedAt)
}

func equalOptionalTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

type IdentityMutationKey struct {
	Scope          contract.Scope
	PrincipalID    string
	OperationID    string
	TargetID       string
	IdempotencyKey string
}

func (key IdentityMutationKey) validate() error {
	if err := key.Scope.Validate(); err != nil {
		return fmt.Errorf("validate identity mutation scope: %w", err)
	}
	if !validStoreID(key.PrincipalID) || !validStoreID(key.OperationID) ||
		!validStoreID(key.TargetID) || !validStoreID(key.IdempotencyKey) {
		return errors.New("identity mutation key contains an invalid identity")
	}
	return nil
}

type IdentityMutation struct {
	Key            IdentityMutationKey
	IntentDigest   contract.SemanticDigest
	ResultRef      string
	ResultRevision uint64
}

func (mutation IdentityMutation) validate() error {
	if err := mutation.Key.validate(); err != nil {
		return err
	}
	if err := mutation.IntentDigest.Validate(); err != nil {
		return fmt.Errorf("validate identity mutation digest: %w", err)
	}
	if !validStoreID(mutation.ResultRef) || mutation.ResultRevision == 0 {
		return errors.New("identity mutation result binding is invalid")
	}
	return nil
}

type IdentityMutationResult struct {
	Mutation IdentityMutation
	Replayed bool
}

func RecordIdentityMutation(ctx context.Context, tx *Transaction, mutation IdentityMutation) (IdentityMutationResult, error) {
	if err := tx.requireMatchingScope(mutation.Key.Scope); err != nil {
		return IdentityMutationResult{}, err
	}
	if err := mutation.validate(); err != nil {
		return IdentityMutationResult{}, err
	}
	if err := requireActiveWorkspace(ctx, tx); err != nil {
		return IdentityMutationResult{}, err
	}
	tag, err := tx.exec(ctx, `
		INSERT INTO pm.identity_mutation_keys (
			org_id, workspace_id, principal_id, operation_id, target_id,
			idempotency_key, intent_digest, result_ref, result_revision
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT DO NOTHING
	`,
		mutation.Key.Scope.OrgID,
		mutation.Key.Scope.WorkspaceID,
		mutation.Key.PrincipalID,
		mutation.Key.OperationID,
		mutation.Key.TargetID,
		mutation.Key.IdempotencyKey,
		string(mutation.IntentDigest),
		mutation.ResultRef,
		mutation.ResultRevision,
	)
	if err != nil {
		return IdentityMutationResult{}, safeMetadataWriteError("record identity mutation", err)
	}
	if tag.RowsAffected() == 1 {
		return IdentityMutationResult{Mutation: mutation}, nil
	}
	stored, err := LoadIdentityMutation(ctx, tx, mutation.Key)
	if err != nil {
		return IdentityMutationResult{}, err
	}
	if stored.IntentDigest != mutation.IntentDigest {
		return IdentityMutationResult{}, ErrIdempotencyConflict
	}
	if stored.ResultRef != mutation.ResultRef || stored.ResultRevision != mutation.ResultRevision {
		return IdentityMutationResult{}, ErrImmutableConflict
	}
	return IdentityMutationResult{Mutation: stored, Replayed: true}, nil
}

func LoadIdentityMutation(ctx context.Context, tx *Transaction, key IdentityMutationKey) (IdentityMutation, error) {
	if err := tx.requireMatchingScope(key.Scope); err != nil {
		return IdentityMutation{}, err
	}
	if err := key.validate(); err != nil {
		return IdentityMutation{}, err
	}
	mutation := IdentityMutation{Key: key}
	var digest string
	if err := tx.queryRow(ctx, `
		SELECT intent_digest, result_ref, result_revision
		FROM pm.identity_mutation_keys
		WHERE principal_id = $1 AND operation_id = $2 AND target_id = $3 AND idempotency_key = $4
	`, key.PrincipalID, key.OperationID, key.TargetID, key.IdempotencyKey).Scan(
		&digest,
		&mutation.ResultRef,
		&mutation.ResultRevision,
	); errors.Is(err, pgx.ErrNoRows) {
		return IdentityMutation{}, ErrNotFound
	} else if err != nil {
		return IdentityMutation{}, fmt.Errorf("load identity mutation: %w", err)
	}
	mutation.IntentDigest = contract.SemanticDigest(digest)
	if err := mutation.validate(); err != nil {
		return IdentityMutation{}, fmt.Errorf("validate stored identity mutation: %w", err)
	}
	return mutation, nil
}
