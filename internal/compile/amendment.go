package compile

import (
	"fmt"
	"slices"
	"strings"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const SourceRevisionDiffSchemaVersion = "pm.source-revision-diff/v1"

type SourceRevisionDiff struct {
	SchemaVersion  string          `json:"schema_version"`
	PreviousDigest string          `json:"previous_source_digest"`
	CurrentDigest  string          `json:"current_source_digest"`
	Changes        []PointerChange `json:"changes"`
}

type PointerChange struct {
	SourceID       string `json:"source_id"`
	Pointer        string `json:"pointer"`
	PreviousSHA256 string `json:"previous_sha256"`
	CurrentSHA256  string `json:"current_sha256"`
}

func DecodeSourceRevisionDiff(document []byte) (SourceRevisionDiff, error) {
	var diff SourceRevisionDiff
	if err := contract.DecodeStrictRequiredObject(
		document,
		&diff,
		"schema_version",
		"previous_source_digest",
		"current_source_digest",
		"changes",
	); err != nil {
		return SourceRevisionDiff{}, sourceError(ErrUnsafeAmendment, "", "", err.Error())
	}
	if err := diff.Validate(); err != nil {
		return SourceRevisionDiff{}, err
	}
	return diff, nil
}

func (diff SourceRevisionDiff) Validate() error {
	if diff.SchemaVersion != SourceRevisionDiffSchemaVersion {
		return sourceError(ErrUnsafeAmendment, "", "/schema_version", "source revision diff schema is unsupported")
	}
	if !validDigest(diff.PreviousDigest) || !validDigest(diff.CurrentDigest) || diff.PreviousDigest == diff.CurrentDigest {
		return sourceError(ErrUnsafeAmendment, "", "", "source revision diff must bind distinct canonical source digests")
	}
	if len(diff.Changes) == 0 {
		return sourceError(ErrUnsafeAmendment, "", "/changes", "source revision diff must identify at least one changed pointer")
	}
	seen := make(map[string]struct{}, len(diff.Changes))
	for index, change := range diff.Changes {
		pointer := fmt.Sprintf("/changes/%d", index)
		if !validIdentity(change.SourceID) || !validJSONPointer(strings.TrimPrefix(change.Pointer, "#")) || change.Pointer == "" {
			return sourceError(ErrUnsafeAmendment, change.SourceID, pointer, "changed source identity and pointer are required")
		}
		if !validDigest(change.PreviousSHA256) || !validDigest(change.CurrentSHA256) || change.PreviousSHA256 == change.CurrentSHA256 {
			return sourceError(ErrUnsafeAmendment, change.SourceID, change.Pointer, "changed pointer must bind distinct canonical hashes")
		}
		key := change.SourceID + "\x00" + change.Pointer
		if _, duplicate := seen[key]; duplicate {
			return sourceError(ErrUnsafeAmendment, change.SourceID, change.Pointer, "changed pointer is duplicated")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func ValidateRevisionDiff(previousDigest, currentDigest string, diff *SourceRevisionDiff) error {
	if previousDigest == "" || previousDigest == currentDigest {
		if diff != nil {
			return sourceError(ErrUnsafeAmendment, "", "", "source revision diff is forbidden when source identity did not change")
		}
		return nil
	}
	if diff == nil {
		return sourceError(ErrUnsafeAmendment, "", "", "source identity changed without an explicit pointer-level revision diff")
	}
	if err := diff.Validate(); err != nil {
		return err
	}
	if diff.PreviousDigest != previousDigest || diff.CurrentDigest != currentDigest {
		return sourceError(ErrUnsafeAmendment, "", "", "source revision diff does not bind the previous and current source identities")
	}
	return nil
}
func ValidateCurrentRevisionDiff(source *ResolvedSource, diff *SourceRevisionDiff) error {
	if diff == nil {
		return nil
	}
	if source == nil || source.Captured == nil || diff.CurrentDigest != source.Captured.Digest {
		return sourceError(ErrUnsafeAmendment, "", "", "revision diff is detached from the current captured source")
	}
	for _, change := range diff.Changes {
		hash, ok := source.PointerHash(change.SourceID, strings.TrimPrefix(change.Pointer, "#"))
		if !ok {
			return sourceError(ErrUnsafeAmendment, change.SourceID, change.Pointer, "changed pointer is absent from the current captured source")
		}
		if hash != change.CurrentSHA256 {
			return sourceError(ErrUnsafeAmendment, change.SourceID, change.Pointer, "changed pointer hash disagrees with the current captured source")
		}
	}
	return nil
}

func ApplyAmendments(
	normalization *Normalization,
	source *ResolvedSource,
	semantics *SemanticsDocument,
) error {
	if normalization == nil || source == nil || source.Captured == nil {
		return sourceError(ErrInvalidSource, "", "", "normalization and resolved source are required")
	}
	if semantics == nil {
		return nil
	}
	if err := semantics.Validate(); err != nil {
		return err
	}
	if semantics.ConnectorID != normalization.ConnectorID || semantics.SourceDigest != normalization.SourceDigest {
		return sourceError(ErrUnsafeAmendment, "", "", "semantic amendment cannot change connector or source authority")
	}
	if semantics.Revision != source.Captured.Lock.SemanticsRevision {
		return sourceError(ErrUnsafeAmendment, "", "/revision", "semantic amendment revision does not match the source lock")
	}
	operationIndex := make(map[string]int, len(normalization.Operations))
	for index := range normalization.Operations {
		operationIndex[normalization.Operations[index].OperationID] = index
	}
	for semanticsIndex := range semantics.Operations {
		amendment := semantics.Operations[semanticsIndex]
		index, ok := operationIndex[amendment.OperationID]
		if !ok {
			return sourceError(ErrUnsafeAmendment, "", fmt.Sprintf("/operations/%d/operation_id", semanticsIndex), "amendment cannot introduce a source operation")
		}
		if err := validateAmendmentSourceRefs(source, amendment.SourceRefs); err != nil {
			return err
		}
		operation := &normalization.Operations[index]
		if amendment.HTTP != nil {
			if err := validateAmendmentSourceRefs(source, amendment.HTTP.Witness.SourceRefs); err != nil {
				return err
			}
			if err := validateHTTPAmendment(operation.HTTP, amendment.HTTP); err != nil {
				return err
			}
		}
		if amendment.RecordExtraction != nil {
			if err := validateAmendmentSourceRefs(source, amendment.RecordExtraction.Witness.SourceRefs); err != nil {
				return err
			}
		}
		if amendment.Pagination != nil {
			if err := validateAmendmentSourceRefs(source, amendment.Pagination.Witness.SourceRefs); err != nil {
				return err
			}
			if amendment.Pagination.Kind == string(contract.OperationPaginationLink) {
				if err := validateLinkPaginationSource(operation, source, amendment.Pagination); err != nil {
					return err
				}
			}
		}
		if amendment.Retry != nil {
			if err := validateAmendmentSourceRefs(source, amendment.Retry.Witness.SourceRefs); err != nil {
				return err
			}
		}
		for _, lane := range amendment.Lanes {
			if err := validateAmendmentSourceRefs(source, lane.SourceRefs); err != nil {
				return err
			}
		}
		operation = &normalization.Operations[index]
		copy := amendment.clone()
		operation.Semantics = &copy
		resolveBoundResponseHeaders(normalization, operation, amendment.Pagination)
		operation.LaneInventory = applyLaneSemantics(operation.LaneInventory, amendment.Lanes)
		applyBlockingDiagnostics(operation, normalization.Diagnostics)
	}
	return nil
}

func validateAmendmentSourceRefs(source *ResolvedSource, references []contract.SourceRef) error {
	if len(references) == 0 {
		return sourceError(ErrUnsafeAmendment, "", "", "semantic amendment fact is not source-linked")
	}
	seen := make(map[string]struct{}, len(references))
	for _, reference := range references {
		pointer := strings.TrimPrefix(reference.Pointer, "#")
		if !validIdentity(reference.SourceID) || !validJSONPointer(pointer) || reference.Pointer == "" {
			return sourceError(ErrUnsafeAmendment, reference.SourceID, reference.Pointer, "semantic amendment source reference is malformed")
		}
		if _, ok := source.RawValue(reference.SourceID, pointer); !ok {
			return sourceError(ErrUnsafeAmendment, reference.SourceID, reference.Pointer, "semantic amendment source pointer is outside the captured closure")
		}
		key := reference.SourceID + "\x00" + pointer
		if _, duplicate := seen[key]; duplicate {
			return sourceError(ErrUnsafeAmendment, reference.SourceID, reference.Pointer, "semantic amendment source reference is duplicated")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateHTTPAmendment(sourceHTTP, amendmentHTTP *HTTPSemantics) error {
	if sourceHTTP == nil {
		return sourceError(ErrUnsafeAmendment, "", "/http", "HTTP amendment cannot create authority absent from independently normalized source facts")
	}
	for _, origin := range amendmentHTTP.Origins {
		if !slices.Contains(sourceHTTP.Origins, origin) {
			return sourceError(ErrUnsafeAmendment, "", "/http/origins", "HTTP amendment cannot add or rewrite a source-admitted origin")
		}
	}
	switch {
	case sourceHTTP.Authorization == nil && amendmentHTTP.Authorization != nil:
		return sourceError(ErrUnsafeAmendment, "", "/http/authorization", "HTTP amendment cannot create a credential channel absent from source")
	case sourceHTTP.Authorization != nil && sourceHTTP.Authorization.Required && amendmentHTTP.Authorization == nil:
		return sourceError(ErrUnsafeAmendment, "", "/http/authorization", "HTTP amendment cannot remove source-required authorization")
	case amendmentHTTP.Authorization != nil && (sourceHTTP.Authorization == nil ||
		*amendmentHTTP.Authorization != *sourceHTTP.Authorization):
		return sourceError(ErrUnsafeAmendment, "", "/http/authorization", "HTTP amendment authorization must exactly match independently normalized source facts")
	}
	if len(amendmentHTTP.Headers) != len(sourceHTTP.Headers) {
		return sourceError(ErrUnsafeAmendment, "", "/http/headers", "HTTP amendment cannot add or remove source-required fixed headers")
	}
	sourceHeaders := make(map[string]string, len(sourceHTTP.Headers))
	for _, header := range sourceHTTP.Headers {
		sourceHeaders[strings.ToLower(header.Name)] = header.Value
	}
	for _, header := range amendmentHTTP.Headers {
		value, admitted := sourceHeaders[strings.ToLower(header.Name)]
		if !admitted || value != header.Value {
			return sourceError(ErrUnsafeAmendment, "", "/http/headers", "HTTP amendment cannot add or rewrite a source-required fixed header")
		}
	}
	sourceReferences := make(map[string]struct{}, len(sourceHTTP.Witness.SourceRefs))
	for _, reference := range sourceHTTP.Witness.SourceRefs {
		sourceReferences[reference.SourceID+"\x00"+reference.Pointer] = struct{}{}
	}
	for _, reference := range amendmentHTTP.Witness.SourceRefs {
		if _, admitted := sourceReferences[reference.SourceID+"\x00"+reference.Pointer]; !admitted {
			return sourceError(ErrUnsafeAmendment, reference.SourceID, reference.Pointer, "HTTP amendment witness is outside independently normalized HTTP source facts")
		}
	}
	return nil
}

func validateLinkPaginationSource(
	operation *NormalizedOperation,
	source *ResolvedSource,
	pagination *Pagination,
) error {
	for _, reference := range pagination.Witness.SourceRefs {
		if !sourceRefDeclaresOperationResponseHeader(operation, reference, pagination.LinkResponseHeader) {
			continue
		}
		if _, present := source.RawValue(reference.SourceID, strings.TrimPrefix(reference.Pointer, "#")); present {
			return nil
		}
	}
	return sourceError(ErrUnsafeAmendment, "", "/pagination/link_response_header", "link pagination requires an exact source-declared operation response header")
}

func sourceRefDeclaresOperationResponseHeader(
	operation *NormalizedOperation,
	reference contract.SourceRef,
	headerName string,
) bool {
	pointer := strings.TrimPrefix(reference.Pointer, "#")
	parts := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	for index := range parts {
		parts[index] = strings.ReplaceAll(strings.ReplaceAll(parts[index], "~1", "/"), "~0", "~")
	}
	if len(parts) < 3 || parts[len(parts)-2] != "headers" ||
		!strings.EqualFold(parts[len(parts)-1], headerName) {
		return false
	}
	for _, operationReference := range operation.SourceRefs {
		if operationReference.SourceID != reference.SourceID {
			continue
		}
		operationPointer := strings.TrimSuffix(strings.TrimPrefix(operationReference.Pointer, "#"), "/")
		if strings.HasPrefix(pointer, operationPointer+"/responses/") {
			return true
		}
	}
	return false
}

func resolveBoundResponseHeaders(normalization *Normalization, operation *NormalizedOperation, pagination *Pagination) {
	if pagination == nil || pagination.Kind != string(contract.OperationPaginationLink) {
		return
	}
	resolved := make(map[string]struct{})
	normalization.Diagnostics = slices.DeleteFunc(normalization.Diagnostics, func(diagnostic Diagnostic) bool {
		if diagnostic.OperationID != operation.OperationID || diagnostic.Code != "unbound_response_header" {
			return false
		}
		for _, fact := range diagnostic.SourceFacts {
			if !sourceRefDeclaresOperationResponseHeader(operation, fact.SourceRef, pagination.LinkResponseHeader) {
				continue
			}
			if slices.Contains(pagination.Witness.SourceRefs, fact.SourceRef) {
				resolved[diagnostic.ID] = struct{}{}
				return true
			}
		}
		return false
	})
	if len(resolved) == 0 {
		return
	}
	isResolved := func(id string) bool {
		_, found := resolved[id]
		return found
	}
	operation.BlockedDiagnosticIDs = slices.DeleteFunc(operation.BlockedDiagnosticIDs, isResolved)
	for index := range operation.ResponseVariants {
		variant := &operation.ResponseVariants[index]
		variant.BlockedDiagnosticIDs = slices.DeleteFunc(variant.BlockedDiagnosticIDs, isResolved)
	}
}

func applyBlockingDiagnostics(operation *NormalizedOperation, diagnostics []Diagnostic) {
	blocked := make(map[contract.Lane][]string)
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity != SeverityError || diagnostic.OperationID != operation.OperationID {
			continue
		}
		for _, lane := range diagnostic.AffectedCells {
			blocked[lane] = append(blocked[lane], diagnostic.Code)
		}
	}
	for index := range operation.LaneInventory {
		lane := &operation.LaneInventory[index]
		codes, affected := blocked[lane.Lane]
		if !affected || lane.Status != LaneEligible {
			continue
		}
		slices.Sort(codes)
		codes = slices.Compact(codes)
		lane.Status = LaneBlocked
		lane.Reason = "compiler diagnostics block this lane: " + strings.Join(codes, ", ")
	}
}
