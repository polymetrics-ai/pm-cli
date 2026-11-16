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
	"fmt"
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

	"go.yaml.in/yaml/v3"
)

// The addresses below are synthetic test vectors: the IPv4 wildcard and
// loopback forms, and 192.0.2.10 from the RFC 5737 documentation range.

func TestIdentityProviderHostAdmitsOnlyASecondIPv4LoopbackAddress(t *testing.T) {
	for _, test := range []struct {
		host, refusal string
	}{
		{"0.0.0.0", "wildcard"},
		{"::", "wildcard"},
		{"192.0.2.10", "non-loopback"},
		{"127.0.0.1", "first loopback"},
		{"::1", "IPv4"},
		{"localhost", "IPv4"},
		{"127.0.0.02", "IPv4"},
		{"", "IPv4"},
	} {
		t.Run(test.host, func(t *testing.T) {
			err := validateIdentityProviderHost(test.host)
			if err == nil || !strings.Contains(err.Error(), test.refusal) {
				t.Fatalf("validateIdentityProviderHost(%q) = %v, want a %s refusal", test.host, err, test.refusal)
			}
		})
	}
	for _, host := range []string{"127.0.0.2", "127.45.6.7"} {
		if err := validateIdentityProviderHost(host); err != nil {
			t.Fatalf("validateIdentityProviderHost(%q) refused a second loopback address: %v", host, err)
		}
	}
}

func TestIdentityProviderEnvironmentIsAllOrNothingAndExact(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "ca.pem"), []byte("synthetic CA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ports := map[string]string{"PM_STACK_POSTGRES_PORT": "12001", "PM_STACK_PUBLIC_PORT": "12005"}
	valid := map[string]string{
		"PM_STACK_IDENTITY_PROVIDER_HOST":   "127.0.0.2",
		"PM_STACK_IDENTITY_PROVIDER_PORT":   "12006",
		"PM_STACK_IDENTITY_PROVIDER_ISSUER": "https://127.0.0.2:12006/realms/pm-test",
		"PM_STACK_IDENTITY_PROVIDER_DIR":    directory,
	}
	with := func(name, value string) func(string) string {
		return func(key string) string {
			if key == name {
				return value
			}
			return valid[key]
		}
	}

	provider, err := identityProviderFromEnvironment(func(string) string { return "" }, ports)
	if err != nil || provider != nil {
		t.Fatalf("no identity provider environment = (%v, %v), want none selected", provider, err)
	}
	provider, err = identityProviderFromEnvironment(with("", ""), ports)
	if err != nil || provider == nil {
		t.Fatalf("valid identity provider environment refused: %v", err)
	}
	if provider.host != valid["PM_STACK_IDENTITY_PROVIDER_HOST"] || provider.port != "12006" ||
		provider.issuer != valid["PM_STACK_IDENTITY_PROVIDER_ISSUER"] {
		t.Fatalf("identity provider = %+v, want the configured host, port and issuer", provider)
	}

	for _, test := range []struct{ name, value string }{
		{"PM_STACK_IDENTITY_PROVIDER_HOST", ""},
		{"PM_STACK_IDENTITY_PROVIDER_PORT", ""},
		{"PM_STACK_IDENTITY_PROVIDER_ISSUER", ""},
		{"PM_STACK_IDENTITY_PROVIDER_DIR", ""},
		{"PM_STACK_IDENTITY_PROVIDER_HOST", "0.0.0.0"},
		{"PM_STACK_IDENTITY_PROVIDER_HOST", "192.0.2.10"},
		{"PM_STACK_IDENTITY_PROVIDER_HOST", "127.0.0.1"},
		{"PM_STACK_IDENTITY_PROVIDER_PORT", "443"},
		{"PM_STACK_IDENTITY_PROVIDER_PORT", "012006"},
		{"PM_STACK_IDENTITY_PROVIDER_PORT", "12005"},
		{"PM_STACK_IDENTITY_PROVIDER_ISSUER", "http://127.0.0.2:12006/realms/pm-test"},
		{"PM_STACK_IDENTITY_PROVIDER_ISSUER", "https://127.0.0.3:12006/realms/pm-test"},
		{"PM_STACK_IDENTITY_PROVIDER_ISSUER", "https://127.0.0.2:12007/realms/pm-test"},
		{"PM_STACK_IDENTITY_PROVIDER_ISSUER", "https://localhost:12006/realms/pm-test"},
		{"PM_STACK_IDENTITY_PROVIDER_ISSUER", "https://127.0.0.2:12006/realms/"},
		{"PM_STACK_IDENTITY_PROVIDER_ISSUER", "https://127.0.0.2:12006/realms/pm-test/"},
		{"PM_STACK_IDENTITY_PROVIDER_ISSUER", "https://127.0.0.2:12006/realms/pm-test?x=1"},
		{"PM_STACK_IDENTITY_PROVIDER_ISSUER", "https://user@127.0.0.2:12006/realms/pm-test"},
		{"PM_STACK_IDENTITY_PROVIDER_DIR", "relative/identity-provider"},
		{"PM_STACK_IDENTITY_PROVIDER_DIR", filepath.Join(directory, "absent")},
	} {
		t.Run(test.name+"="+test.value, func(t *testing.T) {
			if provider, err := identityProviderFromEnvironment(with(test.name, test.value), ports); err == nil {
				t.Fatalf("unsafe identity provider environment was admitted: %+v", provider)
			}
		})
	}
}

