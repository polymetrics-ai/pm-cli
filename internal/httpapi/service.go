package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/oapi-codegen/nullable"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const (
	browserSessionCookie = "__Host-pm_session"
	browserCSRFCookie    = "__Host-pm_csrf"
	oidcChallengeCookie  = "__Host-pm_oidc"
	csrfHeader           = "X-CSRF-Token"
	defaultPageSize      = 100
	maximumCursorBytes   = 4096
)

var errInvalidHTTPInput = errors.New("invalid HTTP application input")

type Service struct {
	publicOrigin          string
	auth                  *auth.Service
	identity              *app.IdentityService
	credentials           *app.CredentialIngress
	instanceID            string
	oidcHTTPClient        *http.Client
	admission             *app.AdmissionService
	runs                  *app.RunService
	resolveEventCursorKey func(context.Context) ([]byte, error)
	eventBufferSize       uint64
	eventWriteTimeout     time.Duration
	draining              <-chan struct{}
	cursorKey             [sha256.Size]byte
}

var _ StrictServerInterface = (*Service)(nil)

type requestStateKey struct{}

type requestState struct {
	inbound       verifiedInbound
	identity      auth.AuthenticatedIdentity
	sessionCookie string
	csrfToken     string
	request       *http.Request
	writer        http.ResponseWriter
}

type requestPolicy uint8

const (
	policyPublic requestPolicy = iota
	policyBrowserLogin
	policyDeviceLogin
	policyAuthenticated
	policyBrowserOnly
)

type cursorEnvelope struct {
	Version       int                    `json:"version"`
	InstanceID    string                 `json:"instance_id"`
	Operation     string                 `json:"operation"`
	OrgID         string                 `json:"org_id"`
	PrincipalID   string                 `json:"principal_id"`
	PrincipalKind contract.PrincipalKind `json:"principal_kind"`
	WorkspaceID   string                 `json:"workspace_id"`
	PageSize      uint32                 `json:"page_size"`
	AfterID       string                 `json:"after_id"`
}

func NewService(configuration ServiceConfig) (*Service, error) {
	if configuration.PublicOrigin == "" || configuration.Auth == nil ||
		configuration.Identity == nil || configuration.Credentials == nil ||
		configuration.Admission == nil || configuration.Runs == nil || configuration.OIDCHTTPClient == nil {
		return nil, errors.New("validated configuration and HTTP service dependencies are required")
	}
	if !validHTTPIdentifier(configuration.InstanceID, 512) {
		return nil, errors.New("HTTP service instance identity is invalid")
	}
	client := *configuration.OIDCHTTPClient
	service := &Service{
		publicOrigin:          configuration.PublicOrigin,
		auth:                  configuration.Auth,
		identity:              configuration.Identity,
		credentials:           configuration.Credentials,
		instanceID:            configuration.InstanceID,
		oidcHTTPClient:        &client,
		admission:             configuration.Admission,
		runs:                  configuration.Runs,
		resolveEventCursorKey: configuration.ResolveEventCursorKey,
		eventBufferSize:       configuration.EventBufferSize,
		eventWriteTimeout:     configuration.EventWriteTimeout,
		draining:              configuration.Draining,
	}
	if _, err := io.ReadFull(rand.Reader, service.cursorKey[:]); err != nil {
		return nil, fmt.Errorf("initialize HTTP cursor authentication: %w", err)
	}
	return service, nil
}

