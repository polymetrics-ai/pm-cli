package main

import (
	"context"
	"errors"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	namespacepb "go.temporal.io/api/namespace/v1"
	"go.temporal.io/api/serviceerror"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"google.golang.org/grpc"
)

// These are Go developer tests, not acceptance proof. They use in-process
// fakes. Each wait is one wait for a fake's signal or for the return of the
// run path, with one time limit. Nothing repeats, and nothing sleeps.

const workerTestLimit = 10 * time.Second

// eventLog records the order of events from the fakes.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (log *eventLog) add(event string) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.events = append(log.events, event)
}

func (log *eventLog) snapshot() []string {
	log.mu.Lock()
	defer log.mu.Unlock()
	return slices.Clone(log.events)
}

// fakeWorker is an in-process fake of the Temporal worker. As the SDK worker
// does, Start starts a poller in its own goroutine and returns. The poller
// completes each unit of work that the test sends, until Stop. fail models
// the SDK contract of OnFatalError: it delivers the error, and then the
// worker stops itself.
type fakeWorker struct {
	log      *eventLog
	onStart  func()
	work     chan chan struct{}
	quit     chan struct{}
	quitOnce sync.Once
	fatal    chan<- error
	stops    atomic.Int32
	selfStop chan struct{}
}

func newFakeWorker(log *eventLog, fatal chan<- error) *fakeWorker {
	return &fakeWorker{
		log:      log,
		work:     make(chan chan struct{}),
		quit:     make(chan struct{}),
		fatal:    fatal,
		selfStop: make(chan struct{}),
	}
}

func (w *fakeWorker) Start() error {
	go func() {
		for {
			select {
			case done := <-w.work:
				close(done)
			case <-w.quit:
				return
			}
		}
	}()
	if w.onStart != nil {
		w.onStart()
	}
	w.log.add("start returned")
	return nil
}

func (w *fakeWorker) Stop() {
	w.stops.Add(1)
	w.quitOnce.Do(func() { close(w.quit) })
}

func (w *fakeWorker) fail(err error) {
	go func() {
		w.fatal <- err
		w.Stop()
		close(w.selfStop)
	}()
}

// fakeReadyHandle is an in-process fake of the ready descriptor. Write
// records the event and its bytes, signals that it started, and then blocks
// until the test releases it, as a write to a full pipe does.
type fakeReadyHandle struct {
	log     *eventLog
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	written atomic.Value
	writes  atomic.Int32
	closed  atomic.Bool
}

func newFakeReadyHandle(t *testing.T, log *eventLog) *fakeReadyHandle {
	handle := &fakeReadyHandle{log: log, entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(handle.release) })
	return handle
}

func (handle *fakeReadyHandle) Write(payload []byte) (int, error) {
	handle.log.add("write started")
	handle.writes.Add(1)
	handle.written.Store(string(payload))
	handle.once.Do(func() { close(handle.entered) })
	<-handle.release
	return len(payload), nil
}

func (handle *fakeReadyHandle) Close() error {
	handle.closed.Store(true)
	return nil
}

func awaitSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(workerTestLimit):
		t.Fatalf("no %s within %s", what, workerTestLimit)
	}
}

func awaitReturn(t *testing.T, returned <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-returned:
		return err
	case <-time.After(workerTestLimit):
		t.Fatalf("run path did not return within %s %s", workerTestLimit, what)
		return nil
	}
}

