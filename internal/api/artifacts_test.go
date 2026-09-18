package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	artifactClientTestInstance   = "instance-artifact-client"
	artifactClientTestWorkspace  = "workspace-artifact-client"
	artifactClientTestID         = "artifact-one"
	artifactClientTestCredential = "pm-artifact-client-session"
)

func TestInspectArtifactDecodesStrictMetadata(t *testing.T) {
	digest := strings.Repeat("a", sha256.Size*2)
	valid, err := json.Marshal(ArtifactMetadata{
		ArtifactId: artifactClientTestID,
		ByteLength: 17,
		MediaType:  "application/octet-stream",
		Sha256:     digest,
	})
	if err != nil {
		t.Fatalf("marshal metadata fixture: %v", err)
	}
	tests := []struct {
		name      string
		body      []byte
		wantError bool
	}{
		{name: "valid", body: valid},
		{
			name: "unknown field",
			body: []byte(`{"artifact_id":"artifact-one","byte_length":17,"media_type":"application/octet-stream","sha256":"` +
				digest + `","backend_key":"must-not-cross"}`),
			wantError: true,
		},
		{
			name: "mismatched artifact identity",
			body: []byte(`{"artifact_id":"artifact-two","byte_length":17,"media_type":"application/octet-stream","sha256":"` +
				digest + `"}`),
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var body *artifactTrackingReadCloser
			client, doer := newScriptedArtifactClient(t, func(request *http.Request) (*http.Response, error) {
				requireArtifactRequest(t, request, http.MethodGet, "/v1/workspaces/"+artifactClientTestWorkspace+"/artifacts/"+artifactClientTestID, "")
				body = &artifactTrackingReadCloser{reader: bytes.NewReader(test.body)}
				header := make(http.Header)
				header.Set("Cache-Control", "no-store")
				header.Set("Content-Type", "application/json")
				header.Set("ETag", artifactETag(digest))
				return &http.Response{
					StatusCode:    http.StatusOK,
					Header:        header,
					Body:          body,
					ContentLength: int64(len(test.body)),
				}, nil
			})

			response, requestErr := client.InspectArtifactWithResponse(
				t.Context(),
				WorkspaceID(artifactClientTestWorkspace),
				ArtifactID(artifactClientTestID),
			)
			if test.wantError {
				if !errors.Is(requestErr, ErrMalformedArtifactResponse) || response != nil {
					t.Fatalf("inspection = (%#v, %v), want malformed response", response, requestErr)
				}
			} else {
				if requestErr != nil || response == nil || response.JSON200 == nil {
					t.Fatalf("inspection = (%#v, %v), want decoded metadata", response, requestErr)
				}
				if *response.JSON200 != (ArtifactMetadata{
					ArtifactId: artifactClientTestID,
					ByteLength: 17,
					MediaType:  "application/octet-stream",
					Sha256:     digest,
				}) {
					t.Fatalf("metadata = %#v", *response.JSON200)
				}
				if !bytes.Equal(response.Body, test.body) {
					t.Fatal("inspection did not retain the exact bounded response body")
				}
			}
			if body == nil || !body.closed {
				t.Fatal("inspection response body was not closed")
			}
			if doer.artifactCalls != 1 {
				t.Fatalf("inspection calls = %d, want 1", doer.artifactCalls)
			}
		})
	}
}

func TestDownloadArtifactStreamsBeyondEnvelopeLimitAndVerifiesDigest(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789abcdef"), maxPublicEnvelopeBytes/16+4096)
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	var body *artifactTrackingReadCloser
	client, doer := newScriptedArtifactClient(t, func(request *http.Request) (*http.Response, error) {
		requireArtifactRequest(t, request, http.MethodGet, artifactContentTestPath(), "")
		body = &artifactTrackingReadCloser{
			reader: &artifactReadSizeGuard{
				reader:  bytes.NewReader(payload),
				maximum: artifactStreamChunkBytes,
			},
		}
		return artifactSuccessResponse(http.StatusOK, body, int64(len(payload)), digest, ""), nil
	})
	destination := newArtifactDigestWriter()

	if err := client.DownloadArtifact(
		t.Context(),
		WorkspaceID(artifactClientTestWorkspace),
		ArtifactID(artifactClientTestID),
		"",
		destination,
	); err != nil {
		t.Fatalf("download full artifact: %v", err)
	}
	if destination.bytes != int64(len(payload)) || destination.writes < 2 ||
		destination.maximumWrite > artifactStreamChunkBytes || destination.sum() != digest {
		t.Fatalf(
			"stream observation = bytes %d writes %d maximum %d digest %s",
			destination.bytes,
			destination.writes,
			destination.maximumWrite,
			destination.sum(),
		)
	}
	if body == nil || !body.closed {
		t.Fatal("full download response body was not closed")
	}
	if doer.artifactCalls != 1 {
		t.Fatalf("full download calls = %d, want 1", doer.artifactCalls)
	}
}

