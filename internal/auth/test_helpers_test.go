package auth_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"golang.org/x/oauth2"
)

const (
	testHost   = "pm.example.test"
	testOrigin = "https://pm.example.test"
)

type authFixture struct {
	migratorDSN string
	pool        *store.Pool
	deployment  string
	alpha       contract.Scope
	beta        contract.Scope
	ada         contract.Principal
	bob         contract.Principal
	charlie     contract.Principal
	delta       contract.Principal
	connection  string
	run         string
	event       string
	artifact    string
}

type rejectingKeySet struct{}

func (rejectingKeySet) VerifySignature(context.Context, string) ([]byte, error) {
	return nil, errors.New("test key set has no signing keys")
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	credentials := loadAuthStoreCredentials(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.Migrate(ctx, credentials.migratorDSN); err != nil {
		t.Fatalf("migrate selected auth store: %T", err)
	}
	pool, err := store.OpenPool(ctx, store.PoolConfig{
		ConnectionString:  credentials.runtimeDSN,
		MaxConns:          8,
		MinConns:          0,
		MaxConnLifetime:   5 * time.Minute,
		MaxConnIdleTime:   time.Minute,
		HealthCheckPeriod: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("open selected auth store: %T", err)
	}
	t.Cleanup(pool.Close)

	suffix := randomFixtureHex(t, 8)
	fixture := &authFixture{
		migratorDSN: credentials.migratorDSN,
		pool:        pool,
		deployment:  "deployment-" + suffix,
		alpha:       contract.Scope{OrgID: "org-" + suffix, WorkspaceID: "workspace-alpha-" + suffix},
		beta:        contract.Scope{OrgID: "org-" + suffix, WorkspaceID: "workspace-beta-" + suffix},
		connection:  "connection-shared-" + suffix,
		run:         "run-shared-" + suffix,
		event:       "event-shared-" + suffix,
		artifact:    "artifact-shared-" + suffix,
	}
	fixture.ada = contract.Principal{OrgID: fixture.alpha.OrgID, PrincipalID: "ada-" + suffix, Kind: contract.PrincipalUser}
	fixture.bob = contract.Principal{OrgID: fixture.alpha.OrgID, PrincipalID: "bob-" + suffix, Kind: contract.PrincipalUser}
	fixture.charlie = contract.Principal{OrgID: fixture.alpha.OrgID, PrincipalID: "charlie-" + suffix, Kind: contract.PrincipalUser}
	fixture.delta = contract.Principal{OrgID: fixture.alpha.OrgID, PrincipalID: "delta-" + suffix, Kind: contract.PrincipalUser}
	fixture.seed(t)
	t.Cleanup(func() { fixture.cleanup(t) })
	return fixture
}

type authStoreCredentials struct {
	runtimeDSN  string
	migratorDSN string
}

func loadAuthStoreCredentials(t *testing.T) authStoreCredentials {
	t.Helper()
	secretDir := os.Getenv("PM_STORE_TEST_SECRETS_DIR")
	if secretDir == "" {
		t.Fatal("PM_STORE_TEST_SECRETS_DIR is required")
	}
	absolute, err := filepath.Abs(secretDir)
	if err != nil {
		t.Fatal("resolve PM_STORE_TEST_SECRETS_DIR")
	}
	endpointBytes, err := os.ReadFile(filepath.Join(absolute, "pm-app-endpoint"))
	if err != nil {
		t.Fatal("read pm-app-endpoint")
	}
	passwordBytes, err := os.ReadFile(filepath.Join(absolute, "pm-app-migrator-password"))
	if err != nil {
		t.Fatal("read pm-app-migrator-password")
	}
	endpoint := strings.TrimSpace(string(endpointBytes))
	password := strings.TrimSpace(string(passwordBytes))
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("pm-app-endpoint is not a PostgreSQL URL")
	}
	if parsed.User == nil || parsed.User.Username() != store.RuntimeRole ||
		strings.TrimPrefix(parsed.Path, "/") != store.ApplicationDatabase || password == "" {
		t.Fatal("PostgreSQL fixture does not select the runtime role and database")
	}
	migrator := *parsed
	migrator.User = url.UserPassword(store.MigratorRole, password)
	return authStoreCredentials{runtimeDSN: endpoint, migratorDSN: migrator.String()}
}

func randomFixtureHex(t *testing.T, byteCount int) string {
	t.Helper()
	value := make([]byte, byteCount)
	if _, err := rand.Read(value); err != nil {
		t.Fatal("generate isolated auth fixture identity")
	}
	return hex.EncodeToString(value)
}

func directTestActor(principal contract.Principal) contract.Actor {
	return contract.Actor{RequestedActor: principal, EffectiveActor: principal}
}

func (fixture *authFixture) seed(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, fixture.migratorDSN)
	if err != nil {
		t.Fatalf("connect auth fixture migrator: %T", err)
	}
	defer connection.Close(ctx)
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin auth fixture seed: %T", err)
	}
	defer tx.Rollback(ctx)

	if err := bindFixtureScope(ctx, tx, fixture.deployment, fixture.alpha, fixture.ada); err != nil {
		t.Fatalf("bind alpha auth fixture: %T", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pm.workspace_namespaces (org_id, workspace_id, name)
		VALUES ($1, $2, $3)
	`, fixture.alpha.OrgID, fixture.alpha.WorkspaceID, "alpha"); err != nil {
		t.Fatalf("seed alpha workspace: %T", err)
	}
	for _, identity := range []contract.Principal{fixture.ada, fixture.bob, fixture.charlie, fixture.delta} {
		if _, err := tx.Exec(ctx, `
			INSERT INTO pm.identities (org_id, identity_id, kind, issuer, subject, revision)
			VALUES ($1, $2, $3, $4, $5, 1)
		`, identity.OrgID, identity.PrincipalID, string(identity.Kind), "https://fixture.example/"+identity.PrincipalID, identity.PrincipalID); err != nil {
			t.Fatalf("seed auth identity: %T", err)
		}
	}
	providerAccount := "provider-account-" + strings.TrimPrefix(fixture.deployment, "deployment-")
	if _, err := tx.Exec(ctx, `
		INSERT INTO pm.provider_accounts (
			org_id, provider_account_id, provider, account_witness, capacity, revision
		) VALUES ($1, $2, 'fixture', $3, 8, 1)
	`, fixture.alpha.OrgID, providerAccount, "account-witness-"+strings.TrimPrefix(fixture.deployment, "deployment-")); err != nil {
		t.Fatalf("seed provider account: %T", err)
	}
	if err := seedFixtureWorkspace(ctx, tx, fixture.alpha, fixture.ada, fixture.connection, fixture.run, fixture.event, fixture.artifact, providerAccount, "admin"); err != nil {
		t.Fatalf("seed alpha protected resources: %T", err)
	}

	if err := bindFixtureScope(ctx, tx, fixture.deployment, fixture.beta, fixture.bob); err != nil {
		t.Fatalf("bind beta auth fixture: %T", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pm.workspace_namespaces (org_id, workspace_id, name)
		VALUES ($1, $2, $3)
	`, fixture.beta.OrgID, fixture.beta.WorkspaceID, "beta"); err != nil {
		t.Fatalf("seed beta workspace: %T", err)
	}
	if err := seedFixtureWorkspace(ctx, tx, fixture.beta, fixture.bob, fixture.connection, fixture.run, fixture.event, fixture.artifact, providerAccount, "operator"); err != nil {
		t.Fatalf("seed beta protected resources: %T", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit auth fixture seed: %T", err)
	}
}

func seedFixtureWorkspace(
	ctx context.Context,
	tx pgx.Tx,
	scope contract.Scope,
	principal contract.Principal,
	connectionID, runID, eventID, artifactID, providerAccountID, role string,
) error {
	permissions := []string{
		"artifact.read",
		"connection.manage",
		"connection.read",
		"event.read",
		"operation.invoke",
		"run.read",
		"workspace.access.manage",
		"workspace.access.read",
		"workspace.read",
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pm.memberships (
			org_id, workspace_id, membership_id, identity_id, role,
			permissions, connection_ids, operation_ids, policy_epoch, control_revision
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1, 1)
	`, scope.OrgID, scope.WorkspaceID, "membership-"+principal.PrincipalID, principal.PrincipalID,
		role, permissions, []string{connectionID}, []string{"operation-read", "operation-write"}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pm.connections (
			org_id, workspace_id, connection_id, connector_id, provider_account_id,
			revision, credential_ref, display_name, state
		) VALUES ($1, $2, $3, 'fixture-connector', $4, 1, '', 'Fixture connection', 'active')
	`, scope.OrgID, scope.WorkspaceID, connectionID, providerAccountID); err != nil {
		return err
	}
	digest := "sha256:" + strings.Repeat("1", 64)
	if _, err := tx.Exec(ctx, `
		INSERT INTO pm.run_specs (
			org_id, workspace_id, run_id, intent_digest, idempotency_fingerprint, spec, accepted_at
		) VALUES ($1, $2, $3, $4, $4, '{}'::jsonb, clock_timestamp())
	`, scope.OrgID, scope.WorkspaceID, runID, digest); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pm.runs (org_id, workspace_id, run_id, state, revision, record, updated_at)
		VALUES ($1, $2, $3, 'accepted', 1, '{}'::jsonb, clock_timestamp())
	`, scope.OrgID, scope.WorkspaceID, runID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pm.server_events (
			org_id, workspace_id, run_id, sequence, event_id, state_revision, event_type, safe_payload, committed_at
		) VALUES ($1, $2, $3, 1, $4, 1, 'fixture.created', '{}'::jsonb, clock_timestamp())
	`, scope.OrgID, scope.WorkspaceID, runID, eventID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO pm.artifacts (
			org_id, workspace_id, artifact_id, object_attempt_key, sha256,
			byte_length, media_type, backend_generation, object_ref, registered_at
		) VALUES ($1, $2, $3, $4, $5, 0, 'application/octet-stream', 'fixture-generation', '{}'::jsonb, clock_timestamp())
	`, scope.OrgID, scope.WorkspaceID, artifactID, "attempt-"+scope.WorkspaceID, strings.Repeat("0", 64))
	return err
}

func bindFixtureScope(
	ctx context.Context,
	tx pgx.Tx,
	deployment string,
	scope contract.Scope,
	principal contract.Principal,
) error {
	_, err := tx.Exec(ctx, `
		SELECT set_config('pm.deployment_id', $1, true),
		       set_config('pm.org_id', $2, true),
		       set_config('pm.workspace_id', $3, true),
		       set_config('pm.requested_actor_id', $4, true),
		       set_config('pm.requested_actor_kind', $5, true),
		       set_config('pm.effective_actor_id', $4, true),
		       set_config('pm.effective_actor_kind', $5, true)
	`, deployment, scope.OrgID, scope.WorkspaceID, principal.PrincipalID, string(principal.Kind))
	return err
}

func (fixture *authFixture) cleanup(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, fixture.migratorDSN)
	if err != nil {
		t.Errorf("connect auth fixture cleanup: %T", err)
		return
	}
	defer connection.Close(ctx)
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Errorf("begin auth fixture cleanup: %T", err)
		return
	}
	defer tx.Rollback(ctx)
	if err := bindFixtureScope(ctx, tx, fixture.deployment, fixture.alpha, fixture.ada); err != nil {
		t.Errorf("bind auth fixture cleanup: %T", err)
		return
	}
	for _, query := range []string{
		`DELETE FROM pm.pm_sessions WHERE deployment_id = $1`,
		`DELETE FROM pm.oidc_login_challenges WHERE deployment_id = $1`,
		`DELETE FROM pm.device_login_challenges WHERE deployment_id = $1`,
		`DELETE FROM pm.device_start_windows WHERE deployment_id = $1`,
		`DELETE FROM pm.auth_bootstrap_identities WHERE deployment_id = $1`,
	} {
		if _, err := tx.Exec(ctx, query, fixture.deployment); err != nil {
			t.Errorf("clean deployment auth rows: %T", err)
			return
		}
	}
	for _, scope := range []contract.Scope{fixture.alpha, fixture.beta} {
		if err := bindFixtureScope(ctx, tx, fixture.deployment, scope, fixture.ada); err != nil {
			t.Errorf("bind scoped auth cleanup: %T", err)
			return
		}
		for _, query := range []string{
			`DELETE FROM pm.credential_refresh_leases WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.credential_mutation_receipts WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.credential_lifecycles WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.encrypted_secrets WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.delegations WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.service_account_credentials WHERE deployment_id = $3 AND org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.service_accounts WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.identity_mutation_keys WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.identity_collection_revisions WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.memberships WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.connections WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.server_events WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.runs WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.run_specs WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.artifacts WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.workspace_namespaces WHERE org_id = $1 AND workspace_id = $2`,
		} {
			arguments := []any{scope.OrgID, scope.WorkspaceID}
			if strings.Contains(query, "$3") {
				arguments = append(arguments, fixture.deployment)
			}
			if _, err := tx.Exec(ctx, query, arguments...); err != nil {
				t.Errorf("clean scoped auth rows: %T", err)
				return
			}
		}
	}
	if err := bindFixtureScope(ctx, tx, fixture.deployment, fixture.alpha, fixture.ada); err != nil {
		t.Errorf("bind auth root cleanup: %T", err)
		return
	}
	if _, err := tx.Exec(ctx, `DELETE FROM pm.identities WHERE org_id = $1`, fixture.alpha.OrgID); err != nil {
		t.Errorf("clean auth identities: %T", err)
		return
	}
	if _, err := tx.Exec(ctx, `DELETE FROM pm.provider_accounts WHERE org_id = $1`, fixture.alpha.OrgID); err != nil {
		t.Errorf("clean auth provider account: %T", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		t.Errorf("commit auth fixture cleanup: %T", err)
	}
}

func (fixture *authFixture) serviceConfig(deployment string, oidcConfiguration auth.OIDCConfig) auth.Config {
	if deployment == "" {
		deployment = fixture.deployment
	}
	if oidcConfiguration.KeySet == nil {
		oidcConfiguration = auth.OIDCConfig{
			Issuer: "https://identity.example.test",
			OAuth2: oauth2.Config{
				ClientID: "pm-test-client",
				Endpoint: oauth2.Endpoint{
					AuthURL:  "https://identity.example.test/authorize",
					TokenURL: "https://identity.example.test/token",
				},
				RedirectURL: "https://pm.example.test/v1/sessions/browser/callback",
				Scopes:      []string{oidc.ScopeOpenID},
			},
			KeySet:   rejectingKeySet{},
			LoginTTL: 5 * time.Minute,
		}
	}
	return auth.Config{
		Pool:         fixture.pool,
		DeploymentID: deployment,
		Random:       rand.Reader,
		Device: auth.DevicePolicy{
			ChallengeTTL:     5 * time.Minute,
			PollInterval:     time.Second,
			StartWindow:      5 * time.Minute,
			MaximumStarts:    3,
			MaximumBodyBytes: 1024,
		},
		Sessions: auth.SessionPolicy{BrowserTTL: time.Hour, DeviceTTL: 24 * time.Hour},
		Inbound: auth.InboundPolicy{
			AllowedHosts:   []string{testHost},
			AllowedOrigins: []string{testOrigin},
		},
		OIDC: oidcConfiguration,
	}
}

func (fixture *authFixture) newAuthService(t *testing.T) *auth.Service {
	t.Helper()
	service, err := auth.NewService(fixture.serviceConfig("", auth.OIDCConfig{}))
	if err != nil {
		t.Fatalf("construct auth service: %v", err)
	}
	return service
}

func (fixture *authFixture) addBootstrapIdentity(
	t *testing.T,
	issuer, subject string,
	principal contract.Principal,
	canApprove bool,
) {
	t.Helper()
	fixture.execMigrator(t, fixture.alpha, `
		INSERT INTO pm.auth_bootstrap_identities (
			deployment_id, issuer, subject, org_id, principal_id, principal_kind, can_approve_devices
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, fixture.deployment, issuer, subject, principal.OrgID, principal.PrincipalID, string(principal.Kind), canApprove)
}

func (fixture *authFixture) execMigrator(t *testing.T, scope contract.Scope, query string, arguments ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, fixture.migratorDSN)
	if err != nil {
		t.Fatalf("connect auth fixture mutation: %T", err)
	}
	defer connection.Close(ctx)
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin auth fixture mutation: %T", err)
	}
	defer tx.Rollback(ctx)
	if err := bindFixtureScope(ctx, tx, fixture.deployment, scope, fixture.ada); err != nil {
		t.Fatalf("bind auth fixture mutation: %T", err)
	}
	if _, err := tx.Exec(ctx, query, arguments...); err != nil {
		t.Fatalf("mutate auth fixture: %T", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit auth fixture mutation: %T", err)
	}
}

