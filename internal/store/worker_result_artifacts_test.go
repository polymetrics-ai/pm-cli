package store

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// This test owns PostgreSQL publication metadata and receipt behavior only.
// Actual shared-object byte durability belongs to the selected object fixture.
func TestWorkerResultArtifactReceiptsControlRunProjection(t *testing.T) {
	credentials, pool := prepareTestStore(t, 8)
	scope := newTestScope(t)
	foreignScope := newTestScope(t)
	t.Cleanup(func() { cleanupTestScope(t, credentials, scope, foreignScope) })

	primary := testAdmissionRequest(t, scope, "worker-result-primary", "worker-result-primary")
	secondary := testAdmissionRequest(t, scope, "worker-result-secondary", "worker-result-secondary")
	foreign := testAdmissionRequest(t, foreignScope, "worker-result-foreign", "worker-result-foreign")
	registerRunViewFixture(t, pool, scope, primary)
	registerRunViewFixture(t, pool, scope, secondary)
	registerRunViewFixture(t, pool, foreignScope, foreign)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	otherConnectionID := "worker-result-other-connection"
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return RegisterConnection(ctx, tx, Connection{
			Scope:             scope,
			ConnectionID:      otherConnectionID,
			ConnectorID:       "run-view-connector",
			ProviderAccountID: "provider-" + primary.Spec.RunID,
			Revision:          1,
		})
	}); err != nil {
		t.Fatalf("register same-scope foreign connection: %v", err)
	}

	primaryZ := prepareWorkerResultSettlement(t, ctx, pool, scope, primary, "primary-z")
	primaryA := prepareWorkerResultSettlement(t, ctx, pool, scope, primary, "primary-a")
	primaryDuplicate := prepareWorkerResultSettlement(t, ctx, pool, scope, primary, "primary-duplicate")
	secondaryM := prepareWorkerResultSettlement(t, ctx, pool, scope, secondary, "secondary-m")

	limits := ObjectCapacityLimits{
		OrganizationBytes: 4096, WorkspaceBytes: 4096,
		OrganizationAttempts: 32, WorkspaceAttempts: 32,
	}
	artifactA := newWorkerResultArtifact(t, scope, primary.InputSnapshot.ConnectionID, "a")
	artifactZ := newWorkerResultArtifact(t, scope, primary.InputSnapshot.ConnectionID, "z")
	unpublished := newWorkerResultArtifact(t, scope, primary.InputSnapshot.ConnectionID, "unpublished")
	wrongConnection := newWorkerResultArtifact(t, scope, otherConnectionID, "wrong-connection")
	foreignArtifact := newWorkerResultArtifact(t, foreignScope, foreign.InputSnapshot.ConnectionID, "foreign")
	artifactM := newWorkerResultArtifact(t, scope, secondary.InputSnapshot.ConnectionID, "m")
	for _, fixture := range []struct {
		scope    contract.Scope
		artifact workerResultArtifactFixture
	}{
		{scope, artifactA},
		{scope, artifactZ},
		{scope, unpublished},
		{scope, wrongConnection},
		{foreignScope, foreignArtifact},
		{scope, artifactM},
	} {
		stageWorkerResultArtifact(t, ctx, pool, fixture.scope, fixture.artifact, limits)
	}
	publishWorkerResultArtifact(t, ctx, pool, scope, artifactA)
	publishWorkerResultArtifact(t, ctx, pool, scope, wrongConnection)
	publishWorkerResultArtifact(t, ctx, pool, foreignScope, foreignArtifact)
	registerUnpublishedWorkerResultArtifact(t, ctx, pool, scope, unpublished)

	// Publication alone is not run-result membership. Only a validated durable
	// worker settlement receipt can make the reference visible.
	assertWorkerResultProjection(t, ctx, pool, scope, primary.Spec.RunID, []string{})

	invalid := []struct {
		name       string
		artifactID string
		kind       string
	}{
		{name: "missing_artifact", artifactID: "worker-result-artifact-missing", kind: workerResultSettlementReceiptKind},
		{name: "registered_but_unpublished", artifactID: unpublished.reference.ArtifactID, kind: workerResultSettlementReceiptKind},
		{name: "wrong_run_connection", artifactID: wrongConnection.reference.ArtifactID, kind: workerResultSettlementReceiptKind},
		{name: "foreign_scope", artifactID: foreignArtifact.reference.ArtifactID, kind: workerResultSettlementReceiptKind},
		{name: "wrong_receipt_kind", artifactID: artifactA.reference.ArtifactID, kind: "settlement"},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			commit := primaryZ.commit(test.artifactID, test.kind, "invalid-"+test.name)
			err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
				_, err := CommitSettlement(ctx, tx, commit)
				return err
			})
			if !errors.Is(err, ErrImmutableConflict) {
				t.Fatalf("invalid worker result settlement = %v; want ErrImmutableConflict", err)
			}
			assertWorkerResultSettlementUnchanged(t, ctx, pool, scope, primaryZ)
		})
	}

	rollback := errors.New("controlled worker result rollback")
	primaryZCommit := primaryZ.commit(artifactZ.reference.ArtifactID, workerResultSettlementReceiptKind, "primary-z")
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		if _, err := CommitObjectPublication(ctx, tx, artifactZ.request.ReservationID, artifactZ.verified); err != nil {
			return err
		}
		if _, err := CommitSettlement(ctx, tx, primaryZCommit); err != nil {
			return err
		}
		view, err := LoadRunView(ctx, tx, primary.Spec.RunID)
		if err != nil {
			return err
		}
		if !slices.Equal(view.ResultRefs, []string{artifactZ.reference.ArtifactID}) {
			t.Fatalf("same-transaction result references = %v", view.ResultRefs)
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatalf("controlled result rollback = %v", err)
	}
	assertWorkerResultSettlementUnchanged(t, ctx, pool, scope, primaryZ)
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := LoadArtifactPublication(ctx, tx, artifactZ.request.ReservationID)
		return err
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rolled-back result publication became visible: %v", err)
	}

	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		if _, err := CommitObjectPublication(ctx, tx, artifactZ.request.ReservationID, artifactZ.verified); err != nil {
			return err
		}
		_, err := CommitSettlement(ctx, tx, primaryZCommit)
		return err
	}); err != nil {
		t.Fatalf("commit atomic worker result publication: %v", err)
	}
	commitWorkerResultSettlement(t, ctx, pool, scope, primaryA.commit(
		artifactA.reference.ArtifactID,
		workerResultSettlementReceiptKind,
		"primary-a",
	))
	commitWorkerResultSettlement(t, ctx, pool, scope, primaryDuplicate.commit(
		artifactZ.reference.ArtifactID,
		workerResultSettlementReceiptKind,
		"primary-duplicate",
	))
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		if _, err := CommitObjectPublication(ctx, tx, artifactM.request.ReservationID, artifactM.verified); err != nil {
			return err
		}
		_, err := CommitSettlement(ctx, tx, secondaryM.commit(
			artifactM.reference.ArtifactID,
			workerResultSettlementReceiptKind,
			"secondary-m",
		))
		return err
	}); err != nil {
		t.Fatalf("commit second run result publication: %v", err)
	}

	primaryRefs := []string{artifactA.reference.ArtifactID, artifactZ.reference.ArtifactID}
	assertWorkerResultProjection(t, ctx, pool, scope, primary.Spec.RunID, primaryRefs)
	assertWorkerResultProjection(t, ctx, pool, scope, secondary.Spec.RunID, []string{artifactM.reference.ArtifactID})

	allowedConnections := []string{primary.InputSnapshot.ConnectionID, secondary.InputSnapshot.ConnectionID}
	allowedOperations := []string{primary.Spec.OperationOrPlanRef.ID, secondary.Spec.OperationOrPlanRef.ID}
	slices.Sort(allowedConnections)
	slices.Sort(allowedOperations)
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		views, err := ListRunViews(ctx, tx, allowedConnections, allowedOperations, "", nil, 8)
		if err != nil {
			return err
		}
		if len(views) != 2 {
			t.Fatalf("authorized result run page length = %d; want 2", len(views))
		}
		byRun := make(map[string][]string, len(views))
		for _, view := range views {
			byRun[view.Record.Spec.RunID] = view.ResultRefs
		}
		if !slices.Equal(byRun[primary.Spec.RunID], primaryRefs) ||
			!slices.Equal(byRun[secondary.Spec.RunID], []string{artifactM.reference.ArtifactID}) {
			t.Fatalf("bulk result projections = %#v", byRun)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

type workerResultArtifactFixture struct {
	request   ObjectReservationRequest
	reference contract.ObjectRef
	verified  PreverifiedObjectRef
	binding   ObjectAttemptBinding
}

func newWorkerResultArtifact(
	t *testing.T,
	scope contract.Scope,
	connectionID string,
	label string,
) workerResultArtifactFixture {
	t.Helper()
	request := ObjectReservationRequest{
		ReservationID: "worker-result-reservation-" + label,
		ConnectionID:  connectionID,
		ArtifactID:    "worker-result-artifact-" + label,
		MaximumBytes:  256,
	}
	reference := testObjectRef(scope, request.ArtifactID, "worker-result-attempt-"+label, "worker-result-"+label)
	verified, err := NewPreverifiedObjectRef(reference, ObjectVerification{
		ObservedSHA256:            reference.SHA256,
		ObservedByteLength:        reference.ByteLength,
		ObservedBackendGeneration: reference.BackendGeneration,
	})
	if err != nil {
		t.Fatalf("construct worker result artifact metadata: %v", err)
	}
	return workerResultArtifactFixture{
		request:   request,
		reference: reference,
		verified:  verified,
		binding: ObjectAttemptBinding{
			Scope: scope, ArtifactID: reference.ArtifactID,
			ObjectAttemptKey: reference.ObjectAttemptKey, SHA256: reference.SHA256,
			ByteLength: reference.ByteLength, MediaType: reference.MediaType,
		},
	}
}

func stageWorkerResultArtifact(
	t *testing.T,
	ctx context.Context,
	pool *Pool,
	scope contract.Scope,
	artifact workerResultArtifactFixture,
	limits ObjectCapacityLimits,
) {
	t.Helper()
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		if _, err := ReserveObjectCapacity(ctx, tx, artifact.request, limits); err != nil {
			return err
		}
		_, err := BindObjectAttempt(ctx, tx, artifact.request.ReservationID, artifact.binding)
		return err
	}); err != nil {
		t.Fatalf("stage worker result artifact metadata: %v", err)
	}
}

