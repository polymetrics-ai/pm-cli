package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"syscall"

	"github.com/cucumber/godog"

	"github.com/polymetrics-ai/pm-cli/tests/starter"
)

// registerListenerSteps registers the steps of the --listen-fd scenarios
// (ADR 0008, criteria N1 to N6). No step connects to the pm-server address,
// and no step repeats an observation or sleeps.
func registerListenerSteps(sc *godog.ScenarioContext, s *stackScenario) {
	sc.Step(`^the starter (?:still )?holds the listener at the configured pm-server address$`, s.holdsServerListener)
	sc.Step(`^the starter holds a second listener at another loopback port$`, s.holdSecondListener)
	sc.Step(`^the starter starts pm-server with --listen-fd on the (held|second) listener and waits for its ready signal or its exit$`, s.startServerWithListenFD)
	sc.Step(`^the starter starts pm-server without --listen-fd and waits for its ready signal or its exit$`, s.startServerWithoutListenFD)
	sc.Step(`^(pm-server|pm-worker) stdout is empty$`, s.stdoutIsEmpty)
}

// holdsServerListener is N1, in both spellings. It makes one observation of
// the held listener of the deployment (H1), with no wait and no connection.
// It passes only when the listener is open, is a TCP socket in the listen
// state, and its local address equals the server.listen value.
func (s *stackScenario) holdsServerListener() error {
	if s.deployment == nil {
		return errors.New("the scenario has no deployment configuration")
	}
	if s.deployment.listener == nil {
		return errors.New("the deployment has no pm-server listener")
	}
	want, err := s.deployment.configuredListen()
	if err != nil {
		return err
	}
	// A released listener is closed, so this read fails.
	state, err := readListenerState(s.deployment.listener)
	if err != nil {
		return fmt.Errorf("the starter does not hold an open pm-server listener: %w", err)
	}
	switch {
	case state.socketType != syscall.SOCK_STREAM || !state.inet4:
		return fmt.Errorf("the held pm-server listener is not a TCP socket for IPv4 (socket type %d)", state.socketType)
	case !state.listening:
		return errors.New("the held pm-server listener is not in the listen state")
	case state.local != want:
		return errors.New("the local address of the held pm-server listener differs from server.listen")
	}
	return nil
}

// holdSecondListener is N2. The starter binds 127.0.0.1:0 once and keeps the
// listener until the end of the scenario. It passes only when its port
// differs from the port of server.listen. It makes no wait and no connection.
func (s *stackScenario) holdSecondListener() error {
	if s.deployment == nil {
		return errors.New("the scenario has no deployment configuration")
	}
	if s.secondListener != nil {
		return errors.New("the starter already holds a second listener")
	}
	configured, err := s.deployment.configuredListen()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("bind the second loopback listener: %w", err)
	}
	s.secondListener = listener.(*net.TCPListener)
	if s.secondListener.Addr().(*net.TCPAddr).AddrPort().Port() == configured.Port() {
		return errors.New("the second listener has the port of server.listen")
	}
	return nil
}

// startServerWithListenFD is N3 (the held listener) and N4 (the second
// listener). It makes the same start as startAndWait, through start, with
// --listen-fd 4 and a copy of the named listener as descriptor 4. N3 uses
// giveListener of the deployment. The harness keeps its own copy of each
// listener until the scenario ends: it does not release it after a failed
// start or after an answer through the edge. Go opens each listener with
// close-on-exec, so pm-server gets no other listener. It waits once for the
// ready line or end of file, with the start limit.
func (s *stackScenario) startServerWithListenFD(ctx context.Context, which string) error {
	if s.deployment == nil {
		return errors.New("the scenario has no deployment configuration")
	}
	config, err := s.serverStartConfig()
	if err != nil {
		return err
	}
	if which == "held" {
		if err := s.deployment.giveListener(&config); err != nil {
			return err
		}
	} else {
		if s.secondListener == nil {
			return errors.New("the starter does not hold a second listener")
		}
		file, err := s.secondListener.File()
		if err != nil {
			return fmt.Errorf("copy the second listener for pm-server: %w", err)
		}
		config.Args = append(config.Args, "--listen-fd", strconv.Itoa(serverListenerFD))
		config.ListenFD = serverListenerFD
		config.Listener = file
	}
	// starter.Start closes the copy. This close covers a step that returns
	// before starter.Start. The error of a second close is ignored.
	defer config.Listener.Close()
	_, err = s.start(ctx, "pm-server", config)
	return err
}

