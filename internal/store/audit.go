package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type AuditEntry struct {
	AuditID   string
	Action    string
	TargetRef string
	Payload   contract.SafePayload
}

func (entry AuditEntry) validate() error {
	if !validStoreID(entry.AuditID) || !validStoreName(entry.Action) || !validStoreID(entry.TargetRef) {
		return errors.New("audit identity, action, or target is invalid")
	}
	if err := entry.Payload.Validate(); err != nil {
		return fmt.Errorf("validate audit payload: %w", err)
	}
	return nil
}

func insertAuditEntry(ctx context.Context, tx *Transaction, entry AuditEntry, committedAt time.Time) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	if err := entry.validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(entry.Payload)
	if err != nil {
		return fmt.Errorf("encode audit payload: %w", err)
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.audit_entries (
			org_id, workspace_id, run_id, audit_id,
			requested_actor_id, requested_actor_kind,
			effective_actor_id, effective_actor_kind,
			action, target_ref, payload, recorded_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`,
		scope.OrgID,
		scope.WorkspaceID,
		entry.TargetRef,
		entry.AuditID,
		tx.actor.RequestedActor.PrincipalID,
		string(tx.actor.RequestedActor.Kind),
		tx.actor.EffectiveActor.PrincipalID,
		string(tx.actor.EffectiveActor.Kind),
		entry.Action,
		entry.TargetRef, json.RawMessage(payload), committedAt); err != nil {
		return fmt.Errorf("insert audit entry: %w", err)
	}
	return nil
}