func publishWorkerResultArtifact(
	t *testing.T,
	ctx context.Context,
	pool *Pool,
	scope contract.Scope,
	artifact workerResultArtifactFixture,
) {
	t.Helper()
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := CommitObjectPublication(ctx, tx, artifact.request.ReservationID, artifact.verified)
		return err
	}); err != nil {
		t.Fatalf("publish worker result artifact metadata: %v", err)
	}
}

func registerUnpublishedWorkerResultArtifact(
	t *testing.T,
	ctx context.Context,
	pool *Pool,
	scope contract.Scope,
	artifact workerResultArtifactFixture,
) {
	t.Helper()
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return RegisterArtifact(ctx, tx, artifact.request.ConnectionID, artifact.verified)
	}); err != nil {
		t.Fatalf("register unpublished worker result artifact metadata: %v", err)
	}
}

type workerResultSettlementFixture struct {
	original contract.Task
	lease    Lease
	dispatch contract.Dispatch
	outcome  contract.MemberOutcome
	settled  contract.Task
}

func prepareWorkerResultSettlement(
	t *testing.T,
	ctx context.Context,
	pool *Pool,
	scope contract.Scope,
	request AdmissionRequest,
	label string,
) workerResultSettlementFixture {
	t.Helper()
	observedAt := time.Now().UTC()
	accepted := testTask(request, "worker-result-task-"+label, observedAt)
	dispatch := testDispatchFor(t, request.Spec.RunID, "worker-result-"+label)
	outcome := contract.MemberOutcome{
		MemberID: dispatch.OrderedMemberIDs[0], EffectKey: dispatch.EffectKey,
		DispatchID: dispatch.DispatchID, State: contract.OutcomeSucceeded,
		ObservedAt: observedAt.Add(2 * time.Microsecond), AssertionRefs: []string{},
	}
	var lease Lease
	var running contract.Task
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		if err := CreateTask(ctx, tx, accepted); err != nil {
			return err
		}
		var err error
		lease, err = ClaimTask(ctx, tx, ClaimRequest{
			Task:    TaskRef{RunID: accepted.RunID, TaskID: accepted.TaskID},
			OwnerID: "worker-result-owner-" + label,
			TTL:     10 * time.Minute,
		})
		if err != nil {
			return err
		}
		running, err = StartWorkerTask(ctx, tx, lease, accepted, observedAt.Add(time.Microsecond))
		if err != nil {
			return err
		}
		return PrepareDispatch(ctx, tx, lease, dispatch)
	}); err != nil {
		t.Fatalf("prepare worker result settlement: %v", err)
	}
	settled, err := running.ApplyEffectSummary(
		contract.EffectSummary{Succeeded: 1}, false, "", outcome.ObservedAt,
	)
	if err != nil {
		t.Fatalf("construct settled worker result task: %v", err)
	}
	return workerResultSettlementFixture{
		original: running,
		lease:    lease,
		dispatch: dispatch,
		outcome:  outcome,
		settled:  settled,
	}
}