func (service *Service) Middleware() MiddlewareFunc {
	return func(c *gin.Context) {
		if c == nil {
			return
		}
		if service == nil || c.Request == nil {
			writeProblem(c, api.NewInternalProblem(nil))
			return
		}
		inbound, ok := c.Request.Context().Value(verifiedInboundKey{}).(verifiedInbound)
		if !ok {
			writeProblem(c, newProblem(http.StatusForbidden, "inbound_policy_rejected", "use_trusted_transport", nil))
			return
		}
		policy, known := policyForRequest(c.Request.Method, c.FullPath())
		if !known {
			writeProblem(c, newProblem(http.StatusBadRequest, "invalid_request", "correct_input", nil))
			return
		}
		state := requestState{inbound: inbound, request: c.Request, writer: c.Writer}
		if policy == policyPublic || policy == policyBrowserLogin || policy == policyDeviceLogin {
			// RequestBoundary admits plaintext here only for the credential-free
			// loopback GET /v1/info exception.
			plaintextDiscovery := policy == policyPublic && !inbound.TLS
			if policy != policyBrowserLogin && !plaintextDiscovery {
				browser := policy == policyPublic && inbound.Origin != ""
				if err := service.auth.ValidateInboundPolicy(auth.InboundRequest{
					TLS: inbound.TLS, Host: inbound.Host, Origin: inbound.Origin,
					Browser: browser, RequireOrigin: false,
				}); err != nil {
					writeProblem(c, problemForKnownError(err, nil))
					return
				}
			}
			service.attachState(c, state)
			c.Next()
			return
		}

		authorization, authorizationPresent, err := oneHeader(c.Request, "Authorization")
		if err != nil {
			writeProblem(c, problemForKnownError(auth.ErrAuthenticationRequired, nil))
			return
		}
		sessionCookie, sessionPresent, err := oneCookie(c.Request, browserSessionCookie)
		if err != nil || authorizationPresent == sessionPresent {
			writeProblem(c, problemForKnownError(auth.ErrAuthenticationRequired, nil))
			return
		}
		browser := sessionPresent
		if policy == policyBrowserOnly && !browser {
			writeProblem(c, problemForKnownError(auth.ErrAuthenticationRequired, nil))
			return
		}
		mutation := c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead
		if err := service.auth.ValidateInboundPolicy(auth.InboundRequest{
			TLS: inbound.TLS, Host: inbound.Host, Origin: inbound.Origin,
			Browser: browser, Mutation: mutation, RequireOrigin: browser && mutation,
		}); err != nil {
			writeProblem(c, problemForKnownError(err, nil))
			return
		}

		var identity auth.AuthenticatedIdentity
		if browser {
			identity, err = service.auth.Authenticate(c.Request.Context(), auth.SessionAuthentication{BrowserCookie: sessionCookie})
		} else {
			bearer, parseErr := parseBearer(authorization)
			if parseErr != nil {
				writeProblem(c, problemForKnownError(auth.ErrAuthenticationRequired, nil))
				return
			}
			if strings.HasPrefix(bearer, auth.ServiceCredentialPrefix+".") {
				identity, err = service.auth.AuthenticateServiceCredential(c.Request.Context(), bearer)
			} else {
				identity, err = service.auth.Authenticate(c.Request.Context(), auth.SessionAuthentication{BearerToken: bearer})
			}
		}
		if err != nil {
			writeProblem(c, problemForKnownError(err, nil))
			return
		}
		state.identity = identity
		state.sessionCookie = sessionCookie
		if browser && mutation {
			csrfToken, tokenPresent, tokenErr := oneHeader(c.Request, csrfHeader)
			csrfCookie, cookiePresent, cookieErr := oneCookie(c.Request, browserCSRFCookie)
			if tokenErr != nil || cookieErr != nil || !tokenPresent || !cookiePresent ||
				len(csrfToken) != len(csrfCookie) || subtle.ConstantTimeCompare([]byte(csrfToken), []byte(csrfCookie)) != 1 {
				writeProblem(c, problemForKnownError(auth.ErrInvalidCSRF, nil))
				return
			}
			validated, csrfErr := service.auth.ValidateCSRF(c.Request.Context(), auth.BrowserCSRFRequest{
				CookieToken: sessionCookie,
				CSRFToken:   csrfToken,
			})
			if csrfErr != nil || validated.Principal != identity.Principal || validated.SessionID != identity.SessionID {
				writeProblem(c, problemForKnownError(auth.ErrInvalidCSRF, nil))
				return
			}
			state.csrfToken = csrfToken
		}
		service.attachState(c, state)
		c.Next()
	}
}

