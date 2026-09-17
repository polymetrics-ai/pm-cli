package testfixture_test

import (
	"io"
	"net/http"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

func TestAuthenticatedFixtureRestart(t *testing.T) {
	fixture := testfixture.New(t)
	requireWorkspaceStatus(t, fixture.Client, fixture.BaseURL, fixture.Scope.WorkspaceID, http.StatusOK)

	foreignClient := fixture.ClientFor(t, fixture.ForeignActor)
	requireWorkspaceStatus(t, foreignClient, fixture.BaseURL, fixture.ForeignScope.WorkspaceID, http.StatusOK)
	requireWorkspaceStatus(t, foreignClient, fixture.BaseURL, fixture.Scope.WorkspaceID, http.StatusNotFound)

	wrongActorClient := fixture.ClientFor(t, fixture.WrongActor)
	requireWorkspaceStatus(t, wrongActorClient, fixture.BaseURL, fixture.Scope.WorkspaceID, http.StatusNotFound)

	fixture.Restart(t)
	requireWorkspaceStatus(t, fixture.Client, fixture.BaseURL, fixture.Scope.WorkspaceID, http.StatusOK)
}

func requireWorkspaceStatus(t *testing.T, client *http.Client, baseURL, workspaceID string, want int) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, baseURL+"/v1/workspaces/"+workspaceID, nil)
	if err != nil {
		t.Fatal("construct fixture workspace request")
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("execute fixture workspace request: %T", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != want {
		t.Fatalf("fixture workspace status = %d, want %d", response.StatusCode, want)
	}
}
