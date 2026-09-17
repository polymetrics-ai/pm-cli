package cli

import (
	"context"
	"encoding/json"
	"errors"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/spf13/cobra"
)

type mutationControl struct {
	requestKey singleString
	revision   singleString
}

func newConnectionsCommand(runtime *commandRuntime) *cobra.Command {
	command := &cobra.Command{Use: "connections", Short: "Manage safe connection metadata", Args: noArgs}
	command.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List safe connections in the selected workspace",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			response, err := bundle.client.API().ListConnectionsWithResponse(ctx, pmapi.WorkspaceID(bundle.config.workspace), nil)
			if err != nil {
				return requestError(ctx, err)
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	})
	command.AddCommand(&cobra.Command{
		Use:   "inspect CONNECTION_ID",
		Short: "Inspect one safe connection",
		Args:  exactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			response, err := bundle.client.API().GetConnectionWithResponse(ctx, pmapi.WorkspaceID(bundle.config.workspace), pmapi.ConnectionID(arguments[0]))
			if err != nil {
				return requestError(ctx, err)
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	})
	command.AddCommand(newConnectionCreateCommand(runtime))
	command.AddCommand(newConnectionUpdateCommand(runtime))
	command.AddCommand(newConnectionDeleteCommand(runtime))
	return command
}

