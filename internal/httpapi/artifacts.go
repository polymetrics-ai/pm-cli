package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// UploadArtifactUnavailable authorizes a bodyless workspace capability probe
// before reporting that no object-byte backend is installed. The shared
// protocol boundary rejects nonempty request bodies before this method.
func (service *Service) UploadArtifactUnavailable(c *gin.Context) {
	if service == nil || c == nil || c.Request == nil {
		return
	}
	state, scope, err := scopedState(c.Request.Context(), c.Param("workspace_id"))
	if err != nil {
		writeProblem(c, problemForKnownError(err, nil))
		return
	}
	if err := service.identity.AuthorizeArtifactWrite(c.Request.Context(), state.identity.Actor, scope); err != nil {
		writeProblem(c, problemForKnownError(err, nil))
		return
	}
	writeUnavailableArtifact(c)
}

// InspectArtifactUnavailable authorizes the exact registered artifact before
// refusing availability; metadata is not a claim that bytes can be served.
func (service *Service) InspectArtifactUnavailable(c *gin.Context) {
	service.authorizeUnavailableArtifactRead(c)
}

// DownloadArtifactUnavailable shares the exact authorization and refusal
// precedence of inspection and never fabricates content or range metadata.
func (service *Service) DownloadArtifactUnavailable(c *gin.Context) {
	service.authorizeUnavailableArtifactRead(c)
}

func (service *Service) authorizeUnavailableArtifactRead(c *gin.Context) {
	if service == nil || c == nil || c.Request == nil {
		return
	}
	state, scope, err := scopedState(c.Request.Context(), c.Param("workspace_id"))
	if err != nil {
		writeProblem(c, problemForKnownError(err, nil))
		return
	}
	if err := service.identity.AuthorizeArtifactRead(
		c.Request.Context(),
		state.identity.Actor,
		scope,
		c.Param("artifact_id"),
	); err != nil {
		writeProblem(c, problemForKnownError(err, nil))
		return
	}
	writeUnavailableArtifact(c)
}

func writeUnavailableArtifact(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Writer.Header().Del("ETag")
	c.Writer.Header().Del("Content-Range")
	c.Writer.Header().Del("Accept-Ranges")
	writeProblem(c, newProblem(http.StatusServiceUnavailable, "capability_unavailable", "after_capability_change", nil))
}
