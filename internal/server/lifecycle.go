package server

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	"golang.org/x/net/netutil"
)

var ErrDrainDeadline = errors.New("server drain deadline exceeded")

// Probe observes an existing dependency. It must honor its context and must not
// start, migrate, stop or otherwise provision that dependency.
type Probe func(context.Context) error

type lifecycleState uint8

const (
	stateNew lifecycleState = iota
	stateStarting
	stateServing
	stateDraining
	stateStopped
)

// Server owns its listener and request contexts, not infrastructure or durable
// execution. Closing a request or draining this process cannot cancel a run.
type Server struct {
	settings       config.ServerConfiguration
	admissionProbe Probe
	dispatchProbe  Probe
	http           *http.Server
	requestContext context.Context
	cancelRequests context.CancelFunc
	diagnostics    io.Writer

	mu        sync.Mutex
	state     lifecycleState
	drainErr  error
	drainOnce sync.Once
	draining  chan struct{}
	stopped   chan struct{}
}

func New(configuration config.Configuration, admission, dispatch Probe, diagnostics io.Writer) (*Server, error) {
	if configuration.SchemaVersion() != "1" || admission == nil || diagnostics == nil {
		return nil, errors.New("validated server configuration, admission probe and diagnostics are required")
	}
	settings := configuration.Server()
	requestContext, cancel := context.WithCancel(context.Background())
	server := &Server{
		settings:       settings,
		admissionProbe: admission,
		dispatchProbe:  dispatch,
		requestContext: requestContext,
		cancelRequests: cancel,
		diagnostics:    diagnostics,
		draining:       make(chan struct{}),
		stopped:        make(chan struct{}),
	}
	server.http = &http.Server{
		ReadHeaderTimeout: settings.ReadHeaderTimeout,
		ReadTimeout:       settings.RequestTimeout,
		WriteTimeout:      settings.WriteTimeout,
		IdleTimeout:       settings.IdleTimeout,
		MaxHeaderBytes:    int(settings.MaxHeaderBytes),
		BaseContext:       func(net.Listener) context.Context { return requestContext },
		// The transport logger must not reflect arbitrary panic values.
		ErrorLog: log.New(safeTransportLog{destination: diagnostics}, "", 0),
	}
	return server, nil
}

type safeTransportLog struct {
	destination io.Writer
}

func (writer safeTransportLog) Write(message []byte) (int, error) {
	_, err := io.WriteString(writer.destination, "pm-server: http_transport_failure\n")
	if err != nil {
		return 0, err
	}
	return len(message), nil
}

// Draining closes before graceful shutdown begins. Streaming handlers select
// this signal in addition to their own request context and release only their
// subscriber; ordinary in-flight requests may finish inside the drain bound.
func (server *Server) Draining() <-chan struct{} {
	return server.draining
}

// Serve accepts ownership of listener even when startup fails. A Server serves
// once. Cancellation of ctx initiates bounded drain, rather than immediately
// cancelling otherwise healthy in-flight requests.
//
// Serve also accepts ownership of a non-nil ready handle (ADR 0005). When the
// admission probe passed, the state is serving and the accept loop calls
// Accept for the first time, Serve starts the one write of ReadyLine in its
// own goroutine, apart from the accept loop. Serve closes a handle that got no
// write. When Serve stops serving, it closes the handle of a write that still
// waits, which ends that write for a non-blocking handle, and it waits once
// for the writer, within one drain bound. A nil handle means no signal.
func (server *Server) Serve(ctx context.Context, listener net.Listener, handler http.Handler, ready io.WriteCloser) error {
	var signal *readySignal
	if ready != nil {
		signal = newReadySignal(ready, server.diagnostics)
		defer signal.finish(server.settings.DrainTimeout)
	}
	if listener == nil {
		return errors.New("server listener is required")
	}
	defer listener.Close()
	if ctx == nil || handler == nil {
		return errors.New("server context and handler are required")
	}
	server.mu.Lock()
	if server.state != stateNew {
		server.mu.Unlock()
		return errors.New("server has already been started or drained")
	}
	server.state = stateStarting
	server.mu.Unlock()
	probeContext, cancelProbe := context.WithTimeout(ctx, server.settings.ProbeTimeout)
	stopProbeCancellation := context.AfterFunc(server.requestContext, cancelProbe)
	probeErr := server.admissionProbe(probeContext)
	cancelProbe()
	stopProbeCancellation()
	if probeErr != nil {
		server.beginDrain()
		<-server.stopped
		return errors.New("server admission dependencies are unavailable")
	}
	server.mu.Lock()
	if server.state != stateStarting || ctx.Err() != nil {
		server.mu.Unlock()
		server.beginDrain()
		<-server.stopped
		return errors.New("server is not available for startup")
	}
	server.http.Handler = handler
	server.state = stateServing
	server.mu.Unlock()

	watchStopped := make(chan struct{})
	go func() {
		defer close(watchStopped)
		select {
		case <-ctx.Done():
			server.beginDrain()
		case <-server.stopped:
		}
	}()
	transportListener := httpapi.CloseOnWriteTimeout(listener)
	serving := netutil.LimitListener(transportListener, int(server.settings.MaxConnections))
	if signal != nil {
		serving = readyListener{Listener: serving, signal: signal}
	}
	err := server.http.Serve(serving)
	server.beginDrain()
	if signal != nil {
		// End a ready write that still waits while the server drains. finish
		// waits for the writer at most one drain bound from here.
		signal.stop(time.Now().Add(server.settings.DrainTimeout))
	}
	<-server.stopped
	<-watchStopped
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.New("server listener failed")
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.drainErr
}

