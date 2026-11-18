// Package keyring starts one private Secret Service for the acceptance
// harness: a private D-Bus session bus on a unix socket in a 0700 job
// directory, and a gnome-keyring-daemon whose login keyring is unlocked with a
// random password. It is test-harness code, not PM behaviour.
//
// The bus configuration lists no service directory, so the bus cannot
// activate a service. Only the keyring daemon and the pm processes get the bus
// address. The start checks that the owner of org.freedesktop.secrets is the
// keyring daemon that it started (GetConnectionUnixProcessID). This is
// hygiene: a process of the same account can still monitor the bus.
//
// Each wait has one limit, and nothing repeats. One deadline, the start
// limit, covers the whole start: the bus address line, the first owner change
// of org.freedesktop.secrets and the bus calls of the owner check. The stop
// sends SIGTERM to each process group, waits once up to the stop limit, then
// sends SIGKILL and waits once up to the kill limit. When a process of a group
// stays after its leader exited, the stop sends SIGKILL to that group and
// reports it. Each stop acts once.
package keyring

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	busDaemon     = "/usr/bin/dbus-daemon"
	keyringDaemon = "/usr/bin/gnome-keyring-daemon"
	busName       = "org.freedesktop.DBus"
	secretsName   = "org.freedesktop.secrets"
	logLimit      = 4 << 10
	// socketLimit keeps the bus socket path below the unix socket path
	// limit of Linux (108 bytes) and macOS (104 bytes).
	socketLimit = 100
)

// Limits holds the three limits of one keyring. Start covers the whole start.
// Stop and Kill bound the two phases of the stop of each daemon.
type Limits struct {
	Start time.Duration
	Stop  time.Duration
	Kill  time.Duration
}

// Keyring is one started private Secret Service. Stop removes it.
type Keyring struct {
	dir     string
	address string
	limits  Limits
	ready   bool
	bus     *daemon
	keyring *daemon
	// busOutput is the read end of the bus address pipe.
	busOutput *os.File
	// beforeKeyring runs after the bus check and before the keyring daemon
	// starts. Only the Go developer tests set it.
	beforeKeyring func(address string) error
	// stopOnce makes Stop act once. A later call returns the first result.
	stopOnce sync.Once
	stopErr  error
}

// daemon is one started process in its own process group. done closes after
// the process exited and was reaped.
type daemon struct {
	name string
	cmd  *exec.Cmd
	done chan struct{}
	log  string
	// stopOnce makes stop act once. A later call returns the first result.
	stopOnce sync.Once
	stopErr  error
}

// Start makes a new 0700 directory in parent and starts the bus and the
// keyring daemon in it. On a failure it stops what it started and removes the
// directory.
func Start(parent string, limits Limits) (*Keyring, error) {
	return start(parent, limits, nil)
}

func start(parent string, limits Limits, beforeKeyring func(string) error) (*Keyring, error) {
	if !filepath.IsAbs(parent) {
		return nil, errors.New("keyring: the parent directory must be an absolute path")
	}
	if limits.Start <= 0 || limits.Stop <= 0 || limits.Kill <= 0 {
		return nil, errors.New("keyring: the start, stop and kill limits must be more than 0")
	}
	dir, err := os.MkdirTemp(parent, "pm-keyring-")
	if err != nil {
		return nil, fmt.Errorf("keyring: create the keyring directory: %w", err)
	}
	k := &Keyring{dir: dir, limits: limits, beforeKeyring: beforeKeyring}
	ctx, cancel := context.WithTimeout(context.Background(), limits.Start)
	defer cancel()
	if err := k.start(ctx); err != nil {
		return nil, errors.Join(err, k.Stop())
	}
	k.ready = true
	return k, nil
}

func (k *Keyring) start(ctx context.Context) error {
	for _, name := range []string{"data", "run", "home", "config"} {
		if err := os.Mkdir(filepath.Join(k.dir, name), 0o700); err != nil {
			return fmt.Errorf("keyring: create the %s directory: %w", name, err)
		}
	}
	if err := k.startBus(ctx); err != nil {
		return err
	}
	return k.startKeyring(ctx)
}

// busConfiguration is the private bus configuration. It lists no
// <servicedir> and no <standard_session_servicedirs/>, so the bus activates
// no service (D1-10). The listen address escapes the socket path as a D-Bus
// address value, and the configuration escapes that address as XML text.
func busConfiguration(socket string) string {
	var listen bytes.Buffer
	_ = xml.EscapeText(&listen, []byte("unix:path="+escapeAddressValue(socket)))
	return `<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>session</type>
  <listen>` + listen.String() + `</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow send_destination="*" eavesdrop="true"/>
    <allow eavesdrop="true"/>
    <allow own="*"/>
  </policy>
</busconfig>
`
}

