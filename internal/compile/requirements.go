package compile

import (
	"fmt"
	"slices"
	"strings"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

var baseHTTPRequirementNames = [...]string{
	"effect.authorize",
	"effect.limit",
	"http.execute",
}

func HTTPCompilationTargetFoundations() []contract.FoundationContract {
	declarations := contract.HTTPExecutionCapabilities()
	byIdentity := make(map[string]contract.FoundationContract, len(declarations))
	for _, declaration := range declarations {
		key := declaration.Foundation.ID + "\x00" + declaration.Foundation.Version
		byIdentity[key] = contract.FoundationContract{
			ID: declaration.Foundation.ID, Version: declaration.Foundation.Version,
			Dependencies: []contract.FoundationRef{},
		}
	}
	keys := make([]string, 0, len(byIdentity))
	for key := range byIdentity {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	result := make([]contract.FoundationContract, 0, len(keys))
	for _, key := range keys {
		result = append(result, byIdentity[key])
	}
	return result
}

func CheckRequirements(
	normalization *Normalization,
	source *ResolvedSource,
	targetFoundations []contract.FoundationContract,
) error {
	if normalization == nil || source == nil || source.Captured == nil {
		return sourceError(ErrInvalidSource, "", "", "normalization and source are required")
	}
	if normalization.ConnectorID != source.Captured.Lock.ConnectorID || normalization.SourceDigest != source.Captured.Digest ||
		normalization.SemanticsRevision != source.Captured.Lock.SemanticsRevision {
		return sourceError(ErrSourceFidelity, "", "", "normalization identity is detached from its captured source")
	}
	if len(normalization.Operations) != len(source.Captured.Lock.Operations) {
		return sourceError(ErrSourceFidelity, "", "/operations", "normalized operation count disagrees with the source lock")
	}
	declarations, err := indexedHTTPDeclarations()
	if err != nil {
		return err
	}
	lockedIDs := make(map[string]struct{}, len(source.Captured.Lock.Operations))
	for _, operation := range source.Captured.Lock.Operations {
		lockedIDs[operation.SourceOperationID] = struct{}{}
	}
	diagnosticIDs := make(map[string]struct{}, len(normalization.Diagnostics))
	for _, diagnostic := range normalization.Diagnostics {
		if diagnostic.ID == "" {
			return sourceError(ErrInvalidSource, "", "/diagnostics", "diagnostic identity is missing")
		}
		if _, duplicate := diagnosticIDs[diagnostic.ID]; duplicate {
			return sourceError(ErrInvalidSource, "", "/diagnostics", "diagnostic identity is duplicated")
		}
		diagnosticIDs[diagnostic.ID] = struct{}{}
		if err := checkSourceFacts(source, diagnostic.SourceFacts, "/diagnostics"); err != nil {
			return err
		}
	}
	seenOperations := make(map[string]struct{}, len(normalization.Operations))
	for operationIndex := range normalization.Operations {
		operation := &normalization.Operations[operationIndex]
		pointer := fmt.Sprintf("/operations/%d", operationIndex)
		if _, expected := lockedIDs[operation.OperationID]; !expected {
			return sourceError(ErrSourceFidelity, "", pointer+"/operation_id", "normalization introduced an operation")
		}
		if _, duplicate := seenOperations[operation.OperationID]; duplicate {
			return sourceError(ErrSourceFidelity, "", pointer+"/operation_id", "normalized operation identity is duplicated")
		}
		seenOperations[operation.OperationID] = struct{}{}
		if operation.Transport.Kind != contract.TransportKindHTTP {
			return sourceError(ErrUnsupportedShape, "", pointer+"/transport", "compiler target has no admitted capability mapping for this transport")
		}
		if err := checkLaneInventory(operation.LaneInventory, pointer+"/lane_inventory"); err != nil {
			return err
		}
		if len(operation.RequestVariants) != len(operation.Raw.RequestVariants) || len(operation.ResponseVariants) != len(operation.Raw.ResponseVariants) {
			return sourceError(ErrSourceFidelity, "", pointer, "normalization dropped a request or response variant")
		}
		if err := checkSourceFacts(source, operation.Raw.SourceFacts, pointer+"/raw/source_facts"); err != nil {
			return err
		}
		if err := checkSourceFacts(source, operation.HTTPFacts, pointer+"/http_facts"); err != nil {
			return err
		}
		if operation.HTTP != nil {
			if !slices.Equal(operation.HTTP.Witness.SourceRefs, sourceRefs(operation.HTTPFacts)) {
				return sourceError(ErrSourceFidelity, "", pointer+"/http", "normalized HTTP witness disagrees with its exact source facts")
			}
		}
		fieldIDs := make(map[string]struct{})
		for _, binding := range operation.CommonBindings {
			if err := checkBindingFact(source, binding, diagnosticIDs, fieldIDs, pointer+"/common_bindings"); err != nil {
				return err
			}
		}
		variantIDs := make(map[string]struct{}, len(operation.RequestVariants))
		variantMedia := make(map[string]struct{}, len(operation.RequestVariants))
		for _, variant := range operation.RequestVariants {
			if !validIdentity(variant.VariantID) || variant.MediaType == "" {
				return sourceError(ErrInvalidSource, "", pointer+"/request_variants", "request variant identity and media type are required")
			}
			if _, duplicate := variantIDs[variant.VariantID]; duplicate {
				return sourceError(ErrSourceFidelity, "", pointer+"/request_variants", "request variant identity is duplicated")
			}
			if _, duplicate := variantMedia[variant.MediaType]; duplicate {
				return sourceError(ErrSourceFidelity, "", pointer+"/request_variants", "request media type is duplicated")
			}
			variantIDs[variant.VariantID] = struct{}{}
			variantMedia[variant.MediaType] = struct{}{}
			if err := checkSourceFacts(source, variant.SourceFacts, pointer+"/request_variants"); err != nil {
				return err
			}
			for _, binding := range variant.Bindings {
				if err := checkBindingFact(source, binding, diagnosticIDs, fieldIDs, pointer+"/request_variants"); err != nil {
					return err
				}
			}
			for _, diagnosticID := range variant.BlockedDiagnosticIDs {
				if _, ok := diagnosticIDs[diagnosticID]; !ok {
					return sourceError(ErrInvalidSource, "", pointer+"/request_variants", "variant refers to an unknown diagnostic")
				}
			}
		}
		if operation.Semantics != nil && operation.Semantics.Pagination != nil {
			for _, fieldID := range paginationRequestFields(*operation.Semantics.Pagination) {
				if _, ok := fieldIDs[fieldID]; !ok {
					return sourceError(ErrUnsafeAmendment, "", pointer+"/semantics/pagination", "pagination references a request field outside the normalized operation")
				}
			}
		}
		responseKeys := make(map[string]struct{}, len(operation.ResponseVariants))
		for _, response := range operation.ResponseVariants {
			key := response.Status + "\x00" + response.MediaType
			if _, duplicate := responseKeys[key]; duplicate {
				return sourceError(ErrSourceFidelity, "", pointer+"/response_variants", "response status/media variant is duplicated")
			}
			responseKeys[key] = struct{}{}
			if err := checkSourceFacts(source, response.SourceFacts, pointer+"/response_variants"); err != nil {
				return err
			}
			if response.NoContent && (response.Schema != nil || response.MediaType != noContentMediaType) {
				return sourceError(ErrSourceFidelity, "", pointer+"/response_variants", "no-content response carries a fabricated schema or media type")
			}
			if !response.NoContent && response.Schema == nil && len(response.BlockedDiagnosticIDs) == 0 {
				return sourceError(ErrSourceFidelity, "", pointer+"/response_variants", "response schema disappeared without an exact diagnostic")
			}
		}
		for _, diagnosticID := range operation.BlockedDiagnosticIDs {
			if _, ok := diagnosticIDs[diagnosticID]; !ok {
				return sourceError(ErrInvalidSource, "", pointer, "operation refers to an unknown diagnostic")
			}
		}
		selectedNames := slices.Clone(baseHTTPRequirementNames[:])
		if operationUsesTypedValues(*operation) {
			selectedNames = append(selectedNames, "value.serialize")
		}
		if operation.Semantics != nil && operation.Semantics.Pagination != nil {
			selectedNames = append(selectedNames, "http.paginate")
		}
		if effectiveHTTP := effectiveOperationHTTP(*operation); effectiveHTTP != nil && effectiveHTTP.Authorization != nil {
			selectedNames = append(selectedNames, "credentials.resolve")
		}
		slices.Sort(selectedNames)
		operation.CapabilityRequirements = make([]contract.CapabilityRequirement, 0, len(selectedNames))
		for _, name := range selectedNames {
			declaration, ok := declarations[name]
			if !ok {
				return sourceError(ErrUnsupportedShape, "", pointer+"/capability_requirements", "normalized behavior has no accepted versioned capability declaration")
			}
			operation.CapabilityRequirements = append(operation.CapabilityRequirements, declaration)
		}
		foundationSet := make(map[string]contract.FoundationRef, len(operation.CapabilityRequirements))
		for _, requirement := range operation.CapabilityRequirements {
			key := requirement.Foundation.ID + "\x00" + requirement.Foundation.Version
			foundationSet[key] = requirement.Foundation
		}
		foundationKeys := make([]string, 0, len(foundationSet))
		for key := range foundationSet {
			foundationKeys = append(foundationKeys, key)
		}
		slices.Sort(foundationKeys)
		foundations := make([]contract.FoundationRef, 0, len(foundationKeys))

		for _, key := range foundationKeys {
			foundations = append(foundations, foundationSet[key])
		}
		if err := contract.CheckFoundationCompatibility(foundations, targetFoundations); err != nil {
			return sourceError(ErrUnsupportedShape, "", pointer+"/capability_requirements", err.Error())
		}
	}
	return nil
}
func operationUsesTypedValues(operation NormalizedOperation) bool {
	effectiveHTTP := effectiveOperationHTTP(operation)
	for _, binding := range operation.CommonBindings {
		if operation.HTTP != nil && operation.HTTP.Authorization != nil &&
			effectiveHTTP != nil && effectiveHTTP.Authorization == nil &&
			binding.FieldID == operation.HTTP.Authorization.FieldID {
			continue
		}
		if binding.Normalized != nil {
			return true
		}
	}
	for _, variant := range operation.RequestVariants {
		if variant.MediaType != parameterOnlyMediaType && variant.Schema != nil {
			return true
		}
	}
	for _, response := range operation.ResponseVariants {
		if !response.NoContent && response.Schema != nil {
			return true
		}
	}
	return false
}
func paginationRequestFields(pagination Pagination) []string {
	switch pagination.Kind {
	case string(contract.OperationPaginationCursor):
		return []string{pagination.CursorRequestField}
	case string(contract.OperationPaginationOffset):
		return []string{pagination.OffsetRequestField, pagination.LimitRequestField}
	case string(contract.OperationPaginationPage):
		return []string{pagination.PageRequestField}
	case string(contract.OperationPaginationLink):
		if pagination.LimitRequestField == "" {
			return []string{pagination.LinkRequestField}
		}
		return []string{pagination.LinkRequestField, pagination.LimitRequestField}
	default:
		return nil
	}
}

func effectiveOperationHTTP(operation NormalizedOperation) *HTTPSemantics {
	if operation.Semantics != nil && operation.Semantics.HTTP != nil {
		return operation.Semantics.HTTP
	}
	return operation.HTTP
}

func indexedHTTPDeclarations() (map[string]contract.CapabilityRequirement, error) {
	declared := contract.HTTPExecutionCapabilities()
	result := make(map[string]contract.CapabilityRequirement, len(declared))
	for _, requirement := range declared {
		if err := requirement.Validate(); err != nil {
			return nil, sourceError(ErrUnsupportedShape, "", "/capability_requirements", err.Error())
		}
		if _, duplicate := result[requirement.Capability]; duplicate {
			return nil, sourceError(ErrUnsupportedShape, "", "/capability_requirements", "shared HTTP capability declaration is duplicated")
		}
		result[requirement.Capability] = requirement
	}
	for _, required := range []string{
		"http.execute", "value.serialize", "effect.authorize", "effect.limit", "http.paginate", "credentials.resolve",
	} {
		if _, ok := result[required]; !ok {
			return nil, sourceError(ErrUnsupportedShape, "", "/capability_requirements", "shared HTTP capability declaration is incomplete")
		}
	}
	return result, nil
}

func checkLaneInventory(inventory []LaneDisposition, pointer string) error {
	if len(inventory) != len(allLanes) {
		return sourceError(ErrSourceFidelity, "", pointer, "every operation must appear in exactly seven lane inventories")
	}
	seen := make(map[contract.Lane]struct{}, len(inventory))
	for _, disposition := range inventory {
		if err := contract.ValidateLane(disposition.Lane); err != nil {
			return sourceError(ErrInvalidSource, "", pointer, "lane identity is unsupported")
		}
		if _, duplicate := seen[disposition.Lane]; duplicate {
			return sourceError(ErrSourceFidelity, "", pointer, "lane inventory contains a duplicate cell")
		}
		seen[disposition.Lane] = struct{}{}
		if !validLaneStatus(disposition.Status) || disposition.Reason == "" || len(disposition.SourceRefs) == 0 {
			return sourceError(ErrSourceFidelity, "", pointer, "lane disposition, reason and source references must be explicit")
		}
	}
	for _, lane := range allLanes {
		if _, ok := seen[lane]; !ok {
			return sourceError(ErrSourceFidelity, "", pointer, "lane inventory is incomplete")
		}
	}
	return nil
}

func checkBindingFact(source *ResolvedSource, binding BindingFact, diagnostics map[string]struct{}, fieldIDs map[string]struct{}, pointer string) error {
	if !validIdentity(binding.FieldID) || len(binding.SourceFacts) == 0 {
		return sourceError(ErrSourceFidelity, "", pointer, "binding identity and source facts are required")
	}
	if err := checkSourceFacts(source, binding.SourceFacts, pointer); err != nil {
		return err
	}
	if _, duplicate := fieldIDs[binding.FieldID]; duplicate {
		return sourceError(ErrSourceFidelity, "", pointer, "field identity collides across common or request-variant bindings")
	}
	fieldIDs[binding.FieldID] = struct{}{}
	if binding.Sensitivity == SensitivityUnknown && binding.Normalized != nil {
		return sourceError(ErrSourceFidelity, "", pointer, "unknown sensitivity entered an admitted field binding")
	}
	if binding.Normalized == nil && binding.BlockedDiagnosticID == "" {
		return sourceError(ErrSourceFidelity, "", pointer, "binding disappeared without an exact diagnostic")
	}
	if binding.BlockedDiagnosticID != "" {
		if _, ok := diagnostics[binding.BlockedDiagnosticID]; !ok {
			return sourceError(ErrInvalidSource, "", pointer, "binding refers to an unknown diagnostic")
		}
	}
	return nil
}

func checkSourceFacts(source *ResolvedSource, facts []SourceFact, pointer string) error {
	for _, fact := range facts {
		if !validDigest(fact.SHA256) {
			return sourceError(ErrSourceFidelity, fact.SourceRef.SourceID, fact.SourceRef.Pointer, "source fact has no exact captured hash")
		}
		actual, ok := source.PointerHash(fact.SourceRef.SourceID, strings.TrimPrefix(fact.SourceRef.Pointer, "#"))
		if !ok || actual != fact.SHA256 {
			return sourceError(ErrSourceFidelity, fact.SourceRef.SourceID, fact.SourceRef.Pointer, "source fact hash disagrees with captured bytes at "+pointer)
		}
	}
	return nil
}
