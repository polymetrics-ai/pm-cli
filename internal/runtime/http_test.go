package runtime

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// testClientIdentity is the bounded product identity the runtime tests configure
// for operations that declare no user-agent field of their own. The identity
// contract itself is exercised by TestProviderClientIdentity.
const testClientIdentity = ClientIdentity("pm-cli/test-fixture")

func TestTypedRequestAndResponseVariants(t *testing.T) {
	t.Run("exact typed provider wire preserves presence", func(t *testing.T) {
		root := contract.Schema{
			Type:     contract.SchemaTypeObject,
			Required: []string{"big", "empty", "flag", "nullable", "zero"},
			Properties: map[string]contract.Schema{
				"absent":   {Type: contract.SchemaTypeString},
				"big":      {Type: contract.SchemaTypeInteger},
				"empty":    {Type: contract.SchemaTypeString},
				"flag":     {Type: contract.SchemaTypeBool},
				"nullable": {Type: contract.SchemaTypeString, Nullable: true},
				"zero":     {Type: contract.SchemaTypeInteger},
			},
		}
		operation := testOperation("typed-wire", http.MethodPost, "/repos/{owner}/items", contract.Schema{Type: contract.SchemaTypeArray, Items: &contract.Schema{Type: contract.SchemaTypeString}})
		operation.Inputs = []contract.FieldBinding{
			testBinding("owner", "owner", contract.FieldLocationPath, true, contract.Schema{Type: contract.SchemaTypeString}, "simple", false),
			testBinding("empty-query", "empty", contract.FieldLocationQuery, false, contract.Schema{Type: contract.SchemaTypeString}, "form", true),
			testBinding("enabled", "enabled", contract.FieldLocationQuery, false, contract.Schema{Type: contract.SchemaTypeBool}, "form", true),
			testBinding("count", "count", contract.FieldLocationQuery, false, contract.Schema{Type: contract.SchemaTypeInteger}, "form", true),
			testBinding("labels", "label", contract.FieldLocationQuery, false, contract.Schema{Type: contract.SchemaTypeArray, Items: &contract.Schema{Type: contract.SchemaTypeString}}, "form", true),
			testBinding("absent-query", "missing", contract.FieldLocationQuery, false, contract.Schema{Type: contract.SchemaTypeString}, "form", true),
			testBinding("mode", "X-Mode", contract.FieldLocationHeader, false, contract.Schema{Type: contract.SchemaTypeString}, "simple", false),
			testBinding("session", "session", contract.FieldLocationCookie, false, contract.Schema{Type: contract.SchemaTypeString}, "form", true),
		}
		sourceDefault := contract.NewStringValue("provider-default")
		operation.Inputs[5].SourceDefault = &sourceDefault
		operation.RequestVariants = []contract.RequestVariant{{
			VariantID: "json-body",
			MediaType: "application/json",
			Required:  true,
			Schema:    root,
			Inputs: []contract.FieldBinding{{
				FieldID: "body", WireName: "", Location: contract.FieldLocationBody, JSONPointer: "", Required: true,
				Nullable: false, Style: "json", Explode: false, AllowReserved: false, Sensitivity: contract.SensitivityPublic, Schema: root,
			}},
			SourceRefs: []contract.SourceRef{{SourceID: "fixture", Pointer: "#/requestBody"}},
		}}
		input := contract.NewObjectValue(map[string]contract.PresentValue{
			"owner":        contract.NewStringValue("org/name"),
			"empty-query":  contract.NewStringValue(""),
			"enabled":      contract.NewBooleanValue(false),
			"count":        testInteger(t, "0"),
			"labels":       contract.NewArrayValue([]contract.PresentValue{contract.NewStringValue("alpha"), contract.NewStringValue("beta")}),
			"absent-query": contract.AbsentValue(),
			"mode":         contract.NewStringValue(""),
			"session":      contract.NewStringValue("opaque"),
			"body": contract.NewObjectValue(map[string]contract.PresentValue{
				"absent":   contract.AbsentValue(),
				"big":      testInteger(t, "9007199254740993"),
				"empty":    contract.NewStringValue(""),
				"flag":     contract.NewBooleanValue(false),
				"nullable": contract.NullValue(),
				"zero":     testInteger(t, "0"),
			}),
		})
		encoded, err := EncodeInputs(operation, input)
		if err != nil {
			t.Fatalf("EncodeInputs: %v", err)
		}
		if encoded.EscapedPath != "/repos/org%2Fname/items" {
			t.Fatalf("path = %q", encoded.EscapedPath)
		}
		if encoded.RawQuery != "empty=&enabled=false&count=0&label=alpha&label=beta" {
			t.Fatalf("query = %q", encoded.RawQuery)
		}
		if got := encoded.Header.Values("X-Mode"); len(got) != 1 || got[0] != "" {
			t.Fatalf("X-Mode = %#v", got)
		}
		if got := encoded.Header.Get("Cookie"); got != "session=opaque" {
			t.Fatalf("Cookie = %q", got)
		}
		if encoded.MediaType != "application/json" || encoded.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("media = %q, header = %q", encoded.MediaType, encoded.Header.Get("Content-Type"))
		}
		wantBody := `{"big":9007199254740993,"empty":"","flag":false,"nullable":null,"zero":0}`
		if string(encoded.Body) != wantBody {
			t.Fatalf("body = %s", encoded.Body)
		}
	})

	t.Run("exact status media and no content remain distinct", func(t *testing.T) {
		schema := contract.Schema{Type: contract.SchemaTypeObject, Required: []string{"accepted"}, Properties: map[string]contract.Schema{"accepted": {Type: contract.SchemaTypeBool}}}
		operation := testOperation("response-variants", http.MethodPost, "/items", schema)
		operation.Responses = []contract.Response{
			{Status: "201", MediaType: "application/json", Schema: &schema, SourceRefs: testSourceRefs("#/responses/201")},
			{Status: "202", MediaType: "application/vnd.fixture+json", Schema: &schema, SourceRefs: testSourceRefs("#/responses/202")},
			{Status: "204", NoContent: true, SourceRefs: testSourceRefs("#/responses/204")},
			{Status: "default", MediaType: "application/json", Schema: &schema, SourceRefs: testSourceRefs("#/responses/default")},
		}
		created, err := DecodeResponse(operation, 201, http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}, []byte(`{"accepted":false}`))
		if err != nil || created.StatusCode != 201 || created.Result == nil {
			t.Fatalf("201 decode = %#v, %v", created, err)
		}
		accepted, err := DecodeResponse(operation, 202, http.Header{"Content-Type": []string{"application/vnd.fixture+json"}}, []byte(`{"accepted":true}`))
		if err != nil || accepted.StatusCode != 202 || accepted.Result == nil || accepted.Result.MediaType != "application/vnd.fixture+json" {
			t.Fatalf("202 decode = %#v, %v", accepted, err)
		}
		noContent, err := DecodeResponse(operation, 204, nil, nil)
		if err != nil || noContent.Result == nil || noContent.Result.Data.Presence() != contract.PresenceAbsent ||
			noContent.Result.MediaType != "" || noContent.Result.Pagination != nil {
			t.Fatalf("204 decode = %#v, %v", noContent, err)
		}
		fallback, err := DecodeResponse(operation, 418, http.Header{"Content-Type": []string{"application/json"}}, []byte(`{"accepted":true}`))
		if err != nil || fallback.StatusCode != 418 || fallback.Result == nil {
			t.Fatalf("default decode = %#v, %v", fallback, err)
		}
		if _, err := DecodeResponse(operation, 202, http.Header{"Content-Type": []string{"application/json"}}, []byte(`{"accepted":true}`)); !errors.Is(err, ErrUnexpectedHTTPResponse) {
			t.Fatalf("media mismatch error = %v", err)
		}
	})

	t.Run("one admitted request reaches the peer exactly once", func(t *testing.T) {
		var requests atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			requests.Add(1)
			if request.URL.EscapedPath() != "/items" || request.URL.RawQuery != "state=open" {
				t.Errorf("wire URL = %s", request.URL.String())
			}
			if request.Header.Get("Accept") != "application/vnd.github+json" ||
				request.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
				t.Errorf("constant source headers = %#v", request.Header)
			}
			if request.UserAgent() != string(testClientIdentity) {
				t.Errorf("provider client identity = %q, want %q", request.UserAgent(), testClientIdentity)
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `[{"id":9007199254740993,"open":false,"title":""}]`)
		}))
		defer server.Close()
		operation := testListOperation("one-request", http.MethodGet, "/items")
		operation.Inputs = []contract.FieldBinding{testBinding("state", "state", contract.FieldLocationQuery, true, contract.Schema{Type: contract.SchemaTypeString}, "form", true)}
		now := time.Now().UTC()
		admitter := &testAdmitter{}
		dialer := &mappedDialer{address: server.Listener.Addr().String()}
		request := testHTTPRequest(t, operation, serverOrigin(t, server.URL), now, contract.NewObjectValue(map[string]contract.PresentValue{"state": contract.NewStringValue("open")}))
		request.Operation.Execution.HTTP.Headers = []contract.OperationHTTPHeader{
			{Name: "Accept", Value: "application/vnd.github+json"},
			{Name: "X-GitHub-Api-Version", Value: "2022-11-28"},
		}
		execution, err := ExecuteHTTP(context.Background(), HTTPDependencies{ClientIdentity: testClientIdentity,
			Admitter: admitter,
			Resolver: fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
			Dialer:   dialer,
		}, request)
		if err != nil {
			t.Fatalf("ExecuteHTTP: %v", err)
		}
		if !execution.PhysicalSend || execution.Result == nil || requests.Load() != 1 || admitter.calls.Load() != 1 || dialer.calls.Load() != 1 {
			t.Fatalf("execution=%#v requests=%d admits=%d dials=%d", execution, requests.Load(), admitter.calls.Load(), dialer.calls.Load())
		}
		records, ok := execution.Result.Data.Elements()
		if !ok || len(records) != 1 {
			t.Fatalf("typed rows = %#v", execution.Result.Data)
		}
		fields, _ := records[0].Fields()
		id, _ := fields["id"].ExactNumber()
		open, _ := fields["open"].Boolean()
		title, _ := fields["title"].TextValue()
		if id.Text != "9007199254740993" || open || title != "" {
			t.Fatalf("typed row = %#v", fields)
		}
	})

	t.Run("constant header collisions fail before provider IO", func(t *testing.T) {
		operation := testListOperation("constant-header-collision", http.MethodGet, "/items")
		operation.Inputs = []contract.FieldBinding{
			testBinding("version", "X-GitHub-Api-Version", contract.FieldLocationHeader, false, contract.Schema{Type: contract.SchemaTypeString}, "simple", false),
		}
		request := testHTTPRequest(t, operation, "https://provider.test", time.Now().UTC(), contract.NewObjectValue(map[string]contract.PresentValue{
			"version": contract.NewStringValue("caller-version"),
		}))
		request.Operation.Execution.HTTP.Headers = []contract.OperationHTTPHeader{{Name: "X-GitHub-Api-Version", Value: "source-version"}}
		admitter := &testAdmitter{}
		dialer := &mappedDialer{address: "127.0.0.1:1"}
		_, err := ExecuteHTTP(context.Background(), HTTPDependencies{ClientIdentity: testClientIdentity, Admitter: admitter, Dialer: dialer}, request)
		if !errors.Is(err, ErrUnsupportedHTTPTransport) && !errors.Is(err, ErrInvalidHTTPInput) {
			t.Fatalf("error = %v", err)
		}
		if admitter.calls.Load() != 0 || dialer.calls.Load() != 0 {
			t.Fatalf("admits=%d dials=%d", admitter.calls.Load(), dialer.calls.Load())
		}
	})

	t.Run("source server base path is retained", func(t *testing.T) {
		var requests atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			requests.Add(1)
			if request.URL.EscapedPath() != "/api/v3/items" {
				t.Errorf("path = %q", request.URL.EscapedPath())
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `[]`)
		}))
		defer server.Close()
		origin := serverOrigin(t, server.URL) + "/api/v3"
		request := testHTTPRequest(t, testListOperation("base-path", http.MethodGet, "/items"), origin, time.Now().UTC(), contract.NewObjectValue(nil))
		dialer := &mappedDialer{address: server.Listener.Addr().String()}
		execution, err := ExecuteHTTP(context.Background(), HTTPDependencies{ClientIdentity: testClientIdentity,
			Admitter: &testAdmitter{},
			Resolver: fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
			Dialer:   dialer,
		}, request)
		if err != nil || execution.Result == nil || requests.Load() != 1 || dialer.calls.Load() != 1 {
			t.Fatalf("execution=%#v requests=%d dials=%d error=%v", execution, requests.Load(), dialer.calls.Load(), err)
		}
	})

	t.Run("missing required input performs zero provider IO", func(t *testing.T) {
		operation := testListOperation("required-input", http.MethodGet, "/repos/{owner}/items")
		operation.Inputs = []contract.FieldBinding{testBinding("owner", "owner", contract.FieldLocationPath, true, contract.Schema{Type: contract.SchemaTypeString}, "simple", false)}
		now := time.Now().UTC()
		admitter := &testAdmitter{}
		dialer := &mappedDialer{address: "127.0.0.1:1"}
		_, err := ExecuteHTTP(context.Background(), HTTPDependencies{ClientIdentity: testClientIdentity,
			Admitter: admitter,
			Resolver: fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
			Dialer:   dialer,
		}, testHTTPRequest(t, operation, "http://provider.test", now, contract.NewObjectValue(nil)))
		if !errors.Is(err, ErrInvalidHTTPInput) {
			t.Fatalf("error = %v", err)
		}
		if admitter.calls.Load() != 0 || dialer.calls.Load() != 0 {
			t.Fatalf("admits=%d dials=%d", admitter.calls.Load(), dialer.calls.Load())
		}
	})
}

