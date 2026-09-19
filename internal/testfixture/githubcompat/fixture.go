// Package githubcompat is the controlled GitHub-compatible destination of the
// credential-free direct-read demo: an HTTPS server that serves the authored
// acme/widgets issue list from a generated test authority and records one
// independent provider observation journal entry per served request.
//
// It is fake evidence. It serves authored bytes, it never contacts GitHub, it
// holds no provider credential, and a passing case here never certifies live
// provider behavior. The generated authority is the only trust root that can
// complete the handshake: the destination reads no system or global trust
// store, changes none, and never disables peer or host name verification.
//
// Served responses are exactly the authored bytes: the same request always
// produces the same status, headers and body. The journal is the destination's
// own record of what it received and returned, in arrival order, and it never
// records provider authentication material as an ordinary field: a
// secret-bearing name is reported as an excluded redaction instead.
package githubcompat

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ErrConfiguration reports refused destination configuration: an unsupported
// fixture, a fault field the selected fixture does not use, a malformed origin,
// or a declared path that is not absolute, real and usable. A transport,
// filesystem or serving failure of otherwise valid configuration is not a
// configuration refusal.
var ErrConfiguration = errors.New("invalid controlled destination configuration")

const (
	// fixtureOwner and fixtureRepo are the authored repository identity. The
	// destination serves exactly this repository; any other repository path is
	// refused instead of answered with another repository's records.
	fixtureOwner = "acme"
	fixtureRepo  = "widgets"

	// fixtureRoute is the only route the destination serves, shaped like the
	// pinned source operation GET /repos/{owner}/{repo}/issues.
	fixtureRoute = "/repos/" + fixtureOwner + "/" + fixtureRepo + "/issues"

	// repositoryURL is the authored repository identity every served record
	// carries. It names the admitted provider origin, not the operator's
	// routing to this listener.
	repositoryURL = "https://api.github.com/repos/" + fixtureOwner + "/" + fixtureRepo

	// foreignOrigin is outside every admitted provider origin, so a client must
	// refuse a continuation that names it instead of following it.
	foreignOrigin = "https://git.example.com"

	// privateRedirectOrigin is a loopback destination the transport policy
	// refuses, so a redirected continuation can never be followed.
	privateRedirectOrigin = "https://127.0.0.1:9"

	// malformedPageToken is the deliberately non-numeric value of the
	// malformed-continuation fixture's next page token.
	malformedPageToken = "two"

	// clampLimit is the record count the provider-clamp fixture honours however
	// large a page the caller requests.
	clampLimit = 2

	// The pinned source declares per_page between 1 and 100 with a default of
	// 30, and page with a default of 1. maximumPage keeps the authored page
	// arithmetic inside a bounded range.
	defaultPageSize = 30
	minimumPageSize = 1
	maximumPageSize = 100
	minimumPage     = 1
	maximumPage     = 1_000_000

	// AuthoredFixturePrefix prefixes every authored fixture value that
	// connectors/github/scenarios/lifecycle.json names in a fixture
	// requirement.
	AuthoredFixturePrefix = "fixture.github.issues."

	// DefaultFixture is served when the fixture flag is absent: the authored
	// two-page acme/widgets issue list.
	DefaultFixture = AuthoredFixturePrefix + "two-pages"

	// defaultMissingField and defaultWrongTypeField are the documented required
	// members the two field faults select when no fault field is declared. Both
	// are required by the pinned issue schema and both sit outside the former
	// display-field subset.
	defaultMissingField   = "node_id"
	defaultWrongTypeField = "comments"
)

// requiredIssueFields is the complete required member list of the pinned issue
// schema (connectors/github/captures/github-rest-api-description,
// #/components/schemas/issue/required). Every authored record declares all of
// them, and a fault field may select only one of them.
var requiredIssueFields = []string{
	"assignee", "closed_at", "comments", "comments_url", "events_url",
	"html_url", "id", "node_id", "labels", "labels_url", "milestone", "number",
	"repository_url", "state", "locked", "title", "url", "user", "created_at",
	"updated_at",
}

// member is one authored name/value pair: either a JSON object member whose
// value is exact compact JSON text that is never re-encoded, so a served page is
// the authored page byte for byte, or one served response header.
type member struct {
	name  string
	value string
}

// authoredMember is the authored constructor of one JSON member. It keeps the
// authored record tables free of keyed literal alignment.
func authoredMember(name, value string) member {
	return member{name: name, value: value}
}

