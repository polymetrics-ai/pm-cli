package compile

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const BundleSchemaVersion = "pm.bundle/v1"

type Bundle struct {
	SchemaVersion         string                          `json:"schema_version"`
	ConnectorID           string                          `json:"connector_id"`
	SourceDigest          string                          `json:"source_digest"`
	SemanticsRevision     string                          `json:"semantics_revision"`
	Operations            []contract.Operation            `json:"operations"`
	Commands              []contract.Command              `json:"commands"`
	UnavailableOperations []contract.UnavailableOperation `json:"unavailable_operations"`
}

type CompileReport struct {
	SchemaVersion     string                 `json:"schema_version"`
	ConnectorID       string                 `json:"connector_id"`
	SourceDigest      string                 `json:"source_digest"`
	SemanticsRevision string                 `json:"semantics_revision"`
	Conversions       []ConversionProvenance `json:"conversions"`
	Operations        []NormalizedOperation  `json:"operations"`
	Diagnostics       []Diagnostic           `json:"diagnostics"`
}

type CommandManifest struct {
	SchemaVersion string             `json:"schema_version"`
	ConnectorID   string             `json:"connector_id"`
	SourceDigest  string             `json:"source_digest"`
	Commands      []contract.Command `json:"commands"`
}
type CompilationInput struct {
	SourceLock           []byte
	Captures             Captures
	Semantics            []byte
	PreviousSourceDigest string
	RevisionDiff         []byte
	TargetFoundations    []contract.FoundationContract
}

type CompilationArtifacts struct {
	Bundle          []byte
	Report          []byte
	CommandManifest []byte
}

func Compile(ctx context.Context, input CompilationInput) (CompilationArtifacts, error) {
	lock, err := DecodeSourceLock(input.SourceLock)
	if err != nil {
		return CompilationArtifacts{}, err
	}
	captured, err := CaptureSource(lock, input.Captures)
	if err != nil {
		return CompilationArtifacts{}, err
	}
	resolved, err := ResolveRefs(captured)
	if err != nil {
		return CompilationArtifacts{}, err
	}
	loaded, err := LoadDialects(ctx, resolved)
	if err != nil {
		return CompilationArtifacts{}, err
	}
	normalization, err := Normalize(ctx, loaded)
	if err != nil {
		return CompilationArtifacts{}, err
	}
	if len(input.Semantics) != 0 {
		semantics, decodeErr := DecodeSemantics(input.Semantics)
		if decodeErr != nil {
			return CompilationArtifacts{}, decodeErr
		}
		if err := ApplyAmendments(normalization, resolved, &semantics); err != nil {
			return CompilationArtifacts{}, err
		}
	}
	var diff *SourceRevisionDiff
	if len(input.RevisionDiff) != 0 {
		decoded, decodeErr := DecodeSourceRevisionDiff(input.RevisionDiff)
		if decodeErr != nil {
			return CompilationArtifacts{}, decodeErr
		}
		diff = &decoded
	}
	if err := ValidateRevisionDiff(input.PreviousSourceDigest, captured.Digest, diff); err != nil {
		return CompilationArtifacts{}, err
	}
	if err := ValidateCurrentRevisionDiff(resolved, diff); err != nil {
		return CompilationArtifacts{}, err
	}
	targetFoundations := input.TargetFoundations
	if len(targetFoundations) == 0 {
		targetFoundations = ConnectorCompilationTargetFoundations()
	}
	if err := CheckRequirements(normalization, resolved, targetFoundations); err != nil {
		return CompilationArtifacts{}, err
	}
	projectedCommands, commandDiagnostics := ProjectCommands(normalization)
	normalization.Diagnostics = append(normalization.Diagnostics, commandDiagnostics...)
	sortDiagnostics(normalization.Diagnostics)
	report, err := EmitReport(normalization)
	if err != nil {
		return CompilationArtifacts{}, err
	}
	commandManifest, err := emitCommands(normalization, projectedCommands)
	if err != nil {
		return CompilationArtifacts{Report: report}, err
	}
	bundle, err := emitBundle(normalization, targetFoundations, projectedCommands)
	if err != nil {
		return CompilationArtifacts{Report: report, CommandManifest: commandManifest}, err
	}
	return CompilationArtifacts{Bundle: bundle, Report: report, CommandManifest: commandManifest}, nil
}

