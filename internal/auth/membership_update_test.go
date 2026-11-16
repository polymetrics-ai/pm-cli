package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// workspaceAuditRecord is one row of pm.workspace_audit_entries, as the stack
// administrator reads it.
type workspaceAuditRecord struct {
	WorkspaceID        string
	RequestedActorID   string
	RequestedActorKind string
	EffectiveActorID   string
	EffectiveActorKind string
	Action             string
	TargetRef          string
	Payload            contract.SafePayload
}

// workspaceAuditReader reads and removes the workspace audit records of the
// fixture organization as the stack administrator, which bypasses row-level
// security, as the acceptance observer does. The runtime role cannot read
// these records.
type workspaceAuditReader struct {
	adminDSN string
	orgID    string
}

func newWorkspaceAuditReader(t *testing.T, fixture *authFixture) workspaceAuditReader {
	t.Helper()
	directory := os.Getenv("PM_STORE_TEST_SECRETS_DIR")
	password, err := os.ReadFile(filepath.Join(directory, "superuser-password"))
	if err != nil || strings.TrimSpace(string(password)) == "" {
		t.Fatal("read the stack administrator credential")
	}
	defer clear(password)
	endpoint, err := os.ReadFile(filepath.Join(directory, "pm-app-endpoint"))
	if err != nil {
		t.Fatal("read pm-app-endpoint")
	}
	parsed, err := url.Parse(strings.TrimSpace(string(endpoint)))
	if err != nil {
		t.Fatal("parse pm-app-endpoint")
	}
	parsed.User = url.UserPassword("pm_stack_admin", strings.TrimSpace(string(password)))
	reader := workspaceAuditReader{adminDSN: parsed.String(), orgID: fixture.alpha.OrgID}
	// This cleanup runs before the fixture cleanup, which removes the
	// workspaces that these records name.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		connection, err := pgx.Connect(ctx, reader.adminDSN)
		if err != nil {
			t.Errorf("connect the stack administrator for cleanup: %T", err)
			return
		}
		defer func() { _ = connection.Close(context.Background()) }()
		if _, err := connection.Exec(ctx, `DELETE FROM pm.workspace_audit_entries WHERE org_id = $1`, reader.orgID); err != nil {
			t.Errorf("remove the workspace audit records: %v", err)
		}
	})
	return reader
}

