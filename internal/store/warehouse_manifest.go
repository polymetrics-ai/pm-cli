package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// Warehouse datasets bind a dataset identity to the connector and connection
// that own it. Every later registration is derived from the committed manifest
// rather than from caller input: a ledger row can only exist for an artifact the
// manifest compare-and-swap already registered in the same transaction.
type WarehouseDataset struct {
	Scope        contract.Scope
	DatasetID    string
	ConnectorID  string
	ConnectionID string
	CreatedAt    time.Time
}

// WarehouseSegmentState is the lifecycle of one registered segment. A collected
// segment is no longer served; a retired segment is superseded but retained.
type WarehouseSegmentState string

const (
	WarehouseSegmentLive      WarehouseSegmentState = "live"
	WarehouseSegmentRetired   WarehouseSegmentState = "retired"
	WarehouseSegmentCollected WarehouseSegmentState = "collected"
)

func (state WarehouseSegmentState) valid() bool {
	switch state {
	case WarehouseSegmentLive, WarehouseSegmentRetired, WarehouseSegmentCollected:
		return true
	default:
		return false
	}
}

// WarehouseSegment is one registered immutable Parquet segment. The ordinal is
// the manifest revision that registered it.
type WarehouseSegment struct {
	Scope             contract.Scope
	DatasetID         string
	Ordinal           uint64
	ManifestRevision  uint64
	ArtifactID        string
	Object            contract.ObjectRef
	RowCount          int64
	ColumnFingerprint string
	Schema            []byte
	State             WarehouseSegmentState
	PublishedAt       time.Time
	RetiredAt         *time.Time
	CollectedAt       *time.Time
}

// WarehouseCursor is the dataset's acknowledged position: the highest ordinal
// whose registered rows this dataset has committed to serve.
type WarehouseCursor struct {
	Scope            contract.Scope
	DatasetID        string
	Ordinal          uint64
	ManifestRevision uint64
	AdvancedAt       time.Time
}

// WarehouseRetentionKind names the durable holder that keeps one registered
// segment alive. Retention reachability covers query snapshots, plans, proofs,
// backups and reservations.
type WarehouseRetentionKind string

const (
	WarehouseRetentionSnapshot    WarehouseRetentionKind = "snapshot"
	WarehouseRetentionPlan        WarehouseRetentionKind = "plan"
	WarehouseRetentionProof       WarehouseRetentionKind = "proof"
	WarehouseRetentionBackup      WarehouseRetentionKind = "backup"
	WarehouseRetentionReservation WarehouseRetentionKind = "reservation"
)

// Valid reports whether the kind belongs to the published retention vocabulary.
func (kind WarehouseRetentionKind) Valid() bool {
	switch kind {
	case WarehouseRetentionSnapshot, WarehouseRetentionPlan, WarehouseRetentionProof,
		WarehouseRetentionBackup, WarehouseRetentionReservation:
		return true
	default:
		return false
	}
}

// WarehouseRetention names one holder of one registered segment.
type WarehouseRetention struct {
	Scope     contract.Scope
	DatasetID string
	Ordinal   uint64
	Kind      WarehouseRetentionKind
	HolderID  string
	CreatedAt time.Time
}

// WarehouseSnapshot is one retained exact segment set at a manifest revision.
type WarehouseSnapshot struct {
	Scope            contract.Scope
	SnapshotID       string
	DatasetID        string
	ManifestRevision uint64
	Segments         []uint64
	CreatedAt        time.Time
}

// WarehouseRetainedSegment reports one superseded segment that a holder keeps.
type WarehouseRetainedSegment struct {
	Ordinal uint64
	Holders []WarehouseRetentionKind
}

// WarehouseRetirement is the outcome of superseding a set of segments: the
// unreferenced ones were collected, the referenced ones stay readable.
type WarehouseRetirement struct {
	Collected []uint64
	Retained  []WarehouseRetainedSegment
}

