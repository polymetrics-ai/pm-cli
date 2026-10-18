package app_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/credentials"
	pmruntime "github.com/polymetrics-ai/pm-cli/internal/runtime"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

const physicalBoundaryKeyRef = "physical-boundary-wrapping-key-v1"

type physicalBoundaryResolver struct{}

func (physicalBoundaryResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}

type physicalBoundaryDialer struct {
	address string
}

func (dialer physicalBoundaryDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, dialer.address)
}

type physicalBoundaryKeyLoader struct {
	key      []byte
	calls    atomic.Int32
	mu       sync.Mutex
	returned [][]byte
}

func (loader *physicalBoundaryKeyLoader) load(ctx context.Context, reference string) ([]byte, error) {
	if ctx == nil || ctx.Err() != nil || reference != physicalBoundaryKeyRef {
		return nil, errors.New("controlled wrapping key unavailable")
	}
	loader.calls.Add(1)
	key := slices.Clone(loader.key)
	loader.mu.Lock()
	loader.returned = append(loader.returned, key)
	loader.mu.Unlock()
	return key, nil
}

func (loader *physicalBoundaryKeyLoader) returnedKeysWiped() bool {
	loader.mu.Lock()
	defer loader.mu.Unlock()
	for _, key := range loader.returned {
		for _, value := range key {
			if value != 0 {
				return false
			}
		}
	}
	return true
}

type physicalBoundaryEvidence struct {
	forgedDenied    bool
	duplicateDenied bool
	secretMatched   bool
	physicalSend    bool
}

type physicalBoundaryReady struct {
	request     app.ExecutionRequest
	call        pmruntime.PhysicalCall
	admitter    pmruntime.PhysicalCallAdmitter
	credentials pmruntime.CredentialResolver
	err         error
}

type physicalBoundaryDenial struct {
	credentialErr error
	sendErr       error
}

func TestPhysicalCallBoundaryCurrentSingleUseAndSessionIndependence(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	const secret = "physical-boundary-credential-canary"
	key := []byte("0123456789abcdef0123456789abcdef")
	t.Cleanup(func() { clear(key) })
	loader := &physicalBoundaryKeyLoader{key: slices.Clone(key)}
	t.Cleanup(func() { clear(loader.key) })

	var peerCalls atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		peerCalls.Add(1)
		if request.Method != http.MethodGet || request.URL.Path != "/fixture" {
			t.Errorf("controlled provider request = %s %s; want GET /fixture", request.Method, request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`"ok"`))
	}))
	defer peer.Close()
	origin := physicalBoundaryOrigin(t, peer)

	var fixture *testfixture.Fixture
	evidence := make(chan physicalBoundaryEvidence, 1)
	fixture = testfixture.NewExecuting(
		t,
		func(ctx context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
			call, admitter, resolver, err := request.PhysicalCallAuthority()
			if err != nil {
				return app.ExecutionObservation{}, err
			}
			forgedCredential := call
			forgedCredential.TaskID += "-other"
			if material, forgedErr := resolver.ResolveCredential(ctx, forgedCredential); forgedErr == nil || material != nil ||
				loader.calls.Load() != 0 {
				if material != nil {
					material.Destroy()
				}
				return app.ExecutionObservation{}, errors.New("forged credential effect reached key resolution")
			}
			material, err := resolver.ResolveCredential(ctx, call)
			if err != nil {
				return app.ExecutionObservation{}, fmt.Errorf("resolve exact callback credential: %w", err)
			}
			secretMatched := false
			if err := material.Use(func(value []byte) error {
				secretMatched = string(value) == secret
				if !secretMatched {
					return errors.New("controlled credential bytes differ")
				}
				return nil
			}); err != nil {
				return app.ExecutionObservation{}, err
			}

			forgedDenied := true
			for _, forge := range physicalBoundaryForgeries(call) {
				if err := admitter.AdmitPhysicalCall(ctx, forge); err == nil {
					forgedDenied = false
					break
				}
			}
			selection, err := fixture.ResolveOperation(ctx, request.Scope, request.ConnectionID, request.Task.OperationID)
			if err != nil {
				return app.ExecutionObservation{}, err
			}
			execution, err := pmruntime.ExecuteHTTP(
				ctx,
				physicalBoundaryHTTPDependencies(admitter, peer),
				physicalBoundaryHTTPRequest(selection.Operation, origin, call, request),
			)
			if err != nil {
				return app.ExecutionObservation{}, err
			}
			_, duplicateErr := pmruntime.ExecuteHTTP(
				ctx,
				physicalBoundaryHTTPDependencies(admitter, peer),
				physicalBoundaryHTTPRequest(selection.Operation, origin, call, request),
			)
			evidence <- physicalBoundaryEvidence{
				forgedDenied: forgedDenied, duplicateDenied: errors.Is(duplicateErr, pmruntime.ErrAdmissionRequired),
				secretMatched: secretMatched, physicalSend: execution.PhysicalSend,
			}
			return successfulPermitObservation(request), nil
		},
		unexpectedPermitReconciliation,
	)
	seedPhysicalBoundaryCredential(t, fixture, key, []byte(secret))
	service := newPhysicalBoundaryWorkerService(t, fixture, temporal, loader.load)
	admitted := admitPermitRun(
		t, fixture, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
		"issue25-physical-boundary-current",
	)
	run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)

	expirePermitTestSessions(t, fixture, fixture.Actor)
	requirePermitTestSessionExpired(t, fixture)
	result, err := executePermitWorkerUnit(t, service, run)
	if err != nil {
		t.Fatalf("execute controlled physical callback: %v", err)
	}
	var observed physicalBoundaryEvidence
	select {
	case observed = <-evidence:
	default:
		t.Fatalf("worker returned without exercising the physical callback: %#v", result)
	}
	if result.State != contract.RunStateSucceeded || !observed.forgedDenied ||
		!observed.duplicateDenied || !observed.secretMatched || !observed.physicalSend {
		t.Fatalf("physical callback result = %#v evidence %#v", result, observed)
	}
	if peerCalls.Load() != 1 {
		t.Fatalf("controlled provider calls = %d; want exactly 1", peerCalls.Load())
	}
	if loader.calls.Load() != 1 || !loader.returnedKeysWiped() {
		t.Fatalf("wrapping-key loads = %d wiped = %t; want 1/true", loader.calls.Load(), loader.returnedKeysWiped())
	}
}

