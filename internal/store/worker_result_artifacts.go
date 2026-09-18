package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// WorkerResultArtifactReferenceName is the sole safe-reference name that makes
// an immutable worker result available through a run projection.
const WorkerResultArtifactReferenceName = "result_artifact"

const workerResultSettlementReceiptKind = "worker_settlement"

func validateWorkerResultArtifactReference(ctx context.Context, tx *Transaction, receipt Receipt) error {
	artifactID := ""
	for _, reference := range receipt.Payload.References {
		if reference.Name == WorkerResultArtifactReferenceName {
			artifactID = reference.Ref
			break
		}
	}
	if artifactID == "" {
		return nil
	}
	if receipt.Kind != workerResultSettlementReceiptKind {
		return fmt.Errorf("result artifact reference requires a worker settlement receipt: %w", ErrImmutableConflict)
	}
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	var published bool
	err = tx.queryRow(ctx, `
		SELECT true
		FROM pm.run_input_snapshots AS input
		JOIN pm.artifacts AS artifact
		  ON artifact.org_id = input.org_id
		 AND artifact.workspace_id = input.workspace_id
		 AND artifact.connection_id = input.connection_id
		 AND artifact.artifact_id = $4
		JOIN pm.object_reservations AS reservation
		  ON reservation.org_id = artifact.org_id
		 AND reservation.workspace_id = artifact.workspace_id
		 AND reservation.connection_id = input.connection_id
		 AND reservation.artifact_id = artifact.artifact_id
		 AND reservation.published_artifact_id = artifact.artifact_id
		 AND reservation.object_attempt_key = artifact.object_attempt_key
		 AND reservation.sha256 = artifact.sha256
		 AND reservation.byte_length = artifact.byte_length
		 AND reservation.media_type = artifact.media_type
		 AND reservation.state = 'published'
		WHERE input.org_id = $1 AND input.workspace_id = $2 AND input.run_id = $3
		LIMIT 1
	`, scope.OrgID, scope.WorkspaceID, receipt.RunID, artifactID).Scan(&published)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("result artifact is not an exact published run artifact: %w", ErrImmutableConflict)
	}
	if err != nil {
		return fmt.Errorf("validate worker result artifact publication: %w", err)
	}
	if !published {
		return fmt.Errorf("result artifact publication witness is invalid: %w", ErrImmutableConflict)
	}
	return nil
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
		JOIN pm.object_reservations AS reservation
		  ON reservation.org_id = artifact.org_id
		 AND reservation.workspace_id = artifact.workspace_id
		 AND reservation.connection_id = input.connection_id
		 AND reservation.artifact_id = artifact.artifact_id
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
	`, scope.OrgID, scope.WorkspaceID, runIDs, workerResultSettlementReceiptKind, WorkerResultArtifactReferenceName)
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
