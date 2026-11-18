package acceptance

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cucumber/godog"
)

// The steps of the device login wait (plan item L-d, issue #325, criteria D4
// of the login proposal). They run the real pm binary against the pm-server
// that the starter started, and a browser session of the deployment's
// Keycloak user through PM.
//
// The device proof, the user code, the browser cookies, the CSRF token and
// the Keycloak password stay in memory. No step writes them to a file, an
// argument, an environment variable or the log (D4-10, D4-11). A failure
// message shows process output and answers only by their form, so it can
// hold no secret, host or address that the step does not know (review
// findings F2 and F3).
const (
	// deviceLoginNoticeLimit bounds the wait for the first stderr line of
	// login-exchange. The command writes the notice line after the answer to
	// its first exchange request. --request-timeout bounds that request, and
	// its default is 30 s (internal/cli/config.go:16). 60 s holds the process
	// start, the stdin read and a full 30 s request.
	deviceLoginNoticeLimit = laneCommandTimeout
	// deviceLoginDefaultRequestTimeout is the --request-timeout default of
	// the CLI (internal/cli/config.go:16).
	deviceLoginDefaultRequestTimeout = 30 * time.Second
	// deviceLoginExitMargin is added to the --request-timeout of the started
	// command line for the exit limit (exchangeExitLimit).
	deviceLoginExitMargin = 30 * time.Second
	// deviceLoginOutputLimit bounds each output stream that the step keeps.
	deviceLoginOutputLimit = laneOutputLimit

	browserSessionCookieName = "__Host-pm_session"
	browserCSRFCookieName    = "__Host-pm_csrf"
)

// deviceLoginNoticeLine is the notice line N1 of L-d, without its line feed.
// Group 1 is the answer code and group 2 is the wait in seconds.
var deviceLoginNoticeLine = regexp.MustCompile(`^notice: device login exchange received (device_authorization_pending|device_poll_too_soon)\. The CLI waits ([1-9][0-9]*)s before the next exchange request\.$`)

// deviceChallenge is the login-start output that the steps keep in memory.
// The proof and the user code are secrets.
type deviceChallenge struct {
	challengeID string
	userCode    string
	proof       string
	// stdout is the whole login-start stdout, kept in memory for the step
	// that reads its poll interval. It holds the proof. close clears it.
	stdout []byte
}

func (deviceChallenge) String() string   { return "device challenge [REDACTED]" }
func (deviceChallenge) GoString() string { return "device challenge [REDACTED]" }

// browserSession is the PM browser session of the deployment's Keycloak
// user: one HTTP client with one cookie jar for PM and Keycloak, as a browser
// has (D1-12). It holds secrets.
type browserSession struct {
	client   *http.Client
	jar      http.CookieJar
	recorder *providerRecorder
	origin   string
	issuer   string
	username string
	password string
	code     string
	state    string
	csrf     string
	session  string
}

func (browserSession) String() string   { return "browser session [REDACTED]" }
func (browserSession) GoString() string { return "browser session [REDACTED]" }

// boundedOutput keeps at most deviceLoginOutputLimit bytes of one output
// stream and counts every byte.
type boundedOutput struct {
	mu    sync.Mutex
	data  []byte
	total int64
}

func (o *boundedOutput) write(chunk []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.total += int64(len(chunk))
	if room := deviceLoginOutputLimit - len(o.data); room > 0 {
		o.data = append(o.data, chunk[:min(room, len(chunk))]...)
	}
}

// text returns the kept bytes, and false when the stream was longer than
// the bound.
func (o *boundedOutput) text() (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return string(o.data), o.total == int64(len(o.data))
}

// firstLine is the first stderr line of login-exchange, without its line
// feed. found is false when stderr ended, or the bound passed, before a line
// feed.
type firstLine struct {
	line  string
	found bool
}

// processStatus is the end of the login-exchange process.
type processStatus struct {
	exited bool
	code   int
	text   string
}

// exchangeProcess is the running pm sessions login-exchange process. It is a
// direct child of the step, with no shell, so that SIGINT reaches it. Its
// stdin, stdout and stderr are pipes that the step owns. Cmd.Wait copies
// nothing, so the wait ends with the exit of the process.
type exchangeProcess struct {
	command *exec.Cmd
	// stdin is the write end of the stdin pipe until the step writes the
	// proof and closes it.
	stdin       *os.File
	stdout      boundedOutput
	stderr      boundedOutput
	first       chan firstLine
	outputEnded chan struct{}
	exited      chan struct{}
	status      processStatus
	// exitLimit bounds the wait of the exit step: the --request-timeout of
	// the started command line plus deviceLoginExitMargin.
	exitLimit time.Duration
}

// deviceLoginScenario is the state of the device login steps in one
// scenario.
type deviceLoginScenario struct {
	shared    *stackScenario
	browser   *browserSession
	challenge *deviceChallenge
	startedAt time.Time
	exchange  *exchangeProcess
}

// registerDeviceLoginSteps registers the device login steps. Call it before
// the stack steps register their After hook: godog runs the After hooks in
// the order of registration, so this hook ends a running pm process before
// the stack steps stop pm-server and the private keyring.
func registerDeviceLoginSteps(sc *godog.ScenarioContext, shared *stackScenario) {
	s := &deviceLoginScenario{shared: shared}
	// godog keeps the step error. The hook returns only its own cleanup
	// error, so the step error is reported once.
	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		return ctx, s.close()
	})
	sc.Step(`^a PM browser session of the deployment's Keycloak user$`, s.browserLogin)
	sc.Step(`^I run "([^"]*)" against the started pm-server and keep its stdout in memory$`, s.loginStart)
	sc.Step(`^I start "([^"]*)" against the started pm-server with the device proof on stdin$`, s.startExchange)
	sc.Step(`^the device login exchange writes the wait notice line(?: for (device_poll_too_soon))? to stderr$`, s.noticeLine)
	sc.Step(`^the device login exchange writes the wait notice line for device_poll_too_soon with a wait of at least (\d+) seconds to stderr$`, s.noticeLineWithWait)
	sc.Step(`^the device login challenge states a poll interval of (\d+) seconds$`, s.challengePollInterval)
	sc.Step(`^the browser session (approves|denies) the device login$`, s.decide)
	sc.Step(`^I send SIGINT to the device login exchange$`, s.sendSIGINT)
	sc.Step(`^the device login exchange exits with code (\d+)$`, s.exitsWithCode)
	sc.Step(`^the device login exchange process has ended$`, s.processEnded)
	sc.Step(`^the device login exchange stderr names (device_authorization_denied)$`, s.stderrNames)
	sc.Step(`^I run "([^"]*)" against the started pm-server with the stored PM session$`, s.runWithStoredSession)
}