// Fact (b), run path. Defects: a ready write before Start (no unit of work
// completes); a ready write on the run path after Start (no return after
// cancel); a ready write that starts before Start returned (the event order).
// Proof limit: the order check sees a write goroutine that the code starts
// too early only when that goroutine runs before Start returns.
func TestReadyWriteRunsApartFromTheWork(t *testing.T) {
	log := &eventLog{}
	handle := newFakeReadyHandle(t, log)
	sdkWorker := newFakeWorker(log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	returned := make(chan error, 1)
	go func() {
		returned <- runWorker(ctx, sdkWorker, make(chan error), &readySignal{handle: handle, stderr: os.Stderr})
	}()

	awaitSignal(t, handle.entered, "start of the ready write")
	if got, _ := handle.written.Load().(string); got != readyLine {
		t.Fatalf("ready write %q, want %q", got, readyLine)
	}
	if got, want := log.snapshot(), []string{"start returned", "write started"}; !slices.Equal(got, want) {
		t.Fatalf("events %q, want %q: the ready write must start only after Start returned", got, want)
	}
	unit := make(chan struct{})
	select {
	case sdkWorker.work <- unit:
	case <-time.After(workerTestLimit):
		t.Fatalf("no poller took a unit of work within %s while the ready write is blocked", workerTestLimit)
	}
	awaitSignal(t, unit, "completed unit of work while the ready write is blocked")

	cancel()
	if err := awaitReturn(t, returned, "after cancel while the ready write is blocked"); err != nil {
		t.Fatalf("run path after cancel: %v", err)
	}
	if sdkWorker.stops.Load() != 1 {
		t.Fatalf("run path made %d Stop calls after cancel, want 1", sdkWorker.stops.Load())
	}
	if handle.writes.Load() != 1 {
		t.Fatalf("ready writes %d, want 1", handle.writes.Load())
	}
}

// Amendment A28. Defect: a ready write without the context check. The run
// path closes the handle before it returns when no write started, so a write
// goroutine that owns the handle leaves it open at the return.
func TestNoReadyWriteWhenTheContextEndsDuringStart(t *testing.T) {
	log := &eventLog{}
	handle := newFakeReadyHandle(t, log)
	sdkWorker := newFakeWorker(log, nil)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sdkWorker.onStart = cancel
	returned := make(chan error, 1)
	go func() {
		returned <- runWorker(ctx, sdkWorker, make(chan error), &readySignal{handle: handle, stderr: os.Stderr})
	}()

	if err := awaitReturn(t, returned, "after the context ended during Start"); err != nil {
		t.Fatalf("run path: %v", err)
	}
	if !handle.closed.Load() {
		t.Fatal("run path returned with the ready handle open: a ready write started after the context ended")
	}
	if handle.writes.Load() != 0 {
		t.Fatalf("ready writes %d after the context ended during Start, want 0", handle.writes.Load())
	}
	if sdkWorker.stops.Load() != 1 {
		t.Fatalf("run path made %d Stop calls, want 1", sdkWorker.stops.Load())
	}
}

// The fatal-error test. Defect: a run path that does not wait for the fatal
// error of OnFatalError. The fake worker stops itself after it delivers the
// error, as the SDK does. The run path makes no second Stop call, because a
// second SDK Stop at the same time can close the stop channel twice.
func TestRunPathReturnsAfterAFatalWorkerError(t *testing.T) {
	log := &eventLog{}
	handle := newFakeReadyHandle(t, log)
	fatal := make(chan error)
	sdkWorker := newFakeWorker(log, fatal)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	returned := make(chan error, 1)
	go func() {
		returned <- runWorker(ctx, sdkWorker, fatal, &readySignal{handle: handle, stderr: os.Stderr})
	}()

	awaitSignal(t, handle.entered, "start of the ready write")
	sdkWorker.fail(errors.New("non-retryable poll error"))
	err := awaitReturn(t, returned, "after a fatal worker error")
	if err == nil || err.Error() != "Temporal worker stopped after a fatal runtime error" {
		t.Fatalf("run path after a fatal worker error: %v", err)
	}
	awaitSignal(t, sdkWorker.selfStop, "stop of the worker after its fatal error")
	if got := sdkWorker.stops.Load(); got != 1 {
		t.Fatalf("Stop calls %d after a fatal worker error, want 1 (the worker stops itself)", got)
	}
}

// Fact (c). Defect: a kept handle without FD_CLOEXEC after the check. The
// handle is also descriptor n itself, not a copy.
func TestReadyHandleHasCloseOnExecAfterTheCheck(t *testing.T) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = readEnd.Close()
		_ = writeEnd.Close()
	})
	// dup gives a descriptor without close-on-exec, as an inherited one has.
	inherited, err := syscall.Dup(int(writeEnd.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if flags, err := fcntl(inherited, syscall.F_GETFD, 0); err != nil || flags&syscall.FD_CLOEXEC != 0 {
		_ = syscall.Close(inherited)
		t.Fatalf("test descriptor flags %#x, %v: want no close-on-exec before the check", flags, err)
	}
	handle, err := openReadyDescriptor(inherited)
	if err != nil {
		_ = syscall.Close(inherited)
		t.Fatalf("check refused an inherited write descriptor: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })
	if got := int(handle.Fd()); got != inherited {
		t.Fatalf("kept handle is descriptor %d, want descriptor %d itself", got, inherited)
	}
	flags, err := fcntl(int(handle.Fd()), syscall.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&syscall.FD_CLOEXEC == 0 {
		t.Fatalf("kept handle flags %#x: want FD_CLOEXEC after the check", flags)
	}
}

// fakeTemporalService is an in-process fake of the Temporal workflow
// service. It answers the two own calls and records each call, its context
// and its call options. Any other method panics.
type fakeTemporalService struct {
	workflowservice.WorkflowServiceClient
	systemInfoErr    error
	systemInfoAnswer bool
	namespaceErr     error
	answerNamespace  string
	calls            []string
	contexts         []context.Context
	options          int
}

func (service *fakeTemporalService) GetSystemInfo(
	ctx context.Context,
	_ *workflowservice.GetSystemInfoRequest,
	options ...grpc.CallOption,
) (*workflowservice.GetSystemInfoResponse, error) {
	service.calls = append(service.calls, "GetSystemInfo")
	service.contexts = append(service.contexts, ctx)
	service.options += len(options)
	if service.systemInfoErr != nil || !service.systemInfoAnswer {
		return nil, service.systemInfoErr
	}
	return &workflowservice.GetSystemInfoResponse{}, nil
}

func (service *fakeTemporalService) DescribeNamespace(
	ctx context.Context,
	request *workflowservice.DescribeNamespaceRequest,
	options ...grpc.CallOption,
) (*workflowservice.DescribeNamespaceResponse, error) {
	service.calls = append(service.calls, "DescribeNamespace "+request.GetNamespace())
	service.contexts = append(service.contexts, ctx)
	service.options += len(options)
	if service.namespaceErr != nil {
		return nil, service.namespaceErr
	}
	return &workflowservice.DescribeNamespaceResponse{
		NamespaceInfo: &namespacepb.NamespaceInfo{Name: service.answerNamespace},
	}, nil
}

type ownCallCase struct {
	name    string
	service fakeTemporalService
	text    string
	calls   []string
}

// The own-call test. Defects: retry options on a call; acceptance of a
// result other than success; a failure text that the status code chooses.
func TestOwnTemporalCallsAcceptOnlySuccess(t *testing.T) {
	const (
		namespace      = "pm-ready"
		dependencyText = "Temporal worker dependency is unavailable"
		namespaceText  = "Temporal namespace is unavailable"
	)
	bothCalls := []string{"GetSystemInfo", "DescribeNamespace " + namespace}
	cases := []ownCallCase{
		{name: "success", service: fakeTemporalService{systemInfoAnswer: true, answerNamespace: namespace}, calls: bothCalls},
		{name: "DescribeNamespace names another namespace", service: fakeTemporalService{systemInfoAnswer: true, answerNamespace: "pm-other"}, text: namespaceText, calls: bothCalls},
		{name: "GetSystemInfo gives no answer", service: fakeTemporalService{answerNamespace: namespace}, text: dependencyText, calls: bothCalls[:1]},
	}
	// Each status code goes to the failure text of its call.
	statuses := []struct {
		name string
		err  error
	}{
		{"not implemented", serviceerror.NewUnimplemented("not implemented")},
		{"unavailable", serviceerror.NewUnavailable("unavailable")},
		{"namespace not found", serviceerror.NewNamespaceNotFound(namespace)},
		{"permission denied", serviceerror.NewPermissionDenied("denied", "")},
	}
	for _, status := range statuses {
		cases = append(cases,
			ownCallCase{name: "GetSystemInfo " + status.name, service: fakeTemporalService{systemInfoErr: status.err, answerNamespace: namespace}, text: dependencyText, calls: bothCalls[:1]},
			ownCallCase{name: "DescribeNamespace " + status.name, service: fakeTemporalService{systemInfoAnswer: true, namespaceErr: status.err}, text: namespaceText, calls: bothCalls},
		)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			service := tc.service
			err := checkTemporalService(ctx, &service, namespace)
			switch {
			case tc.text == "" && err != nil:
				t.Fatalf("own calls refused a success answer: %v", err)
			case tc.text != "" && (err == nil || err.Error() != tc.text):
				t.Fatalf("own calls result %v, want %q", err, tc.text)
			}
			if !slices.Equal(service.calls, tc.calls) {
				t.Fatalf("calls %q, want %q (each call once, in order)", service.calls, tc.calls)
			}
			if service.options != 0 {
				t.Fatalf("calls carried %d call options, want none", service.options)
			}
			for index, callContext := range service.contexts {
				if callContext != ctx {
					t.Fatalf("call %d used another context than the caller's, which can carry retry options", index)
				}
			}
		})
	}
}
