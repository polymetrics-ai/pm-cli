package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

func TestReplayGapSnapshotAndTerminalRender(t *testing.T) {
	t.Run("pm-cli-run-watch-E01-C01_first_writer_rollback", func(t *testing.T) {
		fixture := newDurableEventFixture(t)
		client := newDurableEventClient(t, fixture)
		runID := admitDurableEventRun(t, fixture, "issue20-first-writer-rollback")
		acceptedCursor := acceptedDurableEventCursor(t, client, fixture, runID)

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		rollback := errors.New("controlled first-writer rollback")
		firstPrepared := make(chan struct{})
		releaseFirst := make(chan struct{})
		firstResult := make(chan error, 1)
		go func() {
			firstResult <- store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
				record, err := store.LoadRun(ctx, tx, runID)
				if err != nil {
					return err
				}
				commit, err := durableRunTransition(record, contract.RunStateQueued, "issue20_first_writer", "c01-first")
				if err != nil {
					return err
				}
				if _, err := store.CommitRunTransition(ctx, tx, commit); err != nil {
					return err
				}
				close(firstPrepared)
				select {
				case <-releaseFirst:
					return rollback
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		}()
		select {
		case <-firstPrepared:
		case err := <-firstResult:
			t.Fatalf("first writer failed before its rollback barrier: %v", err)
		case <-ctx.Done():
			t.Fatal("first writer did not acquire the durable run transition lock")
		}

		type transitionResult struct {
			event contract.ServerEvent
			err   error
		}
		secondReady := make(chan struct{})
		secondResult := make(chan transitionResult, 1)
		go func() {
			var event contract.ServerEvent
			err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
				record, err := store.LoadRun(ctx, tx, runID)
				if err != nil {
					return err
				}
				commit, err := durableRunTransition(record, contract.RunStateQueued, "issue20_second_committed", "c01-second")
				if err != nil {
					return err
				}
				close(secondReady)
				event, err = store.CommitRunTransition(ctx, tx, commit)
				return err
			})
			secondResult <- transitionResult{event: event, err: err}
		}()
		select {
		case <-secondReady:
		case result := <-secondResult:
			t.Fatalf("second writer failed before contending for the run row: %v", result.err)
		case <-ctx.Done():
			t.Fatal("second writer did not reach the run-row lock")
		}
		select {
		case result := <-secondResult:
			t.Fatalf("second writer escaped the held run-row lock: event=%#v err=%v", result.event, result.err)
		case <-time.After(50 * time.Millisecond):
		}
		close(releaseFirst)
		if err := <-firstResult; !errors.Is(err, rollback) {
			t.Fatalf("first writer rollback = %v; want controlled rollback", err)
		}
		result := <-secondResult
		if result.err != nil || result.event.Sequence != 2 {
			t.Fatalf("second writer commit = event %#v err %v; want contiguous sequence 2", result.event, result.err)
		}
		events := loadDurableEvents(t, fixture, runID)
		if len(events) != 2 || events[0].Sequence != 1 || events[1].Sequence != 2 ||
			!slices.Equal(events[1].SafePayload.ReasonCodes, []string{"issue20_second_committed"}) {
			t.Fatalf("durable rollback history = %#v; want accepted then only the second committed writer", events)
		}

		fixture.Restart(t)
		client = newDurableEventClient(t, fixture)
		watchContext, stopWatch := context.WithTimeout(t.Context(), 5*time.Second)
		defer stopWatch()
		var replay []pmapi.RunEventData
		watchResult, watchErr := client.WatchEvents(
			watchContext,
			pmapi.WorkspaceID(fixture.Scope.WorkspaceID),
			pmapi.RunID(runID),
			pmapi.WatchEventsOptions{
				Cursor: acceptedCursor,
				Handle: func(frame pmapi.RunEventFrame) error {
					if frame.RunEvent != nil {
						replay = append(replay, *frame.RunEvent)
						stopWatch()
					}
					return nil
				},
			},
		)
		if !errors.Is(watchErr, context.Canceled) || len(replay) != 1 || replay[0].Sequence != "2" ||
			!slices.Equal(replay[0].SafePayload.ReasonCodes, []string{"issue20_second_committed"}) ||
			watchResult.Cursor == "" || watchResult.Cursor == acceptedCursor {
			t.Fatalf("replica rollback replay = result %#v events %#v err %v", watchResult, replay, watchErr)
		}
	})

	t.Run("pm-cli-run-watch-E01-C02_delayed_writer_commits_on_another_replica", func(t *testing.T) {
		fixture := newDurableEventFixture(t)
		client := newDurableEventClient(t, fixture)
		runID := admitDurableEventRun(t, fixture, "issue20-delayed-writer")
		acceptedCursor := acceptedDurableEventCursor(t, client, fixture, runID)

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		prepared := make(chan struct{})
		release := make(chan struct{})
		committed := make(chan error, 1)
		go func() {
			committed <- store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
				record, err := store.LoadRun(ctx, tx, runID)
				if err != nil {
					return err
				}
				commit, err := durableRunTransition(record, contract.RunStateQueued, "issue20_delayed_committed", "c02-queued")
				if err != nil {
					return err
				}
				if _, err := store.CommitRunTransition(ctx, tx, commit); err != nil {
					return err
				}
				close(prepared)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		}()
		select {
		case <-prepared:
		case err := <-committed:
			t.Fatalf("delayed writer failed before commit release: %v", err)
		case <-ctx.Done():
			t.Fatal("delayed writer did not reach its commit barrier")
		}
		close(release)
		if err := <-committed; err != nil {
			t.Fatalf("commit delayed writer: %v", err)
		}
		runningEvent := commitDurableTransition(
			t,
			fixture,
			runID,
			contract.RunStateRunning,
			"issue20_writer_running",
			"c02-running",
		)
		if runningEvent.Sequence != 3 {
			t.Fatalf("running event sequence = %d; want 3", runningEvent.Sequence)
		}
		terminalEvent := commitDurableTransition(
			t,
			fixture,
			runID,
			contract.RunStateSucceeded,
			"issue20_writer_succeeded",
			"c02-terminal",
		)
		if terminalEvent.Sequence != 4 {
			t.Fatalf("terminal event sequence = %d; want 4", terminalEvent.Sequence)
		}

		fixture.Restart(t)
		client = newDurableEventClient(t, fixture)
		replayContext, stopReplay := context.WithTimeout(t.Context(), 5*time.Second)
		defer stopReplay()
		var replay []pmapi.RunEventData
		watchResult, err := client.WatchEvents(
			replayContext,
			pmapi.WorkspaceID(fixture.Scope.WorkspaceID),
			pmapi.RunID(runID),
			pmapi.WatchEventsOptions{
				Cursor: acceptedCursor,
				Handle: func(frame pmapi.RunEventFrame) error {
					if frame.RunEvent != nil {
						replay = append(replay, *frame.RunEvent)
					}
					return nil
				},
			},
		)
		if err != nil || len(replay) != 3 ||
			replay[0].Sequence != "2" || replay[1].Sequence != "3" || replay[2].Sequence != "4" ||
			watchResult.Snapshot.Run.State != pmapi.RunStateSucceeded ||
			watchResult.Snapshot.WatermarkSequence != "4" || watchResult.Cursor == acceptedCursor {
			t.Fatalf("delayed cross-replica terminal replay = result %#v events %#v err %v", watchResult, replay, err)
		}
	})

	t.Run("pm-cli-run-watch-E01-C03_retention_gap_requires_scoped_snapshot_cursor", func(t *testing.T) {
		fixture := newDurableEventFixture(t)
		client := newDurableEventClient(t, fixture)
		runID := admitDurableEventRun(t, fixture, "issue20-retention-gap")
		acceptedCursor := acceptedDurableEventCursor(t, client, fixture, runID)
		committed := commitDurableTransition(
			t,
			fixture,
			runID,
			contract.RunStateRunning,
			"issue20_gap_committed",
			"c03-gap",
		)
		fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `
				DELETE FROM pm.server_events
				WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND sequence = $4
			`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID, committed.Sequence)
			if err == nil && tag.RowsAffected() != 1 {
				return fmt.Errorf("retention fixture deleted %d events", tag.RowsAffected())
			}
			return err
		})

		watchContext, stopWatch := context.WithTimeout(t.Context(), 5*time.Second)
		defer stopWatch()
		var observedEvents int
		watchResult, watchErr := client.WatchEvents(
			watchContext,
			pmapi.WorkspaceID(fixture.Scope.WorkspaceID),
			pmapi.RunID(runID),
			pmapi.WatchEventsOptions{
				Cursor: acceptedCursor,
				Handle: func(frame pmapi.RunEventFrame) error {
					if frame.RunEvent != nil {
						observedEvents++
					}
					if frame.Snapshot != nil && frame.Snapshot.RetentionGap {
						stopWatch()
					}
					return nil
				},
			},
		)
		if !errors.Is(watchErr, context.Canceled) || observedEvents != 0 ||
			!watchResult.Snapshot.RetentionGap || watchResult.Snapshot.WatermarkSequence != "2" ||
			watchResult.Snapshot.Run.RunRef != runID || watchResult.Cursor != watchResult.Snapshot.Cursor ||
			watchResult.Cursor == "" || watchResult.Cursor == acceptedCursor {
			t.Fatalf("retention-gap recovery = result %#v event_count %d err %v", watchResult, observedEvents, watchErr)
		}
	})

	t.Run("pm-cli-run-watch-E01-C04_slow_reader_resumes_last_delivered_cursor", func(t *testing.T) {
		fixture := newDurableEventFixture(t)
		client := newDurableEventClient(t, fixture)
		runID := admitDurableEventRun(t, fixture, "issue20-slow-reader")
		queued := commitDurableTransition(
			t,
			fixture,
			runID,
			contract.RunStateQueued,
			"issue20_second_committed",
			"c04-queued",
		)
		seedDurableBulkEvents(t, fixture, runID, queued.Sequence, 1000)

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
				t.Errorf("bound slow-reader receive buffer: %v", err)
				return
			}
			receiveConnection.Store(tcpConnection)
		}}
		tracedContext := httptrace.WithClientTrace(t.Context(), trace)
		watchContext, stopWatch := context.WithTimeout(tracedContext, 5*time.Second)
		defer stopWatch()
		entered := make(chan struct{})
		release := make(chan struct{})
		var callbacks atomic.Int32
		type watchOutcome struct {
			result pmapi.WatchEventsResult
			err    error
		}
		outcome := make(chan watchOutcome, 1)
		go func() {
			result, err := client.WatchEvents(
				watchContext,
				pmapi.WorkspaceID(fixture.Scope.WorkspaceID),
				pmapi.RunID(runID),
				pmapi.WatchEventsOptions{Handle: func(frame pmapi.RunEventFrame) error {
					if frame.RunEvent == nil {
						return nil
					}
					callbacks.Add(1)
					if frame.RunEvent.Sequence == "1" {
						close(entered)
						<-release
						return nil
					}
					if err := watchContext.Err(); err != nil {
						return err
					}
					return nil
				}},
			)
			outcome <- watchOutcome{result: result, err: err}
		}()
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("slow synchronous event consumer was not reached")
		}
		if callbacks.Load() != 1 {
			t.Fatalf("blocked event consumer received %d callbacks; want exactly one in-flight callback", callbacks.Load())
		}
		if receiveConnection.Load() == nil {
			t.Fatal("slow reader did not bind its controlled TCP receive buffer")
		}
		observation := time.NewTimer(2 * time.Second)
		defer observation.Stop()
		poll := time.NewTicker(10 * time.Millisecond)
		defer poll.Stop()
		for fixture.ActiveRequests() != 0 {
			select {
			case <-observation.C:
				t.Fatalf("slow event subscriber exceeded its bounded server write deadline with %d active requests", fixture.ActiveRequests())
			case <-poll.C:
			}
		}
		stopWatch()
		close(release)
		var interrupted watchOutcome
		select {
		case interrupted = <-outcome:
		case <-time.After(2 * time.Second):
			t.Fatal("slow event consumer did not stop within its cancellation budget")
		}
		if !errors.Is(interrupted.err, context.Canceled) || interrupted.result.Cursor == "" {
			t.Fatalf("slow consumer interruption = result %#v err %v", interrupted.result, interrupted.err)
		}

		resumeContext, stopResume := context.WithTimeout(t.Context(), 5*time.Second)
		defer stopResume()
		var resumed []string
		resumedResult, resumedErr := client.WatchEvents(
			resumeContext,
			pmapi.WorkspaceID(fixture.Scope.WorkspaceID),
			pmapi.RunID(runID),
			pmapi.WatchEventsOptions{
				Cursor: interrupted.result.Cursor,
				Handle: func(frame pmapi.RunEventFrame) error {
					if frame.RunEvent != nil {
						if err := resumeContext.Err(); err != nil {
							return err
						}
						resumed = append(resumed, frame.RunEvent.Sequence)
						stopResume()
					}
					return nil
				},
			},
		)
		if !errors.Is(resumedErr, context.Canceled) || !slices.Equal(resumed, []string{"2"}) ||
			resumedResult.Cursor == "" || resumedResult.Cursor == interrupted.result.Cursor {
			t.Fatalf("slow consumer resume = result %#v sequences %v err %v", resumedResult, resumed, resumedErr)
		}
	})
}

