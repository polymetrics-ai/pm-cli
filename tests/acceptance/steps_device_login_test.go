package acceptance

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"regexp"
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
// argument, an environment variable or the log. Every failure message
// redacts them (D4-10, D4-11).
const (
	// deviceLoginNoticeLimit bounds the wait for the first stderr line of
	// login-exchange. The command writes the notice line after the answer to
	// its first exchange request. --request-timeout bounds that request, and
	// its default is 30 s (internal/cli/config.go:16). 60 s holds the process
	// start, the stdin read and a full 30 s request.
	deviceLoginNoticeLimit = laneCommandTimeout
	// deviceLoginExitLimit bounds the wait for the exit of login-exchange
	// after the notice line. --request-timeout bounds the whole wait of the
	// command (D4-7), so the command ends at most 30 s after its first
	// exchange request with the default. 60 s holds that.
	deviceLoginExitLimit = laneCommandTimeout
	// deviceLoginOutputLimit bounds each output stream that the step keeps.
	deviceLoginOutputLimit = laneOutputLimit
	// deviceLoginMessageLimit bounds the output text in a failure message.
	deviceLoginMessageLimit = 2048

	browserSessionCookieName = "__Host-pm_session"
	browserCSRFCookieName    = "__Host-pm_csrf"
)

// deviceLoginNoticeLine is the notice line N1 of L-d, without its line feed.
var deviceLoginNoticeLine = regexp.MustCompile(`^notice: device login exchange received (device_authorization_pending|device_poll_too_soon)\. The CLI waits [1-9][0-9]*s before the next exchange request\.$`)

// deviceChallenge is the login-start output that the steps keep in memory.
// The proof and the user code are secrets.
type deviceChallenge struct {
	challengeID string
	userCode    string
	proof       string
}

func (deviceChallenge) String() string   { return "device challenge [REDACTED]" }
func (deviceChallenge) GoString() string { return "device challenge [REDACTED]" }

