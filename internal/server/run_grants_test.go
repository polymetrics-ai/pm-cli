package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

const integrationRunGrantOperation = "fixture.read"

type integrationAcceptedRunGrant struct {
	RunID string
	Grant contract.ExecutionGrant
}

func TestRunGrantCheckUsesStoredGrantAfterLoginExpiry(t *testing.T) {
	fixture := newServerIntegrationFixture(t)
	browser := fixture.loginBrowser(t, integrationAuthTimeFresh, false)
	allowIntegrationRunOperation(t, fixture, integrationRunGrantOperation)
	pool := openIntegrationRunGrantPool(t, fixture)

	original := admitIntegrationRunGrant(t, fixture, pool, integrationRunGrantOperation, 7)
	replacement := admitIntegrationRunGrant(t, fixture, pool, integrationRunGrantOperation, 11)
	check := app.RunGrantCheck{
		RunID:                original.RunID,
		Permission:           "operation.invoke",
		ResourceKind:         auth.ResourceRun,
		ResourceID:           original.RunID,
		ConnectionID:         fixture.connectionID,
		OperationID:          integrationRunGrantOperation,
		CurrentApprovalEpoch: original.Grant.PolicyEpochs.Approval,
	}

	wrongApproval := check
	wrongApproval.CurrentApprovalEpoch++
	_, err := recheckIntegrationRunGrant(
		t,
		pool,
		fixture.application.services.Authentication,
		fixture.scope,
		contract.Actor{RequestedActor: fixture.operator, EffectiveActor: fixture.operator},
		wrongApproval,
	)
	requireIntegrationRunGrantFailure(t, err, 403, "grant_unavailable", "refresh_authorization")

	wrongConnection := check
	wrongConnection.ConnectionID = "connection-outside-accepted-grant"
	_, err = recheckIntegrationRunGrant(
		t,
		pool,
		fixture.application.services.Authentication,
		fixture.scope,
		contract.Actor{RequestedActor: fixture.operator, EffectiveActor: fixture.operator},
		wrongConnection,
	)
	requireIntegrationRunGrantFailure(t, err, 403, "grant_unavailable", "refresh_authorization")

	wrongOperation := check
	wrongOperation.OperationID = "fixture.write"
	_, err = recheckIntegrationRunGrant(
		t,
		pool,
		fixture.application.services.Authentication,
		fixture.scope,
		contract.Actor{RequestedActor: fixture.operator, EffectiveActor: fixture.operator},
		wrongOperation,
	)
	requireIntegrationRunGrantFailure(t, err, 403, "grant_unavailable", "refresh_authorization")

	mixedRun := check
	mixedRun.ResourceID = replacement.RunID
	_, err = recheckIntegrationRunGrant(
		t,
		pool,
		fixture.application.services.Authentication,
		fixture.scope,
		contract.Actor{RequestedActor: fixture.operator, EffectiveActor: fixture.operator},
		mixedRun,
	)
	requireIntegrationRunGrantFailure(t, err, 404, "resource_not_found", "correct_scope")

	foreignActor := contract.Actor{RequestedActor: fixture.foreignUser, EffectiveActor: fixture.foreignUser}
	_, err = recheckIntegrationRunGrant(
		t,
		pool,
		fixture.application.services.Authentication,
		fixture.foreignScope,
		foreignActor,
		check,
	)
	requireIntegrationRunGrantFailure(t, err, 404, "resource_not_found", "correct_scope")

	beforeExpiry, err := recheckIntegrationRunGrant(
		t,
		pool,
		fixture.application.services.Authentication,
		fixture.scope,
		contract.Actor{RequestedActor: fixture.operator, EffectiveActor: fixture.operator},
		check,
	)
	if err != nil {
		t.Fatalf("recheck accepted stored grant before login expiry: %v", err)
	}
	if !beforeExpiry.Equal(original.Grant) || beforeExpiry.Equal(replacement.Grant) {
		t.Fatal("run grant recheck did not return only the original persisted grant")
	}

	expiredSessions := fixture.execScoped(t, fixture.scope, `
		UPDATE pm.pm_sessions
		SET authenticated_at = NULL,
		    issued_at = clock_timestamp() - interval '2 hours',
		    expires_at = clock_timestamp() - interval '1 hour'
		WHERE deployment_id = $1 AND org_id = $2 AND principal_id = $3
		  AND revoked_at IS NULL
	`, fixture.deploymentID, fixture.operator.OrgID, fixture.operator.PrincipalID)
	if expiredSessions != 1 {
		t.Fatalf("expired login sessions = %d, want 1", expiredSessions)
	}
	expiredLogin := fixture.request(t, browser.client, http.MethodGet, "/v1/sessions/me", nil, nil)
	requireIntegrationStatus(t, expiredLogin, http.StatusUnauthorized, "expired browser login session")

	afterExpiry, err := recheckIntegrationRunGrant(
		t,
		pool,
		fixture.application.services.Authentication,
		fixture.scope,
		contract.Actor{RequestedActor: fixture.operator, EffectiveActor: fixture.operator},
		check,
	)
	if err != nil {
		t.Fatalf("recheck accepted stored grant after login expiry: %v", err)
	}
	if !afterExpiry.Equal(original.Grant) || !afterExpiry.Equal(beforeExpiry) {
		t.Fatal("ordinary login expiry changed independently durable run authority")
	}
}

