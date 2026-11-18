// Package starter starts one process with a ready descriptor and bounds every
// wait on it. It is test-harness code for the acceptance suite, not PM
// behaviour. The acceptance steps pass the real timer source. The Go developer
// tests of this package pass a fake timer source.
//
// The starter owns the start limit (ADR 0005, amendment v4, change A6). It
// reads the ready pipe with one blocking reader and one deadline: the start
// limit, counted from the start of the process. The deadline covers the whole
// read, the ready line and the end of file after it. Readiness is the ready
// line. When the deadline passes before the read ended, the starter closes
// its ready read end, so the read ends. The reader queues each event under
// the lock with which the watcher marks the deadline. So a ready line that
// came before the deadline stays readiness, also when a step reads it after
// the deadline, and an event that comes after the deadline counts as the
// passed deadline. A wait for the ready line or for the
// end of file that the deadline ends runs the stop sequence: SIGTERM, one exit
// observation up to the stop limit, then SIGKILL and the same exit
// observation up to the kill limit. Each signal goes at most once, and each
// wait has one timer. Nothing repeats.
//
// A Process is not safe for concurrent use. One step at a time calls it.
package starter

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Timer is a one-shot timer. It has no Reset, so the starter cannot re-arm a
// timer: a new limit needs a new timer from the source.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// TimerSource creates one-shot timers.
type TimerSource interface {
	NewTimer(d time.Duration) Timer
}

// RealTimers is the timer source of the acceptance steps.
type RealTimers struct{}

// NewTimer returns a timer from time.NewTimer.
func (RealTimers) NewTimer(d time.Duration) Timer {
	return realTimer{timer: time.NewTimer(d)}
}

type realTimer struct{ timer *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.timer.C }
func (t realTimer) Stop() bool          { return t.timer.Stop() }

// Limits holds the three process limits of one start.
type Limits struct {
	// Start is counted from the start of the process. It covers the ready
	// line and the end of file after it.
	Start time.Duration
	// Stop bounds phase 1 of the stop sequence, after SIGTERM.
	Stop time.Duration
	// Kill bounds phase 2 of the stop sequence, after SIGKILL. It also bounds
	// the wait for the exit after end of file with no ready line, and the
	// wait for the output readers after an exit.
	Kill time.Duration
}

// ReadyMode names what the process gets at its ready descriptor.
type ReadyMode int

const (
	// ReadyWriteEnd gives the process the write end of the ready pipe.
	ReadyWriteEnd ReadyMode = iota
	// ReadyClosed gives the process no file at the ready descriptor.
	ReadyClosed
	// ReadyReadOnly gives the process the read end of the ready pipe, a
	// descriptor that is open only for reading.
	ReadyReadOnly
)

// Config describes one process start.
type Config struct {
	Path string
	// Args are all the process arguments. The starter adds none, so the
	// caller chooses the flag that names the ready descriptor.
	Args []string
	Env  []string
	Dir  string
	// Stdin is given to the process as is. Nil gives the null device.
	Stdin *os.File
	// ReadyFD is the number of the ready descriptor in the process, 3 or
	// more. The descriptors from 3 to ReadyFD-1 are closed in the process.
	ReadyFD   int
	ReadyMode ReadyMode
	// ReadyLine is the exact ready line, with its line feed.
	ReadyLine string
	Limits    Limits
	Timers    TimerSource
	// SnapshotBytes bounds each output snapshot: the report keeps the last
	// SnapshotBytes bytes of standard output and of standard error.
	SnapshotBytes int
	// StdoutTee and StderrTee, when set, also receive each output chunk.
	StdoutTee io.Writer
	StderrTee io.Writer

	// reap waits for the process and reaps it. Nil means cmd.Wait. The Go
	// developer tests use it to hold the reaper.
	reap func(*exec.Cmd) error
}

