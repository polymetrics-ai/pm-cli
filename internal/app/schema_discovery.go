package app

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	pmruntime "github.com/polymetrics-ai/pm-cli/internal/runtime"
	"github.com/polymetrics-ai/pm-cli/internal/schema"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// discoveryAdmissionOperation scopes the idempotency keys of discovery
// admissions apart from ordinary run admissions.
const discoveryAdmissionOperation = "schema.discover"

// ErrSchemaDiscoveryIncomplete reports a metadata observation that does not
// give one complete candidate. Nothing is stored for it.
var ErrSchemaDiscoveryIncomplete = errors.New("schema discovery is not a complete candidate")

// DiscoveryInvocation asks for one discovery of one declared relation.
type DiscoveryInvocation struct {
	ConnectionRef string
	Resource      string
	RequestKey    string
}

// DeclaredDiscovery is the declared relation of one connection that a
// discovery binds: the operator's relation binding and the connector's
// declared metadata operation for that binding. It needs no selected snapshot.
type DeclaredDiscovery struct {
	ConnectorID        string
	OperationID        string
	RelationBinding    string
	Resource           string
	ProviderAccountID  string
	PermissionRevision string
	SourceRevision     string
}

// DiscoveryResolver resolves the declared relation of one connection that
// names the resource. It reports false when no declared relation binding of
// that connection names it.
type DiscoveryResolver func(scope contract.Scope, connectionID, resource string) (DeclaredDiscovery, bool)

func schemaRelationUnknownFailure() *Failure {
	return &Failure{HTTPStatus: 404, Code: "schema_relation_unknown", RetryAction: "correct_input"}
}

