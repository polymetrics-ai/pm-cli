package app

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	pmruntime "github.com/polymetrics-ai/pm-cli/internal/runtime"
	pmsync "github.com/polymetrics-ai/pm-cli/internal/sync"
)

// HTTPDirectReadDependencies contains only process-static HTTP runtime
// resources. Current admission and credential authority are acquired from each
// ExecutionRequest and are never injectable here.
type HTTPDirectReadDependencies struct {
	Resolver      pmruntime.DNSResolver
	Dialer        pmruntime.ContextDialer
	TLSConfig     *tls.Config
	Continuations *ContinuationService
	Objects       pmruntime.ObjectReader
	Now           func() time.Time
}

// NewHTTPDirectReadRegistration constructs the shared HTTP direct-read handler.
// It deliberately registers no write lane or reconciliation callback.
func NewHTTPDirectReadRegistration(dependencies HTTPDirectReadDependencies) HandlerRegistration {
	capabilities := httpDirectReadCapabilities(dependencies.Continuations != nil)
	runtimeDependencies := pmruntime.HTTPDependencies{
		Objects:   dependencies.Objects,
		Resolver:  dependencies.Resolver,
		Dialer:    dependencies.Dialer,
		TLSConfig: dependencies.TLSConfig,
		Now:       dependencies.Now,
	}
	return HandlerRegistration{
		Lanes:         []contract.Lane{contract.LaneDirectRead},
		Foundations:   httpDirectReadFoundations(capabilities),
		Capabilities:  capabilities,
		Execute:       newHTTPDirectReadCallback(runtimeDependencies, dependencies.Continuations),
		continuations: dependencies.Continuations,
	}
}

func newHTTPDirectReadCallback(static pmruntime.HTTPDependencies, continuationService *ContinuationService) ExecutionCallback {
	return func(ctx context.Context, request ExecutionRequest) (ExecutionObservation, error) {
		if ctx == nil {
			return ExecutionObservation{}, context.Canceled
		}
		if err := request.HTTPPolicy.ValidateForOperation(request.Operation); err != nil {
			return ExecutionObservation{}, fmt.Errorf("HTTP direct-read policy does not match the operation: %w", err)
		}
		call, admitter, credentials, err := request.PhysicalCallAuthority()
		if err != nil {
			return ExecutionObservation{}, err
		}
		if err := validateHTTPDirectReadAuthority(request, call); err != nil {
			return ExecutionObservation{}, err
		}
		if continuationRequiredForHTTP(request.Operation, request.Controls) {
			if request.continuation == nil || continuationService == nil ||
				request.continuation.service != continuationService {
				return ExecutionObservation{}, workerFailure(ErrWorkerCapabilityUnavailable, ErrContinuationInvalid)
			}
		}
		runtimeInput, err := continuationRuntimeInput(request.Operation, request.Input, request.Controls)
		if err != nil {
			return ExecutionObservation{}, err
		}

		dependencies := static
		dependencies.Admitter = admitter
		dependencies.Credentials = credentials
		dependencies.Continuations = request.continuation
		httpRequest := pmruntime.HTTPRequest{
			Operation: request.Operation,
			Egress: pmruntime.EgressPolicy{
				SelectedOrigin: request.HTTPPolicy.SelectedOrigin,
				RequireTLS:     true,
			},
			Call:     call,
			Input:    runtimeInput,
			Controls: request.Controls,
			Limits: pmruntime.CallLimits{
				MaxRequestBytes:  request.HTTPPolicy.MaxRequestBytes,
				MaxResponseBytes: request.HTTPPolicy.MaxResponseBytes,
				MaxDNSAnswers:    request.HTTPPolicy.MaxDNSAnswers,
				TotalRecordCap:   request.Controls.TotalRecordCap,
				AttemptNumber:    request.AttemptNumber,
				MaxAttempts:      request.HTTPPolicy.MaxAttempts,
				MaxRetryDelay:    request.HTTPPolicy.MaxRetryDelay,
			},
			Continuation: request.Controls.ContinuationValue(),
		}

		var execution pmruntime.HTTPExecution
		executed := false
		result, directErr := pmsync.DirectRead(ctx, pmsync.HTTPExecutorFunc(func(
			ctx context.Context,
			request pmruntime.HTTPRequest,
		) (pmruntime.HTTPExecution, error) {
			executed = true
			var executeErr error
			execution, executeErr = pmruntime.ExecuteHTTP(ctx, dependencies, request)
			return execution, executeErr
		}), httpRequest)
		if !executed {
			return ExecutionObservation{}, directErr
		}

		observedAt := httpDirectReadNow(static.Now)
		validResult := directErr == nil
		observation := ExecutionObservation{
			HTTP:     httpDirectReadObservation(execution, observedAt),
			Outcomes: httpDirectReadOutcomes(request, execution, validResult, observedAt),
		}
		if validResult && httpDirectReadOutcomeState(execution, true) == contract.OutcomeSucceeded {
			// DirectRead has validated both the ActionResult and its selected
			// source response before it reaches common worker publication. A
			// declared non-2xx response remains definitive rejection evidence,
			// not a successful action result.
			observation.Result = &result
		}
		return observation, directErr
	}
}

