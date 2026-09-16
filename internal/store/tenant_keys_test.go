package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const (
	compositeIdentityID         = "ada"
	compositeMembershipID       = "membership-shared"
	compositeProviderAccount    = "account-shared"
	compositeConnectionID       = "connection-shared"
	compositePlanID             = "plan-shared"
	compositeApprovalID         = "approval-shared"
	compositeRunID              = "00000000-0000-4000-8000-000000000404"
	compositeTaskID             = "task-shared"
	compositeEventID            = "event-shared"
	compositeArtifactID         = "artifact-shared"
	compositeManifestID         = "manifest-shared"
	compositeMutationKey        = "identity-key-shared"
	compositeMutationTarget     = "membership-shared"
	compositeMutationAction     = "membership.grant"
	compositeAdmissionKey       = "admission-key-shared"
	compositeAdmissionAction    = "plan.execute"
	compositeAdmissionTarget    = "plan-shared"
	compositeForeignRunID       = "00000000-0000-4000-8000-000000000405"
	compositeApprovalCrossRunID = "00000000-0000-4000-8000-000000000406"
	compositeForeignConnectID   = "connection-foreign-only"
)

type compositeTenantFixture struct {
	workspace      WorkspaceIdentity
	identity       Identity
	membership     Membership
	mutation       IdentityMutation
	account        ProviderAccount
	connection     Connection
	planRevision   string
	planDigest     contract.SemanticDigest
	approvalDigest contract.SemanticDigest
	request        AdmissionRequest
	task           contract.Task
	artifact       contract.ObjectRef
}

