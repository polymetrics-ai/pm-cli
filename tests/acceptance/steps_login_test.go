package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cucumber/godog"
)

// The login steps of slice W1a (plan item #72, plan v5 row 18). They log in
// the deployment user of the lane through the product login flow (LOGIN
// proposal v3, decided): a browser-equivalent login through PM and the job
// Keycloak with one shared cookie jar (D1-12), then the device login of the
// real pm binary, approved by that browser session (D4-11). pm stores the PM
// session in the private keyring of the scenario, so that each later pm
// command of the scenario uses it.
//
// The Keycloak password, the browser cookies, the CSRF token, the device
// proof and the user code stay in memory. No step writes them to a file, an
// argument, an environment variable or the log. A failure message shows
// process output and answers only by their form (describeSafeLines,
// describeProblem, keycloakMessage).

// laneLoginDeviceID is the device ID of the device login of the login step.
// It is not a secret.
const laneLoginDeviceID = "acceptance-device"

// pmCookiePrefix is the prefix of each PM cookie: __Host-pm_session,
// __Host-pm_csrf and __Host-pm_oidc (internal/httpapi/service.go:29-31).
const pmCookiePrefix = "__Host-pm_"

// providerRecorder is the transport of the browser session. It sees each
// request after the client added the cookies of the jar, and before the
// request leaves. For each request to the Keycloak origin, it keeps a count
// and the names of the PM cookies that the request carried. It keeps no
// cookie value. So a scenario observes every provider-bound request of the
// login and the re-login, not only the final jar (D1-12, finding R2-4).
type providerRecorder struct {
	origin string
	next   http.RoundTripper

	mu        sync.Mutex
	requests  int
	pmCookies []string
}

func newProviderRecorder(issuer string, next http.RoundTripper) (*providerRecorder, error) {
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("the Keycloak issuer is not an absolute URL")
	}
	return &providerRecorder{origin: strings.ToLower(parsed.Scheme + "://" + parsed.Host), next: next}, nil
}

// RoundTrip records the request when it goes to the Keycloak origin, and
// then sends it with the next transport.
func (r *providerRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL != nil && strings.ToLower(request.URL.Scheme+"://"+request.URL.Host) == r.origin {
		var names []string
		for _, cookie := range request.Cookies() {
			if strings.HasPrefix(cookie.Name, pmCookiePrefix) {
				names = append(names, cookie.Name)
			}
		}
		r.mu.Lock()
		r.requests++
		r.pmCookies = append(r.pmCookies, names...)
		r.mu.Unlock()
	}
	return r.next.RoundTrip(request)
}

// check fails when a provider-bound request carried a PM cookie, and when no
// provider-bound request was recorded, because then the check observed
// nothing. Its message names the cookies, never their values.
func (r *providerRecorder) check() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.requests == 0 {
		return errors.New("the browser sent no request to the Keycloak origin, so the PM cookie check observed nothing")
	}
	if len(r.pmCookies) > 0 {
		names := slices.Compact(slices.Sorted(slices.Values(r.pmCookies)))
		return fmt.Errorf("%d of the PM cookies %s reached the Keycloak origin, in %d provider-bound requests (D1-12)",
			len(r.pmCookies), strings.Join(names, ", "), r.requests)
	}
	return nil
}

// loginScenario is the state of the W1a login steps in one scenario.
type loginScenario struct {
	shared   *stackScenario
	lane     *lane
	browser  *browserSession
	loggedIn bool
}

// registerLoginSteps registers the W1a login steps (criteria-worker v4, step
// list lines 553 and 554, and LOGIN proposal v3, D1-12). Two phrases are
// proposals until the W1a RED names its own text: the re-login and the
// cookie check.
func registerLoginSteps(sc *godog.ScenarioContext, shared *stackScenario, l *lane) {
	s := &loginScenario{shared: shared, lane: l}
	// godog keeps the step error. The hook returns only its own cleanup
	// error, so the step error is reported once.
	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		return ctx, s.close()
	})
	sc.Step(`^the user is logged in through the product login flow$`, s.logIn)
	sc.Step(`^the user logs in again through the product login flow with a fresh login$`, s.logInAgain)
	sc.Step(`^no request to the identity provider during the login carried a PM cookie$`, s.noPMCookieToProvider)
	sc.Step(`^I run "([^"]*)" as the user$`, s.runAsUser)
}

