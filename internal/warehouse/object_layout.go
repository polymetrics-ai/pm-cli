package warehouse

import (
	"errors"
	"fmt"
	"strings"
)

// The warehouse namespace is structural, not textual. Every object this package
// publishes is addressed by a dataset identity that carries the workspace scope
// (through the store's own scoping), the connector, the connection, and the
// dataset. Components are deliberately narrow so that two different identities
// can never render the same dataset or segment identity: an unsafe component is
// refused rather than normalised, and a layout without the canonical version
// tag is refused rather than guessed.
const (
	// WarehouseNamespaceVersion is the only admitted layout version.
	WarehouseNamespaceVersion = "wh1"
	// MaximumWarehouseComponentBytes bounds one namespace component.
	MaximumWarehouseComponentBytes = 64
	// warehouse separator values. A component may not contain either, so the
	// rendering is injective by construction.
	warehouseComponentSeparator = "."
	warehouseSegmentInfix       = "s"
	warehouseReservationPrefix  = "whr"
)

var (
	// ErrObjectLayout reports a namespace component that cannot belong to a
	// canonical warehouse object identity.
	ErrObjectLayout = errors.New("warehouse object namespace is invalid")
	// ErrAmbiguousLayout reports an identity that is not in the canonical
	// versioned form. It is never normalised into one.
	ErrAmbiguousLayout = errors.New("warehouse dataset identity is ambiguous")
)

// WarehouseLayout is the canonical structural identity of one warehouse dataset:
// which connector, which connection inside the caller's workspace scope, and
// which dataset. A dataset is never addressable without its connection, so a
// cross-connection read is a namespace violation rather than a lookup detail.
type WarehouseLayout struct {
	ConnectorID  string
	ConnectionID string
	Dataset      string
}

// NewWarehouseLayout validates one dataset identity before it can name an
// object, a manifest resource, or a registered relation.
func NewWarehouseLayout(connectorID, connectionID, dataset string) (WarehouseLayout, error) {
	layout := WarehouseLayout{ConnectorID: connectorID, ConnectionID: connectionID, Dataset: dataset}
	if err := layout.Validate(); err != nil {
		return WarehouseLayout{}, err
	}
	return layout, nil
}

// Validate refuses an unsafe or ambiguous namespace component.
func (layout WarehouseLayout) Validate() error {
	for _, component := range []struct {
		name  string
		value string
	}{
		{"connector", layout.ConnectorID},
		{"connection", layout.ConnectionID},
		{"dataset", layout.Dataset},
	} {
		if err := validateWarehouseComponent(component.value); err != nil {
			return fmt.Errorf("%w: %s %s", ErrObjectLayout, component.name, err)
		}
	}
	return nil
}

// DatasetID is the manifest resource identity of this dataset. It is injective:
// no two distinct layouts render the same identity, and a component carrying a
// separator, a parent reference, or a path character is refused when the layout
// is composed.
func (layout WarehouseLayout) DatasetID() string {
	return strings.Join([]string{
		WarehouseNamespaceVersion, layout.ConnectorID, layout.ConnectionID, layout.Dataset,
	}, warehouseComponentSeparator)
}

// RelationName is the relation a registered query addresses. It is the dataset
// component, so a query can only name the dataset it was admitted for.
func (layout WarehouseLayout) RelationName() string { return layout.Dataset }

// SegmentArtifactID is the artifact identity of one publication attempt. The
// attempt identity makes an exact replay resolvable before any new object I/O,
// and the dataset identity keeps two datasets' segments apart.
func (layout WarehouseLayout) SegmentArtifactID(attemptID string) (string, error) {
	if err := layout.Validate(); err != nil {
		return "", err
	}
	if err := validateWarehouseComponent(attemptID); err != nil {
		return "", fmt.Errorf("%w: segment attempt %s", ErrObjectLayout, err)
	}
	return strings.Join([]string{
		layout.DatasetID(), warehouseSegmentInfix, attemptID,
	}, warehouseComponentSeparator), nil
}

// SegmentReservationID is the scoped capacity reservation of one publication
// attempt. It follows the same structure so an operator can correlate a
// retained attempt with the dataset that owns it.
func (layout WarehouseLayout) SegmentReservationID(attemptID string) (string, error) {
	artifactID, err := layout.SegmentArtifactID(attemptID)
	if err != nil {
		return "", err
	}
	return warehouseReservationPrefix + warehouseComponentSeparator + artifactID, nil
}

// ParseWarehouseDatasetID reads back one canonical dataset identity. A legacy or
// unversioned form is refused as ambiguous: it is never normalised into the
// versioned namespace, because two different historic layouts could render the
// same text.
func ParseWarehouseDatasetID(datasetID string) (WarehouseLayout, error) {
	components := strings.Split(datasetID, warehouseComponentSeparator)
	if len(components) != 4 || components[0] != WarehouseNamespaceVersion {
		return WarehouseLayout{}, fmt.Errorf("%w: %q is not a %s dataset identity",
			ErrAmbiguousLayout, datasetID, WarehouseNamespaceVersion)
	}
	layout := WarehouseLayout{ConnectorID: components[1], ConnectionID: components[2], Dataset: components[3]}
	if err := layout.Validate(); err != nil {
		return WarehouseLayout{}, fmt.Errorf("%w: %q", ErrAmbiguousLayout, datasetID)
	}
	return layout, nil
}

// validateWarehouseComponent admits one lowercase structural component. A
// component may not be empty, may not carry a separator, a parent reference, a
// path character, control text, or any character outside the closed set.
func validateWarehouseComponent(value string) error {
	switch {
	case value == "":
		return errors.New("must not be empty")
	case len(value) > MaximumWarehouseComponentBytes:
		return fmt.Errorf("must not exceed %d bytes", MaximumWarehouseComponentBytes)
	case value == "." || value == "..":
		return errors.New("must not be a directory reference")
	}
	for index, character := range value {
		valid := character >= 'a' && character <= 'z' || character >= '0' && character <= '9' ||
			character == '-' || character == '_'
		if !valid {
			return errors.New("must use only lowercase letters, digits, '-' and '_'")
		}
		if index == 0 && (character == '-' || character == '_') {
			return errors.New("must start with a lowercase letter or a digit")
		}
	}
	return nil
}
