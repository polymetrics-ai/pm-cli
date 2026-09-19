package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oapi-codegen/nullable"
	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const (
	resultDocumentInstanceID  = "server-result-document"
	resultDocumentProfileName = "result-document"
	resultDocumentWorkspace   = "alpha"
	resultDocumentConnection  = "c1"
	resultDocumentRunID       = "run-result-document"
	resultDocumentArtifact    = "artifact-result-document"
	resultDocumentCredential  = "pm-result-document-session"
)

// resultDocumentOptions selects the projection and event truth one fixture
// serves. The published document itself is always the same independently
// authored complete action result.
type resultDocumentOptions struct {
	state      pmapi.RunState
	unknown    string
	references []string
	oversized  bool
}

// resultDocumentFixture serves the minimum qualified PM server surface a
// generated command and the runs namespace need: one catalog page, admission,
// the durable event stream, the run-result projection and the exact published
// artifact bytes. It counts every route so a test can prove which surface was
// actually consumed.
type resultDocumentFixture struct {
	options         resultDocumentOptions
	document        []byte
	snapshot        pmapi.CatalogSnapshot
	server          *httptest.Server
	profilesPath    string
	sessions        pmapi.SessionStore
	invocationCalls atomic.Int32
	eventCalls      atomic.Int32
	resultCalls     atomic.Int32
	artifactCalls   atomic.Int32
}

func newResultDocumentFixture(t *testing.T, options resultDocumentOptions) *resultDocumentFixture {
	t.Helper()
	fixture := &resultDocumentFixture{options: options, document: publishedResultDocument(t)}
	fixture.snapshot = commandSnapshot([]pmapi.CommandFieldDescriptor{
		commandField("--query-workspace", "query.workspace", "workspace", contract.FieldLocationQuery),
	})
	fixture.snapshot.Binding.ServerInstanceID = resultDocumentInstanceID
	fixture.snapshot.Binding.WorkspaceID = resultDocumentWorkspace
	fixture.snapshot.Binding.ConnectionID = resultDocumentConnection
	for index := range fixture.snapshot.Commands {
		fixture.snapshot.Commands[index].ConnectionId = resultDocumentConnection
	}
	principal := fixture.snapshot.Binding.Principal

	fixture.server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/info":
			writeResultDocumentJSON(t, writer, pmapi.ServerInfo{
				ProtocolMajor:    pmapi.ProtocolMajor,
				ProtocolVersion:  pmapi.ServerInfoProtocolVersion(pmapi.WireVersion),
				InstanceId:       resultDocumentInstanceID,
				Features:         []string{"exact-values", "wire-problem-v1"},
				RequiredFeatures: []string{"exact-values"},
			})
		case "/v1/workspaces/" + resultDocumentWorkspace + "/commands":
			writeResultDocumentJSON(t, writer, pmapi.SafeCommandPage{
				CatalogRevision:  fixture.snapshot.Binding.CatalogRevision,
				ConnectionId:     resultDocumentConnection,
				Items:            fixture.snapshot.Commands,
				NextCursor:       nullable.NewNullNullable[string](),
				Principal:        principal,
				ServerInstanceId: resultDocumentInstanceID,
				WorkspaceId:      resultDocumentWorkspace,
			})
		case "/v1/workspaces/" + resultDocumentWorkspace + "/invocations":
			fixture.invocationCalls.Add(1)
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(writer).Encode(pmapi.AdmissionResponse{
				IntentDigest: "sha256:" + strings.Repeat("a", 64),
				OutcomeReference: pmapi.SafeOutcomeReference{
					Kind: pmapi.SafeOutcomeReferenceKindRun,
					Ref:  resultDocumentRunID,
				},
				RequestKey: request.Header.Get("Idempotency-Key"),
				RunRef:     resultDocumentRunID,
				State:      pmapi.AdmissionResponseStateAccepted,
			})
		case "/v1/workspaces/" + resultDocumentWorkspace + "/runs/" + resultDocumentRunID + "/events":
			fixture.eventCalls.Add(1)
			writeResultDocumentEventSnapshot(t, writer, options)
		case "/v1/workspaces/" + resultDocumentWorkspace + "/runs/" + resultDocumentRunID + "/result":
			fixture.resultCalls.Add(1)
			writeResultDocumentJSON(t, writer, fixture.runResult())
		case "/v1/workspaces/" + resultDocumentWorkspace + "/artifacts/" + resultDocumentArtifact + "/content":
			fixture.artifactCalls.Add(1)
			fixture.writeArtifactContent(writer)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(fixture.server.Close)

	fixture.profilesPath = filepath.Join(t.TempDir(), "profiles.json")
	handle, err := pmapi.DefaultSessionHandle(resultDocumentProfileName, resultDocumentInstanceID)
	if err != nil {
		t.Fatalf("derive result document session handle: %v", err)
	}
	if err := pmapi.AddServerProfile(fixture.profilesPath, pmapi.ServerProfile{
		Name:          resultDocumentProfileName,
		ServerURL:     fixture.server.URL,
		InstanceID:    resultDocumentInstanceID,
		SessionHandle: handle,
	}); err != nil {
		t.Fatalf("write result document profile: %v", err)
	}
	sessions := pmapi.NewMemorySessionStore()
	sessionKey, err := pmapi.NewSessionKey(resultDocumentInstanceID, handle)
	if err != nil {
		t.Fatalf("derive result document session key: %v", err)
	}
	if err := sessions.Store(t.Context(), sessionKey, pmapi.PMDeviceSession{
		ExpiresAt:           time.Now().Add(time.Hour),
		PmSessionCredential: resultDocumentCredential,
		Principal:           principal,
		SessionId:           "session-result-document",
	}); err != nil {
		t.Fatalf("store result document session: %v", err)
	}
	fixture.sessions = sessions
	return fixture
}