// logIn is the step "the user is logged in through the product login flow".
// It uses the lane deployment and its Keycloak user, which the operator
// bound with pm-server bootstrap. It adds the lane server profile, with the
// scenario HOME and private keyring, when the scenario has none. Then:
//  1. The browser session logs the user in through PM, in one shared jar.
//     No provider-bound request may carry a PM cookie (D1-12).
//  2. pm sessions login-start, with its stdout in memory only (D4-11).
//  3. The browser session approves the device login.
//  4. pm sessions login-exchange --wait=true, with the device proof on
//     stdin. The step waits once for its exit, with one limit. pm follows
//     the server cadence itself (D4-4) and stores the session.
func (s *loginScenario) logIn(ctx context.Context) error {
	if s.browser != nil {
		return errors.New("the user already logged in in this scenario")
	}
	if err := s.lane.ensure(ctx); err != nil {
		return err
	}
	d := s.lane.deployment
	if !s.shared.hasProfile(laneProfileName) {
		if err := s.shared.addServerProfile(ctx, laneProfileName, d); err != nil {
			return err
		}
	}
	b, err := newBrowserSession(d)
	if err != nil {
		return err
	}
	s.browser = b
	if err := b.login(ctx, false); err != nil {
		return fmt.Errorf("the browser login of the user through PM: %w", err)
	}
	if err := b.recorder.check(); err != nil {
		return err
	}
	environment, err := s.environment()
	if err != nil {
		return err
	}
	code, stdout, stderr, err := runPMProcess(ctx, s.shared.harness.bins.pm, []string{"sessions", "login-start", "--device-id", laneLoginDeviceID}, environment)
	if err != nil {
		return err
	}
	challenge, err := checkLoginStart(code, stdout, stderr)
	if err != nil {
		return err
	}
	defer clear(challenge.stdout)
	if err := decideDeviceLogin(ctx, b, challenge, "approves"); err != nil {
		return err
	}
	if err := runDeviceExchange(ctx, s.shared.harness.bins.pm, []string{"sessions", "login-exchange", "--wait=true"}, environment, challenge); err != nil {
		return err
	}
	s.loggedIn = true
	godog.Logf(ctx, "login: the lane user logged in through PM in one browser jar, and pm stored the device session in the private keyring")
	return nil
}

// logInAgain is the re-login: the browser session logs the user in again,
// with a fresh Keycloak login, in the same jar. The recorder keeps
// observing, and no provider-bound request may carry a PM cookie (D1-12).
func (s *loginScenario) logInAgain(ctx context.Context) error {
	if s.browser == nil || !s.loggedIn {
		return errors.New("the user is not logged in in this scenario")
	}
	if err := s.lane.ensure(ctx); err != nil {
		return err
	}
	if err := s.browser.login(ctx, true); err != nil {
		return fmt.Errorf("the browser re-login of the user through PM: %w", err)
	}
	return s.browser.recorder.check()
}

// noPMCookieToProvider checks every provider-bound request of the login and
// the re-login of this scenario (D1-12).
func (s *loginScenario) noPMCookieToProvider() error {
	if s.browser == nil {
		return errors.New("the user did not log in through the browser in this scenario")
	}
	return s.browser.recorder.check()
}

// runAsUser is the step `I run "<command>" as the user`. It runs the command
// line of the feature text unchanged against the lane server. It adds no
// flag (criteria-worker v4 wait-spelling addendum, line 9). It selects the
// lane profile with PM_SERVER_PROFILE, and pm uses its default os session
// store: the PM session that the login step stored in the private keyring.
// It writes the shared result state.
func (s *loginScenario) runAsUser(ctx context.Context, line string) error {
	if !s.loggedIn {
		return errors.New("the user is not logged in in this scenario")
	}
	if err := s.lane.ensure(ctx); err != nil {
		return err
	}
	args, err := commandArguments(line)
	if err != nil {
		return err
	}
	environment, err := s.environment()
	if err != nil {
		return err
	}
	code, stdout, stderr, err := runPMProcess(ctx, s.shared.harness.bins.pm, args, environment)
	if err != nil {
		return err
	}
	s.shared.result = authenticatedResult(code, stdout, stderr, s.lane.deployment.deploymentID)
	return nil
}

// authenticatedResult is the shared result state of a command that ran with
// the user's PM session. The shared exit code step shows the stdout and the
// stderr of a result when it fails, so they keep only text that is safe by
// its form (storedSessionResult). The raw stdout stays in privateStdout for
// the result oracles only (#402 review F1).
func authenticatedResult(code int, stdout, stderr, deploymentID string) commandResult {
	result := storedSessionResult(code, stdout, stderr, deploymentID)
	result.authenticated = true
	result.privateStdout = stdout
	return result
}

