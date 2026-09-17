package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const (
	eventCursorDomain    = "pm/http/event-cursor/v1\x00"
	eventPollInterval    = 100 * time.Millisecond
	maximumEventPageSize = uint64(1000)
)

var errInvalidEventCursor = errors.New("invalid event cursor")

type eventCursorEnvelope struct {
	Version      int    `json:"version"`
	DeploymentID string `json:"deployment_id"`
	OrgID        string `json:"org_id"`
	WorkspaceID  string `json:"workspace_id"`
	RunID        string `json:"run_id"`
	Sequence     string `json:"sequence"`
}

type eventSnapshotWire struct {
	Run               api.RunView `json:"run"`
	WatermarkSequence string      `json:"watermark_sequence"`
	RetentionGap      bool        `json:"retention_gap"`
	Cursor            string      `json:"cursor"`
}

type eventSafeCountWire struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type eventSafePayloadWire struct {
	References  []contract.SafeReference `json:"references"`
	Counts      []eventSafeCountWire     `json:"counts"`
	ReasonCodes []string                 `json:"reason_codes"`
}

type runEventWire struct {
	RunRef        string               `json:"run_ref"`
	Sequence      string               `json:"sequence"`
	StateRevision string               `json:"state_revision"`
	Type          string               `json:"type"`
	SafePayload   eventSafePayloadWire `json:"safe_payload"`
	CommittedAt   time.Time            `json:"committed_at"`
}

type eventStreamSnapshot struct {
	Run          app.RunView
	Watermark    uint64
	RetentionGap bool
}

type eventStreamMessageKind uint8

const (
	eventStreamSnapshotMessage eventStreamMessageKind = iota + 1
	eventStreamEventMessage
)

type eventStreamMessage struct {
	Kind     eventStreamMessageKind
	Snapshot eventStreamSnapshot
	Event    contract.ServerEvent
	Err      error
}

// StreamRunEvents serves the one non-bufferable ordinary route. Authentication
// middleware has already established the actor; this method checks current run
// and event authority before resolving signer material or reading an event row.
func (service *Service) StreamRunEvents(c *gin.Context) {
	if service == nil || c == nil || c.Request == nil {
		return
	}
	state, scope, err := scopedState(c.Request.Context(), c.Param("workspace_id"))
	if err != nil {
		writeProblem(c, problemForKnownError(err, nil))
		return
	}
	runID := c.Param("run_id")
	if err := service.runs.AuthorizeRunEvents(c.Request.Context(), state.identity.Actor, scope, runID); err != nil {
		writeProblem(c, problemForKnownError(err, nil))
		return
	}
	if service.resolveEventCursorKey == nil || service.eventBufferSize == 0 || service.eventWriteTimeout <= 0 {
		writeEventCapabilityUnavailable(c)
		return
	}
	key, err := service.resolveEventCursorKey(c.Request.Context())
	if err != nil || len(key) != sha256.Size {
		clear(key)
		writeEventCapabilityUnavailable(c)
		return
	}
	defer clear(key)

	cursor, present, headerErr := oneHeader(c.Request, "Last-Event-ID")
	if headerErr != nil || present && (cursor == "" || len(cursor) > maximumCursorBytes) {
		writeInvalidEventCursor(c)
		return
	}
	var after uint64
	if present {
		after, err = service.parseEventCursor(key, scope, runID, cursor)
		if err != nil {
			writeInvalidEventCursor(c)
			return
		}
	}
	pageSize := min(service.eventBufferSize, maximumEventPageSize)
	initial, err := service.runs.ReadRunEvents(
		c.Request.Context(),
		state.identity.Actor,
		scope,
		runID,
		after,
		0,
		uint32(pageSize),
	)
	if err != nil {
		writeProblem(c, problemForKnownError(err, nil))
		return
	}

	controller := http.NewResponseController(c.Writer)
	if err := controller.SetWriteDeadline(time.Now().Add(service.eventWriteTimeout)); err != nil {
		writeEventCapabilityUnavailable(c)
		return
	}
	if err := controller.SetWriteDeadline(time.Time{}); err != nil {
		writeEventCapabilityUnavailable(c)
		return
	}
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()

	producerContext, cancelProducer := context.WithCancel(c.Request.Context())
	messages := make(chan eventStreamMessage, int(pageSize))
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		service.produceRunEventStream(
			producerContext,
			state.identity.Actor,
			scope,
			runID,
			after,
			uint32(pageSize),
			initial,
			messages,
		)
	}()
	defer func() {
		cancelProducer()
		<-producerDone
	}()

	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Status(http.StatusOK)

	heartbeatInterval := service.eventWriteTimeout / 2
	if heartbeatInterval > 15*time.Second {
		heartbeatInterval = 15 * time.Second
	}
	if heartbeatInterval < time.Millisecond {
		heartbeatInterval = time.Millisecond
	}
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-service.draining:
			return
		case <-heartbeat.C:
			if err := writeEventStreamComment(c.Writer, controller, service.eventWriteTimeout); err != nil {
				return
			}
		case message, ok := <-messages:
			if !ok || message.Err != nil {
				return
			}
			switch message.Kind {
			case eventStreamSnapshotMessage:
				cursor, err := service.signEventCursor(key, scope, runID, message.Snapshot.Watermark)
				if err != nil {
					return
				}
				document := eventSnapshotWire{
					Run:               runResponse(message.Snapshot.Run),
					WatermarkSequence: strconv.FormatUint(message.Snapshot.Watermark, 10),
					RetentionGap:      message.Snapshot.RetentionGap,
					Cursor:            cursor,
				}
				if err := writeEventStreamFrame(c.Writer, controller, service.eventWriteTimeout, "snapshot", "", document); err != nil {
					return
				}
			case eventStreamEventMessage:
				cursor, err := service.signEventCursor(key, scope, runID, message.Event.Sequence)
				if err != nil {
					return
				}
				document := runEventWire{
					RunRef:        message.Event.RunID,
					Sequence:      strconv.FormatUint(message.Event.Sequence, 10),
					StateRevision: strconv.FormatUint(message.Event.StateRevision, 10),
					Type:          message.Event.Type,
					SafePayload:   eventSafePayloadForWire(message.Event.SafePayload),
					CommittedAt:   message.Event.CommittedAt,
				}
				if err := writeEventStreamFrame(c.Writer, controller, service.eventWriteTimeout, "run_event", cursor, document); err != nil {
					return
				}
			default:
				return
			}
		}
	}
}

