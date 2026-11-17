package app

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/store"
)

const bootstrapTestInput = `{
  "org_id": "org-first",
  "workspace_id": "workspace-first",
  "workspace_name": "First workspace",
  "admin_issuer": "https://identity.test.invalid",
  "admin_subject": "admin-subject",
  "provider_accounts": [{"connector_id": "postgres", "display_name": "Customer database"}]
}`

func declaredPostgresBundle(t *testing.T) []string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "connectors", "data", "postgres.bundle.json"))
	if err != nil {
		t.Fatalf("resolve the postgres bundle: %v", err)
	}
	return []string{path}
}

// recordingWrite is the store step of a check. It records each request and
// returns the given error.
type recordingWrite struct {
	requests []store.BootstrapRequest
	err      error
}

func (w *recordingWrite) write(_ context.Context, request store.BootstrapRequest) error {
	w.requests = append(w.requests, request)
	return w.err
}

// Go developer check, not acceptance proof. Each refusal of the input comes
// before the store step, so nothing is written and no credential is read.
// Defect: a refusal that reaches the store, or that gives another line.
func TestBootstrapRefusesInvalidInputBeforeTheStore(t *testing.T) {
	for name, testCase := range map[string]struct {
		change string
		want   error
	}{
		"empty workspace name":  {`"workspace_name": "First workspace"|"workspace_name": ""`, ErrBootstrapWorkspaceName},
		"empty admin subject":   {`"admin_subject": "admin-subject"|"admin_subject": ""`, ErrBootstrapAdminSubject},
		"undeclared connector":  {`"connector_id": "postgres"|"connector_id": "github"`, ErrBootstrapUndeclaredConnector},
		"empty organization":    {`"org_id": "org-first"|"org_id": ""`, ErrBootstrapInput},
		"unknown field":         {`"org_id": "org-first"|"org_id": "org-first", "password": "x"`, ErrBootstrapInput},
		"missing field":         {`"workspace_name": "First workspace",|`, ErrBootstrapInput},
		"missing account field": {`, "display_name": "Customer database"|`, ErrBootstrapInput},
		"second document": {`"Customer database"}]
}|"Customer database"}]
} {}`, ErrBootstrapInput},
	} {
		t.Run(name, func(t *testing.T) {
			old, replacement, _ := strings.Cut(testCase.change, "|")
			if strings.Count(bootstrapTestInput, old) != 1 {
				t.Fatalf("test input has no single %q", old)
			}
			document := strings.Replace(bootstrapTestInput, old, replacement, 1)
			recorder := &recordingWrite{}
			input, err := DecodeBootstrapInput(strings.NewReader(document))
			if err == nil {
				_, err = RunBootstrap(context.Background(), BootstrapConfig{
					DeploymentID:     "deployment-first",
					ConnectorBundles: declaredPostgresBundle(t),
					Write:            recorder.write,
				}, input)
			}
			if !errors.Is(err, testCase.want) || err.Error() != testCase.want.Error() {
				t.Fatalf("bootstrap = %v; want exactly %v", err, testCase.want)
			}
			if len(recorder.requests) != 0 {
				t.Fatalf("refused bootstrap reached the store %d times", len(recorder.requests))
			}
		})
	}
}

// Go developer check, not acceptance proof. The store step gets the admin
// with every product permission and no connection or operation grant, and the
// provider account of a declared connector. Each store refusal keeps its
// meaning. Defect: an admin without the right to manage memberships and
// connections, or an uncertain commit reported as a refusal or a success.
func TestBootstrapWritesTheAdminAndMapsStoreRefusals(t *testing.T) {
	input, err := DecodeBootstrapInput(strings.NewReader(bootstrapTestInput))
	if err != nil {
		t.Fatalf("decode the input: %v", err)
	}
	recorder := &recordingWrite{}
	configuration := BootstrapConfig{
		DeploymentID:     "deployment-first",
		ConnectorBundles: declaredPostgresBundle(t),
		Write:            recorder.write,
	}
	result, err := RunBootstrap(context.Background(), configuration, input)
	if err != nil || len(recorder.requests) != 1 {
		t.Fatalf("bootstrap = %v with %d store requests; want one success", err, len(recorder.requests))
	}
	request := recorder.requests[0]
	if request.DeploymentID != "deployment-first" || request.Scope.OrgID != "org-first" || request.Scope.WorkspaceID != "workspace-first" ||
		request.Role != "admin" || !reflect.DeepEqual(request.Permissions, bootstrapAdminPermissions) ||
		len(request.ProviderAccounts) != 1 || request.ProviderAccounts[0].ConnectorID != "postgres" ||
		request.ProviderAccounts[0].Capacity != 1 || request.Action != "workspace.bootstrap" {
		t.Fatalf("store request = %+v", request)
	}
	for _, permission := range []string{"connection.manage", "workspace.access.manage"} {
		found := false
		for _, held := range request.Permissions {
			found = found || held == permission
		}
		if !found {
			t.Fatalf("admin permissions %v do not hold %s", request.Permissions, permission)
		}
	}
	if result.Admin.IdentityID != request.AdminIdentityID || result.Membership.MembershipID != request.MembershipID ||
		result.ProviderAccounts[0].ProviderAccountID != request.ProviderAccounts[0].AccountID ||
		result.Workspace.Name != "First workspace" || result.Admin.Subject != "admin-subject" {
		t.Fatalf("result %+v does not name the rows of the request %+v", result, request)
	}

	for name, testCase := range map[string]struct {
		storeErr error
		want     error
	}{
		"scope bootstrapped": {store.ErrBootstrapScopeExists, ErrBootstrapScopeExists},
		"login bound":        {store.ErrBootstrapLoginBound, ErrBootstrapLoginBound},
		"credential refused": {store.ErrBootstrapAuthority, ErrBootstrapAuthority},
		"commit uncertain":   {errors.Join(store.ErrCommitOutcomeUnknown, errors.New("commit store transaction")), ErrBootstrapOutcomeUnknown},
		"store failure":      {errors.New("insert workspace: connection reset"), ErrBootstrapFailed},
	} {
		t.Run(name, func(t *testing.T) {
			configuration.Write = (&recordingWrite{err: testCase.storeErr}).write
			if _, err := RunBootstrap(context.Background(), configuration, input); err != testCase.want {
				t.Fatalf("bootstrap with store error %v = %v; want %v", testCase.storeErr, err, testCase.want)
			}
		})
	}
}

// Go developer check, not acceptance proof. Without the bootstrap credential
// in the secret directory, the store step refuses before any database
// connection. Defect: a bootstrap that falls back to another credential.
func TestBootstrapStoreWriterNeedsTheBootstrapCredential(t *testing.T) {
	directory := t.TempDir()
	for name, root := range map[string]string{
		"no bootstrap member":  directory,
		"no secret directory":  filepath.Join(directory, "absent"),
		"empty directory path": "",
	} {
		write := BootstrapStoreWriter(root, "secret://operator/postgres")
		if err := write(context.Background(), store.BootstrapRequest{}); !errors.Is(err, store.ErrBootstrapAuthority) {
			t.Errorf("%s: store step = %v; want %v", name, err, store.ErrBootstrapAuthority)
		}
	}
}
