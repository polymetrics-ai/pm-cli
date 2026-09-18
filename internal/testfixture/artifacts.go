package testfixture

import (
	"bytes"
	"os"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/credentials"
)

type ArtifactGateway string

const (
	PrimaryArtifactGateway   ArtifactGateway = "primary"
	SecondaryArtifactGateway ArtifactGateway = "secondary"
)

// OpenArtifacts uses the actual process-owned adapter, operator secret ingress,
// and private scratch lifecycle against one selected real shared-object gateway.
// It is not an in-memory or local-file replacement for shared bytes.
func OpenArtifacts(t testing.TB, gateway ArtifactGateway) *app.ArtifactResources {
	t.Helper()
	path := os.Getenv("PM_STACK_TEST_CONFIG")
	if path == "" {
		t.Fatal("PM_STACK_TEST_CONFIG is required for the selected artifact fixture")
	}
	document, err := config.Load(bytes.NewReader(readBoundedConfiguration(t, path)))
	if err != nil {
		t.Fatalf("load selected artifact configuration: %v", err)
	}
	configuration, err := config.Validate(document, config.Prelaunch{})
	if err != nil {
		t.Fatalf("validate selected artifact configuration: %v", err)
	}
	artifacts := configuration.Artifacts()
	if !artifacts.Execution.Configured() {
		t.Fatal("selected artifact execution controls are required")
	}
	switch gateway {
	case PrimaryArtifactGateway:
		artifacts.Endpoint = os.Getenv("PM_STACK_TEST_S3_PRIMARY_ENDPOINT")
		if artifacts.Endpoint == "" {
			t.Fatal("selected primary artifact gateway is required")
		}
	case SecondaryArtifactGateway:
		artifacts.Endpoint = os.Getenv("PM_STACK_TEST_S3_SECONDARY_ENDPOINT")
		if artifacts.Endpoint == "" {
			t.Fatal("selected secondary artifact gateway is required")
		}
	default:
		t.Fatal("artifact gateway is not selected by the owned fixture")
	}
	secretsDirectory := os.Getenv("PM_STACK_SECRETS_DIR")
	if secretsDirectory == "" {
		t.Fatal("PM_STACK_SECRETS_DIR is required for the selected artifact fixture")
	}
	secrets, err := credentials.OpenOperatorSecrets(secretsDirectory)
	if err != nil {
		t.Fatalf("open selected artifact secret authority: %v", err)
	}
	t.Cleanup(func() {
		if err := secrets.Close(); err != nil {
			t.Errorf("close artifact secret authority: %v", err)
		}
	})
	resources, err := app.OpenArtifactResources(artifacts, secrets)
	if err != nil {
		t.Fatalf("open actual artifact byte resources: %v", err)
	}
	if resources == nil {
		t.Fatal("selected artifact backend is unavailable")
	}
	t.Cleanup(func() {
		if err := resources.Close(); err != nil {
			t.Errorf("close artifact byte resources: %v", err)
		}
	})
	return resources
}
