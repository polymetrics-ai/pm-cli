package clientparity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	sdkclient "go.temporal.io/sdk/client"
	sdkworker "go.temporal.io/sdk/worker"

	"github.com/polymetrics-ai/pm-cli/internal/activities"
	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/connectors"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/httpapi"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	pmtemporal "github.com/polymetrics-ai/pm-cli/internal/temporal"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
	"github.com/polymetrics-ai/pm-cli/internal/workflows"
)

const (
	parityProfileName = "parity"
	parityInputLimit  = 128 << 10
)

var parityTemporalIdentitySequence atomic.Uint64

type parityFixtureSource struct {
	SchemaVersion     string `json:"schema_version"`
	OperationID       string `json:"operation_id"`
	OperationRevision string `json:"operation_revision"`
	InputField        string `json:"input_field"`
	FirstText         string `json:"first_text"`
	ChangedText       string `json:"changed_text"`
	RequestKey        string `json:"request_key"`
	EvidenceMode      string `json:"evidence_mode"`
}

type parityEffectEvidence struct {
	RunID        string
	ConnectionID string
	OperationID  string
	TaskID       string
	EffectKey    contract.EffectKey
	DispatchID   contract.DispatchID
	AttemptID    contract.AttemptID
	Text         string
}

type parityEffectOracle struct {
	mu                 sync.Mutex
	expectedRunID      string
	expectedConnection string
	expectedOperation  string
	expectedField      string
	expectedText       string
	calls              int
	evidence           parityEffectEvidence
}

func (oracle *parityEffectOracle) bind(runID, connectionID, operationID, field, text string) error {
	oracle.mu.Lock()
	defer oracle.mu.Unlock()
	if oracle.expectedRunID != "" || runID == "" || connectionID == "" || operationID == "" || field == "" {
		return errors.New("controlled effect oracle binding is invalid")
	}
	oracle.expectedRunID = runID
	oracle.expectedConnection = connectionID
	oracle.expectedOperation = operationID
	oracle.expectedField = field
	oracle.expectedText = text
	return nil
}

func (oracle *parityEffectOracle) execute(ctx context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
	if ctx == nil || ctx.Err() != nil {
		return app.ExecutionObservation{}, errors.New("controlled effect context is unavailable")
	}
	oracle.mu.Lock()
	defer oracle.mu.Unlock()
	oracle.calls++
	if oracle.calls != 1 || oracle.expectedRunID == "" {
		return app.ExecutionObservation{}, errors.New("controlled effect was invoked more than once or before binding")
	}
	fields, ok := request.Input.Fields()
	if !ok || len(fields) != 1 {
		return app.ExecutionObservation{}, errors.New("controlled effect did not receive one typed input field")
	}
	text, ok := fields[oracle.expectedField].TextValue()
	if !ok || text != oracle.expectedText || request.Run.RunID != oracle.expectedRunID ||
		request.ConnectionID != oracle.expectedConnection ||
		request.Run.OperationOrPlanRef.Kind != contract.OperationReference ||
		request.Run.OperationOrPlanRef.ID != oracle.expectedOperation {
		return app.ExecutionObservation{}, errors.New("controlled effect received a different admitted intent")
	}
	oracle.evidence = parityEffectEvidence{
		RunID:        request.Run.RunID,
		ConnectionID: request.ConnectionID,
		OperationID:  request.Run.OperationOrPlanRef.ID,
		TaskID:       request.Task.TaskID,
		EffectKey:    request.Dispatch.EffectKey,
		DispatchID:   request.Dispatch.DispatchID,
		AttemptID:    request.Attempt.AttemptID,
		Text:         text,
	}
	observedAt := time.Now().UTC()
	outcomes := make([]contract.MemberOutcome, 0, len(request.Dispatch.OrderedMemberIDs))
	for _, memberID := range request.Dispatch.OrderedMemberIDs {
		outcomes = append(outcomes, contract.MemberOutcome{
			MemberID:      memberID,
			EffectKey:     request.Dispatch.EffectKey,
			DispatchID:    request.Dispatch.DispatchID,
			State:         contract.OutcomeSucceeded,
			ObservedAt:    observedAt,
			AssertionRefs: []string{},
		})
	}
	return app.ExecutionObservation{
		Result: &contract.ActionResult{
			MediaType: "application/json",
			Data:      contract.NewStringValue(text),
		},
		Outcomes: outcomes,
	}, nil
}

func (oracle *parityEffectOracle) snapshot() (parityEffectEvidence, int) {
	oracle.mu.Lock()
	defer oracle.mu.Unlock()
	return oracle.evidence, oracle.calls
}

type parityDurableCounts struct {
	Runs          int64
	RunSpecs      int64
	AdmissionKeys int64
	Inputs        int64
	StartCommands int64
	Effects       int64
}

type parityRunState struct {
	Record contract.RunRecord
	Input  store.RunInputSnapshot
}

type parityTemporalHarness struct {
	configuration config.Configuration
	worker        config.WorkerConfiguration
	namespace     string
	backend       sdkclient.Client
	relayClient   *pmtemporal.Client
	observer      contract.WorkflowContinuationObserver
}

type parityWorkerRun struct {
	execution contract.WorkflowExecution
	reference contract.WorkflowRunRef
	plan      testfixture.SealedWorkerPlan
	service   *app.WorkerService
}

type lockedToolAsset struct {
	ID         string `json:"id"`
	Version    string `json:"version"`
	Platform   string `json:"platform"`
	URL        string `json:"url"`
	SHA256     string `json:"sha256"`
	Kind       string `json:"kind"`
	Executable string `json:"executable"`
}

type toolchainLock struct {
	SchemaVersion string            `json:"schema_version"`
	Assets        []lockedToolAsset `json:"assets"`
}

type bootstrapMarker struct {
	SchemaVersion string `json:"schema_version"`
	Asset         struct {
		ID       string `json:"id"`
		Version  string `json:"version"`
		Platform string `json:"platform"`
		SHA256   string `json:"sha256"`
	} `json:"asset"`
	Executables map[string]string `json:"executables"`
}

type playwrightReport struct {
	Suites []playwrightSuite `json:"suites"`
	Errors []json.RawMessage `json:"errors"`
}

type playwrightSuite struct {
	Suites []playwrightSuite `json:"suites"`
	Specs  []playwrightSpec  `json:"specs"`
}

type playwrightSpec struct {
	Title string           `json:"title"`
	OK    bool             `json:"ok"`
	Tests []playwrightTest `json:"tests"`
}

type playwrightTest struct {
	ExpectedStatus string             `json:"expectedStatus"`
	Results        []playwrightResult `json:"results"`
}

type playwrightResult struct {
	Status string `json:"status"`
}

