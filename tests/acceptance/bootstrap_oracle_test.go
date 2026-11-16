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

func TestBootstrapFailureMessageWithholdsSecret(t *testing.T) {
	const password = "Zq9-oracle-Pass"
	root := t.TempDir()
	endpoint := "postgresql://pm_app_bootstrap:" + password + "@db.invalid:5432/pm_app\n"
	if err := os.WriteFile(filepath.Join(root, bootstrapCredentialMember), []byte(endpoint), 0o600); err != nil {
		t.Fatalf("write the oracle secret: %v", err)
	}
	run := &bootstrapRun{deployment: &deployment{secrets: root}}
	if shown := run.shown("pm-server: unknown flag: --input\n"); shown != `"pm-server: unknown flag: --input\n"` {
		t.Fatalf("the message shows clean output as %s", shown)
	}
	for name, output := range map[string]string{
		"raw":     "connect " + endpoint,
		"escaped": `{"note":"` + unicodeEscaped(password) + `"}`,
	} {
		shown := run.shown(output)
		if strings.Contains(shown, password) || strings.Contains(shown, unicodeEscaped(password)) ||
			!strings.Contains(shown, "withheld") {
			t.Errorf("%s case: the message does not withhold the output: %s", name, shown)
		}
	}
}

// oracleScope returns a run and the one-scope reading of a correct
// bootstrap of that run. membership changes the membership row.
func oracleScope(t *testing.T, membership func(map[string]any)) (*bootstrapRun, bootstrapState) {
	t.Helper()
	input := bootstrapInput{
		OrgID:            "acceptance-org-oracle",
		WorkspaceID:      "acceptance-workspace-oracle",
		WorkspaceName:    "Acceptance workspace oracle",
		AdminIssuer:      "https://issuer.invalid",
		AdminSubject:     bootstrapAdminSubject,
		ProviderAccounts: []bootstrapProviderAccount{{ConnectorID: "postgres", DisplayName: "Acceptance postgres account"}},
	}
	run := &bootstrapRun{deployment: &deployment{deploymentID: "pm-acceptance-oracle"}, input: input}
	admin := map[string]any{
		"org_id": input.OrgID, "workspace_id": input.WorkspaceID, "membership_id": "membership-1",
		"identity_id": "identity-1", "role": "admin",
		"resource_grants": []string{"permission:connection.manage", "permission:workspace.access.manage"},
		"permissions":     []string{"connection.manage", "workspace.access.manage"},
		"connection_ids":  []string{}, "operation_ids": []string{},
		"expires_at": nil, "revoked_at": nil,
	}
	if membership != nil {
		membership(admin)
	}
	rows := map[string]any{
		"workspace_namespaces": map[string]any{"org_id": input.OrgID, "workspace_id": input.WorkspaceID, "name": input.WorkspaceName},
		"identities": map[string]any{"org_id": input.OrgID, "identity_id": "identity-1", "kind": "user",
			"issuer": input.AdminIssuer, "subject": input.AdminSubject},
		"auth_bootstrap_identities": map[string]any{"deployment_id": "pm-acceptance-oracle", "issuer": input.AdminIssuer,
			"subject": input.AdminSubject, "org_id": input.OrgID, "principal_id": "identity-1",
			"principal_kind": "user", "enabled": true},
		"memberships":       admin,
		"provider_accounts": map[string]any{"org_id": input.OrgID, "provider_account_id": "account-1", "provider": "postgres"},
		"bootstrap_records": map[string]any{"deployment_id": "pm-acceptance-oracle", "org_id": input.OrgID,
			"workspace_id": input.WorkspaceID, "admin_identity_id": "identity-1", "membership_id": "membership-1"},
		"workspace_audit_entries": map[string]any{"org_id": input.OrgID, "requested_actor_kind": "operator",
			"effective_actor_kind": "operator"},
	}
	state := bootstrapState{"audit_entries": nil}
	for table, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatalf("encode the oracle %s row: %v", table, err)
		}
		state[table] = []string{string(encoded)}
	}
	return run, state
}

func TestBootstrapScopeCheckAcceptsAdminMembership(t *testing.T) {
	run, state := oracleScope(t, nil)
	if err := checkScope(state, run, true); err != nil {
		t.Fatalf("the check refuses a correct admin membership: %v", err)
	}
}

func TestBootstrapScopeCheckRefusesNonAdminMembership(t *testing.T) {
	cases := map[string]func(map[string]any){
		"a linked membership with no authority": func(row map[string]any) {
			row["role"] = "member"
			row["resource_grants"] = []string{}
			row["permissions"] = []string{}
		},
		"no right to manage connections": func(row map[string]any) {
			row["resource_grants"] = []string{"permission:workspace.access.manage"}
			row["permissions"] = []string{"workspace.access.manage"}
		},
		"no right to manage memberships": func(row map[string]any) {
			row["resource_grants"] = []string{"permission:connection.manage"}
			row["permissions"] = []string{"connection.manage"}
		},
		"a connection at the start": func(row map[string]any) {
			row["resource_grants"] = []string{"connection:connection-1", "permission:connection.manage", "permission:workspace.access.manage"}
			row["connection_ids"] = []string{"connection-1"}
		},
		"a revoked membership": func(row map[string]any) {
			row["revoked_at"] = "2026-11-16T00:00:00Z"
		},
	}
	for name, change := range cases {
		run, state := oracleScope(t, change)
		if err := checkScope(state, run, true); err == nil {
			t.Errorf("%s: the check accepts a membership with no admin authority", name)
		}
	}
}
