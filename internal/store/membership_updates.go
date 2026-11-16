package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// ErrMembershipGrantLimit reports that an update would give a membership more
// grants than one membership can hold.
var ErrMembershipGrantLimit = errors.New("membership grant list is full")

// MembershipUpdateAuditAction is the action of the workspace audit record that
// each changed membership update writes.
const MembershipUpdateAuditAction = "membership.update"

// UpdateMembershipConnectionsRequest adds connections to one membership at its
// exact current control revision. ConnectionIDs is strictly sorted.
type UpdateMembershipConnectionsRequest struct {
	Scope            contract.Scope
	MembershipID     string
	ConnectionIDs    []string
	ExpectedRevision uint64
	AuditID          string
}

// MembershipConnectionUpdate is the locked membership before the update, the
// membership after it, and the connections that the update added. Unchanged is
// true when the membership held each connection already. Then nothing was
// written.
type MembershipConnectionUpdate struct {
	Previous  MembershipAuthority
	Current   MembershipAuthority
	Added     []string
	Unchanged bool
}

// UpdateMembershipConnections locks the membership and compares its control
// revision. It refuses a revoked or expired membership. For a change, it
// writes the new grants, a new control revision and policy epoch, the new
// membership collection revision and one workspace audit record in the
// caller's transaction. It never removes a grant.
func UpdateMembershipConnections(
	ctx context.Context,
	tx *Transaction,
	request UpdateMembershipConnectionsRequest,
) (MembershipConnectionUpdate, error) {
	if err := tx.requireMatchingScope(request.Scope); err != nil {
		return MembershipConnectionUpdate{}, err
	}
	if !validStoreID(request.MembershipID) || len(request.ConnectionIDs) == 0 ||
		!validStoreIDList(request.ConnectionIDs) || request.ExpectedRevision == 0 ||
		!validStoreID(request.AuditID) {
		return MembershipConnectionUpdate{}, errors.New("membership connection update is invalid")
	}
	previous, err := lockMembership(ctx, tx, request.MembershipID)
	if err != nil {
		return MembershipConnectionUpdate{}, err
	}
	if previous.ControlRevision != request.ExpectedRevision {
		return MembershipConnectionUpdate{}, ErrCASConflict
	}
	now, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return MembershipConnectionUpdate{}, err
	}
	if previous.RevokedAt != nil || previous.ExpiresAt != nil && !previous.ExpiresAt.After(now) {
		return MembershipConnectionUpdate{}, ErrImmutableConflict
	}
	added := make([]string, 0, len(request.ConnectionIDs))
	for _, connectionID := range request.ConnectionIDs {
		if !slices.Contains(previous.ConnectionIDs, connectionID) {
			added = append(added, connectionID)
		}
	}
	if len(added) == 0 {
		return MembershipConnectionUpdate{Previous: previous, Current: previous, Added: []string{}, Unchanged: true}, nil
	}
	connections := append(append([]string{}, previous.ConnectionIDs...), added...)
	slices.Sort(connections)
	grants := append([]string{}, previous.ResourceGrants...)
	for _, connectionID := range added {
		if grant := "connection:" + connectionID; !slices.Contains(grants, grant) {
			grants = append(grants, grant)
		}
	}
	slices.Sort(grants)
	if !validStoreIDList(connections) || !validStoreResourceGrantList(grants) {
		return MembershipConnectionUpdate{}, ErrMembershipGrantLimit
	}

	current := previous
	if err := tx.queryRow(ctx, `
		UPDATE pm.memberships
		SET resource_grants = $3, connection_ids = $4,
		    policy_epoch = policy_epoch + 1, control_revision = control_revision + 1
		WHERE membership_id = $1 AND control_revision = $2 AND revoked_at IS NULL
		RETURNING policy_epoch, control_revision
	`, request.MembershipID, request.ExpectedRevision, grants, connections).Scan(
		&current.PolicyEpoch, &current.ControlRevision,
	); errors.Is(err, pgx.ErrNoRows) {
		return MembershipConnectionUpdate{}, ErrCASConflict
	} else if err != nil {
		return MembershipConnectionUpdate{}, safeMetadataWriteError("update workspace membership connections", err)
	}
	current.ResourceGrants = grants
	current.ConnectionIDs = connections
	if _, err := bumpIdentityCollection(ctx, tx, IdentityCollectionMemberships); err != nil {
		return MembershipConnectionUpdate{}, err
	}

	references := []contract.SafeReference{
		{Name: "membership", Ref: previous.MembershipID},
		{Name: "principal", Ref: previous.Principal.PrincipalID},
	}
	for index, connectionID := range added {
		references = append(references, contract.SafeReference{
			Name: "added_connection_" + strconv.Itoa(index+1), Ref: connectionID,
		})
	}
	if err := insertWorkspaceAuditEntry(ctx, tx, AuditEntry{
		AuditID:   request.AuditID,
		Action:    MembershipUpdateAuditAction,
		TargetRef: previous.MembershipID,
		Payload: contract.SafePayload{
			References: references,
			Counts: []contract.SafeCount{
				{Name: "previous_control_revision", Value: previous.ControlRevision},
				{Name: "control_revision", Value: current.ControlRevision},
				{Name: "added_connections", Value: uint64(len(added))},
			},
			ReasonCodes: []string{},
		},
	}); err != nil {
		return MembershipConnectionUpdate{}, err
	}
	return MembershipConnectionUpdate{Previous: previous, Current: current, Added: added}, nil
}

// insertWorkspaceAuditEntry writes one workspace audit record that belongs to
// no run. The scope and both actors come from the transaction. Row-level
// security refuses another scope or another actor.
func insertWorkspaceAuditEntry(ctx context.Context, tx *Transaction, entry AuditEntry) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	if err := entry.validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(entry.Payload)
	if err != nil {
		return fmt.Errorf("encode workspace audit payload: %w", err)
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.workspace_audit_entries (
			org_id, workspace_id, audit_id,
			requested_actor_id, requested_actor_kind,
			effective_actor_id, effective_actor_kind,
			action, target_ref, payload
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`,
		scope.OrgID,
		scope.WorkspaceID,
		entry.AuditID,
		tx.actor.RequestedActor.PrincipalID,
		string(tx.actor.RequestedActor.Kind),
		tx.actor.EffectiveActor.PrincipalID,
		string(tx.actor.EffectiveActor.Kind),
		entry.Action,
		entry.TargetRef,
		json.RawMessage(payload),
	); err != nil {
		return safeMetadataWriteError("insert workspace audit entry", err)
	}
	return nil
}
