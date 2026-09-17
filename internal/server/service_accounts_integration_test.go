package server

import (
	"bytes"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type integrationServiceAccount struct {
	ResourceID      string
	Credential      string
	ControlRevision string
	GrantRevision   uint64
	ExpiresAt       time.Time
}

type integrationServiceAccountCreationDocument struct {
	integrationMutationDocument
	Credential string `json:"pm_automation_credential"`
}

type integrationServiceAccountMetadata struct {
	ServiceAccountRef string     `json:"service_account_ref"`
	DisplayName       *string    `json:"display_name"`
	WorkspaceScopes   []string   `json:"workspace_scopes"`
	ConnectionScopes  []string   `json:"connection_scopes"`
	OperationScopes   []string   `json:"operation_scopes"`
	GrantRevision     string     `json:"grant_revision"`
	ControlRevision   string     `json:"control_revision"`
	ExpiresAt         time.Time  `json:"expires_at"`
	RevokedAt         *time.Time `json:"revoked_at"`
}

type integrationServiceAccountPage struct {
	CollectionRevision string                              `json:"collection_revision"`
	Items              []integrationServiceAccountMetadata `json:"items"`
}

func TestAuthenticatedApplicationServiceAccountMutations(t *testing.T) {
	fixture := newServerIntegrationFixture(t)
	operationIDs := []string{"fixture.read@revision-1"}
	operatorMembershipID := "membership-" + fixture.operator.PrincipalID
	if rows := fixture.execScoped(t, fixture.scope, `
		UPDATE pm.memberships
		SET operation_ids = $1
		WHERE membership_id = $2 AND identity_id = $3
	`, operationIDs, operatorMembershipID, fixture.operator.PrincipalID); rows != 1 {
		t.Fatal("service-account scenario did not bind the exact owned operator operation ceiling")
	}
	browser := fixture.loginBrowser(t, integrationAuthTimeFresh, false)

	workspaceScopes := []string{fixture.scope.WorkspaceID}
	connectionIDs := []string{fixture.connectionID}
	expiresAt := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Microsecond)
	displayName := "Integration automation"
	collectionPath := "/v1/workspaces/" + fixture.scope.WorkspaceID + "/service-accounts"

	initialPage, initialList := readIntegrationServiceAccountPage(t, fixture, browser)
	initialRevision := parseIntegrationServiceAccountRevision(t, initialPage.CollectionRevision)
	if len(initialPage.Items) != 0 || initialList.header.Get("ETag") != quotedIntegrationRevision(initialPage.CollectionRevision) {
		t.Fatal("fresh service-account collection did not expose its current empty revision")
	}

	missingCreateKey := "service-account-create-needs-precondition"
	missingCreateDigest := integrationServiceAccountCreateDigest(
		t, fixture.scope, displayName, workspaceScopes, connectionIDs, operationIDs, expiresAt, initialRevision,
	)
	missingCreateDocument := integrationServiceAccountCreateRequest(
		missingCreateKey, missingCreateDigest, initialRevision, displayName, workspaceScopes, connectionIDs, operationIDs, expiresAt,
	)
	missingCreate := fixture.requestJSON(t, browser.client, http.MethodPost, collectionPath, missingCreateDocument,
		integrationMutationHeaders(browser, fixture.origin, missingCreateKey, ""))
	requireIntegrationStatus(t, missingCreate, http.StatusPreconditionRequired, "service-account create without If-Match")
	requireIntegrationProblemCode(t, missingCreate, "control_revision_required")

	staleCreateKey := "service-account-create-stale-precondition"
	staleCreateRevision := initialRevision + 1
	staleCreateDigest := integrationServiceAccountCreateDigest(
		t, fixture.scope, displayName, workspaceScopes, connectionIDs, operationIDs, expiresAt, staleCreateRevision,
	)
	staleCreateDocument := integrationServiceAccountCreateRequest(
		staleCreateKey, staleCreateDigest, staleCreateRevision, displayName, workspaceScopes, connectionIDs, operationIDs, expiresAt,
	)
	staleCreate := fixture.requestJSON(t, browser.client, http.MethodPost, collectionPath, staleCreateDocument,
		integrationMutationHeaders(browser, fixture.origin, staleCreateKey, strconv.FormatUint(staleCreateRevision, 10)))
	requireIntegrationStatus(t, staleCreate, http.StatusPreconditionFailed, "service-account create with stale If-Match")
	requireIntegrationProblemCode(t, staleCreate, "control_revision_mismatch")
	pageAfterCreateRefusals, _ := readIntegrationServiceAccountPage(t, fixture, browser)
	if pageAfterCreateRefusals.CollectionRevision != initialPage.CollectionRevision || len(pageAfterCreateRefusals.Items) != 0 {
		t.Fatal("refused service-account creation changed the collection")
	}

	createKey := "service-account-create-current"
	created := createIntegrationServiceAccount(
		t, fixture, browser, createKey, displayName, expiresAt, connectionIDs, operationIDs,
	)
	originalCreateDigest := integrationServiceAccountCreateDigest(
		t, fixture.scope, displayName, workspaceScopes, connectionIDs, operationIDs, expiresAt, initialRevision,
	)
	originalCreateDocument := integrationServiceAccountCreateRequest(
		createKey, originalCreateDigest, initialRevision, displayName, workspaceScopes, connectionIDs, operationIDs, expiresAt,
	)
	requireIntegrationAutomationCredentialStatus(t, fixture, created.Credential, "service-account-created-credential", http.StatusServiceUnavailable)

	pageBeforeCreateReplay, listBeforeCreateReplay := readIntegrationServiceAccountPage(t, fixture, browser)
	if len(pageBeforeCreateReplay.Items) != 1 {
		t.Fatal("original service-account creation did not persist exactly one resource")
	}
	requireIntegrationServiceAccountCredentialAbsent(t, listBeforeCreateReplay, created.Credential)
	createReplay := fixture.requestJSON(t, browser.client, http.MethodPost, collectionPath, originalCreateDocument,
		integrationMutationHeaders(browser, fixture.origin, createKey, ""))
	requireIntegrationStatus(t, createReplay, http.StatusOK, "service-account exact create replay")
	var replayedCreate integrationMutationDocument
	decodeIntegrationJSON(t, createReplay.body, &replayedCreate, "service-account create replay")
	if !replayedCreate.Replay || replayedCreate.ResourceID != created.ResourceID ||
		replayedCreate.ResultRevision != pageBeforeCreateReplay.CollectionRevision ||
		replayedCreate.ResultDisposition != "prior_mutation_result" ||
		replayedCreate.OutcomeReference.Kind != "service_account" ||
		replayedCreate.OutcomeReference.Ref != created.ResourceID ||
		createReplay.header.Get("ETag") != quotedIntegrationRevision(created.ControlRevision) {
		t.Fatal("service-account create replay did not return its original safe metadata")
	}
	requireIntegrationServiceAccountCredentialAbsent(t, createReplay, created.Credential)
	pageAfterCreateReplay, _ := readIntegrationServiceAccountPage(t, fixture, browser)
	if pageAfterCreateReplay.CollectionRevision != pageBeforeCreateReplay.CollectionRevision || len(pageAfterCreateReplay.Items) != 1 {
		t.Fatal("service-account create replay caused another effect")
	}

	changedDisplayName := "Changed integration automation"
	// An unchanged caller digest cannot conceal changed canonical typed input.
	// Replay identity belongs to the server, not the caller's digest claim.
	changedCreateDocument := integrationServiceAccountCreateRequest(
		createKey, originalCreateDigest, initialRevision, changedDisplayName, workspaceScopes, connectionIDs, operationIDs, expiresAt,
	)
	changedCreate := fixture.requestJSON(t, browser.client, http.MethodPost, collectionPath, changedCreateDocument,
		integrationMutationHeaders(browser, fixture.origin, createKey, ""))
	requireIntegrationStatus(t, changedCreate, http.StatusConflict, "service-account changed create intent")
	requireIntegrationProblemCode(t, changedCreate, "idempotency_conflict")
	requireIntegrationServiceAccountCredentialAbsent(t, changedCreate, created.Credential)
	pageAfterCreateConflict, _ := readIntegrationServiceAccountPage(t, fixture, browser)
	if pageAfterCreateConflict.CollectionRevision != pageBeforeCreateReplay.CollectionRevision || len(pageAfterCreateConflict.Items) != 1 {
		t.Fatal("conflicting service-account creation changed durable state")
	}

	currentControlRevision := parseIntegrationServiceAccountRevision(t, created.ControlRevision)
	revokePath := collectionPath + "/" + created.ResourceID + "/revoke"
	missingRevokeKey := "service-account-revoke-needs-precondition"
	missingRevokeDigest := integrationServiceAccountRevokeDigest(
		t, fixture.scope, created.ResourceID, strconv.FormatUint(created.GrantRevision, 10), created.ExpiresAt, currentControlRevision,
	)
	missingRevokeDocument := integrationServiceAccountRevokeRequest(
		missingRevokeKey, missingRevokeDigest, currentControlRevision, created.ResourceID,
		strconv.FormatUint(created.GrantRevision, 10), created.ExpiresAt,
	)
	missingRevoke := fixture.requestJSON(t, browser.client, http.MethodPost, revokePath, missingRevokeDocument,
		integrationMutationHeaders(browser, fixture.origin, missingRevokeKey, ""))
	requireIntegrationStatus(t, missingRevoke, http.StatusPreconditionRequired, "service-account revoke without If-Match")
	requireIntegrationProblemCode(t, missingRevoke, "control_revision_required")

	staleRevokeKey := "service-account-revoke-stale-precondition"
	staleRevokeRevision := currentControlRevision + 1
	staleGrantRevision := strconv.FormatUint(staleRevokeRevision, 10)
	staleRevokeDigest := integrationServiceAccountRevokeDigest(
		t, fixture.scope, created.ResourceID, staleGrantRevision, created.ExpiresAt, staleRevokeRevision,
	)
	staleRevokeDocument := integrationServiceAccountRevokeRequest(
		staleRevokeKey, staleRevokeDigest, staleRevokeRevision, created.ResourceID, staleGrantRevision, created.ExpiresAt,
	)
	staleRevoke := fixture.requestJSON(t, browser.client, http.MethodPost, revokePath, staleRevokeDocument,
		integrationMutationHeaders(browser, fixture.origin, staleRevokeKey, staleGrantRevision))
	requireIntegrationStatus(t, staleRevoke, http.StatusPreconditionFailed, "service-account revoke with stale If-Match")
	requireIntegrationProblemCode(t, staleRevoke, "control_revision_mismatch")
	pageAfterRevokeRefusals, _ := readIntegrationServiceAccountPage(t, fixture, browser)
	unchangedAccount := findIntegrationServiceAccountMetadata(t, pageAfterRevokeRefusals, created.ResourceID)
	if pageAfterRevokeRefusals.CollectionRevision != pageBeforeCreateReplay.CollectionRevision ||
		unchangedAccount.ControlRevision != created.ControlRevision || unchangedAccount.RevokedAt != nil {
		t.Fatal("refused service-account revocation changed durable state")
	}

	revokeKey := "service-account-revoke-current"
	revokeDigest := integrationServiceAccountRevokeDigest(
		t, fixture.scope, created.ResourceID, strconv.FormatUint(created.GrantRevision, 10), created.ExpiresAt, currentControlRevision,
	)
	revokeDocument := integrationServiceAccountRevokeRequest(
		revokeKey, revokeDigest, currentControlRevision, created.ResourceID,
		strconv.FormatUint(created.GrantRevision, 10), created.ExpiresAt,
	)
	revoked := fixture.requestJSON(t, browser.client, http.MethodPost, revokePath, revokeDocument,
		integrationMutationHeaders(browser, fixture.origin, revokeKey, created.ControlRevision))
	requireIntegrationStatus(t, revoked, http.StatusOK, "service-account current revocation")
	var revokeResult integrationMutationDocument
	decodeIntegrationJSON(t, revoked.body, &revokeResult, "service-account revocation")
	revokedControlRevision := strconv.FormatUint(currentControlRevision+1, 10)
	if revokeResult.Replay || revokeResult.ResourceID != created.ResourceID ||
		revokeResult.ResultRevision != revokedControlRevision || revokeResult.ResultDisposition != "revoked" ||
		revokeResult.OutcomeReference.Kind != "service_account" || revokeResult.OutcomeReference.Ref != created.ResourceID ||
		revoked.header.Get("ETag") != quotedIntegrationRevision(revokedControlRevision) {
		t.Fatal("service-account revocation did not return its durable safe metadata")
	}
	requireIntegrationServiceAccountCredentialAbsent(t, revoked, created.Credential)

	pageAfterRevoke, listAfterRevoke := readIntegrationServiceAccountPage(t, fixture, browser)
	revokedAccount := findIntegrationServiceAccountMetadata(t, pageAfterRevoke, created.ResourceID)
	if revokedAccount.ControlRevision != revokedControlRevision || revokedAccount.GrantRevision != revokedControlRevision ||
		revokedAccount.RevokedAt == nil || !revokedAccount.ExpiresAt.Equal(created.ExpiresAt) {
		t.Fatal("service-account revocation was not durably observable through safe metadata")
	}
	requireIntegrationServiceAccountCredentialAbsent(t, listAfterRevoke, created.Credential)
	requireIntegrationAutomationCredentialStatus(t, fixture, created.Credential, "service-account-revoked-credential", http.StatusUnauthorized)

	revokeReplay := fixture.requestJSON(t, browser.client, http.MethodPost, revokePath, revokeDocument,
		integrationMutationHeaders(browser, fixture.origin, revokeKey, ""))
	requireIntegrationStatus(t, revokeReplay, http.StatusOK, "service-account exact revoke replay")
	var replayedRevoke integrationMutationDocument
	decodeIntegrationJSON(t, revokeReplay.body, &replayedRevoke, "service-account revoke replay")
	if !replayedRevoke.Replay || replayedRevoke.ResourceID != created.ResourceID ||
		replayedRevoke.ResultRevision != revokeResult.ResultRevision ||
		replayedRevoke.ResultDisposition != "prior_mutation_result" ||
		replayedRevoke.OutcomeReference.Kind != "service_account" ||
		replayedRevoke.OutcomeReference.Ref != created.ResourceID ||
		revokeReplay.header.Get("ETag") != revoked.header.Get("ETag") {
		t.Fatal("service-account revoke replay did not return its original safe metadata")
	}
	requireIntegrationServiceAccountCredentialAbsent(t, revokeReplay, created.Credential)
	pageAfterRevokeReplay, _ := readIntegrationServiceAccountPage(t, fixture, browser)
	if pageAfterRevokeReplay.CollectionRevision != pageAfterRevoke.CollectionRevision ||
		findIntegrationServiceAccountMetadata(t, pageAfterRevokeReplay, created.ResourceID).ControlRevision != revokedControlRevision {
		t.Fatal("service-account revoke replay caused another effect")
	}

	changedRevokeExpiry := created.ExpiresAt.Add(-time.Microsecond)
	changedRevokeDigest := integrationServiceAccountRevokeDigest(
		t, fixture.scope, created.ResourceID, strconv.FormatUint(created.GrantRevision, 10), changedRevokeExpiry, currentControlRevision,
	)
	changedRevokeDocument := integrationServiceAccountRevokeRequest(
		revokeKey, changedRevokeDigest, currentControlRevision, created.ResourceID,
		strconv.FormatUint(created.GrantRevision, 10), changedRevokeExpiry,
	)
	changedRevoke := fixture.requestJSON(t, browser.client, http.MethodPost, revokePath, changedRevokeDocument,
		integrationMutationHeaders(browser, fixture.origin, revokeKey, ""))
	requireIntegrationStatus(t, changedRevoke, http.StatusConflict, "service-account changed revoke intent")
	requireIntegrationProblemCode(t, changedRevoke, "idempotency_conflict")
	requireIntegrationServiceAccountCredentialAbsent(t, changedRevoke, created.Credential)
	pageAfterRevokeConflict, _ := readIntegrationServiceAccountPage(t, fixture, browser)
	if pageAfterRevokeConflict.CollectionRevision != pageAfterRevoke.CollectionRevision ||
		findIntegrationServiceAccountMetadata(t, pageAfterRevokeConflict, created.ResourceID).ControlRevision != revokedControlRevision {
		t.Fatal("conflicting service-account revocation changed durable state")
	}

	replacement := createIntegrationServiceAccount(
		t, fixture, browser, "service-account-create-replacement", displayName, expiresAt, connectionIDs, operationIDs,
	)
	if replacement.ResourceID == created.ResourceID || replacement.Credential == created.Credential {
		t.Fatal("authorized revoke-and-replace recovered or rotated the old service-account identity")
	}
	requireIntegrationAutomationCredentialStatus(t, fixture, replacement.Credential, "service-account-replacement-credential", http.StatusServiceUnavailable)
	pageAfterReplacement, listAfterReplacement := readIntegrationServiceAccountPage(t, fixture, browser)
	if len(pageAfterReplacement.Items) != 2 ||
		findIntegrationServiceAccountMetadata(t, pageAfterReplacement, created.ResourceID).RevokedAt == nil ||
		findIntegrationServiceAccountMetadata(t, pageAfterReplacement, replacement.ResourceID).RevokedAt != nil {
		t.Fatal("authorized service-account replacement did not create a distinct active identity")
	}
	requireIntegrationServiceAccountCredentialAbsent(t, listAfterReplacement, created.Credential, replacement.Credential)

	memberships := fixture.request(t, browser.client, http.MethodGet,
		"/v1/workspaces/"+fixture.scope.WorkspaceID+"/memberships", nil, nil)
	requireIntegrationStatus(t, memberships, http.StatusOK, "operator membership before replay authority removal")
	var membershipPage integrationMembershipPage
	decodeIntegrationJSON(t, memberships.body, &membershipPage, "operator membership before replay authority removal")
	operatorMembership := findIntegrationMembership(t, membershipPage, operatorMembershipID)
	operatorMembershipRevision := parseIntegrationServiceAccountRevision(t, operatorMembership.ControlRevision)
	authorityRevokeKey := "service-account-replay-authority-revoke"
	authorityRevokeDigest := integrationMembershipRevokeDigest(
		t, fixture.scope, operatorMembershipID, operatorMembership.BoundScopeDigest, operatorMembershipRevision,
	)
	authorityRevokeDocument := integrationMembershipRevokeRequest(
		authorityRevokeKey, authorityRevokeDigest, operatorMembership.ControlRevision,
		operatorMembershipID, operatorMembership.BoundScopeDigest,
	)
	authorityRevoked := fixture.requestJSON(t, browser.client, http.MethodPost,
		"/v1/workspaces/"+fixture.scope.WorkspaceID+"/memberships/"+operatorMembershipID+"/revoke",
		authorityRevokeDocument,
		integrationMutationHeaders(browser, fixture.origin, authorityRevokeKey, operatorMembership.ControlRevision))
	requireIntegrationStatus(t, authorityRevoked, http.StatusOK, "operator authority revocation")

	unauthorizedCreateReplay := fixture.requestJSON(t, browser.client, http.MethodPost, collectionPath, originalCreateDocument,
		integrationMutationHeaders(browser, fixture.origin, createKey, ""))
	requireIntegrationStatus(t, unauthorizedCreateReplay, http.StatusForbidden, "service-account create replay without current authority")
	requireIntegrationProblemCode(t, unauthorizedCreateReplay, "grant_unavailable")
	requireIntegrationServiceAccountCredentialAbsent(t, unauthorizedCreateReplay, created.Credential, replacement.Credential)
	unauthorizedRevokeReplay := fixture.requestJSON(t, browser.client, http.MethodPost, revokePath, revokeDocument,
		integrationMutationHeaders(browser, fixture.origin, revokeKey, ""))
	requireIntegrationStatus(t, unauthorizedRevokeReplay, http.StatusForbidden, "service-account revoke replay without current authority")
	requireIntegrationProblemCode(t, unauthorizedRevokeReplay, "grant_unavailable")
	requireIntegrationServiceAccountCredentialAbsent(t, unauthorizedRevokeReplay, created.Credential, replacement.Credential)
}

