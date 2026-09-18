package app_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/connectors"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

func TestRegisteredCatalogBoundary(t *testing.T) {
	t.Parallel()
	const (
		operationID       = "op-1"
		missingHandlerID  = "op-2"
		semanticsRevision = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	bundle := catalogBundle(t, semanticsRevision)
	registry, err := connectors.NewRegistry(bundle)
	if err != nil {
		t.Fatalf("construct immutable bundle registry: %v", err)
	}
	selection, err := app.ResolveBundledOperation(registry, "fixture-connector", operationID)
	if err != nil {
		t.Fatalf("resolve bundled operation: %v", err)
	}
	operationRevision, err := app.OperationSelectionRevision(selection)
	if err != nil {
		t.Fatalf("derive operation selection revision: %v", err)
	}
	var downstreamCalls atomic.Int32
	handlers := &app.HandlerRegistry{}
	foundation := contract.FoundationContract{ID: "FND-6", Version: "1.0.0", Dependencies: []contract.FoundationRef{}}
	capability := contract.CapabilityRequirement{Capability: "http.execute", Foundation: contract.FoundationRef{ID: foundation.ID, Version: foundation.Version}}
	if err := handlers.RegisterHTTP(app.HandlerRegistration{
		Lanes:        []contract.Lane{contract.LaneDirectRead},
		Foundations:  []contract.FoundationContract{foundation},
		Capabilities: []contract.CapabilityRequirement{capability},
		Execute: func(context.Context, app.ExecutionRequest) (app.ExecutionObservation, error) {
			downstreamCalls.Add(1)
			return app.ExecutionObservation{}, nil
		},
	}); err != nil {
		t.Fatalf("register controlled read handler: %v", err)
	}
	configure := func(dependencies testfixture.ServiceDependencies, configuration *httpapi.ServiceConfig) error {
		catalog, err := app.ServeCatalog(app.CatalogServiceConfig{
			Pool:      dependencies.Pool,
			Authorize: configuration.Auth.AuthorizeResource,
			Registry:  registry,
			Handlers:  handlers,
		})
		if err != nil {
			return err
		}
		configuration.Catalog = catalog
		return nil
	}
	fixture := testfixture.NewOperation(t, operationID, configure)
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
					FROM unnest(resource_grants || ARRAY['permission:catalog.read', 'operation:op-2']) AS value
					ORDER BY value
				),
				operation_ids = ARRAY(
					SELECT DISTINCT value
					FROM unnest(operation_ids || ARRAY['op-2']) AS value
					ORDER BY value
				),
				policy_epoch = policy_epoch + 1,
				control_revision = control_revision + 1
			WHERE org_id = $1 AND workspace_id = $2 AND identity_id = $3
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, fixture.Actor.RequestedActor.PrincipalID)
		return err
	})

	pageDocument, status := catalogGET(t, fixture.Client, fixture.BaseURL+"/v1/workspaces/"+url.PathEscape(fixture.Scope.WorkspaceID)+"/commands?connection_id="+url.QueryEscape(fixture.Connection.ConnectionID))
	if status != http.StatusOK {
		t.Fatalf("authorized command catalog status = %d, body = %s", status, pageDocument)
	}
	var page api.SafeCommandPage
	if err := json.Unmarshal(pageDocument, &page); err != nil {
		t.Fatalf("decode safe command page: %v", err)
	}
	var foundAvailable, foundRequestVariants bool
	for _, command := range page.Items {
		if command.OperationId != operationID {
			continue
		}
		foundAvailable = command.OperationRevision == operationRevision &&
			slices.Contains(command.AvailableLanes, api.CommandDescriptorAvailableLanes(contract.LaneDirectRead))
		foundRequestVariants = command.RequestBody.RequestMediaFieldID == "request-content-type" &&
			len(command.RequestBody.Variants) == 2 &&
			command.RequestBody.Variants[0].VariantID == "json" &&
			command.RequestBody.Variants[0].MediaType == "application/json" &&
			slices.Equal(command.RequestBody.Variants[0].FieldIDs, []string{"json-name"}) &&
			command.RequestBody.Variants[0].Schema.Type == contract.SchemaTypeObject &&
			slices.Equal(command.RequestBody.Variants[0].Schema.Required, []string{"name"}) &&
			command.RequestBody.Variants[0].Schema.Properties["name"].Type == contract.SchemaTypeString &&
			command.RequestBody.Variants[1].VariantID == "merge" &&
			command.RequestBody.Variants[1].MediaType == "application/merge-patch+json" &&
			slices.Equal(command.RequestBody.Variants[1].FieldIDs, []string{"merge-name"}) &&
			command.RequestBody.Variants[1].Schema.Type == contract.SchemaTypeObject &&
			slices.Equal(command.RequestBody.Variants[1].Schema.Required, []string{"name"}) &&
			command.RequestBody.Variants[1].Schema.Properties["name"].Type == contract.SchemaTypeString &&
			!command.RequestBody.Variants[0].Required && !command.RequestBody.Variants[1].Required
	}
	if !foundAvailable || !foundRequestVariants || page.CatalogRevision == "" || page.ConnectionId != fixture.Connection.ConnectionID {
		t.Fatalf("authorized catalog did not retain op-1/revision/request variants/binding: %#v", page)
	}

	connectorDocument, status := catalogGET(t, fixture.Client, fixture.BaseURL+"/v1/workspaces/"+url.PathEscape(fixture.Scope.WorkspaceID)+"/connectors/"+url.PathEscape(fixture.Connection.ConnectorID))
	if status != http.StatusOK {
		t.Fatalf("authorized connector catalog status = %d, body = %s", status, connectorDocument)
	}
	var connector api.CatalogConnector
	if err := json.Unmarshal(connectorDocument, &connector); err != nil {
		t.Fatalf("decode connector descriptor: %v", err)
	}
	foundConnectorOperation := false
	for _, operation := range connector.Operations {
		if operation.OperationId == operationID {
			foundConnectorOperation = true
			if len(operation.Lanes) != 7 {
				t.Fatalf("canonical lane count = %d, want 7", len(operation.Lanes))
			}
		}
	}
	if !foundConnectorOperation {
		t.Fatalf("connector catalog omitted %q: %#v", operationID, connector)
	}

	foreignDocument, status := catalogGET(t, fixture.Client, fixture.BaseURL+"/v1/workspaces/"+url.PathEscape(fixture.Scope.WorkspaceID)+"/commands?connection_id="+url.QueryEscape(fixture.ForeignConnection.ConnectionID))
	if status != http.StatusNotFound {
		t.Fatalf("foreign connection catalog status = %d, want 404", status)
	}
	if strings.Contains(string(foreignDocument), operationID) || strings.Contains(string(foreignDocument), semanticsRevision) {
		t.Fatalf("foreign refusal disclosed a descriptor: %s", foreignDocument)
	}

	missingDocument, status := catalogGET(t, fixture.Client, fixture.BaseURL+"/v1/workspaces/"+url.PathEscape(fixture.Scope.WorkspaceID)+"/commands/"+url.PathEscape(missingHandlerID)+"?connection_id="+url.QueryEscape(fixture.Connection.ConnectionID))
	if status != http.StatusServiceUnavailable {
		t.Fatalf("missing-handler command status = %d, want 503; body = %s", status, missingDocument)
	}
	var problem api.Problem
	if err := json.Unmarshal(missingDocument, &problem); err != nil || problem.Code != "capability_unavailable" {
		t.Fatalf("missing handler problem = %#v, decode error = %v", problem, err)
	}
	if calls := downstreamCalls.Load(); calls != 0 {
		t.Fatalf("catalog activity executed downstream callback %d times", calls)
	}
}

