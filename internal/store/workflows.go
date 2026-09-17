package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

var (
	ErrWorkflowBindingConflict = errors.New("workflow binding conflicts with durable PM intent")
	ErrWorkflowRestartUnsafe   = errors.New("workflow restart is not proven safe")
)

type WorkflowHandoff struct {
	Record       contract.RunRecord
	Budget       RunBudgetSnapshot
	Intent       WorkflowCommandIntent
	IntentSealed bool
	HasDispatch  bool
}

type WorkflowWitnessKind string

const (
	WorkflowWitnessNeverBoundNoDispatch WorkflowWitnessKind = "never_bound_no_dispatch"
	WorkflowWitnessBindingMismatch      WorkflowWitnessKind = "binding_mismatch"
	WorkflowWitnessPriorBoundNotFound   WorkflowWitnessKind = "prior_bound_not_found"
	WorkflowWitnessClosedHistory        WorkflowWitnessKind = "closed_history"
)

type WorkflowWitness struct {
	Scope         contract.Scope
	RunID         string
	CommandID     string
	Generation    uint64
	WorkflowID    string
	TemporalRunID string
	Kind          WorkflowWitnessKind
	RunRevision   uint64
	ClaimFence    uint64
	ObservedAt    time.Time
}

type WorkflowBindingCommit struct {
	Claim               OutboxClaim
	IntentDigest        contract.SemanticDigest
	TemporalRunID       string
	FirstExecutionRunID string
	Condition           contract.OrchestrationCondition
}

type WorkflowBindingCommitResult struct {
	Record         contract.RunRecord
	AcknowledgedAt time.Time
}

type OrchestrationObservationCommit struct {
	Claim               OutboxClaim
	IntentDigest        contract.SemanticDigest
	TemporalRunID       string
	FirstExecutionRunID string
	Condition           contract.OrchestrationCondition
	ObservedAt          time.Time
	WitnessKind         WorkflowWitnessKind
	Acknowledge         bool
}

type OrchestrationObservationCommitResult struct {
	Record          contract.RunRecord
	Witness         WorkflowWitness
	WitnessRecorded bool
	AcknowledgedAt  time.Time
}

func (kind WorkflowWitnessKind) validate() error {
	switch kind {
	case WorkflowWitnessNeverBoundNoDispatch,
		WorkflowWitnessBindingMismatch,
		WorkflowWitnessPriorBoundNotFound,
		WorkflowWitnessClosedHistory:
		return nil
	default:
		return errors.New("workflow witness kind is invalid")
	}
}

func (witness WorkflowWitness) validate() error {
	if err := witness.Scope.Validate(); err != nil {
		return err
	}
	if !validStoreID(witness.RunID) || !validStoreID(witness.CommandID) ||
		!validStoreID(witness.WorkflowID) || witness.Generation == 0 ||
		witness.Generation > maxStoreFence || witness.RunRevision == 0 ||
		witness.RunRevision > maxStoreFence || witness.ClaimFence == 0 ||
		witness.ClaimFence > maxStoreFence {
		return errors.New("workflow witness contains an invalid identity or revision")
	}
	if witness.TemporalRunID != "" && !validStoreID(witness.TemporalRunID) {
		return errors.New("workflow witness Temporal run identity is invalid")
	}
	if err := witness.Kind.validate(); err != nil {
		return err
	}
	if witness.ObservedAt.IsZero() || witness.ObservedAt.Location() != time.UTC {
		return errors.New("workflow witness time is invalid")
	}
	return nil
}

