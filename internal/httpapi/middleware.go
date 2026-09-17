package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/polymetrics-ai/pm-cli/internal/api"
)

type verifiedInboundKey struct{}

// verifiedInbound exists only after the configured transport boundary has
// checked forwarding provenance. Authentication must not trust raw proxy headers.
type verifiedInbound struct {
	TLS      bool
	Host     string
	Origin   string
	ClientIP string
}

// TransportPolicy contains the resolved controls used by this HTTP boundary.
// It does not resolve files, environment variables, or operator configuration.
type TransportPolicy struct {
	PublicOrigin     string
	TrustedProxies   []string
	RequestTimeout   time.Duration
	MaximumBodyBytes int64
}

// RequestBoundary precedes authentication and application I/O. It authenticates
// no caller: loopback and trusted ingress only establish transport provenance.
func RequestBoundary(configuration TransportPolicy, diagnostics io.Writer) (gin.HandlerFunc, error) {
	if diagnostics == nil || configuration.RequestTimeout <= 0 || configuration.MaximumBodyBytes <= 0 {
		return nil, errors.New("resolved bounded transport controls and diagnostics are required")
	}
	origin, err := url.Parse(configuration.PublicOrigin)
	if err != nil || origin.Scheme != "https" || origin.Hostname() == "" || origin.User != nil ||
		origin.RawQuery != "" || origin.Fragment != "" || origin.Opaque != "" ||
		(origin.EscapedPath() != "" && origin.EscapedPath() != "/") {
		return nil, errors.New("server origin is invalid")
	}
	trusted := make([]netip.Prefix, 0, len(configuration.TrustedProxies))
	for _, value := range configuration.TrustedProxies {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			address, addressErr := netip.ParseAddr(value)
			if addressErr != nil {
				return nil, errors.New("trusted proxy is invalid")
			}
			prefix = netip.PrefixFrom(address, address.BitLen())
		}
		if prefix.Bits() == 0 || prefix != prefix.Masked() {
			return nil, errors.New("trusted proxy must be a bounded canonical network")
		}
		for _, existing := range trusted {
			if prefix == existing {
				return nil, errors.New("trusted proxy is duplicated")
			}
		}
		trusted = append(trusted, prefix)
	}
	maximumBody := configuration.MaximumBodyBytes
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("X-Frame-Options", "DENY")
		defer func() {
			if recover() != nil {
				problem := api.NewInternalProblem(nil)
				_, _ = fmt.Fprintf(diagnostics, "pm-server: request_failed incident=%s\n", problem.IncidentId)
				if !c.Writer.Written() {
					writeProblem(c, problem)
				} else {
					c.Abort()
				}
			}
		}()
		request := c.Request
		if !sameOrigin("https://"+request.Host, origin) || !safeTransport(request, origin, trusted) {
			writeProblem(c, api.NewProtocolProblem(nil))
			return
		}
		query, err := url.ParseQuery(request.URL.RawQuery)
		if err != nil {
			writeProblem(c, api.NewProtocolProblem(nil))
			return
		}
		for name := range query {
			if api.IsForbiddenPublicField(name) || strings.EqualFold(name, "authorization") || strings.EqualFold(name, "token") || strings.EqualFold(name, "pm_session") {
				writeProblem(c, api.NewProtocolProblem(nil))
				return
			}
		}
		if request.Body != nil {
			request.Body = http.MaxBytesReader(c.Writer, request.Body, maximumBody)
		}
		ctx, cancel := context.WithTimeout(request.Context(), configuration.RequestTimeout)
		defer cancel()
		ctx = context.WithValue(ctx, verifiedInboundKey{}, verifiedInbound{
			TLS:  request.TLS != nil || request.Header.Get("X-Forwarded-Proto") == "https",
			Host: request.Host, Origin: request.Header.Get("Origin"), ClientIP: c.ClientIP(),
		})
		c.Request = request.WithContext(ctx)
		c.Next()
	}, nil
}

func safeTransport(request *http.Request, expected *url.URL, trusted []netip.Prefix) bool {
	for _, name := range []string{"Authorization", "Origin", "X-Forwarded-Proto", "X-Forwarded-Host", "X-Forwarded-For", "X-Real-IP"} {
		if len(request.Header.Values(name)) > 1 {
			return false
		}
	}
	if origins := request.Header.Values("Origin"); len(origins) == 1 && !sameOrigin(origins[0], expected) {
		return false
	}
	peerTrusted := false
	if remote, err := netip.ParseAddrPort(request.RemoteAddr); err == nil {
		for _, prefix := range trusted {
			if prefix.Contains(remote.Addr().Unmap()) {
				peerTrusted = true
				break
			}
		}
	}
	if request.Header.Get("Forwarded") != "" {
		// This deployment boundary admits the explicitly handled Caddy
		// X-Forwarded fields, not a second forwarding grammar.
		return false
	}
	for _, name := range []string{"X-Forwarded-Proto", "X-Forwarded-Host", "X-Forwarded-For", "X-Real-IP"} {
		if request.Header.Get(name) != "" && !peerTrusted {
			return false
		}
	}
	if host := request.Header.Get("X-Forwarded-Host"); host != "" && !sameOrigin("https://"+host, expected) {
		return false
	}
	if forwarded := request.Header.Get("X-Forwarded-For"); forwarded != "" {
		addresses := strings.Split(forwarded, ",")
		if len(addresses) > 16 {
			return false
		}
		for _, address := range addresses {
			parsed, err := netip.ParseAddr(strings.TrimSpace(address))
			if err != nil || parsed.Zone() != "" {
				return false
			}
		}
	}
	if address := request.Header.Get("X-Real-IP"); address != "" {
		parsed, err := netip.ParseAddr(address)
		if err != nil || parsed.Zone() != "" {
			return false
		}
	}
	secure := request.TLS != nil
	if protocol := request.Header.Get("X-Forwarded-Proto"); protocol != "" {
		if !peerTrusted || protocol != "https" {
			return false
		}
		secure = true
	}
	if secure {
		return true
	}
	// Non-secret compatibility and health may be inspected at a local
	// listener. Authentication material is never accepted on that transport.
	if request.Method != http.MethodGet || request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
		return false
	}
	switch request.URL.Path {
	case "/v1/info", "/health/live", "/health/ready":
		remote, err := netip.ParseAddrPort(request.RemoteAddr)
		return err == nil && remote.Addr().Unmap().IsLoopback()
	default:
		return false
	}
}

func sameOrigin(raw string, expected *url.URL) bool {
	value, err := url.Parse(raw)
	if err != nil || value.Scheme != "https" || value.Host == "" || value.User != nil || value.Path != "" || value.RawQuery != "" || value.Fragment != "" || value.Opaque != "" {
		return false
	}
	port := value.Port()
	if port == "" {
		port = "443"
	}
	expectedPort := expected.Port()
	if expectedPort == "" {
		expectedPort = "443"
	}
	return strings.EqualFold(value.Hostname(), expected.Hostname()) && port == expectedPort
}

func writeProblem(c *gin.Context, problem api.Problem) {
	c.Header("Content-Type", "application/problem+json")
	c.Header("Cache-Control", "no-store")
	c.AbortWithStatusJSON(problem.Status, problem)
}
