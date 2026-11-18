package store

import (
	"context"
	"errors"
	"fmt"
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
	// already have a committed bootstrap of the same operator input.
	ErrBootstrapScopeExists = errors.New("bootstrap scope is already bootstrapped")
	// ErrBootstrapScopeOtherInput means that the deployment and organization
	// already have a committed bootstrap of another operator input, or that
	// the organization already has rows. errors.Is also matches it as
	// ErrBootstrapScopeExists.
	ErrBootstrapScopeOtherInput = fmt.Errorf("%w with another input", ErrBootstrapScopeExists)
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
	// The connection and the identity check come before any transaction. A
	// PostgreSQL that does not answer must not hold the bootstrap, so this work
	// has its own limit. Nothing is written yet, so its end is a plain failure.
	identityCtx, cancel := context.WithTimeout(ctx, bootstrapIdentityLimit)
	defer cancel()
	if err := pool.verifyBootstrapIdentity(identityCtx); err != nil {
		inner.Close()
		return nil, err
	}
	return pool, nil
}

// bootstrapIdentityLimit bounds the connection and the identity check of the
// bootstrap pool. Only a Go developer check in this package changes it.
var bootstrapIdentityLimit = 30 * time.Second

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
// accounts, the bootstrap record and one audit record. The bootstrap role
// has no table grant: one call of the definer function pm.operator_bootstrap
// checks the scope and the login and writes every row (ADR 0006). The
// function computes the input digest itself, so a refusal of a taken scope
// tells the same input from another one.
func Bootstrap(ctx context.Context, pool *Pool, request BootstrapRequest) error {
	if ctx == nil {
		return errors.New("bootstrap context is required")
	}
	if err := request.validate(); err != nil {
		return err
	}
	accountIDs := make([]string, 0, len(request.ProviderAccounts))
	connectors := make([]string, 0, len(request.ProviderAccounts))
	names := make([]string, 0, len(request.ProviderAccounts))
	capacities := make([]int64, 0, len(request.ProviderAccounts))
	for _, account := range request.ProviderAccounts {
		accountIDs = append(accountIDs, account.AccountID)
		connectors = append(connectors, account.ConnectorID)
		names = append(names, account.DisplayName)
		capacities = append(capacities, account.Capacity)
	}
	return WithinTransaction(ctx, pool, func(ctx context.Context, tx *Transaction) error {
		if err := request.runStep(ctx, bootstrapStepChecked); err != nil {
			return err
		}
		var outcome string
		if err := tx.queryRow(ctx, `
			SELECT pm.operator_bootstrap($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		`, request.DeploymentID, request.Scope.OrgID, request.Scope.WorkspaceID, request.WorkspaceName,
			request.AdminIdentityID, request.AdminIssuer, request.AdminSubject, request.MembershipID,
			request.Role, request.Permissions, accountIDs, connectors, names, capacities,
			request.AuditID, request.Action).Scan(&outcome); err != nil {
			return bootstrapStoreError("run the operator bootstrap", err)
		}
		switch outcome {
		case "bootstrapped":
		case "scope_same_input":
			return ErrBootstrapScopeExists
		case "scope_other_input":
			return ErrBootstrapScopeOtherInput
		case "login_bound":
			return ErrBootstrapLoginBound
		default:
			return fmt.Errorf("operator bootstrap returned the unknown outcome %q", outcome)
		}
		return request.runStep(ctx, bootstrapStepWritten)
	})
}

func (request BootstrapRequest) runStep(ctx context.Context, step bootstrapStep) error {
	if request.step == nil {
		return nil
	}
	return request.step(ctx, step)
}

// bootstrapStoreError maps the PostgreSQL refusals that have a bootstrap
// meaning. pm.operator_bootstrap already turns a unique violation of the
// bootstrap record or of the login binding into its outcome. These two cases
// stay for a refusal outside the function. A bootstrap record conflict
// outside the function had no input digest compare, so it does not claim the
// same input or another input. Its outcome is unknown: a run with the same
// input gets the digest compare of the function.
func bootstrapStoreError(action string, err error) error {
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) {
		return fmt.Errorf("%s: %w", action, err)
	}
	switch {
	case databaseError.Code == "23505" && databaseError.ConstraintName == "bootstrap_records_pkey":
		return fmt.Errorf("%s: bootstrap record conflict with no input digest compare: %w", action, ErrCommitOutcomeUnknown)
	case databaseError.Code == "23505" && databaseError.ConstraintName == "auth_bootstrap_identities_pkey":
		return ErrBootstrapLoginBound
	case databaseError.Code == "42501" || strings.HasPrefix(databaseError.Code, "28"):
		return ErrBootstrapAuthority
	default:
		return fmt.Errorf("%s: %w", action, err)
	}
}
