package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

// The checks in this file cover the object reservations of an earlier attempt
// of a unit. Each attempt reserves its own result object, and the fresh
// attempt also reserves a continuation object. The reservation IDs depend on
// the task lease fence, so a retry gets new IDs. When the retry settles, it
// must cancel each earlier held reservation. Each earlier bound reservation
// stays charged and unchanged, and only the internal audit entry records it.
//
// PostgreSQL and Temporal are the real stack. The provider and the object
// store are controlled peers. A scoped trigger, callback hooks and an SQL
// lease expiry force the order. No check sleeps, polls or retries.

const (
	earlierObjectsRetainedReason     = "earlier_attempt_objects_retained"
	earlierReservationsReleasedCount = "earlier_object_reservations_released"
	earlierReservationsRetainedCount = "earlier_object_reservations_retained"
)

func TestSettlementResolvesEarlierAttemptReservations(t *testing.T) {
	check := newUnadmittedCheck(t, "fixture.http.earlier-attempt-reservations")
	tailToken, sendToken := check.produceTokens(t, "earlier-reservations")

	t.Run("held reservations after the unadmitted recovery", func(t *testing.T) {
		run := check.admitResume(t, "earlier-reservations-unadmitted", sendToken)
		before := workspaceObjectReservations(t, check.fixture)
		bytes, attempts := workspaceObjectCapacity(t, check.fixture)
		check.failFirstSettlement(t, run, func(context.Context, app.ExecutionRequest, app.ExecutionCallback) (app.ExecutionObservation, error) {
			return app.ExecutionObservation{}, errors.New("controlled callback failure before the HTTP runtime")
		})
		assertLatestRunPermitState(t, check.fixture, run.reference.RunID, store.AccountPermitReserved)
		earlier := requireAttemptReservations(t, check.fixture, before, store.ObjectReservationHeld)

		unit, err := executePermitWorkerUnit(t, check.service, run)
		if err != nil || unit.State != contract.RunStateBlocked {
			t.Fatalf("unadmitted retry = %#v error=%v; want blocked with every member unsent", unit, err)
		}
		requireReservationStates(t, check.fixture, earlier, store.ObjectReservationReleased)
		requireWorkspaceObjectCapacity(t, check.fixture, bytes, attempts)
		requireEarlierReservationRecord(t, check.fixture, run.reference.RunID, 2, 0)
	})

	t.Run("held reservations after reconciliation", func(t *testing.T) {
		run := check.admitResume(t, "earlier-reservations-reconcile", sendToken)
		before := workspaceObjectReservations(t, check.fixture)
		bytes, attempts := workspaceObjectCapacity(t, check.fixture)
		var admitErr error
		check.failFirstSettlement(t, run, func(ctx context.Context, request app.ExecutionRequest, _ app.ExecutionCallback) (app.ExecutionObservation, error) {
			call, admitter, _, err := request.PhysicalCallAuthority()
			if err == nil {
				err = admitter.AdmitPhysicalCall(ctx, call)
			}
			admitErr = err
			return app.ExecutionObservation{}, errors.New("controlled callback failure after the admission")
		})
		if admitErr != nil {
			t.Fatalf("controlled admission: %v", admitErr)
		}
		assertLatestRunPermitState(t, check.fixture, run.reference.RunID, store.AccountPermitDispatched)
		earlier := requireAttemptReservations(t, check.fixture, before, store.ObjectReservationHeld)

		unit, err := executePermitWorkerUnit(t, check.service, run)
		if err != nil || unit.State != contract.RunStateReconciling {
			t.Fatalf("reconciliation retry = %#v error=%v; want reconciling", unit, err)
		}
		requireReservationStates(t, check.fixture, earlier, store.ObjectReservationReleased)
		requireWorkspaceObjectCapacity(t, check.fixture, bytes, attempts)
		requireEarlierReservationRecord(t, check.fixture, run.reference.RunID, 2, 0)
	})

	t.Run("bound reservations stay charged with a reason", func(t *testing.T) {
		before := workspaceObjectReservations(t, check.fixture)
		bytes, attempts := workspaceObjectCapacity(t, check.fixture)
		run, _ := check.failLocalTailSettlement(t, "earlier-reservations-bound", tailToken)
		earlier := requireAttemptReservations(t, check.fixture, before, store.ObjectReservationBound)

		unit, err := executePermitWorkerUnit(t, check.service, run)
		if err != nil || unit.State != contract.RunStateBlocked {
			t.Fatalf("unadmitted retry = %#v error=%v; want blocked with every member unsent", unit, err)
		}
		after := workspaceObjectReservations(t, check.fixture)
		var charged int64
		ids := make([]string, 0, len(earlier))
		for id, row := range earlier {
			if after[id] != row {
				t.Fatalf("earlier bound reservation %s = %#v; want it unchanged as %#v", id, after[id], row)
			}
			charged += row.charged
			ids = append(ids, id)
		}
		requireNoArtifacts(t, check.fixture, ids)
		requireWorkspaceObjectCapacity(t, check.fixture, bytes+charged, attempts+2)
		requireEarlierReservationRecord(t, check.fixture, run.reference.RunID, 0, 2)
	})

	t.Run("a first attempt records nothing", func(t *testing.T) {
		run := check.admitFirstPage(t, "earlier-reservations-first-attempt")
		unit, err := executePermitWorkerUnit(t, check.service, run)
		if err != nil || unit.State != contract.RunStateSucceeded {
			t.Fatalf("first-page unit = %#v error=%v; want succeeded", unit, err)
		}
		requireEarlierReservationRecord(t, check.fixture, run.reference.RunID, 0, 0)
	})
}