// escapeAddressValue escapes a value of a D-Bus address: each byte outside
// [-0-9A-Za-z_/.\\*] becomes %XX (D-Bus specification, "Server Addresses").
func escapeAddressValue(value string) string {
	var escaped strings.Builder
	for _, b := range []byte(value) {
		switch {
		case b >= '0' && b <= '9', b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z',
			b == '-', b == '_', b == '/', b == '.', b == '\\', b == '*':
			escaped.WriteByte(b)
		default:
			fmt.Fprintf(&escaped, "%%%02x", b)
		}
	}
	return escaped.String()
}

// addressSocketPath returns the decoded path of a unix:path= address, up to
// its first key separator. It reports false for any other address.
func addressSocketPath(address string) (string, bool) {
	value, ok := strings.CutPrefix(address, "unix:path=")
	if !ok {
		return "", false
	}
	value, _, _ = strings.Cut(value, ",")
	var path []byte
	for index := 0; index < len(value); index++ {
		if value[index] != '%' {
			path = append(path, value[index])
			continue
		}
		if index+2 >= len(value) {
			return "", false
		}
		var b byte
		if _, err := fmt.Sscanf(value[index+1:index+3], "%02x", &b); err != nil {
			return "", false
		}
		path = append(path, b)
		index += 2
	}
	return string(path), true
}

// startBus starts dbus-daemon and waits for its address line, the one line
// that it prints after it listens. The socket is in the 0700 keyring
// directory.
func (k *Keyring) startBus(ctx context.Context) error {
	socket := filepath.Join(k.dir, "bus")
	if len(socket) > socketLimit {
		return fmt.Errorf("keyring: the bus socket path has %d bytes, more than %d: use a shorter job directory", len(socket), socketLimit)
	}
	config := filepath.Join(k.dir, "bus.conf")
	if err := writeNewFile(config, []byte(busConfiguration(socket))); err != nil {
		return err
	}
	read, write, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("keyring: open the bus address pipe: %w", err)
	}
	k.busOutput = read
	cmd := exec.Command(busDaemon, "--config-file="+config, "--nofork", "--nopidfile", "--print-address=1")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "HOME=" + filepath.Join(k.dir, "home")}
	cmd.Stdout = write
	k.bus, err = startDaemon("dbus-daemon", cmd, filepath.Join(k.dir, "bus.log"))
	_ = write.Close()
	if err != nil {
		return err
	}
	line := make(chan string, 1)
	go func() {
		text, _ := bufio.NewReader(io.LimitReader(read, 4096)).ReadString('\n')
		line <- strings.TrimSpace(text)
	}()
	select {
	case k.address = <-line:
	case <-k.bus.done:
		return fmt.Errorf("keyring: dbus-daemon exited before it printed its address: %s", k.bus.logTail())
	case <-ctx.Done():
		return fmt.Errorf("keyring: dbus-daemon printed no address within the start limit %s", k.limits.Start)
	}
	if path, ok := addressSocketPath(k.address); !ok || path != socket {
		return fmt.Errorf("keyring: dbus-daemon did not print the address of its private socket: %s", k.bus.logTail())
	}
	return nil
}

// ownerSignals receives the owner changes of org.freedesktop.secrets that the
// bus itself sends. It keeps only the first new owner.
type ownerSignals chan string

func (owners ownerSignals) DeliverSignal(iface, member string, signal *dbus.Signal) {
	if iface != busName || member != "NameOwnerChanged" || signal.Sender != busName || len(signal.Body) != 3 {
		return
	}
	name, _ := signal.Body[0].(string)
	owner, _ := signal.Body[2].(string)
	if name != secretsName || owner == "" {
		return
	}
	select {
	case owners <- owner:
	default:
	}
}

