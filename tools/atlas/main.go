package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	atlasSchemaVersion        = "pm.foundation-atlas/v1"
	requirementsSchemaVersion = "pm.foundation-requirements/v1"
	maxTraceabilityInputBytes = 8 << 20
)

var (
	foundationIDPattern = regexp.MustCompile(`^FND-[0-9]{2}$`)
	versionPattern      = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	registrationPattern = regexp.MustCompile(`((?:[A-Za-z0-9_.-]+/)*[A-Za-z0-9_.-]+\.go):([A-Za-z_][A-Za-z0-9_]*)`)
)

var sourcePaths = []string{
	"docs/architecture/pm-server/architecture/FOUNDATIONS.json",
	"docs/architecture/pm-server/architecture/FOUNDATION-MAPPINGS.json",
	"docs/architecture/pm-server/implementation/FILE-OWNERSHIP.json",
	"docs/architecture/pm-server/implementation/VALIDATION-MATRIX.json",
	"docs/architecture/pm-server/implementation/PACKET-CONTRACTS.json",
}

var generatedPaths = []string{
	"docs/foundations/atlas.json",
	"docs/foundations/requirements.json",
}

// Options selects one complete authoring closure. Count overrides exist only so
// V80 can exercise isolated destructive fixtures; the normal project command
// intentionally uses the complete 20/43/44 defaults.
type Options struct {
	Root                 string
	ExpectedFoundations  int
	ExpectedRequirements int
	ExpectedPackets      int
}

type sourceIdentity struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type foundationReference struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type foundationsDocument struct {
	SchemaVersion string       `json:"schema_version"`
	Foundations   []foundation `json:"foundations"`
}

type foundation struct {
	ID                   string   `json:"id"`
	Version              string   `json:"version"`
	Name                 string   `json:"name"`
	Owner                string   `json:"owner"`
	Files                []string `json:"files"`
	Interface            string   `json:"interface"`
	Dependencies         []string `json:"dependencies"`
	RegistrationPoint    string   `json:"registration_point"`
	Consumers            []string `json:"consumers"`
	TestIDs              []string `json:"test_ids"`
	ImplementationStatus string   `json:"implementation_status"`
	EvidenceStatus       string   `json:"evidence_status"`
}

type mappingsDocument struct {
	SchemaVersion string        `json:"schema_version"`
	Requirements  []requirement `json:"requirements"`
}

type requirement struct {
	ID               string                `json:"id"`
	Connector        string                `json:"connector"`
	Operation        string                `json:"operation"`
	ScopeKind        string                `json:"scope_kind"`
	Status           string                `json:"status"`
	FoundationRefs   []foundationReference `json:"foundation_refs"`
	Lanes            []string              `json:"lanes"`
	Registration     string                `json:"registration"`
	Files            []string              `json:"files"`
	TestIDs          []string              `json:"test_ids"`
	RequiredBehavior []string              `json:"required_behavior"`
	Admission        string                `json:"admission"`
	ExistingEvidence string                `json:"existing_evidence"`
	MissingProof     string                `json:"missing_proof"`
	EvidenceScope    string                `json:"evidence_scope"`
}

type ownershipDocument struct {
	SchemaVersion string           `json:"schema_version"`
	Entries       []ownershipEntry `json:"entries"`
}

type ownershipEntry struct {
	ID           string   `json:"id"`
	PriorGroupID string   `json:"prior_group_id"`
	Paths        []string `json:"paths"`
	Package      string   `json:"package"`
	ProcessOwner string   `json:"process_owner"`
	ReviewState  string   `json:"review_state"`
}

type validationDocument struct {
	SchemaVersion string            `json:"schema_version"`
	Entries       []validationEntry `json:"entries"`
}

type validationEntry struct {
	ID             string   `json:"id"`
	OwnerFile      string   `json:"owner_file"`
	Symbol         string   `json:"symbol"`
	Contexts       []string `json:"contexts"`
	PlannedCommand string   `json:"planned_command"`
	CommandStatus  string   `json:"command_status"`
}

type packetDocument struct {
	SchemaVersion string   `json:"schema_version"`
	Issues        []packet `json:"issues"`
}

type packet struct {
	ID    string       `json:"id"`
	Files []packetFile `json:"files"`
}

type packetFile struct {
	Path            string   `json:"path"`
	GroupID         string   `json:"group_id"`
	PriorGroupID    string   `json:"prior_group_id"`
	Owner           string   `json:"owner"`
	AssignmentOwner string   `json:"assignment_owner"`
	GoPackage       string   `json:"go_package"`
	Process         string   `json:"process"`
	Interfaces      []string `json:"interfaces"`
}

type ownershipReference struct {
	Path             string `json:"path"`
	GroupID          string `json:"group_id"`
	PacketID         string `json:"packet_id"`
	OwnershipPackage string `json:"ownership_package,omitempty"`
	AssignmentOwner  string `json:"assignment_owner,omitempty"`
	GoPackage        string `json:"go_package,omitempty"`
	OwnershipProcess string `json:"ownership_process,omitempty"`
	PacketProcess    string `json:"packet_process,omitempty"`
}

type handlerBinding struct {
	Path             string `json:"path"`
	Symbol           string `json:"symbol"`
	DeclarationState string `json:"declaration_state"`
}

type atlasGap struct {
	Kind    string `json:"kind"`
	Pointer string `json:"pointer"`
	Detail  string `json:"detail"`
}

