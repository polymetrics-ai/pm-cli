package warehouse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// The supervisor passes exactly this environment to the child. Nothing is
// inherited: no provider, object-store, or PostgreSQL credential, no PM server
// or stack variable, and no engine discovery variable can reach the child.
const (
	childDescriptorEnvironment = "PM_QUERY_CLOSE_DESCRIPTORS"
	childUserEnvironment       = "HOME"
	childTemporaryEnvironment  = "TMPDIR"
	childPathEnvironment       = "PATH"
	childLocaleEnvironment     = "LC_ALL"
	childLanguageEnvironment   = "LANG"
	childZoneEnvironment       = "TZ"
	childPathValue             = "/usr/bin:/bin"
	childDiagnosticPrefix      = "pm-query ready pid="
	// descriptorScanLimit matches the child's own sweep range.
	descriptorScanLimit = 256
)

// ChildLimits are the supervisor's enforced caps for one child attempt. Every
// bound is required and positive: an absent bound is refused rather than
// defaulted, and no request may raise a cap.
type ChildLimits struct {
	MaxRequestBytes     int64
	MaxResponseBytes    int64
	MaxDiagnosticsBytes int64
	MemoryBytes         int64
	TempBytes           int64
	WallTime            time.Duration
	StartupTimeout      time.Duration
}

// Validate refuses a missing or non-positive supervisor bound.
func (limits ChildLimits) Validate() error {
	switch {
	case limits.MaxRequestBytes <= 0:
		return refusal(RefusalBoundExceeded, "the child request byte cap is absent or non-positive")
	case limits.MaxResponseBytes <= 0:
		return refusal(RefusalBoundExceeded, "the child response byte cap is absent or non-positive")
	case limits.MaxDiagnosticsBytes <= 0:
		return refusal(RefusalBoundExceeded, "the child diagnostic byte cap is absent or non-positive")
	case limits.MemoryBytes <= 0:
		return refusal(RefusalBoundExceeded, "the child memory cap is absent or non-positive")
	case limits.TempBytes <= 0:
		return refusal(RefusalBoundExceeded, "the child temporary byte cap is absent or non-positive")
	case limits.WallTime <= 0:
		return refusal(RefusalBoundExceeded, "the child wall-time cap is absent or non-positive")
	case limits.StartupTimeout <= 0:
		return refusal(RefusalBoundExceeded, "the child startup cap is absent or non-positive")
	case limits.MemoryBytes > MaximumQueryMemoryBytes:
		return refusal(RefusalBoundExceeded, "the child memory cap exceeds the protocol capability")
	case limits.TempBytes > MaximumQueryTempBytes:
		return refusal(RefusalBoundExceeded, "the child temporary cap exceeds the protocol capability")
	}
	return nil
}

// ChildConfig is the supervisor's own configuration. AdmittedRoots is the
// authorization anchor: a request may only name directories below it, so a
// caller cannot widen its own scope by declaration.
type ChildConfig struct {
	BinaryPath    string
	ScratchRoot   string
	AdmittedRoots []string
	Limits        ChildLimits
	// Diagnostics receives the child's bounded, sanitized standard error as it
	// is produced, so an operator (and a supervised test) can observe the live
	// child without the child ever writing a credential.
	Diagnostics io.Writer
}

// QueryChild supervises restricted child attempts. One instance may run several
// attempts; each attempt gets its own process, its own scratch directory, and
// its own pipes.
type QueryChild struct {
	config ChildConfig
}