// browserSession is the PM browser session of the deployment's Keycloak
// user: one HTTP client with one cookie jar. It holds secrets.
type browserSession struct {
	client   *http.Client
	origin   string
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

// secrets lists each secret that the scenario holds, for the redaction of a
// failure message.
func (s *deviceLoginScenario) secrets() []string {
	var values []string
	if s.challenge != nil {
		values = append(values, s.challenge.proof, s.challenge.userCode)
	}
	if s.browser != nil {
		values = append(values, s.browser.password, s.browser.code, s.browser.state, s.browser.csrf, s.browser.session)
	}
	return values
}

// redact returns the text for a failure message: each form of each secret
// replaced, at most deviceLoginMessageLimit bytes, quoted.
func (s *deviceLoginScenario) redact(text string) string {
	return redactSecrets(text, s.secrets()...)
}

func redactSecrets(text string, secrets ...string) string {
	for _, secret := range secrets {
		for _, form := range deviceLoginSecretForms(secret) {
			text = strings.ReplaceAll(text, form, "[REDACTED]")
		}
	}
	if len(text) > deviceLoginMessageLimit {
		text = text[:deviceLoginMessageLimit] + "..."
	}
	return fmt.Sprintf("%q", text)
}

// deviceLoginSecretForms returns the forms of a secret that the redaction
// replaces: exact, JSON string content, base64, base64url with and without
// padding, URL-encoded and hex.
func deviceLoginSecretForms(secret string) []string {
	if secret == "" {
		return nil
	}
	value := []byte(secret)
	encoded, _ := json.Marshal(secret)
	return []string{
		secret,
		strings.TrimSuffix(strings.TrimPrefix(string(encoded), `"`), `"`),
		base64.StdEncoding.EncodeToString(value),
		base64.RawStdEncoding.EncodeToString(value),
		base64.URLEncoding.EncodeToString(value),
		base64.RawURLEncoding.EncodeToString(value),
		url.QueryEscape(secret),
		url.PathEscape(secret),
		hex.EncodeToString(value),
	}
}

// describeOutputText describes one output stream for a failure message. A
// stream that was longer than its bound is not shown, because the bound can
// cut a secret, and then the redaction cannot find it.
func (s *deviceLoginScenario) describeOutputText(name string, output *boundedOutput) string {
	text, whole := output.text()
	if !whole {
		return fmt.Sprintf("%s: more than %d bytes, not shown", name, deviceLoginOutputLimit)
	}
	return fmt.Sprintf("%s (redacted, %d bytes): %s", name, len(text), s.redact(text))
}

// runPM runs one pm process with the environment of these steps and waits
// once for its exit, with the limit of one lane command.
func (s *deviceLoginScenario) runPM(parent context.Context, args []string) (int, string, string, error) {
	environment, err := s.environment()
	if err != nil {
		return 0, "", "", err
	}
	ctx, cancel := context.WithTimeout(parent, laneCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, s.shared.harness.bins.pm, args...)
	command.Env = environment
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	runErr := command.Run()
	if ctx.Err() != nil {
		return 0, "", "", fmt.Errorf("pm did not finish within %s: %q", laneCommandTimeout, args)
	}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		return 0, stdout.String(), stderr.String(), nil
	case errors.As(runErr, &exitErr):
		return exitErr.ExitCode(), stdout.String(), stderr.String(), nil
	default:
		return 0, "", "", fmt.Errorf("start pm: %w", runErr)
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
// client and one cookie jar: POST /v1/sessions/browser/challenges, the
// Keycloak login form, and POST /v1/sessions/browser with the code and the
// state of the Keycloak redirect to the callback path. It sends no request
// to the callback path itself. pm-server has no handler for it, and a
// browser front end sends the code and the state to POST /v1/sessions/browser.
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
	if d.identity == nil || d.identity.Username == "" || d.identity.Password == "" || d.stack.identity.admin == nil {
		return errors.New("the deployment has no user in the job Keycloak")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(d.authority.pem) || !roots.AppendCertsFromPEM(d.stack.identity.ca) {
		return errors.New("the browser cannot read the deployment CA or the Keycloak CA")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return fmt.Errorf("create the browser cookie jar: %w", err)
	}
	b := &browserSession{
		client: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig:   &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
				DisableKeepAlives: true,
			},
			Jar: jar,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		origin:   d.origin,
		password: d.identity.Password,
	}
	s.browser = b
	issuer := d.stack.identity.admin.Issuer()

	// 1. PM starts the OIDC login and sets its challenge cookie.
	start, err := json.Marshal(map[string]bool{"reauthenticate": false})
	if err != nil {
		return fmt.Errorf("encode the browser login start: %w", err)
	}
	status, _, body, err := b.send(ctx, http.MethodPost, b.origin+"/v1/sessions/browser/challenges", start, jsonHeaders(b.origin))
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("POST /v1/sessions/browser/challenges returned status %d, want 201: %s", status, describeProblem(body))
	}
	var challenge struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	if err := json.Unmarshal(body, &challenge); err != nil || !strings.HasPrefix(challenge.AuthorizationURL, issuer+"/protocol/openid-connect/auth?") {
		return errors.New("the browser login challenge has no authorization URL of the job Keycloak")
	}

	// 2. The Keycloak login form.
	status, _, page, err := b.send(ctx, http.MethodGet, challenge.AuthorizationURL, nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("the Keycloak authorization request returned status %d, want 200 with the login form: %s", status, s.keycloakMessage(page))
	}
	action, err := loginFormAction(page, issuer)
	if err != nil {
		return fmt.Errorf("%w: %s", err, s.keycloakMessage(page))
	}

	// 3. The credentials of the deployment user. Keycloak redirects to the
	// callback path with the code and the state.
	form := url.Values{"username": {d.identity.Username}, "password": {d.identity.Password}}
	status, header, page, err := b.send(ctx, http.MethodPost, action, []byte(form.Encode()),
		http.Header{"Content-Type": {"application/x-www-form-urlencoded"}})
	if err != nil {
		return err
	}
	if status != http.StatusFound && status != http.StatusSeeOther {
		return fmt.Errorf("the Keycloak login returned status %d, want a redirect to the PM callback: %s", status, s.keycloakMessage(page))
	}
	callback, err := url.Parse(header.Get("Location"))
	if err != nil || callback.Scheme+"://"+callback.Host+callback.Path != b.origin+browserCallbackPath {
		return errors.New("the Keycloak login did not redirect to the PM callback path of the deployment")
	}
	query := callback.Query()
	if problem := query.Get("error"); problem != "" {
		return fmt.Errorf("the Keycloak login redirected with the error %s: %s", s.redact(problem), s.redact(query.Get("error_description")))
	}
	b.code, b.state = query.Get("code"), query.Get("state")
	if b.code == "" || b.state == "" {
		return errors.New("the Keycloak redirect to the PM callback has no code or no state")
	}

	// 4. PM completes the login and sets the session and CSRF cookies.
	complete, err := json.Marshal(map[string]string{"code": b.code, "state": b.state})
	if err != nil {
		return fmt.Errorf("encode the browser login completion: %w", err)
	}
	status, _, body, err = b.send(ctx, http.MethodPost, b.origin+"/v1/sessions/browser", complete, jsonHeaders(b.origin))
	clear(complete)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("POST /v1/sessions/browser returned status %d, want 204: %s", status, describeProblem(body))
	}
	originURL, err := url.Parse(b.origin)
	if err != nil {
		return fmt.Errorf("parse the deployment origin: %w", err)
	}
	for _, cookie := range jar.Cookies(originURL) {
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
	godog.Logf(ctx, "browser session: the deployment's Keycloak user logged in through PM")
	return nil
}

