package schema

import (
	"fmt"
	"slices"
	"strings"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// Observation is one declared metadata capability result for the bound resource.
// Discovery never infers fields from sample rows: every observation names the
// declared capability, the resource it describes and the declared metadata
// pointer of every field.
type Observation struct {
	Capability string `json:"capability"`
	Resource   string `json:"resource"`
	Available  bool   `json:"available"`
	Truncated  bool   `json:"truncated"`
	// DeniedPermissions lists declared permissions the account was denied.
	DeniedPermissions []string `json:"denied_permissions"`
	// Unsupported lists declared shapes the account reported but this
	// foundation cannot carry. Each stays explicit without suppressing the
	// supported declarations around it.
	Unsupported []string `json:"unsupported"`
	Fields      []Field  `json:"fields"`
}

func (observation Observation) validate(index int) error {
	path := fmt.Sprintf("observations[%d]", index)
	if !validBoundedIdentity(observation.Capability) {
		return errorf(ErrInvalidDiscovery, path+".capability", "must name the declared metadata capability")
	}
	if !validBoundedIdentity(observation.Resource) {
		return errorf(ErrInvalidDiscovery, path+".resource", "must name the declared resource")
	}
	if observation.Fields == nil {
		return errorf(ErrInvalidDiscovery, path+".fields", "must be declared explicitly, even when empty")
	}
	for _, permission := range observation.DeniedPermissions {
		if !validBoundedIdentity(permission) {
			return errorf(ErrInvalidDiscovery, path+".denied_permissions", "must name a declared permission")
		}
	}
	for _, shape := range observation.Unsupported {
		if !validBoundedIdentity(shape) {
			return errorf(ErrInvalidDiscovery, path+".unsupported", "must name a declared shape")
		}
	}
	return nil
}

// Discover normalizes declared metadata observations into one candidate of the
// bound scope. Requiredness, writability, logical type, enums, association,
// precision and the exact provider identifier spelling survive normalization;
// completeness is always explicit.
func Discover(binding Binding, observations []Observation) (Candidate, error) {
	if err := binding.Validate(); err != nil {
		return Candidate{}, err
	}
	completeness, err := AssessCompleteness(observations)
	if err != nil {
		return Candidate{}, err
	}
	fields := make([]Field, 0, len(observations))
	declarations := make(map[string]Field, len(observations))
	gaps := slices.Clone(completeness.Gaps)
	for index, observation := range observations {
		if observation.Resource != binding.Resource {
			return Candidate{}, errorf(ErrSnapshotScope, fmt.Sprintf("observations[%d].resource", index), "describes another resource than the bound discovery")
		}
		for _, field := range observation.Fields {
			if err := field.validateDeclaration(); err != nil {
				return Candidate{}, err
			}
			switch field.Schema.Type {
			case contract.SchemaTypeUnion, contract.SchemaTypeBinaryRef:
				// One unsupported shape stays explicit and never suppresses the
				// supported declarations around it.
				gap, err := NewGap(GapUnsupportedShape, field.Pointer, "the declared metadata field declares a shape this foundation cannot carry")
				if err != nil {
					return Candidate{}, err
				}
				gaps = append(gaps, gap)
				continue
			}
			if err := field.Validate(); err != nil {
				return Candidate{}, err
			}
			prior, duplicate := declarations[field.Name]
			if duplicate {
				priorFingerprint, err := prior.Fingerprint()
				if err != nil {
					return Candidate{}, err
				}
				fingerprint, err := field.Fingerprint()
				if err != nil {
					return Candidate{}, err
				}
				if priorFingerprint == fingerprint {
					continue
				}
				gap, err := NewGap(GapConflictingField, field.Name, "two declared metadata capabilities disagree about this field")
				if err != nil {
					return Candidate{}, err
				}
				gaps = append(gaps, gap)
				continue
			}
			declarations[field.Name] = field
			fields = append(fields, field)
		}
	}
	completeness, err = assessGaps(gaps)
	if err != nil {
		return Candidate{}, err
	}
	slices.SortFunc(fields, func(left, right Field) int { return strings.Compare(left.Name, right.Name) })
	if fields == nil {
		fields = []Field{}
	}
	return Candidate{Binding: binding, Fields: fields, Completeness: completeness}, nil
}
