package acceptance

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/store"
)

const (
	laneTaskQueue       = "pm-control"
	laneIdentityRef     = "secret://lane/identity"
	laneEncryptionRef   = "secret://lane/encryption"
	laneStartTimeout    = 10 * time.Minute
	laneReadyTimeout    = 90 * time.Second
	laneReadyInterval   = 200 * time.Millisecond
	laneStopTimeout     = 45 * time.Second
	laneOutputLimit     = 64 << 10
	laneFileLimit       = 2 << 20
	applicationDatabase = "pm_app"
	stackAdminRole      = "pm_stack_admin"
)

// laneWorkerControls are the bounded worker controls of the lane. The stack
// configuration has no worker block. These values are the same as the test
// worker configuration in internal/testfixture.
var laneWorkerControls = json.RawMessage(`{
  "task_queue": "pm-control",
  "dial_timeout": "5s",
  "drain_timeout": "5s",
  "max_concurrent_activities": 32,
  "max_concurrent_workflow_tasks": 4,
  "activity_timeouts": {
    "schedule_to_close": "20s",
    "start_to_close": "10s",
    "heartbeat": "2s",
    "transport": "5s"
  }
}`)

// lane runs the real pm-server (API and relay) and pm-worker binaries on the
// job-owned dependency stack. It starts on the first step that needs it, so a
// run of only the offline scenarios needs no stack. The lane starts once per
// suite. A failed start stays failed: every later lane step reports the same
// error, and the lane never tries to start again.
type lane struct {
	bins binaries

	started  bool
	startErr error

	dir          string
	deploymentID string
	origin       string
	caFile       string
	authority    *laneAuthority
	identity     *laneIdentityProvider
	edge         *httptest.Server
	observer     *observer
	server       *laneProcess
	worker       *laneProcess
}

func newLane(bins binaries) *lane {
	return &lane{bins: bins}
}

// ensure starts the lane once and then checks that both processes still run.
func (l *lane) ensure(ctx context.Context) error {
	if l.startErr != nil {
		return fmt.Errorf("the lane failed to start earlier in this suite: %w", l.startErr)
	}
	if !l.started {
		l.started = true
		if err := l.start(ctx); err != nil {
			l.startErr = err
			return err
		}
	}
	for _, process := range []*laneProcess{l.server, l.worker} {
		if process.exited() {
			return fmt.Errorf("%s is no longer running: %v\n%s", process.name, process.waitErr, process.logs)
		}
	}
	return nil
}

// workerIdentity is the Temporal identity that pm-worker gives itself.
func (l *lane) workerIdentity() string {
	return l.deploymentID + "/pm-worker"
}

