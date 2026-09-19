package runtime

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// minimumSQLTLSVersion is the single protocol floor for every PostgreSQL
// connector connection. TLS 1.2 or newer is required; nothing in this package
// can lower it.
const minimumSQLTLSVersion = tls.VersionTLS12

var ErrSQLTLSRequired = errors.New("strict PostgreSQL TLS verification is required")

// SQLTLSConfig is the exact caller- or configuration-supplied TLS request for
// one customer PostgreSQL endpoint. Every weakening field is recognized and
// refused rather than ignored: a request that tries to skip verification,
// accept plaintext, or lower the protocol floor never produces a usable
// configuration.
type SQLTLSConfig struct {
	// CACertificatePEM is the exact PEM trust-anchor bundle for this endpoint.
	CACertificatePEM []byte
	// ServerName is the explicit name the server certificate must verify
	// against. It is never inferred and never taken from the TLS peer.
	ServerName string
	// MinimumVersion requests a protocol floor. Zero selects TLS 1.2; any
	// lower value is refused.
	MinimumVersion uint16
	// InsecureSkipVerify and AllowPlaintext are refused whenever set.
	InsecureSkipVerify bool
	AllowPlaintext     bool
}

func (config SQLTLSConfig) validate() error {
	if config.InsecureSkipVerify {
		return fmt.Errorf("%w: certificate verification cannot be skipped", ErrSQLTLSRequired)
	}
	if config.AllowPlaintext {
		return fmt.Errorf("%w: plaintext PostgreSQL transport is not permitted", ErrSQLTLSRequired)
	}
	if len(config.CACertificatePEM) == 0 {
		return fmt.Errorf("%w: trust anchor material is absent", ErrSQLTLSRequired)
	}
	if err := validateSQLServerName(config.ServerName); err != nil {
		return err
	}
	if config.MinimumVersion != 0 && config.MinimumVersion < minimumSQLTLSVersion {
		return fmt.Errorf("%w: protocol floor %#x is below TLS 1.2", ErrSQLTLSRequired, config.MinimumVersion)
	}
	return nil
}

// NewStrictSQLTLS is the only PostgreSQL TLS constructor in this package. It
// parses the PEM trust-anchor bundle, requires an explicit server name, fixes
// the protocol floor at TLS 1.2 or newer, and installs a post-handshake check
// that refuses any session without a verified certificate chain.
func NewStrictSQLTLS(config SQLTLSConfig) (*tls.Config, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	anchors, err := strictSQLTrustAnchors(config.CACertificatePEM)
	if err != nil {
		return nil, err
	}
	minimum := config.MinimumVersion
	if minimum == 0 {
		minimum = minimumSQLTLSVersion
	}
	serverName := config.ServerName
	return &tls.Config{
		ServerName:         serverName,
		RootCAs:            anchors,
		MinVersion:         minimum,
		InsecureSkipVerify: false,
		Renegotiation:      tls.RenegotiateNever,
		VerifyConnection: func(state tls.ConnectionState) error {
			return verifyStrictSQLConnection(state, serverName, minimum)
		},
	}, nil
}

// strictSQLTrustAnchors decodes one PEM bundle and keeps only certificate
// authority certificates. A leaf certificate smuggled into the trust store, a
// non-certificate PEM block, or trailing garbage is refused.
func strictSQLTrustAnchors(material []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	remaining := material
	for {
		block, rest := pem.Decode(remaining)
		if block == nil {
			if len(bytes.TrimSpace(remaining)) != 0 {
				return nil, fmt.Errorf("%w: trust anchor material is not a PEM certificate bundle", ErrSQLTLSRequired)
			}
			break
		}
		remaining = rest
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("%w: trust anchor bundle contains a %q block", ErrSQLTLSRequired, block.Type)
		}
		certificates, err := x509.ParseCertificates(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: trust anchor certificate cannot be parsed", ErrSQLTLSRequired)
		}
		for _, certificate := range certificates {
			if !certificate.BasicConstraintsValid || !certificate.IsCA ||
				certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
				return nil, fmt.Errorf("%w: trust anchor must be a certificate authority certificate", ErrSQLTLSRequired)
			}
			pool.AddCert(certificate)
		}
	}
	if len(pool.Subjects()) == 0 {
		return nil, fmt.Errorf("%w: trust anchor bundle contains no certificate", ErrSQLTLSRequired)
	}
	return pool, nil
}