func createIntegrationServiceAccount(
	t *testing.T,
	fixture *serverIntegrationFixture,
	browser integrationBrowserSession,
	requestKey string,
	displayName string,
	expiresAt time.Time,
	connections, operations []string,
) integrationServiceAccount {
	t.Helper()
	if expiresAt.Location() != time.UTC || expiresAt.Nanosecond()%int(time.Microsecond) != 0 {
		t.Fatal("service-account integration expiry must use PostgreSQL-compatible UTC precision")
	}
	page, _ := readIntegrationServiceAccountPage(t, fixture, browser)
	collectionRevision := parseIntegrationServiceAccountRevision(t, page.CollectionRevision)
	workspaceScopes := []string{fixture.scope.WorkspaceID}
	digest := integrationServiceAccountCreateDigest(
		t, fixture.scope, displayName, workspaceScopes, connections, operations, expiresAt, collectionRevision,
	)
	document := integrationServiceAccountCreateRequest(
		requestKey, digest, collectionRevision, displayName, workspaceScopes, connections, operations, expiresAt,
	)
	response := fixture.requestJSON(t, browser.client, http.MethodPost,
		"/v1/workspaces/"+fixture.scope.WorkspaceID+"/service-accounts",
		document,
		integrationMutationHeaders(browser, fixture.origin, requestKey, page.CollectionRevision))
	requireIntegrationStatus(t, response, http.StatusCreated, "service-account creation")
	var creation integrationServiceAccountCreationDocument
	decodeIntegrationJSON(t, response.body, &creation, "service-account creation")
	if creation.ResourceID == "" || creation.Credential == "" || creation.Replay ||
		creation.ResultRevision != strconv.FormatUint(collectionRevision+1, 10) ||
		creation.ResultDisposition != "created" || creation.OutcomeReference.Kind != "service_account" ||
		creation.OutcomeReference.Ref != creation.ResourceID || response.header.Get("ETag") != `"1"` {
		t.Fatal("service-account creation did not return its durable identity and one-time credential")
	}

	storedPage, storedResponse := readIntegrationServiceAccountPage(t, fixture, browser)
	stored := findIntegrationServiceAccountMetadata(t, storedPage, creation.ResourceID)
	grantRevision := parseIntegrationServiceAccountRevision(t, stored.GrantRevision)
	if storedPage.CollectionRevision != creation.ResultRevision || stored.ControlRevision == "" ||
		stored.DisplayName == nil || *stored.DisplayName != displayName ||
		!slices.Equal(stored.WorkspaceScopes, workspaceScopes) ||
		!slices.Equal(stored.ConnectionScopes, connections) ||
		!slices.Equal(stored.OperationScopes, operations) ||
		!stored.ExpiresAt.Equal(expiresAt) || stored.RevokedAt != nil {
		t.Fatal("created service account was not durably readable as safe scoped metadata")
	}
	requireIntegrationServiceAccountCredentialAbsent(t, storedResponse, creation.Credential)
	return integrationServiceAccount{
		ResourceID:      creation.ResourceID,
		Credential:      creation.Credential,
		ControlRevision: stored.ControlRevision,
		GrantRevision:   grantRevision,
		ExpiresAt:       stored.ExpiresAt,
	}
}

