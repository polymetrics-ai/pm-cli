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
	"sync/atomic"
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

	t.Run("declared error response that repeats the record pointer is not paginated", func(t *testing.T) {
		// The compiled connector emits the operation's record extraction pointer
		// on every response variant, so a declared error response carries the
		// root pointer while its schema addresses an object, never records. Such
		// a response is the provider's definitive refusal and must decode as the
		// declared body instead of being refused as a page.
		errorSchema := contract.Schema{
			Type:     contract.SchemaTypeObject,
			Required: []string{"message"},
			Properties: map[string]contract.Schema{
				"message": {Type: contract.SchemaTypeString},
			},
		}
		rootPointer := ""
		operationWithError := testLinkOperation("link-list-with-error")
		operationWithError.Responses[0].RecordPointer = &rootPointer
		operationWithError.Responses = append(operationWithError.Responses, contract.Response{
			Status: "404", MediaType: "application/json", Schema: &errorSchema,
			RecordPointer: &rootPointer,
			SourceRefs:    testSourceRefs("#/responses/404"),
		})
		if err := operationWithError.Execution.Pagination.Validate(operationWithError); err != nil {
			t.Fatalf("pagination fact with a declared error response: %v", err)
		}
		var requests atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			requests.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(writer, `{"message":"Not Found"}`)
		}))
		defer server.Close()
		now := time.Now().UTC()
		request := testHTTPRequest(t, operationWithError, serverOrigin(t, server.URL), now, linkInput(t, "all", new(uint64(1)), new(uint64(2))))
		request.Continuation = contract.AbsentValue()
		dialer := &mappedDialer{address: server.Listener.Addr().String()}
		execution, err := ExecuteHTTP(context.Background(), HTTPDependencies{ClientIdentity: testClientIdentity,
			Admitter:      &testAdmitter{},
			Continuations: newMemoryContinuationCodec(),
			Resolver:      fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
			Dialer:        dialer,
		}, request)
		if err != nil {
			t.Fatalf("declared error response was refused: %v: %v", err, errors.Unwrap(err))
		}
		if !execution.PhysicalSend || execution.Disposition != DispositionResponded ||
			execution.Response.StatusCode != http.StatusNotFound || execution.Result == nil || requests.Load() != 1 {
			t.Fatalf("execution=%#v requests=%d", execution, requests.Load())
		}
		if execution.Result.Pagination != nil {
			t.Fatalf("error response acquired pagination = %#v", execution.Result.Pagination)
		}
		fields, ok := execution.Result.Data.Fields()
		message, present := fields["message"].TextValue()
		if !ok || !present || message != "Not Found" {
			t.Fatalf("declared error body = %#v", execution.Result.Data)
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
		_, _, _, err = sealLinkContinuationTarget(
			operation,
			operation.Execution.Pagination,
			linkInput(t, "open", new(uint64(1)), new(uint64(2))),
			"/items?state=open&page=2&per_page=2&token=attacker",
			pageHTTPContext{
				RequestURL: requestURL,
				Authorization: &contract.OperationAuthorization{
					Required: true, Audience: "https://provider.test", Placement: contract.FieldLocationQuery,
					WireName: "token", FieldID: "token",
				},
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
		_, _, _, err = sealLinkContinuationTarget(
			reservedOperation,
			reservedOperation.Execution.Pagination,
			reservedInput,
			"/items?state=open&page=two%26injected%3Dtrue&per_page=2",
			pageHTTPContext{RequestURL: reservedRequestURL},
		)
		if !errors.Is(err, ErrNavigationConflict) {
			t.Fatalf("re-encoding Link navigation error = %v", err)
		}
	})
}

// providerTargetScheme repeats the scheme the controlled destination was
// reached with, so an absolute provider target names the admitted origin
// exactly as the destination serves it.
func providerTargetScheme(request *http.Request) string {
	if request.TLS != nil {
		return "https"
	}
	return "http"
}

// recordedGitHubIssueListNextTarget is the provider navigation of a
// credential-free read of
// https://api.github.com/repos/golang/go/issues?per_page=2&page=1 observed on
// 2026-09-19: HTTP 200 with Link rel="next" naming the canonical
// repository-id route, an `after` cursor and the page that cursor addresses.
// The admitted GitHub slice declares query `page` as its only navigation on
// /repos/{owner}/{repo}/issues and query `per_page` as its page size, and the
// pinned capture declares neither an `after` parameter nor the
// /repositories/{id}/issues route, so this target is provider truth the
// admitted strategy cannot express and must keep failing rather than be
// replayed as a URL.
const recordedGitHubIssueListNextTarget = `/repositories/23096959/issues?per_page=2&page=2&after=Y3Vyc29yOnYyOpLPAAABoLjpTwjPAAAAAUhclFY%3D`

