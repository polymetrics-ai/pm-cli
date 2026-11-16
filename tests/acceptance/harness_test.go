package acceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/tests/starter"
)

// The four limits of the shared harness, in one place (criteria N24). The
// steps give the three process limits and the real timer source to the
// starter. Every go build of the Gherkin run waits once, with the build limit.
const (
	// startLimit is counted from the start of a process. It covers the ready
	// line and the end of file after it. The value is provisional (ADR 0005,
	// amendment v4, change A6).
	startLimit = 120 * time.Second
	// stopLimit bounds the wait for the exit after SIGTERM.
	stopLimit = 45 * time.Second
	// killLimit bounds the wait for the exit after SIGKILL, and the two more
	// bounded waits of the starter.
	killLimit = 10 * time.Second
	// buildLimit bounds each go build with its own deadline (source: the #261
	// build bound, tests/acceptance/main_test.go:70 at 4be05ea). The shared
	// migration also has its own deadline of this value: the criteria name
	// four constants, so the migration adds no fifth one.
	buildLimit = 10 * time.Minute
)

// processLimits are the three process limits that the steps give the starter.
var processLimits = starter.Limits{Start: startLimit, Stop: stopLimit, Kill: killLimit}

const (
	laneIdentityRef     = "secret://lane/identity"
	laneEncryptionRef   = "secret://lane/encryption"
	laneOutputLimit     = 64 << 10
	laneFileLimit       = 2 << 20
	applicationDatabase = "pm_app"
)

// laneWorkerControls are the bounded worker controls of the lane. The stack
// configuration has no worker block. These values are the same as the test
// worker configuration in internal/testfixture.
var laneWorkerControls = json.RawMessage(`{
  "task_queue": "pm-control",
  "dial_timeout": "5s",
  "drain_timeout": "5s",
  "max_concurrent_activities": 32,
  "max_concurrent_workflow_tasks": 4,
  "activity_timeouts": {
    "schedule_to_close": "20s",
    "start_to_close": "10s",
    "heartbeat": "2s",
    "transport": "5s"
  }
}`)

// stackHarness is the suite state of the steps that start pm-server and
// pm-worker. Scenarios run one at a time, so it needs no lock.
type stackHarness struct {
	bins binaries
	dir  string

	built    bool
	buildErr error
	server   string
	worker   string

	migrated   bool
	migrateErr error
}

func newStackHarness(bins binaries, dir string) *stackHarness {
	return &stackHarness{bins: bins, dir: dir}
}

// prepare is the shared build and migration step. It builds pm-server and
// pm-worker once for the suite. When stack is not nil, it also applies the PM
// application migrations once for the suite. Each go build waits once, with
// its own build-limit deadline, created right before that build. The
// migration waits once, with its own deadline of the build-limit value. A
// failure stays failed: a later call reports the same error and does not
// try again.
func (h *stackHarness) prepare(parent context.Context, stack *stackInputs) error {
	if !h.built {
		h.built = true
		h.buildErr = h.build(parent)
	}
	if h.buildErr != nil {
		return fmt.Errorf("the shared build of pm-server and pm-worker failed in this suite: %w", h.buildErr)
	}
	if stack == nil {
		return nil
	}
	if !h.migrated {
		h.migrated = true
		h.migrateErr = migrate(parent, *stack)
	}
	if h.migrateErr != nil {
		return fmt.Errorf("the shared migration failed in this suite: %w", h.migrateErr)
	}
	return nil
}

func (h *stackHarness) build(parent context.Context) error {
	var err error
	if h.server, err = h.buildOne(parent, "pm-server", "./cmd/pm-server"); err != nil {
		return err
	}
	h.worker, err = h.buildOne(parent, "pm-worker", "./cmd/pm-worker")
	return err
}

// buildOne runs one go build. It waits once, with its own deadline: the
// build limit.
func (h *stackHarness) buildOne(parent context.Context, name, pkg string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, buildLimit)
	defer cancel()
	return buildBinary(ctx, h.bins, h.dir, name, pkg, false)
}

// migrate applies the PM application migrations. pm-server and pm-worker
// never migrate. The harness acts as the operator and uses the migrator role.
// It waits once, with its own deadline of the build-limit value.
func migrate(parent context.Context, stack stackInputs) error {
	ctx, cancel := context.WithTimeout(parent, buildLimit)
	defer cancel()
	if err := store.Migrate(ctx, stack.postgres.dsn(store.MigratorRole, stack.postgres.migratorPassword, applicationDatabase)); err != nil {
		return fmt.Errorf("migrate the PM application store: %w", err)
	}
	return nil
}

// binary returns the built pm-server or pm-worker. Call prepare first.
func (h *stackHarness) binary(name string) (string, error) {
	switch {
	case !h.built || h.buildErr != nil:
		return "", fmt.Errorf("%s is not built", name)
	case name == "pm-server":
		return h.server, nil
	case name == "pm-worker":
		return h.worker, nil
	}
	return "", fmt.Errorf("unknown process %q", name)
}
