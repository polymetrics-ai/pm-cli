package app

import (
	"errors"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// This check needs no dependency stack. It gives the receipt comparison of
// commitWorkerResult a constructed receipt, because a real PostgreSQL commit
// returns only the receipt of the reservation that it binds.
func TestWorkerResultReceiptMustMatchReservation(t *testing.T) {
	const (
		reservationID = "worker-result-v1-reserved"
		connectionID  = "connection-reserved"
	)
	plan := workerUnitPlan{
		state: workerRunState{input: store.RunInputSnapshot{ConnectionID: connectionID}},
		resultReservation: store.ObjectReservation{Request: store.ObjectReservationRequest{
			ReservationID: reservationID, ArtifactID: reservationID, ConnectionID: connectionID,
			Purpose: store.ArtifactPurposePublic,
		}},
	}
	matching := store.ArtifactPublicationReceipt{
		ReservationID: reservationID, ConnectionID: connectionID,
		Purpose: store.ArtifactPurposePublic,
		Object:  contract.ObjectRef{ArtifactID: reservationID},
	}

	artifactID, err := workerResultReceiptArtifactID(plan, matching)
	if err != nil || artifactID != reservationID {
		t.Fatalf("matching receipt: artifact=%q error=%v; want %q and no error", artifactID, err, reservationID)
	}

	mismatches := map[string]func(*store.ArtifactPublicationReceipt){
		"reservation ID": func(receipt *store.ArtifactPublicationReceipt) { receipt.ReservationID = "worker-result-v1-other" },
		"connection ID":  func(receipt *store.ArtifactPublicationReceipt) { receipt.ConnectionID = "connection-other" },
		"purpose":        func(receipt *store.ArtifactPublicationReceipt) { receipt.Purpose = store.ArtifactPurposeContinuation },
		"artifact ID":    func(receipt *store.ArtifactPublicationReceipt) { receipt.Object.ArtifactID = "artifact-other" },
	}
	for name, change := range mismatches {
		t.Run(name, func(t *testing.T) {
			receipt := matching
			change(&receipt)
			artifactID, err := workerResultReceiptArtifactID(plan, receipt)
			if !errors.Is(err, store.ErrImmutableConflict) || artifactID != "" {
				t.Fatalf("mismatched %s: artifact=%q error=%v; want no artifact and %v",
					name, artifactID, err, store.ErrImmutableConflict)
			}
		})
	}
}
