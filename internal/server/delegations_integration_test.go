package server

import (
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type integrationDelegationDocument struct {
	DelegationRef     string     `json:"delegation_ref"`
	RequestedActorRef string     `json:"requested_actor_ref"`
	EffectiveActorRef string     `json:"effective_actor_ref"`
	WorkspaceScopes   []string   `json:"workspace_scopes"`
	ConnectionScopes  []string   `json:"connection_scopes"`
	OperationScopes   []string   `json:"operation_scopes"`
	ScopeDigest       string     `json:"scope_digest"`
	ControlRevision   string     `json:"control_revision"`
	ExpiresAt         time.Time  `json:"expires_at"`
	RevokedAt         *time.Time `json:"revoked_at"`
}

type integrationDelegationPage struct {
	CollectionRevision string                          `json:"collection_revision"`
	Items              []integrationDelegationDocument `json:"items"`
}

func TestAuthenticatedApplicationDelegationMutations(t *testing.T) {
	fixture := newServerIntegrationFixture(t)
	const operationID = "fixture.read"
	if rows := fixture.execScoped(t, fixture.scope, `
		UPDATE pm.memberships
		SET operation_ids = $1
		WHERE org_id = $2 AND workspace_id = $3 AND identity_id = $4
	`, []string{operationID}, fixture.scope.OrgID, fixture.scope.WorkspaceID, fixture.operator.PrincipalID); rows != 1 {
		t.Fatalf("seed bounded operator operation grant rows = %d, want 1", rows)
	}
	browser := fixture.loginBrowser(t, integrationAuthTimeFresh, false)
	accountExpiresAt := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Microsecond)
	account := createIntegrationServiceAccount(
		t,
		fixture,
		browser,
		"delegation-parent-service",
		"Delegation parent",
		accountExpiresAt,
		[]string{fixture.connectionID},
		[]string{operationID},
	)
	requestedActor := contract.Principal{
		OrgID:       fixture.scope.OrgID,
		PrincipalID: account.ResourceID,
		Kind:        contract.PrincipalServiceAccount,
	}
	effectiveActor := fixture.operator
	delegationExpiresAt := account.ExpiresAt.Add(-time.Hour)
	delegationsPath := "/v1/workspaces/" + fixture.scope.WorkspaceID + "/delegations"

	initialList := fixture.request(t, browser.client, http.MethodGet, delegationsPath, nil, nil)
	requireIntegrationStatus(t, initialList, http.StatusOK, "initial scoped delegation list")
	var initialPage integrationDelegationPage
	decodeIntegrationJSON(t, initialList.body, &initialPage, "initial scoped delegation list")
	initialRevision := integrationDelegationRevision(t, initialPage.CollectionRevision)
	if len(initialPage.Items) != 0 || initialList.header.Get("ETag") != `"`+initialPage.CollectionRevision+`"` {
		t.Fatal("fresh delegation collection did not expose its empty current revision")
	}
	foreignList := fixture.request(t, browser.client, http.MethodGet,
		"/v1/workspaces/"+fixture.foreignScope.WorkspaceID+"/delegations", nil, nil)
	requireIntegrationStatus(t, foreignList, http.StatusNotFound, "foreign scoped delegation list")

	serviceClient := fixture.newHTTPClient(t, false)
	broaderOperations := []string{operationID, operationID + ".broader"}
	broaderKey := "delegation-create-broader"
	broaderDigest := integrationDelegationCreateDigest(
		t,
		fixture.scope,
		requestedActor,
		effectiveActor,
		[]string{fixture.scope.WorkspaceID},
		[]string{fixture.connectionID},
		broaderOperations,
		delegationExpiresAt,
		initialRevision,
	)
	broaderRequest := integrationDelegationCreateRequest(
		broaderKey,
		broaderDigest,
		initialPage.CollectionRevision,
		requestedActor.PrincipalID,
		effectiveActor.PrincipalID,
		[]string{fixture.scope.WorkspaceID},
		[]string{fixture.connectionID},
		broaderOperations,
		delegationExpiresAt,
	)
	broader := fixture.requestJSON(t, serviceClient, http.MethodPost, delegationsPath, broaderRequest,
		integrationDelegationServiceHeaders(account.Credential, broaderKey, initialPage.CollectionRevision))
	requireIntegrationStatus(t, broader, http.StatusForbidden, "broader delegation operation scope")
	requireIntegrationProblemCode(t, broader, "grant_unavailable")

	missingCreateKey := "delegation-create-needs-precondition"
	createDigest := integrationDelegationCreateDigest(
		t,
		fixture.scope,
		requestedActor,
		effectiveActor,
		[]string{fixture.scope.WorkspaceID},
		[]string{fixture.connectionID},
		[]string{operationID},
		delegationExpiresAt,
		initialRevision,
	)
	missingCreateRequest := integrationDelegationCreateRequest(
		missingCreateKey,
		createDigest,
		initialPage.CollectionRevision,
		requestedActor.PrincipalID,
		effectiveActor.PrincipalID,
		[]string{fixture.scope.WorkspaceID},
		[]string{fixture.connectionID},
		[]string{operationID},
		delegationExpiresAt,
	)
	missingCreate := fixture.requestJSON(t, serviceClient, http.MethodPost, delegationsPath, missingCreateRequest,
		integrationDelegationServiceHeaders(account.Credential, missingCreateKey, ""))
	requireIntegrationStatus(t, missingCreate, http.StatusPreconditionRequired, "delegation create precondition")
	requireIntegrationProblemCode(t, missingCreate, "control_revision_required")

	unchangedBeforeCreate := fixture.request(t, browser.client, http.MethodGet, delegationsPath, nil, nil)
	requireIntegrationStatus(t, unchangedBeforeCreate, http.StatusOK, "delegation list after create refusals")
	var unchangedBeforeCreatePage integrationDelegationPage
	decodeIntegrationJSON(t, unchangedBeforeCreate.body, &unchangedBeforeCreatePage, "delegation list after create refusals")
	if unchangedBeforeCreatePage.CollectionRevision != initialPage.CollectionRevision || len(unchangedBeforeCreatePage.Items) != 0 {
		t.Fatal("delegation create refusals changed the durable collection")
	}

	createKey := "delegation-create-current"
	createRequest := integrationDelegationCreateRequest(
		createKey,
		createDigest,
		initialPage.CollectionRevision,
		requestedActor.PrincipalID,
		effectiveActor.PrincipalID,
		[]string{fixture.scope.WorkspaceID},
		[]string{fixture.connectionID},
		[]string{operationID},
		delegationExpiresAt,
	)
	createdResponse := fixture.requestJSON(t, serviceClient, http.MethodPost, delegationsPath, createRequest,
		integrationDelegationServiceHeaders(account.Credential, createKey, initialPage.CollectionRevision))
	requireIntegrationStatus(t, createdResponse, http.StatusCreated, "current delegation create")
	var created integrationMutationDocument
	decodeIntegrationJSON(t, createdResponse.body, &created, "delegation create result")
	createdCollectionRevision := strconv.FormatUint(initialRevision+1, 10)
	if created.ResourceID == "" || created.Replay || created.ResultRevision != createdCollectionRevision ||
		created.ResultDisposition != "created" || created.OutcomeReference.Kind != "delegation" ||
		created.OutcomeReference.Ref != created.ResourceID || createdResponse.header.Get("ETag") != `"1"` {
		t.Fatal("fresh delegation create did not return its durable server-owned result")
	}

	createReplayResponse := fixture.requestJSON(t, serviceClient, http.MethodPost, delegationsPath, createRequest,
		integrationDelegationServiceHeaders(account.Credential, createKey, ""))
	requireIntegrationStatus(t, createReplayResponse, http.StatusOK, "exact delegation create replay")
	var createReplay integrationMutationDocument
	decodeIntegrationJSON(t, createReplayResponse.body, &createReplay, "delegation create replay result")
	if !createReplay.Replay || createReplay.ResourceID != created.ResourceID ||
		createReplay.ResultRevision != created.ResultRevision ||
		createReplay.ResultDisposition != "prior_mutation_result" ||
		createReplay.OutcomeReference.Kind != "delegation" ||
		createReplay.OutcomeReference.Ref != created.ResourceID || createReplayResponse.header.Get("ETag") != `"1"` {
		t.Fatal("exact delegation create replay did not return the original safe result")
	}

	changedCreateExpiresAt := delegationExpiresAt.Add(-time.Minute)
	changedCreateDigest := integrationDelegationCreateDigest(
		t,
		fixture.scope,
		requestedActor,
		effectiveActor,
		[]string{fixture.scope.WorkspaceID},
		[]string{fixture.connectionID},
		[]string{operationID},
		changedCreateExpiresAt,
		initialRevision,
	)
	changedCreateRequest := integrationDelegationCreateRequest(
		createKey,
		changedCreateDigest,
		initialPage.CollectionRevision,
		requestedActor.PrincipalID,
		effectiveActor.PrincipalID,
		[]string{fixture.scope.WorkspaceID},
		[]string{fixture.connectionID},
		[]string{operationID},
		changedCreateExpiresAt,
	)
	changedCreate := fixture.requestJSON(t, serviceClient, http.MethodPost, delegationsPath, changedCreateRequest,
		integrationDelegationServiceHeaders(account.Credential, createKey, ""))
	requireIntegrationStatus(t, changedCreate, http.StatusConflict, "changed delegation create intent")
	requireIntegrationProblemCode(t, changedCreate, "idempotency_conflict")

	staleCreateKey := "delegation-create-stale"
	staleCreateRequest := integrationDelegationCreateRequest(
		staleCreateKey,
		createDigest,
		initialPage.CollectionRevision,
		requestedActor.PrincipalID,
		effectiveActor.PrincipalID,
		[]string{fixture.scope.WorkspaceID},
		[]string{fixture.connectionID},
		[]string{operationID},
		delegationExpiresAt,
	)
	staleCreate := fixture.requestJSON(t, serviceClient, http.MethodPost, delegationsPath, staleCreateRequest,
		integrationDelegationServiceHeaders(account.Credential, staleCreateKey, initialPage.CollectionRevision))
	requireIntegrationStatus(t, staleCreate, http.StatusPreconditionFailed, "stale delegation collection revision")
	requireIntegrationProblemCode(t, staleCreate, "control_revision_mismatch")

	listedAfterCreate := fixture.request(t, browser.client, http.MethodGet, delegationsPath, nil, nil)
	requireIntegrationStatus(t, listedAfterCreate, http.StatusOK, "created delegation list")
	var createdPage integrationDelegationPage
	decodeIntegrationJSON(t, listedAfterCreate.body, &createdPage, "created delegation list")
	listedDelegation := findIntegrationDelegation(t, createdPage, created.ResourceID)
	if createdPage.CollectionRevision != createdCollectionRevision || len(createdPage.Items) != 1 ||
		listedAfterCreate.header.Get("ETag") != `"`+createdCollectionRevision+`"` ||
		listedDelegation.RequestedActorRef != account.ResourceID ||
		listedDelegation.EffectiveActorRef != fixture.operator.PrincipalID ||
		!slices.Equal(listedDelegation.WorkspaceScopes, []string{fixture.scope.WorkspaceID}) ||
		!slices.Equal(listedDelegation.ConnectionScopes, []string{fixture.connectionID}) ||
		!slices.Equal(listedDelegation.OperationScopes, []string{operationID}) ||
		listedDelegation.ControlRevision != "1" || listedDelegation.RevokedAt != nil ||
		!listedDelegation.ExpiresAt.Equal(delegationExpiresAt) ||
		!listedDelegation.ExpiresAt.Before(account.ExpiresAt) ||
		contract.SemanticDigest(listedDelegation.ScopeDigest).Validate() != nil {
		t.Fatal("scoped delegation list did not retain the bounded requested/effective actor grant")
	}

	delegationPath := delegationsPath + "/" + created.ResourceID + "/revoke"
	originalControlRevision := integrationDelegationRevision(t, listedDelegation.ControlRevision)
	revokeDigest := integrationDelegationRevokeDigest(
		t,
		fixture.scope,
		created.ResourceID,
		requestedActor,
		effectiveActor,
		contract.SemanticDigest(listedDelegation.ScopeDigest),
		listedDelegation.ExpiresAt,
		originalControlRevision,
	)
	missingRevokeKey := "delegation-revoke-needs-precondition"
	missingRevokeRequest := integrationDelegationRevokeRequest(
		missingRevokeKey,
		revokeDigest,
		listedDelegation.ControlRevision,
		created.ResourceID,
		listedDelegation.RequestedActorRef,
		listedDelegation.EffectiveActorRef,
		listedDelegation.ScopeDigest,
		listedDelegation.ExpiresAt,
	)
	missingRevoke := fixture.requestJSON(t, browser.client, http.MethodPost, delegationPath, missingRevokeRequest,
		integrationMutationHeaders(browser, fixture.origin, missingRevokeKey, ""))
	requireIntegrationStatus(t, missingRevoke, http.StatusPreconditionRequired, "delegation revoke precondition")
	requireIntegrationProblemCode(t, missingRevoke, "control_revision_required")

	listedAfterMissingRevoke := fixture.request(t, browser.client, http.MethodGet, delegationsPath, nil, nil)
	requireIntegrationStatus(t, listedAfterMissingRevoke, http.StatusOK, "delegation list after revoke refusal")
	var missingRevokePage integrationDelegationPage
	decodeIntegrationJSON(t, listedAfterMissingRevoke.body, &missingRevokePage, "delegation list after revoke refusal")
	stillActive := findIntegrationDelegation(t, missingRevokePage, created.ResourceID)
	if missingRevokePage.CollectionRevision != createdCollectionRevision || stillActive.RevokedAt != nil ||
		stillActive.ControlRevision != listedDelegation.ControlRevision {
		t.Fatal("missing delegation revoke precondition changed the durable grant")
	}

	revokeKey := "delegation-revoke-current"
	revokeRequest := integrationDelegationRevokeRequest(
		revokeKey,
		revokeDigest,
		listedDelegation.ControlRevision,
		created.ResourceID,
		listedDelegation.RequestedActorRef,
		listedDelegation.EffectiveActorRef,
		listedDelegation.ScopeDigest,
		listedDelegation.ExpiresAt,
	)
	revokedResponse := fixture.requestJSON(t, browser.client, http.MethodPost, delegationPath, revokeRequest,
		integrationMutationHeaders(browser, fixture.origin, revokeKey, listedDelegation.ControlRevision))
	requireIntegrationStatus(t, revokedResponse, http.StatusOK, "current delegation revoke")
	var revoked integrationMutationDocument
	decodeIntegrationJSON(t, revokedResponse.body, &revoked, "delegation revoke result")
	revokedControlRevision := strconv.FormatUint(originalControlRevision+1, 10)
	if revoked.Replay || revoked.ResourceID != created.ResourceID ||
		revoked.ResultRevision != revokedControlRevision || revoked.ResultDisposition != "revoked" ||
		revoked.OutcomeReference.Kind != "delegation" || revoked.OutcomeReference.Ref != created.ResourceID ||
		revokedResponse.header.Get("ETag") != `"`+revokedControlRevision+`"` {
		t.Fatal("delegation revoke did not return the exact changed resource metadata")
	}

	listedAfterRevoke := fixture.request(t, browser.client, http.MethodGet, delegationsPath, nil, nil)
	requireIntegrationStatus(t, listedAfterRevoke, http.StatusOK, "revoked delegation list")
	var revokedPage integrationDelegationPage
	decodeIntegrationJSON(t, listedAfterRevoke.body, &revokedPage, "revoked delegation list")
	durableRevocation := findIntegrationDelegation(t, revokedPage, created.ResourceID)
	revokedCollectionRevision := strconv.FormatUint(integrationDelegationRevision(t, createdCollectionRevision)+1, 10)
	if revokedPage.CollectionRevision != revokedCollectionRevision || len(revokedPage.Items) != 1 ||
		durableRevocation.ControlRevision != revokedControlRevision || durableRevocation.RevokedAt == nil ||
		!durableRevocation.ExpiresAt.Equal(listedDelegation.ExpiresAt) ||
		durableRevocation.ScopeDigest != listedDelegation.ScopeDigest ||
		durableRevocation.RequestedActorRef != account.ResourceID ||
		durableRevocation.EffectiveActorRef != fixture.operator.PrincipalID {
		t.Fatal("delegation revoke was not durably observable with its immutable actor binding")
	}
	revokedAt := *durableRevocation.RevokedAt

	revokeReplayResponse := fixture.requestJSON(t, browser.client, http.MethodPost, delegationPath, revokeRequest,
		integrationMutationHeaders(browser, fixture.origin, revokeKey, ""))
	requireIntegrationStatus(t, revokeReplayResponse, http.StatusOK, "exact delegation revoke replay")
	var revokeReplay integrationMutationDocument
	decodeIntegrationJSON(t, revokeReplayResponse.body, &revokeReplay, "delegation revoke replay result")
	if !revokeReplay.Replay || revokeReplay.ResourceID != revoked.ResourceID ||
		revokeReplay.ResultRevision != revoked.ResultRevision ||
		revokeReplay.ResultDisposition != "prior_mutation_result" ||
		revokeReplay.OutcomeReference.Kind != "delegation" ||
		revokeReplay.OutcomeReference.Ref != revoked.ResourceID ||
		revokeReplayResponse.header.Get("ETag") != `"`+revokedControlRevision+`"` {
		t.Fatal("exact delegation revoke replay did not return the original safe result")
	}

	changedRevokeExpiresAt := listedDelegation.ExpiresAt.Add(-time.Minute)
	changedRevokeDigest := integrationDelegationRevokeDigest(
		t,
		fixture.scope,
		created.ResourceID,
		requestedActor,
		effectiveActor,
		contract.SemanticDigest(listedDelegation.ScopeDigest),
		changedRevokeExpiresAt,
		originalControlRevision,
	)
	changedRevokeRequest := integrationDelegationRevokeRequest(
		revokeKey,
		changedRevokeDigest,
		listedDelegation.ControlRevision,
		created.ResourceID,
		listedDelegation.RequestedActorRef,
		listedDelegation.EffectiveActorRef,
		listedDelegation.ScopeDigest,
		changedRevokeExpiresAt,
	)
	changedRevoke := fixture.requestJSON(t, browser.client, http.MethodPost, delegationPath, changedRevokeRequest,
		integrationMutationHeaders(browser, fixture.origin, revokeKey, ""))
	requireIntegrationStatus(t, changedRevoke, http.StatusConflict, "changed delegation revoke intent")
	requireIntegrationProblemCode(t, changedRevoke, "idempotency_conflict")

	staleRevokeKey := "delegation-revoke-stale"
	staleRevokeRequest := integrationDelegationRevokeRequest(
		staleRevokeKey,
		revokeDigest,
		listedDelegation.ControlRevision,
		created.ResourceID,
		listedDelegation.RequestedActorRef,
		listedDelegation.EffectiveActorRef,
		listedDelegation.ScopeDigest,
		listedDelegation.ExpiresAt,
	)
	staleRevoke := fixture.requestJSON(t, browser.client, http.MethodPost, delegationPath, staleRevokeRequest,
		integrationMutationHeaders(browser, fixture.origin, staleRevokeKey, listedDelegation.ControlRevision))
	requireIntegrationStatus(t, staleRevoke, http.StatusPreconditionFailed, "stale delegation control revision")
	requireIntegrationProblemCode(t, staleRevoke, "control_revision_mismatch")

	unchangedKey := "delegation-revoke-unchanged"
	unchangedRevision := integrationDelegationRevision(t, durableRevocation.ControlRevision)
	unchangedDigest := integrationDelegationRevokeDigest(
		t,
		fixture.scope,
		created.ResourceID,
		requestedActor,
		effectiveActor,
		contract.SemanticDigest(durableRevocation.ScopeDigest),
		durableRevocation.ExpiresAt,
		unchangedRevision,
	)
	unchangedRequest := integrationDelegationRevokeRequest(
		unchangedKey,
		unchangedDigest,
		durableRevocation.ControlRevision,
		created.ResourceID,
		durableRevocation.RequestedActorRef,
		durableRevocation.EffectiveActorRef,
		durableRevocation.ScopeDigest,
		durableRevocation.ExpiresAt,
	)
	unchangedResponse := fixture.requestJSON(t, browser.client, http.MethodPost, delegationPath, unchangedRequest,
		integrationMutationHeaders(browser, fixture.origin, unchangedKey, durableRevocation.ControlRevision))
	requireIntegrationStatus(t, unchangedResponse, http.StatusOK, "already revoked delegation")
	var unchanged integrationMutationDocument
	decodeIntegrationJSON(t, unchangedResponse.body, &unchanged, "unchanged delegation revoke result")
	if unchanged.Replay || unchanged.ResourceID != created.ResourceID ||
		unchanged.ResultRevision != durableRevocation.ControlRevision || unchanged.ResultDisposition != "unchanged" ||
		unchanged.OutcomeReference.Kind != "delegation" || unchanged.OutcomeReference.Ref != created.ResourceID ||
		unchangedResponse.header.Get("ETag") != `"`+durableRevocation.ControlRevision+`"` {
		t.Fatal("already revoked delegation did not return unchanged safe metadata")
	}

	finalList := fixture.request(t, browser.client, http.MethodGet, delegationsPath, nil, nil)
	requireIntegrationStatus(t, finalList, http.StatusOK, "delegation list after replay and unchanged revoke")
	var finalPage integrationDelegationPage
	decodeIntegrationJSON(t, finalList.body, &finalPage, "delegation list after replay and unchanged revoke")
	finalDelegation := findIntegrationDelegation(t, finalPage, created.ResourceID)
	if finalPage.CollectionRevision != revokedCollectionRevision || len(finalPage.Items) != 1 ||
		finalDelegation.ControlRevision != durableRevocation.ControlRevision || finalDelegation.RevokedAt == nil ||
		!finalDelegation.RevokedAt.Equal(revokedAt) {
		t.Fatal("delegation replay, conflict, stale, or unchanged paths produced another durable effect")
	}

	memberships := fixture.request(t, browser.client, http.MethodGet,
		"/v1/workspaces/"+fixture.scope.WorkspaceID+"/memberships", nil, nil)
	requireIntegrationStatus(t, memberships, http.StatusOK, "current authority membership list")
	var membershipPage integrationMembershipPage
	decodeIntegrationJSON(t, memberships.body, &membershipPage, "current authority membership list")
	membershipRef := ""
	boundScopeDigest := ""
	membershipRevision := ""
	for _, membership := range membershipPage.Items {
		if membership.PrincipalRef == fixture.operator.PrincipalID && membership.RevokedAt == nil {
			membershipRef = membership.MembershipRef
			boundScopeDigest = membership.BoundScopeDigest
			membershipRevision = membership.ControlRevision
			break
		}
	}
	if membershipRef == "" || boundScopeDigest == "" {
		t.Fatal("current effective actor membership was not safely observable")
	}
	authorityRevokeKey := "delegation-effective-authority-revoke"
	authorityRevision := integrationDelegationRevision(t, membershipRevision)
	authorityRevokeDigest := integrationMembershipRevokeDigest(
		t,
		fixture.scope,
		membershipRef,
		boundScopeDigest,
		authorityRevision,
	)
	authorityRevokeRequest := integrationMembershipRevokeRequest(
		authorityRevokeKey,
		authorityRevokeDigest,
		membershipRevision,
		membershipRef,
		boundScopeDigest,
	)
	authorityRevoke := fixture.requestJSON(t, browser.client, http.MethodPost,
		"/v1/workspaces/"+fixture.scope.WorkspaceID+"/memberships/"+membershipRef+"/revoke",
		authorityRevokeRequest,
		integrationMutationHeaders(browser, fixture.origin, authorityRevokeKey, membershipRevision))
	requireIntegrationStatus(t, authorityRevoke, http.StatusOK, "effective actor authority revoke")

	createReplayWithoutCurrentAuthority := fixture.requestJSON(t, serviceClient, http.MethodPost, delegationsPath, createRequest,
		integrationDelegationServiceHeaders(account.Credential, createKey, ""))
	requireIntegrationStatus(t, createReplayWithoutCurrentAuthority, http.StatusForbidden,
		"delegation create replay without current effective authority")
	requireIntegrationProblemCode(t, createReplayWithoutCurrentAuthority, "grant_unavailable")
	revokeReplayWithoutCurrentAuthority := fixture.requestJSON(t, browser.client, http.MethodPost, delegationPath, revokeRequest,
		integrationMutationHeaders(browser, fixture.origin, revokeKey, ""))
	requireIntegrationStatus(t, revokeReplayWithoutCurrentAuthority, http.StatusForbidden,
		"delegation revoke replay without current caller authority")
	requireIntegrationProblemCode(t, revokeReplayWithoutCurrentAuthority, "grant_unavailable")
}

