package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	pmruntime "github.com/polymetrics-ai/pm-cli/internal/runtime"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// ArtifactExecution carries the artifact byte resources this process already
// opened for its own artifact surface and shares with the HTTP direct-read
// registration. The opening process keeps ownership: this carrier never opens,
// mutates or closes them, and it is empty when byte execution is
// intentionally unconfigured.
type ArtifactExecution struct {
	Backend            *objects.S3
	Limits             store.ObjectCapacityLimits
	MaximumObjectBytes int64
}

// Configured reports whether this process has artifact byte execution.
func (execution ArtifactExecution) Configured() bool {
	return execution.Backend != nil && execution.MaximumObjectBytes > 0
}

// ServicesConfig carries resolved startup controls and operator-loaded secret
// values. It is never a public request or an independently resolved config file.
type ServicesConfig struct {
	DeploymentID      string
	PublicOrigin      string
	PostgresEndpoint  string
	StoreProbeTimeout time.Duration
	OIDCIssuer        string
	OIDCClientID      string
	OIDCClientSecret  string
	OIDCHTTPClient    *http.Client
	CredentialIngress CredentialIngressConfig
	RunPolicy         store.RunBudgetPolicy
	ResolveHTTPPolicy HTTPPolicyResolver
	ProviderHTTPTLS   *tls.Config
	ConnectorBundles  []string
	Artifacts         ArtifactExecution
	// SQLDeclarations is the declared customer SQL declaration this process
	// registers next to the HTTP one. An absent declaration registers nothing,
	// so admission stays truthfully unavailable for the PostgreSQL transport
	// instead of advertising a callback that is not registered.
	SQLDeclarations config.ProviderSQLConfiguration
	// SQLResources opens the process-static resources of one declared customer
	// connection. It is required exactly when a SQL connection is declared.
	SQLResources func(config.ProviderSQLConnection) (SQLConnectionResources, error)
	// ClientIdentity is the bounded product identity this process sends as the
	// default provider user agent for every operation that declares no
	// user-agent field of its own.
	ClientIdentity pmruntime.ClientIdentity
}

func (ServicesConfig) String() string                 { return "application services configuration [REDACTED]" }
func (configuration ServicesConfig) GoString() string { return configuration.String() }

// Services owns the application store and its identity/credential use cases.
// Its composed dependencies are immutable after successful construction.
type Services struct {
	Authentication *auth.Service
	Identity       *IdentityService
	Credentials    *CredentialIngress
	Catalog        *CatalogService
	Admission      *AdmissionService
	Runs           *RunService
	pool           *store.Pool
	publicOrigin   string
}