// environment is the environment of each pm process of the login steps: the
// scenario HOME, the lane deployment CA as the only trusted root, the lane
// profile and the private keyring of the scenario.
func (s *loginScenario) environment() ([]string, error) {
	if s.shared.home == "" || s.shared.keyring == nil || s.lane.deployment == nil {
		return nil, errors.New("the lane server profile and the private keyring are not set")
	}
	environment := []string{
		"HOME=" + s.shared.home, "PATH=/usr/bin:/bin", "LANG=C",
		"SSL_CERT_FILE=" + s.lane.deployment.caFile,
		"PM_SERVER_PROFILE=" + laneProfileName,
	}
	return append(environment, s.shared.keyring.Environment()...), nil
}

// close closes the browser connections and drops the browser session.
func (s *loginScenario) close() error {
	if s.browser != nil && s.browser.client != nil {
		s.browser.client.CloseIdleConnections()
	}
	s.browser, s.loggedIn = nil, false
	return nil
}

// runDeviceExchange starts pm sessions login-exchange with the arguments and
// --challenge-id from the login-start output. It writes the device proof and
// one line feed to the stdin pipe and closes the pipe (D4-10, D4-13). One
// deadline bounds the write, the exit and the end of the output: the exit
// limit of the command line, or the end of the parent context if that comes
// first (#402 review F2). The write runs in a goroutine. At the deadline the
// step kills the process, joins the output readers, closes the pipe and joins
// the write before it returns. Each other early return also joins the output
// readers (stopExchangeProcess). It needs exit code 0. A failure message
// shows the output by its form only.
func runDeviceExchange(parent context.Context, binary string, args, environment []string, challenge *deviceChallenge) error {
	exitLimit, err := exchangeExitLimit(args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, exitLimit)
	defer cancel()
	args = append(slices.Clone(args), "--challenge-id", challenge.challengeID)
	process, err := startExchangeProcess(binary, args, environment)
	if err != nil {
		return err
	}
	writer, err := process.takeStdin()
	if err != nil {
		return errors.Join(err, stopExchangeProcess(process))
	}
	written := make(chan error, 1)
	go func() { written <- writeProof(writer, challenge.proof) }()
	select {
	case err := <-written:
		if err != nil {
			return errors.Join(
				fmt.Errorf("write the device proof to the login-exchange stdin pipe and close the pipe: %s", pipeCategory(err)),
				stopExchangeProcess(process))
		}
	case <-ctx.Done():
		stopErr := stopExchangeProcess(process)
		_ = writer.Close()
		<-written
		return errors.Join(
			fmt.Errorf("the device proof write to pm sessions login-exchange did not end within %s or before the step ended. %s", exitLimit, process.describeStatus()),
			stopErr)
	}
	if !process.awaitEndUntil(ctx.Done()) {
		return errors.Join(
			fmt.Errorf("pm sessions login-exchange did not end within %s or before the step ended. %s\n%s", exitLimit, process.describeStatus(), describeOutputText("stderr", &process.stderr)),
			stopExchangeProcess(process))
	}
	if !process.status.exited || process.status.code != 0 {
		return fmt.Errorf("pm sessions login-exchange: %s, want exit code 0\n%s", process.describeStatus(), describeOutputText("stderr", &process.stderr))
	}
	return nil
}

// stopExchangeProcess ends a login-exchange process that is still running,
// with SIGKILL, and waits for its exit with the kill limit. Then it joins the
// output readers (#402 review R2-F1).
func stopExchangeProcess(p *exchangeProcess) error {
	if p.stdin != nil {
		_ = p.stdin.Close()
		p.stdin = nil
	}
	return errors.Join(killExchangeProcess(p), p.joinOutput())
}

// killExchangeProcess sends SIGKILL to a login-exchange process that is
// still running and waits for its exit with the kill limit.
func killExchangeProcess(p *exchangeProcess) error {
	select {
	case <-p.exited:
		return nil
	default:
	}
	if err := p.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("end the device login exchange: %w", err)
	}
	timer := time.NewTimer(killLimit)
	defer timer.Stop()
	select {
	case <-p.exited:
		return nil
	case <-timer.C:
		return fmt.Errorf("the device login exchange did not end within %s after SIGKILL", killLimit)
	}
}

