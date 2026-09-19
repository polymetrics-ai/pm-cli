package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
)

var ErrDestinationDenied = errors.New("HTTP destination denied")

type DNSResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type ContextDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

// EgressPolicy is server-owned policy selected for one connection. A selected
// origin is not itself source evidence; it must also occur in Origins above.
type EgressPolicy struct {
	SelectedOrigin string
	RequireTLS     bool
}

// PrivateDatabaseAllowance is intentionally not part of EgressPolicy. It is
// accepted only by ValidatePrivateDatabaseDestination and cannot widen HTTP.
type PrivateDatabaseAllowance struct {
	Prefixes []netip.Prefix
}

func validateDestination(sourceOrigins []string, policy EgressPolicy) (*url.URL, error) {
	selected, err := canonicalHTTPBase(policy.SelectedOrigin)
	if err != nil {
		return nil, fmt.Errorf("%w: selected origin is invalid", ErrDestinationDenied)
	}
	if policy.RequireTLS && selected.Scheme != "https" {
		return nil, fmt.Errorf("%w: server policy requires TLS", ErrDestinationDenied)
	}
	var matched *url.URL
	for _, candidate := range sourceOrigins {
		base, candidateErr := canonicalHTTPBase(candidate)
		if candidateErr != nil {
			return nil, fmt.Errorf("%w: source origin is invalid", ErrDestinationDenied)
		}
		if sameHTTPBase(base, selected) {
			if matched != nil {
				return nil, fmt.Errorf("%w: selected origin is ambiguous", ErrDestinationDenied)
			}
			matched = base
		}
	}
	if matched == nil {
		return nil, fmt.Errorf("%w: selected origin is not source-admitted", ErrDestinationDenied)
	}
	return matched, nil
}

func canonicalHTTPBase(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil {
		return nil, ErrDestinationDenied
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, ErrDestinationDenied
	}
	if parsed.User != nil || parsed.Host == "" || parsed.Opaque != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, ErrDestinationDenied
	}
	if parsed.Path != "" &&
		(!strings.HasPrefix(parsed.Path, "/") || unsafeHTTPPath(parsed.Path) ||
			strings.ContainsAny(parsed.Path, "{}")) {
		return nil, ErrDestinationDenied
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" || strings.Contains(host, "%") {
		return nil, ErrDestinationDenied
	}
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return nil, ErrDestinationDenied
	}
	parsed.Host = net.JoinHostPort(host, strconv.FormatUint(portNumber, 10))
	if parsed.Path == "/" {
		parsed.Path = ""
		parsed.RawPath = ""
	}
	return parsed, nil
}

func canonicalOrigin(raw string) (*url.URL, error) {
	parsed, err := canonicalHTTPBase(raw)
	if err != nil || parsed.Path != "" || parsed.RawPath != "" {
		return nil, ErrDestinationDenied
	}
	return parsed, nil
}

func sameHTTPBase(left, right *url.URL) bool {
	return sameOrigin(left, right) &&
		strings.TrimSuffix(left.EscapedPath(), "/") == strings.TrimSuffix(right.EscapedPath(), "/")
}

func sameOrigin(left, right *url.URL) bool {
	return left != nil && right != nil && left.Scheme == right.Scheme && strings.EqualFold(left.Host, right.Host)
}

func newPinnedDialContext(
	resolver DNSResolver,
	dialer ContextDialer,
	origin *url.URL,
	maxAnswers uint32,
) func(context.Context, string, string) (net.Conn, error) {
	var resolveOnce sync.Once
	var pinned []netip.Addr
	var resolveErr error
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if dialer == nil {
			return nil, fmt.Errorf("%w: dialer is absent", ErrDestinationDenied)
		}
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("%w: transport address is invalid", ErrDestinationDenied)
		}
		if !strings.EqualFold(host, origin.Hostname()) || port != origin.Port() {
			return nil, fmt.Errorf("%w: transport attempted a different origin", ErrDestinationDenied)
		}
		resolveOnce.Do(func() {
			pinned, resolveErr = resolveAddresses(ctx, resolver, host, maxAnswers)
			if resolveErr == nil {
				// Resolver order is not routing authority. The one selected
				// numeric address is immutable for this physical call.
				sort.Slice(pinned, func(i, j int) bool { return pinned[i].Compare(pinned[j]) < 0 })
				pinned = pinned[:1]
			}
		})
		if resolveErr != nil {
			return nil, resolveErr
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(pinned[0].String(), port))
	}
}

