package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/oapi-codegen/nullable"
	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/spf13/cobra"
)

var errWatchReconciliationRequired = errors.New("run has an unknown effect and requires reconciliation")

// maximumRunResultDocumentBytes is the explicit finite bound for one published
// result document read into memory. The complete document must fit it: a
// larger document fails before any byte reaches stdout, and the exact stream
// remains readable through `pm artifacts download`, which does not buffer.
const maximumRunResultDocumentBytes = 16 << 20

var errRunResultDocumentExceedsBound = errors.New("run result document exceeds the client download bound")

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
		Short: "Render the authoritative published result document for one run",
		Long: `Render the authoritative action document one run published.

The command reads the run's result projection, then streams the exact published
document bytes for its single immutable result reference to stdout regardless of
--output or --json. The command adds no newline or JSON envelope, so every
nested value, explicit null, false, zero, empty value and exact
integer/decimal/identifier the server published is preserved byte for byte.
Selection and record filtering were already applied server-side.

Diagnostics remain on stderr. A run with no published document, or with more
than one published document, fails explicitly instead of printing an empty or
fabricated result. A document larger than the client bound fails explicitly;
read it with pm artifacts download instead.`,
		Args: exactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			return fetchAndRenderPublishedRunResult(ctx, command.OutOrStdout(), bundle, arguments[0])
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
			completed, err := watchRunToTerminal(ctx, command.OutOrStdout(), bundle, runID)
			if err != nil || !completed {
				return err
			}
			return fetchAndRenderRunResult(ctx, command.OutOrStdout(), bundle, runID)
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