func integrationDelegationServiceHeaders(credential, idempotencyKey, ifMatch string) http.Header {
	headers := http.Header{
		"Authorization":   []string{"Bearer " + credential},
		"Idempotency-Key": []string{idempotencyKey},
	}
	if ifMatch != "" {
		headers.Set("If-Match", ifMatch)
	}
	return headers
}

func integrationDelegationCreateRequest(
	requestKey string,
	digest contract.SemanticDigest,
	revision string,
	requestedActorRef string,
	effectiveActorRef string,
	workspaceScopes []string,
	connectionScopes []string,
	operationScopes []string,
	expiresAt time.Time,
) map[string]any {
	return map[string]any{
		"protocol": map[string]any{
			"wire_version":      "pm.api/v1",
			"required_features": []string{},
			"request_key":       requestKey,
			"intent_digest":     string(digest),
			"expected_revision": revision,
		},
		"delegation": map[string]any{
			"requested_actor_ref": requestedActorRef,
			"effective_actor_ref": effectiveActorRef,
			"workspace_scopes":    workspaceScopes,
			"connection_scopes":   connectionScopes,
			"operation_scopes":    operationScopes,
			"expires_at":          expiresAt,
		},
	}
}

func integrationDelegationCreateDigest(
	t *testing.T,
	scope contract.Scope,
	requestedActor contract.Principal,
	effectiveActor contract.Principal,
	workspaceScopes []string,
	connectionIDs []string,
	operationIDs []string,
	expiresAt time.Time,
	revision uint64,
) contract.SemanticDigest {
	t.Helper()
	intent := struct {
		Operation       string             `json:"operation"`
		Scope           contract.Scope     `json:"scope"`
		RequestedActor  contract.Principal `json:"requested_actor"`
		EffectiveActor  contract.Principal `json:"effective_actor"`
		WorkspaceScopes []string           `json:"workspace_scopes"`
		ConnectionIDs   []string           `json:"connection_ids"`
		OperationIDs    []string           `json:"operation_ids"`
		ExpiresAt       time.Time          `json:"expires_at"`
		IntentRevision  uint64             `json:"expected_revision"`
	}{
		Operation:       "delegation.create",
		Scope:           scope,
		RequestedActor:  requestedActor,
		EffectiveActor:  effectiveActor,
		WorkspaceScopes: workspaceScopes,
		ConnectionIDs:   connectionIDs,
		OperationIDs:    operationIDs,
		ExpiresAt:       expiresAt,
		IntentRevision:  revision,
	}
	return integrationCanonicalDigest(t, intent)
}

