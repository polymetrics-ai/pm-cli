package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	pmcompile "github.com/polymetrics-ai/pm-cli/internal/compile"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const (
	fixtureConnectorID       = "synthetic"
	fixtureSourceID          = "root"
	fixtureOperationID       = "listItems"
	fixtureScenarioID        = "synthetic-read"
	fixtureSemanticsRevision = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
)

// fixtureNoContentMediaType is the compiler's declared media type for a
// response that declares no content (internal/compile/normalize.go).
const fixtureNoContentMediaType = "application/x.pm.no-content"

// fixtureSourceDocument mirrors the admitted OpenAPI 3.0 shape of the compiler's
// complete-inventory fixture: one read-only operation with a bounded JSON record
// array, one no-content response and one problem response.
var fixtureSourceDocument = []byte(`openapi: 3.0.3
info:
  title: Synthetic connector source
  version: 1.0.0
servers:
  - url: https://api.synthetic.example.test/v1
paths:
  /items:
    get:
      operationId: listItems
      parameters:
        - name: tags
          in: query
          required: true
          style: form
          explode: false
          schema:
            type: array
            items:
              type: string
      responses:
        "200":
          description: items
          content:
            application/json:
              schema:
                type: object
                additionalProperties: false
                required: [items]
                properties:
                  items:
                    type: array
                    items:
                      type: object
                      additionalProperties: false
                      required: [id]
                      properties:
                        id:
                          type: string
        "204":
          description: no content
        default:
          description: problem
          content:
            application/problem+json:
              schema:
                type: object
                additionalProperties: false
                required: [title]
                properties:
                  title:
                    type: string
`)

var fixtureLicense = []byte(`MIT License

Copyright (c) 2026 Synthetic Connector Authors

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction.
`)

func TestConnectorGenerationWritesDeclaredOutputs(t *testing.T) {
	root := t.TempDir()
	writeConnectorFixture(t, root)

	status, stdout, stderr := runFixtureDriver(t, defaultConnectorDependencies(), "--root", root)
	if status != 0 {
		t.Fatalf("connector generation status = %d; stderr: %s", status, stderr)
	}
	report := decodeConnectorReport(t, stdout)
	if report.Check || len(report.Connectors) != 1 {
		t.Fatalf("compile report = %#v", report)
	}
	entry := report.Connectors[0]
	if entry.ConnectorID != fixtureConnectorID {
		t.Fatalf("reported connector = %q", entry.ConnectorID)
	}
	if entry.SourceDigest == "" || entry.SemanticsRevision != fixtureSemanticsRevision {
		t.Fatalf("reported identity = %#v", entry)
	}
	if got, want := reportedOutputPaths(entry), expectedOutputPaths(); !slices.Equal(got, want) {
		t.Fatalf("reported outputs = %v; want %v", got, want)
	}
	for _, output := range entry.Outputs {
		if output.Status != "written" {
			t.Fatalf("output %s status = %q", output.Path, output.Status)
		}
		assertPublishedFile(t, root, output)
	}
	assertFixtureTree(t, root)
	assertLaneInventory(t, root)
}

func TestConnectorGenerationIsDeterministicAndCheckable(t *testing.T) {
	root := t.TempDir()
	writeConnectorFixture(t, root)

	if status, _, stderr := runFixtureDriver(t, defaultConnectorDependencies(), "--root", root); status != 0 {
		t.Fatalf("first generation status = %d; stderr: %s", status, stderr)
	}
	first := digestFixtureTree(t, root)
	if status, _, stderr := runFixtureDriver(t, defaultConnectorDependencies(), "--root", root); status != 0 {
		t.Fatalf("second generation status = %d; stderr: %s", status, stderr)
	}
	second := digestFixtureTree(t, root)
	if !maps.Equal(first, second) {
		t.Fatalf("identical pinned inputs produced different bytes:\nfirst:  %v\nsecond: %v", first, second)
	}

	status, stdout, stderr := runFixtureDriver(t, defaultConnectorDependencies(), "--root", root, "--check")
	if status != 0 {
		t.Fatalf("check status = %d; stderr: %s", status, stderr)
	}
	report := decodeConnectorReport(t, stdout)
	if !report.Check || len(report.Connectors) != 1 {
		t.Fatalf("check report = %#v", report)
	}
	for _, output := range report.Connectors[0].Outputs {
		if output.Status != "unchanged" {
			t.Fatalf("check status for %s = %q", output.Path, output.Status)
		}
	}
}