func TestCLIAndBrowserEquivalentUseCases(t *testing.T) {
	root := parityRepositoryRoot(t)
	fixtureSourceDocument := readDeclaredFile(t, filepath.Join(root, "tests/client-parity/fixtures.json"), parityInputLimit)
	fixtureSource := decodeParityFixtureSource(t, fixtureSourceDocument)
	var cursorKey [32]byte
	if _, err := rand.Read(cursorKey[:]); err != nil {
		t.Fatal("generate owned parity event cursor key")
	}

	counters := &parityCounters{}
	oracle := &parityEffectOracle{}
	handlers := &app.HandlerRegistry{}
	foundation := contract.FoundationRef{ID: "FND-6", Version: "1.0.0"}
	if err := handlers.RegisterHTTP(app.HandlerRegistration{
		Lanes: []contract.Lane{contract.LaneDirectRead},
		Foundations: []contract.FoundationContract{{
			ID: foundation.ID, Version: foundation.Version, Dependencies: []contract.FoundationRef{},
		}},
		Capabilities: []contract.CapabilityRequirement{{Capability: "http.execute", Foundation: foundation}},
		Execute: func(ctx context.Context, request app.ExecutionRequest) (app.ExecutionObservation, error) {
			counters.effects.Add(1)
			return oracle.execute(ctx, request)
		},
	}); err != nil {
		t.Fatalf("register controlled HTTP executor: %v", err)
	}

	var (
		authentication *auth.Service
		registry       *connectors.Registry
		resolver       app.OperationResolver
		bundle         connectors.Bundle
	)
	fixture := testfixture.NewOperation(t, fixtureSource.OperationID, func(
		dependencies testfixture.ServiceDependencies,
		service *httpapi.ServiceConfig,
	) error {
		var err error
		bundle, err = loadParityBundle(fixtureSourceDocument, fixtureSource, dependencies.Connection.ConnectorID)
		if err != nil {
			return err
		}
		registry, err = connectors.NewRegistry(bundle)
		if err != nil {
			return fmt.Errorf("construct controlled bundle registry: %w", err)
		}
		resolver = func(
			_ context.Context,
			scope contract.Scope,
			connectionID string,
			operationID string,
		) (app.OperationSelection, error) {
			primary := scope.Equal(dependencies.Scope) && connectionID == dependencies.Connection.ConnectionID &&
				dependencies.Connection.ConnectorID == bundle.ConnectorID
			foreign := scope.Equal(dependencies.ForeignScope) && connectionID == dependencies.ForeignConnection.ConnectionID &&
				dependencies.ForeignConnection.ConnectorID == bundle.ConnectorID
			if operationID != fixtureSource.OperationID || !primary && !foreign {
				return app.OperationSelection{}, errors.New("controlled bundled operation is unavailable in this scope")
			}
			counters.admissions.Add(1)
			return app.ResolveBundledOperation(registry, bundle.ConnectorID, operationID)
		}
		catalog, err := app.ServeCatalog(app.CatalogServiceConfig{
			Pool: dependencies.Pool, Authorize: service.Auth.AuthorizeResource,
			Registry: registry, Handlers: handlers,
		})
		if err != nil {
			return fmt.Errorf("construct shared catalog service: %w", err)
		}
		admission, err := app.NewAdmissionService(app.AdmissionServiceConfig{
			Pool: dependencies.Pool, DeploymentID: dependencies.DeploymentID,
			Authorize: service.Auth.AuthorizeResource, Registry: handlers,
			ResolveOperation: resolver, Policy: parityRunPolicy(),
		})
		if err != nil {
			return fmt.Errorf("construct shared admission service: %w", err)
		}
		authentication = service.Auth
		service.Catalog = catalog
		service.Admission = admission
		service.EventBufferSize = 2
		service.EventWriteTimeout = time.Second
		service.ResolveEventCursorKey = func(ctx context.Context) ([]byte, error) {
			if ctx == nil || ctx.Err() != nil {
				return nil, errors.New("event cursor key request is not live")
			}
			return bytes.Clone(cursorKey[:]), nil
		}
		return nil
	})
	if authentication == nil || registry == nil || resolver == nil || fixture.Connection.ConnectorID != bundle.ConnectorID {
		t.Fatal("shared bundle catalog, resolver, admission, and authentication were not installed")
	}
	fixture.WorkerRegistry = handlers
	fixture.ResolveOperation = resolver
	grantParityCatalogRead(t, fixture)

	selection, err := app.ResolveBundledOperation(registry, bundle.ConnectorID, fixtureSource.OperationID)
	if err != nil {
		t.Fatalf("resolve selected bundled operation: %v", err)
	}
	if selection.SchemaRefs == nil || len(selection.SchemaRefs) != 0 {
		t.Fatal("selected fixture operation did not preserve the explicit empty schema revision set")
	}
	operationRevision, err := app.OperationSelectionRevision(selection)
	if err != nil {
		t.Fatalf("derive selected operation revision: %v", err)
	}
	if operationRevision == fixtureSource.OperationRevision || bundle.SemanticsRevision == operationRevision {
		t.Fatal("source label, bundle semantics, and selected operation identity were conflated")
	}

	certificate, certificatePin := parityCertificate(t, fixture)
	cliSession := issueParityDeviceSession(t, authentication, fixture.Actor.RequestedActor)
	profilePath := filepath.Join(t.TempDir(), "profiles.json")
	sessionHandle, err := pmapi.DefaultSessionHandle(parityProfileName, fixture.DeploymentID)
	if err != nil {
		t.Fatalf("derive owned CLI session handle: %v", err)
	}
	if err := pmapi.AddServerProfile(profilePath, pmapi.ServerProfile{
		Name: parityProfileName, ServerURL: fixture.BaseURL,
		InstanceID: fixture.DeploymentID, SessionHandle: sessionHandle,
	}); err != nil {
		t.Fatalf("write owned CLI profile: %v", err)
	}
	cliBase := parityCLIInput{
		ProfilesPath: profilePath, InstanceID: fixture.DeploymentID,
		SessionHandle: sessionHandle, Certificate: append([]byte(nil), certificate.Raw...),
		Session: cliSession,
	}
	cliInvocationArgs := []string{
		"--server-profile", parityProfileName,
		"--workspace", fixture.Scope.WorkspaceID,
		"--connection", fixture.Connection.ConnectionID,
		"--output", "json",
		"--request-timeout", "10s",
		"--session-store", "memory",
		"fixture-echo",
		"--text", fixtureSource.FirstText,
		"--request-key", fixtureSource.RequestKey,
	}
	cliInput := cliBase
	cliInput.Args = cliInvocationArgs
	admission, err := pmapi.DecodeAdmissionResponse(runParityCLI(t, cliInput))
	if err != nil {
		t.Fatalf("decode generated CLI admission: %v", err)
	}
	if admission.Replay || string(admission.State) != "accepted" || admission.RunRef == "" ||
		admission.RequestKey != fixtureSource.RequestKey ||
		contract.SemanticDigest(admission.IntentDigest).Validate() != nil {
		t.Fatal("generated CLI did not return one fresh durable admission")
	}

	expectedInput := contract.NewObjectValue(map[string]contract.PresentValue{
		fixtureSource.InputField: contract.NewStringValue(fixtureSource.FirstText),
	})
	initialState := loadParityRunState(t, fixture, admission.RunRef)
	assertParityAcceptedState(t, fixture, fixtureSource, operationRevision, admission, expectedInput, initialState)
	initialCounts := parityDurableStateCounts(t, fixture, fixture.Scope, fixtureSource.RequestKey)
	if initialCounts != (parityDurableCounts{Runs: 1, RunSpecs: 1, AdmissionKeys: 1, Inputs: 1, StartCommands: 1, Effects: 0}) {
		t.Fatalf("CLI durable allocation counts = %#v; want one run/spec/key/input/start and zero effects", initialCounts)
	}
	foreignCounts := parityDurableStateCounts(t, fixture, fixture.ForeignScope, fixtureSource.RequestKey)
	if foreignCounts != (parityDurableCounts{}) {
		t.Fatalf("foreign scope was mutated by the CLI admission: %#v", foreignCounts)
	}
	if err := oracle.bind(
		admission.RunRef,
		fixture.Connection.ConnectionID,
		fixtureSource.OperationID,
		fixtureSource.InputField,
		fixtureSource.FirstText,
	); err != nil {
		t.Fatalf("bind controlled effect oracle: %v", err)
	}

	bootstrapNonce := installBrowserSurface(t, fixture, authentication, root, counters)
	invocationInput := map[string]any{
		"connection_ref":  fixture.Connection.ConnectionID,
		"input":           expectedInput,
		"lane":            string(contract.LaneDirectRead),
		"operation_ref":   fixtureSource.OperationID,
		"result_controls": contract.ResultControls{},
	}
	browserFixture := browserInput{
		BaseURL: fixture.BaseURL, BootstrapNonce: bootstrapNonce,
		InstanceID: fixture.DeploymentID, WorkspaceID: fixture.Scope.WorkspaceID,
		ForeignWorkspaceID: fixture.ForeignScope.WorkspaceID,
		OperationRevision:  operationRevision, RequestKey: fixtureSource.RequestKey,
		InputField: fixtureSource.InputField, Input: invocationInput,
		FirstText: fixtureSource.FirstText, ChangedText: fixtureSource.ChangedText,
		ExpectedRunRef: admission.RunRef, ExpectedIntentDigest: admission.IntentDigest,
		CanonicalCases: parityCanonicalCases(t),
	}
	browserInputPath := writeParityBrowserInput(t, browserFixture)
	runPinnedPlaywright(t, root, browserInputPath, certificatePin)

	afterBrowser := loadParityRunState(t, fixture, admission.RunRef)
	if !reflect.DeepEqual(afterBrowser.Record.Spec, initialState.Record.Spec) ||
		!afterBrowser.Input.Input.Equal(expectedInput) ||
		afterBrowser.Record.CancelRequested || afterBrowser.Record.State != contract.RunStateAccepted ||
		len(afterBrowser.Record.WorkflowBinding.Observations) != 0 {
		t.Fatal("browser replay, conflict, denial, or view abort changed the original accepted RunSpec")
	}
	afterBrowserCounts := parityDurableStateCounts(t, fixture, fixture.Scope, fixtureSource.RequestKey)
	if afterBrowserCounts != initialCounts || counters.effects.Load() != 0 {
		t.Fatalf("browser cases created another durable run or downstream effect: before=%#v after=%#v effects=%d", initialCounts, afterBrowserCounts, counters.effects.Load())
	}
	foreignCounts = parityDurableStateCounts(t, fixture, fixture.ForeignScope, fixtureSource.RequestKey)
	if foreignCounts != (parityDurableCounts{}) {
		t.Fatalf("browser authority cases mutated the foreign scope: %#v", foreignCounts)
	}

	inspectInput := cliBase
	inspectInput.Args = []string{
		"--server-profile", parityProfileName,
		"--workspace", fixture.Scope.WorkspaceID,
		"--output", "json",
		"--request-timeout", "10s",
		"--session-store", "memory",
		"runs", "inspect", admission.RunRef,
	}
	var inspected pmapi.RunView
	if err := json.Unmarshal(runParityCLI(t, inspectInput), &inspected); err != nil {
		t.Fatalf("decode generated CLI run view: %v", err)
	}
	if inspected.RunRef != admission.RunRef || string(inspected.State) != "accepted" || inspected.CancelRequested {
		t.Fatal("generated CLI did not observe the accepted run after the browser view abort")
	}

	temporal := newParityTemporalHarness(t)
	workerRun := prepareParityWorkerRun(t, fixture, temporal, handlers, resolver, admission.RunRef)
	projection := executeParityWorkerRun(t, temporal, workerRun)
	if projection.RunID != admission.RunRef || projection.State != contract.RunStateSucceeded {
		t.Fatalf("real Temporal workflow projection = %#v; want the original succeeded run", projection)
	}
	assertParityExecution(t, fixture, fixtureSource, admission, initialState, expectedInput, workerRun, oracle, counters)
	t.Run("generated_cli_allocates_and_persists_request_key", func(t *testing.T) {
		exerciseGeneratedCLIAllocatedKey(t, fixtureSourceDocument, fixtureSource)
	})
}