// TestLinkProviderNavigationBoundary pins the exact boundary of the admitted
// Link strategy: a provider next target on the declared origin and path that
// differs only in the declared navigation field yields one scoped
// continuation, while the recorded GitHub target keeps failing on both counts
// the declared source cannot express — it leaves the declared route and it
// carries a cursor no admitted field names.
func TestLinkProviderNavigationBoundary(t *testing.T) {
	t.Run("absolute declared-origin target yields one scoped continuation", func(t *testing.T) {
		var (
			peerMu      sync.Mutex
			seenQueries []string
		)
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			peerMu.Lock()
			seenQueries = append(seenQueries, request.URL.RawQuery)
			peerMu.Unlock()
			writer.Header().Set("Content-Type", "application/json")
			switch request.URL.Query().Get("page") {
			case "1":
				// An absolute target on the admitted origin qualifies itself as
				// the untrusted source of one scalar navigation value: the
				// provider's own parameter order is not the sent order.
				writer.Header().Set("Link",
					`<`+providerTargetScheme(request)+`://`+request.Host+`/items?per_page=2&page=2&state=open>; rel="next", `+
						`<`+providerTargetScheme(request)+`://`+request.Host+`/items?per_page=2&page=9&state=open>; rel="last"`)
				_, _ = io.WriteString(writer, `[{"id":1,"open":true,"title":"one"},{"id":2,"open":true,"title":"two"}]`)
			case "2":
				// The provider ended the authored source: a truthful last page
				// names no next relation.
				writer.Header().Set("Link",
					`<`+providerTargetScheme(request)+`://`+request.Host+`/items?per_page=2&page=1&state=open>; rel="first", `+
						`<`+providerTargetScheme(request)+`://`+request.Host+`/items?per_page=2&page=1&state=open>; rel="prev"`)
				_, _ = io.WriteString(writer, `[{"id":3,"open":true,"title":"three"}]`)
			default:
				t.Errorf("unexpected page query %q", request.URL.RawQuery)
				writer.WriteHeader(http.StatusBadRequest)
			}
		}))
		defer server.Close()
		operation := testLinkOperation("link-absolute-navigation")
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
		binding := ContinuationBinding{
			Scope:        contract.Scope{OrgID: "org-fixture", WorkspaceID: "workspace-fixture"},
			ConnectionID: "connection-fixture", OperationID: operation.OperationID,
			OperationRevision: "operation-revision-fixture", RunID: "run-fixture",
		}
		first, err := ExecuteHTTP(
			context.Background(), dependencies,
			testHTTPRequest(t, operation, origin, now, linkInput(t, "open", new(uint64(1)), new(uint64(2)))),
		)
		if err != nil {
			t.Fatalf("declared-origin Link page: %v (cause %v)", err, errors.Unwrap(err))
		}
		page := first.Result.Pagination
		if page == nil || page.SourceExhausted || page.Continuation.Presence() != contract.PresenceValue ||
			page.EffectiveWireLimit == nil || *page.EffectiveWireLimit != 2 ||
			page.FetchedCount != 2 || page.ReturnedCount != 2 || page.EmittedCount != 2 {
			t.Fatalf("declared-origin Link pagination = %#v", page)
		}
		state := codec.mustState(t, binding, page.Continuation)
		next, present := state.NextInputs["page"].ExactNumber()
		if state.SourceExhausted || len(state.NextInputs) != 1 || !present || next.Text != "2" ||
			state.EffectiveWireLimit == nil || *state.EffectiveWireLimit != 2 ||
			state.MediaType != "application/json" || len(state.UnreadTail) != 0 {
			t.Fatalf("declared-origin continuation state = %#v", state)
		}

		secondRequest := testHTTPRequest(t, operation, origin, now, linkInput(t, "open", nil, nil))
		secondRequest.Continuation = page.Continuation
		second, err := ExecuteHTTP(context.Background(), dependencies, secondRequest)
		if err != nil {
			t.Fatalf("continued Link page: %v", err)
		}
		if second.Result.Pagination == nil || !second.Result.Pagination.SourceExhausted ||
			second.Result.Pagination.Continuation.Presence() != contract.PresenceAbsent ||
			second.Result.Pagination.FetchedCount != 1 {
			t.Fatalf("continued Link pagination = %#v", second.Result.Pagination)
		}
		peerMu.Lock()
		gotQueries := strings.Join(seenQueries, "|")
		peerMu.Unlock()
		// The continuation is re-requested through the declared operation in
		// the declared field order; the provider's absolute target is never
		// replayed as a URL.
		if gotQueries != "state=open&page=1&per_page=2|state=open&page=2&per_page=2" ||
			admitter.calls.Load() != 2 || dialer.calls.Load() != 2 {
			t.Fatalf("queries=%q admits=%d dials=%d", gotQueries, admitter.calls.Load(), dialer.calls.Load())
		}
	})

	t.Run("canonical route without a declared route policy keeps failing", func(t *testing.T) {
		var requests atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			requests.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			// Every value this target carries is declared navigation or the sent
			// page size; only the path differs from the admitted route, so this
			// case fails exactly when an undeclared path is admitted.
			writer.Header().Set("Link",
				`<`+providerTargetScheme(request)+`://`+request.Host+`/repositories/23096959/issues?state=open&page=2&per_page=2>; rel="next"`)
			_, _ = io.WriteString(writer, `[{"id":1,"open":true,"title":"one"},{"id":2,"open":true,"title":"two"}]`)
		}))
		defer server.Close()
		operation := testLinkOperation("link-canonical-route-navigation")
		origin := serverOrigin(t, server.URL)
		admitter := &testAdmitter{}
		dialer := &mappedDialer{address: server.Listener.Addr().String()}
		execution, err := ExecuteHTTP(context.Background(), HTTPDependencies{ClientIdentity: testClientIdentity,
			Admitter:      admitter,
			Continuations: newMemoryContinuationCodec(),
			Resolver:      fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
			Dialer:        dialer,
		}, testHTTPRequest(t, operation, origin, time.Now().UTC(), linkInput(t, "open", new(uint64(1)), new(uint64(2)))))
		if !errors.Is(err, ErrNavigationConflict) || !execution.PhysicalSend {
			t.Fatalf("canonical route navigation execution=%#v error=%v", execution, err)
		}
		if requests.Load() != 1 || admitter.calls.Load() != 1 || dialer.calls.Load() != 1 {
			t.Fatalf("canonical route navigation requests=%d admits=%d dials=%d",
				requests.Load(), admitter.calls.Load(), dialer.calls.Load())
		}
	})

	t.Run("canonical route cursor target without a declared route policy keeps failing", func(t *testing.T) {
		var requests atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			requests.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("Link", `<`+providerTargetScheme(request)+`://`+request.Host+recordedGitHubIssueListNextTarget+`>; rel="next"`)
			_, _ = io.WriteString(writer, `[{"id":1,"open":true,"title":"one"},{"id":2,"open":true,"title":"two"}]`)
		}))
		defer server.Close()
		operation := testLinkOperation("link-recorded-github-navigation")
		origin := serverOrigin(t, server.URL)
		admitter := &testAdmitter{}
		dialer := &mappedDialer{address: server.Listener.Addr().String()}
		execution, err := ExecuteHTTP(context.Background(), HTTPDependencies{ClientIdentity: testClientIdentity,
			Admitter:      admitter,
			Continuations: newMemoryContinuationCodec(),
			Resolver:      fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
			Dialer:        dialer,
		}, testHTTPRequest(t, operation, origin, time.Now().UTC(), linkInput(t, "open", new(uint64(1)), new(uint64(2)))))
		if !errors.Is(err, ErrNavigationConflict) || !execution.PhysicalSend {
			t.Fatalf("recorded GitHub navigation execution=%#v error=%v", execution, err)
		}
		if requests.Load() != 1 || admitter.calls.Load() != 1 || dialer.calls.Load() != 1 {
			t.Fatalf("recorded GitHub navigation requests=%d admits=%d dials=%d",
				requests.Load(), admitter.calls.Load(), dialer.calls.Load())
		}
	})

	t.Run("provider cursor on the declared route is sealed and replayed verbatim", func(t *testing.T) {
		const cursor = `after=Y3Vyc29yOnYyOpLPAAABoLjpTwjPAAAAAUhclFY%3D`
		var (
			mu      sync.Mutex
			queries []string
		)
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			mu.Lock()
			queries = append(queries, request.URL.RawQuery)
			mu.Unlock()
			writer.Header().Set("Content-Type", "application/json")
			switch request.URL.Query().Get("page") {
			case "1":
				writer.Header().Set("Link",
					`<`+providerTargetScheme(request)+`://`+request.Host+`/items?state=open&page=2&per_page=2&`+cursor+`>; rel="next"`)
				_, _ = io.WriteString(writer, `[{"id":1,"open":true,"title":"one"},{"id":2,"open":true,"title":"two"}]`)
			case "2":
				_, _ = io.WriteString(writer, `[{"id":3,"open":true,"title":"three"}]`)
			default:
				t.Errorf("unexpected cursor page %q", request.URL.RawQuery)
				writer.WriteHeader(http.StatusBadRequest)
			}
		}))
		defer server.Close()
		operation := testLinkOperation("link-provider-cursor")
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
		first, err := ExecuteHTTP(context.Background(), dependencies,
			testHTTPRequest(t, operation, origin, time.Now().UTC(), linkInput(t, "open", new(uint64(1)), new(uint64(2)))))
		if err != nil {
			t.Fatalf("provider cursor first page: %v (cause %v)", err, errors.Unwrap(err))
		}
		page := first.Result.Pagination
		if page == nil || page.SourceExhausted || page.Continuation.Presence() != contract.PresenceValue {
			t.Fatalf("provider cursor pagination = %#v", page)
		}
		state := codec.mustState(t, linkTestBinding(operation.OperationID), page.Continuation)
		if state.NextTarget == nil || state.NextTarget.Origin != origin ||
			state.NextTarget.EscapedPath != "/items" ||
			state.NextTarget.RawQuery != "state=open&page=2&per_page=2&"+cursor {
			t.Fatalf("sealed provider target = %#v", state.NextTarget)
		}
		next, present := state.NextInputs["page"].ExactNumber()
		if !present || next.Text != "2" {
			t.Fatalf("sealed navigation = %#v", state.NextInputs)
		}
		// The provider cursor is server-owned state: a caller that names it as a
		// typed input is refused before any provider IO.
		conflicting := testHTTPRequest(t, operation, origin, time.Now().UTC(), linkInput(t, "open", nil, nil))
		conflicting.Continuation = page.Continuation
		conflicting.Input = contract.NewObjectValue(map[string]contract.PresentValue{
			"state": contract.NewStringValue("open"),
			"after": contract.NewStringValue("caller-supplied-cursor"),
		})
		if _, err := ExecuteHTTP(context.Background(), dependencies, conflicting); !errors.Is(err, ErrInvalidHTTPInput) {
			t.Fatalf("caller cursor injection error = %v", err)
		}
		if admitter.calls.Load() != 1 || dialer.calls.Load() != 1 {
			t.Fatalf("caller cursor injection performed IO: admits=%d dials=%d", admitter.calls.Load(), dialer.calls.Load())
		}

		continued := testHTTPRequest(t, operation, origin, time.Now().UTC(), linkInput(t, "open", nil, nil))
		continued.Continuation = page.Continuation
		followed, err := ExecuteHTTP(context.Background(), dependencies, continued)
		if err != nil {
			t.Fatalf("provider cursor continuation: %v (cause %v)", err, errors.Unwrap(err))
		}
		if followed.Result.Pagination == nil || !followed.Result.Pagination.SourceExhausted ||
			followed.Result.Pagination.Continuation.Presence() != contract.PresenceAbsent {
			t.Fatalf("provider cursor final pagination = %#v", followed.Result.Pagination)
		}
		mu.Lock()
		gotQueries := strings.Join(queries, "|")
		mu.Unlock()
		want := "state=open&page=1&per_page=2|state=open&page=2&per_page=2&" + cursor
		if gotQueries != want {
			t.Fatalf("provider cursor queries = %q; want %q", gotQueries, want)
		}
		if admitter.calls.Load() != 2 || dialer.calls.Load() != 2 {
			t.Fatalf("provider cursor admits=%d dials=%d", admitter.calls.Load(), dialer.calls.Load())
		}
	})

	t.Run("last page without a next relation is exhausted, never continued", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("Link",
				`<`+providerTargetScheme(request)+`://`+request.Host+`/items?state=open&page=1&per_page=2>; rel="first", `+
					`<`+providerTargetScheme(request)+`://`+request.Host+`/items?state=open&page=3&per_page=2>; rel="last"`)
			_, _ = io.WriteString(writer, `[{"id":1,"open":true,"title":"one"},{"id":2,"open":true,"title":"two"}]`)
		}))
		defer server.Close()
		operation := testLinkOperation("link-last-page")
		execution, err := ExecuteHTTP(context.Background(), HTTPDependencies{ClientIdentity: testClientIdentity,
			Admitter:      &testAdmitter{},
			Continuations: newMemoryContinuationCodec(),
			Resolver:      fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
			Dialer:        &mappedDialer{address: server.Listener.Addr().String()},
		}, testHTTPRequest(t, operation, serverOrigin(t, server.URL), time.Now().UTC(), linkInput(t, "open", new(uint64(1)), new(uint64(2)))))
		if err != nil {
			t.Fatalf("last Link page: %v", err)
		}
		page := execution.Result.Pagination
		if page == nil || !page.SourceExhausted || page.Continuation.Presence() != contract.PresenceAbsent ||
			len(page.UnreadTail) != 0 || page.FetchedCount != 2 || page.ReturnedCount != 2 || page.EmittedCount != 2 ||
			page.EffectiveWireLimit == nil || *page.EffectiveWireLimit != 2 {
			t.Fatalf("last Link pagination = %#v", page)
		}
	})

	t.Run("continuation naming another operation's navigation is refused before provider IO", func(t *testing.T) {
		operation := testOffsetOperation()
		if err := operation.Execution.Pagination.Validate(operation); err != nil {
			t.Fatalf("offset pagination fact: %v", err)
		}
		codec := newMemoryContinuationCodec()
		binding := ContinuationBinding{
			Scope:        contract.Scope{OrgID: "org-fixture", WorkspaceID: "workspace-fixture"},
			ConnectionID: "connection-fixture", OperationID: operation.OperationID,
			OperationRevision: "operation-revision-fixture", RunID: "run-fixture",
		}
		foreign, err := codec.SealContinuation(context.Background(), binding, ContinuationState{
			MediaType:  "application/json",
			UnreadTail: []contract.PresentValue{},
			NextInputs: map[string]contract.PresentValue{"page": testInteger(t, "2")},
		})
		if err != nil {
			t.Fatalf("seal foreign continuation: %v", err)
		}
		request := testHTTPRequest(t, operation, "https://provider.test", time.Now().UTC(), paginationInput(t, nil, nil))
		request.Continuation = foreign
		admitter := &testAdmitter{}
		dialer := &mappedDialer{address: "127.0.0.1:1"}
		execution, err := ExecuteHTTP(context.Background(), HTTPDependencies{ClientIdentity: testClientIdentity,
			Admitter:      admitter,
			Continuations: codec,
			Resolver:      fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
			Dialer:        dialer,
		}, request)
		if !errors.Is(err, ErrNavigationConflict) || execution.PhysicalSend {
			t.Fatalf("foreign continuation execution=%#v error=%v", execution, err)
		}
		if admitter.calls.Load() != 0 || dialer.calls.Load() != 0 {
			t.Fatalf("foreign continuation admits=%d dials=%d", admitter.calls.Load(), dialer.calls.Load())
		}
	})
}