func TestConnectorCheckDetectsDriftAndMissingOutputs(t *testing.T) {
	root := t.TempDir()
	writeConnectorFixture(t, root)
	if status, _, stderr := runFixtureDriver(t, defaultConnectorDependencies(), "--root", root); status != 0 {
		t.Fatalf("generation status = %d; stderr: %s", status, stderr)
	}
	driftedPath := filepath.Join(root, "connectors", fixtureConnectorID, "lanes", "direct_read.json")
	drifted, err := os.ReadFile(driftedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(driftedPath, append(drifted, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	missingPath := filepath.Join(root, "internal", "connectors", "data", fixtureConnectorID+".report.json")
	if err := os.Remove(missingPath); err != nil {
		t.Fatal(err)
	}

	status, _, stderr := runFixtureDriver(t, defaultConnectorDependencies(), "--root", root, "--check")
	if status != 1 {
		t.Fatalf("check status = %d; want drift refusal; stderr: %s", status, stderr)
	}
	for _, want := range []string{
		"connectors/" + fixtureConnectorID + "/lanes/direct_read.json",
		"internal/connectors/data/" + fixtureConnectorID + ".report.json",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("check stderr = %q; want reported drift for %s", stderr, want)
		}
	}
	unchanged, err := os.ReadFile(driftedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(unchanged, append(drifted, '\n')) {
		t.Fatal("check mode rewrote a drifted artifact instead of only reporting it")
	}
	if _, err := os.Stat(missingPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("check mode recreated a missing artifact instead of only reporting it")
	}
}

func TestConnectorRefusesCaptureThatDoesNotMatchItsLock(t *testing.T) {
	root := t.TempDir()
	writeConnectorFixture(t, root)
	capturePath := filepath.Join(root, "connectors", fixtureConnectorID, "captures", fixtureSourceID)
	captured, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(capturePath, append(captured, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	status, _, stderr := runFixtureDriver(t, defaultConnectorDependencies(), "--root", root)
	if status != 1 {
		t.Fatalf("status = %d; want refusal for a capture that contradicts its lock; stderr: %s", status, stderr)
	}
	if !strings.Contains(stderr, "source lock") {
		t.Fatalf("stderr = %q; want the capture identity contradiction", stderr)
	}
	assertOutputsAbsent(t, root)
}

func TestConnectorRefusesInvalidAuthoredScenarios(t *testing.T) {
	scenariosPath := func(root string) string {
		return filepath.Join(root, "connectors", fixtureConnectorID, "scenarios", "lifecycle.json")
	}
	cases := []struct {
		name    string
		mutate  func(t *testing.T, root string)
		message string
	}{
		{
			name: "missing scenarios directory",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				if err := os.RemoveAll(filepath.Dir(scenariosPath(root))); err != nil {
					t.Fatal(err)
				}
			},
			message: "scenarios directory is missing",
		},
		{
			name: "no authored scenario document",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				if err := os.Remove(scenariosPath(root)); err != nil {
					t.Fatal(err)
				}
			},
			message: "no authored scenario document",
		},
		{
			name: "unknown scenario member",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				mutateScenarioDocument(t, scenariosPath(root), func(document []map[string]any) []map[string]any {
					document[0]["unexpected"] = true
					return document
				})
			},
			message: "scenario document lifecycle.json",
		},
		{
			name: "missing scenario member",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				mutateScenarioDocument(t, scenariosPath(root), func(document []map[string]any) []map[string]any {
					delete(document[0], "assertions")
					return document
				})
			},
			message: "scenario document lifecycle.json",
		},
		{
			name: "duplicate scenario identity",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				mutateScenarioDocument(t, scenariosPath(root), func(document []map[string]any) []map[string]any {
					return append(document, document[0])
				})
			},
			message: "duplicated",
		},
		{
			name: "scenario of another connector",
			mutate: func(t *testing.T, root string) {
				t.Helper()
				mutateScenarioDocument(t, scenariosPath(root), func(document []map[string]any) []map[string]any {
					document[0]["connector_id"] = "other"
					return document
				})
			},
			message: "belongs to connector",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeConnectorFixture(t, root)
			test.mutate(t, root)

			status, _, stderr := runFixtureDriver(t, defaultConnectorDependencies(), "--root", root)
			if status != 1 {
				t.Fatalf("status = %d; want refusal; stderr: %s", status, stderr)
			}
			if !strings.Contains(stderr, test.message) {
				t.Fatalf("stderr = %q; want %q", stderr, test.message)
			}
			assertOutputsAbsent(t, root)
		})
	}
}

