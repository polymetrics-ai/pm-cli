package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/credentials"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

type integrationCurrentSessionDocument struct {
	AuthenticatedAt *time.Time `json:"authenticated_at"`
	Transport       string     `json:"transport"`
	Principal       struct {
		Kind        string `json:"kind"`
		PrincipalID string `json:"principal_id"`
	} `json:"principal"`
	Memberships struct {
		Items []struct {
			OrgID       string `json:"org_id"`
			WorkspaceID string `json:"workspace_id"`
		} `json:"items"`
	} `json:"memberships"`
}

type integrationDeviceChallengeDocument struct {
	ChallengeID         string    `json:"challenge_id"`
	UserCode            string    `json:"user_code"`
	DeviceProof         string    `json:"device_proof"`
	VerificationURI     string    `json:"verification_uri"`
	ExpiresAt           time.Time `json:"expires_at"`
	PollIntervalSeconds int       `json:"poll_interval_seconds"`
}

type integrationDeviceSessionDocument struct {
	SessionID           string    `json:"session_id"`
	PMSessionCredential string    `json:"pm_session_credential"`
	ExpiresAt           time.Time `json:"expires_at"`
	Principal           struct {
		Kind        string `json:"kind"`
		PrincipalID string `json:"principal_id"`
	} `json:"principal"`
}

func TestAuthenticatedApplicationBrowserDeviceAndSession(t *testing.T) {
	fixture := newServerIntegrationFixture(t)
	operationRef := "fixture.read@revision-1"
	allowIntegrationRunOperation(t, fixture, operationRef)
	fixture.pauseForBrowserProbe(t)

	loginPage := fixture.request(t, fixture.newHTTPClient(t, false), http.MethodGet, "/login", nil, nil)
	requireIntegrationStatus(t, loginPage, http.StatusOK, "local browser login page")
	if !bytes.Contains(loginPage.body, []byte("Sign in")) {
		t.Fatal("local browser login page did not render its sign-in action")
	}
	readiness := fixture.request(t, fixture.newHTTPClient(t, false), http.MethodGet, "/health/ready", nil, nil)
	requireIntegrationStatus(t, readiness, http.StatusServiceUnavailable, "readiness without dispatch")
	var readinessState Readiness
	decodeIntegrationJSON(t, readiness.body, &readinessState, "readiness")
	if !readinessState.Admission || readinessState.Dispatch || readinessState.Draining {
		t.Fatal("readiness did not isolate the intentionally unavailable dispatch capability")
	}

	browser := fixture.loginBrowser(t, integrationAuthTimeFresh, false)
	current := fixture.request(t, browser.client, http.MethodGet, "/v1/sessions/me", nil, nil)
	requireIntegrationStatus(t, current, http.StatusOK, "current browser session")
	var browserSession integrationCurrentSessionDocument
	decodeIntegrationJSON(t, current.body, &browserSession, "current browser session")
	if browserSession.Transport != "browser_session" || browserSession.Principal.PrincipalID != fixture.operator.PrincipalID ||
		browserSession.AuthenticatedAt == nil || len(browserSession.Memberships.Items) != 1 ||
		browserSession.Memberships.Items[0].WorkspaceID != fixture.scope.WorkspaceID {
		t.Fatal("current browser session did not preserve its signed identity and sole authorized workspace")
	}
	invocationKey := "authenticated-handler-unavailable"
	invocation := fixture.requestJSON(t, browser.client, http.MethodPost,
		"/v1/workspaces/"+fixture.scope.WorkspaceID+"/invocations",
		map[string]any{
			"protocol": map[string]any{
				"wire_version":      "pm.api/v1",
				"required_features": []string{},
				"request_key":       invocationKey,
				"intent_digest":     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"expected_revision": "revision-1",
			},
			"connection_ref": fixture.connectionID,
			"operation_ref":  operationRef,
			"lane":           "direct_read",
			"input": map[string]any{
				"presence": "value",
				"type":     "object",
				"fields":   map[string]any{},
			},
			"result_controls": map[string]any{},
		},
		integrationMutationHeaders(browser, fixture.origin, invocationKey, ""))
	requireIntegrationStatus(t, invocation, http.StatusServiceUnavailable, "authenticated invocation without registered handler")
	requireIntegrationProblemCode(t, invocation, "capability_unavailable")

	logoutHeaders := browser.mutationHeaders(fixture.origin)
	logout := fixture.request(t, browser.client, http.MethodDelete, "/v1/sessions/current", nil, logoutHeaders)
	requireIntegrationStatus(t, logout, http.StatusNoContent, "browser logout")
	afterLogout := fixture.request(t, browser.client, http.MethodGet, "/v1/sessions/me", nil, nil)
	requireIntegrationStatus(t, afterLogout, http.StatusUnauthorized, "revoked browser session")

	approver := fixture.loginBrowser(t, integrationAuthTimeFresh, false)
	deviceClient := fixture.newHTTPClient(t, false)
	start := fixture.requestJSON(t, deviceClient, http.MethodPost, "/v1/sessions/device", map[string]any{
		"device_id": "server-integration-device",
	}, nil)
	requireIntegrationStatus(t, start, http.StatusCreated, "device login start")
	var challenge integrationDeviceChallengeDocument
	decodeIntegrationJSON(t, start.body, &challenge, "device login challenge")
	if challenge.VerificationURI != fixture.origin+"/login/device" ||
		!challenge.ExpiresAt.After(time.Now()) {
		t.Fatal("device login challenge omitted its real returned proof or bounded poll contract")
	}

	fixture.makeDevicePollEligible(t, challenge.ChallengeID)
	pending := fixture.requestJSON(t, deviceClient, http.MethodPost, "/v1/sessions/device/exchange", map[string]any{
		"challenge_id": challenge.ChallengeID,
		"proof":        challenge.DeviceProof,
	}, nil)
	requireIntegrationStatus(t, pending, http.StatusConflict, "pending device exchange")
	requireIntegrationProblemCode(t, pending, "device_authorization_pending")

	approveHeaders := approver.mutationHeaders(fixture.origin)
	approved := fixture.requestJSON(t, approver.client, http.MethodPost, "/v1/sessions/device/approve", map[string]any{
		"challenge_id": challenge.ChallengeID,
		"user_code":    challenge.UserCode,
	}, approveHeaders)
	requireIntegrationStatus(t, approved, http.StatusNoContent, "browser device approval")
	fixture.makeDevicePollEligible(t, challenge.ChallengeID)
	exchange := fixture.requestJSON(t, deviceClient, http.MethodPost, "/v1/sessions/device/exchange", map[string]any{
		"challenge_id": challenge.ChallengeID,
		"proof":        challenge.DeviceProof,
	}, nil)
	requireIntegrationStatus(t, exchange, http.StatusOK, "approved device exchange")
	var deviceSession integrationDeviceSessionDocument
	decodeIntegrationJSON(t, exchange.body, &deviceSession, "device session")
	if deviceSession.SessionID == "" || deviceSession.PMSessionCredential == "" ||
		deviceSession.Principal.PrincipalID != fixture.operator.PrincipalID || !deviceSession.ExpiresAt.After(time.Now()) {
		t.Fatal("approved device exchange did not return its original PM session credential")
	}

	used := fixture.requestJSON(t, deviceClient, http.MethodPost, "/v1/sessions/device/exchange", map[string]any{
		"challenge_id": challenge.ChallengeID,
		"proof":        challenge.DeviceProof,
	}, nil)
	requireIntegrationStatus(t, used, http.StatusConflict, "consumed device exchange")
	requireIntegrationProblemCode(t, used, "login_challenge_used")
	if bytes.Contains(used.body, []byte(deviceSession.PMSessionCredential)) {
		t.Fatal("consumed device exchange reflected the one-time session credential")
	}

	bearerHeaders := http.Header{"Authorization": []string{"Bearer " + deviceSession.PMSessionCredential}}
	deviceCurrent := fixture.request(t, deviceClient, http.MethodGet, "/v1/sessions/me", nil, bearerHeaders)
	requireIntegrationStatus(t, deviceCurrent, http.StatusOK, "current device session")
	var currentDevice integrationCurrentSessionDocument
	decodeIntegrationJSON(t, deviceCurrent.body, &currentDevice, "current device session")
	if currentDevice.Transport != "device_session" || currentDevice.AuthenticatedAt != nil ||
		currentDevice.Principal.PrincipalID != fixture.operator.PrincipalID {
		t.Fatal("current device session projected browser-only authentication state")
	}
	deviceLogout := fixture.request(t, deviceClient, http.MethodDelete, "/v1/sessions/current", nil, bearerHeaders)
	requireIntegrationStatus(t, deviceLogout, http.StatusNoContent, "device logout")
	revokedDevice := fixture.request(t, deviceClient, http.MethodGet, "/v1/sessions/me", nil, bearerHeaders)
	requireIntegrationStatus(t, revokedDevice, http.StatusUnauthorized, "revoked device session")
}

