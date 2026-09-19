package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/puddle/v2"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

const admissionOperation = "run.accept"

type RunInvocation struct {
	ConnectionRef    string
	OperationRef     string
	ExpectedRevision string
	RequestKey       string
	Lane             contract.Lane
	Input            contract.PresentValue
	ResultControls   contract.ResultControls
	ApprovalRef      contract.PresentValue
}

type OperationSelection struct {
	Operation  contract.Operation
	SchemaRefs []string
}

type OperationResolver func(
	context.Context,
	contract.Scope,
	string,
	string,
) (OperationSelection, error)

// HTTPPolicyResolver selects already-resolved server controls for one exact
// connection. Admission invokes it only after current resource authorization.
type HTTPPolicyResolver func(contract.Scope, string) (contract.HTTPExecutionPolicy, bool)

type AdmissionServiceConfig struct {
	Pool              *store.Pool
	DeploymentID      string
	Authorize         auth.AuthorizationCallback
	Registry          *HandlerRegistry
	ResolveOperation  OperationResolver
	ResolveHTTPPolicy HTTPPolicyResolver
	Policy            store.RunBudgetPolicy
	Random            io.Reader
}

// AdmissionService owns authenticated acceptance only. Handler resolution proves
// local reachability, but admission never invokes a provider callback.
type AdmissionService struct {
	pool              *store.Pool
	deploymentID      string
	authorize         auth.AuthorizationCallback
	registry          *HandlerRegistry
	resolveOperation  OperationResolver
	resolveHTTPPolicy HTTPPolicyResolver
	policy            store.RunBudgetPolicy
	random            io.Reader
}

func NewAdmissionService(configuration AdmissionServiceConfig) (*AdmissionService, error) {
	if configuration.Pool == nil {
		return nil, errors.New("admission service store pool is required")
	}
	if !validApplicationID(configuration.DeploymentID) {
		return nil, errors.New("admission service deployment identity is invalid")
	}
	if configuration.Authorize == nil {
		return nil, errors.New("admission service current authorization callback is required")
	}
	if err := configuration.Policy.Validate(); err != nil {
		return nil, errors.New("admission service run budget policy is invalid")
	}
	if configuration.Policy.HTTP != nil {
		return nil, errors.New("HTTP policy must be selected for the authorized connection")
	}
	if configuration.Registry == nil {
		configuration.Registry = &HandlerRegistry{}
	}
	if configuration.Random == nil {
		configuration.Random = rand.Reader
	}
	return &AdmissionService{
		pool:              configuration.Pool,
		deploymentID:      configuration.DeploymentID,
		authorize:         configuration.Authorize,
		registry:          configuration.Registry,
		resolveOperation:  configuration.ResolveOperation,
		resolveHTTPPolicy: configuration.ResolveHTTPPolicy,
		policy:            configuration.Policy,
		random:            configuration.Random,
	}, nil
}

// OperationSelectionRevision binds the complete selected operation descriptor
// and the exact selected schema revisions. Schema references are normalized as
// an unordered immutable set before hashing.
func OperationSelectionRevision(selection OperationSelection) (string, error) {
	normalized, err := normalizeOperationSelection(selection)
	if err != nil {
		return "", err
	}
	digest, err := canonicalIntent(struct {
		Operation  contract.Operation `json:"operation"`
		SchemaRefs []string           `json:"schema_refs"`
	}{
		Operation:  normalized.Operation,
		SchemaRefs: normalized.SchemaRefs,
	})
	if err != nil {
		return "", err
	}
	return string(digest), nil
}

