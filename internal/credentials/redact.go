package credentials

import (
	"encoding/json"
)

// Diagnostic is untrusted provider-facing diagnostic input. Headers, bodies,
// errors and messages are intentionally excluded from every projection; they
// may contain reflected credentials even when a provider labels them safe.
type Diagnostic struct {
	Code    string              `json:"-"`
	Message string              `json:"-"`
	Headers map[string][]string `json:"-"`
	Body    []byte              `json:"-"`
	Err     error               `json:"-"`
}

func (diagnostic Diagnostic) String() string {
	return "credential diagnostic [REDACTED]"
}

func (diagnostic Diagnostic) GoString() string {
	return diagnostic.String()
}

func (diagnostic Diagnostic) MarshalJSON() ([]byte, error) {
	return json.Marshal(Redact(diagnostic))
}

type SafeDiagnostic struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

var safeDiagnosticDetails = map[string]string{
	"credential_authorization_denied":      "credential authorization was denied",
	"credential_unavailable":               "credential material is unavailable",
	"credential_stale":                     "credential binding is no longer current",
	"credential_wrapping_key_unavailable":  "credential encryption is unavailable",
	"credential_refresh_busy":              "credential refresh is already in progress",
	"credential_refresh_rejected":          "credential refresh was rejected",
	"credential_refresh_outcome_uncertain": "credential refresh outcome is uncertain and reauthorization is required",
	"credential_reauthorization_required":  "credential reauthorization is required",
}

// Redact returns an allowlisted diagnostic selected only by a fixed safe code.
// It never reflects the input message, headers, body, error text, or known
// secret bytes. The variadic secret argument lets callers make the security
// boundary explicit without retaining or serializing those values.
func Redact(diagnostic Diagnostic, knownSecrets ...[]byte) SafeDiagnostic {
	_ = knownSecrets
	if detail, ok := safeDiagnosticDetails[diagnostic.Code]; ok {
		return SafeDiagnostic{Code: diagnostic.Code, Detail: detail}
	}
	return SafeDiagnostic{
		Code:   "credential_operation_failed",
		Detail: "credential operation failed",
	}
}
