package app_test

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/connectors"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture/githubcompat"
)

// The committed generated execution facts and the authored lane cases this test
// reads. Nothing below is copied from an observed PM result: the bundle supplies
// the transport, field and response facts, the authored cases supply the
// expected records and the expected request, and the controlled destination
// supplies the independent observation journal.
const (
	generatedGitHubConnectorID = "github"
	generatedGitHubOperationID = "issues/list-for-repo"

	// generatedGitHubOrigin is the provider origin the pinned source declares and
	// the origin the controlled destination impersonates. The committed bundle
	// must still admit exactly this origin, so a drifted origin fails closed
	// instead of selecting another destination.
	generatedGitHubOrigin = "https://api.github.com"

	// generatedGitHubBundleFile and generatedGitHubLaneFile are the declared
	// Bazel runfiles of the committed bundle and the authored direct-read cases.
	generatedGitHubBundleFile = "internal/connectors/data/github.bundle.json"
	generatedGitHubLaneFile   = "connectors/github/lanes/direct_read.json"

	// The authored direct-read cases this test evaluates. The two positive cases
	// share one fixture selection and one declared input; the negative case names
	// the wrong-record selection and still asserts the requested identity.
	generatedGitHubCaseMixedEntries   = "github-read-mixed-entries"
	generatedGitHubCaseNestedPresence = "github-read-complete-nested-presence"
	generatedGitHubCaseWrongRecordID  = "github-read-wrong-record-id"

	// generatedGitHubRequestBytes and generatedGitHubResponseBytes bound the
	// provider request and the authored page (page one of the authored records is
	// 3755 bytes). generatedGitHubResultBytes bounds the immutable published
	// result, and is also the bound of the durable continuation envelope.
	generatedGitHubRequestBytes  = 1 << 10
	generatedGitHubResponseBytes = 1 << 13
	generatedGitHubResultBytes   = 1 << 16

	// generatedGitHubJSONMediaType is the media type every authored destination
	// response declares.
	generatedGitHubJSONMediaType = "application/json; charset=utf-8"
)

// generatedGitHubBundleFacts are the committed generated execution facts this
// test resolved and the private bundle copy the composition reads.
type generatedGitHubBundleFacts struct {
	bundle     connectors.Bundle
	selection  app.OperationSelection
	bundlePath string
}

// generatedGitHubAuthoredCase is one authored direct-read case: its identity,
// the fixture selection it needs and the assertions it declares.
type generatedGitHubAuthoredCase struct {
	caseID     string
	fixture    string
	stepInput  map[string]any
	assertions []generatedGitHubAssertion
}

// generatedGitHubJournalEntry is one recorded observation of the controlled
// destination: what it received and what it returned. It decodes the
// destination's own journal contract and carries no provider credential.
type generatedGitHubJournalEntry struct {
	Method            string                     `json:"method"`
	Target            string                     `json:"target"`
	Status            int                        `json:"status"`
	ResponseMediaType string                     `json:"response_media_type"`
	RequestFields     map[string]json.RawMessage `json:"request_fields"`
	ResponseFields    map[string]json.RawMessage `json:"response_fields"`
	Redactions        []contract.ProofRedaction  `json:"redactions"`
}

// generatedGitHubRead is one completed controlled read: the provider-side
// observation, the durable terminal run and its immutable published result.
type generatedGitHubRead struct {
	runID      string
	status     int
	entries    int
	result     contract.ActionResult
	published  []byte
	dials      int32
	mismatches []string
}

// The authored lane inventory members this test reads. The remaining members are
// deliberately not decoded: an oracle that reads only what it evaluates cannot
// invent an expectation from an unread field.
type generatedGitHubInventory struct {
	SchemaVersion string                       `json:"schema_version"`
	ConnectorID   string                       `json:"connector_id"`
	Lane          string                       `json:"lane"`
	Operations    []generatedGitHubInventoryOp `json:"operations"`
}

type generatedGitHubInventoryOp struct {
	OperationID string                         `json:"operation_id"`
	Cases       []generatedGitHubInventoryCase `json:"cases"`
}

type generatedGitHubInventoryCase struct {
	CaseID   string                     `json:"case_id"`
	Scenario generatedGitHubScenarioDoc `json:"scenario"`
}

type generatedGitHubScenarioDoc struct {
	ScenarioID   string                       `json:"scenario_id"`
	ConnectorID  string                       `json:"connector_id"`
	Lane         string                       `json:"lane"`
	OperationID  string                       `json:"operation_id"`
	Requirements []generatedGitHubRequirement `json:"requirements"`
	Steps        []generatedGitHubStep        `json:"steps"`
	Assertions   []generatedGitHubAssertion   `json:"assertions"`
}

type generatedGitHubRequirement struct {
	RequirementID string `json:"requirement_id"`
	Kind          string `json:"kind"`
	Value         string `json:"value"`
}

type generatedGitHubStep struct {
	StepID string          `json:"step_id"`
	Kind   string          `json:"kind"`
	Action string          `json:"action"`
	Input  json.RawMessage `json:"input"`
}

type generatedGitHubAssertion struct {
	AssertionID string          `json:"assertion_id"`
	StepID      string          `json:"step_id"`
	Kind        string          `json:"kind"`
	Pointer     string          `json:"pointer"`
	Expected    json.RawMessage `json:"expected"`
}

// generatedGitHubDestination is one prepared controlled GitHub-compatible
// destination and its independent observation journal.
type generatedGitHubDestination struct {
	facts   githubcompat.Facts
	journal string
}

