package app_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/connectors"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

const (
	connectorCompositionConnectorID  = "fixture-connector"
	connectorCompositionReadID       = "op-composition-read"
	connectorCompositionWriteID      = "op-composition-write"
	connectorCompositionCommandName  = "inspect-widget"
	connectorCompositionSecretMarker = "connector-bundle-bytes-must-not-leak"
)

// TestConnectorCompositionWithoutDeclaredBundles proves the zero-bundle
// deployment stays truthfully unavailable: no connector, no registered
// callback, and a resolver that refuses every operation.
func TestConnectorCompositionWithoutDeclaredBundles(t *testing.T) {
	t.Parallel()
	composition, err := app.OpenConnectorComposition(app.ConnectorCompositionConfig{})
	if err != nil {
		t.Fatalf("compose empty connector declaration: %v", err)
	}
	if composition.Configured() {
		t.Fatal("empty connector declaration became configured")
	}
	registry := composition.Registry()
	if registry == nil || registry.Revision().Validate() != nil {
		t.Fatalf("empty connector registry = %#v", registry)
	}
	if catalog := registry.Catalog(); len(catalog.Connectors) != 0 {
		t.Fatalf("empty connector declaration projected %d connectors", len(catalog.Connectors))
	}
	handlers := &app.HandlerRegistry{}
	if err := composition.RegisterDirectRead(handlers, app.HTTPDirectReadDependencies{}); err != nil {
		t.Fatalf("register direct read without declared bundles: %v", err)
	}
	if manifest := app.BuildHandlerManifest(handlers); len(manifest.Handlers) != 0 {
		t.Fatalf("undeclared connector composition registered %d handlers", len(manifest.Handlers))
	}
	resolver := composition.Resolver()
	if resolver == nil {
		t.Fatal("undeclared connector composition exposed no resolver")
	}
	selection, err := resolver(
		context.Background(),
		contract.Scope{OrgID: "org-undeclared", WorkspaceID: "workspace-undeclared"},
		"connection-undeclared",
		connectorCompositionReadID,
	)
	if !errors.Is(err, app.ErrConnectorUnavailable) || selection.Operation.OperationID != "" {
		t.Fatalf("undeclared resolution = (%#v, %v); want explicit unavailability", selection, err)
	}
}

