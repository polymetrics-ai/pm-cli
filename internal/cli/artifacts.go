package cli

import (
	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/spf13/cobra"
)

// The canonical artifact routes are streaming/manual operations deliberately
// excluded from the ordinary generated client. QualifiedClient supplies their
// authenticated PM-server transport without introducing provider access.
func newArtifactsCommand(runtime *commandRuntime) *cobra.Command {
	command := &cobra.Command{Use: "artifacts", Short: "Use PM artifact capabilities", Args: noArgs}
	command.AddCommand(&cobra.Command{
		Use:   "upload",
		Short: "Request the authorized artifact upload capability",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			response, err := bundle.client.UploadArtifactWithResponse(ctx, pmapi.WorkspaceID(bundle.config.workspace))
			if err != nil {
				return requestError(ctx, err)
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	})
	command.AddCommand(&cobra.Command{
		Use:   "inspect ARTIFACT_ID",
		Short: "Authorize inspection of one PM artifact",
		Args:  exactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			response, err := bundle.client.InspectArtifactWithResponse(ctx, pmapi.WorkspaceID(bundle.config.workspace), pmapi.ArtifactID(arguments[0]))
			if err != nil {
				return requestError(ctx, err)
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	})
	command.AddCommand(&cobra.Command{
		Use:   "download ARTIFACT_ID",
		Short: "Authorize download of one PM artifact",
		Args:  exactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			response, err := bundle.client.DownloadArtifactWithResponse(ctx, pmapi.WorkspaceID(bundle.config.workspace), pmapi.ArtifactID(arguments[0]))
			if err != nil {
				return requestError(ctx, err)
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	})
	return command
}
