package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	syntheticDigestA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	syntheticDigestB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestResolvedDependencyPolicy(t *testing.T) {
	repoRoot := dependencyPolicyRepoRoot(t)
	goCommand := os.Getenv("PM_GO")
	if goCommand == "" {
		goCommand = "go"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	t.Run("selected application and tools graphs resolve exactly", func(t *testing.T) {
		manifest, err := loadStrictJSON[VersionsManifest](filepath.Join(repoRoot, "tools", "versions.json"))
		if err != nil {
			t.Fatalf("load selected versions: %v", err)
		}
		if err := validateVersions(manifest); err != nil {
			t.Fatalf("validate selected versions: %v", err)
		}
		actualGoVersion, err := resolvedGoVersion(ctx, goCommand, repoRoot)
		if err != nil {
			t.Fatalf("resolve Go version: %v", err)
		}
		if actualGoVersion != manifest.GoVersion {
			t.Fatalf("resolved Go version = %s; want %s (set PM_GO to the pinned executable when needed)", actualGoVersion, manifest.GoVersion)
		}
		checks := []struct {
			name       string
			root       string
			mainModule string
			selected   []ModuleSelection
		}{
			{name: "application", root: repoRoot, mainModule: applicationModule, selected: manifest.Modules.Application},
			{name: "tools", root: filepath.Join(repoRoot, "tools"), mainModule: toolsModule, selected: manifest.Modules.Tools},
		}
		for _, check := range checks {
			graph, err := resolveModuleGraph(ctx, goCommand, check.root, check.mainModule, false)
			if err != nil {
				t.Fatalf("resolve %s graph: %v", check.name, err)
			}
			if err := checkSelectedModules(check.name, graph, check.selected); err != nil {
				t.Fatalf("check %s selections: %v", check.name, err)
			}
			if err := checkGraphForbidden(check.name, graph, manifest.Forbidden); err != nil {
				t.Fatalf("check %s forbidden identities: %v", check.name, err)
			}
		}
	})

	t.Run("unused exact GoReleaser transitive requirement is rejected with its chain", func(t *testing.T) {
		root := writeSyntheticModuleGraph(t, "v2.18.1")
		graph, err := resolveModuleGraph(ctx, goCommand, root, "example.test/root", true)
		if err != nil {
			t.Fatalf("resolve synthetic graph: %v", err)
		}
		selected := []ModuleSelection{{Path: "example.test/parent", Version: "v1.0.0"}}
		if err := checkSelectedModules("synthetic", graph, selected); err != nil {
			t.Fatalf("check selected synthetic parent: %v", err)
		}
		undeclaredPin := append(selected, ModuleSelection{Path: "github.com/goreleaser/goreleaser/v2", Version: "v2.18.1"})
		if err := checkSelectedModules("synthetic", graph, undeclaredPin); err == nil {
			t.Fatal("a transitive MVS result was accepted as an explicit manifest pin")
		}
		rule := ForbiddenModule{
			Path:    "github.com/goreleaser/goreleaser/v2",
			Version: "v2.18.1",
			Reason:  "V56 exact rejected tool identity",
		}
		err = checkGraphForbidden("synthetic", graph, []ForbiddenModule{rule})
		var violation *PolicyViolationError
		if !errors.As(err, &violation) {
			t.Fatalf("forbidden transitive edge error = %v; want PolicyViolationError", err)
		}
		wantChain := []string{
			"example.test/root",
			"example.test/parent@v1.0.0",
			"github.com/goreleaser/goreleaser/v2@v2.18.1",
		}
		if !reflect.DeepEqual(violation.Chain, wantChain) {
			t.Fatalf("forbidden chain = %v; want %v", violation.Chain, wantChain)
		}
	})

	t.Run("unlisted GoReleaser version is not broadened into a family rule", func(t *testing.T) {
		root := writeSyntheticModuleGraph(t, "v2.18.2")
		graph, err := resolveModuleGraph(ctx, goCommand, root, "example.test/root", true)
		if err != nil {
			t.Fatalf("resolve synthetic graph: %v", err)
		}
		rule := ForbiddenModule{
			Path:    "github.com/goreleaser/goreleaser/v2",
			Version: "v2.18.1",
			Reason:  "V56 exact rejected tool identity",
		}
		if err := checkGraphForbidden("synthetic", graph, []ForbiddenModule{rule}); err != nil {
			t.Fatalf("exact policy rejected a different version: %v", err)
		}
	})

	t.Run("CycloneDX and SPDX report package and file inventory scopes without conflating source and installed digests", func(t *testing.T) {
		platform := canonicalPlatform(runtime.GOOS + "-" + runtime.GOARCH)
		cyclone := writeTestFile(t, "tool-a.cdx.json", fmt.Sprintf(`{
  "bomFormat": "CycloneDX",
  "specVersion": "1.6",
  "version": 1,
  "metadata": {
    "component": {
      "type": "application",
      "name": "example-tool-a",
      "version": "1.2.3"
    },
    "tools": {
      "components": [{
        "type": "application",
        "name": "syft",
        "version": "1.51.1"
      }]
    }
  },
  "components": [{
    "type": "library",
    "name": "example-dependency-a",
    "version": "7.8.9",
    "purl": "pkg:generic/example-dependency-a@7.8.9"
  }, {
    "type": "file",
    "name": "bin/example-tool-a",
    "hashes": [{"alg": "SHA-256", "content": %q}]
  }]
}`, syntheticDigestB))
		spdx := writeTestFile(t, "tool-b.spdx.json", fmt.Sprintf(`{
  "spdxVersion": "SPDX-2.3",
  "dataLicense": "CC0-1.0",
  "SPDXID": "SPDXRef-DOCUMENT",
  "name": "example-tool-b",
  "documentNamespace": "https://example.invalid/spdx/tool-b",
  "packages": [{
    "name": "example-tool-b",
    "versionInfo": "4.5.6",
    "SPDXID": "SPDXRef-Root-tool-b",
    "downloadLocation": "NOASSERTION"
  }, {
    "name": "example-dependency-b",
    "versionInfo": "9.8.7",
    "SPDXID": "SPDXRef-Package-dependency-b",
    "downloadLocation": "NOASSERTION",
    "checksums": [{"algorithm": "SHA256", "checksumValue": %q}],
    "externalRefs": [{
      "referenceCategory": "PACKAGE-MANAGER",
      "referenceType": "purl",
      "referenceLocator": "pkg:generic/example-dependency-b@9.8.7"
    }]
  }],
  "relationships": [{
    "spdxElementId": "SPDXRef-DOCUMENT",
    "relationshipType": "DESCRIBES",
    "relatedSpdxElement": "SPDXRef-Root-tool-b"
  }]
}`, syntheticDigestA))
		documents := make([]sbomDocument, 0, 2)
		for _, path := range []string{cyclone, spdx} {
			document, err := loadSBOM(path)
			if err != nil {
				t.Fatalf("load %s: %v", filepath.Base(path), err)
			}
			documents = append(documents, document)
		}
		locks := ToolchainsManifest{Assets: []ToolchainAsset{
			{ID: "example-tool-a", Version: "1.2.3", Platform: platform, SHA256: syntheticDigestA},
			{ID: "example-tool-b", Version: "4.5.6", Platform: platform, SHA256: syntheticDigestB},
		}}
		assets, reports, err := checkSBOMCoverage(locks, documents)
		if err != nil {
			t.Fatalf("check repeated SBOM coverage: %v", err)
		}
		wantAssets := []AssetResult{
			{
				ID: "example-tool-a", Version: "1.2.3", Platform: platform,
				SourceSHA256: syntheticDigestA, SBOMDocumentSHA256: documents[0].documentSHA256,
				InventoryScope:     inventoryScopeRecognizedPackagesAndInstalledFiles,
				RecognizedPackages: 1, InventoriedFiles: 1, InventoriedFileSHA256s: 1,
			},
			{
				ID: "example-tool-b", Version: "4.5.6", Platform: platform,
				SourceSHA256: syntheticDigestB, SBOMDocumentSHA256: documents[1].documentSHA256,
				InventoryScope:     inventoryScopeRecognizedPackages,
				RecognizedPackages: 1,
			},
		}
		if !reflect.DeepEqual(assets, wantAssets) {
			t.Fatalf("covered assets = %#v; want %#v", assets, wantAssets)
		}
		if len(reports) != 2 ||
			reports[0].Format != "cyclonedx" ||
			reports[0].SubjectName != "example-tool-a" ||
			reports[0].InventoryScope != inventoryScopeRecognizedPackagesAndInstalledFiles ||
			reports[0].RecognizedPackages != 1 ||
			reports[0].InventoriedFiles != 1 ||
			reports[0].InventoriedFileSHA256s != 1 ||
			reports[1].Format != "spdx" ||
			reports[1].SubjectName != "example-tool-b" ||
			reports[1].InventoryScope != inventoryScopeRecognizedPackages ||
			reports[1].RecognizedPackages != 1 ||
			reports[1].InventoriedFiles != 0 ||
			reports[1].InventoriedFileSHA256s != 0 {
			t.Fatalf("SBOM reports = %#v; want explicit package/file inventory scopes", reports)
		}
	})

	t.Run("missing malformed and empty scanner evidence fails closed", func(t *testing.T) {
		_, err := CheckDependencies(ctx, CheckOptions{Root: repoRoot})
		if !errors.Is(err, ErrExternalEvidenceRequired) {
			t.Fatalf("zero-SBOM error = %v; want ErrExternalEvidenceRequired", err)
		}
		malformed := writeTestFile(t, "malformed.json", `{"bomFormat":"CycloneDX"`)
		if _, err := loadSBOM(malformed); err == nil {
			t.Fatal("malformed SBOM unexpectedly passed")
		}
		empty := writeTestFile(t, "empty.cdx.json", `{
  "bomFormat": "CycloneDX",
  "specVersion": "1.6",
  "metadata": {
    "component": {
      "type": "application",
      "name": "example-tool",
      "version": "1.0.0"
    }
  }
}`)
		if _, err := loadSBOM(empty); err == nil || !strings.Contains(err.Error(), "no recognized package or hashed file inventory") {
			t.Fatalf("empty scanner evidence error = %v; want explicit empty-inventory refusal", err)
		}
		badDigest := writeTestFile(t, "bad-digest.json", `{
  "bomFormat": "CycloneDX",
  "specVersion": "1.6",
  "metadata": {
    "component": {
      "type": "application",
      "name": "example-tool",
      "version": "1.0.0"
    }
  },
  "components": [{
    "type": "file",
    "name": "example-tool-file",
    "hashes": [{"alg": "SHA-256", "content": "not-a-digest"}]
  }]
}`)
		if _, err := loadSBOM(badDigest); err == nil || !strings.Contains(err.Error(), "malformed SHA-256") {
			t.Fatalf("bad installed-content digest error = %v; want malformed SHA-256 refusal", err)
		}
		fileOnly := writeTestFile(t, "file-only.cdx.json", fmt.Sprintf(`{
  "bomFormat": "CycloneDX",
  "specVersion": "1.6",
  "metadata": {
    "component": {
      "type": "application",
      "name": "example-tool",
      "version": "1.0.0"
    }
  },
  "components": [{
    "type": "file",
    "name": "bin/example-tool",
    "hashes": [{"alg": "SHA-256", "content": %q}]
  }]
}`, syntheticDigestA))
		document, err := loadSBOM(fileOnly)
		if err != nil {
			t.Fatalf("load hashed file-only inventory: %v", err)
		}
		if document.inventoryScope() != inventoryScopeInstalledFiles ||
			document.recognizedPackages != 0 ||
			document.inventoriedFiles != 1 ||
			document.inventoriedFileSHA256s != 1 {
			t.Fatalf("file-only inventory = %#v; want one hashed installed file without recognized-package coverage", document)
		}
		platform := canonicalPlatform(runtime.GOOS + "-" + runtime.GOARCH)
		assets, reports, err := checkSBOMCoverage(
			ToolchainsManifest{Assets: []ToolchainAsset{{
				ID: "example-tool", Version: "1.0.0", Platform: platform, SHA256: syntheticDigestB,
			}}},
			[]sbomDocument{document},
		)
		if err != nil {
			t.Fatalf("cover locked asset with hashed file-only inventory: %v", err)
		}
		if len(assets) != 1 ||
			assets[0].InventoryScope != inventoryScopeInstalledFiles ||
			assets[0].RecognizedPackages != 0 ||
			assets[0].InventoriedFiles != 1 ||
			assets[0].InventoriedFileSHA256s != 1 ||
			len(reports) != 1 ||
			reports[0].InventoryScope != inventoryScopeInstalledFiles {
			t.Fatalf("file-only coverage = assets %#v, reports %#v; want explicit installed-files scope", assets, reports)
		}
		locks := ToolchainsManifest{
			SchemaVersion: ToolchainsSchema,
			GoVersion:     "1.27.1",
			Assets: []ToolchainAsset{{
				ID: "example-tool", Version: "1.0.0", Platform: "all",
				URL: "https://example.invalid/tool", SHA256: "short", Kind: "binary", Executable: "example-tool",
			}},
			Modules: []ToolchainModule{{
				Name: "example-module", Version: "1.0.0", Integrity: "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", URL: "https://example.invalid/module",
			}},
			Images: []ToolchainImage{{
				Name: "example/image", Version: "1.0.0", IndexDigest: "sha256:" + syntheticDigestA, SourceURL: "https://example.invalid/image",
			}},
		}
		if err := validateToolchains(locks); err == nil || !strings.Contains(err.Error(), "SHA-256") {
			t.Fatalf("bad locked source digest error = %v; want SHA-256 refusal", err)
		}
	})

	t.Run("wrong scanned-tool subject is refused", func(t *testing.T) {
		platform := canonicalPlatform(runtime.GOOS + "-" + runtime.GOARCH)
		path := writeTestFile(t, "wrong-subject.cdx.json", fmt.Sprintf(`{
  "bomFormat": "CycloneDX",
  "specVersion": "1.6",
  "metadata": {
    "component": {
      "type": "application",
      "name": "different-tool",
      "version": "1.0.0"
    }
  },
  "components": [{
    "type": "library",
    "name": "example-dependency",
    "version": "2.0.0",
    "hashes": [{"alg": "SHA-256", "content": %q}]
  }]
}`, syntheticDigestB))
		document, err := loadSBOM(path)
		if err != nil {
			t.Fatalf("load wrong-subject control: %v", err)
		}
		locks := ToolchainsManifest{Assets: []ToolchainAsset{{
			ID: "example-tool", Version: "1.0.0", Platform: platform, SHA256: syntheticDigestA,
		}}}
		if _, _, err := checkSBOMCoverage(locks, []sbomDocument{document}); err == nil || !strings.Contains(err.Error(), "missing scanned-tool subject") {
			t.Fatalf("wrong selected asset error = %v; want scanned-tool subject refusal", err)
		}
	})

	t.Run("standard library PURL versions preserve real mismatch refusals", func(t *testing.T) {
		for _, tc := range []struct {
			name, version, purl string
			reject              bool
		}{
			{"standard library representation", "go1.27.1", "pkg:golang/stdlib@1.27.1", false},
			{"different standard library release", "go1.27.2", "pkg:golang/stdlib@1.27.1", true},
			{"ordinary module is not a Go release", "go1.27.1", "pkg:golang/example.test/library@1.27.1", true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				path := writeTestFile(t, "versions.cdx.json", fmt.Sprintf(`{
  "bomFormat": "CycloneDX", "specVersion": "1.6",
  "metadata": {"component": {"type": "application", "name": "example-tool", "version": "1.0.0"}},
  "components": [{"type": "library", "name": "observed-package", "version": %q, "purl": %q}]
}`, tc.version, tc.purl))
				_, err := loadSBOM(path)
				if (err != nil) != tc.reject {
					t.Fatalf("version identity decision for %s: %v", tc.name, err)
				}
			})
		}
	})

	t.Run("unversioned packages remain visible without bypassing denials", func(t *testing.T) {
		path := writeTestFile(t, "unversioned.cdx.json", `{
  "bomFormat": "CycloneDX", "specVersion": "1.6",
  "metadata": {"component": {"type": "application", "name": "example-tool", "version": "1.0.0"}},
  "components": [{"type": "library", "name": "source-root", "version": "UNKNOWN"}]
}`)
		document, err := loadSBOM(path)
		if err != nil {
			t.Fatalf("retain source-root inventory without inventing a version: %v", err)
		}
		rule := ForbiddenModule{Path: "github.com/goreleaser/goreleaser/v2", Version: "v2.18.1", Reason: "V56 rejected release"}
		if err := checkSBOMForbidden(document, []ForbiddenModule{rule}); err != nil {
			t.Fatalf("unrelated source-root inventory: %v", err)
		}
		_, reports, err := checkSBOMCoverage(ToolchainsManifest{Assets: []ToolchainAsset{{
			ID: "example-tool", Version: "1.0.0", Platform: canonicalPlatform(runtime.GOOS + "-" + runtime.GOARCH), SHA256: syntheticDigestA,
		}}}, []sbomDocument{document})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(reports)
		if err != nil {
			t.Fatal(err)
		}
		var observed []struct {
			UnversionedPackages []string `json:"unversioned_packages"`
		}
		if err := json.Unmarshal(encoded, &observed); err != nil {
			t.Fatal(err)
		}
		if len(observed) != 1 || !reflect.DeepEqual(observed[0].UnversionedPackages, []string{"source-root"}) {
			t.Fatalf("unversioned inventory is not visible to the report consumer: %s", encoded)
		}
		unknownForbidden := writeTestFile(t, "unknown-forbidden.cdx.json", `{
  "bomFormat": "CycloneDX", "specVersion": "1.6",
  "metadata": {"component": {"type": "application", "name": "example-tool", "version": "1.0.0"}},
  "components": [{"type": "library", "name": "goreleaser", "version": "UNKNOWN",
    "purl": "pkg:golang/github.com/goreleaser/goreleaser/v2"}]
}`)
		document, err = loadSBOM(unknownForbidden)
		if err != nil {
			t.Fatal(err)
		}
		var violation *PolicyViolationError
		if err := checkSBOMForbidden(document, []ForbiddenModule{rule}); !errors.As(err, &violation) {
			t.Fatalf("unversioned forbidden identity bypassed policy: %v", err)
		}
	})

	t.Run("GoReleaser exact identity is rejected in external SBOM", func(t *testing.T) {
		path := writeTestFile(t, "goreleaser.cdx.json", fmt.Sprintf(`{
  "bomFormat": "CycloneDX",
  "specVersion": "1.6",
  "metadata": {
    "component": {
      "type": "application",
      "name": "example-tool",
      "version": "1.0.0"
    }
  },
  "components": [{
    "type": "application",
    "name": "goreleaser",
    "version": "v2.18.1",
    "purl": "pkg:golang/github.com/goreleaser/goreleaser/v2@v2.18.1",
    "hashes": [{"alg": "SHA-256", "content": %q}]
  }]
}`, syntheticDigestA))
		document, err := loadSBOM(path)
		if err != nil {
			t.Fatalf("load GoReleaser control: %v", err)
		}
		rule := ForbiddenModule{
			Path:    "github.com/goreleaser/goreleaser/v2",
			Version: "v2.18.1",
			Reason:  "V56 exact rejected tool identity",
		}
		err = checkSBOMForbidden(document, []ForbiddenModule{rule})
		var violation *PolicyViolationError
		if !errors.As(err, &violation) || violation.Identity != "github.com/goreleaser/goreleaser/v2@v2.18.1" {
			t.Fatalf("GoReleaser SBOM error = %v; want exact PolicyViolationError", err)
		}
	})

	if supplied := os.Getenv("PM_SBOMS"); supplied != "" {
		t.Run("supplied external evidence covers the selected toolchain", func(t *testing.T) {
			result, err := CheckDependencies(ctx, CheckOptions{
				Root:           repoRoot,
				VersionsPath:   "tools/versions.json",
				ToolchainsPath: "build/toolchains.lock.json",
				SBOMPaths:      filepath.SplitList(supplied),
				GoCommand:      goCommand,
			})
			if err != nil {
				t.Fatalf("check selected graphs and supplied external evidence: %v", err)
			}
			if result.Status != "pass" || result.EvidenceScope != externalEvidenceScope {
				t.Fatalf("result scope = %#v; want explicit passing full-input inventory scope", result)
			}
		})
	} else {
		t.Log("PM_SBOMS was not supplied: selected graphs and fail-closed SBOM controls ran, but this test result is not external-tool SBOM qualification")
	}
}

