package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/server"
)

// These cases exercise real sockets with controlled dependency observations.
// TestAuthenticatedApplicationDrainPreservesAcceptedRun covers the independent
// PostgreSQL record across closure of the fully composed API application.
func TestBoundedLifecycleDrain(t *testing.T) {
	t.Run("E02-C01 healthy requests finish and subscribers release", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		fixture := startLifecycle(t, nil, func(service *server.Server, router *gin.Engine) {
			router.GET("/ordinary", func(c *gin.Context) {
				close(entered)
				select {
				case <-release:
					c.String(http.StatusOK, "finished")
				case <-c.Request.Context().Done():
				}
			})
			router.GET("/stream", func(c *gin.Context) {
				c.Header("Content-Type", "text/event-stream")
				_, _ = io.WriteString(c.Writer, "data: ready\n\n")
				c.Writer.Flush()
				select {
				case <-service.Draining():
				case <-c.Request.Context().Done():
				}
			})
		})
		ordinary := make(chan error, 1)
		go func() {
			response, err := fixture.client.Get(fixture.url + "/ordinary")
			if err == nil {
				var body []byte
				body, err = io.ReadAll(response.Body)
				_ = response.Body.Close()
				if err == nil && (response.StatusCode != http.StatusOK || string(body) != "finished") {
					err = errors.New("ordinary request did not complete")
				}
			}
			ordinary <- err
		}()
		awaitLifecycleSignal(t, entered)
		stream, err := fixture.client.Get(fixture.url + "/stream")
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Body.Close()
		drain := make(chan error, 1)
		go func() { drain <- fixture.service.Drain(context.Background()) }()
		awaitLifecycleSignal(t, fixture.service.Draining())
		if state := fixture.service.Readiness(context.Background()); state.Admission || state.Dispatch || !state.Draining {
			t.Fatalf("draining server remained ready: %+v", state)
		}
		close(release)
		if err := <-ordinary; err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(stream.Body); err != nil {
			t.Fatalf("subscriber did not finish cleanly: %v", err)
		}
		if err := <-drain; err != nil {
			t.Fatal(err)
		}
		fixture.requireStopped(t)
	})

	t.Run("E02-C02 blocked request receives cancellation at the two-second fixture bound", func(t *testing.T) {
		entered := make(chan struct{})
		cancelled := make(chan struct{})
		fixture := startLifecycle(t, nil, func(_ *server.Server, router *gin.Engine) {
			router.GET("/blocked", func(c *gin.Context) {
				close(entered)
				<-c.Request.Context().Done()
				close(cancelled)
			})
		})
		requestDone := make(chan struct{})
		go func() {
			defer close(requestDone)
			response, err := fixture.client.Get(fixture.url + "/blocked")
			if err == nil {
				_ = response.Body.Close()
			}
		}()
		awaitLifecycleSignal(t, entered)
		start := time.Now()
		err := fixture.service.Drain(context.Background())
		elapsed := time.Since(start)
		if !errors.Is(err, server.ErrDrainDeadline) || elapsed < 2*time.Second || elapsed > 4*time.Second {
			t.Fatalf("bounded forced drain: duration=%s error=%v", elapsed, err)
		}
		awaitLifecycleSignal(t, cancelled)
		awaitLifecycleSignal(t, requestDone)
		fixture.requireStopped(t)
	})

	t.Run("E02-C03 client cancellation closes only that request", func(t *testing.T) {
		entered := make(chan struct{})
		cancelled := make(chan struct{})
		fixture := startLifecycle(t, nil, func(_ *server.Server, router *gin.Engine) {
			router.GET("/owned-request", func(c *gin.Context) {
				close(entered)
				<-c.Request.Context().Done()
				close(cancelled)
			})
			router.GET("/still-serving", func(c *gin.Context) { c.Status(http.StatusNoContent) })
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, fixture.url+"/owned-request", nil)
		if err != nil {
			t.Fatal(err)
		}
		requestDone := make(chan struct{})
		go func() {
			defer close(requestDone)
			response, err := fixture.client.Do(request)
			if err == nil {
				_ = response.Body.Close()
			}
		}()
		awaitLifecycleSignal(t, entered)
		cancel()
		awaitLifecycleSignal(t, cancelled)
		awaitLifecycleSignal(t, requestDone)
		response, err := fixture.client.Get(fixture.url + "/still-serving")
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("independent request status=%d", response.StatusCode)
		}
		select {
		case <-fixture.service.Draining():
			t.Fatal("client cancellation initiated server drain")
		default:
		}
	})

	t.Run("E01-C01 and E02-C04 dispatch outage does not imply admission outage", func(t *testing.T) {
		var dispatchAvailable atomic.Bool
		dispatchAvailable.Store(true)
		fixture := startLifecycle(t, func(context.Context) error {
			if !dispatchAvailable.Load() {
				return errors.New("controlled dispatch outage")
			}
			return nil
		}, func(service *server.Server, router *gin.Engine) {
			router.GET("/readiness", func(c *gin.Context) { c.JSON(http.StatusOK, service.Readiness(c.Request.Context())) })
		})
		readiness := func() server.Readiness {
			response, err := fixture.client.Get(fixture.url + "/readiness")
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var result server.Readiness
			if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
			return result
		}
		if state := readiness(); !state.Admission || !state.Dispatch || state.Draining {
			t.Fatalf("healthy readiness=%+v", state)
		}
		dispatchAvailable.Store(false)
		if state := readiness(); !state.Admission || state.Dispatch || state.Draining {
			t.Fatalf("degraded readiness=%+v", state)
		}
	})
}

