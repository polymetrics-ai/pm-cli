package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/oapi-codegen/nullable"
	"net/http"
	"strings"
	"sync"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const (
	maximumCatalogPages    = 100
	maximumCatalogCommands = 4096
)

var ErrCatalogEnvelope = errors.New("catalog response binding is invalid")

type CatalogBinding struct {
	ServerInstanceID string
	Principal        Principal
	WorkspaceID      WorkspaceID
	ConnectionID     string
	CatalogRevision  string
}

type CatalogSnapshot struct {
	Binding  CatalogBinding
	Commands []CommandDescriptor
}

type catalogCacheKey struct {
	serverInstanceID string
	principalKind    PrincipalKind
	principalID      string
	workspaceID      WorkspaceID
	connectionID     string
	catalogRevision  string
}

// CatalogCache keeps safe descriptor snapshots only under their complete
// server/principal/workspace/connection/revision binding. It never stores a
// credential, source document, or provider value.
type CatalogCache struct {
	mu      sync.RWMutex
	entries map[catalogCacheKey]CatalogSnapshot
}

func NewCatalogCache() *CatalogCache {
	return &CatalogCache{entries: make(map[catalogCacheKey]CatalogSnapshot)}
}

func (cache *CatalogCache) Store(snapshot CatalogSnapshot) error {
	if cache == nil || validateCatalogSnapshot(snapshot) != nil {
		return ErrCatalogEnvelope
	}
	copy, err := cloneCatalogSnapshot(snapshot)
	if err != nil {
		return ErrCatalogEnvelope
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.entries == nil {
		cache.entries = make(map[catalogCacheKey]CatalogSnapshot)
	}
	cache.entries[catalogKey(copy.Binding)] = copy
	return nil
}

func (cache *CatalogCache) Load(binding CatalogBinding) (CatalogSnapshot, bool) {
	if cache == nil || validateCatalogBinding(binding) != nil {
		return CatalogSnapshot{}, false
	}
	cache.mu.RLock()
	snapshot, present := cache.entries[catalogKey(binding)]
	cache.mu.RUnlock()
	if !present {
		return CatalogSnapshot{}, false
	}
	copy, err := cloneCatalogSnapshot(snapshot)
	return copy, err == nil
}

func LoadCatalog(
	ctx context.Context,
	client *QualifiedClient,
	cache *CatalogCache,
	workspaceID WorkspaceID,
	connectionID string,
	filter string,
) (CatalogSnapshot, error) {
	if ctx == nil || client == nil || client.generated == nil ||
		!client.principal.Kind.Valid() || !validIdentity(client.principal.PrincipalId, 512) ||
		!validIdentity(string(workspaceID), 512) || !validIdentity(connectionID, 512) ||
		len(filter) > 512 || strings.TrimSpace(filter) != filter {
		return CatalogSnapshot{}, ErrCatalogEnvelope
	}
	pageSize := 100
	params := &ListCatalogCommandsParams{ConnectionId: connectionID, PageSize: &pageSize}
	if filter != "" {
		params.Filter = &filter
	}
	var snapshot CatalogSnapshot
	seenCommands := make(map[string]struct{})
	seenOperations := make(map[string]struct{})
	for pageIndex := range maximumCatalogPages {
		response, err := client.generated.ListCatalogCommandsWithResponse(ctx, workspaceID, params)
		if err != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				return CatalogSnapshot{}, contextErr
			}
			return CatalogSnapshot{}, ErrClientTransport
		}
		if response.StatusCode() != http.StatusOK {
			envelope, decodeErr := DecodeEnvelope(response)
			if decodeErr != nil {
				return CatalogSnapshot{}, ErrCatalogEnvelope
			}
			if problem := EnvelopeError(envelope); problem != nil {
				return CatalogSnapshot{}, problem
			}
			return CatalogSnapshot{}, ErrCatalogEnvelope
		}
		if response.JSON200 == nil {
			return CatalogSnapshot{}, ErrCatalogEnvelope
		}
		page := *response.JSON200
		binding := CatalogBinding{
			ServerInstanceID: page.ServerInstanceId,
			Principal:        page.Principal,
			WorkspaceID:      WorkspaceID(page.WorkspaceId),
			ConnectionID:     page.ConnectionId,
			CatalogRevision:  page.CatalogRevision,
		}
		if err := validateCatalogBinding(binding); err != nil ||
			binding.ServerInstanceID != client.information.Value.InstanceId ||
			binding.Principal.Kind != client.principal.Kind ||
			binding.Principal.PrincipalId != client.principal.PrincipalId ||
			binding.WorkspaceID != workspaceID || binding.ConnectionID != connectionID {
			return CatalogSnapshot{}, ErrCatalogEnvelope
		}
		if pageIndex == 0 {
			snapshot = CatalogSnapshot{Binding: binding, Commands: []CommandDescriptor{}}
		} else if binding != snapshot.Binding {
			return CatalogSnapshot{}, ErrCatalogEnvelope
		}
		for _, command := range page.Items {
			if err := validateCommandDescriptor(command, binding); err != nil {
				return CatalogSnapshot{}, err
			}
			if _, duplicate := seenCommands[command.Name]; duplicate {
				return CatalogSnapshot{}, ErrCatalogEnvelope
			}
			if _, duplicate := seenOperations[command.OperationId]; duplicate {
				return CatalogSnapshot{}, ErrCatalogEnvelope
			}
			seenCommands[command.Name] = struct{}{}
			seenOperations[command.OperationId] = struct{}{}
			snapshot.Commands = append(snapshot.Commands, command)
			if len(snapshot.Commands) > maximumCatalogCommands {
				return CatalogSnapshot{}, ErrCatalogEnvelope
			}
		}
		cursor, more, err := catalogNextCursor(page.NextCursor)
		if err != nil {
			return CatalogSnapshot{}, err
		}
		if !more {
			if err := validateCatalogSnapshot(snapshot); err != nil {
				return CatalogSnapshot{}, err
			}
			if cache != nil {
				if err := cache.Store(snapshot); err != nil {
					return CatalogSnapshot{}, err
				}
			}
			return cloneCatalogSnapshot(snapshot)
		}
		params.Cursor = &cursor
	}
	return CatalogSnapshot{}, ErrCatalogEnvelope
}

