package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// D4-2: the device poll cadence goes out as whole seconds, rounded up, so a
// client that follows Retry-After never polls before the stored next poll time.
func TestDevicePollRetryAfterRoundsUp(t *testing.T) {
	for _, test := range []struct {
		wait time.Duration
		want string
	}{
		{wait: time.Nanosecond, want: "1"},
		{wait: 999 * time.Millisecond, want: "1"},
		{wait: time.Second, want: "1"},
		{wait: time.Second + time.Millisecond, want: "2"},
		{wait: 2500 * time.Millisecond, want: "3"},
		{wait: 5 * time.Second, want: "5"},
		{wait: time.Minute, want: "60"},
		{wait: 0, want: ""},
		{wait: -time.Second, want: ""},
	} {
		recorder := httptest.NewRecorder()
		ctx := context.WithValue(context.Background(), requestStateKey{}, requestState{writer: recorder})
		setRetryAfter(ctx, test.wait)
		if got := recorder.Header().Values("Retry-After"); len(got) > 1 || recorder.Header().Get("Retry-After") != test.want {
			t.Errorf("wait %v: Retry-After = %q, want %q", test.wait, got, test.want)
		}
	}
}

// The device poll Retry-After never leaves the public 1 to 60 second contract
// that the client enforces, even when the stored wait is longer.
func TestDevicePollRetryAfterNeverExceedsSixtySeconds(t *testing.T) {
	for _, wait := range []time.Duration{time.Minute + time.Nanosecond, 61 * time.Second, 10 * time.Minute} {
		recorder := httptest.NewRecorder()
		ctx := context.WithValue(context.Background(), requestStateKey{}, requestState{writer: recorder})
		setRetryAfter(ctx, wait)
		if got := recorder.Header().Values("Retry-After"); len(got) != 1 || got[0] != "60" {
			t.Errorf("wait %v: Retry-After = %q, want the contract bound \"60\"", wait, got)
		}
	}
}
