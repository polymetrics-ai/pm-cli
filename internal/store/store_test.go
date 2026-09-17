package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type testStoreCredentials struct {
	runtimeDSN  string
	migratorDSN string
}

func loadTestStoreCredentials(t *testing.T) testStoreCredentials {
	t.Helper()
	secretDir := os.Getenv("PM_STORE_TEST_SECRETS_DIR")
	if secretDir == "" {
		t.Fatal("PM_STORE_TEST_SECRETS_DIR is required")
	}
	absolute, err := filepath.Abs(secretDir)
	if err != nil {
		t.Fatal("resolve PM_STORE_TEST_SECRETS_DIR")
	}
	endpointBytes, err := os.ReadFile(filepath.Join(absolute, "pm-app-endpoint"))
	if err != nil {
		t.Fatal("read pm-app-endpoint")
	}
	passwordBytes, err := os.ReadFile(filepath.Join(absolute, "pm-app-migrator-password"))
	if err != nil {
		t.Fatal("read pm-app-migrator-password")
	}
	endpoint := strings.TrimSpace(string(endpointBytes))
	password := strings.TrimSpace(string(passwordBytes))
	if endpoint == "" || password == "" {
		t.Fatal("PostgreSQL fixture credentials are empty")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("pm-app-endpoint is not a PostgreSQL URL")
	}
	if parsed.User == nil || parsed.User.Username() != RuntimeRole || strings.TrimPrefix(parsed.Path, "/") != ApplicationDatabase {
		t.Fatal("pm-app-endpoint does not identify the selected runtime role and database")
	}
	migrator := *parsed
	migrator.User = url.UserPassword(MigratorRole, password)
	return testStoreCredentials{runtimeDSN: endpoint, migratorDSN: migrator.String()}
}

