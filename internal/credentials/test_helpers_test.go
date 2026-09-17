package credentials

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

type credentialTestCredentials struct {
	runtimeDSN  string
	migratorDSN string
}

type credentialTestPools struct {
	credentials credentialTestCredentials
	first       *store.Pool
}

func prepareCredentialTestPools(t *testing.T) credentialTestPools {
	t.Helper()
	credentials := loadCredentialTestCredentials(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.Migrate(ctx, credentials.migratorDSN); err != nil {
		t.Fatalf("migrate selected credential store: %T", err)
	}
	pool, err := store.OpenPool(ctx, store.PoolConfig{
		ConnectionString:  credentials.runtimeDSN,
		MaxConns:          4,
		MinConns:          0,
		MaxConnLifetime:   5 * time.Minute,
		MaxConnIdleTime:   time.Minute,
		HealthCheckPeriod: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("open selected credential store pool: %T", err)
	}
	t.Cleanup(pool.Close)
	return credentialTestPools{credentials: credentials, first: pool}
}

func loadCredentialTestCredentials(t *testing.T) credentialTestCredentials {
	t.Helper()
	endpoint, parsed := loadCredentialTestEndpoint(t)
	passwordBytes, err := os.ReadFile(filepath.Join(credentialTestSecretsDirectory(t), "pm-app-migrator-password"))
	if err != nil {
		t.Fatal("read pm-app-migrator-password")
	}
	password := strings.TrimSpace(string(passwordBytes))
	if password == "" {
		t.Fatal("credential test migrator password is empty")
	}
	migrator := *parsed
	migrator.User = url.UserPassword(store.MigratorRole, password)
	return credentialTestCredentials{runtimeDSN: endpoint, migratorDSN: migrator.String()}
}

func loadCredentialTestRuntimeDSN(t *testing.T) string {
	t.Helper()
	endpoint, _ := loadCredentialTestEndpoint(t)
	return endpoint
}

func loadCredentialTestEndpoint(t *testing.T) (string, *url.URL) {
	t.Helper()
	endpointBytes, err := os.ReadFile(filepath.Join(credentialTestSecretsDirectory(t), "pm-app-endpoint"))
	if err != nil {
		t.Fatal("read pm-app-endpoint")
	}
	endpoint := strings.TrimSpace(string(endpointBytes))
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("pm-app-endpoint is not a PostgreSQL URL")
	}
	if parsed.User == nil || parsed.User.Username() != store.RuntimeRole ||
		strings.TrimPrefix(parsed.Path, "/") != store.ApplicationDatabase {
		t.Fatal("credential test PostgreSQL identity is invalid")
	}
	return endpoint, parsed
}

func credentialTestSecretsDirectory(t *testing.T) string {
	t.Helper()
	secretDir := os.Getenv("PM_STORE_TEST_SECRETS_DIR")
	if secretDir == "" {
		t.Fatal("PM_STORE_TEST_SECRETS_DIR is required")
	}
	absolute, err := filepath.Abs(secretDir)
	if err != nil {
		t.Fatal("resolve PM_STORE_TEST_SECRETS_DIR")
	}
	return absolute
}

type credentialFixture struct {
	pools       credentialTestPools
	scope       contract.Scope
	actor       contract.Actor
	account     store.ProviderAccount
	connection  store.Connection
	key         []byte
	ownsAccount bool
}

func newCredentialFixture(t *testing.T, pools credentialTestPools) credentialFixture {
	t.Helper()
	suffix := credentialRandomHex(t, 8)
	scope := contract.Scope{
		OrgID:       "org-credential-" + suffix,
		WorkspaceID: "workspace-credential-" + suffix,
	}
	principal := contract.Principal{
		OrgID:       scope.OrgID,
		PrincipalID: "ada-" + suffix,
		Kind:        contract.PrincipalUser,
	}
	fixture := credentialFixture{
		pools: pools,
		scope: scope,
		actor: contract.Actor{RequestedActor: principal, EffectiveActor: principal},
		account: store.ProviderAccount{
			OrgID:          scope.OrgID,
			AccountID:      "account-" + suffix,
			Provider:       "github",
			AccountWitness: "account-witness-" + suffix,
			Capacity:       1,
			Revision:       1,
		},
		key:         bytes.Repeat([]byte{0x5a}, wrappingKeyBytes),
		ownsAccount: true,
	}
	fixture.connection = store.Connection{
		Scope:             scope,
		ConnectionID:      "connection-" + suffix,
		ConnectorID:       "github",
		ProviderAccountID: fixture.account.AccountID,
		Revision:          1,
		CredentialRef:     "credential-" + suffix,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.WithinScope(ctx, pools.first, scope, fixture.actor, func(ctx context.Context, tx *store.Transaction) error {
		if err := store.RegisterWorkspace(ctx, tx, store.WorkspaceIdentity{Scope: scope, Name: "credential workspace " + suffix}); err != nil {
			return err
		}
		if err := store.RegisterIdentity(ctx, tx, store.Identity{
			Principal: principal,
			Issuer:    "https://issuer.example.invalid/" + suffix,
			Subject:   "subject-" + suffix,
			Revision:  1,
		}); err != nil {
			return err
		}
		if err := store.RegisterProviderAccount(ctx, tx, fixture.account); err != nil {
			return err
		}
		return store.RegisterConnection(ctx, tx, fixture.connection)
	}); err != nil {
		t.Fatalf("register credential fixture: %v", err)
	}
	t.Cleanup(func() { cleanupCredentialFixture(t, fixture) })
	return fixture
}

func newCredentialAlias(t *testing.T, pools credentialTestPools, base credentialFixture) credentialFixture {
	t.Helper()
	suffix := credentialRandomHex(t, 8)
	fixture := credentialFixture{
		pools:   pools,
		scope:   contract.Scope{OrgID: base.scope.OrgID, WorkspaceID: "workspace-alias-" + suffix},
		actor:   base.actor,
		account: base.account,
		key:     base.key,
	}
	fixture.connection = store.Connection{
		Scope:             fixture.scope,
		ConnectionID:      "connection-alias-" + suffix,
		ConnectorID:       base.connection.ConnectorID,
		ProviderAccountID: base.account.AccountID,
		Revision:          1,
		CredentialRef:     "credential-alias-" + suffix,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.WithinScope(ctx, pools.first, fixture.scope, fixture.actor, func(ctx context.Context, tx *store.Transaction) error {
		if err := store.RegisterWorkspace(ctx, tx, store.WorkspaceIdentity{
			Scope: fixture.scope,
			Name:  "credential alias workspace " + suffix,
		}); err != nil {
			return err
		}
		return store.RegisterConnection(ctx, tx, fixture.connection)
	}); err != nil {
		t.Fatalf("register credential alias fixture: %v", err)
	}
	t.Cleanup(func() { cleanupCredentialFixture(t, fixture) })
	return fixture
}

func cleanupCredentialFixture(t *testing.T, fixture credentialFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, fixture.pools.credentials.migratorDSN)
	if err != nil {
		t.Fatal("connect credential fixture cleanup")
	}
	defer func() { _ = connection.Close(context.Background()) }()
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatal("begin credential fixture cleanup")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `
		SELECT set_config('pm.org_id', $1, true),
		       set_config('pm.workspace_id', $2, true)
	`, fixture.scope.OrgID, fixture.scope.WorkspaceID); err != nil {
		t.Fatal("install credential fixture cleanup scope")
	}
	statements := []string{
		`DELETE FROM pm.credential_refresh_leases WHERE org_id = $1 AND $2 <> ''`,
		`DELETE FROM pm.credential_mutation_receipts WHERE org_id = $1 AND workspace_id = $2`,
		`DELETE FROM pm.credential_lifecycles WHERE org_id = $1 AND workspace_id = $2`,
		`DELETE FROM pm.encrypted_secrets WHERE org_id = $1 AND workspace_id = $2`,
		`DELETE FROM pm.identity_collection_revisions WHERE org_id = $1 AND workspace_id = $2`,
		`DELETE FROM pm.connections WHERE org_id = $1 AND workspace_id = $2`,
	}
	if fixture.ownsAccount {
		statements = append(
			statements,
			`DELETE FROM pm.identities WHERE org_id = $1 AND $2 <> ''`,
			`DELETE FROM pm.provider_accounts WHERE org_id = $1 AND $2 <> ''`,
		)
	}
	statements = append(statements, `DELETE FROM pm.workspace_namespaces WHERE org_id = $1 AND workspace_id = $2`)
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement, fixture.scope.OrgID, fixture.scope.WorkspaceID); err != nil {
			t.Fatalf("clean credential fixture: %T", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal("commit credential fixture cleanup")
	}
}

func credentialRandomHex(t *testing.T, byteCount int) string {
	t.Helper()
	value := make([]byte, byteCount)
	if _, err := rand.Read(value); err != nil {
		t.Fatal("generate credential test identity")
	}
	return hex.EncodeToString(value)
}

func projectionContainsSecret(projection []byte, secret []byte) bool {
	if len(secret) == 0 {
		return false
	}
	return bytes.Contains(projection, secret) ||
		bytes.Contains(projection, []byte(base64.StdEncoding.EncodeToString(secret))) ||
		bytes.Contains(projection, []byte(hex.EncodeToString(secret))) ||
		bytes.Contains(projection, []byte(fmt.Sprint(secret)))
}

type countingKeyResolver struct {
	key   []byte
	err   error
	calls atomic.Int64
}

func (resolver *countingKeyResolver) resolve(_ context.Context, _ string) ([]byte, error) {
	resolver.calls.Add(1)
	if resolver.err != nil {
		return nil, resolver.err
	}
	return resolver.key, nil
}

type countingAuthorization struct {
	err   error
	calls atomic.Int64
}

func (authorization *countingAuthorization) authorize(_ context.Context, tx *store.Transaction, access Access) error {
	authorization.calls.Add(1)
	if tx == nil || !access.Scope.Equal(access.Connection.Scope) ||
		access.Connection.ConnectionID == "" || access.Account.AccountID == "" {
		return errors.New("invalid authorization access")
	}
	return authorization.err
}

func (fixture credentialFixture) storeRequest(secret []byte, key string) StoreRequest {
	return StoreRequest{
		Scope:          fixture.scope,
		Actor:          fixture.actor,
		ConnectionID:   fixture.connection.ConnectionID,
		CredentialRef:  fixture.connection.CredentialRef,
		IdempotencyKey: key,
		KeyRef:         "wrapping-key-v1",
		Scopes:         []string{"issues:read", "issues:write"},
		AuthEpoch:      1,
		Secret:         secret,
	}
}

func (fixture credentialFixture) storeCredential(t *testing.T, secret []byte, key string) (CredentialStatus, contract.CredentialRef) {
	t.Helper()
	resolver := &countingKeyResolver{key: fixture.key}
	authorization := &countingAuthorization{}
	status, err := StoreEncryptedCredential(
		context.Background(),
		fixture.pools.first,
		resolver.resolve,
		authorization.authorize,
		fixture.storeRequest(secret, key),
	)
	if err != nil {
		t.Fatalf("store credential fixture: %v", err)
	}
	encrypted := fixture.loadEncrypted(t, status.CiphertextVersion)
	return status, encrypted.Binding
}

func (fixture credentialFixture) loadLifecycle(t *testing.T) (store.CredentialLifecycle, error) {
	t.Helper()
	var lifecycle store.CredentialLifecycle
	err := store.WithinScope(context.Background(), fixture.pools.first, fixture.scope, fixture.actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		lifecycle, err = store.LoadCredentialLifecycle(ctx, tx, fixture.connection.ConnectionID)
		return err
	})
	return lifecycle, err
}

