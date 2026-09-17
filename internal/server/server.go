package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	sdkclient "go.temporal.io/sdk/client"

	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/credentials"
	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	pmtemporal "github.com/polymetrics-ai/pm-cli/internal/temporal"
)

const (
	oidcRequestTimeout        = 15 * time.Second
	oidcResponseMaximumBytes  = 1 << 20
	wrappingKeyBytes          = 32
	temporalDialTimeout       = 5 * time.Second
	temporalRequestTimeout    = 20 * time.Second
	temporalRouteClaimTTL     = time.Minute
	temporalRelayIdleDelay    = time.Second
	temporalRelayFailureDelay = 2 * time.Second
)

const (
	browserCallbackPath    = "/v1/sessions/browser/callback"
	oidcClientSecretMember = "oidc-client-secret"
	wrappingKeyMember      = "wrapping-key"
)

// Application owns the composed HTTP process and every resource opened for it.
// It observes existing dependencies only; it never provisions or migrates them.
type Application struct {
	handler        http.Handler
	lifecycle      *Server
	services       *app.Services
	secrets        *credentials.OperatorSecrets
	oidcTransport  *boundedOIDCTransport
	temporalClient *pmtemporal.Client
	relayCancel    context.CancelFunc
	relayDone      <-chan error

	closeOnce sync.Once
	closeErr  error
}

// Open composes the authenticated API around an already validated immutable
// configuration. Startup verifies the existing control store and OIDC discovery
// document. It composes durable admission and run controls; registered execution
// handlers and worker dispatch remain separate capabilities.
func Open(
	ctx context.Context,
	configuration config.Configuration,
	secretsDirectory string,
	diagnostics io.Writer,
) (*Application, error) {
	return openWithOIDCFactory(ctx, configuration, secretsDirectory, diagnostics, newOIDCHTTPClient)
}

