package auth

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

type DeviceStart struct {
	ClientIP     string
	DeviceID     string
	RequestBytes int64
}

type DeviceChallenge struct {
	ChallengeID  string
	UserCode     string
	DeviceProof  string `json:"-"`
	ExpiresAt    time.Time
	PollInterval time.Duration
}

type BrowserDeviceDecision struct {
	ChallengeID string `json:"challenge_id"`
	UserCode    string `json:"user_code"`
	CookieToken string `json:"-"`
	CSRFToken   string `json:"-"`
}

type DeviceProof struct {
	ChallengeID string
	Proof       string `json:"-"`
}

func (service *Service) StartDeviceLogin(ctx context.Context, request DeviceStart) (DeviceChallenge, error) {
	if service == nil || service.pool == nil || rejectNilContext(ctx) != nil {
		return DeviceChallenge{}, ErrInvalidRequest
	}
	if request.RequestBytes < 0 || request.RequestBytes > service.device.MaximumBodyBytes {
		return DeviceChallenge{}, ErrRequestTooLarge
	}
	clientIP, err := netip.ParseAddr(request.ClientIP)
	if err != nil || !validOpaque(request.DeviceID, 512) {
		return DeviceChallenge{}, ErrInvalidRequest
	}
	canonicalClientIP := clientIP.Unmap().String()
	challengeID, err := randomOpaque(service.random, "device-", 18)
	if err != nil {
		return DeviceChallenge{}, err
	}
	userCode, err := randomOpaque(service.random, "PM-", 16)
	if err != nil {
		return DeviceChallenge{}, err
	}
	proof, err := randomOpaque(service.random, "pm_device_", 32)
	if err != nil {
		return DeviceChallenge{}, err
	}
	result := DeviceChallenge{
		ChallengeID:  challengeID,
		UserCode:     userCode,
		DeviceProof:  proof,
		PollInterval: service.device.PollInterval,
	}
	allowed := false
	err = store.WithinAuthDeployment(ctx, service.pool, service.deploymentID, func(ctx context.Context, tx *store.Transaction) error {
		var reserveErr error
		allowed, reserveErr = store.ReserveDeviceStart(
			ctx,
			tx,
			service.deploymentID,
			sha256Bytes("client\x00"+canonicalClientIP),
			sha256Bytes("device\x00"+request.DeviceID),
			service.device.StartWindow,
			service.device.MaximumStarts,
		)
		if reserveErr != nil || !allowed {
			return reserveErr
		}
		now, timeErr := store.ReadAuthenticationTime(ctx, tx)
		if timeErr != nil {
			return timeErr
		}
		result.ExpiresAt = now.Add(service.device.ChallengeTTL)
		return store.InsertDeviceChallenge(ctx, tx, store.DeviceChallenge{
			DeploymentID: service.deploymentID,
			ChallengeID:  challengeID,
			DeviceHash:   sha256Bytes("device\x00" + request.DeviceID),
			ClientHash:   sha256Bytes("client\x00" + canonicalClientIP),
			ProofHash:    sha256Bytes(proof),
			UserCodeHash: sha256Bytes(userCode),
			State:        store.DeviceChallengePending,
			PollInterval: service.device.PollInterval,
			ExpiresAt:    result.ExpiresAt,
		})
	})
	if err != nil {
		return DeviceChallenge{}, fmt.Errorf("start device login: %w", err)
	}
	if !allowed {
		return DeviceChallenge{}, ErrRateLimited
	}
	return result, nil
}

func (service *Service) ApproveDeviceLoginFromBrowser(
	ctx context.Context,
	decision BrowserDeviceDecision,
) error {
	return service.decideDeviceLoginFromBrowser(ctx, decision, true)
}

func (service *Service) DenyDeviceLoginFromBrowser(
	ctx context.Context,
	decision BrowserDeviceDecision,
) error {
	return service.decideDeviceLoginFromBrowser(ctx, decision, false)
}

func (service *Service) decideDeviceLoginFromBrowser(
	ctx context.Context,
	decision BrowserDeviceDecision,
	approve bool,
) error {
	if service == nil || service.pool == nil || rejectNilContext(ctx) != nil ||
		!validOpaque(decision.ChallengeID, 512) || !validOpaque(decision.UserCode, maximumTokenBytes) {
		return ErrChallengeInvalid
	}
	identity, err := service.ValidateCSRF(ctx, BrowserCSRFRequest{
		CookieToken: decision.CookieToken,
		CSRFToken:   decision.CSRFToken,
	})
	if err != nil {
		return err
	}
	storeDecision := store.BrowserDeviceDecision{
		DeploymentID: service.deploymentID,
		ChallengeID:  decision.ChallengeID,
		UserCodeHash: sha256Bytes(decision.UserCode),
		SessionID:    identity.SessionID,
		Principal:    identity.Principal,
	}
	err = store.WithinAuthDeployment(ctx, service.pool, service.deploymentID, func(ctx context.Context, tx *store.Transaction) error {
		if approve {
			return store.ApproveDeviceChallengeFromBrowser(ctx, tx, storeDecision)
		}
		return store.DenyDeviceChallengeFromBrowser(ctx, tx, storeDecision)
	})
	return mapStoreChallengeError(err)
}

func (service *Service) ExchangeDeviceProof(ctx context.Context, proof DeviceProof) (PMSession, error) {
	if service == nil || service.pool == nil || rejectNilContext(ctx) != nil ||
		!validOpaque(proof.ChallengeID, 512) || !validOpaque(proof.Proof, maximumTokenBytes) {
		return PMSession{}, ErrChallengeInvalid
	}
	var session PMSession
	var outcome error
	err := store.WithinAuthDeployment(ctx, service.pool, service.deploymentID, func(ctx context.Context, tx *store.Transaction) error {
		exchangeErr := store.ExchangeDeviceChallenge(
			ctx,
			tx,
			service.deploymentID,
			proof.ChallengeID,
			sha256Bytes(proof.Proof),
			func(principal contract.Principal) (store.PMSessionRecord, error) {
				prepared, record, prepareErr := service.prepareSessionMaterial(
					ctx,
					tx,
					store.SessionTransportDevice,
				)
				if prepareErr != nil {
					return store.PMSessionRecord{}, prepareErr
				}
				prepared.Principal = principal
				record.Principal = principal
				session = prepared
				return record, nil
			},
		)
		switch {
		case exchangeErr == nil:
			return nil
		case errors.Is(exchangeErr, store.ErrAuthChallengePending),
			errors.Is(exchangeErr, store.ErrAuthPollTooSoon),
			errors.Is(exchangeErr, store.ErrAuthChallengeDenied),
			errors.Is(exchangeErr, store.ErrAuthChallengeExpired),
			errors.Is(exchangeErr, store.ErrAuthChallengeUsed):
			outcome = exchangeErr
			return nil
		default:
			return exchangeErr
		}
	})
	if err != nil {
		return PMSession{}, mapStoreChallengeError(err)
	}
	if outcome != nil {
		return PMSession{}, mapStoreChallengeError(outcome)
	}
	return session, nil
}
