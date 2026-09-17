package cli

import (
	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/spf13/cobra"
)

func newTasksCommand(runtime *commandRuntime) *cobra.Command {
	command := &cobra.Command{Use: "tasks", Short: "Inspect server-owned durable task projections", Args: noArgs}
	command.AddCommand(&cobra.Command{
		Use:   "inspect TASK_ID",
		Short: "Inspect one task in the explicitly selected workspace",
		Args:  exactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			response, err := bundle.client.API().GetTaskWithResponse(ctx, pmapi.WorkspaceID(bundle.config.workspace), pmapi.TaskID(arguments[0]))
			if err != nil {
				return requestError(ctx, err)
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	})
	return command
}
