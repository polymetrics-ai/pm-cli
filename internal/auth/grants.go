package auth

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

type ResourceKind string

const (
	ResourceWorkspace  ResourceKind = "workspace"
	ResourceConnection ResourceKind = "connection"
	ResourceRun        ResourceKind = "run"
	ResourceEvent      ResourceKind = "event"
	ResourceArtifact   ResourceKind = "artifact"
)

type AuthorizationRequest struct {
	Scope        contract.Scope
	Actor        contract.Actor
	Permission   string
	ResourceKind ResourceKind
	ResourceID   string
	RunID        string
	ConnectionID string
	OperationID  string
	// AutomationScope selects explicit service-account workspace/connection/operation
	// scope authority. It never derives or grants an HTTP permission.
	AutomationScope bool
}

type Authorization struct {
	Scope         contract.Scope
	Actor         contract.Actor
	Permission    string
	ConnectionID  string
	OperationID   string
	Permissions   []string
	ConnectionIDs []string
	OperationIDs  []string
	ExpiresAt     time.Time
	PolicyEpochs  contract.PolicyEpochs
	AuthorityRef  string
}

// ContainsGrant reports whether a proposed child grant is no broader or
// longer-lived than this current authorization. Inputs must be canonical
// strictly-sorted sets; a nil expiry means an unbounded membership lifetime.
func (authorization Authorization) ContainsGrant(
	permissions, connectionIDs, operationIDs []string,
	expiresAt *time.Time,
) bool {
	if !sortedSubset(permissions, authorization.Permissions) ||
		!sortedSubset(connectionIDs, authorization.ConnectionIDs) ||
		!sortedSubset(operationIDs, authorization.OperationIDs) {
		return false
	}
	if expiresAt == nil {
		return authorization.ExpiresAt.Equal(farFuture)
	}
	return !expiresAt.After(authorization.ExpiresAt)
}

type AuthorizationCallback func(context.Context, *store.Transaction, AuthorizationRequest) (Authorization, error)

type authoritySet struct {
	permissions  []string
	connections  []string
	operations   []string
	expiresAt    time.Time
	authorityRef string
	policyEpochs contract.PolicyEpochs
}

// ResourceAuthorization is the session-free current-authority surface shared
// by authenticated request services and workers. Both methods require the
// caller's already installed scoped transaction.
type ResourceAuthorization interface {
	AuthorizeResource(context.Context, *store.Transaction, AuthorizationRequest) (Authorization, error)
	RecheckGrant(context.Context, *store.Transaction, GrantCheck) (Authorization, error)
}

// ResourceAuthorizer contains no identity-session, OIDC, device, or inbound
// policy state. All current authority is loaded through the supplied scoped
// transaction by the same implementation used by Service.
type ResourceAuthorizer struct{}

func NewResourceAuthorizer() *ResourceAuthorizer {
	return &ResourceAuthorizer{}
}

func (authorizer *ResourceAuthorizer) AuthorizeResource(
	ctx context.Context,
	tx *store.Transaction,
	request AuthorizationRequest,
) (Authorization, error) {
	if authorizer == nil {
		return Authorization{}, ErrAuthorizationDenied
	}
	return authorizeResource(ctx, tx, request)
}

// AuthorizeResource evaluates current membership/service/delegation and
// connection authority inside the caller-owned scoped transaction. It performs
// no session lookup and is therefore safe to compose atomically with a later
// credential, admission, or access mutation after the caller authenticated.
func (service *Service) AuthorizeResource(
	ctx context.Context,
	tx *store.Transaction,
	request AuthorizationRequest,
) (Authorization, error) {
	if service == nil {
		return Authorization{}, ErrAuthorizationDenied
	}
	return authorizeResource(ctx, tx, request)
}

