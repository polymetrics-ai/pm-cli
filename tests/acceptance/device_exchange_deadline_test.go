package acceptance

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRunDeviceExchangeEndsABlockedProofWriteAtItsDeadline (#402 review F2):
// the login-exchange process does not read its stdin, and the device proof
// is larger than the pipe buffer, so the write of the proof blocks. The
// deadline of the step must end the write: the step kills the process,
// joins the write and returns a failure that does not show the proof. The
// parent context of this check gives the deadline.
func TestRunDeviceExchangeEndsABlockedProofWriteAtItsDeadline(t *testing.T) {
	finished := joinOnCleanup(t, "runDeviceExchange")
	release := releasePipe(t)
	challenge := &deviceChallenge{challengeID: "device-challenge-1", proof: strings.Repeat("p", 1<<20)}
	environment := []string{"PATH=/usr/bin:/bin", "LANG=C", "RELEASE=" + release}
	parent, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	ended := make(chan error, 1)
	go func() {
		defer close(finished)
		ended <- runDeviceExchange(parent, "/bin/sh", []string{"-c", `read _ < "$RELEASE"`}, environment, challenge)
	}()
	select {
	case err := <-ended:
		if err == nil || !strings.Contains(err.Error(), "device proof write") {
			t.Fatalf("runDeviceExchange = %v, want the failure for the proof write that did not end", err)
		}
		if strings.Contains(err.Error(), strings.Repeat("p", 64)) {
			t.Fatal("the failure message shows the device proof")
		}
	case <-time.After(processCheckGuard):
		t.Fatal("runDeviceExchange did not end while its proof write was blocked")
	}
}
