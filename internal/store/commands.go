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

const (
	WorkflowCommandStartRun      = "start_run"
	WorkflowCommandEnsureBinding = "ensure_workflow_binding"
	WorkflowCommandRepair        = "repair_projection"

	maximumWorkflowRouteLimit = uint32(32)
	maximumWorkflowRouteTTL   = 5 * time.Minute
)

type WorkflowRouteRequest struct {
	OwnerID string
	Limit   uint32
	TTL     time.Duration
}

type WorkflowRoute struct {
	Claim OutboxClaim
	Actor contract.Actor
}

type WorkflowCommandPlan struct {
	Namespace        string
	TaskQueue        string
	WorkflowType     string
	WorkflowID       string
	Origin           contract.RunOrigin
	Generation       uint64
	ExecutionTimeout time.Duration
	BudgetDigest     contract.SemanticDigest
}

type WorkflowCommandIntent struct {
	Scope     contract.Scope
	RunID     string
	CommandID string
	Kind      string
	Plan      WorkflowCommandPlan
	Digest    contract.SemanticDigest
	SealedAt  time.Time
}

func (request WorkflowRouteRequest) validate() error {
	if !validStoreID(request.OwnerID) {
		return errors.New("workflow route owner identity is invalid")
	}
	if request.Limit == 0 || request.Limit > maximumWorkflowRouteLimit {
		return errors.New("workflow route limit is invalid")
	}
	if request.TTL <= 0 || request.TTL > maximumWorkflowRouteTTL || request.TTL%time.Microsecond != 0 {
		return errors.New("workflow route TTL is invalid")
	}
	return nil
}

func (route WorkflowRoute) validate() error {
	if err := route.Claim.validate(); err != nil {
		return err
	}
	if err := route.Actor.Validate(); err != nil {
		return err
	}
	if route.Actor.RequestedActor.OrgID != route.Claim.Scope.OrgID ||
		route.Actor.EffectiveActor.OrgID != route.Claim.Scope.OrgID {
		return ErrScopeMismatch
	}
	if route.Claim.Command.PayloadRef != route.Claim.RunID || !workflowCommandKind(route.Claim.Command.Kind) {
		return ErrImmutableConflict
	}
	return nil
}

func (plan WorkflowCommandPlan) validate(kind string) error {
	if !validStoreID(plan.Namespace) || plan.TaskQueue != contract.ControlTaskQueue ||
		!validStoreID(plan.WorkflowType) || !validStoreID(plan.WorkflowID) ||
		plan.Generation == 0 || plan.Generation > maxStoreFence {
		return errors.New("workflow command plan contains an invalid identity or generation")
	}
	if plan.ExecutionTimeout <= 0 {
		return errors.New("workflow command execution timeout is invalid")
	}
	if err := plan.BudgetDigest.Validate(); err != nil {
		return fmt.Errorf("validate workflow command budget digest: %w", err)
	}
	switch kind {
	case WorkflowCommandStartRun:
		if plan.Origin == contract.RunOriginSchedule {
			return errors.New("start workflow command plan is invalid")
		}
	case WorkflowCommandEnsureBinding:
		if plan.Origin != contract.RunOriginSchedule {
			return errors.New("binding-only workflow command plan is invalid")
		}
	case WorkflowCommandRepair:
	default:
		return errors.New("workflow command kind is invalid")
	}
	switch plan.Origin {
	case contract.RunOriginAPI, contract.RunOriginSchedule, contract.RunOriginRecovery:
	default:
		return errors.New("workflow command origin is invalid")
	}
	return nil
}

func (intent WorkflowCommandIntent) validate() error {
	if err := intent.Scope.Validate(); err != nil {
		return err
	}
	if !validStoreID(intent.RunID) || !validStoreID(intent.CommandID) || !workflowCommandKind(intent.Kind) {
		return errors.New("workflow command intent identity is invalid")
	}
	if err := intent.Plan.validate(intent.Kind); err != nil {
		return err
	}
	if err := intent.Digest.Validate(); err != nil {
		return err
	}
	if intent.SealedAt.IsZero() || intent.SealedAt.Location() != time.UTC {
		return errors.New("workflow command intent seal time is invalid")
	}
	digest, err := workflowCommandDigest(intent.Scope, intent.RunID, intent.CommandID, intent.Kind, intent.Plan)
	if err != nil {
		return err
	}
	if digest != intent.Digest {
		return ErrImmutableConflict
	}
	return nil
}

func workflowCommandKind(kind string) bool {
	switch kind {
	case WorkflowCommandStartRun, WorkflowCommandEnsureBinding, WorkflowCommandRepair:
		return true
	default:
		return false
	}
}