// RegisterWarehouseDataset registers one dataset identity under the connection
// that owns it. Exact replay returns the stored identity.
func RegisterWarehouseDataset(ctx context.Context, tx *Transaction, dataset WarehouseDataset) (WarehouseDataset, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return WarehouseDataset{}, err
	}
	if dataset.Scope != scope {
		return WarehouseDataset{}, ErrScopeMismatch
	}
	if !validStoreID(dataset.DatasetID) || !validStoreID(dataset.ConnectorID) || !validStoreID(dataset.ConnectionID) {
		return WarehouseDataset{}, errors.New("warehouse dataset identity is invalid")
	}
	// created_at is the column default, so the runtime never writes it and an
	// exact replay cannot rewrite the registration time.
	stored, err := scanWarehouseDataset(tx.queryRow(ctx, `
		INSERT INTO pm.warehouse_datasets (
			org_id, workspace_id, dataset_id, connector_id, connection_id
		) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (org_id, workspace_id, dataset_id) DO NOTHING
		RETURNING connector_id, connection_id, created_at
	`, scope.OrgID, scope.WorkspaceID, dataset.DatasetID, dataset.ConnectorID, dataset.ConnectionID), dataset.DatasetID)
	if err == nil {
		stored.Scope = scope
		return stored, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return WarehouseDataset{}, fmt.Errorf("register warehouse dataset: %w", err)
	}
	retained, err := LoadWarehouseDataset(ctx, tx, dataset.DatasetID)
	if err != nil {
		return WarehouseDataset{}, err
	}
	if retained.ConnectorID != dataset.ConnectorID || retained.ConnectionID != dataset.ConnectionID {
		return WarehouseDataset{}, ErrImmutableConflict
	}
	return retained, nil
}

// LoadWarehouseDataset loads one dataset identity inside the transaction's scope.
func LoadWarehouseDataset(ctx context.Context, tx *Transaction, datasetID string) (WarehouseDataset, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return WarehouseDataset{}, err
	}
	if !validStoreID(datasetID) {
		return WarehouseDataset{}, errors.New("warehouse dataset identity is invalid")
	}
	stored, err := scanWarehouseDataset(tx.queryRow(ctx, `
		SELECT connector_id, connection_id, created_at
		FROM pm.warehouse_datasets
		WHERE org_id = $1 AND workspace_id = $2 AND dataset_id = $3
	`, scope.OrgID, scope.WorkspaceID, datasetID), datasetID)
	if err != nil {
		return WarehouseDataset{}, err
	}
	stored.Scope = scope
	return stored, nil
}

func scanWarehouseDataset(row pgx.Row, datasetID string) (WarehouseDataset, error) {
	dataset := WarehouseDataset{DatasetID: datasetID}
	if err := row.Scan(&dataset.ConnectorID, &dataset.ConnectionID, &dataset.CreatedAt); errors.Is(err, pgx.ErrNoRows) {
		return WarehouseDataset{}, ErrNotFound
	} else if err != nil {
		return WarehouseDataset{}, warehouseWriteError("load warehouse dataset", err)
	}
	dataset.CreatedAt = dataset.CreatedAt.UTC()
	return dataset, nil
}

