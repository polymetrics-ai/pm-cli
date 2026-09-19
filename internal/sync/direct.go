package sync

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"strings"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	pmruntime "github.com/polymetrics-ai/pm-cli/internal/runtime"
)

var (
	ErrDirectReadUnavailable = errors.New("direct read is unavailable")
	ErrInvalidDirectResult   = errors.New("invalid direct read result")
)

// HTTPExecutor is the narrow application-owned seam around the shared runtime.
// It allows worker registration to remain in internal/app without importing the
// application package back into runtime or sync.
type HTTPExecutor interface {
	ExecuteHTTP(context.Context, pmruntime.HTTPRequest) (pmruntime.HTTPExecution, error)
}

type HTTPExecutorFunc func(context.Context, pmruntime.HTTPRequest) (pmruntime.HTTPExecution, error)

func (function HTTPExecutorFunc) ExecuteHTTP(ctx context.Context, request pmruntime.HTTPRequest) (pmruntime.HTTPExecution, error) {
	return function(ctx, request)
}

// DirectRead executes one shared HTTP action (or serves an authenticated unread
// continuation tail) and returns typed action data. Provider-specific parsing
// and provider switches are deliberately absent.
func DirectRead(ctx context.Context, executor HTTPExecutor, request pmruntime.HTTPRequest) (contract.ActionResult, error) {
	if executor == nil {
		return contract.ActionResult{}, ErrDirectReadUnavailable
	}
	if !directReadEligible(request.Operation) {
		return contract.ActionResult{}, fmt.Errorf("%w: operation is not source-eligible", ErrDirectReadUnavailable)
	}
	execution, err := executor.ExecuteHTTP(ctx, request)
	if err != nil {
		return contract.ActionResult{}, err
	}
	if execution.Result == nil {
		return contract.ActionResult{}, fmt.Errorf("%w: source returned no action result", ErrInvalidDirectResult)
	}
	if execution.PhysicalSend {
		if execution.Result.Data.Presence() == contract.PresenceAbsent &&
			!declaresNoContentStatus(request.Operation, execution.Response.StatusCode) {
			return contract.ActionResult{}, fmt.Errorf("%w: absent data does not match the selected response", ErrInvalidDirectResult)
		}
		if execution.Result.Data.Presence() != contract.PresenceAbsent &&
			!declaresMediaStatus(request.Operation, execution.Response.StatusCode, execution.Result.MediaType) {
			return contract.ActionResult{}, fmt.Errorf("%w: result media does not match the selected response", ErrInvalidDirectResult)
		}
	} else if execution.Disposition != pmruntime.DispositionNotSent || execution.Response.StatusCode != 0 {
		return contract.ActionResult{}, fmt.Errorf("%w: unsent direct result has contradictory response evidence", ErrInvalidDirectResult)
	}
	if err := ValidateDirectResult(request.Operation, *execution.Result); err != nil {
		return contract.ActionResult{}, err
	}
	return *execution.Result, nil
}

func ValidateDirectResult(operation contract.Operation, result contract.ActionResult) error {
	if err := result.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidDirectResult, err)
	}
	if result.Data.Presence() == contract.PresenceAbsent {
		for _, response := range operation.Responses {
			if response.NoContent {
				return nil
			}
		}
		return fmt.Errorf("%w: absent data is not declared by the operation", ErrInvalidDirectResult)
	}
	if result.Pagination != nil {
		if _, ok := result.Data.Elements(); !ok {
			return fmt.Errorf("%w: paginated direct data is not an array", ErrInvalidDirectResult)
		}
	}
	for _, response := range operation.Responses {
		if response.NoContent || response.MediaType == "" {
			continue
		}
		if sameMediaType(response.MediaType, result.MediaType) {
			return nil
		}
	}
	return fmt.Errorf("%w: media type is not declared by the operation", ErrInvalidDirectResult)
}

func directReadEligible(operation contract.Operation) bool {
	matched := false
	for _, eligibility := range operation.LaneEligibility {
		if eligibility.Lane != contract.LaneDirectRead {
			continue
		}
		if matched || eligibility.Status != contract.LaneEligible {
			return false
		}
		matched = true
	}
	return matched
}

func declaresNoContentStatus(operation contract.Operation, statusCode int) bool {
	status := fmt.Sprintf("%d", statusCode)
	exactMatched := false
	for _, response := range operation.Responses {
		if response.Status == status {
			exactMatched = true
			if response.NoContent {
				return true
			}
		}
	}
	if exactMatched {
		return false
	}
	for _, response := range operation.Responses {
		if response.Status == "default" && response.NoContent {
			return true
		}
	}
	return false
}

func declaresMediaStatus(operation contract.Operation, statusCode int, mediaType string) bool {
	status := fmt.Sprintf("%d", statusCode)
	exactMatched := false
	for _, response := range operation.Responses {
		if response.Status != status {
			continue
		}
		exactMatched = true
		if !response.NoContent && sameMediaType(response.MediaType, mediaType) {
			return true
		}
	}
	if exactMatched {
		return false
	}
	for _, response := range operation.Responses {
		if response.Status == "default" && !response.NoContent && sameMediaType(response.MediaType, mediaType) {
			return true
		}
	}
	return false
}

func sameMediaType(left, right string) bool {
	leftType, leftParams, leftErr := mime.ParseMediaType(left)
	rightType, rightParams, rightErr := mime.ParseMediaType(right)
	if leftErr != nil || rightErr != nil || !strings.EqualFold(leftType, rightType) {
		return false
	}
	for name, value := range leftParams {
		if rightParams[name] != value {
			return false
		}
	}
	return true
}