func TestRunGrantCheckRejectsDurableRevocationAndCurrentAuthorityChange(t *testing.T) {
	fixture := newServerIntegrationFixture(t)
	allowIntegrationRunOperation(t, fixture, integrationRunGrantOperation)
	pool := openIntegrationRunGrantPool(t, fixture)
	actor := contract.Actor{RequestedActor: fixture.operator, EffectiveActor: fixture.operator}

	revokedRun := admitIntegrationRunGrant(t, fixture, pool, integrationRunGrantOperation, 0)
	currentAuthorityRun := admitIntegrationRunGrant(t, fixture, pool, integrationRunGrantOperation, 0)
	check := func(runID string) app.RunGrantCheck {
		return app.RunGrantCheck{
			RunID:        runID,
			Permission:   "operation.invoke",
			ResourceKind: auth.ResourceRun,
			ResourceID:   runID,
			ConnectionID: fixture.connectionID,
			OperationID:  integrationRunGrantOperation,
		}
	}

	if _, err := recheckIntegrationRunGrant(
		t,
		pool,
		fixture.application.services.Authentication,
		fixture.scope,
		actor,
		check(revokedRun.RunID),
	); err != nil {
		t.Fatalf("baseline recheck before durable revocation: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var changed bool
	if err := store.WithinScope(ctx, pool, fixture.scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		stored, revoked, err := store.RevokeRunExecutionGrant(ctx, tx, revokedRun.RunID, revokedRun.Grant.GrantEpoch)
		if err == nil && (!stored.Grant.Equal(revokedRun.Grant) || stored.RevokedAt == nil) {
			return errors.New("durable revocation did not retain the original accepted grant")
		}
		changed = revoked
		return err
	}); err != nil {
		t.Fatalf("revoke accepted run grant: %v", err)
	}
	if !changed {
		t.Fatal("first durable run grant revocation was reported unchanged")
	}
	_, err := recheckIntegrationRunGrant(
		t,
		pool,
		fixture.application.services.Authentication,
		fixture.scope,
		actor,
		check(revokedRun.RunID),
	)
	requireIntegrationRunGrantFailure(t, err, 403, "grant_unavailable", "refresh_authorization")

	if _, err := recheckIntegrationRunGrant(
		t,
		pool,
		fixture.application.services.Authentication,
		fixture.scope,
		actor,
		check(currentAuthorityRun.RunID),
	); err != nil {
		t.Fatalf("baseline recheck before current authority change: %v", err)
	}
	changedConnections := fixture.execScoped(t, fixture.scope, `
		UPDATE pm.connections
		SET revision = revision + 1, updated_at = clock_timestamp()
		WHERE org_id = $1 AND workspace_id = $2 AND connection_id = $3
	`, fixture.scope.OrgID, fixture.scope.WorkspaceID, fixture.connectionID)
	if changedConnections != 1 {
		t.Fatalf("changed current connection authorities = %d, want 1", changedConnections)
	}
	_, err = recheckIntegrationRunGrant(
		t,
		pool,
		fixture.application.services.Authentication,
		fixture.scope,
		actor,
		check(currentAuthorityRun.RunID),
	)
	requireIntegrationRunGrantFailure(t, err, 403, "grant_unavailable", "refresh_authorization")
}

func TestRunGrantCheckUsesCurrentAutomationAuthority(t *testing.T) {
	fixture := newServerIntegrationFixture(t)
	allowIntegrationRunOperation(t, fixture, integrationRunGrantOperation)
	browser := fixture.loginBrowser(t, integrationAuthTimeFresh, false)
	account := createIntegrationServiceAccount(
		t,
		fixture,
		browser,
		"run-grant-service-account",
		"Run grant service account",
		time.Now().UTC().Add(2*time.Hour).Truncate(time.Microsecond),
		[]string{fixture.connectionID},
		[]string{integrationRunGrantOperation},
	)
	pool := openIntegrationRunGrantPool(t, fixture)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	serviceIdentity, err := fixture.application.services.Authentication.AuthenticateServiceCredential(ctx, account.Credential)
	if err != nil {
		t.Fatalf("authenticate current service account for run grant: %v", err)
	}
	var revisions store.IdentityCollectionRevisions
	if err := store.WithinScope(ctx, pool, fixture.scope, serviceIdentity.Actor, func(ctx context.Context, tx *store.Transaction) error {
		var loadErr error
		revisions, loadErr = store.LoadIdentityCollectionRevisions(ctx, tx)
		return loadErr
	}); err != nil {
		t.Fatalf("load delegation collection revision for run grant: %v", err)
	}
	delegationRevision := revisions.Delegations
	delegationResult, err := fixture.application.services.Identity.CreateDelegation(ctx, serviceIdentity.Actor, fixture.scope, app.CreateDelegation{
		RequestedActor:  serviceIdentity.Principal,
		EffectiveActor:  fixture.operator,
		WorkspaceScopes: []string{fixture.scope.WorkspaceID},
		ConnectionIDs:   []string{fixture.connectionID},
		OperationIDs:    []string{integrationRunGrantOperation},
		ExpiresAt:       account.ExpiresAt.Add(-time.Hour),
		Control: app.MutationControl{
			IdempotencyKey:   "run-grant-delegation",
			IntentRevision:   delegationRevision,
			ExpectedRevision: &delegationRevision,
		},
	})
	if err != nil {
		t.Fatalf("create current one-hop delegation for run grant: %v", err)
	}
	if delegationResult.Mutation.Status != app.StatusCreated || delegationResult.Delegation.PolicyEpoch == 0 {
		t.Fatal("run grant delegation did not retain current durable authority")
	}
	delegatedActor, err := app.DelegatedActor(delegationResult.Delegation)
	if err != nil {
		t.Fatalf("construct current delegated run actor: %v", err)
	}

	var serviceAuthority store.ServiceAccountAuthority
	var membershipAuthority store.MembershipAuthority
	var connectionAuthority store.ConnectionAuthority
	if err := store.WithinScope(ctx, pool, fixture.scope, serviceIdentity.Actor, func(ctx context.Context, tx *store.Transaction) error {
		var loadErr error
		serviceAuthority, loadErr = store.LoadServiceAccountAuthority(ctx, tx, serviceIdentity.Principal.PrincipalID)
		if loadErr != nil {
			return loadErr
		}
		membershipAuthority, loadErr = store.LoadMembershipAuthority(ctx, tx, fixture.operator.PrincipalID)
		if loadErr != nil {
			return loadErr
		}
		connectionAuthority, loadErr = store.LoadConnectionAuthority(ctx, tx, fixture.connectionID)
		return loadErr
	}); err != nil {
		t.Fatalf("load current automation authority for run grant: %v", err)
	}

	serviceRun := admitIntegrationActorRunGrant(
		t,
		fixture,
		pool,
		integrationRunGrantOperation,
		serviceIdentity.Actor,
		contract.PolicyEpochs{Service: serviceAuthority.PolicyEpoch, Connection: connectionAuthority.Revision},
		serviceAuthority.ExpiresAt,
	)
	delegatedRun := admitIntegrationActorRunGrant(
		t,
		fixture,
		pool,
		integrationRunGrantOperation,
		delegatedActor,
		contract.PolicyEpochs{
			Membership: membershipAuthority.PolicyEpoch,
			Service:    serviceAuthority.PolicyEpoch,
			Delegation: delegationResult.Delegation.PolicyEpoch,
			Connection: connectionAuthority.Revision,
		},
		delegationResult.Delegation.ExpiresAt,
	)

	expiredSessions := fixture.execScoped(t, fixture.scope, `
		UPDATE pm.pm_sessions
		SET authenticated_at = NULL,
		    issued_at = clock_timestamp() - interval '2 hours',
		    expires_at = clock_timestamp() - interval '1 hour'
		WHERE deployment_id = $1 AND org_id = $2 AND principal_id = $3
		  AND revoked_at IS NULL
	`, fixture.deploymentID, fixture.operator.OrgID, fixture.operator.PrincipalID)
	if expiredSessions != 1 {
		t.Fatalf("expired automation-test login sessions = %d, want 1", expiredSessions)
	}
	expiredLogin := fixture.request(t, browser.client, http.MethodGet, "/v1/sessions/me", nil, nil)
	requireIntegrationStatus(t, expiredLogin, http.StatusUnauthorized, "expired automation-test browser login session")

	serviceCheck := app.RunGrantCheck{
		RunID:        serviceRun.RunID,
		Permission:   "operation.invoke",
		ResourceKind: auth.ResourceRun,
		ResourceID:   serviceRun.RunID,
		ConnectionID: fixture.connectionID,
		OperationID:  integrationRunGrantOperation,
	}
	checkedService, err := recheckIntegrationRunGrant(
		t, pool, fixture.application.services.Authentication, fixture.scope, serviceIdentity.Actor, serviceCheck,
	)
	if err != nil || !checkedService.Equal(serviceRun.Grant) {
		t.Fatalf("current service-account run grant after login expiry = %#v, %v", checkedService, err)
	}
	delegatedCheck := serviceCheck
	delegatedCheck.RunID = delegatedRun.RunID
	delegatedCheck.ResourceID = delegatedRun.RunID
	checkedDelegation, err := recheckIntegrationRunGrant(
		t, pool, fixture.application.services.Authentication, fixture.scope, delegatedActor, delegatedCheck,
	)
	if err != nil || !checkedDelegation.Equal(delegatedRun.Grant) {
		t.Fatalf("current delegated run grant after login expiry = %#v, %v", checkedDelegation, err)
	}

	if err := store.WithinScope(ctx, pool, fixture.scope, delegatedActor, func(ctx context.Context, tx *store.Transaction) error {
		revoked, unchanged, revokeErr := store.RevokeDelegation(ctx, tx, store.RevokeDelegationRequest{
			Scope:            fixture.scope,
			DelegationID:     delegationResult.Delegation.DelegationID,
			RequestedActor:   delegationResult.Delegation.RequestedActor,
			EffectiveActor:   delegationResult.Delegation.EffectiveActor,
			ParentGrantRef:   delegationResult.Delegation.ParentGrantRef,
			ParentCeiling:    delegationResult.Delegation.ParentCeiling,
			ConnectionIDs:    delegationResult.Delegation.ConnectionIDs,
			OperationIDs:     delegationResult.Delegation.OperationIDs,
			ExpiresAt:        delegationResult.Delegation.ExpiresAt,
			ExpectedRevision: delegationResult.Delegation.ControlRevision,
		})
		if revokeErr == nil && (unchanged || revoked.RevokedAt == nil) {
			return errors.New("current delegation revocation did not change durable authority")
		}
		return revokeErr
	}); err != nil {
		t.Fatalf("revoke current delegated run authority: %v", err)
	}
	_, err = recheckIntegrationRunGrant(
		t, pool, fixture.application.services.Authentication, fixture.scope, delegatedActor, delegatedCheck,
	)
	requireIntegrationRunGrantFailure(t, err, 403, "grant_unavailable", "refresh_authorization")
	if _, err := recheckIntegrationRunGrant(
		t, pool, fixture.application.services.Authentication, fixture.scope, serviceIdentity.Actor, serviceCheck,
	); err != nil {
		t.Fatalf("delegation revocation changed direct service authority: %v", err)
	}

	if err := store.WithinScope(ctx, pool, fixture.scope, serviceIdentity.Actor, func(ctx context.Context, tx *store.Transaction) error {
		revoked, unchanged, revokeErr := store.RevokeServiceAccount(ctx, tx, store.RevokeServiceAccountRequest{
			Scope:            fixture.scope,
			ServiceAccountID: serviceAuthority.ServiceAccountID,
			Permissions:      serviceAuthority.Permissions,
			ConnectionIDs:    serviceAuthority.ConnectionIDs,
			OperationIDs:     serviceAuthority.OperationIDs,
			ExpiresAt:        serviceAuthority.ExpiresAt,
			ExpectedRevision: serviceAuthority.ControlRevision,
		})
		if revokeErr == nil && (unchanged || revoked.RevokedAt == nil) {
			return errors.New("current service-account revocation did not change durable authority")
		}
		return revokeErr
	}); err != nil {
		t.Fatalf("revoke current service-account run authority: %v", err)
	}
	_, err = recheckIntegrationRunGrant(
		t, pool, fixture.application.services.Authentication, fixture.scope, serviceIdentity.Actor, serviceCheck,
	)
	requireIntegrationRunGrantFailure(t, err, 403, "grant_unavailable", "refresh_authorization")
}

func TestRunGrantCheckHidesAllWrongActorGrantStates(t *testing.T) {
	fixture := newServerIntegrationFixture(t)
	allowIntegrationRunOperation(t, fixture, integrationRunGrantOperation)
	pool := openIntegrationRunGrantPool(t, fixture)
	owner := contract.Actor{RequestedActor: fixture.operator, EffectiveActor: fixture.operator}
	wrongActor := contract.Actor{RequestedActor: fixture.grantTarget, EffectiveActor: fixture.grantTarget}
	current := admitIntegrationRunGrant(t, fixture, pool, integrationRunGrantOperation, 0)
	revoked := admitIntegrationRunGrant(t, fixture, pool, integrationRunGrantOperation, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.WithinScope(ctx, pool, fixture.scope, owner, func(ctx context.Context, tx *store.Transaction) error {
		_, _, err := store.RevokeRunExecutionGrant(ctx, tx, revoked.RunID, revoked.Grant.GrantEpoch)
		return err
	}); err != nil {
		t.Fatalf("seed revoked run grant for actor hiding: %v", err)
	}

	for _, testCase := range []struct {
		name  string
		runID string
	}{
		{name: "missing", runID: integrationRunGrantUUID(t)},
		{name: "current", runID: current.RunID},
		{name: "revoked", runID: revoked.RunID},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := recheckIntegrationRunGrant(
				t,
				pool,
				fixture.application.services.Authentication,
				fixture.scope,
				wrongActor,
				app.RunGrantCheck{
					RunID:        testCase.runID,
					Permission:   "operation.invoke",
					ResourceKind: auth.ResourceRun,
					ResourceID:   testCase.runID,
					ConnectionID: fixture.connectionID,
					OperationID:  integrationRunGrantOperation,
				},
			)
			requireIntegrationRunGrantFailure(t, err, 404, "resource_not_found", "correct_scope")
		})
	}
}