func (l *lane) start(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, laneStartTimeout)
	defer cancel()
	stack, err := readStackInputs()
	if err != nil {
		return err
	}
	if l.dir, err = os.MkdirTemp("", "pm-acceptance-lane-"); err != nil {
		return fmt.Errorf("create the lane directory: %w", err)
	}
	for _, directory := range []string{"bin", "home", "tmp"} {
		if err := os.Mkdir(filepath.Join(l.dir, directory), 0o700); err != nil {
			return fmt.Errorf("create the lane %s directory: %w", directory, err)
		}
	}
	serverBinary, err := buildBinary(ctx, l.bins, filepath.Join(l.dir, "bin"), "pm-server", "./cmd/pm-server")
	if err != nil {
		return err
	}
	workerBinary, err := buildBinary(ctx, l.bins, filepath.Join(l.dir, "bin"), "pm-worker", "./cmd/pm-worker")
	if err != nil {
		return err
	}
	// pm-server and pm-worker never migrate. The lane acts as the operator
	// and applies the PM application migrations with the migrator role.
	if err := store.Migrate(ctx, stack.postgres.dsn(store.MigratorRole, stack.postgres.migratorPassword, applicationDatabase)); err != nil {
		return fmt.Errorf("migrate the PM application store: %w", err)
	}
	if l.observer, err = openObserver(ctx, stack); err != nil {
		return err
	}
	if l.authority, err = newLaneAuthority(); err != nil {
		return err
	}
	l.caFile = filepath.Join(l.dir, "lane-ca.pem")
	if err := writePrivateFile(l.caFile, l.authority.pem); err != nil {
		return err
	}
	if l.identity, err = newLaneIdentityProvider(l.authority); err != nil {
		return err
	}
	backend, err := reserveLoopbackAddress()
	if err != nil {
		return err
	}
	if l.edge, err = newLaneEdge(l.authority, backend); err != nil {
		return err
	}
	l.origin = l.edge.URL
	l.deploymentID = "pm-lane-" + randomHex(6)
	secrets, err := l.writeSecrets(stack)
	if err != nil {
		return err
	}
	configuration, err := l.writeConfiguration(stack, backend)
	if err != nil {
		return err
	}

	if l.server, err = l.startProcess("pm-server", serverBinary, configuration, secrets); err != nil {
		return err
	}
	if err := awaitLane(ctx, []*laneProcess{l.server}, l.serverLive); err != nil {
		return fmt.Errorf("pm-server did not become live: %w", err)
	}
	if l.worker, err = l.startProcess("pm-worker", workerBinary, configuration, secrets); err != nil {
		return err
	}
	if err := awaitLane(ctx, []*laneProcess{l.server, l.worker}, func(ctx context.Context) error {
		return l.observer.seesPoller(ctx, laneTaskQueue, l.workerIdentity())
	}); err != nil {
		return fmt.Errorf("the observer did not see the pm-worker poller: %w", err)
	}
	return nil
}

// awaitLane is the startup gate of the lane. Neither binary reports when it
// is ready, so the gate repeats one read-only observation until it passes, a
// process exits, or the deadline passes. It never repeats a scenario action
// and it never hides a failure: a scenario step observes once.
func awaitLane(parent context.Context, processes []*laneProcess, ready func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(parent, laneReadyTimeout)
	defer cancel()
	ticker := time.NewTicker(laneReadyInterval)
	defer ticker.Stop()
	for {
		observation := ready(ctx)
		if observation == nil {
			return nil
		}
		for _, process := range processes {
			if process.exited() {
				return fmt.Errorf("%s exited: %v\n%s", process.name, process.waitErr, process.logs)
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w; last observation: %v", ctx.Err(), observation)
		case <-ticker.C:
		}
	}
}

// serverLive reads /health/live through the lane edge.
func (l *lane) serverLive(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, l.origin+"/health/live", nil)
	if err != nil {
		return err
	}
	httpClient := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: l.authority.pool()}},
	}
	defer httpClient.CloseIdleConnections()
	response, err := httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("/health/live returned %s", response.Status)
	}
	return nil
}

// writeSecrets makes the lane operator secret root. It copies only the stack
// secret members that the two processes read, and it adds the lane identity
// provider secret, its CA and a new wrapping key. pm-server opens the root
// with os.Root, so the members are copies, not links.
func (l *lane) writeSecrets(stack stackInputs) (string, error) {
	root := filepath.Join(l.dir, "secrets")
	copies := []struct {
		reference string
		members   []string
	}{
		{stack.postgresRef, []string{"pm-app-endpoint"}},
		{stack.temporalRef, []string{"ca.pem", "client.pem", "client-key.pem"}},
		{stack.artifactsRef, []string{"ca.pem", "access-key", "secret-key"}},
	}
	for _, bundle := range copies {
		relative, err := secretBundlePath(bundle.reference)
		if err != nil {
			return "", err
		}
		for _, member := range bundle.members {
			contents, err := readBoundedFile(filepath.Join(stack.secretsDir, relative, member), laneFileLimit)
			if err != nil {
				return "", err
			}
			if err := writeSecretMember(root, relative, member, contents); err != nil {
				return "", err
			}
		}
	}
	identity, err := secretBundlePath(laneIdentityRef)
	if err != nil {
		return "", err
	}
	if err := writeSecretMember(root, identity, "oidc-client-secret", []byte(l.identity.clientSecret)); err != nil {
		return "", err
	}
	if err := writeSecretMember(root, identity, "ca.pem", l.authority.pem); err != nil {
		return "", err
	}
	encryption, err := secretBundlePath(laneEncryptionRef)
	if err != nil {
		return "", err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("generate the lane wrapping key: %w", err)
	}
	if err := writeSecretMember(root, encryption, "wrapping-key", key); err != nil {
		return "", err
	}
	return root, nil
}

