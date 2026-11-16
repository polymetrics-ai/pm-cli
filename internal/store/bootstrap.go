package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// The operator bootstrap is the actor of its audit record (ADR 0006).
const (
	BootstrapActorKind = "operator"
	BootstrapActorID   = "bootstrap"
)

var (
	// ErrBootstrapScopeExists means that the deployment and organization
	// already have a committed bootstrap record.
	ErrBootstrapScopeExists = errors.New("bootstrap scope is already bootstrapped")
	// ErrBootstrapLoginBound means that the admin issuer and subject already
	// have a login binding in the deployment, for any organization.
	ErrBootstrapLoginBound = errors.New("bootstrap admin login is already bound")
	// ErrBootstrapAuthority means that PostgreSQL refused the bootstrap
	// credential, or that its role is not the bootstrap role with its limits.
	ErrBootstrapAuthority = errors.New("bootstrap requires operator authority")
)

// OpenBootstrapPool opens a pool for the operator bootstrap role. It refuses
// any other role or database, and a role with more authority than the
// bootstrap role may have. The running server never opens this pool.
func OpenBootstrapPool(ctx context.Context, connectionString string) (*Pool, error) {
	if ctx == nil {
		return nil, errors.New("pool context is required")
	}
	parsed, err := pgxpool.ParseConfig(connectionString)
	if err != nil || parsed.ConnConfig.Database != ApplicationDatabase || parsed.ConnConfig.User != BootstrapRole {
		return nil, ErrBootstrapAuthority
	}
	configurePool(parsed, PoolConfig{
		MaxConns:          2,
		MinConns:          0,
		MaxConnLifetime:   5 * time.Minute,
		MaxConnIdleTime:   time.Minute,
		HealthCheckPeriod: time.Minute,
	})
	inner, err := pgxpool.NewWithConfig(ctx, parsed)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL bootstrap pool: %w", err)
	}
	pool := &Pool{inner: inner}
	if err := pool.verifyBootstrapIdentity(ctx); err != nil {
		inner.Close()
		return nil, err
	}
	return pool, nil
}

func (pool *Pool) verifyBootstrapIdentity(ctx context.Context) error {
	var database, role string
	var superuser, inherit, createRole, createDB, bypassRLS, replication, migratorMember, runtimeMember, databaseCreate, databaseTemporary bool
	err := pool.inner.QueryRow(ctx, `
		SELECT current_database(), current_user,
		       roles.rolsuper, roles.rolinherit, roles.rolcreaterole, roles.rolcreatedb,
		       roles.rolbypassrls, roles.rolreplication,
		       pg_has_role(current_user, 'pm_app_migrator', 'MEMBER'),
		       pg_has_role(current_user, 'pm_app_runtime', 'MEMBER'),
		       has_database_privilege(current_user, current_database(), 'CREATE'),
		       has_database_privilege(current_user, current_database(), 'TEMP')
		FROM pg_roles AS roles
		WHERE roles.rolname = current_user
	`).Scan(
		&database, &role,
		&superuser, &inherit, &createRole, &createDB, &bypassRLS, &replication,
		&migratorMember, &runtimeMember, &databaseCreate, &databaseTemporary,
	)
	if err != nil {
		return bootstrapStoreError("verify bootstrap store identity", err)
	}
	if database != ApplicationDatabase || role != BootstrapRole ||
		superuser || inherit || createRole || createDB || bypassRLS || replication ||
		migratorMember || runtimeMember || databaseCreate || databaseTemporary {
		return ErrBootstrapAuthority
	}
	return nil
}

// BootstrapRequest holds every row of one operator bootstrap. The caller
// makes the identifiers. The request holds no secret.
type BootstrapRequest struct {
	DeploymentID     string
	Scope            contract.Scope
	WorkspaceName    string
	AdminIdentityID  string
	AdminIssuer      string
	AdminSubject     string
	MembershipID     string
	Role             string
	Permissions      []string
	ProviderAccounts []BootstrapProviderAccount
	AuditID          string
	Action           string

	// step is a developer-check seam. Only tests in this package set it.
	step func(context.Context, bootstrapStep) error
}

type BootstrapProviderAccount struct {
	AccountID   string
	ConnectorID string
	DisplayName string
	Capacity    int64
}

