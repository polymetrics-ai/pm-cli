package starter

// Go developer tests of the shared starter. They are not acceptance proof.
// Each test passes the fake timer source, so no case depends on the speed of
// the runner. A test waits once, with one time limit, for each signal from a
// fake and for each starter result. Nothing repeats. No sleep, polling or
// retry. Each test names the defect that makes it fail.

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

const (
	fakeStart   = 1 * time.Hour
	fakeStop    = 2 * time.Hour
	fakeKill    = 3 * time.Hour
	testLimit   = 20 * time.Second
	readyLine   = "helper ready\n"
	snapshotMax = 64
)

// fakeTimers records each timer creation with its duration, each Stop, each
// fire and the marks of the test, in one ordered log. A fake timer fires only
// when the test fires it.
type fakeTimers struct {
	mu      sync.Mutex
	log     []string
	created chan *fakeTimer
}

type fakeTimer struct {
	id     int
	d      time.Duration
	c      chan time.Time
	source *fakeTimers
}

func newFakeTimers() *fakeTimers {
	return &fakeTimers{created: make(chan *fakeTimer, 32)}
}

func (f *fakeTimers) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	timer := &fakeTimer{id: len(f.log), d: d, c: make(chan time.Time, 1), source: f}
	f.log = append(f.log, fmt.Sprintf("create %d %s", timer.id, d))
	f.mu.Unlock()
	f.created <- timer
	return timer
}

func (t *fakeTimer) C() <-chan time.Time { return t.c }

func (t *fakeTimer) Stop() bool {
	t.source.mark(fmt.Sprintf("stop %d", t.id))
	return true
}

func (f *fakeTimers) mark(entry string) {
	f.mu.Lock()
	f.log = append(f.log, entry)
	f.mu.Unlock()
}

func (f *fakeTimers) fire(t *fakeTimer) {
	f.mark(fmt.Sprintf("fire %d", t.id))
	t.c <- time.Now()
}

func (f *fakeTimers) entries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

// awaitCreated waits once, with one limit, for the next timer creation.
func awaitCreated(t *testing.T, f *fakeTimers, want time.Duration) *fakeTimer {
	t.Helper()
	select {
	case timer := <-f.created:
		if timer.d != want {
			t.Fatalf("starter created a timer of %s, want %s; log %q", timer.d, want, f.entries())
		}
		return timer
	case <-time.After(testLimit):
		t.Fatalf("starter created no timer of %s within %s; log %q", want, testLimit, f.entries())
	}
	return nil
}

// markerWriter is a fake output tee. It signals once when its output holds
// the marker.
type markerWriter struct {
	mu     sync.Mutex
	data   bytes.Buffer
	marker string
	seen   chan struct{}
	once   sync.Once
}

func newMarkerWriter(marker string) *markerWriter {
	return &markerWriter{marker: marker, seen: make(chan struct{})}
}

func (w *markerWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.data.Write(p)
	if strings.Contains(w.data.String(), w.marker) {
		w.once.Do(func() { close(w.seen) })
	}
	return len(p), nil
}

func awaitSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(testLimit):
		t.Fatalf("no %s within %s", what, testLimit)
	}
}

type result struct {
	report Report
	ok     bool
}

func awaitResult(t *testing.T, results <-chan result, f *fakeTimers) result {
	t.Helper()
	select {
	case r := <-results:
		return r
	case <-time.After(testLimit):
		t.Fatalf("no starter result within %s; timer log %q", testLimit, f.entries())
	}
	return result{}
}

// heldStdin gives the child a standard input that stays open until the test
// ends, so that `cat` in the child blocks.
func heldStdin(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = write.Close() })
	return read, write
}

