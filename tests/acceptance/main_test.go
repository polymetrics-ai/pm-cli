// Package acceptance runs the Gherkin acceptance suite against real PM
// binaries. It is the only behavioral gate in CI (AGENTS.md).
package acceptance

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cucumber/godog"
)

// binaries holds the paths of the PM binaries built once for the suite.
type binaries struct {
	pm string
}

// TestFeatures runs every scenario in features/. Strict mode fails the run on
// an undefined or pending step. PM_ACCEPTANCE_TAGS selects scenarios by tag.
func TestFeatures(t *testing.T) {
	bins := buildBinaries(t)
	suite := godog.TestSuite{
		Name: "pm",
		ScenarioInitializer: func(sc *godog.ScenarioContext) {
			registerCLISteps(sc, bins)
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
	dir := t.TempDir()
	pm := filepath.Join(dir, "pm")
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("find the go tool on PATH: %v", err)
	}
	cmd := exec.Command(goTool, "build", "-o", pm, "./cmd/pm")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build pm: %v\n%s", err, out)
	}
	return binaries{pm: pm}
}
