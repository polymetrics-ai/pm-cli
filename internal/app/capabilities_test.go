package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

func TestRegisteredHandlerReachability(t *testing.T) {
	t.Run("reachable_callbacks", func(t *testing.T) {
		var registry app.HandlerRegistry
		executeCalls := 0
		reconcileCalls := 0
		expectedDeadline := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
		registration := httpRegistrationForTest(
			func(ctx context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
				executeCalls++
				if ctx == nil || request.Scope.OrgID != "org-a" || request.Scope.WorkspaceID != "workspace-a" || request.Deadline != expectedDeadline {
					t.Fatalf("execution request = %#v; want exact in-process scope and deadline", request)
				}
				return observationForRegistryTest("executed"), nil
			},
			func(ctx context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
				reconcileCalls++
				if ctx == nil || request.Scope.WorkspaceID != "workspace-a" {
					t.Fatalf("reconciliation request = %#v; want exact in-process request", request)
				}
				return observationForRegistryTest("reconciled"), nil
			},
		)
		if err := registry.RegisterHTTP(registration); err != nil {
			t.Fatalf("RegisterHTTP: %v", err)
		}
		operation := operationForRegistryTest("http", contract.LaneDirectRead, slices.Clone(registration.Capabilities))
		resolved, err := registry.Resolve(operation, contract.LaneDirectRead)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if resolved.Kind != app.HandlerHTTP || resolved.Lane != contract.LaneDirectRead || !resolved.ReconciliationAvailable {
			t.Fatalf("resolved handler = %#v; want reachable HTTP execution and reconciliation", resolved)
		}
		request := app.ExecutionRequest{
			Scope:    contract.Scope{OrgID: "org-a", WorkspaceID: "workspace-a"},
			Input:    contract.NewObjectValue(map[string]contract.PresentValue{"field-a": contract.NewStringValue("value-a")}),
			Deadline: expectedDeadline,
		}
		executed, err := resolved.Execute(context.Background(), request)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if text, ok := observationTextForRegistryTest(executed); !ok || text != "executed" {
			t.Fatalf("execution observation = %#v; want registered callback result", executed)
		}
		reconciled, err := resolved.Reconcile(context.Background(), request)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if text, ok := observationTextForRegistryTest(reconciled); !ok || text != "reconciled" {
			t.Fatalf("reconciliation observation = %#v; want registered callback result", reconciled)
		}
		if executeCalls != 1 || reconcileCalls != 1 {
			t.Fatalf("callback calls = execute %d, reconcile %d; want one each", executeCalls, reconcileCalls)
		}
	})

	t.Run("sql_reachability_and_removal", func(t *testing.T) {
		var registry app.HandlerRegistry
		executeCalls := 0
		reconcileCalls := 0
		expectedDeadline := time.Date(2026, time.September, 17, 12, 0, 0, 0, time.UTC)
		registration := sqlRegistrationForTest(
			func(ctx context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
				executeCalls++
				if ctx == nil || request.Scope.OrgID != "org-a" || request.Scope.WorkspaceID != "workspace-a" || request.Deadline != expectedDeadline {
					t.Fatalf("SQL execution request = %#v; want exact in-process scope and deadline", request)
				}
				return observationForRegistryTest("applied"), nil
			},
			func(ctx context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
				reconcileCalls++
				if ctx == nil || request.Scope.WorkspaceID != "workspace-a" {
					t.Fatalf("SQL reconciliation request = %#v; want exact in-process request", request)
				}
				return observationForRegistryTest("reconciled"), nil
			},
		)
		if err := registry.RegisterSQL(registration); err != nil {
			t.Fatalf("RegisterSQL: %v", err)
		}
		operation := operationForRegistryTest(contract.TransportKindPostgreSQL, contract.LaneDirectRead, slices.Clone(registration.Capabilities))
		resolved, err := registry.Resolve(operation, contract.LaneDirectRead)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if resolved.Kind != app.HandlerSQL || resolved.Lane != contract.LaneDirectRead || !resolved.ReconciliationAvailable {
			t.Fatalf("resolved SQL handler = %#v; want reachable SQL execution and reconciliation", resolved)
		}
		request := app.ExecutionRequest{
			Scope:    contract.Scope{OrgID: "org-a", WorkspaceID: "workspace-a"},
			Input:    contract.NewObjectValue(map[string]contract.PresentValue{"field-a": contract.NewStringValue("value-a")}),
			Deadline: expectedDeadline,
		}
		executed, err := resolved.Execute(context.Background(), request)
		if err != nil {
			t.Fatalf("SQL Execute: %v", err)
		}
		if text, ok := observationTextForRegistryTest(executed); !ok || text != "applied" {
			t.Fatalf("SQL execution observation = %#v; want registered callback result", executed)
		}
		reconciled, err := resolved.Reconcile(context.Background(), request)
		if err != nil {
			t.Fatalf("SQL Reconcile: %v", err)
		}
		if text, ok := observationTextForRegistryTest(reconciled); !ok || text != "reconciled" {
			t.Fatalf("SQL reconciliation observation = %#v; want registered callback result", reconciled)
		}
		if executeCalls != 1 || reconcileCalls != 1 {
			t.Fatalf("SQL callback calls = execute %d, reconcile %d; want one each", executeCalls, reconcileCalls)
		}

		// Removing the SQL registration leaves the published descriptor unable to
		// authorize a SQL dispatch, so no SQL send is reachable at all.
		var withoutSQL app.HandlerRegistry
		if manifest := app.BuildHandlerManifest(&withoutSQL); manifest.Handlers == nil || len(manifest.Handlers) != 0 {
			t.Fatalf("manifest without registration = %#v; want explicit empty handler list", manifest)
		}
		if _, err := withoutSQL.Resolve(operation, contract.LaneDirectRead); !errors.Is(err, app.ErrHandlerUnavailable) {
			t.Fatalf("SQL Resolve without registration error = %v; want handler unavailable", err)
		} else {
			var failure *app.HandlerError
			if !errors.As(err, &failure) || failure.Lane != contract.LaneDirectRead || failure.Handler != "" {
				t.Fatalf("SQL Resolve without registration error = %#v; want typed direct-read HandlerError naming no registered handler", err)
			}
		}

		// The same published descriptor cannot authorize an HTTP dispatch while
		// only the SQL handler is registered, and no provider call is reachable.
		httpOperation := operationForRegistryTest(contract.TransportKindHTTP, contract.LaneDirectRead, slices.Clone(registration.Capabilities))
		if _, err := registry.Resolve(httpOperation, contract.LaneDirectRead); !errors.Is(err, app.ErrHandlerUnavailable) {
			t.Fatalf("HTTP Resolve with only SQL registered error = %v; want handler unavailable", err)
		}
	})

	t.Run("missing_registration_and_reconciliation", func(t *testing.T) {
		registration := httpRegistrationForTest(controlledCallbackForRegistryTest("executed"), nil)
		operation := operationForRegistryTest("http", contract.LaneDirectRead, slices.Clone(registration.Capabilities))
		var empty app.HandlerRegistry
		emptyManifest := app.BuildHandlerManifest(&empty)
		if emptyManifest.Handlers == nil || len(emptyManifest.Handlers) != 0 {
			t.Fatalf("empty manifest = %#v; want explicit empty handler list", emptyManifest)
		}
		if _, err := empty.Resolve(operation, contract.LaneDirectRead); !errors.Is(err, app.ErrHandlerUnavailable) {
			t.Fatalf("Resolve without registration error = %v; want handler unavailable", err)
		} else {
			var failure *app.HandlerError
			if !errors.As(err, &failure) || failure.Lane != contract.LaneDirectRead {
				t.Fatalf("Resolve without registration error = %#v; want typed direct-read HandlerError", err)
			}
		}

		var registered app.HandlerRegistry
		if err := registered.RegisterHTTP(registration); err != nil {
			t.Fatalf("RegisterHTTP: %v", err)
		}
		manifest := app.BuildHandlerManifest(&registered)
		if len(manifest.Handlers) != 1 || manifest.Handlers[0].ReconciliationAvailable {
			t.Fatalf("manifest = %#v; want executable handler with reconciliation explicitly unavailable", manifest)
		}
		resolved, err := registered.Resolve(operation, contract.LaneDirectRead)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if resolved.ReconciliationAvailable {
			t.Fatal("resolved handler reports absent reconciliation as available")
		}
		if _, err := resolved.Reconcile(context.Background(), app.ExecutionRequest{}); !errors.Is(err, app.ErrReconciliationUnavailable) {
			t.Fatalf("Reconcile without callback error = %v; want reconciliation unavailable", err)
		}

		var registrationRemoved app.HandlerRegistry
		if _, err := registrationRemoved.Resolve(operation, contract.LaneDirectRead); !errors.Is(err, app.ErrHandlerUnavailable) {
			t.Fatalf("Resolve after registration removal error = %v; want handler unavailable", err)
		}
	})

	t.Run("foundation_capability_and_lane_mismatch", func(t *testing.T) {
		var registry app.HandlerRegistry
		registration := httpRegistrationForTest(controlledCallbackForRegistryTest("executed"), nil)
		if err := registry.RegisterHTTP(registration); err != nil {
			t.Fatalf("RegisterHTTP: %v", err)
		}

		foundationMismatch := operationForRegistryTest("http", contract.LaneDirectRead, slices.Clone(registration.Capabilities))
		for index := range foundationMismatch.CapabilityRequirements {
			if foundationMismatch.CapabilityRequirements[index].Capability == "http.execute" {
				foundationMismatch.CapabilityRequirements[index].Foundation.Version = "2.0.0"
			}
		}
		if _, err := registry.Resolve(foundationMismatch, contract.LaneDirectRead); !errors.Is(err, app.ErrHandlerIncompatible) || !errors.Is(err, contract.ErrFoundationIncompatible) {
			t.Fatalf("foundation mismatch error = %v; want handler and foundation incompatibility", err)
		}

		capabilityMismatch := operationForRegistryTest(contract.TransportKindHTTP, contract.LaneDirectRead, slices.Clone(registration.Capabilities))
		capabilityMismatch.CapabilityRequirements = append(capabilityMismatch.CapabilityRequirements, contract.CapabilityRequirement{
			Capability: "pagination",
			Foundation: contract.FoundationRef{ID: "FND-6", Version: "1.0.0"},
		})
		if _, err := registry.Resolve(capabilityMismatch, contract.LaneDirectRead); !errors.Is(err, app.ErrHandlerIncompatible) {
			t.Fatalf("capability mismatch error = %v; want handler incompatible", err)
		}

		for _, status := range []contract.LaneStatus{
			contract.LaneUnsupported,
			contract.LaneNotApplicable,
			contract.LaneBlocked,
			contract.LaneUnproven,
		} {
			ineligibleLane := operationForRegistryTest(contract.TransportKindHTTP, contract.LaneDirectRead, slices.Clone(registration.Capabilities))
			ineligibleLane.LaneEligibility[0] = contract.LaneEligibility{
				Lane: contract.LaneDirectRead, Status: status, Reason: "not-executable",
			}
			if _, err := registry.Resolve(ineligibleLane, contract.LaneDirectRead); !errors.Is(err, app.ErrHandlerUnavailable) {
				t.Fatalf("lane status %q error = %v; want handler unavailable", status, err)
			}
		}

		if _, err := registry.Resolve(operationForRegistryTest(contract.TransportKindHTTP, contract.LaneDirectRead, slices.Clone(registration.Capabilities)), contract.LaneETL); !errors.Is(err, app.ErrHandlerUnavailable) {
			t.Fatalf("undeclared lane error = %v; want handler unavailable", err)
		}

		postgresOperation := operationForRegistryTest(contract.TransportKindPostgreSQL, contract.LaneDirectRead, slices.Clone(registration.Capabilities))
		if _, err := registry.Resolve(postgresOperation, contract.LaneDirectRead); !errors.Is(err, app.ErrHandlerUnavailable) {
			t.Fatalf("transport-specific handler mismatch error = %v; want handler unavailable", err)
		}
	})

	t.Run("duplicate_and_ambiguous_registration", func(t *testing.T) {
		registration := httpRegistrationForTest(controlledCallbackForRegistryTest("executed"), nil)
		var registry app.HandlerRegistry
		if err := registry.RegisterHTTP(registration); err != nil {
			t.Fatalf("RegisterHTTP: %v", err)
		}
		if err := registry.RegisterHTTP(registration); !errors.Is(err, app.ErrHandlerConflict) {
			t.Fatalf("duplicate RegisterHTTP error = %v; want handler conflict", err)
		}
		if err := registry.RegisterETL(registration); !errors.Is(err, app.ErrHandlerConflict) {
			t.Fatalf("overlapping RegisterETL error = %v; want ambiguous registration rejected", err)
		}

		var invalid app.HandlerRegistry
		missingCallback := registration
		missingCallback.Execute = nil
		if err := invalid.RegisterHTTP(missingCallback); !errors.Is(err, app.ErrHandlerInvalid) {
			t.Fatalf("missing callback error = %v; want invalid handler", err)
		}
		duplicateLane := registration
		duplicateLane.Lanes = []contract.Lane{contract.LaneDirectRead, contract.LaneDirectRead}
		if err := invalid.RegisterHTTP(duplicateLane); !errors.Is(err, app.ErrHandlerInvalid) {
			t.Fatalf("duplicate lane error = %v; want invalid handler", err)
		}
		duplicateCapability := registration
		duplicateCapability.Capabilities = append(duplicateCapability.Capabilities, duplicateCapability.Capabilities[0])
		if err := invalid.RegisterHTTP(duplicateCapability); !errors.Is(err, app.ErrHandlerInvalid) {
			t.Fatalf("duplicate capability error = %v; want invalid handler", err)
		}
	})

	t.Run("deterministic_manifest", func(t *testing.T) {
		controlled := controlledCallbackForRegistryTest("controlled")
		reconciliation := controlledCallbackForRegistryTest("reconciled")
		httpA := httpRegistrationForTest(controlled, nil)
		httpA.Lanes = []contract.Lane{contract.LaneDirectWrite, contract.LaneDirectRead}
		sqlA := sqlRegistrationForTest(controlled, reconciliation)
		extensionsA := extensionRegistrationsForTest(controlled)
		var first app.HandlerRegistry
		if err := first.RegisterBinary(extensionsA[3]); err != nil {
			t.Fatalf("RegisterBinary: %v", err)
		}
		if err := first.RegisterReverse(extensionsA[2]); err != nil {
			t.Fatalf("RegisterReverse: %v", err)
		}
		if err := first.RegisterApply(extensionsA[1]); err != nil {
			t.Fatalf("RegisterApply: %v", err)
		}
		if err := first.RegisterETL(extensionsA[0]); err != nil {
			t.Fatalf("RegisterETL: %v", err)
		}
		if err := first.RegisterSQL(sqlA); err != nil {
			t.Fatalf("RegisterSQL: %v", err)
		}
		if err := first.RegisterHTTP(httpA); err != nil {
			t.Fatalf("RegisterHTTP: %v", err)
		}
		httpA.Lanes[0] = contract.LaneETL
		httpA.Capabilities[0].Capability = "mutated-after-registration"
		httpA.Foundations[0].ID = "FND-999"

		httpB := httpRegistrationForTest(controlled, nil)
		httpB.Lanes = []contract.Lane{contract.LaneDirectRead, contract.LaneDirectWrite}
		slices.Reverse(httpB.Capabilities)
		slices.Reverse(httpB.Foundations)
		sqlB := sqlRegistrationForTest(controlled, reconciliation)
		slices.Reverse(sqlB.Lanes)
		slices.Reverse(sqlB.Capabilities)
		slices.Reverse(sqlB.Foundations)
		extensionsB := extensionRegistrationsForTest(controlled)
		var second app.HandlerRegistry
		if err := second.RegisterHTTP(httpB); err != nil {
			t.Fatalf("second RegisterHTTP: %v", err)
		}
		if err := second.RegisterSQL(sqlB); err != nil {
			t.Fatalf("second RegisterSQL: %v", err)
		}
		if err := second.RegisterETL(extensionsB[0]); err != nil {
			t.Fatalf("second RegisterETL: %v", err)
		}
		if err := second.RegisterApply(extensionsB[1]); err != nil {
			t.Fatalf("second RegisterApply: %v", err)
		}
		if err := second.RegisterReverse(extensionsB[2]); err != nil {
			t.Fatalf("second RegisterReverse: %v", err)
		}
		if err := second.RegisterBinary(extensionsB[3]); err != nil {
			t.Fatalf("second RegisterBinary: %v", err)
		}

		firstManifest := app.BuildHandlerManifest(&first)
		secondManifest := app.BuildHandlerManifest(&second)
		if !reflect.DeepEqual(firstManifest, secondManifest) {
			t.Fatalf("manifests differ by registration/input order:\nfirst: %#v\nsecond: %#v", firstManifest, secondManifest)
		}
		if !reflect.DeepEqual(firstManifest, app.BuildHandlerManifest(&first)) {
			t.Fatal("repeated manifest generation changed consumer-visible output")
		}
		wantKinds := []app.HandlerKind{app.HandlerHTTP, app.HandlerSQL, app.HandlerETL, app.HandlerApply, app.HandlerReverse, app.HandlerBinary}
		if len(firstManifest.Handlers) != len(wantKinds) {
			t.Fatalf("manifest handlers = %#v; want all six registered executor classes", firstManifest.Handlers)
		}
		for index, wantKind := range wantKinds {
			if firstManifest.Handlers[index].Kind != wantKind {
				t.Fatalf("manifest handler %d kind = %q; want %q", index, firstManifest.Handlers[index].Kind, wantKind)
			}
		}
		if !reflect.DeepEqual(firstManifest.Handlers[0].Lanes, []contract.Lane{contract.LaneDirectRead, contract.LaneDirectWrite}) {
			t.Fatalf("HTTP manifest lanes = %#v; want canonical direct-read/direct-write order", firstManifest.Handlers[0].Lanes)
		}
		if firstManifest.Handlers[0].Foundations[0].ID != "FND-3" || firstManifest.Handlers[0].Capabilities[0].Capability != "http.execute" {
			t.Fatalf("HTTP manifest metadata = %#v; want canonical immutable foundation/capability order", firstManifest.Handlers[0])
		}
		if !firstManifest.Handlers[1].ReconciliationAvailable {
			t.Fatal("SQL manifest omitted its registered reconciliation callback")
		}
	})
}