func TestPhysicalCallBoundaryDeniesRevokedExpiredAndFencedAuthorityBeforeKeyOrSend(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	cases := []struct {
		name   string
		mutate func(*testing.T, *testfixture.Fixture, string, app.ExecutionRequest)
	}{
		{
			name: "revoked_grant",
			mutate: func(t *testing.T, fixture *testfixture.Fixture, runID string, _ app.ExecutionRequest) {
				revokePermitTestAuthority(t, fixture, runID, "grant")
			},
		},
		{
			name: "shared_account_cooldown",
			mutate: func(t *testing.T, fixture *testfixture.Fixture, _ string, request app.ExecutionRequest) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := store.WithinScope(ctx, fixture.Pool, request.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
					permit, err := store.LoadAccountPermit(ctx, tx, request.PermitRef)
					if err != nil {
						return err
					}
					return store.RecordAccountCooldown(ctx, tx, permit, permit.ReservedAt.Add(time.Hour))
				}); err != nil {
					t.Fatalf("record concurrent provider cooldown: %v", err)
				}
			},
		},
		{
			name: "expired_account_permit",
			mutate: func(t *testing.T, fixture *testfixture.Fixture, _ string, request app.ExecutionRequest) {
				fixture.SetupSQL(t, request.Scope, func(ctx context.Context, tx pgx.Tx) error {
					tag, err := tx.Exec(ctx, `
						UPDATE pm.account_permits
						SET expires_at = reserved_at + interval '1 microsecond'
						WHERE org_id = $1 AND workspace_id = $2 AND permit_id = $3
						  AND state = 'dispatched'
					`, request.Scope.OrgID, request.Scope.WorkspaceID, request.PermitRef)
					if err != nil {
						return err
					}
					if tag.RowsAffected() != 1 {
						return errors.New("controlled permit expiry changed no dispatched permit")
					}
					return nil
				})
			},
		},
		{
			name: "fenced_task_lease",
			mutate: func(t *testing.T, fixture *testfixture.Fixture, _ string, request app.ExecutionRequest) {
				fixture.SetupSQL(t, request.Scope, func(ctx context.Context, tx pgx.Tx) error {
					tag, err := tx.Exec(ctx, `
						UPDATE pm.task_leases
						SET fence = fence + 1
						WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND task_id = $4
					`, request.Scope.OrgID, request.Scope.WorkspaceID, request.Run.RunID, request.Task.TaskID)
					if err != nil {
						return err
					}
					if tag.RowsAffected() != 1 {
						return errors.New("controlled task fencing changed no lease")
					}
					return nil
				})
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			key := []byte("0123456789abcdef0123456789abcdef")
			t.Cleanup(func() { clear(key) })
			loader := &physicalBoundaryKeyLoader{key: slices.Clone(key)}
			t.Cleanup(func() { clear(loader.key) })
			var peerCalls atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				peerCalls.Add(1)
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`"unexpected"`))
			}))
			defer peer.Close()
			origin := physicalBoundaryOrigin(t, peer)
			ready := make(chan physicalBoundaryReady, 1)
			release := make(chan struct{})
			denied := make(chan physicalBoundaryDenial, 1)
			var fixture *testfixture.Fixture
			fixture = testfixture.NewExecuting(
				t,
				func(ctx context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
					call, admitter, resolver, authorityErr := request.PhysicalCallAuthority()
					ready <- physicalBoundaryReady{
						request: request, call: call, admitter: admitter, credentials: resolver, err: authorityErr,
					}
					if authorityErr != nil {
						return app.ExecutionObservation{}, authorityErr
					}
					select {
					case <-release:
					case <-ctx.Done():
						return app.ExecutionObservation{}, ctx.Err()
					}
					material, credentialErr := resolver.ResolveCredential(ctx, call)
					if material != nil {
						material.Destroy()
					}
					selection, resolveErr := fixture.ResolveOperation(
						ctx, request.Scope, request.ConnectionID, request.Task.OperationID,
					)
					var sendErr error
					if resolveErr != nil {
						sendErr = resolveErr
					} else {
						_, sendErr = pmruntime.ExecuteHTTP(
							ctx,
							physicalBoundaryHTTPDependencies(admitter, peer),
							physicalBoundaryHTTPRequest(selection.Operation, origin, call, request),
						)
					}
					denied <- physicalBoundaryDenial{credentialErr: credentialErr, sendErr: sendErr}
					return app.ExecutionObservation{}, errors.Join(credentialErr, sendErr)
				},
				unexpectedPermitReconciliation,
			)
			seedPhysicalBoundaryCredential(t, fixture, key, []byte("denied-credential-canary"))
			service := newPhysicalBoundaryWorkerService(t, fixture, temporal, loader.load)
			admitted := admitPermitRun(
				t, fixture, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
				"issue25-physical-boundary-denied-"+test.name,
			)
			run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
			completed := make(chan permitExecutionResult, 1)
			go func() {
				result, err := executePermitWorkerUnitContext(context.Background(), service, run)
				completed <- permitExecutionResult{result: result, err: err}
			}()
			var callback physicalBoundaryReady
			select {
			case callback = <-ready:
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not reach controlled physical boundary")
			}
			if callback.err != nil || callback.admitter == nil || callback.credentials == nil {
				close(release)
				<-completed
				t.Fatalf("fresh callback authority = %#v error %v", callback.call, callback.err)
			}
			test.mutate(t, fixture, admitted.RunRef, callback.request)
			close(release)
			var observed physicalBoundaryDenial
			select {
			case observed = <-denied:
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not report controlled physical denial")
			}
			if observed.credentialErr == nil || !errors.Is(observed.sendErr, pmruntime.ErrAdmissionRequired) {
				t.Fatalf("credential error = %v send error = %v; want both denied", observed.credentialErr, observed.sendErr)
			}
			<-completed
			if loader.calls.Load() != 0 || peerCalls.Load() != 0 {
				t.Fatalf("denied callback accessed key/provider = %d/%d; want 0/0", loader.calls.Load(), peerCalls.Load())
			}
		})
	}
}