func (fixture workerResultSettlementFixture) commit(
	artifactID string,
	receiptKind string,
	label string,
) SettlementCommit {
	receipt := testReceipt("worker-result-receipt-"+label, receiptKind, fixture.original.RunID)
	receipt.Payload.References = append(receipt.Payload.References, contract.SafeReference{
		Name: WorkerResultArtifactReferenceName,
		Ref:  artifactID,
	})
	return SettlementCommit{
		Lease: fixture.lease, DispatchID: fixture.dispatch.DispatchID,
		Outcomes: []contract.MemberOutcome{fixture.outcome}, Task: fixture.settled,
		Receipt: receipt,
		Audit:   testAudit("worker-result-audit-"+label, "worker_member_settlement", fixture.original.RunID),
		Event:   testEvent("worker-result-event-"+label, "worker_member_settlement", fixture.original.RunID),
		Outbox:  testOutbox("worker-result-outbox-"+label, "worker_settlement_recorded", fixture.original.RunID),
	}
}

func commitWorkerResultSettlement(
	t *testing.T,
	ctx context.Context,
	pool *Pool,
	scope contract.Scope,
	commit SettlementCommit,
) {
	t.Helper()
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := CommitSettlement(ctx, tx, commit)
		return err
	}); err != nil {
		t.Fatalf("commit worker result settlement: %v", err)
	}
}