func policyForRequest(method, path string) (requestPolicy, bool) {
	switch method + " " + path {
	case "GET /v1/info":
		return policyPublic, true
	case "POST /v1/sessions/browser/challenges", "POST /v1/sessions/browser":
		return policyBrowserLogin, true
	case "POST /v1/sessions/device", "POST /v1/sessions/device/exchange":
		return policyDeviceLogin, true
	case "POST /v1/sessions/device/approve", "POST /v1/sessions/device/deny",
		"POST /admin/v1/workspaces/:workspace_id/connections/:connection_id/credentials":
		return policyBrowserOnly, true
	case "DELETE /v1/sessions/current", "GET /v1/sessions/me",
		"GET /v1/workspaces", "GET /v1/workspaces/:workspace_id",
		"GET /v1/workspaces/:workspace_id/connections", "POST /v1/workspaces/:workspace_id/connections",
		"GET /v1/workspaces/:workspace_id/connections/:connection_id", "PATCH /v1/workspaces/:workspace_id/connections/:connection_id",
		"DELETE /v1/workspaces/:workspace_id/connections/:connection_id",
		"GET /v1/workspaces/:workspace_id/delegations", "POST /v1/workspaces/:workspace_id/delegations",
		"POST /v1/workspaces/:workspace_id/delegations/:id/revoke",
		"POST /v1/workspaces/:workspace_id/invocations",
		"GET /v1/workspaces/:workspace_id/runs",
		"GET /v1/workspaces/:workspace_id/runs/:run_id",
		"GET /v1/workspaces/:workspace_id/runs/:run_id/result",
		"GET /v1/workspaces/:workspace_id/runs/:run_id/tasks",
		"GET /v1/workspaces/:workspace_id/runs/:run_id/events",
		"POST /v1/workspaces/:workspace_id/artifacts",
		"GET /v1/workspaces/:workspace_id/artifacts/:artifact_id",
		"GET /v1/workspaces/:workspace_id/artifacts/:artifact_id/content",
		"GET /v1/workspaces/:workspace_id/tasks/:task_id",
		"POST /v1/workspaces/:workspace_id/runs/:run_id/cancel",
		"POST /v1/workspaces/:workspace_id/runs/:run_id/resume",
		"GET /v1/workspaces/:workspace_id/memberships", "POST /v1/workspaces/:workspace_id/memberships",
		"POST /v1/workspaces/:workspace_id/memberships/:id/revoke",
		"GET /v1/workspaces/:workspace_id/service-accounts", "POST /v1/workspaces/:workspace_id/service-accounts",
		"POST /v1/workspaces/:workspace_id/service-accounts/:id/revoke":
		return policyAuthenticated, true
	default:
		return 0, false
	}
}

func (service *Service) attachState(c *gin.Context, state requestState) {
	ctx := context.WithValue(c.Request.Context(), requestStateKey{}, state)
	c.Request = c.Request.WithContext(ctx)
}

func stateFromContext(ctx context.Context) (requestState, bool) {
	if ctx == nil {
		return requestState{}, false
	}
	state, ok := ctx.Value(requestStateKey{}).(requestState)
	return state, ok
}

func authenticatedState(ctx context.Context) (requestState, error) {
	state, ok := stateFromContext(ctx)
	if !ok || state.identity.Principal.Validate() != nil {
		return requestState{}, auth.ErrAuthenticationRequired
	}
	return state, nil
}

func oneHeader(request *http.Request, name string) (string, bool, error) {
	values := request.Header.Values(name)
	if len(values) == 0 {
		return "", false, nil
	}
	if len(values) != 1 || values[0] == "" {
		return "", false, errors.New("header must occur exactly once")
	}
	return values[0], true, nil
}

