package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/schema"
)

// GetConnectionSchema reports the immutable metadata snapshot one authorized
// connection currently selects, together with the effective typed fields built
// from it. Nothing on this route discovers metadata, promotes a snapshot, or
// selects one: the connection's own config.json owns the sole schema_ref
// selection, and a failed or partial discovery never replaces the last complete
// valid snapshot. The route reads no provider credential and performs no
// provider I/O.
func (service *Service) GetConnectionSchema(
	ctx context.Context,
	request GetConnectionSchemaRequestObject,
) (GetConnectionSchemaResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	connection, err := service.identity.InspectConnection(ctx, state.identity.Actor, scope, request.ConnectionId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	store, err := service.connectionSchemaStore(request.ConnectionId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	binding := schema.Binding{
		ConnectorID:        connection.ConnectorID,
		ConnectionID:       connection.ConnectionID,
		ProviderAccountID:  connection.ProviderAccountID,
		PermissionRevision: request.Params.PermissionRevision,
		Resource:           request.Params.Resource,
		SourceRevision:     request.Params.SourceRevision,
	}
	if request.Params.ProviderAccountId != nil {
		binding.ProviderAccountID = *request.Params.ProviderAccountId
	}
	selected, present, err := store.Selection()
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if request.Params.SnapshotId != nil {
		reference := contract.SemanticDigest(*request.Params.SnapshotId)
		if err := reference.Validate(); err != nil {
			return asProblemResponse(errInvalidHTTPInput, nil), nil
		}
		selected = reference
		present = true
	}
	if !present {
		// An absent selection is explicit: the connection has no complete
		// discovery yet, and this route does not invent one.
		return GetConnectionSchema200JSONResponse(api.ConnectionSchemaInspection{
			OrgId:        scope.OrgID,
			WorkspaceId:  scope.WorkspaceID,
			ConnectorId:  binding.ConnectorID,
			ConnectionId: binding.ConnectionID,
			Selected:     nullableText(""),
			Snapshot:     absentSchemaSnapshot(binding),
			Fields:       []api.SchemaField{},
		}), nil
	}
	snapshot, err := store.LoadFor(binding, selected)
	if err != nil {
		return asProblemResponse(concealedSchemaSelector(err), nil), nil
	}
	projected, err := connectionSchemaResponse(scope, binding, selected, &snapshot)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	return GetConnectionSchema200JSONResponse(projected), nil
}

// concealedSchemaSelector translates the metadata store's not-found and
// scope-mismatch refusals into the shared concealment refusal. The selector is
// the caller's own: the binding members and the snapshot identity it names are
// correctable input, so a snapshot this exact binding cannot see is refused
// exactly like a restricted artifact — one 404 that does not disclose whether
// the snapshot exists under another connection, account, permission revision,
// resource or source revision — and never as an internal fault. The store
// cannot tell this route which of the two it held, and concealing both is the
// answer that reveals nothing. A stored payload that is present but unusable
// keeps the store's own refusal.
func concealedSchemaSelector(err error) error {
	switch {
	case errors.Is(err, schema.ErrSnapshotScope), errors.Is(err, schema.ErrSnapshotUnavailable):
		return fmt.Errorf("%w: the selected metadata snapshot is unavailable in the authenticated scope",
			auth.ErrResourceNotFound)
	default:
		return err
	}
}

// connectionSchemaStore resolves the connection's owned metadata state
// directory beneath the configured root. A directory traversal or an
// unconfigured root is a refusal, never a default location.
func (service *Service) connectionSchemaStore(connectionID string) (*schema.Store, error) {
	if service.connectionMetadataRoot == "" {
		return nil, fmt.Errorf("connection metadata state is unavailable: %w", capabilityUnavailable())
	}
	if connectionID == "" || connectionID == "." || connectionID == ".." ||
		strings.ContainsAny(connectionID, `/\`) || strings.ContainsRune(connectionID, 0) {
		return nil, fmt.Errorf("%w: connection_id cannot name a state directory", errInvalidHTTPInput)
	}
	return schema.NewStore(filepath.Join(service.connectionMetadataRoot, connectionID))
}

// absentSchemaSnapshot reports the withheld snapshot identity of a connection
// that has selected nothing yet. It carries the exact binding the caller asked
// about and no fields, so an absent selection is never rendered as an empty
// discovery.
func absentSchemaSnapshot(binding schema.Binding) api.ConnectionSchemaSnapshot {
	return api.ConnectionSchemaSnapshot{
		ConnectorId:        binding.ConnectorID,
		ConnectionId:       binding.ConnectionID,
		ProviderAccountId:  binding.ProviderAccountID,
		PermissionRevision: binding.PermissionRevision,
		Resource:           binding.Resource,
		SourceRevision:     binding.SourceRevision,
		ContentFingerprint: "",
		Complete:           false,
		Blockers:           []string{"no_complete_discovery"},
		FieldCount:         0,
	}
}

// connectionSchemaResponse projects one selected immutable snapshot. Every
// field is the exact declaration the snapshot holds; nothing is inferred from a
// sample row or normalized into a second catalog.
func connectionSchemaResponse(
	scope contract.Scope,
	binding schema.Binding,
	selected contract.SemanticDigest,
	snapshot *schema.Snapshot,
) (api.ConnectionSchemaInspection, error) {
	if snapshot == nil {
		return api.ConnectionSchemaInspection{}, errors.New("a schema inspection requires a selected snapshot")
	}
	content, err := snapshot.ContentFingerprint()
	if err != nil {
		return api.ConnectionSchemaInspection{}, err
	}
	if content != snapshot.Fingerprint {
		return api.ConnectionSchemaInspection{}, errors.New("the selected snapshot does not match its declared fingerprint")
	}
	stored := snapshot.Binding
	if stored != binding {
		return api.ConnectionSchemaInspection{}, errors.New("the selected snapshot is bound to another scope")
	}
	projected := api.ConnectionSchemaInspection{
		OrgId:        scope.OrgID,
		WorkspaceId:  scope.WorkspaceID,
		ConnectorId:  stored.ConnectorID,
		ConnectionId: stored.ConnectionID,
		Selected:     nullableText(string(selected)),
		Snapshot: api.ConnectionSchemaSnapshot{
			ConnectorId:        stored.ConnectorID,
			ConnectionId:       stored.ConnectionID,
			ProviderAccountId:  stored.ProviderAccountID,
			PermissionRevision: stored.PermissionRevision,
			Resource:           stored.Resource,
			SourceRevision:     stored.SourceRevision,
			ContentFingerprint: string(snapshot.Fingerprint),
			Complete:           snapshot.Completeness.Complete,
			Blockers:           make([]string, 0, len(snapshot.Completeness.Gaps)),
			FieldCount:         len(snapshot.Fields),
		},
		Fields: make([]api.SchemaField, 0, len(snapshot.Fields)),
	}
	for _, gap := range snapshot.Completeness.Gaps {
		projected.Snapshot.Blockers = append(projected.Snapshot.Blockers, string(gap.Kind)+":"+gap.Subject)
	}
	for _, field := range snapshot.Fields {
		kind, err := schemaFieldKind(field.Schema.Type)
		if err != nil {
			return api.ConnectionSchemaInspection{}, err
		}
		projected.Fields = append(projected.Fields, api.SchemaField{
			Name:      field.Name,
			Pointer:   field.Pointer,
			Required:  field.Required,
			Writable:  field.Writable,
			Type:      kind,
			Precision: field.Precision,
			Scale:     field.Scale,
		})
	}
	return projected, nil
}

// schemaFieldKind maps one declared contract type onto the closed wire
// vocabulary. A shape this projection cannot carry is refused rather than
// approximated.
func schemaFieldKind(kind contract.SchemaType) (api.SchemaFieldType, error) {
	projected := api.SchemaFieldType(kind)
	if kind == contract.SchemaTypeDecimal {
		// The declared logical type is a decimal; the wire vocabulary names the
		// same logical family "number" and carries precision and scale beside
		// it, so the declaration stays exact.
		projected = api.SchemaFieldTypeNumber
	}
	if !projected.Valid() {
		return "", fmt.Errorf("the selected snapshot declares a field type this projection cannot carry: %s", kind)
	}
	return projected, nil
}

// The schema inspection route carries no mutation identity, so every refusal
// serializes as the shared problem document.
func (r problemResponse) VisitGetConnectionSchemaResponse(w http.ResponseWriter) error {
	return r.write(w)
}
