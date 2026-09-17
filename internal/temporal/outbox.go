package temporal

import (
	"context"
	"errors"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	sdkclient "go.temporal.io/sdk/client"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

const routeDeadlineGuard = 250 * time.Millisecond

var ErrRouteLeaseInsufficient = errors.New("workflow route lease is insufficient for another Temporal request")

type RelayConfig struct {
	Store        *store.Pool
	Client       *Client
	OwnerID      string
	ClaimLimit   uint32
	ClaimTTL     time.Duration
	IdleDelay    time.Duration
	FailureDelay time.Duration
}

type Relay struct {
	store        *store.Pool
	client       *Client
	ownerID      string
	claimLimit   uint32
	claimTTL     time.Duration
	idleDelay    time.Duration
	failureDelay time.Duration
}

type HandoffDisposition string

const (
	HandoffStarted          HandoffDisposition = "started"
	HandoffReconciled       HandoffDisposition = "reconciled"
	HandoffBindingConfirmed HandoffDisposition = "binding_confirmed"
	HandoffRepairRequired   HandoffDisposition = "repair_required"
	HandoffUnsafeToRestart  HandoffDisposition = "unsafe_to_restart"
)

type HandoffResult struct {
	Disposition   HandoffDisposition
	WorkflowID    string
	TemporalRunID string
	Generation    uint64
}

func NewRelay(configuration RelayConfig) (*Relay, error) {
	if configuration.Store == nil || configuration.Client == nil {
		return nil, errors.New("workflow relay store and client are required")
	}
	if !validTemporalIdentity(configuration.OwnerID) {
		return nil, errors.New("workflow relay owner identity is invalid")
	}
	if configuration.ClaimLimit == 0 || configuration.ClaimLimit > 32 {
		return nil, errors.New("workflow relay claim limit is invalid")
	}
	if configuration.ClaimTTL <= 0 || configuration.ClaimTTL > 5*time.Minute ||
		configuration.ClaimTTL%time.Microsecond != 0 ||
		configuration.ClaimTTL <= configuration.Client.config.RequestTimeout+routeDeadlineGuard {
		return nil, errors.New("workflow relay claim TTL is invalid")
	}
	if configuration.IdleDelay <= 0 || configuration.FailureDelay <= 0 ||
		configuration.IdleDelay > 5*time.Minute || configuration.FailureDelay > 5*time.Minute {
		return nil, errors.New("workflow relay delays are invalid")
	}
	return &Relay{
		store: configuration.Store, client: configuration.Client,
		ownerID: configuration.OwnerID, claimLimit: configuration.ClaimLimit,
		claimTTL: configuration.ClaimTTL, idleDelay: configuration.IdleDelay,
		failureDelay: configuration.FailureDelay,
	}, nil
}

// Run owns only the cancellable relay loop. Cancellation stops new claims and
// bounded RPCs; it never requests cancellation or termination of a workflow.
func (relay *Relay) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("workflow relay context is required")
	}
	for {
		worked, err := relay.runOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		delay := time.Duration(0)
		switch {
		case err != nil:
			delay = relay.failureDelay
		case !worked:
			delay = relay.idleDelay
		}
		if delay == 0 {
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil
		case <-timer.C:
		}
	}
}

func (relay *Relay) RunOnce(ctx context.Context) error {
	_, err := relay.runOnce(ctx)
	return err
}

func (relay *Relay) runOnce(ctx context.Context) (bool, error) {
	if relay == nil || relay.store == nil || relay.client == nil {
		return false, errors.New("workflow relay is required")
	}
	if ctx == nil {
		return false, errors.New("workflow relay context is required")
	}
	var routes []store.WorkflowRoute
	err := store.WithinTransaction(ctx, relay.store, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		routes, err = store.ClaimWorkflowRoutes(ctx, tx, store.WorkflowRouteRequest{
			OwnerID: relay.ownerID,
			Limit:   relay.claimLimit,
			TTL:     relay.claimTTL,
		})
		return err
	})
	if err != nil {
		return false, err
	}
	var routeErrors []error
	for _, route := range routes {
		if ctx.Err() != nil {
			return len(routes) != 0, errors.Join(append(routeErrors, ctx.Err())...)
		}
		if err := relay.processRoute(ctx, route); err != nil {
			if deferErr := relay.deferRoute(ctx, route); deferErr != nil && !errors.Is(deferErr, store.ErrOutboxClaimLost) {
				err = errors.Join(err, deferErr)
			}
			routeErrors = append(routeErrors, err)
		}
	}
	return len(routes) != 0, errors.Join(routeErrors...)
}

