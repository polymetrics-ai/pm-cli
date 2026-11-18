package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
)

const (
	deviceTestInstance  = "instance-device-wait"
	deviceTestProfile   = "device-wait"
	deviceTestChallenge = "challenge-device-wait"
	deviceTestProof     = "device-proof-must-stay-in-memory"
	deviceTestSession   = "pm-session-credential-must-stay-private"
	waitingNotice       = "before the next exchange request."
	malformedTimingText = "error: device login wait stopped. The server answer did not have exactly one Retry-After value from 1 to 60 seconds.\n"
	unknownOutcomeText  = "error: device login exchange outcome is unknown. The exchange request may have reached the server, but the CLI received no valid answer. Start a new device login with pm sessions login-start.\n"
	notStoredText       = "error: device login exchange succeeded, but the CLI did not store the PM session. Start a new device login with pm sessions login-start.\n"
	waitCancelledText   = "error: device login was cancelled before the next exchange request. The CLI sent no request after the cancellation.\n"
	readCancelledText   = "error: device login was cancelled while the CLI read the proof from stdin. The CLI sent no exchange request.\n"
)

// deviceAnswer is one scripted answer of the fake exchange endpoint. A
// dropped answer closes the connection after the server reads the request.
type deviceAnswer struct {
	status     int
	code       string
	action     string
	retryAfter []string
	drop       bool
}

func pendingAnswer(retryAfter ...string) deviceAnswer {
	return deviceAnswer{status: http.StatusConflict, code: "device_authorization_pending", action: "poll_after_interval", retryAfter: retryAfter}
}

func tooSoonAnswer(retryAfter ...string) deviceAnswer {
	return deviceAnswer{status: http.StatusTooManyRequests, code: "device_poll_too_soon", action: "poll_after_interval", retryAfter: retryAfter}
}

func approvedAnswer() deviceAnswer { return deviceAnswer{status: http.StatusOK} }

// deviceExchangeRecorder is a fake PM server. It records every request and
// every exchange body, and it fails the test on an exchange request that has
// no scripted answer.
type deviceExchangeRecorder struct {
	t        *testing.T
	mu       sync.Mutex
	answers  []deviceAnswer
	requests []string
	bodies   []pmapi.DeviceProof
	server   *httptest.Server
}

func newDeviceExchangeRecorder(t *testing.T, answers ...deviceAnswer) *deviceExchangeRecorder {
	t.Helper()
	recorder := &deviceExchangeRecorder{t: t, answers: answers}
	recorder.server = httptest.NewTLSServer(http.HandlerFunc(recorder.serve))
	t.Cleanup(recorder.server.Close)
	return recorder
}

func (recorder *deviceExchangeRecorder) serve(writer http.ResponseWriter, request *http.Request) {
	recorder.mu.Lock()
	recorder.requests = append(recorder.requests, request.Method+" "+request.URL.Path)
	recorder.mu.Unlock()
	switch request.URL.Path {
	case "/v1/info":
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(pmapi.ServerInfo{
			ProtocolMajor:    pmapi.ProtocolMajor,
			ProtocolVersion:  pmapi.ServerInfoProtocolVersion(pmapi.WireVersion),
			InstanceId:       deviceTestInstance,
			Features:         []string{"exact-values", "wire-problem-v1"},
			RequiredFeatures: []string{"exact-values"},
		})
	case "/v1/sessions/device/exchange":
		var body pmapi.DeviceProof
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			recorder.t.Errorf("decode device exchange body: %v", err)
		}
		recorder.mu.Lock()
		recorder.bodies = append(recorder.bodies, body)
		index := len(recorder.bodies) - 1
		var answer deviceAnswer
		scripted := index < len(recorder.answers)
		if scripted {
			answer = recorder.answers[index]
		}
		recorder.mu.Unlock()
		if !scripted {
			recorder.t.Errorf("device exchange request %d has no scripted answer", index+1)
			http.Error(writer, "unexpected request", http.StatusInternalServerError)
			return
		}
		recorder.write(writer, answer)
	default:
		http.NotFound(writer, request)
	}
}