func (snapshot CatalogSnapshot) ExecutableCommands() []CommandDescriptor {
	commands := make([]CommandDescriptor, 0, len(snapshot.Commands))
	for _, command := range snapshot.Commands {
		if len(command.AvailableLanes) == 0 || command.UnavailabilityCode != nil {
			continue
		}
		commands = append(commands, command)
	}
	return commands
}

func validateCatalogSnapshot(snapshot CatalogSnapshot) error {
	if err := validateCatalogBinding(snapshot.Binding); err != nil || snapshot.Commands == nil {
		return ErrCatalogEnvelope
	}
	seenNames := make(map[string]struct{}, len(snapshot.Commands))
	seenOperations := make(map[string]struct{}, len(snapshot.Commands))
	for _, command := range snapshot.Commands {
		if err := validateCommandDescriptor(command, snapshot.Binding); err != nil {
			return err
		}
		if _, duplicate := seenNames[command.Name]; duplicate {
			return ErrCatalogEnvelope
		}
		if _, duplicate := seenOperations[command.OperationId]; duplicate {
			return ErrCatalogEnvelope
		}
		seenNames[command.Name] = struct{}{}
		seenOperations[command.OperationId] = struct{}{}
	}
	return nil
}

func validateCatalogBinding(binding CatalogBinding) error {
	if !validIdentity(binding.ServerInstanceID, 512) || !validIdentity(string(binding.WorkspaceID), 512) ||
		!validIdentity(binding.ConnectionID, 512) || contract.SemanticDigest(binding.CatalogRevision).Validate() != nil ||
		!binding.Principal.Kind.Valid() || !validIdentity(binding.Principal.PrincipalId, 512) {
		return ErrCatalogEnvelope
	}
	return nil
}

