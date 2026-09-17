package contract

import (
	"encoding/json"
	"time"
)

const IdentitySchemaVersion = "pm.identity/v1"

// Scope identifies one explicit workspace inside one organization. It is a
// reference only; authorization and RLS enforcement belong to their owners.
type Scope struct {
	OrgID       string `json:"org_id"`
	WorkspaceID string `json:"workspace_id"`
}

func (s *Scope) UnmarshalJSON(data []byte) error {
	type wire Scope
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "org_id", "workspace_id"); err != nil {
		return err
	}
	value := Scope(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*s = value
	return nil
}

func (s Scope) Validate() error {
	if !validIdentityPart(s.OrgID) {
		return contractError(ErrInvalidIdentity, "scope.org_id", "must identify one organization")
	}
	if !validIdentityPart(s.WorkspaceID) {
		return contractError(ErrInvalidIdentity, "scope.workspace_id", "must identify one explicit workspace")
	}
	return nil
}

func (s Scope) Equal(other Scope) bool {
	return s.OrgID == other.OrgID && s.WorkspaceID == other.WorkspaceID
}

// Tenant is the versioned safe identity of an organization.
type Tenant struct {
	OrgID    string `json:"org_id"`
	Revision uint64 `json:"revision"`
}

func (t *Tenant) UnmarshalJSON(data []byte) error {
	type wire Tenant
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "org_id", "revision"); err != nil {
		return err
	}
	value := Tenant(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*t = value
	return nil
}

func (t Tenant) Validate() error {
	if !validIdentityPart(t.OrgID) {
		return contractError(ErrInvalidIdentity, "tenant.org_id", "must identify one organization")
	}
	if t.Revision == 0 {
		return contractError(ErrInvalidIdentity, "tenant.revision", "must be a positive immutable revision")
	}
	return nil
}

func decodeRequiredObject(data []byte, target any, required ...string) error {
	_, err := decodeRequiredObjectFields(data, target, required...)
	return err
}

func decodeRequiredObjectFields(data []byte, target any, required ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := decodeContractJSON(data, &fields); err != nil {
		return nil, err
	}
	for _, field := range required {
		if _, ok := fields[field]; !ok {
			return nil, contractError(ErrInvalidContract, field, "required field is absent")
		}
	}
	if err := decodeStrictJSON(data, target); err != nil {
		return nil, err
	}
	return fields, nil
}

func validateUTCInstant(value time.Time, path string) error {
	if value.IsZero() {
		return contractError(ErrInvalidContract, path, "must be recorded explicitly")
	}
	if value.Location() != time.UTC {
		return contractError(ErrInvalidContract, path, "must use the canonical UTC location")
	}
	return nil
}

func validateIdentityList(values []string, path string, allowEmpty bool) error {
	return validateBoundedIdentityList(values, path, allowEmpty, 512)
}

func validateBoundedIdentityList(values []string, path string, allowEmpty bool, maximumBytes int) error {
	if values == nil {
		return contractError(ErrInvalidContract, path, "must be explicitly present, even when empty")
	}
	if !allowEmpty && len(values) == 0 {
		return contractError(ErrInvalidContract, path, "must contain at least one scoped identity")
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !validBoundedIdentityPart(value, maximumBytes) {
			return contractError(ErrInvalidIdentity, path, "contains an invalid opaque identity")
		}
		if _, duplicate := seen[value]; duplicate {
			return contractError(ErrDuplicateField, path, "identity is duplicated")
		}
		seen[value] = struct{}{}
	}
	return nil
}

func sameStringSetSubset(values, ceiling []string) bool {
	allowed := make(map[string]struct{}, len(ceiling))
	for _, value := range ceiling {
		allowed[value] = struct{}{}
	}
	for _, value := range values {
		if _, ok := allowed[value]; !ok {
			return false
		}
	}
	return true
}
func validReasonCode(value string) bool {
	if len(value) == 0 || len(value) > 128 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for index := 1; index < len(value); index++ {
		character := value[index]
		if (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') ||
			character == '_' || character == '-' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
