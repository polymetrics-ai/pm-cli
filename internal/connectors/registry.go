package connectors

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

var (
	ErrConnectorNotFound = errors.New("connector is not installed")
	ErrOperationNotFound = errors.New("connector operation is not installed")
)

// Registry owns only immutable compiled bundles. It has no source reader,
// compiler, credential resolver, provider client, or execution callback.
type Registry struct {
	revision   contract.SemanticDigest
	connectors map[string]Connector
	ordered    []string
}

func NewRegistry(bundles ...Bundle) (*Registry, error) {
	registry := &Registry{
		connectors: make(map[string]Connector, len(bundles)),
		ordered:    make([]string, 0, len(bundles)),
	}
	canonical := make([]Bundle, 0, len(bundles))
	for _, candidate := range bundles {
		bundle, err := cloneBundle(candidate)
		if err != nil {
			return nil, err
		}
		if _, duplicate := registry.connectors[bundle.ConnectorID]; duplicate {
			return nil, invalidBundle("connector_id %q is duplicated", bundle.ConnectorID)
		}
		registry.connectors[bundle.ConnectorID] = connectorFromBundle(bundle)
		registry.ordered = append(registry.ordered, bundle.ConnectorID)
		canonical = append(canonical, bundle)
	}
	slices.Sort(registry.ordered)
	slices.SortFunc(canonical, func(left, right Bundle) int {
		return strings.Compare(left.ConnectorID, right.ConnectorID)
	})
	document, err := json.Marshal(struct {
		SchemaVersion string   `json:"schema_version"`
		Connectors    []Bundle `json:"connectors"`
	}{SchemaVersion: "pm.catalog/v1", Connectors: canonical})
	if err != nil {
		return nil, fmt.Errorf("encode connector catalog revision: %w", err)
	}
	revision, err := contract.CanonicalDigest(document)
	if err != nil {
		return nil, fmt.Errorf("compute connector catalog revision: %w", err)
	}
	registry.revision = revision
	return registry, nil
}

func (registry *Registry) Revision() contract.SemanticDigest {
	if registry == nil {
		return ""
	}
	return registry.revision
}

func (registry *Registry) Catalog() Catalog {
	if registry == nil {
		return Catalog{Connectors: []Connector{}}
	}
	catalog := Catalog{Revision: registry.revision, Connectors: make([]Connector, 0, len(registry.ordered))}
	for _, connectorID := range registry.ordered {
		catalog.Connectors = append(catalog.Connectors, cloneConnector(registry.connectors[connectorID]))
	}
	return catalog
}

func (registry *Registry) Connector(connectorID string) (Connector, error) {
	if registry == nil || !validIdentifier(connectorID, 512) {
		return Connector{}, ErrConnectorNotFound
	}
	connector, present := registry.connectors[connectorID]
	if !present {
		return Connector{}, ErrConnectorNotFound
	}
	return cloneConnector(connector), nil
}

func (registry *Registry) Operation(connectorID, operationID string) (Operation, error) {
	connector, err := registry.Connector(connectorID)
	if err != nil {
		return Operation{}, err
	}
	for _, operation := range connector.Operations {
		if operation.Contract.OperationID == operationID {
			return operation, nil
		}
	}
	return Operation{}, ErrOperationNotFound
}
