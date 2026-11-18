package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/tests/keyring"
)

// processCheckGuard ends a broken check of the login process bounds (#402
// review F1 to F3). A correct run ends long before it.
const processCheckGuard = laneCommandTimeout + killLimit

// releasePipe returns the path of a named pipe that a fake process reads one
// line from, so that it ends. The test holds the pipe open for read and
// write (O_RDWR): the open of the fake never waits, and the line never waits
// for a reader. The cleanup writes the line and closes the pipe. Register
// the cleanup that joins a goroutine before this call, so that the release
// runs first.
func releasePipe(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "release")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	pipe, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pipe.Write([]byte("release\n"))
		_ = pipe.Close()
	})
	return path
}

// joinOnCleanup registers a cleanup that waits, within the guard, until the
// returned channel closes. The goroutine of the check closes it when it ends.
func joinOnCleanup(t *testing.T, name string) chan struct{} {
	t.Helper()
	finished := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-finished:
		case <-time.After(processCheckGuard):
			t.Errorf("%s did not end after the release", name)
		}
	})
	return finished
}

// writeFakePM writes an executable shell script and returns its path.
func writeFakePM(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pm")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunAsUserKeepsTheCredentialOutOfTheDiagnostics (#402 review F1): a
// command that runs as the user can print a credential on a failure path.
// The step `I run "<command>" as the user` runs a fake pm that prints a
// synthetic credential. The shared exit code step and the K1b workspace
// oracle must fail without the credential in their messages.
func TestRunAsUserKeepsTheCredentialOutOfTheDiagnostics(t *testing.T) {
	fake := laneFakeServer(t)
	l := &lane{deployment: newLaneListenerDeployment(t), started: true}
	l.deployment.deploymentID = "pm-acceptance-1"
	startLaneFakeServer(t, l, fake, writeLaneFakeConfiguration(t, l.deployment.listen))
	worker := &lane{deployment: newLaneListenerDeployment(t)}
	startLaneFakeServer(t, worker, fake, writeLaneFakeConfiguration(t, worker.deployment.listen))
	l.worker = worker.server

	page := "items\t[{\"org_id\":\"acceptance-org-1\",\"workspace_id\":\"" + syntheticCredential + "\"}]\nnext_cursor\tnull\n"
	for _, test := range []struct {
		name   string
		script string
		check  func(*loginScenario, *bootstrapScenario) error
	}{
		{
			name:   "the exit code step after a failed command",
			script: "#!/bin/sh\nprintf '%s\\n' '" + syntheticCredential + "'\nprintf 'error: %s\\n' '" + syntheticCredential + "' >&2\nexit 3\n",
			check: func(s *loginScenario, _ *bootstrapScenario) error {
				return s.shared.result.exitCodeIs(0)
			},
		},
		{
			name:   "the workspace oracle with another workspace",
			script: "#!/bin/sh\nprintf '" + strings.ReplaceAll(page, "\t", "\\t") + "'\n",
			check: func(_ *loginScenario, b *bootstrapScenario) error {
				return b.namesBootstrappedWorkspace()
			},
		},
		{
			name:   "the workspace oracle with output that is not a page",
			script: "#!/bin/sh\nprintf 'session\\t%s\\n' '" + syntheticCredential + "'\n",
			check: func(_ *loginScenario, b *bootstrapScenario) error {
				return b.namesBootstrappedWorkspace()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			shared := &stackScenario{
				harness: &stackHarness{bins: binaries{pm: writeFakePM(t, test.script)}},
				home:    t.TempDir(),
				keyring: &keyring.Keyring{},
			}
			s := &loginScenario{shared: shared, lane: l, loggedIn: true}
			b := &bootstrapScenario{shared: shared, lane: l, first: &bootstrapInput{OrgID: "acceptance-org-1", WorkspaceID: "acceptance-workspace-1"}}
			if err := s.runAsUser(t.Context(), "pm workspaces list"); err != nil {
				t.Fatalf("run the fake pm as the user: %v", err)
			}
			err := test.check(s, b)
			if err == nil {
				t.Fatal("the step passed, want a failure")
			}
			checkNoForms(t, err.Error(), forbiddenSecretForms())
		})
	}
}

// TestRunPMProcessEndsWhenADescendantKeepsTheOutput (#402 review F3): pm
// exits, and a process that it started keeps the stdout pipe open. The wait
// of runPMProcess must still end, with a failure, and not wait for that
// process.
func TestRunPMProcessEndsWhenADescendantKeepsTheOutput(t *testing.T) {
	finished := joinOnCleanup(t, "runPMProcess")
	release := releasePipe(t)
	script := writeFakePM(t, "#!/bin/sh\n( read _ < \"$RELEASE\" ) &\nexit 0\n")
	environment := []string{"PATH=/usr/bin:/bin", "LANG=C", "RELEASE=" + release}
	ended := make(chan error, 1)
	go func() {
		defer close(finished)
		_, _, _, err := runPMProcess(t.Context(), script, nil, environment)
		ended <- err
	}()
	select {
	case err := <-ended:
		if err == nil || !strings.Contains(err.Error(), "kept the output open") {
			t.Fatalf("runPMProcess = %v, want the failure for the output that stayed open", err)
		}
	case <-time.After(processCheckGuard):
		t.Fatal("runPMProcess did not end while a process that pm started kept the output open")
	}
}
