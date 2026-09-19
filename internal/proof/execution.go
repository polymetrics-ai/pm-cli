package proof

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/connectors"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

// ComponentIdentityHashBound bounds every operator-declared artifact the runner
// actually hashes. It is deliberately finite and small.
const ComponentIdentityHashBound = 512 << 20

// JournalBound bounds the controlled-provider observation journal.
const JournalBound = 16 << 20

var (
	// errLiveObservationUnavailable is returned instead of fabricating provider
	// wire evidence. A live provider read has no observation source the runner
	// owns, so live receipts stay blocked until the operator supplies one.
	errLiveObservationUnavailable = errors.New("live provider observation source is unavailable")
	errTerminalProjection         = errors.New("terminal run projection observed")
)

// PMPathConfig selects one real PM deployment and the exact actual artifacts
// that constitute it. Every path is operator-declared: the runner hashes those
// bytes and never infers identity from a name, descriptor or claim.
type PMPathConfig struct {
	ProfilesPath string
	ProfileName  string
	Workspace    string
	Connection   string
	SessionMode  string

	PMClientBinary    string
	APIBinary         string
	WorkerBinary      string
	QueryHelperBinary string
	BundlePath        string
	ProviderJournal   string

	MaximumFileBytes int64
	RequestTimeout   time.Duration
	WaitTimeout      time.Duration
	HTTPClient       pmapi.HttpRequestDoer
	Now              func() time.Time
}

// PMPathExecutor drives the actual public client/API/worker path for one
// scenario step. It never reads a generated descriptor as evidence of a result
// and never copies expected data into an observation.
type PMPathExecutor struct {
	configuration PMPathConfig
	profile       pmapi.ServerProfile
	bundle        []byte
	connectorID   string
	hashes        componentHashes
	journal       *providerJournal

	observation ExecutorObservation
}

// ExecutorObservation reports which exact artifacts the runner hashed. It is
// retained for the receipt command line and never treated as proof by itself.
type ExecutorObservation struct {
	PMClientHash      contract.SourceHash
	APIHash           contract.SourceHash
	WorkerHash        contract.SourceHash
	QueryHelperHash   *contract.SourceHash
	BundleHash        contract.SourceHash
	ProviderJournal   string
	DeploymentProfile string
	ServerURL         string
	InstanceID        string
}

type componentHashes struct {
	client      contract.SourceHash
	api         contract.SourceHash
	worker      contract.SourceHash
	queryHelper *contract.SourceHash
	bundle      contract.SourceHash
}

// NewPMPathExecutor hashes the declared artifacts once and prepares the
// authenticated public client. A missing, oversized, non-regular or unreadable
// artifact fails construction instead of producing weaker evidence.
func NewPMPathExecutor(ctx context.Context, configuration PMPathConfig) (*PMPathExecutor, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, errors.New("proof executor context is unavailable")
	}
	if configuration.MaximumFileBytes <= 0 || configuration.MaximumFileBytes > ComponentIdentityHashBound {
		return nil, errors.New("proof executor requires a finite artifact hash bound")
	}
	if configuration.RequestTimeout <= 0 || configuration.WaitTimeout <= 0 {
		return nil, errors.New("proof executor requires finite request and wait timeouts")
	}
	if configuration.ProfilesPath == "" || configuration.ProfileName == "" ||
		configuration.Workspace == "" || configuration.Connection == "" {
		return nil, errors.New("proof executor requires an explicit server profile, workspace and connection")
	}
	hashes, err := hashComponentArtifacts(configuration)
	if err != nil {
		return nil, err
	}
	bundle, err := readBoundedProofFile(configuration.BundlePath, configuration.MaximumFileBytes)
	if err != nil {
		return nil, fmt.Errorf("read declared execution bundle: %w", err)
	}
	bundleRecord, err := connectors.LoadBundle(bytes.NewReader(bundle))
	if err != nil {
		return nil, fmt.Errorf("decode declared execution bundle: %w", err)
	}
	profile, err := pmapi.LoadServerProfile(configuration.ProfilesPath, configuration.ProfileName)
	if err != nil {
		return nil, fmt.Errorf("load declared server profile: %w", err)
	}
	executor := &PMPathExecutor{
		configuration: configuration, profile: profile, bundle: bundle,
		connectorID: bundleRecord.ConnectorID, hashes: hashes,
	}
	if configuration.ProviderJournal != "" {
		journal, err := openProviderJournal(configuration.ProviderJournal, configuration.MaximumFileBytes)
		if err != nil {
			return nil, err
		}
		executor.journal = journal
	}
	executor.observation = ExecutorObservation{
		PMClientHash: hashes.client, APIHash: hashes.api, WorkerHash: hashes.worker,
		QueryHelperHash: hashes.queryHelper, BundleHash: hashes.bundle,
		ProviderJournal: configuration.ProviderJournal, DeploymentProfile: profile.Name,
		ServerURL: profile.ServerURL, InstanceID: profile.InstanceID,
	}
	return executor, nil
}