// watchRunToTerminal follows one accepted run through its durable committed
// event stream, rendering every frame it delivers. It reuses the canonical
// watch semantics - committed-order replay, signed cursors, reconnect from the
// last consumed cursor - instead of a polling loop, and preserves the existing
// cancellation precedence: an interrupted view ends only this subscription and
// never requests durable cancellation, while a view deadline stays a view
// deadline. The boolean reports whether the caller may read the terminal
// projection; it is false exactly when the watch ended by cancellation,
// deadline or transport failure, which never triggers another view request.
func watchRunToTerminal(
	ctx context.Context,
	frames io.Writer,
	bundle clientBundle,
	runID string,
) (bool, error) {
	_, err := bundle.client.WatchEvents(
		ctx,
		pmapi.WorkspaceID(bundle.config.workspace),
		pmapi.RunID(runID),
		pmapi.WatchEventsOptions{Handle: func(frame pmapi.RunEventFrame) error {
			if err := Render(frames, bundle.config.output, frame); err != nil {
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
			return false, context.DeadlineExceeded
		}
		if errors.Is(err, context.Canceled) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// fetchRunResultResponse reads the run-result projection and applies the
// existing validation. A non-2xx response is returned with a nil JSON200 so its
// problem envelope keeps its original classification and exit code.
func fetchRunResultResponse(
	ctx context.Context,
	bundle clientBundle,
	runID string,
) (*pmapi.GetRunResultResponse, error) {
	response, err := bundle.client.API().GetRunResultWithResponse(
		ctx,
		pmapi.WorkspaceID(bundle.config.workspace),
		pmapi.RunID(runID),
	)
	if err != nil {
		return nil, requestError(ctx, err)
	}
	if response == nil || response.HTTPResponse == nil {
		return nil, pmapi.ErrMalformedEnvelope
	}
	if response.HTTPResponse.StatusCode >= 200 && response.HTTPResponse.StatusCode < 300 &&
		(response.JSON200 == nil || !validRunResult(response.JSON200, runID)) {
		return nil, pmapi.ErrMalformedEnvelope
	}
	return response, nil
}

// fetchAndRenderRunResult keeps the observation behaviour of the run
// projection: the validated result-reference listing is rendered, and the run's
// unresolved or terminal outcome keeps its existing exit code.
func fetchAndRenderRunResult(
	ctx context.Context,
	writer io.Writer,
	bundle clientBundle,
	runID string,
) error {
	response, err := fetchRunResultResponse(ctx, bundle, runID)
	if err != nil {
		return err
	}
	if err := renderWireResponse(writer, bundle.config.output, response); err != nil {
		return err
	}
	if response.JSON200 == nil {
		return pmapi.ErrMalformedEnvelope
	}
	return runResultStateError(response.JSON200)
}

// fetchAndRenderPublishedRunResult renders the run's authoritative published
// action document, then keeps the existing unresolved/terminal exit outcome.
// The published document is the only result content this command prints; a run
// without exactly one published document fails explicitly, and the existing
// uncertainty precedence still reports an unresolved outcome first.
func fetchAndRenderPublishedRunResult(
	ctx context.Context,
	writer io.Writer,
	bundle clientBundle,
	runID string,
) error {
	response, err := fetchRunResultResponse(ctx, bundle, runID)
	if err != nil {
		return err
	}
	if response.JSON200 == nil {
		envelope, err := pmapi.DecodeEnvelope(response)
		if err != nil {
			return err
		}
		if problem := pmapi.EnvelopeError(envelope); problem != nil {
			return problem
		}
		return pmapi.ErrMalformedEnvelope
	}
	documentErr := renderPublishedRunResultDocument(ctx, writer, bundle, response.JSON200)
	if stateErr := runResultStateError(response.JSON200); stateErr != nil {
		return stateErr
	}
	return documentErr
}

// renderPublishedRunResultDocument downloads the one immutable result document
// a run published and writes its exact bytes to writer. The document already
// carries the server-applied selection and record filtering, so the client
// never re-renders, re-serializes or re-filters it.
func renderPublishedRunResultDocument(
	ctx context.Context,
	writer io.Writer,
	bundle clientBundle,
	result *pmapi.RunResultResponse,
) error {
	if writer == nil {
		return errors.New("output writer is unavailable")
	}
	switch len(result.ResultReferences) {
	case 0:
		// The existing capability-unavailable failure: exit code 5 with a
		// sanitized diagnostic and no stdout content, never an empty or
		// fabricated document.
		return &exitError{
			code:    5,
			message: "run result is unavailable: this run has published no result document",
		}
	case 1:
	default:
		return &exitError{
			code: 5,
			message: fmt.Sprintf(
				"run result is unavailable: this run published %d result documents; read one exact document with pm artifacts download REF",
				len(result.ResultReferences),
			),
		}
	}
	document, err := downloadRunResultDocument(ctx, bundle, result.ResultReferences[0].ResultRef)
	if err != nil {
		return err
	}
	if _, err := writer.Write(document); err != nil {
		return err
	}
	return nil
}

// boundedRunResultBuffer holds one result document up to an explicit finite
// bound. A write beyond the bound fails the download instead of truncating it,
// and nothing is rendered from a buffer that did not complete.
type boundedRunResultBuffer struct {
	document  bytes.Buffer
	remaining int64
}

func (buffer *boundedRunResultBuffer) Write(chunk []byte) (int, error) {
	if int64(len(chunk)) > buffer.remaining {
		return 0, errRunResultDocumentExceedsBound
	}
	buffer.remaining -= int64(len(chunk))
	return buffer.document.Write(chunk)
}

// downloadRunResultDocument reads one exact published document through the
// qualified artifact download path. The server verifies the complete object's
// digest and length before any byte reaches the client, and the client bounds
// what it will hold: an oversized or failed document keeps its structured
// classification and prints nothing.
func downloadRunResultDocument(ctx context.Context, bundle clientBundle, resultRef string) ([]byte, error) {
	buffer := &boundedRunResultBuffer{remaining: maximumRunResultDocumentBytes}
	err := bundle.client.DownloadArtifact(
		ctx,
		pmapi.WorkspaceID(bundle.config.workspace),
		pmapi.ArtifactID(resultRef),
		"",
		buffer,
	)
	if errors.Is(err, errRunResultDocumentExceedsBound) {
		return nil, &exitError{
			code: 5,
			message: fmt.Sprintf(
				"run result document exceeds the %d byte client download bound; read it with pm artifacts download %s",
				maximumRunResultDocumentBytes,
				resultRef,
			),
		}
	}
	if err != nil {
		return nil, err
	}
	return buffer.document.Bytes(), nil
}

// runResultStateError maps one validated run-result projection to the existing
// unresolved/terminal exit outcome.
func runResultStateError(result *pmapi.RunResultResponse) error {
	if result.UnknownEffectCount != "0" || result.State == pmapi.RunStateReconciling {
		return &exitError{code: 7, message: "run outcome is unresolved; reconciliation is required"}
	}
	switch result.State {
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
