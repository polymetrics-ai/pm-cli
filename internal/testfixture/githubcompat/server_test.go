package githubcompat_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture/githubcompat"
)

const (
	// fixtureTarget is the authored route of the controlled destination.
	fixtureTarget = "/repos/acme/widgets/issues"

	// notFoundDocument and validationFailedDocument are the authored
	// GitHub-shaped error documents the destination serves.
	notFoundDocument         = `{"message":"Not Found","documentation_url":"https://docs.github.com/rest"}`
	methodNotAllowedDocument = `{"message":"Method Not Allowed","documentation_url":"https://docs.github.com/rest"}`
	validationFailedDocument = `{"message":"Validation Failed","errors":[{"resource":"Issue","field":"page","code":"invalid"}],"documentation_url":"https://docs.github.com/rest"}`

	// jsonMediaType is the media type of every authored JSON response.
	jsonMediaType = "application/json; charset=utf-8"

	// fixtureOrigin is the admitted origin every authored continuation link
	// names.
	fixtureOrigin = "https://api.github.com"
)

// The authored nested objects of every record, transcribed independently of the
// package so a served page is pinned member by member.
const (
	authoredUserObject = `{"login":"ada","id":1,"node_id":"U_kgDOAA0001",` +
		`"avatar_url":"https://avatars.githubusercontent.com/u/1?v=4","gravatar_id":"",` +
		`"url":"https://api.github.com/users/ada","html_url":"https://github.com/ada",` +
		`"followers_url":"https://api.github.com/users/ada/followers",` +
		`"following_url":"https://api.github.com/users/ada/following{/other_user}",` +
		`"gists_url":"https://api.github.com/users/ada/gists{/gist_id}",` +
		`"starred_url":"https://api.github.com/users/ada/starred{/owner}{/repo}",` +
		`"subscriptions_url":"https://api.github.com/users/ada/subscriptions",` +
		`"organizations_url":"https://api.github.com/users/ada/orgs",` +
		`"repos_url":"https://api.github.com/users/ada/repos",` +
		`"events_url":"https://api.github.com/users/ada/events{/privacy}",` +
		`"received_events_url":"https://api.github.com/users/ada/received_events",` +
		`"type":"User","site_admin":false,"name":"Ada Lovelace","email":null}`

	authoredLabelObject = `{"id":208045946,"node_id":"MDU6TGFiZWwyMDgwNDU5NDY=",` +
		`"url":"https://api.github.com/repos/acme/widgets/labels/bug","name":"bug",` +
		`"description":"Something is not working","color":"d73a4a","default":true}`
)

// authoredPullRequestObject is the discriminator of the pull-request-shaped
// entry id 9002 number 18.
const authoredPullRequestObject = `{"url":"https://api.github.com/repos/acme/widgets/pulls/18",` +
	`"html_url":"https://github.com/acme/widgets/pull/18",` +
	`"diff_url":"https://github.com/acme/widgets/pull/18.diff",` +
	`"patch_url":"https://github.com/acme/widgets/pull/18.patch","merged_at":null}`

// authoredIssue9001 is the exact authored record of the ordinary issue id 9001
// number 17: every member the pinned issue schema requires, the intentional
// absence of state_reason, and the explicit nulls, false, zero and empty array
// the accepted examples declare.
const authoredIssue9001 = `{` +
	`"id":9001,` +
	`"node_id":"I_kwDOA9001",` +
	`"url":"https://api.github.com/repos/acme/widgets/issues/17",` +
	`"repository_url":"https://api.github.com/repos/acme/widgets",` +
	`"labels_url":"https://api.github.com/repos/acme/widgets/issues/17/labels{/name}",` +
	`"comments_url":"https://api.github.com/repos/acme/widgets/issues/17/comments",` +
	`"events_url":"https://api.github.com/repos/acme/widgets/issues/17/events",` +
	`"html_url":"https://github.com/acme/widgets/issues/17",` +
	`"number":17,` +
	`"state":"open",` +
	`"title":"Widget list omits archived entries",` +
	`"body":"The archived widget never appears in the list.",` +
	`"user":` + authoredUserObject + `,` +
	`"labels":[` + authoredLabelObject + `],` +
	`"assignee":null,` +
	`"assignees":[],` +
	`"milestone":null,` +
	`"locked":false,` +
	`"active_lock_reason":null,` +
	`"comments":0,` +
	`"closed_at":null,` +
	`"created_at":"2026-01-05T04:15:00Z",` +
	`"updated_at":"2026-01-06T09:30:00Z",` +
	`"author_association":"OWNER"` +
	`}`

// authoredPullRequest9002 is the exact authored record of the pull-request-shaped
// entry id 9002 number 18.
const authoredPullRequest9002 = `{` +
	`"id":9002,` +
	`"node_id":"I_kwDOA9002",` +
	`"url":"https://api.github.com/repos/acme/widgets/issues/18",` +
	`"repository_url":"https://api.github.com/repos/acme/widgets",` +
	`"labels_url":"https://api.github.com/repos/acme/widgets/issues/18/labels{/name}",` +
	`"comments_url":"https://api.github.com/repos/acme/widgets/issues/18/comments",` +
	`"events_url":"https://api.github.com/repos/acme/widgets/issues/18/events",` +
	`"html_url":"https://github.com/acme/widgets/pull/18",` +
	`"number":18,` +
	`"state":"open",` +
	`"title":"Fix widget",` +
	`"body":null,` +
	`"user":` + authoredUserObject + `,` +
	`"labels":[],` +
	`"assignee":null,` +
	`"assignees":[],` +
	`"milestone":null,` +
	`"locked":false,` +
	`"active_lock_reason":null,` +
	`"comments":0,` +
	`"closed_at":null,` +
	`"created_at":"2026-01-07T11:00:00Z",` +
	`"updated_at":"2026-01-08T12:45:00Z",` +
	`"author_association":"OWNER",` +
	`"pull_request":` + authoredPullRequestObject + `,` +
	`"draft":false` +
	`}`

