package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/connectors"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

const catalogPermission = "catalog.read"

type CatalogServiceConfig struct {
	Pool      *store.Pool
	Authorize auth.AuthorizationCallback
	Registry  *connectors.Registry
	Handlers  *HandlerRegistry
}

// CatalogService projects immutable compiled bundles through current
// authorization and the actual in-process handler registry. It never calls an
// execution callback and has no provider client.
type CatalogService struct {
	pool      *store.Pool
	authorize auth.AuthorizationCallback
	registry  *connectors.Registry
	handlers  *HandlerRegistry
}

func ServeCatalog(configuration CatalogServiceConfig) (*CatalogService, error) {
	if configuration.Pool == nil || configuration.Authorize == nil || configuration.Registry == nil || configuration.Handlers == nil {
		return nil, errors.New("catalog service store, authorization, bundle registry, and handler registry are required")
	}
	if configuration.Registry.Revision().Validate() != nil {
		return nil, errors.New("catalog bundle registry revision is invalid")
	}
	return &CatalogService{
		pool:      configuration.Pool,
		authorize: configuration.Authorize,
		registry:  configuration.Registry,
		handlers:  configuration.Handlers,
	}, nil
}

type CatalogLane struct {
	Lane               contract.Lane
	SourceStatus       contract.LaneStatus
	SourceReason       string
	Available          bool
	UnavailabilityCode string
}

type CatalogOperation struct {
	OperationID       string
	OperationRevision string
	SourceRefs        []contract.SourceRef
	DiagnosticCodes   []string
	CommandName       *string
	Lanes             []CatalogLane
}

type CatalogConnector struct {
	ConnectorID       string
	SourceDigest      string
	SemanticsRevision string
	CatalogRevision   string
	Operations        []CatalogOperation
}

type CatalogConnectorPage struct {
	CatalogRevision string
	Items           []CatalogConnector
	Next            string
}

type CatalogCommandField struct {
	Flag    string
	Binding contract.FieldBinding
}

type CatalogCommand struct {
	Name               string
	Aliases            []string
	ConnectorID        string
	ConnectionID       string
	OperationID        string
	OperationRevision  string
	CatalogRevision    string
	Fields             []CatalogCommandField
	AvailableLanes     []contract.Lane
	UnavailabilityCode string
}

type CatalogCommandPage struct {
	CatalogRevision string
	Items           []CatalogCommand
	Next            string
}

func (service *CatalogService) ListConnectors(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	after string,
	pageSize uint32,
) (CatalogConnectorPage, error) {
	if service == nil || ctx == nil || pageSize == 0 || pageSize > 100 || (after != "" && !validApplicationID(after)) {
		return CatalogConnectorPage{}, invalidRequestFailure()
	}
	if err := service.authorizeWorkspaceCatalog(ctx, actor, scope); err != nil {
		return CatalogConnectorPage{}, err
	}
	revision, err := service.catalogRevision()
	if err != nil {
		return CatalogConnectorPage{}, err
	}
	catalog := service.registry.Catalog()
	page := CatalogConnectorPage{CatalogRevision: revision, Items: []CatalogConnector{}}
	for _, connector := range catalog.Connectors {
		if connector.ConnectorID <= after {
			continue
		}
		if len(page.Items) == int(pageSize) {
			page.Next = page.Items[len(page.Items)-1].ConnectorID
			break
		}
		item, projectionErr := service.projectConnector(connector, revision)
		if projectionErr != nil {
			return CatalogConnectorPage{}, projectionErr
		}
		page.Items = append(page.Items, item)
	}
	return page, nil
}

func (service *CatalogService) GetConnector(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	connectorID string,
) (CatalogConnector, error) {
	if service == nil || ctx == nil || !validApplicationID(connectorID) {
		return CatalogConnector{}, invalidRequestFailure()
	}
	if err := service.authorizeWorkspaceCatalog(ctx, actor, scope); err != nil {
		return CatalogConnector{}, err
	}
	connector, err := service.registry.Connector(connectorID)
	if err != nil {
		return CatalogConnector{}, resourceNotFoundFailure(err)
	}
	revision, err := service.catalogRevision()
	if err != nil {
		return CatalogConnector{}, err
	}
	return service.projectConnector(connector, revision)
}