type bootstrapStep string

const (
	bootstrapStepChecked bootstrapStep = "checked"
	bootstrapStepWritten bootstrapStep = "written"
)

func (request BootstrapRequest) validate() error {
	if !validStoreID(request.DeploymentID) || request.Scope.Validate() != nil || !validStoreID(request.WorkspaceName) ||
		!validStoreID(request.AdminIdentityID) || !validStoreID(request.AdminIssuer) || !validStoreID(request.AdminSubject) ||
		!validStoreID(request.MembershipID) || !validStoreName(request.Role) || !validStoreID(request.AuditID) ||
		!validStoreName(request.Action) || !validPermissionList(request.Permissions) {
		return errors.New("bootstrap request is invalid")
	}
	seen := make(map[string]struct{}, len(request.ProviderAccounts))
	for _, account := range request.ProviderAccounts {
		if !validStoreID(account.AccountID) || !validStoreName(account.ConnectorID) ||
			!validDisplayName(account.DisplayName) || account.Capacity <= 0 {
			return errors.New("bootstrap provider account is invalid")
		}
		if _, duplicate := seen[account.AccountID]; duplicate {
			return errors.New("bootstrap provider account is duplicated")
		}
		seen[account.AccountID] = struct{}{}
	}
	return nil
}

// Bootstrap writes the first workspace of an organization, its first user
// identity with the login binding, the admin membership, the provider
// accounts, the bootstrap record and one audit record in one transaction. The
// bootstrap record comes first: its primary key is the point where a
// concurrent bootstrap of the same scope waits and then fails.
func Bootstrap(ctx context.Context, pool *Pool, request BootstrapRequest) error {
	if ctx == nil {
		return errors.New("bootstrap context is required")
	}
	if err := request.validate(); err != nil {
		return err
	}
	resourceGrants := make([]string, 0, len(request.Permissions))
	for _, permission := range request.Permissions {
		resourceGrants = append(resourceGrants, "permission:"+permission)
	}
	slices.Sort(resourceGrants)
	payload, err := json.Marshal(contract.SafePayload{
		References: []contract.SafeReference{
			{Name: "deployment", Ref: request.DeploymentID},
			{Name: "admin_identity", Ref: request.AdminIdentityID},
			{Name: "membership", Ref: request.MembershipID},
		},
		Counts:      []contract.SafeCount{{Name: "provider_accounts", Value: uint64(len(request.ProviderAccounts))}},
		ReasonCodes: []string{},
	})
	if err != nil {
		return fmt.Errorf("encode bootstrap audit payload: %w", err)
	}
	err = WithinTransaction(ctx, pool, func(ctx context.Context, tx *Transaction) error {
		if _, err := tx.exec(ctx, `
			SELECT set_config('pm.deployment_id', $1, true),
			       set_config('pm.org_id', $2, true),
			       set_config('pm.workspace_id', $3, true),
			       set_config('pm.requested_actor_id', $4, true),
			       set_config('pm.requested_actor_kind', $5, true),
			       set_config('pm.effective_actor_id', $4, true),
			       set_config('pm.effective_actor_kind', $5, true)
		`, request.DeploymentID, request.Scope.OrgID, request.Scope.WorkspaceID, BootstrapActorID, BootstrapActorKind); err != nil {
			return fmt.Errorf("install bootstrap scope: %w", err)
		}
		var scopeBootstrapped, loginBound bool
		if err := tx.queryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM pm.bootstrap_records WHERE deployment_id = $1 AND org_id = $2),
			       EXISTS (SELECT 1 FROM pm.auth_bootstrap_identities WHERE deployment_id = $1 AND issuer = $3 AND subject = $4)
		`, request.DeploymentID, request.Scope.OrgID, request.AdminIssuer, request.AdminSubject).Scan(&scopeBootstrapped, &loginBound); err != nil {
			return fmt.Errorf("check bootstrap scope: %w", err)
		}
		if scopeBootstrapped {
			return ErrBootstrapScopeExists
		}
		if loginBound {
			return ErrBootstrapLoginBound
		}
		if err := request.runStep(ctx, bootstrapStepChecked); err != nil {
			return err
		}
		statements := []bootstrapStatement{
			{"insert bootstrap record", `
				INSERT INTO pm.bootstrap_records (deployment_id, org_id, workspace_id, admin_identity_id, membership_id)
				VALUES ($1, $2, $3, $4, $5)`,
				[]any{request.DeploymentID, request.Scope.OrgID, request.Scope.WorkspaceID, request.AdminIdentityID, request.MembershipID}},
			{"insert workspace", `
				INSERT INTO pm.workspace_namespaces (org_id, workspace_id, name) VALUES ($1, $2, $3)`,
				[]any{request.Scope.OrgID, request.Scope.WorkspaceID, request.WorkspaceName}},
			{"insert admin identity", `
				INSERT INTO pm.identities (org_id, identity_id, kind, issuer, subject, revision)
				VALUES ($1, $2, 'user', $3, $4, 1)`,
				[]any{request.Scope.OrgID, request.AdminIdentityID, request.AdminIssuer, request.AdminSubject}},
			{"insert admin login binding", `
				INSERT INTO pm.auth_bootstrap_identities (
					deployment_id, issuer, subject, org_id, principal_id, principal_kind, can_approve_devices, enabled
				) VALUES ($1, $2, $3, $4, $5, 'user', true, true)`,
				[]any{request.DeploymentID, request.AdminIssuer, request.AdminSubject, request.Scope.OrgID, request.AdminIdentityID}},
			{"insert admin membership", `
				INSERT INTO pm.memberships (
					org_id, workspace_id, membership_id, identity_id, role,
					resource_grants, permissions, connection_ids, operation_ids, policy_epoch, control_revision
				) VALUES ($1, $2, $3, $4, $5, $6, $7, ARRAY[]::text[], ARRAY[]::text[], 1, 1)`,
				[]any{request.Scope.OrgID, request.Scope.WorkspaceID, request.MembershipID, request.AdminIdentityID,
					request.Role, resourceGrants, request.Permissions}},
		}
		for _, account := range request.ProviderAccounts {
			statements = append(statements, bootstrapStatement{"insert provider account", `
				INSERT INTO pm.provider_accounts (
					org_id, provider_account_id, provider, account_witness, capacity, revision, display_name
				) VALUES ($1, $2, $3, $2, $4, 1, $5)`,
				[]any{request.Scope.OrgID, account.AccountID, account.ConnectorID, account.Capacity, account.DisplayName}})
		}
		statements = append(statements, bootstrapStatement{"insert bootstrap audit record", `
			INSERT INTO pm.workspace_audit_entries (
				org_id, workspace_id, audit_id, requested_actor_id, requested_actor_kind,
				effective_actor_id, effective_actor_kind, action, target_ref, payload
			) VALUES ($1, $2, $3, $4, $5, $4, $5, $6, $2, $7)`,
			[]any{request.Scope.OrgID, request.Scope.WorkspaceID, request.AuditID, BootstrapActorID, BootstrapActorKind,
				request.Action, json.RawMessage(payload)}})
		for _, statement := range statements {
			if _, err := tx.exec(ctx, statement.sql, statement.arguments...); err != nil {
				return bootstrapStoreError(statement.action, err)
			}
		}
		return request.runStep(ctx, bootstrapStepWritten)
	})
	return err
}

type bootstrapStatement struct {
	action    string
	sql       string
	arguments []any
}

func (request BootstrapRequest) runStep(ctx context.Context, step bootstrapStep) error {
	if request.step == nil {
		return nil
	}
	return request.step(ctx, step)
}

// bootstrapStoreError maps the PostgreSQL refusals that have a bootstrap
// meaning. A unique violation of the bootstrap record or of the login binding
// comes from a concurrent bootstrap that committed first.
func bootstrapStoreError(action string, err error) error {
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) {
		return fmt.Errorf("%s: %w", action, err)
	}
	switch {
	case databaseError.Code == "23505" && databaseError.ConstraintName == "bootstrap_records_pkey":
		return ErrBootstrapScopeExists
	case databaseError.Code == "23505" && databaseError.ConstraintName == "auth_bootstrap_identities_pkey":
		return ErrBootstrapLoginBound
	case databaseError.Code == "42501" || strings.HasPrefix(databaseError.Code, "28"):
		return ErrBootstrapAuthority
	default:
		return fmt.Errorf("%s: %w", action, err)
	}
}
