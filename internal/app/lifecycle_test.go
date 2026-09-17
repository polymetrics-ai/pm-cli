package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

func newInitialRunFixture(t *testing.T) *testfixture.Fixture {
	t.Helper()
	return testfixture.NewAdmitting(t)
}

type initialRunResponse struct {
	status      int
	problemCode string
	problem     api.Problem
	admission   api.AdmissionResponse
	document    []byte
}

func TestAcceptedRunSurvivesClientExit(t *testing.T) {
	temporal := newPermitTemporalHarness(t)

	t.Run("pm-cli-run-watch-E02-C01_logout_or_session_expiry_does_not_cancel", func(t *testing.T) {
		var sends atomic.Int32
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
			t,
			fixture,
			fixture.Client,
			fixture.Scope,
			fixture.Connection.ConnectionID,
			"issue20-session-expiry",
		)
		run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)

		expirePermitTestSessions(t, fixture, fixture.Actor)
		requirePermitTestSessionExpired(t, fixture)

		result, err := executePermitWorkerUnit(t, service, run)
		if err != nil {
			t.Fatalf("execute accepted run after its viewing session expired: %v", err)
		}
		record := loadLifecycleRunRecord(t, fixture, admitted.RunRef)
		if result.State != contract.RunStateSucceeded || sends.Load() != 1 ||
			record.Spec.RunID != admitted.RunRef || record.CancelRequested {
			t.Fatalf(
				"session-independent accepted run = result %#v sends %d record %#v; want same succeeded run without cancellation",
				result,
				sends.Load(),
				record,
			)
		}
	})

	t.Run("pm-cli-run-watch-E02-C02_api_or_worker_death_recovers_accepted_run", func(t *testing.T) {
		var sends atomic.Int32
		fixture := testfixture.NewExecuting(
			t,
			func(_ context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
				sends.Add(1)
				return successfulPermitObservation(request), nil
			},
			unexpectedPermitReconciliation,
		)
		admitted := admitPermitRun(
			t,
			fixture,
			fixture.Client,
			fixture.Scope,
			fixture.Connection.ConnectionID,
			"issue20-api-recovery",
		)
		fixture.Restart(t)
		replacement := newPermitWorkerService(t, fixture, temporal)
		run := bindPermitWorkerRun(t, fixture, temporal, fixture.Scope, fixture.Actor, admitted.RunRef)
		run.plan = sealPermitWorkerPlan(
			t,
			fixture,
			replacement,
			fixture.Scope,
			fixture.Actor,
			run.execution,
			run.reference,
		)
		result, err := executePermitWorkerUnit(t, replacement, run)
		if err != nil {
			t.Fatalf("recover accepted run after API runtime replacement: %v", err)
		}
		view := getRunViewForTest(t, fixture.Client, runPathForTest(fixture, admitted.RunRef))
		if result.State != contract.RunStateSucceeded || sends.Load() != 1 ||
			run.reference.RunID != admitted.RunRef || view.RunRef != admitted.RunRef || view.CancelRequested {
			t.Fatalf(
				"recovered accepted run = result %#v sends %d reference %#v view %#v; want one uncancelled durable identity",
				result,
				sends.Load(),
				run.reference,
				view,
			)
		}
	})

	t.Run("pm-cli-run-watch-E02-C03_client_exit_second_client_observes_run", func(t *testing.T) {
		var sends atomic.Int32
		fixture := testfixture.NewExecuting(
			t,
			func(_ context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
				sends.Add(1)
				return successfulPermitObservation(request), nil
			},
			unexpectedPermitReconciliation,
		)
		firstClient := fixture.ClientFor(t, fixture.Actor)
		admitted := admitPermitRun(
			t,
			fixture,
			firstClient,
			fixture.Scope,
			fixture.Connection.ConnectionID,
			"issue20-client-exit",
		)
		firstClient.CloseIdleConnections()

		secondClient := fixture.ClientFor(t, fixture.Actor)
		view := getRunViewForTest(t, secondClient, runPathForTest(fixture, admitted.RunRef))
		service := newPermitWorkerService(t, fixture, temporal)
		run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
		result, err := executePermitWorkerUnit(t, service, run)
		if err != nil {
			t.Fatalf("continue accepted run after the admitting client exited: %v", err)
		}
		if view.RunRef != admitted.RunRef || view.State != api.RunStateAccepted || view.CancelRequested ||
			result.State != contract.RunStateSucceeded || sends.Load() != 1 {
			t.Fatalf(
				"second-client observation = view %#v result %#v sends %d; want accepted then independently succeeded",
				view,
				result,
				sends.Load(),
			)
		}
	})

	t.Run("pm-cli-run-watch-E02-C04_api_restart_serves_durable_run_and_outbox", func(t *testing.T) {
		fixture := testfixture.NewAdmitting(t)
		admitted := admitPermitRun(
			t,
			fixture,
			fixture.Client,
			fixture.Scope,
			fixture.Connection.ConnectionID,
			"issue20-api-restart",
		)
		beforeView := getRunViewForTest(t, fixture.Client, runPathForTest(fixture, admitted.RunRef))
		beforeOutbox := loadLifecycleStartOutbox(t, fixture, admitted.RunRef)

		fixture.Restart(t)

		afterView := getRunViewForTest(t, fixture.Client, runPathForTest(fixture, admitted.RunRef))
		afterOutbox := loadLifecycleStartOutbox(t, fixture, admitted.RunRef)
		if afterView.RunRef != beforeView.RunRef || afterView.Revision != beforeView.Revision ||
			afterView.State != beforeView.State || afterView.CancelRequested ||
			afterOutbox != beforeOutbox || afterOutbox.count != 1 {
			t.Fatalf(
				"post-restart durability = before view %#v outbox %#v; after view %#v outbox %#v",
				beforeView,
				beforeOutbox,
				afterView,
				afterOutbox,
			)
		}
	})

	t.Run("pm-cli-run-watch-E02-C05_worker_replacement_resumes_same_run_identity", func(t *testing.T) {
		var sends atomic.Int32
		fixture := testfixture.NewExecuting(
			t,
			func(_ context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
				sends.Add(1)
				return successfulPermitObservation(request), nil
			},
			unexpectedPermitReconciliation,
		)
		departing := newPermitWorkerService(t, fixture, temporal)
		admitted := admitPermitRun(
			t,
			fixture,
			fixture.Client,
			fixture.Scope,
			fixture.Connection.ConnectionID,
			"issue20-worker-replacement",
		)
		run := preparePermitWorkerRun(t, fixture, departing, temporal, fixture.Scope, fixture.Actor, admitted)
		deadContext, stopDeparting := context.WithCancel(context.Background())
		stopDeparting()
		if _, err := executePermitWorkerUnitContext(deadContext, departing, run); err == nil {
			t.Fatal("departing worker unexpectedly executed after its process context was cancelled")
		}
		if sends.Load() != 0 {
			t.Fatalf("departing worker reached provider execution %d times; want zero", sends.Load())
		}

		replacement := newPermitWorkerService(t, fixture, temporal)
		result, err := executePermitWorkerUnit(t, replacement, run)
		if err != nil {
			t.Fatalf("replacement worker resume accepted run: %v", err)
		}
		record := loadLifecycleRunRecord(t, fixture, admitted.RunRef)
		if result.TaskID != run.plan.TaskID || result.State != contract.RunStateSucceeded ||
			run.reference.RunID != admitted.RunRef || record.Spec.RunID != admitted.RunRef ||
			record.CancelRequested || sends.Load() != 1 {
			t.Fatalf(
				"replacement execution = result %#v reference %#v record %#v sends %d; want same uncancelled run/task and one send",
				result,
				run.reference,
				record,
				sends.Load(),
			)
		}
	})
}