func continuationRequiredForHTTP(operation contract.Operation, controls contract.ResultControls) bool {
	return controls.HasContinuation() || controls.TotalRecordCap != nil ||
		operation.Execution != nil && operation.Execution.Pagination != nil
}

func continuationRuntimeInput(
	operation contract.Operation,
	input contract.PresentValue,
	controls contract.ResultControls,
) (contract.PresentValue, error) {
	if !controls.HasContinuation() {
		return input, nil
	}
	if err := contract.ValidateOperationInput(operation, input); err != nil {
		return contract.PresentValue{}, ErrContinuationInvalid
	}
	if operation.Execution == nil || operation.Execution.Pagination == nil {
		return input, nil
	}
	fields, ok := input.Fields()
	if !ok {
		return contract.PresentValue{}, ErrContinuationInvalid
	}
	navigationField := pmruntime.PaginationNavigationField(operation.Execution.Pagination)
	if navigationField == "" {
		return contract.PresentValue{}, ErrContinuationInvalid
	}
	delete(fields, navigationField)
	return contract.NewObjectValue(fields), nil
}

func validateHTTPDirectReadAuthority(request ExecutionRequest, call pmruntime.PhysicalCall) error {
	if len(request.Dispatch.OrderedMemberIDs) != 1 {
		return fmt.Errorf("%w: HTTP direct read requires exactly one independently attributable dispatch member", pmruntime.ErrInvalidPhysicalCall)
	}
	if err := request.Dispatch.Validate(); err != nil {
		return fmt.Errorf("HTTP direct-read dispatch identity is invalid: %w", err)
	}
	if err := request.Attempt.Validate(); err != nil {
		return fmt.Errorf("HTTP direct-read attempt identity is invalid: %w", err)
	}
	if request.Attempt.AttemptID != request.Dispatch.AttemptID ||
		request.Attempt.DispatchID != request.Dispatch.DispatchID ||
		call.OperationID != request.Operation.OperationID ||
		call.OperationRevision != request.Dispatch.OperationRevision ||
		call.DispatchID != request.Dispatch.DispatchID ||
		call.Attempt.AttemptID != request.Attempt.AttemptID ||
		call.Attempt.DispatchID != request.Attempt.DispatchID ||
		call.Attempt.ReceiptRef != request.Attempt.ReceiptRef ||
		!call.Attempt.RecordedAt.Equal(request.Attempt.RecordedAt) ||
		!call.Deadline.Equal(request.Deadline) {
		return fmt.Errorf("%w: callback request differs from its physical-call authority", pmruntime.ErrInvalidPhysicalCall)
	}
	return nil
}