// Observation returns the exact hashed artifact identity retained by the
// receipt command line and provenance.
func (executor *PMPathExecutor) Observation() ExecutorObservation {
	if executor == nil {
		return ExecutorObservation{}
	}
	return executor.observation
}

// ConnectorID returns the connector identity of the hashed bundle.
func (executor *PMPathExecutor) ConnectorID() string {
	if executor == nil {
		return ""
	}
	return executor.connectorID
}

// ExecuteStep performs the actual public invocation for one authored step and
// returns only what the executing components observed.
func (executor *PMPathExecutor) ExecuteStep(ctx context.Context, request StepRequest) (StepExecution, error) {
	if executor == nil || ctx == nil {
		return StepExecution{}, errors.New("proof executor and context are required")
	}
	if request.Identity.Identity.ExecutionKind == "live" {
		return StepExecution{}, errLiveObservationUnavailable
	}
	if request.Identity.Identity.ExecutionKind != "fake" {
		return StepExecution{}, errors.New("proof step requires an explicit fake or live evidence kind")
	}
	if executor.journal == nil {
		return StepExecution{}, errors.New("controlled provider observation journal is required for fake evidence")
	}
	if request.Step.Kind != "invoke" && request.Step.Kind != "readback" {
		return StepExecution{}, errors.New("public direct-read proof supports invoke and readback steps only")
	}
	if request.Step.ConnectionRef != "" && request.Step.ConnectionRef != executor.configuration.Connection {
		return StepExecution{}, errors.New("proof step selects a different connection than the hashed deployment")
	}
	operation, err := executor.bundleOperation(request.Step.Action)
	if err != nil {
		return StepExecution{}, err
	}
	client, err := executor.qualify(ctx)
	if err != nil {
		return StepExecution{}, err
	}
	descriptor, err := executor.commandDescriptor(ctx, client, request.Step.Action)
	if err != nil {
		return StepExecution{}, err
	}
	input, err := bindNativeInput(request.Input, descriptor)
	if err != nil {
		return StepExecution{}, err
	}
	controls, err := bindResultControls(request.Step.ResultControls)
	if err != nil {
		return StepExecution{}, err
	}
	started := executor.now()
	baseline, err := executor.journal.entries()
	if err != nil {
		return StepExecution{}, err
	}
	runID, err := executor.submit(ctx, client, descriptor, input, controls, request.RequestKey)
	if err != nil {
		return StepExecution{}, err
	}
	if err := executor.awaitTerminal(ctx, client, runID); err != nil {
		return StepExecution{}, err
	}
	observation, result, effect, err := executor.readResult(ctx, client, runID)
	if err != nil {
		return StepExecution{}, err
	}
	entry, err := executor.journal.takeAfter(len(baseline))
	if err != nil {
		return StepExecution{}, err
	}
	finished := executor.now()
	execution := StepExecution{
		Identity: Currentness{
			Identity: CaseIdentity{
				ConnectorID: executor.connectorID, Lane: contract.LaneDirectRead,
				OperationID: operation.OperationID, BehaviorID: request.Identity.Identity.BehaviorID,
				CaseID: request.Identity.Identity.CaseID, ExecutionKind: request.Identity.Identity.ExecutionKind,
				ScopeHash: executor.scopeHash(request.Identity.Identity.ScopeHash),
			},
			SourceHashes:          sourceHashes(bundleSourceDigest(executor.bundle)),
			BundleHash:            executor.hashes.bundle,
			SchemaHash:            executor.hashes.bundle,
			AssertionHash:         request.Identity.AssertionHash,
			PMClientHash:          executor.hashes.client,
			PMAPIHash:             executor.hashes.api,
			PMWorkerHash:          executor.hashes.worker,
			QueryHelperHash:       executor.hashes.queryHelper,
			HandlerVersions:       map[string]string{"catalog": descriptor.CatalogRevision, "operation": descriptor.OperationRevision},
			ImplementationClosure: contract.ImplementationClosure{Known: false},
		},
		RunID: runID, Command: executor.command(runID, request.Step.Action, descriptor),
		Request: entry.request, Response: entry.response, Observation: observation,
		Effect: effect, StartedAt: started, FinishedAt: finished, ExitCode: exitCodeFor(effect),
	}
	if result == nil {
		return StepExecution{}, errors.New("public path produced no authoritative result projection")
	}
	return execution, nil
}

// DeploymentObservation is what the runner observed about the exact running
// deployment before any case executes. It supplies the currentness inputs that
// only the live server can reveal (catalog and operation revisions) without
// executing a case.
type DeploymentObservation struct {
	ConnectorID       string
	BundleHash        contract.SourceHash
	ScopeHash         contract.SemanticDigest
	HandlerVersions   map[string]map[string]string
	SourceHashes      []contract.SourceHash
	DeploymentProfile string
	InstanceID        string
}

