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
	"strconv"
	"strings"
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
	read := pageCheckRead(t, `[{"id":1},{"id":2},{"id":3}]`)
	if state := httpDirectReadOutcomeState(read.execution, read.err == nil); state != contract.OutcomeSucceeded {
		t.Fatalf("provider member outcome of a 2xx page = %q, direct-read error = %v, continuation seal failed = %t; "+
			"the continuation's object-store publication must not decide the provider outcome",
			state, read.err, errors.Is(read.err, pmruntime.ErrContinuationRequired))
	}
	if read.result.Pagination == nil || !read.result.Pagination.Continuation.Equal(
		contract.NewStringValue(read.codec.reservation.Request.ArtifactID),
	) {
		t.Fatalf("page result continuation = %#v; want the reserved continuation identity", read.result.Pagination)
	}
}

// An envelope over its object-size bound is a publication failure. The page
// step keeps the provider outcome of the 2xx page. The publication step
// returns objects.ErrObjectLimit before any object-store I/O, and the worker
// then settles it as a publication failure.
func TestOversizedContinuationEnvelopeIsAPublicationFailure(t *testing.T) {
	records := make([]string, 0, 40)
	for id := 1; id <= 40; id++ {
		records = append(records, `{"id":`+strconv.Itoa(id)+`}`)
	}
	read := pageCheckRead(t, "["+strings.Join(records, ",")+"]")
	if state := httpDirectReadOutcomeState(read.execution, read.err == nil); state != contract.OutcomeSucceeded {
		t.Fatalf("provider member outcome of a 2xx page with an oversized continuation = %q, direct-read error = %v; "+
			"want succeeded", state, read.err)
	}
	if size := int64(len(read.codec.envelope)); size <= read.codec.service.maximumBytes {
		t.Fatalf("sealed continuation envelope = %d bytes; want more than the %d byte bound",
			size, read.codec.service.maximumBytes)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	publication, err := read.codec.publishForResult(ctx, &read.result)
	if !errors.Is(err, objects.ErrObjectLimit) || publication != nil {
		t.Fatalf("publication of an oversized continuation = %#v error=%v; want objects.ErrObjectLimit", publication, err)
	}
}

// pageCheckObservation is one controlled page step: the transport execution,
// the direct-read result and error, and the worker codec that sealed it.
type pageCheckObservation struct {
	execution pmruntime.HTTPExecution
	result    contract.ActionResult
	err       error
	codec     *boundContinuationCodec
}

// pageCheckRead runs the real runtime page step and the real worker codec
// against a local provider that answers one 200 page with body. It fails the
// test unless exactly one 200 response arrived.
func pageCheckRead(t *testing.T, body string) pageCheckObservation {
	t.Helper()
	var providerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, body)
	}))
	t.Cleanup(server.Close)
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
	observed := pageCheckObservation{codec: codec}
	observed.result, observed.err = pmsync.DirectRead(ctx, pmsync.HTTPExecutorFunc(func(
		ctx context.Context,
		request pmruntime.HTTPRequest,
	) (pmruntime.HTTPExecution, error) {
		var executeErr error
		observed.execution, executeErr = pmruntime.ExecuteHTTP(ctx, dependencies, request)
		return observed.execution, executeErr
	}), request)
	execution := observed.execution
	if providerCalls.Load() != 1 || !execution.PhysicalSend ||
		execution.Disposition != pmruntime.DispositionResponded || execution.Response.StatusCode != http.StatusOK {
		t.Fatalf("controlled provider exchange = calls %d, send %t, disposition %q, status %d; want one 200 response",
			providerCalls.Load(), execution.PhysicalSend, execution.Disposition, execution.Response.StatusCode)
	}
	return observed
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

