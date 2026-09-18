package cli

import (
	"bytes"
	"context"
	"testing"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/spf13/cobra"
)

const (
	testCatalogRevision   = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testOperationRevision = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestStableNamesFlagsHelp(t *testing.T) {
	t.Parallel()
	t.Run("unsafe collisions refuse", testCommandTreeRejectsControlAndWireCollisions)
	t.Run("help manual and completion share the validated tree", testHelpManualAndCompletionShareValidatedTree)
}

func testCommandTreeRejectsControlAndWireCollisions(t *testing.T) {
	for _, test := range []struct {
		name   string
		fields []pmapi.CommandFieldDescriptor
	}{
		{
			name:   "provider attempts to own workspace selector",
			fields: []pmapi.CommandFieldDescriptor{commandField("--workspace", "query.workspace", "workspace", contract.FieldLocationQuery)},
		},
		{
			name:   "provider attempts to own result controls",
			fields: []pmapi.CommandFieldDescriptor{commandField("--result-controls", "query.result-controls", "result-controls", contract.FieldLocationQuery)},
		},
		{
			name: "same wire name and flag across locations",
			fields: []pmapi.CommandFieldDescriptor{
				commandField("--tenant", "query.tenant", "tenant", contract.FieldLocationQuery),
				commandField("--tenant", "header.tenant", "tenant", contract.FieldLocationHeader),
			},
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			root := &cobra.Command{Use: "pm"}
			err := BuildCommandTree(root, commandSnapshot(test.fields), func(context.Context, DynamicInvocation) error { return nil })
			if err == nil {
				t.Fatal("unsafe generated command tree was accepted")
			}
		})
	}
}

func testHelpManualAndCompletionShareValidatedTree(t *testing.T) {
	fields := []pmapi.CommandFieldDescriptor{
		commandField("--query-workspace", "query.workspace", "workspace", contract.FieldLocationQuery),
		commandField("--header-workspace", "header.workspace", "workspace", contract.FieldLocationHeader),
	}
	snapshot := commandSnapshot(fields)
	root := NewRoot(Options{Catalog: &snapshot, DynamicInvoke: func(context.Context, DynamicInvocation) error { return nil }})
	projected, _, err := root.Find([]string{"inspect-widget"})
	if err != nil {
		t.Fatalf("find generated command: %v", err)
	}
	if projected.Annotations[dynamicOperationAnnotation] != "op-1" || projected.Annotations[dynamicRevisionAnnotation] != testOperationRevision {
		t.Fatalf("generated annotations = %#v", projected.Annotations)
	}

	help := &bytes.Buffer{}
	if err := RenderHelp(projected, help); err != nil {
		t.Fatalf("render generated help: %v", err)
	}
	manual := &bytes.Buffer{}
	if err := RenderCommandSurface(root, CommandSurfaceManual, "", manual); err != nil {
		t.Fatalf("render generated manual: %v", err)
	}
	completion := &bytes.Buffer{}
	if err := RenderCommandSurface(root, CommandSurfaceCompletion, "bash", completion); err != nil {
		t.Fatalf("render generated completion: %v", err)
	}
	for name, document := range map[string][]byte{
		"help": help.Bytes(), "manual": manual.Bytes(), "completion": completion.Bytes(),
	} {
		for _, expected := range []string{"inspect-widget", "read-widget", "query-workspace", "header-workspace"} {
			if !bytes.Contains(document, []byte(expected)) {
				t.Fatalf("%s omitted %q from the executable tree projection", name, expected)
			}
		}
	}
}

func commandSnapshot(fields []pmapi.CommandFieldDescriptor) pmapi.CatalogSnapshot {
	return pmapi.CatalogSnapshot{
		Binding: pmapi.CatalogBinding{
			ServerInstanceID: "server-1",
			Principal:        pmapi.Principal{Kind: pmapi.PrincipalKindUser, PrincipalId: "ada"},
			WorkspaceID:      "alpha",
			ConnectionID:     "c1",
			CatalogRevision:  testCatalogRevision,
		},
		Commands: []pmapi.CommandDescriptor{{
			Name:              "inspect-widget",
			Aliases:           []string{"read-widget"},
			ConnectorId:       "widgets",
			ConnectionId:      "c1",
			OperationId:       "op-1",
			OperationRevision: testOperationRevision,
			CatalogRevision:   testCatalogRevision,
			Fields:            fields,
			RequestBody:       contract.OperationRequestProjection{Variants: []contract.RequestVariantProjection{}},
			AvailableLanes:    []pmapi.CommandDescriptorAvailableLanes{pmapi.CommandDescriptorAvailableLanes(contract.LaneDirectRead)},
		}},
	}
}

func commandField(flag, fieldID, wireName string, location contract.FieldLocation) pmapi.CommandFieldDescriptor {
	style := "form"
	explode := true
	if location == contract.FieldLocationHeader {
		style = "simple"
		explode = false
	}
	return pmapi.CommandFieldDescriptor{
		Flag: flag,
		Binding: contract.FieldBinding{
			FieldID: fieldID, WireName: wireName, Location: location, JSONPointer: "",
			Required: true, Nullable: false, Style: style, Explode: explode, AllowReserved: false,
			Sensitivity: contract.SensitivityPublic, Schema: contract.Schema{Type: contract.SchemaTypeString},
		},
	}
}