// joinOutput waits for the end of the output with the kill limit. A process
// that pm started can keep the output pipes open after pm ends. Then
// joinOutput closes the read ends, joins the readers and fails. The close
// ends the blocked reads: os.File.Close cancels the pending I/O of a pipe.
func (p *exchangeProcess) joinOutput() error {
	timer := time.NewTimer(killLimit)
	defer timer.Stop()
	select {
	case <-p.outputEnded:
		return nil
	case <-timer.C:
	}
	for _, reader := range p.readEnds {
		_ = reader.Close()
	}
	<-p.outputEnded
	return fmt.Errorf("a process that pm sessions login-exchange started kept the output open for %s after the stop. The step closed the read ends", killLimit)
}

// namesBootstrappedWorkspace is the K1b step "the lane command stdout names
// the bootstrapped workspace and no other workspace" (criteria-worker v4,
// line 558). It reads the workspace page of pm workspaces list from the
// shared result state and compares it with the input of the bootstrap of the
// scenario.
func (s *bootstrapScenario) namesBootstrappedWorkspace() error {
	if s.first == nil {
		return errors.New("the operator did not bootstrap the deployment in this scenario")
	}
	result := s.shared.result
	if !result.ran {
		return errors.New("no lane command ran")
	}
	if !result.authenticated {
		return errors.New("the last lane command did not run as the user")
	}
	return checkOnlyWorkspace(result.privateStdout, s.first.OrgID, s.first.WorkspaceID)
}

// workspaceItem is the part of one workspace of the page that the check
// reads (api/pm.openapi.yaml, Workspace).
type workspaceItem struct {
	OrgID       string `json:"org_id"`
	WorkspaceID string `json:"workspace_id"`
}

// checkOnlyWorkspace needs exactly one workspace on the page, with the
// organization and workspace IDs, and no next page. So another workspace on
// this page or on a later page fails the check. The stdout is the output of
// an authenticated command, so a failure message shows only its byte count
// and fixed text, never the output (#402 review F1).
func checkOnlyWorkspace(stdout, orgID, workspaceID string) error {
	items, nextCursor, err := readWorkspacePage(stdout)
	if err != nil {
		return fmt.Errorf("read the workspace page from the lane command stdout (%d bytes, not shown): %w", len(stdout), err)
	}
	if string(nextCursor) != "null" {
		return fmt.Errorf("the workspace page has a next page, so it can hold another workspace (stdout: %d bytes, not shown)", len(stdout))
	}
	if len(items) != 1 || items[0].WorkspaceID != workspaceID || items[0].OrgID != orgID {
		return fmt.Errorf("the workspace page names %d workspaces, want only the bootstrapped workspace %q of organization %q (stdout: %d bytes, not shown)",
			len(items), workspaceID, orgID, len(stdout))
	}
	return nil
}

// readWorkspacePage reads the workspace page that pm prints: one JSON object
// (JSON output), or exactly one "items<TAB>JSON value" line and one
// "next_cursor<TAB>JSON value" line (text output, internal/cli/render.go).
func readWorkspacePage(stdout string) ([]workspaceItem, json.RawMessage, error) {
	var page struct {
		Items      *[]workspaceItem `json:"items"`
		NextCursor json.RawMessage  `json:"next_cursor"`
	}
	if strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		if err := json.Unmarshal([]byte(stdout), &page); err != nil || page.Items == nil || page.NextCursor == nil {
			return nil, nil, errors.New("the stdout is not a workspace page with items and next_cursor")
		}
		return *page.Items, page.NextCursor, nil
	}
	fields := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
		key, value, ok := strings.Cut(line, "\t")
		if !ok || (key != "items" && key != "next_cursor") {
			return nil, nil, errors.New("the stdout has a line that is not an items or next_cursor field")
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, nil, fmt.Errorf("the stdout has the %s field more than once", key)
		}
		fields[key] = value
	}
	if len(fields) != 2 {
		return nil, nil, errors.New("the stdout does not have both the items and the next_cursor field")
	}
	var items []workspaceItem
	if err := json.Unmarshal([]byte(fields["items"]), &items); err != nil || items == nil {
		return nil, nil, errors.New("the items field is not a JSON array of workspaces")
	}
	var cursor json.RawMessage
	if err := json.Unmarshal([]byte(fields["next_cursor"]), &cursor); err != nil {
		return nil, nil, errors.New("the next_cursor field is not a JSON value")
	}
	return items, cursor, nil
}
