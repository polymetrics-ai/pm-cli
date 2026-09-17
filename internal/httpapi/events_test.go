package httpapi_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

const (
	eventWriteTimeout = 100 * time.Millisecond
	eventReadTimeout  = 5 * time.Second
)

var eventCursorFixtureKey = [32]byte{
	0xb8, 0x33, 0x6e, 0x24, 0x77, 0xc2, 0x2f, 0xe1,
	0x42, 0x5a, 0x99, 0x08, 0x16, 0xaf, 0x63, 0x3d,
	0xe0, 0x34, 0x9b, 0x5c, 0x70, 0x8f, 0x11, 0xa6,
	0x4d, 0x88, 0xf3, 0x20, 0x5e, 0xcb, 0x79, 0x01,
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

type sseFrame struct {
	Event string
	ID    string
	Data  []byte
}

type eventStream struct {
	response *http.Response
	cancel   context.CancelFunc
	scanner  *bufio.Scanner
}

func TestScopedDurableEventRoutes(t *testing.T) {
	t.Run("R10_E01_C01_foreign_run_and_scope_are_concealed_before_events", func(t *testing.T) {
		fixture := newEventFixture(t, true)
		primaryRunID := admitEventRun(
			t,
			fixture,
			fixture.Client,
			fixture.Scope,
			fixture.Connection.ConnectionID,
			"event-scope-primary",
		)
		foreignClient := fixture.ClientFor(t, fixture.ForeignActor)
		foreignRunID := admitEventRun(
			t,
			fixture,
			foreignClient,
			fixture.ForeignScope,
			fixture.ForeignConnection.ConnectionID,
			"event-scope-foreign",
		)

		for _, test := range []struct {
			name   string
			client *http.Client
			path   string
		}{
			{
				name:   "foreign run hidden inside authorized workspace",
				client: fixture.Client,
				path:   eventPath(fixture, fixture.Scope, foreignRunID),
			},
			{
				name:   "unauthorized workspace hidden before run lookup",
				client: fixture.Client,
				path:   eventPath(fixture, fixture.ForeignScope, foreignRunID),
			},
			{
				name:   "foreign principal cannot inspect primary run",
				client: foreignClient,
				path:   eventPath(fixture, fixture.Scope, primaryRunID),
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				response := getEventRoute(t, test.client, test.path, "")
				requireEventProblem(t, response, http.StatusNotFound, "resource_not_found")
				for _, canary := range []string{primaryRunID, foreignRunID, "run_accepted", "safe_payload"} {
					if bytes.Contains(response.document, []byte(canary)) {
						t.Fatalf("concealed event response disclosed %q: %s", canary, response.document)
					}
				}
			})
		}
	})

	t.Run("R10_E01_C02_gap_snapshot_keeps_terminal_truth_with_late_event", func(t *testing.T) {
		fixture := newEventFixture(t, true)
		runID := admitEventRun(
			t,
			fixture,
			fixture.Client,
			fixture.Scope,
			fixture.Connection.ConnectionID,
			"event-terminal-gap",
		)
		cursor := readAcceptedEventCursor(t, fixture, runID)
		seedFixtureEvent(t, fixture, runID, 2, 1, "fixture_retained_history")
		seedFixtureEvent(t, fixture, runID, 3, 1, "fixture_retained_history")
		seedFixtureEvent(t, fixture, runID, 4, 1, "fixture_retained_history")
		deleteFixtureEventsAfter(t, fixture, runID, 1)
		requestEventRunCancel(t, fixture, runID, "1")
		requireFixtureEvent(t, fixture, runID, 5, "run_cancel_requested")
		seedTerminalEventHistory(t, fixture, runID, true)
		deleteFixtureEventsAfter(t, fixture, runID, 1)

		stream := openEventStream(t, fixture.Client, eventPath(fixture, fixture.Scope, runID), cursor)
		snapshot := requireEventSnapshot(t, stream.next(t))
		if snapshot.Run.RunRef != runID || contract.RunState(snapshot.Run.State) != contract.RunStateSucceeded ||
			string(snapshot.Run.Revision) != "4" || snapshot.WatermarkSequence != "8" || !snapshot.RetentionGap {
			stream.close()
			t.Fatalf("gap snapshot = %#v; want terminal revision 4 at durable watermark 8 with explicit gap", snapshot)
		}
		if snapshot.Cursor == cursor {
			stream.close()
			t.Fatal("gap snapshot cursor did not advance to its durable watermark")
		}
		stream.close()

		const exactBrowserCount uint64 = 9007199254740993
		seedFixtureEvent(
			t,
			fixture,
			runID,
			9,
			4,
			"fixture_cleanup_observed",
			contract.SafeCount{Name: "exact_fixture_count", Value: exactBrowserCount},
		)
		reconnected := openEventStream(t, fixture.Client, eventPath(fixture, fixture.Scope, runID), snapshot.Cursor)
		defer reconnected.close()
		reconnectSnapshot := requireEventSnapshot(t, reconnected.next(t))
		if reconnectSnapshot.WatermarkSequence != "9" || reconnectSnapshot.RetentionGap {
			t.Fatalf("post-retention snapshot = %#v; want monotonic watermark 9 without a new gap", reconnectSnapshot)
		}
		lateFrame := reconnected.next(t)
		lateEvent := requireRunEvent(t, lateFrame, runID, 9, 4, "fixture_cleanup_observed")
		if len(lateEvent.SafePayload.Counts) != 1 ||
			lateEvent.SafePayload.Counts[0].Name != "exact_fixture_count" ||
			lateEvent.SafePayload.Counts[0].Value != strconv.FormatUint(exactBrowserCount, 10) {
			t.Fatalf("browser-safe exact count = %#v; want exact decimal string %d", lateEvent.SafePayload.Counts, exactBrowserCount)
		}
		var lateFields map[string]json.RawMessage
		if err := json.Unmarshal(lateFrame.Data, &lateFields); err != nil {
			t.Fatalf("decode late event fields: %v", err)
		}
		if _, hasState := lateFields["state"]; hasState {
			t.Fatal("late event carried an alternate run state instead of the authoritative terminal snapshot")
		}
	})

	t.Run("R10_E01_C03_slow_reader_closes_with_bounds_and_reconnects", func(t *testing.T) {
		t.Cleanup(func() {
			t.Error("DIAGNOSTIC CAPTURE ONLY: exposing bounded transport logs from the Linux probe; not a correctness verdict")
		})
		fixture := newEventFixture(t, true)
		runID := admitEventRun(
			t,
			fixture,
			fixture.Client,
			fixture.Scope,
			fixture.Connection.ConnectionID,
			"event-slow-reader",
		)
		seedBulkEventHistory(t, fixture, runID, 1000)

		var receiveConnection atomic.Pointer[net.TCPConn]
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
			tlsConnection, ok := info.Conn.(*tls.Conn)
			if !ok {
				t.Errorf("slow-reader connection is %T, want TLS", info.Conn)
				return
			}
			tcpConnection, ok := tlsConnection.NetConn().(*net.TCPConn)
			if !ok {
				t.Errorf("slow-reader underlying connection is %T, want TCP", tlsConnection.NetConn())
				return
			}
			if err := tcpConnection.SetReadBuffer(1024); err != nil {
				t.Errorf("bound owned slow-reader receive buffer: %v", err)
				return
			}
			receiveConnection.Store(tcpConnection)
		}}
		slowClient := fixture.ClientFor(t, fixture.Actor)
		stream := openEventStream(t, slowClient, eventPath(fixture, fixture.Scope, runID), "", trace)
		slowConnection := receiveConnection.Load()
		if slowConnection == nil {
			stream.close()
			t.Fatal("slow-reader request did not configure its owned TCP receive buffer")
		}
		snapshot := requireEventSnapshot(t, stream.next(t))
		if snapshot.WatermarkSequence != "1001" || snapshot.RetentionGap {
			stream.close()
			t.Fatalf("slow-reader snapshot = %#v; want complete watermark without a gap", snapshot)
		}
		first := stream.next(t)
		requireRunEvent(t, first, runID, 1, 1, "run_accepted")
		if first.ID == "" {
			stream.close()
			t.Fatal("first committed event did not carry an opaque reconnect cursor")
		}

		// Stop consuming with a two-event application buffer, bounded fixture
		// TCP send/receive buffers, and a 100 ms write bound.
		time.Sleep(10 * eventWriteTimeout)
		// Backpressure has exercised the write deadline. Restore the receive
		// window before observing EOF: Linux can otherwise spend the entire
		// closure bound draining bytes already queued by the closed writer.
		// Without the write deadline the stream remains live after reopening
		// the window, so the same closure oracle must still fail.
		if err := slowConnection.SetReadBuffer(64 * 1024); err != nil {
			stream.close()
			t.Fatalf("restore owned receive window for closure observation: %v", err)
		}
		drained := make(chan error, 1)
		go func() {
			_, err := io.Copy(io.Discard, stream.response.Body)
			drained <- err
		}()
		select {
		case <-drained:
		case <-time.After(2 * time.Second):
			stream.close()
			t.Fatal("slow event writer remained live beyond its bounded write timeout")
		}
		stream.close()

		reconnected := openEventStream(t, fixture.Client, eventPath(fixture, fixture.Scope, runID), first.ID)
		defer reconnected.close()
		reconnectSnapshot := requireEventSnapshot(t, reconnected.next(t))
		if reconnectSnapshot.WatermarkSequence != "1001" || reconnectSnapshot.RetentionGap {
			t.Fatalf("reconnect snapshot = %#v; want the same durable watermark", reconnectSnapshot)
		}
		requireRunEvent(t, reconnected.next(t), runID, 2, 1, "fixture_bulk_committed")
	})

	t.Run("R10_E01_C04_disconnect_and_server_drain_release_stream_without_durable_cancellation", func(t *testing.T) {
		draining := make(chan struct{})
		fixture := newEventFixture(t, true, draining)
		runID := admitEventRun(
			t,
			fixture,
			fixture.Client,
			fixture.Scope,
			fixture.Connection.ConnectionID,
			"event-disconnect",
		)
		stream := openEventStream(t, fixture.Client, eventPath(fixture, fixture.Scope, runID), "")
		requireEventSnapshot(t, stream.next(t))
		accepted := stream.next(t)
		requireRunEvent(t, accepted, runID, 1, 1, "run_accepted")
		stream.close()

		seedFixtureEvent(t, fixture, runID, 2, 1, "fixture_after_disconnect")
		record := loadFixtureRun(t, fixture, runID)
		if record.CancelRequested || record.State != contract.RunStateAccepted || record.Revision != 1 {
			t.Fatalf("client disconnect changed durable run state: %#v", record)
		}

		reconnected := openEventStream(t, fixture.Client, eventPath(fixture, fixture.Scope, runID), accepted.ID)
		defer reconnected.close()
		snapshot := requireEventSnapshot(t, reconnected.next(t))
		if snapshot.WatermarkSequence != "2" || snapshot.RetentionGap {
			t.Fatalf("disconnect reconnect snapshot = %#v", snapshot)
		}
		requireRunEvent(t, reconnected.next(t), runID, 2, 1, "fixture_after_disconnect")
		close(draining)
		requireEventStreamClosed(t, reconnected)
		afterDrain := loadFixtureRun(t, fixture, runID)
		if afterDrain.CancelRequested || afterDrain.State != contract.RunStateAccepted || afterDrain.Revision != 1 {
			t.Fatalf("server drain changed durable run state: %#v", afterDrain)
		}
	})

	t.Run("R10_E01_C05_tampered_foreign_and_restart_cursors", func(t *testing.T) {
		fixture := newEventFixture(t, true)
		runID := admitEventRun(
			t,
			fixture,
			fixture.Client,
			fixture.Scope,
			fixture.Connection.ConnectionID,
			"event-cursor-primary",
		)
		otherRunID := admitEventRun(
			t,
			fixture,
			fixture.Client,
			fixture.Scope,
			fixture.Connection.ConnectionID,
			"event-cursor-other",
		)
		cursor := readAcceptedEventCursor(t, fixture, runID)

		tampered := getEventRoute(t, fixture.Client, eventPath(fixture, fixture.Scope, runID), tamperEventCursor(cursor))
		tamperedProblem := requireEventProblem(t, tampered, http.StatusBadRequest, "invalid_event_cursor")
		if tamperedProblem.SafeRetryAction != "correct_input" {
			t.Fatalf("tampered cursor retry action = %q; want correct_input", tamperedProblem.SafeRetryAction)
		}
		if bytes.Contains(tampered.document, []byte("snapshot")) {
			t.Fatal("invalid cursor silently reset to a snapshot")
		}

		foreign := getEventRoute(t, fixture.Client, eventPath(fixture, fixture.Scope, otherRunID), cursor)
		foreignProblem := requireEventProblem(t, foreign, http.StatusBadRequest, "invalid_event_cursor")
		if foreignProblem.SafeRetryAction != "correct_input" {
			t.Fatalf("run-bound cursor retry action = %q; want correct_input", foreignProblem.SafeRetryAction)
		}
		if bytes.Contains(foreign.document, []byte(runID)) {
			t.Fatal("run-bound cursor failure disclosed its source run")
		}

		fixture.Restart(t)
		seedFixtureEvent(t, fixture, runID, 2, 1, "fixture_after_restart")
		restarted := openEventStream(t, fixture.Client, eventPath(fixture, fixture.Scope, runID), cursor)
		defer restarted.close()
		snapshot := requireEventSnapshot(t, restarted.next(t))
		if snapshot.WatermarkSequence != "2" || snapshot.RetentionGap {
			t.Fatalf("restart snapshot = %#v; stable-authority cursor was not resumed", snapshot)
		}
		requireRunEvent(t, restarted.next(t), runID, 2, 1, "fixture_after_restart")
	})

	t.Run("R10_E01_C06_committed_replay_is_strictly_ordered_after_snapshot", func(t *testing.T) {
		fixture := newEventFixture(t, true)
		runID := admitEventRun(
			t,
			fixture,
			fixture.Client,
			fixture.Scope,
			fixture.Connection.ConnectionID,
			"event-ordered-replay",
		)
		cursor := readAcceptedEventCursor(t, fixture, runID)
		seedTerminalEventHistory(t, fixture, runID, false)

		stream := openEventStream(t, fixture.Client, eventPath(fixture, fixture.Scope, runID), cursor)
		defer stream.close()
		snapshot := requireEventSnapshot(t, stream.next(t))
		if snapshot.Run.RunRef != runID || contract.RunState(snapshot.Run.State) != contract.RunStateSucceeded ||
			string(snapshot.Run.Revision) != "3" || snapshot.WatermarkSequence != "3" || snapshot.RetentionGap {
			t.Fatalf("ordered replay snapshot = %#v", snapshot)
		}
		runningFrame := stream.next(t)
		running := requireRunEvent(t, runningFrame, runID, 2, 2, "fixture_run_started")
		terminalFrame := stream.next(t)
		terminal := requireRunEvent(t, terminalFrame, runID, 3, 3, "fixture_run_succeeded")
		if runningFrame.ID == "" || terminalFrame.ID == "" || runningFrame.ID == terminalFrame.ID {
			t.Fatal("durable ordered events did not carry distinct opaque cursors")
		}
		if !running.CommittedAt.Before(terminal.CommittedAt) {
			t.Fatalf("committed replay order = %s then %s; want strict sequence order", running.CommittedAt, terminal.CommittedAt)
		}
	})

	t.Run("R10_E01_C07_unconfigured_signer_is_explicitly_unavailable_before_event_reads", func(t *testing.T) {
		fixture := newEventFixture(t, false)
		runID := admitEventRun(
			t,
			fixture,
			fixture.Client,
			fixture.Scope,
			fixture.Connection.ConnectionID,
			"event-capability-unavailable",
		)
		seedMalformedFixtureEvent(t, fixture, runID)

		response := getEventRoute(t, fixture.Client, eventPath(fixture, fixture.Scope, runID), "")
		requireEventProblem(t, response, http.StatusServiceUnavailable, "capability_unavailable")
		for _, canary := range []string{"malformed-event-canary", "run_accepted", runID} {
			if bytes.Contains(response.document, []byte(canary)) {
				t.Fatalf("unavailable event route disclosed %q: %s", canary, response.document)
			}
		}
		if count := countFixtureEvents(t, fixture, runID); count != 2 {
			t.Fatalf("unavailable event route changed durable events: count=%d, want 2", count)
		}

		foreignClient := fixture.ClientFor(t, fixture.ForeignActor)
		foreign := getEventRoute(t, foreignClient, eventPath(fixture, fixture.Scope, runID), "")
		requireEventProblem(t, foreign, http.StatusNotFound, "resource_not_found")
	})

	t.Run("R10_E01_C08_run_and_event_read_are_independently_required", func(t *testing.T) {
		for _, test := range []struct {
			name               string
			removedPermission  string
			retainedPermission string
		}{
			{
				name:               "run_read_without_event_read",
				removedPermission:  "event.read",
				retainedPermission: "run.read",
			},
			{
				name:               "event_read_without_run_read",
				removedPermission:  "run.read",
				retainedPermission: "event.read",
			},
		} {
			t.Run(test.name, func(t *testing.T) {
				fixture := newEventFixture(t, true)
				runID := admitEventRun(
					t,
					fixture,
					fixture.Client,
					fixture.Scope,
					fixture.Connection.ConnectionID,
					"event-independent-permissions-"+test.name,
				)
				seedMalformedFixtureEvent(t, fixture, runID)
				removeEventFixturePermission(
					t,
					fixture,
					test.removedPermission,
					test.retainedPermission,
				)

				response := getEventRoute(
					t,
					fixture.Client,
					eventPath(fixture, fixture.Scope, runID),
					"",
				)
				problem := requireEventProblem(t, response, http.StatusNotFound, "resource_not_found")
				if problem.SafeRetryAction != "correct_scope" {
					t.Fatalf(
						"missing %s retry action = %q; want correct_scope",
						test.removedPermission,
						problem.SafeRetryAction,
					)
				}
				for _, canary := range []string{
					runID,
					"run_accepted",
					"malformed-event-canary",
					"fixture_malformed",
					"safe_payload",
				} {
					if bytes.Contains(response.document, []byte(canary)) {
						t.Fatalf(
							"missing %s disclosed event data %q: %s",
							test.removedPermission,
							canary,
							response.document,
						)
					}
				}
				if count := countFixtureEvents(t, fixture, runID); count != 2 {
					t.Fatalf(
						"missing %s changed durable events: count=%d, want 2",
						test.removedPermission,
						count,
					)
				}
			})
		}
	})
}

