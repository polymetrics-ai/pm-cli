package schema

import (
	"maps"
	"slices"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// FieldChangeKind is one relevant constraint change of a declared field.
type FieldChangeKind string

const (
	// FieldAdded reports an account field the active snapshot did not declare.
	FieldAdded FieldChangeKind = "added"
	// FieldRemoved reports an active field the account declaration dropped.
	FieldRemoved FieldChangeKind = "removed"
	// FieldTypeChanged reports a changed logical type.
	FieldTypeChanged FieldChangeKind = "type_changed"
	// FieldEnumChanged reports changed declared enum values.
	FieldEnumChanged FieldChangeKind = "enum_changed"
	// FieldNullabilityChanged reports a changed declared nullability.
	FieldNullabilityChanged FieldChangeKind = "nullability_changed"
	// FieldRequirednessChanged reports a changed declared requiredness.
	FieldRequirednessChanged FieldChangeKind = "requiredness_changed"
	// FieldWritabilityChanged reports a field that became read-only or writable.
	FieldWritabilityChanged FieldChangeKind = "writability_changed"
	// FieldAssociationChanged reports a changed declared association.
	FieldAssociationChanged FieldChangeKind = "association_changed"
	// FieldPrecisionChanged reports changed declared precision or scale.
	FieldPrecisionChanged FieldChangeKind = "precision_changed"
	// FieldConstraintsChanged reports a residual constraint change that is
	// relevant even though no single named member changed.
	FieldConstraintsChanged FieldChangeKind = "constraints_changed"
)

// FieldChange names one relevant change without provider payload.
type FieldChange struct {
	Field  string          `json:"field"`
	Kind   FieldChangeKind `json:"kind"`
	Detail string          `json:"detail"`
}

// Drift is the difference between the active snapshot and one discovered
// candidate. Only relevant constraints appear: a provenance move does not
// invalidate a saved plan, while a type, enum, nullability, requiredness,
// writability, association or precision change does.
type Drift struct {
	BindingChanged bool          `json:"binding_changed"`
	BindingDetail  string        `json:"binding_detail,omitempty"`
	Changes        []FieldChange `json:"changes"`
	// BindingFields lists every declared field when the binding itself changed:
	// the account's declarations then belong to another snapshot scope, so no
	// field of either side may be reused.
	BindingFields []string `json:"binding_fields"`
}

// Empty reports whether the discovered metadata leaves every declaration usable.
func (drift Drift) Empty() bool {
	return !drift.BindingChanged && len(drift.Changes) == 0
}

// AffectedFields lists every field whose constraints changed. A binding change
// affects every declared field of both sides.
func (drift Drift) AffectedFields() []string {
	if drift.BindingChanged {
		return slices.Clone(drift.BindingFields)
	}
	affected := make([]string, 0, len(drift.Changes))
	seen := make(map[string]struct{}, len(drift.Changes))
	for _, change := range drift.Changes {
		if _, present := seen[change.Field]; present {
			continue
		}
		seen[change.Field] = struct{}{}
		affected = append(affected, change.Field)
	}
	slices.Sort(affected)
	return affected
}

// InvalidatesPlan reports whether a saved plan or approval bound to this field
// set must be re-approved. Drift of a field the profile does not depend on
// leaves it usable; a profile without field dependencies depends on the whole
// snapshot.
func (drift Drift) InvalidatesPlan(plan PlanBinding) bool {
	if drift.Empty() {
		return false
	}
	if drift.BindingChanged {
		return true
	}
	if len(plan.Fields) == 0 {
		return true
	}
	affected := make(map[string]struct{}, len(drift.Changes))
	for _, name := range drift.AffectedFields() {
		affected[name] = struct{}{}
	}
	for _, name := range plan.Fields {
		if _, present := affected[name]; present {
			return true
		}
	}
	return false
}

// PlanBinding is the schema dependency of one saved plan or approval: the
// reference it observed and the fields it depends on. No field dependency means
// the whole snapshot.
type PlanBinding struct {
	ProfileID string                  `json:"profile_id"`
	SchemaRef contract.SemanticDigest `json:"schema_ref"`
	Fields    []string                `json:"fields"`
}

// DetectDrift compares the active snapshot with one discovered candidate.
func DetectDrift(active Snapshot, discovered Candidate) (Drift, error) {
	if err := active.Validate(); err != nil {
		return Drift{}, err
	}
	if err := discovered.Validate(); err != nil {
		return Drift{}, err
	}
	drift := Drift{Changes: []FieldChange{}, BindingFields: []string{}}
	if active.Binding != discovered.Binding {
		drift.BindingChanged = true
		drift.BindingDetail = "the connection, account, permission revision, resource or source revision changed"
	}
	drift.BindingFields = unionFieldNames(active.Fields, discovered.Fields)
	activeFields := indexFields(active.Fields)
	discoveredFields := indexFields(discovered.Fields)
	for _, name := range drift.BindingFields {
		before, declared := activeFields[name]
		after, stillDeclared := discoveredFields[name]
		switch {
		case !declared:
			drift.Changes = append(drift.Changes, FieldChange{
				Field: name, Kind: FieldAdded, Detail: "the account metadata declares a field the active snapshot does not",
			})
		case !stillDeclared:
			drift.Changes = append(drift.Changes, FieldChange{
				Field: name, Kind: FieldRemoved, Detail: "the account metadata no longer declares this field",
			})
		default:
			changes, err := compareDeclarations(name, before, after)
			if err != nil {
				return Drift{}, err
			}
			drift.Changes = append(drift.Changes, changes...)
		}
	}
	return drift, nil
}

func compareDeclarations(name string, before, after Field) ([]FieldChange, error) {
	changes := make([]FieldChange, 0, 4)
	if before.Schema.Type != after.Schema.Type {
		changes = append(changes, FieldChange{Field: name, Kind: FieldTypeChanged, Detail: "the declared logical type changed"})
	}
	if !equalEnums(before.Schema.Enum, after.Schema.Enum) {
		changes = append(changes, FieldChange{Field: name, Kind: FieldEnumChanged, Detail: "the declared enum values changed"})
	}
	if before.Schema.Nullable != after.Schema.Nullable {
		changes = append(changes, FieldChange{Field: name, Kind: FieldNullabilityChanged, Detail: "the declared nullability changed"})
	}
	if before.Required != after.Required {
		changes = append(changes, FieldChange{Field: name, Kind: FieldRequirednessChanged, Detail: "the declared requiredness changed"})
	}
	if before.Writable != after.Writable {
		changes = append(changes, FieldChange{Field: name, Kind: FieldWritabilityChanged, Detail: "the declared writability changed"})
	}
	if before.Association != after.Association {
		changes = append(changes, FieldChange{Field: name, Kind: FieldAssociationChanged, Detail: "the declared association changed"})
	}
	if before.Precision != after.Precision || before.Scale != after.Scale {
		changes = append(changes, FieldChange{Field: name, Kind: FieldPrecisionChanged, Detail: "the declared precision or scale changed"})
	}
	if len(changes) > 0 {
		return changes, nil
	}
	beforeFingerprint, err := before.Fingerprint()
	if err != nil {
		return nil, err
	}
	afterFingerprint, err := after.Fingerprint()
	if err != nil {
		return nil, err
	}
	if beforeFingerprint != afterFingerprint {
		changes = append(changes, FieldChange{Field: name, Kind: FieldConstraintsChanged, Detail: "the declared constraints changed"})
	}
	return changes, nil
}

func equalEnums(before, after []contract.PresentValue) bool {
	return slices.EqualFunc(before, after, func(left, right contract.PresentValue) bool { return left.Equal(right) })
}

// WriteWitness records the field constraints a saved plan or approval observed,
// so a write revalidates exactly those constraints through the selected
// snapshot before it runs. An empty field set depends on the whole snapshot.
type WriteWitness struct {
	SchemaRef contract.SemanticDigest            `json:"schema_ref"`
	Binding   Binding                            `json:"binding"`
	Fields    map[string]contract.SemanticDigest `json:"field_fingerprints"`
}

func (witness WriteWitness) Validate() error {
	if err := witness.SchemaRef.Validate(); err != nil {
		return errorf(ErrInvalidSnapshot, "witness.schema_ref", "must be a declared content digest")
	}
	if err := witness.Binding.Validate(); err != nil {
		return err
	}
	if witness.Fields == nil {
		return errorf(ErrInvalidSnapshot, "witness.field_fingerprints", "must be declared explicitly, even when empty")
	}
	for name, fingerprint := range witness.Fields {
		if !validBoundedIdentity(name) {
			return errorf(ErrInvalidSnapshot, "witness.field_fingerprints", "must name declared fields")
		}
		if err := fingerprint.Validate(); err != nil {
			return errorf(ErrInvalidSnapshot, "witness.field_fingerprints", "must carry declared field digests")
		}
	}
	return nil
}

// Witness records the observed constraints of the named fields.
func Witness(snapshot Snapshot, fieldNames []string) (WriteWitness, error) {
	if err := snapshot.Validate(); err != nil {
		return WriteWitness{}, err
	}
	witness := WriteWitness{SchemaRef: snapshot.Fingerprint, Binding: snapshot.Binding, Fields: map[string]contract.SemanticDigest{}}
	for _, name := range fieldNames {
		field, present := fieldByName(snapshot.Fields, name)
		if !present {
			return WriteWitness{}, errorf(ErrInvalidSnapshot, "witness."+name, "a saved plan cannot depend on a field the selected snapshot does not declare")
		}
		fingerprint, err := field.Fingerprint()
		if err != nil {
			return WriteWitness{}, err
		}
		witness.Fields[name] = fingerprint
	}
	return witness, nil
}

// RevalidateWrite refuses a write whose observed constraints no longer hold for
// the selected snapshot. Drift of unrelated declarations stays usable.
func RevalidateWrite(witness WriteWitness, active Snapshot) error {
	if err := witness.Validate(); err != nil {
		return err
	}
	if err := active.Validate(); err != nil {
		return err
	}
	if witness.Binding != active.Binding {
		return errorf(ErrSnapshotScope, "witness.binding", "the connection, account, permissions, resource or source revision changed since the plan was accepted")
	}
	if len(witness.Fields) == 0 {
		if witness.SchemaRef != active.Fingerprint {
			return errorf(ErrSchemaDrift, "witness.schema_ref", "the selected snapshot changed since the plan was accepted")
		}
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(witness.Fields)) {
		field, present := fieldByName(active.Fields, name)
		if !present {
			return errorf(ErrSchemaDrift, "witness."+name, "the selected snapshot no longer declares this field")
		}
		fingerprint, err := field.Fingerprint()
		if err != nil {
			return err
		}
		if fingerprint != witness.Fields[name] {
			return errorf(ErrSchemaDrift, "witness."+name, "the declared constraints changed since the plan was accepted")
		}
	}
	return nil
}

func unionFieldNames(active, discovered []Field) []string {
	seen := make(map[string]struct{}, len(active)+len(discovered))
	names := make([]string, 0, len(active)+len(discovered))
	for _, field := range active {
		seen[field.Name] = struct{}{}
		names = append(names, field.Name)
	}
	for _, field := range discovered {
		if _, present := seen[field.Name]; present {
			continue
		}
		seen[field.Name] = struct{}{}
		names = append(names, field.Name)
	}
	slices.Sort(names)
	return names
}

func indexFields(fields []Field) map[string]Field {
	index := make(map[string]Field, len(fields))
	for _, field := range fields {
		index[field.Name] = field
	}
	return index
}

func fieldByName(fields []Field, name string) (Field, bool) {
	for _, field := range fields {
		if field.Name == name {
			return field, true
		}
	}
	return Field{}, false
}
