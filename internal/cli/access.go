package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oapi-codegen/nullable"
	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/spf13/cobra"
)

type singleList struct {
	values []string
	set    bool
	name   string
}

func (value *singleList) Set(raw string) error {
	if value.set || raw == "" || len(raw) > maximumInputLength {
		return fmt.Errorf("%w: --%s requires one non-empty comma-separated value", errUsage, value.name)
	}
	seen := make(map[string]struct{})
	for _, item := range strings.Split(raw, ",") {
		if item == "" || strings.TrimSpace(item) != item {
			return fmt.Errorf("%w: --%s contains an empty or non-canonical member", errUsage, value.name)
		}
		if _, duplicate := seen[item]; duplicate {
			return fmt.Errorf("%w: --%s contains a duplicate member", errUsage, value.name)
		}
		seen[item] = struct{}{}
		value.values = append(value.values, item)
	}
	value.set = true
	return nil
}
func (value *singleList) String() string { return strings.Join(value.values, ",") }
func (*singleList) Type() string         { return "csv" }

func NewAccessCommands(runtime *commandRuntime) *cobra.Command {
	command := &cobra.Command{Use: "access", Short: "Manage revision-safe PM access grants", Args: noArgs}
	command.AddCommand(newMembershipCommands(runtime))
	command.AddCommand(newServiceAccountCommands(runtime))
	command.AddCommand(newDelegationCommands(runtime))
	return command
}

func newMembershipCommands(runtime *commandRuntime) *cobra.Command {
	command := &cobra.Command{Use: "memberships", Short: "Manage workspace memberships", Args: noArgs}
	command.AddCommand(accessListCommand(runtime, "list", func(ctx commandContext) (pmapi.WireResponse, error) {
		return ctx.bundle.client.API().ListMembershipsWithResponse(ctx.context, pmapi.WorkspaceID(ctx.bundle.config.workspace), nil)
	}))
	command.AddCommand(newMembershipGrantCommand(runtime))
	command.AddCommand(newMembershipRevokeCommand(runtime))
	return command
}

func newMembershipGrantCommand(runtime *commandRuntime) *cobra.Command {
	control := newMutationControl()
	var principal, role, expires singleString
	var grants singleList
	principal.name, role.name, expires.name, grants.name = "principal-ref", "role", "expires-at", "resource-grants"
	command := &cobra.Command{Use: "grant", Short: "Grant a typed workspace membership", Args: noArgs}
	command.RunE = func(command *cobra.Command, _ []string) error {
		if !principal.set || !role.set || !grants.set {
			return usageFailure(errors.New("--principal-ref, --role and --resource-grants are required"))
		}
		expiration, err := optionalExpiration(expires)
		if err != nil {
			return usageFailure(err)
		}
		ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
		if err != nil {
			return err
		}
		defer cancel()
		var request pmapi.MembershipGrantRequest
		request.Membership.PrincipalRef = principal.value
		request.Membership.Role = role.value
		request.Membership.ResourceGrants = append([]string(nil), grants.values...)
		request.Membership.ExpiresAt = expiration
		protocol, parameters, err := control.protocol("membership.grant", bundle.config.workspace, principal.value, request.Membership)
		if err != nil {
			return err
		}
		request.Protocol = protocol
		mutationParameters := &pmapi.GrantMembershipParams{IdempotencyKey: parameters.key, IfMatch: parameters.revision}
		response, err := sendRecoveredMutation(ctx, runtime, bundle, "membership.grant", principal.value, protocol, request, func(attemptContext context.Context) (pmapi.RecoveryAttemptResult, error) {
			value, sendErr := bundle.client.API().GrantMembershipWithResponse(attemptContext, pmapi.WorkspaceID(bundle.config.workspace), mutationParameters, request)
			return recoveredResponse(value, sendErr)
		})
		if err != nil {
			return err
		}
		return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
	}
	addMutationFlags(command, control)
	command.Flags().Var(&principal, principal.name, "Principal identity")
	command.Flags().Var(&role, role.name, "Membership role")
	command.Flags().Var(&grants, grants.name, "Comma-separated resource grants")
	command.Flags().Var(&expires, expires.name, "Optional canonical RFC3339 expiry")
	return command
}

