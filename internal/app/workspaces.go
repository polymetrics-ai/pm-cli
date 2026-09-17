package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

const (
	StatusOK                   = 200
	StatusCreated              = 201
	StatusConflict             = 409
	StatusGone                 = 410
	StatusPreconditionFailed   = 412
	StatusPreconditionRequired = 428
)

const minimumMutationReceiptRetention = 7 * 24 * time.Hour

type Failure struct {
	HTTPStatus        int
	Code              string
	RetryAction       string
	ExistingResultRef string
	cause             error
}

func (failure *Failure) Error() string {
	if failure == nil {
		return "<nil>"
	}
	return failure.Code
}

func (failure *Failure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.cause
}

type IdentityServiceConfig struct {
	Pool             *store.Pool
	DeploymentID     string
	Authorize        auth.AuthorizationCallback
	Random           io.Reader
	ReceiptRetention time.Duration
}

// IdentityService is the bounded owner of workspace, connection, membership,
// service-account and delegation use cases. It has no provider execution path.
type IdentityService struct {
	pool             *store.Pool
	deploymentID     string
	authorize        auth.AuthorizationCallback
	random           io.Reader
	receiptRetention time.Duration
}

func NewIdentityService(configuration IdentityServiceConfig) (*IdentityService, error) {
	if configuration.Pool == nil {
		return nil, errors.New("identity service store pool is required")
	}
	if configuration.Authorize == nil {
		return nil, errors.New("identity service current authorization callback is required")
	}
	if !validApplicationID(configuration.DeploymentID) {
		return nil, errors.New("identity service deployment identity is invalid")
	}
	if configuration.Random == nil {
		configuration.Random = rand.Reader
	}
	if configuration.ReceiptRetention < minimumMutationReceiptRetention ||
		configuration.ReceiptRetention > 365*24*time.Hour {
		return nil, errors.New("identity mutation receipt retention must be between seven and 365 days")
	}
	return &IdentityService{
		pool:             configuration.Pool,
		deploymentID:     configuration.DeploymentID,
		authorize:        configuration.Authorize,
		random:           configuration.Random,
		receiptRetention: configuration.ReceiptRetention,
	}, nil
}

type Workspace struct {
	Scope              contract.Scope
	Name               string
	MembershipID       string
	Role               string
	MembershipRevision uint64
}

type WorkspacePage struct {
	Items      []Workspace
	NextCursor string
}

func (service *IdentityService) ListWorkspaces(
	ctx context.Context,
	principal contract.Principal,
	afterWorkspaceID string,
	pageSize uint32,
) (WorkspacePage, error) {
	if service == nil || ctx == nil || pageSize == 0 || pageSize > 100 {
		return WorkspacePage{}, invalidRequestFailure()
	}
	if err := principal.Validate(); err != nil || principal.Kind != contract.PrincipalUser {
		return WorkspacePage{}, authenticationFailure()
	}
	queryLimit := pageSize + 1
	var stored []store.AuthorizedWorkspace
	err := store.WithinPrincipalScope(ctx, service.pool, principal, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		stored, err = store.ListAuthorizedWorkspaces(ctx, tx, principal, afterWorkspaceID, queryLimit)
		return err
	})
	if err != nil {
		return WorkspacePage{}, err
	}
	page := WorkspacePage{Items: make([]Workspace, 0, min(len(stored), int(pageSize)))}
	for index, item := range stored {
		if index == int(pageSize) {
			page.NextCursor = page.Items[len(page.Items)-1].Scope.WorkspaceID
			break
		}
		page.Items = append(page.Items, Workspace{
			Scope:              item.Scope,
			Name:               item.Name,
			MembershipID:       item.MembershipID,
			Role:               item.Role,
			MembershipRevision: item.MembershipRevision,
		})
	}
	return page, nil
}

