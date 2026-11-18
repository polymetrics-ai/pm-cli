package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Go developer check, not acceptance proof (issue #366, developer check C2).
// The configured identity.device_poll_interval reaches each new device login
// challenge through the real application composition. The start answer states
// the interval, the store keeps it, and the stored first permitted poll is
// the insertion time plus the interval in the database clock. Without the
// setting, the interval is 5 seconds. Defect: pm-server reads the setting but
// does not use it, or it states one interval and stores another.
func TestDevicePollIntervalReachesNewChallenges(t *testing.T) {
	for _, test := range []struct {
		name     string
		options  []serverIntegrationOption
		interval time.Duration
	}{
		{name: "configured 60 seconds", options: []serverIntegrationOption{withDevicePollInterval("60s")}, interval: time.Minute},
		{name: "default without the setting", interval: 5 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newServerIntegrationFixture(t, test.options...)
			client := fixture.newHTTPClient(t, false)
			before := fixture.databaseClock(t)
			challenge := startIntegrationDeviceLogin(t, fixture, client)
			after := fixture.databaseClock(t)
			if time.Duration(challenge.PollIntervalSeconds)*time.Second != test.interval {
				t.Fatalf("device login start states a poll interval of %d seconds, want %s",
					challenge.PollIntervalSeconds, test.interval)
			}
			requireStoredDevicePoll(t, fixture.storedDevicePoll(t, challenge.ChallengeID), test.interval,
				before, after, "first permitted poll")
			if test.interval != time.Minute {
				return
			}
			tooSoon := exchangeIntegrationDeviceLogin(t, fixture, client, challenge)
			requireIntegrationStatus(t, tooSoon, http.StatusTooManyRequests, "device exchange right after the start")
			requireIntegrationProblemCode(t, tooSoon, "device_poll_too_soon")
		})
	}
}

// Go developer check, not acceptance proof (issue #366, developer check C4,
// the third developer check of the criteria addendum). After a pending
// answer, the next permitted poll is the answer time plus the configured
// interval in the database clock. No Gherkin scenario sees a pending answer
// after the first permitted poll. Defect: a fixed or wrong cadence after a
// pending answer.
func TestDevicePollIntervalSetsThePendingCadence(t *testing.T) {
	fixture := newServerIntegrationFixture(t, withDevicePollInterval("60s"))
	client := fixture.newHTTPClient(t, false)
	challenge := startIntegrationDeviceLogin(t, fixture, client)
	fixture.makeDevicePollEligible(t, challenge.ChallengeID)
	before := fixture.databaseClock(t)
	pending := exchangeIntegrationDeviceLogin(t, fixture, client, challenge)
	after := fixture.databaseClock(t)
	requireIntegrationStatus(t, pending, http.StatusConflict, "device exchange after the first permitted poll")
	requireIntegrationProblemCode(t, pending, "device_authorization_pending")
	requireStoredDevicePoll(t, fixture.storedDevicePoll(t, challenge.ChallengeID), time.Minute,
		before, after, "next permitted poll after a pending answer")
}

func startIntegrationDeviceLogin(
	t *testing.T,
	fixture *serverIntegrationFixture,
	client *http.Client,
) integrationDeviceChallengeDocument {
	t.Helper()
	start := fixture.requestJSON(t, client, http.MethodPost, "/v1/sessions/device", map[string]any{
		"device_id": "server-integration-device-poll-interval",
	}, nil)
	requireIntegrationStatus(t, start, http.StatusCreated, "device login start")
	var challenge integrationDeviceChallengeDocument
	decodeIntegrationJSON(t, start.body, &challenge, "device login challenge")
	return challenge
}

func exchangeIntegrationDeviceLogin(
	t *testing.T,
	fixture *serverIntegrationFixture,
	client *http.Client,
	challenge integrationDeviceChallengeDocument,
) integrationHTTPResponse {
	t.Helper()
	return fixture.requestJSON(t, client, http.MethodPost, "/v1/sessions/device/exchange", map[string]any{
		"challenge_id": challenge.ChallengeID,
		"proof":        challenge.DeviceProof,
	}, nil)
}

// storedDevicePoll is the stored poll state of one device login challenge.
type storedDevicePoll struct {
	intervalSeconds int64
	nextPollAt      time.Time
}

// requireStoredDevicePoll checks the stored interval and the stored next
// permitted poll. The store sets the next permitted poll from its database
// clock while it answers one request. The two database clock readings before
// and after that request bracket that time, so the check needs no tolerance.
func requireStoredDevicePoll(
	t *testing.T,
	stored storedDevicePoll,
	interval time.Duration,
	before, after time.Time,
	description string,
) {
	t.Helper()
	if time.Duration(stored.intervalSeconds)*time.Second != interval {
		t.Fatalf("stored device poll interval = %d seconds, want %s", stored.intervalSeconds, interval)
	}
	if base := stored.nextPollAt.Add(-interval); base.Before(before) || base.After(after) {
		t.Fatalf("stored %s is %s after the request start, and the request took %s; want %s plus at most the request time",
			description, stored.nextPollAt.Sub(before), after.Sub(before), interval)
	}
}

// databaseClock reads clock_timestamp() of the application store, the clock
// that the store uses for device poll times.
func (fixture *serverIntegrationFixture) databaseClock(t *testing.T) time.Time {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, fixture.migratorDSN)
	if err != nil {
		t.Fatalf("connect server integration database clock: %T", err)
	}
	defer connection.Close(ctx)
	var now time.Time
	if err := connection.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("read server integration database clock: %T", err)
	}
	return now
}

func (fixture *serverIntegrationFixture) storedDevicePoll(t *testing.T, challengeID string) storedDevicePoll {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, fixture.migratorDSN)
	if err != nil {
		t.Fatalf("connect server integration device poll observation: %T", err)
	}
	defer connection.Close(ctx)
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin server integration device poll observation: %T", err)
	}
	defer tx.Rollback(ctx)
	if err := bindServerIntegrationScope(ctx, tx, fixture.deploymentID, fixture.scope, fixture.operator); err != nil {
		t.Fatalf("bind server integration device poll observation: %T", err)
	}
	var stored storedDevicePoll
	if err := tx.QueryRow(ctx, `
		SELECT poll_interval_seconds, next_poll_at
		FROM pm.device_login_challenges
		WHERE deployment_id = $1 AND challenge_id = $2
	`, fixture.deploymentID, challengeID).Scan(&stored.intervalSeconds, &stored.nextPollAt); err != nil {
		t.Fatalf("observe server integration device poll state: %T", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit server integration device poll observation: %T", err)
	}
	return stored
}
