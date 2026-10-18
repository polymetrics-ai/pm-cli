package app_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	pmruntime "github.com/polymetrics-ai/pm-cli/internal/runtime"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

// testClientIdentity is the bounded product identity the HTTP direct-read tests
// configure. A process that registers a declared connector slice must identify
// itself; the identity contract itself is exercised by the runtime package.
const testClientIdentity = pmruntime.ClientIdentity("pm-cli/test-fixture")

func TestHTTPDirectReadRegistrationRequiresSealedPolicyAndPhysicalAuthority(t *testing.T) {
	selection := httpDirectReadSelection("fixture.http.direct-read.requirements", "https://provider.test", http.StatusOK, false)
	policy := httpDirectReadPolicy("https://provider.test")
	dialer := &httpDirectReadDialer{address: "127.0.0.1:1"}
	registration := app.NewHTTPDirectReadRegistration(app.HTTPDirectReadDependencies{ClientIdentity: testClientIdentity,
		Resolver: httpDirectReadResolver{},
		Dialer:   dialer,
	})

	if _, err := registration.Execute(context.Background(), app.ExecutionRequest{
		Operation: selection.Operation,
	}); err == nil || errors.Is(err, app.ErrWorkerAuthorization) {
		t.Fatalf("missing sealed policy error = %v; want policy refusal before authority", err)
	}
	if _, err := registration.Execute(context.Background(), app.ExecutionRequest{
		Operation:  selection.Operation,
		HTTPPolicy: policy,
	}); !errors.Is(err, app.ErrWorkerAuthorization) {
		t.Fatalf("missing physical authority error = %v; want ErrWorkerAuthorization", err)
	}
	if dialer.calls.Load() != 0 {
		t.Fatalf("missing policy/authority reached provider dialer %d times", dialer.calls.Load())
	}
}