func TestReconciliationCallbackCannotAcquireFreshPhysicalAuthority(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	reconciliationAuthority := make(chan error, 1)
	interrupted := errors.New("controlled interruption before worker settlement")
	fixture := testfixture.NewExecuting(
		t,
		func(context.Context, app.ExecutionRequest) (app.ExecutionObservation, error) {
			panic(interrupted)
		},
		func(_ context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
			_, _, _, err := request.PhysicalCallAuthority()
			reconciliationAuthority <- err
			if err == nil {
				return app.ExecutionObservation{}, errors.New("reconciliation gained fresh-send authority")
			}
			return successfulPermitObservation(request), nil
		},
	)
	service := newPermitWorkerService(t, fixture, temporal)
	admitted := admitPermitRun(
		t, fixture, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
		"issue25-reconciliation-no-send",
	)
	run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
	var interruption any
	func() {
		defer func() { interruption = recover() }()
		_, _ = executePermitWorkerUnit(t, service, run)
	}()
	if interruption != interrupted {
		t.Fatalf("fresh callback interruption = %v; want the controlled pre-settlement interruption", interruption)
	}
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE pm.task_leases
			SET lease_expires_at = clock_timestamp() - interval '1 microsecond'
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND task_id = $4
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, run.reference.RunID, run.plan.TaskID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("controlled worker interruption did not expire exactly its own task lease")
		}
		return nil
	})
	second, err := executePermitWorkerUnit(t, service, run)
	if err != nil || second.State != contract.RunStateSucceeded {
		t.Fatalf("controlled reconciliation = %#v error %v", second, err)
	}
	select {
	case authorityErr := <-reconciliationAuthority:
		if !errors.Is(authorityErr, app.ErrWorkerAuthorization) {
			t.Fatalf("reconciliation physical authority error = %v; want ErrWorkerAuthorization", authorityErr)
		}
	default:
		t.Fatalf("worker returned without exercising its reconciliation callback: %#v", second)
	}
}

