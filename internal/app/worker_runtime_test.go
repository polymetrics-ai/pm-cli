package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/credentials"
)

func TestWorkerRuntimeCloseClosesSecretsAfterArtifactCleanupFailure(t *testing.T) {
	secretsDirectory := t.TempDir()
	bundleDirectory := filepath.Join(secretsDirectory, "runtime")
	if err := os.Mkdir(bundleDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	const secretValue = "runtime-close-secret-sentinel"
	if err := os.WriteFile(filepath.Join(bundleDirectory, "member"), []byte(secretValue), 0o600); err != nil {
		t.Fatal(err)
	}
	secrets, err := credentials.OpenOperatorSecrets(secretsDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if value, readErr := secrets.Text("secret://runtime", "member"); readErr != nil || value != secretValue {
		t.Fatalf("read open worker secrets = %q, %v", value, readErr)
	}

	scratch := filepath.Join(t.TempDir(), "artifact-scratch")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "active"), []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := &WorkerRuntime{
		artifacts: &ArtifactResources{scratch: scratch, identity: identity},
		secrets:   secrets,
	}

	closeErr := runtime.Close()
	if closeErr == nil {
		t.Fatal("close worker runtime with occupied artifact scratch succeeded")
	}
	if strings.Contains(closeErr.Error(), secretValue) || strings.Contains(closeErr.Error(), secretsDirectory) {
		t.Fatalf("worker runtime close error exposed secret material: %v", closeErr)
	}
	if _, readErr := secrets.Text("secret://runtime", "member"); readErr == nil {
		t.Fatal("worker secrets remained open after artifact cleanup failure")
	}
}

