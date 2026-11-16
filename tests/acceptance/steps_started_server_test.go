package acceptance

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cucumber/godog"
)

// startedServerProfile is the pm server profile name for the pm-server that
// the starter started in the scenario.
const startedServerProfile = "started"

// registerStartedServerSteps registers the steps that send pm requests to the
// pm-server that the starter started, and the observer step that reads its
// server information (criteria steps 2, 6, 14 and 30).
func registerStartedServerSteps(sc *godog.ScenarioContext, s *stackScenario) {
	sc.Step(`^a pm server profile for the started pm-server$`, s.addStartedServerProfile)
	sc.Step(`^I run "([^"]*)" against the started pm-server$`, s.runAgainstStartedServer)
	sc.Step(`^the lane command stdout contains the deployment ID of the started pm-server$`, s.stdoutHasStartedServerDeploymentID)
	sc.Step(`^the observer reads the started pm-server information through its own HTTPS client, and its instance ID equals the instance ID in the lane command stdout$`, s.observerReadsStartedServerInstanceID)
}

// addStartedServerProfile is step 2. It adds a pm profile for the public
// origin of the deployment, which is its HTTPS edge (criteria N3). The pm
// command does not contact the server.
func (s *stackScenario) addStartedServerProfile(ctx context.Context) error {
	if s.deployment == nil {
		return errors.New("the scenario has no deployment configuration")
	}
	return s.addServerProfile(ctx, startedServerProfile, s.deployment)
}

// runAgainstStartedServer is step 6. It runs one pm command through the HTTPS
// edge of the deployment and writes the shared result state (criteria N10).
// It does not wait for or act on the process (criteria N7).
func (s *stackScenario) runAgainstStartedServer(ctx context.Context, line string) error {
	if _, err := s.started("pm-server"); err != nil {
		return err
	}
	return s.runAgainst(ctx, line, startedServerProfile, s.deployment)
}

// stdoutHasStartedServerDeploymentID is step 14. It reads the shared result
// state.
func (s *stackScenario) stdoutHasStartedServerDeploymentID() error {
	if s.deployment == nil {
		return errors.New("the scenario has no deployment configuration")
	}
	if !s.result.ran {
		return errors.New("no lane command ran")
	}
	if !strings.Contains(s.result.stdout, s.deployment.deploymentID) {
		return fmt.Errorf("stdout does not contain the deployment ID %q of the started pm-server\nstdout: %s", s.deployment.deploymentID, s.result.stdout)
	}
	return nil
}

// observerReadsStartedServerInstanceID is step 30. The observer makes one
// HTTPS request for the server information of the started pm-server through
// the HTTPS edge of step 1 (criteria N22).
func (s *stackScenario) observerReadsStartedServerInstanceID(ctx context.Context) error {
	if s.deployment == nil {
		return errors.New("the scenario has no deployment configuration")
	}
	if !s.result.ran {
		return errors.New("no lane command ran")
	}
	printed, err := instanceIDInOutput(s.result.stdout)
	if err != nil {
		return err
	}
	observed, err := readServerInstanceID(ctx, s.deployment.origin, s.deployment.authority.pem)
	if err != nil {
		return err
	}
	if observed != printed {
		return fmt.Errorf("the observer read instance ID %q from the started pm-server, but the lane command printed %q", observed, printed)
	}
	return nil
}

// readServerInstanceID is the observer helper of steps 29 and 30. It makes
// one GET request for /v1/info at the origin, with one time limit, no loop
// and no retry. The client trusts only the given CA. It uses no pm profile,
// no PM session, no proxy and no redirect. It returns the instance ID of the
// response.
func readServerInstanceID(parent context.Context, origin string, caPEM []byte) (string, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return "", errors.New("the observer cannot read the deployment CA")
	}
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	// One request has the same limit as one lane command.
	ctx, cancel := context.WithTimeout(parent, laneCommandTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/v1/info", nil)
	if err != nil {
		return "", fmt.Errorf("make the observer server information request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("the observer server information request failed: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, laneOutputLimit+1))
	if err != nil {
		return "", fmt.Errorf("read the observer server information response: %w", err)
	}
	if len(body) > laneOutputLimit {
		return "", fmt.Errorf("the observer server information response is larger than %d bytes", laneOutputLimit)
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the observer server information request returned status %d\nbody: %s", response.StatusCode, body)
	}
	var information struct {
		InstanceID string `json:"instance_id"`
	}
	if err := json.Unmarshal(body, &information); err != nil || information.InstanceID == "" {
		return "", fmt.Errorf("the observer server information response has no instance ID\nbody: %s", body)
	}
	return information.InstanceID, nil
}

// instanceIDInOutput reads the instance ID from the server information
// document that pm server info prints. The document is a JSON object (JSON
// output) or one "key<TAB>JSON value" line for each field (text output).
func instanceIDInOutput(stdout string) (string, error) {
	var document struct {
		InstanceID string `json:"instance_id"`
	}
	if json.Unmarshal([]byte(stdout), &document) == nil && document.InstanceID != "" {
		return document.InstanceID, nil
	}
	var found []string
	for _, line := range strings.Split(stdout, "\n") {
		key, value, ok := strings.Cut(line, "\t")
		if !ok || key != "instance_id" {
			continue
		}
		var instanceID string
		if json.Unmarshal([]byte(value), &instanceID) != nil || instanceID == "" {
			return "", fmt.Errorf("the instance ID line in the lane command stdout is not a JSON string: %q", line)
		}
		found = append(found, instanceID)
	}
	if len(found) != 1 {
		return "", fmt.Errorf("the lane command stdout has %d instance ID lines, want 1\nstdout: %s", len(found), stdout)
	}
	return found[0], nil
}