type atlasFoundation struct {
	ID                   string                `json:"id"`
	Version              string                `json:"version"`
	Name                 string                `json:"name"`
	Owner                string                `json:"owner"`
	Interface            string                `json:"interface"`
	ImplementationState  string                `json:"implementation_state"`
	EvidenceState        string                `json:"evidence_state"`
	CapabilityState      string                `json:"capability_state"`
	Dependencies         []foundationReference `json:"dependencies"`
	Consumers            []string              `json:"consumers"`
	Files                []ownershipReference  `json:"files"`
	RegistrationBindings []handlerBinding      `json:"registration_bindings"`
	RequirementIDs       []string              `json:"requirement_ids"`
	TestIDs              []string              `json:"test_ids"`
	Gaps                 []atlasGap            `json:"gaps"`
}

type handlerDeclaration struct {
	PacketID         string `json:"packet_id"`
	Path             string `json:"path"`
	Symbol           string `json:"symbol"`
	DeclarationState string `json:"declaration_state"`
}

type atlasCounts struct {
	Foundations   int `json:"foundations"`
	Requirements  int `json:"requirements"`
	Packets       int `json:"packets"`
	DeclaredPaths int `json:"declared_paths"`
	ActualGoFiles int `json:"actual_go_files"`
}

type atlasValidation struct {
	ID             string   `json:"id"`
	OwnerFile      string   `json:"owner_file"`
	Symbol         string   `json:"symbol"`
	Contexts       []string `json:"contexts"`
	PlannedCommand string   `json:"planned_command"`
	EvidenceState  string   `json:"evidence_state"`
}

type atlasDocument struct {
	SchemaVersion       string               `json:"schema_version"`
	Authority           string               `json:"authority"`
	Sources             []sourceIdentity     `json:"sources"`
	Counts              atlasCounts          `json:"counts"`
	Validation          atlasValidation      `json:"validation"`
	Foundations         []atlasFoundation    `json:"foundations"`
	HandlerDeclarations []handlerDeclaration `json:"handler_declarations"`
	Gaps                []atlasGap           `json:"gaps"`
}

type requirementProjection struct {
	ID               string                `json:"id"`
	Connector        string                `json:"connector"`
	Operation        string                `json:"operation"`
	ScopeKind        string                `json:"scope_kind"`
	DeclaredStatus   string                `json:"declared_status"`
	FoundationRefs   []foundationReference `json:"foundation_refs"`
	Lanes            []string              `json:"lanes"`
	Registration     string                `json:"registration"`
	Files            []ownershipReference  `json:"files"`
	TestIDs          []string              `json:"test_ids"`
	RequiredBehavior []string              `json:"required_behavior"`
	Admission        string                `json:"admission"`
	ExistingEvidence string                `json:"existing_evidence"`
	MissingProof     string                `json:"missing_proof"`
	EvidenceScope    string                `json:"evidence_scope"`
}

type requirementsDocument struct {
	SchemaVersion string                  `json:"schema_version"`
	Authority     string                  `json:"authority"`
	Sources       []sourceIdentity        `json:"sources"`
	Requirements  []requirementProjection `json:"requirements"`
}

type traceability struct {
	Atlas        atlasDocument
	Requirements requirementsDocument
}

type ownerRecord struct {
	GroupID          string
	PriorGroupID     string
	PacketID         string
	OwnershipPackage string
	AssignmentOwner  string
	GoPackage        string
	OwnershipProcess string
	PacketProcess    string
}

type conformanceError struct {
	Kind    string
	Pointer string
	Detail  string
}

func (err *conformanceError) Error() string {
	if err.Pointer == "" {
		return fmt.Sprintf("%s: %s", err.Kind, err.Detail)
	}
	return fmt.Sprintf("%s at %s: %s", err.Kind, err.Pointer, err.Detail)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(arguments []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("pm-foundation-atlas", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: pm-foundation-atlas --root ROOT [--output-root ROOT] [--check]")
		fmt.Fprintln(stderr, "Validate the complete authoring closure and deterministically write or compare the Foundation Atlas projections.")
		flags.PrintDefaults()
	}
	root := flags.String("root", "", "repository root containing the accepted planning and source declarations")
	outputRoot := flags.String("output-root", "", "output tree; defaults to --root")
	check := flags.Bool("check", false, "compare exact generated bytes without writing")
	expectedFoundations := flags.Int("expected-foundations", 20, "required foundation count; override only for isolated V80 fixtures")
	expectedRequirements := flags.Int("expected-requirements", 43, "required requirement count; override only for isolated V80 fixtures")
	expectedPackets := flags.Int("expected-packets", 44, "required canonical packet count; override only for isolated V80 fixtures")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "pm-foundation-atlas: unexpected positional arguments: %s\n", strings.Join(flags.Args(), " "))
		return 2
	}
	if strings.TrimSpace(*root) == "" {
		fmt.Fprintln(stderr, "pm-foundation-atlas: --root is required")
		flags.Usage()
		return 2
	}
	if *outputRoot == "" {
		*outputRoot = *root
	}

	checked, err := CheckTraceability(Options{
		Root:                 *root,
		ExpectedFoundations:  *expectedFoundations,
		ExpectedRequirements: *expectedRequirements,
		ExpectedPackets:      *expectedPackets,
	})
	if err != nil {
		fmt.Fprintf(stderr, "pm-foundation-atlas: %v\n", err)
		return 1
	}
	outputs, err := RenderAtlas(checked)
	if err != nil {
		fmt.Fprintf(stderr, "pm-foundation-atlas: %v\n", err)
		return 1
	}
	if *check {
		err = compareGeneratedOutputs(*outputRoot, outputs)
	} else {
		err = writeGeneratedOutputs(*outputRoot, outputs)
	}
	if err != nil {
		fmt.Fprintf(stderr, "pm-foundation-atlas: %v\n", err)
		return 1
	}
	return 0
}

