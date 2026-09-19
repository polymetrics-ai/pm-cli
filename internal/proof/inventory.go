package proof

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/polymetrics-ai/pm-cli/internal/connectors"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const LaneInventorySchemaVersion = "pm.lane-inventory/v1"

var inventoryLanes = [...]contract.Lane{
	contract.LaneDirectRead,
	contract.LaneDirectWrite,
	contract.LaneETL,
	contract.LaneReverseETL,
	contract.LaneBinaryDownload,
	contract.LaneBinaryUpload,
	contract.LaneSyncTransport,
}

// LaneInventory is a generated authoring projection, not execution authority.
// Every compiled operation occurs once, including unavailable operations and
// cells for which no independently authored scenario exists.
type LaneInventory struct {
	SchemaVersion string               `json:"schema_version"`
	ConnectorID   string               `json:"connector_id"`
	Lane          contract.Lane        `json:"lane"`
	SourceDigest  string               `json:"source_digest"`
	BundleHash    contract.SourceHash  `json:"bundle_hash"`
	Operations    []InventoryOperation `json:"operations"`
}

type InventoryOperation struct {
	OperationID string               `json:"operation_id"`
	Status      contract.LaneStatus  `json:"status"`
	Reason      string               `json:"reason"`
	SourceRefs  []contract.SourceRef `json:"source_refs"`
	Cases       []InventoryCase      `json:"cases"`
}

// A scenario identifies one authored behavior/case. There is no release-batch
// identity and no generated expected record. Its assertion and input bytes are
// copied from the authored declaration, never from a provider response.
type InventoryCase struct {
	BehaviorID   string                  `json:"behavior_id"`
	CaseID       string                  `json:"case_id"`
	ScenarioHash contract.SemanticDigest `json:"scenario_hash"`
	Scenario     contract.Scenario       `json:"scenario"`
}

// Inventory derives all seven lanes from one validated execution bundle. A
// source-eligible cell remains unproven until an actual registered public path
// and independent oracle execute; inventory generation cannot certify it.
func Inventory(bundleDocument []byte, scenarios []contract.Scenario) ([]LaneInventory, error) {
	bundle, err := connectors.LoadBundle(bytes.NewReader(bundleDocument))
	if err != nil {
		return nil, fmt.Errorf("read inventory bundle: %w", err)
	}
	type operationCell struct {
		id          string
		sources     []contract.SourceRef
		eligibility []contract.LaneEligibility
	}
	operations := make([]operationCell, 0, len(bundle.Operations)+len(bundle.UnavailableOperations))
	known := make(map[string]struct{}, cap(operations))
	for _, operation := range bundle.Operations {
		operations = append(operations, operationCell{operation.OperationID, operation.SourceRefs, operation.LaneEligibility})
		known[operation.OperationID] = struct{}{}
	}
	for _, operation := range bundle.UnavailableOperations {
		operations = append(operations, operationCell{operation.OperationID, operation.SourceRefs, operation.LaneEligibility})
		known[operation.OperationID] = struct{}{}
	}
	slices.SortFunc(operations, func(left, right operationCell) int {
		return strings.Compare(left.id, right.id)
	})
	type cellKey struct {
		operation string
		lane      contract.Lane
	}
	cases := make(map[cellKey][]InventoryCase)
	seenScenarios := make(map[string]struct{}, len(scenarios))
	for _, scenario := range scenarios {
		if err := scenario.Validate(); err != nil {
			return nil, fmt.Errorf("validate inventory scenario: %w", err)
		}
		if scenario.ConnectorID != bundle.ConnectorID {
			return nil, errors.New("scenario belongs to a different connector")
		}
		if _, exists := known[scenario.OperationID]; !exists {
			return nil, errors.New("scenario operation is absent from the complete bundle inventory")
		}
		if _, duplicate := seenScenarios[scenario.ScenarioID]; duplicate {
			return nil, errors.New("scenario identity is duplicated")
		}
		seenScenarios[scenario.ScenarioID] = struct{}{}
		document, err := json.Marshal(scenario)
		if err != nil {
			return nil, fmt.Errorf("encode inventory scenario: %w", err)
		}
		digest, err := contract.CanonicalDigest(document)
		if err != nil {
			return nil, fmt.Errorf("hash inventory scenario: %w", err)
		}
		var ownedScenario contract.Scenario
		if err := json.Unmarshal(document, &ownedScenario); err != nil {
			return nil, fmt.Errorf("retain inventory scenario: %w", err)
		}
		key := cellKey{scenario.OperationID, scenario.Lane}
		cases[key] = append(cases[key], InventoryCase{
			BehaviorID: scenario.ScenarioID, CaseID: scenario.ScenarioID,
			ScenarioHash: digest, Scenario: ownedScenario,
		})
	}
	for key := range cases {
		slices.SortFunc(cases[key], func(left, right InventoryCase) int {
			return strings.Compare(left.CaseID, right.CaseID)
		})
	}
	bundleHash := contract.RawSourceHash(bundleDocument)
	inventories := make([]LaneInventory, 0, len(inventoryLanes))
	for _, lane := range inventoryLanes {
		inventory := LaneInventory{
			SchemaVersion: LaneInventorySchemaVersion,
			ConnectorID:   bundle.ConnectorID, Lane: lane,
			SourceDigest: bundle.SourceDigest, BundleHash: bundleHash,
			Operations: make([]InventoryOperation, 0, len(operations)),
		}
		for _, operation := range operations {
			var disposition *contract.LaneEligibility
			for index := range operation.eligibility {
				if operation.eligibility[index].Lane == lane {
					disposition = &operation.eligibility[index]
					break
				}
			}
			if disposition == nil {
				return nil, errors.New("validated bundle lacks a required lane disposition")
			}
			ownedCases := cases[cellKey{operation.id, lane}]
			if ownedCases == nil {
				ownedCases = []InventoryCase{}
			}
			inventory.Operations = append(inventory.Operations, InventoryOperation{
				OperationID: operation.id, Status: disposition.Status, Reason: disposition.Reason,
				SourceRefs: slices.Clone(operation.sources), Cases: ownedCases,
			})
		}
		inventories = append(inventories, inventory)
	}
	return inventories, nil
}

