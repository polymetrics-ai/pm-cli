package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// WorkerResultArtifactReferenceName is the sole safe-reference name that makes
// an immutable public worker result available through a run projection.
const WorkerResultArtifactReferenceName = "result_artifact"

// WorkerContinuationArtifactReferenceName binds one private continuation object
// to the producing run through the same immutable worker settlement receipt as
// its public result.
const WorkerContinuationArtifactReferenceName = "continuation_artifact"

const workerResultSettlementReceiptKind = "worker_settlement"

type WorkerContinuationOrigin struct {
	RunID  string
	Object contract.ObjectRef
}

func validateWorkerResultArtifactReference(ctx context.Context, tx *Transaction, receipt Receipt) error {
	resultArtifactID := ""
	continuationArtifactID := ""
	for _, reference := range receipt.Payload.References {
		switch reference.Name {
		case WorkerResultArtifactReferenceName:
			resultArtifactID = reference.Ref
		case WorkerContinuationArtifactReferenceName:
			continuationArtifactID = reference.Ref
		}
	}
	if resultArtifactID == "" && continuationArtifactID == "" {
		return nil
	}
	if receipt.Kind != workerResultSettlementReceiptKind {
		return fmt.Errorf("worker artifact reference requires a worker settlement receipt: %w", ErrImmutableConflict)
	}
	if resultArtifactID == "" {
		return fmt.Errorf("continuation artifact requires a same-receipt public result: %w", ErrImmutableConflict)
	}
	if err := validatePublishedWorkerArtifact(
		ctx,
		tx,
		receipt.RunID,
		resultArtifactID,
		ArtifactPurposePublic,
		false,
	); err != nil {
		return fmt.Errorf("validate public worker result artifact: %w", err)
	}
	if continuationArtifactID == "" {
		return nil
	}
	if err := validatePublishedWorkerArtifact(
		ctx,
		tx,
		receipt.RunID,
		continuationArtifactID,
		ArtifactPurposeContinuation,
		true,
	); err != nil {
		return fmt.Errorf("validate private worker continuation artifact: %w", err)
	}
	if err := refuseDifferentWorkerContinuationProducer(
		ctx,
		tx,
		receipt.RunID,
		continuationArtifactID,
	); err != nil {
		return err
	}
	return nil
}

func validatePublishedWorkerArtifact(
	ctx context.Context,
	tx *Transaction,
	runID string,
	artifactID string,
	purpose ArtifactPurpose,
	lockPublication bool,
) error {
	_, err := loadPublishedWorkerArtifactPublication(
		ctx,
		tx,
		runID,
		artifactID,
		purpose,
		lockPublication,
	)
	return err
}

func loadPublishedWorkerArtifactPublication(
	ctx context.Context,
	tx *Transaction,
	runID string,
	artifactID string,
	purpose ArtifactPurpose,
	lockPublication bool,
) (ArtifactPublicationReceipt, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return ArtifactPublicationReceipt{}, err
	}
	if !validStoreID(runID) || !validStoreID(artifactID) || !purpose.valid() {
		return ArtifactPublicationReceipt{}, ErrImmutableConflict
	}
	query := `
		SELECT reservation.reservation_id
		FROM pm.run_input_snapshots AS input
		JOIN pm.artifacts AS artifact
		  ON artifact.org_id = input.org_id
		 AND artifact.workspace_id = input.workspace_id
		 AND artifact.connection_id = input.connection_id
		 AND artifact.artifact_id = $4
		 AND artifact.purpose = $5
		JOIN pm.object_reservations AS reservation
		  ON reservation.org_id = artifact.org_id
		 AND reservation.workspace_id = artifact.workspace_id
		 AND reservation.connection_id = input.connection_id
		 AND reservation.artifact_id = artifact.artifact_id
		 AND reservation.purpose = artifact.purpose
		 AND reservation.published_artifact_id = artifact.artifact_id
		 AND reservation.object_attempt_key = artifact.object_attempt_key
		 AND reservation.sha256 = artifact.sha256
		 AND reservation.byte_length = artifact.byte_length
		 AND reservation.media_type = artifact.media_type
		 AND reservation.state = 'published'
		WHERE input.org_id = $1 AND input.workspace_id = $2 AND input.run_id = $3`
	if lockPublication {
		query += ` FOR UPDATE OF reservation`
	}
	var reservationID string
	err = tx.queryRow(
		ctx,
		query,
		scope.OrgID,
		scope.WorkspaceID,
		runID,
		artifactID,
		string(purpose),
	).Scan(&reservationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ArtifactPublicationReceipt{}, fmt.Errorf(
			"artifact is not an exact published run artifact: %w",
			ErrImmutableConflict,
		)
	}
	if err != nil {
		return ArtifactPublicationReceipt{}, fmt.Errorf(
			"validate worker artifact publication: %w",
			err,
		)
	}
	publication, err := LoadArtifactPublication(ctx, tx, reservationID)
	if err != nil {
		return ArtifactPublicationReceipt{}, fmt.Errorf(
			"load worker artifact publication witness: %w",
			err,
		)
	}
	if publication.Purpose != purpose ||
		publication.Object.ArtifactID != artifactID {
		return ArtifactPublicationReceipt{}, fmt.Errorf(
			"worker artifact publication witness is invalid: %w",
			ErrImmutableConflict,
		)
	}
	return publication, nil
}