func dependencyPolicyRepoRoot(t *testing.T) string {
	t.Helper()
	if supplied := os.Getenv("PM_REPO_ROOT"); supplied != "" {
		root, err := filepath.Abs(supplied)
		if err != nil {
			t.Fatalf("resolve PM_REPO_ROOT: %v", err)
		}
		return root
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("locate repository go.mod from %s: %v; set PM_REPO_ROOT for Bazel runfiles", root, err)
	}
	return root
}

func writeSyntheticModuleGraph(t *testing.T, goReleaserVersion string) string {
	t.Helper()
	fixture := t.TempDir()
	root := filepath.Join(fixture, "root")
	parent := filepath.Join(fixture, "parent")
	goReleaser := filepath.Join(fixture, "goreleaser")
	for _, directory := range []string{root, parent, goReleaser} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatalf("create synthetic module directory: %v", err)
		}
	}
	writeFile(t, filepath.Join(root, "go.mod"), fmt.Sprintf(`module example.test/root

go 1.27.1

require example.test/parent v1.0.0

replace example.test/parent v1.0.0 => ../parent
replace github.com/goreleaser/goreleaser/v2 %s => ../goreleaser
`, goReleaserVersion))
	writeFile(t, filepath.Join(root, "root.go"), "package root\n\nconst Name = \"synthetic-root\"\n")
	writeFile(t, filepath.Join(parent, "go.mod"), fmt.Sprintf(`module example.test/parent

go 1.27.1

require github.com/goreleaser/goreleaser/v2 %s
`, goReleaserVersion))
	writeFile(t, filepath.Join(goReleaser, "go.mod"), "module github.com/goreleaser/goreleaser/v2\n\ngo 1.27.1\n")
	return root
}

func writeTestFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	writeFile(t, path, content)
	return path
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