// failFirstSettlement runs the first attempt of run with hook and makes its
// settlement fail with a scoped trigger. It then expires the task lease, so
// the next attempt can claim the task.
func (check *unadmittedCheck) failFirstSettlement(t *testing.T, run permitWorkerRun, hook unadmittedHook) {
	t.Helper()
	remove := installWorkerSettlementFailure(t, check.fixture, run.reference.RunID)
	check.setHook(hook)
	unit, err := executePermitWorkerUnit(t, check.service, run)
	check.setHook(nil)
	remove()
	if err == nil || errors.Is(err, app.ErrWorkerConflict) {
		t.Fatalf("unit whose settlement failed = %#v error=%v; want a retryable settlement failure", unit, err)
	}
	if outcomes := unadmittedOutcomes(t, check.fixture, run); len(outcomes) != 0 {
		t.Fatalf("failed settlement wrote member outcomes %#v", outcomes)
	}
	expireWorkerTaskLease(t, check.fixture, run)
}

type objectReservationRow struct {
	purpose    string
	state      store.ObjectReservationState
	charged    int64
	attemptKey string
	digest     string
}

func workspaceObjectReservations(t *testing.T, fixture *testfixture.Fixture) map[string]objectReservationRow {
	t.Helper()
	reservations := map[string]objectReservationRow{}
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT reservation_id, purpose, state, charged_bytes,
			       coalesce(object_attempt_key, ''), coalesce(sha256, '')
			FROM pm.object_reservations
			WHERE org_id = $1 AND workspace_id = $2
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id, state string
			var row objectReservationRow
			if err := rows.Scan(&id, &row.purpose, &state, &row.charged, &row.attemptKey, &row.digest); err != nil {
				return err
			}
			row.state = store.ObjectReservationState(state)
			reservations[id] = row
		}
		return rows.Err()
	})
	return reservations
}

// requireAttemptReservations returns the reservations that are new since
// before. They must be exactly one result and one continuation reservation of
// the failed attempt, both in state.
func requireAttemptReservations(
	t *testing.T,
	fixture *testfixture.Fixture,
	before map[string]objectReservationRow,
	state store.ObjectReservationState,
) map[string]objectReservationRow {
	t.Helper()
	created := map[string]objectReservationRow{}
	purposes := []string{}
	for id, row := range workspaceObjectReservations(t, fixture) {
		if _, existed := before[id]; existed {
			continue
		}
		if row.state != state {
			t.Fatalf("failed attempt's reservation %s = %#v; want state %q", id, row, state)
		}
		created[id] = row
		purposes = append(purposes, row.purpose)
	}
	slices.Sort(purposes)
	if !slices.Equal(purposes, []string{string(store.ArtifactPurposeContinuation), string(store.ArtifactPurposePublic)}) {
		t.Fatalf("failed attempt's reservations = %#v; want one continuation and one public reservation", created)
	}
	return created
}