// TestConnectorCompositionRefusesUnsafeBundleDeclarations proves every bundle
// input failure is explicit, bounded and byte-safe, and that connector order is
// canonical regardless of declaration order.
func TestConnectorCompositionRefusesUnsafeBundleDeclarations(t *testing.T) {
	t.Parallel()
	document := connectorCompositionDocument(t, connectorCompositionBundle(t))
	bundlePath := connectorCompositionWriteBundle(t, "connector.bundle.json", document)

	t.Run("unreadable declared bundle", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing.bundle.json")
		_, err := app.OpenConnectorComposition(app.ConnectorCompositionConfig{
			BundlePaths: []string{missing},
		})
		if err == nil {
			t.Fatal("missing declared connector bundle composed successfully")
		}
	})

	t.Run("directory declaration", func(t *testing.T) {
		directory := t.TempDir()
		_, err := app.OpenConnectorComposition(app.ConnectorCompositionConfig{
			BundlePaths: []string{directory},
		})
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("directory declaration error = %v; want regular-file refusal", err)
		}
	})

	t.Run("symlink declaration", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "linked.bundle.json")
		if err := os.Symlink(bundlePath, link); err != nil {
			t.Fatalf("create declared bundle symlink: %v", err)
		}
		_, err := app.OpenConnectorComposition(app.ConnectorCompositionConfig{
			BundlePaths: []string{link},
		})
		if err == nil {
			t.Fatal("symlinked declared connector bundle composed successfully")
		}
	})

	t.Run("relative declaration", func(t *testing.T) {
		for _, declared := range []string{"connectors/github.bundle.json", ""} {
			_, err := app.OpenConnectorComposition(app.ConnectorCompositionConfig{
				BundlePaths: []string{declared},
			})
			if err == nil {
				t.Fatalf("relative declaration %q composed successfully", declared)
			}
		}
	})

	t.Run("oversized declared bundle", func(t *testing.T) {
		oversized := append(
			[]byte(connectorCompositionSecretMarker),
			bytes.Repeat([]byte("x"), int(contract.MaxContractDocumentBytes))...,
		)
		if int64(len(oversized)) <= contract.MaxContractDocumentBytes {
			t.Fatal("oversized declaration fixture did not exceed the bundle byte limit")
		}
		path := connectorCompositionWriteBundle(t, "oversized.bundle.json", oversized)
		_, err := app.OpenConnectorComposition(app.ConnectorCompositionConfig{
			BundlePaths: []string{path},
		})
		if err == nil || !strings.Contains(err.Error(), "byte limit") {
			t.Fatalf("oversized declaration error = %v; want byte-limit refusal", err)
		}
		if strings.Contains(err.Error(), connectorCompositionSecretMarker) {
			t.Fatalf("oversized declaration error leaked declared bytes: %v", err)
		}
	})

	t.Run("undecodable declared bundle", func(t *testing.T) {
		undecodable := []byte(`{"schema_version":"` + connectors.BundleSchemaVersion +
			`","connector_id":"` + connectorCompositionConnectorID +
			`","source_digest":"sha256:` + strings.Repeat("a", 64) +
			`","semantics_revision":"sha256:` + strings.Repeat("b", 64) +
			`","operations":[],"commands":[],"unavailable_operations":[],"source_document":"` +
			connectorCompositionSecretMarker + `"}`)
		path := connectorCompositionWriteBundle(t, "undecodable.bundle.json", undecodable)
		_, err := app.OpenConnectorComposition(app.ConnectorCompositionConfig{
			BundlePaths: []string{path},
		})
		if err == nil {
			t.Fatal("undecodable declared connector bundle composed successfully")
		}
		if strings.Contains(err.Error(), connectorCompositionSecretMarker) {
			t.Fatalf("undecodable declaration error leaked declared bytes: %v", err)
		}
	})

	t.Run("duplicate connector declaration", func(t *testing.T) {
		duplicate := connectorCompositionWriteBundle(t, "duplicate.bundle.json", document)
		_, err := app.OpenConnectorComposition(app.ConnectorCompositionConfig{
			BundlePaths: []string{bundlePath, duplicate},
		})
		if err == nil || !strings.Contains(err.Error(), "duplicates connector") {
			t.Fatalf("duplicate declaration error = %v; want duplicate-connector refusal", err)
		}
	})

	t.Run("canonical connector order", func(t *testing.T) {
		other := connectorCompositionBundle(t)
		other.ConnectorID = connectorCompositionConnectorID + "-secondary"
		otherPath := connectorCompositionWriteBundle(
			t, "secondary.bundle.json", connectorCompositionDocument(t, other),
		)
		declared := []struct {
			name  string
			paths []string
		}{
			{name: "declared order", paths: []string{bundlePath, otherPath}},
			{name: "reversed order", paths: []string{otherPath, bundlePath}},
			{name: "single connector", paths: []string{otherPath}},
		}
		revisions := make([]string, 0, len(declared))
		for _, candidate := range declared {
			composition, err := app.OpenConnectorComposition(app.ConnectorCompositionConfig{
				BundlePaths: candidate.paths,
			})
			if err != nil {
				t.Fatalf("%s composition: %v", candidate.name, err)
			}
			if !composition.Configured() {
				t.Fatalf("%s composition is unconfigured", candidate.name)
			}
			revisions = append(revisions, string(composition.Registry().Revision()))
		}
		if revisions[0] != revisions[1] {
			t.Fatalf("declaration order changed the catalog revision: %v", revisions)
		}
		if revisions[0] == revisions[2] {
			t.Fatalf("a distinct bundle set shares catalog revision %q", revisions[0])
		}
	})
}