// LoadWorkflowHandoff revalidates an exact current claim and returns only the
// scoped durable facts needed to choose a reconciliation branch.
func LoadWorkflowHandoff(ctx context.Context, tx *Transaction, claim OutboxClaim) (WorkflowHandoff, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return WorkflowHandoff{}, err
	}
	if err := claim.validate(); err != nil {
		return WorkflowHandoff{}, err
	}
	if !scope.Equal(claim.Scope) {
		return WorkflowHandoff{}, ErrScopeMismatch
	}
	if err := requireCurrentOutboxClaim(ctx, tx, claim); err != nil {
		return WorkflowHandoff{}, err
	}
	record, err := loadRunForUpdate(ctx, tx, claim.RunID)
	if err != nil {
		return WorkflowHandoff{}, err
	}
	if err := record.Validate(); err != nil {
		return WorkflowHandoff{}, fmt.Errorf("validate workflow handoff run: %w", err)
	}
	budget, err := LoadRunBudgetSnapshot(ctx, tx, claim.RunID)
	if err != nil {
		return WorkflowHandoff{}, err
	}
	var hasDispatch bool
	if err := tx.queryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pm.dispatches
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
		)
	`, scope.OrgID, scope.WorkspaceID, claim.RunID).Scan(&hasDispatch); err != nil {
		return WorkflowHandoff{}, fmt.Errorf("inspect workflow dispatch evidence: %w", err)
	}
	handoff := WorkflowHandoff{Record: record, Budget: budget, HasDispatch: hasDispatch}
	intent, err := loadWorkflowCommandIntent(ctx, tx, claim.RunID, claim.Command.CommandID)
	if err == nil {
		handoff.Intent = intent
		handoff.IntentSealed = true
	} else if !errors.Is(err, ErrNotFound) {
		return WorkflowHandoff{}, err
	}
	return handoff, nil
}

// PersistNoDispatchWitness records the narrow evidence required before a
// never-bound Temporal NotFound may enter the stable-start branch.
func PersistNoDispatchWitness(
	ctx context.Context,
	tx *Transaction,
	claim OutboxClaim,
	intentDigest contract.SemanticDigest,
) (WorkflowWitness, error) {
	if claim.Command.Kind != WorkflowCommandStartRun {
		return WorkflowWitness{}, ErrWorkflowRestartUnsafe
	}
	if err := requireCurrentOutboxClaim(ctx, tx, claim); err != nil {
		return WorkflowWitness{}, err
	}
	intent, err := requireWorkflowIntent(ctx, tx, claim, intentDigest)
	if err != nil {
		return WorkflowWitness{}, err
	}
	run, err := loadRunForUpdate(ctx, tx, claim.RunID)
	if err != nil {
		return WorkflowWitness{}, err
	}
	if terminalStoreRunState(run.State) || run.WorkflowBinding.Generation != intent.Plan.Generation ||
		run.WorkflowBinding.WorkflowID != intent.Plan.WorkflowID || len(run.WorkflowBinding.Observations) != 0 {
		return WorkflowWitness{}, ErrWorkflowRestartUnsafe
	}
	var hasDispatch bool
	if err := tx.queryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pm.dispatches
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
		)
	`, claim.Scope.OrgID, claim.Scope.WorkspaceID, claim.RunID).Scan(&hasDispatch); err != nil {
		return WorkflowWitness{}, fmt.Errorf("inspect no-dispatch witness: %w", err)
	}
	if hasDispatch {
		return WorkflowWitness{}, ErrWorkflowRestartUnsafe
	}
	return insertWorkflowWitness(ctx, tx, claim, intent, run.Revision, WorkflowWitnessNeverBoundNoDispatch, "")
}

