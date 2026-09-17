package testfixture

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"golang.org/x/oauth2"
)

const (
	fixtureTimeout = 30 * time.Second
	requestTimeout = 10 * time.Second
)

// ConnectionRef identifies one fixture-owned connection and its organization-
// owned provider account. It contains no provider credential material.
type ConnectionRef struct {
	ConnectionID      string
	ConnectorID       string
	ProviderAccountID string
	AccountWitness    string
	CredentialRef     string
}

// ServiceDependencies are the durable inputs available while the HTTP service
// is being constructed. Pool is the newly opened pool on every restart.
type ServiceDependencies struct {
	Pool              *store.Pool
	DeploymentID      string
	Scope             contract.Scope
	ForeignScope      contract.Scope
	Actor             contract.Actor
	ForeignActor      contract.Actor
	WrongActor        contract.Actor
	Connection        ConnectionRef
	ForeignConnection ConnectionRef
	OperationID       string
}

// ConfigureService may bind real application services into the HTTP service
// configuration. Core authentication, identity, credential, origin, and OIDC
// dependencies must remain the fixture's real instances.
type ConfigureService func(ServiceDependencies, *httpapi.ServiceConfig) error

// Fixture owns two isolated workspace scopes in one isolated organization, a
// runtime pool, three persisted PM sessions, and one real Gin server behind TLS.
type Fixture struct {
	// Pool is owned by the fixture. Restart closes it and replaces it with a new
	// store.OpenPool result against the same selected PostgreSQL fixture.
	Pool *store.Pool

	DeploymentID      string
	Scope             contract.Scope
	ForeignScope      contract.Scope
	Actor             contract.Actor
	ForeignActor      contract.Actor
	WrongActor        contract.Actor
	Connection        ConnectionRef
	ForeignConnection ConnectionRef
	OperationID       string
	WorkerRegistry    *app.HandlerRegistry
	ResolveOperation  app.OperationResolver

	// Client authenticates Actor with a real opaque device-session bearer token.
	// The token is retained privately and is never returned or logged.
	Client  *http.Client
	BaseURL string

	runtimeDSN    string
	migratorDSN   string
	configure     ConfigureService
	server        *httptest.Server
	sessionTokens map[contract.Principal]string
	clients       []*http.Client
	seeded        bool
	closed        bool
}

// String redacts fixture database and session material.
func (*Fixture) String() string { return "authenticated PostgreSQL/Gin fixture [REDACTED]" }

// GoString redacts fixture database and session material.
func (fixture *Fixture) GoString() string { return fixture.String() }

// New creates one isolated fixture. At most one optional service configurator
// may be supplied. Its default admission service is real but truthfully has no
// operation resolver or registered execution capability.
func New(t testing.TB, configure ...ConfigureService) *Fixture {
	t.Helper()
	runtimeDSN, migratorDSN := loadStoreCredentials(t)
	suffix := randomHex(t, 8)
	organizationID := "org-run-api-" + suffix
	fixture := &Fixture{
		DeploymentID: "run-api-" + suffix,
		Scope: contract.Scope{
			OrgID:       organizationID,
			WorkspaceID: "workspace-primary-" + suffix,
		},
		ForeignScope: contract.Scope{
			OrgID:       organizationID,
			WorkspaceID: "workspace-foreign-" + suffix,
		},
		OperationID: "fixture.read",
		runtimeDSN:  runtimeDSN,
		migratorDSN: migratorDSN,
		configure:   selectConfigurator(t, nil, configure),
	}
	primary := contract.Principal{OrgID: organizationID, PrincipalID: "actor-primary-" + suffix, Kind: contract.PrincipalUser}
	foreign := contract.Principal{OrgID: organizationID, PrincipalID: "actor-foreign-" + suffix, Kind: contract.PrincipalUser}
	wrong := contract.Principal{OrgID: organizationID, PrincipalID: "actor-wrong-" + suffix, Kind: contract.PrincipalUser}
	fixture.Actor = directActor(primary)
	fixture.ForeignActor = directActor(foreign)
	fixture.WrongActor = directActor(wrong)
	providerAccountID := "provider-account-" + suffix
	accountWitness := "account-witness-" + suffix
	fixture.Connection = ConnectionRef{
		ConnectionID:      "connection-primary-" + suffix,
		ConnectorID:       "fixture-connector",
		ProviderAccountID: providerAccountID,
		AccountWitness:    accountWitness,
		CredentialRef:     "credential-primary-" + suffix,
	}
	fixture.ForeignConnection = ConnectionRef{
		ConnectionID:      "connection-foreign-" + suffix,
		ConnectorID:       "fixture-connector",
		ProviderAccountID: providerAccountID,
		AccountWitness:    accountWitness,
		CredentialRef:     "credential-foreign-" + suffix,
	}

	t.Cleanup(func() { fixture.Close(t) })
	fixture.migrate(t)
	fixture.seed(t)
	fixture.seeded = true
	fixture.open(t)
	return fixture
}

