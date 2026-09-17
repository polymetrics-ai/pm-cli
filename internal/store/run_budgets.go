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

// RunBudgetPolicy is the complete finite policy applied to one admission. Every
// field is resolved by typed server configuration before admission begins.
type RunBudgetPolicy struct {
	Pending              AdmissionLimits `json:"pending"`
	RunLifetime          time.Duration   `json:"run_lifetime"`
	InputMaxBytes        uint64          `json:"input_max_bytes"`
	WorkspaceActiveRuns  uint64          `json:"workspace_active_runs"`
	WorkflowPayloadBytes uint64          `json:"workflow_payload_bytes"`
	WorkflowFanout       uint64          `json:"workflow_fanout"`
}

func (policy RunBudgetPolicy) Validate() error {
	if err := policy.Pending.Validate(); err != nil {
		return err
	}
	if policy.RunLifetime <= 0 {
		return errors.New("run lifetime must be positive")
	}
	limits := []uint64{
		policy.InputMaxBytes,
		policy.WorkspaceActiveRuns,
		policy.WorkflowPayloadBytes,
		policy.WorkflowFanout,
	}
	for _, limit := range limits {
		if limit == 0 || limit > maxStoreFence {
			return errors.New("run budget bounds must be positive signed 64-bit values")
		}
	}
	return nil
}

// RunBudgetSnapshot is the immutable policy authority applied to an accepted
// run. Its digest is bound from the complete scope, identity, revision and
// policy; a RunSpec carries only the corresponding BudgetBinding.
type RunBudgetSnapshot struct {
	Scope     contract.Scope          `json:"scope"`
	RunID     string                  `json:"run_id"`
	BudgetRef string                  `json:"budget_ref"`
	Revision  uint64                  `json:"revision"`
	Policy    RunBudgetPolicy         `json:"policy"`
	Digest    contract.SemanticDigest `json:"digest"`
}

func NewRunBudgetSnapshot(
	scope contract.Scope,
	runID string,
	budgetRef string,
	policy RunBudgetPolicy,
) (RunBudgetSnapshot, error) {
	snapshot := RunBudgetSnapshot{
		Scope:     scope,
		RunID:     runID,
		BudgetRef: budgetRef,
		Revision:  1,
		Policy:    policy,
	}
	digest, err := runBudgetDigest(snapshot)
	if err != nil {
		return RunBudgetSnapshot{}, err
	}
	snapshot.Digest = digest
	if err := validateRunBudgetSnapshot(snapshot); err != nil {
		return RunBudgetSnapshot{}, err
	}
	return snapshot, nil
}

func (snapshot RunBudgetSnapshot) Binding() contract.BudgetBinding {
	return contract.BudgetBinding{
		BudgetRef: snapshot.BudgetRef,
		Revision:  snapshot.Revision,
		Digest:    snapshot.Digest,
	}
}

// PutRunBudgetSnapshot persists the exact policy applied to an already inserted
// RunSpec and proves that the RunSpec carries the same immutable binding.
func PutRunBudgetSnapshot(
	ctx context.Context,
	tx *Transaction,
	spec contract.RunSpec,
	snapshot RunBudgetSnapshot,
) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("validate accepted RunSpec for budget snapshot: %w", err)
	}
	if err := validateRunBudgetSnapshot(snapshot); err != nil {
		return err
	}
	specScope := contract.Scope{OrgID: spec.OrgID, WorkspaceID: spec.WorkspaceID}
	if !scope.Equal(specScope) || !scope.Equal(snapshot.Scope) {
		return ErrScopeMismatch
	}
	if snapshot.RunID != spec.RunID || snapshot.Binding() != spec.Budget {
		return fmt.Errorf("bind run budget snapshot to accepted RunSpec: %w", ErrImmutableConflict)
	}

	encodedSnapshot, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode run budget snapshot: %w", err)
	}
	encodedSpec, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("encode accepted RunSpec for budget snapshot: %w", err)
	}
	tag, err := tx.exec(ctx, `
		INSERT INTO pm.run_budget_snapshots (
			org_id, workspace_id, run_id, budget_ref, revision, digest, snapshot
		)
		SELECT $1, $2, $3, $4, $5, $6, $7
		FROM pm.run_specs AS accepted
		WHERE accepted.org_id = $1
		  AND accepted.workspace_id = $2
		  AND accepted.run_id = $3
		  AND accepted.spec = $8
		  AND accepted.spec->'budget' = jsonb_build_object(
			  'budget_ref', $4::text,
			  'revision', $5::bigint,
			  'digest', $6::text
		  )
	`,
		scope.OrgID,
		scope.WorkspaceID,
		snapshot.RunID,
		snapshot.BudgetRef,
		int64(snapshot.Revision),
		string(snapshot.Digest),
		json.RawMessage(encodedSnapshot),
		json.RawMessage(encodedSpec),
	)
	if err != nil {
		return safeMetadataWriteError("insert run budget snapshot", err)
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
		return fmt.Errorf("classify run budget RunSpec binding: %w", err)
	}
	return fmt.Errorf("bind run budget snapshot to accepted RunSpec: %w", ErrImmutableConflict)
}

