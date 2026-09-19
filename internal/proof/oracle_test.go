// This file owns the issue #27 (pm-github-direct-slice) V32 first-owned test
// entrypoint TestFalseSuccessAndIndependentCases: independent oracle
// expectations for the GitHub direct-read slice, proof-case selection and
// receipt retention, and the seven-lane inventory refusal rules. Reusable
// runner mechanics (step execution, cleanup arming, report publication) remain
// owned by issue #26 (pm-connector-proof-runner) and are not exercised here.
package proof

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const (
	v32ConnectorID = "github"
	v32OperationID = "issues/list-for-repo"
	v32Repository  = "acme/widgets"

	v32ExactCaseID = "github-read-exact"
	v32MixedCaseID = "github-read-mixed"

	v32ScopeLabel       = "workspace-alpha-account-controlled"
	v32SourceLockLabel  = "github-source-lock"
	v32SemanticsLabel   = "github-semantics"
	v32BundleLabel      = "bundle-github-direct-read"
	v32SchemaLabel      = "schema-github-direct-read"
	v32ClientLabel      = "pm-client-thin-github"
	v32APIQueryLabel    = "pm-api-direct-read"
	v32WorkerLabel      = "pm-worker-http-transport"
	v32ClosureLabel     = "implementation-closure-github-direct-read"
	v32AssertionSetName = "assertion-set-github-direct-read"

	v32ForeignContinuation = "https://evil.example.test/repos/acme/widgets/issues?page=2"
)

// v32Issue9001JSON is the independently seeded complete issue record for the
// exact repository read: provider identity 9001/17 in acme/widgets with nested
// repository data, explicit null body, absent optional milestone, false, zero
// and empty values all preserved.
const v32Issue9001JSON = `{
"presence":"value","type":"object","fields":{
  "id":{"presence":"value","type":"integer","exact":"9001","precision":4,"scale":0},
  "number":{"presence":"value","type":"integer","exact":"17","precision":2,"scale":0},
  "title":{"presence":"value","type":"string","text":"Widget list fails on an empty page"},
  "state":{"presence":"value","type":"string","text":"open"},
  "locked":{"presence":"value","type":"bool","boolean":false},
  "comments":{"presence":"value","type":"integer","exact":"0","precision":1,"scale":0},
  "body":{"presence":"null"},
  "milestone":{"presence":"absent"},
  "assignees":{"presence":"value","type":"array","elements":[]},
  "repository":{"presence":"value","type":"object","fields":{
    "full_name":{"presence":"value","type":"string","text":"acme/widgets"},
    "name":{"presence":"value","type":"string","text":"widgets"},
    "owner":{"presence":"value","type":"object","fields":{
      "login":{"presence":"value","type":"string","text":"acme"}}}}},
  "labels":{"presence":"value","type":"array","elements":[{"presence":"value","type":"string","text":"bug"},{"presence":"value","type":"string","text":"regression"}]},
  "pull_request":{"presence":"absent"}
}}`

// v32Issue9004JSON is the second independently seeded issue record of the first
// page: identity 9004/20 in acme/widgets.
const v32Issue9004JSON = `{
"presence":"value","type":"object","fields":{
  "id":{"presence":"value","type":"integer","exact":"9004","precision":4,"scale":0},
  "number":{"presence":"value","type":"integer","exact":"20","precision":2,"scale":0},
  "title":{"presence":"value","type":"string","text":"Pagination skips the last record"},
  "state":{"presence":"value","type":"string","text":"open"},
  "repository":{"presence":"value","type":"object","fields":{
    "full_name":{"presence":"value","type":"string","text":"acme/widgets"}}},
  "labels":{"presence":"value","type":"array","elements":[{"presence":"value","type":"string","text":"bug"}]},
  "pull_request":{"presence":"absent"}
}}`

// v32Issue9005JSON is the independently seeded issue record of the second page:
// identity 9005/21 in acme/widgets.
const v32Issue9005JSON = `{
"presence":"value","type":"object","fields":{
  "id":{"presence":"value","type":"integer","exact":"9005","precision":4,"scale":0},
  "number":{"presence":"value","type":"integer","exact":"21","precision":2,"scale":0},
  "title":{"presence":"value","type":"string","text":"Continuation duplicates a page"},
  "state":{"presence":"value","type":"string","text":"open"},
  "repository":{"presence":"value","type":"object","fields":{
    "full_name":{"presence":"value","type":"string","text":"acme/widgets"}}},
  "pull_request":{"presence":"absent"}
}}`

// v32PullRequest9002JSON is the independently seeded pull-request-shaped entry:
// identity 9002/18 in acme/widgets carrying the pull_request discriminator with
// exact nested values, an explicit null, and false.
const v32PullRequest9002JSON = `{
"presence":"value","type":"object","fields":{
  "id":{"presence":"value","type":"integer","exact":"9002","precision":4,"scale":0},
  "number":{"presence":"value","type":"integer","exact":"18","precision":2,"scale":0},
  "title":{"presence":"value","type":"string","text":"Add widgets to the dashboard"},
  "state":{"presence":"value","type":"string","text":"open"},
  "repository":{"presence":"value","type":"object","fields":{
    "full_name":{"presence":"value","type":"string","text":"acme/widgets"}}},
  "labels":{"presence":"value","type":"array","elements":[]},
  "body":{"presence":"null"},
  "pull_request":{"presence":"value","type":"object","fields":{
    "url":{"presence":"value","type":"string","text":"https://api.github.com/repos/acme/widgets/pulls/18"},
    "merged_at":{"presence":"null"},
    "draft":{"presence":"value","type":"bool","boolean":false}}}
}}`

// v32ExactReadPageJSON is the seeded single-page result for the exact
// repository read: one complete issue record, no continuation, exhausted source.
const v32ExactReadPageJSON = `{
  "media_type": "application/json",
  "data": {"presence": "value", "type": "array", "elements": [` + v32Issue9001JSON + `]},
  "pagination": {
    "effective_wire_limit": 100,
    "continuation": {"presence": "absent"},
    "unread_tail": [],
    "fetched_count": 1,
    "returned_count": 1,
    "emitted_count": 1,
    "provider_total": {"exact": "1", "precision": 1, "scale": 0},
    "source_exhausted": true
  }
}`

// v32ForeignRecordPageJSON is a successful HTTP 200 page with a plausible record
// shape, truthful counts, and a different record/repository identity. It must
// never satisfy an independently authored identity expectation.
const v32ForeignRecordPageJSON = `{
  "media_type": "application/json",
  "data": {"presence": "value", "type": "array", "elements": [{
    "presence": "value", "type": "object", "fields": {
      "id":{"presence":"value","type":"integer","exact":"9100","precision":4,"scale":0},
      "number":{"presence":"value","type":"integer","exact":"42","precision":2,"scale":0},
      "title":{"presence":"value","type":"string","text":"Unrelated repository issue"},
      "repository":{"presence":"value","type":"object","fields":{
        "full_name":{"presence":"value","type":"string","text":"acme/gadgets"}}},
      "pull_request":{"presence":"absent"}
    }}]},
  "pagination": {
    "effective_wire_limit": 100,
    "continuation": {"presence": "absent"},
    "unread_tail": [],
    "fetched_count": 1,
    "returned_count": 1,
    "emitted_count": 1,
    "provider_total": {"exact": "1", "precision": 1, "scale": 0},
    "source_exhausted": true
  }
}`

// v32FirstPageJSON and v32SecondPageJSON are the seeded two-page source. The
// continuation is the admitted same-origin cursor; the second page is terminal
// with independently seeded identity 9005/21.
const v32FirstPageJSON = `{
  "media_type": "application/json",
  "data": {"presence": "value", "type": "array", "elements": [` + v32Issue9001JSON + `,` + v32Issue9004JSON + `]},
  "pagination": {
    "effective_wire_limit": 100,
    "continuation": {"presence": "value", "type": "string", "text": "issues-page-2"},
    "unread_tail": [],
    "fetched_count": 2,
    "returned_count": 2,
    "emitted_count": 2,
    "provider_total": {"exact": "3", "precision": 1, "scale": 0},
    "source_exhausted": false
  }
}`

const v32SecondPageJSON = `{
  "media_type": "application/json",
  "data": {"presence": "value", "type": "array", "elements": [` + v32Issue9005JSON + `]},
  "pagination": {
    "effective_wire_limit": 100,
    "continuation": {"presence": "absent"},
    "unread_tail": [],
    "fetched_count": 1,
    "returned_count": 1,
    "emitted_count": 1,
    "provider_total": {"exact": "3", "precision": 1, "scale": 0},
    "source_exhausted": true
  }
}`

// v32ClampedPageJSON is a page whose wire page size was clamped to three by the
// controlled provider although the caller asked for 100; only the actually
// effective wire limit may be reported.
const v32ClampedPageJSON = `{
  "media_type": "application/json",
  "data": {"presence": "value", "type": "array", "elements": [` + v32Issue9001JSON + `,` + v32Issue9004JSON + `,` + v32Issue9005JSON + `]},
  "pagination": {
    "effective_wire_limit": 3,
    "continuation": {"presence": "value", "type": "string", "text": "issues-page-3"},
    "unread_tail": [],
    "fetched_count": 3,
    "returned_count": 3,
    "emitted_count": 3,
    "provider_total": {"exact": "5", "precision": 1, "scale": 0},
    "source_exhausted": false
  }
}`

