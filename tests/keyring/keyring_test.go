package keyring

import (
	"bufio"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Defect: a bus configuration that lists a service directory, the standard
// session service directories or an include can activate a second Secret
// Service (D1-10).
func TestBusConfigurationActivatesNoService(t *testing.T) {
	socket := "/job/keyring/bus/socket"
	config := busConfiguration(socket)
	for _, banned := range []string{"<servicedir", "servicedirs", "<include", "<user>"} {
		if strings.Contains(config, banned) {
			t.Errorf("the bus configuration holds %q:\n%s", banned, config)
		}
	}
	if strings.Count(config, "<listen>") != 1 || !strings.Contains(config, "<listen>unix:path="+socket+"</listen>") {
		t.Errorf("the bus configuration does not listen only on the private socket:\n%s", config)
	}
}

// startStandIn starts a stand-in daemon and waits once, with one limit, for
// its first line. Right after the start, before any assertion, it registers
// the stop of the stand-in for every exit of the test. The stop acts once, so
// a test that stops the stand-in itself stops nothing twice.
func startStandIn(t *testing.T, dir, script, want string) *daemon {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = read.Close() })
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Stdout = write
	d, err := startDaemon(context.Background(), "stand-in daemon", cmd, filepath.Join(dir, "stand-in.log"))
	_ = write.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.stop(Limits{Stop: 5 * time.Second, Kill: 10 * time.Second}, false) })
	line := make(chan string, 1)
	go func() {
		text, _ := bufio.NewReader(read).ReadString('\n')
		line <- text
	}()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case text := <-line:
		if text != want {
			t.Fatalf("the stand-in daemon printed %q, want %q", text, want)
		}
	case <-timer.C:
		t.Fatal("the stand-in daemon printed no line within 10s")
	}
	return d
}

// Defect: a stop that hides a daemon that ignored SIGTERM, or that leaves the
// keyring directory, is a cleanup failure that nobody sees (Part 5).
func TestStopReportsADaemonThatNeededSIGKILL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keyring")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The stand-in ignores SIGTERM after its line. exec keeps the ignored
	// signal.
	d := startStandIn(t, dir, `trap "" TERM; echo ignoring; exec sleep 600`, "ignoring\n")
	k := &Keyring{dir: dir, limits: Limits{Start: time.Second, Stop: 200 * time.Millisecond, Kill: 10 * time.Second}, ready: true, keyring: d}
	err := k.Stop()
	if err == nil || !strings.Contains(err.Error(), "needed SIGKILL") {
		t.Fatalf("Stop() = %v, want the SIGKILL report", err)
	}
	if _, statErr := os.Lstat(dir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the keyring directory is still there after Stop: %v", statErr)
	}
}

// Defect: a stop that hides a daemon that exited during the scenario. The pm
// processes then had no Secret Service.
func TestStopReportsADaemonThatExitedBeforeTheStop(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keyring")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	d := startStandIn(t, dir, `echo exiting; exit 3`, "exiting\n")
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-d.done:
	case <-timer.C:
		t.Fatal("the stand-in daemon did not exit within 10s")
	}
	k := &Keyring{dir: dir, limits: Limits{Start: time.Second, Stop: time.Second, Kill: time.Second}, ready: true, bus: d}
	if err := k.Stop(); err == nil || !strings.Contains(err.Error(), "exited before the stop") {
		t.Fatalf("Stop() = %v, want the early exit report", err)
	}
}

// testLimit is the one time limit of each wait of the checks below.
const testLimit = 20 * time.Second

// makeFIFO makes a named pipe. The cleanup opens it once for writing with no
// wait, so a stand-in that still reads it ends when the test ends.
func makeFIFO(t *testing.T, path string) string {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if writer, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = writer.Close()
		}
	})
	return path
}

