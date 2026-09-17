package httpapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// This consumer exercises the real generated binder and process middleware. It
// deliberately has no admission, authentication, persistence, or provider effect.
type ginBoundaryConsumer struct {
	*Service
	observeInfo  func(context.Context) error
	observeInput func(api.InvocationRequest)
}

func (consumer *ginBoundaryConsumer) GetServerInfo(ctx context.Context, request GetServerInfoRequestObject) (GetServerInfoResponseObject, error) {
	if consumer.observeInfo != nil {
		if err := consumer.observeInfo(ctx); err != nil {
			return nil, err
		}
	}
	return consumer.Service.GetServerInfo(ctx, request)
}

func (consumer *ginBoundaryConsumer) SubmitInvocation(_ context.Context, request SubmitInvocationRequestObject) (SubmitInvocationResponseObject, error) {
	if consumer.observeInput != nil {
		consumer.observeInput(*request.Body)
	}
	return problemResponse{problem: newProblem(http.StatusServiceUnavailable, "capability_unavailable", "after_capability_change", nil)}, nil
}

func ginBoundaryRouter(t *testing.T, consumer *ginBoundaryConsumer, diagnostics io.Writer) *gin.Engine {
	t.Helper()
	consumer.Service = &Service{instanceID: "gin-boundary-fixture"}
	router := gin.New()
	if err := RegisterRoutes(router, TransportPolicy{
		PublicOrigin:     "https://pm.example.test",
		TrustedProxies:   []string{"192.0.2.4/32"},
		RequestTimeout:   5 * time.Second,
		MaximumBodyBytes: 2048,
	}, consumer, diagnostics); err != nil {
		t.Fatal(err)
	}
	return router
}