func TestConnectorRefusesInvalidLaneArtifacts(t *testing.T) {
	stubLanes := func(mutate func(map[string][]byte)) connectorLaneEmitter {
		return func([]byte, []contract.Scenario) (map[string][]byte, error) {
			emitted := make(map[string][]byte, len(connectorLanes)*2+1)
			for _, lane := range connectorLanes {
				emitted["lanes/"+string(lane)+".json"] = []byte("{}\n")
				emitted["scripts/"+string(lane)+".sh"] = []byte("#!/bin/sh\n")
			}
			mutate(emitted)
			return emitted, nil
		}
	}
	cases := []struct {
		name    string
		mutate  func(map[string][]byte)
		message string
		escaped string
	}{
		{
			name: "unknown lane artifact",
			mutate: func(emitted map[string][]byte) {
				emitted["lanes/synthetic_lane.json"] = []byte("{}\n")
			},
			message: "unknown artifact",
		},
		{
			name: "omitted lane artifact",
			mutate: func(emitted map[string][]byte) {
				delete(emitted, "scripts/direct_read.sh")
			},
			message: "omitted 1 canonical lane artifact",
		},
		{
			name: "empty lane artifact",
			mutate: func(emitted map[string][]byte) {
				emitted["lanes/etl.json"] = nil
			},
			message: "empty artifact",
		},
		{
			name: "artifact outside the declared tree",
			mutate: func(emitted map[string][]byte) {
				emitted["../../escaped.json"] = []byte("{}\n")
			},
			message: "unknown artifact",
			escaped: "escaped.json",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeConnectorFixture(t, root)
			dependencies := defaultConnectorDependencies()
			dependencies.lanes = stubLanes(test.mutate)

			status, _, stderr := runFixtureDriver(t, dependencies, "--root", root)
			if status != 1 {
				t.Fatalf("status = %d; want refusal; stderr: %s", status, stderr)
			}
			if !strings.Contains(stderr, test.message) {
				t.Fatalf("stderr = %q; want %q", stderr, test.message)
			}
			assertOutputsAbsent(t, root)
			if test.escaped != "" {
				if _, err := os.Stat(filepath.Join(root, test.escaped)); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("a lane artifact escaped the connector root: %s", test.escaped)
				}
			}
		})
	}
}

func TestConnectorRefusesSymlinkedOutputDirectory(t *testing.T) {
	root := t.TempDir()
	writeConnectorFixture(t, root)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal", "connectors"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "internal", "connectors", "data")); err != nil {
		t.Fatal(err)
	}

	status, _, stderr := runFixtureDriver(t, defaultConnectorDependencies(), "--root", root)
	if status != 1 {
		t.Fatalf("status = %d; want refusal for an output directory outside the root; stderr: %s", status, stderr)
	}
	if !strings.Contains(stderr, "resolves outside") {
		t.Fatalf("stderr = %q; want the resolved-directory refusal", stderr)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("output was written outside the repository root: %v", entryNames(entries))
	}
}

func TestConnectorRefusesDuplicateConnectorRoot(t *testing.T) {
	root := t.TempDir()
	writeConnectorFixture(t, root)
	copyConnectorRoot(t, root, fixtureConnectorID, fixtureConnectorID+"-copy")

	status, _, stderr := runFixtureDriver(t, defaultConnectorDependencies(), "--root", root)
	if status != 1 {
		t.Fatalf("status = %d; want refusal for a duplicated connector root; stderr: %s", status, stderr)
	}
	if !strings.Contains(stderr, "duplicate") {
		t.Fatalf("stderr = %q; want the duplicate connector refusal", stderr)
	}
	assertOutputsAbsent(t, root)
}