func EmitBundle(normalization *Normalization, supported []contract.FoundationContract) ([]byte, error) {
	if normalization == nil {
		return nil, sourceError(ErrInvalidSource, "", "", "normalization is required")
	}
	commands, _ := ProjectCommands(normalization)
	return emitBundle(normalization, supported, commands)
}

func emitBundle(
	normalization *Normalization,
	supported []contract.FoundationContract,
	commands []contract.Command,
) ([]byte, error) {
	if normalization == nil {
		return nil, sourceError(ErrInvalidSource, "", "", "normalization is required")
	}
	diagnosticsByOperation := make(map[string][]Diagnostic)
	operationsByID := make(map[string]NormalizedOperation, len(normalization.Operations))
	for _, operation := range normalization.Operations {
		operationsByID[operation.OperationID] = operation
	}
	for _, diagnostic := range normalization.Diagnostics {
		if diagnostic.Severity != SeverityError {
			continue
		}
		// A diagnostic whose operation is not in the normalized inventory keeps
		// the interface-description grammar; it is refused by the inventory check
		// below instead of being admitted as documentation metadata.
		dialect := DialectOpenAPI30
		if operation, attributed := operationsByID[diagnostic.OperationID]; attributed {
			dialect = sourceSelectorDialect(operation)
		}
		if !isolatableOperationDiagnostic(diagnostic, dialect) {
			return nil, diagnosticEmissionError(diagnostic)
		}
		diagnosticsByOperation[diagnostic.OperationID] = append(diagnosticsByOperation[diagnostic.OperationID], diagnostic)
	}
	if len(normalization.SourceOperationIDs) == 0 {
		return nil, sourceError(ErrInvalidSource, "", "/operations", "source-selected operation inventory is required")
	}
	if len(normalization.Operations) != len(normalization.SourceOperationIDs) {
		return nil, sourceError(ErrSourceFidelity, "", "/operations", "normalized operation count disagrees with the source-selected inventory")
	}
	sourceSelectedOperations := make(map[string]struct{}, len(normalization.SourceOperationIDs))
	for index, operationID := range normalization.SourceOperationIDs {
		if !validIdentity(operationID) {
			return nil, sourceError(ErrInvalidSource, "", fmt.Sprintf("/source_operations/%d", index), "source-selected operation identity is invalid")
		}
		if _, duplicate := sourceSelectedOperations[operationID]; duplicate {
			return nil, sourceError(ErrSourceFidelity, "", fmt.Sprintf("/source_operations/%d", index), "source-selected operation identity is duplicated")
		}
		sourceSelectedOperations[operationID] = struct{}{}
	}
	operations := slices.Clone(normalization.Operations)
	slices.SortFunc(operations, func(left, right NormalizedOperation) int {
		return strings.Compare(left.OperationID, right.OperationID)
	})
	projectedCommands := slices.Clone(commands)
	if projectedCommands == nil {
		projectedCommands = []contract.Command{}
	}
	bundle := Bundle{
		SchemaVersion: BundleSchemaVersion, ConnectorID: normalization.ConnectorID,
		SourceDigest: normalization.SourceDigest, SemanticsRevision: normalization.SemanticsRevision,
		Operations: make([]contract.Operation, 0, len(operations)),
		Commands:   projectedCommands, UnavailableOperations: make([]contract.UnavailableOperation, 0),
	}
	if len(operations) == 0 || !validIdentity(bundle.ConnectorID) || !validDigest(bundle.SourceDigest) || !validDigest(bundle.SemanticsRevision) {
		return nil, sourceError(ErrInvalidSource, "", "", "bundle identity, source digest, semantics revision and combined operation inventory are required")
	}
	seenOperations := make(map[string]struct{}, len(operations))
	executableOperations := make(map[string]struct{}, len(operations))
	for index := range operations {
		normalized := operations[index]
		if !validIdentity(normalized.OperationID) {
			return nil, sourceError(ErrInvalidSource, "", fmt.Sprintf("/operations/%d/operation_id", index), "operation identity is invalid")
		}
		if _, selected := sourceSelectedOperations[normalized.OperationID]; !selected {
			return nil, sourceError(ErrSourceFidelity, "", fmt.Sprintf("/operations/%d/operation_id", index), "operation is outside the source-selected inventory")
		}
		if _, duplicate := seenOperations[normalized.OperationID]; duplicate {
			return nil, sourceError(ErrSourceFidelity, "", fmt.Sprintf("/operations/%d/operation_id", index), "operation identity is duplicated across the bundle inventory")
		}
		seenOperations[normalized.OperationID] = struct{}{}
		operationDiagnostics := diagnosticsByOperation[normalized.OperationID]
		if len(operationDiagnostics) != 0 || len(normalized.BlockedDiagnosticIDs) != 0 {
			unavailable, err := emitUnavailableOperation(normalized, operationDiagnostics, supported)
			if err != nil {
				return nil, sourceError(ErrUnsupportedShape, "", fmt.Sprintf("/operations/%d", index), err.Error())
			}
			bundle.UnavailableOperations = append(bundle.UnavailableOperations, unavailable)
			continue
		}
		operation, err := emitOperation(normalized, supported)
		if err != nil {
			return nil, sourceError(ErrUnsupportedShape, "", fmt.Sprintf("/operations/%d", index), err.Error())
		}
		bundle.Operations = append(bundle.Operations, operation)
		executableOperations[operation.OperationID] = struct{}{}
	}
	for operationID := range diagnosticsByOperation {
		if _, present := seenOperations[operationID]; !present {
			return nil, sourceError(ErrInvalidSource, "", "/diagnostics", "operation diagnostic refers to an operation outside the source-selected inventory")
		}
	}
	seenCommandNames := make(map[string]struct{}, len(bundle.Commands))
	seenCommandOperations := make(map[string]struct{}, len(bundle.Commands))
	for _, command := range bundle.Commands {
		if err := command.Validate(); err != nil {
			return nil, sourceError(ErrInvalidSource, "", "/commands", err.Error())
		}
		if _, executable := executableOperations[command.OperationID]; !executable {
			return nil, sourceError(ErrSourceFidelity, "", "/commands", "command refers to an unavailable or absent executable operation")
		}
		if _, duplicate := seenCommandNames[command.Name]; duplicate {
			return nil, sourceError(ErrSourceFidelity, "", "/commands", "command name is duplicated")
		}
		if _, duplicate := seenCommandOperations[command.OperationID]; duplicate {
			return nil, sourceError(ErrSourceFidelity, "", "/commands", "operation has more than one runtime command descriptor")
		}
		seenCommandNames[command.Name] = struct{}{}
		seenCommandOperations[command.OperationID] = struct{}{}
	}
	if len(bundle.Operations)+len(bundle.UnavailableOperations) == 0 {
		return nil, sourceError(ErrInvalidSource, "", "/operations", "combined operation inventory must not be empty")
	}
	return marshalCanonicalDocument(bundle)
}