// The page step seals a continuation even when the provider callback context
// has already ended, and the object-store failure surfaces only from the
// publication step, as a publication error.
func TestContinuationPublicationFailureComesFromThePublicationStep(t *testing.T) {
	binding := pmruntime.ContinuationBinding{
		Scope:        contract.Scope{OrgID: "org-fixture", WorkspaceID: "workspace-fixture"},
		ConnectionID: "connection-fixture", OperationID: "fixture.http.page-check",
		OperationRevision: "operation-revision-fixture", RunID: "run-fixture",
	}
	codec := pageCheckUnpublishableCodec(t, binding)
	record := contract.NewObjectValue(map[string]contract.PresentValue{"id": pageCheckInteger(t, "3")})
	callback, endCallback := context.WithCancel(context.Background())
	endCallback()
	token, err := codec.SealContinuation(callback, binding, pmruntime.ContinuationState{
		MediaType: "application/json", UnreadTail: []contract.PresentValue{record},
		NextInputs: map[string]contract.PresentValue{},
	})
	if err != nil || !token.Equal(contract.NewStringValue(codec.reservation.Request.ArtifactID)) {
		t.Fatalf("seal after the provider callback ended = %#v error=%v; want the reserved identity", token, err)
	}
	result := &contract.ActionResult{
		MediaType: "application/json",
		Data:      contract.NewArrayValue([]contract.PresentValue{}),
		Pagination: &contract.ResultPagination{
			Continuation: token, UnreadTail: []contract.PresentValue{record},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	publication, err := codec.publishForResult(ctx, result)
	if err == nil || publication != nil {
		t.Fatalf("publication through an unavailable object store = %#v error=%v; want a publication error", publication, err)
	}
	if _, err := codec.publishForResult(ctx, result); !errors.Is(err, ErrContinuationInvalid) {
		t.Fatalf("second publication of one sealed continuation error = %v; want a refusal", err)
	}
}

// The publication step does not inherit the provider callback deadline, and
// it does not use the reserve that it leaves for the settlement, whether the
// activity attempt deadline or the accepted run deadline comes first. The
// check starts from the end of the publication step.
// TestProviderCallbackLeavesTheSettlementOneReserve checks the provider
// callback bound.
func TestWorkerPublicationContextIsNotTheTransportBound(t *testing.T) {
	// The settlement reserve is min(heartbeat/2, transport/2) = 1s.
	const heartbeat, transport, reserve = 2 * time.Second, 5 * time.Second, time.Second

	t.Run("publication is not bounded by the transport limit", func(t *testing.T) {
		attemptDeadline := time.Now().Add(time.Hour)
		attempt, cancelAttempt := context.WithDeadline(context.Background(), attemptDeadline)
		defer cancelAttempt()
		publication, cancelPublication := workerPublicationContext(
			attempt, attemptDeadline.Add(time.Hour), heartbeat, transport,
		)
		defer cancelPublication()
		if deadline, ok := publication.Deadline(); !ok || !deadline.Equal(attemptDeadline.Add(-2*reserve)) {
			t.Fatalf("publication deadline = %v %t; want two settlement reserves before %v", deadline, ok, attemptDeadline)
		}
	})

	// Each case puts both deadlines less than one transport limit ahead. A
	// settlement context made now then has the same deadline as one made at the
	// moment the publication step ends.
	for _, testCase := range []struct {
		name     string
		attempt  time.Duration
		accepted time.Duration
	}{
		{name: "attempt deadline first", attempt: 4 * time.Second, accepted: time.Hour},
		{name: "accepted run deadline first", attempt: 4500 * time.Millisecond, accepted: 2 * time.Second},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			now := time.Now()
			attempt, cancelAttempt := context.WithDeadline(context.Background(), now.Add(testCase.attempt))
			defer cancelAttempt()
			accepted := now.Add(testCase.accepted)
			publication, cancelPublication := workerPublicationContext(attempt, accepted, heartbeat, transport)
			defer cancelPublication()
			settlement, cancelSettlement := workerSettlementContext(attempt, accepted, heartbeat, transport)
			defer cancelSettlement()
			publicationEnd, publicationBounded := publication.Deadline()
			settlementEnd, settlementBounded := settlement.Deadline()
			if !publicationBounded || !settlementBounded || settlementEnd.Sub(publicationEnd) < reserve {
				t.Fatalf("settlement budget after the publication step = %v (publication ends %v, settlement ends %v); "+
					"want a positive budget of at least one %v reserve",
					settlementEnd.Sub(publicationEnd), publicationEnd.Sub(now), settlementEnd.Sub(now), reserve)
			}
		})
	}
}

