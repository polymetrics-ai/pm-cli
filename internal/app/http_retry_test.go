package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/activities"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
	"github.com/polymetrics-ai/pm-cli/internal/workflows"
	sdkclient "go.temporal.io/sdk/client"
	sdkworker "go.temporal.io/sdk/worker"
)

const durableHTTPRetryResult = `{"big":9007199254740993,"nested":{"empty":"","flag":false},"null":null}`

func TestDurableHTTPRetryAcrossTemporalWorkerReplacement(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	producer := testfixture.OpenArtifacts(t, testfixture.PrimaryArtifactGateway)
	consumer := testfixture.OpenArtifacts(t, testfixture.SecondaryArtifactGateway)
	var peerCalls atomic.Int32
	var callTimesMu sync.Mutex
	var callTimes []time.Time
	origin, tlsConfig, dialer := newHTTPDirectReadTLSPeer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		callTimesMu.Lock()
		callTimes = append(callTimes, time.Now().UTC())
		callTimesMu.Unlock()
		call := peerCalls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		if call == 1 {
			writer.Header().Set("Retry-After", "5")
			writer.WriteHeader(http.StatusServiceUnavailable)
		}
		_, _ = io.WriteString(writer, durableHTTPRetryResult)
	}))
	selection := durableHTTPRetrySelection("fixture.http.retry.temporal", origin)
	policy := httpDirectReadPolicy(origin)
	fixture := newHTTPDirectReadFixture(
		t,
		selection,
		policy,
		app.HTTPDirectReadDependencies{
			Resolver: httpDirectReadResolver{}, Dialer: dialer, TLSConfig: tlsConfig,
		},
		func(_ testfixture.ServiceDependencies, configuration *httpapi.ServiceConfig) error {
			configuration.Artifacts = consumer.Backend
			return nil
		},
	)
	firstPublisher, err := objects.NewPublisher(fixture.Pool, producer.Backend, producer.Limits)
	if err != nil {
		t.Fatal(err)
	}
	firstService := workerResultService(t, fixture, temporal, firstPublisher, 4096)
	admitted := admitHTTPDirectReadRun(
		t, fixture, selection, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
		"durable-http-retry-temporal",
	)
	run := preparePermitWorkerRun(t, fixture, firstService, temporal, fixture.Scope, fixture.Actor, admitted)
	firstWorker := startDurableHTTPRetryWorker(t, temporal.backend, temporal.timeouts, firstService)
	firstStopped := false
	t.Cleanup(func() {
		if !firstStopped {
			firstWorker.Stop()
		}
	})

	root, retry, parentLease, _ := waitPreparedHTTPRetry(t, fixture, run)
	if retry.Retry == nil || retry.AttemptNumber != 2 || len(retry.Attempts) != 0 ||
		retry.State != store.DispatchPrepared {
		t.Fatalf("prepared retry leaf = %#v", retry)
	}
	if peerCalls.Load() != 1 {
		t.Fatalf("provider calls before retry eligibility = %d; want 1", peerCalls.Load())
	}
	remaining := time.Until(retry.Retry.NotBefore)
	if remaining <= time.Second {
		t.Fatalf("retry eligibility remaining = %s; want enough time to prove no early activity", remaining)
	}
	timer := time.NewTimer(remaining / 2)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-time.After(5 * time.Second):
		t.Fatal("bounded pre-eligibility observation timed out")
	}
	if peerCalls.Load() != 1 {
		t.Fatalf("provider was called before durable eligibility: %d calls", peerCalls.Load())
	}
	firstWorker.Stop()
	firstStopped = true
	if peerCalls.Load() != 1 {
		t.Fatalf("draining the first worker dispatched the prepared retry early")
	}

	secondPublisher, err := objects.NewPublisher(fixture.Pool, producer.Backend, producer.Limits)
	if err != nil {
		t.Fatal(err)
	}
	secondService := workerResultService(t, fixture, temporal, secondPublisher, 4096)
	secondWorker := startDurableHTTPRetryWorker(t, temporal.backend, temporal.timeouts, secondService)
	defer secondWorker.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var projection contract.WorkflowProjectionResult
	if err := temporal.backend.GetWorkflow(
		ctx, run.execution.WorkflowID, run.execution.RunID,
	).Get(ctx, &projection); err != nil {
		t.Fatal(err)
	}
	if projection.State != contract.RunStateSucceeded {
		t.Fatalf("retry workflow projection = %#v; want succeeded", projection)
	}
	if peerCalls.Load() != 2 || dialer.calls.Load() != 2 {
		t.Fatalf("provider calls/dials = %d/%d; want 2/2", peerCalls.Load(), dialer.calls.Load())
	}
	callTimesMu.Lock()
	observedCallTimes := append([]time.Time(nil), callTimes...)
	callTimesMu.Unlock()
	if len(observedCallTimes) != 2 || observedCallTimes[1].Before(retry.Retry.NotBefore) {
		t.Fatalf("provider call times = %v; retry not_before = %s", observedCallTimes, retry.Retry.NotBefore)
	}
	assertDurableHTTPRetryLineage(t, fixture, run, root, retry, parentLease)

	duplicateRun := run
	duplicateRun.plan.Dispatch = root.Intent
	duplicate, err := executePermitWorkerUnit(t, secondService, duplicateRun)
	if err != nil || duplicate.State != contract.RunStateReconciling || duplicate.UnknownEffectCount != 1 {
		t.Fatalf("duplicate old qualified unit = %#v error=%v; want its deferred parent settlement", duplicate, err)
	}
	if peerCalls.Load() != 2 {
		t.Fatalf("duplicate old qualified unit resent provider request: %d calls", peerCalls.Load())
	}

	refs := workerResultReferences(t, fixture, admitted.RunRef)
	if len(refs) != 1 {
		t.Fatalf("result references = %v; want one retry result", refs)
	}
	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	payload := workerResultGET(
		t,
		fixture,
		"/v1/workspaces/"+fixture.Scope.WorkspaceID+"/artifacts/"+refs[0]+"/content",
		http.StatusOK,
	)
	var result contract.ActionResult
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatal(err)
	}
	assertHTTPDirectReadExactResult(t, result)
}

