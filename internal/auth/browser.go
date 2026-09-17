package auth

import (
	"context"
	"crypto/subtle"
	"errors"

	"github.com/polymetrics-ai/pm-cli/internal/store"
)

const BrowserSessionCookieName = "__Host-pm_session"

type BrowserCookie struct {
	Name     string
	Value    string `json:"-"`
	Path     string
	Secure   bool
	HTTPOnly bool
	SameSite string
}

type BrowserCSRFRequest struct {
	CookieToken string
	CSRFToken   string
}

func NewBrowserCookie(session PMSession) (BrowserCookie, error) {
	if session.Transport != store.SessionTransportBrowser || !validOpaque(session.Token, maximumTokenBytes) {
		return BrowserCookie{}, ErrInvalidRequest
	}
	return BrowserCookie{
		Name:     BrowserSessionCookieName,
		Value:    session.Token,
		Path:     "/",
		Secure:   true,
		HTTPOnly: true,
		SameSite: "Lax",
	}, nil
}

// ValidateCSRF authenticates the cookie session and then compares the submitted
// token with the session-bound digest. Device bearer material can never satisfy
// this browser mutation check.
func (service *Service) ValidateCSRF(ctx context.Context, request BrowserCSRFRequest) (AuthenticatedIdentity, error) {
	if service == nil || rejectNilContext(ctx) != nil ||
		!validOpaque(request.CookieToken, maximumTokenBytes) || !validOpaque(request.CSRFToken, maximumTokenBytes) {
		return AuthenticatedIdentity{}, ErrInvalidCSRF
	}
	var record store.PMSessionRecord
	err := store.WithinAuthDeployment(ctx, service.pool, service.deploymentID, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		record, err = store.LoadPMSessionByTokenHash(ctx, tx, service.deploymentID, sha256Bytes(request.CookieToken))
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return AuthenticatedIdentity{}, ErrInvalidCSRF
		}
		return AuthenticatedIdentity{}, err
	}
	if record.Transport != store.SessionTransportBrowser || len(record.CSRFHash) != 32 ||
		subtle.ConstantTimeCompare(record.CSRFHash, sha256Bytes(request.CSRFToken)) != 1 {
		return AuthenticatedIdentity{}, ErrInvalidCSRF
	}
	return AuthenticatedIdentity{
		Principal:       record.Principal,
		Actor:           directActor(record.Principal),
		Source:          AuthenticationBrowserSession,
		SessionID:       record.SessionID,
		AuthenticatedAt: record.AuthenticatedAt,
		ExpiresAt:       record.ExpiresAt,
	}, nil
}
