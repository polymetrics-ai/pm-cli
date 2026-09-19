package httpapi

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
)

const maximumArtifactRangeHeaderBytes = 256

// UploadArtifactUnavailable authorizes a bodyless workspace capability probe
// before reporting that public artifact upload is unavailable. An installed
// read backend does not enable this upload protocol.
func (service *Service) UploadArtifactUnavailable(c *gin.Context) {
	if service == nil || c == nil || c.Request == nil {
		return
	}
	prepareArtifactResponse(c)
	state, scope, err := scopedState(c.Request.Context(), c.Param("workspace_id"))
	if err != nil {
		writeProblem(c, problemForKnownError(err, nil))
		return
	}
	if err := service.identity.AuthorizeArtifactWrite(c.Request.Context(), state.identity.Actor, scope); err != nil {
		writeProblem(c, problemForKnownError(err, nil))
		return
	}
	writeUnavailableArtifactUpload(c)
}

// InspectArtifact authorizes one registered artifact and verifies its complete
// bytes before returning the deliberately limited public metadata projection.
func (service *Service) InspectArtifact(c *gin.Context) {
	if service == nil || c == nil || c.Request == nil {
		return
	}
	prepareArtifactResponse(c)
	state, scope, err := scopedState(c.Request.Context(), c.Param("workspace_id"))
	if err != nil {
		writeProblem(c, problemForKnownError(err, nil))
		return
	}
	metadata, err := service.artifacts.Inspect(
		c.Request.Context(),
		state.identity.Actor,
		scope,
		c.Param("artifact_id"),
	)
	if err != nil {
		writeProblem(c, problemForKnownError(err, nil))
		return
	}
	c.Header("ETag", artifactValidator(metadata.SHA256))
	c.Header("X-Content-Type-Options", "nosniff")
	c.JSON(http.StatusOK, api.ArtifactMetadata{
		ArtifactId: metadata.ArtifactID,
		Sha256:     metadata.SHA256,
		ByteLength: metadata.ByteLength,
		MediaType:  metadata.MediaType,
	})
}

// DownloadArtifact authorizes the registered artifact before backend
// availability or range validity is disclosed, then streams exactly one fully
// verified full or partial representation.
func (service *Service) DownloadArtifact(c *gin.Context) {
	if service == nil || c == nil || c.Request == nil {
		return
	}
	prepareArtifactResponse(c)
	state, scope, err := scopedState(c.Request.Context(), c.Param("workspace_id"))
	if err != nil {
		writeProblem(c, problemForKnownError(err, nil))
		return
	}
	content, err := service.artifacts.Open(
		c.Request.Context(),
		state.identity.Actor,
		scope,
		c.Param("artifact_id"),
		parseArtifactRange(c.Request),
	)
	if err != nil {
		var rangeFailure *app.ArtifactRangeFailure
		if errors.As(err, &rangeFailure) {
			c.Header("Accept-Ranges", "bytes")
			c.Header("Content-Range", "bytes */"+strconv.FormatInt(rangeFailure.ByteLength, 10))
		}
		writeProblem(c, problemForKnownError(err, nil))
		return
	}
	defer content.Body.Close()

	status := http.StatusOK
	if content.Partial {
		status = http.StatusPartialContent
		end := content.Offset + content.Length - 1
		c.Header(
			"Content-Range",
			"bytes "+strconv.FormatInt(content.Offset, 10)+"-"+strconv.FormatInt(end, 10)+
				"/"+strconv.FormatInt(content.Metadata.ByteLength, 10),
		)
	}
	c.Header("Accept-Ranges", "bytes")
	c.Header("Content-Length", strconv.FormatInt(content.Length, 10))
	c.Header("Content-Type", content.Metadata.MediaType)
	// Provider-controlled media must never become an active PM-origin document.
	c.Header("Content-Disposition", "attachment")
	c.Header("ETag", artifactValidator(content.Metadata.SHA256))
	c.Header("X-Content-Type-Options", "nosniff")
	c.Status(status)
	if content.Length == 0 {
		return
	}
	// The fixed Content-Length makes a short verified reader a transport failure,
	// never a smaller successful representation. CopyN prevents extra bytes.
	written, copyErr := io.CopyN(c.Writer, content.Body, content.Length)
	if copyErr != nil || written != content.Length {
		return
	}
}

func parseArtifactRange(request *http.Request) app.ArtifactRangeSelection {
	value, present, err := oneHeader(request, "Range")
	if err != nil {
		return app.ArtifactRangeSelection{Kind: app.ArtifactRangeInvalid}
	}
	if !present {
		return app.ArtifactRangeSelection{Kind: app.ArtifactRangeFull}
	}
	if len(value) > maximumArtifactRangeHeaderBytes {
		return app.ArtifactRangeSelection{Kind: app.ArtifactRangeInvalid}
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return app.ArtifactRangeSelection{Kind: app.ArtifactRangeInvalid}
	}
	unit, value, found := strings.Cut(value, "=")
	if !found || !strings.EqualFold(unit, "bytes") || value == "" ||
		strings.ContainsAny(value, " \t,") {
		return app.ArtifactRangeSelection{Kind: app.ArtifactRangeInvalid}
	}
	first, last, found := strings.Cut(value, "-")
	if !found || strings.Contains(last, "-") {
		return app.ArtifactRangeSelection{Kind: app.ArtifactRangeInvalid}
	}
	switch {
	case first == "":
		length, ok := parseArtifactRangeBound(last)
		if !ok || length == 0 {
			return app.ArtifactRangeSelection{Kind: app.ArtifactRangeInvalid}
		}
		return app.ArtifactRangeSelection{Kind: app.ArtifactRangeSuffix, SuffixLength: length}
	case last == "":
		start, ok := parseArtifactRangeBound(first)
		if !ok {
			return app.ArtifactRangeSelection{Kind: app.ArtifactRangeInvalid}
		}
		return app.ArtifactRangeSelection{Kind: app.ArtifactRangeFrom, Start: start}
	default:
		start, startOK := parseArtifactRangeBound(first)
		end, endOK := parseArtifactRangeBound(last)
		if !startOK || !endOK || end < start {
			return app.ArtifactRangeSelection{Kind: app.ArtifactRangeInvalid}
		}
		return app.ArtifactRangeSelection{Kind: app.ArtifactRangeClosed, Start: start, End: end}
	}
}

func parseArtifactRangeBound(value string) (int64, bool) {
	if value == "" {
		return 0, false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, false
		}
	}
	bound, err := strconv.ParseInt(value, 10, 64)
	return bound, err == nil
}

func artifactValidator(sha256 string) string {
	return `"sha256:` + sha256 + `"`
}

func prepareArtifactResponse(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Writer.Header().Del("ETag")
	c.Writer.Header().Del("Content-Length")
	c.Writer.Header().Del("Content-Range")
	c.Writer.Header().Del("Accept-Ranges")
}

func writeUnavailableArtifactUpload(c *gin.Context) {
	writeProblem(c, newProblem(http.StatusServiceUnavailable, "capability_unavailable", "after_capability_change", nil))
}