// v32LocalCapPageJSON fetched three records but a caller total record cap of two
// truncates inside the page: two records are emitted and the unread tail keeps
// the third record instead of dropping it or claiming exhaustion.
const v32LocalCapPageJSON = `{
  "media_type": "application/json",
  "data": {"presence": "value", "type": "array", "elements": [` + v32Issue9001JSON + `,` + v32Issue9004JSON + `]},
  "pagination": {
    "effective_wire_limit": 3,
    "total_record_cap": 2,
    "continuation": {"presence": "value", "type": "string", "text": "issues-page-3"},
    "unread_tail": [` + v32Issue9005JSON + `],
    "fetched_count": 3,
    "returned_count": 3,
    "emitted_count": 2,
    "provider_total": {"exact": "5", "precision": 1, "scale": 0},
    "source_exhausted": false
  }
}`

// v32EmptyFilteredPageJSON is a zero-match filtered page: no record is emitted,
// yet the source continuation, wire limit and unread tail remain truthful and
// the empty page is not reported as exhaustion.
const v32EmptyFilteredPageJSON = `{
  "media_type": "application/json",
  "data": {"presence": "value", "type": "array", "elements": []},
  "pagination": {
    "effective_wire_limit": 100,
    "total_record_cap": 2,
    "continuation": {"presence": "value", "type": "string", "text": "issues-page-3"},
    "unread_tail": [` + v32Issue9005JSON + `],
    "fetched_count": 3,
    "returned_count": 3,
    "emitted_count": 0,
    "provider_total": {"exact": "5", "precision": 1, "scale": 0},
    "source_exhausted": false
  }
}`

// v32MixedPageJSON preserves both returned entries: the issue entry keeps its
// complete data and an absent pull_request discriminator, and the
// pull-request-shaped entry keeps its exact discriminator and nested values.
const v32MixedPageJSON = `{
  "media_type": "application/json",
  "data": {"presence": "value", "type": "array", "elements": [` + v32Issue9001JSON + `,` + v32PullRequest9002JSON + `]},
  "pagination": {
    "effective_wire_limit": 100,
    "continuation": {"presence": "absent"},
    "unread_tail": [],
    "fetched_count": 2,
    "returned_count": 2,
    "emitted_count": 2,
    "provider_total": {"exact": "2", "precision": 1, "scale": 0},
    "source_exhausted": true
  }
}`

// v32HiddenExclusionPageJSON is the forbidden hidden-entry behavior: the source
// returned two entries, but the published result silently drops the
// pull-request-shaped entry while the source counts stay truthful.
const v32HiddenExclusionPageJSON = `{
  "media_type": "application/json",
  "data": {"presence": "value", "type": "array", "elements": [` + v32Issue9001JSON + `]},
  "pagination": {
    "effective_wire_limit": 100,
    "continuation": {"presence": "absent"},
    "unread_tail": [],
    "fetched_count": 2,
    "returned_count": 2,
    "emitted_count": 1,
    "provider_total": {"exact": "2", "precision": 1, "scale": 0},
    "source_exhausted": true
  }
}`

// v32MalformedContinuationRefusalJSON and v32ForeignContinuationPageJSON model
// the refusal of a malformed next-page token and the forbidden result of
// following a foreign-host continuation instead of refusing it.
const v32MalformedContinuationRefusalJSON = `{
  "media_type": "application/json",
  "data": {"presence": "absent"},
  "rejected_continuation": {"presence": "value", "type": "string", "text": "%%malformed-cursor%%"},
  "effect": {"state": "unsent", "receipt_refs": [], "owned_object_refs": []},
  "error": {"code": "invalid_continuation"}
}`

const v32ForeignContinuationPageJSON = `{
  "media_type": "application/json",
  "data": {"presence": "value", "type": "array", "elements": [` + v32Issue9005JSON + `]},
  "pagination": {
    "effective_wire_limit": 100,
    "continuation": {"presence": "value", "type": "string", "text": "` + v32ForeignContinuation + `"},
    "unread_tail": [],
    "fetched_count": 1,
    "returned_count": 1,
    "emitted_count": 1,
    "source_exhausted": false
  },
  "effect": {"state": "succeeded", "receipt_refs": ["provider-receipt-foreign-host"], "owned_object_refs": []}
}`

// v32RedirectedPrivateRefusalJSON records the admitted refusal of a redirected
// private destination, and v32RedirectedPrivatePageJSON the forbidden result of
// having followed it with a real provider send.
const v32RedirectedPrivateRefusalJSON = `{
  "media_type": "application/json",
  "data": {"presence": "absent"},
  "rejected_destination": {"presence": "value", "type": "string", "text": "http://10.0.0.7/internal/issues"},
  "effect": {"state": "rejected", "receipt_refs": [], "owned_object_refs": []},
  "error": {"code": "destination_not_admitted"}
}`

const v32RedirectedPrivatePageJSON = `{
  "media_type": "application/json",
  "data": {"presence": "value", "type": "array", "elements": [` + v32Issue9005JSON + `]},
  "pagination": {
    "effective_wire_limit": 100,
    "continuation": {"presence": "absent"},
    "unread_tail": [],
    "fetched_count": 1,
    "returned_count": 1,
    "emitted_count": 1,
    "source_exhausted": true
  },
  "effect": {"state": "succeeded", "receipt_refs": ["provider-receipt-private-destination"], "owned_object_refs": []}
}`

// v32DeniedConnectionRefusalJSON and v32RevokedConnectionRefusalJSON are the
// published refusal observations for a denied connection in the wrong workspace
// and a revoked connection: no result document, and no provider send receipt.
const v32DeniedConnectionRefusalJSON = `{
  "media_type": "application/json",
  "data": {"presence": "absent"},
  "workspace": {"presence": "value", "type": "string", "text": "workspace-beta"},
  "effect": {"state": "rejected", "receipt_refs": [], "owned_object_refs": []},
  "error": {"code": "connection_denied"}
}`

const v32RevokedConnectionRefusalJSON = `{
  "media_type": "application/json",
  "data": {"presence": "absent"},
  "workspace": {"presence": "value", "type": "string", "text": "workspace-alpha"},
  "effect": {"state": "unsent", "receipt_refs": [], "owned_object_refs": []},
  "error": {"code": "connection_revoked"}
}`

// v32CredentialPresenceJSON is the forbidden credential-presence-only
// observation: the server-configured connection is present, but no source
// record was ever read.
const v32CredentialPresenceJSON = `{
  "media_type": "application/json",
  "data": {"presence": "value", "type": "object", "fields": {
    "credential_present": {"presence": "value", "type": "bool", "boolean": true},
    "authentication": {"presence": "value", "type": "string", "text": "server-configured-connection"}}},
  "pagination": {
    "effective_wire_limit": 100,
    "continuation": {"presence": "absent"},
    "unread_tail": [],
    "fetched_count": 0,
    "returned_count": 0,
    "emitted_count": 0,
    "source_exhausted": false
  }
}`

// v32BundleJSON is the seeded GitHub direct-read bundle: one executable
// issues/list-for-repo operation that is source-eligible for direct_read only,
// with every other lane explicitly unsupported, unproven, not applicable or
// blocked, plus its safe command projection.
const v32BundleJSON = `{
  "schema_version": "pm.bundle/v1",
  "connector_id": "github",
  "source_digest": "sha256:1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f809",
  "semantics_revision": "sha256:f0e1d2c3b4a5968778695a4b3c2d1e0ff0e1d2c3b4a5968778695a4b3c2d1e0f",
  "operations": [{
    "schema_version": "pm.operation/v1",
    "operation_id": "issues/list-for-repo",
    "source_refs": [{"source_id": "GITHUB-REST-SPEC-ACCEPTED", "pointer": "#/paths/~1repos~1{owner}~1{repo}~1issues/get"}],
    "transport": {"kind": "http", "method": "GET", "path": "/repos/{owner}/{repo}/issues"},
    "inputs": [
      {"field_id": "owner", "wire_name": "owner", "location": "path", "json_pointer": "/path/owner", "required": true, "nullable": false, "style": "simple", "explode": false, "allow_reserved": false, "sensitivity": "public", "schema": {"type": "string"}},
      {"field_id": "repo", "wire_name": "repo", "location": "path", "json_pointer": "/path/repo", "required": true, "nullable": false, "style": "simple", "explode": false, "allow_reserved": false, "sensitivity": "public", "schema": {"type": "string"}},
      {"field_id": "per-page", "wire_name": "per_page", "location": "query", "json_pointer": "/query/per_page", "required": false, "nullable": false, "style": "form", "explode": true, "allow_reserved": false, "sensitivity": "public", "schema": {"type": "integer"}}
    ],
    "request_variants": [],
    "responses": [{
      "status": "200",
      "no_content": false,
      "media_type": "application/json",
      "schema": {"type": "array", "items": {"type": "object", "required": ["id", "number", "title", "labels"], "properties": {
        "id": {"type": "integer"},
        "number": {"type": "integer"},
        "title": {"type": "string"},
        "labels": {"type": "array", "items": {"type": "string"}},
        "pull_request": {"type": "object", "nullable": true}}}},
      "source_refs": [{"source_id": "GITHUB-REST-SPEC-ACCEPTED", "pointer": "#/paths/~1repos~1{owner}~1{repo}~1issues/get/responses/200"}]
    }],
    "capability_requirements": [
      {"capability": "http.execute", "foundation": {"id": "FND-06", "version": "1.0.0"}}
    ],
    "lane_eligibility": [
      {"lane": "direct_read", "status": "eligible", "reason": "source-declared-repository-issue-read"},
      {"lane": "direct_write", "status": "unsupported", "reason": "source-does-not-declare-write"},
      {"lane": "etl", "status": "unproven", "reason": "runtime-proof-not-yet-established"},
      {"lane": "reverse_etl", "status": "not_applicable", "reason": "read-operation-has-no-destination-write"},
      {"lane": "binary_download", "status": "not_applicable", "reason": "response-is-not-binary"},
      {"lane": "binary_upload", "status": "not_applicable", "reason": "request-is-not-binary"},
      {"lane": "sync_transport", "status": "blocked", "reason": "sync-semantics-are-not-declared"}
    ]
  }],
  "commands": [{
    "name": "issues-list-for-repo",
    "operation_id": "issues/list-for-repo",
    "fields": [
      {"flag": "--owner", "field_id": "owner", "location": "path", "required": true, "sensitive": false},
      {"flag": "--repo", "field_id": "repo", "location": "path", "required": true, "sensitive": false},
      {"flag": "--per-page", "field_id": "per-page", "location": "query", "required": false, "sensitive": false}
    ]
  }],
  "unavailable_operations": []
}`

