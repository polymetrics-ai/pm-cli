package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/spf13/cobra"
)

// The warehouse commands are thin clients of the typed warehouse routes: they
// submit an explicitly scoped request through the shared authenticated PM
// client and render the server's answer. They never read an object, open a
// database, or resolve a provider credential, and they never send statement
// text to the server.
const maximumWarehouseQueryDocumentBytes = 4 << 20

// newWarehouseCommand returns the one warehouse namespace.
func newWarehouseCommand(runtime *commandRuntime) *cobra.Command {
	command := &cobra.Command{
		Use:   "warehouse",
		Short: "Read one connection's committed warehouse dataset",
		Long: `Read the committed warehouse view of one explicitly selected connection.

Both commands are read-only. They select the connection with the shared --connection control,
never with a raw dataset path, and they require no provider credential: the server reads its own
registered segments through its own shared storage and bounded query child.

Help and completion for this namespace work offline, without a reachable server or any credential.`,
		Args: noArgs,
	}
	command.AddCommand(newWarehouseSnapshotCommand(runtime))
	command.AddCommand(newWarehouseQueryCommand(runtime))
	return command
}

func newWarehouseSnapshotCommand(runtime *commandRuntime) *cobra.Command {
	dataset := singleString{name: "dataset"}
	snapshotID := singleString{name: "snapshot-id"}
	command := &cobra.Command{
		Use:   "snapshot",
		Short: "Inspect the committed revision, cursor and registered segments of one dataset",
		Long: `Report the dataset's committed manifest revision, its acknowledged cursor and the
registered immutable segments that currently serve it. Listing the shared objects never
establishes dataset membership: only the registered manifest does.`,
		Args: noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			connection, err := requiredConnection(runtime, dataset)
			if err != nil {
				return err
			}
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			params := &pmapi.GetWarehouseDatasetParams{}
			if snapshotID.set {
				value := snapshotID.value
				params.SnapshotId = &value
			}
			response, err := bundle.client.API().GetWarehouseDatasetWithResponse(
				ctx,
				pmapi.WorkspaceID(bundle.config.workspace),
				pmapi.ConnectionID(connection),
				pmapi.DatasetName(dataset.value),
				params,
			)
			if err != nil {
				return requestError(ctx, err)
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	}
	command.Flags().Var(&dataset, dataset.name, "Declared dataset component of the connection's warehouse namespace")
	command.Flags().Var(&snapshotID, snapshotID.name, "Read one retained snapshot's exact segment set")
	return command
}

func newWarehouseQueryCommand(runtime *commandRuntime) *cobra.Command {
	dataset := singleString{name: "dataset"}
	queryFile := singleString{name: "query-file"}
	command := &cobra.Command{
		Use:   "query",
		Short: "Run one bounded typed query over a dataset's registered segments",
		Long: `Run one admitted typed query over the dataset's immutable registered segments and
print the exact typed rows.

The query document declares a relation, projections, typed predicates, order, parameter
bindings and a limit with snake_case members:

  {"relation":"orders","project":[{"column":"id","alias":null}],"predicates":[],
   "order":[],"parameters":[],"limit":100,"snapshot_id":null}

No member can carry statement text, so raw SQL, file functions, extensions, unknown
identifiers and cross-scope paths are refused by the server before it reads any object byte.`,
		Args: exactArgs(0),
		RunE: func(command *cobra.Command, _ []string) error {
			connection, err := requiredConnection(runtime, dataset)
			if err != nil {
				return err
			}
			if !queryFile.set {
				return usageFailure(errors.New("--query-file is required"))
			}
			document, err := readWarehouseQueryDocument(queryFile.value)
			if err != nil {
				return usageFailure(err)
			}
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			response, err := bundle.client.API().RunWarehouseQueryWithResponse(
				ctx,
				pmapi.WorkspaceID(bundle.config.workspace),
				pmapi.ConnectionID(connection),
				pmapi.DatasetName(dataset.value),
				&pmapi.RunWarehouseQueryParams{},
				document,
			)
			if err != nil {
				return requestError(ctx, err)
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	}
	command.Flags().Var(&dataset, dataset.name, "Declared dataset component of the connection's warehouse namespace")
	command.Flags().Var(&queryFile, queryFile.name, "Read the typed query document from this file")
	return command
}

// requiredConnection resolves the shared --connection control and the required
// dataset member once, so no command invents a dataset identity from a path.
func requiredConnection(runtime *commandRuntime, dataset singleString) (string, error) {
	if !dataset.set {
		return "", usageFailure(errors.New("--dataset is required"))
	}
	connection := runtime.flags.connection
	if !connection.set {
		return "", usageFailure(errors.New("--connection is required"))
	}
	return connection.value, nil
}

// readWarehouseQueryDocument reads one bounded query document and refuses an
// unknown, duplicated, wrongly cased or incomplete member before any request is
// sent.
func readWarehouseQueryDocument(path string) (pmapi.WarehouseQueryRequest, error) {
	if !filepath.IsAbs(path) && filepath.Clean(path) != path {
		return pmapi.WarehouseQueryRequest{}, errors.New("--query-file must be one cleaned path")
	}
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return pmapi.WarehouseQueryRequest{}, errors.New("the query document is unavailable")
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, maximumWarehouseQueryDocumentBytes+1))
	if err != nil {
		return pmapi.WarehouseQueryRequest{}, errors.New("the query document could not be read")
	}
	if len(content) > maximumWarehouseQueryDocumentBytes {
		return pmapi.WarehouseQueryRequest{}, errors.New("the query document exceeds the client bound")
	}
	var document pmapi.WarehouseQueryRequest
	if err := contract.DecodeStrictRequiredObject(content, &document,
		"relation", "project", "predicates", "order", "parameters", "limit", "snapshot_id"); err != nil {
		return pmapi.WarehouseQueryRequest{}, fmt.Errorf("the query document is not a declared typed query: %v", err)
	}
	return document, nil
}