func integrationServiceAccountCreateDigest(
	t *testing.T,
	scope contract.Scope,
	displayName string,
	workspaceScopes, connectionIDs, operationIDs []string,
	expiresAt time.Time,
	revision uint64,
) contract.SemanticDigest {
	t.Helper()
	intentDisplayName := displayName
	intent := struct {
		Operation       string         `json:"operation"`
		Scope           contract.Scope `json:"scope"`
		DisplayName     *string        `json:"display_name"`
		WorkspaceScopes []string       `json:"workspace_scopes"`
		ConnectionIDs   []string       `json:"connection_ids"`
		OperationIDs    []string       `json:"operation_ids"`
		ExpiresAt       time.Time      `json:"expires_at"`
		IntentRevision  uint64         `json:"expected_revision"`
	}{
		Operation:       "service_account.create",
		Scope:           scope,
		DisplayName:     &intentDisplayName,
		WorkspaceScopes: workspaceScopes,
		ConnectionIDs:   connectionIDs,
		OperationIDs:    operationIDs,
		ExpiresAt:       expiresAt,
		IntentRevision:  revision,
	}
	return integrationCanonicalDigest(t, intent)
}

func integrationServiceAccountRevokeDigest(
	t *testing.T,
	scope contract.Scope,
	serviceAccountID, grantRevision string,
	expiresAt time.Time,
	revision uint64,
) contract.SemanticDigest {
	t.Helper()
	intent := struct {
		Operation        string         `json:"operation"`
		Scope            contract.Scope `json:"scope"`
		ServiceAccountID string         `json:"service_account_id"`
		GrantRevision    string         `json:"grant_revision"`
		ExpiresAt        time.Time      `json:"expires_at"`
		IntentRevision   uint64         `json:"expected_revision"`
	}{
		Operation:        "service_account.revoke",
		Scope:            scope,
		ServiceAccountID: serviceAccountID,
		GrantRevision:    grantRevision,
		ExpiresAt:        expiresAt,
		IntentRevision:   revision,
	}
	return integrationCanonicalDigest(t, intent)
}