func TestFalseSuccessAndIndependentCases(t *testing.T) {
	t.Run("github_public_read", v32GitHubPublicRead)
	t.Run("github_pages", v32GitHubPages)
	t.Run("github_invalid_next", v32GitHubInvalidNext)
	t.Run("github_scope", v32GitHubScope)
	t.Run("github_wrong_record", v32GitHubWrongRecord)
	t.Run("github_mixed_entries", v32GitHubMixedEntries)
	t.Run("independent_cases", v32IndependentCases)
	t.Run("lane_inventory", v32LaneInventory)
}

// v32GitHubPublicRead covers pm-github-direct-slice-S01/S05-C03: the authored
// id/number/repository identities and the complete nested record contract pass
// on the seeded page, while an HTTP 200 page from another repository fails even
// though its media type and counts are truthful.
func v32GitHubPublicRead(t *testing.T) {
	observation := OracleObservation{Document: json.RawMessage(v32ExactReadPageJSON)}
	v32RequireAllPassed(t, v32Evaluate(t, v32ExactReadAssertions(), observation))

	foreign := OracleObservation{Document: json.RawMessage(v32ForeignRecordPageJSON)}
	outcomes := v32Evaluate(t, v32ExactReadAssertions(), foreign)
	v32RequireFailed(t, outcomes, "record-id", "record-number", "record-repository", "record-row")
	v32RequirePassed(t, outcomes, "media-type", "page-counts", "page-terminal")
}

// v32ExactReadAssertions is the independently authored expectation set for one
// exact acme/widgets issue read: identity, nested data, presence distinctions
// and truthful single-page pagination.
func v32ExactReadAssertions() []contract.ScenarioAssertion {
	const step = "read"
	return []contract.ScenarioAssertion{
		v32Assertion("media-type", step, "equals", "/media_type", `"application/json"`),
		v32Assertion("record-row", step, "row", "/data/elements/0", v32Issue9001JSON),
		v32Assertion("record-id", step, "equals", "/data/elements/0/fields/id",
			`{"presence":"value","type":"integer","exact":"9001","precision":4,"scale":0}`),
		v32Assertion("record-number", step, "equals", "/data/elements/0/fields/number",
			`{"presence":"value","type":"integer","exact":"17","precision":2,"scale":0}`),
		v32Assertion("record-repository", step, "equals", "/data/elements/0/fields/repository/fields/full_name",
			`{"presence":"value","type":"string","text":"acme/widgets"}`),
		v32Assertion("record-owner", step, "contains", "/data/elements/0/fields/repository",
			`{"presence":"value","type":"object","fields":{"owner":{"presence":"value","type":"object","fields":{"login":{"presence":"value","type":"string","text":"acme"}}}}}`),
		v32Assertion("record-labels", step, "equals", "/data/elements/0/fields/labels/elements/1",
			`{"presence":"value","type":"string","text":"regression"}`),
		v32Assertion("record-pull-request-absent", step, "equals", "/data/elements/0/fields/pull_request", `{"presence":"absent"}`),
		v32Assertion("record-body-null", step, "equals", "/data/elements/0/fields/body", `{"presence":"null"}`),
		v32Assertion("record-milestone-absent", step, "equals", "/data/elements/0/fields/milestone", `{"presence":"absent"}`),
		v32Assertion("record-locked-false", step, "equals", "/data/elements/0/fields/locked", `{"presence":"value","type":"bool","boolean":false}`),
		v32Assertion("record-comments-zero", step, "equals", "/data/elements/0/fields/comments", `{"presence":"value","type":"integer","exact":"0","precision":1,"scale":0}`),
		v32Assertion("record-assignees-empty", step, "equals", "/data/elements/0/fields/assignees", `{"presence":"value","type":"array","elements":[]}`),
		v32Assertion("page-wire-limit", step, "equals", "/pagination/effective_wire_limit", `100`),
		v32Assertion("page-fetched", step, "equals", "/pagination/fetched_count", `1`),
		v32Assertion("page-returned", step, "equals", "/pagination/returned_count", `1`),
		v32Assertion("page-counts", step, "equals", "/pagination/emitted_count", `1`),
		v32Assertion("page-unread-tail", step, "equals", "/pagination/unread_tail", `[]`),
		v32Assertion("page-terminal", step, "equals", "/pagination/source_exhausted", `true`),
		v32Assertion("page-continuation-absent", step, "equals", "/pagination/continuation", `{"presence":"absent"}`),
		v32Assertion("page-provider-total", step, "equals", "/pagination/provider_total", `{"exact":"1","precision":1,"scale":0}`),
	}
}

// v32GitHubPages covers pm-github-direct-slice-S02 and S06-C07: exact
// continuation and page counts across two pages, an honestly reported
// provider-clamped wire size, a local cap that retains its unread tail, and a
// zero-match filtered page that is not source exhaustion.
func v32GitHubPages(t *testing.T) {
	t.Run("next_page", func(t *testing.T) {
		first := v32Evaluate(t, []contract.ScenarioAssertion{
			v32Assertion("first-id-0", "read", "equals", "/data/elements/0/fields/id",
				`{"presence":"value","type":"integer","exact":"9001","precision":4,"scale":0}`),
			v32Assertion("first-id-1", "read", "equals", "/data/elements/1/fields/id",
				`{"presence":"value","type":"integer","exact":"9004","precision":4,"scale":0}`),
			v32Assertion("first-number-1", "read", "equals", "/data/elements/1/fields/number",
				`{"presence":"value","type":"integer","exact":"20","precision":2,"scale":0}`),
			v32Assertion("first-no-third", "read", "absent", "/data/elements/2", `null`),
			v32Assertion("first-continuation", "read", "equals", "/pagination/continuation",
				`{"presence":"value","type":"string","text":"issues-page-2"}`),
			v32Assertion("first-wire-limit", "read", "equals", "/pagination/effective_wire_limit", `100`),
			v32Assertion("first-fetched", "read", "equals", "/pagination/fetched_count", `2`),
			v32Assertion("first-returned", "read", "equals", "/pagination/returned_count", `2`),
			v32Assertion("first-emitted", "read", "equals", "/pagination/emitted_count", `2`),
			v32Assertion("first-not-exhausted", "read", "equals", "/pagination/source_exhausted", `false`),
			v32Assertion("first-unread-tail", "read", "equals", "/pagination/unread_tail", `[]`),
		}, OracleObservation{Document: json.RawMessage(v32FirstPageJSON)})
		v32RequireAllPassed(t, first)

		second := v32Evaluate(t, []contract.ScenarioAssertion{
			v32Assertion("second-id", "read", "equals", "/data/elements/0/fields/id",
				`{"presence":"value","type":"integer","exact":"9005","precision":4,"scale":0}`),
			v32Assertion("second-number", "read", "equals", "/data/elements/0/fields/number",
				`{"presence":"value","type":"integer","exact":"21","precision":2,"scale":0}`),
			v32Assertion("second-terminal", "read", "equals", "/pagination/source_exhausted", `true`),
			v32Assertion("second-continuation-absent", "read", "equals", "/pagination/continuation", `{"presence":"absent"}`),
			v32Assertion("second-wire-limit", "read", "equals", "/pagination/effective_wire_limit", `100`),
			v32Assertion("second-fetched", "read", "equals", "/pagination/fetched_count", `1`),
			v32Assertion("second-returned", "read", "equals", "/pagination/returned_count", `1`),
			v32Assertion("second-emitted", "read", "equals", "/pagination/emitted_count", `1`),
			v32Assertion("second-unread-tail", "read", "equals", "/pagination/unread_tail", `[]`),
		}, OracleObservation{Document: json.RawMessage(v32SecondPageJSON)})
		v32RequireAllPassed(t, second)
	})

	t.Run("provider_clamp", func(t *testing.T) {
		outcomes := v32Evaluate(t, []contract.ScenarioAssertion{
			v32Assertion("clamped-wire-limit", "read", "equals", "/pagination/effective_wire_limit", `3`),
			v32Assertion("requested-wire-limit-not-reported", "read", "equals", "/pagination/effective_wire_limit", `100`),
			v32Assertion("clamped-fetched", "read", "equals", "/pagination/fetched_count", `3`),
			v32Assertion("clamped-returned", "read", "equals", "/pagination/returned_count", `3`),
			v32Assertion("clamped-emitted", "read", "equals", "/pagination/emitted_count", `3`),
			v32Assertion("clamped-continuation", "read", "equals", "/pagination/continuation",
				`{"presence":"value","type":"string","text":"issues-page-3"}`),
			v32Assertion("clamped-not-exhausted", "read", "equals", "/pagination/source_exhausted", `false`),
			v32Assertion("clamped-ids", "read", "equals", "/data/elements/2/fields/id",
				`{"presence":"value","type":"integer","exact":"9005","precision":4,"scale":0}`),
		}, OracleObservation{Document: json.RawMessage(v32ClampedPageJSON)})
		v32RequirePassed(t, outcomes,
			"clamped-wire-limit", "clamped-fetched", "clamped-returned", "clamped-emitted", "clamped-continuation", "clamped-not-exhausted", "clamped-ids")
		v32RequireFailed(t, outcomes, "requested-wire-limit-not-reported")
	})

	t.Run("local_cap", func(t *testing.T) {
		outcomes := v32Evaluate(t, []contract.ScenarioAssertion{
			v32Assertion("cap-total", "read", "equals", "/pagination/total_record_cap", `2`),
			v32Assertion("cap-fetched", "read", "equals", "/pagination/fetched_count", `3`),
			v32Assertion("cap-returned", "read", "equals", "/pagination/returned_count", `3`),
			v32Assertion("cap-emitted", "read", "equals", "/pagination/emitted_count", `2`),
			v32Assertion("cap-continuation", "read", "equals", "/pagination/continuation",
				`{"presence":"value","type":"string","text":"issues-page-3"}`),
			v32Assertion("cap-not-exhausted", "read", "equals", "/pagination/source_exhausted", `false`),
			v32Assertion("cap-emitted-id-1", "read", "equals", "/data/elements/1/fields/id",
				`{"presence":"value","type":"integer","exact":"9004","precision":4,"scale":0}`),
			v32Assertion("cap-no-third-emitted", "read", "absent", "/data/elements/2", `null`),
			v32Assertion("cap-tail-id", "read", "equals", "/pagination/unread_tail/0/fields/id",
				`{"presence":"value","type":"integer","exact":"9005","precision":4,"scale":0}`),
			v32Assertion("cap-tail-only-one", "read", "absent", "/pagination/unread_tail/1", `null`),
		}, OracleObservation{Document: json.RawMessage(v32LocalCapPageJSON)})
		v32RequireAllPassed(t, outcomes)
	})

	t.Run("empty_filtered_page_is_not_exhaustion", func(t *testing.T) {
		outcomes := v32Evaluate(t, []contract.ScenarioAssertion{
			v32Assertion("filtered-emitted-zero", "read", "equals", "/pagination/emitted_count", `0`),
			v32Assertion("filtered-fetched", "read", "equals", "/pagination/fetched_count", `3`),
			v32Assertion("filtered-returned", "read", "equals", "/pagination/returned_count", `3`),
			v32Assertion("filtered-no-records", "read", "equals", "/data/elements", `[]`),
			v32Assertion("filtered-no-first-record", "read", "absent", "/data/elements/0", `null`),
			v32Assertion("filtered-wire-limit", "read", "equals", "/pagination/effective_wire_limit", `100`),
			v32Assertion("filtered-continuation", "read", "equals", "/pagination/continuation",
				`{"presence":"value","type":"string","text":"issues-page-3"}`),
			v32Assertion("filtered-not-exhausted", "read", "equals", "/pagination/source_exhausted", `false`),
			v32Assertion("filtered-fabricated-exhaustion", "read", "equals", "/pagination/source_exhausted", `true`),
			v32Assertion("filtered-tail-id", "read", "equals", "/pagination/unread_tail/0/fields/id",
				`{"presence":"value","type":"integer","exact":"9005","precision":4,"scale":0}`),
		}, OracleObservation{Document: json.RawMessage(v32EmptyFilteredPageJSON)})
		v32RequirePassed(t, outcomes,
			"filtered-emitted-zero", "filtered-fetched", "filtered-returned", "filtered-no-records",
			"filtered-no-first-record", "filtered-wire-limit", "filtered-continuation",
			"filtered-not-exhausted", "filtered-tail-id")
		v32RequireFailed(t, outcomes, "filtered-fabricated-exhaustion")
	})
}