func newMembershipRevokeCommand(runtime *commandRuntime) *cobra.Command {
	control := newMutationControl()
	var scopeDigest, expires singleString
	scopeDigest.name, expires.name = "bound-scope-digest", "expires-at"
	command := &cobra.Command{Use: "revoke MEMBERSHIP_REF", Short: "Revoke an exact workspace membership grant", Args: exactArgs(1)}
	command.RunE = func(command *cobra.Command, arguments []string) error {
		if !scopeDigest.set {
			return usageFailure(errors.New("--bound-scope-digest is required"))
		}
		expiration, err := optionalExpiration(expires)
		if err != nil {
			return usageFailure(err)
		}
		ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
		if err != nil {
			return err
		}
		defer cancel()
		var request pmapi.MembershipRevokeRequest
		request.Membership.MembershipRef = arguments[0]
		request.Membership.BoundScopeDigest = scopeDigest.value
		request.Membership.ExpiresAt = expiration
		protocol, parameters, err := control.protocol("membership.revoke", bundle.config.workspace, arguments[0], request.Membership)
		if err != nil {
			return err
		}
		request.Protocol = protocol
		mutationParameters := &pmapi.RevokeMembershipParams{IdempotencyKey: parameters.key, IfMatch: parameters.revision}
		response, err := sendRecoveredMutation(ctx, runtime, bundle, "membership.revoke", arguments[0], protocol, request, func(attemptContext context.Context) (pmapi.RecoveryAttemptResult, error) {
			value, sendErr := bundle.client.API().RevokeMembershipWithResponse(attemptContext, pmapi.WorkspaceID(bundle.config.workspace), pmapi.ResourceID(arguments[0]), mutationParameters, request)
			return recoveredResponse(value, sendErr)
		})
		if err != nil {
			return err
		}
		return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
	}
	addMutationFlags(command, control)
	command.Flags().Var(&scopeDigest, scopeDigest.name, "Exact bound-scope digest")
	command.Flags().Var(&expires, expires.name, "Optional canonical RFC3339 expiry from the current grant")
	return command
}

func newServiceAccountCommands(runtime *commandRuntime) *cobra.Command {
	command := &cobra.Command{Use: "service-accounts", Short: "Manage bounded PM service accounts", Args: noArgs}
	command.AddCommand(accessListCommand(runtime, "list", func(ctx commandContext) (pmapi.WireResponse, error) {
		return ctx.bundle.client.API().ListServiceAccountsWithResponse(ctx.context, pmapi.WorkspaceID(ctx.bundle.config.workspace), nil)
	}))
	command.AddCommand(newServiceAccountCreateCommand(runtime))
	command.AddCommand(newServiceAccountRevokeCommand(runtime))
	return command
}

func newServiceAccountCreateCommand(runtime *commandRuntime) *cobra.Command {
	control := newMutationControl()
	var displayName, expires singleString
	var workspaceScopes, connectionScopes, operationScopes singleList
	displayName.name, expires.name = "display-name", "expires-at"
	workspaceScopes.name, connectionScopes.name, operationScopes.name = "workspace-scopes", "connection-scopes", "operation-scopes"
	command := &cobra.Command{Use: "create", Short: "Create a bounded PM service account", Args: noArgs}
	command.RunE = func(command *cobra.Command, _ []string) error {
		if !expires.set || !workspaceScopes.set || !connectionScopes.set || !operationScopes.set {
			return usageFailure(errors.New("--expires-at and all three scope flags are required"))
		}
		expiration, err := requiredExpiration(expires.value)
		if err != nil {
			return usageFailure(err)
		}
		ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
		if err != nil {
			return err
		}
		defer cancel()
		var request pmapi.ServiceAccountCreateRequest
		request.ServiceAccount.ExpiresAt = expiration
		request.ServiceAccount.WorkspaceScopes = append([]string(nil), workspaceScopes.values...)
		request.ServiceAccount.ConnectionScopes = append([]string(nil), connectionScopes.values...)
		request.ServiceAccount.OperationScopes = append([]string(nil), operationScopes.values...)
		request.ServiceAccount.DisplayName = nullable.NewNullNullable[string]()
		if displayName.set {
			request.ServiceAccount.DisplayName = nullable.NewNullableWithValue(displayName.value)
		}
		protocol, parameters, err := control.protocol("service-account.create", bundle.config.workspace, "", request.ServiceAccount)
		if err != nil {
			return err
		}
		request.Protocol = protocol
		mutationParameters := &pmapi.CreateServiceAccountParams{IdempotencyKey: parameters.key, IfMatch: parameters.revision}
		response, err := sendRecoveredMutation(ctx, runtime, bundle, "service-account.create", "", protocol, request, func(attemptContext context.Context) (pmapi.RecoveryAttemptResult, error) {
			value, sendErr := bundle.client.API().CreateServiceAccountWithResponse(attemptContext, pmapi.WorkspaceID(bundle.config.workspace), mutationParameters, request)
			return recoveredResponse(value, sendErr)
		})
		if err != nil {
			return err
		}
		return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
	}
	addMutationFlags(command, control)
	command.Flags().Var(&displayName, displayName.name, "Optional display name")
	command.Flags().Var(&expires, expires.name, "Canonical RFC3339 expiry")
	command.Flags().Var(&workspaceScopes, workspaceScopes.name, "Comma-separated workspace ceilings")
	command.Flags().Var(&connectionScopes, connectionScopes.name, "Comma-separated connection ceilings")
	command.Flags().Var(&operationScopes, operationScopes.name, "Comma-separated operation ceilings")
	return command
}

