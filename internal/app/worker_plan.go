package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// workerPlanningLeaseTTL bounds the planning lease that serializes dispatch
// planning for one task. The lease carries no send authority, so it is
// deliberately far shorter than any accepted run lifetime: a planner that dies
// after claiming but before committing leaves the run unplanned for at most
// this window, and the next load re-plans from the same durable facts.
const workerPlanningLeaseTTL = time.Second

// planWorkerTaskDispatch makes one accepted operation-reference direct-read task
// runnable through the existing worker unit path by persisting its single
// immutable dispatch intent before any provider I/O.
//
// Planning reads only durable rows: the accepted run and its immutable input
// snapshot, the sealed execution grant, the admitted connection and its current
// provider-account witness, and the sealed HTTP execution policy. The provider
// account witness, execution grant epoch and operation revision must all match
// the accepted run or planning fails closed.
//
// This path never creates send authority. It writes the effect intent, the
// dispatch intent and its ordered member under a bounded planning lease; the
// permit, the physical attempt and the provider call remain exclusive to the
// existing worker unit path (claimWorkerTask, workerAttemptCommit and
// newWorkerPhysicalCallBoundary).
//
// A nil result means the task must not be planned in this call: it is already
// planned, cancelled, terminal, awaiting settlement, holding durable effect
// evidence, or another planner owns the bounded planning lease. Every other
// outcome is an explicit failure; no capability outside the one supported
// bounded direct-read shape is ever planned speculatively.
func (service *WorkerService) planWorkerTaskDispatch(
	ctx context.Context,
	tx *store.Transaction,
	state workerRunState,
	task contract.Task,
) error {
	taskRef := store.TaskRef{RunID: task.RunID, TaskID: task.TaskID}
	switch _, err := store.LoadWorkerDispatch(ctx, tx, taskRef); {
	case err == nil:
		// Exactly one dispatch lineage already owns this logical invocation.
		// Planning never rewrites it, never advances it and never derives a
		// second effect for the same task.
		return nil
	case !errors.Is(err, store.ErrNotFound):
		return err
	}
	if state.record.CancelRequested || task.CancelRequested ||
		workerTerminalState(state.record.State) || workerTerminalState(task.State) ||
		workerTaskRequiresSettlement(task.State) {
		// Cancellation, terminal and settling tasks belong to the existing
		// control and settlement paths. Reporting no plan here preserves those
		// semantics instead of inventing work for them.
		return nil
	}
	reference := state.record.Spec.OperationOrPlanRef
	if reference.Kind != contract.OperationReference {
		return workerFailure(ErrWorkerCapabilityUnavailable, errors.New(
			"dispatch planning is defined only for an accepted operation reference",
		))
	}
	if state.input.Lane != contract.LaneDirectRead && state.input.Lane != contract.LaneDirectWrite {
		return workerFailure(ErrWorkerCapabilityUnavailable, errors.New(
			"dispatch planning supports the direct_read lane and the PostgreSQL direct_write transport",
		))
	}
	if reference.ID != task.OperationID {
		return workerFailure(ErrWorkerConflict, errors.New(
			"operation task differs from the accepted operation",
		))
	}
	if !state.deadline.After(state.now) {
		return workerFailure(ErrWorkerAuthorization, errors.New(
			"accepted run lifetime has expired",
		))
	}
	if err := requireUnplannedRunEffects(ctx, tx, state, task); err != nil {
		return err
	}
	selection, handler, err := service.resolveHandler(ctx, state, task.OperationID, reference.Revision, state.input.Lane)
	if err != nil {
		return err
	}
	if state.input.Lane == contract.LaneDirectWrite &&
		selection.Operation.Transport.Kind != contract.TransportKindPostgreSQL {
		return workerFailure(ErrWorkerCapabilityUnavailable, errors.New(
			"dispatch planning supports the PostgreSQL direct_write transport only",
		))
	}
	responseBytes, err := workerDispatchResponseBytes(state, selection, handler, service.sqlBounds)
	if err != nil {
		return err
	}
	if responseBytes == 0 {
		// The accepted operation declares no bounded execution this planner may
		// derive a dispatch from. Reporting no plan preserves the existing
		// "accepted but not yet dispatchable" path.
		return nil
	}
	connection, err := store.LoadConnection(ctx, tx, state.input.ConnectionID)
	if err != nil {
		return err
	}
	account, err := store.LoadProviderAccount(ctx, tx, connection.ProviderAccountID)
	if err != nil {
		return err
	}
	requestBytes, err := json.Marshal(state.input.Input)
	if err != nil {
		return workerFailure(ErrWorkerConflict, errors.New(
			"encode the exact immutable invocation input",
		))
	}
	dispatch, err := deriveWorkerDirectReadDispatch(state, task, account.AccountWitness, requestBytes, responseBytes)
	if err != nil {
		return err
	}
	lease, err := claimWorkerPlanningLease(ctx, tx, state, taskRef)
	if errors.Is(err, store.ErrClaimLost) {
		// Another planner or worker owns this task right now. Never create a
		// competing effect: the next load observes the one durable dispatch or
		// plans again once the bounded lease lapses.
		return nil
	}
	if err != nil {
		return err
	}
	if err := store.PrepareDispatch(ctx, tx, lease, dispatch); err != nil {
		return err
	}
	// The dispatch is durable, so the planning lease has done its work. Ending
	// it here lets the physical unit claim the same task immediately instead of
	// waiting out a planning lease that grants no authority.
	return store.ReleaseTaskLease(ctx, tx, lease)
}

