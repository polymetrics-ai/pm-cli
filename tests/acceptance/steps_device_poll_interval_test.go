package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/cucumber/godog"
)

// devicePollIntervalLimit is the time limit of the one login-start command of
// the D1 step: the limit of one lane command.
const devicePollIntervalLimit = laneCommandTimeout

// devicePollIntervalScenario is the state of the device poll interval steps
// of #366 (criteria-poll-interval-v3.md, section "New step phrases"). After
// the D1 step it holds only the stated poll interval. It never holds the
// login-start stdout, the device proof or the user code.
type devicePollIntervalScenario struct {
	shared *stackScenario
	// kept is true after the D1 step kept a poll interval.
	kept bool
	// pollInterval is the poll_interval_seconds of the login-start answer.
	pollInterval int
}

// jsonInteger is the form of a JSON integer.
var jsonInteger = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)$`)

// registerDevicePollIntervalSteps registers the four new steps of #366.
func registerDevicePollIntervalSteps(sc *godog.ScenarioContext, shared *stackScenario) {
	s := &devicePollIntervalScenario{shared: shared}
	sc.Step(`^the deployment configuration sets the device poll interval to (\d+) seconds$`, s.setSeconds)
	sc.Step(`^the deployment configuration sets the device poll interval to the JSON value (.+)$`, s.setJSONValue)
	sc.Step(`^I start a device login on the started pm-server and keep only its poll interval$`, s.startAndKeepPollInterval)
	sc.Step(`^the device login start answer states a poll interval of (\d+) seconds$`, s.statesPollInterval)
}

// setSeconds is the step "the deployment configuration sets the device poll
// interval to <n> seconds". It supplies the JSON string "<n>s".
func (s *devicePollIntervalScenario) setSeconds(seconds string) error {
	token, err := json.Marshal(seconds + "s")
	if err != nil {
		return errors.New("the device poll interval seconds do not encode as a JSON string")
	}
	return s.set(token)
}

// setJSONValue is the step "the deployment configuration sets the device
// poll interval to the JSON value <token>". It supplies the token exactly as
// the step writes it, and the configuration writer writes it byte for byte.
// The token must be valid JSON.
func (s *devicePollIntervalScenario) setJSONValue(value string) error {
	token := []byte(value)
	if !json.Valid(token) {
		return errors.New("the device poll interval value of the step is not valid JSON")
	}
	return s.set(token)
}

// set keeps the token in the deployment. It refuses when the configuration
// file is already written, because the writer writes the file only once. It
// writes no file and starts no process.
func (s *devicePollIntervalScenario) set(token []byte) error {
	d := s.shared.deployment
	if d == nil {
		return errors.New("the scenario has no deployment configuration")
	}
	if d.devicePollInterval != nil {
		return errors.New("the deployment configuration already sets the device poll interval")
	}
	if d.configuration != "" {
		return errors.New("the deployment configuration file is already written, so the device poll interval step must come before it")
	}
	// The name of the file that writeConfiguration writes.
	if _, err := os.Lstat(filepath.Join(d.dir, "pm-deployment.json")); !errors.Is(err, fs.ErrNotExist) {
		return errors.New("the deployment configuration file already exists, or it cannot be checked")
	}
	d.devicePollInterval = json.RawMessage(bytes.Clone(token))
	return nil
}

// startAndKeepPollInterval is the D1 step "I start a device login on the
// started pm-server and keep only its poll interval". It runs pm sessions
// login-start --device-id acceptance-device once against the started
// pm-server, with one time limit. The command must exit with code 0 and its
// stdout must parse before the step keeps anything. Then the step keeps only
// the integer poll_interval_seconds and discards the rest of stdout. The
// command stderr is discarded. A failure message shows only the exit code, a
// fixed reason or the time limit, never stdout or stderr.
func (s *devicePollIntervalScenario) startAndKeepPollInterval(parent context.Context) error {
	if _, err := s.shared.started("pm-server"); err != nil {
		return err
	}
	if s.kept {
		return errors.New("the scenario already kept a device login poll interval")
	}
	d := s.shared.deployment
	if d == nil {
		return errors.New("the scenario has no deployment configuration")
	}
	if s.shared.home == "" {
		return errors.New("the pm server profile is not set")
	}
	// The environment of the device login steps: the scenario HOME, the
	// deployment CA as the only trusted root, PM_SERVER_PROFILE for the
	// profile of the started pm-server, and the private keyring.
	environment := []string{
		"HOME=" + s.shared.home, "PATH=/usr/bin:/bin", "LANG=C",
		"SSL_CERT_FILE=" + d.caFile,
		"PM_SERVER_PROFILE=" + startedServerProfile,
	}
	if s.shared.keyring != nil {
		environment = append(environment, s.shared.keyring.Environment()...)
	}
	ctx, cancel := context.WithTimeout(parent, devicePollIntervalLimit)
	defer cancel()
	command := exec.CommandContext(ctx, s.shared.harness.bins.pm, "sessions", "login-start", "--device-id", "acceptance-device")
	command.Env = environment
	var stdout bytes.Buffer
	command.Stdout = &stdout
	// A nil Stderr is the null device: the step never holds stderr.
	command.Stderr = nil
	runErr := command.Run()
	// The device proof is in stdout. Clear it on every return.
	defer clear(stdout.Bytes())
	if ctx.Err() != nil {
		return fmt.Errorf("pm sessions login-start did not finish within %s", devicePollIntervalLimit)
	}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		return fmt.Errorf("pm sessions login-start exited with code %d, want 0", exitErr.ExitCode())
	default:
		return errors.New("pm sessions login-start did not start")
	}
	interval, ok := loginStartPollInterval(stdout.Bytes())
	if !ok {
		return errors.New("the pm sessions login-start stdout does not parse as an answer with one integer poll_interval_seconds")
	}
	s.pollInterval = interval
	s.kept = true
	return nil
}

// statesPollInterval is the D1 step "the device login start answer states a
// poll interval of <n> seconds". A mismatch shows only the two integers.
func (s *devicePollIntervalScenario) statesPollInterval(want int) error {
	if !s.kept {
		return errors.New("the scenario kept no device login poll interval")
	}
	if s.pollInterval != want {
		return fmt.Errorf("the device login start answer states a poll interval of %d seconds, want %d", s.pollInterval, want)
	}
	return nil
}

// loginStartPollInterval reads poll_interval_seconds from the login-start
// stdout. The stdout is one JSON object (JSON output) or one "key<TAB>JSON
// value" line for each field, sorted by key (text output,
// internal/cli/render.go). Each key must occur once, each value must be valid
// JSON, and poll_interval_seconds must be a JSON integer. It returns no other
// field.
func loginStartPollInterval(stdout []byte) (int, bool) {
	fields := map[string]json.RawMessage{}
	if trimmed := bytes.TrimSpace(stdout); len(trimmed) > 0 && trimmed[0] == '{' {
		if json.Unmarshal(trimmed, &fields) != nil {
			return 0, false
		}
	} else {
		text, found := strings.CutSuffix(string(stdout), "\n")
		if !found || text == "" {
			return 0, false
		}
		for _, line := range strings.Split(text, "\n") {
			key, value, ok := strings.Cut(line, "\t")
			if !ok || key == "" || !json.Valid([]byte(value)) {
				return 0, false
			}
			if _, duplicate := fields[key]; duplicate {
				return 0, false
			}
			fields[key] = json.RawMessage(value)
		}
	}
	raw, found := fields["poll_interval_seconds"]
	if !found {
		return 0, false
	}
	// A JSON integer only: no string, fraction or exponent.
	if !jsonInteger.Match(raw) {
		return 0, false
	}
	interval, err := strconv.Atoi(string(raw))
	if err != nil {
		return 0, false
	}
	return interval, true
}