func newDurableEventFixture(t *testing.T) *testfixture.Fixture {
	t.Helper()
	key := sha256.Sum256([]byte("pm-cli-issue20-durable-event-fixture"))
	return testfixture.NewAdmitting(t, func(_ testfixture.ServiceDependencies, configuration *httpapi.ServiceConfig) error {
		configuration.EventBufferSize = 2
		configuration.EventWriteTimeout = 100 * time.Millisecond
		configuration.ResolveEventCursorKey = func(ctx context.Context) ([]byte, error) {
			if ctx == nil || ctx.Err() != nil {
				return nil, errors.New("event cursor signer requires a live request")
			}
			return bytes.Clone(key[:]), nil
		}
		return nil
	})
}

func newDurableEventClient(t *testing.T, fixture *testfixture.Fixture) *pmapi.QualifiedClient {
	t.Helper()
	handle, err := pmapi.DefaultSessionHandle("issue20-events", fixture.DeploymentID)
	if err != nil {
		t.Fatalf("derive durable event session handle: %v", err)
	}
	profile := pmapi.ServerProfile{
		Name:          "issue20-events",
		ServerURL:     fixture.BaseURL,
		InstanceID:    fixture.DeploymentID,
		SessionHandle: handle,
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client, err := pmapi.NewQualifiedClient(ctx, profile, pmapi.QualifiedClientOptions{
		HTTPClient:     fixture.Client,
		RequestTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("qualify durable event client: %v", err)
	}
	sessions := pmapi.NewMemorySessionStore()
	key, err := pmapi.NewSessionKey(fixture.DeploymentID, handle)
	if err != nil {
		t.Fatalf("construct durable event session key: %v", err)
	}
	if err := sessions.Store(ctx, key, pmapi.PMDeviceSession{
		ExpiresAt:           time.Now().Add(time.Hour),
		PmSessionCredential: "parent-owned-fixture-transport",
		Principal: pmapi.Principal{
			Kind:        pmapi.PrincipalKindUser,
			PrincipalId: fixture.Actor.RequestedActor.PrincipalID,
		},
		SessionId: "issue20-event-session",
	}); err != nil {
		t.Fatalf("store durable event session: %v", err)
	}
	client, err = client.WithSession(ctx, sessions)
	if err != nil {
		t.Fatalf("authenticate durable event client: %v", err)
	}
	return client
}

func admitDurableEventRun(t *testing.T, fixture *testfixture.Fixture, requestKey string) string {
	t.Helper()
	document, err := json.Marshal(testfixture.Invocation(t, fixture, requestKey))
	if err != nil {
		t.Fatalf("encode durable event admission: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fixture.BaseURL+"/v1/workspaces/"+url.PathEscape(fixture.Scope.WorkspaceID)+"/invocations",
		bytes.NewReader(document),
	)
	if err != nil {
		t.Fatalf("construct durable event admission: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", requestKey)
	response, err := fixture.Client.Do(request)
	if err != nil {
		t.Fatalf("submit durable event admission: %v", err)
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if readErr != nil || len(body) > 64*1024 || response.StatusCode != http.StatusAccepted {
		t.Fatalf("durable event admission = HTTP %d body %q read_err %v", response.StatusCode, body, readErr)
	}
	var admitted pmapi.AdmissionResponse
	if err := json.Unmarshal(body, &admitted); err != nil || admitted.RunRef == "" {
		t.Fatalf("decode durable event admission: %v %#v", err, admitted)
	}
	return admitted.RunRef
}

func acceptedDurableEventCursor(
	t *testing.T,
	client *pmapi.QualifiedClient,
	fixture *testfixture.Fixture,
	runID string,
) pmapi.EventCursor {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result, err := client.WatchEvents(
		ctx,
		pmapi.WorkspaceID(fixture.Scope.WorkspaceID),
		pmapi.RunID(runID),
		pmapi.WatchEventsOptions{Handle: func(frame pmapi.RunEventFrame) error {
			if frame.RunEvent != nil {
				if frame.RunEvent.Sequence != "1" {
					return fmt.Errorf("initial event sequence = %q; want 1", frame.RunEvent.Sequence)
				}
				cancel()
			}
			return nil
		}},
	)
	if !errors.Is(err, context.Canceled) || result.Cursor == "" {
		t.Fatalf("accepted event cursor = result %#v err %v", result, err)
	}
	return result.Cursor
}

func durableRunTransition(
	record contract.RunRecord,
	state contract.RunState,
	reason string,
	suffix string,
) (store.RunTransitionCommit, error) {
	next, err := record.Transition(state, reason, record.ProjectionObservedAt.Add(time.Second))
	if err != nil {
		return store.RunTransitionCommit{}, err
	}
	identity := fmt.Sprintf("issue20-%s-%d", suffix, next.Revision)
	safe := contract.SafePayload{
		References:  []contract.SafeReference{{Name: "run", Ref: record.Spec.RunID}},
		Counts:      []contract.SafeCount{},
		ReasonCodes: []string{reason},
	}
	return store.RunTransitionCommit{
		Run: next,
		Receipt: store.Receipt{
			ReceiptID: "receipt-" + identity,
			Kind:      "run_transition",
			RunID:     record.Spec.RunID,
			Payload:   safe,
		},
		Audit: store.AuditEntry{
			AuditID:   "audit-" + identity,
			Action:    "run_transitioned",
			TargetRef: record.Spec.RunID,
			Payload:   safe,
		},
		Event: store.EventInput{
			EventID: "event-" + identity,
			Type:    "run_transitioned",
			Payload: safe,
		},
		Outbox: store.OutboxCommand{
			CommandID:  "outbox-" + identity,
			Kind:       "project_run",
			PayloadRef: record.Spec.RunID,
		},
	}, nil
}

func seedDurableBulkEvents(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID string,
	after uint64,
	count int,
) {
	t.Helper()
	if count <= 0 || count > 1000 {
		t.Fatal("durable bulk event count must be between one and 1000")
	}
	references := make([]contract.SafeReference, 16)
	for index := range references {
		references[index] = contract.SafeReference{
			Name: fmt.Sprintf("issue20_ref_%02d", index),
			Ref:  fmt.Sprintf("%03d-%s", index, strings.Repeat("x", 500)),
		}
	}
	payload := contract.SafePayload{
		References:  references,
		Counts:      []contract.SafeCount{},
		ReasonCodes: []string{"issue20_bulk_committed"},
	}
	if err := payload.Validate(); err != nil {
		t.Fatalf("construct durable bulk event payload: %v", err)
	}
	document, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode durable bulk event payload: %v", err)
	}
	finalSequence := after + uint64(count)
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE pm.runs
			SET last_event_sequence = $4
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
			  AND last_event_sequence = $5
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID, finalSequence, after)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("bulk event watermark update affected %d rows", tag.RowsAffected())
		}
		tag, err = tx.Exec(ctx, `
			INSERT INTO pm.server_events (
				org_id, workspace_id, run_id, sequence, event_id,
				state_revision, event_type, safe_payload, committed_at
			)
			SELECT
				$1, $2, $3, generated.sequence,
				'issue20-bulk-event-' || generated.sequence::text,
				2, 'issue20_bulk_committed', $4::jsonb,
				clock_timestamp() + (generated.sequence * interval '1 microsecond')
			FROM generate_series($5::bigint + 1, $6::bigint) AS generated(sequence)
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID, string(document), after, finalSequence)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != int64(count) {
			return fmt.Errorf("bulk event insert affected %d rows, want %d", tag.RowsAffected(), count)
		}
		return nil
	})
}

func commitDurableTransition(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID string,
	state contract.RunState,
	reason string,
	suffix string,
) contract.ServerEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var event contract.ServerEvent
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		record, err := store.LoadRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		commit, err := durableRunTransition(record, state, reason, suffix)
		if err != nil {
			return err
		}
		event, err = store.CommitRunTransition(ctx, tx, commit)
		return err
	}); err != nil {
		t.Fatalf("commit durable run transition: %v", err)
	}
	return event
}

func loadDurableEvents(t *testing.T, fixture *testfixture.Fixture, runID string) []contract.ServerEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var events []contract.ServerEvent
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		events, err = store.ListEvents(ctx, tx, runID, 0, 16)
		return err
	}); err != nil {
		t.Fatalf("load durable event history: %v", err)
	}
	return events
}