func (c Config) check() error {
	switch {
	case c.Path == "":
		return errors.New("starter: the process path is empty")
	case c.Timers == nil:
		return errors.New("starter: no timer source")
	case c.Limits.Start <= 0 || c.Limits.Stop <= 0 || c.Limits.Kill <= 0:
		return errors.New("starter: the start, stop and kill limits must be more than 0")
	case c.ReadyFD < 3:
		return fmt.Errorf("starter: ready descriptor %d is less than 3", c.ReadyFD)
	case c.ReadyMode < ReadyWriteEnd || c.ReadyMode > ReadyReadOnly:
		return fmt.Errorf("starter: unknown ready mode %d", c.ReadyMode)
	case len(c.ReadyLine) < 2 || !strings.HasSuffix(c.ReadyLine, "\n"):
		return errors.New("starter: the ready line must have text and end with a line feed")
	case c.SnapshotBytes <= 0:
		return errors.New("starter: the snapshot bound must be more than 0")
	}
	return nil
}

// Outcome names how the wait on the ready pipe ended.
type Outcome string

const (
	OutcomeNone             Outcome = "no ready line and no end of file yet"
	OutcomeReady            Outcome = "ready line"
	OutcomeReadyClosed      Outcome = "ready line, then end of file"
	OutcomeEOFBeforeReady   Outcome = "end of file before the ready line"
	OutcomeStartLimitPassed Outcome = "the start limit passed before the ready line"
	OutcomeCloseLimitPassed Outcome = "end of file did not come before the start limit"
	OutcomeProtocolError    Outcome = "data other than one ready line"
)

// StopResult names the result of the stop sequence.
type StopResult string

const (
	StopNone          StopResult = "no signal from the starter"
	StopAfterSIGTERM  StopResult = "exited after SIGTERM"
	StopAfterSIGKILL  StopResult = "exited after SIGKILL"
	StopNoExitAtLimit StopResult = "did not exit within the kill limit"
)

// Report is the state of one started process at the end of one wait.
type Report struct {
	Outcome Outcome
	Stop    StopResult
	PID     int
	// Exited is true when the process exited before the report.
	Exited bool
	// ExitCode is the exit code. It is -1 when the process did not exit or a
	// signal ended it.
	ExitCode int
	// ReadyData holds the bytes read from the ready pipe: at most the length
	// of the ready line and one more byte.
	ReadyData []byte
	// ReadyEOF is true when the starter read end of file on the ready pipe.
	ReadyEOF bool
	// ReadyReadStopped is true when the starter ended its read of the ready
	// pipe before end of file: the start deadline passed, or the starter
	// stopped watching the process.
	ReadyReadStopped bool
	// Stdout and Stderr are bounded snapshots: the last bytes read so far.
	Stdout []byte
	Stderr []byte
	// StdoutTotal and StderrTotal count every byte read from each pipe.
	StdoutTotal int64
	StderrTotal int64
	// OutputEnded is true when both output readers read end of file.
	OutputEnded bool
	// Elapsed is the time from the start to the ready line or to end of file
	// on the ready pipe. It is 0 when neither came.
	Elapsed time.Duration
}

// String gives the report in one line for the step output.
func (r Report) String() string {
	possibleSurvivor := ""
	if r.Stop == StopNoExitAtLimit {
		possibleSurvivor = fmt.Sprintf(" possible survivor: process %d", r.PID)
	}
	return fmt.Sprintf("outcome=%q stop=%q%s pid=%d exited=%t exit=%d elapsed=%s ready=%q ready_eof=%t ready_read_stopped=%t stdout=%q stderr=%q",
		r.Outcome, r.Stop, possibleSurvivor, r.PID, r.Exited, r.ExitCode, r.Elapsed, r.ReadyData, r.ReadyEOF, r.ReadyReadStopped, r.Stdout, r.Stderr)
}

type readyEventKind uint8

const (
	eventLine readyEventKind = iota
	eventEOF
	eventBadData
	// eventStopped: the starter closed the ready read end before end of file.
	eventStopped
)

type readyEvent struct {
	kind readyEventKind
	at   time.Time
}