// firstRecordedCursor and secondRecordedCursor are two distinct opaque cursors
// of the recorded GitHub navigation shape: a provider that paginates by cursor
// issues a fresh cursor for every page, so each followed hop replaces the
// cursor the runtime replayed from the sealed target before it.
const (
	firstRecordedCursor  = `Y3Vyc29yOnYyOpLPAAABoLjpTwjPAAAAAUhclFY%3D`
	secondRecordedCursor = `Y3Vyc29yOnYyOpLPAAABoLjpTwjPAAAAAUhcnVi%3D`
)

// linkRotationHop is one page of a scripted provider sequence: the records the
// page carries and, unless the provider ends the source at that page, the next
// target it declares.
type linkRotationHop struct {
	records string
	link    string
}

// linkRotationWindow serves a scripted multi-hop Link sequence to one read:
// every requested page answers with its scripted records and next target, and
// the window records the wire query of every request it received, in order.
type linkRotationWindow struct {
	server    *httptest.Server
	origin    string
	operation contract.Operation
	codec     *memoryContinuationCodec
	binding   ContinuationBinding
	admitter  *testAdmitter
	dialer    *mappedDialer

	mu      sync.Mutex
	queries []string
}

func startLinkRotationWindow(t *testing.T, script map[string]linkRotationHop) *linkRotationWindow {
	t.Helper()
	operation := testLinkOperation("link-provider-cursor-rotation")
	if err := operation.Execution.Pagination.Validate(operation); err != nil {
		t.Fatalf("Link pagination fact: %v", err)
	}
	window := &linkRotationWindow{
		operation: operation,
		codec:     newMemoryContinuationCodec(),
		binding:   linkTestBinding(operation.OperationID),
		admitter:  &testAdmitter{},
	}
	window.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		window.mu.Lock()
		window.queries = append(window.queries, request.URL.RawQuery)
		window.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		hop, scripted := script[request.URL.Query().Get("page")]
		if !scripted {
			t.Errorf("unscripted cursor rotation page %q", request.URL.RawQuery)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if hop.link != "" {
			writer.Header().Set("Link", hop.link)
		}
		_, _ = io.WriteString(writer, hop.records)
	}))
	t.Cleanup(window.server.Close)
	window.origin = serverOrigin(t, window.server.URL)
	window.dialer = &mappedDialer{address: window.server.Listener.Addr().String()}
	return window
}

