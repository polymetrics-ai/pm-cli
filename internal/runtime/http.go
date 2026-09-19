package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

var (
	ErrUnsupportedHTTPTransport = errors.New("unsupported HTTP transport behavior")
	ErrHTTPTransport            = errors.New("HTTP transport failed")
	ErrRequestTooLarge          = errors.New("encoded HTTP request exceeds its server-owned bound")
	ErrResponseTooLarge         = errors.New("HTTP response exceeds its server-owned bound")
	// ErrClientIdentityRequired reports that a provider request cannot be
	// identified: the operation declares no user-agent input of its own and the
	// process has no configured client identity. The request is refused instead
	// of being sent with an empty value.
	ErrClientIdentityRequired = errors.New("provider client identity is required")
)

func validateHTTPSourceFacts(
	operation contract.Operation,
	policy EgressPolicy,
) (*contract.OperationHTTP, *url.URL, error) {
	if operation.Transport.Kind != contract.TransportKindHTTP {
		return nil, nil, ErrUnsupportedHTTPTransport
	}
	if err := operation.Transport.Validate(); err != nil {
		return nil, nil, fmt.Errorf("%w: invalid HTTP transport contract", ErrUnsupportedHTTPTransport)
	}
	if len(operation.Responses) == 0 {
		return nil, nil, fmt.Errorf("%w: response contract is absent", ErrUnsupportedHTTPTransport)
	}
	for _, response := range operation.Responses {
		if err := response.Validate(contract.TransportKindHTTP); err != nil {
			return nil, nil, fmt.Errorf("%w: invalid response contract", ErrUnsupportedHTTPTransport)
		}
		if response.Schema != nil && schemaContainsBinaryRef(*response.Schema) {
			return nil, nil, ErrBinaryResponseBoundary
		}
		if !response.NoContent && !isJSONWireMediaType(response.MediaType) {
			return nil, nil, fmt.Errorf("%w: typed response media is not JSON", ErrUnsupportedHTTPTransport)
		}
	}
	if operation.Execution == nil || operation.Execution.HTTP == nil {
		return nil, nil, fmt.Errorf("%w: source HTTP facts are absent", ErrUnsupportedHTTPTransport)
	}
	if err := operation.Execution.Validate(operation); err != nil {
		return nil, nil, fmt.Errorf("%w: invalid operation execution facts", ErrUnsupportedHTTPTransport)
	}
	facts := operation.Execution.HTTP
	origin, err := validateDestination(facts.Origins, policy)
	if err != nil {
		return nil, nil, err
	}
	if facts.Authorization != nil {
		audience, audienceErr := canonicalOrigin(facts.Authorization.Audience)
		if audienceErr != nil || !sameOrigin(audience, origin) {
			return nil, nil, fmt.Errorf("%w: credential audience is not the admitted destination audience", ErrDestinationDenied)
		}
	}
	return facts, origin, nil
}

type ObjectReader interface {
	OpenObject(context.Context, contract.ObjectRef) (io.ReadCloser, error)
}

type HTTPDependencies struct {
	Admitter      PhysicalCallAdmitter
	Credentials   CredentialResolver
	Continuations ContinuationCodec
	Objects       ObjectReader
	Resolver      DNSResolver
	Dialer        ContextDialer
	TLSConfig     *tls.Config
	Now           func() time.Time
	// ClientIdentity is the bounded product identity this process sends as the
	// default provider user agent. An operation that declares its own
	// user-agent input keeps ownership of that field and needs no identity here;
	// every other operation requires one, so a process that cannot identify
	// itself refuses the request instead of sending an empty value.
	ClientIdentity ClientIdentity
}

type HTTPRequest struct {
	Operation    contract.Operation
	Egress       EgressPolicy
	Call         PhysicalCall
	Input        contract.PresentValue
	Controls     contract.ResultControls
	Limits       CallLimits
	Continuation contract.PresentValue
}

type WireObservation struct {
	Method             string
	EscapedPath        string
	RawQuery           string
	RequestBodyBytes   uint64
	EffectiveWireLimit *uint64
	GetBodyPresent     bool
	IdempotencyKeySent bool
}

type HTTPExecution struct {
	Response     DecodedResponse
	Result       *contract.ActionResult
	Retry        RetryDirective
	Cooldown     RetryCooldown
	Disposition  AttemptDisposition
	PhysicalSend bool
	// VerifiedLocalResult is true only when a previously authenticated
	// continuation tail produced this result without a provider round trip. It
	// is logical result evidence, never physical-send evidence.
	VerifiedLocalResult bool
	Wire                WireObservation
}