func newServiceAccountRevokeCommand(runtime *commandRuntime) *cobra.Command {
	control := newMutationControl()
	var expires, grantRevision singleString
	expires.name, grantRevision.name = "expires-at", "grant-revision"
	command := &cobra.Command{Use: "revoke SERVICE_ACCOUNT_REF", Short: "Revoke an exact PM service account grant", Args: exactArgs(1)}
	command.RunE = func(command *cobra.Command, arguments []string) error {
		if !expires.set || !grantRevision.set {
			return usageFailure(errors.New("--expires-at and --grant-revision are required"))
		}
		expiration, err := requiredExpiration(expires.value)
		if err != nil {
			return usageFailure(err)
		}
		ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
		if err != nil {
			return err
		}
		defer cancel()
		var request pmapi.ServiceAccountRevokeRequest
		request.ServiceAccount.ServiceAccountRef = arguments[0]
		request.ServiceAccount.ExpiresAt = expiration
		request.ServiceAccount.GrantRevision = grantRevision.value
		protocol, parameters, err := control.protocol("service-account.revoke", bundle.config.workspace, arguments[0], request.ServiceAccount)
		if err != nil {
			return err
		}
		request.Protocol = protocol
		mutationParameters := &pmapi.RevokeServiceAccountParams{IdempotencyKey: parameters.key, IfMatch: parameters.revision}
		response, err := sendRecoveredMutation(ctx, runtime, bundle, "service-account.revoke", arguments[0], protocol, request, func(attemptContext context.Context) (pmapi.RecoveryAttemptResult, error) {
			value, sendErr := bundle.client.API().RevokeServiceAccountWithResponse(attemptContext, pmapi.WorkspaceID(bundle.config.workspace), pmapi.ResourceID(arguments[0]), mutationParameters, request)
			return recoveredResponse(value, sendErr)
		})
		if err != nil {
			return err
		}
		return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
	}
	addMutationFlags(command, control)
	command.Flags().Var(&expires, expires.name, "Canonical RFC3339 expiry from the current grant")
	command.Flags().Var(&grantRevision, grantRevision.name, "Exact service-account grant revision")
	return command
}

func newDelegationCommands(runtime *commandRuntime) *cobra.Command {
	command := &cobra.Command{Use: "delegations", Short: "Manage bounded one-hop delegations", Args: noArgs}
	command.AddCommand(accessListCommand(runtime, "list", func(ctx commandContext) (pmapi.WireResponse, error) {
		return ctx.bundle.client.API().ListDelegationsWithResponse(ctx.context, pmapi.WorkspaceID(ctx.bundle.config.workspace), nil)
	}))
	command.AddCommand(newDelegationCreateCommand(runtime))
	command.AddCommand(newDelegationRevokeCommand(runtime))
	return command
}

func newDelegationCreateCommand(runtime *commandRuntime) *cobra.Command {
	control := newMutationControl()
	var requested, effective, expires singleString
	var workspaceScopes, connectionScopes, operationScopes singleList
	requested.name, effective.name, expires.name = "requested-actor-ref", "effective-actor-ref", "expires-at"
	workspaceScopes.name, connectionScopes.name, operationScopes.name = "workspace-scopes", "connection-scopes", "operation-scopes"
	command := &cobra.Command{Use: "create", Short: "Create one bounded delegation", Args: noArgs}
	command.RunE = func(command *cobra.Command, _ []string) error {
		if !requested.set || !effective.set || !expires.set || !workspaceScopes.set || !connectionScopes.set || !operationScopes.set {
			return usageFailure(errors.New("actor, expiry and all three scope flags are required"))
		}
		expiration, err := requiredExpiration(expires.value)
		if err != nil {
			return usageFailure(err)
		}
		ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
		if err != nil {
			return err
		}
		defer cancel()
		var request pmapi.DelegationCreateRequest
		request.Delegation.RequestedActorRef = requested.value
		request.Delegation.EffectiveActorRef = effective.value
		request.Delegation.ExpiresAt = expiration
		request.Delegation.WorkspaceScopes = append([]string(nil), workspaceScopes.values...)
		request.Delegation.ConnectionScopes = append([]string(nil), connectionScopes.values...)
		request.Delegation.OperationScopes = append([]string(nil), operationScopes.values...)
		protocol, parameters, err := control.protocol("delegation.create", bundle.config.workspace, effective.value, request.Delegation)
		if err != nil {
			return err
		}
		request.Protocol = protocol
		mutationParameters := &pmapi.CreateDelegationParams{IdempotencyKey: parameters.key, IfMatch: parameters.revision}
		response, err := sendRecoveredMutation(ctx, runtime, bundle, "delegation.create", requested.value+"->"+effective.value, protocol, request, func(attemptContext context.Context) (pmapi.RecoveryAttemptResult, error) {
			value, sendErr := bundle.client.API().CreateDelegationWithResponse(attemptContext, pmapi.WorkspaceID(bundle.config.workspace), mutationParameters, request)
			return recoveredResponse(value, sendErr)
		})
		if err != nil {
			return err
		}
		return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
	}
	addMutationFlags(command, control)
	command.Flags().Var(&requested, requested.name, "Requested actor identity")
	command.Flags().Var(&effective, effective.name, "Effective actor identity")
	command.Flags().Var(&expires, expires.name, "Canonical RFC3339 expiry")
	command.Flags().Var(&workspaceScopes, workspaceScopes.name, "Comma-separated workspace ceilings")
	command.Flags().Var(&connectionScopes, connectionScopes.name, "Comma-separated connection ceilings")
	command.Flags().Var(&operationScopes, operationScopes.name, "Comma-separated operation ceilings")
	return command
}

