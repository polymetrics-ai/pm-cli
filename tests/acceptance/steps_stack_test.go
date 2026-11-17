package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cucumber/godog"

	"github.com/polymetrics-ai/pm-cli/tests/keyring"
	"github.com/polymetrics-ai/pm-cli/tests/starter"
)

const laneCommandTimeout = 60 * time.Second

// stackScenario is the state of one scenario that uses the shared steps: its
// deployment configuration, the processes that the starter started, the pm
// client HOME and private keyring, and the shared result state of the last pm
// command.
type stackScenario struct {
	harness    *stackHarness
	deployment *deployment
	processes  map[string]*startedProcess
	runDir     string
	home       string
	keyring    *keyring.Keyring
	workspace  string
	result     commandResult
}

// startedProcess is one process that the starter started, with the report of
// the last wait on it.
type startedProcess struct {
	process *starter.Process
	report  starter.Report
}

// commandResult is the shared scenario result state: the last pm command
// that the scenario ran against a PM server.
type commandResult struct {
	ran      bool
	exitCode int
	stdout   string
	stderr   string
	// deploymentID is the deployment ID of the server that the command ran
	// against.
	deploymentID string
}

// registerStackSteps registers each step that two or more slices use: the
// steps that start and observe pm-server and pm-worker through the shared
// starter, and the result steps of the shared result state.
func registerStackSteps(sc *godog.ScenarioContext, harness *stackHarness, lane *lane) {
	scenario := &stackScenario{harness: harness, processes: map[string]*startedProcess{}}
	// godog keeps the step error. The hook returns only its own cleanup
	// error, so the step error is reported once.
	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		return ctx, scenario.close()
	})
	sc.Step(`^a new PM deployment configuration on the job-owned stack$`, scenario.newConfiguration)
	sc.Step(`^the starter starts (pm-server|pm-worker) and waits for its ready signal or its exit$`, scenario.startAndWait)
	sc.Step(`^the starter received the (pm-server|pm-worker) ready signal$`, scenario.receivedReadySignal)
	sc.Step(`^the starter received no (pm-server|pm-worker) ready signal$`, scenario.receivedNoReadySignal)
	sc.Step(`^the starter sends SIGTERM to (pm-server|pm-worker)$`, scenario.sendSIGTERM)
	sc.Step(`^(pm-server|pm-worker) is still running$`, scenario.stillRunning)
	sc.Step(`^(pm-server|pm-worker) exits with code (\d+)$`, scenario.exitsWithCode)
	sc.Step(`^(pm-server|pm-worker) stderr contains "([^"]*)"$`, scenario.stderrContains)
	sc.Step(`^I run (pm-server|pm-worker) with "([^"]*)" and descriptor 3 (as the write end of a ready pipe|closed|open only for reading)$`, scenario.runWithDescriptor)
	sc.Step(`^(pm-server|pm-worker) stderr is exactly the line: (.*)$`, scenario.stderrIsExactlyLine)
	sc.Step(`^the lane command exits with code (\d+)$`, scenario.result.exitCodeIs)
	sc.Step(`^the lane command stdout contains the lane deployment ID$`, scenario.result.stdoutHasDeploymentID)
	sc.Step(`^the lane command stderr contains "([^"]*)"$`, scenario.result.stderrContains)
	registerStartedServerSteps(sc, scenario)
	registerBootstrapSteps(sc, scenario)
	registerLaneSteps(sc, scenario, lane)
	registerDevicePollIntervalSteps(sc, scenario)
}

// newConfiguration is step 1. It runs the shared build and migration step,
// then makes a new deployment configuration.
func (s *stackScenario) newConfiguration(ctx context.Context) error {
	if s.deployment != nil {
		return errors.New("the scenario already has a deployment configuration")
	}
	stack, err := readStackInputs()
	if err != nil {
		return err
	}
	if err := s.harness.prepare(ctx, &stack); err != nil {
		return err
	}
	if s.deployment, err = newDeployment(stack); err != nil {
		return err
	}
	return nil
}

