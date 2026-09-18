package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	maximumArtifactRangeHeaderBytes = 256
	artifactStreamChunkBytes        = 32 << 10
)

var (
	ErrMalformedArtifactResponse = errors.New("malformed PM artifact response")
	ErrArtifactLengthMismatch    = errors.New("PM artifact response length does not match Content-Length")
	ErrArtifactIntegrity         = errors.New("PM artifact integrity verification failed")
)

// ArtifactOperationResponse is the bounded response envelope for the manual
// upload and inspection operations deliberately excluded from the generated
// client. JSON200 is populated only for a strictly validated inspection.
type ArtifactOperationResponse struct {
	OperationID  string
	Body         []byte
	HTTPResponse *http.Response
	JSON200      *ArtifactMetadata
}

func (response *ArtifactOperationResponse) StatusCode() int {
	if response == nil || response.HTTPResponse == nil {
		return 0
	}
	return response.HTTPResponse.StatusCode
}

func (response *ArtifactOperationResponse) GetBody() []byte {
	if response == nil {
		return nil
	}
	return response.Body
}

func (response *ArtifactOperationResponse) ContentType() string {
	if response == nil || response.HTTPResponse == nil {
		return ""
	}
	return response.HTTPResponse.Header.Get("Content-Type")
}

func (client *QualifiedClient) UploadArtifactWithResponse(ctx context.Context, workspaceID WorkspaceID) (*ArtifactOperationResponse, error) {
	response, err := client.doArtifactOperation(ctx, "uploadArtifact", workspaceID, "")
	if err != nil {
		return nil, err
	}
	if response.StatusCode() >= http.StatusOK && response.StatusCode() < http.StatusMultipleChoices {
		return nil, ErrMalformedArtifactResponse
	}
	return response, nil
}

func (client *QualifiedClient) InspectArtifactWithResponse(ctx context.Context, workspaceID WorkspaceID, artifactID ArtifactID) (*ArtifactOperationResponse, error) {
	response, err := client.doArtifactOperation(ctx, "inspectArtifact", workspaceID, artifactID)
	if err != nil {
		return nil, err
	}
	if response.StatusCode() != http.StatusOK {
		if response.StatusCode() >= http.StatusOK && response.StatusCode() < http.StatusMultipleChoices {
			return nil, ErrMalformedArtifactResponse
		}
		return response, nil
	}
	metadata, err := decodeArtifactMetadata(response.Body, artifactID)
	if err != nil || !canonicalArtifactMetadataHeaders(response.HTTPResponse, metadata.Sha256) {
		return nil, ErrMalformedArtifactResponse
	}
	response.JSON200 = &metadata
	return response, nil
}

// DownloadArtifact streams one exact full or single-range representation to
// destination. It uses the already qualified and authenticated transport, owns
// the response body, performs no retry, and never buffers the complete object.
func (client *QualifiedClient) DownloadArtifact(
	ctx context.Context,
	workspaceID WorkspaceID,
	artifactID ArtifactID,
	rangeHeader string,
	destination io.Writer,
) (resultErr error) {
	if destination == nil {
		return ErrUnsafeClientRequest
	}
	generated, request, err := client.newArtifactRequest(ctx, "downloadArtifact", workspaceID, artifactID)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "*/*, application/problem+json")
	if rangeHeader != "" {
		if strings.ContainsAny(rangeHeader, "\r\n\x00") {
			return ErrUnsafeClientRequest
		}
		request.Header.Set("Range", rangeHeader)
	}
	if err := generated.applyEditors(ctx, request, nil); err != nil {
		return err
	}

	doer := artifactStreamingDoer(generated.Client)
	if doer == nil {
		return ErrClientTransport
	}
	response, err := doer.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return ErrClientTransport
	}
	if response == nil || response.Body == nil {
		return ErrClientTransport
	}
	defer func() {
		if closeErr := response.Body.Close(); resultErr == nil && closeErr != nil {
			resultErr = ErrClientTransport
		}
	}()

	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		return decodeArtifactProblem(ctx, response)
	}
	selection, validSelection := parseArtifactRangeRequest(rangeHeader)
	representation, err := validateArtifactRepresentation(response, rangeHeader, selection, validSelection)
	if err != nil {
		return err
	}
	return streamArtifactRepresentation(ctx, response.Body, destination, representation)
}