// NewQueryChild validates the supervisor configuration before any attempt.
func NewQueryChild(config ChildConfig) (*QueryChild, error) {
	if err := config.Limits.Validate(); err != nil {
		return nil, err
	}
	path, err := filepath.Abs(config.BinaryPath)
	if err != nil {
		return nil, refusal(RefusalChildStartup, "resolve the child binary path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, refusal(RefusalChildStartup, "the child binary is unavailable: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o100 == 0 {
		return nil, refusal(RefusalChildStartup, "the child binary must be an executable regular file, not a symlink")
	}
	if len(config.AdmittedRoots) == 0 {
		return nil, refusal(RefusalUnadmittedPath, "the supervisor requires at least one admitted root")
	}
	roots := make([]string, 0, len(config.AdmittedRoots))
	for _, root := range config.AdmittedRoots {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root {
			return nil, refusal(RefusalUnadmittedPath, "an admitted root must be one absolute, cleaned path")
		}
		info, err := os.Lstat(root)
		if err != nil || !info.IsDir() {
			return nil, refusal(RefusalUnadmittedPath, "an admitted root must be an existing directory")
		}
		roots = append(roots, root)
	}
	scratchRoot, err := filepath.Abs(config.ScratchRoot)
	if err != nil {
		return nil, refusal(RefusalEnvironment, "resolve the scratch root")
	}
	probe, err := NewBoundScratch(scratchRoot)
	if err != nil {
		return nil, refusal(RefusalEnvironment, "the scratch root is unusable: %v", err)
	}
	if err := probe.Close(); err != nil {
		return nil, refusal(RefusalEnvironment, "the scratch root is not disposable: %v", err)
	}
	return &QueryChild{config: ChildConfig{
		BinaryPath:    path,
		ScratchRoot:   scratchRoot,
		AdmittedRoots: roots,
		Limits:        config.Limits,
		Diagnostics:   config.Diagnostics,
	}}, nil
}

// Execute runs exactly one supervised attempt. A refusal is returned as a typed
// *Refusal error and never as rows: an unavailable answer stays unavailable.
// The supervisor never writes an admitted file, a source manifest, or an effect
// record, so killing the child cannot advance durable state.
func (child *QueryChild) Execute(ctx context.Context, request QueryRequest) (QueryResponse, error) {
	var response QueryResponse
	if ctx == nil {
		return response, refusal(RefusalProtocol, "an execution requires a non-nil context")
	}
	if err := child.admit(request); err != nil {
		return response, err
	}
	document, err := EncodeQueryRequest(request)
	if err != nil {
		return response, err
	}
	if int64(len(document)) > child.config.Limits.MaxRequestBytes {
		return response, refusal(RefusalBoundExceeded, "the encoded request exceeds the supervisor request cap")
	}
	scratch, err := NewBoundScratch(child.config.ScratchRoot)
	if err != nil {
		return response, refusal(RefusalEnvironment, "create the per-attempt scratch directory: %v", err)
	}
	defer func() { _ = scratch.Close() }()
	attempt, err := startChildAttempt(ctx, child.config, scratch, document)
	if err != nil {
		return response, err
	}
	defer attempt.terminate()
	return attempt.await(ctx, request, child.config.Limits)
}

// admit refuses a request the supervisor itself can reject: an unadmitted path,
// an unregistered relation, an unknown identifier, a function-shaped name, or a
// bound above the configured capability. None of these reach a child process.
func (child *QueryChild) admit(request QueryRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	for _, directory := range request.AdmittedDirectories {
		if !withinAdmittedRoots(directory, child.config.AdmittedRoots) {
			return refusal(RefusalUnadmittedPath, "the request declares an admitted directory outside the supervised roots")
		}
		if err := inspectAdmittedDirectory(directory); err != nil {
			return err
		}
	}
	for _, relation := range request.Relations {
		if !withinAdmittedRoots(relation.Path, child.config.AdmittedRoots) ||
			!withinAdmittedRoots(relation.Path, request.AdmittedDirectories) {
			return refusal(RefusalUnadmittedPath, "relation %q is outside the admitted directories", relation.Name)
		}
		if err := inspectAdmittedFile(relation.Path, "relation "+strconv.Quote(relation.Name)); err != nil {
			return err
		}
	}
	if request.Select != nil && request.Select.Output != nil {
		if !withinAdmittedRoots(request.Select.Output.Path, child.config.AdmittedRoots) ||
			!withinAdmittedRoots(request.Select.Output.Path, request.AdmittedDirectories) {
			return refusal(RefusalUnadmittedPath, "the output path is outside the admitted directories")
		}
		if err := inspectAdmittedOutput(request.Select.Output.Path); err != nil {
			return err
		}
	}
	if request.Encode != nil {
		if !withinAdmittedRoots(request.Encode.Output.Path, child.config.AdmittedRoots) ||
			!withinAdmittedRoots(request.Encode.Output.Path, request.AdmittedDirectories) {
			return refusal(RefusalUnadmittedPath, "the output path is outside the admitted directories")
		}
		if err := inspectAdmittedOutput(request.Encode.Output.Path); err != nil {
			return err
		}
	}
	if request.Bounds.MemoryBytes > child.config.Limits.MemoryBytes {
		return refusal(RefusalBoundExceeded, "the declared memory bound exceeds the supervisor capability")
	}
	if request.Bounds.TempBytes > child.config.Limits.TempBytes {
		return refusal(RefusalBoundExceeded, "the declared temporary bound exceeds the supervisor capability")
	}
	if request.Bounds.WallTime > child.config.Limits.WallTime {
		return refusal(RefusalBoundExceeded, "the declared wall time exceeds the supervisor capability")
	}
	if request.Bounds.Bytes+responseEnvelopeSlack > child.config.Limits.MaxResponseBytes {
		return refusal(RefusalBoundExceeded, "the declared byte bound does not fit the supervisor response cap")
	}
	return nil
}

// withinAdmittedRoots reports whether path is inside one of the authorized
// roots.
func withinAdmittedRoots(path string, roots []string) bool {
	for _, root := range roots {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		// "." is the root itself, which is inside it; any parent escape is not.
		if strings.HasPrefix(relative, "..") {
			continue
		}
		return true
	}
	return false
}

func inspectAdmittedDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return refusal(RefusalUnadmittedPath, "an admitted directory is not readable: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return refusal(RefusalUnadmittedPath, "an admitted directory must not be a symlink")
	}
	if !info.IsDir() {
		return refusal(RefusalUnadmittedPath, "an admitted directory must be a directory")
	}
	return nil
}

// inspectAdmittedFile refuses a path the child must never open: a symlink, a
// FIFO, a device, a directory, or a missing file.
func inspectAdmittedFile(path, description string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return refusal(RefusalUnadmittedPath, "%s is not readable: %v", description, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return refusal(RefusalUnadmittedPath, "%s is a symlink", description)
	}
	if !info.Mode().IsRegular() {
		return refusal(RefusalUnadmittedPath, "%s is not a regular file", description)
	}
	return nil
}

func inspectAdmittedOutput(path string) error {
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return refusal(RefusalUnadmittedPath, "the output path is a symlink")
		}
		return refusal(RefusalUnadmittedPath, "the output path already exists and an immutable segment is never overwritten")
	case !errors.Is(err, os.ErrNotExist):
		return refusal(RefusalUnadmittedPath, "the output path cannot be inspected: %v", err)
	}
	return inspectAdmittedDirectory(filepath.Dir(path))
}