// record is one authored JSON object with its members in provider order.
type record []member

// object encodes the record as compact JSON.
func (entry record) object() string {
	var builder strings.Builder
	builder.WriteByte('{')
	for index, field := range entry {
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(quoted(field.name))
		builder.WriteByte(':')
		builder.WriteString(field.value)
	}
	builder.WriteByte('}')
	return builder.String()
}

// value returns the authored value of one member.
func (entry record) value(name string) (string, bool) {
	for _, field := range entry {
		if field.name == name {
			return field.value, true
		}
	}
	return "", false
}

// withValue returns a copy of the record with one member's value replaced. It
// refuses a member the record does not declare instead of silently adding one.
func (entry record) withValue(name, value string) (record, error) {
	if _, declared := entry.value(name); !declared {
		return nil, fmt.Errorf("authored record does not declare %s", name)
	}
	replaced := make(record, 0, len(entry))
	for _, field := range entry {
		if field.name == name {
			replaced = append(replaced, authoredMember(name, value))
			continue
		}
		replaced = append(replaced, field)
	}
	return replaced, nil
}

// withoutValue returns a copy of the record without one member. It refuses a
// member the record does not declare.
func (entry record) withoutValue(name string) (record, error) {
	if _, declared := entry.value(name); !declared {
		return nil, fmt.Errorf("authored record does not declare %s", name)
	}
	removed := make(record, 0, len(entry))
	for _, field := range entry {
		if field.name == name {
			continue
		}
		removed = append(removed, field)
	}
	return removed, nil
}

// decimal renders one exact integer without a floating point round trip.
func decimal(value int64) string {
	return strconv.FormatInt(value, 10)
}

// quoted renders one JSON string. Marshalling a string cannot fail, and the
// authored pages are parsed strictly by the package test.
func quoted(value string) string {
	document, _ := json.Marshal(value)
	return string(document)
}

// authoredUser is the complete simple-user object the pinned source requires
// (eighteen members) for every authored record, including the explicit null
// email of an otherwise complete identity.
const authoredUser = `{"login":"ada","id":1,"node_id":"U_kgDOAA0001",` +
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

// authoredLabelBug is the authored label object of the first record.
const authoredLabelBug = `{"id":208045946,"node_id":"MDU6TGFiZWwyMDgwNDU5NDY=",` +
	`"url":"` + repositoryURL + `/labels/bug","name":"bug",` +
	`"description":"Something is not working","color":"d73a4a","default":true}`

// authoredRecord returns the complete source-shaped record the pinned source
// serves for one authored identity. Every member the pinned issue schema
// requires is present; state_reason is deliberately absent and assignee,
// milestone, closed_at and active_lock_reason are explicit nulls. A
// pull-request-shaped entry also carries the pull-request html_url and the
// pull_request discriminator whose merged_at is an explicit null.
func authoredRecord(
	id, number int64,
	title, body, labels, createdAt, updatedAt string,
	pull bool,
) record {
	htmlURL := "https://github.com/" + fixtureOwner + "/" + fixtureRepo + "/issues/" + decimal(number)
	if pull {
		htmlURL = "https://github.com/" + fixtureOwner + "/" + fixtureRepo + "/pull/" + decimal(number)
	}
	issueURL := repositoryURL + "/issues/" + decimal(number)
	entry := record{
		authoredMember("id", decimal(id)),
		authoredMember("node_id", quoted("I_kwDOA"+decimal(id))),
		authoredMember("url", quoted(issueURL)),
		authoredMember("repository_url", quoted(repositoryURL)),
		authoredMember("labels_url", quoted(issueURL+"/labels{/name}")),
		authoredMember("comments_url", quoted(issueURL+"/comments")),
		authoredMember("events_url", quoted(issueURL+"/events")),
		authoredMember("html_url", quoted(htmlURL)),
		authoredMember("number", decimal(number)),
		authoredMember("state", quoted("open")),
		authoredMember("title", quoted(title)),
		authoredMember("body", body),
		authoredMember("user", authoredUser),
		authoredMember("labels", labels),
		authoredMember("assignee", "null"),
		authoredMember("assignees", "[]"),
		authoredMember("milestone", "null"),
		authoredMember("locked", "false"),
		authoredMember("active_lock_reason", "null"),
		authoredMember("comments", "0"),
		authoredMember("closed_at", "null"),
		authoredMember("created_at", quoted(createdAt)),
		authoredMember("updated_at", quoted(updatedAt)),
		authoredMember("author_association", quoted("OWNER")),
	}
	if !pull {
		return entry
	}
	pullPath := "https://api.github.com/repos/" + fixtureOwner + "/" + fixtureRepo + "/pulls/" + decimal(number)
	pullRequest := `{"url":` + quoted(pullPath) +
		`,"html_url":` + quoted(htmlURL) +
		`,"diff_url":` + quoted(htmlURL+".diff") +
		`,"patch_url":` + quoted(htmlURL+".patch") +
		`,"merged_at":null}`
	return append(entry, authoredMember("pull_request", pullRequest), authoredMember("draft", "false"))
}