type lifecycleFixture struct {
	service *server.Server
	client  *http.Client
	url     string
	served  chan error
}

func startLifecycle(t *testing.T, dispatch server.Probe, register func(*server.Server, *gin.Engine)) *lifecycleFixture {
	t.Helper()
	raw := `{
  "schema_version":"1","deployment_id":"lifecycle-fixture","public_origin":"https://pm.example.invalid",
  "server":{"drain_timeout":"2s"},
  "postgres":{"ownership":"managed","database":"pm_app","endpoint_ref":"secret://fixture/postgres"},
  "temporal":{"ownership":"managed","endpoint":"temporal.example.invalid:7233","namespace":"fixture","client_identity_ref":"secret://fixture/temporal","retention":"720h","task_queues":["pm-control"]},
  "artifacts":{"ownership":"managed","driver":"s3","endpoint":"https://objects.example.invalid","bucket":"fixture-objects","credential_ref":"secret://fixture/objects","attempt_key_policy":"unique_immutable"},
  "identity":{"issuer":"https://identity.example.invalid","client_id":"pm-server","client_secret_ref":"secret://fixture/oidc"},
  "encryption":{"wrapping_key_ref":"secret://fixture/wrapping"},"admission":{"profile":"development"},
  "limits":{"workspace_active_runs":4,"workflow_payload_bytes":65536,"workflow_fanout":32,"run_lifetime":"1h","event_buffer":128,"query_children":1,"query_child_memory_bytes":4294967296,"query_child_deadline":"5m","request_bytes":1048576,"metadata_deadline":"5s"}
}`
	document, err := config.Load(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := config.Validate(document, config.Prelaunch{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := server.New(configuration, func(context.Context) error { return nil }, dispatch, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	register(service, router)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	transport := &http.Transport{MaxIdleConns: 4, MaxIdleConnsPerHost: 4}
	fixture := &lifecycleFixture{
		service: service,
		client:  &http.Client{Transport: transport, Timeout: 5 * time.Second},
		url:     "http://" + listener.Addr().String(),
		served:  make(chan error, 1),
	}
	go func() { fixture.served <- service.Serve(ctx, listener, router) }()
	t.Cleanup(func() {
		cancel()
		closeContext, stop := context.WithTimeout(context.Background(), 4*time.Second)
		defer stop()
		if err := service.Drain(closeContext); err != nil && !errors.Is(err, server.ErrDrainDeadline) {
			t.Errorf("lifecycle cleanup: %v", err)
		}
		transport.CloseIdleConnections()
		fixture.requireStopped(t)
	})
	return fixture
}

func (fixture *lifecycleFixture) requireStopped(t *testing.T) {
	t.Helper()
	if fixture.served == nil {
		return
	}
	select {
	case err := <-fixture.served:
		fixture.served = nil
		if err != nil && !errors.Is(err, server.ErrDrainDeadline) {
			t.Fatalf("serve result: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("server goroutine outlived bounded drain")
	}
	connection, err := net.DialTimeout("tcp", strings.TrimPrefix(fixture.url, "http://"), 100*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		t.Fatal("owned listener remained open after drain")
	}
}

func awaitLifecycleSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(4 * time.Second):
		t.Fatal("owned lifecycle activity did not finish within its bound")
	}
}
