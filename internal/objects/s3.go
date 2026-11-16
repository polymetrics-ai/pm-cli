package objects

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// Credentials come only from the operator's already-approved secret ingress.
// There is no SDK environment, profile, instance-role, or endpoint discovery.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

func (Credentials) String() string               { return "shared object credentials [REDACTED]" }
func (credentials Credentials) GoString() string { return credentials.String() }

type S3Configuration struct {
	Endpoint         string
	Bucket           string
	Region           string
	RootCAs          *x509.CertPool
	LoadCredentials  func(context.Context) (Credentials, error)
	ScratchDirectory string
	Bounds           Bounds
}

// S3 retains one explicit endpoint, bucket, trust root, and private scratch
// directory. Scratch holds only unlinked verification files, never durable
// artifacts or a local fallback when the shared backend is unavailable.
type S3 struct {
	client    *s3.Client
	transport *http.Transport
	bucket    string
	bounds    Bounds
	scratch   *os.Root
	inFlight  chan struct{}
}

func NewS3(configuration S3Configuration) (*S3, error) {
	if err := configuration.Bounds.validate(); err != nil {
		return nil, err
	}
	endpoint, err := url.Parse(configuration.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil ||
		(endpoint.Path != "" && endpoint.Path != "/") || endpoint.RawPath != "" ||
		endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.Opaque != "" ||
		configuration.RootCAs == nil || configuration.LoadCredentials == nil || configuration.ScratchDirectory == "" ||
		!validBucket(configuration.Bucket) || !validRegion(configuration.Region) {
		return nil, ErrInvalidObject
	}
	scratch, err := os.OpenRoot(configuration.ScratchDirectory)
	if err != nil {
		return nil, ErrObjectUnavailable
	}
	info, err := scratch.Stat(".")
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		_ = scratch.Close()
		return nil, ErrInvalidObject
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		_ = scratch.Close()
		return nil, ErrInvalidObject
	}
	transport := defaultTransport.Clone()
	transport.Proxy = nil
	transport.DisableCompression = true
	transport.MaxConnsPerHost = configuration.Bounds.MaxInFlight
	transport.MaxIdleConns = configuration.Bounds.MaxInFlight
	transport.MaxIdleConnsPerHost = configuration.Bounds.MaxInFlight
	transport.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    configuration.RootCAs.Clone(),
	}
	loader := configuration.LoadCredentials
	client := s3.New(s3.Options{
		BaseEndpoint:                   aws.String(endpoint.String()),
		Region:                         configuration.Region,
		UsePathStyle:                   true,
		Retryer:                        aws.NopRetryer{},
		DisableMultiRegionAccessPoints: true,
		DisableS3ExpressSessionAuth:    aws.Bool(true),
		Credentials: aws.CredentialsProviderFunc(func(ctx context.Context) (aws.Credentials, error) {
			loaded, err := loader(ctx)
			if err != nil || !validCredential(loaded.AccessKeyID, false) ||
				!validCredential(loaded.SecretAccessKey, false) || !validCredential(loaded.SessionToken, true) {
				return aws.Credentials{}, ErrObjectUnavailable
			}
			return aws.Credentials{
				AccessKeyID:     loaded.AccessKeyID,
				SecretAccessKey: loaded.SecretAccessKey,
				SessionToken:    loaded.SessionToken,
				Source:          "pm-operator-artifact-reference",
			}, nil
		}),
		HTTPClient: &http.Client{
			// DIAGNOSTIC ONLY (issue #279 canary). Never merge.
			Transport:     &diagnostic279Transport{inner: transport},
			Timeout:       configuration.Bounds.RequestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	})
	return &S3{
		client:    client,
		transport: transport,
		bucket:    configuration.Bucket,
		bounds:    configuration.Bounds,
		scratch:   scratch,
		inFlight:  make(chan struct{}, configuration.Bounds.MaxInFlight),
	}, nil
}

// Close follows cancellation and closure of the owner's active operations.
func (backend *S3) Close() error {
	if backend == nil {
		return nil
	}
	backend.transport.CloseIdleConnections()
	return backend.scratch.Close()
}