// CommitWorkflowBinding appends only execution-chain observations, leaves the
// business run state and pending reservation untouched, schedules one repair
// command on the existing outbox, and acknowledges the exact handoff claim.
func CommitWorkflowBinding(
	ctx context.Context,
	tx *Transaction,
	commit WorkflowBindingCommit,
) (WorkflowBindingCommitResult, error) {
	if !validStoreID(commit.TemporalRunID) || !validStoreID(commit.FirstExecutionRunID) {
		return WorkflowBindingCommitResult{}, errors.New("Temporal workflow binding identity is invalid")
	}
	if err := requireCurrentOutboxClaim(ctx, tx, commit.Claim); err != nil {
		return WorkflowBindingCommitResult{}, err
	}
	intent, err := requireWorkflowIntent(ctx, tx, commit.Claim, commit.IntentDigest)
	if err != nil {
		return WorkflowBindingCommitResult{}, err
	}
	stored, err := loadRunForUpdate(ctx, tx, commit.Claim.RunID)
	if err != nil {
		return WorkflowBindingCommitResult{}, err
	}
	if err := requireWorkflowBindingCore(stored.WorkflowBinding, intent); err != nil {
		return WorkflowBindingCommitResult{}, err
	}
	observedAt, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return WorkflowBindingCommitResult{}, err
	}
	if observedAt.Before(stored.ProjectionObservedAt) {
		observedAt = stored.ProjectionObservedAt
	}
	updatedBinding, err := bindWorkflowChain(stored.WorkflowBinding, commit.FirstExecutionRunID, commit.TemporalRunID, observedAt)
	if err != nil {
		return WorkflowBindingCommitResult{}, err
	}
	updated := stored
	updated.WorkflowBinding = updatedBinding
	updated.OrchestrationCondition = commit.Condition
	updated.ProjectionObservedAt = observedAt
	updated.Revision = stored.Revision + 1
	if err := updated.Validate(); err != nil {
		return WorkflowBindingCommitResult{}, fmt.Errorf("validate bound workflow run: %w", err)
	}

	repairCommand := OutboxCommand{
		CommandID:  workflowRecordID("workflow-projection", commit.Claim.Scope.OrgID, commit.Claim.Scope.WorkspaceID, commit.Claim.RunID, strconv.FormatUint(intent.Plan.Generation, 10)),
		Kind:       WorkflowCommandRepair,
		PayloadRef: commit.Claim.RunID,
	}
	if err := commitWorkflowRecord(ctx, tx, stored, updated, commit.Claim, "workflow_bound", commit.TemporalRunID, &repairCommand); err != nil {
		return WorkflowBindingCommitResult{}, err
	}
	acknowledgedAt, err := AcknowledgeOutbox(ctx, tx, commit.Claim)
	if err != nil {
		return WorkflowBindingCommitResult{}, err
	}
	return WorkflowBindingCommitResult{Record: updated, AcknowledgedAt: acknowledgedAt}, nil
}

// WorkflowBindingCommitted reconciles an unknown transaction reply against the
// original command fence and the atomic binding/repair artifacts. A false
// result is not rollback evidence; the caller must retain the unknown outcome.
func WorkflowBindingCommitted(
	ctx context.Context,
	tx *Transaction,
	claim OutboxClaim,
	intentDigest contract.SemanticDigest,
	temporalRunID string,
	firstExecutionRunID string,
) (bool, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return false, err
	}
	if err := claim.validate(); err != nil {
		return false, err
	}
	if !scope.Equal(claim.Scope) {
		return false, ErrScopeMismatch
	}
	if !validStoreID(temporalRunID) || !validStoreID(firstExecutionRunID) {
		return false, errors.New("workflow binding reconciliation identity is invalid")
	}
	intent, err := requireWorkflowIntent(ctx, tx, claim, intentDigest)
	if err != nil {
		return false, err
	}
	record, err := LoadRun(ctx, tx, claim.RunID)
	if err != nil {
		return false, err
	}
	if err := requireWorkflowBindingCore(record.WorkflowBinding, intent); err != nil {
		return false, nil
	}
	if len(record.WorkflowBinding.Observations) == 0 ||
		record.WorkflowBinding.Observations[0].TemporalRunID != firstExecutionRunID {
		return false, nil
	}
	foundRun := false
	for _, observation := range record.WorkflowBinding.Observations {
		if observation.TemporalRunID == temporalRunID {
			foundRun = true
			break
		}
	}
	if !foundRun {
		return false, nil
	}
	repairCommandID := workflowRecordID(
		"workflow-projection",
		claim.Scope.OrgID,
		claim.Scope.WorkspaceID,
		claim.RunID,
		strconv.FormatUint(intent.Plan.Generation, 10),
	)
	var committed bool
	err = tx.queryRow(ctx, `
		SELECT
			EXISTS (
				SELECT 1
				FROM pm.outbox_commands
				WHERE org_id = $1 AND workspace_id = $2
				  AND run_id = $3 AND command_id = $4
				  AND kind = $5 AND payload_ref = $3
				  AND claim_fence = $6
				  AND state = 'acknowledged' AND acknowledged_at IS NOT NULL
			)
			AND EXISTS (
				SELECT 1
				FROM pm.outbox_commands
				WHERE org_id = $1 AND workspace_id = $2
				  AND run_id = $3 AND command_id = $7
				  AND kind = $8 AND payload_ref = $3
			)
	`, claim.Scope.OrgID, claim.Scope.WorkspaceID, claim.RunID,
		claim.Command.CommandID, claim.Command.Kind, int64(claim.Fence),
		repairCommandID, WorkflowCommandRepair).Scan(&committed)
	if err != nil {
		return false, fmt.Errorf("reconcile workflow binding commit: %w", err)
	}
	return committed, nil
}