func EmitReport(normalization *Normalization) ([]byte, error) {
	if normalization == nil {
		return nil, sourceError(ErrInvalidSource, "", "", "normalization is required")
	}
	report := CompileReport{
		SchemaVersion: ReportSchemaVersion, ConnectorID: normalization.ConnectorID,
		SourceDigest: normalization.SourceDigest, SemanticsRevision: normalization.SemanticsRevision,
		Conversions: slices.Clone(normalization.Conversions), Operations: slices.Clone(normalization.Operations),
		Diagnostics: slices.Clone(normalization.Diagnostics),
	}
	return marshalCanonicalDocument(report)
}

func EmitCommands(normalization *Normalization) ([]byte, error) {
	if normalization == nil {
		return nil, sourceError(ErrInvalidSource, "", "", "normalization is required")
	}
	commands, _ := ProjectCommands(normalization)
	return emitCommands(normalization, commands)
}

func emitCommands(normalization *Normalization, commands []contract.Command) ([]byte, error) {
	if normalization == nil {
		return nil, sourceError(ErrInvalidSource, "", "", "normalization is required")
	}
	projected := slices.Clone(commands)
	if projected == nil {
		projected = []contract.Command{}
	}
	manifest := CommandManifest{
		SchemaVersion: CommandsSchemaVersion, ConnectorID: normalization.ConnectorID,
		SourceDigest: normalization.SourceDigest, Commands: projected,
	}
	return marshalCanonicalDocument(manifest)
}