func TestInitialAdmissionAndRegistry(t *testing.T) {
	exerciseInitialUnavailableHandler(t)
	exerciseInitialAuthority(t)
	exerciseInitialPendingBudget(t)
	exerciseInitialRetryReceipts(t)
	exerciseInitialRunViews(t)
	exerciseInitialRunCancel(t)
	exerciseInitialRunResume(t)
	exerciseInitialRunCancelReceipts(t)
	exerciseInitialRunCancelPreconditions(t)
}

type admittedRunForTest struct {
	invocation api.InvocationRequest
	response   api.AdmissionResponse
}

type runRouteResponse struct {
	status   int
	header   http.Header
	document []byte
}

type runControlCounts struct {
	commands     int64
	receipts     int64
	audits       int64
	events       int64
	mutationKeys int64
	pending      int64
}

func exerciseInitialRunViews(t *testing.T) {
	t.Helper()
	t.Run("scoped_views", func(t *testing.T) {
		fixture := testfixture.NewAdmitting(t)
		admitted := admitRunForTest(t, fixture, "run-view-admission")
		revokeAcceptedExecutionGrant(t, fixture, admitted.response.RunRef)
		createRunViewTask(t, fixture, admitted.response.RunRef, "view-task")
		runPath := runPathForTest(fixture, admitted.response.RunRef)
		tasksPath := runPath + "/tasks?page_size=10"
		var taskWireID string

		t.Run("C06_authorized_views", func(t *testing.T) {
			runResponse := sendRunRoute(t, fixture.Client, http.MethodGet, runPath, nil, nil)
			if runResponse.status != http.StatusOK {
				t.Fatalf("authorized GetRun status = %d, want 200: %s", runResponse.status, runResponse.document)
			}
			run := decodeRunRoute[api.RunView](t, runResponse)
			if run.RunRef != admitted.response.RunRef ||
				!slices.Equal(run.ConnectionRefs, []string{admitted.invocation.ConnectionRef}) ||
				run.Intent.Id != admitted.invocation.OperationRef ||
				run.Intent.Revision != admitted.invocation.Protocol.ExpectedRevision ||
				string(run.State) != string(contract.RunStateAccepted) ||
				run.CancelRequested ||
				len(run.ResultReferences) != 0 {
				t.Fatalf("authorized run view = %#v; want exact lean accepted identity", run)
			}
			for _, forbidden := range []string{
				`"spec"`,
				`"execution_grant_ref"`,
				`"input_refs"`,
				`"credential_ref"`,
				`"grant_record"`,
			} {
				if bytes.Contains(runResponse.document, []byte(forbidden)) {
					t.Fatalf("authorized run view exposed forbidden field %s", forbidden)
				}
			}

			listResponse := sendRunRoute(
				t,
				fixture.Client,
				http.MethodGet,
				fixture.BaseURL+"/v1/workspaces/"+url.PathEscape(fixture.Scope.WorkspaceID)+"/runs?page_size=10",
				nil,
				nil,
			)
			if listResponse.status != http.StatusOK {
				t.Fatalf("authorized ListRuns status = %d, want 200: %s", listResponse.status, listResponse.document)
			}
			page := decodeRunRoute[api.RunPage](t, listResponse)
			if len(page.Items) != 1 || page.Items[0].RunRef != admitted.response.RunRef {
				t.Fatalf("authorized run page = %#v; want only admitted run", page.Items)
			}

			tasksResponse := sendRunRoute(t, fixture.Client, http.MethodGet, tasksPath, nil, nil)
			if tasksResponse.status != http.StatusOK {
				t.Fatalf("authorized ListRunTasks status = %d, want 200: %s", tasksResponse.status, tasksResponse.document)
			}
			tasks := decodeRunRoute[api.TaskPage](t, tasksResponse)
			if len(tasks.Items) != 1 || tasks.Items[0].RunRef != admitted.response.RunRef ||
				tasks.Items[0].OperationId != fixture.OperationID ||
				!strings.HasPrefix(tasks.Items[0].TaskId, "task1_") {
				t.Fatalf("authorized task page = %#v; want exact encoded task identity", tasks.Items)
			}
			taskWireID = tasks.Items[0].TaskId
			taskPath := fixture.BaseURL + "/v1/workspaces/" + url.PathEscape(fixture.Scope.WorkspaceID) +
				"/tasks/" + url.PathEscape(taskWireID)
			taskResponse := sendRunRoute(t, fixture.Client, http.MethodGet, taskPath, nil, nil)
			if taskResponse.status != http.StatusOK {
				t.Fatalf("authorized GetTask status = %d, want 200: %s", taskResponse.status, taskResponse.document)
			}
			task := decodeRunRoute[api.TaskView](t, taskResponse)
			if task.TaskId != taskWireID || task.RunRef != admitted.response.RunRef || len(task.MemberOutcomes) != 0 {
				t.Fatalf("authorized task view = %#v; want listed composite identity", task)
			}
		})

		t.Run("C05_unavailable_result", func(t *testing.T) {
			resultResponse := sendRunRoute(t, fixture.Client, http.MethodGet, runPath+"/result", nil, nil)
			requireRunProblem(t, resultResponse, http.StatusServiceUnavailable, "capability_unavailable")
		})

		foreignClient := fixture.ClientFor(t, fixture.ForeignActor)
		assertConcealed := func(t *testing.T, path string) {
			t.Helper()
			response := sendRunRoute(t, foreignClient, http.MethodGet, path, nil, nil)
			requireRunProblem(t, response, http.StatusNotFound, "resource_not_found")
			for _, secret := range []string{
				admitted.response.RunRef,
				taskWireID,
				admitted.invocation.ConnectionRef,
				string(contract.RunStateAccepted),
			} {
				if secret != "" && bytes.Contains(response.document, []byte(secret)) {
					t.Fatalf("foreign run response disclosed %q: %s", secret, response.document)
				}
			}
		}
		t.Run("C01_foreign_list", func(t *testing.T) {
			assertConcealed(
				t,
				fixture.BaseURL+"/v1/workspaces/"+url.PathEscape(fixture.Scope.WorkspaceID)+"/runs?page_size=10",
			)
		})
		t.Run("C02_foreign_inspect", func(t *testing.T) {
			assertConcealed(t, runPath)
		})
		t.Run("C03_foreign_result", func(t *testing.T) {
			assertConcealed(t, runPath+"/result")
		})
		t.Run("C04_foreign_tasks", func(t *testing.T) {
			if taskWireID == "" {
				t.Fatal("authorized task case did not provide a returned composite task identity")
			}
			assertConcealed(t, tasksPath)
			assertConcealed(
				t,
				fixture.BaseURL+"/v1/workspaces/"+url.PathEscape(fixture.Scope.WorkspaceID)+
					"/tasks/"+url.PathEscape(taskWireID),
			)
		})
	})
}

