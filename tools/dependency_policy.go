package tools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const (
	DependenciesSchema = "pm.dependencies/v1"
	ToolchainsSchema   = "pm.toolchains/v1"
	PolicyResultSchema = "pm.dependency-policy.result/v1"

	applicationModule     = "github.com/polymetrics-ai/pm-cli"
	toolsModule           = "github.com/polymetrics-ai/pm-cli/tools"
	maxJSONBytes          = 64 << 20
	maxCommandOutput      = 32 << 20
	maxCommandError       = 1 << 20
	externalEvidenceScope = "resolved-module-graphs-and-current-platform-external-tool-inventories"

	// Inventory scopes report observed records, not package-discovery completeness.
	// In particular, installed-files does not assert recognized-package coverage.
	inventoryScopeRecognizedPackages                  = "recognized-packages"
	inventoryScopeInstalledFiles                      = "installed-files"
	inventoryScopeRecognizedPackagesAndInstalledFiles = "recognized-packages-and-installed-files"
)

var ErrExternalEvidenceRequired = errors.New("at least one external CycloneDX or SPDX SBOM is required")

type ModuleSelection struct {
	Path      string `json:"path"`
	Version   string `json:"version"`
	License   string `json:"license"`
	SourceURL string `json:"source_url"`
}

type ForbiddenModule struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Reason  string `json:"reason"`
}

type ModuleSelections struct {
	Application []ModuleSelection `json:"application"`
	Tools       []ModuleSelection `json:"tools"`
}

type VersionsManifest struct {
	SchemaVersion string            `json:"schema_version"`
	GoVersion     string            `json:"go_version"`
	Modules       ModuleSelections  `json:"modules"`
	Forbidden     []ForbiddenModule `json:"forbidden"`
}

type ToolchainAttestation struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type ToolchainAsset struct {
	ID          string                `json:"id"`
	Version     string                `json:"version"`
	Platform    string                `json:"platform"`
	URL         string                `json:"url"`
	SHA256      string                `json:"sha256"`
	Kind        string                `json:"kind"`
	Executable  string                `json:"executable"`
	Attestation *ToolchainAttestation `json:"attestation,omitempty"`
}

type ToolchainModule struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Integrity string `json:"integrity"`
	URL       string `json:"url"`
}

type ToolchainImage struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	IndexDigest string `json:"index_digest"`
	SourceURL   string `json:"source_url"`
}

type ToolchainsManifest struct {
	SchemaVersion string            `json:"schema_version"`
	GoVersion     string            `json:"go_version"`
	Assets        []ToolchainAsset  `json:"assets"`
	Modules       []ToolchainModule `json:"modules"`
	Images        []ToolchainImage  `json:"images"`
}

type CheckOptions struct {
	Root           string
	VersionsPath   string
	ToolchainsPath string
	SBOMPaths      []string
	GoCommand      string
}

type ModuleGraphResult struct {
	Name            string `json:"name"`
	MainModule      string `json:"main_module"`
	ResolvedModules int    `json:"resolved_modules"`
	GraphEdges      int    `json:"graph_edges"`
}

type SBOMResult struct {
	Format                 string   `json:"format"`
	DocumentSHA256         string   `json:"document_sha256"`
	SubjectName            string   `json:"subject_name"`
	SubjectVersion         string   `json:"subject_version"`
	InventoryScope         string   `json:"inventory_scope"`
	RecognizedPackages     int      `json:"recognized_packages"`
	UnversionedPackages    []string `json:"unversioned_packages,omitempty"`
	InventoriedFiles       int      `json:"inventoried_files"`
	InventoriedFileSHA256s int      `json:"inventoried_file_sha256s"`
	LockedIdentities       []string `json:"locked_identities"`
}

type AssetResult struct {
	ID                     string `json:"id"`
	Version                string `json:"version"`
	Platform               string `json:"platform"`
	SourceSHA256           string `json:"source_sha256"`
	SBOMDocumentSHA256     string `json:"sbom_document_sha256"`
	InventoryScope         string `json:"inventory_scope"`
	RecognizedPackages     int    `json:"recognized_packages"`
	InventoriedFiles       int    `json:"inventoried_files"`
	InventoriedFileSHA256s int    `json:"inventoried_file_sha256s"`
}

type CheckResult struct {
	SchemaVersion string              `json:"schema_version"`
	Status        string              `json:"status"`
	EvidenceScope string              `json:"evidence_scope"`
	GoVersion     string              `json:"go_version"`
	Platform      string              `json:"platform"`
	ModuleGraphs  []ModuleGraphResult `json:"module_graphs"`
	ExternalSBOMs []SBOMResult        `json:"external_sboms"`
	Assets        []AssetResult       `json:"assets"`
}

type PolicyViolationError struct {
	Scope    string
	Identity string
	Reason   string
	Chain    []string
}

func (e *PolicyViolationError) Error() string {
	chain := strings.Join(e.Chain, " -> ")
	if chain == "" {
		chain = e.Identity
	}
	return fmt.Sprintf("dependency policy violation in %s: %s (%s); chain: %s", e.Scope, e.Identity, e.Reason, chain)
}

