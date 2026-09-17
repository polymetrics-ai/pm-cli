package cli

import (
	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/spf13/cobra"
)

func newWorkspacesCommand(runtime *commandRuntime) *cobra.Command {
	command := &cobra.Command{Use: "workspaces", Short: "Inspect explicitly authorized workspaces", Args: noArgs}
	command.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List workspaces authorized for the current PM session",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, false, true)
			if err != nil {
				return err
			}
			defer cancel()
			response, err := bundle.client.API().ListWorkspacesWithResponse(ctx, nil)
			if err != nil {
				return requestError(ctx, err)
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	})
	command.AddCommand(&cobra.Command{
		Use:   "inspect",
		Short: "Inspect the explicitly selected workspace",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			response, err := bundle.client.API().GetWorkspaceWithResponse(ctx, pmapi.WorkspaceID(bundle.config.workspace))
			if err != nil {
				return requestError(ctx, err)
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	})
	return command
}