// authoredIssue9003 is the exact authored record of the ordinary issue id 9003
// number 19 that page two of the authored source holds.
const authoredIssue9003 = `{` +
	`"id":9003,` +
	`"node_id":"I_kwDOA9003",` +
	`"url":"https://api.github.com/repos/acme/widgets/issues/19",` +
	`"repository_url":"https://api.github.com/repos/acme/widgets",` +
	`"labels_url":"https://api.github.com/repos/acme/widgets/issues/19/labels{/name}",` +
	`"comments_url":"https://api.github.com/repos/acme/widgets/issues/19/comments",` +
	`"events_url":"https://api.github.com/repos/acme/widgets/issues/19/events",` +
	`"html_url":"https://github.com/acme/widgets/issues/19",` +
	`"number":19,` +
	`"state":"open",` +
	`"title":"Add widget pagination",` +
	`"body":null,` +
	`"user":` + authoredUserObject + `,` +
	`"labels":[],` +
	`"assignee":null,` +
	`"assignees":[],` +
	`"milestone":null,` +
	`"locked":false,` +
	`"active_lock_reason":null,` +
	`"comments":0,` +
	`"closed_at":null,` +
	`"created_at":"2026-01-09T05:20:00Z",` +
	`"updated_at":"2026-01-10T06:05:00Z",` +
	`"author_association":"OWNER"` +
	`}`

// authoredPageOne, authoredPageTwo and authoredClampedPage are the exact bodies
// the authored acme/widgets source serves.
var (
	authoredPageOne     = `[` + authoredIssue9001 + `,` + authoredPullRequest9002 + `]`
	authoredPageTwo     = `[` + authoredIssue9003 + `]`
	authoredClampedPage = authoredPageOne
	authoredEmptyPage   = `[]`
)

// authoredNextLink is the continuation page one carries.
const authoredNextLink = `<https://api.github.com/repos/acme/widgets/issues?per_page=2&page=2>; rel="next"`