func catalogGET(t *testing.T, client *http.Client, endpoint string) ([]byte, int) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatalf("construct catalog request: %v", err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("send catalog request: %v", err)
	}
	defer response.Body.Close()
	document, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read catalog response: %v", err)
	}
	return document, response.StatusCode
}

func catalogBundle(t *testing.T, revision string) connectors.Bundle {
	t.Helper()
	read := catalogOperationWithRequestVariants("op-1", contract.LaneDirectRead)
	write := catalogOperation("op-2", contract.LaneDirectWrite)
	bundle := connectors.Bundle{
		SchemaVersion:     connectors.BundleSchemaVersion,
		ConnectorID:       "fixture-connector",
		SourceDigest:      "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SemanticsRevision: revision,
		Operations:        []contract.Operation{read, write},
		Commands: []contract.Command{
			{
				Name: "inspect-widget", OperationID: read.OperationID,
				Fields: []contract.CommandField{
					{Flag: "--query-workspace", FieldID: "query.workspace", Location: contract.FieldLocationQuery, Required: true, Sensitive: false},
					{Flag: "--request-content-type", FieldID: "request-content-type", Location: contract.FieldLocationHeader, Required: false, Sensitive: false},
					{Flag: "--json-name", FieldID: "json-name", Location: contract.FieldLocationBody, Required: true, Sensitive: false},
					{Flag: "--merge-name", FieldID: "merge-name", Location: contract.FieldLocationBody, Required: true, Sensitive: false},
				},
			},
			{Name: "write-widget", OperationID: write.OperationID, Fields: []contract.CommandField{{Flag: "--query-workspace", FieldID: "query.workspace", Location: contract.FieldLocationQuery, Required: true, Sensitive: false}}},
		},
		UnavailableOperations: []contract.UnavailableOperation{},
	}
	if err := bundle.Validate(); err != nil {
		t.Fatalf("validate catalog fixture bundle: %v", err)
	}
	return bundle
}