func exerciseGeneratedCLIAllocatedKey(
	t *testing.T,
	sourceDocument []byte,
	source parityFixtureSource,
) {
	t.Helper()
	var effectCalls, invocationCalls atomic.Int64
	handlers := &app.HandlerRegistry{}
	foundation := contract.FoundationRef{ID: "FND-6", Version: "1.0.0"}
	if err := handlers.RegisterHTTP(app.HandlerRegistration{
		Lanes: []contract.Lane{contract.LaneDirectRead},
		Foundations: []contract.FoundationContract{{
			ID: foundation.ID, Version: foundation.Version, Dependencies: []contract.FoundationRef{},
		}},
		Capabilities: []contract.CapabilityRequirement{{Capability: "http.execute", Foundation: foundation}},
		Execute: func(context.Context, app.ExecutionRequest) (app.ExecutionObservation, error) {
			effectCalls.Add(1)
			return app.ExecutionObservation{}, errors.New("allocated-key smoke must not execute a provider effect")
		},
	}); err != nil {
		t.Fatalf("register allocated-key smoke handler: %v", err)
	}

	var (
		authentication *auth.Service
		registry       *connectors.Registry
		resolver       app.OperationResolver
		bundle         connectors.Bundle
	)
	fixture := testfixture.NewOperation(t, source.OperationID, func(
		dependencies testfixture.ServiceDependencies,
		service *httpapi.ServiceConfig,
	) error {
		var err error
		bundle, err = loadParityBundle(sourceDocument, source, dependencies.Connection.ConnectorID)
		if err != nil {
			return err
		}
		registry, err = connectors.NewRegistry(bundle)
		if err != nil {
			return fmt.Errorf("construct allocated-key bundle registry: %w", err)
		}
		resolver = func(
			_ context.Context,
			scope contract.Scope,
			connectionID string,
			operationID string,
		) (app.OperationSelection, error) {
			if !scope.Equal(dependencies.Scope) ||
				connectionID != dependencies.Connection.ConnectionID ||
				dependencies.Connection.ConnectorID != bundle.ConnectorID ||
				operationID != source.OperationID {
				return app.OperationSelection{}, errors.New("allocated-key bundled operation is unavailable in this scope")
			}
			return app.ResolveBundledOperation(registry, bundle.ConnectorID, operationID)
		}
		catalog, err := app.ServeCatalog(app.CatalogServiceConfig{
			Pool: dependencies.Pool, Authorize: service.Auth.AuthorizeResource,
			Registry: registry, Handlers: handlers,
		})
		if err != nil {
			return fmt.Errorf("construct allocated-key catalog service: %w", err)
		}
		authorizeAdmission := func(
			ctx context.Context,
			tx *store.Transaction,
			request auth.AuthorizationRequest,
		) (auth.Authorization, error) {
			invocationCalls.Add(1)
			return service.Auth.AuthorizeResource(ctx, tx, request)
		}
		admission, err := app.NewAdmissionService(app.AdmissionServiceConfig{
			Pool: dependencies.Pool, DeploymentID: dependencies.DeploymentID,
			Authorize: authorizeAdmission, Registry: handlers,
			ResolveOperation: resolver, Policy: parityRunPolicy(),
		})
		if err != nil {
			return fmt.Errorf("construct allocated-key admission service: %w", err)
		}
		authentication = service.Auth
		service.Catalog = catalog
		service.Admission = admission
		return nil
	})
	if authentication == nil || registry == nil || resolver == nil {
		t.Fatal("allocated-key smoke did not install its isolated runtime services")
	}
	grantParityCatalogRead(t, fixture)
	selection, err := app.ResolveBundledOperation(registry, bundle.ConnectorID, source.OperationID)
	if err != nil {
		t.Fatalf("resolve allocated-key smoke operation: %v", err)
	}
	operationRevision, err := app.OperationSelectionRevision(selection)
	if err != nil {
		t.Fatalf("derive allocated-key smoke operation revision: %v", err)
	}

	certificate, _ := parityCertificate(t, fixture)
	session := issueParityDeviceSession(t, authentication, fixture.Actor.RequestedActor)
	clientRoot := t.TempDir()
	profilePath := filepath.Join(clientRoot, "profiles.json")
	sessionHandle, err := pmapi.DefaultSessionHandle("allocated-key", fixture.DeploymentID)
	if err != nil {
		t.Fatalf("derive allocated-key session handle: %v", err)
	}
	if err := pmapi.AddServerProfile(profilePath, pmapi.ServerProfile{
		Name: "allocated-key", ServerURL: fixture.BaseURL,
		InstanceID: fixture.DeploymentID, SessionHandle: sessionHandle,
	}); err != nil {
		t.Fatalf("write allocated-key CLI profile: %v", err)
	}
	args := []string{
		"--server-profile", "allocated-key",
		"--workspace", fixture.Scope.WorkspaceID,
		"--connection", fixture.Connection.ConnectionID,
		"--output", "json",
		"--request-timeout", "10s",
		"--session-store", "memory",
		"fixture-echo",
		"--text", source.FirstText,
	}
	if slices.Contains(args, "--request-key") {
		t.Fatal("allocated-key CLI smoke unexpectedly selected an explicit replay key")
	}
	cliBase := parityCLIInput{
		ProfilesPath: profilePath, InstanceID: fixture.DeploymentID,
		SessionHandle: sessionHandle, Certificate: append([]byte(nil), certificate.Raw...),
		Session: session,
	}
	firstInput := cliBase
	firstInput.Args = args
	output := runParityCLI(t, firstInput)
	admission, err := pmapi.DecodeAdmissionResponse(output)
	if err != nil {
		t.Fatalf("decode allocated-key generated CLI admission: %v", err)
	}
	if admission.Replay || string(admission.State) != "accepted" ||
		!strings.HasPrefix(admission.RequestKey, "pm_") ||
		admission.RequestKey == source.RequestKey ||
		len(admission.RequestKey) > 200 {
		t.Fatal("generated CLI did not return one fresh client-allocated request key")
	}
	expectedInput := contract.NewObjectValue(map[string]contract.PresentValue{
		source.InputField: contract.NewStringValue(source.FirstText),
	})
	state := loadParityRunState(t, fixture, admission.RunRef)
	assertParityAcceptedState(t, fixture, source, operationRevision, admission, expectedInput, state)
	counts := parityDurableStateCounts(t, fixture, fixture.Scope, admission.RequestKey)
	if counts != (parityDurableCounts{Runs: 1, RunSpecs: 1, AdmissionKeys: 1, Inputs: 1, StartCommands: 1, Effects: 0}) ||
		effectCalls.Load() != 0 || invocationCalls.Load() != 1 {
		t.Fatalf("allocated-key smoke durable state = %#v effects=%d invocations=%d; want one unexecuted isolated run", counts, effectCalls.Load(), invocationCalls.Load())
	}

	recoveryDirectory, err := pmapi.DefaultRecoveryDirectory(profilePath)
	if err != nil {
		t.Fatalf("resolve allocated-key recovery directory: %v", err)
	}
	directoryInformation, err := os.Lstat(recoveryDirectory)
	if err != nil || !directoryInformation.IsDir() || directoryInformation.Mode()&os.ModeSymlink != 0 ||
		directoryInformation.Mode().Perm()&0o077 != 0 {
		t.Fatal("generated CLI did not create one private recovery directory")
	}
	entries, err := os.ReadDir(recoveryDirectory)
	if err != nil || len(entries) != 1 {
		t.Fatal("generated CLI did not persist exactly one isolated recovery record")
	}
	entry := entries[0]
	if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
		t.Fatal("generated CLI recovery record is not an owned regular file")
	}
	recoveryPath := filepath.Join(recoveryDirectory, entry.Name())
	recoveryInformation := requireRegularFile(t, recoveryPath, 2048)
	if recoveryInformation.Mode().Perm()&0o077 != 0 {
		t.Fatal("generated CLI recovery record is not private")
	}
	recoveryDocument := readBoundedFile(t, recoveryPath, 2048)
	if bytes.Contains(recoveryDocument, []byte(source.FirstText)) ||
		bytes.Contains(recoveryDocument, []byte(source.ChangedText)) ||
		bytes.Contains(recoveryDocument, []byte(session.PmSessionCredential)) {
		t.Fatal("generated CLI recovery record contains plaintext input or session material")
	}

	replayArgs := append(slices.Clone(args), "--request-key", admission.RequestKey)
	replayInput := cliBase
	replayInput.Args = replayArgs
	replayOutput := runParityCLI(t, replayInput)
	replay, err := pmapi.DecodeAdmissionResponse(replayOutput)
	if err != nil {
		t.Fatalf("decode allocated-key generated CLI replay: %v", err)
	}
	if !replay.Replay || string(replay.State) != "accepted" ||
		replay.RequestKey != admission.RequestKey ||
		replay.RunRef != admission.RunRef ||
		replay.IntentDigest != admission.IntentDigest {
		t.Fatal("fresh generated CLI process did not resume the allocated request key")
	}
	replayState := loadParityRunState(t, fixture, replay.RunRef)
	assertParityAcceptedState(t, fixture, source, operationRevision, replay, expectedInput, replayState)
	if replayCounts := parityDurableStateCounts(t, fixture, fixture.Scope, admission.RequestKey); replayCounts != counts ||
		effectCalls.Load() != 0 || invocationCalls.Load() != 2 {
		t.Fatalf("allocated-key replay changed durable state: %#v effects=%d invocations=%d", replayCounts, effectCalls.Load(), invocationCalls.Load())
	}

	changedArgs := slices.Clone(replayArgs)
	changedTextSelected := false
	for index := 1; index < len(changedArgs); index++ {
		if changedArgs[index-1] == "--text" {
			changedArgs[index] = source.ChangedText
			changedTextSelected = true
			break
		}
	}
	if !changedTextSelected {
		t.Fatal("allocated-key conflict smoke could not select its changed intent")
	}

	invocationsBeforeConflict := invocationCalls.Load()
	changedInput := cliBase
	changedInput.Args = changedArgs
	conflict := runParityCLIProcess(t, changedInput)
	var exitError *exec.ExitError
	if !errors.As(conflict.err, &exitError) || exitError.ExitCode() != 4 ||
		len(conflict.stdout) != 0 || len(conflict.stderr) == 0 {
		t.Fatalf(
			"changed allocated-key intent result = err %v stdout %q stderr %q; want local recovery conflict exit 4",
			conflict.err,
			conflict.stdout,
			conflict.stderr,
		)
	}
	if bytes.Contains(conflict.stderr, []byte(source.FirstText)) ||
		bytes.Contains(conflict.stderr, []byte(source.ChangedText)) ||
		bytes.Contains(conflict.stderr, []byte(session.PmSessionCredential)) {
		t.Fatal("allocated-key recovery conflict diagnostic contains plaintext input or session material")
	}
	if effectCalls.Load() != 0 || invocationCalls.Load() != invocationsBeforeConflict {
		t.Fatalf(
			"allocated-key recovery conflict crossed the local boundary: effects=%d invocations=%d before=%d",
			effectCalls.Load(),
			invocationCalls.Load(),
			invocationsBeforeConflict,
		)
	}
	if conflictCounts := parityDurableStateCounts(t, fixture, fixture.Scope, admission.RequestKey); conflictCounts != counts {
		t.Fatalf("allocated-key recovery conflict changed durable state: %#v", conflictCounts)
	}
	if afterConflict := readBoundedFile(t, recoveryPath, 2048); !bytes.Equal(afterConflict, recoveryDocument) {
		t.Fatal("allocated-key recovery conflict changed the persisted recovery record")
	}
}