// writeConfiguration writes the lane copy of the stack configuration. The
// copy keeps the stack PostgreSQL, Temporal, artifact, admission and limit
// settings. It changes the deployment identity, the public origin, the
// identity provider and the wrapping key reference, and adds the server and
// worker controls. The stack configuration file does not change.
func (l *lane) writeConfiguration(stack stackInputs, backend string) (string, error) {
	document := maps.Clone(stack.document)
	values := map[string]any{
		"deployment_id": l.deploymentID,
		"public_origin": l.origin,
		"server": map[string]any{
			"listen":          backend,
			"trusted_proxies": []string{"127.0.0.1/32"},
			"drain_timeout":   "5s",
		},
		"identity": map[string]any{
			"issuer":            l.identity.issuer,
			"client_id":         laneOIDCClientID,
			"client_secret_ref": laneIdentityRef,
			"ca_ref":            laneIdentityRef,
		},
		"encryption": map[string]any{"wrapping_key_ref": laneEncryptionRef},
		"worker":     laneWorkerControls,
	}
	for key, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", fmt.Errorf("encode the lane %s configuration: %w", key, err)
		}
		document[key] = encoded
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode the lane configuration: %w", err)
	}
	path := filepath.Join(l.dir, "pm-lane.json")
	if err := writePrivateFile(path, append(encoded, '\n')); err != nil {
		return "", err
	}
	return path, nil
}

func (l *lane) startProcess(name, binary, configuration, secrets string) (*laneProcess, error) {
	home := filepath.Join(l.dir, "home")
	command := exec.Command(binary, "--config", configuration, "--secrets-directory", secrets)
	command.Dir = home
	// A clean environment: no proxy, no inherited PM or cloud setting.
	command.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LANG=C", "TMPDIR=" + filepath.Join(l.dir, "tmp")}
	process := &laneProcess{name: name, command: command, logs: &tailBuffer{limit: laneOutputLimit}, done: make(chan struct{})}
	command.Stdout = process.logs
	command.Stderr = process.logs
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", name, err)
	}
	go func() {
		process.waitErr = command.Wait()
		close(process.done)
	}()
	return process, nil
}

// close stops the processes, the edge and the identity provider, drops the
// observer role and removes the lane directory.
func (l *lane) close() error {
	var errs []error
	errs = append(errs, l.worker.stop(), l.server.stop())
	if l.edge != nil {
		l.edge.Close()
	}
	l.identity.close()
	errs = append(errs, l.observer.close())
	if l.dir != "" {
		errs = append(errs, os.RemoveAll(l.dir))
	}
	return errors.Join(errs...)
}

// laneProcess is one real PM process of the lane.
type laneProcess struct {
	name    string
	command *exec.Cmd
	logs    *tailBuffer
	done    chan struct{}
	waitErr error
}

func (process *laneProcess) exited() bool {
	if process == nil {
		return true
	}
	select {
	case <-process.done:
		return true
	default:
		return false
	}
}

// stop sends SIGTERM and waits for the process to drain and exit. A process
// that exited with an error, or that does not stop in time, is a failure.
func (process *laneProcess) stop() error {
	if process == nil {
		return nil
	}
	if !process.exited() {
		_ = process.command.Process.Signal(syscall.SIGTERM)
	}
	timer := time.NewTimer(laneStopTimeout)
	defer timer.Stop()
	select {
	case <-process.done:
	case <-timer.C:
		_ = process.command.Process.Kill()
		<-process.done
		return fmt.Errorf("%s did not stop within %s after SIGTERM\n%s", process.name, laneStopTimeout, process.logs)
	}
	if process.waitErr != nil {
		return fmt.Errorf("%s exited with an error: %v\n%s", process.name, process.waitErr, process.logs)
	}
	return nil
}

