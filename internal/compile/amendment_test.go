package compile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	if _, err := validateHTTPAmendment(nil, source, &narrowed); err != nil {
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
			if _, err := validateHTTPAmendment(nil, source, &amendment); err == nil {
				t.Fatal("HTTP amendment expanded independently normalized source authority")
			}
		})
	}

	required := source.clone()
	required.Authorization.Required = true
	withoutAuthorization := required.clone()
	withoutAuthorization.Authorization = nil
	if _, err := validateHTTPAmendment(nil, &required, &withoutAuthorization); err == nil {
		t.Fatal("HTTP amendment removed source-required authorization")
	}
	if _, err := validateHTTPAmendment(nil, nil, source); err == nil {
		t.Fatal("HTTP amendment created authority for an operation without normalized HTTP facts")
	}
}

// TestHTTPAmendmentCannotCreateChannelAbsentFromDescription pins the boundary a
// credential channel witnessed by a supplemental official capture must not
// weaken: when the description declares no authorization at all, an amendment
// that cites only the description can never create one.
func TestHTTPAmendmentCannotCreateChannelAbsentFromDescription(t *testing.T) {
	source := &HTTPSemantics{
		Origins: []string{"https://api.example.test"},
		Witness: Witness{
			ProofID:    "source-http",
			SourceRefs: []contract.SourceRef{{SourceID: "root", Pointer: "#/servers/0"}},
		},
	}
	amendment := source.clone()
	amendment.Authorization = &contract.OperationAuthorization{
		Required:  true,
		Audience:  "https://api.example.test",
		Placement: contract.FieldLocationHeader,
		WireName:  "Authorization",
		Prefix:    "Bearer ",
		FieldID:   "auth.bearer",
	}
	_, err := validateHTTPAmendment(nil, source, &amendment)
	if !errors.Is(err, ErrUnsafeAmendment) || !strings.Contains(err.Error(), "/http/authorization") {
		t.Fatalf("channel absent from the description error = %v; want ErrUnsafeAmendment at /http/authorization", err)
	}
}

// documentationAmendmentSourceID is the admitted supplemental capture used by
// the documentation-witnessed authorization cases. It is not an OpenAPI
// description: it is retained documentation bytes, addressable only by exact
// byte ranges.
const documentationAmendmentSourceID = "example-docs-rest-authentication"

// documentationAmendmentCapture is the retained documentation capture of the
// synthetic fixture. Its bytes are the only authority the created credential
// channel may cite.
const documentationAmendmentCapture = `<!doctype html>
<html><body>
<h2>About authentication</h2>
<p>After creating a token, you can authenticate your request by sending the token in the Authorization header of your request.</p>
<pre><code>curl --request GET \
--url "https://api.example.test/v3/items" \
--header "Authorization: Bearer YOUR-TOKEN"
</code></pre>
<p>In most cases, you can use Authorization: Bearer or Authorization: token to pass a token.</p>
</body></html>
`

// documentationAmendmentOpenAPI is a minimal OpenAPI 3.0 description that
// declares no security scheme at all: no authorization channel, one fixed
// source header and one JSON record array.
const documentationAmendmentOpenAPI = `{
  "openapi": "3.0.3",
  "info": {"title": "Documentation-witnessed authorization fixture", "version": "1.0.0"},
  "servers": [{"url": "https://api.example.test/v3"}],
  "paths": {
    "/items": {
      "get": {
        "operationId": "listItems",
        "description": "Send the token as Authorization: Bearer <TOKEN>.",
        "parameters": [
          {"name": "page", "in": "query", "schema": {"type": "string"}},
          {
            "name": "X-API-Version",
            "in": "header",
            "required": true,
            "schema": {"type": "string", "enum": ["2024-01-01"]}
          }
        ],
        "responses": {
          "200": {
            "description": "records",
            "content": {
              "application/json": {
                "schema": {
                  "type": "array",
                  "items": {
                    "type": "object",
                    "additionalProperties": false,
                    "required": ["id"],
                    "properties": {"id": {"type": "integer"}}
                  }
                }
              }
            }
          }
        }
      }
    }
  }
}`

func documentationAmendmentWitnessRef(t *testing.T, needle string) contract.SourceRef {
	t.Helper()
	offset := strings.Index(documentationAmendmentCapture, needle)
	if offset < 0 {
		t.Fatalf("fixture documentation capture does not contain %q", needle)
	}
	return contract.SourceRef{
		SourceID: documentationAmendmentSourceID,
		Pointer:  fmt.Sprintf("#bytes:%d-%d", offset, offset+len(needle)),
	}
}

