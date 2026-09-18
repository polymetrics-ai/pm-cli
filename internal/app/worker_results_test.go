package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

// Controlled callback values exercise real worker settlement, PostgreSQL,
// Temporal execution identity and shared-object bytes, not provider HTTP proof.
func TestWorkerResultPublication(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	t.Run("committed bytes survive producer close and duplicate execution", func(t *testing.T) {
		producer := testfixture.OpenArtifacts(t, testfixture.PrimaryArtifactGateway)
		consumer := testfixture.OpenArtifacts(t, testfixture.SecondaryArtifactGateway)
		integer, err := contract.NewIntegerValue("9007199254740993")
		if err != nil {
			t.Fatal(err)
		}
		expected := contract.ActionResult{MediaType: "application/json", Data: contract.NewArrayValue([]contract.PresentValue{
			contract.NewObjectValue(map[string]contract.PresentValue{"exact": integer, "null": contract.NullValue(), "absent": contract.AbsentValue()}),
		})}
		var calls atomic.Int32
		fixture := testfixture.NewExecuting(t, func(_ context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
			calls.Add(1)
			observation := successfulPermitObservation(request)
			observation.Result = &expected
			return observation, nil
		}, unexpectedPermitReconciliation, func(_ testfixture.ServiceDependencies, configuration *httpapi.ServiceConfig) error {
			configuration.Artifacts = consumer.Backend
			return nil
		})
		publisher, err := objects.NewPublisher(fixture.Pool, producer.Backend, producer.Limits)
		if err != nil {
			t.Fatal(err)
		}
		service := workerResultService(t, fixture, temporal, publisher, 4096)
		admitted := admitPermitRun(t, fixture, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID, "worker-result-published")
		run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
		result, err := executePermitWorkerUnit(t, service, run)
		if err != nil {
			t.Fatal(err)
		}
		if result.State != contract.RunStateSucceeded || result.UnknownEffectCount != 0 {
			t.Fatalf("settlement = %#v", result)
		}
		refs := workerResultReferences(t, fixture, admitted.RunRef)
		if len(refs) != 1 {
			t.Fatalf("committed worker result references = %v; want one shared artifact", refs)
		}
		if err := producer.Close(); err != nil {
			t.Fatal(err)
		}
		payload := workerResultGET(t, fixture, "/v1/workspaces/"+fixture.Scope.WorkspaceID+"/artifacts/"+refs[0]+"/content", http.StatusOK)
		var got contract.ActionResult
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatal(err)
		}
		gotBytes, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		wantBytes, err := json.Marshal(expected)
		if err != nil {
			t.Fatal(err)
		}
		if string(gotBytes) != string(wantBytes) {
			t.Fatal("shared result changed exact values or presence after producer close")
		}
		replayed, err := executePermitWorkerUnit(t, service, run)
		if err != nil {
			t.Fatal(err)
		}
		if replayed != result || calls.Load() != 1 || !slices.Equal(workerResultReferences(t, fixture, admitted.RunRef), refs) {
			t.Fatal("duplicate activity changed committed result or executed callback again")
		}
	})

	t.Run("capacity refusal records no physical attempt", func(t *testing.T) {
		producer := testfixture.OpenArtifacts(t, testfixture.PrimaryArtifactGateway)
		var calls atomic.Int32
		fixture := testfixture.NewExecuting(t, func(_ context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
			calls.Add(1)
			return successfulPermitObservation(request), nil
		}, unexpectedPermitReconciliation)
		limits := producer.Limits
		limits.OrganizationAttempts, limits.WorkspaceAttempts = 1, 1
		publisher, err := objects.NewPublisher(fixture.Pool, producer.Backend, limits)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := publisher.Reserve(ctx, fixture.Actor, fixture.Scope, store.ObjectReservationRequest{
			ReservationID: "occupied-slot", ArtifactID: "occupied-artifact", ConnectionID: fixture.Connection.ConnectionID, MaximumBytes: 1,
		}); err != nil {
			t.Fatal(err)
		}
		service := workerResultService(t, fixture, temporal, publisher, 4096)
		admitted := admitPermitRun(t, fixture, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID, "worker-result-capacity")
		run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
		_, executeErr := executePermitWorkerUnit(t, service, run)
		if executeErr == nil || calls.Load() != 0 {
			t.Fatalf("capacity refusal: error=%v callback calls=%d", executeErr, calls.Load())
		}
		if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
			dispatch, err := store.LoadWorkerDispatch(ctx, tx, store.TaskRef{RunID: admitted.RunRef, TaskID: run.plan.TaskID})
			if err != nil {
				return err
			}
			if len(dispatch.Attempts) != 0 || dispatch.State != store.DispatchPrepared {
				return errors.New("storage refusal left a recorded physical attempt")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("oversized result does not erase observed effects", func(t *testing.T) {
		producer := testfixture.OpenArtifacts(t, testfixture.PrimaryArtifactGateway)
		fixture := testfixture.NewExecuting(t, func(_ context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
			observation := successfulPermitObservation(request)
			observation.Result = &contract.ActionResult{MediaType: "application/json", Data: contract.NewStringValue("too large for the reserved result")}
			return observation, nil
		}, unexpectedPermitReconciliation)
		limits := producer.Limits
		limits.OrganizationAttempts, limits.WorkspaceAttempts = 1, 1
		publisher, err := objects.NewPublisher(fixture.Pool, producer.Backend, limits)
		if err != nil {
			t.Fatal(err)
		}
		service := workerResultService(t, fixture, temporal, publisher, 1)
		admitted := admitPermitRun(t, fixture, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID, "worker-result-too-large")
		run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
		result, err := executePermitWorkerUnit(t, service, run)
		if err != nil {
			t.Fatal(err)
		}
		if result.State != contract.RunStateSucceeded || result.UnknownEffectCount != 0 {
			t.Fatal("result failure replaced known effects or exposed incomplete bytes")
		}
		unavailable := workerResultGET(t, fixture, "/v1/workspaces/"+fixture.Scope.WorkspaceID+"/runs/"+admitted.RunRef+"/result", http.StatusServiceUnavailable)
		var problem struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(unavailable, &problem); err != nil || problem.Code != "capability_unavailable" {
			t.Fatal("failed publication did not retain the typed unavailable result response")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := publisher.Reserve(ctx, fixture.Actor, fixture.Scope, store.ObjectReservationRequest{
			ReservationID: "after-oversized-result", ArtifactID: "after-oversized-result",
			ConnectionID: fixture.Connection.ConnectionID, MaximumBytes: 1,
		}); err != nil {
			t.Fatalf("unbound failed result leaked its only capacity slot: %v", err)
		}
	})
}

func workerResultService(t *testing.T, fixture *testfixture.Fixture, temporal *permitTemporalHarness, publisher *objects.Publisher, maximumBytes int64) *app.WorkerService {
	t.Helper()
	service, err := app.NewWorkerService(app.WorkerServiceConfig{
		Pool: fixture.Pool, DeploymentID: fixture.DeploymentID, Registry: fixture.WorkerRegistry,
		ResolveOperation: fixture.ResolveOperation, Authorization: auth.NewResourceAuthorizer(),
		ObserveContinuation: temporal.observer, ActivityTimeouts: temporal.timeouts,
		ArtifactPublisher: publisher, MaximumResultBytes: maximumBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func workerResultReferences(t *testing.T, fixture *testfixture.Fixture, runID string) []string {
	t.Helper()
	payload := workerResultGET(t, fixture, "/v1/workspaces/"+fixture.Scope.WorkspaceID+"/runs/"+runID+"/result", http.StatusOK)
	var result api.RunResultResponse
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatal(err)
	}
	refs := make([]string, len(result.ResultReferences))
	for index, reference := range result.ResultReferences {
		refs[index] = reference.ResultRef
	}
	return refs
}

func workerResultGET(t *testing.T, fixture *testfixture.Fixture, path string, expectedStatus int) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fixture.BaseURL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := fixture.Client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != expectedStatus {
		t.Fatalf("result read status = %d; want %d", response.StatusCode, expectedStatus)
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) > 8192 {
		t.Fatal("fixture result exceeded its finite bound")
	}
	return payload
}