func (recorder *deviceExchangeRecorder) write(writer http.ResponseWriter, answer deviceAnswer) {
	if answer.drop {
		hijacker, ok := writer.(http.Hijacker)
		if !ok {
			recorder.t.Error("fake server cannot drop the connection")
			return
		}
		connection, _, err := hijacker.Hijack()
		if err != nil {
			recorder.t.Errorf("hijack device exchange connection: %v", err)
			return
		}
		_ = connection.Close()
		return
	}
	for _, value := range answer.retryAfter {
		writer.Header().Add("Retry-After", value)
	}
	if answer.status == http.StatusOK {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(pmapi.PMDeviceSession{
			ExpiresAt:           time.Now().Add(time.Hour).UTC().Truncate(time.Second),
			PmSessionCredential: deviceTestSession,
			Principal:           pmapi.Principal{Kind: pmapi.PrincipalKindUser, PrincipalId: "ada-device-wait"},
			SessionId:           "session-device-wait",
		})
		return
	}
	writer.Header().Set("Content-Type", "application/problem+json")
	writer.WriteHeader(answer.status)
	_ = json.NewEncoder(writer).Encode(pmapi.Problem{
		Type:            "https://pm.invalid/problems/" + answer.code,
		Title:           "Device login answer",
		Status:          answer.status,
		Detail:          "The device login exchange did not complete.",
		Code:            answer.code,
		IncidentId:      "incident-device-wait",
		SafeRetryAction: answer.action,
	})
}

func (recorder *deviceExchangeRecorder) exchangeCount() int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return len(recorder.bodies)
}

func (recorder *deviceExchangeRecorder) exchangeBodies() []pmapi.DeviceProof {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]pmapi.DeviceProof(nil), recorder.bodies...)
}

func (recorder *deviceExchangeRecorder) requestCount() int {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return len(recorder.requests)
}

// deviceRun is one in-process run of `pm sessions login-exchange` against the
// fake server. A developer check, never acceptance proof.
type deviceRun struct {
	t        *testing.T
	recorder *deviceExchangeRecorder
	profiles string
	sessions *pmapi.MemorySessionStore
	store    pmapi.SessionStore
	pauses   []time.Duration
	pause    func(context.Context, time.Duration) error
	stdin    io.Reader
	stderr   io.Writer
}

func newDeviceRun(t *testing.T, recorder *deviceExchangeRecorder) *deviceRun {
	t.Helper()
	profiles := filepath.Join(t.TempDir(), "profiles.json")
	handle, err := pmapi.DefaultSessionHandle(deviceTestProfile, deviceTestInstance)
	if err != nil {
		t.Fatalf("session handle: %v", err)
	}
	if err := pmapi.AddServerProfile(profiles, pmapi.ServerProfile{
		Name: deviceTestProfile, ServerURL: recorder.server.URL, InstanceID: deviceTestInstance, SessionHandle: handle,
	}); err != nil {
		t.Fatalf("write profile: %v", err)
	}
	run := &deviceRun{t: t, recorder: recorder, profiles: profiles, sessions: pmapi.NewMemorySessionStore()}
	run.store = run.sessions
	run.pause = func(_ context.Context, delay time.Duration) error {
		run.pauses = append(run.pauses, delay)
		return nil
	}
	return run
}

