package cli

import (
	"context"
	"errors"
	"strings"

	"github.com/oapi-codegen/nullable"
	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/spf13/cobra"
)

var errWatchReconciliationRequired = errors.New("run has an unknown effect and requires reconciliation")

// NewRunsCommands returns the one server-backed runs namespace. It exposes
// observation and explicit durable controls only; connector invocation is
// generated separately and is not a generic raw run submission surface.
func NewRunsCommands(runtime *commandRuntime) *cobra.Command {
	command := &cobra.Command{Use: "runs", Short: "Observe and control server-owned durable runs", Args: noArgs}
	command.AddCommand(newRunsListCommand(runtime))
	command.AddCommand(newRunInspectCommand(runtime))
	command.AddCommand(newRunResultCommand(runtime))
	command.AddCommand(newRunTasksCommand(runtime))
	command.AddCommand(newRunWatchCommand(runtime))
	command.AddCommand(newRunCancelCommand(runtime))
	command.AddCommand(newRunResumeCommand(runtime))
	return command
}

func newRunsListCommand(runtime *commandRuntime) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List runs in the explicitly selected workspace",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			response, err := bundle.client.API().ListRunsWithResponse(ctx, pmapi.WorkspaceID(bundle.config.workspace), nil)
			if err != nil {
				return requestError(ctx, err)
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	}
}

func newRunInspectCommand(runtime *commandRuntime) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect RUN_ID",
		Short: "Inspect one run in the explicitly selected workspace",
		Args:  exactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			response, err := bundle.client.API().GetRunWithResponse(
				ctx,
				pmapi.WorkspaceID(bundle.config.workspace),
				pmapi.RunID(arguments[0]),
			)
			if err != nil {
				return requestError(ctx, err)
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	}
}

func newRunResultCommand(runtime *commandRuntime) *cobra.Command {
	return &cobra.Command{
		Use:   "result RUN_ID",
		Short: "Render the authoritative result projection for one run",
		Args:  exactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			return fetchAndRenderRunResult(ctx, command, bundle, arguments[0])
		},
	}
}

func newRunTasksCommand(runtime *commandRuntime) *cobra.Command {
	return &cobra.Command{
		Use:   "tasks RUN_ID",
		Short: "Render durable tasks and exact member outcomes for one run",
		Args:  exactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			response, err := bundle.client.API().ListRunTasksWithResponse(
				ctx,
				pmapi.WorkspaceID(bundle.config.workspace),
				pmapi.RunID(arguments[0]),
				nil,
			)
			if err != nil {
				return requestError(ctx, err)
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	}
}

func newRunWatchCommand(runtime *commandRuntime) *cobra.Command {
	return &cobra.Command{
		Use:   "watch RUN_ID",
		Short: "Watch durable run events without cancelling accepted work",
		Args:  exactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			runID := arguments[0]
			_, err = bundle.client.WatchEvents(
				ctx,
				pmapi.WorkspaceID(bundle.config.workspace),
				pmapi.RunID(runID),
				pmapi.WatchEventsOptions{Handle: func(frame pmapi.RunEventFrame) error {
					if err := Render(command.OutOrStdout(), bundle.config.output, frame); err != nil {
						return err
					}
					if frame.Snapshot != nil && frame.Snapshot.Run.UnknownEffectCount != "0" {
						return errWatchReconciliationRequired
					}
					return nil
				}},
			)
			if err != nil && !errors.Is(err, errWatchReconciliationRequired) {
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return context.DeadlineExceeded
				}
				if errors.Is(err, context.Canceled) {
					return nil
				}
				return err
			}
			return fetchAndRenderRunResult(ctx, command, bundle, runID)
		},
	}
}