// sourceSelectorDialect returns the dialect whose selector grammar governs the
// source facts of one normalized operation: the compiler lowers a documentation
// dialect to the native PostgreSQL transport (normalizeDocumentationOperations)
// and every interface-description dialect to the HTTP transport, so the
// operation itself names the grammar of its source set. All three
// interface-description dialects share one RFC 6901 pointer grammar, which
// DialectOpenAPI30 names here; an operation that declares neither transport keeps
// that grammar and can therefore never admit a documentation anchor.
func sourceSelectorDialect(operation NormalizedOperation) Dialect {
	if operation.Transport.Kind == contract.TransportKindPostgreSQL {
		return DialectPostgreSQLDocs16
	}
	return DialectOpenAPI30
}

// isolatableOperationDiagnostic reports whether one diagnostic can become
// unavailable operation metadata instead of refusing the whole bundle. Every
// source fact must name a well-formed selector of the source grammar the
// operation was lowered from, so a documentation anchor is admitted exactly
// where the operation declares the documentation transport.
func isolatableOperationDiagnostic(diagnostic Diagnostic, dialect Dialect) bool {
	if diagnostic.ID == "" || diagnostic.OperationID == "" || len(diagnostic.SourceFacts) == 0 || len(diagnostic.AffectedCells) == 0 {
		return false
	}
	for _, fact := range diagnostic.SourceFacts {
		if !validIdentity(fact.SourceRef.SourceID) || fact.SourceRef.Pointer == "" ||
			!validSourceSelector(dialect, fact.SourceRef.Pointer) || !validDigest(fact.SHA256) {
			return false
		}
	}
	seenLanes := make(map[contract.Lane]struct{}, len(diagnostic.AffectedCells))
	for _, lane := range diagnostic.AffectedCells {
		if err := contract.ValidateLane(lane); err != nil {
			return false
		}
		if _, duplicate := seenLanes[lane]; duplicate {
			return false
		}
		seenLanes[lane] = struct{}{}
	}
	switch diagnostic.Code {
	case "unsupported_http_transport",
		"unsupported_http_authority",
		"unsupported_authentication",
		"unsupported_fixed_headers",
		"unsupported_parameter_shape",
		"unknown_sensitivity",
		"missing_request_schema",
		"unsupported_request_serialization",
		"unsupported_response_metadata",
		"unbound_response_header",
		"missing_response_schema",
		"missing_schema",
		"recursive_schema_reference",
		"boolean_schema",
		"unsupported_schema_keyword",
		"unsupported_schema_type",
		"unsupported_union_constraint",
		"unsupported_union",
		"unsupported_schema_constraint",
		"unsupported_all_of",
		"missing_array_items":
		return true
	default:
		return false
	}
}

func diagnosticEmissionError(diagnostic Diagnostic) error {
	kind := ErrUnsupportedShape
	switch diagnostic.Code {
	case "source_fidelity_disagreement", "invalid_source_default", "invalid_normalized_binding", "invalid_normalized_schema":
		kind = ErrSourceFidelity
	}
	return sourceError(
		kind,
		"",
		diagnostic.NormalizedPointer,
		"bundle emission is blocked by diagnostic "+diagnostic.ID+" ("+diagnostic.Code+"): "+diagnostic.Detail,
	)
}