type lifecycleOutboxIdentity struct {
	count     int64
	commandID string
	state     string
}

func loadLifecycleStartOutbox(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID string,
) lifecycleOutboxIdentity {
	t.Helper()
	var identity lifecycleOutboxIdentity
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*), COALESCE(min(command_id), ''), COALESCE(min(state::text), '')
			FROM pm.outbox_commands
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND kind = 'start_run'
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID).Scan(
			&identity.count,
			&identity.commandID,
			&identity.state,
		)
	})
	if identity.count != 1 || identity.commandID == "" || identity.state == "" {
		t.Fatalf("accepted run start outbox = %#v; want one durable command", identity)
	}
	return identity
}

func loadLifecycleRunRecord(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID string,
) contract.RunRecord {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var record contract.RunRecord
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		view, err := store.LoadRunView(ctx, tx, runID)
		if err != nil {
			return err
		}
		record = view.Record
		return nil
	}); err != nil {
		t.Fatalf("load accepted lifecycle run %q: %v", runID, err)
	}
	return record
}

func submitInitialRun(
	t *testing.T,
	fixture *testfixture.Fixture,
	client *http.Client,
	workspaceID, requestKey string,
) initialRunResponse {
	t.Helper()
	return submitInitialInvocation(t, fixture, client, workspaceID, testfixture.Invocation(t, fixture, requestKey))
}

