package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/oapi-codegen/nullable"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

func (service *Service) SubmitInvocation(ctx context.Context, request SubmitInvocationRequestObject) (SubmitInvocationResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if request.Body == nil || request.Params.IdempotencyKey == "" || request.Params.IdempotencyKey != request.Body.Protocol.RequestKey {
		return invalidRequestResponse(), nil
	}
	approval := contract.AbsentValue()
	if request.Body.ApprovalRef.IsSpecified() {
		if request.Body.ApprovalRef.IsNull() {
			approval = contract.NullValue()
		} else {
			value, err := request.Body.ApprovalRef.Get()
			if err != nil {
				return invalidRequestResponse(), nil
			}
			approval = contract.NewStringValue(value)
		}
	}
	result, err := service.admission.AcceptRun(ctx, state.identity.Actor, scope, app.RunInvocation{
		ConnectionRef:    request.Body.ConnectionRef,
		OperationRef:     request.Body.OperationRef,
		ExpectedRevision: request.Body.Protocol.ExpectedRevision,
		RequestKey:       request.Body.Protocol.RequestKey,
		Lane:             contract.Lane(request.Body.Lane),
		Input:            request.Body.Input,
		ResultControls:   request.Body.ResultControls,
		ApprovalRef:      approval,
	})
	if err != nil {
		return asMutationProblemResponse(err, request.Body.Protocol.RequestKey, api.SafeOutcomeReferenceKindRun, ""), nil
	}
	return SubmitInvocation202JSONResponse(api.AdmissionResponse{
		IntentDigest:     string(result.IntentDigest),
		OutcomeReference: api.SafeOutcomeReference{Kind: api.SafeOutcomeReferenceKindRun, Ref: result.RunID},
		Replay:           result.Replayed,
		RequestKey:       request.Body.Protocol.RequestKey,
		RunRef:           result.RunID,
		State:            api.AdmissionResponseStateAccepted,
	}), nil
}

func (service *Service) ListRuns(ctx context.Context, request ListRunsRequestObject) (ListRunsResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	operation := "runs"
	var stateFilter *contract.RunState
	if request.Params.State != nil {
		value := contract.RunState(*request.Params.State)
		stateFilter = &value
		operation += ":" + string(value)
	}
	after, pageSize, err := service.parsePage(operation, state.identity.Principal, scope, request.Params.Cursor, request.Params.PageSize)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	page, err := service.runs.ListRuns(ctx, state.identity.Actor, scope, after, stateFilter, pageSize)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	next, err := service.nextCursor(operation, state.identity.Principal, scope, pageSize, page.Next)
	if err != nil {
		return nil, err
	}
	items := make([]api.RunView, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, runResponse(item))
	}
	return ListRuns200JSONResponse(api.RunPage{Items: items, NextCursor: next}), nil
}