type integrationMutationDocument struct {
	ResourceID        string `json:"resource_id"`
	ResultRevision    string `json:"result_revision"`
	ResultDisposition string `json:"result_disposition"`
	Replay            bool   `json:"replay"`
	OutcomeReference  struct {
		Kind string `json:"kind"`
		Ref  string `json:"ref"`
	} `json:"outcome_reference"`
}

type integrationMembershipPage struct {
	CollectionRevision string `json:"collection_revision"`
	Items              []struct {
		MembershipRef    string  `json:"membership_ref"`
		PrincipalRef     string  `json:"principal_ref"`
		BoundScopeDigest string  `json:"bound_scope_digest"`
		ControlRevision  string  `json:"control_revision"`
		RevokedAt        *string `json:"revoked_at"`
	} `json:"items"`
}

func TestAuthenticatedApplicationDrainPreservesAcceptedRun(t *testing.T) {
	fixture := newServerIntegrationFixture(t)
	if changed := fixture.execScoped(t, fixture.scope, `
		UPDATE pm.memberships
		SET permissions = ARRAY(
		        SELECT DISTINCT value FROM unnest(permissions || $1::text[]) AS value ORDER BY value
		    ),
		    resource_grants = ARRAY(
		        SELECT DISTINCT value FROM unnest(resource_grants || $2::text[]) AS value ORDER BY value
		    ),
		    operation_ids = $3
		WHERE org_id = $4 AND workspace_id = $5 AND identity_id = $6
	`, []string{"run.read", "event.read"}, []string{"permission:run.read", "permission:event.read"},
		[]string{"fixture.read"}, fixture.scope.OrgID, fixture.scope.WorkspaceID, fixture.operator.PrincipalID); changed != 1 {
		t.Fatal("grant both read authorities to the owned lifecycle fixture")
	}
	browser := fixture.loginBrowser(t, integrationAuthTimeFresh, false)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	observer, err := store.OpenPool(ctx, store.PoolConfig{
		ConnectionString:  fixture.runtimeDSN,
		MaxConns:          2,
		MaxConnLifetime:   time.Minute,
		MaxConnIdleTime:   time.Minute,
		HealthCheckPeriod: time.Minute,
	})
	if err != nil {
		t.Fatalf("open independent accepted-run observer: %T", err)
	}
	defer observer.Close()

	// Seed an already committed foundation record, not an HTTP admission or
	// Temporal execution claim. The observer is independent of the API's pool.
	runBytes := make([]byte, 16)
	if _, err := rand.Read(runBytes); err != nil {
		t.Fatal("generate isolated lifecycle run identity")
	}
	runBytes[6] = (runBytes[6] & 0x0f) | 0x40
	runBytes[8] = (runBytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(runBytes)
	runID := encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
	binding, err := contract.NewAPIWorkflowBinding(runID)
	if err != nil {
		t.Fatalf("bind isolated lifecycle run: %v", err)
	}
	actor := contract.Actor{RequestedActor: fixture.operator, EffectiveActor: fixture.operator}
	fingerprint := sha256.Sum256([]byte(fixture.scope.WorkspaceID))
	digest := contract.SemanticDigest("sha256:" + hex.EncodeToString(fingerprint[:]))
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
	budget, err := store.NewRunBudgetSnapshot(fixture.scope, runID, "fixture-budget-"+runID, policy)
	if err != nil {
		t.Fatalf("construct lifecycle run budget: %v", err)
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
			Lane:           contract.LaneDirectRead,
			ConnectionID:   fixture.connectionID,
			Input:          contract.NewObjectValue(map[string]contract.PresentValue{}),
			ResultControls: contract.ResultControls{},
			ApprovalRef:    contract.AbsentValue(),
		},
		Key: store.AdmissionKey{
			PrincipalID: fixture.operator.PrincipalID, OperationID: "fixture.read",
			TargetID: fixture.connectionID, IdempotencyKey: "lifecycle-existing-run",
		},
		Spec: contract.RunSpec{
			RunID: runID, OrgID: fixture.scope.OrgID, WorkspaceID: fixture.scope.WorkspaceID,
			Actor: actor, ExecutionGrantRef: "fixture-grant-" + runID, Origin: contract.RunOriginAPI,
			OperationOrPlanRef: contract.OperationOrPlanRef{
				Kind: contract.OperationReference, ID: "fixture.read", Revision: "revision-1",
			},
			InputRefs: []string{"input-" + runID}, SchemaRefs: []string{}, CapabilityVersions: []contract.CapabilityRequirement{},
			Budget:                 budget.Binding(),
			IdempotencyFingerprint: digest,
		},
		Grant: contract.ExecutionGrant{
			GrantRef: "fixture-grant-" + runID, GrantEpoch: 1, Scope: fixture.scope, Actor: actor,
			ConnectionIDs: []string{fixture.connectionID}, OperationIDs: []string{"fixture.read"}, ApprovalRefs: []string{},
			ExpiresAt:    time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
			PolicyEpochs: contract.PolicyEpochs{Membership: 1, Connection: 1},
		},
		WorkflowBinding: binding,
		Receipt:         store.Receipt{ReceiptID: "receipt-" + runID, Kind: "admission", RunID: runID, Payload: payload},
		Audit:           store.AuditEntry{AuditID: "audit-" + runID, Action: "run_admitted", TargetRef: runID, Payload: payload},
		Event:           store.EventInput{EventID: "event-" + runID, Type: "run_admitted", Payload: payload},
		Outbox:          store.OutboxCommand{CommandID: "outbox-" + runID, Kind: "start_run", PayloadRef: runID},
	}
	var accepted store.AdmissionResult
	if err := store.WithinScope(ctx, observer, fixture.scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		var admitErr error
		accepted, admitErr = store.AdmitRun(ctx, tx, request)
		return admitErr
	}); err != nil {
		t.Fatalf("commit pre-existing lifecycle run: %v", err)
	}
	readRun := func() contract.RunRecord {
		t.Helper()
		var record contract.RunRecord
		if err := store.WithinScope(ctx, observer, fixture.scope, actor, func(ctx context.Context, tx *store.Transaction) error {
			replayed, err := store.LookupAdmission(ctx, tx, request.Key)
			if err != nil {
				return err
			}
			if replayed.RunID != accepted.RunID || !replayed.AcceptedAt.Equal(accepted.AcceptedAt) {
				return errors.New("durable admission identity changed across API lifecycle")
			}
			record, err = store.LoadRun(ctx, tx, accepted.RunID)
			return err
		}); err != nil {
			t.Fatalf("observe independently committed lifecycle run: %v", err)
		}
		return record
	}
	before := readRun()
	if before.State != contract.RunStateAccepted || before.CancelRequested {
		t.Fatal("lifecycle fixture did not contain a committed uncancelled accepted run")
	}
	streamRequest, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fixture.origin+"/v1/workspaces/"+fixture.scope.WorkspaceID+"/runs/"+runID+"/events", nil)
	if err != nil {
		t.Fatal("construct production event stream request")
	}
	stream, err := browser.client.Do(streamRequest)
	if err != nil {
		t.Fatalf("open production event stream: %v", err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK || !strings.HasPrefix(stream.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("production event stream returned HTTP %d instead of SSE", stream.StatusCode)
	}
	scanner := bufio.NewScanner(stream.Body)
	var eventName string
	var snapshotData strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if eventName == "" && snapshotData.Len() == 0 {
				continue
			}
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch name {
		case "event":
			eventName = value
		case "data":
			snapshotData.WriteString(value)
			snapshotData.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read production stream snapshot: %v", err)
	}
	var snapshot struct {
		Run struct {
			RunRef string `json:"run_ref"`
		} `json:"run"`
		Cursor string `json:"cursor"`
	}
	if err := json.Unmarshal([]byte(snapshotData.String()), &snapshot); err != nil ||
		eventName != "snapshot" || snapshot.Run.RunRef != runID || snapshot.Cursor == "" {
		t.Fatal("production stream did not return the authorized run and signed recovery cursor")
	}
	if err := fixture.application.Close(); err != nil {
		t.Fatalf("drain authenticated API application: %v", err)
	}
	for scanner.Scan() {
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("production stream did not reach clean EOF during drain: %v", err)
	}
	after := readRun()
	if !reflect.DeepEqual(after, before) {
		t.Fatal("API shutdown changed or cancelled the independently durable accepted run")
	}
}

func TestAuthenticatedApplicationScopeMutationAndCredentialIngress(t *testing.T) {
	fixture := newServerIntegrationFixture(t)
	browser := fixture.loginBrowser(t, integrationAuthTimeFresh, false)

	workspaces := fixture.request(t, browser.client, http.MethodGet, "/v1/workspaces", nil, nil)
	requireIntegrationStatus(t, workspaces, http.StatusOK, "authorized workspace list")
	if !bytes.Contains(workspaces.body, []byte(fixture.scope.WorkspaceID)) ||
		bytes.Contains(workspaces.body, []byte(fixture.foreignScope.WorkspaceID)) ||
		bytes.Contains(workspaces.body, []byte("Concealed foreign workspace")) {
		t.Fatal("workspace listing did not conceal the real foreign scope")
	}
	foreignWorkspace := fixture.request(t, browser.client, http.MethodGet, "/v1/workspaces/"+fixture.foreignScope.WorkspaceID, nil, nil)
	requireIntegrationStatus(t, foreignWorkspace, http.StatusNotFound, "foreign workspace concealment")
	if bytes.Contains(foreignWorkspace.body, []byte("Concealed foreign workspace")) ||
		bytes.Contains(foreignWorkspace.body, []byte(fixture.accountWitness)) {
		t.Fatal("foreign workspace denial exposed concealed authority metadata")
	}

	grantReturnedID := exerciseIntegrationMembershipMutation(t, fixture, browser)
	if grantReturnedID == "" {
		t.Fatal("membership mutation did not use its returned server-owned identity")
	}
	exerciseIntegrationCredentialIngress(t, fixture, browser)
}

func exerciseIntegrationMembershipMutation(
	t *testing.T,
	fixture *serverIntegrationFixture,
	browser integrationBrowserSession,
) string {
	t.Helper()
	resourceGrants := []string{"permission:workspace.read"}
	grantDigest := integrationMembershipGrantDigest(t, fixture.scope, fixture.grantTarget, "reader", resourceGrants, 1)

	malformedKey := "membership-malformed"
	malformedDocument := integrationMembershipGrantRequest(malformedKey, grantDigest, "1", fixture.grantTarget.PrincipalID, "reader", resourceGrants)
	malformedBytes, err := json.Marshal(malformedDocument)
	if err != nil {
		t.Fatal("encode malformed membership integration request")
	}
	malformedBytes = bytes.Replace(malformedBytes, []byte(`"role":"reader"`), []byte(`"role":"reader","role":"operator"`), 1)
	if !bytes.Contains(malformedBytes, []byte(`"role":"operator"`)) {
		t.Fatal("membership duplicate-field fixture was not materialized")
	}
	malformedHeaders := integrationMutationHeaders(browser, fixture.origin, malformedKey, "1")
	malformedHeaders.Set("Content-Type", "application/json")
	malformed := fixture.request(t, browser.client, http.MethodPost, "/v1/workspaces/"+fixture.scope.WorkspaceID+"/memberships", malformedBytes, malformedHeaders)
	requireIntegrationStatus(t, malformed, http.StatusBadRequest, "duplicate typed membership field")

	duplicateHeaderKey := "membership-duplicate-header"
	duplicateHeaderDocument := integrationMembershipGrantRequest(duplicateHeaderKey, grantDigest, "1", fixture.grantTarget.PrincipalID, "reader", resourceGrants)
	duplicateHeaders := integrationMutationHeaders(browser, fixture.origin, duplicateHeaderKey, "1")
	duplicateHeaders.Add("If-Match", "1")
	duplicateHeader := fixture.requestJSON(t, browser.client, http.MethodPost, "/v1/workspaces/"+fixture.scope.WorkspaceID+"/memberships", duplicateHeaderDocument, duplicateHeaders)
	requireIntegrationStatus(t, duplicateHeader, http.StatusBadRequest, "duplicate If-Match header")

	preconditionKey := "membership-needs-precondition"
	preconditionDocument := integrationMembershipGrantRequest(preconditionKey, grantDigest, "1", fixture.grantTarget.PrincipalID, "reader", resourceGrants)
	preconditionHeaders := integrationMutationHeaders(browser, fixture.origin, preconditionKey, "")
	precondition := fixture.requestJSON(t, browser.client, http.MethodPost, "/v1/workspaces/"+fixture.scope.WorkspaceID+"/memberships", preconditionDocument, preconditionHeaders)
	requireIntegrationStatus(t, precondition, http.StatusPreconditionRequired, "fresh membership precondition")

	grantKey := "membership-current-grant"
	grantDocument := integrationMembershipGrantRequest(grantKey, grantDigest, "1", fixture.grantTarget.PrincipalID, "reader", resourceGrants)
	grantHeaders := integrationMutationHeaders(browser, fixture.origin, grantKey, "1")
	granted := fixture.requestJSON(t, browser.client, http.MethodPost, "/v1/workspaces/"+fixture.scope.WorkspaceID+"/memberships", grantDocument, grantHeaders)
	requireIntegrationStatus(t, granted, http.StatusCreated, "current membership grant")
	var grant integrationMutationDocument
	decodeIntegrationJSON(t, granted.body, &grant, "membership grant result")
	if grant.ResourceID == "" || grant.ResourceID == fixture.grantTarget.PrincipalID || grant.Replay ||
		grant.ResultDisposition != "created" || grant.OutcomeReference.Ref != grant.ResourceID || granted.header.Get("ETag") != `"1"` {
		t.Fatal("fresh membership grant did not return its durable server-owned result")
	}

	replayHeaders := integrationMutationHeaders(browser, fixture.origin, grantKey, "")
	replayed := fixture.requestJSON(t, browser.client, http.MethodPost, "/v1/workspaces/"+fixture.scope.WorkspaceID+"/memberships", grantDocument, replayHeaders)
	requireIntegrationStatus(t, replayed, http.StatusOK, "equal membership replay before fresh precondition")
	var replay integrationMutationDocument
	decodeIntegrationJSON(t, replayed.body, &replay, "membership replay result")
	if !replay.Replay || replay.ResourceID != grant.ResourceID || replay.ResultRevision != grant.ResultRevision ||
		replay.ResultDisposition != "prior_mutation_result" || replay.OutcomeReference.Ref != grant.ResourceID {
		t.Fatal("equal membership replay did not return the original durable result")
	}

	conflictDigest := integrationMembershipGrantDigest(t, fixture.scope, fixture.grantTarget, "operator", resourceGrants, 1)
	conflictDocument := integrationMembershipGrantRequest(grantKey, conflictDigest, "1", fixture.grantTarget.PrincipalID, "operator", resourceGrants)
	conflict := fixture.requestJSON(t, browser.client, http.MethodPost, "/v1/workspaces/"+fixture.scope.WorkspaceID+"/memberships", conflictDocument, replayHeaders)
	requireIntegrationStatus(t, conflict, http.StatusConflict, "conflicting membership intent")
	requireIntegrationProblemCode(t, conflict, "idempotency_conflict")

	listed := fixture.request(t, browser.client, http.MethodGet, "/v1/workspaces/"+fixture.scope.WorkspaceID+"/memberships", nil, nil)
	requireIntegrationStatus(t, listed, http.StatusOK, "durable membership list")
	var page integrationMembershipPage
	decodeIntegrationJSON(t, listed.body, &page, "durable membership list")
	membership := findIntegrationMembership(t, page, grant.ResourceID)
	if membership.PrincipalRef != fixture.grantTarget.PrincipalID || membership.BoundScopeDigest == "" ||
		membership.ControlRevision != "1" || membership.RevokedAt != nil {
		t.Fatal("fresh membership was not durably observable at its returned identity")
	}

	revokeDigest := integrationMembershipRevokeDigest(t, fixture.scope, grant.ResourceID, membership.BoundScopeDigest, 1)
	preconditionRevokeKey := "membership-revoke-needs-precondition"
	preconditionRevokeDocument := integrationMembershipRevokeRequest(preconditionRevokeKey, revokeDigest, "1", grant.ResourceID, membership.BoundScopeDigest)
	preconditionRevokeHeaders := integrationMutationHeaders(browser, fixture.origin, preconditionRevokeKey, "")
	preconditionRevoke := fixture.requestJSON(t, browser.client, http.MethodPost,
		"/v1/workspaces/"+fixture.scope.WorkspaceID+"/memberships/"+grant.ResourceID+"/revoke",
		preconditionRevokeDocument, preconditionRevokeHeaders)
	requireIntegrationStatus(t, preconditionRevoke, http.StatusPreconditionRequired, "fresh membership revocation precondition")

	revokeKey := "membership-current-revoke"
	revokeDocument := integrationMembershipRevokeRequest(revokeKey, revokeDigest, "1", grant.ResourceID, membership.BoundScopeDigest)
	revokeHeaders := integrationMutationHeaders(browser, fixture.origin, revokeKey, "1")
	revoked := fixture.requestJSON(t, browser.client, http.MethodPost,
		"/v1/workspaces/"+fixture.scope.WorkspaceID+"/memberships/"+grant.ResourceID+"/revoke",
		revokeDocument, revokeHeaders)
	requireIntegrationStatus(t, revoked, http.StatusOK, "current membership revocation")
	var revoke integrationMutationDocument
	decodeIntegrationJSON(t, revoked.body, &revoke, "membership revocation result")
	if revoke.ResourceID != grant.ResourceID || revoke.ResultDisposition != "revoked" || revoke.Replay ||
		revoked.header.Get("ETag") != `"2"` {
		t.Fatal("membership revocation did not change the exact current returned grant")
	}

	listedAfterRevoke := fixture.request(t, browser.client, http.MethodGet, "/v1/workspaces/"+fixture.scope.WorkspaceID+"/memberships", nil, nil)
	requireIntegrationStatus(t, listedAfterRevoke, http.StatusOK, "durable revoked membership list")
	var revokedPage integrationMembershipPage
	decodeIntegrationJSON(t, listedAfterRevoke.body, &revokedPage, "durable revoked membership list")
	durableRevocation := findIntegrationMembership(t, revokedPage, grant.ResourceID)
	if durableRevocation.ControlRevision != "2" || durableRevocation.RevokedAt == nil || *durableRevocation.RevokedAt == "" {
		t.Fatal("current membership revocation was not durably observable")
	}
	return grant.ResourceID
}

func exerciseIntegrationCredentialIngress(
	t *testing.T,
	fixture *serverIntegrationFixture,
	fresh integrationBrowserSession,
) {
	t.Helper()
	privatePath := "/admin/v1/workspaces/" + fixture.scope.WorkspaceID + "/connections/" + fixture.connectionID + "/credentials"
	foreignPath := "/admin/v1/workspaces/" + fixture.foreignScope.WorkspaceID + "/connections/" + fixture.foreignConnectionID + "/credentials"
	deniedSecret := []byte("pm-private-denied-canary-4f8a6e13")
	deniedBody := integrationCredentialBody(deniedSecret)
	restoreWrappingKey := fixture.hideWrappingKey(t)

	missingAuthenticationHeaders := http.Header{
		"Origin":          []string{fixture.origin},
		"X-CSRF-Token":    []string{fresh.csrf},
		"Idempotency-Key": []string{"credential-missing-auth"},
	}
	missingAuthentication := fixture.requestJSON(t, fixture.newHTTPClient(t, false), http.MethodPost, privatePath, deniedBody, missingAuthenticationHeaders)
	requireIntegrationStatus(t, missingAuthentication, http.StatusUnauthorized, "private credential missing authentication")

	missingOriginHeaders := http.Header{
		"X-CSRF-Token":    []string{fresh.csrf},
		"Idempotency-Key": []string{"credential-missing-origin"},
	}
	missingOrigin := fixture.requestJSON(t, fresh.client, http.MethodPost, privatePath, deniedBody, missingOriginHeaders)
	requireIntegrationStatus(t, missingOrigin, http.StatusForbidden, "private credential missing origin")

	missingCSRFHeaders := http.Header{
		"Origin":          []string{fixture.origin},
		"Idempotency-Key": []string{"credential-missing-csrf"},
	}
	missingCSRF := fixture.requestJSON(t, fresh.client, http.MethodPost, privatePath, deniedBody, missingCSRFHeaders)
	requireIntegrationStatus(t, missingCSRF, http.StatusForbidden, "private credential missing CSRF")

	absentAuthenticationTime := fixture.loginBrowser(t, integrationAuthTimeAbsent, false)
	absentHeaders := integrationCredentialHeaders(absentAuthenticationTime, fixture.origin, "credential-no-auth-time")
	absent := fixture.requestJSON(t, absentAuthenticationTime.client, http.MethodPost, privatePath, deniedBody, absentHeaders)
	requireIntegrationStatus(t, absent, http.StatusForbidden, "private credential absent signed authentication time")
	requireIntegrationProblemCode(t, absent, "reauthentication_required")

	staleAuthenticationTime := fixture.loginBrowser(t, integrationAuthTimeStale, false)
	staleHeaders := integrationCredentialHeaders(staleAuthenticationTime, fixture.origin, "credential-stale-auth-time")
	stale := fixture.requestJSON(t, staleAuthenticationTime.client, http.MethodPost, privatePath, deniedBody, staleHeaders)
	requireIntegrationStatus(t, stale, http.StatusForbidden, "private credential stale signed authentication time")
	requireIntegrationProblemCode(t, stale, "reauthentication_required")

	foreignHeaders := integrationCredentialHeaders(fresh, fixture.origin, "credential-foreign-scope")
	foreign := fixture.requestJSON(t, fresh.client, http.MethodPost, foreignPath, deniedBody, foreignHeaders)
	requireIntegrationStatus(t, foreign, http.StatusNotFound, "private credential foreign authority")
	requireIntegrationProblemCode(t, foreign, "resource_not_found")
	for _, denial := range []integrationHTTPResponse{
		missingAuthentication,
		missingOrigin,
		missingCSRF,
		absent,
		stale,
		foreign,
	} {
		if integrationProjectionContainsSecret(denial.body, deniedSecret) {
			t.Fatal("private credential denial reflected submitted secret material")
		}
	}

	for _, scope := range []contract.Scope{fixture.scope, fixture.foreignScope} {
		encrypted, lifecycle, receipts := fixture.credentialStorageCounts(t, scope)
		if encrypted != 0 || lifecycle != 0 || receipts != 0 {
			t.Fatal("private credential denial reached key-backed storage")
		}
	}
	restoreWrappingKey()

	secret := []byte("pm-private-valid-canary-8a5e1f27e744")
	body := integrationCredentialBody(secret)
	validHeaders := integrationCredentialHeaders(fresh, fixture.origin, "credential-valid-current")
	stored := fixture.requestJSON(t, fresh.client, http.MethodPost, privatePath, body, validHeaders)
	requireIntegrationStatus(t, stored, http.StatusOK, "valid private credential ingress")
	var status struct {
		CredentialRef     string `json:"credential_ref"`
		CiphertextVersion int64  `json:"ciphertext_version"`
		LifecycleRevision int64  `json:"lifecycle_revision"`
		Status            string `json:"status"`
	}
	decodeIntegrationJSON(t, stored.body, &status, "private credential status")
	if status.CredentialRef != "credential-"+fixture.connectionID || status.CiphertextVersion != 1 ||
		status.LifecycleRevision != 1 || status.Status != "active" || stored.header.Get("ETag") != `"1"` ||
		integrationProjectionContainsSecret(stored.body, secret) ||
		integrationProjectionContainsSecret(stored.body, fixture.wrappingKey) ||
		bytes.Contains(stored.body, []byte(integrationSecretReference)) {
		t.Fatal("private credential ingress did not return only its opaque durable status")
	}

	replayed := fixture.requestJSON(t, fresh.client, http.MethodPost, privatePath, body, validHeaders)
	requireIntegrationStatus(t, replayed, http.StatusOK, "private credential exact replay")
	if !bytes.Equal(stored.body, replayed.body) || replayed.header.Get("ETag") != stored.header.Get("ETag") {
		t.Fatal("private credential exact replay changed its original safe result")
	}
	conflictingSecret := []byte("pm-private-conflicting-canary")
	conflictingBody := integrationCredentialBody(conflictingSecret)
	conflict := fixture.requestJSON(t, fresh.client, http.MethodPost, privatePath, conflictingBody, validHeaders)
	requireIntegrationStatus(t, conflict, http.StatusConflict, "private credential conflicting replay")
	requireIntegrationProblemCode(t, conflict, "idempotency_conflict")
	if integrationProjectionContainsSecret(conflict.body, conflictingSecret) {
		t.Fatal("private credential conflict reflected submitted secret material")
	}

	encryptedCount, lifecycleCount, receiptCount := fixture.credentialStorageCounts(t, fixture.scope)
	if encryptedCount != 1 || lifecycleCount != 1 || receiptCount != 1 {
		t.Fatal("valid private credential ingress did not persist exactly one versioned encrypted result")
	}
	observeAndResolveIntegrationCredential(t, fixture, secret)

	connection := fixture.request(t, fresh.client, http.MethodGet,
		"/v1/workspaces/"+fixture.scope.WorkspaceID+"/connections/"+fixture.connectionID, nil, nil)
	requireIntegrationStatus(t, connection, http.StatusOK, "ordinary connection after credential ingress")
	connections := fixture.request(t, fresh.client, http.MethodGet,
		"/v1/workspaces/"+fixture.scope.WorkspaceID+"/connections", nil, nil)
	requireIntegrationStatus(t, connections, http.StatusOK, "ordinary connection list after credential ingress")
	current := fixture.request(t, fresh.client, http.MethodGet, "/v1/sessions/me", nil, nil)
	requireIntegrationStatus(t, current, http.StatusOK, "ordinary session after credential ingress")
	ordinaryProjection := bytes.Join([][]byte{connection.body, connections.body, current.body}, []byte("\n"))
	if integrationProjectionContainsSecret(ordinaryProjection, secret) ||
		integrationProjectionContainsSecret(ordinaryProjection, fixture.wrappingKey) ||
		bytes.Contains(ordinaryProjection, []byte(integrationSecretReference)) {
		t.Fatal("ordinary JSON projected provider credential or wrapping-key material")
	}
}

func observeAndResolveIntegrationCredential(t *testing.T, fixture *serverIntegrationFixture, secret []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := store.OpenPool(ctx, store.PoolConfig{
		ConnectionString:  fixture.runtimeDSN,
		MaxConns:          2,
		MaxConnLifetime:   time.Minute,
		MaxConnIdleTime:   time.Minute,
		HealthCheckPeriod: time.Minute,
	})
	if err != nil {
		t.Fatalf("open independent credential observer: %T", err)
	}
	defer pool.Close()
	actor := contract.Actor{RequestedActor: fixture.operator, EffectiveActor: fixture.operator}
	var lifecycle store.CredentialLifecycle
	var encrypted store.EncryptedSecret
	err = store.WithinScope(ctx, pool, fixture.scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		var loadErr error
		lifecycle, loadErr = store.LoadCredentialLifecycle(ctx, tx, fixture.connectionID)
		if loadErr != nil {
			return loadErr
		}
		encrypted, loadErr = store.LoadEncryptedSecret(ctx, tx, lifecycle.CredentialRef, lifecycle.CiphertextVersion)
		return loadErr
	})
	if err != nil {
		t.Fatalf("observe durable encrypted credential: %v", err)
	}
	if lifecycle.CiphertextVersion != 1 || lifecycle.Revision != 1 || lifecycle.Status != store.CredentialLifecycleActive ||
		encrypted.Binding.CiphertextVersion != 1 || encrypted.Binding.CredentialRef != lifecycle.CredentialRef ||
		encrypted.Binding.KeyRef != integrationSecretReference || bytes.Equal(encrypted.Ciphertext, secret) ||
		integrationProjectionContainsSecret(encrypted.Ciphertext, secret) {
		t.Fatal("durable credential state was not an opaque encrypted version bound to its current lifecycle")
	}

	material, err := credentials.ResolveForEffect(
		ctx,
		pool,
		func(_ context.Context, reference string) ([]byte, error) {
			if reference != integrationSecretReference {
				return nil, errors.New("unexpected wrapping-key reference")
			}
			return fixture.wrappingKey, nil
		},
		func(_ context.Context, tx *store.Transaction, access credentials.Access) error {
			if tx == nil || access.Action != credentials.ActionResolveEffect || access.Stage != credentials.StagePreEffect ||
				access.Effect == nil || !access.Scope.Equal(fixture.scope) || !access.Actor.Equal(actor) ||
				access.Connection.ConnectionID != fixture.connectionID {
				return errors.New("unexpected credential effect authorization")
			}
			return nil
		},
		credentials.ResolveRequest{
			Scope:   fixture.scope,
			Actor:   actor,
			Binding: encrypted.Binding,
			Effect: credentials.EffectAccess{
				ExecutionGrantRef:  "integration-grant",
				OperationID:        "fixture.read",
				OperationRevision:  "revision-1",
				EffectRef:          "effect-1",
				DispatchID:         "dispatch-1",
				AttemptID:          "attempt-1",
				PermitRef:          "permit-1",
				ApprovalRef:        "approval-1",
				Deadline:           time.Now().UTC().Add(30 * time.Second),
				ReconciliationOnly: false,
			},
		},
	)
	if err != nil {
		t.Fatalf("resolve persisted integration credential for an admitted effect: %v", err)
	}
	defer material.Destroy()
	if err := material.Use(func(plaintext []byte) error {
		if !bytes.Equal(plaintext, secret) {
			return errors.New("decrypted credential does not match the stored private input")
		}
		return nil
	}); err != nil {
		t.Fatalf("consume resolved integration credential: %v", err)
	}
}

