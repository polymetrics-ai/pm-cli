package app_test

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

const durableContinuationObjectBytes int64 = 4096

type durableContinuationAuthority struct {
	selection app.OperationSelection
	policy    contract.HTTPExecutionPolicy
}

func TestDurableHTTPContinuationAcrossWorkerRestart(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	producer := testfixture.OpenArtifacts(t, testfixture.PrimaryArtifactGateway)
	consumer := testfixture.OpenArtifacts(t, testfixture.SecondaryArtifactGateway)
	var providerCalls atomic.Int32
	origin, tlsConfig, dialer := newHTTPDirectReadTLSPeer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		providerCalls.Add(1)
		project := request.URL.Query().Get("project")
		page := request.URL.Query().Get("page")
		writer.Header().Set("Content-Type", "application/json")
		switch project + "/" + page {
		case "chain/1", "filtered/1", "reject/1":
			_, _ = io.WriteString(writer, `{"records":[{"id":1},{"id":2},{"id":3}],"last":false}`)
		case "chain/2", "filtered/2", "reject/2":
			_, _ = io.WriteString(writer, `{"records":[{"id":4}],"last":true}`)
		case "terminal/1":
			_, _ = io.WriteString(writer, `{"records":[{"id":10},{"id":11},{"id":12}],"last":true}`)
		default:
			t.Errorf("unexpected provider continuation request project=%q page=%q", project, page)
			http.Error(writer, "unexpected controlled request", http.StatusBadRequest)
		}
	}))
	authority := &durableContinuationAuthority{
		selection: durableHTTPContinuationSelection("fixture.http.durable-continuation", origin),
		policy:    httpDirectReadPolicy(origin),
	}
	dependencies := app.HTTPDirectReadDependencies{ClientIdentity: testClientIdentity,
		Resolver: httpDirectReadResolver{}, Dialer: dialer, TLSConfig: tlsConfig,
	}
	fixture, producerPublisher := newDurableContinuationFixture(
		t, authority, dependencies, producer, consumer, producer.Limits,
	)
	producerService := workerResultService(
		t, fixture, temporal, producerPublisher, durableContinuationObjectBytes,
	)

	cap := uint64(2)
	first := admitDurableContinuationRun(
		t,
		fixture,
		authority.selection,
		fixture.Client,
		fixture.Scope,
		fixture.Connection.ConnectionID,
		"durable-continuation-first",
		durableContinuationInput(t, "chain", 1),
		contract.ResultControls{TotalRecordCap: &cap},
	)
	firstRun := preparePermitWorkerRun(t, fixture, producerService, temporal, fixture.Scope, fixture.Actor, first)
	firstUnit, err := executePermitWorkerUnit(t, producerService, firstRun)
	if err != nil || firstUnit.State != contract.RunStateSucceeded || providerCalls.Load() != 1 {
		t.Fatalf("first continuation unit = %#v error=%v provider_calls=%d", firstUnit, err, providerCalls.Load())
	}
	firstResult := loadDurableContinuationResult(t, fixture, first.RunRef)
	assertDurableContinuationIDs(t, firstResult, 1, 2)
	if firstResult.Pagination == nil || firstResult.Pagination.TotalRecordCap == nil ||
		*firstResult.Pagination.TotalRecordCap != cap || len(firstResult.Pagination.UnreadTail) != 1 ||
		firstResult.Pagination.FetchedCount != 3 || firstResult.Pagination.EmittedCount != 2 {
		t.Fatalf("first pagination truth = %#v", firstResult.Pagination)
	}
	firstToken := requireDurableContinuationToken(t, firstResult)
	assertPrivateContinuationOrigin(t, fixture, firstToken, first.RunRef)

	if err := producer.Close(); err != nil {
		t.Fatal(err)
	}
	consumerRegistry, consumerPublisher := newDurableContinuationWorker(
		t, fixture, dependencies, consumer, producer.Limits,
	)
	fixture.WorkerRegistry = consumerRegistry
	consumerService := workerResultService(
		t, fixture, temporal, consumerPublisher, durableContinuationObjectBytes,
	)

	second := admitDurableContinuationRun(
		t,
		fixture,
		authority.selection,
		fixture.Client,
		fixture.Scope,
		fixture.Connection.ConnectionID,
		"durable-continuation-second",
		contract.NewObjectValue(map[string]contract.PresentValue{}),
		continuationControls(firstToken),
	)
	assertRestoredContinuationSnapshot(t, fixture, second.RunRef, "chain", 1, cap, firstToken, false)
	secondRun := preparePermitWorkerRun(t, fixture, consumerService, temporal, fixture.Scope, fixture.Actor, second)
	secondUnit, err := executePermitWorkerUnit(t, consumerService, secondRun)
	if err != nil || secondUnit.State != contract.RunStateSucceeded || secondUnit.UnknownEffectCount != 0 || providerCalls.Load() != 1 {
		t.Fatalf("local continuation unit = %#v error=%v provider_calls=%d", secondUnit, err, providerCalls.Load())
	}
	secondResult := loadDurableContinuationResult(t, fixture, second.RunRef)
	assertDurableContinuationIDs(t, secondResult, 3)
	if secondResult.Pagination == nil || secondResult.Pagination.FetchedCount != 1 ||
		secondResult.Pagination.ReturnedCount != 1 || secondResult.Pagination.EmittedCount != 1 {
		t.Fatalf("local-tail pagination truth = %#v", secondResult.Pagination)
	}
	secondToken := requireDurableContinuationToken(t, secondResult)
	if secondToken == firstToken {
		t.Fatal("continuation token did not rotate to the current producing run")
	}
	assertPrivateContinuationOrigin(t, fixture, secondToken, second.RunRef)
	assertLatestRunPermitState(t, fixture, second.RunRef, store.AccountPermitReleased)

	third := admitDurableContinuationRun(
		t,
		fixture,
		authority.selection,
		fixture.Client,
		fixture.Scope,
		fixture.Connection.ConnectionID,
		"durable-continuation-third",
		contract.NewObjectValue(map[string]contract.PresentValue{}),
		continuationControls(secondToken),
	)
	assertRestoredContinuationSnapshot(t, fixture, third.RunRef, "chain", 1, cap, secondToken, false)
	thirdRun := preparePermitWorkerRun(t, fixture, consumerService, temporal, fixture.Scope, fixture.Actor, third)
	thirdUnit, err := executePermitWorkerUnit(t, consumerService, thirdRun)
	if err != nil || thirdUnit.State != contract.RunStateSucceeded || providerCalls.Load() != 2 {
		t.Fatalf("next-page continuation unit = %#v error=%v provider_calls=%d", thirdUnit, err, providerCalls.Load())
	}
	thirdResult := loadDurableContinuationResult(t, fixture, third.RunRef)
	assertDurableContinuationIDs(t, thirdResult, 4)
	if thirdResult.Pagination == nil || thirdResult.Pagination.Continuation.Presence() != contract.PresenceAbsent ||
		!thirdResult.Pagination.SourceExhausted || thirdResult.Pagination.FetchedCount != 1 {
		t.Fatalf("next-page terminal pagination truth = %#v", thirdResult.Pagination)
	}

	t.Run("required navigation terminal unread tail remains local", func(t *testing.T) {
		before := providerCalls.Load()
		terminal := admitDurableContinuationRun(
			t, fixture, authority.selection, fixture.Client, fixture.Scope,
			fixture.Connection.ConnectionID, "durable-continuation-terminal-first",
			durableContinuationInput(t, "terminal", 1), contract.ResultControls{TotalRecordCap: &cap},
		)
		terminalRun := preparePermitWorkerRun(t, fixture, consumerService, temporal, fixture.Scope, fixture.Actor, terminal)
		if _, err := executePermitWorkerUnit(t, consumerService, terminalRun); err != nil {
			t.Fatal(err)
		}
		terminalResult := loadDurableContinuationResult(t, fixture, terminal.RunRef)
		terminalToken := requireDurableContinuationToken(t, terminalResult)
		if terminalResult.Pagination == nil || terminalResult.Pagination.SourceExhausted ||
			len(terminalResult.Pagination.UnreadTail) != 1 || providerCalls.Load() != before+1 {
			t.Fatalf("terminal producer pagination = %#v calls=%d", terminalResult.Pagination, providerCalls.Load())
		}
		resumed := admitDurableContinuationRun(
			t, fixture, authority.selection, fixture.Client, fixture.Scope,
			fixture.Connection.ConnectionID, "durable-continuation-terminal-tail",
			contract.NewObjectValue(map[string]contract.PresentValue{}), continuationControls(terminalToken),
		)
		assertRestoredContinuationSnapshot(t, fixture, resumed.RunRef, "terminal", 1, cap, terminalToken, false)
		resumedRun := preparePermitWorkerRun(t, fixture, consumerService, temporal, fixture.Scope, fixture.Actor, resumed)
		unit, err := executePermitWorkerUnit(t, consumerService, resumedRun)
		if err != nil || unit.State != contract.RunStateSucceeded || providerCalls.Load() != before+1 {
			t.Fatalf("terminal local tail = %#v error=%v calls=%d", unit, err, providerCalls.Load())
		}
		resumedResult := loadDurableContinuationResult(t, fixture, resumed.RunRef)
		assertDurableContinuationIDs(t, resumedResult, 12)
		if resumedResult.Pagination == nil || !resumedResult.Pagination.SourceExhausted ||
			resumedResult.Pagination.Continuation.Presence() != contract.PresenceAbsent {
			t.Fatalf("terminal local result pagination = %#v", resumedResult.Pagination)
		}
		assertLatestRunPermitState(t, fixture, resumed.RunRef, store.AccountPermitReleased)
	})

	t.Run("filter-empty page keeps usable continuation", func(t *testing.T) {
		before := providerCalls.Load()
		filterValue := durableContinuationInteger(t, 99)
		filtered := admitDurableContinuationRun(
			t, fixture, authority.selection, fixture.Client, fixture.Scope,
			fixture.Connection.ConnectionID, "durable-continuation-filtered-first",
			durableContinuationInput(t, "filtered", 1), contract.ResultControls{
				TotalRecordCap: &cap,
				Selection:      &contract.FieldSelection{Paths: []contract.FieldPath{{"id"}}},
				Filter: &contract.RecordFilter{
					Path: contract.FieldPath{"id"}, Operator: contract.FilterEqual, Value: &filterValue,
				},
			},
		)
		filteredRun := preparePermitWorkerRun(t, fixture, consumerService, temporal, fixture.Scope, fixture.Actor, filtered)
		if _, err := executePermitWorkerUnit(t, consumerService, filteredRun); err != nil {
			t.Fatal(err)
		}
		filteredResult := loadDurableContinuationResult(t, fixture, filtered.RunRef)
		assertDurableContinuationIDs(t, filteredResult)
		filteredToken := requireDurableContinuationToken(t, filteredResult)
		if filteredResult.Pagination == nil || filteredResult.Pagination.EmittedCount != 0 ||
			len(filteredResult.Pagination.UnreadTail) != 0 || providerCalls.Load() != before+1 {
			t.Fatalf("filtered first page = %#v calls=%d", filteredResult.Pagination, providerCalls.Load())
		}
		resumed := admitDurableContinuationRun(
			t, fixture, authority.selection, fixture.Client, fixture.Scope,
			fixture.Connection.ConnectionID, "durable-continuation-filtered-second",
			contract.NewObjectValue(map[string]contract.PresentValue{}), continuationControls(filteredToken),
		)
		assertRestoredContinuationSnapshot(t, fixture, resumed.RunRef, "filtered", 1, cap, filteredToken, true)
		resumedRun := preparePermitWorkerRun(t, fixture, consumerService, temporal, fixture.Scope, fixture.Actor, resumed)
		if _, err := executePermitWorkerUnit(t, consumerService, resumedRun); err != nil {
			t.Fatal(err)
		}
		resumedResult := loadDurableContinuationResult(t, fixture, resumed.RunRef)
		assertDurableContinuationIDs(t, resumedResult)
		if resumedResult.Pagination == nil || resumedResult.Pagination.Continuation.Presence() != contract.PresenceAbsent ||
			!resumedResult.Pagination.SourceExhausted || providerCalls.Load() != before+2 {
			t.Fatalf("filtered continuation page = %#v calls=%d", resumedResult.Pagination, providerCalls.Load())
		}
	})

	t.Run("continuation authority mismatches refuse before provider IO", func(t *testing.T) {
		producerAdmission := admitDurableContinuationRun(
			t, fixture, authority.selection, fixture.Client, fixture.Scope,
			fixture.Connection.ConnectionID, "durable-continuation-rejection-producer",
			durableContinuationInput(t, "reject", 1), contract.ResultControls{TotalRecordCap: &cap},
		)
		producerRun := preparePermitWorkerRun(t, fixture, consumerService, temporal, fixture.Scope, fixture.Actor, producerAdmission)
		if _, err := executePermitWorkerUnit(t, consumerService, producerRun); err != nil {
			t.Fatal(err)
		}
		token := requireDurableContinuationToken(t, loadDurableContinuationResult(t, fixture, producerAdmission.RunRef))
		before := providerCalls.Load()

		assertRejected := func(name string, client *http.Client, scope contract.Scope, connectionID string, selection app.OperationSelection, input contract.PresentValue, controls contract.ResultControls) {
			t.Helper()
			response := submitDurableContinuationRun(
				t, fixture, selection, client, scope, connectionID,
				"durable-continuation-reject-"+name, input, controls,
			)
			requireInitialProblem(t, response, http.StatusBadRequest, "invalid_request")
			if providerCalls.Load() != before {
				t.Fatalf("%s rejection reached provider: calls=%d want=%d", name, providerCalls.Load(), before)
			}
		}
		assertRejected(
			"forged", fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
			authority.selection, contract.NewObjectValue(map[string]contract.PresentValue{}),
			continuationControls("missing-continuation-receipt"),
		)
		assertRejected(
			"input", fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
			authority.selection,
			contract.NewObjectValue(map[string]contract.PresentValue{"project": contract.NewStringValue("different")}),
			continuationControls(token),
		)
		otherCap := uint64(3)
		conflictingControls := continuationControls(token)
		conflictingControls.TotalRecordCap = &otherCap
		assertRejected(
			"control", fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
			authority.selection, contract.NewObjectValue(map[string]contract.PresentValue{}), conflictingControls,
		)
		conflictingSelection := continuationControls(token)
		conflictingSelection.Selection = &contract.FieldSelection{Paths: []contract.FieldPath{{"id"}}}
		assertRejected(
			"selection", fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
			authority.selection, contract.NewObjectValue(map[string]contract.PresentValue{}), conflictingSelection,
		)
		assertRejected(
			"raw-navigation", fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
			authority.selection,
			contract.NewObjectValue(map[string]contract.PresentValue{"page": durableContinuationInteger(t, 2)}),
			continuationControls(token),
		)
		foreignClient := fixture.ClientFor(t, fixture.ForeignActor)
		assertRejected(
			"scope-connection", foreignClient, fixture.ForeignScope, fixture.ForeignConnection.ConnectionID,
			authority.selection, contract.NewObjectValue(map[string]contract.PresentValue{}), continuationControls(token),
		)

		originalPolicy := authority.policy
		authority.policy.SelectedOrigin = "https://alternate.provider.test"
		assertRejected(
			"origin", fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
			authority.selection, contract.NewObjectValue(map[string]contract.PresentValue{}), continuationControls(token),
		)
		authority.policy = originalPolicy

		originalSelection := authority.selection
		authority.selection.SchemaRefs = []string{"http-durable-continuation-schema-v2"}
		assertRejected(
			"source-revision", fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
			authority.selection, contract.NewObjectValue(map[string]contract.PresentValue{}), continuationControls(token),
		)
		authority.selection = originalSelection

		zero := uint64(0)
		invalidControls := continuationControls(token)
		invalidControls.TotalRecordCap = &zero
		assertRejected(
			"invalid-control", fixture.Client, fixture.Scope, fixture.Connection.ConnectionID,
			authority.selection, contract.NewObjectValue(map[string]contract.PresentValue{}), invalidControls,
		)

		accountResume := admitDurableContinuationRun(
			t, fixture, authority.selection, fixture.Client, fixture.Scope,
			fixture.Connection.ConnectionID, "durable-continuation-account-mismatch",
			contract.NewObjectValue(map[string]contract.PresentValue{}), continuationControls(token),
		)
		accountRun := preparePermitWorkerRun(t, fixture, consumerService, temporal, fixture.Scope, fixture.Actor, accountResume)
		fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `
				UPDATE pm.provider_accounts
				SET account_witness = 'changed-continuation-account', revision = revision + 1
				WHERE org_id = $1 AND provider_account_id = $2
			`, fixture.Scope.OrgID, fixture.Connection.ProviderAccountID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errors.New("controlled account identity update did not affect one row")
			}
			return nil
		})
		if _, err := executePermitWorkerUnit(t, consumerService, accountRun); err == nil {
			t.Fatal("changed provider-account identity was accepted")
		}
		_, attempts := permitTestDurableCounts(t, fixture, fixture.Scope, accountResume.RunRef)
		if attempts != 0 || providerCalls.Load() != before {
			t.Fatalf("account mismatch attempts/calls = %d/%d; want 0/%d", attempts, providerCalls.Load(), before)
		}

		revokePermitTestAuthority(t, fixture, producerAdmission.RunRef, "principal")
		revoked := submitDurableContinuationRun(
			t, fixture, authority.selection, fixture.Client, fixture.Scope,
			fixture.Connection.ConnectionID, "durable-continuation-revoked",
			contract.NewObjectValue(map[string]contract.PresentValue{}), continuationControls(token),
		)
		requireInitialProblem(t, revoked, http.StatusForbidden, "grant_unavailable")
		if providerCalls.Load() != before {
			t.Fatalf("revoked continuation reached provider: calls=%d want=%d", providerCalls.Load(), before)
		}
	})
}

