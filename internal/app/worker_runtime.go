package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/credentials"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	pmruntime "github.com/polymetrics-ai/pm-cli/internal/runtime"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

const (
	workerStorePoolSize               int32 = 16
	workerCredentialWrappingKeyMember       = "wrapping-key"
	workerCredentialWrappingKeyBytes        = 32
)

type WorkerRuntimeConfig struct {
	Configuration        config.Configuration
	SecretsDirectory     string
	Registry             *HandlerRegistry
	ResolveOperation     OperationResolver
	Authorization        auth.ResourceAuthorization
	ScheduledRunAdmitter ScheduledRunAdmitter
	// ClientIdentity is the bounded product identity this worker sends as the
	// default provider user agent for every operation that declares no
	// user-agent field of its own.
	ClientIdentity pmruntime.ClientIdentity
}

// WorkerRuntime owns the prepared dependencies shared by the standalone
// Temporal worker. The command must dial its one SDK client, construct the
// trusted continuation observer from that client, and call NewService before
// polling. SDK workers and clients must be stopped before Close is called.
type WorkerRuntime struct {
	TemporalTLS *tls.Config

	serviceConfiguration WorkerServiceConfig
	service              *WorkerService
	serviceMu            sync.Mutex
	closed               bool
	pool                 *store.Pool
	secrets              *credentials.OperatorSecrets
	artifacts            *ArtifactResources
	continuations        *ContinuationService
	closeOnce            sync.Once
	closeErr             error
}

func OpenWorkerRuntime(ctx context.Context, configuration WorkerRuntimeConfig) (*WorkerRuntime, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, errors.New("worker startup context is unavailable")
	}
	workerConfiguration := configuration.Configuration.Worker()
	if !workerConfiguration.Configured() {
		return nil, errors.New("worker configuration is required")
	}
	if configuration.SecretsDirectory == "" {
		return nil, errors.New("worker secrets directory is required")
	}

	secrets, err := credentials.OpenOperatorSecrets(configuration.SecretsDirectory)
	if err != nil {
		return nil, errors.New("worker secrets are unavailable")
	}
	var pool *store.Pool
	var artifacts *ArtifactResources
	fail := func(cause error) (*WorkerRuntime, error) {
		return nil, errors.Join(cause, closeWorkerRuntimeResources(artifacts, pool, secrets))
	}

	postgres := configuration.Configuration.Postgres()
	postgresEndpoint, err := secrets.Text(postgres.EndpointRef, "pm-app-endpoint")
	if err != nil {
		return fail(errors.New("worker PostgreSQL endpoint is unavailable"))
	}
	temporalConfiguration := configuration.Configuration.Temporal()
	temporalTLS, err := loadWorkerTemporalTLS(secrets, temporalConfiguration)
	if err != nil {
		return fail(err)
	}
	providerHTTPTLS, err := LoadProviderHTTPTLS(secrets, configuration.Configuration.ProviderHTTP())
	if err != nil {
		return fail(err)
	}

	startupContext, cancel := context.WithTimeout(ctx, workerConfiguration.DialTimeout)
	defer cancel()
	pool, err = store.OpenPool(startupContext, store.PoolConfig{
		ConnectionString:  postgresEndpoint,
		MaxConns:          workerStorePoolSize,
		MinConns:          0,
		MaxConnLifetime:   30 * time.Minute,
		MaxConnIdleTime:   5 * time.Minute,
		HealthCheckPeriod: time.Minute,
	})
	if err != nil {
		return fail(errors.New("worker PostgreSQL connection is unavailable"))
	}

	artifactConfiguration := configuration.Configuration.Artifacts()
	artifacts, err = OpenArtifactResources(artifactConfiguration, secrets)
	if err != nil {
		return fail(errors.Join(errors.New("worker artifact resources are unavailable"), err))
	}
	composition, err := OpenConnectorComposition(ConnectorCompositionConfig{
		BundlePaths: configuration.Configuration.Connectors().BundlePaths(),
	})
	if err != nil {
		return fail(fmt.Errorf("declared connector bundles are unavailable: %w", err))
	}
	var artifactPublisher *objects.Publisher
	var continuationService *ContinuationService
	if artifacts != nil {
		artifactPublisher, err = objects.NewPublisher(pool, artifacts.Backend, artifacts.Limits)
		if err != nil {
			return fail(errors.Join(errors.New("worker artifact publisher is unavailable"), err))
		}
		continuationService, err = NewContinuationService(
			pool,
			artifactPublisher,
			artifacts.Backend,
			int64(artifactConfiguration.Execution.MaxObjectBytes),
		)
		if err != nil {
			return fail(errors.Join(errors.New("worker continuation service is unavailable"), err))
		}
	}

	registry := configuration.Registry
	if registry == nil {
		registry = &HandlerRegistry{}
	}
	if err := composition.RegisterDirectRead(registry, HTTPDirectReadDependencies{
		TLSConfig:      providerHTTPTLS,
		Continuations:  continuationService,
		ClientIdentity: configuration.ClientIdentity,
	}); err != nil {
		return fail(errors.Join(errors.New("worker connector direct-read registration failed"), err))
	}
	declaredSQL := configuration.Configuration.ProviderSQL()
	sqlComposition, err := OpenSQLComposition(SQLCompositionConfig{
		Declarations: declaredSQL,
		Bundles:      composition.Registry(),
		Dialer:       nil,
		Resources: func(connection config.ProviderSQLConnection) (SQLConnectionResources, error) {
			return LoadProviderSQLResources(secrets, connection)
		},
	})
	if err != nil {
		return fail(errors.Join(errors.New("worker customer SQL composition is unavailable"), err))
	}
	if err := sqlComposition.Register(registry); err != nil {
		return fail(errors.Join(errors.New("worker customer SQL registration failed"), err))
	}
	resolver := configuration.ResolveOperation
	if resolver == nil {
		resolver = unavailableWorkerOperation
		if composition.Configured() {
			resolver = composition.Resolver()
		}
	}
	authorization := configuration.Authorization
	if authorization == nil {
		authorization = auth.NewResourceAuthorizer()
	}
	wrappingKeyRef := configuration.Configuration.Encryption().WrappingKeyRef
	loadCredentialWrappingKey := func(ctx context.Context, reference string) ([]byte, error) {
		if ctx == nil || ctx.Err() != nil || reference != wrappingKeyRef {
			return nil, errors.New("worker credential wrapping key is unavailable")
		}
		key, readErr := secrets.Read(reference, workerCredentialWrappingKeyMember)
		if readErr != nil {
			return nil, errors.New("worker credential wrapping key is unavailable")
		}
		if len(key) != workerCredentialWrappingKeyBytes {
			clear(key)
			return nil, errors.New("worker credential wrapping key must contain exactly 32 raw bytes")
		}
		return key, nil
	}
	serviceConfiguration := WorkerServiceConfig{
		Pool:                      pool,
		DeploymentID:              configuration.Configuration.DeploymentID(),
		Registry:                  registry,
		ResolveOperation:          resolver,
		Authorization:             authorization,
		ActivityTimeouts:          workerConfiguration.ActivityTimeouts,
		ScheduledRunAdmitter:      configuration.ScheduledRunAdmitter,
		LoadCredentialWrappingKey: loadCredentialWrappingKey,
		ArtifactPublisher:         artifactPublisher,
		MaximumResultBytes:        int64(artifactConfiguration.Execution.MaxObjectBytes),
		SQLBounds:                 sqlComposition.Bounds,
	}

	return &WorkerRuntime{
		TemporalTLS:          temporalTLS,
		serviceConfiguration: serviceConfiguration,
		pool:                 pool,
		secrets:              secrets,
		artifacts:            artifacts,
		continuations:        continuationService,
	}, nil
}

