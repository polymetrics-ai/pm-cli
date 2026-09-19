package runtime

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestRetryRequiresKnownReplaySafety(t *testing.T) {
	t.Run("accepted request then socket loss is unknown and never blindly replayed", func(t *testing.T) {
		var accepted atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("read request: %v", err)
			}
			if string(body) != `{"enabled":false}` {
				t.Errorf("body = %s", body)
			}
			accepted.Add(1)
			hijacker, ok := writer.(http.Hijacker)
			if !ok {
				t.Error("response writer cannot hijack")
				return
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = connection.Close()
		}))
		defer server.Close()

		operation := testBodyOperation("socket-loss")
		operation.Execution = &contract.OperationExecution{
			Retry: testRetryFact(contract.OperationRetryUnsafe, ""),
		}
		now := time.Now().UTC()
		request := testHTTPRequest(t, operation, serverOrigin(t, server.URL), now, contract.NewObjectValue(map[string]contract.PresentValue{
			"body": contract.NewObjectValue(map[string]contract.PresentValue{"enabled": contract.NewBooleanValue(false)}),
		}))
		dialer := &mappedDialer{address: server.Listener.Addr().String()}
		execution, err := ExecuteHTTP(context.Background(), HTTPDependencies{
			Admitter: &testAdmitter{},
			Resolver: fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
			Dialer:   dialer,
		}, request)
		var attemptErr *AttemptError
		if !errors.As(err, &attemptErr) || attemptErr.Disposition != DispositionUnknown {
			t.Fatalf("error = %T %v; execution = %#v", err, err, execution)
		}
		if execution.Retry.Eligible || execution.Retry.RequiresCondition {
			t.Fatalf("retry = %#v", execution.Retry)
		}
		if accepted.Load() != 1 || dialer.calls.Load() != 1 {
			t.Fatalf("accepted=%d dials=%d", accepted.Load(), dialer.calls.Load())
		}
		if execution.Wire.GetBodyPresent || execution.Wire.IdempotencyKeySent {
			t.Fatalf("wire replay state = %#v", execution.Wire)
		}
	})

	t.Run("connection loss before an HTTP send is definitive unsent", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		var accepted atomic.Int64
		done := make(chan struct{})
		go func() {
			defer close(done)
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			accepted.Add(1)
			_ = connection.SetDeadline(time.Now().Add(time.Second))
			buffer := make([]byte, 1)
			_, _ = connection.Read(buffer)
			_ = connection.Close()
		}()
		operation := testListOperation("tls-loss", http.MethodGet, "/items")
		operation.Execution = &contract.OperationExecution{
			Retry: testRetryFact(contract.OperationRetryUnsafe, ""),
		}
		now := time.Now().UTC()
		origin := "https://provider.test:" + strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
		request := testHTTPRequest(t, operation, origin, now, contract.NewObjectValue(nil))
		request.Egress.RequireTLS = true
		dialer := &mappedDialer{address: listener.Addr().String()}
		execution, err := ExecuteHTTP(context.Background(), HTTPDependencies{
			Admitter: &testAdmitter{},
			Resolver: fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
			Dialer:   dialer,
		}, request)
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("listener goroutine did not stop")
		}
		var attemptErr *AttemptError
		if !errors.As(err, &attemptErr) || attemptErr.Disposition != DispositionNotSent {
			t.Fatalf("error = %T %v", err, err)
		}
		if !execution.Retry.Eligible || execution.Retry.Reason != "definitive_unsent" {
			t.Fatalf("retry = %#v", execution.Retry)
		}
		if accepted.Load() != 1 || dialer.calls.Load() != 1 {
			t.Fatalf("accepted=%d dials=%d", accepted.Load(), dialer.calls.Load())
		}
	})

	t.Run("declared replay remains a bounded Temporal instruction", func(t *testing.T) {
		limits := CallLimits{
			MaxRequestBytes: 1, MaxResponseBytes: 1, MaxDNSAnswers: 1,
			AttemptNumber: 1, MaxAttempts: 3, MaxRetryDelay: 30 * time.Second,
		}
		unknown := retryForTransportFailure(nil, DispositionUnknown, limits)
		if unknown.Eligible || unknown.RequiresCondition {
			t.Fatalf("unknown retry = %#v", unknown)
		}
		unsafe := retryForTransportFailure(testRetryFact(contract.OperationRetryUnsafe, ""), DispositionUnknown, limits)
		if unsafe.Eligible || unsafe.RequiresCondition {
			t.Fatalf("unsafe retry = %#v", unsafe)
		}
		safe := retryForTransportFailure(testRetryFact(contract.OperationRetrySafe, ""), DispositionUnknown, limits)
		if !safe.Eligible || safe.RequiresCondition {
			t.Fatalf("safe retry = %#v", safe)
		}
		responded := retryForTransportFailure(testRetryFact(contract.OperationRetrySafe, ""), DispositionResponded, limits)
		if responded.Eligible || responded.RequiresCondition {
			t.Fatalf("responded retry without an admitted status fact = %#v", responded)
		}
		conditional := retryForTransportFailure(
			testRetryFact(contract.OperationRetryConditional, "provider-key-absent"),
			DispositionUnknown,
			limits,
		)
		if conditional.Eligible || !conditional.RequiresCondition {
			t.Fatalf("conditional retry = %#v", conditional)
		}
		limits.AttemptNumber = limits.MaxAttempts
		if exhausted := retryForTransportFailure(testRetryFact(contract.OperationRetrySafe, ""), DispositionUnknown, limits); exhausted.Eligible {
			t.Fatalf("exhausted retry = %#v", exhausted)
		}
	})

	t.Run("source status and Retry-After remain one-send instructions", func(t *testing.T) {
		var requests atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("Retry-After", "3")
			writer.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(writer, `[]`)
		}))
		defer server.Close()
		operation := testListOperation("status-retry", http.MethodGet, "/items")
		operation.Responses[0].Status = "429"
		operation.Execution = &contract.OperationExecution{
			Retry: testRetryFact(contract.OperationRetrySafe, "", http.StatusTooManyRequests),
		}
		now := time.Now().UTC()
		request := testHTTPRequest(t, operation, serverOrigin(t, server.URL), now, contract.NewObjectValue(nil))
		admitter := &testAdmitter{}
		dialer := &mappedDialer{address: server.Listener.Addr().String()}
		execution, err := ExecuteHTTP(context.Background(), HTTPDependencies{
			Admitter: admitter,
			Resolver: fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
			Dialer:   dialer,
			Now:      func() time.Time { return now },
		}, request)
		if err != nil {
			t.Fatalf("ExecuteHTTP: %v", err)
		}
		if !execution.Retry.Eligible || execution.Retry.RequiresCondition ||
			execution.Retry.Reason != "source_declared_http_status" {
			t.Fatalf("retry = %#v", execution.Retry)
		}
		if !execution.Cooldown.Present || !execution.Cooldown.Usable ||
			!execution.Cooldown.NotBefore.Equal(now.Add(3*time.Second)) {
			t.Fatalf("cooldown = %#v", execution.Cooldown)
		}
		if requests.Load() != 1 || admitter.calls.Load() != 1 || dialer.calls.Load() != 1 {
			t.Fatalf("requests=%d admits=%d dials=%d", requests.Load(), admitter.calls.Load(), dialer.calls.Load())
		}
	})

	t.Run("cooldown is independent of replay eligibility and exactly bounded", func(t *testing.T) {
		now := time.Date(2026, time.September, 18, 12, 0, 0, 0, time.UTC)
		date := now.Add(10 * time.Second).Format(http.TimeFormat)
		for _, testCase := range []struct {
			name        string
			values      []string
			max         time.Duration
			want        time.Time
			wantPresent bool
			wantUsable  bool
		}{
			{name: "absent", max: 10 * time.Second},
			{name: "delay seconds", values: []string{"5"}, max: 10 * time.Second, want: now.Add(5 * time.Second), wantPresent: true, wantUsable: true},
			{name: "HTTP date", values: []string{date}, max: 10 * time.Second, want: now.Add(10 * time.Second), wantPresent: true, wantUsable: true},
			{name: "past HTTP date", values: []string{now.Add(-time.Second).Format(http.TimeFormat)}, max: 10 * time.Second, want: now.Add(-time.Second), wantPresent: true, wantUsable: true},
			{name: "malformed", values: []string{"1.5"}, max: 10 * time.Second, wantPresent: true},
			{name: "overflow", values: []string{"18446744073709551615"}, max: 10 * time.Second, wantPresent: true},
			{name: "over bound", values: []string{"11"}, max: 10 * time.Second, wantPresent: true},
			{name: "ambiguous", values: []string{"1", "2"}, max: 10 * time.Second, wantPresent: true},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				header := make(http.Header)
				for _, value := range testCase.values {
					header.Add("Retry-After", value)
				}
				cooldown := retryAfterCooldown(header, now, testCase.max)
				if cooldown.Present != testCase.wantPresent ||
					cooldown.Usable != testCase.wantUsable ||
					(testCase.wantUsable && !cooldown.NotBefore.Equal(testCase.want)) {
					t.Fatalf("cooldown = %#v", cooldown)
				}
			})
		}
		limits := CallLimits{AttemptNumber: 1, MaxAttempts: 3}
		unsafe := retryForHTTPStatus(
			testRetryFact(contract.OperationRetryUnsafe, "", http.StatusTooManyRequests),
			http.StatusTooManyRequests,
			limits,
			now,
			now.Add(time.Minute),
		)
		if unsafe.Eligible || unsafe.RequiresCondition {
			t.Fatalf("unsafe status retry = %#v", unsafe)
		}
		unlisted := retryForHTTPStatus(
			testRetryFact(contract.OperationRetrySafe, "", http.StatusServiceUnavailable),
			http.StatusTooManyRequests,
			limits,
			now,
			now.Add(time.Minute),
		)
		if unlisted.Eligible || unlisted.RequiresCondition {
			t.Fatalf("unlisted status retry = %#v", unlisted)
		}
		conditional := retryForHTTPStatus(
			testRetryFact(contract.OperationRetryConditional, "provider-key-absent", http.StatusTooManyRequests),
			http.StatusTooManyRequests,
			limits,
			now,
			now.Add(time.Minute),
		)
		if conditional.Eligible || !conditional.RequiresCondition {
			t.Fatalf("conditional status retry = %#v", conditional)
		}
		atDeadline := retryForHTTPStatus(
			testRetryFact(contract.OperationRetrySafe, "", http.StatusTooManyRequests),
			http.StatusTooManyRequests,
			limits,
			now,
			now,
		)
		if atDeadline.Eligible || atDeadline.RequiresCondition {
			t.Fatalf("deadline-exhausted retry = %#v", atDeadline)
		}
	})

	t.Run("HTTP2-only upstream is not silently negotiated", func(t *testing.T) {
		var handled atomic.Int64
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			handled.Add(1)
		}))
		server.EnableHTTP2 = true
		server.StartTLS()
		defer server.Close()
		if server.TLS == nil {
			t.Fatal("TLS fixture is absent")
		}
		operation := testListOperation("http2-only", http.MethodGet, "/items")
		now := time.Now().UTC()
		origin := serverOrigin(t, server.URL)
		request := testHTTPRequest(t, operation, origin, now, contract.NewObjectValue(nil))
		request.Egress.RequireTLS = true
		clientTransport, ok := server.Client().Transport.(*http.Transport)
		if !ok || clientTransport.TLSClientConfig == nil {
			t.Fatal("TLS client fixture is absent")
		}
		_, err := ExecuteHTTP(context.Background(), HTTPDependencies{
			Admitter:  &testAdmitter{},
			Resolver:  fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
			Dialer:    &mappedDialer{address: server.Listener.Addr().String()},
			TLSConfig: clientTransport.TLSClientConfig.Clone(),
		}, request)
		if err == nil || handled.Load() != 0 {
			t.Fatalf("HTTP2-only execution error=%v handled=%d", err, handled.Load())
		}
	})
}

