package stack

import (
	"context"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	freshMasterStatus  = `{"Topology":{"Max":4,"Free":4,"Layouts":null},"Version":"4.46"}`
	readyMasterStatus  = `{"Topology":{"Max":4,"Free":2,"Layouts":[{"replication":"000","collection":"pm-development-artifacts","writables":[1]},{"replication":"000","collection":"","writables":[2]}]}}`
	bucketOnlyStatus   = `{"Topology":{"Max":4,"Free":3,"Layouts":[{"replication":"000","collection":"pm-development-artifacts","writables":[1]}]}}`
	bucketGrowCall     = "http://seaweed-master:9333/vol/grow?collection=pm-development-artifacts&count=1"
	defaultGrowCall    = "http://seaweed-master:9333/vol/grow?count=1"
	unjoinedStatus     = `{"Topology":{"Max":0,"Free":0,"Layouts":null}}`
	bucketMissingError = "artifact bucket has no writable volume"
	// The texts of a grow request that got no success answer.
	bucketGrowFailedError  = "no success answer arrived for the grow request of the artifact bucket. A volume may have grown. Run pm-stack up again. It reads the volume status before it grows."
	defaultGrowFailedError = "no success answer arrived for the grow request of the default collection. A volume may have grown. Run pm-stack up again. It reads the volume status before it grows."
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
			wantError: bucketGrowFailedError,
		},
		{
			name:      "a refused grow of the default collection fails with its own text",
			grow:      true,
			before:    bucketOnlyStatus,
			growExit:  22,
			wantGrows: []string{defaultGrowCall},
			wantError: defaultGrowFailedError,
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
			wantError: "cannot read the artifact volume status",
		},
		{
			name:      "a status that is not JSON fails",
			grow:      true,
			before:    "not json",
			wantError: "cannot read the artifact volume status",
		},
		{
			name:      "a status without a topology fails",
			grow:      true,
			before:    `{"Version":"4.46"}`,
			wantError: "cannot read the artifact volume status",
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

// TestEnsureArtifactVolumesEndsAHungGrowAtTheCallLimit runs the grow step
// against a fake master whose /vol/grow call never answers: the fake starts a
// child that waits on a FIFO that no process writes, and the child keeps the
// output of the call open. Defect it catches: a grow call that does not end
// when its context ends, or that waits for the output of a child after that,
// blocks pm-stack up until the caller stops it. The step must end with an
// error that does not say that no volume grew, after exactly one grow call.
//
// The call limit here is one hour, so no call of the step can meet it: the
// status read and the start of each fake process take as long as the host
// needs. The grow call ends because the test ends the context of the step at
// one product event: the fake has recorded the grow call. An earlier version
// used a 3 s call limit for every call, so on a loaded host the status read
// met that limit on correct code. TestSeaweedMasterEndsAHungCallAtItsLimit
// checks the call limit itself.
func TestEnsureArtifactVolumesEndsAHungGrowAtTheCallLimit(t *testing.T) {
	fake := installHungGrowMaster(t, freshMasterStatus)
	rt := &runtime{root: fake, project: "pm-artifact-volumes-test", stdout: io.Discard, stderr: io.Discard,
		masterCallLimit: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := growStarted(t, fake)
	done := make(chan error, 1)
	go func() {
		done <- ensureArtifactVolumes(ctx, noRecreateTestConfiguration(t), rt, true)
	}()
	var err error
	// The one guard: a step that never returns fails the test. It measures no
	// time of a call.
	guard := time.After(30 * time.Second)
	select {
	case <-started:
		cancel()
	case err = <-done:
		t.Fatalf("the grow step returned %v before the fake recorded a grow call", err)
	case <-guard:
		t.Fatal("the fake recorded no grow call")
	}
	select {
	case err = <-done:
	case <-guard:
		t.Fatal("the grow step did not return after its context ended: the hung master call does not end")
	}
	const want = bucketGrowFailedError
	if err == nil || err.Error() != want {
		t.Fatalf("ensureArtifactVolumes error = %v, want %q", err, want)
	}
	if got := recordedMasterCalls(t, fake, "grow.log"); !slices.Equal(got, []string{bucketGrowCall}) {
		t.Fatalf("grow calls = %q, want exactly %q", got, bucketGrowCall)
	}
}

// TestSeaweedMasterEndsAHungCallAtItsLimit calls the master once, with a
// call limit of 1 s and no deadline of the caller, against the fake whose
// /vol/grow call never answers and whose child keeps the output open.
// Defect it catches: a master call with no time limit of its own, or one that
// waits for the output of a child after its limit. Whichever process of the
// call runs when the limit comes (docker or python3), the call must end with
// an error. The result does not depend on how fast a process starts.
func TestSeaweedMasterEndsAHungCallAtItsLimit(t *testing.T) {
	fake := installHungGrowMaster(t, freshMasterStatus)
	rt := &runtime{root: fake, project: "pm-artifact-volumes-test", stdout: io.Discard, stderr: io.Discard,
		masterCallLimit: time.Second}
	// Read the start signal, so that a fast call reaches the child that keeps
	// the output open, and the output wait after the limit is in the check.
	growStarted(t, fake)
	done := make(chan error, 1)
	go func() {
		query := url.Values{"collection": {"pm-development-artifacts"}, "count": {"1"}}
		_, err := rt.seaweedMaster(context.Background(), noRecreateTestConfiguration(t), "/vol/grow", query)
		done <- err
	}()
	// The one guard: a call that never returns fails the test. The limit is
	// 1 s and the output wait after it is masterCallWaitDelay.
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the hung master call succeeded, want an error at its limit")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the master call did not return: the hung call has no effective time limit")
	}
}