// CheckTraceability consumes planning declarations and source presence only. It
// never loads a runtime registry or promotes a declaration into capability or
// proof authority.
func CheckTraceability(options Options) (traceability, error) {
	root, err := resolveRoot(options.Root)
	if err != nil {
		return traceability{}, err
	}
	if options.ExpectedFoundations <= 0 || options.ExpectedRequirements <= 0 || options.ExpectedPackets <= 0 {
		return traceability{}, errors.New("expected closure counts must be positive")
	}

	var foundations foundationsDocument
	var mappings mappingsDocument
	var ownership ownershipDocument
	var validations validationDocument
	var packets packetDocument
	destinations := []any{&foundations, &mappings, &ownership, &validations, &packets}
	sources := make([]sourceIdentity, 0, len(sourcePaths))
	for index, relative := range sourcePaths {
		contents, err := loadStrictJSON(filepath.Join(root, filepath.FromSlash(relative)), destinations[index])
		if err != nil {
			return traceability{}, fmt.Errorf("load %s: %w", relative, err)
		}
		digest := sha256.Sum256(contents)
		sources = append(sources, sourceIdentity{Path: relative, SHA256: hex.EncodeToString(digest[:])})
	}
	if foundations.SchemaVersion != "pm.plan.foundations/v2" {
		return traceability{}, conformance("unsupported source version", "/foundations/schema_version", foundations.SchemaVersion)
	}
	if mappings.SchemaVersion != "pm.plan.mappings/v2" {
		return traceability{}, conformance("unsupported source version", "/requirements/schema_version", mappings.SchemaVersion)
	}
	if ownership.SchemaVersion != "pm.plan.files/v2" {
		return traceability{}, conformance("unsupported source version", "/file_ownership/schema_version", ownership.SchemaVersion)
	}
	if validations.SchemaVersion != "pm.plan.validation/v2" {
		return traceability{}, conformance("unsupported source version", "/validations/schema_version", validations.SchemaVersion)
	}
	if packets.SchemaVersion != "pm.context.packet-contracts/v2" {
		return traceability{}, conformance("unsupported source version", "/packets/schema_version", packets.SchemaVersion)
	}
	if len(foundations.Foundations) != options.ExpectedFoundations {
		return traceability{}, conformance("incomplete foundation map", "/foundations", fmt.Sprintf("found %d; require %d", len(foundations.Foundations), options.ExpectedFoundations))
	}
	if len(mappings.Requirements) != options.ExpectedRequirements {
		return traceability{}, conformance("incomplete requirement map", "/requirements", fmt.Sprintf("found %d; require %d", len(mappings.Requirements), options.ExpectedRequirements))
	}
	if len(packets.Issues) != options.ExpectedPackets {
		return traceability{}, conformance("incomplete packet map", "/packets", fmt.Sprintf("found %d; require %d", len(packets.Issues), options.ExpectedPackets))
	}

	owners, declarations, err := buildOwnership(root, ownership, packets)
	if err != nil {
		return traceability{}, err
	}
	actualGoFiles, err := validateGoInventory(root, owners)
	if err != nil {
		return traceability{}, err
	}
	validation, err := validateV80(validations)
	if err != nil {
		return traceability{}, err
	}
	atlasFoundations, foundationVersions, err := validateFoundations(root, foundations.Foundations, owners)
	if err != nil {
		return traceability{}, err
	}
	for ordinal := 1; ordinal <= options.ExpectedFoundations; ordinal++ {
		id := fmt.Sprintf("FND-%02d", ordinal)
		if _, present := foundationVersions[id]; !present {
			return traceability{}, conformance("incomplete foundation map", "/foundations", "missing "+id)
		}
	}
	requirements, reverseRequirements, err := validateRequirements(mappings.Requirements, foundationVersions, owners)
	if err != nil {
		return traceability{}, err
	}
	for index := range atlasFoundations {
		atlasFoundations[index].RequirementIDs = cloneStrings(reverseRequirements[atlasFoundations[index].ID])
		sort.Strings(atlasFoundations[index].RequirementIDs)
	}

	gaps := make([]atlasGap, 0)
	for _, foundation := range atlasFoundations {
		gaps = append(gaps, foundation.Gaps...)
	}
	sort.Slice(gaps, func(left, right int) bool {
		if gaps[left].Pointer != gaps[right].Pointer {
			return gaps[left].Pointer < gaps[right].Pointer
		}
		if gaps[left].Kind != gaps[right].Kind {
			return gaps[left].Kind < gaps[right].Kind
		}
		return gaps[left].Detail < gaps[right].Detail
	})

	result := traceability{
		Atlas: atlasDocument{
			SchemaVersion: atlasSchemaVersion,
			Authority:     "authoring_projection_only; runtime availability and proof remain with their named source owners",
			Sources:       cloneSources(sources),
			Counts: atlasCounts{
				Foundations:   len(atlasFoundations),
				Requirements:  len(requirements),
				Packets:       len(packets.Issues),
				DeclaredPaths: len(owners),
				ActualGoFiles: len(actualGoFiles),
			},
			Validation:          validation,
			Foundations:         atlasFoundations,
			HandlerDeclarations: declarations,
			Gaps:                gaps,
		},
		Requirements: requirementsDocument{
			SchemaVersion: requirementsSchemaVersion,
			Authority:     "design_impact_projection_only; mappings are unions and do not establish runtime readiness",
			Sources:       cloneSources(sources),
			Requirements:  requirements,
		},
	}
	return result, nil
}