func (service *IdentityService) InspectWorkspace(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
) (Workspace, error) {
	if service == nil || ctx == nil {
		return Workspace{}, invalidRequestFailure()
	}
	var result Workspace
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		if _, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:        scope,
			Actor:        actor,
			Permission:   "workspace.read",
			ResourceKind: auth.ResourceWorkspace,
			ResourceID:   scope.WorkspaceID,
		}); err != nil {
			return mapAuthorizationFailure(err)
		}
		workspace, err := store.LoadWorkspace(ctx, tx)
		if err != nil {
			return err
		}
		membership, err := store.LoadMembershipAuthority(ctx, tx, actor.EffectiveActor.PrincipalID)
		if err != nil {
			return err
		}
		result = Workspace{
			Scope:              scope,
			Name:               workspace.Identity.Name,
			MembershipID:       membership.MembershipID,
			Role:               membership.Role,
			MembershipRevision: membership.ControlRevision,
		}
		return nil
	})
	return result, err
}

type MutationControl struct {
	IdempotencyKey   string
	IntentRevision   uint64
	ExpectedRevision *uint64
}

type MutationMetadata struct {
	ResultRef       string
	Revision        uint64
	ControlRevision uint64
	Status          int
	Replayed        bool
	Unchanged       bool
}

type Membership struct {
	MembershipID     string
	Principal        contract.Principal
	Role             string
	ResourceGrants   []string
	Permissions      []string
	ConnectionIDs    []string
	OperationIDs     []string
	BoundScopeDigest contract.SemanticDigest
	PolicyEpoch      uint64
	ControlRevision  uint64
	ExpiresAt        *time.Time
	RevokedAt        *time.Time
}

type MembershipPage struct {
	Items              []Membership
	CollectionRevision uint64
	NextCursor         string
}

type GrantMembership struct {
	Principal      contract.Principal
	Role           string
	ResourceGrants []string
	ExpiresAt      *time.Time
	Control        MutationControl
}

type RevokeMembership struct {
	MembershipID     string
	BoundScopeDigest contract.SemanticDigest
	ExpiresAt        *time.Time
	Control          MutationControl
}

type MembershipMutationResult struct {
	Membership Membership
	Mutation   MutationMetadata
}

func (service *IdentityService) ListMemberships(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	afterID string,
	pageSize uint32,
) (MembershipPage, error) {
	if service == nil || ctx == nil || pageSize == 0 || pageSize > 100 {
		return MembershipPage{}, invalidRequestFailure()
	}
	var result MembershipPage
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		if _, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:        scope,
			Actor:        actor,
			Permission:   "workspace.access.read",
			ResourceKind: auth.ResourceWorkspace,
			ResourceID:   scope.WorkspaceID,
		}); err != nil {
			return mapAuthorizationFailure(err)
		}
		items, revisions, err := store.ListMembershipAuthorities(ctx, tx, afterID, pageSize+1)
		if err != nil {
			return err
		}
		result.CollectionRevision = revisions.Memberships
		result.Items = make([]Membership, 0, min(len(items), int(pageSize)))
		for index, item := range items {
			if index == int(pageSize) {
				result.NextCursor = result.Items[len(result.Items)-1].MembershipID
				break
			}
			membership, convertErr := membershipFromStore(item)
			if convertErr != nil {
				return convertErr
			}
			result.Items = append(result.Items, membership)
		}
		return nil
	})
	return result, err
}