func newRunCancelCommand(runtime *commandRuntime) *cobra.Command {
	control := newMutationControl()
	command := &cobra.Command{
		Use:   "cancel RUN_ID",
		Short: "Persist an explicit authorized run cancellation request",
		Args:  exactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			runID := arguments[0]
			protocol, parameters, err := control.protocol("run.cancel", bundle.config.workspace, runID, struct{}{})
			if err != nil {
				return err
			}
			request := pmapi.RunCancelRequest{Protocol: protocol}
			params := &pmapi.CancelRunParams{IdempotencyKey: parameters.key, IfMatch: parameters.revision}
			response, err := sendRecoveredMutation(
				ctx,
				runtime,
				bundle,
				"run.cancel",
				runID,
				protocol,
				request,
				func(attemptContext context.Context) (pmapi.RecoveryAttemptResult, error) {
					value, sendErr := bundle.client.API().CancelRunWithResponse(
						attemptContext,
						pmapi.WorkspaceID(bundle.config.workspace),
						pmapi.RunID(runID),
						params,
						request,
					)
					if sendErr != nil {
						return pmapi.RecoveryAttemptResult{}, sendErr
					}
					if value == nil || value.HTTPResponse == nil {
						return pmapi.RecoveryAttemptResult{}, pmapi.ErrClientTransport
					}
					return pmapi.RecoveryAttemptResult{
						Response:   value,
						RetryAfter: value.HTTPResponse.Header.Get("Retry-After"),
					}, nil
				},
			)
			if err != nil {
				return err
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	}
	addMutationFlags(command, control)
	return command
}

func newRunResumeCommand(runtime *commandRuntime) *cobra.Command {
	control := newMutationControl()
	var recoveryGeneration, approvalRef singleString
	recoveryGeneration.name = "recovery-generation"
	approvalRef.name = "approval-ref"
	command := &cobra.Command{
		Use:   "resume RUN_ID",
		Short: "Request one explicit authorized recovery generation",
		Args:  exactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			if !recoveryGeneration.set {
				return usageFailure(errors.New("--recovery-generation is required"))
			}
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			runID := arguments[0]
			approval := nullable.NewNullNullable[string]()
			var approvalValue *string
			if approvalRef.set {
				approval = nullable.NewNullableWithValue(approvalRef.value)
				approvalValue = &approvalRef.value
			}
			intent := struct {
				ApprovalRef        *string `json:"approval_ref"`
				RecoveryGeneration string  `json:"recovery_generation"`
			}{ApprovalRef: approvalValue, RecoveryGeneration: recoveryGeneration.value}
			protocol, parameters, err := control.protocol("run.resume", bundle.config.workspace, runID, intent)
			if err != nil {
				return err
			}
			request := pmapi.RunResumeRequest{
				ApprovalRef:        approval,
				Protocol:           protocol,
				RecoveryGeneration: recoveryGeneration.value,
			}
			params := &pmapi.ResumeRunParams{IdempotencyKey: parameters.key, IfMatch: parameters.revision}
			response, err := sendRecoveredMutation(
				ctx,
				runtime,
				bundle,
				"run.resume",
				runID,
				protocol,
				request,
				func(attemptContext context.Context) (pmapi.RecoveryAttemptResult, error) {
					value, sendErr := bundle.client.API().ResumeRunWithResponse(
						attemptContext,
						pmapi.WorkspaceID(bundle.config.workspace),
						pmapi.RunID(runID),
						params,
						request,
					)
					if sendErr != nil {
						return pmapi.RecoveryAttemptResult{}, sendErr
					}
					if value == nil || value.HTTPResponse == nil {
						return pmapi.RecoveryAttemptResult{}, pmapi.ErrClientTransport
					}
					return pmapi.RecoveryAttemptResult{
						Response:   value,
						RetryAfter: value.HTTPResponse.Header.Get("Retry-After"),
					}, nil
				},
			)
			if err != nil {
				return err
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	}
	addMutationFlags(command, control)
	command.Flags().Var(&recoveryGeneration, recoveryGeneration.name, "Exact recovery-generation identity")
	command.Flags().Var(&approvalRef, approvalRef.name, "Explicit approval identity when required")
	return command
}

func fetchAndRenderRunResult(
	ctx context.Context,
	command *cobra.Command,
	bundle clientBundle,
	runID string,
) error {
	response, err := bundle.client.API().GetRunResultWithResponse(
		ctx,
		pmapi.WorkspaceID(bundle.config.workspace),
		pmapi.RunID(runID),
	)
	if err != nil {
		return requestError(ctx, err)
	}
	if response == nil || response.HTTPResponse == nil {
		return pmapi.ErrMalformedEnvelope
	}
	if response.HTTPResponse.StatusCode >= 200 && response.HTTPResponse.StatusCode < 300 &&
		(response.JSON200 == nil || !validRunResult(response.JSON200, runID)) {
		return pmapi.ErrMalformedEnvelope
	}
	if err := renderWireResponse(command.OutOrStdout(), bundle.config.output, response); err != nil {
		return err
	}
	if response.JSON200 == nil {
		return pmapi.ErrMalformedEnvelope
	}
	if response.JSON200.UnknownEffectCount != "0" || response.JSON200.State == pmapi.RunStateReconciling {
		return &exitError{code: 7, message: "run outcome is unresolved; reconciliation is required"}
	}
	switch response.JSON200.State {
	case pmapi.RunStateSucceeded:
		return nil
	case pmapi.RunStatePartial:
		return &exitError{code: 6, message: "run completed with a partial result"}
	case pmapi.RunStateFailed:
		return &exitError{code: 6, message: "run failed"}
	case pmapi.RunStateCancelled:
		return &exitError{code: 6, message: "run was cancelled"}
	default:
		return nil
	}
}

func validRunResult(result *pmapi.RunResultResponse, runID string) bool {
	if result == nil || result.RunRef != runID || !result.State.Valid() || result.ResultReferences == nil ||
		result.UnknownEffectCount != result.EffectSummary.Unknown {
		return false
	}
	for _, value := range []string{
		result.Revision,
		result.EffectSummary.Rejected,
		result.EffectSummary.Succeeded,
		result.EffectSummary.Unchanged,
		result.EffectSummary.Unknown,
		result.EffectSummary.Unsent,
		result.UnknownEffectCount,
	} {
		if !canonicalRunCounter(value) {
			return false
		}
	}
	if result.Revision == "0" {
		return false
	}
	references := make(map[string]struct{}, len(result.ResultReferences))
	for _, reference := range result.ResultReferences {
		if !canonicalRunIdentity(reference.ResultRef) {
			return false
		}
		if _, duplicate := references[reference.ResultRef]; duplicate {
			return false
		}
		references[reference.ResultRef] = struct{}{}
	}
	return true
}

func canonicalRunCounter(value string) bool {
	if value == "" || len(value) > 20 || len(value) > 1 && value[0] == '0' {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func canonicalRunIdentity(value string) bool {
	return value != "" && len(value) <= 512 && strings.TrimSpace(value) == value
}
