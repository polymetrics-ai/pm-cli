package acceptance

import (
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/tests/keycloak"
	"github.com/polymetrics-ai/pm-cli/tests/starter"
)

// deployment is one new PM deployment configuration on the job-owned stack,
// apart from the lane of PR #261. Each deployment has its own deployment ID,
// held loopback listener, CA, HTTPS edge and public origin, its own client
// and user in the job-owned Keycloak, and a Temporal namespace that is the
// stack namespace unless a step changes it. pm-server listens on plain HTTP
// behind the HTTPS edge, because authenticated API and browser traffic needs
// such an edge (docs/development/setup.md:149).
type deployment struct {
	stack         stackInputs
	dir           string
	deploymentID  string
	listen        string
	origin        string
	caFile        string
	namespace     string
	secrets       string
	configuration string
	authority     *laneAuthority
	identity      *keycloak.Identity
	edge          *httptest.Server

	// listener is the harness copy of the pm-server listener (ADR 0008). The
	// harness binds it, writes its address into the configuration, and gives
	// pm-server a copy with --listen-fd. releaseListener closes it once.
	listener    *net.TCPListener
	releaseOnce sync.Once
}

// newDeployment makes the deployment directory, the CA, the HTTPS edge, the
// client and the user of the deployment in the job-owned Keycloak, and the
// secret root. The first start writes the configuration.
func newDeployment(stack stackInputs) (*deployment, error) {
	d := &deployment{stack: stack, namespace: stack.temporal.namespace}
	if err := d.open(); err != nil {
		return nil, errors.Join(err, d.close())
	}
	return d, nil
}

func (d *deployment) open() error {
	var err error
	if d.dir, err = os.MkdirTemp("", "pm-acceptance-deployment-"); err != nil {
		return fmt.Errorf("create the deployment directory: %w", err)
	}
	for _, directory := range []string{"home", "tmp"} {
		if err := os.Mkdir(filepath.Join(d.dir, directory), 0o700); err != nil {
			return fmt.Errorf("create the deployment %s directory: %w", directory, err)
		}
	}
	if d.authority, err = newLaneAuthority(); err != nil {
		return err
	}
	d.caFile = filepath.Join(d.dir, "deployment-ca.pem")
	if err := writePrivateFile(d.caFile, d.authority.pem); err != nil {
		return err
	}
	if d.listener, err = holdLoopbackListener(); err != nil {
		return err
	}
	d.listen = d.listener.Addr().String()
	// The harness closes its copy of the listener after pm-server served one
	// request through the edge (ADR 0008, "Ownership of the listener").
	if d.edge, err = newLaneEdge(d.authority, d.listen, d.releaseListener); err != nil {
		return err
	}
	d.origin = d.edge.URL
	// Provision keeps what it created also on an error, so that close
	// removes it.
	if d.identity, err = d.stack.identity.admin.Provision(d.origin + browserCallbackPath); err != nil {
		return err
	}
	d.deploymentID = "pm-acceptance-" + randomHex(6)
	d.secrets, err = d.writeSecrets(d.stack)
	return err
}

// useTemporalNamespace sets the Temporal namespace of the configuration. It
// must come before the first start, because the first start writes the
// configuration.
func (d *deployment) useTemporalNamespace(namespace string) error {
	if d.configuration != "" {
		return errors.New("the deployment configuration is already written")
	}
	d.namespace = namespace
	return nil
}

// configurationPath writes the configuration at the first call and returns
// its path.
func (d *deployment) configurationPath() (string, error) {
	if d.configuration == "" {
		path, err := d.writeConfiguration(d.stack, d.listen)
		if err != nil {
			return "", err
		}
		d.configuration = path
	}
	return d.configuration, nil
}

// home is the working directory and HOME of the started processes.
func (d *deployment) home() string {
	return filepath.Join(d.dir, "home")
}

// processEnvironment is a clean environment: no proxy, no inherited PM or
// cloud setting.
func (d *deployment) processEnvironment() []string {
	return []string{"HOME=" + d.home(), "PATH=/usr/bin:/bin", "LANG=C", "TMPDIR=" + filepath.Join(d.dir, "tmp")}
}

// serverListenerFD is the number of the inherited listener descriptor in
// pm-server.
const serverListenerFD = 4

