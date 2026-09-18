package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

type CommandSurface string

const (
	CommandSurfaceHelp       CommandSurface = "help"
	CommandSurfaceManual     CommandSurface = "manual"
	CommandSurfaceCompletion CommandSurface = "completion"
)

// RenderHelp renders the validated command tree without performing discovery
// or execution I/O.
func RenderHelp(root *cobra.Command, writer io.Writer) error {
	return RenderCommandSurface(root, CommandSurfaceHelp, "", writer)
}

// RenderCommandSurface renders help, a complete manual, or shell completion
// from the same already-validated executable Cobra tree. It performs no
// configuration, authentication, catalog, or execution I/O.
func RenderCommandSurface(root *cobra.Command, surface CommandSurface, shell string, writer io.Writer) error {
	if root == nil || writer == nil {
		return errors.New("command surface and output are required")
	}
	original := root.OutOrStdout()
	root.SetOut(writer)
	defer root.SetOut(original)
	switch surface {
	case CommandSurfaceHelp:
		return root.Help()
	case CommandSurfaceManual:
		return renderManual(root, writer)
	case CommandSurfaceCompletion:
		if err := renderCompletionManifest(root, writer); err != nil {
			return err
		}
		switch shell {
		case "bash":
			return root.GenBashCompletionV2(writer, true)
		case "zsh":
			return root.GenZshCompletion(writer)
		case "fish":
			return root.GenFishCompletion(writer, true)
		case "powershell":
			return root.GenPowerShellCompletion(writer)
		default:
			return usageFailure(errors.New("completion shell must be bash, zsh, fish, or powershell"))
		}
	default:
		return errors.New("command surface kind is unsupported")
	}
}

func renderCompletionManifest(root *cobra.Command, writer io.Writer) error {
	var render func(*cobra.Command) error
	render = func(command *cobra.Command) error {
		if command.Hidden {
			return nil
		}
		if _, err := fmt.Fprintf(writer, "# PM-COMMAND %s ALIASES %v\n", command.CommandPath(), command.Aliases); err != nil {
			return err
		}
		for _, line := range strings.Split(strings.TrimSpace(command.NonInheritedFlags().FlagUsages()), "\n") {
			if line == "" {
				continue
			}
			if _, err := fmt.Fprintf(writer, "# PM-FLAG %s %s\n", command.CommandPath(), strings.TrimSpace(line)); err != nil {
				return err
			}
		}
		for _, child := range command.Commands() {
			if err := render(child); err != nil {
				return err
			}
		}
		return nil
	}
	return render(root)
}

func renderManual(root *cobra.Command, writer io.Writer) error {
	var render func(*cobra.Command) error
	render = func(command *cobra.Command) error {
		if command.Hidden {
			return nil
		}
		if _, err := fmt.Fprintf(writer, "COMMAND %s\n\n", command.CommandPath()); err != nil {
			return err
		}
		original := command.OutOrStdout()
		command.SetOut(writer)
		if err := command.Help(); err != nil {
			command.SetOut(original)
			return err
		}
		command.SetOut(original)
		if _, err := fmt.Fprintln(writer); err != nil {
			return err
		}
		for _, child := range command.Commands() {
			if err := render(child); err != nil {
				return err
			}
		}
		return nil
	}
	return render(root)
}

func newManualCommand(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:   "manual",
		Short: "Render a complete manual from the executable command tree",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return RenderCommandSurface(root, CommandSurfaceManual, "", command.OutOrStdout())
		},
	}
}

func newCompletionCommand(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:   "completion SHELL",
		Short: "Render shell completion from the executable command tree",
		Args:  exactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			return RenderCommandSurface(root, CommandSurfaceCompletion, arguments[0], command.OutOrStdout())
		},
	}
}
