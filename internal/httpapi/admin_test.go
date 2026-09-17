package httpapi

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestPrivateIngressBoundary(t *testing.T) {
	canary := []byte("isolated-private-ingress-canary")
	encoded := base64.StdEncoding.EncodeToString(canary)
	privatePath := "/admin/v1/workspaces/alpha/connections/connection-alpha/credentials"
	validBody := fmt.Sprintf(`{"credential_value":%q,"scopes":["read"]}`, encoded)
	router := gin.New()
	// The real registered boundary must still reach authentication. This fixture
	// denies it; encrypted storage and step-up have separate same-stack cases.
	if err := RegisterPMStrictHandlers(router, nil, nil, StrictGinServerOptions{}, GinServerOptions{
		Middlewares: []MiddlewareFunc{func(c *gin.Context) { c.AbortWithStatus(http.StatusUnauthorized) }},
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		path   string
		body   string
		status int
	}{
		{name: "private shape still requires authentication", path: privatePath, body: validBody, status: http.StatusUnauthorized},
		{name: "duplicate private field is refused", path: privatePath, body: fmt.Sprintf(`{"credential_value":%q,"credential_value":%q,"scopes":[]}`, encoded, encoded), status: http.StatusBadRequest},
		{name: "wrong-case private field is refused", path: privatePath, body: fmt.Sprintf(`{"Credential_Value":%q,"scopes":[]}`, encoded), status: http.StatusBadRequest},
		{name: "undeclared private field is refused", path: privatePath, body: fmt.Sprintf(`{"credential_value":%q,"scopes":[],"provider_secret":%q}`, encoded, encoded), status: http.StatusBadRequest},
		{name: "private input cannot enter an ordinary route", path: "/v1/workspaces/alpha/memberships", body: validBody, status: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "private-boundary")
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			request.Body.Close()
			if response.Code != test.status {
				t.Fatalf("HTTP status = %d, want %d", response.Code, test.status)
			}
			if strings.Contains(response.Body.String(), string(canary)) || strings.Contains(response.Body.String(), encoded) {
				t.Fatal("private input appeared in a response")
			}
		})
	}

	input := AdminCredentialInput{CredentialValue: canary, Scopes: []string{"read"}}
	diagnostic := fmt.Sprintf("%v %#v", input, input)
	for _, representation := range []string{string(canary), encoded, fmt.Sprintf("%x", canary), fmt.Sprint(canary)} {
		if strings.Contains(diagnostic, representation) {
			t.Fatal("generated private input leaked through diagnostic formatting")
		}
	}
}