// giveListener adds the held listener to a start of pm-server: the flag
// --listen-fd 4 and a new copy of the socket as descriptor 4. Go sets
// close-on-exec on that copy, and starter.Start closes it on every path. The
// harness keeps its own copy until releaseListener.
func (d *deployment) giveListener(config *starter.Config) error {
	if d.listener == nil {
		return errors.New("the deployment holds no pm-server listener")
	}
	file, err := d.listener.File()
	if err != nil {
		return fmt.Errorf("copy the held pm-server listener: %w", err)
	}
	config.Args = append(slices.Clone(config.Args), "--listen-fd", strconv.Itoa(serverListenerFD))
	config.ListenFD = serverListenerFD
	config.Listener = file
	return nil
}

// releaseListener closes the harness copy of the pm-server listener. It acts
// once. The harness calls it when pm-server served a request through the
// edge, when the launch or the start of pm-server failed, when a step stops
// pm-server, and when the deployment closes.
func (d *deployment) releaseListener() {
	d.releaseOnce.Do(func() {
		if d.listener != nil {
			_ = d.listener.Close()
		}
	})
}

// close stops the edge, removes the client and the user of the deployment
// from Keycloak, checks the Keycloak container record and log for the
// secrets, and removes the deployment directory. It reports each failure.
// The steps stop the processes before they close the deployment.
func (d *deployment) close() error {
	d.releaseListener()
	if d.edge != nil {
		d.edge.Close()
	}
	var errs []error
	if d.identity != nil {
		errs = append(errs, d.stack.identity.admin.Remove(d.identity), d.stack.identity.checkRecords(d.dir, d.identity))
	}
	if d.dir != "" {
		errs = append(errs, os.RemoveAll(d.dir))
	}
	return errors.Join(errs...)
}

// writeSecrets makes the deployment operator secret root. It copies only the
// stack secret members that the two processes read, and it adds the client
// secret of the deployment, the CA of the job-owned Keycloak and a new
// wrapping key.
// pm-server opens the root with os.Root, so the members are copies, not links.
func (d *deployment) writeSecrets(stack stackInputs) (string, error) {
	root := filepath.Join(d.dir, "secrets")
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
	if err := writeSecretMember(root, identity, "oidc-client-secret", []byte(d.identity.ClientSecret)); err != nil {
		return "", err
	}
	if err := writeSecretMember(root, identity, "ca.pem", stack.identity.ca); err != nil {
		return "", err
	}
	encryption, err := secretBundlePath(laneEncryptionRef)
	if err != nil {
		return "", err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("generate the deployment wrapping key: %w", err)
	}
	if err := writeSecretMember(root, encryption, "wrapping-key", key); err != nil {
		return "", err
	}
	return root, nil
}

// writeConfiguration writes the deployment copy of the stack configuration.
// The copy keeps the stack PostgreSQL, Temporal, artifact, admission and
// limit settings. It changes the deployment identity, the public origin, the
// identity client, the wrapping key reference and the Temporal namespace,
// and adds the server and worker controls. The stack configuration file does
// not change.
func (d *deployment) writeConfiguration(stack stackInputs, backend string) (string, error) {
	document := maps.Clone(stack.document)
	temporal := map[string]json.RawMessage{}
	if err := json.Unmarshal(document["temporal"], &temporal); err != nil {
		return "", fmt.Errorf("decode the stack temporal configuration: %w", err)
	}
	namespace, err := json.Marshal(d.namespace)
	if err != nil {
		return "", fmt.Errorf("encode the deployment Temporal namespace: %w", err)
	}
	temporal["namespace"] = namespace
	values := map[string]any{
		"deployment_id": d.deploymentID,
		"public_origin": d.origin,
		"server": map[string]any{
			"listen":          backend,
			"trusted_proxies": []string{"127.0.0.1/32"},
			"drain_timeout":   "5s",
		},
		"identity": map[string]any{
			"issuer":            stack.identity.admin.Issuer(),
			"client_id":         d.identity.ClientID,
			"client_secret_ref": laneIdentityRef,
			"ca_ref":            laneIdentityRef,
		},
		"encryption": map[string]any{"wrapping_key_ref": laneEncryptionRef},
		"worker":     laneWorkerControls,
		"temporal":   temporal,
	}
	for key, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", fmt.Errorf("encode the deployment %s configuration: %w", key, err)
		}
		document[key] = encoded
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode the deployment configuration: %w", err)
	}
	path := filepath.Join(d.dir, "pm-deployment.json")
	if err := writePrivateFile(path, append(encoded, '\n')); err != nil {
		return "", err
	}
	return path, nil
}