func TestHTTPDirectReadRegistrationWorkerBehavior(t *testing.T) {
	temporal := newPermitTemporalHarness(t)

	t.Run("exact full nested null result reaches immutable shared artifact", func(t *testing.T) {
		producer := testfixture.OpenArtifacts(t, testfixture.PrimaryArtifactGateway)
		consumer := testfixture.OpenArtifacts(t, testfixture.SecondaryArtifactGateway)
		var peerCalls atomic.Int32
		origin, tlsConfig, dialer := newHTTPDirectReadTLSPeer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			peerCalls.Add(1)
			if request.Method != http.MethodGet || request.URL.EscapedPath() != "/fixture" {
				t.Errorf("provider request = %s %s; want GET /fixture", request.Method, request.URL.EscapedPath())
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"big":9007199254740993,"nested":{"empty":"","flag":false},"null":null}`)
		}))
		selection := httpDirectReadSelection("fixture.http.direct-read.result", origin, http.StatusOK, false)
		policy := httpDirectReadPolicy(origin)
		fixture := newHTTPDirectReadFixture(
			t,
			selection,
			policy,
			app.HTTPDirectReadDependencies{ClientIdentity: testClientIdentity,
				Resolver:  httpDirectReadResolver{},
				Dialer:    dialer,
				TLSConfig: tlsConfig,
			},
			func(_ testfixture.ServiceDependencies, configuration *httpapi.ServiceConfig) error {
				configuration.Artifacts = consumer.Backend
				return nil
			},
		)
		publisher, err := objects.NewPublisher(fixture.Pool, producer.Backend, producer.Limits)
		if err != nil {
			t.Fatal(err)
		}
		service := workerResultService(t, fixture, temporal, publisher, 4096)
		admitted := admitHTTPDirectReadRun(
			t, fixture, selection, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
			"http-direct-read-exact-result",
		)
		run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
		result, err := executePermitWorkerUnit(t, service, run)
		if err != nil || result.State != contract.RunStateSucceeded || result.UnknownEffectCount != 0 {
			t.Fatalf("direct-read settlement = %#v error %v", result, err)
		}
		if peerCalls.Load() != 1 || dialer.calls.Load() != 1 {
			t.Fatalf("provider calls/dials = %d/%d; want 1/1", peerCalls.Load(), dialer.calls.Load())
		}
		refs := workerResultReferences(t, fixture, admitted.RunRef)
		if len(refs) != 1 {
			t.Fatalf("result references = %v; want one immutable shared artifact", refs)
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
		var got contract.ActionResult
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatal(err)
		}
		assertHTTPDirectReadExactResult(t, got)
	})

	t.Run("current denial is receipt-bound unsent and prevents provider IO", func(t *testing.T) {
		var peerCalls atomic.Int32
		origin, tlsConfig, dialer := newHTTPDirectReadTLSPeer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			peerCalls.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"big":9007199254740993,"nested":{"empty":"","flag":false},"null":null}`)
		}))
		entered := make(chan struct{})
		release := make(chan struct{})
		var gateOnce sync.Once
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		clock := func() time.Time {
			gateOnce.Do(func() {
				close(entered)
				<-release
			})
			return time.Now().UTC()
		}
		selection := httpDirectReadSelection("fixture.http.direct-read.denied", origin, http.StatusOK, false)
		policy := httpDirectReadPolicy(origin)
		fixture := newHTTPDirectReadFixture(t, selection, policy, app.HTTPDirectReadDependencies{ClientIdentity: testClientIdentity,
			Resolver: httpDirectReadResolver{}, Dialer: dialer, TLSConfig: tlsConfig, Now: clock,
		}, nil)
		service := newPermitWorkerService(t, fixture, temporal)
		admitted := admitHTTPDirectReadRun(
			t, fixture, selection, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
			"http-direct-read-current-denial",
		)
		run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
		completed := make(chan permitExecutionResult, 1)
		go func() {
			result, err := executePermitWorkerUnitContext(context.Background(), service, run)
			completed <- permitExecutionResult{result: result, err: err}
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("HTTP runtime did not reach controlled pre-admission clock")
		}
		revokePermitTestAuthority(t, fixture, admitted.RunRef, "grant")
		releaseOnce.Do(func() { close(release) })
		var completedResult permitExecutionResult
		select {
		case completedResult = <-completed:
		case <-time.After(5 * time.Second):
			t.Fatal("denied direct read did not settle")
		}
		if completedResult.err != nil || completedResult.result.State != contract.RunStateBlocked ||
			completedResult.result.UnknownEffectCount != 0 {
			t.Fatalf("denied settlement = %#v error %v; want blocked known-unsent", completedResult.result, completedResult.err)
		}
		if peerCalls.Load() != 0 || dialer.calls.Load() != 0 {
			t.Fatalf("denied provider calls/dials = %d/%d; want 0/0", peerCalls.Load(), dialer.calls.Load())
		}
		outcomes, attempts := loadHTTPDirectReadSettlement(t, fixture, fixture.Scope, fixture.Actor, run)
		if len(outcomes) != 1 || len(attempts) != 1 || outcomes[0].State != contract.OutcomeUnsent ||
			!slices.Equal(outcomes[0].AssertionRefs, []string{attempts[0].ReceiptRef}) {
			t.Fatalf("denied outcomes = %#v attempts = %#v; want exact receipt-bound unsent", outcomes, attempts)
		}
	})

	t.Run("response loss remains unknown and cannot publish success", func(t *testing.T) {
		var peerCalls atomic.Int32
		origin, tlsConfig, dialer := newHTTPDirectReadTLSPeer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			peerCalls.Add(1)
			hijacker, ok := writer.(http.Hijacker)
			if !ok {
				t.Error("controlled TLS response writer cannot close the accepted request")
				return
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack controlled TLS connection: %v", err)
				return
			}
			_ = connection.Close()
		}))
		selection := httpDirectReadSelection("fixture.http.direct-read.unknown", origin, http.StatusOK, false)
		policy := httpDirectReadPolicy(origin)
		fixture := newHTTPDirectReadFixture(t, selection, policy, app.HTTPDirectReadDependencies{ClientIdentity: testClientIdentity,
			Resolver: httpDirectReadResolver{}, Dialer: dialer, TLSConfig: tlsConfig,
		}, nil)
		service := newPermitWorkerService(t, fixture, temporal)
		admitted := admitHTTPDirectReadRun(
			t, fixture, selection, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
			"http-direct-read-unknown-response",
		)
		run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
		result, err := executePermitWorkerUnit(t, service, run)
		if err != nil || result.State != contract.RunStateReconciling || result.UnknownEffectCount != 1 {
			t.Fatalf("unknown direct-read settlement = %#v error %v", result, err)
		}
		if peerCalls.Load() != 1 || dialer.calls.Load() != 1 {
			t.Fatalf("unknown provider calls/dials = %d/%d; want 1/1", peerCalls.Load(), dialer.calls.Load())
		}
		outcomes, attempts := loadHTTPDirectReadSettlement(t, fixture, fixture.Scope, fixture.Actor, run)
		if len(outcomes) != 1 || len(attempts) != 1 || outcomes[0].State != contract.OutcomeUnknown ||
			!slices.Equal(outcomes[0].AssertionRefs, []string{attempts[0].ReceiptRef}) {
			t.Fatalf("unknown outcomes = %#v attempts = %#v", outcomes, attempts)
		}
		unavailable := workerResultGET(
			t,
			fixture,
			"/v1/workspaces/"+fixture.Scope.WorkspaceID+"/runs/"+admitted.RunRef+"/result",
			http.StatusServiceUnavailable,
		)
		var problem struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(unavailable, &problem); err != nil || problem.Code != "capability_unavailable" {
			t.Fatalf("unknown response fabricated a result: payload=%s error=%v", unavailable, err)
		}
	})

	const cooldownBody = `{"big":9007199254740993,"nested":{"empty":"","flag":false},"null":null}`
	for _, responseCase := range []struct {
		name      string
		body      string
		truncated bool
	}{
		{"complete body", cooldownBody, false},
		{"truncated body", cooldownBody, true},
		{"invalid JSON", "{", false},
	} {
		t.Run("Retry-After blocks the next same-account alias before IO/"+responseCase.name, func(t *testing.T) {
			var peerCalls atomic.Int32
			origin, tlsConfig, dialer := newHTTPDirectReadTLSPeer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				peerCalls.Add(1)
				writer.Header().Set("Content-Type", "application/json")
				writer.Header().Set("Retry-After", "30")
				if responseCase.truncated {
					writer.Header().Set("Content-Length", fmt.Sprint(len(responseCase.body)+1))
				}
				writer.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(writer, responseCase.body)
			}))
			selection := httpDirectReadSelection("fixture.http.direct-read.cooldown", origin, http.StatusTooManyRequests, true)
			policy := httpDirectReadPolicy(origin)
			fixture := newHTTPDirectReadFixture(t, selection, policy, app.HTTPDirectReadDependencies{ClientIdentity: testClientIdentity,
				Resolver: httpDirectReadResolver{}, Dialer: dialer, TLSConfig: tlsConfig,
			}, nil)
			service := newPermitWorkerService(t, fixture, temporal)
			firstAdmission := admitHTTPDirectReadRun(
				t, fixture, selection, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
				"http-direct-read-cooldown-first",
			)
			first := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, firstAdmission)
			firstResult, err := executePermitWorkerUnit(t, service, first)
			if err != nil || firstResult.State != contract.RunStateReconciling || firstResult.UnknownEffectCount != 1 {
				t.Fatalf("429 direct-read settlement = %#v error %v; want deferred source-safe retry", firstResult, err)
			}
			firstOutcomes, _ := loadHTTPDirectReadSettlement(t, fixture, fixture.Scope, fixture.Actor, first)
			if len(firstOutcomes) != 1 || firstOutcomes[0].State != contract.OutcomeRejected {
				t.Fatalf("429 outcomes = %#v; want rejected", firstOutcomes)
			}

			foreignClient := fixture.ClientFor(t, fixture.ForeignActor)
			secondAdmission := admitHTTPDirectReadRun(
				t, fixture, selection, foreignClient, fixture.ForeignScope, fixture.ForeignConnection.ConnectionID,
				"http-direct-read-cooldown-alias",
			)
			second := preparePermitWorkerRun(
				t, fixture, service, temporal, fixture.ForeignScope, fixture.ForeignActor, secondAdmission,
			)
			_, secondErr := executePermitWorkerUnit(t, service, second)
			if !errors.Is(secondErr, store.ErrAccountCooldown) {
				t.Fatalf("same-account alias error = %v; want ErrAccountCooldown", secondErr)
			}
			_, secondAttempts := permitTestDurableCounts(t, fixture, fixture.ForeignScope, secondAdmission.RunRef)
			if peerCalls.Load() != 1 || dialer.calls.Load() != 1 || secondAttempts != 0 {
				t.Fatalf(
					"cooldown calls/dials/second attempts = %d/%d/%d; want 1/1/0",
					peerCalls.Load(), dialer.calls.Load(), secondAttempts,
				)
			}
		})
	}
}