// tailBuffer keeps the last limit bytes of a process output.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (buffer *tailBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	buffer.data = append(buffer.data, value...)
	if extra := len(buffer.data) - buffer.limit; extra > 0 {
		buffer.data = bytes.Clone(buffer.data[extra:])
	}
	return len(value), nil
}

func (buffer *tailBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return string(buffer.data)
}

// newLaneEdge starts the HTTPS edge of the lane. pm-server listens on plain
// HTTP behind a trusted edge, as it does in a deployment. The edge keeps the
// public Host and sets the X-Forwarded headers that pm-server checks.
func newLaneEdge(authority *laneAuthority, backend string) (*httptest.Server, error) {
	certificate, err := authority.serverCertificate("PM acceptance lane edge")
	if err != nil {
		return nil, err
	}
	target := &url.URL{Scheme: "http", Host: backend}
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConnsPerHost:   2,
	}
	edge := httptest.NewUnstartedServer(&httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(target)
			request.Out.Host = request.In.Host
			request.SetXForwarded()
		},
		Transport: transport,
		ErrorHandler: func(response http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(response, "lane backend unavailable", http.StatusBadGateway)
		},
	})
	edge.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	edge.Config.ReadHeaderTimeout = 5 * time.Second
	edge.StartTLS()
	return edge, nil
}

// reserveLoopbackAddress returns a free loopback address for the pm-server
// listener. pm-server opens its own listener, so the lane closes this one.
func reserveLoopbackAddress() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("reserve a loopback address for pm-server: %w", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return "", fmt.Errorf("release the reserved pm-server address: %w", err)
	}
	return address, nil
}

// stackInputs are the job-owned stack inputs that scripts/ci/with-stack-fixture.py
// gives to the suite.
type stackInputs struct {
	document     map[string]json.RawMessage
	secretsDir   string
	postgresRef  string
	temporalRef  string
	artifactsRef string
	postgres     stackPostgres
	temporal     stackTemporal
}

type stackPostgres struct {
	endpoint         *url.URL
	migratorPassword string
	adminPassword    string
}

// dsn returns a connection string for the stack PostgreSQL server. It keeps
// the host, port and TLS settings of the runtime endpoint.
func (postgres stackPostgres) dsn(user, password, database string) string {
	endpoint := *postgres.endpoint
	endpoint.User = url.UserPassword(user, password)
	endpoint.Path = "/" + database
	return endpoint.String()
}

type stackTemporal struct {
	endpoint  string
	namespace string
	bundle    string
}