func physicalBoundaryForgeries(call pmruntime.PhysicalCall) []pmruntime.PhysicalCall {
	forgeries := make([]pmruntime.PhysicalCall, 0, 17)
	appendForgery := func(mutate func(*pmruntime.PhysicalCall)) {
		forged := call
		mutate(&forged)
		forgeries = append(forgeries, forged)
	}
	appendForgery(func(forged *pmruntime.PhysicalCall) { forged.Scope.WorkspaceID += "-other" })
	appendForgery(func(forged *pmruntime.PhysicalCall) { forged.ConnectionID += "-other" })
	appendForgery(func(forged *pmruntime.PhysicalCall) { forged.OperationID += "-other" })
	appendForgery(func(forged *pmruntime.PhysicalCall) { forged.OperationRevision += "-other" })
	appendForgery(func(forged *pmruntime.PhysicalCall) { forged.RunID += "-other" })
	appendForgery(func(forged *pmruntime.PhysicalCall) { forged.TaskID += "-other" })
	appendForgery(func(forged *pmruntime.PhysicalCall) { forged.ExecutionGrantRef += "-other" })
	appendForgery(func(forged *pmruntime.PhysicalCall) { forged.GrantEpoch++ })
	appendForgery(func(forged *pmruntime.PhysicalCall) {
		forged.DispatchID = contract.DispatchID(string(forged.DispatchID) + "-other")
	})
	appendForgery(func(forged *pmruntime.PhysicalCall) {
		forged.Attempt.AttemptID = contract.AttemptID(string(forged.Attempt.AttemptID) + "-other")
	})
	appendForgery(func(forged *pmruntime.PhysicalCall) {
		forged.Attempt.RecordedAt = forged.Attempt.RecordedAt.Add(time.Microsecond)
	})
	appendForgery(func(forged *pmruntime.PhysicalCall) { forged.PermitRef += "-other" })
	appendForgery(func(forged *pmruntime.PhysicalCall) { forged.AccountWitness += "-other" })
	appendForgery(func(forged *pmruntime.PhysicalCall) { forged.Budget.Revision++ })
	appendForgery(func(forged *pmruntime.PhysicalCall) { forged.Capacity.EncodedBytes++ })
	appendForgery(func(forged *pmruntime.PhysicalCall) { forged.Deadline = forged.Deadline.Add(-time.Microsecond) })
	appendForgery(func(forged *pmruntime.PhysicalCall) { forged.ReconciliationOnly = true })
	return forgeries
}

func physicalBoundaryHTTPDependencies(
	admitter pmruntime.PhysicalCallAdmitter,
	peer *httptest.Server,
) pmruntime.HTTPDependencies {
	return pmruntime.HTTPDependencies{ClientIdentity: testClientIdentity,
		Admitter: admitter,
		Resolver: physicalBoundaryResolver{},
		Dialer:   physicalBoundaryDialer{address: peer.Listener.Addr().String()},
	}
}

