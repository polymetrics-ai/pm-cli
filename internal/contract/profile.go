package contract

// Profile identifies an immutable logical synchronization policy revision.
// Admission/configuration profiles are owned by the configuration package, not this type.
type Profile struct {
	ProfileID              string                  `json:"profile_id"`
	Revision               string                  `json:"revision"`
	Lane                   Lane                    `json:"lane"`
	SourceOperationID      string                  `json:"source_operation_id"`
	DestinationOperationID string                  `json:"destination_operation_id,omitempty"`
	Capabilities           []CapabilityRequirement `json:"capability_requirements"`
}

func (p Profile) Validate() error {
	if !validIdentityPart(p.ProfileID) || !validIdentityPart(p.Revision) {
		return contractError(ErrInvalidContract, "profile", "profile ID and immutable revision are required")
	}
	if !validLane(p.Lane) {
		return contractError(ErrInvalidContract, "profile.lane", "lane is unsupported")
	}
	if !validIdentityPart(p.SourceOperationID) {
		return contractError(ErrInvalidContract, "profile.source_operation_id", "must reference a stable source operation")
	}
	requiresDestination := p.Lane == LaneDirectWrite || p.Lane == LaneReverseETL || p.Lane == LaneBinaryUpload || p.Lane == LaneSyncTransport
	if requiresDestination && !validIdentityPart(p.DestinationOperationID) {
		return contractError(ErrInvalidContract, "profile.destination_operation_id", "destination lane requires a stable destination operation")
	}
	if !requiresDestination && p.DestinationOperationID != "" && !validIdentityPart(p.DestinationOperationID) {
		return contractError(ErrInvalidContract, "profile.destination_operation_id", "must be canonical when present")
	}
	if len(p.Capabilities) == 0 {
		return contractError(ErrInvalidContract, "profile.capability_requirements", "must preserve the selected capability closure")
	}
	seen := make(map[string]struct{}, len(p.Capabilities))
	for _, requirement := range p.Capabilities {
		if err := requirement.Validate(); err != nil {
			return err
		}
		key := requirement.Capability + "\x00" + requirement.Foundation.ID + "@" + requirement.Foundation.Version
		if _, duplicate := seen[key]; duplicate {
			return contractError(ErrDuplicateField, "profile.capability_requirements", "requirement is duplicated")
		}
		seen[key] = struct{}{}
	}
	return nil
}