// TestDurableLinkContinuationFollowsTheSealedProviderTarget proves the
// owner-approved link continuation policy through the durable envelope: the
// provider-issued canonical-route target and its cursor survive a worker
// restart inside the server-owned continuation, the next request is issued
// against the provider's path with the cursor preserved verbatim, and the
// records of every page must identify the same resource.
func TestDurableLinkContinuationFollowsTheSealedProviderTarget(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	producer := testfixture.OpenArtifacts(t, testfixture.PrimaryArtifactGateway)
	consumer := testfixture.OpenArtifacts(t, testfixture.SecondaryArtifactGateway)
	var (
		mu       sync.Mutex
		requests []string
	)
	origin, tlsConfig, dialer := newHTTPDirectReadTLSPeer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		requests = append(requests, request.URL.EscapedPath()+"?"+request.URL.RawQuery)
		mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Query().Get("page") == "1" {
			writer.Header().Set("Link",
				`</repositories/23096959/issues?project=chain&per_page=2&page=2&after=Y3Vyc29yOnYyOpLPAAABoLjpTwjPAAAAAUhclFY%3D>; rel="next"`)
			_, _ = io.WriteString(writer,
				`{"records":[{"id":1,"repo":"golang/go"},{"id":2,"repo":"golang/go"}]}`)
			return
		}
		_, _ = io.WriteString(writer, `{"records":[{"id":3,"repo":"golang/go"}]}`)
	}))
	authority := &durableContinuationAuthority{
		selection: durableLinkContinuationSelection("fixture.http.link-continuation", origin),
		policy:    httpDirectReadPolicy(origin),
	}
	dependencies := app.HTTPDirectReadDependencies{ClientIdentity: testClientIdentity,
		Resolver: httpDirectReadResolver{}, Dialer: dialer, TLSConfig: tlsConfig,
	}
	fixture, producerPublisher := newDurableContinuationFixture(
		t, authority, dependencies, producer, consumer, producer.Limits,
	)
	producerService := workerResultService(
		t, fixture, temporal, producerPublisher, durableContinuationObjectBytes,
	)

	cap := uint64(2)
	first := admitDurableContinuationRun(
		t, fixture, authority.selection, fixture.Client, fixture.Scope,
		fixture.Connection.ConnectionID, "durable-link-continuation-first",
		durableLinkContinuationInput(t, "chain", 1, 2), contract.ResultControls{TotalRecordCap: &cap},
	)
	firstRun := preparePermitWorkerRun(t, fixture, producerService, temporal, fixture.Scope, fixture.Actor, first)
	firstUnit, err := executePermitWorkerUnit(t, producerService, firstRun)
	if err != nil || firstUnit.State != contract.RunStateSucceeded {
		t.Fatalf("link continuation first unit = %#v error=%v", firstUnit, err)
	}
	firstResult := loadDurableContinuationResult(t, fixture, first.RunRef)
	assertDurableContinuationIDs(t, firstResult, 1, 2)
	firstToken := requireDurableContinuationToken(t, firstResult)

	consumerRegistry, consumerPublisher := newDurableContinuationWorker(
		t, fixture, dependencies, consumer, producer.Limits,
	)
	fixture.WorkerRegistry = consumerRegistry
	consumerService := workerResultService(
		t, fixture, temporal, consumerPublisher, durableContinuationObjectBytes,
	)
	second := admitDurableContinuationRun(
		t, fixture, authority.selection, fixture.Client, fixture.Scope,
		fixture.Connection.ConnectionID, "durable-link-continuation-second",
		contract.NewObjectValue(map[string]contract.PresentValue{}), continuationControls(firstToken),
	)
	secondRun := preparePermitWorkerRun(t, fixture, consumerService, temporal, fixture.Scope, fixture.Actor, second)
	secondUnit, err := executePermitWorkerUnit(t, consumerService, secondRun)
	if err != nil || secondUnit.State != contract.RunStateSucceeded {
		t.Fatalf("link continuation resumed unit = %#v error=%v", secondUnit, err)
	}
	secondResult := loadDurableContinuationResult(t, fixture, second.RunRef)
	assertDurableContinuationIDs(t, secondResult, 3)
	if secondResult.Pagination == nil || !secondResult.Pagination.SourceExhausted ||
		secondResult.Pagination.Continuation.Presence() != contract.PresenceAbsent {
		t.Fatalf("link continuation final pagination = %#v", secondResult.Pagination)
	}
	mu.Lock()
	got := strings.Join(requests, "|")
	mu.Unlock()
	want := "/fixture?project=chain&page=1&per_page=2|" +
		"/repositories/23096959/issues?project=chain&page=2&per_page=2&after=Y3Vyc29yOnYyOpLPAAABoLjpTwjPAAAAAUhclFY%3D"
	if got != want {
		t.Fatalf("link continuation requests = %q; want %q", got, want)
	}
}