// OpenServices observes an existing compatible store; it never provisions or
// migrates infrastructure. Failed construction closes every acquired pool.
func OpenServices(ctx context.Context, configuration ServicesConfig) (*Services, error) {
	if ctx == nil || !validApplicationID(configuration.DeploymentID) ||
		configuration.PostgresEndpoint == "" || configuration.StoreProbeTimeout <= 0 ||
		configuration.OIDCHTTPClient == nil {
		return nil, errors.New("resolved application service dependencies are required")
	}
	origin, err := url.Parse(configuration.PublicOrigin)
	if err != nil || origin.Scheme != "https" || origin.Hostname() == "" || origin.User != nil ||
		origin.RawQuery != "" || origin.Fragment != "" || origin.Opaque != "" ||
		(origin.EscapedPath() != "" && origin.EscapedPath() != "/") {
		return nil, errors.New("configured public origin is invalid")
	}
	publicOrigin := (&url.URL{Scheme: origin.Scheme, Host: origin.Host}).String()
	composition, err := OpenConnectorComposition(ConnectorCompositionConfig{
		BundlePaths: configuration.ConnectorBundles,
	})
	if err != nil {
		return nil, fmt.Errorf("declared connector bundles are unavailable: %w", err)
	}
	startupContext, cancel := context.WithTimeout(ctx, configuration.StoreProbeTimeout)
	pool, err := store.OpenPool(startupContext, store.PoolConfig{
		ConnectionString:  configuration.PostgresEndpoint,
		MaxConns:          16,
		MinConns:          0,
		MaxConnLifetime:   30 * time.Minute,
		MaxConnIdleTime:   5 * time.Minute,
		HealthCheckPeriod: time.Minute,
	})
	cancel()
	if err != nil {
		return nil, errors.New("compatible non-owner application store is unavailable")
	}
	constructed := false
	defer func() {
		if !constructed {
			pool.Close()
		}
	}()
	oidcConfiguration, err := auth.DiscoverOIDC(ctx, auth.DiscoveryConfig{
		Issuer:       configuration.OIDCIssuer,
		ClientID:     configuration.OIDCClientID,
		ClientSecret: configuration.OIDCClientSecret,
		RedirectURL:  publicOrigin + "/v1/sessions/browser/callback",
		LoginTTL:     10 * time.Minute,
	}, configuration.OIDCHTTPClient)
	if err != nil {
		return nil, errors.New("OIDC provider discovery is unavailable or invalid")
	}
	authentication, err := auth.NewService(auth.Config{
		Pool:         pool,
		DeploymentID: configuration.DeploymentID,
		Device: auth.DevicePolicy{
			ChallengeTTL:     10 * time.Minute,
			PollInterval:     5 * time.Second,
			StartWindow:      time.Minute,
			MaximumStarts:    10,
			MaximumBodyBytes: 64 << 10,
		},
		Sessions: auth.SessionPolicy{
			BrowserTTL: 12 * time.Hour,
			DeviceTTL:  24 * time.Hour,
		},
		Inbound: auth.InboundPolicy{
			AllowedHosts:   []string{origin.Host},
			AllowedOrigins: []string{publicOrigin},
		},
		OIDC: oidcConfiguration,
	})
	if err != nil {
		return nil, errors.New("authentication service composition failed")
	}
	identity, err := NewIdentityService(IdentityServiceConfig{
		Pool:             pool,
		DeploymentID:     configuration.DeploymentID,
		Authorize:        authentication.AuthorizeResource,
		ReceiptRetention: 7 * 24 * time.Hour,
	})
	if err != nil {
		return nil, errors.New("identity application composition failed")
	}
	ingress, err := NewCredentialIngress(identity, configuration.CredentialIngress)
	if err != nil {
		return nil, errors.New("credential application composition failed")
	}
	continuations, err := openHTTPContinuations(pool, configuration.Artifacts)
	if err != nil {
		return nil, err
	}
	handlers := &HandlerRegistry{}
	if err := composition.RegisterDirectRead(handlers, HTTPDirectReadDependencies{
		TLSConfig:      configuration.ProviderHTTPTLS,
		Continuations:  continuations,
		ClientIdentity: configuration.ClientIdentity,
	}); err != nil {
		return nil, errors.Join(errors.New("connector direct-read registration failed"), err)
	}
	sqlComposition, err := OpenSQLComposition(SQLCompositionConfig{
		Declarations: configuration.SQLDeclarations,
		Bundles:      composition.Registry(),
		Resources:    configuration.SQLResources,
	})
	if err != nil {
		return nil, errors.Join(errors.New("customer SQL composition is unavailable"), err)
	}
	if err := sqlComposition.Register(handlers); err != nil {
		return nil, errors.Join(errors.New("customer SQL registration failed"), err)
	}
	catalog, err := ServeCatalog(CatalogServiceConfig{
		Pool:      pool,
		Authorize: authentication.AuthorizeResource,
		Registry:  composition.Registry(),
		Handlers:  handlers,
	})
	if err != nil {
		return nil, errors.New("catalog application composition failed")
	}
	admission, err := NewAdmissionService(AdmissionServiceConfig{
		Pool:              pool,
		DeploymentID:      configuration.DeploymentID,
		Authorize:         authentication.AuthorizeResource,
		Registry:          handlers,
		ResolveOperation:  composition.Resolver(),
		Policy:            configuration.RunPolicy,
		ResolveHTTPPolicy: configuration.ResolveHTTPPolicy,
	})
	if err != nil {
		return nil, errors.New("durable admission composition failed")
	}
	runs, err := NewRunService(RunServiceConfig{
		Pool:             pool,
		DeploymentID:     configuration.DeploymentID,
		Authorize:        authentication.AuthorizeResource,
		ReceiptRetention: 7 * 24 * time.Hour,
	})
	if err != nil {
		return nil, errors.New("run control composition failed")
	}
	constructed = true
	return &Services{
		Authentication: authentication,
		Identity:       identity,
		Credentials:    ingress,
		Catalog:        catalog,
		Admission:      admission,
		Runs:           runs,
		pool:           pool,
		publicOrigin:   publicOrigin,
	}, nil
}

func (services *Services) PublicOrigin() string           { return services.publicOrigin }
func (services *Services) Ping(ctx context.Context) error { return services.pool.Ping(ctx) }

// Store returns the existing runtime pool. Services retains ownership and is
// solely responsible for closing it.
func (services *Services) Store() *store.Pool { return services.pool }

func (services *Services) Close() { services.pool.Close() }

// openHTTPContinuations composes the artifact publisher and the continuation
// service the shared HTTP direct-read registration depends on, using the
// artifact byte resources this process already opened. It returns nil without
// object I/O when byte execution is intentionally unconfigured, so an
// undeclared deployment stays truthfully unavailable.
func openHTTPContinuations(pool *store.Pool, execution ArtifactExecution) (*ContinuationService, error) {
	if !execution.Configured() {
		return nil, nil
	}
	publisher, err := objects.NewPublisher(pool, execution.Backend, execution.Limits)
	if err != nil {
		return nil, errors.Join(errors.New("HTTP continuation artifact publisher is unavailable"), err)
	}
	continuations, err := NewContinuationService(
		pool,
		publisher,
		execution.Backend,
		execution.MaximumObjectBytes,
	)
	if err != nil {
		return nil, errors.Join(errors.New("HTTP continuation service is unavailable"), err)
	}
	return continuations, nil
}
