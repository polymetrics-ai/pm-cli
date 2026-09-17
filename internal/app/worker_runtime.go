package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/credentials"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

const workerStorePoolSize int32 = 16

type WorkerRuntimeConfig struct {
	Configuration        config.Configuration
	SecretsDirectory     string
	Registry             *HandlerRegistry
	ResolveOperation     OperationResolver
	Authorization        auth.ResourceAuthorization
	ScheduledRunAdmitter ScheduledRunAdmitter
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
	closeSecrets := true
	defer func() {
		if closeSecrets {
			_ = secrets.Close()
		}
	}()

	postgres := configuration.Configuration.Postgres()
	postgresEndpoint, err := secrets.Text(postgres.EndpointRef, "pm-app-endpoint")
	if err != nil {
		return nil, errors.New("worker PostgreSQL endpoint is unavailable")
	}
	temporalConfiguration := configuration.Configuration.Temporal()
	temporalTLS, err := loadWorkerTemporalTLS(secrets, temporalConfiguration)
	if err != nil {
		return nil, err
	}

	startupContext, cancel := context.WithTimeout(ctx, workerConfiguration.DialTimeout)
	defer cancel()
	pool, err := store.OpenPool(startupContext, store.PoolConfig{
		ConnectionString:  postgresEndpoint,
		MaxConns:          workerStorePoolSize,
		MinConns:          0,
		MaxConnLifetime:   30 * time.Minute,
		MaxConnIdleTime:   5 * time.Minute,
		HealthCheckPeriod: time.Minute,
	})
	if err != nil {
		return nil, errors.New("worker PostgreSQL connection is unavailable")
	}
	closePool := true
	defer func() {
		if closePool {
			pool.Close()
		}
	}()

	registry := configuration.Registry
	if registry == nil {
		registry = &HandlerRegistry{}
	}
	resolver := configuration.ResolveOperation
	if resolver == nil {
		resolver = unavailableWorkerOperation
	}
	authorization := configuration.Authorization
	if authorization == nil {
		authorization = auth.NewResourceAuthorizer()
	}
	serviceConfiguration := WorkerServiceConfig{
		Pool:                 pool,
		DeploymentID:         configuration.Configuration.DeploymentID(),
		Registry:             registry,
		ResolveOperation:     resolver,
		Authorization:        authorization,
		ActivityTimeouts:     workerConfiguration.ActivityTimeouts,
		ScheduledRunAdmitter: configuration.ScheduledRunAdmitter,
	}

	closePool = false
	closeSecrets = false
	return &WorkerRuntime{
		TemporalTLS:          temporalTLS,
		serviceConfiguration: serviceConfiguration,
		pool:                 pool,
		secrets:              secrets,
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
		if runtime.pool != nil {
			runtime.pool.Close()
		}
		if runtime.secrets != nil {
			runtime.closeErr = runtime.secrets.Close()
		}
	})
	return runtime.closeErr
}