func CheckDependencies(ctx context.Context, opts CheckOptions) (CheckResult, error) {
	var result CheckResult
	if ctx == nil {
		return result, errors.New("dependency policy requires a non-nil context")
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return result, fmt.Errorf("resolve repository root: %w", err)
	}
	if info, err := os.Stat(root); err != nil {
		return result, fmt.Errorf("stat repository root: %w", err)
	} else if !info.IsDir() {
		return result, fmt.Errorf("repository root %q is not a directory", root)
	}
	if len(opts.SBOMPaths) == 0 {
		return result, ErrExternalEvidenceRequired
	}

	versionsPath := inputPath(root, opts.VersionsPath)
	toolchainsPath := inputPath(root, opts.ToolchainsPath)
	versions, err := loadStrictJSON[VersionsManifest](versionsPath)
	if err != nil {
		return result, fmt.Errorf("load dependency versions: %w", err)
	}
	if err := validateVersions(versions); err != nil {
		return result, fmt.Errorf("validate dependency versions: %w", err)
	}
	toolchains, err := loadStrictJSON[ToolchainsManifest](toolchainsPath)
	if err != nil {
		return result, fmt.Errorf("load toolchain locks: %w", err)
	}
	if err := validateToolchains(toolchains); err != nil {
		return result, fmt.Errorf("validate toolchain locks: %w", err)
	}
	if versions.GoVersion != toolchains.GoVersion {
		return result, fmt.Errorf("Go version mismatch: dependency policy selects %s but toolchain lock selects %s", versions.GoVersion, toolchains.GoVersion)
	}

	documents := make([]sbomDocument, 0, len(opts.SBOMPaths))
	for _, suppliedPath := range opts.SBOMPaths {
		if strings.TrimSpace(suppliedPath) == "" {
			return result, errors.New("SBOM path must not be empty")
		}
		document, err := loadSBOM(inputPath(root, suppliedPath))
		if err != nil {
			return result, fmt.Errorf("load SBOM %q: %w", suppliedPath, err)
		}
		if err := checkSBOMForbidden(document, versions.Forbidden); err != nil {
			return result, err
		}
		documents = append(documents, document)
	}
	assetResults, sbomResults, err := checkSBOMCoverage(toolchains, documents)
	if err != nil {
		return result, err
	}

	goCommand := strings.TrimSpace(opts.GoCommand)
	if goCommand == "" {
		goCommand = strings.TrimSpace(os.Getenv("PM_GO"))
	}
	if goCommand == "" {
		goCommand = "go"
	}
	actualGoVersion, err := resolvedGoVersion(ctx, goCommand, root)
	if err != nil {
		return result, err
	}
	if actualGoVersion != versions.GoVersion {
		return result, fmt.Errorf("resolved Go executable reports %s; policy requires %s", actualGoVersion, versions.GoVersion)
	}

	checks := []struct {
		name       string
		root       string
		mainModule string
		selected   []ModuleSelection
	}{
		{name: "application", root: root, mainModule: applicationModule, selected: versions.Modules.Application},
		{name: "tools", root: filepath.Join(root, "tools"), mainModule: toolsModule, selected: versions.Modules.Tools},
	}
	graphResults := make([]ModuleGraphResult, 0, len(checks))
	for _, item := range checks {
		graph, err := resolveModuleGraph(ctx, goCommand, item.root, item.mainModule, false)
		if err != nil {
			return result, fmt.Errorf("resolve %s module graph: %w", item.name, err)
		}
		if err := checkSelectedModules(item.name, graph, item.selected); err != nil {
			return result, err
		}
		if err := checkGraphForbidden(item.name, graph, versions.Forbidden); err != nil {
			return result, err
		}
		graphResults = append(graphResults, ModuleGraphResult{
			Name:            item.name,
			MainModule:      item.mainModule,
			ResolvedModules: len(graph.modules) - 1,
			GraphEdges:      graph.edgeCount,
		})
	}

	result = CheckResult{
		SchemaVersion: PolicyResultSchema,
		Status:        "pass",
		EvidenceScope: externalEvidenceScope,
		GoVersion:     actualGoVersion,
		Platform:      canonicalPlatform(runtime.GOOS + "-" + runtime.GOARCH),
		ModuleGraphs:  graphResults,
		ExternalSBOMs: sbomResults,
		Assets:        assetResults,
	}
	return result, nil
}

func inputPath(root, supplied string) string {
	if filepath.IsAbs(supplied) {
		return filepath.Clean(supplied)
	}
	return filepath.Join(root, supplied)
}

func validateVersions(manifest VersionsManifest) error {
	if manifest.SchemaVersion != DependenciesSchema {
		return fmt.Errorf("schema_version must be %q", DependenciesSchema)
	}
	if err := validateGoVersion(manifest.GoVersion); err != nil {
		return err
	}
	groups := []struct {
		name    string
		modules []ModuleSelection
	}{
		{name: "application", modules: manifest.Modules.Application},
		{name: "tools", modules: manifest.Modules.Tools},
	}
	for _, group := range groups {
		if len(group.modules) == 0 {
			return fmt.Errorf("modules.%s must not be empty", group.name)
		}
		seen := make(map[string]struct{}, len(group.modules))
		for _, module := range group.modules {
			if err := validateModuleIdentity(module.Path, module.Version); err != nil {
				return fmt.Errorf("modules.%s: %w", group.name, err)
			}
			if strings.TrimSpace(module.License) == "" {
				return fmt.Errorf("modules.%s %s: license must not be empty", group.name, module.Path)
			}
			if err := validateHTTPSURL(module.SourceURL); err != nil {
				return fmt.Errorf("modules.%s %s source_url: %w", group.name, module.Path, err)
			}
			if _, exists := seen[module.Path]; exists {
				return fmt.Errorf("modules.%s contains duplicate path %s", group.name, module.Path)
			}
			seen[module.Path] = struct{}{}
		}
	}
	if len(manifest.Forbidden) == 0 {
		return errors.New("forbidden policy identities must not be empty")
	}
	seenForbidden := make(map[string]struct{}, len(manifest.Forbidden))
	for _, forbidden := range manifest.Forbidden {
		if err := validateModuleIdentity(forbidden.Path, forbidden.Version); err != nil {
			return fmt.Errorf("forbidden: %w", err)
		}
		if strings.TrimSpace(forbidden.Reason) == "" {
			return fmt.Errorf("forbidden %s@%s: reason must not be empty", forbidden.Path, forbidden.Version)
		}
		key := forbidden.Path + "@" + forbidden.Version
		if _, exists := seenForbidden[key]; exists {
			return fmt.Errorf("duplicate forbidden identity %s", key)
		}
		seenForbidden[key] = struct{}{}
	}
	return nil
}