func (service *Service) GetRun(ctx context.Context, request GetRunRequestObject) (GetRunResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	view, err := service.runs.GetRun(ctx, state.identity.Actor, scope, request.RunId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	setETag(ctx, view.Record.Revision)
	return GetRun200JSONResponse{Body: runResponse(view)}, nil
}

func (service *Service) GetRunResult(ctx context.Context, request GetRunResultRequestObject) (GetRunResultResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	view, err := service.runs.GetRunResult(ctx, state.identity.Actor, scope, request.RunId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	setETag(ctx, view.Record.Revision)
	effects := effectSummaryResponse(view.Record.EffectSummary)
	return GetRunResult200JSONResponse{Body: api.RunResultResponse{
		RunRef:             view.Record.Spec.RunID,
		State:              api.RunState(view.Record.State),
		EffectSummary:      effects,
		UnknownEffectCount: effects.Unknown,
		Revision:           strconv.FormatUint(view.Record.Revision, 10),
		ResultReferences:   resultReferencesResponse(view.ResultRefs),
	}}, nil
}

func (service *Service) ListRunTasks(ctx context.Context, request ListRunTasksRequestObject) (ListRunTasksResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	operation := "run_tasks:" + request.RunId
	after, pageSize, err := service.parsePage(operation, state.identity.Principal, scope, request.Params.Cursor, request.Params.PageSize)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	page, err := service.runs.ListRunTasks(ctx, state.identity.Actor, scope, request.RunId, after, pageSize)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	next, err := service.nextCursor(operation, state.identity.Principal, scope, pageSize, page.Next)
	if err != nil {
		return nil, err
	}
	items := make([]api.TaskView, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, taskResponse(item))
	}
	return ListRunTasks200JSONResponse(api.TaskPage{Items: items, NextCursor: next}), nil
}

func (service *Service) GetTask(ctx context.Context, request GetTaskRequestObject) (GetTaskResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	ref, err := decodeTaskID(request.TaskId)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	view, err := service.runs.GetTask(ctx, state.identity.Actor, scope, ref)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	setETag(ctx, view.Record.Revision)
	return GetTask200JSONResponse{Body: taskResponse(view)}, nil
}

func (service *Service) CancelRun(ctx context.Context, request CancelRunRequestObject) (CancelRunResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if request.Body == nil {
		return invalidRequestResponse(), nil
	}
	control, err := parseMutationControl(request.Params.IdempotencyKey, request.Body.Protocol, request.Params.IfMatch)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	result, err := service.runs.RequestCancel(ctx, state.identity.Actor, scope, request.RunId, control)
	if err != nil {
		return asMutationProblemResponse(err, request.Body.Protocol.RequestKey, api.SafeOutcomeReferenceKindRun, request.RunId), nil
	}
	setETag(ctx, result.Revision)
	return CancelRun202JSONResponse(api.RunCancelResponse{
		CommandRef:       result.CommandID,
		RunRef:           result.RunID,
		CancelRequested:  true,
		ResultRevision:   strconv.FormatUint(result.Revision, 10),
		OutcomeReference: api.SafeOutcomeReference{Kind: api.SafeOutcomeReferenceKindRun, Ref: result.RunID},
		Replay:           result.Replayed,
	}), nil
}

func (service *Service) ResumeRun(ctx context.Context, request ResumeRunRequestObject) (ResumeRunResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if request.Body == nil {
		return invalidRequestResponse(), nil
	}
	control, err := parseMutationControl(request.Params.IdempotencyKey, request.Body.Protocol, request.Params.IfMatch)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	approval, err := requiredNullableString(request.Body.ApprovalRef)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	err = service.runs.RequestResume(ctx, state.identity.Actor, scope, request.RunId, request.Body.RecoveryGeneration, approval, control)
	if err == nil {
		return nil, errors.New("resume did not produce an authorized recovery outcome")
	}
	return asMutationProblemResponse(err, request.Body.Protocol.RequestKey, api.SafeOutcomeReferenceKindRun, request.RunId), nil
}

func runResponse(view app.RunView) api.RunView {
	record := view.Record
	effects := effectSummaryResponse(record.EffectSummary)
	return api.RunView{
		RunRef:         record.Spec.RunID,
		ConnectionRefs: cloneStrings(view.ConnectionIDs),
		Intent: api.RunIntentReference{
			Kind:     api.RunIntentReferenceKind(record.Spec.OperationOrPlanRef.Kind),
			Id:       record.Spec.OperationOrPlanRef.ID,
			Revision: record.Spec.OperationOrPlanRef.Revision,
		},
		Origin:                 api.RunOrigin(record.Spec.Origin),
		State:                  api.RunState(record.State),
		EffectSummary:          effects,
		UnknownEffectCount:     effects.Unknown,
		CancelRequested:        record.CancelRequested,
		OrchestrationCondition: api.OrchestrationCondition(record.OrchestrationCondition),
		ProjectionObservedAt:   record.ProjectionObservedAt,
		BlockedReason:          runBlockedReason(record.BlockedReason),
		Revision:               strconv.FormatUint(record.Revision, 10),
		ResultReferences:       resultReferencesResponse(view.ResultRefs),
	}
}

func taskResponse(view app.TaskView) api.TaskView {
	record := view.Record
	effects := effectSummaryResponse(record.EffectSummary)
	outcomes := make([]api.MemberOutcome, 0, len(view.Outcomes))
	for _, stored := range view.Outcomes {
		outcome := stored.Outcome
		outcomes = append(outcomes, api.MemberOutcome{
			MemberId:      string(outcome.MemberID),
			EffectKey:     stored.EffectRef,
			DispatchId:    string(outcome.DispatchID),
			State:         api.EffectState(outcome.State),
			ObservedAt:    outcome.ObservedAt,
			AssertionRefs: cloneStrings(outcome.AssertionRefs),
		})
	}
	return api.TaskView{
		TaskId:                 encodeTaskID(store.TaskRef{RunID: record.RunID, TaskID: record.TaskID}),
		RunRef:                 record.RunID,
		OperationId:            record.OperationID,
		Lane:                   api.TaskViewLane(record.Lane),
		BehaviorId:             record.BehaviorID,
		State:                  api.RunState(record.State),
		EffectSummary:          effects,
		UnknownEffectCount:     effects.Unknown,
		CancelRequested:        record.CancelRequested,
		OrchestrationCondition: api.OrchestrationCondition(record.OrchestrationCondition),
		ProjectionObservedAt:   record.ProjectionObservedAt,
		BlockedReason:          runBlockedReason(record.BlockedReason),
		Revision:               strconv.FormatUint(record.Revision, 10),
		MemberOutcomes:         outcomes,
	}
}

func effectSummaryResponse(summary contract.EffectSummary) api.EffectSummary {
	return api.EffectSummary{
		Unsent:    strconv.FormatUint(summary.Unsent, 10),
		Succeeded: strconv.FormatUint(summary.Succeeded, 10),
		Unchanged: strconv.FormatUint(summary.Unchanged, 10),
		Rejected:  strconv.FormatUint(summary.Rejected, 10),
		Unknown:   strconv.FormatUint(summary.Unknown, 10),
	}
}

func resultReferencesResponse(references []string) []api.RunResultReference {
	result := make([]api.RunResultReference, 0, len(references))
	for _, reference := range references {
		result = append(result, api.RunResultReference{ResultRef: reference})
	}
	return result
}

func runBlockedReason(reason string) nullable.Nullable[string] {
	if reason == "" {
		return nullable.NewNullNullable[string]()
	}
	return nullable.NewNullableWithValue(reason)
}

// The delimiter cannot occur in a stored identity. The byte framing preserves
// both exact IDs within the wire bound even when JSON quoting would expand them.
// It confers no authority: GetTask still authorizes the stored scoped run.
func encodeTaskID(ref store.TaskRef) string {
	return "task1_" + base64.RawURLEncoding.EncodeToString([]byte(ref.RunID+"\x00"+ref.TaskID))
}

func decodeTaskID(value string) (store.TaskRef, error) {
	if !validHTTPIdentifier(value, 2048) || !strings.HasPrefix(value, "task1_") {
		return store.TaskRef{}, errInvalidHTTPInput
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(value, "task1_"))
	if err != nil || len(decoded) > 1025 || !utf8.Valid(decoded) {
		return store.TaskRef{}, errInvalidHTTPInput
	}
	runID, taskID, found := strings.Cut(string(decoded), "\x00")
	if !found || runID == "" || taskID == "" || len(runID) > 512 || len(taskID) > 512 || strings.ContainsRune(taskID, '\x00') {
		return store.TaskRef{}, errInvalidHTTPInput
	}
	return store.TaskRef{RunID: runID, TaskID: taskID}, nil
}

func (r problemResponse) VisitListRunsResponse(w http.ResponseWriter) error     { return r.write(w) }
func (r problemResponse) VisitGetRunResponse(w http.ResponseWriter) error       { return r.write(w) }
func (r problemResponse) VisitGetRunResultResponse(w http.ResponseWriter) error { return r.write(w) }
func (r problemResponse) VisitListRunTasksResponse(w http.ResponseWriter) error { return r.write(w) }
func (r problemResponse) VisitGetTaskResponse(w http.ResponseWriter) error      { return r.write(w) }
func (r problemResponse) VisitCancelRunResponse(w http.ResponseWriter) error    { return r.write(w) }
func (r problemResponse) VisitResumeRunResponse(w http.ResponseWriter) error    { return r.write(w) }
