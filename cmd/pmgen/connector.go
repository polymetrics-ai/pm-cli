package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	pmcompile "github.com/polymetrics-ai/pm-cli/internal/compile"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/proof"
)

const connectorUsageText = `usage: pmgen connector --root PATH [--connector ID]... [--check]

Required:
  --root PATH                   repository root holding connectors/ and internal/connectors/data/

Optional:
  --connector ID                generate this connector root; repeat to select several.
                                Default: every connectors/<id> holding a source.lock.json
  --check                       regenerate in memory and fail when the checkout differs

Pinned inputs read from one connector root:
  source.lock.json              immutable source set and operation inventory
  semantics.json                source-linked semantic amendments
  captures/<source_id>          captured source bytes named by the lock source identity
  captures/<license-name>       captured license bytes named by the lock license path
  scenarios/*.json              authored scenario arrays

Generated outputs:
  internal/connectors/data/<id>.bundle.json   runtime bundle
  internal/connectors/data/<id>.report.json   raw/normalized fidelity report
  connectors/<id>/lanes/<lane>.json           seven lane inventories
  connectors/<id>/scripts/<lane>.sh           seven thin lane wrappers
`

const connectorReportSchemaVersion = "pm.connector-generation/v1"

// connectorLanes is the canonical seven-lane inventory. A lane emitter that
// omits, invents or duplicates a cell is refused instead of silently changing
// the generated inventory that prove.sh consumes.
var connectorLanes = [...]contract.Lane{
	contract.LaneDirectRead,
	contract.LaneDirectWrite,
	contract.LaneETL,
	contract.LaneReverseETL,
	contract.LaneBinaryDownload,
	contract.LaneBinaryUpload,
	contract.LaneSyncTransport,
}

// scenarioDocumentFields are the closed, explicitly present members of one
// authored scenario. The contract decoder refuses unknown, duplicate, null and
// mistyped members of the same boundaries the proof runner later reads.
var scenarioDocumentFields = [...]string{
	"scenario_id",
	"connector_id",
	"lane",
	"operation_id",
	"source_refs",
	"requirements",
	"steps",
	"assertions",
	"cleanup",
}

type connectorCompileFunc func(context.Context, pmcompile.CompilationInput) (pmcompile.CompilationArtifacts, error)

type connectorLaneEmitter func([]byte, []contract.Scenario) (map[string][]byte, error)

// connectorDependencies are the two owned generation steps. Tests replace them
// to exercise refusal, exact path and publication behavior.
type connectorDependencies struct {
	compile connectorCompileFunc
	lanes   connectorLaneEmitter
}

func defaultConnectorDependencies() connectorDependencies {
	return connectorDependencies{compile: pmcompile.Compile, lanes: proof.EmitLaneArtifacts}
}

type connectorOptions struct {
	root       string
	connectors repeatedFlag
	check      bool
}

type connectorInput struct {
	connectorID string
	root        string
	sourceLock  []byte
	semantics   []byte
	captures    pmcompile.Captures
	scenarios   []contract.Scenario
}

type connectorOutput struct {
	path string
	data []byte
}

type connectorGeneration struct {
	input             connectorInput
	sourceDigest      string
	semanticsRevision string
	outputs           []connectorOutput
}

type connectorReport struct {
	SchemaVersion string                 `json:"schema_version"`
	Check         bool                   `json:"check"`
	Connectors    []connectorReportEntry `json:"connectors"`
}

type connectorReportEntry struct {
	ConnectorID       string                  `json:"connector_id"`
	SourceDigest      string                  `json:"source_digest"`
	SemanticsRevision string                  `json:"semantics_revision"`
	Outputs           []connectorReportOutput `json:"outputs"`
}

type connectorReportOutput struct {
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	ByteLength int    `json:"byte_length"`
	Status     string `json:"status"`
}

