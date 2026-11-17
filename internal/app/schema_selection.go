package app

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/schema"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// schemaSelectionOperation scopes the mutation receipts of schema selections.
const schemaSelectionOperation = "schema.select"

// SchemaSelectionService selects one stored complete candidate of a declared
// relation (ADR 0007, #285 slice 285c). It uses the store, the current
// authorization and the mutation receipts of the identity service, and the
// declared relation resolver of the customer SQL composition. It performs no
// provider I/O and reads no credential.
type SchemaSelectionService struct {
	identity        *IdentityService
	resolveRelation DiscoveryResolver
}

// NewSchemaSelectionService composes the selection use case. A nil resolver
// declares no relation, so every selection is refused with
// schema_relation_unknown.
func NewSchemaSelectionService(identity *IdentityService, resolveRelation DiscoveryResolver) (*SchemaSelectionService, error) {
	if identity == nil || identity.pool == nil || identity.authorize == nil {
		return nil, errors.New("schema selection requires the identity service")
	}
	return &SchemaSelectionService{identity: identity, resolveRelation: resolveRelation}, nil
}

// SelectSchema selects one candidate of one declared relation of a connection
// at the selection revision that the caller read.
type SelectSchema struct {
	ConnectionID string
	Resource     string
	SchemaRef    contract.SemanticDigest
	Control      MutationControl
}

// SchemaSelectionResult is the safe durable result of one selection. A replay
// returns the stored result.
type SchemaSelectionResult struct {
	ConnectionID      string
	Resource          string
	SchemaRef         string
	SelectionRevision uint64
	FirstSelection    bool
	Mutation          MutationMetadata
}

// SchemaSelectionView is the current selection of one connection and resource.
// SchemaRef and Snapshot are empty while no selection exists, and Revision is
// then 1.
type SchemaSelectionView struct {
	Revision  uint64
	SchemaRef contract.SemanticDigest
	Snapshot  *schema.Snapshot
}

// Select runs the checks of ADR 0007 in one transaction, in this order:
//  1. connection.manage on the connection, from the current grants;
//  2. the mutation control and the input;
//  3. the mutation receipt, so a replay returns the stored result, also
//     without If-Match;
//  4. the fresh precondition (If-Match);
//  5. the candidate: a stored complete candidate of a declared relation of the
//     connection, with the binding of that declaration;
//  6. the compare and set of the selection revision, with one audit record.
//
// Each refusal rolls back the transaction, so it writes no selection, no
// revision, no audit record and no receipt.
func (service *SchemaSelectionService) Select(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	request SelectSchema,
) (SchemaSelectionResult, error) {
	if service == nil || ctx == nil || !validApplicationID(request.ConnectionID) {
		return SchemaSelectionResult{}, invalidRequestFailure()
	}
	identity := service.identity
	var result SchemaSelectionResult
	err := store.WithinScope(ctx, identity.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		if err := store.BindAuthDeployment(ctx, tx, identity.deploymentID); err != nil {
			return err
		}
		if _, err := identity.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:        scope,
			Actor:        actor,
			Permission:   "connection.manage",
			ResourceKind: auth.ResourceConnection,
			ResourceID:   request.ConnectionID,
		}); err != nil {
			return mapAuthorizationFailure(err)
		}
		intentRevision, err := validateMutationControl(request.Control)
		if err != nil {
			return err
		}
		if !validApplicationID(request.Resource) || request.SchemaRef.Validate() != nil {
			return invalidRequestFailure()
		}
		digest, err := canonicalIntent(struct {
			Operation      string         `json:"operation"`
			Scope          contract.Scope `json:"scope"`
			ConnectionID   string         `json:"connection_id"`
			Resource       string         `json:"resource"`
			SchemaRef      string         `json:"schema_ref"`
			IntentRevision uint64         `json:"expected_revision"`
		}{
			Operation:      schemaSelectionOperation,
			Scope:          scope,
			ConnectionID:   request.ConnectionID,
			Resource:       request.Resource,
			SchemaRef:      string(request.SchemaRef),
			IntentRevision: intentRevision,
		})
		if err != nil {
			return invalidRequestFailure()
		}
		key := mutationReceiptKey(scope, actor, schemaSelectionOperation, request.ConnectionID, request.Control.IdempotencyKey)
		begin, err := store.BeginIdentityMutation(ctx, tx, key, digest, intentRevision)
		if err != nil {
			return mapMutationFailure(err)
		}
		if !begin.Fresh {
			if err := decodeReplay(begin.Receipt, &result); err != nil {
				return err
			}
			result.Mutation.Status = StatusOK
			result.Mutation.Replayed = true
			return nil
		}
		if err := requireFreshMutationPrecondition(request.Control); err != nil {
			return err
		}
		if err := service.requireSelectableCandidate(ctx, tx, scope, request); err != nil {
			return err
		}
		auditID, err := randomApplicationID(identity.random, "audit-", 18)
		if err != nil {
			return err
		}
		change, err := store.SelectSchemaCandidate(ctx, tx, store.SelectSchemaCandidateRequest{
			ConnectionID:     request.ConnectionID,
			Resource:         request.Resource,
			SchemaRef:        string(request.SchemaRef),
			ExpectedRevision: intentRevision,
			AuditID:          auditID,
		})
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return schemaRelationUnknownFailure()
			}
			return mapMutationFailure(err)
		}
		result = SchemaSelectionResult{
			ConnectionID:      request.ConnectionID,
			Resource:          request.Resource,
			SchemaRef:         change.Current.SchemaRef,
			SelectionRevision: change.Current.Revision,
			FirstSelection:    !change.Previous.Present,
			Mutation: MutationMetadata{
				ResultRef:       request.ConnectionID,
				Revision:        change.Current.Revision,
				ControlRevision: change.Current.Revision,
				Status:          StatusOK,
			},
		}
		return identity.finishMutation(ctx, tx, key, result.Mutation, result)
	})
	return result, err
}

