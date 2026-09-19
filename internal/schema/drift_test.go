package schema

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestScopedCompleteSchema(t *testing.T) {
	t.Run("V73_two_accounts_with_the_same_resource_name_never_share_metadata", func(t *testing.T) {
		accountA := testBinding("account-a")
		accountB := testBinding("account-b")
		storeA := testStore(t, accountA)
		storeB := testStore(t, accountB)
		fieldsA := []Field{
			{Name: "lifecyclestage", Pointer: "/properties/lifecyclestage", Schema: declaredStringEnumSchema("lead", "customer"), Writable: true},
			{Name: "id", Pointer: "/properties/id", Schema: contract.Schema{Type: contract.SchemaTypeString}, Required: true},
		}
		fieldsB := []Field{
			{Name: "custom_tier", Pointer: "/properties/custom_tier", Schema: declaredStringEnumSchema("gold", "silver"), Writable: true},
			{Name: "id", Pointer: "/properties/id", Schema: contract.Schema{Type: contract.SchemaTypeString}, Required: true},
		}
		activeA := promoteCandidate(t, storeA, accountA, fieldsA)
		activeB := promoteCandidate(t, storeB, accountB, fieldsB)

		if !slices.Equal(fieldNames(activeA.Fields), []string{"id", "lifecyclestage"}) ||
			!slices.Equal(fieldNames(activeB.Fields), []string{"custom_tier", "id"}) {
			t.Fatalf("account metadata = %v / %v; want account-scoped declarations for the same %q resource",
				fieldNames(activeA.Fields), fieldNames(activeB.Fields), testResource)
		}
		if activeA.Fingerprint == activeB.Fingerprint {
			t.Fatalf("two accounts share the content fingerprint %q", activeA.Fingerprint)
		}
		// A payload copied between connections is still bounded to its own scope:
		// the selection may name it, but the store never serves it for another
		// account or connection.
		copyPayload(t, storeA, storeB, activeA.Fingerprint)
		if _, err := storeB.LoadFor(accountB, activeA.Fingerprint); !errors.Is(err, ErrSnapshotScope) {
			t.Fatalf("foreign account reference = %v; want %v", err, ErrSnapshotScope)
		}
		otherConnection := accountB
		otherConnection.ConnectionID = "connection-other"
		if _, err := storeB.LoadFor(otherConnection, activeB.Fingerprint); !errors.Is(err, ErrSnapshotScope) {
			t.Fatalf("foreign connection reference = %v; want %v", err, ErrSnapshotScope)
		}
		pointSelection(t, storeB, activeA.Fingerprint)
		if _, present, err := storeB.Active(accountB); !errors.Is(err, ErrSnapshotScope) || present {
			t.Fatalf("active foreign payload = present=%t err=%v; want %v", present, err, ErrSnapshotScope)
		}
	})

	t.Run("V73_selection_conflicts_and_partial_candidates_leave_the_complete_snapshot_active", func(t *testing.T) {
		binding := testBinding(testAccountID)
		store := testStore(t, binding)
		active := promoteComplete(t, store, binding)

		fields := testFields()
		fields[0].Schema = declaredStringEnumSchema("open", "closed", "archived")
		drifted := testCandidate(t, binding, fields)
		stale := contract.SemanticDigest("sha256:" + repeatHex("0", 64))
		if _, err := store.Promote(binding, drifted, stale); !errors.Is(err, ErrSelectionConflict) {
			t.Fatalf("stale expected reference = %v; want %v", err, ErrSelectionConflict)
		}
		assertActive(t, store, binding, active.Fingerprint)

		partial, err := Discover(binding, []Observation{{
			Capability: testCapability, Resource: testResource, Available: true, Truncated: true, Fields: testFields(),
		}})
		if err != nil {
			t.Fatalf("Discover partial: %v", err)
		}
		if _, err := store.Promote(binding, partial, active.Fingerprint); !errors.Is(err, ErrIncompleteDiscovery) {
			t.Fatalf("partial promotion = %v; want %v", err, ErrIncompleteDiscovery)
		}
		assertActive(t, store, binding, active.Fingerprint)

		// An incomplete document that reached the state directory by any other
		// route is refused at the read boundary rather than served.
		document, err := partial.Snapshot()
		if err != nil {
			t.Fatalf("partial snapshot document: %v", err)
		}
		encoded, err := json.Marshal(document)
		if err != nil {
			t.Fatalf("encode partial snapshot: %v", err)
		}
		writePayload(t, store, document.Fingerprint, encoded)
		pointSelection(t, store, document.Fingerprint)
		if _, present, err := store.Active(binding); !errors.Is(err, ErrIncompleteDiscovery) || present {
			t.Fatalf("incomplete active snapshot = present=%t err=%v; want %v", present, err, ErrIncompleteDiscovery)
		}
	})

	t.Run("V73_an_unrelated_field_change_does_not_invalidate_unaffected_profiles", func(t *testing.T) {
		binding := testBinding(testAccountID)
		store := testStore(t, binding)
		active := promoteComplete(t, store, binding)
		stagePlan := PlanBinding{ProfileID: "plan-stage", SchemaRef: active.Fingerprint, Fields: []string{"stage"}}
		tierPlan := PlanBinding{ProfileID: "plan-tier", SchemaRef: active.Fingerprint, Fields: []string{"tier"}}
		wholePlan := PlanBinding{ProfileID: "plan-resource", SchemaRef: active.Fingerprint}

		fields := testFields()
		fields[0].Schema = declaredStringEnumSchema("open", "closed", "archived")
		drift, err := DetectDrift(active, testCandidate(t, binding, fields))
		if err != nil {
			t.Fatalf("DetectDrift: %v", err)
		}
		if !slices.Equal(drift.AffectedFields(), []string{"stage"}) {
			t.Fatalf("affected fields = %v; want the drifted field only", drift.AffectedFields())
		}
		if !drift.InvalidatesPlan(stagePlan) || drift.InvalidatesPlan(tierPlan) {
			t.Fatalf("plan invalidation = stage:%t tier:%t; want only the dependent profile invalidated",
				drift.InvalidatesPlan(stagePlan), drift.InvalidatesPlan(tierPlan))
		}
		if !drift.InvalidatesPlan(wholePlan) {
			t.Fatalf("drift %#v did not invalidate a profile bound to the whole snapshot", drift)
		}

		promoted, err := store.Promote(binding, testCandidate(t, binding, fields), active.Fingerprint)
		if err != nil {
			t.Fatalf("promote drifted candidate: %v", err)
		}
		stageWitness, err := Witness(active, stagePlan.Fields)
		if err != nil {
			t.Fatalf("Witness(stage): %v", err)
		}
		tierWitness, err := Witness(active, tierPlan.Fields)
		if err != nil {
			t.Fatalf("Witness(tier): %v", err)
		}
		if err := RevalidateWrite(stageWitness, promoted); !errors.Is(err, ErrSchemaDrift) {
			t.Fatalf("stage revalidation = %v; want %v", err, ErrSchemaDrift)
		}
		if err := RevalidateWrite(tierWitness, promoted); err != nil {
			t.Fatalf("tier revalidation = %v; want unrelated metadata to stay usable", err)
		}
	})

	t.Run("V73_snapshot_content_is_immutable_and_fingerprint_verified", func(t *testing.T) {
		binding := testBinding(testAccountID)
		store := testStore(t, binding)
		active := promoteComplete(t, store, binding)
		original := readPayload(t, store, active.Fingerprint)

		fields := testFields()
		fields[1].Schema = declaredStringEnumSchema("free", "pro", "enterprise")
		promoted, err := store.Promote(binding, testCandidate(t, binding, fields), active.Fingerprint)
		if err != nil {
			t.Fatalf("promote drifted candidate: %v", err)
		}
		if resolved, err := store.LoadFor(binding, active.Fingerprint); err != nil || resolved.Fingerprint != active.Fingerprint {
			t.Fatalf("retained snapshot = %#v err=%v; want the earlier revision retained", resolved, err)
		}
		if !slices.Equal(readPayload(t, store, active.Fingerprint), original) {
			t.Fatalf("promotion rewrote the earlier immutable payload")
		}
		if !slices.Equal(snapshotFiles(t, store), sortedNames(snapshotFileName(active.Fingerprint), snapshotFileName(promoted.Fingerprint))) {
			t.Fatalf("snapshot payloads = %v; want both immutable revisions", snapshotFiles(t, store))
		}

		writePayload(t, store, active.Fingerprint, []byte(`{"schema_version":"pm.schema-snapshot/v1"}`))
		if _, err := store.LoadFor(binding, active.Fingerprint); !errors.Is(err, ErrSnapshotCorrupt) {
			t.Fatalf("tampered payload = %v; want %v", err, ErrSnapshotCorrupt)
		}
		if _, _, err := store.Active(binding); err != nil {
			t.Fatalf("active after a retained payload was tampered: %v", err)
		}
	})
}

