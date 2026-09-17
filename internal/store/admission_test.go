package store_test

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
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/oapi-codegen/nullable"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

func TestDurableAcceptedBefore202(t *testing.T) {
	t.Run("S01/C01_committed", func(t *testing.T) {
		fixture := testfixture.NewAdmitting(t)
		request := testfixture.Invocation(t, fixture, "committed-before-response")
		var observedRun string
		client := wrapAdmissionClient(fixture.Client, &observingAdmissionTransport{
			base: fixture.Client.Transport,
			observe: func(response *http.Response) error {
				if response.StatusCode != http.StatusAccepted {
					return fmt.Errorf("observed HTTP %d before caller received response", response.StatusCode)
				}
				runID, err := lookupAdmissionRun(context.Background(), fixture, request.Protocol.RequestKey)
				if err != nil {
					return fmt.Errorf("observe committed admission before returning 202: %w", err)
				}
				assertAdmissionRows(t, fixture, runID)
				observedRun = runID
				return nil
			},
		})
		response := requireAdmissionRoute(t, fixture, client, request)
		admission := requireAcceptedAdmission(t, response, false)
		if observedRun == "" || admission.RunRef != observedRun {
			t.Fatalf("202 run = %q, pre-response durable run = %q", admission.RunRef, observedRun)
		}
		assertRunReadable(t, fixture, admission.RunRef)
	})

	t.Run("S02", func(t *testing.T) {
		t.Run("C01_lost_response", func(t *testing.T) {
			fixture := testfixture.NewAdmitting(t)
			request := testfixture.Invocation(t, fixture, "lost-response")
			lost := errors.New("injected response loss after HTTP completion")
			client := wrapAdmissionClient(fixture.Client, &lostAdmissionTransport{
				base: fixture.Client.Transport,
				err:  lost,
			})
			if _, err := executeAdmissionRoute(fixture, client, request); !errors.Is(err, lost) {
				t.Fatalf("lost response error = %v; want injected transport loss", err)
			}
			runID, err := lookupAdmissionRun(context.Background(), fixture, request.Protocol.RequestKey)
			if err != nil {
				t.Fatalf("lookup committed lost response: %v", err)
			}
			replayed := requireAcceptedAdmission(t, requireAdmissionRoute(t, fixture, fixture.Client, request), true)
			if replayed.RunRef != runID {
				t.Fatalf("lost-response replay run = %q; want %q", replayed.RunRef, runID)
			}
			assertAdmissionRows(t, fixture, runID)
		})

		t.Run("C02_api_restart", func(t *testing.T) {
			fixture := testfixture.NewAdmitting(t)
			request := testfixture.Invocation(t, fixture, "api-restart")
			accepted := requireAcceptedAdmission(t, requireAdmissionRoute(t, fixture, fixture.Client, request), false)
			fixture.Restart(t)
			replayed := requireAcceptedAdmission(t, requireAdmissionRoute(t, fixture, fixture.Client, request), true)
			if replayed.RunRef != accepted.RunRef {
				t.Fatalf("restart replay run = %q; want %q", replayed.RunRef, accepted.RunRef)
			}
			assertAdmissionRows(t, fixture, accepted.RunRef)
		})

		t.Run("C03_unknown_commit_acknowledgement", func(t *testing.T) {
			var faultPool *store.Pool
			var fault *store.CommitReplyFaultForTest
			fixture := testfixture.New(t, func(dependencies testfixture.ServiceDependencies, configuration *httpapi.ServiceConfig) error {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				var err error
				faultPool, fault, err = store.NewCommitReplyFaultPoolForTest(ctx, dependencies.Pool)
				if err != nil {
					return err
				}
				return configureAdmissionPool(dependencies, configuration, faultPool)
			})
			t.Cleanup(faultPool.Close)
			request := testfixture.Invocation(t, fixture, "unknown-commit-ack")
			observedRun := make(chan string, 1)
			if err := fault.Arm(func() error {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				for {
					runID, err := lookupAdmissionRun(ctx, fixture, request.Protocol.RequestKey)
					if err == nil {
						observedRun <- runID
						return nil
					}
					if !errors.Is(err, store.ErrNotFound) {
						return err
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-ticker.C:
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			unknown := requireAdmissionRoute(t, fixture, fixture.Client, request)
			problem := requireAdmissionProblem(t, unknown, http.StatusServiceUnavailable, "admission_outcome_unknown")
			if problem.SafeRetryAction != "same_key_reconcile_only" || problem.OutcomeReference == nil ||
				problem.OutcomeReference.Kind != api.SafeOutcomeReferenceKindRequest ||
				problem.OutcomeReference.Ref != request.Protocol.RequestKey {
				t.Fatalf("unknown outcome recovery envelope = %#v", problem)
			}
			if !fault.Completed() {
				t.Fatal("physical COMMIT reply fault did not observe the committed admission")
			}
			runID := <-observedRun
			replayed := requireAcceptedAdmission(t, requireAdmissionRoute(t, fixture, fixture.Client, request), true)
			if replayed.RunRef != runID {
				t.Fatalf("unknown-outcome replay run = %q; want %q", replayed.RunRef, runID)
			}
			assertAdmissionRows(t, fixture, runID)
		})
	})

	t.Run("S03/C01_changed_input", func(t *testing.T) {
		fixture := testfixture.NewAdmitting(t)
		request := testfixture.Invocation(t, fixture, "changed-input")
		accepted := requireAcceptedAdmission(t, requireAdmissionRoute(t, fixture, fixture.Client, request), false)
		changed := request
		changed.Input = contract.NewObjectValue(map[string]contract.PresentValue{
			"changed": contract.NewStringValue("actual typed input"),
		})
		changed.Protocol.IntentDigest = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
		response := requireAdmissionRoute(t, fixture, fixture.Client, changed)
		requireAdmissionProblem(t, response, http.StatusConflict, "idempotency_conflict")
		assertAdmissionRows(t, fixture, accepted.RunRef)
	})

	t.Run("S04", func(t *testing.T) {
		t.Run("C01_rollback", func(t *testing.T) {
			fixture := testfixture.NewAdmitting(t)
			removeFault := installAdmissionRollbackFault(t, fixture)
			t.Cleanup(removeFault)
			request := testfixture.Invocation(t, fixture, "precommit-rollback")
			response := requireAdmissionRoute(t, fixture, fixture.Client, request)
			problem := requireAdmissionProblem(t, response, http.StatusServiceUnavailable, "control_store_unavailable")
			if problem.SafeRetryAction != "same_key_after_dependency_recovery" {
				t.Fatalf("rollback retry action = %q", problem.SafeRetryAction)
			}
			assertAdmissionAbsent(t, fixture, request.Protocol.RequestKey)
		})

		t.Run("C02_postgres_unavailable", func(t *testing.T) {
			var admissionPool *store.Pool
			fixture := testfixture.New(t, func(dependencies testfixture.ServiceDependencies, configuration *httpapi.ServiceConfig) error {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				pool, _, err := store.NewCommitReplyFaultPoolForTest(ctx, dependencies.Pool)
				if err != nil {
					return err
				}
				admissionPool = pool
				return configureAdmissionPool(dependencies, configuration, admissionPool)
			})
			admissionPool.Close()
			request := testfixture.Invocation(t, fixture, "postgres-unavailable")
			response := requireAdmissionRoute(t, fixture, fixture.Client, request)
			requireAdmissionProblem(t, response, http.StatusServiceUnavailable, "control_store_unavailable")
			assertAdmissionAbsent(t, fixture, request.Protocol.RequestKey)
		})
	})

	t.Run("S05", func(t *testing.T) {
		t.Run("C01_same_fingerprint", func(t *testing.T) {
			fixture := testfixture.NewAdmitting(t)
			left := testfixture.Invocation(t, fixture, "same-fingerprint-race")
			right := left
			right.Protocol.IntentDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
			responses := raceAdmissions(t, fixture, left, right)
			if responses[0].status != http.StatusAccepted || responses[1].status != http.StatusAccepted {
				t.Fatalf("same-fingerprint race statuses = %d, %d; want 202, 202", responses[0].status, responses[1].status)
			}
			first := requireAcceptedAdmission(t, responses[0], responses[0].admission.Replay)
			second := requireAcceptedAdmission(t, responses[1], responses[1].admission.Replay)
			if first.RunRef != second.RunRef || first.Replay == second.Replay {
				t.Fatalf("same-fingerprint race = %#v %#v; want one original and one replay", first, second)
			}
			assertAdmissionRows(t, fixture, first.RunRef)
		})

		t.Run("C02_different_fingerprint", func(t *testing.T) {
			fixture := testfixture.NewAdmitting(t)
			left := testfixture.Invocation(t, fixture, "different-fingerprint-race")
			right := left
			right.ApprovalRef = nullable.NewNullableWithValue("approval-race")
			responses := raceAdmissions(t, fixture, left, right)
			statuses := []int{responses[0].status, responses[1].status}
			slices.Sort(statuses)
			if !slices.Equal(statuses, []int{http.StatusAccepted, http.StatusConflict}) {
				t.Fatalf("different-fingerprint race statuses = %v; want 202 and 409", statuses)
			}
			var runID string
			for _, response := range responses {
				if response.status == http.StatusAccepted {
					runID = requireAcceptedAdmission(t, response, false).RunRef
				} else {
					requireAdmissionProblem(t, response, http.StatusConflict, "idempotency_conflict")
				}
			}
			assertAdmissionRows(t, fixture, runID)
		})
	})
}

type admissionRouteResponse struct {
	status    int
	document  []byte
	admission api.AdmissionResponse
	problem   api.Problem
}

func executeAdmissionRoute(
	fixture *testfixture.Fixture,
	client *http.Client,
	request api.InvocationRequest,
) (admissionRouteResponse, error) {
	document, err := json.Marshal(request)
	if err != nil {
		return admissionRouteResponse{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fixture.BaseURL+"/v1/workspaces/"+url.PathEscape(fixture.Scope.WorkspaceID)+"/invocations",
		bytes.NewReader(document),
	)
	if err != nil {
		return admissionRouteResponse{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Idempotency-Key", request.Protocol.RequestKey)
	response, err := client.Do(httpRequest)
	if err != nil {
		return admissionRouteResponse{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(body) > 64*1024 {
		return admissionRouteResponse{}, errors.New("admission response exceeded its bounded envelope")
	}
	result := admissionRouteResponse{status: response.StatusCode, document: body}
	if response.StatusCode == http.StatusAccepted {
		err = json.Unmarshal(body, &result.admission)
	} else {
		err = json.Unmarshal(body, &result.problem)
	}
	if err != nil {
		return admissionRouteResponse{}, fmt.Errorf("decode admission route response: %w", err)
	}
	return result, nil
}

func requireAdmissionRoute(
	t *testing.T,
	fixture *testfixture.Fixture,
	client *http.Client,
	request api.InvocationRequest,
) admissionRouteResponse {
	t.Helper()
	response, err := executeAdmissionRoute(fixture, client, request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func requireAcceptedAdmission(t *testing.T, response admissionRouteResponse, replay bool) api.AdmissionResponse {
	t.Helper()
	if response.status != http.StatusAccepted || response.admission.RunRef == "" ||
		response.admission.Replay != replay || response.admission.State != api.AdmissionResponseStateAccepted {
		t.Fatalf("admission response = HTTP %d %#v", response.status, response.admission)
	}
	return response.admission
}

func requireAdmissionProblem(t *testing.T, response admissionRouteResponse, status int, code string) api.Problem {
	t.Helper()
	if response.status != status || response.problem.Status != status || response.problem.Code != code {
		t.Fatalf("admission problem = HTTP %d %#v; want %d/%s", response.status, response.problem, status, code)
	}
	return response.problem
}

func raceAdmissions(
	t *testing.T,
	fixture *testfixture.Fixture,
	left, right api.InvocationRequest,
) []admissionRouteResponse {
	t.Helper()
	clients := []*http.Client{fixture.Client, fixture.ClientFor(t, fixture.Actor)}
	requests := []api.InvocationRequest{left, right}
	type outcome struct {
		response admissionRouteResponse
		err      error
	}
	outcomes := make(chan outcome, 2)
	for index := range requests {
		index := index
		go func() {
			response, err := executeAdmissionRoute(fixture, clients[index], requests[index])
			outcomes <- outcome{response: response, err: err}
		}()
	}
	responses := make([]admissionRouteResponse, 0, 2)
	for range 2 {
		result := <-outcomes
		if result.err != nil {
			t.Fatal(result.err)
		}
		responses = append(responses, result.response)
	}
	return responses
}

func lookupAdmissionRun(
	ctx context.Context,
	fixture *testfixture.Fixture,
	requestKey string,
) (string, error) {
	var runID string
	key := store.AdmissionKey{
		PrincipalID:    fixture.Actor.RequestedActor.PrincipalID,
		OperationID:    "run.accept",
		TargetID:       fixture.Scope.WorkspaceID,
		IdempotencyKey: requestKey,
	}
	err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		result, err := store.LookupAdmission(ctx, tx, key)
		if err == nil {
			runID = result.RunID
		}
		return err
	})
	return runID, err
}

func assertAdmissionRows(t *testing.T, fixture *testfixture.Fixture, runID string) {
	t.Helper()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		counts := make([]int64, 11)
		err := tx.QueryRow(ctx, `
			SELECT
				(SELECT count(*) FROM pm.admission_keys WHERE run_id = $1),
				(SELECT count(*) FROM pm.run_specs WHERE run_id = $1),
				(SELECT count(*) FROM pm.runs WHERE run_id = $1),
				(SELECT count(*) FROM pm.run_execution_grants WHERE run_id = $1),
				(SELECT count(*) FROM pm.run_input_snapshots WHERE run_id = $1),
				(SELECT count(*) FROM pm.run_budget_snapshots WHERE run_id = $1),
				(SELECT count(*) FROM pm.pending_reservations WHERE run_id = $1),
				(SELECT count(*) FROM pm.receipts WHERE run_id = $1 AND kind = 'run_admission'),
				(SELECT count(*) FROM pm.audit_entries WHERE target_ref = $1 AND action = 'run_accepted'),
				(SELECT count(*) FROM pm.server_events WHERE run_id = $1 AND event_type = 'run_accepted'),
				(SELECT count(*) FROM pm.outbox_commands WHERE run_id = $1 AND kind = 'start_run')
		`, runID).Scan(
			&counts[0], &counts[1], &counts[2], &counts[3], &counts[4], &counts[5],
			&counts[6], &counts[7], &counts[8], &counts[9], &counts[10],
		)
		if err != nil {
			return err
		}
		for index, count := range counts {
			if count != 1 {
				return fmt.Errorf("admission durable row count[%d] = %d; want 1", index, count)
			}
		}
		return nil
	})
}

func assertAdmissionAbsent(t *testing.T, fixture *testfixture.Fixture, requestKey string) {
	t.Helper()
	_, err := lookupAdmissionRun(context.Background(), fixture, requestKey)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("precommit refusal admission lookup = %v; want ErrNotFound", err)
	}
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		var keys, runs, outbox int64
		if err := tx.QueryRow(ctx, `
			SELECT
				(SELECT count(*) FROM pm.admission_keys WHERE idempotency_key = $1),
				(SELECT count(*) FROM pm.run_specs),
				(SELECT count(*) FROM pm.outbox_commands WHERE kind = 'start_run')
		`, requestKey).Scan(&keys, &runs, &outbox); err != nil {
			return err
		}
		if keys != 0 || runs != 0 || outbox != 0 {
			return fmt.Errorf("precommit refusal retained keys=%d runs=%d outbox=%d", keys, runs, outbox)
		}
		return nil
	})
}

func assertRunReadable(t *testing.T, fixture *testfixture.Fixture, runID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fixture.BaseURL+"/v1/workspaces/"+url.PathEscape(fixture.Scope.WorkspaceID)+"/runs/"+url.PathEscape(runID),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := fixture.Client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		t.Fatalf("committed run GET = %d: %s", response.StatusCode, body)
	}
}

func configureAdmissionPool(
	dependencies testfixture.ServiceDependencies,
	configuration *httpapi.ServiceConfig,
	pool *store.Pool,
) error {
	registry := &app.HandlerRegistry{}
	if err := registry.RegisterHTTP(externalAdmissionRegistration()); err != nil {
		return err
	}
	admission, err := app.NewAdmissionService(app.AdmissionServiceConfig{
		Pool:             pool,
		DeploymentID:     dependencies.DeploymentID,
		Authorize:        configuration.Auth.AuthorizeResource,
		Registry:         registry,
		ResolveOperation: externalAdmissionResolver(dependencies),
		Policy: store.RunBudgetPolicy{
			Pending: store.AdmissionLimits{
				GlobalPending: 3, WorkspacePending: 2, WorkspaceMaxPendingAge: 30 * time.Second,
			},
			RunLifetime:          time.Hour,
			InputMaxBytes:        1 << 20,
			WorkspaceActiveRuns:  4,
			WorkflowPayloadBytes: 1 << 16,
			WorkflowFanout:       32,
		},
		Random: rand.Reader,
	})
	if err != nil {
		return err
	}
	configuration.Admission = admission
	return nil
}

func externalAdmissionResolver(dependencies testfixture.ServiceDependencies) app.OperationResolver {
	return func(
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
		return externalAdmissionSelection(operationID), nil
	}
}

func externalAdmissionSelection(operationID string) app.OperationSelection {
	foundation := contract.FoundationRef{ID: "FND-6", Version: "1.0.0"}
	sourceRefs := []contract.SourceRef{
		{SourceID: "fixture-source", Pointer: "#/operations/fixture"},
	}
	return app.OperationSelection{
		Operation: contract.Operation{
			SchemaVersion:   contract.OperationSchemaVersion,
			OperationID:     operationID,
			SourceRefs:      sourceRefs,
			Transport:       contract.Transport{Kind: contract.TransportKindHTTP, Method: http.MethodGet, Path: "/fixture"},
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
			LaneEligibility: storeLaneEligibilityForTest(contract.LaneDirectRead),
		},
		SchemaRefs: []string{"fixture-schema-v1"},
	}
}

func storeLaneEligibilityForTest(eligible contract.Lane) []contract.LaneEligibility {
	lanes := []contract.Lane{
		contract.LaneDirectRead,
		contract.LaneDirectWrite,
		contract.LaneETL,
		contract.LaneReverseETL,
		contract.LaneBinaryDownload,
		contract.LaneBinaryUpload,
		contract.LaneSyncTransport,
	}
	eligibility := make([]contract.LaneEligibility, 0, len(lanes))
	for _, lane := range lanes {
		status := contract.LaneUnsupported
		reason := "fixture-lane-unsupported"
		if lane == eligible {
			status = contract.LaneEligible
			reason = "fixture-handler-registered"
		}
		eligibility = append(eligibility, contract.LaneEligibility{Lane: lane, Status: status, Reason: reason})
	}
	return eligibility
}

func externalAdmissionRegistration() app.HandlerRegistration {
	foundation := contract.FoundationRef{ID: "FND-6", Version: "1.0.0"}
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

func installAdmissionRollbackFault(t *testing.T, fixture *testfixture.Fixture) func() {
	t.Helper()
	suffix := strings.ReplaceAll(fixture.DeploymentID, "-", "_")
	functionName := pgx.Identifier{"pm", "fail_admission_" + suffix}.Sanitize()
	triggerName := pgx.Identifier{"fail_admission_" + suffix}.Sanitize()
	workspace := strings.ReplaceAll(fixture.Scope.WorkspaceID, "'", "''")
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `CREATE FUNCTION `+functionName+`() RETURNS trigger
			LANGUAGE plpgsql SECURITY INVOKER AS $$
			BEGIN
				IF NEW.workspace_id = '`+workspace+`' THEN
					RAISE EXCEPTION 'controlled admission rollback' USING ERRCODE = '40001';
				END IF;
				RETURN NEW;
			END
			$$`)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `CREATE TRIGGER `+triggerName+`
			BEFORE INSERT ON pm.outbox_commands
			FOR EACH ROW EXECUTE FUNCTION `+functionName+`()`)
		return err
	})
	return func() {
		fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `DROP TRIGGER `+triggerName+` ON pm.outbox_commands`); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `DROP FUNCTION `+functionName+`()`)
			return err
		})
	}
}

type observingAdmissionTransport struct {
	base    http.RoundTripper
	observe func(*http.Response) error
}

func (transport *observingAdmissionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if err := transport.observe(response); err != nil {
		response.Body.Close()
		return nil, err
	}
	return response, nil
}

type lostAdmissionTransport struct {
	base http.RoundTripper
	err  error
}

func (transport *lostAdmissionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	_, copyErr := io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024+1))
	closeErr := response.Body.Close()
	if copyErr != nil {
		return nil, copyErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return nil, transport.err
}

func wrapAdmissionClient(base *http.Client, transport http.RoundTripper) *http.Client {
	return &http.Client{
		Transport:     transport,
		CheckRedirect: base.CheckRedirect,
		Jar:           base.Jar,
		Timeout:       20 * time.Second,
	}
}
