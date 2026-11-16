// Package acceptance runs the Gherkin acceptance suite against real PM
// binaries. It is the only behavioral gate in CI (AGENTS.md).
package acceptance

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cucumber/godog"
)

// binaries holds the repository root, the go tool and the pm binary built
// once for the suite. The shared build step builds pm-server and pm-worker
// only when a scenario starts one of them.
type binaries struct {
	root   string
	goTool string
	pm     string
}

// TestFeatures runs every scenario in features/. Strict mode fails the run on
// an undefined or pending step. PM_ACCEPTANCE_TAGS selects scenarios by tag.
// Scenarios tagged @stack need the job-owned stack; CI runs them inside
// scripts/ci/with-stack-fixture.py.
func TestFeatures(t *testing.T) {
	bins := buildBinaries(t)
	stack := newStackHarness(bins, t.TempDir())
	lane := newLane(bins)
	t.Cleanup(func() {
		if err := lane.close(); err != nil {
			t.Errorf("stop the acceptance lane: %v", err)
		}
	})
	suite := godog.TestSuite{
		Name: "pm",
		ScenarioInitializer: func(sc *godog.ScenarioContext) {
			registerCLISteps(sc, bins, stack, lane)
		},
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"features"},
			Tags:     os.Getenv("PM_ACCEPTANCE_TAGS"),
			Strict:   true,
			TestingT: t,
		},
	}
	if status := suite.Run(); status != 0 {
		t.Fatalf("acceptance suite failed with status %d", status)
	}
}

// buildBinaries compiles the real pm binary with the go tool on PATH (the
// pinned toolchain under tools/bootstrap.py exec). Scenarios execute it as a separate process.
func buildBinaries(t *testing.T) binaries {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("find the go tool on PATH: %v", err)
	}
	bins := binaries{root: root, goTool: goTool}
	ctx, cancel := context.WithTimeout(context.Background(), buildLimit)
	defer cancel()
	if bins.pm, err = buildBinary(ctx, bins, t.TempDir(), "pm", "./cmd/pm", false); err != nil {
		t.Fatal(err)
	}
	return bins
}

// buildBinary compiles one command of this module, with cgo only when cgo is
// true. It waits once for the go tool to exit, up to the deadline of ctx (the
// build limit). The go tool writes its output to a file, not to a pipe. So
// Cmd.Wait starts no copy of the output and waits only for the exit, and the
// deadline also ends the read of the build output. The file is read after the
// exit, at most laneOutputLimit bytes.
func buildBinary(ctx context.Context, bins binaries, dir, name, pkg string, cgo bool) (string, error) {
	output := filepath.Join(dir, name)
	log, err := os.CreateTemp(dir, name+"-build-*.log")
	if err != nil {
		return "", fmt.Errorf("create the %s build log: %w", name, err)
	}
	defer func() {
		_ = log.Close()
		_ = os.Remove(log.Name())
	}()
	cgoEnabled := "0"
	if cgo {
		cgoEnabled = "1"
	}
	cmd := exec.CommandContext(ctx, bins.goTool, "build", "-o", output, pkg)
	cmd.Dir = bins.root
	cmd.Env = append(os.Environ(), "CGO_ENABLED="+cgoEnabled)
	cmd.Stdout = log
	cmd.Stderr = log
	runErr := cmd.Run()
	if runErr == nil {
		return output, nil
	}
	text, readErr := io.ReadAll(io.NewSectionReader(log, 0, laneOutputLimit))
	if readErr != nil {
		text = []byte(fmt.Sprintf("(read the build log: %v)", readErr))
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("build %s did not finish before its deadline: %w\n%s", name, ctx.Err(), text)
	}
	return "", fmt.Errorf("build %s: %w\n%s", name, runErr, text)
}