func integrationServiceAccountCreateRequest(
	requestKey string,
	digest contract.SemanticDigest,
	revision uint64,
	displayName string,
	workspaceScopes, connectionIDs, operationIDs []string,
	expiresAt time.Time,
) map[string]any {
	return map[string]any{
		"protocol": map[string]any{
			"wire_version":      "pm.api/v1",
			"required_features": []string{},
			"request_key":       requestKey,
			"intent_digest":     string(digest),
			"expected_revision": strconv.FormatUint(revision, 10),
		},
		"service_account": map[string]any{
			"display_name":      displayName,
			"workspace_scopes":  workspaceScopes,
			"connection_scopes": connectionIDs,
			"operation_scopes":  operationIDs,
			"expires_at":        expiresAt,
		},
	}
}

func integrationServiceAccountRevokeRequest(
	requestKey string,
	digest contract.SemanticDigest,
	revision uint64,
	serviceAccountID, grantRevision string,
	expiresAt time.Time,
) map[string]any {
	return map[string]any{
		"protocol": map[string]any{
			"wire_version":      "pm.api/v1",
			"required_features": []string{},
			"request_key":       requestKey,
			"intent_digest":     string(digest),
			"expected_revision": strconv.FormatUint(revision, 10),
		},
		"service_account": map[string]any{
			"service_account_ref": serviceAccountID,
			"grant_revision":      grantRevision,
			"expires_at":          expiresAt,
		},
	}
}