func promoteCandidate(t *testing.T, store *Store, binding Binding, fields []Field) Snapshot {
	t.Helper()
	promoted, err := store.Promote(binding, testCandidate(t, binding, fields), "")
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	return promoted
}

func assertActive(t *testing.T, store *Store, binding Binding, reference contract.SemanticDigest) {
	t.Helper()
	active, present, err := store.Active(binding)
	if err != nil || !present || active.Fingerprint != reference {
		t.Fatalf("active = %#v present=%t err=%v; want %q", active, present, err, reference)
	}
}

func pointSelection(t *testing.T, store *Store, reference contract.SemanticDigest) {
	t.Helper()
	document, err := json.Marshal(map[string]string{selectionMemberName: string(reference)})
	if err != nil {
		t.Fatalf("encode selection: %v", err)
	}
	if err := os.WriteFile(filepath.Join(store.directory, selectionFileName), document, 0o600); err != nil {
		t.Fatalf("point the connection selection: %v", err)
	}
}

func copyPayload(t *testing.T, from, to *Store, reference contract.SemanticDigest) {
	t.Helper()
	payload := readPayload(t, from, reference)
	writePayload(t, to, reference, payload)
}

func readPayload(t *testing.T, store *Store, reference contract.SemanticDigest) []byte {
	t.Helper()
	document, err := os.ReadFile(filepath.Join(store.directory, snapshotDirectoryName, snapshotPayloadDirectoryName, snapshotFileName(reference)))
	if err != nil {
		t.Fatalf("read snapshot payload %q: %v", reference, err)
	}
	return document
}

func writePayload(t *testing.T, store *Store, reference contract.SemanticDigest, document []byte) {
	t.Helper()
	directory := filepath.Join(store.directory, snapshotDirectoryName, snapshotPayloadDirectoryName)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("create snapshot payload directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, snapshotFileName(reference)), document, 0o600); err != nil {
		t.Fatalf("write snapshot payload %q: %v", reference, err)
	}
}

func sortedNames(values ...string) []string {
	slices.Sort(values)
	return values
}

func repeatHex(character string, count int) string {
	repeated := make([]byte, 0, count*len(character))
	for range count {
		repeated = append(repeated, character...)
	}
	return string(repeated)
}
