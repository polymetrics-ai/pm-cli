package runtime

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

var ErrOutcomeUnknown = errors.New("HTTP attempt outcome is unknown")

type AttemptDisposition string

const (
	DispositionNotSent   AttemptDisposition = "not_sent"
	DispositionUnknown   AttemptDisposition = "unknown"
	DispositionResponded AttemptDisposition = "responded"
)

type RetryDirective struct {
	Eligible          bool
	RequiresCondition bool
	Reason            string
}

type RetryCooldown struct {
	Present   bool
	Usable    bool
	NotBefore time.Time
}

func retryForTransportFailure(
	retry *contract.OperationRetry,
	disposition AttemptDisposition,
	limits CallLimits,
) RetryDirective {
	if limits.AttemptNumber >= limits.MaxAttempts {
		return RetryDirective{}
	}
	if disposition == DispositionNotSent {
		return RetryDirective{Eligible: true, Reason: "definitive_unsent"}
	}
	if disposition != DispositionUnknown {
		return RetryDirective{}
	}
	return retryForClassification(retry, "source_declared_replay_safe")
}

func retryForHTTPStatus(
	retry *contract.OperationRetry,
	statusCode int,
	limits CallLimits,
	observedAt time.Time,
	deadline time.Time,
) RetryDirective {
	if retry == nil || limits.AttemptNumber >= limits.MaxAttempts ||
		!observedAt.Before(deadline) || statusCode < 0 || statusCode > int(^uint16(0)) {
		return RetryDirective{}
	}
	admitted := false
	for _, status := range retry.HTTPStatuses {
		if status == uint16(statusCode) {
			admitted = true
			break
		}
	}
	if !admitted {
		return RetryDirective{}
	}
	return retryForClassification(retry, "source_declared_http_status")
}

func retryForClassification(retry *contract.OperationRetry, safeReason string) RetryDirective {
	if retry == nil {
		return RetryDirective{}
	}
	switch retry.Classification {
	case contract.OperationRetrySafe:
		return RetryDirective{Eligible: true, Reason: safeReason}
	case contract.OperationRetryConditional:
		return RetryDirective{RequiresCondition: true, Reason: "source_condition_required"}
	default:
		return RetryDirective{}
	}
}

func retryAfterCooldown(header http.Header, observedAt time.Time, maxDelay time.Duration) RetryCooldown {
	values := header.Values("Retry-After")
	if len(values) == 0 {
		return RetryCooldown{}
	}
	cooldown := RetryCooldown{Present: true}
	if len(values) != 1 || observedAt.IsZero() || maxDelay <= 0 {
		return cooldown
	}
	value := strings.TrimSpace(values[0])
	if value == "" {
		return cooldown
	}
	if decimalDigits(value) {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > uint64(time.Duration(1<<63-1)/time.Second) {
			return cooldown
		}
		delay := time.Duration(seconds) * time.Second
		if delay > maxDelay {
			return cooldown
		}
		cooldown.Usable = true
		cooldown.NotBefore = observedAt.Add(delay)
		return cooldown
	}
	notBefore, err := http.ParseTime(value)
	if err != nil {
		return cooldown
	}
	notBefore = notBefore.UTC()
	if notBefore.After(observedAt.Add(maxDelay)) {
		return cooldown
	}
	cooldown.Usable = true
	cooldown.NotBefore = notBefore
	return cooldown
}

func decimalDigits(value string) bool {
	for index := range len(value) {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return value != ""
}