func (service *IdentityService) GrantMembership(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	request GrantMembership,
) (MembershipMutationResult, error) {
	if service == nil || ctx == nil {
		return MembershipMutationResult{}, invalidRequestFailure()
	}
	var result MembershipMutationResult
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		authorization, err := service.bindAndAuthorizeAccessMutation(ctx, tx, actor, scope)
		if err != nil {
			return err
		}
		intentRevision, err := validateMutationControl(request.Control)
		if err != nil {
			return err
		}
		permissions, connectionIDs, operationIDs, err := parseResourceGrants(request.ResourceGrants)
		if err != nil {
			return invalidRequestFailure()
		}
		if !authorization.ContainsGrant(
			permissions,
			connectionIDs,
			operationIDs,
			request.ExpiresAt,
		) {
			return mapAuthorizationFailure(auth.ErrResourceNotFound)
		}
		digest, err := canonicalIntent(struct {
			Operation      string             `json:"operation"`
			Scope          contract.Scope     `json:"scope"`
			Principal      contract.Principal `json:"principal"`
			Role           string             `json:"role"`
			ResourceGrants []string           `json:"resource_grants"`
			ExpiresAt      *time.Time         `json:"expires_at"`
			IntentRevision uint64             `json:"expected_revision"`
		}{
			Operation:      "membership.grant",
			Scope:          scope,
			Principal:      request.Principal,
			Role:           request.Role,
			ResourceGrants: request.ResourceGrants,
			ExpiresAt:      request.ExpiresAt,
			IntentRevision: intentRevision,
		})
		if err != nil {
			return invalidRequestFailure()
		}
		key := mutationReceiptKey(scope, actor, "membership.grant", "memberships", request.Control.IdempotencyKey)
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
		membershipID, err := randomApplicationID(service.random, "membership-", 18)
		if err != nil {
			return err
		}
		stored, collectionRevision, err := store.GrantMembership(ctx, tx, store.GrantMembershipRequest{
			MembershipID:               membershipID,
			ResourceGrants:             request.ResourceGrants,
			Scope:                      scope,
			Principal:                  request.Principal,
			Role:                       request.Role,
			Permissions:                permissions,
			ConnectionIDs:              connectionIDs,
			OperationIDs:               operationIDs,
			ExpiresAt:                  request.ExpiresAt,
			ExpectedCollectionRevision: intentRevision,
		})
		if err != nil {
			return mapMutationFailure(err)
		}
		membership, err := membershipFromStore(stored)
		if err != nil {
			return err
		}
		result = MembershipMutationResult{
			Membership: membership,
			Mutation: MutationMetadata{
				ResultRef:       membershipID,
				Revision:        collectionRevision,
				ControlRevision: stored.ControlRevision,
				Status:          StatusCreated,
			},
		}
		return service.finishMutation(ctx, tx, key, result.Mutation, result)
	})
	return result, err
}

func (service *IdentityService) RevokeMembership(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	request RevokeMembership,
) (MembershipMutationResult, error) {
	if service == nil || ctx == nil || !validApplicationID(request.MembershipID) {
		return MembershipMutationResult{}, invalidRequestFailure()
	}
	var result MembershipMutationResult
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		if _, err := service.bindAndAuthorizeAccessMutation(ctx, tx, actor, scope); err != nil {
			return err
		}
		intentRevision, err := validateMutationControl(request.Control)
		if err != nil {
			return err
		}
		if err := request.BoundScopeDigest.Validate(); err != nil ||
			request.ExpiresAt != nil &&
				(request.ExpiresAt.IsZero() || request.ExpiresAt.Location() != time.UTC) {
			return invalidRequestFailure()
		}
		digest, err := canonicalIntent(struct {
			Operation        string                  `json:"operation"`
			Scope            contract.Scope          `json:"scope"`
			MembershipID     string                  `json:"membership_id"`
			BoundScopeDigest contract.SemanticDigest `json:"bound_scope_digest"`
			ExpiresAt        *time.Time              `json:"expires_at"`
			IntentRevision   uint64                  `json:"expected_revision"`
		}{
			Operation:        "membership.revoke",
			Scope:            scope,
			MembershipID:     request.MembershipID,
			BoundScopeDigest: request.BoundScopeDigest,
			ExpiresAt:        request.ExpiresAt,
			IntentRevision:   intentRevision,
		})
		if err != nil {
			return invalidRequestFailure()
		}
		key := mutationReceiptKey(scope, actor, "membership.revoke", request.MembershipID, request.Control.IdempotencyKey)
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
		current, err := store.LoadMembershipByID(ctx, tx, request.MembershipID)
		if err != nil {
			return mapMutationFailure(err)
		}
		currentMembership, err := membershipFromStore(current)
		if err != nil {
			return err
		}
		if currentMembership.BoundScopeDigest != request.BoundScopeDigest ||
			!equalOptionalApplicationTime(currentMembership.ExpiresAt, request.ExpiresAt) {
			return mapMutationFailure(store.ErrCASConflict)
		}
		stored, unchanged, err := store.RevokeMembership(ctx, tx, store.RevokeMembershipRequest{
			Scope:            scope,
			MembershipID:     current.MembershipID,
			Principal:        current.Principal,
			Role:             current.Role,
			ResourceGrants:   current.ResourceGrants,
			Permissions:      current.Permissions,
			ConnectionIDs:    current.ConnectionIDs,
			OperationIDs:     current.OperationIDs,
			ExpiresAt:        current.ExpiresAt,
			ExpectedRevision: intentRevision,
		})
		if err != nil {
			return mapMutationFailure(err)
		}
		membership, err := membershipFromStore(stored)
		if err != nil {
			return err
		}
		result = MembershipMutationResult{
			Membership: membership,
			Mutation: MutationMetadata{
				ResultRef:       request.MembershipID,
				Revision:        stored.ControlRevision,
				ControlRevision: stored.ControlRevision,
				Status:          StatusOK,
				Unchanged:       unchanged,
			},
		}
		return service.finishMutation(ctx, tx, key, result.Mutation, result)
	})
	return result, err
}