// RegisterWarehouseSegment binds the manifest revision this transaction just
// advanced to the exact registered artifact, the declared row count and the
// canonical schema. The row is derived from the committed manifest, so a ledger
// row can never name bytes that were not verified and registered, and a
// publication can never skip an ordinal.
func RegisterWarehouseSegment(
	ctx context.Context,
	tx *Transaction,
	datasetID string,
	rowCount int64,
	columnFingerprint string,
	schema []byte,
) (WarehouseSegment, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return WarehouseSegment{}, err
	}
	if rowCount < 0 || !validColumnFingerprint(columnFingerprint) || len(schema) == 0 || !jsonObjectDocument(schema) {
		return WarehouseSegment{}, errors.New("warehouse segment registration is invalid")
	}
	if _, err := LoadWarehouseDataset(ctx, tx, datasetID); err != nil {
		return WarehouseSegment{}, err
	}
	manifest, err := LoadManifest(ctx, tx, datasetID)
	if err != nil {
		return WarehouseSegment{}, err
	}
	var previous uint64
	if err := tx.queryRow(ctx, `
		SELECT COALESCE(max(segment_ordinal), 0)
		FROM pm.warehouse_segments
		WHERE org_id = $1 AND workspace_id = $2 AND dataset_id = $3
	`, scope.OrgID, scope.WorkspaceID, datasetID).Scan(&previous); err != nil {
		return WarehouseSegment{}, fmt.Errorf("read the registered warehouse ordinal: %w", err)
	}
	if manifest.Revision != previous+1 || manifest.Revision == 0 {
		return WarehouseSegment{}, ErrImmutableConflict
	}
	publishedAt, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return WarehouseSegment{}, err
	}
	segment := WarehouseSegment{
		Scope:             scope,
		DatasetID:         datasetID,
		Ordinal:           manifest.Revision,
		ManifestRevision:  manifest.Revision,
		ArtifactID:        manifest.Object.ArtifactID,
		Object:            manifest.Object,
		RowCount:          rowCount,
		ColumnFingerprint: columnFingerprint,
		Schema:            append([]byte(nil), schema...),
		State:             WarehouseSegmentLive,
		PublishedAt:       publishedAt,
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.warehouse_segments (
			org_id, workspace_id, dataset_id, segment_ordinal, manifest_revision,
			artifact_id, object_attempt_key, row_count, column_fingerprint,
			schema_document, state, published_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'live', $11)
	`,
		scope.OrgID, scope.WorkspaceID, datasetID, int64(segment.Ordinal), int64(segment.ManifestRevision),
		segment.ArtifactID, segment.Object.ObjectAttemptKey, segment.RowCount, segment.ColumnFingerprint,
		json.RawMessage(schema), publishedAt,
	); err != nil {
		return WarehouseSegment{}, warehouseWriteError("register warehouse segment", err)
	}
	return segment, nil
}

// LoadWarehouseSegments loads the complete registered ledger of one dataset in
// ordinal order, including segments that were superseded.
func LoadWarehouseSegments(ctx context.Context, tx *Transaction, datasetID string) ([]WarehouseSegment, error) {
	return loadWarehouseSegments(ctx, tx, datasetID, "")
}

// LoadWarehouseLiveSegments loads the segments that currently serve one dataset.
func LoadWarehouseLiveSegments(ctx context.Context, tx *Transaction, datasetID string) ([]WarehouseSegment, error) {
	return loadWarehouseSegments(ctx, tx, datasetID, string(WarehouseSegmentLive))
}

func loadWarehouseSegments(ctx context.Context, tx *Transaction, datasetID string, state string) ([]WarehouseSegment, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return nil, err
	}
	if !validStoreID(datasetID) {
		return nil, errors.New("warehouse dataset identity is invalid")
	}
	rows, err := tx.query(ctx, `
		SELECT segment.segment_ordinal, segment.manifest_revision, segment.artifact_id,
		       segment.row_count, segment.column_fingerprint, segment.schema_document,
		       segment.state, segment.published_at, segment.retired_at, segment.collected_at,
		       artifact.object_ref
		FROM pm.warehouse_segments AS segment
		JOIN pm.artifacts AS artifact
		  ON artifact.org_id = segment.org_id
		 AND artifact.workspace_id = segment.workspace_id
		 AND artifact.artifact_id = segment.artifact_id
		 AND artifact.object_attempt_key = segment.object_attempt_key
		WHERE segment.org_id = $1 AND segment.workspace_id = $2 AND segment.dataset_id = $3
		  AND ($4 = '' OR segment.state = $4)
		ORDER BY segment.segment_ordinal
	`, scope.OrgID, scope.WorkspaceID, datasetID, state)
	if err != nil {
		return nil, fmt.Errorf("load registered warehouse segments: %w", err)
	}
	defer rows.Close()
	segments := make([]WarehouseSegment, 0, 8)
	for rows.Next() {
		segment, err := scanWarehouseSegment(rows, scope, datasetID)
		if err != nil {
			return nil, err
		}
		segments = append(segments, segment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate registered warehouse segments: %w", err)
	}
	return segments, nil
}

// LoadWarehouseSegmentAt loads one registered ordinal.
func LoadWarehouseSegmentAt(ctx context.Context, tx *Transaction, datasetID string, ordinal uint64) (WarehouseSegment, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return WarehouseSegment{}, err
	}
	if !validStoreID(datasetID) || ordinal == 0 {
		return WarehouseSegment{}, errors.New("warehouse segment identity is invalid")
	}
	rows, err := tx.query(ctx, `
		SELECT segment.segment_ordinal, segment.manifest_revision, segment.artifact_id,
		       segment.row_count, segment.column_fingerprint, segment.schema_document,
		       segment.state, segment.published_at, segment.retired_at, segment.collected_at,
		       artifact.object_ref
		FROM pm.warehouse_segments AS segment
		JOIN pm.artifacts AS artifact
		  ON artifact.org_id = segment.org_id
		 AND artifact.workspace_id = segment.workspace_id
		 AND artifact.artifact_id = segment.artifact_id
		 AND artifact.object_attempt_key = segment.object_attempt_key
		WHERE segment.org_id = $1 AND segment.workspace_id = $2 AND segment.dataset_id = $3
		  AND segment.segment_ordinal = $4
	`, scope.OrgID, scope.WorkspaceID, datasetID, int64(ordinal))
	if err != nil {
		return WarehouseSegment{}, fmt.Errorf("load registered warehouse segment: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return WarehouseSegment{}, fmt.Errorf("iterate registered warehouse segment: %w", err)
		}
		return WarehouseSegment{}, ErrNotFound
	}
	return scanWarehouseSegment(rows, scope, datasetID)
}

// LoadWarehouseSegmentByArtifact resolves one artifact to its registered segment
// so an exact publication replay never becomes a second copy of the same rows.
func LoadWarehouseSegmentByArtifact(ctx context.Context, tx *Transaction, artifactID string) (WarehouseSegment, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return WarehouseSegment{}, err
	}
	if !validStoreID(artifactID) {
		return WarehouseSegment{}, errors.New("warehouse artifact identity is invalid")
	}
	rows, err := tx.query(ctx, `
		SELECT segment.dataset_id, segment.segment_ordinal, segment.manifest_revision, segment.artifact_id,
		       segment.row_count, segment.column_fingerprint, segment.schema_document,
		       segment.state, segment.published_at, segment.retired_at, segment.collected_at,
		       artifact.object_ref
		FROM pm.warehouse_segments AS segment
		JOIN pm.artifacts AS artifact
		  ON artifact.org_id = segment.org_id
		 AND artifact.workspace_id = segment.workspace_id
		 AND artifact.artifact_id = segment.artifact_id
		 AND artifact.object_attempt_key = segment.object_attempt_key
		WHERE segment.org_id = $1 AND segment.workspace_id = $2 AND segment.artifact_id = $3
	`, scope.OrgID, scope.WorkspaceID, artifactID)
	if err != nil {
		return WarehouseSegment{}, fmt.Errorf("load warehouse segment by artifact: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return WarehouseSegment{}, fmt.Errorf("iterate warehouse segment by artifact: %w", err)
		}
		return WarehouseSegment{}, ErrNotFound
	}
	segment, err := scanWarehouseSegmentWithDataset(rows, scope)
	if err != nil {
		return WarehouseSegment{}, err
	}
	return segment, nil
}

type warehouseSegmentScanner interface {
	Scan(destinations ...any) error
}

func scanWarehouseSegment(rows warehouseSegmentScanner, scope contract.Scope, datasetID string) (WarehouseSegment, error) {
	segment := WarehouseSegment{Scope: scope, DatasetID: datasetID}
	var encoded []byte
	var ordinal, revision int64
	if err := rows.Scan(&ordinal, &revision, &segment.ArtifactID, &segment.RowCount,
		&segment.ColumnFingerprint, &segment.Schema, &segment.State, &segment.PublishedAt,
		&segment.RetiredAt, &segment.CollectedAt, &encoded); err != nil {
		return WarehouseSegment{}, fmt.Errorf("scan warehouse segment: %w", err)
	}
	if ordinal <= 0 || revision != ordinal {
		return WarehouseSegment{}, ErrImmutableConflict
	}
	segment.Ordinal = uint64(ordinal)
	segment.ManifestRevision = uint64(revision)
	return decodeWarehouseSegmentObject(segment, encoded)
}

func scanWarehouseSegmentWithDataset(rows warehouseSegmentScanner, scope contract.Scope) (WarehouseSegment, error) {
	var datasetID string
	segment := WarehouseSegment{Scope: scope}
	var encoded []byte
	var ordinal, revision int64
	if err := rows.Scan(&datasetID, &ordinal, &revision, &segment.ArtifactID, &segment.RowCount,
		&segment.ColumnFingerprint, &segment.Schema, &segment.State, &segment.PublishedAt,
		&segment.RetiredAt, &segment.CollectedAt, &encoded); err != nil {
		return WarehouseSegment{}, fmt.Errorf("scan warehouse segment: %w", err)
	}
	if ordinal <= 0 || revision != ordinal {
		return WarehouseSegment{}, ErrImmutableConflict
	}
	segment.DatasetID = datasetID
	segment.Ordinal = uint64(ordinal)
	segment.ManifestRevision = uint64(revision)
	return decodeWarehouseSegmentObject(segment, encoded)
}

func decodeWarehouseSegmentObject(segment WarehouseSegment, encoded []byte) (WarehouseSegment, error) {
	if err := json.Unmarshal(encoded, &segment.Object); err != nil {
		return WarehouseSegment{}, fmt.Errorf("decode the registered segment object reference: %w", err)
	}
	if segment.Object.OrgID != segment.Scope.OrgID || segment.Object.WorkspaceID != segment.Scope.WorkspaceID {
		return WarehouseSegment{}, ErrScopeMismatch
	}
	segment.PublishedAt = segment.PublishedAt.UTC()
	if segment.RetiredAt != nil {
		value := segment.RetiredAt.UTC()
		segment.RetiredAt = &value
	}
	if segment.CollectedAt != nil {
		value := segment.CollectedAt.UTC()
		segment.CollectedAt = &value
	}
	return segment, nil
}

// LoadWarehouseCursor loads one dataset's acknowledged position.
func LoadWarehouseCursor(ctx context.Context, tx *Transaction, datasetID string) (WarehouseCursor, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return WarehouseCursor{}, err
	}
	if !validStoreID(datasetID) {
		return WarehouseCursor{}, errors.New("warehouse dataset identity is invalid")
	}
	var ordinal, revision int64
	cursor := WarehouseCursor{Scope: scope, DatasetID: datasetID}
	if err := tx.queryRow(ctx, `
		SELECT cursor_ordinal, manifest_revision, advanced_at
		FROM pm.warehouse_dataset_cursors
		WHERE org_id = $1 AND workspace_id = $2 AND dataset_id = $3
	`, scope.OrgID, scope.WorkspaceID, datasetID).Scan(&ordinal, &revision, &cursor.AdvancedAt); errors.Is(err, pgx.ErrNoRows) {
		return WarehouseCursor{}, ErrNotFound
	} else if err != nil {
		return WarehouseCursor{}, fmt.Errorf("load warehouse dataset cursor: %w", err)
	}
	cursor.Ordinal = uint64(ordinal)
	cursor.ManifestRevision = uint64(revision)
	cursor.AdvancedAt = cursor.AdvancedAt.UTC()
	return cursor, nil
}

// AdvanceWarehouseCursor advances one acknowledged position by exactly one
// registered ordinal. The next position must be adjacent to the expected one,
// and it must already name a registered segment, so a cursor can never skip a
// revision or run ahead of the rows that were committed.
func AdvanceWarehouseCursor(
	ctx context.Context,
	tx *Transaction,
	datasetID string,
	expectedOrdinal uint64,
	nextOrdinal uint64,
) (WarehouseCursor, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return WarehouseCursor{}, err
	}
	if !validStoreID(datasetID) || nextOrdinal != expectedOrdinal+1 {
		return WarehouseCursor{}, ErrImmutableConflict
	}
	segment, err := LoadWarehouseSegmentAt(ctx, tx, datasetID, nextOrdinal)
	if err != nil {
		return WarehouseCursor{}, err
	}
	if segment.State == WarehouseSegmentCollected {
		return WarehouseCursor{}, ErrImmutableConflict
	}
	advancedAt, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return WarehouseCursor{}, err
	}
	cursor := WarehouseCursor{
		Scope: scope, DatasetID: datasetID, Ordinal: nextOrdinal,
		ManifestRevision: segment.ManifestRevision, AdvancedAt: advancedAt,
	}
	if expectedOrdinal == 0 {
		var revision int64
		err := tx.queryRow(ctx, `
			INSERT INTO pm.warehouse_dataset_cursors (
				org_id, workspace_id, dataset_id, cursor_ordinal, manifest_revision, advanced_at
			) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (org_id, workspace_id, dataset_id) DO NOTHING
			RETURNING manifest_revision
		`, scope.OrgID, scope.WorkspaceID, datasetID, int64(nextOrdinal), int64(segment.ManifestRevision), advancedAt).Scan(&revision)
		if errors.Is(err, pgx.ErrNoRows) {
			return WarehouseCursor{}, ErrCASConflict
		}
		if err != nil {
			return WarehouseCursor{}, warehouseWriteError("insert warehouse dataset cursor", err)
		}
		cursor.ManifestRevision = uint64(revision)
		return cursor, nil
	}
	var revision int64
	err = tx.queryRow(ctx, `
		UPDATE pm.warehouse_dataset_cursors
		SET cursor_ordinal = $4, manifest_revision = $5, advanced_at = $6
		WHERE org_id = $1 AND workspace_id = $2 AND dataset_id = $3 AND cursor_ordinal = $7
		RETURNING manifest_revision
	`, scope.OrgID, scope.WorkspaceID, datasetID, int64(nextOrdinal), int64(segment.ManifestRevision), advancedAt,
		int64(expectedOrdinal)).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return WarehouseCursor{}, ErrCASConflict
	}
	if err != nil {
		return WarehouseCursor{}, warehouseWriteError("advance warehouse dataset cursor", err)
	}
	cursor.ManifestRevision = uint64(revision)
	return cursor, nil
}

// RetainWarehouseSegment records one durable holder of one registered segment.
// Exact replay returns the stored row; a different holder identity for the same
// position is a conflict.
func RetainWarehouseSegment(ctx context.Context, tx *Transaction, retention WarehouseRetention) (WarehouseRetention, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return WarehouseRetention{}, err
	}
	if retention.Scope != scope || !validStoreID(retention.DatasetID) || retention.Ordinal == 0 ||
		!retention.Kind.Valid() || !validStoreID(retention.HolderID) {
		return WarehouseRetention{}, errors.New("warehouse retention request is invalid")
	}
	segment, err := LoadWarehouseSegmentAt(ctx, tx, retention.DatasetID, retention.Ordinal)
	if err != nil {
		return WarehouseRetention{}, err
	}
	if segment.State == WarehouseSegmentCollected {
		return WarehouseRetention{}, ErrImmutableConflict
	}
	retention.Scope = scope
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.warehouse_segment_references (
			org_id, workspace_id, dataset_id, segment_ordinal, holder_kind, holder_id
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (org_id, workspace_id, dataset_id, segment_ordinal, holder_kind, holder_id) DO NOTHING
	`,
		scope.OrgID, scope.WorkspaceID, retention.DatasetID, int64(retention.Ordinal),
		retention.Kind, retention.HolderID,
	); err != nil {
		return WarehouseRetention{}, warehouseWriteError("retain warehouse segment", err)
	}
	if err := tx.queryRow(ctx, `
		SELECT created_at
		FROM pm.warehouse_segment_references
		WHERE org_id = $1 AND workspace_id = $2 AND dataset_id = $3
		  AND segment_ordinal = $4 AND holder_kind = $5 AND holder_id = $6
	`, scope.OrgID, scope.WorkspaceID, retention.DatasetID, int64(retention.Ordinal),
		retention.Kind, retention.HolderID).Scan(&retention.CreatedAt); err != nil {
		return WarehouseRetention{}, fmt.Errorf("read warehouse retention registration: %w", err)
	}
	retention.CreatedAt = retention.CreatedAt.UTC()
	return retention, nil
}