type AttemptError struct {
	Disposition AttemptDisposition
	Retry       RetryDirective
	Cooldown    RetryCooldown
	cause       error
}

func (e *AttemptError) Error() string {
	switch e.Disposition {
	case DispositionNotSent:
		return "HTTP attempt was not sent"
	case DispositionUnknown:
		return "HTTP attempt may have been accepted"
	default:
		return "HTTP response could not be accepted"
	}
}

func (e *AttemptError) Unwrap() error { return e.cause }

func (e *AttemptError) Is(target error) bool {
	return target == ErrOutcomeUnknown && e.Disposition == DispositionUnknown
}

// ExecuteHTTP performs at most one HTTP/1.1 RoundTrip. It never follows a
// redirect, sleeps, retries, synthesizes request replay state, or manufactures
// an idempotency key. Any later send needs a new durable attempt and a fresh
// call to PhysicalCallAdmitter.
func ExecuteHTTP(ctx context.Context, dependencies HTTPDependencies, request HTTPRequest) (HTTPExecution, error) {
	if ctx == nil {
		return HTTPExecution{}, context.Canceled
	}
	if err := request.Limits.validate(); err != nil {
		return HTTPExecution{}, err
	}
	if request.Limits.MaxRequestBytes >= uint64(^uint64(0)>>1) || request.Limits.MaxResponseBytes >= uint64(^uint64(0)>>1) {
		return HTTPExecution{}, ErrInvalidLimits
	}
	if err := request.Call.validate(request.Operation, request.Limits); err != nil {
		return HTTPExecution{}, err
	}
	now := time.Now
	if dependencies.Now != nil {
		now = dependencies.Now
	}
	current := now().UTC()
	if !current.Before(request.Call.Deadline) {
		return HTTPExecution{}, context.DeadlineExceeded
	}
	boundedCtx, cancel := context.WithDeadline(ctx, request.Call.Deadline)
	defer cancel()
	httpFacts, origin, err := validateHTTPSourceFacts(request.Operation, request.Egress)
	if err != nil {
		return HTTPExecution{}, err
	}
	binding := ContinuationBinding{
		Scope:             request.Call.Scope,
		ConnectionID:      request.Call.ConnectionID,
		OperationID:       request.Operation.OperationID,
		OperationRevision: request.Call.OperationRevision,
		RunID:             request.Call.RunID,
	}
	var activePagination *contract.OperationPagination
	var activeRetry *contract.OperationRetry
	if request.Operation.Execution != nil {
		activePagination = request.Operation.Execution.Pagination
		activeRetry = request.Operation.Execution.Retry
	}
	localPagination := activePagination == nil && (request.Limits.TotalRecordCap != nil ||
		request.Continuation.Presence() != "" && request.Continuation.Presence() != contract.PresenceAbsent)
	prepared, err := preparePagination(
		boundedCtx,
		request.Operation,
		activePagination,
		localPagination,
		request.Input,
		request.Continuation,
		dependencies.Continuations,
		binding,
	)
	if err != nil {
		return HTTPExecution{}, err
	}
	wireLimit, err := effectiveWireLimit(prepared.input, activePagination)
	if err != nil {
		return HTTPExecution{}, err
	}
	if prepared.state != nil && len(prepared.state.UnreadTail) != 0 {
		result, err := serveContinuationTail(
			boundedCtx,
			*prepared.state,
			request.Limits,
			request.Controls,
			dependencies.Continuations,
			binding,
		)
		if err != nil {
			return HTTPExecution{}, err
		}
		return HTTPExecution{
			Result:              &result,
			Disposition:         DispositionNotSent,
			PhysicalSend:        false,
			VerifiedLocalResult: true,
			Wire:                WireObservation{Method: request.Operation.Transport.Method, EffectiveWireLimit: cloneUint64(wireLimit)},
		}, nil
	}
	// The client identity is resolved before any credential resolution, request
	// construction, admission or provider I/O. A verified continuation tail
	// served above never reaches this point, so no identity is needed to answer a
	// local result without a provider round trip.
	clientIdentity, appliesClientIdentity, err := resolveClientIdentity(request.Operation, dependencies.ClientIdentity)
	if err != nil {
		return HTTPExecution{}, err
	}
	encoded, err := EncodeInputs(request.Operation, prepared.input)
	if err != nil {
		return HTTPExecution{}, err
	}
	if httpFacts.Authorization != nil {
		if err := validateAuthorizationChannel(&encoded, *httpFacts.Authorization); err != nil {
			return HTTPExecution{}, err
		}
	}
	if encoded.BodyReference != nil {
		if encoded.BodyReference.MediaType != encoded.MediaType {
			return HTTPExecution{}, fmt.Errorf("%w: binary reference media type differs from the request variant", ErrUnsupportedWireValue)
		}
		body, bodyErr := readObjectBody(boundedCtx, dependencies.Objects, request.Call, *encoded.BodyReference, request.Limits.MaxRequestBytes)
		if bodyErr != nil {
			return HTTPExecution{}, bodyErr
		}
		encoded.Body = body
		encoded.BodyReference = nil
	}
	if httpFacts.Authorization != nil {
		if dependencies.Credentials == nil {
			if httpFacts.Authorization.Required {
				return HTTPExecution{}, ErrCredentialRequired
			}
		} else {
			material, credentialErr := dependencies.Credentials.ResolveCredential(boundedCtx, request.Call)
			if credentialErr != nil {
				return HTTPExecution{}, ErrCredentialRequired
			}
			if material == nil {
				if httpFacts.Authorization.Required {
					return HTTPExecution{}, ErrCredentialRequired
				}
			} else {
				defer material.Destroy()
				credentialBinding := material.CredentialBinding()
				if err := credentialBinding.Validate(); err != nil ||
					credentialBinding.WorkspaceID != request.Call.Scope.WorkspaceID ||
					credentialBinding.ConnectionID != request.Call.ConnectionID ||
					credentialBinding.ProviderAccountWitness != request.Call.AccountWitness {
					return HTTPExecution{}, ErrCredentialRequired
				}
				if err := applyAuthorization(&encoded, *httpFacts.Authorization, material); err != nil {
					return HTTPExecution{}, err
				}
			}
		}
	}
	if appliesClientIdentity {
		if err := applyClientIdentity(&encoded, clientIdentity); err != nil {
			return HTTPExecution{}, err
		}
	}
	encoded.EncodedBytes, err = encodedInputSize(encoded)
	if err != nil {
		return HTTPExecution{}, err
	}
	if uint64(len(encoded.Body)) > request.Limits.MaxRequestBytes || encoded.EncodedBytes > request.Limits.MaxRequestBytes {
		return HTTPExecution{}, ErrRequestTooLarge
	}
	target, err := buildTargetURL(origin, encoded)
	if err != nil {
		return HTTPExecution{}, err
	}
	if !sameOriginURL(target, origin) {
		return HTTPExecution{}, ErrDestinationDenied
	}
	httpRequest, err := newOneShotRequest(boundedCtx, request.Operation.Transport.Method, target, encoded)
	if err != nil {
		return HTTPExecution{}, err
	}
	if !declaresHeader(request.Operation, "Idempotency-Key") && httpRequest.Header.Get("Idempotency-Key") != "" {
		return HTTPExecution{}, fmt.Errorf("%w: idempotency key is not source-declared", ErrInvalidHTTPInput)
	}
	if httpRequest.GetBody != nil {
		return HTTPExecution{}, fmt.Errorf("%w: replayable request body was synthesized", ErrUnsupportedHTTPTransport)
	}

	resolver := dependencies.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	dialer := dependencies.Dialer
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	tlsConfig := &tls.Config{}
	if dependencies.TLSConfig != nil {
		tlsConfig = dependencies.TLSConfig.Clone()
	}
	tlsConfig.ServerName = origin.Hostname()
	tlsConfig.NextProtos = []string{"http/1.1"}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            newPinnedDialContext(resolver, dialer, origin, request.Limits.MaxDNSAnswers),
		ForceAttemptHTTP2:      false,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		TLSClientConfig:        tlsConfig,
		TLSNextProto:           map[string]func(string, *tls.Conn) http.RoundTripper{},
		ResponseHeaderTimeout:  request.Call.Deadline.Sub(current),
		MaxResponseHeaderBytes: int64(request.Limits.MaxResponseBytes),
	}
	defer transport.CloseIdleConnections()
	var wroteHeaders atomic.Bool
	var wroteRequest atomic.Bool
	trace := &httptrace.ClientTrace{
		WroteHeaders: func() { wroteHeaders.Store(true) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				wroteRequest.Store(true)
			}
		},
	}
	httpRequest = httpRequest.WithContext(httptrace.WithClientTrace(httpRequest.Context(), trace))
	if err := admitPhysicalCall(boundedCtx, dependencies.Admitter, request.Call); err != nil {
		return HTTPExecution{}, err
	}
	response, roundTripErr := transport.RoundTrip(httpRequest)
	observation := WireObservation{
		Method:             request.Operation.Transport.Method,
		EscapedPath:        target.EscapedPath(),
		RawQuery:           safeObservedRawQuery(target, httpFacts.Authorization),
		RequestBodyBytes:   uint64(len(encoded.Body)),
		EffectiveWireLimit: cloneUint64(wireLimit),
		GetBodyPresent:     httpRequest.GetBody != nil,
		IdempotencyKeySent: httpRequest.Header.Get("Idempotency-Key") != "",
	}
	if roundTripErr != nil {
		disposition := DispositionNotSent
		if wroteHeaders.Load() || wroteRequest.Load() {
			disposition = DispositionUnknown
		}
		retry := retryForTransportFailure(activeRetry, disposition, request.Limits)
		cause := error(ErrHTTPTransport)
		if boundedCtx.Err() != nil {
			retry = RetryDirective{}
			cause = boundedCtx.Err()
		} else if !now().UTC().Before(request.Call.Deadline) {
			retry = RetryDirective{}
			cause = context.DeadlineExceeded
		}
		// net/http transport errors can embed the full URL. Never retain that
		// error because query-placed credentials are request-scoped secrets.
		return HTTPExecution{Retry: retry, Disposition: disposition, PhysicalSend: true, Wire: observation}, &AttemptError{
			Disposition: disposition,
			Retry:       retry,
			cause:       cause,
		}
	}
	defer response.Body.Close()
	if err := validateResponseTransport(response); err != nil {
		return HTTPExecution{Response: DecodedResponse{StatusCode: response.StatusCode}, Disposition: DispositionResponded, PhysicalSend: true, Wire: observation}, &AttemptError{
			Disposition: DispositionResponded,
			cause:       err,
		}
	}
	if err := validateRedirectTarget(response.StatusCode, response.Header, target); err != nil {
		return HTTPExecution{Response: DecodedResponse{StatusCode: response.StatusCode}, Disposition: DispositionResponded, PhysicalSend: true, Wire: observation}, &AttemptError{
			Disposition: DispositionResponded,
			cause:       err,
		}
	}
	observedAt := now().UTC()
	retry := retryForHTTPStatus(activeRetry, response.StatusCode, request.Limits, observedAt, request.Call.Deadline)
	cooldown := retryAfterCooldown(response.Header, observedAt, request.Limits.MaxRetryDelay)
	body, err := readResponseBody(response, request.Limits.MaxResponseBytes)
	if err != nil {
		return HTTPExecution{Response: DecodedResponse{StatusCode: response.StatusCode}, Retry: retry, Cooldown: cooldown, Disposition: DispositionResponded, PhysicalSend: true, Wire: observation}, &AttemptError{
			Disposition: DispositionResponded,
			Retry:       retry,
			Cooldown:    cooldown,
			cause:       err,
		}
	}
	decoded, err := DecodeResponse(request.Operation, response.StatusCode, response.Header, body)
	// Receiving the status line is independent of body validity. Keep that
	// bounded fact even when decoding produces no usable action result.
	decoded.StatusCode = response.StatusCode
	if err != nil {
		return HTTPExecution{Response: decoded, Retry: retry, Cooldown: cooldown, Disposition: DispositionResponded, PhysicalSend: true, Wire: observation}, &AttemptError{
			Disposition: DispositionResponded,
			Retry:       retry,
			Cooldown:    cooldown,
			cause:       err,
		}
	}
	if decoded.Result != nil && decoded.Result.Data.Presence() == contract.PresenceAbsent && response.ContentLength > 0 {
		return HTTPExecution{Response: decoded, Retry: retry, Cooldown: cooldown, Disposition: DispositionResponded, PhysicalSend: true, Wire: observation}, &AttemptError{
			Disposition: DispositionResponded,
			Retry:       retry,
			Cooldown:    cooldown,
			cause:       ErrUnexpectedHTTPResponse,
		}
	}
	if prepared.state != nil && prepared.state.MediaType != "" && decoded.Result != nil &&
		decoded.Result.Data.Presence() != contract.PresenceAbsent &&
		decoded.Result.MediaType != prepared.state.MediaType {
		return HTTPExecution{Response: decoded, Retry: retry, Cooldown: cooldown, Disposition: DispositionResponded, PhysicalSend: true, Wire: observation}, &AttemptError{
			Disposition: DispositionResponded,
			Retry:       retry,
			Cooldown:    cooldown,
			cause:       ErrInvalidProviderValue,
		}
	}
	execution := HTTPExecution{
		Response:     decoded,
		Retry:        retry,
		Cooldown:     cooldown,
		Disposition:  DispositionResponded,
		PhysicalSend: true,
		Wire:         observation,
	}
	if decoded.Result != nil {
		result, pageErr := applyPage(
			boundedCtx,
			request.Operation,
			decoded,
			activePagination,
			prepared.input,
			request.Limits,
			request.Controls,
			dependencies.Continuations,
			binding,
			pageHTTPContext{
				RequestURL:     target,
				ResponseHeader: response.Header,
				Authorization:  httpFacts.Authorization,
			},
		)
		if pageErr != nil {
			return execution, &AttemptError{Disposition: DispositionResponded, Retry: retry, Cooldown: cooldown, cause: pageErr}
		}
		execution.Result = &result
	}
	return execution, nil
}

