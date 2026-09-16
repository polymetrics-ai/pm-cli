package contract

import "time"

// Approval binds an approver to the complete immutable action, input, workset,
// schema, account, configuration and budget context. It is representation only;
// later policy owners decide whether it is currently dispatchable.
type Approval struct {
	ApprovalRef            string             `json:"approval_ref"`
	OrgID                  string             `json:"org_id"`
	WorkspaceID            string             `json:"workspace_id"`
	PlanRef                OperationOrPlanRef `json:"plan_ref"`
	ActionSetDigest        SemanticDigest     `json:"action_set_digest"`
	InputDigest            SemanticDigest     `json:"input_digest"`
	WorksetDigest          SemanticDigest     `json:"workset_digest"`
	SchemaRefs             []string           `json:"schema_refs"`
	ProviderAccountWitness string             `json:"provider_account_witness"`
	ConfigurationRevision  string             `json:"configuration_revision"`
	BudgetDigest           SemanticDigest     `json:"budget_digest"`
	RowCount               uint64             `json:"row_count"`
	ExpiresAt              time.Time          `json:"expires_at"`
	ApprovedBy             Actor              `json:"approved_by"`
}

func (a *Approval) UnmarshalJSON(data []byte) error {
	type wire Approval
	var decoded wire
	if err := decodeRequiredObject(data, &decoded,
		"approval_ref", "org_id", "workspace_id", "plan_ref", "action_set_digest",
		"input_digest", "workset_digest", "schema_refs", "provider_account_witness",
		"configuration_revision", "budget_digest", "row_count", "expires_at", "approved_by",
	); err != nil {
		return err
	}
	value := Approval(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*a = value
	return nil
}

func (a Approval) Validate() error {
	if !validIdentityPart(a.ApprovalRef) {
		return contractError(ErrInvalidIdentity, "approval.approval_ref", "must identify one immutable approval")
	}
	scope := Scope{OrgID: a.OrgID, WorkspaceID: a.WorkspaceID}
	if err := scope.Validate(); err != nil {
		return err
	}
	if err := a.PlanRef.Validate(); err != nil {
		return err
	}
	if a.PlanRef.Kind != PlanReference {
		return contractError(ErrInvalidContract, "approval.plan_ref.kind", "approval must bind a sealed plan revision")
	}
	digests := [...]struct {
		path   string
		digest SemanticDigest
	}{
		{path: "approval.action_set_digest", digest: a.ActionSetDigest},
		{path: "approval.input_digest", digest: a.InputDigest},
		{path: "approval.workset_digest", digest: a.WorksetDigest},
		{path: "approval.budget_digest", digest: a.BudgetDigest},
	}
	for _, binding := range digests {
		if err := binding.digest.Validate(); err != nil {
			return contractError(ErrInvalidContract, binding.path, "must be an exact semantic SHA-256 digest")
		}
	}
	if err := validateIdentityList(a.SchemaRefs, "approval.schema_refs", true); err != nil {
		return err
	}
	if !validIdentityPart(a.ProviderAccountWitness) {
		return contractError(ErrInvalidIdentity, "approval.provider_account_witness", "must bind the approved provider-account identity")
	}
	if !validIdentityPart(a.ConfigurationRevision) {
		return contractError(ErrInvalidIdentity, "approval.configuration_revision", "must bind one immutable configuration revision")
	}
	if err := validateUTCInstant(a.ExpiresAt, "approval.expires_at"); err != nil {
		return err
	}
	if err := a.ApprovedBy.Validate(); err != nil {
		return err
	}
	if a.ApprovedBy.RequestedActor.OrgID != scope.OrgID || a.ApprovedBy.EffectiveActor.OrgID != scope.OrgID {
		return contractError(ErrInvalidIdentity, "approval.approved_by", "must remain in the approval organization")
	}
	return nil
}
