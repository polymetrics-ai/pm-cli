package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/spf13/cobra"
)

// The schema commands are thin clients of the connection schema routes. show
// reports the immutable metadata snapshot the connection currently selects and
// the exact typed fields it declares. discover asks the server to admit one
// durable discovery run; pm-worker performs the metadata I/O. No schema
// command selects a snapshot or resolves a provider credential, and a failed
// or partial discovery never replaces the last complete valid snapshot.
func newSchemaCommand(runtime *commandRuntime) *cobra.Command {
	command := &cobra.Command{
		Use:   "schema",
		Short: "Discover or inspect the metadata snapshots of one connection",
		Long: `Discover or inspect the immutable metadata snapshots of one explicitly selected connection.

show reports the connection's current selection, that snapshot's declared binding and
completeness, and the effective typed fields built from it. It is read-only. discover starts one
durable discovery run of one declared relation, which stores one complete candidate snapshot and
never selects it. No schema command requires a provider credential in the client.

Help and completion for this namespace work offline, without a reachable server or any credential.`,
		Args: noArgs,
	}
	command.AddCommand(newSchemaShowCommand(runtime), newSchemaDiscoverCommand(runtime))
	return command
}

func newSchemaDiscoverCommand(runtime *commandRuntime) *cobra.Command {
	resource := singleString{name: "resource"}
	requestKey := singleString{name: "request-key"}
	command := &cobra.Command{
		Use:   "discover",
		Short: "Start one discovery of a declared relation of one connection",
		Long: `Start one durable discovery of one relation that the connection declares.

The server admits a discovery run. pm-worker runs only the connector's declared metadata
operation for that relation and stores one complete candidate snapshot. A discovery never
selects a snapshot.

With --wait, the command waits for the run and exits with a nonzero code when the run does
not succeed. An interrupt ends only the wait, not the run, and exits with code 0 (issue #437).
Run pm runs inspect RUN_ID with the run ID from the notice to see its state.

A resource that the connection does not declare is refused with schema_relation_unknown and
exit code 3 before any run starts. Check the --resource value against the relations that the
connection declares.

When the outcome of the admission is unknown, the command exits with code 7 and names the
request key. Repeat the same command with that --request-key to get the same run. A new key
starts a new discovery.`,
		Args: noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			connection := runtime.flags.connection
			if !connection.set {
				return usageFailure(errors.New("--connection is required"))
			}
			if !resource.set {
				return usageFailure(errors.New("--resource is required"))
			}
			explicitKey := ""
			if requestKey.set {
				explicitKey = requestKey.value
			}
			selection, err := pmapi.SelectRequestKey(explicitKey)
			if err != nil {
				if errors.Is(err, pmapi.ErrInvalidRequestKey) {
					return usageFailure(errors.New("--request-key is invalid"))
				}
				return err
			}
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			response, err := bundle.client.API().DiscoverConnectionSchemaWithResponse(
				ctx,
				pmapi.WorkspaceID(bundle.config.workspace),
				pmapi.ConnectionID(connection.value),
				&pmapi.DiscoverConnectionSchemaParams{IdempotencyKey: pmapi.IdempotencyKey(selection.RequestKey)},
				pmapi.DiscoverConnectionSchemaJSONRequestBody{Resource: resource.value},
			)
			if err != nil {
				return discoveryAdmissionError(ctx, selection.RequestKey, err)
			}
			if !bundle.config.wait {
				return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
			}
			envelope, err := pmapi.DecodeEnvelope(response)
			if err != nil {
				return err
			}
			if problem := pmapi.EnvelopeError(envelope); problem != nil {
				return problem
			}
			admitted, err := pmapi.DecodeAdmissionResponse(envelope.Body)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(command.ErrOrStderr(), "notice: accepted discovery run %s; waiting for its terminal state\n", admitted.RunRef)
			waitContext, cancelWait := context.WithTimeout(command.Context(), bundle.config.requestTimeout)
			defer cancelWait()
			// The event stream is followed for its terminal projection only. The
			// run result projection then decides the exit code.
			completed, err := watchRunToTerminal(waitContext, io.Discard, bundle, admitted.RunRef)
			if err != nil || !completed {
				return err
			}
			return fetchAndRenderRunResult(waitContext, command.OutOrStdout(), bundle, admitted.RunRef)
		},
	}
	command.Flags().Var(&resource, resource.name, "Declared resource of the relation to discover")
	command.Flags().Var(&requestKey, requestKey.name, "Existing retained request key for an intentional replay; omit to allocate a new key")
	return command
}

// discoveryAdmissionError maps a failed admission send. A transport failure
// can follow a committed admission, so its outcome is unknown: the command
// exits with code 7 and names the request key, as the shared mutation recovery
// does, and a repeat with that key replays the same admission. Every other
// failure keeps its own exit code.
func discoveryAdmissionError(ctx context.Context, requestKey string, err error) error {
	err = requestError(ctx, err)
	if !errors.Is(err, pmapi.ErrClientTransport) {
		return err
	}
	return &exitError{
		code:    7,
		message: fmt.Sprintf("PM discovery admission outcome is unknown; repeat only this command with %s to get the same run", requestKeyFlag(requestKey)),
		cause:   err,
	}
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
