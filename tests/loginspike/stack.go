package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

const applicationDatabase = "pm_app"

// stackInputs are the facts of the job-owned stack that the spike reads. The
// layout follows the shared acceptance harness (tests/acceptance at e8747e5).
type stackInputs struct {
	document         map[string]json.RawMessage
	secretsDir       string
	postgresRef      string
	temporalRef      string
	artifactsRef     string
	endpoint         *url.URL
	migratorPassword string
	adminPassword    string
}

func readStackInputs() (stackInputs, error) {
	environment := map[string]string{}
	for _, name := range []string{"PM_STACK_TEST_CONFIG", "PM_STACK_SECRETS_DIR", "PM_STORE_TEST_SECRETS_DIR"} {
		value := os.Getenv(name)
		if value == "" || !filepath.IsAbs(value) {
			return stackInputs{}, fmt.Errorf("%s must name an absolute path", name)
		}
		environment[name] = value
	}
	contents, err := readBoundedFile(environment["PM_STACK_TEST_CONFIG"], 1<<20)
	if err != nil {
		return stackInputs{}, err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(contents, &document); err != nil || document == nil {
		return stackInputs{}, errors.New("the stack configuration is not a JSON object")
	}
	var sections struct {
		Postgres struct {
			EndpointRef string `json:"endpoint_ref"`
		} `json:"postgres"`
		Temporal struct {
			ClientIdentityRef string `json:"client_identity_ref"`
		} `json:"temporal"`
		Artifacts struct {
			CredentialRef string `json:"credential_ref"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(contents, &sections); err != nil {
		return stackInputs{}, fmt.Errorf("decode the stack configuration: %w", err)
	}
	read := func(member string) (string, error) {
		value, err := readBoundedFile(filepath.Join(environment["PM_STORE_TEST_SECRETS_DIR"], member), 64<<10)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(value)), nil
	}
	endpointText, err := read("pm-app-endpoint")
	if err != nil {
		return stackInputs{}, err
	}
	endpoint, err := url.Parse(endpointText)
	if err != nil || endpoint.User == nil || endpoint.User.Username() != store.RuntimeRole {
		return stackInputs{}, errors.New("the stack PostgreSQL endpoint is not the runtime role")
	}
	migrator, err := read("pm-app-migrator-password")
	if err != nil {
		return stackInputs{}, err
	}
	admin, err := read("superuser-password")
	if err != nil {
		return stackInputs{}, err
	}
	for _, value := range []string{migrator, admin} {
		report.secret(value)
	}
	if password, ok := endpoint.User.Password(); ok {
		report.secret(password)
	}
	return stackInputs{
		document:         document,
		secretsDir:       environment["PM_STACK_SECRETS_DIR"],
		postgresRef:      sections.Postgres.EndpointRef,
		temporalRef:      sections.Temporal.ClientIdentityRef,
		artifactsRef:     sections.Artifacts.CredentialRef,
		endpoint:         endpoint,
		migratorPassword: migrator,
		adminPassword:    admin,
	}, nil
}

func (s stackInputs) dsn(user, password string) string {
	endpoint := *s.endpoint
	endpoint.User = url.UserPassword(user, password)
	endpoint.Path = "/" + applicationDatabase
	return endpoint.String()
}

func (s stackInputs) migrate(ctx context.Context) error {
	return store.Migrate(ctx, s.dsn(store.MigratorRole, s.migratorPassword))
}

// seed is the operator setup of one deployment: the workspace, the user, its
// bootstrap login identity, the provider account, one GitHub connection and a
// membership that may store the connection credential. The product has no
// operator command for these rows yet (issues #284 and #285). The statements
// follow internal/server/integration_fixture_test.go.
type seed struct {
	deploymentID      string
	issuer            string
	subject           string
	orgID             string
	workspaceID       string
	principalID       string
	providerAccountID string
	accountWitness    string
	connectionID      string
	credentialRef     string
}

func newSeed(deploymentID, issuer, subject string) seed {
	token := randomHex(4)
	return seed{
		deploymentID:      deploymentID,
		issuer:            issuer,
		subject:           subject,
		orgID:             "org-spike-" + token,
		workspaceID:       "ws-spike-" + token,
		principalID:       "user-spike-" + token,
		providerAccountID: "provider-account-spike-" + token,
		accountWitness:    "witness-spike-" + token,
		connectionID:      "connection-spike-" + token,
		credentialRef:     "credential-spike-" + token,
	}
}

func (s stackInputs) applySeed(ctx context.Context, value seed) error {
	connection, err := pgx.Connect(ctx, s.dsn(store.MigratorRole, s.migratorPassword))
	if err != nil {
		return errors.New("connect the seed transaction")
	}
	defer connection.Close(context.Background())
	tx, err := connection.Begin(ctx)
	if err != nil {
		return errors.New("begin the seed transaction")
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `
		SELECT set_config('pm.deployment_id', $1, true),
		       set_config('pm.org_id', $2, true),
		       set_config('pm.workspace_id', $3, true),
		       set_config('pm.requested_actor_id', $4, true),
		       set_config('pm.requested_actor_kind', 'user', true),
		       set_config('pm.effective_actor_id', $4, true),
		       set_config('pm.effective_actor_kind', 'user', true)
	`, value.deploymentID, value.orgID, value.workspaceID, value.principalID); err != nil {
		return fmt.Errorf("bind the seed scope: %w", safeSQLError(err))
	}
	permissions := []string{
		"connection.credentials.manage", "connection.manage", "connection.read",
		"operation.invoke", "workspace.access.manage", "workspace.access.read", "workspace.read",
	}
	resourceGrants := []string{"connection:" + value.connectionID}
	for _, permission := range permissions {
		resourceGrants = append(resourceGrants, "permission:"+permission)
	}
	statements := []struct {
		name string
		sql  string
		args []any
	}{
		{"workspace", `INSERT INTO pm.workspace_namespaces (org_id, workspace_id, name) VALUES ($1, $2, 'Login spike workspace')`,
			[]any{value.orgID, value.workspaceID}},
		{"identity", `INSERT INTO pm.identities (org_id, identity_id, kind, issuer, subject, revision) VALUES ($1, $2, 'user', $3, $4, 1)`,
			[]any{value.orgID, value.principalID, value.issuer, value.subject}},
		{"provider account", `INSERT INTO pm.provider_accounts (org_id, provider_account_id, provider, account_witness, capacity, revision) VALUES ($1, $2, 'github', $3, 8, 1)`,
			[]any{value.orgID, value.providerAccountID, value.accountWitness}},
		{"membership", `INSERT INTO pm.memberships (org_id, workspace_id, membership_id, identity_id, role, resource_grants, permissions, connection_ids, operation_ids, policy_epoch, control_revision) VALUES ($1, $2, $3, $4, 'admin', $5, $6, $7, $8, 1, 1)`,
			[]any{value.orgID, value.workspaceID, "membership-" + value.principalID, value.principalID, resourceGrants, permissions, []string{value.connectionID}, []string{}}},
		{"connection", `INSERT INTO pm.connections (org_id, workspace_id, connection_id, connector_id, provider_account_id, revision, credential_ref, display_name, state) VALUES ($1, $2, $3, 'github', $4, 1, $5, 'Login spike connection', 'active')`,
			[]any{value.orgID, value.workspaceID, value.connectionID, value.providerAccountID, value.credentialRef}},
		{"bootstrap identity", `INSERT INTO pm.auth_bootstrap_identities (deployment_id, issuer, subject, org_id, principal_id, principal_kind, can_approve_devices) VALUES ($1, $2, $3, $4, $5, 'user', true)`,
			[]any{value.deploymentID, value.issuer, value.subject, value.orgID, value.principalID}},
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			return fmt.Errorf("seed %s: %w", statement.name, safeSQLError(err))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the seed: %w", safeSQLError(err))
	}
	return nil
}

// storedCredential reads the stored ciphertext rows of one credential with the
// stack administrator role, which bypasses row-level security. It returns the
// number of rows and whether any ciphertext holds the plaintext value.
func (s stackInputs) storedCredential(ctx context.Context, value seed, plaintext []byte) (int, bool, error) {
	connection, err := pgx.Connect(ctx, s.dsn("pm_stack_admin", s.adminPassword))
	if err != nil {
		return 0, false, errors.New("connect the administrator reader")
	}
	defer connection.Close(context.Background())
	rows, err := connection.Query(ctx, `
		SELECT ciphertext FROM pm.encrypted_secrets
		WHERE org_id = $1 AND workspace_id = $2 AND credential_ref = $3
	`, value.orgID, value.workspaceID, value.credentialRef)
	if err != nil {
		return 0, false, fmt.Errorf("read stored credential rows: %w", safeSQLError(err))
	}
	defer rows.Close()
	count, found := 0, false
	for rows.Next() {
		var ciphertext []byte
		if err := rows.Scan(&ciphertext); err != nil {
			return 0, false, errors.New("scan a stored credential row")
		}
		count++
		found = found || containsAny(ciphertext, plaintext)
	}
	return count, found, rows.Err()
}

// safeSQLError keeps only the PostgreSQL error code and message class. It never
// returns the statement arguments.
func safeSQLError(err error) error {
	type coder interface{ SQLState() string }
	var withCode coder
	if errors.As(err, &withCode) {
		return fmt.Errorf("SQLSTATE %s", withCode.SQLState())
	}
	return errors.New("database error")
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s", filepath.Base(path))
	}
	defer file.Close()
	information, err := file.Stat()
	if err != nil || !information.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(contents)) > limit || len(contents) == 0 {
		return nil, fmt.Errorf("%s is empty, unreadable or too large", filepath.Base(path))
	}
	return contents, nil
}

func writePrivateFile(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s", filepath.Base(path))
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s", filepath.Base(path))
	}
	return file.Close()
}

func secretBundlePath(reference string) (string, error) {
	parsed, err := url.Parse(reference)
	if err != nil || parsed.Scheme != "secret" || parsed.Host == "" {
		return "", fmt.Errorf("secret reference %q is invalid", reference)
	}
	relative := filepath.Clean(filepath.FromSlash(parsed.Host + "/" + strings.TrimPrefix(parsed.Path, "/")))
	if !filepath.IsLocal(relative) {
		return "", fmt.Errorf("secret reference %q leaves the secret root", reference)
	}
	return relative, nil
}

func writeSecretMember(root, bundle, member string, contents []byte) error {
	directory := filepath.Join(root, bundle)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create secret bundle %s", bundle)
	}
	return writePrivateFile(filepath.Join(directory, member), contents)
}

func randomHex(size int) string {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		panic("read random bytes")
	}
	return hex.EncodeToString(value)
}

// randomToken returns size random characters from a 64-character alphabet
// (6 bits each, no modulo bias).
func randomToken(size int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		panic("read random bytes")
	}
	for index, b := range value {
		value[index] = alphabet[b&63]
	}
	return string(value)
}

func elapsed(start time.Time) string {
	return fmt.Sprintf("%.1fs", time.Since(start).Seconds())
}