func integrationDelegationRevokeRequest(
	requestKey string,
	digest contract.SemanticDigest,
	revision string,
	delegationRef string,
	requestedActorRef string,
	effectiveActorRef string,
	scopeDigest string,
	expiresAt time.Time,
) map[string]any {
	return map[string]any{
		"protocol": map[string]any{
			"wire_version":      "pm.api/v1",
			"required_features": []string{},
			"request_key":       requestKey,
			"intent_digest":     string(digest),
			"expected_revision": revision,
		},
		"delegation": map[string]any{
			"delegation_ref":      delegationRef,
			"requested_actor_ref": requestedActorRef,
			"effective_actor_ref": effectiveActorRef,
			"scope_digest":        scopeDigest,
			"expires_at":          expiresAt,
		},
	}
}

func integrationDelegationRevokeDigest(
	t *testing.T,
	scope contract.Scope,
	delegationID string,
	requestedActor contract.Principal,
	effectiveActor contract.Principal,
	scopeDigest contract.SemanticDigest,
	expiresAt time.Time,
	revision uint64,
) contract.SemanticDigest {
	t.Helper()
	intent := struct {
		Operation      string                  `json:"operation"`
		Scope          contract.Scope          `json:"scope"`
		DelegationID   string                  `json:"delegation_id"`
		RequestedActor contract.Principal      `json:"requested_actor"`
		EffectiveActor contract.Principal      `json:"effective_actor"`
		ScopeDigest    contract.SemanticDigest `json:"scope_digest"`
		ExpiresAt      time.Time               `json:"expires_at"`
		IntentRevision uint64                  `json:"expected_revision"`
	}{
		Operation:      "delegation.revoke",
		Scope:          scope,
		DelegationID:   delegationID,
		RequestedActor: requestedActor,
		EffectiveActor: effectiveActor,
		ScopeDigest:    scopeDigest,
		ExpiresAt:      expiresAt,
		IntentRevision: revision,
	}
	return integrationCanonicalDigest(t, intent)
}

func integrationDelegationRevision(t *testing.T, revision string) uint64 {
	t.Helper()
	parsed, err := strconv.ParseUint(revision, 10, 64)
	if err != nil || parsed == 0 {
		t.Fatalf("delegation revision %q is not a positive integer", revision)
	}
	return parsed
}

func findIntegrationDelegation(
	t *testing.T,
	page integrationDelegationPage,
	delegationID string,
) integrationDelegationDocument {
	t.Helper()
	for _, delegation := range page.Items {
		if delegation.DelegationRef == delegationID {
			return delegation
		}
	}
	t.Fatal("returned delegation identity was not durably listed")
	return integrationDelegationDocument{}
}