func newOneShotRequest(ctx context.Context, method string, target *url.URL, encoded EncodedInputs) (*http.Request, error) {
	var body io.Reader
	if len(encoded.Body) != 0 {
		body = io.NopCloser(bytes.NewReader(encoded.Body))
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, fmt.Errorf("%w: request construction failed", ErrInvalidHTTPInput)
	}
	request.Header = encoded.Header.Clone()
	// The encoded request already carries the effective client identity: either
	// a caller-supplied source-declared user-agent field or the runtime's own
	// bounded product identity. A declared field that stayed absent is left
	// empty so the Go transport default never stands in for a provider field the
	// source declared.
	if _, declared := request.Header["User-Agent"]; !declared {
		request.Header["User-Agent"] = []string{""}
	}
	request.Close = true
	request.GetBody = nil
	if body != nil {
		request.ContentLength = int64(len(encoded.Body))
	}
	return request, nil
}

func buildTargetURL(origin *url.URL, encoded EncodedInputs) (*url.URL, error) {
	reference, err := url.Parse(encoded.EscapedPath)
	if origin == nil || err != nil || reference.IsAbs() || reference.Host != "" || reference.User != nil ||
		reference.Opaque != "" || reference.RawQuery != "" || reference.Fragment != "" ||
		!strings.HasPrefix(reference.Path, "/") || unsafeHTTPPath(reference.Path) {
		return nil, fmt.Errorf("%w: encoded path is invalid", ErrInvalidHTTPInput)
	}
	base := *origin
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
		if base.RawPath != "" {
			base.RawPath += "/"
		}
	}
	relative := &url.URL{
		Path:     strings.TrimPrefix(reference.Path, "/"),
		RawQuery: encoded.RawQuery,
	}
	if reference.RawPath != "" {
		relative.RawPath = strings.TrimPrefix(reference.RawPath, "/")
	}
	return base.ResolveReference(relative), nil
}

