package api

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

var (
	ErrClientTransport        = errors.New("PM API transport failed")
	ErrCompatibility          = errors.New("PM API compatibility negotiation failed")
	ErrInstanceMismatch       = errors.New("PM server instance does not match the selected profile")
	ErrSessionRequired        = errors.New("selected server profile has no usable PM session")
	ErrUnsafeClientRequest    = errors.New("PM API request target or authentication header is unsafe")
	ErrMutationOutcomeUnknown = errors.New("PM mutation outcome is unknown")
)

// ClientCompatibility describes one API-major client feature set. Release
// marketing versions do not participate in wire compatibility.
type ClientCompatibility struct {
	ProtocolMajor     int
	SupportedFeatures []string
}

func CurrentClientCompatibility() ClientCompatibility {
	return ClientCompatibility{ProtocolMajor: ProtocolMajor, SupportedFeatures: slices.Clone(SupportedFeatures[:])}
}

type QualifiedClientOptions struct {
	HTTPClient     HttpRequestDoer
	RequestTimeout time.Duration
	Compatibility  ClientCompatibility
}

// QualifiedClient is an immutable layer around the generated response client.
// Construction performs /v1/info compatibility and exact instance binding
// before callers can obtain the generated API interface.
type QualifiedClient struct {
	profile        ServerProfile
	information    DecodedServerInfo
	principal      Principal
	httpClient     HttpRequestDoer
	requestTimeout time.Duration
	compatibility  ClientCompatibility
	generated      *ClientWithResponses
}