func openWithOIDCFactory(
	ctx context.Context,
	configuration config.Configuration,
	secretsDirectory string,
	diagnostics io.Writer,
	oidcFactory func() (*http.Client, *boundedOIDCTransport),
) (*Application, error) {
	if ctx == nil || configuration.SchemaVersion() != "1" || diagnostics == nil {
		return nil, errors.New("validated server configuration, context, and diagnostics are required")
	}
	secrets, err := credentials.OpenOperatorSecrets(secretsDirectory)
	if err != nil {
		return nil, err
	}
	fail := func(cause error, services *app.Services, transport *boundedOIDCTransport) (*Application, error) {
		return nil, errors.Join(cause, closeApplicationResources(services, secrets, transport))
	}
	endpoint, err := secrets.Text(configuration.Postgres().EndpointRef, "pm-app-endpoint")
	if err != nil {
		return fail(errors.New("server control store is unavailable"), nil, nil)
	}
	identitySettings := configuration.Identity()
	serverSettings := configuration.Server()
	wrappingKeyRef := configuration.Encryption().WrappingKeyRef
	clientSecret, err := secrets.Text(identitySettings.ClientSecretRef, oidcClientSecretMember)
	if err != nil {
		return fail(errors.New("OIDC client secret is unavailable"), nil, nil)
	}
	runPolicy, err := app.RunPolicyFromConfiguration(configuration)
	if err != nil {
		return fail(errors.New("resolved run admission policy is invalid"), nil, nil)
	}
	oidcClient, oidcTransport := oidcFactory()
	services, err := app.OpenServices(ctx, app.ServicesConfig{
		DeploymentID:      configuration.DeploymentID(),
		PublicOrigin:      configuration.PublicOrigin(),
		PostgresEndpoint:  endpoint,
		StoreProbeTimeout: serverSettings.ProbeTimeout,
		OIDCIssuer:        identitySettings.Issuer,
		OIDCClientID:      identitySettings.ClientID,
		OIDCClientSecret:  clientSecret,
		OIDCHTTPClient:    oidcClient,
		RunPolicy:         runPolicy,
		CredentialIngress: app.CredentialIngressConfig{
			StepUpMaximumAge: identitySettings.StepUpMaximumAge,
			WrappingKeyRef:   wrappingKeyRef,
			LoadWrappingKey: func(ctx context.Context, reference string) ([]byte, error) {
				if ctx == nil || ctx.Err() != nil || reference != wrappingKeyRef {
					return nil, errors.New("wrapping key is unavailable")
				}
				key, readErr := secrets.Read(reference, wrappingKeyMember)
				if readErr != nil {
					return nil, errors.New("wrapping key is unavailable")
				}
				if len(key) != wrappingKeyBytes {
					clear(key)
					return nil, errors.New("wrapping key must contain exactly 32 raw bytes")
				}
				return key, nil
			},
		},
	})
	if err != nil {
		return fail(err, nil, oidcTransport)
	}
	service, err := httpapi.NewService(httpapi.ServiceConfig{
		PublicOrigin:   services.PublicOrigin(),
		Auth:           services.Authentication,
		Identity:       services.Identity,
		Credentials:    services.Credentials,
		Admission:      services.Admission,
		Runs:           services.Runs,
		InstanceID:     configuration.DeploymentID(),
		OIDCHTTPClient: oidcClient,
	})
	if err != nil {
		return fail(errors.New("HTTP application composition failed"), services, oidcTransport)
	}
	lifecycle, err := New(configuration, services.Ping, nil, diagnostics)
	if err != nil {
		return fail(errors.New("server lifecycle composition failed"), services, oidcTransport)
	}
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	transportPolicy := httpapi.TransportPolicy{
		PublicOrigin:     services.PublicOrigin(),
		TrustedProxies:   serverSettings.TrustedProxies,
		RequestTimeout:   serverSettings.RequestTimeout,
		MaximumBodyBytes: int64(configuration.Limits().RequestBytes),
	}
	if err := httpapi.RegisterRoutes(router, transportPolicy, service, diagnostics, service.Middleware()); err != nil {
		_ = lifecycle.Drain(context.Background())
		return fail(errors.New("server route composition failed"), services, oidcTransport)
	}
	registerHealthRoutes(router, lifecycle)
	if err := registerBrowserRoutes(router); err != nil {
		_ = lifecycle.Drain(context.Background())
		return fail(errors.New("local browser asset composition failed"), services, oidcTransport)
	}
	temporalClient, relay, err := composeTemporalRelay(configuration, secrets, services)
	if err != nil {
		_ = lifecycle.Drain(context.Background())
		return fail(errors.New("Temporal relay composition failed"), services, oidcTransport)
	}
	relayContext, relayCancel := context.WithCancel(ctx)
	relayDone := make(chan error, 1)
	go func() {
		relayDone <- relay.Run(relayContext)
	}()
	return &Application{
		handler:        router,
		lifecycle:      lifecycle,
		services:       services,
		secrets:        secrets,
		oidcTransport:  oidcTransport,
		temporalClient: temporalClient,
		relayCancel:    relayCancel,
		relayDone:      relayDone,
	}, nil
}

// Handler returns the complete generated API, private ingress, health, and
// minimal local browser surface behind the same request boundary.
func (application *Application) Handler() http.Handler {
	if application == nil {
		return nil
	}
	return application.handler
}

// Serve transfers listener ownership to the bounded lifecycle server.
func (application *Application) Serve(ctx context.Context, listener net.Listener) error {
	if application == nil || application.lifecycle == nil || application.handler == nil {
		return errors.New("open server application is required")
	}
	return application.lifecycle.Serve(ctx, listener, application.handler)
}

// Close drains requests, stops the bounded relay, closes its Temporal client,
// then closes the control pool, OIDC transport, and descriptor-bound secret
// root. It is safe to call more than once and never cancels a durable workflow.
func (application *Application) Close() error {
	if application == nil {
		return nil
	}
	application.closeOnce.Do(func() {
		var drainErr error
		if application.lifecycle != nil {
			drainErr = application.lifecycle.Drain(context.Background())
		}
		if application.relayCancel != nil {
			application.relayCancel()
		}
		var relayErr error
		if application.relayDone != nil {
			relayErr = <-application.relayDone
		}
		if application.temporalClient != nil {
			application.temporalClient.Close()
		}
		application.closeErr = errors.Join(
			drainErr,
			relayErr,
			closeApplicationResources(application.services, application.secrets, application.oidcTransport),
		)
	})
	return application.closeErr
}

