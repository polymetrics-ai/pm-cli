package compile

import (
	"fmt"
	"strings"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestNoSecretOrPolicyOverlayEscape(t *testing.T) {
	base := `{
  "schema_version":"pm.semantics/v1",
  "connector_id":"synthetic",
  "source_digest":"sha256:%s",
  "revision":"sha256:%s",
  "operations":[],
  %s
}`
	for _, test := range []struct {
		name  string
		field string
	}{
		{name: "origin expansion", field: `"origins":["https://attacker.invalid"]`},
		{name: "credential injection", field: `"credentials":{"token":"secret"}`},
		{name: "executable overlay", field: `"executable":"/bin/sh"`},
		{name: "capability expansion", field: `"capability_requirements":[{"capability":"arbitrary.execute","foundation":{"id":"FND-99","version":"1.0.0"}}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := fmt.Sprintf(base, strings.Repeat("1", 64), strings.Repeat("2", 64), test.field)
			if _, err := DecodeSemantics([]byte(document)); err == nil {
				t.Fatalf("unsafe semantic overlay %s was accepted", test.name)
			}
		})
	}

	previous := "sha256:" + strings.Repeat("3", 64)
	current := "sha256:" + strings.Repeat("4", 64)
	if err := ValidateRevisionDiff(previous, current, nil); err == nil {
		t.Fatal("changed source identity without an explicit revision diff was accepted")
	}
	diff := &SourceRevisionDiff{
		SchemaVersion:  SourceRevisionDiffSchemaVersion,
		PreviousDigest: previous,
		CurrentDigest:  current,
		Changes: []PointerChange{{
			SourceID: "root", Pointer: "/paths/~1widgets/get",
			PreviousSHA256: "sha256:" + strings.Repeat("5", 64),
			CurrentSHA256:  "sha256:" + strings.Repeat("6", 64),
		}},
	}
	if err := ValidateRevisionDiff(previous, current, diff); err != nil {
		t.Fatalf("exact pointer-level revision diff was rejected: %v", err)
	}
	if err := ValidateRevisionDiff(current, current, diff); err == nil {
		t.Fatal("revision diff was accepted without a source identity change")
	}
}

func TestDecodeSemanticsRejectsLossyExecutionFacts(t *testing.T) {
	envelope := `{
  "schema_version":"pm.semantics/v1",
  "connector_id":"synthetic",
  "source_digest":"sha256:%s",
  "revision":"sha256:%s",
  "operations":[{
    "operation_id":"listItems",
    "source_refs":[{"source_id":"root","pointer":"#/paths/~1items/get"}],
    %s,
    "lanes":[]
  }]
}`
	witness := `"witness":{"proof_id":"source-reviewed","source_refs":[{"source_id":"root","pointer":"#/paths/~1items/get"}]}`
	validFacts := `"http":{"origins":["https://api.example.test/v3"],"headers":[{"name":"X-API-Version","value":"2022-11-28"}],` + witness + `},` +
		`"pagination":{"kind":"cursor","cursor_request_field":"cursor","cursor_response_pointer":"/next",` + witness + `},` +
		`"retry":{"classification":"unsafe","http_statuses":[429,502],` + witness + `}`
	valid := fmt.Sprintf(envelope, strings.Repeat("1", 64), strings.Repeat("2", 64), validFacts)
	if _, err := DecodeSemantics([]byte(valid)); err != nil {
		t.Fatalf("valid execution facts were rejected: %v", err)
	}

	for _, test := range []struct {
		name  string
		facts string
	}{
		{
			name: "empty field from another pagination kind",
			facts: `"pagination":{"kind":"cursor","cursor_request_field":"cursor","cursor_response_pointer":"/next",` +
				`"offset_request_field":"",` + witness + `}`,
		},
		{
			name: "explicit empty optional response pointer",
			facts: `"pagination":{"kind":"offset","offset_request_field":"offset","limit_request_field":"limit",` +
				`"total_response_pointer":"",` + witness + `}`,
		},
		{
			name:  "empty condition on unconditional retry fact",
			facts: `"retry":{"classification":"unsafe","condition":"",` + witness + `}`,
		},
		{
			name:  "explicit empty HTTP headers",
			facts: `"http":{"origins":["https://api.example.test/v3"],"headers":[],` + witness + `}`,
		},
		{
			name:  "null HTTP authorization",
			facts: `"http":{"origins":["https://api.example.test/v3"],"authorization":null,` + witness + `}`,
		},
		{
			name: "link field on cursor pagination",
			facts: `"pagination":{"kind":"cursor","cursor_request_field":"cursor","cursor_response_pointer":"/next",` +
				`"link_response_header":"Link",` + witness + `}`,
		},
		{
			name:  "empty retry status facts",
			facts: `"retry":{"classification":"unsafe","http_statuses":[],` + witness + `}`,
		},
		{
			name:  "duplicate retry status facts",
			facts: `"retry":{"classification":"unsafe","http_statuses":[429,429],` + witness + `}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := fmt.Sprintf(envelope, strings.Repeat("1", 64), strings.Repeat("2", 64), test.facts)
			if _, err := DecodeSemantics([]byte(document)); err == nil {
				t.Fatal("lossy execution fact was accepted")
			}
		})
	}
}