func submitInitialInvocation(
	t *testing.T,
	fixture *testfixture.Fixture,
	client *http.Client,
	workspaceID string,
	request api.InvocationRequest,
) initialRunResponse {
	t.Helper()
	response, err := executeInitialInvocation(fixture, client, workspaceID, request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func executeInitialInvocation(
	fixture *testfixture.Fixture,
	client *http.Client,
	workspaceID string,
	request api.InvocationRequest,
) (initialRunResponse, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return initialRunResponse{}, fmt.Errorf("encode typed initial invocation: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fixture.BaseURL+"/v1/workspaces/"+url.PathEscape(workspaceID)+"/invocations",
		bytes.NewReader(encoded),
	)
	if err != nil {
		return initialRunResponse{}, fmt.Errorf("construct initial invocation: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Idempotency-Key", request.Protocol.RequestKey)
	httpResponse, err := client.Do(httpRequest)
	if err != nil {
		return initialRunResponse{}, fmt.Errorf("send initial invocation: %w", err)
	}
	defer httpResponse.Body.Close()
	document, err := io.ReadAll(io.LimitReader(httpResponse.Body, 64*1024+1))
	if err != nil || len(document) > 64*1024 {
		return initialRunResponse{}, errors.New("initial invocation response exceeded its bounded readable envelope")
	}
	result := initialRunResponse{status: httpResponse.StatusCode, document: document}
	if httpResponse.StatusCode == http.StatusAccepted {
		if err := json.Unmarshal(document, &result.admission); err != nil {
			return initialRunResponse{}, fmt.Errorf("decode admission response: %w", err)
		}
		return result, nil
	}
	if err := json.Unmarshal(document, &result.problem); err != nil {
		return initialRunResponse{}, errors.New("initial invocation returned a non-JSON problem envelope")
	}
	result.problemCode = result.problem.Code
	return result, nil
}

func exerciseInitialUnavailableHandler(t *testing.T) {
	t.Helper()
	t.Run("unavailable_handler", func(t *testing.T) {
		t.Run("C01_missing_handler", func(t *testing.T) {
			fixture := testfixture.New(t)
			response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "missing-handler")
			requireInitialProblem(t, response, http.StatusServiceUnavailable, "capability_unavailable")
		})

		t.Run("C02_foundation_mismatch", func(t *testing.T) {
			fixture := newConfiguredInitialFixture(
				t,
				initialSelection("fixture.read", "/fixture", "fixture-schema-v1"),
				initialRegistration("2.0.0"),
				initialRunPolicy(),
			)
			response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "foundation-mismatch")
			requireInitialProblem(t, response, http.StatusConflict, "foundation_incompatible")
		})

		t.Run("C03_registered_control", func(t *testing.T) {
			fixture := newInitialRunFixture(t)
			response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "registered-control")
			requireFreshInitialAdmission(t, response, "registered-control")
			var snapshot store.RunInputSnapshot
			if err := store.WithinScope(context.Background(), fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
				var err error
				snapshot, err = store.LoadRunInputSnapshot(ctx, tx, response.admission.RunRef)
				return err
			}); err != nil {
				t.Fatalf("load committed admission input: %v", err)
			}
			if snapshot.Lane != contract.LaneDirectRead {
				t.Fatalf("committed admission lane = %q; want requested %q", snapshot.Lane, contract.LaneDirectRead)
			}
		})
	})
}

func exerciseInitialAuthority(t *testing.T) {
	t.Helper()
	t.Run("authority", func(t *testing.T) {
		t.Run("C01_wrong_workspace", func(t *testing.T) {
			fixture := testfixture.New(t)
			foreignClient := fixture.ClientFor(t, fixture.ForeignActor)
			response := submitInitialRun(t, fixture, foreignClient, fixture.Scope.WorkspaceID, "wrong-workspace")
			requireInitialProblem(t, response, http.StatusNotFound, "resource_not_found")
		})

		t.Run("C02_denied_connection", func(t *testing.T) {
			fixture := testfixture.New(t)
			request := testfixture.Invocation(t, fixture, "denied-connection")
			request.ConnectionRef = fixture.ForeignConnection.ConnectionID
			response := submitInitialInvocation(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, request)
			requireInitialProblem(t, response, http.StatusNotFound, "resource_not_found")
		})

		t.Run("C03_expired_grant", func(t *testing.T) {
			fixture := testfixture.New(t)
			fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `
					UPDATE pm.memberships
					SET expires_at = clock_timestamp() - interval '1 second'
					WHERE identity_id = $1
				`, fixture.Actor.RequestedActor.PrincipalID)
				return err
			})
			response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "expired-grant")
			requireInitialProblem(t, response, http.StatusForbidden, "grant_unavailable")
		})

		t.Run("C04_revoked_grant", func(t *testing.T) {
			fixture := testfixture.New(t)
			fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `
					UPDATE pm.memberships
					SET revoked_at = clock_timestamp(), policy_epoch = policy_epoch + 1,
					    control_revision = control_revision + 1
					WHERE identity_id = $1
				`, fixture.Actor.RequestedActor.PrincipalID)
				return err
			})
			response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "revoked-grant")
			requireInitialProblem(t, response, http.StatusForbidden, "grant_unavailable")
		})

		t.Run("C05_stale_schema", func(t *testing.T) {
			fixture := newConfiguredInitialFixture(
				t,
				initialSelection("fixture.read", "/fixture", "fixture-schema-v2"),
				initialRegistration("1.0.0"),
				initialRunPolicy(),
			)
			response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "stale-schema")
			requireInitialProblem(t, response, http.StatusConflict, "specification_stale")
		})

		t.Run("C06_stale_descriptor", func(t *testing.T) {
			fixture := newConfiguredInitialFixture(
				t,
				initialSelection("fixture.read", "/fixture-v2", "fixture-schema-v1"),
				initialRegistration("1.0.0"),
				initialRunPolicy(),
			)
			response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "stale-descriptor")
			requireInitialProblem(t, response, http.StatusConflict, "specification_stale")
		})
	})
}