// Upload consumes exactly one attempt. The caller must have committed the
// matching scoped byte reservation before calling it. Success means independently
// verified remote bytes, not a published artifact or a committed run result.
func (backend *S3) Upload(ctx context.Context, attempt Attempt) (VerifiedObject, error) {
	if backend == nil || attempt.state == nil {
		return VerifiedObject{}, ErrInvalidObject
	}
	if !attempt.state.consumed.CompareAndSwap(false, true) {
		return VerifiedObject{}, ErrAttemptUsed
	}
	state := attempt.state
	defer func() {
		clear(state.payload)
		state.payload = nil
	}()
	descriptor := state.descriptor
	if descriptor.ByteLength > backend.bounds.MaxObjectBytes {
		return VerifiedObject{}, ErrObjectLimit
	}
	diagnosticStart := time.Now()
	operationContext, release, err := backend.begin(ctx)
	diagnosticBegin := time.Since(diagnosticStart)
	if err != nil {
		diagnostic279Upload(diagnosticStart, diagnosticBegin, 0, 0, "begin")
		return VerifiedObject{}, err
	}
	defer release()
	digest, err := hex.DecodeString(descriptor.SHA256)
	if err != nil {
		return VerifiedObject{}, ErrInvalidObject
	}
	response, err := backend.client.PutObject(operationContext, &s3.PutObjectInput{
		Bucket:         aws.String(backend.bucket),
		Key:            aws.String(descriptor.ObjectAttemptKey),
		Body:           bytes.NewReader(state.payload),
		ContentLength:  aws.Int64(descriptor.ByteLength),
		ContentType:    aws.String(descriptor.MediaType),
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(digest)),
		IfNoneMatch:    aws.String("*"),
	})
	diagnosticPut := time.Since(diagnosticStart) - diagnosticBegin
	if err != nil {
		diagnostic279Upload(diagnosticStart, diagnosticBegin, diagnosticPut, 0, "put")
		var apiError smithy.APIError
		if errors.As(err, &apiError) && (apiError.ErrorCode() == "PreconditionFailed" || apiError.ErrorCode() == "ConditionalRequestConflict") {
			return VerifiedObject{}, ErrObjectConflict
		}
		return VerifiedObject{}, unavailable(operationContext)
	}
	generation, err := backendGeneration(aws.ToString(response.VersionId), aws.ToString(response.ETag))
	if err != nil {
		return VerifiedObject{}, err
	}
	ref := contract.ObjectRef{
		OrgID:             descriptor.Scope.OrgID,
		WorkspaceID:       descriptor.Scope.WorkspaceID,
		ArtifactID:        descriptor.ArtifactID,
		ObjectAttemptKey:  descriptor.ObjectAttemptKey,
		SHA256:            descriptor.SHA256,
		ByteLength:        descriptor.ByteLength,
		MediaType:         descriptor.MediaType,
		BackendGeneration: generation,
	}
	if err := ref.Validate(); err != nil {
		return VerifiedObject{}, ErrObjectIntegrity
	}
	diagnosticVerifyStart := time.Now()
	if err := backend.verifyRemote(operationContext, ref, io.Discard); err != nil {
		diagnostic279Upload(diagnosticStart, diagnosticBegin, diagnosticPut, time.Since(diagnosticVerifyStart), "verify")
		return VerifiedObject{}, err
	}
	diagnostic279Upload(diagnosticStart, diagnosticBegin, diagnosticPut, time.Since(diagnosticVerifyStart), "none")
	return VerifiedObject{ref: ref}, nil
}

// Verify observes the registered object's actual complete bytes. It performs no
// listing, registration, publication, or authorization on behalf of the caller.
func (backend *S3) Verify(ctx context.Context, ref contract.ObjectRef) error {
	if err := backend.validateReference(ref); err != nil {
		return err
	}
	operationContext, release, err := backend.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	return backend.verifyRemote(operationContext, ref, io.Discard)
}

// OpenVerified returns only bytes from one fully verified generation. Even a
// range is verified against the complete registered SHA256 before any content
// becomes readable; corrupt prefixes are never streamed to the consumer.
// The caller owns Close. Cancellation also closes the scratch file and releases
// the bounded backend slot.
func (backend *S3) OpenVerified(ctx context.Context, ref contract.ObjectRef, selection ByteRange) (io.ReadCloser, error) {
	if err := backend.validateReference(ref); err != nil {
		return nil, err
	}
	if err := selection.validate(ref.ByteLength); err != nil {
		return nil, err
	}
	operationContext, release, err := backend.begin(ctx)
	if err != nil {
		return nil, err
	}
	file, err := backend.newScratchFile()
	if err != nil {
		release()
		return nil, err
	}
	if err := backend.verifyRemote(operationContext, ref, file); err != nil {
		_ = file.Close()
		release()
		return nil, err
	}
	return newVerifiedReader(operationContext, file, selection, release), nil
}

func (backend *S3) validateReference(ref contract.ObjectRef) error {
	if backend == nil {
		return ErrInvalidObject
	}
	if err := ref.Validate(); err != nil {
		return ErrInvalidObject
	}
	if ref.ByteLength > backend.bounds.MaxObjectBytes {
		return ErrObjectLimit
	}
	if err := validateObjectKey(ref); err != nil {
		return err
	}
	_, _, err := generationCondition(ref.BackendGeneration)
	return err
}