// childAttempt is one live supervised process with its own pipes and scratch.
type childAttempt struct {
	command     *exec.Cmd
	stdin       *os.File
	stdout      io.ReadCloser
	diagnostics *boundedDiagnostics
	scratch     *BoundScratch
	exit        chan error
	closeOnce   sync.Once
}

type attemptOutput struct {
	document []byte
	err      error
}

// startChildAttempt spawns the child with a cleared environment, exactly three
// inherited standard descriptors, its own process group, and its own scratch.
func startChildAttempt(ctx context.Context, config ChildConfig, scratch *BoundScratch, document []byte) (*childAttempt, error) {
	stdinRead, stdinWrite, err := os.Pipe()
	if err != nil {
		return nil, refusal(RefusalChildStartup, "create the child request pipe: %v", err)
	}
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		closeAll(stdinRead, stdinWrite)
		return nil, refusal(RefusalChildStartup, "create the child response pipe: %v", err)
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		closeAll(stdinRead, stdinWrite, stdoutRead, stdoutWrite)
		return nil, refusal(RefusalChildStartup, "create the child diagnostic pipe: %v", err)
	}
	descriptors, err := inheritedDescriptors()
	if err != nil {
		closeAll(stdinRead, stdinWrite, stdoutRead, stdoutWrite, stderrRead, stderrWrite)
		return nil, refusal(RefusalChildStartup, "inspect the supervisor descriptors: %v", err)
	}
	command := exec.Command(config.BinaryPath)
	command.Env = childEnvironment(scratch.Path(), descriptors)
	command.Stdin = stdinRead
	command.Stdout = stdoutWrite
	command.Stderr = stderrWrite
	command.ExtraFiles = nil
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	attempt := &childAttempt{
		command:     command,
		stdin:       stdinWrite,
		stdout:      stdoutRead,
		diagnostics: newBoundedDiagnostics(config.Diagnostics, config.Limits.MaxDiagnosticsBytes),
		scratch:     scratch,
		exit:        make(chan error, 1),
	}
	if err := command.Start(); err != nil {
		closeAll(stdinRead, stdinWrite, stdoutRead, stdoutWrite, stderrRead, stderrWrite)
		return nil, refusal(RefusalChildStartup, "start the restricted child: %v", err)
	}
	// The child owns its ends. The supervisor closes its copies immediately so
	// no leaked descriptor can keep a pipe open past the attempt.
	closeAll(stdinRead, stdoutWrite, stderrWrite)
	go func() { attempt.exit <- command.Wait() }()
	go func() { attempt.diagnostics.consume(stderrRead) }()
	go func() {
		_, _ = stdinWrite.Write(document)
		_ = stdinWrite.Close()
	}()
	_ = ctx
	return attempt, nil
}

