package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

var (
	ErrAuthenticationRequired = errors.New("authentication required")
	ErrAuthorizationDenied    = errors.New("authorization denied")
	ErrResourceNotFound       = errors.New("resource not found")
	ErrGrantUnavailable       = errors.New("authorization grant unavailable")
	ErrInvalidRequest         = errors.New("invalid authentication request")
	ErrInvalidInboundPolicy   = errors.New("inbound request policy denied")
	ErrInvalidCSRF            = errors.New("browser CSRF validation failed")
	ErrChallengeInvalid       = errors.New("login challenge is invalid")
	ErrChallengeExpired       = errors.New("login challenge expired")
	ErrChallengeUsed          = errors.New("login challenge already used")
	ErrChallengePending       = errors.New("login challenge is pending")
	ErrChallengeDenied        = errors.New("login challenge was denied")
	ErrPollTooSoon            = errors.New("login challenge polled too soon")
	ErrRateLimited            = errors.New("login initiation rate limit exceeded")
	ErrRequestTooLarge        = errors.New("login initiation request is too large")
	ErrOIDCVerification       = errors.New("OIDC verification failed")
	ErrDelegationInvalid      = errors.New("delegation is invalid")
)

const (
	maximumOpaqueInputBytes = 2048
	maximumTokenBytes       = 4096
)

type DevicePolicy struct {
	ChallengeTTL     time.Duration
	PollInterval     time.Duration
	StartWindow      time.Duration
	MaximumStarts    uint32
	MaximumBodyBytes int64
}

type SessionPolicy struct {
	BrowserTTL time.Duration
	DeviceTTL  time.Duration
}

type InboundPolicy struct {
	AllowedHosts   []string
	AllowedOrigins []string
}

type Config struct {
	Pool         *store.Pool
	DeploymentID string
	Random       io.Reader
	Device       DevicePolicy
	Sessions     SessionPolicy
	Inbound      InboundPolicy
	OIDC         OIDCConfig
}

type Service struct {
	pool         *store.Pool
	deploymentID string
	random       io.Reader
	device       DevicePolicy
	sessions     SessionPolicy
	inbound      InboundPolicy
	oidc         OIDCConfig
}

func NewService(configuration Config) (*Service, error) {
	if configuration.Pool == nil {
		return nil, errors.New("authentication store pool is required")
	}
	if !validOpaque(configuration.DeploymentID, 512) {
		return nil, errors.New("authentication deployment identity is invalid")
	}
	if configuration.Random == nil {
		configuration.Random = rand.Reader
	}
	if configuration.Device.ChallengeTTL <= 0 || configuration.Device.ChallengeTTL > 30*time.Minute ||
		configuration.Device.PollInterval < time.Second || configuration.Device.PollInterval > time.Minute ||
		configuration.Device.PollInterval%time.Second != 0 ||
		configuration.Device.StartWindow < time.Second || configuration.Device.StartWindow > time.Hour ||
		configuration.Device.StartWindow%time.Second != 0 ||
		configuration.Device.MaximumStarts == 0 || configuration.Device.MaximumStarts > 100 ||
		configuration.Device.MaximumBodyBytes <= 0 || configuration.Device.MaximumBodyBytes > 64*1024 {
		return nil, errors.New("device login bounds are invalid")
	}
	if configuration.Sessions.BrowserTTL <= 0 || configuration.Sessions.BrowserTTL > 24*time.Hour ||
		configuration.Sessions.DeviceTTL <= 0 || configuration.Sessions.DeviceTTL > 30*24*time.Hour {
		return nil, errors.New("PM session lifetime bounds are invalid")
	}
	inbound, err := validateInboundConfiguration(configuration.Inbound)
	if err != nil {
		return nil, err
	}
	if err := configuration.OIDC.validate(); err != nil {
		return nil, err
	}
	return &Service{
		pool:         configuration.Pool,
		deploymentID: configuration.DeploymentID,
		random:       configuration.Random,
		device:       configuration.Device,
		sessions:     configuration.Sessions,
		inbound:      inbound,
		oidc:         configuration.OIDC,
	}, nil
}

