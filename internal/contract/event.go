package contract

import "time"

type SafeReference struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
}

func (r *SafeReference) UnmarshalJSON(data []byte) error {
	type wire SafeReference
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "name", "ref"); err != nil {
		return err
	}
	value := SafeReference(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*r = value
	return nil
}

func (r SafeReference) Validate() error {
	if !validReasonCode(r.Name) || !validIdentityPart(r.Ref) {
		return contractError(ErrInvalidContract, "safe_payload.references", "must contain only canonically named opaque references")
	}
	return nil
}

type SafeCount struct {
	Name  string `json:"name"`
	Value uint64 `json:"value"`
}

func (c *SafeCount) UnmarshalJSON(data []byte) error {
	type wire SafeCount
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "name", "value"); err != nil {
		return err
	}
	value := SafeCount(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*c = value
	return nil
}

func (c SafeCount) Validate() error {
	if !validReasonCode(c.Name) {
		return contractError(ErrInvalidContract, "safe_payload.counts.name", "must be a safe explicit count name")
	}
	return nil
}

// SafePayload deliberately excludes arbitrary values and provider response
// bodies. It can expose only opaque references, integer counts and reason codes.
type SafePayload struct {
	References  []SafeReference `json:"references"`
	Counts      []SafeCount     `json:"counts"`
	ReasonCodes []string        `json:"reason_codes"`
}

func (p *SafePayload) UnmarshalJSON(data []byte) error {
	type wire SafePayload
	var decoded wire
	if err := decodeRequiredObject(data, &decoded, "references", "counts", "reason_codes"); err != nil {
		return err
	}
	value := SafePayload(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*p = value
	return nil
}

func (p SafePayload) Validate() error {
	if p.References == nil || p.Counts == nil || p.ReasonCodes == nil {
		return contractError(ErrInvalidContract, "safe_payload", "all safe payload collections must be explicitly present, even when empty")
	}
	seenRefs := make(map[string]struct{}, len(p.References))
	for _, reference := range p.References {
		if err := reference.Validate(); err != nil {
			return err
		}
		if _, duplicate := seenRefs[reference.Name]; duplicate {
			return contractError(ErrDuplicateField, "safe_payload.references.name", "reference name is duplicated")
		}
		seenRefs[reference.Name] = struct{}{}
	}
	seenCounts := make(map[string]struct{}, len(p.Counts))
	for _, count := range p.Counts {
		if err := count.Validate(); err != nil {
			return err
		}
		if _, duplicate := seenCounts[count.Name]; duplicate {
			return contractError(ErrDuplicateField, "safe_payload.counts.name", "count name is duplicated")
		}
		seenCounts[count.Name] = struct{}{}
	}
	seenReasons := make(map[string]struct{}, len(p.ReasonCodes))
	for _, reason := range p.ReasonCodes {
		if !validReasonCode(reason) {
			return contractError(ErrInvalidContract, "safe_payload.reason_codes", "contains a noncanonical or unsafe reason code")
		}
		if _, duplicate := seenReasons[reason]; duplicate {
			return contractError(ErrDuplicateField, "safe_payload.reason_codes", "reason code is duplicated")
		}
		seenReasons[reason] = struct{}{}
	}
	return nil
}

// ServerEvent carries the state revision and commit timestamp required by the
// transactional event contract; this DTO does not prove that a commit occurred.
type ServerEvent struct {
	OrgID         string      `json:"org_id"`
	WorkspaceID   string      `json:"workspace_id"`
	RunID         string      `json:"run_id"`
	Sequence      uint64      `json:"sequence"`
	StateRevision uint64      `json:"state_revision"`
	Type          string      `json:"type"`
	SafePayload   SafePayload `json:"safe_payload"`
	CommittedAt   time.Time   `json:"committed_at"`
}

func (e *ServerEvent) UnmarshalJSON(data []byte) error {
	type wire ServerEvent
	var decoded wire
	if err := decodeRequiredObject(data, &decoded,
		"org_id", "workspace_id", "run_id", "sequence", "state_revision", "type",
		"safe_payload", "committed_at",
	); err != nil {
		return err
	}
	value := ServerEvent(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*e = value
	return nil
}

func (e ServerEvent) Validate() error {
	if err := (Scope{OrgID: e.OrgID, WorkspaceID: e.WorkspaceID}).Validate(); err != nil {
		return err
	}
	if !validIdentityPart(e.RunID) {
		return contractError(ErrInvalidIdentity, "server_event.run_id", "must bind one stable PM run")
	}
	if e.Sequence == 0 {
		return contractError(ErrInvalidContract, "server_event.sequence", "must be a positive per-run sequence")
	}
	if e.StateRevision == 0 {
		return contractError(ErrInvalidContract, "server_event.state_revision", "must bind a positive committed state revision")
	}
	if !validReasonCode(e.Type) {
		return contractError(ErrInvalidContract, "server_event.type", "must be a safe explicit event type")
	}
	if err := e.SafePayload.Validate(); err != nil {
		return err
	}
	return validateUTCInstant(e.CommittedAt, "server_event.committed_at")
}