func requireReservationStates(
	t *testing.T,
	fixture *testfixture.Fixture,
	reservations map[string]objectReservationRow,
	state store.ObjectReservationState,
) {
	t.Helper()
	current := workspaceObjectReservations(t, fixture)
	for id, earlier := range reservations {
		if got := current[id].state; got != state {
			t.Fatalf("earlier %s reservation %s state = %q; want %q", earlier.purpose, id, got, state)
		}
	}
}

func workspaceObjectCapacity(t *testing.T, fixture *testfixture.Fixture) (int64, int64) {
	t.Helper()
	var bytes, attempts int64
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT reserved_bytes, reserved_attempts FROM pm.object_workspace_capacity
			WHERE org_id = $1 AND workspace_id = $2
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID).Scan(&bytes, &attempts)
	})
	return bytes, attempts
}

func requireWorkspaceObjectCapacity(t *testing.T, fixture *testfixture.Fixture, bytes, attempts int64) {
	t.Helper()
	if gotBytes, gotAttempts := workspaceObjectCapacity(t, fixture); gotBytes != bytes || gotAttempts != attempts {
		t.Fatalf("workspace object capacity bytes/attempts = %d/%d; want %d/%d", gotBytes, gotAttempts, bytes, attempts)
	}
}

func requireNoArtifacts(t *testing.T, fixture *testfixture.Fixture, artifactIDs []string) {
	t.Helper()
	var count int64
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM pm.artifacts
			WHERE org_id = $1 AND workspace_id = $2 AND artifact_id = ANY($3)
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, artifactIDs).Scan(&count)
	})
	if count != 0 {
		t.Fatalf("earlier bound reservations have %d registered artifacts; want none", count)
	}
}

// requireEarlierReservationRecord checks the run's one settlement. Only its
// internal audit entry may name the earlier reservations. The public receipt
// and event must not.
func requireEarlierReservationRecord(t *testing.T, fixture *testfixture.Fixture, runID string, released, retained uint64) {
	t.Helper()
	var encoded [3][]byte
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT
				(SELECT payload FROM pm.audit_entries
				 WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND action = 'worker_member_settlement'),
				(SELECT payload FROM pm.receipts
				 WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND kind = 'worker_settlement'),
				(SELECT safe_payload FROM pm.server_events
				 WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND event_type = 'worker_member_settlement')
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID).Scan(&encoded[0], &encoded[1], &encoded[2])
	})
	var payloads [3]contract.SafePayload
	for index, value := range encoded {
		if err := json.Unmarshal(value, &payloads[index]); err != nil {
			t.Fatalf("decode settlement payload %d: %v", index, err)
		}
	}
	audit := payloads[0]
	for name, want := range map[string]uint64{
		earlierReservationsReleasedCount: released,
		earlierReservationsRetainedCount: retained,
	} {
		got, present := safePayloadCount(audit, name)
		if present != (want != 0) || got != want {
			t.Fatalf("audit count %s = %d present=%t; want %d present=%t", name, got, present, want, want != 0)
		}
	}
	if slices.Contains(audit.ReasonCodes, earlierObjectsRetainedReason) != (retained != 0) {
		t.Fatalf("audit reason codes = %q; want %s present=%t", audit.ReasonCodes, earlierObjectsRetainedReason, retained != 0)
	}
	for index, public := range map[int]string{1: "receipt", 2: "event"} {
		payload := payloads[index]
		_, releasedPresent := safePayloadCount(payload, earlierReservationsReleasedCount)
		_, retainedPresent := safePayloadCount(payload, earlierReservationsRetainedCount)
		if releasedPresent || retainedPresent || slices.Contains(payload.ReasonCodes, earlierObjectsRetainedReason) {
			t.Fatalf("public settlement %s payload = %#v; want no earlier reservation field", public, payload)
		}
	}
}

func safePayloadCount(payload contract.SafePayload, name string) (uint64, bool) {
	for _, count := range payload.Counts {
		if count.Name == name {
			return count.Value, true
		}
	}
	return 0, false
}