func TestDefinitiveUnsentHTTPRetryCreatesOneProviderRequest(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	var providerCalls atomic.Int32
	origin, tlsConfig, directDialer := newHTTPDirectReadTLSPeer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, durableHTTPRetryResult)
	}))
	dialer := &failFirstHTTPDialer{address: directDialer.address}
	selection := httpDirectReadSelection("fixture.http.retry.definitive-unsent", origin, http.StatusOK, false)
	fixture := newHTTPDirectReadFixture(t, selection, httpDirectReadPolicy(origin), app.HTTPDirectReadDependencies{
		Resolver: httpDirectReadResolver{}, Dialer: dialer, TLSConfig: tlsConfig,
	}, nil)
	service := newPermitWorkerService(t, fixture, temporal)
	admitted := admitHTTPDirectReadRun(
		t, fixture, selection, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
		"durable-http-retry-definitive-unsent",
	)
	run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
	first, err := executePermitWorkerUnit(t, service, run)
	if err != nil || first.State != contract.RunStateRunning || first.UnknownEffectCount != 0 {
		t.Fatalf("definitive-unsent parent result = %#v error=%v", first, err)
	}
	if providerCalls.Load() != 0 || dialer.calls.Load() != 1 {
		t.Fatalf("first dial/provider calls = %d/%d; want 1/0", dialer.calls.Load(), providerCalls.Load())
	}
	root, retry, _, _ := waitPreparedHTTPRetry(t, fixture, run)
	if retry.Retry == nil || retry.Retry.Basis != store.WorkerRetryDefinitiveUnsent {
		t.Fatalf("definitive-unsent retry metadata = %#v", retry.Retry)
	}
	snapshot, err := service.LoadWorkflow(context.Background(), run.execution, run.reference)
	if err != nil || len(snapshot.Units) != 1 || snapshot.Units[0].DispatchID != retry.Intent.DispatchID {
		t.Fatalf("eligible definitive-unsent snapshot = %#v error=%v", snapshot, err)
	}
	run.plan.Dispatch = retry.Intent
	second, err := executePermitWorkerUnit(t, service, run)
	if err != nil || second.State != contract.RunStateSucceeded || second.UnknownEffectCount != 0 {
		t.Fatalf("definitive-unsent retry result = %#v error=%v", second, err)
	}
	if providerCalls.Load() != 1 || dialer.calls.Load() != 2 {
		t.Fatalf("final dial/provider calls = %d/%d; want 2/1", dialer.calls.Load(), providerCalls.Load())
	}
	assertHTTPRetryOutcomeStates(
		t, fixture, run, root.Intent.DispatchID, retry.Intent.DispatchID,
		contract.OutcomeUnsent, contract.OutcomeSucceeded,
	)
}