// Defect: a parent directory with a character that XML or a D-Bus address
// must escape gives an invalid bus configuration or another socket path (M4).
func TestBusConfigurationEncodesTheSocketPath(t *testing.T) {
	socket := `/job/a&b <c> "d" 'e' %f,g;h=i/bus`
	var parsed struct {
		Listen []string `xml:"listen"`
	}
	if err := xml.Unmarshal([]byte(busConfiguration(socket)), &parsed); err != nil {
		t.Fatalf("the bus configuration is not valid XML: %v", err)
	}
	if len(parsed.Listen) != 1 {
		t.Fatalf("the bus configuration has %d listen addresses, want one", len(parsed.Listen))
	}
	value, ok := strings.CutPrefix(parsed.Listen[0], "unix:path=")
	if !ok || strings.ContainsAny(value, ",;= &<>\"'") {
		t.Fatalf("the listen address %q is not one escaped unix:path value", parsed.Listen[0])
	}
	if path, err := url.PathUnescape(value); err != nil || path != socket {
		t.Fatalf("the listen address %q decodes to %q (%v), want %q", parsed.Listen[0], path, err, socket)
	}
}

// Defect: the stop ends the leader of a daemon process group but leaves a
// descendant that ignores SIGTERM (M2). The descendant holds the write end of
// the alive pipe. End of file on that pipe proves that it exited, because the
// exit of a process closes its descriptors.
func TestStopEndsADescendantThatOutlivesTheLeader(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keyring")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pipes := t.TempDir()
	alive := makeFIFO(t, filepath.Join(pipes, "alive"))
	stall := makeFIFO(t, filepath.Join(pipes, "stall"))
	descendant := filepath.Join(pipes, "descendant.sh")
	if err := os.WriteFile(descendant, []byte(fmt.Sprintf(`trap "" TERM
exec 3> '%s'
echo descendant
read line < '%s'
`, alive, stall)), 0o600); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() {
		reader, err := os.Open(alive)
		if err != nil {
			exited <- err
			return
		}
		defer reader.Close()
		_, err = reader.Read(make([]byte, 1))
		exited <- err
	}()
	// The leader starts the descendant and waits for it. SIGTERM ends the
	// leader. The descendant ignores SIGTERM.
	d := startStandIn(t, dir, fmt.Sprintf(`sh '%s' & wait`, descendant), "descendant\n")
	k := &Keyring{dir: dir, limits: Limits{Start: time.Second, Stop: 10 * time.Second, Kill: 10 * time.Second}, ready: true, keyring: d}
	err := k.Stop()
	if err == nil || !strings.Contains(err.Error(), "still there") {
		t.Fatalf("Stop() = %v, want the report of the process that was left", err)
	}
	select {
	case readErr := <-exited:
		if !errors.Is(readErr, io.EOF) {
			t.Fatalf("read of the alive pipe = %v; want end of file", readErr)
		}
	case <-time.After(testLimit):
		t.Fatalf("the descendant still runs %s after Stop returned", testLimit)
	}
}