// requireSelectableCandidate refuses a resource that no declared relation of
// the connection names, and a candidate that this connection and resource do
// not hold, with schema_relation_unknown. The candidate must also carry the
// binding of the current declaration, be complete, and decode to its own
// digest. A stored document that does not decode is a storage fault, not a
// selector that the caller can correct.
func (service *SchemaSelectionService) requireSelectableCandidate(
	ctx context.Context,
	tx *store.Transaction,
	scope contract.Scope,
	request SelectSchema,
) error {
	if service.resolveRelation == nil {
		return schemaRelationUnknownFailure()
	}
	declared, present := service.resolveRelation(scope, request.ConnectionID, request.Resource)
	if !present {
		return schemaRelationUnknownFailure()
	}
	candidate, err := store.LoadSchemaCandidate(ctx, tx, request.ConnectionID, string(request.SchemaRef))
	if errors.Is(err, store.ErrNotFound) {
		return schemaRelationUnknownFailure()
	}
	if err != nil {
		return err
	}
	snapshot, err := schema.DecodeSnapshot(candidate.Document)
	if err != nil || snapshot.Fingerprint != request.SchemaRef {
		return fmt.Errorf("%w: the stored candidate does not match its reference", schema.ErrSnapshotCorrupt)
	}
	if snapshot.Binding != declaredSnapshotBinding(scope, request.ConnectionID, declared) ||
		!snapshot.Completeness.Complete {
		return schemaRelationUnknownFailure()
	}
	return nil
}

// declaredSnapshotBinding is the snapshot binding that the current declaration
// of one relation gives.
func declaredSnapshotBinding(scope contract.Scope, connectionID string, declared DeclaredDiscovery) schema.Binding {
	return schema.Binding{
		OrgID:              scope.OrgID,
		WorkspaceID:        scope.WorkspaceID,
		ConnectorID:        declared.ConnectorID,
		ConnectionID:       connectionID,
		ProviderAccountID:  declared.ProviderAccountID,
		PermissionRevision: declared.PermissionRevision,
		Resource:           declared.Resource,
		SourceRevision:     declared.SourceRevision,
	}
}

// DeclaredRelation is one declared relation binding of a connection and the
// snapshot binding that it reads.
type DeclaredRelation struct {
	ConnectorID        string
	RelationBinding    string
	Resource           string
	ProviderAccountID  string
	PermissionRevision string
	SourceRevision     string
}