// v32GitHubInvalidNext covers pm-github-direct-slice-S03: a malformed,
// foreign-host or redirected/private continuation must be refused before
// another send, and the oracle must never accept a document that carries the
// unrefused continuation or a provider send receipt for it.
func v32GitHubInvalidNext(t *testing.T) {
	t.Run("malformed_token_refused", func(t *testing.T) {
		outcomes := v32Evaluate(t, []contract.ScenarioAssertion{
			v32Assertion("refusal-unsent", "read", "effect", "/effect/state", `"unsent"`),
			v32Assertion("refusal-no-send-receipt", "read", "equals", "/effect/receipt_refs", `[]`),
			v32Assertion("refusal-no-owned-objects", "read", "equals", "/effect/owned_object_refs", `[]`),
			v32Assertion("refusal-no-records", "read", "absent", "/data/elements/0", `null`),
			v32Assertion("refusal-no-continuation", "read", "absent", "/pagination/continuation", `null`),
		}, OracleObservation{Document: json.RawMessage(v32MalformedContinuationRefusalJSON)})
		v32RequireAllPassed(t, outcomes)
	})

	t.Run("foreign_host_refused_before_send", func(t *testing.T) {
		followed := OracleObservation{Document: json.RawMessage(v32ForeignContinuationPageJSON)}
		outcomes := v32Evaluate(t, []contract.ScenarioAssertion{
			v32Assertion("foreign-unsent", "read", "effect", "/effect/state", `"unsent"`),
			v32Assertion("foreign-no-send-receipt", "read", "equals", "/effect/receipt_refs", `[]`),
			v32Assertion("foreign-no-continuation", "read", "equals", "/pagination/continuation", `{"presence":"absent"}`),
			v32Assertion("foreign-not-exhausted", "read", "equals", "/pagination/source_exhausted", `false`),
			v32Assertion("foreign-admitted-continuation", "read", "equals", "/pagination/continuation",
				`{"presence":"value","type":"string","text":"issues-page-3"}`),
		}, followed)
		v32RequireFailed(t, outcomes,
			"foreign-unsent", "foreign-no-send-receipt", "foreign-no-continuation", "foreign-admitted-continuation")
		v32RequirePassed(t, outcomes, "foreign-not-exhausted")
	})

	t.Run("redirected_private_refused", func(t *testing.T) {
		refusal := v32Evaluate(t, []contract.ScenarioAssertion{
			v32Assertion("redirect-rejected", "read", "effect", "/effect/state", `"rejected"`),
			v32Assertion("redirect-no-send-receipt", "read", "equals", "/effect/receipt_refs", `[]`),
			v32Assertion("redirect-no-records", "read", "absent", "/data/elements/0", `null`),
		}, OracleObservation{Document: json.RawMessage(v32RedirectedPrivateRefusalJSON)})
		v32RequireAllPassed(t, refusal)

		followed := v32Evaluate(t, []contract.ScenarioAssertion{
			v32Assertion("private-no-send-receipt", "read", "equals", "/effect/receipt_refs", `[]`),
			v32Assertion("private-unsent", "read", "effect", "/effect/state", `"unsent"`),
			v32Assertion("private-rejected", "read", "effect", "/effect/state", `"rejected"`),
			v32Assertion("private-no-continuation", "read", "equals", "/pagination/continuation", `{"presence":"absent"}`),
		}, OracleObservation{Document: json.RawMessage(v32RedirectedPrivatePageJSON)})
		v32RequireFailed(t, followed, "private-no-send-receipt", "private-unsent", "private-rejected")
		v32RequirePassed(t, followed, "private-no-continuation")
	})
}

// v32GitHubScope covers pm-github-direct-slice-S04: a wrong-workspace read
// publishes no result document at all, and denied or revoked connections are
// refusals with zero provider sends, so every authored read expectation fails
// instead of passing.
func v32GitHubScope(t *testing.T) {
	readAssertions := []contract.ScenarioAssertion{
		v32Assertion("scope-record-id", "read", "equals", "/data/elements/0/fields/id",
			`{"presence":"value","type":"integer","exact":"9001","precision":4,"scale":0}`),
		v32Assertion("scope-record-number", "read", "equals", "/data/elements/0/fields/number",
			`{"presence":"value","type":"integer","exact":"17","precision":2,"scale":0}`),
		v32Assertion("scope-record-repository", "read", "equals", "/data/elements/0/fields/repository/fields/full_name",
			`{"presence":"value","type":"string","text":"acme/widgets"}`),
		v32Assertion("scope-row", "read", "row", "/data/elements/0", v32Issue9001JSON),
	}

	t.Run("wrong_workspace_publishes_no_document", func(t *testing.T) {
		outcomes := v32Evaluate(t, append(readAssertions,
			v32Assertion("scope-no-result", "read", "absent", "/data", `null`),
		), OracleObservation{})
		v32RequireFailed(t, outcomes, "scope-record-id", "scope-record-number", "scope-record-repository", "scope-row")
		v32RequirePassed(t, outcomes, "scope-no-result")
	})

	t.Run("denied_connection_zero_sends", func(t *testing.T) {
		outcomes := v32Evaluate(t, append(readAssertions,
			v32Assertion("denied-state", "read", "effect", "/effect/state", `"rejected"`),
			v32Assertion("denied-no-send-receipt", "read", "equals", "/effect/receipt_refs", `[]`),
			v32Assertion("denied-no-result", "read", "absent", "/data/elements/0", `null`),
		), OracleObservation{Document: json.RawMessage(v32DeniedConnectionRefusalJSON)})
		v32RequireFailed(t, outcomes, "scope-record-id", "scope-record-number", "scope-record-repository", "scope-row")
		v32RequirePassed(t, outcomes, "denied-state", "denied-no-send-receipt", "denied-no-result")
	})

	t.Run("revoked_connection_zero_sends", func(t *testing.T) {
		outcomes := v32Evaluate(t, append(readAssertions,
			v32Assertion("revoked-state", "read", "effect", "/effect/state", `"unsent"`),
			v32Assertion("revoked-no-send-receipt", "read", "equals", "/effect/receipt_refs", `[]`),
			v32Assertion("revoked-no-result", "read", "absent", "/data/elements/0", `null`),
		), OracleObservation{Document: json.RawMessage(v32RevokedConnectionRefusalJSON)})
		v32RequireFailed(t, outcomes, "scope-record-id", "scope-record-number", "scope-record-repository", "scope-row")
		v32RequirePassed(t, outcomes, "revoked-state", "revoked-no-send-receipt", "revoked-no-result")
	})
}

