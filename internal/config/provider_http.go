package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// ProviderHTTPConfiguration is absent unless the operator explicitly declares
// complete finite HTTP controls and at least one scoped connection destination.
type ProviderHTTPConfiguration struct {
	maxRequestBytes      uint64
	maxResponseBytes     uint64
	maxDNSAnswers        uint32
	maxAttempts          uint32
	maxRetryDelay        time.Duration
	destinations         []providerHTTPDestination
	policies             map[providerHTTPDestinationKey]contract.HTTPExecutionPolicy
	tlsTrustRootRef      string
	tlsTrustRootDeclared bool
	configured           bool
}

type providerHTTPDestination struct {
	scope        contract.Scope
	connectionID string
	origin       string
}

type providerHTTPDestinationKey struct {
	orgID        string
	workspaceID  string
	connectionID string
}

func (configuration ProviderHTTPConfiguration) Configured() bool {
	return configuration.configured
}

// TLSTrustRootRef returns the exact declared operator secret reference for the
// provider egress trust root. The declaration is optional: an absent reference
// returns false and leaves provider egress on the process system root store. A
// declared reference is retained verbatim and never falls back to another
// reference or to the system store.
func (configuration ProviderHTTPConfiguration) TLSTrustRootRef() (string, bool) {
	if !configuration.configured || !configuration.tlsTrustRootDeclared {
		return "", false
	}
	return configuration.tlsTrustRootRef, true
}

// Policy returns a value copy only for the exact configured organization,
// workspace, and connection binding. It never falls back to another scope or
// destination.
func (configuration ProviderHTTPConfiguration) Policy(
	scope contract.Scope,
	connectionID string,
) (contract.HTTPExecutionPolicy, bool) {
	if !configuration.configured {
		return contract.HTTPExecutionPolicy{}, false
	}
	policy, present := configuration.policies[providerHTTPDestinationKey{
		orgID: scope.OrgID, workspaceID: scope.WorkspaceID, connectionID: connectionID,
	}]
	return policy, present
}

func (configuration ProviderHTTPConfiguration) clone() ProviderHTTPConfiguration {
	cloned := configuration
	cloned.destinations = append([]providerHTTPDestination(nil), configuration.destinations...)
	if configuration.policies != nil {
		cloned.policies = make(map[providerHTTPDestinationKey]contract.HTTPExecutionPolicy, len(configuration.policies))
		for key, policy := range configuration.policies {
			cloned.policies[key] = policy
		}
	}
	return cloned
}

var providerHTTPRequiredFields = []string{
	"max_request_bytes",
	"max_response_bytes",
	"max_dns_answers",
	"max_attempts",
	"max_retry_delay",
	"destinations",
}

// The operator-owned provider egress trust root is the only optional key; every
// other accepted key is part of the complete finite control set.
var providerHTTPFields = append(
	append([]string(nil), providerHTTPRequiredFields...),
	"tls_trust_root_ref",
)

var providerHTTPDestinationFields = []string{
	"org_id",
	"workspace_id",
	"connection_id",
	"origin",
}

func decodeProviderHTTP(raw json.RawMessage) (ProviderHTTPConfiguration, error) {
	if len(raw) == 0 {
		return ProviderHTTPConfiguration{}, nil
	}
	object, err := decodeObject(raw, "provider_http", providerHTTPFields, providerHTTPRequiredFields)
	if err != nil {
		return ProviderHTTPConfiguration{}, err
	}
	configuration := ProviderHTTPConfiguration{configured: true}
	if configuration.maxRequestBytes, err = decodeUint64(
		object,
		"max_request_bytes",
		"provider_http.max_request_bytes",
	); err != nil {
		return ProviderHTTPConfiguration{}, err
	}
	if configuration.maxResponseBytes, err = decodeUint64(
		object,
		"max_response_bytes",
		"provider_http.max_response_bytes",
	); err != nil {
		return ProviderHTTPConfiguration{}, err
	}
	if configuration.maxDNSAnswers, err = decodeProviderHTTPUint32(
		object,
		"max_dns_answers",
		"provider_http.max_dns_answers",
	); err != nil {
		return ProviderHTTPConfiguration{}, err
	}
	if configuration.maxAttempts, err = decodeProviderHTTPUint32(
		object,
		"max_attempts",
		"provider_http.max_attempts",
	); err != nil {
		return ProviderHTTPConfiguration{}, err
	}
	if configuration.maxRetryDelay, err = decodeDuration(
		object,
		"max_retry_delay",
		"provider_http.max_retry_delay",
	); err != nil {
		return ProviderHTTPConfiguration{}, err
	}
	if configuration.destinations, err = decodeProviderHTTPDestinations(object["destinations"]); err != nil {
		return ProviderHTTPConfiguration{}, err
	}
	if _, declared := object["tls_trust_root_ref"]; declared {
		configuration.tlsTrustRootDeclared = true
		if configuration.tlsTrustRootRef, err = decodeString(
			object,
			"tls_trust_root_ref",
			"provider_http.tls_trust_root_ref",
		); err != nil {
			return ProviderHTTPConfiguration{}, err
		}
	}
	return configuration, nil
}

func decodeProviderHTTPUint32(
	object map[string]json.RawMessage,
	name string,
	field string,
) (uint32, error) {
	value, err := decodeUint64(object, name, field)
	if err != nil {
		return 0, err
	}
	const maximum = uint64(1<<32 - 1)
	if value > maximum {
		return 0, diagnostic(field, DiagnosticBounds)
	}
	return uint32(value), nil
}