// environment is the environment of each pm process of these steps: the
// scenario HOME, the deployment CA as the only trusted root, the private
// keyring of the scenario, and PM_SERVER_PROFILE for the profile of the
// started pm-server. The steps select the profile with the environment, so
// that they add no flag to a command line of the feature text.
func (s *deviceLoginScenario) environment() ([]string, error) {
	if s.shared.deployment == nil {
		return nil, errors.New("the scenario has no deployment configuration")
	}
	if s.shared.home == "" {
		return nil, errors.New("the pm server profile is not set")
	}
	environment := []string{
		"HOME=" + s.shared.home, "PATH=/usr/bin:/bin", "LANG=C",
		"SSL_CERT_FILE=" + s.shared.deployment.caFile,
		"PM_SERVER_PROFILE=" + startedServerProfile,
	}
	if s.shared.keyring != nil {
		environment = append(environment, s.shared.keyring.Environment()...)
	}
	return environment, nil
}

// knownDeviceLoginLines are the stderr lines that a failure message can show
// whole. Each is a fixed text of L-d (the texts E1 to E6 of the L-d PR text)
// or of the CLI, with no value in it.
var knownDeviceLoginLines = []string{
	"error: device login wait stopped. The server answer did not have exactly one Retry-After value from 1 to 60 seconds.",
	"error: device login exchange outcome is unknown. The exchange request may have reached the server, but the CLI received no valid answer. Start a new device login with pm sessions login-start.",
	"error: device login exchange succeeded, but the CLI did not store the PM session. Start a new device login with pm sessions login-start.",
	"error: device login was cancelled before the next exchange request. The CLI sent no request after the cancellation.",
	"error: device login was cancelled while the CLI read the proof from stdin. The CLI sent no exchange request.",
	"error: device login wait stopped. The next exchange request does not fit in the --request-timeout limit. Run the command again with a longer --request-timeout.",
	"error: view deadline exceeded",
}

// problemLine is the form of a CLI problem line:
// "error: <code>: <detail> (incident <id>)". A failure message shows only
// its code and its incident ID, never the detail. The code has only
// lowercase letters and underscores. The incident ID is "pm-" and 26 base32
// characters (internal/api/protocol.go:714). The device proof starts with
// "pm_device_" and the user code with "PM-", and each holds base64url
// characters (internal/auth/device.go:56-61). So neither can have the form
// of the incident ID, and describeSafeLine refuses a code that starts with
// "pm_device_".
var problemLine = regexp.MustCompile(`^error: ([a-z_]{1,64}): .*\(incident (pm-[A-Z2-7]{26})\)$`)

// describedLineLimit is the number of lines that a failure message
// describes for one output stream.
const describedLineLimit = 16

// describeSafeLines describes one process output stream for a failure
// message. It shows a line only by its form: the notice line N1 and the
// fixed lines whole, a problem line as its code and incident ID, and every
// other line as its byte count. So the message cannot hold a secret that the
// step does not know, such as a proof that login-start printed before the
// step read it, or a PM session credential. A stream that was longer than
// its bound is not described.
func describeSafeLines(name, text string, whole bool) string {
	if !whole {
		return fmt.Sprintf("%s: more than %d bytes, not shown", name, deviceLoginOutputLimit)
	}
	if text == "" {
		return name + ": 0 bytes"
	}
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	parts := make([]string, 0, min(len(lines), describedLineLimit+1))
	for index, raw := range lines {
		if index == describedLineLimit {
			parts = append(parts, fmt.Sprintf("%d more lines, not shown", len(lines)-index))
			break
		}
		line, complete := strings.CutSuffix(raw, "\n")
		parts = append(parts, describeSafeLine(line, complete))
	}
	return fmt.Sprintf("%s (%d bytes, line count %d, shown by form only):\n  %s", name, len(text), len(lines), strings.Join(parts, "\n  "))
}

// describeSafeLine describes one output line by its form.
func describeSafeLine(line string, complete bool) string {
	suffix := ""
	if !complete {
		suffix = " (no line feed)"
	}
	if deviceLoginNoticeLine.MatchString(line) || slices.Contains(knownDeviceLoginLines, line) {
		return fmt.Sprintf("%q%s", line, suffix)
	}
	if match := problemLine.FindStringSubmatch(line); match != nil && !strings.HasPrefix(match[1], "pm_device_") {
		return fmt.Sprintf("a problem line with code %s and incident %s%s", match[1], match[2], suffix)
	}
	return fmt.Sprintf("a line of %d bytes, not shown%s", len(line), suffix)
}

// describeOutputText describes the kept text of one output stream by form.
func describeOutputText(name string, output *boundedOutput) string {
	text, whole := output.text()
	return describeSafeLines(name, text, whole)
}

// runPM runs one pm process with the environment of these steps and waits
// once for its exit, with the limit of one lane command.
func (s *deviceLoginScenario) runPM(parent context.Context, args []string) (int, string, string, error) {
	environment, err := s.environment()
	if err != nil {
		return 0, "", "", err
	}
	return runPMProcess(parent, s.shared.harness.bins.pm, args, environment)
}

// runPMProcess runs one pm process with the environment and waits once for
// its exit and the end of its output. The limit of one lane command bounds
// the run. A process that pm started can keep the output pipes open after pm
// exits or is killed. The wait then ends at most killLimit later
// (Cmd.WaitDelay): it closes the pipes and fails (#402 review F3). An error
// never shows the output.
func runPMProcess(parent context.Context, binary string, args, environment []string) (int, string, string, error) {
	ctx, cancel := context.WithTimeout(parent, laneCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = environment
	command.WaitDelay = killLimit
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	runErr := command.Run()
	if ctx.Err() != nil {
		return 0, "", "", fmt.Errorf("pm did not finish within %s: %q", laneCommandTimeout, args)
	}
	if errors.Is(runErr, exec.ErrWaitDelay) {
		return 0, "", "", fmt.Errorf("pm exited, but a process that it started kept the output open for %s after the exit: %q", killLimit, args)
	}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		return 0, stdout.String(), stderr.String(), nil
	case errors.As(runErr, &exitErr):
		return exitErr.ExitCode(), stdout.String(), stderr.String(), nil
	default:
		return 0, "", "", errors.New("start pm: the pm binary did not start")
	}
}