// A provider callback that uses its whole bound must leave the settlement at
// least one settlement reserve, whichever of the accepted run deadline and the
// activity attempt deadline comes first. ExecuteUnit runs the callback, then
// the publication step, then the settlement. The check takes the callback
// deadline as the moment the callback ends: the latest end that its bound
// allows. It reads context deadlines only and does not wait.
//
// In each cutoff case the settlement cutoff is less than one transport limit
// ahead. The settlement deadline is then the cutoff, whether the worker makes
// the settlement context now or when the callback ends.
func TestProviderCallbackLeavesTheSettlementOneReserve(t *testing.T) {
	// The settlement reserve is min(heartbeat/2, transport/2) = 1s.
	const heartbeat, transport, reserve = 2 * time.Second, 5 * time.Second, time.Second

	for _, testCase := range []struct {
		name     string
		attempt  time.Duration
		accepted time.Duration // Zero for a reconcile callback: it has no accepted run deadline.
	}{
		{name: "accepted run deadline first", attempt: 4500 * time.Millisecond, accepted: 2 * time.Second},
		{name: "attempt deadline first", attempt: 4 * time.Second, accepted: time.Hour},
		{name: "reconcile callback", attempt: 4 * time.Second},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			now := time.Now()
			attempt, cancelAttempt := context.WithDeadline(context.Background(), now.Add(testCase.attempt))
			defer cancelAttempt()
			var accepted time.Time
			if testCase.accepted != 0 {
				accepted = now.Add(testCase.accepted)
			}
			callback, cancelCallback := workerCallbackContext(attempt, accepted, heartbeat, transport)
			defer cancelCallback()
			settlement, cancelSettlement := workerSettlementContext(attempt, accepted, heartbeat, transport)
			defer cancelSettlement()
			callbackEnd, callbackBounded := callback.Deadline()
			settlementEnd, settlementBounded := settlement.Deadline()
			if !callbackBounded || !settlementBounded || settlementEnd.Sub(callbackEnd) < reserve {
				t.Fatalf("settlement budget after a provider callback that uses its whole bound = %v "+
					"(callback ends %v, settlement ends %v); want at least one %v reserve",
					settlementEnd.Sub(callbackEnd), callbackEnd.Sub(now), settlementEnd.Sub(now), reserve)
			}
		})
	}

	// Far from both deadlines, the callback keeps its whole transport limit.
	t.Run("transport limit first", func(t *testing.T) {
		attempt, cancelAttempt := context.WithTimeout(context.Background(), time.Hour)
		defer cancelAttempt()
		before := time.Now()
		callback, cancelCallback := workerCallbackContext(attempt, before.Add(time.Hour), heartbeat, transport)
		defer cancelCallback()
		after := time.Now()
		callbackEnd, callbackBounded := callback.Deadline()
		if !callbackBounded || callbackEnd.Before(before.Add(transport)) || callbackEnd.After(after.Add(transport)) {
			t.Fatalf("callback deadline = %v %t; want one %v transport limit after the call",
				callbackEnd.Sub(before), callbackBounded, transport)
		}
	})
}