func TestIdentityProviderUserIsNeverContainerRoot(t *testing.T) {
	if user, err := identityProviderUser(true, 2001, 2002); err != nil || user != "1000:0" {
		t.Fatalf("rootless identity provider user = (%q, %v), want the image's non-root user 1000:0", user, err)
	}
	if user, err := identityProviderUser(false, 2001, 2002); err != nil || user != "2001:2002" {
		t.Fatalf("rootful identity provider user = (%q, %v), want the job account 2001:2002", user, err)
	}
	if user, err := identityProviderUser(false, 0, 0); err == nil {
		t.Fatalf("rootful identity provider accepted the root account: %q", user)
	}
}

func TestIdentityProviderReadinessRequiresTheExactIssuerOverVerifiedTLS(t *testing.T) {
	var served string
	status := http.StatusOK
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/realms/pm-test/.well-known/openid-configuration" {
			http.NotFound(writer, request)
			return
		}
		writer.WriteHeader(status)
		_ = json.NewEncoder(writer).Encode(map[string]string{"issuer": served})
	}))
	defer server.Close()
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(address.Host)
	if err != nil {
		t.Fatal(err)
	}
	// identityProviderFromEnvironment resolves the directory the same way.
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(directory, "ca.pem"), certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	issuer := "https://" + net.JoinHostPort(host, port) + "/realms/pm-test"
	provider := &identityProvider{host: host, port: port, issuer: issuer, dir: directory}

	served = issuer
	if err := checkIdentityProvider(context.Background(), provider); err != nil {
		t.Fatalf("exact issuer was not ready: %v", err)
	}
	failedStep := func() string { // the code that pm-stack prints for a failed wait
		err := checkIdentityProvider(context.Background(), provider)
		return FailedStep(&stepError{"identity-provider-ready", fmt.Errorf("job identity provider readiness: %w", err)})
	}
	for _, test := range []struct {
		name   string
		issuer string
		status int
		step   string
	}{
		{"request-derived issuer", "https://localhost:" + port + "/realms/pm-test", http.StatusOK, "issuer-mismatch"},
		{"other realm", "https://" + net.JoinHostPort(host, port) + "/realms/master", http.StatusOK, "issuer-mismatch"},
		{"not ready", issuer, http.StatusServiceUnavailable, "status-503"},
	} {
		t.Run(test.name, func(t *testing.T) {
			served, status = test.issuer, test.status
			if step := failedStep(); step != "identity-provider-ready/"+test.step {
				t.Fatalf("identity provider readiness gave step %q for a wrong discovery answer", step)
			}
		})
	}
	served, status = issuer, http.StatusOK
	if err := os.WriteFile(filepath.Join(directory, "ca.pem"), otherCertificateAuthorityPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	if step := failedStep(); step != "identity-provider-ready/tls-certificate" {
		t.Fatalf("identity provider readiness gave step %q for a server outside the stack CA", step)
	}
	server.Close()
	if step := failedStep(); step != "identity-provider-ready/connection-refused" {
		t.Fatalf("identity provider readiness gave step %q for a closed port", step)
	}
	if FailedStep(&stepError{"render", errors.New("a path")}) != "render" || FailedStep(errors.New("no step")) != "" {
		t.Fatal("FailedStep did not give only the fixed step code")
	}
}

