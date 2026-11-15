package app

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	pmruntime "github.com/polymetrics-ai/pm-cli/internal/runtime"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	pmsync "github.com/polymetrics-ai/pm-cli/internal/sync"
)

// This check needs no dependency stack. The provider is a local HTTP peer, the
// admission boundary is a fake, and the continuation codec is the real worker
// codec bound to an object-store publication that cannot complete: its
// PostgreSQL bind has no connection and its S3 endpoint is never reached.
//
// The page step must seal the continuation without object-store I/O. The
// provider member outcome of a 2xx page must not depend on the object store,
// because the worker publishes the continuation after the provider callback,
// as a separate concern from provider-effect truth.
func TestContinuationSealKeepsTheProviderOutcomeOfA2xxPage(t *testing.T) {
	var providerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `[{"id":1},{"id":2},{"id":3}]`)
	}))
	defer server.Close()
	served, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	origin := "http://provider.test:" + served.Port()
	operation := pageCheckOperation(origin)
	now := time.Now().UTC()
	request := pageCheckRequest(t, operation, origin, now)
	codec := pageCheckUnpublishableCodec(t, pmruntime.ContinuationBinding{
		Scope: request.Call.Scope, ConnectionID: request.Call.ConnectionID,
		OperationID: operation.OperationID, OperationRevision: request.Call.OperationRevision,
		RunID: request.Call.RunID,
	})
	dependencies := pmruntime.HTTPDependencies{
		ClientIdentity: pmruntime.ClientIdentity("pm-cli/test-fixture"),
		Admitter:       pageCheckAdmitter{},
		Continuations:  codec,
		Resolver:       pageCheckResolver{},
		Dialer:         pageCheckDialer{address: server.Listener.Addr().String()},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var execution pmruntime.HTTPExecution
	result, directErr := pmsync.DirectRead(ctx, pmsync.HTTPExecutorFunc(func(
		ctx context.Context,
		request pmruntime.HTTPRequest,
	) (pmruntime.HTTPExecution, error) {
		var executeErr error
		execution, executeErr = pmruntime.ExecuteHTTP(ctx, dependencies, request)
		return execution, executeErr
	}), request)

	if providerCalls.Load() != 1 || !execution.PhysicalSend ||
		execution.Disposition != pmruntime.DispositionResponded || execution.Response.StatusCode != http.StatusOK {
		t.Fatalf("controlled provider exchange = calls %d, send %t, disposition %q, status %d; want one 200 response",
			providerCalls.Load(), execution.PhysicalSend, execution.Disposition, execution.Response.StatusCode)
	}
	if state := httpDirectReadOutcomeState(execution, directErr == nil); state != contract.OutcomeSucceeded {
		t.Fatalf("provider member outcome of a 2xx page = %q, direct-read error = %v, continuation seal failed = %t; "+
			"the continuation's object-store publication must not decide the provider outcome",
			state, directErr, errors.Is(directErr, pmruntime.ErrContinuationRequired))
	}
	if result.Pagination == nil || !result.Pagination.Continuation.Equal(
		contract.NewStringValue(codec.reservation.Request.ArtifactID),
	) {
		t.Fatalf("page result continuation = %#v; want the reserved continuation identity", result.Pagination)
	}
}

func pageCheckOperation(origin string) contract.Operation {
	sources := []contract.SourceRef{{SourceID: "page-check-source", Pointer: "#/operation"}}
	record := contract.Schema{
		Type: contract.SchemaTypeObject, Required: []string{"id"},
		Properties: map[string]contract.Schema{"id": {Type: contract.SchemaTypeInteger}},
	}
	records := contract.Schema{Type: contract.SchemaTypeArray, Items: &record}
	return contract.Operation{
		SchemaVersion:   contract.OperationSchemaVersion,
		OperationID:     "fixture.http.page-check",
		SourceRefs:      sources,
		Transport:       contract.Transport{Kind: contract.TransportKindHTTP, Method: http.MethodGet, Path: "/items"},
		Inputs:          []contract.FieldBinding{},
		RequestVariants: []contract.RequestVariant{},
		Responses: []contract.Response{{
			Status: "200", MediaType: "application/json", Schema: &records, SourceRefs: sources,
		}},
		CapabilityRequirements: []contract.CapabilityRequirement{},
		LaneEligibility: []contract.LaneEligibility{{
			Lane: contract.LaneDirectRead, Status: contract.LaneEligible, Reason: "fixture",
		}},
		Execution: &contract.OperationExecution{HTTP: &contract.OperationHTTP{
			Origins: []string{origin},
			Witness: contract.ExecutionWitness{ProofID: "page-check-http", SourceRefs: sources},
		}},
	}
}