func closeApplicationResources(services *app.Services, secrets *credentials.OperatorSecrets, transport *boundedOIDCTransport) error {
	if transport != nil {
		transport.Close()
	}
	if services != nil {
		services.Close()
	}
	if secrets != nil {
		if err := secrets.Close(); err != nil {
			return errors.New("operator secret bundle cleanup failed")
		}
	}
	return nil
}

func composeTemporalRelay(
	configuration config.Configuration,
	secrets *credentials.OperatorSecrets,
	services *app.Services,
) (*pmtemporal.Client, *pmtemporal.Relay, error) {
	if secrets == nil || services == nil || services.Store() == nil {
		return nil, nil, errors.New("Temporal relay dependencies are required")
	}
	temporalConfiguration := configuration.Temporal()
	controlTaskQueue, err := configuredControlTaskQueue(temporalConfiguration.TaskQueues)
	if err != nil {
		return nil, nil, err
	}
	randomSuffix := make([]byte, 16)
	if _, err := rand.Read(randomSuffix); err != nil {
		return nil, nil, errors.New("Temporal relay identity is unavailable")
	}
	ownerID := configuration.DeploymentID() + "/control-relay/" + hex.EncodeToString(randomSuffix)
	temporalClient, err := pmtemporal.NewClient(pmtemporal.ClientConfig{
		Namespace:            temporalConfiguration.Namespace,
		ControlTaskQueue:     controlTaskQueue,
		WorkflowType:         contract.RunWorkflowType,
		ProjectionUpdateName: contract.ProjectionUpdateName,
		DialTimeout:          temporalDialTimeout,
		RequestTimeout:       temporalRequestTimeout,
		Dial: lazyTemporalDial(
			secrets,
			temporalConfiguration.Endpoint,
			temporalConfiguration.Namespace,
			temporalConfiguration.ClientIdentityRef,
			ownerID,
		),
	})
	if err != nil {
		return nil, nil, err
	}
	relay, err := pmtemporal.NewRelay(pmtemporal.RelayConfig{
		Store:        services.Store(),
		Client:       temporalClient,
		OwnerID:      ownerID,
		ClaimLimit:   8,
		ClaimTTL:     temporalRouteClaimTTL,
		IdleDelay:    temporalRelayIdleDelay,
		FailureDelay: temporalRelayFailureDelay,
	})
	if err != nil {
		temporalClient.Close()
		return nil, nil, err
	}
	return temporalClient, relay, nil
}

func configuredControlTaskQueue(taskQueues []string) (string, error) {
	for _, taskQueue := range taskQueues {
		if taskQueue == contract.ControlTaskQueue {
			return taskQueue, nil
		}
	}
	return "", errors.New("configured Temporal task queues omit pm-control")
}

func lazyTemporalDial(
	secrets *credentials.OperatorSecrets,
	endpoint string,
	namespace string,
	identityReference string,
	identity string,
) pmtemporal.DialFunc {
	return func(ctx context.Context) (pmtemporal.WorkflowClient, error) {
		if ctx == nil || ctx.Err() != nil {
			return nil, errors.New("Temporal dial context is unavailable")
		}
		host, _, err := net.SplitHostPort(endpoint)
		if err != nil {
			return nil, errors.New("configured Temporal endpoint is invalid")
		}
		ca, err := secrets.Read(identityReference, "ca.pem")
		if err != nil {
			return nil, errors.New("Temporal CA is unavailable")
		}
		defer clear(ca)
		certificatePEM, err := secrets.Read(identityReference, "client.pem")
		if err != nil {
			return nil, errors.New("Temporal client certificate is unavailable")
		}
		defer clear(certificatePEM)
		privateKeyPEM, err := secrets.Read(identityReference, "client-key.pem")
		if err != nil {
			return nil, errors.New("Temporal client key is unavailable")
		}
		defer clear(privateKeyPEM)
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(ca) {
			return nil, errors.New("Temporal CA is invalid")
		}
		certificate, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
		if err != nil {
			return nil, errors.New("Temporal client identity is invalid")
		}
		return sdkclient.DialContext(ctx, sdkclient.Options{
			HostPort:    endpoint,
			Namespace:   namespace,
			Identity:    identity,
			Credentials: sdkclient.NewMTLSCredentials(certificate),
			ConnectionOptions: sdkclient.ConnectionOptions{TLS: &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    roots,
				ServerName: host,
			}},
		})
	}
}