// TestConnectorCompositionResolvesDeclaredConnectorInScope proves the resolver
// reads the connection only through the caller's authorized scope transaction
// and never falls back to another connector.
func TestConnectorCompositionResolvesDeclaredConnectorInScope(t *testing.T) {
	t.Parallel()
	bundle := connectorCompositionBundle(t)
	bundlePath := connectorCompositionWriteBundle(t, "connector.bundle.json", connectorCompositionDocument(t, bundle))
	var composition *app.ConnectorComposition
	fixture := testfixture.NewOperation(t, connectorCompositionReadID, func(
		_ testfixture.ServiceDependencies,
		_ *httpapi.ServiceConfig,
	) error {
		var err error
		composition, err = app.OpenConnectorComposition(app.ConnectorCompositionConfig{
			BundlePaths: []string{bundlePath},
		})
		return err
	})
	if composition == nil {
		t.Fatal("connector composition was not constructed for the fixture")
	}
	resolver := composition.Resolver()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, present := store.ScopedTransaction(ctx); present {
		t.Fatal("scoped transaction is observable outside a transaction callback")
	}
	if _, err := resolver(
		ctx, fixture.Scope, fixture.Connection.ConnectionID, connectorCompositionReadID,
	); !errors.Is(err, app.ErrConnectorUnavailable) {
		t.Fatalf("resolution without a scope transaction = %v; want explicit unavailability", err)
	}
	want, err := app.ResolveBundledOperation(
		composition.Registry(), fixture.Connection.ConnectorID, connectorCompositionReadID,
	)
	if err != nil {
		t.Fatalf("resolve installed connector operation: %v", err)
	}
	undeclaredConnectionID := "connection-undeclared-connector"
	err = store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(
		ctx context.Context,
		tx *store.Transaction,
	) error {
		return store.RegisterConnection(ctx, tx, store.Connection{
			Scope:             fixture.Scope,
			ConnectionID:      undeclaredConnectionID,
			ConnectorID:       "connector-not-declared",
			ProviderAccountID: fixture.Connection.ProviderAccountID,
			Revision:          1,
			CredentialRef:     fixture.Connection.CredentialRef,
		})
	})
	if err != nil {
		t.Fatalf("register connection with undeclared connector: %v", err)
	}

	var (
		captured        context.Context
		selection       app.OperationSelection
		mismatchErr     error
		undeclaredEr    error
		unknownErr      error
		absentOperatErr error
	)
	err = store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(
		ctx context.Context,
		tx *store.Transaction,
	) error {
		captured = ctx
		scopedTransaction, present := store.ScopedTransaction(ctx)
		if !present || scopedTransaction != tx || !scopedTransaction.Scope().Equal(fixture.Scope) {
			t.Fatal("scoped transaction accessor did not expose the active transaction")
		}
		if _, present := store.ScopedTransaction(context.Background()); present {
			t.Fatal("scoped transaction leaked into an unrelated context")
		}
		mismatchErr = func() error {
			_, err := resolver(ctx, fixture.ForeignScope, fixture.Connection.ConnectionID, connectorCompositionReadID)
			return err
		}()
		undeclaredEr = func() error {
			_, err := resolver(ctx, fixture.Scope, undeclaredConnectionID, connectorCompositionReadID)
			return err
		}()
		unknownErr = func() error {
			_, err := resolver(ctx, fixture.Scope, "connection-absent", connectorCompositionReadID)
			return err
		}()
		absentOperatErr = func() error {
			_, err := resolver(ctx, fixture.Scope, fixture.Connection.ConnectionID, "op-composition-absent")
			return err
		}()
		selection, err = resolver(ctx, fixture.Scope, fixture.Connection.ConnectionID, connectorCompositionReadID)
		return err
	})
	if err != nil {
		t.Fatalf("resolve declared connector operation inside its scope: %v", err)
	}
	if selection.Operation.OperationID != want.Operation.OperationID ||
		selection.Operation.Transport != want.Operation.Transport ||
		len(selection.SchemaRefs) != len(want.SchemaRefs) {
		t.Fatalf("resolved selection = %#v; want %#v", selection, want)
	}
	for name, failure := range map[string]error{
		"foreign scope":        mismatchErr,
		"undeclared connector": undeclaredEr,
		"absent connection":    unknownErr,
		"absent operation":     absentOperatErr,
	} {
		if !errors.Is(failure, app.ErrConnectorUnavailable) {
			t.Fatalf("%s resolution = %v; want explicit unavailability", name, failure)
		}
	}
	if !errors.Is(undeclaredEr, connectors.ErrConnectorNotFound) {
		t.Fatalf("undeclared connector failure = %v; want connector-not-installed cause", undeclaredEr)
	}
	if _, err := resolver(
		captured, fixture.Scope, fixture.Connection.ConnectionID, connectorCompositionReadID,
	); err == nil {
		t.Fatal("retained transaction callback context resolved an operation after its callback returned")
	}
}