func physicalBoundaryHTTPRequest(
	operation contract.Operation,
	origin string,
	call pmruntime.PhysicalCall,
	request app.ExecutionRequest,
) pmruntime.HTTPRequest {
	execution := contract.OperationExecution{}
	if operation.Execution != nil {
		execution = *operation.Execution
	}
	execution.HTTP = &contract.OperationHTTP{
		Origins: []string{origin},
		Witness: contract.ExecutionWitness{
			ProofID:    "controlled-physical-boundary",
			SourceRefs: slices.Clone(operation.SourceRefs),
		},
	}
	operation.Execution = &execution
	return pmruntime.HTTPRequest{
		Operation: operation,
		Egress:    pmruntime.EgressPolicy{SelectedOrigin: origin, RequireTLS: false},
		Call:      call,
		Input:     request.Input,
		Controls:  request.Controls,
		Limits: pmruntime.CallLimits{
			MaxRequestBytes: 512, MaxResponseBytes: 256, MaxDNSAnswers: 1,
			AttemptNumber: 1, MaxAttempts: 1, MaxRetryDelay: time.Second,
		},
		Continuation: contract.AbsentValue(),
	}
}

func physicalBoundaryOrigin(t *testing.T, peer *httptest.Server) string {
	t.Helper()
	_, port, err := net.SplitHostPort(peer.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return "http://provider.test:" + port
}

func seedPhysicalBoundaryCredential(
	t *testing.T,
	fixture *testfixture.Fixture,
	key []byte,
	secret []byte,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := credentials.StoreEncryptedCredential(
		ctx,
		fixture.Pool,
		func(ctx context.Context, reference string) ([]byte, error) {
			if ctx == nil || ctx.Err() != nil || reference != physicalBoundaryKeyRef {
				return nil, errors.New("controlled wrapping key unavailable")
			}
			return key, nil
		},
		func(_ context.Context, _ *store.Transaction, access credentials.Access) error {
			if access.Action != credentials.ActionManage || access.Stage != credentials.StageMutation ||
				!access.Scope.Equal(fixture.Scope) || !access.Actor.Equal(fixture.Actor) ||
				access.Connection.ConnectionID != fixture.Connection.ConnectionID || access.Effect != nil {
				return errors.New("controlled credential mutation authority differs")
			}
			return nil
		},
		credentials.StoreRequest{
			Scope: fixture.Scope, Actor: fixture.Actor,
			ConnectionID: fixture.Connection.ConnectionID, CredentialRef: fixture.Connection.CredentialRef,
			IdempotencyKey: "physical-boundary-credential-v1", KeyRef: physicalBoundaryKeyRef,
			Scopes: []string{"fixture.read"}, AuthEpoch: 1, Secret: secret,
		},
	)
	if err != nil {
		t.Fatalf("seed controlled credential: %v", err)
	}
}

// TestCredentialChannelAttachmentBoundaries pins the whole server-held
// credential path at the worker boundary: a declared channel is served by the
// same reference the worker resolves after its grant checks, a reference that
// is absent or no longer current is refused before any key, ciphertext or
// provider byte is touched, an unapproved origin never sees a credential, and
// an operation that declares no channel attaches nothing.
func TestCredentialChannelAttachmentBoundaries(t *testing.T) {
	const secret = "credential-channel-fixture-canary"
	cases := []struct {
		slug             string
		name             string
		declared         bool
		seedCredential   bool
		revokeCredential bool
		revokeConnection bool
		selectedOrigin   string
		wantError        error
		wantHeader       string
		wantPeerCalls    int64
		wantKeyLoads     int32
		wantPhysicalSend bool
		wantCallback     bool
	}{
		{
			slug: "attached", name: "declared channel attaches the server-held reference after grant checks",
			declared: true, seedCredential: true, wantCallback: true,
			wantHeader: "Bearer " + secret, wantPeerCalls: 1, wantKeyLoads: 1, wantPhysicalSend: true,
		},
		{
			slug: "missing", name: "missing credential reference is refused before key resolution or send",
			declared: true, wantCallback: true, wantError: pmruntime.ErrCredentialRequired,
		},
		{
			slug: "revoked-credential", name: "credential no longer active is refused before key resolution or send",
			declared: true, seedCredential: true, revokeCredential: true, wantCallback: true,
			wantError: pmruntime.ErrCredentialRequired,
		},
		{
			slug: "revoked-connection", name: "revoked connection never reaches the credential boundary",
			declared: true, seedCredential: true, revokeConnection: true,
		},
		{
			slug: "unapproved-origin", name: "unapproved origin is refused before credential resolution or send",
			declared: true, seedCredential: true, selectedOrigin: "http://unapproved.invalid", wantCallback: true,
			wantError: pmruntime.ErrDestinationDenied,
		},
		{
			slug: "undeclared", name: "operation without a declared channel attaches nothing",
			seedCredential: true, wantCallback: true,
			wantPeerCalls: 1, wantPhysicalSend: true,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			temporal := newPermitTemporalHarness(t)
			key := []byte("0123456789abcdef0123456789abcdef")
			t.Cleanup(func() { clear(key) })
			loader := &physicalBoundaryKeyLoader{key: slices.Clone(key)}
			t.Cleanup(func() { clear(loader.key) })

			var peerCalls atomic.Int64
			var observedHeader atomic.Value
			observedHeader.Store("")
			peer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				peerCalls.Add(1)
				observedHeader.Store(request.Header.Get("Authorization"))
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`"ok"`))
			}))
			defer peer.Close()
			origin := physicalBoundaryOrigin(t, peer)

			type credentialChannelEvidence struct {
				sendErr error
				send    bool
			}
			evidence := make(chan credentialChannelEvidence, 1)
			var fixture *testfixture.Fixture
			fixture = testfixture.NewExecuting(
				t,
				func(ctx context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
					call, admitter, resolver, err := request.PhysicalCallAuthority()
					if err != nil {
						return app.ExecutionObservation{}, err
					}
					// Grant checks first: a callback whose identity differs never
					// reaches the server-held reference.
					forged := call
					forged.TaskID += "-other"
					material, forgedErr := resolver.ResolveCredential(ctx, forged)
					if material != nil {
						material.Destroy()
					}
					if forgedErr == nil {
						return app.ExecutionObservation{}, errors.New("forged credential effect reached the server-held reference")
					}
					selection, err := fixture.ResolveOperation(ctx, request.Scope, request.ConnectionID, request.Task.OperationID)
					if err != nil {
						return app.ExecutionObservation{}, err
					}
					httpRequest := physicalBoundaryHTTPRequest(selection.Operation, origin, call, request)
					if test.declared {
						httpRequest = physicalBoundaryAuthorizationRequest(
							selection.Operation, origin, call, request,
							&contract.OperationAuthorization{
								Required: true, Audience: origin, Placement: contract.FieldLocationHeader,
								WireName: "Authorization", Prefix: "Bearer ", FieldID: "auth.fixture-bearer",
							},
						)
					}
					if test.selectedOrigin != "" {
						httpRequest.Egress.SelectedOrigin = test.selectedOrigin
					}
					execution, sendErr := pmruntime.ExecuteHTTP(
						ctx,
						physicalBoundaryCredentialHTTPDependencies(admitter, resolver, peer),
						httpRequest,
					)
					evidence <- credentialChannelEvidence{sendErr: sendErr, send: execution.PhysicalSend}
					if sendErr != nil {
						return app.ExecutionObservation{}, sendErr
					}
					return successfulPermitObservation(request), nil
				},
				unexpectedPermitReconciliation,
			)
			if test.seedCredential {
				seedPhysicalBoundaryCredential(t, fixture, key, []byte(secret))
			}
			service := newPhysicalBoundaryWorkerService(t, fixture, temporal, loader.load)
			admitted := admitPermitRun(
				t, fixture, fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
				"credential-channel-"+test.slug,
			)
			run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
			if test.revokeCredential {
				fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
					tag, err := tx.Exec(ctx, `
						UPDATE pm.credential_lifecycles
						SET status = 'reauthorization_required', revision = revision + 1,
						    updated_at = clock_timestamp()
						WHERE org_id = $1 AND workspace_id = $2 AND connection_id = $3
						  AND status = 'active'
					`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, fixture.Connection.ConnectionID)
					if err != nil {
						return err
					}
					if tag.RowsAffected() != 1 {
						return errors.New("controlled credential revocation changed no active lifecycle")
					}
					return nil
				})
			}
			if test.revokeConnection {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
					_, _, revokeErr := store.RevokeConnectionControl(ctx, tx, fixture.Connection.ConnectionID, 1)
					return revokeErr
				}); err != nil {
					t.Fatalf("revoke controlled connection: %v", err)
				}
			}
			_, _ = executePermitWorkerUnit(t, service, run)

			var observed credentialChannelEvidence
			reached := true
			if test.wantCallback {
				select {
				case observed = <-evidence:
				case <-time.After(5 * time.Second):
					t.Fatal("worker did not reach the controlled credential boundary")
				}
			} else {
				select {
				case observed = <-evidence:
				case <-time.After(time.Second):
					reached = false
				}
			}
			if reached != test.wantCallback {
				t.Fatalf("controlled credential boundary reached = %t; want %t", reached, test.wantCallback)
			}
			if reached {
				if !errors.Is(observed.sendErr, test.wantError) {
					t.Fatalf("controlled credential attempt error = %v; want %v", observed.sendErr, test.wantError)
				}
				if observed.send != test.wantPhysicalSend {
					t.Fatalf("physical send = %t; want %t", observed.send, test.wantPhysicalSend)
				}
			}
			if header := observedHeader.Load(); header != test.wantHeader {
				t.Fatalf("controlled peer Authorization header = %v; want %q", header, test.wantHeader)
			}
			if peerCalls.Load() != test.wantPeerCalls {
				t.Fatalf("controlled peer calls = %d; want %d", peerCalls.Load(), test.wantPeerCalls)
			}
			if loader.calls.Load() != test.wantKeyLoads {
				t.Fatalf("wrapping-key loads = %d; want %d", loader.calls.Load(), test.wantKeyLoads)
			}
			if test.wantKeyLoads != 0 && !loader.returnedKeysWiped() {
				t.Fatal("resolved wrapping key was retained after the attempt")
			}
		})
	}
}

