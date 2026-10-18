package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// TestProviderPageSizeClamp covers the one admitted provider page-size
// difference: a Link next target that names a positive integer strictly smaller
// than the size the runtime actually sent. The runtime follows that target,
// reports the page size the provider honoured, restores it for the next page and
// still refuses every other declared difference.
func TestProviderPageSizeClamp(t *testing.T) {
	t.Run("clamped page reports the honoured size with truthful counts", func(t *testing.T) {
		running := startClampedWindow(t)
		defer running.server.Close()
		first := running.read(t, linkInput(t, "open", new(uint64(1)), new(uint64(5))), contract.AbsentValue())
		page := first.Result.Pagination
		if page == nil || page.EffectiveWireLimit == nil || *page.EffectiveWireLimit != 2 ||
			page.FetchedCount != 2 || page.ReturnedCount != 2 || page.EmittedCount != 2 ||
			page.SourceExhausted || page.Continuation.Presence() != contract.PresenceValue {
			t.Fatalf("clamped Link pagination = %#v", page)
		}
		rows, _ := first.Result.Data.Elements()
		if got := linkRecordIDs(rows); got != "1,2" {
			t.Fatalf("clamped Link records = %s", got)
		}
		state := running.codec.mustState(t, running.binding, page.Continuation)
		next, present := state.NextInputs["page"].ExactNumber()
		if state.EffectiveWireLimit == nil || *state.EffectiveWireLimit != 2 ||
			!present || next.Text != "2" || state.SourceExhausted {
			t.Fatalf("clamped continuation state = %#v", state)
		}
		if got := running.observed(); got != "state=open&page=1&per_page=5" {
			t.Fatalf("clamped page sent %q; want the admitted size", got)
		}
	})

	t.Run("clamped continuation follows the provider target and exhausts the source", func(t *testing.T) {
		running := startClampedWindow(t)
		defer running.server.Close()
		first := running.read(t, linkInput(t, "open", new(uint64(1)), new(uint64(5))), contract.AbsentValue())
		// The producing run's admitted size, which the server-owned continuation
		// restore repeats after the provider clamped it.
		followed := running.read(t, linkInput(t, "open", nil, new(uint64(5))), first.Result.Pagination.Continuation)
		page := followed.Result.Pagination
		if page == nil || page.EffectiveWireLimit == nil || *page.EffectiveWireLimit != 2 ||
			page.FetchedCount != 1 || page.ReturnedCount != 1 || page.EmittedCount != 1 ||
			!page.SourceExhausted || page.Continuation.Presence() != contract.PresenceAbsent {
			t.Fatalf("clamped final page pagination = %#v", page)
		}
		rows, _ := followed.Result.Data.Elements()
		if got := linkRecordIDs(rows); got != "3" {
			t.Fatalf("clamped final page records = %s", got)
		}
		if got := running.observed(); got != "state=open&page=1&per_page=5|state=open&page=2&per_page=2" {
			t.Fatalf("clamped read queried %q; want the provider's declared target", got)
		}
	})

	t.Run("every other declared page-size difference is refused", func(t *testing.T) {
		for _, testCase := range []struct {
			name string
			link string
		}{
			{name: "increase", link: `</items?state=open&per_page=6&page=2>; rel="next"`},
			{name: "rewritten", link: `</items?state=open&per_page=05&page=2>; rel="next"`},
			{name: "zero", link: `</items?state=open&per_page=0&page=2>; rel="next"`},
			{name: "non-integer", link: `</items?state=open&per_page=two&page=2>; rel="next"`},
			{name: "dropped", link: `</items?state=open&page=2>; rel="next"`},
			{name: "duplicated", link: `</items?state=open&per_page=2&per_page=3&page=2>; rel="next"`},
			{name: "unrelated query", link: `</items?state=closed&per_page=2&page=2>; rel="next"`},
			{name: "foreign host", link: `<https://evil.test/items?state=open&per_page=2&page=2>; rel="next"`},
			{name: "duplicated navigation", link: `</items?state=open&per_page=2&page=2&page=3>; rel="next"`},
			{name: "malformed navigation", link: `</items?state=open&per_page=2&page=two>; rel="next"`},
			{name: "absent navigation", link: `</items?state=open&per_page=2>; rel="next"`},
			{name: "no progress", link: `</items?state=open&per_page=2&page=1>; rel="next"`},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				var (
					requestsMu sync.Mutex
					requests   int
				)
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
					requestsMu.Lock()
					requests++
					requestsMu.Unlock()
					writer.Header().Set("Content-Type", "application/json")
					writer.Header().Set("Link", testCase.link)
					_, _ = io.WriteString(writer, `[{"id":1,"open":true,"title":"one"}]`)
				}))
				defer server.Close()
				operation := testLinkOperation("link-clamp-refusal")
				origin := serverOrigin(t, server.URL)
				admitter := &testAdmitter{}
				dialer := &mappedDialer{address: server.Listener.Addr().String()}
				execution, err := ExecuteHTTP(context.Background(), HTTPDependencies{ClientIdentity: testClientIdentity,
					Admitter:      admitter,
					Continuations: newMemoryContinuationCodec(),
					Resolver:      fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
					Dialer:        dialer,
				}, testHTTPRequest(t, operation, origin, time.Now().UTC(), linkInput(t, "open", new(uint64(1)), new(uint64(5)))))
				if !errors.Is(err, ErrNavigationConflict) || !execution.PhysicalSend {
					t.Fatalf("execution=%#v error=%v", execution, err)
				}
				requestsMu.Lock()
				count := requests
				requestsMu.Unlock()
				if count != 1 || admitter.calls.Load() != 1 || dialer.calls.Load() != 1 {
					t.Fatalf("requests=%d admits=%d dials=%d", count, admitter.calls.Load(), dialer.calls.Load())
				}
			})
		}
	})

	t.Run("clamped navigation that embeds a credential channel is refused", func(t *testing.T) {
		operation := testLinkOperation("link-clamped-secret")
		operation.Inputs = append(operation.Inputs, contract.FieldBinding{
			FieldID: "token", WireName: "token", Location: contract.FieldLocationQuery,
			JSONPointer: "/query/token", Required: true, Nullable: false,
			Style: "form", Explode: true, Sensitivity: contract.SensitivitySecret,
			Schema: contract.Schema{Type: contract.SchemaTypeString},
		})
		requestURL, err := url.Parse("https://provider.test/items?state=open&page=1&per_page=5&token=credential-canary")
		if err != nil {
			t.Fatal(err)
		}
		_, _, declared, err := sealLinkContinuationTarget(
			operation,
			operation.Execution.Pagination,
			linkInput(t, "open", new(uint64(1)), new(uint64(5))),
			"/items?state=open&per_page=2&page=2&token=attacker",
			pageHTTPContext{
				RequestURL: requestURL,
				Authorization: &contract.OperationAuthorization{
					Required: true, Audience: "https://provider.test", Placement: contract.FieldLocationQuery,
					WireName: "token", FieldID: "token",
				},
			},
		)
		if !errors.Is(err, ErrNavigationConflict) || declared != nil {
			t.Fatalf("credential-bearing clamped Link declared=%v error=%v", declared, err)
		}
	})
}