// Drain stops readiness immediately and bounds shutdown by the configured
// deadline. A caller may stop waiting without abandoning the owned shutdown.
func (server *Server) Drain(ctx context.Context) error {
	if ctx == nil {
		return errors.New("drain context is required")
	}
	server.beginDrain()
	select {
	case <-server.stopped:
		server.mu.Lock()
		defer server.mu.Unlock()
		return server.drainErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (server *Server) beginDrain() {
	server.drainOnce.Do(func() {
		server.mu.Lock()
		server.state = stateDraining
		close(server.draining)
		server.mu.Unlock()
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), server.settings.DrainTimeout)
			defer cancel()
			err := server.http.Shutdown(ctx)
			server.cancelRequests()
			if err != nil {
				// Shutdown does not forcibly cancel active requests at its
				// deadline. Cancel the owned base context and close sockets.
				_ = server.http.Close()
				err = ErrDrainDeadline
			}
			server.mu.Lock()
			server.drainErr = err
			server.state = stateStopped
			server.mu.Unlock()
			close(server.stopped)
		}()
	})
}

// ReadyLine is the one ready line of pm-server (ADR 0005).
const ReadyLine = "pm-server ready\n"

// readyWriteFailed is the one standard error line after a failed ready write.
const readyWriteFailed = "pm-server: the ready signal write failed. The process continues.\n"

// readySignal owns the ready handle of one Serve call. Either start or drop
// runs, once. While Serve serves, the write has no deadline: the signal waits
// until the starter reads (ADR 0005). When Serve stops serving, stop closes
// the handle of a write that still waits, and finish waits once for the
// writer. The handle is closed once, on every path.
type readySignal struct {
	once        sync.Once
	ready       io.WriteCloser
	diagnostics io.Writer

	started   atomic.Bool
	stopping  atomic.Bool
	closeOnce sync.Once
	written   chan struct{}
	joinBy    time.Time
}

func newReadySignal(ready io.WriteCloser, diagnostics io.Writer) *readySignal {
	return &readySignal{ready: ready, diagnostics: diagnostics, written: make(chan struct{})}
}

// start runs the one write in its own goroutine, so a full pipe cannot stop
// the accept loop.
func (signal *readySignal) start() {
	signal.once.Do(func() {
		signal.started.Store(true)
		go signal.write()
	})
}

// drop closes a handle that got no write.
func (signal *readySignal) drop() {
	signal.once.Do(signal.close)
}

// close closes the handle once.
func (signal *readySignal) close() {
	signal.closeOnce.Do(func() { _ = signal.ready.Close() })
}

// write writes the ready line once and closes the handle. A failed write
// prints the write-failure line, unless stop ended the write: then the
// process does not continue, so it prints nothing.
func (signal *readySignal) write() {
	defer close(signal.written)
	if _, err := io.WriteString(signal.ready, ReadyLine); err != nil && !signal.stopping.Load() {
		_, _ = io.WriteString(signal.diagnostics, readyWriteFailed)
	}
	signal.close()
}

// stop runs when Serve stops serving. It keeps joinBy, the end of the one
// wait in finish. For a started write, it marks the stop and then closes the
// handle. The close ends a write that still waits on a non-blocking handle,
// which is the handle that pm-server makes from its ready descriptor.
func (signal *readySignal) stop(joinBy time.Time) {
	signal.joinBy = joinBy
	if !signal.started.Load() {
		return
	}
	signal.stopping.Store(true)
	signal.close()
}

// finish runs when Serve returns, on every path. It closes a handle that got
// no write. For a started write, it ends the write (stop, if Serve did not
// call it) and waits once for the writer, until joinBy.
func (signal *readySignal) finish(drainBound time.Duration) {
	signal.drop()
	if !signal.started.Load() {
		return
	}
	if signal.joinBy.IsZero() {
		signal.stop(time.Now().Add(drainBound))
	}
	timer := time.NewTimer(time.Until(signal.joinBy))
	defer timer.Stop()
	select {
	case <-signal.written:
	case <-timer.C:
	}
}

// readyListener starts the ready signal when the accept loop calls Accept
// for the first time.
type readyListener struct {
	net.Listener
	signal *readySignal
}

func (listener readyListener) Accept() (net.Conn, error) {
	listener.signal.start()
	return listener.Listener.Accept()
}