// jsonHeaders are the headers of a PM browser request with a JSON body.
func jsonHeaders(origin string) http.Header {
	return http.Header{"Content-Type": {"application/json"}, "Origin": {origin}}
}

// send sends one request with the browser client, with one limit and no
// redirect, and reads at most laneFileLimit bytes of the answer. An error
// names the method and the path, never the query.
func (b *browserSession) send(parent context.Context, method, target string, body []byte, header http.Header) (int, http.Header, []byte, error) {
	ctx, cancel := context.WithTimeout(parent, laneCommandTimeout)
	defer cancel()
	place := method + " " + targetPath(target)
	request, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("make the browser request %s", place)
	}
	for name, values := range header {
		request.Header[name] = values
	}
	response, err := b.client.Do(request)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return 0, nil, nil, fmt.Errorf("the browser request %s failed within %s: %v", place, laneCommandTimeout, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, laneFileLimit+1))
	if err != nil {
		return 0, nil, nil, fmt.Errorf("read the answer to the browser request %s: %v", place, err)
	}
	if len(data) > laneFileLimit {
		return 0, nil, nil, fmt.Errorf("the answer to the browser request %s is larger than %d bytes", place, laneFileLimit)
	}
	return response.StatusCode, response.Header, data, nil
}

// targetPath returns the origin and the path of a URL, with no query.
func targetPath(target string) string {
	parsed, err := url.Parse(target)
	if err != nil {
		return "(an invalid URL)"
	}
	return parsed.Scheme + "://" + parsed.Host + parsed.Path
}

// describeProblem describes a PM answer for a failure message: only the
// problem code of a problem document, never the raw body.
func describeProblem(body []byte) string {
	var problem struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(body, &problem) == nil && problem.Code != "" && len(problem.Code) <= 128 &&
		!strings.ContainsFunc(problem.Code, func(r rune) bool { return !(r == '_' || r >= 'a' && r <= 'z') }) {
		return fmt.Sprintf("problem code %s", problem.Code)
	}
	return fmt.Sprintf("%d bytes, not a PM problem document, not shown", len(body))
}

