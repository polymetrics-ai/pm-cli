package acceptance

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/tests/keyring"
)

// processCheckGuard ends a broken check of the login process bounds (#402
// review F1 to F3, R2-F1 and R2-F2). A correct run ends long before it.
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

// descendantLine is the shell line of a fake descendant: a process that a
// fake pm starts in the background and leaves running (#402 review R2-F2).
// It keeps the stdout and the stderr of the fake pm open. It writes
// "started" to the end pipe, reads one line from the release pipe and
// writes "ended" to the end pipe as its last command.
const descendantLine = `( echo started > "$END"; read _ < "$RELEASE"; echo ended > "$END" ) &`

// fakeDescendant holds the environment of a fake descendant and the lines
// that the reader of its end pipe got.
type fakeDescendant struct {
	environment []string
	lines       <-chan string
}

// startFakeDescendant makes the release pipe and the end pipe of a fake
// descendant and starts the reader of the end pipe. Call it before the start
// of the fake pm. The test holds both named pipes open for read and write
// (O_RDWR), so no open waits. The cleanup writes the release line and waits,
// within the guard, for the "ended" line. When that line does not come, the
// cleanup fails the test and ends the reader with a stop line. Then it joins
// the reader. The release pipe closes after that wait, so a late open of the
// release pipe still gets the line.
func startFakeDescendant(t *testing.T, name string) *fakeDescendant {
	t.Helper()
	dir := t.TempDir()
	releasePath, endPath := filepath.Join(dir, "release"), filepath.Join(dir, "end")
	for _, path := range []string{releasePath, endPath} {
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	release, err := os.OpenFile(releasePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = release.Close() })
	end, err := os.OpenFile(endPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 3)
	t.Cleanup(func() {
		_, _ = release.Write([]byte("release\n"))
		guard := time.NewTimer(processCheckGuard)
		defer guard.Stop()
		if !awaitLine(lines, "ended\n", guard.C) {
			t.Errorf("%s did not end within %s after the release", name, processCheckGuard)
			_, _ = end.Write([]byte("stop\n"))
		}
		for range lines {
		}
		_ = end.Close()
	})
	go func() {
		defer close(lines)
		reader := bufio.NewReader(end)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			lines <- line
			if line == "ended\n" || line == "stop\n" {
				return
			}
		}
	}()
	return &fakeDescendant{environment: []string{"RELEASE=" + releasePath, "END=" + endPath}, lines: lines}
}

// waitStart waits, within the guard, for the "started" line.
func (d *fakeDescendant) waitStart() error {
	guard := time.NewTimer(processCheckGuard)
	defer guard.Stop()
	if !awaitLine(d.lines, "started\n", guard.C) {
		return errors.New("the fake descendant did not start")
	}
	return nil
}

// awaitLine reads lines until it gets want. It reports false when the
// channel closes or the timer fires first.
func awaitLine(lines <-chan string, want string, timer <-chan time.Time) bool {
	for {
		select {
		case line, open := <-lines:
			if !open {
				return false
			}
			if line == want {
				return true
			}
		case <-timer:
			return false
		}
	}
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
// process. The cleanup joins that process (#402 review R2-F2).
func TestRunPMProcessEndsWhenADescendantKeepsTheOutput(t *testing.T) {
	finished := joinOnCleanup(t, "runPMProcess")
	descendant := startFakeDescendant(t, "the process that the fake pm started")
	script := writeFakePM(t, "#!/bin/sh\n"+descendantLine+"\nexit 0\n")
	environment := append([]string{"PATH=/usr/bin:/bin", "LANG=C"}, descendant.environment...)
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
