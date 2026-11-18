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
	// owner leads the process group of the daemon, and ownerInput is the
	// write end of its stdin pipe (startGroupOwner).
	owner      *exec.Cmd
	ownerInput io.WriteCloser
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
	k.bus, err = startDaemon(ctx, "dbus-daemon", cmd, filepath.Join(k.dir, "bus.log"))
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
	if err := k.startKeyringDaemon(ctx); err != nil {
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
func (k *Keyring) startKeyringDaemon(ctx context.Context) error {
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
	k.keyring, err = startDaemon(ctx, "gnome-keyring-daemon", cmd, filepath.Join(k.dir, "keyring.log"))
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

// groupOwnerReadyLimit bounds the one wait for the ready byte of a group
// owner.
const groupOwnerReadyLimit = 10 * time.Second

// signalGroup sends sig to the process group pgid. A developer check replaces
// it.
var signalGroup = func(pgid int, sig syscall.Signal) error { return syscall.Kill(-pgid, sig) }

// groupOwnerScript is the shell script of a group owner. A developer check
// replaces it.
var groupOwnerScript = `trap "" TERM; printf r; exec cat > /dev/null`

// newPipe makes one pipe. A developer check replaces it.
var newPipe = os.Pipe

// startGroupOwner starts the owner of the process group of one daemon. The
// owner leads a new process group, ignores SIGTERM, writes one ready byte and
// then reads its stdin pipe until end of file. Only this process holds that
// pipe, so the owner exits when the harness ends. The group ID is the process
// ID of the owner. The kernel keeps that ID while the owner runs and, after
// it exits, until the harness reaps it. The owner ignores SIGTERM because the
// stop sends SIGTERM to the whole group first. The ready byte shows that the
// owner ignores SIGTERM before any signal can come. An ended start context
// refuses the start. Each failure path closes every pipe end.
func startGroupOwner(ctx context.Context) (*exec.Cmd, io.WriteCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("the start limit ended before the group owner started: %w", err)
	}
	inRead, inWrite, err := newPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("open the stdin pipe of the group owner: %w", err)
	}
	outRead, outWrite, err := newPipe()
	if err != nil {
		_ = inRead.Close()
		_ = inWrite.Close()
		return nil, nil, fmt.Errorf("open the stdout pipe of the group owner: %w", err)
	}
	owner := exec.Command("/bin/sh", "-c", groupOwnerScript)
	owner.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	owner.Stdin = inRead
	owner.Stdout = outWrite
	startErr := owner.Start()
	// After the start, the owner holds its own copies of these two ends.
	_ = inRead.Close()
	_ = outWrite.Close()
	if startErr != nil {
		_ = inWrite.Close()
		_ = outRead.Close()
		return nil, nil, startErr
	}
	readyErr := awaitOwnerReady(ctx, outRead)
	_ = outRead.Close()
	if readyErr != nil {
		return nil, nil, errors.Join(readyErr, killGroup(owner.Process.Pid), endGroupOwner(owner, inWrite))
	}
	return owner, inWrite, nil
}