// commandArguments splits a command line of the feature text on spaces. The
// line must start with "pm" and the given subcommand.
func commandArguments(line string, subcommand ...string) ([]string, error) {
	args := strings.Fields(line)
	if len(args) < 1+len(subcommand) || args[0] != "pm" {
		return nil, fmt.Errorf("command must start with pm %s: %q", strings.Join(subcommand, " "), line)
	}
	for index, word := range subcommand {
		if args[1+index] != word {
			return nil, fmt.Errorf("command must start with pm %s: %q", strings.Join(subcommand, " "), line)
		}
	}
	return args[1:], nil
}

// browserLogin is the step "a PM browser session of the deployment's
// Keycloak user". It logs in the deployment user through PM with one HTTP
// client and one cookie jar (login).
func (s *deviceLoginScenario) browserLogin(ctx context.Context) error {
	d := s.shared.deployment
	if d == nil {
		return errors.New("the scenario has no deployment configuration")
	}
	if _, err := s.shared.started("pm-server"); err != nil {
		return err
	}
	if s.browser != nil {
		return errors.New("the scenario already has a browser session")
	}
	b, err := newBrowserSession(d)
	if err != nil {
		return err
	}
	s.browser = b
	if err := b.login(ctx, false); err != nil {
		return err
	}
	godog.Logf(ctx, "browser session: the deployment's Keycloak user logged in through PM")
	return nil
}

// newBrowserSession makes the browser of the deployment's Keycloak user: one
// HTTP client with one cookie jar for PM and Keycloak, as a browser has
// (D1-12). It trusts only the deployment CA and the Keycloak CA. Its
// transport records every request to the Keycloak origin (providerRecorder).
// It sends no request.
func newBrowserSession(d *deployment) (*browserSession, error) {
	if d.identity == nil || d.identity.Username == "" || d.identity.Password == "" || d.stack.identity.admin == nil {
		return nil, errors.New("the deployment has no user in the job Keycloak")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(d.authority.pem) || !roots.AppendCertsFromPEM(d.stack.identity.ca) {
		return nil, errors.New("the browser cannot read the deployment CA or the Keycloak CA")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create the browser cookie jar: %w", err)
	}
	issuer := d.stack.identity.admin.Issuer()
	recorder, err := newProviderRecorder(issuer, &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
	})
	if err != nil {
		return nil, err
	}
	return &browserSession{
		client: &http.Client{
			Transport: recorder,
			Jar:       jar,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		jar:      jar,
		recorder: recorder,
		origin:   d.origin,
		issuer:   issuer,
		username: d.identity.Username,
		password: d.identity.Password,
	}, nil
}

// login logs the user in through PM: POST /v1/sessions/browser/challenges,
// the Keycloak login form, and POST /v1/sessions/browser with the code and
// the state of the Keycloak redirect to the callback path. It sends no
// request to the callback path itself. pm-server has no handler for it, and a
// browser front end sends the code and the state to POST /v1/sessions/browser.
// With reauthenticate, PM asks Keycloak for a new login with max_age=0 and
// prompt=login (internal/auth/oidc.go:113-119), so Keycloak shows its form
// again, also when the jar holds a Keycloak session. Each request uses the
// same jar.
func (b *browserSession) login(ctx context.Context, reauthenticate bool) error {
	// 1. PM starts the OIDC login and sets its challenge cookie.
	start, err := json.Marshal(map[string]bool{"reauthenticate": reauthenticate})
	if err != nil {
		return fmt.Errorf("encode the browser login start: %w", err)
	}
	status, _, body, err := b.send(ctx, operationBrowserChallenge, http.MethodPost, b.origin+"/v1/sessions/browser/challenges", start, jsonHeaders(b.origin))
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("the %s request returned status %d, want 201: %s", operationBrowserChallenge, status, describeProblem(body))
	}
	var challenge struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	if err := json.Unmarshal(body, &challenge); err != nil || !strings.HasPrefix(challenge.AuthorizationURL, b.issuer+"/protocol/openid-connect/auth?") {
		return errors.New("the browser login challenge has no authorization URL of the job Keycloak")
	}

	// 2. The Keycloak login form.
	status, _, page, err := b.send(ctx, operationKeycloakForm, http.MethodGet, challenge.AuthorizationURL, nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("the %s request returned status %d, want 200 with the login form: %s", operationKeycloakForm, status, keycloakMessage(page))
	}
	action, err := loginFormAction(page, b.issuer)
	if err != nil {
		return fmt.Errorf("%w: %s", err, keycloakMessage(page))
	}

	// 3. The credentials of the deployment user. Keycloak redirects to the
	// callback path with the code and the state.
	form := url.Values{"username": {b.username}, "password": {b.password}}
	encoded := []byte(form.Encode())
	status, header, page, err := b.send(ctx, operationKeycloakSubmit, http.MethodPost, action, encoded,
		http.Header{"Content-Type": {"application/x-www-form-urlencoded"}})
	clear(encoded)
	if err != nil {
		return err
	}
	if status != http.StatusFound && status != http.StatusSeeOther {
		return fmt.Errorf("the %s request returned status %d, want a redirect to the PM callback: %s", operationKeycloakSubmit, status, keycloakMessage(page))
	}
	if b.code, b.state, err = callbackCodeAndState(header.Get("Location"), b.origin); err != nil {
		return err
	}

	// 4. PM completes the login and sets the session and CSRF cookies.
	complete, err := json.Marshal(map[string]string{"code": b.code, "state": b.state})
	if err != nil {
		return fmt.Errorf("encode the browser login completion: %w", err)
	}
	status, _, body, err = b.send(ctx, operationBrowserSession, http.MethodPost, b.origin+"/v1/sessions/browser", complete, jsonHeaders(b.origin))
	clear(complete)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("the %s request returned status %d, want 204: %s", operationBrowserSession, status, describeProblem(body))
	}
	originURL, err := url.Parse(b.origin)
	if err != nil {
		return errors.New("the deployment origin is not a URL")
	}
	b.session, b.csrf = "", ""
	for _, cookie := range b.jar.Cookies(originURL) {
		switch cookie.Name {
		case browserSessionCookieName:
			b.session = cookie.Value
		case browserCSRFCookieName:
			b.csrf = cookie.Value
		}
	}
	if b.session == "" || b.csrf == "" {
		return errors.New("the browser login did not set the PM session cookie and the CSRF cookie")
	}
	return nil
}

