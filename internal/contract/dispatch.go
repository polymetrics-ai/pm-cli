package contract

type CapacityReservation struct {
	OutcomeSlots  uint64 `json:"outcome_slots"`
	ReadbackSlots uint64 `json:"readback_slots"`
	EncodedBytes  uint64 `json:"encoded_bytes"`
}

type Dispatch struct {
	RunID               string              `json:"run_id"`
	EffectKey           EffectKey           `json:"effect_key"`
	DispatchID          DispatchID          `json:"dispatch_id"`
	OrderedMemberIDs    []MemberID          `json:"ordered_member_ids"`
	PayloadHash         PayloadHash         `json:"payload_hash"`
	RequestArtifactRef  string              `json:"request_artifact_ref"`
	OperationRevision   string              `json:"operation_revision"`
	AccountWitness      string              `json:"account_witness"`
	GrantEpoch          uint64              `json:"grant_epoch"`
	CapacityReservation CapacityReservation `json:"capacity_reservation"`
	AttemptID           AttemptID           `json:"attempt_id"`
}

func (d Dispatch) Validate() error {
	if !validIdentityPart(d.RunID) {
		return contractError(ErrInvalidDispatch, "run_id", "must reference one durable accepted intent")
	}
	if err := d.EffectKey.Validate(); err != nil {
		return err
	}
	if err := d.DispatchID.Validate(); err != nil {
		return err
	}
	if len(d.OrderedMemberIDs) == 0 {
		return contractError(ErrInvalidDispatch, "ordered_member_ids", "must preserve at least one independently attributable member")
	}
	seen := make(map[MemberID]struct{}, len(d.OrderedMemberIDs))
	for _, memberID := range d.OrderedMemberIDs {
		if err := memberID.Validate(); err != nil {
			return err
		}
		if _, duplicate := seen[memberID]; duplicate {
			return contractError(ErrDuplicateField, "ordered_member_ids", "member ID is duplicated")
		}
		seen[memberID] = struct{}{}
	}
	if err := d.PayloadHash.Validate(); err != nil {
		return err
	}
	if !validIdentityPart(d.RequestArtifactRef) {
		return contractError(ErrInvalidDispatch, "request_artifact_ref", "must reference immutable request bytes")
	}
	if !validIdentityPart(d.OperationRevision) {
		return contractError(ErrInvalidDispatch, "operation_revision", "must freeze the operation revision")
	}
	if !validIdentityPart(d.AccountWitness) || d.AccountWitness != d.EffectKey.EntityKey.AccountWitness {
		return contractError(ErrInvalidDispatch, "account_witness", "must match the effect entity provider account")
	}
	if d.GrantEpoch == 0 {
		return contractError(ErrInvalidDispatch, "grant_epoch", "must bind a positive checked grant epoch")
	}
	if d.CapacityReservation.OutcomeSlots < uint64(len(d.OrderedMemberIDs)) {
		return contractError(ErrInvalidDispatch, "capacity_reservation.outcome_slots", "cannot record every member outcome")
	}
	if d.CapacityReservation.EncodedBytes == 0 {
		return contractError(ErrInvalidDispatch, "capacity_reservation.encoded_bytes", "must reserve finite encoded output capacity")
	}
	if err := d.AttemptID.Validate(); err != nil {
		return err
	}
	return nil
}