func newEventFixture(t *testing.T, signerAvailable bool, draining ...<-chan struct{}) *testfixture.Fixture {
	t.Helper()
	if len(draining) > 1 {
		t.Fatal("event fixture accepts at most one drain signal")
	}
	return testfixture.NewAdmitting(t, func(_ testfixture.ServiceDependencies, configuration *httpapi.ServiceConfig) error {
		configuration.EventBufferSize = 2
		configuration.EventWriteTimeout = eventWriteTimeout
		if len(draining) == 1 {
			configuration.Draining = draining[0]
		}
		if signerAvailable {
			configuration.ResolveEventCursorKey = func(ctx context.Context) ([]byte, error) {
				if ctx == nil || ctx.Err() != nil {
					return nil, errors.New("event cursor key request is not live")
				}
				return bytes.Clone(eventCursorFixtureKey[:]), nil
			}
		}
		return nil
	})
}

func removeEventFixturePermission(
	t *testing.T,
	fixture *testfixture.Fixture,
	removedPermission string,
	retainedPermission string,
) {
	t.Helper()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE pm.memberships
			SET permissions = array_remove(permissions, $4),
			    resource_grants = array_remove(resource_grants, 'permission:' || $4)
			WHERE org_id = $1 AND workspace_id = $2 AND identity_id = $3
			  AND $4 = ANY(permissions) AND $5 = ANY(permissions)
			  AND 'permission:' || $4 = ANY(resource_grants)
			  AND 'permission:' || $5 = ANY(resource_grants)
		`,
			fixture.Scope.OrgID,
			fixture.Scope.WorkspaceID,
			fixture.Actor.RequestedActor.PrincipalID,
			removedPermission,
			retainedPermission,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf(
				"remove %s while retaining %s affected %d memberships",
				removedPermission,
				retainedPermission,
				tag.RowsAffected(),
			)
		}
		var removedPresent, retainedPresent, removedGrantPresent, retainedGrantPresent bool
		if err := tx.QueryRow(ctx, `
			SELECT
				$4 = ANY(permissions),
				$5 = ANY(permissions),
				'permission:' || $4 = ANY(resource_grants),
				'permission:' || $5 = ANY(resource_grants)
			FROM pm.memberships
			WHERE org_id = $1 AND workspace_id = $2 AND identity_id = $3
		`,
			fixture.Scope.OrgID,
			fixture.Scope.WorkspaceID,
			fixture.Actor.RequestedActor.PrincipalID,
			removedPermission,
			retainedPermission,
		).Scan(
			&removedPresent,
			&retainedPresent,
			&removedGrantPresent,
			&retainedGrantPresent,
		); err != nil {
			return err
		}
		if removedPresent || removedGrantPresent || !retainedPresent || !retainedGrantPresent {
			return fmt.Errorf(
				"permission fixture after removing %s and retaining %s = permissions(%t,%t) grants(%t,%t)",
				removedPermission,
				retainedPermission,
				removedPresent,
				retainedPresent,
				removedGrantPresent,
				retainedGrantPresent,
			)
		}
		return nil
	})
}

func admitEventRun(
	t *testing.T,
	fixture *testfixture.Fixture,
	client *http.Client,
	scope contract.Scope,
	connectionID string,
	requestKey string,
) string {
	t.Helper()
	invocation := testfixture.Invocation(t, fixture, requestKey)
	invocation.ConnectionRef = connectionID
	document, err := json.Marshal(invocation)
	if err != nil {
		t.Fatalf("encode event fixture admission: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), eventReadTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fixture.BaseURL+"/v1/workspaces/"+url.PathEscape(scope.WorkspaceID)+"/invocations",
		bytes.NewReader(document),
	)
	if err != nil {
		t.Fatalf("construct event fixture admission: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", requestKey)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("send event fixture admission: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(body) > 64*1024 {
		t.Fatal("event fixture admission response exceeded its bounded envelope")
	}
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("event fixture admission = HTTP %d: %s", response.StatusCode, body)
	}
	var admitted api.AdmissionResponse
	if err := json.Unmarshal(body, &admitted); err != nil || admitted.RunRef == "" || admitted.Replay {
		t.Fatalf("decode event fixture admission: response=%#v error=%v", admitted, err)
	}
	return admitted.RunRef
}

func eventPath(fixture *testfixture.Fixture, scope contract.Scope, runID string) string {
	return fixture.BaseURL + "/v1/workspaces/" + url.PathEscape(scope.WorkspaceID) +
		"/runs/" + url.PathEscape(runID) + "/events"
}

func openEventStream(t *testing.T, client *http.Client, endpoint, cursor string, traces ...*httptrace.ClientTrace) *eventStream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), eventReadTimeout)
	for _, trace := range traces {
		ctx = httptrace.WithClientTrace(ctx, trace)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		cancel()
		t.Fatalf("construct event stream request: %v", err)
	}
	request.Header.Set("Accept", "text/event-stream")
	if cursor != "" {
		request.Header.Set("Last-Event-ID", cursor)
	}
	response, err := client.Do(request)
	if err != nil {
		cancel()
		t.Fatalf("open event stream: %v", err)
	}
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		document, readErr := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
		_ = response.Body.Close()
		cancel()
		if readErr != nil || len(document) > 64*1024 {
			t.Fatal("failed event stream response exceeded its bounded envelope")
		}
		t.Fatalf("open event stream = HTTP %d content-type %q: %s", response.StatusCode, response.Header.Get("Content-Type"), document)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		_ = response.Body.Close()
		cancel()
		t.Fatalf("event stream cache control = %q, want no-store", response.Header.Get("Cache-Control"))
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	return &eventStream{response: response, cancel: cancel, scanner: scanner}
}

func (stream *eventStream) close() {
	if stream == nil {
		return
	}
	if stream.cancel != nil {
		stream.cancel()
	}
	if stream.response != nil && stream.response.Body != nil {
		_ = stream.response.Body.Close()
	}
}

func (stream *eventStream) next(t *testing.T) sseFrame {
	t.Helper()
	var frame sseFrame
	for stream.scanner.Scan() {
		line := stream.scanner.Text()
		if line == "" {
			if frame.Event != "" || frame.ID != "" || len(frame.Data) != 0 {
				return frame
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch name {
		case "event":
			frame.Event = value
		case "id":
			frame.ID = value
		case "data":
			if len(frame.Data) != 0 {
				frame.Data = append(frame.Data, '\n')
			}
			frame.Data = append(frame.Data, value...)
		}
	}
	if err := stream.scanner.Err(); err != nil {
		t.Fatalf("read event stream frame: %v", err)
	}
	t.Fatal("event stream closed before the required frame")
	return sseFrame{}
}
func requireEventStreamClosed(t *testing.T, stream *eventStream) {
	t.Helper()
	closed := make(chan error, 1)
	go func() {
		for stream.scanner.Scan() {
		}
		closed <- stream.scanner.Err()
	}()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("drained event stream ended with an error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event stream remained live after server drain")
	}
}

func requireEventSnapshot(t *testing.T, frame sseFrame) eventSnapshotWire {
	t.Helper()
	if frame.Event != "snapshot" || frame.ID != "" {
		t.Fatalf("event snapshot frame = %#v; want snapshot without a cursor ID", frame)
	}
	var snapshot eventSnapshotWire
	if err := json.Unmarshal(frame.Data, &snapshot); err != nil {
		t.Fatalf("decode event snapshot: %v: %s", err, frame.Data)
	}
	if snapshot.Run.RunRef == "" || snapshot.WatermarkSequence == "" || snapshot.Cursor == "" {
		t.Fatalf("event snapshot omitted run, watermark, or signed recovery cursor: %#v", snapshot)
	}
	return snapshot
}

func requireRunEvent(
	t *testing.T,
	frame sseFrame,
	runID string,
	sequence uint64,
	stateRevision uint64,
	eventType string,
) runEventWire {
	t.Helper()
	if frame.Event != "run_event" || frame.ID == "" {
		t.Fatalf("run event frame = %#v; want run_event with opaque ID", frame)
	}
	var event runEventWire
	if err := json.Unmarshal(frame.Data, &event); err != nil {
		t.Fatalf("decode run event: %v: %s", err, frame.Data)
	}
	if event.RunRef != runID || event.Sequence != strconv.FormatUint(sequence, 10) ||
		event.StateRevision != strconv.FormatUint(stateRevision, 10) || event.Type != eventType ||
		event.CommittedAt.IsZero() {
		t.Fatalf(
			"run event = %#v; want run=%q sequence=%d state_revision=%d type=%q",
			event,
			runID,
			sequence,
			stateRevision,
			eventType,
		)
	}
	if err := validateEventSafePayload(event.SafePayload); err != nil {
		t.Fatalf("run event safe payload is invalid: %v", err)
	}
	return event
}
func validateEventSafePayload(payload eventSafePayloadWire) error {
	if payload.References == nil || payload.Counts == nil || payload.ReasonCodes == nil {
		return errors.New("all public safe-payload collections must be explicit")
	}
	counts := make([]contract.SafeCount, len(payload.Counts))
	for index, count := range payload.Counts {
		value, err := strconv.ParseUint(count.Value, 10, 64)
		if err != nil || strconv.FormatUint(value, 10) != count.Value {
			return fmt.Errorf("count %q is not a canonical exact counter", count.Name)
		}
		counts[index] = contract.SafeCount{Name: count.Name, Value: value}
	}
	return (contract.SafePayload{
		References:  payload.References,
		Counts:      counts,
		ReasonCodes: payload.ReasonCodes,
	}).Validate()
}

func readAcceptedEventCursor(t *testing.T, fixture *testfixture.Fixture, runID string) string {
	t.Helper()
	stream := openEventStream(t, fixture.Client, eventPath(fixture, fixture.Scope, runID), "")
	defer stream.close()
	snapshot := requireEventSnapshot(t, stream.next(t))
	if snapshot.WatermarkSequence != "1" || snapshot.RetentionGap {
		t.Fatalf("initial event snapshot = %#v; want watermark 1 without a gap", snapshot)
	}
	accepted := stream.next(t)
	requireRunEvent(t, accepted, runID, 1, 1, "run_accepted")
	return accepted.ID
}

type boundedEventResponse struct {
	status   int
	header   http.Header
	document []byte
}

func getEventRoute(t *testing.T, client *http.Client, endpoint, cursor string) boundedEventResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), eventReadTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatalf("construct event route request: %v", err)
	}
	request.Header.Set("Accept", "text/event-stream")
	if cursor != "" {
		request.Header.Set("Last-Event-ID", cursor)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("send event route request: %v", err)
	}
	defer response.Body.Close()
	document, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(document) > 64*1024 {
		t.Fatal("event route problem exceeded its bounded envelope")
	}
	return boundedEventResponse{status: response.StatusCode, header: response.Header.Clone(), document: document}
}

func requireEventProblem(t *testing.T, response boundedEventResponse, status int, code string) api.Problem {
	t.Helper()
	if !strings.HasPrefix(response.header.Get("Content-Type"), "application/problem+json") {
		t.Fatalf("event problem content type = %q, want application/problem+json: %s", response.header.Get("Content-Type"), response.document)
	}
	var problem api.Problem
	if err := json.Unmarshal(response.document, &problem); err != nil {
		t.Fatalf("decode event problem: %v: %s", err, response.document)
	}
	if response.status != status || problem.Status != status || problem.Code != code {
		t.Fatalf("event problem = HTTP %d %#v; want %d/%s", response.status, problem, status, code)
	}
	return problem
}

func tamperEventCursor(cursor string) string {
	if cursor == "" {
		return "tampered"
	}
	replacement := byte('a')
	if cursor[0] == replacement {
		replacement = 'b'
	}
	return string(replacement) + cursor[1:]
}

func loadFixtureRun(t *testing.T, fixture *testfixture.Fixture, runID string) contract.RunRecord {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), eventReadTimeout)
	defer cancel()
	var record contract.RunRecord
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		record, err = store.LoadRun(ctx, tx, runID)
		return err
	}); err != nil {
		t.Fatalf("load event fixture run: %v", err)
	}
	return record
}

func seedTerminalEventHistory(t *testing.T, fixture *testfixture.Fixture, runID string, includeLate bool) {
	t.Helper()
	accepted := loadFixtureRun(t, fixture, runID)
	firstSequence := loadFixtureEventWatermark(t, fixture, runID) + 1
	observedAt := time.Now().UTC()
	running, err := accepted.Transition(contract.RunStateRunning, "fixture_run_started", observedAt)
	if err != nil {
		t.Fatalf("construct running event fixture state: %v", err)
	}
	terminal, err := running.Transition(contract.RunStateSucceeded, "fixture_run_succeeded", observedAt.Add(time.Microsecond))
	if err != nil {
		t.Fatalf("construct terminal event fixture state: %v", err)
	}
	recordDocument, err := json.Marshal(terminal)
	if err != nil {
		t.Fatalf("encode terminal event fixture state: %v", err)
	}
	payloadDocument := fixtureSafePayloadDocument(t, "event_fixture")
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		lastSequence := firstSequence + 1
		if includeLate {
			lastSequence++
		}
		tag, err := tx.Exec(ctx, `
			UPDATE pm.runs
			SET state = $4, revision = $5, record = $6::jsonb,
			    last_event_sequence = $7, updated_at = clock_timestamp()
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
			  AND last_event_sequence = $8
		`,
			fixture.Scope.OrgID,
			fixture.Scope.WorkspaceID,
			runID,
			string(terminal.State),
			terminal.Revision,
			string(recordDocument),
			lastSequence,
			firstSequence-1,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("terminal event fixture update affected %d rows", tag.RowsAffected())
		}
		if err := insertFixtureEvent(ctx, tx, fixture.Scope, runID, firstSequence, running.Revision, "fixture_run_started", payloadDocument); err != nil {
			return err
		}
		if err := insertFixtureEvent(ctx, tx, fixture.Scope, runID, firstSequence+1, terminal.Revision, "fixture_run_succeeded", payloadDocument); err != nil {
			return err
		}
		if includeLate {
			return insertFixtureEvent(ctx, tx, fixture.Scope, runID, firstSequence+2, terminal.Revision, "fixture_cleanup_observed", payloadDocument)
		}
		return nil
	})
}

func seedFixtureEvent(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID string,
	sequence uint64,
	stateRevision uint64,
	eventType string,
	counts ...contract.SafeCount,
) {
	t.Helper()
	payloadDocument := fixtureSafePayloadDocument(t, "event_fixture", counts...)
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := advanceFixtureEventWatermark(ctx, tx, fixture.Scope, runID, sequence); err != nil {
			return err
		}
		return insertFixtureEvent(ctx, tx, fixture.Scope, runID, sequence, stateRevision, eventType, payloadDocument)
	})
}

func insertFixtureEvent(
	ctx context.Context,
	tx pgx.Tx,
	scope contract.Scope,
	runID string,
	sequence uint64,
	stateRevision uint64,
	eventType string,
	payloadDocument []byte,
) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO pm.server_events (
			org_id, workspace_id, run_id, sequence, event_id,
			state_revision, event_type, safe_payload, committed_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8::jsonb,
			clock_timestamp() + ($4::bigint * interval '1 microsecond')
		)
	`,
		scope.OrgID,
		scope.WorkspaceID,
		runID,
		sequence,
		fmt.Sprintf("fixture-event-%d", sequence),
		stateRevision,
		eventType,
		string(payloadDocument),
	)
	return err
}