// TestAuthoredPagesAndContinuationAreServedExactly proves that the authored pages
// and their continuation are served byte for byte and that the authored records
// keep their exact identifiers, nesting and presence.
func TestAuthoredPagesAndContinuationAreServedExactly(t *testing.T) {
	t.Parallel()
	running := start(t, nil)

	t.Run("page_one_is_the_authored_page_with_the_authored_next_link", func(t *testing.T) {
		served := perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=1", nil)
		requireStatus(t, served, http.StatusOK)
		requireBody(t, served, authoredPageOne)
		requireHeader(t, served, "Content-Type", jsonMediaType)
		requireHeader(t, served, "Content-Length", strconv.Itoa(len(authoredPageOne)))
		requireHeader(t, served, "Link", authoredNextLink)
	})

	t.Run("page_two_ends_the_authored_source", func(t *testing.T) {
		served := perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=2", nil)
		requireStatus(t, served, http.StatusOK)
		requireBody(t, served, authoredPageTwo)
		requireAbsentHeader(t, served, "Link")
	})

	t.Run("records_preserve_exact_identifiers_null_false_zero_and_empty_array", func(t *testing.T) {
		records := decodePage(t, perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=1", nil).body)
		if len(records) != 2 {
			t.Fatalf("served page holds %d records, want 2", len(records))
		}
		issue := records[0]
		requireField(t, issue, "id", "9001")
		requireField(t, issue, "number", "17")
		requireField(t, issue, "repository_url", `"https://api.github.com/repos/acme/widgets"`)
		requireField(t, issue, "locked", "false")
		requireField(t, issue, "comments", "0")
		requireField(t, issue, "assignees", "[]")
		requireField(t, issue, "assignee", "null")
		requireField(t, issue, "milestone", "null")
		requireField(t, issue, "closed_at", "null")
		requireField(t, issue, "state", `"open"`)
		absentField(t, issue, "state_reason")
		absentField(t, issue, "pull_request")
		user := decodeObject(t, field(t, issue, "user"))
		requireField(t, user, "login", `"ada"`)
		requireField(t, user, "email", "null")
		labels := decodeArray(t, field(t, issue, "labels"))
		if len(labels) != 1 {
			t.Fatalf("authored issue carries %d labels, want 1", len(labels))
		}
		requireField(t, labels[0], "name", `"bug"`)
		second := decodePage(t, perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=2", nil).body)[0]
		requireField(t, second, "id", "9003")
		requireField(t, second, "number", "19")
	})

	t.Run("pull_request_entry_keeps_its_discriminator_and_nested_values", func(t *testing.T) {
		records := decodePage(t, perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=1", nil).body)
		pull := records[1]
		requireField(t, pull, "id", "9002")
		requireField(t, pull, "number", "18")
		requireField(t, pull, "title", `"Fix widget"`)
		requireField(t, pull, "body", "null")
		pullRequest := decodeObject(t, field(t, pull, "pull_request"))
		requireField(t, pullRequest, "url", `"https://api.github.com/repos/acme/widgets/pulls/18"`)
		requireField(t, pullRequest, "merged_at", "null")
	})

	t.Run("provider_clamp_honours_two_records_and_names_the_clamped_size", func(t *testing.T) {
		running := start(t, fixture("fixture.github.issues.provider-clamp"))
		served := perform(t, running, http.MethodGet, fixtureTarget+"?per_page=5&page=1", nil)
		requireStatus(t, served, http.StatusOK)
		requireBody(t, served, authoredClampedPage)
		requireHeader(t, served, "Link", `<https://api.github.com/repos/acme/widgets/issues?per_page=2&page=2>; rel="next"`)
	})

	t.Run("five_record_page_keeps_the_unread_tail", func(t *testing.T) {
		running := start(t, fixture("fixture.github.issues.five-record-page"))
		served := perform(t, running, http.MethodGet, fixtureTarget+"?per_page=5&page=1", nil)
		requireStatus(t, served, http.StatusOK)
		records := decodePage(t, served.body)
		if len(records) != 5 {
			t.Fatalf("five-record page holds %d records, want 5", len(records))
		}
		for index, want := range []string{"9001", "9002", "9003", "9004", "9005"} {
			requireField(t, records[index], "id", want)
		}
		requireHeader(t, served, "Link", `<https://api.github.com/repos/acme/widgets/issues?per_page=5&page=2>; rel="next"`)
	})

	t.Run("page_beyond_the_authored_source_is_an_empty_array", func(t *testing.T) {
		served := perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=9", nil)
		requireStatus(t, served, http.StatusOK)
		requireBody(t, served, authoredEmptyPage)
		requireAbsentHeader(t, served, "Link")
	})
}

// TestFaultFixturesProduceDocumentedResponses proves that every documented
// fixture fault and refusal serves exactly the response it documents.
func TestFaultFixturesProduceDocumentedResponses(t *testing.T) {
	t.Parallel()

	t.Run("wrong_record_id", func(t *testing.T) {
		running := start(t, fixture("fixture.github.issues.wrong-record-id"))
		served := perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=1", nil)
		requireStatus(t, served, http.StatusOK)
		requireBody(t, served, replace(t, authoredPageOne, `"id":9001`, `"id":9101`))
		requireHeader(t, served, "Link", authoredNextLink)
	})

	t.Run("wrong_record_number", func(t *testing.T) {
		running := start(t, fixture("fixture.github.issues.wrong-record-number"))
		served := perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=1", nil)
		requireStatus(t, served, http.StatusOK)
		requireBody(t, served, replace(t, authoredPageOne, `"number":17`, `"number":71`))
	})

	t.Run("wrong_record_repository", func(t *testing.T) {
		running := start(t, fixture("fixture.github.issues.wrong-record-repository"))
		served := perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=1", nil)
		requireStatus(t, served, http.StatusOK)
		requireBody(t, served, replace(t, authoredPageOne,
			`"repository_url":"https://api.github.com/repos/acme/widgets"`,
			`"repository_url":"https://api.github.com/repos/acme/gadgets"`))
	})

	t.Run("missing_required_field_defaults_to_node_id", func(t *testing.T) {
		running := start(t, fixture("fixture.github.issues.missing-required-field"))
		served := perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=1", nil)
		requireStatus(t, served, http.StatusOK)
		requireBody(t, served, replace(t, authoredPageOne, `"node_id":"I_kwDOA9001",`, ""))
	})

	t.Run("missing_required_field_follows_the_declared_member", func(t *testing.T) {
		running := start(t, func(configuration *githubcompat.Config) {
			fixture("fixture.github.issues.missing-required-field")(configuration)
			configuration.FaultField = "title"
		})
		served := perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=1", nil)
		requireStatus(t, served, http.StatusOK)
		requireBody(t, served, replace(t, authoredPageOne,
			`"title":"Widget list omits archived entries",`, ""))
	})

	t.Run("wrong_field_type_defaults_to_comments", func(t *testing.T) {
		running := start(t, fixture("fixture.github.issues.wrong-field-type"))
		served := perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=1", nil)
		requireStatus(t, served, http.StatusOK)
		requireBody(t, served, replace(t, authoredPageOne, `"comments":0`, `"comments":"wrong-type"`))
	})

	t.Run("wrong_field_type_follows_the_declared_member", func(t *testing.T) {
		running := start(t, func(configuration *githubcompat.Config) {
			fixture("fixture.github.issues.wrong-field-type")(configuration)
			configuration.FaultField = "title"
		})
		served := perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=1", nil)
		requireStatus(t, served, http.StatusOK)
		requireBody(t, served, replace(t, authoredPageOne,
			`"title":"Widget list omits archived entries"`, `"title":7`))
	})

	t.Run("malformed_continuation_token", func(t *testing.T) {
		running := start(t, fixture("fixture.github.issues.malformed-continuation"))
		served := perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=1", nil)
		requireStatus(t, served, http.StatusOK)
		requireBody(t, served, authoredPageOne)
		requireHeader(t, served, "Link", `<https://api.github.com/repos/acme/widgets/issues?per_page=2&page=two>; rel="next"`)
	})

	t.Run("foreign_host_continuation", func(t *testing.T) {
		running := start(t, fixture("fixture.github.issues.foreign-host-continuation"))
		served := perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=1", nil)
		requireStatus(t, served, http.StatusOK)
		requireBody(t, served, authoredPageOne)
		requireHeader(t, served, "Link", `<https://git.example.com/repos/acme/widgets/issues?per_page=2&page=2>; rel="next"`)
	})

	t.Run("redirected_continuation", func(t *testing.T) {
		running := start(t, fixture("fixture.github.issues.redirected-continuation"))
		served := perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=1", nil)
		requireStatus(t, served, http.StatusFound)
		requireBody(t, served, "")
		requireHeader(t, served, "Location", "https://127.0.0.1:9/repos/acme/widgets/issues?per_page=2&page=2")
		requireAbsentHeader(t, served, "Link")
	})

	t.Run("not_found", func(t *testing.T) {
		running := start(t, fixture("fixture.github.issues.not-found"))
		served := perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=1", nil)
		requireStatus(t, served, http.StatusNotFound)
		requireBody(t, served, notFoundDocument)
		requireHeader(t, served, "Content-Type", jsonMediaType)
	})

	t.Run("unprocessable", func(t *testing.T) {
		running := start(t, fixture("fixture.github.issues.unprocessable"))
		served := perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=1", nil)
		requireStatus(t, served, http.StatusUnprocessableEntity)
		requireBody(t, served, validationFailedDocument)
	})

	t.Run("undeclared_repository_is_not_served", func(t *testing.T) {
		running := start(t, nil)
		for _, target := range []string{"/repos/other/widgets/issues", "/repos/acme/gadgets/issues"} {
			served := perform(t, running, http.MethodGet, target+"?per_page=2&page=1", nil)
			requireStatus(t, served, http.StatusNotFound)
			requireBody(t, served, notFoundDocument)
		}
	})

	t.Run("undeclared_route_is_not_served", func(t *testing.T) {
		running := start(t, nil)
		served := perform(t, running, http.MethodGet, "/repos/acme/widgets", nil)
		requireStatus(t, served, http.StatusNotFound)
		requireBody(t, served, notFoundDocument)
	})

	t.Run("undeclared_method_is_not_served", func(t *testing.T) {
		running := start(t, nil)
		served := perform(t, running, http.MethodPost, fixtureTarget, nil)
		requireStatus(t, served, http.StatusMethodNotAllowed)
		requireBody(t, served, methodNotAllowedDocument)
		requireHeader(t, served, "Allow", http.MethodGet)
	})

	t.Run("refused_query_values_name_their_field", func(t *testing.T) {
		running := start(t, nil)
		for _, refused := range []struct {
			target string
			body   string
		}{
			{
				target: fixtureTarget + "?per_page=2&page=two",
				body:   validationFailedDocument,
			},
			{
				target: fixtureTarget + "?per_page=0&page=1",
				body:   `{"message":"Validation Failed","errors":[{"resource":"Issue","field":"per_page","code":"invalid"}],"documentation_url":"https://docs.github.com/rest"}`,
			},
			{
				target: fixtureTarget + "?per_page=2&per_page=3",
				body:   `{"message":"Validation Failed","errors":[{"resource":"Issue","field":"per_page","code":"invalid"}],"documentation_url":"https://docs.github.com/rest"}`,
			},
			{
				target: fixtureTarget + "?per_page=101&page=1",
				body:   `{"message":"Validation Failed","errors":[{"resource":"Issue","field":"per_page","code":"invalid"}],"documentation_url":"https://docs.github.com/rest"}`,
			},
		} {
			served := perform(t, running, http.MethodGet, refused.target, nil)
			requireStatus(t, served, http.StatusUnprocessableEntity)
			requireBody(t, served, refused.body)
		}
	})
}

// TestJournalRecordsExactlyOneEntryPerServedRequest proves that the journal holds
// exactly one strictly decodable entry per served request, in arrival order, with
// the received target, the served status and the hash of the exact served bytes.
func TestJournalRecordsExactlyOneEntryPerServedRequest(t *testing.T) {
	t.Parallel()
	running := start(t, nil)

	if _, err := os.Stat(running.facts.JournalPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("journal exists before the first served request: %v", err)
	}

	accept := map[string]string{"Accept": "application/vnd.github+json"}
	first := perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=1", accept)
	second := perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=2", accept)
	third := perform(t, running, http.MethodGet, "/repos/other/widgets/issues", accept)

	lines := journalLines(t, running.facts.JournalPath)
	if len(lines) != 3 {
		t.Fatalf("journal holds %d entries for 3 served requests", len(lines))
	}
	want := []struct {
		target string
		status int
		body   []byte
		link   string
	}{
		{fixtureTarget + "?per_page=2&page=1", http.StatusOK, []byte(authoredPageOne), authoredNextLink},
		{fixtureTarget + "?per_page=2&page=2", http.StatusOK, []byte(authoredPageTwo), ""},
		{"/repos/other/widgets/issues", http.StatusNotFound, []byte(notFoundDocument), ""},
	}
	for index, line := range lines {
		entry := decodeRunnerEntry(t, line)
		if entry.Target != want[index].target || entry.Status != want[index].status {
			t.Fatalf("journal entry %d = %d %q, want %d %q", index, entry.Status, entry.Target, want[index].status, want[index].target)
		}
		if entry.Method != http.MethodGet {
			t.Fatalf("journal entry %d records method %q", index, entry.Method)
		}
		if entry.MediaType != "application/vnd.github+json" {
			t.Fatalf("journal entry %d records media type %q", index, entry.MediaType)
		}
		if entry.ResponseMediaType != jsonMediaType {
			t.Fatalf("journal entry %d records response media type %q", index, entry.ResponseMediaType)
		}
		if entry.RequestBodySHA256 != nil {
			t.Fatalf("journal entry %d records a request body hash for a GET", index)
		}
		if entry.ResponseBodySHA256 == nil {
			t.Fatalf("journal entry %d records no response body hash", index)
		}
		if expected := contract.RawSourceHash(want[index].body); *entry.ResponseBodySHA256 != expected {
			t.Fatalf("journal entry %d hashes %s, want %s", index, *entry.ResponseBodySHA256, expected)
		}
		if entry.Redactions == nil || len(entry.Redactions) != 0 {
			t.Fatalf("journal entry %d records redactions %v", index, entry.Redactions)
		}
		if want[index].status == http.StatusOK {
			if got := recordedQueryValue(t, entry, "per_page"); got != "2" {
				t.Fatalf("journal entry %d records per_page %q, want the received text", index, got)
			}
		}
		if got, present := recordedRequestHeader(t, entry, "accept"); !present || got != "application/vnd.github+json" {
			t.Fatalf("journal entry %d records accept %q (present %t)", index, got, present)
		}
		if got, present := recordedResponseHeader(t, entry, "content-type"); !present || got != jsonMediaType {
			t.Fatalf("journal entry %d records content-type %q (present %t)", index, got, present)
		}
		if got, present := recordedResponseHeader(t, entry, "link"); present != (want[index].link != "") || (present && got != want[index].link) {
			t.Fatalf("journal entry %d records link %q (present %t)", index, got, present)
		}
	}
	if got := recordedQueryValue(t, decodeRunnerEntry(t, lines[0]), "page"); got != "1" {
		t.Fatalf("journal entry 0 records page %q, want the received text", got)
	}
	requireStatus(t, first, http.StatusOK)
	requireStatus(t, second, http.StatusOK)
	requireStatus(t, third, http.StatusNotFound)

	t.Run("concurrent_requests_each_get_exactly_one_entry", func(t *testing.T) {
		running := start(t, nil)
		const requests = 8
		var wait sync.WaitGroup
		failures := make(chan error, requests)
		for index := range requests {
			wait.Add(1)
			go func(index int) {
				defer wait.Done()
				target := fixtureTarget + "?per_page=2&page=" + strconv.Itoa(index+1)
				request, err := http.NewRequest(http.MethodGet, "https://"+running.facts.Listen+target, nil)
				if err != nil {
					failures <- err
					return
				}
				response, err := running.client.Do(request)
				if err != nil {
					failures <- err
					return
				}
				_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
				closeErr := response.Body.Close()
				switch {
				case readErr != nil:
					failures <- readErr
				case closeErr != nil:
					failures <- closeErr
				case response.StatusCode != http.StatusOK:
					failures <- fmt.Errorf("served status %d for %s", response.StatusCode, target)
				}
			}(index)
		}
		wait.Wait()
		close(failures)
		for failure := range failures {
			t.Fatalf("concurrent controlled destination request failed: %v", failure)
		}
		lines := journalLines(t, running.facts.JournalPath)
		if len(lines) != requests {
			t.Fatalf("journal holds %d entries for %d concurrent served requests", len(lines), requests)
		}
		targets := make(map[string]int, requests)
		for _, line := range lines {
			targets[decodeRunnerEntry(t, line).Target]++
		}
		for index := range requests {
			target := fixtureTarget + "?per_page=2&page=" + strconv.Itoa(index+1)
			if targets[target] != 1 {
				t.Fatalf("journal records target %q %d times, want exactly once", target, targets[target])
			}
		}
	})

	t.Run("journal_is_created_by_the_first_served_request", func(t *testing.T) {
		running := start(t, nil)
		perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=1", nil)
		information, err := os.Stat(running.facts.JournalPath)
		if err != nil {
			t.Fatalf("inspect journal after the first served request: %v", err)
		}
		if !information.Mode().IsRegular() || information.Size() <= 0 {
			t.Fatal("journal is not a nonempty regular file after the first served request")
		}
		if information.Mode().Perm()&0o077 != 0 {
			t.Fatalf("journal mode %v is readable beyond its owner", information.Mode().Perm())
		}
	})
}

// TestJournalRedactsSecretBearingRequestFields proves that provider
// authentication material is never recorded as an ordinary field: it becomes an
// excluded redaction, and neither the value nor its name reaches the journal.
func TestJournalRedactsSecretBearingRequestFields(t *testing.T) {
	t.Parallel()
	running := start(t, nil)
	const (
		authorizationSecret = "Bearer ghp_fixturecredential0001"
		cookieSecret        = "pm-session=fixturecookie0002"
		querySecret         = "fixturetoken0003"
	)
	served := perform(t, running, http.MethodGet,
		fixtureTarget+"?per_page=2&page=1&access_token="+querySecret,
		map[string]string{
			"Authorization": authorizationSecret,
			"Cookie":        cookieSecret,
			"Accept":        "application/vnd.github+json",
		},
	)
	requireStatus(t, served, http.StatusOK)
	requireBody(t, served, authoredPageOne)

	lines := journalLines(t, running.facts.JournalPath)
	if len(lines) != 1 {
		t.Fatalf("journal holds %d entries for 1 served request", len(lines))
	}
	for _, secret := range []string{authorizationSecret, cookieSecret, querySecret} {
		if strings.Contains(lines[0], secret) {
			t.Fatalf("journal records the secret %q", secret)
		}
	}
	entry := decodeRunnerEntry(t, lines[0])
	if entry.Target != fixtureTarget+"?per_page=2&page=1&access_token=redacted" {
		t.Fatalf("journal target = %q, want the received target with the secret value replaced", entry.Target)
	}
	want := []contract.ProofRedaction{
		{Pointer: "/headers/authorization", Replacement: "excluded"},
		{Pointer: "/headers/cookie", Replacement: "excluded"},
		{Pointer: "/query/access_token", Replacement: "excluded"},
	}
	if len(entry.Redactions) != len(want) {
		t.Fatalf("journal records %d redactions, want %d: %v", len(entry.Redactions), len(want), entry.Redactions)
	}
	for index, redaction := range want {
		if entry.Redactions[index] != redaction {
			t.Fatalf("redaction %d = %+v, want %+v", index, entry.Redactions[index], redaction)
		}
	}
	if _, present := recordedRequestHeader(t, entry, "authorization"); present {
		t.Fatal("journal records the authorization header as an ordinary field")
	}
	if _, present := recordedRequestHeader(t, entry, "cookie"); present {
		t.Fatal("journal records the cookie header as an ordinary field")
	}
	if got := recordedQueryValue(t, entry, "access_token"); got != "" {
		t.Fatalf("journal records the access_token query value %q", got)
	}
	if got := recordedQueryValue(t, entry, "page"); got != "1" {
		t.Fatalf("journal records page %q, want the received text", got)
	}
	if entry.MediaType != "application/vnd.github+json" {
		t.Fatalf("journal records media type %q, want the declared Accept preference", entry.MediaType)
	}
}

// TestTLSChainsOnlyUnderTheDeclaredAuthorityAndNames proves that host name and
// chain verification are satisfied by the declared generated root and by nothing
// else, that the declared names cover the admitted origin, and that the declared
// root survives a restart while the derived server certificate follows the
// declared names.
func TestTLSChainsOnlyUnderTheDeclaredAuthorityAndNames(t *testing.T) {
	t.Parallel()
	running := start(t, nil)

	t.Run("declared_authority_verifies_chain_and_origin_host_name", func(t *testing.T) {
		served := perform(t, running, http.MethodGet, fixtureTarget+"?per_page=2&page=1", nil)
		requireStatus(t, served, http.StatusOK)
		if served.connection == nil || len(served.connection.PeerCertificates) == 0 {
			t.Fatal("served response carries no verified peer certificate")
		}
		certificate := served.connection.PeerCertificates[0]
		if err := certificate.VerifyHostname("api.github.com"); err != nil {
			t.Fatalf("server certificate does not cover the admitted origin: %v", err)
		}
		if err := certificate.VerifyHostname("127.0.0.1"); err != nil {
			t.Fatalf("server certificate does not cover the declared loopback address: %v", err)
		}
		if err := certificate.VerifyHostname("undeclared.example.test"); err == nil {
			t.Fatal("server certificate covers an undeclared host name")
		}
		authority, err := os.ReadFile(running.facts.CAPath)
		if err != nil {
			t.Fatalf("read declared trust authority: %v", err)
		}
		parsed, err := parseCertificate(t, authority)
		if err != nil {
			t.Fatalf("parse declared trust authority: %v", err)
		}
		if !parsed.IsCA || !parsed.BasicConstraintsValid {
			t.Fatal("declared trust authority is not a certificate authority")
		}
		if err := certificate.CheckSignatureFrom(parsed); err != nil {
			t.Fatalf("server certificate is not signed by the declared trust authority: %v", err)
		}
	})

	t.Run("system_roots_cannot_verify_the_generated_certificate", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodGet, "https://"+running.facts.Listen+fixtureTarget, nil)
		if err != nil {
			t.Fatalf("construct request: %v", err)
		}
		client := &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				Proxy:           nil,
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			t.Fatal("a client that trusts only the system roots completed the handshake")
		}
		var verificationError *tls.CertificateVerificationError
		if !errors.As(err, &verificationError) {
			t.Fatalf("system root verification failed with %v, want a certificate verification failure", err)
		}
	})

	t.Run("an_undeclared_host_name_is_refused", func(t *testing.T) {
		client := clientTrusting(t, running.facts.CAPath, "undeclared.example.test")
		request, err := http.NewRequest(http.MethodGet, "https://"+running.facts.Listen+fixtureTarget, nil)
		if err != nil {
			t.Fatalf("construct request: %v", err)
		}
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			t.Fatal("host name verification passed for an undeclared name")
		}
		var hostnameError x509.HostnameError
		if !errors.As(err, &hostnameError) {
			t.Fatalf("host name verification failed with %v, want a host name error", err)
		}
	})

	t.Run("private_keys_are_not_group_or_world_readable", func(t *testing.T) {
		for _, name := range []string{"ca-key.pem", "server-key.pem"} {
			information, err := os.Stat(filepath.Join(filepath.Dir(running.facts.CAPath), name))
			if err != nil {
				t.Fatalf("inspect %s: %v", name, err)
			}
			if information.Mode().Perm()&0o077 != 0 {
				t.Fatalf("%s mode %v is readable beyond its owner", name, information.Mode().Perm())
			}
		}
	})

	t.Run("authority_survives_a_restart_and_the_leaf_follows_declared_names", func(t *testing.T) {
		directory := t.TempDir()
		configuration := func(hosts []string) func(*githubcompat.Config) {
			return func(configuration *githubcompat.Config) {
				configuration.TLSDir = filepath.Join(directory, "tls")
				configuration.Journal = filepath.Join(directory, "journal.jsonl")
				configuration.Hosts = hosts
			}
		}
		first := start(t, configuration(nil))
		firstAuthority := readFile(t, first.facts.CAPath)
		firstCertificate := peerCertificate(t, perform(t, first, http.MethodGet, fixtureTarget+"?per_page=2&page=1", nil))
		first.stop()
		second := start(t, configuration([]string{"extra.example.test"}))
		secondAuthority := readFile(t, second.facts.CAPath)
		secondCertificate := peerCertificate(t, perform(t, second, http.MethodGet, fixtureTarget+"?per_page=2&page=1", nil))
		if string(firstAuthority) != string(secondAuthority) {
			t.Fatal("a restart replaced the declared trust authority")
		}
		if err := secondCertificate.VerifyHostname("extra.example.test"); err != nil {
			t.Fatalf("reissued server certificate does not cover a declared name: %v", err)
		}
		if err := firstCertificate.VerifyHostname("extra.example.test"); err == nil {
			t.Fatal("the first server certificate covers a name that was not declared for it")
		}
	})

	t.Run("an_unparsable_declared_authority_is_refused_unchanged", func(t *testing.T) {
		directory := t.TempDir()
		tlsDirectory := filepath.Join(directory, "tls")
		configuration := githubcompat.Config{
			Listen:  "127.0.0.1:0",
			Origin:  fixtureOrigin,
			TLSDir:  tlsDirectory,
			Journal: filepath.Join(directory, "journal.jsonl"),
			Fixture: githubcompat.DefaultFixture,
		}
		if _, err := githubcompat.Prepare(configuration); err != nil {
			t.Fatalf("prepare controlled destination: %v", err)
		}
		declared := filepath.Join(tlsDirectory, "ca.pem")
		replaced := []byte("not a certificate")
		if err := os.WriteFile(declared, replaced, 0o644); err != nil {
			t.Fatalf("replace the declared trust authority: %v", err)
		}
		if _, err := githubcompat.Prepare(configuration); err == nil {
			t.Fatal("an unparsable declared trust authority was accepted")
		}
		if got := readFile(t, declared); string(got) != string(replaced) {
			t.Fatal("an unparsable declared trust authority was silently replaced")
		}
	})
}

