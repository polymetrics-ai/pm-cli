package config_test

import (
	"strings"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/config"
)

// providerSQLRelationBindings is the declared relation binding surface of the
// customer connection. The binding identity is the operation's declared logical
// relation; the concrete schema, table and key columns are server-owned facts
// that never come from public input.
const providerSQLRelationBindings = `[{"binding":"postgres.reusable-relation","schema":"customer","table":"records","key_columns":["record_id"],"snapshot":{"provider_account_id":"account-postgres-main","permission_revision":"permissions-v1","resource":"customer.records","source_revision":"source-v1"}}]`

const providerSQLConnections = `[{"org_id":"org-alpha","workspace_id":"workspace-alpha","connection_id":"postgres-main","connector_id":"postgres","host":"127.0.0.1","port":5432,"database":"customer_db","user":"customer_role","tls_trust_ref":"secret://fixture/provider-sql","password_ref":"credential-postgres-main","state_directory":"/opt/pm/state/postgres-main","relation_bindings":` + providerSQLRelationBindings + `}]`

const providerSQLDeclaration = `{"max_connect_duration":"5s","max_statement_duration":"10s","max_total_duration":"30s","max_rows":256,"max_bytes":65536,"max_attempts":2,"connections":` + providerSQLConnections + `}`

// TestProviderSQLDeclaration proves the server-owned customer SQL declaration is
// part of the same strict configuration boundary as the provider HTTP policy:
// a complete declaration resolves, and a malformed one is refused.
func TestProviderSQLDeclaration(t *testing.T) {
	t.Run("declared connection resolves its exact server-owned facts", func(t *testing.T) {
		document, err := config.Load(strings.NewReader(configuredProviderSQLDocument(providerSQLDeclaration)))
		if err != nil {
			t.Fatalf("load declared provider SQL configuration: %v", err)
		}
		if _, err := config.Validate(document, config.Prelaunch{}); err != nil {
			t.Fatalf("declared provider SQL configuration was refused: %v", err)
		}
	})

	// The declaration is a closed boundary: a member the decoder does not
	// declare, a duplicated connection, a wrongly typed bound, an absent
	// required member and an unusable endpoint must each be refused instead of
	// being ignored, coerced or defaulted.
	for _, refusal := range []struct {
		name     string
		document string
	}{
		{
			name: "unknown member",
			document: strings.Replace(providerSQLDeclaration, `"max_rows":256`,
				`"max_rows":256,"undeclared_member":1`, 1),
		},
		{
			name: "duplicated connection identity",
			document: strings.Replace(providerSQLConnections, `[{"org_id"`,
				`[{"org_id":"org-alpha","workspace_id":"workspace-alpha","connection_id":"postgres-main","connector_id":"postgres","host":"127.0.0.1","port":5432,"database":"customer_db","user":"customer_role","tls_trust_ref":"secret://fixture/provider-sql","password_ref":"credential-postgres-main","state_directory":"/opt/pm/state/postgres-main","relation_bindings":`+providerSQLRelationBindings+`},{"org_id"`, 1),
		},
		{
			// Connection identities are workspace-scoped, so another workspace may
			// declare the same identity, but never the same schema state directory.
			name: "state directory shared across workspaces",
			document: strings.Replace(providerSQLConnections, `[{"org_id"`,
				`[{"org_id":"org-alpha","workspace_id":"workspace-beta","connection_id":"postgres-main","connector_id":"postgres","host":"127.0.0.1","port":5432,"database":"customer_db","user":"customer_role","tls_trust_ref":"secret://fixture/provider-sql","password_ref":"credential-postgres-main","state_directory":"/opt/pm/state/postgres-main","relation_bindings":`+providerSQLRelationBindings+`},{"org_id"`, 1),
		},
		{
			name:     "wrongly typed bound",
			document: strings.Replace(providerSQLDeclaration, `"max_rows":256`, `"max_rows":"many"`, 1),
		},
		{
			name: "absent required member",
			document: strings.Replace(providerSQLConnections,
				`"state_directory":"/opt/pm/state/postgres-main",`, ``, 1),
		},
		{
			name: "unusable endpoint",
			document: strings.Replace(providerSQLConnections,
				`"port":5432`, `"port":0`, 1),
		},
	} {
		t.Run("refuses "+refusal.name, func(t *testing.T) {
			declaration := providerSQLDeclaration
			if strings.Contains(refusal.document, `"connections"`) {
				declaration = refusal.document
			} else {
				declaration = `{"max_connect_duration":"5s","max_statement_duration":"10s","max_total_duration":"30s","max_rows":256,"max_bytes":65536,"max_attempts":2,"connections":` + refusal.document + `}`
			}
			// The strict boundary refuses a malformed declaration while loading it
			// or while resolving it; either stage counts, and only a declaration
			// that survives both is a missing refusal.
			document, err := config.Load(strings.NewReader(configuredProviderSQLDocument(declaration)))
			if err != nil {
				return
			}
			if _, err := config.Validate(document, config.Prelaunch{}); err == nil {
				t.Fatalf("the %s declaration was accepted", refusal.name)
			}
		})
	}
}

// configuredProviderSQLDocument places one declared provider_sql block in the
// same complete startup document the other configuration cases use.
func configuredProviderSQLDocument(providerSQL string) string {
	return strings.Replace(
		startupConfiguration,
		`"schema_version":"1"`,
		`"provider_sql":`+providerSQL+`,"schema_version":"1"`,
		1,
	)
}
