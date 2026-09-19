package app

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/credentials"
)

// providerHTTPTrustDocument is one complete valid document whose provider HTTP
// block optionally declares the operator-owned egress trust root. The %s slot
// takes the raw optional key/value pair.
const providerHTTPTrustDocument = `{
  "schema_version":"1", "deployment_id":"provider-http-trust-fixture", "public_origin":"https://pm.example.invalid",
  "postgres":{"ownership":"managed","database":"pm_app","endpoint_ref":"secret://fixture/postgres"},
  "temporal":{"ownership":"managed","endpoint":"temporal.example.invalid:7233","namespace":"fixture","client_identity_ref":"secret://fixture/temporal","retention":"720h","task_queues":["pm-control"]},
  "artifacts":{"ownership":"managed","driver":"s3","endpoint":"https://objects.example.invalid","bucket":"fixture-objects","credential_ref":"secret://fixture/objects","attempt_key_policy":"unique_immutable"},
  "identity":{"issuer":"https://identity.example.invalid","client_id":"pm-server","client_secret_ref":"secret://fixture/oidc"},
  "encryption":{"wrapping_key_ref":"secret://fixture/wrapping"},
  "admission":{"profile":"development"},
  "limits":{"workspace_active_runs":4,"workflow_payload_bytes":65536,"workflow_fanout":32,"run_lifetime":"1h","event_buffer":128,"query_children":1,"query_child_memory_bytes":4294967296,"query_child_deadline":"5m","request_bytes":1048576,"metadata_deadline":"5s"},
  "provider_http":{"max_request_bytes":1048576,"max_response_bytes":8388608,"max_dns_answers":8,"max_attempts":3,"max_retry_delay":"45s"%s,"destinations":[{"org_id":"org-fixture","workspace_id":"workspace-fixture","connection_id":"github-main","origin":"https://provider.test"}]}
}`

const (
	providerHTTPTrustMember           = "ca.pem"
	providerHTTPTrustBundle           = "fixture/provider-http"
	providerHTTPTrustDeclaration      = `,"tls_trust_root_ref":"secret://fixture/provider-http"`
	providerHTTPTrustResponseBody     = "provider-fixture-body"
	providerHTTPTrustServerName       = "provider.test"
	providerHTTPTrustForeignServerDNS = "other.provider.test"
)