func (relay *Relay) processRoute(ctx context.Context, route store.WorkflowRoute) error {
	switch route.Claim.Command.Kind {
	case store.WorkflowCommandStartRun:
		_, err := relay.StartOrReconcile(ctx, route)
		return err
	case store.WorkflowCommandEnsureBinding:
		_, err := relay.EnsureWorkflowBinding(ctx, route)
		return err
	case store.WorkflowCommandRepair:
		_, err := relay.RepairProjection(ctx, route)
		return err
	default:
		return errors.New("unsupported workflow route kind")
	}
}

func (relay *Relay) deferRoute(ctx context.Context, route store.WorkflowRoute) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	retryAt := time.Now().UTC().Add(relay.failureDelay)
	return store.WithinScope(ctx, relay.store, route.Claim.Scope, route.Actor, func(ctx context.Context, tx *store.Transaction) error {
		return store.DeferWorkflowCommand(ctx, tx, route.Claim, retryAt)
	})
}

// StartOrReconcile executes only an API/recovery start command. It describes
// before starting, seals all idempotent inputs, and accepts an existing
// execution only after exact start-history reconciliation.
func (relay *Relay) StartOrReconcile(ctx context.Context, route store.WorkflowRoute) (HandoffResult, error) {
	if route.Claim.Command.Kind != store.WorkflowCommandStartRun {
		return HandoffResult{}, errors.New("start reconciliation requires a start_run route")
	}
	handoff, intent, err := relay.sealRoute(ctx, route)
	if err != nil {
		return HandoffResult{}, err
	}
	if terminalRunState(handoff.Record.State) || handoff.Record.WorkflowBinding.Generation != intent.Plan.Generation {
		return HandoffResult{
			Disposition: HandoffUnsafeToRestart,
			WorkflowID:  intent.Plan.WorkflowID,
			Generation:  intent.Plan.Generation,
		}, relay.markUnknown(ctx, route, intent, store.WorkflowWitnessBindingMismatch, "")
	}
	input, err := startRunInput(route.Claim, intent)
	if err != nil {
		return HandoffResult{}, err
	}

	if len(handoff.Record.WorkflowBinding.Observations) != 0 {
		currentRunID := handoff.Record.WorkflowBinding.CurrentTemporalRunID()
		execution, err := relay.lookupWorkflowExecution(ctx, route, intent.Plan.WorkflowID, currentRunID, true)
		if err != nil {
			if workflowNotFound(err) {
				markErr := relay.markUnknown(ctx, route, intent, store.WorkflowWitnessPriorBoundNotFound, currentRunID)
				return HandoffResult{Disposition: HandoffUnsafeToRestart, WorkflowID: intent.Plan.WorkflowID, TemporalRunID: currentRunID, Generation: intent.Plan.Generation}, markErr
			}
			if workflowEvidenceMismatch(err) {
				markErr := relay.markUnknown(ctx, route, intent, store.WorkflowWitnessBindingMismatch, currentRunID)
				return HandoffResult{Disposition: HandoffRepairRequired, WorkflowID: intent.Plan.WorkflowID, TemporalRunID: currentRunID, Generation: intent.Plan.Generation}, markErr
			}
			return HandoffResult{}, err
		}
		return relay.reconcileStartedExecution(ctx, route, intent, input, execution, HandoffReconciled)
	}

	execution, err := relay.lookupWorkflowExecution(ctx, route, intent.Plan.WorkflowID, "", true)
	if err == nil {
		return relay.reconcileStartedExecution(ctx, route, intent, input, execution, HandoffReconciled)
	}
	if workflowEvidenceMismatch(err) {
		markErr := relay.markUnknown(ctx, route, intent, store.WorkflowWitnessBindingMismatch, "")
		return HandoffResult{Disposition: HandoffRepairRequired, WorkflowID: intent.Plan.WorkflowID, Generation: intent.Plan.Generation}, markErr
	}
	if !workflowNotFound(err) {
		return HandoffResult{}, err
	}
	if err := store.WithinScope(ctx, relay.store, route.Claim.Scope, route.Actor, func(ctx context.Context, tx *store.Transaction) error {
		_, err := store.PersistNoDispatchWitness(ctx, tx, route.Claim, intent.Digest)
		return err
	}); err != nil {
		if errors.Is(err, store.ErrWorkflowRestartUnsafe) {
			return HandoffResult{Disposition: HandoffUnsafeToRestart, WorkflowID: intent.Plan.WorkflowID, Generation: intent.Plan.Generation}, nil
		}
		return HandoffResult{}, err
	}

	workflowRun, err := routeRPC(ctx, relay, route, func(callCtx context.Context, backend WorkflowClient) (sdkclient.WorkflowRun, error) {
		return backend.ExecuteWorkflow(callCtx, sdkclient.StartWorkflowOptions{
			ID:                                       intent.Plan.WorkflowID,
			TaskQueue:                                intent.Plan.TaskQueue,
			WorkflowExecutionTimeout:                 intent.Plan.ExecutionTimeout,
			WorkflowIDConflictPolicy:                 enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
			WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
			WorkflowExecutionErrorWhenAlreadyStarted: true,
		}, intent.Plan.WorkflowType, input)
	})
	if err != nil {
		var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
		if !errors.As(err, &alreadyStarted) {
			return HandoffResult{}, err
		}
		execution, lookupErr := relay.lookupWorkflowExecution(ctx, route, intent.Plan.WorkflowID, "", true)
		if lookupErr != nil {
			if workflowNotFound(lookupErr) {
				markErr := relay.markUnknown(ctx, route, intent, store.WorkflowWitnessPriorBoundNotFound, "")
				return HandoffResult{Disposition: HandoffUnsafeToRestart, WorkflowID: intent.Plan.WorkflowID, Generation: intent.Plan.Generation}, markErr
			}
			if workflowEvidenceMismatch(lookupErr) {
				markErr := relay.markUnknown(ctx, route, intent, store.WorkflowWitnessBindingMismatch, "")
				return HandoffResult{Disposition: HandoffRepairRequired, WorkflowID: intent.Plan.WorkflowID, Generation: intent.Plan.Generation}, markErr
			}
			return HandoffResult{}, lookupErr
		}
		return relay.reconcileStartedExecution(ctx, route, intent, input, execution, HandoffReconciled)
	}
	if workflowRun == nil || workflowRun.GetID() != intent.Plan.WorkflowID ||
		!validTemporalIdentity(workflowRun.GetRunID()) {
		return HandoffResult{}, ErrWorkflowBindingMismatch
	}
	execution, lookupErr := relay.lookupWorkflowExecution(
		ctx, route, intent.Plan.WorkflowID, workflowRun.GetRunID(), true,
	)
	if lookupErr != nil {
		if workflowNotFound(lookupErr) {
			markErr := relay.markUnknown(
				ctx, route, intent, store.WorkflowWitnessPriorBoundNotFound, workflowRun.GetRunID(),
			)
			return HandoffResult{
				Disposition: HandoffUnsafeToRestart,
				WorkflowID:  intent.Plan.WorkflowID, TemporalRunID: workflowRun.GetRunID(),
				Generation: intent.Plan.Generation,
			}, markErr
		}
		if workflowEvidenceMismatch(lookupErr) {
			markErr := relay.markUnknown(
				ctx, route, intent, store.WorkflowWitnessBindingMismatch, workflowRun.GetRunID(),
			)
			return HandoffResult{
				Disposition: HandoffRepairRequired,
				WorkflowID:  intent.Plan.WorkflowID, TemporalRunID: workflowRun.GetRunID(),
				Generation: intent.Plan.Generation,
			}, markErr
		}
		return HandoffResult{}, lookupErr
	}
	if reportedFirstExecutionRunID := workflowRun.GetFirstExecutionRunID(); reportedFirstExecutionRunID != "" &&
		(!validTemporalIdentity(reportedFirstExecutionRunID) ||
			reportedFirstExecutionRunID != execution.FirstExecutionRunID) {
		markErr := relay.markUnknown(
			ctx, route, intent, store.WorkflowWitnessBindingMismatch, execution.RunID,
		)
		return HandoffResult{
			Disposition: HandoffRepairRequired,
			WorkflowID:  intent.Plan.WorkflowID, TemporalRunID: execution.RunID,
			Generation: intent.Plan.Generation,
		}, markErr
	}
	return relay.reconcileStartedExecution(ctx, route, intent, input, execution, HandoffStarted)
}

