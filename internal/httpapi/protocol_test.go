package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// Conditional revision absence belongs to the authenticated application, which
// checks replay before requiring a revision for a new mutation. Malformed or
// contradictory supplied metadata still belongs to the pre-handler boundary.
func TestConditionalRevisionReachesAuthorization(t *testing.T) {
	const body = `{"protocol":{"wire_version":"pm.api/v1","required_features":[],"request_key":"revision-boundary","intent_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","expected_revision":"1"},"membership":{"principal_ref":"ada","role":"reader","resource_grants":[],"expires_at":null}}`

	router := gin.New()
	// This protected fixture refuses authorization before any service or durable
	// I/O. A service is deliberately unreachable, not a simulated mutation.
	err := RegisterPMStrictHandlers(router, nil, nil, StrictGinServerOptions{}, GinServerOptions{
		Middlewares: []MiddlewareFunc{func(c *gin.Context) {
			c.AbortWithStatus(http.StatusForbidden)
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		values []string
		status int
	}{
		{name: "absent revision still requires authorization", status: http.StatusForbidden},
		{name: "matching revision still requires authorization", values: []string{"1"}, status: http.StatusForbidden},
		{name: "duplicate revision is malformed", values: []string{"1", "1"}, status: http.StatusBadRequest},
		{name: "explicit empty revision is malformed", values: []string{""}, status: http.StatusBadRequest},
		{name: "revision contradicting intent is malformed", values: []string{"2"}, status: http.StatusBadRequest},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/workspaces/alpha/memberships", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "revision-boundary")
			for _, value := range test.values {
				request.Header.Add("If-Match", value)
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("HTTP status = %d, want %d", response.Code, test.status)
			}
		})
	}
}

func TestBodylessOperationBoundary(t *testing.T) {
	router := gin.New()
	err := RegisterPMStrictHandlers(router, nil, nil, StrictGinServerOptions{}, GinServerOptions{
		Middlewares: []MiddlewareFunc{func(c *gin.Context) {
			c.AbortWithStatus(http.StatusNoContent)
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		target string
		body   string
		status int
	}{
		{name: "bodyless request needs no mutation envelope", target: "/v1/info", status: http.StatusNoContent},
		{name: "undeclared query metadata is refused", target: "/v1/info?wire_version=pm.api%2Fv99", status: http.StatusBadRequest},
		{name: "malformed query is refused", target: "/v1/info?cursor=%zz", status: http.StatusBadRequest},
		{name: "undeclared body is refused", target: "/v1/info", body: `{}`, status: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.target, strings.NewReader(test.body))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("HTTP status = %d, want %d", response.Code, test.status)
			}
		})
	}
}
