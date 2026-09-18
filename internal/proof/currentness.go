package proof

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type Mode string

const (
	ModeRetry         Mode = "retry"
	ModeAll           Mode = "all"
	ModeUnimplemented Mode = "unimplemented"
)

type CaseIdentity struct {
	ConnectorID   string                  `json:"connector_id"`
	Lane          contract.Lane           `json:"lane"`
	OperationID   string                  `json:"operation_id"`
	BehaviorID    string                  `json:"behavior_id"`
	CaseID        string                  `json:"case_id"`
	ExecutionKind string                  `json:"execution_kind"`
	ScopeHash     contract.SemanticDigest `json:"scope_hash"`
}

type Currentness struct {
	Identity              CaseIdentity
	SourceHashes          []contract.SourceHash
	BundleHash            contract.SourceHash
	SchemaHash            contract.SourceHash
	PMClientHash          contract.SourceHash
	PMAPIHash             contract.SourceHash
	PMWorkerHash          contract.SourceHash
	AssertionHash         contract.SemanticDigest
	QueryHelperHash       *contract.SourceHash
	HandlerVersions       map[string]string
	ImplementationClosure contract.ImplementationClosure
}

type CaseCandidate struct {
	Current      Currentness
	Availability contract.LaneStatus
	Reason       string
}

type ReceiptRecord struct {
	Sequence     uint64                  `json:"sequence"`
	ID           contract.SemanticDigest `json:"id"`
	Availability contract.LaneStatus     `json:"availability"`
	Receipt      contract.ProofReceipt   `json:"receipt"`
}

type CaseDecision struct {
	Candidate CaseCandidate
	Execute   bool
	Receipt   *ReceiptRecord
	Reason    string
	Reused    bool
}

func ParseMode(value string) (Mode, error) {
	if value == "" {
		return ModeRetry, nil
	}
	switch Mode(value) {
	case ModeRetry, ModeAll, ModeUnimplemented:
		return Mode(value), nil
	default:
		return "", fmt.Errorf("proof selection mode %q is unsupported", value)
	}
}

func CaseKey(identity CaseIdentity) (string, error) {
	if err := validateCaseIdentity(identity); err != nil {
		return "", err
	}
	document, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("encode proof case identity: %w", err)
	}
	digest, err := contract.CanonicalDigest(document)
	if err != nil {
		return "", fmt.Errorf("digest proof case identity: %w", err)
	}
	return string(digest), nil
}

func IsCurrent(receipt contract.ProofReceipt, current Currentness) (bool, error) {
	if err := validateCurrentness(current); err != nil {
		return false, err
	}
	if err := receipt.Validate(); err != nil {
		return false, fmt.Errorf("validate proof receipt: %w", err)
	}
	if !proofPassed(receipt.Status) ||
		(receipt.Provenance.Reused != nil && *receipt.Provenance.Reused) {
		return false, nil
	}
	identity, err := receiptCaseIdentity(receipt)
	if err != nil {
		return false, nil
	}
	if identity != current.Identity {
		return false, nil
	}
	if !sameSourceSet(receipt.SourceHashes, current.SourceHashes) ||
		!sameRequiredSourceHash(receipt.BundleHash, current.BundleHash) ||
		!sameRequiredSourceHash(receipt.SchemaHash, current.SchemaHash) ||
		!sameRequiredSourceHash(receipt.PMClientHash, current.PMClientHash) ||
		!sameRequiredSourceHash(receipt.PMAPIHash, current.PMAPIHash) ||
		!sameRequiredSourceHash(receipt.PMWorkerHash, current.PMWorkerHash) ||
		receipt.AssertionHash != current.AssertionHash ||
		!sameOptionalSourceHash(receipt.QueryHelperHashIfUsed, current.QueryHelperHash) ||
		!sameStringMap(receipt.HandlerVersions, current.HandlerVersions) {
		return false, nil
	}
	if receipt.ImplementationClosure == nil ||
		!receipt.ImplementationClosure.Known || receipt.ImplementationClosure.Digest == nil ||
		!current.ImplementationClosure.Known || current.ImplementationClosure.Digest == nil {
		return false, nil
	}
	return *receipt.ImplementationClosure.Digest == *current.ImplementationClosure.Digest, nil
}