func exerciseInitialPendingBudget(t *testing.T) {
	t.Helper()
	t.Run("pending_budget", func(t *testing.T) {
		t.Run("C01_below_count", func(t *testing.T) {
			fixture := newInitialPolicyFixture(t, initialRunPolicy())
			first := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "pending-below-first")
			requireFreshInitialAdmission(t, first, "pending-below-first")
			finalSlot := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "pending-below-final-slot")
			requireFreshInitialAdmission(t, finalSlot, "pending-below-final-slot")
			if countInitialReservations(t, fixture, fixture.Scope) != 2 {
				t.Fatal("the final available workspace slot did not commit exactly one reservation")
			}
		})

		t.Run("C02_equal_count", func(t *testing.T) {
			t.Run("workspace", func(t *testing.T) {
				fixture := newInitialPolicyFixture(t, initialRunPolicy())
				for _, key := range []string{"pending-equal-one", "pending-equal-two"} {
					requireFreshInitialAdmission(t, submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, key), key)
				}
				response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "pending-equal-refused")
				requireInitialProblem(t, response, http.StatusTooManyRequests, "workspace_admission_count_limit")
				if countInitialReservations(t, fixture, fixture.Scope) != 2 {
					t.Fatal("workspace refusal discarded an accepted reservation")
				}
			})

			t.Run("deployment", func(t *testing.T) {
				policy := initialRunPolicy()
				policy.Pending.GlobalPending = 2
				fixture := newInitialPolicyFixture(t, policy)
				requireFreshInitialAdmission(
					t,
					submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "pending-global-primary"),
					"pending-global-primary",
				)
				foreignRequest := testfixture.Invocation(t, fixture, "pending-global-foreign")
				foreignRequest.ConnectionRef = fixture.ForeignConnection.ConnectionID
				foreignClient := fixture.ClientFor(t, fixture.ForeignActor)
				requireFreshInitialAdmission(
					t,
					submitInitialInvocation(t, fixture, foreignClient, fixture.ForeignScope.WorkspaceID, foreignRequest),
					"pending-global-foreign",
				)
				response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "pending-global-refused")
				requireInitialProblem(t, response, http.StatusServiceUnavailable, "deployment_admission_overload")
				if countInitialReservations(t, fixture, fixture.Scope) != 1 ||
					countInitialReservations(t, fixture, fixture.ForeignScope) != 1 {
					t.Fatal("deployment refusal discarded an accepted reservation")
				}
			})
		})

		t.Run("C03_above_count", func(t *testing.T) {
			higher := initialRunPolicy()
			higher.Pending.GlobalPending = 3
			higher.Pending.WorkspacePending = 3
			fixture := newInitialPolicyFixture(t, higher)
			for _, key := range []string{"pending-above-one", "pending-above-two", "pending-above-three"} {
				requireFreshInitialAdmission(t, submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, key), key)
			}
			lower := initialRunPolicy()
			lower.Pending.GlobalPending = 2
			lower.Pending.WorkspacePending = 2
			fixture.Restart(t, initialAdmissionConfigurator(
				initialSelection("fixture.read", "/fixture", "fixture-schema-v1"),
				initialRegistration("1.0.0"),
				lower,
			))
			response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "pending-above-refused")
			requireInitialProblem(t, response, http.StatusTooManyRequests, "workspace_admission_count_limit")
			if countInitialReservations(t, fixture, fixture.Scope) != 3 {
				t.Fatal("above-limit refusal altered pre-existing accepted reservations")
			}
		})

		t.Run("C04_age_limit", func(t *testing.T) {
			for _, test := range []struct {
				name       string
				ageSeconds int
				wantStatus int
			}{
				{name: "just_below", ageSeconds: 29, wantStatus: http.StatusAccepted},
				{name: "equal", ageSeconds: 30, wantStatus: http.StatusTooManyRequests},
				{name: "above", ageSeconds: 31, wantStatus: http.StatusTooManyRequests},
			} {
				t.Run(test.name, func(t *testing.T) {
					fixture := newInitialPolicyFixture(t, initialRunPolicy())
					first := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "pending-age-"+test.name+"-first")
					requireFreshInitialAdmission(t, first, "pending-age-"+test.name+"-first")
					setInitialReservationAge(t, fixture, first.admission.RunRef, test.ageSeconds)
					response := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "pending-age-"+test.name+"-next")
					if test.wantStatus == http.StatusAccepted {
						requireFreshInitialAdmission(t, response, "pending-age-"+test.name+"-next")
						return
					}
					requireInitialProblem(t, response, http.StatusTooManyRequests, "workspace_admission_age_limit")
					if countInitialReservations(t, fixture, fixture.Scope) != 1 {
						t.Fatal("age refusal discarded or replaced the accepted reservation")
					}
				})
			}
		})

		t.Run("C05_concurrent_last_slot", func(t *testing.T) {
			policy := initialRunPolicy()
			policy.Pending.GlobalPending = 1
			policy.Pending.WorkspacePending = 1
			fixture := newInitialPolicyFixture(t, policy)
			requests := map[string]api.InvocationRequest{
				"pending-race-left":  testfixture.Invocation(t, fixture, "pending-race-left"),
				"pending-race-right": testfixture.Invocation(t, fixture, "pending-race-right"),
			}
			type outcome struct {
				key      string
				response initialRunResponse
				err      error
			}
			outcomes := make(chan outcome, 2)
			for key, request := range requests {
				key, request := key, request
				go func() {
					response, err := executeInitialInvocation(fixture, fixture.Client, fixture.Scope.WorkspaceID, request)
					outcomes <- outcome{key: key, response: response, err: err}
				}()
			}
			statuses := make([]int, 0, 2)
			var accepted outcome
			for range 2 {
				result := <-outcomes
				if result.err != nil {
					t.Fatal(result.err)
				}
				statuses = append(statuses, result.response.status)
				if result.response.status == http.StatusAccepted {
					accepted = result
				} else {
					requireInitialProblem(t, result.response, http.StatusTooManyRequests, "workspace_admission_count_limit")
				}
			}
			slices.Sort(statuses)
			if !slices.Equal(statuses, []int{http.StatusAccepted, http.StatusTooManyRequests}) {
				t.Fatalf("last-slot statuses = %v; want one 202 and one 429", statuses)
			}
			replayed := submitInitialInvocation(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, requests[accepted.key])
			if replayed.status != http.StatusAccepted || !replayed.admission.Replay ||
				replayed.admission.RunRef != accepted.response.admission.RunRef {
				t.Fatalf("last-slot exact replay = %#v; want original accepted run", replayed)
			}
			if countInitialReservations(t, fixture, fixture.Scope) != 1 {
				t.Fatal("last-slot replay reserved a second slot")
			}
		})

		t.Run("C06_profiles_and_overrides", func(t *testing.T) {
			for _, test := range []struct {
				name      string
				admission string
				want      store.AdmissionLimits
			}{
				{
					name:      "development",
					admission: `{"profile":"development"}`,
					want: store.AdmissionLimits{
						GlobalPending: 32, WorkspacePending: 8, WorkspaceMaxPendingAge: 2 * time.Minute,
					},
				},
				{
					name:      "standard",
					admission: `{"profile":"standard"}`,
					want: store.AdmissionLimits{
						GlobalPending: 256, WorkspacePending: 32, WorkspaceMaxPendingAge: 10 * time.Minute,
					},
				},
				{
					name: "explicit overrides",
					admission: `{"profile":"development","overrides":{` +
						`"global_pending":5,"workspace_pending":2,"workspace_max_pending_age":"45s"}}`,
					want: store.AdmissionLimits{
						GlobalPending: 5, WorkspacePending: 2, WorkspaceMaxPendingAge: 45 * time.Second,
					},
				},
			} {
				t.Run(test.name, func(t *testing.T) {
					document, err := config.Load(bytes.NewBufferString(initialConfiguration(test.admission)))
					if err != nil {
						t.Fatal(err)
					}
					configuration, err := config.Validate(document, config.Prelaunch{})
					if err != nil {
						t.Fatal(err)
					}
					policy, err := app.RunPolicyFromConfiguration(configuration)
					if err != nil {
						t.Fatal(err)
					}
					if policy.Pending != test.want || policy.RunLifetime != time.Hour ||
						policy.InputMaxBytes != 1<<20 || policy.WorkspaceActiveRuns != 4 ||
						policy.WorkflowPayloadBytes != 1<<16 || policy.WorkflowFanout != 32 {
						t.Fatalf("resolved policy = %#v; want pending %#v and explicit finite limits", policy, test.want)
					}
				})
			}
		})

		t.Run("C07_prestart_projection_and_start_transition", func(t *testing.T) {
			fixture := newInitialRunFixture(t)
			accepted := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "prestart-projection")
			requireFreshInitialAdmission(t, accepted, "prestart-projection")
			before := getRunViewForTest(t, fixture.Client, runPathForTest(fixture, accepted.admission.RunRef))
			if contract.RunState(before.State) != contract.RunStateAccepted {
				t.Fatalf("pre-start run state = %q; want accepted", before.State)
			}
			pendingBefore, _, _ := pendingReservationForTest(t, fixture, accepted.admission.RunRef)
			if pendingBefore != 1 {
				t.Fatalf("pre-start outstanding reservation = %d; want 1", pendingBefore)
			}
			transitionInitialRun(t, fixture, accepted.admission.RunRef, contract.RunStateQueued, "temporal_submission_acknowledged")
			prestart := getRunViewForTest(t, fixture.Client, runPathForTest(fixture, accepted.admission.RunRef))
			pendingPrestart, releasedPrestart, _ := pendingReservationForTest(t, fixture, accepted.admission.RunRef)
			if contract.RunState(prestart.State) != contract.RunStateQueued || pendingPrestart != 1 || releasedPrestart {
				t.Fatalf("pre-start acknowledgement released reservation: view=%#v pending=%d released=%v", prestart, pendingPrestart, releasedPrestart)
			}
			transitionInitialRun(t, fixture, accepted.admission.RunRef, contract.RunStateRunning, "worker_execution_started")
			after := getRunViewForTest(t, fixture.Client, runPathForTest(fixture, accepted.admission.RunRef))
			if contract.RunState(after.State) != contract.RunStateRunning || after.Revision <= prestart.Revision {
				t.Fatalf("started run projection = %#v; want later running revision", after)
			}
			pendingAfter, released, reason := pendingReservationForTest(t, fixture, accepted.admission.RunRef)
			if pendingAfter != 0 || !released || reason != "execution_started" {
				t.Fatalf("started reservation = count %d released %v reason %q", pendingAfter, released, reason)
			}
		})
	})
}

