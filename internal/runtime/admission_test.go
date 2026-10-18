package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
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
			_, err := ExecuteHTTP(context.Background(), HTTPDependencies{ClientIdentity: testClientIdentity,
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
			_, err := ExecuteHTTP(context.Background(), HTTPDependencies{ClientIdentity: testClientIdentity,
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
			execution, err := ExecuteHTTP(context.Background(), HTTPDependencies{ClientIdentity: testClientIdentity,
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
			_, err := ExecuteHTTP(ctx, HTTPDependencies{ClientIdentity: testClientIdentity,
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
			dependencies := HTTPDependencies{ClientIdentity: testClientIdentity,
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

	t.Run("sql", func(t *testing.T) {
		fixture := openSQLFixture(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		now := time.Now().UTC()
		connection := fixture.connection()
		read := sqlTestOperation(t, contract.PostgreSQLStatementMetadata, fixture.relation.Binding)
		write := sqlTestOperation(t, contract.PostgreSQLStatementMappedUpsert, fixture.relation.Binding)
		admitted := sqlTestCall(t, read, connection, fixture.relation, nil, sqlTestLimits(), now).Call.Scope

		type sqlAttempt struct {
			observation SQLObservation
			err         error
			admitter    *stagedAdmitter
			probe       *sqlProtocolProbe
			resolver    *staticCredentialResolver
		}
		execute := func(t *testing.T, admitter PhysicalCallAdmitter, operation contract.Operation, input []SQLColumnValue) (SQLObservation, error, *staticCredentialResolver, *sqlProtocolProbe) {
			t.Helper()
			call := sqlTestCall(t, operation, connection, fixture.relation, input, sqlTestLimits(), now)
			resolver := &staticCredentialResolver{secret: []byte(fixture.rolePassword), call: call.Call}
			probe := &sqlProtocolProbe{}
			dependencies := SQLDependencies{
				Admitter:    admitter,
				Credentials: resolver,
				Now:         func() time.Time { return time.Now().UTC() },
			}
			dependencies.frontend = probe.frontend
			observation, err := SQLTransport{Dependencies: dependencies}.Execute(ctx, call)
			return observation, err, resolver, probe
		}
		attempt := func(t *testing.T, failAt int64, failure error, operation contract.Operation, input []SQLColumnValue) sqlAttempt {
			t.Helper()
			admitter := &stagedAdmitter{failAt: failAt, err: failure}
			observation, err, resolver, probe := execute(t, admitter, operation, input)
			return sqlAttempt{observation: observation, err: err, admitter: admitter, probe: probe, resolver: resolver}
		}
		writeInput := func(key string) []SQLColumnValue {
			return []SQLColumnValue{
				{Column: "record_id", Value: mustSQLInt8(t, key)},
				{Column: "amount", Value: mustSQLNumeric(t, "4.0000", 9, 4)},
				{Column: "flagged", Value: SQLBooleanValue(true)},
				{Column: "note", Value: SQLTextValue("admission boundary probe")},
				{Column: "observed_at", Value: mustSQLInstant(t, "2026-09-19T07:00:00Z")},
				{Column: "local_wall", Value: mustSQLLocalTimestamp(t, "2026-09-19 07:00:00")},
			}
		}
		committedRows := func(t *testing.T, key string) string {
			t.Helper()
			row := fixture.adminRow(t, ctx, `SELECT count(*)::text AS rows FROM "`+sqlFixtureSchema+`"."`+sqlFixtureRelation+`" WHERE "record_id" = `+key)
			return *row["rows"]
		}

		for index, refusal := range []struct {
			name string
			err  error
		}{
			{name: "expired grant", err: errors.New("durable grant expired")},
			{name: "revoked grant", err: errors.New("durable grant revoked")},
			{name: "fenced permit", err: errors.New("account permit fenced")},
			{name: "exhausted budget", err: errors.New("run budget exhausted")},
			{name: "cancelled run", err: context.Canceled},
		} {
			t.Run("connection boundary/"+refusal.name, func(t *testing.T) {
				result := attempt(t, 1, refusal.err, read, nil)
				if !errors.Is(result.err, ErrAdmissionRequired) {
					t.Fatalf("error = %v", result.err)
				}
				if result.admitter.calls.Load() != 1 {
					t.Fatalf("admits=%d", result.admitter.calls.Load())
				}
				if result.resolver.calls.Load() != 0 {
					t.Fatalf("credential material was resolved for a refused call: %d", result.resolver.calls.Load())
				}
				begins, statements, commits := result.probe.counters()
				if result.observation.PhysicalSend || begins != 0 || statements != 0 || commits != 0 {
					t.Fatalf("refused connection boundary reached the target: %#v sends=%d/%d/%d", result.observation, begins, statements, commits)
				}
				if result.observation.Outcome != SQLOutcomeKnownRollback {
					t.Fatalf("refused connection boundary outcome = %q", result.observation.Outcome)
				}
			})

			key := strconv.Itoa(100 + index)
			t.Run("statement boundary/"+refusal.name, func(t *testing.T) {
				result := attempt(t, 2, refusal.err, write, writeInput(key))
				if !errors.Is(result.err, ErrAdmissionRequired) {
					t.Fatalf("error = %v", result.err)
				}
				begins, statements, commits := result.probe.counters()
				if begins != 1 || statements != 0 || commits != 0 {
					t.Fatalf("refused statement boundary sent %d/%d/%d", begins, statements, commits)
				}
				if result.observation.Outcome != SQLOutcomeKnownRollback || result.observation.Retry.Eligible {
					t.Fatalf("refused statement boundary observation=%#v", result.observation)
				}
				if rows := committedRows(t, key); rows != "0" {
					t.Fatalf("refused statement boundary committed %s rows", rows)
				}
			})

			t.Run("commit boundary/"+refusal.name, func(t *testing.T) {
				result := attempt(t, 3, refusal.err, write, writeInput(key))
				if !errors.Is(result.err, ErrAdmissionRequired) {
					t.Fatalf("error = %v", result.err)
				}
				begins, statements, commits := result.probe.counters()
				if begins != 1 || statements != 1 || commits != 0 {
					t.Fatalf("refused commit boundary sent %d/%d/%d", begins, statements, commits)
				}
				if result.observation.Outcome != SQLOutcomeKnownRollback {
					t.Fatalf("refused commit boundary outcome = %q", result.observation.Outcome)
				}
				if rows := committedRows(t, key); rows != "0" {
					t.Fatalf("refused commit boundary committed %s rows", rows)
				}
			})
		}

		t.Run("healthy current grant admits each send exactly once", func(t *testing.T) {
			admitter := &stagedAdmitter{}
			observation, err, resolver, probe := execute(t, admitter, write, writeInput("200"))
			if err != nil || observation.Outcome != SQLOutcomeKnownCommit {
				t.Fatalf("healthy observation=%#v error=%v", observation, err)
			}
			begins, statements, commits := probe.counters()
			if begins != 1 || statements != 1 || commits != 1 {
				t.Fatalf("healthy send counts = %d/%d/%d", begins, statements, commits)
			}
			if admitter.calls.Load() != 3 {
				t.Fatalf("healthy admits=%d; want one admitted boundary per allowed send", admitter.calls.Load())
			}
			if resolver.calls.Load() != 1 {
				t.Fatalf("credential material resolves=%d; want one use per attempt", resolver.calls.Load())
			}
			if rows := committedRows(t, "200"); rows != "1" {
				t.Fatalf("healthy current grant committed %s rows", rows)
			}
		})

		t.Run("foreign workspace scope is refused without a send", func(t *testing.T) {
			scope := &workspaceScopeAdmitter{admitted: admitted}
			probe := &sqlProtocolProbe{}
			transport := SQLTransport{Dependencies: SQLDependencies{
				Admitter: scope,
				Now:      func() time.Time { return time.Now().UTC() },
				frontend: probe.frontend,
			}}
			foreign := sqlTestCall(t, write, connection, fixture.relation, writeInput("300"), sqlTestLimits(), now)
			foreign.Call.Scope.WorkspaceID = "workspace-foreign-sql-fixture"
			resolver := &staticCredentialResolver{secret: []byte(fixture.rolePassword), call: foreign.Call}
			transport.Dependencies.Credentials = resolver
			observation, err := transport.Execute(ctx, foreign)
			if !errors.Is(err, ErrAdmissionRequired) {
				t.Fatalf("foreign workspace error = %v", err)
			}
			scopes := scope.observedScopes()
			if len(scopes) != 1 || scopes[0].WorkspaceID != foreign.Call.Scope.WorkspaceID {
				t.Fatalf("foreign workspace scope was not re-authorized as declared: %#v", scopes)
			}
			if resolver.calls.Load() != 0 {
				t.Fatalf("credential material was resolved for a foreign workspace: %d", resolver.calls.Load())
			}
			begins, statements, commits := probe.counters()
			if observation.PhysicalSend || begins != 0 || statements != 0 || commits != 0 {
				t.Fatalf("foreign workspace reached the target: %#v sends=%d/%d/%d", observation, begins, statements, commits)
			}
			if observation.Outcome != SQLOutcomeKnownRollback || observation.Retry.Eligible {
				t.Fatalf("foreign workspace outcome=%#v", observation)
			}
			if rows := committedRows(t, "300"); rows != "0" {
				t.Fatalf("foreign workspace committed %s rows", rows)
			}
			// The refused scope must not disable the registered handler: the
			// same transport still executes the admitted workspace scope.
			admittedCall := sqlTestCall(t, read, connection, fixture.relation, nil, sqlTestLimits(), now)
			admittedResolver := &staticCredentialResolver{secret: []byte(fixture.rolePassword), call: admittedCall.Call}
			transport.Dependencies.Credentials = admittedResolver
			observation, err = transport.Execute(ctx, admittedCall)
			if err != nil || observation.Outcome != SQLOutcomeKnownCommit {
				t.Fatalf("admitted scope after a foreign refusal observation=%#v error=%v", observation, err)
			}
			if admittedResolver.calls.Load() != 1 {
				t.Fatalf("admitted scope resolved credentials %d times", admittedResolver.calls.Load())
			}
			begins, statements, commits = probe.counters()
			if begins != 1 || statements != 1 || commits != 1 {
				t.Fatalf("admitted scope after a foreign refusal sent %d/%d/%d", begins, statements, commits)
			}
			if scope.calls.Load() != 4 {
				t.Fatalf("admitted scope admits=%d; want the refused boundary plus one per allowed send", scope.calls.Load())
			}
		})
	})
}