// CommitOrchestrationObservation mutates only the orthogonal orchestration
// projection and append-only binding observations. It cannot manufacture a
// business transition, an effect result, or pending-capacity release.
func CommitOrchestrationObservation(
	ctx context.Context,
	tx *Transaction,
	commit OrchestrationObservationCommit,
) (OrchestrationObservationCommitResult, error) {
	if !validStoreID(commit.TemporalRunID) || !validStoreID(commit.FirstExecutionRunID) {
		return OrchestrationObservationCommitResult{}, errors.New("Temporal orchestration identity is invalid")
	}
	if commit.ObservedAt.IsZero() || commit.ObservedAt.Location() != time.UTC {
		return OrchestrationObservationCommitResult{}, errors.New("orchestration observation time must be UTC")
	}
	if err := requireCurrentOutboxClaim(ctx, tx, commit.Claim); err != nil {
		return OrchestrationObservationCommitResult{}, err
	}
	intent, err := requireWorkflowIntent(ctx, tx, commit.Claim, commit.IntentDigest)
	if err != nil {
		return OrchestrationObservationCommitResult{}, err
	}
	stored, err := loadRunForUpdate(ctx, tx, commit.Claim.RunID)
	if err != nil {
		return OrchestrationObservationCommitResult{}, err
	}
	if commit.ObservedAt.Before(stored.ProjectionObservedAt) {
		return OrchestrationObservationCommitResult{}, ErrWorkflowBindingConflict
	}
	if err := requireWorkflowBindingCore(stored.WorkflowBinding, intent); err != nil {
		return OrchestrationObservationCommitResult{}, err
	}
	updatedBinding, err := bindWorkflowChain(stored.WorkflowBinding, commit.FirstExecutionRunID, commit.TemporalRunID, commit.ObservedAt)
	if err != nil {
		return OrchestrationObservationCommitResult{}, err
	}
	updated := stored
	updated.WorkflowBinding = updatedBinding
	updated.OrchestrationCondition = commit.Condition
	updated.ProjectionObservedAt = commit.ObservedAt
	updated.Revision = stored.Revision + 1
	if err := updated.Validate(); err != nil {
		return OrchestrationObservationCommitResult{}, fmt.Errorf("validate orchestration observation: %w", err)
	}

	result := OrchestrationObservationCommitResult{Record: updated}
	if err := commitWorkflowRecord(ctx, tx, stored, updated, commit.Claim, "orchestration_observed", commit.TemporalRunID, nil); err != nil {
		return OrchestrationObservationCommitResult{}, err
	}
	if commit.WitnessKind != "" {
		witness, err := insertWorkflowWitness(ctx, tx, commit.Claim, intent, updated.Revision, commit.WitnessKind, commit.TemporalRunID)
		if err != nil {
			return OrchestrationObservationCommitResult{}, err
		}
		result.Witness = witness
		result.WitnessRecorded = true
	}
	if commit.Acknowledge {
		acknowledgedAt, err := AcknowledgeOutbox(ctx, tx, commit.Claim)
		if err != nil {
			return OrchestrationObservationCommitResult{}, err
		}
		result.AcknowledgedAt = acknowledgedAt
	}
	return result, nil
}