func validateToolchains(manifest ToolchainsManifest) error {
	if manifest.SchemaVersion != ToolchainsSchema {
		return fmt.Errorf("schema_version must be %q", ToolchainsSchema)
	}
	if err := validateGoVersion(manifest.GoVersion); err != nil {
		return err
	}
	if len(manifest.Assets) == 0 {
		return errors.New("assets must not be empty")
	}
	seenAssets := make(map[string]struct{}, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		if strings.TrimSpace(asset.ID) == "" || strings.TrimSpace(asset.Version) == "" || strings.TrimSpace(asset.Platform) == "" {
			return errors.New("asset id, version, and platform must not be empty")
		}
		if err := validateHTTPSURL(asset.URL); err != nil {
			return fmt.Errorf("asset %s URL: %w", asset.ID, err)
		}
		if err := validateSHA256(asset.SHA256); err != nil {
			return fmt.Errorf("asset %s SHA-256: %w", asset.ID, err)
		}
		switch asset.Kind {
		case "binary", "tar.gz", "tar.xz", "zip":
		default:
			return fmt.Errorf("asset %s has unsupported kind %q", asset.ID, asset.Kind)
		}
		if err := validateArchiveRelative(asset.Executable); err != nil {
			return fmt.Errorf("asset %s executable: %w", asset.ID, err)
		}
		if asset.Attestation != nil {
			if err := validateHTTPSURL(asset.Attestation.URL); err != nil {
				return fmt.Errorf("asset %s attestation URL: %w", asset.ID, err)
			}
			if err := validateSHA256(asset.Attestation.SHA256); err != nil {
				return fmt.Errorf("asset %s attestation SHA-256: %w", asset.ID, err)
			}
		}
		key := asset.ID + "@" + asset.Version + "/" + canonicalPlatform(asset.Platform)
		if _, exists := seenAssets[key]; exists {
			return fmt.Errorf("duplicate asset identity %s", key)
		}
		seenAssets[key] = struct{}{}
	}
	if len(manifest.Modules) == 0 {
		return errors.New("Bazel modules must not be empty")
	}
	seenModules := make(map[string]struct{}, len(manifest.Modules))
	for _, module := range manifest.Modules {
		if strings.TrimSpace(module.Name) == "" || strings.TrimSpace(module.Version) == "" {
			return errors.New("Bazel module name and version must not be empty")
		}
		if err := validateHTTPSURL(module.URL); err != nil {
			return fmt.Errorf("Bazel module %s URL: %w", module.Name, err)
		}
		if err := validateIntegrity(module.Integrity); err != nil {
			return fmt.Errorf("Bazel module %s integrity: %w", module.Name, err)
		}
		key := module.Name + "@" + module.Version
		if _, exists := seenModules[key]; exists {
			return fmt.Errorf("duplicate Bazel module identity %s", key)
		}
		seenModules[key] = struct{}{}
	}
	if len(manifest.Images) == 0 {
		return errors.New("images must not be empty")
	}
	seenImages := make(map[string]struct{}, len(manifest.Images))
	for _, image := range manifest.Images {
		if strings.TrimSpace(image.Name) == "" || strings.TrimSpace(image.Version) == "" {
			return errors.New("image name and version must not be empty")
		}
		if !strings.HasPrefix(image.IndexDigest, "sha256:") {
			return fmt.Errorf("image %s index_digest must use sha256", image.Name)
		}
		if err := validateSHA256(strings.TrimPrefix(image.IndexDigest, "sha256:")); err != nil {
			return fmt.Errorf("image %s index_digest: %w", image.Name, err)
		}
		if err := validateHTTPSURL(image.SourceURL); err != nil {
			return fmt.Errorf("image %s source_url: %w", image.Name, err)
		}
		key := image.Name + "@" + image.Version
		if _, exists := seenImages[key]; exists {
			return fmt.Errorf("duplicate image identity %s", key)
		}
		seenImages[key] = struct{}{}
	}
	return nil
}

func validateGoVersion(version string) error {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return fmt.Errorf("go_version %q must contain major, minor, and patch", version)
	}
	for _, part := range parts {
		if part == "" {
			return fmt.Errorf("go_version %q is malformed", version)
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return fmt.Errorf("go_version %q is malformed", version)
			}
		}
	}
	return nil
}

func validateModuleIdentity(path, version string) error {
	if strings.TrimSpace(path) == "" || strings.ContainsAny(path, " \t\r\n@") {
		return fmt.Errorf("invalid module path %q", path)
	}
	if !strings.HasPrefix(version, "v") || strings.ContainsAny(version, " \t\r\n") {
		return fmt.Errorf("invalid version %q for module %s", version, path)
	}
	return nil
}

func validateHTTPSURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("must be an absolute HTTPS URL, got %q", raw)
	}
	return nil
}

func validateSHA256(value string) error {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return errors.New("must be 64 lowercase hexadecimal characters")
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return errors.New("must be 64 lowercase hexadecimal characters")
	}
	return nil
}