func prepareTestStore(t *testing.T, maxConns int32) (testStoreCredentials, *Pool) {
	t.Helper()
	credentials := loadTestStoreCredentials(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Migrate(ctx, credentials.migratorDSN); err != nil {
		t.Fatalf("migrate selected store: %T", err)
	}
	pool, err := OpenPool(ctx, PoolConfig{
		ConnectionString:  credentials.runtimeDSN,
		MaxConns:          maxConns,
		MinConns:          0,
		MaxConnLifetime:   5 * time.Minute,
		MaxConnIdleTime:   time.Minute,
		HealthCheckPeriod: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("open selected store pool: %T", err)
	}
	t.Cleanup(pool.Close)
	return credentials, pool
}

func newTestScope(t *testing.T) contract.Scope {
	t.Helper()
	return contract.Scope{
		OrgID:       "org-" + randomHex(t, 8),
		WorkspaceID: "workspace-" + randomHex(t, 8),
	}
}

func testActor(scope contract.Scope) contract.Actor {
	principal := contract.Principal{OrgID: scope.OrgID, PrincipalID: "ada", Kind: contract.PrincipalUser}
	return contract.Actor{RequestedActor: principal, EffectiveActor: principal}
}

func randomHex(t *testing.T, byteCount int) string {
	t.Helper()
	value := make([]byte, byteCount)
	if _, err := rand.Read(value); err != nil {
		t.Fatal("generate isolated test identity")
	}
	return hex.EncodeToString(value)
}

func randomUUID(t *testing.T) string {
	t.Helper()
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		t.Fatal("generate run identity")
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

func testDigest(seed string) contract.SemanticDigest {
	digest := sha256.Sum256([]byte(seed))
	return contract.SemanticDigest("sha256:" + hex.EncodeToString(digest[:]))
}

func testAdmissionRequest(t *testing.T, scope contract.Scope, key, seed string) AdmissionRequest {
	t.Helper()
	actor := testActor(scope)
	digest := testDigest(seed)
	runID := randomUUID(t)
	budget, err := NewRunBudgetSnapshot(scope, runID, "budget-"+runID, testRunBudgetPolicy())
	if err != nil {
		t.Fatalf("construct run budget snapshot: %v", err)
	}
	spec := contract.RunSpec{
		RunID:                  runID,
		OrgID:                  scope.OrgID,
		WorkspaceID:            scope.WorkspaceID,
		Actor:                  actor,
		ExecutionGrantRef:      "grant-" + runID,
		Origin:                 contract.RunOriginAPI,
		OperationOrPlanRef:     contract.OperationOrPlanRef{Kind: contract.OperationReference, ID: "operation-" + seed, Revision: "revision-1"},
		InputRefs:              []string{"input-" + runID},
		SchemaRefs:             []string{},
		CapabilityVersions:     []contract.CapabilityRequirement{},
		Budget:                 budget.Binding(),
		IdempotencyFingerprint: digest,
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("construct RunSpec: %v", err)
	}
	binding, err := contract.NewAPIWorkflowBinding(runID)
	if err != nil {
		t.Fatalf("construct WorkflowBinding: %v", err)
	}
	return AdmissionRequest{
		BudgetSnapshot: budget,
		InputSnapshot: RunInputSnapshot{
			Scope: scope, RunID: runID, InputRef: spec.InputRefs[0],
			Lane:         contract.LaneDirectRead,
			ConnectionID: "connection-" + runID,
			Input: contract.NewObjectValue(map[string]contract.PresentValue{
				"seed": contract.NewStringValue(seed),
			}),
			ResultControls: contract.ResultControls{},
			ApprovalRef:    contract.AbsentValue(),
		},
		Grant: contract.ExecutionGrant{
			GrantRef: spec.ExecutionGrantRef, GrantEpoch: 1, Scope: scope, Actor: actor,
			ConnectionIDs: []string{}, OperationIDs: []string{spec.OperationOrPlanRef.ID}, ApprovalRefs: []string{},
			ExpiresAt: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), PolicyEpochs: contract.PolicyEpochs{Membership: 1},
		},
		Key: AdmissionKey{
			PrincipalID:    actor.RequestedActor.PrincipalID,
			OperationID:    spec.OperationOrPlanRef.ID,
			TargetID:       "target-" + seed,
			IdempotencyKey: key,
		},
		Spec:            spec,
		WorkflowBinding: binding,
		Receipt:         testReceipt("receipt-admission-"+seed, "admission", runID),
		Audit:           testAudit("audit-admission-"+seed, "run_admitted", runID),
		Event:           testEvent("event-admission-"+seed, "run_admitted", runID),
		Outbox:          testOutbox("outbox-admission-"+seed, "start_run", runID),
	}
}

func testRunBudgetPolicy() RunBudgetPolicy {
	return RunBudgetPolicy{
		Pending: AdmissionLimits{
			GlobalPending:          256,
			WorkspacePending:       32,
			WorkspaceMaxPendingAge: 10 * time.Minute,
		},
		RunLifetime:          24 * time.Hour,
		InputMaxBytes:        1 << 20,
		WorkspaceActiveRuns:  32,
		WorkflowPayloadBytes: 1 << 16,
		WorkflowFanout:       32,
	}
}

func setAdmissionPolicyForTest(t *testing.T, request *AdmissionRequest, policy RunBudgetPolicy) {
	t.Helper()
	budget, err := NewRunBudgetSnapshot(
		request.BudgetSnapshot.Scope,
		request.BudgetSnapshot.RunID,
		request.BudgetSnapshot.BudgetRef,
		policy,
	)
	if err != nil {
		t.Fatalf("replace run budget policy: %v", err)
	}
	request.BudgetSnapshot = budget
	request.Spec.Budget = budget.Binding()
}

func setAdmissionLimitsForTest(t *testing.T, request *AdmissionRequest, limits AdmissionLimits) {
	t.Helper()
	policy := request.BudgetSnapshot.Policy
	policy.Pending = limits
	setAdmissionPolicyForTest(t, request, policy)
}

func rebindAdmissionBudgetForTest(t *testing.T, request *AdmissionRequest) {
	t.Helper()
	budget, err := NewRunBudgetSnapshot(
		contract.Scope{OrgID: request.Spec.OrgID, WorkspaceID: request.Spec.WorkspaceID},
		request.Spec.RunID,
		"budget-"+request.Spec.RunID,
		request.BudgetSnapshot.Policy,
	)
	if err != nil {
		t.Fatalf("rebind run budget snapshot: %v", err)
	}
	request.BudgetSnapshot = budget
	request.Spec.Budget = budget.Binding()
}

func testTask(request AdmissionRequest, taskID string, observedAt time.Time) contract.Task {
	return contract.Task{
		TaskID:                 taskID,
		RunID:                  request.Spec.RunID,
		OrgID:                  request.Spec.OrgID,
		WorkspaceID:            request.Spec.WorkspaceID,
		OperationID:            request.Spec.OperationOrPlanRef.ID,
		Lane:                   contract.LaneDirectWrite,
		BehaviorID:             "behavior-1",
		InputRefs:              []string{},
		SchemaRefs:             []string{},
		State:                  contract.RunStateAccepted,
		EffectSummary:          contract.EffectSummary{},
		UnknownEffectCount:     0,
		CancelRequested:        false,
		OrchestrationCondition: contract.OrchestrationPending,
		ProjectionObservedAt:   observedAt.UTC(),
		Revision:               1,
	}
}

func testReceipt(id, kind, runID string) Receipt {
	return Receipt{
		ReceiptID: id,
		Kind:      kind,
		RunID:     runID,
		Payload: contract.SafePayload{
			References:  []contract.SafeReference{},
			Counts:      []contract.SafeCount{},
			ReasonCodes: []string{},
		},
	}
}

func testAudit(id, action, runID string) AuditEntry {
	return AuditEntry{
		AuditID:   id,
		Action:    action,
		TargetRef: runID,
		Payload: contract.SafePayload{
			References:  []contract.SafeReference{},
			Counts:      []contract.SafeCount{},
			ReasonCodes: []string{},
		},
	}
}

func testEvent(id, eventType, runID string) EventInput {
	return EventInput{
		EventID: id,
		Type:    eventType,
		Payload: contract.SafePayload{
			References:  []contract.SafeReference{{Name: "run", Ref: runID}},
			Counts:      []contract.SafeCount{},
			ReasonCodes: []string{},
		},
	}
}

func testOutbox(id, kind, runID string) OutboxCommand {
	return OutboxCommand{CommandID: id, Kind: kind, PayloadRef: runID}
}

func cleanupTestScope(t *testing.T, credentials testStoreCredentials, scopes ...contract.Scope) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, credentials.migratorDSN)
	if err != nil {
		t.Fatal("connect migrator for scoped cleanup")
	}
	defer func() { _ = conn.Close(context.Background()) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal("begin scoped cleanup")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	setScope := func(scope contract.Scope) {
		if _, err := tx.Exec(ctx, `SELECT set_config('pm.org_id', $1, true), set_config('pm.workspace_id', $2, true)`, scope.OrgID, scope.WorkspaceID); err != nil {
			t.Fatal("install scoped cleanup context")
		}
	}
	for _, scope := range scopes {
		setScope(scope)
		for _, statement := range []string{
			`DELETE FROM pm.account_permits WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.checkpoints WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.encrypted_secrets WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.run_plan_bindings WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.approval_bindings WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.plan_bindings WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.identity_mutation_keys WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.memberships WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.dispatch_attempts WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.manifests WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.artifacts WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.connections WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.run_specs WHERE org_id = $1 AND workspace_id = $2`,
			`DELETE FROM pm.workspace_namespaces WHERE org_id = $1 AND workspace_id = $2`,
		} {
			if _, err := tx.Exec(ctx, statement, scope.OrgID, scope.WorkspaceID); err != nil {
				var databaseError *pgconn.PgError
				if errors.As(err, &databaseError) {
					t.Fatalf("clean isolated store scope %s/%s: SQLSTATE %s table=%s constraint=%s", scope.OrgID, scope.WorkspaceID, databaseError.Code, databaseError.TableName, databaseError.ConstraintName)
				}
				t.Fatalf("clean isolated store rows: %T", err)
			}
		}
	}
	// Organization-owned roots can be shared by several owned workspaces.
	// Remove them only after all of those workspace references are gone.
	for _, scope := range scopes {
		setScope(scope)
		for _, statement := range []string{
			`DELETE FROM pm.identities WHERE org_id = $1`,
			`DELETE FROM pm.provider_accounts WHERE org_id = $1`,
		} {
			if _, err := tx.Exec(ctx, statement, scope.OrgID); err != nil {
				var databaseError *pgconn.PgError
				if errors.As(err, &databaseError) {
					t.Fatalf("clean isolated organization %s: SQLSTATE %s table=%s constraint=%s", scope.OrgID, databaseError.Code, databaseError.TableName, databaseError.ConstraintName)
				}
				t.Fatalf("clean isolated organization rows: %T", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal("commit isolated store cleanup")
	}
}

func admitForTest(t *testing.T, pool *Pool, scope contract.Scope, request AdmissionRequest) AdmissionResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var result AdmissionResult
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		result, err = AdmitRun(ctx, tx, request)
		return err
	}); err != nil {
		t.Fatalf("admit test run: %v", err)
	}
	return result
}

func requireNotFound(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v; want ErrNotFound", err)
	}
}