func TestSourceSafeUnknownHTTPRetryRetainsChargedParentPermit(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	var providerCalls atomic.Int32
	origin, tlsConfig, dialer := newHTTPDirectReadTLSPeer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		call := providerCalls.Add(1)
		if call == 1 {
			hijacker, ok := writer.(http.Hijacker)
			if !ok {
				t.Error("TLS peer response writer cannot hijack")
				return
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack TLS peer connection: %v", err)
				return
			}
			_ = connection.Close()
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, durableHTTPRetryResult)
	}))
	selection := durableHTTPRetrySelection("fixture.http.retry.source-safe-unknown", origin)
	fixture := newHTTPDirectReadFixture(t, selection, httpDirectReadPolicy(origin), app.HTTPDirectReadDependencies{
		Resolver: httpDirectReadResolver{}, Dialer: dialer, TLSConfig: tlsConfig,
	}, nil)
	service := newPermitWorkerService(t, fixture, temporal)
	admitted := admitHTTPDirectReadRun(
		t, fixture, selection, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
		"durable-http-retry-source-safe-unknown",
	)
	run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
	first, err := executePermitWorkerUnit(t, service, run)
	if err != nil || first.State != contract.RunStateReconciling || first.UnknownEffectCount != 1 {
		t.Fatalf("unknown parent result = %#v error=%v", first, err)
	}
	root, retry, _, _ := waitPreparedHTTPRetry(t, fixture, run)
	if retry.Retry == nil || retry.Retry.Basis != store.WorkerRetrySourceDeclaredSafe {
		t.Fatalf("source-safe retry metadata = %#v", retry.Retry)
	}
	assertHTTPRetryParentPermitState(t, fixture, run, root, store.AccountPermitUnknown)
	run.plan.Dispatch = retry.Intent
	second, err := executePermitWorkerUnit(t, service, run)
	if err != nil || second.State != contract.RunStateSucceeded || second.UnknownEffectCount != 0 {
		t.Fatalf("source-safe retry result = %#v error=%v", second, err)
	}
	if providerCalls.Load() != 2 || dialer.calls.Load() != 2 {
		t.Fatalf("source-safe retry calls/dials = %d/%d; want 2/2", providerCalls.Load(), dialer.calls.Load())
	}
	assertHTTPRetryOutcomeStates(
		t, fixture, run, root.Intent.DispatchID, retry.Intent.DispatchID,
		contract.OutcomeUnknown, contract.OutcomeSucceeded,
	)
}