// ObserveDeployment qualifies the declared server, session and catalog once and
// returns only observed identity. It performs no invocation and never treats a
// descriptor as execution.
func (executor *PMPathExecutor) ObserveDeployment(ctx context.Context) (DeploymentObservation, error) {
	if executor == nil || ctx == nil {
		return DeploymentObservation{}, errors.New("proof executor and context are required")
	}
	client, err := executor.qualify(ctx)
	if err != nil {
		return DeploymentObservation{}, err
	}
	requestContext, cancel := context.WithTimeout(ctx, executor.configuration.RequestTimeout)
	defer cancel()
	snapshot, err := pmapi.LoadCatalog(
		requestContext, client, pmapi.NewCatalogCache(),
		pmapi.WorkspaceID(executor.configuration.Workspace), executor.configuration.Connection, "",
	)
	if err != nil {
		return DeploymentObservation{}, fmt.Errorf("qualify declared catalog binding: %w", err)
	}
	if snapshot.Binding.ServerInstanceID != executor.profile.InstanceID {
		return DeploymentObservation{}, errors.New("qualified catalog instance differs from the selected server profile")
	}
	if snapshot.Binding.ConnectionID != executor.configuration.Connection ||
		string(snapshot.Binding.WorkspaceID) != executor.configuration.Workspace {
		return DeploymentObservation{}, errors.New("qualified catalog binding differs from the selected workspace and connection")
	}
	versions := make(map[string]map[string]string)
	for _, descriptor := range snapshot.ExecutableCommands() {
		if descriptor.ConnectorId != executor.connectorID {
			return DeploymentObservation{}, errors.New("qualified connection selects a different connector than the hashed bundle")
		}
		versions[descriptor.OperationId] = map[string]string{
			"catalog": descriptor.CatalogRevision, "operation": descriptor.OperationRevision,
		}
	}
	return DeploymentObservation{
		ConnectorID: executor.connectorID, BundleHash: executor.hashes.bundle,
		ScopeHash: executor.scopeHash(""), HandlerVersions: versions,
		SourceHashes:      sourceHashes(bundleSourceDigest(executor.bundle)),
		DeploymentProfile: executor.profile.Name, InstanceID: executor.profile.InstanceID,
	}, nil
}

func (executor *PMPathExecutor) now() time.Time {
	if executor.configuration.Now != nil {
		return executor.configuration.Now().UTC()
	}
	return time.Now().UTC()
}

func (executor *PMPathExecutor) bundleOperation(operationID string) (contract.Operation, error) {
	record, err := connectors.LoadBundle(bytes.NewReader(executor.bundle))
	if err != nil {
		return contract.Operation{}, err
	}
	for _, operation := range record.Operations {
		if operation.OperationID == operationID {
			return operation, nil
		}
	}
	return contract.Operation{}, errors.New("authored step operation is absent from the declared execution bundle")
}