var (
	keycloakLoginForm = regexp.MustCompile(`(?is)<form\b[^>]*\bid="kc-form-login"[^>]*>`)
	formAction        = regexp.MustCompile(`(?is)\saction="([^"]*)"`)
	keycloakTexts     = []*regexp.Regexp{
		regexp.MustCompile(`(?is)<title>\s*([^<]*?)\s*</title>`),
		regexp.MustCompile(`(?is)\bid="input-error[^"]*"[^>]*>\s*([^<]*?)\s*<`),
		regexp.MustCompile(`(?is)\bclass="[^"]*kc-feedback-text[^"]*"[^>]*>\s*([^<]*?)\s*<`),
		regexp.MustCompile(`(?is)\bclass="[^"]*instruction[^"]*"[^>]*>\s*([^<]*?)\s*<`),
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

// keycloakMessage describes a Keycloak page for a failure message: its
// title and its visible error and feedback texts, redacted. It never shows
// the page itself, which holds session codes.
func (s *deviceLoginScenario) keycloakMessage(page []byte) string {
	var texts []string
	for _, pattern := range keycloakTexts {
		for _, match := range pattern.FindAllSubmatch(page, 4) {
			if text := strings.TrimSpace(html.UnescapeString(string(match[1]))); text != "" {
				texts = append(texts, text)
			}
		}
	}
	if len(texts) == 0 {
		return fmt.Sprintf("%d bytes, no Keycloak title or message", len(page))
	}
	return fmt.Sprintf("%d bytes, Keycloak texts %s", len(page), s.redact(strings.Join(texts, " | ")))
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
	challenge, err := s.checkLoginStart(code, stdout, stderr)
	if err != nil {
		return err
	}
	s.challenge = challenge
	s.startedAt = time.Now()
	return nil
}

// checkLoginStart reads the login-start result. A failure message never
// shows stdout: it holds the proof, and the proof is not known before the
// read. The stderr text is redacted for each proof and user code that the
// stdout holds.
func (s *deviceLoginScenario) checkLoginStart(code int, stdout, stderr string) (*deviceChallenge, error) {
	fields := deviceChallengeFields(stdout)
	if code != 0 {
		secrets := append(s.secrets(), fields["device_proof"]...)
		secrets = append(secrets, fields["user_code"]...)
		return nil, fmt.Errorf("pm sessions login-start exited with code %d, want 0\nstdout: %d bytes, not shown\nstderr (redacted, %d bytes): %s",
			code, len(stdout), len(stderr), redactSecrets(stderr, secrets...))
	}
	challenge, err := parseDeviceChallenge(fields)
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
	environment, err := s.environment()
	if err != nil {
		return err
	}
	args = append(args, "--challenge-id", s.challenge.challengeID)
	process, err := startExchangeProcess(s.shared.harness.bins.pm, args, environment)
	if err != nil {
		return err
	}
	s.exchange = process
	if err := process.writeStdin(s.challenge.proof); err != nil {
		return fmt.Errorf("write the device proof to the login-exchange stdin pipe and close the pipe: %v", err)
	}
	godog.Logf(ctx, "login-exchange started %s after login-start ended", time.Since(s.startedAt).Round(time.Millisecond))
	return nil
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
		return nil, fmt.Errorf("start pm sessions login-exchange: %w", err)
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
	writer := p.stdin
	if writer == nil {
		return errors.New("the stdin pipe is already closed")
	}
	p.stdin = nil
	line := []byte(proof + "\n")
	_, writeErr := writer.Write(line)
	clear(line)
	return errors.Join(writeErr, writer.Close())
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
// For scenario 1 it must name device_poll_too_soon. Any other first line,
// end of file or the end of the process fails the step.
func (s *deviceLoginScenario) noticeLine(ctx context.Context, code string) error {
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
			deviceLoginNoticeLimit, p.describeStatus(), s.describeOutputText("stderr", &p.stderr))
	}
	if first.found {
		if match := deviceLoginNoticeLine.FindStringSubmatch(first.line); match != nil {
			if code == "" || match[1] == code {
				godog.Logf(ctx, "login-exchange notice line names %s", match[1])
				return nil
			}
			return fmt.Errorf("the wait notice line names %s, want %s: the first exchange request did not reach pm-server within 5 s of login-start (the five-second assumption of this scenario)\nfirst stderr line (redacted): %s",
				match[1], code, s.redact(first.line))
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
		what, p.describeStatus(), s.describeOutputText("stderr", &p.stderr))
}

// decide is the step in which the browser session approves or denies the
// device login, with the browser cookies and the CSRF token
// (api/pm.openapi.yaml:95-134). It needs status 204.
func (s *deviceLoginScenario) decide(ctx context.Context, decision string) error {
	if s.browser == nil || s.browser.csrf == "" {
		return errors.New("the scenario has no browser session")
	}
	if s.challenge == nil {
		return errors.New("the scenario did not start a device login")
	}
	path := map[string]string{"approves": "/v1/sessions/device/approve", "denies": "/v1/sessions/device/deny"}[decision]
	body, err := json.Marshal(map[string]string{"challenge_id": s.challenge.challengeID, "user_code": s.challenge.userCode})
	if err != nil {
		return fmt.Errorf("encode the device decision: %w", err)
	}
	header := jsonHeaders(s.browser.origin)
	header.Set("X-CSRF-Token", s.browser.csrf)
	status, _, answer, err := s.browser.send(ctx, http.MethodPost, s.browser.origin+path, body, header)
	clear(body)
	if err != nil {
		return err
	}
	return s.checkDecision(path, status, answer)
}

func (s *deviceLoginScenario) checkDecision(path string, status int, answer []byte) error {
	if status != http.StatusNoContent {
		return fmt.Errorf("POST %s returned status %d, want 204: %s", path, status, describeProblem(answer))
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
		return fmt.Errorf("the device login exchange ended before the step sent SIGINT: %s\n%s", p.describeStatus(), s.describeOutputText("stderr", &p.stderr))
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
	timer := time.NewTimer(deviceLoginExitLimit)
	defer timer.Stop()
	if !p.awaitEnd(timer.C) {
		return fmt.Errorf("the device login exchange did not end within %s; %s\n%s", deviceLoginExitLimit, p.describeStatus(), s.describeOutputText("stderr", &p.stderr))
	}
	if !p.status.exited || p.status.code != want {
		return fmt.Errorf("%s, want exit code %d\n%s", p.describeStatus(), want, s.describeOutputText("stderr", &p.stderr))
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
		return fmt.Errorf("the device login exchange stderr does not name %s\n%s", code, s.describeOutputText("stderr", &p.stderr))
	}
	return nil
}

// runWithStoredSession is the step that runs one pm command against the
// started pm-server with the default OS session store, so that the command
// reads the PM session that login-exchange stored in the private keyring. It
// writes the shared result state.
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
	s.shared.result = commandResult{ran: true, exitCode: code, stdout: stdout, stderr: stderr, deploymentID: s.shared.deployment.deploymentID}
	return nil
}

// close ends a login-exchange process that is still running, with SIGKILL,
// and waits for it with the kill limit. It closes the browser connections.
func (s *deviceLoginScenario) close() error {
	var errs []error
	if p := s.exchange; p != nil {
		if p.stdin != nil {
			_ = p.stdin.Close()
			p.stdin = nil
		}
		select {
		case <-p.exited:
		default:
			if err := p.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				errs = append(errs, fmt.Errorf("end the device login exchange: %w", err))
			}
			timer := time.NewTimer(killLimit)
			select {
			case <-p.exited:
			case <-timer.C:
				errs = append(errs, fmt.Errorf("the device login exchange did not end within %s after SIGKILL", killLimit))
			}
			timer.Stop()
		}
	}
	if s.browser != nil && s.browser.client != nil {
		s.browser.client.CloseIdleConnections()
	}
	s.browser, s.challenge, s.exchange = nil, nil, nil
	return errors.Join(errs...)
}
