package contract

import "time"

// PolicyEpochs preserves each independently revocable authority snapshot.
// Zero is a legitimate explicit epoch for an authority not used by a grant.
type PolicyEpochs struct {
	Membership uint64 `json:"membership"`
	Service    uint64 `json:"service"`
	Delegation uint64 `json:"delegation"`
	Connection uint64 `json:"connection"`
	Approval   uint64 `json:"approval"`
}

func (p *PolicyEpochs) UnmarshalJSON(data []byte) error {
	type wire PolicyEpochs
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "membership", "service", "delegation", "connection", "approval"); err != nil {
		return err
	}
	*p = PolicyEpochs(decoded)
	return nil
}

// GrantCeiling is an explicit finite parent scope. Empty arrays grant no
// members; they never mean all connections, operations, or approvals.
type GrantCeiling struct {
	Scope         Scope     `json:"scope"`
	ConnectionIDs []string  `json:"connection_ids"`
	OperationIDs  []string  `json:"operation_ids"`
	ApprovalRefs  []string  `json:"approval_refs"`
	ExpiresAt     time.Time `json:"expires_at"`
}

func (c *GrantCeiling) UnmarshalJSON(data []byte) error {
	type wire GrantCeiling
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "scope", "connection_ids", "operation_ids", "approval_refs", "expires_at"); err != nil {
		return err
	}
	value := GrantCeiling(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*c = value
	return nil
}

func (c GrantCeiling) Validate() error {
	return c.validate("grant_ceiling")
}

func (c GrantCeiling) validate(path string) error {
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if err := validateIdentityList(c.ConnectionIDs, path+".connection_ids", true); err != nil {
		return err
	}
	if err := validateIdentityList(c.OperationIDs, path+".operation_ids", true); err != nil {
		return err
	}
	if err := validateIdentityList(c.ApprovalRefs, path+".approval_refs", true); err != nil {
		return err
	}
	return validateUTCInstant(c.ExpiresAt, path+".expires_at")
}

func (c GrantCeiling) Equal(other GrantCeiling) bool {
	return c.Scope.Equal(other.Scope) &&
		equalStringSlices(c.ConnectionIDs, other.ConnectionIDs) &&
		equalStringSlices(c.OperationIDs, other.OperationIDs) &&
		equalStringSlices(c.ApprovalRefs, other.ApprovalRefs) &&
		c.ExpiresAt.Equal(other.ExpiresAt)
}

// ExecutionGrant is durable accepted authority. It intentionally contains no
// login-session identity: session expiry cannot erase this epoch or expiry.
type ExecutionGrant struct {
	GrantRef      string       `json:"grant_ref"`
	GrantEpoch    uint64       `json:"grant_epoch"`
	Scope         Scope        `json:"scope"`
	Actor         Actor        `json:"actor"`
	ConnectionIDs []string     `json:"connection_ids"`
	OperationIDs  []string     `json:"operation_ids"`
	ApprovalRefs  []string     `json:"approval_refs"`
	ExpiresAt     time.Time    `json:"expires_at"`
	PolicyEpochs  PolicyEpochs `json:"policy_epochs"`
}