// LoadWarehouseRetention loads every holder of one registered segment.
func LoadWarehouseRetention(ctx context.Context, tx *Transaction, datasetID string, ordinal uint64) ([]WarehouseRetention, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return nil, err
	}
	if !validStoreID(datasetID) || ordinal == 0 {
		return nil, errors.New("warehouse retention identity is invalid")
	}
	rows, err := tx.query(ctx, `
		SELECT holder_kind, holder_id, created_at
		FROM pm.warehouse_segment_references
		WHERE org_id = $1 AND workspace_id = $2 AND dataset_id = $3 AND segment_ordinal = $4
		ORDER BY holder_kind, holder_id
	`, scope.OrgID, scope.WorkspaceID, datasetID, int64(ordinal))
	if err != nil {
		return nil, fmt.Errorf("load warehouse segment retention: %w", err)
	}
	defer rows.Close()
	retentions := make([]WarehouseRetention, 0, 4)
	for rows.Next() {
		retention := WarehouseRetention{Scope: scope, DatasetID: datasetID, Ordinal: ordinal}
		if err := rows.Scan(&retention.Kind, &retention.HolderID, &retention.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan warehouse segment retention: %w", err)
		}
		retention.CreatedAt = retention.CreatedAt.UTC()
		retentions = append(retentions, retention)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate warehouse segment retention: %w", err)
	}
	return retentions, nil
}