func authorizeResource(
	ctx context.Context,
	tx *store.Transaction,
	request AuthorizationRequest,
) (Authorization, error) {
	if rejectNilContext(ctx) != nil || tx == nil ||
		request.Permission == "" || !validOpaque(request.Permission, 128) {
		return Authorization{}, ErrAuthorizationDenied
	}
	if err := request.Scope.Validate(); err != nil {
		return Authorization{}, ErrResourceNotFound
	}
	if err := request.Actor.Validate(); err != nil {
		return Authorization{}, ErrAuthorizationDenied
	}
	if request.Actor.RequestedActor.OrgID != request.Scope.OrgID ||
		request.Actor.EffectiveActor.OrgID != request.Scope.OrgID {
		return Authorization{}, ErrResourceNotFound
	}
	if err := store.RequireAuthorizationContext(tx, request.Scope, request.Actor); err != nil {
		return Authorization{}, ErrResourceNotFound
	}
	if request.ConnectionID != "" && !validOpaque(request.ConnectionID, 512) ||
		request.OperationID != "" && !validOpaque(request.OperationID, 1024) ||
		request.ResourceID != "" && !validOpaque(request.ResourceID, 512) ||
		request.RunID != "" && !validOpaque(request.RunID, 512) {
		return Authorization{}, ErrResourceNotFound
	}
	if request.ResourceKind == ResourceArtifact && request.ConnectionID != "" {
		return Authorization{}, ErrResourceNotFound
	}
	connectionID := request.ConnectionID
	if request.ResourceKind == ResourceConnection {
		if connectionID != "" && connectionID != request.ResourceID {
			return Authorization{}, ErrResourceNotFound
		}
		connectionID = request.ResourceID
	}
	now, err := store.ReadAuthenticationTime(ctx, tx)
	if err != nil {
		return Authorization{}, err
	}
	current, err := loadActorAuthority(ctx, tx, request.Actor, now)
	if err != nil {
		return Authorization{}, err
	}
	automationScope := request.Actor.RequestedActor.Kind == contract.PrincipalServiceAccount
	if automationScope != request.AutomationScope {
		return Authorization{}, ErrResourceNotFound
	}
	if (!automationScope || request.Actor.Delegation != nil) &&
		!slices.Contains(current.permissions, request.Permission) {
		return Authorization{}, ErrResourceNotFound
	}
	if request.OperationID != "" && !slices.Contains(current.operations, request.OperationID) {
		return Authorization{}, ErrResourceNotFound
	}
	if request.ResourceKind == ResourceArtifact {
		connectionID, err = store.LoadArtifactConnectionID(ctx, tx, request.ResourceID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrScopeMismatch) {
				return Authorization{}, ErrResourceNotFound
			}
			return Authorization{}, err
		}
	}
	if connectionID != "" && !slices.Contains(current.connections, connectionID) {
		return Authorization{}, ErrResourceNotFound
	}
	if request.ResourceKind != ResourceArtifact {
		if err := store.AuthorizeResourceExistence(
			ctx,
			tx,
			string(request.ResourceKind),
			request.ResourceID,
			request.RunID,
		); err != nil {
			if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrScopeMismatch) {
				return Authorization{}, ErrResourceNotFound
			}
			return Authorization{}, err
		}
	}
	if connectionID != "" {
		connection, loadErr := store.LoadConnectionAuthority(ctx, tx, connectionID)
		if loadErr != nil || connection.State != "active" || connection.RevokedAt != nil {
			if loadErr != nil && !errors.Is(loadErr, store.ErrNotFound) {
				return Authorization{}, loadErr
			}
			return Authorization{}, ErrResourceNotFound
		}
		current.policyEpochs.Connection = connection.Revision
	}
	return Authorization{
		Scope:         request.Scope,
		Actor:         request.Actor,
		Permission:    request.Permission,
		ConnectionID:  connectionID,
		OperationID:   request.OperationID,
		Permissions:   append([]string(nil), current.permissions...),
		ConnectionIDs: append([]string(nil), current.connections...),
		OperationIDs:  append([]string(nil), current.operations...),
		ExpiresAt:     current.expiresAt,
		PolicyEpochs:  current.policyEpochs,
		AuthorityRef:  current.authorityRef,
	}, nil
}