func readStackInputs() (stackInputs, error) {
	environment := map[string]string{}
	for _, name := range []string{"PM_STACK_TEST_CONFIG", "PM_STACK_SECRETS_DIR", "PM_STORE_TEST_SECRETS_DIR"} {
		value := os.Getenv(name)
		if value == "" || !filepath.IsAbs(value) {
			return stackInputs{}, fmt.Errorf("%s must name an absolute path: run the @stack scenarios through scripts/ci/with-stack-fixture.py", name)
		}
		environment[name] = value
	}
	contents, err := readBoundedFile(environment["PM_STACK_TEST_CONFIG"], laneFileLimit)
	if err != nil {
		return stackInputs{}, err
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(contents, &document); err != nil || document == nil {
		return stackInputs{}, errors.New("the stack configuration is not a JSON object")
	}
	var sections struct {
		Postgres struct {
			EndpointRef string `json:"endpoint_ref"`
		} `json:"postgres"`
		Temporal struct {
			Endpoint          string `json:"endpoint"`
			Namespace         string `json:"namespace"`
			ClientIdentityRef string `json:"client_identity_ref"`
		} `json:"temporal"`
		Artifacts struct {
			CredentialRef string `json:"credential_ref"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(contents, &sections); err != nil {
		return stackInputs{}, fmt.Errorf("decode the stack configuration: %w", err)
	}
	temporalBundle, err := secretBundlePath(sections.Temporal.ClientIdentityRef)
	if err != nil {
		return stackInputs{}, err
	}
	postgres, err := readStackPostgres(environment["PM_STORE_TEST_SECRETS_DIR"])
	if err != nil {
		return stackInputs{}, err
	}
	if sections.Temporal.Endpoint == "" || sections.Temporal.Namespace == "" {
		return stackInputs{}, errors.New("the stack configuration has no Temporal endpoint or namespace")
	}
	return stackInputs{
		document:     document,
		secretsDir:   environment["PM_STACK_SECRETS_DIR"],
		postgresRef:  sections.Postgres.EndpointRef,
		temporalRef:  sections.Temporal.ClientIdentityRef,
		artifactsRef: sections.Artifacts.CredentialRef,
		postgres:     postgres,
		temporal: stackTemporal{
			endpoint:  sections.Temporal.Endpoint,
			namespace: sections.Temporal.Namespace,
			bundle:    filepath.Join(environment["PM_STACK_SECRETS_DIR"], temporalBundle),
		},
	}, nil
}

func readStackPostgres(directory string) (stackPostgres, error) {
	read := func(member string) (string, error) {
		contents, err := readBoundedFile(filepath.Join(directory, member), 64<<10)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(contents)), nil
	}
	endpointText, err := read("pm-app-endpoint")
	if err != nil {
		return stackPostgres{}, err
	}
	endpoint, err := url.Parse(endpointText)
	if err != nil || (endpoint.Scheme != "postgres" && endpoint.Scheme != "postgresql") || endpoint.User == nil ||
		endpoint.User.Username() != store.RuntimeRole || strings.TrimPrefix(endpoint.Path, "/") != applicationDatabase {
		return stackPostgres{}, errors.New("the stack PostgreSQL runtime endpoint is not the pm_app runtime role")
	}
	migrator, err := read("pm-app-migrator-password")
	if err != nil {
		return stackPostgres{}, err
	}
	admin, err := read("superuser-password")
	if err != nil {
		return stackPostgres{}, err
	}
	if migrator == "" || admin == "" {
		return stackPostgres{}, errors.New("a stack PostgreSQL password is empty")
	}
	return stackPostgres{endpoint: endpoint, migratorPassword: migrator, adminPassword: admin}, nil
}

// secretBundlePath maps a secret://bundle/path reference to its relative
// directory, the same way the operator secret reader does.
func secretBundlePath(reference string) (string, error) {
	parsed, err := url.Parse(reference)
	if err != nil || parsed.Scheme != "secret" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("secret reference %q is invalid", reference)
	}
	relative := filepath.FromSlash(parsed.Host + "/" + strings.TrimPrefix(parsed.Path, "/"))
	relative = filepath.Clean(relative)
	if !filepath.IsLocal(relative) {
		return "", fmt.Errorf("secret reference %q leaves the secret root", reference)
	}
	return relative, nil
}

func writeSecretMember(root, bundle, member string, contents []byte) error {
	directory := filepath.Join(root, bundle)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create the lane secret bundle %s: %w", bundle, err)
	}
	return writePrivateFile(filepath.Join(directory, member), contents)
}

// writePrivateFile creates a new mode 0600 file. It never replaces a file.
func writePrivateFile(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	return nil
}

// readBoundedFile reads one regular file of at most limit bytes.
func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", filepath.Base(path), err)
	}
	defer file.Close()
	information, err := file.Stat()
	if err != nil || !information.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	if int64(len(contents)) > limit || len(contents) == 0 {
		return nil, fmt.Errorf("%s is empty or larger than %d bytes", filepath.Base(path), limit)
	}
	return contents, nil
}

func randomHex(size int) string {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		panic(fmt.Sprintf("read random bytes: %v", err))
	}
	return hex.EncodeToString(value)
}