// growStarted returns a channel that closes when the fake master has recorded
// a grow call. The fake writes to the FIFO grow.started after it records the
// call and before it waits. The read ends at that write, or at the cleanup.
func growStarted(t *testing.T, directory string) <-chan struct{} {
	t.Helper()
	fifo := filepath.Join(directory, "grow.started")
	started := make(chan struct{})
	go func() {
		if reader, err := os.Open(fifo); err == nil {
			buffer := make([]byte, 1)
			if n, _ := reader.Read(buffer); n == 1 {
				close(started)
			}
			_ = reader.Close()
		}
	}()
	t.Cleanup(func() {
		if writer, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = writer.Close()
		}
	})
	return started
}

// installHungGrowMaster writes fake python3 and docker programs and puts them
// first on PATH. /dir/status prints the status document. /vol/grow records
// its URL, writes to the FIFO grow.started (it waits there until a reader
// opens it), and then runs cat on a FIFO that no process writes, so the call
// never ends and cat keeps its output open. A cleanup opens the FIFO for
// writing once, so a waiting cat ends with the test.
func installHungGrowMaster(t *testing.T, status string) string {
	t.Helper()
	directory := t.TempDir()
	bin := filepath.Join(directory, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(directory, "grow.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(directory, "grow.started"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if writer, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = writer.Close()
		}
	})
	writeTestFile(t, filepath.Join(directory, "status"), status, 0o600)
	writeTestFile(t, filepath.Join(bin, "python3"), `#!/bin/sh
for argument in "$@"; do
  url=$argument
done
case "$url" in
  */dir/status)
    cat "$FAKE_MASTER_DIR/status"
    ;;
  */vol/grow\?*)
    printf '%s\n' "$url" >> "$FAKE_MASTER_DIR/grow.log"
    printf 'started' > "$FAKE_MASTER_DIR/grow.started"
    cat "$FAKE_MASTER_DIR/grow.fifo"
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

// TestStartArtifactStoreGrowsTheVolumesBeforeTheFilerStarts runs
// startArtifacts (startArtifactStore, then the bucket wait) against fake
// python3 (docker compose through tools/bootstrap.py) and docker programs on
// PATH. The fakes record each
// compose up call, each /vol/grow call and the bucket step in one ordered
// log. Defect it catches (#381): pm-stack up grows the artifact volumes after
// seaweed-filer starts. The filer writes its metadata log to the default
// collection about one minute after it starts. In SeaweedFS 4.46 that write
// grows a default volume when the default collection has no writable volume.
// When pm-stack reads the master status before that growth and grows after
// it, the stack is ready with two default volumes and only one spare slot.
func TestStartArtifactStoreGrowsTheVolumesBeforeTheFilerStarts(t *testing.T) {
	for _, test := range []struct {
		name   string
		before string
		after  string
		want   []string
	}{
		{
			name:   "a fresh master grows both volumes before the filer starts",
			before: freshMasterStatus,
			after:  readyMasterStatus,
			want: []string{
				"up seaweed-master",
				"up seaweed-volume",
				"grow " + bucketGrowCall,
				"grow " + defaultGrowCall,
				"up seaweed-filer",
				"up seaweed-s3-a seaweed-s3-b caddy",
				"bucket",
			},
		},
		{
			name:   "a second up with writable volumes grows nothing",
			before: readyMasterStatus,
			want: []string{
				"up seaweed-master",
				"up seaweed-volume",
				"up seaweed-filer",
				"up seaweed-s3-a seaweed-s3-b caddy",
				"bucket",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := installFakeArtifactStore(t, test.before, test.after)
			rt := &runtime{root: fake, project: "pm-artifact-store-test", stdout: io.Discard, stderr: io.Discard}
			ensureBucket := func(context.Context) error {
				return appendTestLine(filepath.Join(fake, "order.log"), "bucket")
			}
			step := "before"
			if err := startArtifacts(context.Background(), noRecreateTestConfiguration(t), rt, &step, ensureBucket); err != nil {
				t.Fatalf("startArtifacts: %v", err)
			}
			if got := recordedMasterCalls(t, fake, "order.log"); !slices.Equal(got, test.want) {
				t.Fatalf("artifact store start order = %q, want %q: each /vol/grow must come before seaweed-filer starts", got, test.want)
			}
		})
	}
}

// TestArtifactStartFailuresReportTheirStep checks the fixed step codes of
// startArtifacts. A failure of the services, the capacity wait or the volume
// grow reports artifacts-start. A failure of the bucket wait reports
// artifact-bucket. The launcher accepts only these listed codes
// (STACK_FAILED_STEP in scripts/ci/with-stack-fixture.py).
func TestArtifactStartFailuresReportTheirStep(t *testing.T) {
	notJoined := `{"Topology":{"Max":0,"Free":0,"Layouts":null},"Version":"4.46"}`
	for _, test := range []struct {
		name     string
		before   string
		after    string
		limit    time.Duration
		bucket   bool
		wantStep string
		wantLog  []string
	}{
		{
			// The context limit ends the capacity wait. Both possible end
			// points, the service starts and the wait, are artifacts-start.
			name:     "the capacity wait ends before the volume server joins",
			before:   notJoined,
			after:    notJoined,
			limit:    2 * time.Second,
			wantStep: "artifacts-start",
			wantLog:  []string{"up seaweed-master", "up seaweed-volume"},
		},
		{
			name:     "the volume grow leaves no writable volume",
			before:   freshMasterStatus,
			after:    freshMasterStatus,
			wantStep: "artifacts-start",
			wantLog:  []string{"up seaweed-master", "up seaweed-volume", "grow " + bucketGrowCall, "grow " + defaultGrowCall},
		},
		{
			name:     "the bucket step fails",
			before:   readyMasterStatus,
			bucket:   true,
			wantStep: "artifact-bucket",
			wantLog: []string{
				"up seaweed-master",
				"up seaweed-volume",
				"up seaweed-filer",
				"up seaweed-s3-a seaweed-s3-b caddy",
				"bucket",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := installFakeArtifactStore(t, test.before, test.after)
			rt := &runtime{root: fake, project: "pm-artifact-store-test", stdout: io.Discard, stderr: io.Discard}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.limit > 0 {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, test.limit)
				defer stop()
			}
			ensureBucket := func(context.Context) error {
				if err := appendTestLine(filepath.Join(fake, "order.log"), "bucket"); err != nil {
					return err
				}
				if test.bucket {
					// The parent context ends with this attempt, so the
					// bucket wait ends at once.
					cancel()
					return context.Canceled
				}
				return nil
			}
			step := "before"
			err := startArtifacts(ctx, noRecreateTestConfiguration(t), rt, &step, ensureBucket)
			if err == nil {
				t.Fatal("startArtifacts passed")
			}
			if step != test.wantStep {
				t.Fatalf("failed step = %q, want %q (error: %v)", step, test.wantStep, err)
			}
			if got := recordedMasterCalls(t, fake, "order.log"); !slices.Equal(got, test.wantLog) {
				t.Fatalf("artifact store calls = %q, want %q", got, test.wantLog)
			}
		})
	}
}

// installFakeArtifactStore writes fake python3 and docker programs and puts
// them first on PATH. The fake python3 prints no Compose config hash, so each
// service goes through the normal up, and it records each compose up call in
// order.log. It answers each compose exec call to the master like
// installFakeMaster: /dir/status prints the before document until a grow call
// was made, and the after document then. /vol/grow records its URL in
// order.log. The fake docker lists no service container. It returns the
// directory that holds the documents and the log.
func installFakeArtifactStore(t *testing.T, before, after string) string {
	t.Helper()
	directory := t.TempDir()
	bin := filepath.Join(directory, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(directory, "status-before"), before, 0o600)
	writeTestFile(t, filepath.Join(directory, "status-after"), after, 0o600)
	writeTestFile(t, filepath.Join(bin, "python3"), `#!/bin/sh
for argument in "$@"; do
  if [ "$argument" = "--hash" ]; then
    exit 0
  fi
  last=$argument
done
call=" $* "
case "$call" in
  *" exec -T seaweed-master curl -fsS -m 10 "*)
    case "$last" in
      */dir/status)
        if [ -f "$FAKE_STORE_DIR/grown" ]; then
          cat "$FAKE_STORE_DIR/status-after"
        else
          cat "$FAKE_STORE_DIR/status-before"
        fi
        ;;
      */vol/grow\?*)
        printf 'grow %s\n' "$last" >> "$FAKE_STORE_DIR/order.log"
        : > "$FAKE_STORE_DIR/grown"
        printf '%s\n' '{"count":1}'
        ;;
      *)
        exit 64
        ;;
    esac
    ;;
  *" up -d "*)
    services=${call#* up -d }
    printf 'up %s\n' "${services% }" >> "$FAKE_STORE_DIR/order.log"
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
  ps)
    ;;
  *)
    exit 64
    ;;
esac
`, 0o700)
	t.Setenv("FAKE_STORE_DIR", directory)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
	return directory
}

// appendTestLine appends one line to a log of a fake.
func appendTestLine(path, line string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(line + "\n"); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
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