func (client *QualifiedClient) doArtifactOperation(
	ctx context.Context,
	operationID string,
	workspaceID WorkspaceID,
	artifactID ArtifactID,
) (*ArtifactOperationResponse, error) {
	generated, request, err := client.newArtifactRequest(ctx, operationID, workspaceID, artifactID)
	if err != nil {
		return nil, err
	}
	switch operationID {
	case "inspectArtifact":
		request.Header.Set("Accept", "application/json, application/problem+json")
	default:
		request.Header.Set("Accept", "application/problem+json")
	}
	if err := generated.applyEditors(ctx, request, nil); err != nil {
		return nil, err
	}
	response, err := generated.Client.Do(request)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, ErrClientTransport
	}
	if response == nil || response.Body == nil {
		return nil, ErrClientTransport
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, ErrClientTransport
	}
	if closeErr != nil {
		return nil, ErrClientTransport
	}
	if len(body) > maxPublicEnvelopeBytes {
		return nil, ErrMalformedEnvelope
	}
	return &ArtifactOperationResponse{
		OperationID:  operationID,
		Body:         body,
		HTTPResponse: response,
	}, nil
}

func (client *QualifiedClient) newArtifactRequest(
	ctx context.Context,
	operationID string,
	workspaceID WorkspaceID,
	artifactID ArtifactID,
) (*Client, *http.Request, error) {
	if client == nil || client.generated == nil || ctx == nil || !validIdentity(string(workspaceID), 512) ||
		!client.principal.Kind.Valid() || !validIdentity(client.principal.PrincipalId, 512) {
		return nil, nil, ErrUnsafeClientRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	control, present := OperationControlFor(operationID)
	if !present || control.OperationID != operationID || control.HasBody || control.HasProtocol ||
		control.RequiresIdempotencyKey || control.RequiresIfMatch || len(control.RequiredBodyFields) != 0 ||
		len(control.OptionalBodyFields) != 0 || len(control.QueryFields) != 0 {
		return nil, nil, ErrCompatibility
	}
	var method, canonicalPath string
	switch operationID {
	case "uploadArtifact":
		method = http.MethodPost
		canonicalPath = "/v1/workspaces/{workspace_id}/artifacts"
		if artifactID != "" {
			return nil, nil, ErrUnsafeClientRequest
		}
	case "inspectArtifact":
		method = http.MethodGet
		canonicalPath = "/v1/workspaces/{workspace_id}/artifacts/{artifact_id}"
	case "downloadArtifact":
		method = http.MethodGet
		canonicalPath = "/v1/workspaces/{workspace_id}/artifacts/{artifact_id}/content"
	default:
		return nil, nil, ErrCompatibility
	}
	routeOperation, routed := OperationIDForRoute(method, canonicalPath)
	if control.Method != method || control.Path != canonicalPath || !routed || routeOperation != operationID {
		return nil, nil, ErrCompatibility
	}
	if strings.Contains(canonicalPath, "{artifact_id}") && !validIdentity(string(artifactID), 512) {
		return nil, nil, ErrUnsafeClientRequest
	}
	path := strings.ReplaceAll(canonicalPath, "{workspace_id}", url.PathEscape(string(workspaceID)))
	path = strings.ReplaceAll(path, "{artifact_id}", url.PathEscape(string(artifactID)))
	if strings.Contains(path, "{") {
		return nil, nil, ErrCompatibility
	}
	generated, ok := client.generated.ClientInterface.(*Client)
	if !ok || generated == nil || generated.Client == nil {
		return nil, nil, ErrCompatibility
	}
	serverURL, err := url.Parse(generated.Server)
	if err != nil {
		return nil, nil, ErrCompatibility
	}
	if strings.HasPrefix(path, "/") {
		path = "." + path
	}
	requestURL, err := serverURL.Parse(path)
	if err != nil {
		return nil, nil, ErrCompatibility
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), nil)
	if err != nil {
		return nil, nil, ErrUnsafeClientRequest
	}
	return generated, request, nil
}