// TestGeneratedGitHubDirectReadThroughControlledDestination proves the
// credential-free GitHub read end to end through the real PM path: the committed
// generated bundle, the real admission service, the real worker planner and its
// physical call, the immutable published result, and the controlled
// GitHub-compatible destination with its own observation journal.
//
// The expectations are the authored direct-read cases, never the observed PM
// result, and the wrong-record control below fails the same oracle while the
// destination still answers HTTP 200. This is fake evidence: the destination
// serves authored bytes, never contacts GitHub, and holds no provider
// credential, so a passing case here never certifies live provider behavior.
func TestGeneratedGitHubDirectReadThroughControlledDestination(t *testing.T) {
	temporal := newPermitTemporalHarness(t)

	t.Run("authored_repository_page_is_published_exactly", func(t *testing.T) {
		observed := runGeneratedGitHubRead(
			t, temporal, generatedGitHubCaseMixedEntries, generatedGitHubCaseNestedPresence,
		)
		if len(observed.mismatches) != 0 {
			t.Fatalf("authored expectations rejected run %s: %v", observed.runID, observed.mismatches)
		}
		records, ok := observed.result.Data.Elements()
		if !ok || len(records) != 2 {
			t.Fatalf("published records of run %s = %#v; want the two authored page-one records",
				observed.runID, observed.result.Data)
		}
		// The published bytes themselves must carry the authored identities, so
		// the exact records survive publication and are not only a decoded view.
		for _, authored := range []string{
			`"exact":"9001"`, `"exact":"17"`, `"text":"Widget list omits archived entries"`,
			`"exact":"9002"`, `"exact":"18"`, `"text":"Fix widget"`,
			`"text":"https://api.github.com/repos/acme/widgets"`, `"text":"ada"`, `"pull_request"`,
		} {
			if !bytes.Contains(observed.published, []byte(authored)) {
				t.Fatalf("published result bytes of run %s do not contain %s", observed.runID, authored)
			}
		}
	})

	t.Run("wrong_record_page_fails_the_authored_identity_expectation", func(t *testing.T) {
		observed := runGeneratedGitHubRead(t, temporal, generatedGitHubCaseWrongRecordID)
		// The transport really answered the authored page: HTTP 200, one recorded
		// request, and the served wrong identity is what PM published.
		if observed.status != http.StatusOK || observed.entries != 1 {
			t.Fatalf("wrong-record destination answered status %d with %d journal entries",
				observed.status, observed.entries)
		}
		if !bytes.Contains(observed.published, []byte(`"exact":"9101"`)) {
			t.Fatalf("published result bytes of run %s do not carry the served wrong identity: %s",
				observed.runID, observed.published)
		}
		if len(observed.mismatches) != 1 {
			t.Fatalf("authored expectations of the wrong-record page reported %v; want one identity mismatch",
				observed.mismatches)
		}
		mismatch := observed.mismatches[0]
		if !strings.Contains(mismatch, "exact-id") || !strings.Contains(mismatch, `"9001"`) ||
			!strings.Contains(mismatch, `"9101"`) {
			t.Fatalf("wrong-record mismatch = %q; want the authored identity 9001 rejected against the served 9101",
				mismatch)
		}
	})
}