func (fixture *authFixture) makeDevicePollEligible(t *testing.T, challengeID string) {
	t.Helper()
	fixture.execMigrator(t, fixture.alpha, `
		UPDATE pm.device_login_challenges
		SET next_poll_at = clock_timestamp() - interval '1 second'
		WHERE deployment_id = $1 AND challenge_id = $2
	`, fixture.deployment, challengeID)
}

func (fixture *authFixture) expireDeviceChallenge(t *testing.T, challengeID string) {
	t.Helper()
	fixture.execMigrator(t, fixture.alpha, `
		UPDATE pm.device_login_challenges
		SET expires_at = clock_timestamp() - interval '1 second'
		WHERE deployment_id = $1 AND challenge_id = $2
	`, fixture.deployment, challengeID)
}

func (fixture *authFixture) expireOIDCChallenge(t *testing.T, challengeID string) {
	t.Helper()
	fixture.execMigrator(t, fixture.alpha, `
		UPDATE pm.oidc_login_challenges
		SET expires_at = clock_timestamp() - interval '1 second'
		WHERE deployment_id = $1 AND challenge_id = $2
	`, fixture.deployment, challengeID)
}

func (fixture *authFixture) revokeMembership(t *testing.T, scope contract.Scope, principal contract.Principal) {
	t.Helper()
	fixture.execMigrator(t, scope, `
		UPDATE pm.memberships
		SET revoked_at = clock_timestamp(), policy_epoch = policy_epoch + 1, control_revision = control_revision + 1
		WHERE org_id = $1 AND workspace_id = $2 AND identity_id = $3
	`, scope.OrgID, scope.WorkspaceID, principal.PrincipalID)
}

func (fixture *authFixture) sessionCount(t *testing.T) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, fixture.migratorDSN)
	if err != nil {
		t.Fatalf("connect auth session count: %T", err)
	}
	defer connection.Close(ctx)
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin auth session count: %T", err)
	}
	defer tx.Rollback(ctx)
	if err := bindFixtureScope(ctx, tx, fixture.deployment, fixture.alpha, fixture.ada); err != nil {
		t.Fatalf("bind auth session count: %T", err)
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM pm.pm_sessions WHERE deployment_id = $1`, fixture.deployment).Scan(&count); err != nil {
		t.Fatalf("count auth sessions: %T", err)
	}
	return count
}

func requireErrorIs(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error = %v, want %v", err, target)
	}
}

func requireAppStatus(t *testing.T, err error, status int) {
	t.Helper()
	var failure *app.Failure
	if !errors.As(err, &failure) || failure.HTTPStatus != status {
		t.Fatalf("error = %v, want application status %d", err, status)
	}
}
