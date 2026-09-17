package auth_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

func TestScopeAndInboundPolicy(t *testing.T) {
	fixture := newAuthFixture(t)
	service := fixture.newAuthService(t)

	inboundCases := []struct {
		name    string
		request auth.InboundRequest
		wantErr bool
	}{
		{
			name:    "remote browser mutation",
			request: auth.InboundRequest{TLS: true, Host: testHost, Origin: testOrigin, Browser: true, Mutation: true},
		},
		{
			name:    "loopback follows identical policy",
			request: auth.InboundRequest{TLS: true, Host: testHost, Origin: testOrigin, Browser: true, Mutation: true, Loopback: true},
		},
		{
			name:    "loopback without TLS is denied",
			request: auth.InboundRequest{Host: testHost, Origin: testOrigin, Browser: true, Mutation: true, Loopback: true},
			wantErr: true,
		},
		{
			name:    "unexpected host is denied",
			request: auth.InboundRequest{TLS: true, Host: "attacker.example.test", Origin: testOrigin, Browser: true, Mutation: true},
			wantErr: true,
		},
		{
			name:    "unexpected origin is denied",
			request: auth.InboundRequest{TLS: true, Host: testHost, Origin: "https://attacker.example.test", Browser: true, Mutation: true},
			wantErr: true,
		},
		{
			name:    "bearer query credential is denied",
			request: auth.InboundRequest{TLS: true, Host: testHost, BearerCredentialInQuery: true},
			wantErr: true,
		},
	}
	for _, test := range inboundCases {
		t.Run(test.name, func(t *testing.T) {
			err := service.ValidateInboundPolicy(test.request)
			if test.wantErr {
				requireErrorIs(t, err, auth.ErrInvalidInboundPolicy)
			} else if err != nil {
				t.Fatalf("ValidateInboundPolicy() error = %v, want nil", err)
			}
		})
	}

	ada := directTestActor(fixture.ada)
	alphaCases := []auth.AuthorizationRequest{
		{
			Scope: fixture.alpha, Actor: ada, Permission: "workspace.read",
			ResourceKind: auth.ResourceWorkspace, ResourceID: fixture.alpha.WorkspaceID,
		},
		{
			Scope: fixture.alpha, Actor: ada, Permission: "connection.read",
			ResourceKind: auth.ResourceConnection, ResourceID: fixture.connection,
			ConnectionID: fixture.connection,
		},
		{
			Scope: fixture.alpha, Actor: ada, Permission: "run.read",
			ResourceKind: auth.ResourceRun, ResourceID: fixture.run,
			ConnectionID: fixture.connection, OperationID: "operation-read",
		},
		{
			Scope: fixture.alpha, Actor: ada, Permission: "event.read",
			ResourceKind: auth.ResourceEvent, ResourceID: fixture.event, RunID: fixture.run,
			ConnectionID: fixture.connection, OperationID: "operation-read",
		},
		{
			Scope: fixture.alpha, Actor: ada, Permission: "artifact.read",
			ResourceKind: auth.ResourceArtifact, ResourceID: fixture.artifact,
			ConnectionID: fixture.connection, OperationID: "operation-read",
		},
	}
	for _, request := range alphaCases {
		err := store.WithinScope(context.Background(), fixture.pool, fixture.alpha, ada, func(ctx context.Context, tx *store.Transaction) error {
			_, err := service.AuthorizeResource(ctx, tx, request)
			return err
		})
		if err != nil {
			t.Fatalf("authorize existing alpha %s: %v", request.ResourceKind, err)
		}
	}
	workerAuthorize := auth.NewResourceAuthorizer()
	var serviceAuthorization, workerAuthorization auth.Authorization
	if err := store.WithinScope(context.Background(), fixture.pool, fixture.alpha, ada, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		serviceAuthorization, err = service.AuthorizeResource(ctx, tx, alphaCases[2])
		if err != nil {
			return err
		}
		workerAuthorization, err = workerAuthorize.AuthorizeResource(ctx, tx, alphaCases[2])
		return err
	}); err != nil {
		t.Fatalf("compare worker-safe resource authorization: %v", err)
	}
	if !reflect.DeepEqual(workerAuthorization, serviceAuthorization) {
		t.Fatalf("worker-safe authorization differed from the canonical service policy\nworker: %#v\nservice: %#v", workerAuthorization, serviceAuthorization)
	}

	// No membership conceals the sibling scope; this is not a revoked known grant.
	for _, request := range alphaCases {
		request.Scope = fixture.beta
		request.ResourceID = betaResourceID(request.ResourceKind, fixture)
		err := store.WithinScope(context.Background(), fixture.pool, fixture.beta, ada, func(ctx context.Context, tx *store.Transaction) error {
			_, err := service.AuthorizeResource(ctx, tx, request)
			return err
		})
		requireErrorIs(t, err, auth.ErrResourceNotFound)
	}

	mismatchedRequest := alphaCases[0]
	mismatchedRequest.Scope = fixture.beta
	mismatchedRequest.ResourceID = fixture.beta.WorkspaceID
	err := store.WithinScope(context.Background(), fixture.pool, fixture.alpha, ada, func(ctx context.Context, tx *store.Transaction) error {
		_, err := service.AuthorizeResource(ctx, tx, mismatchedRequest)
		return err
	})
	requireErrorIs(t, err, auth.ErrResourceNotFound)

	bob := directTestActor(fixture.bob)
	err = store.WithinScope(context.Background(), fixture.pool, fixture.alpha, ada, func(ctx context.Context, tx *store.Transaction) error {
		request := alphaCases[0]
		request.Actor = bob
		_, err := service.AuthorizeResource(ctx, tx, request)
		return err
	})
	requireErrorIs(t, err, auth.ErrResourceNotFound)

	foreignScope := contract.Scope{OrgID: "org-foreign", WorkspaceID: fixture.alpha.WorkspaceID}
	err = store.WithinScope(context.Background(), fixture.pool, fixture.alpha, ada, func(ctx context.Context, tx *store.Transaction) error {
		request := alphaCases[0]
		request.Scope = foreignScope
		_, err := service.AuthorizeResource(ctx, tx, request)
		return err
	})
	requireErrorIs(t, err, auth.ErrResourceNotFound)

	fixture.revokeMembership(t, fixture.alpha, fixture.ada)
	err = store.WithinScope(context.Background(), fixture.pool, fixture.alpha, ada, func(ctx context.Context, tx *store.Transaction) error {
		_, err := service.AuthorizeResource(ctx, tx, alphaCases[0])
		return err
	})
	requireErrorIs(t, err, auth.ErrGrantUnavailable)

	for _, denial := range []error{auth.ErrResourceNotFound, auth.ErrAuthorizationDenied, auth.ErrGrantUnavailable, auth.ErrDelegationInvalid} {
		if !auth.IsSafeScopeDenial(denial) {
			t.Fatalf("IsSafeScopeDenial(%v) = false", denial)
		}
	}
}

func betaResourceID(kind auth.ResourceKind, fixture *authFixture) string {
	switch kind {
	case auth.ResourceWorkspace:
		return fixture.beta.WorkspaceID
	case auth.ResourceConnection:
		return fixture.connection
	case auth.ResourceRun:
		return fixture.run
	case auth.ResourceEvent:
		return fixture.event
	case auth.ResourceArtifact:
		return fixture.artifact
	default:
		return ""
	}
}
