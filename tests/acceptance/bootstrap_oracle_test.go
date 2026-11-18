package acceptance

// Developer checks of the bootstrap step code (red review round 1). They are
// not acceptance proof. They need no stack, start no process and write no
// PM row. Each case feeds a checker of the steps a fixed input and expects
// the checker to pass or to fail.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/tests/keycloak"
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
	if source, found, complete := findSecret(clean, forms); found || !complete {
		t.Fatalf("the check does not pass clean output: found %t (%s), complete %t", found, source, complete)
	}
	leak := []byte(`{"workspace_id":"acceptance-workspace-1","note":"` + unicodeEscaped(password) + `"}` + "\n")
	if bytes.Contains(leak, []byte(password)) {
		t.Fatal("the escaped case holds the raw password, so it tests nothing")
	}
	source, found, _ := findSecret(leak, forms)
	if !found {
		t.Fatal("the check does not find a password written with JSON unicode escapes")
	}
	if strings.Contains(source, password) {
		t.Fatal("the source of the finding holds the secret value")
	}
}

func TestBootstrapSecretCheckFindsEscapedTextInDuplicateMember(t *testing.T) {
	const password = "Zq9-oracle-Pass"
	forms := writeOracleSecrets(t, map[string][]byte{
		bootstrapCredentialMember: []byte("postgresql://pm_app_bootstrap:" + password + "@db.invalid:5432/pm_app\n"),
	})
	// Valid JSON: a decoded map keeps only the last "note", which is clean.
	leak := []byte(`{"workspace_id":"acceptance-workspace-1","note":"` + unicodeEscaped(password) + `","note":"clean"}` + "\n")
	var decoded map[string]any
	if err := json.Unmarshal(leak, &decoded); err != nil || decoded["note"] != "clean" {
		t.Fatalf("the duplicate-member case is not valid JSON with a clean last member: %v", err)
	}
	if bytes.Contains(leak, []byte(password)) {
		t.Fatal("the duplicate-member case holds the raw password, so it tests nothing")
	}
	if _, found, complete := findSecret(leak, forms); !found || !complete {
		t.Fatalf("the check does not find the password in the overwritten member: found %t, complete %t", found, complete)
	}
}

func TestBootstrapSecretCheckRefusesOutputThatItCannotInspect(t *testing.T) {
	const password = "Zq9-oracle-Pass"
	forms := writeOracleSecrets(t, map[string][]byte{
		bootstrapCredentialMember: []byte("postgresql://pm_app_bootstrap:" + password + "@db.invalid:5432/pm_app\n"),
	})
	escaped := unicodeEscaped(password)
	// Truncated escaped JSON: the string is closed, so the scan decodes it and
	// finds the secret, but the output is not complete.
	// Escaped JSON after a prefix: the decoder stops at the prefix, so the
	// scan does not decode the string, and the output is not complete.
	for name, output := range map[string]string{
		"truncated escaped JSON":      `{"note":"` + escaped + `"`,
		"escaped JSON after a prefix": `error: {"note":"` + escaped + `"}`,
	} {
		_, found, complete := findSecret([]byte(output), forms)
		if name == "truncated escaped JSON" && !found {
			t.Errorf("%s case: the check does not find the closed escaped secret", name)
		}
		if complete {
			t.Errorf("%s case: the check reports the output as complete", name)
		}
		if !found && complete {
			t.Errorf("%s case: the check reports the output as clean and complete", name)
		}
	}
	// Output that stops inside an open object or array, with no secret.
	for _, output := range []string{`{"a":1`, `[1,2`, `{"note":`} {
		if _, found, complete := findSecret([]byte(output), forms); found || complete {
			t.Errorf("%q: found %t, complete %t, want found false and complete false", output, found, complete)
		}
	}
}