// RelationResolver resolves the declared relation binding of one connection
// that a PostgreSQL operation names. It reports false when the connection
// declares no such binding.
type RelationResolver func(scope contract.Scope, connectionID, relationBinding string) (DeclaredRelation, bool)

func schemaSelectionAbsentFailure() *Failure {
	return &Failure{HTTPStatus: StatusConflict, Code: "schema_selection_absent", RetryAction: "after_capability_change"}
}

// selectedRelationSchema returns the schema reference that a run of one
// PostgreSQL operation pins, in the admission transaction (ADR 0007, plan 285c
// section 2.7). It returns nil for another transport, and for a relation
// binding that the connection does not declare: then admission does not
// change, and pm-worker refuses as before. A declared relation with no
// selection, or with a selected candidate whose binding is not the current
// declaration, is refused with schema_selection_absent before any run.
func (service *AdmissionService) selectedRelationSchema(
	ctx context.Context,
	tx *store.Transaction,
	scope contract.Scope,
	connectionID string,
	operation contract.Operation,
) ([]string, error) {
	if operation.Transport.Kind != contract.TransportKindPostgreSQL || service.resolveRelation == nil {
		return nil, nil
	}
	declared, present := service.resolveRelation(scope, connectionID, operation.Transport.RelationBinding)
	if !present {
		return nil, nil
	}
	selection, err := store.LoadSchemaSelection(ctx, tx, connectionID, declared.Resource)
	if err != nil {
		return nil, err
	}
	if !selection.Present {
		return nil, schemaSelectionAbsentFailure()
	}
	candidate, err := store.LoadSchemaCandidate(ctx, tx, connectionID, selection.SchemaRef)
	if err != nil {
		return nil, fmt.Errorf("load the selected candidate: %w", err)
	}
	if candidate.Resource != declared.Resource || candidate.ConnectorID != declared.ConnectorID ||
		candidate.ProviderAccountID != declared.ProviderAccountID ||
		candidate.PermissionRevision != declared.PermissionRevision ||
		candidate.SourceRevision != declared.SourceRevision {
		return nil, schemaSelectionAbsentFailure()
	}
	return []string{selection.SchemaRef}, nil
}

// mergeSchemaRefs adds the pinned references to the references of the
// operation selection, as one sorted set.
func mergeSchemaRefs(selected []string, pinned []string) []string {
	if len(pinned) == 0 {
		return selected
	}
	merged := append(slices.Clone(selected), pinned...)
	slices.Sort(merged)
	return slices.Compact(merged)
}

// InspectSchemaSelection reads the current selection of one resource of an
// authorized connection and decodes its selected snapshot through the strict
// snapshot decoder. No selection is not an error: the view then has revision 1
// and no snapshot.
func (service *IdentityService) InspectSchemaSelection(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	connectionID string,
	resource string,
) (SchemaSelectionView, error) {
	if service == nil || ctx == nil || !validApplicationID(connectionID) || !validApplicationID(resource) {
		return SchemaSelectionView{}, invalidRequestFailure()
	}
	var view SchemaSelectionView
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		if _, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:        scope,
			Actor:        actor,
			Permission:   "connection.read",
			ResourceKind: auth.ResourceConnection,
			ResourceID:   connectionID,
		}); err != nil {
			return mapAuthorizationFailure(err)
		}
		selection, err := store.LoadSchemaSelection(ctx, tx, connectionID, resource)
		if err != nil {
			return err
		}
		view = SchemaSelectionView{Revision: selection.Revision}
		if !selection.Present {
			return nil
		}
		candidate, err := store.LoadSchemaCandidate(ctx, tx, connectionID, selection.SchemaRef)
		if err != nil {
			return fmt.Errorf("load the selected candidate: %w", err)
		}
		reference := contract.SemanticDigest(selection.SchemaRef)
		decoded, err := schema.DecodeSnapshot(candidate.Document)
		if err != nil || decoded.Fingerprint != reference {
			return fmt.Errorf("%w: the selected candidate does not match its reference", schema.ErrSnapshotCorrupt)
		}
		view.SchemaRef = reference
		view.Snapshot = &decoded
		return nil
	})
	if err != nil {
		return SchemaSelectionView{}, err
	}
	return view, nil
}