// newLaneEdge starts the HTTPS edge of the lane. pm-server listens on plain
// HTTP behind a trusted edge, as it does in a deployment. The edge keeps the
// public Host and sets the X-Forwarded headers that pm-server checks.
func newLaneEdge(authority *laneAuthority, backend string, served func()) (*httptest.Server, error) {
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
		// served runs for each answer of pm-server, after pm-server served
		// the request. A failed request does not reach it.
		ModifyResponse: func(*http.Response) error {
			served()
			return nil
		},
		ErrorHandler: func(response http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(response, "lane backend unavailable", http.StatusBadGateway)
		},
	})
	edge.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	edge.Config.ReadHeaderTimeout = 5 * time.Second
	edge.StartTLS()
	return edge, nil
}

// holdLoopbackListener binds the loopback address of the pm-server listener
// with port 0 and keeps the listener (ADR 0008). No other socket can take the
// port between this bind and the start of pm-server, because pm-server gets a
// copy of this socket with --listen-fd. Go sets close-on-exec on the socket,
// so no other child process inherits it.
func holdLoopbackListener() (*net.TCPListener, error) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, fmt.Errorf("hold a loopback address for pm-server: %w", err)
	}
	return listener, nil
}

// Developer check of the held pm-server listener (ADR 0008), not acceptance
// proof. It needs no stack. Defect: the harness frees the port between its
// bind and the start of pm-server, so another socket can take it; a copy that
// the harness keeps has no close-on-exec; or a copy stays open after
// releaseListener.
func TestDeploymentHoldsThePmServerListenerUntilItsRelease(t *testing.T) {
	listener, err := holdLoopbackListener()
	if err != nil {
		t.Fatal(err)
	}
	d := &deployment{listener: listener, listen: listener.Addr().String()}
	t.Cleanup(d.releaseListener)
	if err := bindAddressOnce(d.listen); !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("a second bind of the held address = %v; want EADDRINUSE", err)
	}
	connection, err := listener.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var held bool
	if err := connection.Control(func(fd uintptr) { held = closeOnExec(t, fd) }); err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("the harness copy of the listener has no close-on-exec")
	}

	config := starter.Config{Args: []string{"--ready-fd", "3"}}
	if err := d.giveListener(&config); err != nil {
		t.Fatal(err)
	}
	if config.ListenFD != serverListenerFD || config.Listener == nil ||
		!slices.Equal(config.Args, []string{"--ready-fd", "3", "--listen-fd", "4"}) {
		t.Fatalf("giveListener gave %q and descriptor %d, want --listen-fd 4 and a copy of the socket", config.Args, config.ListenFD)
	}
	if !closeOnExec(t, config.Listener.Fd()) {
		t.Fatal("the copy for pm-server has no close-on-exec in the harness")
	}
	// starter.Start closes this copy. The check closes it in its place.
	if err := config.Listener.Close(); err != nil {
		t.Fatal(err)
	}

	d.releaseListener()
	d.releaseListener()
	if err := bindAddressOnce(d.listen); err != nil {
		t.Fatalf("a bind after the release = %v; want a free address, with no copy left", err)
	}
	if err := d.giveListener(&starter.Config{}); err == nil {
		t.Fatal("giveListener after the release = nil error, want a refusal")
	}
}

// bindAddressOnce binds address once and closes the new listener at once.
func bindAddressOnce(address string) error {
	listener, err := net.Listen("tcp4", address)
	if err == nil {
		_ = listener.Close()
	}
	return err
}

// closeOnExec reports whether descriptor fd has the close-on-exec flag.
func closeOnExec(t *testing.T, fd uintptr) bool {
	t.Helper()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFD, 0)
	if errno != 0 {
		t.Fatalf("read the descriptor flags: %v", errno)
	}
	return flags&syscall.FD_CLOEXEC != 0
}