func TestCompositeTenantReferences(t *testing.T) {
	credentials, pool := prepareTestStore(t, 4)
	alphaScope := newTestScope(t)
	betaScope := newTestScope(t)
	t.Cleanup(func() {
		cleanupTestScope(t, credentials, alphaScope, betaScope)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	alpha := newCompositeTenantFixture(t, alphaScope, "alpha")
	beta := newCompositeTenantFixture(t, betaScope, "beta")
	seedCompositeTenantFixture(t, ctx, pool, alpha)
	seedCompositeTenantFixture(t, ctx, pool, beta)
	seedForeignOnlyReferences(t, ctx, pool, beta)

	t.Run("correct_scope_resolves_each_persisted_binding", func(t *testing.T) {
		assertCompositeTenantFixture(t, ctx, pool, alpha)
		assertCompositeTenantFixture(t, ctx, pool, beta)
	})

	t.Run("equal_identity_mutation_and_admission_keys_remain_tenant_local", func(t *testing.T) {
		changed := alpha.mutation
		changed.IntentDigest = beta.mutation.IntentDigest
		changed.ResultRef = beta.mutation.ResultRef
		err := WithinScope(ctx, pool, alphaScope, testActor(alphaScope), func(ctx context.Context, tx *Transaction) error {
			_, err := RecordIdentityMutation(ctx, tx, changed)
			return err
		})
		if !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("changed alpha identity mutation = %v; want ErrIdempotencyConflict", err)
		}
		if err := WithinScope(ctx, pool, alphaScope, testActor(alphaScope), func(ctx context.Context, tx *Transaction) error {
			stored, err := LoadIdentityMutation(ctx, tx, alpha.mutation.Key)
			if err != nil {
				return err
			}
			if stored.IntentDigest != alpha.mutation.IntentDigest || stored.ResultRef != alpha.mutation.ResultRef {
				t.Fatalf("alpha mutation resolved foreign tenant state: %#v", stored)
			}
			admission, err := LookupAdmission(ctx, tx, alpha.request.Key)
			if err != nil {
				return err
			}
			if admission.RunID != compositeRunID || admission.IntentDigest != alpha.request.Spec.IdempotencyFingerprint {
				t.Fatalf("alpha admission key resolved foreign tenant state: %#v", admission)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("foreign_plan_and_approval_cannot_bind_a_local_run", func(t *testing.T) {
		cross := newCompositeOperationRequest(t, alphaScope, compositeForeignRunID, "cross-plan")
		err := WithinScope(ctx, pool, alphaScope, testActor(alphaScope), func(ctx context.Context, tx *Transaction) error {
			if _, err := AdmitRun(ctx, tx, cross); err != nil {
				return err
			}
			_, err := tx.exec(ctx, `
				INSERT INTO pm.run_plan_bindings (
					org_id, workspace_id, run_id, plan_id, plan_revision, approval_id
				) VALUES ($1, $2, $3, $4, $5, $6)
			`,
				alphaScope.OrgID,
				alphaScope.WorkspaceID,
				cross.Spec.RunID,
				compositePlanID,
				beta.planRevision,
				compositeApprovalID,
			)
			return err
		})
		requireSQLState(t, err, "23503")
		if err := WithinScope(ctx, pool, alphaScope, testActor(alphaScope), func(ctx context.Context, tx *Transaction) error {
			_, err := LoadRun(ctx, tx, cross.Spec.RunID)
			return err
		}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("failed composite plan binding left a run = %v; want ErrNotFound", err)
		}

		if err := WithinScope(ctx, pool, alphaScope, testActor(alphaScope), func(ctx context.Context, tx *Transaction) error {
			_, err := tx.exec(ctx, `
				INSERT INTO pm.plan_bindings (
					org_id, workspace_id, plan_id, plan_revision, binding_digest
				) VALUES ($1, $2, $3, $4, $5)
			`, alphaScope.OrgID, alphaScope.WorkspaceID, compositePlanID, beta.planRevision, string(testDigest("alpha-plan-for-approval-check")))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		approvalCross := newCompositeOperationRequest(t, alphaScope, compositeApprovalCrossRunID, "cross-approval")
		err = WithinScope(ctx, pool, alphaScope, testActor(alphaScope), func(ctx context.Context, tx *Transaction) error {
			if _, err := AdmitRun(ctx, tx, approvalCross); err != nil {
				return err
			}
			_, err := tx.exec(ctx, `
				INSERT INTO pm.run_plan_bindings (
					org_id, workspace_id, run_id, plan_id, plan_revision, approval_id
				) VALUES ($1, $2, $3, $4, $5, $6)
			`,
				alphaScope.OrgID,
				alphaScope.WorkspaceID,
				approvalCross.Spec.RunID,
				compositePlanID,
				beta.planRevision,
				compositeApprovalID,
			)
			return err
		})
		requireSQLState(t, err, "23503")
		if err := WithinScope(ctx, pool, alphaScope, testActor(alphaScope), func(ctx context.Context, tx *Transaction) error {
			_, err := LoadRun(ctx, tx, approvalCross.Spec.RunID)
			return err
		}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("failed composite approval binding left a run = %v; want ErrNotFound", err)
		}
	})

	t.Run("foreign_run_cannot_parent_a_local_task_or_event", func(t *testing.T) {
		candidate := alpha.task
		candidate.RunID = compositeForeignRunID
		candidate.TaskID = "task-cross-run"
		err := WithinScope(ctx, pool, alphaScope, testActor(alphaScope), func(ctx context.Context, tx *Transaction) error {
			return CreateTask(ctx, tx, candidate)
		})
		requireSQLState(t, err, "23503")

		err = WithinScope(ctx, pool, alphaScope, testActor(alphaScope), func(ctx context.Context, tx *Transaction) error {
			_, err := insertEvent(ctx, tx, compositeForeignRunID, 1, EventInput{
				EventID: "event-cross-run",
				Type:    "cross_run",
				Payload: contract.SafePayload{
					References:  []contract.SafeReference{},
					Counts:      []contract.SafeCount{},
					ReasonCodes: []string{},
				},
			}, time.Date(2026, time.September, 16, 0, 0, 0, 0, time.UTC))
			return err
		})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("foreign event parent = %v; want ErrNotFound", err)
		}
	})

	t.Run("foreign_artifact_connection_and_identity_are_scope_hidden", func(t *testing.T) {
		err := WithinScope(ctx, pool, alphaScope, testActor(alphaScope), func(ctx context.Context, tx *Transaction) error {
			foreign, err := NewPreverifiedObjectRef(beta.artifact, ObjectVerification{
				ObservedSHA256:            beta.artifact.SHA256,
				ObservedByteLength:        beta.artifact.ByteLength,
				ObservedBackendGeneration: beta.artifact.BackendGeneration,
			})
			if err != nil {
				return err
			}
			return RegisterArtifact(ctx, tx, foreign)
		})
		if !errors.Is(err, ErrScopeMismatch) {
			t.Fatalf("foreign artifact registration = %v; want ErrScopeMismatch", err)
		}

		err = WithinScope(ctx, pool, alphaScope, testActor(alphaScope), func(ctx context.Context, tx *Transaction) error {
			return RegisterConnection(ctx, tx, beta.connection)
		})
		if !errors.Is(err, ErrScopeMismatch) {
			t.Fatalf("foreign connection registration = %v; want ErrScopeMismatch", err)
		}
		err = WithinScope(ctx, pool, alphaScope, testActor(alphaScope), func(ctx context.Context, tx *Transaction) error {
			return RegisterIdentity(ctx, tx, beta.identity)
		})
		if !errors.Is(err, ErrScopeMismatch) {
			t.Fatalf("foreign identity registration = %v; want ErrScopeMismatch", err)
		}

		var foreignErr, missingErr error
		if err := WithinScope(ctx, pool, alphaScope, testActor(alphaScope), func(ctx context.Context, tx *Transaction) error {
			_, foreignErr = LoadConnection(ctx, tx, compositeForeignConnectID)
			_, missingErr = LoadConnection(ctx, tx, "connection-never-created")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(foreignErr, ErrNotFound) || !errors.Is(missingErr, ErrNotFound) || foreignErr.Error() != missingErr.Error() {
			t.Fatalf("foreign and absent connection errors differ: foreign=%v absent=%v", foreignErr, missingErr)
		}
	})

	t.Run("checkpoint_identity_and_manifest_advance_together", func(t *testing.T) {
		testCheckpointPublication(t, ctx, pool, alpha)
	})

	t.Run("task_lease_cannot_cross_equal_tenant_identity", func(t *testing.T) {
		testCrossScopeTaskLease(t, ctx, pool, alpha, beta)
	})

	t.Run("workspace_tombstone_retains_and_refuses_the_key_namespace", func(t *testing.T) {
		if err := WithinScope(ctx, pool, alphaScope, testActor(alphaScope), func(ctx context.Context, tx *Transaction) error {
			return RetireWorkspace(ctx, tx, alpha.workspace)
		}); err != nil {
			t.Fatal(err)
		}
		err := WithinScope(ctx, pool, alphaScope, testActor(alphaScope), func(ctx context.Context, tx *Transaction) error {
			_, err := tx.exec(ctx, `
				UPDATE pm.workspace_namespaces
				SET tombstoned_at = NULL
				WHERE org_id = $1 AND workspace_id = $2
			`, alphaScope.OrgID, alphaScope.WorkspaceID)
			return err
		})
		requireSQLState(t, err, "23514")
		err = WithinScope(ctx, pool, alphaScope, testActor(alphaScope), func(ctx context.Context, tx *Transaction) error {
			return RegisterWorkspace(ctx, tx, alpha.workspace)
		})
		if !errors.Is(err, ErrWorkspaceRetired) {
			t.Fatalf("workspace namespace reuse = %v; want ErrWorkspaceRetired", err)
		}
		if err := WithinScope(ctx, pool, alphaScope, testActor(alphaScope), func(ctx context.Context, tx *Transaction) error {
			stored, err := LoadIdentityMutation(ctx, tx, alpha.mutation.Key)
			if err != nil {
				return err
			}
			if stored.ResultRef != alpha.mutation.ResultRef {
				t.Fatalf("retired namespace lost its mutation guard: %#v", stored)
			}
			newMutation := alpha.mutation
			newMutation.Key.IdempotencyKey = "new-key-after-retirement"
			_, err = RecordIdentityMutation(ctx, tx, newMutation)
			return err
		}); !errors.Is(err, ErrWorkspaceRetired) {
			t.Fatalf("new mutation in retired namespace = %v; want ErrWorkspaceRetired", err)
		}
	})
}

func testCrossScopeTaskLease(t *testing.T, ctx context.Context, pool *Pool, alpha, beta compositeTenantFixture) {
	t.Helper()
	taskRef := TaskRef{RunID: compositeRunID, TaskID: compositeTaskID}
	var alphaLease Lease
	if err := WithinScope(ctx, pool, alpha.workspace.Scope, testActor(alpha.workspace.Scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		alphaLease, err = LoadLease(ctx, tx, taskRef)
		if err != nil {
			return err
		}
		alphaLease, err = RenewLease(ctx, tx, alphaLease, 2*time.Minute)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var betaLease Lease
	if err := WithinScope(ctx, pool, beta.workspace.Scope, testActor(beta.workspace.Scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		betaLease, err = ClaimTask(ctx, tx, ClaimRequest{
			Task: taskRef, OwnerID: alphaLease.OwnerID, ExpectedFence: 0, TTL: time.Minute,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if betaLease.OwnerID != alphaLease.OwnerID || betaLease.Fence != alphaLease.Fence {
		t.Fatalf("tenant leases do not share the intended owner/fence collision: alpha=%#v beta=%#v", alphaLease, betaLease)
	}

	unexpectedAcceptance := errors.New("foreign task lease unexpectedly accepted")
	expectScopeMismatch := func(name string, operation func(context.Context, *Transaction) error) {
		t.Run(name, func(t *testing.T) {
			err := WithinScope(ctx, pool, beta.workspace.Scope, testActor(beta.workspace.Scope), func(ctx context.Context, tx *Transaction) error {
				if err := operation(ctx, tx); err != nil {
					return err
				}
				return unexpectedAcceptance
			})
			if errors.Is(err, unexpectedAcceptance) {
				t.Fatal("foreign task lease reached the tenant-local operation")
			}
			if !errors.Is(err, ErrScopeMismatch) {
				t.Fatalf("foreign task lease error = %v; want ErrScopeMismatch", err)
			}
		})
	}
	expectScopeMismatch("require_current", func(ctx context.Context, tx *Transaction) error {
		return requireCurrentLease(ctx, tx, alphaLease)
	})
	expectScopeMismatch("renew", func(ctx context.Context, tx *Transaction) error {
		_, err := RenewLease(ctx, tx, alphaLease, 5*time.Minute)
		return err
	})
	var loadedBetaLease Lease
	if err := WithinScope(ctx, pool, beta.workspace.Scope, testActor(beta.workspace.Scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		loadedBetaLease, err = LoadLease(ctx, tx, taskRef)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loadedBetaLease, betaLease) {
		t.Fatalf("foreign renewal changed beta lease: got %#v want %#v", loadedBetaLease, betaLease)
	}
	if err := WithinScope(ctx, pool, beta.workspace.Scope, testActor(beta.workspace.Scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		betaLease, err = RenewLease(ctx, tx, betaLease, 2*time.Minute)
		return err
	}); err != nil {
		t.Fatalf("renew own-scope lease: %v", err)
	}

	running, err := beta.task.ObserveOrchestration(contract.OrchestrationRunning, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	running.State = contract.RunStateRunning
	if err := running.Validate(); err != nil {
		t.Fatal(err)
	}
	transition := TaskTransitionCommit{
		Lease:   alphaLease,
		Task:    running,
		Receipt: testReceipt("receipt-beta-running", "task_transition", compositeRunID),
		Audit:   testAudit("audit-beta-running", "task_running", compositeRunID),
		Event:   testEvent("event-beta-running", "task_running", compositeRunID),
		Outbox:  testOutbox("outbox-beta-running", "project_task", compositeRunID),
	}
	expectScopeMismatch("task_transition", func(ctx context.Context, tx *Transaction) error {
		_, err := CommitTaskTransition(ctx, tx, transition)
		return err
	})
	if err := WithinScope(ctx, pool, beta.workspace.Scope, testActor(beta.workspace.Scope), func(ctx context.Context, tx *Transaction) error {
		stored, err := LoadTask(ctx, tx, taskRef)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(stored, beta.task) {
			t.Fatalf("foreign transition changed beta task: %#v", stored)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	transition.Lease = betaLease
	if err := WithinScope(ctx, pool, beta.workspace.Scope, testActor(beta.workspace.Scope), func(ctx context.Context, tx *Transaction) error {
		_, err := CommitTaskTransition(ctx, tx, transition)
		return err
	}); err != nil {
		t.Fatalf("commit own-scope task transition: %v", err)
	}

	foreignDispatch := testDispatchFor(t, compositeRunID, "foreign-lease")
	expectScopeMismatch("effect_prepare", func(ctx context.Context, tx *Transaction) error {
		return PrepareDispatch(ctx, tx, alphaLease, foreignDispatch)
	})
	if err := WithinScope(ctx, pool, beta.workspace.Scope, testActor(beta.workspace.Scope), func(ctx context.Context, tx *Transaction) error {
		_, err := LoadDispatch(ctx, tx, taskRef, foreignDispatch.DispatchID)
		return err
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign lease retained beta dispatch = %v; want ErrNotFound", err)
	}

	dispatch := testDispatchFor(t, compositeRunID, "beta-settlement")
	if err := WithinScope(ctx, pool, beta.workspace.Scope, testActor(beta.workspace.Scope), func(ctx context.Context, tx *Transaction) error {
		return PrepareDispatch(ctx, tx, betaLease, dispatch)
	}); err != nil {
		t.Fatalf("prepare own-scope effect: %v", err)
	}
	outcome := contract.MemberOutcome{
		MemberID: dispatch.OrderedMemberIDs[0], EffectKey: dispatch.EffectKey,
		DispatchID: dispatch.DispatchID, State: contract.OutcomeSucceeded,
		ObservedAt: time.Now().UTC(), AssertionRefs: []string{},
	}
	succeeded, err := running.ApplyEffectSummary(contract.EffectSummary{Succeeded: 1}, false, "", outcome.ObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	settlement := SettlementCommit{
		Lease: alphaLease, DispatchID: dispatch.DispatchID, Outcomes: []contract.MemberOutcome{outcome}, Task: succeeded,
		Receipt: testReceipt("receipt-beta-settlement", "settlement", compositeRunID),
		Audit:   testAudit("audit-beta-settlement", "dispatch_settled", compositeRunID),
		Event:   testEvent("event-beta-settlement", "dispatch_settled", compositeRunID),
		Outbox:  testOutbox("outbox-beta-settlement", "project_task", compositeRunID),
	}
	expectScopeMismatch("settlement", func(ctx context.Context, tx *Transaction) error {
		_, err := CommitSettlement(ctx, tx, settlement)
		return err
	})
	if err := WithinScope(ctx, pool, beta.workspace.Scope, testActor(beta.workspace.Scope), func(ctx context.Context, tx *Transaction) error {
		stored, err := LoadTask(ctx, tx, taskRef)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(stored, running) {
			t.Fatalf("foreign settlement changed beta task: %#v", stored)
		}
		outcomes, err := ListMemberOutcomes(ctx, tx, taskRef, dispatch.DispatchID)
		if err != nil {
			return err
		}
		if len(outcomes) != 0 {
			t.Fatalf("foreign settlement retained beta outcomes: %#v", outcomes)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	settlement.Lease = betaLease
	if err := WithinScope(ctx, pool, beta.workspace.Scope, testActor(beta.workspace.Scope), func(ctx context.Context, tx *Transaction) error {
		_, err := CommitSettlement(ctx, tx, settlement)
		return err
	}); err != nil {
		t.Fatalf("commit own-scope settlement: %v", err)
	}

	verified, err := NewPreverifiedObjectRef(beta.artifact, ObjectVerification{
		ObservedSHA256: beta.artifact.SHA256, ObservedByteLength: beta.artifact.ByteLength,
		ObservedBackendGeneration: beta.artifact.BackendGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := ManifestCheckpointCAS{
		Next: CheckpointIdentity{
			ConnectionID: compositeConnectionID, ResourceID: "lease-scope-checkpoint",
			SourceGeneration: "lease-scope-generation", SchemaFingerprint: testDigest("lease-scope-schema"),
			Partition: "lease-scope-partition",
			Continuation: CheckpointContinuation{
				Present: true, Kind: "cursor", Opaque: []byte{},
			},
		},
		Manifest: ManifestCAS{ResourceID: "lease-scope-manifest", Object: verified},
		Lease:    alphaLease,
	}
	expectScopeMismatch("checkpoint", func(ctx context.Context, tx *Transaction) error {
		_, err := CommitManifestCheckpoint(ctx, tx, checkpoint)
		return err
	})
	var checkpointErr, manifestErr error
	if err := WithinScope(ctx, pool, beta.workspace.Scope, testActor(beta.workspace.Scope), func(ctx context.Context, tx *Transaction) error {
		_, checkpointErr = LoadCheckpoint(ctx, tx, checkpoint.Next.ConnectionID, checkpoint.Next.ResourceID)
		_, manifestErr = LoadManifest(ctx, tx, checkpoint.Manifest.ResourceID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(checkpointErr, ErrNotFound) || !errors.Is(manifestErr, ErrNotFound) {
		t.Fatalf("foreign lease retained beta publication: checkpoint=%v manifest=%v; want ErrNotFound", checkpointErr, manifestErr)
	}
	checkpoint.Lease = betaLease
	if err := WithinScope(ctx, pool, beta.workspace.Scope, testActor(beta.workspace.Scope), func(ctx context.Context, tx *Transaction) error {
		_, err := CommitManifestCheckpoint(ctx, tx, checkpoint)
		return err
	}); err != nil {
		t.Fatalf("commit own-scope checkpoint: %v", err)
	}
}

func newCompositeTenantFixture(t *testing.T, scope contract.Scope, label string) compositeTenantFixture {
	t.Helper()
	workspace := WorkspaceIdentity{Scope: scope, Name: "workspace-" + label}
	identity := Identity{
		Principal: contract.Principal{OrgID: scope.OrgID, PrincipalID: compositeIdentityID, Kind: contract.PrincipalUser},
		Issuer:    "issuer-" + label,
		Subject:   "subject-" + label,
		Revision:  1,
	}
	membership := Membership{
		Scope: scope, MembershipID: compositeMembershipID, IdentityID: compositeIdentityID,
		Role: "operator_" + label, PolicyEpoch: 1,
	}
	mutation := IdentityMutation{
		Key: IdentityMutationKey{
			Scope: scope, PrincipalID: compositeIdentityID, OperationID: compositeMutationAction,
			TargetID: compositeMutationTarget, IdempotencyKey: compositeMutationKey,
		},
		IntentDigest: testDigest("identity-mutation-" + label),
		ResultRef:    "mutation-result-" + label, ResultRevision: 1,
	}
	account := ProviderAccount{
		OrgID: scope.OrgID, AccountID: compositeProviderAccount, Provider: "provider",
		AccountWitness: "account-witness-" + label, Capacity: 4, Revision: 1,
	}
	connection := Connection{
		Scope: scope, ConnectionID: compositeConnectionID, ConnectorID: "connector-" + label,
		ProviderAccountID: compositeProviderAccount, Revision: 1,
	}
	request := newCompositePlanRequest(t, scope, label)
	task := testTask(request, compositeTaskID, time.Date(2026, time.September, 16, 0, 0, 0, 0, time.UTC))
	task.BehaviorID = "behavior-" + label
	artifact := testObjectRef(scope, compositeArtifactID, "attempt-"+label, label)
	return compositeTenantFixture{
		workspace:      workspace,
		identity:       identity,
		membership:     membership,
		mutation:       mutation,
		account:        account,
		connection:     connection,
		planRevision:   "plan-revision-" + label,
		planDigest:     testDigest("plan-binding-" + label),
		approvalDigest: testDigest("approval-binding-" + label),
		request:        request,
		task:           task,
		artifact:       artifact,
	}
}

func newCompositePlanRequest(t *testing.T, scope contract.Scope, label string) AdmissionRequest {
	t.Helper()
	request := testAdmissionRequest(t, scope, compositeAdmissionKey, "tenant-reference-"+label)
	request.Key = AdmissionKey{
		PrincipalID:    compositeIdentityID,
		OperationID:    compositeAdmissionAction,
		TargetID:       compositeAdmissionTarget,
		IdempotencyKey: compositeAdmissionKey,
	}
	request.Spec.RunID = compositeRunID
	request.Spec.OperationOrPlanRef = contract.OperationOrPlanRef{
		Kind: contract.PlanReference, ID: compositePlanID, Revision: "plan-revision-" + label,
	}
	request.Spec.IdempotencyFingerprint = testDigest("admission-" + label)
	binding, err := contract.NewAPIWorkflowBinding(compositeRunID)
	if err != nil {
		t.Fatal(err)
	}
	request.WorkflowBinding = binding
	request.Receipt = testReceipt("receipt-shared", "admission", compositeRunID)
	request.Audit = testAudit("audit-shared", "run_admitted", compositeRunID)
	request.Event = testEvent(compositeEventID, "tenant_reference_"+label, compositeRunID)
	request.Outbox = testOutbox("outbox-shared", "start_run", compositeRunID)
	return request
}

func newCompositeOperationRequest(t *testing.T, scope contract.Scope, runID, label string) AdmissionRequest {
	t.Helper()
	request := testAdmissionRequest(t, scope, "operation-key-"+label, label)
	request.Spec.RunID = runID
	binding, err := contract.NewAPIWorkflowBinding(runID)
	if err != nil {
		t.Fatal(err)
	}
	request.WorkflowBinding = binding
	request.Receipt = testReceipt("receipt-"+label, "admission", runID)
	request.Audit = testAudit("audit-"+label, "run_admitted", runID)
	request.Event = testEvent("event-"+label, "run_admitted", runID)
	request.Outbox = testOutbox("outbox-"+label, "start_run", runID)
	return request
}

func seedCompositeTenantFixture(t *testing.T, ctx context.Context, pool *Pool, fixture compositeTenantFixture) {
	t.Helper()
	verified, err := NewPreverifiedObjectRef(fixture.artifact, ObjectVerification{
		ObservedSHA256:            fixture.artifact.SHA256,
		ObservedByteLength:        fixture.artifact.ByteLength,
		ObservedBackendGeneration: fixture.artifact.BackendGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WithinScope(ctx, pool, fixture.workspace.Scope, testActor(fixture.workspace.Scope), func(ctx context.Context, tx *Transaction) error {
		for _, register := range []func() error{
			func() error { return RegisterWorkspace(ctx, tx, fixture.workspace) },
			func() error { return RegisterIdentity(ctx, tx, fixture.identity) },
			func() error { return RegisterMembership(ctx, tx, fixture.membership) },
			func() error { return RegisterProviderAccount(ctx, tx, fixture.account) },
			func() error { return RegisterConnection(ctx, tx, fixture.connection) },
		} {
			if err := register(); err != nil {
				return err
			}
		}
		if _, err := RecordIdentityMutation(ctx, tx, fixture.mutation); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, `
			INSERT INTO pm.plan_bindings (
				org_id, workspace_id, plan_id, plan_revision, binding_digest
			) VALUES ($1, $2, $3, $4, $5)
		`, fixture.workspace.Scope.OrgID, fixture.workspace.Scope.WorkspaceID, compositePlanID, fixture.planRevision, string(fixture.planDigest)); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, `
			INSERT INTO pm.approval_bindings (
				org_id, workspace_id, approval_id, plan_id, plan_revision, binding_digest
			) VALUES ($1, $2, $3, $4, $5, $6)
		`, fixture.workspace.Scope.OrgID, fixture.workspace.Scope.WorkspaceID, compositeApprovalID, compositePlanID, fixture.planRevision, string(fixture.approvalDigest)); err != nil {
			return err
		}
		if _, err := AdmitRun(ctx, tx, fixture.request); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, `
			INSERT INTO pm.run_plan_bindings (
				org_id, workspace_id, run_id, plan_id, plan_revision, approval_id
			) VALUES ($1, $2, $3, $4, $5, $6)
		`, fixture.workspace.Scope.OrgID, fixture.workspace.Scope.WorkspaceID, compositeRunID, compositePlanID, fixture.planRevision, compositeApprovalID); err != nil {
			return err
		}
		if err := CreateTask(ctx, tx, fixture.task); err != nil {
			return err
		}
		_, err := CommitManifestCAS(ctx, tx, ManifestCAS{
			ResourceID: compositeManifestID, ExpectedRevision: 0, Object: verified,
		})
		return err
	}); err != nil {
		t.Fatalf("seed composite tenant fixture: %v", err)
	}
}

func seedForeignOnlyReferences(t *testing.T, ctx context.Context, pool *Pool, fixture compositeTenantFixture) {
	t.Helper()
	request := newCompositeOperationRequest(t, fixture.workspace.Scope, compositeForeignRunID, "foreign-only")
	connection := fixture.connection
	connection.ConnectionID = compositeForeignConnectID
	if err := WithinScope(ctx, pool, fixture.workspace.Scope, testActor(fixture.workspace.Scope), func(ctx context.Context, tx *Transaction) error {
		if err := RegisterConnection(ctx, tx, connection); err != nil {
			return err
		}
		_, err := AdmitRun(ctx, tx, request)
		return err
	}); err != nil {
		t.Fatalf("seed foreign-only tenant references: %v", err)
	}
}

func assertCompositeTenantFixture(t *testing.T, ctx context.Context, pool *Pool, fixture compositeTenantFixture) {
	t.Helper()
	if err := WithinScope(ctx, pool, fixture.workspace.Scope, testActor(fixture.workspace.Scope), func(ctx context.Context, tx *Transaction) error {
		identity, err := LoadIdentity(ctx, tx, compositeIdentityID)
		if err != nil {
			return err
		}
		membership, err := LoadMembership(ctx, tx, compositeMembershipID)
		if err != nil {
			return err
		}
		mutation, err := LoadIdentityMutation(ctx, tx, fixture.mutation.Key)
		if err != nil {
			return err
		}
		account, err := LoadProviderAccount(ctx, tx, compositeProviderAccount)
		if err != nil {
			return err
		}
		connection, err := LoadConnection(ctx, tx, compositeConnectionID)
		if err != nil {
			return err
		}
		run, err := LoadRun(ctx, tx, compositeRunID)
		if err != nil {
			return err
		}
		task, err := LoadTask(ctx, tx, TaskRef{RunID: compositeRunID, TaskID: compositeTaskID})
		if err != nil {
			return err
		}
		events, err := ListEvents(ctx, tx, compositeRunID, 0, 10)
		if err != nil {
			return err
		}
		artifact, err := LoadArtifact(ctx, tx, compositeArtifactID)
		if err != nil {
			return err
		}
		var planRevision, planDigest, approvalDigest string
		if err := tx.queryRow(ctx, `
			SELECT binding.plan_revision, plan.binding_digest, approval.binding_digest
			FROM pm.run_plan_bindings AS binding
			JOIN pm.plan_bindings AS plan
			  ON plan.org_id = binding.org_id
			 AND plan.workspace_id = binding.workspace_id
			 AND plan.plan_id = binding.plan_id
			 AND plan.plan_revision = binding.plan_revision
			JOIN pm.approval_bindings AS approval
			  ON approval.org_id = binding.org_id
			 AND approval.workspace_id = binding.workspace_id
			 AND approval.approval_id = binding.approval_id
			 AND approval.plan_id = binding.plan_id
			 AND approval.plan_revision = binding.plan_revision
			WHERE binding.run_id = $1
		`, compositeRunID).Scan(&planRevision, &planDigest, &approvalDigest); err != nil {
			return err
		}
		if identity != fixture.identity || !equalMembership(membership, fixture.membership) ||
			mutation.IntentDigest != fixture.mutation.IntentDigest || mutation.ResultRef != fixture.mutation.ResultRef ||
			account != fixture.account || connection != fixture.connection ||
			run.Spec.OperationOrPlanRef.Revision != fixture.planRevision ||
			task.BehaviorID != fixture.task.BehaviorID || len(events) != 1 || events[0].Type != fixture.request.Event.Type ||
			artifact != fixture.artifact || planRevision != fixture.planRevision ||
			planDigest != string(fixture.planDigest) || approvalDigest != string(fixture.approvalDigest) {
			return fmt.Errorf("persisted tenant references did not resolve to their owning scope")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func requireSQLState(t *testing.T, err error, expected string) {
	t.Helper()
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) || databaseError.Code != expected {
		t.Fatalf("database error = %v; want SQLSTATE %s", err, expected)
	}
}

func testCheckpointPublication(t *testing.T, ctx context.Context, pool *Pool, fixture compositeTenantFixture) {
	t.Helper()
	scope := fixture.workspace.Scope
	var lease Lease
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		lease, err = ClaimTask(ctx, tx, ClaimRequest{
			Task:    TaskRef{RunID: fixture.task.RunID, TaskID: fixture.task.TaskID},
			OwnerID: "checkpoint-writer", TTL: time.Minute,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	verified, err := NewPreverifiedObjectRef(fixture.artifact, ObjectVerification{
		ObservedSHA256: fixture.artifact.SHA256, ObservedByteLength: fixture.artifact.ByteLength,
		ObservedBackendGeneration: fixture.artifact.BackendGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	change := ManifestCheckpointCAS{
		Next: CheckpointIdentity{
			ConnectionID: fixture.connection.ConnectionID, ResourceID: "checkpoint-dataset",
			SourceGeneration: "generation-one", SchemaFingerprint: testDigest("checkpoint-schema"),
			Partition:    "partition-one",
			Continuation: CheckpointContinuation{Present: true, Kind: "cursor", Opaque: []byte{}},
		},
		Manifest: ManifestCAS{ResourceID: "checkpoint-manifest", Object: verified},
		Lease:    lease,
	}
	publish := func(change ManifestCheckpointCAS, afterWrite error) (Checkpoint, error) {
		var checkpoint Checkpoint
		err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			var err error
			checkpoint, err = CommitManifestCheckpoint(ctx, tx, change)
			if err != nil {
				return err
			}
			return afterWrite
		})
		return checkpoint, err
	}
	assertPublished := func(t *testing.T, expected Checkpoint, object contract.ObjectRef) {
		t.Helper()
		var checkpoint Checkpoint
		var manifest Manifest
		if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			var err error
			checkpoint, err = LoadCheckpoint(ctx, tx, change.Next.ConnectionID, change.Next.ResourceID)
			if err != nil {
				return err
			}
			manifest, err = LoadManifest(ctx, tx, change.Manifest.ResourceID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(checkpoint, expected) ||
			manifest.Revision != expected.CommittedManifestRevision ||
			!reflect.DeepEqual(manifest.Object, object) {
			t.Fatal("checkpoint and exact registered manifest did not retain the same committed publication")
		}
	}
	first, err := publish(change, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != 1 || first.CommittedManifestRevision != 1 ||
		!first.Identity.Continuation.Present || first.Identity.Continuation.Opaque == nil ||
		len(first.Identity.Continuation.Opaque) != 0 {
		t.Fatal("initial checkpoint did not preserve an explicitly empty continuation")
	}
	assertPublished(t, first, fixture.artifact)

	nextObject := testObjectRef(scope, "checkpoint-next-object", "checkpoint-next-attempt", "checkpoint-next")
	nextVerified, err := NewPreverifiedObjectRef(nextObject, ObjectVerification{
		ObservedSHA256: nextObject.SHA256, ObservedByteLength: nextObject.ByteLength,
		ObservedBackendGeneration: nextObject.BackendGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	change.Expected = &first
	change.Manifest.ExpectedRevision = 1
	change.Manifest.Object = nextVerified
	change.Next.Continuation.Opaque = []byte{0, 255, 1}
	change.Next.Continuation.TieBreaker = "next-member"
	for _, testCase := range []struct {
		name  string
		alter func(*Checkpoint)
	}{
		{"source_generation", func(c *Checkpoint) { c.Identity.SourceGeneration = "other-generation" }},
		{"schema", func(c *Checkpoint) { c.Identity.SchemaFingerprint = testDigest("other-schema") }},
		{"partition", func(c *Checkpoint) { c.Identity.Partition = "other-partition" }},
		{"continuation_presence", func(c *Checkpoint) { c.Identity.Continuation = CheckpointContinuation{} }},
		{"continuation_bytes", func(c *Checkpoint) { c.Identity.Continuation.Opaque = []byte{9} }},
		{"tie_breaker", func(c *Checkpoint) { c.Identity.Continuation.TieBreaker = "other-member" }},
		{"unread_tail", func(c *Checkpoint) { c.Identity.Continuation.UnreadTailRef = "other-tail" }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			wrong := first
			testCase.alter(&wrong)
			candidate := change
			candidate.Expected = &wrong
			if _, err := publish(candidate, nil); !errors.Is(err, ErrCASConflict) {
				t.Fatalf("changed expected checkpoint identity = %v; want ErrCASConflict", err)
			}
			assertPublished(t, first, fixture.artifact)
		})
	}
	stale := change
	stale.Lease.Fence++
	if _, err := publish(stale, nil); !errors.Is(err, ErrStaleFence) {
		t.Fatalf("checkpoint with stale writer fence = %v; want ErrStaleFence", err)
	}
	assertPublished(t, first, fixture.artifact)
	interrupted := errors.New("interrupt after checkpoint and manifest writes")
	if _, err := publish(change, interrupted); !errors.Is(err, interrupted) {
		t.Fatalf("publication interruption = %v; want callback failure", err)
	}
	assertPublished(t, first, fixture.artifact)
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := LoadArtifact(ctx, tx, nextObject.ArtifactID)
		return err
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rolled-back publication retained its new artifact metadata: %v", err)
	}
	second, err := publish(change, nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision != 2 || second.CommittedManifestRevision != 2 ||
		!reflect.DeepEqual(second.Identity.Continuation, change.Next.Continuation) {
		t.Fatal("successful publication did not advance the exact continuation and both revisions")
	}
	assertPublished(t, second, nextObject)
	if _, err := publish(change, nil); !errors.Is(err, ErrCASConflict) {
		t.Fatalf("stale checkpoint publication = %v; want ErrCASConflict", err)
	}
	assertPublished(t, second, nextObject)
	err = WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := CommitManifestCAS(ctx, tx, ManifestCAS{
			ResourceID: change.Manifest.ResourceID, ExpectedRevision: 2, Object: verified,
		})
		return err
	})
	requireSQLState(t, err, "23503")
	assertPublished(t, second, nextObject)
}
