package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Go developer check, not acceptance proof (issue #366, developer check C3).
// pm-server and pm-server bootstrap refuse a device poll interval outside 1 s
// to 60 s. The error is the exact configuration diagnostic that main writes
// after "pm-server: ". The configuration check comes before any secret read:
// the secrets directory and the bootstrap input do not exist. Defect: a
// refusal with another kind or another text.
func TestDevicePollIntervalRefusalLine(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "deploy", "server.example.json"))
	if err != nil {
		t.Fatalf("read the example server configuration: %v", err)
	}
	const secretReference = `"client_secret_ref": "secret://operator/pm-development-oidc"`
	raw := strings.Replace(string(example), secretReference,
		secretReference+",\n    \"device_poll_interval\": \"61s\"", 1)
	if raw == string(example) {
		t.Fatal("the example identity was not replaced")
	}
	directory := t.TempDir()
	configuration := filepath.Join(directory, "server.json")
	if err := os.WriteFile(configuration, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	secrets := filepath.Join(directory, "absent-secrets")
	input := filepath.Join(directory, "absent-input.json")
	const want = "configuration bounds at identity.device_poll_interval"
	for name, args := range map[string][]string{
		"pm-server": {"--config", configuration, "--secrets-directory", secrets},
		"pm-server bootstrap": {
			"bootstrap", "--config", configuration, "--secrets-directory", secrets, "--input", input,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(context.Background(), args, nil, &stdout, &stderr)
			if err == nil || err.Error() != want {
				t.Fatalf("%s error = %v; want exactly %q", name, err, want)
			}
			if stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("%s wrote stdout %q and stderr %q; want nothing before main writes its line",
					name, stdout.String(), stderr.String())
			}
		})
	}
}