func runConnector(
	ctx context.Context,
	args []string,
	stdout, stderr io.Writer,
	dependencies connectorDependencies,
) int {
	options, err := parseConnectorOptions(args, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "pmgen: %v\n", err)
		return 2
	}
	root, err := resolveConnectorRoot(options.root)
	if err != nil {
		fmt.Fprintf(stderr, "pmgen: connector: %v\n", err)
		return 1
	}
	generations, err := generateConnectors(ctx, root, options.connectors, dependencies)
	if err != nil {
		fmt.Fprintf(stderr, "pmgen: connector: %v\n", err)
		return 1
	}
	report := connectorReport{
		SchemaVersion: connectorReportSchemaVersion,
		Check:         options.check,
		Connectors:    make([]connectorReportEntry, 0, len(generations)),
	}
	var drifted []string
	for _, generation := range generations {
		entry, drift, err := publishConnectorGeneration(root, generation, options.check)
		if err != nil {
			fmt.Fprintf(stderr, "pmgen: connector: %v\n", err)
			return 1
		}
		report.Connectors = append(report.Connectors, entry)
		drifted = append(drifted, drift...)
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "pmgen: connector: encode report: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\n", encoded)
	if len(drifted) != 0 {
		for _, path := range drifted {
			fmt.Fprintf(stderr, "pmgen: connector: generated bytes differ from the checkout: %s\n", path)
		}
		return 1
	}
	return 0
}

func parseConnectorOptions(args []string, stderr io.Writer) (connectorOptions, error) {
	var options connectorOptions
	set := flag.NewFlagSet("pmgen connector", flag.ContinueOnError)
	set.SetOutput(stderr)
	set.StringVar(&options.root, "root", "", "repository root holding connectors/")
	set.Var(&options.connectors, "connector", "connector identity to generate")
	set.BoolVar(&options.check, "check", false, "regenerate in memory and fail on any difference")
	set.Usage = func() { fmt.Fprint(stderr, connectorUsageText) }
	if err := set.Parse(args); err != nil {
		return connectorOptions{}, err
	}
	if set.NArg() != 0 {
		return connectorOptions{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(set.Args(), " "))
	}
	if options.root == "" {
		return connectorOptions{}, errors.New("root is required")
	}
	seen := make(map[string]struct{}, len(options.connectors))
	for _, connectorID := range options.connectors {
		if err := validateConnectorIdentity(connectorID); err != nil {
			return connectorOptions{}, err
		}
		if _, duplicate := seen[connectorID]; duplicate {
			return connectorOptions{}, fmt.Errorf("connector identity %q is duplicated", connectorID)
		}
		seen[connectorID] = struct{}{}
	}
	return options, nil
}

// validateConnectorIdentity keeps one connector identity to a single bounded
// path component so generated paths can never escape their declared root.
func validateConnectorIdentity(connectorID string) error {
	if connectorID == "" || strings.TrimSpace(connectorID) != connectorID {
		return errors.New("connector identity must be a nonempty canonical path component")
	}
	if connectorID == "." || connectorID == ".." || strings.ContainsAny(connectorID, "/\\\x00") {
		return fmt.Errorf("connector identity %q must be one path component", connectorID)
	}
	if len(connectorID) > 1024 {
		return fmt.Errorf("connector identity %q exceeds its bound", connectorID)
	}
	return nil
}

// resolveConnectorRoot binds every generated path to one absolute repository
// root, so the same pinned inputs produce the same relative report paths and
// the declared output guard cannot be escaped by a relative invocation.
func resolveConnectorRoot(root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("read root %s: %w", root, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("root %s must be a directory", root)
	}
	return absolute, nil
}