func exerciseInitialRunCancel(t *testing.T) {
	t.Helper()
	t.Run("control_intent", func(t *testing.T) {
		t.Run("C01_authorized_cancel", func(t *testing.T) {
			fixture := testfixture.NewAdmitting(t)
			admitted := admitRunForTest(t, fixture, "cancel-durable-admission")
			before := getRunViewForTest(t, fixture.Client, runPathForTest(fixture, admitted.response.RunRef))
			response := sendRunCancel(
				t,
				fixture,
				fixture.Client,
				admitted.response.RunRef,
				"cancel-durable-key",
				before.Revision,
				true,
			)
			if response.status != http.StatusAccepted {
				t.Fatalf("CancelRun status = %d, want 202: %s", response.status, response.document)
			}
			cancelled := decodeRunRoute[api.RunCancelResponse](t, response)
			if cancelled.RunRef != admitted.response.RunRef || cancelled.CommandRef == "" ||
				!bool(cancelled.CancelRequested) || cancelled.Replay {
				t.Fatalf("fresh cancellation response = %#v", cancelled)
			}
			after := getRunViewForTest(t, fixture.Client, runPathForTest(fixture, admitted.response.RunRef))
			beforeRevision, err := strconv.ParseUint(before.Revision, 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			if after.Revision != strconv.FormatUint(beforeRevision+1, 10) ||
				cancelled.ResultRevision != after.Revision ||
				!after.CancelRequested ||
				after.State != before.State ||
				after.OrchestrationCondition != before.OrchestrationCondition ||
				!reflect.DeepEqual(after.EffectSummary, before.EffectSummary) ||
				after.UnknownEffectCount != before.UnknownEffectCount {
				t.Fatalf("durable cancel projection before=%#v after=%#v response=%#v", before, after, cancelled)
			}
			requireRunControlCounts(t, fixture, admitted.response.RunRef, runControlCounts{
				commands: 1, receipts: 1, audits: 1, events: 1, mutationKeys: 1, pending: 1,
			})
		})

		t.Run("C04_expired_authority", func(t *testing.T) {
			fixture := testfixture.NewAdmitting(t)
			admitted := admitRunForTest(t, fixture, "cancel-authority-admission")
			before := getRunViewForTest(t, fixture.Client, runPathForTest(fixture, admitted.response.RunRef))
			first := sendRunCancel(
				t,
				fixture,
				fixture.Client,
				admitted.response.RunRef,
				"cancel-authority-key",
				before.Revision,
				true,
			)
			if first.status != http.StatusAccepted {
				t.Fatalf("initial cancellation status = %d, want 202: %s", first.status, first.document)
			}
			firstResult := decodeRunRoute[api.RunCancelResponse](t, first)
			expirePrimaryMembership(t, fixture)

			replay := sendRunCancel(
				t,
				fixture,
				fixture.Client,
				admitted.response.RunRef,
				"cancel-authority-key",
				before.Revision,
				false,
			)
			requireRunProblem(t, replay, http.StatusForbidden, "grant_unavailable")
			fresh := sendRunCancel(
				t,
				fixture,
				fixture.Client,
				admitted.response.RunRef,
				"cancel-authority-fresh-key",
				firstResult.ResultRevision,
				true,
			)
			requireRunProblem(t, fresh, http.StatusForbidden, "grant_unavailable")
			requireRunControlCounts(t, fixture, admitted.response.RunRef, runControlCounts{
				commands: 1, receipts: 1, audits: 1, events: 1, mutationKeys: 1, pending: 1,
			})
		})

		t.Run("C03_foreign_actor", func(t *testing.T) {
			fixture := testfixture.NewAdmitting(t)
			admitted := admitRunForTest(t, fixture, "cancel-foreign-admission")
			before := getRunViewForTest(t, fixture.Client, runPathForTest(fixture, admitted.response.RunRef))
			response := sendRunCancel(
				t,
				fixture,
				fixture.ClientFor(t, fixture.ForeignActor),
				admitted.response.RunRef,
				"cancel-foreign-key",
				before.Revision,
				true,
			)
			requireRunProblem(t, response, http.StatusNotFound, "resource_not_found")
			requireRunControlCounts(t, fixture, admitted.response.RunRef, runControlCounts{pending: 1})
		})

		t.Run("C02_rollback", func(t *testing.T) {
			fixture := testfixture.NewAdmitting(t)
			admitted := admitRunForTest(t, fixture, "cancel-rollback-admission")
			runPath := runPathForTest(fixture, admitted.response.RunRef)
			before := getRunViewForTest(t, fixture.Client, runPath)
			removeFailure := installRunCancelOutboxFailure(t, fixture, admitted.response.RunRef)
			response := sendRunCancel(
				t,
				fixture,
				fixture.Client,
				admitted.response.RunRef,
				"cancel-rollback-key",
				before.Revision,
				true,
			)
			removeFailure()
			problem := requireRunProblem(t, response, http.StatusServiceUnavailable, "control_store_unavailable")
			if problem.SafeRetryAction != "same_key_after_dependency_recovery" {
				t.Fatalf("control rollback retry action = %q", problem.SafeRetryAction)
			}
			after := getRunViewForTest(t, fixture.Client, runPath)
			if after.Revision != before.Revision || after.CancelRequested || after.State != before.State {
				t.Fatalf("failed cancellation changed run before=%#v after=%#v", before, after)
			}
			requireRunControlCounts(t, fixture, admitted.response.RunRef, runControlCounts{pending: 1})
		})
	})
}

func exerciseInitialRunResume(t *testing.T) {
	t.Helper()
	t.Run("resume_unavailable", func(t *testing.T) {
		t.Run("C01_missing_resume", func(t *testing.T) {
			fixture := testfixture.NewAdmitting(t)
			admitted := admitRunForTest(t, fixture, "resume-unavailable-admission")
			runPath := runPathForTest(fixture, admitted.response.RunRef)
			before := getRunViewForTest(t, fixture.Client, runPath)
			response := sendRunResume(
				t,
				fixture,
				fixture.Client,
				admitted.response.RunRef,
				"resume-unavailable-key",
				before.Revision,
				true,
			)
			requireRunProblem(t, response, http.StatusConflict, "resume_unavailable")
			after := getRunViewForTest(t, fixture.Client, runPath)
			if after.Revision != before.Revision || after.CancelRequested != before.CancelRequested || after.State != before.State {
				t.Fatalf("unavailable resume changed run before=%#v after=%#v", before, after)
			}
			requireRunControlCounts(t, fixture, admitted.response.RunRef, runControlCounts{pending: 1})
		})
	})
}

func exerciseInitialRunCancelReceipts(t *testing.T) {
	t.Helper()
	t.Run("cancel_replay", func(t *testing.T) {
		fixture := testfixture.NewAdmitting(t)
		admitted := admitRunForTest(t, fixture, "cancel-replay-admission")
		before := getRunViewForTest(t, fixture.Client, runPathForTest(fixture, admitted.response.RunRef))
		var first api.RunCancelResponse

		t.Run("C01_same_request", func(t *testing.T) {
			firstResponse := sendRunCancel(
				t,
				fixture,
				fixture.Client,
				admitted.response.RunRef,
				"cancel-replay-key",
				before.Revision,
				true,
			)
			if firstResponse.status != http.StatusAccepted {
				t.Fatalf("fresh cancel status = %d, want 202: %s", firstResponse.status, firstResponse.document)
			}
			first = decodeRunRoute[api.RunCancelResponse](t, firstResponse)

			replayResponse := sendRunCancel(
				t,
				fixture,
				fixture.Client,
				admitted.response.RunRef,
				"cancel-replay-key",
				before.Revision,
				false,
			)
			if replayResponse.status != http.StatusAccepted {
				t.Fatalf("exact cancel replay status = %d, want 202: %s", replayResponse.status, replayResponse.document)
			}
			replay := decodeRunRoute[api.RunCancelResponse](t, replayResponse)
			if !replay.Replay || replay.CommandRef != first.CommandRef ||
				replay.RunRef != first.RunRef || replay.ResultRevision != first.ResultRevision {
				t.Fatalf("exact replay = %#v; want original %#v with replay=true", replay, first)
			}
			requireRunControlCounts(t, fixture, admitted.response.RunRef, runControlCounts{
				commands: 1, receipts: 1, audits: 1, events: 1, mutationKeys: 1, pending: 1,
			})
		})

		t.Run("C02_changed_fingerprint", func(t *testing.T) {
			if first.ResultRevision == "" {
				t.Fatal("same-request case did not produce an original durable response")
			}
			changed := sendRunCancel(
				t,
				fixture,
				fixture.Client,
				admitted.response.RunRef,
				"cancel-replay-key",
				first.ResultRevision,
				true,
			)
			requireRunProblem(t, changed, http.StatusConflict, "idempotency_conflict")
			requireRunControlCounts(t, fixture, admitted.response.RunRef, runControlCounts{
				commands: 1, receipts: 1, audits: 1, events: 1, mutationKeys: 1, pending: 1,
			})
		})

		t.Run("C03_expired_response_references_run", func(t *testing.T) {
			if first.CommandRef == "" {
				t.Fatal("same-request case did not produce an original durable command")
			}
			expireAndCompactRunCancelReceipt(t, fixture, admitted.response.RunRef)
			expired := sendRunCancel(
				t,
				fixture,
				fixture.Client,
				admitted.response.RunRef,
				"cancel-replay-key",
				before.Revision,
				false,
			)
			problem := requireRunProblem(t, expired, http.StatusGone, "retry_receipt_expired")
			if problem.OutcomeReference == nil ||
				problem.OutcomeReference.Kind != api.SafeOutcomeReferenceKindRun ||
				problem.OutcomeReference.Ref != admitted.response.RunRef ||
				problem.OutcomeReference.Ref == first.CommandRef {
				t.Fatalf("expired cancellation outcome reference = %#v; want admitted run %q", problem.OutcomeReference, admitted.response.RunRef)
			}
			requireRunControlCounts(t, fixture, admitted.response.RunRef, runControlCounts{
				commands: 1, receipts: 1, audits: 1, events: 1, mutationKeys: 1, pending: 1,
			})
		})
	})
}

func exerciseInitialRunCancelPreconditions(t *testing.T) {
	t.Helper()
	t.Run("stale_control_revision", func(t *testing.T) {
		fixture := testfixture.NewAdmitting(t)
		admitted := admitRunForTest(t, fixture, "cancel-precondition-admission")
		runPath := runPathForTest(fixture, admitted.response.RunRef)
		before := getRunViewForTest(t, fixture.Client, runPath)
		firstResponse := sendRunCancel(
			t,
			fixture,
			fixture.Client,
			admitted.response.RunRef,
			"cancel-precondition-initial-key",
			before.Revision,
			true,
		)
		if firstResponse.status != http.StatusAccepted {
			t.Fatalf("initial cancel status = %d, want 202: %s", firstResponse.status, firstResponse.document)
		}
		first := decodeRunRoute[api.RunCancelResponse](t, firstResponse)

		t.Run("C01_stale_revision", func(t *testing.T) {
			stale := sendRunCancel(
				t,
				fixture,
				fixture.Client,
				admitted.response.RunRef,
				"cancel-precondition-stale-key",
				before.Revision,
				true,
			)
			requireRunProblem(t, stale, http.StatusPreconditionFailed, "control_revision_mismatch")
			requireRunControlCounts(t, fixture, admitted.response.RunRef, runControlCounts{
				commands: 1, receipts: 1, audits: 1, events: 1, mutationKeys: 1, pending: 1,
			})
		})
		t.Run("C02_missing_revision", func(t *testing.T) {
			missing := sendRunCancel(
				t,
				fixture,
				fixture.Client,
				admitted.response.RunRef,
				"cancel-precondition-missing-key",
				first.ResultRevision,
				false,
			)
			requireRunProblem(t, missing, http.StatusPreconditionRequired, "control_revision_required")
			after := getRunViewForTest(t, fixture.Client, runPath)
			if after.Revision != first.ResultRevision || !after.CancelRequested {
				t.Fatalf("precondition failures changed durable cancellation: %#v", after)
			}
			requireRunControlCounts(t, fixture, admitted.response.RunRef, runControlCounts{
				commands: 1, receipts: 1, audits: 1, events: 1, mutationKeys: 1, pending: 1,
			})
		})
	})
}

func admitRunForTest(t *testing.T, fixture *testfixture.Fixture, requestKey string) admittedRunForTest {
	t.Helper()
	invocation := testfixture.Invocation(t, fixture, requestKey)
	response := sendRunRoute(
		t,
		fixture.Client,
		http.MethodPost,
		fixture.BaseURL+"/v1/workspaces/"+url.PathEscape(fixture.Scope.WorkspaceID)+"/invocations",
		invocation,
		map[string]string{"Idempotency-Key": requestKey},
	)
	if response.status != http.StatusAccepted {
		t.Fatalf("admission status = %d, want 202: %s", response.status, response.document)
	}
	accepted := decodeRunRoute[api.AdmissionResponse](t, response)
	if accepted.RunRef == "" || accepted.Replay || accepted.RequestKey != requestKey ||
		string(accepted.State) != "accepted" {
		t.Fatalf("admission response = %#v", accepted)
	}
	return admittedRunForTest{invocation: invocation, response: accepted}
}

func sendRunCancel(
	t *testing.T,
	fixture *testfixture.Fixture,
	client *http.Client,
	runID, requestKey, expectedRevision string,
	includeIfMatch bool,
) runRouteResponse {
	t.Helper()
	headers := map[string]string{"Idempotency-Key": requestKey}
	if includeIfMatch {
		headers["If-Match"] = expectedRevision
	}
	return sendRunRoute(
		t,
		client,
		http.MethodPost,
		runPathForTest(fixture, runID)+"/cancel",
		api.RunCancelRequest{Protocol: runControlProtocol(requestKey, expectedRevision)},
		headers,
	)
}

func sendRunResume(
	t *testing.T,
	fixture *testfixture.Fixture,
	client *http.Client,
	runID, requestKey, expectedRevision string,
	includeIfMatch bool,
) runRouteResponse {
	t.Helper()
	headers := map[string]string{"Idempotency-Key": requestKey}
	if includeIfMatch {
		headers["If-Match"] = expectedRevision
	}
	body := map[string]any{
		"approval_ref":        nil,
		"protocol":            runControlProtocol(requestKey, expectedRevision),
		"recovery_generation": "recovery-generation-1",
	}
	return sendRunRoute(t, client, http.MethodPost, runPathForTest(fixture, runID)+"/resume", body, headers)
}

func runControlProtocol(requestKey, expectedRevision string) api.ProtocolMetadata {
	return api.ProtocolMetadata{
		WireVersion:      api.ProtocolMetadataWireVersion(api.WireVersion),
		RequiredFeatures: []string{},
		RequestKey:       requestKey,
		IntentDigest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		ExpectedRevision: expectedRevision,
	}
}

func sendRunRoute(
	t *testing.T,
	client *http.Client,
	method, endpoint string,
	body any,
	headers map[string]string,
) runRouteResponse {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal("encode run route request:", err)
		}
		reader = bytes.NewReader(encoded)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		t.Fatal("construct run route request:", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal("send run route request:", err)
	}
	defer response.Body.Close()
	document, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(document) > 64*1024 {
		t.Fatal("run route response exceeded its bounded readable envelope")
	}
	return runRouteResponse{status: response.StatusCode, header: response.Header.Clone(), document: document}
}

