package schema

import (
	"sync"
	"testing"
)

// TestRaceCanary has a real data race on purpose: two goroutines write
// counter with no synchronization. It shows that the race detector in the
// unit run of the CI `checks` job fails the test. Without -race the test
// passes. Issue #307. This branch is never merged.
func TestRaceCanary(t *testing.T) {
	counter := 0
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			counter++
		}()
	}
	wg.Wait()
	if counter == 0 {
		t.Fatal("no goroutine wrote counter")
	}
}