func generateConnectors(
	ctx context.Context,
	root string,
	selection repeatedFlag,
	dependencies connectorDependencies,
) ([]connectorGeneration, error) {
	discovered, err := discoverConnectorRoots(root)
	if err != nil {
		return nil, err
	}
	selected := discovered
	if len(selection) != 0 {
		selected = slices.Clone(selection)
		slices.Sort(selected)
		for _, connectorID := range selected {
			if !slices.Contains(discovered, connectorID) {
				return nil, fmt.Errorf("connector %q has no connectors/%s root holding source.lock.json", connectorID, connectorID)
			}
		}
	}
	generations := make([]connectorGeneration, 0, len(selected))
	for _, connectorID := range selected {
		input, err := loadConnectorInput(root, connectorID)
		if err != nil {
			return nil, err
		}
		artifacts, err := dependencies.compile(ctx, pmcompile.CompilationInput{
			SourceLock: input.sourceLock,
			Captures:   input.captures,
			Semantics:  input.semantics,
		})
		if err != nil {
			return nil, fmt.Errorf("connector %s: compile: %w", connectorID, err)
		}
		lanes, err := dependencies.lanes(artifacts.Bundle, input.scenarios)
		if err != nil {
			return nil, fmt.Errorf("connector %s: lane inventory: %w", connectorID, err)
		}
		outputs, err := connectorOutputs(root, input, artifacts, lanes)
		if err != nil {
			return nil, fmt.Errorf("connector %s: %w", connectorID, err)
		}
		sourceDigest, semanticsRevision, err := connectorBundleIdentity(input.connectorID, artifacts.Bundle)
		if err != nil {
			return nil, fmt.Errorf("connector %s: %w", connectorID, err)
		}
		generations = append(generations, connectorGeneration{
			input:             input,
			sourceDigest:      sourceDigest,
			semanticsRevision: semanticsRevision,
			outputs:           outputs,
		})
	}
	return generations, nil
}

// discoverConnectorRoots lists connector directory identities in one stable
// order. A directory is a connector root only when it holds a regular
// source.lock.json; symlinks and other entries are not followed.
func discoverConnectorRoots(root string) ([]string, error) {
	connectorsRoot := filepath.Join(root, "connectors")
	entries, err := os.ReadDir(connectorsRoot)
	if err != nil {
		return nil, fmt.Errorf("read connectors/: %w", err)
	}
	found := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := os.Lstat(filepath.Join(connectorsRoot, entry.Name(), "source.lock.json"))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if err := validateConnectorIdentity(entry.Name()); err != nil {
			return nil, fmt.Errorf("connectors/%s: %w", entry.Name(), err)
		}
		found = append(found, entry.Name())
	}
	slices.Sort(found)
	return found, nil
}

func loadConnectorInput(root, connectorID string) (connectorInput, error) {
	connectorRoot := filepath.Join(root, "connectors", connectorID)
	sourceLock, err := readRegularFile(filepath.Join(connectorRoot, "source.lock.json"), contract.MaxContractDocumentBytes)
	if err != nil {
		return connectorInput{}, fmt.Errorf("connector %s: read source.lock.json: %w", connectorID, err)
	}
	lock, err := pmcompile.DecodeSourceLock(sourceLock)
	if err != nil {
		return connectorInput{}, fmt.Errorf("connector %s: %w", connectorID, err)
	}
	if lock.ConnectorID != connectorID {
		return connectorInput{}, fmt.Errorf(
			"connector %s: source.lock.json declares connector identity %q; a connector root declares its own directory identity and no second root may duplicate it",
			connectorID, lock.ConnectorID)
	}
	semantics, err := readRegularFile(filepath.Join(connectorRoot, "semantics.json"), contract.MaxContractDocumentBytes)
	if err != nil {
		return connectorInput{}, fmt.Errorf("connector %s: read semantics.json: %w", connectorID, err)
	}
	captures, err := loadConnectorCaptures(connectorRoot, connectorID, lock)
	if err != nil {
		return connectorInput{}, err
	}
	scenarios, err := loadConnectorScenarios(connectorRoot, connectorID)
	if err != nil {
		return connectorInput{}, err
	}
	return connectorInput{
		connectorID: connectorID,
		root:        connectorRoot,
		sourceLock:  sourceLock,
		semantics:   semantics,
		captures:    captures,
		scenarios:   scenarios,
	}, nil
}