// NewAdmitting creates the controlled positive admission fixture. Its execution
// callback proves local capability reachability; admission never executes it.
// One optional service configurator runs after the real admission service is set.
func NewAdmitting(t testing.TB, configure ...ConfigureService) *Fixture {
	t.Helper()
	return newAdmitting(t, nil, nil, selectConfigurator(t, nil, configure))
}

// NewExecuting creates the same real authenticated admission fixture and also
// exposes the exact operation registry and resolver to a production worker.
func NewExecuting(
	t testing.TB,
	execute app.ExecutionCallback,
	reconcile app.ExecutionCallback,
) *Fixture {
	t.Helper()
	if execute == nil {
		t.Fatal("controlled worker execution callback is required")
	}
	return newAdmitting(t, execute, reconcile, nil)
}

func newAdmitting(
	t testing.TB,
	execute app.ExecutionCallback,
	reconcile app.ExecutionCallback,
	configure ConfigureService,
) *Fixture {
	t.Helper()
	registry, _, err := NewExecutionBoundary(ServiceDependencies{}, execute, reconcile)
	if err != nil {
		t.Fatal(err)
	}
	var resolver app.OperationResolver
	fixture := New(t, func(dependencies ServiceDependencies, configuration *httpapi.ServiceConfig) error {
		resolver = admittingResolver(dependencies)
		admission, err := app.NewAdmissionService(app.AdmissionServiceConfig{
			Pool:             dependencies.Pool,
			DeploymentID:     dependencies.DeploymentID,
			Authorize:        configuration.Auth.AuthorizeResource,
			Registry:         registry,
			ResolveOperation: resolver,
			Policy:           admittingPolicy(),
			Random:           rand.Reader,
		})
		if err != nil {
			return err
		}
		configuration.Admission = admission
		if configure != nil {
			return configure(dependencies, configuration)
		}
		return nil
	})
	fixture.WorkerRegistry = registry
	fixture.ResolveOperation = resolver
	return fixture
}

// Invocation returns a valid request for the fixture's controlled operation.
// The claimed protocol digest is deliberately inert; the server recomputes the
// admission fingerprint from this exact typed request.
func Invocation(t testing.TB, fixture *Fixture, requestKey string) api.InvocationRequest {
	t.Helper()
	if fixture == nil || requestKey == "" || len(requestKey) > 512 {
		t.Fatal("controlled admission invocation requires a fixture and bounded request key")
	}
	revision, err := app.OperationSelectionRevision(admittingSelection(fixture.OperationID))
	if err != nil {
		t.Fatalf("resolve controlled operation revision: %v", err)
	}
	return api.InvocationRequest{
		ConnectionRef:  fixture.Connection.ConnectionID,
		OperationRef:   fixture.OperationID,
		Lane:           api.InvocationRequestLane(contract.LaneDirectRead),
		Input:          contract.NewObjectValue(map[string]contract.PresentValue{}),
		ResultControls: contract.ResultControls{},
		Protocol: api.ProtocolMetadata{
			WireVersion:      api.ProtocolMetadataWireVersion(api.WireVersion),
			RequiredFeatures: []string{},
			RequestKey:       requestKey,
			IntentDigest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			ExpectedRevision: revision,
		},
	}
}

// ClientFor returns another TLS client authenticated as one of the fixture's
// three direct actors. Restart and Close invalidate every previously returned
// client; call ClientFor again after Restart.
func (fixture *Fixture) ClientFor(t testing.TB, actor contract.Actor) *http.Client {
	t.Helper()
	if fixture == nil || fixture.closed || fixture.server == nil || fixture.BaseURL == "" {
		t.Fatal("authenticated client requires a running fixture")
	}
	if err := actor.Validate(); err != nil || actor.Delegation != nil ||
		!actor.RequestedActor.Equal(actor.EffectiveActor) {
		t.Fatal("authenticated fixture client requires a direct fixture actor")
	}
	token, ok := fixture.sessionTokens[actor.RequestedActor]
	if !ok {
		t.Fatal("actor is not owned by this fixture")
	}
	client := fixture.newClient(t, token)
	fixture.clients = append(fixture.clients, client)
	return client
}