func loadActorAuthority(
	ctx context.Context,
	tx *store.Transaction,
	actor contract.Actor,
	now time.Time,
) (authoritySet, error) {
	if actor.Delegation == nil {
		return loadDirectAuthority(ctx, tx, actor.RequestedActor, now)
	}
	if actor.Delegation.Hop != 1 {
		return authoritySet{}, ErrDelegationInvalid
	}
	delegation, err := store.LoadDelegationAuthority(ctx, tx, actor.Delegation.DelegationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return authoritySet{}, ErrGrantUnavailable
		}
		return authoritySet{}, err
	}
	if delegation.Hop != 1 || delegation.RevokedAt != nil || !delegation.ExpiresAt.After(now) ||
		delegation.RequestedActor.Kind != contract.PrincipalServiceAccount ||
		delegation.EffectiveActor.Kind != contract.PrincipalUser ||
		!delegation.RequestedActor.Equal(actor.RequestedActor) ||
		!delegation.EffectiveActor.Equal(actor.EffectiveActor) ||
		delegation.ParentGrantRef != actor.Delegation.ParentGrantRef ||
		!actor.Delegation.ParentCeiling.Equal(delegation.ParentCeiling) {
		return authoritySet{}, ErrGrantUnavailable
	}
	parent, err := loadDirectAuthority(ctx, tx, actor.RequestedActor, now)
	if err != nil {
		return authoritySet{}, err
	}
	effective, err := loadDirectAuthority(ctx, tx, actor.EffectiveActor, now)
	if err != nil {
		return authoritySet{}, err
	}
	if parent.authorityRef != delegation.ParentGrantRef ||
		!sortedSubset(delegation.ParentCeiling.ConnectionIDs, parent.connections) ||
		!sortedSubset(delegation.ParentCeiling.OperationIDs, parent.operations) ||
		parent.expiresAt.Before(delegation.ParentCeiling.ExpiresAt) {
		return authoritySet{}, ErrGrantUnavailable
	}
	connections := intersectSorted(parent.connections, effective.connections, delegation.ConnectionIDs)
	operations := intersectSorted(parent.operations, effective.operations, delegation.OperationIDs)
	permissions := append([]string(nil), effective.permissions...)
	expiresAt := earliestTime(parent.expiresAt, effective.expiresAt, delegation.ExpiresAt)
	return authoritySet{
		permissions:  permissions,
		connections:  connections,
		operations:   operations,
		expiresAt:    expiresAt,
		authorityRef: delegation.DelegationID,
		policyEpochs: contract.PolicyEpochs{
			Membership: effective.policyEpochs.Membership,
			Service:    parent.policyEpochs.Service,
			Delegation: delegation.PolicyEpoch,
		},
	}, nil
}

func loadDirectAuthority(
	ctx context.Context,
	tx *store.Transaction,
	principal contract.Principal,
	now time.Time,
) (authoritySet, error) {
	switch principal.Kind {
	case contract.PrincipalUser:
		membership, err := store.LoadMembershipAuthority(ctx, tx, principal.PrincipalID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return authoritySet{}, ErrResourceNotFound
			}
			return authoritySet{}, err
		}
		if membership.RevokedAt != nil || membership.ExpiresAt != nil && !membership.ExpiresAt.After(now) {
			return authoritySet{}, ErrGrantUnavailable
		}
		expiresAt := farFuture
		if membership.ExpiresAt != nil {
			expiresAt = *membership.ExpiresAt
		}
		return authoritySet{
			permissions:  membership.Permissions,
			connections:  membership.ConnectionIDs,
			operations:   membership.OperationIDs,
			expiresAt:    expiresAt,
			authorityRef: membership.MembershipID,
			policyEpochs: contract.PolicyEpochs{Membership: membership.PolicyEpoch},
		}, nil
	case contract.PrincipalServiceAccount:
		account, err := store.LoadServiceAccountAuthority(ctx, tx, principal.PrincipalID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return authoritySet{}, ErrResourceNotFound
			}
			return authoritySet{}, err
		}
		if account.RevokedAt != nil || !account.ExpiresAt.After(now) {
			return authoritySet{}, ErrGrantUnavailable
		}
		return authoritySet{
			permissions:  account.Permissions,
			connections:  account.ConnectionIDs,
			operations:   account.OperationIDs,
			expiresAt:    account.ExpiresAt,
			authorityRef: account.ServiceAccountID,
			policyEpochs: contract.PolicyEpochs{Service: account.PolicyEpoch},
		}, nil
	default:
		return authoritySet{}, ErrAuthorizationDenied
	}
}