func (reader workspaceAuditReader) records(t *testing.T) []workspaceAuditRecord {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, reader.adminDSN)
	if err != nil {
		t.Fatalf("connect the stack administrator: %T", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	rows, err := connection.Query(ctx, `
		SELECT workspace_id, requested_actor_id, requested_actor_kind, effective_actor_id,
		       effective_actor_kind, action, target_ref, payload
		FROM pm.workspace_audit_entries
		WHERE org_id = $1
		ORDER BY recorded_at, audit_id
	`, reader.orgID)
	if err != nil {
		t.Fatalf("read the workspace audit records: %v", err)
	}
	defer rows.Close()
	var result []workspaceAuditRecord
	for rows.Next() {
		var record workspaceAuditRecord
		var payload []byte
		if err := rows.Scan(&record.WorkspaceID, &record.RequestedActorID, &record.RequestedActorKind,
			&record.EffectiveActorID, &record.EffectiveActorKind, &record.Action, &record.TargetRef, &payload); err != nil {
			t.Fatalf("scan a workspace audit record: %v", err)
		}
		if err := json.Unmarshal(payload, &record.Payload); err != nil {
			t.Fatalf("decode a workspace audit payload: %v", err)
		}
		result = append(result, record)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the workspace audit records: %v", err)
	}
	return result
}

// Check: an admin adds a new connection to its own membership with the
// revision that it read. The membership grants list the connection, the other
// grants stay, and the control revision, the policy epoch and the collection
// revision are each one higher. One audit record names the actor, both
// revisions and the added connection. The admin can then use the connection.
// A stale revision, a missing If-Match, a changed intent, an unchanged add,
// an actor without authority, a connection that is unknown, revoked or in
// another workspace, a revoked target and a service account each change
// nothing and write no audit record. Defect it catches: a membership update
// that skips the revision, the authority rule or the audit record, or that
// removes or widens another grant.
func TestMembershipUpdateAddsConnectionsWithRevisionAndAudit(t *testing.T) {
	fixture := newAuthFixture(t)
	audit := newWorkspaceAuditReader(t, fixture)
	authService := fixture.newAuthService(t)
	identityService, err := app.NewIdentityService(app.IdentityServiceConfig{
		Pool: fixture.pool, DeploymentID: fixture.deployment,
		Authorize:        authService.AuthorizeResource,
		ReceiptRetention: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewIdentityService(): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ada := directTestActor(fixture.ada)
	adaMembership := "membership-" + fixture.ada.PrincipalID

	existing, err := identityService.InspectConnection(ctx, ada, fixture.alpha, fixture.connection)
	if err != nil {
		t.Fatalf("InspectConnection(): %v", err)
	}
	connectionRevision := uint64(1)
	createConnection := func(name string) string {
		t.Helper()
		created, err := identityService.CreateConnection(ctx, ada, fixture.alpha, app.CreateConnection{
			ConnectorID: existing.ConnectorID, ProviderAccountID: existing.ProviderAccountID, DisplayName: name,
			Control: mutationControl("create-"+name, connectionRevision, true),
		})
		if err != nil {
			t.Fatalf("CreateConnection(%s): %v", name, err)
		}
		connectionRevision = created.Mutation.Revision
		return created.Connection.ConnectionID
	}
	readConnection := func(actor contract.Actor, connectionID string) error {
		return store.WithinScope(ctx, fixture.pool, fixture.alpha, actor, func(ctx context.Context, tx *store.Transaction) error {
			_, err := authService.AuthorizeResource(ctx, tx, auth.AuthorizationRequest{
				Scope: fixture.alpha, Actor: actor, Permission: "connection.read",
				ResourceKind: auth.ResourceConnection, ResourceID: connectionID, ConnectionID: connectionID,
			})
			return err
		})
	}
	memberships := func() (map[string]app.Membership, uint64) {
		t.Helper()
		page, err := identityService.ListMemberships(ctx, ada, fixture.alpha, "", 100)
		if err != nil {
			t.Fatalf("ListMemberships(): %v", err)
		}
		result := make(map[string]app.Membership, len(page.Items))
		for _, item := range page.Items {
			result[item.MembershipID] = item
		}
		return result, page.CollectionRevision
	}
	update := func(actor contract.Actor, membershipID string, connections []string, control app.MutationControl) (app.MembershipMutationResult, error) {
		return identityService.UpdateMembership(ctx, actor, fixture.alpha, app.UpdateMembership{
			MembershipID: membershipID, ConnectionIDs: connections, Control: control,
		})
	}
	// requireNoChange checks that a refusal left the memberships, the
	// collection revision and the audit records as they were.
	requireNoChange := func(name string, before map[string]app.Membership, beforeCollection uint64, beforeAudit int) {
		t.Helper()
		after, afterCollection := memberships()
		if afterCollection != beforeCollection || len(after) != len(before) {
			t.Fatalf("%s changed the membership collection: %d/%d memberships, revision %d/%d",
				name, len(after), len(before), afterCollection, beforeCollection)
		}
		for id, membership := range before {
			changed := after[id]
			if changed.ControlRevision != membership.ControlRevision || changed.PolicyEpoch != membership.PolicyEpoch ||
				!slices.Equal(changed.ResourceGrants, membership.ResourceGrants) ||
				!slices.Equal(changed.ConnectionIDs, membership.ConnectionIDs) {
				t.Fatalf("%s changed membership %s: %#v; want %#v", name, id, changed, membership)
			}
		}
		if got := len(audit.records(t)); got != beforeAudit {
			t.Fatalf("%s wrote audit records: %d; want %d", name, got, beforeAudit)
		}
	}

	added := createConnection("membership-update-added")
	requireErrorIs(t, readConnection(ada, added), auth.ErrResourceNotFound)
	initial, initialCollection := memberships()
	adaBefore := initial[adaMembership]
	if adaBefore.ControlRevision != 1 || slices.Contains(adaBefore.ConnectionIDs, added) {
		t.Fatalf("admin membership before the update = %#v", adaBefore)
	}

	// A fresh update with no If-Match is refused with 428.
	_, err = update(ada, adaMembership, []string{added}, mutationControl("update-add", 1, false))
	requireAppStatus(t, err, app.StatusPreconditionRequired)
	requireNoChange("update without If-Match", initial, initialCollection, 0)

	// The self-add.
	result, err := update(ada, adaMembership, []string{added}, mutationControl("update-add", 1, true))
	if err != nil {
		t.Fatalf("UpdateMembership(self-add): %v", err)
	}
	if result.Mutation.Unchanged || result.Mutation.Replayed || result.Mutation.Status != app.StatusOK ||
		result.Mutation.Revision != 2 || result.Membership.ControlRevision != 2 {
		t.Fatalf("self-add result = %#v; want a changed update at revision 2", result.Mutation)
	}
	updated, updatedCollection := memberships()
	adaAfter := updated[adaMembership]
	wantConnections := append([]string{added}, adaBefore.ConnectionIDs...)
	slices.Sort(wantConnections)
	wantGrants := append([]string{"connection:" + added}, adaBefore.ResourceGrants...)
	slices.Sort(wantGrants)
	if adaAfter.ControlRevision != 2 || adaAfter.PolicyEpoch != adaBefore.PolicyEpoch+1 ||
		!slices.Equal(adaAfter.ConnectionIDs, wantConnections) || !slices.Equal(adaAfter.ResourceGrants, wantGrants) ||
		!slices.Equal(adaAfter.Permissions, adaBefore.Permissions) || !slices.Equal(adaAfter.OperationIDs, adaBefore.OperationIDs) ||
		adaAfter.Role != adaBefore.Role || adaAfter.ExpiresAt != nil || adaAfter.RevokedAt != nil {
		t.Fatalf("admin membership after the update = %#v; before %#v", adaAfter, adaBefore)
	}
	if updatedCollection != initialCollection+1 {
		t.Fatalf("membership collection revision = %d; want %d", updatedCollection, initialCollection+1)
	}
	records := audit.records(t)
	if len(records) != 1 {
		t.Fatalf("audit records after the self-add = %d; want 1", len(records))
	}
	record := records[0]
	wantPayload := contract.SafePayload{
		References: []contract.SafeReference{
			{Name: "membership", Ref: adaMembership},
			{Name: "principal", Ref: fixture.ada.PrincipalID},
			{Name: "added_connection_1", Ref: added},
		},
		Counts: []contract.SafeCount{
			{Name: "previous_control_revision", Value: 1},
			{Name: "control_revision", Value: 2},
			{Name: "added_connections", Value: 1},
		},
		ReasonCodes: []string{},
	}
	if record.WorkspaceID != fixture.alpha.WorkspaceID || record.Action != "membership.update" ||
		record.TargetRef != adaMembership || record.RequestedActorID != fixture.ada.PrincipalID ||
		record.RequestedActorKind != "user" || record.EffectiveActorID != fixture.ada.PrincipalID ||
		record.EffectiveActorKind != "user" || !equalSafePayload(record.Payload, wantPayload) {
		t.Fatalf("audit record = %#v; want the actor, the membership, both revisions and the added connection", record)
	}
	if err := readConnection(ada, added); err != nil {
		t.Fatalf("admin read of the added connection after the update: %v", err)
	}

	// A replay of the same key and intent returns the stored result, also
	// with no If-Match. A changed intent with the same key is refused.
	replayed, err := update(ada, adaMembership, []string{added}, mutationControl("update-add", 1, false))
	if err != nil || !replayed.Mutation.Replayed || replayed.Mutation.Revision != 2 {
		t.Fatalf("replay = %#v, %v; want the stored result at revision 2", replayed.Mutation, err)
	}
	changedIntent := []string{added, fixture.connection}
	slices.Sort(changedIntent)
	_, err = update(ada, adaMembership, changedIntent, mutationControl("update-add", 1, true))
	requireAppStatus(t, err, app.StatusConflict)
	requireNoChange("replay and changed intent", updated, updatedCollection, 1)

	// A stale revision is refused with 412.
	_, err = update(ada, adaMembership, []string{added}, mutationControl("update-stale", 1, true))
	requireAppStatus(t, err, app.StatusPreconditionFailed)
	requireNoChange("stale revision", updated, updatedCollection, 1)

	// An add of a connection that the membership holds changes nothing.
	unchanged, err := update(ada, adaMembership, []string{added}, mutationControl("update-unchanged", 2, true))
	if err != nil || !unchanged.Mutation.Unchanged || unchanged.Mutation.Revision != 2 {
		t.Fatalf("unchanged add = %#v, %v; want unchanged at revision 2", unchanged.Mutation, err)
	}
	requireNoChange("unchanged add", updated, updatedCollection, 1)

	// Two more members: charlie without access authority, and delta with
	// access authority but no connection.manage.
	grant := func(principal contract.Principal, grants []string) string {
		t.Helper()
		_, collection := memberships()
		granted, err := identityService.GrantMembership(ctx, ada, fixture.alpha, app.GrantMembership{
			Principal: principal, Role: "member", ResourceGrants: grants,
			Control: mutationControl("grant-"+principal.PrincipalID, collection, true),
		})
		if err != nil {
			t.Fatalf("GrantMembership(%s): %v", principal.PrincipalID, err)
		}
		return granted.Membership.MembershipID
	}
	charlieMembership := grant(fixture.charlie, []string{"connection:" + fixture.connection, "permission:workspace.read"})
	deltaMembership := grant(fixture.delta, []string{"permission:workspace.access.manage", "permission:workspace.read"})
	notHeld := createConnection("membership-update-not-held")
	revoked := createConnection("membership-update-revoked")
	// A revoke needs connection.manage on the connection, so the admin adds it
	// first.
	if _, err := update(ada, adaMembership, []string{revoked}, mutationControl("update-before-revoke", 2, true)); err != nil {
		t.Fatalf("self-add of the connection to revoke: %v", err)
	}
	connectionState, err := identityService.InspectConnection(ctx, ada, fixture.alpha, revoked)
	if err != nil {
		t.Fatalf("InspectConnection(revoked): %v", err)
	}
	if _, err := identityService.DeleteConnection(ctx, ada, fixture.alpha, app.DeleteConnection{
		ConnectionID: revoked, Control: mutationControl("delete-revoked", connectionState.Revision, true),
	}); err != nil {
		t.Fatalf("DeleteConnection(): %v", err)
	}
	otherWorkspace := "connection-beta-only-" + strings.TrimPrefix(fixture.deployment, "deployment-")
	fixture.execMigrator(t, fixture.beta, `
		INSERT INTO pm.connections (
			org_id, workspace_id, connection_id, connector_id, provider_account_id,
			revision, credential_ref, display_name, state
		) VALUES ($1, $2, $3, 'fixture-connector', $4, 1, '', 'Beta connection', 'active')
	`, fixture.beta.OrgID, fixture.beta.WorkspaceID, otherWorkspace, existing.ProviderAccountID)

	current, currentCollection := memberships()
	auditCount := len(audit.records(t))
	adaRevision := current[adaMembership].ControlRevision
	charlie := directTestActor(fixture.charlie)
	delta := directTestActor(fixture.delta)
	for _, refusal := range []struct {
		name       string
		actor      contract.Actor
		membership string
		connection string
		revision   uint64
	}{
		{"actor without workspace.access.manage", charlie, charlieMembership, added, current[charlieMembership].ControlRevision},
		{"self-add without connection.manage", delta, deltaMembership, added, current[deltaMembership].ControlRevision},
		{"another membership with a connection that the actor does not hold", ada, charlieMembership, notHeld, current[charlieMembership].ControlRevision},
		{"unknown connection", ada, adaMembership, "connection-unknown", adaRevision},
		{"revoked connection", ada, adaMembership, revoked, adaRevision},
		{"connection of another workspace", ada, adaMembership, otherWorkspace, adaRevision},
		{"unknown membership", ada, "membership-unknown", added, adaRevision},
	} {
		_, err := update(refusal.actor, refusal.membership, []string{refusal.connection},
			mutationControl("refusal-"+strings.ReplaceAll(refusal.name, " ", "-"), refusal.revision, true))
		requireAppStatus(t, err, http.StatusNotFound)
		requireNoChange(refusal.name, current, currentCollection, auditCount)
	}

	// For another member's membership, the actor adds a connection that it
	// holds, as a grant does.
	toCharlie, err := update(ada, charlieMembership, []string{added},
		mutationControl("update-charlie", current[charlieMembership].ControlRevision, true))
	if err != nil || toCharlie.Mutation.Unchanged || !slices.Contains(toCharlie.Membership.ConnectionIDs, added) {
		t.Fatalf("add of a held connection to another membership = %#v, %v", toCharlie, err)
	}
	if got := len(audit.records(t)); got != auditCount+1 {
		t.Fatalf("audit records after the add to another membership = %d; want %d", got, auditCount+1)
	}

	// A revoked target membership is refused with 409.
	fixture.revokeMembership(t, fixture.alpha, fixture.charlie)
	current, currentCollection = memberships()
	auditCount = len(audit.records(t))
	_, err = update(ada, charlieMembership, []string{fixture.connection},
		mutationControl("update-revoked-target", current[charlieMembership].ControlRevision, true))
	requireAppStatus(t, err, app.StatusConflict)
	requireNoChange("revoked target membership", current, currentCollection, auditCount)

	// A service account cannot manage memberships.
	expires := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Microsecond)
	serviceAccount, err := identityService.CreateServiceAccount(ctx, ada, fixture.alpha, app.CreateServiceAccount{
		WorkspaceScopes: []string{fixture.alpha.WorkspaceID},
		ConnectionIDs:   []string{fixture.connection},
		OperationIDs:    []string{"operation-read"},
		ExpiresAt:       expires,
		Control:         mutationControl("service-membership-update", 1, true),
	})
	if err != nil {
		t.Fatalf("CreateServiceAccount(): %v", err)
	}
	robot := contract.Actor{RequestedActor: serviceAccount.ServiceAccount.Principal, EffectiveActor: serviceAccount.ServiceAccount.Principal}
	_, err = update(robot, adaMembership, []string{notHeld}, mutationControl("update-by-robot", adaRevision, true))
	requireAppStatus(t, err, http.StatusNotFound)
	requireNoChange("service account", current, currentCollection, auditCount)
}

func equalSafePayload(left, right contract.SafePayload) bool {
	return slices.Equal(left.References, right.References) &&
		slices.Equal(left.Counts, right.Counts) &&
		slices.Equal(left.ReasonCodes, right.ReasonCodes)
}

// Check: a request through a delegation is refused with 404 by the access
// check, before any connection rule, and writes nothing. The request is one
// that the connection rule for another membership would accept for the
// delegated authority: the delegation holds the connection and outlasts the
// target. The delegating user's own request succeeds. Defect it catches: a
// path that lets a delegated actor add connections.
func TestMembershipUpdateRefusesADelegatedActorBeforeTheConnectionRule(t *testing.T) {
	fixture := newAuthFixture(t)
	audit := newWorkspaceAuditReader(t, fixture)
	authService := fixture.newAuthService(t)
	identityService, err := app.NewIdentityService(app.IdentityServiceConfig{
		Pool: fixture.pool, DeploymentID: fixture.deployment,
		Authorize: authService.AuthorizeResource, ReceiptRetention: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewIdentityService(): %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ada := directTestActor(fixture.ada)
	expires := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Microsecond)
	scopes := app.CreateServiceAccount{
		WorkspaceScopes: []string{fixture.alpha.WorkspaceID}, ConnectionIDs: []string{fixture.connection},
		OperationIDs: []string{"operation-read"}, ExpiresAt: expires,
		Control: mutationControl("delegated-update-service", 1, true),
	}
	service, err := identityService.CreateServiceAccount(ctx, ada, fixture.alpha, scopes)
	if err != nil {
		t.Fatalf("CreateServiceAccount(): %v", err)
	}
	serviceIdentity, err := authService.AuthenticateServiceCredential(ctx, service.OneTimeCredential)
	if err != nil {
		t.Fatalf("AuthenticateServiceCredential(): %v", err)
	}
	delegationExpires := expires.Add(-time.Minute)
	delegation, err := identityService.CreateDelegation(ctx, serviceIdentity.Actor, fixture.alpha, app.CreateDelegation{
		RequestedActor: serviceIdentity.Principal, EffectiveActor: fixture.ada,
		WorkspaceScopes: scopes.WorkspaceScopes, ConnectionIDs: scopes.ConnectionIDs, OperationIDs: scopes.OperationIDs,
		ExpiresAt: delegationExpires, Control: mutationControl("delegated-update-delegation", 1, true),
	})
	if err != nil {
		t.Fatalf("CreateDelegation(): %v", err)
	}
	delegated, err := app.DelegatedActor(delegation.Delegation)
	if err != nil {
		t.Fatalf("DelegatedActor(): %v", err)
	}
	page, err := identityService.ListMemberships(ctx, ada, fixture.alpha, "", 100)
	if err != nil {
		t.Fatalf("ListMemberships(): %v", err)
	}
	targetExpires := delegationExpires.Add(-time.Minute)
	target, err := identityService.GrantMembership(ctx, ada, fixture.alpha, app.GrantMembership{
		Principal: fixture.charlie, Role: "member", ResourceGrants: []string{"permission:workspace.read"},
		ExpiresAt: &targetExpires, Control: mutationControl("delegated-update-target", page.CollectionRevision, true),
	})
	if err != nil {
		t.Fatalf("GrantMembership(): %v", err)
	}
	request := app.UpdateMembership{
		MembershipID: target.Membership.MembershipID, ConnectionIDs: []string{fixture.connection},
		Control: mutationControl("delegated-update", target.Membership.ControlRevision, true),
	}

	// The access check itself refuses the delegated actor.
	err = store.WithinScope(ctx, fixture.pool, fixture.alpha, delegated, func(ctx context.Context, tx *store.Transaction) error {
		_, err := authService.AuthorizeResource(ctx, tx, auth.AuthorizationRequest{
			Scope: fixture.alpha, Actor: delegated, Permission: "workspace.access.manage",
			ResourceKind: auth.ResourceWorkspace, ResourceID: fixture.alpha.WorkspaceID,
		})
		return err
	})
	requireErrorIs(t, err, auth.ErrResourceNotFound)
	_, err = identityService.UpdateMembership(ctx, delegated, fixture.alpha, request)
	requireAppStatus(t, err, http.StatusNotFound)
	after, err := identityService.ListMemberships(ctx, ada, fixture.alpha, "", 100)
	if err != nil {
		t.Fatalf("ListMemberships(): %v", err)
	}
	for _, item := range after.Items {
		if item.MembershipID == request.MembershipID &&
			(item.ControlRevision != target.Membership.ControlRevision || slices.Contains(item.ConnectionIDs, fixture.connection)) {
			t.Fatalf("delegated request changed the membership: %#v", item)
		}
	}
	if after.CollectionRevision != page.CollectionRevision+1 || len(audit.records(t)) != 0 {
		t.Fatalf("delegated request wrote: collection revision %d (want %d), %d audit records (want 0)",
			after.CollectionRevision, page.CollectionRevision+1, len(audit.records(t)))
	}

	// The same add by the delegating user passes the connection rule.
	request.Control = mutationControl("direct-update", target.Membership.ControlRevision, true)
	direct, err := identityService.UpdateMembership(ctx, ada, fixture.alpha, request)
	if err != nil || direct.Mutation.Unchanged || !slices.Contains(direct.Membership.ConnectionIDs, fixture.connection) {
		t.Fatalf("direct add of the same connection = %#v, %v; want an update", direct.Mutation, err)
	}
}