// EnsureWorkflowBinding confirms only the exact already-adopted scheduled
// execution. This path has no ExecuteWorkflow capability or fallback.
func (relay *Relay) EnsureWorkflowBinding(ctx context.Context, route store.WorkflowRoute) (HandoffResult, error) {
	if route.Claim.Command.Kind != store.WorkflowCommandEnsureBinding {
		return HandoffResult{}, errors.New("binding reconciliation requires an ensure_workflow_binding route")
	}
	handoff, intent, err := relay.sealRoute(ctx, route)
	if err != nil {
		return HandoffResult{}, err
	}
	binding := handoff.Record.WorkflowBinding
	if binding.Origin != contract.RunOriginSchedule || !binding.Adopted || len(binding.Observations) == 0 {
		return HandoffResult{}, store.ErrWorkflowBindingConflict
	}
	currentRunID := binding.CurrentTemporalRunID()
	execution, err := relay.lookupWorkflowExecution(ctx, route, binding.WorkflowID, currentRunID, false)
	if err != nil {
		if workflowNotFound(err) {
			markErr := relay.markUnknown(ctx, route, intent, store.WorkflowWitnessPriorBoundNotFound, currentRunID)
			return HandoffResult{Disposition: HandoffUnsafeToRestart, WorkflowID: binding.WorkflowID, TemporalRunID: currentRunID, Generation: binding.Generation}, markErr
		}
		if workflowEvidenceMismatch(err) {
			markErr := relay.markUnknown(ctx, route, intent, store.WorkflowWitnessBindingMismatch, currentRunID)
			return HandoffResult{Disposition: HandoffRepairRequired, WorkflowID: binding.WorkflowID, TemporalRunID: currentRunID, Generation: binding.Generation}, markErr
		}
		return HandoffResult{}, err
	}
	if err := verifyPersistedChain(execution, binding); err != nil ||
		execution.WorkflowType != intent.Plan.WorkflowType ||
		execution.TaskQueue != intent.Plan.TaskQueue ||
		execution.ExecutionTimeout != temporalDuration(intent.Plan.ExecutionTimeout) {
		markErr := relay.markUnknown(ctx, route, intent, store.WorkflowWitnessBindingMismatch, execution.RunID)
		return HandoffResult{Disposition: HandoffRepairRequired, WorkflowID: binding.WorkflowID, TemporalRunID: execution.RunID, Generation: binding.Generation}, markErr
	}
	result, err := relay.commitBinding(ctx, route, intent, execution.RunID, execution.FirstExecutionRunID, orchestrationCondition(execution.Status))
	if err != nil {
		return HandoffResult{}, err
	}
	result.Disposition = HandoffBindingConfirmed
	return result, nil
}