// ClaimWorkflowRoutes invokes the sole bounded cross-scope routing operation.
// The returned actor is the immutable accepted actor used to enter the normal
// scoped transaction boundary; it is not a new authorization decision.
func ClaimWorkflowRoutes(ctx context.Context, tx *Transaction, request WorkflowRouteRequest) ([]WorkflowRoute, error) {
	if tx == nil || tx.tx == nil {
		return nil, errors.New("store transaction is required")
	}
	if tx.scoped {
		return nil, errors.New("workflow routes require an unscoped transaction")
	}
	if err := request.validate(); err != nil {
		return nil, err
	}
	rows, err := tx.query(ctx, `
		SELECT org_id, workspace_id, run_id, command_id, kind, payload_ref,
		       owner_id, fence, claimed_at, expires_at, actor
		FROM pm.claim_workflow_outbox(
			$1,
			$2,
			make_interval(secs => $3::double precision / 1000000.0)
		)
	`, request.OwnerID, int32(request.Limit), request.TTL.Microseconds())
	if err != nil {
		return nil, fmt.Errorf("claim workflow routes: %w", err)
	}
	defer rows.Close()

	routes := make([]WorkflowRoute, 0, request.Limit)
	for rows.Next() {
		var route WorkflowRoute
		var actorJSON []byte
		if err := rows.Scan(
			&route.Claim.Scope.OrgID,
			&route.Claim.Scope.WorkspaceID,
			&route.Claim.RunID,
			&route.Claim.Command.CommandID,
			&route.Claim.Command.Kind,
			&route.Claim.Command.PayloadRef,
			&route.Claim.OwnerID,
			&route.Claim.Fence,
			&route.Claim.ClaimedAt,
			&route.Claim.ExpiresAt,
			&actorJSON,
		); err != nil {
			return nil, fmt.Errorf("scan workflow route: %w", err)
		}
		route.Claim.ClaimedAt = route.Claim.ClaimedAt.UTC()
		route.Claim.ExpiresAt = route.Claim.ExpiresAt.UTC()
		if err := json.Unmarshal(actorJSON, &route.Actor); err != nil {
			return nil, fmt.Errorf("decode workflow route actor: %w", err)
		}
		if err := route.validate(); err != nil {
			return nil, fmt.Errorf("validate workflow route: %w", err)
		}
		routes = append(routes, route)
		if uint32(len(routes)) > request.Limit {
			return nil, ErrImmutableConflict
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow routes: %w", err)
	}
	return routes, nil
}

// SealWorkflowCommand persists the exact namespace, queue, workflow type,
// generation, budget and timeout used by one existing outbox command. Exact
// replay returns the prior seal; configuration or payload drift conflicts.
func SealWorkflowCommand(
	ctx context.Context,
	tx *Transaction,
	claim OutboxClaim,
	plan WorkflowCommandPlan,
) (WorkflowCommandIntent, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return WorkflowCommandIntent{}, err
	}
	if err := claim.validate(); err != nil {
		return WorkflowCommandIntent{}, err
	}
	if !scope.Equal(claim.Scope) {
		return WorkflowCommandIntent{}, ErrScopeMismatch
	}
	if err := plan.validate(claim.Command.Kind); err != nil {
		return WorkflowCommandIntent{}, err
	}
	if err := requireCurrentOutboxClaim(ctx, tx, claim); err != nil {
		return WorkflowCommandIntent{}, err
	}

	run, err := loadRunForUpdate(ctx, tx, claim.RunID)
	if err != nil {
		return WorkflowCommandIntent{}, err
	}
	budget, err := LoadRunBudgetSnapshot(ctx, tx, claim.RunID)
	if err != nil {
		return WorkflowCommandIntent{}, err
	}
	binding := run.WorkflowBinding
	if claim.Command.PayloadRef != claim.RunID || binding.RunID != claim.RunID ||
		binding.WorkflowID != plan.WorkflowID || binding.Origin != plan.Origin ||
		binding.Generation != plan.Generation || run.Spec.Origin != plan.Origin ||
		budget.Digest != plan.BudgetDigest || budget.Policy.RunLifetime != plan.ExecutionTimeout {
		return WorkflowCommandIntent{}, ErrImmutableConflict
	}
	digest, err := workflowCommandDigest(scope, claim.RunID, claim.Command.CommandID, claim.Command.Kind, plan)
	if err != nil {
		return WorkflowCommandIntent{}, err
	}
	sealedAt, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return WorkflowCommandIntent{}, err
	}
	_, err = tx.exec(ctx, `
		INSERT INTO pm.workflow_command_intents (
			org_id, workspace_id, run_id, command_id, kind,
			namespace, task_queue, workflow_type, workflow_id,
			origin, generation, execution_timeout_nanoseconds,
			budget_digest, command_digest, sealed_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (org_id, workspace_id, run_id, command_id) DO NOTHING
	`, scope.OrgID, scope.WorkspaceID, claim.RunID, claim.Command.CommandID, claim.Command.Kind,
		plan.Namespace, plan.TaskQueue, plan.WorkflowType, plan.WorkflowID,
		string(plan.Origin), int64(plan.Generation), plan.ExecutionTimeout.Nanoseconds(),
		string(plan.BudgetDigest), string(digest), sealedAt)
	if err != nil {
		return WorkflowCommandIntent{}, safeMetadataWriteError("seal workflow command", err)
	}
	intent, err := loadWorkflowCommandIntent(ctx, tx, claim.RunID, claim.Command.CommandID)
	if err != nil {
		return WorkflowCommandIntent{}, err
	}
	expected := WorkflowCommandIntent{
		Scope: scope, RunID: claim.RunID, CommandID: claim.Command.CommandID,
		Kind: claim.Command.Kind, Plan: plan, Digest: digest,
	}
	if intent.Scope != expected.Scope || intent.RunID != expected.RunID ||
		intent.CommandID != expected.CommandID || intent.Kind != expected.Kind ||
		intent.Plan != expected.Plan || intent.Digest != expected.Digest {
		return WorkflowCommandIntent{}, ErrImmutableConflict
	}
	return intent, nil
}