func verifyStrictSQLConnection(state tls.ConnectionState, serverName string, minimum uint16) error {
	if state.Version < minimum {
		return fmt.Errorf("%w: negotiated protocol %#x is below the required floor %#x", ErrSQLTLSRequired, state.Version, minimum)
	}
	if len(state.PeerCertificates) == 0 {
		return fmt.Errorf("%w: server presented no certificate", ErrSQLTLSRequired)
	}
	if len(state.VerifiedChains) == 0 || len(state.VerifiedChains[0]) == 0 {
		return fmt.Errorf("%w: server certificate was not verified against the declared trust anchors", ErrSQLTLSRequired)
	}
	if state.ServerName != "" && state.ServerName != serverName {
		return fmt.Errorf("%w: negotiated session name differs from the declared server name", ErrSQLTLSRequired)
	}
	if err := state.VerifiedChains[0][0].VerifyHostname(serverName); err != nil {
		return fmt.Errorf("%w: verified certificate does not cover the declared server name", ErrSQLTLSRequired)
	}
	return nil
}

func validateSQLServerName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: server name is absent", ErrSQLTLSRequired)
	}
	if len(name) > 253 || strings.TrimSpace(name) != name {
		return fmt.Errorf("%w: server name is not a bounded host name", ErrSQLTLSRequired)
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		if character <= 0x20 || character >= 0x7f {
			return fmt.Errorf("%w: server name contains an unusable character", ErrSQLTLSRequired)
		}
		switch character {
		case '/', '\\', ':', '@', ',', '?', '#', '[':
			return fmt.Errorf("%w: server name must be one host name without a port, path, or scheme", ErrSQLTLSRequired)
		}
	}
	return nil
}

// SQLTLSWitness records the negotiated protocol facts of one live PostgreSQL
// session so the effect evidence names the exact encrypted session rather than
// a configuration that merely intended encryption.
type SQLTLSWitness struct {
	ServerName            string    `json:"server_name"`
	ProtocolVersion       uint16    `json:"protocol_version"`
	CipherSuite           uint16    `json:"cipher_suite"`
	PeerCertificateSHA256 string    `json:"peer_certificate_sha256"`
	ObservedAt            time.Time `json:"observed_at"`
}

func (witness SQLTLSWitness) ProtocolName() string { return tls.VersionName(witness.ProtocolVersion) }

func (witness SQLTLSWitness) CipherSuiteName() string {
	return tls.CipherSuiteName(witness.CipherSuite)
}

func (witness SQLTLSWitness) Validate() error {
	if err := validateSQLServerName(witness.ServerName); err != nil {
		return err
	}
	if witness.ProtocolVersion < minimumSQLTLSVersion {
		return fmt.Errorf("%w: witnessed protocol %#x is below the required floor", ErrSQLTLSRequired, witness.ProtocolVersion)
	}
	if witness.CipherSuite == 0 {
		return fmt.Errorf("%w: witnessed cipher suite is absent", ErrSQLTLSRequired)
	}
	if len(witness.PeerCertificateSHA256) != sha256.Size*2 {
		return fmt.Errorf("%w: witnessed peer certificate digest is absent", ErrSQLTLSRequired)
	}
	if _, err := hex.DecodeString(witness.PeerCertificateSHA256); err != nil {
		return fmt.Errorf("%w: witnessed peer certificate digest is invalid", ErrSQLTLSRequired)
	}
	if witness.ObservedAt.IsZero() || witness.ObservedAt.Location() != time.UTC {
		return fmt.Errorf("%w: witness time must be an exact UTC instant", ErrSQLTLSRequired)
	}
	return nil
}

// ObserveStrictSQLTLS records the negotiated protocol facts of one live
// connection. A connection that is not a completed, strictly verified TLS
// session has no witness at all: plaintext or verify-less sessions cannot be
// passed off as encrypted evidence.
func ObserveStrictSQLTLS(conn net.Conn, serverName string, observedAt time.Time) (SQLTLSWitness, error) {
	if err := validateSQLServerName(serverName); err != nil {
		return SQLTLSWitness{}, err
	}
	if conn == nil {
		return SQLTLSWitness{}, fmt.Errorf("%w: connection is absent", ErrSQLTLSRequired)
	}
	tlsConnection, ok := conn.(*tls.Conn)
	if !ok {
		return SQLTLSWitness{}, fmt.Errorf("%w: connection is not a TLS session", ErrSQLTLSRequired)
	}
	state := tlsConnection.ConnectionState()
	if !state.HandshakeComplete {
		return SQLTLSWitness{}, fmt.Errorf("%w: TLS handshake did not complete", ErrSQLTLSRequired)
	}
	if err := verifyStrictSQLConnection(state, serverName, minimumSQLTLSVersion); err != nil {
		return SQLTLSWitness{}, err
	}
	digest := sha256.Sum256(state.PeerCertificates[0].Raw)
	witness := SQLTLSWitness{
		ServerName:            serverName,
		ProtocolVersion:       state.Version,
		CipherSuite:           state.CipherSuite,
		PeerCertificateSHA256: hex.EncodeToString(digest[:]),
		ObservedAt:            observedAt.UTC(),
	}
	if err := witness.Validate(); err != nil {
		return SQLTLSWitness{}, err
	}
	return witness, nil
}