func readIntegrationServiceAccountPage(
	t *testing.T,
	fixture *serverIntegrationFixture,
	browser integrationBrowserSession,
) (integrationServiceAccountPage, integrationHTTPResponse) {
	t.Helper()
	response := fixture.request(t, browser.client, http.MethodGet,
		"/v1/workspaces/"+fixture.scope.WorkspaceID+"/service-accounts", nil, nil)
	requireIntegrationStatus(t, response, http.StatusOK, "service-account collection read")
	var page integrationServiceAccountPage
	decodeIntegrationJSON(t, response.body, &page, "service-account collection")
	parseIntegrationServiceAccountRevision(t, page.CollectionRevision)
	if response.header.Get("ETag") != quotedIntegrationRevision(page.CollectionRevision) {
		t.Fatal("service-account collection did not expose its authoritative revision")
	}
	return page, response
}

func findIntegrationServiceAccountMetadata(
	t *testing.T,
	page integrationServiceAccountPage,
	serviceAccountID string,
) integrationServiceAccountMetadata {
	t.Helper()
	for _, item := range page.Items {
		if item.ServiceAccountRef == serviceAccountID {
			return item
		}
	}
	t.Fatal("returned service-account identity was not durably listed")
	return integrationServiceAccountMetadata{}
}

func parseIntegrationServiceAccountRevision(t *testing.T, value string) uint64 {
	t.Helper()
	revision, err := strconv.ParseUint(value, 10, 64)
	if err != nil || revision == 0 {
		t.Fatal("service-account integration response contained an invalid revision")
	}
	return revision
}

