package contract

import (
	"math"
	"strings"
	"time"
)

const maximumHTTPExecutionBytes = uint64(math.MaxInt64 - 1)

// HTTPExecutionPolicy is the complete finite HTTP authority selected by the
// server for one admitted run. It contains no provider defaults or credentials.
type HTTPExecutionPolicy struct {
	MaxRequestBytes  uint64        `json:"max_request_bytes"`
	MaxResponseBytes uint64        `json:"max_response_bytes"`
	MaxDNSAnswers    uint32        `json:"max_dns_answers"`
	MaxAttempts      uint32        `json:"max_attempts"`
	MaxRetryDelay    time.Duration `json:"max_retry_delay"`
	SelectedOrigin   string        `json:"selected_origin"`
}

func (policy *HTTPExecutionPolicy) UnmarshalJSON(data []byte) error {
	type wire HTTPExecutionPolicy
	var decoded wire
	if err := decodeRequiredObject(data, &decoded,
		"max_request_bytes", "max_response_bytes", "max_dns_answers",
		"max_attempts", "max_retry_delay", "selected_origin",
	); err != nil {
		return err
	}
	value := HTTPExecutionPolicy(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*policy = value
	return nil
}

func (policy HTTPExecutionPolicy) Validate() error {
	_, err := policy.validatedOrigin()
	return err
}

// ValidateForOperation proves that the server-selected origin is exactly one
// source-admitted HTTP origin for the operation, including any base path.
func (policy HTTPExecutionPolicy) ValidateForOperation(operation Operation) error {
	selectedOrigin, err := policy.validatedOrigin()
	if err != nil {
		return err
	}
	if operation.Execution == nil || operation.Execution.HTTP == nil || len(operation.Execution.HTTP.Origins) == 0 {
		return contractError(ErrInvalidContract, "http_execution_policy.selected_origin", "operation has no admitted HTTP origin")
	}

	matches := 0
	for _, origin := range operation.Execution.HTTP.Origins {
		identity, _, err := validateHTTPOrigin(origin)
		if err != nil {
			return err
		}
		if identity == selectedOrigin {
			matches++
		}
	}
	switch matches {
	case 1:
		return nil
	case 0:
		return contractError(ErrInvalidContract, "http_execution_policy.selected_origin", "selected origin is not admitted by the operation")
	default:
		return contractError(ErrInvalidContract, "http_execution_policy.selected_origin", "selected origin is ambiguous in the operation authority")
	}
}

func (policy HTTPExecutionPolicy) validatedOrigin() (string, error) {
	if policy.MaxRequestBytes == 0 || policy.MaxRequestBytes > maximumHTTPExecutionBytes {
		return "", contractError(ErrInvalidContract, "http_execution_policy.max_request_bytes", "must be a positive finite signed 64-bit byte bound")
	}
	if policy.MaxResponseBytes == 0 || policy.MaxResponseBytes > maximumHTTPExecutionBytes {
		return "", contractError(ErrInvalidContract, "http_execution_policy.max_response_bytes", "must be a positive finite signed 64-bit byte bound")
	}
	if policy.MaxDNSAnswers == 0 {
		return "", contractError(ErrInvalidContract, "http_execution_policy.max_dns_answers", "must be a positive finite answer bound")
	}
	if policy.MaxAttempts == 0 {
		return "", contractError(ErrInvalidContract, "http_execution_policy.max_attempts", "must be a positive finite attempt bound")
	}
	if policy.MaxRetryDelay <= 0 {
		return "", contractError(ErrInvalidContract, "http_execution_policy.max_retry_delay", "must be a positive finite duration")
	}
	identity, audience, err := validateHTTPOrigin(policy.SelectedOrigin)
	if err != nil || !strings.HasPrefix(audience, "https://") {
		return "", contractError(ErrInvalidContract, "http_execution_policy.selected_origin", "must be an absolute HTTPS base URL without credentials, query, fragment, or template")
	}
	return identity, nil
}