func (service *CatalogService) ListCommands(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	connectionID string,
	filter string,
	after string,
	pageSize uint32,
) (CatalogCommandPage, error) {
	if service == nil || ctx == nil || !validApplicationID(connectionID) || len(filter) > 512 || strings.TrimSpace(filter) != filter ||
		pageSize == 0 || pageSize > 100 || (after != "" && !validApplicationOpaque(after, 1024)) {
		return CatalogCommandPage{}, invalidRequestFailure()
	}
	connection, authorization, err := service.authorizeConnectionCatalog(ctx, actor, scope, connectionID, "")
	if err != nil {
		return CatalogCommandPage{}, err
	}
	connector, err := service.registry.Connector(connection.ConnectorID)
	if err != nil {
		return CatalogCommandPage{}, resourceNotFoundFailure(err)
	}
	revision, err := service.catalogRevision()
	if err != nil {
		return CatalogCommandPage{}, err
	}
	allowed := make(map[string]struct{}, len(authorization.OperationIDs))
	for _, operationID := range authorization.OperationIDs {
		allowed[operationID] = struct{}{}
	}
	restrictOperations := len(allowed) != 0 ||
		actor.RequestedActor.Kind == contract.PrincipalServiceAccount ||
		actor.EffectiveActor.Kind == contract.PrincipalServiceAccount ||
		actor.Delegation != nil
	commands := make([]CatalogCommand, 0, len(connector.Operations))
	for _, operation := range connector.Operations {
		if operation.Command == nil || (restrictOperations && !containsOperation(allowed, operation.Contract.OperationID)) {
			continue
		}
		command, projectionErr := service.projectCommand(connector, connectionID, operation, revision)
		if projectionErr != nil {
			return CatalogCommandPage{}, projectionErr
		}
		if filter != "" && !strings.Contains(command.Name, filter) && !strings.Contains(command.OperationID, filter) {
			continue
		}
		commands = append(commands, command)
	}
	sort.Slice(commands, func(left, right int) bool {
		return commands[left].OperationID < commands[right].OperationID
	})
	page := CatalogCommandPage{CatalogRevision: revision, Items: []CatalogCommand{}}
	for _, command := range commands {
		if command.OperationID <= after {
			continue
		}
		if len(page.Items) == int(pageSize) {
			page.Next = page.Items[len(page.Items)-1].OperationID
			break
		}
		page.Items = append(page.Items, command)
	}
	return page, nil
}

func (service *CatalogService) GetCommand(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	connectionID string,
	operationID string,
) (CatalogCommand, error) {
	if service == nil || ctx == nil || !validApplicationID(connectionID) || !validApplicationOpaque(operationID, 1024) {
		return CatalogCommand{}, invalidRequestFailure()
	}
	connection, _, err := service.authorizeConnectionCatalog(ctx, actor, scope, connectionID, operationID)
	if err != nil {
		return CatalogCommand{}, err
	}
	connector, err := service.registry.Connector(connection.ConnectorID)
	if err != nil {
		return CatalogCommand{}, resourceNotFoundFailure(err)
	}
	for _, operation := range connector.Operations {
		if operation.Contract.OperationID != operationID {
			continue
		}
		if operation.Command == nil {
			return CatalogCommand{}, resourceNotFoundFailure(connectors.ErrOperationNotFound)
		}
		revision, revisionErr := service.catalogRevision()
		if revisionErr != nil {
			return CatalogCommand{}, revisionErr
		}
		command, projectionErr := service.projectCommand(connector, connectionID, operation, revision)
		if projectionErr != nil {
			return CatalogCommand{}, projectionErr
		}
		if len(command.AvailableLanes) == 0 {
			return CatalogCommand{}, capabilityUnavailableFailure(ErrHandlerUnavailable)
		}
		return command, nil
	}
	return CatalogCommand{}, resourceNotFoundFailure(connectors.ErrOperationNotFound)
}