func fixtureSafePayloadDocument(t *testing.T, reason string, counts ...contract.SafeCount) []byte {
	t.Helper()
	if counts == nil {
		counts = []contract.SafeCount{}
	}
	payload := contract.SafePayload{
		References:  []contract.SafeReference{},
		Counts:      counts,
		ReasonCodes: []string{reason},
	}
	if err := payload.Validate(); err != nil {
		t.Fatalf("validate event fixture safe payload: %v", err)
	}
	document, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode event fixture safe payload: %v", err)
	}
	return document
}

func deleteFixtureEventsAfter(t *testing.T, fixture *testfixture.Fixture, runID string, after uint64) {
	t.Helper()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			DELETE FROM pm.server_events
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND sequence > $4
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID, after)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errors.New("event gap fixture retained every event")
		}
		return nil
	})
}

func seedBulkEventHistory(t *testing.T, fixture *testfixture.Fixture, runID string, count int) {
	t.Helper()
	if count <= 0 || count > 1000 {
		t.Fatal("bulk event fixture count must be between one and 1000")
	}
	references := make([]contract.SafeReference, 16)
	for index := range references {
		references[index] = contract.SafeReference{
			Name: fmt.Sprintf("fixture_ref_%02d", index),
			Ref:  fmt.Sprintf("%03d-%s", index, strings.Repeat("x", 500)),
		}
	}
	payload := contract.SafePayload{
		References:  references,
		Counts:      []contract.SafeCount{},
		ReasonCodes: []string{"fixture_bulk_committed"},
	}
	if err := payload.Validate(); err != nil {
		t.Fatalf("construct bounded bulk event payload: %v", err)
	}
	payloadDocument, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode bulk event payload: %v", err)
	}
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE pm.runs
			SET last_event_sequence = $4
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
			  AND last_event_sequence = 1
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID, uint64(count+1))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("bulk event watermark update affected %d rows", tag.RowsAffected())
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO pm.server_events (
				org_id, workspace_id, run_id, sequence, event_id,
				state_revision, event_type, safe_payload, committed_at
			)
			SELECT
				$1, $2, $3, generated.sequence,
				'fixture-bulk-event-' || generated.sequence::text,
				1, 'fixture_bulk_committed', $4::jsonb,
				clock_timestamp() + (generated.sequence * interval '1 microsecond')
			FROM generate_series(2, $5::bigint + 1) AS generated(sequence)
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID, string(payloadDocument), count)
		return err
	})
}