func loadWorkflowCommandIntent(ctx context.Context, tx *Transaction, runID, commandID string) (WorkflowCommandIntent, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return WorkflowCommandIntent{}, err
	}
	intent := WorkflowCommandIntent{Scope: scope, RunID: runID, CommandID: commandID}
	var origin, budgetDigest, commandDigest string
	var generation, timeoutNanoseconds int64
	err = tx.queryRow(ctx, `
		SELECT kind, namespace, task_queue, workflow_type, workflow_id,
		       origin, generation, execution_timeout_nanoseconds,
		       budget_digest, command_digest, sealed_at
		FROM pm.workflow_command_intents
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND command_id = $4
	`, scope.OrgID, scope.WorkspaceID, runID, commandID).Scan(
		&intent.Kind,
		&intent.Plan.Namespace,
		&intent.Plan.TaskQueue,
		&intent.Plan.WorkflowType,
		&intent.Plan.WorkflowID,
		&origin,
		&generation,
		&timeoutNanoseconds,
		&budgetDigest,
		&commandDigest,
		&intent.SealedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkflowCommandIntent{}, ErrNotFound
	}
	if err != nil {
		return WorkflowCommandIntent{}, fmt.Errorf("load workflow command intent: %w", err)
	}
	if generation <= 0 || timeoutNanoseconds <= 0 {
		return WorkflowCommandIntent{}, ErrImmutableConflict
	}
	intent.Plan.Origin = contract.RunOrigin(origin)
	intent.Plan.Generation = uint64(generation)
	intent.Plan.ExecutionTimeout = time.Duration(timeoutNanoseconds)
	intent.Plan.BudgetDigest = contract.SemanticDigest(budgetDigest)
	intent.Digest = contract.SemanticDigest(commandDigest)
	intent.SealedAt = intent.SealedAt.UTC()
	if err := intent.validate(); err != nil {
		return WorkflowCommandIntent{}, fmt.Errorf("validate workflow command intent: %w", err)
	}
	return intent, nil
}

func requireCurrentOutboxClaim(ctx context.Context, tx *Transaction, claim OutboxClaim) error {
	if err := claim.validate(); err != nil {
		return err
	}
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	if scope != claim.Scope {
		return errors.New("workflow outbox claim scope does not match the transaction")
	}
	var current bool
	err = tx.queryRow(ctx, `
		SELECT true
		FROM pm.outbox_commands
		WHERE org_id = $1 AND workspace_id = $2
		  AND run_id = $3 AND command_id = $4
		  AND kind = $5 AND payload_ref = $6
		  AND state = 'pending' AND acknowledged_at IS NULL
		  AND claim_owner = $7 AND claim_fence = $8
		  AND claim_expires_at > clock_timestamp()
	`, claim.Scope.OrgID, claim.Scope.WorkspaceID, claim.RunID,
		claim.Command.CommandID, claim.Command.Kind, claim.Command.PayloadRef,
		claim.OwnerID, int64(claim.Fence)).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrOutboxClaimLost
	}
	if err != nil {
		return fmt.Errorf("validate workflow outbox claim: %w", err)
	}
	return nil
}