func validateIntegrity(value string) error {
	const prefix = "sha256-"
	if !strings.HasPrefix(value, prefix) {
		return errors.New("must use sha256- base64 integrity")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil || len(decoded) != sha256.Size {
		return errors.New("must contain exactly one base64-encoded SHA-256 digest")
	}
	return nil
}

func validateArchiveRelative(value string) error {
	if value == "" || filepath.IsAbs(value) || strings.Contains(value, "\\") {
		return fmt.Errorf("must be a nonempty archive-relative slash path, got %q", value)
	}
	clean := filepath.ToSlash(filepath.Clean(value))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != value {
		return fmt.Errorf("must be a canonical archive-relative slash path, got %q", value)
	}
	return nil
}

func loadStrictJSON[T any](path string) (T, error) {
	var value T
	data, err := readBounded(path, maxJSONBytes)
	if err != nil {
		return value, err
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return value, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return value, err
	}
	return value, nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("input exceeds %d-byte limit", limit)
	}
	return data, nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder, "$", 0); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func consumeJSONValue(decoder *json.Decoder, location string, depth int) error {
	if depth > 128 {
		return fmt.Errorf("JSON nesting exceeds limit at %s", location)
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("non-string JSON object key at %s", location)
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("duplicate JSON key %q at %s", key, location)
			}
			seen[key] = struct{}{}
			if err := consumeJSONValue(decoder, location+"."+key, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return fmt.Errorf("malformed JSON object at %s", location)
		}
	case '[':
		index := 0
		for decoder.More() {
			if err := consumeJSONValue(decoder, fmt.Sprintf("%s[%d]", location, index), depth+1); err != nil {
				return err
			}
			index++
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return fmt.Errorf("malformed JSON array at %s", location)
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q at %s", delimiter, location)
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

type goListModule struct {
	Path     string        `json:"Path"`
	Version  string        `json:"Version"`
	Main     bool          `json:"Main"`
	Indirect bool          `json:"Indirect"`
	Sum      string        `json:"Sum"`
	GoModSum string        `json:"GoModSum"`
	Replace  *goListModule `json:"Replace"`
	Error    *struct {
		Err string `json:"Err"`
	} `json:"Error"`
}

type moduleGraph struct {
	main      string
	modules   map[string]goListModule
	declared  map[string]string
	edges     map[string][]string
	edgeCount int
}

func resolveModuleGraph(ctx context.Context, goCommand, root, expectedMain string, allowLocalReplacements bool) (moduleGraph, error) {
	var graph moduleGraph
	listOutput, err := runGo(ctx, goCommand, root, "list", "-mod=readonly", "-m", "-json", "all")
	if err != nil {
		return graph, err
	}
	modules := make(map[string]goListModule)
	decoder := json.NewDecoder(bytes.NewReader(listOutput))
	mainCount := 0
	for {
		var module goListModule
		if err := decoder.Decode(&module); err != nil {
			if err == io.EOF {
				break
			}
			return graph, fmt.Errorf("decode go list output: %w", err)
		}
		if module.Error != nil {
			return graph, fmt.Errorf("go list module %s: %s", module.Path, module.Error.Err)
		}
		if module.Path == "" {
			return graph, errors.New("go list returned a module without a path")
		}
		if _, exists := modules[module.Path]; exists {
			return graph, fmt.Errorf("go list returned duplicate module path %s", module.Path)
		}
		if module.Main {
			mainCount++
			if module.Path != expectedMain {
				return graph, fmt.Errorf("main module is %s; expected %s", module.Path, expectedMain)
			}
		} else {
			if module.Version == "" {
				return graph, fmt.Errorf("resolved module %s has no version", module.Path)
			}
			if module.Replace != nil && !allowLocalReplacements {
				return graph, fmt.Errorf("resolved module %s@%s uses replacement %s; locked graphs must not use replacements", module.Path, module.Version, module.Replace.Path)
			}
			if module.Replace == nil && (module.Sum == "" || module.GoModSum == "") {
				return graph, fmt.Errorf("resolved module %s@%s lacks complete module and go.mod sums", module.Path, module.Version)
			}
		}
		modules[module.Path] = module
	}
	if mainCount != 1 {
		return graph, fmt.Errorf("go list returned %d main modules; GOWORK=off requires exactly one", mainCount)
	}

	graphOutput, err := runGo(ctx, goCommand, root, "mod", "graph")
	if err != nil {
		return graph, err
	}
	edges := make(map[string][]string)
	declared := make(map[string]string)
	edgeSet := make(map[string]struct{})
	scanner := bufio.NewScanner(bytes.NewReader(graphOutput))
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			return graph, fmt.Errorf("malformed go mod graph line %q", scanner.Text())
		}
		from, okFrom := normalizeGraphNode(fields[0], expectedMain, modules)
		to, okTo := normalizeGraphNode(fields[1], expectedMain, modules)
		if !okFrom || !okTo {
			continue
		}
		if fields[0] == expectedMain {
			path, version, _ := strings.Cut(fields[1], "@")
			declared[path] = version
		}
		key := from + "\x00" + to
		if _, exists := edgeSet[key]; exists {
			continue
		}
		edgeSet[key] = struct{}{}
		edges[from] = append(edges[from], to)
	}
	if err := scanner.Err(); err != nil {
		return graph, fmt.Errorf("read go mod graph: %w", err)
	}
	for node := range edges {
		sort.Strings(edges[node])
	}
	return moduleGraph{main: expectedMain, modules: modules, declared: declared, edges: edges, edgeCount: len(edgeSet)}, nil
}

func normalizeGraphNode(node, main string, modules map[string]goListModule) (string, bool) {
	path := node
	if index := strings.LastIndex(node, "@"); index >= 0 {
		path = node[:index]
	}
	if path == main {
		return main, true
	}
	module, exists := modules[path]
	if !exists || module.Main {
		return "", false
	}
	return module.Path + "@" + module.Version, true
}

func checkSelectedModules(scope string, graph moduleGraph, selected []ModuleSelection) error {
	expected := make(map[string]ModuleSelection, len(selected))
	for _, module := range selected {
		expected[module.Path] = module
		resolved, exists := graph.modules[module.Path]
		if !exists {
			return fmt.Errorf("%s graph is missing selected module %s@%s", scope, module.Path, module.Version)
		}
		if resolved.Version != module.Version {
			return fmt.Errorf("%s graph resolved %s@%s; selected version is %s", scope, module.Path, resolved.Version, module.Version)
		}
		if graph.declared[module.Path] != module.Version {
			return fmt.Errorf("%s go.mod does not explicitly declare selected pin %s@%s", scope, module.Path, module.Version)
		}
	}
	for path, resolved := range graph.modules {
		if resolved.Main || resolved.Indirect {
			continue
		}
		if _, exists := expected[path]; !exists {
			return fmt.Errorf("%s graph has undeclared direct module %s@%s", scope, path, resolved.Version)
		}
	}
	return nil
}

func checkGraphForbidden(scope string, graph moduleGraph, forbidden []ForbiddenModule) error {
	for _, rule := range forbidden {
		module, exists := graph.modules[rule.Path]
		if !exists || module.Main || !matchesVersion(module.Version, rule.Version) {
			continue
		}
		target := module.Path + "@" + module.Version
		chain := graphPath(graph, target)
		return &PolicyViolationError{
			Scope:    scope + " module graph",
			Identity: target,
			Reason:   rule.Reason,
			Chain:    chain,
		}
	}
	return nil
}