func decodeParityFixtureSource(t *testing.T, document []byte) parityFixtureSource {
	t.Helper()
	var source parityFixtureSource
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&source); err != nil || decoder.Decode(new(any)) != io.EOF {
		t.Fatal("decode strict client parity fixture source")
	}
	if source.SchemaVersion != "pm.client-parity-fixture/v1" || source.OperationID != "fixture.echo" ||
		source.OperationRevision != "r1" || source.InputField != "text" ||
		source.FirstText != "alpha-one" || source.ChangedText != "alpha-two" ||
		source.RequestKey != "parity-1" || source.EvidenceMode != "controlled_same_stack" {
		t.Fatal("client parity fixture source does not match its accepted operation inventory")
	}
	return source
}

func loadParityBundle(sourceDocument []byte, source parityFixtureSource, connectorID string) (connectors.Bundle, error) {
	semanticsDocument, err := json.Marshal(struct {
		SchemaVersion string `json:"schema_version"`
		OperationID   string `json:"operation_id"`
		Revision      string `json:"revision"`
	}{
		SchemaVersion: source.SchemaVersion,
		OperationID:   source.OperationID,
		Revision:      source.OperationRevision,
	})
	if err != nil {
		return connectors.Bundle{}, fmt.Errorf("encode fixture semantics identity: %w", err)
	}
	semanticsRevision, err := contract.CanonicalDigest(semanticsDocument)
	if err != nil {
		return connectors.Bundle{}, fmt.Errorf("derive fixture semantics identity: %w", err)
	}
	foundation := contract.FoundationRef{ID: "FND-6", Version: "1.0.0"}
	sourceRefs := []contract.SourceRef{{
		SourceID: "client-parity-fixture-r1", Pointer: "#/operation_id",
	}}
	bundleDocument, err := json.Marshal(connectors.Bundle{
		SchemaVersion:     connectors.BundleSchemaVersion,
		ConnectorID:       connectorID,
		SourceDigest:      string(contract.RawSourceHash(sourceDocument)),
		SemanticsRevision: string(semanticsRevision),
		Operations: []contract.Operation{{
			SchemaVersion: contract.OperationSchemaVersion,
			OperationID:   source.OperationID,
			SourceRefs:    sourceRefs,
			Transport:     contract.Transport{Kind: contract.TransportKindHTTP, Method: http.MethodGet, Path: "/fixture/echo"},
			Inputs: []contract.FieldBinding{{
				FieldID: source.InputField, WireName: source.InputField, Location: contract.FieldLocationQuery,
				JSONPointer: "", Required: true, Nullable: false, Style: "form", Explode: true,
				AllowReserved: false, Sensitivity: contract.SensitivityPublic,
				Schema: contract.Schema{Type: contract.SchemaTypeString},
			}},
			RequestVariants: []contract.RequestVariant{},
			Responses: []contract.Response{{
				Status: "200", NoContent: false, MediaType: "application/json",
				Schema: &contract.Schema{Type: contract.SchemaTypeString}, SourceRefs: sourceRefs,
			}},
			CapabilityRequirements: []contract.CapabilityRequirement{{Capability: "http.execute", Foundation: foundation}},
			LaneEligibility:        parityLaneEligibility(),
		}},
		Commands: []contract.Command{{
			Name: "fixture-echo", OperationID: source.OperationID,
			Fields: []contract.CommandField{{
				Flag: "--text", FieldID: source.InputField, Location: contract.FieldLocationQuery,
				Required: true, Sensitive: false,
			}},
		}},
		UnavailableOperations: []contract.UnavailableOperation{},
	})
	if err != nil {
		return connectors.Bundle{}, fmt.Errorf("encode controlled bundle: %w", err)
	}
	bundle, err := connectors.LoadBundle(bytes.NewReader(bundleDocument))
	if err != nil {
		return connectors.Bundle{}, fmt.Errorf("load controlled bundle through runtime ABI: %w", err)
	}
	return bundle, nil
}

func parityLaneEligibility() []contract.LaneEligibility {
	lanes := []contract.Lane{
		contract.LaneDirectRead,
		contract.LaneDirectWrite,
		contract.LaneETL,
		contract.LaneReverseETL,
		contract.LaneBinaryDownload,
		contract.LaneBinaryUpload,
		contract.LaneSyncTransport,
	}
	eligibility := make([]contract.LaneEligibility, 0, len(lanes))
	for _, lane := range lanes {
		status := contract.LaneUnsupported
		reason := "fixture-lane-unsupported"
		if lane == contract.LaneDirectRead {
			status = contract.LaneEligible
			reason = "fixture-handler-registered"
		}
		eligibility = append(eligibility, contract.LaneEligibility{Lane: lane, Status: status, Reason: reason})
	}
	return eligibility
}

func parityRunPolicy() store.RunBudgetPolicy {
	return store.RunBudgetPolicy{
		Pending: store.AdmissionLimits{
			GlobalPending: 3, WorkspacePending: 2, WorkspaceMaxPendingAge: 30 * time.Second,
		},
		RunLifetime: time.Hour, InputMaxBytes: 1 << 20, WorkspaceActiveRuns: 4,
		WorkflowPayloadBytes: 1 << 16, WorkflowFanout: 32,
	}
}

