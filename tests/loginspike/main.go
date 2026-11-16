// Command loginspike is a throwaway experiment for issue #72 (spike branch
// t3/72-login-spike only, never merged). It runs in the bdd job of the CI image
// and checks three ways for a Gherkin scenario to log in with the real pm
// binary: a private Secret Service keyring for the os session store, a real
// open-source OIDC provider in the job's engine, and a file channel that
// brings a dummy provider token to the credential ingress. It prints PASS or
// FAIL for each check and never prints a password, unlock secret or token.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "keyring-store" || os.Args[1] == "keyring-load") {
		os.Exit(keyringChild(os.Args[1]))
	}
	if len(os.Args) < 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "usage: loginspike run --pm PATH --pm-server PATH")
		os.Exit(2)
	}
	flags := flag.NewFlagSet("run", flag.ExitOnError)
	pm := flags.String("pm", "", "pm binary")
	pmServer := flags.String("pm-server", "", "pm-server binary")
	_ = flags.Parse(os.Args[2:])
	os.Exit(run(*pm, *pmServer))
}

type tokenIntake struct {
	file []byte
	env  []byte
}

func run(pm, pmServer string) int {
	begin := time.Now()
	report.INFO("start", "login spike: commit %s, run %s attempt %s", os.Getenv("GITHUB_SHA"), os.Getenv("GITHUB_RUN_ID"), os.Getenv("GITHUB_RUN_ATTEMPT"))
	self, err := os.Executable()
	if err != nil || pm == "" || pmServer == "" {
		report.FAIL("start", "the spike binary paths are incomplete")
		return 1
	}
	runnerTemp := os.Getenv("RUNNER_TEMP")
	if runnerTemp == "" {
		report.FAIL("start", "RUNNER_TEMP is required")
		return 1
	}
	work := filepath.Join(runnerTemp, "pm-login-spike-"+randomHex(6))
	if err := os.Mkdir(work, 0o700); err != nil {
		report.FAIL("start", "create the job work directory")
		return 1
	}
	defer func() {
		_ = os.RemoveAll(work)
		_, statErr := os.Stat(work)
		report.check("cleanup.work", errors.Is(statErr, os.ErrNotExist), "the job work directory (configs, logs, CA, secret roots) is removed")
		report.INFO("summary", "pass=%d fail=%d duration=%s", report.pass, report.fail, elapsed(begin))
		report.INFO("done", "login spike finished")
	}()

	tokens := intakeTokens()

	var k *keyring
	if keyringInstallFacts() {
		k, err = startKeyring(work)
		if err != nil {
			report.FAIL("1a.keyring", "%v", err)
			k.stop()
			k = nil
		} else {
			k.roundTrip(self)
			inArgs, inEnv := scanProcesses([]byte(k.password))
			report.check("1a.keyring.password", inArgs == 0 && inEnv == 0, "the unlock password is in no process command line or environment (args %d, env %d)", inArgs, inEnv)
		}
	}
	defer func() {
		if k != nil {
			k.stop()
		}
	}()

	stack, err := readStackInputs()
	if !report.check("stack.inputs", err == nil, "the job-owned stack inputs are readable (%v)", errText(err)) {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	err = stack.migrate(ctx)
	cancel()
	if !report.check("stack.migrate", err == nil, "the PM application store is migrated (%v)", errText(err)) {
		return 0
	}
	for _, candidate := range candidates {
		runCandidate(candidate, stack, work, k, pm, pmServer, tokens)
	}
	leakChecks(tokens, work)
	return 0
}

// intakeTokens reads the dummy token that in-ci-image.sh wrote to a 0600 file
// on the runner, removes the file, and reads the environment-channel dummy for
// comparison. Neither value is printed; only a short digest is.
func intakeTokens() tokenIntake {
	intake := tokenIntake{}
	path := os.Getenv("PM_SPIKE_W2_TOKEN_FILE")
	if path == "" {
		report.FAIL("3.channel.file", "PM_SPIKE_W2_TOKEN_FILE is absent")
	} else {
		information, statErr := os.Stat(path)
		value, err := readBoundedFile(path, 4096)
		removeErr := os.Remove(path)
		_, afterErr := os.Stat(path)
		if report.check("3.channel.file.read", err == nil && statErr == nil, "the dummy token file is readable in the job container") {
			report.check("3.channel.file.mode", information.Mode().Perm() == 0o600, "the dummy token file mode is %o", information.Mode().Perm())
			intake.file = value
			report.secret(string(value))
			report.INFO("3.channel.file.digest", "dummy token sha256 prefix %s, length %d", shortDigest(value), len(value))
		}
		report.check("3.channel.file.removed", removeErr == nil && errors.Is(afterErr, os.ErrNotExist), "the spike removed the dummy token file after one read")
		_ = os.Remove(filepath.Dir(path))
	}
	if value := os.Getenv("PM_SPIKE_ENV_CHANNEL"); value != "" {
		intake.env = []byte(value)
		report.secret(value)
		report.INFO("3.channel.env.digest", "environment-channel dummy sha256 prefix %s, length %d", shortDigest(intake.env), len(value))
		_ = os.Unsetenv("PM_SPIKE_ENV_CHANNEL")
	}
	return intake
}

func runCandidate(c candidate, stack stackInputs, work string, k *keyring, pm, pmServer string, tokens tokenIntake) {
	id := c.id
	short := strings.TrimPrefix(id, "2.")
	dir, err := candidateDir(work, short)
	if err != nil {
		report.FAIL(id, "%v", err)
		return
	}
	d, err := newDeployment(stack, work, short)
	if !report.check(id+".deployment", err == nil, "a new deployment with its own CA and HTTPS edge (%v)", errText(err)) {
		return
	}
	defer d.stop()
	reference, _, _, ok := pull(id, c.references)
	if !report.check(id+".pull", ok, "the provider image is pulled from its registry (reference %s)", reference) {
		return
	}
	port, err := freePort()
	if err != nil {
		report.FAIL(id, "no free loopback port")
		return
	}
	issuer := fmt.Sprintf("https://127.0.0.1:%d%s", port, c.issuerPath)
	clientSecret := randomToken(40)
	password := randomToken(32)
	report.secret(clientSecret)
	report.secret(password)
	_, certPEM, keyPEM, err := d.ca.leafWithKey("PM login spike identity provider", true)
	if err != nil {
		report.FAIL(id, "issue the provider certificate")
		return
	}
	argumentsFor, err := c.prepare(dir, port, issuer, d.redirectURI(), clientSecret, password, certPEM, keyPEM)
	if !report.check(id+".config", err == nil, "provider configuration written in a 0700 job directory (%v)", errText(err)) {
		return
	}
	box := &container{name: fmt.Sprintf("pm-login-spike-%s-%s-%s", short, os.Getenv("GITHUB_RUN_ID"), randomHex(3)), logPath: filepath.Join(dir, "container.log")}
	defer box.remove(id)
	readyIn, err := box.start(id, argumentsFor(reference), c.ready, c.readyLimit)
	if !report.check(id+".start", err == nil, "the provider printed its ready line after %.1fs (%v)", readyIn.Seconds(), errText(err)) {
		report.tail(id+".log", box.logPath, 30)
		return
	}
	if short == "keycloak" {
		removeErr := os.Remove(filepath.Join(dir, "pm-realm.json"))
		report.check(id+".import.removed", removeErr == nil, "the realm import file with the user password is removed from the job directory after start")
	}
	box.stats(id)

	facts, ok := clientChecks(id, d, issuer, clientSecret, c.login, password)
	if !ok {
		report.tail(id+".log", box.logPath, 30)
		return
	}
	value := newSeed(d.deploymentID, issuer, facts.subject)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	err = stack.applySeed(ctx, value)
	cancel()
	if !report.check(id+".seed", err == nil, "operator seed rows for this deployment (%v)", errText(err)) {
		return
	}
	secrets, err := d.writeSecrets(clientSecret, d.ca.pem)
	if err != nil {
		report.FAIL(id+".pmserver", "write the secret root: %v", err)
		return
	}
	configuration, err := d.writeConfiguration(issuer)
	if err != nil {
		report.FAIL(id+".pmserver", "write the configuration: %v", err)
		return
	}
	startedAt := time.Now()
	if err := d.start(pmServer, configuration, secrets); err != nil {
		report.FAIL(id+".pmserver", "%v", err)
		return
	}
	err = d.waitReady(90 * time.Second)
	if !report.check(id+".pmserver.ready", err == nil, "pm-server discovered the issuer and became ready in %s (%v)", elapsed(startedAt), errText(err)) {
		report.tail(id+".pmserver.log", d.logPath, 30)
		return
	}

	browser := newPMBrowser(d)
	steps, err := browser.login(d, c.login, password, true)
	reauthenticated := report.check(id+".pm.login.reauth", err == nil, "PM browser login with reauthenticate=true (needs auth_time) succeeds (steps %s, %v)", steps, errText(err))
	if !reauthenticated {
		browser = newPMBrowser(d)
		steps, err = browser.login(d, c.login, password, false)
		if !report.check(id+".pm.login.plain", err == nil, "PM browser login without reauthentication succeeds (steps %s, %v)", steps, errText(err)) {
			report.tail(id+".pmserver.log", d.logPath, 20)
			return
		}
	}
	if k != nil {
		deviceLogin(id, pm, d, browser, k, value.principalID)
	} else {
		report.FAIL(id+".pm.device", "not run: no private keyring")
	}
	if tokens.file != nil {
		response, err := browser.storeCredential(value.workspaceID, value.connectionID, tokens.file)
		stored := err == nil && response.status == 200 && findString(response.body, "status") == "active"
		report.check(id+".w2.ingress", stored, "the credential ingress stores the dummy token from the browser session (status %d code %q, reauthenticated %t)", response.status, response.problemCode(), reauthenticated)
		if stored {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			count, found, err := stack.storedCredential(ctx, value, tokens.file)
			cancel()
			report.check(id+".w2.atrest", err == nil && count >= 1 && !found, "PostgreSQL holds %d ciphertext row(s) and no plaintext dummy (%v)", count, errText(err))
		}
	}
	box.stats(id)
	_ = d.stop()
	for label, path := range map[string]string{"pm-server log": d.logPath, "provider log": box.logPath} {
		hits := 0
		for _, secret := range []string{string(tokens.file), password, clientSecret} {
			if secret == "" {
				continue
			}
			if contents, err := os.ReadFile(path); err == nil && containsAny(contents, []byte(secret)) {
				hits++
			}
		}
		report.check(id+".logs.clean", hits == 0, "the %s holds no dummy token, user password or client secret (hits %d)", label, hits)
	}
}

// leakChecks searches every place the spike can see for the dummy values.
func leakChecks(tokens tokenIntake, work string) {
	if tokens.file != nil {
		inArgs, inEnv := scanProcesses(tokens.file)
		report.check("3.channel.file.proc", inArgs == 0 && inEnv == 0, "the file-channel dummy is in no process command line or environment (args %d, env %d)", inArgs, inEnv)
	}
	if tokens.env != nil {
		inArgs, inEnv := scanProcesses(tokens.env)
		report.check("3.channel.env.proc", inArgs == 0 && inEnv == 0, "the env-channel dummy is in no process command line or environment (args %d, env %d)", inArgs, inEnv)
	}
	name := fmt.Sprintf("pmcli-%s-%s-bdd", os.Getenv("GITHUB_RUN_ID"), os.Getenv("GITHUB_RUN_ATTEMPT"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	record, err := docker(ctx, "inspect", "--format", "{{json .Config.Env}} {{json .Args}} {{json .Config.Cmd}}", name)
	cancel()
	if report.check("3.inspect.read", err == nil, "the job container record is readable through the engine socket (%v)", errText(err)) {
		if tokens.file != nil {
			report.check("3.channel.file.inspect", !containsAny([]byte(record), tokens.file), "the engine's job container record does not hold the file-channel dummy")
		}
		if tokens.env != nil {
			report.check("3.channel.env.inspect", !containsAny([]byte(record), tokens.env), "the engine's job container record does not hold the env-channel dummy")
		}
	}
	manifests, _ := filepath.Glob(filepath.Join(os.Getenv("RUNNER_TEMP"), "pmcli-bdd-*.json"))
	hits := 0
	for _, path := range manifests {
		contents, err := os.ReadFile(path)
		if err == nil && ((tokens.file != nil && containsAny(contents, tokens.file)) || (tokens.env != nil && containsAny(contents, tokens.env))) {
			hits++
		}
	}
	report.check("3.manifest.clean", hits == 0, "the stack fixture manifest holds no dummy (files %d, hits %d)", len(manifests), hits)
	printed := report.printedCopy()
	clean := (tokens.file == nil || !containsAny(printed, tokens.file)) && (tokens.env == nil || !containsAny(printed, tokens.env))
	report.check("3.output.clean", clean, "the spike output holds no dummy value")
	report.check("output.scrub", report.scrubHits == 0, "no printed line needed redaction (redactions %d)", report.scrubHits)
}