func artifactStreamingDoer(doer HttpRequestDoer) HttpRequestDoer {
	switch bounded := doer.(type) {
	case boundedResponseDoer:
		return bounded.next
	case *boundedResponseDoer:
		return bounded.next
	default:
		return doer
	}
}

func decodeArtifactMetadata(document []byte, expectedID ArtifactID) (ArtifactMetadata, error) {
	fields := []string{"artifact_id", "sha256", "byte_length", "media_type"}
	if _, err := decodeExactJSONObject(document, fields, fields); err != nil {
		return ArtifactMetadata{}, err
	}
	var metadata ArtifactMetadata
	if err := json.Unmarshal(document, &metadata); err != nil {
		return ArtifactMetadata{}, err
	}
	if metadata.ArtifactId != string(expectedID) || !validIdentity(metadata.ArtifactId, 512) ||
		!validArtifactDigest(metadata.Sha256) || metadata.ByteLength < 0 || metadata.MediaType == "" ||
		len(metadata.MediaType) > 512 {
		return ArtifactMetadata{}, ErrMalformedArtifactResponse
	}
	if _, _, err := mime.ParseMediaType(metadata.MediaType); err != nil {
		return ArtifactMetadata{}, ErrMalformedArtifactResponse
	}
	return metadata, nil
}

