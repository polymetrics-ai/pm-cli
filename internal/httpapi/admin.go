package httpapi

import (
	"bytes"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/polymetrics-ai/pm-cli/internal/api"
)

// validateAdminHTTPRequest is reached only for the source-generated private
// route. It shares the duplicate/exact-field parser, not the ordinary public
// secret allowlist. Authentication, step-up, current grants and typed binding
// still precede any credential persistence.
func validateAdminHTTPRequest(request *http.Request) error {
	if request == nil || request.URL == nil || request.Method != http.MethodPost || request.URL.RawQuery != "" {
		return api.ErrMalformedEnvelope
	}
	keys := request.Header.Values("Idempotency-Key")
	if len(keys) != 1 || len(keys[0]) > 200 || strings.TrimSpace(keys[0]) == "" || !utf8.ValidString(keys[0]) || strings.ContainsAny(keys[0], "\x00\r\n") {
		return api.ErrMalformedEnvelope
	}
	mediaTypes := request.Header.Values("Content-Type")
	if len(mediaTypes) != 1 {
		return api.ErrMalformedEnvelope
	}
	mediaType, _, err := mime.ParseMediaType(mediaTypes[0])
	if err != nil || !strings.EqualFold(mediaType, "application/json") || request.Body == nil {
		return api.ErrMalformedEnvelope
	}
	const maximumAdminEnvelope = 1 << 20
	body, err := io.ReadAll(io.LimitReader(request.Body, maximumAdminEnvelope+1))
	request.Body.Close()
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	if err != nil || len(body) > maximumAdminEnvelope {
		return api.ErrMalformedEnvelope
	}
	return api.ValidateServerObjectJSON(body, adminCredentialAllowedFields[:], adminCredentialRequiredFields[:])
}