func (service *IdentityService) bindAndAuthorizeAccessMutation(
	ctx context.Context,
	tx *store.Transaction,
	actor contract.Actor,
	scope contract.Scope,
) (auth.Authorization, error) {
	if err := store.BindAuthDeployment(ctx, tx, service.deploymentID); err != nil {
		return auth.Authorization{}, err
	}
	authorization, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
		Scope:        scope,
		Actor:        actor,
		Permission:   "workspace.access.manage",
		ResourceKind: auth.ResourceWorkspace,
		ResourceID:   scope.WorkspaceID,
	})
	if err != nil {
		return auth.Authorization{}, mapAuthorizationFailure(err)
	}
	return authorization, nil
}

func (service *IdentityService) finishMutation(
	ctx context.Context,
	tx *store.Transaction,
	key store.MutationReceiptKey,
	metadata MutationMetadata,
	response any,
) error {
	encoded, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("encode safe identity mutation result: %w", err)
	}
	_, err = store.FinishIdentityMutation(
		ctx,
		tx,
		key,
		metadata.ResultRef,
		metadata.Revision,
		metadata.Status,
		encoded,
		service.receiptRetention,
	)
	return err
}

func canonicalIntent(value any) (contract.SemanticDigest, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return contract.CanonicalDigest(encoded)
}

func mutationReceiptKey(
	scope contract.Scope,
	actor contract.Actor,
	operationID, targetID, idempotencyKey string,
) store.MutationReceiptKey {
	return store.MutationReceiptKey{
		Scope:          scope,
		PrincipalID:    actor.RequestedActor.PrincipalID,
		OperationID:    operationID,
		TargetID:       targetID,
		IdempotencyKey: idempotencyKey,
	}
}

func validateMutationControl(control MutationControl) (uint64, error) {
	if !validApplicationID(control.IdempotencyKey) || control.IntentRevision == 0 {
		return 0, invalidRequestFailure()
	}
	if control.ExpectedRevision != nil &&
		(*control.ExpectedRevision == 0 || *control.ExpectedRevision != control.IntentRevision) {
		return 0, invalidRequestFailure()
	}
	return control.IntentRevision, nil
}

func requireFreshMutationPrecondition(control MutationControl) error {
	if control.ExpectedRevision == nil {
		return &Failure{
			HTTPStatus:  StatusPreconditionRequired,
			Code:        "control_revision_required",
			RetryAction: "refresh_control_revision",
		}
	}
	return nil
}

func decodeReplay(receipt store.MutationReceipt, target any) error {
	if len(receipt.SafeResult) == 0 || string(receipt.SafeResult) == "null" {
		return &Failure{
			HTTPStatus:        StatusGone,
			Code:              "retry_receipt_expired",
			RetryAction:       "inspect_existing_result",
			ExistingResultRef: receipt.ResultRef,
			cause:             store.ErrMutationReceiptExpired,
		}
	}
	if err := json.Unmarshal(receipt.SafeResult, target); err != nil {
		return fmt.Errorf("decode durable identity mutation replay: %w", err)
	}
	return nil
}