func TestDownloadArtifactValidatesClosedFromAndSuffixRanges(t *testing.T) {
	object := []byte("0123456789")
	sum := sha256.Sum256(object)
	digest := hex.EncodeToString(sum[:])
	tests := []struct {
		name         string
		requestRange string
		contentRange string
		body         []byte
	}{
		{name: "closed", requestRange: "bytes=2-20", contentRange: "bytes 2-9/10", body: object[2:]},
		{name: "from", requestRange: "bytes=4-", contentRange: "bytes 4-9/10", body: object[4:]},
		{name: "suffix", requestRange: "bytes=-3", contentRange: "bytes 7-9/10", body: object[7:]},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var body *artifactTrackingReadCloser
			client, doer := newScriptedArtifactClient(t, func(request *http.Request) (*http.Response, error) {
				requireArtifactRequest(t, request, http.MethodGet, artifactContentTestPath(), test.requestRange)
				body = &artifactTrackingReadCloser{reader: bytes.NewReader(test.body)}
				return artifactSuccessResponse(
					http.StatusPartialContent,
					body,
					int64(len(test.body)),
					digest,
					test.contentRange,
				), nil
			})
			var destination bytes.Buffer

			if err := client.DownloadArtifact(
				t.Context(),
				WorkspaceID(artifactClientTestWorkspace),
				ArtifactID(artifactClientTestID),
				test.requestRange,
				&destination,
			); err != nil {
				t.Fatalf("download %s range: %v", test.name, err)
			}
			if !bytes.Equal(destination.Bytes(), test.body) {
				t.Fatalf("range bytes = %q, want %q", destination.Bytes(), test.body)
			}
			if body == nil || !body.closed {
				t.Fatal("range response body was not closed")
			}
			if doer.artifactCalls != 1 {
				t.Fatalf("range download calls = %d, want 1", doer.artifactCalls)
			}
		})
	}
}

func TestDownloadArtifactRejectsSilentRangeMismatch(t *testing.T) {
	object := []byte("0123456789")
	sum := sha256.Sum256(object)
	digest := hex.EncodeToString(sum[:])
	body := &artifactTrackingReadCloser{reader: bytes.NewReader(object[3:6])}
	client, _ := newScriptedArtifactClient(t, func(request *http.Request) (*http.Response, error) {
		requireArtifactRequest(t, request, http.MethodGet, artifactContentTestPath(), "bytes=2-4")
		return artifactSuccessResponse(http.StatusPartialContent, body, 3, digest, "bytes 3-5/10"), nil
	})
	var destination bytes.Buffer

	err := client.DownloadArtifact(
		t.Context(),
		WorkspaceID(artifactClientTestWorkspace),
		ArtifactID(artifactClientTestID),
		"bytes=2-4",
		&destination,
	)
	if !errors.Is(err, ErrMalformedArtifactResponse) {
		t.Fatalf("mismatched range error = %v, want malformed artifact response", err)
	}
	if destination.Len() != 0 {
		t.Fatalf("mismatched range wrote %d bytes", destination.Len())
	}
	if !body.closed {
		t.Fatal("mismatched range response body was not closed")
	}
}