// MarkWorkflowCommandUnknown records a durable repair witness, moves only the
// orchestration projection to unknown, and removes the exact command from the
// claimable queue without acknowledging an external effect.
func MarkWorkflowCommandUnknown(
	ctx context.Context,
	tx *Transaction,
	claim OutboxClaim,
	intentDigest contract.SemanticDigest,
	kind WorkflowWitnessKind,
	temporalRunID string,
) (WorkflowWitness, error) {
	if temporalRunID != "" && !validStoreID(temporalRunID) {
		return WorkflowWitness{}, errors.New("workflow repair Temporal run identity is invalid")
	}
	if err := kind.validate(); err != nil {
		return WorkflowWitness{}, err
	}
	if err := requireCurrentOutboxClaim(ctx, tx, claim); err != nil {
		return WorkflowWitness{}, err
	}
	intent, err := requireWorkflowIntent(ctx, tx, claim, intentDigest)
	if err != nil {
		return WorkflowWitness{}, err
	}
	stored, err := loadRunForUpdate(ctx, tx, claim.RunID)
	if err != nil {
		return WorkflowWitness{}, err
	}
	if err := requireWorkflowBindingCore(stored.WorkflowBinding, intent); err != nil {
		return WorkflowWitness{}, err
	}
	observedAt, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return WorkflowWitness{}, err
	}
	if observedAt.Before(stored.ProjectionObservedAt) {
		observedAt = stored.ProjectionObservedAt
	}
	updated, err := stored.ObserveOrchestration(contract.OrchestrationUnknown, observedAt)
	if err != nil {
		return WorkflowWitness{}, fmt.Errorf("record unknown orchestration: %w", err)
	}
	if err := commitWorkflowRecord(ctx, tx, stored, updated, claim, "workflow_repair_required", temporalRunID, nil); err != nil {
		return WorkflowWitness{}, err
	}
	witness, err := insertWorkflowWitness(ctx, tx, claim, intent, updated.Revision, kind, temporalRunID)
	if err != nil {
		return WorkflowWitness{}, err
	}
	tag, err := tx.exec(ctx, `
		UPDATE pm.outbox_commands
		SET state = 'unknown', claim_owner = NULL, claim_expires_at = NULL
		WHERE org_id = $1 AND workspace_id = $2
		  AND run_id = $3 AND command_id = $4
		  AND kind = $5 AND payload_ref = $6
		  AND state = 'pending' AND acknowledged_at IS NULL
		  AND claim_owner = $7 AND claim_fence = $8
		  AND claim_expires_at > clock_timestamp()
	`, claim.Scope.OrgID, claim.Scope.WorkspaceID, claim.RunID,
		claim.Command.CommandID, claim.Command.Kind, claim.Command.PayloadRef,
		claim.OwnerID, int64(claim.Fence))
	if err != nil {
		return WorkflowWitness{}, fmt.Errorf("mark workflow command unknown: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return WorkflowWitness{}, ErrOutboxClaimLost
	}
	return witness, nil
}

func requireWorkflowIntent(
	ctx context.Context,
	tx *Transaction,
	claim OutboxClaim,
	digest contract.SemanticDigest,
) (WorkflowCommandIntent, error) {
	if err := digest.Validate(); err != nil {
		return WorkflowCommandIntent{}, err
	}
	intent, err := loadWorkflowCommandIntent(ctx, tx, claim.RunID, claim.Command.CommandID)
	if err != nil {
		return WorkflowCommandIntent{}, err
	}
	if intent.Digest != digest || intent.Scope != claim.Scope || intent.RunID != claim.RunID ||
		intent.CommandID != claim.Command.CommandID || intent.Kind != claim.Command.Kind {
		return WorkflowCommandIntent{}, ErrImmutableConflict
	}
	return intent, nil
}

func requireWorkflowBindingCore(binding contract.WorkflowBinding, intent WorkflowCommandIntent) error {
	if binding.RunID != intent.RunID || binding.WorkflowID != intent.Plan.WorkflowID ||
		binding.Origin != intent.Plan.Origin || binding.Generation != intent.Plan.Generation {
		return ErrWorkflowBindingConflict
	}
	if binding.Origin == contract.RunOriginSchedule != binding.Adopted {
		return ErrWorkflowBindingConflict
	}
	return nil
}

func bindWorkflowChain(
	binding contract.WorkflowBinding,
	firstExecutionRunID string,
	currentRunID string,
	observedAt time.Time,
) (contract.WorkflowBinding, error) {
	if err := binding.Validate(); err != nil {
		return binding, err
	}
	if !validStoreID(firstExecutionRunID) || !validStoreID(currentRunID) {
		return binding, ErrWorkflowBindingConflict
	}
	updated := binding
	if len(updated.Observations) == 0 {
		var err error
		updated, err = updated.ObserveTemporalRun(firstExecutionRunID, observedAt)
		if err != nil {
			return binding, fmt.Errorf("observe first Temporal execution: %w", err)
		}
	} else if updated.Observations[0].TemporalRunID != firstExecutionRunID {
		return binding, ErrWorkflowBindingConflict
	}
	if currentRunID == updated.CurrentTemporalRunID() {
		return updated, nil
	}
	for _, observation := range updated.Observations {
		if observation.TemporalRunID == currentRunID {
			return binding, ErrWorkflowBindingConflict
		}
	}
	var err error
	updated, err = updated.ObserveTemporalRun(currentRunID, observedAt)
	if err != nil {
		return binding, fmt.Errorf("observe current Temporal execution: %w", err)
	}
	return updated, nil
}

