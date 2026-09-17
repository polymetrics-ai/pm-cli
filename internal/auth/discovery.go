package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type DiscoveryConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	LoginTTL     time.Duration
}

func (DiscoveryConfig) String() string                 { return "configured OIDC discovery [REDACTED]" }
func (configuration DiscoveryConfig) GoString() string { return configuration.String() }

type providerMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	SigningAlgorithms     []string `json:"id_token_signing_alg_values_supported"`
}

// DiscoverOIDC binds endpoints and signing keys to the configured issuer. The
// caller owns the bounded, redirect-refusing HTTPS client and its transport.
func DiscoverOIDC(ctx context.Context, configuration DiscoveryConfig, client *http.Client) (OIDCConfig, error) {
	if ctx == nil || client == nil || !validDiscoveryURL(configuration.Issuer) {
		return OIDCConfig{}, errors.New("configured HTTPS OIDC discovery is required")
	}
	discoveryContext, cancel := context.WithTimeout(oidc.ClientContext(ctx, client), 15*time.Second)
	defer cancel()
	provider, err := oidc.NewProvider(discoveryContext, configuration.Issuer)
	if err != nil {
		return OIDCConfig{}, err
	}
	var metadata providerMetadata
	if err := provider.Claims(&metadata); err != nil || metadata.Issuer != configuration.Issuer {
		return OIDCConfig{}, errors.New("OIDC discovery metadata is invalid")
	}
	if !validDiscoveryURL(metadata.AuthorizationEndpoint) || !validDiscoveryURL(metadata.TokenEndpoint) || !validDiscoveryURL(metadata.JWKSURI) {
		return OIDCConfig{}, errors.New("OIDC endpoints must use HTTPS")
	}
	endpoint := provider.Endpoint()
	if endpoint.AuthURL != metadata.AuthorizationEndpoint || endpoint.TokenURL != metadata.TokenEndpoint {
		return OIDCConfig{}, errors.New("OIDC endpoint metadata is inconsistent")
	}
	endpoint.AuthStyle = oauth2.AuthStyleInHeader
	if len(metadata.SigningAlgorithms) > 0 && !slices.Contains(metadata.SigningAlgorithms, "RS256") {
		return OIDCConfig{}, errors.New("OIDC provider does not advertise the supported signing algorithm")
	}
	keyContext := oidc.ClientContext(context.Background(), client)
	return OIDCConfig{
		Issuer: configuration.Issuer,
		OAuth2: oauth2.Config{
			ClientID:     configuration.ClientID,
			ClientSecret: configuration.ClientSecret,
			Endpoint:     endpoint,
			RedirectURL:  configuration.RedirectURL,
			Scopes:       []string{oidc.ScopeOpenID},
		},
		KeySet:   oidc.NewRemoteKeySet(keyContext, metadata.JWKSURI),
		LoginTTL: configuration.LoginTTL,
	}, nil
}

func validDiscoveryURL(raw string) bool {
	if len(raw) < len("https://a") || len(raw) > 2048 {
		return false
	}
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" &&
		parsed.User == nil && parsed.Fragment == "" && parsed.Opaque == ""
}
