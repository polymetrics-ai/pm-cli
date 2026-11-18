package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mainProcessVariable makes the test binary run main() with the arguments in
// mainArgumentsVariable, in its own process.
const (
	mainProcessVariable   = "PM_STACK_TEST_RUN_MAIN"
	mainArgumentsVariable = "PM_STACK_TEST_MAIN_ARGUMENTS"
)

func TestMain(m *testing.M) {
	if os.Getenv(mainProcessVariable) == "1" {
		var arguments []string
		if err := json.Unmarshal([]byte(os.Getenv(mainArgumentsVariable)), &arguments); err != nil {
			os.Exit(3)
		}
		os.Args = append([]string{"pm-stack"}, arguments...)
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Review F9 and round 2 F1: pm-stack up prints the fixed failed-step line
// also when it fails before it starts a dependency: while it parses its
// arguments, or opens, loads or validates the configuration. The check runs
// main() of pm-stack in its own process and reads its stderr and exit status.
func TestUpConfigurationFailurePrintsItsStep(t *testing.T) {
	directory := t.TempDir()
	malformed := filepath.Join(directory, "malformed.json")
	if err := os.WriteFile(malformed, []byte("{not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(directory, "invalid.json")
	if err := os.WriteFile(invalid, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "malformed configuration", args: []string{"up", "--config", malformed}},
		{name: "configuration that does not validate", args: []string{"up", "--config", invalid}},
		// Round 2 F1: argument-parsing failures of an explicit up.
		{name: "missing configuration path", args: []string{"up", "--config"}},
		{name: "missing dependency selection", args: []string{"up", "--dependency"}},
		{name: "repeated configuration flag", args: []string{"up", "--config", invalid, "--config", invalid}},
		{name: "repeated dependency flag", args: []string{"up", "--dependency", "all", "--dependency", "all"}},
		{name: "unknown flag", args: []string{"up", "--verbose"}},
		{name: "unknown argument", args: []string{"up", "postgres"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			lines := runMainFailure(t, test.args)
			if len(lines) != 2 || !strings.HasPrefix(lines[0], "pm-stack: ") || lines[1] != "pm-stack: failed step configuration" {
				t.Fatalf("stderr = %q, want the error line and then exactly %q", lines, "pm-stack: failed step configuration")
			}
		})
	}
}

// Round 2 F1: only an explicit up gets the failed-step line. A parsing
// failure of another action prints its error line only.
func TestOtherActionFailurePrintsNoStep(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "check with a missing configuration path", args: []string{"check", "--config"}},
		{name: "check with a dependency selection", args: []string{"check", "--dependency", "all"}},
		{name: "drain with an unknown flag", args: []string{"drain", "--verbose"}},
		{name: "unsupported action", args: []string{"upgrade"}},
		{name: "no action", args: []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			lines := runMainFailure(t, test.args)
			if len(lines) != 1 || !strings.HasPrefix(lines[0], "pm-stack: ") || strings.Contains(lines[0], "failed step") {
				t.Fatalf("stderr = %q, want exactly one error line and no failed step", lines)
			}
		})
	}
}

// runMainFailure runs main() of pm-stack with args in its own process. It
// needs exit status 1 and returns the stderr lines.
func runMainFailure(t *testing.T, args []string) []string {
	t.Helper()
	arguments, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0])
	command.Env = append(os.Environ(), mainProcessVariable+"=1", mainArgumentsVariable+"="+string(arguments))
	var stderr bytes.Buffer
	command.Stderr = &stderr
	err = command.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("pm-stack %s = %v, want exit status 1\nstderr: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
}
