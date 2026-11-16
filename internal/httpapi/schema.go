package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/schema"
)

// GetConnectionSchema reports the selection of one resource of an authorized
// connection: the selected immutable snapshot, the effective typed fields built
// from it, and the selection revision of the resource. The route reads the
// selection and the stored candidates from PM PostgreSQL. The selection
// revision is 1 while the resource has no selection, and the ETag header
// carries the same revision. Nothing on this route discovers metadata or
// selects a snapshot, and a failed or partial discovery never replaces the
// last complete valid snapshot. The route reads no provider credential and
// performs no provider I/O.
func (service *Service) GetConnectionSchema(
	ctx context.Context,
	request GetConnectionSchemaRequestObject,
) (GetConnectionSchemaResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	actor := state.identity.Actor
	connection, err := service.identity.InspectConnection(ctx, actor, scope, request.ConnectionId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	binding := schema.Binding{
		OrgID:              scope.OrgID,
		WorkspaceID:        scope.WorkspaceID,
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
	view, err := service.identity.InspectSchemaSelection(ctx, actor, scope, connection.ConnectionID, binding.Resource)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	var projected api.ConnectionSchemaInspection
	switch {
	case request.Params.SnapshotId != nil:
		// A named snapshot is read from the stored candidates of the
		// connection, and it must carry the exact binding that the caller names.
		reference := contract.SemanticDigest(*request.Params.SnapshotId)
		if err := reference.Validate(); err != nil {
			return asProblemResponse(errInvalidHTTPInput, nil), nil
		}
		snapshot, err := service.identity.InspectSchemaCandidate(ctx, actor, scope, binding.ConnectionID, reference)
		if err != nil {
			return asProblemResponse(err, nil), nil
		}
		if snapshot.Binding != binding {
			return asProblemResponse(concealedSchemaSelector(schema.ErrSnapshotScope), nil), nil
		}
		if projected, err = connectionSchemaResponse(scope, binding, reference, &snapshot); err != nil {
			return asProblemResponse(err, nil), nil
		}
	case view.Snapshot == nil:
		// An absent selection is explicit: this route does not invent one.
		projected = api.ConnectionSchemaInspection{
			OrgId:        scope.OrgID,
			WorkspaceId:  scope.WorkspaceID,
			ConnectorId:  binding.ConnectorID,
			ConnectionId: binding.ConnectionID,
			Selected:     nullableText(""),
			Snapshot:     absentSchemaSnapshot(binding, "no_selection"),
			Fields:       []api.SchemaField{},
		}
	default:
		if view.Snapshot.Binding != binding {
			return asProblemResponse(concealedSchemaSelector(schema.ErrSnapshotScope), nil), nil
		}
		if projected, err = connectionSchemaResponse(scope, binding, view.SchemaRef, view.Snapshot); err != nil {
			return asProblemResponse(err, nil), nil
		}
	}
	projected.SelectionRevision = int64(view.Revision)
	setETag(ctx, view.Revision)
	return GetConnectionSchema200JSONResponse{Body: projected}, nil
}

// SelectConnectionSchema selects one stored complete candidate of a declared
// relation at the selection revision that the caller read. It uses the shared
// mutation control: the Idempotency-Key, the protocol expected_revision and
// If-Match. This route performs no provider I/O and reads no credential.
func (service *Service) SelectConnectionSchema(
	ctx context.Context,
	request SelectConnectionSchemaRequestObject,
) (SelectConnectionSchemaResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if request.Body == nil {
		return invalidRequestResponse(), nil
	}
	control, err := parseMutationControl(string(request.Params.IdempotencyKey), request.Body.Protocol, request.Params.IfMatch)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	if service.schemaSelection == nil {
		return asMutationProblemResponse(capabilityUnavailable(), request.Body.Protocol.RequestKey,
			api.SafeOutcomeReferenceKindConnection, request.ConnectionId), nil
	}
	result, err := service.schemaSelection.Select(ctx, state.identity.Actor, scope, app.SelectSchema{
		ConnectionID: request.ConnectionId,
		Resource:     request.Body.Selection.Resource,
		SchemaRef:    contract.SemanticDigest(request.Body.Selection.SchemaRef),
		Control:      control,
	})
	if err != nil {
		return asMutationProblemResponse(err, request.Body.Protocol.RequestKey,
			api.SafeOutcomeReferenceKindConnection, request.ConnectionId), nil
	}
	disposition := api.MutationResultDispositionUpdated
	if result.FirstSelection {
		disposition = api.MutationResultDispositionCreated
	}
	fresh, replay, err := mutationResponses(result.Mutation, request.ConnectionId,
		api.SafeOutcomeReferenceKindConnection, state.identity.Actor, disposition)
	if err != nil {
		return nil, err
	}
	body := api.SelectConnectionSchema200JSONResponseBody{}
	if result.Mutation.Replayed {
		err = body.FromMutationReplayResponse(replay)
	} else {
		err = body.FromMutationResultResponse(fresh)
	}
	if err != nil {
		return nil, err
	}
	setETag(ctx, result.SelectionRevision)
	return SelectConnectionSchema200JSONResponse(body), nil
}

func (r problemResponse) VisitSelectConnectionSchemaResponse(w http.ResponseWriter) error {
	return r.write(w)
}

// DiscoverConnectionSchema admits one durable discovery run of one declared
// relation. The run stores one complete candidate through a worker activity;
// this route performs no provider I/O, reads no credential and never selects.
func (service *Service) DiscoverConnectionSchema(
	ctx context.Context,
	request DiscoverConnectionSchemaRequestObject,
) (DiscoverConnectionSchemaResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	requestKey := string(request.Params.IdempotencyKey)
	if request.Body == nil || requestKey == "" {
		return invalidRequestResponse(), nil
	}
	result, err := service.admission.AcceptDiscovery(ctx, state.identity.Actor, scope, app.DiscoveryInvocation{
		ConnectionRef: request.ConnectionId,
		Resource:      request.Body.Resource,
		RequestKey:    requestKey,
	})
	if err != nil {
		return asMutationProblemResponse(err, requestKey, api.SafeOutcomeReferenceKindRun, ""), nil
	}
	return DiscoverConnectionSchema202JSONResponse(api.AdmissionResponse{
		IntentDigest:     string(result.IntentDigest),
		OutcomeReference: api.SafeOutcomeReference{Kind: api.SafeOutcomeReferenceKindRun, Ref: result.RunID},
		Replay:           result.Replayed,
		RequestKey:       requestKey,
		RunRef:           result.RunID,
		State:            api.AdmissionResponseStateAccepted,
	}), nil
}

func (r problemResponse) VisitDiscoverConnectionSchemaResponse(w http.ResponseWriter) error {
	return r.write(w)
}

// concealedSchemaSelector translates the metadata store's refusals that the
// caller's own selector can correct into the shared concealment refusal. The
// selector is the caller's own: the binding members and the snapshot identity it
// names are correctable input, so a snapshot this exact binding cannot see, and
// a reference no stored payload answers, are refused exactly like a restricted
// artifact — one 404 that does not disclose whether the snapshot exists under
// another connection, account, permission revision, resource or source revision
// — and never as an internal fault. Only those two refusals are concealed. A
// selected payload that is present but unreadable, or unreadable because it is
// corrupt, keeps the store's own refusal: no selector the caller can name
// answers it, so reporting it as a missing one would hide a storage fault.
func concealedSchemaSelector(err error) error {
	switch {
	case errors.Is(err, schema.ErrSnapshotScope), errors.Is(err, schema.ErrSnapshotNotFound):
		return fmt.Errorf("%w: the selected metadata snapshot is unavailable in the authenticated scope",
			auth.ErrResourceNotFound)
	default:
		return err
	}
}

// absentSchemaSnapshot reports the withheld snapshot identity of a connection
// that has selected nothing yet. It carries the exact binding the caller asked
// about, the one blocker that explains the absence, and no fields, so an
// absent selection is never rendered as an empty discovery.
func absentSchemaSnapshot(binding schema.Binding, blocker string) api.ConnectionSchemaSnapshot {
	return api.ConnectionSchemaSnapshot{
		ConnectorId:        binding.ConnectorID,
		ConnectionId:       binding.ConnectionID,
		ProviderAccountId:  binding.ProviderAccountID,
		PermissionRevision: binding.PermissionRevision,
		Resource:           binding.Resource,
		SourceRevision:     binding.SourceRevision,
		ContentFingerprint: "",
		Complete:           false,
		Blockers:           []string{blocker},
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