func (executor *PMPathExecutor) qualify(ctx context.Context) (*pmapi.QualifiedClient, error) {
	requestContext, cancel := context.WithTimeout(ctx, executor.configuration.RequestTimeout)
	defer cancel()
	client, err := pmapi.NewQualifiedClient(requestContext, executor.profile, pmapi.QualifiedClientOptions{
		HTTPClient: configurationHTTPClient(executor.configuration), RequestTimeout: executor.configuration.RequestTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("qualify declared PM server: %w", err)
	}
	mode := pmapi.SessionStoreMode(executor.configuration.SessionMode)
	if mode == "" {
		mode = pmapi.SessionStoreOS
	}
	store, err := pmapi.NewSessionStore(mode)
	if err != nil {
		return nil, pmapi.ErrSessionStoreUnavailable
	}
	authenticated, err := client.WithSession(requestContext, store)
	if err != nil {
		return nil, fmt.Errorf("authenticate declared PM session: %w", err)
	}
	return authenticated, nil
}

func configurationHTTPClient(configuration PMPathConfig) pmapi.HttpRequestDoer {
	return configuration.HTTPClient
}

func (executor *PMPathExecutor) commandDescriptor(ctx context.Context, client *pmapi.QualifiedClient, operationID string) (pmapi.CommandDescriptor, error) {
	requestContext, cancel := context.WithTimeout(ctx, executor.configuration.RequestTimeout)
	defer cancel()
	snapshot, err := pmapi.LoadCatalog(
		requestContext, client, pmapi.NewCatalogCache(),
		pmapi.WorkspaceID(executor.configuration.Workspace), executor.configuration.Connection, "",
	)
	if err != nil {
		return pmapi.CommandDescriptor{}, fmt.Errorf("qualify declared catalog binding: %w", err)
	}
	for _, descriptor := range snapshot.ExecutableCommands() {
		if descriptor.OperationId != operationID {
			continue
		}
		if !slices.Contains(descriptor.AvailableLanes, pmapi.CommandDescriptorAvailableLanes(contract.LaneDirectRead)) {
			return pmapi.CommandDescriptor{}, errors.New("qualified command does not declare the direct-read lane")
		}
		return descriptor, nil
	}
	return pmapi.CommandDescriptor{}, errors.New("qualified catalog exposes no executable command for the authored operation")
}

func (executor *PMPathExecutor) submit(
	ctx context.Context,
	client *pmapi.QualifiedClient,
	descriptor pmapi.CommandDescriptor,
	input contract.PresentValue,
	controls *contract.ResultControls,
	requestKey string,
) (string, error) {
	selection, err := pmapi.SelectRequestKey(requestKey)
	if err != nil {
		return "", fmt.Errorf("proof request identity is invalid: %w", err)
	}
	payload := struct {
		ConnectionRef  string                  `json:"connection_ref"`
		Input          contract.PresentValue   `json:"input"`
		Lane           contract.Lane           `json:"lane"`
		OperationRef   string                  `json:"operation_ref"`
		ResultControls contract.ResultControls `json:"result_controls"`
	}{
		ConnectionRef: executor.configuration.Connection, Input: input,
		Lane: contract.LaneDirectRead, OperationRef: descriptor.OperationId,
	}
	if controls != nil {
		payload.ResultControls = *controls
	}
	canonical, err := json.Marshal(struct {
		ExpectedRevision string `json:"expected_revision"`
		Operation        string `json:"operation"`
		Payload          any    `json:"payload"`
		Target           string `json:"target"`
		Workspace        string `json:"workspace"`
	}{
		ExpectedRevision: descriptor.OperationRevision, Operation: "invocation.submit",
		Payload: payload, Target: executor.configuration.Connection + "/" + descriptor.OperationId,
		Workspace: executor.configuration.Workspace,
	})
	if err != nil {
		return "", err
	}
	intentDigest, err := contract.CanonicalDigest(canonical)
	if err != nil {
		return "", err
	}
	requestContext, cancel := context.WithTimeout(ctx, executor.configuration.RequestTimeout)
	defer cancel()
	response, err := client.API().SubmitInvocationWithResponse(
		requestContext,
		pmapi.WorkspaceID(executor.configuration.Workspace),
		&pmapi.SubmitInvocationParams{IdempotencyKey: pmapi.IdempotencyKey(selection.RequestKey)},
		pmapi.InvocationRequest{
			ConnectionRef: executor.configuration.Connection, Input: input,
			Lane: pmapi.InvocationRequestLane(contract.LaneDirectRead), OperationRef: descriptor.OperationId,
			Protocol: pmapi.ProtocolMetadata{
				ExpectedRevision: descriptor.OperationRevision, IntentDigest: string(intentDigest),
				RequestKey:       selection.RequestKey,
				RequiredFeatures: []string{"catalog-v1", "exact-values"},
				WireVersion:      pmapi.ProtocolMetadataWireVersion(pmapi.WireVersion),
			},
			ResultControls: payload.ResultControls,
		},
	)
	if err != nil {
		return "", fmt.Errorf("submit declared invocation: %w", err)
	}
	accepted := response.JSON202
	if accepted == nil || response.StatusCode() != 202 || string(accepted.State) != "accepted" ||
		accepted.RequestKey != selection.RequestKey || accepted.RunRef == "" {
		return "", errors.New("declared invocation was not accepted with its exact request identity")
	}
	return accepted.RunRef, nil
}

func (executor *PMPathExecutor) awaitTerminal(ctx context.Context, client *pmapi.QualifiedClient, runID string) error {
	waitContext, cancel := context.WithTimeout(ctx, executor.configuration.WaitTimeout)
	defer cancel()
	_, err := client.WatchEvents(
		waitContext,
		pmapi.WorkspaceID(executor.configuration.Workspace),
		pmapi.RunID(runID),
		pmapi.WatchEventsOptions{Handle: func(frame pmapi.RunEventFrame) error {
			if frame.Snapshot == nil {
				return nil
			}
			if runStateTerminal(string(frame.Snapshot.Run.State)) {
				return errTerminalProjection
			}
			return nil
		}},
	)
	if err != nil && !errors.Is(err, errTerminalProjection) && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("observe durable run projection: %w", err)
	}
	return nil
}

func runStateTerminal(state string) bool {
	switch contract.RunState(state) {
	case contract.RunStateSucceeded, contract.RunStatePartial, contract.RunStateFailed, contract.RunStateCancelled:
		return true
	default:
		return false
	}
}

