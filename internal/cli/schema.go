package cli

import (
	"errors"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/spf13/cobra"
)

// The schema commands are thin clients of the connection schema route. They
// report the immutable metadata snapshot the connection currently selects and
// the exact typed fields it declares. They never discover metadata, never
// promote or select a snapshot, and never resolve a provider credential: the
// connection's own config.json owns the sole schema_ref selection, and a failed
// or partial discovery never replaces the last complete valid snapshot.
func newSchemaCommand(runtime *commandRuntime) *cobra.Command {
	command := &cobra.Command{
		Use:   "schema",
		Short: "Inspect the selected metadata snapshot of one connection",
		Long: `Inspect the immutable metadata snapshot one explicitly selected connection uses.

The command reports the connection's current selection, that snapshot's declared binding and
completeness, and the effective typed fields built from it. It is read-only, requires no provider
credential, and performs no discovery or selection.

Help and completion for this namespace work offline, without a reachable server or any credential.`,
		Args: noArgs,
	}
	command.AddCommand(newSchemaShowCommand(runtime))
	return command
}

func newSchemaShowCommand(runtime *commandRuntime) *cobra.Command {
	resource := singleString{name: "resource"}
	permissionRevision := singleString{name: "permission-revision"}
	sourceRevision := singleString{name: "source-revision"}
	providerAccount := singleString{name: "provider-account"}
	snapshotID := singleString{name: "snapshot-id"}
	command := &cobra.Command{
		Use:   "show",
		Short: "Show one connection's selected snapshot and its effective typed fields",
		Long: `Show the exact binding, completeness and effective fields of one connection's selected
metadata snapshot.

The binding members are the caller's declared scope, not a widening selector: the server serves
only a snapshot whose stored binding matches the connection, provider account, permission
revision, resource and source revision named here. An absent selection is reported explicitly
instead of being rendered as an empty discovery.`,
		Args: noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			connection, err := requiredSchemaConnection(runtime, resource, permissionRevision, sourceRevision)
			if err != nil {
				return err
			}
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			params := &pmapi.GetConnectionSchemaParams{
				Resource:           pmapi.SchemaResource(resource.value),
				PermissionRevision: pmapi.SchemaPermissionRevision(permissionRevision.value),
				SourceRevision:     pmapi.SchemaSourceRevision(sourceRevision.value),
			}
			if providerAccount.set {
				value := pmapi.SchemaProviderAccount(providerAccount.value)
				params.ProviderAccountId = &value
			}
			if snapshotID.set {
				value := snapshotID.value
				params.SnapshotId = &value
			}
			response, err := bundle.client.API().GetConnectionSchemaWithResponse(
				ctx,
				pmapi.WorkspaceID(bundle.config.workspace),
				pmapi.ConnectionID(connection),
				params,
			)
			if err != nil {
				return requestError(ctx, err)
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	}
	command.Flags().Var(&resource, resource.name, "Declared provider resource the metadata is bound to")
	command.Flags().Var(&permissionRevision, permissionRevision.name, "Permission revision the discovery observed")
	command.Flags().Var(&sourceRevision, sourceRevision.name, "Source revision the discovery observed")
	command.Flags().Var(&providerAccount, providerAccount.name, "Provider account the discovery observed; defaults to the connection's stored account")
	command.Flags().Var(&snapshotID, snapshotID.name, "Inspect one declared snapshot instead of the connection's selection")
	return command
}

// requiredSchemaConnection resolves the shared --connection control and the
// binding members the route requires, so an incomplete binding is a usage
// refusal rather than a server round-trip.
func requiredSchemaConnection(
	runtime *commandRuntime,
	resource singleString,
	permissionRevision singleString,
	sourceRevision singleString,
) (string, error) {
	connection := runtime.flags.connection
	if !connection.set {
		return "", usageFailure(errors.New("--connection is required"))
	}
	for _, member := range []singleString{resource, permissionRevision, sourceRevision} {
		if !member.set {
			return "", usageFailure(errors.New("--" + member.name + " is required"))
		}
	}
	return connection.value, nil
}