// TestConnectorCompositionServesDirectReadCatalogAndAdmission proves the real
// API composition projects the declared lane, refuses the lane it did not
// register, and admits a run through the shared resolver.
func TestConnectorCompositionServesDirectReadCatalogAndAdmission(t *testing.T) {
	t.Parallel()
	const requestKey = "connector-composition-admission"
	bundle := connectorCompositionBundle(t)
	bundlePath := connectorCompositionWriteBundle(t, "connector.bundle.json", connectorCompositionDocument(t, bundle))
	var (
		composition *app.ConnectorComposition
		catalog     *app.CatalogService
	)
	fixture := testfixture.NewOperation(t, connectorCompositionReadID, func(
		dependencies testfixture.ServiceDependencies,
		configuration *httpapi.ServiceConfig,
	) error {
		var err error
		composition, err = app.OpenConnectorComposition(app.ConnectorCompositionConfig{
			BundlePaths: []string{bundlePath},
		})
		if err != nil {
			return err
		}
		handlers := &app.HandlerRegistry{}
		if err := composition.RegisterDirectRead(handlers, app.HTTPDirectReadDependencies{}); err != nil {
			return err
		}
		catalog, err = app.ServeCatalog(app.CatalogServiceConfig{
			Pool:      dependencies.Pool,
			Authorize: configuration.Auth.AuthorizeResource,
			Registry:  composition.Registry(),
			Handlers:  handlers,
		})
		if err != nil {
			return err
		}
		configuration.Catalog = catalog
		admission, err := app.NewAdmissionService(app.AdmissionServiceConfig{
			Pool:             dependencies.Pool,
			DeploymentID:     dependencies.DeploymentID,
			Authorize:        configuration.Auth.AuthorizeResource,
			Registry:         handlers,
			ResolveOperation: composition.Resolver(),
			Policy:           httpDirectReadAdmissionPolicy(),
			Random:           rand.Reader,
		})
		if err != nil {
			return err
		}
		configuration.Admission = admission
		return nil
	})
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE pm.memberships
			SET permissions = ARRAY(
					SELECT DISTINCT value
					FROM unnest(permissions || ARRAY['catalog.read']) AS value
					ORDER BY value
				),
				resource_grants = ARRAY(
					SELECT DISTINCT value
					FROM unnest(resource_grants || ARRAY['permission:catalog.read']) AS value
					ORDER BY value
				),
				policy_epoch = policy_epoch + 1,
				control_revision = control_revision + 1
			WHERE org_id = $1 AND workspace_id = $2 AND identity_id = $3
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, fixture.Actor.RequestedActor.PrincipalID)
		return err
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	selection, err := app.ResolveBundledOperation(
		composition.Registry(), fixture.Connection.ConnectorID, connectorCompositionReadID,
	)
	if err != nil {
		t.Fatalf("resolve declared connector operation: %v", err)
	}
	revision, err := app.OperationSelectionRevision(selection)
	if err != nil {
		t.Fatalf("derive declared operation revision: %v", err)
	}
	page, err := catalog.ListConnectors(ctx, fixture.Actor, fixture.Scope, "", 10)
	if err != nil {
		t.Fatalf("list declared connector catalog: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ConnectorID != fixture.Connection.ConnectorID {
		t.Fatalf("declared connector catalog = %#v", page.Items)
	}
	reads := make(map[string]app.CatalogOperation, len(page.Items[0].Operations))
	for _, operation := range page.Items[0].Operations {
		reads[operation.OperationID] = operation
	}
	if reads[connectorCompositionReadID].OperationRevision != revision {
		t.Fatalf(
			"projected read operation revision = %q; want %q",
			reads[connectorCompositionReadID].OperationRevision, revision,
		)
	}
	readLane := catalogLane(t, reads[connectorCompositionReadID], contract.LaneDirectRead)
	if !readLane.Available || readLane.UnavailabilityCode != "" {
		t.Fatalf("declared direct_read lane = %#v; want available", readLane)
	}
	writeLane := catalogLane(t, reads[connectorCompositionWriteID], contract.LaneDirectWrite)
	if writeLane.Available || writeLane.UnavailabilityCode != "capability_unavailable" {
		t.Fatalf("unregistered direct_write lane = %#v; want capability_unavailable", writeLane)
	}
	commands, err := catalog.ListCommands(ctx, fixture.Actor, fixture.Scope, fixture.Connection.ConnectionID, "", "", 10)
	if err != nil {
		t.Fatalf("list declared connector commands: %v", err)
	}
	if len(commands.Items) != 1 || commands.Items[0].Name != connectorCompositionCommandName ||
		commands.Items[0].OperationID != connectorCompositionReadID ||
		len(commands.Items[0].AvailableLanes) != 1 ||
		commands.Items[0].AvailableLanes[0] != contract.LaneDirectRead ||
		commands.Items[0].UnavailabilityCode != "" {
		t.Fatalf("declared connector commands = %#v", commands.Items)
	}
	response := submitInitialInvocation(t, fixture, fixture.Client, fixture.Scope.WorkspaceID, api.InvocationRequest{
		ConnectionRef: fixture.Connection.ConnectionID,
		OperationRef:  connectorCompositionReadID,
		Lane:          api.InvocationRequestLane(contract.LaneDirectRead),
		Input: contract.NewObjectValue(map[string]contract.PresentValue{
			"query.workspace": contract.NewStringValue("connector-composition-workspace"),
		}),
		ResultControls: contract.ResultControls{},
		Protocol: api.ProtocolMetadata{
			WireVersion:      api.ProtocolMetadataWireVersion(api.WireVersion),
			RequiredFeatures: []string{},
			RequestKey:       requestKey,
			IntentDigest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			ExpectedRevision: revision,
		},
	})
	requireFreshInitialAdmission(t, response, requestKey)
}