func validateCommandDescriptor(command CommandDescriptor, binding CatalogBinding) error {
	if !validCommandSegment(command.Name) || !validIdentity(command.ConnectorId, 512) ||
		command.ConnectionId != binding.ConnectionID || !validIdentity(command.OperationId, 1024) ||
		contract.SemanticDigest(command.OperationRevision).Validate() != nil || command.CatalogRevision != binding.CatalogRevision ||
		command.Aliases == nil || command.Fields == nil || command.AvailableLanes == nil {
		return ErrCatalogEnvelope
	}
	seenNames := map[string]struct{}{command.Name: {}}
	for _, alias := range command.Aliases {
		if !validCommandSegment(alias) {
			return ErrCatalogEnvelope
		}
		if _, duplicate := seenNames[alias]; duplicate {
			return ErrCatalogEnvelope
		}
		seenNames[alias] = struct{}{}
	}
	seenFlags := make(map[string]struct{}, len(command.Fields))
	seenFields := make(map[string]struct{}, len(command.Fields))
	for _, field := range command.Fields {
		if !validLongFlag(field.Flag) || field.Binding.Validate() != nil || field.Binding.Sensitivity != contract.SensitivityPublic ||
			(field.Binding.Location == contract.FieldLocationBody && field.Binding.JSONPointer == "") {
			return ErrCatalogEnvelope
		}
		if _, duplicate := seenFlags[field.Flag]; duplicate {
			return ErrCatalogEnvelope
		}
		if _, duplicate := seenFields[field.Binding.FieldID]; duplicate {
			return ErrCatalogEnvelope
		}
		seenFlags[field.Flag] = struct{}{}
		seenFields[field.Binding.FieldID] = struct{}{}
	}
	seenLanes := make(map[CommandDescriptorAvailableLanes]struct{}, len(command.AvailableLanes))
	for _, lane := range command.AvailableLanes {
		if contract.ValidateLane(contract.Lane(lane)) != nil {
			return ErrCatalogEnvelope
		}
		if _, duplicate := seenLanes[lane]; duplicate {
			return ErrCatalogEnvelope
		}
		seenLanes[lane] = struct{}{}
	}
	if len(command.AvailableLanes) == 0 && command.UnavailabilityCode == nil {
		return ErrCatalogEnvelope
	}
	if len(command.AvailableLanes) != 0 && command.UnavailabilityCode != nil {
		return ErrCatalogEnvelope
	}
	return nil
}

func catalogNextCursor(value nullable.Nullable[string]) (string, bool, error) {
	if !value.IsSpecified() {
		return "", false, ErrCatalogEnvelope
	}
	if value.IsNull() {
		return "", false, nil
	}
	cursor, err := value.Get()
	if err != nil || cursor == "" || len(cursor) > 4096 {
		return "", false, ErrCatalogEnvelope
	}
	return cursor, true, nil
}

func catalogKey(binding CatalogBinding) catalogCacheKey {
	return catalogCacheKey{
		serverInstanceID: binding.ServerInstanceID,
		principalKind:    binding.Principal.Kind,
		principalID:      binding.Principal.PrincipalId,
		workspaceID:      binding.WorkspaceID,
		connectionID:     binding.ConnectionID,
		catalogRevision:  binding.CatalogRevision,
	}
}

func cloneCatalogSnapshot(snapshot CatalogSnapshot) (CatalogSnapshot, error) {
	document, err := json.Marshal(snapshot)
	if err != nil {
		return CatalogSnapshot{}, err
	}
	var copy CatalogSnapshot
	if err := json.Unmarshal(document, &copy); err != nil {
		return CatalogSnapshot{}, err
	}
	return copy, nil
}

func validCommandSegment(value string) bool {
	if !validIdentity(value, 512) || strings.ContainsAny(value, " /\\") || strings.HasPrefix(value, "-") {
		return false
	}
	return true
}

func validLongFlag(value string) bool {
	if !strings.HasPrefix(value, "--") || len(value) < 3 || strings.ContainsAny(value, " =\t\r\n") {
		return false
	}
	for _, character := range strings.TrimPrefix(value, "--") {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}
