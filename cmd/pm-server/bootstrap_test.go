package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Go developer check, not acceptance proof. A refused bootstrap writes
// nothing on stdout, and its error is the exact text that main writes after
// "pm-server: " as one stderr line. A bootstrap without the bootstrap
// credential is refused after its input checks. Defect: a refusal with
// another line, output on stdout, or a store connection without the
// credential.
func TestBootstrapRefusalHasOneLineAndNoStdout(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "deploy", "server.example.json"))
	if err != nil {
		t.Fatalf("read the example server configuration: %v", err)
	}
	directory := t.TempDir()
	configuration := filepath.Join(directory, "server.json")
	if err := os.WriteFile(configuration, example, 0o600); err != nil {
		t.Fatal(err)
	}
	secrets := filepath.Join(directory, "secrets")
	if err := os.Mkdir(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	const valid = `{"org_id": "org-first", "workspace_id": "workspace-first", "workspace_name": "First workspace",
		"admin_issuer": "https://identity.test.invalid", "admin_subject": "admin-subject", "provider_accounts": []}`
	for name, testCase := range map[string]struct {
		input string
		line  string
	}{
		"empty workspace name": {strings.Replace(valid, `"First workspace"`, `""`, 1), "bootstrap workspace name is required"},
		"empty admin subject":  {strings.Replace(valid, `"admin-subject"`, `""`, 1), "bootstrap admin subject is required"},
		"undeclared connector": {strings.Replace(valid, `[]`, `[{"connector_id": "postgres", "display_name": "Customer database"}]`, 1),
			"bootstrap provider account names an undeclared connector"},
		"no bootstrap credential": {valid, "bootstrap requires operator authority"},
	} {
		t.Run(name, func(t *testing.T) {
			input := filepath.Join(t.TempDir(), "input.json")
			if err := os.WriteFile(input, []byte(testCase.input), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			err := run(context.Background(), []string{
				"bootstrap", "--config", configuration, "--secrets-directory", secrets, "--input", input,
			}, nil, &stdout, &stderr)
			if err == nil || err.Error() != testCase.line {
				t.Fatalf("bootstrap error = %v; want exactly %q", err, testCase.line)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("refused bootstrap wrote stdout %q and stderr %q; want nothing before main writes its line", stdout.String(), stderr.String())
			}
		})
	}
}

// Go developer check, not acceptance proof. A FIFO as --input has no writer,
// so a plain open waits for one. Defect: the bootstrap blocks on the FIFO. It
// must refuse the input at once with the input line, and write nothing on
// stdout or stderr.
func TestBootstrapRefusesAnInputThatIsNotARegularFile(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "deploy", "server.example.json"))
	if err != nil {
		t.Fatalf("read the example server configuration: %v", err)
	}
	directory := t.TempDir()
	configuration := filepath.Join(directory, "server.json")
	if err := os.WriteFile(configuration, example, 0o600); err != nil {
		t.Fatal(err)
	}
	secrets := filepath.Join(directory, "secrets")
	if err := os.Mkdir(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(directory, "input.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("make the FIFO input: %v", err)
	}
	// When the bootstrap still waits in an open of the FIFO, a writer end ends
	// that wait, so no goroutine outlives the test.
	t.Cleanup(func() {
		if writer, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = writer.Close()
		}
	})
	type outcome struct {
		err            error
		stdout, stderr string
	}
	done := make(chan outcome, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		err := run(context.Background(), []string{
			"bootstrap", "--config", configuration, "--secrets-directory", secrets, "--input", fifo,
		}, nil, &stdout, &stderr)
		done <- outcome{err: err, stdout: stdout.String(), stderr: stderr.String()}
	}()
	guard := time.NewTimer(10 * time.Second)
	defer guard.Stop()
	select {
	case got := <-done:
		if got.err == nil || got.err.Error() != "bootstrap input is invalid" {
			t.Fatalf("bootstrap with a FIFO input = %v; want exactly %q", got.err, "bootstrap input is invalid")
		}
		if got.stdout != "" || got.stderr != "" {
			t.Fatalf("bootstrap with a FIFO input wrote stdout %q and stderr %q; want nothing", got.stdout, got.stderr)
		}
	case <-guard.C:
		t.Fatal("bootstrap with a FIFO input did not return in 10 s; want the refusal at once")
	}
}
