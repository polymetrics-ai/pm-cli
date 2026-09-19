package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestReturnedCountsAndCompleteness(t *testing.T) {
	operation := testOffsetOperation()
	pagination := operation.Execution.Pagination
	if err := pagination.Validate(operation); err != nil {
		t.Fatalf("pagination fact: %v", err)
	}
	codec := newMemoryContinuationCodec()
	binding := ContinuationBinding{
		Scope:        contract.Scope{OrgID: "org-fixture", WorkspaceID: "workspace-fixture"},
		ConnectionID: "connection-fixture", OperationID: operation.OperationID,
		OperationRevision: "operation-revision-fixture", RunID: "run-fixture",
	}

	t.Run("mixed filtered page retains exact counts and unread tail", func(t *testing.T) {
		records := []contract.PresentValue{
			testPageRecord(t, "1", true),
			testPageRecord(t, "2", false),
			testPageRecord(t, "3", true),
			testPageRecord(t, "4", false),
			testPageRecord(t, "5", true),
		}
		decoded := testOffsetResponse(t, records, "12")
		cap := uint64(2)
		match := contract.NewBooleanValue(true)
		controls := contract.ResultControls{Filter: &contract.RecordFilter{
			Path: contract.FieldPath{"keep"}, Operator: contract.FilterEqual, Value: &match,
		}}
		result, err := applyPage(
			context.Background(), operation, decoded, pagination,
			paginationInput(t, new(uint64(0)), new(uint64(5))),
			CallLimits{TotalRecordCap: &cap}, controls, codec, binding, pageHTTPContext{},
		)
		if err != nil {
			t.Fatalf("applyPage: %v", err)
		}
		page := result.Pagination
		if page == nil || page.EffectiveWireLimit == nil || *page.EffectiveWireLimit != 5 ||
			page.FetchedCount != 5 || page.ReturnedCount != 5 || page.EmittedCount != 2 || page.SourceExhausted {
			t.Fatalf("pagination = %#v", page)
		}
		if len(page.UnreadTail) != 2 || page.Continuation.Presence() != contract.PresenceValue {
			t.Fatalf("tail/continuation = %#v", page)
		}
		emitted, _ := result.Data.Elements()
		if got := pageRecordIDs(emitted); got != "1,3" {
			t.Fatalf("emitted IDs = %s", got)
		}
		state := codec.mustState(t, binding, page.Continuation)
		if got := pageRecordIDs(state.UnreadTail); got != "4,5" {
			t.Fatalf("retained IDs = %s", got)
		}
		next, ok := state.NextInputs["offset"].ExactNumber()
		if !ok || next.Text != "5" {
			t.Fatalf("next offset = %#v", state.NextInputs)
		}
		if page.ProviderTotal == nil || page.ProviderTotal.Text != "12" {
			t.Fatalf("provider total = %#v", page.ProviderTotal)
		}
	})

	t.Run("zero emitted matches do not imply source exhaustion", func(t *testing.T) {
		records := []contract.PresentValue{testPageRecord(t, "1", false), testPageRecord(t, "2", false)}
		decoded := testOffsetResponse(t, records, "12")
		match := contract.NewBooleanValue(true)
		result, err := applyPage(
			context.Background(), operation, decoded, pagination,
			paginationInput(t, new(uint64(0)), new(uint64(5))),
			CallLimits{}, contract.ResultControls{Filter: &contract.RecordFilter{
				Path: contract.FieldPath{"keep"}, Operator: contract.FilterEqual, Value: &match,
			}}, codec, binding, pageHTTPContext{},
		)
		if err != nil {
			t.Fatalf("applyPage: %v", err)
		}
		if result.Pagination.EmittedCount != 0 || result.Pagination.SourceExhausted || result.Pagination.Continuation.Presence() != contract.PresenceValue {
			t.Fatalf("pagination = %#v", result.Pagination)
		}
	})

	t.Run("non-record response does not acquire operation pagination", func(t *testing.T) {
		errorSchema := contract.Schema{
			Type:     contract.SchemaTypeObject,
			Required: []string{"message"},
			Properties: map[string]contract.Schema{
				"message": {Type: contract.SchemaTypeString},
			},
		}
		operationWithError := operation
		operationWithError.Responses = append(append([]contract.Response(nil), operation.Responses...), contract.Response{
			Status: "400", MediaType: "application/json", Schema: &errorSchema,
			SourceRefs: testSourceRefs("#/responses/400"),
		})
		errorValue := contract.NewObjectValue(map[string]contract.PresentValue{
			"message": contract.NewStringValue("invalid request"),
		})
		decoded := DecodedResponse{
			StatusCode: 400,
			Header:     make(http.Header),
			Document:   errorValue,
			Result: &contract.ActionResult{
				MediaType: "application/json",
				Data:      errorValue,
			},
		}
		cap := uint64(1)
		result, err := applyPage(
			context.Background(),
			operationWithError,
			decoded,
			pagination,
			contract.NewObjectValue(nil),
			CallLimits{TotalRecordCap: &cap},
			contract.ResultControls{},
			codec,
			binding,
			pageHTTPContext{},
		)
		if err != nil {
			t.Fatalf("non-record response: %v", err)
		}
		if result.Pagination != nil {
			t.Fatalf("non-record response pagination = %#v", result.Pagination)
		}
		fields, ok := result.Data.Fields()
		message, present := fields["message"].TextValue()
		if !ok || !present || message != "invalid request" {
			t.Fatalf("non-record response changed: %#v", result)
		}
	})

	t.Run("cursor and page modes advance only from declared response facts", func(t *testing.T) {
		cursorOperation := testListOperation("cursor-list", http.MethodGet, "/items")
		cursorOperation.Inputs = []contract.FieldBinding{
			testBinding("cursor", "cursor", contract.FieldLocationQuery, false, contract.Schema{Type: contract.SchemaTypeString}, "form", true),
		}
		cursorRecords := *cursorOperation.Responses[0].Schema
		cursorOperation.Responses[0].Schema = &contract.Schema{
			Type: contract.SchemaTypeObject,
			Properties: map[string]contract.Schema{
				"records": cursorRecords,
				"next":    {Type: contract.SchemaTypeString},
			},
		}
		cursorRecordPointer := "/records"
		cursorOperation.Responses[0].RecordPointer = &cursorRecordPointer
		cursorOperation.Execution = &contract.OperationExecution{Pagination: &contract.OperationPagination{
			Kind:                  contract.OperationPaginationCursor,
			CursorRequestField:    "cursor",
			CursorResponsePointer: "/next",
			Witness:               testExecutionWitness(),
		}}
		cursorPagination := cursorOperation.Execution.Pagination
		if err := cursorPagination.Validate(cursorOperation); err != nil {
			t.Fatalf("cursor pagination: %v", err)
		}
		_, cursorState, exhausted, _, err := nextPaginationState(DecodedResponse{
			Document: contract.NewObjectValue(map[string]contract.PresentValue{"next": contract.NewStringValue("opaque-next")}),
		}, cursorPagination, contract.NewObjectValue(nil), 2, nil)
		cursor, cursorPresent := cursorState.NextInputs["cursor"].TextValue()
		if err != nil || exhausted || !cursorPresent || cursor != "opaque-next" {
			t.Fatalf("cursor state=%#v exhausted=%v error=%v", cursorState, exhausted, err)
		}

		pageOperation := testListOperation("page-list", http.MethodGet, "/items")
		pageOperation.Inputs = []contract.FieldBinding{
			testBinding("page", "page", contract.FieldLocationQuery, false, contract.Schema{Type: contract.SchemaTypeInteger}, "form", true),
		}
		pageRecords := *pageOperation.Responses[0].Schema
		pageOperation.Responses[0].Schema = &contract.Schema{
			Type: contract.SchemaTypeObject,
			Properties: map[string]contract.Schema{
				"records": pageRecords,
				"last":    {Type: contract.SchemaTypeBool},
			},
		}
		pageRecordPointer := "/records"
		pageOperation.Responses[0].RecordPointer = &pageRecordPointer
		pageOperation.Execution = &contract.OperationExecution{Pagination: &contract.OperationPagination{
			Kind:                    contract.OperationPaginationPage,
			PageRequestField:        "page",
			LastPageResponsePointer: "/last",
			Witness:                 testExecutionWitness(),
		}}
		pagePagination := pageOperation.Execution.Pagination
		if err := pagePagination.Validate(pageOperation); err != nil {
			t.Fatalf("page pagination: %v", err)
		}
		_, pageState, exhausted, _, err := nextPaginationState(DecodedResponse{
			Document: contract.NewObjectValue(map[string]contract.PresentValue{"last": contract.NewBooleanValue(false)}),
		}, pagePagination, contract.NewObjectValue(map[string]contract.PresentValue{"page": testInteger(t, "2")}), 2, nil)
		nextPage, pagePresent := pageState.NextInputs["page"].ExactNumber()
		if err != nil || exhausted || !pagePresent || nextPage.Text != "3" {
			t.Fatalf("page state=%#v exhausted=%v error=%v", pageState, exhausted, err)
		}

		withoutLast := *pagePagination
		withoutLast.LastPageResponsePointer = ""
		if err := withoutLast.Validate(pageOperation); err != nil {
			t.Fatalf("page pagination without last-page pointer: %v", err)
		}
		_, _, exhausted, _, err = nextPaginationState(
			DecodedResponse{Document: contract.NewObjectValue(nil)},
			&withoutLast,
			contract.NewObjectValue(map[string]contract.PresentValue{"page": testInteger(t, "3")}),
			0,
			nil,
		)
		if err != nil || !exhausted {
			t.Fatalf("page without last-page pointer exhausted=%v error=%v", exhausted, err)
		}
	})

	t.Run("offset without provider total uses only its admitted sent limit", func(t *testing.T) {
		withoutTotal := *pagination
		withoutTotal.TotalResponsePointer = ""
		if err := withoutTotal.Validate(operation); err != nil {
			t.Fatalf("offset pagination without total pointer: %v", err)
		}
		limit := uint64(5)
		_, state, exhausted, _, err := nextPaginationState(
			DecodedResponse{Document: contract.NewObjectValue(nil)},
			&withoutTotal,
			paginationInput(t, new(uint64(0)), &limit),
			limit,
			&limit,
		)
		next, present := state.NextInputs["offset"].ExactNumber()
		if err != nil || exhausted || !present || next.Text != "5" {
			t.Fatalf("full offset page state=%#v exhausted=%v error=%v", state, exhausted, err)
		}
		_, _, exhausted, _, err = nextPaginationState(
			DecodedResponse{Document: contract.NewObjectValue(nil)},
			&withoutTotal,
			paginationInput(t, new(uint64(0)), &limit),
			limit-1,
			&limit,
		)
		if err != nil || !exhausted {
			t.Fatalf("short offset page exhausted=%v error=%v", exhausted, err)
		}
	})

	t.Run("known twelve-record source takes exactly three sent-size pages", func(t *testing.T) {
		var seenOffsets []string
		var seenLimits []string
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			query := request.URL.Query()
			seenOffsets = append(seenOffsets, query.Get("offset"))
			seenLimits = append(seenLimits, query.Get("limit"))
			offset, offsetErr := strconv.Atoi(query.Get("offset"))
			limit, limitErr := strconv.Atoi(query.Get("limit"))
			if offsetErr != nil || limitErr != nil || limit != 5 || offset < 0 || offset >= 12 {
				t.Errorf("wire query = %q", request.URL.RawQuery)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			end := offset + limit
			if end > 12 {
				end = 12
			}
			var body strings.Builder
			body.WriteString(`{"records":[`)
			for index := offset; index < end; index++ {
				if index != offset {
					body.WriteByte(',')
				}
				_, _ = fmt.Fprintf(&body, `{"empty":"","id":%d,"keep":true}`, index)
			}
			body.WriteString(`],"total":12}`)
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(body.String()))
		}))
		defer server.Close()

		now := time.Now().UTC()
		origin := serverOrigin(t, server.URL)
		admitter := &testAdmitter{}
		dialer := &mappedDialer{address: server.Listener.Addr().String()}
		continuation := contract.AbsentValue()
		var last contract.ActionResult
		for page := range 3 {
			input := contract.NewObjectValue(nil)
			if page == 0 {
				input = paginationInput(t, new(uint64(0)), new(uint64(5)))
			}
			request := testHTTPRequest(t, operation, origin, now, input)
			if request.Operation.Execution == nil || request.Operation.Execution.Pagination == nil {
				t.Fatal("request lost generated pagination facts")
			}
			request.Continuation = continuation
			attemptID, err := contract.NewAttemptID(fmt.Sprintf("attempt-offset-list-%d", page+1))
			if err != nil {
				t.Fatal(err)
			}
			request.Call.Attempt.AttemptID = attemptID
			request.Call.Attempt.ReceiptRef = fmt.Sprintf("receipt-offset-list-%d", page+1)
			execution, err := ExecuteHTTP(context.Background(), HTTPDependencies{ClientIdentity: testClientIdentity,
				Admitter:      admitter,
				Continuations: codec,
				Resolver:      fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
				Dialer:        dialer,
			}, request)
			if err != nil {
				t.Fatalf("page %d: %v", page+1, err)
			}
			wantCount := uint64(5)
			if page == 2 {
				wantCount = 2
			}
			if execution.Result == nil || execution.Result.Pagination == nil ||
				execution.Result.Pagination.EffectiveWireLimit == nil ||
				*execution.Result.Pagination.EffectiveWireLimit != 5 ||
				execution.Result.Pagination.FetchedCount != wantCount ||
				execution.Result.Pagination.ReturnedCount != wantCount ||
				execution.Result.Pagination.EmittedCount != wantCount ||
				execution.Result.Pagination.ProviderTotal == nil ||
				execution.Result.Pagination.ProviderTotal.Text != "12" ||
				execution.Result.Pagination.SourceExhausted != (page == 2) {
				t.Fatalf("page %d result = %#v", page+1, execution.Result)
			}
			last = *execution.Result
			continuation = execution.Result.Pagination.Continuation
			if page < 2 && continuation.Presence() != contract.PresenceValue {
				t.Fatalf("page %d lost continuation", page+1)
			}
		}
		if strings.Join(seenOffsets, ",") != "0,5,10" || strings.Join(seenLimits, ",") != "5,5,5" {
			t.Fatalf("offsets=%v limits=%v", seenOffsets, seenLimits)
		}
		if admitter.calls.Load() != 3 || dialer.calls.Load() != 3 ||
			last.Pagination == nil || !last.Pagination.SourceExhausted ||
			last.Pagination.Continuation.Presence() != contract.PresenceAbsent {
			t.Fatalf("admits=%d dials=%d last=%#v", admitter.calls.Load(), dialer.calls.Load(), last.Pagination)
		}
	})

	t.Run("unsent size remains absent and raw navigation conflicts", func(t *testing.T) {
		offset := uint64(0)
		input := paginationInput(t, &offset, nil)
		result, err := applyPage(context.Background(), operation, testOffsetResponse(t, []contract.PresentValue{testPageRecord(t, "1", true)}, "12"), pagination, input, CallLimits{}, contract.ResultControls{}, codec, binding, pageHTTPContext{})
		if err != nil {
			t.Fatalf("applyPage: %v", err)
		}
		if result.Pagination.EffectiveWireLimit != nil || result.Pagination.SourceExhausted {
			t.Fatalf("unsent limit pagination = %#v", result.Pagination)
		}
		conflicting := paginationInput(t, &offset, new(uint64(5)))
		_, err = preparePagination(context.Background(), operation, pagination, false, conflicting, result.Pagination.Continuation, codec, binding)
		if !errors.Is(err, ErrNavigationConflict) {
			t.Fatalf("conflict error = %v", err)
		}
	})
	t.Run("RFC8288 Link navigation preserves sent size and unread tails", func(t *testing.T) {
		var (
			peerMu      sync.Mutex
			seenQueries []string
		)
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			peerMu.Lock()
			seenQueries = append(seenQueries, request.URL.RawQuery)
			peerMu.Unlock()
			page := request.URL.Query().Get("page")
			writer.Header().Set("Content-Type", "application/json")
			switch page {
			case "1":
				writer.Header().Set("Link", `</items?page=2&per_page=2&state=open>; rel="next"`)
				_, _ = io.WriteString(writer, `[{"id":1,"open":true,"title":"one"},{"id":2,"open":true,"title":"two"}]`)
			case "2":
				_, _ = io.WriteString(writer, `[{"id":3,"open":true,"title":"three"}]`)
			default:
				t.Errorf("unexpected page query %q", request.URL.RawQuery)
				writer.WriteHeader(http.StatusBadRequest)
			}
		}))
		defer server.Close()
		operation := testLinkOperation("link-list")
		if err := operation.Execution.Pagination.Validate(operation); err != nil {
			t.Fatalf("Link pagination fact: %v", err)
		}
		now := time.Now().UTC()
		origin := serverOrigin(t, server.URL)
		codec := newMemoryContinuationCodec()
		admitter := &testAdmitter{}
		dialer := &mappedDialer{address: server.Listener.Addr().String()}
		dependencies := HTTPDependencies{ClientIdentity: testClientIdentity,
			Admitter:      admitter,
			Continuations: codec,
			Resolver:      fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
			Dialer:        dialer,
		}
		cap := uint64(1)
		firstRequest := testHTTPRequest(t, operation, origin, now, linkInput(t, "open", new(uint64(1)), new(uint64(2))))
		firstRequest.Limits.TotalRecordCap = &cap
		first, err := ExecuteHTTP(context.Background(), dependencies, firstRequest)
		if err != nil {
			t.Fatalf("first Link page: %v", err)
		}
		if first.Result == nil || first.Result.Pagination == nil ||
			first.Result.Pagination.EffectiveWireLimit == nil ||
			*first.Result.Pagination.EffectiveWireLimit != 2 ||
			first.Result.Pagination.FetchedCount != 2 ||
			first.Result.Pagination.EmittedCount != 1 ||
			len(first.Result.Pagination.UnreadTail) != 1 ||
			first.Result.Pagination.Continuation.Presence() != contract.PresenceValue {
			t.Fatalf("first Link result = %#v", first.Result)
		}
		firstContinuation := first.Result.Pagination.Continuation

		conflicting := testHTTPRequest(t, operation, origin, now, linkInput(t, "open", new(uint64(9)), nil))
		conflicting.Continuation = firstContinuation
		conflicting.Limits.TotalRecordCap = &cap
		if _, err := ExecuteHTTP(context.Background(), dependencies, conflicting); !errors.Is(err, ErrNavigationConflict) {
			t.Fatalf("raw navigation conflict error = %v", err)
		}
		if admitter.calls.Load() != 1 || dialer.calls.Load() != 1 {
			t.Fatalf("conflict performed IO: admits=%d dials=%d", admitter.calls.Load(), dialer.calls.Load())
		}

		tailRequest := testHTTPRequest(t, operation, origin, now, linkInput(t, "open", nil, nil))
		tailRequest.Continuation = firstContinuation
		tailRequest.Limits.TotalRecordCap = &cap
		tail, err := ExecuteHTTP(context.Background(), dependencies, tailRequest)
		if err != nil {
			t.Fatalf("retained Link tail: %v", err)
		}
		if tail.PhysicalSend || tail.Result == nil || tail.Result.Pagination == nil ||
			tail.Result.Pagination.EffectiveWireLimit == nil ||
			*tail.Result.Pagination.EffectiveWireLimit != 2 ||
			tail.Result.Pagination.Continuation.Presence() != contract.PresenceValue {
			t.Fatalf("retained Link tail = %#v", tail)
		}
		tailRows, _ := tail.Result.Data.Elements()
		if got := linkRecordIDs(tailRows); got != "2" {
			t.Fatalf("retained Link tail IDs = %s", got)
		}

		finalRequest := testHTTPRequest(t, operation, origin, now, linkInput(t, "open", nil, nil))
		finalRequest.Continuation = tail.Result.Pagination.Continuation
		finalRequest.Limits.TotalRecordCap = &cap
		final, err := ExecuteHTTP(context.Background(), dependencies, finalRequest)
		if err != nil {
			t.Fatalf("final Link page: %v", err)
		}
		if final.Result == nil || final.Result.Pagination == nil ||
			!final.Result.Pagination.SourceExhausted ||
			final.Result.Pagination.Continuation.Presence() != contract.PresenceAbsent {
			t.Fatalf("final Link result = %#v", final.Result)
		}
		peerMu.Lock()
		gotQueries := strings.Join(seenQueries, "|")
		peerMu.Unlock()
		if gotQueries != "state=open&page=1&per_page=2|state=open&page=2&per_page=2" ||
			admitter.calls.Load() != 2 || dialer.calls.Load() != 2 {
			t.Fatalf("queries=%q admits=%d dials=%d", gotQueries, admitter.calls.Load(), dialer.calls.Load())
		}
	})

	t.Run("Link pagination never fabricates an absent page size", func(t *testing.T) {
		operation := testLinkOperation("link-without-limit")
		pagination := operation.Execution.Pagination
		input := linkInput(t, "open", new(uint64(1)), nil)
		requestURL, err := url.Parse("https://provider.test/items?state=open&page=1")
		if err != nil {
			t.Fatal(err)
		}
		header := make(http.Header)
		header.Set("Link", `</items?state=open&page=2>; rel="next"`)
		records := []contract.PresentValue{testPageRecord(t, "1", true)}
		decoded := DecodedResponse{
			StatusCode: 200,
			Header:     make(http.Header),
			Document:   contract.NewArrayValue(records),
			Result:     &contract.ActionResult{MediaType: "application/json", Data: contract.NewArrayValue(records)},
		}
		codec := newMemoryContinuationCodec()
		binding := ContinuationBinding{
			Scope:        contract.Scope{OrgID: "org-fixture", WorkspaceID: "workspace-fixture"},
			ConnectionID: "connection-fixture", OperationID: operation.OperationID,
			OperationRevision: "operation-revision-fixture", RunID: "run-fixture",
		}
		result, err := applyPage(
			context.Background(), operation, decoded, pagination, input,
			CallLimits{}, contract.ResultControls{}, codec, binding,
			pageHTTPContext{RequestURL: requestURL, ResponseHeader: header},
		)
		if err != nil {
			t.Fatalf("apply Link page: %v", err)
		}
		if result.Pagination == nil {
			t.Fatal("Link pagination metadata is absent")
		}
		state := codec.mustState(t, binding, result.Pagination.Continuation)
		next, present := state.NextInputs["page"].ExactNumber()
		if result.Pagination.EffectiveWireLimit != nil || state.EffectiveWireLimit != nil ||
			!present || next.Text != "2" {
			t.Fatalf("pagination=%#v state=%#v", result.Pagination, state)
		}
	})

	t.Run("hostile or ambiguous Link next never causes an internal resend", func(t *testing.T) {
		for _, testCase := range []struct {
			name string
			link string
		}{
			{name: "cross origin", link: `<https://evil.test/items?page=2&per_page=2&state=open>; rel="next"`},
			{name: "changed path", link: `</other?page=2&per_page=2&state=open>; rel="next"`},
			{name: "changed unrelated query", link: `</items?page=2&per_page=2&state=closed>; rel="next"`},
			{name: "duplicate navigation", link: `</items?page=2&page=3&per_page=2&state=open>; rel="next"`},
			{name: "multiple next", link: `</items?page=2&per_page=2&state=open>; rel="next", </items?page=3&per_page=2&state=open>; rel="next"`},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				var requests int
				var requestsMu sync.Mutex
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
					requestsMu.Lock()
					requests++
					requestsMu.Unlock()
					writer.Header().Set("Content-Type", "application/json")
					writer.Header().Set("Link", testCase.link)
					_, _ = io.WriteString(writer, `[{"id":1,"open":true,"title":"one"}]`)
				}))
				defer server.Close()
				operation := testLinkOperation("hostile-link")
				origin := serverOrigin(t, server.URL)
				request := testHTTPRequest(t, operation, origin, time.Now().UTC(), linkInput(t, "open", new(uint64(1)), new(uint64(2))))
				admitter := &testAdmitter{}
				dialer := &mappedDialer{address: server.Listener.Addr().String()}
				execution, err := ExecuteHTTP(context.Background(), HTTPDependencies{ClientIdentity: testClientIdentity,
					Admitter:      admitter,
					Continuations: newMemoryContinuationCodec(),
					Resolver:      fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
					Dialer:        dialer,
				}, request)
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
		operation := testLinkOperation("link-secret")
		operation.Inputs = append(operation.Inputs, contract.FieldBinding{
			FieldID: "token", WireName: "token", Location: contract.FieldLocationQuery,
			JSONPointer: "/query/token", Required: true, Nullable: false,
			Style: "form", Explode: true, Sensitivity: contract.SensitivitySecret,
			Schema: contract.Schema{Type: contract.SchemaTypeString},
		})
		requestURL, err := url.Parse("https://provider.test/items?state=open&page=1&per_page=2&token=credential-canary")
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = linkNavigationValue(
			operation,
			operation.Execution.Pagination,
			"page",
			"/items?state=open&page=2&per_page=2&token=attacker",
			linkInput(t, "open", new(uint64(1)), new(uint64(2))),
			requestURL,
			&contract.OperationAuthorization{
				Required: true, Audience: "https://provider.test", Placement: contract.FieldLocationQuery,
				WireName: "token", FieldID: "token",
			},
		)
		if !errors.Is(err, ErrNavigationConflict) {
			t.Fatalf("credential-bearing Link error = %v", err)
		}

		reservedOperation := testLinkOperation("link-reserved-navigation")
		for index := range reservedOperation.Inputs {
			if reservedOperation.Inputs[index].FieldID == "page" {
				reservedOperation.Inputs[index].Schema = contract.Schema{Type: contract.SchemaTypeString}
				reservedOperation.Inputs[index].AllowReserved = true
			}
		}
		reservedRequestURL, err := url.Parse("https://provider.test/items?state=open&page=one&per_page=2")
		if err != nil {
			t.Fatal(err)
		}
		reservedInput := contract.NewObjectValue(map[string]contract.PresentValue{
			"state":    contract.NewStringValue("open"),
			"page":     contract.NewStringValue("one"),
			"per-page": testInteger(t, "2"),
		})
		_, _, err = linkNavigationValue(
			reservedOperation,
			reservedOperation.Execution.Pagination,
			"page",
			"/items?state=open&page=two%26injected%3Dtrue&per_page=2",
			reservedInput,
			reservedRequestURL,
			nil,
		)
		if !errors.Is(err, ErrNavigationConflict) {
			t.Fatalf("re-encoding Link navigation error = %v", err)
		}
	})
}