// RenderAtlas renders canonical JSON bytes for the two disposable projections.
func RenderAtlas(checked traceability) (map[string][]byte, error) {
	atlas, err := marshalDocument(checked.Atlas)
	if err != nil {
		return nil, fmt.Errorf("render atlas: %w", err)
	}
	requirements, err := marshalDocument(checked.Requirements)
	if err != nil {
		return nil, fmt.Errorf("render requirements: %w", err)
	}
	return map[string][]byte{
		generatedPaths[0]: atlas,
		generatedPaths[1]: requirements,
	}, nil
}

func resolveRoot(root string) (string, error) {
	resolvedRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve repository root: %w", err)
	}
	info, err := os.Stat(resolvedRoot)
	if err != nil {
		return "", fmt.Errorf("stat repository root: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("repository root is not a directory")
	}
	return resolvedRoot, nil
}

func buildOwnership(root string, ownership ownershipDocument, packets packetDocument) (map[string]ownerRecord, []handlerDeclaration, error) {
	owners := make(map[string]ownerRecord)
	ownershipIDs := make(map[string]struct{}, len(ownership.Entries))
	for index, entry := range ownership.Entries {
		pointer := "/file_ownership/entries/" + strconv.Itoa(index)
		if entry.ID == "" || entry.Package == "" || entry.ProcessOwner == "" {
			return nil, nil, conformance("invalid file ownership", pointer, "group identity, ownership package and process owner are required")
		}
		if _, duplicate := ownershipIDs[entry.ID]; duplicate {
			return nil, nil, conformance("duplicate authority", pointer+"/id", entry.ID)
		}
		ownershipIDs[entry.ID] = struct{}{}
		for pathIndex, path := range entry.Paths {
			pathPointer := pointer + "/paths/" + strconv.Itoa(pathIndex)
			if err := validateRelativePath(path); err != nil {
				return nil, nil, conformance("invalid file ownership", pathPointer, err.Error())
			}
			if _, duplicate := owners[path]; duplicate {
				return nil, nil, conformance("duplicate authority", pathPointer, path)
			}
			owners[path] = ownerRecord{
				GroupID:          entry.ID,
				PriorGroupID:     entry.PriorGroupID,
				OwnershipPackage: entry.Package,
				OwnershipProcess: entry.ProcessOwner,
			}
		}
	}

	packetIDs := make(map[string]struct{}, len(packets.Issues))
	packetPaths := make(map[string]string)
	declarations := make([]handlerDeclaration, 0)
	for issueIndex, issue := range packets.Issues {
		issuePointer := "/packets/issues/" + strconv.Itoa(issueIndex)
		if issue.ID == "" {
			return nil, nil, conformance("invalid packet declaration", issuePointer+"/id", "packet identity is required")
		}
		if _, duplicate := packetIDs[issue.ID]; duplicate {
			return nil, nil, conformance("duplicate authority", issuePointer+"/id", issue.ID)
		}
		packetIDs[issue.ID] = struct{}{}
		for fileIndex, file := range issue.Files {
			filePointer := issuePointer + "/files/" + strconv.Itoa(fileIndex)
			if err := validateRelativePath(file.Path); err != nil {
				return nil, nil, conformance("invalid packet declaration", filePointer+"/path", err.Error())
			}
			if file.GroupID == "" {
				return nil, nil, conformance("invalid packet declaration", filePointer+"/group_id", "group identity is required")
			}
			if prior, duplicate := packetPaths[file.Path]; duplicate {
				return nil, nil, conformance("duplicate authority", filePointer+"/path", fmt.Sprintf("%s also declared by %s", file.Path, prior))
			}
			packetPaths[file.Path] = issue.ID
			assignmentOwner := file.AssignmentOwner
			if assignmentOwner == "" {
				assignmentOwner = file.Owner
			}
			if file.GoPackage != "" && !token.IsIdentifier(file.GoPackage) {
				return nil, nil, conformance("invalid packet declaration", filePointer+"/go_package", file.GoPackage)
			}
			if existing, present := owners[file.Path]; present {
				if existing.GroupID != file.GroupID || (existing.PriorGroupID != "" && file.PriorGroupID != "" && existing.PriorGroupID != file.PriorGroupID) {
					return nil, nil, conformance("duplicate authority", filePointer+"/group_id", fmt.Sprintf("%s disagrees with file-ownership group %s", file.GroupID, existing.GroupID))
				}
				existing.PacketID = issue.ID
				if existing.PriorGroupID == "" {
					existing.PriorGroupID = file.PriorGroupID
				}
				existing.AssignmentOwner = assignmentOwner
				existing.GoPackage = file.GoPackage
				if file.Process != "" {
					existing.PacketProcess = file.Process
				}
				owners[file.Path] = existing
			} else {
				owners[file.Path] = ownerRecord{
					GroupID:         file.GroupID,
					PriorGroupID:    file.PriorGroupID,
					PacketID:        issue.ID,
					AssignmentOwner: assignmentOwner,
					GoPackage:       file.GoPackage,
					PacketProcess:   file.Process,
				}
			}
			for interfaceIndex, symbol := range file.Interfaces {
				if !token.IsIdentifier(symbol) {
					return nil, nil, conformance("invalid handler declaration", filePointer+"/interfaces/"+strconv.Itoa(interfaceIndex), symbol)
				}
				state, err := declarationState(root, file.Path, symbol)
				if err != nil {
					return nil, nil, err
				}
				declaration := handlerDeclaration{
					PacketID:         issue.ID,
					Path:             file.Path,
					Symbol:           symbol,
					DeclarationState: state,
				}
				declarations = append(declarations, declaration)
			}
		}
	}
	sort.Slice(declarations, func(left, right int) bool {
		if declarations[left].Path != declarations[right].Path {
			return declarations[left].Path < declarations[right].Path
		}
		if declarations[left].Symbol != declarations[right].Symbol {
			return declarations[left].Symbol < declarations[right].Symbol
		}
		return declarations[left].PacketID < declarations[right].PacketID
	})
	return owners, declarations, nil
}