func closeAll(closers ...io.Closer) {
	for _, closer := range closers {
		_ = closer.Close()
	}
}

// childEnvironment builds the child's complete environment: no inherited
// variable, no credential, and an explicit declaration of every inherited
// descriptor the child must close.
func childEnvironment(scratch string, descriptors []int) []string {
	values := []string{
		childUserEnvironment + "=" + scratch,
		childTemporaryEnvironment + "=" + scratch,
		childPathEnvironment + "=" + childPathValue,
		childLocaleEnvironment + "=C",
		childLanguageEnvironment + "=C",
		childZoneEnvironment + "=UTC",
	}
	declared := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		declared = append(declared, strconv.Itoa(descriptor))
	}
	sort.Strings(declared)
	values = append(values, childDescriptorEnvironment+"="+strings.Join(declared, ","))
	return values
}

// inheritedDescriptors reports every descriptor above the standard three that
// this process holds without close-on-exec. Those are exactly the descriptors a
// child process would inherit, so the declaration is complete: Go's runtime and
// standard library always open descriptors with close-on-exec set.
func inheritedDescriptors() ([]int, error) {
	descriptors := []int{}
	for descriptor := 3; descriptor < descriptorScanLimit; descriptor++ {
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(descriptor), uintptr(syscall.F_GETFD), 0)
		if errno != 0 {
			continue
		}
		if flags&uintptr(syscall.FD_CLOEXEC) != 0 {
			continue
		}
		descriptors = append(descriptors, descriptor)
	}
	return descriptors, nil
}

// boundedDiagnostics forwards at most the configured number of child diagnostic
// bytes, records whether the child announced its restricted startup, and never
// blocks the supervisor.
type boundedDiagnostics struct {
	sink      io.Writer
	limit     int64
	written   int64
	announced bool
	retained  string
	mutex     sync.Mutex
}

func newBoundedDiagnostics(sink io.Writer, limit int64) *boundedDiagnostics {
	return &boundedDiagnostics{sink: sink, limit: limit}
}

func (diagnostics *boundedDiagnostics) consume(reader io.ReadCloser) {
	defer func() { _ = reader.Close() }()
	buffer := make([]byte, 4096)
	for {
		read, err := reader.Read(buffer)
		if read > 0 {
			diagnostics.forward(buffer[:read])
		}
		if err != nil {
			return
		}
	}
}

func (diagnostics *boundedDiagnostics) forward(data []byte) {
	sanitized := sanitizeDiagnostic(data)
	diagnostics.mutex.Lock()
	if !diagnostics.announced && strings.Contains(sanitized, childDiagnosticPrefix) {
		diagnostics.announced = true
	}
	remaining := diagnostics.limit - diagnostics.written
	sink := diagnostics.sink
	if remaining > 0 {
		if int64(len(sanitized)) > remaining {
			sanitized = sanitized[:remaining]
		}
		diagnostics.written += int64(len(sanitized))
		diagnostics.retained = appendTail(diagnostics.retained, sanitized)
	} else {
		sanitized = ""
	}
	diagnostics.mutex.Unlock()
	if sink == nil || sanitized == "" {
		return
	}
	_, _ = sink.Write([]byte(sanitized))
}

