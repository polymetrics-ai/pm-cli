package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type Receipt struct {
	ReceiptID string
	Kind      string
	RunID     string
	Payload   contract.SafePayload
}

func (receipt Receipt) validate() error {
	if !validStoreID(receipt.ReceiptID) || !validStoreName(receipt.Kind) || !validStoreID(receipt.RunID) {
		return errors.New("receipt identity or kind is invalid")
	}
	if err := receipt.Payload.Validate(); err != nil {
		return fmt.Errorf("validate receipt payload: %w", err)
	}
	return nil
}

func insertReceipt(ctx context.Context, tx *Transaction, receipt Receipt, committedAt time.Time) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	if err := receipt.validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(receipt.Payload)
	if err != nil {
		return fmt.Errorf("encode receipt payload: %w", err)
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.receipts (
			org_id, workspace_id, run_id, receipt_id, kind, payload, recorded_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, scope.OrgID, scope.WorkspaceID, receipt.RunID, receipt.ReceiptID, receipt.Kind, json.RawMessage(payload), committedAt); err != nil {
		return fmt.Errorf("insert durable receipt: %w", err)
	}
	return nil
}