// requireUnplannedRunEffects refuses to plan over any durable effect evidence.
// It is the durable guard against a second effect for one logical invocation and
// against re-planning work whose previous physical outcome is unknown.
func requireUnplannedRunEffects(
	ctx context.Context,
	tx *store.Transaction,
	state workerRunState,
	task contract.Task,
) error {
	effects, err := store.ListWorkerRunEffects(ctx, tx, state.record.Spec.RunID)
	if err != nil {
		return err
	}
	if len(effects) != 0 ||
		state.record.EffectSummary.Total() != 0 || state.record.UnknownEffectCount != 0 ||
		task.EffectSummary.Total() != 0 || task.UnknownEffectCount != 0 {
		return workerFailure(ErrWorkerConflict, errors.New(
			"durable effect evidence already exists for this logical invocation",
		))
	}
	return nil
}

// workerDispatchResponseBytes resolves the sealed response byte bound of one
// accepted operation. An operation that declares no bounded execution reports
// zero, which keeps the existing "accepted but not yet dispatchable" path. An
// operation whose declared transport has no usable bound in this process is a
// hard capability failure: planning never invents a bound.
func workerDispatchResponseBytes(
	state workerRunState,
	selection OperationSelection,
	handler ResolvedHandler,
	resolveSQLBounds SQLBoundsResolver,
) (uint64, error) {
	switch selection.Operation.Transport.Kind {
	case contract.TransportKindHTTP:
		if selection.Operation.Execution == nil || selection.Operation.Execution.HTTP == nil {
			return 0, nil
		}
		if handler.Kind != HandlerHTTP {
			return 0, workerFailure(ErrWorkerCapabilityUnavailable, errors.New(
				"dispatch planning requires the shared HTTP handler",
			))
		}
		if state.budget.Policy.HTTP == nil {
			return 0, workerFailure(ErrWorkerCapabilityUnavailable, errors.New(
				"accepted run carries no sealed HTTP execution policy",
			))
		}
		return state.budget.Policy.HTTP.MaxResponseBytes, nil
	case contract.TransportKindPostgreSQL:
		if handler.Kind != HandlerSQL {
			return 0, workerFailure(ErrWorkerCapabilityUnavailable, errors.New(
				"dispatch planning requires the shared PostgreSQL handler",
			))
		}
		if resolveSQLBounds == nil {
			return 0, workerFailure(ErrWorkerCapabilityUnavailable, errors.New(
				"this process resolves no declared customer SQL connection",
			))
		}
		bounds, declared := resolveSQLBounds(state.route.Scope, state.input.ConnectionID)
		if !declared {
			return 0, workerFailure(ErrWorkerCapabilityUnavailable, errors.New(
				"no customer SQL connection is declared for this accepted connection",
			))
		}
		return bounds.MaxBytes, nil
	default:
		return 0, workerFailure(ErrWorkerCapabilityUnavailable, errors.New(
			"dispatch planning supports only the HTTP and PostgreSQL transports",
		))
	}
}