// attempt performs one admitted attempt against the scripted sequence.
func (window *linkRotationWindow) attempt(t *testing.T, input, continuation contract.PresentValue) (HTTPExecution, error) {
	t.Helper()
	request := testHTTPRequest(t, window.operation, window.origin, time.Now().UTC(), input)
	request.Continuation = continuation
	return ExecuteHTTP(context.Background(), HTTPDependencies{ClientIdentity: testClientIdentity,
		Admitter:      window.admitter,
		Continuations: window.codec,
		Resolver:      fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
		Dialer:        window.dialer,
	}, request)
}

// follow performs the hop that must publish its page and returns the
// continuation the next hop resumes from together with its sealed state.
func (window *linkRotationWindow) follow(t *testing.T, hop int, ids string, input, continuation contract.PresentValue) (contract.PresentValue, ContinuationState) {
	t.Helper()
	page := window.served(t, hop, ids, input, continuation)
	if page.SourceExhausted || page.Continuation.Presence() != contract.PresenceValue {
		t.Fatalf("cursor rotation hop %d pagination = %#v", hop, page)
	}
	return page.Continuation, window.codec.mustState(t, window.binding, page.Continuation)
}

// exhaust performs the hop that must end the source without a continuation.
func (window *linkRotationWindow) exhaust(t *testing.T, hop int, ids string, input, continuation contract.PresentValue) {
	t.Helper()
	page := window.served(t, hop, ids, input, continuation)
	if !page.SourceExhausted || page.Continuation.Presence() != contract.PresenceAbsent {
		t.Fatalf("cursor rotation hop %d pagination = %#v", hop, page)
	}
}