// Process is one started process.
type Process struct {
	config     Config
	cmd        *exec.Cmd
	started    time.Time
	startTimer Timer
	// ready receives at most two events from the one ready reader.
	ready chan readyEvent
	// exited closes when the wait for the process exit returned. It is the
	// one exit observation of the process.
	exited     chan struct{}
	readers    sync.WaitGroup
	readersEnd chan struct{}
	stdoutRead *os.File
	stderrRead *os.File
	stdout     *snapshot
	stderr     *snapshot

	// readyRead is the read end of the ready pipe.
	readyRead *os.File
	// readerDone closes when the ready reader returns.
	readerDone chan struct{}
	// deadline closes when the start limit passed before the ready reader
	// returned. The watcher closes it, and it is the only receiver of the
	// start timer.
	deadline chan struct{}
	// unwatch closes when the starter stops watching the process.
	unwatch     chan struct{}
	unwatchOnce sync.Once

	readyMu      sync.Mutex
	readyData    []byte
	readyEOF     bool
	readyStopped bool
	// expired is true when the start deadline passed. The watcher sets it
	// under readyMu before it closes deadline.
	expired bool

	outcome   Outcome
	outcomeAt time.Time
	done      bool
	last      Report
}

// Start starts the process and its start timer. It gives the process only
// the descriptors that Config names, and it closes its own copy of each write
// end, so end of file comes when the process closes its copies or exits.
func Start(config Config) (*Process, error) {
	if err := config.check(); err != nil {
		return nil, err
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("starter: create the ready pipe: %w", err)
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		closeAll(readyRead, readyWrite)
		return nil, fmt.Errorf("starter: create the stdout pipe: %w", err)
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		closeAll(readyRead, readyWrite, stdoutRead, stdoutWrite)
		return nil, fmt.Errorf("starter: create the stderr pipe: %w", err)
	}
	cmd := exec.Command(config.Path, config.Args...)
	cmd.Env = config.Env
	cmd.Dir = config.Dir
	if config.Stdin != nil {
		cmd.Stdin = config.Stdin
	}
	// Files, not writers: exec then starts no copy goroutine, so the wait for
	// the process observes only its exit, not end of file on its output.
	cmd.Stdout = stdoutWrite
	cmd.Stderr = stderrWrite
	// Entry i becomes descriptor 3+i. A nil entry is closed in the process.
	cmd.ExtraFiles = make([]*os.File, config.ReadyFD-2)
	switch config.ReadyMode {
	case ReadyWriteEnd:
		cmd.ExtraFiles[config.ReadyFD-3] = readyWrite
	case ReadyReadOnly:
		cmd.ExtraFiles[config.ReadyFD-3] = readyRead
	}
	if err := cmd.Start(); err != nil {
		closeAll(readyRead, readyWrite, stdoutRead, stdoutWrite, stderrRead, stderrWrite)
		return nil, fmt.Errorf("starter: start %s: %w", config.Path, err)
	}
	process := &Process{
		config:     config,
		cmd:        cmd,
		started:    time.Now(),
		startTimer: config.Timers.NewTimer(config.Limits.Start),
		ready:      make(chan readyEvent, 2),
		exited:     make(chan struct{}),
		readersEnd: make(chan struct{}),
		readerDone: make(chan struct{}),
		deadline:   make(chan struct{}),
		unwatch:    make(chan struct{}),
		readyRead:  readyRead,
		stdoutRead: stdoutRead,
		stderrRead: stderrRead,
		stdout:     newSnapshot(config.SnapshotBytes),
		stderr:     newSnapshot(config.SnapshotBytes),
		outcome:    OutcomeNone,
	}
	closeAll(readyWrite, stdoutWrite, stderrWrite)

	reap := config.reap
	if reap == nil {
		reap = (*exec.Cmd).Wait
	}
	go func() {
		_ = reap(cmd)
		close(process.exited)
	}()
	process.readers.Add(2)
	go process.copyOutput(stdoutRead, process.stdout, config.StdoutTee)
	go process.copyOutput(stderrRead, process.stderr, config.StderrTee)
	go func() {
		process.readers.Wait()
		close(process.readersEnd)
	}()
	go process.readReady(readyRead)
	go process.watchReady()
	return process, nil
}

// watchReady owns the start timer. One start deadline bounds the whole read of
// the ready pipe: the ready line and the end of file after it. When the
// deadline passes before the reader returned, or when the starter stops
// watching, the watcher closes the ready read end, so the blocking read ends.
// Readiness stays at the ready line: a step that needs only readiness has
// already returned, and it does not fail when the read ends later.
func (p *Process) watchReady() {
	select {
	case <-p.readerDone:
		p.startTimer.Stop()
	case <-p.startTimer.C():
		p.readyMu.Lock()
		p.expired = true
		p.readyMu.Unlock()
		close(p.deadline)
		_ = p.readyRead.Close()
	case <-p.unwatch:
		p.startTimer.Stop()
		_ = p.readyRead.Close()
	}
}