func TestProviderHTTPTrustRootVerification(t *testing.T) {
	secretsDirectory := t.TempDir()
	authority := newProviderHTTPTrustAuthority(t, providerHTTPTrustServerName)
	writeProviderHTTPTrustRoot(t, secretsDirectory, authority.caPEM)
	peer := startProviderHTTPTrustPeer(t, authority.certificate)
	origin := "https://" + providerHTTPTrustServerName + ":" + providerHTTPTrustPeerPort(t, peer)

	secrets := openProviderHTTPTrustSecrets(t, secretsDirectory)
	declared := loadProviderHTTPTrustConfiguration(t, providerHTTPTrustDeclaration)
	loaded, err := LoadProviderHTTPTLS(secrets, declared)
	if err != nil {
		t.Fatalf("declared provider HTTP trust root was refused: %v", err)
	}
	if loaded == nil {
		t.Fatal("declared provider HTTP trust root produced no TLS configuration")
	}
	if loaded.InsecureSkipVerify || loaded.ServerName != "" || loaded.ClientCAs != nil ||
		len(loaded.Certificates) != 0 || loaded.RootCAs == nil {
		t.Fatalf("declared provider HTTP trust root changed verification or pinning: %#v", loaded)
	}

	t.Run("declared root verifies the controlled provider endpoint", func(t *testing.T) {
		status, body, err := providerHTTPTrustRequest(t, loaded, origin, peer)
		if err != nil || status != http.StatusOK || body != providerHTTPTrustResponseBody {
			t.Fatalf("provider response = (%d, %q, %v); want (%d, %q, nil)",
				status, body, err, http.StatusOK, providerHTTPTrustResponseBody)
		}
	})

	t.Run("a different hostname is refused by name verification", func(t *testing.T) {
		foreignHost := "https://" + providerHTTPTrustForeignServerDNS + ":" + providerHTTPTrustPeerPort(t, peer)
		_, _, err := providerHTTPTrustRequest(t, loaded, foreignHost, peer)
		var hostnameError x509.HostnameError
		if !errors.As(err, &hostnameError) {
			t.Fatalf("foreign provider hostname error = %v; want x509.HostnameError", err)
		}
	})

	t.Run("a certificate outside the declared root is refused by chain verification", func(t *testing.T) {
		foreign := newProviderHTTPTrustAuthority(t, providerHTTPTrustServerName)
		foreignPeer := startProviderHTTPTrustPeer(t, foreign.certificate)
		foreignOrigin := "https://" + providerHTTPTrustServerName + ":" + providerHTTPTrustPeerPort(t, foreignPeer)
		_, _, err := providerHTTPTrustRequest(t, loaded, foreignOrigin, foreignPeer)
		var authorityError x509.UnknownAuthorityError
		if !errors.As(err, &authorityError) {
			t.Fatalf("untrusted provider certificate error = %v; want x509.UnknownAuthorityError", err)
		}
	})

	t.Run("absence keeps the process system root store", func(t *testing.T) {
		absent := loadProviderHTTPTrustConfiguration(t, "")
		unconfigured, err := LoadProviderHTTPTLS(secrets, absent)
		if err != nil || unconfigured != nil {
			t.Fatalf("absent provider HTTP trust root = (%v, %v); want nil, nil", unconfigured, err)
		}
		if withoutSecrets, noSecretErr := LoadProviderHTTPTLS(nil, absent); noSecretErr != nil || withoutSecrets != nil {
			t.Fatalf("absent provider HTTP trust root required secret authority: (%v, %v)", withoutSecrets, noSecretErr)
		}
		// The generated authority exists only in the declared bundle, so the
		// unchanged default path still refuses it through the system roots.
		_, _, err = providerHTTPTrustRequest(t, unconfigured, origin, peer)
		var verificationError *tls.CertificateVerificationError
		if !errors.As(err, &verificationError) {
			t.Fatalf("system root store accepted the declared-only authority: %v", err)
		}
	})
}

func TestProviderHTTPTrustRootFailsClosed(t *testing.T) {
	const sentinel = "provider-trust-root-secret-sentinel"
	declared := loadProviderHTTPTrustConfiguration(t, providerHTTPTrustDeclaration)
	armored := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte(sentinel)})

	for _, test := range []struct {
		name    string
		content []byte
	}{
		{name: "missing declared root"},
		{name: "empty declared root", content: []byte{}},
		{name: "oversized declared root", content: bytes.Repeat([]byte(sentinel+"\n"), 6000)},
		{name: "unparsable declared root", content: []byte("-----BEGIN CERTIFICATE-----\n" + sentinel + "\n-----END CERTIFICATE-----\n")},
		{name: "non-certificate declared root", content: armored},
	} {
		t.Run(test.name, func(t *testing.T) {
			secretsDirectory := t.TempDir()
			memberDirectory := filepath.Join(secretsDirectory, filepath.FromSlash(providerHTTPTrustBundle))
			if err := os.MkdirAll(memberDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			member := filepath.Join(memberDirectory, providerHTTPTrustMember)
			if test.content != nil {
				if err := os.WriteFile(member, test.content, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			secrets := openProviderHTTPTrustSecrets(t, secretsDirectory)
			loaded, err := LoadProviderHTTPTLS(secrets, declared)
			if err == nil || loaded != nil {
				t.Fatalf("declared provider HTTP trust root = (%v, %v); want a refusal", loaded, err)
			}
			for _, forbidden := range []string{sentinel, secretsDirectory, memberDirectory, member} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("provider HTTP trust root refusal exposed %q: %v", forbidden, err)
				}
			}
		})
	}

	t.Run("declared root without operator secret authority", func(t *testing.T) {
		loaded, err := LoadProviderHTTPTLS(nil, declared)
		if err == nil || loaded != nil {
			t.Fatalf("declared provider HTTP trust root without secret authority = (%v, %v); want a refusal", loaded, err)
		}
	})
}