func TestConnectorOptionsAndSelectionAreRefused(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "connectors"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		args    []string
		status  int
		message string
	}{
		{
			name:    "missing root",
			args:    []string{},
			status:  2,
			message: "root is required",
		},
		{
			name:    "positional argument",
			args:    []string{"--root", root, "extra"},
			status:  2,
			message: "unexpected positional arguments",
		},
		{
			name:    "duplicate selection",
			args:    []string{"--root", root, "--connector", fixtureConnectorID, "--connector", fixtureConnectorID},
			status:  2,
			message: "duplicated",
		},
		{
			name:    "identity outside one path component",
			args:    []string{"--root", root, "--connector", "../escape"},
			status:  2,
			message: "must be one path component",
		},
		{
			name:    "unknown connector root",
			args:    []string{"--root", root, "--connector", fixtureConnectorID},
			status:  1,
			message: "has no connectors/" + fixtureConnectorID + " root",
		},
		{
			name:    "absent repository root",
			args:    []string{"--root", filepath.Join(root, "absent")},
			status:  1,
			message: "read root",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			status, _, stderr := runFixtureDriver(t, defaultConnectorDependencies(), test.args...)
			if status != test.status {
				t.Fatalf("status = %d; want %d; stderr: %s", status, test.status, stderr)
			}
			if !strings.Contains(stderr, test.message) {
				t.Fatalf("stderr = %q; want %q", stderr, test.message)
			}
		})
	}
}

func TestDeclaredConnectorOutputScope(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "repo")
	admitted := []string{
		filepath.Join(root, "internal", "connectors", "data", "github.bundle.json"),
		filepath.Join(root, "internal", "connectors", "data", "github.report.json"),
		filepath.Join(root, "connectors", "github", "lanes", "direct_read.json"),
		filepath.Join(root, "connectors", "github", "scripts", "sync_transport.sh"),
	}
	for _, path := range admitted {
		if !isDeclaredConnectorOutput(root, "github", path) {
			t.Fatalf("declared output %s was refused", path)
		}
	}
	refused := []string{
		filepath.Join(root, "internal", "connectors", "data", "other.bundle.json"),
		filepath.Join(root, "connectors", "github", "lanes", "synthetic_lane.json"),
		filepath.Join(root, "connectors", "other", "lanes", "direct_read.json"),
		filepath.Join(root, "connectors", "github", "lanes", "direct_read.json.bak"),
		filepath.Join(root, "connectors", "github", "lanes", "direct_read.json", "extra"),
		filepath.Join(root, "..", "escape.json"),
	}
	for _, path := range refused {
		if isDeclaredConnectorOutput(root, "github", path) {
			t.Fatalf("undeclared output %s was admitted", path)
		}
	}
}

