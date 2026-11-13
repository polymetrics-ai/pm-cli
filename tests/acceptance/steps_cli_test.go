package acceptance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/cucumber/godog"
)

// cliRun records one real pm process: its exit code and both output streams.
type cliRun struct {
	bins     binaries
	home     string
	exitCode int
	stdout   string
	stderr   string
	ran      bool
}

func registerCLISteps(sc *godog.ScenarioContext, bins binaries) {
	registerPmgenSteps(sc)
	run := &cliRun{bins: bins}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		home, err := os.MkdirTemp("", "pm-acceptance-home-")
		if err != nil {
			return ctx, fmt.Errorf("create scenario home: %w", err)
		}
		run.home = home
		return ctx, nil
	})
	sc.After(func(ctx context.Context, _ *godog.Scenario, err error) (context.Context, error) {
		if run.home != "" {
			if removeErr := os.RemoveAll(run.home); removeErr != nil && err == nil {
				return ctx, fmt.Errorf("remove scenario home: %w", removeErr)
			}
		}
		return ctx, err
	})
	sc.Step(`^I run "([^"]*)"$`, run.runCommand)
	sc.Step(`^the exit code is (\d+)$`, run.exitCodeIs)
	sc.Step(`^stdout is not empty$`, run.stdoutNotEmpty)
	sc.Step(`^stdout contains "([^"]*)"$`, run.streamContains("stdout"))
	sc.Step(`^stderr contains "([^"]*)"$`, run.streamContains("stderr"))
}

// runCommand executes the built pm binary with an isolated HOME and no
// inherited PM configuration, so a scenario never reads a developer profile.
func (r *cliRun) runCommand(line string) error {
	args := strings.Fields(line)
	if len(args) == 0 || args[0] != "pm" {
		return fmt.Errorf("command must start with pm: %q", line)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.bins.pm, args[1:]...)
	cmd.Env = []string{"HOME=" + r.home, "PATH=/usr/bin:/bin", "LANG=C"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	r.stdout, r.stderr, r.ran = stdout.String(), stderr.String(), true
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		r.exitCode = 0
	case errors.As(err, &exitErr):
		r.exitCode = exitErr.ExitCode()
	default:
		return fmt.Errorf("start pm: %w", err)
	}
	if ctx.Err() != nil {
		return fmt.Errorf("pm did not finish within 30s: %q", line)
	}
	return nil
}

func (r *cliRun) exitCodeIs(want int) error {
	if !r.ran {
		return errors.New("no command ran")
	}
	if r.exitCode != want {
		return fmt.Errorf("exit code %d, want %d\nstdout: %s\nstderr: %s", r.exitCode, want, r.stdout, r.stderr)
	}
	return nil
}

func (r *cliRun) stdoutNotEmpty() error {
	if strings.TrimSpace(r.stdout) == "" {
		return errors.New("stdout is empty")
	}
	return nil
}

func (r *cliRun) streamContains(stream string) func(string) error {
	return func(want string) error {
		got := r.stdout
		if stream == "stderr" {
			got = r.stderr
		}
		if !strings.Contains(got, want) {
			return fmt.Errorf("%s does not contain %q\n%s: %s", stream, want, stream, got)
		}
		return nil
	}
}