func TestHTTPAmendmentCannotExpandSourceAuthority(t *testing.T) {
	source := &HTTPSemantics{
		Origins: []string{"https://api.example.test/v3", "https://api.example.test/v4"},
		Authorization: &contract.OperationAuthorization{
			Required:  false,
			Audience:  "https://api.example.test",
			Placement: contract.FieldLocationHeader,
			WireName:  "Authorization",
			Prefix:    "Bearer ",
			FieldID:   "auth.token",
		},
		Headers: []contract.OperationHTTPHeader{{Name: "X-API-Version", Value: "2022-11-28"}},
		Witness: Witness{
			ProofID: "source-http",
			SourceRefs: []contract.SourceRef{
				{SourceID: "root", Pointer: "#/servers/0"},
				{SourceID: "root", Pointer: "#/servers/1"},
				{SourceID: "root", Pointer: "#/security"},
			},
		},
	}
	narrowed := source.clone()
	narrowed.Origins = []string{"https://api.example.test/v3"}
	narrowed.Authorization = nil
	narrowed.Witness.SourceRefs = []contract.SourceRef{{SourceID: "root", Pointer: "#/servers/0"}}
	if err := validateHTTPAmendment(source, &narrowed); err != nil {
		t.Fatalf("source-subset HTTP amendment was rejected: %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*HTTPSemantics)
	}{
		{
			name: "origin expansion",
			mutate: func(amendment *HTTPSemantics) {
				amendment.Origins = []string{"https://attacker.invalid"}
			},
		},
		{
			name: "credential injection",
			mutate: func(amendment *HTTPSemantics) {
				amendment.Authorization = &contract.OperationAuthorization{
					Audience:  "https://api.example.test",
					Placement: contract.FieldLocationHeader,
					WireName:  "X-Injected-Token",
					FieldID:   "auth.injected",
				}
			},
		},
		{
			name: "fixed header rewrite",
			mutate: func(amendment *HTTPSemantics) {
				amendment.Headers[0].Value = "attacker"
			},
		},
		{
			name: "witness expansion",
			mutate: func(amendment *HTTPSemantics) {
				amendment.Witness.SourceRefs = []contract.SourceRef{{
					SourceID: "root",
					Pointer:  "#/paths/~1unrelated/get",
				}}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			amendment := source.clone()
			test.mutate(&amendment)
			if err := validateHTTPAmendment(source, &amendment); err == nil {
				t.Fatal("HTTP amendment expanded independently normalized source authority")
			}
		})
	}

	required := source.clone()
	required.Authorization.Required = true
	withoutAuthorization := required.clone()
	withoutAuthorization.Authorization = nil
	if err := validateHTTPAmendment(&required, &withoutAuthorization); err == nil {
		t.Fatal("HTTP amendment removed source-required authorization")
	}
	if err := validateHTTPAmendment(nil, source); err == nil {
		t.Fatal("HTTP amendment created authority for an operation without normalized HTTP facts")
	}
}