// startAndWait is step 5. The starter gives the write end of a new ready pipe
// as descriptor 3, with --ready-fd 3.
func (s *stackScenario) startAndWait(ctx context.Context, name string) error {
	if s.deployment == nil {
		return errors.New("the scenario has no deployment configuration")
	}
	configuration, err := s.deployment.configurationPath()
	if err != nil {
		return err
	}
	_, err = s.start(ctx, name, starter.Config{
		Args:    []string{"--config", configuration, "--secrets-directory", s.deployment.secrets, "--ready-fd", "3"},
		Env:     s.deployment.processEnvironment(),
		Dir:     s.deployment.home(),
		ReadyFD: 3,
	})
	return err
}

// runWithDescriptor is step 24. It needs no stack: it uses only the shared
// build. It waits for the ready line or end of file, and then for the exit.
func (s *stackScenario) runWithDescriptor(ctx context.Context, name, arguments, descriptor string) error {
	modes := map[string]starter.ReadyMode{
		"as the write end of a ready pipe": starter.ReadyWriteEnd,
		"closed":                           starter.ReadyClosed,
		"open only for reading":            starter.ReadyReadOnly,
	}
	mode, ok := modes[descriptor]
	if !ok {
		return fmt.Errorf("unknown descriptor 3 mode %q", descriptor)
	}
	if err := s.harness.prepare(ctx, nil); err != nil {
		return err
	}
	directory, err := s.runDirectory()
	if err != nil {
		return err
	}
	started, err := s.start(ctx, name, starter.Config{
		Args:      strings.Fields(arguments),
		Env:       []string{"HOME=" + filepath.Join(directory, "home"), "PATH=/usr/bin:/bin", "LANG=C", "TMPDIR=" + filepath.Join(directory, "tmp")},
		Dir:       filepath.Join(directory, "home"),
		ReadyFD:   3,
		ReadyMode: mode,
	})
	if err != nil {
		return err
	}
	started.report = started.process.AwaitExit()
	return nil
}

// start starts the named process through the shared starter, with the
// process limits and the real timer source, and waits once for its ready line
// or end of file. Only the start limit fails the step. The Then steps check
// every other result. The step output records the duration of the start.
func (s *stackScenario) start(ctx context.Context, name string, config starter.Config) (*startedProcess, error) {
	if _, ok := s.processes[name]; ok {
		return nil, fmt.Errorf("the scenario already started %s", name)
	}
	binary, err := s.harness.binary(name)
	if err != nil {
		return nil, err
	}
	config.Path = binary
	config.ReadyLine = name + " ready\n"
	config.Limits = processLimits
	config.Timers = starter.RealTimers{}
	config.SnapshotBytes = laneOutputLimit
	process, err := starter.Start(config)
	if err != nil {
		return nil, err
	}
	started := &startedProcess{process: process}
	s.processes[name] = started
	started.report, _ = process.AwaitReady()
	godog.Logf(ctx, "%s start: %s, after %s (start limit %s, stop %q)",
		name, started.report.Outcome, started.report.Elapsed, startLimit, started.report.Stop)
	if started.report.Outcome == starter.OutcomeStartLimitPassed {
		return nil, fmt.Errorf("%s: %s: %s", name, started.report.Outcome, started.report)
	}
	return started, nil
}

func (s *stackScenario) started(name string) (*startedProcess, error) {
	started, ok := s.processes[name]
	if !ok {
		return nil, fmt.Errorf("the starter did not start %s in this scenario", name)
	}
	return started, nil
}

// receivedReadySignal is step 7. It compares the first line of the ready
// pipe, without its line feed, with the fixed text.
func (s *stackScenario) receivedReadySignal(name string) error {
	started, err := s.started(name)
	if err != nil {
		return err
	}
	report := started.process.Snapshot()
	line, _, found := bytes.Cut(report.ReadyData, []byte("\n"))
	if !found || string(line) != name+" ready" {
		return fmt.Errorf("the starter did not receive the %s ready signal: %s", name, report)
	}
	return nil
}