func insertWorkflowWitness(
	ctx context.Context,
	tx *Transaction,
	claim OutboxClaim,
	intent WorkflowCommandIntent,
	runRevision uint64,
	kind WorkflowWitnessKind,
	temporalRunID string,
) (WorkflowWitness, error) {
	if err := kind.validate(); err != nil {
		return WorkflowWitness{}, err
	}
	observedAt, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return WorkflowWitness{}, err
	}
	witnessID := workflowRecordID(
		"workflow-witness",
		claim.Scope.OrgID,
		claim.Scope.WorkspaceID,
		claim.RunID,
		claim.Command.CommandID,
		strconv.FormatUint(claim.Fence, 10),
		string(kind),
		temporalRunID,
	)
	witness := WorkflowWitness{
		Scope: claim.Scope, RunID: claim.RunID, CommandID: claim.Command.CommandID,
		Generation: intent.Plan.Generation, WorkflowID: intent.Plan.WorkflowID,
		TemporalRunID: temporalRunID, Kind: kind, RunRevision: runRevision,
		ClaimFence: claim.Fence, ObservedAt: observedAt,
	}
	if err := witness.validate(); err != nil {
		return WorkflowWitness{}, err
	}
	var temporalValue any
	if temporalRunID != "" {
		temporalValue = temporalRunID
	}
	_, err = tx.exec(ctx, `
		INSERT INTO pm.workflow_reconciliation_witnesses (
			org_id, workspace_id, run_id, command_id, witness_id,
			generation, workflow_id, temporal_run_id, kind,
			run_revision, claim_fence, observed_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (org_id, workspace_id, run_id, command_id, witness_id) DO NOTHING
	`, claim.Scope.OrgID, claim.Scope.WorkspaceID, claim.RunID, claim.Command.CommandID,
		witnessID, int64(witness.Generation), witness.WorkflowID, temporalValue,
		string(witness.Kind), int64(witness.RunRevision), int64(witness.ClaimFence), witness.ObservedAt)
	if err != nil {
		return WorkflowWitness{}, safeMetadataWriteError("insert workflow reconciliation witness", err)
	}
	stored, err := loadWorkflowWitness(ctx, tx, claim, witnessID)
	if err != nil {
		return WorkflowWitness{}, err
	}
	witness.ObservedAt = stored.ObservedAt
	if !reflect.DeepEqual(stored, witness) {
		return WorkflowWitness{}, ErrImmutableConflict
	}
	return stored, nil
}

func loadWorkflowWitness(ctx context.Context, tx *Transaction, claim OutboxClaim, witnessID string) (WorkflowWitness, error) {
	witness := WorkflowWitness{Scope: claim.Scope, RunID: claim.RunID, CommandID: claim.Command.CommandID}
	var generation, runRevision, claimFence int64
	var kind string
	var temporalRunID *string
	err := tx.queryRow(ctx, `
		SELECT generation, workflow_id, temporal_run_id, kind,
		       run_revision, claim_fence, observed_at
		FROM pm.workflow_reconciliation_witnesses
		WHERE org_id = $1 AND workspace_id = $2
		  AND run_id = $3 AND command_id = $4 AND witness_id = $5
	`, claim.Scope.OrgID, claim.Scope.WorkspaceID, claim.RunID,
		claim.Command.CommandID, witnessID).Scan(
		&generation, &witness.WorkflowID, &temporalRunID, &kind,
		&runRevision, &claimFence, &witness.ObservedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowWitness{}, ErrNotFound
	}
	if err != nil {
		return WorkflowWitness{}, fmt.Errorf("load workflow reconciliation witness: %w", err)
	}
	if generation <= 0 || runRevision <= 0 || claimFence <= 0 {
		return WorkflowWitness{}, ErrImmutableConflict
	}
	witness.Generation = uint64(generation)
	witness.RunRevision = uint64(runRevision)
	witness.ClaimFence = uint64(claimFence)
	witness.Kind = WorkflowWitnessKind(kind)
	if temporalRunID != nil {
		witness.TemporalRunID = *temporalRunID
	}
	witness.ObservedAt = witness.ObservedAt.UTC()
	if err := witness.validate(); err != nil {
		return WorkflowWitness{}, fmt.Errorf("validate workflow reconciliation witness: %w", err)
	}
	return witness, nil
}

