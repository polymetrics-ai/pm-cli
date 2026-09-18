package app

import (
	"crypto/tls"
	"crypto/x509"
	"errors"

	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/credentials"
)

// LoadProviderHTTPTLS returns the optional operator-owned provider egress trust
// root as the process-static TLS configuration the shared HTTP direct-read
// registration already accepts. An absent declaration returns nil without any
// secret access, so provider egress keeps the process system root store.
//
// A declared root is one bounded descriptor-confined ca.pem read. Only RootCAs
// comes from that pool: hostname pinning, chain verification, the required TLS
// origin policy, and the process transport stay exactly as they are, and no
// system or globally installed trust store is changed. A declared root that is
// missing, empty, oversized, or unparsable fails this call with a constant
// bounded error that never contains secret material. The caller owns the
// supplied operator secret root.
func LoadProviderHTTPTLS(
	secrets *credentials.OperatorSecrets,
	configuration config.ProviderHTTPConfiguration,
) (*tls.Config, error) {
	reference, declared := configuration.TLSTrustRootRef()
	if !declared {
		return nil, nil
	}
	if secrets == nil {
		return nil, errors.New("provider HTTP trust root authority is unavailable")
	}
	ca, err := secrets.Read(reference, "ca.pem")
	if err != nil {
		return nil, errors.New("provider HTTP trust root is unavailable")
	}
	roots := x509.NewCertPool()
	validRoot := roots.AppendCertsFromPEM(ca)
	clear(ca)
	if !validRoot {
		return nil, errors.New("provider HTTP trust root is invalid")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, nil
}
