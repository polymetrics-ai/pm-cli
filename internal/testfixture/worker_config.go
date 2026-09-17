package testfixture

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/config"
)

const workerConfigurationLimit = 2 << 20

var testWorkerControls = json.RawMessage(`{
  "task_queue": "pm-control",
  "dial_timeout": "5s",
  "drain_timeout": "5s",
  "max_concurrent_activities": 32,
  "max_concurrent_workflow_tasks": 4,
  "activity_timeouts": {
    "schedule_to_close": "20s",
    "start_to_close": "10s",
    "heartbeat": "2s",
    "transport": "5s"
  }
}`)

// WorkerConfigurationArtifact is one explicitly test-only worker configuration
// derived from the selected fixture configuration. Path remains inside the
// owned fixture root; SHA256 binds the exact bytes supplied to subprocesses.
type WorkerConfigurationArtifact struct {
	Path          string
	SHA256        string
	Configuration config.Configuration
}

// NewWorkerConfiguration preserves the selected fixture's deployment,
// namespace, endpoints, and secret references, and adds only explicit bounded
// worker controls. It never overwrites the operator-selected server document.
func NewWorkerConfiguration(t testing.TB) WorkerConfigurationArtifact {
	t.Helper()
	selectedPath := os.Getenv("PM_STACK_TEST_CONFIG")
	if selectedPath == "" {
		t.Fatal("PM_STACK_TEST_CONFIG is required for the selected worker fixture")
	}
	selected := readBoundedConfiguration(t, selectedPath)
	selectedDocument, err := config.Load(bytes.NewReader(selected))
	if err != nil {
		t.Fatalf("load selected fixture configuration: %v", err)
	}
	if _, err := config.Validate(selectedDocument, config.Prelaunch{}); err != nil {
		t.Fatalf("validate selected fixture configuration: %v", err)
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(selected, &root); err != nil {
		t.Fatalf("decode selected fixture configuration: %v", err)
	}
	if _, exists := root["worker"]; exists {
		t.Fatal("selected fixture configuration unexpectedly contains worker controls")
	}
	root["worker"] = append(json.RawMessage(nil), testWorkerControls...)
	derived, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatalf("encode test-only worker configuration: %v", err)
	}
	derived = append(derived, '\n')

	fixtureRoot := filepath.Dir(selectedPath)
	leaf := "worker-test-" + randomHex(t, 8) + ".json"
	rootHandle, err := os.OpenRoot(fixtureRoot)
	if err != nil {
		t.Fatalf("open selected fixture root: %v", err)
	}
	file, err := rootHandle.OpenFile(leaf, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = rootHandle.Close()
		t.Fatalf("create test-only worker configuration: %v", err)
	}
	if written, err := file.Write(derived); err != nil || written != len(derived) {
		if err == nil {
			err = io.ErrShortWrite
		}
		_ = file.Close()
		_ = rootHandle.Remove(leaf)
		_ = rootHandle.Close()
		t.Fatalf("write test-only worker configuration: %v", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = rootHandle.Remove(leaf)
		_ = rootHandle.Close()
		t.Fatalf("sync test-only worker configuration: %v", err)
	}
	if err := file.Close(); err != nil {
		_ = rootHandle.Remove(leaf)
		_ = rootHandle.Close()
		t.Fatalf("close test-only worker configuration: %v", err)
	}
	if err := rootHandle.Close(); err != nil {
		_ = os.Remove(filepath.Join(fixtureRoot, leaf))
		t.Fatalf("close selected fixture root: %v", err)
	}
	path := filepath.Join(fixtureRoot, leaf)
	t.Cleanup(func() {
		cleanupRoot, err := os.OpenRoot(fixtureRoot)
		if err != nil {
			t.Errorf("open selected fixture root for worker cleanup: %v", err)
			return
		}
		defer cleanupRoot.Close()
		if err := cleanupRoot.Remove(leaf); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove test-only worker configuration: %v", err)
		}
	})

	document, err := config.Load(bytes.NewReader(derived))
	if err != nil {
		t.Fatalf("load derived worker configuration: %v", err)
	}
	configuration, err := config.Validate(document, config.Prelaunch{})
	if err != nil {
		t.Fatalf("validate derived worker configuration: %v", err)
	}
	if !configuration.Worker().Configured() {
		t.Fatal("derived worker configuration did not enable explicit worker controls")
	}
	digest := sha256.Sum256(derived)
	return WorkerConfigurationArtifact{
		Path:          path,
		SHA256:        "sha256:" + hex.EncodeToString(digest[:]),
		Configuration: configuration,
	}
}

// Verify rereads the exact artifact and refuses replacement or mutation before
// a subprocess or worker lifecycle consumes it.
func (artifact WorkerConfigurationArtifact) Verify(t testing.TB) {
	t.Helper()
	if artifact.Path == "" || artifact.SHA256 == "" {
		t.Fatal("worker configuration artifact identity is required")
	}
	contents := readBoundedConfiguration(t, artifact.Path)
	digest := sha256.Sum256(contents)
	if got := "sha256:" + hex.EncodeToString(digest[:]); got != artifact.SHA256 {
		t.Fatalf("worker configuration artifact hash = %q; want %q", got, artifact.SHA256)
	}
	document, err := config.Load(bytes.NewReader(contents))
	if err != nil {
		t.Fatalf("reload worker configuration artifact: %v", err)
	}
	configuration, err := config.Validate(document, config.Prelaunch{})
	if err != nil {
		t.Fatalf("revalidate worker configuration artifact: %v", err)
	}
	if !configuration.Worker().Configured() {
		t.Fatal("worker configuration artifact lost explicit worker controls")
	}
}

func readBoundedConfiguration(t testing.TB, path string) []byte {
	t.Helper()
	return readBoundedRegularFile(t, path, workerConfigurationLimit, "selected configuration")
}

func readBoundedRegularFile(t testing.TB, path string, limit int64, label string) []byte {
	t.Helper()
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || limit <= 0 {
		t.Fatalf("%s path or bound is invalid", label)
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatalf("open %s directory: %v", label, err)
	}
	defer root.Close()
	file, err := root.OpenFile(filepath.Base(path), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open %s: %v", label, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatalf("inspect opened %s: %v", label, err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("%s is not a regular file", label)
	}
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		t.Fatalf("read %s: %v", label, err)
	}
	if int64(len(contents)) > limit {
		t.Fatalf("%s exceeds the fixture bound", label)
	}
	return contents
}
