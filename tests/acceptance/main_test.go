// Package acceptance runs the Gherkin acceptance suite against real PM
// binaries. It is the only behavioral gate in CI (AGENTS.md).
package acceptance

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/cucumber/godog"
)

// binaries holds the repository root, the go tool and the pm binary built
// once for the suite. The lane builds pm-server and pm-worker only when a
// @stack scenario runs.
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
	lane := newLane(bins)
	t.Cleanup(func() {
		if err := lane.close(); err != nil {
			t.Errorf("stop the acceptance lane: %v", err)
		}
	})
	suite := godog.TestSuite{
		Name: "pm",
		ScenarioInitializer: func(sc *godog.ScenarioContext) {
			registerCLISteps(sc, bins)
			registerLaneSteps(sc, lane)
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if bins.pm, err = buildBinary(ctx, bins, t.TempDir(), "pm", "./cmd/pm"); err != nil {
		t.Fatal(err)
	}
	return bins
}

// buildBinary compiles one command of this module without cgo, as the
// Makefile builds pm, pm-server and pm-worker.
func buildBinary(ctx context.Context, bins binaries, dir, name, pkg string) (string, error) {
	output := filepath.Join(dir, name)
	cmd := exec.CommandContext(ctx, bins.goTool, "build", "-o", output, pkg)
	cmd.Dir = bins.root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build %s: %w\n%s", name, err, out)
	}
	return output, nil
}