// stopWatching ends the watch of the ready pipe. The watcher then closes the
// ready read end if the read has not ended.
func (p *Process) stopWatching() {
	p.unwatchOnce.Do(func() { close(p.unwatch) })
}

// PID returns the process ID.
func (p *Process) PID() int { return p.cmd.Process.Pid }

// Done reports whether the starter finished with the process: it exited and
// the starter reported it, or the stop sequence ended.
func (p *Process) Done() bool { return p.done }

// Running reports, with one observation and no wait, whether the process has
// not exited. It does not depend on the reaper goroutine: when the exit
// observation is still open, it asks the kernel once (exitedWithoutReaping).
func (p *Process) Running() bool {
	select {
	case <-p.exited:
		return false
	default:
	}
	return !exitedWithoutReaping(p.cmd.Process.Pid)
}

// idTypePID is the waitid id type P_PID: 1 on Linux and on Darwin.
const idTypePID = 1

// siginfo holds a siginfo_t: 128 bytes on Linux and 104 bytes on Darwin.
// Only si_signo, the first int of the structure on both, is read.
type siginfo struct {
	signo int32
	_     [63]int32
}

// exitedWithoutReaping makes one waitid call with WEXITED, WNOHANG and
// WNOWAIT. The call reports an exited child without reaping it, so the reaper
// still gets the exit status. With WNOHANG and no exited child, si_signo stays
// 0; for an exited child it is SIGCHLD. ECHILD means that the reaper already
// reaped the child. Any other error counts as an exit, so that a check of a
// running process fails rather than passes.
func exitedWithoutReaping(pid int) bool {
	var info siginfo
	_, _, errno := syscall.Syscall6(syscall.SYS_WAITID, idTypePID, uintptr(pid),
		uintptr(unsafe.Pointer(&info)), syscall.WEXITED|syscall.WNOHANG|syscall.WNOWAIT, 0, 0)
	if errno != 0 {
		return true
	}
	return info.signo == int32(syscall.SIGCHLD)
}

// Snapshot returns the current report without a wait.
func (p *Process) Snapshot() Report {
	if p.done {
		return p.last
	}
	return p.report(p.outcome, StopNone, p.outcomeAt)
}

// AwaitReady waits for the ready line or end of file, up to the start limit.
// It returns true at the ready line, and the process keeps running. After end
// of file with no ready line, it waits for the exit (awaitExit). After other
// data on the ready pipe, and at the start limit, it runs the stop sequence.
func (p *Process) AwaitReady() (Report, bool) {
	if p.done {
		return p.last, false
	}
	if p.outcome == OutcomeReady || p.outcome == OutcomeReadyClosed {
		return p.report(p.outcome, StopNone, p.outcomeAt), true
	}
	select {
	case event := <-p.ready:
		return p.readyEvent(event)
	case <-p.deadline:
	}
	// The deadline passed. An event that the reader queued before it is in
	// p.ready already (queue), and it decides.
	select {
	case event := <-p.ready:
		return p.readyEvent(event)
	default:
		p.outcome = OutcomeStartLimitPassed
		return p.stopSequence(), false
	}
}

// readyEvent ends AwaitReady with the first event of the ready reader.
func (p *Process) readyEvent(event readyEvent) (Report, bool) {
	switch event.kind {
	case eventLine:
		p.outcome, p.outcomeAt = OutcomeReady, event.at
		return p.report(OutcomeReady, StopNone, event.at), true
	case eventEOF:
		p.outcome, p.outcomeAt = OutcomeEOFBeforeReady, event.at
		return p.awaitExit(), false
	case eventStopped:
		p.outcome = OutcomeStartLimitPassed
		return p.stopSequence(), false
	default:
		p.outcome, p.outcomeAt = OutcomeProtocolError, event.at
		return p.stopSequence(), false
	}
}