func graphPath(graph moduleGraph, target string) []string {
	queue := []string{graph.main}
	previous := map[string]string{graph.main: ""}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current == target {
			break
		}
		for _, next := range graph.edges[current] {
			if _, seen := previous[next]; seen {
				continue
			}
			previous[next] = current
			queue = append(queue, next)
		}
	}
	if _, found := previous[target]; !found {
		return []string{graph.main, target + " (resolved; dependency edge unavailable)"}
	}
	path := make([]string, 0, 4)
	for current := target; current != ""; current = previous[current] {
		path = append(path, current)
	}
	for left, right := 0, len(path)-1; left < right; left, right = left+1, right-1 {
		path[left], path[right] = path[right], path[left]
	}
	return path
}

func resolvedGoVersion(ctx context.Context, goCommand, root string) (string, error) {
	output, err := runGo(ctx, goCommand, root, "version")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(output))
	if len(fields) < 3 || fields[0] != "go" || fields[1] != "version" || !strings.HasPrefix(fields[2], "go") {
		return "", fmt.Errorf("unexpected go version output %q", strings.TrimSpace(string(output)))
	}
	return strings.TrimPrefix(fields[2], "go"), nil
}

func runGo(ctx context.Context, goCommand, root string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, goCommand, args...)
	command.Dir = root
	command.Env = goEnvironment()
	stdout := newCappedBuffer(maxCommandOutput)
	stderr := newCappedBuffer(maxCommandError)
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("%s %s: %w", goCommand, strings.Join(args, " "), ctxErr)
	}
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", goCommand, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	if stdout.truncated {
		return nil, fmt.Errorf("%s %s output exceeded %d bytes", goCommand, strings.Join(args, " "), maxCommandOutput)
	}
	if stderr.truncated {
		return nil, fmt.Errorf("%s %s diagnostic output exceeded %d bytes", goCommand, strings.Join(args, " "), maxCommandError)
	}
	return stdout.Bytes(), nil
}

func goEnvironment() []string {
	environment := os.Environ()
	filtered := environment[:0]
	for _, entry := range environment {
		key := entry
		if index := strings.IndexByte(entry, '='); index >= 0 {
			key = entry[:index]
		}
		switch key {
		case "GOWORK", "GOTOOLCHAIN", "GOFLAGS":
			continue
		default:
			filtered = append(filtered, entry)
		}
	}
	return append(filtered, "GOWORK=off", "GOTOOLCHAIN=local", "GOFLAGS=")
}

type cappedBuffer struct {
	buffer    bytes.Buffer
	remaining int
	truncated bool
}

func newCappedBuffer(limit int) *cappedBuffer {
	return &cappedBuffer{remaining: limit}
}

func (b *cappedBuffer) Write(data []byte) (int, error) {
	originalLength := len(data)
	if len(data) > b.remaining {
		data = data[:b.remaining]
		b.truncated = true
	}
	if len(data) > 0 {
		_, _ = b.buffer.Write(data)
		b.remaining -= len(data)
	}
	return originalLength, nil
}

func (b *cappedBuffer) Bytes() []byte {
	return b.buffer.Bytes()
}

func (b *cappedBuffer) String() string {
	return b.buffer.String()
}

type sbomComponent struct {
	Type       string
	Name       string
	ModulePath string
	Version    string
	SHA256     []string
}

type sbomDocument struct {
	format                 string
	documentSHA256         string
	subjectName            string
	subjectVersion         string
	recognizedPackages     int
	unversionedPackages    []string
	inventoriedFiles       int
	inventoriedFileSHA256s int
	components             []sbomComponent
	locked                 []string
}

func (d sbomDocument) inventoryScope() string {
	switch {
	case d.recognizedPackages > 0 && d.inventoriedFiles > 0:
		return inventoryScopeRecognizedPackagesAndInstalledFiles
	case d.recognizedPackages > 0:
		return inventoryScopeRecognizedPackages
	case d.inventoriedFiles > 0:
		return inventoryScopeInstalledFiles
	default:
		return ""
	}
}

type cycloneDXDocument struct {
	BOMFormat   string               `json:"bomFormat"`
	SpecVersion string               `json:"specVersion"`
	Metadata    cycloneDXMetadata    `json:"metadata"`
	Components  []cycloneDXComponent `json:"components"`
}

type cycloneDXMetadata struct {
	Component *cycloneDXComponent `json:"component"`
	Tools     json.RawMessage     `json:"tools"`
}

type cycloneDXComponent struct {
	Type       string               `json:"type"`
	Name       string               `json:"name"`
	Version    string               `json:"version"`
	PURL       string               `json:"purl"`
	Hashes     []cycloneDXHash      `json:"hashes"`
	Components []cycloneDXComponent `json:"components"`
}

type cycloneDXHash struct {
	Algorithm string `json:"alg"`
	Content   string `json:"content"`
}

type spdxDocument struct {
	SPDXVersion       string             `json:"spdxVersion"`
	DataLicense       string             `json:"dataLicense"`
	SPDXID            string             `json:"SPDXID"`
	Name              string             `json:"name"`
	DocumentNamespace string             `json:"documentNamespace"`
	DocumentDescribes []string           `json:"documentDescribes"`
	Packages          []spdxPackage      `json:"packages"`
	Relationships     []spdxRelationship `json:"relationships"`
}

type spdxPackage struct {
	SPDXID       string            `json:"SPDXID"`
	Name         string            `json:"name"`
	VersionInfo  string            `json:"versionInfo"`
	Checksums    []spdxChecksum    `json:"checksums"`
	ExternalRefs []spdxExternalRef `json:"externalRefs"`
}

type spdxRelationship struct {
	ElementID string `json:"spdxElementId"`
	Type      string `json:"relationshipType"`
	RelatedID string `json:"relatedSpdxElement"`
}

type spdxChecksum struct {
	Algorithm     string `json:"algorithm"`
	ChecksumValue string `json:"checksumValue"`
}

type spdxExternalRef struct {
	ReferenceType    string `json:"referenceType"`
	ReferenceLocator string `json:"referenceLocator"`
}