// writeConnectorFixture writes one complete connector root: a pinned OpenAPI 3.0
// capture with its license, one source operation, a source-linked semantic
// amendment and one authored scenario. The lock records the exact capture bytes
// so the driver and the compiler verify the same identity.
func writeConnectorFixture(t *testing.T, root string) {
	t.Helper()
	connectorRoot := filepath.Join(root, "connectors", fixtureConnectorID)
	for _, name := range []string{"captures", "scenarios"} {
		if err := os.MkdirAll(filepath.Join(connectorRoot, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sourceDigest := sha256.Sum256(fixtureSourceDocument)
	licenseDigest := sha256.Sum256(fixtureLicense)
	lock := pmcompile.SourceLock{
		SchemaVersion: pmcompile.SourceLockSchemaVersion,
		ConnectorID:   fixtureConnectorID,
		SourceSet: pmcompile.SourceSet{
			Dialect: pmcompile.DialectOpenAPI30,
			Documents: []pmcompile.SourceDocument{{
				SourceID:    fixtureSourceID,
				OfficialURL: "https://synthetic.example.test/openapi.yaml",
				Path:        "openapi.yaml",
				CapturedAt:  "2026-09-19T00:00:00Z",
				ByteLength:  int64(len(fixtureSourceDocument)),
				SHA256:      hex.EncodeToString(sourceDigest[:]),
				License: pmcompile.SourceLicense{
					SPDX:           "MIT",
					Path:           "connectors/" + fixtureConnectorID + "/captures/LICENSE.md",
					SHA256:         hex.EncodeToString(licenseDigest[:]),
					Copyright:      "Copyright (c) 2026 Synthetic Connector Authors",
					NoticeRequired: true,
				},
			}},
			ReferencesClosed: true,
		},
		Operations: []pmcompile.SourceOperation{{
			SourceOperationID: fixtureOperationID,
			SourceRefs:        []contract.SourceRef{{SourceID: fixtureSourceID, Pointer: "#/paths/~1items/get"}},
			RequestVariants: []pmcompile.VariantRef{{
				Pointer:   "#/paths/~1items/get",
				MediaType: "application/x.pm.parameters",
			}},
			ResponseVariants: []pmcompile.VariantRef{
				{
					Pointer:   "#/paths/~1items/get/responses/200/content/application~1json",
					Status:    "200",
					MediaType: "application/json",
				},
				{
					Pointer:   "#/paths/~1items/get/responses/204",
					Status:    "204",
					MediaType: fixtureNoContentMediaType,
				},
				{
					Pointer:   "#/paths/~1items/get/responses/default/content/application~1problem+json",
					Status:    "default",
					MediaType: "application/problem+json",
				},
			},
		}},
		SemanticsRevision: fixtureSemanticsRevision,
	}
	lockDocument, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := pmcompile.DecodeSourceLock(lockDocument)
	if err != nil {
		t.Fatalf("fixture source lock is not decodable: %v", err)
	}
	captures := pmcompile.Captures{
		Documents: map[string][]byte{fixtureSourceID: fixtureSourceDocument},
		Licenses:  map[string][]byte{fixtureSourceID: fixtureLicense},
	}
	captured, err := pmcompile.CaptureSource(decoded, captures)
	if err != nil {
		t.Fatalf("fixture capture is not admitted: %v", err)
	}
	semantics := pmcompile.SemanticsDocument{
		SchemaVersion: pmcompile.SemanticsSchemaVersion,
		ConnectorID:   fixtureConnectorID,
		SourceDigest:  captured.Digest,
		Revision:      fixtureSemanticsRevision,
		Operations: []pmcompile.OperationSemantics{{
			OperationID: fixtureOperationID,
			SourceRefs:  []contract.SourceRef{{SourceID: fixtureSourceID, Pointer: "#/paths/~1items/get"}},
			Lanes: []pmcompile.LaneSemantics{{
				Lane:           contract.LaneDirectRead,
				Classification: pmcompile.LaneEligible,
				Reason:         "authored fixture: read-only GET with a declared bounded JSON record array",
				SourceRefs:     []contract.SourceRef{{SourceID: fixtureSourceID, Pointer: "#/paths/~1items/get"}},
			}},
		}},
	}
	semanticsDocument, err := json.Marshal(semantics)
	if err != nil {
		t.Fatal(err)
	}
	scenario := contract.Scenario{
		ScenarioVersion: "pm.scenario/v1",
		ScenarioID:      fixtureScenarioID,
		ConnectorID:     fixtureConnectorID,
		Lane:            contract.LaneDirectRead,
		OperationID:     fixtureOperationID,
		SourceRefs:      []contract.SourceRef{{SourceID: fixtureSourceID, Pointer: "#/paths/~1items/get"}},
		Requirements:    []contract.ScenarioRequirement{},
		Steps: []contract.ScenarioStep{{
			StepID:    "read_page",
			Kind:      "invoke",
			DependsOn: []string{},
			Action:    fixtureOperationID,
			Input:     json.RawMessage(`{"query.tags":["synthetic"]}`),
			Captures:  []contract.ScenarioCapture{},
		}},
		Assertions: []contract.ScenarioAssertion{{
			AssertionID: "data-is-record-array",
			StepID:      "read_page",
			Kind:        "equals",
			Pointer:     "/data/type",
			Expected:    json.RawMessage(`"array"`),
		}},
		Cleanup: contract.ScenarioCleanup{
			Always:          true,
			OwnedIDCaptures: []string{},
			Steps:           []contract.ScenarioStep{},
		},
	}
	scenarioDocument, err := json.Marshal([]contract.Scenario{scenario})
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(connectorRoot, "captures", fixtureSourceID), fixtureSourceDocument)
	writeFixtureFile(t, filepath.Join(connectorRoot, "captures", "LICENSE.md"), fixtureLicense)
	writeFixtureFile(t, filepath.Join(connectorRoot, "source.lock.json"), lockDocument)
	writeFixtureFile(t, filepath.Join(connectorRoot, "semantics.json"), semanticsDocument)
	writeFixtureFile(t, filepath.Join(connectorRoot, "scenarios", "lifecycle.json"), scenarioDocument)

	// Report the compiler's own diagnostic when the synthetic fixture drifts
	// away from an admitted shape instead of tripping every driver assertion.
	if _, err := pmcompile.Compile(context.Background(), pmcompile.CompilationInput{
		SourceLock: lockDocument,
		Captures:   captures,
		Semantics:  semanticsDocument,
	}); err != nil {
		t.Fatalf("fixture connector does not compile: %v", err)
	}
}

func runFixtureDriver(t *testing.T, dependencies connectorDependencies, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	status := runWith(context.Background(), append([]string{"connector"}, args...), &stdout, &stderr, dependencies)
	return status, stdout.String(), stderr.String()
}

func decodeConnectorReport(t *testing.T, stdout string) connectorReport {
	t.Helper()
	var report connectorReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("decode compile report: %v\nstdout: %s", err, stdout)
	}
	if report.SchemaVersion != connectorReportSchemaVersion {
		t.Fatalf("compile report revision = %q", report.SchemaVersion)
	}
	return report
}

func expectedOutputPaths() []string {
	paths := []string{
		"internal/connectors/data/" + fixtureConnectorID + ".bundle.json",
		"internal/connectors/data/" + fixtureConnectorID + ".report.json",
	}
	for _, lane := range connectorLanes {
		paths = append(paths,
			"connectors/"+fixtureConnectorID+"/lanes/"+string(lane)+".json",
			"connectors/"+fixtureConnectorID+"/scripts/"+string(lane)+".sh")
	}
	slices.Sort(paths)
	return paths
}

func reportedOutputPaths(entry connectorReportEntry) []string {
	paths := make([]string, 0, len(entry.Outputs))
	for _, output := range entry.Outputs {
		paths = append(paths, output.Path)
	}
	slices.Sort(paths)
	return paths
}

func assertPublishedFile(t *testing.T, root string, output connectorReportOutput) {
	t.Helper()
	published, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(output.Path)))
	if err != nil {
		t.Fatalf("read reported output %s: %v", output.Path, err)
	}
	digest := sha256.Sum256(published)
	if got := "sha256:" + hex.EncodeToString(digest[:]); got != output.SHA256 {
		t.Fatalf("output %s digest = %s; report says %s", output.Path, got, output.SHA256)
	}
	if len(published) != output.ByteLength {
		t.Fatalf("output %s byte length = %d; report says %d", output.Path, len(published), output.ByteLength)
	}
}