func grantParityCatalogRead(t *testing.T, fixture *testfixture.Fixture) {
	t.Helper()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE pm.memberships
			SET permissions = ARRAY(
			        SELECT DISTINCT value
			        FROM unnest(permissions || ARRAY['catalog.read']) AS value
			        ORDER BY value
			    ),
			    resource_grants = ARRAY(
			        SELECT DISTINCT value
			        FROM unnest(resource_grants || ARRAY['permission:catalog.read']) AS value
			        ORDER BY value
			    ),
			    policy_epoch = policy_epoch + 1,
			    control_revision = control_revision + 1
			WHERE identity_id = $1
			  AND NOT permissions @> ARRAY['catalog.read']::text[]
		`, fixture.Actor.RequestedActor.PrincipalID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("catalog permission fixture update did not affect one membership")
		}
		return nil
	})
}

func issueParityDeviceSession(t *testing.T, authentication *auth.Service, principal contract.Principal) pmapi.PMDeviceSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	session, err := authentication.IssuePMSession(ctx, auth.SessionIssue{
		Principal: principal, Transport: store.SessionTransportDevice,
	})
	if err != nil {
		t.Fatalf("issue persisted CLI device session: %v", err)
	}
	return pmapi.PMDeviceSession{
		ExpiresAt:           session.ExpiresAt,
		PmSessionCredential: session.Token,
		Principal: pmapi.Principal{
			Kind: pmapi.PrincipalKind(session.Principal.Kind), PrincipalId: session.Principal.PrincipalID,
		},
		SessionId: session.SessionID,
	}
}

func assertParityAcceptedState(
	t *testing.T,
	fixture *testfixture.Fixture,
	source parityFixtureSource,
	operationRevision string,
	admission pmapi.AdmissionResponse,
	expectedInput contract.PresentValue,
	state parityRunState,
) {
	t.Helper()
	spec := state.Record.Spec
	if spec.RunID != admission.RunRef || spec.OrgID != fixture.Scope.OrgID || spec.WorkspaceID != fixture.Scope.WorkspaceID ||
		!reflect.DeepEqual(spec.Actor, fixture.Actor) || spec.Origin != contract.RunOriginAPI ||
		spec.OperationOrPlanRef.Kind != contract.OperationReference ||
		spec.OperationOrPlanRef.ID != source.OperationID ||
		spec.OperationOrPlanRef.Revision != operationRevision ||
		spec.SchemaRefs == nil || len(spec.SchemaRefs) != 0 || len(spec.InputRefs) != 1 ||
		string(spec.IdempotencyFingerprint) != admission.IntentDigest {
		t.Fatal("CLI admission did not pin the exact selected operation, actor, input and idempotency identity")
	}
	if state.Record.State != contract.RunStateAccepted || state.Record.CancelRequested ||
		state.Record.WorkflowBinding.RunID != admission.RunRef ||
		state.Record.WorkflowBinding.Origin != contract.RunOriginAPI ||
		state.Record.WorkflowBinding.WorkflowID != "pm/run/"+admission.RunRef ||
		state.Record.WorkflowBinding.Generation != 1 ||
		len(state.Record.WorkflowBinding.Observations) != 0 {
		t.Fatal("fresh CLI admission did not remain an unrelayed accepted API run")
	}
	if !state.Input.Scope.Equal(fixture.Scope) || state.Input.RunID != admission.RunRef ||
		state.Input.InputRef != spec.InputRefs[0] || state.Input.ConnectionID != fixture.Connection.ConnectionID ||
		state.Input.Lane != contract.LaneDirectRead || !state.Input.Input.Equal(expectedInput) ||
		!reflect.DeepEqual(state.Input.ResultControls, contract.ResultControls{}) {
		t.Fatal("CLI admission did not persist the exact typed invocation input")
	}
}

func loadParityRunState(t *testing.T, fixture *testfixture.Fixture, runID string) parityRunState {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var state parityRunState
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(
		ctx context.Context,
		tx *store.Transaction,
	) error {
		var err error
		state.Record, err = store.LoadRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		state.Input, err = store.LoadRunInputSnapshot(ctx, tx, runID)
		return err
	}); err != nil {
		t.Fatalf("load exact parity run state: %v", err)
	}
	return state
}

func parityDurableStateCounts(
	t *testing.T,
	fixture *testfixture.Fixture,
	scope contract.Scope,
	requestKey string,
) parityDurableCounts {
	t.Helper()
	var counts parityDurableCounts
	fixture.SetupSQL(t, scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT
				(SELECT count(*) FROM pm.runs WHERE org_id = $1 AND workspace_id = $2),
				(SELECT count(*) FROM pm.run_specs WHERE org_id = $1 AND workspace_id = $2),
				(SELECT count(*) FROM pm.admission_keys WHERE org_id = $1 AND workspace_id = $2 AND idempotency_key = $3),
				(SELECT count(*) FROM pm.run_input_snapshots WHERE org_id = $1 AND workspace_id = $2),
				(SELECT count(*) FROM pm.outbox_commands WHERE org_id = $1 AND workspace_id = $2 AND kind = 'start_run'),
				(SELECT count(*) FROM pm.effects WHERE org_id = $1 AND workspace_id = $2)
		`, scope.OrgID, scope.WorkspaceID, requestKey).Scan(
			&counts.Runs,
			&counts.RunSpecs,
			&counts.AdmissionKeys,
			&counts.Inputs,
			&counts.StartCommands,
			&counts.Effects,
		)
	})
	return counts
}

func parityCanonicalCases(t *testing.T) []browserCanonicalCase {
	t.Helper()
	values := []any{
		map[string]any{
			"z":      int64(0),
			"a":      "<&\u2028",
			"nested": []any{false, nil, int64(9007199254740991)},
		},
		[]any{map[string]any{"β": "two", "a": "one"}, int64(-1000)},
		map[string]any{"empty": "", "nullable": nil, "truth": true},
	}
	cases := make([]browserCanonicalCase, 0, len(values))
	for _, value := range values {
		document, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("encode canonical parity case: %v", err)
		}
		digest, err := contract.CanonicalDigest(document)
		if err != nil {
			t.Fatalf("derive canonical parity digest: %v", err)
		}
		cases = append(cases, browserCanonicalCase{Value: value, Digest: string(digest)})
	}
	return cases
}

func writeParityBrowserInput(t *testing.T, input browserInput) string {
	t.Helper()
	document, err := json.Marshal(input)
	if err != nil || len(document) == 0 || len(document) > parityInputLimit {
		t.Fatal("encode bounded browser parity input")
	}
	path := filepath.Join(t.TempDir(), "parity-input.json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal("create private browser parity input")
	}
	if written, writeErr := file.Write(document); writeErr != nil || written != len(document) {
		_ = file.Close()
		t.Fatal("write complete private browser parity input")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		t.Fatal("sync private browser parity input")
	}
	if err := file.Close(); err != nil {
		t.Fatal("close private browser parity input")
	}
	information, err := os.Lstat(path)
	if err != nil || !information.Mode().IsRegular() || information.Mode()&0o077 != 0 || information.Size() != int64(len(document)) {
		t.Fatal("private browser parity input identity changed")
	}
	return path
}

func materializePlaywrightTest(t *testing.T, root string) string {
	t.Helper()
	source := readDeclaredFile(
		t,
		filepath.Join(root, "tests/client-parity/browser.ts"),
		parityInputLimit,
	)
	testDirectory := filepath.Join(t.TempDir(), "playwright-tests")
	if err := os.Mkdir(testDirectory, 0o700); err != nil {
		t.Fatal("create private materialized Playwright test directory")
	}
	testPath := filepath.Join(testDirectory, "browser.ts")
	file, err := os.OpenFile(testPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal("create private materialized Playwright test")
	}
	if written, writeErr := file.Write(source); writeErr != nil || written != len(source) {
		_ = file.Close()
		t.Fatal("write complete private materialized Playwright test")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		t.Fatal("sync private materialized Playwright test")
	}
	if err := file.Close(); err != nil {
		t.Fatal("close private materialized Playwright test")
	}
	information, err := os.Lstat(testPath)
	if err != nil || !information.Mode().IsRegular() || information.Mode()&0o077 != 0 ||
		information.Size() != int64(len(source)) {
		t.Fatal("private materialized Playwright test identity changed")
	}

	declaredModules := filepath.Join(root, "tests/client-parity/node_modules")
	moduleInformation, err := os.Stat(declaredModules)
	if err != nil || !moduleInformation.IsDir() {
		t.Fatal("declared Playwright package tree is unavailable")
	}
	moduleLink := filepath.Join(testDirectory, "node_modules")
	if err := os.Symlink(declaredModules, moduleLink); err != nil {
		t.Fatal("link declared Playwright packages into the private test directory")
	}
	linkInformation, err := os.Lstat(moduleLink)
	if err != nil || linkInformation.Mode()&os.ModeSymlink == 0 {
		t.Fatal("private Playwright package link identity changed")
	}
	return testDirectory
}