// authoredRecords returns the authored provider records in provider order. The
// first record is the ordinary issue the accepted examples identify as id 9001
// number 17, the second is the pull-request-shaped entry id 9002 number 18 and
// the third is the ordinary issue id 9003 number 19.
func authoredRecords() []record {
	return []record{
		authoredRecord(
			9001, 17, "Widget list omits archived entries",
			quoted("The archived widget never appears in the list."),
			"["+authoredLabelBug+"]",
			"2026-01-05T04:15:00Z", "2026-01-06T09:30:00Z",
			false,
		),
		authoredRecord(
			9002, 18, "Fix widget", "null", "[]",
			"2026-01-07T11:00:00Z", "2026-01-08T12:45:00Z",
			true,
		),
		authoredRecord(
			9003, 19, "Add widget pagination", "null", "[]",
			"2026-01-09T05:20:00Z", "2026-01-10T06:05:00Z",
			false,
		),
		authoredRecord(
			9004, 20, "Document widget filters",
			quoted("The filter operators need one worked example."),
			"[]",
			"2026-01-11T08:00:00Z", "2026-01-12T08:30:00Z",
			false,
		),
		authoredRecord(
			9005, 21, "Retire widget fallback",
			quoted("The fallback path hides provider errors."),
			"[]",
			"2026-01-13T07:10:00Z", "2026-01-14T07:55:00Z",
			false,
		),
		authoredRecord(
			9006, 22, "Tighten widget scopes", "null", "[]",
			"2026-01-15T06:40:00Z", "2026-01-16T06:45:00Z",
			false,
		),
	}
}

// fault is one documented deviation from the authored provider behavior. Every
// nonempty fault is also the short name of the fixture that serves it.
type fault string

const (
	faultNone                   fault = ""
	faultWrongID                fault = "wrong-record-id"
	faultWrongNumber            fault = "wrong-record-number"
	faultWrongRepository        fault = "wrong-record-repository"
	faultMissingField           fault = "missing-required-field"
	faultWrongType              fault = "wrong-field-type"
	faultMalformedContinuation  fault = "malformed-continuation"
	faultForeignContinuation    fault = "foreign-host-continuation"
	faultRedirectedContinuation fault = "redirected-continuation"
	faultProviderClamp          fault = "provider-clamp"
	faultNotFound               fault = "not-found"
	faultUnprocessable          fault = "unprocessable"
)

// Fixture describes one authored fixture selection the destination can serve.
type Fixture struct {
	// Name is the short authored fixture name.
	Name string
	// Value is the canonical value: the authored fixture value a lifecycle
	// scenario requirement names, or the endpoint-defined value of a selection
	// no authored requirement names.
	Value string
	// Scenario names the authored lifecycle scenarios that need this selection,
	// empty when the selection is endpoint-defined.
	Scenario string
	// Behavior documents the exact response this selection serves.
	Behavior string
}

// authoredHeadRecords returns the first count authored records. Selections that
// document a source ending after the third record must not serve the remaining
// authored records, so the authored list and the served window stay consistent.
func authoredHeadRecords(count int) []record {
	records := authoredRecords()
	if count >= len(records) {
		return records
	}
	return records[:count]
}

// authoredFixture pairs one documented selection with the records and the
// single fault it serves.
type authoredFixture struct {
	Fixture
	records []record
	fault   fault
}