func quotedIntegrationRevision(value string) string {
	return `"` + value + `"`
}

func requireIntegrationServiceAccountCredentialAbsent(
	t *testing.T,
	response integrationHTTPResponse,
	credentials ...string,
) {
	t.Helper()
	if bytes.Contains(response.body, []byte(`"pm_automation_credential"`)) {
		t.Fatal("safe service-account response reissued the creation-only credential field")
	}
	for _, credential := range credentials {
		if integrationProjectionContainsSecret(response.body, []byte(credential)) {
			t.Fatal("safe service-account response exposed one-time credential material")
		}
	}
}

func requireIntegrationAutomationCredentialStatus(
	t *testing.T,
	fixture *serverIntegrationFixture,
	credential, requestKey string,
	wantedStatus int,
) {
	t.Helper()
	headers := http.Header{
		"Authorization":   []string{"Bearer " + credential},
		"Idempotency-Key": []string{requestKey},
	}
	response := fixture.requestJSON(t, fixture.newHTTPClient(t, false), http.MethodPost,
		"/v1/workspaces/"+fixture.scope.WorkspaceID+"/invocations",
		map[string]any{
			"protocol": map[string]any{
				"wire_version":      "pm.api/v1",
				"required_features": []string{},
				"request_key":       requestKey,
				"intent_digest":     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"expected_revision": "revision-1",
			},
			"connection_ref": fixture.connectionID,
			"operation_ref":  "fixture.read@revision-1",
			"lane":           "direct_read",
			"input": map[string]any{
				"presence": "value",
				"type":     "object",
				"fields":   map[string]any{},
			},
			"result_controls": map[string]any{},
		},
		headers)
	requireIntegrationStatus(t, response, wantedStatus, "service-account automation credential")
	switch wantedStatus {
	case http.StatusServiceUnavailable:
		requireIntegrationProblemCode(t, response, "capability_unavailable")
	case http.StatusUnauthorized:
		requireIntegrationProblemCode(t, response, "authentication_required")
	default:
		t.Fatal("unsupported service-account credential proof status")
	}
}
