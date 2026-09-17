package contract

type CapabilityRequirement struct {
	Capability string        `json:"capability"`
	Foundation FoundationRef `json:"foundation"`
}

func (r CapabilityRequirement) Validate() error {
	if !validIdentityPart(r.Capability) {
		return contractError(ErrInvalidContract, "capability_requirement.capability", "must be a nonempty valid UTF-8 canonical name")
	}
	if err := r.Foundation.Validate(); err != nil {
		return err
	}
	return nil
}
