package testfixture

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	sdkclient "go.temporal.io/sdk/client"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/credentials"
)

const (
	temporalFixtureReadyTimeout = 15 * time.Second
	temporalProfileLimit        = 1 << 20
	rolloverOwnerEmail          = "pm-stack-fixture@invalid"
)

// TemporalOptions resolves the selected real mutual-TLS Temporal endpoint. The
// namespace must be either the primary selected namespace or the explicitly
// owned rollover-profile namespace.
func TemporalOptions(
	t testing.TB,
	configuration config.Configuration,
	namespace string,
	identity string,
) sdkclient.Options {
	t.Helper()
	if !configuration.Worker().Configured() || identity == "" {
		t.Fatal("real Temporal fixture requires explicit worker controls and identity")
	}
	temporalConfiguration := configuration.Temporal()
	rolloverNamespace := os.Getenv("PM_STACK_TEST_ROLLOVER_NAMESPACE")
	if namespace != temporalConfiguration.Namespace && (rolloverNamespace == "" || namespace != rolloverNamespace) {
		t.Fatal("Temporal namespace is not owned by the selected fixture")
	}
	secretsDirectory := os.Getenv("PM_STACK_SECRETS_DIR")
	if secretsDirectory == "" {
		t.Fatal("PM_STACK_SECRETS_DIR is required for the selected Temporal fixture")
	}
	secretStore, err := credentials.OpenOperatorSecrets(secretsDirectory)
	if err != nil {
		t.Fatalf("open selected operator secrets: %v", err)
	}
	defer secretStore.Close()
	caPEM, err := secretStore.Read(temporalConfiguration.ClientIdentityRef, "ca.pem")
	if err != nil {
		t.Fatalf("read selected Temporal CA: %v", err)
	}
	defer clear(caPEM)
	certificatePEM, err := secretStore.Read(temporalConfiguration.ClientIdentityRef, "client.pem")
	if err != nil {
		t.Fatalf("read selected Temporal client certificate: %v", err)
	}
	defer clear(certificatePEM)
	privateKeyPEM, err := secretStore.Read(temporalConfiguration.ClientIdentityRef, "client-key.pem")
	if err != nil {
		t.Fatalf("read selected Temporal client key: %v", err)
	}
	defer clear(privateKeyPEM)
	certificate, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
	if err != nil {
		t.Fatal("selected Temporal client identity is invalid")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("selected Temporal CA bundle is invalid")
	}
	host, _, err := net.SplitHostPort(temporalConfiguration.Endpoint)
	if err != nil || host == "" {
		t.Fatal("selected Temporal endpoint is not host:port")
	}
	return sdkclient.Options{
		HostPort:    temporalConfiguration.Endpoint,
		Namespace:   namespace,
		Identity:    identity,
		Credentials: sdkclient.NewMTLSCredentials(certificate),
		ConnectionOptions: sdkclient.ConnectionOptions{TLS: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
			ServerName: host,
		}},
	}
}