func (service *AdmissionService) policyForOperation(scope contract.Scope, connectionID string, operation contract.Operation) (store.RunBudgetPolicy, error) {
	policy := service.policy
	if operation.Execution == nil || operation.Execution.HTTP == nil {
		return policy, nil
	}
	if service.resolveHTTPPolicy == nil {
		return store.RunBudgetPolicy{}, capabilityUnavailableFailure(errors.New("provider HTTP policy is unavailable"))
	}
	httpPolicy, configured := service.resolveHTTPPolicy(scope, connectionID)
	if !configured {
		return store.RunBudgetPolicy{}, capabilityUnavailableFailure(errors.New("connection HTTP policy is unavailable"))
	}
	if err := httpPolicy.ValidateForOperation(operation); err != nil {
		return store.RunBudgetPolicy{}, capabilityUnavailableFailure(err)
	}
	policy.HTTP = &httpPolicy
	return policy, nil
}

func RunPolicyFromConfiguration(configuration config.Configuration) (store.RunBudgetPolicy, error) {
	admission := configuration.Admission()
	limits := configuration.Limits()
	policy := store.RunBudgetPolicy{
		Pending: store.AdmissionLimits{
			GlobalPending:          admission.GlobalPending,
			WorkspacePending:       admission.WorkspacePending,
			WorkspaceMaxPendingAge: admission.WorkspaceMaxPendingAge,
		},
		RunLifetime:          limits.RunLifetime,
		InputMaxBytes:        limits.RequestBytes,
		WorkspaceActiveRuns:  limits.WorkspaceActiveRuns,
		WorkflowPayloadBytes: limits.WorkflowPayloadBytes,
		WorkflowFanout:       limits.WorkflowFanout,
	}
	if err := policy.Validate(); err != nil {
		return store.RunBudgetPolicy{}, err
	}
	return policy, nil
}