// WorkflowClaimRemaining revalidates the exact scoped route claim against the
// database clock and returns only its conservative microsecond lease remainder.
func WorkflowClaimRemaining(ctx context.Context, tx *Transaction, claim OutboxClaim) (time.Duration, error) {
	if err := claim.validate(); err != nil {
		return 0, err
	}
	scope, err := tx.requireScope()
	if err != nil {
		return 0, err
	}
	if !scope.Equal(claim.Scope) {
		return 0, ErrScopeMismatch
	}
	var remainingMicroseconds int64
	err = tx.queryRow(ctx, `
		SELECT floor(
			extract(epoch FROM (claim_expires_at - clock_timestamp())) * 1000000
		)::bigint
		FROM pm.outbox_commands
		WHERE org_id = $1 AND workspace_id = $2
		  AND run_id = $3 AND command_id = $4
		  AND kind = $5 AND payload_ref = $6
		  AND state = 'pending' AND acknowledged_at IS NULL
		  AND claim_owner = $7 AND claim_fence = $8
		  AND claim_expires_at > clock_timestamp()
	`, claim.Scope.OrgID, claim.Scope.WorkspaceID, claim.RunID,
		claim.Command.CommandID, claim.Command.Kind, claim.Command.PayloadRef,
		claim.OwnerID, int64(claim.Fence)).Scan(&remainingMicroseconds)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && remainingMicroseconds <= 0 {
		return 0, ErrOutboxClaimLost
	}
	if err != nil {
		return 0, fmt.Errorf("measure workflow outbox claim: %w", err)
	}
	return time.Duration(remainingMicroseconds) * time.Microsecond, nil
}

func workflowCommandDigest(
	scope contract.Scope,
	runID string,
	commandID string,
	kind string,
	plan WorkflowCommandPlan,
) (contract.SemanticDigest, error) {
	encoded, err := json.Marshal(struct {
		Scope                       contract.Scope          `json:"scope"`
		RunID                       string                  `json:"run_id"`
		CommandID                   string                  `json:"command_id"`
		Kind                        string                  `json:"kind"`
		Namespace                   string                  `json:"namespace"`
		TaskQueue                   string                  `json:"task_queue"`
		WorkflowType                string                  `json:"workflow_type"`
		WorkflowID                  string                  `json:"workflow_id"`
		Origin                      contract.RunOrigin      `json:"origin"`
		Generation                  uint64                  `json:"generation"`
		ExecutionTimeoutNanoseconds int64                   `json:"execution_timeout_nanoseconds"`
		BudgetDigest                contract.SemanticDigest `json:"budget_digest"`
	}{
		Scope: scope, RunID: runID, CommandID: commandID, Kind: kind,
		Namespace: plan.Namespace, TaskQueue: plan.TaskQueue,
		WorkflowType: plan.WorkflowType, WorkflowID: plan.WorkflowID,
		Origin: plan.Origin, Generation: plan.Generation,
		ExecutionTimeoutNanoseconds: plan.ExecutionTimeout.Nanoseconds(),
		BudgetDigest:                plan.BudgetDigest,
	})
	if err != nil {
		return "", fmt.Errorf("encode workflow command digest: %w", err)
	}
	digest, err := contract.CanonicalDigest(encoded)
	if err != nil {
		return "", fmt.Errorf("digest workflow command: %w", err)
	}
	return digest, nil
}

// DeferWorkflowCommand preserves the same durable command and fence lineage.
// The expired claim remains attributable until the next bounded claimant
// advances the fence.
func DeferWorkflowCommand(ctx context.Context, tx *Transaction, claim OutboxClaim, retryAt time.Time) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	if err := claim.validate(); err != nil {
		return err
	}
	if !scope.Equal(claim.Scope) {
		return ErrScopeMismatch
	}
	if retryAt.IsZero() || retryAt.Location() != time.UTC {
		return errors.New("workflow retry time must be an explicit UTC instant")
	}
	tag, err := tx.exec(ctx, `
		WITH defer_clock AS (SELECT clock_timestamp() AS value)
		UPDATE pm.outbox_commands AS command
		SET available_at = GREATEST($9, defer_clock.value + interval '1 second'),
		    claim_expires_at = defer_clock.value
		FROM defer_clock
		WHERE command.org_id = $1 AND command.workspace_id = $2
		  AND command.run_id = $3 AND command.command_id = $4
		  AND command.kind = $5 AND command.payload_ref = $6
		  AND command.state = 'pending' AND command.acknowledged_at IS NULL
		  AND command.claim_owner = $7 AND command.claim_fence = $8
		  AND command.claim_expires_at > defer_clock.value
	`, claim.Scope.OrgID, claim.Scope.WorkspaceID, claim.RunID,
		claim.Command.CommandID, claim.Command.Kind, claim.Command.PayloadRef,
		claim.OwnerID, int64(claim.Fence), retryAt)
	if err != nil {
		return fmt.Errorf("defer workflow command: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrOutboxClaimLost
	}
	return nil
}
