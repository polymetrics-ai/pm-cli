package server

import (
	"context"
	"testing"
	"time"
)

func TestApplicationCloseStopsRelayBeforeReturning(t *testing.T) {
	relayContext, cancelRelay := context.WithCancel(context.Background())
	relayCancelled := make(chan struct{})
	releaseRelay := make(chan struct{})
	relayDone := make(chan error, 1)
	go func() {
		<-relayContext.Done()
		close(relayCancelled)
		<-releaseRelay
		relayDone <- nil
	}()
	application := &Application{relayCancel: cancelRelay, relayDone: relayDone}
	closed := make(chan error, 1)
	go func() { closed <- application.Close() }()

	select {
	case <-relayCancelled:
	case <-time.After(time.Second):
		t.Fatal("application close did not cancel its relay")
	}
	select {
	case err := <-closed:
		t.Fatalf("application close returned before the relay stopped: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseRelay)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("application close after orderly relay stop: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("application close did not finish after the relay stopped")
	}
	if err := application.Close(); err != nil {
		t.Fatalf("idempotent application close: %v", err)
	}
}

func TestConfiguredControlTaskQueueSelectsNamedMember(t *testing.T) {
	queue, err := configuredControlTaskQueue([]string{"pm-read", "pm-control", "pm-effect"})
	if err != nil {
		t.Fatalf("select configured pm-control member: %v", err)
	}
	if queue != "pm-control" {
		t.Fatalf("selected task queue = %q; want named pm-control member", queue)
	}
	if _, err := configuredControlTaskQueue([]string{"pm-read", "pm-effect"}); err == nil {
		t.Fatal("task queue selection accepted configuration without pm-control")
	}
}