func testOffsetOperation() contract.Operation {
	record := contract.Schema{
		Type:     contract.SchemaTypeObject,
		Required: []string{"id", "keep", "empty"},
		Properties: map[string]contract.Schema{
			"id":    {Type: contract.SchemaTypeInteger},
			"keep":  {Type: contract.SchemaTypeBool},
			"empty": {Type: contract.SchemaTypeString},
		},
	}
	envelope := contract.Schema{
		Type:     contract.SchemaTypeObject,
		Required: []string{"records", "total"},
		Properties: map[string]contract.Schema{
			"records": {Type: contract.SchemaTypeArray, Items: &record},
			"total":   {Type: contract.SchemaTypeInteger},
		},
	}
	operation := testOperation("offset-list", http.MethodGet, "/items", envelope)
	recordPointer := "/records"
	operation.Responses[0].RecordPointer = &recordPointer
	operation.Inputs = []contract.FieldBinding{
		testBinding("offset", "offset", contract.FieldLocationQuery, false, contract.Schema{Type: contract.SchemaTypeInteger}, "form", true),
		testBinding("limit", "limit", contract.FieldLocationQuery, false, contract.Schema{Type: contract.SchemaTypeInteger}, "form", true),
	}
	operation.Execution = &contract.OperationExecution{Pagination: &contract.OperationPagination{
		Kind:                 contract.OperationPaginationOffset,
		OffsetRequestField:   "offset",
		LimitRequestField:    "limit",
		TotalResponsePointer: "/total",
		Witness:              testExecutionWitness(),
	}}
	return operation
}
func testLinkOperation(id string) contract.Operation {
	operation := testListOperation(id, http.MethodGet, "/items")
	operation.Inputs = []contract.FieldBinding{
		testBinding("state", "state", contract.FieldLocationQuery, true, contract.Schema{Type: contract.SchemaTypeString}, "form", true),
		testBinding("page", "page", contract.FieldLocationQuery, false, contract.Schema{Type: contract.SchemaTypeInteger}, "form", true),
		testBinding("per-page", "per_page", contract.FieldLocationQuery, false, contract.Schema{Type: contract.SchemaTypeInteger}, "form", true),
	}
	operation.Execution = &contract.OperationExecution{Pagination: &contract.OperationPagination{
		Kind:               contract.OperationPaginationLink,
		LinkRequestField:   "page",
		LinkResponseHeader: "Link",
		LimitRequestField:  "per-page",
		Witness:            testExecutionWitness(),
	}}
	return operation
}