func allowIntegrationRunOperation(t *testing.T, fixture *serverIntegrationFixture, operationID string) {
	t.Helper()
	changed := fixture.execScoped(t, fixture.scope, `
		UPDATE pm.memberships
		SET operation_ids = $1
		WHERE org_id = $2 AND workspace_id = $3 AND identity_id = $4
	`, []string{operationID}, fixture.scope.OrgID, fixture.scope.WorkspaceID, fixture.operator.PrincipalID)
	if changed != 1 {
		t.Fatalf("memberships granted the integration operation = %d, want 1", changed)
	}
}

func openIntegrationRunGrantPool(t *testing.T, fixture *serverIntegrationFixture) *store.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := store.OpenPool(ctx, store.PoolConfig{
		ConnectionString:  fixture.runtimeDSN,
		MaxConns:          2,
		MaxConnLifetime:   time.Minute,
		MaxConnIdleTime:   time.Minute,
		HealthCheckPeriod: time.Minute,
	})
	if err != nil {
		t.Fatalf("open run grant integration pool: %T", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func admitIntegrationRunGrant(
	t *testing.T,
	fixture *serverIntegrationFixture,
	pool *store.Pool,
	operationID string,
	approvalEpoch uint64,
) integrationAcceptedRunGrant {
	t.Helper()
	return admitIntegrationActorRunGrant(
		t,
		fixture,
		pool,
		operationID,
		contract.Actor{RequestedActor: fixture.operator, EffectiveActor: fixture.operator},
		contract.PolicyEpochs{Membership: 1, Connection: 1, Approval: approvalEpoch},
		time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
	)
}

func admitIntegrationActorRunGrant(
	t *testing.T,
	fixture *serverIntegrationFixture,
	pool *store.Pool,
	operationID string,
	actor contract.Actor,
	policyEpochs contract.PolicyEpochs,
	expiresAt time.Time,
) integrationAcceptedRunGrant {
	t.Helper()
	runID := integrationRunGrantUUID(t)
	fingerprintBytes := sha256.Sum256([]byte("run-grant:" + runID))
	fingerprint := contract.SemanticDigest("sha256:" + hex.EncodeToString(fingerprintBytes[:]))
	approvalRefs := []string{}
	if policyEpochs.Approval != 0 {
		approvalRefs = []string{"approval-" + runID}
	}
	approvalRef := contract.AbsentValue()
	if len(approvalRefs) != 0 {
		approvalRef = contract.NewStringValue(approvalRefs[0])
	}
	policy := store.RunBudgetPolicy{
		Pending: store.AdmissionLimits{
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
	budget, err := store.NewRunBudgetSnapshot(fixture.scope, runID, "budget-"+runID, policy)
	if err != nil {
		t.Fatalf("construct run grant integration budget: %v", err)
	}
	grant := contract.ExecutionGrant{
		GrantRef:      "grant-" + runID,
		GrantEpoch:    1,
		Scope:         fixture.scope,
		Actor:         actor,
		ConnectionIDs: []string{fixture.connectionID},
		OperationIDs:  []string{operationID},
		ApprovalRefs:  approvalRefs,
		ExpiresAt:     expiresAt,
		PolicyEpochs:  policyEpochs,
	}
	binding, err := contract.NewAPIWorkflowBinding(runID)
	if err != nil {
		t.Fatalf("bind run grant integration workflow: %v", err)
	}
	payload := contract.SafePayload{
		References:  []contract.SafeReference{{Name: "run", Ref: runID}},
		Counts:      []contract.SafeCount{},
		ReasonCodes: []string{},
	}
	request := store.AdmissionRequest{
		BudgetSnapshot: budget,
		InputSnapshot: store.RunInputSnapshot{
			Scope: fixture.scope, RunID: runID, InputRef: "input-" + runID,
			ConnectionID:   fixture.connectionID,
			Input:          contract.NewObjectValue(map[string]contract.PresentValue{}),
			ResultControls: contract.ResultControls{},
			ApprovalRef:    approvalRef,
		},
		Key: store.AdmissionKey{
			PrincipalID:    actor.RequestedActor.PrincipalID,
			OperationID:    operationID,
			TargetID:       fixture.connectionID,
			IdempotencyKey: "grant-check-" + runID,
		},
		Spec: contract.RunSpec{
			RunID:             runID,
			OrgID:             fixture.scope.OrgID,
			WorkspaceID:       fixture.scope.WorkspaceID,
			Actor:             actor,
			ExecutionGrantRef: grant.GrantRef,
			Origin:            contract.RunOriginAPI,
			OperationOrPlanRef: contract.OperationOrPlanRef{
				Kind: contract.OperationReference, ID: operationID, Revision: "revision-1",
			},
			InputRefs:              []string{"input-" + runID},
			SchemaRefs:             []string{},
			CapabilityVersions:     []contract.CapabilityRequirement{},
			Budget:                 budget.Binding(),
			IdempotencyFingerprint: fingerprint,
		},
		Grant:           grant,
		WorkflowBinding: binding,
		Receipt: store.Receipt{
			ReceiptID: "receipt-" + runID,
			Kind:      "admission",
			RunID:     runID,
			Payload:   payload,
		},
		Audit: store.AuditEntry{
			AuditID:   "audit-" + runID,
			Action:    "run_admitted",
			TargetRef: runID,
			Payload:   payload,
		},
		Event:  store.EventInput{EventID: "event-" + runID, Type: "run_admitted", Payload: payload},
		Outbox: store.OutboxCommand{CommandID: "outbox-" + runID, Kind: "start_run", PayloadRef: runID},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var accepted store.AdmissionResult
	if err := store.WithinScope(ctx, pool, fixture.scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		var admitErr error
		accepted, admitErr = store.AdmitRun(ctx, tx, request)
		return admitErr
	}); err != nil {
		t.Fatalf("admit run grant integration row: %v", err)
	}
	if accepted.RunID != runID || accepted.Replayed {
		t.Fatal("run grant integration admission did not create its exact durable identity")
	}
	return integrationAcceptedRunGrant{RunID: runID, Grant: grant}
}

func recheckIntegrationRunGrant(
	t *testing.T,
	pool *store.Pool,
	authorization *auth.Service,
	scope contract.Scope,
	actor contract.Actor,
	check app.RunGrantCheck,
) (contract.ExecutionGrant, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var grant contract.ExecutionGrant
	err := store.WithinScope(ctx, pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		var checkErr error
		grant, checkErr = app.RecheckRunGrant(ctx, tx, authorization, check)
		return checkErr
	})
	return grant, err
}

func requireIntegrationRunGrantFailure(t *testing.T, err error, status int, code, retryAction string) {
	t.Helper()
	if err == nil {
		t.Fatalf("run grant check succeeded; want %s", code)
	}
	var failure *app.Failure
	if !errors.As(err, &failure) {
		t.Fatalf("run grant check error = %T; want safe application failure", err)
	}
	if failure.HTTPStatus != status || failure.Code != code || failure.RetryAction != retryAction {
		t.Fatalf("run grant failure = %#v; want status %d code %q retry %q", failure, status, code, retryAction)
	}
}

func integrationRunGrantUUID(t *testing.T) string {
	t.Helper()
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		t.Fatal("generate run grant integration identity")
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}