func loadSBOM(path string) (sbomDocument, error) {
	var document sbomDocument
	data, err := readBounded(path, maxJSONBytes)
	if err != nil {
		return document, err
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return document, err
	}
	var kind struct {
		BOMFormat   string `json:"bomFormat"`
		SPDXVersion string `json:"spdxVersion"`
	}
	if err := json.Unmarshal(data, &kind); err != nil {
		return document, err
	}
	switch {
	case kind.BOMFormat == "CycloneDX" && kind.SPDXVersion == "":
		document, err = parseCycloneDX(data)
	case strings.HasPrefix(kind.SPDXVersion, "SPDX-2.") && kind.BOMFormat == "":
		document, err = parseSPDX(data)
	default:
		err = errors.New("SBOM must be one unambiguous CycloneDX JSON or SPDX 2.x JSON document")
	}
	if err != nil {
		return sbomDocument{}, err
	}
	digest := sha256.Sum256(data)
	document.documentSHA256 = hex.EncodeToString(digest[:])
	if document.inventoryScope() == "" {
		return sbomDocument{}, fmt.Errorf("%s SBOM for %s@%s contains no recognized package or hashed file inventory components", document.format, document.subjectName, document.subjectVersion)
	}
	return document, nil
}

func parseCycloneDX(data []byte) (sbomDocument, error) {
	var document sbomDocument
	var source cycloneDXDocument
	if err := json.Unmarshal(data, &source); err != nil {
		return document, err
	}
	if source.BOMFormat != "CycloneDX" || strings.TrimSpace(source.SpecVersion) == "" {
		return document, errors.New("CycloneDX bomFormat and specVersion are required")
	}
	if source.Metadata.Component == nil {
		return document, errors.New("CycloneDX metadata.component must identify the scanned tool")
	}
	subjectComponents := make([]sbomComponent, 0, 1)
	if err := collectCycloneDXComponent(*source.Metadata.Component, &subjectComponents); err != nil {
		return document, fmt.Errorf("CycloneDX metadata component: %w", err)
	}
	subject := subjectComponents[0]
	if strings.TrimSpace(subject.Type) == "" || strings.TrimSpace(subject.Version) == "" {
		return document, errors.New("CycloneDX metadata.component type, name, and version are required for scanned-tool identity")
	}
	document.format = "cyclonedx"
	document.subjectName = subject.Name
	document.subjectVersion = subject.Version
	document.components = append(document.components, subjectComponents...)
	evidence := append([]sbomComponent(nil), subjectComponents[1:]...)
	for _, sourceComponent := range source.Components {
		collected := make([]sbomComponent, 0, 1)
		if err := collectCycloneDXComponent(sourceComponent, &collected); err != nil {
			return document, fmt.Errorf("CycloneDX component: %w", err)
		}
		document.components = append(document.components, collected...)
		evidence = append(evidence, collected...)
	}
	for _, component := range evidence {
		if err := validateDiscoveredComponent(component); err != nil {
			return document, fmt.Errorf("CycloneDX discovered component: %w", err)
		}
		if strings.EqualFold(component.Type, "file") {
			document.inventoriedFiles++
			document.inventoriedFileSHA256s += len(component.SHA256)
			continue
		}
		document.recognizedPackages++
		if unversionedComponent(component.Version) {
			document.unversionedPackages = append(document.unversionedPackages, component.Name)
		}
	}
	if len(source.Metadata.Tools) > 0 && string(source.Metadata.Tools) != "null" {
		toolComponents, err := parseCycloneDXTools(source.Metadata.Tools)
		if err != nil {
			return document, err
		}
		for _, component := range toolComponents {
			collected := make([]sbomComponent, 0, 1)
			if err := collectCycloneDXComponent(component, &collected); err != nil {
				return document, fmt.Errorf("CycloneDX metadata tool: %w", err)
			}
			document.components = append(document.components, collected...)
		}
	}
	return document, nil
}

func parseCycloneDXTools(data json.RawMessage) ([]cycloneDXComponent, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if trimmed[0] == '[' {
		var legacy []cycloneDXComponent
		if err := json.Unmarshal(trimmed, &legacy); err != nil {
			return nil, fmt.Errorf("decode CycloneDX legacy metadata tools: %w", err)
		}
		return legacy, nil
	}
	if trimmed[0] == '{' {
		var current struct {
			Components []cycloneDXComponent `json:"components"`
		}
		if err := json.Unmarshal(trimmed, &current); err != nil {
			return nil, fmt.Errorf("decode CycloneDX metadata tools: %w", err)
		}
		return current.Components, nil
	}
	return nil, errors.New("CycloneDX metadata.tools must be an object or array")
}

func collectCycloneDXComponent(source cycloneDXComponent, output *[]sbomComponent) error {
	if strings.TrimSpace(source.Name) == "" {
		return errors.New("component name must not be empty")
	}
	component := sbomComponent{Type: strings.TrimSpace(source.Type), Name: strings.TrimSpace(source.Name), Version: strings.TrimSpace(source.Version)}
	if source.PURL != "" {
		modulePath, version, err := parsePURL(source.PURL)
		if err != nil {
			return err
		}
		component.ModulePath = modulePath
		if component.Version == "" {
			component.Version = version
		} else if version != "" && !componentVersionsEqual(source.PURL, modulePath, component.Version, version) {
			return fmt.Errorf("component %s has conflicting version and purl", source.Name)
		}
	}
	for _, hash := range source.Hashes {
		if !isSHA256Algorithm(hash.Algorithm) {
			continue
		}
		if err := validateSHA256(strings.ToLower(hash.Content)); err != nil {
			return fmt.Errorf("component %s has malformed SHA-256 digest", source.Name)
		}
		component.SHA256 = append(component.SHA256, strings.ToLower(hash.Content))
	}
	*output = append(*output, component)
	for _, nested := range source.Components {
		if err := collectCycloneDXComponent(nested, output); err != nil {
			return err
		}
	}
	return nil
}