func newOIDCHTTPClient() (*http.Client, *boundedOIDCTransport) {
	base := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           16,
		MaxIdleConnsPerHost:    4,
		MaxConnsPerHost:        8,
		IdleConnTimeout:        30 * time.Second,
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  10 * time.Second,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}
	lifetime, cancel := context.WithCancel(context.Background())
	transport := &boundedOIDCTransport{
		next:     base,
		lifetime: lifetime,
		cancel:   cancel,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   oidcRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return client, transport
}

var errOIDCResponseTooLarge = errors.New("OIDC response exceeds the configured bound")

type boundedOIDCTransport struct {
	next      *http.Transport
	lifetime  context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
}

func (transport *boundedOIDCTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport == nil || transport.next == nil || transport.lifetime == nil ||
		request == nil || request.URL == nil || request.URL.Scheme != "https" ||
		request.URL.Host == "" || request.URL.User != nil {
		return nil, errors.New("OIDC request target is invalid")
	}
	if transport.lifetime.Err() != nil {
		return nil, errors.New("OIDC HTTP client is closed")
	}
	requestContext, cancel := context.WithCancel(request.Context())
	stopLifetimeCancellation := context.AfterFunc(transport.lifetime, cancel)
	finish := func() {
		stopLifetimeCancellation()
		cancel()
	}
	response, err := transport.next.RoundTrip(request.Clone(requestContext))
	if err != nil {
		finish()
		return nil, err
	}
	if response.Body == nil {
		finish()
		return response, nil
	}
	if response.ContentLength > oidcResponseMaximumBytes {
		_ = response.Body.Close()
		finish()
		return nil, errOIDCResponseTooLarge
	}
	response.Body = &boundedOIDCBody{
		body:      response.Body,
		remaining: oidcResponseMaximumBytes,
		finish:    finish,
	}
	return response, nil
}

func (transport *boundedOIDCTransport) Close() {
	if transport == nil {
		return
	}
	transport.closeOnce.Do(func() {
		if transport.cancel != nil {
			transport.cancel()
		}
		if transport.next != nil {
			transport.next.CloseIdleConnections()
		}
	})
}

type boundedOIDCBody struct {
	body       io.ReadCloser
	remaining  int64
	exceeded   bool
	finish     func()
	finishOnce sync.Once
}

func (body *boundedOIDCBody) Read(destination []byte) (int, error) {
	if body.exceeded {
		return 0, errOIDCResponseTooLarge
	}
	if len(destination) == 0 {
		read, err := body.body.Read(destination)
		if err != nil {
			body.finishRequest()
		}
		return read, err
	}
	maximum := int64(len(destination))
	if maximum > body.remaining+1 {
		maximum = body.remaining + 1
	}
	read, err := body.body.Read(destination[:int(maximum)])
	if int64(read) > body.remaining {
		accepted := int(body.remaining)
		body.remaining = 0
		body.exceeded = true
		_ = body.body.Close()
		body.finishRequest()
		return accepted, errOIDCResponseTooLarge
	}
	body.remaining -= int64(read)
	if err != nil {
		body.finishRequest()
	}
	return read, err
}

func (body *boundedOIDCBody) Close() error {
	err := body.body.Close()
	body.finishRequest()
	return err
}

func (body *boundedOIDCBody) finishRequest() {
	body.finishOnce.Do(func() {
		if body.finish != nil {
			body.finish()
		}
	})
}

func registerHealthRoutes(router *gin.Engine, lifecycle *Server) {
	router.GET("/health/live", func(context *gin.Context) {
		context.JSON(http.StatusOK, gin.H{"status": "live"})
	})
	router.GET("/health/ready", func(context *gin.Context) {
		readiness := lifecycle.Readiness(context.Request.Context())
		status := http.StatusServiceUnavailable
		if readiness.Admission && readiness.Dispatch && !readiness.Draining {
			status = http.StatusOK
		}
		context.JSON(status, readiness)
	})
}