// jsonHeaders are the headers of a PM browser request with a JSON body.
func jsonHeaders(origin string) http.Header {
	return http.Header{"Content-Type": {"application/json"}, "Origin": {origin}}
}

// The fixed names of the browser requests. A failure message names a request
// only by its name, and a PM request also by its API path: never by a
// scheme, a host, a port or a query (review finding F3).
const (
	operationBrowserChallenge = "PM browser challenge (POST /v1/sessions/browser/challenges)"
	operationKeycloakForm     = "Keycloak login form"
	operationKeycloakSubmit   = "Keycloak login submit"
	operationBrowserSession   = "PM browser session (POST /v1/sessions/browser)"
	operationDeviceApprove    = "device approve (POST /v1/sessions/device/approve)"
	operationDeviceDeny       = "device deny (POST /v1/sessions/device/deny)"
)

// errAnswerTooLarge is the failure of an answer that is larger than its
// bound.
var errAnswerTooLarge = errors.New("answer too large")

// send sends one request with the browser client, with one limit and no
// redirect, and reads at most laneFileLimit bytes of the answer. An error
// names the operation and a fixed transport category, never the target or
// the raw transport error, which can hold a host or an address.
func (b *browserSession) send(parent context.Context, operation, method, target string, body []byte, header http.Header) (int, http.Header, []byte, error) {
	ctx, cancel := context.WithTimeout(parent, laneCommandTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("the %s request could not be made", operation)
	}
	for name, values := range header {
		request.Header[name] = values
	}
	response, err := b.client.Do(request)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("the %s request failed (limit %s): %s", operation, laneCommandTimeout, transportCategory(err))
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, laneFileLimit+1))
	if err == nil && len(data) > laneFileLimit {
		err = errAnswerTooLarge
	}
	if err != nil {
		return 0, nil, nil, fmt.Errorf("the answer to the %s request failed: %s", operation, transportCategory(err))
	}
	return response.StatusCode, response.Header, data, nil
}

// transportCategory maps a transport error to a fixed category. It never
// returns the error text.
func transportCategory(err error) string {
	var (
		netErr          net.Error
		dnsErr          *net.DNSError
		verifyErr       *tls.CertificateVerificationError
		recordErr       tls.RecordHeaderError
		alertErr        tls.AlertError
		unknownAuthErr  x509.UnknownAuthorityError
		hostnameErr     x509.HostnameError
		invalidCertErr  x509.CertificateInvalidError
		systemRootsErr  x509.SystemRootsError
		insecureAlgoErr x509.InsecureAlgorithmError
	)
	switch {
	case errors.Is(err, errAnswerTooLarge):
		return fmt.Sprintf("answer too large (more than %d bytes)", laneFileLimit)
	case errors.Is(err, http.ErrUseLastResponse):
		return "redirect refused"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return "connection closed"
	case errors.As(err, &dnsErr):
		return "name lookup failure"
	case errors.As(err, &verifyErr), errors.As(err, &recordErr), errors.As(err, &alertErr),
		errors.As(err, &unknownAuthErr), errors.As(err, &hostnameErr), errors.As(err, &invalidCertErr),
		errors.As(err, &systemRootsErr), errors.As(err, &insecureAlgoErr):
		return "TLS failure"
	default:
		return "other transport failure"
	}
}

// callbackCodeAndState reads the code and the state from the Keycloak
// redirect to the PM callback path. A failure message shows no part of the
// Location: only an OAuth error code that has the form of one.
func callbackCodeAndState(location, origin string) (string, string, error) {
	callback, err := url.Parse(location)
	if err != nil || callback.Scheme+"://"+callback.Host+callback.Path != origin+browserCallbackPath {
		return "", "", errors.New("the Keycloak login did not redirect to the PM callback path of the deployment")
	}
	query := callback.Query()
	if problem := query.Get("error"); problem != "" {
		if oauthErrorCode.MatchString(problem) {
			return "", "", fmt.Errorf("the Keycloak login redirected to the PM callback with the error %s", problem)
		}
		return "", "", errors.New("the Keycloak login redirected to the PM callback with an error that does not have the form of an OAuth error code, not shown")
	}
	code, state := query.Get("code"), query.Get("state")
	if code == "" || state == "" {
		return "", "", errors.New("the Keycloak redirect to the PM callback has no code or no state")
	}
	return code, state, nil
}

// oauthErrorCode is the form of an OAuth error code (RFC 6749, section
// 4.1.2.1) that a failure message can show.
var oauthErrorCode = regexp.MustCompile(`^[a-z_]{1,64}$`)

// describeProblem describes a PM answer for a failure message: only the
// problem code of a problem document, never the raw body.
func describeProblem(body []byte) string {
	var problem struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(body, &problem) == nil && problem.Code != "" && len(problem.Code) <= 64 &&
		!strings.ContainsFunc(problem.Code, func(r rune) bool { return !(r == '_' || r >= 'a' && r <= 'z') }) {
		return fmt.Sprintf("problem code %s", problem.Code)
	}
	return fmt.Sprintf("%d bytes, not a PM problem document, not shown", len(body))
}

