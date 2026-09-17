package credentials

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestOperatorSecretStartupBoundary(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, "fixture", "wrapping")
	if err := os.MkdirAll(bundle, 0700); err != nil {
		t.Fatal(err)
	}
	secrets, err := OpenOperatorSecrets(root)
	if err != nil {
		t.Fatal(err)
	}
	defer secrets.Close()

	t.Run("exact binary key bytes", func(t *testing.T) {
		key := []byte{0, 1, 255, ' ', '\n', 0, 128}
		if err := os.WriteFile(filepath.Join(bundle, "key"), key, 0600); err != nil {
			t.Fatal(err)
		}
		actual, err := secrets.Read("secret://fixture/wrapping", "key")
		if err != nil || !bytes.Equal(actual, key) {
			t.Fatalf("binary secret bytes were changed: %v", err)
		}
		clear(actual)
	})

	t.Run("outside symlink cannot resolve a secret", func(t *testing.T) {
		outside := filepath.Join(t.TempDir(), "outside-key")
		if err := os.WriteFile(outside, []byte("synthetic-outside-key"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(bundle, "escape")); err != nil {
			t.Fatal(err)
		}
		if value, err := secrets.Read("secret://fixture/wrapping", "escape"); err == nil || len(value) != 0 {
			t.Fatal("out-of-root secret bytes crossed the startup boundary")
		}
	})

	t.Run("nonregular secret cannot block startup", func(t *testing.T) {
		fifo := filepath.Join(bundle, "fifo")
		if err := syscall.Mkfifo(fifo, 0600); err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			_, err := secrets.Read("secret://fixture/wrapping", "fifo")
			result <- err
		}()
		select {
		case err := <-result:
			if err == nil {
				t.Fatal("FIFO was admitted as an operator secret")
			}
		case <-time.After(time.Second):
			// Release a regressed blocking reader before failing the test;
			// the negative case must not strand a goroutine or the suite.
			writer, err := syscall.Open(fifo, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
			if err == nil {
				_ = syscall.Close(writer)
			}
			select {
			case <-result:
			case <-time.After(time.Second):
				t.Fatal("blocked secret reader could not be released")
			}
			t.Fatal("FIFO open blocked process startup")
		}
	})

	t.Run("mounted root identity survives pathname replacement", func(t *testing.T) {
		parent := t.TempDir()
		path := filepath.Join(parent, "mount")
		originalBundle := filepath.Join(path, "fixture", "wrapping")
		if err := os.MkdirAll(originalBundle, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(originalBundle, "key"), []byte("original-owned-key"), 0600); err != nil {
			t.Fatal(err)
		}
		mounted, err := OpenOperatorSecrets(path)
		if err != nil {
			t.Fatal(err)
		}
		defer mounted.Close()
		if err := os.Rename(path, filepath.Join(parent, "retained")); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(originalBundle, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(originalBundle, "key"), []byte("replacement-key"), 0600); err != nil {
			t.Fatal(err)
		}
		value, err := mounted.Read("secret://fixture/wrapping", "key")
		if err != nil || string(value) != "original-owned-key" {
			t.Fatalf("a replaced pathname changed the owned secret root: %v", err)
		}
		clear(value)
	})
}