func catalogOperation(operationID string, eligible contract.Lane) contract.Operation {
	foundation := contract.FoundationRef{ID: "FND-6", Version: "1.0.0"}
	sourceRefs := []contract.SourceRef{{SourceID: "fixture-source", Pointer: "#/operations/" + operationID}}
	return contract.Operation{
		SchemaVersion: contract.OperationSchemaVersion,
		OperationID:   operationID,
		SourceRefs:    sourceRefs,
		Transport:     contract.Transport{Kind: contract.TransportKindHTTP, Method: http.MethodGet, Path: "/widgets"},
		Inputs: []contract.FieldBinding{{
			FieldID: "query.workspace", WireName: "workspace", Location: contract.FieldLocationQuery,
			JSONPointer: "", Required: true, Nullable: false, Style: "form", Explode: true,
			AllowReserved: false, Sensitivity: contract.SensitivityPublic, Schema: contract.Schema{Type: contract.SchemaTypeString},
		}},
		RequestVariants: []contract.RequestVariant{},
		Responses: []contract.Response{{
			Status: "200", NoContent: false, MediaType: "application/json",
			Schema: &contract.Schema{Type: contract.SchemaTypeString}, SourceRefs: sourceRefs,
		}},
		CapabilityRequirements: []contract.CapabilityRequirement{{Capability: "http.execute", Foundation: foundation}},
		LaneEligibility:        catalogLanes(eligible),
	}
}

func catalogOperationWithRequestVariants(operationID string, eligible contract.Lane) contract.Operation {
	operation := catalogOperation(operationID, eligible)
	operation.Transport.Method = http.MethodPost
	selector := contract.FieldBinding{
		FieldID: "request-content-type", WireName: contract.RequestMediaTypeWireName, Location: contract.FieldLocationHeader,
		JSONPointer: "", Required: false, Nullable: false, Style: "simple", Explode: false,
		AllowReserved: false, Sensitivity: contract.SensitivityPublic,
		Schema: contract.Schema{
			Type: contract.SchemaTypeString,
			Enum: []contract.PresentValue{
				contract.NewStringValue("application/json"),
				contract.NewStringValue("application/merge-patch+json"),
			},
		},
	}
	root := contract.Schema{
		Type:       contract.SchemaTypeObject,
		Required:   []string{"name"},
		Properties: map[string]contract.Schema{"name": {Type: contract.SchemaTypeString}},
	}
	jsonName := catalogBodyBinding("json-name", "name", root.Properties["name"])
	mergeName := catalogBodyBinding("merge-name", "name", root.Properties["name"])
	operation.Inputs = append(operation.Inputs, selector)
	operation.RequestMediaFieldID = selector.FieldID
	operation.RequestVariants = []contract.RequestVariant{
		{
			VariantID: "json", MediaType: "application/json", Required: false, Schema: root,
			Inputs:     []contract.FieldBinding{jsonName},
			SourceRefs: []contract.SourceRef{{SourceID: "fixture-source", Pointer: "#/requestBody/content/application~1json"}},
		},
		{
			VariantID: "merge", MediaType: "application/merge-patch+json", Required: false, Schema: root,
			Inputs:     []contract.FieldBinding{mergeName},
			SourceRefs: []contract.SourceRef{{SourceID: "fixture-source", Pointer: "#/requestBody/content/application~1merge-patch+json"}},
		},
	}
	return operation
}

func catalogBodyBinding(fieldID, wireName string, schema contract.Schema) contract.FieldBinding {
	return contract.FieldBinding{
		FieldID: fieldID, WireName: wireName, Location: contract.FieldLocationBody, JSONPointer: "/" + wireName,
		Required: true, Nullable: false, Style: "json", Explode: false, AllowReserved: false,
		Sensitivity: contract.SensitivityPublic, Schema: schema,
	}
}

func catalogLanes(eligible contract.Lane) []contract.LaneEligibility {
	lanes := []contract.Lane{
		contract.LaneDirectRead,
		contract.LaneDirectWrite,
		contract.LaneETL,
		contract.LaneReverseETL,
		contract.LaneBinaryDownload,
		contract.LaneBinaryUpload,
		contract.LaneSyncTransport,
	}
	result := make([]contract.LaneEligibility, 0, len(lanes))
	for _, lane := range lanes {
		status := contract.LaneUnsupported
		reason := "fixture-lane-unsupported"
		if lane == eligible {
			status = contract.LaneEligible
			reason = "fixture-lane-eligible"
		}
		result = append(result, contract.LaneEligibility{Lane: lane, Status: status, Reason: reason})
	}
	return result
}
