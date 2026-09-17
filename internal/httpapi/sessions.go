package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/oapi-codegen/nullable"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
)

type oidcCookieState struct {
	ChallengeID  string `json:"challenge_id"`
	PKCEVerifier string `json:"pkce_verifier"`
}

func (service *Service) StartBrowserLogin(ctx context.Context, request StartBrowserLoginRequestObject) (StartBrowserLoginResponseObject, error) {
	state, ok := stateFromContext(ctx)
	if !ok || request.Body == nil {
		return invalidRequestResponse(), nil
	}
	challenge, err := service.auth.BeginOIDCLogin(ctx, auth.OIDCLoginStart{
		TLS:            state.inbound.TLS,
		Origin:         state.inbound.Origin,
		Host:           state.inbound.Host,
		Reauthenticate: request.Body.Reauthenticate,
	})
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	cookieValue, err := encodeOIDCCookie(oidcCookieState{
		ChallengeID:  challenge.ChallengeID,
		PKCEVerifier: challenge.PKCEVerifier,
	})
	if err != nil {
		return nil, err
	}
	setCookie(ctx, &http.Cookie{
		Name:     oidcChallengeCookie,
		Value:    cookieValue,
		Path:     "/",
		Expires:  challenge.ExpiresAt,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	return StartBrowserLogin201JSONResponse(api.BrowserLoginChallenge{
		AuthorizationUrl: challenge.AuthorizationURL,
		ExpiresAt:        challenge.ExpiresAt,
	}), nil
}

func (service *Service) CompleteBrowserLogin(ctx context.Context, request CompleteBrowserLoginRequestObject) (CompleteBrowserLoginResponseObject, error) {
	state, ok := stateFromContext(ctx)
	if !ok || request.Body == nil {
		return invalidRequestResponse(), nil
	}
	cookieValue, present, err := cookieFromContext(ctx, oidcChallengeCookie)
	clearOIDCCookie(ctx)
	if err != nil || !present {
		return asProblemResponse(auth.ErrChallengeInvalid, nil), nil
	}
	cookieState, err := decodeOIDCCookie(cookieValue)
	if err != nil {
		return asProblemResponse(auth.ErrChallengeInvalid, nil), nil
	}
	oauthContext := oidc.ClientContext(ctx, service.oidcHTTPClient)
	result, err := service.auth.CompleteOIDCLogin(oauthContext, auth.OIDCLoginCallback{
		ChallengeID:  cookieState.ChallengeID,
		Code:         request.Body.Code,
		State:        request.Body.State,
		PKCEVerifier: cookieState.PKCEVerifier,
		TLS:          state.inbound.TLS,
		Origin:       state.inbound.Origin,
		Host:         state.inbound.Host,
	})
	if err != nil {
		outcome := api.SafeOutcomeReference{Kind: api.SafeOutcomeReferenceKindRequest, Ref: cookieState.ChallengeID}
		return asProblemResponse(err, &outcome), nil
	}
	if result.Cookie.Name != browserSessionCookie || result.Cookie.Path != "/" || !result.Cookie.Secure ||
		!result.Cookie.HTTPOnly || result.Cookie.SameSite != "Lax" || result.Session.CSRFToken == "" {
		return nil, errors.New("browser authentication returned invalid cookie material")
	}
	setCookie(ctx, &http.Cookie{
		Name:     result.Cookie.Name,
		Value:    result.Cookie.Value,
		Path:     result.Cookie.Path,
		Expires:  result.Session.ExpiresAt,
		Secure:   result.Cookie.Secure,
		HttpOnly: result.Cookie.HTTPOnly,
		SameSite: http.SameSiteLaxMode,
	})
	setCookie(ctx, &http.Cookie{
		Name:     browserCSRFCookie,
		Value:    result.Session.CSRFToken,
		Path:     "/",
		Expires:  result.Session.ExpiresAt,
		Secure:   true,
		HttpOnly: false,
		SameSite: http.SameSiteStrictMode,
	})
	return CompleteBrowserLogin204Response{}, nil
}

func (service *Service) StartDeviceLogin(ctx context.Context, request StartDeviceLoginRequestObject) (StartDeviceLoginResponseObject, error) {
	state, ok := stateFromContext(ctx)
	if !ok || request.Body == nil {
		return invalidRequestResponse(), nil
	}
	requestBytes := int64(0)
	if state.request != nil {
		requestBytes = state.request.ContentLength
	}
	if requestBytes < 0 {
		requestBytes = 0
	}
	challenge, err := service.auth.StartDeviceLogin(ctx, auth.DeviceStart{
		ClientIP:     state.inbound.ClientIP,
		DeviceID:     request.Body.DeviceId,
		RequestBytes: requestBytes,
	})
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	pollSeconds := int(challenge.PollInterval / time.Second)
	if pollSeconds <= 0 || time.Duration(pollSeconds)*time.Second != challenge.PollInterval {
		return nil, errors.New("device authentication returned invalid poll interval")
	}
	return StartDeviceLogin201JSONResponse(api.DeviceChallenge{
		ChallengeId:         challenge.ChallengeID,
		DeviceProof:         challenge.DeviceProof,
		ExpiresAt:           challenge.ExpiresAt,
		PollIntervalSeconds: pollSeconds,
		UserCode:            challenge.UserCode,
		VerificationUri:     strings.TrimSuffix(service.publicOrigin, "/") + "/login/device",
	}), nil
}

func (service *Service) ApproveDeviceLogin(ctx context.Context, request ApproveDeviceLoginRequestObject) (ApproveDeviceLoginResponseObject, error) {
	state, err := authenticatedState(ctx)
	if err != nil || request.Body == nil || state.identity.Source != auth.AuthenticationBrowserSession {
		if err == nil {
			err = auth.ErrAuthenticationRequired
		}
		return asProblemResponse(err, nil), nil
	}
	err = service.auth.ApproveDeviceLoginFromBrowser(ctx, auth.BrowserDeviceDecision{
		ChallengeID: request.Body.ChallengeId,
		UserCode:    request.Body.UserCode,
		CookieToken: state.sessionCookie,
		CSRFToken:   state.csrfToken,
	})
	if err != nil {
		outcome := api.SafeOutcomeReference{Kind: api.SafeOutcomeReferenceKindRequest, Ref: request.Body.ChallengeId}
		return asProblemResponse(err, &outcome), nil
	}
	return ApproveDeviceLogin204Response{}, nil
}

func (service *Service) DenyDeviceLogin(ctx context.Context, request DenyDeviceLoginRequestObject) (DenyDeviceLoginResponseObject, error) {
	state, err := authenticatedState(ctx)
	if err != nil || request.Body == nil || state.identity.Source != auth.AuthenticationBrowserSession {
		if err == nil {
			err = auth.ErrAuthenticationRequired
		}
		return asProblemResponse(err, nil), nil
	}
	err = service.auth.DenyDeviceLoginFromBrowser(ctx, auth.BrowserDeviceDecision{
		ChallengeID: request.Body.ChallengeId,
		UserCode:    request.Body.UserCode,
		CookieToken: state.sessionCookie,
		CSRFToken:   state.csrfToken,
	})
	if err != nil {
		outcome := api.SafeOutcomeReference{Kind: api.SafeOutcomeReferenceKindRequest, Ref: request.Body.ChallengeId}
		return asProblemResponse(err, &outcome), nil
	}
	return DenyDeviceLogin204Response{}, nil
}

func (service *Service) ExchangeDeviceLogin(ctx context.Context, request ExchangeDeviceLoginRequestObject) (ExchangeDeviceLoginResponseObject, error) {
	if request.Body == nil {
		return invalidRequestResponse(), nil
	}
	session, err := service.auth.ExchangeDeviceProof(ctx, auth.DeviceProof{
		ChallengeID: request.Body.ChallengeId,
		Proof:       request.Body.Proof,
	})
	if err != nil {
		outcome := api.SafeOutcomeReference{Kind: api.SafeOutcomeReferenceKindRequest, Ref: request.Body.ChallengeId}
		return asProblemResponse(err, &outcome), nil
	}
	return ExchangeDeviceLogin200JSONResponse(api.PMDeviceSession{
		ExpiresAt:           session.ExpiresAt,
		PmSessionCredential: session.Token,
		Principal:           principalResponse(session.Principal),
		SessionId:           session.SessionID,
	}), nil
}

func (service *Service) RevokeCurrentSession(ctx context.Context, _ RevokeCurrentSessionRequestObject) (RevokeCurrentSessionResponseObject, error) {
	state, err := authenticatedState(ctx)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if err := service.auth.RevokeSession(ctx, state.identity); err != nil {
		return asProblemResponse(err, nil), nil
	}
	if state.identity.Source == auth.AuthenticationBrowserSession {
		clearBrowserCookies(ctx)
	}
	return RevokeCurrentSession204Response{}, nil
}

func (service *Service) GetCurrentSession(ctx context.Context, request GetCurrentSessionRequestObject) (GetCurrentSessionResponseObject, error) {
	state, err := authenticatedState(ctx)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if state.identity.Source == auth.AuthenticationServiceCredential {
		return asProblemResponse(auth.ErrAuthenticationRequired, nil), nil
	}
	afterID, pageSize, err := service.parsePage("current_session_memberships", state.identity.Principal, contractScopeNone(), request.Params.Cursor, request.Params.PageSize)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	page, err := service.identity.ListWorkspaces(ctx, state.identity.Principal, afterID, pageSize)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	next, err := service.nextCursor("current_session_memberships", state.identity.Principal, contractScopeNone(), pageSize, page.NextCursor)
	if err != nil {
		return nil, err
	}
	items := make([]api.Workspace, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, workspaceResponse(item))
	}
	authenticatedAt := nullable.NewNullNullable[time.Time]()
	if !state.identity.AuthenticatedAt.IsZero() {
		authenticatedAt = nullable.NewNullableWithValue(state.identity.AuthenticatedAt)
	}
	transport := api.DeviceSession
	if state.identity.Source == auth.AuthenticationBrowserSession {
		transport = api.BrowserSession
	}
	return GetCurrentSession200JSONResponse(api.CurrentSession{
		AuthenticatedAt: authenticatedAt,
		ExpiresAt:       state.identity.ExpiresAt,
		Memberships:     api.WorkspacePage{Items: items, NextCursor: next},
		Principal:       principalResponse(state.identity.Principal),
		Transport:       transport,
	}), nil
}

func encodeOIDCCookie(value oidcCookieState) (string, error) {
	if !validHTTPIdentifier(value.ChallengeID, 512) || !validHTTPIdentifier(value.PKCEVerifier, 8192) {
		return "", errors.New("OIDC challenge cookie material is invalid")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeOIDCCookie(value string) (oidcCookieState, error) {
	if value == "" || len(value) > 16384 {
		return oidcCookieState{}, errors.New("OIDC challenge cookie is invalid")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return oidcCookieState{}, errors.New("OIDC challenge cookie is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(decoded))
	decoder.DisallowUnknownFields()
	var result oidcCookieState
	if err := decoder.Decode(&result); err != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		!validHTTPIdentifier(result.ChallengeID, 512) || !validHTTPIdentifier(result.PKCEVerifier, 8192) {
		return oidcCookieState{}, errors.New("OIDC challenge cookie is invalid")
	}
	return result, nil
}

func cookieFromContext(ctx context.Context, name string) (string, bool, error) {
	state, ok := stateFromContext(ctx)
	if !ok || state.request == nil {
		return "", false, errors.New("HTTP request context is unavailable")
	}
	return oneCookie(state.request, name)
}

func setCookie(ctx context.Context, cookie *http.Cookie) {
	if state, ok := stateFromContext(ctx); ok && state.writer != nil {
		http.SetCookie(state.writer, cookie)
	}
}

func clearOIDCCookie(ctx context.Context) {
	setCookie(ctx, &http.Cookie{
		Name:     oidcChallengeCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(1, 0).UTC(),
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearBrowserCookies(ctx context.Context) {
	for _, cookie := range []*http.Cookie{
		{Name: browserSessionCookie, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode},
		{Name: browserCSRFCookie, Path: "/", Secure: true, HttpOnly: false, SameSite: http.SameSiteStrictMode},
	} {
		cookie.Value = ""
		cookie.MaxAge = -1
		cookie.Expires = time.Unix(1, 0).UTC()
		setCookie(ctx, cookie)
	}
}
