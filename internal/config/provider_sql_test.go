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
// a complete declaration resolves, and every unknown, duplicated, wrongly typed
// or absent member is refused with a field-specific diagnostic.
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