// TestDurableLinkContinuationCannotResumeUnderAnotherSelectedOrigin proves the
// durable binding of a sealed provider target: the same continuation cannot be
// resumed once the durable authority selects another origin, so a sealed target
// is only ever re-authorized against the origin the producing run was admitted
// to. The runtime re-checks the sealed origin itself before every request.
func TestDurableLinkContinuationCannotResumeUnderAnotherSelectedOrigin(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	producer := testfixture.OpenArtifacts(t, testfixture.PrimaryArtifactGateway)
	consumer := testfixture.OpenArtifacts(t, testfixture.SecondaryArtifactGateway)
	var providerCalls atomic.Int32
	origin, tlsConfig, dialer := newHTTPDirectReadTLSPeer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		providerCalls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Query().Get("page") == "1" {
			writer.Header().Set("Link",
				`</repositories/23096959/issues?project=chain&per_page=2&page=2&after=Y3Vyc29yOnYyOpLPAAABoLjpTwjPAAAAAUhclFY%3D>; rel="next"`)
			_, _ = io.WriteString(writer,
				`{"records":[{"id":1,"repo":"golang/go"},{"id":2,"repo":"golang/go"}]}`)
			return
		}
		_, _ = io.WriteString(writer, `{"records":[{"id":3,"repo":"golang/go"}]}`)
	}))
	authority := &durableContinuationAuthority{
		selection: durableLinkContinuationSelection("fixture.http.link-continuation-origin", origin),
		policy:    httpDirectReadPolicy(origin),
	}
	dependencies := app.HTTPDirectReadDependencies{ClientIdentity: testClientIdentity,
		Resolver: httpDirectReadResolver{}, Dialer: dialer, TLSConfig: tlsConfig,
	}
	fixture, publisher := newDurableContinuationFixture(
		t, authority, dependencies, producer, consumer, producer.Limits,
	)
	service := workerResultService(t, fixture, temporal, publisher, durableContinuationObjectBytes)
	cap := uint64(2)
	first := admitDurableContinuationRun(
		t, fixture, authority.selection, fixture.Client, fixture.Scope,
		fixture.Connection.ConnectionID, "durable-link-origin-first",
		durableLinkContinuationInput(t, "chain", 1, 2), contract.ResultControls{TotalRecordCap: &cap},
	)
	firstRun := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, first)
	if _, err := executePermitWorkerUnit(t, service, firstRun); err != nil {
		t.Fatal(err)
	}
	token := requireDurableContinuationToken(t, loadDurableContinuationResult(t, fixture, first.RunRef))
	before := providerCalls.Load()
	originalPolicy := authority.policy
	authority.policy.SelectedOrigin = "https://alternate.provider.test"
	response := submitDurableContinuationRun(
		t, fixture, authority.selection, fixture.Client, fixture.Scope,
		fixture.Connection.ConnectionID, "durable-link-origin-second",
		contract.NewObjectValue(map[string]contract.PresentValue{}), continuationControls(token),
	)
	requireInitialProblem(t, response, http.StatusBadRequest, "invalid_request")
	if providerCalls.Load() != before {
		t.Fatalf("another selected origin reached provider: calls=%d want=%d", providerCalls.Load(), before)
	}
	authority.policy = originalPolicy
}