// Defect: after the leader of a daemon process group exited and the harness
// reaped it, the stop sends a signal to the old group ID (round 2 F2). After
// the reap, the kernel can give that ID to an unrelated process, so the
// signal can reach an unrelated group. Here the stand-in exits before the
// stop, as before a delayed cleanup. At each group signal of the stop, the
// process whose ID is the group ID must still exist, alive or not yet reaped:
// kill(pgid, 0) must succeed. At least one group signal must go out. The
// check causes no PID reuse.
func TestStopSignalsOnlyAnOwnedGroup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keyring")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	d := startStandIn(t, dir, "echo leaving", "leaving\n")
	select {
	case <-d.done:
	case <-time.After(testLimit):
		t.Fatalf("the stand-in daemon did not exit within %s", testLimit)
	}
	previous := signalGroup
	var mutex sync.Mutex
	var signals int
	var unowned []error
	signalGroup = func(pgid int, sig syscall.Signal) error {
		owned := syscall.Kill(pgid, 0)
		mutex.Lock()
		signals++
		if owned != nil {
			unowned = append(unowned, owned)
		}
		mutex.Unlock()
		return previous(pgid, sig)
	}
	t.Cleanup(func() { signalGroup = previous })
	if err := d.stop(Limits{Stop: 5 * time.Second, Kill: 10 * time.Second}, false); err != nil {
		t.Fatalf("stop after the exit of the leader = %v; want no error", err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if signals == 0 {
		t.Error("the stop sent no group signal")
	}
	for _, err := range unowned {
		t.Errorf("a group signal went to a group ID whose process was already reaped: kill(pgid, 0) = %v", err)
	}
}

// Defect: the stop waits with no limit for the exit of the group owner
// (round 3 finding N1). Here the final group signal is refused (EPERM) and
// the owner is stopped (SIGSTOP), so the end of its stdin pipe does not end
// it. The stop must return within its own limit with the signal error and
// the "not reaped" error. The cleanup of the check ends the owner on every
// exit. Each wait has one time limit. No sleep, no retry.
func TestStopBoundsTheOwnerEnd(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keyring")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stall := makeFIFO(t, filepath.Join(t.TempDir(), "stall"))
	previousLimit := groupOwnerEndLimit
	groupOwnerEndLimit = 200 * time.Millisecond
	previousSignal := signalGroup
	var mutex sync.Mutex
	var owners []int
	signalGroup = func(pgid int, sig syscall.Signal) error {
		if sig != syscall.SIGKILL {
			return previousSignal(pgid, sig)
		}
		mutex.Lock()
		owners = append(owners, pgid)
		mutex.Unlock()
		if err := syscall.Kill(pgid, syscall.SIGSTOP); err != nil {
			return err
		}
		return syscall.EPERM
	}
	t.Cleanup(func() {
		groupOwnerEndLimit = previousLimit
		signalGroup = previousSignal
	})
	d := startStandIn(t, dir, fmt.Sprintf(`echo waiting; read line < '%s'`, stall), "waiting\n")
	// This cleanup runs before the stop cleanup of startStandIn. It ends the
	// stopped owner, so a stop that still waits for it can return.
	t.Cleanup(func() {
		mutex.Lock()
		defer mutex.Unlock()
		for _, pgid := range owners {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			_ = syscall.Kill(-pgid, syscall.SIGCONT)
		}
	})
	result := make(chan error, 1)
	go func() { result <- d.stop(Limits{Stop: 5 * time.Second, Kill: 10 * time.Second}, false) }()
	select {
	case err := <-result:
		if !errors.Is(err, syscall.EPERM) || err == nil || !strings.Contains(err.Error(), "is not reaped") {
			t.Fatalf("stop with a stopped owner and a refused group signal = %v; want the signal error and the owner error", err)
		}
	case <-time.After(testLimit):
		t.Fatalf("the stop did not return within %s", testLimit)
	}
}

// Defect: the wait for the ready byte of the group owner ignores the start
// context of the harness (round 3 finding N2). The owner here keeps its
// stdout open and never sends the byte. An ended start context must refuse
// the start, a cancellation must end the start, and a start deadline must
// end the wait. Each must return far before groupOwnerReadyLimit (10 s).
// Each wait has one time limit. No sleep, no retry.
func TestStartGroupOwnerEndsWithTheStartContext(t *testing.T) {
	previous := groupOwnerScript
	groupOwnerScript = `trap "" TERM; exec cat`
	t.Cleanup(func() { groupOwnerScript = previous })
	const checkLimit = 5 * time.Second
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	deadline, stop := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer stop()
	cancelled, cancelStart := context.WithCancel(context.Background())
	defer cancelStart()
	for name, ctx := range map[string]context.Context{
		"an ended start context":                 ended,
		"a start deadline before the ready byte": deadline,
		"a cancellation while the owner starts":  cancelled,
	} {
		t.Run(name, func(t *testing.T) {
			result := make(chan error, 1)
			done := make(chan struct{})
			// After a failure the start can still run, and it reads the seams
			// of this check. So the cleanup waits once for its end, with one
			// limit, before any seam is restored.
			t.Cleanup(func() {
				select {
				case <-done:
				case <-time.After(2 * groupOwnerReadyLimit):
					t.Errorf("startGroupOwner did not end within %s", 2*groupOwnerReadyLimit)
				}
			})
			go func() {
				defer close(done)
				owner, input, err := startGroupOwner(ctx)
				if err == nil {
					_ = killGroup(owner.Process.Pid)
					_ = endGroupOwner(owner, input)
				}
				result <- err
			}()
			if ctx == cancelled {
				cancelStart()
			}
			select {
			case err := <-result:
				if err == nil || !errors.Is(err, ctx.Err()) {
					t.Fatalf("startGroupOwner = %v; want an error with the end of the start context", err)
				}
			case <-time.After(checkLimit):
				t.Fatalf("startGroupOwner did not return within %s of an ended start context", checkLimit)
			}
		})
	}
}

// Defect: when the second pipe of the group owner cannot be made, the first
// pipe stays open (round 3 finding N3). The check makes the second pipe
// fail and then requires that both ends of the first pipe are closed.
func TestStartGroupOwnerClosesThePipesAfterASecondPipeFailure(t *testing.T) {
	previous := newPipe
	var first []*os.File
	calls := 0
	newPipe = func() (*os.File, *os.File, error) {
		calls++
		if calls > 1 {
			return nil, nil, errors.New("no more pipes")
		}
		read, write, err := os.Pipe()
		if err == nil {
			first = append(first, read, write)
		}
		return read, write, err
	}
	t.Cleanup(func() {
		newPipe = previous
		for _, file := range first {
			_ = file.Close()
		}
	})
	if _, _, err := startGroupOwner(context.Background()); err == nil {
		t.Fatal("startGroupOwner with a failed second pipe = nil error; want the pipe error")
	}
	if len(first) != 2 {
		t.Fatalf("the first pipe was not made: %d ends", len(first))
	}
	if _, err := first[1].Write([]byte("x")); !errors.Is(err, os.ErrClosed) {
		t.Errorf("a write to the first pipe after the failure = %v; want os.ErrClosed", err)
	}
	if _, err := first[0].Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Errorf("a read from the first pipe after the failure = %v; want os.ErrClosed", err)
	}
}

