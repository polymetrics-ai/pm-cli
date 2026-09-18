package connectors

import (
	"bytes"
	"encoding/json"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// Catalog is an immutable snapshot of installed runtime bundles. Every value
// returned from it is detached from registry storage.
type Catalog struct {
	Revision   contract.SemanticDigest
	Connectors []Connector
}

type Connector struct {
	ConnectorID           string
	SourceDigest          contract.SemanticDigest
	SemanticsRevision     contract.SemanticDigest
	Operations            []Operation
	UnavailableOperations []contract.UnavailableOperation
}

type Operation struct {
	Contract contract.Operation
	Command  *contract.Command
}

func connectorFromBundle(bundle Bundle) Connector {
	commands := make(map[string]contract.Command, len(bundle.Commands))
	for _, command := range bundle.Commands {
		commands[command.OperationID] = command
	}
	connector := Connector{
		ConnectorID:           bundle.ConnectorID,
		SourceDigest:          contract.SemanticDigest(bundle.SourceDigest),
		SemanticsRevision:     contract.SemanticDigest(bundle.SemanticsRevision),
		Operations:            make([]Operation, 0, len(bundle.Operations)),
		UnavailableOperations: append([]contract.UnavailableOperation{}, bundle.UnavailableOperations...),
	}
	for _, operation := range bundle.Operations {
		projected := Operation{Contract: operation}
		if command, present := commands[operation.OperationID]; present {
			copy := command
			projected.Command = &copy
		}
		connector.Operations = append(connector.Operations, projected)
	}
	return cloneConnector(connector)
}

func cloneConnector(connector Connector) Connector {
	// Connector contracts contain nested maps, slices, pointers, and typed exact
	// values. A JSON round trip is the single maintained deep-copy boundary and
	// retains their canonical wire representation.
	document, err := json.Marshal(connectorWire(connector))
	if err != nil {
		panic("validated connector catalog failed to encode: " + err.Error())
	}
	var bundle Bundle
	if err := contract.DecodeStrictRequiredObject(
		document,
		&bundle,
		"schema_version",
		"connector_id",
		"source_digest",
		"semantics_revision",
		"operations",
		"commands",
		"unavailable_operations",
	); err != nil {
		panic("validated connector catalog failed to clone: " + err.Error())
	}
	return connectorFromValidatedBundle(bundle)
}

func connectorFromValidatedBundle(bundle Bundle) Connector {
	commands := make(map[string]contract.Command, len(bundle.Commands))
	for _, command := range bundle.Commands {
		commands[command.OperationID] = command
	}
	connector := Connector{
		ConnectorID:           bundle.ConnectorID,
		SourceDigest:          contract.SemanticDigest(bundle.SourceDigest),
		SemanticsRevision:     contract.SemanticDigest(bundle.SemanticsRevision),
		Operations:            make([]Operation, 0, len(bundle.Operations)),
		UnavailableOperations: bundle.UnavailableOperations,
	}
	for _, operation := range bundle.Operations {
		item := Operation{Contract: operation}
		if command, present := commands[operation.OperationID]; present {
			copy := command
			item.Command = &copy
		}
		connector.Operations = append(connector.Operations, item)
	}
	return connector
}

func connectorWire(connector Connector) Bundle {
	commands := make([]contract.Command, 0, len(connector.Operations))
	operations := make([]contract.Operation, 0, len(connector.Operations))
	for _, item := range connector.Operations {
		operations = append(operations, item.Contract)
		if item.Command != nil {
			commands = append(commands, *item.Command)
		}
	}
	return Bundle{
		SchemaVersion:         BundleSchemaVersion,
		ConnectorID:           connector.ConnectorID,
		SourceDigest:          string(connector.SourceDigest),
		SemanticsRevision:     string(connector.SemanticsRevision),
		Operations:            operations,
		Commands:              commands,
		UnavailableOperations: connector.UnavailableOperations,
	}
}

func cloneBundle(bundle Bundle) (Bundle, error) {
	document, err := json.Marshal(bundle)
	if err != nil {
		return Bundle{}, err
	}
	return LoadBundle(bytes.NewReader(document))
}