// deriveWorkerDirectReadDispatch derives the exact dispatch intent for one
// bounded call from the accepted durable facts. Identity is a pure function of
// those facts, so repeated or concurrent planning converges on one dispatch,
// one effect and one member. responseBytes is the sealed response byte bound of
// the declared transport and is reserved before any provider I/O.
func deriveWorkerDirectReadDispatch(
	state workerRunState,
	task contract.Task,
	accountWitness string,
	requestBytes []byte,
	responseBytes uint64,
) (contract.Dispatch, error) {
	entity, err := contract.NewEntityKey(accountWitness, "operation", task.OperationID, task.BehaviorID)
	if err != nil {
		return contract.Dispatch{}, workerFailure(ErrWorkerConflict, err)
	}
	effect, err := contract.NewEffectKey(entity, task.TaskID, task.BehaviorID)
	if err != nil {
		return contract.Dispatch{}, workerFailure(ErrWorkerConflict, err)
	}
	identity := struct {
		Schema         string         `json:"schema"`
		Scope          contract.Scope `json:"scope"`
		RunID          string         `json:"run_id"`
		TaskID         string         `json:"task_id"`
		BehaviorID     string         `json:"behavior_id"`
		OperationID    string         `json:"operation_id"`
		Revision       string         `json:"operation_revision"`
		AccountWitness string         `json:"account_witness"`
		GrantEpoch     uint64         `json:"grant_epoch"`
	}{
		Schema:         "pm.operation-invocation-dispatch/v1",
		Scope:          state.route.Scope,
		RunID:          state.record.Spec.RunID,
		TaskID:         task.TaskID,
		BehaviorID:     task.BehaviorID,
		OperationID:    task.OperationID,
		Revision:       state.record.Spec.OperationOrPlanRef.Revision,
		AccountWitness: accountWitness,
		GrantEpoch:     state.grant.Grant.GrantEpoch,
	}
	dispatchID, err := workerRecordID("dispatch-v1-", identity)
	if err != nil {
		return contract.Dispatch{}, err
	}
	memberID, err := workerRecordID("member-v1-", identity)
	if err != nil {
		return contract.Dispatch{}, err
	}
	attemptID, err := workerRecordID("attempt-v1-", identity)
	if err != nil {
		return contract.Dispatch{}, err
	}
	dispatch := contract.Dispatch{
		RunID:            state.record.Spec.RunID,
		EffectKey:        effect,
		DispatchID:       contract.DispatchID(dispatchID),
		OrderedMemberIDs: []contract.MemberID{contract.MemberID(memberID)},
		PayloadHash:      contract.NewPayloadHash(requestBytes),
		// The immutable request bytes stay in the admitted run input snapshot;
		// the dispatch only retains their exact reference and hash.
		RequestArtifactRef: state.input.InputRef,
		OperationRevision:  state.record.Spec.OperationOrPlanRef.Revision,
		AccountWitness:     accountWitness,
		GrantEpoch:         state.grant.Grant.GrantEpoch,
		CapacityReservation: contract.CapacityReservation{
			OutcomeSlots:  1,
			ReadbackSlots: 1,
			// Reserve the exact accepted request bytes plus the sealed response
			// bound before any provider I/O.
			EncodedBytes: uint64(len(requestBytes)) + responseBytes,
		},
		AttemptID: contract.AttemptID(attemptID),
	}
	if err := dispatch.Validate(); err != nil {
		return contract.Dispatch{}, workerFailure(ErrWorkerConflict, err)
	}
	return dispatch, nil
}

// claimWorkerPlanningLease takes the bounded, non-authorizing lease that
// serializes planning for one task. It is never a send lease: the physical unit
// claims the same task again through claimWorkerTask and binds its own permit.
func claimWorkerPlanningLease(
	ctx context.Context,
	tx *store.Transaction,
	state workerRunState,
	ref store.TaskRef,
) (store.Lease, error) {
	ownerID, err := workerRecordID("plan-owner-v1-", struct {
		Scope  contract.Scope `json:"scope"`
		RunID  string         `json:"run_id"`
		TaskID string         `json:"task_id"`
	}{state.route.Scope, ref.RunID, ref.TaskID})
	if err != nil {
		return store.Lease{}, err
	}
	ttl := minWorkerDuration(workerPlanningLeaseTTL, state.deadline.Sub(state.now))
	if ttl < time.Microsecond {
		return store.Lease{}, workerFailure(ErrWorkerAuthorization, errors.New(
			"no accepted lifetime remains for dispatch planning",
		))
	}
	expectedFence := uint64(0)
	current, err := store.LoadLease(ctx, tx, ref)
	if err == nil {
		if current.ExpiresAt.After(state.now) {
			return store.Lease{}, store.ErrClaimLost
		}
		expectedFence = current.Fence
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.Lease{}, err
	}
	return store.ClaimTask(ctx, tx, store.ClaimRequest{
		Task: ref, OwnerID: ownerID, ExpectedFence: expectedFence, TTL: ttl,
	})
}