func validateGoInventory(root string, owners map[string]ownerRecord) ([]string, error) {
	files := make([]string, 0)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if relative == "." {
				return nil
			}
			base := entry.Name()
			if base == ".git" || base == ".codegraph" || base == ".cache" || base == "node_modules" || strings.HasPrefix(base, "bazel-") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(entry.Name()) != ".go" {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return conformance("ownerless file", "/source_inventory/"+escapePointer(relative), "Go source is not a regular file")
		}
		owner, present := owners[relative]
		if !present {
			return conformance("ownerless file", "/source_inventory/"+escapePointer(relative), relative)
		}
		if err := validateGoBoundary(path, relative, owner); err != nil {
			return err
		}
		files = append(files, relative)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("inspect Go source ownership: %w", err)
	}
	sort.Strings(files)
	return files, nil
}
func validateGoBoundary(path, relative string, owner ownerRecord) error {
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		return conformance("invalid package boundary", "/source_inventory/"+escapePointer(relative), err.Error())
	}
	if owner.GoPackage != "" {
		actual := parsed.Name.Name
		externalTestPackage := strings.HasSuffix(relative, "_test.go") && actual == owner.GoPackage+"_test"
		if actual != owner.GoPackage && !externalTestPackage {
			return conformance("invalid package boundary", "/source_inventory/"+escapePointer(relative), fmt.Sprintf("declared Go package %s; source package %s", owner.GoPackage, actual))
		}
	}
	if !strings.HasPrefix(relative, "internal/store/") || strings.HasSuffix(relative, "_test.go") {
		return nil
	}
	if owner.GoPackage != "store" {
		return conformance("invalid package boundary", "/source_inventory/"+escapePointer(relative), "production store source must declare go_package store")
	}
	for _, imported := range parsed.Imports {
		importPath, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			return conformance("invalid package boundary", "/source_inventory/"+escapePointer(relative), "invalid import path")
		}
		for _, forbidden := range []string{"app", "auth"} {
			marker := "/internal/" + forbidden
			if strings.HasSuffix(importPath, marker) || strings.Contains(importPath, marker+"/") {
				return conformance("invalid package boundary", "/source_inventory/"+escapePointer(relative), "store source imports forbidden consumer package "+forbidden)
			}
		}
	}
	return nil
}

func validateV80(document validationDocument) (atlasValidation, error) {
	var selected *validationEntry
	for index := range document.Entries {
		if document.Entries[index].ID != "V80" {
			continue
		}
		if selected != nil {
			return atlasValidation{}, conformance("duplicate authority", "/validations/entries/"+strconv.Itoa(index)+"/id", "V80")
		}
		selected = &document.Entries[index]
	}
	if selected == nil {
		return atlasValidation{}, conformance("missing validation", "/validations", "V80")
	}
	if selected.OwnerFile != "tools/traceability_test.go" || selected.Symbol != "TestTraceabilityClosure" {
		return atlasValidation{}, conformance("invalid validation binding", "/validations/V80", "V80 must remain bound to tools/traceability_test.go:TestTraceabilityClosure")
	}
	if !contains(selected.Contexts, "foundation_fixture") || !contains(selected.Contexts, "same_stack_integration") {
		return atlasValidation{}, conformance("invalid validation binding", "/validations/V80/contexts", "both required environments must be preserved")
	}
	if strings.TrimSpace(selected.PlannedCommand) == "" {
		return atlasValidation{}, conformance("invalid validation binding", "/validations/V80/planned_command", "planned command is required")
	}
	switch selected.CommandStatus {
	case "UNEXECUTED":
		// The Atlas deliberately has no proof-receipt authority. A future
		// source-owned evidence contract must be added before any pass label
		// can be projected.
	case "PASS", "PASSED":
		return atlasValidation{}, conformance("unsupported evidence claim", "/validations/V80/receipt", "pass has no exact matching receipt from the proof authority")
	default:
		return atlasValidation{}, conformance("unsupported evidence claim", "/validations/V80/command_status", selected.CommandStatus)
	}
	return atlasValidation{
		ID:             selected.ID,
		OwnerFile:      selected.OwnerFile,
		Symbol:         selected.Symbol,
		Contexts:       cloneStrings(selected.Contexts),
		PlannedCommand: selected.PlannedCommand,
		EvidenceState:  "unexecuted",
	}, nil
}