// documentationAmendmentWitness cites the documented statement that the token
// travels in the Authorization header and the documented Bearer form.
func documentationAmendmentWitness(t *testing.T) Witness {
	t.Helper()
	return Witness{
		ProofID: "example-docs-rest-bearer-authorization",
		SourceRefs: []contract.SourceRef{
			documentationAmendmentWitnessRef(t, "sending the token in the Authorization header of your request"),
			documentationAmendmentWitnessRef(t, "Authorization: Bearer YOUR-TOKEN"),
		},
	}
}

// documentationAmendmentHTTPSemantics is the baseline amendment for the fixture
// operation: it narrows the origin to the admitted one, repeats the
// source-required fixed header exactly and creates the credential channel from
// the documentation witness.
func documentationAmendmentHTTPSemantics(t *testing.T) *HTTPSemantics {
	t.Helper()
	return &HTTPSemantics{
		Origins:       []string{"https://api.example.test/v3"},
		Authorization: documentationAmendmentAuthorization(),
		Headers:       []contract.OperationHTTPHeader{{Name: "X-API-Version", Value: "2024-01-01"}},
		Witness:       documentationAmendmentWitness(t),
	}
}

func documentationAmendmentAuthorization() *contract.OperationAuthorization {
	return &contract.OperationAuthorization{
		Required:  false,
		Audience:  "https://api.example.test",
		Placement: contract.FieldLocationHeader,
		WireName:  "Authorization",
		Prefix:    "Bearer ",
		FieldID:   "auth.bearer",
	}
}

// documentationAmendmentSource admits the fixture description plus the
// supplemental documentation capture, which is not an OpenAPI document and is
// never a reference target of the description.
func documentationAmendmentSource(t *testing.T) (SourceLock, Captures) {
	t.Helper()
	operation := testOperation("listItems", "root", "#/paths/~1items/get", nil, []VariantRef{{
		Pointer: "#/paths/~1items/get/responses/200/content/application~1json", Status: "200", MediaType: "application/json",
	}})
	lock, captures := testSourceInput(
		t, DialectOpenAPI30, map[string][]byte{"root": []byte(documentationAmendmentOpenAPI)}, nil, []SourceOperation{operation})
	for index := range lock.SourceSet.Documents {
		lock.SourceSet.Documents[index].Dialect = DialectOpenAPI30
	}
	license := testFixture(t, "LICENSE.txt")
	licenseDigest := sha256.Sum256(license)
	documentation := []byte(documentationAmendmentCapture)
	documentationDigest := sha256.Sum256(documentation)
	lock.SourceSet.Documents = append(lock.SourceSet.Documents, SourceDocument{
		SourceID:    documentationAmendmentSourceID,
		Dialect:     DialectOfficialDocumentation,
		OfficialURL: "https://docs.example.test/authenticating-to-the-rest-api",
		Path:        "authenticating-to-the-rest-api",
		CapturedAt:  "2026-09-19T12:43:59Z",
		ByteLength:  int64(len(documentation)),
		SHA256:      hex.EncodeToString(documentationDigest[:]),
		License: SourceLicense{
			SPDX:           "CC-BY-4.0",
			Path:           "LICENSE.txt",
			SHA256:         hex.EncodeToString(licenseDigest[:]),
			Copyright:      "Copyright (c) Example Documentation Authors",
			NoticeRequired: true,
		},
	})
	captures.Documents[documentationAmendmentSourceID] = documentation
	captures.Licenses[documentationAmendmentSourceID] = license
	return lock, captures
}

