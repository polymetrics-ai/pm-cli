//go:build linux

package keyring

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// These checks run the real dbus-daemon and gnome-keyring-daemon of the CI
// image (slice L-b). Without them, the checks fail. They do not skip.

func linuxLimits() Limits {
	return Limits{Start: 60 * time.Second, Stop: 10 * time.Second, Kill: 10 * time.Second}
}

// shortParent is a parent directory with a short path, because the path of
// t.TempDir can be too long for the bus socket.
func shortParent(t *testing.T) string {
	t.Helper()
	parent, err := os.MkdirTemp("", "kr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	return parent
}

// Defect: a keyring that starts without a checked owner, or a stop that
// leaves the daemons or the directory (D1-10, D1-11).
func TestStartChecksTheSecretsOwner(t *testing.T) {
	k, err := Start(shortParent(t), linuxLimits())
	if err != nil {
		t.Fatal(err)
	}
	dir := k.dir
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	address, ok := strings.CutPrefix(k.Environment()[0], "DBUS_SESSION_BUS_ADDRESS=")
	if !ok || len(k.Environment()) != 1 {
		t.Fatalf("Environment() = %d entries, want only the bus address", len(k.Environment()))
	}
	conn, err := dbus.Connect(address, dbus.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	var owner string
	var pid uint32
	bus := conn.BusObject()
	err = errors.Join(
		bus.CallWithContext(ctx, busName+".GetNameOwner", 0, secretsName).Store(&owner),
		bus.CallWithContext(ctx, busName+".GetConnectionUnixProcessID", 0, owner).Store(&pid),
	)
	_ = conn.Close()
	if err != nil || int64(pid) != int64(k.KeyringPID()) {
		t.Fatalf("the owner of %s is process %d (%v), want the keyring daemon %d", secretsName, pid, err, k.KeyringPID())
	}
	if err := k.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the keyring directory is still there after Stop: %v", err)
	}
}

// Defect: the harness accepts the first owner of org.freedesktop.secrets,
// also a process that is not the keyring daemon that it started (S9).
func TestStartRefusesAForeignSecretsOwner(t *testing.T) {
	parent := shortParent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, err := start(parent, linuxLimits(), func(address string) error {
		// The foreign owner keeps the name until the test ends.
		conn, err := dbus.Connect(address, dbus.WithContext(ctx))
		if err != nil {
			return err
		}
		reply, err := conn.RequestName(secretsName, dbus.NameFlagDoNotQueue)
		if err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
			return errors.Join(errors.New("the foreign owner did not get the name"), err)
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "not the keyring daemon that the harness started") {
		t.Fatalf("start() = %v, want the owner refusal", err)
	}
	entries, readErr := os.ReadDir(parent)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("the refused start left %d entries in its parent (%v)", len(entries), readErr)
	}
}