// Defect: a test that ends before it stops its stand-in daemon leaves the
// stand-in running (M3).
func TestStandInStopsWhenItsTestEndsEarly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keyring")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stall := makeFIFO(t, filepath.Join(t.TempDir(), "stall"))
	var d *daemon
	t.Run("an end before the stop", func(t *testing.T) {
		d = startStandIn(t, dir, fmt.Sprintf(`echo waiting; read line < '%s'`, stall), "waiting\n")
	})
	if d == nil {
		t.Fatal("the run ended before the stand-in started")
	}
	select {
	case <-d.done:
	case <-time.After(testLimit):
		t.Fatalf("the stand-in daemon still runs %s after its test ended", testLimit)
	}
}

// Defect: a second Stop, for example a fallback cleanup after an explicit
// Stop, stops the same daemon again and reports a false failure (M3).
func TestStopActsOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keyring")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stall := makeFIFO(t, filepath.Join(t.TempDir(), "stall"))
	d := startStandIn(t, dir, fmt.Sprintf(`echo waiting; read line < '%s'`, stall), "waiting\n")
	k := &Keyring{dir: dir, limits: Limits{Start: time.Second, Stop: 10 * time.Second, Kill: 10 * time.Second}, ready: true, keyring: d}
	first := k.Stop()
	second := k.Stop()
	if first != nil || second != nil {
		t.Fatalf("Stop() = %v, then %v; want no error from either call", first, second)
	}
}

// Defect: logTail reads the whole daemon log into memory before it keeps the
// last logLimit bytes (M5). The log is a sparse file of 64 MiB.
func TestLogTailReadsOnlyTheTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const size = 64 << 20
	tail := strings.Repeat("t", logLimit-4) + "end\n"
	if err := file.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte(tail), size-int64(len(tail))); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	d := &daemon{name: "test daemon", log: path}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got := d.logTail()
	runtime.ReadMemStats(&after)
	if want := fmt.Sprintf("test daemon log: %q", tail); got != want {
		t.Fatalf("logTail() = %d bytes, want the last %d bytes of the log", len(got), logLimit)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("logTail allocated %d bytes for a %d-byte log, want at most 1 MiB", allocated, size)
	}
}