// v32GitHubWrongRecord covers pm-github-direct-slice-S05: a successful HTTP
// response with a wrong id, wrong repository-scoped number, wrong repository, a
// missing required (nested) field or a wrong declared type fails the
// independently authored expectation that the seeded record already satisfies.
func v32GitHubWrongRecord(t *testing.T) {
	assertions := v32ExactReadAssertions()

	t.Run("wrong_id", func(t *testing.T) {
		outcomes := v32Evaluate(t, assertions, OracleObservation{
			Document: json.RawMessage(v32ReplaceOnce(t, v32ExactReadPageJSON, `"exact":"9001"`, `"exact":"9017"`)),
		})
		v32RequireFailed(t, outcomes, "record-id", "record-row")
		v32RequirePassed(t, outcomes, "record-number", "record-repository", "page-counts")
	})

	t.Run("wrong_number", func(t *testing.T) {
		outcomes := v32Evaluate(t, assertions, OracleObservation{
			Document: json.RawMessage(v32ReplaceOnce(t, v32ExactReadPageJSON, `"exact":"17"`, `"exact":"71"`)),
		})
		v32RequireFailed(t, outcomes, "record-number", "record-row")
		v32RequirePassed(t, outcomes, "record-id", "record-repository", "page-counts")
	})

	t.Run("wrong_repository", func(t *testing.T) {
		outcomes := v32Evaluate(t, assertions, OracleObservation{
			Document: json.RawMessage(v32ReplaceOnce(t, v32ExactReadPageJSON, v32Repository, "acme/gadgets")),
		})
		v32RequireFailed(t, outcomes, "record-repository", "record-row")
		v32RequirePassed(t, outcomes, "record-id", "record-number", "page-counts")
	})

	t.Run("missing_required_field", func(t *testing.T) {
		labels := `{"presence":"value","type":"array","elements":[{"presence":"value","type":"string","text":"bug"},{"presence":"value","type":"string","text":"regression"}]}`
		outcomes := v32Evaluate(t, assertions, OracleObservation{
			Document: json.RawMessage(v32ReplaceOnce(t, v32ExactReadPageJSON, labels, `{"presence":"absent"}`)),
		})
		v32RequireFailed(t, outcomes, "record-labels", "record-row")
		v32RequirePassed(t, outcomes, "record-id", "record-number", "record-repository")
	})

	t.Run("missing_nested_required_field", func(t *testing.T) {
		fullName := `"full_name":{"presence":"value","type":"string","text":"acme/widgets"}`
		outcomes := v32Evaluate(t, assertions, OracleObservation{
			Document: json.RawMessage(v32ReplaceOnce(t, v32ExactReadPageJSON, fullName, `"full_name":{"presence":"absent"}`)),
		})
		v32RequireFailed(t, outcomes, "record-repository", "record-row")
		v32RequirePassed(t, outcomes, "record-id", "record-number", "record-owner")
	})

	t.Run("wrong_field_type", func(t *testing.T) {
		integerID := `{"presence":"value","type":"integer","exact":"9001","precision":4,"scale":0}`
		outcomes := v32Evaluate(t, assertions, OracleObservation{
			Document: json.RawMessage(v32ReplaceOnce(t, v32ExactReadPageJSON, integerID, `{"presence":"value","type":"string","text":"9001"}`)),
		})
		v32RequireFailed(t, outcomes, "record-id", "record-row")
		v32RequirePassed(t, outcomes, "record-number", "record-repository", "page-counts")
	})

	t.Run("whole_page_from_another_repository", func(t *testing.T) {
		outcomes := v32Evaluate(t, assertions, OracleObservation{Document: json.RawMessage(v32ForeignRecordPageJSON)})
		v32RequireFailed(t, outcomes, "record-id", "record-number", "record-repository", "record-row")
		v32RequirePassed(t, outcomes, "media-type", "page-counts", "page-terminal")
	})
}

// v32GitHubMixedEntries covers pm-github-direct-slice-S06-C01/C03/C04: both the
// issue and the pull-request-shaped entry reach the result with their exact
// identities, discriminator and nested presence, and a hidden exclusion of the
// pull-request entry is refused.
func v32GitHubMixedEntries(t *testing.T) {
	mixedAssertions := []contract.ScenarioAssertion{
		v32Assertion("mixed-issue-id", "read", "equals", "/data/elements/0/fields/id",
			`{"presence":"value","type":"integer","exact":"9001","precision":4,"scale":0}`),
		v32Assertion("mixed-issue-number", "read", "equals", "/data/elements/0/fields/number",
			`{"presence":"value","type":"integer","exact":"17","precision":2,"scale":0}`),
		v32Assertion("mixed-issue-repository", "read", "equals", "/data/elements/0/fields/repository/fields/full_name",
			`{"presence":"value","type":"string","text":"acme/widgets"}`),
		v32Assertion("mixed-issue-not-pull-request", "read", "equals", "/data/elements/0/fields/pull_request", `{"presence":"absent"}`),
		v32Assertion("mixed-issue-body-null", "read", "equals", "/data/elements/0/fields/body", `{"presence":"null"}`),
		v32Assertion("mixed-issue-milestone-absent", "read", "equals", "/data/elements/0/fields/milestone", `{"presence":"absent"}`),
		v32Assertion("mixed-issue-locked-false", "read", "equals", "/data/elements/0/fields/locked", `{"presence":"value","type":"bool","boolean":false}`),
		v32Assertion("mixed-issue-comments-zero", "read", "equals", "/data/elements/0/fields/comments", `{"presence":"value","type":"integer","exact":"0","precision":1,"scale":0}`),
		v32Assertion("mixed-issue-assignees-empty", "read", "equals", "/data/elements/0/fields/assignees", `{"presence":"value","type":"array","elements":[]}`),
		v32Assertion("mixed-pr-id", "read", "equals", "/data/elements/1/fields/id",
			`{"presence":"value","type":"integer","exact":"9002","precision":4,"scale":0}`),
		v32Assertion("mixed-pr-number", "read", "equals", "/data/elements/1/fields/number",
			`{"presence":"value","type":"integer","exact":"18","precision":2,"scale":0}`),
		v32Assertion("mixed-pr-repository", "read", "equals", "/data/elements/1/fields/repository/fields/full_name",
			`{"presence":"value","type":"string","text":"acme/widgets"}`),
		v32Assertion("mixed-pr-discriminator", "read", "equals", "/data/elements/1/fields/pull_request/presence", `"value"`),
		v32Assertion("mixed-pr-url", "read", "equals", "/data/elements/1/fields/pull_request/fields/url",
			`{"presence":"value","type":"string","text":"https://api.github.com/repos/acme/widgets/pulls/18"}`),
		v32Assertion("mixed-pr-merged-at-null", "read", "equals", "/data/elements/1/fields/pull_request/fields/merged_at", `{"presence":"null"}`),
		v32Assertion("mixed-pr-draft-false", "read", "equals", "/data/elements/1/fields/pull_request/fields/draft", `{"presence":"value","type":"bool","boolean":false}`),
		v32Assertion("mixed-no-third-entry", "read", "absent", "/data/elements/2", `null`),
		v32Assertion("mixed-fetched", "read", "equals", "/pagination/fetched_count", `2`),
		v32Assertion("mixed-returned", "read", "equals", "/pagination/returned_count", `2`),
		v32Assertion("mixed-emitted", "read", "equals", "/pagination/emitted_count", `2`),
		v32Assertion("mixed-terminal", "read", "equals", "/pagination/source_exhausted", `true`),
	}

	v32RequireAllPassed(t, v32Evaluate(t, mixedAssertions, OracleObservation{Document: json.RawMessage(v32MixedPageJSON)}))

	hidden := v32Evaluate(t, mixedAssertions, OracleObservation{Document: json.RawMessage(v32HiddenExclusionPageJSON)})
	v32RequireFailed(t, hidden,
		"mixed-pr-id", "mixed-pr-number", "mixed-pr-repository", "mixed-pr-discriminator",
		"mixed-pr-url", "mixed-pr-merged-at-null", "mixed-pr-draft-false", "mixed-emitted")
	v32RequirePassed(t, hidden, "mixed-issue-id", "mixed-issue-number", "mixed-fetched", "mixed-returned")
}

