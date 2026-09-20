package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
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

type unavailableDoer struct {
	calls int
}

func (doer *unavailableDoer) Do(*http.Request) (*http.Response, error) {
	doer.calls++
	return nil, errors.New("network unavailable")
}

func TestOfflineDocumentationRouting(t *testing.T) {
	t.Parallel()
	snapshot := commandSnapshot([]pmapi.CommandFieldDescriptor{
		commandField("--query-workspace", "query.workspace", "workspace", contract.FieldLocationQuery),
	})
	for _, test := range []struct {
		name     string
		args     []string
		catalog  *pmapi.CatalogSnapshot
		expected string
	}{
		{name: "manual", args: []string{"manual"}, expected: "COMMAND pm manual"},
		{name: "completion", args: []string{"completion", "bash"}, expected: "# PM-COMMAND pm completion"},
		{name: "help command", args: []string{"help", "completion"}},
		{name: "generated help from local authorized catalog", args: []string{"inspect-widget", "--help"}, catalog: &snapshot, expected: "--query-workspace"},
		{name: "generated help command from local authorized catalog", args: []string{"help", "inspect-widget"}, catalog: &snapshot, expected: "op-1"},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			var environmentLookups, sessionLookups int
			network := &unavailableDoer{}
			var stdout, stderr bytes.Buffer
			code := Execute(t.Context(), Options{
				Args:         test.args,
				Stdout:       &stdout,
				Stderr:       &stderr,
				ProfilesPath: filepath.Join(t.TempDir(), "missing-profiles.json"),
				LookupEnv: func(string) (string, bool) {
					environmentLookups++
					return "", false
				},
				HTTPClient: network,
				SessionStore: func(pmapi.SessionStoreMode) (pmapi.SessionStore, error) {
					sessionLookups++
					return nil, errors.New("session store unavailable")
				},
				Catalog: test.catalog,
			})
			if code != 0 || environmentLookups != 0 || sessionLookups != 0 || network.calls != 0 ||
				(test.expected != "" && !bytes.Contains(stdout.Bytes(), []byte(test.expected))) {
				t.Fatalf(
					"offline surface = code %d env %d sessions %d network %d stdout %q stderr %q",
					code,
					environmentLookups,
					sessionLookups,
					network.calls,
					stdout.String(),
					stderr.String(),
				)
			}
		})
	}
}

func TestProjectedRequestVariantSemantics(t *testing.T) {
	t.Parallel()
	selector := commandField(
		"--request-content-type",
		"request-content-type",
		contract.RequestMediaTypeWireName,
		contract.FieldLocationHeader,
	)
	selector.Binding.Required = false
	selector.Binding.Schema.Enum = []contract.PresentValue{
		contract.NewStringValue("application/json"),
		contract.NewStringValue("application/merge-patch+json"),
	}
	root := contract.Schema{
		Type:       contract.SchemaTypeObject,
		Required:   []string{"name"},
		Properties: map[string]contract.Schema{"name": {Type: contract.SchemaTypeString}},
	}
	jsonName := bodyCommandField("--json-name", "json-name", "name", true, contract.Schema{Type: contract.SchemaTypeString})
	mergeName := bodyCommandField("--merge-name", "merge-name", "name", true, contract.Schema{Type: contract.SchemaTypeString})
	snapshot := commandSnapshot([]pmapi.CommandFieldDescriptor{selector, jsonName, mergeName})
	snapshot.Commands[0].RequestBody = contract.OperationRequestProjection{
		RequestMediaFieldID: selector.Binding.FieldID,
		Variants: []contract.RequestVariantProjection{
			{
				VariantID: "json", MediaType: "application/json", Required: false,
				Schema: root, FieldIDs: []string{jsonName.Binding.FieldID},
			},
			{
				VariantID: "merge", MediaType: "application/merge-patch+json", Required: false,
				Schema: root, FieldIDs: []string{mergeName.Binding.FieldID},
			},
		},
	}

	for _, test := range []struct {
		name         string
		args         []string
		wantCode     int
		wantCalls    int
		wantField    string
		wantFieldSet bool
	}{
		{
			name:      "selected JSON variant",
			args:      []string{"--request-content-type", "application/json", "--json-name", "Ada"},
			wantField: jsonName.Binding.FieldID, wantFieldSet: true, wantCalls: 1,
		},
		{
			name:      "selected merge variant",
			args:      []string{"--request-content-type", "application/merge-patch+json", "--merge-name", "Grace"},
			wantField: mergeName.Binding.FieldID, wantFieldSet: true, wantCalls: 1,
		},
		{name: "optional body absent", wantCalls: 1},
		{
			name:     "selected variant missing required field",
			args:     []string{"--request-content-type", "application/json"},
			wantCode: 2,
		},
		{
			name:     "nonselected variant field",
			args:     []string{"--request-content-type", "application/json", "--merge-name", "Grace"},
			wantCode: 2,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			var calls int
			var captured DynamicInvocation
			var stderr bytes.Buffer
			args := append([]string{"inspect-widget"}, test.args...)
			code := Execute(t.Context(), Options{
				Args: args, Stderr: &stderr, Catalog: &snapshot,
				DynamicInvoke: func(_ context.Context, invocation DynamicInvocation) error {
					calls++
					captured = invocation
					return nil
				},
			})
			if code != test.wantCode || calls != test.wantCalls {
				t.Fatalf("variant result = code %d calls %d stderr %q; want code %d calls %d", code, calls, stderr.String(), test.wantCode, test.wantCalls)
			}
			if calls == 0 {
				return
			}
			fields, present := captured.Input.Fields()
			if !present {
				t.Fatalf("captured input = %#v; want field object", captured.Input)
			}
			_, fieldSet := fields[test.wantField]
			if fieldSet != test.wantFieldSet {
				t.Fatalf("captured fields = %#v; field %q presence = %t", fields, test.wantField, fieldSet)
			}
		})
	}
	t.Run("malformed variant membership refuses before invocation", func(t *testing.T) {
		malformed := commandSnapshot([]pmapi.CommandFieldDescriptor{selector, jsonName, mergeName})
		malformed.Commands[0].RequestBody = snapshot.Commands[0].RequestBody.Clone()
		malformed.Commands[0].RequestBody.Variants[0].FieldIDs = []string{mergeName.Binding.FieldID}
		var calls int
		code := Execute(t.Context(), Options{
			Args: []string{"inspect-widget"}, Catalog: &malformed,
			DynamicInvoke: func(context.Context, DynamicInvocation) error {
				calls++
				return nil
			},
		})
		if code != 2 || calls != 0 {
			t.Fatalf("malformed membership = code %d calls %d; want code 2 calls 0", code, calls)
		}
	})
}