// OpenTemporal connects with TemporalOptions and owns the resulting client for
// the lifetime of the test.
func OpenTemporal(
	t testing.TB,
	configuration config.Configuration,
	namespace string,
	identity string,
) sdkclient.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), configuration.Worker().DialTimeout)
	defer cancel()
	client, err := sdkclient.DialContext(ctx, TemporalOptions(t, configuration, namespace, identity))
	if err != nil {
		t.Fatalf("dial selected Temporal fixture: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

// CleanupWorkflowChain registers bounded cleanup for one exact execution root.
// The server-side termination guard prevents a reused workflow ID from
// terminating a different chain; deletion then names every observed run.
func CleanupWorkflowChain(
	t testing.TB,
	client sdkclient.Client,
	namespace, workflowID, firstRunID string,
) {
	t.Helper()
	if client == nil || namespace == "" || workflowID == "" || firstRunID == "" ||
		namespace != strings.TrimSpace(namespace) ||
		workflowID != strings.TrimSpace(workflowID) ||
		firstRunID != strings.TrimSpace(firstRunID) {
		t.Fatal("Temporal workflow cleanup requires an exact namespace, workflow, and first run")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		service := client.WorkflowService()
		_, terminateErr := service.TerminateWorkflowExecution(
			ctx,
			&workflowservice.TerminateWorkflowExecutionRequest{
				Namespace: namespace,
				WorkflowExecution: &commonpb.WorkflowExecution{
					WorkflowId: workflowID,
				},
				Reason:              "owned test fixture cleanup",
				Identity:            "pm-testfixture-cleanup",
				FirstExecutionRunId: firstRunID,
			},
		)
		if terminateErr != nil {
			var notFound *serviceerror.NotFound
			var failedPrecondition *serviceerror.FailedPrecondition
			if !errors.As(terminateErr, &notFound) &&
				!errors.As(terminateErr, &failedPrecondition) {
				t.Errorf("terminate exact Temporal workflow chain: %v", terminateErr)
				return
			}
		}

		runIDs, err := temporalWorkflowChainRunIDs(
			ctx,
			client,
			workflowID,
			firstRunID,
		)
		if err != nil {
			var notFound *serviceerror.NotFound
			if terminateErr != nil && errors.As(terminateErr, &notFound) &&
				errors.As(err, &notFound) {
				return
			}
			t.Errorf("enumerate exact Temporal workflow chain: %v", err)
			return
		}
		for index := len(runIDs) - 1; index >= 0; index-- {
			_, err := service.DeleteWorkflowExecution(
				ctx,
				&workflowservice.DeleteWorkflowExecutionRequest{
					Namespace: namespace,
					WorkflowExecution: &commonpb.WorkflowExecution{
						WorkflowId: workflowID,
						RunId:      runIDs[index],
					},
				},
			)
			var notFound *serviceerror.NotFound
			if err != nil && !errors.As(err, &notFound) {
				t.Errorf("delete exact Temporal workflow run %q: %v", runIDs[index], err)
				return
			}
		}
	})
}

func temporalWorkflowChainRunIDs(
	ctx context.Context,
	client sdkclient.Client,
	workflowID, firstRunID string,
) ([]string, error) {
	const maximumChainRuns = 256
	runIDs := make([]string, 0, 4)
	seen := make(map[string]struct{}, 4)
	runID := firstRunID
	for len(runIDs) < maximumChainRuns {
		if _, duplicate := seen[runID]; duplicate {
			return nil, errors.New("Temporal workflow chain contains a repeated run identity")
		}
		seen[runID] = struct{}{}
		iterator := client.GetWorkflowHistory(
			ctx,
			workflowID,
			runID,
			false,
			enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT,
		)
		var (
			observedStart bool
			successor     string
		)
		for iterator.HasNext() {
			event, err := iterator.Next()
			if err != nil {
				return nil, err
			}
			if started := event.GetWorkflowExecutionStartedEventAttributes(); started != nil {
				if observedStart || started.GetFirstExecutionRunId() != firstRunID {
					return nil, errors.New("Temporal workflow run does not belong to the exact first root")
				}
				observedStart = true
			}
			if continued := event.GetWorkflowExecutionContinuedAsNewEventAttributes(); continued != nil {
				if successor != "" && successor != continued.GetNewExecutionRunId() {
					return nil, errors.New("Temporal workflow run has contradictory continuation identities")
				}
				successor = continued.GetNewExecutionRunId()
			}
		}
		if !observedStart {
			return nil, fmt.Errorf("Temporal workflow run %q has no start event", runID)
		}
		runIDs = append(runIDs, runID)
		if successor == "" {
			return runIDs, nil
		}
		runID = successor
	}
	return nil, errors.New("Temporal workflow chain exceeds the fixture cleanup bound")
}

// TemporalNamespace identifies the exact server-owned namespace selected for
// one fixture profile. ID is the immutable Temporal namespace UUID.
type TemporalNamespace struct {
	Name string
	ID   string
}

// RolloverNamespace registers and observes the auxiliary namespace whose
// server-side continue-as-new suggestion is constrained by the fixture-owned
// dynamic configuration profile. The primary namespace remains unmodified.
func RolloverNamespace(t testing.TB, configuration config.Configuration) TemporalNamespace {
	t.Helper()
	primary := configuration.Temporal().Namespace
	namespace := os.Getenv("PM_STACK_TEST_ROLLOVER_NAMESPACE")
	if namespace == "" || namespace != primary+"-rollover" {
		t.Fatal("fixture-owned rollover namespace is required and must be the exact auxiliary identity")
	}
	profilePath := os.Getenv("PM_STACK_TEMPORAL_DYNAMIC_CONFIG")
	profile := readBoundedRegularFile(t, profilePath, temporalProfileLimit, "fixture Temporal profile")
	profileDigestBytes := sha256.Sum256(profile)
	profileDigest := "sha256:" + hex.EncodeToString(profileDigestBytes[:])
	description := "PM owned rollover fixture for " + primary
	ownerData := map[string]string{
		"pm.fixture.owner":                   primary,
		"pm.fixture.temporal_profile_sha256": profileDigest,
	}
	client := OpenTemporal(t, configuration, primary, "pm-rollover-namespace-fixture")
	ctx, cancel := context.WithTimeout(context.Background(), temporalFixtureReadyTimeout)
	defer cancel()
	_, err := client.WorkflowService().RegisterNamespace(ctx, &workflowservice.RegisterNamespaceRequest{
		Namespace:                        namespace,
		Description:                      description,
		OwnerEmail:                       rolloverOwnerEmail,
		WorkflowExecutionRetentionPeriod: durationpb.New(configuration.Temporal().Retention),
		Data:                             maps.Clone(ownerData),
	})
	if err != nil {
		var exists *serviceerror.NamespaceAlreadyExists
		if !errors.As(err, &exists) {
			t.Fatalf("register fixture-owned rollover namespace: %v", err)
		}
	}
	for {
		response, describeErr := client.WorkflowService().DescribeNamespace(
			ctx,
			&workflowservice.DescribeNamespaceRequest{Namespace: namespace},
		)
		if describeErr == nil {
			info := response.GetNamespaceInfo()
			namespaceConfig := response.GetConfig()
			if info == nil || info.GetName() != namespace || !validTemporalNamespaceID(info.GetId()) ||
				info.GetState() != enumspb.NAMESPACE_STATE_REGISTERED ||
				info.GetDescription() != description || info.GetOwnerEmail() != rolloverOwnerEmail ||
				!maps.Equal(info.GetData(), ownerData) || namespaceConfig == nil ||
				namespaceConfig.GetWorkflowExecutionRetentionTtl() == nil ||
				namespaceConfig.GetWorkflowExecutionRetentionTtl().AsDuration() != configuration.Temporal().Retention {
				t.Fatal("existing rollover namespace does not retain this fixture's exact ownership and retention")
			}
			return TemporalNamespace{Name: namespace, ID: info.GetId()}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("observe fixture-owned rollover namespace: %v", describeErr)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func validTemporalNamespaceID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
			return false
		}
	}
	return true
}