func decodeProviderHTTPDestinations(raw json.RawMessage) ([]providerHTTPDestination, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, diagnostic("provider_http.destinations", DiagnosticType)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(trimmed, &entries); err != nil {
		return nil, diagnostic("provider_http.destinations", DiagnosticType)
	}
	destinations := make([]providerHTTPDestination, len(entries))
	for index, entry := range entries {
		path := "provider_http.destinations[" + strconv.Itoa(index) + "]"
		object, err := decodeObject(entry, path, providerHTTPDestinationFields, providerHTTPDestinationFields)
		if err != nil {
			return nil, err
		}
		if destinations[index].scope.OrgID, err = decodeString(object, "org_id", path+".org_id"); err != nil {
			return nil, err
		}
		if destinations[index].scope.WorkspaceID, err = decodeString(object, "workspace_id", path+".workspace_id"); err != nil {
			return nil, err
		}
		if destinations[index].connectionID, err = decodeString(object, "connection_id", path+".connection_id"); err != nil {
			return nil, err
		}
		if destinations[index].origin, err = decodeString(object, "origin", path+".origin"); err != nil {
			return nil, err
		}
	}
	return destinations, nil
}

func resolveProviderHTTP(configuration ProviderHTTPConfiguration) (ProviderHTTPConfiguration, error) {
	if !configuration.configured {
		return ProviderHTTPConfiguration{}, nil
	}
	if len(configuration.destinations) == 0 {
		return ProviderHTTPConfiguration{}, diagnostic("provider_http.destinations", DiagnosticBounds)
	}
	if configuration.tlsTrustRootDeclared {
		if err := validateSecretReference(
			configuration.tlsTrustRootRef,
			"provider_http.tls_trust_root_ref",
		); err != nil {
			return ProviderHTTPConfiguration{}, err
		}
	}
	policies := make(map[providerHTTPDestinationKey]contract.HTTPExecutionPolicy, len(configuration.destinations))
	for index, destination := range configuration.destinations {
		path := "provider_http.destinations[" + strconv.Itoa(index) + "]"
		if err := destination.scope.Validate(); err != nil {
			return ProviderHTTPConfiguration{}, providerHTTPScopeDiagnostic(err, path)
		}
		if !validProviderHTTPConnectionID(destination.connectionID) {
			return ProviderHTTPConfiguration{}, diagnostic(path+".connection_id", DiagnosticValue)
		}
		key := providerHTTPDestinationKey{
			orgID: destination.scope.OrgID, workspaceID: destination.scope.WorkspaceID, connectionID: destination.connectionID,
		}
		if _, duplicate := policies[key]; duplicate {
			return ProviderHTTPConfiguration{}, diagnostic(path+".connection_id", DiagnosticDuplicate)
		}
		policy := contract.HTTPExecutionPolicy{
			MaxRequestBytes:  configuration.maxRequestBytes,
			MaxResponseBytes: configuration.maxResponseBytes,
			MaxDNSAnswers:    configuration.maxDNSAnswers,
			MaxAttempts:      configuration.maxAttempts,
			MaxRetryDelay:    configuration.maxRetryDelay,
			SelectedOrigin:   destination.origin,
		}
		if err := policy.Validate(); err != nil {
			return ProviderHTTPConfiguration{}, providerHTTPPolicyDiagnostic(err, path+".origin")
		}
		policies[key] = policy
	}
	return ProviderHTTPConfiguration{
		configured:           true,
		policies:             policies,
		tlsTrustRootRef:      configuration.tlsTrustRootRef,
		tlsTrustRootDeclared: configuration.tlsTrustRootDeclared,
	}, nil
}

func providerHTTPScopeDiagnostic(err error, path string) error {
	var contractErr *contract.ContractError
	if errors.As(err, &contractErr) {
		switch contractErr.Path {
		case "scope.org_id":
			return diagnostic(path+".org_id", DiagnosticValue)
		case "scope.workspace_id":
			return diagnostic(path+".workspace_id", DiagnosticValue)
		}
	}
	return diagnostic(path, DiagnosticValue)
}

func providerHTTPPolicyDiagnostic(err error, originField string) error {
	var contractErr *contract.ContractError
	if errors.As(err, &contractErr) {
		switch contractErr.Path {
		case "http_execution_policy.max_request_bytes":
			return diagnostic("provider_http.max_request_bytes", DiagnosticBounds)
		case "http_execution_policy.max_response_bytes":
			return diagnostic("provider_http.max_response_bytes", DiagnosticBounds)
		case "http_execution_policy.max_dns_answers":
			return diagnostic("provider_http.max_dns_answers", DiagnosticBounds)
		case "http_execution_policy.max_attempts":
			return diagnostic("provider_http.max_attempts", DiagnosticBounds)
		case "http_execution_policy.max_retry_delay":
			return diagnostic("provider_http.max_retry_delay", DiagnosticBounds)
		case "http_execution_policy.selected_origin":
			return diagnostic(originField, DiagnosticValue)
		}
	}
	return diagnostic("provider_http", DiagnosticValue)
}

// Connection IDs use the same bounded opaque identity grammar as contract
// scopes and connection grants. There is no standalone exported connection ID
// value solely for configuration parsing.
func validProviderHTTPConnectionID(value string) bool {
	if value == "" || len(value) > 512 || value != strings.TrimSpace(value) || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}