func canonicalArtifactMetadataHeaders(response *http.Response, digest string) bool {
	if response == nil || !exactArtifactHeader(response.Header, "Cache-Control", "no-store") ||
		!exactArtifactHeader(response.Header, "ETag", artifactETag(digest)) {
		return false
	}
	contentType, ok := singleArtifactHeader(response.Header, "Content-Type")
	if !ok {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

type artifactRangeKind uint8

const (
	artifactRangeInvalid artifactRangeKind = iota
	artifactRangeFull
	artifactRangeClosed
	artifactRangeFrom
	artifactRangeSuffix
)

type artifactRangeRequest struct {
	kind   artifactRangeKind
	start  int64
	end    int64
	suffix int64
}

func parseArtifactRangeRequest(value string) (artifactRangeRequest, bool) {
	if value == "" {
		return artifactRangeRequest{kind: artifactRangeFull}, true
	}
	if len(value) > maximumArtifactRangeHeaderBytes {
		return artifactRangeRequest{kind: artifactRangeInvalid}, false
	}
	value = strings.TrimSpace(value)
	unit, interval, found := strings.Cut(value, "=")
	if !found || !strings.EqualFold(unit, "bytes") || interval == "" || strings.ContainsAny(interval, " \t,") {
		return artifactRangeRequest{kind: artifactRangeInvalid}, false
	}
	first, last, found := strings.Cut(interval, "-")
	if !found || strings.Contains(last, "-") {
		return artifactRangeRequest{kind: artifactRangeInvalid}, false
	}
	switch {
	case first == "":
		length, ok := parseArtifactRangeBound(last)
		if !ok || length == 0 {
			return artifactRangeRequest{kind: artifactRangeInvalid}, false
		}
		return artifactRangeRequest{kind: artifactRangeSuffix, suffix: length}, true
	case last == "":
		start, ok := parseArtifactRangeBound(first)
		if !ok {
			return artifactRangeRequest{kind: artifactRangeInvalid}, false
		}
		return artifactRangeRequest{kind: artifactRangeFrom, start: start}, true
	default:
		start, startOK := parseArtifactRangeBound(first)
		end, endOK := parseArtifactRangeBound(last)
		if !startOK || !endOK || end < start {
			return artifactRangeRequest{kind: artifactRangeInvalid}, false
		}
		return artifactRangeRequest{kind: artifactRangeClosed, start: start, end: end}, true
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

type artifactRepresentation struct {
	length int64
	digest string
	whole  bool
}

func validateArtifactRepresentation(
	response *http.Response,
	rangeHeader string,
	selection artifactRangeRequest,
	validSelection bool,
) (artifactRepresentation, error) {
	if response == nil || response.Uncompressed || len(response.TransferEncoding) != 0 ||
		len(response.Header.Values("Content-Encoding")) != 0 ||
		!exactArtifactHeader(response.Header, "Accept-Ranges", "bytes") ||
		!exactArtifactHeader(response.Header, "Cache-Control", "no-store") ||
		!exactArtifactHeader(response.Header, "Content-Disposition", "attachment") {
		return artifactRepresentation{}, ErrMalformedArtifactResponse
	}
	contentType, ok := singleArtifactHeader(response.Header, "Content-Type")
	if !ok {
		return artifactRepresentation{}, ErrMalformedArtifactResponse
	}
	if _, _, err := mime.ParseMediaType(contentType); err != nil {
		return artifactRepresentation{}, ErrMalformedArtifactResponse
	}
	lengthValue, ok := singleArtifactHeader(response.Header, "Content-Length")
	if !ok {
		return artifactRepresentation{}, ErrMalformedArtifactResponse
	}
	length, ok := parseCanonicalArtifactInteger(lengthValue)
	if !ok || response.ContentLength != length {
		return artifactRepresentation{}, ErrMalformedArtifactResponse
	}
	etag, ok := singleArtifactHeader(response.Header, "ETag")
	if !ok {
		return artifactRepresentation{}, ErrMalformedArtifactResponse
	}
	digest, ok := artifactDigestFromETag(etag)
	if !ok {
		return artifactRepresentation{}, ErrMalformedArtifactResponse
	}

	switch response.StatusCode {
	case http.StatusOK:
		if rangeHeader != "" || !validSelection || selection.kind != artifactRangeFull ||
			len(response.Header.Values("Content-Range")) != 0 {
			return artifactRepresentation{}, ErrMalformedArtifactResponse
		}
		return artifactRepresentation{length: length, digest: digest, whole: true}, nil
	case http.StatusPartialContent:
		if rangeHeader == "" || !validSelection || selection.kind == artifactRangeFull || length == 0 {
			return artifactRepresentation{}, ErrMalformedArtifactResponse
		}
		contentRange, ok := singleArtifactHeader(response.Header, "Content-Range")
		if !ok {
			return artifactRepresentation{}, ErrMalformedArtifactResponse
		}
		start, end, total, ok := parseArtifactContentRange(contentRange)
		if !ok || end-start+1 != length {
			return artifactRepresentation{}, ErrMalformedArtifactResponse
		}
		expectedStart, expectedEnd, ok := resolveArtifactRangeRequest(selection, total)
		if !ok || start != expectedStart || end != expectedEnd {
			return artifactRepresentation{}, ErrMalformedArtifactResponse
		}
		return artifactRepresentation{
			length: length,
			digest: digest,
			whole:  start == 0 && end == total-1,
		}, nil
	default:
		return artifactRepresentation{}, ErrMalformedArtifactResponse
	}
}

func resolveArtifactRangeRequest(selection artifactRangeRequest, total int64) (int64, int64, bool) {
	if total <= 0 {
		return 0, 0, false
	}
	switch selection.kind {
	case artifactRangeClosed:
		if selection.start < 0 || selection.end < selection.start || selection.start >= total {
			return 0, 0, false
		}
		return selection.start, min(selection.end, total-1), true
	case artifactRangeFrom:
		if selection.start < 0 || selection.start >= total {
			return 0, 0, false
		}
		return selection.start, total - 1, true
	case artifactRangeSuffix:
		if selection.suffix <= 0 {
			return 0, 0, false
		}
		length := min(selection.suffix, total)
		return total - length, total - 1, true
	default:
		return 0, 0, false
	}
}

func parseArtifactContentRange(value string) (int64, int64, int64, bool) {
	if !strings.HasPrefix(value, "bytes ") {
		return 0, 0, 0, false
	}
	interval, totalValue, found := strings.Cut(strings.TrimPrefix(value, "bytes "), "/")
	if !found || strings.Contains(totalValue, "/") {
		return 0, 0, 0, false
	}
	startValue, endValue, found := strings.Cut(interval, "-")
	if !found || strings.Contains(endValue, "-") {
		return 0, 0, 0, false
	}
	start, startOK := parseCanonicalArtifactInteger(startValue)
	end, endOK := parseCanonicalArtifactInteger(endValue)
	total, totalOK := parseCanonicalArtifactInteger(totalValue)
	if !startOK || !endOK || !totalOK || total <= 0 || end < start || end >= total {
		return 0, 0, 0, false
	}
	return start, end, total, true
}

func parseCanonicalArtifactInteger(value string) (int64, bool) {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return 0, false
	}
	parsed, ok := parseArtifactRangeBound(value)
	return parsed, ok && strconv.FormatInt(parsed, 10) == value
}

func validArtifactDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func artifactETag(digest string) string {
	return `"sha256:` + digest + `"`
}

func artifactDigestFromETag(value string) (string, bool) {
	const prefix = `"sha256:`
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, `"`) || len(value) != len(prefix)+sha256.Size*2+1 {
		return "", false
	}
	digest := value[len(prefix) : len(value)-1]
	return digest, validArtifactDigest(digest)
}

func singleArtifactHeader(header http.Header, name string) (string, bool) {
	values := header.Values(name)
	if len(values) != 1 || values[0] == "" {
		return "", false
	}
	return values[0], true
}

func exactArtifactHeader(header http.Header, name, expected string) bool {
	value, ok := singleArtifactHeader(header, name)
	return ok && value == expected
}

func decodeArtifactProblem(ctx context.Context, response *http.Response) error {
	document, err := io.ReadAll(io.LimitReader(response.Body, maxPublicEnvelopeBytes+1))
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return ErrClientTransport
	}
	if len(document) > maxPublicEnvelopeBytes {
		return ErrMalformedEnvelope
	}
	problem, err := DecodeProblem(response.StatusCode, response.Header.Get("Content-Type"), document)
	if err != nil {
		return err
	}
	if response.StatusCode == http.StatusRequestedRangeNotSatisfiable && !canonicalArtifactRangeRefusal(response.Header) {
		return ErrMalformedArtifactResponse
	}
	return &ProblemError{Decoded: problem}
}

func canonicalArtifactRangeRefusal(header http.Header) bool {
	if !exactArtifactHeader(header, "Accept-Ranges", "bytes") ||
		!exactArtifactHeader(header, "Cache-Control", "no-store") {
		return false
	}
	value, ok := singleArtifactHeader(header, "Content-Range")
	if !ok || !strings.HasPrefix(value, "bytes */") {
		return false
	}
	total, ok := parseCanonicalArtifactInteger(strings.TrimPrefix(value, "bytes */"))
	return ok && total >= 0
}

func streamArtifactRepresentation(
	ctx context.Context,
	body io.Reader,
	destination io.Writer,
	representation artifactRepresentation,
) error {
	var verifier hash.Hash
	if representation.whole {
		verifier = sha256.New()
	}
	remaining := representation.length
	var buffer []byte
	if remaining > 0 {
		buffer = make([]byte, min(int64(artifactStreamChunkBytes), remaining))
	}
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		limit := int64(len(buffer))
		if remaining < limit {
			limit = remaining
		}
		read, readErr := body.Read(buffer[:int(limit)])
		if read < 0 || int64(read) > limit {
			return ErrClientTransport
		}
		if read > 0 {
			written, writeErr := destination.Write(buffer[:read])
			if writeErr != nil {
				return writeErr
			}
			if written != read {
				return io.ErrShortWrite
			}
			if verifier != nil {
				_, _ = verifier.Write(buffer[:read])
			}
			remaining -= int64(read)
		}
		if readErr != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
				if remaining != 0 || errors.Is(readErr, io.ErrUnexpectedEOF) {
					return ErrArtifactLengthMismatch
				}
				break
			}
			return ErrClientTransport
		}
		if read == 0 {
			return ErrClientTransport
		}
	}

	var extra [1]byte
	read, readErr := body.Read(extra[:])
	if read != 0 {
		return ErrArtifactLengthMismatch
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if !errors.Is(readErr, io.EOF) {
		return ErrClientTransport
	}
	if verifier != nil && hex.EncodeToString(verifier.Sum(nil)) != representation.digest {
		return ErrArtifactIntegrity
	}
	return nil
}
