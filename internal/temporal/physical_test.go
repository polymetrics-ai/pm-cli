package temporal

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/serviceerror"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"github.com/jackc/pgx/v5"

	"github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

const (
	physicalTemporalDialTimeout    = 5 * time.Second
	physicalTemporalRequestTimeout = 8 * time.Second
	physicalRouteTTL               = 12 * time.Second
	physicalLongRouteTTL           = 30 * time.Second
)

type physicalTemporalSelection struct {
	configuration config.Configuration
	secretBundle  string
}

func loadPhysicalTemporalSelection(t testing.TB) physicalTemporalSelection {
	t.Helper()
	configurationPath := requiredPhysicalEnvironment(t, "PM_STACK_TEST_CONFIG")
	file, err := os.Open(configurationPath)
	if err != nil {
		t.Fatalf("open selected stack configuration: %v", err)
	}
	document, err := config.Load(file)
	_ = file.Close()
	if err != nil {
		t.Fatalf("load selected stack configuration: %v", err)
	}
	configuration, err := config.Validate(document, config.Prelaunch{})
	if err != nil {
		t.Fatalf("validate selected stack configuration: %v", err)
	}
	return physicalTemporalSelection{
		configuration: configuration,
		secretBundle: resolvePhysicalSecretBundle(
			t,
			requiredPhysicalEnvironment(t, "PM_STACK_SECRETS_DIR"),
			configuration.Temporal().ClientIdentityRef,
		),
	}
}

func requiredPhysicalEnvironment(t testing.TB, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is required for the selected physical Temporal fixture", name)
	}
	return value
}

func resolvePhysicalSecretBundle(t testing.TB, root, reference string) string {
	t.Helper()
	parsed, err := url.Parse(reference)
	if err != nil || parsed.Scheme != "secret" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		t.Fatal("selected configuration contains an invalid Temporal secret reference")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatalf("resolve selected secret root: %v", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve selected secret root: %v", err)
	}
	pathValue, err := url.PathUnescape(filepath.FromSlash(parsed.Host + "/" + strings.TrimPrefix(parsed.EscapedPath(), "/")))
	if err != nil {
		t.Fatal("selected configuration contains an invalid escaped Temporal secret reference")
	}
	bundle, err := filepath.EvalSymlinks(filepath.Join(root, pathValue))
	if err != nil {
		t.Fatalf("resolve selected Temporal secret bundle: %v", err)
	}
	relative, err := filepath.Rel(root, bundle)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatal("selected Temporal secret bundle escapes its root")
	}
	return bundle
}

func dialPhysicalTemporal(ctx context.Context, selection physicalTemporalSelection, identity string) (sdkclient.Client, error) {
	ca, err := os.ReadFile(filepath.Join(selection.secretBundle, "ca.pem"))
	if err != nil {
		return nil, errors.New("read selected Temporal CA")
	}
	defer clear(ca)
	certificatePEM, err := os.ReadFile(filepath.Join(selection.secretBundle, "client.pem"))
	if err != nil {
		return nil, errors.New("read selected Temporal client certificate")
	}
	defer clear(certificatePEM)
	privateKeyPEM, err := os.ReadFile(filepath.Join(selection.secretBundle, "client-key.pem"))
	if err != nil {
		return nil, errors.New("read selected Temporal client key")
	}
	defer clear(privateKeyPEM)
	certificate, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
	if err != nil {
		return nil, errors.New("load selected Temporal client identity")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("selected Temporal CA bundle is invalid")
	}
	temporalConfiguration := selection.configuration.Temporal()
	host, _, err := net.SplitHostPort(temporalConfiguration.Endpoint)
	if err != nil {
		return nil, errors.New("selected Temporal endpoint is not host:port")
	}
	return sdkclient.DialContext(ctx, sdkclient.Options{
		HostPort:    temporalConfiguration.Endpoint,
		Namespace:   temporalConfiguration.Namespace,
		Identity:    identity,
		Credentials: sdkclient.NewMTLSCredentials(certificate),
		ConnectionOptions: sdkclient.ConnectionOptions{TLS: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
			ServerName: host,
		}},
	})
}

