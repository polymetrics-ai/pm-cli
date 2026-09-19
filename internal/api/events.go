package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	maximumEventCursorBytes = 4096
	eventReconnectDelay     = 100 * time.Millisecond
)

var (
	ErrMalformedEventStream = errors.New("PM event stream is malformed")
	errEventStreamClosed    = errors.New("PM event stream closed before terminal truth")
)

// RunEventFrame is one canonical SSE frame. Exactly one of Snapshot or
// RunEvent is present. Cursor is populated only for a committed run event;
// snapshots carry their recovery cursor inside Snapshot.
type RunEventFrame struct {
	Kind     string                `json:"event"`
	Cursor   EventCursor           `json:"cursor,omitempty"`
	Snapshot *RunEventSnapshotData `json:"snapshot,omitempty"`
	RunEvent *RunEventData         `json:"run_event,omitempty"`
}

// WatchEventsOptions requires a synchronous consumer so delivery remains
// bounded by the caller. Cursor is the last fully consumed opaque SSE ID, or a
// snapshot recovery cursor returned after an explicit retention gap.
type WatchEventsOptions struct {
	Cursor EventCursor
	Handle func(RunEventFrame) error
}

// WatchEventsResult identifies the last fully consumed committed cursor and
// the latest authoritative run snapshot. A non-nil error never implies run
// cancellation or any other durable control action.
type WatchEventsResult struct {
	Cursor   EventCursor
	Snapshot RunEventSnapshotData
}

// WatchEvents consumes the canonical scoped SSE route, reconnecting from only
// the last fully delivered cursor. A retention gap advances that cursor only
// after its authoritative snapshot is delivered. The method returns normally
// after a terminal snapshot and the corresponding committed replay finish.
func (client *QualifiedClient) WatchEvents(
	ctx context.Context,
	workspaceID WorkspaceID,
	runID RunID,
	options WatchEventsOptions,
) (WatchEventsResult, error) {
	if client == nil || client.generated == nil || ctx == nil || options.Handle == nil ||
		!validIdentity(string(workspaceID), 512) || !validIdentity(string(runID), 512) ||
		!client.principal.Kind.Valid() || !validIdentity(client.principal.PrincipalId, 512) ||
		!validEventCursor(options.Cursor, true) {
		return WatchEventsResult{}, ErrUnsafeClientRequest
	}
	if err := ctx.Err(); err != nil {
		return WatchEventsResult{}, err
	}
	control, present := OperationControlFor("watchRunEvents")
	routeOperation, routed := OperationIDForRoute(http.MethodGet, "/v1/workspaces/{workspace_id}/runs/{run_id}/events")
	if !present || control.Method != http.MethodGet || control.Path != "/v1/workspaces/{workspace_id}/runs/{run_id}/events" ||
		control.HasBody || control.HasProtocol || control.RequiresIdempotencyKey || control.RequiresIfMatch ||
		len(control.QueryFields) != 0 || !routed || routeOperation != "watchRunEvents" {
		return WatchEventsResult{}, ErrCompatibility
	}
	generated, ok := client.generated.ClientInterface.(*Client)
	if !ok || generated == nil || generated.Client == nil {
		return WatchEventsResult{}, ErrCompatibility
	}
	state := eventWatchState{runID: runID, cursor: options.Cursor}
	for {
		response, err := openEventResponse(ctx, generated, workspaceID, runID, state.cursor)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return state.result(), contextErr
			}
			if !errors.Is(err, ErrClientTransport) {
				return state.result(), err
			}
			if waitErr := waitForEventReconnect(ctx); waitErr != nil {
				return state.result(), waitErr
			}
			continue
		}
		if response.StatusCode != http.StatusOK {
			err = decodeEventProblem(response)
			return state.result(), err
		}
		if !canonicalEventStreamResponse(response) {
			_ = response.Body.Close()
			return state.result(), ErrMalformedEventStream
		}
		err = consumeEventResponse(ctx, response.Body, &state, options.Handle)
		_ = response.Body.Close()
		if err == nil {
			return state.result(), nil
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return state.result(), contextErr
		}
		if !errors.Is(err, errEventStreamClosed) {
			return state.result(), err
		}
		if state.terminal && state.haveSequence && state.lastSequence == state.watermark {
			return state.result(), nil
		}
		if err := waitForEventReconnect(ctx); err != nil {
			return state.result(), err
		}
	}
}