func unsafeHTTPPath(path string) bool {
	if strings.Contains(path, "//") || strings.ContainsRune(path, '\\') {
		return true
	}
	for start := 1; start <= len(path); {
		end := strings.IndexByte(path[start:], '/')
		if end < 0 {
			end = len(path)
		} else {
			end += start
		}
		segment := path[start:end]
		if segment == "." || segment == ".." {
			return true
		}
		if end == len(path) {
			break
		}
		start = end + 1
	}
	return false
}

func safeObservedRawQuery(target *url.URL, authorization *contract.OperationAuthorization) string {
	if authorization == nil || authorization.Placement != contract.FieldLocationQuery {
		return target.RawQuery
	}
	query := target.Query()
	query.Del(authorization.WireName)
	return query.Encode()
}

func validateResponseTransport(response *http.Response) error {
	if response == nil || response.ProtoMajor != 1 || response.ProtoMinor != 1 ||
		response.StatusCode == http.StatusSwitchingProtocols || response.Header.Get("Upgrade") != "" {
		return ErrUnsupportedHTTPTransport
	}
	return nil
}

func validateRedirectTarget(statusCode int, header http.Header, requestURL *url.URL) error {
	switch statusCode {
	case http.StatusMultipleChoices, http.StatusMovedPermanently, http.StatusFound,
		http.StatusSeeOther, http.StatusUseProxy, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return nil
	}
	locations := header.Values("Location")
	if len(locations) == 0 {
		return nil
	}
	if len(locations) != 1 || requestURL == nil {
		return ErrDestinationDenied
	}
	reference, err := url.Parse(strings.TrimSpace(locations[0]))
	if err != nil || reference.User != nil {
		return ErrDestinationDenied
	}
	target := requestURL.ResolveReference(reference)
	if !sameOriginURL(target, requestURL) {
		return ErrDestinationDenied
	}
	return nil
}