func TestProjectedAllOfInputUsesSharedSemantics(t *testing.T) {
	t.Parallel()
	schema := contract.Schema{
		Type: contract.SchemaTypeObject,
		AllOf: []contract.Schema{
			{
				Type:       contract.SchemaTypeObject,
				Required:   []string{"kind"},
				Properties: map[string]contract.Schema{"kind": {Type: contract.SchemaTypeString}},
			},
			{
				Type:     contract.SchemaTypeObject,
				Required: []string{"active"},
				Properties: map[string]contract.Schema{
					"active": {Type: contract.SchemaTypeBool, Enum: []contract.PresentValue{contract.NewBooleanValue(true)}},
				},
			},
		},
	}
	field := commandField("--filter", "query.filter", "filter", contract.FieldLocationQuery)
	field.Binding.Schema = schema
	snapshot := commandSnapshot([]pmapi.CommandFieldDescriptor{field})
	for _, test := range []struct {
		name      string
		value     contract.PresentValue
		wantCode  int
		wantCalls int
	}{
		{
			name: "disjoint required properties",
			value: contract.NewObjectValue(map[string]contract.PresentValue{
				"kind": contract.NewStringValue("widget"), "active": contract.NewBooleanValue(true),
			}),
			wantCalls: 1,
		},
		{
			name: "unknown property refused",
			value: contract.NewObjectValue(map[string]contract.PresentValue{
				"kind": contract.NewStringValue("widget"), "active": contract.NewBooleanValue(true),
				"extra": contract.NewStringValue("refuse"),
			}),
			wantCode: 2,
		},
		{
			name: "branch constraint retained",
			value: contract.NewObjectValue(map[string]contract.PresentValue{
				"kind": contract.NewStringValue("widget"), "active": contract.NewBooleanValue(false),
			}),
			wantCode: 2,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			var calls int
			var stderr bytes.Buffer
			code := Execute(t.Context(), Options{
				Args:   []string{"inspect-widget", "--filter", encodeDynamicValue(t, test.value)},
				Stderr: &stderr, Catalog: &snapshot,
				DynamicInvoke: func(context.Context, DynamicInvocation) error {
					calls++
					return nil
				},
			})
			if code != test.wantCode || calls != test.wantCalls {
				t.Fatalf("allOf result = code %d calls %d stderr %q; want code %d calls %d", code, calls, stderr.String(), test.wantCode, test.wantCalls)
			}
		})
	}
}

func bodyCommandField(flag, fieldID, wireName string, required bool, schema contract.Schema) pmapi.CommandFieldDescriptor {
	return pmapi.CommandFieldDescriptor{
		Flag: flag,
		Binding: contract.FieldBinding{
			FieldID: fieldID, WireName: wireName, Location: contract.FieldLocationBody, JSONPointer: "/" + wireName,
			Required: required, Nullable: schema.Nullable, Style: "json", Explode: false, AllowReserved: false,
			Sensitivity: contract.SensitivityPublic, Schema: schema,
		},
	}
}

func encodeDynamicValue(t *testing.T, value contract.PresentValue) string {
	t.Helper()
	document, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode typed dynamic value: %v", err)
	}
	return string(document)
}
