package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestArtifactMetadataCrashWindows(t *testing.T) {
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
	connectionID := registerArtifactTestConnections(t, ctx, pool, scope, "primary")[0]
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		_, err := CommitManifestCAS(ctx, tx, ManifestCAS{
			ConnectionID: connectionID, ResourceID: "result", ExpectedRevision: 0, Object: priorVerified,
		})
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
			return RegisterArtifact(ctx, tx, connectionID, orphan)
		}); err != nil {
			t.Fatalf("register controlled orphan metadata: %v", err)
		}
		err = WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			_, err := CommitManifestCAS(ctx, tx, ManifestCAS{
				ConnectionID: connectionID, ResourceID: "result", ExpectedRevision: 0, Object: orphan,
			})
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
	connectionID := registerArtifactTestConnections(t, ctx, pool, scope, "primary")[0]
	registerArtifactTestConnections(t, ctx, pool, foreignScope, "foreign")
	var prior Manifest
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		var err error
		prior, err = CommitManifestCAS(ctx, tx, ManifestCAS{
			ConnectionID: connectionID, ResourceID: "result", ExpectedRevision: 0, Object: priorVerified,
		})
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
			_, err := CommitManifestCAS(ctx, tx, ManifestCAS{
				ConnectionID: connectionID, ResourceID: "result",
				ExpectedRevision: prior.Revision, Object: foreignVerified,
			})
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
			_, err := CommitManifestCAS(ctx, tx, ManifestCAS{
				ConnectionID: connectionID, ResourceID: "result",
				ExpectedRevision: 0, Object: candidateVerified,
			})
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
			if _, err := CommitManifestCAS(ctx, tx, ManifestCAS{
				ConnectionID: connectionID, ResourceID: "result",
				ExpectedRevision: prior.Revision, Object: candidateVerified,
			}); err != nil {
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
			_, err := CommitManifestCAS(ctx, tx, ManifestCAS{
				ConnectionID: connectionID, ResourceID: "result",
				ExpectedRevision: prior.Revision, Object: candidateVerified,
			})
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

func TestArtifactAuthorityBindingIsImmutable(t *testing.T) {
	credentials, pool := prepareTestStore(t, 3)
	scope := newTestScope(t)
	t.Cleanup(func() { cleanupTestScope(t, credentials, scope) })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connectionIDs := registerArtifactTestConnections(t, ctx, pool, scope, "primary", "other")
	ref := testObjectRef(scope, "artifact-authority", "attempt-authority", "authority")
	verified, err := NewPreverifiedObjectRef(ref, ObjectVerification{
		ObservedSHA256:            ref.SHA256,
		ObservedByteLength:        ref.ByteLength,
		ObservedBackendGeneration: ref.BackendGeneration,
	})
	if err != nil {
		t.Fatalf("construct preverified authority fixture: %v", err)
	}

	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		if err := RegisterArtifact(ctx, tx, connectionIDs[0], verified); err != nil {
			return err
		}
		if err := RegisterArtifact(ctx, tx, connectionIDs[0], verified); err != nil {
			return err
		}
		connectionID, err := LoadArtifactConnectionID(ctx, tx, ref.ArtifactID)
		if err != nil {
			return err
		}
		if connectionID != connectionIDs[0] {
			t.Fatalf("artifact authority = %q; want %q", connectionID, connectionIDs[0])
		}
		return nil
	}); err != nil {
		t.Fatalf("register and replay exact artifact authority: %v", err)
	}

	err = WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return RegisterArtifact(ctx, tx, connectionIDs[1], verified)
	})
	if !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("conflicting artifact authority = %v; want ErrImmutableConflict", err)
	}
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		connectionID, err := LoadArtifactConnectionID(ctx, tx, ref.ArtifactID)
		if err != nil {
			return err
		}
		if connectionID != connectionIDs[0] {
			t.Fatalf("conflicting replay changed artifact authority to %q; want %q", connectionID, connectionIDs[0])
		}
		return nil
	}); err != nil {
		t.Fatalf("load retained artifact authority: %v", err)
	}
}