func emitUnavailableOperation(
	normalized NormalizedOperation,
	diagnostics []Diagnostic,
	supported []contract.FoundationContract,
) (contract.UnavailableOperation, error) {
	if len(diagnostics) == 0 || len(normalized.BlockedDiagnosticIDs) == 0 {
		return contract.UnavailableOperation{}, fmt.Errorf("operation %q has no complete source-linked unsupported diagnostic set", normalized.OperationID)
	}
	if err := validateUnavailableRequirements(normalized.CapabilityRequirements, supported); err != nil {
		return contract.UnavailableOperation{}, fmt.Errorf("operation %q has invalid capability requirements: %w", normalized.OperationID, err)
	}
	diagnosticsByID := make(map[string]Diagnostic, len(diagnostics))
	diagnosticCodes := make([]string, 0, len(diagnostics))
	codesByLane := make(map[contract.Lane][]string)
	for _, diagnostic := range diagnostics {
		if !isolatableOperationDiagnostic(diagnostic, sourceSelectorDialect(normalized)) || diagnostic.OperationID != normalized.OperationID {
			return contract.UnavailableOperation{}, fmt.Errorf("operation %q has a diagnostic that cannot become unavailable metadata", normalized.OperationID)
		}
		if _, duplicate := diagnosticsByID[diagnostic.ID]; duplicate {
			return contract.UnavailableOperation{}, fmt.Errorf("operation %q has a duplicate diagnostic identity", normalized.OperationID)
		}
		diagnosticsByID[diagnostic.ID] = diagnostic
		diagnosticCodes = append(diagnosticCodes, diagnostic.Code)
		for _, lane := range diagnostic.AffectedCells {
			codesByLane[lane] = append(codesByLane[lane], diagnostic.Code)
		}
	}
	blockedIDs := make(map[string]struct{}, len(normalized.BlockedDiagnosticIDs))
	for _, diagnosticID := range normalized.BlockedDiagnosticIDs {
		if _, duplicate := blockedIDs[diagnosticID]; duplicate {
			return contract.UnavailableOperation{}, fmt.Errorf("operation %q repeats a blocked diagnostic identity", normalized.OperationID)
		}
		if _, present := diagnosticsByID[diagnosticID]; !present {
			return contract.UnavailableOperation{}, fmt.Errorf("operation %q is blocked by a non-isolatable or unknown diagnostic", normalized.OperationID)
		}
		blockedIDs[diagnosticID] = struct{}{}
	}
	for diagnosticID := range diagnosticsByID {
		if _, blocked := blockedIDs[diagnosticID]; !blocked {
			return contract.UnavailableOperation{}, fmt.Errorf("operation %q diagnostic is detached from its blocked operation boundary", normalized.OperationID)
		}
	}
	slices.Sort(diagnosticCodes)
	diagnosticCodes = slices.Compact(diagnosticCodes)
	unavailable := contract.UnavailableOperation{
		OperationID:     normalized.OperationID,
		SourceRefs:      slices.Clone(normalized.SourceRefs),
		DiagnosticCodes: diagnosticCodes,
		LaneEligibility: make([]contract.LaneEligibility, 0, len(normalized.LaneInventory)),
	}
	for _, disposition := range normalized.LaneInventory {
		status, ok := contractLaneStatus(disposition.Status)
		if !ok {
			return contract.UnavailableOperation{}, fmt.Errorf("operation %q has an unsupported lane status", normalized.OperationID)
		}
		reason := disposition.Reason
		affectedCodes := codesByLane[disposition.Lane]
		if len(affectedCodes) != 0 {
			slices.Sort(affectedCodes)
			affectedCodes = slices.Compact(affectedCodes)
			if status == contract.LaneEligible {
				status = contract.LaneBlocked
				reason = "compiler diagnostics block this lane: " + strings.Join(affectedCodes, ", ")
			} else if status == contract.LaneBlocked && strings.HasPrefix(reason, "compiler diagnostics block this lane") {
				reason = "compiler diagnostics block this lane: " + strings.Join(affectedCodes, ", ")
			}
		}
		if status == contract.LaneEligible {
			return contract.UnavailableOperation{}, fmt.Errorf("operation %q retained an executable lane outside its diagnostic boundary", normalized.OperationID)
		}
		unavailable.LaneEligibility = append(unavailable.LaneEligibility, contract.LaneEligibility{
			Lane: disposition.Lane, Status: status, Reason: reason,
		})
	}
	if err := unavailable.Validate(); err != nil {
		return contract.UnavailableOperation{}, err
	}
	return unavailable, nil
}