func openEventResponse(
	ctx context.Context,
	generated *Client,
	workspaceID WorkspaceID,
	runID RunID,
	cursor EventCursor,
) (*http.Response, error) {
	serverURL, err := url.Parse(generated.Server)
	if err != nil {
		return nil, ErrCompatibility
	}
	path := "/v1/workspaces/" + url.PathEscape(string(workspaceID)) + "/runs/" + url.PathEscape(string(runID)) + "/events"
	if strings.HasPrefix(path, "/") {
		path = "." + path
	}
	requestURL, err := serverURL.Parse(path)
	if err != nil {
		return nil, ErrCompatibility
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, ErrUnsafeClientRequest
	}
	request.Header.Set("Accept", "text/event-stream")
	if cursor != "" {
		request.Header.Set("Last-Event-ID", cursor)
	}
	if err := generated.applyEditors(ctx, request, nil); err != nil {
		return nil, err
	}
	doer := generated.Client
	switch bounded := doer.(type) {
	case boundedResponseDoer:
		doer = bounded.next
	case *boundedResponseDoer:
		doer = bounded.next
	}
	if doer == nil {
		return nil, ErrClientTransport
	}
	response, err := doer.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, ErrClientTransport
	}
	if response == nil || response.Body == nil {
		return nil, ErrClientTransport
	}
	return response, nil
}

func canonicalEventStreamResponse(response *http.Response) bool {
	mediaType, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		return false
	}
	for name, value := range parameters {
		if !strings.EqualFold(name, "charset") || !strings.EqualFold(value, "utf-8") {
			return false
		}
	}
	for _, directive := range strings.Split(response.Header.Get("Cache-Control"), ",") {
		if strings.EqualFold(strings.TrimSpace(directive), "no-store") {
			return true
		}
	}
	return false
}

type eventProblemResponse struct {
	body     []byte
	response *http.Response
}

func (response eventProblemResponse) GetBody() []byte { return response.body }
func (response eventProblemResponse) StatusCode() int { return response.response.StatusCode }
func (response eventProblemResponse) ContentType() string {
	return response.response.Header.Get("Content-Type")
}

func decodeEventProblem(response *http.Response) error {
	defer response.Body.Close()
	document, err := io.ReadAll(io.LimitReader(response.Body, maxPublicEnvelopeBytes+1))
	if err != nil || len(document) > maxPublicEnvelopeBytes {
		return ErrMalformedEnvelope
	}
	envelope, err := DecodeEnvelope(eventProblemResponse{body: document, response: response})
	if err != nil {
		return err
	}
	if problem := EnvelopeError(envelope); problem != nil {
		return problem
	}
	return ErrMalformedEnvelope
}