// runGeneratedGitHubRead drives one complete controlled read: it resolves the
// committed generated operation, serves the authored fixture selection the
// selected cases require, admits the authored input through the real admission
// service, plans and executes it through the real worker path, and evaluates the
// authored assertions against the immutable published result.
func runGeneratedGitHubRead(
	t *testing.T,
	temporal *permitTemporalHarness,
	caseIDs ...string,
) generatedGitHubRead {
	t.Helper()
	cases := generatedGitHubAuthoredCases(t, caseIDs)
	facts := generatedGitHubGeneratedFacts(t)
	destination := openGeneratedGitHubDestination(t, cases[0].fixture)

	// The controlled destination impersonates exactly the origin the committed
	// bundle admits, and the selected policy may name no other origin.
	if destination.facts.Origin != generatedGitHubOrigin {
		t.Fatalf("controlled destination origin = %q; want the admitted origin %q",
			destination.facts.Origin, generatedGitHubOrigin)
	}
	policy := contract.HTTPExecutionPolicy{
		MaxRequestBytes:  generatedGitHubRequestBytes,
		MaxResponseBytes: generatedGitHubResponseBytes,
		MaxDNSAnswers:    1,
		MaxAttempts:      2,
		MaxRetryDelay:    time.Minute,
		SelectedOrigin:   destination.facts.Origin,
	}
	if err := policy.ValidateForOperation(facts.selection.Operation); err != nil {
		t.Fatalf("controlled HTTP policy is not admitted by the generated operation: %v", err)
	}
	input := generatedGitHubAuthoredInput(t, facts.selection.Operation, cases[0].stepInput)
	target := generatedGitHubExpectedTarget(t, facts.selection.Operation, input)

	producer := testfixture.OpenArtifacts(t, testfixture.PrimaryArtifactGateway)
	consumer := testfixture.OpenArtifacts(t, testfixture.SecondaryArtifactGateway)
	dialer := &httpDirectReadDialer{address: destination.facts.Listen}
	trust := generatedGitHubTrustConfig(t, destination.facts.CAPath)
	var (
		registry  *app.HandlerRegistry
		resolver  app.OperationResolver
		publisher *objects.Publisher
	)
	fixture := testfixture.NewOperation(t, generatedGitHubOperationID, func(
		dependencies testfixture.ServiceDependencies,
		configuration *httpapi.ServiceConfig,
	) error {
		composition, err := app.OpenConnectorComposition(app.ConnectorCompositionConfig{
			BundlePaths: []string{facts.bundlePath},
		})
		if err != nil {
			return err
		}
		publisher, err = objects.NewPublisher(dependencies.Pool, producer.Backend, producer.Limits)
		if err != nil {
			return err
		}
		continuations, err := app.NewContinuationService(
			dependencies.Pool, publisher, producer.Backend, generatedGitHubResultBytes,
		)
		if err != nil {
			return err
		}
		registry = &app.HandlerRegistry{}
		if err := composition.RegisterDirectRead(registry, app.HTTPDirectReadDependencies{
			Resolver: httpDirectReadResolver{}, Dialer: dialer, TLSConfig: trust,
			Continuations: continuations,
		}); err != nil {
			return err
		}
		resolver = composition.Resolver()
		admission, err := app.NewAdmissionService(app.AdmissionServiceConfig{
			Pool:              dependencies.Pool,
			DeploymentID:      dependencies.DeploymentID,
			Authorize:         configuration.Auth.AuthorizeResource,
			Registry:          registry,
			ResolveOperation:  resolver,
			ResolveHTTPPolicy: generatedGitHubPolicyResolver(dependencies, policy),
			Policy:            httpDirectReadAdmissionPolicy(),
			Random:            cryptorand.Reader,
		})
		if err != nil {
			return err
		}
		configuration.Admission = admission
		// The published bytes are written through the primary gateway and read
		// back through the independent secondary gateway.
		configuration.Artifacts = consumer.Backend
		return nil
	})
	fixture.WorkerRegistry = registry
	fixture.ResolveOperation = resolver
	if publisher == nil || registry == nil || resolver == nil {
		t.Fatal("controlled composition did not construct its actual registry, resolver and publisher")
	}
	// The fixture seeds its own connection identity; the committed bundle's
	// connector is what the real resolver must find on that durable row.
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE pm.connections
			SET connector_id = $4
			WHERE org_id = $1 AND workspace_id = $2 AND connection_id = $3
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, fixture.Connection.ConnectionID, facts.bundle.ConnectorID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("fixture connection %q is not the seeded connection", fixture.Connection.ConnectionID)
		}
		return nil
	})

	service := workerResultService(t, fixture, temporal, publisher, generatedGitHubResultBytes)
	admitted := generatedGitHubAdmitRun(
		t, fixture, facts.selection, fixture.Client, fixture.Scope,
		fixture.Connection.ConnectionID, "generated-github-"+cases[0].caseID, input,
	)
	planned := planWorkerRun(t, fixture, service, temporal, fixture.Scope, fixture.Actor, admitted)
	if planned.dispatch.State != store.DispatchPrepared || len(planned.dispatch.Attempts) != 0 {
		t.Fatalf("production plan already holds send authority: %#v", planned.dispatch)
	}
	if planned.unit.DispatchID != planned.dispatch.Intent.DispatchID {
		t.Fatalf("planned unit %#v differs from its durable dispatch %#v", planned.unit, planned.dispatch.Intent.DispatchID)
	}
	result, err := executePermitWorkerUnit(t, service, permitWorkerRun{
		execution: planned.execution, reference: planned.reference,
		plan: testfixture.SealedWorkerPlan{TaskID: planned.unit.TaskID, Dispatch: planned.dispatch.Intent},
	})
	if err != nil || result.State != contract.RunStateSucceeded || result.UnknownEffectCount != 0 {
		t.Fatalf("controlled direct-read settlement = %#v error %v", result, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	projection, err := service.FinalizeRunProjection(ctx, planned.execution, contract.WorkflowProjectionRequest{
		Run: planned.reference,
	})
	if err != nil || projection.State != contract.RunStateSucceeded {
		t.Fatalf("controlled direct-read projection = %#v error %v", projection, err)
	}
	var record contract.RunRecord
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(
		ctx context.Context,
		tx *store.Transaction,
	) error {
		var err error
		record, err = store.LoadRun(ctx, tx, planned.reference.RunID)
		return err
	}); err != nil {
		t.Fatalf("load terminal controlled run: %v", err)
	}
	if record.State != contract.RunStateSucceeded || record.UnknownEffectCount != 0 {
		t.Fatalf("durable controlled run = %#v; want a terminal succeeded run", record)
	}

	references := workerResultReferences(t, fixture, planned.reference.RunID)
	if len(references) != 1 {
		t.Fatalf("published result references = %v; want one immutable result", references)
	}
	// The published page is far larger than the shared small-result reader's
	// bound, so it is read under the same bound the worker published it with.
	published := generatedGitHubPublishedBytes(t, fixture, references[0], generatedGitHubResultBytes)
	var publishedResult contract.ActionResult
	if err := json.Unmarshal(published, &publishedResult); err != nil {
		t.Fatalf("decode published controlled result: %v", err)
	}
	if err := publishedResult.Validate(); err != nil {
		t.Fatalf("validate published controlled result: %v", err)
	}
	// The generated operation declares a Link continuation, so the runtime must
	// report the authored page's continuation metadata and no provider total.
	observed := generatedGitHubRead{
		runID: planned.reference.RunID, result: publishedResult,
		published: published, dials: dialer.calls.Load(),
	}
	generatedGitHubRequirePagination(t, observed.result, cases[0].stepInput)
	if observed.dials != 1 {
		t.Fatalf("controlled destination dials = %d; want exactly one physical dial", observed.dials)
	}
	entries := generatedGitHubJournalEntries(t, destination.journal)
	observed.entries = len(entries)
	if len(entries) != 1 {
		t.Fatalf("controlled destination journal holds %d entries; want exactly one served request", len(entries))
	}
	observed.status = entries[0].Status
	generatedGitHubRequireObservation(t, facts.selection.Operation, entries[0], target, cases[0].stepInput)
	observed.mismatches = generatedGitHubAssertionMismatches(t, cases, publishedResult)
	return observed
}