// assertFixtureTree requires the pinned inputs plus exactly the declared
// generated outputs and nothing else.
func assertFixtureTree(t *testing.T, root string) {
	t.Helper()
	expected := []string{
		"connectors/" + fixtureConnectorID + "/captures/LICENSE.md",
		"connectors/" + fixtureConnectorID + "/captures/" + fixtureSourceID,
		"connectors/" + fixtureConnectorID + "/scenarios/lifecycle.json",
		"connectors/" + fixtureConnectorID + "/semantics.json",
		"connectors/" + fixtureConnectorID + "/source.lock.json",
	}
	expected = append(expected, expectedOutputPaths()...)
	slices.Sort(expected)
	actual := fixtureFiles(t, root)
	if !slices.Equal(actual, expected) {
		t.Fatalf("generated tree = %v; want exactly %v", actual, expected)
	}
}

func digestFixtureTree(t *testing.T, root string) map[string]string {
	t.Helper()
	digests := make(map[string]string)
	for _, relative := range fixtureFiles(t, root) {
		document, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(document)
		digests[relative] = hex.EncodeToString(digest[:])
	}
	return digests
}

func fixtureFiles(t *testing.T, root string) []string {
	t.Helper()
	found := []string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		found = append(found, filepath.ToSlash(relative))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(found)
	return found
}