// startKeyring subscribes to the owner changes of org.freedesktop.secrets,
// starts gnome-keyring-daemon with the unlock password on stdin, waits once
// for the first owner change, and checks that the new owner is that daemon
// (D1-11).
func (k *Keyring) startKeyring(ctx context.Context) error {
	owners := make(ownerSignals, 1)
	conn, err := dbus.Connect(k.address, dbus.WithContext(ctx), dbus.WithSignalHandler(owners))
	if err != nil {
		return fmt.Errorf("keyring: connect to the private bus: %w", err)
	}
	defer conn.Close()
	if err := conn.AddMatchSignalContext(ctx, dbus.WithMatchSender(busName), dbus.WithMatchInterface(busName),
		dbus.WithMatchMember("NameOwnerChanged"), dbus.WithMatchArg(0, secretsName)); err != nil {
		return fmt.Errorf("keyring: subscribe to the owner of %s: %w", secretsName, err)
	}
	bus := conn.BusObject()
	var owned bool
	if err := bus.CallWithContext(ctx, busName+".NameHasOwner", 0, secretsName).Store(&owned); err != nil {
		return fmt.Errorf("keyring: ask the bus for the owner of %s: %w", secretsName, err)
	}
	if owned {
		return fmt.Errorf("keyring: %s has an owner before the keyring daemon starts", secretsName)
	}
	if k.beforeKeyring != nil {
		if err := k.beforeKeyring(k.address); err != nil {
			return err
		}
	}
	if err := k.startKeyringDaemon(); err != nil {
		return err
	}
	var owner string
	select {
	case owner = <-owners:
	case <-k.keyring.done:
		return fmt.Errorf("keyring: gnome-keyring-daemon exited before it owned %s: %s", secretsName, k.keyring.logTail())
	case <-ctx.Done():
		return fmt.Errorf("keyring: %s got no owner within the start limit %s: %s", secretsName, k.limits.Start, k.keyring.logTail())
	}
	var pid uint32
	if err := bus.CallWithContext(ctx, busName+".GetConnectionUnixProcessID", 0, owner).Store(&pid); err != nil {
		return fmt.Errorf("keyring: ask the bus for the process of the %s owner: %w", secretsName, err)
	}
	if want := k.keyring.cmd.Process.Pid; int64(pid) != int64(want) {
		return fmt.Errorf("keyring: the owner of %s is process %d, not the keyring daemon that the harness started (process %d)", secretsName, pid, want)
	}
	var current string
	if err := bus.CallWithContext(ctx, busName+".GetNameOwner", 0, secretsName).Store(&current); err != nil {
		return fmt.Errorf("keyring: ask the bus for the current owner of %s: %w", secretsName, err)
	}
	if current != owner {
		return fmt.Errorf("keyring: the owner of %s changed during the owner check", secretsName)
	}
	return nil
}

// startKeyringDaemon starts gnome-keyring-daemon. The random unlock password
// goes only to the stdin pipe of the daemon, and the harness keeps no copy.
func (k *Keyring) startKeyringDaemon() error {
	secret := make([]byte, 30)
	if _, err := rand.Read(secret); err != nil {
		return fmt.Errorf("keyring: generate the unlock password: %w", err)
	}
	password := []byte(base64.RawURLEncoding.EncodeToString(secret))
	clear(secret)
	defer clear(password)
	read, write, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("keyring: open the unlock pipe: %w", err)
	}
	cmd := exec.Command(keyringDaemon, "--foreground", "--unlock", "--components=secrets")
	cmd.Env = []string{
		"PATH=/usr/bin:/bin", "LANG=C",
		"HOME=" + filepath.Join(k.dir, "home"),
		"XDG_DATA_HOME=" + filepath.Join(k.dir, "data"),
		"XDG_RUNTIME_DIR=" + filepath.Join(k.dir, "run"),
		"XDG_CONFIG_HOME=" + filepath.Join(k.dir, "config"),
		"DBUS_SESSION_BUS_ADDRESS=" + k.address,
	}
	cmd.Stdin = read
	k.keyring, err = startDaemon("gnome-keyring-daemon", cmd, filepath.Join(k.dir, "keyring.log"))
	_ = read.Close()
	if err != nil {
		_ = write.Close()
		return err
	}
	_, writeErr := write.Write(password)
	if err := errors.Join(writeErr, write.Close()); err != nil {
		return fmt.Errorf("keyring: give the unlock password to gnome-keyring-daemon: %w", err)
	}
	return nil
}

// Environment is the only environment entry that a pm process gets for the
// keyring: the private bus address.
func (k *Keyring) Environment() []string {
	return []string{"DBUS_SESSION_BUS_ADDRESS=" + k.address}
}

// KeyringPID is the process ID of the keyring daemon that owns
// org.freedesktop.secrets.
func (k *Keyring) KeyringPID() int {
	return k.keyring.cmd.Process.Pid
}