// generatedGitHubGeneratedFacts loads the committed generated bundle, fails
// closed when it no longer declares the admitted direct read, and returns the
// private copy the composition reads.
func generatedGitHubGeneratedFacts(t *testing.T) generatedGitHubBundleFacts {
	t.Helper()
	document := generatedGitHubCommittedBytes(
		t, generatedGitHubCommittedFile(t, generatedGitHubBundleFile), contract.MaxContractDocumentBytes,
	)
	bundle, err := connectors.LoadBundle(bytes.NewReader(document))
	if err != nil {
		t.Fatalf("load committed connector bundle: %v", err)
	}
	if bundle.ConnectorID != generatedGitHubConnectorID {
		t.Fatalf("committed bundle declares connector %q; want %q", bundle.ConnectorID, generatedGitHubConnectorID)
	}
	var declared *contract.Operation
	for index := range bundle.Operations {
		if bundle.Operations[index].OperationID == generatedGitHubOperationID {
			declared = &bundle.Operations[index]
			break
		}
	}
	if declared == nil {
		t.Fatalf("committed bundle no longer declares operation %q", generatedGitHubOperationID)
	}
	if declared.Transport.Kind != contract.TransportKindHTTP || declared.Transport.Method != http.MethodGet ||
		declared.Transport.Path != "/repos/{owner}/{repo}/issues" {
		t.Fatalf("generated transport = %#v; want the admitted issue list GET", declared.Transport)
	}
	eligible := false
	for _, lane := range declared.LaneEligibility {
		if lane.Lane == contract.LaneDirectRead {
			eligible = lane.Status == contract.LaneEligible
		}
	}
	if !eligible {
		t.Fatalf("generated operation %q no longer declares direct_read eligible", generatedGitHubOperationID)
	}
	if declared.Execution == nil || declared.Execution.HTTP == nil ||
		len(declared.Execution.HTTP.Origins) != 1 || declared.Execution.HTTP.Origins[0] != generatedGitHubOrigin {
		t.Fatalf("generated operation does not admit exactly the controlled origin %q: %#v",
			generatedGitHubOrigin, declared.Execution)
	}
	pagination := declared.Execution.Pagination
	if pagination == nil || pagination.Kind != contract.OperationPaginationLink ||
		pagination.LinkResponseHeader != "Link" || pagination.LinkRequestField != "query.page" ||
		pagination.LimitRequestField != "query.per-page" {
		t.Fatalf("generated continuation facts = %#v; want the declared Link continuation", pagination)
	}
	// The composition refuses a symlinked declaration, and a Bazel runfile is a
	// symlink, so the exact committed bytes are copied into a private file.
	bundlePath := filepath.Join(t.TempDir(), "github.bundle.json")
	if err := os.WriteFile(bundlePath, document, 0o600); err != nil {
		t.Fatalf("declare committed connector bundle: %v", err)
	}
	if written := generatedGitHubCommittedBytes(t, bundlePath, contract.MaxContractDocumentBytes); !bytes.Equal(written, document) {
		t.Fatal("declared connector bundle copy changed the committed bytes")
	}
	composition, err := app.OpenConnectorComposition(app.ConnectorCompositionConfig{
		BundlePaths: []string{bundlePath},
	})
	if err != nil {
		t.Fatalf("compose committed connector bundle: %v", err)
	}
	if !composition.Configured() {
		t.Fatal("committed connector bundle composed an undeclared connector")
	}
	selection, err := app.ResolveBundledOperation(
		composition.Registry(), generatedGitHubConnectorID, generatedGitHubOperationID,
	)
	if err != nil {
		t.Fatalf("resolve bundled generated operation: %v", err)
	}
	if selection.Operation.OperationID != generatedGitHubOperationID ||
		selection.Operation.Transport != declared.Transport {
		t.Fatalf("resolved generated operation = %#v; want the committed declaration %#v",
			selection.Operation, declared.Transport)
	}
	return generatedGitHubBundleFacts{bundle: bundle, selection: selection, bundlePath: bundlePath}
}

// generatedGitHubAuthoredCases reads the authored direct-read cases this test
// evaluates. Every selected case must name one fixture selection and one
// declared input, so one controlled run answers all of them.
func generatedGitHubAuthoredCases(t *testing.T, caseIDs []string) []generatedGitHubAuthoredCase {
	t.Helper()
	if len(caseIDs) == 0 {
		t.Fatal("controlled generated read requires at least one authored case")
	}
	document := generatedGitHubCommittedBytes(
		t, generatedGitHubCommittedFile(t, generatedGitHubLaneFile), contract.MaxContractDocumentBytes,
	)
	var inventory generatedGitHubInventory
	if err := json.Unmarshal(document, &inventory); err != nil {
		t.Fatalf("decode authored lane inventory: %v", err)
	}
	if inventory.SchemaVersion != "pm.lane-inventory/v1" || inventory.ConnectorID != generatedGitHubConnectorID ||
		inventory.Lane != string(contract.LaneDirectRead) {
		t.Fatalf("authored lane inventory = %#v; want the declared %s direct_read inventory",
			inventory, generatedGitHubConnectorID)
	}
	indexed := make(map[string]generatedGitHubInventoryCase)
	for _, operation := range inventory.Operations {
		if operation.OperationID != generatedGitHubOperationID {
			continue
		}
		for _, authored := range operation.Cases {
			indexed[authored.CaseID] = authored
		}
	}
	served := make(map[string]struct{})
	for _, fixture := range githubcompat.Fixtures() {
		served[fixture.Value] = struct{}{}
	}
	cases := make([]generatedGitHubAuthoredCase, 0, len(caseIDs))
	for _, caseID := range caseIDs {
		authored, present := indexed[caseID]
		if !present {
			t.Fatalf("authored lane inventory no longer declares case %q", caseID)
		}
		scenario := authored.Scenario
		if scenario.ScenarioID != caseID || scenario.ConnectorID != generatedGitHubConnectorID ||
			scenario.OperationID != generatedGitHubOperationID ||
			scenario.Lane != string(contract.LaneDirectRead) || len(scenario.Steps) != 1 {
			t.Fatalf("authored case %q scenario = %#v; want one admitted direct-read invoke", caseID, scenario)
		}
		step := scenario.Steps[0]
		if step.Kind != "invoke" || step.Action != generatedGitHubOperationID || step.StepID == "" {
			t.Fatalf("authored case %q step = %#v; want the declared issue list invoke", caseID, step)
		}
		var stepInput map[string]any
		decoder := json.NewDecoder(bytes.NewReader(step.Input))
		decoder.UseNumber()
		if err := decoder.Decode(&stepInput); err != nil || len(stepInput) == 0 {
			t.Fatalf("decode authored case %q input: %v", caseID, err)
		}
		fixture := ""
		for _, requirement := range scenario.Requirements {
			if requirement.Kind == "fixture" {
				fixture = requirement.Value
			}
		}
		if _, available := served[fixture]; !available {
			t.Fatalf("authored case %q requires fixture %q, which the controlled destination does not serve",
				caseID, fixture)
		}
		if len(scenario.Assertions) == 0 {
			t.Fatalf("authored case %q declares no assertion", caseID)
		}
		candidate := generatedGitHubAuthoredCase{
			caseID: caseID, fixture: fixture,
			stepInput: stepInput, assertions: scenario.Assertions,
		}
		// Every selected case must be answered by the same single controlled run.
		if len(cases) != 0 && (candidate.fixture != cases[0].fixture ||
			!generatedGitHubSameInput(cases[0].stepInput, candidate.stepInput)) {
			t.Fatalf("authored case %q needs fixture %q and input %v; want the selected %q and %v",
				caseID, candidate.fixture, candidate.stepInput, cases[0].fixture, cases[0].stepInput)
		}
		cases = append(cases, candidate)
	}
	return cases
}