func validateUnavailableRequirements(
	requirements []contract.CapabilityRequirement,
	supported []contract.FoundationContract,
) error {
	if len(requirements) == 0 {
		return fmt.Errorf("exact compiler-derived capability closure is absent")
	}
	seenRequirements := make(map[string]struct{}, len(requirements))
	seenFoundations := make(map[string]struct{}, len(requirements))
	foundations := make([]contract.FoundationRef, 0, len(requirements))
	for _, requirement := range requirements {
		if err := requirement.Validate(); err != nil {
			return err
		}
		requirementKey := requirement.Capability + "\x00" + requirement.Foundation.ID
		if _, duplicate := seenRequirements[requirementKey]; duplicate {
			return fmt.Errorf("capability/foundation binding is duplicated")
		}
		seenRequirements[requirementKey] = struct{}{}
		foundationKey := requirement.Foundation.ID + "\x00" + requirement.Foundation.Version
		if _, seen := seenFoundations[foundationKey]; !seen {
			seenFoundations[foundationKey] = struct{}{}
			foundations = append(foundations, requirement.Foundation)
		}
	}
	return contract.CheckFoundationCompatibility(foundations, supported)
}

func emitOperation(normalized NormalizedOperation, supported []contract.FoundationContract) (contract.Operation, error) {
	if len(normalized.BlockedDiagnosticIDs) != 0 {
		return contract.Operation{}, fmt.Errorf("operation %q is blocked by diagnostics", normalized.OperationID)
	}
	if len(normalized.CapabilityRequirements) == 0 {
		return contract.Operation{}, fmt.Errorf("operation %q has no exact compiler-derived capability closure", normalized.OperationID)
	}
	operation := contract.Operation{
		SchemaVersion: contract.OperationSchemaVersion, OperationID: normalized.OperationID,
		SourceRefs: slices.Clone(normalized.SourceRefs), Transport: normalized.Transport,
		Inputs: []contract.FieldBinding{}, RequestVariants: []contract.RequestVariant{},
		Responses: []contract.Response{}, CapabilityRequirements: slices.Clone(normalized.CapabilityRequirements),
		LaneEligibility: []contract.LaneEligibility{},
	}
	effectiveHTTP := effectiveOperationHTTP(normalized)
	for _, fact := range normalized.CommonBindings {
		if normalized.HTTP != nil && normalized.HTTP.Authorization != nil &&
			effectiveHTTP != nil && effectiveHTTP.Authorization == nil &&
			fact.FieldID == normalized.HTTP.Authorization.FieldID {
			continue
		}
		if fact.Normalized == nil || fact.BlockedDiagnosticID != "" {
			return contract.Operation{}, fmt.Errorf("operation %q has an unadmitted common binding %q", normalized.OperationID, fact.FieldID)
		}
		if fact.Normalized.Location == contract.FieldLocationBody {
			return contract.Operation{}, fmt.Errorf("operation %q has a body binding outside its request variant", normalized.OperationID)
		}
		operation.Inputs = append(operation.Inputs, *fact.Normalized)
	}
	for _, normalizedVariant := range normalized.RequestVariants {
		if normalizedVariant.MediaType == parameterOnlyMediaType {
			if normalizedVariant.Schema != nil || len(normalizedVariant.Bindings) != 0 || normalizedVariant.BodyRequired {
				return contract.Operation{}, fmt.Errorf("operation %q has contradictory parameter-only request facts", normalized.OperationID)
			}
			continue
		}
		if normalizedVariant.Schema == nil || len(normalizedVariant.BlockedDiagnosticIDs) != 0 {
			return contract.Operation{}, fmt.Errorf("operation %q has an unadmitted request variant %q", normalized.OperationID, normalizedVariant.VariantID)
		}
		variant := contract.RequestVariant{
			VariantID: normalizedVariant.VariantID, MediaType: normalizedVariant.MediaType,
			Required: normalizedVariant.BodyRequired, Schema: *normalizedVariant.Schema,
			Inputs: []contract.FieldBinding{}, SourceRefs: sourceRefs(normalizedVariant.SourceFacts),
		}
		for _, fact := range normalizedVariant.Bindings {
			if fact.Normalized == nil || fact.BlockedDiagnosticID != "" {
				return contract.Operation{}, fmt.Errorf("operation %q has an unadmitted request binding %q", normalized.OperationID, fact.FieldID)
			}
			variant.Inputs = append(variant.Inputs, *fact.Normalized)
		}
		operation.RequestVariants = append(operation.RequestVariants, variant)
	}
	if len(operation.RequestVariants) > 1 {
		selector, err := requestMediaSelector(normalized.OperationID, operation.RequestVariants, operation.Inputs)
		if err != nil {
			return contract.Operation{}, err
		}
		operation.RequestMediaFieldID = selector.FieldID
		operation.Inputs = append(operation.Inputs, selector)
	}
	recordPointerMatched := false
	for _, normalizedResponse := range normalized.ResponseVariants {
		if len(normalizedResponse.BlockedDiagnosticIDs) != 0 {
			return contract.Operation{}, fmt.Errorf("operation %q has an unadmitted response variant", normalized.OperationID)
		}
		response := contract.Response{
			Status: normalizedResponse.Status, NoContent: normalizedResponse.NoContent,
			SourceRefs: sourceRefs(normalizedResponse.SourceFacts),
		}
		if !response.NoContent {
			if normalizedResponse.Schema == nil || normalizedResponse.MediaType == "" {
				return contract.Operation{}, fmt.Errorf("operation %q lost a response media schema", normalized.OperationID)
			}
			response.MediaType = normalizedResponse.MediaType
			response.Schema = normalizedResponse.Schema
			if normalized.Semantics != nil && normalized.Semantics.RecordExtraction != nil {
				pointer := normalized.Semantics.RecordExtraction.RecordsPointer
				if schemaContainsPointer(*normalizedResponse.Schema, pointer) {
					response.RecordPointer = &pointer
					recordPointerMatched = true
				}
			}
		}
		operation.Responses = append(operation.Responses, response)
	}
	if normalized.Semantics != nil && normalized.Semantics.RecordExtraction != nil && !recordPointerMatched {
		return contract.Operation{}, fmt.Errorf("operation %q record extraction pointer does not address any emitted response schema", normalized.OperationID)
	}
	operation.Execution = emitOperationExecution(normalized)
	for _, disposition := range normalized.LaneInventory {
		status, ok := contractLaneStatus(disposition.Status)
		if !ok {
			return contract.Operation{}, fmt.Errorf("operation %q has an unsupported lane status", normalized.OperationID)
		}
		operation.LaneEligibility = append(operation.LaneEligibility, contract.LaneEligibility{
			Lane: disposition.Lane, Status: status, Reason: disposition.Reason,
		})
	}
	slices.SortFunc(operation.Inputs, func(left, right contract.FieldBinding) int {
		return strings.Compare(left.FieldID, right.FieldID)
	})
	slices.SortFunc(operation.RequestVariants, func(left, right contract.RequestVariant) int {
		return strings.Compare(left.VariantID, right.VariantID)
	})
	slices.SortFunc(operation.Responses, func(left, right contract.Response) int {
		if compared := strings.Compare(left.Status, right.Status); compared != 0 {
			return compared
		}
		return strings.Compare(left.MediaType, right.MediaType)
	})
	slices.SortFunc(operation.CapabilityRequirements, func(left, right contract.CapabilityRequirement) int {
		if compared := strings.Compare(left.Capability, right.Capability); compared != 0 {
			return compared
		}
		if compared := strings.Compare(left.Foundation.ID, right.Foundation.ID); compared != 0 {
			return compared
		}
		return strings.Compare(left.Foundation.Version, right.Foundation.Version)
	})
	if err := operation.Validate(supported); err != nil {
		return contract.Operation{}, err
	}
	return operation, nil
}

