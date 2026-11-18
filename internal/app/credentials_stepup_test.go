package app

import (
	"testing"
	"time"
)

// TestRecentCredentialAuthenticationWithAControlledClock is the developer
// check of the stale step-up at the credential ingress (LOGIN proposal v3,
// Part 5, slice W1a). Gherkin cannot age a login without a sleep, so the
// check gives recentCredentialAuthentication fixed times: a controlled clock.
// It is not acceptance proof.
func TestRecentCredentialAuthenticationWithAControlledClock(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	const maximumAge = 5 * time.Minute
	for _, test := range []struct {
		name            string
		authenticatedAt time.Time
		maximumAge      time.Duration
		recent          bool
	}{
		{name: "a login at the same instant is recent", authenticatedAt: now, maximumAge: maximumAge, recent: true},
		{name: "a login exactly the maximum age ago is recent", authenticatedAt: now.Add(-maximumAge), maximumAge: maximumAge, recent: true},
		{name: "a login one second older than the maximum age is stale", authenticatedAt: now.Add(-maximumAge - time.Second), maximumAge: maximumAge, recent: false},
		{name: "a login one nanosecond older than the maximum age is stale", authenticatedAt: now.Add(-maximumAge - time.Nanosecond), maximumAge: maximumAge, recent: false},
		{name: "a login one hour ago is stale", authenticatedAt: now.Add(-time.Hour), maximumAge: maximumAge, recent: false},
		{name: "a login after the clock time is refused", authenticatedAt: now.Add(time.Second), maximumAge: maximumAge, recent: false},
		{name: "a zero login time is refused", authenticatedAt: time.Time{}, maximumAge: maximumAge, recent: false},
		{name: "a login time at or before the Unix epoch is refused", authenticatedAt: time.Unix(0, 0).UTC(), maximumAge: maximumAge, recent: false},
		{name: "a login time that is not UTC is refused", authenticatedAt: now.In(time.FixedZone("UTC+1", 3600)), maximumAge: maximumAge, recent: false},
		{name: "a zero maximum age refuses every login", authenticatedAt: now, maximumAge: 0, recent: false},
		{name: "a negative maximum age refuses every login", authenticatedAt: now, maximumAge: -time.Second, recent: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := recentCredentialAuthentication(test.authenticatedAt, test.maximumAge, now); got != test.recent {
				t.Fatalf("recentCredentialAuthentication = %t, want %t", got, test.recent)
			}
		})
	}
}
