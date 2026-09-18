package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestScopedObjectCapacity(t *testing.T) {
	credentials, pool := prepareTestStore(t, 5)
	fixture := registerPermitTestFixture(t, credentials, pool, permitFixtureOptions{Capacity: 4})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	limits := ObjectCapacityLimits{OrganizationBytes: 32, WorkspaceBytes: 24, OrganizationAttempts: 8, WorkspaceAttempts: 4}
	alpha := ObjectReservationRequest{ReservationID: "object-alpha", ConnectionID: fixture.alphaConnections[0], ArtifactID: "artifact-alpha", MaximumBytes: 16}
	beta := ObjectReservationRequest{ReservationID: "object-beta", ConnectionID: fixture.betaConnection, ArtifactID: "artifact-beta", MaximumBytes: 16}
	reserve := func(scope contract.Scope, request ObjectReservationRequest) (ObjectReservation, error) {
		var reservation ObjectReservation
		err := WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
			var err error
			reservation, err = ReserveObjectCapacity(ctx, tx, request, limits)
			return err
		})
		return reservation, err
	}
	first, err := reserve(fixture.alpha, alpha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reserve(fixture.beta, beta); err != nil {
		t.Fatal(err)
	}
	replayed, err := reserve(fixture.alpha, alpha)
	if err != nil || !replayed.ReservedAt.Equal(first.ReservedAt) {
		t.Fatalf("exact reservation replay = %#v, %v; want original allocation", replayed, err)
	}
	more := ObjectReservationRequest{ReservationID: "object-more", ConnectionID: fixture.alphaConnections[0], ArtifactID: "artifact-more", MaximumBytes: 1}
	if _, err := reserve(fixture.alpha, more); !errors.Is(err, ErrObjectCapacityExceeded) {
		t.Fatalf("cross-workspace organization ceiling = %v", err)
	}
	changed := alpha
	changed.MaximumBytes--
	if _, err := reserve(fixture.alpha, changed); !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("changed reservation replay = %v", err)
	}
	for range 2 {
		if err := WithinScope(ctx, pool, fixture.alpha, testActor(fixture.alpha), func(ctx context.Context, tx *Transaction) error {
			return CancelObjectReservation(ctx, tx, alpha.ReservationID)
		}); err != nil {
			t.Fatal(err)
		}
	}
	more.ConnectionID, more.MaximumBytes = fixture.betaConnection, 9
	if _, err := reserve(fixture.beta, more); !errors.Is(err, ErrObjectCapacityExceeded) {
		t.Fatalf("workspace ceiling below available organization ceiling = %v", err)
	}
	more.ConnectionID, more.MaximumBytes = fixture.alphaConnections[0], 16
	if _, err := reserve(fixture.alpha, more); err != nil {
		t.Fatalf("released exact capacity was not reusable: %v", err)
	}
	if err := WithinScope(ctx, pool, fixture.beta, testActor(fixture.beta), func(ctx context.Context, tx *Transaction) error {
		_, err := LoadObjectReservation(ctx, tx, more.ReservationID)
		return err
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign workspace observed reservation: %v", err)
	}
}