func TestUnknownHTTPAttemptWithoutUnconditionalSourceSafetyCreatesNoRetry(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		classification contract.OperationRetryClassification
		condition      string
	}{
		{name: "unsafe", classification: contract.OperationRetryUnsafe},
		{name: "conditional", classification: contract.OperationRetryConditional, condition: "provider-key-absent"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			temporal := newPermitTemporalHarness(t)
			var providerCalls atomic.Int32
			origin, tlsConfig, dialer := newHTTPDirectReadTLSPeer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				providerCalls.Add(1)
				hijacker, ok := writer.(http.Hijacker)
				if !ok {
					t.Error("TLS peer response writer cannot hijack")
					return
				}
				connection, _, err := hijacker.Hijack()
				if err != nil {
					t.Errorf("hijack TLS peer connection: %v", err)
					return
				}
				_ = connection.Close()
			}))
			selection := durableHTTPRetrySelection("fixture.http.retry."+testCase.name+"-unknown", origin)
			execution := *selection.Operation.Execution
			retry := *execution.Retry
			retry.Classification = testCase.classification
			retry.Condition = testCase.condition
			execution.Retry = &retry
			selection.Operation.Execution = &execution
			fixture := newHTTPDirectReadFixture(t, selection, httpDirectReadPolicy(origin), app.HTTPDirectReadDependencies{
				Resolver: httpDirectReadResolver{}, Dialer: dialer, TLSConfig: tlsConfig,
			}, nil)
			service := newPermitWorkerService(t, fixture, temporal)
			admitted := admitHTTPDirectReadRun(
				t, fixture, selection, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
				"durable-http-retry-"+testCase.name+"-unknown",
			)
			run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
			result, err := executePermitWorkerUnit(t, service, run)
			if err != nil || result.State != contract.RunStateReconciling || result.UnknownEffectCount != 1 {
				t.Fatalf("%s unknown result = %#v error=%v", testCase.name, result, err)
			}
			root, leaf := loadHTTPRetryDispatches(t, fixture, run)
			if providerCalls.Load() != 1 || dialer.calls.Load() != 1 ||
				leaf.Intent.DispatchID != root.Intent.DispatchID || leaf.AttemptNumber != 1 {
				t.Fatalf("%s unknown calls/dials/leaf = %d/%d/%#v", testCase.name, providerCalls.Load(), dialer.calls.Load(), leaf)
			}
		})
	}
}

func TestDurableHTTPRetryStopsAtMaxAttempts(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	var providerCalls atomic.Int32
	origin, tlsConfig, dialer := newHTTPDirectReadTLSPeer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(writer, durableHTTPRetryResult)
	}))
	selection := durableHTTPRetrySelection("fixture.http.retry.max-attempts", origin)
	fixture := newHTTPDirectReadFixture(t, selection, httpDirectReadPolicy(origin), app.HTTPDirectReadDependencies{
		Resolver: httpDirectReadResolver{}, Dialer: dialer, TLSConfig: tlsConfig,
	}, nil)
	service := newPermitWorkerService(t, fixture, temporal)
	admitted := admitHTTPDirectReadRun(
		t, fixture, selection, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
		"durable-http-retry-max-attempts",
	)
	run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
	first, err := executePermitWorkerUnit(t, service, run)
	if err != nil || first.State != contract.RunStateReconciling || first.UnknownEffectCount != 1 {
		t.Fatalf("first retryable response = %#v error=%v", first, err)
	}
	_, retry, _, _ := waitPreparedHTTPRetry(t, fixture, run)
	run.plan.Dispatch = retry.Intent
	second, err := executePermitWorkerUnit(t, service, run)
	if err != nil || second.State != contract.RunStateFailed || second.UnknownEffectCount != 0 {
		t.Fatalf("exhausted retry response = %#v error=%v", second, err)
	}
	_, leaf := loadHTTPRetryDispatches(t, fixture, run)
	if providerCalls.Load() != 2 || dialer.calls.Load() != 2 ||
		leaf.Intent.DispatchID != retry.Intent.DispatchID || leaf.AttemptNumber != 2 {
		t.Fatalf("exhausted retry calls/dials/leaf = %d/%d/%#v", providerCalls.Load(), dialer.calls.Load(), leaf)
	}
}