func httpDirectReadObservation(execution pmruntime.HTTPExecution, observedAt time.Time) *HTTPAttemptObservation {
	disposition := execution.Disposition
	if disposition == "" && !execution.PhysicalSend {
		// Runtime preflight and current-admission refusals occur before a
		// RoundTrip and therefore return no populated transport disposition.
		disposition = pmruntime.DispositionNotSent
	}
	return &HTTPAttemptObservation{
		Disposition:         disposition,
		PhysicalSend:        execution.PhysicalSend,
		verifiedLocalResult: execution.VerifiedLocalResult,
		StatusCode:          execution.Response.StatusCode,
		ObservedAt:          observedAt,
		Retry:               execution.Retry,
		Cooldown:            execution.Cooldown,
	}
}

func httpDirectReadOutcomes(
	request ExecutionRequest,
	execution pmruntime.HTTPExecution,
	validResult bool,
	observedAt time.Time,
) []contract.MemberOutcome {
	state := httpDirectReadOutcomeState(execution, validResult)
	assertionRefs := []string{}
	if state == contract.OutcomeUnsent || state == contract.OutcomeUnknown {
		// The exact durable attempt receipt is the witness that lets settlement
		// distinguish a definitely-unsent latest attempt from generic unknown.
		assertionRefs = []string{request.Attempt.ReceiptRef}
	}
	outcomes := make([]contract.MemberOutcome, 0, len(request.Dispatch.OrderedMemberIDs))
	for _, memberID := range request.Dispatch.OrderedMemberIDs {
		outcomes = append(outcomes, contract.MemberOutcome{
			MemberID:      memberID,
			EffectKey:     request.Dispatch.EffectKey,
			DispatchID:    request.Dispatch.DispatchID,
			State:         state,
			ObservedAt:    observedAt,
			AssertionRefs: append(make([]string, 0, len(assertionRefs)), assertionRefs...),
		})
	}
	return outcomes
}

func httpDirectReadOutcomeState(execution pmruntime.HTTPExecution, validResult bool) contract.OutcomeState {
	if validResult && execution.VerifiedLocalResult && !execution.PhysicalSend &&
		execution.Disposition == pmruntime.DispositionNotSent {
		return contract.OutcomeSucceeded
	}
	if validResult && execution.PhysicalSend && execution.Disposition == pmruntime.DispositionResponded &&
		execution.Response.StatusCode >= 200 && execution.Response.StatusCode < 300 {
		return contract.OutcomeSucceeded
	}
	switch execution.Disposition {
	case pmruntime.DispositionNotSent:
		// PhysicalSend records that the one-use boundary admitted this
		// RoundTrip. The transport disposition independently proves that no
		// request bytes were transmitted.
		return contract.OutcomeUnsent
	case "":
		if !execution.PhysicalSend {
			return contract.OutcomeUnsent
		}
		return contract.OutcomeUnknown
	case pmruntime.DispositionUnknown:
		return contract.OutcomeUnknown
	case pmruntime.DispositionResponded:
		// A received non-2xx source response or a response that cannot be
		// decoded is definitive, but is not action success. Source records in a
		// response body are result data and are never positionally mapped to
		// independently settled dispatch members.
		return contract.OutcomeRejected
	default:
		return contract.OutcomeUnknown
	}
}

func httpDirectReadNow(now func() time.Time) time.Time {
	if now == nil {
		now = time.Now
	}
	return now().UTC()
}

func httpDirectReadCapabilities(pagination bool) []contract.CapabilityRequirement {
	declared := contract.HTTPExecutionCapabilities()
	capabilities := make([]contract.CapabilityRequirement, 0, len(declared))
	for _, capability := range declared {
		if capability.Capability == "http.paginate" && !pagination {
			continue
		}
		capabilities = append(capabilities, capability)
	}
	return capabilities
}

func httpDirectReadFoundations(capabilities []contract.CapabilityRequirement) []contract.FoundationContract {
	foundations := make([]contract.FoundationContract, 0, len(capabilities))
	seen := make(map[contract.FoundationRef]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if _, duplicate := seen[capability.Foundation]; duplicate {
			continue
		}
		seen[capability.Foundation] = struct{}{}
		foundations = append(foundations, contract.FoundationContract{
			ID:           capability.Foundation.ID,
			Version:      capability.Foundation.Version,
			Dependencies: []contract.FoundationRef{},
		})
	}
	return foundations
}
