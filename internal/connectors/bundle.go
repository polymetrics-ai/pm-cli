package connectors

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const BundleSchemaVersion = "pm.bundle/v1"

var ErrInvalidBundle = errors.New("connector bundle is invalid")

// Bundle is the only runtime connector input. It contains the admitted
// operation contracts, the safe command projection, and explicit unavailable
// source operations; runtime code never reads compiler inputs or sidecars.
type Bundle struct {
	SchemaVersion         string                          `json:"schema_version"`
	ConnectorID           string                          `json:"connector_id"`
	SourceDigest          string                          `json:"source_digest"`
	SemanticsRevision     string                          `json:"semantics_revision"`
	Operations            []contract.Operation            `json:"operations"`
	Commands              []contract.Command              `json:"commands"`
	UnavailableOperations []contract.UnavailableOperation `json:"unavailable_operations"`
}

func LoadBundle(reader io.Reader) (Bundle, error) {
	if reader == nil {
		return Bundle{}, fmt.Errorf("%w: reader is required", ErrInvalidBundle)
	}
	document, err := io.ReadAll(io.LimitReader(reader, contract.MaxContractDocumentBytes+1))
	if err != nil {
		return Bundle{}, fmt.Errorf("%w: read document", ErrInvalidBundle)
	}
	if len(document) > contract.MaxContractDocumentBytes {
		return Bundle{}, fmt.Errorf("%w: document exceeds the contract byte limit", ErrInvalidBundle)
	}
	var bundle Bundle
	if err := contract.DecodeStrictRequiredObject(
		document,
		&bundle,
		"schema_version",
		"connector_id",
		"source_digest",
		"semantics_revision",
		"operations",
		"commands",
		"unavailable_operations",
	); err != nil {
		return Bundle{}, fmt.Errorf("%w: %v", ErrInvalidBundle, err)
	}
	if err := bundle.Validate(); err != nil {
		return Bundle{}, err
	}
	return bundle, nil
}

func (bundle Bundle) Validate() error {
	if bundle.SchemaVersion != BundleSchemaVersion {
		return invalidBundle("schema_version is unsupported")
	}
	if !validIdentifier(bundle.ConnectorID, 512) {
		return invalidBundle("connector_id is invalid")
	}
	if contract.SemanticDigest(bundle.SourceDigest).Validate() != nil {
		return invalidBundle("source_digest is invalid")
	}
	if contract.SemanticDigest(bundle.SemanticsRevision).Validate() != nil {
		return invalidBundle("semantics_revision is invalid")
	}
	if bundle.Operations == nil || bundle.Commands == nil || bundle.UnavailableOperations == nil {
		return invalidBundle("operation, command, and unavailable inventories must be explicit")
	}
	if len(bundle.Operations)+len(bundle.UnavailableOperations) == 0 {
		return invalidBundle("combined operation inventory is empty")
	}

	operations := make(map[string]contract.Operation, len(bundle.Operations))
	for _, operation := range bundle.Operations {
		if err := operation.Validate(foundationsFor(operation)); err != nil {
			return invalidBundle("operation %q is invalid: %v", operation.OperationID, err)
		}
		if _, duplicate := operations[operation.OperationID]; duplicate {
			return invalidBundle("operation_id %q is duplicated", operation.OperationID)
		}
		operations[operation.OperationID] = operation
	}
	for _, unavailable := range bundle.UnavailableOperations {
		if err := unavailable.Validate(); err != nil {
			return invalidBundle("unavailable operation %q is invalid: %v", unavailable.OperationID, err)
		}
		if _, duplicate := operations[unavailable.OperationID]; duplicate {
			return invalidBundle("operation_id %q is duplicated across inventories", unavailable.OperationID)
		}
		operations[unavailable.OperationID] = contract.Operation{}
	}

	seenNames := make(map[string]struct{}, len(bundle.Commands))
	seenOperations := make(map[string]struct{}, len(bundle.Commands))
	for _, command := range bundle.Commands {
		if err := command.Validate(); err != nil {
			return invalidBundle("command %q is invalid: %v", command.Name, err)
		}
		operation, executable := operations[command.OperationID]
		if !executable || operation.OperationID == "" {
			return invalidBundle("command %q does not reference an executable operation", command.Name)
		}
		if _, duplicate := seenNames[command.Name]; duplicate {
			return invalidBundle("command name %q is duplicated", command.Name)
		}
		if _, duplicate := seenOperations[command.OperationID]; duplicate {
			return invalidBundle("operation %q has multiple commands", command.OperationID)
		}
		if err := validateCommandProjection(command, operation); err != nil {
			return invalidBundle("command %q is not an exact safe projection: %v", command.Name, err)
		}
		seenNames[command.Name] = struct{}{}
		seenOperations[command.OperationID] = struct{}{}
	}
	return nil
}

func foundationsFor(operation contract.Operation) []contract.FoundationContract {
	foundations := make([]contract.FoundationContract, 0, len(operation.CapabilityRequirements))
	seen := make(map[string]struct{}, len(operation.CapabilityRequirements))
	for _, requirement := range operation.CapabilityRequirements {
		key := requirement.Foundation.ID + "\x00" + requirement.Foundation.Version
		if _, present := seen[key]; present {
			continue
		}
		seen[key] = struct{}{}
		foundations = append(foundations, contract.FoundationContract{
			ID:           requirement.Foundation.ID,
			Version:      requirement.Foundation.Version,
			Dependencies: []contract.FoundationRef{},
		})
	}
	return foundations
}

func validateCommandProjection(command contract.Command, operation contract.Operation) error {
	bindings := make(map[string]contract.FieldBinding, len(operation.Inputs))
	for _, binding := range operation.Inputs {
		bindings[binding.FieldID] = binding
	}
	for _, variant := range operation.RequestVariants {
		for _, binding := range variant.Inputs {
			bindings[binding.FieldID] = binding
		}
	}
	if len(command.Fields) != len(bindings) {
		return errors.New("field inventory does not match the executable operation")
	}
	for _, field := range command.Fields {
		binding, present := bindings[field.FieldID]
		if !present {
			return fmt.Errorf("field_id %q is absent from the executable operation", field.FieldID)
		}
		if field.Sensitive || binding.Sensitivity != contract.SensitivityPublic {
			return fmt.Errorf("field_id %q is not public", field.FieldID)
		}
		if binding.Location == contract.FieldLocationBody && binding.JSONPointer == "" {
			return fmt.Errorf("field_id %q is an unsafe whole-body binding", field.FieldID)
		}
		if field.Location != binding.Location || field.Required != binding.Required {
			return fmt.Errorf("field_id %q changes its typed binding", field.FieldID)
		}
	}
	return nil
}

func validIdentifier(value string, maximum int) bool {
	if value == "" || len(value) > maximum || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character <= 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func invalidBundle(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidBundle, fmt.Sprintf(format, arguments...))
}