func validateFoundations(root string, foundations []foundation, owners map[string]ownerRecord) ([]atlasFoundation, map[string]string, error) {
	versions := make(map[string]string, len(foundations))
	interfaces := make(map[string]string, len(foundations))
	for index, foundation := range foundations {
		pointer := "/foundations/" + strconv.Itoa(index)
		if !foundationIDPattern.MatchString(foundation.ID) || !versionPattern.MatchString(foundation.Version) {
			return nil, nil, conformance("invalid foundation identity", pointer, foundation.ID+"@"+foundation.Version)
		}
		if _, duplicate := versions[foundation.ID]; duplicate {
			return nil, nil, conformance("duplicate authority", pointer+"/id", foundation.ID)
		}
		versions[foundation.ID] = foundation.Version
		if strings.TrimSpace(foundation.Interface) == "" {
			return nil, nil, conformance("missing semantic authority", pointer+"/interface", foundation.ID)
		}
		if prior, duplicate := interfaces[foundation.Interface]; duplicate {
			return nil, nil, conformance("duplicate authority", pointer+"/interface", fmt.Sprintf("semantic boundary also owned by %s", prior))
		}
		interfaces[foundation.Interface] = foundation.ID
	}

	projected := make([]atlasFoundation, 0, len(foundations))
	for index, foundation := range foundations {
		pointer := "/foundations/" + strconv.Itoa(index)
		if foundation.Name == "" || foundation.Owner == "" {
			return nil, nil, conformance("invalid foundation declaration", pointer, "name and owner are required")
		}
		if len(foundation.Consumers) == 0 {
			return nil, nil, conformance("missing-consumer closure", pointer+"/consumers", foundation.ID+" has no declared consumer")
		}
		for consumerIndex, consumer := range foundation.Consumers {
			if strings.TrimSpace(consumer) == "" {
				return nil, nil, conformance("missing-consumer closure", pointer+"/consumers/"+strconv.Itoa(consumerIndex), foundation.ID+" has an empty consumer")
			}
		}
		if foundation.ImplementationStatus != "planned" && foundation.ImplementationStatus != "unavailable" && foundation.ImplementationStatus != "implemented" {
			return nil, nil, conformance("invalid implementation state", pointer+"/implementation_status", foundation.ImplementationStatus)
		}
		if foundation.EvidenceStatus != "unexecuted" {
			return nil, nil, conformance("unsupported evidence claim", pointer+"/evidence_status", foundation.ID+" has no source-owned exact proof receipt in the Atlas input closure")
		}

		dependencies := make([]foundationReference, 0, len(foundation.Dependencies))
		seenDependencies := make(map[string]struct{}, len(foundation.Dependencies))
		for dependencyIndex, dependency := range foundation.Dependencies {
			version, present := versions[dependency]
			if !present {
				return nil, nil, conformance("unresolved contract version", pointer+"/dependencies/"+strconv.Itoa(dependencyIndex), dependency)
			}
			if _, duplicate := seenDependencies[dependency]; duplicate {
				return nil, nil, conformance("duplicate foundation dependency", pointer+"/dependencies/"+strconv.Itoa(dependencyIndex), dependency)
			}
			seenDependencies[dependency] = struct{}{}
			dependencies = append(dependencies, foundationReference{ID: dependency, Version: version})
		}

		files, err := ownershipReferences(foundation.Files, owners, pointer+"/files")
		if err != nil {
			return nil, nil, err
		}
		bindings, err := parseRegistrationBindings(root, foundation.RegistrationPoint)
		if err != nil {
			return nil, nil, conformance("invalid handler declaration", pointer+"/registration_point", err.Error())
		}
		if len(bindings) == 0 {
			return nil, nil, conformance("missing-consumer closure", pointer+"/registration_point", foundation.ID+" has no path:symbol binding")
		}
		gaps := make([]atlasGap, 0)
		for bindingIndex, binding := range bindings {
			if _, present := owners[binding.Path]; !present {
				return nil, nil, conformance("ownerless file", pointer+"/registration_bindings/"+strconv.Itoa(bindingIndex)+"/path", binding.Path)
			}
			if binding.DeclarationState != "present" {
				gap := atlasGap{
					Kind:    "handler_" + binding.DeclarationState,
					Pointer: "/foundations/" + escapePointer(foundation.ID) + "/registration_bindings/" + strconv.Itoa(bindingIndex),
					Detail:  binding.Path + ":" + binding.Symbol,
				}
				gaps = append(gaps, gap)
				if foundation.ImplementationStatus == "implemented" {
					return nil, nil, conformance("missing-handler closure", pointer+"/registration_point", foundation.ID+" requires "+binding.Path+":"+binding.Symbol)
				}
			}
		}
		projected = append(projected, atlasFoundation{
			ID:                   foundation.ID,
			Version:              foundation.Version,
			Name:                 foundation.Name,
			Owner:                foundation.Owner,
			Interface:            foundation.Interface,
			ImplementationState:  foundation.ImplementationStatus,
			EvidenceState:        foundation.EvidenceStatus,
			CapabilityState:      "not_established_by_atlas",
			Dependencies:         dependencies,
			Consumers:            cloneStrings(foundation.Consumers),
			Files:                files,
			RegistrationBindings: bindings,
			RequirementIDs:       []string{},
			TestIDs:              cloneStrings(foundation.TestIDs),
			Gaps:                 gaps,
		})
	}
	sort.Slice(projected, func(left, right int) bool { return projected[left].ID < projected[right].ID })
	return projected, versions, nil
}