func exerciseInitialRetryReceipts(t *testing.T) {
	t.Helper()
	t.Run("retry_receipts", func(t *testing.T) {
		t.Run("C01_unresolved_beyond_retention", func(t *testing.T) {
			fixture := newInitialRunFixture(t)
			first := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "receipt-unresolved")
			requireFreshInitialAdmission(t, first, "receipt-unresolved")
			ageAdmissionReceipt(t, fixture, first.admission.RunRef, false)
			replayed := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "receipt-unresolved")
			if replayed.status != http.StatusAccepted || !replayed.admission.Replay ||
				replayed.admission.RunRef != first.admission.RunRef {
				t.Fatalf("unresolved aged replay = %#v; want original accepted run", replayed)
			}
			if compactedAdmissionReceipt(t, fixture, first.admission.RunRef) {
				t.Fatal("unresolved admission response was compacted")
			}
		})

		t.Run("C02_resolved_response_expired", func(t *testing.T) {
			fixture := newInitialRunFixture(t)
			first := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "receipt-expired")
			requireFreshInitialAdmission(t, first, "receipt-expired")
			transitionInitialRun(t, fixture, first.admission.RunRef, contract.RunStateFailed, "fixture_terminal_failure")
			ageAdmissionReceipt(t, fixture, first.admission.RunRef, true)
			expired := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "receipt-expired")
			problem := requireInitialProblem(t, expired, http.StatusGone, "retry_receipt_expired")
			if problem.OutcomeReference == nil || problem.OutcomeReference.Ref != first.admission.RunRef ||
				problem.OutcomeReference.Kind != api.SafeOutcomeReferenceKindRun {
				t.Fatalf("expired receipt outcome = %#v; want authorized run reference", problem.OutcomeReference)
			}
			if !compactedAdmissionReceipt(t, fixture, first.admission.RunRef) {
				t.Fatal("410 was emitted before admission compaction committed")
			}
		})

		t.Run("C03_changed_intent_after_compaction", func(t *testing.T) {
			fixture := newInitialRunFixture(t)
			first := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "receipt-changed")
			requireFreshInitialAdmission(t, first, "receipt-changed")
			transitionInitialRun(t, fixture, first.admission.RunRef, contract.RunStateFailed, "fixture_terminal_failure")
			ageAdmissionReceipt(t, fixture, first.admission.RunRef, true)
			requireInitialProblem(
				t,
				submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "receipt-changed"),
				http.StatusGone,
				"retry_receipt_expired",
			)
			changed := testfixture.Invocation(t, fixture, "receipt-changed")
			changed.Input = contract.NewObjectValue(map[string]contract.PresentValue{
				"changed": contract.NewStringValue("intent"),
			})
			response := submitInitialInvocation(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, changed)
			requireInitialProblem(t, response, http.StatusConflict, "idempotency_conflict")
		})

		t.Run("C04_compaction_races_recovery", func(t *testing.T) {
			fixture := newInitialRunFixture(t)
			first := submitInitialRun(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, "receipt-race")
			requireFreshInitialAdmission(t, first, "receipt-race")
			transitionInitialRun(t, fixture, first.admission.RunRef, contract.RunStateFailed, "fixture_terminal_failure")
			ageAdmissionReceipt(t, fixture, first.admission.RunRef, true)
			type outcome struct {
				response initialRunResponse
				err      error
			}
			outcomes := make(chan outcome, 2)
			request := testfixture.Invocation(t, fixture, "receipt-race")
			for range 2 {
				go func() {
					response, err := executeInitialInvocation(fixture, fixture.Client, fixture.Scope.WorkspaceID, request)
					outcomes <- outcome{response: response, err: err}
				}()
			}
			for range 2 {
				result := <-outcomes
				if result.err != nil {
					t.Fatal(result.err)
				}
				requireInitialProblem(t, result.response, http.StatusGone, "retry_receipt_expired")
			}
			if !compactedAdmissionReceipt(t, fixture, first.admission.RunRef) {
				t.Fatal("racing recovery left the resolved response uncompact")
			}
			assertSingleAdmissionRun(t, fixture, first.admission.RunRef)
		})
	})
}