func linkInput(t *testing.T, state string, page, limit *uint64) contract.PresentValue {
	t.Helper()
	fields := map[string]contract.PresentValue{"state": contract.NewStringValue(state)}
	if page != nil {
		fields["page"] = testInteger(t, strconv.FormatUint(*page, 10))
	}
	if limit != nil {
		fields["per-page"] = testInteger(t, strconv.FormatUint(*limit, 10))
	}
	return contract.NewObjectValue(fields)
}

func linkRecordIDs(records []contract.PresentValue) string {
	ids := make([]string, 0, len(records))
	for _, record := range records {
		fields, _ := record.Fields()
		id, _ := fields["id"].ExactNumber()
		ids = append(ids, id.Text)
	}
	return strings.Join(ids, ",")
}

func testExecutionWitness() contract.ExecutionWitness {
	return contract.ExecutionWitness{
		ProofID:    "fixture-pagination-proof",
		SourceRefs: testSourceRefs("#/x-pm-pagination"),
	}
}

func testOffsetResponse(t *testing.T, records []contract.PresentValue, total string) DecodedResponse {
	t.Helper()
	document := contract.NewObjectValue(map[string]contract.PresentValue{
		"records": contract.NewArrayValue(records),
		"total":   testInteger(t, total),
	})
	result := contract.ActionResult{MediaType: "application/json", Data: contract.NewArrayValue(records)}
	return DecodedResponse{StatusCode: 200, Header: make(http.Header), Document: document, Result: &result}
}

