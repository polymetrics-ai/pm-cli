package httpapi

import (
	"testing"

	"github.com/gin-gonic/gin"
)

// TestEveryPublicRouteHasARequestPolicy checks that the request policy table
// names every generated public route. It needs no dependency stack. Defect it
// catches: a new route that the request middleware refuses with 400
// invalid_request for every caller, before authentication and before its
// handler, because the policy table does not name it.
func TestEveryPublicRouteHasARequestPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	// Registration only stores the generated wrappers; no handler runs here.
	RegisterHandlers(router, nil)
	routes := router.Routes()
	if len(routes) == 0 {
		t.Fatal("the generated public API registered no route")
	}
	for _, route := range routes {
		if _, known := policyForRequest(route.Method, route.Path); !known {
			t.Errorf("generated route %s %s has no request policy", route.Method, route.Path)
		}
	}
}