// receivedNoReadySignal is step 8: end of file on the ready pipe with no data.
func (s *stackScenario) receivedNoReadySignal(name string) error {
	started, err := s.started(name)
	if err != nil {
		return err
	}
	report := started.process.Snapshot()
	if len(report.ReadyData) != 0 || !report.ReadyEOF {
		return fmt.Errorf("the starter did not read end of file with no data on the %s ready pipe: %s", name, report)
	}
	return nil
}

// sendSIGTERM is step 10. It runs the stop sequence. It does not fail when
// the process already exited: the next step checks the exit code.
func (s *stackScenario) sendSIGTERM(ctx context.Context, name string) error {
	started, err := s.started(name)
	if err != nil {
		return err
	}
	started.report = started.process.Stop()
	godog.Logf(ctx, "%s stop: %s", name, started.report.Stop)
	return nil
}

// stillRunning is step 11. It makes one observation of the exit, with no
// wait. Running does not depend on the reaper goroutine: a process that
// exited right after its ready line does not count as running.
func (s *stackScenario) stillRunning(name string) error {
	started, err := s.started(name)
	if err != nil {
		return err
	}
	if started.process.Done() || !started.process.Running() {
		return fmt.Errorf("%s is not running: %s", name, started.process.Snapshot())
	}
	return nil
}

// exitsWithCode is step 12. If the process has not exited, it waits once
// for the exit, with the kill limit, and then runs the stop sequence.
func (s *stackScenario) exitsWithCode(name string, want int) error {
	started, err := s.started(name)
	if err != nil {
		return err
	}
	started.report = started.process.AwaitExit()
	if !started.report.Exited || started.report.ExitCode != want {
		return fmt.Errorf("%s exit code %d, want %d: %s", name, started.report.ExitCode, want, started.report)
	}
	return nil
}

// stderrContains is step 13. It reads the stderr snapshot.
func (s *stackScenario) stderrContains(name, want string) error {
	started, err := s.started(name)
	if err != nil {
		return err
	}
	report := started.process.Snapshot()
	if !strings.Contains(string(report.Stderr), want) {
		return fmt.Errorf("%s stderr does not contain %q: %s", name, want, report)
	}
	return nil
}

// stderrIsExactlyLine is step 38. It reads the whole stderr after the exit.
// It passes only when stderr is the line and one line feed, with no other
// byte.
func (s *stackScenario) stderrIsExactlyLine(name, line string) error {
	started, err := s.started(name)
	if err != nil {
		return err
	}
	report := started.process.Snapshot()
	switch {
	case !report.Exited:
		return fmt.Errorf("%s has not exited, so its whole stderr is not known: %s", name, report)
	case !report.OutputEnded || report.StderrTotal != int64(len(report.Stderr)):
		return fmt.Errorf("the starter does not hold the whole %s stderr (%d bytes read): %s", name, report.StderrTotal, report)
	case string(report.Stderr) != line+"\n":
		return fmt.Errorf("%s stderr is %q, want exactly the line %q", name, report.Stderr, line+"\n")
	}
	return nil
}

// runDirectory makes the scenario directory of step 24, with home and tmp.
func (s *stackScenario) runDirectory() (string, error) {
	if s.runDir != "" {
		return s.runDir, nil
	}
	directory, err := os.MkdirTemp("", "pm-acceptance-run-")
	if err != nil {
		return "", fmt.Errorf("create the scenario run directory: %w", err)
	}
	s.runDir = directory
	for _, name := range []string{"home", "tmp"} {
		if err := os.Mkdir(filepath.Join(directory, name), 0o700); err != nil {
			return "", fmt.Errorf("create the scenario %s directory: %w", name, err)
		}
	}
	return directory, nil
}

