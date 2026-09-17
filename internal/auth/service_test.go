package auth_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

func mutationControl(key string, revision uint64, includeHeader bool) app.MutationControl {
	control := app.MutationControl{IdempotencyKey: key, IntentRevision: revision}
	if includeHeader {
		control.ExpectedRevision = new(revision)
	}
	return control
}

func alternateDigest(value contract.SemanticDigest) contract.SemanticDigest {
	fill := "f"
	if strings.HasSuffix(string(value), strings.Repeat("f", 64)) {
		fill = "e"
	}
	return contract.SemanticDigest("sha256:" + strings.Repeat(fill, 64))
}

func TestConnectionNameValidation(t *testing.T) {
	fixture := newAuthFixture(t)
	authService := fixture.newAuthService(t)
	identityService, err := app.NewIdentityService(app.IdentityServiceConfig{
		Pool: fixture.pool, DeploymentID: fixture.deployment,
		Authorize:        authService.AuthorizeResource,
		ReceiptRetention: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin := directTestActor(fixture.ada)
	connection, err := identityService.InspectConnection(ctx, admin, fixture.alpha, fixture.connection)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("empty create name is a client refusal", func(t *testing.T) {
		_, err := identityService.CreateConnection(ctx, admin, fixture.alpha, app.CreateConnection{
			ConnectorID:       connection.ConnectorID,
			ProviderAccountID: connection.ProviderAccountID,
			DisplayName:       "",
			Control:           mutationControl("empty-connection-create-name", 1, true),
		})
		requireAppStatus(t, err, http.StatusBadRequest)
	})
	t.Run("empty update name is a client refusal", func(t *testing.T) {
		_, err := identityService.UpdateConnection(ctx, admin, fixture.alpha, app.UpdateConnection{
			ConnectionID: connection.ConnectionID,
			DisplayName:  "",
			Control:      mutationControl("empty-connection-update-name", connection.Revision, true),
		})
		requireAppStatus(t, err, http.StatusBadRequest)
	})
}

func TestServiceAndDelegationScope(t *testing.T) {
	fixture := newAuthFixture(t)
	authService := fixture.newAuthService(t)
	identityService, err := app.NewIdentityService(app.IdentityServiceConfig{
		Pool: fixture.pool, DeploymentID: fixture.deployment,
		Authorize:        authService.AuthorizeResource,
		ReceiptRetention: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewIdentityService(): %v", err)
	}
	ctx := context.Background()
	admin := directTestActor(fixture.ada)
	expires := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Microsecond)

	membershipRequest := app.GrantMembership{
		Principal: fixture.charlie,
		Role:      "operator",
		ResourceGrants: []string{
			"connection:" + fixture.connection,
			"operation:operation-read",
			"permission:operation.invoke",
			"permission:workspace.access.read",
			"permission:workspace.read",
		},
		ExpiresAt: new(expires),
		Control:   mutationControl("membership-create-charlie", 1, false),
	}
	contradictoryControl := membershipRequest
	contradictoryControl.Control = mutationControl("membership-contradictory-control", 1, true)
	contradictoryControl.Control.ExpectedRevision = new(uint64(2))
	if _, err := identityService.GrantMembership(ctx, admin, fixture.alpha, contradictoryControl); err == nil {
		t.Fatal("membership grant accepted contradictory body and If-Match revisions")
	} else {
		requireAppStatus(t, err, http.StatusBadRequest)
	}
	if _, err := identityService.GrantMembership(ctx, admin, fixture.alpha, membershipRequest); err == nil {
		t.Fatal("fresh membership grant without If-Match succeeded")
	} else {
		requireAppStatus(t, err, app.StatusPreconditionRequired)
	}
	membershipRequest.Control = mutationControl("membership-create-charlie", 1, true)
	membershipResult, err := identityService.GrantMembership(ctx, admin, fixture.alpha, membershipRequest)
	if err != nil {
		t.Fatalf("GrantMembership(): %v", err)
	}
	if membershipResult.Mutation.Status != app.StatusCreated ||
		!slices.Equal(membershipResult.Membership.ResourceGrants, membershipRequest.ResourceGrants) ||
		membershipResult.Membership.BoundScopeDigest.Validate() != nil {
		t.Fatalf("membership result = %#v", membershipResult)
	}
	membershipReplayRequest := membershipRequest
	membershipReplayRequest.Control.ExpectedRevision = nil
	membershipReplay, err := identityService.GrantMembership(ctx, admin, fixture.alpha, membershipReplayRequest)
	if err != nil {
		t.Fatalf("GrantMembership(replay without If-Match): %v", err)
	}
	if !membershipReplay.Mutation.Replayed ||
		membershipReplay.Mutation.ResultRef != membershipResult.Mutation.ResultRef ||
		membershipReplay.Membership.BoundScopeDigest != membershipResult.Membership.BoundScopeDigest {
		t.Fatalf("membership replay = %#v", membershipReplay)
	}
	changedMembership := membershipRequest
	changedMembership.Role = "viewer"
	if _, err := identityService.GrantMembership(ctx, admin, fixture.alpha, changedMembership); err == nil {
		t.Fatal("changed membership intent reused an idempotency key")
	} else {
		requireAppStatus(t, err, app.StatusConflict)
	}
	staleMembership := membershipRequest
	staleMembership.Principal = fixture.delta
	staleMembership.Control = mutationControl("membership-create-stale", 1, true)
	if _, err := identityService.GrantMembership(ctx, admin, fixture.alpha, staleMembership); err == nil {
		t.Fatal("stale membership collection revision succeeded")
	} else {
		requireAppStatus(t, err, app.StatusPreconditionFailed)
	}
	membershipPage, err := identityService.ListMemberships(ctx, admin, fixture.alpha, "", 100)
	if err != nil {
		t.Fatalf("ListMemberships(): %v", err)
	}
	foundMembership := false
	for _, membership := range membershipPage.Items {
		if membership.MembershipID == membershipResult.Membership.MembershipID {
			foundMembership = slices.Equal(membership.ResourceGrants, membershipRequest.ResourceGrants) &&
				membership.BoundScopeDigest == membershipResult.Membership.BoundScopeDigest
		}
	}
	if !foundMembership {
		t.Fatal("listed membership did not preserve resource_grants and bound_scope_digest")
	}
	charlie := directTestActor(fixture.charlie)
	err = store.WithinScope(ctx, fixture.pool, fixture.alpha, charlie, func(ctx context.Context, tx *store.Transaction) error {
		_, err := authService.AuthorizeResource(ctx, tx, auth.AuthorizationRequest{
			Scope: fixture.alpha, Actor: charlie, Permission: "connection.read",
			ResourceKind: auth.ResourceConnection, ResourceID: fixture.connection,
			ConnectionID: fixture.connection,
		})
		return err
	})
	requireErrorIs(t, err, auth.ErrResourceNotFound)

	membershipRevoke := app.RevokeMembership{
		MembershipID:     membershipResult.Membership.MembershipID,
		BoundScopeDigest: membershipResult.Membership.BoundScopeDigest,
		ExpiresAt:        membershipResult.Membership.ExpiresAt,
		Control:          mutationControl("membership-revoke-charlie", membershipResult.Membership.ControlRevision, false),
	}
	if _, err := identityService.RevokeMembership(ctx, admin, fixture.alpha, membershipRevoke); err == nil {
		t.Fatal("fresh membership revoke without If-Match succeeded")
	} else {
		requireAppStatus(t, err, app.StatusPreconditionRequired)
	}
	staleMembershipRevoke := membershipRevoke
	staleMembershipRevoke.BoundScopeDigest = alternateDigest(membershipRevoke.BoundScopeDigest)
	staleMembershipRevoke.Control = mutationControl("membership-revoke-stale", membershipResult.Membership.ControlRevision, true)
	if _, err := identityService.RevokeMembership(ctx, admin, fixture.alpha, staleMembershipRevoke); err == nil {
		t.Fatal("membership revoke with stale bound scope succeeded")
	} else {
		requireAppStatus(t, err, app.StatusPreconditionFailed)
	}
	membershipRevoke.Control = mutationControl("membership-revoke-charlie", membershipResult.Membership.ControlRevision, true)
	membershipRevokeResult, err := identityService.RevokeMembership(ctx, admin, fixture.alpha, membershipRevoke)
	if err != nil {
		t.Fatalf("RevokeMembership(): %v", err)
	}
	if membershipRevokeResult.Membership.RevokedAt == nil {
		t.Fatal("membership revoke did not persist revocation")
	}
	membershipRevokeReplay := membershipRevoke
	membershipRevokeReplay.Control.ExpectedRevision = nil
	replayedMembershipRevoke, err := identityService.RevokeMembership(ctx, admin, fixture.alpha, membershipRevokeReplay)
	if err != nil || !replayedMembershipRevoke.Mutation.Replayed {
		t.Fatalf("RevokeMembership(replay) = %#v, %v", replayedMembershipRevoke, err)
	}
	changedMembershipRevoke := membershipRevoke
	changedMembershipRevoke.ExpiresAt = nil
	if _, err := identityService.RevokeMembership(ctx, admin, fixture.alpha, changedMembershipRevoke); err == nil {
		t.Fatal("changed membership revoke reused an idempotency key")
	} else {
		requireAppStatus(t, err, app.StatusConflict)
	}

	displayName := "Build robot"
	serviceRequest := app.CreateServiceAccount{
		DisplayName:     new(displayName),
		WorkspaceScopes: []string{fixture.alpha.WorkspaceID},
		ConnectionIDs:   []string{fixture.connection},
		OperationIDs:    []string{"operation-read"},
		ExpiresAt:       expires,
		Control:         mutationControl("service-create-robot", 1, false),
	}
	if _, err := identityService.CreateServiceAccount(ctx, admin, fixture.alpha, serviceRequest); err == nil {
		t.Fatal("fresh service-account create without If-Match succeeded")
	} else {
		requireAppStatus(t, err, app.StatusPreconditionRequired)
	}
	serviceRequest.Control = mutationControl("service-create-robot", 1, true)
	serviceResult, err := identityService.CreateServiceAccount(ctx, admin, fixture.alpha, serviceRequest)
	if err != nil {
		t.Fatalf("CreateServiceAccount(): %v", err)
	}
	if serviceResult.OneTimeCredential == "" || serviceResult.ServiceAccount.DisplayName == nil ||
		*serviceResult.ServiceAccount.DisplayName != displayName ||
		!slices.Equal(serviceResult.ServiceAccount.WorkspaceScopes, serviceRequest.WorkspaceScopes) ||
		serviceResult.ServiceAccount.GrantRevision != "1" {
		t.Fatalf("service-account result = %#v", serviceResult)
	}
	serviceReplayRequest := serviceRequest
	serviceReplayRequest.Control.ExpectedRevision = nil
	serviceReplay, err := identityService.CreateServiceAccount(ctx, admin, fixture.alpha, serviceReplayRequest)
	if err != nil {
		t.Fatalf("CreateServiceAccount(replay): %v", err)
	}
	if !serviceReplay.Mutation.Replayed || serviceReplay.OneTimeCredential != "" ||
		serviceReplay.ServiceAccount.ServiceAccountID != serviceResult.ServiceAccount.ServiceAccountID {
		t.Fatalf("service-account replay = %#v", serviceReplay)
	}
	changedService := serviceRequest
	changedService.DisplayName = new("Changed robot")
	if _, err := identityService.CreateServiceAccount(ctx, admin, fixture.alpha, changedService); err == nil {
		t.Fatal("changed service-account intent reused an idempotency key")
	} else {
		requireAppStatus(t, err, app.StatusConflict)
	}
	staleService := serviceRequest
	staleService.DisplayName = nil
	staleService.Control = mutationControl("service-create-stale", 1, true)
	if _, err := identityService.CreateServiceAccount(ctx, admin, fixture.alpha, staleService); err == nil {
		t.Fatal("stale service-account collection revision succeeded")
	} else {
		requireAppStatus(t, err, app.StatusPreconditionFailed)
	}
	servicePage, err := identityService.ListServiceAccounts(ctx, admin, fixture.alpha, "", 100)
	if err != nil {
		t.Fatalf("ListServiceAccounts(): %v", err)
	}
	foundService := false
	for _, serviceAccount := range servicePage.Items {
		if serviceAccount.ServiceAccountID == serviceResult.ServiceAccount.ServiceAccountID {
			foundService = serviceAccount.DisplayName != nil && *serviceAccount.DisplayName == displayName &&
				slices.Equal(serviceAccount.WorkspaceScopes, serviceRequest.WorkspaceScopes) &&
				serviceAccount.GrantRevision == serviceResult.ServiceAccount.GrantRevision
		}
	}
	if !foundService {
		t.Fatal("listed service account did not preserve display_name, workspace_scopes, and grant_revision")
	}
	serviceIdentity, err := authService.AuthenticateServiceCredential(ctx, serviceResult.OneTimeCredential)
	if err != nil {
		t.Fatalf("AuthenticateServiceCredential(): %v", err)
	}
	if serviceIdentity.Principal != serviceResult.ServiceAccount.Principal ||
		serviceIdentity.Source != auth.AuthenticationServiceCredential ||
		!serviceIdentity.AuthenticatedAt.IsZero() {
		t.Fatalf("service credential identity = %#v", serviceIdentity)
	}

	var serviceAuthorization auth.Authorization
	err = store.WithinScope(ctx, fixture.pool, fixture.alpha, serviceIdentity.Actor, func(ctx context.Context, tx *store.Transaction) error {
		var authorizeErr error
		serviceAuthorization, authorizeErr = authService.AuthorizeResource(ctx, tx, auth.AuthorizationRequest{
			Scope: fixture.alpha, Actor: serviceIdentity.Actor, Permission: "operation.invoke",
			ResourceKind: auth.ResourceRun, ResourceID: fixture.run,
			ConnectionID: fixture.connection, OperationID: "operation-read",
			AutomationScope: true,
		})
		return authorizeErr
	})
	if err != nil || len(serviceAuthorization.Permissions) != 0 {
		t.Fatalf("explicit service-account scopes were not usable without invented permissions: %#v, %v", serviceAuthorization, err)
	}
	err = store.WithinScope(ctx, fixture.pool, fixture.alpha, serviceIdentity.Actor, func(ctx context.Context, tx *store.Transaction) error {
		_, err := authService.AuthorizeResource(ctx, tx, auth.AuthorizationRequest{
			Scope: fixture.alpha, Actor: serviceIdentity.Actor, Permission: "operation.invoke",
			ResourceKind: auth.ResourceRun, ResourceID: fixture.run,
			ConnectionID: fixture.connection, OperationID: "operation-read",
		})
		return err
	})
	requireErrorIs(t, err, auth.ErrResourceNotFound)

	delegationExpires := expires.Add(-time.Minute)
	delegationRequest := app.CreateDelegation{
		RequestedActor:  serviceIdentity.Principal,
		EffectiveActor:  fixture.ada,
		WorkspaceScopes: []string{fixture.alpha.WorkspaceID},
		ConnectionIDs:   []string{fixture.connection},
		OperationIDs:    []string{"operation-read"},
		ExpiresAt:       delegationExpires,
		Control:         mutationControl("delegation-create-ada", 1, false),
	}
	if _, err := identityService.CreateDelegation(ctx, serviceIdentity.Actor, fixture.alpha, delegationRequest); err == nil {
		t.Fatal("fresh delegation create without If-Match succeeded")
	} else {
		requireAppStatus(t, err, app.StatusPreconditionRequired)
	}
	delegationRequest.Control = mutationControl("delegation-create-ada", 1, true)
	delegationResult, err := identityService.CreateDelegation(ctx, serviceIdentity.Actor, fixture.alpha, delegationRequest)
	if err != nil {
		t.Fatalf("CreateDelegation(): %v", err)
	}
	if delegationResult.Delegation.Hop != 1 ||
		!slices.Equal(delegationResult.Delegation.WorkspaceScopes, delegationRequest.WorkspaceScopes) ||
		delegationResult.Delegation.ScopeDigest.Validate() != nil ||
		delegationResult.Delegation.ParentGrantRef != serviceResult.ServiceAccount.ServiceAccountID {
		t.Fatalf("delegation result = %#v", delegationResult)
	}
	delegationReplayRequest := delegationRequest
	delegationReplayRequest.Control.ExpectedRevision = nil
	delegationReplay, err := identityService.CreateDelegation(ctx, serviceIdentity.Actor, fixture.alpha, delegationReplayRequest)
	if err != nil || !delegationReplay.Mutation.Replayed ||
		delegationReplay.Delegation.ScopeDigest != delegationResult.Delegation.ScopeDigest {
		t.Fatalf("CreateDelegation replay: replayed=%t digest=%q want=%q error=%v", delegationReplay.Mutation.Replayed, delegationReplay.Delegation.ScopeDigest, delegationResult.Delegation.ScopeDigest, err)
	}
	changedDelegation := delegationRequest
	changedDelegation.ExpiresAt = delegationExpires.Add(-time.Minute)
	if _, err := identityService.CreateDelegation(ctx, serviceIdentity.Actor, fixture.alpha, changedDelegation); err == nil {
		t.Fatal("changed delegation intent reused an idempotency key")
	} else {
		requireAppStatus(t, err, app.StatusConflict)
	}
	staleDelegation := delegationRequest
	staleDelegation.Control = mutationControl("delegation-create-stale", 1, true)
	if _, err := identityService.CreateDelegation(ctx, serviceIdentity.Actor, fixture.alpha, staleDelegation); err == nil {
		t.Fatal("stale delegation collection revision succeeded")
	} else {
		requireAppStatus(t, err, app.StatusPreconditionFailed)
	}
	delegationPage, err := identityService.ListDelegations(ctx, admin, fixture.alpha, "", 100)
	if err != nil {
		t.Fatalf("ListDelegations(): %v", err)
	}
	foundDelegation := false
	for _, delegation := range delegationPage.Items {
		if delegation.DelegationID == delegationResult.Delegation.DelegationID {
			foundDelegation = delegation.ScopeDigest == delegationResult.Delegation.ScopeDigest &&
				slices.Equal(delegation.WorkspaceScopes, delegationRequest.WorkspaceScopes)
		}
	}
	if !foundDelegation {
		t.Fatal("listed delegation did not preserve workspace_scopes and scope_digest")
	}

	delegatedActor, err := app.DelegatedActor(delegationResult.Delegation)
	if err != nil {
		t.Fatalf("DelegatedActor(): %v", err)
	}
	err = store.WithinScope(ctx, fixture.pool, fixture.alpha, delegatedActor, func(ctx context.Context, tx *store.Transaction) error {
		_, err := authService.AuthorizeResource(ctx, tx, auth.AuthorizationRequest{
			Scope: fixture.alpha, Actor: delegatedActor, Permission: "operation.invoke",
			ResourceKind: auth.ResourceRun, ResourceID: fixture.run,
			ConnectionID: fixture.connection, OperationID: "operation-read",
			AutomationScope: true,
		})
		return err
	})
	if err != nil {
		t.Fatalf("authorize one-hop delegated request: %v", err)
	}
	err = store.WithinScope(ctx, fixture.pool, fixture.alpha, delegatedActor, func(ctx context.Context, tx *store.Transaction) error {
		_, err := authService.AuthorizeResource(ctx, tx, auth.AuthorizationRequest{
			Scope: fixture.alpha, Actor: delegatedActor, Permission: "operation.invoke",
			ResourceKind: auth.ResourceRun, ResourceID: fixture.run,
			ConnectionID: fixture.connection, OperationID: "operation-write",
			AutomationScope: true,
		})
		return err
	})
	requireErrorIs(t, err, auth.ErrResourceNotFound)
	secondHop := *delegatedActor.Delegation
	secondHop.Hop = 2
	delegatedActor.Delegation = &secondHop
	if err := delegatedActor.Validate(); err == nil {
		t.Fatal("second delegation hop validated")
	}

	delegationRevoke := app.RevokeDelegation{
		DelegationID:   delegationResult.Delegation.DelegationID,
		RequestedActor: delegationResult.Delegation.RequestedActor,
		EffectiveActor: delegationResult.Delegation.EffectiveActor,
		ScopeDigest:    delegationResult.Delegation.ScopeDigest,
		ExpiresAt:      delegationResult.Delegation.ExpiresAt,
		Control:        mutationControl("delegation-revoke-ada", delegationResult.Delegation.ControlRevision, false),
	}
	if _, err := identityService.RevokeDelegation(ctx, admin, fixture.alpha, delegationRevoke); err == nil {
		t.Fatal("fresh delegation revoke without If-Match succeeded")
	} else {
		requireAppStatus(t, err, app.StatusPreconditionRequired)
	}
	staleDelegationRevoke := delegationRevoke
	staleDelegationRevoke.ScopeDigest = alternateDigest(delegationRevoke.ScopeDigest)
	staleDelegationRevoke.Control = mutationControl("delegation-revoke-stale", delegationResult.Delegation.ControlRevision, true)
	if _, err := identityService.RevokeDelegation(ctx, admin, fixture.alpha, staleDelegationRevoke); err == nil {
		t.Fatal("delegation revoke with stale scope digest succeeded")
	} else {
		requireAppStatus(t, err, app.StatusPreconditionFailed)
	}
	delegationRevoke.Control = mutationControl("delegation-revoke-ada", delegationResult.Delegation.ControlRevision, true)
	delegationRevokeResult, err := identityService.RevokeDelegation(ctx, admin, fixture.alpha, delegationRevoke)
	if err != nil {
		t.Fatalf("RevokeDelegation(): %v", err)
	}
	if delegationRevokeResult.Delegation.RevokedAt == nil {
		t.Fatal("delegation revoke did not persist revocation")
	}
	delegationRevokeReplay := delegationRevoke
	delegationRevokeReplay.Control.ExpectedRevision = nil
	replayedDelegationRevoke, err := identityService.RevokeDelegation(ctx, admin, fixture.alpha, delegationRevokeReplay)
	if err != nil || !replayedDelegationRevoke.Mutation.Replayed {
		t.Fatalf("RevokeDelegation(replay) = %#v, %v", replayedDelegationRevoke, err)
	}
	changedDelegationRevoke := delegationRevoke
	changedDelegationRevoke.ExpiresAt = delegationRevoke.ExpiresAt.Add(-time.Second)
	if _, err := identityService.RevokeDelegation(ctx, admin, fixture.alpha, changedDelegationRevoke); err == nil {
		t.Fatal("changed delegation revoke reused an idempotency key")
	} else {
		requireAppStatus(t, err, app.StatusConflict)
	}

	serviceRevoke := app.RevokeServiceAccount{
		ServiceAccountID: serviceResult.ServiceAccount.ServiceAccountID,
		GrantRevision:    serviceResult.ServiceAccount.GrantRevision,
		ExpiresAt:        serviceResult.ServiceAccount.ExpiresAt,
		Control:          mutationControl("service-revoke-robot", serviceResult.ServiceAccount.ControlRevision, false),
	}
	if _, err := identityService.RevokeServiceAccount(ctx, admin, fixture.alpha, serviceRevoke); err == nil {
		t.Fatal("fresh service-account revoke without If-Match succeeded")
	} else {
		requireAppStatus(t, err, app.StatusPreconditionRequired)
	}
	staleServiceRevoke := serviceRevoke
	staleServiceRevoke.ExpiresAt = staleServiceRevoke.ExpiresAt.Add(-time.Second)
	staleServiceRevoke.Control = mutationControl("service-revoke-stale", serviceResult.ServiceAccount.ControlRevision, true)
	if _, err := identityService.RevokeServiceAccount(ctx, admin, fixture.alpha, staleServiceRevoke); err == nil {
		t.Fatal("service-account revoke with stale grant binding succeeded")
	} else {
		requireAppStatus(t, err, app.StatusPreconditionFailed)
	}
	serviceRevoke.Control = mutationControl("service-revoke-robot", serviceResult.ServiceAccount.ControlRevision, true)
	serviceRevokeResult, err := identityService.RevokeServiceAccount(ctx, admin, fixture.alpha, serviceRevoke)
	if err != nil {
		t.Fatalf("RevokeServiceAccount(): %v", err)
	}
	if serviceRevokeResult.ServiceAccount.RevokedAt == nil {
		t.Fatal("service-account revoke did not persist revocation")
	}
	serviceRevokeReplay := serviceRevoke
	serviceRevokeReplay.Control.ExpectedRevision = nil
	replayedServiceRevoke, err := identityService.RevokeServiceAccount(ctx, admin, fixture.alpha, serviceRevokeReplay)
	if err != nil || !replayedServiceRevoke.Mutation.Replayed ||
		replayedServiceRevoke.OneTimeCredential != "" {
		t.Fatalf("RevokeServiceAccount(replay) = %#v, %v", replayedServiceRevoke, err)
	}
	changedServiceRevoke := serviceRevoke
	changedServiceRevoke.ExpiresAt = serviceRevoke.ExpiresAt.Add(-time.Second)
	if _, err := identityService.RevokeServiceAccount(ctx, admin, fixture.alpha, changedServiceRevoke); err == nil {
		t.Fatal("changed service-account revoke reused an idempotency key")
	} else {
		requireAppStatus(t, err, app.StatusConflict)
	}
	if _, err := authService.AuthenticateServiceCredential(ctx, serviceResult.OneTimeCredential); err != auth.ErrAuthenticationRequired {
		t.Fatalf("revoked service credential error = %v, want ErrAuthenticationRequired", err)
	}

	fixture.revokeMembership(t, fixture.alpha, fixture.ada)
	assertNoUnauthorizedReplay := func(name string, replayed bool, err error) {
		t.Helper()
		if err == nil || replayed {
			t.Fatalf("%s bypassed current authorization: replayed=%v error=%v", name, replayed, err)
		}
	}
	resultMembershipGrant, replayErr := identityService.GrantMembership(ctx, admin, fixture.alpha, membershipReplayRequest)
	assertNoUnauthorizedReplay("membership grant", resultMembershipGrant.Mutation.Replayed, replayErr)
	resultMembershipRevoke, replayErr := identityService.RevokeMembership(ctx, admin, fixture.alpha, membershipRevokeReplay)
	assertNoUnauthorizedReplay("membership revoke", resultMembershipRevoke.Mutation.Replayed, replayErr)
	resultServiceCreate, replayErr := identityService.CreateServiceAccount(ctx, admin, fixture.alpha, serviceReplayRequest)
	assertNoUnauthorizedReplay("service-account create", resultServiceCreate.Mutation.Replayed, replayErr)
	resultServiceRevoke, replayErr := identityService.RevokeServiceAccount(ctx, admin, fixture.alpha, serviceRevokeReplay)
	assertNoUnauthorizedReplay("service-account revoke", resultServiceRevoke.Mutation.Replayed, replayErr)
	resultDelegationCreate, replayErr := identityService.CreateDelegation(ctx, serviceIdentity.Actor, fixture.alpha, delegationReplayRequest)
	assertNoUnauthorizedReplay("delegation create", resultDelegationCreate.Mutation.Replayed, replayErr)
	resultDelegationRevoke, replayErr := identityService.RevokeDelegation(ctx, admin, fixture.alpha, delegationRevokeReplay)
	assertNoUnauthorizedReplay("delegation revoke", resultDelegationRevoke.Mutation.Replayed, replayErr)
}
