package acceptance

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/polymetrics-ai/pm-cli/tests/starter"
)

// laneFakeServerName is the file name of the fake pm-server of
// TestLaneGivesItsPmServerTheHeldListener. The check makes a symbolic link
// with this name to the test binary. A start of the test binary through that
// link runs the fake and not the tests.
const laneFakeServerName = "lane-fake-pm-server"

func init() {
	if filepath.Base(os.Args[0]) == laneFakeServerName {
		os.Exit(runLaneFakeServer(os.Args[1:], os.Stderr))
	}
}

// Developer check of the lane start with the held listener (ADR 0008), not
// acceptance proof. It needs no stack. The fake pm-server binds the
// configured address itself unless it gets --listen-fd, as pm-server does.
// Defect: the lane starts pm-server without the held listener of its
// deployment, so the bind of pm-server gets EADDRINUSE while the deployment
// holds the address and the lane gets no ready signal. Or a failed start
// keeps the harness copy of the listener open. Each wait is the start or stop
// limit of the starter. No sleep, poll or retry.
func TestLaneGivesItsPmServerTheHeldListener(t *testing.T) {
	fake := laneFakeServer(t)

	t.Run("the start gets the held listener", func(t *testing.T) {
		l := &lane{deployment: newLaneListenerDeployment(t)}
		configuration := writeLaneFakeConfiguration(t, l.deployment.listen)
		process, err := l.startBinary(context.Background(), "pm-server", fake, configuration)
		if err != nil {
			t.Fatalf("the lane pm-server start while the deployment holds the address = %v; want the ready signal", err)
		}
		report := process.Stop()
		if report.Stop != starter.StopAfterSIGTERM || report.ExitCode != 0 {
			t.Fatalf("stop the fake pm-server: %s", report)
		}
	})

	// R13 of the W1b plan v2.1: the O4 restart stops and starts pm-server
	// after many answers through the edge. Defect: the first answer releases
	// the held listener, so the restarted pm-server gets no listener.
	t.Run("a restart scenario restarts pm-server with the held listener", func(t *testing.T) {
		l := &lane{deployment: newLaneListenerDeployment(t), restartsServer: true}
		l.holdListenerForRestart()
		configuration := writeLaneFakeConfiguration(t, l.deployment.listen)
		startLaneFakeServer(t, l, fake, configuration)
		l.deployment.servedThroughEdge()
		if err := l.restartServerBinary(context.Background(), fake, configuration); err != nil {
			t.Fatalf("the restart of pm-server after an answer through the edge = %v; want the ready signal", err)
		}
	})

	// W1b plan v2.2, M6: a restart that fails must not keep the held
	// socket open until the deployment closes.
	t.Run("a failed restart releases the held listener", func(t *testing.T) {
		l := &lane{deployment: newLaneListenerDeployment(t), restartsServer: true}
		l.holdListenerForRestart()
		configuration := writeLaneFakeConfiguration(t, l.deployment.listen)
		startLaneFakeServer(t, l, fake, configuration)
		l.deployment.servedThroughEdge()
		missing := filepath.Join(t.TempDir(), "missing.json")
		if err := l.restartServerBinary(context.Background(), fake, missing); err == nil {
			t.Fatal("a restart with no configuration file = nil error; want a failed start")
		}
		if err := bindAddressOnce(l.deployment.listen); err != nil {
			t.Fatalf("a bind of the held address after the failed restart = %v; want a free address", err)
		}
	})

	t.Run("a scenario without the restart tag releases the listener after the first answer", func(t *testing.T) {
		l := &lane{deployment: newLaneListenerDeployment(t)}
		l.holdListenerForRestart()
		configuration := writeLaneFakeConfiguration(t, l.deployment.listen)
		startLaneFakeServer(t, l, fake, configuration)
		l.deployment.servedThroughEdge()
		if err := stopLaneProcess("pm-server", l.server); err != nil {
			t.Fatal(err)
		}
		if err := bindAddressOnce(l.deployment.listen); err != nil {
			t.Fatalf("a bind of the address after the first answer and the stop = %v; want a free address", err)
		}
		if err := l.restartServerBinary(context.Background(), fake, configuration); err == nil {
			t.Fatal("a restart without the restart tag = nil error; want the refusal that names the tag")
		}
	})

	t.Run("a failed start releases the held listener", func(t *testing.T) {
		d := newLaneListenerDeployment(t)
		l := &lane{deployment: d}
		missing := filepath.Join(t.TempDir(), "missing.json")
		if _, err := l.startBinary(context.Background(), "pm-server", fake, missing); err == nil {
			t.Fatal("the lane pm-server start with no configuration file = nil error; want a failed start")
		}
		if err := bindAddressOnce(d.listen); err != nil {
			t.Fatalf("a bind of the held address after the failed start = %v; want a free address", err)
		}
	})
}