func (executor *PMPathExecutor) readResult(
	ctx context.Context,
	client *pmapi.QualifiedClient,
	runID string,
) (OracleObservation, json.RawMessage, contract.ProofEffect, error) {
	requestContext, cancel := context.WithTimeout(ctx, executor.configuration.RequestTimeout)
	defer cancel()
	response, err := client.API().GetRunResultWithResponse(
		requestContext, pmapi.WorkspaceID(executor.configuration.Workspace), pmapi.RunID(runID), nil,
	)
	if err != nil {
		return OracleObservation{}, nil, contract.ProofEffect{}, fmt.Errorf("read authoritative run result: %w", err)
	}
	view := response.JSON200
	if view == nil || len(view.ResultReferences) == 0 {
		return OracleObservation{}, nil, contract.ProofEffect{}, errors.New("run exposes no published result projection")
	}
	var document boundedDocument
	document.limit = executor.configuration.MaximumFileBytes
	for _, reference := range view.ResultReferences {
		requestContext, cancel := context.WithTimeout(ctx, executor.configuration.RequestTimeout)
		err := client.DownloadArtifact(
			requestContext, pmapi.WorkspaceID(executor.configuration.Workspace),
			pmapi.ArtifactID(reference.ResultRef), "", &document,
		)
		cancel()
		if err != nil {
			return OracleObservation{}, nil, contract.ProofEffect{}, fmt.Errorf("download published result bytes: %w", err)
		}
	}
	result := json.RawMessage(bytes.Clone(document.Bytes()))
	effect := effectFromSummary(view.EffectSummary)
	return OracleObservation{Document: result}, result, effect, nil
}

// boundedDocument accumulates downloaded result bytes under an explicit bound so
// an oversized published object cannot exhaust the runner's memory.
type boundedDocument struct {
	buffer bytes.Buffer
	limit  int64
}

func (document *boundedDocument) Write(content []byte) (int, error) {
	if document.limit <= 0 || int64(document.buffer.Len())+int64(len(content)) > document.limit {
		return 0, errors.New("published result exceeds the declared byte bound")
	}
	return document.buffer.Write(content)
}

func (document *boundedDocument) Bytes() []byte { return document.buffer.Bytes() }

func effectFromSummary(summary pmapi.EffectSummary) contract.ProofEffect {
	effect := contract.ProofEffect{
		State: contract.OutcomeUnsent, ReceiptRefs: []string{}, OwnedObjectRefs: []string{},
	}
	switch {
	case counterPositive(summary.Unknown):
		effect.State = contract.OutcomeUnknown
	case counterPositive(summary.Rejected):
		effect.State = contract.OutcomeRejected
	case counterPositive(summary.Succeeded), counterPositive(summary.Unchanged):
		effect.State = contract.OutcomeSucceeded
	default:
		effect.State = contract.OutcomeUnsent
	}
	return effect
}

func counterPositive(counter pmapi.ExactCounter) bool {
	return counter != "" && counter != "0"
}

func exitCodeFor(effect contract.ProofEffect) int {
	if effect.State == contract.OutcomeSucceeded || effect.State == contract.OutcomeUnchanged {
		return 0
	}
	return 1
}

func (executor *PMPathExecutor) scopeHash(declared contract.SemanticDigest) contract.SemanticDigest {
	if declared.Validate() == nil {
		return declared
	}
	document, err := json.Marshal(struct {
		Profile    string `json:"profile"`
		Instance   string `json:"instance_id"`
		Workspace  string `json:"workspace"`
		Connection string `json:"connection"`
	}{
		Profile: executor.profile.Name, Instance: executor.profile.InstanceID,
		Workspace: executor.configuration.Workspace, Connection: executor.configuration.Connection,
	})
	if err != nil {
		return ""
	}
	digest, err := contract.CanonicalDigest(document)
	if err != nil {
		return ""
	}
	return digest
}

func (executor *PMPathExecutor) command(runID, operationID string, descriptor pmapi.CommandDescriptor) []string {
	parts := []string{
		"pm", "--server-profile", executor.profile.Name,
		"--workspace", executor.configuration.Workspace,
		"--connection", executor.configuration.Connection,
		"connector", executor.connectorID, operationID,
		"--lane", string(contract.LaneDirectRead),
	}
	parts = append(parts, "--run", runID, "--catalog-revision", descriptor.CatalogRevision,
		"--operation-revision", descriptor.OperationRevision,
		"--bundle", string(executor.observation.BundleHash), "--query-helper", queryHelperText(executor.hashes.queryHelper))
	return parts
}

func queryHelperText(hash *contract.SourceHash) string {
	if hash == nil {
		return "none"
	}
	return string(*hash)
}