func NewQualifiedClient(ctx context.Context, profile ServerProfile, options QualifiedClientOptions) (*QualifiedClient, error) {
	if ctx == nil {
		return nil, ErrClientTransport
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := profile.validate(); err != nil {
		return nil, err
	}
	requestTimeout := options.RequestTimeout
	if requestTimeout == 0 {
		requestTimeout = 30 * time.Second
	}
	if requestTimeout < time.Second || requestTimeout > 5*time.Minute {
		return nil, errors.New("PM API request timeout must be between 1s and 5m")
	}
	compatibility := options.Compatibility
	if compatibility.ProtocolMajor == 0 && compatibility.SupportedFeatures == nil {
		compatibility = CurrentClientCompatibility()
	}
	if compatibility.ProtocolMajor != ProtocolMajor || !validFeatureList(compatibility.SupportedFeatures) {
		return nil, ErrCompatibility
	}
	for _, feature := range compatibility.SupportedFeatures {
		if !IsSupportedFeature(feature) {
			return nil, ErrCompatibility
		}
	}
	doer := options.HTTPClient
	if doer == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		// A fresh HTTP/1.1 connection prevents net/http from adding a hidden
		// transport-level replay beneath the explicit three-send recovery loop.
		transport.DisableKeepAlives = true
		transport.ForceAttemptHTTP2 = false
		transport.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
		doer = &http.Client{
			Transport: transport,
			Timeout:   requestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	doer = boundedResponseDoer{next: doer}
	generated, err := NewClientWithResponses(profile.ServerURL, WithHTTPClient(doer))
	if err != nil {
		return nil, ErrCompatibility
	}
	response, err := generated.GetServerInfoWithResponse(ctx)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, ErrClientTransport
	}
	if response.StatusCode() != http.StatusOK {
		envelope, decodeErr := DecodeEnvelope(response)
		if decodeErr != nil {
			return nil, ErrCompatibility
		}
		if problem := EnvelopeError(envelope); problem != nil {
			return nil, problem
		}
		return nil, ErrCompatibility
	}
	information, err := DecodeServerInfo(response.Body, compatibility.ProtocolMajor, compatibility.SupportedFeatures)
	if err != nil {
		return nil, ErrCompatibility
	}
	if information.Value.InstanceId != profile.InstanceID {
		return nil, ErrInstanceMismatch
	}
	return &QualifiedClient{
		profile:        cloneServerProfile(profile),
		information:    cloneServerInfo(information),
		httpClient:     doer,
		requestTimeout: requestTimeout,
		compatibility:  ClientCompatibility{ProtocolMajor: compatibility.ProtocolMajor, SupportedFeatures: slices.Clone(compatibility.SupportedFeatures)},
		generated:      generated,
	}, nil
}

// WithSession returns a separately authenticated generated client after the
// already-qualified instance's PM session is loaded. No fallback store is
// consulted and the original unauthenticated client is unchanged.
func (client *QualifiedClient) WithSession(ctx context.Context, store SessionStore) (*QualifiedClient, error) {
	if client == nil || ctx == nil || store == nil {
		return nil, ErrSessionRequired
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, err := NewSessionKey(client.profile.InstanceID, client.profile.SessionHandle)
	if err != nil {
		return nil, ErrSessionRequired
	}
	session, err := store.Load(ctx, key)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		if errors.Is(err, ErrSessionStoreUnavailable) {
			return nil, ErrSessionStoreUnavailable
		}
		return nil, ErrSessionRequired
	}
	if !session.ExpiresAt.After(time.Now()) {
		return nil, ErrSessionRequired
	}
	origin, err := url.Parse(client.profile.ServerURL)
	if err != nil {
		return nil, ErrUnsafeClientRequest
	}
	editor := func(_ context.Context, request *http.Request) error {
		if request == nil || request.URL == nil || request.URL.Scheme != origin.Scheme || request.URL.Host != origin.Host || request.URL.User != nil || request.Header.Get("Authorization") != "" {
			return ErrUnsafeClientRequest
		}
		request.Header.Set("Authorization", "Bearer "+session.PmSessionCredential)
		return nil
	}
	generated, err := NewClientWithResponses(client.profile.ServerURL, WithHTTPClient(client.httpClient), WithRequestEditorFn(editor))
	if err != nil {
		return nil, ErrUnsafeClientRequest
	}
	copy := *client
	copy.profile = cloneServerProfile(client.profile)
	copy.information = cloneServerInfo(client.information)
	copy.compatibility.SupportedFeatures = slices.Clone(client.compatibility.SupportedFeatures)
	copy.principal = session.Principal
	copy.generated = generated
	return &copy, nil
}

// API exposes only the generated response interface after qualification.
// Issues 20 and 23 can consume this method without importing a second transport.
func (client *QualifiedClient) API() ClientWithResponsesInterface {
	if client == nil || client.generated == nil {
		return nil
	}
	generated, ok := client.generated.ClientInterface.(*Client)
	if !ok || generated == nil {
		return nil
	}
	copy := *generated
	copy.RequestEditors = slices.Clone(generated.RequestEditors)
	return &ClientWithResponses{ClientInterface: &copy}
}

func (client *QualifiedClient) ServerInfo() DecodedServerInfo {
	if client == nil {
		return DecodedServerInfo{}
	}
	return cloneServerInfo(client.information)
}

func (client *QualifiedClient) Profile() ServerProfile {
	if client == nil {
		return ServerProfile{}
	}
	return cloneServerProfile(client.profile)
}

// ArtifactOperationResponse is the typed response envelope for the three
// canonical manual artifact operations deliberately excluded from the
// ordinary generated client. The canonical schema currently exposes only
// problem responses for these routes.
type ArtifactOperationResponse struct {
	OperationID  string
	Body         []byte
	HTTPResponse *http.Response
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
	return client.doArtifactOperation(ctx, "uploadArtifact", workspaceID, "")
}

func (client *QualifiedClient) InspectArtifactWithResponse(ctx context.Context, workspaceID WorkspaceID, artifactID ArtifactID) (*ArtifactOperationResponse, error) {
	return client.doArtifactOperation(ctx, "inspectArtifact", workspaceID, artifactID)
}

func (client *QualifiedClient) DownloadArtifactWithResponse(ctx context.Context, workspaceID WorkspaceID, artifactID ArtifactID) (*ArtifactOperationResponse, error) {
	return client.doArtifactOperation(ctx, "downloadArtifact", workspaceID, artifactID)
}

func (client *QualifiedClient) doArtifactOperation(ctx context.Context, operationID string, workspaceID WorkspaceID, artifactID ArtifactID) (*ArtifactOperationResponse, error) {
	if client == nil || client.generated == nil || ctx == nil || !validIdentity(string(workspaceID), 512) ||
		!client.principal.Kind.Valid() || !validIdentity(client.principal.PrincipalId, 512) {
		return nil, ErrUnsafeClientRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	control, present := OperationControlFor(operationID)
	if !present || control.HasBody || control.HasProtocol || control.RequiresIdempotencyKey ||
		control.RequiresIfMatch || len(control.QueryFields) != 0 {
		return nil, ErrCompatibility
	}
	var method, canonicalPath string
	switch operationID {
	case "uploadArtifact":
		method = http.MethodPost
		canonicalPath = "/v1/workspaces/{workspace_id}/artifacts"
		if artifactID != "" {
			return nil, ErrUnsafeClientRequest
		}
	case "inspectArtifact":
		method = http.MethodGet
		canonicalPath = "/v1/workspaces/{workspace_id}/artifacts/{artifact_id}"
	case "downloadArtifact":
		method = http.MethodGet
		canonicalPath = "/v1/workspaces/{workspace_id}/artifacts/{artifact_id}/content"
	default:
		return nil, ErrCompatibility
	}
	routeOperation, routed := OperationIDForRoute(method, canonicalPath)
	if control.Method != method || control.Path != canonicalPath || !routed || routeOperation != operationID {
		return nil, ErrCompatibility
	}
	if strings.Contains(canonicalPath, "{artifact_id}") && !validIdentity(string(artifactID), 512) {
		return nil, ErrUnsafeClientRequest
	}
	path := strings.ReplaceAll(canonicalPath, "{workspace_id}", url.PathEscape(string(workspaceID)))
	path = strings.ReplaceAll(path, "{artifact_id}", url.PathEscape(string(artifactID)))
	if strings.Contains(path, "{") {
		return nil, ErrCompatibility
	}
	generated, ok := client.generated.ClientInterface.(*Client)
	if !ok || generated == nil || generated.Client == nil {
		return nil, ErrCompatibility
	}
	serverURL, err := url.Parse(generated.Server)
	if err != nil {
		return nil, ErrCompatibility
	}
	if strings.HasPrefix(path, "/") {
		path = "." + path
	}
	requestURL, err := serverURL.Parse(path)
	if err != nil {
		return nil, ErrCompatibility
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), nil)
	if err != nil {
		return nil, ErrUnsafeClientRequest
	}
	request.Header.Set("Accept", "application/problem+json")
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
	return &ArtifactOperationResponse{
		OperationID:  operationID,
		Body:         append([]byte(nil), body...),
		HTTPResponse: response,
	}, nil
}

type RecoveryAttemptResult struct {
	Response   WireResponse
	RetryAfter string
}

type RecoveryAttempt func(context.Context) (RecoveryAttemptResult, error)

// SendMutationWithRecovery persists a secret-free key and typed-intent
// fingerprint before the first mutation send, then performs at most three
// unchanged sends inside one 30-second deadline. It never changes a key,
// revision or body and never owns provider/run retry timing.
func (client *QualifiedClient) SendMutationWithRecovery(
	ctx context.Context,
	store RecoveryStore,
	workspaceID WorkspaceID,
	operation string,
	target string,
	protocol ProtocolMetadata,
	typedRequest any,
	attempt RecoveryAttempt,
) (WireResponse, error) {
	if client == nil || client.generated == nil || ctx == nil || store == nil || attempt == nil ||
		!validIdentity(string(workspaceID), 512) || !validIdentity(operation, 512) || len(target) > 1024 ||
		(target != "" && strings.TrimSpace(target) != target) ||
		!client.principal.Kind.Valid() || !validIdentity(client.principal.PrincipalId, 512) {
		return nil, ErrMalformedEnvelope
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	protocolDocument, err := json.Marshal(protocol)
	if err != nil {
		return nil, ErrMalformedEnvelope
	}
	protocolObject, err := decodeJSONObject(protocolDocument)
	if err != nil || validateProtocolMetadata(protocolObject) != nil {
		return nil, ErrMalformedEnvelope
	}
	document, err := json.Marshal(typedRequest)
	if err != nil {
		return nil, ErrMalformedEnvelope
	}
	requestObject, err := decodeJSONObject(document)
	if err != nil {
		return nil, ErrMalformedEnvelope
	}
	embeddedProtocol, present := requestObject["protocol"]
	if !present {
		return nil, ErrMalformedEnvelope
	}
	embeddedDigest, err := contract.CanonicalDigest(embeddedProtocol)
	if err != nil {
		return nil, ErrMalformedEnvelope
	}
	protocolDigest, err := contract.CanonicalDigest(protocolDocument)
	if err != nil || embeddedDigest != protocolDigest {
		return nil, ErrMalformedEnvelope
	}
	typedFingerprint, err := contract.CanonicalDigest(document)
	if err != nil {
		return nil, ErrMalformedEnvelope
	}
	intentFingerprint := contract.SemanticDigest(protocol.IntentDigest)
	if err := intentFingerprint.Validate(); err != nil {
		return nil, ErrMalformedEnvelope
	}
	record := RecoveryRecord{
		InstanceID:              client.profile.InstanceID,
		WorkspaceID:             string(workspaceID),
		PrincipalKind:           client.principal.Kind,
		PrincipalID:             client.principal.PrincipalId,
		Operation:               operation,
		Target:                  target,
		RequestKey:              protocol.RequestKey,
		IntentFingerprint:       intentFingerprint,
		TypedRequestFingerprint: typedFingerprint,
	}
	if err := store.Persist(ctx, record); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(30 * time.Second)
	if parentDeadline, present := ctx.Deadline(); present && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	recoveryContext, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err := recoveryContext.Err(); err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, ErrClientTransport
	}
	var lastResponse WireResponse
	for sendIndex := range 3 {
		result, sendErr := attempt(recoveryContext)
		if sendErr == nil && result.Response != nil {
			lastResponse = result.Response
			if !retryableMutationResponse(result.Response) {
				return result.Response, nil
			}
		}
		if sendIndex == 2 {
			break
		}
		delay := []time.Duration{250 * time.Millisecond, time.Second}[sendIndex]
		if sendErr == nil && result.Response != nil {
			retryAfter, present, valid := parseRetryAfter(result.RetryAfter, time.Now())
			if present && !valid {
				return result.Response, nil
			}
			if retryAfter > delay {
				delay = retryAfter
			}
		}
		if !time.Now().Add(delay).Before(deadline) {
			if lastResponse != nil {
				return lastResponse, nil
			}
			return nil, ErrMutationOutcomeUnknown
		}
		timer := time.NewTimer(delay)
		select {
		case <-recoveryContext.Done():
			timer.Stop()
			if lastResponse != nil {
				return lastResponse, nil
			}
			return nil, ErrMutationOutcomeUnknown
		case <-timer.C:
		}
	}
	if lastResponse != nil {
		return lastResponse, nil
	}
	return nil, ErrMutationOutcomeUnknown
}

// SubmitInvocationWithRecovery is the typed invocation adapter consumed by the
// generated-command projection. It uses the same generated client transport.
func (client *QualifiedClient) SubmitInvocationWithRecovery(
	ctx context.Context,
	store RecoveryStore,
	workspaceID WorkspaceID,
	params *SubmitInvocationParams,
	body SubmitInvocationJSONRequestBody,
) (*SubmitInvocationResponse, error) {
	if params == nil || string(params.IdempotencyKey) != body.Protocol.RequestKey {
		return nil, ErrMalformedEnvelope
	}
	response, err := client.SendMutationWithRecovery(ctx, store, workspaceID, "invocation.submit", body.ConnectionRef+"/"+body.OperationRef, body.Protocol, body, func(attemptContext context.Context) (RecoveryAttemptResult, error) {
		value, sendErr := client.generated.SubmitInvocationWithResponse(attemptContext, workspaceID, params, body)
		if sendErr != nil || value == nil || value.HTTPResponse == nil {
			return RecoveryAttemptResult{}, sendErr
		}
		return RecoveryAttemptResult{Response: value, RetryAfter: value.HTTPResponse.Header.Get("Retry-After")}, nil
	})
	if err != nil {
		return nil, err
	}
	typed, ok := response.(*SubmitInvocationResponse)
	if !ok {
		return nil, ErrMalformedEnvelope
	}
	return typed, nil
}

func retryableMutationResponse(response WireResponse) bool {
	if response == nil {
		return true
	}
	switch response.StatusCode() {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		envelope, err := DecodeEnvelope(response)
		if err != nil || envelope.Problem == nil {
			return false
		}
		switch envelope.Problem.Value.SafeRetryAction {
		case "after_backlog_progress", "same_key_after_dependency_recovery", "same_key_reconcile_only":
			return true
		default:
			return false
		}
	case http.StatusConflict, http.StatusGone, http.StatusPreconditionFailed, http.StatusPreconditionRequired:
		return false
	default:
		return false
	}
}

func parseRetryAfter(raw string, now time.Time) (time.Duration, bool, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false, true
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds < 0 {
			return 0, true, false
		}
		if seconds > 30 {
			return 31 * time.Second, true, true
		}
		return time.Duration(seconds) * time.Second, true, true
	}
	instant, err := http.ParseTime(raw)
	if err != nil || instant.Before(now) {
		return 0, true, false
	}
	return instant.Sub(now), true, true
}