func integrationMutationHeaders(
	session integrationBrowserSession,
	origin string,
	idempotencyKey string,
	ifMatch string,
) http.Header {
	headers := session.mutationHeaders(origin)
	headers.Set("Idempotency-Key", idempotencyKey)
	if ifMatch != "" {
		headers.Set("If-Match", ifMatch)
	}
	return headers
}

func integrationCredentialHeaders(
	session integrationBrowserSession,
	origin string,
	idempotencyKey string,
) http.Header {
	headers := session.mutationHeaders(origin)
	headers.Set("Idempotency-Key", idempotencyKey)
	return headers
}

func integrationCredentialBody(secret []byte) map[string]any {
	return map[string]any{
		"credential_value": base64.StdEncoding.EncodeToString(secret),
		"scopes":           []string{"issues:read", "issues:write"},
	}
}

func integrationMembershipGrantRequest(
	requestKey string,
	digest contract.SemanticDigest,
	revision string,
	principalID string,
	role string,
	resourceGrants []string,
) map[string]any {
	return map[string]any{
		"protocol": map[string]any{
			"wire_version":      "pm.api/v1",
			"required_features": []string{},
			"request_key":       requestKey,
			"intent_digest":     string(digest),
			"expected_revision": revision,
		},
		"membership": map[string]any{
			"principal_ref":   principalID,
			"role":            role,
			"resource_grants": resourceGrants,
			"expires_at":      nil,
		},
	}
}