// CreateWarehouseSnapshot pins the exact readable registered set at one
// revision and records the snapshot as the retention holder of every segment in
// that set. A revision whose rows are no longer complete cannot be pinned.
func CreateWarehouseSnapshot(ctx context.Context, tx *Transaction, snapshot WarehouseSnapshot) (WarehouseSnapshot, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return WarehouseSnapshot{}, err
	}
	if snapshot.Scope != scope || !validStoreID(snapshot.SnapshotID) || !validStoreID(snapshot.DatasetID) ||
		snapshot.ManifestRevision == 0 {
		return WarehouseSnapshot{}, errors.New("warehouse snapshot request is invalid")
	}
	if _, err := LoadWarehouseDataset(ctx, tx, snapshot.DatasetID); err != nil {
		return WarehouseSnapshot{}, err
	}
	manifest, err := LoadManifest(ctx, tx, snapshot.DatasetID)
	if err != nil {
		return WarehouseSnapshot{}, err
	}
	if snapshot.ManifestRevision > manifest.Revision {
		return WarehouseSnapshot{}, ErrImmutableConflict
	}
	segments, err := LoadWarehouseSegments(ctx, tx, snapshot.DatasetID)
	if err != nil {
		return WarehouseSnapshot{}, err
	}
	superseded, err := loadWarehouseRetirementsThrough(ctx, tx, snapshot.DatasetID, snapshot.ManifestRevision)
	if err != nil {
		return WarehouseSnapshot{}, err
	}
	retained := make([]uint64, 0, len(segments))
	for _, segment := range segments {
		// Membership at the revision is every ordinal registered by then that
		// was not already retired by then: a compacted source is not a member
		// beside the replacement that superseded it, even while a retention
		// holder keeps its bytes.
		if segment.Ordinal > snapshot.ManifestRevision {
			continue
		}
		if _, retired := superseded[segment.Ordinal]; retired {
			continue
		}
		if segment.State == WarehouseSegmentCollected {
			return WarehouseSnapshot{}, ErrImmutableConflict
		}
		retained = append(retained, segment.Ordinal)
	}
	if len(retained) == 0 {
		return WarehouseSnapshot{}, ErrImmutableConflict
	}
	var createdAt time.Time
	if err := tx.queryRow(ctx, `
		INSERT INTO pm.warehouse_snapshots (
			org_id, workspace_id, snapshot_id, dataset_id, manifest_revision
		) VALUES ($1, $2, $3, $4, $5)
		RETURNING created_at
	`,
		scope.OrgID, scope.WorkspaceID, snapshot.SnapshotID, snapshot.DatasetID,
		int64(snapshot.ManifestRevision),
	).Scan(&createdAt); err != nil {
		return WarehouseSnapshot{}, warehouseWriteError("create warehouse snapshot", err)
	}
	createdAt = createdAt.UTC()
	for _, ordinal := range retained {
		if _, err := RetainWarehouseSegment(ctx, tx, WarehouseRetention{
			Scope: scope, DatasetID: snapshot.DatasetID, Ordinal: ordinal,
			Kind: WarehouseRetentionSnapshot, HolderID: snapshot.SnapshotID,
		}); err != nil {
			return WarehouseSnapshot{}, err
		}
	}
	snapshot.Scope = scope
	snapshot.Segments = retained
	snapshot.CreatedAt = createdAt
	return snapshot, nil
}