// TestProviderClientIdentity pins the runtime's client identity contract against
// a controlled peer. A provider that requires a client identity is answered by
// the process's own bounded product identity when, and only when, the operation
// declares no user-agent field of its own; a declared user-agent field always
// wins; and a process without a usable identity fails closed instead of sending
// an empty value.
func TestProviderClientIdentity(t *testing.T) {
	type observation struct {
		agent   string
		present bool
	}
	newPeer := func(t *testing.T) (*httptest.Server, <-chan observation) {
		t.Helper()
		observed := make(chan observation, 1)
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			_, present := request.Header["User-Agent"]
			observed <- observation{agent: request.UserAgent(), present: present}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `[]`)
		}))
		t.Cleanup(server.Close)
		return server, observed
	}
	run := func(t *testing.T, dependencies HTTPDependencies, operation contract.Operation, origin string, input contract.PresentValue) (HTTPExecution, error) {
		t.Helper()
		return ExecuteHTTP(context.Background(), dependencies, testHTTPRequest(t, operation, origin, time.Now().UTC(), input))
	}

	t.Run("configured product identity reaches the peer exactly", func(t *testing.T) {
		server, observed := newPeer(t)
		identity := ProductClientIdentity("test-version")
		if identity != "pm-cli/test-version" {
			t.Fatalf("product identity = %q", identity)
		}
		admitter := &testAdmitter{}
		dialer := &mappedDialer{address: server.Listener.Addr().String()}
		execution, err := run(t, HTTPDependencies{
			ClientIdentity: identity,
			Admitter:       admitter,
			Resolver:       fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
			Dialer:         dialer,
		}, testListOperation("client-identity-default", http.MethodGet, "/items"), serverOrigin(t, server.URL), contract.NewObjectValue(nil))
		if err != nil || !execution.PhysicalSend || dialer.calls.Load() != 1 {
			t.Fatalf("execution=%#v dials=%d error=%v", execution, dialer.calls.Load(), err)
		}
		if got := <-observed; !got.present || got.agent != "pm-cli/test-version" {
			t.Fatalf("peer observed client identity = %#v, want %q", got, identity)
		}
	})

	t.Run("declared user agent field wins over the runtime identity", func(t *testing.T) {
		server, observed := newPeer(t)
		operation := testListOperation("client-identity-declared", http.MethodGet, "/items")
		operation.Inputs = []contract.FieldBinding{
			testBinding("header.user-agent", "User-Agent", contract.FieldLocationHeader, true, contract.Schema{Type: contract.SchemaTypeString}, "simple", false),
		}
		execution, err := run(t, HTTPDependencies{
			ClientIdentity: ProductClientIdentity("test-version"),
			Admitter:       &testAdmitter{},
			Resolver:       fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
			Dialer:         &mappedDialer{address: server.Listener.Addr().String()},
		}, operation, serverOrigin(t, server.URL), contract.NewObjectValue(map[string]contract.PresentValue{
			"header.user-agent": contract.NewStringValue("caller-agent/1"),
		}))
		if err != nil || !execution.PhysicalSend {
			t.Fatalf("execution=%#v error=%v", execution, err)
		}
		if got := <-observed; !got.present || got.agent != "caller-agent/1" {
			t.Fatalf("peer observed declared client identity = %#v, want %q", got, "caller-agent/1")
		}
	})

	t.Run("declared user agent field that stayed absent is never substituted", func(t *testing.T) {
		server, observed := newPeer(t)
		operation := testListOperation("client-identity-absent", http.MethodGet, "/items")
		operation.Inputs = []contract.FieldBinding{
			testBinding("header.user-agent", "User-Agent", contract.FieldLocationHeader, false, contract.Schema{Type: contract.SchemaTypeString}, "simple", false),
		}
		execution, err := run(t, HTTPDependencies{
			ClientIdentity: ProductClientIdentity("test-version"),
			Admitter:       &testAdmitter{},
			Resolver:       fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
			Dialer:         &mappedDialer{address: server.Listener.Addr().String()},
		}, operation, serverOrigin(t, server.URL), contract.NewObjectValue(nil))
		if err != nil || !execution.PhysicalSend {
			t.Fatalf("execution=%#v error=%v", execution, err)
		}
		if got := <-observed; got.present || got.agent != "" {
			t.Fatalf("peer observed substituted client identity = %#v, want an absent field", got)
		}
	})

	t.Run("unconfigured identity fails closed without provider IO", func(t *testing.T) {
		server, observed := newPeer(t)
		admitter := &testAdmitter{}
		dialer := &mappedDialer{address: server.Listener.Addr().String()}
		_, err := run(t, HTTPDependencies{
			Admitter: admitter,
			Dialer:   dialer,
		}, testListOperation("client-identity-unconfigured", http.MethodGet, "/items"), serverOrigin(t, server.URL), contract.NewObjectValue(nil))
		if !errors.Is(err, ErrClientIdentityRequired) {
			t.Fatalf("error = %v", err)
		}
		if admitter.calls.Load() != 0 || dialer.calls.Load() != 0 {
			t.Fatalf("admits=%d dials=%d", admitter.calls.Load(), dialer.calls.Load())
		}
		select {
		case got := <-observed:
			t.Fatalf("peer observed %#v", got)
		default:
		}
	})

	t.Run("malformed identity fails closed without provider IO", func(t *testing.T) {
		cases := []struct {
			name     string
			identity ClientIdentity
		}{
			{name: "padded", identity: ClientIdentity("pm-cli/test-version ")},
			{name: "control", identity: ClientIdentity("pm-cli/test-version\r\nx-injected: 1")},
			{name: "oversized", identity: ProductClientIdentity(strings.Repeat("version-", 40))},
			{name: "empty", identity: ClientIdentity("")},
		}
		for _, testCase := range cases {
			t.Run(testCase.name, func(t *testing.T) {
				server, observed := newPeer(t)
				admitter := &testAdmitter{}
				dialer := &mappedDialer{address: server.Listener.Addr().String()}
				_, err := run(t, HTTPDependencies{
					ClientIdentity: testCase.identity,
					Admitter:       admitter,
					Dialer:         dialer,
				}, testListOperation("client-identity-malformed", http.MethodGet, "/items"), serverOrigin(t, server.URL), contract.NewObjectValue(nil))
				if !errors.Is(err, ErrClientIdentityRequired) {
					t.Fatalf("error = %v", err)
				}
				if admitter.calls.Load() != 0 || dialer.calls.Load() != 0 {
					t.Fatalf("admits=%d dials=%d", admitter.calls.Load(), dialer.calls.Load())
				}
				select {
				case got := <-observed:
					t.Fatalf("peer observed %#v", got)
				default:
				}
			})
		}
	})
}