func resolveAddresses(ctx context.Context, resolver DNSResolver, host string, maxAnswers uint32) ([]netip.Addr, error) {
	if address, err := netip.ParseAddr(host); err == nil {
		if err := validatePublicHTTPAddress(address.Unmap()); err != nil {
			return nil, err
		}
		return []netip.Addr{address.Unmap()}, nil
	}
	if resolver == nil {
		return nil, fmt.Errorf("%w: resolver is absent", ErrDestinationDenied)
	}
	addresses, err := resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("%w: DNS resolution failed", ErrDestinationDenied)
	}
	if len(addresses) == 0 || uint64(len(addresses)) > uint64(maxAnswers) {
		return nil, fmt.Errorf("%w: DNS answer count is empty or exceeds its bound", ErrDestinationDenied)
	}
	seen := make(map[netip.Addr]struct{}, len(addresses))
	validated := make([]netip.Addr, 0, len(addresses))
	for _, address := range addresses {
		address = address.Unmap()
		if err := validatePublicHTTPAddress(address); err != nil {
			return nil, err
		}
		if _, duplicate := seen[address]; duplicate {
			continue
		}
		seen[address] = struct{}{}
		validated = append(validated, address)
	}
	if len(validated) == 0 {
		return nil, fmt.Errorf("%w: DNS answers contain no usable address", ErrDestinationDenied)
	}
	return validated, nil
}

func validatePublicHTTPAddress(address netip.Addr) error {
	if !address.IsValid() || !address.IsGlobalUnicast() {
		return fmt.Errorf("%w: address is not global unicast", ErrDestinationDenied)
	}
	for _, prefix := range forbiddenHTTPPrefixes {
		if prefix.Contains(address) {
			return fmt.Errorf("%w: private, metadata, loopback, documentation, or reserved address", ErrDestinationDenied)
		}
	}
	return nil
}

// ValidatePrivateDatabaseDestination is a separate policy surface for an
// explicitly approved database destination. It cannot be used by ExecuteHTTP.
func ValidatePrivateDatabaseDestination(address netip.Addr, allowance PrivateDatabaseAllowance) error {
	address = address.Unmap()
	if !address.IsValid() {
		return fmt.Errorf("%w: database address is invalid", ErrDestinationDenied)
	}
	for _, prefix := range allowance.Prefixes {
		if prefix.IsValid() && prefix.Contains(address) {
			return nil
		}
	}
	return fmt.Errorf("%w: database address is outside its explicit private allowance", ErrDestinationDenied)
}

func mustPrefix(value string) netip.Prefix {
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		panic(err)
	}
	return prefix
}

var forbiddenHTTPPrefixes = []netip.Prefix{
	mustPrefix("0.0.0.0/8"),
	mustPrefix("10.0.0.0/8"),
	mustPrefix("100.64.0.0/10"),
	mustPrefix("127.0.0.0/8"),
	mustPrefix("169.254.0.0/16"),
	mustPrefix("172.16.0.0/12"),
	mustPrefix("192.0.0.0/24"),
	mustPrefix("192.0.2.0/24"),
	mustPrefix("192.168.0.0/16"),
	mustPrefix("198.18.0.0/15"),
	mustPrefix("198.51.100.0/24"),
	mustPrefix("203.0.113.0/24"),
	mustPrefix("224.0.0.0/4"),
	mustPrefix("240.0.0.0/4"),
	mustPrefix("::/128"),
	mustPrefix("::1/128"),
	mustPrefix("64:ff9b::/96"),
	mustPrefix("64:ff9b:1::/48"),
	mustPrefix("2001::/32"),
	mustPrefix("2002::/16"),
	mustPrefix("fc00::/7"),
	mustPrefix("fe80::/10"),
	mustPrefix("ff00::/8"),
	mustPrefix("2001:db8::/32"),
}
