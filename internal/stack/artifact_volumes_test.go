package stack

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	freshMasterStatus  = `{"Topology":{"Max":4,"Free":4,"Layouts":null},"Version":"4.46"}`
	readyMasterStatus  = `{"Topology":{"Max":4,"Free":2,"Layouts":[{"replication":"000","collection":"pm-development-artifacts","writables":[1]},{"replication":"000","collection":"","writables":[2]}]}}`
	bucketOnlyStatus   = `{"Topology":{"Max":4,"Free":3,"Layouts":[{"replication":"000","collection":"pm-development-artifacts","writables":[1]}]}}`
	bucketGrowCall     = "http://seaweed-master:9333/vol/grow?collection=pm-development-artifacts&count=1"
	defaultGrowCall    = "http://seaweed-master:9333/vol/grow?count=1"
	unjoinedStatus     = `{"Topology":{"Max":0,"Free":0,"Layouts":null}}`
	bucketMissingError = "artifact bucket has no writable volume"
)

// TestEnsureArtifactVolumes runs ensureArtifactVolumes against fake python3
// (docker compose through tools/bootstrap.py) and docker programs on PATH.
// The fake master answers /dir/status with one document before the first
// grow call and with a second document after it. It records each grow call.
func TestEnsureArtifactVolumes(t *testing.T) {
	for _, test := range []struct {
		name      string
		grow      bool
		before    string
		after     string
		growExit  int
		statusErr bool
		wantGrows []string
		wantError string
	}{
		{
			name:      "a fresh master grows one volume for each empty layout",
			grow:      true,
			before:    freshMasterStatus,
			after:     readyMasterStatus,
			wantGrows: []string{bucketGrowCall, defaultGrowCall},
		},
		{
			name:   "writable layouts grow nothing",
			grow:   true,
			before: readyMasterStatus,
		},
		{
			name:      "only the empty layout grows",
			grow:      true,
			before:    bucketOnlyStatus,
			after:     readyMasterStatus,
			wantGrows: []string{defaultGrowCall},
		},
		{
			name:      "a refused grow fails with the named error",
			grow:      true,
			before:    freshMasterStatus,
			growExit:  22,
			wantGrows: []string{bucketGrowCall},
			wantError: bucketMissingError,
		},
		{
			name:      "a grow of no volume fails with the named error",
			grow:      true,
			before:    freshMasterStatus,
			after:     freshMasterStatus,
			wantGrows: []string{bucketGrowCall, defaultGrowCall},
			wantError: bucketMissingError,
		},
		{
			name:      "the default collection must also have a writable volume",
			grow:      true,
			before:    bucketOnlyStatus,
			after:     bucketOnlyStatus,
			wantGrows: []string{defaultGrowCall},
			wantError: "default collection has no writable volume",
		},
		{
			name:      "an unreadable status fails",
			grow:      true,
			statusErr: true,
			wantError: "read the artifact volume status",
		},
		{
			name:      "a status that is not JSON fails",
			grow:      true,
			before:    "not json",
			wantError: "read the artifact volume status",
		},
		{
			name:      "a status without a topology fails",
			grow:      true,
			before:    `{"Version":"4.46"}`,
			wantError: "read the artifact volume status",
		},
		{
			name: "only the exact collection names the bucket layout",
			grow: true,
			before: `{"Topology":{"Max":4,"Free":1,"Layouts":[` +
				`{"collection":"pm-development-artifacts-old","writables":[1]},` +
				`{"collection":"PM-DEVELOPMENT-ARTIFACTS","writables":[2]},` +
				`{"collection":"","writables":[3]}]}}`,
			after:     readyMasterStatus,
			wantGrows: []string{bucketGrowCall},
		},
		{
			name:   "the check form passes on writable layouts",
			before: readyMasterStatus,
		},
		{
			name:      "the check form fails and grows nothing",
			before:    freshMasterStatus,
			wantError: bucketMissingError,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := installFakeMaster(t, test.before, test.after, test.growExit, test.statusErr)
			rt := &runtime{root: fake, project: "pm-artifact-volumes-test", stdout: io.Discard, stderr: io.Discard}
			err := ensureArtifactVolumes(context.Background(), noRecreateTestConfiguration(t), rt, test.grow)
			switch {
			case test.wantError == "" && err != nil:
				t.Fatalf("ensureArtifactVolumes: %v", err)
			case test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)):
				t.Fatalf("ensureArtifactVolumes error = %v, want one that contains %q", err, test.wantError)
			}
			if got := recordedMasterCalls(t, fake, "grow.log"); !slices.Equal(got, test.wantGrows) {
				t.Fatalf("grow calls = %q, want %q", got, test.wantGrows)
			}
			for _, call := range recordedMasterCalls(t, fake, "exec.log") {
				if !strings.Contains(call, " exec -T seaweed-master curl -fsS -m 10 http://seaweed-master:9333/") {
					t.Fatalf("master call does not run curl inside the master container: %q", call)
				}
			}
		})
	}
}

