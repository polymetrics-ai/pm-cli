package contract

import "time"

type ScheduleOverlap string

const (
	ScheduleOverlapSkip           ScheduleOverlap = "SKIP"
	ScheduleOverlapBufferOne      ScheduleOverlap = "BUFFER_ONE"
	ScheduleOverlapBufferAll      ScheduleOverlap = "BUFFER_ALL"
	ScheduleOverlapCancelOther    ScheduleOverlap = "CANCEL_OTHER"
	ScheduleOverlapTerminateOther ScheduleOverlap = "TERMINATE_OTHER"
	ScheduleOverlapAllowAll       ScheduleOverlap = "ALLOW_ALL"
)

// Schedule is an immutable desired schedule revision. It does not claim that a
// corresponding Temporal schedule has been applied.
type Schedule struct {
	ScheduleID            string          `json:"schedule_id"`
	Revision              uint64          `json:"revision"`
	FlowOrProfileRevision string          `json:"flow_or_profile_revision"`
	Timezone              string          `json:"timezone"`
	CalendarSpec          string          `json:"calendar_spec"`
	Overlap               ScheduleOverlap `json:"overlap"`
	CatchupWindow         string          `json:"catchup_window"`
	Jitter                string          `json:"jitter"`
	ExecutionPolicyRef    string          `json:"execution_policy_ref"`
}

func (s *Schedule) UnmarshalJSON(data []byte) error {
	type wire Schedule
	var decoded wire
	fields, err := decodeRequiredObjectFields(data, &decoded,
		"schedule_id", "revision", "flow_or_profile_revision", "timezone",
		"calendar_spec", "overlap", "catchup_window", "execution_policy_ref",
	)
	if err != nil {
		return err
	}
	if _, present := fields["jitter"]; !present {
		decoded.Jitter = "0"
	}
	value := Schedule(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*s = value
	return nil
}

func (s Schedule) Validate() error {
	references := [...]struct {
		path  string
		value string
	}{
		{path: "schedule.schedule_id", value: s.ScheduleID},
		{path: "schedule.flow_or_profile_revision", value: s.FlowOrProfileRevision},
		{path: "schedule.execution_policy_ref", value: s.ExecutionPolicyRef},
	}
	for _, reference := range references {
		if !validIdentityPart(reference.value) {
			return contractError(ErrInvalidIdentity, reference.path, "must be an explicit immutable reference")
		}
	}
	if !validIdentityPart(s.CalendarSpec) {
		return contractError(ErrInvalidContract, "schedule.calendar_spec", "must be an explicit canonical Temporal calendar specification")
	}
	if s.Revision == 0 {
		return contractError(ErrInvalidIdentity, "schedule.revision", "must be a positive immutable revision")
	}
	if s.Timezone != "UTC" {
		return contractError(ErrInvalidContract, "schedule.timezone", "initial schedule contract requires explicit UTC")
	}
	switch s.Overlap {
	case ScheduleOverlapSkip, ScheduleOverlapBufferOne, ScheduleOverlapBufferAll,
		ScheduleOverlapCancelOther, ScheduleOverlapTerminateOther, ScheduleOverlapAllowAll:
	default:
		return contractError(ErrInvalidContract, "schedule.overlap", "must be a supported explicit Temporal overlap policy")
	}
	catchup, err := time.ParseDuration(s.CatchupWindow)
	if err != nil || catchup <= 0 {
		return contractError(ErrInvalidContract, "schedule.catchup_window", "must be a finite positive Go duration")
	}
	jitter, err := time.ParseDuration(s.Jitter)
	if err != nil || jitter < 0 {
		return contractError(ErrInvalidContract, "schedule.jitter", "must be an explicit nonnegative Go duration")
	}
	return nil
}