// Stop stops the keyring daemon and the bus, removes the keyring directory,
// and checks that it is gone. It reports each cleanup failure: a daemon that
// exited before the stop, a daemon that needed SIGKILL or did not exit, a
// process that was left in a daemon process group, and a directory that is
// still there. It acts once. A later call returns the first result, so a
// fallback cleanup after an explicit Stop stops nothing twice.
func (k *Keyring) Stop() error {
	if k == nil {
		return nil
	}
	k.stopOnce.Do(func() { k.stopErr = k.stop() })
	return k.stopErr
}

func (k *Keyring) stop() error {
	errs := []error{k.keyring.stop(k.limits, k.ready), k.bus.stop(k.limits, k.ready)}
	if k.busOutput != nil {
		errs = append(errs, k.busOutput.Close())
	}
	if err := os.RemoveAll(k.dir); err != nil {
		errs = append(errs, fmt.Errorf("keyring: remove the keyring directory: %w", err))
	} else if _, err := os.Lstat(k.dir); !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, errors.New("keyring: the keyring directory is still there after its removal"))
	}
	return errors.Join(errs...)
}

// startDaemon starts cmd in its own process group with stdout, when not set,
// and stderr in a new log file.
func startDaemon(name string, cmd *exec.Cmd, logPath string) (*daemon, error) {
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("keyring: create the %s log: %w", name, err)
	}
	if cmd.Stdout == nil {
		cmd.Stdout = log
	}
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		return nil, fmt.Errorf("keyring: start %s: %w", name, err)
	}
	d := &daemon{name: name, cmd: cmd, done: make(chan struct{}), log: logPath}
	go func() {
		_ = cmd.Wait()
		_ = log.Close()
		close(d.done)
	}()
	return d, nil
}

// stop runs the stop sequence once. A later call returns the first result. A
// daemon that exited before the stop is an error when the keyring was ready.
func (d *daemon) stop(limits Limits, ready bool) error {
	if d == nil {
		return nil
	}
	d.stopOnce.Do(func() { d.stopErr = d.stopSequence(limits, ready) })
	return d.stopErr
}

func (d *daemon) stopSequence(limits Limits, ready bool) error {
	select {
	case <-d.done:
		if ready {
			return errors.Join(fmt.Errorf("keyring: %s exited before the stop: %s", d.name, d.cmd.ProcessState), d.killSurvivors())
		}
		return d.killSurvivors()
	default:
	}
	group := -d.cmd.Process.Pid
	_ = syscall.Kill(group, syscall.SIGTERM)
	stopTimer := time.NewTimer(limits.Stop)
	defer stopTimer.Stop()
	select {
	case <-d.done:
		return d.killSurvivors()
	case <-stopTimer.C:
	}
	_ = syscall.Kill(group, syscall.SIGKILL)
	killTimer := time.NewTimer(limits.Kill)
	defer killTimer.Stop()
	select {
	case <-d.done:
		return errors.Join(fmt.Errorf("keyring: %s did not exit within %s of SIGTERM and needed SIGKILL", d.name, limits.Stop), d.killSurvivors())
	case <-killTimer.C:
		return fmt.Errorf("keyring: %s (process %d) did not exit within %s of SIGKILL", d.name, d.cmd.Process.Pid, limits.Kill)
	}
}

// killSurvivors runs after the leader of the daemon process group exited. It
// makes one observation, with no wait. When a process of the group is left,
// it sends SIGKILL to the whole group and reports that process. A process
// group keeps its ID while one of its processes runs, so the signal reaches
// only that group.
func (d *daemon) killSurvivors() error {
	group := -d.cmd.Process.Pid
	if err := syscall.Kill(group, 0); errors.Is(err, syscall.ESRCH) {
		return nil
	}
	_ = syscall.Kill(group, syscall.SIGKILL)
	return fmt.Errorf("keyring: a process of the %s process group was still there after its leader exited, and the stop sent SIGKILL to the group", d.name)
}

// logTail returns at most the last logLimit bytes of the daemon log, for a
// start error. It reads only those bytes, not the whole log.
func (d *daemon) logTail() string {
	file, err := os.Open(d.log)
	if err != nil {
		return "(no log)"
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "(no log)"
	}
	offset := max(info.Size()-logLimit, 0)
	contents, err := io.ReadAll(io.NewSectionReader(file, offset, logLimit))
	if err != nil {
		return "(no log)"
	}
	return fmt.Sprintf("%s log: %q", d.name, contents)
}

func writeNewFile(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("keyring: create %s: %w", filepath.Base(path), err)
	}
	_, writeErr := file.Write(contents)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return fmt.Errorf("keyring: write %s: %w", filepath.Base(path), err)
	}
	return nil
}