// bindNativeInput converts authored exact JSON into the typed logical input
// object the public boundary requires. It is schema-directed: a value whose
// JSON shape does not match its declared logical type fails instead of being
// coerced.
func bindNativeInput(document json.RawMessage, descriptor pmapi.CommandDescriptor) (contract.PresentValue, error) {
	if len(document) == 0 || len(document) > contract.MaxContractDocumentBytes {
		return contract.PresentValue{}, errors.New("authored step input exceeds the contract document bound")
	}
	if _, err := contract.CanonicalDigest(document); err != nil {
		return contract.PresentValue{}, errors.New("authored step input is not canonical exact JSON")
	}
	if trimmed := bytes.TrimSpace(document); len(trimmed) == 0 || trimmed[0] != '{' {
		return contract.PresentValue{}, errors.New("authored step input must be a JSON object")
	}
	if len(descriptor.Fields) == 0 {
		var empty map[string]json.RawMessage
		if err := json.Unmarshal(document, &empty); err != nil || len(empty) != 0 {
			return contract.PresentValue{}, errors.New("authored step input must be an empty object for this operation")
		}
		return contract.NewObjectValue(map[string]contract.PresentValue{}), nil
	}
	schemas := make(map[string]contract.Schema, len(descriptor.Fields))
	bindings := make([]contract.FieldBinding, 0, len(descriptor.Fields))
	for _, field := range descriptor.Fields {
		schemas[field.Binding.FieldID] = field.Binding.Schema
		bindings = append(bindings, field.Binding)
	}
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	if err := decoder.Decode(&fields); err != nil {
		return contract.PresentValue{}, fmt.Errorf("decode authored step input: %w", err)
	}
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	for fieldID := range fields {
		if _, declared := schemas[fieldID]; !declared {
			return contract.PresentValue{}, fmt.Errorf("authored step input supplies undeclared field %q", fieldID)
		}
	}
	values := make(map[string]contract.PresentValue, len(fields))
	for fieldID, raw := range fields {
		value, err := bindNativeValue(raw, schemas[fieldID])
		if err != nil {
			return contract.PresentValue{}, fmt.Errorf("authored field %q: %w", fieldID, err)
		}
		values[fieldID] = value
	}
	typed := contract.NewObjectValue(values)
	if descriptor.RequestBody.ValidateInput(bindings, typed) != nil {
		return contract.PresentValue{}, errors.New("authored step input does not satisfy the declared operation input contract")
	}
	return typed, nil
}

func bindNativeValue(document json.RawMessage, schema contract.Schema) (contract.PresentValue, error) {
	trimmed := bytes.TrimSpace(document)
	if len(trimmed) == 0 {
		return contract.PresentValue{}, errors.New("authored value is empty")
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return contract.NullValue(), nil
	}
	switch schema.Type {
	case contract.SchemaTypeString:
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return contract.PresentValue{}, errors.New("declared string field received a non-string JSON value")
		}
		return contract.NewStringValue(text), nil
	case contract.SchemaTypeBool:
		var flag bool
		if err := json.Unmarshal(trimmed, &flag); err != nil {
			return contract.PresentValue{}, errors.New("declared bool field received a non-bool JSON value")
		}
		return contract.NewBooleanValue(flag), nil
	case contract.SchemaTypeInteger:
		number, ok := exactJSONNumber(trimmed)
		if !ok {
			return contract.PresentValue{}, errors.New("declared integer field received a non-numeric JSON value")
		}
		return contract.NewIntegerValue(number)
	case contract.SchemaTypeDecimal:
		number, ok := exactJSONNumber(trimmed)
		if !ok {
			return contract.PresentValue{}, errors.New("declared decimal field received a non-numeric JSON value")
		}
		precision, scale, ok := decimalLiteralShape(number)
		if !ok {
			return contract.PresentValue{}, errors.New("declared decimal literal is not an exact decimal")
		}
		return contract.NewDecimalValue(number, precision, scale)
	case contract.SchemaTypeDate:
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return contract.PresentValue{}, errors.New("declared date field received a non-string JSON value")
		}
		return contract.NewDateValue(text)
	case contract.SchemaTypeTimestampInstant:
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return contract.PresentValue{}, errors.New("declared instant field received a non-string JSON value")
		}
		return contract.NewInstantValue(text)
	case contract.SchemaTypeTimestampLocal:
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return contract.PresentValue{}, errors.New("declared local timestamp field received a non-string JSON value")
		}
		return contract.NewLocalTimestampValue(text)
	case contract.SchemaTypeArray:
		var elements []json.RawMessage
		if err := json.Unmarshal(trimmed, &elements); err != nil {
			return contract.PresentValue{}, errors.New("declared array field received a non-array JSON value")
		}
		values := make([]contract.PresentValue, 0, len(elements))
		for _, element := range elements {
			if schema.Items == nil {
				return contract.PresentValue{}, errors.New("declared array field has no element schema")
			}
			value, err := bindNativeValue(element, *schema.Items)
			if err != nil {
				return contract.PresentValue{}, err
			}
			values = append(values, value)
		}
		return contract.NewArrayValue(values), nil
	case contract.SchemaTypeObject:
		var properties map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &properties); err != nil {
			return contract.PresentValue{}, errors.New("declared object field received a non-object JSON value")
		}
		values := make(map[string]contract.PresentValue, len(properties))
		for name, raw := range properties {
			element, declared := schema.Properties[name]
			if !declared {
				return contract.PresentValue{}, fmt.Errorf("declared object field received undeclared property %q", name)
			}
			value, err := bindNativeValue(raw, element)
			if err != nil {
				return contract.PresentValue{}, err
			}
			values[name] = value
		}
		return contract.NewObjectValue(values), nil
	default:
		return contract.PresentValue{}, errors.New("authored field type is not executable through the direct-read path")
	}
}