type fixedResolver struct {
	addresses []netip.Addr
	err       error
}

func (r fixedResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return append([]netip.Addr(nil), r.addresses...), r.err
}

type mappedDialer struct {
	address string
	err     error
	calls   atomic.Int64
}

func (d *mappedDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	d.calls.Add(1)
	if d.err != nil {
		return nil, d.err
	}
	return (&net.Dialer{}).DialContext(ctx, network, d.address)
}

type testAdmitter struct {
	err   error
	calls atomic.Int64
}

func (a *testAdmitter) AdmitPhysicalCall(ctx context.Context, _ PhysicalCall) error {
	a.calls.Add(1)
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("unbounded context")
	}
	return a.err
}

func testOperation(id, method, path string, responseSchema contract.Schema) contract.Operation {
	return contract.Operation{
		SchemaVersion:   contract.OperationSchemaVersion,
		OperationID:     id,
		SourceRefs:      testSourceRefs("#/operation"),
		Transport:       contract.Transport{Kind: contract.TransportKindHTTP, Method: method, Path: path},
		Inputs:          []contract.FieldBinding{},
		RequestVariants: []contract.RequestVariant{},
		Responses: []contract.Response{{
			Status: "200", MediaType: "application/json", Schema: &responseSchema, SourceRefs: testSourceRefs("#/responses/200"),
		}},
		CapabilityRequirements: []contract.CapabilityRequirement{},
		LaneEligibility:        []contract.LaneEligibility{{Lane: contract.LaneDirectRead, Status: contract.LaneEligible, Reason: "fixture"}},
	}
}