func decodeRunRoute[T any](t *testing.T, response runRouteResponse) T {
	t.Helper()
	var decoded T
	if err := json.Unmarshal(response.document, &decoded); err != nil {
		t.Fatalf("decode run route response: %v: %s", err, response.document)
	}
	return decoded
}

func requireRunProblem(t *testing.T, response runRouteResponse, status int, code string) api.Problem {
	t.Helper()
	problem := decodeRunRoute[api.Problem](t, response)
	if response.status != status || problem.Status != status || problem.Code != code {
		t.Fatalf("run problem = HTTP %d %#v; want %d/%s", response.status, problem, status, code)
	}
	return problem
}

func getRunViewForTest(t *testing.T, client *http.Client, path string) api.RunView {
	t.Helper()
	response := sendRunRoute(t, client, http.MethodGet, path, nil, nil)
	if response.status != http.StatusOK {
		t.Fatalf("GetRun status = %d, want 200: %s", response.status, response.document)
	}
	return decodeRunRoute[api.RunView](t, response)
}

func runPathForTest(fixture *testfixture.Fixture, runID string) string {
	return fixture.BaseURL + "/v1/workspaces/" + url.PathEscape(fixture.Scope.WorkspaceID) +
		"/runs/" + url.PathEscape(runID)
}

