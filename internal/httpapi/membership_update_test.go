package httpapi

import (
	"net/http"
	"testing"
)

// Check: the membership update route has the authenticated request policy. It
// needs no dependency stack. Defect it catches: a route that the request
// middleware refuses with 400 invalid_request before its handler.
func TestMembershipUpdateRouteHasAuthenticatedPolicy(t *testing.T) {
	policy, known := policyForRequest(http.MethodPatch, "/v1/workspaces/:workspace_id/memberships/:id")
	if !known || policy != policyAuthenticated {
		t.Fatalf("policy for PATCH /v1/workspaces/:workspace_id/memberships/:id = %v, %v; want the authenticated policy", policy, known)
	}
}