func (backend *S3) begin(ctx context.Context) (context.Context, func(), error) {
	if ctx == nil {
		return nil, nil, ErrInvalidObject
	}
	operationContext, cancel := context.WithTimeout(ctx, backend.bounds.RequestTimeout)
	select {
	case backend.inFlight <- struct{}{}:
		if operationContext.Err() != nil {
			<-backend.inFlight
			cancel()
			return nil, nil, unavailable(operationContext)
		}
		return operationContext, func() { <-backend.inFlight; cancel() }, nil
	case <-operationContext.Done():
		cancel()
		return nil, nil, unavailable(operationContext)
	}
}

func backendGeneration(version, etag string) (string, error) {
	if version != "" && version != "null" {
		if !validArtifactID(version) || len(version) > 504 {
			return "", ErrObjectIntegrity
		}
		return "version:" + version, nil
	}
	if len(etag) < 2 || etag[0] != '"' || etag[len(etag)-1] != '"' || !validArtifactID(etag) || len(etag) > 507 {
		return "", ErrObjectIntegrity
	}
	return "etag:" + etag, nil
}

func generationCondition(generation string) (version, etag *string, err error) {
	kind, value, present := strings.Cut(generation, ":")
	if !present {
		return nil, nil, ErrObjectIntegrity
	}
	switch kind {
	case "version":
		if value == "" || value == "null" || !validArtifactID(value) || len(value) > 504 {
			return nil, nil, ErrObjectIntegrity
		}
		return aws.String(value), nil, nil
	case "etag":
		if _, err := backendGeneration("", value); err != nil {
			return nil, nil, err
		}
		return nil, aws.String(value), nil
	default:
		return nil, nil, ErrObjectIntegrity
	}
}

func unavailable(ctx context.Context) error {
	if ctx != nil && ctx.Err() != nil {
		return errors.Join(ErrObjectUnavailable, ctx.Err())
	}
	return ErrObjectUnavailable
}

func validBucket(value string) bool {
	if len(value) < 3 || len(value) > 63 || strings.Contains(value, "..") || strings.Contains(value, ".-") || strings.Contains(value, "-.") {
		return false
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			continue
		}
		if (character == '-' || character == '.') && index > 0 && index < len(value)-1 {
			continue
		}
		return false
	}
	return true
}

