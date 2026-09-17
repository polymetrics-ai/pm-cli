package compile

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"unicode"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func ProjectCommands(normalization *Normalization) ([]contract.Command, []Diagnostic) {
	if normalization == nil {
		return nil, nil
	}
	operations := slices.Clone(normalization.Operations)
	slices.SortFunc(operations, func(left, right NormalizedOperation) int {
		return strings.Compare(left.OperationID, right.OperationID)
	})
	commands := make([]contract.Command, 0, len(operations))
	var diagnostics []Diagnostic
	usedNames := make(map[string]struct{}, len(operations))
	for _, operation := range operations {
		command, diagnostic := projectCommand(operation, usedNames)
		if diagnostic != nil {
			diagnostics = append(diagnostics, *diagnostic)
			continue
		}
		commands = append(commands, *command)
		usedNames[command.Name] = struct{}{}
	}
	return commands, diagnostics
}

func projectCommand(operation NormalizedOperation, usedNames map[string]struct{}) (*contract.Command, *Diagnostic) {
	facts := operationFacts(operation)
	if len(operation.BlockedDiagnosticIDs) != 0 {
		return nil, commandDiagnostic(operation, "", facts, "operation is unavailable and cannot receive a public command descriptor")
	}
	if credentialOperationID(operation.OperationID) {
		return nil, commandDiagnostic(operation, "", facts, "credential-issuing operation cannot be projected as a public command")
	}
	for _, binding := range operation.CommonBindings {
		if binding.Sensitivity != SensitivityPublic {
			return nil, commandDiagnostic(operation, binding.FieldID, facts, "operation contains a secret or unclassified request field")
		}
		if binding.Normalized == nil {
			return nil, commandDiagnostic(operation, binding.FieldID, facts, "operation contains a request field that was not admitted to the runtime contract")
		}
	}
	for _, variant := range operation.RequestVariants {
		for _, binding := range variant.Bindings {
			if binding.Sensitivity != SensitivityPublic {
				return nil, commandDiagnostic(operation, binding.FieldID, facts, "operation contains a secret or unclassified request field")
			}
			if binding.Normalized == nil {
				return nil, commandDiagnostic(operation, binding.FieldID, facts, "operation contains a request field that was not admitted to the runtime contract")
			}
			if binding.Normalized.Location == contract.FieldLocationBody && binding.Normalized.JSONPointer == "" {
				return nil, commandDiagnostic(operation, binding.FieldID, facts, "raw body input is not safe to project as a public command flag")
			}
		}
	}
	for _, response := range operation.ResponseVariants {
		if response.Sensitivity != SensitivityPublic && !response.NoContent {
			return nil, commandDiagnostic(operation, "", facts, "operation can return secret or unclassified response data")
		}
	}
	name := commandSegment(operation.OperationID)
	if _, collision := usedNames[name]; collision {
		digest := sha256.Sum256([]byte(operation.OperationID))
		name += "-" + hex.EncodeToString(digest[:4])
	}
	command := contract.Command{Name: name, OperationID: operation.OperationID, Fields: []contract.CommandField{}}
	flags := make(map[string]struct{})
	appendField := func(binding BindingFact) {
		field := *binding.Normalized
		flag := commandFlag(field)
		if _, collision := flags[flag]; collision {
			flag = "--" + commandSegment(string(field.Location)) + "-" + commandSegment(field.FieldID)
		}
		if _, collision := flags[flag]; collision {
			digest := sha256.Sum256([]byte(field.FieldID))
			flag += "-" + hex.EncodeToString(digest[:4])
		}
		flags[flag] = struct{}{}
		command.Fields = append(command.Fields, contract.CommandField{
			Flag: flag, FieldID: field.FieldID, Location: field.Location, Required: field.Required, Sensitive: false,
		})
	}
	bodyVariantCount := 0
	for _, variant := range operation.RequestVariants {
		if variant.MediaType != parameterOnlyMediaType {
			bodyVariantCount++
		}
	}
	if bodyVariantCount > 1 {
		flag := "--content-type"
		flags[flag] = struct{}{}
		command.Fields = append(command.Fields, contract.CommandField{
			Flag: flag, FieldID: requestMediaFieldID(operation.OperationID),
			Location: contract.FieldLocationHeader, Required: false, Sensitive: false,
		})
	}
	for _, binding := range operation.CommonBindings {
		appendField(binding)
	}
	for _, variant := range operation.RequestVariants {
		for _, binding := range variant.Bindings {
			appendField(binding)
		}
	}
	slices.SortFunc(command.Fields, func(left, right contract.CommandField) int {
		return strings.Compare(left.Flag, right.Flag)
	})
	if err := command.Validate(); err != nil {
		diagnostic := commandDiagnostic(operation, "", facts, err.Error())
		return nil, diagnostic
	}
	return &command, nil
}

func commandDiagnostic(operation NormalizedOperation, fieldID string, facts []SourceFact, message string) *Diagnostic {
	diagnostic := newDiagnostic(
		"unsafe_command_projection",
		SeverityWarning,
		operation.OperationID,
		fieldID,
		facts,
		"/commands/"+escapePointerToken(operation.OperationID),
		nil,
		nil,
		message,
	)
	return &diagnostic
}

func operationFacts(operation NormalizedOperation) []SourceFact {
	return slices.Clone(operation.Raw.SourceFacts)
}

func commandFlag(binding contract.FieldBinding) string {
	candidate := binding.WireName
	if binding.Location == contract.FieldLocationBody {
		pieces := strings.Split(binding.JSONPointer, "/")
		if len(pieces) > 0 {
			candidate = strings.ReplaceAll(strings.ReplaceAll(pieces[len(pieces)-1], "~1", "/"), "~0", "~")
		}
	}
	if candidate == "" {
		candidate = binding.FieldID
	}
	return "--" + commandSegment(candidate)
}

func commandSegment(value string) string {
	var builder strings.Builder
	lastDash := false
	for _, character := range strings.ToLower(value) {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			builder.WriteRune(character)
			lastDash = false
			continue
		}
		if !lastDash && builder.Len() > 0 {
			builder.WriteByte('-')
			lastDash = true
		}
	}
	segment := strings.Trim(builder.String(), "-")
	if segment == "" {
		return "field"
	}
	return segment
}
