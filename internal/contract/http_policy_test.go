package contract

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestHTTPExecutionPolicyStrictDecoding(t *testing.T) {
	valid := `{"max_request_bytes":9223372036854775806,"max_response_bytes":9223372036854775806,"max_dns_answers":4294967295,"max_attempts":4294967295,"max_retry_delay":9223372036854775807,"selected_origin":"https://api.example.test/v1"}`
	var decoded HTTPExecutionPolicy
	if err := json.Unmarshal([]byte(valid), &decoded); err != nil {
		t.Fatalf("decode maximum finite HTTP policy: %v", err)
	}
	want := HTTPExecutionPolicy{
		MaxRequestBytes:  uint64(math.MaxInt64 - 1),
		MaxResponseBytes: uint64(math.MaxInt64 - 1),
		MaxDNSAnswers:    ^uint32(0),
		MaxAttempts:      ^uint32(0),
		MaxRetryDelay:    time.Duration(math.MaxInt64),
		SelectedOrigin:   "https://api.example.test/v1",
	}
	if decoded != want {
		t.Fatalf("decoded HTTP policy = %#v; want %#v", decoded, want)
	}

	invalid := []struct {
		name     string
		document string
	}{
		{name: "missing required", document: strings.Replace(valid, `,"selected_origin":"https://api.example.test/v1"`, "", 1)},
		{name: "duplicate", document: strings.Replace(valid, `"max_attempts":4294967295`, `"max_attempts":1,"max_attempts":4294967295`, 1)},
		{name: "unknown", document: strings.Replace(valid, `}`, `,"request_timeout":1}`, 1)},
		{name: "document null", document: `null`},
		{name: "field null", document: strings.Replace(valid, `"max_retry_delay":9223372036854775807`, `"max_retry_delay":null`, 1)},
		{name: "negative unsigned sentinel", document: strings.Replace(valid, `"max_request_bytes":9223372036854775806`, `"max_request_bytes":-1`, 1)},
		{name: "signed byte sentinel", document: strings.Replace(valid, `"max_request_bytes":9223372036854775806`, `"max_request_bytes":9223372036854775807`, 1)},
		{name: "uint64 overflow", document: strings.Replace(valid, `"max_request_bytes":9223372036854775806`, `"max_request_bytes":18446744073709551616`, 1)},
		{name: "uint32 overflow", document: strings.Replace(valid, `"max_dns_answers":4294967295`, `"max_dns_answers":4294967296`, 1)},
		{name: "fractional duration", document: strings.Replace(valid, `"max_retry_delay":9223372036854775807`, `"max_retry_delay":1.5`, 1)},
		{name: "zero attempt bound", document: strings.Replace(valid, `"max_attempts":4294967295`, `"max_attempts":0`, 1)},
		{name: "trailing value", document: valid + `{}`},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			var policy HTTPExecutionPolicy
			if err := json.Unmarshal([]byte(test.document), &policy); err == nil {
				t.Fatalf("accepted invalid HTTP policy: %s", test.document)
			}
		})
	}
}