// DecodeLaneInventory rejects forged status, missing operation cells and stale
// source/bundle/scenario hashes by re-deriving the complete selected lane. The
// caller supplies its declared document bound before any decoder allocation.
func DecodeLaneInventory(document, bundleDocument []byte, maximumDocumentBytes int64) (LaneInventory, error) {
	if maximumDocumentBytes <= 0 || int64(len(document)) > maximumDocumentBytes {
		return LaneInventory{}, errors.New("lane inventory exceeds its explicit document bound")
	}
	var inventory LaneInventory
	if err := contract.DecodeStrictRequiredObject(document, &inventory,
		"schema_version", "connector_id", "lane", "source_digest", "bundle_hash", "operations"); err != nil {
		return LaneInventory{}, fmt.Errorf("decode lane inventory: %w", err)
	}
	if inventory.SchemaVersion != LaneInventorySchemaVersion || inventory.Operations == nil {
		return LaneInventory{}, errors.New("lane inventory version or operation array is invalid")
	}
	if err := contract.ValidateLane(inventory.Lane); err != nil {
		return LaneInventory{}, err
	}
	scenarios := []contract.Scenario{}
	for _, operation := range inventory.Operations {
		if operation.Cases == nil {
			return LaneInventory{}, errors.New("inventory cases must be an explicit array")
		}
		for _, candidate := range operation.Cases {
			if candidate.Scenario.Lane != inventory.Lane || candidate.Scenario.OperationID != operation.OperationID {
				return LaneInventory{}, errors.New("inventory scenario is bound to a different cell")
			}
			scenarios = append(scenarios, candidate.Scenario)
		}
	}
	expected, err := Inventory(bundleDocument, scenarios)
	if err != nil {
		return LaneInventory{}, err
	}
	actualDigest, err := contract.CanonicalDigest(document)
	if err != nil {
		return LaneInventory{}, err
	}
	for _, candidate := range expected {
		if candidate.Lane != inventory.Lane {
			continue
		}
		encoded, err := json.Marshal(candidate)
		if err != nil {
			return LaneInventory{}, err
		}
		expectedDigest, err := contract.CanonicalDigest(encoded)
		if err != nil {
			return LaneInventory{}, err
		}
		if actualDigest != expectedDigest {
			return LaneInventory{}, errors.New("lane inventory differs from its declared bundle and scenarios")
		}
		return candidate, nil
	}
	return LaneInventory{}, errors.New("lane inventory is not canonical")
}

// EmitLaneArtifacts returns only deterministic generated bytes. The normal
// generator owns their atomic publication alongside its one runtime bundle.
func EmitLaneArtifacts(bundleDocument []byte, scenarios []contract.Scenario) (map[string][]byte, error) {
	inventories, err := Inventory(bundleDocument, scenarios)
	if err != nil {
		return nil, err
	}
	artifacts := make(map[string][]byte, len(inventoryLanes)*2)
	for _, inventory := range inventories {
		document, err := json.MarshalIndent(inventory, "", "  ")
		if err != nil {
			return nil, err
		}
		artifacts["lanes/"+string(inventory.Lane)+".json"] = append(document, '\n')
		wrapper := "#!/bin/sh\nset -eu\nroot=$(CDPATH= cd \"$(dirname \"$0\")/../../..\" && pwd)\nexec \"$root/scripts/prove.sh\" --connector " +
			proofShellWord(inventory.ConnectorID) + " --lane " + proofShellWord(string(inventory.Lane)) + " \"$@\"\n"
		artifacts["scripts/"+string(inventory.Lane)+".sh"] = []byte(wrapper)
	}
	return artifacts, nil
}

func proofShellWord(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