// v32IndependentCases covers pm-github-direct-slice-S09: a failed case must not
// erase a passing sibling's receipt, credential presence alone must never pass a
// read case, and a receipt that is stale for the current source, build or
// account scope cannot certify the current case.
func v32IndependentCases(t *testing.T) {
	store, err := OpenReceiptStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatalf("open proof receipt store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close proof receipt store: %v", err)
		}
	})

	scope := v32SemanticDigest(v32ScopeLabel)
	exact := v32CaseIdentity(v32ExactCaseID, "live", scope)
	mixed := v32CaseIdentity(v32MixedCaseID, "live", scope)
	started := time.Date(2026, 9, 19, 15, 0, 0, 0, time.UTC)
	exactReceipt := v32ReceiptFixture(t, exact, contract.ProofPassedLive, started)
	mixedReceipt := v32ReceiptFixture(t, mixed, contract.ProofPassedLive, started)
	mixedFailed := v32FailedReceipt(t, mixedReceipt, "run-"+v32MixedCaseID+"-failed", started.Add(5*time.Second))

	exactRecord, err := store.Append(t.Context(), contract.LaneEligible, exactReceipt)
	if err != nil {
		t.Fatalf("append passing exact receipt: %v", err)
	}
	mixedFailureRecord, err := store.Append(t.Context(), contract.LaneEligible, mixedFailed)
	if err != nil {
		t.Fatalf("append failed sibling receipt: %v", err)
	}

	survivor, found, err := store.Latest(t.Context(), exact)
	if err != nil || !found {
		t.Fatalf("passing sibling receipt missing after sibling failure: found=%v err=%v", found, err)
	}
	if survivor.ID != exactRecord.ID || survivor.Receipt.Status != contract.ProofPassedLive {
		t.Fatalf("passing sibling receipt was replaced: %#v", survivor)
	}

	decisions, err := SelectCases(ModeRetry, []CaseCandidate{
		{Current: v32CurrentnessFixture(t, exactReceipt), Availability: contract.LaneEligible, Reason: "independent exact read case"},
		{Current: v32CurrentnessFixture(t, mixedFailed), Availability: contract.LaneEligible, Reason: "independent mixed read case"},
	}, map[string]ReceiptRecord{
		v32CaseKey(t, exact): exactRecord,
		v32CaseKey(t, mixed): mixedFailureRecord,
	})
	if err != nil {
		t.Fatalf("select proof cases after a sibling failure: %v", err)
	}
	if !decisions[0].Reused || decisions[0].Execute {
		t.Fatalf("current passing sibling was not reused: %#v", decisions[0])
	}
	if !decisions[1].Execute || decisions[1].Reused {
		t.Fatalf("failed sibling was not re-executed: %#v", decisions[1])
	}
	if decisions[1].Receipt == nil || decisions[1].Receipt.Receipt.Status != contract.ProofFailed {
		t.Fatalf("failed sibling evidence was erased: %#v", decisions[1].Receipt)
	}

	t.Run("credential_presence_alone_cannot_pass", func(t *testing.T) {
		credentialOnly := OracleObservation{Document: json.RawMessage(v32CredentialPresenceJSON)}
		outcomes := v32Evaluate(t, v32ExactReadAssertions(), credentialOnly)
		v32RequireFailed(t, outcomes, "record-id", "record-number", "record-repository", "record-row")
		v32RequirePassed(t, outcomes, "media-type")

		forged := v32CloneReceipt(t, exactReceipt)
		forged.Assertions = make([]contract.ProofAssertionObservation, 0, len(outcomes))
		for _, outcome := range outcomes {
			forged.Assertions = append(forged.Assertions, outcome)
		}
		if err := forged.Validate(); err == nil {
			t.Fatal("credential presence was assembled into a completed passing receipt")
		}

		unproven := v32CaseIdentity("github-read-credential-only", "live", scope)
		pending := v32ReceiptFixture(t, unproven, contract.ProofPassedLive, started)
		decisions, err := SelectCases(ModeRetry,
			[]CaseCandidate{{Current: v32CurrentnessFixture(t, pending), Availability: contract.LaneEligible, Reason: "credential presence is not evidence"}},
			map[string]ReceiptRecord{})
		if err != nil {
			t.Fatalf("select case without a receipt: %v", err)
		}
		if !decisions[0].Execute || decisions[0].Reused || decisions[0].Receipt != nil {
			t.Fatalf("credential presence was treated as recorded evidence: %#v", decisions[0])
		}
	})

	t.Run("stale_receipt_cannot_certify_current_case", func(t *testing.T) {
		current := v32CurrentnessFixture(t, exactReceipt)
		fresh, err := IsCurrent(exactReceipt, current)
		if err != nil {
			t.Fatalf("evaluate current receipt: %v", err)
		}
		if !fresh {
			t.Fatal("matching current case was not certified by its fresh receipt")
		}

		variations := []struct {
			name    string
			current Currentness
		}{
			{name: "changed_source", current: func() Currentness {
				stale := v32CloneCurrentness(current)
				stale.SourceHashes = []contract.SourceHash{v32SourceHash(v32SourceLockLabel), v32SourceHash("github-semantics-rebuilt")}
				return stale
			}()},
			{name: "changed_build", current: func() Currentness {
				stale := v32CloneCurrentness(current)
				stale.BundleHash = v32SourceHash("bundle-github-direct-read-rebuilt")
				return stale
			}()},
			{name: "changed_account_scope", current: func() Currentness {
				stale := v32CloneCurrentness(current)
				stale.Identity.ScopeHash = v32SemanticDigest("workspace-beta-account")
				return stale
			}()},
		}
		for _, variation := range variations {
			t.Run(variation.name, func(t *testing.T) {
				certified, err := IsCurrent(exactReceipt, variation.current)
				if err != nil {
					t.Fatalf("evaluate stale receipt: %v", err)
				}
				if certified {
					t.Fatal("stale receipt certified the current proof case")
				}
				// A receipt from another account scope is a different case
				// identity, so it is not the latest receipt of this case. It
				// may only be presented under its own exact key.
				latest := map[string]ReceiptRecord{}
				if candidateKey, err := CaseKey(variation.current.Identity); err != nil {
					t.Fatalf("derive candidate case key: %v", err)
				} else if candidateKey == v32CaseKey(t, exact) {
					latest[candidateKey] = exactRecord
				}
				decisions, err := SelectCases(ModeRetry,
					[]CaseCandidate{{Current: variation.current, Availability: contract.LaneEligible, Reason: "stale receipt must not be reused"}},
					latest)
				if err != nil {
					t.Fatalf("select stale proof case: %v", err)
				}
				if !decisions[0].Execute || decisions[0].Reused {
					t.Fatalf("stale receipt was reused: %#v", decisions[0])
				}
			})
		}
	})
}