func integrationMembershipRevokeRequest(
	requestKey string,
	digest contract.SemanticDigest,
	revision string,
	membershipID string,
	boundScopeDigest string,
) map[string]any {
	return map[string]any{
		"protocol": map[string]any{
			"wire_version":      "pm.api/v1",
			"required_features": []string{},
			"request_key":       requestKey,
			"intent_digest":     string(digest),
			"expected_revision": revision,
		},
		"membership": map[string]any{
			"membership_ref":     membershipID,
			"bound_scope_digest": boundScopeDigest,
			"expires_at":         nil,
		},
	}
}

func integrationMembershipGrantDigest(
	t *testing.T,
	scope contract.Scope,
	principal contract.Principal,
	role string,
	resourceGrants []string,
	revision uint64,
) contract.SemanticDigest {
	t.Helper()
	intent := struct {
		Operation      string             `json:"operation"`
		Scope          contract.Scope     `json:"scope"`
		Principal      contract.Principal `json:"principal"`
		Role           string             `json:"role"`
		ResourceGrants []string           `json:"resource_grants"`
		ExpiresAt      *time.Time         `json:"expires_at"`
		IntentRevision uint64             `json:"expected_revision"`
	}{
		Operation:      "membership.grant",
		Scope:          scope,
		Principal:      principal,
		Role:           role,
		ResourceGrants: resourceGrants,
		IntentRevision: revision,
	}
	return integrationCanonicalDigest(t, intent)
}