// providerHTTPTrustAuthority is one generated certificate authority with the
// provider egress server certificate it signed. No network or public authority
// is involved.
type providerHTTPTrustAuthority struct {
	caPEM       []byte
	certificate tls.Certificate
}

func newProviderHTTPTrustAuthority(t *testing.T, serverName string) providerHTTPTrustAuthority {
	t.Helper()
	now := time.Now().UTC()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatalf("generate provider trust authority key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "provider trust test authority"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(cryptorand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("issue provider trust authority: %v", err)
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatalf("generate provider server key: %v", err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: serverName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{serverName},
	}
	serverDER, err := x509.CreateCertificate(cryptorand.Reader, serverTemplate, caTemplate, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("issue provider server certificate: %v", err)
	}
	return providerHTTPTrustAuthority{
		caPEM:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		certificate: tls.Certificate{Certificate: [][]byte{serverDER}, PrivateKey: serverKey},
	}
}

// startProviderHTTPTrustPeer serves one HTTP/1.1 provider endpoint on loopback
// with the generated server certificate.
func startProviderHTTPTrustPeer(t *testing.T, certificate tls.Certificate) *httptest.Server {
	t.Helper()
	peer := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(providerHTTPTrustResponseBody))
	}))
	peer.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{certificate},
		NextProtos:   []string{"http/1.1"},
	}
	peer.StartTLS()
	t.Cleanup(peer.Close)
	return peer
}

// providerHTTPTrustRequest performs one provider request with the same derived
// client settings the shared HTTP runtime builds from these dependencies: the
// supplied trust pool is cloned, the selected origin pins the TLS server name,
// and HTTP/1.1 is the only offered protocol. The pinned dialer reaches the
// controlled loopback peer, never the public network.
func providerHTTPTrustRequest(
	t *testing.T,
	loaded *tls.Config,
	origin string,
	peer *httptest.Server,
) (int, string, error) {
	t.Helper()
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Hostname() == "" {
		t.Fatalf("parse provider origin %q: %v", origin, err)
	}
	tlsConfig := &tls.Config{}
	if loaded != nil {
		tlsConfig = loaded.Clone()
	}
	tlsConfig.ServerName = parsed.Hostname()
	tlsConfig.NextProtos = []string{"http/1.1"}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   tlsConfig,
			ForceAttemptHTTP2: false,
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, peer.Listener.Addr().String())
			},
		},
	}
	response, err := client.Get(origin + "/v1/fixture")
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 64))
	if readErr != nil {
		return response.StatusCode, "", readErr
	}
	return response.StatusCode, string(body), nil
}

func providerHTTPTrustPeerPort(t *testing.T, peer *httptest.Server) string {
	t.Helper()
	parsed, err := url.Parse(peer.URL)
	if err != nil || parsed.Port() == "" {
		t.Fatalf("parse controlled provider peer address %q: %v", peer.URL, err)
	}
	return parsed.Port()
}

func writeProviderHTTPTrustRoot(t *testing.T, secretsDirectory string, caPEM []byte) {
	t.Helper()
	directory := filepath.Join(secretsDirectory, filepath.FromSlash(providerHTTPTrustBundle))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("create provider trust bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, providerHTTPTrustMember), caPEM, 0o600); err != nil {
		t.Fatalf("write provider trust root: %v", err)
	}
}

func openProviderHTTPTrustSecrets(t *testing.T, directory string) *credentials.OperatorSecrets {
	t.Helper()
	secrets, err := credentials.OpenOperatorSecrets(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := secrets.Close(); err != nil {
			t.Errorf("close provider trust secrets: %v", err)
		}
	})
	return secrets
}

func loadProviderHTTPTrustConfiguration(t *testing.T, declaration string) config.ProviderHTTPConfiguration {
	t.Helper()
	document, err := config.Load(strings.NewReader(fmt.Sprintf(providerHTTPTrustDocument, declaration)))
	if err != nil {
		t.Fatalf("load provider HTTP trust document: %v", err)
	}
	configuration, err := config.Validate(document, config.Prelaunch{})
	if err != nil {
		t.Fatalf("validate provider HTTP trust document: %v", err)
	}
	return configuration.ProviderHTTP()
}