func cloneServerInfo(information DecodedServerInfo) DecodedServerInfo {
	copy := information
	copy.Value.Features = slices.Clone(information.Value.Features)
	copy.Value.RequiredFeatures = slices.Clone(information.Value.RequiredFeatures)
	copy.Value.AdditionalProperties = cloneRawMap(information.Value.AdditionalProperties)
	copy.AdditiveFields = cloneRawMap(information.AdditiveFields)
	return copy
}

func cloneRawMap(values map[string]json.RawMessage) map[string]json.RawMessage {
	if values == nil {
		return nil
	}
	result := make(map[string]json.RawMessage, len(values))
	for name, value := range values {
		result[name] = append(json.RawMessage(nil), value...)
	}
	return result
}

type boundedResponseDoer struct {
	next HttpRequestDoer
}

func (doer boundedResponseDoer) Do(request *http.Request) (*http.Response, error) {
	if doer.next == nil || request == nil {
		return nil, ErrClientTransport
	}
	response, err := doer.next.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if contextErr := request.Context().Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, ErrClientTransport
	}
	if response == nil || response.Body == nil {
		return nil, ErrClientTransport
	}
	response.Body = &limitedReadCloser{Reader: io.LimitReader(response.Body, maxPublicEnvelopeBytes+1), Closer: response.Body}
	return response, nil
}

type limitedReadCloser struct {
	io.Reader
	io.Closer
}