var (
	keycloakLoginForm = regexp.MustCompile(`(?is)<form\b[^>]*\bid="kc-form-login"[^>]*>`)
	formAction        = regexp.MustCompile(`(?is)\saction="([^"]*)"`)
	keycloakTexts     = []*regexp.Regexp{
		regexp.MustCompile(`(?is)\bid="input-error[^"]*"[^>]*>\s*([^<]*?)\s*<`),
		regexp.MustCompile(`(?is)\bclass="[^"]*kc-feedback-text[^"]*"[^>]*>\s*([^<]*?)\s*<`),
		regexp.MustCompile(`(?is)\bclass="[^"]*instruction[^"]*"[^>]*>\s*([^<]*?)\s*<`),
	}
	// knownKeycloakTexts are the Keycloak messages that a failure message
	// can show. They are fixed English texts of Keycloak with no value in
	// them. Any other page text can hold the issuer, a host or an address,
	// so a failure message only counts it.
	knownKeycloakTexts = []string{
		"Invalid username or password.",
		"Invalid username or email.",
		"Account is disabled, contact your administrator.",
		"Account is temporarily disabled; contact your administrator or retry later.",
		"Cookie not found. Please make sure cookies are enabled in your browser.",
		"Invalid parameter: redirect_uri",
		"Client not found.",
		"Page not found",
		"Action expired. Please continue with login now.",
		"Your login attempt timed out. Login will start from the beginning.",
		"You took too long to login. Login process starting from beginning.",
		"You are already logged in.",
	}
)

// loginFormAction returns the action URL of the Keycloak login form. It
// must be an https URL of the Keycloak origin.
func loginFormAction(page []byte, issuer string) (string, error) {
	tag := keycloakLoginForm.Find(page)
	if tag == nil {
		return "", errors.New("the Keycloak answer has no login form kc-form-login")
	}
	match := formAction.FindSubmatch(tag)
	if match == nil {
		return "", errors.New("the Keycloak login form has no action")
	}
	action := html.UnescapeString(string(match[1]))
	parsed, err := url.Parse(action)
	expected, expectedErr := url.Parse(issuer)
	if err != nil || expectedErr != nil || parsed.Scheme != "https" || parsed.Host != expected.Host {
		return "", errors.New("the Keycloak login form action is not an https URL of the Keycloak origin")
	}
	return action, nil
}

// keycloakMessage describes a Keycloak page for a failure message: its byte
// count, whether it has the login form, and its error and feedback texts.
// It shows a text only when the text is one of knownKeycloakTexts, and
// otherwise only counts it. It never shows the page, which holds session
// codes and can hold the issuer, a host or an address.
func keycloakMessage(page []byte) string {
	var known []string
	other := 0
	for _, pattern := range keycloakTexts {
		for _, match := range pattern.FindAllSubmatch(page, 4) {
			text := strings.TrimSpace(html.UnescapeString(string(match[1])))
			switch {
			case text == "":
			case slices.Contains(knownKeycloakTexts, text):
				known = append(known, fmt.Sprintf("%q", text))
			default:
				other++
			}
		}
	}
	form := "no login form"
	if keycloakLoginForm.Match(page) {
		form = "a login form"
	}
	return fmt.Sprintf("Keycloak page of %d bytes with %s, known texts [%s] and %d other texts, not shown",
		len(page), form, strings.Join(known, ", "), other)
}

// loginStart is the step that runs pm sessions login-start and keeps its
// stdout in memory only (D4-11). It needs exit code 0 and one challenge ID,
// user code and device proof in the output.
func (s *deviceLoginScenario) loginStart(ctx context.Context, line string) error {
	if _, err := s.shared.started("pm-server"); err != nil {
		return err
	}
	if s.challenge != nil {
		return errors.New("the scenario already started a device login")
	}
	args, err := commandArguments(line, "sessions", "login-start")
	if err != nil {
		return err
	}
	code, stdout, stderr, err := s.runPM(ctx, args)
	if err != nil {
		return err
	}
	challenge, err := checkLoginStart(code, stdout, stderr)
	if err != nil {
		return err
	}
	challenge.stdout = []byte(stdout)
	s.challenge = challenge
	s.startedAt = time.Now()
	return nil
}

// checkLoginStart reads the login-start result. A failure message never
// shows stdout: it holds the proof. It shows stderr only by form
// (describeSafeLines), because the step cannot know each secret that a
// failed login-start can write (review finding F2).
func checkLoginStart(code int, stdout, stderr string) (*deviceChallenge, error) {
	if code != 0 {
		return nil, fmt.Errorf("pm sessions login-start exited with code %d, want 0\nstdout: %d bytes, not shown\n%s",
			code, len(stdout), describeSafeLines("stderr", stderr, true))
	}
	challenge, err := parseDeviceChallenge(deviceChallengeFields(stdout))
	if err != nil {
		return nil, fmt.Errorf("read the pm sessions login-start stdout (%d bytes, not shown): %w", len(stdout), err)
	}
	return challenge, nil
}

// deviceChallengeFields reads each JSON string value of the login-start
// output, by its key: one JSON object (JSON output), or one
// "key<TAB>JSON value" line for each field (text output).
func deviceChallengeFields(stdout string) map[string][]string {
	fields := map[string][]string{}
	var document map[string]json.RawMessage
	if json.Unmarshal([]byte(stdout), &document) == nil {
		for key, raw := range document {
			var value string
			if json.Unmarshal(raw, &value) == nil {
				fields[key] = append(fields[key], value)
			}
		}
	} else {
		for _, line := range strings.Split(stdout, "\n") {
			key, raw, ok := strings.Cut(line, "\t")
			if !ok {
				continue
			}
			var value string
			if json.Unmarshal([]byte(raw), &value) == nil {
				fields[key] = append(fields[key], value)
			}
		}
	}
	return fields
}

// parseDeviceChallenge needs exactly one nonempty challenge ID, user code
// and device proof in the fields. An error never holds a value of the
// output.
func parseDeviceChallenge(fields map[string][]string) (*deviceChallenge, error) {
	values := map[string]string{}
	for _, key := range []string{"challenge_id", "user_code", "device_proof"} {
		if len(fields[key]) != 1 || fields[key][0] == "" {
			return nil, fmt.Errorf("the output has %d nonempty JSON string values for %s, want 1", len(fields[key]), key)
		}
		values[key] = fields[key][0]
	}
	if strings.ContainsAny(values["device_proof"], "\r\n") {
		return nil, errors.New("the device proof is not one line")
	}
	return &deviceChallenge{challengeID: values["challenge_id"], userCode: values["user_code"], proof: values["device_proof"]}, nil
}