// authoredFixtures are the authored fixture selections in authored order.
var authoredFixtures = []authoredFixture{
	{
		Fixture: Fixture{
			Name: "exact-page", Value: AuthoredFixturePrefix + "exact-page",
			Scenario: "github-read-exact-repository",
			Behavior: "serves page one of the authored acme/widgets list exactly: issue id 9001 number 17, then pull-request-shaped entry id 9002 number 18, with the authored next-page link",
		},
		records: authoredRecords(),
	},
	{
		Fixture: Fixture{
			Name: "two-pages", Value: AuthoredFixturePrefix + "two-pages",
			Scenario: "github-read-next-page",
			Behavior: "serves the same authored records as exact-page and continues to page two, which holds issue id 9003 number 19 and no link, so the authored source ends",
		},
		records: authoredHeadRecords(3),
	},
	{
		Fixture: Fixture{
			Name: "provider-clamp", Value: AuthoredFixturePrefix + "provider-clamp",
			Scenario: "github-read-provider-clamp",
			Behavior: "serves the authored records with the effective wire page size clamped to 2 however large per_page is, and the next-page link names the clamped size",
		},
		records: authoredRecords(),
		fault:   faultProviderClamp,
	},
	{
		Fixture: Fixture{
			Name: "five-record-page", Value: AuthoredFixturePrefix + "five-record-page",
			Scenario: "github-read-local-record-cap",
			Behavior: "serves six authored records, so per_page=5 returns five records and a next-page link and the unread tail stays on the source",
		},
		records: authoredRecords(),
	},
	{
		Fixture: Fixture{
			Name: "mixed-issue-and-pull-request", Value: AuthoredFixturePrefix + "mixed-issue-and-pull-request",
			Scenario: "github-read-mixed-entries, github-read-complete-nested-presence, github-read-explicit-field-selection, github-read-explicit-record-filter, github-read-filtered-page-continuation",
			Behavior: "serves the same authored records as two-pages: the ordinary issue with its null, false, zero and empty-array members, the pull-request-shaped entry with its pull_request discriminator, then the second ordinary issue",
		},
		records: authoredHeadRecords(3),
	},
	{
		Fixture: Fixture{
			Name: "wrong-record-id", Value: AuthoredFixturePrefix + "wrong-record-id",
			Scenario: "github-read-wrong-record-id",
			Behavior: "serves the two-pages records with the first record's provider id replaced by 9101",
		},
		records: authoredRecords(),
		fault:   faultWrongID,
	},
	{
		Fixture: Fixture{
			Name: "wrong-record-number", Value: AuthoredFixturePrefix + "wrong-record-number",
			Scenario: "github-read-wrong-record-number",
			Behavior: "serves the two-pages records with the first record's repository-scoped number replaced by 71",
		},
		records: authoredRecords(),
		fault:   faultWrongNumber,
	},
	{
		Fixture: Fixture{
			Name: "wrong-record-repository", Value: AuthoredFixturePrefix + "wrong-record-repository",
			Scenario: "github-read-wrong-record-repository",
			Behavior: "serves the two-pages records with the first record's repository_url naming https://api.github.com/repos/acme/gadgets instead of the requested repository",
		},
		records: authoredRecords(),
		fault:   faultWrongRepository,
	},
	{
		Fixture: Fixture{
			Name: "missing-required-field", Value: AuthoredFixturePrefix + "missing-required-field",
			Scenario: "github-read-missing-required-field",
			Behavior: "serves the two-pages records with one required member of the first record absent; the fault field selects the member and defaults to node_id",
		},
		records: authoredRecords(),
		fault:   faultMissingField,
	},
	{
		Fixture: Fixture{
			Name: "wrong-field-type", Value: AuthoredFixturePrefix + "wrong-field-type",
			Scenario: "github-read-wrong-field-type",
			Behavior: "serves the two-pages records with one member of the first record carrying a value of a JSON type the pinned schema does not declare; the fault field selects the member and defaults to comments",
		},
		records: authoredRecords(),
		fault:   faultWrongType,
	},
	{
		Fixture: Fixture{
			Name: "malformed-continuation", Value: AuthoredFixturePrefix + "malformed-continuation",
			Scenario: "github-read-unsafe-continuation-malformed",
			Behavior: "serves the two-pages records with a next-page link whose declared page token is not a page number (page=two), so a client that validates the token cannot follow it",
		},
		records: authoredRecords(),
		fault:   faultMalformedContinuation,
	},
	{
		Fixture: Fixture{
			Name: "foreign-host-continuation", Value: AuthoredFixturePrefix + "foreign-host-continuation",
			Scenario: "github-read-unsafe-continuation-foreign-host",
			Behavior: "serves page one with a next-page link on https://git.example.com, a host outside every admitted provider origin",
		},
		records: authoredRecords(),
		fault:   faultForeignContinuation,
	},
	{
		Fixture: Fixture{
			Name: "redirected-continuation", Value: AuthoredFixturePrefix + "redirected-continuation",
			Scenario: "github-read-unsafe-continuation-redirect",
			Behavior: "answers every request with 302 Found and a Location naming the loopback destination https://127.0.0.1:9, which transport policy refuses; the response carries no body and no link",
		},
		records: authoredRecords(),
		fault:   faultRedirectedContinuation,
	},
	{
		Fixture: Fixture{
			Name: "not-found", Value: AuthoredFixturePrefix + "not-found",
			Behavior: "answers 404 Not Found with the authored GitHub-shaped error body, whatever the request asks for",
		},
		fault: faultNotFound,
	},
	{
		Fixture: Fixture{
			Name: "unprocessable", Value: AuthoredFixturePrefix + "unprocessable",
			Behavior: "answers 422 Unprocessable Entity with the authored validation body naming the page query field",
		},
		fault: faultUnprocessable,
	},
}