// generatedGitHubSameInput reports whether two authored inputs declare exactly
// the same fields and values.
func generatedGitHubSameInput(left, right map[string]any) bool {
	canonical := func(input map[string]any) string {
		document, err := json.Marshal(input)
		if err != nil {
			return ""
		}
		return generatedGitHubCanonicalRaw(document)
	}
	return canonical(left) == canonical(right)
}

// generatedGitHubAuthoredInput converts one authored invoke input into the typed
// value the generated field bindings admit, so the run executes the declared
// request rather than a request this test invented.
func generatedGitHubAuthoredInput(
	t *testing.T,
	operation contract.Operation,
	declared map[string]any,
) contract.PresentValue {
	t.Helper()
	// The authored map is read-only: the same case supplies the expected request
	// and the observation assertions after this conversion.
	remaining := make(map[string]any, len(declared))
	for field, value := range declared {
		remaining[field] = value
	}
	fields := make(map[string]contract.PresentValue, len(declared))
	for _, binding := range operation.Inputs {
		supplied, present := remaining[binding.FieldID]
		if !present {
			continue
		}
		switch binding.Schema.Type {
		case contract.SchemaTypeString:
			text, ok := supplied.(string)
			if !ok {
				t.Fatalf("authored field %q = %#v; want a declared string", binding.FieldID, supplied)
			}
			fields[binding.FieldID] = contract.NewStringValue(text)
		case contract.SchemaTypeInteger:
			number, ok := supplied.(json.Number)
			if !ok {
				t.Fatalf("authored field %q = %#v; want a declared integer", binding.FieldID, supplied)
			}
			value, err := contract.NewIntegerValue(number.String())
			if err != nil {
				t.Fatalf("authored field %q = %q is not an exact integer: %v", binding.FieldID, number, err)
			}
			fields[binding.FieldID] = value
		default:
			t.Fatalf("authored field %q declares unsupported type %q", binding.FieldID, binding.Schema.Type)
		}
		delete(remaining, binding.FieldID)
	}
	if len(remaining) != 0 {
		t.Fatalf("authored input declares fields the generated operation does not: %v", remaining)
	}
	input := contract.NewObjectValue(fields)
	if err := contract.ValidateOperationInput(operation, input); err != nil {
		t.Fatalf("authored input is not admitted by the generated operation: %v", err)
	}
	return input
}

// generatedGitHubExpectedTarget builds the exact request target the generated
// transport facts and the authored input require: the declared template path
// with its path bindings substituted, then the declared query bindings in
// declaration order.
func generatedGitHubExpectedTarget(
	t *testing.T,
	operation contract.Operation,
	input contract.PresentValue,
) string {
	t.Helper()
	fields, ok := input.Fields()
	if !ok {
		t.Fatal("authored input is not a present object")
	}
	path := operation.Transport.Path
	query := make([]string, 0)
	for _, binding := range operation.Inputs {
		value, supplied := fields[binding.FieldID]
		if !supplied || value.Presence() != contract.PresenceValue {
			continue
		}
		switch binding.Location {
		case contract.FieldLocationPath:
			text, ok := value.TextValue()
			if !ok {
				t.Fatalf("authored path binding %q is not a string", binding.FieldID)
			}
			path = strings.Replace(path, "{"+binding.WireName+"}", url.PathEscape(text), 1)
		case contract.FieldLocationQuery:
			number, ok := value.ExactNumber()
			if !ok {
				t.Fatalf("authored query binding %q is not an exact value", binding.FieldID)
			}
			query = append(query, binding.WireName+"="+strings.ReplaceAll(url.QueryEscape(number.Text), "+", "%20"))
		default:
			t.Fatalf("authored binding %q declares unsupported location %q", binding.FieldID, binding.Location)
		}
	}
	if strings.Contains(path, "{") {
		t.Fatalf("authored input left the declared path template %q unresolved", path)
	}
	if len(query) == 0 {
		return path
	}
	return path + "?" + strings.Join(query, "&")
}

// generatedGitHubPolicyResolver selects the controlled HTTP policy for exactly
// the fixture's own connections, so no other scope can borrow it.
func generatedGitHubPolicyResolver(
	dependencies testfixture.ServiceDependencies,
	policy contract.HTTPExecutionPolicy,
) func(contract.Scope, string) (contract.HTTPExecutionPolicy, bool) {
	return func(scope contract.Scope, connectionID string) (contract.HTTPExecutionPolicy, bool) {
		primary := scope.Equal(dependencies.Scope) && connectionID == dependencies.Connection.ConnectionID
		foreign := scope.Equal(dependencies.ForeignScope) && connectionID == dependencies.ForeignConnection.ConnectionID
		return policy, primary || foreign
	}
}