func newConnectionCreateCommand(runtime *commandRuntime) *cobra.Command {
	control := newMutationControl()
	var connectorID, displayName, providerAccountID singleString
	connectorID.name = "connector-id"
	displayName.name = "display-name"
	providerAccountID.name = "provider-account-id"
	command := &cobra.Command{
		Use:   "create",
		Short: "Create a connection from typed safe metadata",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if !connectorID.set || !displayName.set || !providerAccountID.set {
				return usageFailure(errors.New("--connector-id, --display-name and --provider-account-id are required"))
			}
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			input := pmapi.SafeConnectionInput{ConnectorId: connectorID.value, DisplayName: displayName.value, ProviderAccountId: providerAccountID.value}
			protocol, params, err := control.protocol("connection.create", bundle.config.workspace, "", input)
			if err != nil {
				return err
			}
			request := pmapi.ConnectionCreateRequest{Connection: input, Protocol: protocol}
			parameters := &pmapi.CreateConnectionParams{IdempotencyKey: params.key, IfMatch: params.revision}
			response, err := sendRecoveredMutation(ctx, runtime, bundle, "connection.create", "", protocol, request, func(attemptContext context.Context) (pmapi.RecoveryAttemptResult, error) {
				value, sendErr := bundle.client.API().CreateConnectionWithResponse(attemptContext, pmapi.WorkspaceID(bundle.config.workspace), parameters, request)
				return recoveredResponse(value, sendErr)
			})
			if err != nil {
				return err
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	}
	addMutationFlags(command, control)
	command.Flags().Var(&connectorID, connectorID.name, "Connector identity")
	command.Flags().Var(&displayName, displayName.name, "Safe display name")
	command.Flags().Var(&providerAccountID, providerAccountID.name, "Authoritative provider-account identity, never a credential")
	return command
}

func newConnectionUpdateCommand(runtime *commandRuntime) *cobra.Command {
	control := newMutationControl()
	var displayName singleString
	displayName.name = "display-name"
	command := &cobra.Command{
		Use:   "update CONNECTION_ID",
		Short: "Update typed safe connection metadata",
		Args:  exactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			if !displayName.set {
				return usageFailure(errors.New("--display-name is required"))
			}
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			patch := pmapi.SafeConnectionPatch{DisplayName: displayName.value}
			protocol, params, err := control.protocol("connection.update", bundle.config.workspace, arguments[0], patch)
			if err != nil {
				return err
			}
			request := pmapi.ConnectionUpdateRequest{Connection: patch, Protocol: protocol}
			parameters := &pmapi.UpdateConnectionParams{IdempotencyKey: params.key, IfMatch: params.revision}
			response, err := sendRecoveredMutation(ctx, runtime, bundle, "connection.update", arguments[0], protocol, request, func(attemptContext context.Context) (pmapi.RecoveryAttemptResult, error) {
				value, sendErr := bundle.client.API().UpdateConnectionWithResponse(attemptContext, pmapi.WorkspaceID(bundle.config.workspace), pmapi.ConnectionID(arguments[0]), parameters, request)
				return recoveredResponse(value, sendErr)
			})
			if err != nil {
				return err
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	}
	addMutationFlags(command, control)
	command.Flags().Var(&displayName, displayName.name, "Safe display name")
	return command
}

func newConnectionDeleteCommand(runtime *commandRuntime) *cobra.Command {
	control := newMutationControl()
	command := &cobra.Command{
		Use:   "delete CONNECTION_ID",
		Short: "Revoke a connection under an exact revision",
		Args:  exactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			protocol, params, err := control.protocol("connection.delete", bundle.config.workspace, arguments[0], struct{}{})
			if err != nil {
				return err
			}
			request := pmapi.ConnectionDeleteRequest{Protocol: protocol}
			parameters := &pmapi.DeleteConnectionParams{IdempotencyKey: params.key, IfMatch: params.revision}
			response, err := sendRecoveredMutation(ctx, runtime, bundle, "connection.delete", arguments[0], protocol, request, func(attemptContext context.Context) (pmapi.RecoveryAttemptResult, error) {
				value, sendErr := bundle.client.API().DeleteConnectionWithResponse(attemptContext, pmapi.WorkspaceID(bundle.config.workspace), pmapi.ConnectionID(arguments[0]), parameters, request)
				return recoveredResponse(value, sendErr)
			})
			if err != nil {
				return err
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	}
	addMutationFlags(command, control)
	return command
}

func sendRecoveredMutation(
	ctx context.Context,
	runtime *commandRuntime,
	bundle clientBundle,
	operation string,
	target string,
	protocol pmapi.ProtocolMetadata,
	request any,
	attempt pmapi.RecoveryAttempt,
) (pmapi.WireResponse, error) {
	store, err := runtime.recoveryStore()
	if err != nil {
		return nil, err
	}
	return bundle.client.SendMutationWithRecovery(ctx, store, pmapi.WorkspaceID(bundle.config.workspace), operation, target, protocol, request, attempt)
}

type generatedRecoveryResponse interface {
	pmapi.WireResponse
}

func recoveredResponse[T generatedRecoveryResponse](response T, err error) (pmapi.RecoveryAttemptResult, error) {
	if err != nil {
		return pmapi.RecoveryAttemptResult{}, err
	}
	var retryAfter string
	switch value := any(response).(type) {
	case *pmapi.CreateConnectionResponse:
		if value == nil || value.HTTPResponse == nil {
			return pmapi.RecoveryAttemptResult{}, pmapi.ErrClientTransport
		}
		retryAfter = value.HTTPResponse.Header.Get("Retry-After")
	case *pmapi.UpdateConnectionResponse:
		if value == nil || value.HTTPResponse == nil {
			return pmapi.RecoveryAttemptResult{}, pmapi.ErrClientTransport
		}
		retryAfter = value.HTTPResponse.Header.Get("Retry-After")
	case *pmapi.DeleteConnectionResponse:
		if value == nil || value.HTTPResponse == nil {
			return pmapi.RecoveryAttemptResult{}, pmapi.ErrClientTransport
		}
		retryAfter = value.HTTPResponse.Header.Get("Retry-After")
	case *pmapi.GrantMembershipResponse:
		if value == nil || value.HTTPResponse == nil {
			return pmapi.RecoveryAttemptResult{}, pmapi.ErrClientTransport
		}
		retryAfter = value.HTTPResponse.Header.Get("Retry-After")
	case *pmapi.RevokeMembershipResponse:
		if value == nil || value.HTTPResponse == nil {
			return pmapi.RecoveryAttemptResult{}, pmapi.ErrClientTransport
		}
		retryAfter = value.HTTPResponse.Header.Get("Retry-After")
	case *pmapi.CreateServiceAccountResponse:
		if value == nil || value.HTTPResponse == nil {
			return pmapi.RecoveryAttemptResult{}, pmapi.ErrClientTransport
		}
		retryAfter = value.HTTPResponse.Header.Get("Retry-After")
	case *pmapi.RevokeServiceAccountResponse:
		if value == nil || value.HTTPResponse == nil {
			return pmapi.RecoveryAttemptResult{}, pmapi.ErrClientTransport
		}
		retryAfter = value.HTTPResponse.Header.Get("Retry-After")
	case *pmapi.CreateDelegationResponse:
		if value == nil || value.HTTPResponse == nil {
			return pmapi.RecoveryAttemptResult{}, pmapi.ErrClientTransport
		}
		retryAfter = value.HTTPResponse.Header.Get("Retry-After")
	case *pmapi.RevokeDelegationResponse:
		if value == nil || value.HTTPResponse == nil {
			return pmapi.RecoveryAttemptResult{}, pmapi.ErrClientTransport
		}
		retryAfter = value.HTTPResponse.Header.Get("Retry-After")
	default:
		return pmapi.RecoveryAttemptResult{}, pmapi.ErrClientTransport
	}
	return pmapi.RecoveryAttemptResult{Response: response, RetryAfter: retryAfter}, nil
}

func newMutationControl() *mutationControl {
	control := &mutationControl{}
	control.requestKey.name = "request-key"
	control.revision.name = "revision"
	return control
}

func addMutationFlags(command *cobra.Command, control *mutationControl) {
	command.Flags().Var(&control.requestKey, control.requestKey.name, "Caller-owned idempotency key")
	command.Flags().Var(&control.revision, control.revision.name, "Exact expected control revision")
}

type mutationParameters struct {
	key      pmapi.IdempotencyKey
	revision *pmapi.IfMatch
}

func (control *mutationControl) protocol(operation, workspace, target string, payload any) (pmapi.ProtocolMetadata, mutationParameters, error) {
	if control == nil || !control.requestKey.set || !control.revision.set {
		return pmapi.ProtocolMetadata{}, mutationParameters{}, usageFailure(errors.New("--request-key and --revision are required"))
	}
	canonical, err := json.Marshal(struct {
		ExpectedRevision string `json:"expected_revision"`
		Operation        string `json:"operation"`
		Payload          any    `json:"payload"`
		Target           string `json:"target"`
		Workspace        string `json:"workspace"`
	}{ExpectedRevision: control.revision.value, Operation: operation, Payload: payload, Target: target, Workspace: workspace})
	if err != nil {
		return pmapi.ProtocolMetadata{}, mutationParameters{}, err
	}
	digest, err := contract.CanonicalDigest(canonical)
	if err != nil {
		return pmapi.ProtocolMetadata{}, mutationParameters{}, err
	}
	revision := pmapi.IfMatch(control.revision.value)
	protocol := pmapi.ProtocolMetadata{
		ExpectedRevision: control.revision.value,
		IntentDigest:     string(digest),
		RequestKey:       control.requestKey.value,
		RequiredFeatures: []string{},
		WireVersion:      pmapi.ProtocolMetadataWireVersion(pmapi.WireVersion),
	}
	return protocol, mutationParameters{key: pmapi.IdempotencyKey(control.requestKey.value), revision: &revision}, nil
}