// TestConfigurationRefusals proves that a refused declaration fails closed with a
// configuration error instead of serving something else.
func TestConfigurationRefusals(t *testing.T) {
	t.Parallel()
	for _, refused := range []struct {
		name   string
		mutate func(*githubcompat.Config)
	}{
		{
			name: "relative_journal_path",
			mutate: func(configuration *githubcompat.Config) {
				configuration.Journal = "journal.jsonl"
			},
		},
		{
			name: "missing_journal_directory",
			mutate: func(configuration *githubcompat.Config) {
				configuration.Journal = filepath.Join(configuration.TLSDir, "absent", "journal.jsonl")
			},
		},
		{
			name: "journal_path_is_a_directory",
			mutate: func(configuration *githubcompat.Config) {
				configuration.Journal = filepath.Dir(configuration.TLSDir)
			},
		},
		{
			name: "relative_tls_directory",
			mutate: func(configuration *githubcompat.Config) {
				configuration.TLSDir = "tls"
			},
		},
		{
			name: "unsupported_fixture",
			mutate: func(configuration *githubcompat.Config) {
				configuration.Fixture = "fixture.github.issues.unwritten"
			},
		},
		{
			name: "fault_field_without_a_field_fault",
			mutate: func(configuration *githubcompat.Config) {
				configuration.FaultField = "node_id"
			},
		},
		{
			name: "fault_field_outside_the_required_members",
			mutate: func(configuration *githubcompat.Config) {
				configuration.Fixture = "fixture.github.issues.missing-required-field"
				configuration.FaultField = "reactions"
			},
		},
		{
			name: "origin_without_tls",
			mutate: func(configuration *githubcompat.Config) {
				configuration.Origin = "http://api.github.com"
			},
		},
		{
			name: "origin_with_a_path",
			mutate: func(configuration *githubcompat.Config) {
				configuration.Origin = "https://api.github.com/api/v3"
			},
		},
		{
			name: "listen_address_without_a_port",
			mutate: func(configuration *githubcompat.Config) {
				configuration.Listen = "127.0.0.1"
			},
		},
	} {
		t.Run(refused.name, func(t *testing.T) {
			directory := t.TempDir()
			configuration := githubcompat.Config{
				Listen:  "127.0.0.1:0",
				Origin:  fixtureOrigin,
				TLSDir:  filepath.Join(directory, "tls"),
				Journal: filepath.Join(directory, "journal.jsonl"),
				Fixture: githubcompat.DefaultFixture,
			}
			refused.mutate(&configuration)
			destination, err := githubcompat.Prepare(configuration)
			if err == nil {
				t.Fatalf("refused configuration was accepted: %+v", destination)
			}
			if !errors.Is(err, githubcompat.ErrConfiguration) {
				t.Fatalf("refused configuration failed with %v, want a configuration error", err)
			}
		})
	}
}

