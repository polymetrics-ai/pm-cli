package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/spf13/cobra"
)

// newBootstrapCommand is the operator subcommand of ADR 0006. It has no
// network route. It reads the bootstrap credential from the secret directory
// only after every input check passed, and it never starts the server.
func newBootstrapCommand(ctx context.Context, stdout io.Writer) *cobra.Command {
	var configurationPath, secretsDirectory, inputPath string
	configurationFlag := &singleStringFlag{name: "config", destination: &configurationPath}
	secretsFlag := &singleStringFlag{name: "secrets-directory", destination: &secretsDirectory}
	inputFlag := &singleStringFlag{name: "input", destination: &inputPath}
	command := &cobra.Command{
		Use:                   "bootstrap",
		Short:                 "Create the first workspace and admin of an organization, once",
		Args:                  cobra.NoArgs,
		SilenceErrors:         true,
		SilenceUsage:          true,
		DisableSuggestions:    true,
		DisableFlagsInUseLine: true,
		RunE: func(*cobra.Command, []string) error {
			for _, flag := range []*singleStringFlag{configurationFlag, secretsFlag, inputFlag} {
				if !flag.present || *flag.destination == "" {
					return errors.New("--" + flag.name + " requires exactly one nonempty path")
				}
			}
			return bootstrap(ctx, configurationPath, secretsDirectory, inputPath, stdout)
		},
	}
	command.Flags().Var(configurationFlag, "config", "absolute or operator-selected PM Server configuration file")
	command.Flags().Var(secretsFlag, "secrets-directory", "operator secret root that holds the bootstrap credential")
	command.Flags().Var(inputFlag, "input", "bootstrap input file; it holds no secret")
	return command
}

// openBootstrapInput opens the input without blocking and accepts only a
// regular file. A FIFO, a device or a directory is refused at once, so the
// bootstrap never waits for a writer.
func openBootstrapInput(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, app.ErrBootstrapInput
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, app.ErrBootstrapInput
	}
	return file, nil
}

// bootstrap writes one JSON document to stdout on success, and nothing on a
// refusal. main writes the one stderr line of a refusal.
func bootstrap(ctx context.Context, configurationPath, secretsDirectory, inputPath string, stdout io.Writer) error {
	configuration, err := loadConfiguration(configurationPath)
	if err != nil {
		return err
	}
	file, err := openBootstrapInput(inputPath)
	if err != nil {
		return err
	}
	input, err := app.DecodeBootstrapInput(file)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return app.ErrBootstrapInput
	}
	result, err := app.RunBootstrap(ctx, app.BootstrapConfig{
		DeploymentID:     configuration.DeploymentID(),
		ConnectorBundles: configuration.Connectors().BundlePaths(),
		Write:            app.BootstrapStoreWriter(secretsDirectory, configuration.Postgres().EndpointRef),
	}, input)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return errors.New("encode the bootstrap result")
	}
	_, err = stdout.Write(append(encoded, '\n'))
	return err
}

func loadConfiguration(path string) (config.Configuration, error) {
	file, err := os.Open(path)
	if err != nil {
		return config.Configuration{}, errors.New("server configuration is unavailable")
	}
	defer file.Close()
	document, err := config.Load(file)
	if err != nil {
		return config.Configuration{}, err
	}
	return config.Validate(document, config.Prelaunch{})
}