func (run *deviceRun) execute(ctx context.Context, extra ...string) (int, string, string) {
	run.t.Helper()
	args := append([]string{
		"sessions", "login-exchange",
		"--server-profile", deviceTestProfile,
		"--session-store", "memory",
		"--challenge-id", deviceTestChallenge,
	}, extra...)
	stdin := run.stdin
	if stdin == nil {
		stdin = strings.NewReader(deviceTestProof + "\n")
	}
	var stdout, stderr bytes.Buffer
	var diagnostics io.Writer = &stderr
	if run.stderr != nil {
		diagnostics = io.MultiWriter(&stderr, run.stderr)
	}
	code := Execute(ctx, Options{
		Args: args, Stdin: stdin, Stdout: &stdout, Stderr: diagnostics,
		ProfilesPath: run.profiles, HTTPClient: run.recorder.server.Client(),
		SessionStore: func(mode pmapi.SessionStoreMode) (pmapi.SessionStore, error) {
			if mode != pmapi.SessionStoreMemory {
				run.t.Errorf("session mode = %q, want memory", mode)
			}
			return run.store, nil
		},
		devicePollPause: run.pause,
	})
	return code, stdout.String(), stderr.String()
}

func (run *deviceRun) requireNoSecret(outputs ...string) {
	run.t.Helper()
	for _, output := range outputs {
		if strings.Contains(output, deviceTestProof) || strings.Contains(output, deviceTestSession) {
			run.t.Fatalf("output carries the device proof or the PM session credential: %q", output)
		}
	}
}

