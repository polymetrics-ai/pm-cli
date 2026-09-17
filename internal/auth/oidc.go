package auth

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"golang.org/x/oauth2"
)

type OIDCConfig struct {
	Issuer   string
	OAuth2   oauth2.Config
	KeySet   oidc.KeySet
	LoginTTL time.Duration
	verifier *oidc.IDTokenVerifier
}

func (configuration *OIDCConfig) validate() error {
	issuer, err := url.Parse(configuration.Issuer)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" || issuer.User != nil ||
		issuer.RawQuery != "" || issuer.Fragment != "" || issuer.String() != configuration.Issuer {
		return errors.New("OIDC issuer must be an exact HTTPS URL")
	}
	if configuration.KeySet == nil || !validOpaque(configuration.OAuth2.ClientID, 512) ||
		configuration.OAuth2.ClientID == "" || !slices.Contains(configuration.OAuth2.Scopes, oidc.ScopeOpenID) ||
		configuration.LoginTTL <= 0 || configuration.LoginTTL > 15*time.Minute {
		return errors.New("OIDC key set, client, openid scope, and bounded login lifetime are required")
	}
	for _, endpoint := range []string{
		configuration.OAuth2.Endpoint.AuthURL,
		configuration.OAuth2.Endpoint.TokenURL,
		configuration.OAuth2.RedirectURL,
	} {
		parsed, parseErr := url.Parse(endpoint)
		if parseErr != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
			return errors.New("OIDC authorization, token, and redirect endpoints must use HTTPS")
		}
	}
	configuration.verifier = oidc.NewVerifier(
		configuration.Issuer,
		configuration.KeySet,
		&oidc.Config{ClientID: configuration.OAuth2.ClientID},
	)
	return nil
}

type OIDCLoginStart struct {
	TLS            bool
	Origin         string
	Host           string
	Reauthenticate bool `json:"reauthenticate"`
}

type OIDCLoginChallenge struct {
	ChallengeID               string
	AuthorizationURL          string
	State                     string `json:"-"`
	Nonce                     string `json:"-"`
	PKCEVerifier              string `json:"-"`
	ExpiresAt                 time.Time
	ReauthenticationRequested bool
}

type OIDCLoginCallback struct {
	ChallengeID  string
	Code         string `json:"-"`
	State        string `json:"-"`
	PKCEVerifier string `json:"-"`
	TLS          bool
	Origin       string
	Host         string
}

type OIDCLoginResult struct {
	Session PMSession
	Cookie  BrowserCookie
}

func (service *Service) BeginOIDCLogin(ctx context.Context, request OIDCLoginStart) (OIDCLoginChallenge, error) {
	if service == nil || service.pool == nil || rejectNilContext(ctx) != nil {
		return OIDCLoginChallenge{}, ErrInvalidRequest
	}
	if err := service.ValidateInboundPolicy(InboundRequest{
		TLS: request.TLS, Host: request.Host, Origin: request.Origin,
		Browser: true, RequireOrigin: true,
	}); err != nil {
		return OIDCLoginChallenge{}, err
	}
	challengeID, err := randomOpaque(service.random, "oidc-", 18)
	if err != nil {
		return OIDCLoginChallenge{}, err
	}
	state, err := randomOpaque(service.random, "state-", 32)
	if err != nil {
		return OIDCLoginChallenge{}, err
	}
	nonce, err := randomOpaque(service.random, "nonce-", 32)
	if err != nil {
		return OIDCLoginChallenge{}, err
	}
	verifier := oauth2.GenerateVerifier()
	options := []oauth2.AuthCodeOption{
		oauth2.S256ChallengeOption(verifier),
		oidc.Nonce(nonce),
	}
	if request.Reauthenticate {
		options = append(
			options,
			oauth2.SetAuthURLParam("max_age", "0"),
			oauth2.SetAuthURLParam("prompt", "login"),
		)
	}
	challenge := OIDCLoginChallenge{
		ChallengeID:               challengeID,
		State:                     state,
		Nonce:                     nonce,
		PKCEVerifier:              verifier,
		ReauthenticationRequested: request.Reauthenticate,
		AuthorizationURL:          service.oidc.OAuth2.AuthCodeURL(state, options...),
	}
	err = store.WithinAuthDeployment(ctx, service.pool, service.deploymentID, func(ctx context.Context, tx *store.Transaction) error {
		now, timeErr := store.ReadAuthenticationTime(ctx, tx)
		if timeErr != nil {
			return timeErr
		}
		challenge.ExpiresAt = now.Add(service.oidc.LoginTTL)
		return store.InsertOIDCLoginChallenge(ctx, tx, store.OIDCLoginChallenge{
			DeploymentID:              service.deploymentID,
			ChallengeID:               challengeID,
			StateHash:                 sha256Bytes(state),
			NonceHash:                 sha256Bytes(nonce),
			VerifierHash:              sha256Bytes(verifier),
			Origin:                    request.Origin,
			Host:                      request.Host,
			ExpiresAt:                 challenge.ExpiresAt,
			ReauthenticationRequested: request.Reauthenticate,
		})
	})
	if err != nil {
		return OIDCLoginChallenge{}, fmt.Errorf("begin OIDC login: %w", err)
	}
	return challenge, nil
}