func SelectCases(mode Mode, candidates []CaseCandidate, latest map[string]ReceiptRecord) ([]CaseDecision, error) {
	parsedMode, err := ParseMode(string(mode))
	if err != nil {
		return nil, err
	}

	keys := make([]string, len(candidates))
	candidateKeys := make(map[string]struct{}, len(candidates))
	for index, candidate := range candidates {
		if candidate.Availability == contract.LaneEligible {
			if err := validateCurrentness(candidate.Current); err != nil {
				return nil, fmt.Errorf("candidate %d currentness: %w", index, err)
			}
		} else if err := validateCaseIdentity(candidate.Current.Identity); err != nil {
			return nil, fmt.Errorf("candidate %d identity: %w", index, err)
		}
		if !validLaneStatus(candidate.Availability) {
			return nil, fmt.Errorf("candidate %d availability is unsupported", index)
		}
		if !validReason(candidate.Reason) {
			return nil, fmt.Errorf("candidate %d reason is invalid", index)
		}
		key, err := CaseKey(candidate.Current.Identity)
		if err != nil {
			return nil, fmt.Errorf("candidate %d identity: %w", index, err)
		}
		if _, duplicate := candidateKeys[key]; duplicate {
			return nil, fmt.Errorf("candidate %d duplicates a proof case identity", index)
		}
		candidateKeys[key] = struct{}{}
		keys[index] = key
	}

	for key, record := range latest {
		if _, expected := candidateKeys[key]; !expected {
			return nil, fmt.Errorf("latest receipt map contains a foreign case key")
		}
		if err := validateReceiptRecord(record); err != nil {
			return nil, fmt.Errorf("latest receipt %q: %w", key, err)
		}
		identity, err := receiptCaseIdentity(record.Receipt)
		if err != nil {
			return nil, fmt.Errorf("latest receipt %q identity: %w", key, err)
		}
		recordKey, err := CaseKey(identity)
		if err != nil || recordKey != key {
			return nil, fmt.Errorf("latest receipt map key does not match its exact case identity")
		}
	}

	decisions := make([]CaseDecision, 0, len(candidates))
	for index, candidate := range candidates {
		decision := CaseDecision{Candidate: candidate, Reason: candidate.Reason}
		record, found := latest[keys[index]]
		if found {
			copyOfRecord := record
			decision.Receipt = &copyOfRecord
		}

		if candidate.Availability != contract.LaneEligible {
			if found && record.Availability != candidate.Availability {
				decision.Receipt = nil
			}
			decisions = append(decisions, decision)
			continue
		}

		currentPass := false
		if found && record.Availability == contract.LaneEligible {
			currentPass, err = IsCurrent(record.Receipt, candidate.Current)
			if err != nil {
				return nil, fmt.Errorf("candidate %d latest receipt currentness: %w", index, err)
			}
		}

		switch parsedMode {
		case ModeAll:
			decision.Execute = true
			decision.Reason = "all mode requires fresh execution"
		case ModeRetry:
			if currentPass {
				decision.Reused = true
				decision.Reason = "latest passing receipt is current"
			} else {
				decision.Execute = true
				if !found {
					decision.Reason = "proof receipt is missing"
				} else if proofPassed(record.Receipt.Status) {
					decision.Reason = "latest passing receipt is not current"
				} else {
					decision.Reason = "latest receipt did not pass"
				}
			}
		case ModeUnimplemented:
			switch {
			case !found:
				decision.Execute = true
				decision.Reason = "eligible case has no proof receipt"
			case record.Availability != contract.LaneEligible:
				decision.Execute = true
				decision.Reason = "case became eligible"
			case currentPass:
				decision.Reused = true
				decision.Reason = "latest passing receipt is current"
			default:
				if proofPassed(record.Receipt.Status) {
					decision.Receipt = nil
				}
				decision.Reason = "existing eligible case is outside unimplemented execution"
			}
		}
		decisions = append(decisions, decision)
	}
	return decisions, nil
}

func validateCurrentness(current Currentness) error {
	if err := validateCaseIdentity(current.Identity); err != nil {
		return err
	}
	if len(current.SourceHashes) == 0 {
		return fmt.Errorf("current source hash set is empty")
	}
	seenSources := make(map[contract.SourceHash]struct{}, len(current.SourceHashes))
	for _, hash := range current.SourceHashes {
		if err := hash.Validate(); err != nil {
			return fmt.Errorf("current source hash is invalid: %w", err)
		}
		if _, duplicate := seenSources[hash]; duplicate {
			return fmt.Errorf("current source hash set contains a duplicate")
		}
		seenSources[hash] = struct{}{}
	}
	for name, hash := range map[string]contract.SourceHash{
		"bundle":    current.BundleHash,
		"schema":    current.SchemaHash,
		"pm client": current.PMClientHash,
		"pm api":    current.PMAPIHash,
		"pm worker": current.PMWorkerHash,
	} {
		if err := hash.Validate(); err != nil {
			return fmt.Errorf("current %s hash is invalid: %w", name, err)
		}
	}
	if err := current.AssertionHash.Validate(); err != nil {
		return fmt.Errorf("current assertion hash is invalid: %w", err)
	}
	if current.QueryHelperHash != nil {
		if err := current.QueryHelperHash.Validate(); err != nil {
			return fmt.Errorf("current query helper hash is invalid: %w", err)
		}
	}
	if len(current.HandlerVersions) == 0 {
		return fmt.Errorf("current handler versions are empty")
	}
	for name, version := range current.HandlerVersions {
		if !validIdentityPart(name) || !validIdentityPart(version) {
			return fmt.Errorf("current handler version is invalid")
		}
	}
	if err := validateImplementationClosure(current.ImplementationClosure); err != nil {
		return fmt.Errorf("current implementation closure: %w", err)
	}
	return nil
}