// These are real PostgreSQL metadata/receipt tests. Descriptor fixtures do not
// assert S3 durability; selected-stack tests exercise actual bytes independently.
func TestObjectPublicationTransactionBoundaries(t *testing.T) {
	credentials, pool := prepareTestStore(t, 4)
	fixture := registerPermitTestFixture(t, credentials, pool, permitFixtureOptions{Capacity: 4})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	limits := ObjectCapacityLimits{OrganizationBytes: 64, WorkspaceBytes: 64, OrganizationAttempts: 2, WorkspaceAttempts: 2}
	request := ObjectReservationRequest{ReservationID: "object-publication", ConnectionID: fixture.alphaConnections[0], ArtifactID: "artifact-publication", MaximumBytes: 32}
	ref := testObjectRef(fixture.alpha, request.ArtifactID, "immutable-publication-attempt", "a")
	ref.ByteLength = 8
	binding := ObjectAttemptBinding{Scope: fixture.alpha, ArtifactID: ref.ArtifactID, ObjectAttemptKey: ref.ObjectAttemptKey, SHA256: ref.SHA256, ByteLength: ref.ByteLength, MediaType: ref.MediaType}
	verified, err := NewPreverifiedObjectRef(ref, ObjectVerification{ObservedSHA256: ref.SHA256, ObservedByteLength: ref.ByteLength, ObservedBackendGeneration: ref.BackendGeneration})
	if err != nil {
		t.Fatal(err)
	}
	within := func(callback ScopedTransactionFunc) error {
		return WithinScope(ctx, pool, fixture.alpha, testActor(fixture.alpha), callback)
	}
	if err := within(func(ctx context.Context, tx *Transaction) error {
		if _, err := ReserveObjectCapacity(ctx, tx, request, limits); err != nil {
			return err
		}
		_, err := BindObjectAttempt(ctx, tx, request.ReservationID, binding)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := within(func(ctx context.Context, tx *Transaction) error {
		return CancelObjectReservation(ctx, tx, request.ReservationID)
	}); !errors.Is(err, ErrObjectReservationClosed) {
		t.Fatalf("bound/possibly-uploaded capacity was released: %v", err)
	}
	wrongBinding := binding
	wrongBinding.ObjectAttemptKey = "replacement-attempt"
	if err := within(func(ctx context.Context, tx *Transaction) error {
		_, err := BindObjectAttempt(ctx, tx, request.ReservationID, wrongBinding)
		return err
	}); !errors.Is(err, ErrImmutableConflict) {
		t.Fatalf("existing physical attempt was rebound: %v", err)
	}
	rollback := errors.New("controlled publication rollback")
	if err := within(func(ctx context.Context, tx *Transaction) error {
		if _, err := CommitObjectPublication(ctx, tx, request.ReservationID, verified); err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatalf("publication rollback = %v", err)
	}
	for _, load := range []ScopedTransactionFunc{
		func(ctx context.Context, tx *Transaction) error {
			_, err := LoadArtifactPublication(ctx, tx, request.ReservationID)
			return err
		},
		func(ctx context.Context, tx *Transaction) error {
			_, err := LoadArtifact(ctx, tx, request.ArtifactID)
			return err
		},
	} {
		if err := within(load); !errors.Is(err, ErrNotFound) {
			t.Fatalf("rolled-back publication became visible: %v", err)
		}
	}
	var receipt ArtifactPublicationReceipt
	if err := within(func(ctx context.Context, tx *Transaction) error {
		var err error
		receipt, err = CommitObjectPublication(ctx, tx, request.ReservationID, verified)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := within(func(ctx context.Context, tx *Transaction) error {
		retained, err := LoadArtifactPublication(ctx, tx, request.ReservationID)
		if err != nil {
			return err
		}
		if retained.Object != ref || retained != receipt {
			t.Fatalf("publication receipt changed: %#v", retained)
		}
		replayed, err := CommitObjectPublication(ctx, tx, request.ReservationID, verified)
		if err != nil {
			return err
		}
		if replayed != retained {
			t.Fatalf("publication replay created a different receipt: %#v", replayed)
		}
		// The actual eight-byte binding returns unused ceiling slack, but its
		// physical-attempt slot remains charged even after publication.
		_, err = ReserveObjectCapacity(ctx, tx, ObjectReservationRequest{ReservationID: "remaining-byte-capacity", ConnectionID: request.ConnectionID, ArtifactID: "artifact-other", MaximumBytes: 55}, limits)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := within(func(ctx context.Context, tx *Transaction) error {
		_, err := ReserveObjectCapacity(ctx, tx, ObjectReservationRequest{ReservationID: "over-attempt-limit", ConnectionID: request.ConnectionID, ArtifactID: "artifact-third", MaximumBytes: 1}, limits)
		return err
	}); !errors.Is(err, ErrObjectCapacityExceeded) {
		t.Fatalf("retained attempt quota = %v", err)
	}
}

func TestObjectReservationCancellationCannotRacePastBinding(t *testing.T) {
	credentials, pool := prepareTestStore(t, 4)
	fixture := registerPermitTestFixture(t, credentials, pool, permitFixtureOptions{Capacity: 4})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	limits := ObjectCapacityLimits{OrganizationBytes: 16, WorkspaceBytes: 16, OrganizationAttempts: 1, WorkspaceAttempts: 1}
	request := ObjectReservationRequest{ReservationID: "object-race", ConnectionID: fixture.alphaConnections[0], ArtifactID: "artifact-race", MaximumBytes: 16}
	ref := testObjectRef(fixture.alpha, request.ArtifactID, "immutable-race-attempt", "b")
	ref.ByteLength = 0
	binding := ObjectAttemptBinding{Scope: fixture.alpha, ArtifactID: ref.ArtifactID, ObjectAttemptKey: ref.ObjectAttemptKey, SHA256: ref.SHA256, ByteLength: 0, MediaType: ref.MediaType}
	if err := WithinScope(ctx, pool, fixture.alpha, testActor(fixture.alpha), func(ctx context.Context, tx *Transaction) error {
		_, err := ReserveObjectCapacity(ctx, tx, request, limits)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var bindErr, cancelErr error
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		bindErr = WithinScope(ctx, pool, fixture.alpha, testActor(fixture.alpha), func(ctx context.Context, tx *Transaction) error {
			_, err := BindObjectAttempt(ctx, tx, request.ReservationID, binding)
			return err
		})
	}()
	go func() {
		defer workers.Done()
		<-start
		cancelErr = WithinScope(ctx, pool, fixture.alpha, testActor(fixture.alpha), func(ctx context.Context, tx *Transaction) error {
			return CancelObjectReservation(ctx, tx, request.ReservationID)
		})
	}()
	close(start)
	workers.Wait()
	if bindErr == nil && errors.Is(cancelErr, ErrObjectReservationClosed) {
		// A zero-byte object must still retain its physical-attempt slot.
		if err := WithinScope(ctx, pool, fixture.alpha, testActor(fixture.alpha), func(ctx context.Context, tx *Transaction) error {
			_, err := ReserveObjectCapacity(ctx, tx, ObjectReservationRequest{ReservationID: "no-released-slot", ConnectionID: request.ConnectionID, ArtifactID: "artifact-next", MaximumBytes: 1}, limits)
			return err
		}); !errors.Is(err, ErrObjectCapacityExceeded) {
			t.Fatalf("bound empty object lost its attempt charge: %v", err)
		}
	} else if cancelErr == nil && errors.Is(bindErr, ErrObjectReservationClosed) {
		if err := WithinScope(ctx, pool, fixture.alpha, testActor(fixture.alpha), func(ctx context.Context, tx *Transaction) error {
			_, err := ReserveObjectCapacity(ctx, tx, ObjectReservationRequest{ReservationID: "released-slot", ConnectionID: request.ConnectionID, ArtifactID: "artifact-next", MaximumBytes: 16}, limits)
			return err
		}); err != nil {
			t.Fatalf("cancelled unbound slot was not reusable: %v", err)
		}
	} else {
		t.Fatalf("bind/cancel race had no single winner: bind=%v cancel=%v", bindErr, cancelErr)
	}
}

func TestObjectCapacityConcurrentWorkspaces(t *testing.T) {
	credentials, pool := prepareTestStore(t, 4)
	fixture := registerPermitTestFixture(t, credentials, pool, permitFixtureOptions{Capacity: 2})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	limits := ObjectCapacityLimits{OrganizationBytes: 16, WorkspaceBytes: 16, OrganizationAttempts: 2, WorkspaceAttempts: 2}
	scopes := []contract.Scope{fixture.alpha, fixture.beta}
	connections := []string{fixture.alphaConnections[0], fixture.betaConnection}
	start := make(chan struct{})
	results := make(chan error, len(scopes))
	for index, scope := range scopes {
		go func() {
			<-start
			results <- WithinScope(ctx, pool, scope, testActor(scope), func(ctx context.Context, tx *Transaction) error {
				_, err := ReserveObjectCapacity(ctx, tx, ObjectReservationRequest{
					ReservationID: "competing-reservation", ConnectionID: connections[index],
					ArtifactID: "competing-artifact", MaximumBytes: 16,
				}, limits)
				return err
			})
		}()
	}
	close(start)
	admitted, refused := 0, 0
	for range scopes {
		err := <-results
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, ErrObjectCapacityExceeded):
			refused++
		default:
			t.Fatalf("concurrent capacity admission: %v", err)
		}
	}
	if admitted != 1 || refused != 1 {
		t.Fatalf("organization storage ceiling admitted %d and refused %d; want one of each", admitted, refused)
	}
}
