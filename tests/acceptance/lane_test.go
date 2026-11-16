package acceptance

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/store"
)

const (
	laneTaskQueue     = "pm-control"
	laneStartTimeout  = 10 * time.Minute
	laneReadyTimeout  = 90 * time.Second
	laneReadyInterval = 200 * time.Millisecond
	laneStopTimeout   = 45 * time.Second
	stackAdminRole    = "pm_stack_admin"
)

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
	serverBinary, err := buildBinary(ctx, l.bins, filepath.Join(l.dir, "bin"), "pm-server", "./cmd/pm-server", false)
	if err != nil {
		return err
	}
	workerBinary, err := buildBinary(ctx, l.bins, filepath.Join(l.dir, "bin"), "pm-worker", "./cmd/pm-worker", false)
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