func waitForEventReconnect(ctx context.Context) error {
	timer := time.NewTimer(eventReconnectDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type eventWatchState struct {
	runID        RunID
	cursor       EventCursor
	snapshot     RunEventSnapshotData
	haveSnapshot bool
	acceptedRun  RunView
	haveIdentity bool
	watermark    uint64
	lastSequence uint64
	haveSequence bool
	terminal     bool
}

func (state *eventWatchState) result() WatchEventsResult {
	return WatchEventsResult{Cursor: state.cursor, Snapshot: state.snapshot}
}

type pendingEventFrame struct {
	event    string
	id       string
	data     []byte
	hasEvent bool
	hasID    bool
	hasData  bool
	bytes    int
}

func consumeEventResponse(
	ctx context.Context,
	body io.Reader,
	state *eventWatchState,
	handle func(RunEventFrame) error,
) error {
	reader := bufio.NewReaderSize(body, 4096)
	var pending pendingEventFrame
	for {
		line, err := readBoundedEventLine(reader)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			if errors.Is(err, ErrMalformedEventStream) {
				return ErrMalformedEventStream
			}
			return errEventStreamClosed
		}
		if len(line) == 0 {
			if !pending.hasEvent && !pending.hasID && !pending.hasData {
				continue
			}
			if err := consumePendingEventFrame(state, pending, handle); err != nil {
				return err
			}
			pending = pendingEventFrame{}
			continue
		}
		if line[0] == ':' {
			if pending.hasEvent || pending.hasID || pending.hasData {
				return ErrMalformedEventStream
			}
			continue
		}
		pending.bytes += len(line) + 1
		if pending.bytes > maxPublicEnvelopeBytes {
			return ErrMalformedEventStream
		}
		field, value, found := bytes.Cut(line, []byte{':'})
		if !found {
			return ErrMalformedEventStream
		}
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		switch string(field) {
		case "event":
			if pending.hasEvent || len(value) == 0 {
				return ErrMalformedEventStream
			}
			pending.event = string(value)
			pending.hasEvent = true
		case "id":
			if pending.hasID || !validEventCursor(EventCursor(value), false) {
				return ErrMalformedEventStream
			}
			pending.id = string(value)
			pending.hasID = true
		case "data":
			if pending.hasData || len(value) == 0 || len(value) > maxPublicEnvelopeBytes {
				return ErrMalformedEventStream
			}
			pending.data = append([]byte(nil), value...)
			pending.hasData = true
		default:
			return ErrMalformedEventStream
		}
	}
}

func readBoundedEventLine(reader *bufio.Reader) ([]byte, error) {
	line := make([]byte, 0, 256)
	for {
		fragment, prefix, err := reader.ReadLine()
		if len(line)+len(fragment) > maxPublicEnvelopeBytes {
			return nil, ErrMalformedEventStream
		}
		line = append(line, fragment...)
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) != 0 {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if !prefix {
			return line, nil
		}
	}
}

func consumePendingEventFrame(
	state *eventWatchState,
	pending pendingEventFrame,
	handle func(RunEventFrame) error,
) error {
	if !pending.hasEvent || !pending.hasData {
		return ErrMalformedEventStream
	}
	switch pending.event {
	case "snapshot":
		if pending.hasID {
			return ErrMalformedEventStream
		}
		var snapshot RunEventSnapshotData
		if err := decodeStrictEventData(pending.data, &snapshot); err != nil {
			return err
		}
		watermark, err := validateEventSnapshot(state, snapshot)
		if err != nil {
			return err
		}
		frame := RunEventFrame{Kind: "snapshot", Snapshot: &snapshot}
		state.snapshot = snapshot
		state.haveSnapshot = true
		state.watermark = watermark
		state.terminal = terminalEventRunState(snapshot.Run.State)
		if err := handle(frame); err != nil {
			return err
		}
		if snapshot.RetentionGap {
			state.cursor = snapshot.Cursor
			state.lastSequence = watermark
			state.haveSequence = true
		} else if state.haveSequence && watermark == state.lastSequence {
			state.cursor = snapshot.Cursor
		} else if !state.haveSequence && (watermark == 0 || state.cursor == snapshot.Cursor) {
			state.lastSequence = watermark
			state.haveSequence = true
			state.cursor = snapshot.Cursor
		}
		return nil
	case "run_event":
		if !pending.hasID || !state.haveSnapshot {
			return ErrMalformedEventStream
		}
		var event RunEventData
		if err := decodeStrictEventData(pending.data, &event); err != nil {
			return err
		}
		sequence, duplicate, err := validateRunEvent(state, pending.id, event)
		if err != nil {
			return err
		}
		if duplicate {
			return nil
		}
		frame := RunEventFrame{Kind: "run_event", Cursor: pending.id, RunEvent: &event}
		if err := handle(frame); err != nil {
			return err
		}
		state.cursor = pending.id
		state.lastSequence = sequence
		state.haveSequence = true
		return nil
	default:
		return ErrMalformedEventStream
	}
}

func decodeStrictEventData(document []byte, target any) error {
	if len(document) == 0 || len(document) > maxPublicEnvelopeBytes || rejectDuplicateJSONFields(document) != nil ||
		!validEventDocumentShape(document, target) {
		return ErrMalformedEventStream
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || requireJSONDecoderEOF(decoder) != nil {
		return ErrMalformedEventStream
	}
	return nil
}

func validEventDocumentShape(document []byte, target any) bool {
	switch target.(type) {
	case *RunEventSnapshotData:
		return validEventSnapshotDocument(document)
	case *RunEventData:
		return validRunEventDocument(document)
	default:
		return false
	}
}

func validEventSnapshotDocument(document []byte) bool {
	root, ok := exactEventObject(document, "cursor", "retention_gap", "run", "watermark_sequence")
	if !ok {
		return false
	}
	run, ok := exactEventObject(
		root["run"],
		"blocked_reason",
		"cancel_requested",
		"connection_refs",
		"effect_summary",
		"intent",
		"orchestration_condition",
		"origin",
		"projection_observed_at",
		"result_references",
		"revision",
		"run_ref",
		"state",
		"unknown_effect_count",
	)
	if !ok {
		return false
	}
	if _, ok := exactEventObject(
		run["effect_summary"],
		"rejected",
		"succeeded",
		"unchanged",
		"unknown",
		"unsent",
	); !ok {
		return false
	}
	if _, ok := exactEventObject(run["intent"], "id", "kind", "revision"); !ok {
		return false
	}
	if _, ok := exactEventArray(run["connection_refs"]); !ok {
		return false
	}
	resultReferences, ok := exactEventArray(run["result_references"])
	if !ok {
		return false
	}
	for _, reference := range resultReferences {
		if _, ok := exactEventObject(reference, "result_ref"); !ok {
			return false
		}
	}
	return true
}

func validRunEventDocument(document []byte) bool {
	root, ok := exactEventObject(
		document,
		"committed_at",
		"run_ref",
		"safe_payload",
		"sequence",
		"state_revision",
		"type",
	)
	if !ok {
		return false
	}
	payload, ok := exactEventObject(root["safe_payload"], "counts", "reason_codes", "references")
	if !ok {
		return false
	}
	counts, ok := exactEventArray(payload["counts"])
	if !ok {
		return false
	}
	for _, count := range counts {
		if _, ok := exactEventObject(count, "name", "value"); !ok {
			return false
		}
	}
	if _, ok := exactEventArray(payload["reason_codes"]); !ok {
		return false
	}
	references, ok := exactEventArray(payload["references"])
	if !ok {
		return false
	}
	for _, reference := range references {
		if _, ok := exactEventObject(reference, "name", "ref"); !ok {
			return false
		}
	}
	return true
}

func exactEventObject(document []byte, fields ...string) (map[string]json.RawMessage, bool) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(document, &object); err != nil || object == nil || len(object) != len(fields) {
		return nil, false
	}
	for _, field := range fields {
		if _, present := object[field]; !present {
			return nil, false
		}
	}
	return object, true
}

func exactEventArray(document []byte) ([]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(document)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, false
	}
	var values []json.RawMessage
	if err := json.Unmarshal(trimmed, &values); err != nil || values == nil {
		return nil, false
	}
	return values, true
}