func noticeLines(stderr string) []string {
	var lines []string
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(line, "notice: ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// D4-2, D4-4, D4-9, D4-10: --wait follows the server cadence and stores the
// session after the first successful exchange.
func TestDeviceLoginExchangeWaitFollowsServerCadence(t *testing.T) {
	t.Parallel()
	recorder := newDeviceExchangeRecorder(t, pendingAnswer("2"), tooSoonAnswer("3"), approvedAnswer())
	run := newDeviceRun(t, recorder)
	code, stdout, stderr := run.execute(t.Context(), "--wait=true")
	if code != 0 {
		t.Fatalf("exit = %d, stderr %q", code, stderr)
	}
	if got := recorder.exchangeCount(); got != 3 {
		t.Fatalf("exchange requests = %d, want 3", got)
	}
	for index, body := range recorder.exchangeBodies() {
		if body.ChallengeId != deviceTestChallenge || body.Proof != deviceTestProof {
			t.Fatalf("exchange body %d does not carry the one proof read from stdin", index+1)
		}
	}
	if want := []time.Duration{2 * time.Second, 8 * time.Second}; len(run.pauses) != 2 ||
		run.pauses[0] != want[0] || run.pauses[1] != want[1] {
		t.Fatalf("waits = %v, want %v (Retry-After, then Retry-After plus 5s)", run.pauses, want)
	}
	notices := noticeLines(stderr)
	wantNotices := []string{
		"notice: device login exchange received device_authorization_pending. The CLI waits 2s before the next exchange request.",
		"notice: device login exchange received device_poll_too_soon. The CLI waits 8s before the next exchange request.",
	}
	if len(notices) != 2 || notices[0] != wantNotices[0] || notices[1] != wantNotices[1] {
		t.Fatalf("notice lines = %q, want %q", notices, wantNotices)
	}
	run.requireNoSecret(stdout, stderr)
	if !strings.Contains(stdout, "session-device-wait") {
		t.Fatalf("stdout = %q, want the safe session identity", stdout)
	}
	handle, _ := pmapi.DefaultSessionHandle(deviceTestProfile, deviceTestInstance)
	key, _ := pmapi.NewSessionKey(deviceTestInstance, handle)
	stored, err := run.sessions.Load(t.Context(), key)
	if err != nil || stored.PmSessionCredential != deviceTestSession {
		t.Fatalf("stored session = %v", err)
	}
}

// D4-1: without --wait the command sends one request, as before. This guard
// also passes on the old code.
func TestDeviceLoginExchangeWithoutWaitSendsOneRequest(t *testing.T) {
	t.Parallel()
	recorder := newDeviceExchangeRecorder(t, pendingAnswer("2"))
	run := newDeviceRun(t, recorder)
	code, stdout, stderr := run.execute(t.Context())
	if code != 4 || recorder.exchangeCount() != 1 || len(run.pauses) != 0 || len(noticeLines(stderr)) != 0 {
		t.Fatalf("exit %d, exchanges %d, waits %v, stderr %q", code, recorder.exchangeCount(), run.pauses, stderr)
	}
	if !strings.Contains(stderr, "device_authorization_pending") {
		t.Fatalf("stderr = %q, want the pending problem", stderr)
	}
	run.requireNoSecret(stdout, stderr)
}

// D4-3: the client accepts exactly one Retry-After of 1 to 60 whole seconds.
func TestDeviceLoginExchangeWaitRejectsMalformedTiming(t *testing.T) {
	t.Parallel()
	for _, answer := range []deviceAnswer{
		pendingAnswer(),
		pendingAnswer("abc"),
		pendingAnswer("0"),
		pendingAnswer("61"),
		pendingAnswer("-1"),
		pendingAnswer("1.5"),
		pendingAnswer("05"),
		pendingAnswer("2, 3"),
		pendingAnswer("2", "3"),
		pendingAnswer("Wed, 21 Oct 2026 07:28:00 GMT"),
		tooSoonAnswer(),
		tooSoonAnswer("61"),
	} {
		recorder := newDeviceExchangeRecorder(t, answer)
		run := newDeviceRun(t, recorder)
		code, stdout, stderr := run.execute(t.Context(), "--wait=true")
		if code != 6 || stderr != malformedTimingText || recorder.exchangeCount() != 1 || len(run.pauses) != 0 {
			t.Errorf("%s Retry-After %q: exit %d, exchanges %d, waits %v, stderr %q",
				answer.code, answer.retryAfter, code, recorder.exchangeCount(), run.pauses, stderr)
		}
		run.requireNoSecret(stdout, stderr)
	}
	recorder := newDeviceExchangeRecorder(t, pendingAnswer("1"), tooSoonAnswer("60"), approvedAnswer())
	run := newDeviceRun(t, recorder)
	code, _, stderr := run.execute(t.Context(), "--wait=true", "--request-timeout", "5m0s")
	if code != 0 || len(run.pauses) != 2 || run.pauses[0] != time.Second || run.pauses[1] != 65*time.Second {
		t.Fatalf("bounds 1 and 60: exit %d, waits %v, stderr %q", code, run.pauses, stderr)
	}
}

// D4-5: every answer other than pending and too soon stops the wait with the
// existing exit code, also when it carries a Retry-After value.
func TestDeviceLoginExchangeWaitStopsOnOtherAnswers(t *testing.T) {
	t.Parallel()
	for _, stop := range []struct {
		answer deviceAnswer
		exit   int
	}{
		{deviceAnswer{status: http.StatusForbidden, code: "device_authorization_denied", action: "restart_login", retryAfter: []string{"1"}}, 3},
		{deviceAnswer{status: http.StatusGone, code: "login_challenge_expired", action: "restart_login", retryAfter: []string{"1"}}, 6},
		{deviceAnswer{status: http.StatusConflict, code: "login_challenge_used", action: "restart_login", retryAfter: []string{"1"}}, 4},
		{deviceAnswer{status: http.StatusTooManyRequests, code: "authentication_rate_limited", action: "retry_after_backoff", retryAfter: []string{"1"}}, 5},
		{deviceAnswer{status: http.StatusBadRequest, code: "login_challenge_invalid", action: "restart_login", retryAfter: []string{"1"}}, 2},
	} {
		recorder := newDeviceExchangeRecorder(t, pendingAnswer("1"), stop.answer)
		run := newDeviceRun(t, recorder)
		code, stdout, stderr := run.execute(t.Context(), "--wait=true")
		if code != stop.exit || recorder.exchangeCount() != 2 || len(run.pauses) != 1 ||
			!strings.Contains(stderr, "error: "+stop.answer.code+": ") {
			t.Errorf("%s: exit %d (want %d), exchanges %d (want 2), waits %v, stderr %q",
				stop.answer.code, code, stop.exit, recorder.exchangeCount(), run.pauses, stderr)
		}
		run.requireNoSecret(stdout, stderr)
	}
}

// D4-6: a transport error after the send stops at once. The command says that
// the outcome is unknown and never repeats the exchange.
func TestDeviceLoginExchangeTransportErrorIsNotRepeated(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		answers   []deviceAnswer
		args      []string
		exchanges int
	}{
		{name: "with --wait", answers: []deviceAnswer{pendingAnswer("1"), {drop: true}}, args: []string{"--wait=true"}, exchanges: 2},
		{name: "without --wait", answers: []deviceAnswer{{drop: true}}, exchanges: 1},
	} {
		recorder := newDeviceExchangeRecorder(t, test.answers...)
		run := newDeviceRun(t, recorder)
		code, stdout, stderr := run.execute(t.Context(), test.args...)
		if code != 5 || !strings.HasSuffix(stderr, unknownOutcomeText) || recorder.exchangeCount() != test.exchanges ||
			len(noticeLines(stderr)) != test.exchanges-1 {
			t.Errorf("%s: exit %d, exchanges %d (want %d), stderr %q", test.name, code, recorder.exchangeCount(), test.exchanges, stderr)
		}
		run.requireNoSecret(stdout, stderr)
	}
}

// D4-7: if the next wait would end after the one total deadline, the command
// stops at once with the timeout result and sends no further request.
func TestDeviceLoginExchangeWaitStopsBeforeDeadline(t *testing.T) {
	t.Parallel()
	recorder := newDeviceExchangeRecorder(t, pendingAnswer("5"))
	run := newDeviceRun(t, recorder)
	code, stdout, stderr := run.execute(t.Context(), "--wait=true", "--request-timeout", "1s")
	if code != 8 || stderr != "error: device login wait stopped. The next exchange request does not fit in the --request-timeout limit. Run the command again with a longer --request-timeout.\n" || recorder.exchangeCount() != 1 || len(run.pauses) != 0 {
		t.Fatalf("exit %d, exchanges %d, waits %v, stderr %q", code, recorder.exchangeCount(), run.pauses, stderr)
	}
	run.requireNoSecret(stdout, stderr)
}

// cancelOnNotice cancels the command context when the wait notice appears,
// as an interrupt after the notice line does in the real binary.
type cancelOnNotice struct {
	cancel context.CancelFunc
}

func (writer cancelOnNotice) Write(data []byte) (int, error) {
	if bytes.Contains(data, []byte(waitingNotice)) {
		writer.cancel()
	}
	return len(data), nil
}

// D4-8 and R2-2: a cancellation during the wait stops it at once with the
// existing cancelled exit code. The recorder shows that no exchange request
// follows the cancellation. The real wait runs here; the server asks for 20s,
// so a wait that ignores the cancellation sends a second request.
func TestDeviceLoginExchangeWaitCancellationSendsNoFurtherRequest(t *testing.T) {
	t.Parallel()
	recorder := newDeviceExchangeRecorder(t, pendingAnswer("20"))
	run := newDeviceRun(t, recorder)
	run.pause = nil
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	run.stderr = cancelOnNotice{cancel: cancel}
	code, stdout, stderr := run.execute(ctx, "--wait=true")
	if code != 6 || recorder.exchangeCount() != 1 || len(noticeLines(stderr)) != 1 ||
		!strings.HasSuffix(stderr, waitCancelledText) {
		t.Fatalf("exit %d, exchanges %d, stderr %q", code, recorder.exchangeCount(), stderr)
	}
	run.requireNoSecret(stdout, stderr)

	cancelled, stop := context.WithCancel(t.Context())
	stop()
	if err := pauseForDevicePoll(cancelled, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("pause with a cancelled context = %v, want context.Canceled", err)
	}
}

// blockingProof is stdin that the caller never closes during the read.
type blockingProof struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (reader *blockingProof) Read([]byte) (int, error) {
	reader.once.Do(func() { close(reader.entered) })
	<-reader.release
	return 0, io.EOF
}

// D4-13 and R2-3: a cancellation during the stdin read ends the command at
// once with the existing cancelled exit code, before any request.
func TestDeviceLoginExchangeCancelledProofReadSendsNoRequest(t *testing.T) {
	t.Parallel()
	recorder := newDeviceExchangeRecorder(t)
	run := newDeviceRun(t, recorder)
	stdin := &blockingProof{entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(stdin.release) })
	run.stdin = stdin
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		code   int
		stdout string
		stderr string
	}
	done := make(chan result, 1)
	go func() {
		code, stdout, stderr := run.execute(ctx, "--wait=true")
		done <- result{code: code, stdout: stdout, stderr: stderr}
	}()
	select {
	case <-stdin.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("login-exchange did not start to read the proof")
	}
	cancel()
	var finished result
	select {
	case finished = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("login-exchange did not end after a cancellation during the proof read")
	}
	if finished.code != 6 || finished.stderr != readCancelledText || recorder.requestCount() != 0 {
		t.Fatalf("exit %d, requests %d, stderr %q", finished.code, recorder.requestCount(), finished.stderr)
	}
	run.requireNoSecret(finished.stdout, finished.stderr)
}