func TestDownloadArtifactDetectsLengthAndDigestMismatch(t *testing.T) {
	tests := []struct {
		name           string
		body           []byte
		declaredLength int64
		digest         string
		want           error
	}{
		{
			name:           "truncated",
			body:           []byte("short"),
			declaredLength: 6,
			digest:         strings.Repeat("1", sha256.Size*2),
			want:           ErrArtifactLengthMismatch,
		},
		{
			name:           "whole object digest mismatch",
			body:           []byte("bytes"),
			declaredLength: 5,
			digest:         strings.Repeat("0", sha256.Size*2),
			want:           ErrArtifactIntegrity,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := &artifactTrackingReadCloser{reader: bytes.NewReader(test.body)}
			client, doer := newScriptedArtifactClient(t, func(request *http.Request) (*http.Response, error) {
				requireArtifactRequest(t, request, http.MethodGet, artifactContentTestPath(), "")
				return artifactSuccessResponse(http.StatusOK, body, test.declaredLength, test.digest, ""), nil
			})
			var destination bytes.Buffer

			err := client.DownloadArtifact(
				t.Context(),
				WorkspaceID(artifactClientTestWorkspace),
				ArtifactID(artifactClientTestID),
				"",
				&destination,
			)
			if !errors.Is(err, test.want) {
				t.Fatalf("download error = %v, want %v", err, test.want)
			}
			if !body.closed {
				t.Fatal("failed download response body was not closed")
			}
			if doer.artifactCalls != 1 {
				t.Fatalf("failed download calls = %d, want 1", doer.artifactCalls)
			}
		})
	}
}

func TestDownloadArtifactReturnsTypedRangeRefusal(t *testing.T) {
	problemDocument, err := json.Marshal(Problem{
		Type:            "https://pm.invalid/problems/artifact-range-not-satisfiable",
		Title:           "Artifact range not satisfiable",
		Status:          http.StatusRequestedRangeNotSatisfiable,
		Detail:          "The requested byte range is not satisfiable.",
		Code:            "artifact_range_not_satisfiable",
		IncidentId:      "incident-artifact-range",
		SafeRetryAction: "correct_input",
	})
	if err != nil {
		t.Fatalf("marshal range problem: %v", err)
	}
	body := &artifactTrackingReadCloser{reader: bytes.NewReader(problemDocument)}
	client, doer := newScriptedArtifactClient(t, func(request *http.Request) (*http.Response, error) {
		requireArtifactRequest(t, request, http.MethodGet, artifactContentTestPath(), "bytes=99-100")
		header := make(http.Header)
		header.Set("Accept-Ranges", "bytes")
		header.Set("Cache-Control", "no-store")
		header.Set("Content-Length", strconv.Itoa(len(problemDocument)))
		header.Set("Content-Range", "bytes */10")
		header.Set("Content-Type", "application/problem+json")
		return &http.Response{
			StatusCode:    http.StatusRequestedRangeNotSatisfiable,
			Header:        header,
			Body:          body,
			ContentLength: int64(len(problemDocument)),
		}, nil
	})
	var destination bytes.Buffer

	err = client.DownloadArtifact(
		t.Context(),
		WorkspaceID(artifactClientTestWorkspace),
		ArtifactID(artifactClientTestID),
		"bytes=99-100",
		&destination,
	)
	var problem *ProblemError
	if !errors.As(err, &problem) || problem.Decoded.Value.Code != "artifact_range_not_satisfiable" {
		t.Fatalf("range refusal = %v, want typed artifact_range_not_satisfiable", err)
	}
	if destination.Len() != 0 || !body.closed || doer.artifactCalls != 1 {
		t.Fatalf(
			"range refusal side effects = bytes %d closed %t calls %d",
			destination.Len(),
			body.closed,
			doer.artifactCalls,
		)
	}
}

