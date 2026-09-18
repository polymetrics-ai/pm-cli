package objects_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

// This case uses selected real shared-object gateways and PostgreSQL. The
// independent readers have separate private scratch roots; it is not yet a
// separate-host or lost-PostgreSQL-COMMIT-response experiment.
func TestUniqueAttemptPublication(t *testing.T) {
	if os.Getenv("PM_STACK_TEST_CONFIG") == "" && os.Getenv("PM_STORE_TEST_SECRETS_DIR") == "" {
		t.Skip("requires the explicitly selected owned stack fixture")
	}
	producer := testfixture.OpenArtifacts(t, testfixture.PrimaryArtifactGateway)
	consumer := testfixture.OpenArtifacts(t, testfixture.SecondaryArtifactGateway)
	fixture := testfixture.New(t, func(_ testfixture.ServiceDependencies, configuration *httpapi.ServiceConfig) error {
		configuration.Artifacts = consumer.Backend
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	publisher, err := objects.NewPublisher(fixture.Pool, producer.Backend, producer.Limits)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := objects.NewPublisher(fixture.Pool, consumer.Backend, consumer.Limits)
	if err != nil {
		t.Fatal(err)
	}
	const artifactID = "real-publication"
	const payload = "first\x00second\xffthird\n"
	const maximumBytes = 64 << 10
	prepare := func(reservationID string) objects.PreparedPublication {
		t.Helper()
		request := store.ObjectReservationRequest{ReservationID: reservationID, ConnectionID: fixture.Connection.ConnectionID, ArtifactID: artifactID, MaximumBytes: maximumBytes}
		if _, err := publisher.Reserve(ctx, fixture.Actor, fixture.Scope, request); err != nil {
			t.Fatal(err)
		}
		attempt, err := objects.NewAttempt(fixture.Scope, artifactID, "application/octet-stream", []byte(payload), maximumBytes)
		if err != nil {
			t.Fatal(err)
		}
		defer attempt.Close()
		prepared, err := publisher.Upload(ctx, fixture.Actor, fixture.Scope, reservationID, attempt)
		if err != nil {
			t.Fatalf("upload and independently verify selected shared bytes: %v", err)
		}
		return prepared
	}
	endpoint := fixture.BaseURL + "/v1/workspaces/" + url.PathEscape(fixture.Scope.WorkspaceID) + "/artifacts/" + artifactID
	request := func(client *http.Client, suffix, selection string) (int, http.Header, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+suffix, nil)
		if err != nil {
			t.Fatal(err)
		}
		if selection != "" {
			req.Header.Set("Range", selection)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, maximumBytes+1))
		if err != nil || len(body) > maximumBytes {
			t.Fatal("artifact response violated the fixture byte bound")
		}
		return response.StatusCode, response.Header, body
	}

	orphan := prepare("uploaded-before-rollback")
	orphanRef, err := orphan.Reference()
	if err != nil {
		t.Fatal(err)
	}
	rollback := errors.New("owned publication transaction rollback")
	err = store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		if _, err := orphan.Commit(ctx, tx); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("publication rollback: %v", err)
	}
	if status, _, _ := request(fixture.Client, "", ""); status != http.StatusNotFound {
		t.Fatalf("uploaded but rolled-back object became public: %d", status)
	}
	if _, err := reader.Recover(ctx, fixture.Actor, fixture.Scope, "uploaded-before-rollback"); err == nil {
		t.Fatal("object existence was accepted as a PostgreSQL publication occurrence")
	}
	if err := consumer.Backend.Verify(ctx, orphanRef); err != nil {
		t.Fatalf("rollback unexpectedly removed the actual retained orphan bytes: %v", err)
	}
	if err := publisher.CancelUnused(ctx, fixture.Actor, fixture.Scope, "uploaded-before-rollback"); !errors.Is(err, store.ErrObjectReservationClosed) {
		t.Fatalf("bound orphan lost its retained capacity charge: %v", err)
	}

	published := prepare("fresh-attempt-after-rollback")
	ref, err := published.Reference()
	if err != nil {
		t.Fatal(err)
	}
	if ref.ObjectAttemptKey == orphanRef.ObjectAttemptKey {
		t.Fatal("retry overwrote the previous physical attempt")
	}
	digest := sha256.Sum256([]byte(payload))
	if ref.SHA256 != hex.EncodeToString(digest[:]) || ref.ByteLength != int64(len(payload)) || ref.BackendGeneration == "" {
		t.Fatal("publication lacks exact digest, length, or observed backend generation")
	}
	var receipt store.ArtifactPublicationReceipt
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		receipt, err = published.Commit(ctx, tx)
		return err
	}); err != nil {
		t.Fatalf("commit verified publication: %v", err)
	}
	if err := producer.Close(); err != nil {
		t.Fatalf("close producer and remove its private scratch root: %v", err)
	}
	fixture.Restart(t)
	reader, err = objects.NewPublisher(fixture.Pool, consumer.Backend, consumer.Limits)
	if err != nil {
		t.Fatal(err)
	}
	endpoint = fixture.BaseURL + "/v1/workspaces/" + url.PathEscape(fixture.Scope.WorkspaceID) + "/artifacts/" + artifactID
	recovered, err := reader.Recover(ctx, fixture.Actor, fixture.Scope, "fresh-attempt-after-rollback")
	if err != nil || recovered.Object != ref || !recovered.PublishedAt.Equal(receipt.PublishedAt) {
		t.Fatalf("independent gateway could not recover the original committed occurrence: %v", err)
	}
	for _, test := range []struct {
		selection, want string
		status          int
	}{
		{"", payload, http.StatusOK},
		{"bytes=5-12", payload[5:13], http.StatusPartialContent},
		{"bytes=-4", payload[len(payload)-4:], http.StatusPartialContent},
	} {
		status, header, body := request(fixture.Client, "/content", test.selection)
		if status != test.status || string(body) != test.want || header.Get("Content-Length") != strconv.Itoa(len(test.want)) || header.Get("Content-Disposition") != "attachment" {
			t.Fatalf("independent authorized byte read %q: status=%d length=%d headers=%v", test.selection, status, len(body), header)
		}
	}
	foreignClient := fixture.ClientFor(t, fixture.ForeignActor)
	for _, suffix := range []string{"", "/content"} {
		if status, _, _ := request(foreignClient, suffix, "bytes=0-1"); status != http.StatusNotFound {
			t.Fatalf("foreign workspace principal could observe a real published object: %d", status)
		}
	}
	if err := consumer.Backend.Verify(ctx, orphanRef); err != nil {
		t.Fatalf("fresh publication mutated the earlier immutable attempt: %v", err)
	}
}