func (relay *Relay) sealRoute(ctx context.Context, route store.WorkflowRoute) (store.WorkflowHandoff, store.WorkflowCommandIntent, error) {
	var handoff store.WorkflowHandoff
	var intent store.WorkflowCommandIntent
	err := store.WithinScope(ctx, relay.store, route.Claim.Scope, route.Actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		handoff, err = store.LoadWorkflowHandoff(ctx, tx, route.Claim)
		if err != nil {
			return err
		}
		plan, err := workflowCommandPlan(handoff, relay.client.config, route.Claim.Command.Kind)
		if err != nil {
			return err
		}
		intent, err = store.SealWorkflowCommand(ctx, tx, route.Claim, plan)
		return err
	})
	return handoff, intent, err
}

func (relay *Relay) reconcileStartedExecution(
	ctx context.Context,
	route store.WorkflowRoute,
	intent store.WorkflowCommandIntent,
	input contract.StartRunInput,
	execution workflowExecution,
	disposition HandoffDisposition,
) (HandoffResult, error) {
	if err := verifyStartedExecution(execution, input, intent); err != nil {
		markErr := relay.markUnknown(ctx, route, intent, store.WorkflowWitnessBindingMismatch, execution.RunID)
		return HandoffResult{Disposition: HandoffRepairRequired, WorkflowID: intent.Plan.WorkflowID, TemporalRunID: execution.RunID, Generation: intent.Plan.Generation}, markErr
	}
	result, err := relay.commitBinding(ctx, route, intent, execution.RunID, execution.FirstExecutionRunID, orchestrationCondition(execution.Status))
	if err != nil {
		return HandoffResult{}, err
	}
	result.Disposition = disposition
	return result, nil
}