func createRunViewTask(t *testing.T, fixture *testfixture.Fixture, runID, taskID string) {
	t.Helper()
	task := contract.Task{
		TaskID:                 taskID,
		RunID:                  runID,
		OrgID:                  fixture.Scope.OrgID,
		WorkspaceID:            fixture.Scope.WorkspaceID,
		OperationID:            fixture.OperationID,
		Lane:                   contract.LaneDirectRead,
		BehaviorID:             "run-view-test",
		InputRefs:              []string{},
		SchemaRefs:             []string{},
		State:                  contract.RunStateAccepted,
		EffectSummary:          contract.EffectSummary{},
		UnknownEffectCount:     0,
		CancelRequested:        false,
		OrchestrationCondition: contract.OrchestrationPending,
		ProjectionObservedAt:   time.Now().UTC(),
		Revision:               1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		return store.CreateTask(ctx, tx, task)
	}); err != nil {
		t.Fatalf("create exact run-view task: %v", err)
	}
}

func revokeAcceptedExecutionGrant(t *testing.T, fixture *testfixture.Fixture, runID string) {
	t.Helper()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE pm.run_execution_grants
			SET revoked_at = clock_timestamp()
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND revoked_at IS NULL
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("revoke accepted execution grant affected %d rows", tag.RowsAffected())
		}
		return nil
	})
}

