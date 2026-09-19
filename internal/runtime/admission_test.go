package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestAllPhysicalCallsAdmitted(t *testing.T) {
	t.Run("http", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `[]`)
		}))
		defer server.Close()
		operation := testListOperation("admission-http", http.MethodGet, "/items")
		now := time.Now().UTC()
		origin := serverOrigin(t, server.URL)
		base := testHTTPRequest(t, operation, origin, now, contract.NewObjectValue(nil))

		for _, testCase := range []struct {
			name string
			err  error
		}{
			{name: "expired grant", err: errors.New("durable grant expired")},
			{name: "revoked grant", err: errors.New("durable grant revoked")},
			{name: "fenced permit", err: errors.New("account permit fenced")},
			{name: "exhausted budget", err: errors.New("run budget exhausted")},
			{name: "cancelled run", err: context.Canceled},
		} {
			admitter := &testAdmitter{err: testCase.err}
			dialer := &mappedDialer{address: server.Listener.Addr().String()}
			_, err := ExecuteHTTP(context.Background(), HTTPDependencies{
				Admitter: admitter,
				Resolver: fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
				Dialer:   dialer,
			}, base)
			if !errors.Is(err, ErrAdmissionRequired) {
				t.Fatalf("%s error = %v", testCase.name, err)
			}
			if admitter.calls.Load() != 1 || dialer.calls.Load() != 0 {
				t.Fatalf("%s admits=%d dials=%d", testCase.name, admitter.calls.Load(), dialer.calls.Load())
			}
		}

		t.Run("absent handler fails before IO", func(t *testing.T) {
			dialer := &mappedDialer{address: server.Listener.Addr().String()}
			_, err := ExecuteHTTP(context.Background(), HTTPDependencies{
				Resolver: fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
				Dialer:   dialer,
			}, base)
			if !errors.Is(err, ErrAdmissionRequired) || dialer.calls.Load() != 0 {
				t.Fatalf("error=%v dials=%d", err, dialer.calls.Load())
			}
		})

		// The runtime admission seam receives only the durable physical call and
		// its bounded context, so no login session reaches this path: one cannot
		// be expired here, and an expired session cannot revoke the grant here
		// either. Session independence is observed at the application boundary by
		// TestPhysicalCallBoundaryCurrentSingleUseAndSessionIndependence
		// (internal/app/physical_boundary_test.go), which expires the real session
		// rows and still observes exactly one controlled physical call. What this
		// seam observes is the counter pair: a granted call is admitted once and
		// sent once.
		t.Run("a granted call is admitted once and sent once", func(t *testing.T) {
			admitter := &testAdmitter{}
			dialer := &mappedDialer{address: server.Listener.Addr().String()}
			execution, err := ExecuteHTTP(context.Background(), HTTPDependencies{
				Admitter: admitter,
				Resolver: fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
				Dialer:   dialer,
			}, base)
			if err != nil {
				t.Fatalf("ExecuteHTTP: %v", err)
			}
			if execution.Result == nil || admitter.calls.Load() != 1 || dialer.calls.Load() != 1 {
				t.Fatalf("execution=%#v admits=%d dials=%d", execution, admitter.calls.Load(), dialer.calls.Load())
			}
		})

		t.Run("cancelled bounded context fails before admission", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			admitter := &testAdmitter{}
			dialer := &mappedDialer{address: server.Listener.Addr().String()}
			_, err := ExecuteHTTP(ctx, HTTPDependencies{
				Admitter: admitter,
				Resolver: fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
				Dialer:   dialer,
			}, base)
			if !errors.Is(err, context.Canceled) || admitter.calls.Load() != 0 || dialer.calls.Load() != 0 {
				t.Fatalf("error=%v admits=%d dials=%d", err, admitter.calls.Load(), dialer.calls.Load())
			}
		})

		t.Run("each later physical attempt is freshly admitted", func(t *testing.T) {
			admitter := &testAdmitter{}
			dialer := &mappedDialer{err: errors.New("dial refused")}
			dependencies := HTTPDependencies{
				Admitter: admitter,
				Resolver: fixedResolver{addresses: []netip.Addr{netip.MustParseAddr("93.184.216.34")}},
				Dialer:   dialer,
			}
			first, err := ExecuteHTTP(context.Background(), dependencies, base)
			if err == nil || first.Disposition != DispositionNotSent || !first.Retry.Eligible {
				t.Fatalf("first execution=%#v error=%v", first, err)
			}
			secondRequest := base
			secondRequest.Limits.AttemptNumber = 2
			attemptID, idErr := contract.NewAttemptID("attempt-admission-http-2")
			if idErr != nil {
				t.Fatal(idErr)
			}
			secondRequest.Call.Attempt.AttemptID = attemptID
			secondRequest.Call.Attempt.RecordedAt = now.UTC()
			second, err := ExecuteHTTP(context.Background(), dependencies, secondRequest)
			if err == nil || second.Disposition != DispositionNotSent {
				t.Fatalf("second execution=%#v error=%v", second, err)
			}
			if admitter.calls.Load() != 2 || dialer.calls.Load() != 2 {
				t.Fatalf("admits=%d dials=%d", admitter.calls.Load(), dialer.calls.Load())
			}
		})
	})
}
