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

type AdmissionKey struct {
	PrincipalID    string
	OperationID    string
	TargetID       string
	IdempotencyKey string
}

func (key AdmissionKey) validate() error {
	if !validStoreID(key.PrincipalID) || !validStoreID(key.OperationID) || !validStoreID(key.TargetID) || !validStoreID(key.IdempotencyKey) {
		return errors.New("admission key contains an invalid identity")
	}
	return nil
}

type OutboxCommand struct {
	CommandID  string
	Kind       string
	PayloadRef string
}

func (command OutboxCommand) validate() error {
	if !validStoreID(command.CommandID) || !validStoreName(command.Kind) || !validStoreID(command.PayloadRef) {
		return errors.New("outbox command contains an invalid identity or kind")
	}
	return nil
}

type AdmissionRequest struct {
	Key             AdmissionKey
	BudgetSnapshot  RunBudgetSnapshot
	Spec            contract.RunSpec
	Grant           contract.ExecutionGrant
	InputSnapshot   RunInputSnapshot
	WorkflowBinding contract.WorkflowBinding
	Receipt         Receipt
	Audit           AuditEntry
	Event           EventInput
	Outbox          OutboxCommand
}

type AdmissionResult struct {
	RunID          string
	IntentDigest   contract.SemanticDigest
	AcceptedAt     time.Time
	Replayed       bool
	ReceiptExpired bool
}

type IdempotencyConflictError struct {
	ExistingRunID string
}

func (err *IdempotencyConflictError) Error() string {
	return fmt.Sprintf("%v: an accepted run already owns this key", ErrIdempotencyConflict)
}

func (err *IdempotencyConflictError) Unwrap() error {
	return ErrIdempotencyConflict
}

// ResolveAdmissionReplay serializes one scope-authorized admission identity and
// classifies it before any fresh capability, staleness, or capacity decision.
// A missing result leaves the key lock held by the caller's transaction.
func ResolveAdmissionReplay(
	ctx context.Context,
	tx *Transaction,
	key AdmissionKey,
	intentDigest contract.SemanticDigest,
) (AdmissionResult, bool, error) {
	if _, err := tx.requireScope(); err != nil {
		return AdmissionResult{}, false, err
	}
	if err := key.validate(); err != nil {
		return AdmissionResult{}, false, err
	}
	if err := intentDigest.Validate(); err != nil {
		return AdmissionResult{}, false, err
	}
	if err := lockAdmissionKey(ctx, tx, key); err != nil {
		return AdmissionResult{}, false, err
	}
	existing, err := lookupAdmission(ctx, tx, key)
	if errors.Is(err, ErrNotFound) {
		return AdmissionResult{}, false, nil
	}
	if err != nil {
		return AdmissionResult{}, false, err
	}
	if existing.IntentDigest != intentDigest {
		return AdmissionResult{}, true, &IdempotencyConflictError{ExistingRunID: existing.RunID}
	}
	existing.Replayed = true
	if existing.ReceiptExpired {
		return existing, true, &AdmissionReceiptExpiredError{ExistingRunID: existing.RunID}
	}
	return existing, true, nil
}

