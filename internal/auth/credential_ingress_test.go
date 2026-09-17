package auth_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

func TestCredentialIngressWipesTransferredWrappingKey(t *testing.T) {
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
	key := bytes.Repeat([]byte{0x5a}, 32)
	defer clear(key)
	ingress, err := app.NewCredentialIngress(identityService, app.CredentialIngressConfig{
		StepUpMaximumAge: time.Minute,
		WrappingKeyRef:   "secret://fixture/ingress-wrapping",
		LoadWrappingKey:  func(context.Context, string) ([]byte, error) { return key, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	actor := directTestActor(fixture.ada)
	existing, err := identityService.InspectConnection(ctx, actor, fixture.alpha, fixture.connection)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := identityService.CreateConnection(ctx, actor, fixture.alpha, app.CreateConnection{
		ConnectorID:       existing.ConnectorID,
		ProviderAccountID: existing.ProviderAccountID,
		DisplayName:       "Wrapping-key ownership",
		Control:           mutationControl("create-key-ownership-connection", 1, true),
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.execMigrator(t, fixture.alpha, `
		UPDATE pm.memberships SET
			permissions = ARRAY(
				SELECT value FROM unnest(permissions || ARRAY['connection.credentials.manage']) AS entries(value)
				ORDER BY value COLLATE "C"
			),
			connection_ids = ARRAY(
				SELECT value FROM unnest(connection_ids || ARRAY[$1::text]) AS entries(value)
				ORDER BY value COLLATE "C"
			),
			policy_epoch = policy_epoch + 1,
			control_revision = control_revision + 1
		WHERE org_id = $2 AND workspace_id = $3 AND identity_id = $4
	`, connection.Connection.ConnectionID, fixture.alpha.OrgID, fixture.alpha.WorkspaceID, fixture.ada.PrincipalID)
	var authenticatedAt time.Time
	err = store.WithinScope(ctx, fixture.pool, fixture.alpha, actor, func(ctx context.Context, tx *store.Transaction) error {
		var readErr error
		authenticatedAt, readErr = store.ReadAuthenticationTime(ctx, tx)
		return readErr
	})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("synthetic-private-ingress-key-ownership-canary")
	defer clear(secret)
	stored, err := ingress.Store(ctx, app.CredentialIngressRequest{
		Identity: auth.AuthenticatedIdentity{
			Actor:           actor,
			Source:          auth.AuthenticationBrowserSession,
			AuthenticatedAt: authenticatedAt,
		},
		Scope:          fixture.alpha,
		ConnectionID:   connection.Connection.ConnectionID,
		IdempotencyKey: "wrapping-key-transfer",
		Secret:         secret,
		Scopes:         []string{"records:read"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stored.CredentialRef == "" || stored.CiphertextVersion != 1 || stored.LifecycleRevision != 1 {
		t.Fatal("private ingress did not return its initial durable credential revision")
	}
	for _, value := range key {
		if value != 0 {
			t.Fatal("private ingress retained transferred wrapping-key bytes after return")
		}
	}
}
