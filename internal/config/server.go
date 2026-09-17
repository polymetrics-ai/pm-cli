package config

import (
	"encoding/json"
	"net/netip"
	"strconv"
	"time"
)

// ServerConfiguration is resolved by the same strict document reader as the
// dependency settings. TrustedProxies is copied at the configuration boundary.
type ServerConfiguration struct {
	Listen            string
	TrustedProxies    []string
	ReadHeaderTimeout time.Duration
	RequestTimeout    time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	DrainTimeout      time.Duration
	ProbeTimeout      time.Duration
	MaxHeaderBytes    uint64
	MaxConnections    uint64
}

func (c Configuration) Server() ServerConfiguration {
	value := c.server
	value.TrustedProxies = append([]string(nil), c.server.TrustedProxies...)
	return value
}

func defaultServerConfiguration() ServerConfiguration {
	return ServerConfiguration{
		Listen:            "127.0.0.1:8080",
		ReadHeaderTimeout: 5 * time.Second,
		RequestTimeout:    30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       time.Minute,
		DrainTimeout:      30 * time.Second,
		ProbeTimeout:      5 * time.Second,
		MaxHeaderBytes:    16 << 10,
		MaxConnections:    256,
	}
}

var serverFields = []string{
	"listen", "trusted_proxies", "read_header_timeout", "request_timeout",
	"write_timeout", "idle_timeout", "drain_timeout", "probe_timeout",
	"max_header_bytes", "max_connections",
}

func decodeServer(raw json.RawMessage) (ServerConfiguration, error) {
	result := defaultServerConfiguration()
	if len(raw) == 0 {
		return result, nil
	}
	object, err := decodeObject(raw, "server", serverFields, nil)
	if err != nil {
		return ServerConfiguration{}, err
	}
	if _, present := object["listen"]; present {
		result.Listen, err = decodeString(object, "listen", "server.listen")
		if err != nil {
			return ServerConfiguration{}, err
		}
	}
	if _, present := object["trusted_proxies"]; present {
		result.TrustedProxies, err = decodeStringArray(object, "trusted_proxies", "server.trusted_proxies")
		if err != nil {
			return ServerConfiguration{}, err
		}
	}
	for _, field := range []struct {
		name  string
		value *time.Duration
	}{
		{"read_header_timeout", &result.ReadHeaderTimeout},
		{"request_timeout", &result.RequestTimeout},
		{"write_timeout", &result.WriteTimeout},
		{"idle_timeout", &result.IdleTimeout},
		{"drain_timeout", &result.DrainTimeout},
		{"probe_timeout", &result.ProbeTimeout},
	} {
		if _, present := object[field.name]; !present {
			continue
		}
		*field.value, err = decodeDuration(object, field.name, "server."+field.name)
		if err != nil {
			return ServerConfiguration{}, err
		}
	}
	for _, field := range []struct {
		name  string
		value *uint64
	}{
		{"max_header_bytes", &result.MaxHeaderBytes},
		{"max_connections", &result.MaxConnections},
	} {
		if _, present := object[field.name]; !present {
			continue
		}
		*field.value, err = decodeUint64(object, field.name, "server."+field.name)
		if err != nil {
			return ServerConfiguration{}, err
		}
	}
	return result, nil
}

func validateServer(value ServerConfiguration) error {
	address, err := netip.ParseAddrPort(value.Listen)
	if err != nil || address.Port() == 0 || address.Addr().Zone() != "" {
		return diagnostic("server.listen", DiagnosticValue)
	}
	if len(value.TrustedProxies) > 32 {
		return diagnostic("server.trusted_proxies", DiagnosticBounds)
	}
	seen := make(map[netip.Prefix]struct{}, len(value.TrustedProxies))
	for index, proxy := range value.TrustedProxies {
		field := "server.trusted_proxies[" + strconv.Itoa(index) + "]"
		prefix, parseErr := netip.ParsePrefix(proxy)
		if parseErr != nil {
			address, addressErr := netip.ParseAddr(proxy)
			if addressErr != nil || address.Zone() != "" {
				return diagnostic(field, DiagnosticValue)
			}
			prefix = netip.PrefixFrom(address, address.BitLen())
		}
		if prefix.Bits() == 0 || prefix != prefix.Masked() || prefix.Addr().Is4In6() {
			return diagnostic(field, DiagnosticValue)
		}
		if _, exists := seen[prefix]; exists {
			return diagnostic(field, DiagnosticDuplicate)
		}
		seen[prefix] = struct{}{}
	}
	for _, field := range []struct {
		name  string
		value time.Duration
	}{
		{"read_header_timeout", value.ReadHeaderTimeout},
		{"request_timeout", value.RequestTimeout},
		{"write_timeout", value.WriteTimeout},
		{"idle_timeout", value.IdleTimeout},
		{"drain_timeout", value.DrainTimeout},
		{"probe_timeout", value.ProbeTimeout},
	} {
		if field.value <= 0 {
			return diagnostic("server."+field.name, DiagnosticValue)
		}
	}
	if value.MaxHeaderBytes == 0 || value.MaxHeaderBytes > 1<<20 {
		return diagnostic("server.max_header_bytes", DiagnosticBounds)
	}
	if value.MaxConnections == 0 || value.MaxConnections > 1<<16 {
		return diagnostic("server.max_connections", DiagnosticBounds)
	}
	return nil
}
