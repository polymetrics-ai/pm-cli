package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Go developer check, not acceptance proof (issue #366, developer check C5).
// The published schema and the runtime agree on the duration forms that the
// shared #/$defs/duration pattern refuses: a leading fraction, a trailing
// point and the two micro signs (U+00B5 and U+03BC). Each value is a whole
// number of seconds from 1 s to 60 s. Defect: a schema that refuses a value
// that the runtime accepts.
func TestDevicePollIntervalSchemaAcceptsRuntimeForms(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("..", "..", "schemas", "server-config.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	document, err := jsonschema.UnmarshalJSON(file)
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	if err := compiler.AddResource(path, document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(path + "#/properties/identity/properties/device_poll_interval")
	if err != nil {
		t.Fatalf("compile the published device poll interval schema: %v", err)
	}
	for _, value := range []string{".5m", "5.s", "1000000µs", "1000000μs"} {
		t.Run(value, func(t *testing.T) {
			if err := schema.Validate(value); err != nil {
				t.Errorf("the published schema refused %q: %v", value, err)
			}
			if _, err := loadAndValidate(withDevicePollInterval(t, `"`+value+`"`)); err != nil {
				t.Errorf("the runtime refused %q: %v", value, err)
			}
		})
	}
}