func (fixture *resultDocumentFixture) runResult() pmapi.RunResultResponse {
	references := make([]pmapi.RunResultReference, 0, len(fixture.options.references))
	for _, reference := range fixture.options.references {
		references = append(references, pmapi.RunResultReference{ResultRef: reference})
	}
	summary := resultDocumentEffectSummary(fixture.options)
	return pmapi.RunResultResponse{
		RunRef:             resultDocumentRunID,
		State:              fixture.options.state,
		EffectSummary:      summary,
		UnknownEffectCount: fixture.options.unknown,
		Revision:           "2",
		ResultReferences:   references,
	}
}

func (fixture *resultDocumentFixture) writeArtifactContent(writer http.ResponseWriter) {
	document := fixture.document
	if fixture.options.oversized {
		document = bytes.Repeat([]byte("x"), maximumRunResultDocumentBytes+1)
	}
	digest := sha256.Sum256(document)
	writer.Header().Set("Accept-Ranges", "bytes")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Disposition", "attachment")
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Content-Length", strconv.Itoa(len(document)))
	writer.Header().Set("ETag", `"sha256:`+hex.EncodeToString(digest[:])+`"`)
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(document)
}

func (fixture *resultDocumentFixture) execute(t *testing.T, arguments ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Execute(t.Context(), Options{
		Args: append([]string{
			"--server-profile", resultDocumentProfileName,
			"--workspace", resultDocumentWorkspace,
			"--session-store", "memory",
		}, arguments...),
		Stdout:       &stdout,
		Stderr:       &stderr,
		ProfilesPath: fixture.profilesPath,
		HTTPClient:   fixture.server.Client(),
		SessionStore: func(pmapi.SessionStoreMode) (pmapi.SessionStore, error) {
			return fixture.sessions, nil
		},
	})
	return code, stdout.String(), stderr.String()
}

func writeResultDocumentJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("encode result document fixture response: %v", err)
	}
}