func TestDurableHTTPRetrySuppressesInvalidOrLateRetryAfter(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		value    string
		maxDelay time.Duration
	}{
		{name: "malformed", value: "not-a-delay", maxDelay: time.Minute},
		{name: "over-bound", value: "61", maxDelay: time.Minute},
		{name: "beyond-original-deadline", value: "3700", maxDelay: 2 * time.Hour},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			temporal := newPermitTemporalHarness(t)
			var providerCalls atomic.Int32
			origin, tlsConfig, dialer := newHTTPDirectReadTLSPeer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				providerCalls.Add(1)
				writer.Header().Set("Content-Type", "application/json")
				writer.Header().Set("Retry-After", testCase.value)
				writer.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(writer, durableHTTPRetryResult)
			}))
			selection := durableHTTPRetrySelection("fixture.http.retry.suppressed-"+testCase.name, origin)
			policy := httpDirectReadPolicy(origin)
			policy.MaxRetryDelay = testCase.maxDelay
			fixture := newHTTPDirectReadFixture(t, selection, policy, app.HTTPDirectReadDependencies{
				Resolver: httpDirectReadResolver{}, Dialer: dialer, TLSConfig: tlsConfig,
			}, nil)
			service := newPermitWorkerService(t, fixture, temporal)
			admitted := admitHTTPDirectReadRun(
				t, fixture, selection, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
				"durable-http-retry-suppressed-"+testCase.name,
			)
			run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
			result, err := executePermitWorkerUnit(t, service, run)
			if err != nil || result.State != contract.RunStateFailed || result.UnknownEffectCount != 0 {
				t.Fatalf("suppressed Retry-After result = %#v error=%v", result, err)
			}
			root, leaf := loadHTTPRetryDispatches(t, fixture, run)
			if providerCalls.Load() != 1 || dialer.calls.Load() != 1 ||
				leaf.Intent.DispatchID != root.Intent.DispatchID || leaf.AttemptNumber != 1 {
				t.Fatalf("suppressed Retry-After calls/dials/leaf = %d/%d/%#v", providerCalls.Load(), dialer.calls.Load(), leaf)
			}
		})
	}
}

func TestDurableHTTPRetryRechecksRevocationBeforeSecondSend(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	var providerCalls atomic.Int32
	origin, tlsConfig, dialer := newHTTPDirectReadTLSPeer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(writer, durableHTTPRetryResult)
	}))
	selection := durableHTTPRetrySelection("fixture.http.retry.revocation", origin)
	fixture := newHTTPDirectReadFixture(t, selection, httpDirectReadPolicy(origin), app.HTTPDirectReadDependencies{
		Resolver: httpDirectReadResolver{}, Dialer: dialer, TLSConfig: tlsConfig,
	}, nil)
	service := newPermitWorkerService(t, fixture, temporal)
	admitted := admitHTTPDirectReadRun(
		t, fixture, selection, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
		"durable-http-retry-revocation",
	)
	run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
	first, err := executePermitWorkerUnit(t, service, run)
	if err != nil || first.State != contract.RunStateReconciling {
		t.Fatalf("first retryable response = %#v error=%v", first, err)
	}
	_, retry, _, _ := waitPreparedHTTPRetry(t, fixture, run)
	revokePermitTestAuthority(t, fixture, admitted.RunRef, "grant")
	run.plan.Dispatch = retry.Intent
	blocked, err := executePermitWorkerUnit(t, service, run)
	if err != nil || blocked.State != contract.RunStateReconciling || blocked.UnknownEffectCount != 1 {
		t.Fatalf("revoked retry result = %#v error=%v; want retained deferred state", blocked, err)
	}
	if providerCalls.Load() != 1 || dialer.calls.Load() != 1 {
		t.Fatalf("revoked retry reached provider: calls/dials = %d/%d", providerCalls.Load(), dialer.calls.Load())
	}
}