func validateEventSnapshot(state *eventWatchState, snapshot RunEventSnapshotData) (uint64, error) {
	watermark, err := parseCanonicalEventCounter(snapshot.WatermarkSequence, true)
	if err != nil || !validEventCursor(snapshot.Cursor, false) ||
		snapshot.Run.RunRef != string(state.runID) || !validEventRunView(snapshot.Run) {
		return 0, ErrMalformedEventStream
	}
	if state.haveSequence && watermark < state.lastSequence ||
		state.haveSnapshot && watermark < state.watermark {
		return 0, ErrMalformedEventStream
	}
	if state.haveSnapshot {
		previousRevision, previousErr := parseCanonicalEventCounter(state.snapshot.Run.Revision, false)
		currentRevision, currentErr := parseCanonicalEventCounter(snapshot.Run.Revision, false)
		if previousErr != nil || currentErr != nil || currentRevision < previousRevision ||
			state.terminal && snapshot.Run.State != state.snapshot.Run.State {
			return 0, ErrMalformedEventStream
		}
	}
	if !state.haveIdentity {
		state.acceptedRun = snapshot.Run
		state.haveIdentity = true
	} else if !sameEventRunIdentity(state.acceptedRun, snapshot.Run) {
		return 0, ErrMalformedEventStream
	}
	return watermark, nil
}

func validateRunEvent(state *eventWatchState, cursor string, event RunEventData) (uint64, bool, error) {
	sequence, err := parseCanonicalEventCounter(event.Sequence, false)
	if err != nil {
		return 0, false, ErrMalformedEventStream
	}
	stateRevision, err := parseCanonicalEventCounter(event.StateRevision, false)
	if err != nil {
		return 0, false, ErrMalformedEventStream
	}
	snapshotRevision, err := parseCanonicalEventCounter(state.snapshot.Run.Revision, false)
	if err != nil || event.RunRef != string(state.runID) || event.CommittedAt.IsZero() ||
		!validIdentity(event.Type, 512) || !validEventSafePayload(event.SafePayload) ||
		sequence > state.watermark || stateRevision > snapshotRevision {
		return 0, false, ErrMalformedEventStream
	}
	if state.haveSequence {
		if sequence == state.lastSequence {
			if cursor == state.cursor {
				return sequence, true, nil
			}
			return 0, false, ErrMalformedEventStream
		}
		if sequence < state.lastSequence || sequence != state.lastSequence+1 {
			return 0, false, ErrMalformedEventStream
		}
	}
	return sequence, false, nil
}