func LoadRunBudgetSnapshot(ctx context.Context, tx *Transaction, runID string) (RunBudgetSnapshot, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return RunBudgetSnapshot{}, err
	}
	if !validStoreID(runID) {
		return RunBudgetSnapshot{}, errors.New("run budget snapshot RunID is invalid")
	}
	var budgetRef, digest string
	var revision int64
	var encoded []byte
	if err := tx.queryRow(ctx, `
		SELECT budget_ref, revision, digest, snapshot
		FROM pm.run_budget_snapshots
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
	`, scope.OrgID, scope.WorkspaceID, runID).Scan(&budgetRef, &revision, &digest, &encoded); errors.Is(err, pgx.ErrNoRows) {
		return RunBudgetSnapshot{}, ErrNotFound
	} else if err != nil {
		return RunBudgetSnapshot{}, fmt.Errorf("load run budget snapshot: %w", err)
	}
	if revision <= 0 {
		return RunBudgetSnapshot{}, ErrImmutableConflict
	}
	var snapshot RunBudgetSnapshot
	if err := json.Unmarshal(encoded, &snapshot); err != nil {
		return RunBudgetSnapshot{}, fmt.Errorf("decode stored run budget snapshot: %w", err)
	}
	if err := validateRunBudgetSnapshot(snapshot); err != nil {
		return RunBudgetSnapshot{}, fmt.Errorf("validate stored run budget snapshot: %w", err)
	}
	if !snapshot.Scope.Equal(scope) || snapshot.RunID != runID ||
		snapshot.BudgetRef != budgetRef || snapshot.Revision != uint64(revision) ||
		string(snapshot.Digest) != digest {
		return RunBudgetSnapshot{}, ErrImmutableConflict
	}
	return snapshot, nil
}

func validateRunBudgetSnapshot(snapshot RunBudgetSnapshot) error {
	if err := snapshot.Scope.Validate(); err != nil {
		return err
	}
	if !validStoreID(snapshot.RunID) || !validStoreID(snapshot.BudgetRef) ||
		snapshot.Revision == 0 || snapshot.Revision > maxStoreFence {
		return errors.New("run budget snapshot contains an invalid identity or revision")
	}
	if err := snapshot.Policy.Validate(); err != nil {
		return fmt.Errorf("validate run budget policy: %w", err)
	}
	if err := snapshot.Digest.Validate(); err != nil {
		return fmt.Errorf("validate run budget digest: %w", err)
	}
	digest, err := runBudgetDigest(snapshot)
	if err != nil {
		return err
	}
	if digest != snapshot.Digest {
		return fmt.Errorf("run budget digest does not bind the snapshot: %w", ErrImmutableConflict)
	}
	return nil
}

func runBudgetDigest(snapshot RunBudgetSnapshot) (contract.SemanticDigest, error) {
	encoded, err := json.Marshal(struct {
		Scope     contract.Scope  `json:"scope"`
		RunID     string          `json:"run_id"`
		BudgetRef string          `json:"budget_ref"`
		Revision  uint64          `json:"revision"`
		Policy    RunBudgetPolicy `json:"policy"`
	}{
		Scope:     snapshot.Scope,
		RunID:     snapshot.RunID,
		BudgetRef: snapshot.BudgetRef,
		Revision:  snapshot.Revision,
		Policy:    snapshot.Policy,
	})
	if err != nil {
		return "", fmt.Errorf("encode run budget digest: %w", err)
	}
	digest, err := contract.CanonicalDigest(encoded)
	if err != nil {
		return "", fmt.Errorf("digest run budget snapshot: %w", err)
	}
	return digest, nil
}
