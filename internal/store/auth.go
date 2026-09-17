package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

var (
	ErrAuthChallengeInvalid = errors.New("authentication challenge is invalid")
	ErrAuthChallengeExpired = errors.New("authentication challenge expired")
	ErrAuthChallengeUsed    = errors.New("authentication challenge already used")
	ErrAuthChallengePending = errors.New("authentication challenge is pending")
	ErrAuthChallengeDenied  = errors.New("authentication challenge was denied")
	ErrAuthPollTooSoon      = errors.New("authentication challenge polled too soon")
)

type AuthDeploymentTransactionFunc func(context.Context, *Transaction) error

// WithinAuthDeployment installs the deployment scope used only by pre-workspace
// authentication records. It does not install a tenant or workspace scope.
func WithinAuthDeployment(
	ctx context.Context,
	pool *Pool,
	deploymentID string,
	fn AuthDeploymentTransactionFunc,
) error {
	if !validStoreID(deploymentID) {
		return errors.New("authentication deployment identity is invalid")
	}
	if fn == nil {
		return errors.New("authentication transaction callback is required")
	}
	return WithinTransaction(ctx, pool, func(ctx context.Context, tx *Transaction) error {
		if err := BindAuthDeployment(ctx, tx, deploymentID); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

// BindAuthDeployment adds the deployment component to an existing transaction.
// IdentityService uses this with WithinScope so service credentials bind both
// deployment and tenant/workspace authority atomically.
func BindAuthDeployment(ctx context.Context, tx *Transaction, deploymentID string) error {
	if !validStoreID(deploymentID) {
		return errors.New("authentication deployment identity is invalid")
	}
	if _, err := tx.exec(ctx, `SELECT set_config('pm.deployment_id', $1, true)`, deploymentID); err != nil {
		return fmt.Errorf("install authentication deployment scope: %w", err)
	}
	return nil
}

type PrincipalTransactionFunc func(context.Context, *Transaction) error

// WithinPrincipalScope supports the one pre-workspace read: listing the
// authenticated principal's own memberships. RLS still limits rows to that
// organization and principal; no mutable current workspace is installed.
func WithinPrincipalScope(
	ctx context.Context,
	pool *Pool,
	principal contract.Principal,
	fn PrincipalTransactionFunc,
) error {
	if err := principal.Validate(); err != nil {
		return fmt.Errorf("validate principal transaction identity: %w", err)
	}
	if fn == nil {
		return errors.New("principal transaction callback is required")
	}
	return WithinTransaction(ctx, pool, func(ctx context.Context, tx *Transaction) error {
		if _, err := tx.exec(ctx, `
			SELECT set_config('pm.org_id', $1, true),
			       set_config('pm.workspace_id', '', true),
			       set_config('pm.requested_actor_id', $2, true),
			       set_config('pm.requested_actor_kind', $3, true),
			       set_config('pm.effective_actor_id', $2, true),
			       set_config('pm.effective_actor_kind', $3, true)
		`, principal.OrgID, principal.PrincipalID, string(principal.Kind)); err != nil {
			return fmt.Errorf("install principal transaction scope: %w", err)
		}
		tx.actor = contract.Actor{RequestedActor: principal, EffectiveActor: principal}
		return fn(ctx, tx)
	})
}

// RequireAuthorizationContext prevents an authorization decision from being
// evaluated against a transaction bound to a different workspace or actor.
func RequireAuthorizationContext(tx *Transaction, scope contract.Scope, actor contract.Actor) error {
	if tx == nil || !tx.scoped || !tx.scope.Equal(scope) || !tx.actor.Equal(actor) {
		return ErrScopeMismatch
	}
	return nil
}

// WithinAuthorizationActor temporarily installs another current actor while
// preserving the transaction's tenant and workspace. Delegation creation uses
// this bounded switch to evaluate the proposed effective user's current grant
// in the same transaction as the requested service account's parent grant.
func WithinAuthorizationActor(
	ctx context.Context,
	tx *Transaction,
	actor contract.Actor,
	fn ScopedTransactionFunc,
) (resultErr error) {
	if tx == nil || !tx.scoped {
		return ErrScopeMismatch
	}
	if err := actor.Validate(); err != nil ||
		actor.RequestedActor.OrgID != tx.scope.OrgID ||
		actor.EffectiveActor.OrgID != tx.scope.OrgID {
		return ErrScopeMismatch
	}
	if fn == nil {
		return errors.New("authorization actor callback is required")
	}
	originalActor := tx.actor
	if err := installAuthorizationActor(ctx, tx, actor); err != nil {
		return err
	}
	defer func() {
		restoreErr := installAuthorizationActor(ctx, tx, originalActor)
		if restoreErr != nil {
			resultErr = errors.Join(resultErr, restoreErr)
		}
	}()
	return fn(ctx, tx)
}

func installAuthorizationActor(ctx context.Context, tx *Transaction, actor contract.Actor) error {
	if _, err := tx.exec(ctx, `
		SELECT set_config('pm.requested_actor_id', $1, true),
		       set_config('pm.requested_actor_kind', $2, true),
		       set_config('pm.effective_actor_id', $3, true),
		       set_config('pm.effective_actor_kind', $4, true)
	`,
		actor.RequestedActor.PrincipalID,
		string(actor.RequestedActor.Kind),
		actor.EffectiveActor.PrincipalID,
		string(actor.EffectiveActor.Kind),
	); err != nil {
		return fmt.Errorf("install authorization actor: %w", err)
	}
	tx.actor = actor
	return nil
}

type SessionTransport string

const (
	SessionTransportBrowser SessionTransport = "browser"
	SessionTransportDevice  SessionTransport = "device"
)

type PMSessionRecord struct {
	DeploymentID     string
	SessionID        string
	Principal        contract.Principal
	Transport        SessionTransport
	TokenHash        []byte
	CSRFHash         []byte
	AuthenticatedAt  time.Time
	BootstrapIssuer  string
	BootstrapSubject string
	IssuedAt         time.Time
	ExpiresAt        time.Time
	RevokedAt        *time.Time
}

func (session PMSessionRecord) validate() error {
	if !validStoreID(session.DeploymentID) || !validStoreID(session.SessionID) {
		return errors.New("PM session deployment or session identity is invalid")
	}
	if err := session.Principal.Validate(); err != nil {
		return fmt.Errorf("validate PM session principal: %w", err)
	}
	if len(session.TokenHash) != 32 {
		return errors.New("PM session token hash must be SHA-256")
	}
	switch session.Transport {
	case SessionTransportBrowser:
		if len(session.CSRFHash) != 32 {
			return errors.New("browser PM session requires a SHA-256 CSRF hash")
		}
		if !session.AuthenticatedAt.IsZero() &&
			(session.AuthenticatedAt.Location() != time.UTC || session.AuthenticatedAt.After(session.IssuedAt)) {
			return errors.New("browser PM session authentication witness is invalid")
		}
		if (session.BootstrapIssuer == "") != (session.BootstrapSubject == "") ||
			session.BootstrapIssuer != "" &&
				(!validStoreID(session.BootstrapIssuer) || !validStoreID(session.BootstrapSubject)) {
			return errors.New("browser PM session bootstrap identity is invalid")
		}
	case SessionTransportDevice:
		if len(session.CSRFHash) != 0 || !session.AuthenticatedAt.IsZero() ||
			session.BootstrapIssuer != "" || session.BootstrapSubject != "" {
			return errors.New("device PM session cannot carry browser authentication material")
		}
	default:
		return errors.New("PM session transport is invalid")
	}
	if session.IssuedAt.IsZero() || session.ExpiresAt.IsZero() ||
		session.IssuedAt.Location() != time.UTC || session.ExpiresAt.Location() != time.UTC ||
		!session.ExpiresAt.After(session.IssuedAt) {
		return errors.New("PM session lifetime is invalid")
	}
	if session.RevokedAt != nil && (session.RevokedAt.IsZero() || session.RevokedAt.Location() != time.UTC) {
		return errors.New("PM session revocation time is invalid")
	}
	return nil
}

func (session *PMSessionRecord) normalizeTimes() {
	if !session.AuthenticatedAt.IsZero() {
		session.AuthenticatedAt = session.AuthenticatedAt.UTC().Truncate(time.Microsecond)
	}
	session.IssuedAt = session.IssuedAt.UTC().Truncate(time.Microsecond)
	session.ExpiresAt = session.ExpiresAt.UTC().Truncate(time.Microsecond)
	if session.RevokedAt != nil {
		value := session.RevokedAt.UTC().Truncate(time.Microsecond)
		session.RevokedAt = &value
	}
}

func InsertPMSession(ctx context.Context, tx *Transaction, session PMSessionRecord) error {
	session.normalizeTimes()
	if err := session.validate(); err != nil {
		return err
	}
	var authenticatedAt *time.Time
	if !session.AuthenticatedAt.IsZero() {
		value := session.AuthenticatedAt
		authenticatedAt = &value
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.pm_sessions (
			deployment_id, session_id, org_id, principal_id, principal_kind,
			transport, token_hash, csrf_hash, authenticated_at,
			bootstrap_issuer, bootstrap_subject, issued_at, expires_at, revoked_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''::bytea), $9,
			NULLIF($10, ''), NULLIF($11, ''), $12, $13, $14
		)
	`,
		session.DeploymentID,
		session.SessionID,
		session.Principal.OrgID,
		session.Principal.PrincipalID,
		string(session.Principal.Kind),
		string(session.Transport),
		session.TokenHash,
		session.CSRFHash,
		authenticatedAt,
		session.BootstrapIssuer,
		session.BootstrapSubject,
		session.IssuedAt,
		session.ExpiresAt,
		session.RevokedAt,
	); err != nil {
		return safeMetadataWriteError("insert PM session", err)
	}
	return nil
}

func LoadPMSessionByTokenHash(ctx context.Context, tx *Transaction, deploymentID string, tokenHash []byte) (PMSessionRecord, error) {
	if !validStoreID(deploymentID) || len(tokenHash) != 32 {
		return PMSessionRecord{}, ErrNotFound
	}
	session := PMSessionRecord{DeploymentID: deploymentID}
	var csrfHash []byte
	var authenticatedAt *time.Time
	if err := tx.queryRow(ctx, `
		SELECT session_id, org_id, principal_id, principal_kind, transport,
		       token_hash, COALESCE(csrf_hash, ''::bytea), authenticated_at,
		       COALESCE(bootstrap_issuer, ''), COALESCE(bootstrap_subject, ''),
		       issued_at, expires_at, revoked_at
		FROM pm.pm_sessions
		WHERE deployment_id = $1 AND token_hash = $2
		  AND revoked_at IS NULL AND expires_at > clock_timestamp()
	`, deploymentID, tokenHash).Scan(
		&session.SessionID,
		&session.Principal.OrgID,
		&session.Principal.PrincipalID,
		&session.Principal.Kind,
		&session.Transport,
		&session.TokenHash,
		&csrfHash,
		&authenticatedAt,
		&session.BootstrapIssuer,
		&session.BootstrapSubject,
		&session.IssuedAt,
		&session.ExpiresAt,
		&session.RevokedAt,
	); errors.Is(err, pgx.ErrNoRows) {
		return PMSessionRecord{}, ErrNotFound
	} else if err != nil {
		return PMSessionRecord{}, fmt.Errorf("load PM session: %w", err)
	}
	session.CSRFHash = csrfHash
	if authenticatedAt != nil {
		session.AuthenticatedAt = *authenticatedAt
	}
	session.normalizeTimes()
	if err := session.validate(); err != nil {
		return PMSessionRecord{}, fmt.Errorf("validate stored PM session: %w", err)
	}
	return session, nil
}

func RevokePMSession(
	ctx context.Context,
	tx *Transaction,
	deploymentID, sessionID string,
	principal contract.Principal,
) error {
	if !validStoreID(deploymentID) || !validStoreID(sessionID) || principal.Validate() != nil {
		return ErrNotFound
	}
	tag, err := tx.exec(ctx, `
		UPDATE pm.pm_sessions
		SET revoked_at = COALESCE(revoked_at, clock_timestamp())
		WHERE deployment_id = $1 AND session_id = $2
		  AND org_id = $3 AND principal_id = $4 AND principal_kind = $5
	`, deploymentID, sessionID, principal.OrgID, principal.PrincipalID, string(principal.Kind))
	if err != nil {
		return safeMetadataWriteError("revoke PM session", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

type BootstrapIdentity struct {
	DeploymentID   string
	Issuer         string
	Subject        string
	Principal      contract.Principal
	ApproveDevices bool
	Enabled        bool
}

func ResolveBootstrapIdentity(ctx context.Context, tx *Transaction, deploymentID, issuer, subject string) (BootstrapIdentity, error) {
	if !validStoreID(deploymentID) || !validStoreID(issuer) || !validStoreID(subject) {
		return BootstrapIdentity{}, ErrNotFound
	}
	identity := BootstrapIdentity{DeploymentID: deploymentID, Issuer: issuer, Subject: subject}
	if err := tx.queryRow(ctx, `
		SELECT org_id, principal_id, principal_kind, can_approve_devices, enabled
		FROM pm.auth_bootstrap_identities
		WHERE deployment_id = $1 AND issuer = $2 AND subject = $3
	`, deploymentID, issuer, subject).Scan(
		&identity.Principal.OrgID,
		&identity.Principal.PrincipalID,
		&identity.Principal.Kind,
		&identity.ApproveDevices,
		&identity.Enabled,
	); errors.Is(err, pgx.ErrNoRows) {
		return BootstrapIdentity{}, ErrNotFound
	} else if err != nil {
		return BootstrapIdentity{}, fmt.Errorf("resolve bootstrap identity: %w", err)
	}
	if err := identity.Principal.Validate(); err != nil {
		return BootstrapIdentity{}, fmt.Errorf("validate stored bootstrap principal: %w", err)
	}
	if !identity.Enabled {
		return BootstrapIdentity{}, ErrNotFound
	}
	return identity, nil
}

type OIDCLoginChallenge struct {
	DeploymentID              string
	ChallengeID               string
	StateHash                 []byte
	NonceHash                 []byte
	VerifierHash              []byte
	Origin                    string
	Host                      string
	ExpiresAt                 time.Time
	CreatedAt                 time.Time
	ReauthenticationRequested bool
	ClaimID                   string
}

func (challenge OIDCLoginChallenge) validate() error {
	if !validStoreID(challenge.DeploymentID) || !validStoreID(challenge.ChallengeID) ||
		!validStoreID(challenge.Origin) || !validStoreID(challenge.Host) {
		return errors.New("OIDC challenge identity or inbound binding is invalid")
	}
	if len(challenge.StateHash) != 32 || len(challenge.NonceHash) != 32 || len(challenge.VerifierHash) != 32 {
		return errors.New("OIDC challenge hashes must be SHA-256")
	}
	if challenge.ExpiresAt.IsZero() || challenge.ExpiresAt.Location() != time.UTC {
		return errors.New("OIDC challenge expiry is invalid")
	}
	if challenge.ClaimID != "" && !validStoreID(challenge.ClaimID) {
		return errors.New("OIDC challenge claim identity is invalid")
	}
	return nil
}

func InsertOIDCLoginChallenge(ctx context.Context, tx *Transaction, challenge OIDCLoginChallenge) error {
	challenge.ExpiresAt = challenge.ExpiresAt.UTC().Truncate(time.Microsecond)
	if err := challenge.validate(); err != nil {
		return err
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.oidc_login_challenges (
			deployment_id, challenge_id, state_hash, nonce_hash, verifier_hash,
			origin, host, expires_at, reauthentication_requested
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`,
		challenge.DeploymentID,
		challenge.ChallengeID,
		challenge.StateHash,
		challenge.NonceHash,
		challenge.VerifierHash,
		challenge.Origin,
		challenge.Host,
		challenge.ExpiresAt,
		challenge.ReauthenticationRequested,
	); err != nil {
		return safeMetadataWriteError("insert OIDC login challenge", err)
	}
	return nil
}

func ClaimOIDCLoginChallenge(
	ctx context.Context,
	tx *Transaction,
	deploymentID, challengeID string,
	stateHash, verifierHash []byte,
	origin, host, claimID string,
) (OIDCLoginChallenge, error) {
	if !validStoreID(deploymentID) || !validStoreID(challengeID) || !validStoreID(origin) ||
		!validStoreID(host) || !validStoreID(claimID) || len(stateHash) != 32 || len(verifierHash) != 32 {
		return OIDCLoginChallenge{}, ErrAuthChallengeInvalid
	}
	challenge := OIDCLoginChallenge{DeploymentID: deploymentID, ChallengeID: challengeID, ClaimID: claimID}
	if err := tx.queryRow(ctx, `
		UPDATE pm.oidc_login_challenges
		SET claim_id = $7, claimed_at = clock_timestamp()
		WHERE deployment_id = $1 AND challenge_id = $2
		  AND state_hash = $3 AND verifier_hash = $4
		  AND origin = $5 AND host = $6
		  AND expires_at > clock_timestamp()
		  AND claim_id IS NULL AND consumed_at IS NULL AND rejected_at IS NULL
		RETURNING state_hash, nonce_hash, verifier_hash, origin, host, expires_at,
		          created_at, reauthentication_requested
	`, deploymentID, challengeID, stateHash, verifierHash, origin, host, claimID).Scan(
		&challenge.StateHash,
		&challenge.NonceHash,
		&challenge.VerifierHash,
		&challenge.Origin,
		&challenge.Host,
		&challenge.ExpiresAt,
		&challenge.CreatedAt,
		&challenge.ReauthenticationRequested,
	); errors.Is(err, pgx.ErrNoRows) {
		return OIDCLoginChallenge{}, ErrAuthChallengeInvalid
	} else if err != nil {
		return OIDCLoginChallenge{}, fmt.Errorf("claim OIDC login challenge: %w", err)
	}
	challenge.ExpiresAt = challenge.ExpiresAt.UTC()
	challenge.CreatedAt = challenge.CreatedAt.UTC()
	return challenge, nil
}

func FinishOIDCLoginChallenge(
	ctx context.Context,
	tx *Transaction,
	deploymentID, challengeID, claimID string,
	session PMSessionRecord,
) error {
	if !validStoreID(deploymentID) || !validStoreID(challengeID) || !validStoreID(claimID) {
		return ErrAuthChallengeInvalid
	}
	tag, err := tx.exec(ctx, `
		UPDATE pm.oidc_login_challenges
		SET consumed_at = clock_timestamp()
		WHERE deployment_id = $1 AND challenge_id = $2 AND claim_id = $3
		  AND consumed_at IS NULL AND rejected_at IS NULL
	`, deploymentID, challengeID, claimID)
	if err != nil {
		return safeMetadataWriteError("consume OIDC login challenge", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrAuthChallengeUsed
	}
	return InsertPMSession(ctx, tx, session)
}

func RejectOIDCLoginChallenge(ctx context.Context, tx *Transaction, deploymentID, challengeID, claimID, reason string) error {
	if !validStoreID(deploymentID) || !validStoreID(challengeID) || !validStoreID(claimID) || !validStoreName(reason) {
		return ErrAuthChallengeInvalid
	}
	_, err := tx.exec(ctx, `
		UPDATE pm.oidc_login_challenges
		SET rejected_at = COALESCE(rejected_at, clock_timestamp()), rejection_reason = $4
		WHERE deployment_id = $1 AND challenge_id = $2 AND claim_id = $3 AND consumed_at IS NULL
	`, deploymentID, challengeID, claimID, reason)
	if err != nil {
		return safeMetadataWriteError("reject OIDC login challenge", err)
	}
	return nil
}

type DeviceChallengeState string

const (
	DeviceChallengePending  DeviceChallengeState = "pending"
	DeviceChallengeApproved DeviceChallengeState = "approved"
	DeviceChallengeDenied   DeviceChallengeState = "denied"
	DeviceChallengeExpired  DeviceChallengeState = "expired"
	DeviceChallengeConsumed DeviceChallengeState = "consumed"
)

type DeviceChallenge struct {
	DeploymentID string
	ChallengeID  string
	DeviceHash   []byte
	ClientHash   []byte
	ProofHash    []byte
	UserCodeHash []byte
	State        DeviceChallengeState
	PollInterval time.Duration
	ExpiresAt    time.Time
}

func (challenge DeviceChallenge) validate() error {
	if !validStoreID(challenge.DeploymentID) || !validStoreID(challenge.ChallengeID) {
		return errors.New("device challenge identity is invalid")
	}
	for _, digest := range [][]byte{challenge.DeviceHash, challenge.ClientHash, challenge.ProofHash, challenge.UserCodeHash} {
		if len(digest) != 32 {
			return errors.New("device challenge hashes must be SHA-256")
		}
	}
	if challenge.State != DeviceChallengePending || challenge.PollInterval < time.Second ||
		challenge.PollInterval%time.Second != 0 || challenge.ExpiresAt.IsZero() ||
		challenge.ExpiresAt.Location() != time.UTC {
		return errors.New("device challenge state, poll interval, or expiry is invalid")
	}
	return nil
}

func ReserveDeviceStart(
	ctx context.Context,
	tx *Transaction,
	deploymentID string,
	clientHash, deviceHash []byte,
	window time.Duration,
	maximum uint32,
) (bool, error) {
	if !validStoreID(deploymentID) || len(clientHash) != 32 || len(deviceHash) != 32 ||
		window < time.Second || window%time.Second != 0 || maximum == 0 {
		return false, errors.New("device start rate bound is invalid")
	}
	clientAllowed, err := reserveDeviceStartDimension(
		ctx,
		tx,
		deploymentID,
		"client",
		clientHash,
		window,
		maximum,
	)
	if err != nil {
		return false, err
	}
	deviceAllowed, err := reserveDeviceStartDimension(
		ctx,
		tx,
		deploymentID,
		"device",
		deviceHash,
		window,
		maximum,
	)
	if err != nil {
		return false, err
	}
	return clientAllowed && deviceAllowed, nil
}

func reserveDeviceStartDimension(
	ctx context.Context,
	tx *Transaction,
	deploymentID, dimensionKind string,
	dimensionHash []byte,
	window time.Duration,
	maximum uint32,
) (bool, error) {
	var allowed bool
	if err := tx.queryRow(ctx, `
		INSERT INTO pm.device_start_windows (
			deployment_id, dimension_kind, dimension_hash, window_started_at, starts, updated_at
		) VALUES ($1, $2, $3, clock_timestamp(), 1, clock_timestamp())
		ON CONFLICT (deployment_id, dimension_kind, dimension_hash) DO UPDATE
		SET starts = CASE
		        WHEN pm.device_start_windows.window_started_at + make_interval(secs => $4) <= clock_timestamp() THEN 1
		        ELSE LEAST(pm.device_start_windows.starts + 1, $5 + 1)
		    END,
		    window_started_at = CASE
		        WHEN pm.device_start_windows.window_started_at + make_interval(secs => $4) <= clock_timestamp() THEN clock_timestamp()
		        ELSE pm.device_start_windows.window_started_at
		    END,
		    updated_at = clock_timestamp()
		RETURNING starts <= $5
	`, deploymentID, dimensionKind, dimensionHash, int64(window/time.Second), int64(maximum)).Scan(&allowed); err != nil {
		return false, fmt.Errorf("reserve device login start: %w", err)
	}
	return allowed, nil
}

func InsertDeviceChallenge(ctx context.Context, tx *Transaction, challenge DeviceChallenge) error {
	challenge.ExpiresAt = challenge.ExpiresAt.UTC().Truncate(time.Microsecond)
	if err := challenge.validate(); err != nil {
		return err
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.device_login_challenges (
			deployment_id, challenge_id, device_hash, client_hash, proof_hash,
			user_code_hash, state, poll_interval_seconds, next_poll_at, expires_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8::bigint,
			clock_timestamp() + make_interval(secs => $8::bigint), $9
		)
	`,
		challenge.DeploymentID,
		challenge.ChallengeID,
		challenge.DeviceHash,
		challenge.ClientHash,
		challenge.ProofHash,
		challenge.UserCodeHash,
		string(challenge.State),
		int64(challenge.PollInterval/time.Second),
		challenge.ExpiresAt,
	); err != nil {
		return safeMetadataWriteError("insert device login challenge", err)
	}
	return nil
}

type BrowserDeviceDecision struct {
	DeploymentID string
	ChallengeID  string
	UserCodeHash []byte
	SessionID    string
	Principal    contract.Principal
}

func (decision BrowserDeviceDecision) validate() error {
	if !validStoreID(decision.DeploymentID) || !validStoreID(decision.ChallengeID) ||
		!validStoreID(decision.SessionID) || len(decision.UserCodeHash) != 32 ||
		decision.Principal.Validate() != nil {
		return ErrAuthChallengeInvalid
	}
	return nil
}

func ApproveDeviceChallengeFromBrowser(
	ctx context.Context,
	tx *Transaction,
	decision BrowserDeviceDecision,
) error {
	if err := decision.validate(); err != nil {
		return err
	}
	tag, err := tx.exec(ctx, `
		UPDATE pm.device_login_challenges AS challenge
		SET state = 'approved', approved_org_id = $4, approved_principal_id = $5,
		    approved_principal_kind = $6, decided_at = clock_timestamp()
		WHERE challenge.deployment_id = $1 AND challenge.challenge_id = $2
		  AND challenge.user_code_hash = $3 AND challenge.state = 'pending'
		  AND challenge.expires_at > clock_timestamp()
		  AND EXISTS (
		      SELECT 1
		      FROM pm.pm_sessions AS session
		      JOIN pm.auth_bootstrap_identities AS operator
		        ON operator.deployment_id = session.deployment_id
		       AND operator.issuer = session.bootstrap_issuer
		       AND operator.subject = session.bootstrap_subject
		      WHERE session.deployment_id = $1 AND session.session_id = $7
		        AND session.org_id = $4 AND session.principal_id = $5
		        AND session.principal_kind = $6 AND session.transport = 'browser'
		        AND session.revoked_at IS NULL AND session.expires_at > clock_timestamp()
		        AND operator.org_id = $4 AND operator.principal_id = $5
		        AND operator.principal_kind = $6
		        AND operator.enabled AND operator.can_approve_devices
		  )
	`,
		decision.DeploymentID,
		decision.ChallengeID,
		decision.UserCodeHash,
		decision.Principal.OrgID,
		decision.Principal.PrincipalID,
		string(decision.Principal.Kind),
		decision.SessionID,
	)
	if err != nil {
		return safeMetadataWriteError("approve device login challenge from browser session", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrAuthChallengeInvalid
	}
	return nil
}

func DenyDeviceChallengeFromBrowser(
	ctx context.Context,
	tx *Transaction,
	decision BrowserDeviceDecision,
) error {
	if err := decision.validate(); err != nil {
		return err
	}
	tag, err := tx.exec(ctx, `
		UPDATE pm.device_login_challenges AS challenge
		SET state = 'denied', decided_at = clock_timestamp()
		WHERE challenge.deployment_id = $1 AND challenge.challenge_id = $2
		  AND challenge.user_code_hash = $3
		  AND challenge.state = 'pending' AND challenge.expires_at > clock_timestamp()
		  AND EXISTS (
		      SELECT 1
		      FROM pm.pm_sessions AS session
		      JOIN pm.auth_bootstrap_identities AS operator
		        ON operator.deployment_id = session.deployment_id
		       AND operator.issuer = session.bootstrap_issuer
		       AND operator.subject = session.bootstrap_subject
		      WHERE session.deployment_id = $1 AND session.session_id = $7
		        AND session.org_id = $4 AND session.principal_id = $5
		        AND session.principal_kind = $6 AND session.transport = 'browser'
		        AND session.revoked_at IS NULL AND session.expires_at > clock_timestamp()
		        AND operator.org_id = $4 AND operator.principal_id = $5
		        AND operator.principal_kind = $6
		        AND operator.enabled AND operator.can_approve_devices
		  )
	`, decision.DeploymentID, decision.ChallengeID, decision.UserCodeHash,
		decision.Principal.OrgID, decision.Principal.PrincipalID,
		string(decision.Principal.Kind), decision.SessionID)
	if err != nil {
		return safeMetadataWriteError("deny device login challenge from browser session", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrAuthChallengeInvalid
	}
	return nil
}

type DeviceSessionPreparer func(contract.Principal) (PMSessionRecord, error)

func ExchangeDeviceChallenge(
	ctx context.Context,
	tx *Transaction,
	deploymentID, challengeID string,
	proofHash []byte,
	prepare DeviceSessionPreparer,
) error {
	if !validStoreID(deploymentID) || !validStoreID(challengeID) || len(proofHash) != 32 || prepare == nil {
		return ErrAuthChallengeInvalid
	}
	var state DeviceChallengeState
	var pollSeconds int64
	var nextPollAt, expiresAt time.Time
	var approvedOrgID, approvedPrincipalID, approvedPrincipalKind *string
	if err := tx.queryRow(ctx, `
		SELECT state, poll_interval_seconds, next_poll_at, expires_at,
		       approved_org_id, approved_principal_id, approved_principal_kind
		FROM pm.device_login_challenges
		WHERE deployment_id = $1 AND challenge_id = $2 AND proof_hash = $3
		FOR UPDATE
	`, deploymentID, challengeID, proofHash).Scan(
		&state,
		&pollSeconds,
		&nextPollAt,
		&expiresAt,
		&approvedOrgID,
		&approvedPrincipalID,
		&approvedPrincipalKind,
	); errors.Is(err, pgx.ErrNoRows) {
		return ErrAuthChallengeInvalid
	} else if err != nil {
		return fmt.Errorf("lock device login challenge: %w", err)
	}
	now, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return err
	}
	if !expiresAt.After(now) {
		if state == DeviceChallengePending || state == DeviceChallengeApproved {
			if _, updateErr := tx.exec(ctx, `
				UPDATE pm.device_login_challenges
				SET state = 'expired', decided_at = COALESCE(decided_at, $3),
				    approved_org_id = NULL, approved_principal_id = NULL,
				    approved_principal_kind = NULL
				WHERE deployment_id = $1 AND challenge_id = $2
			`, deploymentID, challengeID, now); updateErr != nil {
				return safeMetadataWriteError("expire device login challenge", updateErr)
			}
		}
		return ErrAuthChallengeExpired
	}
	if (state == DeviceChallengePending || state == DeviceChallengeApproved) && now.Before(nextPollAt) {
		return ErrAuthPollTooSoon
	}
	switch state {
	case DeviceChallengePending:
		if _, err := tx.exec(ctx, `
			UPDATE pm.device_login_challenges
			SET next_poll_at = $3::timestamptz + make_interval(secs => $4), poll_count = poll_count + 1
			WHERE deployment_id = $1 AND challenge_id = $2
		`, deploymentID, challengeID, now, pollSeconds); err != nil {
			return safeMetadataWriteError("advance device login poll cadence", err)
		}
		return ErrAuthChallengePending
	case DeviceChallengeDenied:
		return ErrAuthChallengeDenied
	case DeviceChallengeExpired:
		return ErrAuthChallengeExpired
	case DeviceChallengeConsumed:
		return ErrAuthChallengeUsed
	case DeviceChallengeApproved:
		if approvedOrgID == nil || approvedPrincipalID == nil || approvedPrincipalKind == nil {
			return errors.New("approved device challenge has no principal binding")
		}
		principal := contract.Principal{
			OrgID:       *approvedOrgID,
			PrincipalID: *approvedPrincipalID,
			Kind:        contract.PrincipalKind(*approvedPrincipalKind),
		}
		if err := principal.Validate(); err != nil {
			return ErrScopeMismatch
		}
		session, err := prepare(principal)
		if err != nil {
			return err
		}
		if session.DeploymentID != deploymentID || session.Transport != SessionTransportDevice ||
			!session.Principal.Equal(principal) {
			return ErrScopeMismatch
		}
		if err := InsertPMSession(ctx, tx, session); err != nil {
			return err
		}
		tag, err := tx.exec(ctx, `
			UPDATE pm.device_login_challenges
			SET state = 'consumed', consumed_at = $3
			WHERE deployment_id = $1 AND challenge_id = $2 AND state = 'approved'
		`, deploymentID, challengeID, now)
		if err != nil {
			return safeMetadataWriteError("consume device login challenge", err)
		}
		if tag.RowsAffected() != 1 {
			return ErrAuthChallengeUsed
		}
		return nil
	default:
		return errors.New("stored device challenge state is invalid")
	}
}

type ServiceCredentialRecord struct {
	DeploymentID     string
	CredentialID     string
	ServiceAccountID string
	Scope            contract.Scope
	SecretHash       []byte
	IssuedAt         time.Time
	ExpiresAt        time.Time
	RevokedAt        *time.Time
}

func (credential ServiceCredentialRecord) validate() error {
	if !validStoreID(credential.DeploymentID) || !validStoreID(credential.CredentialID) ||
		!validStoreID(credential.ServiceAccountID) || len(credential.SecretHash) != 32 {
		return errors.New("service credential identity or digest is invalid")
	}
	if err := credential.Scope.Validate(); err != nil {
		return fmt.Errorf("validate service credential scope: %w", err)
	}
	if credential.IssuedAt.IsZero() || credential.ExpiresAt.IsZero() ||
		credential.IssuedAt.Location() != time.UTC || credential.ExpiresAt.Location() != time.UTC ||
		!credential.ExpiresAt.After(credential.IssuedAt) {
		return errors.New("service credential lifetime is invalid")
	}
	if credential.RevokedAt != nil && (credential.RevokedAt.IsZero() || credential.RevokedAt.Location() != time.UTC) {
		return errors.New("service credential revocation time is invalid")
	}
	return nil
}

func LoadServiceCredential(
	ctx context.Context,
	tx *Transaction,
	deploymentID, credentialID string,
	secretHash []byte,
) (ServiceCredentialRecord, error) {
	if !validStoreID(deploymentID) || !validStoreID(credentialID) || len(secretHash) != 32 {
		return ServiceCredentialRecord{}, ErrNotFound
	}
	record := ServiceCredentialRecord{DeploymentID: deploymentID, CredentialID: credentialID}
	if err := tx.queryRow(ctx, `
		SELECT org_id, workspace_id, service_account_id, secret_hash, issued_at, expires_at, revoked_at
		FROM pm.service_account_credentials
		WHERE deployment_id = $1 AND credential_id = $2 AND secret_hash = $3
	`, deploymentID, credentialID, secretHash).Scan(
		&record.Scope.OrgID,
		&record.Scope.WorkspaceID,
		&record.ServiceAccountID,
		&record.SecretHash,
		&record.IssuedAt,
		&record.ExpiresAt,
		&record.RevokedAt,
	); errors.Is(err, pgx.ErrNoRows) {
		return ServiceCredentialRecord{}, ErrNotFound
	} else if err != nil {
		return ServiceCredentialRecord{}, fmt.Errorf("load service account credential: %w", err)
	}
	record.IssuedAt = record.IssuedAt.UTC()
	record.ExpiresAt = record.ExpiresAt.UTC()
	if record.RevokedAt != nil {
		value := record.RevokedAt.UTC()
		record.RevokedAt = &value
	}
	if err := record.validate(); err != nil {
		return ServiceCredentialRecord{}, fmt.Errorf("validate stored service credential: %w", err)
	}
	return record, nil
}

type MembershipAuthority struct {
	MembershipID    string
	Scope           contract.Scope
	Principal       contract.Principal
	Role            string
	ResourceGrants  []string
	Permissions     []string
	ConnectionIDs   []string
	OperationIDs    []string
	PolicyEpoch     uint64
	ControlRevision uint64
	ExpiresAt       *time.Time
	RevokedAt       *time.Time
}

type ServiceAccountAuthority struct {
	ServiceAccountID string
	Scope            contract.Scope
	Principal        contract.Principal
	DisplayName      *string
	Permissions      []string
	ConnectionIDs    []string
	OperationIDs     []string
	PolicyEpoch      uint64
	ControlRevision  uint64
	ExpiresAt        time.Time
	RevokedAt        *time.Time
}

type DelegationAuthority struct {
	DelegationID    string
	Scope           contract.Scope
	RequestedActor  contract.Principal
	EffectiveActor  contract.Principal
	ParentGrantRef  string
	ParentCeiling   contract.GrantCeiling
	ConnectionIDs   []string
	OperationIDs    []string
	PolicyEpoch     uint64
	ControlRevision uint64
	ExpiresAt       time.Time
	RevokedAt       *time.Time
	Hop             uint8
}

type ConnectionAuthority struct {
	Connection
	State       string
	DisplayName string
	RevokedAt   *time.Time
}

func LoadMembershipAuthority(ctx context.Context, tx *Transaction, principalID string) (MembershipAuthority, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return MembershipAuthority{}, err
	}
	if !validStoreID(principalID) {
		return MembershipAuthority{}, ErrNotFound
	}
	authority := MembershipAuthority{
		Scope:     scope,
		Principal: contract.Principal{OrgID: scope.OrgID, PrincipalID: principalID, Kind: contract.PrincipalUser},
	}
	if err := tx.queryRow(ctx, `
		SELECT membership_id, role, resource_grants, permissions, connection_ids, operation_ids,
		       policy_epoch, control_revision, expires_at, revoked_at
		FROM pm.memberships
		WHERE identity_id = $1
	`, principalID).Scan(
		&authority.MembershipID,
		&authority.Role,
		&authority.ResourceGrants,
		&authority.Permissions,
		&authority.ConnectionIDs,
		&authority.OperationIDs,
		&authority.PolicyEpoch,
		&authority.ControlRevision,
		&authority.ExpiresAt,
		&authority.RevokedAt,
	); errors.Is(err, pgx.ErrNoRows) {
		return MembershipAuthority{}, ErrNotFound
	} else if err != nil {
		return MembershipAuthority{}, fmt.Errorf("load membership authority: %w", err)
	}
	normalizeAuthorityTimes(&authority.ExpiresAt, &authority.RevokedAt)
	return authority, nil
}

func LoadMembershipByID(ctx context.Context, tx *Transaction, membershipID string) (MembershipAuthority, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return MembershipAuthority{}, err
	}
	if !validStoreID(membershipID) {
		return MembershipAuthority{}, ErrNotFound
	}
	authority := MembershipAuthority{Scope: scope, MembershipID: membershipID}
	if err := tx.queryRow(ctx, `
		SELECT identity_id, role, resource_grants, permissions, connection_ids, operation_ids,
		       policy_epoch, control_revision, expires_at, revoked_at
		FROM pm.memberships
		WHERE membership_id = $1
	`, membershipID).Scan(
		&authority.Principal.PrincipalID,
		&authority.Role,
		&authority.ResourceGrants,
		&authority.Permissions,
		&authority.ConnectionIDs,
		&authority.OperationIDs,
		&authority.PolicyEpoch,
		&authority.ControlRevision,
		&authority.ExpiresAt,
		&authority.RevokedAt,
	); errors.Is(err, pgx.ErrNoRows) {
		return MembershipAuthority{}, ErrNotFound
	} else if err != nil {
		return MembershipAuthority{}, fmt.Errorf("load membership by identity: %w", err)
	}
	authority.Principal.OrgID = scope.OrgID
	authority.Principal.Kind = contract.PrincipalUser
	normalizeAuthorityTimes(&authority.ExpiresAt, &authority.RevokedAt)
	return authority, nil
}

func LoadServiceAccountAuthority(ctx context.Context, tx *Transaction, serviceAccountID string) (ServiceAccountAuthority, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return ServiceAccountAuthority{}, err
	}
	if !validStoreID(serviceAccountID) {
		return ServiceAccountAuthority{}, ErrNotFound
	}
	authority := ServiceAccountAuthority{
		Scope:            scope,
		ServiceAccountID: serviceAccountID,
		Principal:        contract.Principal{OrgID: scope.OrgID, PrincipalID: serviceAccountID, Kind: contract.PrincipalServiceAccount},
	}
	if err := tx.queryRow(ctx, `
		SELECT display_name, permissions, connection_ids, operation_ids, policy_epoch,
		       control_revision, expires_at, revoked_at
		FROM pm.service_accounts
		WHERE service_account_id = $1
	`, serviceAccountID).Scan(
		&authority.DisplayName,
		&authority.Permissions,
		&authority.ConnectionIDs,
		&authority.OperationIDs,
		&authority.PolicyEpoch,
		&authority.ControlRevision,
		&authority.ExpiresAt,
		&authority.RevokedAt,
	); errors.Is(err, pgx.ErrNoRows) {
		return ServiceAccountAuthority{}, ErrNotFound
	} else if err != nil {
		return ServiceAccountAuthority{}, fmt.Errorf("load service account authority: %w", err)
	}
	authority.ExpiresAt = authority.ExpiresAt.UTC()
	if authority.RevokedAt != nil {
		value := authority.RevokedAt.UTC()
		authority.RevokedAt = &value
	}
	return authority, nil
}

func LoadDelegationAuthority(ctx context.Context, tx *Transaction, delegationID string) (DelegationAuthority, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return DelegationAuthority{}, err
	}
	if !validStoreID(delegationID) {
		return DelegationAuthority{}, ErrNotFound
	}
	authority := DelegationAuthority{Scope: scope, DelegationID: delegationID}
	if err := tx.queryRow(ctx, `
		SELECT requested_actor_id, requested_actor_kind, effective_actor_id, effective_actor_kind,
		       parent_grant_ref, parent_connection_ids, parent_operation_ids,
		       parent_approval_refs, parent_expires_at, connection_ids, operation_ids,
		       policy_epoch, control_revision, expires_at, revoked_at, hop
		FROM pm.delegations
		WHERE delegation_id = $1
	`, delegationID).Scan(
		&authority.RequestedActor.PrincipalID,
		&authority.RequestedActor.Kind,
		&authority.EffectiveActor.PrincipalID,
		&authority.EffectiveActor.Kind,
		&authority.ParentGrantRef,
		&authority.ParentCeiling.ConnectionIDs,
		&authority.ParentCeiling.OperationIDs,
		&authority.ParentCeiling.ApprovalRefs,
		&authority.ParentCeiling.ExpiresAt,
		&authority.ConnectionIDs,
		&authority.OperationIDs,
		&authority.PolicyEpoch,
		&authority.ControlRevision,
		&authority.ExpiresAt,
		&authority.RevokedAt,
		&authority.Hop,
	); errors.Is(err, pgx.ErrNoRows) {
		return DelegationAuthority{}, ErrNotFound
	} else if err != nil {
		return DelegationAuthority{}, fmt.Errorf("load delegation authority: %w", err)
	}
	authority.RequestedActor.OrgID = scope.OrgID
	authority.EffectiveActor.OrgID = scope.OrgID
	authority.ParentCeiling.Scope = scope
	authority.ParentCeiling.ExpiresAt = authority.ParentCeiling.ExpiresAt.UTC()
	authority.ExpiresAt = authority.ExpiresAt.UTC()
	if authority.RevokedAt != nil {
		value := authority.RevokedAt.UTC()
		authority.RevokedAt = &value
	}
	return authority, nil
}

func LoadConnectionAuthority(ctx context.Context, tx *Transaction, connectionID string) (ConnectionAuthority, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return ConnectionAuthority{}, err
	}
	if !validStoreID(connectionID) {
		return ConnectionAuthority{}, ErrNotFound
	}
	authority := ConnectionAuthority{Connection: Connection{Scope: scope, ConnectionID: connectionID}}
	if err := tx.queryRow(ctx, `
		SELECT connector_id, provider_account_id, revision, credential_ref,
		       state, COALESCE(display_name, ''), revoked_at
		FROM pm.connections
		WHERE connection_id = $1
	`, connectionID).Scan(
		&authority.ConnectorID,
		&authority.ProviderAccountID,
		&authority.Revision,
		&authority.CredentialRef,
		&authority.State,
		&authority.DisplayName,
		&authority.RevokedAt,
	); errors.Is(err, pgx.ErrNoRows) {
		return ConnectionAuthority{}, ErrNotFound
	} else if err != nil {
		return ConnectionAuthority{}, fmt.Errorf("load connection authority: %w", err)
	}
	if authority.RevokedAt != nil {
		value := authority.RevokedAt.UTC()
		authority.RevokedAt = &value
	}
	return authority, nil
}

type AuthorizedWorkspace struct {
	Scope              contract.Scope
	Name               string
	MembershipID       string
	Role               string
	MembershipRevision uint64
}

func ListAuthorizedWorkspaces(
	ctx context.Context,
	tx *Transaction,
	principal contract.Principal,
	afterWorkspaceID string,
	limit uint32,
) ([]AuthorizedWorkspace, error) {
	if err := principal.Validate(); err != nil {
		return nil, err
	}
	if principal.Kind != contract.PrincipalUser || limit == 0 || limit > 101 ||
		(afterWorkspaceID != "" && !validStoreID(afterWorkspaceID)) {
		return nil, errors.New("workspace list request is invalid")
	}
	rows, err := tx.query(ctx, `
		SELECT workspace.org_id, workspace.workspace_id, workspace.name,
		       membership.membership_id, membership.role, membership.control_revision
		FROM pm.memberships AS membership
		JOIN pm.workspace_namespaces AS workspace
		  ON workspace.org_id = membership.org_id AND workspace.workspace_id = membership.workspace_id
		WHERE membership.org_id = $1 AND membership.identity_id = $2
		  AND membership.revoked_at IS NULL
		  AND (membership.expires_at IS NULL OR membership.expires_at > clock_timestamp())
		  AND workspace.tombstoned_at IS NULL
		  AND workspace.workspace_id > $3
		ORDER BY workspace.workspace_id
		LIMIT $4
	`, principal.OrgID, principal.PrincipalID, afterWorkspaceID, int64(limit))
	if err != nil {
		return nil, fmt.Errorf("list authorized workspaces: %w", err)
	}
	defer rows.Close()
	result := make([]AuthorizedWorkspace, 0, limit)
	for rows.Next() {
		var workspace AuthorizedWorkspace
		if err := rows.Scan(
			&workspace.Scope.OrgID,
			&workspace.Scope.WorkspaceID,
			&workspace.Name,
			&workspace.MembershipID,
			&workspace.Role,
			&workspace.MembershipRevision,
		); err != nil {
			return nil, fmt.Errorf("scan authorized workspace: %w", err)
		}
		result = append(result, workspace)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate authorized workspaces: %w", err)
	}
	return result, nil
}

// ReadAuthenticationTime returns PostgreSQL's current time inside tx so
// authentication expiry decisions use the same clock as durable transitions.
func ReadAuthenticationTime(ctx context.Context, tx *Transaction) (time.Time, error) {
	if tx == nil {
		return time.Time{}, errors.New("authentication transaction is required")
	}
	return readDatabaseTime(ctx, tx)
}

// RevokePrincipalPMSessions revokes only login sessions. Accepted execution
// grants are immutable evidence and are deliberately outside this operation.
func RevokePrincipalPMSessions(
	ctx context.Context,
	tx *Transaction,
	deploymentID string,
	principal contract.Principal,
) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	if !validStoreID(deploymentID) || principal.Validate() != nil || principal.OrgID != scope.OrgID {
		return ErrScopeMismatch
	}
	if _, err := tx.exec(ctx, `
		UPDATE pm.pm_sessions
		SET revoked_at = COALESCE(revoked_at, clock_timestamp())
		WHERE deployment_id = $1 AND org_id = $2 AND principal_id = $3
		  AND principal_kind = $4 AND revoked_at IS NULL
	`, deploymentID, principal.OrgID, principal.PrincipalID, string(principal.Kind)); err != nil {
		return safeMetadataWriteError("revoke principal PM sessions", err)
	}
	return nil
}

// AuthorizeResourceExistence establishes only scoped resource existence. It
// never grants permission and must run after current authority is evaluated.
func AuthorizeResourceExistence(ctx context.Context, tx *Transaction, kind, resourceID, runID string) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	if !validStoreID(resourceID) || (kind != "event" && runID != "") {
		return ErrNotFound
	}
	switch kind {
	case "workspace":
		if resourceID != scope.WorkspaceID {
			return ErrNotFound
		}
		return requireActiveWorkspace(ctx, tx)
	case "connection":
		_, err := LoadConnectionAuthority(ctx, tx, resourceID)
		return err
	case "run":
		return requireScopedResource(ctx, tx, "SELECT true FROM pm.runs WHERE run_id = $1", resourceID)
	case "event":
		if !validStoreID(runID) {
			return ErrNotFound
		}
		return requireScopedResource(
			ctx,
			tx,
			"SELECT true FROM pm.server_events WHERE run_id = $1 AND event_id = $2",
			runID,
			resourceID,
		)
	case "artifact":
		return requireScopedResource(ctx, tx, "SELECT true FROM pm.artifacts WHERE artifact_id = $1", resourceID)
	default:
		return ErrNotFound
	}
}

func requireScopedResource(ctx context.Context, tx *Transaction, query string, arguments ...any) error {
	var exists bool
	if err := tx.queryRow(ctx, query, arguments...).Scan(&exists); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("load scoped authorization resource: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return nil
}

func normalizeAuthorityTimes(expiresAt, revokedAt **time.Time) {
	if *expiresAt != nil {
		value := (*expiresAt).UTC()
		*expiresAt = &value
	}
	if *revokedAt != nil {
		value := (*revokedAt).UTC()
		*revokedAt = &value
	}
}