func (service *CatalogService) authorizeWorkspaceCatalog(ctx context.Context, actor contract.Actor, scope contract.Scope) error {
	return store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		_, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:           scope,
			Actor:           actor,
			Permission:      catalogPermission,
			ResourceKind:    auth.ResourceWorkspace,
			ResourceID:      scope.WorkspaceID,
			AutomationScope: actor.RequestedActor.Kind == contract.PrincipalServiceAccount,
		})
		return mapAuthorizationFailure(err)
	})
}

func (service *CatalogService) authorizeConnectionCatalog(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	connectionID string,
	operationID string,
) (store.Connection, auth.Authorization, error) {
	var connection store.Connection
	var authorization auth.Authorization
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		if _, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:           scope,
			Actor:           actor,
			Permission:      catalogPermission,
			ResourceKind:    auth.ResourceWorkspace,
			ResourceID:      scope.WorkspaceID,
			AutomationScope: actor.RequestedActor.Kind == contract.PrincipalServiceAccount,
		}); err != nil {
			return mapAuthorizationFailure(err)
		}
		var err error
		authorization, err = service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:           scope,
			Actor:           actor,
			Permission:      "connection.read",
			ResourceKind:    auth.ResourceConnection,
			ResourceID:      connectionID,
			ConnectionID:    connectionID,
			OperationID:     operationID,
			AutomationScope: actor.RequestedActor.Kind == contract.PrincipalServiceAccount,
		})
		if err != nil {
			return mapAuthorizationFailure(err)
		}
		connection, err = store.LoadConnection(ctx, tx, connectionID)
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrScopeMismatch) {
			return resourceNotFoundFailure(err)
		}
		return err
	})
	return connection, authorization, err
}

func (service *CatalogService) catalogRevision() (string, error) {
	document, err := json.Marshal(struct {
		BundleCatalog string          `json:"bundle_catalog"`
		Handlers      HandlerManifest `json:"handlers"`
	}{BundleCatalog: string(service.registry.Revision()), Handlers: BuildHandlerManifest(service.handlers)})
	if err != nil {
		return "", fmt.Errorf("encode catalog capability revision: %w", err)
	}
	digest, err := contract.CanonicalDigest(document)
	if err != nil {
		return "", fmt.Errorf("compute catalog capability revision: %w", err)
	}
	return string(digest), nil
}

// ResolveBundledOperation is the single executable selection path shared by
// catalog projection and admission composition. Bundle semantics identity is
// intentionally separate from the operation-selection revision.
func ResolveBundledOperation(
	registry *connectors.Registry,
	connectorID string,
	operationID string,
) (OperationSelection, error) {
	if registry == nil {
		return OperationSelection{}, errors.New("connector registry is required")
	}
	operation, err := registry.Operation(connectorID, operationID)
	if err != nil {
		return OperationSelection{}, err
	}
	return normalizeOperationSelection(OperationSelection{
		Operation:  operation.Contract,
		SchemaRefs: []string{},
	})
}

func (service *CatalogService) operationRevision(connectorID string, operationID string) (string, error) {
	selection, err := ResolveBundledOperation(service.registry, connectorID, operationID)
	if err != nil {
		return "", capabilityUnavailableFailure(err)
	}
	revision, err := OperationSelectionRevision(selection)
	if err != nil {
		return "", capabilityUnavailableFailure(err)
	}
	return revision, nil
}

