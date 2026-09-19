// Package schema owns account metadata discovery coordination, normalization,
// immutable scoped snapshots, typed effective schemas and compatibility
// predicates. A snapshot is durable state of one connection: it lives under the
// connection's owned state directory, and the connection's config.json owns the
// sole schema_ref selection. There is no second metadata catalog, global
// CURRENT or mirrored active selector.
package schema

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

const maxGapDetailBytes = 512

// GapKind identifies why a declared metadata fact is not usable as typed data.
type GapKind string

const (
	// GapCapabilityUnavailable records a declared metadata capability that did
	// not report the bound resource.
	GapCapabilityUnavailable GapKind = "capability_unavailable"
	// GapTruncatedMetadata records a declaration whose page continuation was
	// not exhausted, so the discovery is partial.
	GapTruncatedMetadata GapKind = "truncated_metadata"
	// GapPermissionDenied records a declared permission the account was denied.
	GapPermissionDenied GapKind = "permission_denied"
	// GapUnsupportedShape records one declared shape this foundation cannot
	// carry. It stays explicit without suppressing supported declarations.
	GapUnsupportedShape GapKind = "unsupported_shape"
	// GapConflictingField records two declarations of one field that disagree,
	// so neither declaration may be chosen silently.
	GapConflictingField GapKind = "conflicting_field"
	// GapRequirementUnavailable records a metadata capability or the exact
	// installed behavioral revision a cell requires.
	GapRequirementUnavailable GapKind = "requirement_unavailable"
	// GapInvalidOverlay records a predicate declaration that is invalid and
	// therefore never evaluated.
	GapInvalidOverlay GapKind = "invalid_overlay"
	// GapPredicateUnsatisfied records an evaluated declared predicate that is
	// false for the selected metadata.
	GapPredicateUnsatisfied GapKind = "predicate_unsatisfied"
)

func (kind GapKind) Valid() bool {
	switch kind {
	case GapCapabilityUnavailable, GapTruncatedMetadata, GapPermissionDenied, GapUnsupportedShape,
		GapConflictingField, GapRequirementUnavailable, GapInvalidOverlay, GapPredicateUnsatisfied:
		return true
	default:
		return false
	}
}

// Blocking reports whether this gap makes a discovery partial. An unsupported
// shape or a failed predicate is explicit but leaves the supported
// declarations around it usable.
func (kind GapKind) Blocking() bool {
	switch kind {
	case GapCapabilityUnavailable, GapTruncatedMetadata, GapPermissionDenied, GapConflictingField:
		return true
	default:
		return false
	}
}

// Gap is one explicit unavailability or conflict. Its identity derives from its
// kind and subject, so the same gap is reported once for every affected cell.
// Detail carries a declared identity or a fixed reason, never provider payload.
type Gap struct {
	ID      string  `json:"id"`
	Kind    GapKind `json:"kind"`
	Subject string  `json:"subject"`
	Detail  string  `json:"detail"`
}

func NewGap(kind GapKind, subject, detail string) (Gap, error) {
	if !kind.Valid() {
		return Gap{}, errorf(ErrInvalidGap, "kind", "is not a declared gap kind")
	}
	if !validBoundedIdentity(subject) {
		return Gap{}, errorf(ErrInvalidGap, "subject", "must be a canonical bounded identity")
	}
	if detail == "" || len(detail) > maxGapDetailBytes {
		return Gap{}, errorf(ErrInvalidGap, "detail", "must carry a bounded reason")
	}
	return Gap{ID: string(kind) + ":" + subject, Kind: kind, Subject: subject, Detail: detail}, nil
}

func (gap Gap) Validate() error {
	expected, err := NewGap(gap.Kind, gap.Subject, gap.Detail)
	if err != nil {
		return err
	}
	if gap.ID != expected.ID {
		return errorf(ErrInvalidGap, "id", "does not match its kind and subject")
	}
	return nil
}

// Completeness is the explicit result of one declared discovery. Complete is
// false when any blocking gap exists, so a partial or failed discovery can
// never be mistaken for a complete account snapshot.
type Completeness struct {
	Complete bool  `json:"complete"`
	Gaps     []Gap `json:"gaps"`
}

