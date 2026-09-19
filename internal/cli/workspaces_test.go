package cli

import (
	"bytes"
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

type countingDoer struct {
	next  pmapi.HttpRequestDoer
	calls atomic.Int32
}

func (doer *countingDoer) Do(request *http.Request) (*http.Response, error) {
	doer.calls.Add(1)
	return doer.next.Do(request)
}

func TestConcurrentExplicitWorkspaces(t *testing.T) {
	fixture := testfixture.New(t)
	peer := newCLIProcessPeer(t, fixture, 2)
	grantPrimaryActorForeignWorkspace(t, fixture)
	profilesPath := filepath.Join(t.TempDir(), "profiles.json")
	handle, err := pmapi.DefaultSessionHandle("shared", fixture.DeploymentID)
	if err != nil {
		t.Fatalf("derive shared session handle: %v", err)
	}
	output := pmapi.OutputJSON
	if err := pmapi.AddServerProfile(profilesPath, pmapi.ServerProfile{
		Name: "shared", ServerURL: peer.URL, InstanceID: fixture.DeploymentID,
		SessionHandle: handle, Output: &output,
	}); err != nil {
		t.Fatalf("write shared profile: %v", err)
	}
	store := pmapi.NewMemorySessionStore()
	key, err := pmapi.NewSessionKey(fixture.DeploymentID, handle)
	if err != nil {
		t.Fatalf("shared session key: %v", err)
	}
	if err := store.Store(t.Context(), key, pmapi.PMDeviceSession{
		ExpiresAt:           time.Now().Add(time.Hour),
		PmSessionCredential: "fixture-token-replaced-by-real-authenticated-transport",
		Principal:           pmapi.Principal{Kind: pmapi.PrincipalKindUser, PrincipalId: fixture.Actor.RequestedActor.PrincipalID},
		SessionId:           "shared-fixture-session",
	}); err != nil {
		t.Fatalf("store shared PM session: %v", err)
	}
	factory := func(mode pmapi.SessionStoreMode) (pmapi.SessionStore, error) {
		if mode != pmapi.SessionStoreMemory {
			t.Fatalf("session mode = %q, want explicit memory", mode)
		}
		return store, nil
	}

	workspaces := []string{fixture.Scope.WorkspaceID, fixture.ForeignScope.WorkspaceID}
	processes := make([]*cliProcess, len(workspaces))
	for index, workspaceID := range workspaces {
		processes[index] = startCLIProcess(t, cliProcessInput{
			Args:         []string{"workspaces", "inspect", "--server-profile", "shared", "--workspace", workspaceID, "--session-store", "memory"},
			ProfilesPath: profilesPath, InstanceID: fixture.DeploymentID, SessionHandle: handle,
			Certificate: peer.Certificate().Raw, PrincipalID: fixture.Actor.RequestedActor.PrincipalID,
		})
	}
	if processes[0].command.Process.Pid == processes[1].command.Process.Pid {
		t.Fatal("workspace invocations did not use distinct client processes")
	}
	for index, workspaceID := range workspaces {
		process := processes[index]
		if err := process.command.Wait(); err != nil {
			t.Fatalf("workspace %q process exit: %v stderr %q", workspaceID, err, process.stderr.String())
		}
		if !strings.Contains(process.stdout.String(), workspaceID) {
			t.Fatalf("workspace %q output omitted its selector: %q", workspaceID, process.stdout.String())
		}
		other := workspaces[1-index]
		if strings.Contains(process.stdout.String(), other) {
			t.Fatalf("workspace %q output crossed into %q: %q", workspaceID, other, process.stdout.String())
		}
	}

	counter := &countingDoer{next: peer.Client()}
	var missingOut, missingErr bytes.Buffer
	missingCode := Execute(t.Context(), Options{
		Args:   []string{"workspaces", "inspect", "--server-profile", "shared", "--session-store", "memory"},
		Stdout: &missingOut, Stderr: &missingErr, ProfilesPath: profilesPath,
		HTTPClient: counter, SessionStore: factory,
	})
	if missingCode != 2 || counter.calls.Load() != 0 {
		t.Fatalf("missing workspace = exit %d HTTP calls %d stderr %q", missingCode, counter.calls.Load(), missingErr.String())
	}

	counter.calls.Store(0)
	var deniedOut, deniedErr bytes.Buffer
	deniedCode := Execute(t.Context(), Options{
		Args:   []string{"workspaces", "inspect", "--server-profile", "shared", "--workspace", "workspace-not-authorized", "--session-store", "memory"},
		Stdout: &deniedOut, Stderr: &deniedErr, ProfilesPath: profilesPath,
		HTTPClient: counter, SessionStore: factory,
	})
	if deniedCode != 3 || counter.calls.Load() < 2 {
		t.Fatalf("server denial = exit %d HTTP calls %d stdout %q stderr %q", deniedCode, counter.calls.Load(), deniedOut.String(), deniedErr.String())
	}
}

func grantPrimaryActorForeignWorkspace(t *testing.T, fixture *testfixture.Fixture) {
	permissions := []string{
		"artifact.read", "connection.credentials.manage", "connection.manage", "connection.read", "event.read",
		"operation.invoke", "run.control", "run.read", "workspace.access.manage", "workspace.access.read", "workspace.read",
	}
	resourceGrants := []string{
		"connection:" + fixture.ForeignConnection.ConnectionID,
		"operation:" + fixture.OperationID,
		"permission:artifact.read", "permission:connection.credentials.manage", "permission:connection.manage",
		"permission:connection.read", "permission:event.read", "permission:operation.invoke", "permission:run.control",
		"permission:run.read", "permission:workspace.access.manage", "permission:workspace.access.read", "permission:workspace.read",
	}
	fixture.SetupSQL(t, fixture.ForeignScope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO pm.memberships (
				org_id, workspace_id, membership_id, identity_id, role,
				resource_grants, permissions, connection_ids, operation_ids,
				policy_epoch, control_revision
			) VALUES ($1, $2, $3, $4, 'operator', $5, $6, $7, $8, 1, 1)
		`, fixture.ForeignScope.OrgID, fixture.ForeignScope.WorkspaceID,
			"membership-cross-"+fixture.Actor.RequestedActor.PrincipalID,
			fixture.Actor.RequestedActor.PrincipalID, resourceGrants, permissions,
			[]string{fixture.ForeignConnection.ConnectionID}, []string{fixture.OperationID})
		return err
	})
}
