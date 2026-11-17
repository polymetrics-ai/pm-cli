package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