func TestArtifactPurposeIsolationAndImmutability(t *testing.T) {
	credentials, pool := prepareTestStore(t, 3)
	scope := newTestScope(t)
	foreignScope := newTestScope(t)
	t.Cleanup(func() {
		cleanupTestScope(t, credentials, scope)
		cleanupTestScope(t, credentials, foreignScope)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	connectionIDs := registerArtifactTestConnections(t, ctx, pool, scope, "purpose", "other")
	registerArtifactTestConnections(t, ctx, pool, foreignScope, "foreign")

	preverify := func(ref contract.ObjectRef) PreverifiedObjectRef {
		t.Helper()
		verified, err := NewPreverifiedObjectRef(ref, ObjectVerification{
			ObservedSHA256:            ref.SHA256,
			ObservedByteLength:        ref.ByteLength,
			ObservedBackendGeneration: ref.BackendGeneration,
		})
		if err != nil {
			t.Fatalf("construct preverified artifact %q: %v", ref.ArtifactID, err)
		}
		return verified
	}
	publicRef := testObjectRef(scope, "artifact-purpose-public", "attempt-purpose-public", "purpose-public")
	continuationRef := testObjectRef(scope, "artifact-purpose-continuation", "attempt-purpose-continuation", "purpose-continuation")
	publicObject := preverify(publicRef)
	continuationObject := preverify(continuationRef)

	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		if err := RegisterArtifact(ctx, tx, connectionIDs[0], publicObject); err != nil {
			return err
		}
		if err := RegisterArtifact(ctx, tx, connectionIDs[0], publicObject); err != nil {
			return err
		}
		if err := RegisterContinuationArtifact(ctx, tx, connectionIDs[0], continuationObject); err != nil {
			return err
		}
		return RegisterContinuationArtifact(ctx, tx, connectionIDs[0], continuationObject)
	}); err != nil {
		t.Fatalf("register and replay purpose-bound artifacts: %v", err)
	}

	assertPurposeAccess := func() {
		t.Helper()
		if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			public, err := LoadArtifact(ctx, tx, publicRef.ArtifactID)
			if err != nil {
				return err
			}
			if public != publicRef {
				t.Fatalf("public artifact = %#v; want %#v", public, publicRef)
			}
			connectionID, err := LoadArtifactConnectionID(ctx, tx, publicRef.ArtifactID)
			if err != nil {
				return err
			}
			if connectionID != connectionIDs[0] {
				t.Fatalf("public artifact connection = %q; want %q", connectionID, connectionIDs[0])
			}
			if _, err := LoadContinuationArtifact(ctx, tx, publicRef.ArtifactID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("continuation read of public artifact = %v; want ErrNotFound", err)
			}

			continuation, err := LoadContinuationArtifact(ctx, tx, continuationRef.ArtifactID)
			if err != nil {
				return err
			}
			if continuation != continuationRef {
				t.Fatalf("continuation artifact = %#v; want %#v", continuation, continuationRef)
			}
			if _, err := LoadArtifact(ctx, tx, continuationRef.ArtifactID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("public read of continuation artifact = %v; want ErrNotFound", err)
			}
			if _, err := LoadArtifactConnectionID(ctx, tx, continuationRef.ArtifactID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("public authority read of continuation artifact = %v; want ErrNotFound", err)
			}
			return nil
		}); err != nil {
			t.Fatalf("load purpose-bound artifacts: %v", err)
		}
	}
	assertPurposeAccess()

	for _, testCase := range []struct {
		name     string
		register func(context.Context, *Transaction) error
	}{
		{
			name: "public_to_continuation",
			register: func(ctx context.Context, tx *Transaction) error {
				return RegisterContinuationArtifact(ctx, tx, connectionIDs[0], publicObject)
			},
		},
		{
			name: "continuation_to_public",
			register: func(ctx context.Context, tx *Transaction) error {
				return RegisterArtifact(ctx, tx, connectionIDs[0], continuationObject)
			},
		},
		{
			name: "continuation_connection_change",
			register: func(ctx context.Context, tx *Transaction) error {
				return RegisterContinuationArtifact(ctx, tx, connectionIDs[1], continuationObject)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := WithinScope(ctx, pool, scope, testActor(scope), testCase.register)
			if !errors.Is(err, ErrImmutableConflict) {
				t.Fatalf("conflicting artifact registration = %v; want ErrImmutableConflict", err)
			}
		})
	}
	assertPurposeAccess()

	foreignRef := testObjectRef(foreignScope, "artifact-purpose-foreign", "attempt-purpose-foreign", "purpose-foreign")
	foreignObject := preverify(foreignRef)
	err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		return RegisterContinuationArtifact(ctx, tx, connectionIDs[0], foreignObject)
	})
	if !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("foreign continuation registration = %v; want ErrScopeMismatch", err)
	}
	err = WithinScope(ctx, pool, foreignScope, testActor(foreignScope), func(ctx context.Context, tx *Transaction) error {
		_, err := LoadContinuationArtifact(ctx, tx, continuationRef.ArtifactID)
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign continuation read = %v; want ErrNotFound", err)
	}
}

func registerArtifactTestConnections(
	t *testing.T,
	ctx context.Context,
	pool *Pool,
	scope contract.Scope,
	labels ...string,
) []string {
	t.Helper()
	connectionIDs := make([]string, len(labels))
	if err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
		if err := RegisterWorkspace(ctx, tx, WorkspaceIdentity{
			Scope: scope,
			Name:  "artifact-workspace",
		}); err != nil {
			return err
		}
		account := ProviderAccount{
			OrgID:          scope.OrgID,
			AccountID:      "artifact-account",
			Provider:       "artifact-provider",
			AccountWitness: "artifact-account-witness",
			Capacity:       8,
			Revision:       1,
		}
		if err := RegisterProviderAccount(ctx, tx, account); err != nil {
			return err
		}
		for index, label := range labels {
			connectionIDs[index] = "artifact-connection-" + label
			if err := RegisterConnection(ctx, tx, Connection{
				Scope:             scope,
				ConnectionID:      connectionIDs[index],
				ConnectorID:       "artifact-connector",
				ProviderAccountID: account.AccountID,
				Revision:          1,
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("register artifact test connections: %v", err)
	}
	return connectionIDs
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