// startShell starts /bin/sh with the script through the starter. The ready
// pipe is descriptor 3 of the child.
func startShell(t *testing.T, f *fakeTimers, script string, stdin *os.File, extra func(*Config), args ...string) *Process {
	t.Helper()
	config := Config{
		Path:          "/bin/sh",
		Args:          append([]string{"-c", script, "sh"}, args...),
		Stdin:         stdin,
		ReadyFD:       3,
		ReadyLine:     readyLine,
		Limits:        Limits{Start: fakeStart, Stop: fakeStop, Kill: fakeKill},
		Timers:        f,
		SnapshotBytes: snapshotMax,
	}
	if extra != nil {
		extra(&config)
	}
	process, err := Start(config)
	if err != nil {
		t.Fatal(err)
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	return process
}

// (a) The child writes no ready line and exits on SIGTERM.
// Defect: no SIGTERM at the start limit, or a report without the output
// snapshot.
func TestStarterSendsSIGTERMAtTheStartLimit(t *testing.T) {
	f := newFakeTimers()
	stdin, _ := heldStdin(t)
	stdout := newMarkerWriter("out-a\n")
	stderr := newMarkerWriter("err-a\n")
	process := startShell(t, f, `printf 'out-a\n'; printf 'err-a\n' >&2; exec cat`, stdin,
		func(c *Config) { c.StdoutTee = stdout; c.StderrTee = stderr })
	start := awaitCreated(t, f, fakeStart)
	awaitSignal(t, stdout.seen, "standard output from the child")
	awaitSignal(t, stderr.seen, "standard error from the child")

	results := make(chan result, 1)
	go func() { r, ok := process.AwaitReady(); results <- result{r, ok} }()
	f.fire(start)
	got := awaitResult(t, results, f)

	if got.ok || got.report.Outcome != OutcomeStartLimitPassed || got.report.Stop != StopAfterSIGTERM {
		t.Fatalf("report %s, want %q and %q", got.report, OutcomeStartLimitPassed, StopAfterSIGTERM)
	}
	if string(got.report.Stdout) != "out-a\n" || string(got.report.Stderr) != "err-a\n" {
		t.Fatalf("report snapshot %s does not hold the child output", got.report)
	}
}

// (b) The child ignores SIGTERM before the test fires the start timer.
// Defect: no SIGKILL after the stop limit.
func TestStarterSendsSIGKILLAfterTheStopLimit(t *testing.T) {
	f := newFakeTimers()
	stdin, _ := heldStdin(t)
	armed := newMarkerWriter("armed\n")
	// The trap runs before the marker, and exec keeps the ignored SIGTERM.
	process := startShell(t, f, `trap '' TERM; printf 'armed\n'; exec cat`, stdin,
		func(c *Config) { c.StdoutTee = armed })
	start := awaitCreated(t, f, fakeStart)
	awaitSignal(t, armed.seen, "the child's marker that it ignores SIGTERM")

	results := make(chan result, 1)
	go func() { r, ok := process.AwaitReady(); results <- result{r, ok} }()
	f.fire(start)
	stop := awaitCreated(t, f, fakeStop)
	f.fire(stop)
	got := awaitResult(t, results, f)

	if got.ok || got.report.Outcome != OutcomeStartLimitPassed || got.report.Stop != StopAfterSIGKILL {
		t.Fatalf("report %s, want %q and %q", got.report, OutcomeStartLimitPassed, StopAfterSIGKILL)
	}
}

// (c) The child writes more than the snapshot bound, then exits with code 1
// and no ready line. Defect: a snapshot that is larger than the bound or does
// not hold the last bytes.
func TestStarterSnapshotHoldsOnlyTheLastBytes(t *testing.T) {
	f := newFakeTimers()
	dir := t.TempDir()
	stdoutData := "stdout-head-" + strings.Repeat("a", 4096) + "-stdout-tail-0123456789abcdefghijklmnopqrstuvwxyz"
	stderrData := "stderr-head-" + strings.Repeat("b", 4096) + "-stderr-tail-ZYXWVUTSRQPONMLKJIHGFEDCBA9876543210"
	stdoutFile := filepath.Join(dir, "stdout")
	stderrFile := filepath.Join(dir, "stderr")
	if err := os.WriteFile(stdoutFile, []byte(stdoutData), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stderrFile, []byte(stderrData), 0o600); err != nil {
		t.Fatal(err)
	}
	process := startShell(t, f, `cat "$1"; cat "$2" >&2; exit 1`, nil, nil, stdoutFile, stderrFile)
	awaitCreated(t, f, fakeStart)

	results := make(chan result, 1)
	go func() { r, ok := process.AwaitReady(); results <- result{r, ok} }()
	got := awaitResult(t, results, f)

	if got.ok || got.report.Outcome != OutcomeEOFBeforeReady || got.report.Stop != StopNone ||
		!got.report.Exited || got.report.ExitCode != 1 {
		t.Fatalf("report %s, want %q, %q and exit 1", got.report, OutcomeEOFBeforeReady, StopNone)
	}
	if want := stdoutData[len(stdoutData)-snapshotMax:]; string(got.report.Stdout) != want {
		t.Fatalf("stdout snapshot %q (%d bytes), want the last %d bytes %q", got.report.Stdout, len(got.report.Stdout), snapshotMax, want)
	}
	if want := stderrData[len(stderrData)-snapshotMax:]; string(got.report.Stderr) != want {
		t.Fatalf("stderr snapshot %q (%d bytes), want the last %d bytes %q", got.report.Stderr, len(got.report.Stderr), snapshotMax, want)
	}
}

// (d) The child writes the ready line when the test releases it, then keeps
// the ready descriptor open. Defect: a new limit after the ready line, or a
// check of the close that does not end at the original start deadline.
func TestStarterCheckOfTheCloseEndsAtTheOriginalStartDeadline(t *testing.T) {
	f := newFakeTimers()
	stdin, release := heldStdin(t)
	process := startShell(t, f, `read x; printf 'helper ready\n' >&3; exec cat`, stdin, nil)
	start := awaitCreated(t, f, fakeStart)

	readyResults := make(chan result, 1)
	go func() { r, ok := process.AwaitReady(); readyResults <- result{r, ok} }()
	if _, err := release.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	ready := awaitResult(t, readyResults, f)
	if !ready.ok || ready.report.Outcome != OutcomeReady {
		t.Fatalf("report %s, want the ready line", ready.report)
	}
	f.mark("ready line")

	closeResults := make(chan result, 1)
	go func() { r, ok := process.AwaitClose(); closeResults <- result{r, ok} }()
	f.fire(start)
	got := awaitResult(t, closeResults, f)

	if got.ok || got.report.Outcome != OutcomeCloseLimitPassed || got.report.Stop != StopAfterSIGTERM {
		t.Fatalf("report %s, want %q and %q", got.report, OutcomeCloseLimitPassed, StopAfterSIGTERM)
	}
	startTimers, firstStart, readyAt, firedAt := 0, -1, -1, -1
	for i, entry := range f.entries() {
		switch {
		case entry == "ready line":
			readyAt = i
		case entry == fmt.Sprintf("fire %d", start.id):
			firedAt = i
		case strings.HasPrefix(entry, "create ") && strings.HasSuffix(entry, " "+fakeStart.String()):
			if firedAt < 0 {
				startTimers++
				if firstStart < 0 {
					firstStart = i
				}
			}
		}
	}
	if startTimers != 1 || firstStart > readyAt || readyAt > firedAt {
		t.Fatalf("want exactly one start timer, made before the ready line and before the fire; log %q", f.entries())
	}
}

// (e) The child closes the ready descriptor with no line and keeps running.
// Defect: a wait for the exit after end of file with no limit, or no stop
// sequence when that limit passes.
func TestStarterStopsAProcessThatClosesWithNoLineAndKeepsRunning(t *testing.T) {
	f := newFakeTimers()
	stdin, _ := heldStdin(t)
	process := startShell(t, f, `exec 3>&-; exec cat`, stdin, nil)
	awaitCreated(t, f, fakeStart)

	results := make(chan result, 1)
	go func() { r, ok := process.AwaitReady(); results <- result{r, ok} }()
	exitWait := awaitCreated(t, f, fakeKill)
	f.fire(exitWait)
	got := awaitResult(t, results, f)

	if got.ok || got.report.Outcome != OutcomeEOFBeforeReady || got.report.Stop != StopAfterSIGTERM {
		t.Fatalf("report %s, want %q and %q", got.report, OutcomeEOFBeforeReady, StopAfterSIGTERM)
	}
	if len(got.report.ReadyData) != 0 || !got.report.ReadyEOF {
		t.Fatalf("report %s, want end of file with no data on the ready pipe", got.report)
	}
}

// (f) The child writes the ready line and keeps the ready descriptor open. The
// step needs only readiness, so it does not call AwaitClose. The test fires
// the start timer after the line. The starter ends its read of the ready
// pipe at that deadline and closes its read end. Readiness stays: the
// process keeps running, and the starter sends no signal.
// Defect: a ready-pipe read with no deadline after the ready line.
func TestStarterEndsTheReadyReadAtTheStartDeadline(t *testing.T) {
	f := newFakeTimers()
	stdin, _ := heldStdin(t)
	process := startShell(t, f, `printf 'helper ready\n' >&3; exec cat`, stdin, nil)
	start := awaitCreated(t, f, fakeStart)

	results := make(chan result, 1)
	go func() { r, ok := process.AwaitReady(); results <- result{r, ok} }()
	if ready := awaitResult(t, results, f); !ready.ok || ready.report.Outcome != OutcomeReady {
		t.Fatalf("report %s, want the ready line", ready.report)
	}
	f.fire(start)
	awaitSignal(t, process.readerDone, "end of the ready-pipe read after the start deadline")

	got := process.Snapshot()
	if got.Outcome != OutcomeReady || got.Stop != StopNone || got.Exited || got.ReadyEOF {
		t.Fatalf("report %s, want readiness kept, no stop sequence, no exit and no end of file", got)
	}
	if !process.Running() {
		t.Fatalf("Running reports a running child as not running: %s", got)
	}
}

// (g) The child writes the ready line, and a second process that it starts
// keeps the ready write end open. After the ready line, the step stops the
// child. The child exits, but no end of file comes, because the second
// process holds the write end. The starter closes its ready read end when it
// stops watching the process.
// Defect: a ready read end that stays open after the starter stops watching.
func TestStarterClosesTheReadyReadEndWhenItStopsWatching(t *testing.T) {
	f := newFakeTimers()
	stdin, _ := heldStdin(t)
	// The second cat reads the held standard input through descriptor 4 and
	// holds descriptor 3 until the test ends.
	process := startShell(t, f, `exec 4<&0; cat <&4 >/dev/null 2>&1 & printf 'helper ready\n' >&3; exec cat`, stdin, nil)
	awaitCreated(t, f, fakeStart)

	results := make(chan result, 1)
	go func() { r, ok := process.AwaitReady(); results <- result{r, ok} }()
	if ready := awaitResult(t, results, f); !ready.ok || ready.report.Outcome != OutcomeReady {
		t.Fatalf("report %s, want the ready line", ready.report)
	}
	go func() { results <- result{process.Stop(), false} }()
	stopped := awaitResult(t, results, f)
	if stopped.report.Stop != StopAfterSIGTERM || stopped.report.ReadyEOF {
		t.Fatalf("report %s, want %q and no end of file on the ready pipe", stopped.report, StopAfterSIGTERM)
	}
	awaitSignal(t, process.readerDone, "end of the ready-pipe read after the starter stopped watching")
}

// testSiginfo holds a siginfo_t for waitid. The test reads no field of it.
type testSiginfo struct {
	_ [64]int32
}

// waitExitWithoutReaping blocks until the process exits, and does not reap
// it: waitid with P_PID (1 on Linux and Darwin), WEXITED and WNOWAIT.
func waitExitWithoutReaping(pid int) error {
	var info testSiginfo
	_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, 1, uintptr(pid),
		uintptr(unsafe.Pointer(&info)), syscall.WEXITED|syscall.WNOWAIT, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// (h) The child writes the ready line and exits at once. The test holds the
// reaper of the starter, so the exit observation of the starter stays open.
// The fake reaper first waits, without reaping, until the child exited, and
// signals the test. Then Running must report the exit.
// Defect: a liveness check that depends on the reaper goroutine, so a
// process that exited right after its ready line counts as running.
func TestStarterRunningSeesAnExitBeforeTheReaper(t *testing.T) {
	f := newFakeTimers()
	exitSeen := make(chan struct{})
	release := make(chan struct{})
	reapErr := make(chan error, 1)
	process := startShell(t, f, `printf 'helper ready\n' >&3; exit 0`, nil, func(c *Config) {
		c.reap = func(cmd *exec.Cmd) error {
			if err := waitExitWithoutReaping(cmd.Process.Pid); err != nil {
				reapErr <- err
			}
			close(exitSeen)
			<-release
			return cmd.Wait()
		}
	})
	t.Cleanup(func() { close(release) })
	awaitCreated(t, f, fakeStart)

	results := make(chan result, 1)
	go func() { r, ok := process.AwaitReady(); results <- result{r, ok} }()
	if ready := awaitResult(t, results, f); !ready.ok || ready.report.Outcome != OutcomeReady {
		t.Fatalf("report %s, want the ready line", ready.report)
	}
	awaitSignal(t, exitSeen, "the fake reaper's signal that the child exited")
	select {
	case err := <-reapErr:
		t.Fatalf("the fake reaper could not observe the exit: %v", err)
	default:
	}
	if process.Running() {
		t.Fatalf("Running reports a child that exited as running: %s", process.Snapshot())
	}
}

// heldListener binds a loopback listener for a check, with port 0, and
// returns it with a copy of its socket for Config.Listener.
func heldListener(t *testing.T) (*net.TCPListener, *os.File) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	file, err := listener.File()
	if err != nil {
		t.Fatal(err)
	}
	return listener, file
}

// assertStarterCopyClosed checks that Start closed its copy of the listener.
func assertStarterCopyClosed(t *testing.T, file *os.File) {
	t.Helper()
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("the starter copy of the listener is still open (stat: %v); want it closed", err)
	}
}