func testPageRecord(t *testing.T, id string, keep bool) contract.PresentValue {
	t.Helper()
	return contract.NewObjectValue(map[string]contract.PresentValue{
		"id":    testInteger(t, id),
		"keep":  contract.NewBooleanValue(keep),
		"empty": contract.NewStringValue(""),
	})
}

func paginationInput(t *testing.T, offset *uint64, limit *uint64) contract.PresentValue {
	t.Helper()
	fields := make(map[string]contract.PresentValue)
	if offset != nil {
		fields["offset"] = testInteger(t, strconv.FormatUint(*offset, 10))
	}
	if limit != nil {
		fields["limit"] = testInteger(t, strconv.FormatUint(*limit, 10))
	}
	return contract.NewObjectValue(fields)
}

func pageRecordIDs(records []contract.PresentValue) string {
	ids := make([]string, 0, len(records))
	for _, record := range records {
		fields, _ := record.Fields()
		id, _ := fields["id"].ExactNumber()
		ids = append(ids, id.Text)
	}
	return strings.Join(ids, ",")
}

type memoryContinuationCodec struct {
	mu     sync.Mutex
	next   uint64
	states map[string]continuationEntry
}

type continuationEntry struct {
	binding ContinuationBinding
	state   ContinuationState
}

func newMemoryContinuationCodec() *memoryContinuationCodec {
	return &memoryContinuationCodec{states: make(map[string]continuationEntry)}
}

