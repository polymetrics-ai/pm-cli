package contract

import "strings"

// Command is a safe presentation descriptor. It does not execute an operation or resolve credentials.
type Command struct {
	Name        string         `json:"name"`
	OperationID string         `json:"operation_id"`
	Fields      []CommandField `json:"fields"`
}

type CommandField struct {
	Flag      string        `json:"flag"`
	FieldID   string        `json:"field_id"`
	Location  FieldLocation `json:"location"`
	Required  bool          `json:"required"`
	Sensitive bool          `json:"sensitive"`
}

func (c Command) Validate() error {
	if !validIdentityPart(c.Name) || strings.ContainsAny(c.Name, " \t\r\n") {
		return contractError(ErrInvalidContract, "command.name", "must be a nonempty single command segment")
	}
	if !validIdentityPart(c.OperationID) {
		return contractError(ErrInvalidContract, "command.operation_id", "must reference a stable operation")
	}
	if c.Fields == nil {
		return contractError(ErrInvalidContract, "command.fields", "must be explicitly declared")
	}
	seenFlags := make(map[string]struct{}, len(c.Fields))
	seenFields := make(map[string]struct{}, len(c.Fields))
	for _, field := range c.Fields {
		if !validIdentityPart(field.Flag) || !strings.HasPrefix(field.Flag, "--") || strings.ContainsAny(field.Flag, " \t\r\n=") {
			return contractError(ErrInvalidContract, "command.fields.flag", "must be an exact long option")
		}
		if !validIdentityPart(field.FieldID) || !validFieldLocation(field.Location) {
			return contractError(ErrInvalidContract, "command.fields", "contains an invalid typed field reference")
		}
		if _, duplicate := seenFlags[field.Flag]; duplicate {
			return contractError(ErrDuplicateField, "command.fields.flag", "flag is duplicated")
		}
		if _, duplicate := seenFields[field.FieldID]; duplicate {
			return contractError(ErrDuplicateField, "command.fields.field_id", "field is duplicated")
		}
		seenFlags[field.Flag] = struct{}{}
		seenFields[field.FieldID] = struct{}{}
	}
	return nil
}
