package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// pmRun is one run of the real pm binary with an explicit environment.
type pmRun struct {
	code   int
	stdout []byte
	stderr []byte
}

func runPM(binary string, environment []string, stdin []byte, args ...string) (pmRun, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = environment
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	err := command.Run()
	if ctx.Err() != nil {
		return pmRun{}, errors.New("pm did not finish within 60s")
	}
	result := pmRun{stdout: stdout.Bytes(), stderr: stderr.Bytes()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		result.code = exitErr.ExitCode()
	default:
		return pmRun{}, errors.New("start pm")
	}
	return result, nil
}

func (r pmRun) summary() string {
	text := strings.TrimSpace(string(r.stderr))
	if len(text) > 160 {
		text = text[:160]
	}
	return report.scrub(text)
}

// findString returns the first string value of key at any depth of a JSON
// document.
func findString(document []byte, key string) string {
	var value any
	if json.Unmarshal(document, &value) != nil {
		return ""
	}
	var walk func(any) string
	walk = func(node any) string {
		switch typed := node.(type) {
		case map[string]any:
			if text, ok := typed[key].(string); ok {
				return text
			}
			for _, child := range typed {
				if found := walk(child); found != "" {
					return found
				}
			}
		case []any:
			for _, child := range typed {
				if found := walk(child); found != "" {
					return found
				}
			}
		}
		return ""
	}
	return walk(value)
}

// deviceLogin logs the real pm binary in through the device flow and checks
// that a second pm process loads the session from the os store.
func deviceLogin(id, pm string, d *deployment, browser *pmBrowser, k *keyring, principalID string) {
	home := filepath.Join(d.dir, "pm-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		report.FAIL(id+".pm.home", "create the pm home")
		return
	}
	base := []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LANG=C", "SSL_CERT_FILE=" + d.caFile}
	withBus := append(append([]string(nil), base...), "DBUS_SESSION_BUS_ADDRESS="+k.address, "XDG_RUNTIME_DIR="+k.runtime)
	profile, err := runPM(pm, base, nil, "server", "profiles", "add", "spike", "--url", d.origin, "--instance-id", d.deploymentID)
	if !report.check(id+".pm.profile", err == nil && profile.code == 0, "pm server profiles add (exit %d %s)", profile.code, profile.summary()) {
		return
	}
	common := []string{"--server-profile", "spike", "--output", "json"}
	start, err := runPM(pm, withBus, nil, append([]string{"sessions", "login-start", "--device-id", "pm-login-spike", "--session-store", "os"}, common...)...)
	challengeID := findString(start.stdout, "challenge_id")
	userCode := findString(start.stdout, "user_code")
	proof := findString(start.stdout, "device_proof")
	report.secret(proof)
	report.secret(userCode)
	started := time.Now()
	if !report.check(id+".pm.loginstart", err == nil && start.code == 0 && challengeID != "" && proof != "", "pm sessions login-start returns a device challenge (exit %d %s)", start.code, start.summary()) {
		return
	}
	approve, err := browser.approveDevice(challengeID, userCode)
	if !report.check(id+".pm.approve", err == nil && approve.status == 204, "the browser session approves the device challenge (status %d code %q)", approve.status, approve.problemCode()) {
		return
	}
	// The server refuses an exchange for poll_interval_seconds (5 s,
	// internal/app/runtime.go:137) after the start, and login-exchange sends
	// one request. The spike waits once for the remaining interval.
	wait := time.Until(started.Add(6 * time.Second))
	if wait > 0 {
		report.INFO(id+".pm.pollwait", "waited %.1fs for the device poll interval before login-exchange", wait.Seconds())
		time.Sleep(wait)
	}
	exchange, err := runPM(pm, withBus, []byte(proof+"\n"), append([]string{"sessions", "login-exchange", "--challenge-id", challengeID, "--session-store", "os"}, common...)...)
	report.check(id+".pm.exchange", err == nil && exchange.code == 0, "pm sessions login-exchange stores the session in the os store (exit %d %s)", exchange.code, exchange.summary())
	inspect, err := runPM(pm, withBus, nil, append([]string{"sessions", "inspect", "--session-store", "os"}, common...)...)
	report.check(id+".pm.inspect.os", err == nil && inspect.code == 0 && findString(inspect.stdout, "principal_id") == principalID,
		"a separate pm process loads the session from the os store and the server returns the seeded principal (exit %d %s)", inspect.code, inspect.summary())
	memory, err := runPM(pm, base, nil, append([]string{"sessions", "inspect", "--session-store", "memory"}, common...)...)
	report.check(id+".pm.inspect.memory", err == nil && memory.code == 3, "the memory store keeps no session across processes (exit %d)", memory.code)
	noBus, err := runPM(pm, base, nil, append([]string{"sessions", "inspect", "--session-store", "os"}, common...)...)
	report.check(id+".pm.inspect.nobus", err == nil && noBus.code != 0, "a pm process without the private bus address cannot use the session (exit %d)", noBus.code)
	for _, output := range [][]byte{start.stderr, exchange.stdout, exchange.stderr, inspect.stdout, inspect.stderr} {
		if containsAny(output, []byte(proof)) {
			report.FAIL(id+".pm.output", "a pm output after login-start holds the device proof")
			return
		}
	}
	report.PASS(id+".pm.output", "no pm output after login-start holds the device proof")
}