func validateDiscoveredComponent(component sbomComponent) error {
	if component.Type == "" {
		return fmt.Errorf("component %s has no declared type", component.Name)
	}
	if strings.EqualFold(component.Type, "file") {
		if len(component.SHA256) == 0 {
			return fmt.Errorf("file component %s has no installed-content SHA-256", component.Name)
		}
		return nil
	}
	return nil
}

// Source-root components need not have independent release versions. Keep them
// visible; an unknown version must never evade a matching version-scoped denial.
func unversionedComponent(version string) bool {
	switch strings.ToLower(strings.TrimSpace(version)) {
	case "", "noassertion", "none", "unknown", "(devel)":
		return true
	}
	return false
}

func parseSPDX(data []byte) (sbomDocument, error) {
	var document sbomDocument
	var source spdxDocument
	if err := json.Unmarshal(data, &source); err != nil {
		return document, err
	}
	if !strings.HasPrefix(source.SPDXVersion, "SPDX-2.") || source.DataLicense != "CC0-1.0" || source.SPDXID != "SPDXRef-DOCUMENT" {
		return document, errors.New("SPDX 2.x spdxVersion, CC0-1.0 dataLicense, and SPDXRef-DOCUMENT identity are required")
	}
	if parsed, err := url.Parse(source.DocumentNamespace); err != nil || !parsed.IsAbs() {
		return document, errors.New("SPDX documentNamespace must be an absolute URI")
	}
	if strings.TrimSpace(source.Name) == "" {
		return document, errors.New("SPDX document name must identify the scanned tool")
	}
	described := make(map[string]struct{})
	for _, identifier := range source.DocumentDescribes {
		if strings.TrimSpace(identifier) == "" {
			return document, errors.New("SPDX documentDescribes contains an empty identifier")
		}
		described[identifier] = struct{}{}
	}
	for _, relationship := range source.Relationships {
		switch {
		case relationship.ElementID == source.SPDXID && strings.EqualFold(relationship.Type, "DESCRIBES"):
			described[relationship.RelatedID] = struct{}{}
		case relationship.RelatedID == source.SPDXID && strings.EqualFold(relationship.Type, "DESCRIBED_BY"):
			described[relationship.ElementID] = struct{}{}
		}
	}
	if len(described) != 1 {
		return document, fmt.Errorf("SPDX document must describe exactly one scanned-tool root package; found %d", len(described))
	}
	var subjectID string
	for identifier := range described {
		subjectID = identifier
	}

	packageIDs := make(map[string]struct{}, len(source.Packages))
	subjectIndex := -1
	components := make([]sbomComponent, 0, len(source.Packages))
	for index, pkg := range source.Packages {
		if strings.TrimSpace(pkg.SPDXID) == "" {
			return document, errors.New("SPDX package SPDXID must not be empty")
		}
		if _, exists := packageIDs[pkg.SPDXID]; exists {
			return document, fmt.Errorf("SPDX package identifier %s is duplicated", pkg.SPDXID)
		}
		packageIDs[pkg.SPDXID] = struct{}{}
		component, err := convertSPDXPackage(pkg)
		if err != nil {
			return document, err
		}
		components = append(components, component)
		if pkg.SPDXID == subjectID {
			subjectIndex = index
		}
	}
	if subjectIndex < 0 {
		return document, fmt.Errorf("SPDX described root package %s is absent", subjectID)
	}
	subject := components[subjectIndex]
	if strings.TrimSpace(subject.Version) == "" || strings.EqualFold(subject.Version, "NOASSERTION") {
		return document, errors.New("SPDX described root package requires the scanned tool version")
	}
	if strings.TrimSpace(source.Name) != subject.Name {
		return document, fmt.Errorf("SPDX document name %q does not match described root package %q", strings.TrimSpace(source.Name), subject.Name)
	}
	document.format = "spdx"
	document.subjectName = subject.Name
	document.subjectVersion = subject.Version
	document.components = components
	for index, component := range components {
		if index == subjectIndex {
			continue
		}
		if err := validateDiscoveredComponent(component); err != nil {
			return document, fmt.Errorf("SPDX discovered package: %w", err)
		}
		document.recognizedPackages++
		if unversionedComponent(component.Version) {
			document.unversionedPackages = append(document.unversionedPackages, component.Name)
		}
	}
	return document, nil
}

func convertSPDXPackage(pkg spdxPackage) (sbomComponent, error) {
	if strings.TrimSpace(pkg.Name) == "" {
		return sbomComponent{}, errors.New("SPDX package name must not be empty")
	}
	component := sbomComponent{Type: "package", Name: strings.TrimSpace(pkg.Name), Version: strings.TrimSpace(pkg.VersionInfo)}
	for _, reference := range pkg.ExternalRefs {
		if !strings.EqualFold(reference.ReferenceType, "purl") {
			continue
		}
		modulePath, version, err := parsePURL(reference.ReferenceLocator)
		if err != nil {
			return sbomComponent{}, fmt.Errorf("SPDX package %s: %w", pkg.Name, err)
		}
		if component.ModulePath != "" && component.ModulePath != modulePath {
			return sbomComponent{}, fmt.Errorf("SPDX package %s has conflicting purl identities", pkg.Name)
		}
		component.ModulePath = modulePath
		if component.Version == "" || strings.EqualFold(component.Version, "NOASSERTION") {
			component.Version = version
		} else if version != "" && !componentVersionsEqual(reference.ReferenceLocator, modulePath, component.Version, version) {
			return sbomComponent{}, fmt.Errorf("SPDX package %s has conflicting version and purl", pkg.Name)
		}
	}
	for _, checksum := range pkg.Checksums {
		if !isSHA256Algorithm(checksum.Algorithm) {
			continue
		}
		if err := validateSHA256(strings.ToLower(checksum.ChecksumValue)); err != nil {
			return sbomComponent{}, fmt.Errorf("SPDX package %s has malformed SHA-256 digest", pkg.Name)
		}
		component.SHA256 = append(component.SHA256, strings.ToLower(checksum.ChecksumValue))
	}
	return component, nil
}

