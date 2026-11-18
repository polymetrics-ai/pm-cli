package acceptance

import (
	"context"
	"errors"
	"fmt"

	"github.com/cucumber/godog"

	"github.com/polymetrics-ai/pm-cli/tests/starter"
)

// lane runs the real pm-server binary, with its API and relay, and the real
// pm-worker binary on the job-owned dependency stack. Each scenario gets its
// own lane: a new deployment, with a new deployment ID, and its own pm-server
// and pm-worker. The lane starts on the first step of the scenario that needs
// it, so a run of only the offline scenarios needs no stack. A failed start
// stays failed for the scenario: every later lane step of the scenario
// reports the same error, and the lane never tries to start again.
//
// The lane uses the shared stack harness: the shared build and migration
// step, one new deployment configuration and the shared starter. The starter
// starts pm-server with --ready-fd 3 and with the held listener of the
// deployment as --listen-fd 4 (ADR 0008). It waits once for its ready line or
// end of file, up to the start limit. After the pm-server ready line, it starts
// pm-worker in the same way. pm-worker uses the worker configuration of the
// deployment. From the start of each process to its ready signal, the lane
// makes no observation of pm-server, pm-worker or a dependency.
type lane struct {
	harness *stackHarness

	started  bool
	startErr error

	observer   *observer
	deployment *deployment
	server     *starter.Process
	worker     *starter.Process
}

// newLane makes the lane of one scenario. Its After hook stops the lane at
// the end of the scenario.
func newLane(sc *godog.ScenarioContext, harness *stackHarness) *lane {
	l := &lane{harness: harness}
	// godog keeps the step error. The hook returns only its own cleanup
	// error, so the step error is reported once.
	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		return ctx, l.close()
	})
	return l
}

// ensure starts the lane once. Then it checks once, with no wait, that
// pm-server and pm-worker still run.
func (l *lane) ensure(ctx context.Context) error {
	if l.startErr != nil {
		return fmt.Errorf("the lane failed to start earlier in this scenario: %w", l.startErr)
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
	if !l.worker.Running() {
		return fmt.Errorf("the lane pm-worker is no longer running: %s", l.worker.Snapshot())
	}
	return nil
}

// running reports an error unless the lane started in this scenario.
func (l *lane) running() error {
	switch {
	case l.startErr != nil:
		return fmt.Errorf("the lane failed to start earlier in this scenario: %w", l.startErr)
	case !l.started:
		return errors.New("the lane is not running")
	}
	return nil
}

// workerProcess returns the pm-worker process that the lane started. It is
// nil until the lane starts pm-worker.
func (l *lane) workerProcess() *starter.Process { return l.worker }

// start opens the observer and the deployment. Then it starts pm-server and,
// after the pm-server ready line, pm-worker. pm-worker does not start when
// pm-server sent no ready line.
func (l *lane) start(ctx context.Context) error {
	stack, err := readStackInputs()
	if err != nil {
		return err
	}
	if err := l.harness.prepare(ctx, &stack); err != nil {
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
	if l.server, err = l.startProcess(ctx, "pm-server", configuration); err != nil {
		return err
	}
	l.worker, err = l.startProcess(ctx, "pm-worker", configuration)
	return err
}

// startProcess starts one lane process through the shared starter, with the
// three process limits and the real timer source, and waits once for its
// ready signal. Only the ready line makes the start pass. End of file before
// the ready line, other data on the ready pipe and the start limit fail the
// step with the report of the starter: the bounded output snapshot and the
// stop result. On each of these paths the starter has finished with the
// process when startProcess returns.
func (l *lane) startProcess(ctx context.Context, name, configuration string) (*starter.Process, error) {
	binary, err := l.harness.binary(name)
	if err != nil {
		return nil, err
	}
	return l.startBinary(ctx, name, binary, configuration)
}

// startBinary starts the lane process name from binary. pm-server gets the
// held listener of the deployment as descriptor 4, with --listen-fd 4 (ADR
// 0008), so it does not bind the configured address itself. The harness
// closes its copy of the listener when giveListener fails, when the launch
// fails and when no ready signal comes. pm-worker gets no listener.
func (l *lane) startBinary(ctx context.Context, name, binary, configuration string) (*starter.Process, error) {
	config := starter.Config{
		Path:          binary,
		Args:          []string{"--config", configuration, "--secrets-directory", l.deployment.secrets, "--ready-fd", "3"},
		Env:           l.deployment.processEnvironment(),
		Dir:           l.deployment.home(),
		ReadyFD:       3,
		ReadyLine:     name + " ready\n",
		Limits:        processLimits,
		Timers:        starter.RealTimers{},
		SnapshotBytes: laneOutputLimit,
	}
	if name != "pm-server" {
		return awaitLaneProcess(ctx, name, config)
	}
	if err := l.deployment.giveListener(&config); err != nil {
		l.deployment.releaseListener()
		return nil, err
	}
	process, err := awaitLaneProcess(ctx, name, config)
	if err != nil {
		l.deployment.releaseListener()
	}
	return process, err
}

// awaitLaneProcess starts the lane process name with config and waits once
// for its ready signal. Only the ready line makes the start pass.
func awaitLaneProcess(ctx context.Context, name string, config starter.Config) (*starter.Process, error) {
	process, err := starter.Start(config)
	if err != nil {
		return nil, err
	}
	report, ready := process.AwaitReady()
	godog.Logf(ctx, "lane %s start: %s, after %s (start limit %s, stop %q)",
		name, report.Outcome, report.Elapsed, startLimit, report.Stop)
	if !ready {
		return process, fmt.Errorf("the lane %s sent no ready signal: %s", name, report)
	}
	return process, nil
}

// close runs at the end of each scenario. It stops pm-worker, then pm-server,
// each with the stop sequence of the starter: SIGTERM, the stop limit,
// SIGKILL and the kill limit. Then it closes the deployment and drops the
// observer role. Each process must exit with code 0 after SIGTERM. A process
// that exited before the end of the scenario is an error. A failed start is
// not reported again here: the start step reported it. A scenario that did
// not start the lane has nothing to close.
func (l *lane) close() error {
	errs := []error{
		stopLaneProcess("pm-worker", l.worker),
		stopLaneProcess("pm-server", l.server),
	}
	if l.deployment != nil {
		errs = append(errs, l.deployment.close())
	}
	errs = append(errs, l.observer.close())
	return errors.Join(errs...)
}

// stopLaneProcess runs the stop sequence for a lane process that the starter
// has not finished with. It returns an error unless the process exited with
// code 0 after SIGTERM.
func stopLaneProcess(name string, process *starter.Process) error {
	if process == nil || process.Done() {
		return nil
	}
	report := process.Stop()
	if report.Stop != starter.StopAfterSIGTERM || report.ExitCode != 0 {
		return fmt.Errorf("stop the lane %s: %s", name, report)
	}
	return nil
}
