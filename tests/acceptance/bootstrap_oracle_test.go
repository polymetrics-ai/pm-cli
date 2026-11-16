package acceptance

// Developer checks of the bootstrap step code (red review round 1). They are
// not acceptance proof. They need no stack, start no process and write no
// PM row. Each case feeds a checker of the steps a fixed input and expects
// the checker to pass or to fail.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeOracleSecrets writes the members into a new secrets directory and
// returns the forms that the no-secret step compares.
func writeOracleSecrets(t *testing.T, members map[string][]byte) []secretForm {
	t.Helper()
	root := t.TempDir()
	for name, contents := range members {
		if err := os.WriteFile(filepath.Join(root, name), contents, 0o600); err != nil {
			t.Fatalf("write the oracle secret %s: %v", name, err)
		}
	}
	forms, err := readDeploymentSecrets(root)
	if err != nil {
		t.Fatalf("read the oracle secrets: %v", err)
	}
	return forms
}

// unicodeEscaped writes each rune of value as a JSON \uXXXX escape.
func unicodeEscaped(value string) string {
	var builder strings.Builder
	for _, r := range value {
		fmt.Fprintf(&builder, `\u%04x`, r)
	}
	return builder.String()
}

func TestBootstrapSecretCheckFindsEscapedText(t *testing.T) {
	const password = "Zq9-oracle-Pass"
	forms := writeOracleSecrets(t, map[string][]byte{
		bootstrapCredentialMember: []byte("postgresql://pm_app_bootstrap:" + password + "@db.invalid:5432/pm_app\n"),
	})
	clean := []byte(`{"workspace_id":"acceptance-workspace-1"}` + "\n")
	if source, found := findSecret(clean, forms); found {
		t.Fatalf("the check finds %s in output with no secret", source)
	}
	leak := []byte(`{"workspace_id":"acceptance-workspace-1","note":"` + unicodeEscaped(password) + `"}` + "\n")
	if bytes.Contains(leak, []byte(password)) {
		t.Fatal("the escaped case holds the raw password, so it tests nothing")
	}
	source, found := findSecret(leak, forms)
	if !found {
		t.Fatal("the check does not find a password written with JSON unicode escapes")
	}
	if strings.Contains(source, password) {
		t.Fatal("the source of the finding holds the secret value")
	}
}

func TestBootstrapSecretCheckFindsBase64WrappingKey(t *testing.T) {
	// A 32-byte wrapping key, as deployment_test.go writes. Its first and
	// last bytes are white space, so a trimmed comparison would miss them.
	key := []byte(" \x00\x01\x02\x7f\x80\x81\xfe\xffwrapping-key-bytes-x0\x9a\n")
	if len(key) != 32 {
		t.Fatalf("the oracle wrapping key has %d bytes, want 32", len(key))
	}
	forms := writeOracleSecrets(t, map[string][]byte{"wrapping-key": key})
	clean := []byte(`{"workspace_id":"acceptance-workspace-1"}` + "\n")
	if source, found := findSecret(clean, forms); found {
		t.Fatalf("the check finds %s in output with no secret", source)
	}
	// encoding/json writes a byte slice as standard base64.
	leak, err := json.Marshal(map[string]any{"workspace_id": "acceptance-workspace-1", "key": key})
	if err != nil {
		t.Fatalf("encode the leak case: %v", err)
	}
	if bytes.Contains(leak, key) {
		t.Fatal("the base64 case holds the raw key, so it tests nothing")
	}
	if _, found := findSecret(leak, forms); !found {
		t.Fatal("the check does not find the wrapping key in its base64 form")
	}
}