func (service *AdmissionService) AcceptRun(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	invocation RunInvocation,
) (store.AdmissionResult, error) {
	if service == nil || ctx == nil ||
		!validApplicationID(invocation.ConnectionRef) ||
		!validApplicationOpaque(invocation.OperationRef, 1024) ||
		!validApplicationID(invocation.ExpectedRevision) ||
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
		authorization, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:           scope,
			Actor:           actor,
			Permission:      "operation.invoke",
			ResourceKind:    auth.ResourceConnection,
			ResourceID:      invocation.ConnectionRef,
			ConnectionID:    invocation.ConnectionRef,
			OperationID:     invocation.OperationRef,
			AutomationScope: actor.RequestedActor.Kind == contract.PrincipalServiceAccount,
		})
		if err != nil {
			return mapAuthorizationFailure(err)
		}
		if err := validateInvocationValues(invocation); err != nil {
			return invalidRequestFailure()
		}
		intentDigest, err := admissionIntentDigest(scope, actor, invocation)
		if err != nil {
			return invalidRequestFailure()
		}
		key := store.AdmissionKey{
			PrincipalID:    actor.RequestedActor.PrincipalID,
			OperationID:    admissionOperation,
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

		if service.resolveOperation == nil {
			return capabilityUnavailableFailure(errors.New("operation resolver is unavailable"))
		}
		selection, err := service.resolveOperation(ctx, scope, invocation.ConnectionRef, invocation.OperationRef)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			return capabilityUnavailableFailure(err)
		}
		selection, err = normalizeOperationSelection(selection)
		if err != nil || selection.Operation.OperationID != invocation.OperationRef {
			return capabilityUnavailableFailure(err)
		}
		selectionRevision, err := OperationSelectionRevision(selection)
		if err != nil {
			return capabilityUnavailableFailure(err)
		}
		if selectionRevision != invocation.ExpectedRevision {
			return specificationStaleFailure()
		}
		hasContinuation := invocation.ResultControls.HasContinuation()
		if !hasContinuation {
			if err := contract.ValidateOperationInput(selection.Operation, invocation.Input); err != nil {
				return invalidRequestFailure()
			}
		}
		if _, err := service.registry.Resolve(selection.Operation, invocation.Lane); err != nil {
			return mapHandlerResolutionFailure(err)
		}
		policy, err := service.policyForOperation(scope, invocation.ConnectionRef, selection.Operation)
		if err != nil {
			return err
		}
		if hasContinuation {
			invocation, err = restoreContinuationInvocation(ctx, tx, invocation, selection.Operation, policy)
			if err != nil {
				return err
			}
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
			scope,
			actor,
			invocation,
			selection,
			policy,
			selectionRevision,
			intentDigest,
			authorization,
			expiresAt,
			key,
		)
		if err != nil {
			return err
		}
		result, err = store.AdmitRun(ctx, tx, request)
		if err != nil {
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

func (service *AdmissionService) newAdmissionRequest(
	scope contract.Scope,
	actor contract.Actor,
	invocation RunInvocation,
	selection OperationSelection,
	policy store.RunBudgetPolicy,
	selectionRevision string,
	intentDigest contract.SemanticDigest,
	authorization auth.Authorization,
	expiresAt time.Time,
	key store.AdmissionKey,
) (store.AdmissionRequest, error) {
	runID, err := randomRunUUID(service.random)
	if err != nil {
		return store.AdmissionRequest{}, err
	}
	grantRef, err := randomApplicationID(service.random, "grant-", 18)
	if err != nil {
		return store.AdmissionRequest{}, err
	}
	inputRef, err := randomApplicationID(service.random, "input-", 18)
	if err != nil {
		return store.AdmissionRequest{}, err
	}
	budgetRef, err := randomApplicationID(service.random, "budget-", 18)
	if err != nil {
		return store.AdmissionRequest{}, err
	}
	receiptID, err := randomApplicationID(service.random, "receipt-", 18)
	if err != nil {
		return store.AdmissionRequest{}, err
	}
	auditID, err := randomApplicationID(service.random, "audit-", 18)
	if err != nil {
		return store.AdmissionRequest{}, err
	}
	eventID, err := randomApplicationID(service.random, "event-", 18)
	if err != nil {
		return store.AdmissionRequest{}, err
	}
	commandID, err := randomApplicationID(service.random, "command-", 18)
	if err != nil {
		return store.AdmissionRequest{}, err
	}

	budget, err := store.NewRunBudgetSnapshot(scope, runID, budgetRef, policy)
	if err != nil {
		return store.AdmissionRequest{}, err
	}
	// The client-supplied approval reference is immutable invocation context,
	// not proof of approval authority. Only an approval owner can add a grant
	// approval reference together with its policy epoch.
	grant := contract.ExecutionGrant{
		GrantRef:      grantRef,
		GrantEpoch:    1,
		Scope:         scope,
		Actor:         actor,
		ConnectionIDs: []string{invocation.ConnectionRef},
		OperationIDs:  []string{invocation.OperationRef},
		ApprovalRefs:  []string{},
		ExpiresAt:     expiresAt,
		PolicyEpochs:  authorization.PolicyEpochs,
	}
	spec := contract.RunSpec{
		RunID:             runID,
		OrgID:             scope.OrgID,
		WorkspaceID:       scope.WorkspaceID,
		Actor:             actor,
		ExecutionGrantRef: grantRef,
		Origin:            contract.RunOriginAPI,
		OperationOrPlanRef: contract.OperationOrPlanRef{
			Kind:     contract.OperationReference,
			ID:       invocation.OperationRef,
			Revision: selectionRevision,
		},
		InputRefs:              []string{inputRef},
		SchemaRefs:             slices.Clone(selection.SchemaRefs),
		CapabilityVersions:     slices.Clone(selection.Operation.CapabilityRequirements),
		Budget:                 budget.Binding(),
		IdempotencyFingerprint: intentDigest,
	}
	workflow, err := contract.NewAPIWorkflowBinding(runID)
	if err != nil {
		return store.AdmissionRequest{}, err
	}
	safe := contract.SafePayload{
		References:  []contract.SafeReference{{Name: "run", Ref: runID}},
		Counts:      []contract.SafeCount{},
		ReasonCodes: []string{"accepted"},
	}
	return store.AdmissionRequest{
		Key:            key,
		BudgetSnapshot: budget,
		Spec:           spec,
		Grant:          grant,
		InputSnapshot: store.RunInputSnapshot{
			Scope:          scope,
			RunID:          runID,
			InputRef:       inputRef,
			Lane:           invocation.Lane,
			ConnectionID:   invocation.ConnectionRef,
			Input:          invocation.Input,
			ResultControls: invocation.ResultControls,
			ApprovalRef:    invocation.ApprovalRef,
		},
		WorkflowBinding: workflow,
		Receipt: store.Receipt{
			ReceiptID: receiptID,
			Kind:      "run_admission",
			RunID:     runID,
			Payload:   safe,
		},
		Audit: store.AuditEntry{
			AuditID:   auditID,
			Action:    "run_accepted",
			TargetRef: runID,
			Payload:   safe,
		},
		Event: store.EventInput{
			EventID: eventID,
			Type:    "run_accepted",
			Payload: safe,
		},
		Outbox: store.OutboxCommand{
			CommandID:  commandID,
			Kind:       "start_run",
			PayloadRef: runID,
		},
	}, nil
}

func normalizeOperationSelection(selection OperationSelection) (OperationSelection, error) {
	if selection.SchemaRefs == nil {
		return OperationSelection{}, errors.New("operation schema revisions must be explicitly present")
	}
	schemaRefs := slices.Clone(selection.SchemaRefs)
	slices.Sort(schemaRefs)
	for index, reference := range schemaRefs {
		if !validApplicationID(reference) || index > 0 && schemaRefs[index-1] == reference {
			return OperationSelection{}, errors.New("operation schema revisions contain an invalid or duplicate identity")
		}
	}
	foundations := shapeFoundations(foundationReferences(selection.Operation.CapabilityRequirements))
	if err := selection.Operation.Validate(foundations); err != nil {
		return OperationSelection{}, err
	}
	selection.SchemaRefs = schemaRefs
	return selection, nil
}

func validateInvocationValues(invocation RunInvocation) error {
	if err := invocation.Input.Validate(); err != nil {
		return err
	}
	if err := invocation.ResultControls.Validate(); err != nil {
		return err
	}
	switch invocation.ApprovalRef.Presence() {
	case contract.PresenceAbsent, contract.PresenceNull:
		return nil
	case contract.PresenceValue:
		value, ok := invocation.ApprovalRef.TextValue()
		if !ok || !validApplicationID(value) {
			return errors.New("approval reference must be an opaque string identity")
		}
		return nil
	default:
		return errors.New("approval reference presence is invalid")
	}
}

func admissionIntentDigest(
	scope contract.Scope,
	actor contract.Actor,
	invocation RunInvocation,
) (contract.SemanticDigest, error) {
	return canonicalIntent(struct {
		Operation        string                  `json:"operation"`
		Scope            contract.Scope          `json:"scope"`
		Actor            contract.Actor          `json:"actor"`
		ConnectionRef    string                  `json:"connection_ref"`
		OperationRef     string                  `json:"operation_ref"`
		ExpectedRevision string                  `json:"expected_revision"`
		Lane             contract.Lane           `json:"lane"`
		Input            contract.PresentValue   `json:"input"`
		ResultControls   contract.ResultControls `json:"result_controls"`
		ApprovalRef      contract.PresentValue   `json:"approval_ref"`
	}{
		Operation:        admissionOperation,
		Scope:            scope,
		Actor:            actor,
		ConnectionRef:    invocation.ConnectionRef,
		OperationRef:     invocation.OperationRef,
		ExpectedRevision: invocation.ExpectedRevision,
		Lane:             invocation.Lane,
		Input:            invocation.Input,
		ResultControls:   invocation.ResultControls,
		ApprovalRef:      invocation.ApprovalRef,
	})
}

func randomRunUUID(reader io.Reader) (string, error) {
	if reader == nil {
		return "", errors.New("run identity generator is invalid")
	}
	var raw [16]byte
	if _, err := io.ReadFull(reader, raw[:]); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

func mapHandlerResolutionFailure(err error) error {
	switch {
	case errors.Is(err, ErrHandlerIncompatible), errors.Is(err, contract.ErrFoundationIncompatible):
		return &Failure{HTTPStatus: 409, Code: "foundation_incompatible", RetryAction: "after_capability_change", cause: err}
	case errors.Is(err, ErrHandlerUnavailable), errors.Is(err, ErrHandlerConflict), errors.Is(err, ErrHandlerInvalid):
		return capabilityUnavailableFailure(err)
	default:
		return err
	}
}

func capabilityUnavailableFailure(cause error) *Failure {
	return &Failure{HTTPStatus: 503, Code: "capability_unavailable", RetryAction: "after_capability_change", cause: cause}
}

func specificationStaleFailure() *Failure {
	return &Failure{HTTPStatus: 409, Code: "specification_stale", RetryAction: "refresh_specification"}
}

func admissionReceiptExpiredFailure(existingRunID string, cause error) *Failure {
	return &Failure{
		HTTPStatus:        410,
		Code:              "retry_receipt_expired",
		RetryAction:       "inspect_existing_result",
		ExistingResultRef: existingRunID,
		cause:             cause,
	}
}

func mapAdmissionFailure(err error) error {
	if err == nil {
		return nil
	}
	var failure *Failure
	if errors.As(err, &failure) {
		return err
	}
	var expired *store.AdmissionReceiptExpiredError
	var conflict *store.IdempotencyConflictError
	var postgresError *pgconn.PgError
	switch {
	case errors.As(err, &expired):
		return admissionReceiptExpiredFailure(expired.ExistingRunID, err)
	case errors.As(err, &conflict), errors.Is(err, store.ErrIdempotencyConflict):
		existing := ""
		if conflict != nil {
			existing = conflict.ExistingRunID
		}
		return &Failure{HTTPStatus: 409, Code: "idempotency_conflict", RetryAction: "new_key_for_new_intent", ExistingResultRef: existing, cause: err}
	case errors.Is(err, store.ErrWorkspaceAdmissionCountLimit):
		return &Failure{HTTPStatus: 429, Code: "workspace_admission_count_limit", RetryAction: "after_backlog_progress", cause: err}
	case errors.Is(err, store.ErrWorkspaceAdmissionAgeLimit):
		return &Failure{HTTPStatus: 429, Code: "workspace_admission_age_limit", RetryAction: "after_backlog_progress", cause: err}
	case errors.Is(err, store.ErrDeploymentAdmissionOverload):
		return &Failure{HTTPStatus: 503, Code: "deployment_admission_overload", RetryAction: "after_backlog_progress", cause: err}
	case errors.Is(err, store.ErrRunInputTooLarge), errors.Is(err, store.ErrRunInputBoundInvalid):
		return invalidRequestFailure()
	case errors.Is(err, store.ErrRunGrantUnavailable):
		return mapAuthorizationFailure(auth.ErrGrantUnavailable)
	case errors.Is(err, store.ErrCommitOutcomeUnknown):
		return &Failure{HTTPStatus: 503, Code: "admission_outcome_unknown", RetryAction: "same_key_reconcile_only", cause: err}
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrScopeMismatch):
		return resourceNotFoundFailure(err)
	case errors.As(err, &postgresError), errors.Is(err, puddle.ErrClosedPool),
		pgconn.SafeToRetry(err), errors.Is(err, context.DeadlineExceeded):
		return &Failure{HTTPStatus: 503, Code: "control_store_unavailable", RetryAction: "same_key_after_dependency_recovery", cause: err}
	default:
		return err
	}
}