func (service *Service) produceRunEventStream(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	runID string,
	after uint64,
	pageSize uint32,
	initial app.RunEventPage,
	messages chan<- eventStreamMessage,
) {
	defer close(messages)
	poll := time.NewTicker(eventPollInterval)
	defer poll.Stop()
	page := initial
	first := true
	for {
		cycleState := page.Run.Record.State
		readThrough := page.ReadThrough
		if first || page.Watermark != after || page.RetentionGap {
			snapshot := eventStreamSnapshot{
				Run:          page.Run,
				Watermark:    page.Watermark,
				RetentionGap: page.RetentionGap,
			}
			if !sendEventStreamMessage(ctx, messages, eventStreamMessage{
				Kind: eventStreamSnapshotMessage, Snapshot: snapshot,
			}) {
				return
			}
		}
		first = false
		for {
			if page.RetentionGap {
				after = readThrough
				break
			}
			for index := range page.Events {
				event := page.Events[index]
				if !sendEventStreamMessage(ctx, messages, eventStreamMessage{
					Kind: eventStreamEventMessage, Event: event,
				}) {
					return
				}
				after = event.Sequence
			}
			if after >= readThrough {
				break
			}
			var err error
			page, err = service.runs.ReadRunEvents(ctx, actor, scope, runID, after, readThrough, pageSize)
			if err != nil {
				sendEventStreamMessage(ctx, messages, eventStreamMessage{Err: err})
				return
			}
			if page.RetentionGap {
				snapshot := eventStreamSnapshot{
					Run:          page.Run,
					Watermark:    page.Watermark,
					RetentionGap: true,
				}
				if !sendEventStreamMessage(ctx, messages, eventStreamMessage{
					Kind: eventStreamSnapshotMessage, Snapshot: snapshot,
				}) {
					return
				}
			}
		}
		if terminalRunEventState(cycleState) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-poll.C:
		}
		var err error
		page, err = service.runs.ReadRunEvents(ctx, actor, scope, runID, after, 0, pageSize)
		if err != nil {
			sendEventStreamMessage(ctx, messages, eventStreamMessage{Err: err})
			return
		}
	}
}

func sendEventStreamMessage(ctx context.Context, messages chan<- eventStreamMessage, message eventStreamMessage) bool {
	select {
	case <-ctx.Done():
		return false
	case messages <- message:
		return true
	}
}

func terminalRunEventState(state contract.RunState) bool {
	switch state {
	case contract.RunStateSucceeded, contract.RunStatePartial, contract.RunStateFailed, contract.RunStateCancelled:
		return true
	default:
		return false
	}
}