// loadWarehouseRetirementsThrough reports every ordinal of one dataset that a
// manifest revision at or before the given revision retired.
func loadWarehouseRetirementsThrough(
	ctx context.Context,
	tx *Transaction,
	datasetID string,
	revision uint64,
) (map[uint64]struct{}, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return nil, err
	}
	rows, err := tx.query(ctx, `
		SELECT segment_ordinal
		FROM pm.warehouse_segments
		WHERE org_id = $1 AND workspace_id = $2 AND dataset_id = $3
		  AND retired_revision IS NOT NULL AND retired_revision <= $4
	`, scope.OrgID, scope.WorkspaceID, datasetID, int64(revision))
	if err != nil {
		return nil, fmt.Errorf("load warehouse segment retirements: %w", err)
	}
	defer rows.Close()
	retired := make(map[uint64]struct{}, 8)
	for rows.Next() {
		var ordinal int64
		if err := rows.Scan(&ordinal); err != nil {
			return nil, fmt.Errorf("scan warehouse segment retirement: %w", err)
		}
		retired[uint64(ordinal)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate warehouse segment retirements: %w", err)
	}
	return retired, nil
}

// LoadWarehouseSnapshot loads one retained snapshot and its exact segment set.
func LoadWarehouseSnapshot(ctx context.Context, tx *Transaction, snapshotID string) (WarehouseSnapshot, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return WarehouseSnapshot{}, err
	}
	if !validStoreID(snapshotID) {
		return WarehouseSnapshot{}, errors.New("warehouse snapshot identity is invalid")
	}
	snapshot := WarehouseSnapshot{Scope: scope, SnapshotID: snapshotID}
	var revision int64
	if err := tx.queryRow(ctx, `
		SELECT dataset_id, manifest_revision, created_at
		FROM pm.warehouse_snapshots
		WHERE org_id = $1 AND workspace_id = $2 AND snapshot_id = $3
	`, scope.OrgID, scope.WorkspaceID, snapshotID).Scan(&snapshot.DatasetID, &revision, &snapshot.CreatedAt); errors.Is(err, pgx.ErrNoRows) {
		return WarehouseSnapshot{}, ErrNotFound
	} else if err != nil {
		return WarehouseSnapshot{}, fmt.Errorf("load warehouse snapshot: %w", err)
	}
	snapshot.ManifestRevision = uint64(revision)
	snapshot.CreatedAt = snapshot.CreatedAt.UTC()
	rows, err := tx.query(ctx, `
		SELECT segment_ordinal
		FROM pm.warehouse_segment_references
		WHERE org_id = $1 AND workspace_id = $2 AND dataset_id = $3
		  AND holder_kind = 'snapshot' AND holder_id = $4
		ORDER BY segment_ordinal
	`, scope.OrgID, scope.WorkspaceID, snapshot.DatasetID, snapshotID)
	if err != nil {
		return WarehouseSnapshot{}, fmt.Errorf("load warehouse snapshot segments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal int64
		if err := rows.Scan(&ordinal); err != nil {
			return WarehouseSnapshot{}, fmt.Errorf("scan warehouse snapshot segment: %w", err)
		}
		snapshot.Segments = append(snapshot.Segments, uint64(ordinal))
	}
	if err := rows.Err(); err != nil {
		return WarehouseSnapshot{}, fmt.Errorf("iterate warehouse snapshot segments: %w", err)
	}
	return snapshot, nil
}