func TestDownloadArtifactPreservesCancellationAndWriteFailureWithoutRetry(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		body := &artifactTrackingReadCloser{reader: artifactCancelReader{cancel: cancel}}
		client, doer := newScriptedArtifactClient(t, func(request *http.Request) (*http.Response, error) {
			requireArtifactRequest(t, request, http.MethodGet, artifactContentTestPath(), "")
			return artifactSuccessResponse(
				http.StatusOK,
				body,
				1,
				strings.Repeat("0", sha256.Size*2),
				"",
			), nil
		})

		err := client.DownloadArtifact(
			ctx,
			WorkspaceID(artifactClientTestWorkspace),
			ArtifactID(artifactClientTestID),
			"",
			io.Discard,
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v, want context.Canceled", err)
		}
		if !body.closed || doer.artifactCalls != 1 {
			t.Fatalf("cancellation closed = %t calls = %d", body.closed, doer.artifactCalls)
		}
	})

	t.Run("write failure", func(t *testing.T) {
		writeFailure := errors.New("destination write failed")
		payload := []byte("write")
		sum := sha256.Sum256(payload)
		body := &artifactTrackingReadCloser{reader: bytes.NewReader(payload)}
		client, doer := newScriptedArtifactClient(t, func(request *http.Request) (*http.Response, error) {
			requireArtifactRequest(t, request, http.MethodGet, artifactContentTestPath(), "")
			return artifactSuccessResponse(
				http.StatusOK,
				body,
				int64(len(payload)),
				hex.EncodeToString(sum[:]),
				"",
			), nil
		})

		err := client.DownloadArtifact(
			t.Context(),
			WorkspaceID(artifactClientTestWorkspace),
			ArtifactID(artifactClientTestID),
			"",
			artifactFailWriter{err: writeFailure},
		)
		if !errors.Is(err, writeFailure) {
			t.Fatalf("write error = %v, want original failure", err)
		}
		if !body.closed || doer.artifactCalls != 1 {
			t.Fatalf("write failure closed = %t calls = %d", body.closed, doer.artifactCalls)
		}
	})
}

type artifactTestDoer struct {
	handler       func(*http.Request) (*http.Response, error)
	artifactCalls int
}

func (doer *artifactTestDoer) Do(request *http.Request) (*http.Response, error) {
	if request.URL.Path == "/v1/info" {
		document, err := json.Marshal(ServerInfo{
			ProtocolMajor:    ProtocolMajor,
			ProtocolVersion:  ServerInfoProtocolVersion(WireVersion),
			InstanceId:       artifactClientTestInstance,
			Features:         []string{"exact-values", "wire-problem-v1"},
			RequiredFeatures: []string{},
		})
		if err != nil {
			return nil, err
		}
		header := make(http.Header)
		header.Set("Content-Length", strconv.Itoa(len(document)))
		header.Set("Content-Type", "application/json")
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        header,
			Body:          io.NopCloser(bytes.NewReader(document)),
			ContentLength: int64(len(document)),
		}, nil
	}
	doer.artifactCalls++
	return doer.handler(request)
}