func writeResultDocumentEventSnapshot(t *testing.T, writer http.ResponseWriter, options resultDocumentOptions) {
	t.Helper()
	orchestration := "completed"
	switch options.state {
	case pmapi.RunStateRunning:
		orchestration = "running"
	case pmapi.RunStateFailed, pmapi.RunStateCancelled:
		orchestration = "failed"
	case pmapi.RunStateReconciling:
		orchestration = "unknown"
	}
	document, err := json.Marshal(map[string]any{
		"cursor":             "cursor-" + resultDocumentRunID,
		"retention_gap":      false,
		"watermark_sequence": "0",
		"run": map[string]any{
			"blocked_reason":          nil,
			"cancel_requested":        false,
			"connection_refs":         []string{resultDocumentConnection},
			"effect_summary":          resultDocumentEffectSummary(options),
			"intent":                  map[string]any{"id": "operation-alpha", "kind": "operation", "revision": "1"},
			"orchestration_condition": orchestration,
			"origin":                  "api",
			"projection_observed_at":  "2026-09-19T00:00:00Z",
			"result_references":       []any{},
			"revision":                "2",
			"run_ref":                 resultDocumentRunID,
			"state":                   options.state,
			"unknown_effect_count":    options.unknown,
		},
	})
	if err != nil {
		t.Fatalf("encode result document event snapshot: %v", err)
	}
	writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte("event: snapshot\ndata: "))
	_, _ = writer.Write(document)
	_, _ = writer.Write([]byte("\n\n"))
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

func resultDocumentEffectSummary(options resultDocumentOptions) pmapi.EffectSummary {
	summary := pmapi.EffectSummary{
		Rejected:  "0",
		Succeeded: "0",
		Unchanged: "0",
		Unknown:   pmapi.ExactCounter(options.unknown),
		Unsent:    "0",
	}
	switch options.state {
	case pmapi.RunStateSucceeded:
		summary.Succeeded = "1"
	case pmapi.RunStatePartial:
		summary.Succeeded = "1"
		summary.Rejected = "1"
	case pmapi.RunStateFailed:
		summary.Rejected = "1"
	case pmapi.RunStateRunning:
		summary.Unsent = "1"
	}
	return summary
}

// publishedResultDocument is the independently authored complete action
// document the fixture publishes: nested objects and arrays, an explicit null,
// a false, a zero, an empty array, an exact large integer and exact
// identifiers must all survive the client byte for byte.
func publishedResultDocument(t *testing.T) []byte {
	t.Helper()
	largeInteger, err := contract.NewIntegerValue("9007199254740993123456790")
	if err != nil {
		t.Fatalf("build exact large integer: %v", err)
	}
	zero, err := contract.NewIntegerValue("0")
	if err != nil {
		t.Fatalf("build exact zero: %v", err)
	}
	number, err := contract.NewIntegerValue("42")
	if err != nil {
		t.Fatalf("build nested exact integer: %v", err)
	}
	result := contract.ActionResult{
		MediaType: "application/json",
		Data: contract.NewArrayValue([]contract.PresentValue{
			contract.NewObjectValue(map[string]contract.PresentValue{
				"id":          largeInteger,
				"archived_at": contract.NullValue(),
				"active":      contract.NewBooleanValue(false),
				"count":       zero,
				"labels":      contract.NewArrayValue([]contract.PresentValue{}),
				"repository": contract.NewObjectValue(map[string]contract.PresentValue{
					"name": contract.NewStringValue("retained"),
					"tags": contract.NewArrayValue([]contract.PresentValue{contract.NewStringValue("triage")}),
				}),
			}),
			contract.NewObjectValue(map[string]contract.PresentValue{
				"pull_request": contract.NewObjectValue(map[string]contract.PresentValue{
					"number": number,
				}),
			}),
		}),
		Pagination: &contract.ResultPagination{
			Continuation:    contract.AbsentValue(),
			UnreadTail:      []contract.PresentValue{},
			FetchedCount:    2,
			ReturnedCount:   2,
			EmittedCount:    2,
			SourceExhausted: true,
		},
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("validate published result document: %v", err)
	}
	document, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("encode published result document: %v", err)
	}
	return document
}

// assertPublishedDocumentValues proves the independently expected values are
// present in the rendered consumer output exactly as the server published
// them, including the values a float64 or omit-empty round trip would destroy.
func assertPublishedDocumentValues(t *testing.T, rendered string) {
	t.Helper()
	for _, expected := range []string{
		`"media_type":"application/json"`,
		`"exact":"9007199254740993123456790"`,
		`"presence":"null"`,
		`"boolean":false`,
		`"exact":"0"`,
		`"elements":[]`,
		`"text":"retained"`,
		`"text":"triage"`,
		`"pull_request"`,
		`"source_exhausted":true`,
	} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("rendered result document lost %s: %q", expected, rendered)
		}
	}
}