func (g *ExecutionGrant) UnmarshalJSON(data []byte) error {
	type wire ExecutionGrant
	var decoded wire
	if err := decodeRequiredObject(data, &decoded,
		"grant_ref", "grant_epoch", "scope", "actor", "connection_ids",
		"operation_ids", "approval_refs", "expires_at", "policy_epochs",
	); err != nil {
		return err
	}
	value := ExecutionGrant(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*g = value
	return nil
}

func (g ExecutionGrant) Validate() error {
	if !validIdentityPart(g.GrantRef) {
		return contractError(ErrInvalidIdentity, "execution_grant.grant_ref", "must identify one durable grant")
	}
	if g.GrantEpoch == 0 {
		return contractError(ErrInvalidIdentity, "execution_grant.grant_epoch", "must be a positive durable epoch")
	}
	if err := g.Scope.Validate(); err != nil {
		return err
	}
	if err := g.Actor.Validate(); err != nil {
		return err
	}
	if g.Actor.RequestedActor.OrgID != g.Scope.OrgID || g.Actor.EffectiveActor.OrgID != g.Scope.OrgID {
		return contractError(ErrInvalidIdentity, "execution_grant.actor", "actor attribution must remain inside the grant organization")
	}
	if err := validateIdentityList(g.ConnectionIDs, "execution_grant.connection_ids", true); err != nil {
		return err
	}
	if err := validateIdentityList(g.OperationIDs, "execution_grant.operation_ids", true); err != nil {
		return err
	}
	if err := validateIdentityList(g.ApprovalRefs, "execution_grant.approval_refs", true); err != nil {
		return err
	}
	if (g.Actor.RequestedActor.Kind == PrincipalUser || g.Actor.EffectiveActor.Kind == PrincipalUser) && g.PolicyEpochs.Membership == 0 {
		return contractError(ErrInvalidIdentity, "execution_grant.policy_epochs.membership", "user authority requires an explicit membership epoch")
	}
	if (g.Actor.RequestedActor.Kind == PrincipalServiceAccount || g.Actor.EffectiveActor.Kind == PrincipalServiceAccount) && g.PolicyEpochs.Service == 0 {
		return contractError(ErrInvalidIdentity, "execution_grant.policy_epochs.service", "service-account authority requires an explicit service epoch")
	}
	if g.Actor.Delegation != nil && g.PolicyEpochs.Delegation == 0 {
		return contractError(ErrInvalidIdentity, "execution_grant.policy_epochs.delegation", "delegated authority requires an explicit delegation epoch")
	}
	if len(g.ConnectionIDs) != 0 && g.PolicyEpochs.Connection == 0 {
		return contractError(ErrInvalidIdentity, "execution_grant.policy_epochs.connection", "connection authority requires an explicit connection epoch")
	}
	if len(g.ApprovalRefs) != 0 && g.PolicyEpochs.Approval == 0 {
		return contractError(ErrInvalidIdentity, "execution_grant.policy_epochs.approval", "approval authority requires an explicit approval epoch")
	}
	if err := validateUTCInstant(g.ExpiresAt, "execution_grant.expires_at"); err != nil {
		return err
	}
	if delegation := g.Actor.Delegation; delegation != nil {
		if g.GrantRef == delegation.ParentGrantRef {
			return contractError(ErrInvalidIdentity, "execution_grant.grant_ref", "delegated grant must be distinct from its parent grant")
		}
		ceiling := delegation.ParentCeiling
		if !g.Scope.Equal(ceiling.Scope) {
			return contractError(ErrInvalidIdentity, "execution_grant.scope", "delegated scope cannot expand or alias the parent workspace")
		}
		if !sameStringSetSubset(g.ConnectionIDs, ceiling.ConnectionIDs) {
			return contractError(ErrInvalidIdentity, "execution_grant.connection_ids", "delegated connections cannot exceed the parent grant")
		}
		if !sameStringSetSubset(g.OperationIDs, ceiling.OperationIDs) {
			return contractError(ErrInvalidIdentity, "execution_grant.operation_ids", "delegated operations cannot exceed the parent grant")
		}
		if !sameStringSetSubset(g.ApprovalRefs, ceiling.ApprovalRefs) {
			return contractError(ErrInvalidIdentity, "execution_grant.approval_refs", "delegated approvals cannot exceed the parent grant")
		}
		if g.ExpiresAt.After(ceiling.ExpiresAt) {
			return contractError(ErrInvalidIdentity, "execution_grant.expires_at", "delegated expiry cannot exceed the parent grant")
		}
	}
	return nil
}

func (g ExecutionGrant) Equal(other ExecutionGrant) bool {
	return g.GrantRef == other.GrantRef &&
		g.GrantEpoch == other.GrantEpoch &&
		g.Scope.Equal(other.Scope) &&
		g.Actor.Equal(other.Actor) &&
		equalStringSlices(g.ConnectionIDs, other.ConnectionIDs) &&
		equalStringSlices(g.OperationIDs, other.OperationIDs) &&
		equalStringSlices(g.ApprovalRefs, other.ApprovalRefs) &&
		g.ExpiresAt.Equal(other.ExpiresAt) &&
		g.PolicyEpochs == other.PolicyEpochs
}