// startExchange is the step that starts pm sessions login-exchange. It runs
// the command line of the feature text and adds only --challenge-id with the
// challenge ID from the login-start output. It changes no other flag. It
// writes the proof and one line feed to the stdin pipe and closes the pipe
// (D4-10, D4-13). It does not wait for the process.
func (s *deviceLoginScenario) startExchange(ctx context.Context, line string) error {
	if _, err := s.shared.started("pm-server"); err != nil {
		return err
	}
	if s.challenge == nil {
		return errors.New("the scenario did not start a device login")
	}
	if s.exchange != nil {
		return errors.New("the scenario already started a device login exchange")
	}
	args, err := commandArguments(line, "sessions", "login-exchange")
	if err != nil {
		return err
	}
	for _, arg := range args {
		if arg == "--challenge-id" || strings.HasPrefix(arg, "--challenge-id=") {
			return errors.New("the command line already has --challenge-id; the step adds it from the login-start output")
		}
	}
	exitLimit, err := exchangeExitLimit(args)
	if err != nil {
		return err
	}
	environment, err := s.environment()
	if err != nil {
		return err
	}
	args = append(args, "--challenge-id", s.challenge.challengeID)
	process, err := startExchangeProcess(s.shared.harness.bins.pm, args, environment)
	if err != nil {
		return err
	}
	process.exitLimit = exitLimit
	s.exchange = process
	if err := process.writeStdin(s.challenge.proof); err != nil {
		return fmt.Errorf("write the device proof to the login-exchange stdin pipe and close the pipe: %s", pipeCategory(err))
	}
	godog.Logf(ctx, "login-exchange started %s after login-start ended", time.Since(s.startedAt).Round(time.Millisecond))
	return nil
}

// exchangeExitLimit derives the exit limit of the started command line: its
// --request-timeout value plus deviceLoginExitMargin, or the CLI default plus
// the margin when the flag is absent. The value must be a canonical Go
// duration from 1 s to 5 min, as the CLI requires
// (internal/cli/config.go, parseCanonicalDuration). A refusal has a fixed
// message.
func exchangeExitLimit(args []string) (time.Duration, error) {
	const flag = "--request-timeout"
	timeout := deviceLoginDefaultRequestTimeout
	found := false
	for index := 0; index < len(args); index++ {
		var raw string
		switch {
		case args[index] == flag:
			if index+1 >= len(args) {
				return 0, errors.New("the command line has --request-timeout with no value")
			}
			index++
			raw = args[index]
		case strings.HasPrefix(args[index], flag+"="):
			raw = strings.TrimPrefix(args[index], flag+"=")
		default:
			continue
		}
		if found {
			return 0, errors.New("the command line has --request-timeout more than once")
		}
		found = true
		value, err := time.ParseDuration(raw)
		if err != nil || value.String() != raw || value < time.Second || value > 5*time.Minute {
			return 0, errors.New("the --request-timeout value of the command line is not a canonical Go duration from 1s to 5m0s")
		}
		timeout = value
	}
	return timeout + deviceLoginExitMargin, nil
}

// startExchangeProcess starts the binary as a direct child, with three pipes.
func startExchangeProcess(binary string, args, environment []string) (*exchangeProcess, error) {
	var opened []*os.File
	closeAll := func() {
		for _, file := range opened {
			_ = file.Close()
		}
	}
	pipe := func() (*os.File, *os.File, error) {
		reader, writer, err := os.Pipe()
		if err == nil {
			opened = append(opened, reader, writer)
		}
		return reader, writer, err
	}
	stdinReader, stdinWriter, err1 := pipe()
	stdoutReader, stdoutWriter, err2 := pipe()
	stderrReader, stderrWriter, err3 := pipe()
	if err := errors.Join(err1, err2, err3); err != nil {
		closeAll()
		return nil, fmt.Errorf("create the login-exchange pipes: %w", err)
	}
	command := exec.Command(binary, args...)
	command.Env = environment
	command.Stdin, command.Stdout, command.Stderr = stdinReader, stdoutWriter, stderrWriter
	if err := command.Start(); err != nil {
		closeAll()
		return nil, errors.New("start pm sessions login-exchange: the pm binary did not start")
	}
	// The child holds its own copies of these ends.
	_ = stdinReader.Close()
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	p := &exchangeProcess{
		command:     command,
		stdin:       stdinWriter,
		first:       make(chan firstLine, 1),
		outputEnded: make(chan struct{}),
		exited:      make(chan struct{}),
	}
	var readers sync.WaitGroup
	readers.Add(2)
	go readStream(stdoutReader, &p.stdout, nil, readers.Done)
	go readStream(stderrReader, &p.stderr, p.first, readers.Done)
	go func() {
		readers.Wait()
		close(p.outputEnded)
	}()
	go func() {
		waitErr := command.Wait()
		state := command.ProcessState
		switch {
		case state == nil:
			p.status = processStatus{code: -1, text: fmt.Sprintf("no exit status: %v", waitErr)}
		default:
			p.status = processStatus{exited: state.Exited(), code: state.ExitCode(), text: state.String()}
		}
		close(p.exited)
	}()
	return p, nil
}

// writeStdin writes the proof and one line feed, then closes the pipe.
func (p *exchangeProcess) writeStdin(proof string) error {
	writer, err := p.takeStdin()
	if err != nil {
		return err
	}
	return writeProof(writer, proof)
}

// takeStdin gives the write end of the stdin pipe to the caller, which
// closes it. stopExchangeProcess then does not close it.
func (p *exchangeProcess) takeStdin() (*os.File, error) {
	writer := p.stdin
	if writer == nil {
		return nil, errors.New("the stdin pipe is already closed")
	}
	p.stdin = nil
	return writer, nil
}

// writeProof writes the proof and one line feed to the writer, then closes
// it.
func writeProof(writer *os.File, proof string) error {
	line := []byte(proof + "\n")
	_, writeErr := writer.Write(line)
	clear(line)
	return errors.Join(writeErr, writer.Close())
}

// pipeCategory names a stdin pipe failure by a fixed category.
func pipeCategory(err error) string {
	if errors.Is(err, syscall.EPIPE) {
		return "the process closed its stdin"
	}
	return "the write or the close failed"
}

