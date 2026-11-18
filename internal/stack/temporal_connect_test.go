package stack

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/config"
	"go.temporal.io/api/serviceerror"
)

// TestTemporalConnectFailureKeepsItsCause checks that pm-stack check keeps
// the cause of a failed Temporal connection. A readiness failure must tell
// the operator whether the server refused the connection or the caller
// stopped the check. The message must not name the configured endpoint.
// The listeners are on the loopback interface. No Temporal server is needed.
func TestTemporalConnectFailureKeepsItsCause(t *testing.T) {
	rt := &runtime{temporalSecretDir: writeTemporalTestIdentity(t)}

	t.Run("a refused connection is unavailable", func(t *testing.T) {
		endpoint := listenAndClose(t)
		err := ensureTemporalNamespace(context.Background(), temporalTestConfiguration(t, endpoint), rt, false)
		var unavailable *serviceerror.Unavailable
		if !errors.As(err, &unavailable) {
			t.Fatalf("connect error does not keep the unavailable cause: %v", err)
		}
		if strings.Contains(err.Error(), endpoint) {
			t.Fatalf("connect error names the configured endpoint: %v", err)
		}
	})

	t.Run("a cancelled check is cancelled", func(t *testing.T) {
		endpoint := listenAndHold(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := ensureTemporalNamespace(ctx, temporalTestConfiguration(t, endpoint), rt, false)
		var canceled *serviceerror.Canceled
		if !errors.As(err, &canceled) {
			t.Fatalf("connect error does not keep the cancelled cause: %v", err)
		}
	})
}

// TestHeldConnectionCleanupWaitsForAnAcceptedConnection stops the
// held-connection helper while its accept goroutine has a just-accepted
// connection that it does not keep yet. The cleanup must wait for that
// goroutine, and then close the connection. Channels force the order. The
// check has no sleep and no retry, and each wait has one time limit. Every
// exit of the check releases and stops the helper (startHeldConnectionCheck).
func TestHeldConnectionCleanupWaitsForAnAcceptedConnection(t *testing.T) {
	check := startHeldConnectionCheck(t)
	defer check.client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), heldConnectionLimit)
	defer cancel()

	check.stop()
	// The cleanup closed the listener and waits for the accept goroutine, or
	// it returned without that wait. Either way, the accept goroutine then
	// continues with its connection.
	select {
	case <-check.joining:
	case <-check.stopped:
	case <-ctx.Done():
		t.Fatal("the cleanup neither waited for the accept goroutine nor returned")
	}
	check.release()
	select {
	case <-check.stopped:
	case <-ctx.Done():
		t.Fatal("the cleanup did not return after the accept goroutine continued")
	}

	// The cleanup closed the accepted connection, so the client reads the end
	// of the stream.
	assertEndOfStream(t, check.client)
}

// TestHeldConnectionCheckCleansUpAfterAnEarlyExit ends one run of the
// held-connection check while the accept goroutine holds the accepted
// connection in its hook, as a timeout before the release does. The cleanup
// of that run must release the hook and stop the helper. The client then
// reads the end of the stream. The check has no sleep and no retry, and the
// read has one time limit.
func TestHeldConnectionCheckCleansUpAfterAnEarlyExit(t *testing.T) {
	var check *heldConnectionCheck
	t.Run("an exit while the helper holds the connection", func(t *testing.T) {
		check = startHeldConnectionCheck(t)
	})
	if check == nil {
		t.Fatal("the run ended before the helper held the connection")
	}
	defer check.client.Close()
	assertEndOfStream(t, check.client)
}

// heldConnectionCheck is one run of holdConnections with one connection
// that the accept goroutine holds in its accepted hook.
type heldConnectionCheck struct {
	client  net.Conn
	joining chan struct{}
	stopped chan struct{}
	// release lets the accept goroutine keep the held connection. stop starts
	// the stop function of the helper in its own goroutine and closes stopped
	// when it returns. Each one acts once, also when it is called again.
	release func()
	stop    func()
}

// heldConnectionLimit is the one time limit of each wait in the
// held-connection checks.
const heldConnectionLimit = 10 * time.Second

// startHeldConnectionCheck starts holdConnections on a loopback listener,
// dials it once and waits until the accept goroutine holds the connection in
// its accepted hook. Before the dial, it registers one cleanup for every exit
// of the test, also a failure or a timeout: the cleanup starts the stop,
// releases the hook and waits once for the stop, with one time limit. The
// stop closes the listener, joins the accept goroutine and closes the held
// connection. The caller closes the client.
func startHeldConnectionCheck(t *testing.T) *heldConnectionCheck {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	check := &heldConnectionCheck{joining: make(chan struct{}), stopped: make(chan struct{})}
	stop := holdConnections(listener, heldConnectionHooks{
		accepted: func() {
			close(entered)
			<-release
		},
		joining: func() { close(check.joining) },
	})
	var releaseOnce, stopOnce sync.Once
	check.release = func() { releaseOnce.Do(func() { close(release) }) }
	check.stop = func() {
		stopOnce.Do(func() {
			go func() {
				defer close(check.stopped)
				stop()
			}()
		})
	}
	t.Cleanup(func() {
		check.stop()
		check.release()
		limit := time.NewTimer(heldConnectionLimit)
		defer limit.Stop()
		select {
		case <-check.stopped:
		case <-limit.C:
			t.Errorf("the held-connection helper did not stop within %s", heldConnectionLimit)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), heldConnectionLimit)
	defer cancel()
	var dialer net.Dialer
	client, err := dialer.DialContext(ctx, "tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	check.client = client
	select {
	case <-entered:
	case <-ctx.Done():
		_ = client.Close()
		t.Fatal("the helper did not accept the connection")
	}
	return check
}

// assertEndOfStream reads once from client, with one time limit, and requires
// the end of the stream.
func assertEndOfStream(t *testing.T, client net.Conn) {
	t.Helper()
	if err := client.SetReadDeadline(time.Now().Add(heldConnectionLimit)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("read from the held connection = %v; want end of stream", err)
	}
}

// listenAndClose accepts each connection and closes it at once, as a port
// forwarder does when no Temporal frontend is behind it.
func listenAndClose(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			_ = connection.Close()
		}
	}()
	return listener.Addr().String()
}