// running is one served controlled destination under test.
type running struct {
	facts  githubcompat.Facts
	client *http.Client
	stop   func()
}

// served is one observed response of the controlled destination.
type served struct {
	status     int
	header     http.Header
	body       []byte
	connection *tls.ConnectionState
}

// fixture selects one authored fixture through the exported configuration.
func fixture(value string) func(*githubcompat.Config) {
	return func(configuration *githubcompat.Config) {
		configuration.Fixture = value
	}
}

// start prepares and serves one controlled destination in a private directory and
// returns its resolved facts, a client that trusts exactly the generated
// authority, and an idempotent stop function.
func start(t *testing.T, mutate func(*githubcompat.Config)) running {
	t.Helper()
	directory := t.TempDir()
	configuration := githubcompat.Config{
		Listen:  "127.0.0.1:0",
		Origin:  fixtureOrigin,
		TLSDir:  filepath.Join(directory, "tls"),
		Journal: filepath.Join(directory, "journal.jsonl"),
		Fixture: githubcompat.DefaultFixture,
	}
	if mutate != nil {
		mutate(&configuration)
	}
	destination, err := githubcompat.Prepare(configuration)
	if err != nil {
		t.Fatalf("prepare controlled destination: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	announced := make(chan githubcompat.Facts, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		if err := destination.Run(ctx, func(facts githubcompat.Facts) { announced <- facts }); err != nil {
			t.Errorf("run controlled destination: %v", err)
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-stopped:
			case <-time.After(10 * time.Second):
				t.Error("controlled destination did not stop")
			}
		})
	}
	t.Cleanup(stop)
	select {
	case facts := <-announced:
		return running{facts: facts, client: clientTrusting(t, facts.CAPath, ""), stop: stop}
	case <-stopped:
		t.Fatal("controlled destination stopped before it announced its bound address")
	case <-time.After(10 * time.Second):
		t.Fatal("controlled destination did not announce its bound address")
	}
	return running{}
}