func eventSafePayloadForWire(payload contract.SafePayload) eventSafePayloadWire {
	counts := make([]eventSafeCountWire, len(payload.Counts))
	for index, count := range payload.Counts {
		counts[index] = eventSafeCountWire{
			Name:  count.Name,
			Value: strconv.FormatUint(count.Value, 10),
		}
	}
	return eventSafePayloadWire{
		References:  payload.References,
		Counts:      counts,
		ReasonCodes: payload.ReasonCodes,
	}
}

func (service *Service) signEventCursor(
	key []byte,
	scope contract.Scope,
	runID string,
	sequence uint64,
) (string, error) {
	if service == nil || len(key) != sha256.Size || sequence > math.MaxInt64 {
		return "", errInvalidEventCursor
	}
	envelope := eventCursorEnvelope{
		Version:      1,
		DeploymentID: service.instanceID,
		OrgID:        scope.OrgID,
		WorkspaceID:  scope.WorkspaceID,
		RunID:        runID,
		Sequence:     strconv.FormatUint(sequence, 10),
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = io.WriteString(mac, eventCursorDomain)
	_, _ = mac.Write(payload)
	return "v1." + base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (service *Service) parseEventCursor(
	key []byte,
	scope contract.Scope,
	runID string,
	cursor string,
) (uint64, error) {
	if service == nil || len(key) != sha256.Size || cursor == "" || len(cursor) > maximumCursorBytes {
		return 0, errInvalidEventCursor
	}
	parts := strings.Split(cursor, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return 0, errInvalidEventCursor
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || base64.RawURLEncoding.EncodeToString(payload) != parts[1] {
		return 0, errInvalidEventCursor
	}
	providedMAC, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(providedMAC) != sha256.Size ||
		base64.RawURLEncoding.EncodeToString(providedMAC) != parts[2] {
		return 0, errInvalidEventCursor
	}
	mac := hmac.New(sha256.New, key)
	_, _ = io.WriteString(mac, eventCursorDomain)
	_, _ = mac.Write(payload)
	if !hmac.Equal(providedMAC, mac.Sum(nil)) {
		return 0, errInvalidEventCursor
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var envelope eventCursorEnvelope
	if err := decoder.Decode(&envelope); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return 0, errInvalidEventCursor
	}
	if envelope.Version != 1 || envelope.DeploymentID != service.instanceID ||
		envelope.OrgID != scope.OrgID || envelope.WorkspaceID != scope.WorkspaceID ||
		envelope.RunID != runID || envelope.Sequence == "" {
		return 0, errInvalidEventCursor
	}
	sequence, err := strconv.ParseUint(envelope.Sequence, 10, 64)
	if err != nil || sequence > math.MaxInt64 || strconv.FormatUint(sequence, 10) != envelope.Sequence {
		return 0, errInvalidEventCursor
	}
	return sequence, nil
}

func writeEventStreamFrame(
	writer http.ResponseWriter,
	controller *http.ResponseController,
	timeout time.Duration,
	event string,
	id string,
	value any,
) error {
	document, err := json.Marshal(value)
	if err != nil {
		return err
	}
	frame := make([]byte, 0, len(document)+len(event)+len(id)+32)
	frame = append(frame, "event: "...)
	frame = append(frame, event...)
	frame = append(frame, '\n')
	if id != "" {
		frame = append(frame, "id: "...)
		frame = append(frame, id...)
		frame = append(frame, '\n')
	}
	frame = append(frame, "data: "...)
	frame = append(frame, document...)
	frame = append(frame, '\n', '\n')
	return writeEventStreamBytes(writer, controller, timeout, frame)
}

func writeEventStreamComment(
	writer http.ResponseWriter,
	controller *http.ResponseController,
	timeout time.Duration,
) error {
	return writeEventStreamBytes(writer, controller, timeout, []byte(": keepalive\n\n"))
}

func writeEventStreamBytes(
	writer http.ResponseWriter,
	controller *http.ResponseController,
	timeout time.Duration,
	data []byte,
) error {
	if err := controller.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	written, err := writer.Write(data)
	if err != nil {
		return err
	}
	if written != len(data) {
		return io.ErrShortWrite
	}
	return controller.Flush()
}

func writeInvalidEventCursor(c *gin.Context) {
	writeProblem(c, newProblem(http.StatusBadRequest, "invalid_event_cursor", "correct_input", nil))
}

func writeEventCapabilityUnavailable(c *gin.Context) {
	writeProblem(c, newProblem(http.StatusServiceUnavailable, "capability_unavailable", "after_capability_change", nil))
}
