package schema

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const (
	testConnectorID    = "hubspot"
	testConnectionID   = "connection-fixture"
	testAccountID      = "account-fixture"
	testPermissionRev  = "permissions-revision-1"
	testSourceRevision = "source-revision-1"
	testResource       = "contacts"
	testCapability     = "hubspot.property-metadata"
)

func testBinding(accountID string) Binding {
	return Binding{
		ConnectorID:        testConnectorID,
		ConnectionID:       testConnectionID,
		ProviderAccountID:  accountID,
		PermissionRevision: testPermissionRev,
		Resource:           testResource,
		SourceRevision:     testSourceRevision,
	}
}

func declaredStringEnumSchema(values ...string) contract.Schema {
	declared := contract.Schema{Type: contract.SchemaTypeString, Enum: make([]contract.PresentValue, 0, len(values))}
	for _, value := range values {
		declared.Enum = append(declared.Enum, contract.NewStringValue(value))
	}
	return declared
}

func testFields() []Field {
	return []Field{
		{
			Name: "stage", Pointer: "/properties/lifecyclestage",
			Schema: declaredStringEnumSchema("open", "closed"), Writable: true,
		},
		{
			Name: "tier", Pointer: "/properties/tier",
			Schema: declaredStringEnumSchema("free", "pro"), Writable: true,
		},
		{
			Name: "amount", Pointer: "/properties/amount",
			Schema: contract.Schema{Type: contract.SchemaTypeDecimal}, Writable: true, Precision: 12, Scale: 2,
		},
		{
			Name: "id", Pointer: "/properties/id",
			Schema: contract.Schema{Type: contract.SchemaTypeString}, Required: true,
		},
	}
}

func testObservation(fields []Field) Observation {
	return Observation{Capability: testCapability, Resource: testResource, Available: true, Fields: fields}
}

func testCandidate(t *testing.T, binding Binding, fields []Field) Candidate {
	t.Helper()
	candidate, err := Discover(binding, []Observation{testObservation(fields)})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return candidate
}

func testStore(t *testing.T, binding Binding) *Store {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "connection-"+binding.ConnectionID)
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatalf("create connection state directory: %v", err)
	}
	store, err := NewStore(directory)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

func promoteComplete(t *testing.T, store *Store, binding Binding) Snapshot {
	t.Helper()
	promoted, err := store.Promote(binding, testCandidate(t, binding, testFields()), "")
	if err != nil {
		t.Fatalf("Promote complete candidate: %v", err)
	}
	return promoted
}

func fieldNamed(t *testing.T, fields []Field, name string) Field {
	t.Helper()
	for _, field := range fields {
		if field.Name == name {
			return field
		}
	}
	t.Fatalf("field %q is absent from %#v", name, fields)
	return Field{}
}

func fieldNames(fields []Field) []string {
	names := make([]string, 0, len(fields))
	for _, field := range fields {
		names = append(names, field.Name)
	}
	slices.Sort(names)
	return names
}

func gapWithSubject(t *testing.T, completeness Completeness, kind GapKind, subject string) Gap {
	t.Helper()
	for _, gap := range completeness.Gaps {
		if gap.Kind == kind && gap.Subject == subject {
			return gap
		}
	}
	t.Fatalf("completeness %#v has no %s gap for %q", completeness, kind, subject)
	return Gap{}
}