func testListOperation(id, method, path string) contract.Operation {
	record := contract.Schema{
		Type:     contract.SchemaTypeObject,
		Required: []string{"id", "open", "title"},
		Properties: map[string]contract.Schema{
			"id":    {Type: contract.SchemaTypeInteger},
			"open":  {Type: contract.SchemaTypeBool},
			"title": {Type: contract.SchemaTypeString},
		},
	}
	return testOperation(id, method, path, contract.Schema{Type: contract.SchemaTypeArray, Items: &record})
}

func testBinding(fieldID, wireName string, location contract.FieldLocation, required bool, schema contract.Schema, style string, explode bool) contract.FieldBinding {
	return contract.FieldBinding{
		FieldID: fieldID, WireName: wireName, Location: location,
		JSONPointer: "/" + string(location) + "/" + wireName,
		Required:    required, Nullable: schema.Nullable, Style: style, Explode: explode,
		AllowReserved: false, Sensitivity: contract.SensitivityPublic, Schema: schema,
	}
}

func testSourceRefs(pointer string) []contract.SourceRef {
	return []contract.SourceRef{{SourceID: "fixture-source", Pointer: pointer}}
}

func testInteger(t *testing.T, exact string) contract.PresentValue {
	t.Helper()
	value, err := contract.NewIntegerValue(exact)
	if err != nil {
		t.Fatalf("NewIntegerValue(%q): %v", exact, err)
	}
	return value
}

