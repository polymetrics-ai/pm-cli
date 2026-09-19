package schema

import (
	"slices"
	"strings"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// StaticSchema is the declared operation surface of one bound operation. Its
// fields come from the generated execution bundle, not from account metadata.
type StaticSchema struct {
	OperationID string  `json:"operation_id"`
	Fields      []Field `json:"fields"`
}

// EffectiveField is one typed flag of the effective schema. Dynamic marks a
// field discovered for this account. Blocked marks a declaration that cannot be
// used as declared, so a caller never writes or renders it as writable.
type EffectiveField struct {
	Field
	Dynamic bool `json:"dynamic"`
	Blocked bool `json:"blocked,omitempty"`
}

// EffectiveSchema is the typed projection of one selected snapshot for one
// operation. Every dynamic flag comes from that snapshot only.
type EffectiveSchema struct {
	OperationID string                  `json:"operation_id"`
	Binding     Binding                 `json:"binding"`
	SchemaRef   contract.SemanticDigest `json:"schema_ref"`
	Complete    bool                    `json:"complete"`
	Fields      []EffectiveField        `json:"fields"`
	Gaps        []Gap                   `json:"gaps"`
}

// BuildEffectiveSchema projects the selected snapshot onto the declared
// operation surface. A declaration that agrees with the operation field folds
// its account constraints onto it; a declaration that disagrees blocks exactly
// that field, and unrelated flags keep their declared type.
func BuildEffectiveSchema(static StaticSchema, snapshot Snapshot) (EffectiveSchema, error) {
	if !validBoundedIdentity(static.OperationID) {
		return EffectiveSchema{}, errorf(ErrInvalidSnapshot, "operation_id", "must be a canonical operation identity")
	}
	if static.Fields == nil {
		return EffectiveSchema{}, errorf(ErrInvalidSnapshot, "fields", "must be declared explicitly, even when empty")
	}
	if err := snapshot.Validate(); err != nil {
		return EffectiveSchema{}, err
	}
	gaps := slices.Clone(snapshot.Completeness.Gaps)
	dynamic := make(map[string]Field, len(snapshot.Fields))
	for _, field := range snapshot.Fields {
		dynamic[field.Name] = field
	}
	declared := make(map[string]struct{}, len(static.Fields))
	fields := make([]EffectiveField, 0, len(static.Fields)+len(snapshot.Fields))
	for _, field := range static.Fields {
		if err := field.Validate(); err != nil {
			return EffectiveSchema{}, err
		}
		if _, duplicate := declared[field.Name]; duplicate {
			return EffectiveSchema{}, errorf(ErrInvalidSnapshot, "fields", "declares the same operation field more than once")
		}
		declared[field.Name] = struct{}{}
		entry := EffectiveField{Field: field}
		discovered, present := dynamic[field.Name]
		if present {
			entry.Dynamic = true
			if discovered.Schema.Type != field.Schema.Type {
				entry.Blocked = true
				entry.Writable = false
				gap, err := NewGap(GapConflictingField, field.Name, "the account declaration disagrees with the declared operation type")
				if err != nil {
					return EffectiveSchema{}, err
				}
				gaps = append(gaps, gap)
			} else {
				// The declared operation surface decides encoding; the account
				// declaration supplies the typed constraints it constrains.
				entry.Schema = discovered.Schema
				entry.Required = field.Required || discovered.Required
				entry.Writable = field.Writable && discovered.Writable
				entry.Association = discovered.Association
				entry.Precision = discovered.Precision
				entry.Scale = discovered.Scale
				entry.Pointer = discovered.Pointer
			}
		}
		fields = append(fields, entry)
	}
	for _, field := range snapshot.Fields {
		if _, present := declared[field.Name]; present {
			continue
		}
		fields = append(fields, EffectiveField{Field: field, Dynamic: true})
	}
	slices.SortFunc(fields, func(left, right EffectiveField) int { return strings.Compare(left.Name, right.Name) })
	completeness, err := assessGaps(gaps)
	if err != nil {
		return EffectiveSchema{}, err
	}
	return EffectiveSchema{
		OperationID: static.OperationID,
		Binding:     snapshot.Binding,
		SchemaRef:   snapshot.Fingerprint,
		Complete:    completeness.Complete,
		Fields:      fields,
		Gaps:        completeness.Gaps,
	}, nil
}