// loadConnectorCaptures admits exactly the capture and license files the lock
// declares, by source identity and by license base name, and refuses every
// other entry of the captures directory.
func loadConnectorCaptures(connectorRoot, connectorID string, lock pmcompile.SourceLock) (pmcompile.Captures, error) {
	capturesRoot := filepath.Join(connectorRoot, "captures")
	if info, err := os.Lstat(capturesRoot); err != nil || !info.IsDir() {
		return pmcompile.Captures{}, fmt.Errorf("connector %s: captures directory is missing", connectorID)
	}
	admitted := make(map[string]struct{}, len(lock.SourceSet.Documents)*2)
	licenseNames := make(map[string]string, len(lock.SourceSet.Documents))
	licensePaths := make(map[string]string, len(lock.SourceSet.Documents))
	for _, document := range lock.SourceSet.Documents {
		admitted[document.SourceID] = struct{}{}
		name := filepath.Base(filepath.FromSlash(document.License.Path))
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
			return pmcompile.Captures{}, fmt.Errorf(
				"connector %s: license path %q has no single bounded file name", connectorID, document.License.Path)
		}
		if previous, ambiguous := licensePaths[name]; ambiguous && previous != document.License.Path {
			return pmcompile.Captures{}, fmt.Errorf(
				"connector %s: license capture name %s is ambiguous between %s and %s", connectorID, name, previous, document.License.Path)
		}
		licensePaths[name] = document.License.Path
		licenseNames[document.SourceID] = name
		admitted[name] = struct{}{}
	}
	entries, err := os.ReadDir(capturesRoot)
	if err != nil {
		return pmcompile.Captures{}, fmt.Errorf("connector %s: read captures: %w", connectorID, err)
	}
	for _, entry := range entries {
		if _, ok := admitted[entry.Name()]; !ok {
			return pmcompile.Captures{}, fmt.Errorf(
				"connector %s: captures/%s is not declared by source.lock.json", connectorID, entry.Name())
		}
	}
	documents := make(map[string][]byte, len(lock.SourceSet.Documents))
	licenses := make(map[string][]byte, len(lock.SourceSet.Documents))
	for _, document := range lock.SourceSet.Documents {
		captured, err := readRegularFile(filepath.Join(capturesRoot, document.SourceID), pmcompile.MaxSourceDocumentBytes)
		if err != nil {
			return pmcompile.Captures{}, fmt.Errorf("connector %s: read capture %s: %w", connectorID, document.SourceID, err)
		}
		license, err := readRegularFile(filepath.Join(capturesRoot, licenseNames[document.SourceID]), contract.MaxContractDocumentBytes)
		if err != nil {
			return pmcompile.Captures{}, fmt.Errorf(
				"connector %s: read license %s: %w", connectorID, licenseNames[document.SourceID], err)
		}
		documents[document.SourceID] = captured
		licenses[document.SourceID] = license
	}
	return pmcompile.Captures{Documents: documents, Licenses: licenses}, nil
}

func loadConnectorScenarios(connectorRoot, connectorID string) ([]contract.Scenario, error) {
	scenariosRoot := filepath.Join(connectorRoot, "scenarios")
	if info, err := os.Lstat(scenariosRoot); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("connector %s: scenarios directory is missing", connectorID)
	}
	entries, err := os.ReadDir(scenariosRoot)
	if err != nil {
		return nil, fmt.Errorf("connector %s: read scenarios: %w", connectorID, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return nil, fmt.Errorf("connector %s: scenarios/%s is not an authored scenario document", connectorID, entry.Name())
		}
		names = append(names, entry.Name())
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("connector %s: no authored scenario document under scenarios/", connectorID)
	}
	slices.Sort(names)
	scenarios := make([]contract.Scenario, 0, len(names))
	sources := make(map[string]string, len(names))
	for _, name := range names {
		document, err := readRegularFile(filepath.Join(scenariosRoot, name), contract.MaxContractDocumentBytes)
		if err != nil {
			return nil, fmt.Errorf("connector %s: read scenario %s: %w", connectorID, name, err)
		}
		decoded, err := decodeScenarioDocument(document, name)
		if err != nil {
			return nil, fmt.Errorf("connector %s: %w", connectorID, err)
		}
		if len(decoded) == 0 {
			return nil, fmt.Errorf("connector %s: scenario document %s declares no scenario", connectorID, name)
		}
		for _, scenario := range decoded {
			if scenario.ConnectorID != connectorID {
				return nil, fmt.Errorf(
					"connector %s: scenario %s in %s belongs to connector %q", connectorID, scenario.ScenarioID, name, scenario.ConnectorID)
			}
			if prior, duplicate := sources[scenario.ScenarioID]; duplicate {
				return nil, fmt.Errorf(
					"connector %s: scenario identity %s is duplicated in %s and %s", connectorID, scenario.ScenarioID, prior, name)
			}
			sources[scenario.ScenarioID] = name
			scenarios = append(scenarios, scenario)
		}
	}
	return scenarios, nil
}