// tail reports the bounded diagnostic tail for inclusion in a refusal reason.
func (diagnostics *boundedDiagnostics) tail() string {
	diagnostics.mutex.Lock()
	defer diagnostics.mutex.Unlock()
	return strings.TrimSpace(diagnostics.retained)
}

func (diagnostics *boundedDiagnostics) announcedStartup() bool {
	diagnostics.mutex.Lock()
	defer diagnostics.mutex.Unlock()
	return diagnostics.announced
}

// appendTail keeps the last bounded diagnostic text without unbounded growth.
func appendTail(existing string, addition string) string {
	combined := sanitizeReason(existing + addition)
	if len(combined) > 512 {
		combined = combined[len(combined)-512:]
		for !utf8.ValidString(combined) && len(combined) > 0 {
			combined = combined[1:]
		}
	}
	return combined
}

// sanitizeDiagnostic removes control characters so a child diagnostic can never
// inject a terminal escape into an operator's log.
func sanitizeDiagnostic(data []byte) string {
	return strings.Map(func(character rune) rune {
		if character < 0x20 || character == 0x7f {
			if character == '\n' {
				return '\n'
			}
			return ' '
		}
		return character
	}, string(data))
}

// await waits for the child while enforcing the startup, wall-time, and response
// caps. It never blocks on one observation alone, so a child that closes its
// response stream and keeps running is still killed on time.
func (attempt *childAttempt) await(ctx context.Context, request QueryRequest, limits ChildLimits) (QueryResponse, error) {
	var response QueryResponse
	// The supervisor enforces its own configured cap. The request's declared
	// wall time is the child's own deadline, and admission already refused a
	// declared value above this cap.
	wallTime := limits.WallTime
	responseLimit := attemptResponseLimit(limits)
	readResult := make(chan attemptOutput, 1)
	go func() {
		document, err := io.ReadAll(io.LimitReader(attempt.stdout, responseLimit+1))
		readResult <- attemptOutput{document: document, err: err}
	}()

	started := time.Now()
	wall := time.NewTimer(wallTime)
	defer wall.Stop()
	startup := time.NewTimer(attemptStartupWindow(wallTime, limits.StartupTimeout))
	defer startup.Stop()
	startupChannel := startup.C
	var output *attemptOutput
	var exitErr error
	exited := false
	// abort terminates the process group, reaps the child so no zombie keeps
	// the supervisor from observing the kill, and reports one typed refusal.
	abort := func(code RefusalCode, format string, arguments ...any) (QueryResponse, error) {
		attempt.terminate()
		if !exited {
			select {
			case exitErr = <-attempt.exit:
				exited = true
			case <-time.After(abortReapTimeout):
			}
		}
		return response, attempt.reportExit(code, format, arguments...)
	}
	for {
		if output != nil && exited {
			return attempt.decode(request, output.document, output.err, exitErr, responseLimit)
		}
		var readChannel <-chan attemptOutput
		if output == nil {
			readChannel = readResult
		}
		var exitChannel <-chan error
		if !exited {
			exitChannel = attempt.exit
		}
		select {
		case value := <-readChannel:
			received := value
			output = &received
		case err := <-exitChannel:
			exitErr = err
			exited = true
		case <-wall.C:
			return abort(RefusalChildTimeout, "the child exceeded the supervisor wall time")
		case <-startupChannel:
			startupChannel = nil
			if !attempt.diagnostics.announcedStartup() {
				// When the wall-time cap has elapsed, the wall-time refusal is
				// the truthful one; otherwise the child never reached its
				// restricted startup.
				if time.Since(started) >= wallTime {
					return abort(RefusalChildTimeout, "the child exceeded the supervisor wall time")
				}
				return abort(RefusalChildStartup, "the child never announced its restricted startup")
			}
		case <-ctx.Done():
			return abort(RefusalChildKilled, "the caller cancelled the attempt")
		}
	}
}