func TestHTTPExecutionPolicyValidationBoundaries(t *testing.T) {
	valid := validHTTPExecutionPolicyForTest()
	valid.MaxRequestBytes = maximumHTTPExecutionBytes
	valid.MaxResponseBytes = maximumHTTPExecutionBytes
	valid.MaxDNSAnswers = ^uint32(0)
	valid.MaxAttempts = ^uint32(0)
	valid.MaxRetryDelay = time.Duration(math.MaxInt64)
	if err := valid.Validate(); err != nil {
		t.Fatalf("maximum finite HTTP policy: %v", err)
	}

	invalid := []struct {
		name   string
		mutate func(*HTTPExecutionPolicy)
	}{
		{name: "zero request bytes", mutate: func(policy *HTTPExecutionPolicy) { policy.MaxRequestBytes = 0 }},
		{name: "request bytes exceed signed finite bound", mutate: func(policy *HTTPExecutionPolicy) { policy.MaxRequestBytes = uint64(math.MaxInt64) }},
		{name: "zero response bytes", mutate: func(policy *HTTPExecutionPolicy) { policy.MaxResponseBytes = 0 }},
		{name: "response bytes exceed signed finite bound", mutate: func(policy *HTTPExecutionPolicy) { policy.MaxResponseBytes = uint64(math.MaxInt64) }},
		{name: "zero DNS answers", mutate: func(policy *HTTPExecutionPolicy) { policy.MaxDNSAnswers = 0 }},
		{name: "zero attempts", mutate: func(policy *HTTPExecutionPolicy) { policy.MaxAttempts = 0 }},
		{name: "zero retry delay", mutate: func(policy *HTTPExecutionPolicy) { policy.MaxRetryDelay = 0 }},
		{name: "negative retry delay", mutate: func(policy *HTTPExecutionPolicy) { policy.MaxRetryDelay = -time.Nanosecond }},
		{name: "HTTP origin", mutate: func(policy *HTTPExecutionPolicy) { policy.SelectedOrigin = "http://api.example.test/v1" }},
		{name: "origin credentials", mutate: func(policy *HTTPExecutionPolicy) { policy.SelectedOrigin = "https://ada@api.example.test/v1" }},
		{name: "origin query", mutate: func(policy *HTTPExecutionPolicy) { policy.SelectedOrigin = "https://api.example.test/v1?cursor=1" }},
		{name: "origin fragment", mutate: func(policy *HTTPExecutionPolicy) { policy.SelectedOrigin = "https://api.example.test/v1#result" }},
		{name: "origin template", mutate: func(policy *HTTPExecutionPolicy) { policy.SelectedOrigin = "https://api.example.test/{tenant}" }},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			policy := validHTTPExecutionPolicyForTest()
			test.mutate(&policy)
			if err := policy.Validate(); err == nil {
				t.Fatalf("accepted invalid HTTP policy: %#v", policy)
			}
		})
	}
}

func TestHTTPExecutionPolicyValidatesSourceOriginSelection(t *testing.T) {
	policy := validHTTPExecutionPolicyForTest()
	policy.SelectedOrigin = "https://API.EXAMPLE.TEST/v1"
	operationWithOrigins := func(origins ...string) Operation {
		return Operation{Execution: &OperationExecution{HTTP: &OperationHTTP{Origins: origins}}}
	}

	tests := []struct {
		name      string
		operation Operation
		wantError bool
	}{
		{
			name:      "exact canonical base path among admitted origins",
			operation: operationWithOrigins("https://api.example.test/v2", "https://api.example.test/v1"),
		},
		{name: "missing execution authority", operation: Operation{}, wantError: true},
		{name: "missing HTTP authority", operation: Operation{Execution: &OperationExecution{}}, wantError: true},
		{name: "empty HTTP origins", operation: operationWithOrigins(), wantError: true},
		{name: "different base path", operation: operationWithOrigins("https://api.example.test/v2"), wantError: true},
		{name: "different audience", operation: operationWithOrigins("https://other.example.test/v1"), wantError: true},
		{
			name:      "ambiguous canonical authority",
			operation: operationWithOrigins("https://api.example.test/v1", "https://API.EXAMPLE.TEST/v1"),
			wantError: true,
		},
		{
			name:      "malformed source authority",
			operation: operationWithOrigins("https://api.example.test/v1", "https://api.example.test/{tenant}"),
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := policy.ValidateForOperation(test.operation)
			if test.wantError && err == nil {
				t.Fatal("invalid operation authority was accepted")
			}
			if !test.wantError && err != nil {
				t.Fatalf("valid operation authority: %v", err)
			}
		})
	}
}

func validHTTPExecutionPolicyForTest() HTTPExecutionPolicy {
	return HTTPExecutionPolicy{
		MaxRequestBytes:  1024,
		MaxResponseBytes: 4096,
		MaxDNSAnswers:    8,
		MaxAttempts:      3,
		MaxRetryDelay:    2 * time.Second,
		SelectedOrigin:   "https://api.example.test/v1",
	}
}