func openPhysicalTemporal(t testing.TB, identity string) sdkclient.Client {
	t.Helper()
	selection := loadPhysicalTemporalSelection(t)
	ctx, cancel := context.WithTimeout(context.Background(), physicalTemporalDialTimeout)
	defer cancel()
	client, err := dialPhysicalTemporal(ctx, selection, identity)
	if err != nil {
		t.Fatalf("dial selected physical Temporal service: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

func openPhysicalStore(t testing.TB) *store.Pool {
	t.Helper()
	directory := requiredPhysicalEnvironment(t, "PM_STORE_TEST_SECRETS_DIR")
	endpoint, err := os.ReadFile(filepath.Join(directory, "pm-app-endpoint"))
	if err != nil {
		t.Fatal("read physical runtime store endpoint")
	}
	defer clear(endpoint)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := store.OpenPool(ctx, store.PoolConfig{
		ConnectionString:  strings.TrimSpace(string(endpoint)),
		MaxConns:          2,
		MinConns:          0,
		MaxConnLifetime:   5 * time.Minute,
		MaxConnIdleTime:   time.Minute,
		HealthCheckPeriod: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("open physical runtime store: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newPhysicalRelay(
	t testing.TB,
	fixture *testfixture.Fixture,
	backend WorkflowClient,
	ownerID string,
	claimTTL time.Duration,
) (*Relay, *Client) {
	t.Helper()
	selection := loadPhysicalTemporalSelection(t)
	client, err := NewClient(ClientConfig{
		Namespace:            selection.configuration.Temporal().Namespace,
		ControlTaskQueue:     ControlTaskQueue,
		WorkflowType:         RunWorkflowType,
		ProjectionUpdateName: ProjectionUpdateName,
		DialTimeout:          physicalTemporalDialTimeout,
		RequestTimeout:       physicalTemporalRequestTimeout,
		Dial: func(context.Context) (WorkflowClient, error) {
			if backend == nil {
				return nil, errors.New("physical Temporal backend is unavailable")
			}
			return backend, nil
		},
	})
	if err != nil {
		t.Fatalf("construct physical Temporal client: %v", err)
	}
	t.Cleanup(client.Close)
	relay, err := NewRelay(RelayConfig{
		Store:        fixture.Pool,
		Client:       client,
		OwnerID:      ownerID,
		ClaimLimit:   1,
		ClaimTTL:     claimTTL,
		IdleDelay:    20 * time.Millisecond,
		FailureDelay: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("construct physical workflow relay: %v", err)
	}
	return relay, client
}

func admitPhysicalRun(t testing.TB, fixture *testfixture.Fixture, key string) api.AdmissionResponse {
	t.Helper()
	invocation := testfixture.Invocation(t, fixture, key)
	encoded, err := json.Marshal(invocation)
	if err != nil {
		t.Fatalf("encode physical admission request: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fixture.BaseURL+"/v1/workspaces/"+url.PathEscape(fixture.Scope.WorkspaceID)+"/invocations",
		bytes.NewReader(encoded),
	)
	if err != nil {
		t.Fatalf("construct physical admission request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	response, err := fixture.Client.Do(request)
	if err != nil {
		t.Fatalf("submit physical admission request: %v", err)
	}
	defer response.Body.Close()
	document, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(document) > 64*1024 {
		t.Fatal("physical admission response exceeded its bounded envelope")
	}
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("physical admission status = %d body=%q; want 202", response.StatusCode, document)
	}
	var admitted api.AdmissionResponse
	if err := json.Unmarshal(document, &admitted); err != nil {
		t.Fatalf("decode physical admission response: %v", err)
	}
	if admitted.RunRef == "" || admitted.Replay {
		t.Fatalf("physical admission response = %#v; want fresh durable run", admitted)
	}
	return admitted
}

func claimPhysicalRoute(
	t testing.TB,
	fixture *testfixture.Fixture,
	ownerID string,
	ttl time.Duration,
) store.WorkflowRoute {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var routes []store.WorkflowRoute
	if err := store.WithinTransaction(ctx, fixture.Pool, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		routes, err = store.ClaimWorkflowRoutes(ctx, tx, store.WorkflowRouteRequest{OwnerID: ownerID, Limit: 1, TTL: ttl})
		return err
	}); err != nil {
		t.Fatalf("claim physical workflow route: %v", err)
	}
	if len(routes) != 1 || !routes[0].Claim.Scope.Equal(fixture.Scope) {
		t.Fatalf("claimed workflow routes = %#v; want one fixture-owned route", routes)
	}
	return routes[0]
}

func reclaimPhysicalRoute(
	t testing.TB,
	fixture *testfixture.Fixture,
	runID string,
	ownerID string,
	ttl time.Duration,
) store.WorkflowRoute {
	t.Helper()
	deadline := time.Now().Add(ttl + 10*time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		var routes []store.WorkflowRoute
		err := store.WithinTransaction(ctx, fixture.Pool, func(ctx context.Context, tx *store.Transaction) error {
			var err error
			routes, err = store.ClaimWorkflowRoutes(ctx, tx, store.WorkflowRouteRequest{OwnerID: ownerID, Limit: 1, TTL: ttl})
			return err
		})
		cancel()
		if err != nil {
			t.Fatalf("reclaim physical workflow route: %v", err)
		}
		if len(routes) == 1 {
			if routes[0].Claim.RunID != runID || !routes[0].Claim.Scope.Equal(fixture.Scope) {
				t.Fatalf("reclaimed route = %#v; want exact fixture run %q", routes[0], runID)
			}
			return routes[0]
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("workflow route %q did not become claimable before deadline", runID)
	return store.WorkflowRoute{}
}

func loadPhysicalRun(t testing.TB, fixture *testfixture.Fixture, runID string) contract.RunRecord {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var record contract.RunRecord
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		record, err = store.LoadRun(ctx, tx, runID)
		return err
	}); err != nil {
		t.Fatalf("load physical run %q: %v", runID, err)
	}
	return record
}

func workflowRouteRows(t testing.TB, fixture *testfixture.Fixture, runID string) (startCount, acknowledgedStarts, dispatches, effects, pending int) {
	t.Helper()
	fixture.SetupSQL(t, fixture.Scope, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT
			    (SELECT count(*) FROM pm.outbox_commands
			     WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND kind = 'start_run'),
			    (SELECT count(*) FROM pm.outbox_commands
			     WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3
			       AND kind = 'start_run' AND acknowledged_at IS NOT NULL),
			    (SELECT count(*) FROM pm.dispatches
			     WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3),
			    (SELECT count(*) FROM pm.effects
			     WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3),
			    (SELECT count(*) FROM pm.pending_reservations
			     WHERE org_id = $1 AND workspace_id = $2 AND run_id = $3 AND released_at IS NULL)
		`, fixture.Scope.OrgID, fixture.Scope.WorkspaceID, runID).Scan(
			&startCount,
			&acknowledgedStarts,
			&dispatches,
			&effects,
			&pending,
		)
	})
	return
}

func observeWorkflowStart(
	t testing.TB,
	client sdkclient.Client,
	workflowID string,
) (string, StartRunInput) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		response, err := client.DescribeWorkflowExecution(ctx, workflowID, "")
		cancel()
		if err != nil {
			var notFound *serviceerror.NotFound
			if errors.As(err, &notFound) {
				time.Sleep(25 * time.Millisecond)
				continue
			}
			t.Fatalf("independently describe accepted Temporal workflow: %v", err)
		}
		runID := response.GetWorkflowExecutionInfo().GetExecution().GetRunId()
		ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
		iterator := client.GetWorkflowHistory(ctx, workflowID, runID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
		if iterator == nil || !iterator.HasNext() {
			cancel()
			t.Fatal("accepted Temporal workflow has no started history")
		}
		event, err := iterator.Next()
		cancel()
		if err != nil {
			t.Fatalf("read accepted Temporal workflow start history: %v", err)
		}
		input := decodePhysicalStartInput(t, event)
		return runID, input
	}
	t.Fatalf("Temporal workflow %q was not independently observable before deadline", workflowID)
	return "", StartRunInput{}
}

func decodePhysicalStartInput(t testing.TB, event *historypb.HistoryEvent) StartRunInput {
	t.Helper()
	if event == nil || event.GetEventType() != enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED ||
		event.GetWorkflowExecutionStartedEventAttributes() == nil {
		t.Fatal("Temporal history did not begin with WorkflowExecutionStarted")
	}
	payloads := event.GetWorkflowExecutionStartedEventAttributes().GetInput().GetPayloads()
	if len(payloads) != 1 {
		t.Fatalf("Temporal workflow start payload count = %d; want 1", len(payloads))
	}
	var input StartRunInput
	if err := converter.GetDefaultDataConverter().FromPayload(payloads[0], &input); err != nil {
		t.Fatalf("decode physical Temporal start input: %v", err)
	}
	if err := input.Validate(); err != nil {
		t.Fatalf("validate physical Temporal start input: %v", err)
	}
	return input
}

func physicalWorkflowStartObject(
	t testing.TB,
	client sdkclient.Client,
	workflowID string,
	runID string,
) map[string]json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	iterator := client.GetWorkflowHistory(ctx, workflowID, runID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	if iterator == nil || !iterator.HasNext() {
		t.Fatal("physical Temporal workflow has no retained start event")
	}
	event, err := iterator.Next()
	if err != nil {
		t.Fatalf("read physical Temporal workflow start event: %v", err)
	}
	if event == nil || event.GetWorkflowExecutionStartedEventAttributes() == nil {
		t.Fatal("physical Temporal history did not begin with a start event")
	}
	payloads := event.GetWorkflowExecutionStartedEventAttributes().GetInput().GetPayloads()
	if len(payloads) != 1 {
		t.Fatalf("physical Temporal start payload count = %d; want 1", len(payloads))
	}
	var object map[string]json.RawMessage
	if err := converter.GetDefaultDataConverter().FromPayload(payloads[0], &object); err != nil {
		t.Fatalf("decode physical Temporal start object: %v", err)
	}
	return object
}

func terminatePhysicalWorkflow(t testing.TB, client sdkclient.Client, workflowID, runID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = client.TerminateWorkflow(ctx, workflowID, runID, "physical fixture cleanup")
	})
}

func deletePhysicalWorkflowHistory(t testing.TB, client sdkclient.Client, workflowID, runID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	description, err := client.DescribeWorkflowExecution(ctx, workflowID, runID)
	cancel()
	if err != nil {
		t.Fatalf("describe physical workflow before exact deletion: %v", err)
	}
	if status := description.GetWorkflowExecutionInfo().GetStatus(); status != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		t.Fatalf("physical workflow deletion prerequisite status = %s; want running", status)
	}

	selection := loadPhysicalTemporalSelection(t)
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	_, err = client.WorkflowService().DeleteWorkflowExecution(ctx, &workflowservice.DeleteWorkflowExecutionRequest{
		Namespace: selection.configuration.Temporal().Namespace,
		WorkflowExecution: &commonpb.WorkflowExecution{
			WorkflowId: workflowID,
			RunId:      runID,
		},
	})
	cancel()
	if err != nil {
		t.Fatalf("delete exact physical workflow history fixture: %v", err)
	}

	// Temporal 1.31.2 deletes asynchronously. Calling the public API while the
	// execution is still running selects its delete-after-terminate close-task
	// path instead of the closed-execution path that waits for a delayed
	// transfer-queue acknowledgement. The two-minute bound exceeds both pinned
	// jittered defaults: the 60s transfer poll (69s maximum) and, defensively,
	// one 30s acknowledgement update (34.5s maximum), with RPC headroom.
	deletionDeadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deletionDeadline) {
		ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
		_, describeErr := client.DescribeWorkflowExecution(ctx, workflowID, runID)
		cancel()
		var notFound *serviceerror.NotFound
		if errors.As(describeErr, &notFound) {
			absent, historyErr := physicalWorkflowHistoryAbsent(client, workflowID, runID)
			if historyErr != nil {
				t.Fatalf("observe deleted physical workflow history: %v", historyErr)
			}
			if absent {
				return
			}
		} else if describeErr != nil {
			t.Fatalf("observe deleted physical workflow execution: %v", describeErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("deleted physical workflow %q run %q remained observable after the bounded asynchronous deletion wait", workflowID, runID)
}

func physicalWorkflowHistoryAbsent(client sdkclient.Client, workflowID, runID string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	iterator := client.GetWorkflowHistory(ctx, workflowID, runID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	if iterator == nil || !iterator.HasNext() {
		return false, nil
	}
	_, err := iterator.Next()
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func randomPhysicalID(t testing.TB, prefix string) string {
	t.Helper()
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		t.Fatal("generate physical fixture identity")
	}
	return prefix + hex.EncodeToString(value)
}

func randomPhysicalUUID(t testing.TB) string {
	t.Helper()
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		t.Fatal("generate physical fixture UUID")
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}

func physicalDigest(seed string) contract.SemanticDigest {
	digest := sha256.Sum256([]byte(seed))
	return contract.SemanticDigest("sha256:" + hex.EncodeToString(digest[:]))
}

func admitScheduledPhysicalRun(
	t testing.TB,
	fixture *testfixture.Fixture,
	workflowID string,
	temporalRunID string,
) string {
	t.Helper()
	runID := randomPhysicalUUID(t)
	binding, err := contract.AdoptScheduledWorkflow(runID, workflowID, temporalRunID, time.Now().UTC())
	if err != nil {
		t.Fatalf("construct adopted scheduled binding: %v", err)
	}
	policy := store.RunBudgetPolicy{
		Pending: store.AdmissionLimits{
			GlobalPending:          256,
			WorkspacePending:       32,
			WorkspaceMaxPendingAge: 10 * time.Minute,
		},
		RunLifetime:          30 * time.Minute,
		InputMaxBytes:        1 << 20,
		WorkspaceActiveRuns:  32,
		WorkflowPayloadBytes: 512 << 10,
		WorkflowFanout:       64,
	}
	budget, err := store.NewRunBudgetSnapshot(fixture.Scope, runID, "budget-"+runID, policy)
	if err != nil {
		t.Fatalf("construct scheduled run budget: %v", err)
	}
	operationID := "scheduled-operation"
	digest := physicalDigest(runID)
	spec := contract.RunSpec{
		RunID:                  runID,
		OrgID:                  fixture.Scope.OrgID,
		WorkspaceID:            fixture.Scope.WorkspaceID,
		Actor:                  fixture.Actor,
		ExecutionGrantRef:      "grant-" + runID,
		Origin:                 contract.RunOriginSchedule,
		OperationOrPlanRef:     contract.OperationOrPlanRef{Kind: contract.OperationReference, ID: operationID, Revision: "revision-1"},
		InputRefs:              []string{"input-" + runID},
		SchemaRefs:             []string{},
		CapabilityVersions:     []contract.CapabilityRequirement{},
		Budget:                 budget.Binding(),
		IdempotencyFingerprint: digest,
	}
	safePayload := contract.SafePayload{References: []contract.SafeReference{}, Counts: []contract.SafeCount{}, ReasonCodes: []string{}}
	request := store.AdmissionRequest{
		Key: store.AdmissionKey{
			PrincipalID:    fixture.Actor.RequestedActor.PrincipalID,
			OperationID:    operationID,
			TargetID:       "schedule-" + runID,
			IdempotencyKey: "schedule-" + runID,
		},
		BudgetSnapshot: budget,
		Spec:           spec,
		Grant: contract.ExecutionGrant{
			GrantRef:      spec.ExecutionGrantRef,
			GrantEpoch:    1,
			Scope:         fixture.Scope,
			Actor:         fixture.Actor,
			ConnectionIDs: []string{},
			OperationIDs:  []string{operationID},
			ApprovalRefs:  []string{},
			ExpiresAt:     time.Now().UTC().Add(24 * time.Hour),
			PolicyEpochs:  contract.PolicyEpochs{Membership: 1},
		},
		InputSnapshot: store.RunInputSnapshot{
			Scope:          fixture.Scope,
			RunID:          runID,
			InputRef:       spec.InputRefs[0],
			ConnectionID:   fixture.Connection.ConnectionID,
			Input:          contract.NewObjectValue(map[string]contract.PresentValue{}),
			ResultControls: contract.ResultControls{},
			ApprovalRef:    contract.AbsentValue(),
		},
		WorkflowBinding: binding,
		Receipt:         store.Receipt{ReceiptID: "receipt-" + runID, Kind: "admission", RunID: runID, Payload: safePayload},
		Audit:           store.AuditEntry{AuditID: "audit-" + runID, Action: "run_admitted", TargetRef: runID, Payload: safePayload},
		Event:           store.EventInput{EventID: "event-" + runID, Type: "run_admitted", Payload: safePayload},
		Outbox:          store.OutboxCommand{CommandID: "ensure-" + runID, Kind: store.WorkflowCommandEnsureBinding, PayloadRef: runID},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.WithinScope(ctx, fixture.Pool, fixture.Scope, fixture.Actor, func(ctx context.Context, tx *store.Transaction) error {
		_, err := store.AdmitRun(ctx, tx, request)
		return err
	}); err != nil {
		t.Fatalf("admit scheduled physical run: %v", err)
	}
	return runID
}

func physicalWorkflowOptions(workflowID string, timeout time.Duration) sdkclient.StartWorkflowOptions {
	return sdkclient.StartWorkflowOptions{
		ID:                                       workflowID,
		TaskQueue:                                ControlTaskQueue,
		WorkflowExecutionTimeout:                 temporalDuration(timeout),
		WorkflowIDConflictPolicy:                 enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
		WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}
}