// decodeScenarioDocument admits one authored document: a JSON array whose
// members are closed scenario objects.
func decodeScenarioDocument(document []byte, source string) ([]contract.Scenario, error) {
	var elements []json.RawMessage
	if err := json.Unmarshal(document, &elements); err != nil {
		return nil, fmt.Errorf("scenario document %s must be a JSON array of scenario objects: %w", source, err)
	}
	scenarios := make([]contract.Scenario, 0, len(elements))
	for index, element := range elements {
		var scenario contract.Scenario
		if err := contract.DecodeStrictRequiredObject(element, &scenario, scenarioDocumentFields[:]...); err != nil {
			return nil, fmt.Errorf("scenario document %s element %d: %w", source, index, err)
		}
		scenarios = append(scenarios, scenario)
	}
	return scenarios, nil
}

// connectorOutputs binds the compiler artifacts and the emitted lane set to the
// exact declared output paths of one connector root. It refuses an emitted lane
// set that is not exactly the canonical seven lanes and seven wrappers.
func connectorOutputs(
	root string,
	input connectorInput,
	artifacts pmcompile.CompilationArtifacts,
	lanes map[string][]byte,
) ([]connectorOutput, error) {
	if len(artifacts.Bundle) == 0 || len(artifacts.Report) == 0 {
		return nil, errors.New("the compiler emitted no bundle or fidelity report")
	}
	outputs := []connectorOutput{
		{
			path: filepath.Join(root, "internal", "connectors", "data", input.connectorID+".bundle.json"),
			data: artifacts.Bundle,
		},
		{
			path: filepath.Join(root, "internal", "connectors", "data", input.connectorID+".report.json"),
			data: artifacts.Report,
		},
	}
	missing := make(map[string]struct{}, len(connectorLanes)*2)
	for _, lane := range connectorLanes {
		missing["lanes/"+string(lane)+".json"] = struct{}{}
		missing["scripts/"+string(lane)+".sh"] = struct{}{}
	}
	for name, data := range lanes {
		if _, expected := missing[name]; !expected {
			return nil, fmt.Errorf("lane emitter returned unknown artifact %q", name)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("lane emitter returned an empty artifact for %q", name)
		}
		delete(missing, name)
		outputs = append(outputs, connectorOutput{path: filepath.Join(input.root, filepath.FromSlash(name)), data: data})
	}
	if len(missing) != 0 {
		omitted := make([]string, 0, len(missing))
		for name := range missing {
			omitted = append(omitted, name)
		}
		slices.Sort(omitted)
		return nil, fmt.Errorf("lane emitter omitted %d canonical lane artifacts: %s", len(omitted), strings.Join(omitted, ", "))
	}
	slices.SortFunc(outputs, func(left, right connectorOutput) int {
		return strings.Compare(left.path, right.path)
	})
	for _, output := range outputs {
		if !isDeclaredConnectorOutput(root, input.connectorID, output.path) {
			return nil, fmt.Errorf("refusing to write outside the declared connector outputs: %s", output.path)
		}
	}
	return outputs, nil
}

// isDeclaredConnectorOutput is the exact write-scope contract of one connector
// root: two repository data files, seven lane inventories and seven wrappers.
func isDeclaredConnectorOutput(root, connectorID, path string) bool {
	relative, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	relative = filepath.ToSlash(relative)
	switch relative {
	case "internal/connectors/data/" + connectorID + ".bundle.json",
		"internal/connectors/data/" + connectorID + ".report.json":
		return true
	}
	for _, lane := range connectorLanes {
		if relative == "connectors/"+connectorID+"/lanes/"+string(lane)+".json" ||
			relative == "connectors/"+connectorID+"/scripts/"+string(lane)+".sh" {
			return true
		}
	}
	return false
}