// clampedWindow drives the authored provider-clamp shape end to end: a
// destination that serves two records for a page of five, declares the clamped
// size in its next-page link, and answers the final page without a link.
type clampedWindow struct {
	server  *httptest.Server
	codec   *memoryContinuationCodec
	binding ContinuationBinding
	origin  string
	now     time.Time

	mu      sync.Mutex
	queries []string
}

func startClampedWindow(t *testing.T) *clampedWindow {
	t.Helper()
	window := &clampedWindow{
		codec:   newMemoryContinuationCodec(),
		now:     time.Now().UTC(),
		binding: linkTestBinding("link-provider-clamp"),
	}
	window.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		window.mu.Lock()
		window.queries = append(window.queries, request.URL.RawQuery)
		window.mu.Unlock()
		query := request.URL.Query()
		writer.Header().Set("Content-Type", "application/json")
		switch query.Get("page") {
		case "1":
			writer.Header().Set("Link", `</items?state=open&per_page=2&page=2>; rel="next"`)
			_, _ = io.WriteString(writer, `[{"id":1,"open":true,"title":"one"},{"id":2,"open":true,"title":"two"}]`)
		case "2":
			_, _ = io.WriteString(writer, `[{"id":3,"open":true,"title":"three"}]`)
		default:
			t.Errorf("clamped window served page %q of %q", query.Get("page"), request.URL.RawQuery)
			writer.WriteHeader(http.StatusBadRequest)
		}
	}))
	window.origin = serverOrigin(t, window.server.URL)
	return window
}

// read performs one admitted attempt against the clamped destination and
// requires it to answer with a published pagination fact.
func (window *clampedWindow) read(t *testing.T, input, continuation contract.PresentValue) HTTPExecution {
	t.Helper()
	operation := testLinkOperation("link-provider-clamp")
	if err := operation.Execution.Pagination.Validate(operation); err != nil {
		t.Fatalf("Link pagination fact: %v", err)
	}
	request := testHTTPRequest(t, operation, window.origin, window.now, input)
	request.Continuation = continuation
	execution, err := ExecuteHTTP(context.Background(), HTTPDependencies{ClientIdentity: testClientIdentity,
		Admitter:      &testAdmitter{},
		Continuations: window.codec,
		Resolver:      fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
		Dialer:        &mappedDialer{address: window.server.Listener.Addr().String()},
	}, request)
	if err != nil {
		t.Fatalf("clamped Link page: %v (navigation conflict=%v)", err, errors.Is(err, ErrNavigationConflict))
	}
	if execution.Result == nil || execution.Result.Pagination == nil {
		t.Fatalf("clamped Link execution = %#v", execution)
	}
	return execution
}

// observed returns the wire queries the clamped destination received, in order.
func (window *clampedWindow) observed() string {
	window.mu.Lock()
	defer window.mu.Unlock()
	return strings.Join(window.queries, "|")
}

// linkTestBinding returns the continuation binding the runtime derives for one
// test Link operation driven by testHTTPRequest.
func linkTestBinding(operationID string) ContinuationBinding {
	return ContinuationBinding{
		Scope:        contract.Scope{OrgID: "org-fixture", WorkspaceID: "workspace-fixture"},
		ConnectionID: "connection-fixture", OperationID: operationID,
		OperationRevision: "operation-revision-fixture", RunID: "run-fixture",
	}
}