func commitWorkflowRecord(
	ctx context.Context,
	tx *Transaction,
	stored contract.RunRecord,
	updated contract.RunRecord,
	claim OutboxClaim,
	reason string,
	temporalRunID string,
	outbox *OutboxCommand,
) error {
	if err := validateRunProjectionUpdate(stored, updated); err != nil {
		return err
	}
	committedAt, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return err
	}
	recordJSON, err := json.Marshal(updated)
	if err != nil {
		return fmt.Errorf("encode workflow run projection: %w", err)
	}
	tag, err := tx.exec(ctx, `
		UPDATE pm.runs
		SET state = $2, revision = $3, record = $4, updated_at = $5
		WHERE run_id = $1 AND revision = $6
	`, updated.Spec.RunID, string(updated.State), int64(updated.Revision),
		json.RawMessage(recordJSON), committedAt, int64(stored.Revision))
	if err != nil {
		return fmt.Errorf("update workflow run projection: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrCASConflict
	}
	payload := workflowSafePayload(updated.Spec.RunID, updated.WorkflowBinding.WorkflowID, temporalRunID, reason)
	material := []string{
		claim.Scope.OrgID, claim.Scope.WorkspaceID, claim.RunID,
		claim.Command.CommandID, strconv.FormatUint(claim.Fence, 10), reason,
	}
	receipt := Receipt{
		ReceiptID: workflowRecordID("receipt", material...),
		Kind:      reason,
		RunID:     claim.RunID,
		Payload:   payload,
	}
	audit := AuditEntry{
		AuditID:   workflowRecordID("audit", material...),
		Action:    reason,
		TargetRef: claim.RunID,
		Payload:   payload,
	}
	event := EventInput{
		EventID: workflowRecordID("event", material...),
		Type:    reason,
		Payload: payload,
	}
	if err := insertReceipt(ctx, tx, receipt, committedAt); err != nil {
		return err
	}
	if err := insertAuditEntry(ctx, tx, audit, committedAt); err != nil {
		return err
	}
	if outbox != nil {
		if err := InsertOutbox(ctx, tx, updated.Spec.RunID, *outbox); err != nil {
			return err
		}
	}
	if _, err := insertEvent(ctx, tx, updated.Spec.RunID, updated.Revision, event, committedAt); err != nil {
		return err
	}
	return nil
}
func validateWorkflowBindingProgression(stored, proposed contract.WorkflowBinding) error {
	if err := stored.Validate(); err != nil {
		return ErrCASConflict
	}
	if err := proposed.Validate(); err != nil {
		return ErrCASConflict
	}
	if stored.RunID != proposed.RunID ||
		stored.Origin != proposed.Origin ||
		stored.WorkflowID != proposed.WorkflowID ||
		stored.Adopted != proposed.Adopted ||
		stored.Generation != proposed.Generation ||
		len(proposed.Observations) < len(stored.Observations) ||
		!reflect.DeepEqual(proposed.Observations[:len(stored.Observations)], stored.Observations) {
		return ErrCASConflict
	}
	return nil
}

func workflowSafePayload(runID, workflowID, temporalRunID, reason string) contract.SafePayload {
	references := []contract.SafeReference{
		{Name: "run", Ref: runID},
		{Name: "workflow", Ref: workflowID},
	}
	if temporalRunID != "" {
		references = append(references, contract.SafeReference{Name: "temporal_run", Ref: temporalRunID})
	}
	return contract.SafePayload{
		References: references,
		Counts:     []contract.SafeCount{},
		ReasonCodes: []string{
			reason,
		},
	}
}

func workflowRecordID(prefix string, values ...string) string {
	digest := sha256.New()
	for _, value := range values {
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write([]byte(value))
	}
	return prefix + "-" + hex.EncodeToString(digest.Sum(nil))
}