func TestComposeSourceAdmitsTheSecondLoopbackBindingOnlyForTheIdentityProvider(t *testing.T) {
	repository, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateComposeSource(repository); err != nil {
		t.Fatalf("repository Compose projection refused: %v", err)
	}
	var overlay composeDocument
	contents, err := os.ReadFile(filepath.Join(repository, identityProviderComposeFile))
	if err != nil {
		t.Fatalf("read the identity provider overlay: %v", err)
	}
	if err := yaml.Unmarshal(contents, &overlay); err != nil {
		t.Fatal(err)
	}
	if service, ok := overlay.Services[identityProviderService]; !ok || len(overlay.Services) != 1 ||
		len(service.Ports) != 1 || service.Ports[0] != identityProviderBinding {
		t.Fatalf("overlay services = %+v, want only %q with one second-loopback binding", overlay.Services, identityProviderService)
	}

	type mutation struct {
		name       string
		base       func(map[string]any)
		identity   func(map[string]any)
		wantRefuse string
	}
	keycloak := func(document map[string]any) map[string]any {
		return document["services"].(map[string]any)[identityProviderService].(map[string]any)
	}
	for _, test := range []mutation{
		{name: "valid", wantRefuse: ""},
		{name: "identity provider wildcard binding", identity: func(d map[string]any) {
			keycloak(d)["ports"] = []any{"0.0.0.0:${PM_STACK_IDENTITY_PROVIDER_PORT:?}:8443"}
		}, wantRefuse: "keycloak"},
		{name: "identity provider all-interface binding", identity: func(d map[string]any) {
			keycloak(d)["ports"] = []any{"${PM_STACK_IDENTITY_PROVIDER_PORT:?}:8443"}
		}, wantRefuse: "keycloak"},
		{name: "identity provider non-loopback binding", identity: func(d map[string]any) {
			keycloak(d)["ports"] = []any{"192.0.2.10:${PM_STACK_IDENTITY_PROVIDER_PORT:?}:8443"}
		}, wantRefuse: "keycloak"},
		{name: "identity provider first loopback binding", identity: func(d map[string]any) {
			keycloak(d)["ports"] = []any{"127.0.0.1:${PM_STACK_IDENTITY_PROVIDER_PORT:?}:8443"}
		}, wantRefuse: "keycloak"},
		{name: "identity provider second port", identity: func(d map[string]any) {
			keycloak(d)["ports"] = []any{identityProviderBinding, "127.0.0.1:${PM_STACK_PUBLIC_PORT:?}:8080"}
		}, wantRefuse: "keycloak"},
		{name: "other overlay service on the second address", identity: func(d map[string]any) {
			d["services"].(map[string]any)["caddy"] = map[string]any{"ports": []any{identityProviderBinding}}
		}, wantRefuse: "caddy"},
		{name: "bundled service on the second address", base: func(d map[string]any) {
			d["services"].(map[string]any)["postgres"].(map[string]any)["ports"] = []any{identityProviderBinding}
		}, wantRefuse: "postgres"},
		{name: "bundled service wildcard binding", base: func(d map[string]any) {
			d["services"].(map[string]any)["postgres"].(map[string]any)["ports"] = []any{"0.0.0.0:${PM_STACK_POSTGRES_PORT:?}:5432"}
		}, wantRefuse: "postgres"},
		{name: "identity provider in the bundled projection", base: func(d map[string]any) {
			d["services"].(map[string]any)[identityProviderService] = map[string]any{"ports": []any{"127.0.0.1:${PM_STACK_PUBLIC_PORT:?}:8443"}}
		}, wantRefuse: "keycloak"},
		{name: "writable root file system", identity: func(d map[string]any) { keycloak(d)["read_only"] = false }, wantRefuse: "keycloak"},
		{name: "retained capabilities", identity: func(d map[string]any) { delete(keycloak(d), "cap_drop") }, wantRefuse: "keycloak"},
		{name: "new privileges", identity: func(d map[string]any) { delete(keycloak(d), "security_opt") }, wantRefuse: "keycloak"},
		{name: "shared stack network", identity: func(d map[string]any) { keycloak(d)["networks"] = []any{"stack-private"} }, wantRefuse: "keycloak"},
		{name: "host network", identity: func(d map[string]any) { keycloak(d)["network_mode"] = "host" }, wantRefuse: "keycloak"},
		{name: "image user", identity: func(d map[string]any) { delete(keycloak(d), "user") }, wantRefuse: "keycloak"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := map[string]any{"services": map[string]any{"postgres": map[string]any{
				"ports": []any{"127.0.0.1:${PM_STACK_POSTGRES_PORT:?}:5432"},
			}}}
			identity := map[string]any{"services": map[string]any{identityProviderService: map[string]any{
				"user":         identityProviderUserVariable,
				"read_only":    true,
				"cap_drop":     []any{"ALL"},
				"security_opt": []any{"no-new-privileges:true"},
				"ports":        []any{identityProviderBinding},
				"networks":     []any{identityProviderNetwork},
			}}}
			if test.base != nil {
				test.base(base)
			}
			if test.identity != nil {
				test.identity(identity)
			}
			root := t.TempDir()
			writeComposeDocument(t, filepath.Join(root, "deploy/compose.yaml"), base)
			writeComposeDocument(t, filepath.Join(root, identityProviderComposeFile), identity)
			err := validateComposeSource(root)
			if test.wantRefuse == "" {
				if err != nil {
					t.Fatalf("valid projection refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantRefuse) {
				t.Fatalf("validateComposeSource = %v, want a refusal that names %q", err, test.wantRefuse)
			}
		})
	}
}

func writeComposeDocument(t *testing.T, path string, document map[string]any) {
	t.Helper()
	contents, err := yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
}

// otherCertificateAuthorityPEM returns a new self-signed CA that signs nothing
// in the tests. It stands for a CA outside the stack.
func otherCertificateAuthorityPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "other test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