func newInitialPolicyFixture(t *testing.T, policy store.RunBudgetPolicy) *testfixture.Fixture {
	t.Helper()
	return newConfiguredInitialFixture(
		t,
		initialSelection("fixture.read", "/fixture", "fixture-schema-v1"),
		initialRegistration("1.0.0"),
		policy,
	)
}

func newConfiguredInitialFixture(
	t *testing.T,
	selection app.OperationSelection,
	registration app.HandlerRegistration,
	policy store.RunBudgetPolicy,
) *testfixture.Fixture {
	t.Helper()
	return testfixture.New(t, initialAdmissionConfigurator(selection, registration, policy))
}

func initialAdmissionConfigurator(
	selection app.OperationSelection,
	registration app.HandlerRegistration,
	policy store.RunBudgetPolicy,
) testfixture.ConfigureService {
	return func(dependencies testfixture.ServiceDependencies, configuration *httpapi.ServiceConfig) error {
		registry := &app.HandlerRegistry{}
		if err := registry.RegisterHTTP(registration); err != nil {
			return err
		}
		admission, err := app.NewAdmissionService(app.AdmissionServiceConfig{
			Pool:         dependencies.Pool,
			DeploymentID: dependencies.DeploymentID,
			Authorize:    configuration.Auth.AuthorizeResource,
			Registry:     registry,
			ResolveOperation: func(
				_ context.Context,
				scope contract.Scope,
				connectionID string,
				operationID string,
			) (app.OperationSelection, error) {
				primary := scope.Equal(dependencies.Scope) && connectionID == dependencies.Connection.ConnectionID
				foreign := scope.Equal(dependencies.ForeignScope) && connectionID == dependencies.ForeignConnection.ConnectionID
				if operationID != dependencies.OperationID || !primary && !foreign {
					return app.OperationSelection{}, errors.New("controlled operation is unavailable")
				}
				return selection, nil
			},
			Policy: policy,
			Random: rand.Reader,
		})
		if err != nil {
			return err
		}
		configuration.Admission = admission
		return nil
	}
}