func TestGinStrictBoundary(t *testing.T) {
	t.Run("typed binder preserves exact values and refuses invalid bodies before the consumer", func(t *testing.T) {
		const decimal = "9007199254740993123456789"
		exact, err := contract.NewIntegerValue(decimal)
		if err != nil {
			t.Fatal(err)
		}
		var request api.InvocationRequest
		if err := json.Unmarshal([]byte(`{"protocol":{"wire_version":"pm.api/v1","required_features":["exact-values"],"request_key":"gin-boundary","intent_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","expected_revision":"revision-01"},"connection_ref":"connection-01","operation_ref":"operation-01@revision-01","lane":"direct_read","input":{"presence":"value","type":"object","fields":{}},"result_controls":{}}`), &request); err != nil {
			t.Fatal(err)
		}
		request.Input = contract.NewObjectValue(map[string]contract.PresentValue{"large": exact})
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		expected, err := json.Marshal(request.Input)
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			name   string
			body   string
			status int
		}{
			{name: "exact typed value", body: string(body), status: http.StatusServiceUnavailable},
			{name: "malformed", body: string(body[:len(body)-1]), status: http.StatusBadRequest},
			{name: "duplicate key", body: strings.Replace(string(body), `"connection_ref":`, `"connection_ref":"other","connection_ref":`, 1), status: http.StatusBadRequest},
			{name: "trailing document", body: string(body) + `{}`, status: http.StatusBadRequest},
			{name: "unknown controlled field", body: strings.TrimSuffix(string(body), "}") + `,"unregistered_control":true}`, status: http.StatusBadRequest},
			{name: "bounded body", body: string(body) + strings.Repeat(" ", 2048), status: http.StatusBadRequest},
		} {
			t.Run(test.name, func(t *testing.T) {
				var observed []byte
				consumer := &ginBoundaryConsumer{observeInput: func(input api.InvocationRequest) {
					var err error
					observed, err = json.Marshal(input.Input)
					if err != nil {
						t.Error(err)
					}
				}}
				router := ginBoundaryRouter(t, consumer, io.Discard)
				request := httptest.NewRequest(http.MethodPost, "https://pm.example.test/v1/workspaces/workspace-01/invocations", strings.NewReader(test.body))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Idempotency-Key", "gin-boundary")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != test.status {
					t.Fatalf("status = %d, want %d", response.Code, test.status)
				}
				if test.status == http.StatusServiceUnavailable {
					if !bytes.Equal(observed, expected) {
						t.Fatal("generated binding changed the exact typed integer")
					}
				} else if observed != nil {
					t.Fatal("invalid body reached the consumer")
				}
			})
		}
	})

	t.Run("forwarding establishes provenance rather than authentication", func(t *testing.T) {
		for _, test := range []struct {
			name    string
			peer    string
			tls     bool
			headers http.Header
			status  int
		}{
			{name: "direct TLS", peer: "198.51.100.8:1234", tls: true, status: http.StatusOK},
			{name: "explicit trusted HTTPS edge", peer: "192.0.2.4:1234", headers: http.Header{"X-Forwarded-Proto": {"https"}, "X-Forwarded-For": {"203.0.113.9"}}, status: http.StatusOK},
			{name: "untrusted forwarding", peer: "198.51.100.8:1234", tls: true, headers: http.Header{"X-Forwarded-For": {"203.0.113.9"}}, status: http.StatusBadRequest},
			{name: "trusted edge cannot downgrade", peer: "192.0.2.4:1234", tls: true, headers: http.Header{"X-Forwarded-Proto": {"http"}}, status: http.StatusBadRequest},
			{name: "forwarded host mismatch", peer: "192.0.2.4:1234", tls: true, headers: http.Header{"X-Forwarded-Host": {"foreign.example.test"}}, status: http.StatusBadRequest},
			{name: "malformed client chain", peer: "192.0.2.4:1234", tls: true, headers: http.Header{"X-Forwarded-For": {"not-an-address"}}, status: http.StatusBadRequest},
			{name: "alternative forwarding grammar", peer: "192.0.2.4:1234", tls: true, headers: http.Header{"Forwarded": {"proto=https"}}, status: http.StatusBadRequest},
			{name: "unexpected origin", peer: "198.51.100.8:1234", tls: true, headers: http.Header{"Origin": {"https://foreign.example.test"}}, status: http.StatusBadRequest},
			{name: "ambiguous origin", peer: "198.51.100.8:1234", tls: true, headers: http.Header{"Origin": {"https://pm.example.test", "https://pm.example.test"}}, status: http.StatusBadRequest},
			{name: "loopback compatibility is nonsecret", peer: "127.0.0.1:1234", status: http.StatusOK},
			{name: "loopback does not admit plaintext credentials", peer: "127.0.0.1:1234", headers: http.Header{"Authorization": {"Bearer synthetic-fixture-token"}}, status: http.StatusBadRequest},
		} {
			t.Run(test.name, func(t *testing.T) {
				called := false
				router := ginBoundaryRouter(t, &ginBoundaryConsumer{observeInfo: func(context.Context) error {
					called = true
					return nil
				}}, io.Discard)
				request := httptest.NewRequest(http.MethodGet, "http://pm.example.test/v1/info", nil)
				request.RemoteAddr = test.peer
				request.Header = test.headers.Clone()
				if test.tls {
					request.TLS = &tls.ConnectionState{}
				}
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if response.Code != test.status || called != (test.status == http.StatusOK) {
					t.Fatalf("status=%d consumer_called=%t; want status=%d", response.Code, called, test.status)
				}
			})
		}
	})

	t.Run("composed middleware preserves loopback public discovery", func(t *testing.T) {
		called := false
		consumer := &ginBoundaryConsumer{observeInfo: func(context.Context) error {
			called = true
			return nil
		}}
		consumer.Service = &Service{instanceID: "gin-boundary-fixture"}
		router := gin.New()
		if err := RegisterRoutes(router, TransportPolicy{
			PublicOrigin:     "https://pm.example.test",
			TrustedProxies:   []string{"192.0.2.4/32"},
			RequestTimeout:   5 * time.Second,
			MaximumBodyBytes: 2048,
		}, consumer, io.Discard, consumer.Service.Middleware()); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodGet, "http://pm.example.test/v1/info", nil)
		request.RemoteAddr = "127.0.0.1:1234"
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !called {
			t.Fatalf("status=%d consumer_called=%t; want status=%d consumer_called=true", response.Code, called, http.StatusOK)
		}
	})

	t.Run("panic and error material never crosses the boundary", func(t *testing.T) {
		const canary = "synthetic-provider-secret-must-not-escape"
		for _, panicMode := range []bool{false, true} {
			var diagnostics bytes.Buffer
			router := ginBoundaryRouter(t, &ginBoundaryConsumer{observeInfo: func(context.Context) error {
				if panicMode {
					panic(canary)
				}
				return errors.New(canary)
			}}, &diagnostics)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://pm.example.test/v1/info", nil))
			if response.Code != http.StatusInternalServerError || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/problem+json") {
				t.Fatalf("panic=%t returned status=%d content_type=%q", panicMode, response.Code, response.Header().Get("Content-Type"))
			}
			if strings.Contains(response.Body.String(), canary) || strings.Contains(diagnostics.String(), canary) {
				t.Fatal("raw application failure escaped in the response or diagnostics")
			}
		}
	})

	t.Run("client cancellation reaches the standard operation context", func(t *testing.T) {
		entered := make(chan struct{})
		observed := make(chan error, 1)
		release := make(chan struct{})
		router := ginBoundaryRouter(t, &ginBoundaryConsumer{observeInfo: func(ctx context.Context) error {
			close(entered)
			select {
			case <-ctx.Done():
				observed <- ctx.Err()
			case <-release:
			}
			return context.Canceled
		}}, io.Discard)
		server := httptest.NewTLSServer(router)
		t.Cleanup(server.Close)
		t.Cleanup(func() { close(release) })
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/info", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Host = "pm.example.test"
		clientResult := make(chan error, 1)
		go func() {
			response, err := server.Client().Do(request)
			if response != nil {
				_ = response.Body.Close()
			}
			clientResult <- err
		}()
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("operation did not receive the request")
		}
		cancel()
		select {
		case err := <-observed:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("operation context error = %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("client cancellation did not reach the operation context")
		}
		if err := <-clientResult; !errors.Is(err, context.Canceled) {
			t.Fatalf("client error = %v", err)
		}
	})
}
