package acceptance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/polymetrics-ai/pm-cli/tests/keycloak"
)

// The stack identity interface of slice L-a (PR #316). This file is the one
// place that binds the harness to it:
//   - PM_STACK_IDENTITY_PROVIDER_ISSUER is the issuer of the test realm.
//   - PM_STACK_IDENTITY_PROVIDER_DIR holds ca.pem, admin-client-id and
//     admin-client-secret, the CA of Keycloak and the scoped admin client.
//   - keycloak is the Compose service in deploy/compose.identity.yaml.
const (
	stackIdentityIssuerVariable    = "PM_STACK_IDENTITY_PROVIDER_ISSUER"
	stackIdentityDirectoryVariable = "PM_STACK_IDENTITY_PROVIDER_DIR"
	stackIdentityService           = "keycloak"
	browserCallbackPath            = "/v1/sessions/browser/callback"
)

// stackIdentity is the job-owned Keycloak of the stack. Each deployment gets
// its own client and user through the scoped admin client only.
type stackIdentity struct {
	admin   *keycloak.Admin
	ca      []byte
	project string
	docker  string
	engine  []string
}

// readStackIdentity reads the stack identity interface of slice L-a from the
// environment that scripts/ci/with-stack-fixture.py gives to the suite.
func readStackIdentity() (stackIdentity, error) {
	issuer := os.Getenv(stackIdentityIssuerVariable)
	directory := os.Getenv(stackIdentityDirectoryVariable)
	if issuer == "" || !filepath.IsAbs(directory) {
		return stackIdentity{}, fmt.Errorf("%s and %s must be set: the job stack needs the Keycloak service of slice L-a", stackIdentityIssuerVariable, stackIdentityDirectoryVariable)
	}
	ca, caErr := readBoundedFile(filepath.Join(directory, "ca.pem"), 64<<10)
	clientID, idErr := readBoundedFile(filepath.Join(directory, "admin-client-id"), 256)
	secret, secretErr := readBoundedFile(filepath.Join(directory, "admin-client-secret"), 256)
	if err := errors.Join(caErr, idErr, secretErr); err != nil {
		return stackIdentity{}, err
	}
	admin, err := keycloak.New(keycloak.Config{
		Issuer:       issuer,
		CA:           ca,
		ClientID:     strings.TrimSpace(string(clientID)),
		ClientSecret: strings.TrimSpace(string(secret)),
		RequestLimit: laneCommandTimeout,
	})
	clear(secret)
	if err != nil {
		return stackIdentity{}, err
	}
	identity := stackIdentity{admin: admin, ca: ca, project: os.Getenv("PM_STACK_PROJECT"), engine: []string{"PATH=" + os.Getenv("PATH")}}
	for _, name := range []string{"DOCKER_HOST", "DOCKER_CONFIG"} {
		value := os.Getenv(name)
		if value == "" {
			return stackIdentity{}, fmt.Errorf("%s is not set: run the @stack scenarios through scripts/ci/with-stack-fixture.py", name)
		}
		identity.engine = append(identity.engine, name+"="+value)
	}
	if identity.project == "" {
		return stackIdentity{}, errors.New("PM_STACK_PROJECT is not set: run the @stack scenarios through scripts/ci/with-stack-fixture.py")
	}
	if identity.docker, err = exec.LookPath("docker"); err != nil {
		return stackIdentity{}, fmt.Errorf("find the docker CLI for the job engine: %w", err)
	}
	return identity, nil
}

// checkRecords reads the Keycloak container record and the provider log once
// each, after the removal of the deployment identity. It fails when either
// holds a form of the admin client secret, the deployment client secret or
// the deployment user password (D1-5). A scan is detection, not a boundary.
func (s stackIdentity) checkRecords(dir string, identity *keycloak.Identity) error {
	listed, err := s.run(dir, "ps", "--all", "--quiet", "--no-trunc",
		"--filter", "label=com.docker.compose.project="+s.project,
		"--filter", "label=com.docker.compose.service="+stackIdentityService)
	if err != nil {
		return err
	}
	containers := strings.Fields(string(listed))
	if len(containers) != 1 {
		return fmt.Errorf("the job engine lists %d Keycloak containers of the stack, want 1", len(containers))
	}
	var errs []error
	for _, record := range []struct {
		name string
		args []string
	}{{"container record", []string{"container", "inspect", containers[0]}}, {"provider log", []string{"logs", containers[0]}}} {
		data, err := s.run(dir, record.args...)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, secret := range s.admin.Leaks(data, identity) {
			errs = append(errs, fmt.Errorf("the Keycloak %s holds the %s", record.name, secret))
		}
		clear(data)
	}
	return errors.Join(errs...)
}

// run runs the docker CLI once against the job engine. It waits once for the
// exit, with laneCommandTimeout. The output goes to a file in dir, not to a
// pipe, so the wait is only for the exit. The file is read after the exit, at
// most laneFileLimit bytes, and removed. The CLI gets only PATH and the engine
// endpoint and configuration of the job.
func (s stackIdentity) run(dir string, args ...string) ([]byte, error) {
	output, err := os.CreateTemp(dir, "docker-*.out")
	if err != nil {
		return nil, fmt.Errorf("create the docker output file: %w", err)
	}
	defer func() {
		_ = output.Close()
		_ = os.Remove(output.Name())
	}()
	ctx, cancel := context.WithTimeout(context.Background(), laneCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, s.docker, args...)
	command.Env = s.engine
	command.Stdout = output
	command.Stderr = output
	runErr := command.Run()
	text, readErr := io.ReadAll(io.NewSectionReader(output, 0, laneFileLimit+1))
	switch {
	case ctx.Err() != nil:
		return nil, fmt.Errorf("docker %s did not finish within %s", args[0], laneCommandTimeout)
	case runErr != nil:
		// The output can hold provider log lines, so the error does not show it.
		return nil, fmt.Errorf("docker %s: %w (%d bytes of output, not shown)", args[0], runErr, len(text))
	case readErr != nil:
		return nil, fmt.Errorf("read the docker %s output: %w", args[0], readErr)
	case len(text) > laneFileLimit:
		return nil, fmt.Errorf("the docker %s output is larger than %d bytes", args[0], laneFileLimit)
	}
	return text, nil
}