func exactJSONNumber(document []byte) (string, bool) {
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return "", false
	}
	if strings.ContainsAny(number.String(), "eE") {
		return "", false
	}
	return number.String(), true
}

func decimalLiteralShape(literal string) (int, int, bool) {
	negative := strings.HasPrefix(literal, "-")
	unsigned := strings.TrimPrefix(literal, "-")
	if unsigned == "" {
		return 0, 0, false
	}
	parts := strings.Split(unsigned, ".")
	if len(parts) > 2 {
		return 0, 0, false
	}
	digits := parts[0]
	scale := 0
	if len(parts) == 2 {
		if parts[1] == "" {
			return 0, 0, false
		}
		digits += parts[1]
		scale = len(parts[1])
	}
	for index := range len(digits) {
		if digits[index] < '0' || digits[index] > '9' {
			return 0, 0, false
		}
	}
	_ = negative
	return len(digits), scale, digits != ""
}

func bindResultControls(controls *contract.ResultControls) (*contract.ResultControls, error) {
	if controls == nil {
		return nil, nil
	}
	if err := controls.Validate(); err != nil {
		return nil, err
	}
	owned := *controls
	if owned.Selection != nil {
		selection := *owned.Selection
		selection.Paths = slices.Clone(owned.Selection.Paths)
		owned.Selection = &selection
	}
	return &owned, nil
}

func sourceHashes(digest string) []contract.SourceHash {
	candidate := contract.SourceHash(digest)
	if candidate.Validate() != nil {
		return []contract.SourceHash{}
	}
	return []contract.SourceHash{candidate}
}

func bundleSourceDigest(document []byte) string {
	record, err := connectors.LoadBundle(bytes.NewReader(document))
	if err != nil {
		return ""
	}
	return record.SourceDigest
}

func hashComponentArtifacts(configuration PMPathConfig) (componentHashes, error) {
	clientHash, err := hashArtifact(configuration.PMClientBinary, configuration.MaximumFileBytes)
	if err != nil {
		return componentHashes{}, fmt.Errorf("hash the actual PM client binary: %w", err)
	}
	apiHash, err := hashArtifact(configuration.APIBinary, configuration.MaximumFileBytes)
	if err != nil {
		return componentHashes{}, fmt.Errorf("hash the actual API binary: %w", err)
	}
	workerHash, err := hashArtifact(configuration.WorkerBinary, configuration.MaximumFileBytes)
	if err != nil {
		return componentHashes{}, fmt.Errorf("hash the actual worker binary: %w", err)
	}
	bundleHash, err := hashArtifact(configuration.BundlePath, configuration.MaximumFileBytes)
	if err != nil {
		return componentHashes{}, fmt.Errorf("hash the declared execution bundle: %w", err)
	}
	hashes := componentHashes{client: clientHash, api: apiHash, worker: workerHash, bundle: bundleHash}
	if configuration.QueryHelperBinary != "" {
		queryHash, err := hashArtifact(configuration.QueryHelperBinary, configuration.MaximumFileBytes)
		if err != nil {
			return componentHashes{}, fmt.Errorf("hash the actual query helper: %w", err)
		}
		hashes.queryHelper = &queryHash
	}
	return hashes, nil
}

func hashArtifact(path string, maximum int64) (contract.SourceHash, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("declared artifact path must be absolute")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximum {
		return "", errors.New("declared artifact is not a bounded regular file")
	}
	document, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return "", err
	}
	if int64(len(document)) != info.Size() {
		return "", errors.New("declared artifact changed while it was hashed")
	}
	return contract.RawSourceHash(document), nil
}

func readBoundedProofFile(path string, maximum int64) ([]byte, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, errors.New("declared path must be absolute")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximum {
		return nil, errors.New("declared path is not a bounded regular file")
	}
	document, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(document)) != info.Size() {
		return nil, errors.New("declared file changed while it was read")
	}
	return document, nil
}

type providerObservation struct {
	request  *contract.ProofRequestObservation
	response *contract.ProofResponseObservation
}

