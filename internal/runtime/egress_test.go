package runtime

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/credentials"
)

func TestDestinationDNSRedirectAuthAudience(t *testing.T) {
	t.Run("metadata private and mixed DNS answers are denied", func(t *testing.T) {
		for _, address := range []string{
			"169.254.169.254", "127.0.0.1", "10.0.0.7", "::1", "fc00::1",
			"64:ff9b::a00:1", "2002:a00:1::",
		} {
			if err := validatePublicHTTPAddress(netip.MustParseAddr(address)); !errors.Is(err, ErrDestinationDenied) {
				t.Fatalf("address %s error = %v", address, err)
			}
		}
		resolver := fixedResolver{addresses: []netip.Addr{
			netip.MustParseAddr("93.184.216.34"),
			netip.MustParseAddr("169.254.169.254"),
		}}
		if _, err := resolveAddresses(context.Background(), resolver, "provider.test", 4); !errors.Is(err, ErrDestinationDenied) {
			t.Fatalf("mixed DNS error = %v", err)
		}
		capture := &captureDialer{err: errors.New("captured")}
		origin, err := canonicalOrigin("https://provider.test")
		if err != nil {
			t.Fatal(err)
		}
		publicResolver := &countingResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}}
		dial := newPinnedDialContext(publicResolver, capture, origin, 4)
		_, _ = dial(context.Background(), "tcp", "provider.test:443")
		_, _ = dial(context.Background(), "tcp", "provider.test:443")
		if publicResolver.calls != 1 ||
			strings.Join(capture.addresses, ",") != "93.184.216.34:443,93.184.216.34:443" {
			t.Fatalf("resolver calls=%d dial addresses=%v", publicResolver.calls, capture.addresses)
		}
	})

	t.Run("cross-audience redirect is denied and authorization is never forwarded", func(t *testing.T) {
		const secret = "credential-canary"
		var firstRequests atomic.Int64
		var secondRequests atomic.Int64
		second := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			secondRequests.Add(1)
		}))
		defer second.Close()
		first := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			firstRequests.Add(1)
			if got := request.Header.Get("Authorization"); got != "Bearer "+secret {
				t.Errorf("Authorization = %q", got)
			}
			writer.Header().Set("Location", second.URL+"/metadata")
			writer.WriteHeader(http.StatusFound)
		}))
		defer first.Close()

		operation := testOperation("redirect", http.MethodGet, "/redirect", contract.Schema{Type: contract.SchemaTypeString})
		operation.Inputs = []contract.FieldBinding{testSecretHeaderBinding("authorization", "Authorization")}
		operation.Responses = []contract.Response{{Status: "302", NoContent: true, SourceRefs: testSourceRefs("#/responses/302")}}
		now := time.Now().UTC()
		origin := serverOrigin(t, first.URL)
		request := testHTTPRequest(t, operation, origin, now, contract.NewObjectValue(nil))
		request.Operation.Execution.HTTP.Authorization = &contract.OperationAuthorization{
			Required: true, Audience: origin, Placement: contract.FieldLocationHeader,
			WireName: "Authorization", Prefix: "Bearer ", FieldID: "authorization",
		}
		resolver := &staticCredentialResolver{secret: []byte(secret), call: request.Call}
		execution, err := ExecuteHTTP(context.Background(), HTTPDependencies{
			Admitter:    &testAdmitter{},
			Credentials: resolver,
			Resolver:    fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
			Dialer:      &mappedDialer{address: first.Listener.Addr().String()},
		}, request)
		if !errors.Is(err, ErrDestinationDenied) {
			assertNoSecret(t, err, secret)
			t.Fatalf("ExecuteHTTP error = %v", err)
		}
		if execution.Disposition != DispositionResponded || !execution.PhysicalSend || execution.Result != nil {
			t.Fatalf("redirect execution = %#v", execution)
		}
		if firstRequests.Load() != 1 || secondRequests.Load() != 0 || resolver.calls.Load() != 1 {
			t.Fatalf("first=%d second=%d credentials=%d", firstRequests.Load(), secondRequests.Load(), resolver.calls.Load())
		}
	})

	t.Run("query authorization is absent from errors observations and results", func(t *testing.T) {
		const secret = "query-credential-canary"
		operation := testOperation("query-auth", http.MethodGet, "/items", contract.Schema{Type: contract.SchemaTypeString})
		operation.Inputs = []contract.FieldBinding{{
			FieldID: "token", WireName: "token", Location: contract.FieldLocationQuery,
			JSONPointer: "/query/token", Required: true, Nullable: false,
			Style: "form", Explode: true, AllowReserved: false,
			Sensitivity: contract.SensitivitySecret, Schema: contract.Schema{Type: contract.SchemaTypeString},
		}}
		now := time.Now().UTC()
		request := testHTTPRequest(t, operation, "https://provider.test", now, contract.NewObjectValue(nil))
		request.Egress.RequireTLS = true
		request.Operation.Execution.HTTP.Authorization = &contract.OperationAuthorization{
			Required: true, Audience: "https://provider.test", Placement: contract.FieldLocationQuery,
			WireName: "token", FieldID: "token",
		}
		resolver := &staticCredentialResolver{secret: []byte(secret), call: request.Call}
		execution, err := ExecuteHTTP(context.Background(), HTTPDependencies{
			Admitter:    &testAdmitter{},
			Credentials: resolver,
			Resolver:    fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
			Dialer:      &captureDialer{err: errors.New(secret)},
		}, request)
		if !errors.Is(err, ErrHTTPTransport) {
			t.Fatalf("error = %T %v", err, err)
		}
		assertNoSecret(t, err, secret)
		if execution.Wire.RawQuery != "" || execution.Result != nil ||
			resolver.last == nil || !resolver.last.destroyed {
			t.Fatalf("execution=%#v material=%#v", execution, resolver.last)
		}
	})

	t.Run("caller query cannot shadow the source authorization channel", func(t *testing.T) {
		operation := testOperation("query-auth-collision", http.MethodGet, "/items", contract.Schema{Type: contract.SchemaTypeString})
		search := testBinding("search", "search", contract.FieldLocationQuery, true, contract.Schema{Type: contract.SchemaTypeString}, "form", true)
		search.AllowReserved = true
		operation.Inputs = []contract.FieldBinding{
			{
				FieldID: "token", WireName: "token", Location: contract.FieldLocationQuery,
				JSONPointer: "/query/token", Required: true, Nullable: false,
				Style: "form", Explode: true, Sensitivity: contract.SensitivitySecret,
				Schema: contract.Schema{Type: contract.SchemaTypeString},
			},
			search,
		}
		request := testHTTPRequest(
			t,
			operation,
			"https://provider.test",
			time.Now().UTC(),
			contract.NewObjectValue(map[string]contract.PresentValue{
				"search": contract.NewStringValue("visible&token=attacker"),
			}),
		)
		request.Operation.Execution.HTTP.Authorization = &contract.OperationAuthorization{
			Required: true, Audience: "https://provider.test", Placement: contract.FieldLocationQuery,
			WireName: "token", FieldID: "token",
		}
		credentials := &staticCredentialResolver{secret: []byte("credential-canary"), call: request.Call}
		admitter := &testAdmitter{}
		dialer := &mappedDialer{address: "127.0.0.1:1"}
		dns := &countingResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}}
		_, err := ExecuteHTTP(context.Background(), HTTPDependencies{
			Admitter: admitter, Credentials: credentials, Resolver: dns, Dialer: dialer,
		}, request)
		if !errors.Is(err, ErrInvalidHTTPInput) {
			t.Fatalf("error = %v", err)
		}
		if credentials.calls.Load() != 0 || credentials.last != nil ||
			admitter.calls.Load() != 0 || dns.calls != 0 || dialer.calls.Load() != 0 {
			t.Fatalf(
				"credential calls=%d material=%#v admits=%d DNS=%d dials=%d",
				credentials.calls.Load(), credentials.last,
				admitter.calls.Load(), dns.calls, dialer.calls.Load(),
			)
		}
	})

	t.Run("optional source authorization remains absent when the resolver reports absent", func(t *testing.T) {
		var requests atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			requests.Add(1)
			if value := request.Header.Get("Authorization"); value != "" {
				t.Errorf("Authorization = %q", value)
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`"ok"`))
		}))
		defer server.Close()
		operation := testOperation("optional-auth", http.MethodGet, "/items", contract.Schema{Type: contract.SchemaTypeString})
		binding := testSecretHeaderBinding("authorization", "Authorization")
		binding.Required = false
		operation.Inputs = []contract.FieldBinding{binding}
		origin := serverOrigin(t, server.URL)
		request := testHTTPRequest(t, operation, origin, time.Now().UTC(), contract.NewObjectValue(nil))
		request.Operation.Execution.HTTP.Authorization = &contract.OperationAuthorization{
			Required: false, Audience: origin, Placement: contract.FieldLocationHeader,
			WireName: "Authorization", Prefix: "Bearer ", FieldID: "authorization",
		}
		dialer := &mappedDialer{address: server.Listener.Addr().String()}
		credentials := &absentCredentialResolver{}
		execution, err := ExecuteHTTP(context.Background(), HTTPDependencies{
			Admitter:    &testAdmitter{},
			Credentials: credentials,
			Resolver:    fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
			Dialer:      dialer,
		}, request)
		if err != nil || execution.Result == nil || requests.Load() != 1 ||
			credentials.calls.Load() != 1 || dialer.calls.Load() != 1 {
			t.Fatalf(
				"execution=%#v requests=%d credential calls=%d dials=%d error=%v",
				execution, requests.Load(), credentials.calls.Load(), dialer.calls.Load(), err,
			)
		}
	})

	t.Run("contradictory authorization audience fails before credentials admission or DNS", func(t *testing.T) {
		operation := testOperation("audience", http.MethodGet, "/items", contract.Schema{Type: contract.SchemaTypeString})
		operation.Inputs = []contract.FieldBinding{testSecretHeaderBinding("authorization", "Authorization")}
		now := time.Now().UTC()
		request := testHTTPRequest(t, operation, "https://provider.test", now, contract.NewObjectValue(nil))
		request.Egress.RequireTLS = true
		request.Operation.Execution.HTTP.Authorization = &contract.OperationAuthorization{
			Required: true, Audience: "https://other.test", Placement: contract.FieldLocationHeader,
			WireName: "Authorization", Prefix: "Bearer ", FieldID: "authorization",
		}
		admitter := &testAdmitter{}
		dialer := &mappedDialer{address: "127.0.0.1:1"}
		credentials := &staticCredentialResolver{secret: []byte("must-not-be-read"), call: request.Call}
		dns := &countingResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}}
		_, err := ExecuteHTTP(context.Background(), HTTPDependencies{
			Admitter: admitter, Credentials: credentials, Resolver: dns, Dialer: dialer,
		}, request)
		if !errors.Is(err, ErrUnsupportedHTTPTransport) {
			t.Fatalf("error = %v", err)
		}
		if credentials.calls.Load() != 0 || dns.calls != 0 || admitter.calls.Load() != 0 || dialer.calls.Load() != 0 {
			t.Fatalf("credentials=%d DNS=%d admits=%d dials=%d", credentials.calls.Load(), dns.calls, admitter.calls.Load(), dialer.calls.Load())
		}
	})

	t.Run("server selector cannot widen source origin authority", func(t *testing.T) {
		operation := testOperation("source-origin-mismatch", http.MethodGet, "/items", contract.Schema{Type: contract.SchemaTypeString})
		operation.Inputs = []contract.FieldBinding{testSecretHeaderBinding("authorization", "Authorization")}
		request := testHTTPRequest(t, operation, "https://source.test/api/v3", time.Now().UTC(), contract.NewObjectValue(nil))
		request.Operation.Execution.HTTP.Authorization = &contract.OperationAuthorization{
			Required: true, Audience: "https://source.test", Placement: contract.FieldLocationHeader,
			WireName: "Authorization", Prefix: "Bearer ", FieldID: "authorization",
		}
		request.Egress.SelectedOrigin = "https://source.test"
		credentials := &staticCredentialResolver{secret: []byte("must-not-be-read"), call: request.Call}
		dns := &countingResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}}
		admitter := &testAdmitter{}
		dialer := &mappedDialer{address: "127.0.0.1:1"}
		_, err := ExecuteHTTP(context.Background(), HTTPDependencies{
			Admitter: admitter, Credentials: credentials, Resolver: dns, Dialer: dialer,
		}, request)
		if !errors.Is(err, ErrDestinationDenied) {
			t.Fatalf("error = %v", err)
		}
		if credentials.calls.Load() != 0 || dns.calls != 0 || admitter.calls.Load() != 0 || dialer.calls.Load() != 0 {
			t.Fatalf("credentials=%d DNS=%d admits=%d dials=%d", credentials.calls.Load(), dns.calls, admitter.calls.Load(), dialer.calls.Load())
		}
	})

	t.Run("missing bundle HTTP authority fails before credentials DNS admission or send", func(t *testing.T) {
		operation := testOperation("missing-http-authority", http.MethodGet, "/items", contract.Schema{Type: contract.SchemaTypeString})
		operation.Inputs = []contract.FieldBinding{testSecretHeaderBinding("authorization", "Authorization")}
		request := testHTTPRequest(t, operation, "https://provider.test", time.Now().UTC(), contract.NewObjectValue(nil))
		request.Operation.Execution.HTTP = nil
		credentials := &staticCredentialResolver{secret: []byte("must-not-be-read"), call: request.Call}
		dns := &countingResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}}
		admitter := &testAdmitter{}
		dialer := &mappedDialer{address: "127.0.0.1:1"}
		_, err := ExecuteHTTP(context.Background(), HTTPDependencies{
			Admitter: admitter, Credentials: credentials, Resolver: dns, Dialer: dialer,
		}, request)
		if !errors.Is(err, ErrUnsupportedHTTPTransport) {
			t.Fatalf("error = %v", err)
		}
		if credentials.calls.Load() != 0 || dns.calls != 0 || admitter.calls.Load() != 0 || dialer.calls.Load() != 0 {
			t.Fatalf("credentials=%d DNS=%d admits=%d dials=%d", credentials.calls.Load(), dns.calls, admitter.calls.Load(), dialer.calls.Load())
		}
	})

	t.Run("private database allowance is separate from HTTP", func(t *testing.T) {
		private := netip.MustParseAddr("10.23.4.5")
		allowance := PrivateDatabaseAllowance{Prefixes: []netip.Prefix{netip.MustParsePrefix("10.23.0.0/16")}}
		if err := ValidatePrivateDatabaseDestination(private, allowance); err != nil {
			t.Fatalf("database allowance: %v", err)
		}
		if err := validatePublicHTTPAddress(private); !errors.Is(err, ErrDestinationDenied) {
			t.Fatalf("HTTP private address error = %v", err)
		}
		outside := netip.MustParseAddr("10.24.4.5")
		if err := ValidatePrivateDatabaseDestination(outside, allowance); !errors.Is(err, ErrDestinationDenied) {
			t.Fatalf("outside database error = %v", err)
		}
	})
}