func validateRequirements(requirements []requirement, versions map[string]string, owners map[string]ownerRecord) ([]requirementProjection, map[string][]string, error) {
	seen := make(map[string]struct{}, len(requirements))
	reverse := make(map[string][]string, len(versions))
	projected := make([]requirementProjection, 0, len(requirements))
	for index, requirement := range requirements {
		pointer := "/requirements/" + strconv.Itoa(index)
		if strings.TrimSpace(requirement.ID) == "" {
			return nil, nil, conformance("invalid requirement", pointer+"/id", "identity is required")
		}
		if _, duplicate := seen[requirement.ID]; duplicate {
			return nil, nil, conformance("duplicate authority", pointer+"/id", requirement.ID)
		}
		seen[requirement.ID] = struct{}{}
		if strings.TrimSpace(requirement.Registration) == "" {
			return nil, nil, conformance("missing-consumer closure", pointer+"/registration", requirement.ID)
		}
		if len(requirement.FoundationRefs) == 0 {
			return nil, nil, conformance("missing foundation mapping", pointer+"/foundation_refs", requirement.ID)
		}
		seenRefs := make(map[string]struct{}, len(requirement.FoundationRefs))
		refs := make([]foundationReference, 0, len(requirement.FoundationRefs))
		for refIndex, ref := range requirement.FoundationRefs {
			version, present := versions[ref.ID]
			if !present || version != ref.Version {
				return nil, nil, conformance("unresolved contract version", pointer+"/foundation_refs/"+strconv.Itoa(refIndex), ref.ID+"@"+ref.Version)
			}
			key := ref.ID + "@" + ref.Version
			if _, duplicate := seenRefs[key]; duplicate {
				return nil, nil, conformance("duplicate foundation mapping", pointer+"/foundation_refs/"+strconv.Itoa(refIndex), key)
			}
			seenRefs[key] = struct{}{}
			refs = append(refs, ref)
			reverse[ref.ID] = append(reverse[ref.ID], requirement.ID)
		}
		files, err := ownershipReferences(requirement.Files, owners, pointer+"/files")
		if err != nil {
			return nil, nil, err
		}
		status := requirement.Status
		if strings.TrimSpace(status) == "" {
			status = "unspecified"
		}
		projected = append(projected, requirementProjection{
			ID:               requirement.ID,
			Connector:        requirement.Connector,
			Operation:        requirement.Operation,
			ScopeKind:        requirement.ScopeKind,
			DeclaredStatus:   status,
			FoundationRefs:   refs,
			Lanes:            cloneStrings(requirement.Lanes),
			Registration:     requirement.Registration,
			Files:            files,
			TestIDs:          cloneStrings(requirement.TestIDs),
			RequiredBehavior: cloneStrings(requirement.RequiredBehavior),
			Admission:        requirement.Admission,
			ExistingEvidence: requirement.ExistingEvidence,
			MissingProof:     requirement.MissingProof,
			EvidenceScope:    requirement.EvidenceScope,
		})
	}
	sort.Slice(projected, func(left, right int) bool { return projected[left].ID < projected[right].ID })
	return projected, reverse, nil
}