// AwaitClose waits, after the ready line, for end of file on the ready pipe,
// up to the same start deadline. It starts no new limit. It returns true at
// end of file.
func (p *Process) AwaitClose() (Report, bool) {
	if p.done {
		return p.last, false
	}
	switch p.outcome {
	case OutcomeReadyClosed:
		return p.report(p.outcome, StopNone, p.outcomeAt), true
	case OutcomeReady:
	default:
		return p.report(p.outcome, StopNone, p.outcomeAt), false
	}
	select {
	case event := <-p.ready:
		return p.closeEvent(event)
	case <-p.deadline:
	}
	// The deadline passed. An end of file that the reader queued before it is
	// in p.ready already (queue), and it decides.
	select {
	case event := <-p.ready:
		return p.closeEvent(event)
	default:
		p.outcome = OutcomeCloseLimitPassed
		return p.stopSequence(), false
	}
}

// closeEvent ends AwaitClose with the event of the ready reader after the
// ready line.
func (p *Process) closeEvent(event readyEvent) (Report, bool) {
	switch event.kind {
	case eventEOF:
		p.outcome, p.outcomeAt = OutcomeReadyClosed, event.at
		return p.report(OutcomeReadyClosed, StopNone, event.at), true
	case eventStopped:
		p.outcome = OutcomeCloseLimitPassed
		return p.stopSequence(), false
	default:
		p.outcome, p.outcomeAt = OutcomeProtocolError, event.at
		return p.stopSequence(), false
	}
}

// AwaitExit waits once for the process exit, up to the kill limit. If the
// process did not exit, it runs the stop sequence.
func (p *Process) AwaitExit() Report {
	if p.done {
		return p.last
	}
	return p.awaitExit()
}

// Stop runs the stop sequence for a process that a step no longer needs. A
// process that already exited gets no signal.
func (p *Process) Stop() Report {
	if p.done {
		return p.last
	}
	return p.stopSequence()
}

// awaitExit is one wait for the exit, with the kill limit. A process that
// did not exit in that time gets the stop sequence.
func (p *Process) awaitExit() Report {
	limit := p.config.Timers.NewTimer(p.config.Limits.Kill)
	select {
	case <-p.exited:
		limit.Stop()
		return p.afterExit(StopNone)
	case <-limit.C():
		return p.stopSequence()
	}
}