func connectorCompositionBundle(t *testing.T) connectors.Bundle {
	t.Helper()
	read := catalogOperation(connectorCompositionReadID, contract.LaneDirectRead)
	read.CapabilityRequirements = httpDirectReadBaseCapabilities()
	write := catalogOperation(connectorCompositionWriteID, contract.LaneDirectWrite)
	write.CapabilityRequirements = httpDirectReadBaseCapabilities()
	bundle := connectors.Bundle{
		SchemaVersion:     connectors.BundleSchemaVersion,
		ConnectorID:       connectorCompositionConnectorID,
		SourceDigest:      "sha256:" + strings.Repeat("a", 64),
		SemanticsRevision: "sha256:" + strings.Repeat("b", 64),
		Operations:        []contract.Operation{read, write},
		Commands: []contract.Command{{
			Name: connectorCompositionCommandName, OperationID: read.OperationID,
			Fields: []contract.CommandField{{
				Flag: "--query-workspace", FieldID: "query.workspace",
				Location: contract.FieldLocationQuery, Required: true, Sensitive: false,
			}},
		}},
		UnavailableOperations: []contract.UnavailableOperation{},
	}
	if err := bundle.Validate(); err != nil {
		t.Fatalf("validate connector composition bundle: %v", err)
	}
	return bundle
}

func connectorCompositionDocument(t *testing.T, bundle connectors.Bundle) []byte {
	t.Helper()
	document, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("encode connector composition bundle: %v", err)
	}
	loaded, err := connectors.LoadBundle(bytes.NewReader(document))
	if err != nil {
		t.Fatalf("reload encoded connector composition bundle: %v", err)
	}
	if loaded.ConnectorID != bundle.ConnectorID {
		t.Fatalf("reloaded connector identity = %q; want %q", loaded.ConnectorID, bundle.ConnectorID)
	}
	return document
}

func connectorCompositionWriteBundle(t *testing.T, name string, document []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, document, 0o600); err != nil {
		t.Fatalf("write declared connector bundle: %v", err)
	}
	return path
}

func catalogLane(t *testing.T, operation app.CatalogOperation, lane contract.Lane) app.CatalogLane {
	t.Helper()
	for _, candidate := range operation.Lanes {
		if candidate.Lane == lane {
			return candidate
		}
	}
	t.Fatalf("projected operation %q has no %q lane: %#v", operation.OperationID, lane, operation.Lanes)
	return app.CatalogLane{}
}