func snapshotFiles(t *testing.T, store *Store) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(store.directory, snapshotDirectoryName, snapshotPayloadDirectoryName))
	if err != nil {
		t.Fatalf("read snapshot payload directory: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}

func stateEntryNames(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read connection state directory: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}

func TestDynamicFieldDriftBeforeWrite(t *testing.T) {
	t.Run("V36_complete_snapshot_keeps_its_content_fingerprint_and_format_version_separate", func(t *testing.T) {
		binding := testBinding(testAccountID)
		store := testStore(t, binding)
		if err := seedConnectionConfig(t, store.directory, `{"connector_id":"hubspot","provider_account_id":"account-fixture"}`); err != nil {
			t.Fatalf("seed connection config: %v", err)
		}
		active := promoteComplete(t, store, binding)

		if !active.Completeness.Complete || len(active.Completeness.Gaps) != 0 {
			t.Fatalf("promoted completeness = %#v; want an explicit complete discovery", active.Completeness)
		}
		if active.Binding != binding {
			t.Fatalf("promoted binding = %#v; want %#v", active.Binding, binding)
		}
		content, err := active.ContentFingerprint()
		if err != nil {
			t.Fatalf("ContentFingerprint: %v", err)
		}
		if active.Fingerprint != content {
			t.Fatalf("fingerprint = %q; want the content fingerprint %q", active.Fingerprint, content)
		}
		if !strings.HasPrefix(string(active.Fingerprint), "sha256:") {
			t.Fatalf("fingerprint = %q; want a content-addressed sha256 reference", active.Fingerprint)
		}

		// The document format version is not a schema revision: the same content
		// projected through another format revision keeps one content fingerprint.
		projected := active
		projected.SchemaVersion = "pm.schema-snapshot/next"
		projectedFingerprint, err := projected.ContentFingerprint()
		if err != nil {
			t.Fatalf("ContentFingerprint of the re-projected document: %v", err)
		}
		if projectedFingerprint != active.Fingerprint {
			t.Fatalf("format version changed the content fingerprint to %q", projectedFingerprint)
		}

		selection, present, err := store.Selection()
		if err != nil || !present || selection != active.Fingerprint {
			t.Fatalf("selection = %q present=%t err=%v; want the promoted fingerprint", selection, present, err)
		}
		config := decodeConfigDocument(t, store.directory)
		if len(config) != 3 || string(config["connector_id"]) != `"hubspot"` ||
			string(config["provider_account_id"]) != `"account-fixture"` {
			t.Fatalf("connection config members = %#v; want the unrelated members preserved exactly", config)
		}
		if string(config[selectionMemberName]) != `"`+string(active.Fingerprint)+`"` {
			t.Fatalf("connection config selection = %s; want %q", config[selectionMemberName], active.Fingerprint)
		}
		if !slices.Equal(stateEntryNames(t, store.directory), []string{selectionFileName, snapshotDirectoryName}) {
			t.Fatalf("connection state entries = %v; want the config and the snapshot directory only", stateEntryNames(t, store.directory))
		}
		current, present, err := store.Active(binding)
		if err != nil || !present || current.Fingerprint != active.Fingerprint {
			t.Fatalf("active = %#v present=%t err=%v; want the promoted snapshot", current, present, err)
		}
		if !slices.Equal(fieldNames(current.Fields), []string{"amount", "id", "stage", "tier"}) {
			t.Fatalf("normalized fields = %v; want every declared field by provider spelling", fieldNames(current.Fields))
		}
		amount := fieldNamed(t, current.Fields, "amount")
		if amount.Schema.Type != contract.SchemaTypeDecimal || amount.Precision != 12 || amount.Scale != 2 || !amount.Writable {
			t.Fatalf("amount = %#v; want preserved type, precision, scale and writability", amount)
		}
		stage := fieldNamed(t, current.Fields, "stage")
		if len(stage.Schema.Enum) != 2 || !stage.Schema.Enum[0].Equal(contract.NewStringValue("open")) {
			t.Fatalf("stage enum = %#v; want the declared enum values in declaration order", stage.Schema.Enum)
		}
		if fieldNamed(t, current.Fields, "id").Writable {
			t.Fatalf("id = %#v; want a read-only declaration to survive normalization", fieldNamed(t, current.Fields, "id"))
		}

		// Replaying identical content is not a new schema revision.
		replay, err := store.Promote(binding, testCandidate(t, binding, testFields()), active.Fingerprint)
		if err != nil {
			t.Fatalf("replay promotion: %v", err)
		}
		if replay.Fingerprint != active.Fingerprint {
			t.Fatalf("replay fingerprint = %q; want the unchanged content fingerprint", replay.Fingerprint)
		}
		drift, err := DetectDrift(active, replay.Candidate())
		if err != nil {
			t.Fatalf("DetectDrift of an exact replay: %v", err)
		}
		if !drift.Empty() {
			t.Fatalf("drift of an exact replay = %#v; want unchanged metadata to stay usable", drift)
		}
		if !slices.Equal(snapshotFiles(t, store), []string{snapshotFileName(active.Fingerprint)}) {
			t.Fatalf("snapshot payloads = %v; want one immutable payload for the unchanged content", snapshotFiles(t, store))
		}
	})

	t.Run("V36_relevant_drift_invalidates_the_bound_fields_and_plans_before_a_write", func(t *testing.T) {
		binding := testBinding(testAccountID)
		store := testStore(t, binding)
		active := promoteComplete(t, store, binding)
		stageWitness, err := Witness(active, []string{"stage"})
		if err != nil {
			t.Fatalf("Witness: %v", err)
		}
		if err := RevalidateWrite(stageWitness, active); err != nil {
			t.Fatalf("unchanged witness: %v", err)
		}

		fields := testFields()
		enumDrifted := fields[0]
		enumDrifted.Schema = declaredStringEnumSchema("open", "closed", "archived")
		fields[0] = enumDrifted
		drifted := testCandidate(t, binding, fields)
		drift, err := DetectDrift(active, drifted)
		if err != nil {
			t.Fatalf("DetectDrift: %v", err)
		}
		if drift.Empty() {
			t.Fatalf("enum drift = %#v; want a relevant change", drift)
		}
		if !slices.Equal(drift.AffectedFields(), []string{"stage"}) {
			t.Fatalf("affected fields = %v; want only the drifted field", drift.AffectedFields())
		}
		if !drift.InvalidatesPlan(PlanBinding{ProfileID: "plan-stage", SchemaRef: active.Fingerprint, Fields: []string{"stage"}}) {
			t.Fatalf("drift %#v did not invalidate the plan bound to the drifted field", drift)
		}
		if drift.InvalidatesPlan(PlanBinding{ProfileID: "plan-amount", SchemaRef: active.Fingerprint, Fields: []string{"amount"}}) {
			t.Fatalf("drift %#v invalidated a plan bound to an unchanged field", drift)
		}
		promoted, err := store.Promote(binding, drifted, active.Fingerprint)
		if err != nil {
			t.Fatalf("promote drifted candidate: %v", err)
		}
		if promoted.Fingerprint == active.Fingerprint {
			t.Fatalf("drifted promotion kept the fingerprint %q", promoted.Fingerprint)
		}
		if err := RevalidateWrite(stageWitness, promoted); !errors.Is(err, ErrSchemaDrift) {
			t.Fatalf("drifted witness revalidation = %v; want %v", err, ErrSchemaDrift)
		}
		amountWitness, err := Witness(active, []string{"amount"})
		if err != nil {
			t.Fatalf("Witness(amount): %v", err)
		}
		if err := RevalidateWrite(amountWitness, promoted); err != nil {
			t.Fatalf("witness of an unchanged field = %v; want unrelated drift to stay usable", err)
		}

		// A writability change is relevant even when the type and enum are stable.
		readOnly := testFields()
		readOnly[0].Writable = false
		readOnlyDrift, err := DetectDrift(active, testCandidate(t, binding, readOnly))
		if err != nil {
			t.Fatalf("DetectDrift of a read-only change: %v", err)
		}
		if !slices.Contains(changeKinds(t, readOnlyDrift, "stage"), FieldWritabilityChanged) {
			t.Fatalf("read-only drift = %#v; want a writability change for stage", readOnlyDrift)
		}
		if !readOnlyDrift.InvalidatesPlan(PlanBinding{ProfileID: "plan-stage", SchemaRef: active.Fingerprint, Fields: []string{"stage"}}) {
			t.Fatalf("read-only drift %#v did not invalidate the write plan", readOnlyDrift)
		}

		// A type change is relevant and never silently remapped.
		retyped := testFields()
		retyped[2].Schema = contract.Schema{Type: contract.SchemaTypeString}
		retyped[2].Precision = 0
		retyped[2].Scale = 0
		typeDrift, err := DetectDrift(active, testCandidate(t, binding, retyped))
		if err != nil {
			t.Fatalf("DetectDrift of a type change: %v", err)
		}
		if !slices.Contains(changeKinds(t, typeDrift, "amount"), FieldTypeChanged) {
			t.Fatalf("type drift = %#v; want a type change for amount", typeDrift)
		}

		// Account and source-revision drift invalidate every field of the binding.
		account := testCandidate(t, binding, testFields())
		account.Binding.ProviderAccountID = "account-other"
		accountDrift, err := DetectDrift(active, account)
		if err != nil {
			t.Fatalf("DetectDrift of account drift: %v", err)
		}
		if !accountDrift.BindingChanged || !slices.Equal(accountDrift.AffectedFields(), []string{"amount", "id", "stage", "tier"}) {
			t.Fatalf("account drift = %#v; want the binding change to affect every field", accountDrift)
		}
		if _, err := store.Promote(binding, account, active.Fingerprint); !errors.Is(err, ErrSnapshotScope) {
			t.Fatalf("promotion of a foreign account candidate = %v; want %v", err, ErrSnapshotScope)
		}
		revision := testCandidate(t, binding, testFields())
		revision.Binding.SourceRevision = "source-revision-2"
		revisionDrift, err := DetectDrift(active, revision)
		if err != nil {
			t.Fatalf("DetectDrift of source-revision drift: %v", err)
		}
		if !revisionDrift.BindingChanged {
			t.Fatalf("source-revision drift = %#v; want a binding change", revisionDrift)
		}

		// A provenance-only move is not constraint drift.
		relocated := testFields()
		relocated[0].Pointer = "/properties/renamed-still-the-same-declaration"
		pointerDrift, err := DetectDrift(active, testCandidate(t, binding, relocated))
		if err != nil {
			t.Fatalf("DetectDrift of a provenance move: %v", err)
		}
		if !pointerDrift.Empty() {
			t.Fatalf("provenance move = %#v; want no constraint drift", pointerDrift)
		}
	})

	t.Run("V36_partial_discovery_never_replaces_the_complete_active_snapshot", func(t *testing.T) {
		binding := testBinding(testAccountID)
		store := testStore(t, binding)
		active := promoteComplete(t, store, binding)

		partial, err := Discover(binding, []Observation{{
			Capability:        testCapability,
			Resource:          testResource,
			Available:         true,
			Truncated:         true,
			DeniedPermissions: []string{"contacts.schema.read"},
			Fields:            testFields(),
		}})
		if err != nil {
			t.Fatalf("Discover partial: %v", err)
		}
		if partial.Completeness.Complete {
			t.Fatalf("truncated discovery completeness = %#v; want an explicit partial result", partial.Completeness)
		}
		gapWithSubject(t, partial.Completeness, GapTruncatedMetadata, testCapability)
		gapWithSubject(t, partial.Completeness, GapPermissionDenied, "contacts.schema.read")
		if _, err := store.Promote(binding, partial, active.Fingerprint); !errors.Is(err, ErrIncompleteDiscovery) {
			t.Fatalf("promotion of a partial discovery = %v; want %v", err, ErrIncompleteDiscovery)
		}
		current, present, err := store.Active(binding)
		if err != nil || !present || current.Fingerprint != active.Fingerprint {
			t.Fatalf("active after a partial promotion = %#v present=%t err=%v; want the complete snapshot", current, present, err)
		}
		fingerprint, err := partial.Fingerprint()
		if err != nil {
			t.Fatalf("partial fingerprint: %v", err)
		}
		if slices.Contains(snapshotFiles(t, store), snapshotFileName(fingerprint)) {
			t.Fatalf("partial discovery left a durable payload behind: %v", snapshotFiles(t, store))
		}

		// An unavailable capability is explicit and blocking, and the same gap is
		// recorded once even when two observations report it.
		unavailable, err := Discover(binding, []Observation{
			{Capability: testCapability, Resource: testResource, Available: false, Fields: []Field{}},
			{Capability: "hubspot.custom-object-metadata", Resource: testResource, Available: true, Fields: []Field{}},
		})
		if err != nil {
			t.Fatalf("Discover unavailable: %v", err)
		}
		if unavailable.Completeness.Complete {
			t.Fatalf("unavailable capability completeness = %#v; want incomplete", unavailable.Completeness)
		}
		gapWithSubject(t, unavailable.Completeness, GapCapabilityUnavailable, testCapability)

		// An unsupported shape stays explicit without suppressing supported fields.
		unsupported, err := Discover(binding, []Observation{{
			Capability:  testCapability,
			Resource:    testResource,
			Available:   true,
			Unsupported: []string{"company_association"},
			Fields: append(testFields(), Field{
				Name: "legacy", Pointer: "/properties/legacy",
				Schema: contract.Schema{Type: contract.SchemaTypeUnion, OneOf: []contract.Schema{
					{Type: contract.SchemaTypeString},
					{Type: contract.SchemaTypeObject, Properties: map[string]contract.Schema{}},
				}},
			}),
		}})
		if err != nil {
			t.Fatalf("Discover unsupported: %v", err)
		}
		if !unsupported.Completeness.Complete {
			t.Fatalf("unsupported-shape completeness = %#v; want an explicit but complete discovery", unsupported.Completeness)
		}
		gapWithSubject(t, unsupported.Completeness, GapUnsupportedShape, "company_association")
		gapWithSubject(t, unsupported.Completeness, GapUnsupportedShape, "/properties/legacy")
		if slices.Contains(fieldNames(unsupported.Fields), "legacy") {
			t.Fatalf("unsupported union field was recorded: %v", fieldNames(unsupported.Fields))
		}
		if !slices.Equal(fieldNames(unsupported.Fields), []string{"amount", "id", "stage", "tier"}) {
			t.Fatalf("unsupported shape suppressed supported fields: %v", fieldNames(unsupported.Fields))
		}
	})
}

func seedConnectionConfig(t *testing.T, directory, document string) error {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, selectionFileName), []byte(document), 0o600); err != nil {
		return err
	}
	return os.Chmod(filepath.Join(directory, selectionFileName), 0o600)
}