func testRetryFact(
	classification contract.OperationRetryClassification,
	condition string,
	statuses ...uint16,
) *contract.OperationRetry {
	return &contract.OperationRetry{
		Classification: classification,
		Condition:      condition,
		HTTPStatuses:   append([]uint16(nil), statuses...),
		Witness: contract.ExecutionWitness{
			ProofID:    "fixture-retry-proof",
			SourceRefs: testSourceRefs("#/x-pm-retry"),
		},
	}
}

func testBodyOperation(id string) contract.Operation {
	bodySchema := contract.Schema{
		Type:       contract.SchemaTypeObject,
		Required:   []string{"enabled"},
		Properties: map[string]contract.Schema{"enabled": {Type: contract.SchemaTypeBool}},
	}
	operation := testOperation(id, http.MethodPost, "/effects", contract.Schema{Type: contract.SchemaTypeObject, Properties: map[string]contract.Schema{}})
	operation.RequestVariants = []contract.RequestVariant{{
		VariantID: "json-body", MediaType: "application/json", Required: true, Schema: bodySchema,
		Inputs: []contract.FieldBinding{{
			FieldID: "body", WireName: "", Location: contract.FieldLocationBody, JSONPointer: "", Required: true,
			Nullable: false, Style: "json", Explode: false, AllowReserved: false,
			Sensitivity: contract.SensitivityPublic, Schema: bodySchema,
		}},
		SourceRefs: testSourceRefs("#/requestBody"),
	}}
	return operation
}
