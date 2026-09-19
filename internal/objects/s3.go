package objects

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

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
			Transport:     transport,
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
	operationContext, release, err := backend.begin(ctx)
	if err != nil {
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
	if err != nil {
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
	if err := backend.verifyRemote(operationContext, ref, io.Discard); err != nil {
		return VerifiedObject{}, err
	}
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
