package store_test

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

// TestArtifactCrashWindows joins actual selected shared bytes with PostgreSQL
// publication and manifest transactions. The old metadata-only cases remain
// separately named in artifact_metadata_test.go and are not physical proof.
func TestArtifactCrashWindows(t *testing.T) {
	if os.Getenv("PM_STACK_TEST_CONFIG") == "" && os.Getenv("PM_STORE_TEST_SECRETS_DIR") == "" {
		t.Skip("requires the explicitly selected owned stack fixture")
	}
	producer := testfixture.OpenArtifacts(t, testfixture.PrimaryArtifactGateway)
	consumer := testfixture.OpenArtifacts(t, testfixture.SecondaryArtifactGateway)
	fixture := testfixture.New(t)
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

	prepare := func(t *testing.T, artifactID, reservationID, payload string) (objects.PreparedPublication, contract.ObjectRef, store.PreverifiedObjectRef) {
		t.Helper()
		request := store.ObjectReservationRequest{ReservationID: reservationID, ConnectionID: fixture.Connection.ConnectionID, ArtifactID: artifactID, MaximumBytes: 64 << 10, Purpose: store.ArtifactPurposePublic}
		if _, err := publisher.Reserve(ctx, fixture.Actor, fixture.Scope, request); err != nil {
			t.Fatal(err)
		}
		attempt, err := objects.NewAttempt(fixture.Scope, artifactID, "application/octet-stream", []byte(payload), int64(request.MaximumBytes))
		if err != nil {
			t.Fatal(err)
		}
		defer attempt.Close()
		publication, err := publisher.Upload(ctx, fixture.Actor, fixture.Scope, reservationID, attempt)
		if err != nil {
			t.Fatalf("prepare actual shared bytes: %v", err)
		}
		ref, err := publication.Reference()
		if err != nil {
			t.Fatal(err)
		}
		// These observations originate in the real adapter's completed readback,
		// unlike the synthetic references used by metadata-only store tests.
		verified, err := store.NewPreverifiedObjectRef(ref, store.ObjectVerification{
			ObservedSHA256: ref.SHA256, ObservedByteLength: ref.ByteLength, ObservedBackendGeneration: ref.BackendGeneration,
		})
		if err != nil {
			t.Fatal(err)
		}
		return publication, ref, verified
	}
	commit := func(pool *store.Pool, publication objects.PreparedPublication, verified store.PreverifiedObjectRef, resourceID string, prior store.Manifest, beforeCommit func() error) (store.Manifest, error) {
		var manifest store.Manifest
		err := store.WithinScope(ctx, pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
			if _, err := publication.Commit(ctx, tx); err != nil {
				return err
			}
			var err error
			manifest, err = store.CommitManifestCAS(ctx, tx, store.ManifestCAS{
				ConnectionID: fixture.Connection.ConnectionID, ResourceID: resourceID, ExpectedRevision: prior.Revision, Object: verified,
			})
			if err != nil {
				return err
			}
			if beforeCommit != nil {
				return beforeCommit()
			}
			return nil
		})
		return manifest, err
	}
	loadManifest := func(t *testing.T, resourceID string) store.Manifest {
		t.Helper()
		var manifest store.Manifest
		if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
			var err error
			manifest, err = store.LoadManifest(ctx, tx, resourceID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return manifest
	}
	assertBytes := func(t *testing.T, ref contract.ObjectRef, want string) {
		t.Helper()
		body, err := consumer.Backend.OpenVerified(ctx, ref, objects.ByteRange{Length: ref.ByteLength})
		if err != nil {
			t.Fatalf("read actual retained object: %v", err)
		}
		defer body.Close()
		payload, err := io.ReadAll(io.LimitReader(body, 64<<10+1))
		if err != nil || string(payload) != want {
			t.Fatalf("retained object bytes changed or became unavailable: %v", err)
		}
	}

	priorPublication, priorRef, priorVerified := prepare(t, "artifact-prior", "reservation-prior", "prior exact bytes")

	t.Run("rollback_after_registration_and_manifest_write_keeps_prior", func(t *testing.T) {
		const resourceID = "real-crash-rollback"
		prior, err := commit(fixture.Pool, priorPublication, priorVerified, resourceID, store.Manifest{}, nil)
		if err != nil {
			t.Fatalf("publish initial actual manifest: %v", err)
		}
		publication, ref, verified := prepare(t, "artifact-rollback", "reservation-rollback", "uploaded but not published")
		interrupted := errors.New("owned interruption before PostgreSQL commit")
		_, err = commit(fixture.Pool, publication, verified, resourceID, prior, func() error { return interrupted })
		if !errors.Is(err, interrupted) {
			t.Fatalf("publication interruption: %v", err)
		}
		retained := loadManifest(t, resourceID)
		if retained.Revision != prior.Revision || retained.Object != priorRef {
			t.Fatal("uncommitted candidate advanced the public manifest")
		}
		if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
			_, err := store.LoadArtifact(ctx, tx, ref.ArtifactID)
			return err
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("rolled-back registration became visible: %v", err)
		}
		if _, err := reader.Recover(ctx, fixture.Actor, fixture.Scope, "reservation-rollback"); err == nil {
			t.Fatal("orphan existence stood in for an immutable publication receipt")
		}
		assertBytes(t, priorRef, "prior exact bytes")
		assertBytes(t, ref, "uploaded but not published")
	})

	t.Run("lost_physical_commit_reply_recovers_exact_occurrence", func(t *testing.T) {
		const resourceID = "real-crash-lost-reply"
		prior, err := commit(fixture.Pool, priorPublication, priorVerified, resourceID, store.Manifest{}, nil)
		if err != nil {
			t.Fatalf("publish independent initial manifest: %v", err)
		}
		publication, ref, verified := prepare(t, "artifact-committed", "reservation-committed", "committed exact bytes")
		faultPool, fault, err := store.NewCommitReplyFaultPoolForTest(ctx, fixture.Pool)
		if err != nil {
			t.Fatal(err)
		}
		defer faultPool.Close()
		var observed store.ArtifactPublicationReceipt
		observeCommitted := func() error {
			observationContext, stop := context.WithTimeout(ctx, 3*time.Second)
			defer stop()
			return store.WithinScope(observationContext, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
				var err error
				observed, err = store.LoadArtifactPublication(ctx, tx, "reservation-committed")
				if err != nil {
					return err
				}
				manifest, err := store.LoadManifest(ctx, tx, resourceID)
				if err != nil {
					return err
				}
				if observed.Object != ref || manifest.Object != ref || manifest.Revision != prior.Revision+1 {
					return errors.New("independent PostgreSQL observer did not see atomic publication and manifest")
				}
				return nil
			})
		}
		_, err = commit(faultPool, publication, verified, resourceID, prior, func() error { return fault.Arm(observeCommitted) })
		if !fault.Completed() || !errors.Is(err, store.ErrCommitOutcomeUnknown) {
			t.Fatalf("actual committed reply was not withheld as an unknown outcome: completed=%v error=%v", fault.Completed(), err)
		}
		recovered, err := reader.Recover(ctx, fixture.Actor, fixture.Scope, "reservation-committed")
		if err != nil || recovered.Object != ref || !recovered.PublishedAt.Equal(observed.PublishedAt) {
			t.Fatalf("recovery did not retain the original independently observed occurrence: %v", err)
		}
		retained := loadManifest(t, resourceID)
		if retained.Revision != prior.Revision+1 || retained.Object != ref {
			t.Fatal("recovery advanced or replaced the committed manifest")
		}
		assertBytes(t, priorRef, "prior exact bytes")
		assertBytes(t, ref, "committed exact bytes")
	})
}