// generatedGitHubAdmitRun admits the authored input through the real admission
// service under the resolved generated operation revision.
func generatedGitHubAdmitRun(
	t *testing.T,
	fixture *testfixture.Fixture,
	selection app.OperationSelection,
	client *http.Client,
	scope contract.Scope,
	connectionID string,
	requestKey string,
	input contract.PresentValue,
) api.AdmissionResponse {
	t.Helper()
	revision, err := app.OperationSelectionRevision(selection)
	if err != nil {
		t.Fatalf("resolve generated operation revision: %v", err)
	}
	return requireFreshInitialAdmission(t, submitInitialInvocation(t, fixture, client, scope.WorkspaceID, api.InvocationRequest{
		ConnectionRef:  connectionID,
		OperationRef:   selection.Operation.OperationID,
		Lane:           api.InvocationRequestLane(contract.LaneDirectRead),
		Input:          input,
		ResultControls: contract.ResultControls{},
		Protocol: api.ProtocolMetadata{
			WireVersion:      api.ProtocolMetadataWireVersion(api.WireVersion),
			RequiredFeatures: []string{},
			RequestKey:       requestKey,
			IntentDigest:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			ExpectedRevision: revision,
		},
	}), requestKey)
}

// generatedGitHubRequirePagination proves the continuation metadata the
// generated Link strategy must produce for the authored page: the declared page
// size, the authored continuation, and no provider total.
func generatedGitHubRequirePagination(
	t *testing.T,
	result contract.ActionResult,
	declared map[string]any,
) {
	t.Helper()
	pageSize, ok := declared["query.per-page"].(json.Number)
	if !ok {
		t.Fatalf("authored input does not declare query.per-page: %v", declared)
	}
	limit, err := strconv.ParseUint(pageSize.String(), 10, 64)
	if err != nil {
		t.Fatalf("authored page size %q is not an exact positive integer: %v", pageSize, err)
	}
	pagination := result.Pagination
	if pagination == nil {
		t.Fatal("published generated result carries no continuation metadata")
	}
	if pagination.EffectiveWireLimit == nil || *pagination.EffectiveWireLimit != limit {
		t.Fatalf("published effective wire limit = %v; want the authored page size %d",
			pagination.EffectiveWireLimit, limit)
	}
	if pagination.Continuation.Presence() != contract.PresenceValue {
		t.Fatalf("published continuation = %#v; want the sealed authored continuation",
			pagination.Continuation)
	}
	if pagination.SourceExhausted {
		t.Fatal("published continuation reports an exhausted source although the authored source continues")
	}
	if pagination.TotalRecordCap != nil || pagination.ProviderTotal != nil || len(pagination.UnreadTail) != 0 {
		t.Fatalf("published pagination facts = %#v; want no caller cap, no provider total and no unread tail", pagination)
	}
	if pagination.FetchedCount != limit || pagination.ReturnedCount != limit || pagination.EmittedCount != limit {
		t.Fatalf("published pagination counts = %d/%d/%d; want the authored page size %d",
			pagination.FetchedCount, pagination.ReturnedCount, pagination.EmittedCount, limit)
	}
}

// generatedGitHubRequireObservation proves the independent provider-side record
// of the one served request: the exact received target, the declared query
// serialization, the authored response shape, and the absence of any provider
// credential material.
func generatedGitHubRequireObservation(
	t *testing.T,
	operation contract.Operation,
	entry generatedGitHubJournalEntry,
	target string,
	declared map[string]any,
) {
	t.Helper()
	if entry.Method != http.MethodGet || entry.Status != http.StatusOK {
		t.Fatalf("controlled destination observed %s status %d; want GET 200", entry.Method, entry.Status)
	}
	if entry.Target != target {
		t.Fatalf("controlled destination observed target %q; want the declared target %q", entry.Target, target)
	}
	var query map[string]string
	if err := json.Unmarshal(entry.RequestFields["query"], &query); err != nil {
		t.Fatalf("decode observed request query: %v", err)
	}
	// Only the declared query bindings may appear, with exactly the authored
	// values: a substituted owner, repository or page size cannot pass.
	expected := make(map[string]string)
	for field, value := range declared {
		binding := generatedGitHubDeclaredBinding(t, operation, field)
		if binding.Location != contract.FieldLocationQuery {
			continue
		}
		expected[binding.WireName] = fmt.Sprint(value)
	}
	if len(expected) == 0 {
		t.Fatalf("authored input declares no query binding: %v", declared)
	}
	if len(query) != len(expected) {
		t.Fatalf("observed query = %v; want exactly the declared query bindings %v", query, expected)
	}
	for wire, text := range expected {
		if query[wire] != text {
			t.Fatalf("observed query %s = %q; want the authored value %q", wire, query[wire], text)
		}
	}
	if len(entry.Redactions) != 0 {
		t.Fatalf("controlled destination recorded credential material: %#v", entry.Redactions)
	}
	if entry.ResponseMediaType != generatedGitHubJSONMediaType {
		t.Fatalf("controlled destination served media type %q; want %q", entry.ResponseMediaType, generatedGitHubJSONMediaType)
	}
	var headers map[string]string
	if err := json.Unmarshal(entry.ResponseFields["headers"], &headers); err != nil {
		t.Fatalf("decode served response headers: %v", err)
	}
	// The authored destination continues with the declared navigation and limit
	// bindings on the declared path, so the served continuation is derived from
	// the generated facts and the authored input rather than transcribed here.
	pagination := operation.Execution.Pagination
	if pagination == nil {
		t.Fatal("generated operation declares no continuation facts")
	}
	path, _, _ := strings.Cut(target, "?")
	link := "<" + generatedGitHubOrigin + path + "?" +
		generatedGitHubDeclaredBinding(t, operation, pagination.LimitRequestField).WireName + "=" +
		generatedGitHubAuthoredFieldText(t, declared, pagination.LimitRequestField) + "&" +
		generatedGitHubDeclaredBinding(t, operation, pagination.LinkRequestField).WireName + "=" +
		strconv.Itoa(generatedGitHubAuthoredPage(t, declared)+1) + `>; rel="next"`
	if headers["link"] != link {
		t.Fatalf("controlled destination served continuation %q; want the authored next page %q", headers["link"], link)
	}
	if headers["content-type"] != generatedGitHubJSONMediaType {
		t.Fatalf("controlled destination served content type %q; want %q", headers["content-type"], generatedGitHubJSONMediaType)
	}
}

