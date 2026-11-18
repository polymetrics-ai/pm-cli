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

// Review F9: pm-stack up prints the fixed failed-step line also when it fails
// before it starts a dependency: while it opens, loads or validates the
// configuration. The check runs main() of pm-stack in its own process and
// reads its stderr and exit status.
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
	} {
		t.Run(test.name, func(t *testing.T) {
			arguments, err := json.Marshal(test.args)
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
				t.Fatalf("pm-stack %s = %v, want exit status 1\nstderr: %s", strings.Join(test.args, " "), err, stderr.String())
			}
			lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
			if len(lines) != 2 || !strings.HasPrefix(lines[0], "pm-stack: ") || lines[1] != "pm-stack: failed step configuration" {
				t.Fatalf("stderr = %q, want the error line and then exactly %q", stderr.String(), "pm-stack: failed step configuration")
			}
		})
	}
}
