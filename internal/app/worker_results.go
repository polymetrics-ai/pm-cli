package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// The ID prefixes of the object reservations that one worker attempt makes.
const (
	workerResultReservationPrefix       = "worker-result-v1-"
	workerContinuationReservationPrefix = "worker-continuation-v1-"
)

// workerAttemptReservationID returns the object reservation ID with prefix for
// one attempt of a dispatch. The task lease owner and fence that the attempt
// claimed identify the attempt. Every stored reservation ID depends on these
// fields, their order and their JSON names, so they must not change.
func workerAttemptReservationID(
	prefix string,
	scope contract.Scope,
	runID string,
	taskID string,
	dispatchID contract.DispatchID,
	ownerID string,
	fence uint64,
) (string, error) {
	return workerRecordID(prefix, struct {
		Scope      contract.Scope      `json:"scope"`
		RunID      string              `json:"run_id"`
		TaskID     string              `json:"task_id"`
		DispatchID contract.DispatchID `json:"dispatch_id"`
		OwnerID    string              `json:"owner_id"`
		Fence      uint64              `json:"fence"`
	}{scope, runID, taskID, dispatchID, ownerID, fence})
}

// Capacity joins the lease/attempt preparation transaction, so a refusal cannot
// leave a recorded send or call an execution handler without result storage.
func (service *WorkerService) reserveWorkerResult(ctx context.Context, tx *store.Transaction, plan workerUnitPlan) (store.ObjectReservation, error) {
	if service.artifactPublisher == nil {
		return store.ObjectReservation{}, nil
	}
	id, err := workerAttemptReservationID(
		workerResultReservationPrefix, plan.state.route.Scope, plan.task.RunID, plan.task.TaskID,
		plan.dispatch.Intent.DispatchID, plan.lease.OwnerID, plan.lease.Fence,
	)
	if err != nil {
		return store.ObjectReservation{}, err
	}
	reservation, err := service.artifactPublisher.ReserveInTransaction(ctx, tx, store.ObjectReservationRequest{
		ReservationID: id, ArtifactID: id, ConnectionID: plan.state.input.ConnectionID,
		Purpose: store.ArtifactPurposePublic, MaximumBytes: uint64(service.maximumResultBytes),
	})
	if err != nil {
		return store.ObjectReservation{}, err
	}
	if reservation.State != store.ObjectReservationHeld {
		return store.ObjectReservation{}, store.ErrObjectReservationClosed
	}
	return reservation, nil
}