// Restart closes the fixture's HTTP clients and TLS server, closes Pool, opens
// a new runtime pool, reconstructs auth/identity/credential/httpapi services
// and Gin, and starts a new TLS listener. Durable rows, deployment ID, scoped
// identities, and opaque PM sessions remain; Pool, Client, and BaseURL are
// replaced. With no argument, the construction configurator is reused.
func (fixture *Fixture) Restart(t testing.TB, configure ...ConfigureService) {
	t.Helper()
	if fixture == nil || fixture.closed {
		t.Fatal("restart requires an open test fixture")
	}
	fixture.configure = selectConfigurator(t, fixture.configure, configure)
	fixture.closeHTTP()
	if fixture.Pool != nil {
		fixture.Pool.Close()
		fixture.Pool = nil
	}
	fixture.open(t)
}

// SetupSQL runs one bounded migrator transaction under one fixture-owned scope.
// It commits only when apply returns nil; the migrator DSN is never exposed.
func (fixture *Fixture) SetupSQL(
	t testing.TB,
	scope contract.Scope,
	apply func(context.Context, pgx.Tx) error,
) {
	t.Helper()
	if fixture == nil || fixture.closed || apply == nil {
		t.Fatal("scoped SQL setup requires an open fixture and callback")
	}
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	if err := fixture.WithinSetupSQL(ctx, scope, apply); err != nil {
		t.Fatalf("apply scoped fixture setup: %v", err)
	}
}

