package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

var ErrAdmissionReceiptExpired = errors.New("admission retry receipt expired")

// AdmissionReceiptExpiredError retains only the existing scope-authorized run
// reference. Expiry is a refusal to replay a response, never a fresh key.
type AdmissionReceiptExpiredError struct {
	ExistingRunID string
}

func (err *AdmissionReceiptExpiredError) Error() string {
	return "admission retry receipt expired; inspect the existing run"
}

func (err *AdmissionReceiptExpiredError) Unwrap() error {
	return ErrAdmissionReceiptExpired
}

type admissionSafeResponse struct {
	RunID        string                  `json:"run_id"`
	IntentDigest contract.SemanticDigest `json:"intent_digest"`
	AcceptedAt   time.Time               `json:"accepted_at"`
}

func encodeAdmissionResponse(result AdmissionResult) ([]byte, error) {
	return json.Marshal(admissionSafeResponse{
		RunID:        result.RunID,
		IntentDigest: result.IntentDigest,
		AcceptedAt:   result.AcceptedAt,
	})
}

func validateAdmissionResponse(encoded []byte, result AdmissionResult) error {
	var response admissionSafeResponse
	if err := json.Unmarshal(encoded, &response); err != nil {
		return fmt.Errorf("decode retained admission response: %w", err)
	}
	if response.RunID != result.RunID || response.IntentDigest != result.IntentDigest || !response.AcceptedAt.Equal(result.AcceptedAt) {
		return ErrImmutableConflict
	}
	return nil
}

func lockAdmissionKey(ctx context.Context, tx *Transaction, key AdmissionKey) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	// NUL-frame identities locally: PostgreSQL text cannot carry NUL. Sharing
	// this lock with compaction linearizes recovery without rewriting the key.
	material := scope.OrgID + "\x00" + scope.WorkspaceID + "\x00" + key.PrincipalID + "\x00" + key.OperationID + "\x00" + key.TargetID + "\x00" + key.IdempotencyKey
	digest := sha256.Sum256([]byte(material))
	if _, err := tx.exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(binary.BigEndian.Uint64(digest[:8]))); err != nil {
		return fmt.Errorf("serialize admission key: %w", err)
	}
	return nil
}

// CompactAdmissionReceipt removes only the full safe response after at least
// 168 hours of known terminal resolution. The scoped key, digest and run remain
// immutable. Current authorization is the caller's prerequisite, as for lookup.
func CompactAdmissionReceipt(ctx context.Context, tx *Transaction, key AdmissionKey) (bool, error) {
	if _, err := tx.requireScope(); err != nil {
		return false, err
	}
	if err := key.validate(); err != nil {
		return false, err
	}
	if err := lockAdmissionKey(ctx, tx, key); err != nil {
		return false, err
	}
	if _, err := lookupAdmission(ctx, tx, key); err != nil {
		return false, err
	}
	tag, err := tx.exec(ctx, `
		UPDATE pm.admission_keys
		SET safe_response = NULL, response_compacted_at = clock_timestamp()
		WHERE principal_id = $1 AND operation_id = $2 AND target_id = $3 AND idempotency_key = $4
		  AND safe_response IS NOT NULL
		  AND resolved_at IS NOT NULL
		  AND resolved_at <= clock_timestamp() - interval '168 hours'
	`, key.PrincipalID, key.OperationID, key.TargetID, key.IdempotencyKey)
	if err != nil {
		return false, fmt.Errorf("compact resolved admission response: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func resolveAdmissionReceipt(ctx context.Context, tx *Transaction, runID string) error {
	var retainedRun string
	err := tx.queryRow(ctx, `
		UPDATE pm.admission_keys
		SET resolved_at = COALESCE(resolved_at, clock_timestamp())
		WHERE run_id = $1
		RETURNING run_id
	`, runID).Scan(&retainedRun)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrImmutableConflict
	}
	if err != nil {
		return fmt.Errorf("resolve admission response retention: %w", err)
	}
	return nil
}