func assertWorkerResultSettlementUnchanged(
	t *testing.T,
	ctx context.Context,
	pool *Pool,
	scope contract.Scope,
	fixture workerResultSettlementFixture,
) {
	t.Helper()
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		stored, err := LoadTask(ctx, tx, fixture.lease.Task)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(stored, fixture.original) {
			t.Fatalf("refused result settlement changed task = %#v", stored)
		}
		outcomes, err := ListMemberOutcomes(ctx, tx, fixture.lease.Task, fixture.dispatch.DispatchID)
		if err != nil {
			return err
		}
		if len(outcomes) != 0 {
			t.Fatalf("refused result settlement retained outcomes = %#v", outcomes)
		}
		view, err := LoadRunView(ctx, tx, fixture.original.RunID)
		if err != nil {
			return err
		}
		if len(view.ResultRefs) != 0 {
			t.Fatalf("refused result settlement exposed references = %v", view.ResultRefs)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertWorkerResultProjection(
	t *testing.T,
	ctx context.Context,
	pool *Pool,
	scope contract.Scope,
	runID string,
	want []string,
) {
	t.Helper()
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		view, err := LoadRunView(ctx, tx, runID)
		if err != nil {
			return err
		}
		if !slices.Equal(view.ResultRefs, want) {
			t.Fatalf("run result references = %v; want %v", view.ResultRefs, want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