func parsePURL(value string) (string, string, error) {
	if !strings.HasPrefix(value, "pkg:") {
		return "", "", fmt.Errorf("invalid purl %q", value)
	}
	body := strings.TrimPrefix(value, "pkg:")
	if index := strings.IndexAny(body, "?#"); index >= 0 {
		body = body[:index]
	}
	slash := strings.IndexByte(body, '/')
	if slash <= 0 || slash == len(body)-1 {
		return "", "", fmt.Errorf("invalid purl %q", value)
	}
	identity := body[slash+1:]
	version := ""
	if index := strings.LastIndexByte(identity, '@'); index >= 0 {
		version = identity[index+1:]
		identity = identity[:index]
	}
	decodedIdentity, err := url.PathUnescape(identity)
	if err != nil || decodedIdentity == "" {
		return "", "", fmt.Errorf("invalid purl identity %q", value)
	}
	decodedVersion, err := url.PathUnescape(version)
	if err != nil {
		return "", "", fmt.Errorf("invalid purl version %q", value)
	}
	return decodedIdentity, decodedVersion, nil
}

func isSHA256Algorithm(value string) bool {
	normalized := strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToUpper(value))
	return normalized == "SHA256"
}

func checkSBOMForbidden(document sbomDocument, forbidden []ForbiddenModule) error {
	for _, component := range document.components {
		identities := []string{component.ModulePath, component.Name}
		for _, identity := range identities {
			if identity == "" {
				continue
			}
			for _, rule := range forbidden {
				if identity == rule.Path && (unversionedComponent(component.Version) || matchesVersion(component.Version, rule.Version)) {
					selected := identity + "@" + component.Version
					return &PolicyViolationError{
						Scope:    document.format + " SBOM " + document.documentSHA256,
						Identity: selected,
						Reason:   rule.Reason,
						Chain:    []string{"external SBOM", selected},
					}
				}
			}
		}
	}
	return nil
}

func checkSBOMCoverage(toolchains ToolchainsManifest, documents []sbomDocument) ([]AssetResult, []SBOMResult, error) {
	currentPlatform := canonicalPlatform(runtime.GOOS + "-" + runtime.GOARCH)
	required := make([]ToolchainAsset, 0)
	for _, asset := range toolchains.Assets {
		platform := canonicalPlatform(asset.Platform)
		if platform == currentPlatform || platform == "all" || platform == "any" || platform == "portable" {
			required = append(required, asset)
		}
	}
	if len(required) == 0 {
		return nil, nil, fmt.Errorf("toolchain lock contains no assets for current platform %s", currentPlatform)
	}
	sort.Slice(required, func(left, right int) bool {
		if required[left].ID != required[right].ID {
			return required[left].ID < required[right].ID
		}
		if required[left].Version != required[right].Version {
			return required[left].Version < required[right].Version
		}
		return required[left].Platform < required[right].Platform
	})
	assetResults := make([]AssetResult, 0, len(required))
	for _, asset := range required {
		covered := false
		observedVersions := make([]string, 0)
		identity := asset.ID + "@" + asset.Version + "/" + canonicalPlatform(asset.Platform)
		for documentIndex := range documents {
			document := &documents[documentIndex]
			if document.subjectName != asset.ID {
				continue
			}
			observedVersions = append(observedVersions, document.subjectVersion)
			if document.subjectVersion != asset.Version {
				continue
			}
			document.locked = append(document.locked, identity)
			if covered {
				continue
			}
			assetResults = append(assetResults, AssetResult{
				ID:                     asset.ID,
				Version:                asset.Version,
				Platform:               canonicalPlatform(asset.Platform),
				SourceSHA256:           asset.SHA256,
				SBOMDocumentSHA256:     document.documentSHA256,
				InventoryScope:         document.inventoryScope(),
				RecognizedPackages:     document.recognizedPackages,
				InventoriedFiles:       document.inventoriedFiles,
				InventoriedFileSHA256s: document.inventoriedFileSHA256s,
			})
			covered = true
		}
		if covered {
			continue
		}
		if len(observedVersions) > 0 {
			sort.Strings(observedVersions)
			return nil, nil, fmt.Errorf("external SBOM subject version mismatch for locked asset %s: observed %v", identity, observedVersions)
		}
		return nil, nil, fmt.Errorf("external SBOM evidence is missing scanned-tool subject %s", identity)
	}
	sbomResults := make([]SBOMResult, 0, len(documents))
	for _, document := range documents {
		if len(document.locked) == 0 {
			return nil, nil, fmt.Errorf("external %s SBOM %s subject %s@%s is not a current-platform locked asset", document.format, document.documentSHA256, document.subjectName, document.subjectVersion)
		}
		sort.Strings(document.locked)
		sbomResults = append(sbomResults, SBOMResult{
			Format:                 document.format,
			DocumentSHA256:         document.documentSHA256,
			SubjectName:            document.subjectName,
			SubjectVersion:         document.subjectVersion,
			InventoryScope:         document.inventoryScope(),
			RecognizedPackages:     document.recognizedPackages,
			UnversionedPackages:    document.unversionedPackages,
			InventoriedFiles:       document.inventoriedFiles,
			InventoriedFileSHA256s: document.inventoriedFileSHA256s,
			LockedIdentities:       document.locked,
		})
	}
	return assetResults, sbomResults, nil
}

func matchesVersion(actual, policy string) bool {
	return versionsEqual(actual, policy)
}

func componentVersionsEqual(purl, identity, left, right string) bool {
	// Go's standard-library build version includes "go"; its PURL does not.
	if strings.HasPrefix(purl, "pkg:golang/") && identity == "stdlib" {
		left = strings.TrimPrefix(left, "go")
		right = strings.TrimPrefix(right, "go")
	}
	return versionsEqual(left, right)
}

func versionsEqual(left, right string) bool {
	if left == right {
		return true
	}
	return strings.TrimPrefix(left, "v") == strings.TrimPrefix(right, "v")
}

func canonicalPlatform(platform string) string {
	normalized := strings.ToLower(strings.TrimSpace(platform))
	normalized = strings.NewReplacer("_", "-", "/", "-", "macos", "darwin", "osx", "darwin", "x86-64", "amd64", "x86_64", "amd64", "aarch64", "arm64").Replace(normalized)
	return normalized
}
