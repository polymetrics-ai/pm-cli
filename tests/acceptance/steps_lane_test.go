package acceptance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/cucumber/godog"
)

const (
	laneProfileName    = "lane"
	laneCommandTimeout = 60 * time.Second
)

// laneScenario is the state of one lane scenario. The lane steps keep their
// own state, apart from the offline CLI steps in steps_cli_test.go.
type laneScenario struct {
	lane      *lane
	home      string
	workspace string
	ran       bool
	exitCode  int
	stdout    string
	stderr    string
	baseline  map[string]int64
}

func registerLaneSteps(sc *godog.ScenarioContext, l *lane) {
	scenario := &laneScenario{lane: l}
	sc.After(func(ctx context.Context, _ *godog.Scenario, err error) (context.Context, error) {
		if scenario.home != "" {
			if removeErr := os.RemoveAll(scenario.home); removeErr != nil && err == nil {
				return ctx, fmt.Errorf("remove the lane scenario home: %w", removeErr)
			}
		}
		return ctx, err
	})
	sc.Step(`^the lane runs pm-server and pm-worker on the job-owned stack$`, scenario.laneRuns)
	sc.Step(`^a pm server profile for the lane server$`, scenario.addProfile)
	sc.Step(`^I run "([^"]*)" against the lane server$`, scenario.runAgainstLane)
	sc.Step(`^the lane command exits with code (\d+)$`, scenario.exitCodeIs)
	sc.Step(`^the lane command stdout contains the lane deployment ID$`, scenario.stdoutHasDeploymentID)
	sc.Step(`^the lane command stderr contains "([^"]*)"$`, scenario.stderrContains)
	sc.Step(`^the observer sees a pm-worker poller on the worker task queue$`, scenario.observerSeesPoller)
	sc.Step(`^the observer sees no new row in the PM run and audit tables$`, scenario.observerSeesNoNewRow)
}

func (s *laneScenario) laneRuns(ctx context.Context) error {
	return s.lane.ensure(ctx)
}

// addProfile adds the lane server profile with the real pm binary, in a new
// HOME for this scenario. It also selects a new workspace ID for the
// scenario. Part a runs no authenticated command, so the server never
// creates that workspace.
func (s *laneScenario) addProfile(ctx context.Context) error {
	if !s.lane.started || s.lane.startErr != nil {
		return errors.New("the lane is not running")
	}
	home, err := os.MkdirTemp("", "pm-acceptance-lane-home-")
	if err != nil {
		return fmt.Errorf("create the lane scenario home: %w", err)
	}
	s.home = home
	s.workspace = "lane-workspace-" + randomHex(6)
	code, stdout, stderr, err := s.runPM(ctx, []string{
		"server", "profiles", "add", laneProfileName,
		"--url", s.lane.origin,
		"--instance-id", s.lane.deploymentID,
	})
	if err != nil {
		return err
	}
	if code != 0 || strings.TrimSpace(stdout) != laneProfileName {
		return fmt.Errorf("pm server profiles add exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	return nil
}

// runAgainstLane runs the command with the lane controls added at the end:
// the lane server profile, the explicit process-local memory session store
// and the scenario workspace. The memory store starts empty in each process,
// so the result does not depend on an OS keyring. The observer counts the run
// and audit rows just before the command runs.
func (s *laneScenario) runAgainstLane(ctx context.Context, line string) error {
	if s.home == "" {
		return errors.New("the lane server profile is not set")
	}
	args := strings.Fields(line)
	if len(args) == 0 || args[0] != "pm" {
		return fmt.Errorf("command must start with pm: %q", line)
	}
	baseline, err := s.lane.observer.countRows(ctx)
	if err != nil {
		return err
	}
	args = append(args[1:], "--server-profile", laneProfileName, "--session-store", "memory", "--workspace", s.workspace)
	code, stdout, stderr, err := s.runPM(ctx, args)
	if err != nil {
		return err
	}
	s.baseline = baseline
	s.ran, s.exitCode, s.stdout, s.stderr = true, code, stdout, stderr
	return nil
}

// runPM runs the built pm binary with the scenario HOME. SSL_CERT_FILE makes
// the lane CA the only trusted root of this pm process.
func (s *laneScenario) runPM(parent context.Context, args []string) (int, string, string, error) {
	ctx, cancel := context.WithTimeout(parent, laneCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, s.lane.bins.pm, args...)
	command.Env = []string{"HOME=" + s.home, "PATH=/usr/bin:/bin", "LANG=C", "SSL_CERT_FILE=" + s.lane.caFile}
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

func (s *laneScenario) exitCodeIs(want int) error {
	if !s.ran {
		return errors.New("no lane command ran")
	}
	if s.exitCode != want {
		return fmt.Errorf("exit code %d, want %d\nstdout: %s\nstderr: %s", s.exitCode, want, s.stdout, s.stderr)
	}
	return nil
}

func (s *laneScenario) stdoutHasDeploymentID() error {
	if !s.ran {
		return errors.New("no lane command ran")
	}
	if !strings.Contains(s.stdout, s.lane.deploymentID) {
		return fmt.Errorf("stdout does not contain the lane deployment ID %q\nstdout: %s", s.lane.deploymentID, s.stdout)
	}
	return nil
}

func (s *laneScenario) stderrContains(want string) error {
	if !s.ran {
		return errors.New("no lane command ran")
	}
	if !strings.Contains(s.stderr, want) {
		return fmt.Errorf("stderr does not contain %q\nstderr: %s", want, s.stderr)
	}
	return nil
}

// observerSeesPoller makes one observation through the observer's own
// Temporal client.
func (s *laneScenario) observerSeesPoller(ctx context.Context) error {
	return s.lane.observer.seesPoller(ctx, laneTaskQueue, s.lane.workerIdentity())
}

func (s *laneScenario) observerSeesNoNewRow(ctx context.Context) error {
	if s.baseline == nil {
		return errors.New("the observer has no count from before the lane command")
	}
	after, err := s.lane.observer.countRows(ctx)
	if err != nil {
		return err
	}
	var changed []string
	for _, table := range observedTables {
		if after[table] != s.baseline[table] {
			changed = append(changed, fmt.Sprintf("pm.%s %d -> %d", table, s.baseline[table], after[table]))
		}
	}
	slices.Sort(changed)
	if len(changed) > 0 {
		return fmt.Errorf("the observer sees changed row counts: %s", strings.Join(changed, ", "))
	}
	return nil
}