func emitOperationExecution(normalized NormalizedOperation) *contract.OperationExecution {
	httpSemantics := effectiveOperationHTTP(normalized)
	var pagination *Pagination
	var retry *RetrySemantics
	if normalized.Semantics != nil {
		pagination = normalized.Semantics.Pagination
		retry = normalized.Semantics.Retry
	}
	if httpSemantics == nil && pagination == nil && retry == nil {
		return nil
	}
	execution := &contract.OperationExecution{}
	if httpSemantics != nil {
		httpFact := semanticsToContractHTTP(*httpSemantics)
		execution.HTTP = &httpFact
	}
	if pagination != nil {
		source := pagination
		execution.Pagination = &contract.OperationPagination{
			Kind:                    contract.OperationPaginationKind(source.Kind),
			CursorRequestField:      source.CursorRequestField,
			CursorResponsePointer:   source.CursorResponsePointer,
			OffsetRequestField:      source.OffsetRequestField,
			LimitRequestField:       source.LimitRequestField,
			TotalResponsePointer:    source.TotalResponsePointer,
			PageRequestField:        source.PageRequestField,
			LastPageResponsePointer: source.LastPageResponsePointer,
			LinkRequestField:        source.LinkRequestField,
			LinkResponseHeader:      source.LinkResponseHeader,
			Witness: contract.ExecutionWitness{
				ProofID:    source.Witness.ProofID,
				SourceRefs: slices.Clone(source.Witness.SourceRefs),
			},
		}
	}
	if retry != nil {
		source := retry
		execution.Retry = &contract.OperationRetry{
			Classification: contract.OperationRetryClassification(source.Classification),
			Condition:      source.Condition,
			HTTPStatuses:   slices.Clone(source.HTTPStatuses),
			Witness: contract.ExecutionWitness{
				ProofID:    source.Witness.ProofID,
				SourceRefs: slices.Clone(source.Witness.SourceRefs),
			},
		}
	}
	return execution
}