// providerJournal is the controlled destination's own record of what it
// received and returned, in arrival order. The controlled endpoint cannot know
// a PM run identity, so correlation is single-flight: the runner records the
// entry count before a case executes and requires exactly one new entry
// afterwards. The runner only reads the journal; a fake receipt without this
// independent provider observation cannot be produced.
type providerJournal struct {
	path    string
	maximum int64
}

func openProviderJournal(path string, maximum int64) (*providerJournal, error) {
	if maximum <= 0 || maximum > JournalBound {
		return nil, errors.New("provider observation journal bound is invalid")
	}
	journal := &providerJournal{path: path, maximum: maximum}
	if _, err := journal.entries(); err != nil {
		return nil, err
	}
	return journal, nil
}

// entries re-reads the bounded journal and returns its observations in order.
func (journal *providerJournal) entries() ([]providerJournalEntry, error) {
	if journal == nil {
		return nil, errors.New("controlled provider observation is required")
	}
	document, err := readBoundedProofFile(journal.path, journal.maximum)
	if err != nil {
		return nil, fmt.Errorf("read controlled provider observation journal: %w", err)
	}
	entries := make([]providerJournalEntry, 0, 8)
	scanner := bufio.NewScanner(bytes.NewReader(document))
	scanner.Buffer(make([]byte, 0, 64<<10), int(journal.maximum))
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var entry providerJournalEntry
		if err := contract.DecodeStrictRequiredObject(line, &entry,
			"method", "target", "media_type", "status", "response_media_type",
			"request_fields", "response_fields", "redactions"); err != nil {
			return nil, fmt.Errorf("decode controlled provider observation line %d: %w", lineNumber, err)
		}
		if entry.Method == "" || entry.Target == "" || entry.Status < 100 || entry.Status > 599 {
			return nil, fmt.Errorf("controlled provider observation line %d is incomplete", lineNumber)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan controlled provider observation journal: %w", err)
	}
	return entries, nil
}

// takeAfter requires exactly one new observation since baseline and returns it.
func (journal *providerJournal) takeAfter(baseline int) (providerObservation, error) {
	entries, err := journal.entries()
	if err != nil {
		return providerObservation{}, err
	}
	if len(entries) != baseline+1 {
		return providerObservation{}, errors.New("controlled provider did not record exactly one observation for this step")
	}
	chosen := entries[baseline]
	requestFields, err := sanitizedFields(chosen.RequestFields)
	if err != nil {
		return providerObservation{}, err
	}
	responseFields, err := sanitizedFields(chosen.ResponseFields)
	if err != nil {
		return providerObservation{}, err
	}
	return providerObservation{
		request: &contract.ProofRequestObservation{
			Method: chosen.Method, Target: chosen.Target, MediaType: chosen.MediaType,
			BodySHA256:      chosen.RequestBodySHA256,
			SanitizedFields: requestFields, Redactions: redactions(chosen.Redactions),
		},
		response: &contract.ProofResponseObservation{
			Status: chosen.Status, MediaType: chosen.ResponseMediaType,
			BodySHA256:      chosen.ResponseBodySHA256,
			SanitizedFields: responseFields, Redactions: redactions(chosen.Redactions),
		},
	}, nil
}

func sanitizedFields(fields map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	if fields == nil {
		return map[string]json.RawMessage{}, nil
	}
	if err := rejectSecretBearingFields(fields); err != nil {
		return nil, err
	}
	return fields, nil
}

// rejectSecretBearingFields fails closed when an observation carries provider
// authentication material as an ordinary field. A writer that must record such
// a fact has to use a redaction entry instead of copying the value.
func rejectSecretBearingFields(fields map[string]json.RawMessage) error {
	for name := range fields {
		normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(name, "_", ""), "-", ""))
		for _, marker := range []string{
			"authorization", "token", "secret", "password", "credential",
			"cookie", "apikey", "privatekey", "signature", "clientsecret",
		} {
			if strings.Contains(normalized, marker) {
				return errors.New("provider observation carries secret-bearing fields and must redact them")
			}
		}
	}
	return nil
}

func redactions(values []contract.ProofRedaction) []contract.ProofRedaction {
	if values == nil {
		return []contract.ProofRedaction{}
	}
	return values
}

type providerJournalEntry struct {
	Method             string                     `json:"method"`
	Target             string                     `json:"target"`
	MediaType          string                     `json:"media_type"`
	RequestBodySHA256  *contract.SourceHash       `json:"request_body_sha256,omitempty"`
	Status             int                        `json:"status"`
	ResponseMediaType  string                     `json:"response_media_type"`
	ResponseBodySHA256 *contract.SourceHash       `json:"response_body_sha256,omitempty"`
	RequestFields      map[string]json.RawMessage `json:"request_fields"`
	ResponseFields     map[string]json.RawMessage `json:"response_fields"`
	Redactions         []contract.ProofRedaction  `json:"redactions"`
}

var _ StepExecutor = (*PMPathExecutor)(nil)