func TestBootstrapSecretCheckFindsShortSecrets(t *testing.T) {
	const filePassword, dsnPassword = "shortpw", "pw7"
	forms := writeOracleSecrets(t, map[string][]byte{
		"short-secret":            []byte(filePassword + "\n"),
		bootstrapCredentialMember: []byte("postgresql://pm_app_bootstrap:" + dsnPassword + "@db.invalid:5432/pm_app\n"),
	})
	clean := []byte(`{"workspace_id":"acceptance-workspace-1"}` + "\n")
	if source, found, complete := findSecret(clean, forms); found || !complete {
		t.Fatalf("the check does not pass clean output: found %t (%s), complete %t", found, source, complete)
	}
	for name, output := range map[string]string{
		"short file secret":           `{"note":"` + filePassword + `"}`,
		"short file secret, escaped":  `{"note":"` + unicodeEscaped(filePassword) + `"}`,
		"short DSN password":          `{"note":"` + dsnPassword + `"}`,
		"short DSN password, escaped": `{"note":"` + unicodeEscaped(dsnPassword) + `"}`,
	} {
		if _, found, _ := findSecret([]byte(output), forms); !found {
			t.Errorf("%s case: the check does not find the secret", name)
		}
		// The failure-message rule: no description holds either secret.
		description := describeOutput(output)
		if strings.Contains(description, filePassword) || strings.Contains(description, dsnPassword) {
			t.Errorf("%s case: the description %q holds the secret", name, description)
		}
		assertDescriptionHoldsNoSecret(t, name, output, forms)
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
	if source, found, complete := findSecret(clean, forms); found || !complete {
		t.Fatalf("the check does not pass clean output: found %t (%s), complete %t", found, source, complete)
	}
	// encoding/json writes a byte slice as standard base64.
	leak, err := json.Marshal(map[string]any{"workspace_id": "acceptance-workspace-1", "key": key})
	if err != nil {
		t.Fatalf("encode the leak case: %v", err)
	}
	if bytes.Contains(leak, key) {
		t.Fatal("the base64 case holds the raw key, so it tests nothing")
	}
	if _, found, _ := findSecret(leak, forms); !found {
		t.Fatal("the check does not find the wrapping key in its base64 form")
	}
}

// assertDescriptionHoldsNoSecret fails when the description of output holds
// the secret value or one of its forms.
func assertDescriptionHoldsNoSecret(t *testing.T, name, output string, forms []secretForm) {
	t.Helper()
	description := describeOutput(output)
	if want := fmt.Sprintf("%d bytes, not shown", len(output)); description != want {
		t.Errorf("%s case: the description is %q, want %q", name, description, want)
	}
	for _, form := range forms {
		if strings.Contains(description, string(form.value)) {
			t.Errorf("%s case: the description holds the form %s", name, form.source)
		}
	}
}

func TestBootstrapFailureMessageShowsNoRawOutput(t *testing.T) {
	const password = "Zq9-oracle-Pass"
	endpoint := "postgresql://pm_app_bootstrap:" + password + "@db.invalid:5432/pm_app\n"
	forms := writeOracleSecrets(t, map[string][]byte{bootstrapCredentialMember: []byte(endpoint)})
	if got, want := describeOutput("pm-server: unknown flag: --input\n"),
		`33 bytes, the known line "pm-server: unknown flag: --input"`; got != want {
		t.Fatalf("the description of a known line is %q, want %q", got, want)
	}
	if got := describeOutput(""); got != "0 bytes" {
		t.Fatalf("the description of empty output is %q, want %q", got, "0 bytes")
	}
	escaped := unicodeEscaped(password)
	for name, output := range map[string]string{
		"raw":                          "connect " + endpoint,
		"complete escaped JSON":        `{"note":"` + escaped + `"}`,
		"truncated escaped JSON":       `{"note":"` + escaped + `"`,
		"escaped JSON after a prefix":  `error: {"note":"` + escaped + `"}`,
		"a known line with the secret": "pm-server: unknown flag: --input " + password + "\n",
	} {
		assertDescriptionHoldsNoSecret(t, name, output, forms)
	}
}

// oracleAdminSubject is the admin subject of the oracle input: a fixed value
// in the form of a Keycloak user ID. The scope check compares it only with
// the oracle rows, so no Keycloak user has it.
const oracleAdminSubject = "00000000-0000-4000-8000-0000000000a1"

// oracleScope returns a run and the one-scope reading of a correct
// bootstrap of that run. membership changes the membership row.
func oracleScope(t *testing.T, membership func(map[string]any)) (*bootstrapRun, bootstrapState) {
	t.Helper()
	input := bootstrapInput{
		OrgID:            "acceptance-org-oracle",
		WorkspaceID:      "acceptance-workspace-oracle",
		WorkspaceName:    "Acceptance workspace oracle",
		AdminIssuer:      "https://issuer.invalid",
		AdminSubject:     oracleAdminSubject,
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
		"workspace_audit_entries": map[string]any{"org_id": input.OrgID,
			"requested_actor_id": "bootstrap", "requested_actor_kind": "operator",
			"effective_actor_id": "bootstrap", "effective_actor_kind": "operator"},
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

// K1a says that the audit record names the operator bootstrap as its actor.
// Defect: a check that reads only the actor kinds and accepts an audit record
// of another operator actor.
func TestBootstrapScopeCheckRefusesAnAuditOfAnotherActor(t *testing.T) {
	cases := map[string]map[string]any{
		"another requested and effective actor": {"requested_actor_id": "someone-else", "effective_actor_id": "someone-else"},
		"another requested actor":               {"requested_actor_id": "someone-else", "effective_actor_id": "bootstrap"},
		"another effective actor":               {"requested_actor_id": "bootstrap", "effective_actor_id": "someone-else"},
	}
	for name, ids := range cases {
		run, state := oracleScope(t, nil)
		audit := map[string]any{"org_id": run.input.OrgID, "requested_actor_kind": "operator", "effective_actor_kind": "operator"}
		for column, value := range ids {
			audit[column] = value
		}
		encoded, err := json.Marshal(audit)
		if err != nil {
			t.Fatalf("encode the %s audit row: %v", name, err)
		}
		state["workspace_audit_entries"] = []string{string(encoded)}
		if err := checkScope(state, run, true); err == nil {
			t.Errorf("the check accepts an audit record with %s and the operator kinds", name)
		}
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

// oracleNoNetwork refuses every request. The oracle Keycloak admin client
// uses it, so the bundle checks cannot reach a Keycloak.
type oracleNoNetwork struct{}

func (oracleNoNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("the bootstrap oracle sends no Keycloak request")
}

// oracleBundleScenario returns a bootstrap scenario with one deployment that
// has not written its configuration. It starts no process, needs no stack and
// sends no request. The configuration step reads only the issuer of the admin
// client and the client ID of the deployment identity. The oracle admin
// client has a fixed issuer with a reserved host name and a transport that
// refuses every request. The deployment identity holds only a client ID. Both
// exist only in these developer checks.
func oracleBundleScenario(t *testing.T) (*bootstrapScenario, *deployment) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve the repository root: %v", err)
	}
	admin, err := keycloak.New(keycloak.Config{
		Issuer:       "https://issuer.invalid/realms/pm-oracle",
		ClientID:     "pm-oracle-admin",
		ClientSecret: "pm-oracle-unused",
		RequestLimit: time.Second,
		Transport:    oracleNoNetwork{},
	})
	if err != nil {
		t.Fatalf("make the oracle Keycloak admin client: %v", err)
	}
	d := &deployment{
		dir:          t.TempDir(),
		deploymentID: "pm-acceptance-oracle",
		listen:       "127.0.0.1:1",
		origin:       "https://127.0.0.1:2",
		namespace:    "oracle",
		identity:     &keycloak.Identity{ClientID: "pm-acceptance-oracle"},
		stack: stackInputs{
			document: map[string]json.RawMessage{
				"postgres": json.RawMessage(`{"endpoint_ref":"secret://operator/postgres"}`),
				"temporal": json.RawMessage(`{"namespace":"stack"}`),
			},
			identity: stackIdentity{admin: admin},
		},
	}
	s := &bootstrapScenario{shared: &stackScenario{harness: &stackHarness{bins: binaries{root: root}}, deployment: d}}
	return s, d
}

func TestBootstrapBundleStepDeclaresOneBundle(t *testing.T) {
	s, d := oracleBundleScenario(t)
	if err := s.declaresBundle("deployment", "postgres"); err != nil {
		t.Fatalf("the step refuses the checked-in postgres bundle: %v", err)
	}
	if d.configuration == "" {
		t.Fatal("the step did not write the configuration")
	}
	if err := s.declaresBundle("deployment", "postgres"); err == nil || !strings.Contains(err.Error(), "item 3") {
		t.Fatalf("a second declaration is not refused at item 3: %v", err)
	}
}

func TestBootstrapBundleStepRefusesWrongInput(t *testing.T) {
	s, _ := oracleBundleScenario(t)
	if err := s.declaresBundle("deployment", "acceptance-undeclared"); err == nil || !strings.Contains(err.Error(), "item 2") {
		t.Errorf("a missing bundle is not refused at item 2: %v", err)
	}
	if err := s.declaresBundle("deployment", "../postgres"); err == nil || !strings.Contains(err.Error(), "item 1") {
		t.Errorf("a path name is not refused at item 1: %v", err)
	}
	s, d := oracleBundleScenario(t)
	if _, err := d.configurationPath(); err != nil {
		t.Fatalf("write the oracle configuration: %v", err)
	}
	if err := s.declaresBundle("deployment", "postgres"); err == nil || !strings.Contains(err.Error(), "item 4") {
		t.Errorf("a written configuration is not refused at item 4: %v", err)
	}
	if err := s.declaresBundle("second deployment", "postgres"); err == nil {
		t.Error("the step accepts a missing second deployment")
	}
}