// awaitOwnerReady reads the one ready byte of a group owner. The read ends at
// the earlier of the deadline of ctx and groupOwnerReadyLimit, and at once
// when ctx ends.
func awaitOwnerReady(ctx context.Context, output *os.File) error {
	deadline := time.Now().Add(groupOwnerReadyLimit)
	startDeadline := false
	if start, ok := ctx.Deadline(); ok && start.Before(deadline) {
		deadline, startDeadline = start, true
	}
	if err := output.SetReadDeadline(deadline); err != nil {
		return fmt.Errorf("the group owner sent no ready byte: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = output.SetReadDeadline(time.Now()) })
	defer stop()
	ready := make([]byte, 1)
	n, err := output.Read(ready)
	if n == 1 && ready[0] == 'r' {
		return nil
	}
	if err == nil {
		err = fmt.Errorf("the byte %q", ready[0])
	}
	cause := ctx.Err()
	if cause == nil && startDeadline && errors.Is(err, os.ErrDeadlineExceeded) {
		// The read deadline was the start deadline. The context can end a
		// moment after the read does.
		cause = context.DeadlineExceeded
	}
	return fmt.Errorf("the group owner sent no ready byte: %w", errors.Join(err, cause))
}

// killGroup sends SIGKILL to the process group pgid. ESRCH means that no
// process of the group is left. Each other error is returned.
func killGroup(pgid int) error {
	if err := signalGroup(pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("keyring: send SIGKILL to the process group: %w", err)
	}
	return nil
}

// groupOwnerEndLimit bounds the wait for the exit of a group owner after the
// final group signal. A developer check sets a shorter value.
var groupOwnerEndLimit = 10 * time.Second

// endGroupOwner closes the stdin pipe of the owner and waits for its exit, at
// most groupOwnerEndLimit. One goroutine reaps the owner. It keeps waiting
// after the limit, so the owner is reaped when it exits. When the limit ends
// first, the owner is not reaped, and no code signals its group after that
// point. The exit by the final SIGKILL is expected. Each other exit is an
// error: the owner exited before the final signal, or the final signal did
// not reach it.
func endGroupOwner(owner *exec.Cmd, input io.Closer) error {
	_ = input.Close()
	result := make(chan error, 1)
	go func() { result <- owner.Wait() }()
	limit := time.NewTimer(groupOwnerEndLimit)
	defer limit.Stop()
	var err error
	select {
	case err = <-result:
	case <-limit.C:
		return fmt.Errorf("keyring: the process group owner did not exit within %s of the final signal, so it is not reaped", groupOwnerEndLimit)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() && status.Signal() == syscall.SIGKILL {
			return nil
		}
	}
	if err == nil {
		err = errors.New("exit status 0")
	}
	return fmt.Errorf("keyring: the process group owner did not end by the final signal: %w", err)
}

// startDaemon starts cmd in the process group of a new group owner, with
// stdout, when not set, and stderr in a new log file. A failed owner start,
// or a failed join of its group, is an error, and the daemon does not start.
// ctx is the shared start context: when it ends, the daemon does not start.
func startDaemon(ctx context.Context, name string, cmd *exec.Cmd, logPath string) (*daemon, error) {
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("keyring: create the %s log: %w", name, err)
	}
	owner, ownerInput, err := startGroupOwner(ctx)
	if err != nil {
		_ = log.Close()
		return nil, fmt.Errorf("keyring: start the process group owner of %s: %w", name, err)
	}
	if err := ctx.Err(); err != nil {
		_ = log.Close()
		return nil, errors.Join(fmt.Errorf("keyring: the start limit ended before %s started: %w", name, err), killGroup(owner.Process.Pid), endGroupOwner(owner, ownerInput))
	}
	if cmd.Stdout == nil {
		cmd.Stdout = log
	}
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: owner.Process.Pid}
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		return nil, errors.Join(fmt.Errorf("keyring: start %s: %w", name, err), killGroup(owner.Process.Pid), endGroupOwner(owner, ownerInput))
	}
	d := &daemon{name: name, cmd: cmd, done: make(chan struct{}), log: logPath, owner: owner, ownerInput: ownerInput}
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

// stopSequence sends SIGTERM to the process group, then SIGKILL, each with one
// limit. The owner ignores SIGTERM. When the leader exits, endGroup ends the
// group. When the stop must send SIGKILL, that SIGKILL is the final group
// signal: it reaches every process of the group, the owner too.
func (d *daemon) stopSequence(limits Limits, ready bool) error {
	select {
	case <-d.done:
		if ready {
			return errors.Join(fmt.Errorf("keyring: %s exited before the stop: %s", d.name, d.cmd.ProcessState), d.endGroup())
		}
		return d.endGroup()
	default:
	}
	group := d.owner.Process.Pid
	_ = signalGroup(group, syscall.SIGTERM)
	stopTimer := time.NewTimer(limits.Stop)
	defer stopTimer.Stop()
	select {
	case <-d.done:
		return d.endGroup()
	case <-stopTimer.C:
	}
	killErr := killGroup(group)
	killTimer := time.NewTimer(limits.Kill)
	defer killTimer.Stop()
	select {
	case <-d.done:
		return errors.Join(fmt.Errorf("keyring: %s did not exit within %s of SIGTERM and needed SIGKILL", d.name, limits.Stop), killErr, endGroupOwner(d.owner, d.ownerInput))
	case <-killTimer.C:
		return errors.Join(fmt.Errorf("keyring: %s (process %d) did not exit within %s of SIGKILL", d.name, d.cmd.Process.Pid, limits.Kill), killErr, endGroupOwner(d.owner, d.ownerInput))
	}
}

// endGroup runs after the leader of the daemon exited and was reaped. The
// owner is alive. It makes one observation of the live processes of the
// group other than the owner, with no wait. When a process is left, it
// reports that process. Then it sends the final SIGKILL to the whole group
// and reaps the owner. Because the owner is not reaped before that signal,
// the group ID still belongs to this daemon. A process that left the group
// (setsid or setpgid) is not seen and gets no signal.
func (d *daemon) endGroup() error {
	group := d.owner.Process.Pid
	var survivors error
	count, err := liveGroupMembers(group)
	switch {
	case err != nil:
		survivors = fmt.Errorf("keyring: observe the %s process group: %w", d.name, err)
	case count > 0:
		survivors = fmt.Errorf("keyring: a process of the %s process group was still there after its leader exited, and the stop sent SIGKILL to the group", d.name)
	}
	return errors.Join(survivors, killGroup(group), endGroupOwner(d.owner, d.ownerInput))
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