// failingSessionStore fails every store. Its cancel, when set, ends the
// command context during the store, as an interrupt or the deadline can.
type failingSessionStore struct {
	cancel context.CancelFunc
}

func (store failingSessionStore) Store(ctx context.Context, _ pmapi.SessionKey, _ pmapi.PMDeviceSession) error {
	if store.cancel != nil {
		store.cancel()
		return ctx.Err()
	}
	return pmapi.ErrSessionStoreUnavailable
}

func (failingSessionStore) Load(context.Context, pmapi.SessionKey) (pmapi.PMDeviceSession, error) {
	return pmapi.PMDeviceSession{}, pmapi.ErrSessionStoreUnavailable
}

func (failingSessionStore) Delete(context.Context, pmapi.SessionKey) error {
	return pmapi.ErrSessionStoreUnavailable
}

// D4-14 and R2-3: after a successful exchange the challenge is consumed. A
// failing or cancelled store says that the login must start again, and the
// recorder shows exactly one successful exchange and no repeat.
func TestDeviceLoginExchangeStoreFailureIsNotRepeated(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		answers   []deviceAnswer
		args      []string
		cancel    bool
		exit      int
		exchanges int
	}{
		{name: "failing store with --wait", answers: []deviceAnswer{pendingAnswer("1"), tooSoonAnswer("1"), approvedAnswer()}, args: []string{"--wait=true"}, exit: 5, exchanges: 3},
		{name: "failing store without --wait", answers: []deviceAnswer{approvedAnswer()}, exit: 5, exchanges: 1},
		{name: "cancellation during the store", answers: []deviceAnswer{pendingAnswer("1"), approvedAnswer()}, args: []string{"--wait=true"}, cancel: true, exit: 6, exchanges: 2},
	} {
		recorder := newDeviceExchangeRecorder(t, test.answers...)
		run := newDeviceRun(t, recorder)
		ctx, cancel := context.WithCancel(t.Context())
		store := failingSessionStore{}
		if test.cancel {
			store.cancel = cancel
		}
		run.store = store
		code, stdout, stderr := run.execute(ctx, test.args...)
		cancel()
		if code != test.exit || !strings.HasSuffix(stderr, notStoredText) || recorder.exchangeCount() != test.exchanges ||
			len(noticeLines(stderr)) != test.exchanges-1 {
			t.Errorf("%s: exit %d (want %d), exchanges %d (want %d), stderr %q",
				test.name, code, test.exit, recorder.exchangeCount(), test.exchanges, stderr)
		}
		run.requireNoSecret(stdout, stderr)
	}
}
