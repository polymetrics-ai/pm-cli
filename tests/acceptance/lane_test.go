package acceptance

import (
	"context"
	"errors"
	"fmt"

	"github.com/cucumber/godog"

	"github.com/polymetrics-ai/pm-cli/tests/starter"
)

// lane runs the real pm-server binary, with its API and relay, on the
// job-owned dependency stack. It starts on the first step that needs it, so a
// run of only the offline scenarios needs no stack. The lane starts once per
// suite. A failed start stays failed: every later lane step reports the same
// error, and the lane never tries to start again.
//
// The lane uses the shared stack harness: the shared build and migration
// step, one new deployment configuration and the shared starter. The starter
// starts pm-server with --ready-fd 3 and waits once for the ready line or end
// of file, up to the start limit. From the start of pm-server to its ready
// signal, the lane makes no observation of pm-server or of a dependency.
type lane struct {
	harness *stackHarness

	started  bool
	startErr error

	observer   *observer
	deployment *deployment
	server     *starter.Process
}

func newLane(harness *stackHarness) *lane {
	return &lane{harness: harness}
}

// ensure starts the lane once. Then it checks once, with no wait, that
// pm-server still runs.
func (l *lane) ensure(ctx context.Context) error {
	if l.startErr != nil {
		return fmt.Errorf("the lane failed to start earlier in this suite: %w", l.startErr)
	}
	if !l.started {
		l.started = true
		if err := l.start(ctx); err != nil {
			l.startErr = err
			return err
		}
	}
	if !l.server.Running() {
		return fmt.Errorf("the lane pm-server is no longer running: %s", l.server.Snapshot())
	}
	return nil
}

// running reports an error unless the lane started in this suite.
func (l *lane) running() error {
	switch {
	case l.startErr != nil:
		return fmt.Errorf("the lane failed to start earlier in this suite: %w", l.startErr)
	case !l.started:
		return errors.New("the lane is not running")
	}
	return nil
}

// start opens the observer and the deployment, then starts pm-server through
// the shared starter and waits for its ready signal. Only the ready line makes
// the start pass. End of file before the ready line, other data on the ready
// pipe and the start limit fail the step with the report of the starter: the
// bounded output snapshot and the stop result.
func (l *lane) start(ctx context.Context) error {
	stack, err := readStackInputs()
	if err != nil {
		return err
	}
	if err := l.harness.prepare(ctx, &stack); err != nil {
		return err
	}
	binary, err := l.harness.binary("pm-server")
	if err != nil {
		return err
	}
	if l.observer, err = openObserver(ctx, stack); err != nil {
		return err
	}
	if l.deployment, err = newDeployment(stack); err != nil {
		return err
	}
	configuration, err := l.deployment.configurationPath()
	if err != nil {
		return err
	}
	if l.server, err = starter.Start(starter.Config{
		Path:          binary,
		Args:          []string{"--config", configuration, "--secrets-directory", l.deployment.secrets, "--ready-fd", "3"},
		Env:           l.deployment.processEnvironment(),
		Dir:           l.deployment.home(),
		ReadyFD:       3,
		ReadyLine:     "pm-server ready\n",
		Limits:        processLimits,
		Timers:        starter.RealTimers{},
		SnapshotBytes: laneOutputLimit,
	}); err != nil {
		return err
	}
	report, ready := l.server.AwaitReady()
	godog.Logf(ctx, "lane pm-server start: %s, after %s (start limit %s, stop %q)",
		report.Outcome, report.Elapsed, startLimit, report.Stop)
	if !ready {
		return fmt.Errorf("the lane pm-server sent no ready signal: %s", report)
	}
	return nil
}

// close stops pm-server with the stop sequence of the starter, closes the
// deployment and drops the observer role. pm-server must exit with code 0
// after SIGTERM. A pm-server that exited before the end of the suite is an
// error. A failed start is not reported again here: the start step reported
// it.
func (l *lane) close() error {
	var errs []error
	if l.server != nil && !l.server.Done() {
		report := l.server.Stop()
		if report.Stop != starter.StopAfterSIGTERM || report.ExitCode != 0 {
			errs = append(errs, fmt.Errorf("stop the lane pm-server: %s", report))
		}
	}
	if l.deployment != nil {
		errs = append(errs, l.deployment.close())
	}
	errs = append(errs, l.observer.close())
	return errors.Join(errs...)
}