func expirePrimaryMembership(t *testing.T, fixture *testfixture.Fixture) {
	t.Helper()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE pm.memberships
			SET expires_at = clock_timestamp() - interval '1 second'
			WHERE org_id = $1 AND workspace_id = $2 AND identity_id = $3
			  AND revoked_at IS NULL
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, fixture.Actor.RequestedActor.PrincipalID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expire primary membership affected %d rows", tag.RowsAffected())
		}
		return nil
	})
}

func expireAndCompactRunCancelReceipt(t *testing.T, fixture *testfixture.Fixture, runID string) {
	t.Helper()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE pm.identity_mutation_keys
			SET response_expires_at = clock_timestamp() - interval '1 second'
			WHERE org_id = $1 AND workspace_id = $2
			  AND principal_id = $3 AND operation_id = 'run.cancel'
			  AND target_id = $4 AND receipt_state = 'resolved'
		`,
			fixture.Scope.OrgID,
			fixture.Scope.WorkspaceID,
			fixture.Actor.RequestedActor.PrincipalID,
			runID,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expire cancellation response affected %d rows", tag.RowsAffected())
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var compacted uint32
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		compacted, err = store.CompactIdentityMutationReceipts(ctx, tx, time.Now().UTC().Add(time.Minute), 16)
		return err
	}); err != nil {
		t.Fatalf("compact expired cancellation response: %v", err)
	}
	if compacted != 1 {
		t.Fatalf("compacted cancellation responses = %d; want 1", compacted)
	}
}

func readRunControlCounts(t *testing.T, fixture *testfixture.Fixture, runID string) runControlCounts {
	t.Helper()
	var counts runControlCounts
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT
				(SELECT count(*) FROM pm.outbox_commands
				 WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND kind = 'cancel_run'),
				(SELECT count(*) FROM pm.receipts
				 WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND kind = 'run_cancel'),
				(SELECT count(*) FROM pm.audit_entries
				 WHERE org_id = $1 AND workspace_id = $2 AND target_ref = $3 AND action = 'run_cancel_requested'),
				(SELECT count(*) FROM pm.server_events
				 WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND event_type = 'run_cancel_requested'),
				(SELECT count(*) FROM pm.identity_mutation_keys
				 WHERE org_id = $1 AND workspace_id = $2 AND target_id = $3 AND operation_id = 'run.cancel'),
				(SELECT count(*) FROM pm.pending_reservations
				 WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND released_at IS NULL)
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID).Scan(
			&counts.commands,
			&counts.receipts,
			&counts.audits,
			&counts.events,
			&counts.mutationKeys,
			&counts.pending,
		)
	})
	return counts
}

func requireRunControlCounts(t *testing.T, fixture *testfixture.Fixture, runID string, want runControlCounts) {
	t.Helper()
	got := readRunControlCounts(t, fixture, runID)
	if got != want {
		t.Fatalf("run control durable counts = %#v; want %#v", got, want)
	}
}

func installRunCancelOutboxFailure(t *testing.T, fixture *testfixture.Fixture, runID string) func() {
	t.Helper()
	suffix := strings.ReplaceAll(fixture.Scope.WorkspaceID, "-", "_")
	if len(suffix) > 24 {
		suffix = suffix[len(suffix)-24:]
	}
	functionName := "test_cancel_failure_" + suffix
	triggerName := "test_cancel_failure_" + suffix
	qualifiedFunction := pgx.Identifier{"pm", functionName}.Sanitize()
	quotedTrigger := pgx.Identifier{triggerName}.Sanitize()
	literal := func(value string) string {
		return "'" + strings.ReplaceAll(value, "'", "''") + "'"
	}
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			"DROP TRIGGER IF EXISTS %s ON pm.outbox_commands", quotedTrigger,
		)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DROP FUNCTION IF EXISTS "+qualifiedFunction+"()"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
			CREATE FUNCTION %s() RETURNS trigger
			LANGUAGE plpgsql SECURITY INVOKER
			AS $body$
			BEGIN
				RAISE EXCEPTION 'injected scoped run-control outbox failure';
				RETURN NEW;
			END
			$body$
		`, qualifiedFunction)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, fmt.Sprintf(`
			CREATE TRIGGER %s
			BEFORE INSERT ON pm.outbox_commands
			FOR EACH ROW
			WHEN (
				NEW.org_id = %s
				AND NEW.workspace_id = %s
				AND NEW.run_id = %s
				AND NEW.kind = 'cancel_run'
			)
			EXECUTE FUNCTION %s()
		`,
			quotedTrigger,
			literal(fixture.Scope.OrgID),
			literal(fixture.Scope.WorkspaceID),
			literal(runID),
			qualifiedFunction,
		))
		return err
	})
	remove := func() {
		fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, fmt.Sprintf(
				"DROP TRIGGER IF EXISTS %s ON pm.outbox_commands", quotedTrigger,
			)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "DROP FUNCTION IF EXISTS "+qualifiedFunction+"()")
			return err
		})
	}
	t.Cleanup(remove)
	return remove
}