type httpDirectReadResolver struct{}

func (httpDirectReadResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}

type httpDirectReadDialer struct {
	address string
	calls   atomic.Int32
}

func (dialer *httpDirectReadDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	dialer.calls.Add(1)
	return (&net.Dialer{}).DialContext(ctx, network, dialer.address)
}

func newHTTPDirectReadTLSPeer(
	t *testing.T,
	handler http.Handler,
) (string, *tls.Config, *httpDirectReadDialer) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatalf("generate controlled provider TLS key: %v", err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "provider.test"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"provider.test"},
	}
	certificateDER, err := x509.CreateCertificate(
		cryptorand.Reader, template, template, &key.PublicKey, key,
	)
	if err != nil {
		t.Fatalf("issue controlled provider TLS certificate: %v", err)
	}
	certificate, err := x509.ParseCertificate(certificateDER)
	if err != nil {
		t.Fatalf("parse controlled provider TLS certificate: %v", err)
	}
	peer := httptest.NewUnstartedServer(handler)
	peer.TLS = &tls.Config{
		MinVersion: tls.VersionTLS12,
		Certificates: []tls.Certificate{{
			Certificate: [][]byte{certificateDER},
			PrivateKey:  key,
		}},
		NextProtos: []string{"http/1.1"},
	}
	peer.StartTLS()
	t.Cleanup(peer.Close)
	parsed, err := url.Parse(peer.URL)
	if err != nil || parsed.Port() == "" {
		t.Fatalf("parse controlled provider address %q: %v", peer.URL, err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	return "https://provider.test:" + parsed.Port(),
		&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		&httpDirectReadDialer{address: peer.Listener.Addr().String()}
}

func httpDirectReadSelection(
	operationID string,
	origin string,
	statusCode int,
	retry bool,
) app.OperationSelection {
	operationSource := []contract.SourceRef{{SourceID: "http-direct-read-fixture", Pointer: "#/operations/read"}}
	responseSource := []contract.SourceRef{{SourceID: "http-direct-read-fixture", Pointer: "#/operations/read/responses"}}
	nested := contract.Schema{
		Type:     contract.SchemaTypeObject,
		Required: []string{"empty", "flag"},
		Properties: map[string]contract.Schema{
			"empty": {Type: contract.SchemaTypeString},
			"flag":  {Type: contract.SchemaTypeBool},
		},
	}
	responseSchema := contract.Schema{
		Type:     contract.SchemaTypeObject,
		Required: []string{"big", "nested", "null"},
		Properties: map[string]contract.Schema{
			"big":    {Type: contract.SchemaTypeInteger},
			"nested": nested,
			"null":   {Type: contract.SchemaTypeString, Nullable: true},
		},
	}
	execution := &contract.OperationExecution{
		HTTP: &contract.OperationHTTP{
			Origins: []string{origin},
			Witness: contract.ExecutionWitness{
				ProofID:    "http-direct-read-fixture-http",
				SourceRefs: operationSource,
			},
		},
	}
	if retry {
		execution.Retry = &contract.OperationRetry{
			Classification: contract.OperationRetrySafe,
			HTTPStatuses:   []uint16{uint16(statusCode)},
			Witness: contract.ExecutionWitness{
				ProofID:    "http-direct-read-fixture-retry",
				SourceRefs: responseSource,
			},
		}
	}
	return app.OperationSelection{
		Operation: contract.Operation{
			SchemaVersion:   contract.OperationSchemaVersion,
			OperationID:     operationID,
			SourceRefs:      operationSource,
			Transport:       contract.Transport{Kind: contract.TransportKindHTTP, Method: http.MethodGet, Path: "/fixture"},
			Inputs:          []contract.FieldBinding{},
			RequestVariants: []contract.RequestVariant{},
			Responses: []contract.Response{{
				Status: fmt.Sprintf("%d", statusCode), MediaType: "application/json",
				Schema: &responseSchema, SourceRefs: responseSource,
			}},
			CapabilityRequirements: httpDirectReadBaseCapabilities(),
			LaneEligibility:        httpDirectReadLaneEligibility(),
			Execution:              execution,
		},
		SchemaRefs: []string{"http-direct-read-schema-v1"},
	}
}

func httpDirectReadBaseCapabilities() []contract.CapabilityRequirement {
	selected := make([]contract.CapabilityRequirement, 0, 4)
	for _, capability := range contract.HTTPExecutionCapabilities() {
		switch capability.Capability {
		case "effect.authorize", "effect.limit", "http.execute", "value.serialize":
			selected = append(selected, capability)
		}
	}
	return selected
}

func httpDirectReadLaneEligibility() []contract.LaneEligibility {
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
		if lane == contract.LaneDirectRead {
			status = contract.LaneEligible
			reason = "shared-http-direct-read"
		}
		eligibility = append(eligibility, contract.LaneEligibility{Lane: lane, Status: status, Reason: reason})
	}
	return eligibility
}