func (relay *Relay) commitBinding(
	ctx context.Context,
	route store.WorkflowRoute,
	intent store.WorkflowCommandIntent,
	temporalRunID string,
	firstExecutionRunID string,
	condition contract.OrchestrationCondition,
) (HandoffResult, error) {
	err := store.WithinScope(ctx, relay.store, route.Claim.Scope, route.Actor, func(ctx context.Context, tx *store.Transaction) error {
		_, err := store.CommitWorkflowBinding(ctx, tx, store.WorkflowBindingCommit{
			Claim: route.Claim, IntentDigest: intent.Digest,
			TemporalRunID: temporalRunID, FirstExecutionRunID: firstExecutionRunID,
			Condition: condition,
		})
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrCommitOutcomeUnknown) {
			var committed bool
			reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), relay.client.config.RequestTimeout)
			defer cancel()
			reconcileErr := store.WithinScope(reconcileCtx, relay.store, route.Claim.Scope, route.Actor, func(ctx context.Context, tx *store.Transaction) error {
				var loadErr error
				committed, loadErr = store.WorkflowBindingCommitted(
					ctx, tx, route.Claim, intent.Digest, temporalRunID, firstExecutionRunID,
				)
				return loadErr
			})
			if reconcileErr != nil {
				return HandoffResult{}, errors.Join(err, reconcileErr)
			}
			if committed {
				return HandoffResult{
					WorkflowID: intent.Plan.WorkflowID, TemporalRunID: temporalRunID,
					Generation: intent.Plan.Generation,
				}, nil
			}
		}
		return HandoffResult{}, err
	}
	return HandoffResult{
		WorkflowID: intent.Plan.WorkflowID, TemporalRunID: temporalRunID,
		Generation: intent.Plan.Generation,
	}, nil
}

func (relay *Relay) markUnknown(
	ctx context.Context,
	route store.WorkflowRoute,
	intent store.WorkflowCommandIntent,
	kind store.WorkflowWitnessKind,
	temporalRunID string,
) error {
	return store.WithinScope(ctx, relay.store, route.Claim.Scope, route.Actor, func(ctx context.Context, tx *store.Transaction) error {
		_, err := store.MarkWorkflowCommandUnknown(ctx, tx, route.Claim, intent.Digest, kind, temporalRunID)
		return err
	})
}

func (relay *Relay) prepareRouteRPC(ctx context.Context, route store.WorkflowRoute) (context.Context, context.CancelFunc, error) {
	if ctx == nil {
		return nil, nil, errors.New("workflow route context is required")
	}
	checkedAt := time.Now()
	var remaining time.Duration
	if err := store.WithinScope(ctx, relay.store, route.Claim.Scope, route.Actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		remaining, err = store.WorkflowClaimRemaining(ctx, tx, route.Claim)
		return err
	}); err != nil {
		return nil, nil, err
	}
	remaining -= time.Since(checkedAt)
	if remaining <= relay.client.config.RequestTimeout+routeDeadlineGuard {
		return nil, nil, ErrRouteLeaseInsufficient
	}
	requestDeadline := time.Now().Add(relay.client.config.RequestTimeout)
	routeDeadline := time.Now().Add(remaining - routeDeadlineGuard)
	if requestDeadline.After(routeDeadline) {
		requestDeadline = routeDeadline
	}
	callCtx, cancel := context.WithDeadline(ctx, requestDeadline)
	return callCtx, cancel, nil
}

func routeRPC[T any](
	ctx context.Context,
	relay *Relay,
	route store.WorkflowRoute,
	call func(context.Context, WorkflowClient) (T, error),
) (T, error) {
	var zero T
	if relay == nil || relay.client == nil || call == nil {
		return zero, errors.New("workflow route RPC is invalid")
	}
	callCtx, cancel, err := relay.prepareRouteRPC(ctx, route)
	if err != nil {
		return zero, err
	}
	defer cancel()
	if err := callCtx.Err(); err != nil {
		return zero, err
	}
	backend, err := relay.client.connection(callCtx)
	if err != nil {
		return zero, err
	}
	if err := callCtx.Err(); err != nil {
		return zero, err
	}
	requestCtx, requestCancel, err := relay.client.requestContext(callCtx)
	if err != nil {
		return zero, err
	}
	defer requestCancel()
	if err := requestCtx.Err(); err != nil {
		return zero, err
	}
	return call(requestCtx, backend)
}

func orchestrationCondition(status enumspb.WorkflowExecutionStatus) contract.OrchestrationCondition {
	switch status {
	case enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING:
		return contract.OrchestrationRunning
	case enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED:
		return contract.OrchestrationCompleted
	case enumspb.WORKFLOW_EXECUTION_STATUS_FAILED:
		return contract.OrchestrationFailed
	case enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED:
		return contract.OrchestrationCancelled
	case enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED:
		return contract.OrchestrationTerminated
	case enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT:
		return contract.OrchestrationTimedOut
	case enumspb.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW:
		return contract.OrchestrationProjectionPending
	default:
		return contract.OrchestrationUnknown
	}
}

func terminalRunState(state contract.RunState) bool {
	switch state {
	case contract.RunStateSucceeded, contract.RunStatePartial, contract.RunStateFailed, contract.RunStateCancelled:
		return true
	default:
		return false
	}
}