func newDelegationRevokeCommand(runtime *commandRuntime) *cobra.Command {
	control := newMutationControl()
	var requested, effective, expires, scopeDigest singleString
	requested.name, effective.name, expires.name, scopeDigest.name = "requested-actor-ref", "effective-actor-ref", "expires-at", "scope-digest"
	command := &cobra.Command{Use: "revoke DELEGATION_REF", Short: "Revoke one exact bounded delegation", Args: exactArgs(1)}
	command.RunE = func(command *cobra.Command, arguments []string) error {
		if !requested.set || !effective.set || !expires.set || !scopeDigest.set {
			return usageFailure(errors.New("actor, expiry and scope-digest flags are required"))
		}
		expiration, err := requiredExpiration(expires.value)
		if err != nil {
			return usageFailure(err)
		}
		ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
		if err != nil {
			return err
		}
		defer cancel()
		var request pmapi.DelegationRevokeRequest
		request.Delegation.DelegationRef = arguments[0]
		request.Delegation.RequestedActorRef = requested.value
		request.Delegation.EffectiveActorRef = effective.value
		request.Delegation.ExpiresAt = expiration
		request.Delegation.ScopeDigest = scopeDigest.value
		protocol, parameters, err := control.protocol("delegation.revoke", bundle.config.workspace, arguments[0], request.Delegation)
		if err != nil {
			return err
		}
		request.Protocol = protocol
		mutationParameters := &pmapi.RevokeDelegationParams{IdempotencyKey: parameters.key, IfMatch: parameters.revision}
		response, err := sendRecoveredMutation(ctx, runtime, bundle, "delegation.revoke", arguments[0], protocol, request, func(attemptContext context.Context) (pmapi.RecoveryAttemptResult, error) {
			value, sendErr := bundle.client.API().RevokeDelegationWithResponse(attemptContext, pmapi.WorkspaceID(bundle.config.workspace), pmapi.ResourceID(arguments[0]), mutationParameters, request)
			return recoveredResponse(value, sendErr)
		})
		if err != nil {
			return err
		}
		return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
	}
	addMutationFlags(command, control)
	command.Flags().Var(&requested, requested.name, "Requested actor identity")
	command.Flags().Var(&effective, effective.name, "Effective actor identity")
	command.Flags().Var(&expires, expires.name, "Canonical RFC3339 expiry from the current delegation")
	command.Flags().Var(&scopeDigest, scopeDigest.name, "Exact delegation scope digest")
	return command
}

type commandContext struct {
	context context.Context
	bundle  clientBundle
}

type accessListCall func(commandContext) (pmapi.WireResponse, error)

func accessListCommand(runtime *commandRuntime, use string, call accessListCall) *cobra.Command {
	return &cobra.Command{
		Use: use, Short: "List safe access-control metadata", Args: noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			response, err := call(commandContext{context: ctx, bundle: bundle})
			if err != nil {
				return requestError(ctx, err)
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	}
}

func optionalExpiration(value singleString) (nullable.Nullable[time.Time], error) {
	if !value.set {
		return nullable.NewNullNullable[time.Time](), nil
	}
	parsed, err := requiredExpiration(value.value)
	if err != nil {
		return nullable.Nullable[time.Time]{}, err
	}
	return nullable.NewNullableWithValue(parsed), nil
}

func requiredExpiration(raw string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || parsed.Format(time.RFC3339Nano) != raw {
		return time.Time{}, errors.New("expiry must be canonical RFC3339")
	}
	return parsed, nil
}