func refuseDifferentWorkerContinuationProducer(
	ctx context.Context,
	tx *Transaction,
	runID string,
	artifactID string,
) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	var otherRunID string
	err = tx.queryRow(ctx, `
		SELECT receipt.run_id
		FROM pm.receipts AS receipt
		CROSS JOIN LATERAL jsonb_array_elements(
			CASE
				WHEN jsonb_typeof(receipt.payload->'references') = 'array'
				THEN receipt.payload->'references'
				ELSE '[]'::jsonb
			END
		) AS continuation_reference(value)
		WHERE receipt.org_id = $1 AND receipt.workspace_id = $2
		  AND receipt.kind = $3
		  AND receipt.run_id <> $4
		  AND jsonb_typeof(continuation_reference.value) = 'object'
		  AND jsonb_typeof(continuation_reference.value->'name') = 'string'
		  AND jsonb_typeof(continuation_reference.value->'ref') = 'string'
		  AND continuation_reference.value->>'name' = $5
		  AND continuation_reference.value->>'ref' = $6
		LIMIT 1
	`,
		scope.OrgID,
		scope.WorkspaceID,
		workerResultSettlementReceiptKind,
		runID,
		WorkerContinuationArtifactReferenceName,
		artifactID,
	).Scan(&otherRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check worker continuation producer lineage: %w", err)
	}
	return fmt.Errorf(
		"continuation artifact already belongs to producer run %q: %w",
		otherRunID,
		ErrImmutableConflict,
	)
}

