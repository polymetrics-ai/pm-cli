package app_test

import (
	"context"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

// publicationCheckContinuationMediaType is the media type of the worker's
// continuation envelope object.
const publicationCheckContinuationMediaType = "application/vnd.polymetrics.pm-continuation+json"

// TestContinuationUploadCompletesAfterTheProviderCallback runs one worker unit
// whose continuation upload cannot complete while the provider callback
// context is live. The object store holds every PUT until the provider callback
// context has ended. The provider answers one valid 2xx page, so the unit must
// settle succeeded with its published result and continuation.
//
// PostgreSQL and Temporal are the real stack; the provider and the object store
// are controlled peers, and channels force the order.
func TestContinuationUploadCompletesAfterTheProviderCallback(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	objectStore := newGatedObjectStore(t)
	var providerCalls atomic.Int32
	origin, tlsConfig, dialer := newHTTPDirectReadTLSPeer(t, publicationCheckProvider(&providerCalls))
	authority := &durableContinuationAuthority{
		selection: durableHTTPContinuationSelection("fixture.http.publication-after-callback", origin),
		policy:    httpDirectReadPolicy(origin),
	}
	dependencies := app.HTTPDirectReadDependencies{ClientIdentity: testClientIdentity,
		Resolver: httpDirectReadResolver{}, Dialer: dialer, TLSConfig: tlsConfig,
	}
	fixture, _ := newDurableContinuationFixture(
		t, authority, dependencies, objectStore.resources, objectStore.resources, objectStore.limits,
	)
	registry, publisher := newCallbackGatedWorker(t, fixture, dependencies, objectStore)
	fixture.WorkerRegistry = registry
	service := workerResultService(t, fixture, temporal, publisher, durableContinuationObjectBytes)

	recordCap := uint64(2)
	admitted := admitDurableContinuationRun(
		t, fixture, authority.selection, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
		"publication-after-callback", durableContinuationInput(t, "chain", 1),
		contract.ResultControls{TotalRecordCap: &recordCap},
	)
	run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
	unit, err := executePermitWorkerUnit(t, service, run)
	if err != nil || unit.State != contract.RunStateSucceeded || unit.UnknownEffectCount != 0 || providerCalls.Load() != 1 {
		t.Fatalf("unit whose continuation upload completes after the provider callback = %#v error=%v provider_calls=%d",
			unit, err, providerCalls.Load())
	}
	if got := objectStore.completedPuts(); !slices.Equal(got, []string{publicationCheckContinuationMediaType, "application/json"}) {
		t.Fatalf("object writes after the provider callback = %q; want the continuation, then the result", got)
	}
	result := loadDurableContinuationResult(t, fixture, admitted.RunRef)
	assertDurableContinuationIDs(t, result, 1, 2)
	assertPrivateContinuationOrigin(t, fixture, requireDurableContinuationToken(t, result), admitted.RunRef)
}

// TestContinuationUploadFailureIsAPublicationFailure runs one worker unit whose
// continuation upload fails after the provider answered one valid 2xx page. The
// failure is a publication failure: the provider member outcome stays
// succeeded, the settlement records that the result publication was
// unavailable, and no result is published without its continuation.
func TestContinuationUploadFailureIsAPublicationFailure(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	objectStore := newGatedObjectStore(t)
	objectStore.refuse(publicationCheckContinuationMediaType)
	objectStore.open()
	var providerCalls atomic.Int32
	origin, tlsConfig, dialer := newHTTPDirectReadTLSPeer(t, publicationCheckProvider(&providerCalls))
	authority := &durableContinuationAuthority{
		selection: durableHTTPContinuationSelection("fixture.http.publication-failure", origin),
		policy:    httpDirectReadPolicy(origin),
	}
	fixture, publisher := newDurableContinuationFixture(t, authority, app.HTTPDirectReadDependencies{
		ClientIdentity: testClientIdentity, Resolver: httpDirectReadResolver{}, Dialer: dialer, TLSConfig: tlsConfig,
	}, objectStore.resources, objectStore.resources, objectStore.limits)
	service := workerResultService(t, fixture, temporal, publisher, durableContinuationObjectBytes)

	recordCap := uint64(2)
	admitted := admitDurableContinuationRun(
		t, fixture, authority.selection, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
		"publication-failure", durableContinuationInput(t, "chain", 1),
		contract.ResultControls{TotalRecordCap: &recordCap},
	)
	run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
	unit, err := executePermitWorkerUnit(t, service, run)
	if err != nil || unit.State != contract.RunStateSucceeded || unit.UnknownEffectCount != 0 || providerCalls.Load() != 1 {
		t.Fatalf("unit whose continuation upload failed = %#v error=%v provider_calls=%d; want the 2xx read to stay succeeded",
			unit, err, providerCalls.Load())
	}
	if refusals := objectStore.refusalCount(); refusals != 1 {
		t.Fatalf("refused continuation uploads = %d; want exactly one", refusals)
	}
	for _, outcome := range publicationCheckOutcomes(t, fixture, admitted.RunRef, run) {
		if outcome.State != contract.OutcomeSucceeded {
			t.Fatalf("provider member outcome after a failed continuation upload = %#v; want succeeded", outcome)
		}
	}
	reasons := publicationCheckSettlementReasons(t, fixture, admitted.RunRef)
	if !slices.Contains(reasons, "result_publication_unavailable") {
		t.Fatalf("settlement reason codes = %q; want result_publication_unavailable", reasons)
	}
	workerResultGET(t, fixture,
		"/v1/workspaces/"+fixture.Scope.WorkspaceID+"/runs/"+admitted.RunRef+"/result", http.StatusServiceUnavailable)
}

func publicationCheckProvider(calls *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"records":[{"id":1},{"id":2},{"id":3}],"last":false}`)
	})
}

// newCallbackGatedWorker registers the real HTTP direct-read handler with its
// own continuation service. Its execution callback opens the object-store gate
// only when the provider callback context has ended.
func newCallbackGatedWorker(
	t *testing.T,
	fixture *testfixture.Fixture,
	dependencies app.HTTPDirectReadDependencies,
	objectStore *gatedObjectStore,
) (*app.HandlerRegistry, *objects.Publisher) {
	t.Helper()
	publisher, err := objects.NewPublisher(fixture.Pool, objectStore.resources.Backend, objectStore.limits)
	if err != nil {
		t.Fatal(err)
	}
	continuations, err := app.NewContinuationService(
		fixture.Pool, publisher, objectStore.resources.Backend, durableContinuationObjectBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	dependencies.Continuations = continuations
	registration := app.NewHTTPDirectReadRegistration(dependencies)
	execute := registration.Execute
	registration.Execute = func(ctx context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
		go func() {
			<-ctx.Done()
			objectStore.open()
		}()
		return execute(ctx, request)
	}
	registry := &app.HandlerRegistry{}
	if err := registry.RegisterHTTP(registration); err != nil {
		t.Fatal(err)
	}
	return registry, publisher
}

func publicationCheckOutcomes(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID string,
	run permitWorkerRun,
) []contract.MemberOutcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var outcomes []contract.MemberOutcome
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(
		ctx context.Context,
		tx *store.Transaction,
	) error {
		var err error
		outcomes, err = store.ListMemberOutcomes(ctx, tx, store.TaskRef{
			RunID: runID, TaskID: run.plan.TaskID,
		}, run.plan.Dispatch.DispatchID)
		return err
	}); err != nil {
		t.Fatalf("load settled member outcomes: %v", err)
	}
	if len(outcomes) == 0 {
		t.Fatal("settled unit has no member outcomes")
	}
	return outcomes
}

func publicationCheckSettlementReasons(t *testing.T, fixture *testfixture.Fixture, runID string) []string {
	t.Helper()
	var reasons []string
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT array(SELECT jsonb_array_elements_text(receipt.payload->'reason_codes'))
			FROM pm.receipts AS receipt
			WHERE receipt.org_id = $1 AND receipt.workspace_id = $2 AND receipt.run_id = $3
			  AND receipt.kind = 'worker_settlement'
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID).Scan(&reasons)
	})
	return reasons
}

// gatedObjectStore is a controlled S3-compatible object store for the worker
// publication checks. It drives the real pinned SDK, the real publisher and the
// real byte verification. A PUT completes only after the gate is open, and the
// store can refuse every PUT of one media type. It is not selected-stack object
// storage.
type gatedObjectStore struct {
	resources *app.ArtifactResources
	limits    store.ObjectCapacityLimits

	gate     chan struct{}
	openOnce sync.Once

	mu       sync.Mutex
	objects  map[string]gatedObject
	refused  string
	refusals int
	puts     []string
}

type gatedObject struct {
	body      []byte
	mediaType string
}

const gatedObjectGeneration = `"controlled-generation"`

func newGatedObjectStore(t *testing.T) *gatedObjectStore {
	t.Helper()
	objectStore := &gatedObjectStore{
		limits: store.ObjectCapacityLimits{
			OrganizationBytes: 1 << 24, WorkspaceBytes: 1 << 24, OrganizationAttempts: 64, WorkspaceAttempts: 64,
		},
		gate:    make(chan struct{}),
		objects: make(map[string]gatedObject),
	}
	server := httptest.NewTLSServer(http.HandlerFunc(objectStore.serveHTTP))
	t.Cleanup(server.Close)
	// Cleanups run in reverse order: release a held PUT before the server closes.
	t.Cleanup(objectStore.open)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	scratch := t.TempDir()
	if err := os.Chmod(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	backend, err := objects.NewS3(objects.S3Configuration{
		Endpoint: server.URL, Bucket: "pm-artifacts", Region: "us-east-1",
		RootCAs: roots, ScratchDirectory: scratch,
		Bounds: objects.Bounds{MaxObjectBytes: 1 << 16, MaxInFlight: 4, RequestTimeout: 30 * time.Second},
		LoadCredentials: func(context.Context) (objects.Credentials, error) {
			return objects.Credentials{AccessKeyID: "controlled-access", SecretAccessKey: "controlled-secret"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	objectStore.resources = &app.ArtifactResources{Backend: backend, Limits: objectStore.limits}
	return objectStore
}

func (objectStore *gatedObjectStore) open() {
	objectStore.openOnce.Do(func() { close(objectStore.gate) })
}

func (objectStore *gatedObjectStore) refuse(mediaType string) {
	objectStore.mu.Lock()
	defer objectStore.mu.Unlock()
	objectStore.refused = mediaType
}

func (objectStore *gatedObjectStore) refusalCount() int {
	objectStore.mu.Lock()
	defer objectStore.mu.Unlock()
	return objectStore.refusals
}

func (objectStore *gatedObjectStore) completedPuts() []string {
	objectStore.mu.Lock()
	defer objectStore.mu.Unlock()
	return slices.Clone(objectStore.puts)
}

func (objectStore *gatedObjectStore) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodPut:
		body, err := io.ReadAll(io.LimitReader(request.Body, 1<<16+1))
		if err != nil || len(body) > 1<<16 {
			writeGatedObjectError(writer, http.StatusRequestEntityTooLarge, "EntityTooLarge")
			return
		}
		select {
		case <-objectStore.gate:
		case <-request.Context().Done():
			return
		}
		mediaType := request.Header.Get("Content-Type")
		objectStore.mu.Lock()
		if mediaType == objectStore.refused {
			objectStore.refusals++
			objectStore.mu.Unlock()
			writeGatedObjectError(writer, http.StatusInternalServerError, "InternalError")
			return
		}
		if _, exists := objectStore.objects[request.URL.Path]; exists || request.Header.Get("If-None-Match") != "*" {
			objectStore.mu.Unlock()
			writeGatedObjectError(writer, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		objectStore.objects[request.URL.Path] = gatedObject{body: body, mediaType: mediaType}
		objectStore.puts = append(objectStore.puts, mediaType)
		objectStore.mu.Unlock()
		writer.Header().Set("ETag", gatedObjectGeneration)
		writer.WriteHeader(http.StatusOK)
	case http.MethodGet:
		objectStore.mu.Lock()
		object, exists := objectStore.objects[request.URL.Path]
		objectStore.mu.Unlock()
		if !exists {
			writeGatedObjectError(writer, http.StatusNotFound, "NoSuchKey")
			return
		}
		if request.Header.Get("If-Match") != gatedObjectGeneration {
			writeGatedObjectError(writer, http.StatusPreconditionFailed, "PreconditionFailed")
			return
		}
		writer.Header().Set("ETag", gatedObjectGeneration)
		writer.Header().Set("Content-Type", object.mediaType)
		writer.Header().Set("Content-Length", strconv.Itoa(len(object.body)))
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(object.body)
	default:
		writeGatedObjectError(writer, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

func writeGatedObjectError(writer http.ResponseWriter, status int, code string) {
	writer.Header().Set("Content-Type", "application/xml")
	writer.WriteHeader(status)
	_, _ = fmt.Fprintf(writer, "<Error><Code>%s</Code></Error>", code)
}
