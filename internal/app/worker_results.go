package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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

// resultPublicationCommit commits a prepared result publication in the
// settlement transaction and returns its receipt.
type resultPublicationCommit func(context.Context, *store.Transaction, *objects.PreparedPublication) (store.ArtifactPublicationReceipt, error)

func commitPreparedPublication(ctx context.Context, tx *store.Transaction, publication *objects.PreparedPublication) (store.ArtifactPublicationReceipt, error) {
	return publication.Commit(ctx, tx)
}

// workerResultCommit is the result publication part of one settlement.
type workerResultCommit struct {
	// artifactID is the published result artifact, or empty.
	artifactID string
	// rejected is true when the publication committed a receipt that does not
	// match the unit's result reservation. The settlement discarded that
	// publication: the reservation stays bound and charged, and no result
	// artifact exists. The member outcomes still settle.
	rejected bool
}

func commitWorkerResult(ctx context.Context, tx *store.Transaction, plan workerUnitPlan, publication *objects.PreparedPublication, commit resultPublicationCommit) (workerResultCommit, error) {
	reservationID := plan.resultReservation.Request.ReservationID
	if publication != nil {
		if commit == nil {
			commit = commitPreparedPublication
		}
		var artifactID string
		// The savepoint discards the staged publication of a rejected receipt
		// before the member outcomes commit in the same transaction.
		err := store.WithinSavepoint(ctx, tx, func(ctx context.Context, tx *store.Transaction) error {
			receipt, err := commit(ctx, tx, publication)
			if err != nil {
				return err
			}
			artifactID, err = workerResultReceiptArtifactID(plan, receipt)
			return err
		})
		if errors.Is(err, errWorkerResultReceiptMismatch) {
			return workerResultCommit{rejected: true}, nil
		}
		if err != nil {
			return workerResultCommit{}, err
		}
		return workerResultCommit{artifactID: artifactID}, nil
	}
	if reservationID == "" {
		return workerResultCommit{}, nil
	}
	reservation, err := store.LoadObjectReservation(ctx, tx, reservationID)
	if err != nil {
		return workerResultCommit{}, err
	}
	if reservation.State == store.ObjectReservationHeld {
		return workerResultCommit{}, store.CancelObjectReservation(ctx, tx, reservationID)
	}
	if reservation.State == store.ObjectReservationBound {
		// A failed or uncertain upload remains charged. Never infer absence from
		// the callback error or cancel a bound physical attempt.
		return workerResultCommit{}, nil
	}
	return workerResultCommit{}, errors.New("unsettled worker result reservation has incompatible state")
}

// errWorkerResultReceiptMismatch marks a committed publication receipt that
// does not match the unit's result reservation.
var errWorkerResultReceiptMismatch = fmt.Errorf("worker result receipt does not match its reservation: %w", store.ErrImmutableConflict)

// workerResultReceiptArtifactID returns the result artifact ID of a committed
// publication receipt for the unit's result reservation. A receipt that does
// not match the reservation is a conflict. Its artifact never becomes the
// unit's result.
func workerResultReceiptArtifactID(plan workerUnitPlan, receipt store.ArtifactPublicationReceipt) (string, error) {
	if receipt.ReservationID != plan.resultReservation.Request.ReservationID ||
		receipt.ConnectionID != plan.state.input.ConnectionID ||
		receipt.Purpose != store.ArtifactPurposePublic ||
		receipt.Object.ArtifactID != plan.resultReservation.Request.ArtifactID {
		return "", errWorkerResultReceiptMismatch
	}
	return receipt.Object.ArtifactID, nil
}
