package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
)

// keyring is one private Secret Service for the job: a private D-Bus session
// bus on a unix socket in a job-owned 0700 directory and a gnome-keyring
// daemon whose login keyring is unlocked with a random password that only this
// process and the daemon's stdin pipe hold.
type keyring struct {
	dir       string
	address   string
	busCmd    *exec.Cmd
	keyCmd    *exec.Cmd
	busDone   chan error
	keyDone   chan error
	password  string
	dataDir   string
	runtime   string
	homeDir   string
	installed bool
}

func keyringInstallFacts() bool {
	status, err := os.ReadFile("/opt/pmcli/login-spike-apt.status")
	installed := err == nil && strings.HasPrefix(strings.TrimSpace(string(status)), "installed")
	report.check("1a.install.image", installed, "dbus and gnome-keyring are in the spike CI image from the pinned snapshot (status %q)", strings.TrimSpace(string(status)))
	for _, path := range []string{"/usr/bin/dbus-launch", "/usr/share/dbus-1/services/org.freedesktop.secrets.service", "/usr/bin/systemctl"} {
		_, statErr := os.Stat(path)
		report.INFO("1a.install.files", "%s present: %t", path, statErr == nil)
	}
	if log, err := os.ReadFile("/opt/pmcli/login-spike-apt.log"); err == nil {
		packages := 0
		for _, line := range strings.Split(string(log), "\n") {
			if strings.HasPrefix(line, "Setting up ") {
				packages++
			}
			if strings.Contains(line, "additional disk space") || strings.Contains(line, "newly installed") {
				report.INFO("1a.install.size", "%s", strings.TrimSpace(line))
			}
		}
		report.INFO("1a.install.size", "packages set up: %d", packages)
		if !installed {
			report.tail("1a.install.log", "/opt/pmcli/login-spike-apt.log", 25)
		}
	}
	for _, path := range []string{"/usr/bin/dbus-daemon", "/usr/bin/gnome-keyring-daemon"} {
		information, err := os.Stat(path)
		if !report.check("1a.install.binary", err == nil, "%s is present", path) {
			return false
		}
		if path == "/usr/bin/gnome-keyring-daemon" {
			report.check("1a.install.nosetuid", information.Mode()&os.ModeSetuid == 0, "gnome-keyring-daemon has no setuid bit")
			report.check("1a.install.nocaps", !hasFileCapability(path), "gnome-keyring-daemon has no file capability")
		}
	}
	return installed
}