func runPinnedPlaywright(t *testing.T, root, inputPath, certificatePin string) {
	t.Helper()
	validateInstalledParityPackages(t, root)
	nodePath := pinnedToolExecutable(t, root, "node")
	browserPath := pinnedToolExecutable(t, root, "chromium")
	parityDirectory := filepath.Join(root, "tests/client-parity")
	playwrightCLI := filepath.Join(parityDirectory, "node_modules/@playwright/test/cli.js")
	playwrightConfig := filepath.Join(parityDirectory, "playwright.config.ts")
	requireDeclaredRegularFile(t, playwrightCLI, 8<<20)
	requireDeclaredRegularFile(t, playwrightConfig, 1<<20)
	testDirectory := materializePlaywrightTest(t, root)
	var privateRoot string
	if artifacts := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR"); artifacts != "" {
		if !filepath.IsAbs(artifacts) {
			t.Fatal("browser parity artifact directory must be absolute")
		}
		var err error
		privateRoot, err = os.MkdirTemp(artifacts, "pm-client-parity-")
		if err != nil {
			t.Fatalf("create private retained browser parity artifacts: %v", err)
		}
		t.Logf("retained browser parity artifacts: %s", privateRoot)
	} else {
		privateRoot = t.TempDir()
	}
	outputDirectory := filepath.Join(privateRoot, "playwright-output")
	if err := os.Mkdir(outputDirectory, 0o700); err != nil {
		t.Fatal("create private Playwright output directory")
	}
	reportPath := filepath.Join(privateRoot, "playwright-report.json")
	report, err := os.OpenFile(reportPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal("create private Playwright report")
	}
	if err := report.Close(); err != nil {
		t.Fatal("close private Playwright report")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, nodePath, playwrightCLI, "test", "--config", playwrightConfig)
	command.Dir = parityDirectory
	command.Env = parityProcessEnvironment(map[string]string{
		"PM_BROWSER_EXECUTABLE":            browserPath,
		"PM_PARITY_CERTIFICATE_SPKI":       certificatePin,
		"PM_PARITY_INPUT":                  inputPath,
		"PM_PARITY_TEST_DIR":               testDirectory,
		"PM_PARITY_OUTPUT_DIR":             outputDirectory,
		"PM_PARITY_REPORT":                 reportPath,
		"PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD": "1",
	})
	var stdout, stderr parityOutput
	command.Stdout = &stdout
	command.Stderr = &stderr
	runErr := command.Run()
	if ctx.Err() != nil {
		t.Fatalf("pinned Playwright exceeded its owned time bound: %v", ctx.Err())
	}
	if runErr != nil {
		t.Fatalf("pinned Playwright failed: %v (stdout=%d bytes, stderr=%d bytes)", runErr, stdout.Len(), stderr.Len())
	}
	validatePlaywrightReport(t, reportPath)
}

func parityProcessEnvironment(overrides map[string]string) []string {
	environment := make(map[string]string)
	for _, key := range []string{"HOME", "LANG", "LC_ALL", "PATH", "TEMP", "TMP", "TMPDIR", "TZ"} {
		if value, present := os.LookupEnv(key); present {
			environment[key] = value
		}
	}
	for key, value := range overrides {
		environment[key] = value
	}
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+environment[key])
	}
	return result
}

func validateInstalledParityPackages(t *testing.T, root string) {
	t.Helper()
	lockPath := filepath.Join(root, "tests/client-parity/package-lock.json")
	document := readDeclaredFile(t, lockPath, 2<<20)
	var lock struct {
		LockfileVersion int `json:"lockfileVersion"`
		Packages        map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(document, &lock); err != nil || lock.LockfileVersion != 3 {
		t.Fatal("decode pinned parity package lock")
	}
	expected := map[string]string{
		"@playwright/test": "1.63.0",
		"playwright":       "1.63.0",
		"playwright-core":  "1.63.0",
		"typescript":       "5.9.3",
		"@types/node":      "24.0.0",
	}
	for name, version := range expected {
		locked, present := lock.Packages["node_modules/"+name]
		if !present || locked.Version != version {
			t.Fatalf("parity package %s is not locked to %s", name, version)
		}
		packagePath := filepath.Join(root, "tests/client-parity/node_modules", filepath.FromSlash(name), "package.json")
		packageDocument := readDeclaredFile(t, packagePath, 1<<20)
		var installed struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(packageDocument, &installed); err != nil || installed.Version != version {
			t.Fatalf("installed parity package %s does not match its lock", name)
		}
	}
}

func pinnedToolExecutable(t *testing.T, root, toolID string) string {
	t.Helper()
	lockDocument := readDeclaredFile(t, filepath.Join(root, "build/toolchains.lock.json"), 2<<20)
	var lock toolchainLock
	if err := json.Unmarshal(lockDocument, &lock); err != nil || lock.SchemaVersion != "pm.toolchains/v1" {
		t.Fatal("decode exact toolchain lock")
	}
	platform := runtime.GOOS + "-" + runtime.GOARCH
	var selected *lockedToolAsset
	for index := range lock.Assets {
		asset := &lock.Assets[index]
		if asset.ID == toolID && asset.Platform == platform {
			if selected != nil {
				t.Fatalf("toolchain lock has duplicate %s assets for %s", toolID, platform)
			}
			selected = asset
		}
	}
	if selected == nil || selected.Version == "" || selected.SHA256 == "" || selected.Executable == "" {
		t.Fatalf("toolchain lock has no %s asset for %s", toolID, platform)
	}
	cacheRoot := os.Getenv("PM_TOOLS_CACHE")
	if cacheRoot == "" {
		cacheRoot = filepath.Join(root, ".cache/pm-tools")
	} else if !filepath.IsAbs(cacheRoot) {
		t.Fatal("PM_TOOLS_CACHE must identify one absolute prebootstrapped cache")
	}
	installRoot := filepath.Join(cacheRoot, "tools", selected.ID, selected.Version, selected.Platform, selected.SHA256)
	markerPath := filepath.Join(installRoot, ".pm-bootstrap.json")
	markerDocument := readBoundedFile(t, markerPath, 64<<10)
	var marker bootstrapMarker
	markerDecoder := json.NewDecoder(bytes.NewReader(markerDocument))
	markerDecoder.DisallowUnknownFields()
	if err := markerDecoder.Decode(&marker); err != nil || markerDecoder.Decode(new(any)) != io.EOF ||
		marker.SchemaVersion != "pm.bootstrap-cache/v1" ||
		marker.Asset.ID != selected.ID || marker.Asset.Version != selected.Version ||
		marker.Asset.Platform != selected.Platform || marker.Asset.SHA256 != selected.SHA256 ||
		len(marker.Executables) == 0 {
		t.Fatalf("prebootstrapped %s cache marker does not authenticate the selected lock asset", toolID)
	}
	executableDigest, present := marker.Executables[selected.Executable]
	if !present || len(executableDigest) != sha256.Size*2 {
		t.Fatalf("prebootstrapped %s cache marker does not authenticate its executable", toolID)
	}
	executablePath := filepath.Join(installRoot, filepath.FromSlash(selected.Executable))
	requireRegularFile(t, executablePath, 1<<30)
	resolvedRoot, err := filepath.EvalSymlinks(installRoot)
	if err != nil {
		t.Fatalf("resolve prebootstrapped %s install root: %v", toolID, err)
	}
	resolvedExecutable, err := filepath.EvalSymlinks(executablePath)
	if err != nil || resolvedExecutable != resolvedRoot && !strings.HasPrefix(resolvedExecutable, resolvedRoot+string(os.PathSeparator)) {
		t.Fatalf("prebootstrapped %s executable escapes its selected install", toolID)
	}
	actualDigest := hashRegularFile(t, executablePath)
	if actualDigest != executableDigest {
		t.Fatalf("prebootstrapped %s executable digest does not match its authenticated cache marker", toolID)
	}
	return executablePath
}

func requireRegularFile(t *testing.T, path string, maximum int64) os.FileInfo {
	t.Helper()
	information, err := os.Lstat(path)
	if err != nil || information.Mode()&os.ModeSymlink != 0 || !information.Mode().IsRegular() ||
		information.Size() <= 0 || information.Size() > maximum {
		t.Fatalf("required private regular file is unavailable: %s", filepath.Base(path))
	}
	return information
}

func requireDeclaredRegularFile(t *testing.T, path string, maximum int64) os.FileInfo {
	t.Helper()
	information, err := os.Stat(path)
	if err != nil || !information.Mode().IsRegular() ||
		information.Size() <= 0 || information.Size() > maximum {
		t.Fatalf("required declared runfile is unavailable: %s", filepath.Base(path))
	}
	return information
}

func hashRegularFile(t *testing.T, path string) string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open authenticated executable: %v", err)
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		t.Fatalf("hash authenticated executable: %v", err)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func validatePlaywrightReport(t *testing.T, path string) {
	t.Helper()
	document := readBoundedFile(t, path, 4<<20)
	var report playwrightReport
	if err := json.Unmarshal(document, &report); err != nil || len(report.Suites) == 0 || len(report.Errors) != 0 {
		t.Fatal("Playwright did not produce a clean bounded JSON report")
	}
	cases := make(map[string]int, 9)
	expectedCases := make(map[string]int, 9)
	for index := range 9 {
		caseID := fmt.Sprintf("C%02d", index+1)
		cases[caseID] = 0
		expectedCases[caseID] = 1
	}
	expectedCases["C06"] = 2
	total := 0
	canonical := 0
	var visit func(playwrightSuite)
	visit = func(suite playwrightSuite) {
		for _, spec := range suite.Specs {
			total++
			if !spec.OK || len(spec.Tests) != 1 || spec.Tests[0].ExpectedStatus != "passed" ||
				len(spec.Tests[0].Results) != 1 || spec.Tests[0].Results[0].Status != "passed" {
				t.Fatalf("Playwright spec %q did not execute once and pass", spec.Title)
			}
			for caseID := range cases {
				if strings.Contains(spec.Title, "-"+caseID+" ") {
					cases[caseID]++
				}
			}
			if spec.Title == "browser canonical intent preserves exact values and Go key ordering" {
				canonical++
			}
		}
		for _, child := range suite.Suites {
			visit(child)
		}
	}
	for _, suite := range report.Suites {
		visit(suite)
	}
	if total != 11 || canonical != 1 {
		t.Fatalf("Playwright executed %d specs and %d canonical proofs; want the exact 11-spec parity inventory", total, canonical)
	}
	for caseID, count := range cases {
		if count != expectedCases[caseID] {
			t.Fatalf("Playwright parity case %s executed %d times; want %d", caseID, count, expectedCases[caseID])
		}
	}
}

func parityRepositoryRoot(t *testing.T) string {
	t.Helper()
	var candidates []string
	runfilesRoot := os.Getenv("TEST_SRCDIR")
	if runfilesRoot != "" {
		if !filepath.IsAbs(runfilesRoot) {
			t.Fatal("Bazel runfiles root must be absolute")
		}
		runfilesRoot = filepath.Clean(runfilesRoot)
		workspace := os.Getenv("TEST_WORKSPACE")
		if workspace != "" && workspace != "." && workspace != ".." && filepath.Base(workspace) == workspace {
			candidates = append(candidates, filepath.Join(runfilesRoot, workspace))
		}
		if workingDirectory, err := os.Getwd(); err == nil {
			workingDirectory, err = filepath.Abs(workingDirectory)
			relative, relativeErr := filepath.Rel(runfilesRoot, workingDirectory)
			insideRunfiles := relativeErr == nil && !filepath.IsAbs(relative) &&
				relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
			if err == nil && insideRunfiles {
				current := workingDirectory
				for {
					candidates = append(candidates, current)
					if current == runfilesRoot {
						break
					}
					parent := filepath.Dir(current)
					if parent == current {
						break
					}
					current = parent
				}
			}
		}
		candidates = append(candidates, runfilesRoot)
	} else {
		if workingDirectory, err := os.Getwd(); err == nil {
			current := workingDirectory
			for range 10 {
				candidates = append(candidates, current)
				parent := filepath.Dir(current)
				if parent == current {
					break
				}
				current = parent
			}
		}
		if _, currentFile, _, ok := runtime.Caller(0); ok && filepath.IsAbs(currentFile) {
			candidates = append(candidates, filepath.Clean(filepath.Join(filepath.Dir(currentFile), "../..")))
		}
	}
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		candidate, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		if _, duplicate := seen[candidate]; duplicate {
			continue
		}
		seen[candidate] = struct{}{}
		if regularFileExists(filepath.Join(candidate, "go.mod")) &&
			regularFileExists(filepath.Join(candidate, "build/toolchains.lock.json")) &&
			regularFileExists(filepath.Join(candidate, "tests/client-parity/fixtures.json")) {
			return candidate
		}
	}
	if runfilesRoot != "" {
		t.Fatal("locate client parity root inside declared Bazel runfiles")
	}
	t.Fatal("locate client parity repository root")
	return ""
}

