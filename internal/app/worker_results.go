package app

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// Capacity joins the lease/attempt preparation transaction, so a refusal cannot
// leave a recorded send or call an execution handler without result storage.
func (service *WorkerService) reserveWorkerResult(ctx context.Context, tx *store.Transaction, plan workerUnitPlan) (store.ObjectReservation, error) {
	if service.artifactPublisher == nil {
		return store.ObjectReservation{}, nil
	}
	identity := struct {
		Scope      contract.Scope      `json:"scope"`
		RunID      string              `json:"run_id"`
		TaskID     string              `json:"task_id"`
		DispatchID contract.DispatchID `json:"dispatch_id"`
		OwnerID    string              `json:"owner_id"`
		Fence      uint64              `json:"fence"`
	}{plan.state.route.Scope, plan.task.RunID, plan.task.TaskID, plan.dispatch.Intent.DispatchID, plan.lease.OwnerID, plan.lease.Fence}
	id, err := workerRecordID("worker-result-v1-", identity)
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
		if receipt.ReservationID != reservationID || receipt.ConnectionID != plan.state.input.ConnectionID ||
			receipt.Purpose != store.ArtifactPurposePublic ||
			receipt.Object.ArtifactID != plan.resultReservation.Request.ArtifactID {
		}
		return receipt.Object.ArtifactID, nil
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
