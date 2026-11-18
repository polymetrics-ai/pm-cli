package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// SchemaSelectionAuditAction is the action of the workspace audit record that
// each successful schema selection writes.
const SchemaSelectionAuditAction = "schema.select"

// FirstSchemaSelectionRevision is the selection revision of a connection and
// resource that has no selection yet (ADR 0007). It is never 0.
const FirstSchemaSelectionRevision uint64 = 1

// SchemaSelection is the current selection of one connection and resource.
// Present is false while no selection exists. Revision is then
// FirstSchemaSelectionRevision and SchemaRef is empty.
type SchemaSelection struct {
	ConnectionID string
	Resource     string
	SchemaRef    string
	Revision     uint64
	Present      bool
}

// SelectSchemaCandidateRequest selects one stored candidate of a connection
// and resource at the selection revision that the caller read.
type SelectSchemaCandidateRequest struct {
	ConnectionID     string
	Resource         string
	SchemaRef        string
	ExpectedRevision uint64
	AuditID          string
}

// SchemaSelectionChange is the selection before and after one successful
// selection.
type SchemaSelectionChange struct {
	Previous SchemaSelection
	Current  SchemaSelection
}

// LoadSchemaSelection returns the current selection of one connection and
// resource in the transaction's scope. A missing row is not an error: it is
// the selection at revision 1 with no selected candidate.
func LoadSchemaSelection(ctx context.Context, tx *Transaction, connectionID, resource string) (SchemaSelection, error) {
	if _, err := tx.requireScope(); err != nil {
		return SchemaSelection{}, err
	}
	if !validStoreID(connectionID) || !validStoreID(resource) {
		return SchemaSelection{}, errors.New("schema selection selector is invalid")
	}
	return loadSchemaSelection(ctx, tx, connectionID, resource, false)
}

func loadSchemaSelection(
	ctx context.Context,
	tx *Transaction,
	connectionID, resource string,
	lock bool,
) (SchemaSelection, error) {
	query := `
		SELECT schema_ref, selection_revision
		FROM pm.schema_selections
		WHERE connection_id = $1 AND resource = $2
	`
	if lock {
		query += ` FOR UPDATE`
	}
	selection := SchemaSelection{ConnectionID: connectionID, Resource: resource}
	var revision int64
	if err := tx.queryRow(ctx, query, connectionID, resource).Scan(&selection.SchemaRef, &revision); errors.Is(err, pgx.ErrNoRows) {
		selection.Revision = FirstSchemaSelectionRevision
		return selection, nil
	} else if err != nil {
		return SchemaSelection{}, fmt.Errorf("load schema selection: %w", err)
	}
	if revision <= int64(FirstSchemaSelectionRevision) || !schemaReferencePattern.MatchString(selection.SchemaRef) {
		return SchemaSelection{}, errors.New("stored schema selection is invalid")
	}
	selection.Revision = uint64(revision)
	selection.Present = true
	return selection, nil
}

// SelectSchemaCandidate compares the expected selection revision with the
// current one and, when they are equal, selects the candidate and adds 1 to
// the revision. It writes one workspace audit record in the caller's
// transaction. A stale expected revision returns ErrCASConflict and writes
// nothing. A candidate that the connection and resource do not hold returns
// ErrNotFound. The caller's transaction rolls back every write of a refusal.
func SelectSchemaCandidate(
	ctx context.Context,
	tx *Transaction,
	request SelectSchemaCandidateRequest,
) (SchemaSelectionChange, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return SchemaSelectionChange{}, err
	}
	if !validStoreID(request.ConnectionID) || !validStoreID(request.Resource) ||
		!schemaReferencePattern.MatchString(request.SchemaRef) ||
		request.ExpectedRevision < FirstSchemaSelectionRevision || !validStoreID(request.AuditID) {
		return SchemaSelectionChange{}, errors.New("schema selection request is invalid")
	}
	previous, err := loadSchemaSelection(ctx, tx, request.ConnectionID, request.Resource, true)
	if err != nil {
		return SchemaSelectionChange{}, err
	}
	if previous.Revision != request.ExpectedRevision {
		return SchemaSelectionChange{}, ErrCASConflict
	}
	current := SchemaSelection{
		ConnectionID: request.ConnectionID,
		Resource:     request.Resource,
		SchemaRef:    request.SchemaRef,
		Revision:     previous.Revision + 1,
		Present:      true,
	}
	if !previous.Present {
		// No row exists to lock. The primary key orders two first selections:
		// the second insert waits for the first and then inserts nothing.
		tag, err := tx.exec(ctx, `
			INSERT INTO pm.schema_selections (
				org_id, workspace_id, connection_id, resource, schema_ref, selection_revision
			) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT DO NOTHING
		`, scope.OrgID, scope.WorkspaceID, request.ConnectionID, request.Resource, request.SchemaRef, int64(current.Revision))
		if err != nil {
			return SchemaSelectionChange{}, safeMetadataWriteError("insert schema selection", err)
		}
		if tag.RowsAffected() != 1 {
			return SchemaSelectionChange{}, ErrCASConflict
		}
	} else {
		var revision int64
		if err := tx.queryRow(ctx, `
			UPDATE pm.schema_selections
			SET schema_ref = $3, selection_revision = selection_revision + 1, selected_at = clock_timestamp()
			WHERE connection_id = $1 AND resource = $2 AND selection_revision = $4
			RETURNING selection_revision
		`, request.ConnectionID, request.Resource, request.SchemaRef, int64(request.ExpectedRevision)).Scan(&revision); errors.Is(err, pgx.ErrNoRows) {
			return SchemaSelectionChange{}, ErrCASConflict
		} else if err != nil {
			return SchemaSelectionChange{}, safeMetadataWriteError("update schema selection", err)
		}
		if uint64(revision) != current.Revision {
			return SchemaSelectionChange{}, ErrCASConflict
		}
	}

	references := []contract.SafeReference{
		{Name: "connection", Ref: request.ConnectionID},
		{Name: "resource", Ref: request.Resource},
		{Name: "schema_ref", Ref: request.SchemaRef},
	}
	if previous.Present {
		references = append(references, contract.SafeReference{Name: "previous_schema_ref", Ref: previous.SchemaRef})
	}
	if err := insertWorkspaceAuditEntry(ctx, tx, AuditEntry{
		AuditID:   request.AuditID,
		Action:    SchemaSelectionAuditAction,
		TargetRef: request.ConnectionID,
		Payload: contract.SafePayload{
			References: references,
			Counts: []contract.SafeCount{
				{Name: "previous_selection_revision", Value: previous.Revision},
				{Name: "selection_revision", Value: current.Revision},
			},
			ReasonCodes: []string{},
		},
	}); err != nil {
		return SchemaSelectionChange{}, err
	}
	return SchemaSelectionChange{Previous: previous, Current: current}, nil
}
