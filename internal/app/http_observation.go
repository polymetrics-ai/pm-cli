package app

import (
	"context"
	"errors"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	pmruntime "github.com/polymetrics-ai/pm-cli/internal/runtime"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// HTTPAttemptObservation carries only bounded transport facts into durable
// settlement. Provider payloads, URLs, headers and credentials are not evidence
// fields; result bytes use the separately verified shared artifact publisher.
type HTTPAttemptObservation struct {
	Disposition         pmruntime.AttemptDisposition
	PhysicalSend        bool
	verifiedLocalResult bool
	StatusCode          int
	ObservedAt          time.Time
	Retry               pmruntime.RetryDirective
	Cooldown            pmruntime.RetryCooldown
}

func verifiedLocalHTTPCompletion(
	plan workerUnitPlan,
	observation ExecutionObservation,
	outcomes []contract.MemberOutcome,
	callbackErr error,
) bool {
	if callbackErr != nil || plan.reconcile || plan.handler.Kind != HandlerHTTP ||
		plan.request.continuation == nil || !plan.request.Controls.HasContinuation() ||
		observation.Result == nil || observation.HTTP == nil ||
		!observation.HTTP.verifiedLocalResult || observation.HTTP.PhysicalSend ||
		observation.HTTP.Disposition != pmruntime.DispositionNotSent ||
		observation.HTTP.StatusCode != 0 || observation.HTTP.Cooldown.Present ||
		len(outcomes) == 0 {
		return false
	}
	plan.request.continuation.mu.Lock()
	opened := plan.request.continuation.opened
	plan.request.continuation.mu.Unlock()
	if !opened {
		return false
	}
	for _, outcome := range outcomes {
		if outcome.State != contract.OutcomeSucceeded {
			return false
		}
	}
	return true
}

func recordWorkerHTTPCooldown(
	ctx context.Context,
	tx *store.Transaction,
	plan workerUnitPlan,
	permit store.AccountPermit,
	observation *HTTPAttemptObservation,
) error {
	if observation == nil || !observation.Cooldown.Present || !observation.Cooldown.Usable {
		return nil
	}
	// A received Retry-After is shared account evidence even when decoding the
	// body or publishing its result fails. It is not permission to retry.
	if !observation.PhysicalSend || observation.Disposition != pmruntime.DispositionResponded ||
		observation.StatusCode < 100 || observation.StatusCode > 599 ||
		observation.ObservedAt.IsZero() || observation.ObservedAt.Location() != time.UTC ||
		plan.request.HTTPPolicy.MaxRetryDelay <= 0 ||
		observation.Cooldown.NotBefore.After(observation.ObservedAt.Add(plan.request.HTTPPolicy.MaxRetryDelay)) {
		return workerFailure(ErrWorkerConflict, errors.New("HTTP cooldown lacks a bounded received-response witness"))
	}
	return store.RecordAccountCooldown(ctx, tx, permit, observation.Cooldown.NotBefore)
}