// TestCheckArtifactVolumeCapacity checks the read-only fact that the start-up
// wait of pm-stack up waits for: the volume server has joined the master.
func TestCheckArtifactVolumeCapacity(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  string
		wantErr bool
	}{
		{"a joined volume server reports capacity", freshMasterStatus, false},
		{"no joined volume server", unjoinedStatus, true},
		{"a status that is not JSON", "not json", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := installFakeMaster(t, test.status, "", 0, false)
			rt := &runtime{root: fake, project: "pm-artifact-volumes-test", stdout: io.Discard, stderr: io.Discard}
			err := checkArtifactVolumeCapacity(context.Background(), noRecreateTestConfiguration(t), rt)
			if (err != nil) != test.wantErr {
				t.Fatalf("checkArtifactVolumeCapacity error = %v, want error %t", err, test.wantErr)
			}
			if got := recordedMasterCalls(t, fake, "grow.log"); len(got) != 0 {
				t.Fatalf("the capacity check grew volumes: %q", got)
			}
		})
	}
}

// installFakeMaster writes fake python3 and docker programs and puts them
// first on PATH. The fake python3 answers each compose exec call to the
// master: /dir/status prints the before document until a grow call was made,
// and the after document then. /vol/grow records the URL and exits with
// growExit. It returns the directory that holds the documents and the logs.
func installFakeMaster(t *testing.T, before, after string, growExit int, statusErr bool) string {
	t.Helper()
	directory := t.TempDir()
	bin := filepath.Join(directory, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(directory, "status-before"), before, 0o600)
	writeTestFile(t, filepath.Join(directory, "status-after"), after, 0o600)
	statusExit := 0
	if statusErr {
		statusExit = 7
	}
	writeTestFile(t, filepath.Join(bin, "python3"), `#!/bin/sh
for argument in "$@"; do
  url=$argument
done
printf '%s\n' "$*" >> "$FAKE_MASTER_DIR/exec.log"
case "$url" in
  */dir/status)
    if [ `+strconv.Itoa(statusExit)+` -ne 0 ]; then
      exit `+strconv.Itoa(statusExit)+`
    fi
    if [ -f "$FAKE_MASTER_DIR/grow.log" ]; then
      cat "$FAKE_MASTER_DIR/status-after"
    else
      cat "$FAKE_MASTER_DIR/status-before"
    fi
    ;;
  */vol/grow\?*)
    printf '%s\n' "$url" >> "$FAKE_MASTER_DIR/grow.log"
    if [ `+strconv.Itoa(growExit)+` -ne 0 ]; then
      exit `+strconv.Itoa(growExit)+`
    fi
    printf '%s\n' '{"count":1}'
    ;;
  *)
    exit 64
    ;;
esac
`, 0o700)
	writeTestFile(t, filepath.Join(bin, "docker"), `#!/bin/sh
case "$1" in
  info)
    printf '%s\n' '["name=seccomp,profile=builtin"]'
    ;;
  *)
    exit 64
    ;;
esac
`, 0o700)
	t.Setenv("FAKE_MASTER_DIR", directory)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
	return directory
}

// recordedMasterCalls returns the lines of one log of the fake master, or no
// lines when the log does not exist.
func recordedMasterCalls(t *testing.T, directory, name string) []string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(directory, name))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read the recorded master calls: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(contents)), "\n")
}