func TestDurableHTTPRetryRechecksChangedAuthorityBeforeSecondSend(t *testing.T) {
	for _, change := range []string{"source_revision", "account_witness", "cancellation"} {
		t.Run(change, func(t *testing.T) {
			temporal := newPermitTemporalHarness(t)
			var providerCalls atomic.Int32
			origin, tlsConfig, dialer := newHTTPDirectReadTLSPeer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				providerCalls.Add(1)
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(writer, durableHTTPRetryResult)
			}))
			selection := durableHTTPRetrySelection("fixture.http.retry."+change, origin)
			fixture := newHTTPDirectReadFixture(t, selection, httpDirectReadPolicy(origin), app.HTTPDirectReadDependencies{
				Resolver: httpDirectReadResolver{}, Dialer: dialer, TLSConfig: tlsConfig,
			}, nil)
			var sourceChanged atomic.Bool
			resolve := fixture.ResolveOperation
			fixture.ResolveOperation = func(ctx context.Context, scope contract.Scope, connectionID, operationID string) (app.OperationSelection, error) {
				current, err := resolve(ctx, scope, connectionID, operationID)
				if err == nil && sourceChanged.Load() {
					current.SchemaRefs = []string{"changed-retry-source-revision"}
				}
				return current, err
			}
			service := newPermitWorkerService(t, fixture, temporal)
			admitted := admitHTTPDirectReadRun(
				t, fixture, selection, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
				"durable-http-retry-"+change,
			)
			run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
			first, err := executePermitWorkerUnit(t, service, run)
			if err != nil || first.State != contract.RunStateReconciling {
				t.Fatalf("first retryable response = %#v error=%v", first, err)
			}
			rootBefore, retryBefore, _, _ := waitPreparedHTTPRetry(t, fixture, run)
			switch change {
			case "source_revision":
				sourceChanged.Store(true)
			case "account_witness":
				fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
					tag, err := tx.Exec(ctx, `
						UPDATE pm.provider_accounts
						SET account_witness = 'changed-retry-account', revision = revision + 1
						WHERE org_id = $1 AND provider_account_id = $2
					`, fixture.Scope.OrgID, fixture.Connection.ProviderAccountID)
					if err != nil {
						return err
					}
					if tag.RowsAffected() != 1 {
						return errors.New("controlled account identity update did not affect one row")
					}
					return nil
				})
			case "cancellation":
				before := getRunViewForTest(t, fixture.Client, runPathForTest(fixture, admitted.RunRef))
				response := sendRunCancel(t, fixture, fixture.Client, admitted.RunRef,
					"cancel-prepared-http-retry", before.Revision, true)
				if response.status != http.StatusAccepted {
					t.Fatalf("cancel prepared retry = HTTP %d; want 202", response.status)
				}
				after := getRunViewForTest(t, fixture.Client, runPathForTest(fixture, admitted.RunRef))
				if !after.CancelRequested {
					t.Fatal("accepted cancellation was not durable")
				}
			}
			run.plan.Dispatch = retryBefore.Intent
			blocked, err := executePermitWorkerUnit(t, service, run)
			if change == "cancellation" {
				if err != nil || blocked.State != contract.RunStateReconciling || blocked.UnknownEffectCount != 1 {
					t.Fatalf("cancelled retry = %#v error=%v; want retained deferred state", blocked, err)
				}
			} else if !errors.Is(err, app.ErrWorkerConflict) {
				t.Fatalf("changed authority retry = %v; want immutable authority conflict", err)
			}
			rootAfter, retryAfter := loadHTTPRetryDispatches(t, fixture, run)
			if !reflect.DeepEqual(rootBefore, rootAfter) || !reflect.DeepEqual(retryBefore, retryAfter) {
				t.Fatal("refused retry changed the immutable parent or fabricated a child attempt")
			}
			permits, attempts := permitTestDurableCounts(t, fixture, fixture.Scope, admitted.RunRef)
			if providerCalls.Load() != 1 || dialer.calls.Load() != 1 || permits != 1 || attempts != 1 {
				t.Fatalf("refused retry calls/dials/permits/attempts = %d/%d/%d/%d; want 1/1/1/1",
					providerCalls.Load(), dialer.calls.Load(), permits, attempts)
			}
		})
	}
}

func durableHTTPRetrySelection(operationID, origin string) app.OperationSelection {
	selection := httpDirectReadSelection(operationID, origin, http.StatusServiceUnavailable, true)
	success := selection.Operation.Responses[0]
	success.Status = "200"
	selection.Operation.Responses = append([]contract.Response{success}, selection.Operation.Responses...)
	return selection
}

func startDurableHTTPRetryWorker(
	t *testing.T,
	client sdkclient.Client,
	timeouts contract.ActivityTimeouts,
	service *app.WorkerService,
) sdkworker.Worker {
	t.Helper()
	activityService, err := activities.New(service)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := workflows.NewCoordinator(timeouts)
	if err != nil {
		t.Fatal(err)
	}
	worker := sdkworker.New(client, contract.ControlTaskQueue, sdkworker.Options{})
	if err := workflows.RegisterWorkflows(worker, coordinator, service.SupportsScheduledAdmission()); err != nil {
		t.Fatal(err)
	}
	if err := activities.RegisterActivities(worker, activityService); err != nil {
		t.Fatal(err)
	}
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	return worker
}