func mapMutationFailure(err error) error {
	if err == nil {
		return nil
	}
	var expired *store.MutationReceiptExpiredError
	switch {
	case errors.As(err, &expired):
		return &Failure{
			HTTPStatus:        StatusGone,
			Code:              "retry_receipt_expired",
			RetryAction:       "inspect_existing_result",
			ExistingResultRef: expired.ResultRef,
			cause:             err,
		}
	case errors.Is(err, store.ErrIdempotencyConflict):
		return &Failure{
			HTTPStatus:  StatusConflict,
			Code:        "idempotency_conflict",
			RetryAction: "new_key_for_new_intent",
			cause:       err,
		}
	case errors.Is(err, store.ErrImmutableConflict):
		return &Failure{
			HTTPStatus:  StatusConflict,
			Code:        "resource_conflict",
			RetryAction: "inspect_existing_result",
			cause:       err,
		}
	case errors.Is(err, store.ErrCASConflict):
		return &Failure{
			HTTPStatus:  StatusPreconditionFailed,
			Code:        "control_revision_mismatch",
			RetryAction: "refresh_control_revision",
			cause:       err,
		}
	case errors.Is(err, store.ErrMutationUnresolved):
		return &Failure{
			HTTPStatus:  503,
			Code:        "mutation_outcome_unknown",
			RetryAction: "same_key_reconcile_only",
			cause:       err,
		}
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrScopeMismatch):
		return resourceNotFoundFailure(err)
	default:
		return err
	}
}

func mapAuthorizationFailure(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, auth.ErrAuthenticationRequired):
		return authenticationFailure()
	case errors.Is(err, auth.ErrGrantUnavailable), errors.Is(err, auth.ErrDelegationInvalid):
		return &Failure{HTTPStatus: 403, Code: "grant_unavailable", RetryAction: "refresh_authorization", cause: err}
	case errors.Is(err, auth.ErrResourceNotFound), errors.Is(err, auth.ErrAuthorizationDenied):
		return resourceNotFoundFailure(err)
	default:
		return err
	}
}

func authenticationFailure() *Failure {
	return &Failure{HTTPStatus: 401, Code: "authentication_required", RetryAction: "refresh_authorization", cause: auth.ErrAuthenticationRequired}
}

func resourceNotFoundFailure(cause error) *Failure {
	return &Failure{HTTPStatus: 404, Code: "resource_not_found", RetryAction: "correct_scope", cause: cause}
}

func invalidRequestFailure() *Failure {
	return &Failure{HTTPStatus: 400, Code: "invalid_request", RetryAction: "correct_input"}
}

func membershipFromStore(value store.MembershipAuthority) (Membership, error) {
	resourceGrants := append([]string(nil), value.ResourceGrants...)
	permissions := append([]string(nil), value.Permissions...)
	connectionIDs := append([]string(nil), value.ConnectionIDs...)
	operationIDs := append([]string(nil), value.OperationIDs...)
	boundScopeDigest, err := canonicalIntent(struct {
		Kind           string             `json:"kind"`
		Scope          contract.Scope     `json:"scope"`
		MembershipID   string             `json:"membership_id"`
		Principal      contract.Principal `json:"principal"`
		Role           string             `json:"role"`
		ResourceGrants []string           `json:"resource_grants"`
		Permissions    []string           `json:"permissions"`
		ConnectionIDs  []string           `json:"connection_ids"`
		OperationIDs   []string           `json:"operation_ids"`
		ExpiresAt      *time.Time         `json:"expires_at"`
	}{
		Kind:           "membership.bound_scope",
		Scope:          value.Scope,
		MembershipID:   value.MembershipID,
		Principal:      value.Principal,
		Role:           value.Role,
		ResourceGrants: resourceGrants,
		Permissions:    permissions,
		ConnectionIDs:  connectionIDs,
		OperationIDs:   operationIDs,
		ExpiresAt:      copyTime(value.ExpiresAt),
	})
	if err != nil {
		return Membership{}, fmt.Errorf("digest membership bound scope: %w", err)
	}
	return Membership{
		MembershipID:     value.MembershipID,
		Principal:        value.Principal,
		Role:             value.Role,
		ResourceGrants:   resourceGrants,
		Permissions:      permissions,
		ConnectionIDs:    connectionIDs,
		OperationIDs:     operationIDs,
		BoundScopeDigest: boundScopeDigest,
		PolicyEpoch:      value.PolicyEpoch,
		ControlRevision:  value.ControlRevision,
		ExpiresAt:        copyTime(value.ExpiresAt),
		RevokedAt:        copyTime(value.RevokedAt),
	}, nil
}