// Fixtures returns every authored fixture selection in authored order.
func Fixtures() []Fixture {
	fixtures := make([]Fixture, 0, len(authoredFixtures))
	for _, entry := range authoredFixtures {
		fixtures = append(fixtures, entry.Fixture)
	}
	return fixtures
}

// selectFixture resolves a fixture value: either the canonical value or its
// trailing short name.
func selectFixture(value string) (authoredFixture, error) {
	name := strings.TrimPrefix(value, AuthoredFixturePrefix)
	for _, entry := range authoredFixtures {
		if value == entry.Value || (name != "" && name == entry.Name) {
			return entry, nil
		}
	}
	return authoredFixture{}, fmt.Errorf("%w: unsupported fixture %q", ErrConfiguration, value)
}

// resolveFaultField returns the required issue member the two field faults
// apply to. It refuses a declared field for any other selection and a member
// outside the pinned required list.
func resolveFaultField(selected authoredFixture, declared string) (string, error) {
	switch selected.fault {
	case faultMissingField:
		return faultField(declared, defaultMissingField)
	case faultWrongType:
		return faultField(declared, defaultWrongTypeField)
	}
	if declared != "" {
		return "", fmt.Errorf(
			"%w: a fault field applies only to %s and %s",
			ErrConfiguration,
			AuthoredFixturePrefix+string(faultMissingField),
			AuthoredFixturePrefix+string(faultWrongType),
		)
	}
	return "", nil
}

// faultField validates one declared fault field against the pinned required
// member list, using the documented default when nothing is declared.
func faultField(declared, fallback string) (string, error) {
	field := declared
	if field == "" {
		field = fallback
	}
	for _, candidate := range requiredIssueFields {
		if candidate == field {
			return field, nil
		}
	}
	return "", fmt.Errorf(
		"%w: fault field %q is not a required member of the pinned issue schema",
		ErrConfiguration, field,
	)
}

// pageRecords returns the records this selection serves, with its documented
// record fault applied to the first authored record. Authored records are never
// mutated in place: the served page is always a fresh list.
func (entry authoredFixture) pageRecords(field string) ([]record, error) {
	records := make([]record, len(entry.records))
	copy(records, entry.records)
	if len(records) == 0 {
		return records, nil
	}
	first := records[0]
	var err error
	switch entry.fault {
	case faultWrongID:
		first, err = first.withValue("id", "9101")
	case faultWrongNumber:
		first, err = first.withValue("number", "71")
	case faultWrongRepository:
		first, err = first.withValue("repository_url", quoted("https://api.github.com/repos/"+fixtureOwner+"/gadgets"))
	case faultMissingField:
		first, err = first.withoutValue(field)
	case faultWrongType:
		authored, declared := first.value(field)
		if !declared {
			err = fmt.Errorf("authored record does not declare %s", field)
			break
		}
		first, err = first.withValue(field, wrongTypeValue(authored))
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfiguration, err)
	}
	records[0] = first
	return records, nil
}