type AuthenticationSource string

const (
	AuthenticationBrowserSession    AuthenticationSource = "browser_session"
	AuthenticationDeviceSession     AuthenticationSource = "device_session"
	AuthenticationServiceCredential AuthenticationSource = "service_credential"
)

type AuthenticatedIdentity struct {
	Principal       contract.Principal
	Actor           contract.Actor
	Source          AuthenticationSource
	SessionID       string
	CredentialID    string
	AuthenticatedAt time.Time
	ExpiresAt       time.Time
}

func directActor(principal contract.Principal) contract.Actor {
	return contract.Actor{RequestedActor: principal, EffectiveActor: principal}
}

func randomOpaque(reader io.Reader, prefix string, byteCount int) (string, error) {
	if reader == nil || byteCount < 16 || byteCount > 64 || !validOpaque(prefix, 64) {
		return "", errors.New("opaque identity generator configuration is invalid")
	}
	value := make([]byte, byteCount)
	if _, err := io.ReadFull(reader, value); err != nil {
		return "", fmt.Errorf("generate opaque authentication identity: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(value), nil
}

func sha256Bytes(value string) []byte {
	digest := sha256.Sum256([]byte(value))
	return append([]byte(nil), digest[:]...)
}

func validOpaque(value string, maximum int) bool {
	if value == "" || len(value) > maximum || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validateInboundConfiguration(policy InboundPolicy) (InboundPolicy, error) {
	if len(policy.AllowedHosts) == 0 || len(policy.AllowedOrigins) == 0 ||
		len(policy.AllowedHosts) > 32 || len(policy.AllowedOrigins) > 32 {
		return InboundPolicy{}, errors.New("exact inbound hosts and origins are required")
	}
	seenHosts := make(map[string]struct{}, len(policy.AllowedHosts))
	for _, host := range policy.AllowedHosts {
		if !validExactHost(host) {
			return InboundPolicy{}, errors.New("configured inbound host is invalid")
		}
		if _, exists := seenHosts[host]; exists {
			return InboundPolicy{}, errors.New("configured inbound host is duplicated")
		}
		seenHosts[host] = struct{}{}
	}
	seenOrigins := make(map[string]struct{}, len(policy.AllowedOrigins))
	for _, origin := range policy.AllowedOrigins {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
			parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.String() != origin {
			return InboundPolicy{}, errors.New("configured browser origin must be an exact HTTPS origin")
		}
		if _, exists := seenOrigins[origin]; exists {
			return InboundPolicy{}, errors.New("configured browser origin is duplicated")
		}
		seenOrigins[origin] = struct{}{}
	}
	policy.AllowedHosts = append([]string(nil), policy.AllowedHosts...)
	policy.AllowedOrigins = append([]string(nil), policy.AllowedOrigins...)
	return policy, nil
}

func validExactHost(host string) bool {
	if !validOpaque(host, 512) || strings.ContainsAny(host, "/?#@") {
		return false
	}
	return true
}

func containsExact(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func mapStoreChallengeError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrAuthChallengeExpired):
		return ErrChallengeExpired
	case errors.Is(err, store.ErrAuthChallengeUsed):
		return ErrChallengeUsed
	case errors.Is(err, store.ErrAuthChallengePending):
		return ErrChallengePending
	case errors.Is(err, store.ErrAuthChallengeDenied):
		return ErrChallengeDenied
	case errors.Is(err, store.ErrAuthPollTooSoon):
		return ErrPollTooSoon
	case errors.Is(err, store.ErrAuthChallengeInvalid), errors.Is(err, store.ErrNotFound):
		return ErrChallengeInvalid
	default:
		return err
	}
}

func rejectNilContext(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidRequest
	}
	return nil
}
