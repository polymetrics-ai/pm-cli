package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

type SessionIssue struct {
	Principal        contract.Principal
	Transport        store.SessionTransport
	authenticatedAt  time.Time
	bootstrapIssuer  string
	bootstrapSubject string
}

type PMSession struct {
	SessionID       string
	Principal       contract.Principal
	Transport       store.SessionTransport
	AuthenticatedAt time.Time
	IssuedAt        time.Time
	ExpiresAt       time.Time
	Token           string `json:"-"`
	CSRFToken       string `json:"-"`
}

type SessionAuthentication struct {
	BearerToken     string
	BrowserCookie   string
	QueryCredential string
}

func (service *Service) IssuePMSession(ctx context.Context, issue SessionIssue) (PMSession, error) {
	if service == nil || service.pool == nil || rejectNilContext(ctx) != nil {
		return PMSession{}, ErrInvalidRequest
	}
	if err := issue.Principal.Validate(); err != nil {
		return PMSession{}, ErrInvalidRequest
	}
	var session PMSession
	err := store.WithinAuthDeployment(ctx, service.pool, service.deploymentID, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		session, err = service.issuePMSession(ctx, tx, issue)
		return err
	})
	if err != nil {
		return PMSession{}, err
	}
	return session, nil
}

func (service *Service) issuePMSession(ctx context.Context, tx *store.Transaction, issue SessionIssue) (PMSession, error) {
	session, record, err := service.preparePMSession(ctx, tx, issue)
	if err != nil {
		return PMSession{}, err
	}
	if err := store.InsertPMSession(ctx, tx, record); err != nil {
		return PMSession{}, err
	}
	return session, nil
}

func (service *Service) preparePMSession(
	ctx context.Context,
	tx *store.Transaction,
	issue SessionIssue,
) (PMSession, store.PMSessionRecord, error) {
	if err := issue.Principal.Validate(); err != nil {
		return PMSession{}, store.PMSessionRecord{}, ErrInvalidRequest
	}
	if (!issue.authenticatedAt.IsZero() || issue.bootstrapIssuer != "" || issue.bootstrapSubject != "") &&
		issue.Transport != store.SessionTransportBrowser ||
		!issue.authenticatedAt.IsZero() && issue.authenticatedAt.Location() != time.UTC ||
		(issue.bootstrapIssuer == "") != (issue.bootstrapSubject == "") {
		return PMSession{}, store.PMSessionRecord{}, ErrInvalidRequest
	}
	session, record, err := service.prepareSessionMaterial(ctx, tx, issue.Transport)
	if err != nil {
		return PMSession{}, store.PMSessionRecord{}, err
	}
	session.Principal = issue.Principal
	record.Principal = issue.Principal
	if !issue.authenticatedAt.IsZero() {
		session.AuthenticatedAt = issue.authenticatedAt.UTC().Truncate(time.Microsecond)
		record.AuthenticatedAt = session.AuthenticatedAt
	}
	record.BootstrapIssuer = issue.bootstrapIssuer
	record.BootstrapSubject = issue.bootstrapSubject
	return session, record, nil
}

func (service *Service) prepareSessionMaterial(
	ctx context.Context,
	tx *store.Transaction,
	transport store.SessionTransport,
) (PMSession, store.PMSessionRecord, error) {
	var lifetime time.Duration
	switch transport {
	case store.SessionTransportBrowser:
		lifetime = service.sessions.BrowserTTL
	case store.SessionTransportDevice:
		lifetime = service.sessions.DeviceTTL
	default:
		return PMSession{}, store.PMSessionRecord{}, ErrInvalidRequest
	}
	sessionID, err := randomOpaque(service.random, "session-", 18)
	if err != nil {
		return PMSession{}, store.PMSessionRecord{}, err
	}
	token, err := randomOpaque(service.random, "pm_session_", 32)
	if err != nil {
		return PMSession{}, store.PMSessionRecord{}, err
	}
	csrfToken := ""
	if transport == store.SessionTransportBrowser {
		csrfToken, err = randomOpaque(service.random, "pm_csrf_", 32)
		if err != nil {
			return PMSession{}, store.PMSessionRecord{}, err
		}
	}
	now, err := store.ReadAuthenticationTime(ctx, tx)
	if err != nil {
		return PMSession{}, store.PMSessionRecord{}, err
	}
	record := store.PMSessionRecord{
		DeploymentID: service.deploymentID,
		SessionID:    sessionID,
		Transport:    transport,
		TokenHash:    sha256Bytes(token),
		IssuedAt:     now,
		ExpiresAt:    now.Add(lifetime),
	}
	if csrfToken != "" {
		record.CSRFHash = sha256Bytes(csrfToken)
	}
	return PMSession{
		SessionID: sessionID,
		Transport: transport,
		IssuedAt:  record.IssuedAt,
		ExpiresAt: record.ExpiresAt,
		Token:     token,
		CSRFToken: csrfToken,
	}, record, nil
}

func (service *Service) Authenticate(ctx context.Context, input SessionAuthentication) (AuthenticatedIdentity, error) {
	if service == nil || service.pool == nil || rejectNilContext(ctx) != nil {
		return AuthenticatedIdentity{}, ErrAuthenticationRequired
	}
	if input.QueryCredential != "" {
		return AuthenticatedIdentity{}, ErrAuthenticationRequired
	}
	provided := 0
	token := ""
	expectedTransport := store.SessionTransport("")
	if input.BearerToken != "" {
		provided++
		token = input.BearerToken
		expectedTransport = store.SessionTransportDevice
	}
	if input.BrowserCookie != "" {
		provided++
		token = input.BrowserCookie
		expectedTransport = store.SessionTransportBrowser
	}
	if provided != 1 || !validOpaque(token, maximumTokenBytes) {
		return AuthenticatedIdentity{}, ErrAuthenticationRequired
	}
	var record store.PMSessionRecord
	err := store.WithinAuthDeployment(ctx, service.pool, service.deploymentID, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		record, err = store.LoadPMSessionByTokenHash(ctx, tx, service.deploymentID, sha256Bytes(token))
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return AuthenticatedIdentity{}, ErrAuthenticationRequired
		}
		return AuthenticatedIdentity{}, fmt.Errorf("authenticate PM session: %w", err)
	}
	if record.Transport != expectedTransport {
		return AuthenticatedIdentity{}, ErrAuthenticationRequired
	}
	source := AuthenticationDeviceSession
	if record.Transport == store.SessionTransportBrowser {
		source = AuthenticationBrowserSession
	}
	return AuthenticatedIdentity{
		Principal:       record.Principal,
		Actor:           directActor(record.Principal),
		Source:          source,
		SessionID:       record.SessionID,
		AuthenticatedAt: record.AuthenticatedAt,
		ExpiresAt:       record.ExpiresAt,
	}, nil
}

func (service *Service) RevokeSession(ctx context.Context, identity AuthenticatedIdentity) error {
	if service == nil || service.pool == nil || rejectNilContext(ctx) != nil ||
		!validOpaque(identity.SessionID, 512) || identity.Source == AuthenticationServiceCredential {
		return ErrAuthenticationRequired
	}
	if err := identity.Principal.Validate(); err != nil {
		return ErrAuthenticationRequired
	}
	return store.WithinPrincipalScope(ctx, service.pool, identity.Principal, func(ctx context.Context, tx *store.Transaction) error {
		if err := store.BindAuthDeployment(ctx, tx, service.deploymentID); err != nil {
			return err
		}
		return store.RevokePMSession(ctx, tx, service.deploymentID, identity.SessionID, identity.Principal)
	})
}