// LoadWorkerContinuationOrigin resolves only canonical PostgreSQL receipt
// lineage. Object existence or token contents never establish a producer.
func LoadWorkerContinuationOrigin(
	ctx context.Context,
	tx *Transaction,
	artifactID string,
) (WorkerContinuationOrigin, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return WorkerContinuationOrigin{}, err
	}
	if !validStoreID(artifactID) {
		return WorkerContinuationOrigin{}, ErrNotFound
	}
	rows, err := tx.query(ctx, `
		SELECT DISTINCT receipt.run_id
		FROM pm.receipts AS receipt
		JOIN pm.run_input_snapshots AS input
		  ON input.org_id = receipt.org_id
		 AND input.workspace_id = receipt.workspace_id
		 AND input.run_id = receipt.run_id
		CROSS JOIN LATERAL jsonb_array_elements(
			CASE
				WHEN jsonb_typeof(receipt.payload->'references') = 'array'
				THEN receipt.payload->'references'
				ELSE '[]'::jsonb
			END
		) AS continuation_reference(value)
		CROSS JOIN LATERAL jsonb_array_elements(
			CASE
				WHEN jsonb_typeof(receipt.payload->'references') = 'array'
				THEN receipt.payload->'references'
				ELSE '[]'::jsonb
			END
		) AS result_reference(value)
		JOIN pm.artifacts AS continuation_artifact
		  ON continuation_artifact.org_id = receipt.org_id
		 AND continuation_artifact.workspace_id = receipt.workspace_id
		 AND continuation_artifact.connection_id = input.connection_id
		 AND continuation_artifact.artifact_id = continuation_reference.value->>'ref'
		 AND continuation_artifact.purpose = $7
		JOIN pm.object_reservations AS continuation_reservation
		  ON continuation_reservation.org_id = continuation_artifact.org_id
		 AND continuation_reservation.workspace_id = continuation_artifact.workspace_id
		 AND continuation_reservation.connection_id = input.connection_id
		 AND continuation_reservation.artifact_id = continuation_artifact.artifact_id
		 AND continuation_reservation.purpose = continuation_artifact.purpose
		 AND continuation_reservation.published_artifact_id = continuation_artifact.artifact_id
		 AND continuation_reservation.object_attempt_key = continuation_artifact.object_attempt_key
		 AND continuation_reservation.sha256 = continuation_artifact.sha256
		 AND continuation_reservation.byte_length = continuation_artifact.byte_length
		 AND continuation_reservation.media_type = continuation_artifact.media_type
		 AND continuation_reservation.state = 'published'
		JOIN pm.artifacts AS result_artifact
		  ON result_artifact.org_id = receipt.org_id
		 AND result_artifact.workspace_id = receipt.workspace_id
		 AND result_artifact.connection_id = input.connection_id
		 AND result_artifact.artifact_id = result_reference.value->>'ref'
		 AND result_artifact.purpose = $8
		JOIN pm.object_reservations AS result_reservation
		  ON result_reservation.org_id = result_artifact.org_id
		 AND result_reservation.workspace_id = result_artifact.workspace_id
		 AND result_reservation.connection_id = input.connection_id
		 AND result_reservation.artifact_id = result_artifact.artifact_id
		 AND result_reservation.purpose = result_artifact.purpose
		 AND result_reservation.published_artifact_id = result_artifact.artifact_id
		 AND result_reservation.object_attempt_key = result_artifact.object_attempt_key
		 AND result_reservation.sha256 = result_artifact.sha256
		 AND result_reservation.byte_length = result_artifact.byte_length
		 AND result_reservation.media_type = result_artifact.media_type
		 AND result_reservation.state = 'published'
		WHERE receipt.org_id = $1 AND receipt.workspace_id = $2
		  AND receipt.kind = $3
		  AND jsonb_typeof(continuation_reference.value) = 'object'
		  AND jsonb_typeof(continuation_reference.value->'name') = 'string'
		  AND jsonb_typeof(continuation_reference.value->'ref') = 'string'
		  AND continuation_reference.value->>'name' = $4
		  AND continuation_reference.value->>'ref' = $6
		  AND jsonb_typeof(result_reference.value) = 'object'
		  AND jsonb_typeof(result_reference.value->'name') = 'string'
		  AND jsonb_typeof(result_reference.value->'ref') = 'string'
		  AND result_reference.value->>'name' = $5
		ORDER BY receipt.run_id
		LIMIT 2
	`,
		scope.OrgID,
		scope.WorkspaceID,
		workerResultSettlementReceiptKind,
		WorkerContinuationArtifactReferenceName,
		WorkerResultArtifactReferenceName,
		artifactID,
		string(ArtifactPurposeContinuation),
		string(ArtifactPurposePublic),
	)
	if err != nil {
		return WorkerContinuationOrigin{}, fmt.Errorf("load worker continuation receipt lineage: %w", err)
	}
	defer rows.Close()
	runIDs := make([]string, 0, 2)
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			return WorkerContinuationOrigin{}, fmt.Errorf("scan worker continuation receipt lineage: %w", err)
		}
		if !validStoreID(runID) {
			return WorkerContinuationOrigin{}, ErrImmutableConflict
		}
		runIDs = append(runIDs, runID)
	}
	if err := rows.Err(); err != nil {
		return WorkerContinuationOrigin{}, fmt.Errorf("iterate worker continuation receipt lineage: %w", err)
	}
	if len(runIDs) == 0 {
		return WorkerContinuationOrigin{}, ErrNotFound
	}
	if len(runIDs) != 1 {
		return WorkerContinuationOrigin{}, fmt.Errorf(
			"continuation artifact has ambiguous producer lineage: %w",
			ErrImmutableConflict,
		)
	}
	publication, err := loadPublishedWorkerArtifactPublication(
		ctx,
		tx,
		runIDs[0],
		artifactID,
		ArtifactPurposeContinuation,
		false,
	)
	if err != nil {
		return WorkerContinuationOrigin{}, err
	}
	return WorkerContinuationOrigin{RunID: runIDs[0], Object: publication.Object}, nil
}

