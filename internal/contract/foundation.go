package contract

import (
	"encoding/json"
	"strings"
)

const FoundationSchemaVersion = "pm.foundation/v1"

type FoundationRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type FoundationContract struct {
	SchemaVersion string          `json:"schema_version,omitempty"`
	ID            string          `json:"id"`
	Version       string          `json:"version"`
	Name          string          `json:"name,omitempty"`
	Owner         string          `json:"owner,omitempty"`
	Dependencies  []FoundationRef `json:"dependencies"`
}

func (f FoundationRef) Validate() error {
	return validateFoundationIdentity(f.ID, f.Version, "foundation")
}

func (f FoundationContract) Validate() error {
	if f.SchemaVersion != "" && f.SchemaVersion != FoundationSchemaVersion {
		return contractError(ErrUnsupportedVersion, "foundation.schema_version", "foundation schema revision is unsupported")
	}
	if err := validateFoundationIdentity(f.ID, f.Version, "foundation"); err != nil {
		return err
	}
	if f.SchemaVersion != "" {
		if !validIdentityPart(f.Name) {
			return contractError(ErrInvalidContract, "foundation.name", "must be a nonempty valid UTF-8 canonical name")
		}
		if !validIdentityPart(f.Owner) {
			return contractError(ErrInvalidContract, "foundation.owner", "must be a nonempty valid UTF-8 canonical owner")
		}
		if f.Dependencies == nil {
			return contractError(ErrInvalidContract, "foundation.dependencies", "must be explicitly declared, even when empty")
		}
	} else {
		if f.Name != "" && !validIdentityPart(f.Name) {
			return contractError(ErrInvalidContract, "foundation.name", "must be canonical when present")
		}
		if f.Owner != "" && !validIdentityPart(f.Owner) {
			return contractError(ErrInvalidContract, "foundation.owner", "must be canonical when present")
		}
	}
	seen := make(map[string]struct{}, len(f.Dependencies))
	for _, dependency := range f.Dependencies {
		if err := dependency.Validate(); err != nil {
			return err
		}
		if dependency.ID == f.ID && dependency.Version == f.Version {
			return contractError(ErrInvalidContract, "foundation.dependencies", "foundation cannot depend on itself")
		}
		key := dependency.ID + "@" + dependency.Version
		if _, duplicate := seen[key]; duplicate {
			return contractError(ErrDuplicateField, "foundation.dependencies", "dependency is duplicated")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func DecodeFoundationContract(document []byte) (FoundationContract, error) {
	var fields map[string]json.RawMessage
	if err := decodeContractJSON(document, &fields); err != nil {
		return FoundationContract{}, err
	}
	for _, required := range []string{"schema_version", "id", "version", "name", "owner", "dependencies"} {
		if _, ok := fields[required]; !ok {
			return FoundationContract{}, contractError(ErrInvalidContract, required, "required field is absent")
		}
	}
	var foundation FoundationContract
	if err := decodeStrictJSON(document, &foundation); err != nil {
		return FoundationContract{}, err
	}
	if foundation.SchemaVersion != FoundationSchemaVersion {
		return FoundationContract{}, contractError(ErrUnsupportedVersion, "foundation.schema_version", "source document requires the supported foundation revision")
	}
	if err := foundation.Validate(); err != nil {
		return FoundationContract{}, err
	}
	return foundation, nil
}

func validateFoundationIdentity(id, version, path string) error {
	if len(id) < 5 || len(id) > 512 || !strings.HasPrefix(id, "FND-") {
		return contractError(ErrInvalidContract, path+".id", "must use the FND-<positive number> identity")
	}
	number := strings.TrimPrefix(id, "FND-")
	if !decimalDigits(number) || strings.Trim(number, "0") == "" {
		return contractError(ErrInvalidContract, path+".id", "must use the FND-<positive number> identity")
	}
	if !validFoundationVersion(version) {
		return contractError(ErrInvalidContract, path+".version", "must be a canonical major.minor.patch version")
	}
	return nil
}

func validFoundationVersion(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') || !decimalDigits(part) {
			return false
		}
	}
	return len(version) <= 512
}

func decimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}
