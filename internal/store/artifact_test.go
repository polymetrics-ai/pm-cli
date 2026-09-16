package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestArtifactCrashWindows(t *testing.T) {
	// These are PostgreSQL metadata crash windows, not object-upload proof.
	credentials, pool := prepareTestStore(t, 3)
	scope := newTestScope(t)
	t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })
	priorRef := testObjectRef(scope, "artifact-prior", "attempt-prior", "a")
	priorVerified, err := NewPreverifiedObjectRef(priorRef, ObjectVerification{
		ObservedSHA256:            priorRef.SHA256,
		ObservedByteLength:        priorRef.ByteLength,
		ObservedBackendGeneration: priorRef.BackendGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := CommitManifestCAS(ctx, tx, ManifestCAS{ResourceID: "result", ExpectedRevision: 0, Object: priorVerified})
		return err
	}); err != nil {
		t.Fatalf("commit prior manifest: %v", err)
	}

	t.Run("registered_object_can_remain_unpublished_after_manifest_failure", func(t *testing.T) {
		orphanRef := testObjectRef(scope, "artifact-orphan", "attempt-orphan", "e")
		orphan, err := NewPreverifiedObjectRef(orphanRef, ObjectVerification{
			ObservedSHA256:            orphanRef.SHA256,
			ObservedByteLength:        orphanRef.ByteLength,
			ObservedBackendGeneration: orphanRef.BackendGeneration,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			return RegisterArtifact(ctx, tx, orphan)
		}); err != nil {
			t.Fatalf("register controlled orphan metadata: %v", err)
		}
		err = WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			_, err := CommitManifestCAS(ctx, tx, ManifestCAS{ResourceID: "result", ExpectedRevision: 0, Object: orphan})
			return err
		})
		if !errors.Is(err, ErrCASConflict) {
			t.Fatalf("registered orphan publication failure = %v; want ErrCASConflict", err)
		}
		if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			stored, err := LoadArtifact(ctx, tx, orphanRef.ArtifactID)
			if err != nil {
				return err
			}
			if stored != orphanRef {
				t.Fatalf("registered immutable reference = %#v; want %#v", stored, orphanRef)
			}
			manifest, err := LoadManifest(ctx, tx, "result")
			if err != nil {
				return err
			}
			if manifest.Object.ArtifactID != priorRef.ArtifactID {
				t.Fatalf("unpublished object replaced manifest: %#v", manifest)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestStoreManifestCommit(t *testing.T) {
	// This proves PostgreSQL metadata atomicity only. It performs no object upload,
	// readback, provider request, or shared-object durability claim.
	credentials, pool := prepareTestStore(t, 3)
	scope := newTestScope(t)
	foreignScope := newTestScope(t)
	t.Cleanup(func() {
		cleanupTestScope(t, credentials, scope)
		cleanupTestScope(t, credentials, foreignScope)
	})

	priorRef := testObjectRef(scope, "artifact-prior", "attempt-prior", "a")
	priorVerified, err := NewPreverifiedObjectRef(priorRef, ObjectVerification{
		ObservedSHA256:            priorRef.SHA256,
		ObservedByteLength:        priorRef.ByteLength,
		ObservedBackendGeneration: priorRef.BackendGeneration,
	})
	if err != nil {
		t.Fatalf("construct prior preverified reference: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var prior Manifest
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		prior, err = CommitManifestCAS(ctx, tx, ManifestCAS{ResourceID: "result", ExpectedRevision: 0, Object: priorVerified})
		return err
	}); err != nil {
		t.Fatalf("commit prior manifest: %v", err)
	}
	if prior.Revision != 1 || prior.Object.ArtifactID != priorRef.ArtifactID {
		t.Fatalf("prior manifest = %#v", prior)
	}

	t.Run("unverified_or_foreign_reference_is_refused", func(t *testing.T) {
		candidate := testObjectRef(scope, "artifact-unverified", "attempt-unverified", "b")
		_, err := NewPreverifiedObjectRef(candidate, ObjectVerification{
			ObservedSHA256:            testObjectRef(scope, "other", "other-attempt", "different").SHA256,
			ObservedByteLength:        candidate.ByteLength,
			ObservedBackendGeneration: candidate.BackendGeneration,
		})
		if !errors.Is(err, ErrObjectNotVerified) {
			t.Fatalf("mismatched verification error = %v; want ErrObjectNotVerified", err)
		}

		foreign := testObjectRef(foreignScope, "artifact-foreign", "attempt-foreign", "c")
		foreignVerified, err := NewPreverifiedObjectRef(foreign, ObjectVerification{
			ObservedSHA256:            foreign.SHA256,
			ObservedByteLength:        foreign.ByteLength,
			ObservedBackendGeneration: foreign.BackendGeneration,
		})
		if err != nil {
			t.Fatal(err)
		}
		err = WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			_, err := CommitManifestCAS(ctx, tx, ManifestCAS{ResourceID: "result", ExpectedRevision: prior.Revision, Object: foreignVerified})
			return err
		})
		if !errors.Is(err, ErrScopeMismatch) {
			t.Fatalf("foreign ObjectRef error = %v; want ErrScopeMismatch", err)
		}
	})

	candidateRef := testObjectRef(scope, "artifact-candidate", "attempt-candidate", "d")
	candidateVerified, err := NewPreverifiedObjectRef(candidateRef, ObjectVerification{
		ObservedSHA256:            candidateRef.SHA256,
		ObservedByteLength:        candidateRef.ByteLength,
		ObservedBackendGeneration: candidateRef.BackendGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("cas_loss_preserves_prior_manifest", func(t *testing.T) {
		err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			_, err := CommitManifestCAS(ctx, tx, ManifestCAS{ResourceID: "result", ExpectedRevision: 0, Object: candidateVerified})
			return err
		})
		if !errors.Is(err, ErrCASConflict) {
			t.Fatalf("lost manifest CAS error = %v; want ErrCASConflict", err)
		}
		assertManifestRef(t, pool, scope, "result", priorRef.ArtifactID, prior.Revision)
	})

	t.Run("rollback_after_candidate_write_preserves_prior_and_orphan_metadata", func(t *testing.T) {
		injected := errors.New("injected manifest commit loss")
		err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			if _, err := CommitManifestCAS(ctx, tx, ManifestCAS{ResourceID: "result", ExpectedRevision: prior.Revision, Object: candidateVerified}); err != nil {
				return err
			}
			return injected
		})
		if !errors.Is(err, injected) {
			t.Fatalf("manifest rollback error = %v; want injected failure", err)
		}
		assertManifestRef(t, pool, scope, "result", priorRef.ArtifactID, prior.Revision)

		if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			_, err := LoadArtifact(ctx, tx, candidateRef.ArtifactID)
			return err
		}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("rolled-back candidate registration error = %v; want ErrNotFound", err)
		}
	})
	t.Run("current_cas_publishes_only_the_preverified_reference", func(t *testing.T) {
		if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			_, err := CommitManifestCAS(ctx, tx, ManifestCAS{ResourceID: "result", ExpectedRevision: prior.Revision, Object: candidateVerified})
			return err
		}); err != nil {
			t.Fatalf("publish verified replacement: %v", err)
		}
		if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			published, err := LoadManifest(ctx, tx, "result")
			if err != nil {
				return err
			}
			if published.Revision != prior.Revision+1 || published.Object != candidateRef {
				t.Fatalf("manifest did not publish the exact verified object at the next revision: %#v", published)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func testObjectRef(scope contract.Scope, artifactID, attemptKey, seed string) contract.ObjectRef {
	digest := testDigest("object-" + seed)
	return contract.ObjectRef{
		OrgID:             scope.OrgID,
		WorkspaceID:       scope.WorkspaceID,
		ArtifactID:        artifactID,
		ObjectAttemptKey:  attemptKey,
		SHA256:            string(digest)[len("sha256:"):],
		ByteLength:        int64(len(seed)),
		MediaType:         "application/octet-stream",
		BackendGeneration: "generation-" + seed,
	}
}

func assertManifestRef(t *testing.T, pool *Pool, scope contract.Scope, resourceID, artifactID string, revision uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		manifest, err := LoadManifest(ctx, tx, resourceID)
		if err != nil {
			return err
		}
		if manifest.Object.ArtifactID != artifactID || manifest.Revision != revision {
			t.Fatalf("manifest = %#v; want artifact %q revision %d", manifest, artifactID, revision)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