func assertOutputsAbsent(t *testing.T, root string) {
	t.Helper()
	for _, relative := range expectedOutputPaths() {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(relative))); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("refused generation still wrote %s", relative)
		}
	}
}

// assertLaneInventory requires the canonical seven lanes with only the authored
// direct_read cell eligible and the authored case attached to it.
func assertLaneInventory(t *testing.T, root string) {
	t.Helper()
	type laneProbe struct {
		SchemaVersion string `json:"schema_version"`
		ConnectorID   string `json:"connector_id"`
		Lane          string `json:"lane"`
		Operations    []struct {
			OperationID string `json:"operation_id"`
			Status      string `json:"status"`
			Reason      string `json:"reason"`
			Cases       []struct {
				BehaviorID string `json:"behavior_id"`
				CaseID     string `json:"case_id"`
			} `json:"cases"`
		} `json:"operations"`
	}
	eligible := []string{}
	for _, lane := range connectorLanes {
		lanePath := filepath.Join(root, "connectors", fixtureConnectorID, "lanes", string(lane)+".json")
		document, err := os.ReadFile(lanePath)
		if err != nil {
			t.Fatal(err)
		}
		var probe laneProbe
		if err := json.Unmarshal(document, &probe); err != nil {
			t.Fatalf("decode lane %s: %v", lane, err)
		}
		if probe.ConnectorID != fixtureConnectorID || probe.Lane != string(lane) {
			t.Fatalf("lane %s is bound to %s/%s", lane, probe.ConnectorID, probe.Lane)
		}
		if len(probe.Operations) != 1 || probe.Operations[0].OperationID != fixtureOperationID {
			t.Fatalf("lane %s operations = %#v", lane, probe.Operations)
		}
		operation := probe.Operations[0]
		if operation.Reason == "" {
			t.Fatalf("lane %s has no disposition reason", lane)
		}
		if operation.Status == string(contract.LaneEligible) {
			eligible = append(eligible, string(lane))
			if lane != contract.LaneDirectRead {
				t.Fatalf("lane %s is eligible without an authored classification", lane)
			}
			if len(operation.Cases) != 1 || operation.Cases[0].BehaviorID != fixtureScenarioID {
				t.Fatalf("eligible lane cases = %#v", operation.Cases)
			}
		} else if len(operation.Cases) != 0 {
			t.Fatalf("lane %s carries cases for an ineligible cell", lane)
		}
		wrapper, err := os.ReadFile(filepath.Join(root, "connectors", fixtureConnectorID, "scripts", string(lane)+".sh"))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"--connector 'synthetic'", "--lane '" + string(lane) + "'"} {
			if !strings.Contains(string(wrapper), want) {
				t.Fatalf("lane %s wrapper = %q; want %s", lane, wrapper, want)
			}
		}
	}
	if !slices.Equal(eligible, []string{string(contract.LaneDirectRead)}) {
		t.Fatalf("eligible lanes = %v; want only direct_read", eligible)
	}
}

// mutateScenarioDocument rewrites one authored scenario array as raw maps so a
// test can introduce an unknown, missing or contradictory member.
func mutateScenarioDocument(t *testing.T, path string, mutate func(document []map[string]any) []map[string]any) {
	t.Helper()
	document, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var scenarios []map[string]any
	if err := json.Unmarshal(document, &scenarios); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(mutate(scenarios))
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, path, encoded)
}

func copyConnectorRoot(t *testing.T, root, source, target string) {
	t.Helper()
	sourceRoot := filepath.Join(root, "connectors", source)
	targetRoot := filepath.Join(root, "connectors", target)
	err := filepath.WalkDir(sourceRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(targetRoot, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o755)
		}
		document, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, document, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func writeFixtureFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