// startLaneFakeServer starts the fake pm-server as the lane pm-server. The
// cleanup stops it.
func startLaneFakeServer(t *testing.T, l *lane, fake, configuration string) {
	t.Helper()
	process, err := l.startBinary(context.Background(), "pm-server", fake, configuration)
	if err != nil {
		t.Fatalf("the lane pm-server start = %v; want the ready signal", err)
	}
	l.server = process
	t.Cleanup(func() {
		if err := stopLaneProcess("pm-server", l.server); err != nil {
			t.Error(err)
		}
	})
}

// laneFakeServer returns the path of a symbolic link with the name
// laneFakeServerName to the test binary.
func laneFakeServer(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), laneFakeServerName)
	if err := os.Symlink(executable, link); err != nil {
		t.Fatal(err)
	}
	return link
}

// newLaneListenerDeployment returns a deployment with only a held listener
// and the directories of the process environment. The cleanup releases the
// listener.
func newLaneListenerDeployment(t *testing.T) *deployment {
	t.Helper()
	listener, err := holdLoopbackListener()
	if err != nil {
		t.Fatal(err)
	}
	d := &deployment{dir: t.TempDir(), listener: listener, listen: listener.Addr().String()}
	t.Cleanup(d.releaseListener)
	d.secrets = filepath.Join(d.dir, "secrets")
	for _, directory := range []string{d.home(), filepath.Join(d.dir, "tmp"), d.secrets} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// laneFakeConfiguration is the part of the server configuration that the
// fake pm-server reads.
type laneFakeConfiguration struct {
	Server struct {
		Listen string `json:"listen"`
	} `json:"server"`
}

// writeLaneFakeConfiguration writes a configuration file with server.listen
// for the fake pm-server.
func writeLaneFakeConfiguration(t *testing.T, listen string) string {
	t.Helper()
	var configuration laneFakeConfiguration
	configuration.Server.Listen = listen
	encoded, err := json.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pm-deployment.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// runLaneFakeServer is the fake pm-server. It reads server.listen from
// --config. With --listen-fd it serves on the inherited listener, which must
// be at that address. Without it, it binds the address itself. Then it writes
// the ready line to the --ready-fd descriptor and accepts connections until
// SIGTERM, which ends it with exit code 0. It returns 1 on each failure.
func runLaneFakeServer(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet(laneFakeServerName, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configurationPath := flags.String("config", "", "")
	flags.String("secrets-directory", "", "")
	readyFD := flags.Int("ready-fd", 0, "")
	listenFD := flags.Int("listen-fd", 0, "")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	fail := func(format string, values ...any) int {
		_, _ = fmt.Fprintf(stderr, "lane fake pm-server: "+format+"\n", values...)
		return 1
	}
	encoded, err := os.ReadFile(*configurationPath)
	if err != nil {
		return fail("read the configuration: %v", err)
	}
	var configuration laneFakeConfiguration
	if err := json.Unmarshal(encoded, &configuration); err != nil {
		return fail("decode the configuration: %v", err)
	}
	listen := configuration.Server.Listen

	var listener net.Listener
	if *listenFD > 0 {
		file := os.NewFile(uintptr(*listenFD), "listen-fd")
		listener, err = net.FileListener(file)
		_ = file.Close()
		if err != nil {
			return fail("use the inherited listener: %v", err)
		}
		if got := listener.Addr().String(); got != listen {
			_ = listener.Close()
			return fail("the inherited listener is at %s, not at the configured address", got)
		}
	} else if listener, err = net.Listen("tcp", listen); err != nil {
		return fail("configured server listener is unavailable: %v", err)
	}
	defer listener.Close()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM)
	ready := os.NewFile(uintptr(*readyFD), "ready-fd")
	_, err = io.WriteString(ready, "pm-server ready\n")
	_ = ready.Close()
	if err != nil {
		return fail("write the ready line: %v", err)
	}
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			_ = connection.Close()
		}
	}()
	<-stop
	return 0
}

// Developer check of the harness listener read (#405 review R2), not
// acceptance proof. Defect: the read refuses a valid held listener on a
// system where getsockopt SO_ACCEPTCONN is not supported (Darwin), so LF1 to
// LF3 fail there before pm-server starts.
func TestHarnessReadsTheStateOfAHeldListener(t *testing.T) {
	listener, err := holdLoopbackListener()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	state, err := readListenerState(listener)
	if err != nil {
		t.Fatalf("readListenerState of a held listener = %v; want its state", err)
	}
	if state.socketType != syscall.SOCK_STREAM || !state.listening || !state.inet4 || state.local.String() != listener.Addr().String() {
		t.Fatalf("readListenerState = %+v; want a listening IPv4 stream socket at %s", state, listener.Addr())
	}
}