// RetireWarehouseSegments supersedes a set of registered segments. A segment
// named by any retention holder is never collected: it moves to the retained
// state and is reported with the holders that keep it. Only a segment that no
// snapshot, plan, proof, backup or reservation names is collected. The
// retirement is recorded at the dataset's committed manifest revision in this
// transaction, so a caller retires a segment in the same transaction that
// advanced the manifest past it, and only an ordinal below that revision.
func RetireWarehouseSegments(
	ctx context.Context,
	tx *Transaction,
	datasetID string,
	ordinals []uint64,
) (WarehouseRetirement, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return WarehouseRetirement{}, err
	}
	retirement := WarehouseRetirement{Collected: []uint64{}, Retained: []WarehouseRetainedSegment{}}
	if len(ordinals) == 0 || len(ordinals) > maximumWarehouseRetirementOrdinals {
		return retirement, errors.New("warehouse retirement request is invalid")
	}
	retiredAt, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return retirement, err
	}
	// A segment retires at the manifest revision of the transaction that
	// retires it: a compaction retires its sources in the transaction that
	// registers their replacement, so that replacement's revision is the first
	// revision whose membership no longer holds them.
	manifest, err := LoadManifest(ctx, tx, datasetID)
	if err != nil {
		return retirement, err
	}
	retiredRevision := manifest.Revision
	seen := make(map[uint64]struct{}, len(ordinals))
	for _, ordinal := range ordinals {
		if ordinal == 0 || ordinal >= retiredRevision {
			return retirement, errors.New("warehouse retirement request is invalid")
		}
		if _, duplicate := seen[ordinal]; duplicate {
			continue
		}
		seen[ordinal] = struct{}{}
		segment, err := LoadWarehouseSegmentAt(ctx, tx, datasetID, ordinal)
		if err != nil {
			return retirement, err
		}
		if segment.State == WarehouseSegmentCollected {
			retirement.Collected = append(retirement.Collected, ordinal)
			continue
		}
		holders, err := LoadWarehouseRetention(ctx, tx, datasetID, ordinal)
		if err != nil {
			return retirement, err
		}
		if len(holders) > 0 {
			kinds := make([]WarehouseRetentionKind, 0, len(holders))
			for _, holder := range holders {
				kinds = append(kinds, holder.Kind)
			}
			retirement.Retained = append(retirement.Retained, WarehouseRetainedSegment{Ordinal: ordinal, Holders: kinds})
			if segment.State == WarehouseSegmentLive {
				if _, err := tx.exec(ctx, `
					UPDATE pm.warehouse_segments
					SET state = 'retired', retired_at = $4, retired_revision = $6
					WHERE org_id = $1 AND workspace_id = $2 AND dataset_id = $3 AND segment_ordinal = $5
				`, scope.OrgID, scope.WorkspaceID, datasetID, retiredAt, int64(ordinal), int64(retiredRevision)); err != nil {
					return retirement, warehouseWriteError("retire retained warehouse segment", err)
				}
			}
			continue
		}
		if _, err := tx.exec(ctx, `
			UPDATE pm.warehouse_segments
			SET state = 'collected', retired_at = $4, collected_at = $4,
			    retired_revision = COALESCE(retired_revision, $6)
			WHERE org_id = $1 AND workspace_id = $2 AND dataset_id = $3 AND segment_ordinal = $5
		`, scope.OrgID, scope.WorkspaceID, datasetID, retiredAt, int64(ordinal), int64(retiredRevision)); err != nil {
			return retirement, warehouseWriteError("collect warehouse segment", err)
		}
		retirement.Collected = append(retirement.Collected, ordinal)
	}
	sort.Slice(retirement.Collected, func(left, right int) bool {
		return retirement.Collected[left] < retirement.Collected[right]
	})
	return retirement, nil
}