// AcceptDiscovery admits one discovery run. It checks, in this order: the
// connection.manage permission, an idempotent replay, the declared relation
// scope (schema_relation_unknown), and the operation.invoke permission of the
// connector's metadata operation that the run executes. It then records the
// run and its discovery intent in one transaction. It never selects, and a
// refusal writes nothing.
func (service *AdmissionService) AcceptDiscovery(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	invocation DiscoveryInvocation,
) (store.AdmissionResult, error) {
	if service == nil || ctx == nil ||
		!validApplicationID(invocation.ConnectionRef) ||
		!validApplicationID(invocation.Resource) ||
		!validApplicationID(invocation.RequestKey) {
		return store.AdmissionResult{}, invalidRequestFailure()
	}
	if err := actor.Validate(); err != nil {
		return store.AdmissionResult{}, authenticationFailure()
	}
	if err := scope.Validate(); err != nil {
		return store.AdmissionResult{}, resourceNotFoundFailure(err)
	}

	var result store.AdmissionResult
	var committedFailure error
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		if err := store.BindAuthDeployment(ctx, tx, service.deploymentID); err != nil {
			return err
		}
		if _, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:        scope,
			Actor:        actor,
			Permission:   "connection.manage",
			ResourceKind: auth.ResourceConnection,
			ResourceID:   invocation.ConnectionRef,
		}); err != nil {
			return mapAuthorizationFailure(err)
		}
		intentDigest, err := canonicalIntent(struct {
			Operation     string         `json:"operation"`
			Scope         contract.Scope `json:"scope"`
			Actor         contract.Actor `json:"actor"`
			ConnectionRef string         `json:"connection_ref"`
			Resource      string         `json:"resource"`
		}{discoveryAdmissionOperation, scope, actor, invocation.ConnectionRef, invocation.Resource})
		if err != nil {
			return invalidRequestFailure()
		}
		key := store.AdmissionKey{
			PrincipalID:    actor.RequestedActor.PrincipalID,
			OperationID:    discoveryAdmissionOperation,
			TargetID:       scope.WorkspaceID,
			IdempotencyKey: invocation.RequestKey,
		}
		replayed, found, err := store.ResolveAdmissionReplay(ctx, tx, key, intentDigest)
		if err != nil {
			return mapAdmissionFailure(err)
		}
		if found {
			compacted, err := store.CompactAdmissionReceipt(ctx, tx, key)
			if err != nil {
				return mapAdmissionFailure(err)
			}
			if compacted {
				committedFailure = admissionReceiptExpiredFailure(replayed.RunID, store.ErrAdmissionReceiptExpired)
				return nil
			}
			result = replayed
			return nil
		}

		if service.resolveDiscovery == nil {
			return schemaRelationUnknownFailure()
		}
		declared, present := service.resolveDiscovery(scope, invocation.ConnectionRef, invocation.Resource)
		if !present {
			return schemaRelationUnknownFailure()
		}
		if service.resolveOperation == nil {
			return capabilityUnavailableFailure(errors.New("operation resolver is unavailable"))
		}
		selection, err := service.resolveOperation(ctx, scope, invocation.ConnectionRef, declared.OperationID)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			return capabilityUnavailableFailure(err)
		}
		selection, err = normalizeOperationSelection(selection)
		if err != nil || !discoveryOperation(selection.Operation, declared) {
			return capabilityUnavailableFailure(errors.New("the declared metadata operation is unavailable"))
		}
		selectionRevision, err := OperationSelectionRevision(selection)
		if err != nil {
			return capabilityUnavailableFailure(err)
		}
		authorization, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:           scope,
			Actor:           actor,
			Permission:      "operation.invoke",
			ResourceKind:    auth.ResourceConnection,
			ResourceID:      invocation.ConnectionRef,
			ConnectionID:    invocation.ConnectionRef,
			OperationID:     declared.OperationID,
			AutomationScope: actor.RequestedActor.Kind == contract.PrincipalServiceAccount,
		})
		if err != nil {
			return mapAuthorizationFailure(err)
		}
		run := RunInvocation{
			ConnectionRef:    invocation.ConnectionRef,
			OperationRef:     declared.OperationID,
			ExpectedRevision: selectionRevision,
			RequestKey:       invocation.RequestKey,
			Lane:             contract.LaneDirectRead,
			Input:            contract.NewObjectValue(map[string]contract.PresentValue{}),
			ResultControls:   contract.ResultControls{},
			ApprovalRef:      contract.AbsentValue(),
		}
		if err := contract.ValidateOperationInput(selection.Operation, run.Input); err != nil {
			return capabilityUnavailableFailure(err)
		}
		if _, err := service.registry.Resolve(selection.Operation, run.Lane); err != nil {
			return mapHandlerResolutionFailure(err)
		}
		policy, err := service.policyForOperation(scope, invocation.ConnectionRef, selection.Operation)
		if err != nil {
			return err
		}
		now, err := store.ReadAuthenticationTime(ctx, tx)
		if err != nil {
			return err
		}
		expiresAt := now.Add(service.policy.RunLifetime).UTC()
		if authorization.ExpiresAt.Before(expiresAt) {
			expiresAt = authorization.ExpiresAt.UTC()
		}
		if !expiresAt.After(now) {
			return mapAuthorizationFailure(auth.ErrGrantUnavailable)
		}
		request, err := service.newAdmissionRequest(
			scope, actor, run, selection, policy, selectionRevision, intentDigest, authorization, expiresAt, key,
		)
		if err != nil {
			return err
		}
		result, err = store.AdmitRun(ctx, tx, request)
		if err != nil {
			return mapAdmissionFailure(err)
		}
		if err := store.RecordSchemaDiscovery(ctx, tx, store.SchemaDiscovery{
			RunID:              request.Spec.RunID,
			ConnectionID:       invocation.ConnectionRef,
			ConnectorID:        declared.ConnectorID,
			Resource:           declared.Resource,
			RelationBinding:    declared.RelationBinding,
			ProviderAccountID:  declared.ProviderAccountID,
			PermissionRevision: declared.PermissionRevision,
			SourceRevision:     declared.SourceRevision,
		}); err != nil {
			return mapAdmissionFailure(err)
		}
		return nil
	})
	if err != nil {
		return store.AdmissionResult{}, mapAdmissionFailure(err)
	}
	if committedFailure != nil {
		return store.AdmissionResult{}, committedFailure
	}
	return result, nil
}