// wrongTypeValue returns a value of a JSON type the pinned schema does not
// declare for the selected member: a JSON string, or a JSON number when the
// authored value is itself a string.
func wrongTypeValue(authored string) string {
	if strings.HasPrefix(authored, `"`) {
		return "7"
	}
	return `"wrong-type"`
}

// forcedStatus is the HTTP status this selection answers regardless of the
// authored records, and 0 when the selection serves the authored records.
func (entry authoredFixture) forcedStatus() int {
	switch entry.fault {
	case faultNotFound:
		return http.StatusNotFound
	case faultUnprocessable:
		return http.StatusUnprocessableEntity
	}
	return 0
}

// window is one served page of the authored record list.
type window struct {
	records []record
	// limit is the effective wire page size this destination honoured.
	limit int
	// more reports whether the authored record list continues after this page.
	more bool
}

// authoredWindow returns the authored records of one requested page under the
// effective wire page size.
func authoredWindow(records []record, page, requested int, clamp bool) window {
	limit := requested
	if clamp && limit > clampLimit {
		limit = clampLimit
	}
	start := (page - 1) * limit
	if start >= len(records) {
		return window{limit: limit}
	}
	end := start + limit
	if end > len(records) {
		end = len(records)
	}
	return window{records: records[start:end], limit: limit, more: end < len(records)}
}

// encodePage encodes one served page as the JSON array the pinned source
// declares.
func encodePage(records []record) string {
	var builder strings.Builder
	builder.WriteByte('[')
	for index, entry := range records {
		if index > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(entry.object())
	}
	builder.WriteByte(']')
	return builder.String()
}

// continuationLink returns the Link header value this selection serves after
// one page, empty when the authored source does not continue. The authored
// destinations send one next relation; the pinned header contract also permits
// relations this destination does not send.
func (entry authoredFixture) continuationLink(origin string, page, limit int, more bool) string {
	if !more {
		return ""
	}
	query := "?per_page=" + decimal(int64(limit)) + "&page="
	switch entry.fault {
	case faultMalformedContinuation:
		return "<" + origin + fixtureRoute + query + malformedPageToken + ">; rel=\"next\""
	case faultForeignContinuation:
		return "<" + foreignOrigin + fixtureRoute + query + decimal(int64(page+1)) + ">; rel=\"next\""
	}
	return "<" + origin + fixtureRoute + query + decimal(int64(page+1)) + ">; rel=\"next\""
}

// redirectLocation is the refused continuation target the redirected selection
// answers every request with, empty for every other selection.
func (entry authoredFixture) redirectLocation(page, limit int) string {
	if entry.fault != faultRedirectedContinuation {
		return ""
	}
	return privateRedirectOrigin + fixtureRoute + "?per_page=" + decimal(int64(limit)) +
		"&page=" + decimal(int64(page+1))
}

// requestQuery is the accepted query of the authored route.
type requestQuery struct {
	page    int
	perPage int
}

// parseRequestQuery validates the declared query values of one request. An
// absent value takes the pinned default; a refused value names the declared
// field so the served error document reports that field.
func parseRequestQuery(values url.Values) (requestQuery, string, error) {
	parsed := requestQuery{page: minimumPage, perPage: defaultPageSize}
	if _, present := values["page"]; present {
		page, err := queryValue(values, "page", minimumPage, maximumPage)
		if err != nil {
			return requestQuery{}, "page", err
		}
		parsed.page = page
	}
	if _, present := values["per_page"]; present {
		perPage, err := queryValue(values, "per_page", minimumPageSize, maximumPageSize)
		if err != nil {
			return requestQuery{}, "per_page", err
		}
		parsed.perPage = perPage
	}
	return parsed, "", nil
}

// queryValue returns one bounded integer query value, refusing a repeated or
// non-integer value instead of guessing one.
func queryValue(values url.Values, name string, minimum, maximum int) (int, error) {
	received := values[name]
	if len(received) != 1 {
		return 0, fmt.Errorf("%s must be declared exactly once", name)
	}
	value, err := strconv.Atoi(received[0])
	if err != nil || value < minimum || value > maximum {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, minimum, maximum)
	}
	return value, nil
}

// validationFailedDocument returns the authored validation document naming the
// declared query field the destination refused.
func validationFailedDocument(field string) string {
	return `{"message":"Validation Failed","errors":[{"resource":"Issue","field":` +
		quoted(field) + `,"code":"invalid"}],"documentation_url":"https://docs.github.com/rest"}`
}
