package keyring

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
// its first line.
func startStandIn(t *testing.T, dir, script, want string) *daemon {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = read.Close() })
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Stdout = write
	d, err := startDaemon("stand-in daemon", cmd, filepath.Join(dir, "stand-in.log"))
	_ = write.Close()
	if err != nil {
		t.Fatal(err)
	}
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