func TestDurableHTTPContinuationCapacityRefusalPrecedesProviderIO(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	producer := testfixture.OpenArtifacts(t, testfixture.PrimaryArtifactGateway)
	consumer := testfixture.OpenArtifacts(t, testfixture.SecondaryArtifactGateway)
	var providerCalls atomic.Int32
	origin, tlsConfig, dialer := newHTTPDirectReadTLSPeer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		providerCalls.Add(1)
	}))
	authority := &durableContinuationAuthority{
		selection: durableHTTPContinuationSelection("fixture.http.continuation-capacity", origin),
		policy:    httpDirectReadPolicy(origin),
	}
	limits := producer.Limits
	limits.OrganizationAttempts = 1
	limits.WorkspaceAttempts = 1
	fixture, publisher := newDurableContinuationFixture(t, authority, app.HTTPDirectReadDependencies{ClientIdentity: testClientIdentity,
		Resolver: httpDirectReadResolver{}, Dialer: dialer, TLSConfig: tlsConfig,
	}, producer, consumer, limits)
	service := workerResultService(t, fixture, temporal, publisher, durableContinuationObjectBytes)
	cap := uint64(2)
	admitted := admitDurableContinuationRun(
		t, fixture, authority.selection, fixture.Client, fixture.Scope,
		fixture.Connection.ConnectionID, "durable-continuation-capacity",
		durableContinuationInput(t, "chain", 1), contract.ResultControls{TotalRecordCap: &cap},
	)
	run := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
	if _, err := executePermitWorkerUnit(t, service, run); err == nil {
		t.Fatal("combined result and continuation capacity refusal was accepted")
	}
	_, attempts := permitTestDurableCounts(t, fixture, fixture.Scope, admitted.RunRef)
	if providerCalls.Load() != 0 || dialer.calls.Load() != 0 || attempts != 0 {
		t.Fatalf("capacity refusal provider/dial/attempt = %d/%d/%d; want 0/0/0", providerCalls.Load(), dialer.calls.Load(), attempts)
	}
}

