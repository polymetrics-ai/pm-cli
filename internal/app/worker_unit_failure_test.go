package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

// TestEndRunAfterUnitFailureRules checks the service rules of the step that
// ends a run after a unit activity failed. It needs PostgreSQL and Temporal.
// It is a developer check, not acceptance proof.
func TestEndRunAfterUnitFailureRules(t *testing.T) {
	temporal := newPermitTemporalHarness(t)

	t.Run("never_sent_unit_ends_failed_once", func(t *testing.T) {
		end := newUnitFailureEndRun(t, temporal, "never-sent")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		before := end.view(t)

		foreign := end.request
		foreign.Unit = contract.WorkflowUnitRef{TaskID: "unit-failure-foreign-task"}
		if _, err := end.service.EndRunAfterUnitFailure(ctx, end.run.execution, foreign); !errors.Is(err, app.ErrWorkerConflict) {
			t.Fatalf("end with a unit outside the run error = %v; want ErrWorkerConflict", err)
		}
		if unchanged := end.view(t); unchanged.Revision != before.Revision {
			t.Fatalf("refused end changed the run revision from %s to %s", before.Revision, unchanged.Revision)
		}

		ended, err := end.service.EndRunAfterUnitFailure(ctx, end.run.execution, end.request)
		if err != nil || ended.State != contract.RunStateFailed {
			t.Fatalf("end of a never sent unit = %#v error %v; want failed", ended, err)
		}
		view := end.view(t)
		if view.State != api.RunStateFailed || view.EffectSummary.Unsent != "1" || view.UnknownEffectCount != "0" ||
			view.OrchestrationCondition != api.OrchestrationConditionFailed {
			t.Fatalf("ended run view = %#v; want failed with one unsent effect and a failed orchestration", view)
		}
		end.requireEvidence(t, "unit_activity_failed")

		again, err := end.service.EndRunAfterUnitFailure(ctx, end.run.execution, end.request)
		if err != nil || again != ended {
			t.Fatalf("repeated end = %#v error %v; want the stored %#v", again, err, ended)
		}
		// The unqualified reference names the same unit with another key, so it
		// finds no stored receipt and reaches the terminal run.
		unqualified := end.request
		unqualified.Unit.DispatchID = ""
		terminal, err := end.service.EndRunAfterUnitFailure(ctx, end.run.execution, unqualified)
		if err != nil || terminal != ended {
			t.Fatalf("end of a terminal run = %#v error %v; want the unchanged %#v", terminal, err, ended)
		}
		if after := end.view(t); after.Revision != view.Revision {
			t.Fatalf("repeated and terminal ends changed the run revision from %s to %s", view.Revision, after.Revision)
		}
		end.requireEvidence(t, "unit_activity_failed")

		done, err := end.service.LoadWorkflow(ctx, end.run.execution, end.run.reference)
		if err != nil || !done.Done || len(done.Units) != 0 {
			t.Fatalf("snapshot after the end = %#v error %v; want done with no unit", done, err)
		}
		if sends := end.sends.Load(); sends != 0 {
			t.Fatalf("provider sends = %d; want none", sends)
		}
	})

	t.Run("cancel_request_ends_cancelled", func(t *testing.T) {
		end := newUnitFailureEndRun(t, temporal, "cancel-request")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		before := end.view(t)
		response := sendRunCancel(t, end.fixture, end.fixture.Client, end.request.Run.RunID, "unit-failure-end-cancel", before.Revision, true)
		if response.status != http.StatusAccepted {
			t.Fatalf("cancel status = %d; want 202: %s", response.status, response.document)
		}
		cancelled := decodeRunRoute[api.RunCancelResponse](t, response)
		applied, err := end.service.ApplyCommand(ctx, end.run.execution, contract.WorkflowCommandRequest{
			Run: end.run.reference, Command: contract.WorkflowCommandRef{CommandID: cancelled.CommandRef},
		})
		if err != nil || !applied.CancelRequested {
			t.Fatalf("apply durable cancel = %#v error %v; want applied cancellation", applied, err)
		}

		ended, err := end.service.EndRunAfterUnitFailure(ctx, end.run.execution, end.request)
		if err != nil || ended.State != contract.RunStateCancelled {
			t.Fatalf("end of a cancelled never sent unit = %#v error %v; want cancelled", ended, err)
		}
		view := end.view(t)
		if view.State != api.RunStateCancelled || view.EffectSummary.Unsent != "1" ||
			view.OrchestrationCondition != api.OrchestrationConditionFailed {
			t.Fatalf("cancelled run view = %#v; want cancelled with one unsent effect and a failed orchestration", view)
		}
		end.requireEvidence(t, "cancel_requested")
		if sends := end.sends.Load(); sends != 0 {
			t.Fatalf("provider sends = %d; want none", sends)
		}
	})

	t.Run("settled_unit_keeps_the_effect_aggregate", func(t *testing.T) {
		end := newUnitFailureEndRun(t, temporal, "settled")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// The unit settles, and the workflow then sees its activity fail, for
		// example because the reply was lost.
		settled, err := executePermitWorkerUnit(t, end.service, end.run)
		if err != nil || settled.State != contract.RunStateSucceeded {
			t.Fatalf("settled unit = %#v error %v; want succeeded", settled, err)
		}

		ended, err := end.service.EndRunAfterUnitFailure(ctx, end.run.execution, end.request)
		if err != nil || ended.State != contract.RunStateSucceeded {
			t.Fatalf("end of a settled unit = %#v error %v; want succeeded", ended, err)
		}
		view := end.view(t)
		if view.State != api.RunStateSucceeded || view.EffectSummary.Succeeded != "1" || view.EffectSummary.Unsent != "0" ||
			view.OrchestrationCondition != api.OrchestrationConditionFailed {
			t.Fatalf("settled run view = %#v; want succeeded with one succeeded effect and a failed orchestration", view)
		}
		end.requireEvidence(t, "effect_aggregate")
		if sends := end.sends.Load(); sends != 1 {
			t.Fatalf("provider sends = %d; want the one settled send", sends)
		}
	})
}