// WithinSetupSQL runs a caller-bounded scoped migrator transaction. It exists
// only for physical fixtures that must coordinate a real database boundary.
func (fixture *Fixture) WithinSetupSQL(
	ctx context.Context,
	scope contract.Scope,
	apply func(context.Context, pgx.Tx) error,
) error {
	if fixture == nil || fixture.closed || ctx == nil || ctx.Err() != nil || apply == nil {
		return errors.New("scoped SQL setup requires an open fixture, context, and callback")
	}
	if !scope.Equal(fixture.Scope) && !scope.Equal(fixture.ForeignScope) {
		return errors.New("SQL setup scope is not owned by this fixture")
	}
	actor := fixture.Actor
	if scope.Equal(fixture.ForeignScope) {
		actor = fixture.ForeignActor
	}
	connection, err := pgx.Connect(ctx, fixture.migratorDSN)
	if err != nil {
		return fmt.Errorf("connect scoped fixture setup: %w", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	tx, err := connection.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin scoped fixture setup: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := bindScope(ctx, tx, fixture.DeploymentID, scope, actor); err != nil {
		return fmt.Errorf("bind scoped fixture setup: %w", err)
	}
	if err := apply(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit scoped fixture setup: %w", err)
	}
	return nil
}

// Close stops HTTP activity, closes the runtime pool, and removes only records
// in this fixture's deployment, organization, and two workspace namespaces.
func (fixture *Fixture) Close(t testing.TB) {
	t.Helper()
	if fixture == nil || fixture.closed {
		return
	}
	fixture.closed = true
	fixture.closeHTTP()
	if fixture.Pool != nil {
		fixture.Pool.Close()
		fixture.Pool = nil
	}
	if fixture.seeded {
		fixture.cleanup(t)
	}
	fixture.sessionTokens = nil
}

func (fixture *Fixture) migrate(t testing.TB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	if err := store.Migrate(ctx, fixture.migratorDSN); err != nil {
		t.Fatalf("migrate selected test fixture store: %T", err)
	}
}

func (fixture *Fixture) open(t testing.TB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	pool, err := store.OpenPool(ctx, store.PoolConfig{
		ConnectionString:  fixture.runtimeDSN,
		MaxConns:          8,
		MinConns:          0,
		MaxConnLifetime:   5 * time.Minute,
		MaxConnIdleTime:   time.Minute,
		HealthCheckPeriod: 30 * time.Second,
	})
	cancel()
	if err != nil {
		t.Fatalf("open selected test fixture store: %T", err)
	}
	fixture.Pool = pool

	server := httptest.NewUnstartedServer(http.NotFoundHandler())
	fixture.server = server
	origin := "https://" + server.Listener.Addr().String()
	parsedOrigin, err := url.Parse(origin)
	if err != nil {
		t.Fatal("construct fixture public origin")
	}
	oidcClient := &http.Client{Transport: rejectingOIDCTransport{}, Timeout: 2 * time.Second}
	authentication, err := auth.NewService(auth.Config{
		Pool:         fixture.Pool,
		DeploymentID: fixture.DeploymentID,
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
			AllowedHosts:   []string{parsedOrigin.Host},
			AllowedOrigins: []string{origin},
		},
		OIDC: syntheticOIDC(origin),
	})
	if err != nil {
		t.Fatalf("construct real fixture authentication service: %v", err)
	}
	identity, err := app.NewIdentityService(app.IdentityServiceConfig{
		Pool:             fixture.Pool,
		DeploymentID:     fixture.DeploymentID,
		Authorize:        authentication.AuthorizeResource,
		Random:           rand.Reader,
		ReceiptRetention: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("construct real fixture identity service: %v", err)
	}
	credentials, err := app.NewCredentialIngress(identity, app.CredentialIngressConfig{
		StepUpMaximumAge: 2 * time.Minute,
		WrappingKeyRef:   "fixture-wrapping-key-" + fixture.DeploymentID,
	})
	if err != nil {
		t.Fatalf("construct real fixture credential ingress: %v", err)
	}
	fixture.issueSessions(t, authentication)

	defaultAdmission, err := app.NewAdmissionService(app.AdmissionServiceConfig{
		Pool:             fixture.Pool,
		DeploymentID:     fixture.DeploymentID,
		Authorize:        authentication.AuthorizeResource,
		Registry:         &app.HandlerRegistry{},
		ResolveOperation: nil,
		Policy:           admittingPolicy(),
		Random:           rand.Reader,
	})
	if err != nil {
		t.Fatalf("construct truthful fixture admission service: %v", err)
	}
	runs, err := app.NewRunService(app.RunServiceConfig{
		Pool:             fixture.Pool,
		DeploymentID:     fixture.DeploymentID,
		Authorize:        authentication.AuthorizeResource,
		Random:           rand.Reader,
		ReceiptRetention: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("construct real fixture run service: %v", err)
	}
	serviceConfig := httpapi.ServiceConfig{
		PublicOrigin:   origin,
		Auth:           authentication,
		Identity:       identity,
		Credentials:    credentials,
		Admission:      defaultAdmission,
		Runs:           runs,
		InstanceID:     fixture.DeploymentID,
		OIDCHTTPClient: oidcClient,
	}
	if fixture.configure != nil {
		dependencies := ServiceDependencies{
			Pool:              fixture.Pool,
			DeploymentID:      fixture.DeploymentID,
			Scope:             fixture.Scope,
			ForeignScope:      fixture.ForeignScope,
			Actor:             fixture.Actor,
			ForeignActor:      fixture.ForeignActor,
			WrongActor:        fixture.WrongActor,
			Connection:        fixture.Connection,
			ForeignConnection: fixture.ForeignConnection,
			OperationID:       fixture.OperationID,
		}
		if err := fixture.configure(dependencies, &serviceConfig); err != nil {
			t.Fatalf("configure fixture HTTP application service: %v", err)
		}
	}
	if serviceConfig.PublicOrigin != origin || serviceConfig.Auth != authentication ||
		serviceConfig.Identity != identity || serviceConfig.Credentials != credentials ||
		serviceConfig.InstanceID != fixture.DeploymentID || serviceConfig.OIDCHTTPClient != oidcClient {
		t.Fatal("fixture service configurator replaced a core real service dependency")
	}
	service, err := httpapi.NewService(serviceConfig)
	if err != nil {
		t.Fatalf("construct real fixture HTTP service: %v", err)
	}
	router := gin.New()
	if err := httpapi.RegisterRoutes(router, httpapi.TransportPolicy{
		PublicOrigin:     origin,
		TrustedProxies:   nil,
		RequestTimeout:   requestTimeout,
		MaximumBodyBytes: 1 << 20,
	}, service, io.Discard, service.Middleware()); err != nil {
		t.Fatalf("register real fixture Gin routes: %v", err)
	}
	server.Config.Handler = router
	server.StartTLS()
	if server.URL != origin {
		t.Fatal("fixture TLS origin changed during startup")
	}
	fixture.BaseURL = server.URL
	fixture.Client = fixture.ClientFor(t, fixture.Actor)
	fixture.assertAuthenticated(t)
}

func (fixture *Fixture) issueSessions(t testing.TB, authentication *auth.Service) {
	t.Helper()
	if len(fixture.sessionTokens) != 0 {
		return
	}
	fixture.sessionTokens = make(map[contract.Principal]string, 3)
	for _, actor := range []contract.Actor{fixture.Actor, fixture.ForeignActor, fixture.WrongActor} {
		ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
		session, err := authentication.IssuePMSession(ctx, auth.SessionIssue{
			Principal: actor.RequestedActor,
			Transport: store.SessionTransportDevice,
		})
		cancel()
		if err != nil {
			t.Fatalf("issue trusted opaque fixture PM session: %v", err)
		}
		fixture.sessionTokens[actor.RequestedActor] = session.Token
	}
}

func (fixture *Fixture) newClient(t testing.TB, token string) *http.Client {
	t.Helper()
	baseTransport, ok := fixture.server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("fixture TLS transport is unavailable")
	}
	transport := baseTransport.Clone()
	transport.Proxy = nil
	origin, err := url.Parse(fixture.BaseURL)
	if err != nil {
		t.Fatal("fixture TLS origin is invalid")
	}
	return &http.Client{
		Transport: &sessionTransport{
			next:       transport,
			originHost: origin.Host,
			token:      token,
		},
		Timeout: requestTimeout,
		CheckRedirect: func(request *http.Request, previous []*http.Request) error {
			if request.URL.Scheme != "https" || request.URL.Host != origin.Host || len(previous) >= 5 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

func (fixture *Fixture) assertAuthenticated(t testing.TB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fixture.BaseURL+"/v1/workspaces/"+fixture.Scope.WorkspaceID,
		nil,
	)
	if err != nil {
		t.Fatal("construct authenticated fixture probe")
	}
	response, err := fixture.Client.Do(request)
	if err != nil {
		t.Fatalf("execute authenticated fixture probe: %T", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("authenticated fixture probe status = %d, want 200", response.StatusCode)
	}
}

func (fixture *Fixture) closeHTTP() {
	for _, client := range fixture.clients {
		client.CloseIdleConnections()
	}
	fixture.clients = nil
	fixture.Client = nil
	if fixture.server != nil {
		fixture.server.CloseClientConnections()
		fixture.server.Close()
		fixture.server = nil
	}
	fixture.BaseURL = ""
}

func (fixture *Fixture) seed(t testing.TB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	connection, err := pgx.Connect(ctx, fixture.migratorDSN)
	if err != nil {
		t.Fatalf("connect fixture seed transaction: %T", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin fixture seed transaction: %T", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := bindScope(ctx, tx, fixture.DeploymentID, fixture.Scope, fixture.Actor); err != nil {
		t.Fatalf("bind primary fixture seed scope: %T", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pm.workspace_namespaces (org_id, workspace_id, name)
		VALUES ($1, $2, 'Primary run API fixture workspace')
	`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID); err != nil {
		t.Fatalf("seed primary fixture workspace: %T", err)
	}
	for _, actor := range []contract.Actor{fixture.Actor, fixture.ForeignActor, fixture.WrongActor} {
		principal := actor.RequestedActor
		if _, err := tx.Exec(ctx, `
			INSERT INTO pm.identities (org_id, identity_id, kind, issuer, subject, revision)
			VALUES ($1, $2, $3, 'https://identity.test.invalid', $2, 1)
		`, principal.OrgID, principal.PrincipalID, string(principal.Kind)); err != nil {
			t.Fatalf("seed fixture human identity: %T", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pm.provider_accounts (
			org_id, provider_account_id, provider, account_witness, capacity, revision
		) VALUES ($1, $2, 'fixture', $3, 8, 1)
	`, fixture.Scope.OrgID, fixture.Connection.ProviderAccountID, fixture.Connection.AccountWitness); err != nil {
		t.Fatalf("seed fixture provider account: %T", err)
	}
	if err := seedWorkspace(ctx, tx, fixture.Scope, fixture.Actor, fixture.Connection, fixture.OperationID, "admin"); err != nil {
		t.Fatalf("seed primary fixture authority: %T", err)
	}

	if err := bindScope(ctx, tx, fixture.DeploymentID, fixture.ForeignScope, fixture.ForeignActor); err != nil {
		t.Fatalf("bind foreign fixture seed scope: %T", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO pm.workspace_namespaces (org_id, workspace_id, name)
		VALUES ($1, $2, 'Foreign run API fixture workspace')
	`, fixture.ForeignScope.OrgID, fixture.ForeignScope.WorkspaceID); err != nil {
		t.Fatalf("seed foreign fixture workspace: %T", err)
	}
	if err := seedWorkspace(ctx, tx, fixture.ForeignScope, fixture.ForeignActor, fixture.ForeignConnection, fixture.OperationID, "operator"); err != nil {
		t.Fatalf("seed foreign fixture authority: %T", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture seed transaction: %T", err)
	}
}

func seedWorkspace(
	ctx context.Context,
	tx pgx.Tx,
	scope contract.Scope,
	actor contract.Actor,
	connection ConnectionRef,
	operationID string,
	role string,
) error {
	permissions := []string{
		"artifact.read",
		"connection.credentials.manage",
		"connection.manage",
		"connection.read",
		"event.read",
		"operation.invoke",
		"run.control",
		"run.read",
		"workspace.access.manage",
		"workspace.access.read",
		"workspace.read",
	}
	resourceGrants := []string{
		"connection:" + connection.ConnectionID,
		"operation:" + operationID,
		"permission:artifact.read",
		"permission:connection.credentials.manage",
		"permission:connection.manage",
		"permission:connection.read",
		"permission:event.read",
		"permission:operation.invoke",
		"permission:run.control",
		"permission:run.read",
		"permission:workspace.access.manage",
		"permission:workspace.access.read",
		"permission:workspace.read",
	}
	principal := actor.RequestedActor
	if _, err := tx.Exec(ctx, `
		INSERT INTO pm.memberships (
			org_id, workspace_id, membership_id, identity_id, role,
			resource_grants, permissions, connection_ids, operation_ids,
			policy_epoch, control_revision
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 1, 1)
	`, scope.OrgID, scope.WorkspaceID, "membership-"+principal.PrincipalID, principal.PrincipalID,
		role, resourceGrants, permissions, []string{connection.ConnectionID}, []string{operationID}); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO pm.connections (
			org_id, workspace_id, connection_id, connector_id, provider_account_id,
			revision, credential_ref, display_name, state
		) VALUES ($1, $2, $3, $4, $5, 1, $6, $7, 'active')
	`, scope.OrgID, scope.WorkspaceID, connection.ConnectionID, connection.ConnectorID,
		connection.ProviderAccountID, connection.CredentialRef, role+" fixture connection")
	return err
}

func (fixture *Fixture) cleanup(t testing.TB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureTimeout)
	defer cancel()
	connection, err := pgx.Connect(ctx, fixture.migratorDSN)
	if err != nil {
		t.Errorf("connect fixture cleanup transaction: %T", err)
		return
	}
	defer func() { _ = connection.Close(context.Background()) }()
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Errorf("begin fixture cleanup transaction: %T", err)
		return
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := bindScope(ctx, tx, fixture.DeploymentID, fixture.Scope, fixture.Actor); err != nil {
		t.Errorf("bind fixture deployment cleanup: %T", err)
		return
	}
	for _, statement := range []string{
		`DELETE FROM pm.pm_sessions WHERE deployment_id = $1`,
		`DELETE FROM pm.oidc_login_challenges WHERE deployment_id = $1`,
		`DELETE FROM pm.device_login_challenges WHERE deployment_id = $1`,
		`DELETE FROM pm.device_start_windows WHERE deployment_id = $1`,
		`DELETE FROM pm.auth_bootstrap_identities WHERE deployment_id = $1`,
	} {
		if _, err := tx.Exec(ctx, statement, fixture.DeploymentID); err != nil {
			t.Errorf("clean fixture deployment records: %T", err)
			return
		}
	}
	for _, owned := range []struct {
		scope contract.Scope
		actor contract.Actor
	}{
		{scope: fixture.Scope, actor: fixture.Actor},
		{scope: fixture.ForeignScope, actor: fixture.ForeignActor},
	} {
		if err := bindScope(ctx, tx, fixture.DeploymentID, owned.scope, owned.actor); err != nil {
			t.Errorf("bind fixture workspace cleanup: %T", err)
			return
		}
		for _, statement := range []string{
			`DELETE FROM pm.workflow_reconciliation_witnesses WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.workflow_command_intents WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.credential_refresh_leases WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.credential_mutation_receipts WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.credential_lifecycles WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.encrypted_secrets WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.delegations WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.service_account_credentials WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.service_accounts WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.account_permits WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.checkpoints WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.manifests WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.artifacts WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.run_plan_bindings WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.approval_bindings WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.plan_bindings WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.identity_mutation_keys WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.identity_collection_revisions WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.memberships WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.connections WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.dispatch_attempts WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.run_specs WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.workspace_namespaces WHERE org_id = $1 AND workspace_id = $2`,
		} {
			if _, err := tx.Exec(ctx, statement, owned.scope.OrgID, owned.scope.WorkspaceID); err != nil {
				var databaseError *pgconn.PgError
				if errors.As(err, &databaseError) {
					t.Errorf("clean fixture workspace records: SQLSTATE %s table=%s constraint=%s", databaseError.Code, databaseError.TableName, databaseError.ConstraintName)
					return
				}
				t.Errorf("clean fixture workspace records: %T", err)
				return
			}
		}
	}
	if err := bindScope(ctx, tx, fixture.DeploymentID, fixture.Scope, fixture.Actor); err != nil {
		t.Errorf("bind fixture organization cleanup: %T", err)
		return
	}
	if _, err := tx.Exec(ctx, `DELETE FROM pm.identities WHERE org_id = $1`, fixture.Scope.OrgID); err != nil {
		t.Errorf("clean fixture identities: %T", err)
		return
	}
	if _, err := tx.Exec(ctx, `DELETE FROM pm.provider_accounts WHERE org_id = $1`, fixture.Scope.OrgID); err != nil {
		t.Errorf("clean fixture provider account: %T", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		t.Errorf("commit fixture cleanup transaction: %T", err)
	}
}

func bindScope(ctx context.Context, tx pgx.Tx, deploymentID string, scope contract.Scope, actor contract.Actor) error {
	_, err := tx.Exec(ctx, `
		SELECT set_config('pm.deployment_id', $1, true),
		       set_config('pm.org_id', $2, true),
		       set_config('pm.workspace_id', $3, true),
		       set_config('pm.requested_actor_id', $4, true),
		       set_config('pm.requested_actor_kind', $5, true),
		       set_config('pm.effective_actor_id', $6, true),
		       set_config('pm.effective_actor_kind', $7, true)
	`, deploymentID, scope.OrgID, scope.WorkspaceID,
		actor.RequestedActor.PrincipalID, string(actor.RequestedActor.Kind),
		actor.EffectiveActor.PrincipalID, string(actor.EffectiveActor.Kind))
	return err
}

func syntheticOIDC(publicOrigin string) auth.OIDCConfig {
	const issuer = "https://identity.test.invalid"
	return auth.OIDCConfig{
		Issuer: issuer,
		OAuth2: oauth2.Config{
			ClientID:     "pm-test-client",
			ClientSecret: "synthetic-not-used",
			Endpoint: oauth2.Endpoint{
				AuthURL:  issuer + "/authorize",
				TokenURL: issuer + "/token",
			},
			RedirectURL: publicOrigin + "/v1/sessions/browser/callback",
			Scopes:      []string{oidc.ScopeOpenID},
		},
		KeySet:   rejectingKeySet{},
		LoginTTL: 5 * time.Minute,
	}
}

func loadStoreCredentials(t testing.TB) (string, string) {
	t.Helper()
	secretsDirectory := os.Getenv("PM_STORE_TEST_SECRETS_DIR")
	if secretsDirectory == "" {
		t.Fatal("PM_STORE_TEST_SECRETS_DIR is required for the physical test fixture")
	}
	absolute, err := filepath.Abs(secretsDirectory)
	if err != nil {
		t.Fatal("resolve physical test fixture secret directory")
	}
	endpointBytes, err := os.ReadFile(filepath.Join(absolute, "pm-app-endpoint"))
	if err != nil {
		t.Fatal("read physical test fixture runtime endpoint")
	}
	defer clear(endpointBytes)
	passwordBytes, err := os.ReadFile(filepath.Join(absolute, "pm-app-migrator-password"))
	if err != nil {
		t.Fatal("read physical test fixture migrator password")
	}
	defer clear(passwordBytes)
	endpoint := strings.TrimSpace(string(endpointBytes))
	password := strings.TrimSpace(string(passwordBytes))
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.User == nil ||
		parsed.User.Username() != store.RuntimeRole || strings.TrimPrefix(parsed.Path, "/") != store.ApplicationDatabase || password == "" {
		t.Fatal("physical PostgreSQL fixture identity is invalid")
	}
	migrator := *parsed
	migrator.User = url.UserPassword(store.MigratorRole, password)
	return endpoint, migrator.String()
}

func admittingPolicy() store.RunBudgetPolicy {
	return store.RunBudgetPolicy{
		Pending: store.AdmissionLimits{
			GlobalPending:          3,
			WorkspacePending:       2,
			WorkspaceMaxPendingAge: 30 * time.Second,
		},
		RunLifetime:          time.Hour,
		InputMaxBytes:        1 << 20,
		WorkspaceActiveRuns:  4,
		WorkflowPayloadBytes: 1 << 16,
		WorkflowFanout:       32,
	}
}

func admittingResolver(dependencies ServiceDependencies) app.OperationResolver {
	return func(
		_ context.Context,
		scope contract.Scope,
		connectionID string,
		operationID string,
	) (app.OperationSelection, error) {
		matchesPrimary := scope.Equal(dependencies.Scope) && connectionID == dependencies.Connection.ConnectionID
		matchesForeign := scope.Equal(dependencies.ForeignScope) && connectionID == dependencies.ForeignConnection.ConnectionID
		if operationID != dependencies.OperationID || !matchesPrimary && !matchesForeign {
			return app.OperationSelection{}, errors.New("controlled operation is unavailable in this scope")
		}
		return admittingSelection(operationID), nil
	}
}

func admittingSelection(operationID string) app.OperationSelection {
	foundation := contract.FoundationRef{ID: "FND-6", Version: "1.0.0"}
	return app.OperationSelection{
		Operation: contract.Operation{
			SchemaVersion: contract.OperationSchemaVersion,
			OperationID:   operationID,
			SourceRefs: []contract.SourceRef{
				{SourceID: "fixture-source", Pointer: "#/operations/fixture"},
			},
			Transport: contract.Transport{Kind: "http", Method: http.MethodGet, Path: "/fixture"},
			Inputs:    []contract.FieldBinding{},
			Responses: []contract.Response{
				{Status: "200", MediaType: "application/json", Schema: contract.Schema{Type: contract.SchemaTypeString}},
			},
			CapabilityRequirements: []contract.CapabilityRequirement{
				{Capability: "http.execute", Foundation: foundation},
			},
			LaneEligibility: []contract.LaneEligibility{{Lane: contract.LaneDirectRead, Eligible: true}},
		},
		SchemaRefs: []string{"fixture-schema-v1"},
	}
}

// NewExecutionBoundary reconstructs the fixture's exact admitted operation
// capability in another real worker process. Dependencies contain only safe
// fixture identities; Pool is not used by the resolver.
func NewExecutionBoundary(
	dependencies ServiceDependencies,
	execute app.ExecutionCallback,
	reconcile app.ExecutionCallback,
) (*app.HandlerRegistry, app.OperationResolver, error) {
	registry := &app.HandlerRegistry{}
	if err := registry.RegisterHTTP(admittingRegistration(execute, reconcile)); err != nil {
		return nil, nil, err
	}
	return registry, admittingResolver(dependencies), nil
}

func admittingRegistration(
	execute app.ExecutionCallback,
	reconcile app.ExecutionCallback,
) app.HandlerRegistration {
	if execute == nil {
		execute = func(context.Context, app.ExecutionRequest) (app.ExecutionObservation, error) {
			return app.ExecutionObservation{}, errors.New("controlled admission fixture callback must not execute during admission")
		}
	}
	foundation := contract.FoundationRef{ID: "FND-6", Version: "1.0.0"}
	return app.HandlerRegistration{
		Lanes: []contract.Lane{contract.LaneDirectRead},
		Foundations: []contract.FoundationContract{
			{ID: foundation.ID, Version: foundation.Version, Dependencies: []contract.FoundationRef{}},
		},
		Capabilities: []contract.CapabilityRequirement{
			{Capability: "http.execute", Foundation: foundation},
		},
		Execute:   execute,
		Reconcile: reconcile,
	}
}

func selectConfigurator(t testing.TB, current ConfigureService, values []ConfigureService) ConfigureService {
	t.Helper()
	if len(values) > 1 {
		t.Fatal("at most one fixture service configurator is allowed")
	}
	if len(values) == 1 {
		return values[0]
	}
	return current
}

func randomHex(t testing.TB, byteCount int) string {
	t.Helper()
	value := make([]byte, byteCount)
	if _, err := rand.Read(value); err != nil {
		t.Fatal("generate isolated fixture namespace")
	}
	return hex.EncodeToString(value)
}

func directActor(principal contract.Principal) contract.Actor {
	return contract.Actor{RequestedActor: principal, EffectiveActor: principal}
}

type rejectingKeySet struct{}

func (rejectingKeySet) VerifySignature(context.Context, string) ([]byte, error) {
	return nil, errors.New("synthetic OIDC key set has no signing keys")
}

type rejectingOIDCTransport struct{}

func (rejectingOIDCTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("synthetic OIDC transport rejects network access")
}

type sessionTransport struct {
	next       *http.Transport
	originHost string
	token      string
}

func (transport *sessionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport == nil || transport.next == nil || request == nil || request.URL == nil ||
		request.URL.Scheme != "https" || request.URL.Host != transport.originHost || request.URL.User != nil {
		return nil, errors.New("authenticated fixture client target is outside its TLS server")
	}
	clone := request.Clone(request.Context())
	clone.Header.Set("Authorization", "Bearer "+transport.token)
	return transport.next.RoundTrip(clone)
}

func (transport *sessionTransport) CloseIdleConnections() {
	if transport != nil && transport.next != nil {
		transport.next.CloseIdleConnections()
	}
}

func (*sessionTransport) String() string { return "authenticated fixture transport [REDACTED]" }

func (transport *sessionTransport) GoString() string { return transport.String() }