func startKeyring(parent string) (*keyring, error) {
	k := &keyring{dir: filepath.Join(parent, "keyring")}
	k.dataDir = filepath.Join(k.dir, "data")
	k.runtime = filepath.Join(k.dir, "run")
	k.homeDir = filepath.Join(k.dir, "home")
	for _, directory := range []string{k.dir, k.dataDir, k.runtime, k.homeDir, filepath.Join(k.dir, "bus")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return k, errors.New("create the keyring directories")
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return k, errors.New("set the keyring directory mode")
		}
	}
	socket := filepath.Join(k.dir, "bus", "socket")
	config := `<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>session</type>
  <listen>unix:path=` + socket + `</listen>
  <auth>EXTERNAL</auth>
  <policy context="default">
    <allow send_destination="*" eavesdrop="true"/>
    <allow eavesdrop="true"/>
    <allow own="*"/>
  </policy>
</busconfig>
`
	configPath := filepath.Join(k.dir, "bus.conf")
	if err := writePrivateFile(configPath, []byte(config)); err != nil {
		return k, err
	}
	busLog, err := os.OpenFile(filepath.Join(k.dir, "bus.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return k, errors.New("create the bus log")
	}
	k.busCmd = exec.Command("/usr/bin/dbus-daemon", "--config-file="+configPath, "--nofork", "--nopidfile", "--print-address=1")
	k.busCmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + k.homeDir, "LANG=C"}
	k.busCmd.Stderr = busLog
	k.busCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := k.busCmd.StdoutPipe()
	if err != nil {
		return k, errors.New("open the bus address pipe")
	}
	start := time.Now()
	if err := k.busCmd.Start(); err != nil {
		return k, errors.New("start dbus-daemon")
	}
	k.busDone = make(chan error, 1)
	addressLine := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		addressLine <- strings.TrimSpace(line)
		_, _ = io.Copy(io.Discard, stdout)
		k.busDone <- k.busCmd.Wait()
		_ = busLog.Close()
	}()
	select {
	case k.address = <-addressLine:
	case <-time.After(15 * time.Second):
		return k, errors.New("dbus-daemon printed no address within 15s")
	}
	if !strings.HasPrefix(k.address, "unix:path="+socket) {
		return k, fmt.Errorf("dbus-daemon address is not the private socket")
	}
	report.PASS("1a.bus.start", "private session bus ready in %s on a unix socket in a 0700 job directory", elapsed(start))
	if information, err := os.Stat(filepath.Join(k.dir, "bus")); err == nil {
		report.check("1a.bus.mode", information.Mode().Perm() == 0o700, "the bus socket directory mode is %o", information.Mode().Perm())
	}

	// Subscribe before the keyring starts, so the owner change is one event.
	conn, err := dbus.Connect(k.address)
	if err != nil {
		return k, errors.New("connect to the private bus")
	}
	defer conn.Close()
	if err := conn.AddMatchSignal(
		dbus.WithMatchInterface("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchArg(0, "org.freedesktop.secrets"),
	); err != nil {
		return k, errors.New("subscribe to the secrets name owner")
	}
	signals := make(chan *dbus.Signal, 8)
	conn.Signal(signals)

	k.password = randomToken(40)
	report.secret(k.password)
	keyLog, err := os.OpenFile(filepath.Join(k.dir, "keyring.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return k, errors.New("create the keyring log")
	}
	k.keyCmd = exec.Command("/usr/bin/gnome-keyring-daemon", "--foreground", "--unlock", "--components=secrets")
	k.keyCmd.Env = k.environment()
	k.keyCmd.Stdout = keyLog
	k.keyCmd.Stderr = keyLog
	k.keyCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := k.keyCmd.StdinPipe()
	if err != nil {
		return k, errors.New("open the keyring unlock pipe")
	}
	start = time.Now()
	if err := k.keyCmd.Start(); err != nil {
		return k, errors.New("start gnome-keyring-daemon")
	}
	k.keyDone = make(chan error, 1)
	go func() {
		k.keyDone <- k.keyCmd.Wait()
		_ = keyLog.Close()
	}()
	_, _ = io.WriteString(stdin, k.password)
	_ = stdin.Close()
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	for {
		select {
		case signal := <-signals:
			if len(signal.Body) == 3 {
				if owner, _ := signal.Body[2].(string); owner != "" {
					report.PASS("1a.keyring.start", "gnome-keyring-daemon owns org.freedesktop.secrets after %s (one owner-change event)", elapsed(start))
					return k, nil
				}
			}
		case err := <-k.keyDone:
			k.keyDone <- err
			report.tail("1a.keyring.log", filepath.Join(k.dir, "keyring.log"), 20)
			return k, errors.New("gnome-keyring-daemon exited before it owned the secrets name")
		case <-timer.C:
			report.tail("1a.keyring.log", filepath.Join(k.dir, "keyring.log"), 20)
			return k, errors.New("no secrets name owner within 20s")
		}
	}
}

// environment is the environment of the keyring daemon and of the pm
// processes that may use the os session store. Other processes never get the
// bus address.
func (k *keyring) environment() []string {
	return []string{
		"PATH=/usr/bin:/bin", "LANG=C", "HOME=" + k.homeDir,
		"XDG_DATA_HOME=" + k.dataDir, "XDG_RUNTIME_DIR=" + k.runtime,
		"XDG_CONFIG_HOME=" + filepath.Join(k.homeDir, "config"),
		"DBUS_SESSION_BUS_ADDRESS=" + k.address,
	}
}

// roundTrip stores a random PM session through the product's os session store
// (internal/api, zalando/go-keyring) in one process and loads it in another.
func (k *keyring) roundTrip(self string) {
	credential := "pm_session_" + randomToken(48)
	report.secret(credential)
	want := sha256.Sum256([]byte(credential))
	storeCmd := exec.Command(self, "keyring-store")
	storeCmd.Env = k.environment()
	storeCmd.Stdin = strings.NewReader(credential)
	output, err := storeCmd.CombinedOutput()
	report.check("1a.osstore.store", err == nil, "process 1 stored a PM session in the os store (%s)", report.scrub(strings.TrimSpace(string(output))))
	loadCmd := exec.Command(self, "keyring-load")
	loadCmd.Env = k.environment()
	output, err = loadCmd.CombinedOutput()
	got := strings.TrimSpace(string(output))
	report.check("1a.osstore.load", err == nil && got == hex.EncodeToString(want[:]), "process 2 loaded the same session from the os store (digest match: %t)", got == hex.EncodeToString(want[:]))
	// Run 1 (dbus-x11 image) took 240 s here. The likely cause: with no bus
	// address, godbus runs dbus-launch, which starts a second session bus, and
	// D-Bus activation starts a second keyring daemon that cannot unlock. The
	// check is timed and bounded, and a stray-process check follows at stop.
	noBusCtx, noBusCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer noBusCancel()
	noBus := exec.CommandContext(noBusCtx, self, "keyring-load")
	noBus.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "HOME=" + k.homeDir, "XDG_RUNTIME_DIR=" + k.runtime}
	noBus.WaitDelay = 5 * time.Second
	noBusStart := time.Now()
	output, err = noBus.CombinedOutput()
	report.check("1a.osstore.nobus", err != nil && noBusCtx.Err() == nil, "a process without the private bus address fails at once and cannot load it (%s after %s)", report.scrub(strings.TrimSpace(string(output))), elapsed(noBusStart))
	hits := scanTree(k.dataDir, []byte(credential))
	report.check("1a.keyring.atrest", hits == 0, "no keyring file holds the session in plain or base64 form (files with hits: %d)", hits)
	files := []string{}
	_ = filepath.WalkDir(k.dataDir, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			information, _ := entry.Info()
			relative, _ := filepath.Rel(k.dataDir, path)
			files = append(files, fmt.Sprintf("%s(%o)", relative, information.Mode().Perm()))
		}
		return nil
	})
	report.INFO("1a.keyring.files", "keyring data files: %s", strings.Join(files, " "))
}