// v32LaneInventory covers pm-github-direct-slice-S10: all seven lane
// inventories exist with only direct_read source-eligible and proven, deleted
// cells and forged readiness are refused by the strict decoder, an unsupported
// lane is never selected, and controlled fake evidence is never recorded or
// relabelled as live provider proof.
func v32LaneInventory(t *testing.T) {
	bundle := []byte(v32BundleJSON)
	inventories, err := Inventory(bundle, []contract.Scenario{v32InventoryScenario()})
	if err != nil {
		t.Fatalf("derive lane inventories: %v", err)
	}
	if len(inventories) != 7 {
		t.Fatalf("lane inventories = %d; want all seven canonical lanes", len(inventories))
	}
	byLane := make(map[contract.Lane]LaneInventory, len(inventories))
	for _, inventory := range inventories {
		if _, duplicate := byLane[inventory.Lane]; duplicate {
			t.Fatalf("lane %q inventory is duplicated", inventory.Lane)
		}
		byLane[inventory.Lane] = inventory
	}
	for _, lane := range []contract.Lane{
		contract.LaneDirectRead, contract.LaneDirectWrite, contract.LaneETL, contract.LaneReverseETL,
		contract.LaneBinaryDownload, contract.LaneBinaryUpload, contract.LaneSyncTransport,
	} {
		if _, present := byLane[lane]; !present {
			t.Fatalf("lane %q inventory is missing", lane)
		}
	}

	directRead := byLane[contract.LaneDirectRead]
	if directRead.SchemaVersion != LaneInventorySchemaVersion || directRead.ConnectorID != v32ConnectorID {
		t.Fatalf("direct_read inventory header = %#v", directRead)
	}
	if len(directRead.Operations) != 1 {
		t.Fatalf("direct_read operations = %d; want one", len(directRead.Operations))
	}
	eligibleCell := directRead.Operations[0]
	if eligibleCell.OperationID != v32OperationID || eligibleCell.Status != contract.LaneEligible ||
		eligibleCell.Reason != "source-declared-repository-issue-read" {
		t.Fatalf("eligible direct_read cell = %#v", eligibleCell)
	}
	if len(eligibleCell.Cases) != 1 {
		t.Fatalf("direct_read authored cases = %d; want one", len(eligibleCell.Cases))
	}
	authoredCase := eligibleCell.Cases[0]
	if authoredCase.BehaviorID != v32ExactCaseID || authoredCase.CaseID != v32ExactCaseID {
		t.Fatalf("direct_read authored case identity = %#v", authoredCase)
	}
	if authoredCase.Scenario.Lane != contract.LaneDirectRead || authoredCase.Scenario.OperationID != v32OperationID {
		t.Fatalf("direct_read authored scenario is bound to a different cell: %#v", authoredCase.Scenario)
	}
	if err := authoredCase.ScenarioHash.Validate(); err != nil {
		t.Fatalf("direct_read scenario hash is not an exact digest: %v", err)
	}

	for lane, want := range map[contract.Lane]struct {
		Status contract.LaneStatus
		Reason string
	}{
		contract.LaneDirectWrite:    {Status: contract.LaneUnsupported, Reason: "source-does-not-declare-write"},
		contract.LaneETL:            {Status: contract.LaneUnproven, Reason: "runtime-proof-not-yet-established"},
		contract.LaneReverseETL:     {Status: contract.LaneNotApplicable, Reason: "read-operation-has-no-destination-write"},
		contract.LaneBinaryDownload: {Status: contract.LaneNotApplicable, Reason: "response-is-not-binary"},
		contract.LaneBinaryUpload:   {Status: contract.LaneNotApplicable, Reason: "request-is-not-binary"},
		contract.LaneSyncTransport:  {Status: contract.LaneBlocked, Reason: "sync-semantics-are-not-declared"},
	} {
		inventory := byLane[lane]
		if len(inventory.Operations) != 1 {
			t.Fatalf("lane %q operations = %d; want one", lane, len(inventory.Operations))
		}
		cell := inventory.Operations[0]
		if cell.Status != want.Status || cell.Reason != want.Reason {
			t.Fatalf("lane %q cell = %#v; want %s/%s", lane, cell, want.Status, want.Reason)
		}
		if len(cell.Cases) != 0 {
			t.Fatalf("lane %q retained %d authored cases while unproven", lane, len(cell.Cases))
		}
	}

	t.Run("canonical_document_round_trips", func(t *testing.T) {
		document := v32MarshalLaneInventory(t, directRead)
		decoded, err := DecodeLaneInventory(document, bundle, 1<<20)
		if err != nil {
			t.Fatalf("decode canonical direct_read inventory: %v", err)
		}
		if decoded.Lane != contract.LaneDirectRead || len(decoded.Operations) != 1 ||
			decoded.Operations[0].Status != contract.LaneEligible || len(decoded.Operations[0].Cases) != 1 ||
			decoded.Operations[0].Cases[0].Scenario.ScenarioID != v32ExactCaseID {
			t.Fatalf("decoded direct_read inventory = %#v", decoded)
		}
	})

	t.Run("deleted_or_forged_documents_are_refused", func(t *testing.T) {
		forged := v32MutateLaneDocument(t, v32MarshalLaneInventory(t, byLane[contract.LaneDirectWrite]),
			func(t *testing.T, document map[string]any) {
				operations, ok := document["operations"].([]any)
				if !ok || len(operations) != 1 {
					t.Fatalf("direct_write fixture operations = %#v", document["operations"])
				}
				cell, ok := operations[0].(map[string]any)
				if !ok {
					t.Fatalf("direct_write fixture cell = %#v", operations[0])
				}
				cell["status"] = string(contract.LaneEligible)
				cell["reason"] = "forged-readiness"
			})
		if _, err := DecodeLaneInventory(forged, bundle, 1<<20); err == nil {
			t.Fatal("forged eligible status on an unsupported lane was accepted")
		}

		deleted := v32MutateLaneDocument(t, v32MarshalLaneInventory(t, directRead),
			func(_ *testing.T, document map[string]any) {
				document["operations"] = []any{}
			})
		if _, err := DecodeLaneInventory(deleted, bundle, 1<<20); err == nil {
			t.Fatal("inventory with a deleted operation cell was accepted")
		}

		staleBundle := []byte(v32ReplaceOnce(t, v32BundleJSON,
			"sha256:f0e1d2c3b4a5968778695a4b3c2d1e0ff0e1d2c3b4a5968778695a4b3c2d1e0f",
			"sha256:0f1e2d3c4b5a697887965a4b3c2d1e0f0f1e2d3c4b5a697887965a4b3c2d1e0f"))
		if _, err := DecodeLaneInventory(v32MarshalLaneInventory(t, directRead), staleBundle, 1<<20); err == nil {
			t.Fatal("inventory was accepted under a stale bundle identity")
		}
	})

	t.Run("unsupported_lane_is_never_selected", func(t *testing.T) {
		scope := v32SemanticDigest(v32ScopeLabel)
		candidate := CaseCandidate{
			Current:      Currentness{Identity: v32CaseIdentity(v32ExactCaseID, "live", scope)},
			Availability: contract.LaneUnsupported,
			Reason:       "source-does-not-declare-write",
		}
		for _, mode := range []Mode{ModeRetry, ModeAll} {
			decisions, err := SelectCases(mode, []CaseCandidate{candidate}, map[string]ReceiptRecord{})
			if err != nil {
				t.Fatalf("select unsupported lane in mode %q: %v", mode, err)
			}
			if decisions[0].Execute || decisions[0].Reused || decisions[0].Receipt != nil {
				t.Fatalf("unsupported lane was selected for execution in mode %q: %#v", mode, decisions[0])
			}
		}
	})

	t.Run("fake_evidence_is_never_live", func(t *testing.T) {
		store, err := OpenReceiptStore(t.TempDir(), 1<<20)
		if err != nil {
			t.Fatalf("open proof receipt store: %v", err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Errorf("close proof receipt store: %v", err)
			}
		})
		scope := v32SemanticDigest(v32ScopeLabel)
		started := time.Date(2026, 9, 19, 16, 0, 0, 0, time.UTC)
		fakeReceipt := v32ReceiptFixture(t, v32CaseIdentity(v32ExactCaseID, "fake", scope), contract.ProofPassedFake, started)

		if _, err := store.Append(t.Context(), contract.LaneUnsupported, fakeReceipt); err == nil {
			t.Fatal("passing controlled receipt was recorded for an unsupported lane")
		}
		if _, err := store.Append(t.Context(), contract.LaneUnproven, fakeReceipt); err == nil {
			t.Fatal("passing controlled receipt was recorded for an unproven lane")
		}
		record, err := store.Append(t.Context(), contract.LaneEligible, fakeReceipt)
		if err != nil {
			t.Fatalf("record controlled fake receipt: %v", err)
		}
		if record.Receipt.Status != contract.ProofPassedFake || record.Receipt.Provenance.ExecutionKind != "fake" {
			t.Fatalf("controlled fake evidence was recorded as live provider proof: %#v", record.Receipt)
		}

		liveCurrent := v32CurrentnessFixture(t, v32ReceiptFixture(t,
			v32CaseIdentity(v32ExactCaseID, "live", scope), contract.ProofPassedLive, started))
		certified, err := IsCurrent(record.Receipt, liveCurrent)
		if err != nil {
			t.Fatalf("evaluate controlled fake receipt against a live case: %v", err)
		}
		if certified {
			t.Fatal("controlled fake receipt certified a live provider case")
		}

		relabelled := v32CloneReceipt(t, fakeReceipt)
		relabelled.Status = contract.ProofPassedLive
		if err := relabelled.Validate(); err == nil {
			t.Fatal("controlled fake receipt was relabelled as live provider proof")
		}
	})
}

// v32InventoryScenario is the seeded direct_read scenario whose only purpose in
// this file is to occupy the eligible cell of the lane inventory.
func v32InventoryScenario() contract.Scenario {
	return contract.Scenario{
		ScenarioVersion: "pm.scenario/v1",
		ScenarioID:      v32ExactCaseID,
		ConnectorID:     v32ConnectorID,
		Lane:            contract.LaneDirectRead,
		OperationID:     v32OperationID,
		SourceRefs: []contract.SourceRef{{
			SourceID: "GITHUB-REST-SPEC-ACCEPTED",
			Pointer:  "#/paths/~1repos~1{owner}~1{repo}~1issues/get",
		}},
		Requirements: []contract.ScenarioRequirement{{
			RequirementID: "direct-read",
			Kind:          "capability",
			Value:         "http-direct-read",
		}},
		Steps: []contract.ScenarioStep{{
			StepID:    "read",
			Kind:      "invoke",
			DependsOn: []string{},
			Action:    "issues-list-for-repo",
			Input:     json.RawMessage(`{"owner":"acme","repo":"widgets"}`),
			Captures:  []contract.ScenarioCapture{},
		}},
		Assertions: []contract.ScenarioAssertion{
			v32Assertion("record-id", "read", "equals", "/data/elements/0/fields/id",
				`{"presence":"value","type":"integer","exact":"9001","precision":4,"scale":0}`),
		},
		Cleanup: contract.ScenarioCleanup{Always: true, OwnedIDCaptures: []string{}, Steps: []contract.ScenarioStep{}},
	}
}

func v32Assertion(assertionID, stepID, kind, pointer, expected string) contract.ScenarioAssertion {
	return contract.ScenarioAssertion{
		AssertionID: assertionID,
		StepID:      stepID,
		Kind:        kind,
		Pointer:     pointer,
		Expected:    json.RawMessage(expected),
	}
}

// v32Evaluate runs every authored assertion through the oracle. A validation
// error is a broken fixture or malformed observation, never an ordinary
// mismatch, so it fails the test immediately.
func v32Evaluate(t *testing.T, assertions []contract.ScenarioAssertion, observation OracleObservation) map[string]contract.ProofAssertionObservation {
	t.Helper()
	outcomes := make(map[string]contract.ProofAssertionObservation, len(assertions))
	for _, assertion := range assertions {
		outcome, err := AssertExpected(assertion, observation)
		if err != nil {
			t.Fatalf("oracle rejected assertion %q: %v", assertion.AssertionID, err)
		}
		outcomes[assertion.AssertionID] = outcome
	}
	return outcomes
}

func v32RequireAllPassed(t *testing.T, outcomes map[string]contract.ProofAssertionObservation) {
	t.Helper()
	for assertionID, outcome := range outcomes {
		if outcome.Passed == nil || !*outcome.Passed {
			t.Fatalf("assertion %q did not pass against the seeded observation", assertionID)
		}
	}
}

func v32RequirePassed(t *testing.T, outcomes map[string]contract.ProofAssertionObservation, assertionIDs ...string) {
	t.Helper()
	for _, assertionID := range assertionIDs {
		outcome, present := outcomes[assertionID]
		if !present {
			t.Fatalf("assertion %q was not evaluated", assertionID)
		}
		if outcome.Passed == nil || !*outcome.Passed {
			t.Fatalf("assertion %q passed=%v; want a pass", assertionID, outcome.Passed)
		}
	}
}