// bindOnce binds address once and closes the new listener at once.
func bindOnce(address string) error {
	listener, err := net.Listen("tcp4", address)
	if err == nil {
		_ = listener.Close()
	}
	return err
}

// (j) The child gets the listener as descriptor 4 and keeps it. The caller
// closes its own copy after the ready line. The address stays taken while the
// child runs, and it is free after the stop.
// Defect: the starter keeps a copy of the listener, or does not give it to
// the process (ADR 0008, "Ownership of the listener").
func TestStarterGivesTheListenerOnlyToTheProcess(t *testing.T) {
	f := newFakeTimers()
	stdin, _ := heldStdin(t)
	listener, file := heldListener(t)
	address := listener.Addr().String()
	process := startShell(t, f, `printf 'helper ready\n' >&3; exec cat`, stdin,
		func(c *Config) { c.ListenFD = 4; c.Listener = file })
	assertStarterCopyClosed(t, file)
	awaitCreated(t, f, fakeStart)

	results := make(chan result, 1)
	go func() { r, ok := process.AwaitReady(); results <- result{r, ok} }()
	if ready := awaitResult(t, results, f); !ready.ok {
		t.Fatalf("report %s, want the ready line", ready.report)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := bindOnce(address); !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("a second bind while the child runs = %v; want EADDRINUSE, because the child holds the listener", err)
	}
	go func() { results <- result{process.Stop(), false} }()
	if stopped := awaitResult(t, results, f); !stopped.report.Exited {
		t.Fatalf("report %s, want the exit of the child", stopped.report)
	}
	if err := bindOnce(address); err != nil {
		t.Fatalf("a bind after the stop = %v; want a free address, with no copy left", err)
	}
}