func validEventSafePayload(payload EventSafePayload) bool {
	if payload.Counts == nil || payload.ReasonCodes == nil || payload.References == nil {
		return false
	}
	countNames := make(map[string]struct{}, len(payload.Counts))
	for _, count := range payload.Counts {
		if !validIdentity(count.Name, 512) || !validCanonicalDecimal(count.Value) {
			return false
		}
		if _, duplicate := countNames[count.Name]; duplicate {
			return false
		}
		countNames[count.Name] = struct{}{}
	}
	reasons := make(map[string]struct{}, len(payload.ReasonCodes))
	for _, reason := range payload.ReasonCodes {
		if !validIdentity(reason, 512) {
			return false
		}
		if _, duplicate := reasons[reason]; duplicate {
			return false
		}
		reasons[reason] = struct{}{}
	}
	referenceNames := make(map[string]struct{}, len(payload.References))
	for _, reference := range payload.References {
		if !validIdentity(reference.Name, 512) || !validIdentity(reference.Ref, 512) {
			return false
		}
		if _, duplicate := referenceNames[reference.Name]; duplicate {
			return false
		}
		referenceNames[reference.Name] = struct{}{}
	}
	return true
}

func validEventRunView(view RunView) bool {
	if !view.State.Valid() || !view.Origin.Valid() || !view.Intent.Kind.Valid() ||
		!view.OrchestrationCondition.Valid() || !validIdentity(view.RunRef, 512) ||
		!validIdentity(view.Intent.Id, 512) || !validIdentity(view.Intent.Revision, 512) ||
		!view.BlockedReason.IsSpecified() || view.ConnectionRefs == nil ||
		view.ResultReferences == nil || view.ProjectionObservedAt.IsZero() {
		return false
	}
	if !view.BlockedReason.IsNull() {
		reason, err := view.BlockedReason.Get()
		if err != nil || !validIdentity(reason, 512) {
			return false
		}
	}
	if _, err := parseCanonicalEventCounter(view.Revision, false); err != nil {
		return false
	}
	counters := []string{
		view.EffectSummary.Rejected,
		view.EffectSummary.Succeeded,
		view.EffectSummary.Unchanged,
		view.EffectSummary.Unknown,
		view.EffectSummary.Unsent,
		view.UnknownEffectCount,
	}
	for _, counter := range counters {
		if !validCanonicalDecimal(counter) {
			return false
		}
	}
	if view.UnknownEffectCount != view.EffectSummary.Unknown {
		return false
	}
	connectionReferences := make(map[string]struct{}, len(view.ConnectionRefs))
	for _, reference := range view.ConnectionRefs {
		if !validIdentity(reference, 512) {
			return false
		}
		if _, duplicate := connectionReferences[reference]; duplicate {
			return false
		}
		connectionReferences[reference] = struct{}{}
	}
	resultReferences := make(map[string]struct{}, len(view.ResultReferences))
	for _, reference := range view.ResultReferences {
		if !validIdentity(reference.ResultRef, 512) {
			return false
		}
		if _, duplicate := resultReferences[reference.ResultRef]; duplicate {
			return false
		}
		resultReferences[reference.ResultRef] = struct{}{}
	}
	return true
}

func sameEventRunIdentity(left, right RunView) bool {
	return left.RunRef == right.RunRef && left.Origin == right.Origin && left.Intent == right.Intent &&
		slices.Equal(left.ConnectionRefs, right.ConnectionRefs)
}

func terminalEventRunState(state RunState) bool {
	switch state {
	case RunStateSucceeded, RunStatePartial, RunStateFailed, RunStateCancelled:
		return true
	default:
		return false
	}
}

func parseCanonicalEventCounter(value string, allowZero bool) (uint64, error) {
	if !validCanonicalDecimal(value) || !allowZero && value == "0" {
		return 0, ErrMalformedEventStream
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, ErrMalformedEventStream
	}
	return parsed, nil
}

func validEventCursor(value EventCursor, allowEmpty bool) bool {
	if value == "" {
		return allowEmpty
	}
	if len(value) > maximumEventCursorBytes {
		return false
	}
	for index := range len(value) {
		if value[index] < '!' || value[index] > '~' {
			return false
		}
	}
	return true
}

func validCanonicalDecimal(value string) bool {
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