// close runs the stop sequence for each started process that the starter has
// not finished, closes the deployment, stops the private keyring and removes
// its directory, and removes the scenario directories.
// A process that the stop sequence stopped, but not with exit code 0 after
// SIGTERM, is an error. A process that exited by itself is not: the steps
// check it.
func (s *stackScenario) close() error {
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(s.processes)) {
		started := s.processes[name]
		if started.process.Done() {
			continue
		}
		report := started.process.Stop()
		if report.Stop != starter.StopNone && (report.Stop != starter.StopAfterSIGTERM || report.ExitCode != 0) {
			errs = append(errs, fmt.Errorf("stop %s at the end of the scenario: %s", name, report))
		}
	}
	if s.deployment != nil {
		errs = append(errs, s.deployment.close())
	}
	errs = append(errs, s.keyring.Stop())
	for _, directory := range []string{s.runDir, s.home} {
		if directory != "" {
			errs = append(errs, os.RemoveAll(directory))
		}
	}
	return errors.Join(errs...)
}

// addServerProfile adds a pm server profile for the deployment with the real
// pm binary, in a new HOME for this scenario. With the HOME, it starts the
// private keyring of the scenario's pm client and selects a new workspace ID
// for the scenario. The server never creates that workspace unless a scenario
// runs an authenticated command.
func (s *stackScenario) addServerProfile(ctx context.Context, profile string, d *deployment) error {
	if s.home == "" {
		home, err := os.MkdirTemp("", "pm-acceptance-stack-home-")
		if err != nil {
			return fmt.Errorf("create the scenario home: %w", err)
		}
		s.home = home
		s.workspace = "acceptance-workspace-" + randomHex(6)
		if s.keyring, err = startKeyring(ctx); err != nil {
			return err
		}
	}
	code, stdout, stderr, err := s.runPM(ctx, d.caFile, []string{
		"server", "profiles", "add", profile,
		"--url", d.origin,
		"--instance-id", d.deploymentID,
	})
	if err != nil {
		return err
	}
	if code != 0 || strings.TrimSpace(stdout) != profile {
		return fmt.Errorf("pm server profiles add exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	return nil
}

// runAgainst runs the command with the controls added at the end: the server
// profile, the explicit process-local memory session store and the scenario
// workspace. The memory store starts empty in each process, so the result
// does not depend on an OS keyring. It writes the shared result state, with
// the deployment ID of the server. It reads no observer.
func (s *stackScenario) runAgainst(ctx context.Context, line, profile string, d *deployment) error {
	if s.home == "" {
		return errors.New("the pm server profile is not set")
	}
	args := strings.Fields(line)
	if len(args) == 0 || args[0] != "pm" {
		return fmt.Errorf("command must start with pm: %q", line)
	}
	args = append(args[1:], "--server-profile", profile, "--session-store", "memory", "--workspace", s.workspace)
	code, stdout, stderr, err := s.runPM(ctx, d.caFile, args)
	if err != nil {
		return err
	}
	s.result = commandResult{ran: true, exitCode: code, stdout: stdout, stderr: stderr, deploymentID: d.deploymentID}
	return nil
}

// startKeyring starts the private Secret Service of the scenario's pm
// client. Its directory is in the job directory, RUNNER_TEMP, and the stop
// step removes it (D1-10). The start checks the owner of
// org.freedesktop.secrets (D1-11). The start waits use the start limit.
func startKeyring(ctx context.Context) (*keyring.Keyring, error) {
	job := os.Getenv("RUNNER_TEMP")
	if !filepath.IsAbs(job) {
		return nil, errors.New("RUNNER_TEMP must name the job directory: run the @stack scenarios through scripts/ci/check-bdd.sh")
	}
	started, err := keyring.Start(job, keyring.Limits{Start: startLimit, Stop: stopLimit, Kill: killLimit})
	if err != nil {
		return nil, err
	}
	godog.Logf(ctx, "private keyring: org.freedesktop.secrets is owned by the keyring daemon that the harness started (process %d)", started.KeyringPID())
	return started, nil
}

// runPM runs the built pm binary with the scenario HOME. SSL_CERT_FILE makes
// the deployment CA the only trusted root of this pm process. The pm process
// is the only process that gets the private bus address of the keyring.
func (s *stackScenario) runPM(parent context.Context, caFile string, args []string) (int, string, string, error) {
	ctx, cancel := context.WithTimeout(parent, laneCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, s.harness.bins.pm, args...)
	command.Env = []string{"HOME=" + s.home, "PATH=/usr/bin:/bin", "LANG=C", "SSL_CERT_FILE=" + caFile}
	if s.keyring != nil {
		command.Env = append(command.Env, s.keyring.Environment()...)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if ctx.Err() != nil {
		return 0, "", "", fmt.Errorf("pm did not finish within %s: %q", laneCommandTimeout, args)
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, stdout.String(), stderr.String(), nil
	case errors.As(err, &exitErr):
		return exitErr.ExitCode(), stdout.String(), stderr.String(), nil
	default:
		return 0, "", "", fmt.Errorf("start pm: %w", err)
	}
}

func (r *commandResult) exitCodeIs(want int) error {
	if !r.ran {
		return errors.New("no lane command ran")
	}
	if r.exitCode != want {
		return fmt.Errorf("exit code %d, want %d\nstdout: %s\nstderr: %s", r.exitCode, want, r.stdout, r.stderr)
	}
	return nil
}

// stdoutHasDeploymentID reads the one instance ID that the lane command
// printed and compares it exactly with the deployment ID of the server that
// the command ran against. pm-server uses its deployment ID as its instance
// ID. The deployment ID in another field, or as a part of a longer value,
// does not count. An empty deployment ID fails.
func (r *commandResult) stdoutHasDeploymentID() error {
	if !r.ran {
		return errors.New("no lane command ran")
	}
	if r.deploymentID == "" {
		return errors.New("the lane command ran against a server with no deployment ID")
	}
	printed, err := r.printedInstanceID()
	if err != nil {
		return err
	}
	if printed != r.deploymentID {
		return fmt.Errorf("the lane command printed instance ID %q, want the lane deployment ID %q\nstdout: %s", printed, r.deploymentID, r.stdout)
	}
	return nil
}

// printedInstanceID reads the instance ID from the server information
// document that pm server info prints: one JSON object (JSON output), or one
// "key<TAB>JSON value" line for each field (text output). It fails unless the
// output has exactly one nonempty instance ID.
func (r *commandResult) printedInstanceID() (string, error) {
	var document struct {
		InstanceID string `json:"instance_id"`
	}
	if json.Unmarshal([]byte(r.stdout), &document) == nil && document.InstanceID != "" {
		return document.InstanceID, nil
	}
	var found []string
	for _, line := range strings.Split(r.stdout, "\n") {
		key, value, ok := strings.Cut(line, "\t")
		if !ok || key != "instance_id" {
			continue
		}
		var instanceID string
		if json.Unmarshal([]byte(value), &instanceID) != nil || instanceID == "" {
			return "", fmt.Errorf("the instance ID line in the lane command stdout is not a nonempty JSON string: %q", line)
		}
		found = append(found, instanceID)
	}
	if len(found) != 1 {
		return "", fmt.Errorf("the lane command stdout has %d instance ID lines, want 1\nstdout: %s", len(found), r.stdout)
	}
	return found[0], nil
}

func (r *commandResult) stderrContains(want string) error {
	if !r.ran {
		return errors.New("no lane command ran")
	}
	if !strings.Contains(r.stderr, want) {
		return fmt.Errorf("stderr does not contain %q\nstderr: %s", want, r.stderr)
	}
	return nil
}