func httpRegistrationForTest(execute, reconcile app.ExecutionCallback) app.HandlerRegistration {
	return app.HandlerRegistration{
		Lanes: []contract.Lane{contract.LaneDirectRead},
		Foundations: []contract.FoundationContract{
			{
				ID: "FND-6", Version: "1.0.0",
				Dependencies: []contract.FoundationRef{{ID: "FND-3", Version: "1.0.0"}},
			},
			{ID: "FND-3", Version: "1.0.0", Dependencies: []contract.FoundationRef{}},
		},
		Capabilities: []contract.CapabilityRequirement{
			{Capability: "typed-values", Foundation: contract.FoundationRef{ID: "FND-3", Version: "1.0.0"}},
			{Capability: "http.execute", Foundation: contract.FoundationRef{ID: "FND-6", Version: "1.0.0"}},
		},
		Execute:   execute,
		Reconcile: reconcile,
	}
}

func sqlRegistrationForTest(execute, reconcile app.ExecutionCallback) app.HandlerRegistration {
	return app.HandlerRegistration{
		Lanes: []contract.Lane{contract.LaneDirectWrite, contract.LaneDirectRead},
		Foundations: []contract.FoundationContract{
			{
				ID: "FND-7", Version: "1.0.0",
				Dependencies: []contract.FoundationRef{{ID: "FND-3", Version: "1.0.0"}},
			},
			{ID: "FND-3", Version: "1.0.0", Dependencies: []contract.FoundationRef{}},
		},
		Capabilities: []contract.CapabilityRequirement{
			{Capability: "typed-values", Foundation: contract.FoundationRef{ID: "FND-3", Version: "1.0.0"}},
			{Capability: "sql.execute", Foundation: contract.FoundationRef{ID: "FND-7", Version: "1.0.0"}},
		},
		Execute:   execute,
		Reconcile: reconcile,
	}
}