// served performs the hop that must answer with a published page and returns
// that page.
func (window *linkRotationWindow) served(t *testing.T, hop int, ids string, input, continuation contract.PresentValue) *contract.ResultPagination {
	t.Helper()
	execution, err := window.attempt(t, input, continuation)
	if err != nil {
		t.Fatalf("cursor rotation hop %d: %v (cause %v)", hop, err, errors.Unwrap(err))
	}
	if execution.Result == nil || execution.Result.Pagination == nil {
		t.Fatalf("cursor rotation hop %d published no pagination fact", hop)
	}
	rows, _ := execution.Result.Data.Elements()
	if got := linkRecordIDs(rows); got != ids {
		t.Fatalf("cursor rotation hop %d records = %q; want %q", hop, got, ids)
	}
	return execution.Result.Pagination
}

// observed returns the wire queries the scripted sequence received, in order.
func (window *linkRotationWindow) observed() string {
	window.mu.Lock()
	defer window.mu.Unlock()
	return strings.Join(window.queries, "|")
}

// requireLinkRotationHop pins one hop's sealed navigation and provider target.
func requireLinkRotationHop(t *testing.T, hop int, state ContinuationState, page, target string) {
	t.Helper()
	next, present := state.NextInputs["page"].ExactNumber()
	if !present || next.Text != page || len(state.NextInputs) != 1 {
		t.Fatalf("cursor rotation hop %d sealed navigation = %#v; want page %s", hop, state.NextInputs, page)
	}
	if state.NextTarget == nil || state.NextTarget.RawQuery != target {
		t.Fatalf("cursor rotation hop %d sealed target = %#v; want query %q", hop, state.NextTarget, target)
	}
}

// linkRotationTarget renders one relative provider next target on the declared
// operation path, carrying the given query exactly as the provider wrote it.
func linkRotationTarget(query string) string {
	return `</items?` + query + `>; rel="next"`
}

// TestLinkProviderCursorRotation pins the boundary between the two query
// namespaces of one Link continuation. The declared query an accepted request
// carried is caller authority and every later provider target must keep it
// unchanged, but the opaque cursor namespace is the provider's own state: this
// runtime replays the cursor of the sealed target, and the provider's next
// target names the cursor of the page after that one. Each hop of such a
// sequence must therefore publish its page with a usable continuation and the
// last hop must end the source truthfully, while a target that changes a
// declared parameter, drops the sent page size or makes no navigation progress
// stays refused.
func TestLinkProviderCursorRotation(t *testing.T) {
	const firstPageQuery = "state=open&page=1&per_page=2"
	const firstHopRecords = `[{"id":1,"open":true,"title":"one"},{"id":2,"open":true,"title":"two"}]`
	firstHop := func(link string) linkRotationHop {
		return linkRotationHop{records: firstHopRecords, link: link}
	}
	// The sealed target keeps the provider's own parameter order; the request
	// that follows it is rendered in the declared field order with the
	// provider's opaque cursors appended verbatim.
	const (
		firstHopTarget   = "state=open&per_page=2&page=2&after=" + firstRecordedCursor
		firstHopQuery    = "state=open&page=2&per_page=2&after=" + firstRecordedCursor
		secondHopTarget  = "state=open&per_page=2&page=3&after=" + secondRecordedCursor
		secondHopQuery   = "state=open&page=3&per_page=2&after=" + secondRecordedCursor
		droppedHopTarget = "state=open&per_page=2&page=3"
		droppedHopQuery  = "state=open&page=3&per_page=2"
	)

	t.Run("a replaced opaque cursor is followed on every hop", func(t *testing.T) {
		window := startLinkRotationWindow(t, map[string]linkRotationHop{
			"1": firstHop(linkRotationTarget(firstHopTarget)),
			"2": {records: `[{"id":3,"open":true,"title":"three"}]`,
				link: linkRotationTarget(secondHopTarget)},
			"3": {records: `[{"id":4,"open":true,"title":"four"}]`},
		})
		continuation, state := window.follow(t, 1, "1,2", linkInput(t, "open", new(uint64(1)), new(uint64(2))), contract.AbsentValue())
		requireLinkRotationHop(t, 1, state, "2", firstHopTarget)

		continuation, state = window.follow(t, 2, "3", linkInput(t, "open", nil, nil), continuation)
		requireLinkRotationHop(t, 2, state, "3", secondHopTarget)

		window.exhaust(t, 3, "4", linkInput(t, "open", nil, nil), continuation)

		want := firstPageQuery + "|" + firstHopQuery + "|" + secondHopQuery
		if got := window.observed(); got != want {
			t.Fatalf("cursor rotation queried %q; want %q", got, want)
		}
		if window.admitter.calls.Load() != 3 || window.dialer.calls.Load() != 3 {
			t.Fatalf("cursor rotation admits=%d dials=%d", window.admitter.calls.Load(), window.dialer.calls.Load())
		}
	})

	t.Run("a dropped opaque cursor is never replayed", func(t *testing.T) {
		window := startLinkRotationWindow(t, map[string]linkRotationHop{
			"1": firstHop(linkRotationTarget(firstHopTarget)),
			"2": {records: `[{"id":3,"open":true,"title":"three"}]`,
				link: linkRotationTarget(droppedHopTarget)},
			"3": {records: `[{"id":4,"open":true,"title":"four"}]`},
		})
		continuation, state := window.follow(t, 1, "1,2", linkInput(t, "open", new(uint64(1)), new(uint64(2))), contract.AbsentValue())
		requireLinkRotationHop(t, 1, state, "2", firstHopTarget)

		continuation, state = window.follow(t, 2, "3", linkInput(t, "open", nil, nil), continuation)
		requireLinkRotationHop(t, 2, state, "3", droppedHopTarget)

		window.exhaust(t, 3, "4", linkInput(t, "open", nil, nil), continuation)

		want := firstPageQuery + "|" + firstHopQuery + "|" + droppedHopQuery
		if got := window.observed(); got != want {
			t.Fatalf("dropped cursor queried %q; want %q", got, want)
		}
	})

	t.Run("a rotating cursor never admits a declared change", func(t *testing.T) {
		for _, testCase := range []struct {
			name string
			link string
		}{
			{name: "changed accepted filter",
				link: linkRotationTarget("state=closed&per_page=2&page=3&after=" + secondRecordedCursor)},
			{name: "dropped sent page size",
				link: linkRotationTarget("state=open&page=3&after=" + secondRecordedCursor)},
			{name: "no navigation progress",
				link: linkRotationTarget("state=open&per_page=2&page=2&after=" + secondRecordedCursor)},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				window := startLinkRotationWindow(t, map[string]linkRotationHop{
					"1": firstHop(linkRotationTarget(firstHopTarget)),
					"2": {records: `[{"id":3,"open":true,"title":"three"}]`, link: testCase.link},
				})
				continuation, _ := window.follow(t, 1, "1,2", linkInput(t, "open", new(uint64(1)), new(uint64(2))), contract.AbsentValue())
				execution, err := window.attempt(t, linkInput(t, "open", nil, nil), continuation)
				if !errors.Is(err, ErrNavigationConflict) || !execution.PhysicalSend {
					t.Fatalf("rotating cursor execution=%#v error=%v", execution, err)
				}
				if execution.Result != nil && execution.Result.Pagination != nil {
					t.Fatalf("rotating cursor published %#v", execution.Result.Pagination)
				}
				want := firstPageQuery + "|" + firstHopQuery
				if got := window.observed(); got != want {
					t.Fatalf("rotating cursor queried %q; want %q", got, want)
				}
				if window.admitter.calls.Load() != 2 || window.dialer.calls.Load() != 2 {
					t.Fatalf("rotating cursor admits=%d dials=%d", window.admitter.calls.Load(), window.dialer.calls.Load())
				}
			})
		}
	})
}