func validateGrantMembership(scope contract.Scope, request GrantMembership) error {
	if err := request.Principal.Validate(); err != nil || request.Principal.Kind != contract.PrincipalUser ||
		request.Principal.OrgID != scope.OrgID || !validApplicationName(request.Role) ||
		!validSortedResourceGrants(request.ResourceGrants) {
		return errors.New("membership grant is invalid")
	}
	if request.ExpiresAt != nil && (request.ExpiresAt.IsZero() || request.ExpiresAt.Location() != time.UTC) {
		return errors.New("membership expiry is invalid")
	}
	return nil
}

func parseResourceGrants(values []string) ([]string, []string, []string, error) {
	permissions := make([]string, 0, len(values))
	connectionIDs := make([]string, 0, len(values))
	operationIDs := make([]string, 0, len(values))
	for _, value := range values {
		kind, target, found := strings.Cut(value, ":")
		if !found || target == "" {
			return nil, nil, nil, errors.New("resource grant is invalid")
		}
		switch kind {
		case "permission":
			if !validApplicationName(target) {
				return nil, nil, nil, errors.New("permission resource grant is invalid")
			}
			permissions = append(permissions, target)
		case "connection":
			if !validApplicationID(target) {
				return nil, nil, nil, errors.New("connection resource grant is invalid")
			}
			connectionIDs = append(connectionIDs, target)
		case "operation":
			if !validApplicationOpaque(target, 1024) {
				return nil, nil, nil, errors.New("operation resource grant is invalid")
			}
			operationIDs = append(operationIDs, target)
		default:
			return nil, nil, nil, errors.New("resource grant kind is invalid")
		}
	}
	return permissions, connectionIDs, operationIDs, nil
}

func validSortedResourceGrants(values []string) bool {
	if values == nil || len(values) > 256 {
		return false
	}
	for index, value := range values {
		if !validApplicationOpaque(value, 1024) || index > 0 && values[index-1] >= value {
			return false
		}
	}
	return true
}

func validSortedIDs(values []string) bool {
	if values == nil || len(values) > 256 {
		return false
	}
	for index, value := range values {
		if !validApplicationID(value) || index > 0 && values[index-1] >= value {
			return false
		}
	}
	return true
}

func validSortedOperationIDs(values []string) bool {
	if values == nil || len(values) > 256 {
		return false
	}
	for index, value := range values {
		if !validApplicationOpaque(value, 1024) || index > 0 && values[index-1] >= value {
			return false
		}
	}
	return true
}

func validSortedNames(values []string) bool {
	if values == nil || len(values) > 128 {
		return false
	}
	for index, value := range values {
		if !validApplicationName(value) || index > 0 && values[index-1] >= value {
			return false
		}
	}
	return true
}

func validApplicationName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' ||
			character == '_' || character == '-' || character == '.' && index > 0 {
			continue
		}
		return false
	}
	return true
}

func validApplicationID(value string) bool {
	return validApplicationOpaque(value, 512)
}

func validApplicationOpaque(value string, maximum int) bool {
	if value == "" || len(value) > maximum || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func equalOptionalApplicationTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func validDisplayName(value string) bool {
	return value != "" && len(value) <= 256 && validApplicationID(value)
}

func validOptionalDisplayName(value *string) bool {
	return value == nil || *value == "" || validDisplayName(*value)
}

func validWorkspaceScopes(scope contract.Scope, values []string) bool {
	return len(values) == 1 && values[0] == scope.WorkspaceID
}

func copyString(value *string) *string {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func randomApplicationID(reader io.Reader, prefix string, byteCount int) (string, error) {
	if reader == nil || !validApplicationName(prefix) || byteCount < 16 || byteCount > 64 {
		return "", errors.New("application identity generator is invalid")
	}
	value := make([]byte, byteCount)
	if _, err := io.ReadFull(reader, value); err != nil {
		return "", fmt.Errorf("generate application identity: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(value), nil
}

func copyTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func sortedSubset(values, ceiling []string) bool {
	for _, value := range values {
		if !slices.Contains(ceiling, value) {
			return false
		}
	}
	return true
}