// physicalBoundaryCredentialHTTPDependencies wires the worker's own credential
// boundary into the runtime, so the declared channel is served by the same
// server-held reference production resolves.
func physicalBoundaryCredentialHTTPDependencies(
	admitter pmruntime.PhysicalCallAdmitter,
	credentialResolver pmruntime.CredentialResolver,
	peer *httptest.Server,
) pmruntime.HTTPDependencies {
	dependencies := physicalBoundaryHTTPDependencies(admitter, peer)
	dependencies.Credentials = credentialResolver
	return dependencies
}

// physicalBoundaryAuthorizationRequest declares one source authorization channel
// on the controlled operation. The secret binding is the source-backed
// credential input the contract requires to match that declaration; it stays
// unsupplied by the caller, as a credential input must.
func physicalBoundaryAuthorizationRequest(
	operation contract.Operation,
	origin string,
	call pmruntime.PhysicalCall,
	request app.ExecutionRequest,
	authorization *contract.OperationAuthorization,
) pmruntime.HTTPRequest {
	httpRequest := physicalBoundaryHTTPRequest(operation, origin, call, request)
	httpRequest.Operation.Inputs = append(slices.Clone(operation.Inputs), contract.FieldBinding{
		FieldID: authorization.FieldID, WireName: authorization.WireName, Location: authorization.Placement,
		JSONPointer: "", Required: authorization.Required, Nullable: false, Style: "simple", Explode: false,
		AllowReserved: false, Sensitivity: contract.SensitivitySecret,
		Schema: contract.Schema{Type: contract.SchemaTypeString},
	})
	execution := *httpRequest.Operation.Execution
	httpFact := *execution.HTTP
	httpFact.Authorization = authorization
	execution.HTTP = &httpFact
	httpRequest.Operation.Execution = &execution
	return httpRequest
}

func newPhysicalBoundaryWorkerService(
	t *testing.T,
	fixture *testfixture.Fixture,
	temporal *permitTemporalHarness,
	loadKey app.WorkerCredentialKeyLoader,
) *app.WorkerService {
	t.Helper()
	service, err := app.NewWorkerService(app.WorkerServiceConfig{
		Pool: fixture.Pool, DeploymentID: fixture.DeploymentID,
		Registry: fixture.WorkerRegistry, ResolveOperation: fixture.ResolveOperation,
		Authorization: auth.NewResourceAuthorizer(), ObserveContinuation: temporal.observer,
		ActivityTimeouts: temporal.timeouts, LoadCredentialWrappingKey: loadKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}
