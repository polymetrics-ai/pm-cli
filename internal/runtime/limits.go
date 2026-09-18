package runtime

import (
	"errors"
	"fmt"
	"time"
)

var ErrInvalidLimits = errors.New("invalid HTTP execution limits")

// CallLimits are server-owned bounds for one physical HTTP attempt. They are
// intentionally separate from source-declared provider pagination fields.
type CallLimits struct {
	MaxRequestBytes  uint64
	MaxResponseBytes uint64
	MaxDNSAnswers    uint32
	AttemptNumber    uint32
	MaxAttempts      uint32
	MaxRetryDelay    time.Duration
	TotalRecordCap   *uint64
}

func (l CallLimits) validate() error {
	if l.MaxRequestBytes == 0 {
		return fmt.Errorf("%w: request byte bound is absent", ErrInvalidLimits)
	}
	if l.MaxResponseBytes == 0 {
		return fmt.Errorf("%w: response byte bound is absent", ErrInvalidLimits)
	}
	if l.MaxDNSAnswers == 0 {
		return fmt.Errorf("%w: DNS answer bound is absent", ErrInvalidLimits)
	}
	if l.AttemptNumber == 0 || l.MaxAttempts == 0 || l.AttemptNumber > l.MaxAttempts {
		return fmt.Errorf("%w: attempt ordinal is outside its finite budget", ErrInvalidLimits)
	}
	if l.MaxRetryDelay <= 0 {
		return fmt.Errorf("%w: retry delay bound is absent", ErrInvalidLimits)
	}
	if l.TotalRecordCap != nil && *l.TotalRecordCap == 0 {
		return fmt.Errorf("%w: total record cap must be positive when present", ErrInvalidLimits)
	}
	return nil
}

func addUint64(left, right uint64) (uint64, error) {
	if ^uint64(0)-left < right {
		return 0, fmt.Errorf("%w: byte count overflow", ErrInvalidLimits)
	}
	return left + right, nil
}
