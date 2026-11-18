package stack

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Review F4: an unreadable identity provider CA gives fixed text. The cause
// can name a private path, so Error() never formats it. errors.Is still
// reaches it.
func TestIdentityProviderCAErrorHasFixedText(t *testing.T) {
	directory := t.TempDir()
	cause := &fs.PathError{Op: "open", Path: filepath.Join(directory, "ca.pem"), Err: fs.ErrPermission}
	var err error = &identityProviderCAError{cause: cause}
	if text := err.Error(); text != "identity provider CA is unreadable" || strings.Contains(text, directory) {
		t.Fatalf("CA error text = %q, want the fixed text with no path", text)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatal("the CA error hides its cause from errors.Is")
	}

	// The real reader with a CA file that passes Stat but cannot be read.
	// A process of root can read a mode-0 file, so only a non-root user can
	// make this read fail. The fixed-text check above holds for every user.
	if os.Geteuid() == 0 {
		t.Log("running as root: a mode-0 file stays readable, so only the fixed-text check applies")
		return
	}
	if err := os.WriteFile(filepath.Join(directory, "ca.pem"), []byte("synthetic CA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(directory, "ca.pem"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(directory, "ca.pem"), 0o600) })
	environment := map[string]string{
		"PM_STACK_IDENTITY_PROVIDER_HOST":   "127.0.0.2",
		"PM_STACK_IDENTITY_PROVIDER_PORT":   "12006",
		"PM_STACK_IDENTITY_PROVIDER_ISSUER": "https://127.0.0.2:12006/realms/pm-test",
		"PM_STACK_IDENTITY_PROVIDER_DIR":    directory,
	}
	ports := map[string]string{"PM_STACK_POSTGRES_PORT": "12001", "PM_STACK_PUBLIC_PORT": "12005"}
	_, err = identityProviderFromEnvironment(func(name string) string { return environment[name] }, ports)
	if err == nil {
		t.Fatal("an unreadable identity provider CA was accepted")
	}
	if strings.Contains(err.Error(), directory) {
		t.Fatalf("the diagnostic names a private path: %q", err)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("the diagnostic lost its cause: %v", err)
	}
}

// Review F7: the provider start has its own finite deadline. When the
// deadline passes, the start ends with the cause timeout, so the step code
// becomes identity-provider-start/timeout.
func TestIdentityProviderStartHasAFiniteDeadline(t *testing.T) {
	const limit = 50 * time.Millisecond
	err := startWithin(context.Background(), limit, func(startCtx context.Context) error {
		deadline, ok := startCtx.Deadline()
		if !ok {
			return errors.New("the start context has no deadline")
		}
		if remaining := time.Until(deadline); remaining > limit {
			return errors.New("the start deadline is later than its limit")
		}
		// A hanging start: it ends only when its context ends, as
		// exec.CommandContext stops the started process.
		<-startCtx.Done()
		return startCtx.Err()
	})
	if err == nil {
		t.Fatal("a start that did not end passed")
	}
	if step := FailedStep(&stepError{step: "identity-provider-start", err: err}); step != "identity-provider-start/timeout" {
		t.Fatalf("the failed step is %q, want identity-provider-start/timeout (error: %v)", step, err)
	}

	// A start that ends in time keeps its own result.
	if err := startWithin(context.Background(), time.Minute, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("a start that ended in time failed: %v", err)
	}
	failure := errors.New("compose refused the start")
	if err := startWithin(context.Background(), time.Minute, func(context.Context) error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("a failed start lost its error: %v", err)
	}
}

// Review F8: one outer deadline for the whole readiness wait. An attempt that
// passes only after that deadline does not count.
func TestWaitForRefusesASuccessAfterItsDeadline(t *testing.T) {
	err := waitFor(context.Background(), 50*time.Millisecond, func(attempt context.Context) error {
		if _, ok := attempt.Deadline(); !ok {
			return errors.New("the attempt has no deadline")
		}
		// The check answers only after its attempt ended.
		<-attempt.Done()
		return nil
	})
	if err == nil {
		t.Fatal("waitFor accepted a check that passed only after the wait limit")
	}
	if step := FailedStep(&stepError{step: "identity-provider-ready", err: err}); step != "identity-provider-ready/timeout" {
		t.Fatalf("the failed step is %q, want identity-provider-ready/timeout (error: %v)", step, err)
	}

	// A check that passes in time passes.
	if err := waitFor(context.Background(), time.Minute, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("waitFor refused a check that passed in time: %v", err)
	}

	// A cancelled parent ends the wait with its own error.
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitFor(parent, time.Minute, func(context.Context) error { return errors.New("not ready") }); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitFor with a cancelled parent = %v, want context.Canceled", err)
	}
}
