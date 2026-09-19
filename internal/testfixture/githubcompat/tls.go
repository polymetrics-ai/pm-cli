package githubcompat

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// The declared TLS directory holds exactly these four files. The operator
	// declares ca.pem as the provider egress trust root; the rest belong to this
	// destination.
	caCertificateName     = "ca.pem"
	caKeyName             = "ca-key.pem"
	serverCertificateName = "server.pem"
	serverKeyName         = "server-key.pem"

	// certificateMode, privateKeyMode and directoryMode are the modes of the
	// material this destination creates. A private key is never group or world
	// readable.
	certificateMode = 0o644
	privateKeyMode  = 0o600
	directoryMode   = 0o700

	// authorityValidity and serverValidity bound the generated material. A
	// reused server certificate is reissued from the same authority when it
	// expires, so the declared trust root survives restarts; an authority outside
	// its window is refused instead of silently replaced.
	authorityValidity = 30 * 24 * time.Hour
	serverValidity    = 7 * 24 * time.Hour
	validitySkew      = time.Hour

	// authorityCommonName names the generated fixture authority. It is not a
	// trust root of any system store and is never installed in one.
	authorityCommonName = "PMCLI controlled GitHub-compatible fixture CA"
)

// authority is the declared fixture authority: its certificate signing key and
// the certificate the operator declares as the egress trust root.
type authority struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
}

// certificateNames returns the ordered certificate names of one destination: the
// admitted origin host first, then the loopback names an operator uses for a
// direct smoke test, then the declared extra names in sorted order. A name that
// repeats another is kept once.
func certificateNames(originHost string, declared []string) []string {
	names := make([]string, 0, len(declared)+4)
	seen := make(map[string]struct{}, len(declared)+4)
	appendName := func(name string) {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return
		}
		key := strings.ToLower(trimmed)
		if address := net.ParseIP(trimmed); address != nil {
			key = address.String()
		}
		if _, duplicate := seen[key]; duplicate {
			return
		}
		seen[key] = struct{}{}
		names = append(names, trimmed)
	}
	appendName(originHost)
	for _, name := range []string{"localhost", "127.0.0.1", "::1"} {
		appendName(name)
	}
	extras := make([]string, len(declared))
	copy(extras, declared)
	sort.Strings(extras)
	for _, name := range extras {
		appendName(name)
	}
	return names
}

// loadTLSMaterial returns the declared trust authority and a server certificate
// that covers every requested name. Existing material is reused exactly: a valid
// declared authority is never replaced with a different one, so the trust root
// the operator declared stays valid across restarts, and a server certificate
// that already covers every requested name and chains to that authority is never
// reissued. Incomplete or unparsable authority material is refused instead of
// silently regenerated; the derived server certificate is always reissued from
// the same authority when it is missing, unreadable, expired or does not cover a
// requested name.
func loadTLSMaterial(directory string, names []string, now time.Time) ([]byte, []byte, error) {
	if err := prepareDirectory(directory); err != nil {
		return nil, nil, err
	}
	issuer, err := loadOrCreateAuthority(directory, now)
	if err != nil {
		return nil, nil, err
	}
	return loadOrIssueServerCertificate(directory, issuer, names, now)
}

