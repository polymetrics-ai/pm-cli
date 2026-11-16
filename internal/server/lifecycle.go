package server

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync"

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
// write. A nil handle means no signal.
func (server *Server) Serve(ctx context.Context, listener net.Listener, handler http.Handler, ready io.WriteCloser) error {
	var signal *readySignal
	if ready != nil {
		signal = &readySignal{ready: ready, diagnostics: server.diagnostics}
		defer signal.drop()
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
// runs, once. The write has no deadline: the signal waits until the starter
// reads (ADR 0005). The handle is closed on every path.
type readySignal struct {
	once        sync.Once
	ready       io.WriteCloser
	diagnostics io.Writer
}

// start runs the one write in its own goroutine, so a full pipe cannot stop
// the accept loop.
func (signal *readySignal) start() {
	signal.once.Do(func() { go signal.write() })
}

// drop closes a handle that got no write.
func (signal *readySignal) drop() {
	signal.once.Do(func() { _ = signal.ready.Close() })
}

func (signal *readySignal) write() {
	if _, err := io.WriteString(signal.ready, ReadyLine); err != nil {
		_, _ = io.WriteString(signal.diagnostics, readyWriteFailed)
	}
	_ = signal.ready.Close()
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
