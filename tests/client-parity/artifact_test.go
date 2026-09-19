package clientparity

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

// The CLI runs in an actual isolated child with a real persisted PM session.
// Its API instance reads verified bytes from the selected secondary gateway
// after the producer and its private scratch root have closed.
func TestArtifactCLISharedBytes(t *testing.T) {
	if os.Getenv("PM_STACK_TEST_CONFIG") == "" && os.Getenv("PM_STORE_TEST_SECRETS_DIR") == "" {
		// docs/development/testing.md and setup.md require missing fixture
		// inputs to fail this case rather than skip it.
		t.Fatal("this physical case requires the explicitly selected owned stack fixture; missing inputs fail rather than skip")
	}
	producer := testfixture.OpenArtifacts(t, testfixture.PrimaryArtifactGateway)
	consumer := testfixture.OpenArtifacts(t, testfixture.SecondaryArtifactGateway)
	var authentication *auth.Service
	fixture := testfixture.New(t, func(_ testfixture.ServiceDependencies, service *httpapi.ServiceConfig) error {
		authentication = service.Auth
		service.Artifacts = consumer.Backend
		return nil
	})
	if authentication == nil {
		t.Fatal("fixture authentication is unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	publisher, err := objects.NewPublisher(fixture.Pool, producer.Backend, producer.Limits)
	if err != nil {
		t.Fatal(err)
	}
	const artifactID = "actual-cli-artifact"
	const payload = "exact\x00binary\xffbytes\n"
	request := store.ObjectReservationRequest{ReservationID: "cli-publication", ConnectionID: fixture.Connection.ConnectionID, ArtifactID: artifactID, MaximumBytes: 1024, Purpose: store.ArtifactPurposePublic}
	if _, err := publisher.Reserve(ctx, fixture.Actor, fixture.Scope, request); err != nil {
		t.Fatal(err)
	}
	attempt, err := objects.NewAttempt(fixture.Scope, artifactID, "application/octet-stream", []byte(payload), int64(request.MaximumBytes))
	if err != nil {
		t.Fatal(err)
	}
	defer attempt.Close()
	publication, err := publisher.Upload(ctx, fixture.Actor, fixture.Scope, request.ReservationID, attempt)
	if err != nil {
		t.Fatalf("prepare actual CLI artifact: %v", err)
	}
	ref, err := publication.Reference()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		_, err := publication.Commit(ctx, tx)
		return err
	}); err != nil {
		t.Fatalf("publish actual CLI artifact: %v", err)
	}
	if err := producer.Close(); err != nil {
		t.Fatalf("close artifact producer: %v", err)
	}

	certificate, _ := parityCertificate(t, fixture)
	session := issueParityDeviceSession(t, authentication, fixture.Actor.RequestedActor)
	const profileName = "artifact-read"
	handle, err := pmapi.DefaultSessionHandle(profileName, fixture.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	profilesPath := filepath.Join(t.TempDir(), "profiles.json")
	if err := pmapi.AddServerProfile(profilesPath, pmapi.ServerProfile{
		Name: profileName, ServerURL: fixture.BaseURL, InstanceID: fixture.DeploymentID, SessionHandle: handle,
	}); err != nil {
		t.Fatal(err)
	}
	base := parityCLIInput{ProfilesPath: profilesPath, InstanceID: fixture.DeploymentID, SessionHandle: handle, Certificate: certificate.Raw, Session: session}
	common := []string{"--server-profile", profileName, "--workspace", fixture.Scope.WorkspaceID, "--session-store", "memory", "--output", "json", "--request-timeout", "10s"}
	run := func(arguments ...string) []byte {
		t.Helper()
		input := base
		input.Args = append(slices.Clone(common), arguments...)
		return runParityCLI(t, input)
	}
	metadataBytes := run("artifacts", "inspect", artifactID)
	var metadata pmapi.ArtifactMetadata
	decoder := json.NewDecoder(bytes.NewReader(metadataBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		t.Fatalf("decode actual CLI metadata without private backend fields: %v", err)
	}
	if metadata.ArtifactId != artifactID || metadata.Sha256 != ref.SHA256 || metadata.ByteLength != ref.ByteLength || metadata.MediaType != ref.MediaType {
		t.Fatal("CLI metadata differs from the exact published object")
	}
	if got := run("artifacts", "download", artifactID); string(got) != payload {
		t.Fatal("CLI full download changed binary bytes or added presentation output")
	}
	if got := run("artifacts", "download", artifactID, "--range", "bytes=2-8"); string(got) != payload[2:9] {
		t.Fatal("CLI range download differs from the requested physical bytes")
	}
}