func ownershipReferences(paths []string, owners map[string]ownerRecord, pointer string) ([]ownershipReference, error) {
	result := make([]ownershipReference, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for index, path := range paths {
		if err := validateRelativePath(path); err != nil {
			return nil, conformance("invalid mapped file", pointer+"/"+strconv.Itoa(index), err.Error())
		}
		if _, duplicate := seen[path]; duplicate {
			return nil, conformance("duplicate file mapping", pointer+"/"+strconv.Itoa(index), path)
		}
		seen[path] = struct{}{}
		owner, present := owners[path]
		if !present {
			return nil, conformance("ownerless file", pointer+"/"+strconv.Itoa(index), path)
		}
		result = append(result, ownershipReference{
			Path:             path,
			GroupID:          owner.GroupID,
			PacketID:         owner.PacketID,
			OwnershipPackage: owner.OwnershipPackage,
			AssignmentOwner:  owner.AssignmentOwner,
			GoPackage:        owner.GoPackage,
			OwnershipProcess: owner.OwnershipProcess,
			PacketProcess:    owner.PacketProcess,
		})
	}
	return result, nil
}

func parseRegistrationBindings(root, registration string) ([]handlerBinding, error) {
	matches := registrationPattern.FindAllStringSubmatch(registration, -1)
	bindings := make([]handlerBinding, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	previousPath := ""
	for _, match := range matches {
		path := resolveRegistrationPath(match[1], previousPath)
		previousPath = path
		symbol := match[2]
		if err := validateRelativePath(path); err != nil {
			return nil, err
		}
		key := path + ":" + symbol
		if _, duplicate := seen[key]; duplicate {
			return nil, fmt.Errorf("duplicate registration binding %s", key)
		}
		seen[key] = struct{}{}
		state, err := declarationState(root, path, symbol)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, handlerBinding{Path: path, Symbol: symbol, DeclarationState: state})
	}
	return bindings, nil
}

func resolveRegistrationPath(path, previous string) string {
	for _, prefix := range []string{"internal/", "cmd/", "tools/", "scripts/", "api/", "schemas/", "docs/", "build/", "tests/", "deploy/", "packaging/"} {
		if strings.HasPrefix(path, prefix) {
			return path
		}
	}
	if strings.Contains(path, "/") {
		return "internal/" + path
	}
	if previous != "" {
		return filepath.ToSlash(filepath.Join(filepath.Dir(previous), path))
	}
	return path
}

func declarationState(root, relative, symbol string) (string, error) {
	path := filepath.Join(root, filepath.FromSlash(relative))
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "unavailable", nil
	}
	if err != nil {
		return "", fmt.Errorf("stat handler declaration %s: %w", relative, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("handler declaration %s is not a regular file", relative)
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		return "", fmt.Errorf("parse handler declaration %s: %w", relative, err)
	}
	if fileDeclares(parsed, symbol) {
		return "present", nil
	}
	return "missing", nil
}

func fileDeclares(file *ast.File, symbol string) bool {
	for _, declaration := range file.Decls {
		switch typed := declaration.(type) {
		case *ast.FuncDecl:
			if typed.Name.Name == symbol {
				return true
			}
		case *ast.GenDecl:
			for _, spec := range typed.Specs {
				switch value := spec.(type) {
				case *ast.TypeSpec:
					if value.Name.Name == symbol {
						return true
					}
				case *ast.ValueSpec:
					for _, name := range value.Names {
						if name.Name == symbol {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

func validateRelativePath(path string) error {
	if path == "" || filepath.IsAbs(path) || path != filepath.ToSlash(path) {
		return fmt.Errorf("path %q must be nonempty relative slash form", path)
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(path)))
	if clean != path || clean == "." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("path %q is not confined canonical form", path)
	}
	return nil
}

func compareGeneratedOutputs(outputRoot string, outputs map[string][]byte) error {
	root, err := filepath.Abs(outputRoot)
	if err != nil {
		return fmt.Errorf("resolve output root: %w", err)
	}
	for _, relative := range generatedPaths {
		expected := outputs[relative]
		actual, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			return conformance("stale generated artifact", "/generated/"+escapePointer(relative), err.Error())
		}
		if !bytes.Equal(actual, expected) {
			return conformance("stale generated artifact", "/generated/"+escapePointer(relative), "bytes differ from accepted source closure")
		}
	}
	return nil
}

func writeGeneratedOutputs(outputRoot string, outputs map[string][]byte) error {
	root, err := filepath.Abs(outputRoot)
	if err != nil {
		return fmt.Errorf("resolve output root: %w", err)
	}
	for _, relative := range generatedPaths {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("create output directory for %s: %w", relative, err)
		}
		temporary, err := os.CreateTemp(filepath.Dir(path), ".atlas-*")
		if err != nil {
			return fmt.Errorf("create temporary %s: %w", relative, err)
		}
		temporaryPath := temporary.Name()
		keep := false
		defer func() {
			_ = temporary.Close()
			if !keep {
				_ = os.Remove(temporaryPath)
			}
		}()
		if err := temporary.Chmod(0o644); err != nil {
			return fmt.Errorf("set generated mode for %s: %w", relative, err)
		}
		if _, err := temporary.Write(outputs[relative]); err != nil {
			return fmt.Errorf("write generated %s: %w", relative, err)
		}
		if err := temporary.Sync(); err != nil {
			return fmt.Errorf("sync generated %s: %w", relative, err)
		}
		if err := temporary.Close(); err != nil {
			return fmt.Errorf("close generated %s: %w", relative, err)
		}
		if err := os.Rename(temporaryPath, path); err != nil {
			return fmt.Errorf("publish generated %s: %w", relative, err)
		}
		keep = true
	}
	return nil
}

func loadStrictJSON(path string, destination any) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("source is not a regular file")
	}
	if info.Size() > maxTraceabilityInputBytes {
		return nil, fmt.Errorf("source exceeds %d-byte limit", maxTraceabilityInputBytes)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := rejectDuplicateJSONKeys(contents); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	if err := decoder.Decode(destination); err != nil {
		return nil, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	return contents, nil
}

func rejectDuplicateJSONKeys(contents []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder, "", 0); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func consumeJSONValue(decoder *json.Decoder, pointer string, depth int) error {
	if depth > 256 {
		return errors.New("JSON nesting exceeds 256 levels")
	}
	tokenValue, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := tokenValue.(json.Delim)
	if !isDelimiter {
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
				return errors.New("JSON object key is not a string")
			}
			childPointer := pointer + "/" + escapePointer(key)
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON key at %s", childPointer)
			}
			seen[key] = struct{}{}
			if err := consumeJSONValue(decoder, childPointer, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("malformed JSON object")
		}
	case '[':
		index := 0
		for decoder.More() {
			if err := consumeJSONValue(decoder, pointer+"/"+strconv.Itoa(index), depth+1); err != nil {
				return err
			}
			index++
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("malformed JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("unexpected data after JSON document")
}

func marshalDocument(document any) ([]byte, error) {
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func conformance(kind, pointer, detail string) error {
	return &conformanceError{Kind: kind, Pointer: pointer, Detail: detail}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	return append([]string(nil), values...)
}

func cloneSources(values []sourceIdentity) []sourceIdentity {
	if len(values) == 0 {
		return []sourceIdentity{}
	}
	return append([]sourceIdentity(nil), values...)
}

func escapePointer(value string) string {
	value = strings.ReplaceAll(value, "~", "~0")
	return strings.ReplaceAll(value, "/", "~1")
}
