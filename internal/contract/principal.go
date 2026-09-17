package contract

type PrincipalKind string

const (
	PrincipalUser           PrincipalKind = "user"
	PrincipalServiceAccount PrincipalKind = "service_account"
)

// Principal identifies a human or organization-owned automation identity. A
// workspace is deliberately not inferred from the principal.
type Principal struct {
	OrgID       string        `json:"org_id"`
	PrincipalID string        `json:"principal_id"`
	Kind        PrincipalKind `json:"kind"`
}

func (p *Principal) UnmarshalJSON(data []byte) error {
	type wire Principal
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "org_id", "principal_id", "kind"); err != nil {
		return err
	}
	value := Principal(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*p = value
	return nil
}

func (p Principal) Validate() error {
	if !validIdentityPart(p.OrgID) {
		return contractError(ErrInvalidIdentity, "principal.org_id", "must identify one organization")
	}
	if !validIdentityPart(p.PrincipalID) {
		return contractError(ErrInvalidIdentity, "principal.principal_id", "must be a nonempty opaque identity")
	}
	switch p.Kind {
	case PrincipalUser, PrincipalServiceAccount:
	default:
		return contractError(ErrInvalidIdentity, "principal.kind", "must be user or service_account")
	}
	return nil
}

func (p Principal) Equal(other Principal) bool {
	return p.OrgID == other.OrgID && p.PrincipalID == other.PrincipalID && p.Kind == other.Kind
}

// Delegation records the sole permitted delegation hop and the immutable
// parent ceiling against which the execution grant is checked.
type Delegation struct {
	DelegationID   string       `json:"delegation_id"`
	ParentGrantRef string       `json:"parent_grant_ref"`
	Hop            uint8        `json:"hop"`
	ParentCeiling  GrantCeiling `json:"parent_ceiling"`
}

func (d *Delegation) UnmarshalJSON(data []byte) error {
	type wire Delegation
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "delegation_id", "parent_grant_ref", "hop", "parent_ceiling"); err != nil {
		return err
	}
	value := Delegation(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*d = value
	return nil
}

func (d Delegation) Validate() error {
	if !validIdentityPart(d.DelegationID) {
		return contractError(ErrInvalidIdentity, "delegation.delegation_id", "must identify one immutable delegation")
	}
	if !validIdentityPart(d.ParentGrantRef) {
		return contractError(ErrInvalidIdentity, "delegation.parent_grant_ref", "must identify the parent grant")
	}
	if d.Hop != 1 {
		return contractError(ErrInvalidIdentity, "delegation.hop", "must be exactly one bounded delegation hop")
	}
	return d.ParentCeiling.validate("delegation.parent_ceiling")
}

// Actor preserves both the requested and effective principals. They are equal
// for direct execution and distinct only under one explicit delegation.
type Actor struct {
	RequestedActor Principal   `json:"requested_actor"`
	EffectiveActor Principal   `json:"effective_actor"`
	Delegation     *Delegation `json:"delegation,omitempty"`
}

func (a *Actor) UnmarshalJSON(data []byte) error {
	type wire Actor
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "requested_actor", "effective_actor"); err != nil {
		return err
	}
	value := Actor(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*a = value
	return nil
}

func (a Actor) Validate() error {
	if err := a.RequestedActor.Validate(); err != nil {
		return err
	}
	if err := a.EffectiveActor.Validate(); err != nil {
		return err
	}
	if a.RequestedActor.OrgID != a.EffectiveActor.OrgID {
		return contractError(ErrInvalidIdentity, "actor", "requested and effective actors must belong to the same organization")
	}
	samePrincipal := a.RequestedActor.Equal(a.EffectiveActor)
	if samePrincipal && a.Delegation != nil {
		return contractError(ErrInvalidIdentity, "actor.delegation", "direct attribution cannot carry a delegation hop")
	}
	if !samePrincipal && a.Delegation == nil {
		return contractError(ErrInvalidIdentity, "actor.delegation", "distinct requested and effective actors require one explicit delegation")
	}
	if a.Delegation != nil {
		if err := a.Delegation.Validate(); err != nil {
			return err
		}
		if a.Delegation.ParentCeiling.Scope.OrgID != a.RequestedActor.OrgID {
			return contractError(ErrInvalidIdentity, "actor.delegation.parent_ceiling.scope.org_id", "must remain in the requested actor organization")
		}
	}
	return nil
}

func (a Actor) Equal(other Actor) bool {
	if !a.RequestedActor.Equal(other.RequestedActor) || !a.EffectiveActor.Equal(other.EffectiveActor) {
		return false
	}
	if a.Delegation == nil || other.Delegation == nil {
		return a.Delegation == nil && other.Delegation == nil
	}
	return a.Delegation.Equal(*other.Delegation)
}

func (d Delegation) Equal(other Delegation) bool {
	return d.DelegationID == other.DelegationID &&
		d.ParentGrantRef == other.ParentGrantRef &&
		d.Hop == other.Hop &&
		d.ParentCeiling.Equal(other.ParentCeiling)
}