func newScriptedArtifactClient(
	t *testing.T,
	handler func(*http.Request) (*http.Response, error),
) (*QualifiedClient, *artifactTestDoer) {
	t.Helper()
	doer := &artifactTestDoer{handler: handler}
	profile := ServerProfile{
		Name:          "artifact-client",
		ServerURL:     "https://pm.example.invalid",
		InstanceID:    artifactClientTestInstance,
		SessionHandle: "session-artifact-client",
	}
	client, err := NewQualifiedClient(t.Context(), profile, QualifiedClientOptions{
		HTTPClient:     doer,
		RequestTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("qualify artifact client: %v", err)
	}
	store := NewMemorySessionStore()
	key, err := NewSessionKey(profile.InstanceID, profile.SessionHandle)
	if err != nil {
		t.Fatalf("artifact session key: %v", err)
	}
	if err := store.Store(t.Context(), key, PMDeviceSession{
		ExpiresAt:           time.Now().Add(time.Hour),
		PmSessionCredential: artifactClientTestCredential,
		Principal:           Principal{Kind: PrincipalKindUser, PrincipalId: "ada-artifact-client"},
		SessionId:           "session-artifact-client",
	}); err != nil {
		t.Fatalf("store artifact session: %v", err)
	}
	client, err = client.WithSession(t.Context(), store)
	if err != nil {
		t.Fatalf("authenticate artifact client: %v", err)
	}
	return client, doer
}

func requireArtifactRequest(t *testing.T, request *http.Request, method, path, rangeHeader string) {
	t.Helper()
	if request.Method != method || request.URL.Path != path {
		t.Fatalf("artifact route = %s %s, want %s %s", request.Method, request.URL.Path, method, path)
	}
	if request.Header.Get("Authorization") != "Bearer "+artifactClientTestCredential {
		t.Fatalf("artifact authorization = %q", request.Header.Get("Authorization"))
	}
	if request.Header.Get("Range") != rangeHeader {
		t.Fatalf("artifact range = %q, want %q", request.Header.Get("Range"), rangeHeader)
	}
	if request.Body != nil || request.ContentLength != 0 {
		t.Fatalf("artifact request unexpectedly carried a body of length %d", request.ContentLength)
	}
}

func artifactContentTestPath() string {
	return "/v1/workspaces/" + artifactClientTestWorkspace + "/artifacts/" + artifactClientTestID + "/content"
}

func artifactSuccessResponse(
	status int,
	body io.ReadCloser,
	length int64,
	digest string,
	contentRange string,
) *http.Response {
	header := make(http.Header)
	header.Set("Accept-Ranges", "bytes")
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Disposition", "attachment")
	header.Set("Content-Length", strconv.FormatInt(length, 10))
	header.Set("Content-Type", "application/octet-stream")
	header.Set("ETag", artifactETag(digest))
	if contentRange != "" {
		header.Set("Content-Range", contentRange)
	}
	return &http.Response{
		StatusCode:    status,
		Header:        header,
		Body:          body,
		ContentLength: length,
	}
}

type artifactTrackingReadCloser struct {
	reader   io.Reader
	closed   bool
	closeErr error
}

func (reader *artifactTrackingReadCloser) Read(destination []byte) (int, error) {
	return reader.reader.Read(destination)
}

func (reader *artifactTrackingReadCloser) Close() error {
	reader.closed = true
	return reader.closeErr
}

type artifactReadSizeGuard struct {
	reader  io.Reader
	maximum int
}

func (reader *artifactReadSizeGuard) Read(destination []byte) (int, error) {
	if len(destination) > reader.maximum {
		return 0, errors.New("artifact client requested an unbounded read")
	}
	return reader.reader.Read(destination)
}

type artifactDigestWriter struct {
	digest       hash.Hash
	bytes        int64
	writes       int
	maximumWrite int
}

func newArtifactDigestWriter() *artifactDigestWriter {
	return &artifactDigestWriter{digest: sha256.New()}
}

func (writer *artifactDigestWriter) Write(document []byte) (int, error) {
	writer.writes++
	writer.bytes += int64(len(document))
	writer.maximumWrite = max(writer.maximumWrite, len(document))
	return writer.digest.Write(document)
}

func (writer *artifactDigestWriter) sum() string {
	return hex.EncodeToString(writer.digest.Sum(nil))
}

type artifactCancelReader struct {
	cancel context.CancelFunc
}

func (reader artifactCancelReader) Read([]byte) (int, error) {
	reader.cancel()
	return 0, context.Canceled
}

type artifactFailWriter struct {
	err error
}

func (writer artifactFailWriter) Write([]byte) (int, error) {
	return 0, writer.err
}

func FuzzArtifactRangesStayWithinObject(f *testing.F) {
	f.Add("bytes=0-0", int64(1), "bytes 0-0/1")
	f.Add("bytes=0-9223372036854775807", int64(9223372036854775807), "bytes 0-9223372036854775806/9223372036854775807")
	f.Add("bytes=-9223372036854775807", int64(0), "bytes 0-9223372036854775807/9223372036854775807")
	f.Add("bytes=9223372036854775808-", int64(10), "bytes -1-9/10")
	f.Add("bytes=１-９", int64(-1), "bytes 0-9/１０")
	f.Add("bytes=0-1,4-5", int64(6), "bytes 4-3/6")
	f.Fuzz(func(t *testing.T, request string, objectLength int64, response string) {
		if len(request) > 1024 || len(response) > 1024 {
			return
		}
		selection, valid := parseArtifactRangeRequest(request)
		if valid {
			start, end, resolved := resolveArtifactRangeRequest(selection, objectLength)
			if resolved && (objectLength <= 0 || start < 0 || end < start || end >= objectLength || end-start+1 <= 0) {
				t.Fatal("accepted request range escapes the object or overflows its byte count")
			}
		}
		start, end, total, valid := parseArtifactContentRange(response)
		if valid && (total <= 0 || start < 0 || end < start || end >= total || end-start+1 <= 0) {
			t.Fatal("accepted response range escapes the object or overflows its byte count")
		}
	})
}