func newDurableContinuationFixture(
	t *testing.T,
	authority *durableContinuationAuthority,
	dependencies app.HTTPDirectReadDependencies,
	producer *app.ArtifactResources,
	consumer *app.ArtifactResources,
	limits store.ObjectCapacityLimits,
) (*testfixture.Fixture, *objects.Publisher) {
	t.Helper()
	registry := &app.HandlerRegistry{}
	var resolver app.OperationResolver
	var publisher *objects.Publisher
	fixture := testfixture.NewOperation(t, authority.selection.Operation.OperationID, func(
		fixtureDependencies testfixture.ServiceDependencies,
		configuration *httpapi.ServiceConfig,
	) error {
		var err error
		publisher, err = objects.NewPublisher(fixtureDependencies.Pool, producer.Backend, limits)
		if err != nil {
			return err
		}
		continuations, err := app.NewContinuationService(
			fixtureDependencies.Pool, publisher, producer.Backend, durableContinuationObjectBytes,
		)
		if err != nil {
			return err
		}
		handlerDependencies := dependencies
		handlerDependencies.Continuations = continuations
		if err := registry.RegisterHTTP(app.NewHTTPDirectReadRegistration(handlerDependencies)); err != nil {
			return err
		}
		resolver = func(ctx context.Context, scope contract.Scope, connectionID, operationID string) (app.OperationSelection, error) {
			if ctx == nil || ctx.Err() != nil {
				return app.OperationSelection{}, context.Canceled
			}
			primary := scope.Equal(fixtureDependencies.Scope) && connectionID == fixtureDependencies.Connection.ConnectionID
			foreign := scope.Equal(fixtureDependencies.ForeignScope) && connectionID == fixtureDependencies.ForeignConnection.ConnectionID
			if operationID != authority.selection.Operation.OperationID || !primary && !foreign {
				return app.OperationSelection{}, errors.New("controlled durable continuation operation is unavailable")
			}
			return authority.selection, nil
		}
		resolvePolicy := func(scope contract.Scope, connectionID string) (contract.HTTPExecutionPolicy, bool) {
			primary := scope.Equal(fixtureDependencies.Scope) && connectionID == fixtureDependencies.Connection.ConnectionID
			foreign := scope.Equal(fixtureDependencies.ForeignScope) && connectionID == fixtureDependencies.ForeignConnection.ConnectionID
			return authority.policy, primary || foreign
		}
		admission, err := app.NewAdmissionService(app.AdmissionServiceConfig{
			Pool: fixtureDependencies.Pool, DeploymentID: fixtureDependencies.DeploymentID,
			Authorize: configuration.Auth.AuthorizeResource, Registry: registry,
			ResolveOperation: resolver, ResolveHTTPPolicy: resolvePolicy,
			Policy: httpDirectReadAdmissionPolicy(), Random: cryptorand.Reader,
		})
		if err != nil {
			return err
		}
		configuration.Admission = admission
		configuration.Artifacts = consumer.Backend
		return nil
	})
	fixture.WorkerRegistry = registry
	fixture.ResolveOperation = resolver
	if publisher == nil {
		t.Fatal("durable continuation fixture did not construct its actual publisher")
	}
	return fixture, publisher
}