// prepareDirectory creates the declared TLS directory when it is absent and
// refuses a declared path that is not a real directory.
func prepareDirectory(directory string) error {
	information, err := os.Stat(directory)
	switch {
	case err == nil:
		if !information.IsDir() {
			return fmt.Errorf("%w: declared tls directory is not a directory", ErrConfiguration)
		}
		return nil
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(directory, directoryMode); err != nil {
			return fmt.Errorf("create declared tls directory: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("inspect declared tls directory: %w", err)
	}
}

// loadOrCreateAuthority reuses the declared authority or generates one when the
// directory holds none. A half-declared or unparsable authority is refused.
func loadOrCreateAuthority(directory string, now time.Time) (authority, error) {
	certificatePEM, hasCertificate, err := readMaterial(filepath.Join(directory, caCertificateName))
	if err != nil {
		return authority{}, fmt.Errorf("read declared trust authority certificate: %w", err)
	}
	keyPEM, hasKey, err := readMaterial(filepath.Join(directory, caKeyName))
	if err != nil {
		return authority{}, fmt.Errorf("read declared trust authority key: %w", err)
	}
	if hasCertificate != hasKey {
		return authority{}, errors.New("declared trust authority is incomplete: ca.pem and ca-key.pem must exist together")
	}
	if !hasCertificate {
		return generateAuthority(directory, now)
	}
	return parseAuthority(certificatePEM, keyPEM, now)
}

// parseAuthority validates the declared authority: a certificate authority
// inside its validity window whose declared key matches its certificate.
func parseAuthority(certificatePEM, keyPEM []byte, now time.Time) (authority, error) {
	certificate, err := parseCertificate(certificatePEM)
	if err != nil {
		return authority{}, fmt.Errorf("declared trust authority certificate: %w", err)
	}
	if !certificate.BasicConstraintsValid || !certificate.IsCA {
		return authority{}, errors.New("declared trust authority certificate is not a certificate authority")
	}
	if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
		return authority{}, errors.New("declared trust authority certificate is outside its validity window; remove the declared tls directory to generate a new trust root")
	}
	key, err := parsePrivateKey(keyPEM)
	if err != nil {
		return authority{}, fmt.Errorf("declared trust authority key: %w", err)
	}
	if !publicKeysEqual(key.Public(), certificate.PublicKey) {
		return authority{}, errors.New("declared trust authority key does not match its certificate")
	}
	return authority{certificate: certificate, key: key}, nil
}

// generateAuthority creates one fixture authority and persists it, so every
// restart of the same declared directory serves the same trust root.
func generateAuthority(directory string, now time.Time) (authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return authority{}, fmt.Errorf("generate trust authority key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return authority{}, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: authorityCommonName},
		NotBefore:             now.Add(-validitySkew),
		NotAfter:              now.Add(authorityValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return authority{}, fmt.Errorf("create trust authority certificate: %w", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return authority{}, fmt.Errorf("parse created trust authority certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return authority{}, fmt.Errorf("encode trust authority key: %w", err)
	}
	generated := authority{certificate: certificate, key: key}
	if err := writeMaterial(filepath.Join(directory, caCertificateName), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), certificateMode); err != nil {
		return authority{}, err
	}
	if err := writeMaterial(filepath.Join(directory, caKeyName), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), privateKeyMode); err != nil {
		return authority{}, err
	}
	return generated, nil
}

// loadOrIssueServerCertificate reuses a server certificate that covers every
// requested name, chains to the declared authority and is inside its validity
// window, and otherwise issues one from that same authority.
func loadOrIssueServerCertificate(
	directory string,
	issuer authority,
	names []string,
	now time.Time,
) ([]byte, []byte, error) {
	certificatePEM, hasCertificate, err := readMaterial(filepath.Join(directory, serverCertificateName))
	if err != nil {
		return nil, nil, fmt.Errorf("read declared server certificate: %w", err)
	}
	keyPEM, hasKey, err := readMaterial(filepath.Join(directory, serverKeyName))
	if err != nil {
		return nil, nil, fmt.Errorf("read declared server key: %w", err)
	}
	if hasCertificate && hasKey {
		certificate, key, err := reuseServerCertificate(certificatePEM, keyPEM, issuer, names, now)
		if err == nil {
			return certificate, key, nil
		}
	}
	return issueServerCertificate(directory, issuer, names, now)
}

// reuseServerCertificate validates one declared server certificate before it is
// served again.
func reuseServerCertificate(
	certificatePEM, keyPEM []byte,
	issuer authority,
	names []string,
	now time.Time,
) ([]byte, []byte, error) {
	certificate, err := parseCertificate(certificatePEM)
	if err != nil {
		return nil, nil, err
	}
	if err := certificate.CheckSignatureFrom(issuer.certificate); err != nil {
		return nil, nil, errors.New("declared server certificate is not signed by the declared trust authority")
	}
	if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
		return nil, nil, errors.New("declared server certificate is outside its validity window")
	}
	for _, name := range names {
		if !certificateCovers(certificate, name) {
			return nil, nil, fmt.Errorf("declared server certificate does not cover %s", name)
		}
	}
	key, err := parsePrivateKey(keyPEM)
	if err != nil {
		return nil, nil, err
	}
	if !publicKeysEqual(key.Public(), certificate.PublicKey) {
		return nil, nil, errors.New("declared server key does not match its certificate")
	}
	return certificatePEM, keyPEM, nil
}

// issueServerCertificate issues and persists one server certificate for the
// declared names from the declared authority.
func issueServerCertificate(
	directory string,
	issuer authority,
	names []string,
	now time.Time,
) ([]byte, []byte, error) {
	if len(names) == 0 {
		return nil, nil, errors.New("a server certificate requires at least one declared name")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate server key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: names[0]},
		NotBefore:             now.Add(-validitySkew),
		NotAfter:              now.Add(serverValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, name := range names {
		if address := net.ParseIP(name); address != nil {
			template.IPAddresses = append(template.IPAddresses, address)
			continue
		}
		template.DNSNames = append(template.DNSNames, name)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, issuer.certificate, &key.PublicKey, issuer.key)
	if err != nil {
		return nil, nil, fmt.Errorf("create server certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("encode server key: %w", err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := writeMaterial(filepath.Join(directory, serverCertificateName), certificatePEM, certificateMode); err != nil {
		return nil, nil, err
	}
	if err := writeMaterial(filepath.Join(directory, serverKeyName), keyPEM, privateKeyMode); err != nil {
		return nil, nil, err
	}
	return certificatePEM, keyPEM, nil
}

// certificateCovers reports whether one certificate satisfies host name
// verification for one declared name.
func certificateCovers(certificate *x509.Certificate, name string) bool {
	if address := net.ParseIP(name); address != nil {
		for _, candidate := range certificate.IPAddresses {
			if candidate.Equal(address) {
				return true
			}
		}
		return false
	}
	return certificate.VerifyHostname(name) == nil
}

// maximumMaterialBytes bounds every fixture authority file the endpoint reads.
const maximumMaterialBytes = 1 << 20

// readMaterial returns one declared material file and reports whether it exists
// at all. A declared path that is not a bounded regular file is refused.
func readMaterial(path string) ([]byte, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer file.Close()
	information, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if !information.Mode().IsRegular() || information.Size() <= 0 || information.Size() > maximumMaterialBytes {
		return nil, false, errors.New("declared material is not a bounded regular file")
	}
	document, err := io.ReadAll(io.LimitReader(file, maximumMaterialBytes+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(document)) != information.Size() {
		return nil, false, errors.New("declared material changed while it was read")
	}
	return document, true, nil
}

// writeMaterial writes one generated material file with an explicit mode, so a
// private key is never created group or world readable.
func writeMaterial(path string, document []byte, mode os.FileMode) error {
	if err := os.WriteFile(path, document, mode); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("restrict %s: %w", filepath.Base(path), err)
	}
	return nil
}

// parseCertificate parses exactly one PEM certificate document.
func parseCertificate(document []byte) (*x509.Certificate, error) {
	block, rest := pem.Decode(document)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("material is not a PEM certificate")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("material carries trailing data after its certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, errors.New("material is not a parseable certificate")
	}
	return certificate, nil
}

// parsePrivateKey parses exactly one PEM PKCS#8 ECDSA private key.
func parsePrivateKey(document []byte) (*ecdsa.PrivateKey, error) {
	block, rest := pem.Decode(document)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("material is not a PEM private key")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("material carries trailing data after its private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("material is not a parseable PKCS#8 private key")
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("material private key is not an ECDSA key")
	}
	return key, nil
}

// publicKeysEqual compares two public keys through their PKIX encoding, so a
// reused key that does not match its certificate is refused instead of served.
func publicKeysEqual(left, right crypto.PublicKey) bool {
	leftDER, leftErr := x509.MarshalPKIXPublicKey(left)
	rightDER, rightErr := x509.MarshalPKIXPublicKey(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftDER, rightDER)
}

// randomSerial returns one positive 128-bit certificate serial number.
func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	for {
		serial, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return nil, fmt.Errorf("generate certificate serial: %w", err)
		}
		if serial.Sign() > 0 {
			return serial, nil
		}
	}
}

// tlsConfiguration returns the process TLS configuration of this destination:
// its own generated authority and server certificate, TLS 1.2 or newer, HTTP/1.1
// only. It never disables certificate chain or host name verification, never
// loads a system or global trust store, and changes none.
func tlsConfiguration(certificate tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{certificate},
		NextProtos:   []string{"http/1.1"},
	}
}