func documentationAmendmentSemantics(
	t *testing.T,
	lock SourceLock,
	captured *CapturedSource,
	httpSemantics *HTTPSemantics,
) []byte {
	t.Helper()
	operationRef := []contract.SourceRef{{SourceID: "root", Pointer: "#/paths/~1items/get"}}
	semantics, err := json.Marshal(SemanticsDocument{
		SchemaVersion: SemanticsSchemaVersion,
		ConnectorID:   lock.ConnectorID,
		SourceDigest:  captured.Digest,
		Revision:      lock.SemanticsRevision,
		Operations: []OperationSemantics{{
			OperationID: "listItems",
			SourceRefs:  operationRef,
			HTTP:        httpSemantics,
			RecordExtraction: &RecordExtraction{
				RecordsPointer: "",
				Witness: Witness{ProofID: "fixture-record-array", SourceRefs: []contract.SourceRef{{
					SourceID: "root",
					Pointer:  "#/paths/~1items/get/responses/200/content/application~1json/schema",
				}}},
			},
			Lanes: []LaneSemantics{
				{Lane: contract.LaneDirectRead, Classification: LaneEligible, Reason: "fixture read with a documentation-witnessed credential channel", SourceRefs: operationRef},
				{Lane: contract.LaneDirectWrite, Classification: LaneNotApplicable, Reason: "fixture operation is a read-only GET", SourceRefs: operationRef},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return semantics
}

func documentationAmendmentCompile(
	t *testing.T,
	lock SourceLock,
	captures Captures,
	httpSemantics *HTTPSemantics,
) (CompilationArtifacts, error) {
	t.Helper()
	lockDocument, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := CaptureSource(lock, captures)
	if err != nil {
		t.Fatal(err)
	}
	return Compile(context.Background(), CompilationInput{
		SourceLock:        lockDocument,
		Captures:          captures,
		Semantics:         documentationAmendmentSemantics(t, lock, captured, httpSemantics),
		TargetFoundations: HTTPCompilationTargetFoundations(),
	})
}

// TestDocumentationWitnessedAuthorizationReachesTheBundle proves the admitted
// direction: a credential channel that the pinned OpenAPI description does not
// declare is created from an official-documentation capture, and the compiled
// operation carries the exact channel, selects the credential capability and
// binds the credential as a secret input whose source facts are the cited
// documentation bytes.
func TestDocumentationWitnessedAuthorizationReachesTheBundle(t *testing.T) {
	lock, captures := documentationAmendmentSource(t)
	httpSemantics := documentationAmendmentHTTPSemantics(t)
	artifacts, err := documentationAmendmentCompile(t, lock, captures, httpSemantics)
	if err != nil {
		t.Fatalf("documentation-witnessed authorization was refused: %v", err)
	}
	var bundle Bundle
	if err := json.Unmarshal(artifacts.Bundle, &bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Operations) != 1 {
		t.Fatalf("executable operations = %#v", bundle.Operations)
	}
	operation := bundle.Operations[0]
	if operation.Execution == nil || operation.Execution.HTTP == nil ||
		operation.Execution.HTTP.Authorization == nil ||
		*operation.Execution.HTTP.Authorization != *documentationAmendmentAuthorization() {
		t.Fatalf("compiled authorization = %#v", operation.Execution)
	}
	if !slices.ContainsFunc(operation.CapabilityRequirements, func(requirement contract.CapabilityRequirement) bool {
		return requirement.Capability == "credentials.resolve"
	}) {
		t.Fatalf("compiled capabilities = %#v", operation.CapabilityRequirements)
	}
	var credential contract.FieldBinding
	credentialFound := false
	for _, candidate := range operation.Inputs {
		if candidate.FieldID == "auth.bearer" {
			credential, credentialFound = candidate, true
		}
	}
	if !credentialFound || credential.Sensitivity != contract.SensitivitySecret || credential.Required ||
		credential.Location != contract.FieldLocationHeader || credential.WireName != "Authorization" {
		t.Fatalf("compiled credential binding = %#v", operation.Inputs)
	}
	var report CompileReport
	if err := json.Unmarshal(artifacts.Report, &report); err != nil {
		t.Fatal(err)
	}
	var facts []SourceFact
	for _, normalized := range report.Operations {
		for _, candidate := range normalized.CommonBindings {
			if candidate.FieldID == "auth.bearer" {
				facts = candidate.SourceFacts
			}
		}
	}
	if len(facts) != len(httpSemantics.Witness.SourceRefs) {
		t.Fatalf("credential binding source facts = %#v", facts)
	}
	citedRange, rangeOK := contract.ParseSourceByteRange(facts[0].SourceRef.Pointer)
	if !rangeOK || facts[0].SourceRef.SourceID != documentationAmendmentSourceID {
		t.Fatalf("credential binding fact %#v does not address the documentation capture", facts[0])
	}
	cited := documentationAmendmentCapture[citedRange.Start:citedRange.End]
	if !strings.Contains(cited, "Authorization") {
		t.Fatalf("credential binding fact %#v does not cite the documented statement", facts[0])
	}
	expected := sha256.Sum256([]byte(cited))
	if facts[0].SHA256 != "sha256:"+hex.EncodeToString(expected[:]) {
		t.Fatalf("credential binding fact %#v is not the digest of the cited bytes", facts[0])
	}
}

// TestOpenAPIWitnessedAuthorizationCreationIsRefused pins the boundary the
// documentation mechanism must not weaken: a credential channel may only be
// created when every witness reference resolves inside an admitted
// official-documentation capture. A witness that cites the description, and a
// witness that mixes the description with documentation, stay refused.
func TestOpenAPIWitnessedAuthorizationCreationIsRefused(t *testing.T) {
	for _, test := range []struct {
		name string
		refs func(*testing.T) []contract.SourceRef
	}{
		{
			name: "description witness only",
			refs: func(*testing.T) []contract.SourceRef {
				return []contract.SourceRef{{SourceID: "root", Pointer: "#/servers/0"}}
			},
		},
		{
			name: "description witness mixed with documentation",
			refs: func(t *testing.T) []contract.SourceRef {
				return append(documentationAmendmentWitness(t).SourceRefs,
					contract.SourceRef{SourceID: "root", Pointer: "#/servers/0"})
			},
		},
		{
			// The description is not supplemental documentation: description bytes
			// that happen to contain the channel words still cannot certify a
			// channel the provider documentation does not state.
			name: "description bytes that name the channel",
			refs: func(*testing.T) []contract.SourceRef {
				return []contract.SourceRef{{SourceID: "root", Pointer: "#/paths/~1items/get/description"}}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			lock, captures := documentationAmendmentSource(t)
			httpSemantics := documentationAmendmentHTTPSemantics(t)
			httpSemantics.Witness = Witness{ProofID: "description-only", SourceRefs: test.refs(t)}
			_, err := documentationAmendmentCompile(t, lock, captures, httpSemantics)
			if !errors.Is(err, ErrUnsafeAmendment) {
				t.Fatalf("credential channel creation error = %v; want ErrUnsafeAmendment", err)
			}
		})
	}
}

// TestUnresolvableDocumentationWitnessIsRefused pins that a documentation
// witness is admitted only when it resolves to real retained bytes and names
// the channel it certifies. A range outside the capture, a malformed range, and
// a range that never names the declared header or scheme are all refused.
func TestUnresolvableDocumentationWitnessIsRefused(t *testing.T) {
	for _, test := range []struct {
		name    string
		pointer string
	}{
		{name: "range beyond the capture", pointer: "#bytes:0-999999"},
		{name: "malformed range", pointer: "#bytes:12"},
		{name: "empty range", pointer: "#bytes:7-7"},
	} {
		t.Run(test.name, func(t *testing.T) {
			lock, captures := documentationAmendmentSource(t)
			httpSemantics := documentationAmendmentHTTPSemantics(t)
			httpSemantics.Witness = Witness{ProofID: "unresolvable", SourceRefs: []contract.SourceRef{{
				SourceID: documentationAmendmentSourceID, Pointer: test.pointer,
			}}}
			_, err := documentationAmendmentCompile(t, lock, captures, httpSemantics)
			if !errors.Is(err, ErrUnsafeAmendment) {
				t.Fatalf("unresolvable documentation witness error = %v; want ErrUnsafeAmendment", err)
			}
		})
	}

	t.Run("a resolving range that does not name the channel", func(t *testing.T) {
		lock, captures := documentationAmendmentSource(t)
		httpSemantics := documentationAmendmentHTTPSemantics(t)
		httpSemantics.Witness = Witness{ProofID: "unrelated-prose", SourceRefs: []contract.SourceRef{
			documentationAmendmentWitnessRef(t, "About authentication"),
		}}
		_, err := documentationAmendmentCompile(t, lock, captures, httpSemantics)
		if !errors.Is(err, ErrUnsafeAmendment) || !strings.Contains(err.Error(), "/http/authorization") {
			t.Fatalf("channel-naming error = %v; want ErrUnsafeAmendment at /http/authorization", err)
		}
	})
}

// TestDocumentationWitnessedChannelCannotWidenSourceAuthority pins that the
// documentation witness creates a credential channel and nothing else: a
// widened origin and a rewritten source-declared fixed header stay refused even
// when the authorization witness itself is admissible.
func TestDocumentationWitnessedChannelCannotWidenSourceAuthority(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*HTTPSemantics)
	}{
		{
			name: "origin widening",
			mutate: func(httpSemantics *HTTPSemantics) {
				httpSemantics.Origins = []string{"https://api.example.test"}
			},
		},
		{
			name: "fixed header rewrite",
			mutate: func(httpSemantics *HTTPSemantics) {
				httpSemantics.Headers = []contract.OperationHTTPHeader{{Name: "X-API-Version", Value: "attacker"}}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			lock, captures := documentationAmendmentSource(t)
			httpSemantics := documentationAmendmentHTTPSemantics(t)
			test.mutate(httpSemantics)
			_, err := documentationAmendmentCompile(t, lock, captures, httpSemantics)
			if !errors.Is(err, ErrUnsafeAmendment) {
				t.Fatalf("source-authority expansion error = %v; want ErrUnsafeAmendment", err)
			}
		})
	}

	t.Run("a mandatory created channel is refused", func(t *testing.T) {
		lock, captures := documentationAmendmentSource(t)
		authorization := documentationAmendmentAuthorization()
		authorization.Required = true
		httpSemantics := documentationAmendmentHTTPSemantics(t)
		httpSemantics.Authorization = authorization
		_, err := documentationAmendmentCompile(t, lock, captures, httpSemantics)
		if !errors.Is(err, ErrUnsafeAmendment) || !strings.Contains(err.Error(), "/http/authorization") {
			t.Fatalf("mandatory created channel error = %v; want ErrUnsafeAmendment at /http/authorization", err)
		}
	})

	t.Run("a channel whose cited bytes do not name its wire name is refused", func(t *testing.T) {
		lock, captures := documentationAmendmentSource(t)
		authorization := documentationAmendmentAuthorization()
		authorization.WireName = "X-Injected-Token"
		httpSemantics := documentationAmendmentHTTPSemantics(t)
		httpSemantics.Authorization = authorization
		_, err := documentationAmendmentCompile(t, lock, captures, httpSemantics)
		if !errors.Is(err, ErrUnsafeAmendment) || !strings.Contains(err.Error(), "/http/authorization") {
			t.Fatalf("invented wire name error = %v; want ErrUnsafeAmendment at /http/authorization", err)
		}
	})
}

// TestCreatedCredentialBindingIsAbsentFromPublicSurfaces pins that a created
// credential channel is never a public command field, catalog entry or CLI
// flag, while the operation itself keeps the secret binding and the capability
// that resolves it.
func TestCreatedCredentialBindingIsAbsentFromPublicSurfaces(t *testing.T) {
	lock, captures := documentationAmendmentSource(t)
	httpSemantics := documentationAmendmentHTTPSemantics(t)
	artifacts, err := documentationAmendmentCompile(t, lock, captures, httpSemantics)
	if err != nil {
		t.Fatalf("documentation-witnessed authorization was refused: %v", err)
	}
	var bundle Bundle
	if err := json.Unmarshal(artifacts.Bundle, &bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Commands) != 1 || bundle.Commands[0].OperationID != "listItems" {
		t.Fatalf("public commands = %#v", bundle.Commands)
	}
	command := bundle.Commands[0]
	if len(command.Fields) == 0 {
		t.Fatalf("public command lost its ordinary fields: %#v", command)
	}
	for _, field := range command.Fields {
		if field.FieldID == "auth.bearer" || field.Sensitive ||
			strings.Contains(strings.ToLower(field.Flag), "authorization") {
			t.Fatalf("public command exposed the credential channel: %#v", field)
		}
	}
	if bytes.Contains(artifacts.CommandManifest, []byte("auth.bearer")) ||
		bytes.Contains(artifacts.CommandManifest, []byte("Authorization")) {
		t.Fatalf("public command manifest exposed the credential channel: %s", artifacts.CommandManifest)
	}
	var operation contract.Operation
	for _, candidate := range bundle.Operations {
		if candidate.OperationID == "listItems" {
			operation = candidate
		}
	}
	if !slices.ContainsFunc(operation.Inputs, func(binding contract.FieldBinding) bool {
		return binding.FieldID == "auth.bearer" && binding.Sensitivity == contract.SensitivitySecret
	}) {
		t.Fatalf("compiled operation dropped the credential binding: %#v", operation.Inputs)
	}
}