func newDurableContinuationWorker(
	t *testing.T,
	fixture *testfixture.Fixture,
	dependencies app.HTTPDirectReadDependencies,
	artifacts *app.ArtifactResources,
	limits store.ObjectCapacityLimits,
) (*app.HandlerRegistry, *objects.Publisher) {
	t.Helper()
	publisher, err := objects.NewPublisher(fixture.Pool, artifacts.Backend, limits)
	if err != nil {
		t.Fatal(err)
	}
	continuations, err := app.NewContinuationService(
		fixture.Pool, publisher, artifacts.Backend, durableContinuationObjectBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	dependencies.Continuations = continuations
	registry := &app.HandlerRegistry{}
	if err := registry.RegisterHTTP(app.NewHTTPDirectReadRegistration(dependencies)); err != nil {
		t.Fatal(err)
	}
	return registry, publisher
}

func durableHTTPContinuationSelection(operationID, origin string) app.OperationSelection {
	operationSource := []contract.SourceRef{{SourceID: "http-direct-read-fixture", Pointer: "#/operations/durable-read"}}
	responseSource := []contract.SourceRef{{SourceID: "http-direct-read-fixture", Pointer: "#/operations/durable-read/responses"}}
	record := contract.Schema{
		Type: contract.SchemaTypeObject, Required: []string{"id"},
		Properties: map[string]contract.Schema{"id": {Type: contract.SchemaTypeInteger}},
	}
	responseSchema := contract.Schema{
		Type: contract.SchemaTypeObject, Required: []string{"records", "last"},
		Properties: map[string]contract.Schema{
			"records": {Type: contract.SchemaTypeArray, Items: &record},
			"last":    {Type: contract.SchemaTypeBool},
		},
	}
	recordPointer := "/records"
	capabilities := httpDirectReadBaseCapabilities()
	for _, capability := range contract.HTTPExecutionCapabilities() {
		if capability.Capability == "http.paginate" {
			capabilities = append(capabilities, capability)
		}
	}
	query := func(fieldID string, required bool, schema contract.Schema) contract.FieldBinding {
		return contract.FieldBinding{
			FieldID: fieldID, WireName: fieldID, Location: contract.FieldLocationQuery,
			JSONPointer: "/query/" + fieldID, Required: required, Nullable: false,
			Style: "form", Explode: true, Sensitivity: contract.SensitivityPublic, Schema: schema,
		}
	}
	return app.OperationSelection{
		Operation: contract.Operation{
			SchemaVersion: contract.OperationSchemaVersion, OperationID: operationID,
			SourceRefs: operationSource,
			Transport:  contract.Transport{Kind: contract.TransportKindHTTP, Method: http.MethodGet, Path: "/fixture"},
			Inputs: []contract.FieldBinding{
				query("project", true, contract.Schema{Type: contract.SchemaTypeString}),
				query("page", true, contract.Schema{Type: contract.SchemaTypeInteger}),
			},
			RequestVariants: []contract.RequestVariant{},
			Responses: []contract.Response{{
				Status: "200", MediaType: "application/json", Schema: &responseSchema,
				SourceRefs: responseSource, RecordPointer: &recordPointer,
			}},
			CapabilityRequirements: capabilities,
			LaneEligibility:        httpDirectReadLaneEligibility(),
			Execution: &contract.OperationExecution{
				HTTP: &contract.OperationHTTP{
					Origins: []string{origin, "https://alternate.provider.test"},
					Witness: contract.ExecutionWitness{ProofID: "durable-http", SourceRefs: operationSource},
				},
				Pagination: &contract.OperationPagination{
					Kind: contract.OperationPaginationPage, PageRequestField: "page",
					LastPageResponsePointer: "/last",
					Witness:                 contract.ExecutionWitness{ProofID: "durable-pagination", SourceRefs: responseSource},
				},
			},
		},
		SchemaRefs: []string{"http-durable-continuation-schema-v1"},
	}
}

// durableLinkContinuationSelection declares the owner-approved link
// continuation policy: the provider's canonical repository route is admitted
// for the same resource, a declared record field identifies that resource, and
// the provider cursor namespace stays undeclared so it can only be sealed from
// a provider response.
func durableLinkContinuationSelection(operationID, origin string) app.OperationSelection {
	operationSource := []contract.SourceRef{{SourceID: "http-direct-read-fixture", Pointer: "#/operations/link-read"}}
	responseSource := []contract.SourceRef{{SourceID: "http-direct-read-fixture", Pointer: "#/operations/link-read/responses"}}
	record := contract.Schema{
		Type: contract.SchemaTypeObject, Required: []string{"id", "repo"},
		Properties: map[string]contract.Schema{
			"id":   {Type: contract.SchemaTypeInteger},
			"repo": {Type: contract.SchemaTypeString},
		},
	}
	responseSchema := contract.Schema{
		Type: contract.SchemaTypeObject, Required: []string{"records"},
		Properties: map[string]contract.Schema{
			"records": {Type: contract.SchemaTypeArray, Items: &record},
		},
	}
	recordPointer := "/records"
	capabilities := httpDirectReadBaseCapabilities()
	for _, capability := range contract.HTTPExecutionCapabilities() {
		if capability.Capability == "http.paginate" {
			capabilities = append(capabilities, capability)
		}
	}
	query := func(fieldID, wireName string, required bool, schema contract.Schema) contract.FieldBinding {
		return contract.FieldBinding{
			FieldID: fieldID, WireName: wireName, Location: contract.FieldLocationQuery,
			JSONPointer: "/query/" + wireName, Required: required, Nullable: false,
			Style: "form", Explode: true, Sensitivity: contract.SensitivityPublic, Schema: schema,
		}
	}
	return app.OperationSelection{
		Operation: contract.Operation{
			SchemaVersion: contract.OperationSchemaVersion, OperationID: operationID,
			SourceRefs: operationSource,
			Transport:  contract.Transport{Kind: contract.TransportKindHTTP, Method: http.MethodGet, Path: "/fixture"},
			Inputs: []contract.FieldBinding{
				query("project", "project", true, contract.Schema{Type: contract.SchemaTypeString}),
				query("page", "page", false, contract.Schema{Type: contract.SchemaTypeInteger}),
				query("per-page", "per_page", false, contract.Schema{Type: contract.SchemaTypeInteger}),
			},
			RequestVariants: []contract.RequestVariant{},
			Responses: []contract.Response{{
				Status: "200", MediaType: "application/json", Schema: &responseSchema,
				SourceRefs: responseSource, RecordPointer: &recordPointer,
			}},
			CapabilityRequirements: capabilities,
			LaneEligibility:        httpDirectReadLaneEligibility(),
			Execution: &contract.OperationExecution{
				HTTP: &contract.OperationHTTP{
					Origins: []string{origin, "https://alternate.provider.test"},
					Witness: contract.ExecutionWitness{ProofID: "durable-link-http", SourceRefs: operationSource},
				},
				Pagination: &contract.OperationPagination{
					Kind:                      contract.OperationPaginationLink,
					LinkRequestField:          "page",
					LinkResponseHeader:        "Link",
					LimitRequestField:         "per-page",
					LinkRoutes:                []string{"/repositories/{repository_id}/issues"},
					LinkRecordIdentityPointer: "/repo",
					Witness:                   contract.ExecutionWitness{ProofID: "durable-link-pagination", SourceRefs: responseSource},
				},
			},
		},
		SchemaRefs: []string{"http-durable-continuation-schema-v1"},
	}
}

func durableLinkContinuationInput(t *testing.T, project string, page, perPage uint64) contract.PresentValue {
	t.Helper()
	return contract.NewObjectValue(map[string]contract.PresentValue{
		"project":  contract.NewStringValue(project),
		"page":     durableContinuationInteger(t, page),
		"per-page": durableContinuationInteger(t, perPage),
	})
}

func durableContinuationInput(t *testing.T, project string, page uint64) contract.PresentValue {
	t.Helper()
	return contract.NewObjectValue(map[string]contract.PresentValue{
		"project": contract.NewStringValue(project),
		"page":    durableContinuationInteger(t, page),
	})
}

func durableContinuationInteger(t *testing.T, value uint64) contract.PresentValue {
	t.Helper()
	result, err := contract.NewIntegerValue(fmt.Sprint(value))
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func continuationControls(token string) contract.ResultControls {
	value := contract.NewStringValue(token)
	return contract.ResultControls{Continuation: &value}
}

func admitDurableContinuationRun(
	t *testing.T,
	fixture *testfixture.Fixture,
	selection app.OperationSelection,
	client *http.Client,
	scope contract.Scope,
	connectionID string,
	requestKey string,
	input contract.PresentValue,
	controls contract.ResultControls,
) api.AdmissionResponse {
	t.Helper()
	return requireFreshInitialAdmission(t, submitDurableContinuationRun(
		t, fixture, selection, client, scope, connectionID, requestKey, input, controls,
	), requestKey)
}

func submitDurableContinuationRun(
	t *testing.T,
	fixture *testfixture.Fixture,
	selection app.OperationSelection,
	client *http.Client,
	scope contract.Scope,
	connectionID string,
	requestKey string,
	input contract.PresentValue,
	controls contract.ResultControls,
) initialRunResponse {
	t.Helper()
	revision, err := app.OperationSelectionRevision(selection)
	if err != nil {
		t.Fatal(err)
	}
	return submitInitialInvocation(t, fixture, client, scope.WorkspaceID, api.InvocationRequest{
		ConnectionRef: connectionID, OperationRef: selection.Operation.OperationID,
		Lane: api.InvocationRequestLane(contract.LaneDirectRead), Input: input, ResultControls: controls,
		Protocol: api.ProtocolMetadata{
			WireVersion: api.ProtocolMetadataWireVersion(api.WireVersion), RequiredFeatures: []string{},
			RequestKey:       requestKey,
			IntentDigest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			ExpectedRevision: revision,
		},
	})
}

func loadDurableContinuationResult(t *testing.T, fixture *testfixture.Fixture, runID string) contract.ActionResult {
	t.Helper()
	refs := workerResultReferences(t, fixture, runID)
	if len(refs) != 1 {
		t.Fatalf("run %q public result references = %v; want exactly one", runID, refs)
	}
	payload := workerResultGET(
		t, fixture, "/v1/workspaces/"+fixture.Scope.WorkspaceID+"/artifacts/"+refs[0]+"/content", http.StatusOK,
	)
	var result contract.ActionResult
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode durable continuation result: %v", err)
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("validate durable continuation result: %v", err)
	}
	return result
}

func requireDurableContinuationToken(t *testing.T, result contract.ActionResult) string {
	t.Helper()
	if result.Pagination == nil {
		t.Fatal("durable result has no pagination metadata")
	}
	token, ok := result.Pagination.Continuation.TextValue()
	if !ok || token == "" {
		t.Fatalf("durable result continuation = %#v", result.Pagination.Continuation)
	}
	return token
}

func assertDurableContinuationIDs(t *testing.T, result contract.ActionResult, expected ...uint64) {
	t.Helper()
	records, ok := result.Data.Elements()
	if !ok || len(records) != len(expected) {
		t.Fatalf("result records = %#v; want IDs %v", result.Data, expected)
	}
	for index, record := range records {
		fields, ok := record.Fields()
		id, okID := fields["id"].ExactNumber()
		if !ok || !okID || id.Text != fmt.Sprint(expected[index]) {
			t.Fatalf("result record %d = %#v; want id %d", index, record, expected[index])
		}
	}
}

func assertPrivateContinuationOrigin(t *testing.T, fixture *testfixture.Fixture, token, producerRunID string) {
	t.Helper()
	path := "/v1/workspaces/" + fixture.Scope.WorkspaceID + "/artifacts/" + token
	workerResultGET(t, fixture, path, http.StatusNotFound)
	workerResultGET(t, fixture, path+"/content", http.StatusNotFound)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		if _, err := store.LoadArtifact(ctx, tx, token); !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("private continuation loaded through public artifact API: %w", err)
		}
		private, err := store.LoadContinuationArtifact(ctx, tx, token)
		if err != nil {
			return err
		}
		origin, err := store.LoadWorkerContinuationOrigin(ctx, tx, token)
		if err != nil {
			return err
		}
		if private.ArtifactID != token || origin.Object != private || origin.RunID != producerRunID {
			return errors.New("private continuation origin differs from its same-receipt producer")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertRestoredContinuationSnapshot(
	t *testing.T,
	fixture *testfixture.Fixture,
	runID, project string,
	page, cap uint64,
	token string,
	wantFilter bool,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		snapshot, err := store.LoadRunInputSnapshot(ctx, tx, runID)
		if err != nil {
			return err
		}
		fields, ok := snapshot.Input.Fields()
		projectValue, projectOK := fields["project"].TextValue()
		pageValue, pageOK := fields["page"].ExactNumber()
		continuation, continuationOK := snapshot.ResultControls.ContinuationValue().TextValue()
		if !ok || !projectOK || projectValue != project || !pageOK || pageValue.Text != fmt.Sprint(page) ||
			snapshot.ResultControls.TotalRecordCap == nil || *snapshot.ResultControls.TotalRecordCap != cap ||
			!continuationOK || continuation != token ||
			(snapshot.ResultControls.Filter != nil) != wantFilter ||
			(snapshot.ResultControls.Selection != nil) != wantFilter {
			return errors.New("admitted continuation snapshot did not restore exact producer input and sticky controls")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func assertLatestRunPermitState(t *testing.T, fixture *testfixture.Fixture, runID string, expected store.AccountPermitState) {
	t.Helper()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		var state string
		if err := tx.QueryRow(ctx, `
			SELECT permit.state
			FROM pm.receipts AS receipt
			CROSS JOIN LATERAL jsonb_array_elements(receipt.payload->'references') AS reference(value)
			JOIN pm.account_permits AS permit
			  ON permit.org_id = receipt.org_id
			 AND permit.workspace_id = receipt.workspace_id
			 AND permit.permit_id = reference.value->>'ref'
			WHERE receipt.org_id = $1 AND receipt.workspace_id = $2 AND receipt.run_id = $3
			  AND receipt.kind = 'worker_attempt'
			  AND reference.value->>'name' = 'permit'
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID).Scan(&state); err != nil {
			return err
		}
		if state != string(expected) {
			return fmt.Errorf("run %q account permit state = %q; want %q", runID, state, expected)
		}
		return nil
	})
}

func TestDurableHTTPContinuationUsesReadableEnvelopeBound(t *testing.T) {
	temporal := newPermitTemporalHarness(t)
	producer := testfixture.OpenArtifacts(t, testfixture.PrimaryArtifactGateway)
	consumer := testfixture.OpenArtifacts(t, testfixture.SecondaryArtifactGateway)
	var providerCalls atomic.Int32
	origin, tlsConfig, dialer := newHTTPDirectReadTLSPeer(t, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"records":[{"id":1},{"id":2}],"last":true}`)
	}))
	authority := &durableContinuationAuthority{
		selection: durableHTTPContinuationSelection("fixture.http.continuation-envelope-bound", origin),
		policy:    httpDirectReadPolicy(origin),
	}
	dependencies := app.HTTPDirectReadDependencies{ClientIdentity: testClientIdentity,
		Resolver: httpDirectReadResolver{}, Dialer: dialer, TLSConfig: tlsConfig,
	}
	limits := producer.Limits
	limits.OrganizationBytes = uint64(contract.MaxContractDocumentBytes + 4*durableContinuationObjectBytes)
	limits.WorkspaceBytes = limits.OrganizationBytes
	fixture, publisher := newDurableContinuationFixture(t, authority, dependencies, producer, consumer, limits)
	continuations, err := app.NewContinuationService(
		fixture.Pool, publisher, producer.Backend, 2*contract.MaxContractDocumentBytes,
	)
	if err != nil {
		t.Fatal(err)
	}
	dependencies.Continuations = continuations
	registry := &app.HandlerRegistry{}
	if err := registry.RegisterHTTP(app.NewHTTPDirectReadRegistration(dependencies)); err != nil {
		t.Fatal(err)
	}
	fixture.WorkerRegistry = registry
	service := workerResultService(t, fixture, temporal, publisher, durableContinuationObjectBytes)
	cap := uint64(1)
	first := admitDurableContinuationRun(
		t, fixture, authority.selection, fixture.Client, fixture.Scope,
		fixture.Connection.ConnectionID, "durable-continuation-envelope-bound-first",
		durableContinuationInput(t, "chain", 1), contract.ResultControls{TotalRecordCap: &cap},
	)
	firstRun := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, first)
	firstUnit, err := executePermitWorkerUnit(t, service, firstRun)
	if err != nil || firstUnit.State != contract.RunStateSucceeded || providerCalls.Load() != 1 {
		t.Fatalf("readable envelope capacity rejected valid execution: unit=%#v error=%v provider_calls=%d", firstUnit, err, providerCalls.Load())
	}
	firstResult := loadDurableContinuationResult(t, fixture, first.RunRef)
	assertDurableContinuationIDs(t, firstResult, 1)
	second := admitDurableContinuationRun(
		t, fixture, authority.selection, fixture.Client, fixture.Scope,
		fixture.Connection.ConnectionID, "durable-continuation-envelope-bound-second",
		contract.NewObjectValue(map[string]contract.PresentValue{}),
		continuationControls(requireDurableContinuationToken(t, firstResult)),
	)
	secondRun := preparePermitWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, second)
	secondUnit, err := executePermitWorkerUnit(t, service, secondRun)
	if err != nil || secondUnit.State != contract.RunStateSucceeded || providerCalls.Load() != 1 {
		t.Fatalf("bounded envelope resume = %#v error=%v provider_calls=%d", secondUnit, err, providerCalls.Load())
	}
	assertDurableContinuationIDs(t, loadDurableContinuationResult(t, fixture, second.RunRef), 2)
	assertLatestRunPermitState(t, fixture, second.RunRef, store.AccountPermitReleased)
}