// assertNoCredentialLeak proves neither surface rendered the PM session
// credential: stdout carries the requested document or admission response, and
// stderr carries diagnostics only.
func assertNoCredentialLeak(t *testing.T, stdout, stderr string) {
	t.Helper()
	if strings.Contains(stdout, resultDocumentCredential) || strings.Contains(stderr, resultDocumentCredential) {
		t.Fatalf("PM session credential leaked into CLI output: stdout %q stderr %q", stdout, stderr)
	}
}

func TestGeneratedCommandWaitRendersPublishedActionDocument(t *testing.T) {
	fixture := newResultDocumentFixture(t, resultDocumentOptions{
		state:      pmapi.RunStateSucceeded,
		unknown:    "0",
		references: []string{resultDocumentArtifact},
	})
	code, stdout, stderr := fixture.execute(
		t,
		"--connection", resultDocumentConnection,
		"--output", "json",
		"inspect-widget", "--query-workspace", "provider",
		"--wait=true",
	)
	if code != 0 || stdout != string(fixture.document) {
		t.Fatalf("wait render = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	assertPublishedDocumentValues(t, stdout)
	if fixture.invocationCalls.Load() != 1 || fixture.eventCalls.Load() != 1 ||
		fixture.resultCalls.Load() != 1 || fixture.artifactCalls.Load() != 1 {
		t.Fatalf(
			"wait route calls = invocation %d event %d result %d artifact %d",
			fixture.invocationCalls.Load(),
			fixture.eventCalls.Load(),
			fixture.resultCalls.Load(),
			fixture.artifactCalls.Load(),
		)
	}
	if !strings.Contains(stderr, resultDocumentRunID) {
		t.Fatalf("wait diagnostics did not identify the accepted run: %q", stderr)
	}
	assertNoCredentialLeak(t, stdout, stderr)
}

func TestGeneratedCommandWithoutWaitKeepsAdmissionResponse(t *testing.T) {
	fixture := newResultDocumentFixture(t, resultDocumentOptions{
		state:      pmapi.RunStateSucceeded,
		unknown:    "0",
		references: []string{resultDocumentArtifact},
	})
	code, stdout, stderr := fixture.execute(
		t,
		"--connection", resultDocumentConnection,
		"--output", "json",
		"inspect-widget", "--query-workspace", "provider",
	)
	if code != 0 || !strings.Contains(stdout, `"state": "accepted"`) ||
		!strings.Contains(stdout, `"run_ref": "`+resultDocumentRunID+`"`) {
		t.Fatalf("admission-only render = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if stdout == string(fixture.document) {
		t.Fatal("admission-only render printed a result document")
	}
	assertNoCredentialLeak(t, stdout, stderr)
	if fixture.invocationCalls.Load() != 1 || fixture.eventCalls.Load() != 0 ||
		fixture.resultCalls.Load() != 0 || fixture.artifactCalls.Load() != 0 {
		t.Fatalf(
			"admission-only route calls = invocation %d event %d result %d artifact %d",
			fixture.invocationCalls.Load(),
			fixture.eventCalls.Load(),
			fixture.resultCalls.Load(),
			fixture.artifactCalls.Load(),
		)
	}
}

func TestGeneratedCommandWaitPreservesUncertaintyPrecedence(t *testing.T) {
	fixture := newResultDocumentFixture(t, resultDocumentOptions{
		state:   pmapi.RunStateReconciling,
		unknown: "1",
	})
	code, stdout, stderr := fixture.execute(
		t,
		"--connection", resultDocumentConnection,
		"--output", "json",
		"inspect-widget", "--query-workspace", "provider",
		"--wait=true",
	)
	if code != 7 || len(stdout) != 0 || !strings.Contains(stderr, "reconciliation") {
		t.Fatalf("unresolved wait = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if fixture.artifactCalls.Load() != 0 {
		t.Fatal("unresolved run without a published document read artifact bytes")
	}
}

func TestRunResultRendersExactPublishedDocument(t *testing.T) {
	fixture := newResultDocumentFixture(t, resultDocumentOptions{
		state:      pmapi.RunStateSucceeded,
		unknown:    "0",
		references: []string{resultDocumentArtifact},
	})
	for _, output := range []string{"text", "json"} {
		t.Run(output, func(t *testing.T) {
			code, stdout, stderr := fixture.execute(t, "--output", output, "runs", "result", resultDocumentRunID)
			if code != 0 || stdout != string(fixture.document) {
				t.Fatalf("runs result %s = code %d stdout %q stderr %q", output, code, stdout, stderr)
			}
			assertPublishedDocumentValues(t, stdout)
			if !strings.Contains(stdout, `"exact":"9007199254740993123456790"`) {
				t.Fatalf("exact large integer was not preserved: %q", stdout)
			}
			assertNoCredentialLeak(t, stdout, stderr)
		})
	}
	if fixture.resultCalls.Load() != 2 || fixture.artifactCalls.Load() != 2 {
		t.Fatalf(
			"runs result route calls = result %d artifact %d",
			fixture.resultCalls.Load(),
			fixture.artifactCalls.Load(),
		)
	}
}

func TestRunResultKeepsTerminalOutcomeWithPublishedDocument(t *testing.T) {
	fixture := newResultDocumentFixture(t, resultDocumentOptions{
		state:      pmapi.RunStateFailed,
		unknown:    "0",
		references: []string{resultDocumentArtifact},
	})
	code, stdout, stderr := fixture.execute(t, "runs", "result", resultDocumentRunID)
	if code != 6 || stdout != string(fixture.document) || !strings.Contains(stderr, "run failed") {
		t.Fatalf("terminal run document = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	assertPublishedDocumentValues(t, stdout)
}

func TestRunResultWithoutPublishedDocumentFailsExplicitly(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		options resultDocumentOptions
		code    int
		message string
	}{
		{
			name:    "succeeded run without a published document",
			options: resultDocumentOptions{state: pmapi.RunStateSucceeded, unknown: "0", references: []string{}},
			code:    5,
			message: "unavailable",
		},
		{
			name:    "unresolved run keeps reconciliation precedence",
			options: resultDocumentOptions{state: pmapi.RunStateReconciling, unknown: "1", references: []string{}},
			code:    7,
			message: "reconciliation",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newResultDocumentFixture(t, testCase.options)
			code, stdout, stderr := fixture.execute(t, "runs", "result", resultDocumentRunID)
			if code != testCase.code || len(stdout) != 0 || !strings.Contains(stderr, testCase.message) {
				t.Fatalf(
					"unpublished result = code %d stdout %q stderr %q; want code %d containing %q",
					code,
					stdout,
					stderr,
					testCase.code,
					testCase.message,
				)
			}
			if fixture.artifactCalls.Load() != 0 {
				t.Fatal("unpublished result read artifact bytes")
			}
		})
	}
}

func TestRunResultWithMultipleDocumentsFailsExplicitly(t *testing.T) {
	fixture := newResultDocumentFixture(t, resultDocumentOptions{
		state:      pmapi.RunStateSucceeded,
		unknown:    "0",
		references: []string{"artifact-result-first", resultDocumentArtifact},
	})
	code, stdout, stderr := fixture.execute(t, "runs", "result", resultDocumentRunID)
	if code != 5 || len(stdout) != 0 || !strings.Contains(stderr, "published 2 result documents") {
		t.Fatalf("multiple published documents = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if fixture.artifactCalls.Load() != 0 {
		t.Fatal("ambiguous published documents read one document silently")
	}
}

func TestRunResultDocumentBeyondBoundFailsExplicitly(t *testing.T) {
	fixture := newResultDocumentFixture(t, resultDocumentOptions{
		state:      pmapi.RunStateSucceeded,
		unknown:    "0",
		references: []string{resultDocumentArtifact},
		oversized:  true,
	})
	code, stdout, stderr := fixture.execute(t, "runs", "result", resultDocumentRunID)
	if code != 5 || len(stdout) != 0 || !strings.Contains(stderr, strconv.Itoa(maximumRunResultDocumentBytes)) {
		t.Fatalf("oversized published document = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if fixture.artifactCalls.Load() != 1 {
		t.Fatalf("oversized document route calls = %d", fixture.artifactCalls.Load())
	}
}