// abortReapTimeout bounds how long a termination waits for the child's exit so
// a killed process is reaped rather than left as a zombie.
const abortReapTimeout = 5 * time.Second

// responseEnvelopeSlack covers the bounded document envelope around a declared
// payload byte bound.
const responseEnvelopeSlack = 1 << 16

// attemptResponseLimit is the supervisor's own response cap. It never depends
// on a value the child chose.
func attemptResponseLimit(limits ChildLimits) int64 {
	return limits.MaxResponseBytes + responseEnvelopeSlack
}

// attemptStartupWindow bounds how long a child may take to announce its
// restricted startup. It never exceeds either the configured startup cap or the
// wall-time cap, so the two observations stay ordered.
func attemptStartupWindow(wallTime, startupTimeout time.Duration) time.Duration {
	window := startupTimeout
	if wallTime < window {
		window = wallTime
	}
	return window
}

// decode validates the child's document against the request before any row is
// accepted.
func (attempt *childAttempt) decode(request QueryRequest, document []byte, readErr, exitErr error, responseLimit int64) (QueryResponse, error) {
	var response QueryResponse
	if readErr != nil {
		return response, attempt.reportExit(RefusalChildExit, "read the child response: %v", readErr)
	}
	if int64(len(document)) > responseLimit {
		attempt.terminate()
		return response, attempt.reportExit(RefusalChildOutputExceed, "the child response exceeds the supervisor response cap")
	}
	if len(document) == 0 {
		return response, attempt.exitRefusal(exitErr, "the child produced no response document")
	}
	decoded, err := DecodeQueryResponse(document)
	if err != nil {
		var childRefusal *Refusal
		if errors.As(err, &childRefusal) {
			childRefusal.ExitCode = exitCodeOf(exitErr)
			return response, childRefusal
		}
		return response, attempt.reportExit(RefusalChildExit, "the child response is not a valid document: %v", err)
	}
	if exitErr != nil {
		return response, attempt.reportExit(RefusalChildExit, "the child exited non-zero after reporting a response")
	}
	if decoded.RequestID != request.RequestID {
		return response, attempt.reportExit(RefusalChildExit, "the child response identifies another request")
	}
	if decoded.Child == nil || decoded.Child.PID != attempt.processID() {
		return response, attempt.reportExit(RefusalChildStartup, "the child response did not come from the supervised process")
	}
	if decoded.Child.TemporaryDirectory == "" ||
		!withinAdmittedRoots(decoded.Child.TemporaryDirectory, []string{attempt.scratch.Path()}) {
		return response, attempt.reportExit(RefusalEnvironment, "the child reported a temporary directory outside its own scratch")
	}
	if err := verifyResponseShape(request, decoded); err != nil {
		return response, attempt.reportExit(FailureCodeOf(err), "%v", err)
	}
	return decoded, nil
}