type unitFailureEndRun struct {
	fixture *testfixture.Fixture
	service *app.WorkerService
	run     permitWorkerRun
	request contract.WorkflowUnitRequest
	sends   *atomic.Int32
}

// newUnitFailureEndRun admits and seals one run whose single unit has a
// planned dispatch and an unsent effect. A unit execution sends once and
// succeeds.
func newUnitFailureEndRun(t *testing.T, temporal *permitTemporalHarness, label string) unitFailureEndRun {
	t.Helper()
	sends := &atomic.Int32{}
	fixture := testfixture.NewExecuting(
		t,
		func(_ context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
			sends.Add(1)
			return successfulPermitObservation(request), nil
		},
		unexpectedPermitReconciliation,
	)
	service := newPermitWorkerService(t, fixture, temporal)
	admitted := admitPermitRun(
		t, fixture, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID, "unit-failure-end-"+label,
	)
	run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
	return unitFailureEndRun{
		fixture: fixture,
		service: service,
		run:     run,
		request: contract.WorkflowUnitRequest{
			Run:  run.reference,
			Unit: contract.WorkflowUnitRef{TaskID: run.plan.TaskID, DispatchID: run.plan.Dispatch.DispatchID},
		},
		sends: sends,
	}
}

func (end unitFailureEndRun) view(t *testing.T) api.RunView {
	t.Helper()
	return getRunViewForTest(t, end.fixture.Client, runPathForTest(end.fixture, end.request.Run.RunID))
}

// requireEvidence requires the run transition reason of the end step and
// exactly one worker_run_projected event that names unit_activity_failed.
func (end unitFailureEndRun) requireEvidence(t *testing.T, transitionReason string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runID := end.request.Run.RunID
	var reason string
	if err := store.WithinScope(ctx, end.fixture.Pool, end.fixture.Scope, end.fixture.Actor, func(
		ctx context.Context,
		tx *store.Transaction,
	) error {
		record, err := store.LoadRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		if len(record.History) != 0 {
			reason = record.History[len(record.History)-1].Reason
		}
		return nil
	}); err != nil {
		t.Fatalf("load ended run: %v", err)
	}
	if reason != transitionReason {
		t.Fatalf("run transition reason = %q; want %q", reason, transitionReason)
	}
	var matching int
	if err := end.fixture.WithinSetupSQL(ctx, end.fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT safe_payload
			FROM pm.server_events
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND event_type = 'worker_run_projected'
			ORDER BY sequence
		`, end.fixture.Scope.OrgID, end.fixture.Scope.WorkspaceID, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var encoded []byte
			if err := rows.Scan(&encoded); err != nil {
				return err
			}
			var payload struct {
				ReasonCodes []string `json:"reason_codes"`
			}
			if err := json.Unmarshal(encoded, &payload); err != nil {
				return err
			}
			if slices.Contains(payload.ReasonCodes, "worker_projection") &&
				slices.Contains(payload.ReasonCodes, "unit_activity_failed") {
				matching++
			}
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read run projection events: %v", err)
	}
	if matching != 1 {
		t.Fatalf("worker_run_projected events with unit_activity_failed = %d; want one", matching)
	}
}