// generatedGitHubDeclaredBinding returns the committed operation's declared
// binding for one field id, so the observed request is compared with the
// generated declaration rather than with a name copied into this test.
func generatedGitHubDeclaredBinding(
	t *testing.T,
	operation contract.Operation,
	fieldID string,
) contract.FieldBinding {
	t.Helper()
	for _, binding := range operation.Inputs {
		if binding.FieldID == fieldID {
			return binding
		}
	}
	t.Fatalf("generated operation does not declare field %q", fieldID)
	return contract.FieldBinding{}
}

// generatedGitHubAuthoredFieldText returns the exact text of one authored input
// value, whatever its declared JSON type.
func generatedGitHubAuthoredFieldText(t *testing.T, declared map[string]any, fieldID string) string {
	t.Helper()
	value, present := declared[fieldID]
	if !present {
		t.Fatalf("authored input does not declare %q", fieldID)
	}
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	default:
		t.Fatalf("authored input %q = %#v; want a declared scalar", fieldID, value)
		return ""
	}
}

// generatedGitHubAuthoredPage returns the authored page input.
func generatedGitHubAuthoredPage(t *testing.T, declared map[string]any) int {
	t.Helper()
	value, present := declared["query.page"]
	if !present {
		t.Fatal("authored input does not declare query.page")
	}
	number, ok := value.(json.Number)
	if !ok {
		t.Fatalf("authored input query.page = %#v; want an integer", value)
	}
	page, err := strconv.Atoi(number.String())
	if err != nil {
		t.Fatalf("authored input query.page = %q; want an integer", number)
	}
	return page
}

// generatedGitHubAssertionMismatches evaluates every authored assertion of the
// selected cases against the published result. Each mismatch names the case, the
// assertion, the pointer and both values, so the wrong-record control can prove
// the oracle rejected an HTTP 200 page instead of restating the result.
func generatedGitHubAssertionMismatches(
	t *testing.T,
	cases []generatedGitHubAuthoredCase,
	result contract.ActionResult,
) []string {
	t.Helper()
	document, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("encode published result for authored assertions: %v", err)
	}
	tree, err := generatedGitHubJSONTree(document)
	if err != nil {
		t.Fatalf("decode published result for authored assertions: %v", err)
	}
	mismatches := make([]string, 0)
	for _, authored := range cases {
		for _, assertion := range authored.assertions {
			if assertion.StepID == "" || assertion.AssertionID == "" || assertion.Pointer == "" {
				t.Fatalf("authored case %q declares an incomplete assertion: %#v", authored.caseID, assertion)
			}
			switch assertion.Kind {
			case "equals", "absent":
			default:
				t.Fatalf("authored case %q declares assertion kind %q, which this oracle does not evaluate",
					authored.caseID, assertion.Kind)
			}
			mismatch, rejected := generatedGitHubAssertionMismatch(assertion, tree)
			if !rejected {
				continue
			}
			mismatches = append(mismatches, fmt.Sprintf(
				"%s %s %s: %s", authored.caseID, assertion.AssertionID, assertion.Pointer, mismatch,
			))
		}
	}
	return mismatches
}

// generatedGitHubAssertionMismatch evaluates one authored assertion against the
// published result. The second result reports a refusal: the caller fails closed
// on any assertion kind it did not select, so an unevaluated expectation can
// never pass silently.
func generatedGitHubAssertionMismatch(assertion generatedGitHubAssertion, tree any) (string, bool) {
	node, found := generatedGitHubPointerValue(tree, assertion.Pointer)
	switch assertion.Kind {
	case "absent":
		if !found {
			return "", false
		}
		var observed contract.PresentValue
		if err := json.Unmarshal(generatedGitHubNode(node), &observed); err == nil &&
			observed.Presence() == contract.PresenceAbsent {
			return "", false
		}
		return "expected an absent value; observed " + generatedGitHubCanonical(node), true
	case "equals":
		var expected contract.PresentValue
		if err := json.Unmarshal(assertion.Expected, &expected); err == nil {
			if !found {
				return "expected " + generatedGitHubCanonicalRaw(assertion.Expected) + "; the pointer is absent", true
			}
			var observed contract.PresentValue
			if err := json.Unmarshal(generatedGitHubNode(node), &observed); err != nil {
				return "observed value is not a typed value: " + generatedGitHubCanonical(node), true
			}
			if expected.Equal(observed) {
				return "", false
			}
			return "expected " + generatedGitHubCanonicalRaw(assertion.Expected) +
				"; observed " + generatedGitHubCanonical(node), true
		}
		if !found {
			return "expected " + generatedGitHubCanonicalRaw(assertion.Expected) + "; the pointer is absent", true
		}
		if generatedGitHubCanonicalRaw(assertion.Expected) != generatedGitHubCanonical(node) {
			return "expected " + generatedGitHubCanonicalRaw(assertion.Expected) +
				"; observed " + generatedGitHubCanonical(node), true
		}
		return "", false
	default:
		return "unsupported authored assertion kind " + assertion.Kind, true
	}
}

// generatedGitHubPointerValue resolves one RFC 6901 pointer against a decoded
// JSON document, reporting whether the pointer identifies a value at all.
func generatedGitHubPointerValue(tree any, pointer string) (any, bool) {
	if pointer == "" || !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	current := tree
	for _, token := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
		switch node := current.(type) {
		case map[string]any:
			value, present := node[token]
			if !present {
				return nil, false
			}
			current = value
		case []any:
			index, err := strconv.Atoi(token)
			if err != nil || index < 0 || index >= len(node) {
				return nil, false
			}
			current = node[index]
		default:
			return nil, false
		}
	}
	return current, true
}

// generatedGitHubJSONTree decodes one document with exact numbers retained.
func generatedGitHubJSONTree(document []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	var tree any
	if err := decoder.Decode(&tree); err != nil {
		return nil, err
	}
	return tree, nil
}