// readStream reads one output stream until end of file. For stderr, it sends
// the first line once: at the first line feed, at end of file, or when the
// line passes the bound.
func readStream(reader *os.File, output *boundedOutput, first chan<- firstLine, done func()) {
	defer done()
	defer reader.Close()
	sent := first == nil
	var line []byte
	buffer := make([]byte, 4096)
	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			output.write(chunk)
			if !sent {
				if index := bytes.IndexByte(chunk, '\n'); index >= 0 {
					first <- firstLine{line: string(append(line, chunk[:index]...)), found: true}
					sent = true
				} else if line = append(line, chunk...); len(line) > deviceLoginOutputLimit {
					first <- firstLine{line: "", found: false}
					sent = true
				}
			}
		}
		if err != nil {
			if !sent {
				first <- firstLine{line: string(line), found: false}
			}
			return
		}
	}
}

// awaitEnd waits for the exit of the process and the end of its output,
// until the timer fires. It reports whether both happened.
func (p *exchangeProcess) awaitEnd(timer <-chan time.Time) bool {
	select {
	case <-p.exited:
	case <-timer:
		return false
	}
	select {
	case <-p.outputEnded:
		return true
	case <-timer:
		return false
	}
}

// awaitEndUntil waits for the exit of the process and the end of its
// output, until done closes. It reports whether both happened.
func (p *exchangeProcess) awaitEndUntil(done <-chan struct{}) bool {
	select {
	case <-p.exited:
	case <-done:
		return false
	}
	select {
	case <-p.outputEnded:
		return true
	case <-done:
		return false
	}
}

// ended reports whether the process exited and its output ended, with no
// wait.
func (p *exchangeProcess) ended() bool {
	select {
	case <-p.exited:
	default:
		return false
	}
	select {
	case <-p.outputEnded:
		return true
	default:
		return false
	}
}

// describeStatus names the state of the process for a failure message.
func (p *exchangeProcess) describeStatus() string {
	select {
	case <-p.exited:
	default:
		return "the process is still running"
	}
	if p.status.exited {
		return fmt.Sprintf("the process exited with code %d", p.status.code)
	}
	return fmt.Sprintf("the process ended with no exit code: %s", p.status.text)
}

func (s *deviceLoginScenario) exchangeProcess() (*exchangeProcess, error) {
	if s.exchange == nil {
		return nil, errors.New("the scenario did not start a device login exchange")
	}
	return s.exchange, nil
}

// noticeLine is the step that waits for the first stderr line of
// login-exchange, with one time limit. The line must be the notice line N1.
// When the step names device_poll_too_soon, the line must name it. Any other
// first line, end of file or the end of the process fails the step.
func (s *deviceLoginScenario) noticeLine(ctx context.Context, code string) error {
	return s.checkNoticeLine(ctx, code, 0)
}

// noticeLineWithWait is the step of scenario 1 (criteria v3, P5): the notice
// line must name device_poll_too_soon and a wait of at least the given
// seconds. A wait of 11 s or more is longer than the default 5 s interval can
// give after a too-soon answer.
func (s *deviceLoginScenario) noticeLineWithWait(ctx context.Context, minimum int) error {
	return s.checkNoticeLine(ctx, "device_poll_too_soon", minimum)
}

// checkNoticeLine waits for the first stderr line and checks it. code is
// empty for either code. minimum is 0 for no wait check. A failure shows the
// line only by its form, and the wait that it found.
func (s *deviceLoginScenario) checkNoticeLine(ctx context.Context, code string, minimum int) error {
	p, err := s.exchangeProcess()
	if err != nil {
		return err
	}
	timer := time.NewTimer(deviceLoginNoticeLimit)
	defer timer.Stop()
	var first firstLine
	select {
	case first = <-p.first:
	case <-timer.C:
		return fmt.Errorf("the device login exchange wrote no complete first stderr line within %s; %s\n%s",
			deviceLoginNoticeLimit, p.describeStatus(), describeOutputText("stderr", &p.stderr))
	}
	if first.found {
		if match := deviceLoginNoticeLine.FindStringSubmatch(first.line); match != nil {
			if code != "" && match[1] != code {
				return fmt.Errorf("the wait notice line names %s, want %s: the first exchange request did not reach pm-server before the first permitted poll time of the challenge\nfirst stderr line: %s",
					match[1], code, describeSafeLine(first.line, true))
			}
			wait, err := strconv.Atoi(match[2])
			if err != nil {
				return fmt.Errorf("the wait of the notice line is not an integer that the step can read\nfirst stderr line: %s", describeSafeLine(first.line, true))
			}
			if wait < minimum {
				return fmt.Errorf("the wait notice line names a wait of %d seconds, want at least %d\nfirst stderr line: %s",
					wait, minimum, describeSafeLine(first.line, true))
			}
			godog.Logf(ctx, "login-exchange notice line names %s and a wait of %d seconds", match[1], wait)
			return nil
		}
	}
	// The failure path waits for the end of the process within the same
	// limit, so that the message can name the exit code and the whole stderr.
	what := "its first stderr line is not the wait notice line"
	if !first.found {
		what = "stderr ended, or passed its bound, before a complete first line"
	}
	p.awaitEnd(timer.C)
	return fmt.Errorf("the device login exchange wrote no wait notice line: %s; %s\n%s",
		what, p.describeStatus(), describeOutputText("stderr", &p.stderr))
}

// challengePollInterval is the step "the device login challenge states a
// poll interval of <n> seconds" (criteria v3, P5). It reads
// poll_interval_seconds from the login-start stdout that the scenario keeps
// in memory, with the parser of the #366 steps. A mismatch shows only the two
// integers. A parse failure has a fixed message. It never shows stdout.
func (s *deviceLoginScenario) challengePollInterval(want int) error {
	if s.challenge == nil {
		return errors.New("the scenario did not start a device login")
	}
	interval, ok := loginStartPollInterval(s.challenge.stdout)
	if !ok {
		return errors.New("the kept pm sessions login-start stdout does not parse as an answer with one integer poll_interval_seconds")
	}
	if interval != want {
		return fmt.Errorf("the device login challenge states a poll interval of %d seconds, want %d", interval, want)
	}
	return nil
}

// decide is the step in which the browser session approves or denies the
// device login, with the browser cookies and the CSRF token
// (api/pm.openapi.yaml:95-134). It needs status 204.
func (s *deviceLoginScenario) decide(ctx context.Context, decision string) error {
	return decideDeviceLogin(ctx, s.browser, s.challenge, decision)
}