var farFuture = time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)

func intersectSorted(sets ...[]string) []string {
	if len(sets) == 0 {
		return []string{}
	}
	result := append([]string(nil), sets[0]...)
	for _, set := range sets[1:] {
		filtered := result[:0]
		left, right := 0, 0
		for left < len(result) && right < len(set) {
			switch {
			case result[left] < set[right]:
				left++
			case result[left] > set[right]:
				right++
			default:
				filtered = append(filtered, result[left])
				left++
				right++
			}
		}
		result = filtered
	}
	return result
}

func sortedSubset(values, ceiling []string) bool {
	valueIndex, ceilingIndex := 0, 0
	for valueIndex < len(values) && ceilingIndex < len(ceiling) {
		switch {
		case values[valueIndex] < ceiling[ceilingIndex]:
			return false
		case values[valueIndex] > ceiling[ceilingIndex]:
			ceilingIndex++
		default:
			valueIndex++
			ceilingIndex++
		}
	}
	return valueIndex == len(values)
}

func earliestTime(values ...time.Time) time.Time {
	result := farFuture
	for _, value := range values {
		if value.Before(result) {
			result = value
		}
	}
	return result
}

type GrantCheck struct {
	Grant                contract.ExecutionGrant
	Permission           string
	ResourceKind         ResourceKind
	ResourceID           string
	RunID                string
	ConnectionID         string
	OperationID          string
	CurrentApprovalEpoch uint64
}

// RecheckGrant deliberately does not consult the login session. It rechecks the
// durable grant, current identity/connection epochs, and an approval epoch
// supplied by the caller that owns approval state.
func (service *Service) RecheckGrant(ctx context.Context, tx *store.Transaction, check GrantCheck) (Authorization, error) {
	if service == nil {
		return Authorization{}, ErrGrantUnavailable
	}
	return recheckGrant(ctx, tx, check)
}

func (authorizer *ResourceAuthorizer) RecheckGrant(
	ctx context.Context,
	tx *store.Transaction,
	check GrantCheck,
) (Authorization, error) {
	if authorizer == nil {
		return Authorization{}, ErrGrantUnavailable
	}
	return recheckGrant(ctx, tx, check)
}

func recheckGrant(ctx context.Context, tx *store.Transaction, check GrantCheck) (Authorization, error) {
	if err := check.Grant.Validate(); err != nil {
		return Authorization{}, ErrGrantUnavailable
	}
	now, err := store.ReadAuthenticationTime(ctx, tx)
	if err != nil {
		return Authorization{}, err
	}
	if !check.Grant.ExpiresAt.After(now) ||
		!slices.Contains(check.Grant.ConnectionIDs, check.ConnectionID) ||
		!slices.Contains(check.Grant.OperationIDs, check.OperationID) {
		return Authorization{}, ErrGrantUnavailable
	}
	if len(check.Grant.ApprovalRefs) != 0 &&
		(check.CurrentApprovalEpoch == 0 || check.CurrentApprovalEpoch != check.Grant.PolicyEpochs.Approval) {
		return Authorization{}, ErrGrantUnavailable
	}
	authorization, err := authorizeResource(ctx, tx, AuthorizationRequest{
		Scope:           check.Grant.Scope,
		Actor:           check.Grant.Actor,
		AutomationScope: check.Grant.Actor.RequestedActor.Kind == contract.PrincipalServiceAccount,
		Permission:      check.Permission,
		ResourceKind:    check.ResourceKind,
		ResourceID:      check.ResourceID,
		RunID:           check.RunID,
		ConnectionID:    check.ConnectionID,
		OperationID:     check.OperationID,
	})
	if err != nil {
		return Authorization{}, err
	}
	current := authorization.PolicyEpochs
	expected := check.Grant.PolicyEpochs
	if current.Membership != expected.Membership || current.Service != expected.Service ||
		current.Delegation != expected.Delegation || current.Connection != expected.Connection {
		return Authorization{}, ErrGrantUnavailable
	}
	return authorization, nil
}