func testSecretHeaderBinding(fieldID, wireName string) contract.FieldBinding {
	return contract.FieldBinding{
		FieldID: fieldID, WireName: wireName, Location: contract.FieldLocationHeader,
		JSONPointer: "/header/" + wireName, Required: true, Nullable: false,
		Style: "simple", Explode: false, AllowReserved: false,
		Sensitivity: contract.SensitivitySecret, Schema: contract.Schema{Type: contract.SchemaTypeString},
	}
}

type absentCredentialResolver struct {
	calls atomic.Int64
}

func (r *absentCredentialResolver) ResolveCredential(context.Context, PhysicalCall) (credentials.CredentialMaterial, error) {
	r.calls.Add(1)
	return nil, nil
}

type staticCredentialResolver struct {
	secret []byte
	call   PhysicalCall
	calls  atomic.Int64
	last   *testCredentialMaterial
}

func (r *staticCredentialResolver) ResolveCredential(context.Context, PhysicalCall) (credentials.CredentialMaterial, error) {
	r.calls.Add(1)
	r.last = &testCredentialMaterial{
		secret: append([]byte(nil), r.secret...),
		binding: contract.CredentialRef{
			CredentialRef: "credential-fixture", WorkspaceID: r.call.Scope.WorkspaceID,
			ConnectionID: r.call.ConnectionID, ProviderAccountWitness: r.call.AccountWitness,
			CiphertextVersion: 1, KeyRef: "key-fixture", Scopes: []string{}, AuthEpoch: 1,
		},
	}
	return r.last, nil
}

type testCredentialMaterial struct {
	secret    []byte
	binding   contract.CredentialRef
	destroyed bool
}

func (m *testCredentialMaterial) CredentialBinding() contract.CredentialRef { return m.binding }

func (m *testCredentialMaterial) Use(consume func([]byte) error) error {
	if m.destroyed || len(m.secret) == 0 {
		return errors.New("credential unavailable")
	}
	secret := append([]byte(nil), m.secret...)
	for index := range m.secret {
		m.secret[index] = 0
	}
	m.secret = nil
	m.destroyed = true
	defer func() {
		for index := range secret {
			secret[index] = 0
		}
	}()
	return consume(secret)
}

func (m *testCredentialMaterial) Destroy() {
	for index := range m.secret {
		m.secret[index] = 0
	}
	m.secret = nil
	m.destroyed = true
}

type captureDialer struct {
	addresses []string
	err       error
}

func (d *captureDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	d.addresses = append(d.addresses, address)
	return nil, d.err
}

type countingResolver struct {
	addresses []netip.Addr
	calls     int
}

func (r *countingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	r.calls++
	return append([]netip.Addr(nil), r.addresses...), nil
}