// maximumWarehouseRetirementOrdinals bounds one retirement request.
const maximumWarehouseRetirementOrdinals = 4096

func validColumnFingerprint(value string) bool {
	return len(value) == len("sha256:")+64 && value[:len("sha256:")] == "sha256:" && validLowerHex(value[len("sha256:"):])
}

func validLowerHex(value string) bool {
	for _, character := range value {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			continue
		}
		return false
	}
	return len(value) > 0
}

// jsonObjectDocument reports whether one bounded document is a JSON object,
// which is the only shape a registered segment schema may have.
func jsonObjectDocument(document []byte) bool {
	if !json.Valid(document) {
		return false
	}
	trimmed := bytes.TrimSpace(document)
	return len(trimmed) >= 2 && trimmed[0] == '{'
}

// warehouseWriteError maps one durable warehouse write refusal to the store's
// error vocabulary. A constraint refusal is a conflict with retained state, not
// an unavailable database.
func warehouseWriteError(operation string, err error) error {
	var databaseError *pgconn.PgError
	if errors.As(err, &databaseError) {
		switch databaseError.Code {
		case "23503", "23505", "23514", "23000":
			return fmt.Errorf("%w: %s", ErrImmutableConflict, operation)
		}
	}
	return fmt.Errorf("%s: %w", operation, err)
}
