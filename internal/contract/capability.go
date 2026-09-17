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

// HTTPExecutionCapabilities returns the canonical compilation declarations for
// shared HTTP behavior. Callers select only capabilities required by the
// normalized operation; declarations do not establish runtime registration.
func HTTPExecutionCapabilities() []CapabilityRequirement {
	return []CapabilityRequirement{
		{
			Capability: "http.execute",
			Foundation: FoundationRef{ID: "FND-06", Version: "1.0.0"},
		},
		{
			Capability: "value.serialize",
			Foundation: FoundationRef{ID: "FND-03", Version: "1.0.0"},
		},
		{
			Capability: "effect.authorize",
			Foundation: FoundationRef{ID: "FND-05", Version: "2.0.0"},
		},
		{
			Capability: "effect.limit",
			Foundation: FoundationRef{ID: "FND-10", Version: "2.0.0"},
		},
		{
			Capability: "http.paginate",
			Foundation: FoundationRef{ID: "FND-08", Version: "1.0.0"},
		},
		{
			Capability: "credentials.resolve",
			Foundation: FoundationRef{ID: "FND-05", Version: "2.0.0"},
		},
	}
}