// listenAndHold accepts each connection and never answers.
func listenAndHold(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(holdConnections(listener, heldConnectionHooks{}))
	return listener.Addr().String()
}

// heldConnectionHooks let a developer check stop holdConnections at its two
// ordering points. A nil hook does nothing.
type heldConnectionHooks struct {
	// accepted runs in the accept goroutine after Accept returns a
	// connection, before the goroutine keeps it.
	accepted func()
	// joining runs in stop after stop closed the listener, before stop waits
	// for the accept goroutine.
	joining func()
}

// holdConnections accepts each connection on listener and never answers. It
// returns the stop function of the helper.
//
// Only the accept goroutine writes the held connections. stop closes the
// listener, which ends Accept, and waits until the goroutine returns. Then
// stop closes the held connections. Nothing sends on a closed channel, and no
// connection stays open after stop.
func holdConnections(listener net.Listener, hooks heldConnectionHooks) (stop func()) {
	var held []net.Conn
	accepting := make(chan struct{})
	go func() {
		defer close(accepting)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			if hooks.accepted != nil {
				hooks.accepted()
			}
			held = append(held, connection)
		}
	}()
	return func() {
		_ = listener.Close()
		if hooks.joining != nil {
			hooks.joining()
		}
		<-accepting
		for _, connection := range held {
			_ = connection.Close()
		}
	}
}

// temporalTestConfiguration returns a valid server configuration whose
// Temporal endpoint is the given loopback listener. The other values follow
// deploy/server.example.json.
func temporalTestConfiguration(t *testing.T, endpoint string) config.Configuration {
	t.Helper()
	encodedEndpoint, err := json.Marshal(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	document := strings.Replace(temporalTestConfigurationDocument, `"{{TEMPORAL_ENDPOINT}}"`, string(encodedEndpoint), 1)
	loaded, err := config.Load(strings.NewReader(document))
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := config.Validate(loaded, config.Prelaunch{})
	if err != nil {
		t.Fatal(err)
	}
	return configuration
}

const temporalTestConfigurationDocument = `{
  "schema_version": "1",
  "deployment_id": "pm-development",
  "public_origin": "https://pm.example.invalid",
  "postgres": {
    "ownership": "bundled",
    "database": "pm_app",
    "endpoint_ref": "secret://operator/pm-development-postgres-dsn"
  },
  "temporal": {
    "ownership": "bundled",
    "endpoint": "{{TEMPORAL_ENDPOINT}}",
    "namespace": "pm-development",
    "client_identity_ref": "secret://operator/pm-development-temporal-mtls",
    "retention": "720h",
    "task_queues": ["pm-control", "pm-read", "pm-effect", "pm-binary-query"]
  },
  "artifacts": {
    "ownership": "bundled",
    "driver": "s3",
    "endpoint": "https://objects.internal.invalid",
    "bucket": "pm-development-artifacts",
    "credential_ref": "secret://operator/pm-development-object-access",
    "attempt_key_policy": "unique_immutable"
  },
  "identity": {
    "issuer": "https://identity.example.invalid",
    "client_id": "pm-server",
    "client_secret_ref": "secret://operator/pm-development-oidc"
  },
  "encryption": {
    "wrapping_key_ref": "secret://operator/pm-development-wrap"
  },
  "admission": {
    "profile": "development"
  },
  "limits": {
    "workspace_active_runs": 4,
    "workflow_payload_bytes": 65536,
    "workflow_fanout": 32,
    "run_lifetime": "24h",
    "event_buffer": 128,
    "query_children": 1,
    "query_child_memory_bytes": 4294967296,
    "query_child_deadline": "5m",
    "request_bytes": 1048576,
    "metadata_deadline": "5s"
  }
}`

// writeTemporalTestIdentity writes a CA and a client identity for the
// loopback listeners. The listeners never complete a handshake, so no server
// certificate is needed.
func writeTemporalTestIdentity(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "pm-stack test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "pm-stack test client"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caTemplate, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	clientKeyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	for name, block := range map[string]*pem.Block{
		"ca.pem":         {Type: "CERTIFICATE", Bytes: caDER},
		"client.pem":     {Type: "CERTIFICATE", Bytes: clientDER},
		"client-key.pem": {Type: "EC PRIVATE KEY", Bytes: clientKeyDER},
	} {
		if err := os.WriteFile(filepath.Join(directory, name), pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}