func testHTTPRequest(t *testing.T, operation contract.Operation, origin string, now time.Time, input contract.PresentValue) HTTPRequest {
	t.Helper()
	dispatchID, err := contract.NewDispatchID("dispatch-" + operation.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := contract.NewAttemptID("attempt-" + operation.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := contract.CanonicalDigest([]byte(`{"fixture":true}`))
	if err != nil {
		t.Fatal(err)
	}
	execution := contract.OperationExecution{}
	if operation.Execution != nil {
		execution = *operation.Execution
	}
	if execution.HTTP == nil {
		execution.HTTP = testHTTPFacts(origin)
	}
	operation.Execution = &execution
	return HTTPRequest{
		Operation: operation,
		Egress:    EgressPolicy{SelectedOrigin: origin},
		Call: PhysicalCall{
			Scope:        contract.Scope{OrgID: "org-fixture", WorkspaceID: "workspace-fixture"},
			ConnectionID: "connection-fixture", OperationID: operation.OperationID, OperationRevision: "operation-revision-fixture",
			RunID: "run-fixture", TaskID: "task-fixture", ExecutionGrantRef: "grant-fixture", GrantEpoch: 1,
			DispatchID: dispatchID,
			Attempt:    contract.Attempt{AttemptID: attemptID, DispatchID: dispatchID, ReceiptRef: "receipt-fixture", RecordedAt: now.Add(-time.Second).UTC()},
			PermitRef:  "permit-fixture", AccountWitness: "account-fixture",
			Budget:   contract.BudgetBinding{BudgetRef: "budget-fixture", Revision: 1, Digest: digest},
			Capacity: contract.CapacityReservation{OutcomeSlots: 1, EncodedBytes: 1 << 20},
			Deadline: now.Add(30 * time.Second).UTC(),
		},
		Input: input,
		Limits: CallLimits{
			MaxRequestBytes: 1 << 20, MaxResponseBytes: 1 << 20, MaxDNSAnswers: 8,
			AttemptNumber: 1, MaxAttempts: 3, MaxRetryDelay: time.Minute,
		},
		Continuation: contract.AbsentValue(),
	}
}

func testHTTPFacts(origin string) *contract.OperationHTTP {
	return &contract.OperationHTTP{
		Origins: []string{origin},
		Witness: contract.ExecutionWitness{
			ProofID:    "fixture-http-proof",
			SourceRefs: testSourceRefs("#/servers/0"),
		},
	}
}

func serverOrigin(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Scheme + "://provider.test:" + parsed.Port()
}

func parseServerPort(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Port() == "" {
		t.Fatal("server port is absent")
	}
	return parsed.Port()
}

func assertNoSecret(t *testing.T, err error, secret string) {
	t.Helper()
	for current := err; current != nil; current = errors.Unwrap(current) {
		if strings.Contains(current.Error(), secret) {
			t.Fatalf("error exposed credential %q", secret)
		}
	}
}