// (k) The launch fails, or the configuration is refused. Defect: the starter
// keeps its copy of the listener when Start returns an error.
func TestStarterClosesTheListenerWhenTheStartIsRefused(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"a launch failure":       func(c *Config) { c.Path = filepath.Join(t.TempDir(), "missing") },
		"the ready descriptor":   func(c *Config) { c.ListenFD = c.ReadyFD },
		"a listener number of 2": func(c *Config) { c.ListenFD = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			listener, file := heldListener(t)
			config := Config{
				Path: "/bin/sh", Args: []string{"-c", "exit 0"}, ReadyFD: 3, ReadyLine: readyLine,
				Limits: Limits{Start: fakeStart, Stop: fakeStop, Kill: fakeKill}, Timers: newFakeTimers(),
				SnapshotBytes: snapshotMax, ListenFD: 4, Listener: file,
			}
			change(&config)
			if _, err := Start(config); err == nil {
				t.Fatal("Start() = nil error, want the refusal")
			}
			assertStarterCopyClosed(t, file)
			address := listener.Addr().String()
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			if err := bindOnce(address); err != nil {
				t.Fatalf("a bind after the refused start = %v; want a free address, with no copy left", err)
			}
		})
	}
}

// (l) The child gets the listener and exits with no ready line. Defect: a
// copy of the listener outlives a failed start.
func TestStarterLeavesNoListenerCopyAfterAFailedStart(t *testing.T) {
	f := newFakeTimers()
	listener, file := heldListener(t)
	address := listener.Addr().String()
	process := startShell(t, f, `exit 1`, nil, func(c *Config) { c.ListenFD = 4; c.Listener = file })
	assertStarterCopyClosed(t, file)
	awaitCreated(t, f, fakeStart)

	results := make(chan result, 1)
	go func() { r, ok := process.AwaitReady(); results <- result{r, ok} }()
	got := awaitResult(t, results, f)
	if got.ok || got.report.Outcome != OutcomeEOFBeforeReady || !got.report.Exited {
		t.Fatalf("report %s, want end of file before the ready line and the exit", got.report)
	}
	// The caller closes its copy on a failed start, as the harness does.
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := bindOnce(address); err != nil {
		t.Fatalf("a bind after the failed start = %v; want a free address, with no copy left", err)
	}
}