func requestMediaSelector(operationID string, variants []contract.RequestVariant, common []contract.FieldBinding) (contract.FieldBinding, error) {
	fieldID := requestMediaFieldID(operationID)
	for _, binding := range common {
		if binding.FieldID == fieldID || (binding.Location == contract.FieldLocationHeader && strings.EqualFold(binding.WireName, contract.RequestMediaTypeWireName)) {
			return contract.FieldBinding{}, fmt.Errorf("operation %q collides with its compiler-derived request media selector", operationID)
		}
	}
	enum := make([]contract.PresentValue, 0, len(variants))
	for _, variant := range variants {
		enum = append(enum, contract.NewStringValue(variant.MediaType))
	}
	return contract.FieldBinding{
		FieldID: fieldID, WireName: contract.RequestMediaTypeWireName, Location: contract.FieldLocationHeader,
		JSONPointer: "", Required: false, Nullable: false, Style: "simple", Explode: false,
		AllowReserved: false, Sensitivity: contract.SensitivityPublic,
		Schema: contract.Schema{Type: contract.SchemaTypeString, Enum: enum},
	}, nil
}

func requestMediaFieldID(operationID string) string {
	return operationID + ".request-media-type"
}

func contractLaneStatus(status LaneStatus) (contract.LaneStatus, bool) {
	switch status {
	case LaneEligible:
		return contract.LaneEligible, true
	case LaneUnsupported:
		return contract.LaneUnsupported, true
	case LaneNotApplicable:
		return contract.LaneNotApplicable, true
	case LaneBlocked:
		return contract.LaneBlocked, true
	case LaneUnproven:
		return contract.LaneUnproven, true
	default:
		return "", false
	}
}

func sourceRefs(facts []SourceFact) []contract.SourceRef {
	result := make([]contract.SourceRef, 0, len(facts))
	for _, fact := range facts {
		result = append(result, fact.SourceRef)
	}
	slices.SortFunc(result, func(left, right contract.SourceRef) int {
		if compared := strings.Compare(left.SourceID, right.SourceID); compared != 0 {
			return compared
		}
		return strings.Compare(left.Pointer, right.Pointer)
	})
	return slices.CompactFunc(result, func(left, right contract.SourceRef) bool {
		return left.SourceID == right.SourceID && left.Pointer == right.Pointer
	})
}

func marshalCanonicalDocument(value any) ([]byte, error) {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}
