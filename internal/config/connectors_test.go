package config_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/config"
)

const connectorsConfiguration = `{"bundles":["/opt/pm/connectors/github.bundle.json","/opt/pm/connectors/linear.bundle.json"]}`

func TestConnectorBundleDeclaration(t *testing.T) {
	t.Run("absence installs no connector", func(t *testing.T) {
		selectors := loadConnectorsConfiguration(t, startupConfiguration).Connectors()
		if selectors.Configured() {
			t.Fatal("absent connectors configuration became configured")
		}
		if paths := selectors.BundlePaths(); len(paths) != 0 {
			t.Fatalf("absent connector bundle paths = %v; want none", paths)
		}
	})

	t.Run("empty array installs no connector", func(t *testing.T) {
		selectors := loadConnectorsConfiguration(
			t,
			configuredConnectorsDocument(`{"bundles":[]}`),
		).Connectors()
		if selectors.Configured() {
			t.Fatal("empty connector bundle declaration became configured")
		}
	})

	configuration := loadConnectorsConfiguration(t, configuredConnectorsDocument(connectorsConfiguration))
	selectors := configuration.Connectors()
	if !selectors.Configured() {
		t.Fatal("declared connector bundles remained unconfigured")
	}
	want := []string{"/opt/pm/connectors/github.bundle.json", "/opt/pm/connectors/linear.bundle.json"}
	if paths := selectors.BundlePaths(); len(paths) != len(want) || paths[0] != want[0] || paths[1] != want[1] {
		t.Fatalf("declared connector bundle paths = %v; want %v", paths, want)
	}

	paths := selectors.BundlePaths()
	paths[0] = "/opt/pm/connectors/replaced.bundle.json"
	fromConfiguration := configuration.Connectors().BundlePaths()
	if fromConfiguration[0] != want[0] {
		t.Fatalf("configuration getter exposed mutable bundle storage: %v", fromConfiguration)
	}
}

func TestConnectorBundleDeclarationRefusals(t *testing.T) {
	for _, test := range []struct {
		name     string
		document string
		field    string
		kind     config.DiagnosticKind
	}{
		{
			name:     "null block",
			document: configuredConnectorsDocument(`null`),
			field:    "connectors",
			kind:     config.DiagnosticType,
		},
		{
			name:     "missing bundles member",
			document: configuredConnectorsDocument(`{}`),
			field:    "connectors.bundles",
			kind:     config.DiagnosticRequired,
		},
		{
			name:     "unknown member",
			document: configuredConnectorsDocument(`{"bundles":["/opt/pm/github.bundle.json"],"sources":["/opt/pm/github.json"]}`),
			field:    "connectors.sources",
			kind:     config.DiagnosticUnknown,
		},
		{
			name:     "wrong-case member",
			document: configuredConnectorsDocument(`{"Bundles":["/opt/pm/github.bundle.json"]}`),
			field:    "connectors.Bundles",
			kind:     config.DiagnosticUnknown,
		},
		{
			name:     "equal duplicate member",
			document: configuredConnectorsDocument(`{"bundles":["/opt/pm/github.bundle.json"],"bundles":[]}`),
			field:    "connectors.bundles",
			kind:     config.DiagnosticDuplicate,
		},
		{
			name:     "bundles member is not an array",
			document: configuredConnectorsDocument(`{"bundles":"/opt/pm/github.bundle.json"}`),
			field:    "connectors.bundles",
			kind:     config.DiagnosticType,
		},
		{
			name:     "null bundle entry",
			document: configuredConnectorsDocument(`{"bundles":[null]}`),
			field:    "connectors.bundles[0]",
			kind:     config.DiagnosticType,
		},
		{
			name:     "relative bundle path",
			document: configuredConnectorsDocument(`{"bundles":["connectors/github.bundle.json"]}`),
			field:    "connectors.bundles[0]",
			kind:     config.DiagnosticValue,
		},
		{
			name:     "empty bundle path",
			document: configuredConnectorsDocument(`{"bundles":[""]}`),
			field:    "connectors.bundles[0]",
			kind:     config.DiagnosticValue,
		},
		{
			name:     "unclean bundle path",
			document: configuredConnectorsDocument(`{"bundles":["/opt/pm/./github.bundle.json"]}`),
			field:    "connectors.bundles[0]",
			kind:     config.DiagnosticValue,
		},
		{
			name:     "trailing whitespace bundle path",
			document: configuredConnectorsDocument(`{"bundles":["/opt/pm/github.bundle.json "]}`),
			field:    "connectors.bundles[0]",
			kind:     config.DiagnosticValue,
		},
		{
			name:     "control character bundle path",
			document: configuredConnectorsDocument(`{"bundles":["/opt/pm/github\u0000.bundle.json"]}`),
			field:    "connectors.bundles[0]",
			kind:     config.DiagnosticValue,
		},
		{
			name: "oversized bundle path",
			document: configuredConnectorsDocument(
				`{"bundles":["/opt/pm/` + strings.Repeat("b", 4096) + `.bundle.json"]}`,
			),
			field: "connectors.bundles[0]",
			kind:  config.DiagnosticValue,
		},
		{
			name:     "duplicate bundle path",
			document: configuredConnectorsDocument(`{"bundles":["/opt/pm/github.bundle.json","/opt/pm/github.bundle.json"]}`),
			field:    "connectors.bundles[1]",
			kind:     config.DiagnosticDuplicate,
		},
		{
			name: "too many declared bundles",
			document: configuredConnectorsDocument(
				`{"bundles":[` + declaredBundlePaths(33) + `]}`,
			),
			field: "connectors.bundles",
			kind:  config.DiagnosticBounds,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			document, err := config.Load(strings.NewReader(test.document))
			if err == nil {
				_, err = config.Validate(document, config.Prelaunch{})
			}
			var diagnostic *config.Diagnostic
			if !errors.As(err, &diagnostic) || diagnostic.Field != test.field || diagnostic.Kind != test.kind {
				t.Fatalf("connector refusal = %v; want %s/%s", err, test.field, test.kind)
			}
		})
	}
}

func configuredConnectorsDocument(connectors string) string {
	return strings.Replace(
		startupConfiguration,
		`"schema_version":"1"`,
		`"connectors":`+connectors+`,"schema_version":"1"`,
		1,
	)
}

func loadConnectorsConfiguration(t *testing.T, raw string) config.Configuration {
	t.Helper()
	document, err := config.Load(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := config.Validate(document, config.Prelaunch{})
	if err != nil {
		t.Fatal(err)
	}
	return configuration
}

func declaredBundlePaths(count int) string {
	paths := make([]string, 0, count)
	for index := range count {
		paths = append(paths, `"/opt/pm/connectors/bundle-`+strconv.Itoa(index)+`.json"`)
	}
	return strings.Join(paths, ",")
}