func initialSelection(operationID, path, schemaRef string) app.OperationSelection {
	foundation := contract.FoundationRef{ID: "FND-6", Version: "1.0.0"}
	sourceRefs := []contract.SourceRef{
		{SourceID: "fixture-source", Pointer: "#/operations/fixture"},
	}
	return app.OperationSelection{
		Operation: contract.Operation{
			SchemaVersion:   contract.OperationSchemaVersion,
			OperationID:     operationID,
			SourceRefs:      sourceRefs,
			Transport:       contract.Transport{Kind: contract.TransportKindHTTP, Method: http.MethodGet, Path: path},
			Inputs:          []contract.FieldBinding{},
			RequestVariants: []contract.RequestVariant{},
			Responses: []contract.Response{
				{
					Status:     "200",
					NoContent:  false,
					MediaType:  "application/json",
					Schema:     &contract.Schema{Type: contract.SchemaTypeString},
					SourceRefs: sourceRefs,
				},
			},
			CapabilityRequirements: []contract.CapabilityRequirement{
				{Capability: "http.execute", Foundation: foundation},
			},
			LaneEligibility: operationLaneEligibilityForTest(contract.LaneDirectRead),
		},
		SchemaRefs: []string{schemaRef},
	}
}

func initialRegistration(version string) app.HandlerRegistration {
	foundation := contract.FoundationRef{ID: "FND-6", Version: version}
	return app.HandlerRegistration{
		Lanes: []contract.Lane{contract.LaneDirectRead},
		Foundations: []contract.FoundationContract{
			{ID: foundation.ID, Version: foundation.Version, Dependencies: []contract.FoundationRef{}},
		},
		Capabilities: []contract.CapabilityRequirement{
			{Capability: "http.execute", Foundation: foundation},
		},
		Execute: func(context.Context, app.ExecutionRequest) (app.ExecutionObservation, error) {
			return app.ExecutionObservation{}, errors.New("admission must not execute the controlled handler callback")
		},
	}
}

func initialRunPolicy() store.RunBudgetPolicy {
	return store.RunBudgetPolicy{
		Pending: store.AdmissionLimits{
			GlobalPending:          3,
			WorkspacePending:       2,
			WorkspaceMaxPendingAge: 30 * time.Second,
		},
		RunLifetime:          time.Hour,
		InputMaxBytes:        1 << 20,
		WorkspaceActiveRuns:  4,
		WorkflowPayloadBytes: 1 << 16,
		WorkflowFanout:       32,
	}
}

func initialConfiguration(admission string) string {
	return `{
		"schema_version":"1",
		"deployment_id":"admission-policy-fixture",
		"public_origin":"https://pm.example.invalid",
		"server":{"listen":"127.0.0.1:8080","drain_timeout":"2s","trusted_proxies":["127.0.0.1/32"]},
		"postgres":{"ownership":"managed","database":"pm_app","endpoint_ref":"secret://fixture/postgres"},
		"temporal":{"ownership":"managed","endpoint":"temporal.example.invalid:7233","namespace":"fixture","client_identity_ref":"secret://fixture/temporal","retention":"720h","task_queues":["pm-control"]},
		"artifacts":{"ownership":"managed","driver":"s3","endpoint":"https://objects.example.invalid","bucket":"fixture-objects","credential_ref":"secret://fixture/objects","attempt_key_policy":"unique_immutable"},
		"identity":{"issuer":"https://identity.example.invalid","client_id":"pm-server","client_secret_ref":"secret://fixture/oidc","step_up_maximum_age":"2m"},
		"encryption":{"wrapping_key_ref":"secret://fixture/wrapping"},
		"admission":` + admission + `,
		"limits":{"workspace_active_runs":4,"workflow_payload_bytes":65536,"workflow_fanout":32,"run_lifetime":"1h","event_buffer":128,"query_children":1,"query_child_memory_bytes":4294967296,"query_child_deadline":"5m","request_bytes":1048576,"metadata_deadline":"5s"}
	}`
}

func requireInitialProblem(t *testing.T, response initialRunResponse, status int, code string) api.Problem {
	t.Helper()
	if response.status != status || response.problem.Status != status || response.problemCode != code {
		t.Fatalf("initial problem = HTTP %d %#v; want %d/%s", response.status, response.problem, status, code)
	}
	return response.problem
}