// discoveryOperation reports whether one resolved operation is the declared
// PostgreSQL metadata operation of the declared relation binding.
func discoveryOperation(operation contract.Operation, declared DeclaredDiscovery) bool {
	return operation.OperationID == declared.OperationID &&
		operation.Transport.Kind == contract.TransportKindPostgreSQL &&
		operation.Transport.StatementKind == contract.PostgreSQLStatementMetadata &&
		operation.Transport.RelationBinding == declared.RelationBinding
}

// discoveryBinding is the snapshot binding of one discovery intent.
func discoveryBinding(scope contract.Scope, discovery store.SchemaDiscovery) schema.Binding {
	return schema.Binding{
		OrgID:              scope.OrgID,
		WorkspaceID:        scope.WorkspaceID,
		ConnectorID:        discovery.ConnectorID,
		ConnectionID:       discovery.ConnectionID,
		ProviderAccountID:  discovery.ProviderAccountID,
		PermissionRevision: discovery.PermissionRevision,
		Resource:           discovery.Resource,
		SourceRevision:     discovery.SourceRevision,
	}
}

var sqlNumericTypeName = regexp.MustCompile(`^numeric\(([1-9][0-9]{0,3}),(0|[1-9][0-9]{0,3})\)$`)

// discoverRelation normalizes the rows of one declared metadata statement into
// one candidate of the binding. Each row names one live column and its exact
// PostgreSQL type. A type with an exact transport kind becomes one typed field.
// Any other type stays an explicit unsupported shape, as the schema package
// decides. A relation with no live columns is an unavailable observation, so
// the candidate is not complete.
func discoverRelation(binding schema.Binding, capability string, rows []pmruntime.SQLRow) (schema.Candidate, error) {
	observation := schema.Observation{
		Capability:        capability,
		Resource:          binding.Resource,
		Available:         len(rows) != 0,
		DeniedPermissions: []string{},
		Unsupported:       []string{},
		Fields:            []schema.Field{},
	}
	for index, row := range rows {
		name, kind, err := sqlMetadataRow(row)
		if err != nil {
			return schema.Candidate{}, fmt.Errorf("metadata row %d: %w", index, err)
		}
		field, supported := discoveredField(name, kind)
		if !supported {
			observation.Unsupported = append(observation.Unsupported, name)
			continue
		}
		observation.Fields = append(observation.Fields, field)
	}
	return schema.Discover(binding, []schema.Observation{observation})
}

func sqlMetadataRow(row pmruntime.SQLRow) (string, string, error) {
	if err := row.Validate(); err != nil {
		return "", "", err
	}
	nameValue, nameOK := row.Value(pmruntime.SQLMetadataColumnName)
	typeValue, typeOK := row.Value(pmruntime.SQLMetadataColumnType)
	if !nameOK || !typeOK {
		return "", "", errors.New("the row does not name a column and its type")
	}
	name, nameOK := nameValue.Text()
	kind, typeOK := typeValue.Text()
	if !nameOK || !typeOK || name == "" || kind == "" {
		return "", "", errors.New("the column name and type must be text")
	}
	return name, kind, nil
}

// discoveredField maps one live PostgreSQL type onto the typed field that the
// read path binds back to exactly the same type (sqlColumnKind). The metadata
// statement observes names and types only, so requiredness and writability
// are not declared.
func discoveredField(name, kind string) (schema.Field, bool) {
	field := schema.Field{Name: name, Pointer: "/columns/" + escapeJSONPointer(name)}
	switch kind {
	case "text":
		field.Schema = contract.Schema{Type: contract.SchemaTypeString}
	case "bigint":
		field.Schema = contract.Schema{Type: contract.SchemaTypeInteger}
	case "boolean":
		field.Schema = contract.Schema{Type: contract.SchemaTypeBool}
	case "timestamp with time zone":
		field.Schema = contract.Schema{Type: contract.SchemaTypeTimestampInstant}
	case "timestamp without time zone":
		field.Schema = contract.Schema{Type: contract.SchemaTypeTimestampLocal}
	default:
		match := sqlNumericTypeName.FindStringSubmatch(kind)
		if match == nil {
			return schema.Field{}, false
		}
		precision, precisionErr := strconv.Atoi(match[1])
		scale, scaleErr := strconv.Atoi(match[2])
		if precisionErr != nil || scaleErr != nil || scale > precision {
			return schema.Field{}, false
		}
		field.Schema = contract.Schema{Type: contract.SchemaTypeDecimal}
		field.Precision = precision
		field.Scale = scale
	}
	if field.Validate() != nil {
		return schema.Field{}, false
	}
	return field, true
}