func httpDirectReadPolicy(origin string) contract.HTTPExecutionPolicy {
	return contract.HTTPExecutionPolicy{
		MaxRequestBytes:  512,
		MaxResponseBytes: 512,
		MaxDNSAnswers:    1,
		MaxAttempts:      2,
		MaxRetryDelay:    time.Minute,
		SelectedOrigin:   origin,
	}
}

func httpDirectReadAdmissionPolicy() store.RunBudgetPolicy {
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

func newHTTPDirectReadFixture(
	t *testing.T,
	selection app.OperationSelection,
	policy contract.HTTPExecutionPolicy,
	dependencies app.HTTPDirectReadDependencies,
	configure testfixture.ConfigureService,
) *testfixture.Fixture {
	t.Helper()
	registry := &app.HandlerRegistry{}
	if err := registry.RegisterHTTP(app.NewHTTPDirectReadRegistration(dependencies)); err != nil {
		t.Fatalf("register production HTTP direct-read handler: %v", err)
	}
	var resolver app.OperationResolver
	fixture := testfixture.NewOperation(t, selection.Operation.OperationID, func(
		fixtureDependencies testfixture.ServiceDependencies,
		configuration *httpapi.ServiceConfig,
	) error {
		resolver = func(
			ctx context.Context,
			scope contract.Scope,
			connectionID string,
			operationID string,
		) (app.OperationSelection, error) {
			if ctx == nil || ctx.Err() != nil {
				return app.OperationSelection{}, context.Canceled
			}
			primary := scope.Equal(fixtureDependencies.Scope) && connectionID == fixtureDependencies.Connection.ConnectionID
			foreign := scope.Equal(fixtureDependencies.ForeignScope) && connectionID == fixtureDependencies.ForeignConnection.ConnectionID
			if operationID != selection.Operation.OperationID || !primary && !foreign {
				return app.OperationSelection{}, errors.New("controlled HTTP direct-read operation is unavailable")
			}
			return selection, nil
		}
		resolvePolicy := func(scope contract.Scope, connectionID string) (contract.HTTPExecutionPolicy, bool) {
			primary := scope.Equal(fixtureDependencies.Scope) && connectionID == fixtureDependencies.Connection.ConnectionID
			foreign := scope.Equal(fixtureDependencies.ForeignScope) && connectionID == fixtureDependencies.ForeignConnection.ConnectionID
			if !primary && !foreign {
				return contract.HTTPExecutionPolicy{}, false
			}
			return policy, true
		}
		admission, err := app.NewAdmissionService(app.AdmissionServiceConfig{
			Pool:              fixtureDependencies.Pool,
			DeploymentID:      fixtureDependencies.DeploymentID,
			Authorize:         configuration.Auth.AuthorizeResource,
			Registry:          registry,
			ResolveOperation:  resolver,
			ResolveHTTPPolicy: resolvePolicy,
			Policy:            httpDirectReadAdmissionPolicy(),
			Random:            cryptorand.Reader,
		})
		if err != nil {
			return err
		}
		configuration.Admission = admission
		if configure != nil {
			return configure(fixtureDependencies, configuration)
		}
		return nil
	})
	fixture.WorkerRegistry = registry
	fixture.ResolveOperation = resolver
	return fixture
}

func admitHTTPDirectReadRun(
	t *testing.T,
	fixture *testfixture.Fixture,
	selection app.OperationSelection,
	client *http.Client,
	scope contract.Scope,
	connectionID string,
	requestKey string,
) api.AdmissionResponse {
	t.Helper()
	revision, err := app.OperationSelectionRevision(selection)
	if err != nil {
		t.Fatalf("resolve HTTP direct-read operation revision: %v", err)
	}
	invocation := api.InvocationRequest{
		ConnectionRef:  connectionID,
		OperationRef:   selection.Operation.OperationID,
		Lane:           api.InvocationRequestLane(contract.LaneDirectRead),
		Input:          contract.NewObjectValue(map[string]contract.PresentValue{}),
		ResultControls: contract.ResultControls{},
		Protocol: api.ProtocolMetadata{
			WireVersion:      api.ProtocolMetadataWireVersion(api.WireVersion),
			RequiredFeatures: []string{},
			RequestKey:       requestKey,
			IntentDigest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			ExpectedRevision: revision,
		},
	}
	response := submitInitialInvocation(t, fixture, client, scope.WorkspaceID, invocation)
	return requireFreshInitialAdmission(t, response, requestKey)
}

func loadHTTPDirectReadSettlement(
	t *testing.T,
	fixture *testfixture.Fixture,
	scope contract.Scope,
	actor contract.Actor,
	run permitWorkerRun,
) ([]contract.MemberOutcome, []contract.Attempt) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var outcomes []contract.MemberOutcome
	var attempts []contract.Attempt
	err := store.WithinScope(ctx, fixture.Pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		dispatch, err := store.LoadDispatch(ctx, tx, store.TaskRef{RunID: run.reference.RunID, TaskID: run.plan.TaskID}, run.plan.Dispatch.DispatchID)
		if err != nil {
			return err
		}
		outcomes, err = store.ListMemberOutcomes(ctx, tx, store.TaskRef{
			RunID: run.reference.RunID, TaskID: run.plan.TaskID,
		}, dispatch.Intent.DispatchID)
		if err != nil {
			return err
		}
		attempts = append([]contract.Attempt(nil), dispatch.Attempts...)
		return nil
	})
	if err != nil {
		t.Fatalf("load HTTP direct-read settlement: %v", err)
	}
	return outcomes, attempts
}

func assertHTTPDirectReadExactResult(t *testing.T, result contract.ActionResult) {
	t.Helper()
	if err := result.Validate(); err != nil || result.MediaType != "application/json" || result.Pagination != nil {
		t.Fatalf("published direct-read result = %#v error %v", result, err)
	}
	fields, ok := result.Data.Fields()
	if !ok || len(fields) != 3 {
		t.Fatalf("published direct-read fields = %#v", result.Data)
	}
	bigValue, ok := fields["big"].ExactNumber()
	if !ok || bigValue.Text != "9007199254740993" {
		t.Fatalf("published exact integer = %#v", fields["big"])
	}
	nested, ok := fields["nested"].Fields()
	if !ok || len(nested) != 2 {
		t.Fatalf("published nested object = %#v", fields["nested"])
	}
	empty, emptyOK := nested["empty"].TextValue()
	flag, flagOK := nested["flag"].Boolean()
	if !emptyOK || empty != "" || !flagOK || flag {
		t.Fatalf("published nested values = %#v", nested)
	}
	if fields["null"].Presence() != contract.PresenceNull {
		t.Fatalf("published null presence = %q", fields["null"].Presence())
	}
}