func regularFileExists(path string) bool {
	information, err := os.Stat(path)
	return err == nil && information.Mode().IsRegular()
}

func readBoundedFile(t *testing.T, path string, maximum int64) []byte {
	t.Helper()
	information := requireRegularFile(t, path, maximum)
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open bounded file %s: %v", filepath.Base(path), err)
	}
	defer file.Close()
	document, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(document)) != information.Size() || int64(len(document)) > maximum {
		t.Fatalf("read bounded file %s", filepath.Base(path))
	}
	return document
}

func readDeclaredFile(t *testing.T, path string, maximum int64) []byte {
	t.Helper()
	information := requireDeclaredRegularFile(t, path, maximum)
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open declared runfile %s: %v", filepath.Base(path), err)
	}
	defer file.Close()
	document, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(document)) != information.Size() || int64(len(document)) > maximum {
		t.Fatalf("read declared runfile %s", filepath.Base(path))
	}
	return document
}

func newParityTemporalHarness(t *testing.T) *parityTemporalHarness {
	t.Helper()
	artifact := testfixture.NewWorkerConfiguration(t)
	artifact.Verify(t)
	configuration := artifact.Configuration
	temporalConfiguration := configuration.Temporal()
	workerConfiguration := configuration.Worker()
	backend := testfixture.OpenTemporal(
		t,
		configuration,
		temporalConfiguration.Namespace,
		nextParityTemporalIdentity("observer"),
	)
	observer, err := pmtemporal.NewWorkflowContinuationObserver(
		backend,
		temporalConfiguration.Namespace,
		workerConfiguration.ActivityTimeouts.Transport,
	)
	if err != nil {
		t.Fatalf("construct real Temporal continuation observer: %v", err)
	}
	relayOptions := testfixture.TemporalOptions(
		t,
		configuration,
		temporalConfiguration.Namespace,
		nextParityTemporalIdentity("relay"),
	)
	relayClient, err := pmtemporal.NewClient(pmtemporal.ClientConfig{
		Namespace:            temporalConfiguration.Namespace,
		ControlTaskQueue:     contract.ControlTaskQueue,
		WorkflowType:         contract.RunWorkflowType,
		ProjectionUpdateName: contract.ProjectionUpdateName,
		DialTimeout:          workerConfiguration.DialTimeout,
		RequestTimeout:       workerConfiguration.ActivityTimeouts.Transport,
		Dial: func(ctx context.Context) (pmtemporal.WorkflowClient, error) {
			return pmtemporal.DialWorkerClient(ctx, relayOptions, workerConfiguration.DialTimeout)
		},
	})
	if err != nil {
		t.Fatalf("construct real Temporal relay client: %v", err)
	}
	t.Cleanup(relayClient.Close)
	return &parityTemporalHarness{
		configuration: configuration,
		worker:        workerConfiguration,
		namespace:     temporalConfiguration.Namespace,
		backend:       backend,
		relayClient:   relayClient,
		observer:      observer,
	}
}

func nextParityTemporalIdentity(role string) string {
	return fmt.Sprintf("client-parity-%s-%d", role, parityTemporalIdentitySequence.Add(1))
}

func prepareParityWorkerRun(
	t *testing.T,
	fixture *testfixture.Fixture,
	temporal *parityTemporalHarness,
	handlers *app.HandlerRegistry,
	resolver app.OperationResolver,
	runID string,
) parityWorkerRun {
	t.Helper()
	var commandID string
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT command_id
			FROM pm.outbox_commands
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
			  AND kind = 'start_run' AND state = 'pending'
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID).Scan(&commandID)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	relay, err := pmtemporal.NewRelay(pmtemporal.RelayConfig{
		Store: fixture.Pool, Client: temporal.relayClient,
		OwnerID: nextParityTemporalIdentity("relay-owner"), ClaimLimit: 1,
		ClaimTTL: 30 * time.Second, IdleDelay: 10 * time.Millisecond, FailureDelay: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("construct production Temporal relay: %v", err)
	}
	if err := relay.RunOnce(ctx); err != nil {
		t.Fatalf("relay accepted parity outbox command: %v", err)
	}

	var run parityWorkerRun
	var firstExecutionRunID string
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(
		ctx context.Context,
		tx *store.Transaction,
	) error {
		record, err := store.LoadRun(ctx, tx, runID)
		if err != nil {
			return err
		}
		intent, err := store.LoadWorkerCommandIntent(ctx, tx, runID, commandID)
		if err != nil {
			return err
		}
		if len(record.WorkflowBinding.Observations) != 1 ||
			record.WorkflowBinding.WorkflowID != intent.Plan.WorkflowID ||
			record.WorkflowBinding.Generation != intent.Plan.Generation ||
			intent.Plan.Namespace != temporal.namespace ||
			intent.Plan.TaskQueue != contract.ControlTaskQueue ||
			intent.Plan.WorkflowType != contract.RunWorkflowType {
			return errors.New("production relay did not persist one exact Temporal execution binding")
		}
		firstExecutionRunID = record.WorkflowBinding.Observations[0].TemporalRunID
		run.execution = contract.WorkflowExecution{
			Namespace:  temporal.namespace,
			WorkflowID: record.WorkflowBinding.WorkflowID,
			RunID:      record.WorkflowBinding.CurrentTemporalRunID(),
		}
		run.reference = contract.WorkflowRunRef{
			Scope: fixture.Scope, RunID: runID,
			WorkflowID:     record.WorkflowBinding.WorkflowID,
			StartCommandID: commandID, Generation: record.WorkflowBinding.Generation,
			IntentDigest: intent.Digest,
		}
		if err := run.execution.Validate(); err != nil {
			return err
		}
		return run.reference.Validate()
	}); err != nil {
		t.Fatalf("load exact relayed parity workflow binding: %v", err)
	}
	description, err := temporal.backend.DescribeWorkflowExecution(ctx, run.execution.WorkflowID, run.execution.RunID)
	if err != nil || description.GetWorkflowExecutionInfo().GetExecution().GetRunId() != run.execution.RunID {
		t.Fatal("describe exact relayed parity workflow execution")
	}
	testfixture.CleanupWorkflowChain(t, temporal.backend, temporal.namespace, run.execution.WorkflowID, firstExecutionRunID)
	run.service = testfixture.NewWorkerService(
		t,
		fixture,
		handlers,
		resolver,
		nil,
		temporal.observer,
		temporal.worker.ActivityTimeouts,
	)
	run.plan = testfixture.SealWorkerPlan(t, fixture, run.service, run.execution, run.reference)
	return run
}