func waitPreparedHTTPRetry(
	t *testing.T,
	fixture *testfixture.Fixture,
	run permitWorkerRun,
) (store.DispatchRecord, store.DispatchRecord, store.Lease, time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		var root, leaf store.DispatchRecord
		var lease store.Lease
		var observedAt time.Time
		err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(
			ctx context.Context,
			tx *store.Transaction,
		) error {
			ref := store.TaskRef{RunID: run.reference.RunID, TaskID: run.plan.TaskID}
			var err error
			root, err = store.LoadInitialWorkerDispatch(ctx, tx, ref)
			if err != nil {
				return err
			}
			leaf, err = store.LoadWorkerDispatch(ctx, tx, ref)
			if err != nil {
				return err
			}
			lease, err = store.LoadLease(ctx, tx, ref)
			if err != nil {
				return err
			}
			observedAt, err = store.ReadAuthenticationTime(ctx, tx)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if leaf.AttemptNumber == 2 && leaf.Retry != nil {
			if observedAt.Before(lease.ExpiresAt) {
				t.Fatalf("parent retry lease remained live: now=%s expires=%s", observedAt, lease.ExpiresAt)
			}
			return root, leaf, lease, observedAt
		}
		select {
		case <-ctx.Done():
			t.Fatal("durable retry child was not prepared")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func loadHTTPRetryDispatches(
	t *testing.T,
	fixture *testfixture.Fixture,
	run permitWorkerRun,
) (store.DispatchRecord, store.DispatchRecord) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var root, leaf store.DispatchRecord
	err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(
		ctx context.Context,
		tx *store.Transaction,
	) error {
		ref := store.TaskRef{RunID: run.reference.RunID, TaskID: run.plan.TaskID}
		var err error
		root, err = store.LoadInitialWorkerDispatch(ctx, tx, ref)
		if err != nil {
			return err
		}
		leaf, err = store.LoadWorkerDispatch(ctx, tx, ref)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return root, leaf
}

func assertDurableHTTPRetryLineage(
	t *testing.T,
	fixture *testfixture.Fixture,
	run permitWorkerRun,
	rootBefore store.DispatchRecord,
	retryBefore store.DispatchRecord,
	parentLease store.Lease,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(
		ctx context.Context,
		tx *store.Transaction,
	) error {
		ref := store.TaskRef{RunID: run.reference.RunID, TaskID: run.plan.TaskID}
		root, err := store.LoadInitialWorkerDispatch(ctx, tx, ref)
		if err != nil {
			return err
		}
		leaf, err := store.LoadWorkerDispatch(ctx, tx, ref)
		if err != nil {
			return err
		}
		lease, err := store.LoadLease(ctx, tx, ref)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(root.Intent, rootBefore.Intent) || root.AttemptNumber != 1 || len(root.Attempts) != 1 ||
			leaf.Intent.DispatchID != retryBefore.Intent.DispatchID || leaf.AttemptNumber != 2 || len(leaf.Attempts) != 1 ||
			leaf.Retry == nil || leaf.Retry.ParentDispatchID != root.Intent.DispatchID ||
			leaf.Retry.ParentAttempt != root.Attempts[0] || leaf.Intent.EffectKey != root.Intent.EffectKey ||
			lease.Fence != parentLease.Fence+1 {
			return errors.New("durable retry lineage, attempts, effect identity, or fresh fence changed")
		}
		if leaf.Intent.DispatchID == root.Intent.DispatchID || leaf.Intent.AttemptID == root.Intent.AttemptID ||
			leaf.Attempts[0].ReceiptRef == root.Attempts[0].ReceiptRef {
			return errors.New("retry reused a parent dispatch, attempt, or receipt identity")
		}
		rootOutcomes, err := store.ListMemberOutcomes(ctx, tx, ref, root.Intent.DispatchID)
		if err != nil {
			return err
		}
		leafOutcomes, err := store.ListMemberOutcomes(ctx, tx, ref, leaf.Intent.DispatchID)
		if err != nil {
			return err
		}
		if len(rootOutcomes) != 1 || rootOutcomes[0].State != contract.OutcomeRejected ||
			len(leafOutcomes) != 1 || leafOutcomes[0].State != contract.OutcomeSucceeded {
			return errors.New("retry lost exact parent rejection or child success")
		}
		effects, err := store.ListWorkerRunEffects(ctx, tx, run.reference.RunID)
		if err != nil {
			return err
		}
		if len(effects) != 1 || effects[0].EffectKey != root.Intent.EffectKey ||
			effects[0].State != contract.OutcomeSucceeded || len(effects[0].Attempts) != 2 {
			return errors.New("retry created a second logical effect or lost attempt history")
		}
		leafSettlement := permitTestRecordID(t, "worker-settlement-v1-", struct {
			Scope      contract.Scope      `json:"scope"`
			RunID      string              `json:"run_id"`
			TaskID     string              `json:"task_id"`
			DispatchID contract.DispatchID `json:"dispatch_id"`
		}{fixture.Scope, run.reference.RunID, run.plan.TaskID, leaf.Intent.DispatchID})
		for _, receipt := range []string{leaf.Retry.SettlementReceiptID, leafSettlement} {
			exists, err := store.WorkerReceiptExists(ctx, tx, run.reference.RunID, receipt, "worker_settlement")
			if err != nil {
				return err
			}
			if !exists {
				return errors.New("durable retry settlement receipt is absent")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	permits, attempts := permitTestDurableCounts(t, fixture, fixture.Scope, run.reference.RunID)
	if permits != 2 || attempts != 2 {
		t.Fatalf("retry permits/attempts = %d/%d; want 2/2", permits, attempts)
	}
}

func assertHTTPRetryParentPermitState(
	t *testing.T,
	fixture *testfixture.Fixture,
	run permitWorkerRun,
	root store.DispatchRecord,
	want store.AccountPermitState,
) {
	t.Helper()
	permitID := permitTestRecordID(t, "permit-v1-", struct {
		Scope      contract.Scope      `json:"scope"`
		RunID      string              `json:"run_id"`
		TaskID     string              `json:"task_id"`
		DispatchID contract.DispatchID `json:"dispatch_id"`
	}{fixture.Scope, run.reference.RunID, run.plan.TaskID, root.Intent.DispatchID})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(
		ctx context.Context,
		tx *store.Transaction,
	) error {
		permit, err := store.LoadAccountPermit(ctx, tx, permitID)
		if err != nil {
			return err
		}
		if permit.State != want {
			return fmt.Errorf("parent permit state = %q; want %q", permit.State, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertHTTPRetryOutcomeStates(
	t *testing.T,
	fixture *testfixture.Fixture,
	run permitWorkerRun,
	rootID contract.DispatchID,
	retryID contract.DispatchID,
	rootState contract.OutcomeState,
	retryState contract.OutcomeState,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(
		ctx context.Context,
		tx *store.Transaction,
	) error {
		ref := store.TaskRef{RunID: run.reference.RunID, TaskID: run.plan.TaskID}
		rootOutcomes, err := store.ListMemberOutcomes(ctx, tx, ref, rootID)
		if err != nil {
			return err
		}
		retryOutcomes, err := store.ListMemberOutcomes(ctx, tx, ref, retryID)
		if err != nil {
			return err
		}
		if len(rootOutcomes) != 1 || rootOutcomes[0].State != rootState ||
			len(retryOutcomes) != 1 || retryOutcomes[0].State != retryState {
			return errors.New("HTTP retry outcome states differ from exact attempts")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

type failFirstHTTPDialer struct {
	address string
	calls   atomic.Int32
}

func (dialer *failFirstHTTPDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	if dialer.calls.Add(1) == 1 {
		return nil, errors.New("controlled dial-before-request failure")
	}
	return (&net.Dialer{}).DialContext(ctx, network, dialer.address)
}
