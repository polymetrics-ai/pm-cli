package cli

import (
	"bytes"
	"context"
	"testing"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestSelectorTreeAndOfflineHelp(t *testing.T) {
	t.Parallel()
	snapshot := commandSnapshot([]pmapi.CommandFieldDescriptor{commandField("--query-workspace", "query.workspace", "workspace", contract.FieldLocationQuery)})

	t.Run("duplicate PM selector refuses before dispatch", func(t *testing.T) {
		var calls int
		status := Execute(t.Context(), Options{
			Args:    []string{"--workspace", "alpha", "--workspace", "beta", "inspect-widget", "--query-workspace", "provider"},
			Stderr:  &bytes.Buffer{},
			Catalog: &snapshot,
			DynamicInvoke: func(context.Context, DynamicInvocation) error {
				calls++
				return nil
			},
		})
		if status != 2 || calls != 0 {
			t.Fatalf("duplicate selector result = (%d, %d calls), want usage refusal before dispatch", status, calls)
		}
	})

	t.Run("tokens after terminator cannot switch PM scope", func(t *testing.T) {
		var calls int
		status := Execute(t.Context(), Options{
			Args:    []string{"--workspace", "alpha", "inspect-widget", "--query-workspace", "provider", "--", "--workspace", "beta"},
			Stderr:  &bytes.Buffer{},
			Catalog: &snapshot,
			DynamicInvoke: func(context.Context, DynamicInvocation) error {
				calls++
				return nil
			},
		})
		if status != 2 || calls != 0 {
			t.Fatalf("post-terminator selector result = (%d, %d calls), want argument refusal without dispatch", status, calls)
		}
	})

	for _, commandName := range []string{"inspect-widget", "read-widget"} {
		commandName := commandName
		t.Run(commandName+" preserves operation identity", func(t *testing.T) {
			var captured DynamicInvocation
			root := NewRoot(Options{
				Args:    []string{"--workspace", "alpha", commandName, "--query-workspace", "provider"},
				Catalog: &snapshot,
				DynamicInvoke: func(_ context.Context, invocation DynamicInvocation) error {
					captured = invocation
					return nil
				},
			})
			if err := root.ExecuteContext(t.Context()); err != nil {
				t.Fatalf("execute %s: %v", commandName, err)
			}
			fields, present := captured.Input.Fields()
			workspace, text := fields["query.workspace"].TextValue()
			if !present || !text || workspace != "provider" {
				t.Fatalf("provider workspace binding = %#v", captured.Input)
			}
			if captured.Descriptor.OperationId != "op-1" || captured.Descriptor.OperationRevision != testOperationRevision ||
				captured.Binding.WorkspaceID != "alpha" {
				t.Fatalf("friendly command changed stable identity: %#v", captured)
			}
		})
	}
	t.Run("offline help has no execution configuration", testInjectedCatalogHelpIsOffline)
}

func testInjectedCatalogHelpIsOffline(t *testing.T) {
	snapshot := commandSnapshot([]pmapi.CommandFieldDescriptor{commandField("--query-workspace", "query.workspace", "workspace", contract.FieldLocationQuery)})
	var calls int
	output := &bytes.Buffer{}
	root := NewRoot(Options{
		Args:    []string{"inspect-widget", "--help"},
		Stdout:  output,
		Stderr:  &bytes.Buffer{},
		Catalog: &snapshot,
		DynamicInvoke: func(context.Context, DynamicInvocation) error {
			calls++
			return nil
		},
	})
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("render injected command help without configuration: %v", err)
	}
	if calls != 0 || !bytes.Contains(output.Bytes(), []byte("--query-workspace")) || !bytes.Contains(output.Bytes(), []byte("op-1")) {
		t.Fatalf("offline help = %q with %d invocations", output.String(), calls)
	}
}