func v32RequireFailed(t *testing.T, outcomes map[string]contract.ProofAssertionObservation, assertionIDs ...string) {
	t.Helper()
	for _, assertionID := range assertionIDs {
		outcome, present := outcomes[assertionID]
		if !present {
			t.Fatalf("assertion %q was not evaluated", assertionID)
		}
		if outcome.Passed == nil || *outcome.Passed {
			t.Fatalf("assertion %q passed=%v; want a mismatch", assertionID, outcome.Passed)
		}
	}
}

// v32ReplaceOnce rewrites one exact marker of a seeded observation document, so
// a false-success variation keeps every other authored field intact.
func v32ReplaceOnce(t *testing.T, document, old, replacement string) string {
	t.Helper()
	if strings.Count(document, old) != 1 {
		t.Fatalf("fixture marker %q occurs %d times; want exactly one", old, strings.Count(document, old))
	}
	return strings.Replace(document, old, replacement, 1)
}

func v32MutateLaneDocument(t *testing.T, document []byte, mutate func(t *testing.T, decoded map[string]any)) []byte {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(document, &decoded); err != nil {
		t.Fatalf("decode lane inventory fixture: %v", err)
	}
	mutate(t, decoded)
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("encode mutated lane inventory fixture: %v", err)
	}
	return encoded
}

func v32MarshalLaneInventory(t *testing.T, inventory LaneInventory) []byte {
	t.Helper()
	document, err := json.Marshal(inventory)
	if err != nil {
		t.Fatalf("encode lane inventory fixture: %v", err)
	}
	return document
}

func v32SourceHash(label string) contract.SourceHash {
	return contract.RawSourceHash([]byte(label))
}

func v32SemanticDigest(label string) contract.SemanticDigest {
	return contract.SemanticDigest(contract.RawSourceHash([]byte(label)))
}

// v32CaseIdentity is the seeded proof case identity: one behavior and case
// identity in the exact GitHub direct_read operation with the authored account
// scope hash.
func v32CaseIdentity(caseID, executionKind string, scope contract.SemanticDigest) CaseIdentity {
	return CaseIdentity{
		ConnectorID:   v32ConnectorID,
		Lane:          contract.LaneDirectRead,
		OperationID:   v32OperationID,
		BehaviorID:    caseID,
		CaseID:        caseID,
		ExecutionKind: executionKind,
		ScopeHash:     scope,
	}
}

func v32CaseKey(t *testing.T, identity CaseIdentity) string {
	t.Helper()
	key, err := CaseKey(identity)
	if err != nil {
		t.Fatalf("derive proof case key: %v", err)
	}
	return key
}

func v32ReceiptFixture(t *testing.T, identity CaseIdentity, status contract.ProofStatus, started time.Time) contract.ProofReceipt {
	t.Helper()
	runID := "run-" + identity.CaseID
	hostClass := "controlled-darwin-arm64"
	reused := false
	passed := true
	bundle := v32SourceHash(v32BundleLabel)
	schema := v32SourceHash(v32SchemaLabel)
	client := v32SourceHash(v32ClientLabel)
	api := v32SourceHash(v32APIQueryLabel)
	worker := v32SourceHash(v32WorkerLabel)
	closure := v32SemanticDigest(v32ClosureLabel)
	scope := identity.ScopeHash
	finished := started.Add(2 * time.Second)
	receipt := contract.ProofReceipt{
		ProofVersion:              contract.ProofSchemaVersion,
		ConnectorID:               identity.ConnectorID,
		Lane:                      identity.Lane,
		OperationID:               identity.OperationID,
		BehaviorID:                identity.BehaviorID,
		CaseID:                    identity.CaseID,
		RunID:                     &runID,
		SourceHashes:              []contract.SourceHash{v32SourceHash(v32SourceLockLabel), v32SourceHash(v32SemanticsLabel)},
		BundleHash:                &bundle,
		SchemaHash:                &schema,
		AssertionHash:             v32SemanticDigest(v32AssertionSetName),
		PMClientHash:              &client,
		PMAPIHash:                 &api,
		PMWorkerHash:              &worker,
		QueryHelperHashIfUsed:     nil,
		HandlerVersions:           map[string]string{"http": "v1"},
		WorkspaceAccountScopeHash: &scope,
		Command:                   []string{"pm", "proof", identity.CaseID},
		Request: &contract.ProofRequestObservation{
			Method:          "GET",
			Target:          "https://api.github.com/repos/acme/widgets/issues?per_page=100",
			MediaType:       "application/json",
			SanitizedFields: map[string]json.RawMessage{"owner": json.RawMessage(`"acme"`), "repo": json.RawMessage(`"widgets"`)},
			Redactions:      []contract.ProofRedaction{},
		},
		Response: &contract.ProofResponseObservation{
			Status:          200,
			MediaType:       "application/json",
			SanitizedFields: map[string]json.RawMessage{"record": json.RawMessage(v32Issue9001JSON)},
			Redactions:      []contract.ProofRedaction{},
		},
		Assertions: []contract.ProofAssertionObservation{{
			AssertionID: "record-id",
			Expected:    json.RawMessage(`{"presence":"value","type":"integer","exact":"9001","precision":4,"scale":0}`),
			Observed:    json.RawMessage(`{"presence":"value","type":"integer","exact":"9001","precision":4,"scale":0}`),
			Passed:      &passed,
		}},
		Effect: contract.ProofEffect{
			State:           contract.OutcomeSucceeded,
			ReceiptRefs:     []string{"provider-receipt-github-read"},
			OwnedObjectRefs: []string{},
		},
		Status:     status,
		StartedAt:  &started,
		FinishedAt: &finished,
		Provenance: contract.ProofProvenance{
			ExecutionKind: identity.ExecutionKind,
			HostClass:     &hostClass,
			Reused:        &reused,
		},
		ImplementationClosure: &contract.ImplementationClosure{Known: true, Digest: &closure},
		CleanupDiagnostics:    []contract.ProofCleanupDiagnostic{},
	}
	if err := receipt.Validate(); err != nil {
		t.Fatalf("seeded proof receipt for case %q is invalid: %v", identity.CaseID, err)
	}
	return receipt
}

func v32FailedReceipt(t *testing.T, original contract.ProofReceipt, runID string, started time.Time) contract.ProofReceipt {
	t.Helper()
	failed := v32CloneReceipt(t, original)
	passed := false
	finished := started.Add(time.Second)
	failed.RunID = &runID
	for index := range failed.Assertions {
		failed.Assertions[index].Passed = &passed
	}
	failed.Effect.State = contract.OutcomeRejected
	failed.Status = contract.ProofFailed
	failed.StartedAt = &started
	failed.FinishedAt = &finished
	if err := failed.Validate(); err != nil {
		t.Fatalf("seeded failed proof receipt is invalid: %v", err)
	}
	return failed
}

func v32CloneReceipt(t *testing.T, receipt contract.ProofReceipt) contract.ProofReceipt {
	t.Helper()
	document, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("encode proof receipt clone: %v", err)
	}
	var cloned contract.ProofReceipt
	if err := json.Unmarshal(document, &cloned); err != nil {
		t.Fatalf("decode proof receipt clone: %v", err)
	}
	return cloned
}

// v32CurrentnessFixture rebuilds the exact current source, build, account and
// assertion identity of a seeded receipt so a difference in any one of them is
// observable through IsCurrent and SelectCases.
func v32CurrentnessFixture(t *testing.T, receipt contract.ProofReceipt) Currentness {
	t.Helper()
	if receipt.BundleHash == nil || receipt.SchemaHash == nil || receipt.PMClientHash == nil ||
		receipt.PMAPIHash == nil || receipt.PMWorkerHash == nil ||
		receipt.WorkspaceAccountScopeHash == nil || receipt.ImplementationClosure == nil ||
		receipt.ImplementationClosure.Digest == nil {
		t.Fatal("seeded receipt lacks its currentness fields")
	}
	closure := *receipt.ImplementationClosure.Digest
	return Currentness{
		Identity:      v32CaseIdentity(receipt.CaseID, receipt.Provenance.ExecutionKind, *receipt.WorkspaceAccountScopeHash),
		SourceHashes:  append([]contract.SourceHash(nil), receipt.SourceHashes...),
		BundleHash:    *receipt.BundleHash,
		SchemaHash:    *receipt.SchemaHash,
		PMClientHash:  *receipt.PMClientHash,
		PMAPIHash:     *receipt.PMAPIHash,
		PMWorkerHash:  *receipt.PMWorkerHash,
		AssertionHash: receipt.AssertionHash,
		HandlerVersions: map[string]string{
			"http": "v1",
		},
		ImplementationClosure: contract.ImplementationClosure{Known: true, Digest: &closure},
	}
}

func v32CloneCurrentness(current Currentness) Currentness {
	cloned := current
	cloned.SourceHashes = append([]contract.SourceHash(nil), current.SourceHashes...)
	cloned.HandlerVersions = make(map[string]string, len(current.HandlerVersions))
	for handler, version := range current.HandlerVersions {
		cloned.HandlerVersions[handler] = version
	}
	if current.QueryHelperHash != nil {
		hash := *current.QueryHelperHash
		cloned.QueryHelperHash = &hash
	}
	if current.ImplementationClosure.Digest != nil {
		digest := *current.ImplementationClosure.Digest
		cloned.ImplementationClosure.Digest = &digest
	}
	return cloned
}
