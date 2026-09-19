package compile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestDialectAndReferenceBoundary(t *testing.T) {
	t.Run("C01 implicit remote reference", func(t *testing.T) {
		document := []byte(`openapi: 3.0.3
info: {title: remote, version: 1.0.0}
paths:
  /x:
    get:
      operationId: getX
      responses:
        "200":
          description: x
          content:
            application/json:
              schema: {$ref: "https://example.invalid/schema.yaml#/X"}
`)
		captured := testCapturedSource(t, DialectOpenAPI30, map[string][]byte{"root": document}, nil, []SourceOperation{testOperation("getX", "root", "#/paths/~1x/get", nil, []VariantRef{{Pointer: "#/paths/~1x/get/responses/200/content/application~1json", Status: "200", MediaType: "application/json"}})})
		if _, err := ResolveRefs(captured); err == nil {
			t.Fatal("implicit remote reference was accepted")
		}
	})

	t.Run("C02 Swagger conversion provenance", func(t *testing.T) {
		document := testFixture(t, "swagger2.json")
		captured := testCapturedSource(t, DialectSwagger20, map[string][]byte{"swagger": document}, nil, []SourceOperation{testOperation("getThings", "swagger", "#/paths/~1things/get", nil, []VariantRef{{Pointer: "#/paths/~1things/get/responses/200", Status: "200", MediaType: "application/json"}})})
		resolved := testResolve(t, captured)
		loaded, err := LoadDialects(context.Background(), resolved)
		if err != nil {
			t.Fatal(err)
		}
		if len(loaded.Documents) != 1 || !loaded.Documents[0].Provenance.Converted || loaded.Documents[0].Provenance.SourceDialect != DialectSwagger20 || loaded.Documents[0].Provenance.OriginalPointer != "#" {
			t.Fatalf("conversion provenance = %#v", loaded.Documents)
		}
	})

	t.Run("C03 allOf branch preservation", func(t *testing.T) {
		document := testFixture(t, "allof-openapi.yaml")
		operation := testOperation("getObjects", "allof", "#/paths/~1objects/get", nil, []VariantRef{{Pointer: "#/paths/~1objects/get/responses/200/content/application~1json", Status: "200", MediaType: "application/json"}})
		normalized := testNormalize(t, DialectOpenAPI30, map[string][]byte{"allof": document}, nil, []SourceOperation{operation})
		schema := normalized.Operations[0].ResponseVariants[0].Schema
		if schema == nil || len(schema.AllOf) != 2 || !slices.Contains(schema.AllOf[0].Required, "id") {
			t.Fatalf("allOf schema = %#v", schema)
		}
		facts := normalized.Operations[0].ResponseVariants[0].SchemaFacts
		if len(facts) < 3 || len(facts[0].AllOfPointers) != 2 || facts[0].AllOfPointers[0] == facts[0].AllOfPointers[1] {
			t.Fatalf("allOf source facts = %#v", facts)
		}
	})

	t.Run("C04 captured local recursive reference", func(t *testing.T) {
		root := testFixture(t, "recursive-root.yaml")
		child := testFixture(t, "recursive-child.yaml")
		references := map[string][]SourceReference{
			"root":  {{FromPointer: "/paths/~1nodes/get/responses/200/content/application~1json/schema/$ref", Reference: "child.yaml#/$defs/Node", TargetSourceID: "child", TargetPointer: "/$defs/Node"}},
			"child": {{FromPointer: "/$defs/Node/properties/next/$ref", Reference: "#/$defs/Node", TargetSourceID: "child", TargetPointer: "/$defs/Node"}},
		}
		operation := testOperation("getNodes", "root", "#/paths/~1nodes/get", nil, []VariantRef{{Pointer: "#/paths/~1nodes/get/responses/200/content/application~1json", Status: "200", MediaType: "application/json"}})
		captured := testCapturedSource(t, DialectOpenAPI31, map[string][]byte{"root": root, "child": child}, references, []SourceOperation{operation})
		first := testResolve(t, captured)
		second := testResolve(t, captured)
		firstRoot, rootOK := first.Document("root")
		secondRoot, secondOK := second.Document("root")
		childDocument, childOK := first.Document("child")
		if !rootOK || !secondOK || !childOK || string(firstRoot.RewrittenJSON) != string(secondRoot.RewrittenJSON) ||
			len(firstRoot.References)+len(childDocument.References) != 2 {
			t.Fatalf("reference resolution was not deterministic: root=%#v child=%#v", firstRoot.References, childDocument.References)
		}
		recursive := false
		for _, reference := range childDocument.References {
			recursive = recursive || reference.Recursive
		}
		if !recursive {
			t.Fatal("recursive identity was not retained")
		}
		references["child"][0].TargetPointer = "/$defs/Node/properties/next"
		contradictory := testCapturedSource(t, DialectOpenAPI31, map[string][]byte{"root": root, "child": child}, references, []SourceOperation{operation})
		if _, err := ResolveRefs(contradictory); err == nil {
			t.Fatal("a captured declaration redirected a source-local reference")
		}
	})

	t.Run("C05 missing captured reference", func(t *testing.T) {
		document := []byte(`openapi: 3.0.3
info: {title: missing, version: 1.0.0}
paths:
  /x:
    get:
      operationId: getX
      responses:
        "200": {description: x, content: {application/json: {schema: {$ref: "missing.yaml#/X"}}}}
`)
		captured := testCapturedSource(t, DialectOpenAPI30, map[string][]byte{"root": document}, nil, []SourceOperation{testOperation("getX", "root", "#/paths/~1x/get", nil, []VariantRef{{Pointer: "#/paths/~1x/get/responses/200/content/application~1json", Status: "200", MediaType: "application/json"}})})
		if _, err := ResolveRefs(captured); err == nil || !strings.Contains(err.Error(), "/paths/~1x/get/responses/200/content/application~1json/schema/$ref") {
			t.Fatalf("missing reference error = %v", err)
		}
	})

	t.Run("C06 unsupported dialect", func(t *testing.T) {
		document := testFixture(t, "complete-openapi.yaml")
		operation := testOperation("getWidgets", "root", "#/paths/~1widgets/get", nil, []VariantRef{{Pointer: "#/paths/~1widgets/get/responses/204", Status: "204", MediaType: noContentMediaType}})
		lock, captures := testSourceInput(t, Dialect("openapi-3.2"), map[string][]byte{"root": document}, nil, []SourceOperation{operation})
		if _, err := CaptureSource(lock, captures); err == nil {
			t.Fatal("unsupported dialect was accepted")
		}
	})

	t.Run("C07 duplicate YAML key", func(t *testing.T) {
		if _, err := parseCapturedDocument(CapturedDocument{Bytes: []byte("openapi: 3.0.3\nopenapi: 3.1.0\n")}); err == nil {
			t.Fatal("duplicate YAML key was accepted")
		}
	})

	t.Run("C08 unsupported YAML tag", func(t *testing.T) {
		if _, err := parseCapturedDocument(CapturedDocument{Bytes: []byte("value: !execute echo\n")}); err == nil {
			t.Fatal("unsupported YAML tag was accepted")
		}
	})

	t.Run("C09 alias budget", func(t *testing.T) {
		var fixture strings.Builder
		fixture.WriteString("base: &base {value: x}\nitems:\n")
		for range maxYAMLAliases + 1 {
			fixture.WriteString("  - *base\n")
		}
		if _, err := parseCapturedDocument(CapturedDocument{Bytes: []byte(fixture.String())}); err == nil {
			t.Fatal("over-budget YAML aliases were accepted")
		}
	})

	t.Run("C10 status media and serialization", func(t *testing.T) {
		document := testFixture(t, "complete-openapi.yaml")
		operation := testOperation("getWidgets", "root", "#/paths/~1widgets/get", nil, []VariantRef{
			{Pointer: "#/paths/~1widgets/get/responses/200/content/application~1json", Status: "200", MediaType: "application/json"},
			{Pointer: "#/paths/~1widgets/get/responses/204", Status: "204", MediaType: noContentMediaType},
			{Pointer: "#/paths/~1widgets/get/responses/default/content/application~1problem+json", Status: "default", MediaType: "application/problem+json"},
		})
		normalized := testNormalize(t, DialectOpenAPI30, map[string][]byte{"root": document}, nil, []SourceOperation{operation})
		actual := normalized.Operations[0]
		if len(actual.ResponseVariants) != 3 || actual.ResponseVariants[1].Status != "204" || !actual.ResponseVariants[1].NoContent {
			t.Fatalf("responses = %#v", actual.ResponseVariants)
		}
		if len(actual.CommonBindings) != 1 || actual.CommonBindings[0].RawStyle != "form" || actual.CommonBindings[0].RawExplode {
			t.Fatalf("query serialization = %#v", actual.CommonBindings)
		}
	})
}

func testFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func testOperation(id, sourceID, pointer string, requests, responses []VariantRef) SourceOperation {
	if requests == nil {
		requests = []VariantRef{{Pointer: pointer, MediaType: parameterOnlyMediaType}}
	}
	return SourceOperation{
		SourceOperationID: id,
		SourceRefs:        []SourceRef{{SourceID: sourceID, Pointer: pointer}},
		RequestVariants:   requests,
		ResponseVariants:  responses,
	}
}

func testSourceInput(t *testing.T, dialect Dialect, documents map[string][]byte, references map[string][]SourceReference, operations []SourceOperation) (SourceLock, Captures) {
	t.Helper()
	license := testFixture(t, "LICENSE.txt")
	licenseHash := sha256.Sum256(license)
	ids := make([]string, 0, len(documents))
	for id := range documents {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	lock := SourceLock{
		SchemaVersion:     SourceLockSchemaVersion,
		ConnectorID:       "synthetic",
		SourceSet:         SourceSet{Dialect: dialect, ReferencesClosed: true},
		Operations:        operations,
		SemanticsRevision: "sha256:" + strings.Repeat("a", 64),
	}
	captures := Captures{Documents: map[string][]byte{}, Licenses: map[string][]byte{}}
	for _, id := range ids {
		data := documents[id]
		digest := sha256.Sum256(data)
		lock.SourceSet.Documents = append(lock.SourceSet.Documents, SourceDocument{
			SourceID: id, OfficialURL: "https://example.invalid/" + id, Path: id,
			CapturedAt: "2026-09-18T00:00:00Z", ByteLength: int64(len(data)), SHA256: hex.EncodeToString(digest[:]),
			License:    SourceLicense{SPDX: "MIT", Path: "LICENSE.txt", SHA256: hex.EncodeToString(licenseHash[:])},
			References: slices.Clone(references[id]),
		})
		captures.Documents[id] = slices.Clone(data)
		captures.Licenses[id] = slices.Clone(license)
	}
	return lock, captures
}

func testCapturedSource(t *testing.T, dialect Dialect, documents map[string][]byte, references map[string][]SourceReference, operations []SourceOperation) *CapturedSource {
	t.Helper()
	lock, captures := testSourceInput(t, dialect, documents, references, operations)
	captured, err := CaptureSource(lock, captures)
	if err != nil {
		t.Fatal(err)
	}
	return captured
}

func testResolve(t *testing.T, captured *CapturedSource) *ResolvedSource {
	t.Helper()
	resolved, err := ResolveRefs(captured)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func testNormalize(t *testing.T, dialect Dialect, documents map[string][]byte, references map[string][]SourceReference, operations []SourceOperation) *Normalization {
	t.Helper()
	resolved := testResolve(t, testCapturedSource(t, dialect, documents, references, operations))
	loaded, err := LoadDialects(context.Background(), resolved)
	if err != nil {
		t.Fatal(err)
	}
	normalization, err := Normalize(context.Background(), loaded)
	if err != nil {
		t.Fatal(err)
	}
	return normalization
}

func FuzzSourceCanonicalization(f *testing.F) {
	for _, seed := range []string{
		`{"integer":9007199254740993,"decimal":1.2300,"absent":null,"enabled":false}`,
		"base: &base {value: 0}\ncopy: *base\n",
		"value: !execute echo\n",
		"value: one\nvalue: two\n",
		"items: [null, false, 0, \"\", {}, []]\n",
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 32<<10 {
			return
		}
		first, err := parseCapturedDocument(CapturedDocument{Bytes: input})
		if err != nil {
			return
		}
		second, err := parseCapturedDocument(CapturedDocument{Bytes: first.canonical})
		if err != nil {
			t.Fatalf("canonical source was refused: %v", err)
		}
		if !bytes.Equal(first.canonical, second.canonical) {
			t.Fatal("canonical source changed exact values after reparsing")
		}
	})
}