// generatedGitHubNode encodes one decoded node. It reports a node that cannot be
// re-encoded as an empty document, which no assertion can match.
func generatedGitHubNode(node any) []byte {
	if node == nil {
		return []byte("null")
	}
	document, err := json.Marshal(node)
	if err != nil {
		return []byte{}
	}
	return document
}

// generatedGitHubCanonical renders one decoded node canonically.
func generatedGitHubCanonical(node any) string {
	tree, err := generatedGitHubJSONTree(generatedGitHubNode(node))
	if err != nil {
		return string(generatedGitHubNode(node))
	}
	document, err := json.Marshal(tree)
	if err != nil {
		return string(generatedGitHubNode(node))
	}
	return string(document)
}

// generatedGitHubCanonicalRaw renders one raw expectation canonically.
func generatedGitHubCanonicalRaw(raw json.RawMessage) string {
	tree, err := generatedGitHubJSONTree(raw)
	if err != nil {
		return string(raw)
	}
	document, err := json.Marshal(tree)
	if err != nil {
		return string(raw)
	}
	return string(document)
}

// openGeneratedGitHubDestination prepares, starts and announces one controlled
// GitHub-compatible destination, and requires it to stop cleanly afterwards.
func openGeneratedGitHubDestination(t *testing.T, fixtureValue string) *generatedGitHubDestination {
	t.Helper()
	directory := t.TempDir()
	journal := filepath.Join(directory, "journal.jsonl")
	destination, err := githubcompat.Prepare(githubcompat.Config{
		Listen:  "127.0.0.1:0",
		Origin:  generatedGitHubOrigin,
		TLSDir:  filepath.Join(directory, "tls"),
		Journal: journal,
		Fixture: fixtureValue,
	})
	if err != nil {
		t.Fatalf("prepare controlled GitHub-compatible destination: %v", err)
	}
	announced := make(chan githubcompat.Facts, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- destination.Run(ctx, func(facts githubcompat.Facts) { announced <- facts })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("controlled destination failed while serving: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("controlled destination did not stop after cancellation")
		}
	})
	select {
	case facts := <-announced:
		if facts.JournalPath != journal || facts.Fixture != fixtureValue {
			t.Fatalf("controlled destination facts = %#v; want journal %q and fixture %q",
				facts, journal, fixtureValue)
		}
		return &generatedGitHubDestination{facts: facts, journal: journal}
	case err := <-done:
		t.Fatalf("controlled destination stopped before announcing its facts: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("controlled destination did not announce its facts")
	}
	return nil
}

// generatedGitHubPublishedBytes reads one published immutable result through the
// authenticated artifact API under the bound the worker published it with.
func generatedGitHubPublishedBytes(
	t *testing.T,
	fixture *testfixture.Fixture,
	artifactRef string,
	maximum int64,
) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		fixture.BaseURL+"/v1/workspaces/"+fixture.Scope.WorkspaceID+"/artifacts/"+artifactRef+"/content",
		nil,
	)
	if err != nil {
		t.Fatalf("construct published result read: %v", err)
	}
	response, err := fixture.Client.Do(request)
	if err != nil {
		t.Fatalf("read published result: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("published result read status = %d; want %d", response.StatusCode, http.StatusOK)
	}
	document, err := io.ReadAll(io.LimitReader(response.Body, maximum+1))
	if err != nil {
		t.Fatalf("read published result bytes: %v", err)
	}
	if int64(len(document)) > maximum {
		t.Fatalf("published result exceeds its %d byte bound", maximum)
	}
	return document
}

// generatedGitHubJournalEntries decodes the destination's own observation
// journal in arrival order. An absent journal is an incomplete input, never an
// empty observation.
func generatedGitHubJournalEntries(t *testing.T, path string) []generatedGitHubJournalEntry {
	t.Helper()
	document := generatedGitHubCommittedBytes(t, path, 1<<20)
	entries := make([]generatedGitHubJournalEntry, 0, 1)
	for _, line := range bytes.Split(bytes.TrimSuffix(document, []byte("\n")), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var entry generatedGitHubJournalEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("decode controlled destination journal entry: %v", err)
		}
		entries = append(entries, entry)
	}
	return entries
}

// generatedGitHubTrustConfig trusts exactly the generated authority of the
// controlled destination. It reads no system or global trust store.
func generatedGitHubTrustConfig(t *testing.T, caPath string) *tls.Config {
	t.Helper()
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(generatedGitHubCommittedBytes(t, caPath, 1<<16)) {
		t.Fatalf("generated destination authority %q is not a PEM certificate", caPath)
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
}

// generatedGitHubCommittedFile resolves one declared repository file inside the
// declared Bazel runfiles, and inside the checkout when the test runs outside
// Bazel. A missing declaration fails closed instead of silently reading another
// document.
func generatedGitHubCommittedFile(t *testing.T, relative string) string {
	t.Helper()
	candidates := make([]string, 0, 2)
	if runfiles := os.Getenv("TEST_SRCDIR"); runfiles != "" {
		workspace := os.Getenv("TEST_WORKSPACE")
		if workspace == "" || workspace == "." || workspace == ".." || filepath.Base(workspace) != workspace {
			t.Fatalf("declared Bazel workspace name is required to locate %s", relative)
		}
		candidates = append(candidates, filepath.Join(runfiles, workspace, filepath.FromSlash(relative)))
	}
	if working, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(working, "..", "..", filepath.FromSlash(relative)))
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		absolute, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		return absolute
	}
	t.Fatalf("declared repository file %s is unavailable", relative)
	return ""
}

// generatedGitHubCommittedBytes reads one declared file under an explicit bound.
func generatedGitHubCommittedBytes(t *testing.T, path string, maximum int64) []byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open declared file %q: %v", path, err)
	}
	defer func() { _ = file.Close() }()
	document, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		t.Fatalf("read declared file %q: %v", path, err)
	}
	if int64(len(document)) > maximum {
		t.Fatalf("declared file %q exceeds its %d byte bound", path, maximum)
	}
	if len(document) == 0 {
		t.Fatalf("declared file %q is empty", path)
	}
	return document
}
