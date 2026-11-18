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

// TestRunDeviceExchangeJoinsTheOutputReadersAtItsDeadline (#402 review
// R2-F1): the proof write blocks, and a process that login-exchange started
// keeps the stdout and the stderr pipes open. The check ends the parent
// context after that process starts. The step must join the output readers
// before it returns, and name the output that stayed open.
func TestRunDeviceExchangeJoinsTheOutputReadersAtItsDeadline(t *testing.T) {
	finished := joinOnCleanup(t, "runDeviceExchange")
	descendant := startFakeDescendant(t, "the process that the fake login-exchange started")
	hold := releasePipe(t)
	challenge := &deviceChallenge{challengeID: "device-challenge-1", proof: strings.Repeat("p", 1<<20)}
	environment := append([]string{"PATH=/usr/bin:/bin", "LANG=C", "HOLD=" + hold}, descendant.environment...)
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	ended := make(chan error, 1)
	go func() {
		defer close(finished)
		ended <- runDeviceExchange(parent, "/bin/sh", []string{"-c", descendantLine + ` read _ < "$HOLD"`}, environment, challenge)
	}()
	if err := descendant.waitStart(); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-ended:
		if err == nil || !strings.Contains(err.Error(), "device proof write") || !strings.Contains(err.Error(), "kept the output open") {
			t.Fatalf("runDeviceExchange = %v, want the failures for the proof write and for the output that stayed open", err)
		}
		if strings.Contains(err.Error(), strings.Repeat("p", 64)) {
			t.Fatal("the failure message shows the device proof")
		}
	case <-time.After(processCheckGuard):
		t.Fatal("runDeviceExchange did not end while a process that login-exchange started kept the output open")
	}
}

// TestStopExchangeProcessJoinsTheOutputReaders (#402 review R2-F1): a
// process that login-exchange started keeps the stdout and the stderr pipes
// open. stopExchangeProcess must join the output readers before it returns,
// and fail. Each early return of runDeviceExchange and the scenario cleanup
// call it.
func TestStopExchangeProcessJoinsTheOutputReaders(t *testing.T) {
	var process *exchangeProcess
	t.Cleanup(func() {
		if process != nil {
			if err := stopExchangeProcess(process); err != nil {
				t.Errorf("stop the fake login-exchange in the cleanup: %v", err)
			}
		}
	})
	finished := joinOnCleanup(t, "stopExchangeProcess")
	descendant := startFakeDescendant(t, "the process that the fake login-exchange started")
	hold := releasePipe(t)
	environment := append([]string{"PATH=/usr/bin:/bin", "LANG=C", "HOLD=" + hold}, descendant.environment...)
	started, err := startExchangeProcess("/bin/sh", []string{"-c", descendantLine + ` read _ < "$HOLD"`}, environment)
	if err != nil {
		close(finished)
		t.Fatal(err)
	}
	process = started
	if err := descendant.waitStart(); err != nil {
		close(finished)
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() {
		defer close(finished)
		stopped <- stopExchangeProcess(process)
	}()
	select {
	case err := <-stopped:
		select {
		case <-process.outputEnded:
		default:
			t.Error("stopExchangeProcess returned while the output readers still ran")
		}
		if err == nil || !strings.Contains(err.Error(), "kept the output open") {
			t.Errorf("stopExchangeProcess = %v, want the failure for the output that stayed open", err)
		}
	case <-time.After(processCheckGuard):
		t.Fatal("stopExchangeProcess did not end while a process that login-exchange started kept the output open")
	}
}