// stopSequence sends SIGTERM, waits on the exit observation up to the stop
// limit, then sends SIGKILL and waits on the same observation up to the kill
// limit. After the kill limit it does not wait for end of file on the output
// pipes: it closes its read ends and reports a possible survivor. The ready
// read can still end with the exit during the sequence; finish closes the
// ready read end after it.
func (p *Process) stopSequence() Report {
	select {
	case <-p.exited:
		return p.afterExit(StopNone)
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	stop := p.config.Timers.NewTimer(p.config.Limits.Stop)
	select {
	case <-p.exited:
		stop.Stop()
		return p.afterExit(StopAfterSIGTERM)
	case <-stop.C():
	}
	_ = p.cmd.Process.Signal(syscall.SIGKILL)
	kill := p.config.Timers.NewTimer(p.config.Limits.Kill)
	select {
	case <-p.exited:
		kill.Stop()
		return p.afterExit(StopAfterSIGKILL)
	case <-kill.C():
	}
	p.closeReadEnds()
	return p.finish(StopNoExitAtLimit)
}

// afterExit waits once for the output readers, up to the kill limit. If they
// did not end, it closes the read ends.
func (p *Process) afterExit(stop StopResult) Report {
	drain := p.config.Timers.NewTimer(p.config.Limits.Kill)
	select {
	case <-p.readersEnd:
		drain.Stop()
	case <-drain.C():
		p.closeReadEnds()
	}
	return p.finish(stop)
}

// finish ends the watch of the process. The ready read end closes if the read
// has not ended, also after the kill limit.
func (p *Process) finish(stop StopResult) Report {
	p.stopWatching()
	p.done = true
	p.last = p.report(p.outcome, stop, p.outcomeAt)
	return p.last
}

func (p *Process) report(outcome Outcome, stop StopResult, at time.Time) Report {
	report := Report{
		Outcome:  outcome,
		Stop:     stop,
		PID:      p.cmd.Process.Pid,
		ExitCode: -1,
	}
	report.ReadyData, report.ReadyEOF, report.ReadyReadStopped = p.readyState()
	var stdoutEnded, stderrEnded bool
	report.Stdout, report.StdoutTotal, stdoutEnded = p.stdout.state()
	report.Stderr, report.StderrTotal, stderrEnded = p.stderr.state()
	report.OutputEnded = stdoutEnded && stderrEnded
	if !at.IsZero() {
		report.Elapsed = at.Sub(p.started)
	}
	select {
	case <-p.exited:
		report.Exited = true
		report.ExitCode = p.cmd.ProcessState.ExitCode()
	default:
	}
	return report
}

func (p *Process) closeReadEnds() {
	closeAll(p.stdoutRead, p.stderrRead)
}

// copyOutput reads one output pipe until end of file, or until the starter
// closes its read end.
func (p *Process) copyOutput(source *os.File, destination *snapshot, tee io.Writer) {
	defer p.readers.Done()
	defer source.Close()
	var writer io.Writer = destination
	if tee != nil {
		writer = io.MultiWriter(destination, tee)
	}
	_, err := io.Copy(writer, source)
	destination.end(err == nil)
}

// readReady is the one blocking reader of the ready pipe. It reads at most
// the ready line and one more byte. It sends one event at the ready line, at
// end of file before the line, at other data, or when the watcher closed the
// read end. After the line it sends one more event: end of file, other data,
// or the closed read end.
func (p *Process) readReady(source *os.File) {
	defer close(p.readerDone)
	defer source.Close()
	line := p.config.ReadyLine
	var one [1]byte
	// Each read blocks until the process writes, closes the descriptor or
	// exits. The loop ends after at most len(line)+1 bytes.
	for index := 0; index <= len(line); index++ {
		if _, err := io.ReadFull(source, one[:]); err != nil {
			eof := errors.Is(err, io.EOF)
			stopped := errors.Is(err, os.ErrClosed)
			p.noteReady(nil, eof, stopped)
			switch {
			case stopped:
				p.queue(eventStopped)
			case eof && (index == 0 || index == len(line)):
				p.queue(eventEOF)
			default:
				p.queue(eventBadData)
			}
			return
		}
		p.noteReady(one[:], false, false)
		if index == len(line) || one[0] != line[index] {
			p.queue(eventBadData)
			return
		}
		if index == len(line)-1 && !p.queue(eventLine) {
			return
		}
	}
}

// queue sends one ready event under readyMu, the lock with which the watcher
// marks the deadline. So each event has one order against the deadline. An
// event queued before the deadline is in p.ready when p.deadline closes. An
// event after the deadline goes as eventStopped. A ready line after the
// deadline also ends the read. The send does not wait: p.ready holds two
// events, and the reader sends at most two. queue reports whether it sent the
// event kind that it got.
func (p *Process) queue(kind readyEventKind) bool {
	p.readyMu.Lock()
	defer p.readyMu.Unlock()
	sent := kind
	if p.expired {
		sent = eventStopped
		if kind == eventLine {
			p.readyStopped = true
		}
	}
	p.ready <- readyEvent{kind: sent, at: time.Now()}
	return sent == kind
}

func (p *Process) noteReady(data []byte, eof, stopped bool) {
	p.readyMu.Lock()
	defer p.readyMu.Unlock()
	p.readyData = append(p.readyData, data...)
	p.readyEOF = p.readyEOF || eof
	p.readyStopped = p.readyStopped || stopped
}

func (p *Process) readyState() ([]byte, bool, bool) {
	p.readyMu.Lock()
	defer p.readyMu.Unlock()
	return append([]byte(nil), p.readyData...), p.readyEOF, p.readyStopped
}

// snapshot keeps the last size bytes written to it and counts every byte.
type snapshot struct {
	mu    sync.Mutex
	size  int
	data  []byte
	total int64
	eof   bool
}

func newSnapshot(size int) *snapshot { return &snapshot{size: size} }

func (s *snapshot) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total += int64(len(p))
	s.data = append(s.data, p...)
	if extra := len(s.data) - s.size; extra > 0 {
		s.data = append(s.data[:0], s.data[extra:]...)
	}
	return len(p), nil
}

func (s *snapshot) end(eof bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eof = eof
}

func (s *snapshot) state() ([]byte, int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.data...), s.total, s.eof
}

func closeAll(files ...*os.File) {
	for _, file := range files {
		_ = file.Close()
	}
}