func (fixture credentialFixture) loadEncrypted(t *testing.T, version uint64) store.EncryptedSecret {
	t.Helper()
	var encrypted store.EncryptedSecret
	err := store.WithinScope(context.Background(), fixture.pools.first, fixture.scope, fixture.actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		encrypted, err = store.LoadEncryptedSecret(ctx, tx, fixture.connection.CredentialRef, version)
		return err
	})
	if err != nil {
		t.Fatalf("load encrypted credential version: %v", err)
	}
	return encrypted
}

func (fixture credentialFixture) requireEncryptedNotFound(t *testing.T, version uint64) {
	t.Helper()
	err := store.WithinScope(context.Background(), fixture.pools.first, fixture.scope, fixture.actor, func(ctx context.Context, tx *store.Transaction) error {
		_, err := store.LoadEncryptedSecret(ctx, tx, fixture.connection.CredentialRef, version)
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("credential version %d error = %v; want ErrNotFound", version, err)
	}
}

func (fixture credentialFixture) revokeConnection(t *testing.T) {
	t.Helper()
	err := store.WithinScope(context.Background(), fixture.pools.first, fixture.scope, fixture.actor, func(ctx context.Context, tx *store.Transaction) error {
		_, unchanged, err := store.RevokeConnectionControl(ctx, tx, fixture.connection.ConnectionID, fixture.connection.Revision)
		if err == nil && unchanged {
			return errors.New("credential fixture connection was already revoked")
		}
		return err
	})
	if err != nil {
		t.Fatalf("revoke credential fixture connection: %T", err)
	}
}

func (fixture credentialFixture) mutateAccountWitness(t *testing.T, witness string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, fixture.pools.credentials.migratorDSN)
	if err != nil {
		t.Fatal("connect account witness fault injector")
	}
	defer func() { _ = connection.Close(context.Background()) }()
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatal("begin account witness fault injection")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `
		SELECT set_config('pm.org_id', $1, true),
		       set_config('pm.workspace_id', $2, true)
	`, fixture.scope.OrgID, fixture.scope.WorkspaceID); err != nil {
		t.Fatal("install account witness fault injection scope")
	}
	changed, err := tx.Exec(ctx, `
		UPDATE pm.provider_accounts
		SET account_witness = $3, revision = revision + 1
		WHERE org_id = $1 AND provider_account_id = $2
	`, fixture.scope.OrgID, fixture.account.AccountID, witness)
	if err != nil {
		t.Fatalf("mutate provider account witness: %T", err)
	}
	if changed.RowsAffected() != 1 {
		t.Fatal("account witness fault injection did not change its owned account")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal("commit account witness fault injection")
	}
}

func (fixture credentialFixture) effect() EffectAccess {
	return EffectAccess{
		ExecutionGrantRef:  "grant-1",
		OperationID:        "github.issues.refresh",
		OperationRevision:  "revision-1",
		EffectRef:          "effect-1",
		DispatchID:         "dispatch-1",
		AttemptID:          "attempt-1",
		PermitRef:          "permit-1",
		ApprovalRef:        "approval-1",
		Deadline:           time.Now().UTC().Add(30 * time.Second),
		ReconciliationOnly: false,
	}
}

func (fixture credentialFixture) refreshRequest(binding contract.CredentialRef, leaseID, ownerID string, ttl time.Duration) RefreshRequest {
	return RefreshRequest{
		Scope:         fixture.scope,
		Actor:         fixture.actor,
		Binding:       binding,
		Effect:        fixture.effect(),
		LeaseID:       leaseID,
		OwnerID:       ownerID,
		ExpectedFence: 0,
		LeaseTTL:      ttl,
	}
}