// clientTrusting returns an HTTPS client that trusts exactly the declared
// authority. An empty host name verifies the address from the request URL.
func clientTrusting(t *testing.T, caPath, hostName string) *http.Client {
	t.Helper()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(readFile(t, caPath)) {
		t.Fatal("declared trust authority is not a usable certificate")
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    roots,
				ServerName: hostName,
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// perform executes one request against the served destination with a client that
// trusts exactly the declared authority and returns the observed response.
func perform(t *testing.T, running running, method, target string, headers map[string]string) served {
	t.Helper()
	request, err := http.NewRequest(method, "https://"+running.facts.Listen+target, nil)
	if err != nil {
		t.Fatalf("construct controlled destination request: %v", err)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := running.client.Do(request)
	if err != nil {
		t.Fatalf("execute controlled destination request: %v", err)
	}
	defer response.Body.Close()
	document, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatalf("read controlled destination response: %v", err)
	}
	return served{
		status:     response.StatusCode,
		header:     response.Header,
		body:       document,
		connection: response.TLS,
	}
}

// readFile returns one declared file this test inspects.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	document, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return document
}

// parseCertificate parses one PEM certificate document.
func parseCertificate(t *testing.T, document []byte) (*x509.Certificate, error) {
	t.Helper()
	block, _ := pem.Decode(document)
	if block == nil {
		return nil, errors.New("document is not a PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

// peerCertificate returns the verified peer certificate of one served response.
func peerCertificate(t *testing.T, served served) *x509.Certificate {
	t.Helper()
	if served.connection == nil || len(served.connection.PeerCertificates) == 0 {
		t.Fatal("served response carries no verified peer certificate")
	}
	return served.connection.PeerCertificates[0]
}

// requireStatus refuses an unexpected served status.
func requireStatus(t *testing.T, served served, want int) {
	t.Helper()
	if served.status != want {
		t.Fatalf("served status = %d, want %d", served.status, want)
	}
}

// requireBody refuses a served body that is not exactly the authored document.
func requireBody(t *testing.T, served served, want string) {
	t.Helper()
	if string(served.body) != want {
		t.Fatalf("served body =\n%s\nwant\n%s", served.body, want)
	}
}

// requireHeader refuses an unexpected served header.
func requireHeader(t *testing.T, served served, name, want string) {
	t.Helper()
	if got := served.header.Get(name); got != want {
		t.Fatalf("served %s = %q, want %q", name, got, want)
	}
}

// requireAbsentHeader refuses a served header that must not be present.
func requireAbsentHeader(t *testing.T, served served, name string) {
	t.Helper()
	if got := served.header.Get(name); got != "" {
		t.Fatalf("served %s = %q, want no such header", name, got)
	}
}

// replace refuses a served body that is not the authored document with one
// documented substitution applied.
func replace(t *testing.T, document, from, to string) string {
	t.Helper()
	if !strings.Contains(document, from) {
		t.Fatalf("authored document does not contain %q", from)
	}
	return strings.Replace(document, from, to, 1)
}

// decodePage decodes one served page into its records with exact JSON values.
func decodePage(t *testing.T, body []byte) []map[string]json.RawMessage {
	t.Helper()
	var records []map[string]json.RawMessage
	if err := json.Unmarshal(body, &records); err != nil {
		t.Fatalf("decode served page: %v", err)
	}
	return records
}

// requireField refuses an unexpected decoded JSON member.
func requireField(t *testing.T, object map[string]json.RawMessage, name, want string) {
	t.Helper()
	if got := string(field(t, object, name)); got != want {
		t.Fatalf("served record field %s = %s, want %s", name, got, want)
	}
}

// absentField refuses a decoded JSON member the authored record does not declare.
func absentField(t *testing.T, object map[string]json.RawMessage, name string) {
	t.Helper()
	if _, declared := object[name]; declared {
		t.Fatalf("served record declares %s, which the authored record does not", name)
	}
}

// decodeObject decodes one nested JSON object member.
func decodeObject(t *testing.T, document string) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(document), &object); err != nil {
		t.Fatalf("decode nested served object: %v", err)
	}
	return object
}

// decodeArray decodes one nested JSON array member.
func decodeArray(t *testing.T, document string) []map[string]json.RawMessage {
	t.Helper()
	var elements []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(document), &elements); err != nil {
		t.Fatalf("decode nested served array: %v", err)
	}
	return elements
}

// field returns one decoded JSON member.
func field(t *testing.T, object map[string]json.RawMessage, name string) string {
	t.Helper()
	value, declared := object[name]
	if !declared {
		t.Fatalf("served record does not declare %s", name)
	}
	return string(value)
}

// runnerEntry mirrors the proof runner's own journal entry wire shape, so
// decoding a journal line through the runner's member list and completeness rule
// is exactly what the runner performs before it accepts an observation.
type runnerEntry struct {
	Method             string                     `json:"method"`
	Target             string                     `json:"target"`
	MediaType          string                     `json:"media_type"`
	RequestBodySHA256  *contract.SourceHash       `json:"request_body_sha256,omitempty"`
	Status             int                        `json:"status"`
	ResponseMediaType  string                     `json:"response_media_type"`
	ResponseBodySHA256 *contract.SourceHash       `json:"response_body_sha256,omitempty"`
	RequestFields      map[string]json.RawMessage `json:"request_fields"`
	ResponseFields     map[string]json.RawMessage `json:"response_fields"`
	Redactions         []contract.ProofRedaction  `json:"redactions"`
}

// decodeRunnerEntry decodes one journal line exactly as the proof runner does.
func decodeRunnerEntry(t *testing.T, line string) runnerEntry {
	t.Helper()
	var entry runnerEntry
	if err := contract.DecodeStrictRequiredObject([]byte(line), &entry,
		"method", "target", "media_type", "status", "response_media_type",
		"request_fields", "response_fields", "redactions"); err != nil {
		t.Fatalf("decode controlled provider observation: %v", err)
	}
	if entry.Method == "" || entry.Target == "" || entry.Status < 100 || entry.Status > 599 {
		t.Fatalf("controlled provider observation is incomplete: %+v", entry)
	}
	return entry
}

// journalLines returns the nonempty lines of the declared journal. It refuses an
// absent or zero-byte journal, exactly as the proof runner does.
func journalLines(t *testing.T, path string) []string {
	t.Helper()
	information, err := os.Stat(path)
	if err != nil {
		t.Fatalf("inspect controlled provider observation journal: %v", err)
	}
	if !information.Mode().IsRegular() || information.Size() <= 0 {
		t.Fatal("controlled provider observation journal is not a nonempty regular file")
	}
	lines := make([]string, 0, 4)
	for _, line := range strings.Split(string(readFile(t, path)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// recordedQueryValue returns one recorded query value, empty when the query field
// is absent.
func recordedQueryValue(t *testing.T, entry runnerEntry, name string) string {
	t.Helper()
	var query map[string]json.RawMessage
	if err := json.Unmarshal(entry.RequestFields["query"], &query); err != nil {
		t.Fatalf("decode recorded query fields: %v", err)
	}
	value, declared := query[name]
	if !declared {
		return ""
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		t.Fatalf("decode recorded query value %s: %v", name, err)
	}
	return text
}

// recordedRequestHeader returns one recorded request header value and whether it
// was recorded at all.
func recordedRequestHeader(t *testing.T, entry runnerEntry, name string) (string, bool) {
	t.Helper()
	return recordedHeader(t, entry.RequestFields, name)
}

// recordedResponseHeader returns one recorded response header value and whether it
// was recorded at all.
func recordedResponseHeader(t *testing.T, entry runnerEntry, name string) (string, bool) {
	t.Helper()
	return recordedHeader(t, entry.ResponseFields, name)
}

// recordedHeader returns one recorded header value from one recorded field
// object.
func recordedHeader(t *testing.T, fields map[string]json.RawMessage, name string) (string, bool) {
	t.Helper()
	var headers map[string]json.RawMessage
	if err := json.Unmarshal(fields["headers"], &headers); err != nil {
		t.Fatalf("decode recorded header fields: %v", err)
	}
	value, declared := headers[name]
	if !declared {
		return "", false
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		t.Fatalf("decode recorded header value %s: %v", name, err)
	}
	return text, true
}