func integrationMembershipRevokeDigest(
	t *testing.T,
	scope contract.Scope,
	membershipID string,
	boundScopeDigest string,
	revision uint64,
) contract.SemanticDigest {
	t.Helper()
	intent := struct {
		Operation        string                  `json:"operation"`
		Scope            contract.Scope          `json:"scope"`
		MembershipID     string                  `json:"membership_id"`
		BoundScopeDigest contract.SemanticDigest `json:"bound_scope_digest"`
		ExpiresAt        *time.Time              `json:"expires_at"`
		IntentRevision   uint64                  `json:"expected_revision"`
	}{
		Operation:        "membership.revoke",
		Scope:            scope,
		MembershipID:     membershipID,
		BoundScopeDigest: contract.SemanticDigest(boundScopeDigest),
		IntentRevision:   revision,
	}
	return integrationCanonicalDigest(t, intent)
}

func integrationCanonicalDigest(t *testing.T, value any) contract.SemanticDigest {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode integration mutation intent: %T", err)
	}
	digest, err := contract.CanonicalDigest(encoded)
	if err != nil {
		t.Fatalf("canonicalize integration mutation intent: %v", err)
	}
	return digest
}

func findIntegrationMembership(t *testing.T, page integrationMembershipPage, membershipID string) struct {
	MembershipRef    string  `json:"membership_ref"`
	PrincipalRef     string  `json:"principal_ref"`
	BoundScopeDigest string  `json:"bound_scope_digest"`
	ControlRevision  string  `json:"control_revision"`
	RevokedAt        *string `json:"revoked_at"`
} {
	t.Helper()
	for _, membership := range page.Items {
		if membership.MembershipRef == membershipID {
			return membership
		}
	}
	t.Fatal("returned membership identity was not durably listed")
	return struct {
		MembershipRef    string  `json:"membership_ref"`
		PrincipalRef     string  `json:"principal_ref"`
		BoundScopeDigest string  `json:"bound_scope_digest"`
		ControlRevision  string  `json:"control_revision"`
		RevokedAt        *string `json:"revoked_at"`
	}{}
}

