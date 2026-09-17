package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

var (
	ErrRunInputBoundInvalid = errors.New("run input snapshot byte bound is invalid")
	ErrRunInputTooLarge     = errors.New("run input snapshot exceeds byte bound")
)

// RunInputSnapshot is the immutable logical invocation input accepted for one
// run. Static operation descriptors remain in the generated connector catalog.
type RunInputSnapshot struct {
	Scope          contract.Scope          `json:"scope"`
	RunID          string                  `json:"run_id"`
	InputRef       string                  `json:"input_ref"`
	ConnectionID   string                  `json:"connection_id"`
	Input          contract.PresentValue   `json:"input"`
	ResultControls contract.ResultControls `json:"result_controls"`
	ApprovalRef    contract.PresentValue   `json:"approval_ref"`
}

// PutRunInputSnapshot writes exactly one immutable logical input snapshot for an
// already inserted RunSpec. maxBytes bounds the complete encoded snapshot and
// must be resolved explicitly by the caller.
func PutRunInputSnapshot(
	ctx context.Context,
	tx *Transaction,
	spec contract.RunSpec,
	snapshot RunInputSnapshot,
	maxBytes uint64,
) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	if maxBytes == 0 {
		return ErrRunInputBoundInvalid
	}
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("validate accepted RunSpec for input snapshot: %w", err)
	}
	if err := validateRunInputSnapshot(snapshot); err != nil {
		return err
	}
	specScope := contract.Scope{OrgID: spec.OrgID, WorkspaceID: spec.WorkspaceID}
	if !scope.Equal(specScope) || !scope.Equal(snapshot.Scope) {
		return ErrScopeMismatch
	}
	if snapshot.RunID != spec.RunID || !runSpecContainsInputRef(spec, snapshot.InputRef) {
		return fmt.Errorf("bind run input snapshot to accepted RunSpec: %w", ErrImmutableConflict)
	}

	encodedSnapshot, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode run input snapshot: %w", err)
	}
	if uint64(len(encodedSnapshot)) > maxBytes {
		return ErrRunInputTooLarge
	}
	encodedSpec, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("encode accepted RunSpec for input snapshot: %w", err)
	}

	tag, err := tx.exec(ctx, `
		INSERT INTO pm.run_input_snapshots (
			org_id, workspace_id, run_id, input_ref, connection_id, snapshot
		)
		SELECT $1, $2, $3, $4, $5, $6
		FROM pm.run_specs AS accepted
		WHERE accepted.org_id = $1
		  AND accepted.workspace_id = $2
		  AND accepted.run_id = $3
		  AND accepted.spec = $7
		  AND (accepted.spec->'input_refs') ? $4::text
	`,
		scope.OrgID,
		scope.WorkspaceID,
		snapshot.RunID,
		snapshot.InputRef,
		snapshot.ConnectionID,
		json.RawMessage(encodedSnapshot),
		json.RawMessage(encodedSpec),
	)
	if err != nil {
		return safeMetadataWriteError("insert run input snapshot", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	var exists bool
	if err := tx.queryRow(ctx, `
		SELECT true
		FROM pm.run_specs
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
	`, scope.OrgID, scope.WorkspaceID, snapshot.RunID).Scan(&exists); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("classify run input RunSpec binding: %w", err)
	}
	return fmt.Errorf("bind run input snapshot to accepted RunSpec: %w", ErrImmutableConflict)
}

// LoadRunInputSnapshot loads the immutable logical invocation input for runID.
func LoadRunInputSnapshot(ctx context.Context, tx *Transaction, runID string) (RunInputSnapshot, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return RunInputSnapshot{}, err
	}
	if !validStoreID(runID) {
		return RunInputSnapshot{}, errors.New("run input snapshot RunID is invalid")
	}

	var inputRef, connectionID string
	var encoded []byte
	if err := tx.queryRow(ctx, `
		SELECT input_ref, connection_id, snapshot
		FROM pm.run_input_snapshots
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
	`, scope.OrgID, scope.WorkspaceID, runID).Scan(&inputRef, &connectionID, &encoded); errors.Is(err, pgx.ErrNoRows) {
		return RunInputSnapshot{}, ErrNotFound
	} else if err != nil {
		return RunInputSnapshot{}, fmt.Errorf("load run input snapshot: %w", err)
	}

	var snapshot RunInputSnapshot
	if err := json.Unmarshal(encoded, &snapshot); err != nil {
		return RunInputSnapshot{}, fmt.Errorf("decode stored run input snapshot: %w", err)
	}
	if err := validateRunInputSnapshot(snapshot); err != nil {
		return RunInputSnapshot{}, fmt.Errorf("validate stored run input snapshot: %w", err)
	}
	if !snapshot.Scope.Equal(scope) || snapshot.RunID != runID ||
		snapshot.InputRef != inputRef || snapshot.ConnectionID != connectionID {
		return RunInputSnapshot{}, ErrImmutableConflict
	}
	return snapshot, nil
}

func validateRunInputSnapshot(snapshot RunInputSnapshot) error {
	if err := snapshot.Scope.Validate(); err != nil {
		return err
	}
	if !validStoreID(snapshot.RunID) || !validStoreID(snapshot.InputRef) || !validStoreID(snapshot.ConnectionID) {
		return errors.New("run input snapshot contains an invalid identity")
	}
	if snapshot.Input.Presence() != contract.PresenceValue || snapshot.Input.Kind() != contract.ValueKindObject {
		return errors.New("run input snapshot must contain a present logical field-ID object")
	}
	fields, _ := snapshot.Input.Fields()
	for fieldID := range fields {
		if !validStoreID(fieldID) {
			return errors.New("run input snapshot contains an invalid logical field ID")
		}
	}
	if err := snapshot.ResultControls.Validate(); err != nil {
		return fmt.Errorf("validate run input result controls: %w", err)
	}
	switch snapshot.ApprovalRef.Presence() {
	case contract.PresenceAbsent, contract.PresenceNull:
	case contract.PresenceValue:
		approvalRef, ok := snapshot.ApprovalRef.TextValue()
		if !ok || !validStoreID(approvalRef) {
			return errors.New("run input approval reference must be an opaque string identity")
		}
	default:
		return errors.New("run input approval reference presence is invalid")
	}
	return nil
}

func runSpecContainsInputRef(spec contract.RunSpec, inputRef string) bool {
	for _, candidate := range spec.InputRefs {
		if candidate == inputRef {
			return true
		}
	}
	return false
}