func executeParityWorkerRun(
	t *testing.T,
	temporal *parityTemporalHarness,
	run parityWorkerRun,
) contract.WorkflowProjectionResult {
	t.Helper()
	activityService, err := activities.New(run.service)
	if err != nil {
		t.Fatalf("construct parity worker activity service: %v", err)
	}
	coordinator, err := workflows.NewCoordinator(temporal.worker.ActivityTimeouts)
	if err != nil {
		t.Fatalf("construct parity workflow coordinator: %v", err)
	}
	workerClient := testfixture.OpenTemporal(
		t,
		temporal.configuration,
		temporal.namespace,
		nextParityTemporalIdentity("worker"),
	)
	worker := sdkworker.New(workerClient, temporal.worker.TaskQueue, sdkworker.Options{
		MaxConcurrentActivityExecutionSize:     int(temporal.worker.MaxConcurrentActivities),
		MaxConcurrentWorkflowTaskExecutionSize: int(temporal.worker.MaxConcurrentWorkflowTasks),
		WorkerStopTimeout:                      temporal.worker.DrainTimeout,
	})
	if err := workflows.RegisterWorkflows(worker, coordinator, run.service.SupportsScheduledAdmission()); err != nil {
		t.Fatalf("register parity workflows: %v", err)
	}
	if err := activities.RegisterActivities(worker, activityService); err != nil {
		t.Fatalf("register parity activities: %v", err)
	}
	if err := worker.Start(); err != nil {
		t.Fatalf("start parity worker: %v", err)
	}
	defer worker.Stop()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	var projection contract.WorkflowProjectionResult
	if err := temporal.backend.GetWorkflow(ctx, run.execution.WorkflowID, run.execution.RunID).Get(ctx, &projection); err != nil {
		t.Fatalf("await real parity workflow: %v", err)
	}
	return projection
}

func assertParityExecution(
	t *testing.T,
	fixture *testfixture.Fixture,
	source parityFixtureSource,
	admission pmapi.AdmissionResponse,
	initial parityRunState,
	expectedInput contract.PresentValue,
	run parityWorkerRun,
	oracle *parityEffectOracle,
	counters *parityCounters,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(
		ctx context.Context,
		tx *store.Transaction,
	) error {
		state, err := store.LoadRun(ctx, tx, admission.RunRef)
		if err != nil {
			return err
		}
		input, err := store.LoadRunInputSnapshot(ctx, tx, admission.RunRef)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(state.Spec, initial.Record.Spec) || !input.Input.Equal(expectedInput) ||
			state.State != contract.RunStateSucceeded || state.CancelRequested ||
			state.WorkflowBinding.WorkflowID != run.execution.WorkflowID ||
			state.WorkflowBinding.CurrentTemporalRunID() != run.execution.RunID ||
			state.WorkflowBinding.Generation != run.reference.Generation ||
			len(state.WorkflowBinding.Observations) != 1 ||
			state.EffectSummary.Succeeded != 1 || state.EffectSummary.Unknown != 0 ||
			state.EffectSummary.Unsent != 0 || state.EffectSummary.Unchanged != 0 || state.EffectSummary.Rejected != 0 {
			return errors.New("final run state lost exact immutable intent, workflow binding, or effect truth")
		}
		task := store.TaskRef{RunID: admission.RunRef, TaskID: run.plan.TaskID}
		dispatch, err := store.LoadWorkerDispatch(ctx, tx, task)
		if err != nil {
			return err
		}
		if dispatch.Intent.EffectKey != run.plan.Dispatch.EffectKey ||
			dispatch.Intent.DispatchID != run.plan.Dispatch.DispatchID ||
			dispatch.Intent.AttemptID != run.plan.Dispatch.AttemptID ||
			!slices.Equal(dispatch.Intent.OrderedMemberIDs, run.plan.Dispatch.OrderedMemberIDs) ||
			len(dispatch.Attempts) != 1 ||
			dispatch.Attempts[0].AttemptID != run.plan.Dispatch.AttemptID ||
			dispatch.Attempts[0].DispatchID != run.plan.Dispatch.DispatchID {
			return errors.New("durable dispatch lost its exact sealed effect identity")
		}
		outcomes, err := store.ListMemberOutcomes(ctx, tx, task, run.plan.Dispatch.DispatchID)
		if err != nil {
			return err
		}
		if len(outcomes) != len(run.plan.Dispatch.OrderedMemberIDs) {
			return errors.New("durable member outcome inventory does not match the sealed dispatch")
		}
		for index, outcome := range outcomes {
			if outcome.MemberID != run.plan.Dispatch.OrderedMemberIDs[index] ||
				outcome.EffectKey != run.plan.Dispatch.EffectKey ||
				outcome.DispatchID != run.plan.Dispatch.DispatchID ||
				outcome.State != contract.OutcomeSucceeded {
				return errors.New("durable member outcome lost exact effect truth")
			}
		}
		effects, err := store.ListWorkerRunEffects(ctx, tx, admission.RunRef)
		if err != nil {
			return err
		}
		if len(effects) != 1 || effects[0].EffectKey != run.plan.Dispatch.EffectKey ||
			effects[0].State != contract.OutcomeSucceeded || len(effects[0].Attempts) != 1 ||
			effects[0].Attempts[0].AttemptID != run.plan.Dispatch.AttemptID {
			return errors.New("durable logical effect is not the one exact succeeded controlled effect")
		}
		view, err := store.LoadRunView(ctx, tx, admission.RunRef)
		if err != nil {
			return err
		}
		if view.Record.Spec.RunID != admission.RunRef || view.Record.State != contract.RunStateSucceeded ||
			view.Record.CancelRequested || view.Record.UnknownEffectCount != 0 ||
			view.Record.EffectSummary.Succeeded != uint64(len(outcomes)) {
			return errors.New("final authorized run view disagrees with durable effect truth")
		}
		return nil
	}); err != nil {
		t.Fatalf("assert final parity durability: %v", err)
	}

	counts := parityDurableStateCounts(t, fixture, fixture.Scope, source.RequestKey)
	if counts != (parityDurableCounts{Runs: 1, RunSpecs: 1, AdmissionKeys: 1, Inputs: 1, StartCommands: 1, Effects: 1}) {
		t.Fatalf("final durable parity counts = %#v; want exactly one logical run and effect", counts)
	}
	foreignCounts := parityDurableStateCounts(t, fixture, fixture.ForeignScope, source.RequestKey)
	if foreignCounts != (parityDurableCounts{}) {
		t.Fatalf("final foreign durable counts = %#v; want no cross-principal mutation", foreignCounts)
	}
	var outboxCount int64
	var outboxCommandID, outboxState string
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT count(*), COALESCE(max(command_id), ''), COALESCE(max(state), '')
			FROM pm.outbox_commands
			WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND kind = 'start_run'
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, admission.RunRef).Scan(
			&outboxCount,
			&outboxCommandID,
			&outboxState,
		)
	})
	if outboxCount != 1 || outboxCommandID != run.reference.StartCommandID || outboxState != "acknowledged" {
		t.Fatal("original Temporal start outbox command was not acknowledged exactly once")
	}
	evidence, calls := oracle.snapshot()
	if calls != 1 || counters.effects.Load() != 1 || evidence.RunID != admission.RunRef ||
		evidence.ConnectionID != fixture.Connection.ConnectionID || evidence.OperationID != source.OperationID ||
		evidence.TaskID != run.plan.TaskID || evidence.EffectKey != run.plan.Dispatch.EffectKey ||
		evidence.DispatchID != run.plan.Dispatch.DispatchID || evidence.AttemptID != run.plan.Dispatch.AttemptID ||
		evidence.Text != source.FirstText {
		t.Fatal("independent controlled provider observation did not retain exactly alpha-one on the original effect")
	}
}