func integrationProjectionContainsSecret(projection []byte, secret []byte) bool {
	if len(secret) == 0 {
		return false
	}
	return bytes.Contains(projection, secret) ||
		bytes.Contains(projection, []byte(base64.StdEncoding.EncodeToString(secret))) ||
		bytes.Contains(projection, []byte(hex.EncodeToString(secret))) ||
		bytes.Contains(projection, []byte(fmt.Sprint(secret)))
}

func requireIntegrationStatus(t *testing.T, response integrationHTTPResponse, wanted int, operation string) {
	t.Helper()
	if response.status != wanted {
		t.Fatalf("%s status = %d, want %d", operation, response.status, wanted)
	}
}

func requireIntegrationProblemCode(t *testing.T, response integrationHTTPResponse, wanted string) {
	t.Helper()
	var problem struct {
		Code string `json:"code"`
	}
	decodeIntegrationJSON(t, response.body, &problem, "integration problem")
	if problem.Code != wanted {
		t.Fatalf("integration problem code = %q, want %q", problem.Code, wanted)
	}
}

func decodeIntegrationJSON(t *testing.T, document []byte, destination any, description string) {
	t.Helper()
	if err := json.Unmarshal(document, destination); err != nil {
		t.Fatalf("decode %s: %v", description, err)
	}
}