func validRegion(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func validCredential(value string, allowEmpty bool) bool {
	if value == "" {
		return allowEmpty
	}
	return len(value) <= 64<<10 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

// DIAGNOSTIC ONLY (issue #279 canary). Never merge.
//
// diagnostic279Transport writes one line to stderr for each object-store HTTP
// request: its order in the process, the time of each connection phase, the
// time to the first response byte and the time until the response body
// closes. It writes no URL, host name, header or body.
type diagnostic279Transport struct {
	inner http.RoundTripper
}

var diagnostic279Sequence atomic.Int64

type diagnostic279Trace struct {
	mu                        sync.Mutex
	start                     time.Time
	dnsStart, dnsDone         time.Time
	connectStart, connectDone time.Time
	tlsStart, tlsDone         time.Time
	gotConn, wrote, firstByte time.Time
	reused                    bool
}

func (trace *diagnostic279Trace) mark(target *time.Time) {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if target.IsZero() {
		*target = time.Now()
	}
}

func diagnostic279Span(from, to time.Time) int64 {
	if from.IsZero() || to.IsZero() {
		return -1
	}
	return to.Sub(from).Microseconds()
}

func (transport *diagnostic279Transport) RoundTrip(request *http.Request) (*http.Response, error) {
	sequence := diagnostic279Sequence.Add(1)
	trace := &diagnostic279Trace{start: time.Now()}
	clientTrace := &httptrace.ClientTrace{
		DNSStart:             func(httptrace.DNSStartInfo) { trace.mark(&trace.dnsStart) },
		DNSDone:              func(httptrace.DNSDoneInfo) { trace.mark(&trace.dnsDone) },
		ConnectStart:         func(string, string) { trace.mark(&trace.connectStart) },
		ConnectDone:          func(string, string, error) { trace.mark(&trace.connectDone) },
		TLSHandshakeStart:    func() { trace.mark(&trace.tlsStart) },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { trace.mark(&trace.tlsDone) },
		WroteRequest:         func(httptrace.WroteRequestInfo) { trace.mark(&trace.wrote) },
		GotFirstResponseByte: func() { trace.mark(&trace.firstByte) },
		GotConn: func(info httptrace.GotConnInfo) {
			trace.mark(&trace.gotConn)
			trace.mu.Lock()
			trace.reused = info.Reused
			trace.mu.Unlock()
		},
	}
	budget := int64(-1)
	if deadline, ok := request.Context().Deadline(); ok {
		budget = deadline.Sub(trace.start).Microseconds()
	}
	response, err := transport.inner.RoundTrip(request.WithContext(httptrace.WithClientTrace(request.Context(), clientTrace)))
	headers := time.Now()
	method := request.Method
	if err != nil {
		trace.write(sequence, method, 0, budget, headers, time.Time{}, true)
		return nil, err
	}
	var once sync.Once
	response.Body = &diagnostic279Body{ReadCloser: response.Body, closed: func() {
		once.Do(func() { trace.write(sequence, method, response.StatusCode, budget, headers, time.Now(), false) })
	}}
	if method == http.MethodPut && response.StatusCode >= 200 && response.StatusCode < 300 {
		diagnostic310AfterFirstWrite()
	}
	return response, nil
}

// DIAGNOSTIC ONLY (issue #310 canary). Never merge.
//
// diagnostic310AfterFirstWrite starts the volume probe of the artifact bucket
// once, after the first object write that succeeds in the whole run. A marker
// file in the module's .cache directory makes later test binaries of the same
// run skip it. The probe runs in the background, so the write does not wait
// for it. It prints counts and its UTC time only.
var diagnostic310Once sync.Once

func diagnostic310AfterFirstWrite() {
	diagnostic310Once.Do(func() {
		root, ok := diagnostic310ModuleRoot()
		if !ok {
			return
		}
		marker, err := os.OpenFile(filepath.Join(root, ".cache", "pm310-first-write"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		_ = marker.Close()
		fmt.Fprintf(os.Stderr, "pm310-volumes: moment=first_write_answered utc=%s\n", time.Now().UTC().Format(time.RFC3339Nano))
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			output, _ := exec.CommandContext(ctx, "python3", "-I", "-B",
				filepath.Join(root, "scripts", "ci", "canary-seaweed-volumes.py"), "after_first_write").CombinedOutput()
			for _, line := range strings.Split(string(output), "\n") {
				if strings.HasPrefix(line, "pm310-volumes: ") {
					fmt.Fprintln(os.Stderr, line)
				}
			}
		}()
	})
}

// diagnostic310ModuleRoot is the nearest directory above the working
// directory that holds go.mod and the probe script.
func diagnostic310ModuleRoot() (string, bool) {
	directory, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(directory, "scripts", "ci", "canary-seaweed-volumes.py")); err == nil {
				return directory, true
			}
			return "", false
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", false
		}
		directory = parent
	}
}

func (trace *diagnostic279Trace) write(
	sequence int64, method string, status int, budget int64, headers, closed time.Time, failed bool,
) {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	fmt.Fprintf(os.Stderr,
		"pm279-object: start_utc=%s seq=%d method=%s status=%d failed=%t reused=%t budget_us=%d dns_us=%d connect_us=%d "+
			"tls_us=%d conn_us=%d write_us=%d first_byte_us=%d headers_us=%d closed_us=%d\n",
		trace.start.UTC().Format(time.RFC3339Nano), sequence, method, status, failed, trace.reused, budget,
		diagnostic279Span(trace.dnsStart, trace.dnsDone), diagnostic279Span(trace.connectStart, trace.connectDone),
		diagnostic279Span(trace.tlsStart, trace.tlsDone), diagnostic279Span(trace.start, trace.gotConn),
		diagnostic279Span(trace.gotConn, trace.wrote), diagnostic279Span(trace.wrote, trace.firstByte),
		diagnostic279Span(trace.start, headers), diagnostic279Span(trace.start, closed),
	)
}

type diagnostic279Body struct {
	io.ReadCloser
	closed func()
}

func (body *diagnostic279Body) Read(buffer []byte) (int, error) {
	count, err := body.ReadCloser.Read(buffer)
	if err == io.EOF {
		body.closed()
	}
	return count, err
}

func (body *diagnostic279Body) Close() error {
	err := body.ReadCloser.Close()
	body.closed()
	return err
}

func diagnostic279Upload(start time.Time, begin, put, verify time.Duration, failed string) {
	fmt.Fprintf(os.Stderr, "pm279-upload: start_utc=%s begin_us=%d put_us=%d verify_us=%d failed=%s\n",
		start.UTC().Format(time.RFC3339Nano), begin.Microseconds(), put.Microseconds(), verify.Microseconds(), failed)
}