func pageCheckRequest(t *testing.T, operation contract.Operation, origin string, now time.Time) pmruntime.HTTPRequest {
	t.Helper()
	dispatchID, err := contract.NewDispatchID("dispatch-page-check")
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := contract.NewAttemptID("attempt-page-check")
	if err != nil {
		t.Fatal(err)
	}
	digest, err := contract.CanonicalDigest([]byte(`{"fixture":true}`))
	if err != nil {
		t.Fatal(err)
	}
	// The record cap retains the third record as an unread tail, so the page
	// step must seal a continuation.
	recordCap := uint64(2)
	return pmruntime.HTTPRequest{
		Operation: operation,
		Egress:    pmruntime.EgressPolicy{SelectedOrigin: origin},
		Call: pmruntime.PhysicalCall{
			Scope:        contract.Scope{OrgID: "org-fixture", WorkspaceID: "workspace-fixture"},
			ConnectionID: "connection-fixture", OperationID: operation.OperationID,
			OperationRevision: "operation-revision-fixture",
			RunID:             "run-fixture", TaskID: "task-fixture", ExecutionGrantRef: "grant-fixture", GrantEpoch: 1,
			DispatchID: dispatchID,
			Attempt: contract.Attempt{
				AttemptID: attemptID, DispatchID: dispatchID, ReceiptRef: "receipt-fixture",
				RecordedAt: now.Add(-time.Second),
			},
			PermitRef: "permit-fixture", AccountWitness: "account-fixture",
			Budget:   contract.BudgetBinding{BudgetRef: "budget-fixture", Revision: 1, Digest: digest},
			Capacity: contract.CapacityReservation{OutcomeSlots: 1, EncodedBytes: 1 << 20},
			Deadline: now.Add(time.Hour),
		},
		Input:    contract.NewObjectValue(map[string]contract.PresentValue{}),
		Controls: contract.ResultControls{TotalRecordCap: &recordCap},
		Limits: pmruntime.CallLimits{
			MaxRequestBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxDNSAnswers: 1,
			TotalRecordCap: &recordCap, AttemptNumber: 1, MaxAttempts: 1, MaxRetryDelay: time.Minute,
		},
		Continuation: contract.AbsentValue(),
	}
}

// pageCheckUnpublishableCodec returns the real worker continuation codec with a
// held continuation reservation. Its object-store publication cannot complete:
// the PostgreSQL pool has no connection, and the S3 endpoint has no listener.
func pageCheckUnpublishableCodec(t *testing.T, binding pmruntime.ContinuationBinding) *boundContinuationCodec {
	t.Helper()
	scratch := t.TempDir()
	if err := os.Chmod(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	backend, err := objects.NewS3(objects.S3Configuration{
		Endpoint: "https://127.0.0.1:9", Bucket: "pm-artifacts", Region: "us-east-1",
		RootCAs: x509.NewCertPool(), ScratchDirectory: scratch,
		Bounds: objects.Bounds{MaxObjectBytes: 4096, MaxInFlight: 1, RequestTimeout: time.Minute},
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
	unconnected := &store.Pool{}
	publisher, err := objects.NewPublisher(unconnected, backend, store.ObjectCapacityLimits{
		OrganizationBytes: 1 << 20, WorkspaceBytes: 1 << 20, OrganizationAttempts: 8, WorkspaceAttempts: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	principal := contract.Principal{
		OrgID: binding.Scope.OrgID, PrincipalID: "user-fixture", Kind: contract.PrincipalUser,
	}
	reservationID := "worker-continuation-v1-page-check"
	return &boundContinuationCodec{
		service: &ContinuationService{
			pool: unconnected, publisher: publisher, backend: backend, maximumBytes: 4096,
		},
		actor:   contract.Actor{RequestedActor: principal, EffectiveActor: principal},
		binding: binding,
		producer: continuationProducer{
			Input: contract.NewObjectValue(map[string]contract.PresentValue{}),
		},
		reservation: store.ObjectReservation{
			Request: store.ObjectReservationRequest{
				ReservationID: reservationID, ConnectionID: binding.ConnectionID, ArtifactID: reservationID,
				Purpose: store.ArtifactPurposeContinuation, MaximumBytes: 4096,
			},
			State: store.ObjectReservationHeld,
		},
		token: contract.AbsentValue(),
	}
}

type pageCheckAdmitter struct{}

func (pageCheckAdmitter) AdmitPhysicalCall(context.Context, pmruntime.PhysicalCall) error { return nil }

type pageCheckResolver struct{}

func (pageCheckResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}

type pageCheckDialer struct{ address string }

func (dialer pageCheckDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, dialer.address)
}
