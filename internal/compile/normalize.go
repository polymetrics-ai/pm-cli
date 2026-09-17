package compile

import (
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const noContentMediaType = "application/x.pm.no-content"
const parameterOnlyMediaType = "application/x.pm.parameters"

type discoveredOperation struct {
	sourceID             string
	pointer              string
	path                 string
	method               string
	pathItem             *openapi3.PathItem
	operation            *openapi3.Operation
	document             *openapi3.T
	unsupportedTransport bool
}

type parameterEntry struct {
	parameter *openapi3.Parameter
	pointer   string
}

type bodyProperty struct {
	name      string
	reference *openapi3.SchemaRef
	pointer   string
	required  bool
}

func Normalize(ctx context.Context, source *LoadedSource) (*Normalization, error) {
	if source == nil || source.Resolved == nil || source.Resolved.Captured == nil {
		return nil, sourceError(ErrInvalidSource, "", "", "loaded source is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	discovered, err := discoverOperations(source)
	if err != nil {
		return nil, err
	}
	locked, err := matchLockedOperations(source.Resolved.Captured.Lock.Operations, discovered)
	if err != nil {
		return nil, err
	}

	normalization := &Normalization{
		ConnectorID:       source.Resolved.Captured.Lock.ConnectorID,
		SemanticsRevision: source.Resolved.Captured.Lock.SemanticsRevision,
		SourceDigest:      source.Resolved.Captured.Digest,
	}
	for _, operation := range source.Resolved.Captured.Lock.Operations {
		normalization.SourceOperationIDs = append(normalization.SourceOperationIDs, operation.SourceOperationID)
	}
	slices.Sort(normalization.SourceOperationIDs)
	for _, document := range source.Documents {
		normalization.Conversions = append(normalization.Conversions, document.Provenance)
	}
	slices.SortFunc(normalization.Conversions, func(left, right ConversionProvenance) int {
		return strings.Compare(left.SourceID, right.SourceID)
	})

	for _, match := range locked {
		operation, diagnostics, err := normalizeOperation(ctx, source, match.lock, match.discovered)
		if err != nil {
			return nil, err
		}
		normalization.Operations = append(normalization.Operations, operation)
		normalization.Diagnostics = append(normalization.Diagnostics, diagnostics...)
	}
	slices.SortFunc(normalization.Operations, func(left, right NormalizedOperation) int {
		return strings.Compare(left.OperationID, right.OperationID)
	})
	sortDiagnostics(normalization.Diagnostics)
	return normalization, nil
}

type lockedOperation struct {
	lock       SourceOperation
	discovered discoveredOperation
}

func discoverOperations(source *LoadedSource) (map[string]discoveredOperation, error) {
	result := make(map[string]discoveredOperation)
	for _, loaded := range source.Documents {
		document := loaded.Document
		if document.Paths != nil {
			paths := document.Paths.Map()
			pathNames := make([]string, 0, len(paths))
			for path := range paths {
				pathNames = append(pathNames, path)
			}
			slices.Sort(pathNames)
			for _, path := range pathNames {
				if err := discoverPathItem(result, loaded.SourceID, path, "/paths/"+escapePointerToken(path), paths[path], document, false); err != nil {
					return nil, err
				}
			}
		}
		webhookNames := make([]string, 0, len(document.Webhooks))
		for name := range document.Webhooks {
			webhookNames = append(webhookNames, name)
		}
		slices.Sort(webhookNames)
		for _, name := range webhookNames {
			if err := discoverPathItem(result, loaded.SourceID, name, "/webhooks/"+escapePointerToken(name), document.Webhooks[name], document, true); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}

func discoverPathItem(
	result map[string]discoveredOperation,
	sourceID string,
	path string,
	pointer string,
	pathItem *openapi3.PathItem,
	document *openapi3.T,
	unsupportedTransport bool,
) error {
	if pathItem == nil {
		return sourceError(ErrInvalidSource, sourceID, pointer, "path item is null")
	}
	operations := pathItem.Operations()
	methods := make([]string, 0, len(operations))
	for method := range operations {
		methods = append(methods, method)
	}
	slices.Sort(methods)
	for _, method := range methods {
		operation := operations[method]
		operationPointer := pointer + "/" + strings.ToLower(method)
		if operation == nil {
			return sourceError(ErrInvalidSource, sourceID, operationPointer, "operation is null")
		}
		key := operationKey(sourceID, operationPointer)
		if _, duplicate := result[key]; duplicate {
			return sourceError(ErrInvalidSource, sourceID, operationPointer, "operation pointer is duplicated")
		}
		if len(operation.Callbacks) != 0 {
			return sourceError(ErrUnsupportedShape, sourceID, operationPointer+"/callbacks", "callback operations require a separate admitted transport shape and are not silently discarded")
		}
		result[key] = discoveredOperation{
			sourceID:             sourceID,
			pointer:              operationPointer,
			path:                 path,
			method:               strings.ToUpper(method),
			pathItem:             pathItem,
			operation:            operation,
			document:             document,
			unsupportedTransport: unsupportedTransport || !supportedHTTPMethod(strings.ToUpper(method)),
		}
	}
	return nil
}

func matchLockedOperations(locks []SourceOperation, discovered map[string]discoveredOperation) ([]lockedOperation, error) {
	matches := make([]lockedOperation, 0, len(locks))
	used := make(map[string]struct{}, len(locks))
	for _, locked := range locks {
		var selected discoveredOperation
		selectedKey := ""
		for _, reference := range locked.SourceRefs {
			pointer := strings.TrimPrefix(reference.Pointer, "#")
			key := operationKey(reference.SourceID, pointer)
			candidate, ok := discovered[key]
			if !ok {
				continue
			}
			if selectedKey != "" && selectedKey != key {
				return nil, sourceError(ErrSourceFidelity, reference.SourceID, pointer, "one source operation record addresses multiple operations")
			}
			selected = candidate
			selectedKey = key
		}
		if selectedKey == "" {
			return nil, sourceError(ErrSourceFidelity, "", "", "source operation does not resolve to an operation in the captured document")
		}
		if _, duplicate := used[selectedKey]; duplicate {
			return nil, sourceError(ErrSourceFidelity, selected.sourceID, selected.pointer, "captured operation is listed more than once")
		}
		used[selectedKey] = struct{}{}
		if selected.operation.OperationID != "" && selected.operation.OperationID != locked.SourceOperationID {
			return nil, sourceError(ErrSourceFidelity, selected.sourceID, selected.pointer+"/operationId", "source operation identity disagrees with the source lock")
		}
		matches = append(matches, lockedOperation{lock: locked, discovered: selected})
	}
	if len(used) != len(discovered) {
		unlisted := make([]string, 0, len(discovered)-len(used))
		for key := range discovered {
			if _, ok := used[key]; !ok {
				unlisted = append(unlisted, key)
			}
		}
		slices.Sort(unlisted)
		return nil, sourceError(ErrSourceFidelity, "", "", "source lock omits captured operations: "+strings.Join(unlisted, ", "))
	}
	slices.SortFunc(matches, func(left, right lockedOperation) int {
		return strings.Compare(left.lock.SourceOperationID, right.lock.SourceOperationID)
	})
	return matches, nil
}

func normalizeOperation(
	_ context.Context,
	source *LoadedSource,
	locked SourceOperation,
	discovered discoveredOperation,
) (NormalizedOperation, []Diagnostic, error) {
	operation := NormalizedOperation{
		OperationID:   locked.SourceOperationID,
		SourceRefs:    slices.Clone(locked.SourceRefs),
		Transport:     contract.Transport{Kind: contract.TransportKindHTTP, Method: discovered.method, Path: discovered.path},
		LaneInventory: defaultLaneInventory(locked.SourceRefs),
		Raw: RawOperationFact{
			SourceOperationID: locked.SourceOperationID,
			SourceFacts:       sourceFacts(source.Resolved, locked.SourceRefs),
			Method:            discovered.method,
			Path:              discovered.path,
			RequestVariants:   slices.Clone(locked.RequestVariants),
			ResponseVariants:  slices.Clone(locked.ResponseVariants),
		},
	}
	var diagnostics []Diagnostic
	if discovered.unsupportedTransport {
		diagnostic := newDiagnostic(
			"unsupported_http_transport",
			SeverityError,
			operation.OperationID,
			"",
			operation.Raw.SourceFacts,
			"/operations/"+escapePointerToken(operation.OperationID)+"/transport",
			operation.Transport,
			allLaneSlice(),
			"source operation uses an HTTP method or webhook path not represented by the runtime transport contract",
		)
		diagnostics = append(diagnostics, diagnostic)
		operation.BlockedDiagnosticIDs = append(operation.BlockedDiagnosticIDs, diagnostic.ID)
	}
	authenticationRequired, authenticationFacts, authenticationErr := operationAuthentication(discovered, source.Resolved)
	if authenticationErr != nil {
		diagnostic := newDiagnostic(
			"unsupported_authentication",
			SeverityError,
			operation.OperationID,
			"",
			operation.Raw.SourceFacts,
			"/operations/"+escapePointerToken(operation.OperationID)+"/authentication",
			nil,
			allLaneSlice(),
			authenticationErr.Error(),
		)
		diagnostics = append(diagnostics, diagnostic)
		operation.BlockedDiagnosticIDs = append(operation.BlockedDiagnosticIDs, diagnostic.ID)
	} else {
		operation.AuthenticationRequired = authenticationRequired
		operation.AuthenticationFacts = authenticationFacts
	}

	policy := buildSensitivityPolicy(discovered.document)
	parameters, err := mergedParameters(discovered)
	if err != nil {
		return NormalizedOperation{}, nil, err
	}
	for _, parameter := range parameters {
		binding, bindingDiagnostics := normalizeParameterBinding(source.Resolved, operation.OperationID, discovered.sourceID, parameter, policy)
		diagnostics = append(diagnostics, bindingDiagnostics...)
		operation.CommonBindings = append(operation.CommonBindings, binding)
		if binding.BlockedDiagnosticID != "" {
			operation.BlockedDiagnosticIDs = append(operation.BlockedDiagnosticIDs, binding.BlockedDiagnosticID)
		}
	}
	slices.SortFunc(operation.CommonBindings, func(left, right BindingFact) int {
		return strings.Compare(left.FieldID, right.FieldID)
	})

	requestVariants, requestDiagnostics, err := normalizeRequestVariants(source.Resolved, operation.OperationID, discovered, locked.RequestVariants, policy)
	if err != nil {
		return NormalizedOperation{}, nil, err
	}
	operation.RequestVariants = requestVariants
	diagnostics = append(diagnostics, requestDiagnostics...)
	for _, variant := range requestVariants {
		operation.BlockedDiagnosticIDs = append(operation.BlockedDiagnosticIDs, variant.BlockedDiagnosticIDs...)
	}

	responseVariants, responseDiagnostics, err := normalizeResponseVariants(source.Resolved, operation.OperationID, discovered, locked.ResponseVariants)
	if err != nil {
		return NormalizedOperation{}, nil, err
	}
	operation.ResponseVariants = responseVariants
	diagnostics = append(diagnostics, responseDiagnostics...)
	for _, variant := range responseVariants {
		operation.BlockedDiagnosticIDs = append(operation.BlockedDiagnosticIDs, variant.BlockedDiagnosticIDs...)
	}
	operation.BlockedDiagnosticIDs = sortedUniqueStrings(operation.BlockedDiagnosticIDs)
	return operation, diagnostics, nil
}

func mergedParameters(discovered discoveredOperation) ([]parameterEntry, error) {
	entries := make([]parameterEntry, 0, len(discovered.pathItem.Parameters)+len(discovered.operation.Parameters))
	positions := make(map[string]int)
	appendParameters := func(parameters openapi3.Parameters, basePointer string, override bool) error {
		for index, reference := range parameters {
			pointer := basePointer + "/" + strconv.Itoa(index)
			if reference == nil || reference.Value == nil {
				return sourceError(ErrUnsupportedShape, discovered.sourceID, pointer, "parameter reference has no captured value")
			}
			parameter := reference.Value
			key := parameter.In + "\x00" + parameter.Name
			if prior, exists := positions[key]; exists {
				if !override {
					return sourceError(ErrInvalidSource, discovered.sourceID, pointer, "parameter wire identity is duplicated")
				}
				entries[prior] = parameterEntry{parameter: parameter, pointer: pointer}
				continue
			}
			positions[key] = len(entries)
			entries = append(entries, parameterEntry{parameter: parameter, pointer: pointer})
		}
		return nil
	}
	pathPointer := "/paths/" + escapePointerToken(discovered.path) + "/parameters"
	if err := appendParameters(discovered.pathItem.Parameters, pathPointer, false); err != nil {
		return nil, err
	}
	if err := appendParameters(discovered.operation.Parameters, discovered.pointer+"/parameters", true); err != nil {
		return nil, err
	}
	return entries, nil
}

func normalizeParameterBinding(
	source *ResolvedSource,
	operationID string,
	sourceID string,
	entry parameterEntry,
	policy sensitivityPolicy,
) (BindingFact, []Diagnostic) {
	parameter := entry.parameter
	fieldID := parameterFieldID(parameter)
	facts := sourceFacts(source, []contract.SourceRef{{SourceID: sourceID, Pointer: "#" + entry.pointer}})
	style, explode := parameterSerialization(parameter)
	bindingFact := BindingFact{
		FieldID:          fieldID,
		SourceFacts:      facts,
		RawRequired:      parameter.Required,
		RawStyle:         style,
		RawExplode:       explode,
		RawAllowReserved: parameter.AllowReserved,
	}
	var diagnostics []Diagnostic
	if parameter.Content != nil || parameter.Schema == nil {
		diagnostic := newDiagnostic(
			"unsupported_parameter_shape",
			SeverityError,
			operationID,
			fieldID,
			facts,
			"/operations/"+escapePointerToken(operationID)+"/common_bindings/"+escapePointerToken(fieldID),
			nil,
			allLaneSlice(),
			"parameter content encoding or missing schema is unsupported",
		)
		bindingFact.Sensitivity = SensitivityUnknown
		bindingFact.BlockedDiagnosticID = diagnostic.ID
		return bindingFact, []Diagnostic{diagnostic}
	}

	normalizedPointer := "/operations/" + escapePointerToken(operationID) + "/common_bindings/" + escapePointerToken(fieldID)
	lowered := lowerSchema(parameter.Schema, schemaContext{
		source:            source,
		sourceID:          sourceID,
		pointer:           entry.pointer + "/schema",
		operationID:       operationID,
		fieldID:           fieldID,
		normalizedPointer: normalizedPointer + "/schema",
		bindingDefault:    true,
	})
	bindingFact.SchemaFacts = lowered.Facts
	diagnostics = append(diagnostics, lowered.Diagnostics...)
	for _, diagnostic := range lowered.Diagnostics {
		if diagnostic.Severity == SeverityError && bindingFact.BlockedDiagnosticID == "" {
			bindingFact.BlockedDiagnosticID = diagnostic.ID
		}
	}
	if lowered.Schema == nil {
		bindingFact.Sensitivity = SensitivityUnknown
		return bindingFact, diagnostics
	}
	bindingFact.RawNullable = lowered.Schema.Nullable
	classification := classifyParameterSensitivity(parameter, parameter.Schema.Value, policy)
	bindingFact.Sensitivity = classification
	if classification == SensitivityUnknown {
		diagnostic := newDiagnostic(
			"unknown_sensitivity",
			SeverityError,
			operationID,
			fieldID,
			facts,
			normalizedPointer,
			nil,
			allLaneSlice(),
			"field sensitivity is unknown and cannot enter an admitted operation",
		)
		diagnostics = append(diagnostics, diagnostic)
		bindingFact.BlockedDiagnosticID = diagnostic.ID
		return bindingFact, diagnostics
	}

	location := contract.FieldLocation(parameter.In)
	binding := contract.FieldBinding{
		FieldID:       fieldID,
		WireName:      parameter.Name,
		Location:      location,
		JSONPointer:   "",
		Required:      parameter.Required,
		Nullable:      lowered.Schema.Nullable,
		Style:         style,
		Explode:       explode,
		AllowReserved: parameter.AllowReserved,
		Sensitivity:   contract.Sensitivity(classification),
		Schema:        *lowered.Schema,
	}
	if rawDefault, ok := resolvedSchemaDefault(source, sourceID, entry.pointer+"/schema"); ok {
		value, err := presentValueFromRaw(rawDefault, binding.Schema)
		if err != nil {
			diagnostic := newDiagnostic(
				"invalid_source_default",
				SeverityError,
				operationID,
				fieldID,
				facts,
				normalizedPointer+"/source_default",
				nil,
				allLaneSlice(),
				err.Error(),
			)
			diagnostics = append(diagnostics, diagnostic)
			bindingFact.BlockedDiagnosticID = diagnostic.ID
		} else {
			binding.SourceDefault = &value
		}
	}
	if err := binding.Validate(); err != nil {
		diagnostic := newDiagnostic(
			"invalid_normalized_binding",
			SeverityError,
			operationID,
			fieldID,
			facts,
			normalizedPointer,
			binding,
			allLaneSlice(),
			err.Error(),
		)
		diagnostics = append(diagnostics, diagnostic)
		bindingFact.BlockedDiagnosticID = diagnostic.ID
		return bindingFact, diagnostics
	}
	bindingFact.Normalized = &binding
	bindingFact.NormalizedSHA256 = hashJSON(binding)
	if fidelity := CheckBindingFidelity(BindingExpectation{
		OperationID:       operationID,
		FieldID:           fieldID,
		SourceFacts:       facts,
		Required:          parameter.Required,
		Nullable:          bindingFact.RawNullable,
		Style:             style,
		Explode:           explode,
		AllowReserved:     parameter.AllowReserved,
		NormalizedPointer: normalizedPointer,
		AffectedCells:     allLaneSlice(),
	}, binding); fidelity != nil {
		diagnostics = append(diagnostics, *fidelity)
		bindingFact.BlockedDiagnosticID = fidelity.ID
		bindingFact.Normalized = nil
		bindingFact.NormalizedSHA256 = ""
	}
	return bindingFact, diagnostics
}

func normalizeRequestVariants(
	source *ResolvedSource,
	operationID string,
	discovered discoveredOperation,
	locked []VariantRef,
	policy sensitivityPolicy,
) ([]RequestVariant, []Diagnostic, error) {
	actual := make(map[string]*openapi3.MediaType)
	bodyRequired := false
	if discovered.operation.RequestBody == nil {
		actual[parameterOnlyMediaType] = nil
	} else {
		if discovered.operation.RequestBody.Value == nil {
			return nil, nil, sourceError(ErrUnsupportedShape, discovered.sourceID, discovered.pointer+"/requestBody", "request body reference has no captured value")
		}
		bodyRequired = discovered.operation.RequestBody.Value.Required
		for mediaType, media := range discovered.operation.RequestBody.Value.Content {
			actual[mediaType] = media
		}
		if len(actual) == 0 {
			return nil, nil, sourceError(ErrInvalidSource, discovered.sourceID, discovered.pointer+"/requestBody/content", "request body content is empty")
		}
	}
	if err := compareRequestVariantInventory(locked, actual); err != nil {
		return nil, nil, sourceError(ErrSourceFidelity, discovered.sourceID, discovered.pointer+"/requestBody", err.Error())
	}

	lockedByMedia := make(map[string]VariantRef, len(locked))
	for _, variant := range locked {
		lockedByMedia[variant.MediaType] = variant
	}
	mediaTypes := make([]string, 0, len(actual))
	for mediaType := range actual {
		mediaTypes = append(mediaTypes, mediaType)
	}
	slices.Sort(mediaTypes)
	variants := make([]RequestVariant, 0, len(mediaTypes))
	var diagnostics []Diagnostic
	for _, mediaType := range mediaTypes {
		lockedVariant := lockedByMedia[mediaType]
		variantID := requestVariantID(mediaType)
		variant := RequestVariant{
			VariantID:    variantID,
			MediaType:    mediaType,
			SourceFacts:  sourceFacts(source, []contract.SourceRef{{SourceID: discovered.sourceID, Pointer: lockedVariant.Pointer}}),
			BodyRequired: bodyRequired,
		}
		media := actual[mediaType]
		if media == nil {
			variants = append(variants, variant)
			continue
		}
		if media.Schema == nil || media.Schema.Value == nil {
			diagnostic := newDiagnostic(
				"missing_request_schema",
				SeverityError,
				operationID,
				"",
				variant.SourceFacts,
				"/operations/"+escapePointerToken(operationID)+"/request_variants/"+escapePointerToken(variantID),
				nil,
				allLaneSlice(),
				"request media variant has no captured schema",
			)
			variant.BlockedDiagnosticIDs = append(variant.BlockedDiagnosticIDs, diagnostic.ID)
			diagnostics = append(diagnostics, diagnostic)
			variants = append(variants, variant)
			continue
		}
		schemaPointer := strings.TrimPrefix(lockedVariant.Pointer, "#") + "/schema"
		properties, complete := collectBodyProperties(media.Schema, schemaPointer, make(map[*openapi3.Schema]struct{}))
		usePropertyBindings := complete && len(properties) != 0 && (!bodyRequired || hasRequiredBodyProperty(properties))
		lowered := lowerSchema(media.Schema, schemaContext{
			source:            source,
			sourceID:          discovered.sourceID,
			pointer:           schemaPointer,
			operationID:       operationID,
			normalizedPointer: "/operations/" + escapePointerToken(operationID) + "/request_variants/" + escapePointerToken(variantID) + "/schema",
			bindingDefault:    !usePropertyBindings,
			propertyBindings:  usePropertyBindings,
		})
		variant.Schema = lowered.Schema
		variant.SchemaFacts = lowered.Facts
		if lowered.Schema != nil {
			variant.NormalizedSHA256 = hashJSON(*lowered.Schema)
		}
		for _, diagnostic := range lowered.Diagnostics {
			diagnostics = append(diagnostics, diagnostic)
			if diagnostic.Severity == SeverityError {
				variant.BlockedDiagnosticIDs = append(variant.BlockedDiagnosticIDs, diagnostic.ID)
			}
		}
		if bodyStyle(mediaType, media.Schema.Value) == "" {
			diagnostic := newDiagnostic(
				"unsupported_request_serialization",
				SeverityError,
				operationID,
				"",
				variant.SourceFacts,
				"/operations/"+escapePointerToken(operationID)+"/request_variants/"+escapePointerToken(variantID),
				nil,
				allLaneSlice(),
				"request media type cannot be represented by the admitted JSON or binary body serializers",
			)
			diagnostics = append(diagnostics, diagnostic)
			variant.BlockedDiagnosticIDs = append(variant.BlockedDiagnosticIDs, diagnostic.ID)
			variant.BlockedDiagnosticIDs = sortedUniqueStrings(variant.BlockedDiagnosticIDs)
			variants = append(variants, variant)
			continue
		}

		if usePropertyBindings {
			for _, property := range properties {
				binding, bindingDiagnostics := normalizeBodyProperty(
					source,
					operationID,
					discovered.sourceID,
					variantID,
					mediaType,
					property,
					policy,
				)
				variant.Bindings = append(variant.Bindings, binding)
				diagnostics = append(diagnostics, bindingDiagnostics...)
				if binding.BlockedDiagnosticID != "" {
					variant.BlockedDiagnosticIDs = append(variant.BlockedDiagnosticIDs, binding.BlockedDiagnosticID)
				}
			}
		} else {
			binding, bindingDiagnostics := normalizeWholeBody(
				source,
				operationID,
				discovered.sourceID,
				variantID,
				mediaType,
				lockedVariant.Pointer,
				bodyRequired,
				media.Schema,
				policy,
			)
			variant.Bindings = append(variant.Bindings, binding)
			diagnostics = append(diagnostics, bindingDiagnostics...)
			if binding.BlockedDiagnosticID != "" {
				variant.BlockedDiagnosticIDs = append(variant.BlockedDiagnosticIDs, binding.BlockedDiagnosticID)
			}
		}
		slices.SortFunc(variant.Bindings, func(left, right BindingFact) int { return strings.Compare(left.FieldID, right.FieldID) })
		variant.BlockedDiagnosticIDs = sortedUniqueStrings(variant.BlockedDiagnosticIDs)
		variants = append(variants, variant)
	}
	return variants, diagnostics, nil
}

func normalizeBodyProperty(
	source *ResolvedSource,
	operationID string,
	sourceID string,
	variantID string,
	mediaType string,
	property bodyProperty,
	_ sensitivityPolicy,
) (BindingFact, []Diagnostic) {
	fieldID := variantID + ".body." + stableFieldSegment(property.name)
	facts := sourceFacts(source, []contract.SourceRef{{SourceID: sourceID, Pointer: "#" + property.pointer}})
	normalizedPointer := "/operations/" + escapePointerToken(operationID) + "/request_variants/" + escapePointerToken(variantID) + "/bindings/" + escapePointerToken(fieldID)
	lowered := lowerSchema(property.reference, schemaContext{
		source:            source,
		sourceID:          sourceID,
		pointer:           property.pointer,
		operationID:       operationID,
		fieldID:           fieldID,
		normalizedPointer: normalizedPointer + "/schema",
		bindingDefault:    true,
	})
	fact := BindingFact{
		FieldID:     fieldID,
		SourceFacts: facts,
		RawRequired: property.required,
		RawStyle:    bodyStyle(mediaType, property.reference.Value),
		RawExplode:  false,
		SchemaFacts: lowered.Facts,
	}
	var diagnostics []Diagnostic
	diagnostics = append(diagnostics, lowered.Diagnostics...)
	for _, diagnostic := range lowered.Diagnostics {
		if diagnostic.Severity == SeverityError && fact.BlockedDiagnosticID == "" {
			fact.BlockedDiagnosticID = diagnostic.ID
		}
	}
	if lowered.Schema == nil {
		fact.Sensitivity = SensitivityUnknown
		return fact, diagnostics
	}
	fact.RawNullable = lowered.Schema.Nullable
	fact.Sensitivity = classifyPropertySensitivity(property.name, property.reference.Value)
	if fact.Sensitivity == SensitivityUnknown {
		diagnostic := newDiagnostic(
			"unknown_sensitivity",
			SeverityError,
			operationID,
			fieldID,
			facts,
			normalizedPointer,
			nil,
			allLaneSlice(),
			"body field sensitivity is unknown and cannot enter an admitted operation",
		)
		diagnostics = append(diagnostics, diagnostic)
		fact.BlockedDiagnosticID = diagnostic.ID
		return fact, diagnostics
	}
	binding := contract.FieldBinding{
		FieldID:       fieldID,
		WireName:      property.name,
		Location:      contract.FieldLocationBody,
		JSONPointer:   "/" + escapePointerToken(property.name),
		Required:      property.required,
		Nullable:      lowered.Schema.Nullable,
		Style:         fact.RawStyle,
		Explode:       false,
		AllowReserved: false,
		Sensitivity:   contract.Sensitivity(fact.Sensitivity),
		Schema:        *lowered.Schema,
	}
	if rawDefault, ok := resolvedSchemaDefault(source, sourceID, property.pointer); ok {
		value, err := presentValueFromRaw(rawDefault, binding.Schema)
		if err != nil {
			diagnostic := newDiagnostic("invalid_source_default", SeverityError, operationID, fieldID, facts, normalizedPointer+"/source_default", nil, allLaneSlice(), err.Error())
			diagnostics = append(diagnostics, diagnostic)
			fact.BlockedDiagnosticID = diagnostic.ID
		} else {
			binding.SourceDefault = &value
		}
	}
	if err := binding.Validate(); err != nil {
		diagnostic := newDiagnostic("invalid_normalized_binding", SeverityError, operationID, fieldID, facts, normalizedPointer, binding, allLaneSlice(), err.Error())
		diagnostics = append(diagnostics, diagnostic)
		fact.BlockedDiagnosticID = diagnostic.ID
		return fact, diagnostics
	}
	if fidelity := CheckBindingFidelity(BindingExpectation{
		OperationID: operationID, FieldID: fieldID, SourceFacts: facts,
		Required: property.required, Nullable: fact.RawNullable, Style: fact.RawStyle,
		Explode: false, AllowReserved: false, NormalizedPointer: normalizedPointer,
		AffectedCells: allLaneSlice(),
	}, binding); fidelity != nil {
		diagnostics = append(diagnostics, *fidelity)
		fact.BlockedDiagnosticID = fidelity.ID
		return fact, diagnostics
	}
	fact.Normalized = &binding
	fact.NormalizedSHA256 = hashJSON(binding)
	return fact, diagnostics
}

func normalizeWholeBody(
	source *ResolvedSource,
	operationID string,
	sourceID string,
	variantID string,
	mediaType string,
	variantPointer string,
	required bool,
	reference *openapi3.SchemaRef,
	_ sensitivityPolicy,
) (BindingFact, []Diagnostic) {
	fieldID := variantID + ".body"
	schemaPointer := strings.TrimPrefix(variantPointer, "#") + "/schema"
	facts := sourceFacts(source, []contract.SourceRef{{SourceID: sourceID, Pointer: "#" + schemaPointer}})
	normalizedPointer := "/operations/" + escapePointerToken(operationID) + "/request_variants/" + escapePointerToken(variantID) + "/bindings/" + escapePointerToken(fieldID)
	lowered := lowerSchema(reference, schemaContext{
		source: source, sourceID: sourceID, pointer: schemaPointer,
		operationID: operationID, fieldID: fieldID, normalizedPointer: normalizedPointer + "/schema",
		bindingDefault: true,
	})
	fact := BindingFact{
		FieldID:     fieldID,
		SourceFacts: facts,
		RawRequired: required,
		RawStyle:    bodyStyle(mediaType, reference.Value),
		SchemaFacts: lowered.Facts,
	}
	var diagnostics []Diagnostic
	diagnostics = append(diagnostics, lowered.Diagnostics...)
	for _, diagnostic := range lowered.Diagnostics {
		if diagnostic.Severity == SeverityError && fact.BlockedDiagnosticID == "" {
			fact.BlockedDiagnosticID = diagnostic.ID
		}
	}
	if lowered.Schema == nil {
		fact.Sensitivity = SensitivityUnknown
		return fact, diagnostics
	}
	fact.RawNullable = lowered.Schema.Nullable
	fact.Sensitivity = schemaSensitivity(reference.Value, make(map[*openapi3.Schema]struct{}))
	if fact.Sensitivity == SensitivityUnknown {
		diagnostic := newDiagnostic("unknown_sensitivity", SeverityError, operationID, fieldID, facts, normalizedPointer, nil, allLaneSlice(), "body sensitivity is unknown and cannot enter an admitted operation")
		diagnostics = append(diagnostics, diagnostic)
		fact.BlockedDiagnosticID = diagnostic.ID
		return fact, diagnostics
	}
	binding := contract.FieldBinding{
		FieldID: fieldID, WireName: "", Location: contract.FieldLocationBody, JSONPointer: "",
		Required: required, Nullable: lowered.Schema.Nullable, Style: fact.RawStyle,
		Explode: false, AllowReserved: false, Sensitivity: contract.Sensitivity(fact.Sensitivity), Schema: *lowered.Schema,
	}
	if rawDefault, ok := resolvedSchemaDefault(source, sourceID, schemaPointer); ok {
		value, err := presentValueFromRaw(rawDefault, binding.Schema)
		if err != nil {
			diagnostic := newDiagnostic("invalid_source_default", SeverityError, operationID, fieldID, facts, normalizedPointer+"/source_default", nil, allLaneSlice(), err.Error())
			diagnostics = append(diagnostics, diagnostic)
			fact.BlockedDiagnosticID = diagnostic.ID
		} else {
			binding.SourceDefault = &value
		}
	}
	if err := binding.Validate(); err != nil {
		diagnostic := newDiagnostic("invalid_normalized_binding", SeverityError, operationID, fieldID, facts, normalizedPointer, binding, allLaneSlice(), err.Error())
		diagnostics = append(diagnostics, diagnostic)
		fact.BlockedDiagnosticID = diagnostic.ID
		return fact, diagnostics
	}
	if fidelity := CheckBindingFidelity(BindingExpectation{
		OperationID: operationID, FieldID: fieldID, SourceFacts: facts,
		Required: required, Nullable: fact.RawNullable, Style: fact.RawStyle,
		Explode: false, AllowReserved: false, NormalizedPointer: normalizedPointer,
		AffectedCells: allLaneSlice(),
	}, binding); fidelity != nil {
		diagnostics = append(diagnostics, *fidelity)
		fact.BlockedDiagnosticID = fidelity.ID
		return fact, diagnostics
	}
	fact.Normalized = &binding
	fact.NormalizedSHA256 = hashJSON(binding)
	return fact, diagnostics
}

type actualResponse struct {
	status    string
	mediaType string
	response  *openapi3.Response
	media     *openapi3.MediaType
}

func normalizeResponseVariants(
	source *ResolvedSource,
	operationID string,
	discovered discoveredOperation,
	locked []VariantRef,
) ([]ResponseVariant, []Diagnostic, error) {
	var actual []actualResponse
	if discovered.operation.Responses == nil {
		return nil, nil, sourceError(ErrInvalidSource, discovered.sourceID, discovered.pointer+"/responses", "responses are missing")
	}
	statuses := discovered.operation.Responses.Keys()
	slices.Sort(statuses)
	for _, status := range statuses {
		reference := discovered.operation.Responses.Value(status)
		if reference == nil || reference.Value == nil {
			return nil, nil, sourceError(ErrUnsupportedShape, discovered.sourceID, discovered.pointer+"/responses/"+escapePointerToken(status), "response reference has no captured value")
		}
		response := reference.Value
		if len(response.Content) == 0 {
			actual = append(actual, actualResponse{status: status, mediaType: noContentMediaType, response: response})
			continue
		}
		mediaTypes := make([]string, 0, len(response.Content))
		for mediaType := range response.Content {
			mediaTypes = append(mediaTypes, mediaType)
		}
		slices.Sort(mediaTypes)
		for _, mediaType := range mediaTypes {
			actual = append(actual, actualResponse{status: status, mediaType: mediaType, response: response, media: response.Content[mediaType]})
		}
	}
	if err := compareResponseVariantInventory(locked, actual); err != nil {
		return nil, nil, sourceError(ErrSourceFidelity, discovered.sourceID, discovered.pointer+"/responses", err.Error())
	}
	lockedByKey := make(map[string]VariantRef, len(locked))
	for _, variant := range locked {
		lockedByKey[variant.Status+"\x00"+variant.MediaType] = variant
	}
	variants := make([]ResponseVariant, 0, len(actual))
	var diagnostics []Diagnostic
	for _, item := range actual {
		lockedVariant := lockedByKey[item.status+"\x00"+item.mediaType]
		variant := ResponseVariant{
			Status:      item.status,
			MediaType:   item.mediaType,
			NoContent:   item.mediaType == noContentMediaType,
			SourceFacts: sourceFacts(source, []contract.SourceRef{{SourceID: discovered.sourceID, Pointer: lockedVariant.Pointer}}),
		}
		if len(item.response.Headers) != 0 || len(item.response.Links) != 0 {
			diagnostic := newDiagnostic(
				"unsupported_response_metadata",
				SeverityError,
				operationID,
				"",
				variant.SourceFacts,
				"/operations/"+escapePointerToken(operationID)+"/response_variants/"+escapePointerToken(item.status+"-"+item.mediaType),
				nil,
				allLaneSlice(),
				"response headers or links remain in raw facts but are not represented by the runtime response contract",
			)
			diagnostics = append(diagnostics, diagnostic)
			variant.BlockedDiagnosticIDs = append(variant.BlockedDiagnosticIDs, diagnostic.ID)
		}
		if variant.NoContent {
			variants = append(variants, variant)
			continue
		}
		if item.media == nil || item.media.Schema == nil || item.media.Schema.Value == nil {
			diagnostic := newDiagnostic("missing_response_schema", SeverityError, operationID, "", variant.SourceFacts, "/operations/"+escapePointerToken(operationID)+"/response_variants", nil, allLaneSlice(), "response media variant has no captured schema")
			diagnostics = append(diagnostics, diagnostic)
			variant.BlockedDiagnosticIDs = append(variant.BlockedDiagnosticIDs, diagnostic.ID)
			variants = append(variants, variant)
			continue
		}
		variant.Sensitivity = schemaSensitivity(item.media.Schema.Value, make(map[*openapi3.Schema]struct{}))
		schemaPointer := strings.TrimPrefix(lockedVariant.Pointer, "#") + "/schema"
		lowered := lowerSchema(item.media.Schema, schemaContext{
			source: source, sourceID: discovered.sourceID, pointer: schemaPointer,
			operationID: operationID, normalizedPointer: "/operations/" + escapePointerToken(operationID) + "/response_variants/" + escapePointerToken(item.status+"-"+item.mediaType) + "/schema",
		})
		variant.Schema = lowered.Schema
		variant.SchemaFacts = lowered.Facts
		if lowered.Schema != nil {
			variant.NormalizedSHA256 = hashJSON(*lowered.Schema)
		}
		for _, diagnostic := range lowered.Diagnostics {
			diagnostics = append(diagnostics, diagnostic)
			if diagnostic.Severity == SeverityError {
				variant.BlockedDiagnosticIDs = append(variant.BlockedDiagnosticIDs, diagnostic.ID)
			}
		}
		variant.BlockedDiagnosticIDs = sortedUniqueStrings(variant.BlockedDiagnosticIDs)
		variants = append(variants, variant)
	}
	return variants, diagnostics, nil
}

func compareRequestVariantInventory(locked []VariantRef, actual map[string]*openapi3.MediaType) error {
	if len(locked) != len(actual) {
		return fmt.Errorf("request variant count disagrees with captured source")
	}
	seen := make(map[string]struct{}, len(locked))
	for _, variant := range locked {
		if _, ok := actual[variant.MediaType]; !ok {
			return fmt.Errorf("request media variant %q is absent from captured source", variant.MediaType)
		}
		seen[variant.MediaType] = struct{}{}
	}
	if len(seen) != len(actual) {
		return fmt.Errorf("request variants are duplicated or incomplete")
	}
	return nil
}

func compareResponseVariantInventory(locked []VariantRef, actual []actualResponse) error {
	if len(locked) != len(actual) {
		return fmt.Errorf("response variant count disagrees with captured source")
	}
	actualKeys := make(map[string]struct{}, len(actual))
	for _, variant := range actual {
		actualKeys[variant.status+"\x00"+variant.mediaType] = struct{}{}
	}
	for _, variant := range locked {
		if _, ok := actualKeys[variant.Status+"\x00"+variant.MediaType]; !ok {
			return fmt.Errorf("response status/media variant %q %q is absent from captured source", variant.Status, variant.MediaType)
		}
	}
	return nil
}

func collectBodyProperties(reference *openapi3.SchemaRef, pointer string, seen map[*openapi3.Schema]struct{}) ([]bodyProperty, bool) {
	if reference == nil || reference.Value == nil {
		return nil, false
	}
	schema := reference.Value
	if _, recursive := seen[schema]; recursive {
		return nil, false
	}
	seen[schema] = struct{}{}
	defer delete(seen, schema)
	_, types, ok := schemaTypes(schema)
	if !ok || (len(types) != 1 || types[0] != "object") {
		return nil, false
	}
	required := make(map[string]struct{}, len(schema.Required))
	for _, name := range schema.Required {
		required[name] = struct{}{}
	}
	properties := make([]bodyProperty, 0, len(schema.Properties))
	seenNames := make(map[string]struct{})
	for name, property := range schema.Properties {
		if property == nil || property.Value == nil || property.Value.ReadOnly {
			continue
		}
		_, isRequired := required[name]
		properties = append(properties, bodyProperty{
			name: name, reference: property,
			pointer: joinPointer(joinPointer(pointer, "properties"), name), required: isRequired,
		})
		seenNames[name] = struct{}{}
	}
	for index, component := range schema.AllOf {
		componentPointer := joinPointer(joinPointer(pointer, "allOf"), strconv.Itoa(index))
		children, complete := collectBodyProperties(component, componentPointer, seen)
		if !complete {
			return properties, false
		}
		for _, child := range children {
			if _, duplicate := seenNames[child.name]; duplicate {
				return properties, false
			}
			seenNames[child.name] = struct{}{}
			properties = append(properties, child)
		}
	}
	slices.SortFunc(properties, func(left, right bodyProperty) int { return strings.Compare(left.name, right.name) })
	return properties, true
}

func parameterFieldID(parameter *openapi3.Parameter) string {
	if value, ok := parameter.Extensions["x-pm-field-id"].(string); ok && validIdentity(value) {
		return value
	}
	return parameter.In + "." + stableFieldSegment(parameter.Name)
}

func requestVariantID(mediaType string) string {
	return "request." + stableFieldSegment(mediaType)
}

func hasRequiredBodyProperty(properties []bodyProperty) bool {
	for _, property := range properties {
		if property.required {
			return true
		}
	}
	return false
}

func stableFieldSegment(value string) string {
	var builder strings.Builder
	lastDash := false
	for _, character := range strings.ToLower(value) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			builder.WriteRune(character)
			lastDash = false
			continue
		}
		if !lastDash {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(builder.String(), "-")
}

func parameterSerialization(parameter *openapi3.Parameter) (string, bool) {
	style := parameter.Style
	if style == "" {
		switch parameter.In {
		case openapi3.ParameterInQuery, openapi3.ParameterInCookie:
			style = "form"
		case openapi3.ParameterInPath, openapi3.ParameterInHeader:
			style = "simple"
		}
	}
	explode := style == "form"
	if parameter.Explode != nil {
		explode = *parameter.Explode
	}
	return style, explode
}

func bodyStyle(mediaType string, schema *openapi3.Schema) string {
	parsed, _, err := mime.ParseMediaType(mediaType)
	if err == nil && (parsed == "application/json" || strings.HasSuffix(parsed, "+json")) {
		return "json"
	}
	if schema != nil && schema.Type != nil && schema.Type.Is("string") && (schema.Format == "binary" || schema.Format == "byte") {
		return "binary"
	}
	return ""
}

func supportedHTTPMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return true
	default:
		return false
	}
}

func operationKey(sourceID, pointer string) string {
	return sourceID + "\x00" + strings.TrimPrefix(pointer, "#")
}

func sortedUniqueStrings(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	result := slices.Clone(values)
	slices.Sort(result)
	return slices.Compact(result)
}

func hashJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return "sha256:" + digestHex(encoded)
}