func loadWorkerResultArtifactReferences(
	ctx context.Context,
	tx *Transaction,
	runIDs []string,
) (map[string][]string, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return nil, err
	}
	if !validStoreIDList(runIDs) {
		return nil, errors.New("worker result run identities are invalid")
	}
	result := make(map[string][]string, len(runIDs))
	for _, runID := range runIDs {
		result[runID] = []string{}
	}
	if len(runIDs) == 0 {
		return result, nil
	}
	rows, err := tx.query(ctx, `
		SELECT DISTINCT receipt.run_id, result_reference.value->>'ref'
		FROM pm.receipts AS receipt
		JOIN pm.run_input_snapshots AS input
		  ON input.org_id = receipt.org_id
		 AND input.workspace_id = receipt.workspace_id
		 AND input.run_id = receipt.run_id
		CROSS JOIN LATERAL jsonb_array_elements(
			CASE
				WHEN jsonb_typeof(receipt.payload->'references') = 'array'
				THEN receipt.payload->'references'
				ELSE '[]'::jsonb
			END
		) AS result_reference(value)
		JOIN pm.artifacts AS artifact
		  ON artifact.org_id = receipt.org_id
		 AND artifact.workspace_id = receipt.workspace_id
		 AND artifact.connection_id = input.connection_id
		 AND artifact.artifact_id = result_reference.value->>'ref'
		 AND artifact.purpose = $6
		JOIN pm.object_reservations AS reservation
		  ON reservation.org_id = artifact.org_id
		 AND reservation.workspace_id = artifact.workspace_id
		 AND reservation.connection_id = input.connection_id
		 AND reservation.artifact_id = artifact.artifact_id
		 AND reservation.purpose = artifact.purpose
		 AND reservation.published_artifact_id = artifact.artifact_id
		 AND reservation.object_attempt_key = artifact.object_attempt_key
		 AND reservation.sha256 = artifact.sha256
		 AND reservation.byte_length = artifact.byte_length
		 AND reservation.media_type = artifact.media_type
		 AND reservation.state = 'published'
		WHERE receipt.org_id = $1 AND receipt.workspace_id = $2
		  AND receipt.run_id = ANY($3::text[])
		  AND receipt.kind = $4
		  AND jsonb_typeof(result_reference.value) = 'object'
		  AND jsonb_typeof(result_reference.value->'name') = 'string'
		  AND jsonb_typeof(result_reference.value->'ref') = 'string'
		  AND result_reference.value->>'name' = $5
		ORDER BY receipt.run_id, result_reference.value->>'ref'
	`, scope.OrgID, scope.WorkspaceID, runIDs, workerResultSettlementReceiptKind,
		WorkerResultArtifactReferenceName, string(ArtifactPurposePublic))
	if err != nil {
		return nil, fmt.Errorf("list durable worker result artifact references: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var runID, artifactID string
		if err := rows.Scan(&runID, &artifactID); err != nil {
			return nil, fmt.Errorf("scan durable worker result artifact reference: %w", err)
		}
		if _, ok := result[runID]; !ok || !validStoreID(artifactID) {
			return nil, ErrImmutableConflict
		}
		result[runID] = append(result[runID], artifactID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate durable worker result artifact references: %w", err)
	}
	return result, nil
}