func readObjectBody(
	ctx context.Context,
	reader ObjectReader,
	call PhysicalCall,
	reference contract.ObjectRef,
	maxBytes uint64,
) ([]byte, error) {
	if reader == nil {
		return nil, fmt.Errorf("%w: object reader is absent", ErrUnsupportedWireValue)
	}
	if err := reference.Validate(); err != nil || reference.OrgID != call.Scope.OrgID || reference.WorkspaceID != call.Scope.WorkspaceID {
		return nil, fmt.Errorf("%w: binary request reference is invalid", ErrUnsupportedWireValue)
	}
	if reference.ByteLength < 0 || uint64(reference.ByteLength) > maxBytes {
		return nil, ErrRequestTooLarge
	}
	stream, err := reader.OpenObject(ctx, reference)
	if err != nil {
		return nil, fmt.Errorf("%w: binary request could not be opened", ErrUnsupportedWireValue)
	}
	defer stream.Close()
	body, err := io.ReadAll(io.LimitReader(stream, int64(maxBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("%w: binary request could not be read", ErrUnsupportedWireValue)
	}
	if uint64(len(body)) > maxBytes || int64(len(body)) != reference.ByteLength {
		return nil, ErrRequestTooLarge
	}
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != reference.SHA256 {
		return nil, fmt.Errorf("%w: binary request digest mismatch", ErrUnsupportedWireValue)
	}
	return body, nil
}

func readResponseBody(response *http.Response, maxBytes uint64) ([]byte, error) {
	if response.ContentLength > int64(maxBytes) {
		return nil, ErrResponseTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(maxBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("%w: response body read failed", ErrUnexpectedHTTPResponse)
	}
	if uint64(len(body)) > maxBytes {
		return nil, ErrResponseTooLarge
	}
	return body, nil
}

func declaresHeader(operation contract.Operation, name string) bool {
	for _, binding := range operation.Inputs {
		if binding.Location == contract.FieldLocationHeader && strings.EqualFold(binding.WireName, name) {
			return true
		}
	}
	return false
}

func serveContinuationTail(
	ctx context.Context,
	state ContinuationState,
	limits CallLimits,
	controls contract.ResultControls,
	codec ContinuationCodec,
	binding ContinuationBinding,
) (contract.ActionResult, error) {
	if state.MediaType == "" {
		return contract.ActionResult{}, fmt.Errorf("%w: continuation media type is absent", ErrNavigationConflict)
	}
	retainedCount := uint64(len(state.UnreadTail))
	emitted, tail, err := applyRecordControls(state.UnreadTail, state.MediaType, controls, limits.TotalRecordCap)
	if err != nil {
		return contract.ActionResult{}, err
	}
	state.UnreadTail = tail
	continuation := contract.AbsentValue()
	exhausted := state.SourceExhausted
	if len(state.UnreadTail) != 0 || len(state.NextInputs) != 0 {
		if codec == nil {
			return contract.ActionResult{}, ErrContinuationRequired
		}
		continuation, err = codec.SealContinuation(ctx, binding, state)
		if err != nil || continuation.Presence() != contract.PresenceValue {
			return contract.ActionResult{}, ErrContinuationRequired
		}
		exhausted = false
	}
	result := contract.ActionResult{
		MediaType: state.MediaType,
		Data:      contract.NewArrayValue(emitted),
		Pagination: &contract.ResultPagination{
			EffectiveWireLimit: cloneUint64(state.EffectiveWireLimit),
			TotalRecordCap:     cloneUint64(limits.TotalRecordCap),
			Continuation:       continuation,
			UnreadTail:         clonePresentValues(tail),
			FetchedCount:       retainedCount,
			ReturnedCount:      retainedCount,
			EmittedCount:       uint64(len(emitted)),
			ProviderTotal:      cloneExactNumber(state.ProviderTotal),
			SourceExhausted:    exhausted,
		},
	}
	if err := result.Validate(); err != nil {
		return contract.ActionResult{}, err
	}
	return result, nil
}

func cloneExactNumber(value *contract.ExactNumber) *contract.ExactNumber {
	if value == nil {
		return nil
	}
	return new(*value)
}