func escapeJSONPointer(token string) string {
	return strings.ReplaceAll(strings.ReplaceAll(token, "~", "~0"), "/", "~1")
}

// settledDiscoveryCandidate returns the candidate that a settlement stores.
// Only a discovery whose every member succeeded, whose callback reported no
// error, and whose observed snapshot is complete and bound to the accepted
// intent gives a candidate. A failed, partial, unsent or unknown discovery
// stores nothing.
func settledDiscoveryCandidate(
	scope contract.Scope,
	discovery store.SchemaDiscovery,
	observed *schema.Snapshot,
	outcomes []contract.MemberOutcome,
	callbackErr error,
) (store.SchemaCandidate, bool, error) {
	if observed == nil || callbackErr != nil || len(outcomes) == 0 {
		return store.SchemaCandidate{}, false, nil
	}
	for _, outcome := range outcomes {
		if outcome.State != contract.OutcomeSucceeded {
			return store.SchemaCandidate{}, false, nil
		}
	}
	if err := observed.Validate(); err != nil {
		return store.SchemaCandidate{}, false, err
	}
	if !observed.Completeness.Complete {
		return store.SchemaCandidate{}, false, nil
	}
	if observed.Binding != discoveryBinding(scope, discovery) {
		return store.SchemaCandidate{}, false, errors.New("the observed snapshot is bound to another scope than the discovery")
	}
	document, err := schema.EncodeSnapshot(*observed)
	if err != nil {
		return store.SchemaCandidate{}, false, err
	}
	return store.SchemaCandidate{
		ConnectionID:       discovery.ConnectionID,
		SchemaRef:          string(observed.Fingerprint),
		ConnectorID:        discovery.ConnectorID,
		ProviderAccountID:  discovery.ProviderAccountID,
		PermissionRevision: discovery.PermissionRevision,
		Resource:           discovery.Resource,
		SourceRevision:     discovery.SourceRevision,
		FieldCount:         len(observed.Fields),
		Document:           document,
		DiscoveredByRunID:  discovery.RunID,
	}, true, nil
}

// InspectSchemaCandidate reads one stored candidate snapshot of an authorized
// connection by its content digest. The stored document is decoded through the
// strict snapshot decoder, which recomputes its digest.
func (service *IdentityService) InspectSchemaCandidate(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	connectionID string,
	schemaRef contract.SemanticDigest,
) (schema.Snapshot, error) {
	if service == nil || ctx == nil || !validApplicationID(connectionID) || schemaRef.Validate() != nil {
		return schema.Snapshot{}, invalidRequestFailure()
	}
	var snapshot schema.Snapshot
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
		candidate, err := store.LoadSchemaCandidate(ctx, tx, connectionID, string(schemaRef))
		if errors.Is(err, store.ErrNotFound) {
			return resourceNotFoundFailure(err)
		}
		if err != nil {
			return err
		}
		decoded, err := schema.DecodeSnapshot(candidate.Document)
		if err != nil || decoded.Fingerprint != schemaRef {
			return fmt.Errorf("%w: the stored candidate does not match its reference", schema.ErrSnapshotCorrupt)
		}
		snapshot = decoded
		return nil
	})
	if err != nil {
		return schema.Snapshot{}, err
	}
	return snapshot, nil
}