// No step of a unit may be planned to end after its task lease ends. The
// worker takes the limits of the provider callback, the publication step and
// the settlement from the lease context (workerLeaseContext). A caller's
// deadline can be later than the lease end: a direct caller can give a unit
// more time than the lease, and a caller can set no deadline. The settlement
// needs a current lease, so a step that ends after the lease makes the
// settlement fail with a stale task fence (issue #279). The check reads
// context deadlines only and does not wait.
//
// In each lease case the lease ends less than one transport limit ahead, so
// each deadline is set by the lease, whether the worker makes the context now
// or at the end of the step before it.
func TestUnitStepsEndBeforeTheTaskLeaseEnds(t *testing.T) {
	// The settlement reserve is min(heartbeat/2, transport/2) = 1s.
	const heartbeat, transport, reserve = 2 * time.Second, 5 * time.Second, time.Second

	for _, testCase := range []struct {
		name     string
		caller   time.Duration // Zero: the caller sets no deadline.
		accepted time.Duration // Zero: a reconcile unit has no accepted run deadline.
	}{
		{name: "caller deadline after the lease", caller: 15 * time.Second, accepted: time.Hour},
		{name: "no caller deadline", accepted: time.Hour},
		{name: "reconcile unit with a caller deadline after the lease", caller: 15 * time.Second},
		{name: "reconcile unit with no caller deadline"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			now := time.Now()
			lease := now.Add(4 * time.Second)
			caller, cancelCaller := context.WithCancel(context.Background())
			if testCase.caller != 0 {
				caller, cancelCaller = context.WithDeadline(context.Background(), now.Add(testCase.caller))
			}
			defer cancelCaller()
			var accepted time.Time
			if testCase.accepted != 0 {
				accepted = now.Add(testCase.accepted)
			}
			unit, cancelUnit := workerLeaseContext(caller, lease)
			defer cancelUnit()
			callback, cancelCallback := workerCallbackContext(unit, accepted, heartbeat, transport)
			defer cancelCallback()
			publication, cancelPublication := workerPublicationContext(unit, accepted, heartbeat, transport)
			defer cancelPublication()
			settlement, cancelSettlement := workerSettlementContext(unit, accepted, heartbeat, transport)
			defer cancelSettlement()
			for _, step := range []struct {
				name string
				ctx  context.Context
				want time.Time
			}{
				{name: "provider callback", ctx: callback, want: lease.Add(-2 * reserve)},
				{name: "publication step", ctx: publication, want: lease.Add(-2 * reserve)},
				{name: "settlement", ctx: settlement, want: lease.Add(-reserve)},
			} {
				if deadline, ok := step.ctx.Deadline(); !ok || !deadline.Equal(step.want) {
					t.Errorf("%s deadline = %v (bounded %t); want %v, before the lease end at %v",
						step.name, deadline.Sub(now), ok, step.want.Sub(now), lease.Sub(now))
				}
			}
		})
	}

	// In the Temporal activity the attempt deadline comes before the lease end:
	// the claim follows the start of the attempt, and both use the sealed
	// start_to_close. The lease context then keeps the exact attempt deadline,
	// so each step keeps the deadline that it has without the lease context.
	for _, testCase := range []struct {
		name    string
		attempt time.Duration
		lease   time.Duration
	}{
		{name: "attempt deadline 30 ms before the lease end", attempt: 9990 * time.Millisecond, lease: 10030 * time.Millisecond},
		{name: "attempt deadline near the cutoff", attempt: 3990 * time.Millisecond, lease: 4030 * time.Millisecond},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			now := time.Now()
			attemptDeadline := now.Add(testCase.attempt)
			attempt, cancelAttempt := context.WithDeadline(context.Background(), attemptDeadline)
			defer cancelAttempt()
			accepted := now.Add(time.Hour)
			unit, cancelUnit := workerLeaseContext(attempt, now.Add(testCase.lease))
			defer cancelUnit()
			if deadline, ok := unit.Deadline(); !ok || !deadline.Equal(attemptDeadline) {
				t.Fatalf("lease context deadline = %v (bounded %t); want the attempt deadline %v",
					deadline.Sub(now), ok, testCase.attempt)
			}
			for _, step := range []struct {
				name string
				make func(context.Context, time.Time, time.Duration, time.Duration) (context.Context, context.CancelFunc)
			}{
				{name: "provider callback", make: workerCallbackContext},
				{name: "publication step", make: workerPublicationContext},
				{name: "settlement", make: workerSettlementContext},
			} {
				before := time.Now()
				today, cancelToday := step.make(attempt, accepted, heartbeat, transport)
				leased, cancelLeased := step.make(unit, accepted, heartbeat, transport)
				after := time.Now()
				todayEnd, todayBounded := today.Deadline()
				leasedEnd, leasedBounded := leased.Deadline()
				cancelToday()
				cancelLeased()
				// A deadline set by the transport limit reads the clock, so the two
				// calls can differ by the time between them. Every other deadline
				// must be equal.
				transportBound := !todayEnd.Before(before.Add(transport)) && !todayEnd.After(after.Add(transport))
				if !todayBounded || !leasedBounded ||
					(!transportBound && !leasedEnd.Equal(todayEnd)) ||
					(transportBound && (leasedEnd.Before(before.Add(transport)) || leasedEnd.After(after.Add(transport)))) {
					t.Errorf("%s deadline with the lease context = %v (bounded %t); want the deadline without it, %v (bounded %t)",
						step.name, leasedEnd.Sub(now), leasedBounded, todayEnd.Sub(now), todayBounded)
				}
			}
		})
	}

	// The lease deadline adds the lease TTL to a local time from before the
	// claim. It compares no local time with a PostgreSQL time, so a PostgreSQL
	// clock one hour ahead does not move it.
	t.Run("lease deadline needs no clock comparison", func(t *testing.T) {
		claimStarted := time.Now()
		claimedAt := claimStarted.Add(time.Hour).Add(250 * time.Millisecond)
		lease := store.Lease{ClaimedAt: claimedAt, ExpiresAt: claimedAt.Add(10 * time.Second)}
		if deadline := workerLeaseDeadline(claimStarted, lease); !deadline.Equal(claimStarted.Add(10 * time.Second)) {
			t.Fatalf("lease deadline = %v after the claim start; want the 10s lease TTL", deadline.Sub(claimStarted))
		}
	})
}

func pageCheckInteger(t *testing.T, exact string) contract.PresentValue {
	t.Helper()
	value, err := contract.NewIntegerValue(exact)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