func seedMalformedFixtureEvent(t *testing.T, fixture *testfixture.Fixture, runID string) {
	t.Helper()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		if err := advanceFixtureEventWatermark(ctx, tx, fixture.Scope, runID, 2); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO pm.server_events (
				org_id, workspace_id, run_id, sequence, event_id,
				state_revision, event_type, safe_payload, committed_at
			) VALUES ($1, $2, $3, 2, 'malformed-event-canary', 1, 'fixture_malformed', '{}'::jsonb, clock_timestamp())
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID)
		return err
	})
}

func advanceFixtureEventWatermark(
	ctx context.Context,
	tx pgx.Tx,
	scope contract.Scope,
	runID string,
	sequence uint64,
) error {
	tag, err := tx.Exec(ctx, `
		UPDATE pm.runs
		SET last_event_sequence = $4
		WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
		  AND last_event_sequence = $4 - 1
	`, scope.OrgID, scope.WorkspaceID, runID, sequence)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("advance fixture event watermark to %d affected %d rows", sequence, tag.RowsAffected())
	}
	return nil
}

func requestEventRunCancel(t *testing.T, fixture *testfixture.Fixture, runID, expectedRevision string) {
	t.Helper()
	const requestKey = "event-retention-cancel"
	body := api.RunCancelRequest{Protocol: api.ProtocolMetadata{
		WireVersion:      api.ProtocolMetadataWireVersion(api.WireVersion),
		RequiredFeatures: []string{},
		RequestKey:       requestKey,
		IntentDigest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		ExpectedRevision: expectedRevision,
	}}
	document, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode event allocator cancellation: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), eventReadTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimSuffix(eventPath(fixture, fixture.Scope, runID), "/events")+"/cancel",
		bytes.NewReader(document),
	)
	if err != nil {
		t.Fatalf("construct event allocator cancellation: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", requestKey)
	request.Header.Set("If-Match", expectedRevision)
	response, err := fixture.Client.Do(request)
	if err != nil {
		t.Fatalf("send event allocator cancellation: %v", err)
	}
	defer response.Body.Close()
	responseDocument, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(responseDocument) > 64*1024 {
		t.Fatal("event allocator cancellation response exceeded its bounded envelope")
	}
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("event allocator cancellation = HTTP %d: %s", response.StatusCode, responseDocument)
	}
}

func loadFixtureEventWatermark(t *testing.T, fixture *testfixture.Fixture, runID string) uint64 {
	t.Helper()
	var watermark uint64
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT last_event_sequence
			FROM pm.runs
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID).Scan(&watermark)
	})
	return watermark
}

func requireFixtureEvent(t *testing.T, fixture *testfixture.Fixture, runID string, sequence uint64, eventType string) {
	t.Helper()
	var storedType string
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT event_type
			FROM pm.server_events
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND sequence = $4
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID, sequence).Scan(&storedType)
	})
	if storedType != eventType {
		t.Fatalf("fixture event %d type = %q, want %q", sequence, storedType, eventType)
	}
}

func countFixtureEvents(t *testing.T, fixture *testfixture.Fixture, runID string) int {
	t.Helper()
	var count int
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*)
			FROM pm.server_events
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID).Scan(&count)
	})
	return count
}