func (completeness Completeness) Validate() error {
	if completeness.Gaps == nil {
		return errorf(ErrInvalidSnapshot, "completeness.gaps", "must be declared explicitly, even when empty")
	}
	if len(completeness.Gaps) > MaxSnapshotGaps {
		return errorf(ErrInvalidSnapshot, "completeness.gaps", "exceeds the supported gap count")
	}
	for _, gap := range completeness.Gaps {
		if err := gap.Validate(); err != nil {
			return err
		}
	}
	deduplicated, err := deduplicateGaps(completeness.Gaps)
	if err != nil {
		return err
	}
	if len(deduplicated) != len(completeness.Gaps) {
		return errorf(ErrInvalidSnapshot, "completeness.gaps", "reports the same gap more than once")
	}
	for _, gap := range completeness.Gaps {
		if completeness.Complete && gap.Kind.Blocking() {
			return errorf(ErrInvalidSnapshot, "completeness.complete", "a blocking gap cannot be reported as complete")
		}
	}
	return nil
}

// deduplicateGaps collapses identical gaps and refuses one identity that
// carries two different reasons, which would otherwise hide a defect.
func deduplicateGaps(gaps []Gap) ([]Gap, error) {
	if gaps == nil {
		return nil, nil
	}
	sorted := slices.Clone(gaps)
	slices.SortFunc(sorted, func(left, right Gap) int {
		if left.ID != right.ID {
			return strings.Compare(left.ID, right.ID)
		}
		return strings.Compare(left.Detail, right.Detail)
	})
	deduplicated := make([]Gap, 0, len(sorted))
	for _, gap := range sorted {
		if len(deduplicated) == 0 {
			deduplicated = append(deduplicated, gap)
			continue
		}
		previous := deduplicated[len(deduplicated)-1]
		if previous.ID != gap.ID {
			deduplicated = append(deduplicated, gap)
			continue
		}
		if previous.Kind != gap.Kind || previous.Subject != gap.Subject || previous.Detail != gap.Detail {
			return nil, errorf(ErrInvalidGap, "id", "one gap identity carries two different reasons")
		}
	}
	return deduplicated, nil
}

// assessGaps derives explicit completeness from one deduplicated gap set.
func assessGaps(gaps []Gap) (Completeness, error) {
	deduplicated, err := deduplicateGaps(gaps)
	if err != nil {
		return Completeness{}, err
	}
	if deduplicated == nil {
		deduplicated = []Gap{}
	}
	complete := true
	for _, gap := range deduplicated {
		if gap.Kind.Blocking() {
			complete = false
			break
		}
	}
	completeness := Completeness{Complete: complete, Gaps: deduplicated}
	if err := completeness.Validate(); err != nil {
		return Completeness{}, err
	}
	return completeness, nil
}

// AssessCompleteness reports what the declared metadata observations establish.
// Each observation is a declared metadata capability result, never a guess
// derived from sample rows.
func AssessCompleteness(observations []Observation) (Completeness, error) {
	if len(observations) == 0 {
		return Completeness{}, errorf(ErrInvalidDiscovery, "observations", "at least one declared metadata observation is required")
	}
	gaps := make([]Gap, 0, len(observations))
	for index, observation := range observations {
		if err := observation.validate(index); err != nil {
			return Completeness{}, err
		}
		if !observation.Available {
			gap, err := NewGap(GapCapabilityUnavailable, observation.Capability, "the declared metadata capability did not report the bound resource")
			if err != nil {
				return Completeness{}, err
			}
			gaps = append(gaps, gap)
		}
		if observation.Truncated {
			gap, err := NewGap(GapTruncatedMetadata, observation.Capability, "the declared page continuation was not exhausted")
			if err != nil {
				return Completeness{}, err
			}
			gaps = append(gaps, gap)
		}
		for _, permission := range observation.DeniedPermissions {
			gap, err := NewGap(GapPermissionDenied, permission, "the declared permission was denied while reading metadata")
			if err != nil {
				return Completeness{}, err
			}
			gaps = append(gaps, gap)
		}
		for _, shape := range observation.Unsupported {
			gap, err := NewGap(GapUnsupportedShape, shape, "the declared metadata shape is unsupported and stays explicit")
			if err != nil {
				return Completeness{}, err
			}
			gaps = append(gaps, gap)
		}
	}
	return assessGaps(gaps)
}

var (
	// ErrInvalidGap reports a gap declaration that cannot be trusted.
	ErrInvalidGap = errors.New("invalid schema gap")
)

func errorf(kind error, path, detail string) error {
	return fmt.Errorf("%w: %s: %s", kind, path, detail)
}