// startServerWithoutListenFD is N5. It starts pm-server with
// --config, --secrets-directory and --ready-fd 3 only, the arguments of the
// base start step, and passes no listener. The harness keeps its held
// listener open. Descriptor 4 is closed in pm-server. It waits once for the
// ready line or end of file, with the start limit.
func (s *stackScenario) startServerWithoutListenFD(ctx context.Context) error {
	if s.deployment == nil {
		return errors.New("the scenario has no deployment configuration")
	}
	config, err := s.serverStartConfig()
	if err != nil {
		return err
	}
	_, err = s.start(ctx, "pm-server", config)
	return err
}

// serverStartConfig is the start of startAndWait before giveListener: the
// configuration, the secret root and --ready-fd 3, with the write end of a
// new ready pipe as descriptor 3. It also sets the hold of the deployment
// listener, so that the harness keeps its copy until the deployment closes.
func (s *stackScenario) serverStartConfig() (starter.Config, error) {
	configuration, err := s.deployment.configurationPath()
	if err != nil {
		return starter.Config{}, err
	}
	s.deployment.holdListener.Store(true)
	return starter.Config{
		Args:    []string{"--config", configuration, "--secrets-directory", s.deployment.secrets, "--ready-fd", "3"},
		Env:     s.deployment.processEnvironment(),
		Dir:     s.deployment.home(),
		ReadyFD: 3,
	}, nil
}

// stdoutIsEmpty is N6. It reads the whole stdout after the exit, with no
// wait. It uses the conditions of stderrIsExactlyLine: the process exited,
// both outputs reached end of file and the starter holds every byte. It
// passes only when stdout has no byte.
func (s *stackScenario) stdoutIsEmpty(name string) error {
	started, err := s.started(name)
	if err != nil {
		return err
	}
	report := started.process.Snapshot()
	switch {
	case !report.Exited:
		return fmt.Errorf("%s has not exited, so its whole stdout is not known: %s", name, report)
	case !report.OutputEnded || report.StdoutTotal != int64(len(report.Stdout)):
		return fmt.Errorf("the starter does not hold the whole %s stdout (%d bytes read): %s", name, report.StdoutTotal, report)
	case report.StdoutTotal != 0:
		return fmt.Errorf("%s stdout is %q, want no byte", name, report.Stdout)
	}
	return nil
}

// configuredListen returns the server.listen value of the deployment. After
// the first start it reads the written configuration file. Before it, it
// returns the value that the configuration gets.
func (d *deployment) configuredListen() (netip.AddrPort, error) {
	value := d.listen
	if d.configuration != "" {
		contents, err := readBoundedFile(d.configuration, laneFileLimit)
		if err != nil {
			return netip.AddrPort{}, err
		}
		var document struct {
			Server struct {
				Listen string `json:"listen"`
			} `json:"server"`
		}
		if err := json.Unmarshal(contents, &document); err != nil {
			return netip.AddrPort{}, fmt.Errorf("decode the server.listen value of the deployment configuration: %w", err)
		}
		value = document.Server.Listen
	}
	address, err := netip.ParseAddrPort(value)
	if err != nil {
		return netip.AddrPort{}, errors.New("the server.listen value of the deployment is not an IP address and port")
	}
	return address, nil
}

// listenerState is one observation of a listener socket.
type listenerState struct {
	socketType int
	listening  bool
	inet4      bool
	local      netip.AddrPort
}

// readListenerState reads the socket type, the listen state and the local
// address of the listener with getsockopt and getsockname. It makes no
// connection. A closed listener is an error.
func readListenerState(listener *net.TCPListener) (listenerState, error) {
	raw, err := listener.SyscallConn()
	if err != nil {
		return listenerState{}, err
	}
	var state listenerState
	var callErr error
	err = raw.Control(func(fd uintptr) {
		if state.socketType, callErr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_TYPE); callErr != nil {
			callErr = fmt.Errorf("getsockopt SO_TYPE: %w", callErr)
			return
		}
		accepting, err := syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_ACCEPTCONN)
		if err != nil {
			callErr = fmt.Errorf("getsockopt SO_ACCEPTCONN: %w", err)
			return
		}
		state.listening = accepting != 0
		name, err := syscall.Getsockname(int(fd))
		if err != nil {
			callErr = fmt.Errorf("getsockname: %w", err)
			return
		}
		if inet4, ok := name.(*syscall.SockaddrInet4); ok {
			state.inet4 = true
			state.local = netip.AddrPortFrom(netip.AddrFrom4(inet4.Addr), uint16(inet4.Port))
		}
	})
	if err != nil {
		return listenerState{}, err
	}
	if callErr != nil {
		return listenerState{}, callErr
	}
	return state, nil
}