func changeKinds(t *testing.T, drift Drift, field string) []FieldChangeKind {
	t.Helper()
	kinds := make([]FieldChangeKind, 0, len(drift.Changes))
	for _, change := range drift.Changes {
		if change.Field == field {
			kinds = append(kinds, change.Kind)
		}
	}
	slices.Sort(kinds)
	return kinds
}

func decodeConfigDocument(t *testing.T, directory string) map[string]json.RawMessage {
	t.Helper()
	document, err := os.ReadFile(filepath.Join(directory, selectionFileName))
	if err != nil {
		t.Fatalf("read connection config: %v", err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(document, &members); err != nil {
		t.Fatalf("connection config is not a JSON object: %v", err)
	}
	return members
}

// TestSchemaPromotionCompareAndSwapAdmitsOneWriter races competing promotions
// of one expected selection through independently opened stores over the same
// connection state, as separate processes would. Exactly one replacement is
// admitted; every other writer observes the selection conflict.
func TestSchemaPromotionCompareAndSwapAdmitsOneWriter(t *testing.T) {
	const writers = 20
	binding := testBinding(testAccountID)
	store := testStore(t, binding)
	active := promoteComplete(t, store, binding)

	candidates := make([]Candidate, writers)
	for index := range candidates {
		fields := append(testFields(), Field{
			Name: "variant_" + strings.Repeat("v", index+1), Pointer: "/properties/variant_" + strings.Repeat("v", index+1),
			Schema: contract.Schema{Type: contract.SchemaTypeString},
		})
		candidates[index] = testCandidate(t, binding, fields)
	}
	start := make(chan struct{})
	results := make(chan error, writers)
	for index := range candidates {
		writer, err := NewStore(store.directory)
		if err != nil {
			t.Fatalf("NewStore for writer %d: %v", index, err)
		}
		go func(writer *Store, candidate Candidate) {
			<-start
			_, err := writer.Promote(binding, candidate, active.Fingerprint)
			results <- err
		}(writer, candidates[index])
	}
	close(start)
	admitted := 0
	for range writers {
		err := <-results
		switch {
		case err == nil:
			admitted++
		case errors.Is(err, ErrSelectionConflict):
		default:
			t.Errorf("competing promotion failed with %v; want success or %v", err, ErrSelectionConflict)
		}
	}
	if admitted != 1 {
		t.Fatalf("compare-and-swap admitted %d competing replacements of the same expected snapshot; want exactly 1", admitted)
	}
	selected, present, err := store.Selection()
	if err != nil || !present || selected == active.Fingerprint {
		t.Fatalf("selection after the race = %q present=%t err=%v; want the one admitted replacement", selected, present, err)
	}
	if !slices.Equal(stateEntryNames(t, store.directory), []string{selectionFileName, snapshotDirectoryName}) {
		t.Fatalf("connection state entries = %v; want the config and the snapshot directory only", stateEntryNames(t, store.directory))
	}
}