func (service *WorkerService) prepareWorkerResult(ctx context.Context, plan workerUnitPlan, result *contract.ActionResult) (*objects.PreparedPublication, error) {
	if result == nil {
		return nil, nil
	}
	if service.artifactPublisher == nil || plan.resultReservation.Request.ReservationID == "" {
		return nil, objects.ErrObjectUnavailable
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	attempt, err := objects.NewAttempt(plan.state.route.Scope, plan.resultReservation.Request.ArtifactID, "application/json", payload, service.maximumResultBytes)
	if err != nil {
		clear(payload)
		return nil, err
	}
	defer attempt.Close()
	publication, err := service.artifactPublisher.Upload(ctx, plan.state.route.Actor, plan.state.route.Scope, plan.resultReservation.Request.ReservationID, attempt)
	if err != nil {
		return nil, err
	}
	return &publication, nil
}

func commitWorkerResult(ctx context.Context, tx *store.Transaction, plan workerUnitPlan, publication *objects.PreparedPublication) (string, error) {
	reservationID := plan.resultReservation.Request.ReservationID
	if publication != nil {
		receipt, err := publication.Commit(ctx, tx)
		if err != nil {
			return "", err
		}
		return workerResultReceiptArtifactID(plan, receipt)
	}
	if reservationID == "" {
		return "", nil
	}
	reservation, err := store.LoadObjectReservation(ctx, tx, reservationID)
	if err != nil {
		return "", err
	}
	if reservation.State == store.ObjectReservationHeld {
		return "", store.CancelObjectReservation(ctx, tx, reservationID)
	}
	if reservation.State == store.ObjectReservationBound {
		// A failed or uncertain upload remains charged. Never infer absence from
		// the callback error or cancel a bound physical attempt.
		return "", nil
	}
	return "", errors.New("unsettled worker result reservation has incompatible state")
}

// earlierAttemptReservations counts the object reservations of the earlier
// attempts of a dispatch that one settlement resolved.
type earlierAttemptReservations struct {
	released uint64
	retained uint64
}

// resolveEarlierAttemptReservations resolves the object reservations of the
// earlier attempts of the settling dispatch, in the settlement transaction. If
// the settlement rolls back, this resolution rolls back with it.
//
// Each earlier attempt of the dispatch claimed the task lease with a lower
// fence and, in the same Temporal run, with the same lease owner. So their
// reservation IDs are derived from the fences 1 to the current fence minus 1.
// Each earlier attempt reserved in the transaction that claimed its fence, and
// that claim committed before the current fence could be claimed. So no
// transaction can still create one of these reservations, and a settlement
// with no earlier reservation takes no capacity lock here.
// Planning fences and other dispatches give IDs that match no reservation. An
// attempt under another Temporal run, after an operator reset of the
// workflow, has another owner, and this function does not find its
// reservations.
//
// A held reservation has no bound object key, so no upload started for it and
// no committed manifest can name bytes for it. It is cancelled. A bound
// reservation may have uploaded bytes, so it stays charged and unchanged, and
// the internal settlement audit entry counts it. Object existence is never a
// witness, so nothing here reads the object store. A published reservation
// needs a committed settlement of this dispatch, which a dispatch that still
// settles cannot have, so it is a conflict.
func (service *WorkerService) resolveEarlierAttemptReservations(
	ctx context.Context,
	tx *store.Transaction,
	plan workerUnitPlan,
) (earlierAttemptReservations, error) {
	var resolved earlierAttemptReservations
	if service.artifactPublisher == nil || plan.lease.Fence <= 1 {
		return resolved, nil
	}
	purposes := map[string]store.ArtifactPurpose{}
	var ids []string
	for fence := uint64(1); fence < plan.lease.Fence; fence++ {
		for _, candidate := range []struct {
			prefix  string
			purpose store.ArtifactPurpose
		}{
			{workerResultReservationPrefix, store.ArtifactPurposePublic},
			{workerContinuationReservationPrefix, store.ArtifactPurposeContinuation},
		} {
			id, err := workerAttemptReservationID(
				candidate.prefix, plan.state.route.Scope, plan.task.RunID, plan.task.TaskID,
				plan.dispatch.Intent.DispatchID, plan.lease.OwnerID, fence,
			)
			if err != nil {
				return earlierAttemptReservations{}, err
			}
			purposes[id] = candidate.purpose
			ids = append(ids, id)
		}
	}
	reservations, err := store.LockObjectReservations(ctx, tx, ids)
	if err != nil {
		return earlierAttemptReservations{}, err
	}
	for _, reservation := range reservations {
		id := reservation.Request.ReservationID
		if reservation.Request.ConnectionID != plan.state.input.ConnectionID ||
			reservation.Request.ArtifactID != id || reservation.Request.Purpose != purposes[id] {
			return earlierAttemptReservations{}, fmt.Errorf(
				"earlier attempt object reservation %s conflicts with its derived identity: %w", id, store.ErrImmutableConflict,
			)
		}
		switch reservation.State {
		case store.ObjectReservationHeld:
			if err := store.CancelObjectReservation(ctx, tx, id); err != nil {
				return earlierAttemptReservations{}, err
			}
			resolved.released++
		case store.ObjectReservationBound:
			// A failed, unfinished or uncertain upload stays charged. Never infer
			// absence from an earlier attempt's failure or cancel a bound attempt.
			resolved.retained++
		case store.ObjectReservationReleased:
		default:
			return earlierAttemptReservations{}, fmt.Errorf(
				"earlier attempt object reservation %s is %s without a settlement: %w", id, reservation.State, store.ErrImmutableConflict,
			)
		}
	}
	return resolved, nil
}

// auditPayload returns a copy of payload that also records the resolved
// earlier reservations. Only the internal settlement audit entry carries it.
// The public receipt and event keep payload unchanged.
func (earlier earlierAttemptReservations) auditPayload(payload contract.SafePayload) contract.SafePayload {
	payload.Counts = slices.Clone(payload.Counts)
	payload.ReasonCodes = slices.Clone(payload.ReasonCodes)
	if earlier.released != 0 {
		payload.Counts = append(payload.Counts, contract.SafeCount{
			Name: "earlier_object_reservations_released", Value: earlier.released,
		})
	}
	if earlier.retained != 0 {
		payload.Counts = append(payload.Counts, contract.SafeCount{
			Name: "earlier_object_reservations_retained", Value: earlier.retained,
		})
		payload.ReasonCodes = append(payload.ReasonCodes, "earlier_attempt_objects_retained")
	}
	return payload
}

// workerResultReceiptArtifactID returns the result artifact ID of a committed
// publication receipt for the unit's result reservation. A receipt that does
// not match the reservation is a conflict. Its artifact never becomes the
// unit's result.
func workerResultReceiptArtifactID(plan workerUnitPlan, receipt store.ArtifactPublicationReceipt) (string, error) {
	if receipt.ReservationID != plan.resultReservation.Request.ReservationID ||
		receipt.ConnectionID != plan.state.input.ConnectionID ||
		receipt.Purpose != store.ArtifactPurposePublic ||
		receipt.Object.ArtifactID != plan.resultReservation.Request.ArtifactID {
		return "", store.ErrImmutableConflict
	}
	return receipt.Object.ArtifactID, nil
}