// recordedGitHubCanonicalNextTarget is the provider navigation recorded from a
// credential-free read of
// https://api.github.com/repos/golang/go/issues?per_page=2&page=1 observed on
// 2026-09-19: HTTP 200 with a Link rel="next" naming the canonical
// repository-id route, an `after` cursor and the page that cursor addresses.
const recordedGitHubCanonicalNextTarget = `/repositories/23096959/issues?per_page=2&page=2&after=Y3Vyc29yOnYyOpLPAAABoLjpTwjPAAAAAUhclFY%3D`

// canonicalRouteNextTarget is the same recorded provider navigation with the
// accepted filter echoed the way the provider echoes it, so it exercises the
// declared-route admission path of one read that sent `state=open`.
const canonicalRouteNextTarget = `/repositories/23096959/issues?state=open&per_page=2&page=2&after=Y3Vyc29yOnYyOpLPAAABoLjpTwjPAAAAAUhclFY%3D`

// TestLinkCanonicalRouteContinuation pins the owner-approved continuation
// policy for the link kind: a provider-issued next target on a declared route
// is sealed with its cursor and followed, while every undeclared route, changed
// accepted parameter, tampered sealed target and foreign resource identity is
// still refused.
func TestLinkCanonicalRouteContinuation(t *testing.T) {
	t.Run("declared route admits the provider target and preserves the cursor verbatim", func(t *testing.T) {
		const cursor = `after=Y3Vyc29yOnYyOpLPAAABoLjpTwjPAAAAAUhclFY%3D`
		var (
			mu       sync.Mutex
			requests []string
		)
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			mu.Lock()
			requests = append(requests, request.URL.EscapedPath()+"?"+request.URL.RawQuery)
			mu.Unlock()
			writer.Header().Set("Content-Type", "application/json")
			switch request.URL.Query().Get("page") {
			case "1":
				writer.Header().Set("Link",
					`<`+providerTargetScheme(request)+`://`+request.Host+canonicalRouteNextTarget+`>; rel="next"`)
				_, _ = io.WriteString(writer,
					`[{"id":1,"open":true,"title":"one","repo":"golang/go"},{"id":2,"open":true,"title":"two","repo":"golang/go"}]`)
			case "2":
				_, _ = io.WriteString(writer, `[{"id":3,"open":true,"title":"three","repo":"golang/go"}]`)
			default:
				t.Errorf("unexpected canonical page %q", request.URL.RawQuery)
				writer.WriteHeader(http.StatusBadRequest)
			}
		}))
		defer server.Close()
		operation := testLinkRouteOperation("link-canonical-route")
		if err := operation.Execution.Pagination.Validate(operation); err != nil {
			t.Fatalf("Link route pagination fact: %v", err)
		}
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
		first, err := ExecuteHTTP(context.Background(), dependencies,
			testHTTPRequest(t, operation, origin, time.Now().UTC(), linkInput(t, "open", new(uint64(1)), new(uint64(2)))))
		if err != nil {
			t.Fatalf("canonical route first page: %v (cause %v)", err, errors.Unwrap(err))
		}
		page := first.Result.Pagination
		if page == nil || page.SourceExhausted || page.Continuation.Presence() != contract.PresenceValue {
			t.Fatalf("canonical route pagination = %#v", page)
		}
		state := codec.mustState(t, linkTestBinding(operation.OperationID), page.Continuation)
		if state.NextTarget == nil || state.NextTarget.Origin != origin ||
			state.NextTarget.EscapedPath != "/repositories/23096959/issues" ||
			state.NextTarget.RawQuery != "state=open&per_page=2&page=2&"+cursor {
			t.Fatalf("sealed canonical target = %#v", state.NextTarget)
		}
		identity, ok := state.RecordIdentity.TextValue()
		if !ok || identity != "golang/go" {
			t.Fatalf("sealed record identity = %#v", state.RecordIdentity)
		}

		continued := testHTTPRequest(t, operation, origin, time.Now().UTC(), linkInput(t, "open", nil, nil))
		continued.Continuation = page.Continuation
		followed, err := ExecuteHTTP(context.Background(), dependencies, continued)
		if err != nil {
			t.Fatalf("canonical route continuation: %v (cause %v)", err, errors.Unwrap(err))
		}
		if followed.Result.Pagination == nil || !followed.Result.Pagination.SourceExhausted ||
			followed.Result.Pagination.Continuation.Presence() != contract.PresenceAbsent ||
			followed.Result.Pagination.FetchedCount != 1 {
			t.Fatalf("canonical route final pagination = %#v", followed.Result.Pagination)
		}
		mu.Lock()
		got := strings.Join(requests, "|")
		mu.Unlock()
		want := "/items?state=open&page=1&per_page=2|/repositories/23096959/issues?state=open&page=2&per_page=2&" + cursor
		if got != want {
			t.Fatalf("canonical route requests = %q; want %q", got, want)
		}
		if admitter.calls.Load() != 2 || dialer.calls.Load() != 2 {
			t.Fatalf("canonical route admits=%d dials=%d", admitter.calls.Load(), dialer.calls.Load())
		}
	})

	t.Run("continuation response that identifies another resource is refused", func(t *testing.T) {
		var requests atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			requests.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			if request.URL.Query().Get("page") == "1" {
				writer.Header().Set("Link",
					`<`+providerTargetScheme(request)+`://`+request.Host+canonicalRouteNextTarget+`>; rel="next"`)
				_, _ = io.WriteString(writer,
					`[{"id":1,"open":true,"title":"one","repo":"golang/go"},{"id":2,"open":true,"title":"two","repo":"golang/go"}]`)
				return
			}
			// The provider answered the sealed target with records that identify
			// a different resource, so the continuation must be refused.
			_, _ = io.WriteString(writer, `[{"id":3,"open":true,"title":"three","repo":"evil/other"}]`)
		}))
		defer server.Close()
		operation := testLinkRouteOperation("link-canonical-foreign-resource")
		origin := serverOrigin(t, server.URL)
		codec := newMemoryContinuationCodec()
		dependencies := HTTPDependencies{ClientIdentity: testClientIdentity,
			Admitter:      &testAdmitter{},
			Continuations: codec,
			Resolver:      fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
			Dialer:        &mappedDialer{address: server.Listener.Addr().String()},
		}
		first, err := ExecuteHTTP(context.Background(), dependencies,
			testHTTPRequest(t, operation, origin, time.Now().UTC(), linkInput(t, "open", new(uint64(1)), new(uint64(2)))))
		if err != nil {
			t.Fatalf("foreign resource first page: %v", err)
		}
		continued := testHTTPRequest(t, operation, origin, time.Now().UTC(), linkInput(t, "open", nil, nil))
		continued.Continuation = first.Result.Pagination.Continuation
		execution, err := ExecuteHTTP(context.Background(), dependencies, continued)
		if !errors.Is(err, ErrNavigationConflict) || !execution.PhysicalSend {
			t.Fatalf("foreign resource execution=%#v error=%v", execution, err)
		}
		if requests.Load() != 2 {
			t.Fatalf("foreign resource requests = %d; want the one followed target", requests.Load())
		}
	})

	t.Run("tampered sealed targets are refused before provider IO", func(t *testing.T) {
		for _, testCase := range []struct {
			name   string
			mutate func(*ContinuationState)
		}{
			{name: "undeclared path", mutate: func(state *ContinuationState) {
				state.NextTarget.EscapedPath = "/repositories/23096959/other"
			}},
			{name: "changed accepted query", mutate: func(state *ContinuationState) {
				state.NextTarget.RawQuery = strings.Replace(state.NextTarget.RawQuery, "per_page=2", "per_page=50", 1)
			}},
			{name: "changed declared filter", mutate: func(state *ContinuationState) {
				state.NextTarget.RawQuery = strings.Replace(state.NextTarget.RawQuery, "page=2", "state=closed", 1)
			}},
			{name: "foreign origin", mutate: func(state *ContinuationState) {
				state.NextTarget.Origin = "https://evil.test"
			}},
			{name: "dropped record identity", mutate: func(state *ContinuationState) {
				state.RecordIdentity = nil
			}},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				var requests atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					requests.Add(1)
					writer.Header().Set("Content-Type", "application/json")
					writer.Header().Set("Link",
						`<`+providerTargetScheme(request)+`://`+request.Host+canonicalRouteNextTarget+`>; rel="next"`)
					_, _ = io.WriteString(writer,
						`[{"id":1,"open":true,"title":"one","repo":"golang/go"},{"id":2,"open":true,"title":"two","repo":"golang/go"}]`)
				}))
				defer server.Close()
				operation := testLinkRouteOperation("link-tampered-target")
				origin := serverOrigin(t, server.URL)
				codec := &mutatingContinuationCodec{
					memoryContinuationCodec: newMemoryContinuationCodec(),
					mutate:                  testCase.mutate,
				}
				admitter := &testAdmitter{}
				dialer := &mappedDialer{address: server.Listener.Addr().String()}
				dependencies := HTTPDependencies{ClientIdentity: testClientIdentity,
					Admitter:      admitter,
					Continuations: codec,
					Resolver:      fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
					Dialer:        dialer,
				}
				first, err := ExecuteHTTP(context.Background(), dependencies,
					testHTTPRequest(t, operation, origin, time.Now().UTC(), linkInput(t, "open", new(uint64(1)), new(uint64(2)))))
				if err != nil {
					t.Fatalf("tampered target first page: %v", err)
				}
				continued := testHTTPRequest(t, operation, origin, time.Now().UTC(), linkInput(t, "open", nil, nil))
				continued.Continuation = first.Result.Pagination.Continuation
				execution, err := ExecuteHTTP(context.Background(), dependencies, continued)
				if !errors.Is(err, ErrNavigationConflict) || execution.PhysicalSend {
					t.Fatalf("tampered target execution=%#v error=%v", execution, err)
				}
				if requests.Load() != 1 || admitter.calls.Load() != 1 || dialer.calls.Load() != 1 {
					t.Fatalf("tampered target requests=%d admits=%d dials=%d",
						requests.Load(), admitter.calls.Load(), dialer.calls.Load())
				}
			})
		}
	})

	t.Run("tampered sealed target that embeds the credential channel is refused", func(t *testing.T) {
		var requests atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			requests.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("Link",
				`<`+providerTargetScheme(request)+`://`+request.Host+canonicalRouteNextTarget+`>; rel="next"`)
			_, _ = io.WriteString(writer,
				`[{"id":1,"open":true,"title":"one","repo":"golang/go"},{"id":2,"open":true,"title":"two","repo":"golang/go"}]`)
		}))
		defer server.Close()
		operation := testLinkRouteOperation("link-tampered-credential")
		origin := serverOrigin(t, server.URL)
		operation.Inputs = append(operation.Inputs, contract.FieldBinding{
			FieldID: "auth.token", WireName: "token", Location: contract.FieldLocationQuery,
			JSONPointer: "/query/token", Required: false, Nullable: false,
			Style: "form", Explode: true, Sensitivity: contract.SensitivitySecret,
			Schema: contract.Schema{Type: contract.SchemaTypeString},
		})
		operation.Execution.HTTP = &contract.OperationHTTP{
			Origins: []string{origin},
			Authorization: &contract.OperationAuthorization{
				Required: false, Audience: origin, Placement: contract.FieldLocationQuery,
				WireName: "token", Prefix: "", FieldID: "auth.token",
			},
			Witness: testExecutionWitness(),
		}
		codec := &mutatingContinuationCodec{
			memoryContinuationCodec: newMemoryContinuationCodec(),
			mutate: func(state *ContinuationState) {
				state.NextTarget.RawQuery += "&token=attacker"
			},
		}
		admitter := &testAdmitter{}
		dialer := &mappedDialer{address: server.Listener.Addr().String()}
		dependencies := HTTPDependencies{ClientIdentity: testClientIdentity,
			Admitter:      admitter,
			Continuations: codec,
			Resolver:      fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
			Dialer:        dialer,
		}
		first, err := ExecuteHTTP(context.Background(), dependencies,
			testHTTPRequest(t, operation, origin, time.Now().UTC(), linkInput(t, "open", new(uint64(1)), new(uint64(2)))))
		if err != nil {
			t.Fatalf("credential target first page: %v (cause %v)", err, errors.Unwrap(err))
		}
		continued := testHTTPRequest(t, operation, origin, time.Now().UTC(), linkInput(t, "open", nil, nil))
		continued.Continuation = first.Result.Pagination.Continuation
		execution, err := ExecuteHTTP(context.Background(), dependencies, continued)
		if !errors.Is(err, ErrNavigationConflict) || execution.PhysicalSend {
			t.Fatalf("credential target execution=%#v error=%v", execution, err)
		}
		if requests.Load() != 1 || admitter.calls.Load() != 1 || dialer.calls.Load() != 1 {
			t.Fatalf("credential target requests=%d admits=%d dials=%d",
				requests.Load(), admitter.calls.Load(), dialer.calls.Load())
		}
	})

	t.Run("hostile canonical-route targets are refused before any resend", func(t *testing.T) {
		for _, testCase := range []struct {
			name string
			link string
		}{
			{name: "cross origin route", link: `<https://evil.test/repositories/23096959/issues?per_page=2&page=2>; rel="next"`},
			{name: "undeclared path", link: `</repositories/23096959/comments?per_page=2&page=2>; rel="next"`},
			{name: "encoded path separator", link: `</repositories/23096959%2F..%2Fother/issues?per_page=2&page=2>; rel="next"`},
			{name: "changed accepted filter", link: `</repositories/23096959/issues?per_page=2&page=2&state=closed>; rel="next"`},
			{name: "dropped page size", link: `</repositories/23096959/issues?page=2>; rel="next"`},
			{name: "ambiguous next", link: `</repositories/23096959/issues?per_page=2&page=2>; rel="next", </repositories/23096959/issues?per_page=2&page=3>; rel="next"`},
			{name: "no progress", link: `</repositories/23096959/issues?per_page=2&page=1>; rel="next"`},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				var requests atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					requests.Add(1)
					writer.Header().Set("Content-Type", "application/json")
					writer.Header().Set("Link", testCase.link)
					_, _ = io.WriteString(writer,
						`[{"id":1,"open":true,"title":"one","repo":"golang/go"},{"id":2,"open":true,"title":"two","repo":"golang/go"}]`)
				}))
				defer server.Close()
				operation := testLinkRouteOperation("link-hostile-route")
				origin := serverOrigin(t, server.URL)
				admitter := &testAdmitter{}
				dialer := &mappedDialer{address: server.Listener.Addr().String()}
				execution, err := ExecuteHTTP(context.Background(), HTTPDependencies{ClientIdentity: testClientIdentity,
					Admitter:      admitter,
					Continuations: newMemoryContinuationCodec(),
					Resolver:      fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
					Dialer:        dialer,
				}, testHTTPRequest(t, operation, origin, time.Now().UTC(), linkInput(t, "open", new(uint64(1)), new(uint64(2)))))
				if !errors.Is(err, ErrNavigationConflict) || !execution.PhysicalSend {
					t.Fatalf("hostile route execution=%#v error=%v", execution, err)
				}
				if requests.Load() != 1 || admitter.calls.Load() != 1 || dialer.calls.Load() != 1 {
					t.Fatalf("hostile route requests=%d admits=%d dials=%d",
						requests.Load(), admitter.calls.Load(), dialer.calls.Load())
				}
			})
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

