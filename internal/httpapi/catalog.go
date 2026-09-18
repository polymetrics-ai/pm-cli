package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
)

func (service *Service) ListCatalogConnectors(ctx context.Context, request ListCatalogConnectorsRequestObject) (ListCatalogConnectorsResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if service.catalog == nil {
		return asProblemResponse(capabilityUnavailable(), nil), nil
	}
	after, pageSize, err := service.parsePage("catalog_connectors", state.identity.Principal, scope, request.Params.Cursor, request.Params.PageSize)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	page, err := service.catalog.ListConnectors(ctx, state.identity.Actor, scope, after, pageSize)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	next, err := service.nextCursor("catalog_connectors", state.identity.Principal, scope, pageSize, page.Next)
	if err != nil {
		return nil, err
	}
	items := make([]api.CatalogConnector, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, catalogConnectorResponse(item))
	}
	return ListCatalogConnectors200JSONResponse(api.CatalogConnectorPage{
		CatalogRevision: page.CatalogRevision,
		Items:           items,
		NextCursor:      next,
	}), nil
}

func (service *Service) GetCatalogConnector(ctx context.Context, request GetCatalogConnectorRequestObject) (GetCatalogConnectorResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if service.catalog == nil {
		return asProblemResponse(capabilityUnavailable(), nil), nil
	}
	connector, err := service.catalog.GetConnector(ctx, state.identity.Actor, scope, request.ConnectorId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	return GetCatalogConnector200JSONResponse(catalogConnectorResponse(connector)), nil
}

func (service *Service) ListCatalogCommands(ctx context.Context, request ListCatalogCommandsRequestObject) (ListCatalogCommandsResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if service.catalog == nil {
		return asProblemResponse(capabilityUnavailable(), nil), nil
	}
	filter := ""
	if request.Params.Filter != nil {
		filter = *request.Params.Filter
	}
	cursorBinding := sha256.Sum256([]byte(request.Params.ConnectionId + "\x00" + filter))
	operation := "catalog_commands:" + hex.EncodeToString(cursorBinding[:])
	after, pageSize, err := service.parsePage(operation, state.identity.Principal, scope, request.Params.Cursor, request.Params.PageSize)
	if err != nil {
		return invalidRequestResponse(), nil
	}
	page, err := service.catalog.ListCommands(ctx, state.identity.Actor, scope, request.Params.ConnectionId, filter, after, pageSize)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	next, err := service.nextCursor(operation, state.identity.Principal, scope, pageSize, page.Next)
	if err != nil {
		return nil, err
	}
	items := make([]api.CommandDescriptor, 0, len(page.Items))
	for _, item := range page.Items {
		items = append(items, catalogCommandResponse(item))
	}
	return ListCatalogCommands200JSONResponse(api.SafeCommandPage{
		ServerInstanceId: service.instanceID,
		Principal:        principalResponse(state.identity.Principal),
		WorkspaceId:      scope.WorkspaceID,
		ConnectionId:     request.Params.ConnectionId,
		CatalogRevision:  page.CatalogRevision,
		Items:            items,
		NextCursor:       next,
	}), nil
}

func (service *Service) GetCatalogCommand(ctx context.Context, request GetCatalogCommandRequestObject) (GetCatalogCommandResponseObject, error) {
	state, scope, err := scopedState(ctx, request.WorkspaceId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	if service.catalog == nil {
		return asProblemResponse(capabilityUnavailable(), nil), nil
	}
	command, err := service.catalog.GetCommand(ctx, state.identity.Actor, scope, request.Params.ConnectionId, request.OperationId)
	if err != nil {
		return asProblemResponse(err, nil), nil
	}
	return GetCatalogCommand200JSONResponse(catalogCommandResponse(command)), nil
}

func catalogConnectorResponse(connector app.CatalogConnector) api.CatalogConnector {
	operations := make([]api.CatalogOperation, 0, len(connector.Operations))
	for _, operation := range connector.Operations {
		lanes := make([]api.CatalogLaneAvailability, 0, len(operation.Lanes))
		for _, lane := range operation.Lanes {
			projected := api.CatalogLaneAvailability{
				Available:    lane.Available,
				Lane:         api.CatalogLaneAvailabilityLane(lane.Lane),
				SourceReason: lane.SourceReason,
				SourceStatus: api.CatalogLaneAvailabilitySourceStatus(lane.SourceStatus),
			}
			if lane.UnavailabilityCode != "" {
				code := lane.UnavailabilityCode
				projected.UnavailabilityCode = &code
			}
			lanes = append(lanes, projected)
		}
		operations = append(operations, api.CatalogOperation{
			CommandName:       operation.CommandName,
			DiagnosticCodes:   append([]string(nil), operation.DiagnosticCodes...),
			Lanes:             lanes,
			OperationId:       operation.OperationID,
			OperationRevision: operation.OperationRevision,
		})
	}
	return api.CatalogConnector{
		CatalogRevision:   connector.CatalogRevision,
		ConnectorId:       connector.ConnectorID,
		Operations:        operations,
		SemanticsRevision: connector.SemanticsRevision,
		SourceDigest:      connector.SourceDigest,
	}
}

func catalogCommandResponse(command app.CatalogCommand) api.CommandDescriptor {
	fields := make([]api.CommandFieldDescriptor, 0, len(command.Fields))
	for _, field := range command.Fields {
		fields = append(fields, api.CommandFieldDescriptor{Binding: field.Binding, Flag: field.Flag})
	}
	lanes := make([]api.CommandDescriptorAvailableLanes, 0, len(command.AvailableLanes))
	for _, lane := range command.AvailableLanes {
		lanes = append(lanes, api.CommandDescriptorAvailableLanes(lane))
	}
	projected := api.CommandDescriptor{
		Aliases:           append([]string{}, command.Aliases...),
		AvailableLanes:    lanes,
		CatalogRevision:   command.CatalogRevision,
		ConnectionId:      command.ConnectionID,
		ConnectorId:       command.ConnectorID,
		Fields:            fields,
		Name:              command.Name,
		OperationId:       command.OperationID,
		OperationRevision: command.OperationRevision,
	}
	if command.UnavailabilityCode != "" {
		code := command.UnavailabilityCode
		projected.UnavailabilityCode = &code
	}
	return projected
}

func capabilityUnavailable() error {
	return &app.Failure{HTTPStatus: http.StatusServiceUnavailable, Code: "capability_unavailable", RetryAction: "after_capability_change"}
}

func (r problemResponse) VisitListCatalogConnectorsResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r problemResponse) VisitGetCatalogConnectorResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r problemResponse) VisitListCatalogCommandsResponse(w http.ResponseWriter) error {
	return r.write(w)
}
func (r problemResponse) VisitGetCatalogCommandResponse(w http.ResponseWriter) error {
	return r.write(w)
}