func AdmitRun(ctx context.Context, tx *Transaction, request AdmissionRequest) (AdmissionResult, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return AdmissionResult{}, err
	}
	if err := request.Key.validate(); err != nil {
		return AdmissionResult{}, err
	}
	if err := request.Spec.Validate(); err != nil {
		return AdmissionResult{}, fmt.Errorf("validate accepted RunSpec: %w", err)
	}
	if err := request.WorkflowBinding.Validate(); err != nil {
		return AdmissionResult{}, fmt.Errorf("validate workflow binding: %w", err)
	}
	if err := request.Receipt.validate(); err != nil {
		return AdmissionResult{}, err
	}
	if err := request.Audit.validate(); err != nil {
		return AdmissionResult{}, err
	}
	if err := request.Event.validate(); err != nil {
		return AdmissionResult{}, err
	}
	if err := request.Outbox.validate(); err != nil {
		return AdmissionResult{}, err
	}
	if request.Spec.OrgID != scope.OrgID || request.Spec.WorkspaceID != scope.WorkspaceID {
		return AdmissionResult{}, ErrScopeMismatch
	}
	if !request.Spec.Actor.Equal(tx.actor) || request.Key.PrincipalID != tx.actor.RequestedActor.PrincipalID {
		return AdmissionResult{}, ErrScopeMismatch
	}
	if request.WorkflowBinding.RunID != request.Spec.RunID || request.Receipt.RunID != request.Spec.RunID || request.Audit.TargetRef != request.Spec.RunID || request.Outbox.PayloadRef != request.Spec.RunID {
		return AdmissionResult{}, errors.New("admission side records do not bind the accepted RunID")
	}
	if err := validateRunBudgetSnapshot(request.BudgetSnapshot); err != nil {
		return AdmissionResult{}, err
	}
	if request.BudgetSnapshot.Binding() != request.Spec.Budget ||
		request.BudgetSnapshot.RunID != request.Spec.RunID {
		return AdmissionResult{}, fmt.Errorf("bind admission budget snapshot: %w", ErrImmutableConflict)
	}
	if err := validateRunGrantBinding(request.Spec, request.Grant); err != nil {
		return AdmissionResult{}, err
	}

	existing, found, err := ResolveAdmissionReplay(ctx, tx, request.Key, request.Spec.IdempotencyFingerprint)
	if err != nil {
		return AdmissionResult{}, err
	}
	if found {
		return existing, nil
	}

	acceptedAt, err := checkPendingAdmission(ctx, tx, request.BudgetSnapshot.Policy.Pending)
	if err != nil {
		return AdmissionResult{}, err
	}
	if !request.Grant.ExpiresAt.After(acceptedAt) {
		return AdmissionResult{}, ErrRunGrantUnavailable
	}
	record, err := contract.NewRunRecord(request.Spec, request.WorkflowBinding, acceptedAt)
	if err != nil {
		return AdmissionResult{}, fmt.Errorf("construct accepted run record: %w", err)
	}
	specJSON, err := json.Marshal(request.Spec)
	if err != nil {
		return AdmissionResult{}, fmt.Errorf("encode accepted RunSpec: %w", err)
	}
	recordJSON, err := json.Marshal(record)
	if err != nil {
		return AdmissionResult{}, fmt.Errorf("encode accepted run record: %w", err)
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.run_specs (
			org_id, workspace_id, run_id, intent_digest, idempotency_fingerprint, spec, accepted_at
		) VALUES ($1, $2, $3, $4, $4, $5, $6)
	`, scope.OrgID, scope.WorkspaceID, request.Spec.RunID, string(request.Spec.IdempotencyFingerprint), json.RawMessage(specJSON), acceptedAt); err != nil {
		return AdmissionResult{}, fmt.Errorf("insert accepted RunSpec: %w", err)
	}
	if err := PutRunBudgetSnapshot(ctx, tx, request.Spec, request.BudgetSnapshot); err != nil {
		return AdmissionResult{}, err
	}
	if err := PutRunInputSnapshot(ctx, tx, request.Spec, request.InputSnapshot, request.BudgetSnapshot.Policy.InputMaxBytes); err != nil {
		return AdmissionResult{}, err
	}
	if err := insertRunExecutionGrant(ctx, tx, request.Spec.RunID, request.Grant); err != nil {
		return AdmissionResult{}, err
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.runs (
			org_id, workspace_id, run_id, state, revision, record, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, scope.OrgID, scope.WorkspaceID, request.Spec.RunID, string(record.State), record.Revision, json.RawMessage(recordJSON), acceptedAt); err != nil {
		return AdmissionResult{}, fmt.Errorf("insert accepted run: %w", err)
	}
	result := AdmissionResult{
		RunID:        request.Spec.RunID,
		IntentDigest: request.Spec.IdempotencyFingerprint,
		AcceptedAt:   acceptedAt,
	}
	responseJSON, err := encodeAdmissionResponse(result)
	if err != nil {
		return AdmissionResult{}, fmt.Errorf("encode safe admission response: %w", err)
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.admission_keys (
			org_id, workspace_id, principal_id, operation_id, target_id, idempotency_key,
			intent_digest, run_id, accepted_at, safe_response
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`,
		scope.OrgID, scope.WorkspaceID,
		request.Key.PrincipalID, request.Key.OperationID, request.Key.TargetID, request.Key.IdempotencyKey,
		string(request.Spec.IdempotencyFingerprint), request.Spec.RunID, acceptedAt, json.RawMessage(responseJSON),
	); err != nil {
		return AdmissionResult{}, fmt.Errorf("insert admission identity: %w", err)
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.pending_reservations (
			org_id, workspace_id, run_id, accepted_at
		) VALUES ($1, $2, $3, $4)
	`, scope.OrgID, scope.WorkspaceID, request.Spec.RunID, acceptedAt); err != nil {
		return AdmissionResult{}, fmt.Errorf("insert pending reservation: %w", err)
	}
	if err := insertReceipt(ctx, tx, request.Receipt, acceptedAt); err != nil {
		return AdmissionResult{}, err
	}
	if err := insertAuditEntry(ctx, tx, request.Audit, acceptedAt); err != nil {
		return AdmissionResult{}, err
	}
	if err := insertOutboxCommand(ctx, tx, request.Spec.RunID, request.Outbox, acceptedAt); err != nil {
		return AdmissionResult{}, err
	}
	if _, err := insertEvent(ctx, tx, request.Spec.RunID, record.Revision, request.Event, acceptedAt); err != nil {
		return AdmissionResult{}, err
	}
	return result, nil
}

func LookupAdmission(ctx context.Context, tx *Transaction, key AdmissionKey) (AdmissionResult, error) {
	if _, err := tx.requireScope(); err != nil {
		return AdmissionResult{}, err
	}
	if err := key.validate(); err != nil {
		return AdmissionResult{}, err
	}
	return lookupAdmission(ctx, tx, key)
}

func lookupAdmission(ctx context.Context, tx *Transaction, key AdmissionKey) (AdmissionResult, error) {
	const query = `
		SELECT run_id, intent_digest, accepted_at, safe_response
		FROM pm.admission_keys
		WHERE principal_id = $1 AND operation_id = $2 AND target_id = $3 AND idempotency_key = $4
	`
	var result AdmissionResult
	var digest string
	var response []byte
	err := tx.queryRow(ctx, query, key.PrincipalID, key.OperationID, key.TargetID, key.IdempotencyKey).Scan(&result.RunID, &digest, &result.AcceptedAt, &response)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdmissionResult{}, ErrNotFound
	}
	if err != nil {
		return AdmissionResult{}, fmt.Errorf("lookup admission identity: %w", err)
	}
	result.IntentDigest = contract.SemanticDigest(digest)
	if err := result.IntentDigest.Validate(); err != nil {
		return AdmissionResult{}, fmt.Errorf("validate stored admission digest: %w", err)
	}
	result.AcceptedAt = result.AcceptedAt.UTC()
	result.ReceiptExpired = response == nil
	if !result.ReceiptExpired {
		if err := validateAdmissionResponse(response, result); err != nil {
			return AdmissionResult{}, err
		}
	}
	return result, nil
}

func LoadRun(ctx context.Context, tx *Transaction, runID string) (contract.RunRecord, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return contract.RunRecord{}, err
	}
	if !validStoreID(runID) {
		return contract.RunRecord{}, errors.New("run identity is invalid")
	}
	var encoded []byte
	if err := tx.queryRow(ctx, `SELECT record FROM pm.runs WHERE run_id = $1`, runID).Scan(&encoded); errors.Is(err, pgx.ErrNoRows) {
		return contract.RunRecord{}, ErrNotFound
	} else if err != nil {
		return contract.RunRecord{}, fmt.Errorf("load run: %w", err)
	}
	var record contract.RunRecord
	if err := json.Unmarshal(encoded, &record); err != nil {
		return contract.RunRecord{}, fmt.Errorf("decode stored run: %w", err)
	}
	if record.Spec.RunID != runID || record.Spec.OrgID != scope.OrgID || record.Spec.WorkspaceID != scope.WorkspaceID {
		return contract.RunRecord{}, ErrScopeMismatch
	}
	return record, nil
}

func insertOutboxCommand(ctx context.Context, tx *Transaction, runID string, command OutboxCommand, createdAt time.Time) error {
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
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.outbox_commands (
			org_id, workspace_id, run_id, command_id, kind, payload_ref, created_at, available_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
	`, scope.OrgID, scope.WorkspaceID, runID, command.CommandID, command.Kind, command.PayloadRef, createdAt); err != nil {
		return fmt.Errorf("insert outbox command: %w", err)
	}
	return nil
}