// testLinkRouteOperation declares the owner-approved link continuation policy:
// the provider's canonical repository route is admitted for the same resource
// and the declared record field `repo` identifies that resource.
func testLinkRouteOperation(id string) contract.Operation {
	operation := testLinkOperation(id)
	schema := *operation.Responses[0].Schema
	record := *schema.Items
	record.Required = append(append([]string(nil), record.Required...), "repo")
	properties := make(map[string]contract.Schema, len(record.Properties)+1)
	for name, property := range record.Properties {
		properties[name] = property
	}
	properties["repo"] = contract.Schema{Type: contract.SchemaTypeString}
	record.Properties = properties
	schema.Items = &record
	operation.Responses[0].Schema = &schema
	operation.Execution.Pagination.LinkRoutes = []string{"/repositories/{repository_id}/issues"}
	operation.Execution.Pagination.LinkRecordIdentityPointer = "/repo"
	return operation
}

// mutatingContinuationCodec opens the sealed state and then tampers with it,
// which is the closest a test can come to mutating the sealed envelope bytes.
type mutatingContinuationCodec struct {
	*memoryContinuationCodec
	mutate func(*ContinuationState)
}

func (codec *mutatingContinuationCodec) OpenContinuation(
	ctx context.Context,
	binding ContinuationBinding,
	token contract.PresentValue,
) (ContinuationState, error) {
	state, err := codec.memoryContinuationCodec.OpenContinuation(ctx, binding, token)
	if err != nil {
		return state, err
	}
	codec.mutate(&state)
	return state, nil
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