func (service *CatalogService) projectConnector(connector connectors.Connector, revision string) (CatalogConnector, error) {
	projected := CatalogConnector{
		ConnectorID:       connector.ConnectorID,
		SourceDigest:      string(connector.SourceDigest),
		SemanticsRevision: string(connector.SemanticsRevision),
		CatalogRevision:   revision,
		Operations:        make([]CatalogOperation, 0, len(connector.Operations)+len(connector.UnavailableOperations)),
	}
	for _, operation := range connector.Operations {
		operationRevision, err := service.operationRevision(connector.ConnectorID, operation.Contract.OperationID)
		if err != nil {
			return CatalogConnector{}, err
		}
		item := CatalogOperation{
			OperationID:       operation.Contract.OperationID,
			OperationRevision: operationRevision,
			SourceRefs:        append([]contract.SourceRef(nil), operation.Contract.SourceRefs...),
			DiagnosticCodes:   []string{},
			Lanes:             make([]CatalogLane, 0, len(operation.Contract.LaneEligibility)),
		}
		if operation.Command != nil {
			name := operation.Command.Name
			item.CommandName = &name
		}
		for _, eligibility := range operation.Contract.LaneEligibility {
			lane := CatalogLane{Lane: eligibility.Lane, SourceStatus: eligibility.Status, SourceReason: eligibility.Reason}
			if eligibility.Status != contract.LaneEligible {
				lane.UnavailabilityCode = "source_" + string(eligibility.Status)
			} else if _, err := service.handlers.Resolve(operation.Contract, eligibility.Lane); err != nil {
				lane.UnavailabilityCode = "capability_unavailable"
			} else {
				lane.Available = true
			}
			item.Lanes = append(item.Lanes, lane)
		}
		projected.Operations = append(projected.Operations, item)
	}
	for _, operation := range connector.UnavailableOperations {
		item := CatalogOperation{
			OperationID:       operation.OperationID,
			OperationRevision: string(connector.SemanticsRevision),
			SourceRefs:        append([]contract.SourceRef(nil), operation.SourceRefs...),
			DiagnosticCodes:   append([]string(nil), operation.DiagnosticCodes...),
			Lanes:             make([]CatalogLane, 0, len(operation.LaneEligibility)),
		}
		for _, eligibility := range operation.LaneEligibility {
			item.Lanes = append(item.Lanes, CatalogLane{
				Lane:               eligibility.Lane,
				SourceStatus:       eligibility.Status,
				SourceReason:       eligibility.Reason,
				UnavailabilityCode: operation.DiagnosticCodes[0],
			})
		}
		projected.Operations = append(projected.Operations, item)
	}
	sort.Slice(projected.Operations, func(left, right int) bool {
		return projected.Operations[left].OperationID < projected.Operations[right].OperationID
	})
	return projected, nil
}

func (service *CatalogService) projectCommand(
	connector connectors.Connector,
	connectionID string,
	operation connectors.Operation,
	revision string,
) (CatalogCommand, error) {
	operationRevision, err := service.operationRevision(connector.ConnectorID, operation.Contract.OperationID)
	if err != nil {
		return CatalogCommand{}, err
	}
	bindings := make(map[string]contract.FieldBinding, len(operation.Contract.Inputs))
	for _, binding := range operation.Contract.Inputs {
		bindings[binding.FieldID] = binding
	}
	for _, variant := range operation.Contract.RequestVariants {
		for _, binding := range variant.Inputs {
			bindings[binding.FieldID] = binding
		}
	}
	command := CatalogCommand{
		Name:              operation.Command.Name,
		Aliases:           []string{},
		ConnectorID:       connector.ConnectorID,
		ConnectionID:      connectionID,
		OperationID:       operation.Contract.OperationID,
		OperationRevision: operationRevision,
		CatalogRevision:   revision,
		Fields:            make([]CatalogCommandField, 0, len(operation.Command.Fields)),
		AvailableLanes:    []contract.Lane{},
	}
	for _, field := range operation.Command.Fields {
		command.Fields = append(command.Fields, CatalogCommandField{Flag: field.Flag, Binding: bindings[field.FieldID]})
	}
	for _, eligibility := range operation.Contract.LaneEligibility {
		if eligibility.Status != contract.LaneEligible {
			continue
		}
		if _, err := service.handlers.Resolve(operation.Contract, eligibility.Lane); err == nil {
			command.AvailableLanes = append(command.AvailableLanes, eligibility.Lane)
		}
	}
	if len(command.AvailableLanes) == 0 {
		command.UnavailabilityCode = "capability_unavailable"
	}
	return command, nil
}

func containsOperation(allowed map[string]struct{}, operationID string) bool {
	_, present := allowed[operationID]
	return present
}