// connectorBundleIdentity reads the identity facts a generated bundle declares
// so the report can be compared with the requested connector and pinned source
// without re-deriving them.
func connectorBundleIdentity(connectorID string, bundle []byte) (string, string, error) {
	var projection struct {
		SchemaVersion     string `json:"schema_version"`
		ConnectorID       string `json:"connector_id"`
		SourceDigest      string `json:"source_digest"`
		SemanticsRevision string `json:"semantics_revision"`
	}
	if err := json.Unmarshal(bundle, &projection); err != nil {
		return "", "", fmt.Errorf("read generated bundle identity: %w", err)
	}
	if projection.SchemaVersion != pmcompile.BundleSchemaVersion {
		return "", "", fmt.Errorf("generated bundle declares unsupported revision %q", projection.SchemaVersion)
	}
	if projection.ConnectorID != connectorID {
		return "", "", fmt.Errorf("generated bundle declares connector %q instead of %q", projection.ConnectorID, connectorID)
	}
	if projection.SourceDigest == "" || projection.SemanticsRevision == "" {
		return "", "", errors.New("generated bundle does not declare its source and semantics identity")
	}
	return projection.SourceDigest, projection.SemanticsRevision, nil
}

// publishConnectorGeneration either writes every artifact atomically or, in
// check mode, compares the regenerated bytes against the checkout without
// writing anything. Both modes report the regenerated identity of every output.
func publishConnectorGeneration(
	root string,
	generation connectorGeneration,
	check bool,
) (connectorReportEntry, []string, error) {
	entry := connectorReportEntry{
		ConnectorID:       generation.input.connectorID,
		SourceDigest:      generation.sourceDigest,
		SemanticsRevision: generation.semanticsRevision,
		Outputs:           make([]connectorReportOutput, 0, len(generation.outputs)),
	}
	var drifted []string
	if !check {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return entry, drifted, fmt.Errorf("resolve root %s: %w", root, err)
		}
		directories := make([]string, 0, 4)
		for _, output := range generation.outputs {
			directory := filepath.Dir(output.path)
			if !slices.Contains(directories, directory) {
				directories = append(directories, directory)
			}
		}
		slices.Sort(directories)
		for _, directory := range directories {
			if err := os.MkdirAll(directory, 0o755); err != nil {
				return entry, drifted, err
			}
			if err := requireOutputDirectory(resolved, directory); err != nil {
				return entry, drifted, err
			}
		}
	}
	for _, output := range generation.outputs {
		relative, err := filepath.Rel(root, output.path)
		if err != nil {
			return entry, drifted, err
		}
		digest := sha256.Sum256(output.data)
		reported := connectorReportOutput{
			Path:       filepath.ToSlash(relative),
			SHA256:     "sha256:" + hex.EncodeToString(digest[:]),
			ByteLength: len(output.data),
		}
		if check {
			existing, err := readRegularFile(output.path, contract.MaxContractDocumentBytes)
			switch {
			case err == nil && bytes.Equal(existing, output.data):
				reported.Status = "unchanged"
			case err == nil:
				reported.Status = "drifted"
				drifted = append(drifted, reported.Path)
			case errors.Is(err, fs.ErrNotExist):
				reported.Status = "missing"
				drifted = append(drifted, reported.Path)
			default:
				return entry, drifted, fmt.Errorf("compare %s: %w", reported.Path, err)
			}
		} else {
			if err := writeAtomic(output.path, output.data); err != nil {
				return entry, drifted, err
			}
			reported.Status = "written"
		}
		entry.Outputs = append(entry.Outputs, reported)
	}
	return entry, drifted, nil
}

// requireOutputDirectory refuses a declared output directory that resolves
// outside the repository root through a symlink.
func requireOutputDirectory(resolvedRoot, directory string) error {
	resolved, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return err
	}
	if resolved != resolvedRoot && !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) {
		return fmt.Errorf("output directory %s resolves outside %s", directory, resolvedRoot)
	}
	return nil
}
