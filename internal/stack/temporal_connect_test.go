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
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
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
//
// Only the accept goroutine writes the held connections. The cleanup closes
// the listener, which ends Accept, and waits until the goroutine returns.
// Then the cleanup closes the held connections. Nothing sends on a closed
// channel, and no connection stays open after the test.
func listenAndHold(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var held []net.Conn
	accepting := make(chan struct{})
	go func() {
		defer close(accepting)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			held = append(held, connection)
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-accepting
		for _, connection := range held {
			_ = connection.Close()
		}
	})
	return listener.Addr().String()
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