func (k *keyring) stop() {
	for _, item := range []struct {
		command *exec.Cmd
		done    chan error
		name    string
	}{{k.keyCmd, k.keyDone, "gnome-keyring-daemon"}, {k.busCmd, k.busDone, "dbus-daemon"}} {
		if item.command == nil || item.command.Process == nil || item.done == nil {
			continue
		}
		_ = syscall.Kill(-item.command.Process.Pid, syscall.SIGTERM)
		select {
		case <-item.done:
			report.PASS("1a.cleanup.process", "%s stopped", item.name)
		case <-time.After(10 * time.Second):
			_ = syscall.Kill(-item.command.Process.Pid, syscall.SIGKILL)
			<-item.done
			report.FAIL("1a.cleanup.process", "%s needed SIGKILL", item.name)
		}
	}
	k.password = ""
	stray := strayBusProcesses()
	report.check("1a.cleanup.stray", stray == 0, "no other dbus-daemon, dbus-launch or gnome-keyring-daemon process is left in the job container (found %d)", stray)
	if err := os.RemoveAll(k.dir); err != nil {
		report.FAIL("1a.cleanup.data", "keyring directory removal failed")
		return
	}
	_, err := os.Stat(k.dir)
	report.check("1a.cleanup.data", errors.Is(err, os.ErrNotExist), "the keyring data directory is removed")
}

// keyringChild runs in a separate process: "keyring-store" reads a credential
// from stdin and stores it; "keyring-load" loads it and prints its digest.
func keyringChild(mode string) int {
	store, err := pmapi.NewSessionStore(pmapi.SessionStoreOS)
	if err != nil {
		fmt.Println("os store unavailable")
		return 1
	}
	key, err := pmapi.NewSessionKey("pm-login-spike-instance", "login-spike")
	if err != nil {
		fmt.Println("session key invalid")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	switch mode {
	case "keyring-store":
		credential, _ := io.ReadAll(io.LimitReader(os.Stdin, 4096))
		session := pmapi.PMDeviceSession{
			ExpiresAt:           time.Now().Add(time.Hour).UTC().Truncate(time.Second),
			PmSessionCredential: string(credential),
			Principal:           pmapi.Principal{Kind: "user", PrincipalId: "user-login-spike"},
			SessionId:           "session-login-spike",
		}
		if err := store.Store(ctx, key, session); err != nil {
			fmt.Printf("store failed: %v\n", err)
			return 1
		}
		fmt.Println("stored")
		return 0
	default:
		session, err := store.Load(ctx, key)
		if err != nil {
			fmt.Printf("load failed: %v\n", err)
			return 1
		}
		sum := sha256.Sum256([]byte(session.PmSessionCredential))
		fmt.Println(hex.EncodeToString(sum[:]))
		return 0
	}
}

// scanTree counts the files under root that hold value in a likely encoding.
func scanTree(root string, value []byte) int {
	hits := 0
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		contents, readErr := os.ReadFile(path)
		if readErr == nil && containsAny(contents, value) {
			hits++
		}
		return nil
	})
	return hits
}

// scanProcesses counts the visible processes whose command line or
// environment holds value.
func scanProcesses(value []byte) (int, int) {
	inArgs, inEnv := 0, 0
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		if _, err := fmt.Sscanf(entry.Name(), "%d", new(int)); err != nil {
			continue
		}
		if contents, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline")); err == nil && bytes.Contains(contents, value) {
			inArgs++
		}
		if contents, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "environ")); err == nil && bytes.Contains(contents, value) {
			inEnv++
		}
	}
	return inArgs, inEnv
}

// strayBusProcesses counts the visible bus and keyring processes. It runs after
// the spike stopped its own, so every one found is a stray.
func strayBusProcesses() int {
	count := 0
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		if _, err := fmt.Sscanf(entry.Name(), "%d", new(int)); err != nil {
			continue
		}
		name, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "comm"))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(string(name)) {
		case "dbus-daemon", "dbus-launch", "gnome-keyring-d":
			count++
		}
	}
	return count
}