func (runtime *WorkerRuntime) NewService(
	observer contract.WorkflowContinuationObserver,
) (*WorkerService, error) {
	if runtime == nil || observer == nil {
		return nil, errors.New("trusted workflow continuation observer is required")
	}
	runtime.serviceMu.Lock()
	defer runtime.serviceMu.Unlock()
	if runtime.closed {
		return nil, errors.New("worker runtime is closed")
	}
	if runtime.service != nil {
		return nil, errors.New("worker application service is already constructed")
	}
	configuration := runtime.serviceConfiguration
	configuration.ObserveContinuation = observer
	service, err := NewWorkerService(configuration)
	if err != nil {
		return nil, errors.New("worker application service is unavailable")
	}
	runtime.service = service
	return service, nil
}

// ContinuationService returns the concrete server-owned codec for handler
// registration before NewService starts polling. No codec exists when artifact
// execution is intentionally unconfigured.
func (runtime *WorkerRuntime) ContinuationService() (*ContinuationService, error) {
	if runtime == nil {
		return nil, errors.New("worker runtime is unavailable")
	}
	runtime.serviceMu.Lock()
	defer runtime.serviceMu.Unlock()
	if runtime.closed {
		return nil, errors.New("worker runtime is closed")
	}
	if runtime.continuations == nil {
		return nil, errors.New("worker continuation service is unconfigured")
	}
	return runtime.continuations, nil
}

func unavailableWorkerOperation(
	context.Context,
	contract.Scope,
	string,
	string,
) (OperationSelection, error) {
	return OperationSelection{}, ErrWorkerCapabilityUnavailable
}

func loadWorkerTemporalTLS(
	secrets *credentials.OperatorSecrets,
	configuration config.TemporalConfiguration,
) (*tls.Config, error) {
	host, _, err := net.SplitHostPort(configuration.Endpoint)
	if err != nil || host == "" {
		return nil, errors.New("configured Temporal endpoint is invalid")
	}
	ca, err := secrets.Read(configuration.ClientIdentityRef, "ca.pem")
	if err != nil {
		return nil, errors.New("worker Temporal CA is unavailable")
	}
	defer clear(ca)
	certificatePEM, err := secrets.Read(configuration.ClientIdentityRef, "client.pem")
	if err != nil {
		return nil, errors.New("worker Temporal client certificate is unavailable")
	}
	defer clear(certificatePEM)
	privateKeyPEM, err := secrets.Read(configuration.ClientIdentityRef, "client-key.pem")
	if err != nil {
		return nil, errors.New("worker Temporal client key is unavailable")
	}
	defer clear(privateKeyPEM)

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("worker Temporal CA is invalid")
	}
	certificate, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
	if err != nil {
		return nil, errors.New("worker Temporal client identity is invalid")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      roots,
		ServerName:   host,
		Certificates: []tls.Certificate{certificate},
	}, nil
}

func (runtime *WorkerRuntime) Close() error {
	if runtime == nil {
		return nil
	}
	runtime.closeOnce.Do(func() {
		runtime.serviceMu.Lock()
		runtime.closed = true
		runtime.serviceMu.Unlock()
		runtime.closeErr = closeWorkerRuntimeResources(runtime.artifacts, runtime.pool, runtime.secrets)
	})
	return runtime.closeErr
}

func closeWorkerRuntimeResources(
	artifacts *ArtifactResources,
	pool *store.Pool,
	secrets *credentials.OperatorSecrets,
) error {
	artifactErr := artifacts.Close()
	if pool != nil {
		pool.Close()
	}
	if secrets != nil {
		if err := secrets.Close(); err != nil {
			return errors.Join(artifactErr, errors.New("worker operator secret bundle cleanup failed"))
		}
	}
	return artifactErr
}
