package acceptance

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"time"
)

const laneOIDCClientID = "pm-acceptance-lane"

// laneAuthority is the lane's own certificate authority. It signs the TLS
// certificates of the lane identity provider and of the lane edge. pm-server
// trusts it for the identity provider through identity.ca_ref, and the pm
// process trusts it through SSL_CERT_FILE. No system trust store changes.
type laneAuthority struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	pem         []byte
}

func newLaneAuthority() (*laneAuthority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate lane CA key: %w", err)
	}
	serial, err := laneSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "PM acceptance lane CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create lane CA: %w", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse lane CA: %w", err)
	}
	return &laneAuthority{
		certificate: certificate,
		key:         key,
		pem:         pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}, nil
}

// serverCertificate issues a loopback TLS server certificate.
func (authority *laneAuthority) serverCertificate(name string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("generate %s TLS key: %w", name, err)
	}
	serial, err := laneSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, authority.certificate, &key.PublicKey, authority.key)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create %s TLS certificate: %w", name, err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// pool returns a certificate pool that holds only the lane CA.
func (authority *laneAuthority) pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(authority.certificate)
	return pool
}

func laneSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	return serial.Add(serial, big.NewInt(1)), nil
}

// laneIdentityProvider is the test OpenID Connect provider of the lane.
// pm-server runs OIDC discovery when it starts, so the lane needs a provider
// that it can reach. This provider serves the discovery document and the
// signing keys only. It has no login flow: part a of issue #72 runs no
// authenticated command. Its authorization and token endpoints refuse every
// request with 501.
type laneIdentityProvider struct {
	server       *httptest.Server
	issuer       string
	clientSecret string
	signingKey   *rsa.PrivateKey
}

func newLaneIdentityProvider(authority *laneAuthority) (*laneIdentityProvider, error) {
	certificate, err := authority.serverCertificate("PM acceptance lane identity provider")
	if err != nil {
		return nil, err
	}
	signingKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate lane OIDC signing key: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generate lane OIDC client secret: %w", err)
	}
	provider := &laneIdentityProvider{
		clientSecret: base64.RawURLEncoding.EncodeToString(secret),
		signingKey:   signingKey,
	}
	provider.server = httptest.NewUnstartedServer(http.HandlerFunc(provider.serveHTTP))
	provider.server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	provider.server.StartTLS()
	provider.issuer = provider.server.URL
	return provider, nil
}

func (provider *laneIdentityProvider) close() {
	if provider != nil && provider.server != nil {
		provider.server.Close()
	}
}

func (provider *laneIdentityProvider) serveHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	if request.Method != http.MethodGet && request.URL.Path != "/token" {
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	switch request.URL.Path {
	case "/.well-known/openid-configuration":
		writeLaneJSON(response, map[string]any{
			"issuer":                                provider.issuer,
			"authorization_endpoint":                provider.issuer + "/authorize",
			"token_endpoint":                        provider.issuer + "/token",
			"jwks_uri":                              provider.issuer + "/jwks",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"code_challenge_methods_supported":      []string{"S256"},
		})
	case "/jwks":
		public := provider.signingKey.PublicKey
		writeLaneJSON(response, map[string]any{"keys": []map[string]string{{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": "pm-acceptance-lane",
			"n":   base64.RawURLEncoding.EncodeToString(public.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(public.E)).Bytes()),
		}}})
	case "/authorize", "/token":
		http.Error(response, "the acceptance lane identity provider has no login flow", http.StatusNotImplemented)
	default:
		http.NotFound(response, request)
	}
}

func writeLaneJSON(response http.ResponseWriter, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		http.Error(response, "encode response", http.StatusInternalServerError)
		return
	}
	response.Header().Set("Content-Type", "application/json")
	_, _ = response.Write(encoded)
}