func requireFreshInitialAdmission(t *testing.T, response initialRunResponse, requestKey string) api.AdmissionResponse {
	t.Helper()
	if response.status != http.StatusAccepted || response.admission.RunRef == "" ||
		response.admission.RequestKey != requestKey || response.admission.Replay ||
		response.admission.State != api.AdmissionResponseStateAccepted || response.admission.IntentDigest == "" {
		t.Fatalf("fresh admission = HTTP %d %#v", response.status, response.admission)
	}
	return response.admission
}
func countInitialReservations(t *testing.T, fixture *testfixture.Fixture, scope contract.Scope) int64 {
	t.Helper()
	var reservations int64
	fixture.SetupSQL(t, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*)
			FROM pm.pending_reservations
			WHERE released_at IS NULL
		`).Scan(&reservations)
	})
	return reservations
}

func setInitialReservationAge(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID string,
	ageSeconds int,
) {
	t.Helper()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE pm.pending_reservations
			SET accepted_at = clock_timestamp() - make_interval(secs => $2)
			WHERE run_id = $1 AND released_at IS NULL
		`, runID, ageSeconds)
		if err == nil && tag.RowsAffected() != 1 {
			return errors.New("controlled pending reservation is absent")
		}
		return err
	})
}

func transitionInitialRun(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID string,
	state contract.RunState,
	reason string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		run, err := store.LoadRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		next, err := run.Transition(state, reason, run.ProjectionObservedAt.Add(time.Second))
		if err != nil {
			return err
		}
		identity := fmt.Sprintf("%s-%d", runID, next.Revision)
		safe := contract.SafePayload{
			References:  []contract.SafeReference{{Name: "run", Ref: runID}},
			Counts:      []contract.SafeCount{},
			ReasonCodes: []string{reason},
		}
		_, err = store.CommitRunTransition(ctx, tx, store.RunTransitionCommit{
			Run: next,
			Receipt: store.Receipt{
				ReceiptID: "receipt-transition-" + identity,
				Kind:      "run_transition",
				RunID:     runID,
				Payload:   safe,
			},
			Audit: store.AuditEntry{
				AuditID:   "audit-transition-" + identity,
				Action:    "run_transitioned",
				TargetRef: runID,
				Payload:   safe,
			},
			Event: store.EventInput{
				EventID: "event-transition-" + identity,
				Type:    "run_transitioned",
				Payload: safe,
			},
			Outbox: store.OutboxCommand{
				CommandID:  "outbox-transition-" + identity,
				Kind:       "project_run",
				PayloadRef: runID,
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("commit controlled run transition: %v", err)
	}
}

func pendingReservationForTest(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID string,
) (int64, bool, string) {
	t.Helper()
	var outstanding int64
	var releasedAt *time.Time
	var reason *string
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE released_at IS NULL), max(released_at), max(release_reason)
			FROM pm.pending_reservations
			WHERE run_id = $1
		`, runID).Scan(&outstanding, &releasedAt, &reason)
	})
	resolvedReason := ""
	if reason != nil {
		resolvedReason = *reason
	}
	return outstanding, releasedAt != nil, resolvedReason
}

func ageAdmissionReceipt(t *testing.T, fixture *testfixture.Fixture, runID string, resolved bool) {
	t.Helper()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		if resolved {
			_, err := tx.Exec(ctx, `
				UPDATE pm.admission_keys
				SET resolved_at = clock_timestamp() - interval '169 hours'
				WHERE run_id = $1
			`, runID)
			return err
		}
		_, err := tx.Exec(ctx, `
			WITH aged AS (
				SELECT clock_timestamp() - interval '169 hours' AS accepted_at
			)
			UPDATE pm.admission_keys AS admission
			SET accepted_at = aged.accepted_at,
			    safe_response = jsonb_set(
			        admission.safe_response,
			        '{accepted_at}',
			        to_jsonb(aged.accepted_at),
			        false
			    )
			FROM aged
			WHERE admission.run_id = $1
			  AND admission.resolved_at IS NULL
			  AND admission.safe_response IS NOT NULL
		`, runID)
		return err
	})
}

func compactedAdmissionReceipt(t *testing.T, fixture *testfixture.Fixture, runID string) bool {
	t.Helper()
	var compacted bool
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT safe_response IS NULL
			FROM pm.admission_keys
			WHERE run_id = $1
		`, runID).Scan(&compacted)
	})
	return compacted
}

func assertSingleAdmissionRun(t *testing.T, fixture *testfixture.Fixture, runID string) {
	t.Helper()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		var keys, runs, commands int64
		if err := tx.QueryRow(ctx, `
			SELECT
				(SELECT count(*) FROM pm.admission_keys WHERE run_id = $1),
				(SELECT count(*) FROM pm.run_specs WHERE run_id = $1),
				(SELECT count(*) FROM pm.outbox_commands WHERE run_id = $1 AND kind = 'start_run')
		`, runID).Scan(&keys, &runs, &commands); err != nil {
			return err
		}
		if keys != 1 || runs != 1 || commands != 1 {
			return fmt.Errorf("recovery duplicated admission rows: keys=%d runs=%d start_commands=%d", keys, runs, commands)
		}
		return nil
	})
}