// decideDeviceLogin sends the decision of the browser session on the device
// login: "approves" or "denies".
func decideDeviceLogin(ctx context.Context, browser *browserSession, challenge *deviceChallenge, decision string) error {
	if browser == nil || browser.csrf == "" {
		return errors.New("the scenario has no browser session")
	}
	if challenge == nil {
		return errors.New("the scenario did not start a device login")
	}
	path, operation := "/v1/sessions/device/approve", operationDeviceApprove
	if decision == "denies" {
		path, operation = "/v1/sessions/device/deny", operationDeviceDeny
	}
	body, err := json.Marshal(map[string]string{"challenge_id": challenge.challengeID, "user_code": challenge.userCode})
	if err != nil {
		return fmt.Errorf("encode the device decision: %w", err)
	}
	header := jsonHeaders(browser.origin)
	header.Set("X-CSRF-Token", browser.csrf)
	status, _, answer, err := browser.send(ctx, operation, http.MethodPost, browser.origin+path, body, header)
	clear(body)
	if err != nil {
		return err
	}
	return checkDecision(operation, status, answer)
}

// checkDecision needs status 204. A failure message shows only the status
// and the problem code.
func checkDecision(operation string, status int, answer []byte) error {
	if status != http.StatusNoContent {
		return fmt.Errorf("the %s request returned status %d, want 204: %s", operation, status, describeProblem(answer))
	}
	return nil
}

// sendSIGINT is the step that sends SIGINT to the running login-exchange
// process. cmd/pm/main.go:15 turns SIGINT into a cancelled context.
func (s *deviceLoginScenario) sendSIGINT() error {
	p, err := s.exchangeProcess()
	if err != nil {
		return err
	}
	select {
	case <-p.exited:
		return fmt.Errorf("the device login exchange ended before the step sent SIGINT: %s\n%s", p.describeStatus(), describeOutputText("stderr", &p.stderr))
	default:
	}
	if err := p.command.Process.Signal(os.Interrupt); err != nil {
		return fmt.Errorf("send SIGINT to the device login exchange: %w", err)
	}
	return nil
}

// exitsWithCode is the step that waits once for the exit of login-exchange,
// with one time limit, and checks its exit code.
func (s *deviceLoginScenario) exitsWithCode(want int) error {
	p, err := s.exchangeProcess()
	if err != nil {
		return err
	}
	if p.exitLimit <= 0 {
		return errors.New("the device login exchange has no exit limit")
	}
	timer := time.NewTimer(p.exitLimit)
	defer timer.Stop()
	if !p.awaitEnd(timer.C) {
		return fmt.Errorf("the device login exchange did not end within %s; %s\n%s", p.exitLimit, p.describeStatus(), describeOutputText("stderr", &p.stderr))
	}
	if !p.status.exited || p.status.code != want {
		return fmt.Errorf("%s, want exit code %d\n%s", p.describeStatus(), want, describeOutputText("stderr", &p.stderr))
	}
	return nil
}

// processEnded is the step that checks, with no wait, that the
// login-exchange process exited by itself and is gone.
func (s *deviceLoginScenario) processEnded() error {
	p, err := s.exchangeProcess()
	if err != nil {
		return err
	}
	if !p.ended() {
		return fmt.Errorf("the device login exchange has not ended: %s", p.describeStatus())
	}
	if !p.status.exited {
		return fmt.Errorf("the device login exchange did not exit by itself: %s", p.describeStatus())
	}
	if p.command != nil {
		if err := p.command.Process.Signal(syscall.Signal(0)); !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("the device login exchange process is still there after its exit: %v", err)
		}
	}
	return nil
}

// stderrNames is the step that checks that the whole stderr of the ended
// login-exchange process names the code.
func (s *deviceLoginScenario) stderrNames(code string) error {
	p, err := s.exchangeProcess()
	if err != nil {
		return err
	}
	if !p.ended() {
		return fmt.Errorf("the device login exchange has not ended, so its whole stderr is not known: %s", p.describeStatus())
	}
	text, whole := p.stderr.text()
	if !whole || !strings.Contains(text, code) {
		return fmt.Errorf("the device login exchange stderr does not name %s\n%s", code, describeOutputText("stderr", &p.stderr))
	}
	return nil
}

// runWithStoredSession is the step that runs one pm command against the
// started pm-server with the default OS session store, so that the command
// reads the PM session that login-exchange stored in the private keyring. It
// writes the shared result state, with its output by form only.
func (s *deviceLoginScenario) runWithStoredSession(ctx context.Context, line string) error {
	if _, err := s.shared.started("pm-server"); err != nil {
		return err
	}
	args, err := commandArguments(line)
	if err != nil {
		return err
	}
	code, stdout, stderr, err := s.runPM(ctx, args)
	if err != nil {
		return err
	}
	s.shared.result = storedSessionResult(code, stdout, stderr, s.shared.deployment.deploymentID)
	return nil
}

// storedSessionResult is the shared result state of a command with the
// stored PM session. The shared exit code step prints the stdout and the
// stderr of the result when it fails. The command runs with a PM session
// that the step does not know, so the result keeps only text that is safe by
// its form: the stdout byte count and the stderr by form.
func storedSessionResult(code int, stdout, stderr, deploymentID string) commandResult {
	return commandResult{
		ran:          true,
		exitCode:     code,
		stdout:       fmt.Sprintf("%d bytes, not kept by the device login steps", len(stdout)),
		stderr:       describeSafeLines("stderr", stderr, true),
		deploymentID: deploymentID,
	}
}

// close ends a login-exchange process that is still running, with SIGKILL,
// and waits for it with the kill limit. It closes the browser connections.
func (s *deviceLoginScenario) close() error {
	var errs []error
	if p := s.exchange; p != nil {
		errs = append(errs, stopExchangeProcess(p))
	}
	if s.browser != nil && s.browser.client != nil {
		s.browser.client.CloseIdleConnections()
	}
	if s.challenge != nil {
		clear(s.challenge.stdout)
	}
	s.browser, s.challenge, s.exchange = nil, nil, nil
	return errors.Join(errs...)
}