func (service *Service) CompleteOIDCLogin(ctx context.Context, callback OIDCLoginCallback) (OIDCLoginResult, error) {
	if service == nil || service.pool == nil || rejectNilContext(ctx) != nil ||
		!validOpaque(callback.ChallengeID, 512) || !validOpaque(callback.Code, maximumTokenBytes) ||
		!validOpaque(callback.State, maximumTokenBytes) || !validOpaque(callback.PKCEVerifier, maximumTokenBytes) {
		return OIDCLoginResult{}, ErrOIDCVerification
	}
	if err := service.ValidateInboundPolicy(InboundRequest{
		TLS: callback.TLS, Host: callback.Host, Origin: callback.Origin,
		Browser: true, RequireOrigin: true,
	}); err != nil {
		return OIDCLoginResult{}, ErrOIDCVerification
	}
	claimID, err := randomOpaque(service.random, "oidc-claim-", 18)
	if err != nil {
		return OIDCLoginResult{}, err
	}
	var challenge store.OIDCLoginChallenge
	err = store.WithinAuthDeployment(ctx, service.pool, service.deploymentID, func(ctx context.Context, tx *store.Transaction) error {
		var claimErr error
		challenge, claimErr = store.ClaimOIDCLoginChallenge(
			ctx,
			tx,
			service.deploymentID,
			callback.ChallengeID,
			sha256Bytes(callback.State),
			sha256Bytes(callback.PKCEVerifier),
			callback.Origin,
			callback.Host,
			claimID,
		)
		return claimErr
	})
	if err != nil {
		return OIDCLoginResult{}, ErrOIDCVerification
	}

	reject := func(reason string) {
		rejectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = store.WithinAuthDeployment(rejectCtx, service.pool, service.deploymentID, func(rejectCtx context.Context, tx *store.Transaction) error {
			return store.RejectOIDCLoginChallenge(rejectCtx, tx, service.deploymentID, callback.ChallengeID, claimID, reason)
		})
	}
	oauthToken, err := service.oidc.OAuth2.Exchange(ctx, callback.Code, oauth2.VerifierOption(callback.PKCEVerifier))
	if err != nil {
		reject("exchange_failed")
		return OIDCLoginResult{}, ErrOIDCVerification
	}
	rawIDToken, ok := oauthToken.Extra("id_token").(string)
	if !ok || !validOpaque(rawIDToken, 64*1024) {
		reject("id_token_missing")
		return OIDCLoginResult{}, ErrOIDCVerification
	}
	verified, err := service.oidc.verifier.Verify(ctx, rawIDToken)
	if err != nil || verified.Issuer != service.oidc.Issuer || verified.Subject == "" ||
		subtle.ConstantTimeCompare(challenge.NonceHash, sha256Bytes(verified.Nonce)) != 1 {
		reject("id_token_invalid")
		return OIDCLoginResult{}, ErrOIDCVerification
	}
	authenticatedAt, hasAuthenticatedAt, err := verifiedOIDCAuthenticationTime(verified, time.Now().UTC())
	if err != nil ||
		challenge.ReauthenticationRequested &&
			(!hasAuthenticatedAt || authenticatedAt.Before(challenge.CreatedAt.Truncate(time.Second))) {
		reject("auth_time_invalid")
		return OIDCLoginResult{}, ErrOIDCVerification
	}

	var result OIDCLoginResult
	err = store.WithinAuthDeployment(ctx, service.pool, service.deploymentID, func(ctx context.Context, tx *store.Transaction) error {
		binding, resolveErr := store.ResolveBootstrapIdentity(
			ctx, tx, service.deploymentID, verified.Issuer, verified.Subject,
		)
		if resolveErr != nil {
			return resolveErr
		}
		session, record, prepareErr := service.preparePMSession(ctx, tx, SessionIssue{
			Principal:        binding.Principal,
			Transport:        store.SessionTransportBrowser,
			authenticatedAt:  authenticatedAt,
			bootstrapIssuer:  verified.Issuer,
			bootstrapSubject: verified.Subject,
		})
		if prepareErr != nil {
			return prepareErr
		}
		if finishErr := store.FinishOIDCLoginChallenge(
			ctx,
			tx,
			service.deploymentID,
			callback.ChallengeID,
			claimID,
			record,
		); finishErr != nil {
			return finishErr
		}
		cookie, cookieErr := NewBrowserCookie(session)
		if cookieErr != nil {
			return cookieErr
		}
		result = OIDCLoginResult{Session: session, Cookie: cookie}
		return nil
	})
	if err != nil {
		reject("identity_unavailable")
		return OIDCLoginResult{}, ErrOIDCVerification
	}
	return result, nil
}

type oidcAuthenticationClaims struct {
	AuthTime json.RawMessage `json:"auth_time"`
}

func verifiedOIDCAuthenticationTime(
	token *oidc.IDToken,
	now time.Time,
) (time.Time, bool, error) {
	var claims oidcAuthenticationClaims
	if err := token.Claims(&claims); err != nil {
		return time.Time{}, false, err
	}
	if len(claims.AuthTime) == 0 {
		return time.Time{}, false, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(claims.AuthTime))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return time.Time{}, false, err
	}
	number, ok := value.(json.Number)
	if !ok {
		return time.Time{}, false, errors.New("OIDC auth_time must be a numeric date")
	}
	seconds, err := strconv.ParseFloat(number.String(), 64)
	nowSeconds := float64(now.Unix()) + float64(now.Nanosecond())/float64(time.Second)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 || seconds > nowSeconds {
		return time.Time{}, false, errors.New("OIDC auth_time is invalid or in the future")
	}
	wholeSeconds, fractionalSeconds := math.Modf(seconds)
	authenticatedAt := time.Unix(
		int64(wholeSeconds),
		int64(fractionalSeconds*float64(time.Second)),
	).UTC()
	if authenticatedAt.After(now) {
		return time.Time{}, false, errors.New("OIDC auth_time is in the future")
	}
	return authenticatedAt, true, nil
}