// verifyResponseShape refuses a response that does not match the request's
// declared projection, column order, output path, or declared bounds.
func verifyResponseShape(request QueryRequest, response QueryResponse) error {
	switch {
	case request.Select != nil:
		relation, err := selectedRelation(request, request.Select.From)
		if err != nil {
			return err
		}
		if len(response.Columns) != len(request.Select.Project) {
			return refusal(RefusalProtocol, "the child returned %d columns for a %d-column projection",
				len(response.Columns), len(request.Select.Project))
		}
		for index, projection := range request.Select.Project {
			declared, present := relationColumn(relation, projection.Column)
			if !present {
				return refusal(RefusalUnknownColumn, "the projection names an undeclared column %q", projection.Column)
			}
			expected := declared
			if projection.Alias != "" {
				expected.Name = projection.Alias
			}
			actual := response.Columns[index]
			if actual.Name != expected.Name || actual.Kind != expected.Kind {
				return refusal(RefusalSchemaMismatch, "the child returned column %q as %s instead of %q as %s",
					actual.Name, actual.Kind, expected.Name, expected.Kind)
			}
		}
		if int64(len(response.Rows)) > request.Bounds.Rows {
			return refusal(RefusalBoundExceeded, "the child returned %d rows beyond the declared row bound", len(response.Rows))
		}
		if request.Select.Output == nil && response.Output != nil {
			return refusal(RefusalProtocol, "the child materialized a segment the request did not declare")
		}
		if request.Select.Output != nil {
			if response.Output == nil {
				return refusal(RefusalProtocol, "the child did not describe the segment the request declared")
			}
			if response.Output.Path != request.Select.Output.Path {
				return refusal(RefusalUnadmittedPath, "the child materialized a segment outside the declared output path")
			}
		}
		return nil
	case request.Encode != nil:
		if len(response.Columns) != len(request.Encode.Columns) {
			return refusal(RefusalProtocol, "the child reported %d columns for a %d-column encoded batch",
				len(response.Columns), len(request.Encode.Columns))
		}
		if response.Output == nil || response.Output.Path != request.Encode.Output.Path {
			return refusal(RefusalProtocol, "the child did not describe the declared encoded segment")
		}
		if response.Output.Bytes > request.Bounds.Bytes {
			return refusal(RefusalBoundExceeded, "the materialized segment exceeds the declared byte bound")
		}
		return nil
	default:
		return refusal(RefusalProtocol, "the request declares no operation")
	}
}

// FailureCodeOf reports the refusal code carried by an error.
func FailureCodeOf(err error) RefusalCode {
	var failure *Refusal
	if errors.As(err, &failure) {
		return failure.Code
	}
	return RefusalProtocol
}

func selectedRelation(request QueryRequest, name string) (QueryRelation, error) {
	for _, relation := range request.Relations {
		if relation.Name == name {
			return relation, nil
		}
	}
	return QueryRelation{}, refusal(RefusalUnknownRelation, "the request does not declare relation %q", name)
}

func relationColumn(relation QueryRelation, name string) (QueryColumn, bool) {
	for _, column := range relation.Columns {
		if column.Name == name {
			return column, true
		}
	}
	return QueryColumn{}, false
}

// exitRefusal maps one process outcome to a typed refusal.
func (attempt *childAttempt) exitRefusal(exitErr error, format string, arguments ...any) error {
	reason := fmt.Sprintf(format, arguments...)
	if signal := signalOf(exitErr); signal != "" {
		return attempt.reportExit(RefusalChildKilled, "%s (terminated by %s)", reason, signal)
	}
	return attempt.reportExit(RefusalChildExit, "%s", reason)
}

// reportExit includes the bounded diagnostic tail, never a payload.
func (attempt *childAttempt) reportExit(code RefusalCode, format string, arguments ...any) error {
	reason := fmt.Sprintf(format, arguments...)
	if tail := attempt.diagnostics.tail(); tail != "" {
		reason = reason + "; child diagnostics: " + tail
	}
	return &Refusal{
		Code:     code,
		Origin:   RefusalOriginSupervisor,
		Reason:   sanitizeReason(reason),
		ExitCode: -1,
		Child:    attempt.report(),
	}
}

// report is the child observation this supervisor can vouch for on its own.
func (attempt *childAttempt) report() *ChildReport {
	if attempt.command == nil || attempt.command.Process == nil {
		return nil
	}
	return &ChildReport{PID: attempt.command.Process.Pid}
}

func (attempt *childAttempt) processID() int {
	if attempt.command == nil || attempt.command.Process == nil {
		return 0
	}
	return attempt.command.Process.Pid
}

// terminate kills the child's whole process group and closes the supervisor's
// ends. It is idempotent and never waits.
func (attempt *childAttempt) terminate() {
	attempt.closeOnce.Do(func() {
		if attempt.command != nil && attempt.command.Process != nil {
			_ = syscall.Kill(-attempt.command.Process.Pid, syscall.SIGKILL)
			_ = attempt.command.Process.Kill()
		}
		if attempt.stdin != nil {
			_ = attempt.stdin.Close()
		}
		if attempt.stdout != nil {
			_ = attempt.stdout.Close()
		}
	})
}

func exitCodeOf(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	if err == nil {
		return 0
	}
	return -1
}

func signalOf(err error) string {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return ""
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return ""
	}
	return status.Signal().String()
}