func validateImplementationClosure(closure contract.ImplementationClosure) error {
	if closure.Known {
		if closure.Digest == nil {
			return fmt.Errorf("known closure requires a digest")
		}
		if err := closure.Digest.Validate(); err != nil {
			return err
		}
		return nil
	}
	if closure.Digest != nil {
		return fmt.Errorf("unknown closure cannot claim a digest")
	}
	return nil
}

func validateCaseIdentity(identity CaseIdentity) error {
	for name, value := range map[string]string{
		"connector": identity.ConnectorID,
		"operation": identity.OperationID,
		"behavior":  identity.BehaviorID,
		"case":      identity.CaseID,
	} {
		if !validIdentityPart(value) {
			return fmt.Errorf("proof case %s identity is invalid", name)
		}
	}
	if err := contract.ValidateLane(identity.Lane); err != nil {
		return fmt.Errorf("proof case lane is invalid: %w", err)
	}
	if identity.ExecutionKind != "fake" && identity.ExecutionKind != "live" {
		return fmt.Errorf("proof case execution kind must be fake or live")
	}
	if err := identity.ScopeHash.Validate(); err != nil {
		return fmt.Errorf("proof case scope hash is invalid: %w", err)
	}
	return nil
}

func receiptCaseIdentity(receipt contract.ProofReceipt) (CaseIdentity, error) {
	if receipt.WorkspaceAccountScopeHash == nil {
		return CaseIdentity{}, fmt.Errorf("proof receipt has no account-workspace scope hash")
	}
	identity := CaseIdentity{
		ConnectorID:   receipt.ConnectorID,
		Lane:          receipt.Lane,
		OperationID:   receipt.OperationID,
		BehaviorID:    receipt.BehaviorID,
		CaseID:        receipt.CaseID,
		ExecutionKind: receipt.Provenance.ExecutionKind,
		ScopeHash:     *receipt.WorkspaceAccountScopeHash,
	}
	if err := validateCaseIdentity(identity); err != nil {
		return CaseIdentity{}, err
	}
	return identity, nil
}

func validateReceiptRecord(record ReceiptRecord) error {
	if record.Sequence == 0 {
		return fmt.Errorf("receipt sequence must be positive")
	}
	if !validLaneStatus(record.Availability) {
		return fmt.Errorf("receipt availability is unsupported")
	}
	if err := record.ID.Validate(); err != nil {
		return fmt.Errorf("receipt ID is invalid: %w", err)
	}
	if record.Receipt.Provenance.Reused != nil && *record.Receipt.Provenance.Reused {
		return fmt.Errorf("physical receipt cannot be marked reused")
	}
	digest, err := proofReceiptDigest(record.Receipt)
	if err != nil {
		return err
	}
	if digest != record.ID {
		return fmt.Errorf("receipt ID does not match its immutable contents")
	}
	if proofPassed(record.Receipt.Status) && record.Availability != contract.LaneEligible {
		return fmt.Errorf("unavailable case cannot carry passing proof")
	}
	_, err = receiptCaseIdentity(record.Receipt)
	return err
}

func proofReceiptDigest(receipt contract.ProofReceipt) (contract.SemanticDigest, error) {
	if err := receipt.Validate(); err != nil {
		return "", fmt.Errorf("validate proof receipt: %w", err)
	}
	document, err := json.Marshal(receipt)
	if err != nil {
		return "", fmt.Errorf("encode proof receipt: %w", err)
	}
	digest, err := contract.CanonicalDigest(document)
	if err != nil {
		return "", fmt.Errorf("digest proof receipt: %w", err)
	}
	return digest, nil
}

func proofPassed(status contract.ProofStatus) bool {
	return status == contract.ProofPassedFake || status == contract.ProofPassedLive
}

func validLaneStatus(status contract.LaneStatus) bool {
	switch status {
	case contract.LaneEligible, contract.LaneUnsupported, contract.LaneNotApplicable,
		contract.LaneBlocked, contract.LaneUnproven:
		return true
	default:
		return false
	}
}

func validIdentityPart(value string) bool {
	return value != "" && len(value) <= 1024 && utf8.ValidString(value) && strings.TrimSpace(value) == value
}

func validReason(value string) bool {
	return value != "" && len(value) <= 4096 && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsRune(value, '\x00')
}

func sameSourceSet(left, right []contract.SourceHash) bool {
	if len(left) != len(right) {
		return false
	}
	members := make(map[contract.SourceHash]struct{}, len(left))
	for _, hash := range left {
		members[hash] = struct{}{}
	}
	if len(members) != len(left) {
		return false
	}
	for _, hash := range right {
		if _, present := members[hash]; !present {
			return false
		}
	}
	return true
}

func sameRequiredSourceHash(actual *contract.SourceHash, expected contract.SourceHash) bool {
	return actual != nil && *actual == expected
}

func sameOptionalSourceHash(left, right *contract.SourceHash) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func sameStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}