func oneCookie(request *http.Request, name string) (string, bool, error) {
	var value string
	count := 0
	for _, cookie := range request.Cookies() {
		if cookie.Name == name {
			count++
			value = cookie.Value
		}
	}
	if count == 0 {
		return "", false, nil
	}
	if count != 1 || value == "" {
		return "", false, errors.New("cookie must occur exactly once")
	}
	return value, true, nil
}

func parseBearer(value string) (string, error) {
	if strings.TrimSpace(value) != value {
		return "", auth.ErrAuthenticationRequired
	}
	parts := strings.Split(value, " ")
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || !validHTTPIdentifier(parts[1], 8192) {
		return "", auth.ErrAuthenticationRequired
	}
	return parts[1], nil
}

func validHTTPIdentifier(value string, maximum int) bool {
	if value == "" || len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character <= 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func (service *Service) parsePage(
	operation string,
	principal contract.Principal,
	scope contract.Scope,
	cursor *string,
	requestedSize *int,
) (string, uint32, error) {
	if cursor == nil {
		size := defaultPageSize
		if requestedSize != nil {
			size = *requestedSize
		}
		if size < 1 || size > 100 {
			return "", 0, errors.New("page size is invalid")
		}
		return "", uint32(size), nil
	}
	if len(*cursor) == 0 || len(*cursor) > maximumCursorBytes {
		return "", 0, errors.New("cursor is invalid")
	}
	parts := strings.Split(*cursor, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return "", 0, errors.New("cursor is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", 0, errors.New("cursor is invalid")
	}
	mac, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(mac) != sha256.Size {
		return "", 0, errors.New("cursor is invalid")
	}
	verifier := hmac.New(sha256.New, service.cursorKey[:])
	_, _ = verifier.Write(payload)
	if !hmac.Equal(mac, verifier.Sum(nil)) {
		return "", 0, errors.New("cursor is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var envelope cursorEnvelope
	if err := decoder.Decode(&envelope); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return "", 0, errors.New("cursor is invalid")
	}
	if envelope.Version != 1 || envelope.InstanceID != service.instanceID || envelope.Operation != operation ||
		envelope.OrgID != principal.OrgID || envelope.PrincipalID != principal.PrincipalID ||
		envelope.PrincipalKind != principal.Kind || envelope.WorkspaceID != scope.WorkspaceID ||
		envelope.PageSize == 0 || envelope.PageSize > 100 || envelope.AfterID == "" {
		return "", 0, errors.New("cursor is invalid")
	}
	if requestedSize != nil && (*requestedSize < 1 || *requestedSize > 100 ||
		uint32(*requestedSize) != envelope.PageSize) {
		return "", 0, errors.New("cursor page size is inconsistent")
	}
	return envelope.AfterID, envelope.PageSize, nil
}

func (service *Service) nextCursor(
	operation string,
	principal contract.Principal,
	scope contract.Scope,
	pageSize uint32,
	afterID string,
) (nullable.Nullable[string], error) {
	if afterID == "" {
		return nullable.NewNullNullable[string](), nil
	}
	envelope := cursorEnvelope{
		Version: 1, InstanceID: service.instanceID, Operation: operation,
		OrgID: principal.OrgID, PrincipalID: principal.PrincipalID, PrincipalKind: principal.Kind,
		WorkspaceID: scope.WorkspaceID, PageSize: pageSize, AfterID: afterID,
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return nullable.Nullable[string]{}, err
	}
	mac := hmac.New(sha256.New, service.cursorKey[:])
	_, _ = mac.Write(payload)
	cursor := "v1." + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if len(cursor) > maximumCursorBytes {
		return nullable.Nullable[string]{}, errors.New("cursor exceeds wire limit")
	}
	return nullable.NewNullableWithValue(cursor), nil
}

func parseMutationControl(idempotencyKey string, protocol api.ProtocolMetadata, ifMatch *string) (app.MutationControl, error) {
	if idempotencyKey == "" || idempotencyKey != protocol.RequestKey {
		return app.MutationControl{}, errInvalidHTTPInput
	}
	intentRevision, err := parseRevision(protocol.ExpectedRevision)
	if err != nil {
		return app.MutationControl{}, err
	}
	control := app.MutationControl{IdempotencyKey: idempotencyKey, IntentRevision: intentRevision}
	if ifMatch != nil {
		expected, err := parseRevision(*ifMatch)
		if err != nil {
			return app.MutationControl{}, err
		}
		control.ExpectedRevision = &expected
	}
	return control, nil
}

func parseRevision(value string) (uint64, error) {
	if value == "" || len(value) > 20 || value[0] == '0' {
		return 0, errors.New("revision is invalid")
	}
	revision, err := strconv.ParseUint(value, 10, 64)
	if err != nil || revision == 0 || strconv.FormatUint(revision, 10) != value {
		return 0, errors.New("revision is invalid")
	}
	return revision, nil
}

func setETag(ctx context.Context, revision uint64) {
	if revision == 0 {
		return
	}
	if state, ok := stateFromContext(ctx); ok && state.writer != nil {
		state.writer.Header().Set("ETag", `"`+strconv.FormatUint(revision, 10)+`"`)
	}
}

func nullableTime(value *time.Time) nullable.Nullable[time.Time] {
	if value == nil {
		return nullable.NewNullNullable[time.Time]()
	}
	return nullable.NewNullableWithValue(*value)
}

func nullableString(value *string) nullable.Nullable[string] {
	if value == nil {
		return nullable.NewNullNullable[string]()
	}
	return nullable.NewNullableWithValue(*value)
}

func requiredNullableTime(value nullable.Nullable[time.Time]) (*time.Time, error) {
	if !value.IsSpecified() {
		return nil, errors.New("required nullable time is absent")
	}
	if value.IsNull() {
		return nil, nil
	}
	result, err := value.Get()
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func requiredNullableString(value nullable.Nullable[string]) (*string, error) {
	if !value.IsSpecified() {
		return nil, errors.New("required nullable string is absent")
	}
	if value.IsNull() {
		return nil, nil
	}
	result, err := value.Get()
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func cloneStrings(values []string) []string {
	result := make([]string, len(values))
	copy(result, values)
	return result
}

func problemForKnownError(err error, outcome *api.SafeOutcomeReference) api.Problem {
	var failure *app.Failure
	if errors.As(err, &failure) {
		var resolvedOutcome *api.SafeOutcomeReference
		switch failure.Code {
		case "retry_receipt_expired":
			if failure.ExistingResultRef != "" && outcome != nil {
				copy := *outcome
				if copy.Kind != api.SafeOutcomeReferenceKindRequest {
					copy.Ref = failure.ExistingResultRef
				}
				resolvedOutcome = &copy
			}
		case "mutation_outcome_unknown":
			if outcome != nil {
				copy := *outcome
				copy.Kind = api.SafeOutcomeReferenceKindRequest
				resolvedOutcome = &copy
			}
		}
		return newProblem(failure.HTTPStatus, failure.Code, failure.RetryAction, resolvedOutcome)
	}
	switch {
	case errors.Is(err, errInvalidHTTPInput):
		return newProblem(http.StatusBadRequest, "invalid_request", "correct_input", nil)
	case errors.Is(err, auth.ErrAuthenticationRequired):
		return newProblem(http.StatusUnauthorized, "authentication_required", "refresh_authorization", outcome)
	case errors.Is(err, auth.ErrInvalidInboundPolicy):
		return newProblem(http.StatusForbidden, "inbound_policy_rejected", "use_trusted_transport", outcome)
	case errors.Is(err, auth.ErrInvalidCSRF):
		return newProblem(http.StatusForbidden, "csrf_validation_failed", "refresh_browser_session", outcome)
	case errors.Is(err, auth.ErrChallengeInvalid), errors.Is(err, auth.ErrInvalidRequest), errors.Is(err, auth.ErrRequestTooLarge):
		return newProblem(http.StatusBadRequest, "login_challenge_invalid", "restart_login", outcome)
	case errors.Is(err, auth.ErrChallengeExpired):
		return newProblem(http.StatusGone, "login_challenge_expired", "restart_login", outcome)
	case errors.Is(err, auth.ErrChallengeUsed):
		return newProblem(http.StatusConflict, "login_challenge_used", "restart_login", outcome)
	case errors.Is(err, auth.ErrChallengePending):
		return newProblem(http.StatusConflict, "device_authorization_pending", "poll_after_interval", outcome)
	case errors.Is(err, auth.ErrChallengeDenied):
		return newProblem(http.StatusForbidden, "device_authorization_denied", "restart_login", outcome)
	case errors.Is(err, auth.ErrPollTooSoon):
		return newProblem(http.StatusTooManyRequests, "device_poll_too_soon", "poll_after_interval", outcome)
	case errors.Is(err, auth.ErrRateLimited):
		return newProblem(http.StatusTooManyRequests, "authentication_rate_limited", "retry_after_backoff", outcome)
	case errors.Is(err, auth.ErrOIDCVerification):
		return newProblem(http.StatusUnauthorized, "oidc_verification_failed", "restart_login", outcome)
	case errors.Is(err, auth.ErrGrantUnavailable), errors.Is(err, auth.ErrDelegationInvalid):
		return newProblem(http.StatusForbidden, "grant_unavailable", "refresh_authorization", outcome)
	case errors.Is(err, auth.ErrResourceNotFound), errors.Is(err, auth.ErrAuthorizationDenied):
		return newProblem(http.StatusNotFound, "resource_not_found", "correct_scope", outcome)
	default:
		return api.NewInternalProblem(err)
	}
}

func newProblem(status int, code, retryAction string, outcome *api.SafeOutcomeReference) api.Problem {
	title, detail := "Request failed", "The request could not be completed safely."
	switch status {
	case http.StatusBadRequest:
		title, detail = "Invalid request", "Correct the request and try again."
	case http.StatusUnauthorized:
		title, detail = "Authentication required", "Establish current authentication and try again."
	case http.StatusForbidden:
		title, detail = "Request denied", "The current authenticated authority cannot complete this request."
	case http.StatusNotFound:
		title, detail = "Resource unavailable", "The resource is unavailable in the authenticated scope."
	case http.StatusConflict:
		title, detail = "Request conflict", "Inspect the current state before continuing."
	case http.StatusGone:
		title, detail = "Request state expired", "The prior request state is no longer available."
	case http.StatusPreconditionFailed, http.StatusPreconditionRequired:
		title, detail = "Revision precondition failed", "Refresh the control revision before continuing."
	case http.StatusTooManyRequests:
		title, detail = "Request limited", "The applicable request limit is currently reached."
	case http.StatusServiceUnavailable:
		title, detail = "Service unavailable", "The service cannot currently complete the request."
	case http.StatusInternalServerError:
		title, detail = "Internal error", "The request could not be completed safely."
	}
	if code == "admission_outcome_unknown" || code == "mutation_outcome_unknown" {
		title, detail = "Outcome unknown", "Reconcile the original request before attempting another mutation."
	}
	return api.Problem{
		Type:             "https://polymetrics.ai/problems/" + code,
		Title:            title,
		Status:           status,
		Detail:           detail,
		Code:             code,
		SafeRetryAction:  retryAction,
		IncidentId:       api.NewInternalProblem(nil).IncidentId,
		OutcomeReference: outcome,
	}
}

type problemResponse struct {
	problem api.Problem
}

func asProblemResponse(err error, outcome *api.SafeOutcomeReference) problemResponse {
	return problemResponse{problem: problemForKnownError(err, outcome)}
}

func asMutationProblemResponse(
	err error,
	requestKey string,
	resourceKind api.SafeOutcomeReferenceKind,
	targetID string,
) problemResponse {
	var failure *app.Failure
	if !errors.As(err, &failure) {
		return asProblemResponse(err, nil)
	}
	var outcome *api.SafeOutcomeReference
	switch failure.Code {
	case "retry_receipt_expired":
		if failure.ExistingResultRef != "" {
			value := api.SafeOutcomeReference{Kind: resourceKind, Ref: failure.ExistingResultRef}
			outcome = &value
		}
	case "admission_outcome_unknown":
		value := api.SafeOutcomeReference{Kind: api.SafeOutcomeReferenceKindRequest, Ref: requestKey}
		outcome = &value
	case "mutation_outcome_unknown":
		value := api.SafeOutcomeReference{Kind: resourceKind, Ref: targetID}
		if targetID == "" {
			value = api.SafeOutcomeReference{Kind: api.SafeOutcomeReferenceKindRequest, Ref: requestKey}
		}
		outcome = &value
	}
	return problemResponse{problem: newProblem(failure.HTTPStatus, failure.Code, failure.RetryAction, outcome)}
}

func invalidRequestResponse() problemResponse {
	return problemResponse{problem: newProblem(http.StatusBadRequest, "invalid_request", "correct_input", nil)}
}

func (response problemResponse) write(writer http.ResponseWriter) error {
	var encoded bytes.Buffer
	if err := json.NewEncoder(&encoded).Encode(response.problem); err != nil {
		return err
	}
	writer.Header().Set("Content-Type", "application/problem+json")
	writer.WriteHeader(response.problem.Status)
	_, err := writer.Write(encoded.Bytes())
	return err
}

func (r problemResponse) VisitAdminStoreCredentialResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r problemResponse) VisitGetServerInfoResponse(w http.ResponseWriter) error { return r.write(w) }
func (r problemResponse) VisitCompleteBrowserLoginResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r problemResponse) VisitStartBrowserLoginResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r problemResponse) VisitRevokeCurrentSessionResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r problemResponse) VisitStartDeviceLoginResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r problemResponse) VisitApproveDeviceLoginResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r problemResponse) VisitDenyDeviceLoginResponse(w http.ResponseWriter) error { return r.write(w) }
func (r problemResponse) VisitExchangeDeviceLoginResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r problemResponse) VisitGetCurrentSessionResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r problemResponse) VisitListWorkspacesResponse(w http.ResponseWriter) error  { return r.write(w) }
func (r problemResponse) VisitGetWorkspaceResponse(w http.ResponseWriter) error    { return r.write(w) }
func (r problemResponse) VisitListConnectionsResponse(w http.ResponseWriter) error { return r.write(w) }
func (r problemResponse) VisitCreateConnectionResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r problemResponse) VisitDeleteConnectionResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r problemResponse) VisitGetConnectionResponse(w http.ResponseWriter) error { return r.write(w) }
func (r problemResponse) VisitUpdateConnectionResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r problemResponse) VisitListDelegationsResponse(w http.ResponseWriter) error { return r.write(w) }
func (r problemResponse) VisitCreateDelegationResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r problemResponse) VisitRevokeDelegationResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r problemResponse) VisitSubmitInvocationResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r problemResponse) VisitListMembershipsResponse(w http.ResponseWriter) error { return r.write(w) }
func (r problemResponse) VisitGrantMembershipResponse(w http.ResponseWriter) error { return r.write(w) }
func (r problemResponse) VisitRevokeMembershipResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r problemResponse) VisitListServiceAccountsResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r problemResponse) VisitCreateServiceAccountResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r problemResponse) VisitRevokeServiceAccountResponse(w http.ResponseWriter) error {
	return r.write(w)
}

func (service *Service) GetServerInfo(_ context.Context, _ GetServerInfoRequestObject) (GetServerInfoResponseObject, error) {
	features := cloneStrings(api.SupportedFeatures[:])
	return GetServerInfo200JSONResponse(api.ServerInfo{
		Features:         features,
		InstanceId:       service.instanceID,
		ProtocolMajor:    api.ProtocolMajor,
		ProtocolVersion:  api.ServerInfoProtocolVersion(api.WireVersion),
		RequiredFeatures: cloneStrings(features),
	}), nil
}