func extensionRegistrationsForTest(execute app.ExecutionCallback) []app.HandlerRegistration {
	return []app.HandlerRegistration{
		singleCapabilityRegistrationForTest("FND-101", "etl.execute", []contract.Lane{contract.LaneETL}, execute),
		singleCapabilityRegistrationForTest("FND-102", "apply.execute", []contract.Lane{contract.LaneDirectWrite}, execute),
		singleCapabilityRegistrationForTest("FND-103", "reverse.execute", []contract.Lane{contract.LaneReverseETL}, execute),
		singleCapabilityRegistrationForTest(
			"FND-104",
			"binary.execute",
			[]contract.Lane{contract.LaneBinaryUpload, contract.LaneBinaryDownload},
			execute,
		),
	}
}

func singleCapabilityRegistrationForTest(
	foundationID string,
	capability string,
	lanes []contract.Lane,
	execute app.ExecutionCallback,
) app.HandlerRegistration {
	foundation := contract.FoundationRef{ID: foundationID, Version: "1.0.0"}
	return app.HandlerRegistration{
		Lanes:        lanes,
		Foundations:  []contract.FoundationContract{{ID: foundation.ID, Version: foundation.Version, Dependencies: []contract.FoundationRef{}}},
		Capabilities: []contract.CapabilityRequirement{{Capability: capability, Foundation: foundation}},
		Execute:      execute,
	}
}

func operationForRegistryTest(
	transportKind string,
	lane contract.Lane,
	capabilities []contract.CapabilityRequirement,
) contract.Operation {
	transport := contract.Transport{Kind: contract.TransportKindHTTP, Method: "GET", Path: "/fixture"}
	status := "200"
	if transportKind == contract.TransportKindPostgreSQL {
		transport = contract.Transport{
			Kind:            contract.TransportKindPostgreSQL,
			StatementKind:   contract.PostgreSQLStatementSelect,
			RelationBinding: "fixture-relation",
		}
		status = "success"
	}
	sourceRefs := []contract.SourceRef{
		{SourceID: "fixture-source", Pointer: "#/operations/fixture"},
	}
	return contract.Operation{
		SchemaVersion:   contract.OperationSchemaVersion,
		OperationID:     "fixture.operation",
		SourceRefs:      sourceRefs,
		Transport:       transport,
		Inputs:          []contract.FieldBinding{},
		RequestVariants: []contract.RequestVariant{},
		Responses: []contract.Response{
			{
				Status:     status,
				NoContent:  false,
				MediaType:  "application/json",
				Schema:     &contract.Schema{Type: contract.SchemaTypeString},
				SourceRefs: slices.Clone(sourceRefs),
			},
		},
		CapabilityRequirements: capabilities,
		LaneEligibility:        operationLaneEligibilityForTest(lane),
	}
}

func operationLaneEligibilityForTest(eligible contract.Lane) []contract.LaneEligibility {
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

func controlledCallbackForRegistryTest(value string) app.ExecutionCallback {
	return func(context.Context, app.ExecutionRequest) (app.ExecutionObservation, error) {
		return observationForRegistryTest(value), nil
	}
}

func observationForRegistryTest(value string) app.ExecutionObservation {
	return app.ExecutionObservation{
		Result: &contract.ActionResult{
			MediaType: "application/json",
			Data:      contract.NewStringValue(value),
		},
		Outcomes: []contract.MemberOutcome{},
	}
}

func observationTextForRegistryTest(observation app.ExecutionObservation) (string, bool) {
	if observation.Result == nil {
		return "", false
	}
	return observation.Result.Data.TextValue()
}
