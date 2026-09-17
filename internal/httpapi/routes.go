package httpapi

import (
	"errors"
	"io"
	"slices"

	"github.com/gin-gonic/gin"
)

type routeService interface {
	StrictServerInterface
	StreamRunEvents(*gin.Context)
	UploadArtifactUnavailable(*gin.Context)
	InspectArtifactUnavailable(*gin.Context)
	DownloadArtifactUnavailable(*gin.Context)
}

// RegisterRoutes composes process transport protection with the generated
// ordinary API and manual streaming routes. Both apply the same protocol guard
// before application authentication and authorization.
func RegisterRoutes(router *gin.Engine, configuration TransportPolicy, service routeService, diagnostics io.Writer, middlewares ...MiddlewareFunc) error {
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
	if err := RegisterPMStrictHandlers(router, service, nil, StrictGinServerOptions{}, GinServerOptions{
		Middlewares: middlewares,
	}); err != nil {
		return err
	}
	streams := router.Group("")
	streams.Use(gin.HandlerFunc(PMProtocolMiddleware))
	for _, middleware := range middlewares {
		streams.Use(gin.HandlerFunc(middleware))
	}
	streams.GET("/v1/workspaces/:workspace_id/runs/:run_id/events", service.StreamRunEvents)
	streams.POST("/v1/workspaces/:workspace_id/artifacts", service.UploadArtifactUnavailable)
	streams.GET("/v1/workspaces/:workspace_id/artifacts/:artifact_id", service.InspectArtifactUnavailable)
	streams.GET("/v1/workspaces/:workspace_id/artifacts/:artifact_id/content", service.DownloadArtifactUnavailable)
	return nil
}
