package httpapi

import (
	"errors"
	"io"
	"slices"

	"github.com/gin-gonic/gin"
)

// RegisterRoutes composes process transport protection with the one generated
// ordinary API tree. Authentication middleware executes after generated strict
// envelope validation and before application methods.
func RegisterRoutes(router *gin.Engine, configuration TransportPolicy, service StrictServerInterface, diagnostics io.Writer, middlewares ...MiddlewareFunc) error {
	if router == nil || service == nil {
		return errors.New("router and application services are required")
	}
	boundary, err := RequestBoundary(configuration, diagnostics)
	if err != nil {
		return err
	}
	if err := router.SetTrustedProxies(slices.Clone(configuration.TrustedProxies)); err != nil {
		return errors.New("trusted proxy configuration is invalid")
	}
	router.Use(boundary)
	return RegisterPMStrictHandlers(router, service, nil, StrictGinServerOptions{}, GinServerOptions{
		Middlewares: middlewares,
	})
}
