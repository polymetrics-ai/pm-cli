package contract

import (
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

type EntityKey struct {
	AccountWitness     string `json:"account_witness"`
	Resource           string `json:"resource"`
	ProviderKey        string `json:"provider_key"`
	CreationGeneration string `json:"creation_generation"`
	// providerKeyPresent separates a legitimate empty provider text key from a missing key.
	providerKeyPresent bool
}

func NewEntityKey(accountWitness, resource, providerKey, creationGeneration string) (EntityKey, error) {
	entity := EntityKey{
		AccountWitness:     accountWitness,
		Resource:           resource,
		ProviderKey:        providerKey,
		CreationGeneration: creationGeneration,
		providerKeyPresent: true,
	}
	if err := entity.Validate(); err != nil {
		return EntityKey{}, err
	}
	return entity, nil
}

func (e EntityKey) MarshalJSON() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	type wire struct {
		AccountWitness     string `json:"account_witness"`
		Resource           string `json:"resource"`
		ProviderKey        string `json:"provider_key"`
		CreationGeneration string `json:"creation_generation"`
	}
	return json.Marshal(wire{
		AccountWitness:     e.AccountWitness,
		Resource:           e.Resource,
		ProviderKey:        e.ProviderKey,
		CreationGeneration: e.CreationGeneration,
	})
}

func (e *EntityKey) UnmarshalJSON(data []byte) error {
	type wire struct {
		AccountWitness     *string `json:"account_witness"`
		Resource           *string `json:"resource"`
		ProviderKey        *string `json:"provider_key"`
		CreationGeneration *string `json:"creation_generation"`
	}
	var decoded wire
	if err := decodeContractJSON(data, &decoded); err != nil {
		return err
	}
	if decoded.AccountWitness == nil || decoded.Resource == nil || decoded.ProviderKey == nil || decoded.CreationGeneration == nil {
		return contractError(ErrInvalidIdentity, "entity_key", "must explicitly declare account, resource, provider key, and creation generation")
	}
	value, err := NewEntityKey(*decoded.AccountWitness, *decoded.Resource, *decoded.ProviderKey, *decoded.CreationGeneration)
	if err != nil {
		return err
	}
	*e = value
	return nil
}

func (e EntityKey) Validate() error {
	if !validIdentityPart(e.AccountWitness) {
		return contractError(ErrInvalidIdentity, "entity_key.account_witness", "must be a nonempty canonical provider-account witness")
	}
	if !validIdentityPart(e.Resource) {
		return contractError(ErrInvalidIdentity, "entity_key.resource", "must be a nonempty canonical resource")
	}
	if !e.providerKeyPresent || !utf8.ValidString(e.ProviderKey) {
		return contractError(ErrInvalidIdentity, "entity_key.provider_key", "must be an explicitly present exact UTF-8 provider key")
	}
	if !validIdentityPart(e.CreationGeneration) {
		return contractError(ErrInvalidIdentity, "entity_key.creation_generation", "must be an explicit creation generation")
	}
	return nil
}

type SemanticBinding struct {
	EntityKey      EntityKey      `json:"entity_key"`
	SemanticDigest SemanticDigest `json:"semantic_digest"`
}

func BindSemantics(entity EntityKey, digest SemanticDigest) (SemanticBinding, error) {
	if err := entity.Validate(); err != nil {
		return SemanticBinding{}, err
	}
	if err := digest.Validate(); err != nil {
		return SemanticBinding{}, err
	}
	return SemanticBinding{EntityKey: entity, SemanticDigest: digest}, nil
}

type EffectKey struct {
	EntityKey  EntityKey `json:"entity_key"`
	Occurrence string    `json:"occurrence"`
	Revision   string    `json:"revision"`
}

func NewEffectKey(entity EntityKey, occurrence, revision string) (EffectKey, error) {
	effect := EffectKey{EntityKey: entity, Occurrence: occurrence, Revision: revision}
	if err := effect.Validate(); err != nil {
		return EffectKey{}, err
	}
	return effect, nil
}

func (e EffectKey) Validate() error {
	if err := e.EntityKey.Validate(); err != nil {
		return err
	}
	if !validIdentityPart(e.Occurrence) {
		return contractError(ErrInvalidIdentity, "effect_key.occurrence", "must identify one intended logical action occurrence")
	}
	if e.Revision != "" && !validIdentityPart(e.Revision) {
		return contractError(ErrInvalidIdentity, "effect_key.revision", "must be canonical when present")
	}
	return nil
}

type DispatchID string
type MemberID string
type AttemptID string

func NewDispatchID(value string) (DispatchID, error) {
	if !validIdentityPart(value) {
		return "", contractError(ErrInvalidIdentity, "dispatch_id", "must be a nonempty opaque ID")
	}
	return DispatchID(value), nil
}

func NewMemberID(value string) (MemberID, error) {
	if !validIdentityPart(value) {
		return "", contractError(ErrInvalidIdentity, "member_id", "must be a nonempty opaque ID")
	}
	return MemberID(value), nil
}

func NewAttemptID(value string) (AttemptID, error) {
	if !validIdentityPart(value) {
		return "", contractError(ErrInvalidIdentity, "attempt_id", "must be a nonempty opaque ID")
	}
	return AttemptID(value), nil
}

func (id DispatchID) Validate() error {
	_, err := NewDispatchID(string(id))
	return err
}

func (id MemberID) Validate() error {
	_, err := NewMemberID(string(id))
	return err
}

func (id AttemptID) Validate() error {
	_, err := NewAttemptID(string(id))
	return err
}

type AttemptBinding struct {
	EntityKey      EntityKey      `json:"entity_key"`
	SemanticDigest SemanticDigest `json:"semantic_digest"`
	DispatchID     DispatchID     `json:"dispatch_id"`
	AttemptID      AttemptID      `json:"attempt_id"`
}

func BindAttempt(semantics SemanticBinding, dispatchID DispatchID, attemptID AttemptID) (AttemptBinding, error) {
	if err := semantics.EntityKey.Validate(); err != nil {
		return AttemptBinding{}, err
	}
	if err := semantics.SemanticDigest.Validate(); err != nil {
		return AttemptBinding{}, err
	}
	if err := dispatchID.Validate(); err != nil {
		return AttemptBinding{}, err
	}
	if err := attemptID.Validate(); err != nil {
		return AttemptBinding{}, err
	}
	return AttemptBinding{
		EntityKey:      semantics.EntityKey,
		SemanticDigest: semantics.SemanticDigest,
		DispatchID:     dispatchID,
		AttemptID:      attemptID,
	}, nil
}

func validIdentityPart(value string) bool {
	return validBoundedIdentityPart(value, 512)
}

func validBoundedIdentityPart(value string, maximumBytes int) bool {
	if value == "" || value != strings.TrimSpace(value) || !utf8.ValidString(value) || len(value) > maximumBytes {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}
