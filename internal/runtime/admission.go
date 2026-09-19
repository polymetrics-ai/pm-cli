package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/credentials"
)

var (
	ErrInvalidPhysicalCall = errors.New("invalid physical HTTP call")
	ErrAdmissionRequired   = errors.New("physical HTTP call admission is required")
	ErrCredentialRequired  = errors.New("credential resolution is required")
)

type admissionError struct {
	cause error
}

func (e *admissionError) Error() string { return "physical HTTP call admission was denied" }

func (e *admissionError) Unwrap() error { return e.cause }

func (e *admissionError) Is(target error) bool { return target == ErrAdmissionRequired }

// PhysicalCall contains only immutable references needed to re-authorize one
// physical send. Implementations must load current durable state rather than
// treating this structure as proof that a grant, permit, fence, or budget is
// still valid.
type PhysicalCall struct {
	Scope              contract.Scope
	ConnectionID       string
	OperationID        string
	OperationRevision  string
	RunID              string
	TaskID             string
	ExecutionGrantRef  string
	GrantEpoch         uint64
	DispatchID         contract.DispatchID
	Attempt            contract.Attempt
	PermitRef          string
	AccountWitness     string
	Budget             contract.BudgetBinding
	Capacity           contract.CapacityReservation
	Deadline           time.Time
	ReconciliationOnly bool
}

func (c PhysicalCall) validate(operation contract.Operation, limits CallLimits) error {
	if err := c.Scope.Validate(); err != nil {
		return fmt.Errorf("%w: scope: %v", ErrInvalidPhysicalCall, err)
	}
	for _, identity := range []struct {
		name  string
		value string
	}{
		{name: "connection", value: c.ConnectionID},
		{name: "operation", value: c.OperationID},
		{name: "operation revision", value: c.OperationRevision},
		{name: "run", value: c.RunID},
		{name: "task", value: c.TaskID},
		{name: "execution grant", value: c.ExecutionGrantRef},
		{name: "permit", value: c.PermitRef},
		{name: "account witness", value: c.AccountWitness},
	} {
		if identity.value == "" {
			return fmt.Errorf("%w: %s reference is absent", ErrInvalidPhysicalCall, identity.name)
		}
	}
	if c.OperationID != operation.OperationID {
		return fmt.Errorf("%w: operation identity does not match the selected contract", ErrInvalidPhysicalCall)
	}
	if c.GrantEpoch == 0 {
		return fmt.Errorf("%w: grant epoch is absent", ErrInvalidPhysicalCall)
	}
	if err := c.DispatchID.Validate(); err != nil {
		return fmt.Errorf("%w: dispatch: %v", ErrInvalidPhysicalCall, err)
	}
	if err := c.Attempt.Validate(); err != nil {
		return fmt.Errorf("%w: attempt: %v", ErrInvalidPhysicalCall, err)
	}
	if c.Attempt.DispatchID != c.DispatchID {
		return fmt.Errorf("%w: attempt is not bound to the selected dispatch", ErrInvalidPhysicalCall)
	}
	if err := c.Budget.Validate(); err != nil {
		return fmt.Errorf("%w: budget: %v", ErrInvalidPhysicalCall, err)
	}
	if c.Capacity.EncodedBytes == 0 || c.Capacity.EncodedBytes < limits.MaxResponseBytes {
		return fmt.Errorf("%w: response byte bound exceeds the durable capacity reservation", ErrInvalidPhysicalCall)
	}
	if c.Deadline.IsZero() {
		return fmt.Errorf("%w: deadline is absent", ErrInvalidPhysicalCall)
	}
	if c.ReconciliationOnly {
		return fmt.Errorf("%w: reconciliation-only calls cannot send", ErrInvalidPhysicalCall)
	}
	return nil
}

// PhysicalCallAdmitter is implemented by the application boundary. It must
// transactionally recheck the durable grant, operation/account authorization,
// live account permit and fence, immutable budget binding, and cancellation at
// the moment this method is called. A successful return is valid for this call
// only and must never be cached for another physical send.
type PhysicalCallAdmitter interface {
	AdmitPhysicalCall(context.Context, PhysicalCall) error
}

// CredentialResolver returns one-use credential material for the exact effect
// attempt. Implementations must use the existing credential policy boundary;
// the runtime never loads raw credential storage directly.
type CredentialResolver interface {
	ResolveCredential(context.Context, PhysicalCall) (credentials.CredentialMaterial, error)
}

func admitPhysicalCall(ctx context.Context, admitter PhysicalCallAdmitter, call PhysicalCall) error {
	if admitter == nil {
		return ErrAdmissionRequired
	}
	deadline, ok := ctx.Deadline()
	if !ok || deadline.After(call.Deadline) {
		return fmt.Errorf("%w: physical send context is not bounded by the durable deadline", ErrInvalidPhysicalCall)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := admitter.AdmitPhysicalCall(ctx, call); err != nil {
		return &admissionError{cause: err}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}