func (c *memoryContinuationCodec) SealContinuation(_ context.Context, binding ContinuationBinding, state ContinuationState) (contract.PresentValue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	token := "continuation-" + strconv.FormatUint(c.next, 10)
	c.states[token] = continuationEntry{binding: binding, state: cloneContinuationState(state)}
	return contract.NewStringValue(token), nil
}

func (c *memoryContinuationCodec) OpenContinuation(_ context.Context, binding ContinuationBinding, token contract.PresentValue) (ContinuationState, error) {
	text, ok := token.TextValue()
	if !ok {
		return ContinuationState{}, errors.New("invalid continuation token")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.states[text]
	if !ok || !entry.binding.Scope.Equal(binding.Scope) || entry.binding.ConnectionID != binding.ConnectionID ||
		entry.binding.OperationID != binding.OperationID || entry.binding.OperationRevision != binding.OperationRevision || entry.binding.RunID != binding.RunID {
		return ContinuationState{}, errors.New("continuation binding mismatch")
	}
	return cloneContinuationState(entry.state), nil
}

func (c *memoryContinuationCodec) mustState(t *testing.T, binding ContinuationBinding, token contract.PresentValue) ContinuationState {
	t.Helper()
	state, err := c.OpenContinuation(context.Background(), binding, token)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func cloneContinuationState(state ContinuationState) ContinuationState {
	copy := state
	copy.EffectiveWireLimit = cloneUint64(state.EffectiveWireLimit)
	copy.ProviderTotal = cloneExactNumber(state.ProviderTotal)
	copy.UnreadTail = clonePresentValues(state.UnreadTail)
	if state.NextInputs != nil {
		copy.NextInputs = make(map[string]contract.PresentValue, len(state.NextInputs))
		for fieldID, value := range state.NextInputs {
			copy.NextInputs[fieldID] = value
		}
	}
	return copy
}
